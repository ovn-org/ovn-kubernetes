package services

import (
	"net"
	"reflect"
	"sort"
	"sync"

	globalconfig "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	v1 "k8s.io/api/core/v1"
	coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

// nodeTracker watches all Node objects and maintains a cache of information relevant
// to service creation. If a new node is created, it requests a resync of all services,
// since need to apply those service's load balancers to the new node as well.
type nodeTracker struct {
	sync.Mutex

	// nodes is the list of nodes we know about
	// map of name -> info
	nodes map[string]nodeInfo

	// resyncFn is the function to call so that all service are resynced
	resyncFn func(nodes []nodeInfo)

	// zone in which this nodeTracker is tracking
	zone string

	netInfo util.NetInfo
}

type nodeInfo struct {
	// the node's Name
	name string
	// The list of physical IPs reported by the gatewayconf annotation
	l3gatewayAddresses []net.IP
	// The list of physical IPs and subnet masks the node has, as reported by the host-cidrs annotation
	hostAddresses []net.IP
	// The pod network subnet(s)
	podSubnets []net.IPNet
	// the name of the node's GatewayRouter, or "" of non-existent
	gatewayRouterName string
	// The name of the node's switch - never empty
	switchName string
	// The chassisID of the node (ovs.external-ids:system-id)
	chassisID string
	// if nodePort is disabled on this node?
	nodePortDisabled bool

	// The node's zone
	zone string
	/** HACK BEGIN **/
	// has the node migrated to remote?
	migrated bool
	/** HACK END **/
}

func (ni *nodeInfo) hostAddressesStr() []string {
	out := make([]string, 0, len(ni.hostAddresses))
	for _, ip := range ni.hostAddresses {
		out = append(out, ip.String())
	}
	return out
}

func (ni *nodeInfo) l3gatewayAddressesStr() []string {
	out := make([]string, 0, len(ni.l3gatewayAddresses))
	for _, ip := range ni.l3gatewayAddresses {
		out = append(out, ip.String())
	}
	return out
}

func newNodeTracker(zone string, resyncFn func(nodes []nodeInfo), netInfo util.NetInfo) *nodeTracker {

	return &nodeTracker{
		nodes:    map[string]nodeInfo{},
		zone:     zone,
		resyncFn: resyncFn,
		netInfo:  netInfo,
	}
}

func (nt *nodeTracker) Start(nodeInformer coreinformers.NodeInformer) (cache.ResourceEventHandlerRegistration, error) {
	return nodeInformer.Informer().AddEventHandler(factory.WithUpdateHandlingForObjReplace(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			node, ok := obj.(*v1.Node)
			if !ok {
				return
			}
			nt.updateNode(node)
		},
		UpdateFunc: func(old, new interface{}) {
			oldObj, ok := old.(*v1.Node)
			if !ok {
				return
			}
			newObj, ok := new.(*v1.Node)
			if !ok {
				return
			}
			// Make sure object was actually changed and not pending deletion
			if oldObj.GetResourceVersion() == newObj.GetResourceVersion() || !newObj.GetDeletionTimestamp().IsZero() {
				return
			}

			// updateNode needs to be called in the following cases:
			// - hostSubnet annotation has changed
			// - L3Gateway annotation's ip addresses have changed
			// - the name of the node (very rare) has changed
			// - the `host-cidrs` annotation changed
			// - node changes its zone
			// - node becomes a hybrid overlay node from a ovn node or vice verse
			// . No need to trigger update for any other field change.
			if util.NodeSubnetAnnotationChanged(oldObj, newObj) ||
				util.NodeL3GatewayAnnotationChanged(oldObj, newObj) ||
				oldObj.Name != newObj.Name ||
				util.NodeHostCIDRsAnnotationChanged(oldObj, newObj) ||
				util.NodeZoneAnnotationChanged(oldObj, newObj) ||
				util.NodeMigratedZoneAnnotationChanged(oldObj, newObj) ||
				util.NoHostSubnet(oldObj) != util.NoHostSubnet(newObj) {
				nt.updateNode(newObj)
			}
		},
		DeleteFunc: func(obj interface{}) {
			node, ok := obj.(*v1.Node)
			if !ok {
				tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
				if !ok {
					klog.Errorf("Couldn't understand non-tombstone object")
					return
				}
				node, ok = tombstone.Obj.(*v1.Node)
				if !ok {
					klog.Errorf("Couldn't understand tombstone object")
					return
				}
			}
			nt.removeNodeWithServiceReSync(node.Name)
		},
	}))

}

// updateNodeInfo updates the node info cache, and syncs all services
// if it changed.
func (nt *nodeTracker) updateNodeInfo(nodeName, switchName, routerName, chassisID string, l3gatewayAddresses,
	hostAddresses []net.IP, podSubnets []*net.IPNet, zone string, nodePortDisabled, migrated bool) {
	ni := nodeInfo{
		name:               nodeName,
		l3gatewayAddresses: l3gatewayAddresses,
		hostAddresses:      hostAddresses,
		podSubnets:         make([]net.IPNet, 0, len(podSubnets)),
		gatewayRouterName:  routerName,
		switchName:         switchName,
		chassisID:          chassisID,
		nodePortDisabled:   nodePortDisabled,
		zone:               zone,
		migrated:           migrated,
	}
	for i := range podSubnets {
		ni.podSubnets = append(ni.podSubnets, *podSubnets[i]) // de-pointer
	}

	klog.Infof("Node %s switch + router changed, syncing services", nodeName)

	nt.Lock()
	defer nt.Unlock()
	if existing, ok := nt.nodes[nodeName]; ok {
		if reflect.DeepEqual(existing, ni) {
			return
		}
	}

	nt.nodes[nodeName] = ni

	// Resync all services
	nt.resyncFn(nt.getZoneNodes())
}

// removeNodeWithServiceReSync removes a node from the LB -> node mapper
// *and* forces full reconciliation of services.
func (nt *nodeTracker) removeNodeWithServiceReSync(nodeName string) {
	nt.removeNode(nodeName)
	nt.Lock()
	nt.resyncFn(nt.getZoneNodes())
	nt.Unlock()
}

// RemoveNode removes a node from the LB -> node mapper
// We don't need to re-sync here, because any stale LBs
// will eventually be cleaned up, and they don't have any cost.
func (nt *nodeTracker) removeNode(nodeName string) {
	nt.Lock()
	defer nt.Unlock()

	delete(nt.nodes, nodeName)
}

// updateNode is called when a node's gateway router / switch / IPs have changed
// The switch exists when the HostSubnet annotation is set.
// The gateway router will exist sometime after the L3Gateway annotation is set.
func (nt *nodeTracker) updateNode(node *v1.Node) {
	klog.V(2).Infof("Processing possible switch / router updates for node %s", node.Name)
	var hsn []*net.IPNet
	var err error
	if nt.netInfo.TopologyType() == types.Layer2Topology {
		for _, subnet := range nt.netInfo.Subnets() {
			hsn = append(hsn, subnet.CIDR)
		}
	} else {
		hsn, err = util.ParseNodeHostSubnetAnnotation(node, nt.netInfo.GetNetworkName())
	}
	if err != nil || hsn == nil || util.NoHostSubnet(node) {
		// usually normal; means the node's gateway hasn't been initialized yet
		klog.Infof("Node %s has invalid / no HostSubnet annotations (probably waiting on initialization), or it's a hybrid overlay node: %v", node.Name, err)
		nt.removeNode(node.Name)
		return
	}

	switchName := nt.netInfo.GetNetworkScopedSwitchName(node.Name)
	grName := ""
	l3gatewayAddresses := []net.IP{}
	chassisID := ""
	nodePortEnabled := false

	// if the node has a gateway config, it will soon have a gateway router
	// so, set the router name
	gwConf, err := util.ParseNodeL3GatewayAnnotation(node)
	if err != nil || gwConf == nil {
		klog.Infof("Node %s has invalid / no gateway config: %v", node.Name, err)
	} else if gwConf.Mode != globalconfig.GatewayModeDisabled {
		grName = nt.netInfo.GetNetworkScopedGWRouterName(node.Name)
		// L3 GW IP addresses are not network-specific, we can take them from the default L3 GW annotation
		for _, ip := range gwConf.IPAddresses {
			l3gatewayAddresses = append(l3gatewayAddresses, ip.IP)
		}
		nodePortEnabled = gwConf.NodePortEnable
		chassisID = gwConf.ChassisID
	}
	hostAddresses, err := util.GetNodeHostAddrs(node)
	if err != nil {
		klog.Warningf("Failed to get node host CIDRs for [%s]: %s", node.Name, err.Error())
	}

	hostAddressesIPs := make([]net.IP, 0, len(hostAddresses))
	for _, ipStr := range hostAddresses {
		ip := net.ParseIP(ipStr)
		hostAddressesIPs = append(hostAddressesIPs, ip)
	}

	nt.updateNodeInfo(
		node.Name,
		switchName,
		grName,
		chassisID,
		l3gatewayAddresses,
		hostAddressesIPs,
		hsn,
		util.GetNodeZone(node),
		!nodePortEnabled,
		util.HasNodeMigratedZone(node),
	)
}

// getZoneNodes returns a list of all nodes (and their relevant information)
// which belong to the nodeTracker 'zone'
// MUST be called with nt locked
func (nt *nodeTracker) getZoneNodes() []nodeInfo {
	out := make([]nodeInfo, 0, len(nt.nodes))
	for _, node := range nt.nodes {
		/** HACK BEGIN **/
		// TODO(tssurya): Remove this HACK a few months from now. This has been added only to
		// minimize disruption for upgrades when moving to interconnect=true.
		// We want the legacy ovnkube-master to wait for remote ovnkube-node to
		// signal it using "k8s.ovn.org/remote-zone-migrated" annotation before
		// considering a node as remote when we upgrade from "global" (1 zone IC)
		// zone to multi-zone. This is so that network disruption for the existing workloads
		// is negligible and until the point where ovnkube-node flips the switch to connect
		// to the new SBDB, it would continue talking to the legacy RAFT ovnkube-sbdb to ensure
		// OVN/OVS flows are intact. Legacy ovnkube-master must not delete the service load
		// balancers for this node till it has finished migration
		if nt.zone == types.OvnDefaultZone {
			if !node.migrated {
				out = append(out, node)
			}
			continue
		}
		/** HACK END **/
		if node.zone == nt.zone {
			out = append(out, node)
		}
	}

	// Sort the returned list of nodes
	// so that other operations that consume this data can just do a DeepEquals of things
	// (e.g. LB routers + switches) without having to do set arithmetic
	sort.SliceStable(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}
