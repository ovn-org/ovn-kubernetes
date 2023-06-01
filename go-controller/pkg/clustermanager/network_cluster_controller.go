package clustermanager

import (
	"context"
	"fmt"
	"net"
	"reflect"
	"sync"

	corev1 "k8s.io/api/core/v1"
	cache "k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"

	hotypes "github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/types"
	houtil "github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/clustermanager/subnetallocator"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	objretry "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/retry"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

// networkClusterController is the cluster controller for the networks.
// An instance of this struct is expected to be created for each network.
// A network is identified by its name and its unique id.
// It listens to the node events and does the following.
//   - allocates subnet from the cluster subnet pool. It also allocates subnets
//     from the hybrid overlay subnet pool if hybrid overlay is enabled.
//     It stores these allocated subnets in the node annotation
//   - stores the network id in each node's annotation.
type networkClusterController struct {
	kube         kube.Interface
	watchFactory *factory.WatchFactory
	stopChan     chan struct{}
	wg           *sync.WaitGroup

	// node events factory handler
	nodeHandler *factory.Handler

	// retry framework for nodes
	retryNodes *objretry.RetryFramework

	// name of the network
	networkName string
	// unique id of the network
	networkID int

	clusterSubnetAllocator *subnetallocator.HostSubnetAllocator
	clusterSubnets         []config.CIDRNetworkEntry

	enableHybridOverlaySubnetAllocator bool
	hybridOverlaySubnetAllocator       *subnetallocator.HostSubnetAllocator

	util.NetInfo
}

func newNetworkClusterController(networkName string, networkID int, clusterSubnets []config.CIDRNetworkEntry,
	ovnClient *util.OVNClusterManagerClientset, wf *factory.WatchFactory,
	enableHybridOverlaySubnetAllocator bool, netInfo util.NetInfo) *networkClusterController {

	kube := &kube.Kube{
		KClient: ovnClient.KubeClient,
	}

	wg := &sync.WaitGroup{}

	var hybridOverlaySubnetAllocator *subnetallocator.HostSubnetAllocator
	if enableHybridOverlaySubnetAllocator {
		hybridOverlaySubnetAllocator = subnetallocator.NewHostSubnetAllocator()
	}
	ncc := &networkClusterController{
		kube:                               kube,
		watchFactory:                       wf,
		stopChan:                           make(chan struct{}),
		wg:                                 wg,
		networkName:                        networkName,
		networkID:                          networkID,
		clusterSubnetAllocator:             subnetallocator.NewHostSubnetAllocator(),
		clusterSubnets:                     clusterSubnets,
		hybridOverlaySubnetAllocator:       hybridOverlaySubnetAllocator,
		enableHybridOverlaySubnetAllocator: enableHybridOverlaySubnetAllocator,
		NetInfo:                            netInfo,
	}

	ncc.initRetryFramework()
	return ncc
}

func (ncc *networkClusterController) initRetryFramework() {
	ncc.retryNodes = ncc.newRetryFramework(factory.NodeType, true)
}

// Start the network cluster controller
// It does the following
//   - initializes the network subnet allocator ranges
//     and hybrid network subnet allocator ranges if hybrid overlay is enabled.
//   - Starts watching the kubernetes nodes
func (ncc *networkClusterController) Start(ctx context.Context) error {
	if err := ncc.clusterSubnetAllocator.InitRanges(ncc.clusterSubnets); err != nil {
		return fmt.Errorf("failed to initialize cluster subnet allocator ranges: %w", err)
	}

	if ncc.enableHybridOverlaySubnetAllocator {
		if err := ncc.hybridOverlaySubnetAllocator.InitRanges(config.HybridOverlay.ClusterSubnets); err != nil {
			return fmt.Errorf("failed to initialize hybrid overlay subnet allocator ranges: %w", err)
		}
	}

	nodeHandler, err := ncc.retryNodes.WatchResource()

	if err != nil {
		return fmt.Errorf("unable to watch nodes: %w", err)
	}

	ncc.nodeHandler = nodeHandler
	return err
}

func (ncc *networkClusterController) Stop() {
	close(ncc.stopChan)
	ncc.wg.Wait()

	if ncc.nodeHandler != nil {
		ncc.watchFactory.RemoveNodeHandler(ncc.nodeHandler)
	}
}

func (ncc *networkClusterController) newRetryFramework(objectType reflect.Type, hasUpdateFunc bool) *objretry.RetryFramework {
	resourceHandler := &objretry.ResourceHandler{
		HasUpdateFunc:          hasUpdateFunc,
		NeedsUpdateDuringRetry: false,
		ObjType:                objectType,
		EventHandler: &networkClusterControllerEventHandler{
			objType:  objectType,
			ncc:      ncc,
			syncFunc: nil,
		},
	}
	return objretry.NewRetryFramework(ncc.stopChan, ncc.wg, ncc.watchFactory, resourceHandler)
}

// hybridOverlayNodeEnsureSubnet allocates a subnet and sets the
// hybrid overlay subnet annotation. It returns any newly allocated subnet
// or an error. If an error occurs, the newly allocated subnet will be released.
func (ncc *networkClusterController) hybridOverlayNodeEnsureSubnet(node *corev1.Node, annotator kube.Annotator) (*net.IPNet, error) {
	var existingSubnets []*net.IPNet
	// Do not allocate a subnet if the node already has one
	subnet, err := houtil.ParseHybridOverlayHostSubnet(node)
	if err != nil {
		// Log the error and try to allocate new subnets
		klog.Warningf("Failed to get node %s hybrid overlay subnet annotation: %v", node.Name, err)
	} else if subnet != nil {
		existingSubnets = []*net.IPNet{subnet}
	}

	// Allocate a new host subnet for this node
	// FIXME: hybrid overlay is only IPv4 for now due to limitations on the Windows side
	hostSubnets, allocatedSubnets, err := ncc.hybridOverlaySubnetAllocator.AllocateNodeSubnets(node.Name, existingSubnets, true, false)
	if err != nil {
		return nil, fmt.Errorf("error allocating hybrid overlay HostSubnet for node %s: %v", node.Name, err)
	}

	if err := annotator.Set(hotypes.HybridOverlayNodeSubnet, hostSubnets[0].String()); err != nil {
		if e := ncc.hybridOverlaySubnetAllocator.ReleaseNodeSubnets(node.Name, allocatedSubnets...); e != nil {
			klog.Warningf("Failed to release hybrid over subnet for the node %s from the allocator : %w", node.Name, e)
		}
		return nil, fmt.Errorf("error setting hybrid overlay host subnet: %w", err)
	}

	return hostSubnets[0], nil
}

func (ncc *networkClusterController) releaseHybridOverlayNodeSubnet(nodeName string) {
	ncc.hybridOverlaySubnetAllocator.ReleaseAllNodeSubnets(nodeName)
	klog.Infof("Deleted hybrid overlay HostSubnets for node %s", nodeName)
}

// handleAddUpdateNodeEvent handles the add or update node event
func (ncc *networkClusterController) handleAddUpdateNodeEvent(node *corev1.Node) error {
	if util.NoHostSubnet(node) {
		if ncc.enableHybridOverlaySubnetAllocator && houtil.IsHybridOverlayNode(node) {
			annotator := kube.NewNodeAnnotator(ncc.kube, node.Name)
			allocatedSubnet, err := ncc.hybridOverlayNodeEnsureSubnet(node, annotator)
			if err != nil {
				return fmt.Errorf("failed to update node %s hybrid overlay subnet annotation: %v", node.Name, err)
			}
			if err := annotator.Run(); err != nil {
				// Release allocated subnet if any errors occurred
				if allocatedSubnet != nil {
					ncc.releaseHybridOverlayNodeSubnet(node.Name)
				}
				return fmt.Errorf("failed to set hybrid overlay annotations for node %s: %v", node.Name, err)
			}
		}
		return nil
	}

	return ncc.syncNodeNetworkAnnotations(node)
}

// syncNodeNetworkAnnotations does 2 things
//   - syncs the node's allocated subnets in the node subnet annotation
//   - syncs the network id in the node network id annotation
func (ncc *networkClusterController) syncNodeNetworkAnnotations(node *corev1.Node) error {
	ncc.clusterSubnetAllocator.Lock()
	defer ncc.clusterSubnetAllocator.Unlock()

	existingSubnets, err := util.ParseNodeHostSubnetAnnotation(node, ncc.networkName)
	if err != nil && !util.IsAnnotationNotSetError(err) {
		// Log the error and try to allocate new subnets
		klog.Warningf("Failed to get node %s host subnets annotations for network %s : %v", node.Name, ncc.networkName, err)
	}

	networkID, err := util.ParseNetworkIDAnnotation(node, ncc.networkName)
	if err != nil && !util.IsAnnotationNotSetError(err) {
		// Log the error and try to allocate new subnets
		klog.Warningf("Failed to get node %s network id annotations for network %s : %v", node.Name, ncc.networkName, err)
	}

	// On return validExistingSubnets will contain any valid subnets that
	// were already assigned to the node. allocatedSubnets will contain
	// any newly allocated subnets required to ensure that the node has one subnet
	// from each enabled IP family.
	ipv4Mode, ipv6Mode := ncc.IPMode()
	validExistingSubnets, allocatedSubnets, err := ncc.clusterSubnetAllocator.AllocateNodeSubnets(node.Name, existingSubnets, ipv4Mode, ipv6Mode)
	if err != nil {
		return err
	}

	// If the existing subnets weren't OK, or new ones were allocated, update the node annotation.
	// This happens in a couple cases:
	// 1) new node: no existing subnets and one or more new subnets were allocated
	// 2) dual-stack to single-stack conversion: two existing subnets but only one will be valid, and no allocated subnets
	// 3) bad subnet annotation: one more existing subnets will be invalid and might have allocated a correct one
	// Also update the node annotation if the networkID doesn't match
	if len(existingSubnets) != len(validExistingSubnets) || len(allocatedSubnets) > 0 || ncc.networkID != networkID {
		updatedSubnetsMap := map[string][]*net.IPNet{ncc.networkName: validExistingSubnets}
		err = ncc.updateNodeNetworkAnnotationsWithRetry(node.Name, updatedSubnetsMap, ncc.networkID)
		if err != nil {
			if errR := ncc.clusterSubnetAllocator.ReleaseNodeSubnets(node.Name, allocatedSubnets...); errR != nil {
				klog.Warningf("Error releasing node %s subnets: %v", node.Name, errR)
			}
			return err
		}
	}

	return nil
}

// handleDeleteNode handles the delete node event
func (ncc *networkClusterController) handleDeleteNode(node *corev1.Node) error {
	if ncc.enableHybridOverlaySubnetAllocator {
		ncc.releaseHybridOverlayNodeSubnet(node.Name)
		return nil
	}

	ncc.clusterSubnetAllocator.Lock()
	defer ncc.clusterSubnetAllocator.Unlock()
	ncc.clusterSubnetAllocator.ReleaseAllNodeSubnets(node.Name)
	return nil
}

func (ncc *networkClusterController) syncNodes(nodes []interface{}) error {
	ncc.clusterSubnetAllocator.Lock()
	defer ncc.clusterSubnetAllocator.Unlock()

	for _, tmp := range nodes {
		node, ok := tmp.(*corev1.Node)
		if !ok {
			return fmt.Errorf("spurious object in syncNodes: %v", tmp)
		}

		if util.NoHostSubnet(node) {
			if ncc.enableHybridOverlaySubnetAllocator && houtil.IsHybridOverlayNode(node) {
				// this is a hybrid overlay node so mark as allocated from the hybrid overlay subnet allocator
				hostSubnet, err := houtil.ParseHybridOverlayHostSubnet(node)
				if err != nil {
					klog.Errorf("Failed to parse hybrid overlay for node %s: %w", node.Name, err)
				} else if hostSubnet != nil {
					klog.V(5).Infof("Node %s contains subnets: %v", node.Name, hostSubnet)
					if err := ncc.hybridOverlaySubnetAllocator.MarkSubnetsAllocated(node.Name, hostSubnet); err != nil {
						klog.Errorf("Failed to mark the subnet %v as allocated in the hybrid subnet allocator for node %s: %v", hostSubnet, node.Name, err)
					}
				}
			}
		} else {
			hostSubnets, _ := util.ParseNodeHostSubnetAnnotation(node, ncc.networkName)
			if len(hostSubnets) > 0 {
				klog.V(5).Infof("Node %s contains subnets: %v for network : %s", node.Name, hostSubnets, ncc.networkName)
				if err := ncc.clusterSubnetAllocator.MarkSubnetsAllocated(node.Name, hostSubnets...); err != nil {
					klog.Errorf("Failed to mark the subnet %v as allocated in the cluster subnet allocator for node %s: %v", hostSubnets, node.Name, err)
				}
			} else {
				klog.V(5).Infof("Node %s contains no subnets for network : %s", node.Name, ncc.networkName)
			}
		}
	}

	return nil
}

// updateNodeNetworkAnnotationsWithRetry will update the node's subnet annotation and network id annotation
func (ncc *networkClusterController) updateNodeNetworkAnnotationsWithRetry(nodeName string, hostSubnetsMap map[string][]*net.IPNet, networkId int) error {
	// Retry if it fails because of potential conflict which is transient. Return error in the
	// case of other errors (say temporary API server down), and it will be taken care of by the
	// retry mechanism.
	resultErr := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		// Informer cache should not be mutated, so get a copy of the object
		node, err := ncc.watchFactory.GetNode(nodeName)
		if err != nil {
			return err
		}

		cnode := node.DeepCopy()
		for netName, hostSubnets := range hostSubnetsMap {
			cnode.Annotations, err = util.UpdateNodeHostSubnetAnnotation(cnode.Annotations, hostSubnets, netName)
			if err != nil {
				return fmt.Errorf("failed to update node %q annotation subnet %s",
					node.Name, util.JoinIPNets(hostSubnets, ","))
			}
		}

		cnode.Annotations, err = util.UpdateNetworkIDAnnotation(cnode.Annotations, ncc.networkName, networkId)
		if err != nil {
			return fmt.Errorf("failed to update node %q network id annotation %d for network %s",
				node.Name, networkId, ncc.networkName)
		}
		return ncc.kube.UpdateNode(cnode)
	})
	if resultErr != nil {
		return fmt.Errorf("failed to update node %s annotation", nodeName)
	}
	return nil
}

// Cleanup the subnet annotations from the node for the secondary networks
func (ncc *networkClusterController) Cleanup(netName string) error {
	if !ncc.IsSecondary() {
		return fmt.Errorf("default network can't be cleaned up")
	}
	// remove hostsubnet annotation for this network
	klog.Infof("Remove node-subnets annotation for network %s on all nodes", ncc.networkName)
	existingNodes, err := ncc.watchFactory.GetNodes()
	if err != nil {
		return fmt.Errorf("error in retrieving the nodes: %v", err)
	}

	for _, node := range existingNodes {
		if util.NoHostSubnet(node) {
			// Secondary network subnet is not allocated for a nohost subnet node
			klog.V(5).Infof("Node %s is not managed by OVN", node.Name)
			continue
		}

		hostSubnetsMap := map[string][]*net.IPNet{ncc.networkName: nil}
		// passing util.InvalidNetworkID deletes the network id annotation for the network.
		err = ncc.updateNodeNetworkAnnotationsWithRetry(node.Name, hostSubnetsMap, util.InvalidNetworkID)
		if err != nil {
			return fmt.Errorf("failed to clear node %q subnet annotation for network %s",
				node.Name, ncc.networkName)
		}

		ncc.clusterSubnetAllocator.ReleaseAllNodeSubnets(node.Name)
	}

	return nil
}

// networkClusterControllerEventHandler object handles the events
// from retry framework.
type networkClusterControllerEventHandler struct {
	objretry.EmptyEventHandler

	objType  reflect.Type
	ncc      *networkClusterController
	syncFunc func([]interface{}) error
}

// networkClusterControllerEventHandler functions

// AddResource adds the specified object to the cluster according to its type and
// returns the error, if any, yielded during object creation.
func (h *networkClusterControllerEventHandler) AddResource(obj interface{}, fromRetryLoop bool) error {
	var err error

	switch h.objType {
	case factory.NodeType:
		node, ok := obj.(*corev1.Node)
		if !ok {
			return fmt.Errorf("could not cast %T object to *corev1.Node", obj)
		}
		if err = h.ncc.handleAddUpdateNodeEvent(node); err != nil {
			klog.Infof("Node add failed for %s, will try again later: %v",
				node.Name, err)
			return err
		}
	default:
		return fmt.Errorf("no add function for object type %s", h.objType)
	}
	return nil
}

// UpdateResource updates the specified object in the cluster to its version in newObj according
// to its type and returns the error, if any, yielded during the object update.
// The inRetryCache boolean argument is to indicate if the given resource is in the retryCache or not.
func (h *networkClusterControllerEventHandler) UpdateResource(oldObj, newObj interface{}, inRetryCache bool) error {
	var err error

	switch h.objType {
	case factory.NodeType:
		node, ok := newObj.(*corev1.Node)
		if !ok {
			return fmt.Errorf("could not cast %T object to *corev1.Node", newObj)
		}
		if err = h.ncc.handleAddUpdateNodeEvent(node); err != nil {
			klog.Infof("Node update failed for %s, will try again later: %v",
				node.Name, err)
			return err
		}
	default:
		return fmt.Errorf("no update function for object type %s", h.objType)
	}
	return nil
}

// DeleteResource deletes the object from the cluster according to the delete logic of its resource type.
// cachedObj is the internal cache entry for this object, used for now for pods and network policies.
func (h *networkClusterControllerEventHandler) DeleteResource(obj, cachedObj interface{}) error {
	switch h.objType {
	case factory.NodeType:
		node, ok := obj.(*corev1.Node)
		if !ok {
			return fmt.Errorf("could not cast obj of type %T to *knet.Node", obj)
		}
		return h.ncc.handleDeleteNode(node)
	}
	return nil
}

func (h *networkClusterControllerEventHandler) SyncFunc(objs []interface{}) error {
	var syncFunc func([]interface{}) error

	if h.syncFunc != nil {
		// syncFunc was provided explicitly
		syncFunc = h.syncFunc
	} else {
		switch h.objType {
		case factory.NodeType:
			syncFunc = h.ncc.syncNodes

		default:
			return fmt.Errorf("no sync function for object type %s", h.objType)
		}
	}
	if syncFunc == nil {
		return nil
	}
	return syncFunc(objs)
}

func (h *networkClusterControllerEventHandler) AreResourcesEqual(obj1, obj2 interface{}) (bool, error) {
	// switch based on type
	if h.objType == factory.NodeType {
		node1, ok := obj1.(*corev1.Node)
		if !ok {
			return false, fmt.Errorf("could not cast obj1 of type %T to *corev1.Node", obj1)
		}
		node2, ok := obj2.(*corev1.Node)
		if !ok {
			return false, fmt.Errorf("could not cast obj2 of type %T to *corev1.Node", obj2)
		}

		// network cluster controller only updates the node/hybrid subnet annotations.
		// Check if the annotations have changed.
		return reflect.DeepEqual(node1.Annotations, node2.Annotations), nil
	}

	return false, nil
}

// getResourceFromInformerCache returns the latest state of the object from the informers cache
// given an object key and its type
func (h *networkClusterControllerEventHandler) GetResourceFromInformerCache(key string) (interface{}, error) {
	var obj interface{}
	var name string
	var err error

	_, name, err = cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return nil, fmt.Errorf("failed to split key %s: %v", key, err)
	}

	switch h.objType {
	case factory.NodeType,
		factory.EgressNodeType:
		obj, err = h.ncc.watchFactory.GetNode(name)

	default:
		err = fmt.Errorf("object type %s not supported, cannot retrieve it from informers cache",
			h.objType)
	}
	return obj, err
}
