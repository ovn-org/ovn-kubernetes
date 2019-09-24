package cluster

import (
	"fmt"
	"net"
	"strings"

	kapi "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	"github.com/openshift/origin/pkg/util/netutils"
	"github.com/sirupsen/logrus"
)

// StartClusterMaster runs a subnet IPAM and a controller that watches arrival/departure
// of nodes in the cluster
// On an addition to the cluster (node create), a new subnet is created for it that will translate
// to creation of a logical switch (done by the node, but could be created here at the master process too)
// Upon deletion of a node, the switch will be deleted
//
// TODO: Verify that the cluster was not already called with a different global subnet
//  If true, then either quit or perform a complete reconfiguration of the cluster (recreate switches/routers with new subnet values)
func (cluster *OvnClusterController) StartClusterMaster(masterNodeName string) error {

	alreadyAllocated := make([]string, 0)
	existingNodes, err := cluster.Kube.GetNodes()
	if err != nil {
		logrus.Errorf("Error in initializing/fetching subnets: %v", err)
		return err
	}
	for _, node := range existingNodes.Items {
		hostsubnet, ok := node.Annotations[OvnHostSubnet]
		if ok {
			alreadyAllocated = append(alreadyAllocated, hostsubnet)
		}
	}
	masterSubnetAllocatorList := make([]*netutils.SubnetAllocator, 0)
	// NewSubnetAllocator is a subnet IPAM, which takes a CIDR (first argument)
	// and gives out subnets of length 'hostSubnetLength' (second argument)
	// but omitting any that exist in 'subrange' (third argument)
	for _, clusterEntry := range config.Default.ClusterSubnets {
		subrange := make([]string, 0)
		for _, allocatedRange := range alreadyAllocated {
			firstAddress, _, err := net.ParseCIDR(allocatedRange)
			if err != nil {
				return err
			}
			if clusterEntry.CIDR.Contains(firstAddress) {
				subrange = append(subrange, allocatedRange)
			}
		}
		subnetAllocator, err := netutils.NewSubnetAllocator(clusterEntry.CIDR.String(), 32-clusterEntry.HostSubnetLength, subrange)
		if err != nil {
			return err
		}
		masterSubnetAllocatorList = append(masterSubnetAllocatorList, subnetAllocator)
	}
	cluster.masterSubnetAllocatorList = masterSubnetAllocatorList

	if err := cluster.SetupMaster(masterNodeName); err != nil {
		logrus.Errorf("Failed to setup master (%v)", err)
		return err
	}

	// Watch all node events.  On creation, addNode will be called that will
	// create a subnet for the switch belonging to that node. On a delete
	// call, the subnet will be returned to the allocator as the switch is
	// deleted from ovn
	return cluster.watchNodes()
}

// SetupMaster creates the central router and load-balancers for the network
func (cluster *OvnClusterController) SetupMaster(masterNodeName string) error {
	if err := setupOVNMaster(masterNodeName); err != nil {
		return err
	}

	// Create a single common distributed router for the cluster.
	stdout, stderr, err := util.RunOVNNbctl("--", "--may-exist", "lr-add", OvnClusterRouter,
		"--", "set", "logical_router", OvnClusterRouter, "external_ids:k8s-cluster-router=yes")
	if err != nil {
		logrus.Errorf("Failed to create a single common distributed router for the cluster, "+
			"stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
		return err
	}

	// Create 2 load-balancers for east-west traffic.  One handles UDP and another handles TCP.
	cluster.TCPLoadBalancerUUID, stderr, err = util.RunOVNNbctl("--data=bare", "--no-heading", "--columns=_uuid", "find", "load_balancer", "external_ids:k8s-cluster-lb-tcp=yes")
	if err != nil {
		logrus.Errorf("Failed to get tcp load-balancer, stderr: %q, error: %v", stderr, err)
		return err
	}

	if cluster.TCPLoadBalancerUUID == "" {
		cluster.TCPLoadBalancerUUID, stderr, err = util.RunOVNNbctl("--", "create", "load_balancer", "external_ids:k8s-cluster-lb-tcp=yes", "protocol=tcp")
		if err != nil {
			logrus.Errorf("Failed to create tcp load-balancer, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
			return err
		}
	}

	cluster.UDPLoadBalancerUUID, stderr, err = util.RunOVNNbctl("--data=bare", "--no-heading", "--columns=_uuid", "find", "load_balancer", "external_ids:k8s-cluster-lb-udp=yes")
	if err != nil {
		logrus.Errorf("Failed to get udp load-balancer, stderr: %q, error: %v", stderr, err)
		return err
	}
	if cluster.UDPLoadBalancerUUID == "" {
		cluster.UDPLoadBalancerUUID, stderr, err = util.RunOVNNbctl("--", "create", "load_balancer", "external_ids:k8s-cluster-lb-udp=yes", "protocol=udp")
		if err != nil {
			logrus.Errorf("Failed to create udp load-balancer, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
			return err
		}
	}

	// Create a logical switch called "join" that will be used to connect gateway routers to the distributed router.
	// The "join" switch will be allocated IP addresses in the range 100.64.0.0/16.
	const joinSubnet string = "100.64.0.1/16"
	joinIP, joinCIDR, _ := net.ParseCIDR(joinSubnet)
	stdout, stderr, err = util.RunOVNNbctl("--may-exist", "ls-add", "join",
		"--", "set", "logical_switch", "join", fmt.Sprintf("other-config:subnet=%s", joinCIDR.String()),
		"--", "set", "logical_switch", "join", fmt.Sprintf("other-config:exclude_ips=%s", joinIP.String()))
	if err != nil {
		logrus.Errorf("Failed to create logical switch called \"join\", stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
		return err
	}

	// Connect the distributed router to "join".
	routerMac, stderr, err := util.RunOVNNbctl("--if-exist", "get", "logical_router_port", "rtoj-"+OvnClusterRouter, "mac")
	if err != nil {
		logrus.Errorf("Failed to get logical router port rtoj-%v, stderr: %q, error: %v", OvnClusterRouter, stderr, err)
		return err
	}
	if routerMac == "" {
		routerMac = util.GenerateMac()
		stdout, stderr, err = util.RunOVNNbctl("--", "--may-exist", "lrp-add", OvnClusterRouter,
			"rtoj-"+OvnClusterRouter, routerMac, joinSubnet)
		if err != nil {
			logrus.Errorf("Failed to add logical router port rtoj-%v, stdout: %q, stderr: %q, error: %v",
				OvnClusterRouter, stdout, stderr, err)
			return err
		}
	}

	// Connect the switch "join" to the router.
	stdout, stderr, err = util.RunOVNNbctl("--", "--may-exist", "lsp-add", "join", "jtor-"+OvnClusterRouter,
		"--", "set", "logical_switch_port", "jtor-"+OvnClusterRouter, "type=router",
		"options:router-port=rtoj-"+OvnClusterRouter, "addresses="+"\""+routerMac+"\"")
	if err != nil {
		logrus.Errorf("Failed to add router-type logical switch port to join, stdout: %q, stderr: %q, error: %v",
			stdout, stderr, err)
		return err
	}

	return nil
}

func parseNodeHostSubnet(node *kapi.Node) (*net.IPNet, error) {
	sub, ok := node.Annotations[OvnHostSubnet]
	if !ok {
		return nil, fmt.Errorf("Error in obtaining host subnet for node %q for deletion", node.Name)
	}

	_, subnet, err := net.ParseCIDR(sub)
	if err != nil {
		return nil, fmt.Errorf("Error in parsing hostsubnet - %v", err)
	}

	return subnet, nil
}

func (cluster *OvnClusterController) ensureNodeLogicalNetwork(nodeName string, hostsubnet *net.IPNet) error {
	ip := util.NextIP(hostsubnet.IP)
	n, _ := hostsubnet.Mask.Size()
	firstIP := fmt.Sprintf("%s/%d", ip.String(), n)

	nodeLRPMac, stderr, err := util.RunOVNNbctl("--if-exist", "get", "logical_router_port", "rtos-"+nodeName, "mac")
	if err != nil {
		logrus.Errorf("Failed to get logical router port,stderr: %q, error: %v", stderr, err)
		return err
	}
	if nodeLRPMac == "" {
		nodeLRPMac = util.GenerateMac()
	}

	// Create a router port and provide it the first address on the node's host subnet
	_, stderr, err = util.RunOVNNbctl("--may-exist", "lrp-add", OvnClusterRouter, "rtos-"+nodeName, nodeLRPMac, firstIP)
	if err != nil {
		logrus.Errorf("Failed to add logical port to router, stderr: %q, error: %v", stderr, err)
		return err
	}

	// Skip the second address of the LogicalSwitch's subnet since we set it aside for the
	// management port on that node.
	secondIP := util.NextIP(ip)

	// Create a logical switch and set its subnet.
	stdout, stderr, err := util.RunOVNNbctl("--", "--may-exist", "ls-add", nodeName,
		"--", "set", "logical_switch", nodeName, "other-config:subnet="+hostsubnet.String(),
		"other-config:exclude_ips="+secondIP.String(),
		"external-ids:gateway_ip="+firstIP)
	if err != nil {
		logrus.Errorf("Failed to create a logical switch %v, stdout: %q, stderr: %q, error: %v", nodeName, stdout, stderr, err)
		return err
	}

	// Connect the switch to the router.
	stdout, stderr, err = util.RunOVNNbctl("--", "--may-exist", "lsp-add", nodeName, "stor-"+nodeName,
		"--", "set", "logical_switch_port", "stor-"+nodeName, "type=router", "options:router-port=rtos-"+nodeName, "addresses="+"\""+nodeLRPMac+"\"")
	if err != nil {
		logrus.Errorf("Failed to add logical port to switch, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
		return err
	}

	// Add our cluster TCP and UDP load balancers to the node switch
	if cluster.TCPLoadBalancerUUID == "" {
		return fmt.Errorf("TCP cluster load balancer not created")
	}
	stdout, stderr, err = util.RunOVNNbctl("set", "logical_switch", nodeName, "load_balancer="+cluster.TCPLoadBalancerUUID)
	if err != nil {
		logrus.Errorf("Failed to set logical switch %v's loadbalancer, stdout: %q, stderr: %q, error: %v", nodeName, stdout, stderr, err)
		return err
	}

	if cluster.UDPLoadBalancerUUID == "" {
		return fmt.Errorf("UDP cluster load balancer not created")
	}
	stdout, stderr, err = util.RunOVNNbctl("add", "logical_switch", nodeName, "load_balancer", cluster.UDPLoadBalancerUUID)
	if err != nil {
		logrus.Errorf("Failed to add logical switch %v's loadbalancer, stdout: %q, stderr: %q, error: %v", nodeName, stdout, stderr, err)
		return err
	}

	return nil
}

func (cluster *OvnClusterController) addNode(node *kapi.Node) (err error) {
	hostsubnet, _ := parseNodeHostSubnet(node)
	if hostsubnet != nil {
		// Node already has subnet assigned; ensure its logical network is set up
		return cluster.ensureNodeLogicalNetwork(node.Name, hostsubnet)
	}

	// Node doesn't have a subnet assigned; reserve a new one for it
	var subnetAllocator *netutils.SubnetAllocator
	err = netutils.ErrSubnetAllocatorFull
	for _, subnetAllocator = range cluster.masterSubnetAllocatorList {
		hostsubnet, err = subnetAllocator.GetNetwork()
		if err == netutils.ErrSubnetAllocatorFull {
			// Current subnet exhausted, check next possible subnet
			continue
		} else if err != nil {
			return fmt.Errorf("Error allocating network for node %s: %v", node.Name, err)
		}
		logrus.Infof("Allocated node %s HostSubnet %s", node.Name, hostsubnet.String())
		break
	}
	if err == netutils.ErrSubnetAllocatorFull {
		return fmt.Errorf("Error allocating network for node %s: %v", node.Name, err)
	}

	defer func() {
		// Release the allocation on error
		if err != nil {
			_ = subnetAllocator.ReleaseNetwork(hostsubnet)
		}
	}()

	// Ensure that the node's logical network has been created
	err = cluster.ensureNodeLogicalNetwork(node.Name, hostsubnet)
	if err != nil {
		return err
	}

	// Set the HostSubnet annotation on the node object to signal
	// to nodes that their logical infrastructure is set up and they can
	// proceed with their initialization
	err = cluster.Kube.SetAnnotationOnNode(node, OvnHostSubnet, hostsubnet.String())
	if err != nil {
		logrus.Errorf("Failed to set node %s host subnet annotation to %q: %v",
			node.Name, hostsubnet.String(), err)
		return err
	}

	return nil
}

func (cluster *OvnClusterController) deleteNodeHostSubnet(nodeName string, subnet *net.IPNet) error {
	for _, possibleSubnet := range cluster.masterSubnetAllocatorList {
		if err := possibleSubnet.ReleaseNetwork(subnet); err == nil {
			logrus.Infof("Deleted HostSubnet %v for node %s", subnet, nodeName)
			return nil
		}
	}
	// SubnetAllocator.network is an unexported field so the only way to figure out if a subnet is in a network is to try and delete it
	// if deletion succeeds then stop iterating, if the list is exhausted the node subnet wasn't deleteted return err
	return fmt.Errorf("Error deleting subnet %v for node %q: subnet not found in any CIDR range or already available", subnet, nodeName)
}

func (cluster *OvnClusterController) deleteNodeLogicalNetwork(nodeName string) error {
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

func (cluster *OvnClusterController) deleteNode(nodeName string, nodeSubnet *net.IPNet) error {
	// Clean up as much as we can but don't hard error
	if nodeSubnet != nil {
		if err := cluster.deleteNodeHostSubnet(nodeName, nodeSubnet); err != nil {
			logrus.Errorf("Error deleting node %s HostSubnet: %v", nodeName, err)
		}
	}

	if err := cluster.deleteNodeLogicalNetwork(nodeName); err != nil {
		logrus.Errorf("Error deleting node %s logical network: %v", nodeName, err)
	}

	if nodeSubnet != nil {
		if err := util.GatewayCleanup(nodeName, nodeSubnet.String()); err != nil {
			return fmt.Errorf("Failed to clean up node %s gateway: (%v)", nodeName, err)
		}
	}

	return nil
}

func (cluster *OvnClusterController) syncNodes(nodes []interface{}) {
	foundNodes := make(map[string]*kapi.Node)
	for _, tmp := range nodes {
		node, ok := tmp.(*kapi.Node)
		if !ok {
			logrus.Errorf("Spurious object in syncNodes: %v", tmp)
			continue
		}
		foundNodes[node.Name] = node
	}

	// We only deal with cleaning up nodes that shouldn't exist here, since
	// watchNodes() will be called for all existing nodes at startup anyway.
	// Note that this list will include the 'join' cluster switch, which we
	// do not want to delete.
	nodeSwitches, stderr, err := util.RunOVNNbctl("--data=bare", "--no-heading",
		"--columns=name,other-config", "find", "logical_switch", "other-config:subnet!=_")
	if err != nil {
		logrus.Errorf("Failed to get node logical switches: stderr: %q, error: %v",
			stderr, err)
		return
	}
	for _, result := range strings.Split(nodeSwitches, "\n\n") {
		// Split result into name and other-config
		items := strings.Split(result, "\n")
		if len(items) != 2 || len(items[0]) == 0 {
			continue
		}
		if items[0] == "join" {
			// Don't delete the cluster switch
			continue
		}
		if _, ok := foundNodes[items[0]]; ok {
			// node still exists, no cleanup to do
			continue
		}

		var subnet *net.IPNet
		configs := strings.Fields(items[1])
		for _, config := range configs {
			if strings.HasPrefix(config, "subnet=") {
				subnetStr := strings.TrimPrefix(config, "subnet=")
				_, subnet, _ = net.ParseCIDR(subnetStr)
				break
			}
		}

		if err := cluster.deleteNode(items[0], subnet); err != nil {
			logrus.Error(err)
		}
	}
}

func (cluster *OvnClusterController) watchNodes() error {
	_, err := cluster.watchFactory.AddNodeHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			node := obj.(*kapi.Node)
			logrus.Debugf("Added event for Node %q", node.Name)
			err := cluster.addNode(node)
			if err != nil {
				logrus.Errorf("error creating subnet for node %s: %v", node.Name, err)
			}
		},
		UpdateFunc: func(old, new interface{}) {},
		DeleteFunc: func(obj interface{}) {
			node := obj.(*kapi.Node)
			logrus.Debugf("Delete event for Node %q", node.Name)
			nodeSubnet, _ := parseNodeHostSubnet(node)
			err := cluster.deleteNode(node.Name, nodeSubnet)
			if err != nil {
				logrus.Error(err)
			}
		},
	}, cluster.syncNodes)
	return err
}
