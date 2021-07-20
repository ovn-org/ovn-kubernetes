package services

import (
	"reflect"
	"sort"
	"sync"

	globalconfig "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
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
	resyncFn func()
}

type nodeInfo struct {
	// the node's Name
	name string
	// The list of physical IPs the node has
	nodeIPs []string
	// the name of the node's GatewayRouter, or "" of non-existent
	gatewayRouterName string
	// The name of the node's switch - never empty
	switchName string
}

func newNodeTracker(nodeInformer coreinformers.NodeInformer) *nodeTracker {
	nt := &nodeTracker{
		nodes: map[string]nodeInfo{},
	}

	nodeInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
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

			nt.updateNode(newObj)
		},
		DeleteFunc: func(obj interface{}) {
			node, ok := obj.(*v1.Node)
			if !ok {
				tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
				if !ok {
					klog.Errorf("couldn't understand non-tombstone object")
					return
				}
				node, ok = tombstone.Obj.(*v1.Node)
				if !ok {
					klog.Errorf("couldn't understand tombstone object")
					return
				}
			}
			nt.removeNode(node.Name)
		},
	})

	return nt

}

// updateNodeInfo updates the node info cache, and syncs all services
// if it changed.
func (nt *nodeTracker) updateNodeInfo(nodeName, switchName, routerName string, nodeIPs []string) {
	nt.Lock()

	ni := nodeInfo{
		name:              nodeName,
		nodeIPs:           nodeIPs,
		gatewayRouterName: routerName,
		switchName:        switchName,
	}
	if existing, ok := nt.nodes[nodeName]; ok {
		if reflect.DeepEqual(existing, ni) {
			nt.Unlock()
			return
		}
	}

	klog.Infof("Node %s switch + router changed, syncing services", nodeName)
	nt.nodes[nodeName] = ni
	nt.Unlock()

	// Resync all services
	nt.resyncFn()
}

// RemoveNode removes a node from the LB -> node mapper
// We don't need to re-sync here, because any stale LBs
// will eventually be cleaned up, and they don't have any cost.
func (nt *nodeTracker) removeNode(nodeName string) {
	nt.Lock()
	defer nt.Unlock()

	delete(nt.nodes, nodeName)
}

// UpdateNode is called when a node's gateway router / switch / IPs have changed
// The switch exists when the HostSubnet annotation is set.
// The gateway router will exist sometime after the L3Gateway annotation is set.
func (nt *nodeTracker) updateNode(node *v1.Node) {
	klog.V(2).Infof("Processing possible switch / router updates for node %s", node.Name)
	hsn, err := util.ParseNodeHostSubnetAnnotation(node)
	if err != nil || hsn == nil {
		klog.Infof("Node %s has invalid / no HostSubnet annotations: %v", node.Name, err)
		nt.removeNode(node.Name)
		return
	}

	switchName := node.Name
	grName := ""
	ips := []string{}

	// if the node has a gateway config, it will soon have a gateway router
	// so, set the router name
	gwConf, err := util.ParseNodeL3GatewayAnnotation(node)
	if err != nil || gwConf == nil {
		klog.Infof("Node %s has invalid / no gateway config: %v", node.Name, err)
	} else if gwConf.Mode != globalconfig.GatewayModeDisabled {
		grName = util.GetGatewayRouterFromNode(node.Name)
		if gwConf.NodePortEnable || gwConf.Mode == globalconfig.GatewayModeShared {
			for _, ip := range gwConf.IPAddresses {
				ips = append(ips, ip.IP.String())
			}
		}
	}

	nt.updateNodeInfo(
		node.Name,
		switchName,
		grName,
		ips,
	)
}

// allNodes returns a list of all nodes (and their relevant information)
func (nt *nodeTracker) allNodes() []nodeInfo {
	nt.Lock()
	defer nt.Unlock()

	out := make([]nodeInfo, 0, len(nt.nodes))
	for _, node := range nt.nodes {
		out = append(out, node)
	}

	// Sort so we don't churn on node add
	sort.SliceStable(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}
