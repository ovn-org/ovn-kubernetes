package ovn

import (
	"fmt"
	"net"
	"reflect"

	kapi "k8s.io/api/core/v1"
	knet "k8s.io/api/networking/v1"
	cache "k8s.io/client-go/tools/cache"

	"k8s.io/klog/v2"

	ocpcloudnetworkapi "github.com/openshift/api/cloudnetwork/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	egressfirewall "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1"
	egressipv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/retry"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

type masterEventHandler struct {
	retry.EventHandler

	objType         reflect.Type
	oc              *Controller
	extraParameters interface{}
	syncFunc        func([]interface{}) error
}

// newRetryFrameworkMasterWithParameters builds and returns a retry framework for the input resource
// type and assigns all ovnk-master-specific function attributes in the returned struct;
// these functions will then be called by the retry logic in the retry package when
// WatchResource() is called.
// newRetryFrameworkMasterWithParameters takes as input a resource type (required)
// and the following optional parameters: a namespace and a label filter for the
// shared informer, a sync function to process all objects of this type at startup,
// and resource-specific extra parameters (used now for network-policy-dependant types).
// In order to create a retry framework for most resource types, newRetryFrameworkMaster is
// to be preferred, as it calls newRetryFrameworkMasterWithParameters with all optional parameters unset.
// newRetryFrameworkMasterWithParameters is instead called directly by the watchers that are
// dynamically created when a network policy is added: PeerNamespaceAndPodSelectorType,
// PeerPodForNamespaceAndPodSelectorType, PeerNamespaceSelectorType, PeerPodSelectorType.
func (oc *Controller) newRetryFrameworkMasterWithParameters(
	objectType reflect.Type,
	syncFunc func([]interface{}) error,
	extraParameters interface{}) *retry.RetryFramework {
	resourceHandler := &retry.ResourceHandler{
		HasUpdateFunc:          hasResourceAnUpdateFunc(objectType),
		NeedsUpdateDuringRetry: needsUpdateDuringRetry(objectType),
		ObjType:                objectType,
		EventHandler: &masterEventHandler{
			objType:         objectType,
			oc:              oc,
			extraParameters: extraParameters, // in use by network policy dynamic watchers
			syncFunc:        syncFunc,
		},
	}
	r := retry.NewRetryFramework(
		oc.watchFactory,
		resourceHandler,
	)
	return r
}

// newRetryFrameworkMaster takes as input a resource type and returns a retry framework
// as defined for that type. This constructor is used for resources (1) that do not need
// any namespace or label filtering in their shared informer, (2) whose sync function
// is assigned statically based on the resource type, (3) that do not need extra
// configuration parameters (extraParameters field).
// This is true for all resource types except for those that are dynamically created when
// adding a network policy.
func (oc *Controller) newRetryFrameworkMaster(objectType reflect.Type) *retry.RetryFramework {
	return oc.newRetryFrameworkMasterWithParameters(objectType, nil, nil)
}

// hasResourceAnUpdateFunc returns true if the given resource type has a dedicated update function.
// It returns false if, upon an update event on this resource type, we instead need to first delete the old
// object and then add the new one.
func hasResourceAnUpdateFunc(objType reflect.Type) bool {
	switch objType {
	case factory.PodType,
		factory.NodeType,
		factory.PeerPodSelectorType,
		factory.PeerPodForNamespaceAndPodSelectorType,
		factory.EgressIPType,
		factory.EgressIPNamespaceType,
		factory.EgressIPPodType,
		factory.EgressNodeType,
		factory.CloudPrivateIPConfigType,
		factory.LocalPodSelectorType,
		factory.NamespaceType:
		return true
	}
	return false
}

// AreResourcesEqual returns true if, given two objects of a known resource type, the update logic for this resource
// type considers them equal and therefore no update is needed. It returns false when the two objects are not considered
// equal and an update needs be executed. This is regardless of how the update is carried out (whether with a dedicated update
// function or with a delete on the old obj followed by an add on the new obj).
func (h *masterEventHandler) AreResourcesEqual(obj1, obj2 interface{}) (bool, error) {
	// switch based on type
	switch h.objType {
	case factory.PolicyType:
		np1, ok := obj1.(*knet.NetworkPolicy)
		if !ok {
			return false, fmt.Errorf("could not cast obj1 of type %T to *knet.NetworkPolicy", obj1)
		}
		np2, ok := obj2.(*knet.NetworkPolicy)
		if !ok {
			return false, fmt.Errorf("could not cast obj2 of type %T to *knet.NetworkPolicy", obj2)
		}
		return reflect.DeepEqual(np1, np2), nil

	case factory.NodeType:
		node1, ok := obj1.(*kapi.Node)
		if !ok {
			return false, fmt.Errorf("could not cast obj1 of type %T to *kapi.Node", obj1)
		}
		node2, ok := obj2.(*kapi.Node)
		if !ok {
			return false, fmt.Errorf("could not cast obj2 of type %T to *kapi.Node", obj2)
		}

		// when shouldUpdateNode is false, the hostsubnet is not assigned by ovn-kubernetes
		shouldUpdate, err := shouldUpdateNode(node2, node1)
		if err != nil {
			klog.Errorf(err.Error())
		}
		return !shouldUpdate, nil

	case factory.PodType,
		factory.EgressIPPodType,
		factory.PeerPodSelectorType,
		factory.PeerPodForNamespaceAndPodSelectorType,
		factory.LocalPodSelectorType:
		// For these types, there was no old vs new obj comparison in the original update code,
		// so pretend they're always different so that the update code gets executed
		return false, nil

	case factory.PeerNamespaceSelectorType,
		factory.PeerNamespaceAndPodSelectorType:
		// For these types there is no update code, so pretend old and new
		// objs are always equivalent and stop processing the update event.
		return true, nil

	case factory.EgressFirewallType:
		oldEgressFirewall, ok := obj1.(*egressfirewall.EgressFirewall)
		if !ok {
			return false, fmt.Errorf("could not cast obj1 of type %T to *egressfirewall.EgressFirewall", obj1)
		}
		newEgressFirewall, ok := obj2.(*egressfirewall.EgressFirewall)
		if !ok {
			return false, fmt.Errorf("could not cast obj2 of type %T to *egressfirewall.EgressFirewall", obj2)
		}
		return reflect.DeepEqual(oldEgressFirewall.Spec, newEgressFirewall.Spec), nil

	case factory.EgressIPType,
		factory.EgressIPNamespaceType,
		factory.EgressNodeType,
		factory.CloudPrivateIPConfigType:
		// force update path for EgressIP resource.
		return false, nil

	case factory.NamespaceType:
		// force update path for Namespace resource.
		return false, nil
	}

	return false, fmt.Errorf("no object comparison for type %s", h.objType)
}

func (oc *Controller) getPortInfo(pod *kapi.Pod) *lpInfo {
	var portInfo *lpInfo
	key := util.GetLogicalPortName(pod.Namespace, pod.Name)
	if !util.PodWantsNetwork(pod) {
		// create dummy logicalPortInfo for host-networked pods
		mac, _ := net.ParseMAC("00:00:00:00:00:00")
		portInfo = &lpInfo{
			logicalSwitch: "host-networked",
			name:          key,
			uuid:          "host-networked",
			ips:           []*net.IPNet{},
			mac:           mac,
		}
	} else {
		portInfo, _ = oc.logicalPortCache.get(key)
	}
	return portInfo
}

// Given an object and its type, GetInternalCacheEntry returns the internal cache entry for this object.
// This is now used only for pods, which will get their the logical port cache entry.
func (h *masterEventHandler) GetInternalCacheEntry(obj interface{}) interface{} {
	switch h.objType {
	case factory.PodType:
		pod := obj.(*kapi.Pod)
		return h.oc.getPortInfo(pod)
	default:
		return nil
	}
}

// Given an object key and its type, getResourceFromInformerCache returns the latest state of the object
// from the informers cache.
func (h *masterEventHandler) GetResourceFromInformerCache(key string) (interface{}, error) {
	var obj interface{}
	var namespace, name string
	var err error

	namespace, name, err = cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return nil, fmt.Errorf("failed to split key %s: %v", key, err)
	}

	switch h.objType {
	case factory.PolicyType:
		obj, err = h.oc.watchFactory.GetNetworkPolicy(namespace, name)

	case factory.NodeType,
		factory.EgressNodeType:
		obj, err = h.oc.watchFactory.GetNode(name)

	case factory.PodType,
		factory.PeerPodSelectorType,
		factory.PeerPodForNamespaceAndPodSelectorType,
		factory.LocalPodSelectorType,
		factory.EgressIPPodType:
		obj, err = h.oc.watchFactory.GetPod(namespace, name)

	case factory.PeerNamespaceAndPodSelectorType,
		factory.PeerNamespaceSelectorType,
		factory.EgressIPNamespaceType,
		factory.NamespaceType:
		obj, err = h.oc.watchFactory.GetNamespace(name)

	case factory.EgressFirewallType:
		obj, err = h.oc.watchFactory.GetEgressFirewall(namespace, name)

	case factory.EgressIPType:
		obj, err = h.oc.watchFactory.GetEgressIP(name)

	case factory.CloudPrivateIPConfigType:
		obj, err = h.oc.watchFactory.GetCloudPrivateIPConfig(name)

	default:
		err = fmt.Errorf("object type %s not supported, cannot retrieve it from informers cache",
			h.objType)
	}
	return obj, err
}

// Given an object and its type, RecordAddEvent records the add event on this object.
func (h *masterEventHandler) RecordAddEvent(obj interface{}) {
	switch h.objType {
	case factory.PodType:
		pod := obj.(*kapi.Pod)
		klog.V(5).Infof("Recording add event on pod %s/%s", pod.Namespace, pod.Name)
		h.oc.podRecorder.AddPod(pod.UID)
		metrics.GetConfigDurationRecorder().Start("pod", pod.Namespace, pod.Name)
	case factory.PolicyType:
		np := obj.(*knet.NetworkPolicy)
		klog.V(5).Infof("Recording add event on network policy %s/%s", np.Namespace, np.Name)
		metrics.GetConfigDurationRecorder().Start("networkpolicy", np.Namespace, np.Name)
	}
}

// Given an object and its type, RecordUpdateEvent records the update event on this object.
func (h *masterEventHandler) RecordUpdateEvent(obj interface{}) {
	switch h.objType {
	case factory.PodType:
		pod := obj.(*kapi.Pod)
		klog.V(5).Infof("Recording update event on pod %s/%s", pod.Namespace, pod.Name)
		metrics.GetConfigDurationRecorder().Start("pod", pod.Namespace, pod.Name)
	case factory.PolicyType:
		np := obj.(*knet.NetworkPolicy)
		klog.V(5).Infof("Recording update event on network policy %s/%s", np.Namespace, np.Name)
		metrics.GetConfigDurationRecorder().Start("networkpolicy", np.Namespace, np.Name)
	}
}

// Given an object and its type, RecordDeleteEvent records the delete event on this object. Only used for pods now.
func (h *masterEventHandler) RecordDeleteEvent(obj interface{}) {
	switch h.objType {
	case factory.PodType:
		pod := obj.(*kapi.Pod)
		klog.V(5).Infof("Recording delete event on pod %s/%s", pod.Namespace, pod.Name)
		h.oc.podRecorder.CleanPod(pod.UID)
		metrics.GetConfigDurationRecorder().Start("pod", pod.Namespace, pod.Name)
	case factory.PolicyType:
		np := obj.(*knet.NetworkPolicy)
		klog.V(5).Infof("Recording delete event on network policy %s/%s", np.Namespace, np.Name)
		metrics.GetConfigDurationRecorder().Start("networkpolicy", np.Namespace, np.Name)
	}
}

func (h *masterEventHandler) RecordSuccessEvent(obj interface{}) {
	switch h.objType {
	case factory.PodType:
		pod := obj.(*kapi.Pod)
		klog.V(5).Infof("Recording success event on pod %s/%s", pod.Namespace, pod.Name)
		metrics.GetConfigDurationRecorder().End("pod", pod.Namespace, pod.Name)
	case factory.PolicyType:
		np := obj.(*knet.NetworkPolicy)
		klog.V(5).Infof("Recording success event on network policy %s/%s", np.Namespace, np.Name)
		metrics.GetConfigDurationRecorder().End("networkpolicy", np.Namespace, np.Name)
	}
}

// Given an object and its type, RecordErrorEvent records an error event on this object.
// Only used for pods now.
func (h *masterEventHandler) RecordErrorEvent(obj interface{}, err error) {
	switch h.objType {
	case factory.PodType:
		pod := obj.(*kapi.Pod)
		klog.V(5).Infof("Recording error event on pod %s/%s", pod.Namespace, pod.Name)
		h.oc.recordPodEvent(err, pod)
	}
}

// Given an object and its type, isResourceScheduled returns true if the object has been scheduled.
// Only applied to pods for now. Returns true for all other types.
func (h *masterEventHandler) IsResourceScheduled(obj interface{}) bool {
	switch h.objType {
	case factory.PodType:
		pod := obj.(*kapi.Pod)
		return util.PodScheduled(pod)
	}
	return true
}

// Given an object type, resourceNeedsUpdate returns true if the object needs to invoke update during iterate retry.
func needsUpdateDuringRetry(objType reflect.Type) bool {
	switch objType {
	case factory.EgressNodeType,
		factory.EgressIPType,
		factory.EgressIPPodType,
		factory.EgressIPNamespaceType,
		factory.CloudPrivateIPConfigType:
		return true
	}
	return false
}

// Given a *RetryFramework instance, an object to add and a boolean specifying if the function was executed from
// iterateRetryResources, AddResource adds the specified object to the cluster according to its type and
// returns the error, if any, yielded during object creation.
func (h *masterEventHandler) AddResource(obj interface{}, fromRetryLoop bool) error {
	var err error

	switch h.objType {
	case factory.PodType:
		pod, ok := obj.(*kapi.Pod)
		if !ok {
			return fmt.Errorf("could not cast %T object to *knet.Pod", obj)
		}
		return h.oc.ensurePod(nil, pod, true)

	case factory.PolicyType:
		np, ok := obj.(*knet.NetworkPolicy)
		if !ok {
			return fmt.Errorf("could not cast %T object to *knet.NetworkPolicy", obj)
		}

		if err = h.oc.addNetworkPolicy(np); err != nil {
			klog.Infof("Network Policy add failed for %s/%s, will try again later: %v",
				np.Namespace, np.Name, err)
			return err
		}

	case factory.NodeType:
		node, ok := obj.(*kapi.Node)
		if !ok {
			return fmt.Errorf("could not cast %T object to *kapi.Node", obj)
		}
		var nodeParams *nodeSyncs
		if fromRetryLoop {
			_, nodeSync := h.oc.addNodeFailed.Load(node.Name)
			_, clusterRtrSync := h.oc.nodeClusterRouterPortFailed.Load(node.Name)
			_, mgmtSync := h.oc.mgmtPortFailed.Load(node.Name)
			_, gwSync := h.oc.gatewaysFailed.Load(node.Name)
			_, hoSync := h.oc.hybridOverlayFailed.Load(node.Name)
			nodeParams = &nodeSyncs{
				nodeSync,
				clusterRtrSync,
				mgmtSync,
				gwSync,
				hoSync}
		} else {
			nodeParams = &nodeSyncs{true, true, true, true, config.HybridOverlay.Enabled}
		}

		if err = h.oc.addUpdateNodeEvent(node, nodeParams); err != nil {
			klog.Infof("Node add failed for %s, will try again later: %v",
				node.Name, err)
			return err
		}

	case factory.PeerPodSelectorType:
		extraParameters := h.extraParameters.(*NetworkPolicyExtraParameters)
		return h.oc.handlePeerPodSelectorAddUpdate(extraParameters.np, extraParameters.gp, obj)

	case factory.PeerNamespaceAndPodSelectorType:
		extraParameters := h.extraParameters.(*NetworkPolicyExtraParameters)
		return h.oc.handlePeerNamespaceAndPodAdd(extraParameters.np, extraParameters.gp,
			extraParameters.podSelector, obj)

	case factory.PeerPodForNamespaceAndPodSelectorType:
		extraParameters := h.extraParameters.(*NetworkPolicyExtraParameters)
		return h.oc.handlePeerPodSelectorAddUpdate(extraParameters.np, extraParameters.gp, obj)

	case factory.PeerNamespaceSelectorType:
		extraParameters := h.extraParameters.(*NetworkPolicyExtraParameters)
		return h.oc.handlePeerNamespaceSelectorAdd(extraParameters.np, extraParameters.gp, obj)

	case factory.LocalPodSelectorType:
		extraParameters := h.extraParameters.(*NetworkPolicyExtraParameters)
		return h.oc.handleLocalPodSelectorAddFunc(
			extraParameters.np,
			obj)

	case factory.EgressFirewallType:
		var err error
		egressFirewall := obj.(*egressfirewall.EgressFirewall).DeepCopy()
		if err = h.oc.addEgressFirewall(egressFirewall); err != nil {
			egressFirewall.Status.Status = egressFirewallAddError
		} else {
			egressFirewall.Status.Status = egressFirewallAppliedCorrectly
			metrics.UpdateEgressFirewallRuleCount(float64(len(egressFirewall.Spec.Egress)))
			metrics.IncrementEgressFirewallCount()
		}
		if err = h.oc.updateEgressFirewallStatusWithRetry(egressFirewall); err != nil {
			klog.Errorf("Failed to update egress firewall status %s, error: %v",
				getEgressFirewallNamespacedName(egressFirewall), err)
		}
		return err

	case factory.EgressIPType:
		eIP := obj.(*egressipv1.EgressIP)
		return h.oc.reconcileEgressIP(nil, eIP)

	case factory.EgressIPNamespaceType:
		namespace := obj.(*kapi.Namespace)
		return h.oc.reconcileEgressIPNamespace(nil, namespace)

	case factory.EgressIPPodType:
		pod := obj.(*kapi.Pod)
		return h.oc.reconcileEgressIPPod(nil, pod)

	case factory.EgressNodeType:
		node := obj.(*kapi.Node)
		if err := h.oc.setupNodeForEgress(node); err != nil {
			return err
		}
		nodeEgressLabel := util.GetNodeEgressLabel()
		nodeLabels := node.GetLabels()
		_, hasEgressLabel := nodeLabels[nodeEgressLabel]
		if hasEgressLabel {
			h.oc.setNodeEgressAssignable(node.Name, true)
		}
		isReady := h.oc.isEgressNodeReady(node)
		if isReady {
			h.oc.setNodeEgressReady(node.Name, true)
		}
		isReachable := h.oc.isEgressNodeReachable(node)
		if isReachable {
			h.oc.setNodeEgressReachable(node.Name, true)
		}
		if hasEgressLabel && isReachable && isReady {
			if err := h.oc.addEgressNode(node.Name); err != nil {
				return err
			}
		}

	case factory.CloudPrivateIPConfigType:
		cloudPrivateIPConfig := obj.(*ocpcloudnetworkapi.CloudPrivateIPConfig)
		return h.oc.reconcileCloudPrivateIPConfig(nil, cloudPrivateIPConfig)

	case factory.NamespaceType:
		ns, ok := obj.(*kapi.Namespace)
		if !ok {
			return fmt.Errorf("could not cast %T object to *kapi.Namespace", obj)
		}
		return h.oc.AddNamespace(ns)

	default:
		return fmt.Errorf("no add function for object type %s", h.objType)
	}

	return nil
}

// Given a *RetryFramework instance, an old and a new object, UpdateResource updates the specified object in the cluster
// to its version in newObj according to its type and returns the error, if any, yielded during the object update.
// The inRetryCache boolean argument is to indicate if the given resource is in the retryCache or not.
func (h *masterEventHandler) UpdateResource(oldObj, newObj interface{}, inRetryCache bool) error {
	switch h.objType {
	case factory.PodType:
		oldPod := oldObj.(*kapi.Pod)
		newPod := newObj.(*kapi.Pod)

		return h.oc.ensurePod(oldPod, newPod, inRetryCache || util.PodScheduled(oldPod) != util.PodScheduled(newPod))

	case factory.NodeType:
		newNode, ok := newObj.(*kapi.Node)
		if !ok {
			return fmt.Errorf("could not cast newObj of type %T to *kapi.Node", newObj)
		}
		oldNode, ok := oldObj.(*kapi.Node)
		if !ok {
			return fmt.Errorf("could not cast oldObj of type %T to *kapi.Node", oldObj)
		}
		// determine what actually changed in this update
		_, nodeSync := h.oc.addNodeFailed.Load(newNode.Name)
		_, failed := h.oc.nodeClusterRouterPortFailed.Load(newNode.Name)
		clusterRtrSync := failed || nodeChassisChanged(oldNode, newNode) || nodeSubnetChanged(oldNode, newNode)
		_, failed = h.oc.mgmtPortFailed.Load(newNode.Name)
		mgmtSync := failed || macAddressChanged(oldNode, newNode) || nodeSubnetChanged(oldNode, newNode)
		_, failed = h.oc.gatewaysFailed.Load(newNode.Name)
		gwSync := (failed || gatewayChanged(oldNode, newNode) ||
			nodeSubnetChanged(oldNode, newNode) || hostAddressesChanged(oldNode, newNode))
		_, hoSync := h.oc.hybridOverlayFailed.Load(newNode.Name)

		return h.oc.addUpdateNodeEvent(newNode, &nodeSyncs{nodeSync, clusterRtrSync, mgmtSync, gwSync, hoSync})

	case factory.PeerPodSelectorType:
		extraParameters := h.extraParameters.(*NetworkPolicyExtraParameters)
		return h.oc.handlePeerPodSelectorAddUpdate(extraParameters.np, extraParameters.gp, newObj)

	case factory.PeerPodForNamespaceAndPodSelectorType:
		extraParameters := h.extraParameters.(*NetworkPolicyExtraParameters)
		return h.oc.handlePeerPodSelectorAddUpdate(extraParameters.np, extraParameters.gp, newObj)

	case factory.LocalPodSelectorType:
		extraParameters := h.extraParameters.(*NetworkPolicyExtraParameters)
		return h.oc.handleLocalPodSelectorAddFunc(
			extraParameters.np,
			newObj)

	case factory.EgressIPType:
		oldEIP := oldObj.(*egressipv1.EgressIP)
		newEIP := newObj.(*egressipv1.EgressIP)
		return h.oc.reconcileEgressIP(oldEIP, newEIP)

	case factory.EgressIPNamespaceType:
		oldNamespace := oldObj.(*kapi.Namespace)
		newNamespace := newObj.(*kapi.Namespace)
		return h.oc.reconcileEgressIPNamespace(oldNamespace, newNamespace)

	case factory.EgressIPPodType:
		oldPod := oldObj.(*kapi.Pod)
		newPod := newObj.(*kapi.Pod)
		return h.oc.reconcileEgressIPPod(oldPod, newPod)

	case factory.EgressNodeType:
		oldNode := oldObj.(*kapi.Node)
		newNode := newObj.(*kapi.Node)
		// Initialize the allocator on every update,
		// ovnkube-node/cloud-network-config-controller will make sure to
		// annotate the node with the egressIPConfig, but that might have
		// happened after we processed the ADD for that object, hence keep
		// retrying for all UPDATEs.
		if err := h.oc.initEgressIPAllocator(newNode); err != nil {
			klog.Warningf("Egress node initialization error: %v", err)
		}
		nodeEgressLabel := util.GetNodeEgressLabel()
		oldLabels := oldNode.GetLabels()
		newLabels := newNode.GetLabels()
		_, oldHadEgressLabel := oldLabels[nodeEgressLabel]
		_, newHasEgressLabel := newLabels[nodeEgressLabel]
		// If the node is not labeled for egress assignment, just return
		// directly, we don't really need to set the ready / reachable
		// status on this node if the user doesn't care about using it.
		if !oldHadEgressLabel && !newHasEgressLabel {
			return nil
		}
		if oldHadEgressLabel && !newHasEgressLabel {
			klog.Infof("Node: %s has been un-labeled, deleting it from egress assignment", newNode.Name)
			h.oc.setNodeEgressAssignable(oldNode.Name, false)
			return h.oc.deleteEgressNode(oldNode.Name)
		}
		isOldReady := h.oc.isEgressNodeReady(oldNode)
		isNewReady := h.oc.isEgressNodeReady(newNode)
		isNewReachable := h.oc.isEgressNodeReachable(newNode)
		h.oc.setNodeEgressReady(newNode.Name, isNewReady)
		h.oc.setNodeEgressReachable(newNode.Name, isNewReachable)
		if !oldHadEgressLabel && newHasEgressLabel {
			klog.Infof("Node: %s has been labeled, adding it for egress assignment", newNode.Name)
			h.oc.setNodeEgressAssignable(newNode.Name, true)
			if isNewReady && isNewReachable {
				if err := h.oc.addEgressNode(newNode.Name); err != nil {
					return err
				}
			} else {
				klog.Warningf("Node: %s has been labeled, but node is not ready"+
					" and reachable, cannot use it for egress assignment", newNode.Name)
			}
			return nil
		}
		if isOldReady == isNewReady {
			return nil
		}
		if !isNewReady {
			klog.Warningf("Node: %s is not ready, deleting it from egress assignment", newNode.Name)
			if err := h.oc.deleteEgressNode(newNode.Name); err != nil {
				return err
			}
		} else if isNewReady && isNewReachable {
			klog.Infof("Node: %s is ready and reachable, adding it for egress assignment", newNode.Name)
			if err := h.oc.addEgressNode(newNode.Name); err != nil {
				return err
			}
		}
		return nil

	case factory.CloudPrivateIPConfigType:
		oldCloudPrivateIPConfig := oldObj.(*ocpcloudnetworkapi.CloudPrivateIPConfig)
		newCloudPrivateIPConfig := newObj.(*ocpcloudnetworkapi.CloudPrivateIPConfig)
		return h.oc.reconcileCloudPrivateIPConfig(oldCloudPrivateIPConfig, newCloudPrivateIPConfig)

	case factory.NamespaceType:
		oldNs, newNs := oldObj.(*kapi.Namespace), newObj.(*kapi.Namespace)
		return h.oc.updateNamespace(oldNs, newNs)
	}
	return fmt.Errorf("no update function for object type %s", h.objType)
}

// Given a *RetryFramework instance, an object and optionally a cachedObj, DeleteResource deletes the object from the cluster
// according to the delete logic of its resource type. cachedObj is the internal cache entry for this object,
// used for now for pods and network policies.
func (h *masterEventHandler) DeleteResource(obj, cachedObj interface{}) error {
	switch h.objType {
	case factory.PodType:
		var portInfo *lpInfo
		pod := obj.(*kapi.Pod)

		if cachedObj != nil {
			portInfo = cachedObj.(*lpInfo)
		}
		h.oc.logicalPortCache.remove(util.GetLogicalPortName(pod.Namespace, pod.Name))
		return h.oc.removePod(pod, portInfo)

	case factory.PolicyType:
		knp, ok := obj.(*knet.NetworkPolicy)
		if !ok {
			return fmt.Errorf("could not cast obj of type %T to *knet.NetworkPolicy", obj)
		}
		return h.oc.deleteNetworkPolicy(knp)

	case factory.NodeType:
		node, ok := obj.(*kapi.Node)
		if !ok {
			return fmt.Errorf("could not cast obj of type %T to *knet.Node", obj)
		}
		return h.oc.deleteNodeEvent(node)

	case factory.PeerPodSelectorType:
		extraParameters := h.extraParameters.(*NetworkPolicyExtraParameters)
		return h.oc.handlePeerPodSelectorDelete(extraParameters.np, extraParameters.gp, obj)

	case factory.PeerNamespaceAndPodSelectorType:
		extraParameters := h.extraParameters.(*NetworkPolicyExtraParameters)
		return h.oc.handlePeerNamespaceAndPodDel(extraParameters.np, extraParameters.gp, obj)

	case factory.PeerPodForNamespaceAndPodSelectorType:
		extraParameters := h.extraParameters.(*NetworkPolicyExtraParameters)
		return h.oc.handlePeerPodSelectorDelete(extraParameters.np, extraParameters.gp, obj)

	case factory.PeerNamespaceSelectorType:
		extraParameters := h.extraParameters.(*NetworkPolicyExtraParameters)
		return h.oc.handlePeerNamespaceSelectorDel(extraParameters.np, extraParameters.gp, obj)

	case factory.LocalPodSelectorType:
		extraParameters := h.extraParameters.(*NetworkPolicyExtraParameters)
		return h.oc.handleLocalPodSelectorDelFunc(
			extraParameters.np,
			obj)

	case factory.EgressFirewallType:
		egressFirewall := obj.(*egressfirewall.EgressFirewall)
		if err := h.oc.deleteEgressFirewall(egressFirewall); err != nil {
			return err
		}
		metrics.UpdateEgressFirewallRuleCount(float64(-len(egressFirewall.Spec.Egress)))
		metrics.DecrementEgressFirewallCount()
		return nil

	case factory.EgressIPType:
		eIP := obj.(*egressipv1.EgressIP)
		return h.oc.reconcileEgressIP(eIP, nil)

	case factory.EgressIPNamespaceType:
		namespace := obj.(*kapi.Namespace)
		return h.oc.reconcileEgressIPNamespace(namespace, nil)

	case factory.EgressIPPodType:
		pod := obj.(*kapi.Pod)
		return h.oc.reconcileEgressIPPod(pod, nil)

	case factory.EgressNodeType:
		node := obj.(*kapi.Node)
		if err := h.oc.deleteNodeForEgress(node); err != nil {
			return err
		}
		nodeEgressLabel := util.GetNodeEgressLabel()
		nodeLabels := node.GetLabels()
		if _, hasEgressLabel := nodeLabels[nodeEgressLabel]; hasEgressLabel {
			if err := h.oc.deleteEgressNode(node.Name); err != nil {
				return err
			}
		}
		return nil

	case factory.CloudPrivateIPConfigType:
		cloudPrivateIPConfig := obj.(*ocpcloudnetworkapi.CloudPrivateIPConfig)
		return h.oc.reconcileCloudPrivateIPConfig(cloudPrivateIPConfig, nil)

	case factory.NamespaceType:
		ns := obj.(*kapi.Namespace)
		return h.oc.deleteNamespace(ns)

	default:
		return fmt.Errorf("object type %s not supported", h.objType)
	}
}

func (h *masterEventHandler) SyncFunc(objs []interface{}) error {
	var syncFunc func([]interface{}) error

	if h.syncFunc != nil {
		// syncFunc was provided explicitly
		syncFunc = h.syncFunc
	} else {
		switch h.objType {
		case factory.PodType:
			syncFunc = h.oc.syncPods

		case factory.PolicyType:
			syncFunc = h.oc.syncNetworkPolicies

		case factory.NodeType:
			syncFunc = h.oc.syncNodes

		case factory.LocalPodSelectorType,
			factory.PeerNamespaceAndPodSelectorType,
			factory.PeerPodSelectorType,
			factory.PeerPodForNamespaceAndPodSelectorType,
			factory.PeerNamespaceSelectorType:
			syncFunc = nil

		case factory.EgressFirewallType:
			syncFunc = h.oc.syncEgressFirewall

		case factory.EgressIPNamespaceType:
			syncFunc = h.oc.syncEgressIPs

		case factory.EgressNodeType:
			syncFunc = h.oc.initClusterEgressPolicies

		case factory.EgressIPPodType,
			factory.EgressIPType,
			factory.CloudPrivateIPConfigType:
			syncFunc = nil

		case factory.NamespaceType:
			syncFunc = h.oc.syncNamespaces

		default:
			return fmt.Errorf("no sync function for object type %s", h.objType)
		}
	}
	if syncFunc == nil {
		return nil
	}
	return syncFunc(objs)
}

// IsObjectInTerminalState returns true if the object is in a terminal state.
// This is used now for pods that are either in a PodSucceeded or in a PodFailed state.
func (h *masterEventHandler) IsObjectInTerminalState(obj interface{}) bool {
	switch h.objType {
	case factory.PodType,
		factory.PeerPodSelectorType,
		factory.PeerPodForNamespaceAndPodSelectorType,
		factory.LocalPodSelectorType,
		factory.EgressIPPodType:
		pod := obj.(*kapi.Pod)
		return util.PodCompleted(pod)

	default:
		return false
	}
}
