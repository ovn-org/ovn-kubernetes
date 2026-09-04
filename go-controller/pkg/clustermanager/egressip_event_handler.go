// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package clustermanager

import (
	"fmt"
	"reflect"

	ocpcloudnetworkapi "github.com/openshift/api/cloudnetwork/v1"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	cache "k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	egressipv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	objretry "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/retry"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// egressIPClusterControllerEventHandler object handles the events
// from retry framework for the egressIPClusterController.
type egressIPClusterControllerEventHandler struct {
	objretry.DefaultEventHandler
	objType  reflect.Type
	eIPC     *egressIPClusterController
	syncFunc func([]interface{}) error
}

func (h *egressIPClusterControllerEventHandler) FilterOutResource(_ interface{}) bool {
	return false
}

// egressIPClusterControllerEventHandler functions

// AddResource adds the specified object to the cluster according to its type and
// returns the error, if any, yielded during object creation.
func (h *egressIPClusterControllerEventHandler) AddResource(obj interface{}, _ bool) error {
	switch h.objType {
	case factory.EgressNodeType:
		node := obj.(*corev1.Node)
		// EgressIP is not supported on hybrid overlay nodes
		if util.NoHostSubnet(node) {
			return nil
		}

		// Initialize the allocator on every add,
		// ovnkube-node/cloud-network-config-controller will make sure to
		// annotate the node with the egressIPConfig, but that might have
		// happened after we processed the ADD for that object, hence keep
		// retrying for all UPDATEs.
		// NOTE(tssurya): It is not clear why the allocator cache was initialized
		// for all the nodes in the cluster for every add and update node events,
		// and why it wasn't done only for egress assignable nodes. However given
		// we are now using node selectors logic, it is easier to track all nodes in
		// cache then have cache churn whenever label changes happen. Number of cluster
		// nodes can't exceed a few thousand at max anyways.
		if err := h.eIPC.initEgressIPAllocator(node); err != nil {
			klog.Warningf("Egress node initialization error: %v", err)
		}
		isReady := h.eIPC.isEgressNodeReady(node)
		if isReady {
			h.eIPC.setNodeEgressReady(node.Name, true)
		}
		// Record the node's intrinsic, node-local health independent of whether
		// any EgressIP currently selects it.
		//
		// Before egressNodeSelector was introduced the equivalent gate was
		// isEgressAssignable(node), which checked only the node's own
		// k8s.ovn.org/egress-assignable label and therefore required no knowledge
		// of EgressIP objects. When egressNodeSelector replaced that check with
		// compileEgressNodeSelectors() (which reads live EgressIP objects), a
		// chicken-and-egg ordering hazard was introduced: if a node is added
		// before any EgressIP exists, compileEgressNodeSelectors returns empty,
		// the node is never marked reachable/assignable, and EIPs created later
		// find zero usable nodes even though the node is healthy.
		//
		// isEgressAssignable is a pure function of the host-cidrs annotation
		// (present, parseable, non-empty), so record it for every node.
		hostCIDRs, err := util.ParseNodeHostCIDRsDropNetMask(node)
		nodeHasUsableHostCIDRs := err == nil && hostCIDRs.Len() > 0
		if err != nil {
			klog.Warningf("Node %s is not egress-assignable: failed to parse host-cidrs: %v", node.Name, err)
		} else if hostCIDRs.Len() == 0 {
			klog.Warningf("Node %s is not egress-assignable: host-cidrs contains no host CIDRs", node.Name)
		}
		h.eIPC.setNodeEgressAssignable(node.Name, nodeHasUsableHostCIDRs)

		isReachable := h.eIPC.isEgressNodeReachable(node)
		if isReachable && isReady {
			h.eIPC.setNodeEgressReachable(node.Name, true)
			// Only wake up the EIP reconciler if this node can satisfy at least
			// one EgressIP's selector; calling addEgressNode for every healthy
			// node would cause needless reconciliation churn for nodes no EIP
			// will ever use.
			if nodeHasUsableHostCIDRs {
				selectors, err := h.eIPC.compileEgressNodeSelectors()
				if err != nil {
					return fmt.Errorf("failed to add egress node %s: %w", node.Name, err)
				}
				nodeLabels := labels.Set(node.GetLabels())
				if matchesAnyEgressIPSelector(nodeLabels, selectors) {
					if err := h.eIPC.addEgressNode(node.Name); err != nil {
						return err
					}
				}
			}
		}
	case factory.EgressIPType:
		eIP := obj.(*egressipv1.EgressIP)
		return h.eIPC.reconcileEgressIP(nil, eIP)
	case factory.CloudPrivateIPConfigType:
		cloudPrivateIPConfig := obj.(*ocpcloudnetworkapi.CloudPrivateIPConfig)
		return h.eIPC.reconcileCloudPrivateIPConfig(nil, cloudPrivateIPConfig)
	default:
		return fmt.Errorf("no add function for object type %s", h.objType)
	}
	return nil
}

// UpdateResource updates the specified object in the cluster to its version in newObj according
// to its type and returns the error, if any, yielded during the object update.
// The inRetryCache boolean argument is to indicate if the given resource is in the retryCache or not.
func (h *egressIPClusterControllerEventHandler) UpdateResource(oldObj, newObj interface{}, inRetryCache bool) error {
	switch h.objType {
	case factory.EgressIPType:
		oldEIP := oldObj.(*egressipv1.EgressIP)
		newEIP := newObj.(*egressipv1.EgressIP)
		return h.eIPC.reconcileEgressIP(oldEIP, newEIP)
	case factory.EgressNodeType:
		oldNode := oldObj.(*corev1.Node)
		newNode := newObj.(*corev1.Node)

		// EgressIP is not supported on hybrid overlay nodes
		if util.NoHostSubnet(newNode) {
			return nil
		}

		// Initialize the allocator on every update,
		// ovnkube-node/cloud-network-config-controller will make sure to
		// annotate the node with the egressIPConfig, but that might have
		// happened after we processed the ADD for that object, hence keep
		// retrying for all UPDATEs.
		// NOTE(tssurya): It is not clear why the allocator cache was initialized
		// for all the nodes in the cluster for every add and update node events,
		// and why it wasn't done only for egress assignable nodes. However given
		// we are now using node selectors logic, it is easier to track all nodes in
		// cache then have cache churn whenever label changes happen. Number of cluster
		// nodes can't exceed a few thousand at max anyways.
		if err := h.eIPC.initEgressIPAllocator(newNode); err != nil {
			klog.Warningf("Egress node initialization error: %v", err)
		}

		// Determine whether this node matches any EgressIP's
		// egressNodeSelector — replaces the old well-known label check.
		selectors, err := h.eIPC.compileEgressNodeSelectors()
		if err != nil {
			return fmt.Errorf("failed to update egress node %s: %w", newNode.Name, err)
		}
		oldNodeLabels := labels.Set(oldNode.GetLabels())
		newNodeLabels := labels.Set(newNode.GetLabels())
		oldMatchesSelector := matchesAnyEgressIPSelector(oldNodeLabels, selectors)
		newMatchesSelector := matchesAnyEgressIPSelector(newNodeLabels, selectors)
		// Refresh node-local health for every node, whether or not it matches a
		// selector. isReady and isEgressAssignable are cheap, no-probe derivations
		// of the Node object whose inputs change only via node events, so re-deriving
		// them here keeps the cached flags fresh. A stale flag would make
		// getSortedEgressData pick a bad node, or skip a good one, when an EgressIP
		// later selects it. Reachability is not refreshed here: it needs a probe, is
		// not a Node field, and is established only for selected nodes (below, or by
		// the periodic reachability loop).
		isNewReady := h.eIPC.isEgressNodeReady(newNode)
		h.eIPC.setNodeEgressReady(newNode.Name, isNewReady)

		// isEgressAssignable is purely a function of the host-cidrs annotation, so
		// re-derive and record it only when that annotation actually changed.
		// oldUsableHostCIDRs/newUsableHostCIDRs are reused below to detect a
		// usable->unusable transition.
		isHostCIDRsAltered := util.NodeHostCIDRsAnnotationChanged(oldNode, newNode)
		var oldUsableHostCIDRs, newUsableHostCIDRs bool
		if isHostCIDRsAltered {
			oldCIDRs, oldErr := util.ParseNodeHostCIDRsDropNetMask(oldNode)
			oldUsableHostCIDRs = oldErr == nil && oldCIDRs.Len() > 0
			newCIDRs, newErr := util.ParseNodeHostCIDRsDropNetMask(newNode)
			newUsableHostCIDRs = newErr == nil && newCIDRs.Len() > 0
			if newErr != nil {
				klog.Warningf("Node %s is not egress-assignable: failed to parse host-cidrs: %v", newNode.Name, newErr)
			} else if newCIDRs.Len() == 0 {
				klog.Warningf("Node %s is not egress-assignable: host-cidrs contains no host CIDRs", newNode.Name)
			}
			h.eIPC.setNodeEgressAssignable(newNode.Name, newUsableHostCIDRs)
		}

		// If the node didn't and still doesn't match any EgressIP selector there are
		// no EIP assignments to reconcile; the health flags above are already fresh.
		// The selector filter is applied by getSortedEgressData at assignment time.
		if !oldMatchesSelector && !newMatchesSelector {
			return nil
		}

		// Host-cidrs transitioned from usable to unusable while still matching a
		// selector - delete assignments. Wipe the allocation cache first so the cache
		// and status.items stay in agreement even if the subsequent reconcile fails
		// partway through (this preserves the pre-refactor behavior where
		// setNodeEgressAssignable(false) wiped the cache as a side effect). A
		// transition is only possible when the annotation changed; inRetryCache
		// handles retries where the old object was overwritten.
		if isHostCIDRsAltered && newMatchesSelector && !newUsableHostCIDRs && (oldUsableHostCIDRs || inRetryCache) {
			klog.Infof("Node: %s is no longer assignable (host-cidrs annotation missing or invalid), "+
				"deleting it from egress assignment", newNode.Name)
			h.eIPC.clearNodeEgressAllocations(newNode.Name)
			if err := h.eIPC.deleteEgressNode(newNode.Name); err != nil {
				return fmt.Errorf("failed to delete egress assignments for node %s: %w", newNode.Name, err)
			}
			return nil
		}

		// Node stopped matching all selectors - remove from assignment, wiping its
		// allocation cache first.
		if oldMatchesSelector && !newMatchesSelector {
			klog.Infof("Node: %s no longer matches any EgressIP selector, deleting it from egress assignment", newNode.Name)
			h.eIPC.clearNodeEgressAllocations(newNode.Name)
			return h.eIPC.deleteEgressNode(oldNode.Name)
		}

		isOldReady := h.eIPC.isEgressNodeReady(oldNode)
		isNewReachable := h.eIPC.isEgressNodeReachable(newNode)
		isCloudEgressIPConfigAltered := util.CloudEgressIPConfigAnnotationChanged(oldNode, newNode)

		// Node started matching a selector — add for assignment.
		if !oldMatchesSelector && newMatchesSelector {
			klog.Infof("Node: %s now matches an EgressIP selector, adding it for egress assignment", newNode.Name)
			if isNewReady && isNewReachable {
				h.eIPC.setNodeEgressReachable(newNode.Name, isNewReachable)
				if err := h.eIPC.addEgressNode(newNode.Name); err != nil {
					return err
				}
			} else {
				klog.Warningf("Node: %s now matches an EgressIP selector, but node is not ready"+
					" and reachable, cannot use it for egress assignment", newNode.Name)
			}
			return nil
		}

		// Both old and new match some selector. If labels changed and the
		// set of matching selectors changed (e.g. node now matches EIP-B
		// instead of EIP-A), re-evaluate assignments. This is a new case
		// that the legacysingle-label approach did not need.
		labelsChanged := !reflect.DeepEqual(oldNode.GetLabels(), newNode.GetLabels())
		if labelsChanged {
			// Did any EgressIP's selector flip its match result on this node?
			// If so, deleteEgressNode + addEgressNode re-evaluates. This is
			// churn-free: deleteEgressNode reconciles every EIP with a status on
			// this node, but validateEgressIPStatus (re-checking selector.Matches
			// against the updated cached labels) only evicts EIPs that actually
			// stopped matching; still-matching ones stay, and addEgressNode only
			// picks up EIPs newly eligible or not yet fully assigned. The
			// reconcile is the authoritative gate; this check just decides whether
			// to re-evaluate at all.
			selectorMatchChanged := false
			for _, sel := range selectors {
				if sel.Matches(oldNodeLabels) != sel.Matches(newNodeLabels) {
					selectorMatchChanged = true
					break
				}
			}
			if selectorMatchChanged {
				klog.V(5).Infof("Node: %s labels changed, re-evaluating egress IP selectors", newNode.Name)
				if err := h.eIPC.deleteEgressNode(newNode.Name); err != nil {
					return err
				}
				if isNewReady && isNewReachable {
					h.eIPC.setNodeEgressReachable(newNode.Name, isNewReachable)
					if err := h.eIPC.addEgressNode(newNode.Name); err != nil {
						return err
					}
				}
			}
		}

		if isOldReady == isNewReady && !isHostCIDRsAltered && !isCloudEgressIPConfigAltered {
			return nil
		}
		if !isNewReady {
			klog.Warningf("Node: %s is not ready, deleting it from egress assignment", newNode.Name)
			if err := h.eIPC.deleteEgressNode(newNode.Name); err != nil {
				return err
			}
		} else if isNewReady && isNewReachable {
			// Build a log message that captures all reasons we are re-evaluating,
			// so operators can correlate annotation changes with re-assignments even
			// when multiple conditions change simultaneously.
			switch {
			case isCloudEgressIPConfigAltered && isHostCIDRsAltered:
				klog.Infof("Node: %s cloud egress IP config annotation and host CIDRs changed, re-evaluating egress IP assignments", newNode.Name)
			case isCloudEgressIPConfigAltered:
				klog.Infof("Node: %s cloud egress IP config annotation changed, re-evaluating egress IP assignments", newNode.Name)
			default:
				klog.Infof("Node: %s is ready and reachable, adding it for egress assignment", newNode.Name)
			}
			h.eIPC.setNodeEgressReachable(newNode.Name, isNewReachable)
			if err := h.eIPC.addEgressNode(newNode.Name); err != nil {
				return err
			}
		}
		if isHostCIDRsAltered {
			// we only need to consider EIPs that are assigned to networks that aren't managed by OVN
			if err := h.eIPC.reconcileSecondaryHostNetworkEIPs(newNode); err != nil {
				return fmt.Errorf("failed to reconsider egress IPs that are secondary host networks: %v", err)
			}
		}
		return nil
	case factory.CloudPrivateIPConfigType:
		oldCloudPrivateIPConfig := oldObj.(*ocpcloudnetworkapi.CloudPrivateIPConfig)
		newCloudPrivateIPConfig := newObj.(*ocpcloudnetworkapi.CloudPrivateIPConfig)
		return h.eIPC.reconcileCloudPrivateIPConfig(oldCloudPrivateIPConfig, newCloudPrivateIPConfig)
	default:
		return fmt.Errorf("no update function for object type %s", h.objType)
	}
}

// DeleteResource deletes the object from the cluster according to the delete logic of its resource type.
// cachedObj is the internal cache entry for this object, used for now for pods and network policies.
func (h *egressIPClusterControllerEventHandler) DeleteResource(obj, _ interface{}) error {
	switch h.objType {
	case factory.EgressIPType:
		eIP := obj.(*egressipv1.EgressIP)
		return h.eIPC.reconcileEgressIP(eIP, nil)
	case factory.EgressNodeType:
		node := obj.(*corev1.Node)
		// EgressIP is not supported on hybrid overlay nodes
		if util.NoHostSubnet(node) {
			return nil
		}
		h.eIPC.deleteNodeForEgress(node)
		// Propagate a selector-compilation failure so the delete is retried:
		// otherwise a node matching an EgressIP selector could skip
		// deleteEgressNode and report success, stranding its assignments.
		selectors, err := h.eIPC.compileEgressNodeSelectors()
		if err != nil {
			return fmt.Errorf("failed to delete egress node %s: %w", node.Name, err)
		}
		nodeLabels := labels.Set(node.GetLabels())
		if matchesAnyEgressIPSelector(nodeLabels, selectors) {
			if err := h.eIPC.deleteEgressNode(node.Name); err != nil {
				return err
			}
		}
		return nil
	case factory.CloudPrivateIPConfigType:
		cloudPrivateIPConfig := obj.(*ocpcloudnetworkapi.CloudPrivateIPConfig)
		return h.eIPC.reconcileCloudPrivateIPConfig(cloudPrivateIPConfig, nil)
	default:
		return fmt.Errorf("no delete function for object type %s", h.objType)
	}
}

func (h *egressIPClusterControllerEventHandler) SyncFunc(objs []interface{}) error {
	var syncFunc func([]interface{}) error

	if h.syncFunc != nil {
		// syncFunc was provided explicitly
		syncFunc = h.syncFunc
	} else {
		switch h.objType {
		case factory.EgressIPType:
			syncFunc = h.eIPC.syncEgressIPMarkAllocator
		case factory.EgressNodeType:
			syncFunc = h.eIPC.initEgressNodeReachability
		case factory.CloudPrivateIPConfigType:
			syncFunc = h.eIPC.syncCloudPrivateIPConfigs

		default:
			return fmt.Errorf("no sync function for object type %s", h.objType)
		}
	}
	if syncFunc == nil {
		return nil
	}
	return syncFunc(objs)
}

// getResourceFromInformerCache returns the latest state of the object from the informers cache
// given an object key and its type
func (h *egressIPClusterControllerEventHandler) GetResourceFromInformerCache(key string) (interface{}, error) {
	var obj interface{}
	var name string
	var err error

	_, name, err = cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return nil, fmt.Errorf("failed to split key %s: %v", key, err)
	}

	switch h.objType {
	case factory.EgressNodeType:
		obj, err = h.eIPC.watchFactory.GetNode(name)
	case factory.CloudPrivateIPConfigType:
		obj, err = h.eIPC.watchFactory.GetCloudPrivateIPConfig(name)
	case factory.EgressIPType:
		obj, err = h.eIPC.watchFactory.GetEgressIP(name)

	default:
		err = fmt.Errorf("object type %s not supported, cannot retrieve it from informers cache",
			h.objType)
	}
	return obj, err
}
