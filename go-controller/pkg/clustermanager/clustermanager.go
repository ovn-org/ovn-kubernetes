package clustermanager

import (
	"context"
	"fmt"
	"math/big"
	"net"
	"os"
	"strconv"
	"sync"
	"time"

	kapi "k8s.io/api/core/v1"
	utilwait "k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	bitmapallocator "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/ipallocator/allocator"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/subnetallocator"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

const (
	OvnNodeAnnotationRetryInterval = 100 * time.Millisecond
	OvnNodeAnnotationRetryTimeout  = 1 * time.Second

	transitSwitchv4Cidr = "169.254.0.0/16"
	transitSwitchv6Cidr = "fd97::/64"

	// Maximum node Ids that can be generated. Limited to maximum nodes supported by k8s.
	maxNodeIds = 5000
)

type ClusterManager struct {
	client       clientset.Interface
	kube         kube.Interface
	watchFactory *factory.WatchFactory
	stopChan     <-chan struct{}

	// FIXME DUAL-STACK -  Make IP Allocators more dual-stack friendly
	clusterSubnetAllocator *subnetallocator.SubnetAllocator

	// event recorder used to post events to k8s
	recorder record.EventRecorder

	// v4HostSubnetsUsed keeps track of number of v4 subnets currently assigned to nodes
	v4HostSubnetsUsed float64

	// v6HostSubnetsUsed keeps track of number of v6 subnets currently assigned to nodes
	v6HostSubnetsUsed float64

	nodeIdBitmap    *bitmapallocator.AllocationBitmap
	nodeIdCache     map[string]int
	nodeIdCacheLock sync.Mutex

	transitSwitchv4Cidr   *net.IPNet
	transitSwitchBasev4Ip *big.Int

	transitSwitchv6Cidr   *net.IPNet
	transitSwitchBasev6Ip *big.Int
}

// NewOvnController creates a new OVN controller for creating logical network
// infrastructure and policy
func NewClusterManager(ovnClient *util.OVNClientset, wf *factory.WatchFactory, stopChan <-chan struct{},
	recorder record.EventRecorder) *ClusterManager {
	kube := &kube.Kube{
		KClient:              ovnClient.KubeClient,
		EIPClient:            ovnClient.EgressIPClient,
		EgressFirewallClient: ovnClient.EgressFirewallClient,
		CloudNetworkClient:   ovnClient.CloudNetworkClient,
	}

	nodeIdBitmap := bitmapallocator.NewContiguousAllocationMap(maxNodeIds, "nodeIds")
	_, _ = nodeIdBitmap.Allocate(0)

	_, tsv4Cidr, _ := net.ParseCIDR(transitSwitchv4Cidr)
	_, tsv6Cidr, _ := net.ParseCIDR(transitSwitchv6Cidr)
	return &ClusterManager{
		client:                 ovnClient.KubeClient,
		kube:                   kube,
		watchFactory:           wf,
		stopChan:               stopChan,
		clusterSubnetAllocator: subnetallocator.NewSubnetAllocator(),
		recorder:               recorder,
		nodeIdBitmap:           nodeIdBitmap,
		nodeIdCache:            make(map[string]int),
		transitSwitchBasev4Ip:  utilnet.BigForIP(tsv4Cidr.IP),
		transitSwitchv4Cidr:    tsv4Cidr,
		transitSwitchBasev6Ip:  utilnet.BigForIP(tsv6Cidr.IP),
		transitSwitchv6Cidr:    tsv4Cidr,
	}
}

type ovnkubeClusterManagerLeaderMetrics struct{}

func (ovnkubeClusterManagerLeaderMetrics) On(string) {
	metrics.MetricClusterManagerLeader.Set(1)
}

func (ovnkubeClusterManagerLeaderMetrics) Off(string) {
	metrics.MetricClusterManagerLeader.Set(0)
}

type ovnkubeClusterManagerLeaderMetricsProvider struct{}

func (_ ovnkubeClusterManagerLeaderMetricsProvider) NewLeaderMetric() leaderelection.SwitchMetric {
	return ovnkubeClusterManagerLeaderMetrics{}
}

// Start waits until this process is the leader before starting master functions
func (cm *ClusterManager) Start(nodeName string, wg *sync.WaitGroup, ctx context.Context) error {
	klog.Infof("Cluster manager Started.")
	// Set up leader election process first
	rl, err := resourcelock.New(
		resourcelock.ConfigMapsLeasesResourceLock,
		config.Kubernetes.OVNConfigNamespace,
		"ovn-kubernetes-cluster-manager",
		cm.client.CoreV1(),
		cm.client.CoordinationV1(),
		resourcelock.ResourceLockConfig{
			Identity:      nodeName,
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
				// run the cluster controller to init the cluster manager
				start := time.Now()
				defer func() {
					end := time.Since(start)
					metrics.MetriClusterManagerReadyDuration.Set(end.Seconds())
				}()

				if err := cm.StartClusterManager(); err != nil {
					panic(err.Error())
				}
				// run the cluster controller to init the master
				// run only on the active master node.
				if err := cm.Run(nodeName); err != nil {
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

	leaderelection.SetProvider(ovnkubeClusterManagerLeaderMetricsProvider{})
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

// StartClusterManager runs a subnet IPAM that watches arrival/departure
// of nodes in the cluster
// On an addition to the cluster (node create), a new subnet is created for it.
// ovnkube-master will create the node logical switch and other resources in the
// OVN Northbound database.
// Upon deletion of a node, the node subnet is released.
//
// TODO: Verify that the cluster was not already called with a different global subnet
//  If true, then either quit or perform a complete reconfiguration of the cluster (recreate switches/routers with new subnet values)
func (cm *ClusterManager) StartClusterManager() error {
	klog.Infof("Starting cluster manager")
	metrics.RegisterClusterManagerFunctional()

	existingNodes, err := cm.kube.GetNodes()
	if err != nil {
		klog.Errorf("Error in fetching nodes: %v", err)
		return err
	}
	klog.V(5).Infof("Existing number of nodes: %d", len(existingNodes.Items))

	klog.Infof("Allocating subnets")
	var v4HostSubnetCount, v6HostSubnetCount float64
	for _, clusterEntry := range config.Default.ClusterSubnets {
		err := cm.clusterSubnetAllocator.AddNetworkRange(clusterEntry.CIDR, clusterEntry.HostSubnetLength)
		if err != nil {
			return err
		}
		klog.V(5).Infof("Added network range %s to the allocator", clusterEntry.CIDR)
		util.CalculateHostSubnetsForClusterEntry(clusterEntry, &v4HostSubnetCount, &v6HostSubnetCount)
	}

	// update metrics for host subnets
	metrics.RecordSubnetCount(v4HostSubnetCount, v6HostSubnetCount)

	return nil
}

// Run starts the actual watching.
func (cm *ClusterManager) Run(nodeName string) error {
	// Start and sync the watch factory to begin listening for events
	if err := cm.watchFactory.Start(); err != nil {
		return err
	}

	if err := cm.WatchNodes(); err != nil {
		return err
	}

	return nil
}

// WatchNodes starts the watching of node resource and calls
// back the appropriate handler logic
func (cm *ClusterManager) WatchNodes() error {
	_, err := cm.watchFactory.AddNodeHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			node := obj.(*kapi.Node)
			klog.V(5).Infof("Added event for Node %q", node.Name)
			if err := cm.addUpdateNodeEvent(node); err != nil {
				klog.Errorf("Failed to handle Add node event for node %s, error: %v",
					node.Name, err)
			}
		},
		UpdateFunc: func(old, new interface{}) {
			node := new.(*kapi.Node)
			klog.V(5).Infof("Update event for Node %q", node.Name)
			if err := cm.addUpdateNodeEvent(node); err != nil {
				klog.Errorf("Failed to handle Update node event for node %s, error: %v",
					node.Name, err)
			}
		},
		DeleteFunc: func(obj interface{}) {
			node := obj.(*kapi.Node)
			klog.V(5).Infof("Deleted event for Node %q", node.Name)
			if err := cm.deleteNode(node); err != nil {
				klog.Errorf("Failed to handle Delete node event for node %s, error: %v",
					node.Name, err)
			}
		},
	}, cm.syncNodes)

	return err
}

func (cm *ClusterManager) syncNodes(nodes []interface{}) error {
	return nil
}

func (cm *ClusterManager) addUpdateNodeEvent(node *kapi.Node) error {
	if noHostSubnet := util.NoHostSubnet(node); noHostSubnet {
		return nil
	}

	return cm.addNode(node)
}

func (cm *ClusterManager) addNode(node *kapi.Node) error {
	var allocatedNodeId int = -1
	hostSubnets, allocatedSubnets, err := cm.allocateNodeSubnets(node)
	if err != nil {
		return err
	}
	// Release the allocation on error
	defer func() {
		if err != nil {
			for _, allocatedSubnet := range allocatedSubnets {
				klog.Warningf("Releasing subnet %v on node %s: %v", allocatedSubnet, node.Name, err)
				errR := cm.clusterSubnetAllocator.ReleaseNetwork(allocatedSubnet)
				if errR != nil {
					klog.Warningf("Error releasing subnet %v on node %s", allocatedSubnet, node.Name)
				}
			}
			cm.removeNodeId(node.Name, allocatedNodeId)
		}
	}()

	allocatedNodeId, err = cm.allocateNodeId(node)
	if err != nil {
		return err
	}

	// Set the HostSubnet annotation on the node object to signal
	// to nodes that their logical infrastructure is set up and they can
	// proceed with their initialization
	nodeAnnotations, err := util.CreateNodeHostSubnetAnnotation(hostSubnets)
	if err != nil {
		return fmt.Errorf("failed to marshal node %q annotation for subnet %s",
			node.Name, util.JoinIPNets(hostSubnets, ","))
	}

	// Add the node id annotation.
	nodeAnnotations[util.OvnNodeId] = strconv.Itoa(allocatedNodeId)

	// Generate v4 and v6 transit switch port IPs for the node.
	nodeTransitSwitchPortIps := cm.allocateNodeTransitSwitchPortIps(allocatedNodeId)
	transitSwitchPortAnnotations, err := util.CreateNodeTransitSwitchPortAddressesAnnotation(nodeTransitSwitchPortIps)
	if err != nil {
		return fmt.Errorf("failed to marshal node transit switch ips for node %s : error - %v",
			node.Name, err)
	}

	for k, v := range transitSwitchPortAnnotations {
		nodeAnnotations[k] = v
	}

	// FIXME: the real solution is to reconcile the node object. Once we have a work-queue based
	// implementation where we can add the item back to the work queue when it fails to
	// reconcile, we can get rid of the PollImmediate.
	err = utilwait.PollImmediate(OvnNodeAnnotationRetryInterval, OvnNodeAnnotationRetryTimeout, func() (bool, error) {
		err = cm.kube.SetAnnotationsOnNode(node.Name, nodeAnnotations)
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

	// If node annotation succeeds and subnets were allocated, update the used subnet count
	if len(allocatedSubnets) > 0 {
		for _, hostSubnet := range hostSubnets {
			util.UpdateUsedHostSubnetsCount(hostSubnet,
				&cm.v4HostSubnetsUsed,
				&cm.v6HostSubnetsUsed, true)
		}
		metrics.RecordSubnetUsage(cm.v4HostSubnetsUsed, cm.v6HostSubnetsUsed)
	}

	return err
}

func (cm *ClusterManager) deleteNode(node *kapi.Node) error {
	nodeSubnets, _ := util.ParseNodeHostSubnetAnnotation(node)
	for _, nodeSubnet := range nodeSubnets {
		err := cm.clusterSubnetAllocator.ReleaseNetwork(nodeSubnet)
		if err != nil {
			return fmt.Errorf("error deleting subnet %v for node %q: %s", nodeSubnet, node.Name, err)
		}
		klog.Infof("Deleted nodeSubnet %v for node %s", nodeSubnet, node.Name)

		util.UpdateUsedHostSubnetsCount(nodeSubnet, &cm.v4HostSubnetsUsed, &cm.v6HostSubnetsUsed, false)
	}
	// update metrics
	metrics.RecordSubnetUsage(cm.v4HostSubnetsUsed, cm.v6HostSubnetsUsed)

	nodeId := util.GetNodeId(node)
	cm.removeNodeId(node.Name, nodeId)
	return nil
}

func (cm *ClusterManager) allocateNodeSubnets(node *kapi.Node) ([]*net.IPNet, []*net.IPNet, error) {
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
		err = cm.clusterSubnetAllocator.ReleaseNetwork(subnet)
		if err != nil {
			klog.Warningf("Error releasing subnet %v on node %s", subnet, node.Name)
		}
	}
	// recreate hostSubnets with the valid subnets
	hostSubnets = hostSubnets[:n]
	// allocate new subnets if needed
	if config.IPv4Mode && !foundIPv4 {
		allocatedHostSubnet, err := cm.clusterSubnetAllocator.AllocateIPv4Network()
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
					errR := cm.clusterSubnetAllocator.ReleaseNetwork(allocatedHostSubnet)
					if errR != nil {
						klog.Warningf("Error releasing subnet %v on node %s", allocatedHostSubnet, node.Name)
					}
				}
			}()
		}
	}
	if config.IPv6Mode && !foundIPv6 {
		allocatedHostSubnet, err := cm.clusterSubnetAllocator.AllocateIPv6Network()
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

func (cm *ClusterManager) removeNodeId(nodeName string, nodeId int) {
	klog.Infof("Deleting node %q ID %d", nodeName, nodeId)
	if nodeId != -1 {
		cm.nodeIdBitmap.Release(nodeId)
	}
	delete(cm.nodeIdCache, nodeName)
}

func (cm *ClusterManager) allocateNodeId(node *kapi.Node) (int, error) {
	cm.nodeIdCacheLock.Lock()
	defer func() {
		cm.nodeIdCacheLock.Unlock()
		klog.Infof("Allocating node id done for node %q", node.Name)
	}()

	var nodeId int
	nodeId = util.GetNodeId(node)

	nodeIdInCache, ok := cm.nodeIdCache[node.Name]
	if ok {
		klog.Infof("Allocate node id : found node id %d for node %q in the cache", nodeIdInCache, node.Name)
	} else {
		nodeIdInCache = -1
	}

	if nodeIdInCache != -1 && nodeId != nodeIdInCache {
		cm.nodeIdCache[node.Name] = nodeId
		return nodeIdInCache, nil
	}

	if nodeIdInCache == -1 && nodeId != -1 {
		cm.nodeIdCache[node.Name] = nodeId
		return nodeId, nil
	}

	// We need to allocate the node id.
	if nodeIdInCache == -1 && nodeId == -1 {
		var allocated bool
		nodeId, allocated, _ = cm.nodeIdBitmap.AllocateNext()
		klog.Infof("Allocate node id : Id allocated for node %q is %d", node.Name, nodeId)
		if allocated {
			cm.nodeIdCache[node.Name] = nodeId
		} else {
			return -1, fmt.Errorf("failed to allocate id for the node %q", node.Name)
		}
	}

	return nodeId, nil
}

func (cm *ClusterManager) allocateNodeTransitSwitchPortIps(nodeId int) []*net.IPNet {
	var transitSwitchPortIps []*net.IPNet

	if config.IPv4Mode {
		nodeTransitSwitchPortv4Ip := utilnet.AddIPOffset(cm.transitSwitchBasev4Ip, nodeId)
		transitSwitchPortIps = append(transitSwitchPortIps, &net.IPNet{IP: nodeTransitSwitchPortv4Ip, Mask: cm.transitSwitchv4Cidr.Mask})
	}

	if config.IPv6Mode {
		nodeTransitSwitchPortv6Ip := utilnet.AddIPOffset(cm.transitSwitchBasev6Ip, nodeId)
		transitSwitchPortIps = append(transitSwitchPortIps, &net.IPNet{IP: nodeTransitSwitchPortv6Ip, Mask: cm.transitSwitchv6Cidr.Mask})
	}

	return transitSwitchPortIps
}
