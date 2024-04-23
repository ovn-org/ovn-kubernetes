package clustermanager

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1 "k8s.io/api/core/v1"
	cache "k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"

	ipamclaimsapi "github.com/k8snetworkplumbingwg/ipamclaims/pkg/crd/ipamclaims/v1alpha1"

	idallocator "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/allocator/id"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/allocator/ip/subnet"
	annotationalloc "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/allocator/pod"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/clustermanager/node"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/clustermanager/pod"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/persistentips"
	objretry "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/retry"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

// networkClusterController is the cluster controller for the networks. An
// instance of this struct is expected to be created for each network. A network
// is identified by its name and its unique id. It handles events at a cluster
// level to support the necessary configuration for the cluster networks.
type networkClusterController struct {
	watchFactory *factory.WatchFactory
	kube         kube.InterfaceOVN
	stopChan     chan struct{}
	wg           *sync.WaitGroup

	// node events factory handler
	nodeHandler *factory.Handler

	// retry framework for nodes
	retryNodes *objretry.RetryFramework

	// retry framework for L2 pod ip allocation
	podHandler *factory.Handler
	retryPods  *objretry.RetryFramework

	// retry framework for persistent ip allocation
	ipamClaimHandler *factory.Handler
	retryIPAMClaims  *objretry.RetryFramework

	podAllocator        *pod.PodAllocator
	nodeAllocator       *node.NodeAllocator
	networkIDAllocator  idallocator.NamedAllocator
	ipamClaimReconciler *persistentips.IPAMClaimReconciler
	subnetAllocator     subnet.Allocator

	util.NetInfo
}

func newNetworkClusterController(networkIDAllocator idallocator.NamedAllocator, netInfo util.NetInfo, ovnClient *util.OVNClusterManagerClientset, wf *factory.WatchFactory) *networkClusterController {
	kube := &kube.KubeOVN{
		Kube: kube.Kube{
			KClient: ovnClient.KubeClient,
		},
		IPAMClaimsClient: ovnClient.IPAMClaimsClient,
	}

	wg := &sync.WaitGroup{}

	ncc := &networkClusterController{
		NetInfo:            netInfo,
		watchFactory:       wf,
		kube:               kube,
		stopChan:           make(chan struct{}),
		wg:                 wg,
		networkIDAllocator: networkIDAllocator,
	}

	return ncc
}

func newDefaultNetworkClusterController(netInfo util.NetInfo, ovnClient *util.OVNClusterManagerClientset, wf *factory.WatchFactory) *networkClusterController {
	// use an allocator that can only allocate a single network ID for the
	// defaiult network
	networkIDAllocator, err := idallocator.NewIDAllocator(types.DefaultNetworkName, 1)
	if err != nil {
		panic(fmt.Errorf("could not build ID allocator for default network: %w", err))
	}
	// Reserve the id 0 for the default network.
	err = networkIDAllocator.ReserveID(types.DefaultNetworkName, defaultNetworkID)
	if err != nil {
		panic(fmt.Errorf("could not reserve default network ID: %w", err))
	}

	namedIDAllocator := networkIDAllocator.ForName(types.DefaultNetworkName)
	return newNetworkClusterController(namedIDAllocator, netInfo, ovnClient, wf)
}

func (ncc *networkClusterController) hasPodAllocation() bool {
	// we only do pod allocation on L2 topologies with interconnect
	switch ncc.TopologyType() {
	case types.Layer2Topology:
		// We need to allocate the PodAnnotation
		return config.OVNKubernetesFeature.EnableInterconnect
	case types.LocalnetTopology:
		// We need to allocate the PodAnnotation if there is IPAM
		return config.OVNKubernetesFeature.EnableInterconnect && len(ncc.Subnets()) > 0
	}
	return false
}

func (ncc *networkClusterController) hasNodeAllocation() bool {
	// we only do node allocation on L3 or default network, and L2 on
	// interconnect
	switch ncc.TopologyType() {
	case types.Layer3Topology:
		// we need to allocate network IDs and subnets
		return true
	case types.Layer2Topology:
		// we need to allocate network IDs
		return config.OVNKubernetesFeature.EnableInterconnect
	default:
		// we need to allocate network IDs and subnets
		return !ncc.IsSecondary()
	}
}

func (ncc *networkClusterController) allowPersistentIPs() bool {
	return config.OVNKubernetesFeature.EnablePersistentIPs &&
		ncc.NetInfo.AllowsPersistentIPs() &&
		util.DoesNetworkRequireIPAM(ncc.NetInfo) &&
		(ncc.NetInfo.TopologyType() == types.Layer2Topology || ncc.NetInfo.TopologyType() == types.LocalnetTopology)
}

func (ncc *networkClusterController) init() error {
	networkID, err := ncc.networkIDAllocator.AllocateID()
	if err != nil {
		return err
	}

	if ncc.hasNodeAllocation() {
		ncc.retryNodes = ncc.newRetryFramework(factory.NodeType, true)

		ncc.nodeAllocator = node.NewNodeAllocator(networkID, ncc.NetInfo, ncc.watchFactory.NodeCoreInformer().Lister(), ncc.kube)
		err := ncc.nodeAllocator.Init()
		if err != nil {
			return fmt.Errorf("failed to initialize host subnet ip allocator: %w", err)
		}
	}

	if ncc.hasPodAllocation() {
		ncc.retryPods = ncc.newRetryFramework(factory.PodType, true)
		ipAllocator, err := newIPAllocatorForNetwork(ncc.NetInfo)
		if err != nil {
			return fmt.Errorf("could not initialize the IP allocator for network %q: %w", ncc.NetInfo.GetNetworkName(), err)
		}
		ncc.subnetAllocator = ipAllocator

		var (
			podAllocationAnnotator *annotationalloc.PodAnnotationAllocator
			ipamClaimsReconciler   persistentips.PersistentAllocations
		)

		if ncc.allowPersistentIPs() {
			ncc.retryIPAMClaims = ncc.newRetryFramework(factory.IPAMClaimsType, true)
			ncc.ipamClaimReconciler = persistentips.NewIPAMClaimReconciler(
				ncc.kube,
				ncc.NetInfo,
				ncc.watchFactory.IPAMClaimsInformer().Lister(),
			)
			ipamClaimsReconciler = ncc.ipamClaimReconciler
		}

		podAllocationAnnotator = annotationalloc.NewPodAnnotationAllocator(
			ncc.NetInfo,
			ncc.watchFactory.PodCoreInformer().Lister(),
			ncc.kube,
			ipamClaimsReconciler,
		)

		ncc.podAllocator = pod.NewPodAllocator(ncc.NetInfo, podAllocationAnnotator, ipAllocator, ipamClaimsReconciler)
		if err := ncc.podAllocator.Init(); err != nil {
			return fmt.Errorf("failed to initialize pod ip allocator: %w", err)
		}
	}

	return nil
}

// Start the network cluster controller. Depending on the cluster configuration
// and type of network, it does the following:
//   - initializes the node allocator and starts listening to node events
//   - initializes the persistent ip allocator and starts listening to IPAMClaim events
//   - initializes the pod ip allocator and starts listening to pod events
func (ncc *networkClusterController) Start(ctx context.Context) error {
	err := ncc.init()
	if err != nil {
		return err
	}

	if ncc.hasNodeAllocation() {
		nodeHandler, err := ncc.retryNodes.WatchResource()
		if err != nil {
			return fmt.Errorf("unable to watch pods: %w", err)
		}
		ncc.nodeHandler = nodeHandler
	}

	if ncc.hasPodAllocation() {
		if ncc.allowPersistentIPs() {
			// we need to start listening to IPAMClaim events before pod events, to
			// ensure we don't start processing pod allocations before having the
			// existing IPAMClaim allocations reserved in the in-memory IP pool.
			ipamClaimHandler, err := ncc.retryIPAMClaims.WatchResource()
			if err != nil {
				return fmt.Errorf("unable to watch IPAMClaims: %w", err)
			}
			ncc.ipamClaimHandler = ipamClaimHandler
		}
		podHandler, err := ncc.retryPods.WatchResource()
		if err != nil {
			return fmt.Errorf("unable to watch pods: %w", err)
		}
		ncc.podHandler = podHandler
	}

	return nil
}

func (ncc *networkClusterController) Stop() {
	close(ncc.stopChan)
	ncc.wg.Wait()

	if ncc.ipamClaimHandler != nil {
		ncc.watchFactory.RemoveIPAMClaimsHandler(ncc.ipamClaimHandler)
	}

	if ncc.nodeHandler != nil {
		ncc.watchFactory.RemoveNodeHandler(ncc.nodeHandler)
	}

	if ncc.podHandler != nil {
		ncc.watchFactory.RemovePodHandler(ncc.podHandler)
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

// Cleanup the subnet annotations from the node for the secondary networks
func (ncc *networkClusterController) Cleanup(netName string) error {
	if !ncc.IsSecondary() {
		return fmt.Errorf("default network can't be cleaned up")
	}

	if ncc.hasNodeAllocation() {
		err := ncc.nodeAllocator.Cleanup(netName)
		if err != nil {
			return err
		}
		ncc.networkIDAllocator.ReleaseID()
	}

	return nil
}

// networkClusterControllerEventHandler object handles the events
// from retry framework.
type networkClusterControllerEventHandler struct {
	objretry.DefaultEventHandler

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
	case factory.PodType:
		pod, ok := obj.(*corev1.Pod)
		if !ok {
			return fmt.Errorf("could not cast %T object to *corev1.Pod", obj)
		}
		err := h.ncc.podAllocator.Reconcile(nil, pod)
		if err != nil {
			klog.Infof("Pod add failed for %s/%s, will try again later: %v",
				pod.Namespace, pod.Name, err)
		}
	case factory.NodeType:
		node, ok := obj.(*corev1.Node)
		if !ok {
			return fmt.Errorf("could not cast %T object to *corev1.Node", obj)
		}
		if err = h.ncc.nodeAllocator.HandleAddUpdateNodeEvent(node); err != nil {
			klog.Infof("Node add failed for %s, will try again later: %v",
				node.Name, err)
			return err
		}
		h.clearInitialNodeNetworkUnavailableCondition(node)
	case factory.IPAMClaimsType:
		return nil
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
	case factory.PodType:
		old, ok := oldObj.(*corev1.Pod)
		if !ok {
			return fmt.Errorf("could not cast %T old object to *corev1.Pod", oldObj)
		}
		new, ok := newObj.(*corev1.Pod)
		if !ok {
			return fmt.Errorf("could not cast %T new object to *corev1.Pod", newObj)
		}
		err := h.ncc.podAllocator.Reconcile(old, new)
		if err != nil {
			klog.Infof("Pod update failed for %s/%s, will try again later: %v",
				new.Namespace, new.Name, err)
		}
	case factory.NodeType:
		node, ok := newObj.(*corev1.Node)
		if !ok {
			return fmt.Errorf("could not cast %T object to *corev1.Node", newObj)
		}
		if err = h.ncc.nodeAllocator.HandleAddUpdateNodeEvent(node); err != nil {
			klog.Infof("Node update failed for %s, will try again later: %v",
				node.Name, err)
			return err
		}
		h.clearInitialNodeNetworkUnavailableCondition(node)
	case factory.IPAMClaimsType:
		return nil
	default:
		return fmt.Errorf("no update function for object type %s", h.objType)
	}
	return nil
}

// DeleteResource deletes the object from the cluster according to the delete logic of its resource type.
// cachedObj is the internal cache entry for this object, used for now for pods and network policies.
func (h *networkClusterControllerEventHandler) DeleteResource(obj, cachedObj interface{}) error {
	switch h.objType {
	case factory.PodType:
		pod, ok := obj.(*corev1.Pod)
		if !ok {
			return fmt.Errorf("could not cast %T object to *corev1.Pod", obj)
		}
		err := h.ncc.podAllocator.Reconcile(pod, nil)
		if err != nil {
			klog.Infof("Pod delete failed for %s/%s, will try again later: %v",
				pod.Namespace, pod.Name, err)
		}
	case factory.NodeType:
		node, ok := obj.(*corev1.Node)
		if !ok {
			return fmt.Errorf("could not cast obj of type %T to *knet.Node", obj)
		}
		return h.ncc.nodeAllocator.HandleDeleteNode(node)
	case factory.IPAMClaimsType:
		ipamClaim, ok := obj.(*ipamclaimsapi.IPAMClaim)
		if !ok {
			return fmt.Errorf("could not cast obj of type %T to *ipamclaimsapi.IPAMClaim", obj)
		}

		ipAllocator := h.ncc.subnetAllocator.ForSubnet(h.ncc.NetInfo.GetNetworkName())
		err := h.ncc.ipamClaimReconciler.Reconcile(ipamClaim, nil, ipAllocator)
		if err != nil && !errors.Is(err, persistentips.ErrIgnoredIPAMClaim) {
			return fmt.Errorf("error deleting IPAMClaim: %w", err)
		} else if errors.Is(err, persistentips.ErrIgnoredIPAMClaim) {
			return nil // let's avoid the log below, since nothing was released.
		}
		klog.Infof("Released IPs %q for network %q", ipamClaim.Status.IPs, ipamClaim.Spec.Network)
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
		case factory.PodType:
			syncFunc = h.ncc.podAllocator.Sync
		case factory.NodeType:
			syncFunc = h.ncc.nodeAllocator.Sync
		case factory.IPAMClaimsType:
			syncFunc = func(claims []interface{}) error {
				return h.ncc.ipamClaimReconciler.Sync(
					claims,
					h.ncc.subnetAllocator.ForSubnet(h.ncc.NetInfo.GetNetworkName()),
				)
			}

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

// GetResourceFromInformerCache returns the latest state of the object from the informers cache
// given an object key and its type
func (h *networkClusterControllerEventHandler) GetResourceFromInformerCache(key string) (interface{}, error) {
	var obj interface{}
	var namespace, name string
	var err error

	namespace, name, err = cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return nil, fmt.Errorf("failed to split key %s: %v", key, err)
	}

	switch h.objType {
	case factory.NodeType:
		obj, err = h.ncc.watchFactory.GetNode(name)
	case factory.PodType:
		obj, err = h.ncc.watchFactory.GetPod(namespace, name)
	case factory.IPAMClaimsType:
		obj, err = h.ncc.watchFactory.GetIPAMClaim(namespace, name)
	default:
		err = fmt.Errorf("object type %s not supported, cannot retrieve it from informers cache",
			h.objType)
	}
	return obj, err
}

// OVN uses an overlay and doesn't need GCE Routes, we need to
// clear the NetworkUnavailable condition that kubelet adds to initial node
// status when using GCE (done here: https://github.com/kubernetes/kubernetes/blob/master/pkg/controller/cloud/node_controller.go#L237).
// See discussion surrounding this here: https://github.com/kubernetes/kubernetes/pull/34398.
// TODO: make upstream kubelet more flexible with overlays and GCE so this
// condition doesn't get added for network plugins that don't want it, and then
// we can remove this function.
func (h *networkClusterControllerEventHandler) clearInitialNodeNetworkUnavailableCondition(origNode *corev1.Node) {
	// If it is not a Cloud Provider node, then nothing to do.
	if origNode.Spec.ProviderID == "" {
		return
	}

	cleared := false
	resultErr := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var err error

		oldNode, err := h.ncc.watchFactory.GetNode(origNode.Name)
		if err != nil {
			return err
		}
		// Informer cache should not be mutated, so get a copy of the object
		node := oldNode.DeepCopy()

		for i := range node.Status.Conditions {
			if node.Status.Conditions[i].Type == corev1.NodeNetworkUnavailable {
				condition := &node.Status.Conditions[i]
				if condition.Status != corev1.ConditionFalse && condition.Reason == "NoRouteCreated" {
					condition.Status = corev1.ConditionFalse
					condition.Reason = "RouteCreated"
					condition.Message = "ovn-kube cleared kubelet-set NoRouteCreated"
					condition.LastTransitionTime = metav1.Now()
					if err = h.ncc.kube.UpdateNodeStatus(node); err == nil {
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

// newIPAllocatorForNetwork returns an initialized subnet allocator for the
// subnets / excluded subnets provided in `netInfo`
func newIPAllocatorForNetwork(netInfo util.NetInfo) (subnet.Allocator, error) {
	ipAllocator := subnet.NewAllocator()

	subnets := netInfo.Subnets()
	ipNets := make([]*net.IPNet, 0, len(subnets))
	for _, subnet := range subnets {
		ipNets = append(ipNets, subnet.CIDR)
	}

	if err := ipAllocator.AddOrUpdateSubnet(
		netInfo.GetNetworkName(),
		ipNets,
		netInfo.ExcludeSubnets()...,
	); err != nil {
		return nil, err
	}

	return ipAllocator, nil
}
