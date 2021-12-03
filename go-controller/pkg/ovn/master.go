package ovn

import (
	"context"
	"fmt"
	"net"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	kapi "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	utilwait "k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	hocontroller "github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/controller"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/informer"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/libovsdbops"
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
func (oc *Controller) Start(nodeName string, wg *sync.WaitGroup, ctx context.Context) error {
	// Set up leader election process first
	rl, err := resourcelock.New(
		resourcelock.ConfigMapsResourceLock,
		config.Kubernetes.OVNConfigNamespace,
		"ovn-kubernetes-master",
		oc.client.CoreV1(),
		nil,
		resourcelock.ResourceLockConfig{
			Identity:      nodeName,
			EventRecorder: oc.recorder,
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
				// run the cluster controller to init the master
				start := time.Now()
				defer func() {
					end := time.Since(start)
					metrics.MetricMasterReadyDuration.Set(end.Seconds())
				}()
				// run only on the active master node.
				metrics.StartMasterMetricUpdater(oc.stopChan, oc.nbClient)
				if err := oc.StartClusterMaster(nodeName); err != nil {
					panic(err.Error())
				}
				if err := oc.Run(wg, nodeName); err != nil {
					panic(err.Error())
				}
			},
			OnStoppedLeading: func() {
				//This node was leader and it lost the election.
				// Whenever the node transitions from leader to follower,
				// we need to handle the transition properly like clearing
				// the cache. It is better to exit for now.
				// kube will restart and this will become a follower.
				klog.Infof("No longer leader; exiting")
				os.Exit(0)
			},
			OnNewLeader: func(newLeaderName string) {
				if newLeaderName != nodeName {
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

	wg.Add(1)
	go func() {
		leaderElector.Run(ctx)
		klog.Infof("Stopped leader election")
		wg.Done()
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

	legacyJoinSwitches, err := libovsdbops.FindPerNodeJoinSwitches(oc.nbClient)
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
//  If true, then either quit or perform a complete reconfiguration of the cluster (recreate switches/routers with new subnet values)
func (oc *Controller) StartClusterMaster(masterNodeName string) error {
	klog.Infof("Starting cluster master")

	// enableOVNLogicalDataPathGroups sets an OVN flag to enable logical datapath
	// groups on OVN 20.12 and later. The option is ignored if OVN doesn't
	// understand it. Logical datapath groups reduce the size of the southbound
	// database in large clusters. ovn-controllers should be upgraded to a version
	// that supports them before the option is turned on by the master.
	dpGroupOpts := map[string]string{"use_logical_dp_groups": "true"}
	if err := libovsdbops.UpdateNBGlobalOptions(oc.nbClient, dpGroupOpts); err != nil {
		klog.Errorf("Failed to set NB global option to enable logical datapath groups: %v", err)
		return err
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

	klog.Infof("Allocating subnets")
	var v4HostSubnetCount, v6HostSubnetCount float64
	for _, clusterEntry := range config.Default.ClusterSubnets {
		err := oc.masterSubnetAllocator.AddNetworkRange(clusterEntry.CIDR, clusterEntry.HostSubnetLength)
		if err != nil {
			return err
		}
		klog.V(5).Infof("Added network range %s to the allocator", clusterEntry.CIDR)
		util.CalculateHostSubnetsForClusterEntry(clusterEntry, &v4HostSubnetCount, &v6HostSubnetCount)
	}
	nodeNames := []string{}
	for _, node := range existingNodes.Items {
		nodeNames = append(nodeNames, node.Name)
	}

	// update metrics for host subnets
	metrics.RecordSubnetCount(v4HostSubnetCount, v6HostSubnetCount)

	if oc.multicastSupport {
		if _, _, err := util.RunOVNSbctl("--columns=_uuid", "list", "IGMP_Group"); err != nil {
			klog.Warningf("Multicast support enabled, however version of OVN in use does not support IGMP Group. " +
				"Disabling Multicast Support")
			oc.multicastSupport = false
		}
	}

	if stdout, _, err := util.RunOVNNbctl("--data=bare", "--format=csv", "--no-headings", "--columns=_uuid,fair",
		"find", "meter", "name="+types.OvnACLLoggingMeter); err == nil {
		if stdout != "" {
			columns := strings.Split(stdout, ",")
			uuid := columns[0]
			fair := columns[1]
			if fair == "false" {
				// fair metering ensures that instead of sharing one meter across several entities
				// each entity will be rate-limited on its own
				if _, _, err := util.RunOVNNbctl("set", "meter", uuid, "fair=true"); err != nil {
					klog.Warningf("Failed to enable 'fair' metering for %s meter: %v", types.OvnACLLoggingMeter, err)
				}
			}
		} else {
			dropRate := strconv.Itoa(config.Logging.ACLLoggingRateLimit)
			if _, _, err := util.RunOVNNbctl("--fair", "meter-add", types.OvnACLLoggingMeter, "drop", dropRate, "pktps"); err != nil {
				klog.Warningf("ACL logging support enabled, however acl-logging meter could not be created: %v. "+
					"Disabling ACL logging support", err)
				oc.aclLoggingEnabled = false
			}
		}
	}

	if err := oc.SetupMaster(masterNodeName, nodeNames); err != nil {
		klog.Errorf("Failed to setup master (%v)", err)
		return err
	}

	if config.HybridOverlay.Enabled {
		oc.hoMaster, err = hocontroller.NewMaster(
			oc.kube,
			oc.watchFactory.NodeInformer(),
			oc.watchFactory.NamespaceInformer(),
			oc.watchFactory.PodInformer(),
			oc.nbClient,
			informer.NewDefaultEventHandler,
		)
		if err != nil {
			return fmt.Errorf("failed to set up hybrid overlay master: %v", err)
		}
	}

	return nil
}

// SetupMaster creates the central router and load-balancers for the network
func (oc *Controller) SetupMaster(masterNodeName string, existingNodeNames []string) error {
	// Create a single common distributed router for the cluster.
	logicalRouter := nbdb.LogicalRouter{
		Name: types.OVNClusterRouter,
		ExternalIDs: map[string]string{
			"k8s-cluster-router": "yes",
		},
		Options: map[string]string{
			"always_learn_from_arp_request": "false",
		},
	}
	if oc.multicastSupport {
		logicalRouter.Options = map[string]string{
			"mcast_relay": "true",
		}
	}
	opModels := []libovsdbops.OperationModel{
		{
			Model:          &logicalRouter,
			ModelPredicate: func(lr *nbdb.LogicalRouter) bool { return lr.Name == types.OVNClusterRouter },
		},
	}
	if _, err := oc.modelClient.CreateOrUpdate(opModels...); err != nil {
		return fmt.Errorf("failed to create a single common distributed router for the cluster, error: %v", err)
	}

	// Determine SCTP support
	hasSCTPSupport, err := util.DetectSCTPSupport()
	if err != nil {
		return err
	}
	oc.SCTPSupport = hasSCTPSupport
	if !oc.SCTPSupport {
		klog.Warningf("SCTP unsupported by this version of OVN. Kubernetes service creation with SCTP will not work ")
	} else {
		klog.Info("SCTP support detected in OVN")
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

	// Initialize the OVNJoinSwitch switch IP manager
	// The OVNJoinSwitch will be allocated IP addresses in the range 100.64.0.0/16 or fd98::/64.
	oc.joinSwIPManager, err = lsm.NewJoinLogicalSwitchIPManager(oc.nbClient, existingNodeNames)
	if err != nil {
		return err
	}

	// Allocate IPs for logical router port "GwRouterToJoinSwitchPrefix + OVNClusterRouter". This should always
	// allocate the first IPs in the join switch subnets
	gwLRPIfAddrs, err := oc.joinSwIPManager.EnsureJoinLRPIPs(types.OVNClusterRouter)
	if err != nil {
		return fmt.Errorf("failed to allocate join switch IP address connected to %s: %v", types.OVNClusterRouter, err)
	}

	// Create OVNJoinSwitch that will be used to connect gateway routers to the distributed router.
	logicalSwitch := nbdb.LogicalSwitch{
		Name: types.OVNJoinSwitch,
	}
	opModels = []libovsdbops.OperationModel{
		{
			Model:          &logicalSwitch,
			ModelPredicate: func(ls *nbdb.LogicalSwitch) bool { return ls.Name == types.OVNJoinSwitch },
		},
	}
	if _, err := oc.modelClient.CreateOrUpdate(opModels...); err != nil {
		return fmt.Errorf("failed to create logical switch %s, error: %v", types.OVNJoinSwitch, err)

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
	opModels = []libovsdbops.OperationModel{
		{
			Model: &logicalRouterPort,
			OnModelUpdates: []interface{}{
				&logicalRouterPort.MAC,
				&logicalRouterPort.Networks,
			},
			DoAfter: func() {
				logicalRouter.Ports = []string{logicalRouterPort.UUID}
			},
		},
		{
			Model:          &logicalRouter,
			ModelPredicate: func(lr *nbdb.LogicalRouter) bool { return lr.Name == types.OVNClusterRouter },
			OnModelMutations: []interface{}{
				&logicalRouter.Ports,
			},
			ErrNotFound: true,
		},
	}
	if _, err := oc.modelClient.CreateOrUpdate(opModels...); err != nil {
		return fmt.Errorf("failed to add logical router port %s, error: %v", drRouterPort, err)
	}

	// Connect the switch OVNJoinSwitch to the router.
	logicalSwitchPort := nbdb.LogicalSwitchPort{
		Name: drSwitchPort,
		Type: "router",
		Options: map[string]string{
			"router-port": drRouterPort,
		},
		Addresses: []string{"router"},
	}
	opModels = []libovsdbops.OperationModel{
		{
			Model: &logicalSwitchPort,
			DoAfter: func() {
				logicalSwitch.Ports = []string{logicalSwitchPort.UUID}
			},
		},
		{
			Model:          &logicalSwitch,
			ModelPredicate: func(ls *nbdb.LogicalSwitch) bool { return ls.Name == types.OVNJoinSwitch },
			OnModelMutations: []interface{}{
				&logicalSwitch.Ports,
			},
			ErrNotFound: true,
		},
	}
	if _, err := oc.modelClient.CreateOrUpdate(opModels...); err != nil {
		return fmt.Errorf("failed to add router-type logical switch port %s to %s, error: %v",
			drSwitchPort, types.OVNJoinSwitch, err)
	}
	return nil
}

func (oc *Controller) addNodeLogicalSwitchPort(logicalSwitchName, portName, portType string, addresses []string, options map[string]string) (string, error) {
	logicalSwitch := nbdb.LogicalSwitch{}
	logicalSwitchPort := nbdb.LogicalSwitchPort{
		Name:      portName,
		Type:      portType,
		Options:   options,
		Addresses: addresses,
	}
	opModels := []libovsdbops.OperationModel{
		{
			Model: &logicalSwitchPort,
			OnModelUpdates: []interface{}{
				&logicalSwitchPort.Addresses,
			},
			DoAfter: func() {
				logicalSwitch.Ports = []string{logicalSwitchPort.UUID}
			},
		},
		{
			Model:          &logicalSwitch,
			ModelPredicate: func(ls *nbdb.LogicalSwitch) bool { return ls.Name == logicalSwitchName },
			OnModelMutations: []interface{}{
				&logicalSwitch.Ports,
			},
			ErrNotFound: true,
		},
	}
	_, err := oc.modelClient.CreateOrUpdate(opModels...)
	if err != nil {
		return "", fmt.Errorf("failed to add logical port %s to switch %s, error: %v", portName, logicalSwitch, err)
	}

	return logicalSwitchPort.UUID, nil
}

func (oc *Controller) syncNodeManagementPort(node *kapi.Node, hostSubnets []*net.IPNet) error {
	macAddress, err := util.ParseNodeManagementPortMACAddress(node)
	if err != nil {
		return err
	}

	if hostSubnets == nil {
		hostSubnets, err = util.ParseNodeHostSubnetAnnotation(node)
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
			logicalRouter := nbdb.LogicalRouter{}
			logicalRouterStaticRoutes := nbdb.LogicalRouterStaticRoute{
				Policy:   &nbdb.LogicalRouterStaticRoutePolicySrcIP,
				IPPrefix: hostSubnet.String(),
				Nexthop:  mgmtIfAddr.IP.String(),
			}
			opModels := []libovsdbops.OperationModel{
				{
					Model: &logicalRouterStaticRoutes,
					ModelPredicate: func(lrsr *nbdb.LogicalRouterStaticRoute) bool {
						return lrsr.IPPrefix == hostSubnet.String() && lrsr.Nexthop == mgmtIfAddr.IP.String()
					},
					OnModelUpdates: []interface{}{
						&logicalRouterStaticRoutes.Nexthop,
						&logicalRouterStaticRoutes.IPPrefix,
					},
					DoAfter: func() {
						if logicalRouterStaticRoutes.UUID != "" {
							logicalRouter.StaticRoutes = []string{logicalRouterStaticRoutes.UUID}
						}
					},
				},
				{
					Model:          &logicalRouter,
					ModelPredicate: func(lr *nbdb.LogicalRouter) bool { return lr.Name == types.OVNClusterRouter },
					OnModelMutations: []interface{}{
						&logicalRouter.StaticRoutes,
					},
					ErrNotFound: true,
				},
			}
			if _, err := oc.modelClient.CreateOrUpdate(opModels...); err != nil {
				return fmt.Errorf("failed to add source IP address based "+
					"routes in distributed router %s, error: %v", types.OVNClusterRouter, err)
			}
		}
	}

	// Create this node's management logical port on the node switch
	portName := types.K8sPrefix + node.Name
	uuid, err := oc.addNodeLogicalSwitchPort(node.Name, portName, "", []string{addresses}, nil)
	if err != nil {
		return err
	}

	err = libovsdbops.AddPortsToPortGroup(oc.nbClient, types.ClusterPortGroupName, uuid)
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
	for _, clusterSubnet := range config.Default.ClusterSubnets {
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
		hostSubnets, err = util.ParseNodeHostSubnetAnnotation(node)
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

	lrpName := types.RouterToSwitchPrefix + node.Name
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
	logicalRouter := nbdb.LogicalRouter{}
	opModels := []libovsdbops.OperationModel{
		{
			Model: &logicalRouterPort,
			OnModelUpdates: []interface{}{
				&logicalRouterPort.MAC,
				&logicalRouterPort.Networks,
			},
			DoAfter: func() {
				if logicalRouterPort.UUID != "" {
					logicalRouter.Ports = []string{logicalRouterPort.UUID}
				}
			},
		},
		{
			Model:          &logicalRouter,
			ModelPredicate: func(lr *nbdb.LogicalRouter) bool { return lr.Name == types.OVNClusterRouter },
			OnModelMutations: []interface{}{
				&logicalRouter.Ports,
			},
			ErrNotFound: true,
		},
	}

	if _, err := oc.modelClient.CreateOrUpdate(opModels...); err != nil {
		klog.Errorf("Failed to add logical router port %s to router %s, error: %v", lrpName, types.OVNClusterRouter, err)
		return err
	}

	gatewayChassisName := lrpName + "-" + chassisID
	gatewayChassis := nbdb.GatewayChassis{
		Name:        gatewayChassisName,
		ChassisName: chassisID,
		Priority:    1,
	}

	opModels = []libovsdbops.OperationModel{
		{
			Model: &gatewayChassis,
			OnModelUpdates: []interface{}{
				&gatewayChassis.ChassisName,
				&gatewayChassis.Priority,
			},
			DoAfter: func() {
				if gatewayChassis.UUID != "" {
					logicalRouterPort.GatewayChassis = []string{gatewayChassis.UUID}
				}
			},
		},
		{
			Model: &logicalRouterPort,
			OnModelMutations: []interface{}{
				&logicalRouterPort.GatewayChassis,
			},
			ErrNotFound: true,
		},
	}

	if _, err := oc.modelClient.CreateOrUpdate(opModels...); err != nil {
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

	logicalSwitch := nbdb.LogicalSwitch{
		Name: nodeName,
	}

	var v4Gateway, v6Gateway net.IP
	var hostNetworkPolicyIPs []net.IP
	logicalRouterPortNetwork := []string{}
	for _, hostSubnet := range hostSubnets {
		gwIfAddr := util.GetNodeGatewayIfAddr(hostSubnet)
		mgmtIfAddr := util.GetNodeManagementIfAddr(hostSubnet)
		logicalRouterPortNetwork = append(logicalRouterPortNetwork, gwIfAddr.String())
		hostNetworkPolicyIPs = append(hostNetworkPolicyIPs, mgmtIfAddr.IP)

		if utilnet.IsIPv6CIDR(hostSubnet) {
			v6Gateway = gwIfAddr.IP

			logicalSwitch.OtherConfig = map[string]string{
				"ipv6_prefix": hostSubnet.IP.String(),
			}
		} else {
			v4Gateway = gwIfAddr.IP
			excludeIPs := mgmtIfAddr.IP.String()
			if config.HybridOverlay.Enabled {
				hybridOverlayIfAddr := util.GetNodeHybridOverlayIfAddr(hostSubnet)
				excludeIPs += ".." + hybridOverlayIfAddr.IP.String()
			}
			logicalSwitch.OtherConfig = map[string]string{
				"subnet":      hostSubnet.String(),
				"exclude_ips": excludeIPs,
			}
		}
	}

	logicalRouterPortName := types.RouterToSwitchPrefix + nodeName
	logicalRouterPort := nbdb.LogicalRouterPort{
		Name:     logicalRouterPortName,
		MAC:      nodeLRPMAC.String(),
		Networks: logicalRouterPortNetwork,
	}
	logicalRouter := nbdb.LogicalRouter{}
	opModels := []libovsdbops.OperationModel{
		{
			Model: &logicalRouterPort,
			OnModelUpdates: []interface{}{
				&logicalRouterPort.Networks,
				&logicalRouterPort.MAC,
			},
			DoAfter: func() {
				logicalRouter.Ports = []string{logicalRouterPort.UUID}
			},
		},
		{
			Model:          &logicalRouter,
			ModelPredicate: func(lr *nbdb.LogicalRouter) bool { return lr.Name == types.OVNClusterRouter },
			OnModelMutations: []interface{}{
				&logicalRouter.Ports,
			},
			ErrNotFound: true,
		},
		{
			Model:          &logicalSwitch,
			ModelPredicate: func(ls *nbdb.LogicalSwitch) bool { return ls.Name == nodeName },
			OnModelUpdates: []interface{}{
				&logicalSwitch.OtherConfig,
			},
		},
	}
	if _, err := oc.modelClient.CreateOrUpdate(opModels...); err != nil {
		return fmt.Errorf("failed to add logical port to router, error: %v", err)
	}

	// also add the join switch IPs for this node - needed in shared gateway mode
	lrpIPs, err := oc.joinSwIPManager.EnsureJoinLRPIPs(nodeName)
	if err != nil {
		return fmt.Errorf("failed to get join switch port IP address for node %s: %v", nodeName, err)
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

		// Create the Node's Logical Switch and set it's subnet
		opModels = []libovsdbops.OperationModel{
			{
				Model:          &logicalSwitch,
				ModelPredicate: func(lr *nbdb.LogicalSwitch) bool { return lr.Name == nodeName },
				OnModelMutations: []interface{}{
					&logicalSwitch.OtherConfig,
				},
				ErrNotFound: true,
			},
		}
		if _, err := oc.modelClient.CreateOrUpdate(opModels...); err != nil {
			return fmt.Errorf("failed to configure Multicast on logical switch for node %s, error: %v", nodeName, err)
		}
	}

	// Connect the switch to the router.
	nodeSwToRtrUUID, err := oc.addNodeLogicalSwitchPort(nodeName, types.SwitchToRouterPrefix+nodeName,
		"router", []string{"router"}, map[string]string{"router-port": types.RouterToSwitchPrefix + nodeName})
	if err != nil {
		klog.Errorf("Failed to add logical port to switch, error: %v", err)
		return err
	}

	err = libovsdbops.AddPortsToPortGroup(oc.nbClient, types.ClusterRtrPortGroupName, nodeSwToRtrUUID)
	if err != nil {
		klog.Errorf(err.Error())
		return err
	}

	// Add the node to the logical switch cache
	return oc.lsManager.AddNode(nodeName, hostSubnets)
}

func (oc *Controller) addNodeAnnotations(node *kapi.Node, hostSubnets []*net.IPNet) error {
	nodeAnnotations, err := util.CreateNodeHostSubnetAnnotation(hostSubnets)
	if err != nil {
		return fmt.Errorf("failed to marshal node %q annotation for subnet %s",
			node.Name, util.JoinIPNets(hostSubnets, ","))
	}
	// FIXME: the real solution is to reconcile the node object. Once we have a work-queue based
	// implementation where we can add the item back to the work queue when it fails to
	// reconcile, we can get rid of the PollImmediate.
	err = utilwait.PollImmediate(OvnNodeAnnotationRetryInterval, OvnNodeAnnotationRetryTimeout, func() (bool, error) {
		err = oc.kube.SetAnnotationsOnNode(node.Name, nodeAnnotations)
		if err != nil {
			klog.Warningf("Failed to set node annotation, will retry for: %v",
				OvnNodeAnnotationRetryTimeout)
		}
		return err == nil, nil
	},
	)
	if err != nil {
		return fmt.Errorf("failed to set node-subnets annotation on node %s: %v",
			node.Name, err)
	}
	return nil
}

func (oc *Controller) allocateNodeSubnets(node *kapi.Node) ([]*net.IPNet, []*net.IPNet, error) {
	hostSubnets, err := util.ParseNodeHostSubnetAnnotation(node)
	if err != nil {
		// Log the error and try to allocate new subnets
		klog.Infof("Failed to get node %s host subnets annotations: %v", node.Name, err)
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
		klog.Infof("Allocated Subnets %v on Node %s", hostSubnets, node.Name)
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
	oc.clearInitialNodeNetworkUnavailableCondition(node, nil)
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
	// Ensure that the node's logical network has been created
	err = oc.ensureNodeLogicalNetwork(node, hostSubnets)
	if err != nil {
		return nil, err
	}

	// Set the HostSubnet annotation on the node object to signal
	// to nodes that their logical infrastructure is set up and they can
	// proceed with their initialization
	err = oc.addNodeAnnotations(node, hostSubnets)
	if err != nil {
		return nil, err
	}

	// delete stale chassis in SBDB if any
	oc.deleteStaleNodeChassis(node)

	// If node annotation succeeds and subnets were allocated, update the used subnet count
	if len(allocatedSubnets) > 0 {
		for _, hostSubnet := range hostSubnets {
			util.UpdateUsedHostSubnetsCount(hostSubnet,
				&oc.v4HostSubnetsUsed,
				&oc.v6HostSubnetsUsed, true)
		}
		metrics.RecordSubnetUsage(oc.v4HostSubnetsUsed, oc.v6HostSubnetsUsed)
	}
	return hostSubnets, nil
}

// check if any existing chassis entries in the SBDB mismatches with node's chassisID annotation
func (oc *Controller) checkNodeChassisMismatch(node *kapi.Node) (bool, error) {
	chassisID, err := util.ParseNodeChassisIDAnnotation(node)
	if err != nil {
		return false, nil
	}

	chassisList, err := libovsdbops.ListChassis(oc.sbClient)
	if err != nil && err != libovsdbclient.ErrNotFound {
		return false, fmt.Errorf("failed to get chassis list for node %s: error: %v", node.Name, err)
	}

	if err == libovsdbclient.ErrNotFound {
		return false, nil
	}

	for _, chassis := range chassisList {
		if chassis.Name == chassisID {
			return false, nil
		}
	}
	return true, nil
}

// delete stale chassis in SBDB if system-id of the specific node has changed.
func (oc *Controller) deleteStaleNodeChassis(node *kapi.Node) {
	mismatch, err := oc.checkNodeChassisMismatch(node)
	if err != nil {
		klog.Errorf("Failed to check if there is any stale chassis for node %s in SBDB: %v", node.Name, err)
	} else if mismatch {
		klog.V(5).Infof("Node %s now has a new chassis ID, delete its stale chassis in SBDB", node.Name)
		if err = libovsdbops.DeleteNodeChassis(oc.sbClient, node.Name); err != nil {
			// Send an event and Log on failure
			oc.recorder.Eventf(node, kapi.EventTypeWarning, "ErrorMismatchChassis",
				"Node %s is now with a new chassis ID. Its stale chassis entry is still in the SBDB",
				node.Name)
			klog.Errorf("Node %s is now with a new chassis ID. Its stale chassis entry is still in the SBDB", node.Name)
		}
	}
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
	lbCache.RemoveSwitch(nodeName)
	// Remove the logical switch associated with the node
	logicalRouterPortName := types.RouterToSwitchPrefix + nodeName
	opModels := []libovsdbops.OperationModel{
		{
			Model:          &nbdb.LogicalSwitch{},
			ModelPredicate: func(ls *nbdb.LogicalSwitch) bool { return ls.Name == nodeName },
		},
		{
			Model:          &nbdb.LogicalRouterPort{},
			ModelPredicate: func(lrp *nbdb.LogicalRouterPort) bool { return lrp.Name == logicalRouterPortName },
		},
	}
	if err := oc.modelClient.Delete(opModels...); err != nil {
		return fmt.Errorf("failed to delete logical switch and logical router port associated with node: %s, error: %v", nodeName, err)
	}
	return nil
}

func (oc *Controller) deleteNode(nodeName string, hostSubnets []*net.IPNet) {
	// Clean up as much as we can but don't hard error
	for _, hostSubnet := range hostSubnets {
		if err := oc.deleteNodeHostSubnet(nodeName, hostSubnet); err != nil {
			klog.Errorf("Error deleting node %s HostSubnet %v: %v", nodeName, hostSubnet, err)
		} else {
			util.UpdateUsedHostSubnetsCount(hostSubnet, &oc.v4HostSubnetsUsed, &oc.v6HostSubnetsUsed, false)
		}
	}
	// update metrics
	metrics.RecordSubnetUsage(oc.v4HostSubnetsUsed, oc.v6HostSubnetsUsed)

	if err := oc.deleteNodeLogicalNetwork(nodeName); err != nil {
		klog.Errorf("Error deleting node %s logical network: %v", nodeName, err)
	}

	if err := oc.gatewayCleanup(nodeName); err != nil {
		klog.Errorf("Failed to clean up node %s gateway: (%v)", nodeName, err)
	}

	if err := oc.joinSwIPManager.ReleaseJoinLRPIPs(nodeName); err != nil {
		klog.Errorf("Failed to clean up GR LRP IPs for node %s: %v", nodeName, err)
	}

	if err := libovsdbops.DeleteNodeChassis(oc.sbClient, nodeName); err != nil {
		klog.Errorf("Failed to remove the chassis associated with node %s in the OVN SB Chassis table: %v", nodeName, err)
	}
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
		klog.Errorf("Status update failed for local node %s: %v", origNode.Name, resultErr)
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

	nodeNames := make([]string, 0, len(nodes.Items))

	for _, node := range nodes.Items {
		nodeNames = append(nodeNames, node.Name)
	}

	chassisList, err := libovsdbops.ListChassis(oc.sbClient)
	if err != nil {
		klog.Errorf("Failed to get chassis list: error: %v", err)
		return
	}

	chassisHostNameMap := map[string]string{}
	for _, chassis := range chassisList {
		chassisHostNameMap[chassis.Hostname] = chassis.Name
	}

	//delete existing nodes from the chassis map.
	for _, nodeName := range nodeNames {
		delete(chassisHostNameMap, nodeName)
	}

	staleChassisNames := []string{}
	for _, v := range chassisHostNameMap {
		staleChassisNames = append(staleChassisNames, v)
	}

	if err = libovsdbops.DeleteChassis(oc.sbClient, staleChassisNames...); err != nil {
		klog.Errorf("Failed Deleting chassis %v error: %v", chassisHostNameMap, err)
		return
	}
}

// We only deal with cleaning up nodes that shouldn't exist here, since
// watchNodes() will be called for all existing nodes at startup anyway.
// Note that this list will include the 'join' cluster switch, which we
// do not want to delete.
func (oc *Controller) syncNodes(nodes []interface{}) {
	foundNodes := sets.NewString()
	for _, tmp := range nodes {
		node, ok := tmp.(*kapi.Node)
		if !ok {
			klog.Errorf("Spurious object in syncNodes: %v", tmp)
			continue
		}
		foundNodes.Insert(node.Name)

		hostSubnets, _ := util.ParseNodeHostSubnetAnnotation(node)
		klog.V(5).Infof("Node %s contains subnets: %v", node.Name, hostSubnets)
		for _, hostSubnet := range hostSubnets {
			err := oc.masterSubnetAllocator.MarkAllocatedNetwork(hostSubnet)
			if err != nil {
				utilruntime.HandleError(err)
			}
			util.UpdateUsedHostSubnetsCount(hostSubnet, &oc.v4HostSubnetsUsed, &oc.v6HostSubnetsUsed, true)
		}

		// For each existing node, reserve its joinSwitch LRP IPs if they already exist.
		_, err := oc.joinSwIPManager.EnsureJoinLRPIPs(node.Name)
		if err != nil {
			klog.Errorf("Failed to get join switch port IP address for node %s: %v", node.Name, err)
		}
	}
	metrics.RecordSubnetUsage(oc.v4HostSubnetsUsed, oc.v6HostSubnetsUsed)

	chassisHostNames := sets.NewString()

	chassisList, err := libovsdbops.ListChassis(oc.sbClient)
	if err != nil {
		klog.Errorf("Failed to get chassis list: error: %v", err)
		return
	}

	for _, chassis := range chassisList {
		chassisHostNames.Insert(chassis.Hostname)
	}

	// Find difference between existing chassis and found nodes
	staleChassis := chassisHostNames.Difference(foundNodes)

	nodeSwitches, err := libovsdbops.FindSwitchesWithOtherConfig(oc.nbClient)
	if err != nil {
		klog.Errorf("Failed to get node logical switches which have other-config set error: %v", err)
		return
	}

	for _, nodeSwitch := range nodeSwitches {
		if foundNodes.Has(nodeSwitch.Name) {
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

		oc.deleteNode(nodeSwitch.Name, subnets)
		//remove the node from the chassis map so we don't delete it twice
		staleChassis.Delete(nodeSwitch.Name)
	}

	if err := libovsdbops.DeleteNodeChassis(oc.sbClient, staleChassis.List()...); err != nil {
		klog.Errorf("Failed Deleting chassis %v error: %v", staleChassis.List(), err)
		return
	}
}
