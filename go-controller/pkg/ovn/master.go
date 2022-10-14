package ovn

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	kapi "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/sbdb"

	hotypes "github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/types"
	houtil "github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/util"
	ovnlb "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/loadbalancer"
	lsm "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/logical_switch_manager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

const (
	// IdledServiceAnnotationSuffix is a constant string representing the suffix of the Service annotation key
	// whose value indicates the time stamp in RFC3339 format when a Service was idled
	IdledServiceAnnotationSuffix   = "idled-at"
	OvnNodeAnnotationRetryInterval = 100 * time.Millisecond
	OvnNodeAnnotationRetryTimeout  = 1 * time.Second
)

type ovnkubeMasterLeaderMetrics struct{}

func (ovnkubeMasterLeaderMetrics) On(string) {
	metrics.MetricMasterLeader.Set(1)
}

func (ovnkubeMasterLeaderMetrics) Off(string) {
	metrics.MetricMasterLeader.Set(0)
}

type ovnkubeMasterLeaderMetricsProvider struct{}

func (_ ovnkubeMasterLeaderMetricsProvider) NewLeaderMetric() leaderelection.SwitchMetric {
	return ovnkubeMasterLeaderMetrics{}
}

// Start waits until this process is the leader before starting master functions
func (cm *ControllerManager) Start(ctx context.Context, cancel context.CancelFunc) error {
	// Set up leader election process first
	rl, err := resourcelock.New(
		// TODO (rravaiol) (bpickard)
		// https://github.com/kubernetes/kubernetes/issues/107454
		// leader election library no longer supports leader-election
		// locks based solely on `endpoints` or `configmaps` resources.
		// Slowly migrating to new API across three releases; with k8s 1.24
		// we're now in the second step ('x+2') bullet from the link above).
		// This will have to be updated for the next k8s bump: to 1.26.
		resourcelock.LeasesResourceLock,
		config.Kubernetes.OVNConfigNamespace,
		"ovn-kubernetes-master",
		cm.client.CoreV1(),
		cm.client.CoordinationV1(),
		resourcelock.ResourceLockConfig{
			Identity:      cm.identity,
			EventRecorder: cm.recorder,
		},
	)
	if err != nil {
		return err
	}

	lec := leaderelection.LeaderElectionConfig{
		Lock:            rl,
		LeaseDuration:   time.Duration(config.MasterHA.ElectionLeaseDuration) * time.Second,
		RenewDeadline:   time.Duration(config.MasterHA.ElectionRenewDeadline) * time.Second,
		RetryPeriod:     time.Duration(config.MasterHA.ElectionRetryPeriod) * time.Second,
		ReleaseOnCancel: true,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				klog.Infof("Won leader election; in active mode")
				start := time.Now()
				defer func() {
					end := time.Since(start)
					metrics.MetricMasterReadyDuration.Set(end.Seconds())
				}()

				if err := cm.Init(); err != nil {
					klog.Error(err)
					cancel()
					return
				}

				if err = cm.Run(); err != nil {
					klog.Error(err)
					cancel()
					return
				}

				// if no default network net-attach-def exists, we'd need to start it now
				if err := cm.defaultController.Start(ctx); err != nil {
					klog.Error(err)
					cancel()
					return
				}
			},
			OnStoppedLeading: func() {
				//This node was leader and it lost the election.
				// Whenever the node transitions from leader to follower,
				// we need to handle the transition properly like clearing
				// the cache.
				klog.Infof("No longer leader; exiting")
				cancel()
			},
			OnNewLeader: func(newLeaderName string) {
				if newLeaderName != cm.identity {
					klog.Infof("Lost the election to %s; in standby mode", newLeaderName)
				}
			},
		},
	}

	leaderelection.SetProvider(ovnkubeMasterLeaderMetricsProvider{})
	leaderElector, err := leaderelection.NewLeaderElector(lec)
	if err != nil {
		return err
	}

	cm.defaultWg.Add(1)
	go func() {
		leaderElector.Run(ctx)
		klog.Infof("Stopped leader election")
		cm.defaultWg.Done()
	}()

	return nil
}

// cleanup obsolete *gressDefaultDeny port groups
func (oc *Controller) upgradeToNamespacedDenyPGOVNTopology(existingNodeList *kapi.NodeList) error {
	err := libovsdbops.DeletePortGroups(oc.nbClient, "ingressDefaultDeny", "egressDefaultDeny")
	if err != nil {
		klog.Errorf("%v", err)
	}
	return nil
}

// delete obsoleted logical OVN entities that are specific for Multiple join switches OVN topology. Also cleanup
// OVN entities for deleted nodes (similar to syncNodes() but for obsoleted Multiple join switches OVN topology)
func (oc *Controller) upgradeToSingleSwitchOVNTopology(existingNodeList *kapi.NodeList) error {
	existingNodes := make(map[string]bool)
	for _, node := range existingNodeList.Items {
		existingNodes[node.Name] = true

		// delete the obsoleted node-join-subnets annotation
		err := oc.kube.SetAnnotationsOnNode(node.Name, map[string]interface{}{"k8s.ovn.org/node-join-subnets": nil})
		if err != nil {
			klog.Errorf("Failed to remove node-join-subnets annotation for node %s", node.Name)
		}
	}

	p := func(item *nbdb.LogicalSwitch) bool {
		return strings.HasPrefix(item.Name, types.JoinSwitchPrefix)
	}
	legacyJoinSwitches, err := libovsdbops.FindLogicalSwitchesWithPredicate(oc.nbClient, p)
	if err != nil {
		klog.Errorf("Failed to remove any legacy per node join switches")
	}

	for _, legacyJoinSwitch := range legacyJoinSwitches {
		// if the node was deleted when ovn-master was down, delete its per-node switch
		nodeName := strings.TrimPrefix(legacyJoinSwitch.Name, types.JoinSwitchPrefix)
		upgradeOnly := true
		if _, ok := existingNodes[nodeName]; !ok {
			_ = oc.deleteNodeLogicalNetwork(nodeName)
			upgradeOnly = false
		}

		// for all nodes include the ones that were deleted, delete its gateway entities.
		// See comments above the multiJoinSwitchGatewayCleanup() function for details.
		err := oc.multiJoinSwitchGatewayCleanup(nodeName, upgradeOnly)
		if err != nil {
			return err
		}
	}
	return nil
}

func (oc *Controller) upgradeOVNTopology(existingNodes *kapi.NodeList) error {
	ver, err := oc.determineOVNTopoVersionFromOVN()
	if err != nil {
		return err
	}

	// If current DB version is greater than OvnSingleJoinSwitchTopoVersion, no need to upgrade to single switch topology
	if ver < types.OvnSingleJoinSwitchTopoVersion {
		klog.Infof("Upgrading to Single Switch OVN Topology")
		err = oc.upgradeToSingleSwitchOVNTopology(existingNodes)
	}
	if err == nil && ver < types.OvnNamespacedDenyPGTopoVersion {
		klog.Infof("Upgrading to Namespace Deny PortGroup OVN Topology")
		err = oc.upgradeToNamespacedDenyPGOVNTopology(existingNodes)
	}
	// If version is less than Host -> Service with OpenFlow, we need to remove and cleanup DGP
	if err == nil && ((ver < types.OvnHostToSvcOFTopoVersion && config.Gateway.Mode == config.GatewayModeShared) ||
		(ver < types.OvnRoutingViaHostTopoVersion)) {
		err = oc.cleanupDGP(existingNodes)
	}
	return err
}

// StartClusterMaster runs a subnet IPAM and a controller that watches arrival/departure
// of nodes in the cluster
// On an addition to the cluster (node create), a new subnet is created for it that will translate
// to creation of a logical switch (done by the node, but could be created here at the master process too)
// Upon deletion of a node, the switch will be deleted
//
// TODO: Verify that the cluster was not already called with a different global subnet
//
//	If true, then either quit or perform a complete reconfiguration of the cluster (recreate switches/routers with new subnet values)
func (oc *Controller) StartClusterMaster() error {
	klog.Infof("Starting cluster master for network %s", oc.nadInfo.NetName)

	// TBD secondary network?
	if !oc.nadInfo.IsSecondary {
		metrics.RegisterMasterPerformance(oc.nbClient)
		metrics.RegisterMasterFunctional()
	}

	existingNodes, err := oc.kube.GetNodes()
	if err != nil {
		klog.Errorf("Error in fetching nodes: %v", err)
		return err
	}
	klog.V(5).Infof("Existing number of nodes: %d", len(existingNodes.Items))
	err = oc.upgradeOVNTopology(existingNodes)
	if err != nil {
		klog.Errorf("Failed to upgrade OVN topology to version %d: %v", types.OvnCurrentTopologyVersion, err)
		return err
	}

	klog.Infof("Allocating subnets for network %s", oc.nadInfo.NetName)
	var v4HostSubnetCount, v6HostSubnetCount float64
	for _, clusterEntry := range oc.clusterSubnets {
		err := oc.masterSubnetAllocator.AddNetworkRange(clusterEntry.CIDR, clusterEntry.HostSubnetLength)
		if err != nil {
			return err
		}
		klog.V(5).Infof("Added network range %s to the allocator", clusterEntry.CIDR)
		util.CalculateHostSubnetsForClusterEntry(clusterEntry, &v4HostSubnetCount, &v6HostSubnetCount)
	}
	if config.HybridOverlay.Enabled && !oc.nadInfo.IsSecondary {
		for _, clusterEntry := range config.HybridOverlay.ClusterSubnets {
			err := oc.hybridOverlaySubnetAllocator.AddNetworkRange(clusterEntry.CIDR, clusterEntry.HostSubnetLength)
			if err != nil {
				return err
			}
			klog.V(5).Infof("Added network range %s to the hybrid overlay allocator", clusterEntry.CIDR)
		}
	}
	nodeNames := []string{}
	for _, node := range existingNodes.Items {
		nodeNames = append(nodeNames, node.Name)
	}

	// update metrics for host subnets
	metrics.RecordSubnetCount(v4HostSubnetCount, v6HostSubnetCount)

	if !oc.nadInfo.IsSecondary {
		if oc.multicastSupport {
			if _, _, err := util.RunOVNSbctl("--columns=_uuid", "list", "IGMP_Group"); err != nil {
				klog.Warningf("Multicast support enabled, however version of OVN in use does not support IGMP Group. " +
					"Disabling Multicast Support")
				oc.multicastSupport = false
			}
		}

		err = oc.createACLLoggingMeter()
		if err != nil {
			klog.Warningf("ACL logging support enabled, however acl-logging meter could not be created: %v. "+
				"Disabling ACL logging support", err)
			oc.aclLoggingEnabled = false
		}

		// FIXME: When https://github.com/ovn-org/libovsdb/issues/235 is fixed,
		// use IsTableSupported(nbdb.LoadBalancerGroup).
		if _, _, err := util.RunOVNNbctl("--columns=_uuid", "list", "Load_Balancer_Group"); err != nil {
			klog.Warningf("Load Balancer Group support enabled, however version of OVN in use does not support Load Balancer Groups.")
		} else {
			loadBalancerGroup := nbdb.LoadBalancerGroup{
				Name: types.ClusterLBGroupName,
			}
			err := libovsdbops.CreateOrUpdateLoadBalancerGroup(oc.nbClient, &loadBalancerGroup)
			if err != nil {
				klog.Errorf("Error creating cluster-wide load balancer group %s: %v", types.ClusterLBGroupName, err)
				return err
			}
			oc.loadBalancerGroupUUID = loadBalancerGroup.UUID
		}
	} else {
		// TBD, not support multicast for secondary network for now
		oc.multicastSupport = false
	}

	if err := oc.SetupMaster(nodeNames); err != nil {
		klog.Errorf("Failed to setup master (%v)", err)
		return err
	}

	return nil
}

// SetupMaster creates the central router and load-balancers for the network
func (oc *Controller) SetupMaster(existingNodeNames []string) error {
	clusterRouterName := oc.nadInfo.Prefix + types.OVNClusterRouter

	// Create a single common distributed router for the cluster.
	logicalRouter := nbdb.LogicalRouter{
		Name: clusterRouterName,
		ExternalIDs: map[string]string{
			"k8s-cluster-router": "yes",
		},
		Options: map[string]string{
			"always_learn_from_arp_request": "false",
		},
	}
	if oc.nadInfo.IsSecondary {
		logicalRouter.ExternalIDs["network_name"] = oc.nadInfo.NetName
	}
	if oc.multicastSupport {
		logicalRouter.Options["mcast_relay"] = "true"
	}

	err := libovsdbops.CreateOrUpdateLogicalRouter(oc.nbClient, &logicalRouter)
	if err != nil {
		return fmt.Errorf("failed to create a single common distributed router for the cluster network %s, error: %v", oc.nadInfo.NetName, err)
	}

	if oc.nadInfo.IsSecondary {
		return nil
	}

	// Create a cluster-wide port group that all logical switch ports are part of
	pg := libovsdbops.BuildPortGroup(types.ClusterPortGroupName, types.ClusterPortGroupName, nil, nil)
	err = libovsdbops.CreateOrUpdatePortGroups(oc.nbClient, pg)
	if err != nil {
		klog.Errorf("Failed to create cluster port group: %v", err)
		return err
	}

	// Create a cluster-wide port group with all node-to-cluster router
	// logical switch ports.  Currently the only user is multicast but it might
	// be used for other features in the future.
	pg = libovsdbops.BuildPortGroup(types.ClusterRtrPortGroupName, types.ClusterRtrPortGroupName, nil, nil)
	err = libovsdbops.CreateOrUpdatePortGroups(oc.nbClient, pg)
	if err != nil {
		klog.Errorf("Failed to create cluster port group: %v", err)
		return err
	}

	// If supported, enable IGMP relay on the router to forward multicast
	// traffic between nodes.
	if oc.multicastSupport {
		// Drop IP multicast globally. Multicast is allowed only if explicitly
		// enabled in a namespace.
		if err := oc.createDefaultDenyMulticastPolicy(); err != nil {
			klog.Errorf("Failed to create default deny multicast policy, error: %v", err)
			return err
		}

		// Allow IP multicast from node switch to cluster router and from
		// cluster router to node switch.
		if err := oc.createDefaultAllowMulticastPolicy(); err != nil {
			klog.Errorf("Failed to create default deny multicast policy, error: %v", err)
			return err
		}
	}

	// Create OVNJoinSwitch that will be used to connect gateway routers to the distributed router.
	logicalSwitch := nbdb.LogicalSwitch{
		Name: types.OVNJoinSwitch,
	}
	// nothing is updated here, so no reason to pass fields
	err = libovsdbops.CreateOrUpdateLogicalSwitch(oc.nbClient, &logicalSwitch)
	if err != nil {
		return fmt.Errorf("failed to create logical switch %+v: %v", logicalSwitch, err)
	}

	// Initialize the OVNJoinSwitch switch IP manager
	// The OVNJoinSwitch will be allocated IP addresses in the range 100.64.0.0/16 or fd98::/64.
	oc.joinSwIPManager, err = lsm.NewJoinLogicalSwitchIPManager(oc.nbClient, logicalSwitch.UUID, existingNodeNames)
	if err != nil {
		return err
	}

	// Allocate IPs for logical router port "GwRouterToJoinSwitchPrefix + OVNClusterRouter". This should always
	// allocate the first IPs in the join switch subnets
	gwLRPIfAddrs, err := oc.joinSwIPManager.EnsureJoinLRPIPs(types.OVNClusterRouter)
	if err != nil {
		return fmt.Errorf("failed to allocate join switch IP address connected to %s: %v", types.OVNClusterRouter, err)
	}

	// Connect the distributed router to OVNJoinSwitch.
	drSwitchPort := types.JoinSwitchToGWRouterPrefix + types.OVNClusterRouter
	drRouterPort := types.GWRouterToJoinSwitchPrefix + types.OVNClusterRouter

	gwLRPMAC := util.IPAddrToHWAddr(gwLRPIfAddrs[0].IP)
	gwLRPNetworks := []string{}
	for _, gwLRPIfAddr := range gwLRPIfAddrs {
		gwLRPNetworks = append(gwLRPNetworks, gwLRPIfAddr.String())
	}
	logicalRouterPort := nbdb.LogicalRouterPort{
		Name:     drRouterPort,
		MAC:      gwLRPMAC.String(),
		Networks: gwLRPNetworks,
	}

	err = libovsdbops.CreateOrUpdateLogicalRouterPorts(oc.nbClient, &logicalRouter,
		[]*nbdb.LogicalRouterPort{&logicalRouterPort}, &logicalRouterPort.MAC, &logicalRouterPort.Networks)
	if err != nil {
		return fmt.Errorf("failed to add logical router port %+v on router %+v: %v", logicalRouterPort, logicalRouter, err)
	}

	// Create OVNJoinSwitch that will be used to connect gateway routers to the
	// distributed router and connect it to said dsitributed router.
	logicalSwitchPort := nbdb.LogicalSwitchPort{
		Name: drSwitchPort,
		Type: "router",
		Options: map[string]string{
			"router-port": drRouterPort,
		},
		Addresses: []string{"router"},
	}
	sw := nbdb.LogicalSwitch{Name: types.OVNJoinSwitch}
	err = libovsdbops.CreateOrUpdateLogicalSwitchPortsOnSwitch(oc.nbClient, &sw, &logicalSwitchPort)
	if err != nil {
		return fmt.Errorf("failed to create logical switch port %+v and switch %s: %v", logicalSwitchPort, types.OVNJoinSwitch, err)
	}

	// Create default gateway Control Plane Protection (COPP) entry for gateway routers
	if oc.defaultGatewayCOPPUUID, err = EnsureDefaultCOPP(oc.nbClient); err != nil {
		return fmt.Errorf("unable to create gateway router control plane protection: %w", err)
	}

	return nil
}

func (oc *Controller) syncNodeManagementPort(node *kapi.Node, hostSubnets []*net.IPNet) error {
	// management port is not needed for non-default network
	if oc.nadInfo.IsSecondary {
		return nil
	}

	macAddress, err := util.ParseNodeManagementPortMACAddress(node)
	if err != nil {
		return err
	}

	if hostSubnets == nil {
		hostSubnets, err = util.ParseNodeHostSubnetAnnotation(node, oc.nadInfo.NetName)
		if err != nil {
			return err
		}
	}

	var v4Subnet *net.IPNet
	addresses := macAddress.String()
	for _, hostSubnet := range hostSubnets {
		mgmtIfAddr := util.GetNodeManagementIfAddr(hostSubnet)
		addresses += " " + mgmtIfAddr.IP.String()

		if err := addAllowACLFromNode(node.Name, mgmtIfAddr.IP, oc.nbClient); err != nil {
			return err
		}

		if !utilnet.IsIPv6CIDR(hostSubnet) {
			v4Subnet = hostSubnet
		}

		if config.Gateway.Mode == config.GatewayModeLocal {
			lrsr := nbdb.LogicalRouterStaticRoute{
				Policy:   &nbdb.LogicalRouterStaticRoutePolicySrcIP,
				IPPrefix: hostSubnet.String(),
				Nexthop:  mgmtIfAddr.IP.String(),
			}
			p := func(item *nbdb.LogicalRouterStaticRoute) bool {
				return item.IPPrefix == lrsr.IPPrefix && item.Nexthop == lrsr.Nexthop && item.Policy != nil && *item.Policy == *lrsr.Policy
			}
			err := libovsdbops.CreateOrUpdateLogicalRouterStaticRoutesWithPredicate(oc.nbClient, types.OVNClusterRouter,
				&lrsr, p)
			if err != nil {
				return fmt.Errorf("error creating static route %+v on router %s: %v", lrsr, types.OVNClusterRouter, err)
			}
		}
	}

	// Create this node's management logical port on the node switch
	logicalSwitchPort := nbdb.LogicalSwitchPort{
		Name:      types.K8sPrefix + node.Name,
		Addresses: []string{addresses},
	}
	sw := nbdb.LogicalSwitch{Name: node.Name}
	err = libovsdbops.CreateOrUpdateLogicalSwitchPortsOnSwitch(oc.nbClient, &sw, &logicalSwitchPort)
	if err != nil {
		return err
	}

	err = libovsdbops.AddPortsToPortGroup(oc.nbClient, types.ClusterPortGroupName, logicalSwitchPort.UUID)
	if err != nil {
		klog.Errorf(err.Error())
		return err
	}

	if v4Subnet != nil {
		if err := util.UpdateNodeSwitchExcludeIPs(oc.nbClient, node.Name, v4Subnet); err != nil {
			return err
		}
	}

	return nil
}

func (oc *Controller) syncGatewayLogicalNetwork(node *kapi.Node, l3GatewayConfig *util.L3GatewayConfig,
	hostSubnets []*net.IPNet, hostAddrs sets.String) error {
	var err error
	var gwLRPIPs, clusterSubnets []*net.IPNet

	if oc.nadInfo.IsSecondary {
		return nil
	}
	for _, clusterSubnet := range oc.clusterSubnets {
		clusterSubnets = append(clusterSubnets, clusterSubnet.CIDR)
	}

	gwLRPIPs, err = oc.joinSwIPManager.EnsureJoinLRPIPs(node.Name)
	if err != nil {
		return fmt.Errorf("failed to allocate join switch port IP address for node %s: %v", node.Name, err)
	}

	drLRPIPs, _ := oc.joinSwIPManager.EnsureJoinLRPIPs(types.OVNClusterRouter)
	err = oc.gatewayInit(node.Name, clusterSubnets, hostSubnets, l3GatewayConfig, oc.SCTPSupport, gwLRPIPs, drLRPIPs)
	if err != nil {
		return fmt.Errorf("failed to init shared interface gateway: %v", err)
	}

	for _, subnet := range hostSubnets {
		hostIfAddr := util.GetNodeManagementIfAddr(subnet)
		l3GatewayConfigIP, err := util.MatchIPNetFamily(utilnet.IsIPv6(hostIfAddr.IP), l3GatewayConfig.IPAddresses)
		if err != nil {
			return err
		}
		relevantHostIPs, err := util.MatchAllIPStringFamily(utilnet.IsIPv6(hostIfAddr.IP), hostAddrs.List())
		if err != nil && err != util.NoIPError {
			return err
		}
		if err := oc.addPolicyBasedRoutes(node.Name, hostIfAddr.IP.String(), l3GatewayConfigIP, relevantHostIPs); err != nil {
			return err
		}
	}

	return err
}

// syncNodeClusterRouterPort ensures a node's LS to the cluster router's LRP is created.
// NOTE: We could have created the router port in ensureNodeLogicalNetwork() instead of here,
// but chassis ID is not available at that moment. We need the chassis ID to set the
// gateway-chassis, which in effect pins the logical switch to the current node in OVN.
// Otherwise, ovn-controller will flood-fill unrelated datapaths unnecessarily, causing scale
// problems.
func (oc *Controller) syncNodeClusterRouterPort(node *kapi.Node, hostSubnets []*net.IPNet) error {
	chassisID, err := util.ParseNodeChassisIDAnnotation(node)
	if err != nil {
		return err
	}

	if hostSubnets == nil {
		hostSubnets, err = util.ParseNodeHostSubnetAnnotation(node, oc.nadInfo.NetName)
		if err != nil {
			return err
		}
	}

	// logical router port MAC is based on IPv4 subnet if there is one, else IPv6
	var nodeLRPMAC net.HardwareAddr
	for _, hostSubnet := range hostSubnets {
		gwIfAddr := util.GetNodeGatewayIfAddr(hostSubnet)
		nodeLRPMAC = util.IPAddrToHWAddr(gwIfAddr.IP)
		if !utilnet.IsIPv6CIDR(hostSubnet) {
			break
		}
	}

	switchName := oc.nadInfo.Prefix + node.Name
	clusterRouterName := oc.nadInfo.Prefix + types.OVNClusterRouter
	lrpName := types.RouterToSwitchPrefix + switchName
	lrpNetworks := []string{}
	for _, hostSubnet := range hostSubnets {
		gwIfAddr := util.GetNodeGatewayIfAddr(hostSubnet)
		lrpNetworks = append(lrpNetworks, gwIfAddr.String())
	}
	logicalRouterPort := nbdb.LogicalRouterPort{
		Name:     lrpName,
		MAC:      nodeLRPMAC.String(),
		Networks: lrpNetworks,
	}
	logicalRouter := nbdb.LogicalRouter{Name: clusterRouterName}

	err = libovsdbops.CreateOrUpdateLogicalRouterPorts(oc.nbClient, &logicalRouter,
		[]*nbdb.LogicalRouterPort{&logicalRouterPort}, &logicalRouterPort.MAC, &logicalRouterPort.Networks)
	if err != nil {
		klog.Errorf("Failed to add logical router port %+v to router %s: %v", logicalRouterPort, types.OVNClusterRouter, err)
		return err
	}

	gatewayChassisName := lrpName + "-" + chassisID
	gatewayChassis := nbdb.GatewayChassis{
		Name:        gatewayChassisName,
		ChassisName: chassisID,
		Priority:    1,
	}

	err = libovsdbops.CreateOrUpdateGatewayChassis(oc.nbClient, &logicalRouterPort, &gatewayChassis,
		&gatewayChassis.Name, &gatewayChassis.ChassisName, &gatewayChassis.Priority)
	if err != nil {
		klog.Errorf("Failed to add gateway chassis %s to logical router port %s, error: %v", chassisID, lrpName, err)
		return err
	}

	return nil
}

func (oc *Controller) ensureNodeLogicalNetwork(node *kapi.Node, hostSubnets []*net.IPNet) error {
	// logical router port MAC is based on IPv4 subnet if there is one, else IPv6
	var nodeLRPMAC net.HardwareAddr
	nodeName := node.Name
	for _, hostSubnet := range hostSubnets {
		gwIfAddr := util.GetNodeGatewayIfAddr(hostSubnet)
		nodeLRPMAC = util.IPAddrToHWAddr(gwIfAddr.IP)
		if !utilnet.IsIPv6CIDR(hostSubnet) {
			break
		}
	}

	switchName := oc.nadInfo.Prefix + nodeName
	logicalSwitch := nbdb.LogicalSwitch{
		Name: switchName,
	}
	if oc.nadInfo.IsSecondary {
		logicalSwitch.ExternalIDs = map[string]string{"network_name": oc.nadInfo.NetName}
	}

	var v4Gateway, v6Gateway net.IP
	var hostNetworkPolicyIPs []net.IP
	logicalRouterPortNetwork := []string{}
	logicalSwitch.OtherConfig = map[string]string{}
	for _, hostSubnet := range hostSubnets {
		gwIfAddr := util.GetNodeGatewayIfAddr(hostSubnet)
		mgmtIfAddr := util.GetNodeManagementIfAddr(hostSubnet)
		logicalRouterPortNetwork = append(logicalRouterPortNetwork, gwIfAddr.String())
		hostNetworkPolicyIPs = append(hostNetworkPolicyIPs, mgmtIfAddr.IP)

		if utilnet.IsIPv6CIDR(hostSubnet) {
			v6Gateway = gwIfAddr.IP

			logicalSwitch.OtherConfig["ipv6_prefix"] =
				hostSubnet.IP.String()
		} else {
			v4Gateway = gwIfAddr.IP
			excludeIPs := mgmtIfAddr.IP.String()
			if !oc.nadInfo.IsSecondary && config.HybridOverlay.Enabled {
				hybridOverlayIfAddr := util.GetNodeHybridOverlayIfAddr(hostSubnet)
				excludeIPs += ".." + hybridOverlayIfAddr.IP.String()
			}
			logicalSwitch.OtherConfig["subnet"] = hostSubnet.String()
			logicalSwitch.OtherConfig["exclude_ips"] = excludeIPs
		}
	}

	if oc.loadBalancerGroupUUID != "" {
		logicalSwitch.LoadBalancerGroup = []string{oc.loadBalancerGroupUUID}
	}

	logicalRouterPortName := types.RouterToSwitchPrefix + switchName
	logicalRouterPort := nbdb.LogicalRouterPort{
		Name:     logicalRouterPortName,
		MAC:      nodeLRPMAC.String(),
		Networks: logicalRouterPortNetwork,
	}
	clusterRouterName := oc.nadInfo.Prefix + types.OVNClusterRouter
	logicalRouter := nbdb.LogicalRouter{Name: clusterRouterName}

	err := libovsdbops.CreateOrUpdateLogicalRouterPorts(oc.nbClient, &logicalRouter,
		[]*nbdb.LogicalRouterPort{&logicalRouterPort}, &logicalRouterPort.Networks, &logicalRouterPort.MAC)
	if err != nil {
		return fmt.Errorf("failed to add logical router port %+v to router %s: %v", logicalRouterPort, clusterRouterName, err)
	}

	// If supported, enable IGMP/MLD snooping and querier on the node.
	if oc.multicastSupport {
		logicalSwitch.OtherConfig["mcast_snoop"] = "true"

		// Configure IGMP/MLD querier if the gateway IP address is known.
		// Otherwise disable it.
		if v4Gateway != nil || v6Gateway != nil {
			logicalSwitch.OtherConfig["mcast_querier"] = "true"
			logicalSwitch.OtherConfig["mcast_eth_src"] = nodeLRPMAC.String()
			if v4Gateway != nil {
				logicalSwitch.OtherConfig["mcast_ip4_src"] = v4Gateway.String()
			}
			if v6Gateway != nil {
				logicalSwitch.OtherConfig["mcast_ip6_src"] = util.HWAddrToIPv6LLA(nodeLRPMAC).String()
			}
		} else {
			logicalSwitch.OtherConfig["mcast_querier"] = "false"
		}
	}

	err = libovsdbops.CreateOrUpdateLogicalSwitch(oc.nbClient, &logicalSwitch, &logicalSwitch.OtherConfig,
		&logicalSwitch.LoadBalancerGroup)
	if err != nil {
		return fmt.Errorf("failed to add logical switch %+v: %v", logicalSwitch, err)
	}

	if !oc.nadInfo.IsSecondary {
		// also add the join switch IPs for this switch - needed in shared gateway mode
		lrpIPs, err := oc.joinSwIPManager.EnsureJoinLRPIPs(switchName)
		if err != nil {
			return fmt.Errorf("failed to get join switch port IP address for switch %s: %v", switchName, err)
		}

		for _, lrpIP := range lrpIPs {
			hostNetworkPolicyIPs = append(hostNetworkPolicyIPs, lrpIP.IP)
		}

		// add the host network IPs for this node to host network namespace's address set
		if err = func() error {
			hostNetworkNamespace := config.Kubernetes.HostNetworkNamespace
			if hostNetworkNamespace != "" {
				nsInfo, nsUnlock, err := oc.ensureNamespaceLocked(hostNetworkNamespace, true, nil)
				if err != nil {
					return fmt.Errorf("failed to ensure namespace locked: %v", err)
				}
				defer nsUnlock()
				if err = nsInfo.addressSet.AddIPs(hostNetworkPolicyIPs); err != nil {
					return err
				}
			}
			return nil
		}(); err != nil {
			return err
		}
	}

	// Connect the switch to the router.
	logicalSwitchPort := nbdb.LogicalSwitchPort{
		Name:      types.SwitchToRouterPrefix + switchName,
		Type:      "router",
		Addresses: []string{"router"},
		Options:   map[string]string{"router-port": types.RouterToSwitchPrefix + switchName},
	}
	sw := nbdb.LogicalSwitch{Name: switchName}
	err = libovsdbops.CreateOrUpdateLogicalSwitchPortsOnSwitch(oc.nbClient, &sw, &logicalSwitchPort)
	if err != nil {
		klog.Errorf("Failed to add logical port %+v to switch %s: %v", logicalSwitchPort, switchName, err)
		return err
	}

	if !oc.nadInfo.IsSecondary {
		err = libovsdbops.AddPortsToPortGroup(oc.nbClient, types.ClusterRtrPortGroupName, logicalSwitchPort.UUID)
		if err != nil {
			klog.Errorf(err.Error())
			return err
		}
	}

	// Add the node to the logical switch cache
	return oc.lsManager.AddNode(switchName, logicalSwitch.UUID, hostSubnets)
}

func (oc *Controller) updateNodeAnnotationWithRetry(nodeName string, hostSubnets []*net.IPNet) error {
	var v4Addr, v6Addr *net.IPNet
	var gwLRPIPs []*net.IPNet
	var err error

	if !oc.nadInfo.IsSecondary {
		gwLRPIPs, err = oc.joinSwIPManager.EnsureJoinLRPIPs(nodeName)
		if err != nil {
			return fmt.Errorf("failed to allocate join switch port IP address for node %s: %v", nodeName, err)
		}
		for _, ip := range gwLRPIPs {
			if ip.IP.To4() != nil {
				v4Addr = ip
			} else if ip.IP.To16() != nil {
				v6Addr = ip
			}
		}
	}

	// Retry if it fails because of potential conflict which is transient. Return error in the
	// case of other errors (say temporary API server down), and it will be taken care of by the
	// retry mechanism.
	resultErr := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		// Informer cache should not be mutated, so get a copy of the object
		node, err := oc.watchFactory.GetNode(nodeName)
		if err != nil {
			return err
		}

		cnode := node.DeepCopy()
		if !oc.nadInfo.IsSecondary {
			cnode.Annotations, err = util.CreateNodeGateRouterLRPAddrAnnotation(cnode.Annotations, v4Addr, v6Addr)
			if err != nil {
				return fmt.Errorf("failed to marshal node %q annotation for Gateway LRP IP %v",
					node.Name, gwLRPIPs)
			}
		}
		cnode.Annotations, err = util.UpdateNodeHostSubnetAnnotation(cnode.Annotations, hostSubnets, oc.nadInfo.NetName)
		if err != nil {
			return fmt.Errorf("failed to update node %q annotation for network %s subnet %s",
				node.Name, oc.nadInfo.NetName, util.JoinIPNets(hostSubnets, ","))
		}
		return oc.kube.UpdateNode(cnode)
	})
	if resultErr != nil {
		return fmt.Errorf("failed to update node %s annotation for network %s", nodeName, oc.nadInfo.NetName)
	}
	return nil
}

func (oc *Controller) allocateNodeSubnets(node *kapi.Node) ([]*net.IPNet, []*net.IPNet, error) {
	hostSubnets, err := util.ParseNodeHostSubnetAnnotation(node, oc.nadInfo.NetName)
	if err != nil {
		// Log the error and try to allocate new subnets
		klog.Infof("Failed to get node %s host subnets annotations for network %s: %v", node.Name, oc.nadInfo.NetName, err)
	}
	allocatedSubnets := []*net.IPNet{}

	// OVN can work in single-stack or dual-stack only.
	currentHostSubnets := len(hostSubnets)
	expectedHostSubnets := 1
	// if dual-stack mode we expect one subnet per each IP family
	if config.IPv4Mode && config.IPv6Mode {
		expectedHostSubnets = 2
	}

	// node already has the expected subnets annotated
	// assume IP families match, i.e. no IPv6 config and node annotation IPv4
	if expectedHostSubnets == currentHostSubnets {
		klog.Infof("Allocated Subnets %v on Node %s for network %s", hostSubnets, node.Name, oc.nadInfo.NetName)
		return hostSubnets, allocatedSubnets, nil
	}

	// Node doesn't have the expected subnets annotated
	// it may happen it has more subnets assigned that configured in OVN
	// like in a dual-stack to single-stack conversion
	// or that it needs to allocate new subnet because it is a new node
	// or has been converted from single-stack to dual-stack
	klog.Infof("Expected %d subnets on node %s, found %d: %v",
		expectedHostSubnets, node.Name, currentHostSubnets, hostSubnets)
	// release unexpected subnets
	// filter in place slice
	// https://github.com/golang/go/wiki/SliceTricks#filter-in-place
	foundIPv4 := false
	foundIPv6 := false
	n := 0
	for _, subnet := range hostSubnets {
		// if the subnet is not going to be reused release it
		if config.IPv4Mode && utilnet.IsIPv4CIDR(subnet) && !foundIPv4 {
			klog.V(5).Infof("Valid IPv4 allocated subnet %v on node %s", subnet, node.Name)
			hostSubnets[n] = subnet
			n++
			foundIPv4 = true
			continue
		}
		if config.IPv6Mode && utilnet.IsIPv6CIDR(subnet) && !foundIPv6 {
			klog.V(5).Infof("Valid IPv6 allocated subnet %v on node %s", subnet, node.Name)
			hostSubnets[n] = subnet
			n++
			foundIPv6 = true
			continue
		}
		// this subnet is no longer needed
		klog.V(5).Infof("Releasing subnet %v on node %s", subnet, node.Name)
		err = oc.masterSubnetAllocator.ReleaseNetwork(subnet)
		if err != nil {
			klog.Warningf("Error releasing subnet %v on node %s", subnet, node.Name)
		}
	}
	// recreate hostSubnets with the valid subnets
	hostSubnets = hostSubnets[:n]
	// allocate new subnets if needed
	if config.IPv4Mode && !foundIPv4 {
		allocatedHostSubnet, err := oc.masterSubnetAllocator.AllocateIPv4Network()
		if err != nil {
			return nil, nil, fmt.Errorf("error allocating network for node %s: %v", node.Name, err)
		}
		// the allocator returns nil if it can't provide a subnet
		// we should filter them out or they will be appended to the slice
		if allocatedHostSubnet != nil {
			klog.V(5).Infof("Allocating subnet %v on node %s", allocatedHostSubnet, node.Name)
			allocatedSubnets = append(allocatedSubnets, allocatedHostSubnet)
			// Release the allocation on error
			defer func() {
				if err != nil {
					klog.Warningf("Releasing subnet %v on node %s: %v", allocatedHostSubnet, node.Name, err)
					errR := oc.masterSubnetAllocator.ReleaseNetwork(allocatedHostSubnet)
					if errR != nil {
						klog.Warningf("Error releasing subnet %v on node %s", allocatedHostSubnet, node.Name)
					}
				}
			}()
		}
	}
	if config.IPv6Mode && !foundIPv6 {
		allocatedHostSubnet, err := oc.masterSubnetAllocator.AllocateIPv6Network()
		if err != nil {
			return nil, nil, fmt.Errorf("error allocating network for node %s: %v", node.Name, err)
		}
		// the allocator returns nil if it can't provide a subnet
		// we should filter them out or they will be appended to the slice
		if allocatedHostSubnet != nil {
			klog.V(5).Infof("Allocating subnet %v on node %s", allocatedHostSubnet, node.Name)
			allocatedSubnets = append(allocatedSubnets, allocatedHostSubnet)
		}
	}
	// check if we were able to allocate the new subnets require
	// this can only happen if OVN is not configured correctly
	// so it will require a reconfiguration and restart.
	wantedSubnets := expectedHostSubnets - currentHostSubnets
	if wantedSubnets > 0 && len(allocatedSubnets) != wantedSubnets {
		return nil, nil, fmt.Errorf("error allocating networks for node %s: %d subnets expected only new %d subnets allocated", node.Name, expectedHostSubnets, len(allocatedSubnets))
	}
	hostSubnets = append(hostSubnets, allocatedSubnets...)
	klog.Infof("Allocated Subnets %v on Node %s", hostSubnets, node.Name)
	return hostSubnets, allocatedSubnets, nil
}

func (oc *Controller) addNode(node *kapi.Node) ([]*net.IPNet, error) {
	hostSubnets, allocatedSubnets, err := oc.allocateNodeSubnets(node)
	if err != nil {
		return nil, err
	}
	// Release the allocation on error
	defer func() {
		if err != nil {
			for _, allocatedSubnet := range allocatedSubnets {
				klog.Warningf("Releasing subnet %v on node %s: %v", allocatedSubnet, node.Name, err)
				errR := oc.masterSubnetAllocator.ReleaseNetwork(allocatedSubnet)
				if errR != nil {
					klog.Warningf("Error releasing subnet %v on node %s", allocatedSubnet, node.Name)
				}
			}
		}
	}()

	// Set the HostSubnet annotation on the node object to signal
	// to nodes that their logical infrastructure is set up and they can
	// proceed with their initialization
	err = oc.updateNodeAnnotationWithRetry(node.Name, hostSubnets)
	if err != nil {
		return nil, err
	}

	if !oc.nadInfo.IsSecondary {
		// delete stale chassis in SBDB if any
		if err = oc.deleteStaleNodeChassis(node); err != nil {
			return nil, err
		}
	}

	// Ensure that the node's logical network has been created. Note that if the
	// subsequent operation in addNode() fails, oc.lsManager.DeleteNode(node.Name)
	// needs to be done, otherwise, this node's IPAM will be overwritten and the
	// same IP could be allocated to multiple Pods scheduled on this node.
	err = oc.ensureNodeLogicalNetwork(node, hostSubnets)
	if err != nil {
		return nil, err
	}

	// If node annotation succeeds and subnets were allocated, update the used subnet count
	if len(allocatedSubnets) > 0 {
		for _, hostSubnet := range hostSubnets {
			util.UpdateUsedHostSubnetsCount(hostSubnet,
				&oc.v4HostSubnetsUsed,
				&oc.v6HostSubnetsUsed, true)
		}
		if !oc.nadInfo.IsSecondary {
			metrics.RecordSubnetUsage(oc.v4HostSubnetsUsed, oc.v6HostSubnetsUsed)
		}
	}
	return hostSubnets, nil
}

// check if any existing chassis entries in the SBDB mismatches with node's chassisID annotation
func (oc *Controller) checkNodeChassisMismatch(node *kapi.Node) (string, error) {
	if oc.nadInfo.IsSecondary {
		return "", nil
	}

	chassisID, err := util.ParseNodeChassisIDAnnotation(node)
	if err != nil {
		return "", nil
	}

	chassisList, err := libovsdbops.ListChassis(oc.sbClient)
	if err != nil {
		return "", fmt.Errorf("failed to get chassis list for node %s: error: %v", node.Name, err)
	}

	for _, chassis := range chassisList {
		if chassis.Hostname == node.Name && chassis.Name != chassisID {
			return chassis.Name, nil
		}
	}
	return "", nil
}

// delete stale chassis in SBDB if system-id of the specific node has changed.
func (oc *Controller) deleteStaleNodeChassis(node *kapi.Node) error {
	if oc.nadInfo.IsSecondary {
		return nil
	}

	staleChassis, err := oc.checkNodeChassisMismatch(node)
	if err != nil {
		return fmt.Errorf("failed to check if there is any stale chassis for node %s in SBDB: %v", node.Name, err)
	} else if staleChassis != "" {
		klog.V(5).Infof("Node %s now has a new chassis ID, delete its stale chassis %s in SBDB", node.Name, staleChassis)
		p := func(item *sbdb.Chassis) bool {
			return item.Name == staleChassis
		}
		if err = libovsdbops.DeleteChassisWithPredicate(oc.sbClient, p); err != nil {
			// Send an event and Log on failure
			oc.recorder.Eventf(node, kapi.EventTypeWarning, "ErrorMismatchChassis",
				"Node %s is now with a new chassis ID. Its stale chassis entry is still in the SBDB",
				node.Name)
			return fmt.Errorf("node %s is now with a new chassis ID. Its stale chassis entry is still in the SBDB", node.Name)
		}
	}
	return nil
}

func (oc *Controller) deleteNodeHostSubnet(nodeName string, subnet *net.IPNet) error {
	err := oc.masterSubnetAllocator.ReleaseNetwork(subnet)
	if err != nil {
		return fmt.Errorf("error deleting subnet %v for node %q: %s", subnet, nodeName, err)
	}
	klog.Infof("Deleted HostSubnet %v for node %s", subnet, nodeName)
	return nil
}

// deleteNodeLogicalNetwork removes the logical switch and logical router port associated with the node
func (oc *Controller) deleteNodeLogicalNetwork(nodeName string) error {
	// Remove switch to lb associations from the LBCache before removing the switch
	lbCache, err := ovnlb.GetLBCache(oc.nbClient)
	if err != nil {
		return fmt.Errorf("failed to get load_balancer cache for node %s: %v", nodeName, err)
	}

	// Remove the logical switch associated with the node
	clusterRouterName := oc.nadInfo.Prefix + types.OVNClusterRouter
	switchName := oc.nadInfo.Prefix + nodeName
	logicalRouterPortName := types.RouterToSwitchPrefix + switchName

	lbCache.RemoveSwitch(switchName)
	err = libovsdbops.DeleteLogicalSwitch(oc.nbClient, switchName)
	if err != nil {
		return fmt.Errorf("failed to delete logical switch %s: %v", switchName, err)
	}

	logiccalRouter := nbdb.LogicalRouter{Name: clusterRouterName}
	logicalRouterPort := nbdb.LogicalRouterPort{
		Name: logicalRouterPortName,
	}
	err = libovsdbops.DeleteLogicalRouterPorts(oc.nbClient, &logiccalRouter, &logicalRouterPort)
	if err != nil {
		return fmt.Errorf("failed to delete router port %s: %v", logicalRouterPort.Name, err)
	}

	return nil
}

func (oc *Controller) deleteNode(nodeName string, hostSubnets []*net.IPNet) error {
	for _, hostSubnet := range hostSubnets {
		if err := oc.deleteNodeHostSubnet(nodeName, hostSubnet); err != nil {
			return fmt.Errorf("error deleting node %s HostSubnet %v: %v", nodeName, hostSubnet, err)
		}
		util.UpdateUsedHostSubnetsCount(hostSubnet, &oc.v4HostSubnetsUsed, &oc.v6HostSubnetsUsed, false)
	}
	if !oc.nadInfo.IsSecondary {
		// update metrics
		metrics.RecordSubnetUsage(oc.v4HostSubnetsUsed, oc.v6HostSubnetsUsed)
	}

	if err := oc.deleteNodeLogicalNetwork(nodeName); err != nil {
		return fmt.Errorf("error deleting node %s logical network: %v", nodeName, err)
	}

	if !oc.nadInfo.IsSecondary {
		if err := oc.gatewayCleanup(nodeName); err != nil {
			return fmt.Errorf("failed to clean up node %s gateway: (%v)", nodeName, err)
		}

		if err := oc.joinSwIPManager.ReleaseJoinLRPIPs(nodeName); err != nil {
			return fmt.Errorf("failed to clean up GR LRP IPs for node %s: %v", nodeName, err)
		}

		p := func(item *sbdb.Chassis) bool {
			return item.Hostname == nodeName
		}
		if err := libovsdbops.DeleteChassisWithPredicate(oc.sbClient, p); err != nil {
			return fmt.Errorf("failed to remove the chassis associated with node %s in the OVN SB Chassis table: %v", nodeName, err)
		}
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
func (oc *Controller) clearInitialNodeNetworkUnavailableCondition(origNode *kapi.Node) {
	if oc.nadInfo.IsSecondary {
		return
	}
	// If it is not a Cloud Provider node, then nothing to do.
	if origNode.Spec.ProviderID == "" {
		return
	}

	cleared := false
	resultErr := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var err error

		oldNode, err := oc.watchFactory.GetNode(origNode.Name)
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
		klog.Errorf("Status update failed for local node %s: %v", origNode.Name, resultErr)
	} else if cleared {
		klog.Infof("Cleared node NetworkUnavailable/NoRouteCreated condition for %s", origNode.Name)
	}
}

// this is the worker function that does the periodic sync of nodes from kube API
// and sbdb and deletes chassis that are stale
func (oc *Controller) syncNodesPeriodic() {
	if oc.nadInfo.IsSecondary {
		return
	}

	//node names is a slice of all node names
	nodes, err := oc.kube.GetNodes()
	if err != nil {
		klog.Errorf("Error getting existing nodes from kube API: %v", err)
		return
	}

	nodeNames := make([]string, 0, len(nodes.Items))

	for _, node := range nodes.Items {
		nodeNames = append(nodeNames, node.Name)
	}

	chassisList, err := libovsdbops.ListChassis(oc.sbClient)
	if err != nil {
		klog.Errorf("Failed to get chassis list: error: %v", err)
		return
	}

	chassisHostNameMap := map[string]*sbdb.Chassis{}
	for _, chassis := range chassisList {
		chassisHostNameMap[chassis.Hostname] = chassis
	}

	//delete existing nodes from the chassis map.
	for _, nodeName := range nodeNames {
		delete(chassisHostNameMap, nodeName)
	}

	staleChassis := []*sbdb.Chassis{}
	for _, v := range chassisHostNameMap {
		staleChassis = append(staleChassis, v)
	}

	if err = libovsdbops.DeleteChassis(oc.sbClient, staleChassis...); err != nil {
		klog.Errorf("Failed Deleting chassis %v error: %v", chassisHostNameMap, err)
		return
	}
}

// We only deal with cleaning up nodes that shouldn't exist here, since
// watchNodes() will be called for all existing nodes at startup anyway.
// Note that this list will include the 'join' cluster switch, which we
// do not want to delete.
func (oc *Controller) syncNodesRetriable(nodes []interface{}) error {
	foundNodes := sets.NewString()
	for _, tmp := range nodes {
		node, ok := tmp.(*kapi.Node)
		if !ok {
			return fmt.Errorf("spurious object in syncNodes: %v", tmp)
		}
		hostSubnets, _ := util.ParseNodeHostSubnetAnnotation(node, oc.nadInfo.NetName)
		if !oc.nadInfo.IsSecondary && config.HybridOverlay.Enabled && len(hostSubnets) == 0 && houtil.IsHybridOverlayNode(node) {
			// this is a hybrid overlay node so mark as allocated from the hybrid overlay subnet allocator
			hostSubnet, err := houtil.ParseHybridOverlayHostSubnet(node)
			if err != nil {
				klog.Warning(err.Error())
			} else if hostSubnet != nil {
				klog.V(5).Infof("Node %s contains subnets: %v", node.Name, hostSubnet)
				if err := oc.hybridOverlaySubnetAllocator.MarkAllocatedNetwork(hostSubnet); err != nil {
					utilruntime.HandleError(err)
				}
			}
			// there is nothing left to be done if this is a hybrid overlay node
			continue
		}

		foundNodes.Insert(node.Name)
		klog.V(5).Infof("Node %s contains subnets: %v for network %s", node.Name, hostSubnets, oc.nadInfo.NetName)
		for _, hostSubnet := range hostSubnets {
			err := oc.masterSubnetAllocator.MarkAllocatedNetwork(hostSubnet)
			if err != nil {
				utilruntime.HandleError(err)
			}
			util.UpdateUsedHostSubnetsCount(hostSubnet, &oc.v4HostSubnetsUsed, &oc.v6HostSubnetsUsed, true)
		}

		if !oc.nadInfo.IsSecondary {
			// For each existing node, reserve its joinSwitch LRP IPs if they already exist.
			_, err := oc.joinSwIPManager.EnsureJoinLRPIPs(node.Name)
			if err != nil {
				// TODO (flaviof): keep going even if EnsureJoinLRPIPs returned an error. Maybe we should not.
				klog.Errorf("Failed to get join switch port IP address for node %s: %v", node.Name, err)
			}
		}
	}
	if !oc.nadInfo.IsSecondary {
		// update metrics for host subnets, default network only for now. TBD
		metrics.RecordSubnetUsage(oc.v4HostSubnetsUsed, oc.v6HostSubnetsUsed)
	}

	p := func(item *nbdb.LogicalSwitch) bool {
		if len(item.OtherConfig) > 0 {
			netName, ok := item.ExternalIDs["network_name"]
			if oc.nadInfo.IsSecondary {
				if ok && netName == oc.nadInfo.NetName {
					return true
				}
			} else if !ok {
				return true
			}
		}
		return false
	}
	nodeSwitches, err := libovsdbops.FindLogicalSwitchesWithPredicate(oc.nbClient, p)
	if err != nil {
		return fmt.Errorf("failed to get node logical switches which have other-config set for network %s: %v", oc.nadInfo.NetName, err)
	}
	if len(nodeSwitches) == 0 {
		klog.Warningf("Did not find any logical switches with other-config for network %s", oc.nadInfo.NetName)
	}

	for _, nodeSwitch := range nodeSwitches {
		nodeName := strings.TrimPrefix(nodeSwitch.Name, oc.nadInfo.Prefix)
		if foundNodes.Has(nodeName) {
			// node still exists, no cleanup to do
			continue
		}

		var subnets []*net.IPNet
		for key, value := range nodeSwitch.OtherConfig {
			var subnet *net.IPNet
			if key == "subnet" {
				_, subnet, err = net.ParseCIDR(value)
				if err != nil {
					klog.Warningf("Unable to parse subnet CIDR %v", value)
					continue
				}
			} else if key == "ipv6_prefix" {
				_, subnet, err = net.ParseCIDR(value + "/64")
				if err != nil {
					klog.Warningf("Unable to parse ipv6_prefix CIDR %v/64", value)
					continue
				}
			}
			if subnet != nil {
				subnets = append(subnets, subnet)
			}
		}
		if len(subnets) == 0 {
			continue
		}

		if err := oc.deleteNode(nodeName, subnets); err != nil {
			return fmt.Errorf("failed to delete node:%s for network %s, err:%v", nodeName, oc.nadInfo.NetName, err)
		}
	}

	if oc.nadInfo.IsSecondary {
		return nil
	}
	// cleanup stale chassis with no corresponding nodes
	chassisList, err := libovsdbops.ListChassis(oc.sbClient)
	if err != nil {
		return fmt.Errorf("failed to get chassis list: %v", err)
	}

	knownChassisNames := sets.NewString()
	chassisDeleteList := []*sbdb.Chassis{}
	for _, chassis := range chassisList {
		knownChassisNames.Insert(chassis.Name)
		// skip chassis that have a corresponding node
		if foundNodes.Has(chassis.Hostname) {
			continue
		}
		chassisDeleteList = append(chassisDeleteList, chassis)
	}

	// cleanup stale chassis private with no corresponding chassis
	chassisPrivateList, err := libovsdbops.ListChassisPrivate(oc.sbClient)
	if err != nil {
		return fmt.Errorf("failed to get chassis private list: %v", err)
	}

	for _, chassis := range chassisPrivateList {
		// skip chassis private that have a corresponding chassis
		if knownChassisNames.Has(chassis.Name) {
			continue
		}
		// we add to the list what would be the corresponding Chassis. Even if
		// the Chassis does not exist in SBDB, DeleteChassis will remove the
		// ChassisPrivate.
		chassisDeleteList = append(chassisDeleteList, &sbdb.Chassis{Name: chassis.Name})
	}

	// Delete stale chassis and associated chassis private
	if err := libovsdbops.DeleteChassis(oc.sbClient, chassisDeleteList...); err != nil {
		return fmt.Errorf("failed deleting chassis %v: %v", chassisDeleteList, err)
	}
	return nil
}

// nodeSyncs structure contains flags for the different failures
// so the retry logic can control what need to retry based
type nodeSyncs struct {
	syncNode              bool
	syncClusterRouterPort bool
	syncMgmtPort          bool
	syncGw                bool
}

func (oc *Controller) addUpdateNodeEvent(node *kapi.Node, nSyncs *nodeSyncs) error {
	var hostSubnets []*net.IPNet
	var errs []error
	var err error

	if noHostSubnet := noHostSubnet(node); noHostSubnet {
		err := oc.lsManager.AddNoHostSubnetNode(oc.nadInfo.Prefix + node.Name)
		if err != nil {
			return fmt.Errorf("nodeAdd: error adding noHost subnet for node %s: %w", node.Name, err)
		}
		if !oc.nadInfo.IsSecondary {
			if config.HybridOverlay.Enabled && houtil.IsHybridOverlayNode(node) {
				annotator := kube.NewNodeAnnotator(oc.kube, node.Name)
				allocatedSubnet, err := oc.hybridOverlayNodeEnsureSubnet(node, annotator)
				if err != nil {
					return fmt.Errorf("failed to update node %s hybrid overlay subnet annotation: %v", node.Name, err)
				}
				if err := annotator.Run(); err != nil {
					// Release allocated subnet if any errors occurred
					if allocatedSubnet != nil {
						_ = oc.releaseHybridOverlayNodeSubnet(node.Name, allocatedSubnet)
					}
					return fmt.Errorf(" failed to set hybrid overlay annotations for node %s: %v", node.Name, err)
				}
			}
			oc.clearInitialNodeNetworkUnavailableCondition(node)
		}
		return nil
	}

	klog.Infof("Adding or Updating Node %q for network %s", node.Name, oc.nadInfo.NetName)
	if nSyncs.syncNode {
		if hostSubnets, err = oc.addNode(node); err != nil {
			oc.addNodeFailed.Store(node.Name, true)
			oc.nodeClusterRouterPortFailed.Store(node.Name, true)
			if !oc.nadInfo.IsSecondary {
				oc.mgmtPortFailed.Store(node.Name, true)
				oc.gatewaysFailed.Store(node.Name, true)
			}
			return fmt.Errorf("nodeAdd: error creating subnet for node %s: %w", node.Name, err)
		}
		oc.addNodeFailed.Delete(node.Name)
	}

	if nSyncs.syncClusterRouterPort {
		if err = oc.syncNodeClusterRouterPort(node, nil); err != nil {
			errs = append(errs, err)
			oc.nodeClusterRouterPortFailed.Store(node.Name, true)
		} else {
			oc.nodeClusterRouterPortFailed.Delete(node.Name)
		}
	}

	if nSyncs.syncMgmtPort {
		err := oc.syncNodeManagementPort(node, hostSubnets)
		if err != nil {
			errs = append(errs, err)
			oc.mgmtPortFailed.Store(node.Name, true)
		} else {
			oc.mgmtPortFailed.Delete(node.Name)
		}
	}

	if !oc.nadInfo.IsSecondary {
		// delete stale chassis in SBDB if any
		if err := oc.deleteStaleNodeChassis(node); err != nil {
			errs = append(errs, err)
		}

		if config.HybridOverlay.Enabled {
			annotator := kube.NewNodeAnnotator(oc.kube, node.Name)
			if err := oc.handleHybridOverlayPort(node, annotator); err != nil {
				errs = append(errs, fmt.Errorf("failed to set up hybrid overlay logical switch port for %s: %v", node.Name, err))
			}
			if err := annotator.Run(); err != nil {
				errs = append(errs, fmt.Errorf("failed to set hybrid overlay annotations for node %s: %v", node.Name, err))
			}

		}

		oc.clearInitialNodeNetworkUnavailableCondition(node)
	}

	if nSyncs.syncGw {
		err := oc.syncNodeGateway(node, nil)
		if err != nil {
			errs = append(errs, err)
			oc.gatewaysFailed.Store(node.Name, true)
		} else {
			oc.gatewaysFailed.Delete(node.Name)
		}
	}

	// ensure pods that already exist on this node have their logical ports created
	// if per pod SNAT is being used, then l3 gateway config is required to be able to add pods
	if _, gwFailed := oc.gatewaysFailed.Load(node.Name); !gwFailed || (!oc.nadInfo.IsSecondary && !config.Gateway.DisableSNATMultipleGWs) {
		if nSyncs.syncNode || nSyncs.syncGw { // do this only if it is a new node add or a gateway sync happened
			options := metav1.ListOptions{
				FieldSelector:   fields.OneTermEqualSelector("spec.nodeName", node.Name).String(),
				ResourceVersion: "0",
			}
			pods, err := oc.client.CoreV1().Pods(metav1.NamespaceAll).List(context.TODO(), options)
			if err != nil {
				errs = append(errs, err)
				klog.Errorf("Unable to list existing pods on node: %s, existing pods on this node may not function",
					node.Name)
			} else {
				klog.V(5).Infof("When adding node %s, found %d pods to add to retryPods", node.Name, len(pods.Items))
				for _, pod := range pods.Items {
					pod := pod
					if util.PodCompleted(&pod) {
						continue
					}
					klog.V(5).Infof("Adding pod %s/%s to retryPods for network %s", pod.Namespace, pod.Name, oc.nadInfo.NetName)
					err = oc.retryPods.AddRetryObjWithAddNoBackoff(&pod)
					if err != nil {
						errs = append(errs, err)
						klog.Errorf("Failed to add pod %s/%s to retryPods for network %s: %v", pod.Namespace, pod.Name, oc.nadInfo.NetName, err)
					}
				}
			}
			oc.retryPods.RequestRetryObjs()
		}
	}

	return kerrors.NewAggregate(errs)
}

func (oc *Controller) deleteNodeEvent(node *kapi.Node) error {
	klog.V(5).Infof("Deleting Node %q. Removing the node from "+
		"various caches", node.Name)

	if !oc.nadInfo.IsSecondary && config.HybridOverlay.Enabled {
		if subnet, _ := houtil.ParseHybridOverlayHostSubnet(node); subnet != nil {
			if err := oc.releaseHybridOverlayNodeSubnet(node.Name, subnet); err != nil {
				return err
			}
		}
		if _, ok := node.Annotations[hotypes.HybridOverlayDRMAC]; ok && !houtil.IsHybridOverlayNode(node) {
			oc.deleteHybridOverlayPort(node)
		}
		if err := oc.removeHybridLRPolicySharedGW(node.Name); err != nil {
			return err
		}
	}
	nodeSubnets, _ := util.ParseNodeHostSubnetAnnotation(node, oc.nadInfo.NetName)
	if err := oc.deleteNode(node.Name, nodeSubnets); err != nil {
		return err
	}
	oc.lsManager.DeleteNode(oc.nadInfo.Prefix + node.Name)
	oc.addNodeFailed.Delete(node.Name)
	oc.mgmtPortFailed.Delete(node.Name)
	oc.gatewaysFailed.Delete(node.Name)
	oc.nodeClusterRouterPortFailed.Delete(node.Name)
	return nil
}

func (oc *Controller) createACLLoggingMeter() error {
	band := &nbdb.MeterBand{
		Action: types.MeterAction,
		Rate:   config.Logging.ACLLoggingRateLimit,
	}
	ops, err := libovsdbops.CreateMeterBandOps(oc.nbClient, nil, band)
	if err != nil {
		return fmt.Errorf("can't create meter band %v: %v", band, err)
	}

	meterFairness := true
	meter := &nbdb.Meter{
		Name: types.OvnACLLoggingMeter,
		Fair: &meterFairness,
		Unit: types.PacketsPerSecond,
	}
	ops, err = libovsdbops.CreateOrUpdateMeterOps(oc.nbClient, ops, meter, []*nbdb.MeterBand{band},
		&meter.Bands, &meter.Fair, &meter.Unit)
	if err != nil {
		return fmt.Errorf("can't create meter %v: %v", meter, err)
	}

	_, err = libovsdbops.TransactAndCheck(oc.nbClient, ops)
	if err != nil {
		return fmt.Errorf("can't transact ACL logging meter: %v", err)
	}

	return nil
}
