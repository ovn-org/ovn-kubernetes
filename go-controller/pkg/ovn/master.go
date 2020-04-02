package ovn

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"reflect"
	"strings"
	"time"

	kapi "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog"
	utilnet "k8s.io/utils/net"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

const (
	// OvnServiceIdledAt is a constant string representing the Service annotation key
	// whose value indicates the time stamp in RFC3339 format when a Service was idled
	OvnServiceIdledAt = "k8s.ovn.org/idled-at"
)

// Start waits until this process is the leader before starting master functions
func (oc *Controller) Start(kClient kubernetes.Interface, nodeName string) error {
	// Set up leader election process first
	rl, err := resourcelock.New(
		resourcelock.ConfigMapsResourceLock,
		config.Kubernetes.OVNConfigNamespace,
		"ovn-kubernetes-master",
		kClient.CoreV1(),
		nil,
		resourcelock.ResourceLockConfig{Identity: nodeName},
	)
	if err != nil {
		return err
	}

	lec := leaderelection.LeaderElectionConfig{
		Lock:          rl,
		LeaseDuration: time.Duration(config.MasterHA.ElectionLeaseDuration) * time.Second,
		RenewDeadline: time.Duration(config.MasterHA.ElectionRenewDeadline) * time.Second,
		RetryPeriod:   time.Duration(config.MasterHA.ElectionRetryPeriod) * time.Second,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				klog.Infof("won leader election; in active mode")
				// run the cluster controller to init the master
				start := time.Now()
				defer func() {
					end := time.Since(start)
					metrics.MetricMasterReadyDuration.Set(end.Seconds())
				}()
				if err := oc.StartClusterMaster(nodeName); err != nil {
					panic(err.Error())
				}
				if err := oc.Run(); err != nil {
					panic(err.Error())
				}
			},
			OnStoppedLeading: func() {
				//This node was leader and it lost the election.
				// Whenever the node transitions from leader to follower,
				// we need to handle the transition properly like clearing
				// the cache. It is better to exit for now.
				// kube will restart and this will become a follower.
				klog.Infof("no longer leader; exiting")
				os.Exit(1)
			},
			OnNewLeader: func(newLeaderName string) {
				if newLeaderName != nodeName {
					klog.Infof("lost the election to %s; in standby mode", newLeaderName)
				}
			},
		},
	}

	leaderElector, err := leaderelection.NewLeaderElector(lec)
	if err != nil {
		return err
	}

	go leaderElector.Run(context.Background())

	return nil
}

// StartClusterMaster runs a subnet IPAM and a controller that watches arrival/departure
// of nodes in the cluster
// On an addition to the cluster (node create), a new subnet is created for it that will translate
// to creation of a logical switch (done by the node, but could be created here at the master process too)
// Upon deletion of a node, the switch will be deleted
//
// TODO: Verify that the cluster was not already called with a different global subnet
//  If true, then either quit or perform a complete reconfiguration of the cluster (recreate switches/routers with new subnet values)
func (oc *Controller) StartClusterMaster(masterNodeName string) error {
	// The gateway router need to be connected to the distributed router via a per-node join switch.
	// We need a subnet allocator that allocates subnet for this per-node join switch. Use the 100.64.0.0/16
	// or fd98::/64 network range with host bits set to 3. The allocator will start allocating subnet that has upto 6
	// host IPs)
	joinSubnet := config.V4JoinSubnet
	if config.IPv6Mode {
		joinSubnet = config.V6JoinSubnet
	}
	_ = oc.joinSubnetAllocator.AddNetworkRange(joinSubnet, 3)

	existingNodes, err := oc.kube.GetNodes()
	if err != nil {
		klog.Errorf("Error in initializing/fetching subnets: %v", err)
		return err
	}
	for _, clusterEntry := range config.Default.ClusterSubnets {
		err := oc.masterSubnetAllocator.AddNetworkRange(clusterEntry.CIDR.String(), clusterEntry.HostBits())
		if err != nil {
			return err
		}
	}
	for _, node := range existingNodes.Items {
		hostsubnet, _ := util.ParseNodeHostSubnetAnnotation(&node)
		if hostsubnet != nil {
			err := oc.masterSubnetAllocator.MarkAllocatedNetwork(hostsubnet)
			if err != nil {
				utilruntime.HandleError(err)
			}
		}
		joinsubnet, _ := util.ParseNodeJoinSubnetAnnotation(&node)
		if joinsubnet != nil {
			err := oc.joinSubnetAllocator.MarkAllocatedNetwork(joinsubnet)
			if err != nil {
				utilruntime.HandleError(err)
			}
		}
	}

	if _, _, err := util.RunOVNNbctl("--columns=_uuid", "list", "port_group"); err != nil {
		klog.Fatal("ovn version too old; does not support port groups")
	}

	if oc.multicastSupport {
		if _, _, err := util.RunOVNSbctl("--columns=_uuid", "list", "IGMP_Group"); err != nil {
			klog.Warningf("Multicast support enabled, however version of OVN in use does not support IGMP Group. " +
				"Disabling Multicast Support")
			oc.multicastSupport = false
		}
		if config.IPv6Mode {
			klog.Warningf("Multicast support enabled, but can not be used along with IPv6. Disabling Multicast Support")
			oc.multicastSupport = false
		}
	}

	if err := oc.SetupMaster(masterNodeName); err != nil {
		klog.Errorf("Failed to setup master (%v)", err)
		return err
	}

	return nil
}

// SetupMaster creates the central router and load-balancers for the network
func (oc *Controller) SetupMaster(masterNodeName string) error {
	clusterRouter := util.GetK8sClusterRouter()
	// Create a single common distributed router for the cluster.
	stdout, stderr, err := util.RunOVNNbctl("--", "--may-exist", "lr-add", clusterRouter,
		"--", "set", "logical_router", clusterRouter, "external_ids:k8s-cluster-router=yes")
	if err != nil {
		klog.Errorf("Failed to create a single common distributed router for the cluster, "+
			"stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
		return err
	}

	// If supported, enable IGMP relay on the router to forward multicast
	// traffic between nodes.
	if oc.multicastSupport {
		stdout, stderr, err = util.RunOVNNbctl("--", "set", "logical_router",
			clusterRouter, "options:mcast_relay=\"true\"")
		if err != nil {
			klog.Errorf("Failed to enable IGMP relay on the cluster router, "+
				"stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
			return err
		}

		// Drop IP multicast globally. Multicast is allowed only if explicitly
		// enabled in a namespace.
		err = createDefaultDenyMulticastPolicy()
		if err != nil {
			klog.Errorf("Failed to create default deny multicast policy, error: %v",
				err)
			return err
		}
	}

	// Create 2 load-balancers for east-west traffic.  One handles UDP and another handles TCP.
	oc.TCPLoadBalancerUUID, stderr, err = util.RunOVNNbctl("--data=bare", "--no-heading", "--columns=_uuid", "find", "load_balancer", "external_ids:k8s-cluster-lb-tcp=yes")
	if err != nil {
		klog.Errorf("Failed to get tcp load-balancer, stderr: %q, error: %v", stderr, err)
		return err
	}

	if oc.TCPLoadBalancerUUID == "" {
		oc.TCPLoadBalancerUUID, stderr, err = util.RunOVNNbctl("--", "create", "load_balancer", "external_ids:k8s-cluster-lb-tcp=yes", "protocol=tcp")
		if err != nil {
			klog.Errorf("Failed to create tcp load-balancer, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
			return err
		}
	}

	oc.UDPLoadBalancerUUID, stderr, err = util.RunOVNNbctl("--data=bare", "--no-heading", "--columns=_uuid", "find", "load_balancer", "external_ids:k8s-cluster-lb-udp=yes")
	if err != nil {
		klog.Errorf("Failed to get udp load-balancer, stderr: %q, error: %v", stderr, err)
		return err
	}
	if oc.UDPLoadBalancerUUID == "" {
		oc.UDPLoadBalancerUUID, stderr, err = util.RunOVNNbctl("--", "create", "load_balancer", "external_ids:k8s-cluster-lb-udp=yes", "protocol=udp")
		if err != nil {
			klog.Errorf("Failed to create udp load-balancer, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
			return err
		}
	}
	return nil
}

func (oc *Controller) addNodeJoinSubnetAnnotations(node *kapi.Node, subnet string) error {
	nodeAnnotations, err := util.CreateNodeJoinSubnetAnnotation(subnet)
	if err != nil {
		return fmt.Errorf("failed to marshal node %q join subnets annotation for subnet %s",
			node.Name, subnet)
	}
	err = oc.kube.SetAnnotationsOnNode(node, nodeAnnotations)
	if err != nil {
		return fmt.Errorf("failed to set node-join-subnets annotation on node %s: %v",
			node.Name, err)
	}
	return nil
}

func (oc *Controller) allocateJoinSubnet(node *kapi.Node) (*net.IPNet, error) {
	joinSubnet, err := util.ParseNodeJoinSubnetAnnotation(node)
	if err == nil {
		return joinSubnet, nil
	}

	// Allocate a new network for the join switch
	joinSubnets, err := oc.joinSubnetAllocator.AllocateNetworks()
	if err != nil {
		return nil, fmt.Errorf("Error allocating subnet for join switch for node %s: %v", node.Name, err)
	}
	if len(joinSubnets) != 1 {
		return nil, fmt.Errorf("Error allocating subnet for join switch for node %s: multiple subnets returned", node.Name)
	}
	joinSubnet = joinSubnets[0]

	defer func() {
		// Release the allocation on error
		if err != nil {
			_ = oc.joinSubnetAllocator.ReleaseNetwork(joinSubnet)
		}
	}()

	// Set annotation on the node
	err = oc.addNodeJoinSubnetAnnotations(node, joinSubnet.String())
	if err != nil {
		return nil, err
	}

	klog.Infof("Allocated join subnet %q for node %q", joinSubnet.String(), node.Name)
	return joinSubnet, nil
}

func (oc *Controller) deleteNodeJoinSubnet(nodeName string, subnet *net.IPNet) error {
	err := oc.joinSubnetAllocator.ReleaseNetwork(subnet)
	if err != nil {
		return fmt.Errorf("Error deleting join subnet %v for node %q: %s", subnet, nodeName, err)
	}
	klog.Infof("Deleted JoinSubnet %v for node %s", subnet, nodeName)
	return nil
}

func (oc *Controller) syncNodeManagementPort(node *kapi.Node, subnet *net.IPNet) error {
	macAddress, err := util.ParseNodeManagementPortMacAddr(node)
	if err != nil {
		return err
	}

	if macAddress == "" {
		// When macAddress was removed, delete the switch port
		stdout, stderr, err := util.RunOVNNbctl("--", "--if-exists", "lsp-del", "k8s-"+node.Name)
		if err != nil {
			klog.Errorf("Failed to delete logical port to switch, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
		}

		return nil
	}

	if subnet == nil {
		subnet, err = util.ParseNodeHostSubnetAnnotation(node)
		if err != nil {
			return err
		}
	}

	_, portIP := util.GetNodeWellKnownAddresses(subnet)

	// Create this node's management logical port on the node switch
	stdout, stderr, err := util.RunOVNNbctl(
		"--", "--may-exist", "lsp-add", node.Name, "k8s-"+node.Name,
		"--", "lsp-set-addresses", "k8s-"+node.Name, macAddress+" "+portIP.IP.String())
	if err != nil {
		klog.Errorf("Failed to add logical port to switch, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
		return err
	}

	if err := addAllowACLFromNode(node.Name, portIP.IP); err != nil {
		return err
	}

	if err := util.UpdateNodeSwitchExcludeIPs(node.Name, subnet); err != nil {
		return err
	}

	return nil
}

func (oc *Controller) syncGatewayLogicalNetwork(node *kapi.Node, l3GatewayConfig *util.L3GatewayConfig, subnet string) error {
	var err error
	var clusterSubnets []string
	for _, clusterSubnet := range config.Default.ClusterSubnets {
		clusterSubnets = append(clusterSubnets, clusterSubnet.CIDR.String())
	}

	// get a subnet for the per-node join switch
	joinSubnet, err := oc.allocateJoinSubnet(node)
	if err != nil {
		return err
	}

	err = util.GatewayInit(clusterSubnets, subnet, joinSubnet, node.Name, l3GatewayConfig)
	if err != nil {
		return fmt.Errorf("failed to init shared interface gateway: %v", err)
	}

	if l3GatewayConfig.Mode == config.GatewayModeShared {
		// Add static routes to OVN Cluster Router to enable pods on this Node to
		// reach the host IP
		err = addStaticRouteToHost(node, l3GatewayConfig.IPAddress.String())
		if err != nil {
			return err
		}
	}

	if l3GatewayConfig.NodePortEnable {
		err = oc.handleNodePortLB(node)
	} else {
		// nodePort disabled, delete gateway load balancers for this node.
		physicalGateway := "GR_" + node.Name
		for _, proto := range []kapi.Protocol{kapi.ProtocolTCP, kapi.ProtocolUDP} {
			lbUUID, _ := oc.getGatewayLoadBalancer(physicalGateway, proto)
			if lbUUID != "" {
				_, _, err := util.RunOVNNbctl("--if-exists", "destroy", "load_balancer", lbUUID)
				if err != nil {
					klog.Errorf("failed to destroy %s load balancer for gateway %s: %v", proto, physicalGateway, err)
				}
			}
		}
	}

	return err
}

func addStaticRouteToHost(node *kapi.Node, nicIP string) error {
	k8sClusterRouter := util.GetK8sClusterRouter()
	subnet, err := util.ParseNodeHostSubnetAnnotation(node)
	if err != nil {
		return fmt.Errorf("failed to get interface IP address for %s (%v)",
			util.K8sMgmtIntfName, err)
	}
	_, secondIP := util.GetNodeWellKnownAddresses(subnet)
	prefix := strings.Split(nicIP, "/")[0] + "/32"
	nexthop := strings.Split(secondIP.String(), "/")[0]
	_, stderr, err := util.RunOVNNbctl("--may-exist", "lr-route-add", k8sClusterRouter, prefix, nexthop)
	if err != nil {
		return fmt.Errorf("failed to add static route '%s via %s' for host %q on %s "+
			"stderr: %q, error: %v", nicIP, secondIP, node.Name, k8sClusterRouter, stderr, err)
	}

	return nil
}

func (oc *Controller) ensureNodeLogicalNetwork(nodeName string, hostsubnet *net.IPNet) error {
	firstIP, secondIP := util.GetNodeWellKnownAddresses(hostsubnet)
	nodeLRPMac := util.IPAddrToHWAddr(firstIP.IP)
	clusterRouter := util.GetK8sClusterRouter()

	// Create a router port and provide it the first address on the node's host subnet
	_, stderr, err := util.RunOVNNbctl("--may-exist", "lrp-add", clusterRouter, "rtos-"+nodeName,
		nodeLRPMac, firstIP.String())
	if err != nil {
		klog.Errorf("Failed to add logical port to router, stderr: %q, error: %v", stderr, err)
		return err
	}

	// Create a logical switch and set its subnet.

	args := []string{
		"--", "--may-exist", "ls-add", nodeName,
		"--", "set", "logical_switch", nodeName,
	}
	if utilnet.IsIPv6CIDR(hostsubnet) {
		args = append(args,
			"other-config:ipv6_prefix="+hostsubnet.IP.String(),
		)
	} else {
		excludeIPs := secondIP.IP.String()
		if config.HybridOverlay.Enabled {
			thirdIP := util.NextIP(secondIP.IP)
			excludeIPs += ".." + thirdIP.String()
		}
		args = append(args,
			"other-config:subnet="+hostsubnet.String(),
			"other-config:exclude_ips="+excludeIPs,
		)
	}
	stdout, stderr, err := util.RunOVNNbctl(args...)
	if err != nil {
		klog.Errorf("Failed to create a logical switch %v, stdout: %q, stderr: %q, error: %v", nodeName, stdout, stderr, err)
		return err
	}

	// If supported, enable IGMP snooping and querier on the node.
	if oc.multicastSupport {
		stdout, stderr, err = util.RunOVNNbctl("set", "logical_switch",
			nodeName, "other-config:mcast_snoop=\"true\"")
		if err != nil {
			klog.Errorf("Failed to enable IGMP on logical switch %v, stdout: %q, stderr: %q, error: %v",
				nodeName, stdout, stderr, err)
			return err
		}

		// Configure querier only if we have an IPv4 address, otherwise
		// disable querier.
		if !utilnet.IsIPv6(firstIP.IP) {
			stdout, stderr, err = util.RunOVNNbctl("set", "logical_switch",
				nodeName, "other-config:mcast_querier=\"true\"",
				"other-config:mcast_eth_src=\""+nodeLRPMac+"\"",
				"other-config:mcast_ip4_src=\""+firstIP.IP.String()+"\"")
			if err != nil {
				klog.Errorf("Failed to enable IGMP Querier on logical switch %v, stdout: %q, stderr: %q, error: %v",
					nodeName, stdout, stderr, err)
				return err
			}
		} else {
			stdout, stderr, err = util.RunOVNNbctl("set", "logical_switch",
				nodeName, "other-config:mcast_querier=\"false\"")
			if err != nil {
				klog.Errorf("Failed to disable IGMP Querier on logical switch %v, stdout: %q, stderr: %q, error: %v",
					nodeName, stdout, stderr, err)
				return err
			}
			klog.Infof("Disabled IGMP Querier on logical switch %v (No IPv4 Source IP available)",
				nodeName)
		}
	}

	// Connect the switch to the router.
	stdout, stderr, err = util.RunOVNNbctl("--", "--may-exist", "lsp-add", nodeName, "stor-"+nodeName,
		"--", "set", "logical_switch_port", "stor-"+nodeName, "type=router", "options:router-port=rtos-"+nodeName, "addresses="+"\""+nodeLRPMac+"\"")
	if err != nil {
		klog.Errorf("Failed to add logical port to switch, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
		return err
	}

	// Add our cluster TCP and UDP load balancers to the node switch
	if oc.TCPLoadBalancerUUID == "" {
		return fmt.Errorf("TCP cluster load balancer not created")
	}
	stdout, stderr, err = util.RunOVNNbctl("set", "logical_switch", nodeName, "load_balancer="+oc.TCPLoadBalancerUUID)
	if err != nil {
		klog.Errorf("Failed to set logical switch %v's loadbalancer, stdout: %q, stderr: %q, error: %v", nodeName, stdout, stderr, err)
		return err
	}

	// Add any service reject ACLs applicable for TCP LB
	acls := oc.getAllACLsForServiceLB(oc.TCPLoadBalancerUUID)
	if len(acls) > 0 {
		_, _, err = util.RunOVNNbctl("add", "logical_switch", nodeName, "acls", strings.Join(acls, ","))
		if err != nil {
			klog.Warningf("Unable to add TCP reject ACLs: %s for switch: %s, error: %v", acls, nodeName, err)
		}
	}

	if oc.UDPLoadBalancerUUID == "" {
		return fmt.Errorf("UDP cluster load balancer not created")
	}
	stdout, stderr, err = util.RunOVNNbctl("add", "logical_switch", nodeName, "load_balancer", oc.UDPLoadBalancerUUID)
	if err != nil {
		klog.Errorf("Failed to add logical switch %v's loadbalancer, stdout: %q, stderr: %q, error: %v", nodeName, stdout, stderr, err)
		return err
	}

	// Add any service reject ACLs applicable for UDP LB
	acls = oc.getAllACLsForServiceLB(oc.UDPLoadBalancerUUID)
	if len(acls) > 0 {
		_, _, err = util.RunOVNNbctl("add", "logical_switch", nodeName, "acls", strings.Join(acls, ","))
		if err != nil {
			klog.Warningf("Unable to add UDP reject ACLs: %s for switch: %s, error %v", acls, nodeName, err)
		}
	}

	// Add the node to the logical switch cache
	oc.lsMutex.Lock()
	defer oc.lsMutex.Unlock()
	if existing, ok := oc.logicalSwitchCache[nodeName]; ok && !reflect.DeepEqual(existing, hostsubnet) {
		klog.Warningf("Node %q logical switch already in cache with subnet %v; replacing with %v", nodeName, existing, hostsubnet)
	}
	oc.logicalSwitchCache[nodeName] = hostsubnet

	return nil
}

func (oc *Controller) addNodeAnnotations(node *kapi.Node, subnet string) error {
	nodeAnnotations, err := util.CreateNodeHostSubnetAnnotation(subnet)
	if err != nil {
		return fmt.Errorf("failed to marshal node %q annotation for subnet %s",
			node.Name, subnet)
	}
	err = oc.kube.SetAnnotationsOnNode(node, nodeAnnotations)
	if err != nil {
		return fmt.Errorf("failed to set node-subnets annotation on node %s: %v",
			node.Name, err)
	}
	return nil
}

func (oc *Controller) addNode(node *kapi.Node) (hostsubnet *net.IPNet, err error) {
	oc.clearInitialNodeNetworkUnavailableCondition(node, nil)

	hostsubnet, _ = util.ParseNodeHostSubnetAnnotation(node)
	if hostsubnet != nil {
		// Node already has subnet assigned; ensure its logical network is set up
		return hostsubnet, oc.ensureNodeLogicalNetwork(node.Name, hostsubnet)
	}

	// Node doesn't have a subnet assigned; reserve a new one for it
	hostsubnets, err := oc.masterSubnetAllocator.AllocateNetworks()
	if err != nil {
		return nil, fmt.Errorf("Error allocating network for node %s: %v", node.Name, err)
	}
	if len(hostsubnets) != 1 {
		return nil, fmt.Errorf("Error allocating network for node %s: multiple subnets returned", node.Name)
	}
	hostsubnet = hostsubnets[0]
	klog.Infof("Allocated node %s HostSubnet %s", node.Name, hostsubnet.String())

	defer func() {
		// Release the allocation on error
		if err != nil {
			_ = oc.masterSubnetAllocator.ReleaseNetwork(hostsubnet)
		}
	}()

	// Ensure that the node's logical network has been created
	err = oc.ensureNodeLogicalNetwork(node.Name, hostsubnet)
	if err != nil {
		return nil, err
	}

	// Set the HostSubnet annotation on the node object to signal
	// to nodes that their logical infrastructure is set up and they can
	// proceed with their initialization
	err = oc.addNodeAnnotations(node, hostsubnet.String())
	if err != nil {
		return nil, err
	}

	return hostsubnet, nil
}

func (oc *Controller) deleteNodeHostSubnet(nodeName string, subnet *net.IPNet) error {
	err := oc.masterSubnetAllocator.ReleaseNetwork(subnet)
	if err != nil {
		return fmt.Errorf("Error deleting subnet %v for node %q: %s", subnet, nodeName, err)
	}
	klog.Infof("Deleted HostSubnet %v for node %s", subnet, nodeName)
	return nil
}

func (oc *Controller) deleteNodeLogicalNetwork(nodeName string) error {
	// Remove the logical switch associated with the node
	if _, stderr, err := util.RunOVNNbctl("--if-exist", "ls-del", nodeName); err != nil {
		return fmt.Errorf("Failed to delete logical switch %s, "+
			"stderr: %q, error: %v", nodeName, stderr, err)
	}

	// Remove the patch port that connects distributed router to node's logical switch
	if _, stderr, err := util.RunOVNNbctl("--if-exist", "lrp-del", "rtos-"+nodeName); err != nil {
		return fmt.Errorf("Failed to delete logical router port rtos-%s, "+
			"stderr: %q, error: %v", nodeName, stderr, err)
	}

	return nil
}

func (oc *Controller) deleteNode(nodeName string, nodeSubnet, joinSubnet *net.IPNet) error {
	// Clean up as much as we can but don't hard error
	if nodeSubnet != nil {
		if err := oc.deleteNodeHostSubnet(nodeName, nodeSubnet); err != nil {
			klog.Errorf("Error deleting node %s HostSubnet %v: %v", nodeName, nodeSubnet, err)
		}
	}
	if joinSubnet != nil {
		if err := oc.deleteNodeJoinSubnet(nodeName, joinSubnet); err != nil {
			klog.Errorf("Error deleting node %s JoinSubnet %v: %v", nodeName, joinSubnet, err)
		}
	}

	if err := oc.deleteNodeLogicalNetwork(nodeName); err != nil {
		klog.Errorf("Error deleting node %s logical network: %v", nodeName, err)
	}

	if err := util.GatewayCleanup(nodeName, nodeSubnet); err != nil {
		return fmt.Errorf("Failed to clean up node %s gateway: (%v)", nodeName, err)
	}

	if err := oc.deleteNodeChassis(nodeName); err != nil {
		return err
	}

	return nil
}

// OVN uses an overlay and doesn't need GCE Routes, we need to
// clear the NetworkUnavailable condition that kubelet adds to initial node
// status when using GCE (done here: https://github.com/kubernetes/kubernetes/blob/master/pkg/controller/cloud/node_controller.go#L237).
// See discussion surrounding this here: https://github.com/kubernetes/kubernetes/pull/34398.
// TODO: make upstream kubelet more flexible with overlays and GCE so this
// condition doesn't get added for network plugins that don't want it, and then
// we can remove this function.
func (oc *Controller) clearInitialNodeNetworkUnavailableCondition(origNode, newNode *kapi.Node) {
	// If it is not a Cloud Provider node, then nothing to do.
	if origNode.Spec.ProviderID == "" {
		return
	}
	// if newNode is not nil, then we are called from UpdateFunc()
	if newNode != nil && reflect.DeepEqual(origNode.Status.Conditions, newNode.Status.Conditions) {
		return
	}

	cleared := false
	resultErr := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var err error

		oldNode, err := oc.kube.GetNode(origNode.Name)
		if err != nil {
			return err
		}
		// Informer cache should not be mutated, so get a copy of the object
		node := oldNode.DeepCopy()

		for i := range node.Status.Conditions {
			if node.Status.Conditions[i].Type == kapi.NodeNetworkUnavailable {
				condition := &node.Status.Conditions[i]
				if condition.Status != kapi.ConditionFalse && condition.Reason == "NoRouteCreated" {
					condition.Status = kapi.ConditionFalse
					condition.Reason = "RouteCreated"
					condition.Message = "ovn-kube cleared kubelet-set NoRouteCreated"
					condition.LastTransitionTime = metav1.Now()
					if err = oc.kube.UpdateNodeStatus(node); err == nil {
						cleared = true
					}
				}
				break
			}
		}
		return err
	})
	if resultErr != nil {
		klog.Errorf("status update failed for local node %s: %v", origNode.Name, resultErr)
	} else if cleared {
		klog.Infof("Cleared node NetworkUnavailable/NoRouteCreated condition for %s", origNode.Name)
	}
}

// this is the worker function that does the periodic sync of nodes from kube API
// and sbdb and deletes chassis that are stale
func (oc *Controller) syncNodesPeriodic() {
	//node names is a slice of all node names
	nodes, err := oc.kube.GetNodes()
	if err != nil {
		klog.Errorf("Error getting existing nodes from kube API: %v", err)
		return
	}

	nodeNames := make([]string, len(nodes.Items))

	for _, node := range nodes.Items {
		nodeNames = append(nodeNames, node.Name)
	}

	chassisData, stderr, err := util.RunOVNSbctl("--data=bare", "--no-heading",
		"--columns=name,hostname", "--format=json", "list", "Chassis")
	if err != nil {
		klog.Errorf("Failed to get chassis list: stderr: %s, error: %v",
			stderr, err)
		return
	}

	chassisMap, err := oc.unmarshalChassisDataIntoMap([]byte(chassisData))
	if err != nil {
		klog.Errorf("Failed to unmarshal chassis data into chassis map, error: %v: %s", err, chassisData)
		return
	}

	//delete existing nodes from the chassis map.
	for _, nodeName := range nodeNames {
		delete(chassisMap, nodeName)
	}

	for nodeName, chassisName := range chassisMap {
		if chassisName != "" {
			_, stderr, err = util.RunOVNSbctl("--if-exist", "chassis-del", chassisName)
			if err != nil {
				klog.Errorf("Failed to delete chassis with name %s for node %s: stderr: %s, error: %v",
					chassisName, nodeName, stderr, err)
			}
		}
	}
}

func (oc *Controller) syncNodes(nodes []interface{}) {
	foundNodes := make(map[string]*kapi.Node)
	for _, tmp := range nodes {
		node, ok := tmp.(*kapi.Node)
		if !ok {
			klog.Errorf("Spurious object in syncNodes: %v", tmp)
			continue
		}
		foundNodes[node.Name] = node
	}

	// We only deal with cleaning up nodes that shouldn't exist here, since
	// watchNodes() will be called for all existing nodes at startup anyway.
	// Note that this list will include the 'join' cluster switch, which we
	// do not want to delete.
	var subnetAttr string
	if config.IPv6Mode {
		subnetAttr = "ipv6_prefix"
	} else {
		subnetAttr = "subnet"
	}

	chassisData, stderr, err := util.RunOVNSbctl("--data=bare", "--no-heading",
		"--columns=name,hostname", "--format=json", "list", "Chassis")
	if err != nil {
		klog.Errorf("Failed to get chassis list: stderr: %q, error: %v",
			stderr, err)
		return
	}

	chassisMap, err := oc.unmarshalChassisDataIntoMap([]byte(chassisData))
	if err != nil {
		klog.Errorf("Failed to unmarshal chassis data into chassis map, error: %v: %s", err, chassisData)
		return
	}

	//delete existing nodes from the chassis map.
	for nodeName := range foundNodes {
		delete(chassisMap, nodeName)
	}

	nodeSwitches, stderr, err := util.RunOVNNbctl("--data=bare", "--no-heading",
		"--columns=name,other-config", "find", "logical_switch",
		"other-config:"+subnetAttr+"!=_")
	if err != nil {
		klog.Errorf("Failed to get node logical switches: stderr: %q, error: %v",
			stderr, err)
		return
	}

	type NodeSubnets struct {
		hostSubnet *net.IPNet
		joinSubnet *net.IPNet
	}
	NodeSubnetsMap := make(map[string]*NodeSubnets)
	for _, result := range strings.Split(nodeSwitches, "\n\n") {
		// Split result into name and other-config
		items := strings.Split(result, "\n")
		if len(items) != 2 || len(items[0]) == 0 {
			continue
		}
		isJoinSwitch := false
		nodeName := items[0]
		if strings.HasPrefix(items[0], util.JoinSwitchPrefix) {
			isJoinSwitch = true
			nodeName = strings.Split(items[0], "_")[1]
		}
		if _, ok := foundNodes[nodeName]; ok {
			// node still exists, no cleanup to do
			continue
		}

		var subnet *net.IPNet
		attrs := strings.Fields(items[1])
		for _, attr := range attrs {
			if strings.HasPrefix(attr, "subnet=") {
				subnetStr := strings.TrimPrefix(attr, "subnet=")
				_, subnet, _ = net.ParseCIDR(subnetStr)
				break
			} else if strings.HasPrefix(attr, "ipv6_prefix=") {
				prefixStr := strings.TrimPrefix(attr, "ipv6_prefix=")
				_, subnet, _ = net.ParseCIDR(prefixStr + "/64")
				break
			}
		}
		var tmp NodeSubnets
		nodeSubnets, ok := NodeSubnetsMap[nodeName]
		if !ok {
			nodeSubnets = &tmp
			NodeSubnetsMap[nodeName] = nodeSubnets
		}
		if isJoinSwitch {
			nodeSubnets.joinSubnet = subnet
		} else {
			nodeSubnets.hostSubnet = subnet
		}
	}

	for nodeName, nodeSubnets := range NodeSubnetsMap {
		if err := oc.deleteNode(nodeName, nodeSubnets.hostSubnet, nodeSubnets.joinSubnet); err != nil {
			klog.Error(err)
		}
		//remove the node from the chassis map so we don't delete it twice
		delete(chassisMap, nodeName)
	}

	for nodeName, chassisName := range chassisMap {
		if chassisName != "" {
			_, stderr, err = util.RunOVNSbctl("--if-exist", "chassis-del", chassisName)
			if err != nil {
				klog.Errorf("Failed to delete chassis with name %s for logical switch %s: stderr: %q, error: %v",
					chassisName, nodeName, stderr, err)
			}
		}
	}
}

func (oc *Controller) unmarshalChassisDataIntoMap(chData []byte) (map[string]string, error) {
	//map of node name to chassis name
	chassisMap := make(map[string]string)

	type chassisList struct {
		Data     [][]string
		Headings []string
	}
	var mapUnmarshal chassisList

	if len(chData) == 0 {
		return chassisMap, nil
	}

	err := json.Unmarshal(chData, &mapUnmarshal)

	if err != nil {
		return nil, fmt.Errorf("Error unmarshaling the chassis data: %s", err)
	}

	for _, chassis := range mapUnmarshal.Data {
		if len(chassis) < 2 || chassis[0] == "" || chassis[1] == "" {
			continue
		}
		chassisMap[chassis[1]] = chassis[0]
	}

	return chassisMap, nil
}

func (oc *Controller) deleteNodeChassis(nodeName string) error {
	chassisName, stderr, err := util.RunOVNSbctl("--data=bare", "--no-heading",
		"--columns=name", "find", "Chassis",
		"hostname="+nodeName)
	if err != nil {
		return fmt.Errorf("Failed to get chassis name for node %s: stderr: %q, error: %v",
			nodeName, stderr, err)
	}

	if chassisName == "" {
		klog.Warningf("Chassis name is empty for node: %s", nodeName)
	} else {
		_, stderr, err = util.RunOVNSbctl("--if-exist", "chassis-del", chassisName)
		if err != nil {
			return fmt.Errorf("Failed to delete chassis with name %s for node %s: stderr: %q, error: %v",
				chassisName, nodeName, stderr, err)
		}
	}

	return nil
}
