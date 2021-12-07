package ovn

import (
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/libovsdb/ovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	egressipv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	kapi "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	errors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	utilwait "k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
)

type egressIPDialer interface {
	dial(ip net.IP) bool
}

var dialer egressIPDialer = &egressIPDial{}

func (oc *Controller) addEgressIP(eIP *egressipv1.EgressIP) error {
	// If the status is set at this point, then we know it's valid from syncEgressIP and we have no assignment to do.
	// Just initialize all watchers (which should not re-create any already existing items in the OVN DB)
	if len(eIP.Status.Items) == 0 {
		if err := oc.assignEgressIPs(eIP); err != nil {
			return fmt.Errorf("unable to assign egress IP: %s, error: %v", eIP.Name, err)
		}
	}

	oc.eIPC.namespaceHandlerMutex.Lock()
	defer oc.eIPC.namespaceHandlerMutex.Unlock()

	sel, err := metav1.LabelSelectorAsSelector(&eIP.Spec.NamespaceSelector)
	if err != nil {
		return fmt.Errorf("invalid namespaceSelector on EgressIP %s: %v", eIP.Name, err)
	}
	if _, exists := oc.eIPC.namespaceHandlerCache[getEgressIPKey(eIP)]; !exists {
		h := oc.watchFactory.AddFilteredNamespaceHandler("", sel,
			cache.ResourceEventHandlerFuncs{
				AddFunc: func(obj interface{}) {
					namespace := obj.(*kapi.Namespace)
					klog.V(5).Infof("EgressIP: %s has matched on namespace: %s", eIP.Name, namespace.Name)
					if err := oc.addNamespaceEgressIP(eIP, namespace); err != nil {
						klog.Errorf("error: unable to add namespace handler for EgressIP: %s, err: %v", eIP.Name, err)
					}
				},
				UpdateFunc: func(oldObj, newObj interface{}) {},
				DeleteFunc: func(obj interface{}) {
					namespace := obj.(*kapi.Namespace)
					klog.V(5).Infof("EgressIP: %s stopped matching on namespace: %s", eIP.Name, namespace.Name)
					if err := oc.deleteNamespaceEgressIP(eIP, namespace); err != nil {
						klog.Errorf("error: unable to delete namespace handler for EgressIP: %s, err: %v", eIP.Name, err)
					}
				},
			}, nil)
		oc.eIPC.namespaceHandlerCache[getEgressIPKey(eIP)] = h
	} else {
		klog.Error("The namespace handler cache for egress IPs is de-synchronized: a namespace handler already exists for egress IP: %s", getEgressIPKey(eIP))
	}
	return nil
}

func (oc *Controller) deleteEgressIP(eIP *egressipv1.EgressIP) error {
	oc.releaseEgressIPs(eIP)

	oc.eIPC.namespaceHandlerMutex.Lock()
	defer oc.eIPC.namespaceHandlerMutex.Unlock()
	if nH, exists := oc.eIPC.namespaceHandlerCache[getEgressIPKey(eIP)]; exists {
		oc.watchFactory.RemoveNamespaceHandler(nH)
		delete(oc.eIPC.namespaceHandlerCache, getEgressIPKey(eIP))
	}

	oc.eIPC.podHandlerMutex.Lock()
	defer oc.eIPC.podHandlerMutex.Unlock()
	namespaces, err := oc.kube.GetNamespaces(eIP.Spec.NamespaceSelector)
	if err != nil {
		return err
	}
	for _, namespace := range namespaces.Items {
		if pH, exists := oc.eIPC.podHandlerCache[getNamespaceKey(&namespace)]; exists {
			oc.watchFactory.RemovePodHandler(pH)
			delete(oc.eIPC.podHandlerCache, getNamespaceKey(&namespace))
		}
		if err := oc.deleteNamespacePodsEgressIP(eIP, &namespace); err != nil {
			return err
		}
	}
	return nil
}

func (oc *Controller) isEgressNodeReady(egressNode *kapi.Node) bool {
	for _, condition := range egressNode.Status.Conditions {
		if condition.Type == v1.NodeReady {
			return condition.Status == v1.ConditionTrue
		}
	}
	return false
}

func (oc *Controller) isEgressNodeReachable(egressNode *kapi.Node) bool {
	oc.eIPC.allocator.Lock()
	defer oc.eIPC.allocator.Unlock()
	if eNode, exists := oc.eIPC.allocator.cache[egressNode.Name]; exists {
		return eNode.isReachable || oc.isReachable(eNode)
	}
	return false
}

func (oc *Controller) syncEgressIPs(eIPs []interface{}) {
	oc.eIPC.allocator.Lock()
	defer oc.eIPC.allocator.Unlock()
	for _, eIP := range eIPs {
		eIP, ok := eIP.(*egressipv1.EgressIP)
		if !ok {
			klog.Errorf("Spurious object in syncEgressIPs: %v", eIP)
			continue
		}
		var validAssignment bool
		for _, eIPStatus := range eIP.Status.Items {
			validAssignment = false
			eNode, exists := oc.eIPC.allocator.cache[eIPStatus.Node]
			if !exists {
				klog.Errorf("Allocator error: EgressIP: %s claims to have an allocation on a node which is unassignable for egress IP: %s", eIP.Name, eIPStatus.Node)
				break
			}
			if eNode.tainted {
				klog.Errorf("Allocator error: EgressIP: %s claims multiple egress IPs on same node: %s, will attempt rebalancing", eIP.Name, eIPStatus.Node)
				break
			}
			if !eNode.isEgressAssignable {
				klog.Errorf("Allocator error: EgressIP: %s assigned to node: %s which does not have egress label, will attempt rebalancing", eIP.Name, eIPStatus.Node)
				break
			}
			if !eNode.isReachable {
				klog.Errorf("Allocator error: EgressIP: %s assigned to node: %s which is not reachable, will attempt rebalancing", eIP.Name, eIPStatus.Node)
				break
			}
			if !eNode.isReady {
				klog.Errorf("Allocator error: EgressIP: %s assigned to node: %s which is not ready, will attempt rebalancing", eIP.Name, eIPStatus.Node)
				break
			}
			ip := net.ParseIP(eIPStatus.EgressIP)
			if ip == nil {
				klog.Errorf("Allocator error: EgressIP allocation contains unparsable IP address: %s", eIPStatus.EgressIP)
				break
			}
			if node := oc.isAnyClusterNodeIP(ip); node != nil {
				klog.Errorf("Allocator error: EgressIP allocation: %s is the IP of node: %s ", ip.String(), node.name)
				break
			}
			if utilnet.IsIPv6(ip) && eNode.v6Subnet != nil {
				if !eNode.v6Subnet.Contains(ip) {
					klog.Errorf("Allocator error: EgressIP allocation: %s on subnet: %s which cannot host it", ip.String(), eNode.v6Subnet.String())
					break
				}
			} else if !utilnet.IsIPv6(ip) && eNode.v4Subnet != nil {
				if !eNode.v4Subnet.Contains(ip) {
					klog.Errorf("Allocator error: EgressIP allocation: %s on subnet: %s which cannot host it", ip.String(), eNode.v4Subnet.String())
					break
				}
			} else {
				klog.Errorf("Allocator error: EgressIP allocation on node: %s which does not support its IP protocol version", eIPStatus.Node)
				break
			}
			validAssignment = true
			eNode.tainted = true
		}
		// In the unlikely event that any status has been misallocated previously:
		// unassign that by updating the entire status and re-allocate properly in addEgressIP
		if !validAssignment {
			namespaces, err := oc.kube.GetNamespaces(eIP.Spec.NamespaceSelector)
			if err != nil {
				klog.Errorf("Unable to list namespaces matched by EgressIP: %s, err: %v", getEgressIPKey(eIP), err)
				continue
			}
			for _, namespace := range namespaces.Items {
				if err := oc.deleteNamespacePodsEgressIP(eIP, &namespace); err != nil {
					klog.Error(err)
				}
			}
			eIP.Status = egressipv1.EgressIPStatus{
				Items: []egressipv1.EgressIPStatusItem{},
			}
			if err := oc.updateEgressIPWithRetry(eIP); err != nil {
				klog.Error(err)
				continue
			}
		}
		for _, eNode := range oc.eIPC.allocator.cache {
			eNode.tainted = false
		}
	}
	if err := oc.eIPC.deleteLegacyEgressReroutePolicies(); err != nil {
		klog.Errorf("Unable to clean up legacy egress IP setup, err: %v", err)
	}

	// This part will take of syncing stale data which we might have in OVN if
	// there's no ovnkube-master running for a while, while there are changes to
	// pods/egress IPs.
	// It will sync:
	// - Egress IPs which have been deleted while ovnkube-master was down
	// - pods/namespaces which have stopped matching on egress IPs while
	//   ovnkube-master was down
	if egressIPToPodIPCache, err := oc.generatePodIPCacheForEgressIP(eIPs); err == nil {
		oc.syncStaleEgressReroutePolicy(egressIPToPodIPCache)
		oc.syncStaleNATRules(egressIPToPodIPCache)
	}
}

func (oc *Controller) syncStaleEgressReroutePolicy(egressIPToPodIPCache map[string]sets.String) {
	logicalRouter := nbdb.LogicalRouter{}
	logicalRouterPolicyRes := []nbdb.LogicalRouterPolicy{}
	opModels := []libovsdbops.OperationModel{
		{
			ModelPredicate: func(lrp *nbdb.LogicalRouterPolicy) bool { return lrp.Priority == types.EgressIPReroutePriority },
			ExistingResult: &logicalRouterPolicyRes,
			DoAfter: func() {
				for _, item := range logicalRouterPolicyRes {
					egressIPName := item.ExternalIDs["name"]
					podIPCache, exists := egressIPToPodIPCache[egressIPName]
					splitMatch := strings.Split(item.Match, " ")
					logicalIP := splitMatch[len(splitMatch)-1]
					parsedLogicalIP := net.ParseIP(logicalIP)
					if !exists || !podIPCache.Has(parsedLogicalIP.String()) {
						logicalRouter.Policies = append(logicalRouter.Policies, item.UUID)
					}
				}
			},
			BulkOp: true,
		},
		{
			Model:          &logicalRouter,
			ModelPredicate: func(lr *nbdb.LogicalRouter) bool { return lr.Name == types.OVNClusterRouter },
			OnModelMutations: []interface{}{
				&logicalRouter.Policies,
			},
		},
	}
	if err := oc.modelClient.Delete(opModels...); err != nil {
		klog.Errorf("Unable to remove stale logical router policies, err: %v", err)
	}
}

func (oc *Controller) syncStaleNATRules(egressIPToPodIPCache map[string]sets.String) {
	predicate := func(item *nbdb.NAT) bool {
		egressIPName, exists := item.ExternalIDs["name"]
		// Skip nat rows that do not have egressIPName attribute available
		if !exists {
			return false
		}
		parsedLogicalIP := net.ParseIP(item.LogicalIP).String()
		podIPCache, exists := egressIPToPodIPCache[egressIPName]
		return !exists || !podIPCache.Has(parsedLogicalIP)
	}

	nats, err := libovsdbops.FindNatsUsingPredicate(oc.nbClient, predicate)
	if err != nil {
		klog.Errorf("Unable to sync egress IPs err: %v", err)
		return
	}

	if len(nats) == 0 {
		// No stale nat entries to deal with: noop.
		return
	}

	routers, err := libovsdbops.FindRoutersUsingNat(oc.nbClient, nats)
	if err != nil {
		klog.Errorf("Unable to sync egress IPs, err: %v", err)
		return
	}

	ops := []ovsdb.Operation{}
	for _, router := range routers {
		ops, err = libovsdbops.DeleteNatsFromRouterOps(oc.nbClient, ops, &router, nats...)
		if err != nil {
			klog.Errorf("Error deleting stale NAT from router %s: %v", router.Name, err)
			continue
		}
	}

	_, err = libovsdbops.TransactAndCheck(oc.nbClient, ops)
	if err != nil {
		klog.Errorf("Error deleting stale NATs: %v", err)
	}
}

// generatePodIPCacheForEgressIP builds a cache of egressIP name -> podIPs for fast
// access when syncing egress IPs. The Egress IP setup will return a lot of
// atomic items with the same general information repeated across most (egressIP
// name, logical IP defined for that name), hence use a cache to avoid round
// trips to the API server per item.
func (oc *Controller) generatePodIPCacheForEgressIP(eIPs []interface{}) (map[string]sets.String, error) {
	egressIPToPodIPCache := make(map[string]sets.String)
	for _, eIP := range eIPs {
		egressIP, ok := eIP.(*egressipv1.EgressIP)
		if !ok {
			continue
		}
		egressIPToPodIPCache[egressIP.Name] = sets.NewString()
		namespaces, err := oc.watchFactory.GetNamespacesBySelector(egressIP.Spec.NamespaceSelector)
		if err != nil {
			klog.Errorf("Error building egress IP sync cache, cannot retrieve namespaces for EgressIP: %s, err: %v", egressIP.Name, err)
			continue
		}
		for _, namespace := range namespaces {
			pods, err := oc.watchFactory.GetPodsBySelector(namespace.Name, egressIP.Spec.PodSelector)
			if err != nil {
				klog.Errorf("Error building egress IP sync cache, cannot retrieve pods for namespace: %s and egress IP: %s, err: %v", namespace.Name, egressIP.Name, err)
				continue
			}
			for _, pod := range pods {
				for _, podIP := range pod.Status.PodIPs {
					ip := net.ParseIP(podIP.IP)
					egressIPToPodIPCache[egressIP.Name].Insert(ip.String())
				}
			}
		}
	}
	return egressIPToPodIPCache, nil
}

func (oc *Controller) isAnyClusterNodeIP(ip net.IP) *egressNode {
	for _, eNode := range oc.eIPC.allocator.cache {
		if ip.Equal(eNode.v6IP) || ip.Equal(eNode.v4IP) {
			return eNode
		}
	}
	return nil
}

func (oc *Controller) updateEgressIPWithRetry(eIP *egressipv1.EgressIP) error {
	retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		return oc.kube.UpdateEgressIP(eIP)
	})
	if retryErr != nil {
		return fmt.Errorf("error in updating status on EgressIP %s: %v", eIP.Name, retryErr)
	}
	return nil
}

func (oc *Controller) addNamespaceEgressIP(eIP *egressipv1.EgressIP, namespace *kapi.Namespace) error {
	oc.eIPC.podHandlerMutex.Lock()
	defer oc.eIPC.podHandlerMutex.Unlock()
	sel, err := metav1.LabelSelectorAsSelector(&eIP.Spec.PodSelector)
	if err != nil {
		return fmt.Errorf("invalid podSelector on EgressIP %s: %v", eIP.Name, err)
	}
	if _, exists := oc.eIPC.podHandlerCache[getNamespaceKey(namespace)]; !exists {
		h := oc.watchFactory.AddFilteredPodHandler(namespace.Name, sel,
			cache.ResourceEventHandlerFuncs{
				AddFunc: func(obj interface{}) {
					pod := obj.(*kapi.Pod)
					klog.V(5).Infof("EgressIP: %s has matched on pod: %s in namespace: %s", eIP.Name, pod.Name, namespace.Name)
					if err := oc.eIPC.addPodEgressIP(eIP, pod); err != nil {
						klog.Errorf("Unable to add pod: %s/%s to EgressIP: %s, err: %v", pod.Namespace, pod.Name, eIP.Name, err)
					}
				},
				UpdateFunc: func(oldObj, newObj interface{}) {
					newPod := newObj.(*kapi.Pod)
					// FYI: the only pod update we care about here is the pod being assigned an IP, which
					// it didn't have when we received the ADD. If the label is changed and it stops matching:
					// this watcher receives a delete.
					if oc.eIPC.needsRetry(newPod) {
						klog.V(5).Infof("EgressIP: %s update for pod: %s in namespace: %s", eIP.Name, newPod.Name, namespace.Name)
						if err := oc.eIPC.addPodEgressIP(eIP, newPod); err != nil {
							klog.Errorf("Unable to add pod: %s/%s to EgressIP: %s, err: %v", newPod.Namespace, newPod.Name, eIP.Name, err)
						}
					}
				},
				DeleteFunc: func(obj interface{}) {
					pod := obj.(*kapi.Pod)
					// FYI: we can be in a situation where we processed a pod ADD for which there was no IP
					// address assigned. If the pod is deleted before the IP address is assigned,
					// we should not process that delete (as nothing exists in OVN for it)
					klog.V(5).Infof("EgressIP: %s has stopped matching on pod: %s in namespace: %s, needs delete: %v", eIP.Name, pod.Name, namespace.Name, !oc.eIPC.needsRetry(pod))
					if !oc.eIPC.needsRetry(pod) {
						if err := oc.eIPC.deletePodEgressIP(eIP, pod); err != nil {
							klog.Errorf("Unable to delete pod: %s/%s to EgressIP: %s, err: %v", pod.Namespace, pod.Name, eIP.Name, err)
						}
					}
				},
			}, nil)
		oc.eIPC.podHandlerCache[getNamespaceKey(namespace)] = h
	} else {
		klog.Errorf("The pod handler cache for egress IPs is de-synchronized: a pod handler already exists for namespace: %s", getNamespaceKey(namespace))
	}
	return nil
}

func (oc *Controller) deleteNamespaceEgressIP(eIP *egressipv1.EgressIP, namespace *kapi.Namespace) error {
	oc.eIPC.podHandlerMutex.Lock()
	defer oc.eIPC.podHandlerMutex.Unlock()
	if pH, exists := oc.eIPC.podHandlerCache[getNamespaceKey(namespace)]; exists {
		oc.watchFactory.RemovePodHandler(pH)
		delete(oc.eIPC.podHandlerCache, getNamespaceKey(namespace))
	}
	if err := oc.deleteNamespacePodsEgressIP(eIP, namespace); err != nil {
		return err
	}
	return nil
}

func (oc *Controller) deleteNamespacePodsEgressIP(eIP *egressipv1.EgressIP, namespace *kapi.Namespace) error {
	pods, err := oc.kube.GetPods(namespace.Name, eIP.Spec.PodSelector)
	if err != nil {
		return err
	}
	for _, pod := range pods.Items {
		if !oc.eIPC.needsRetry(&pod) {
			if err := oc.eIPC.deletePodEgressIP(eIP, &pod); err != nil {
				return err
			}
		}
	}
	return nil
}

func (oc *Controller) assignEgressIPs(eIP *egressipv1.EgressIP) error {
	oc.eIPC.allocator.Lock()
	assignments := []egressipv1.EgressIPStatusItem{}
	defer func() {
		eIP.Status.Items = assignments
		oc.eIPC.allocator.Unlock()
	}()
	assignableNodes, existingAllocations := oc.getSortedEgressData()
	if len(assignableNodes) == 0 {
		oc.eIPC.assignmentRetry[eIP.Name] = true
		eIPRef := kapi.ObjectReference{
			Kind: "EgressIP",
			Name: eIP.Name,
		}
		oc.recorder.Eventf(&eIPRef, kapi.EventTypeWarning, "NoMatchingNodeFound", "no assignable nodes for EgressIP: %s, please tag at least one node with label: %s", eIP.Name, util.GetNodeEgressLabel())
		return fmt.Errorf("no assignable nodes")
	}
	klog.V(5).Infof("Current assignments are: %+v", existingAllocations)
	for _, egressIP := range eIP.Spec.EgressIPs {
		klog.V(5).Infof("Will attempt assignment for egress IP: %s", egressIP)
		eIPC := net.ParseIP(egressIP)
		if eIPC == nil {
			eIPRef := kapi.ObjectReference{
				Kind: "EgressIP",
				Name: eIP.Name,
			}
			oc.recorder.Eventf(&eIPRef, kapi.EventTypeWarning, "InvalidEgressIP", "egress IP: %s for object EgressIP: %s is not a valid IP address", egressIP, eIP.Name)
			return fmt.Errorf("unable to parse provided EgressIP: %s, invalid", egressIP)
		}
		if node := oc.isAnyClusterNodeIP(eIPC); node != nil {
			eIPRef := kapi.ObjectReference{
				Kind: "EgressIP",
				Name: eIP.Name,
			}
			oc.recorder.Eventf(
				&eIPRef,
				kapi.EventTypeWarning,
				"UnsupportedRequest",
				"Egress IP: %v for object EgressIP: %s is the IP address of node: %s, this is unsupported", eIPC, eIP.Name, node.name,
			)
			return fmt.Errorf("egress IP: %v is the IP address of node: %s", eIPC, node.name)
		}
		for i := 0; i < len(assignableNodes); i++ {
			klog.V(5).Infof("Attempting assignment on egress node: %+v", assignableNodes[i])
			if _, exists := existingAllocations[eIPC.String()]; exists {
				klog.V(5).Infof("EgressIP: %v is already allocated, skipping", eIPC)
				break
			}
			if assignableNodes[i].tainted {
				klog.V(5).Infof("Node: %s is already in use by another egress IP for this EgressIP: %s, trying another node", assignableNodes[i].name, eIP.Name)
				continue
			}
			if (assignableNodes[i].v6Subnet != nil && assignableNodes[i].v6Subnet.Contains(eIPC)) ||
				(assignableNodes[i].v4Subnet != nil && assignableNodes[i].v4Subnet.Contains(eIPC)) {
				assignableNodes[i].tainted, oc.eIPC.allocator.cache[assignableNodes[i].name].allocations[eIPC.String()] = true, true
				assignments = append(assignments, egressipv1.EgressIPStatusItem{
					EgressIP: eIPC.String(),
					Node:     assignableNodes[i].name,
				})
				klog.V(5).Infof("Successful assignment of egress IP: %s on node: %+v", egressIP, assignableNodes[i])
				break
			}
		}
	}
	if len(assignments) == 0 {
		oc.eIPC.assignmentRetry[eIP.Name] = true
		eIPRef := kapi.ObjectReference{
			Kind: "EgressIP",
			Name: eIP.Name,
		}
		oc.recorder.Eventf(&eIPRef, kapi.EventTypeWarning, "NoMatchingNodeFound", "No matching nodes found, which can host any of the egress IPs: %v for object EgressIP: %s", eIP.Spec.EgressIPs, eIP.Name)
		return fmt.Errorf("no matching host found")
	}
	if len(assignments) < len(eIP.Spec.EgressIPs) {
		oc.eIPC.assignmentRetry[eIP.Name] = true
		eIPRef := kapi.ObjectReference{
			Kind: "EgressIP",
			Name: eIP.Name,
		}
		oc.recorder.Eventf(&eIPRef, kapi.EventTypeWarning, "UnassignedRequest", "Not all egress IPs for EgressIP: %s could be assigned, please tag more nodes", eIP.Name)
	}
	return nil
}

func (oc *Controller) releaseEgressIPs(eIP *egressipv1.EgressIP) {
	oc.eIPC.allocator.Lock()
	defer oc.eIPC.allocator.Unlock()
	for _, status := range eIP.Status.Items {
		klog.V(5).Infof("Releasing egress IP assignment: %s", status.EgressIP)
		if node, exists := oc.eIPC.allocator.cache[status.Node]; exists {
			delete(node.allocations, status.EgressIP)
		}
		klog.V(5).Infof("Remaining allocations on node are: %+v", oc.eIPC.allocator.cache[status.Node])
	}
}

func (oc *Controller) getSortedEgressData() ([]egressNode, map[string]bool) {
	assignableNodes := []egressNode{}
	allAllocations := make(map[string]bool)
	for _, eNode := range oc.eIPC.allocator.cache {
		if eNode.isEgressAssignable && eNode.isReady && eNode.isReachable {
			assignableNodes = append(assignableNodes, *eNode)
		}
		for ip := range eNode.allocations {
			allAllocations[ip] = true
		}
	}
	sort.Slice(assignableNodes, func(i, j int) bool {
		return len(assignableNodes[i].allocations) < len(assignableNodes[j].allocations)
	})
	return assignableNodes, allAllocations
}

func (oc *Controller) setNodeEgressAssignable(nodeName string, isAssignable bool) {
	oc.eIPC.allocator.Lock()
	defer oc.eIPC.allocator.Unlock()
	if eNode, exists := oc.eIPC.allocator.cache[nodeName]; exists {
		eNode.isEgressAssignable = isAssignable
	}
}

func (oc *Controller) setNodeEgressReady(nodeName string, isReady bool) {
	oc.eIPC.allocator.Lock()
	defer oc.eIPC.allocator.Unlock()
	if eNode, exists := oc.eIPC.allocator.cache[nodeName]; exists {
		eNode.isReady = isReady
	}
}

func (oc *Controller) setNodeEgressReachable(nodeName string, isReachable bool) {
	oc.eIPC.allocator.Lock()
	defer oc.eIPC.allocator.Unlock()
	if eNode, exists := oc.eIPC.allocator.cache[nodeName]; exists {
		eNode.isReachable = isReachable
	}
}

func (oc *Controller) addEgressNode(egressNode *kapi.Node) error {
	klog.V(5).Infof("Egress node: %s about to be initialized", egressNode.Name)
	lsp := nbdb.LogicalSwitchPort{
		Name:    types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + egressNode.Name,
		Options: map[string]string{"nat-addresses": "router"},
	}
	opModel := libovsdbops.OperationModel{
		Model: &lsp,
		OnModelMutations: []interface{}{
			&lsp.Options,
		},
		ErrNotFound: true,
	}
	if _, err := oc.modelClient.CreateOrUpdate(opModel); err != nil {
		klog.Errorf("Unable to configure GARP on external logical switch port for egress node: %s, "+
			"this will result in packet drops during egress IP re-assignment,  err: %v", egressNode.Name, err)

	}
	oc.eIPC.assignmentRetryMutex.Lock()
	defer oc.eIPC.assignmentRetryMutex.Unlock()
	for eIPName := range oc.eIPC.assignmentRetry {
		klog.V(5).Infof("Re-assignment for EgressIP: %s attempted by new node: %s", eIPName, egressNode.Name)
		eIP, err := oc.kube.GetEgressIP(eIPName)
		if errors.IsNotFound(err) {
			klog.Errorf("Re-assignment for EgressIP: EgressIP: %s not found in the api-server, err: %v", eIPName, err)
			delete(oc.eIPC.assignmentRetry, eIP.Name)
			continue
		}
		if err != nil {
			klog.Errorf("Re-assignment for EgressIP: unable to retrieve EgressIP: %s from the api-server, err: %v", eIPName, err)
			continue
		}
		newEIP, err := oc.reassignEgressIP(eIP)
		if err != nil {
			klog.Errorf("Re-assignment for EgressIP: %s failed, err: %v", eIP.Name, err)
			continue
		}
		if len(newEIP.Spec.EgressIPs) == len(newEIP.Status.Items) {
			delete(oc.eIPC.assignmentRetry, eIP.Name)
		}
	}
	return nil
}

func (oc *Controller) deleteEgressNode(egressNode *kapi.Node) error {
	klog.V(5).Infof("Egress node: %s about to be removed", egressNode.Name)
	lsp := nbdb.LogicalSwitchPort{
		Name:    types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + egressNode.Name,
		Options: map[string]string{"nat-addresses": ""},
	}
	opModel := libovsdbops.OperationModel{
		Model: &lsp,
		OnModelMutations: []interface{}{
			&lsp.Options,
		},
		ErrNotFound: true,
	}
	if err := oc.modelClient.Delete(opModel); err != nil {
		klog.Errorf("Unable to remove GARP configuration on external logical switch port for egress node: %s, err: %v", egressNode.Name, err)
	}

	egressIPs, err := oc.kube.GetEgressIPs()
	if err != nil {
		return fmt.Errorf("unable to list egressIPs, err: %v", err)
	}
	for _, eIP := range egressIPs.Items {
		needsReassignment := false
		for _, status := range eIP.Status.Items {
			if status.Node == egressNode.Name {
				needsReassignment = true
				break
			}
		}
		if needsReassignment {
			if _, err := oc.reassignEgressIP(&eIP); err != nil {
				klog.Errorf("EgressIP: %s re-assignmnent error: %v", eIP.Name, err)
			}
		}
	}
	return nil
}

func (oc *Controller) reassignEgressIP(eIP *egressipv1.EgressIP) (*egressipv1.EgressIP, error) {
	klog.V(5).Infof("EgressIP: %s about to be re-assigned", eIP.Name)
	if err := oc.deleteEgressIP(eIP); err != nil {
		return nil, fmt.Errorf("old egress IP deletion failed, err: %v", err)
	}
	eIP = eIP.DeepCopy()
	eIP.Status = egressipv1.EgressIPStatus{
		Items: []egressipv1.EgressIPStatusItem{},
	}
	var reassignError error
	if err := oc.addEgressIP(eIP); err != nil {
		reassignError = fmt.Errorf("new egress IP assignment failed, err: %v", err)
	}
	if err := oc.updateEgressIPWithRetry(eIP); err != nil {
		return nil, fmt.Errorf("update of new egress IP failed, err: %v", err)
	}
	return eIP, reassignError
}

func (oc *Controller) initEgressIPAllocator(node *kapi.Node) (err error) {
	oc.eIPC.allocator.Lock()
	defer oc.eIPC.allocator.Unlock()
	if _, exists := oc.eIPC.allocator.cache[node.Name]; !exists {
		var v4IP, v6IP net.IP
		var v4Subnet, v6Subnet *net.IPNet
		v4IfAddr, v6IfAddr, err := util.ParseNodePrimaryIfAddr(node)
		if err != nil {
			klog.V(5).Infof("Unable to use node for egress assignment, err: %v", err)
			return nil
		}
		if v4IfAddr != "" {
			v4IP, v4Subnet, err = net.ParseCIDR(v4IfAddr)
			if err != nil {
				return err
			}
		}
		if v6IfAddr != "" {
			v6IP, v6Subnet, err = net.ParseCIDR(v6IfAddr)
			if err != nil {
				return err
			}
		}

		nodeSubnets, err := util.ParseNodeHostSubnetAnnotation(node)
		if err != nil {
			return fmt.Errorf("failed to parse node %s subnets annotation %v", node.Name, err)
		}
		mgmtIPs := make([]net.IP, len(nodeSubnets))
		for i, subnet := range nodeSubnets {
			mgmtIPs[i] = util.GetNodeManagementIfAddr(subnet).IP
		}

		oc.eIPC.allocator.cache[node.Name] = &egressNode{
			name:        node.Name,
			v4IP:        v4IP,
			v6IP:        v6IP,
			v4Subnet:    v4Subnet,
			v6Subnet:    v6Subnet,
			mgmtIPs:     mgmtIPs,
			allocations: make(map[string]bool),
		}
	}
	return nil
}

func (oc *Controller) addNodeForEgress(node *v1.Node) error {
	v4Addr, v6Addr := getNodeInternalAddrs(node)
	v4ClusterSubnet, v6ClusterSubnet := getClusterSubnets()
	if err := oc.createDefaultNoRerouteNodePolicies(v4Addr, v6Addr, v4ClusterSubnet, v6ClusterSubnet); err != nil {
		return err
	}
	if err := oc.initEgressIPAllocator(node); err != nil {
		return fmt.Errorf("egress node initialization error: %v", err)
	}
	return nil
}

func (oc *Controller) deleteNodeForEgress(node *v1.Node) error {
	v4Addr, v6Addr := getNodeInternalAddrs(node)
	v4ClusterSubnet, v6ClusterSubnet := getClusterSubnets()
	if err := oc.deleteDefaultNoRerouteNodePolicies(v4Addr, v6Addr, v4ClusterSubnet, v6ClusterSubnet); err != nil {
		return err
	}
	oc.eIPC.allocator.Lock()
	delete(oc.eIPC.allocator.cache, node.Name)
	oc.eIPC.allocator.Unlock()
	return nil
}

func (oc *Controller) initClusterEgressPolicies(nodes []interface{}) {
	v4ClusterSubnet, v6ClusterSubnet := getClusterSubnets()
	oc.createDefaultNoReroutePodPolicies(v4ClusterSubnet, v6ClusterSubnet)
	oc.createDefaultNoRerouteServicePolicies(v4ClusterSubnet, v6ClusterSubnet)
	go oc.checkEgressNodesReachability()
}

// egressNode is a cache helper used for egress IP assignment, representing an egress node
type egressNode struct {
	v4IP               net.IP
	v6IP               net.IP
	v4Subnet           *net.IPNet
	v6Subnet           *net.IPNet
	mgmtIPs            []net.IP
	allocations        map[string]bool
	isReady            bool
	isReachable        bool
	isEgressAssignable bool
	tainted            bool
	name               string
}

type allocator struct {
	*sync.Mutex
	// A cache used for egress IP assignments containing data for all cluster nodes
	// used for egress IP assignments
	cache map[string]*egressNode
}

type egressIPController struct {
	// Cache used for retrying pods which did not have an IP address when we processed the EgressIP object
	podRetry sync.Map

	// Cache of gateway join router IPs, usefull since these should not change often
	gatewayIPCache sync.Map

	// Mutex used for syncing the map retrying EgressIP objects
	assignmentRetryMutex *sync.Mutex

	// Cache used for retrying EgressIP objects which were created before any node existed.
	assignmentRetry map[string]bool

	// Mutex used for syncing the egressIP namespace handlers
	namespaceHandlerMutex *sync.Mutex

	// Cache used for keeping track of EgressIP namespace handlers
	namespaceHandlerCache map[string]*factory.Handler

	// Mutex used for syncing the egressIP pod handlers
	podHandlerMutex *sync.Mutex

	// Cache used for keeping track of EgressIP pod handlers
	podHandlerCache map[string]*factory.Handler

	allocator allocator

	// libovsdb northbound client interface
	nbClient libovsdbclient.Client

	// modelClient for performing idempotent NB operations
	modelClient libovsdbops.ModelClient

	// watchFactory watching k8s objects
	watchFactory *factory.WatchFactory
}

func (e *egressIPController) addPodEgressIP(eIP *egressipv1.EgressIP, pod *kapi.Pod) error {
	if pod.Spec.HostNetwork {
		return nil
	}
	podIPs, err := e.getPodIPs(pod)
	if err != nil || len(podIPs) == 0 {
		e.podRetry.Store(getPodKey(pod), true)
		return nil
	}
	if e.needsRetry(pod) {
		e.podRetry.Delete(getPodKey(pod))
	}
	if err := e.handleEgressReroutePolicy(podIPs, eIP.Status.Items, eIP.Name, e.createEgressReroutePolicy); err != nil {
		return fmt.Errorf("unable to create logical router policy, err: %v", err)
	}

	if config.Gateway.DisableSNATMultipleGWs {
		// remove snats to->nodeIP (from the node where pod exists) for these podIPs before adding the snat to->egressIP
		err = deletePerPodGRSNAT(e.nbClient, pod.Spec.NodeName, podIPs)
		if err != nil {
			return err
		}
	}

	var ops []ovsdb.Operation
	for _, status := range eIP.Status.Items {
		if ops, err = createNATRuleOps(e.nbClient, ops, podIPs, status, eIP.Name); err != nil {
			return fmt.Errorf("unable to create NAT rule for status: %v, err: %v", status, err)
		}
	}
	_, err = libovsdbops.TransactAndCheck(e.nbClient, ops)
	return err
}

func (e *egressIPController) deletePodEgressIP(eIP *egressipv1.EgressIP, pod *kapi.Pod) error {
	if pod.Spec.HostNetwork {
		return nil
	}
	podIPs, err := e.getPodIPs(pod)
	if err != nil {
		return fmt.Errorf("unable to retrieve pod IPs, err: %v", err)
	}
	if len(podIPs) == 0 {
		return fmt.Errorf("unable to retrieve pod IPs, err: no pod IPs defined")
	}
	if err := e.handleEgressReroutePolicy(podIPs, eIP.Status.Items, eIP.Name, e.deleteEgressReroutePolicy); err != nil {
		return fmt.Errorf("unable to delete logical router policy, err: %v", err)
	}

	var ops []ovsdb.Operation
	for _, status := range eIP.Status.Items {
		if ops, err = deleteNATRuleOps(e.nbClient, ops, podIPs, status, eIP.Name); err != nil {
			return fmt.Errorf("unable to delete NAT rule for status: %v, err: %v", status, err)
		}
	}
	_, err = libovsdbops.TransactAndCheck(e.nbClient, ops)
	if err != nil {
		return err
	}

	if config.Gateway.DisableSNATMultipleGWs {
		// add snats to->nodeIP (on the node where the pod exists) for these podIPs after deleting the snat to->egressIP
		err = addPerPodGRSNAT(e.nbClient, e.watchFactory, pod, podIPs)
		if err != nil {
			return err
		}
	}

	return nil
}

func (e *egressIPController) getGatewayRouterJoinIP(node string, wantsIPv6 bool) (net.IP, error) {
	var gatewayIPs []*net.IPNet
	if item, exists := e.gatewayIPCache.Load(node); exists {
		var ok bool
		if gatewayIPs, ok = item.([]*net.IPNet); !ok {
			return nil, fmt.Errorf("unable to cast node: %s gatewayIP cache item to correct type", node)
		}
	} else {
		err := utilwait.ExponentialBackoff(retry.DefaultRetry, func() (bool, error) {
			var err error
			gatewayIPs, err = util.GetLRPAddrs(e.nbClient, types.GWRouterToJoinSwitchPrefix+types.GWRouterPrefix+node)
			if err != nil {
				klog.Errorf("Attempt at finding node gateway router network information failed, err: %v", err)
			}
			return err == nil, nil
		})
		if err != nil {
			return nil, err
		}
		e.gatewayIPCache.Store(node, gatewayIPs)
	}

	if gatewayIP, err := util.MatchIPNetFamily(wantsIPv6, gatewayIPs); gatewayIP != nil {
		return gatewayIP.IP, nil
	} else {
		return nil, fmt.Errorf("could not find node %s gateway router: %v", node, err)
	}
}

func (e *egressIPController) getPodIPs(pod *kapi.Pod) ([]*net.IPNet, error) {
	podAnnotation, err := util.UnmarshalPodAnnotation(pod.Annotations)
	if err != nil {
		return nil, err
	}
	return podAnnotation.IPs, nil
}

func (e *egressIPController) needsRetry(pod *kapi.Pod) bool {
	_, retry := e.podRetry.Load(getPodKey(pod))
	return retry
}

// createEgressReroutePolicy uses logical router policies to force egress traffic to the egress node, for that we need
// to retrive the internal gateway router IP attached to the egress node. This method handles both the shared and
// local gateway mode case
func (e *egressIPController) handleEgressReroutePolicy(podIPNets []*net.IPNet, statuses []egressipv1.EgressIPStatusItem, egressIPName string, cb func(filterOption, egressIPName string, gatewayRouterIPs []string) error) error {
	gatewayRouterIPv4s, gatewayRouterIPv6s := []string{}, []string{}
	for _, status := range statuses {
		isEgressIPv6 := utilnet.IsIPv6String(status.EgressIP)
		gatewayRouterIP, err := e.getGatewayRouterJoinIP(status.Node, isEgressIPv6)
		if err != nil {
			klog.Errorf("Unable to retrieve gateway IP for node: %s, protocol is IPv6: %v, err: %v", status.Node, isEgressIPv6, err)
			continue
		}
		if isEgressIPv6 {
			gatewayRouterIPv6s = append(gatewayRouterIPv6s, gatewayRouterIP.String())
		} else {
			gatewayRouterIPv4s = append(gatewayRouterIPv4s, gatewayRouterIP.String())
		}
	}
	for _, podIPNet := range podIPNets {
		podIP := podIPNet.IP
		if utilnet.IsIPv6(podIP) {
			if len(gatewayRouterIPv6s) > 0 {
				filterOption := fmt.Sprintf("ip6.src == %s", podIP.String())
				if err := cb(filterOption, egressIPName, gatewayRouterIPv6s); err != nil {
					return err
				}
			} else {
				klog.Errorf("Egress IPv6 request cannot be handled for pod IP: %s, no IPv6 gateway router addresses are associated with the egress nodes", podIP)
			}
		} else if !utilnet.IsIPv6(podIP) {
			if len(gatewayRouterIPv4s) > 0 {
				filterOption := fmt.Sprintf("ip4.src == %s", podIP.String())
				if err := cb(filterOption, egressIPName, gatewayRouterIPv4s); err != nil {
					return err
				}
			} else {
				klog.Errorf("Egress IPv4 request cannot be handled for pod IP: %s, no IPv4 gateway router addresses are associated with the egress nodes", podIP)
			}
		}
	}
	return nil
}

func (e *egressIPController) createEgressReroutePolicy(filterOption, egressIPName string, gatewayRouterIPs []string) error {
	logicalRouter := nbdb.LogicalRouter{}
	logicalRouterPolicy := nbdb.LogicalRouterPolicy{
		Match:    filterOption,
		Priority: types.EgressIPReroutePriority,
		Nexthops: gatewayRouterIPs,
		Action:   nbdb.LogicalRouterPolicyActionReroute,
		ExternalIDs: map[string]string{
			"name": egressIPName,
		},
	}
	opsModel := []libovsdbops.OperationModel{
		{
			Model: &logicalRouterPolicy,
			ModelPredicate: func(lrp *nbdb.LogicalRouterPolicy) bool {
				return lrp.Match == filterOption && lrp.Priority == types.EgressIPReroutePriority && isStringSetEqual(lrp.Nexthops, gatewayRouterIPs) && lrp.ExternalIDs["name"] == egressIPName
			},
			DoAfter: func() {
				if logicalRouterPolicy.UUID != "" {
					logicalRouter.Policies = []string{logicalRouterPolicy.UUID}
				}
			},
		},
		{
			Model:          &logicalRouter,
			ModelPredicate: func(lr *nbdb.LogicalRouter) bool { return lr.Name == types.OVNClusterRouter },
			OnModelMutations: []interface{}{
				&logicalRouter.Policies,
			},
			ErrNotFound: true,
		},
	}
	if _, err := e.modelClient.CreateOrUpdate(opsModel...); err != nil {
		return fmt.Errorf("unable to create logical router policy, err: %v", err)
	}
	return nil
}

func (e *egressIPController) deleteEgressReroutePolicy(filterOption, egressIPName string, gatewayRouterIPs []string) error {
	logicalRouter := nbdb.LogicalRouter{}
	logicalRouterPolicyRes := []nbdb.LogicalRouterPolicy{}
	opsModel := []libovsdbops.OperationModel{
		{
			ModelPredicate: func(lrp *nbdb.LogicalRouterPolicy) bool {
				return lrp.Match == filterOption && lrp.Priority == types.EgressIPReroutePriority && isStringSetEqual(lrp.Nexthops, gatewayRouterIPs) && lrp.ExternalIDs["name"] == egressIPName
			},
			ExistingResult: &logicalRouterPolicyRes,
			DoAfter: func() {
				logicalRouter.Policies = libovsdbops.ExtractUUIDsFromModels(&logicalRouterPolicyRes)
			},
			BulkOp: true,
		},
		{
			Model:          &logicalRouter,
			ModelPredicate: func(lr *nbdb.LogicalRouter) bool { return lr.Name == types.OVNClusterRouter },
			OnModelMutations: []interface{}{
				&logicalRouter.Policies,
			},
		},
	}
	if err := e.modelClient.Delete(opsModel...); err != nil {
		return fmt.Errorf("unable to remove logical router policy, err: %v", err)
	}
	return nil
}

func (e *egressIPController) deleteLegacyEgressReroutePolicies() error {
	logicalRouter := nbdb.LogicalRouter{}
	logicalRouterPolicyRes := []nbdb.LogicalRouterPolicy{}
	opsModel := []libovsdbops.OperationModel{
		{
			ModelPredicate: func(lrp *nbdb.LogicalRouterPolicy) bool {
				return lrp.Priority == types.EgressIPReroutePriority && lrp.Nexthop != nil
			},
			ExistingResult: &logicalRouterPolicyRes,
			DoAfter: func() {
				logicalRouter.Policies = libovsdbops.ExtractUUIDsFromModels(&logicalRouterPolicyRes)
			},
			BulkOp: true,
		},
		{
			Model:          &logicalRouter,
			ModelPredicate: func(lr *nbdb.LogicalRouter) bool { return lr.Name == types.OVNClusterRouter },
			OnModelMutations: []interface{}{
				&logicalRouter.Policies,
			},
		},
	}
	if err := e.modelClient.Delete(opsModel...); err != nil {
		return fmt.Errorf("unable to remove legacy logical router policies, err: %v", err)
	}
	return nil
}

func (oc *Controller) checkEgressNodesReachability() {
	for {
		reAddOrDelete := map[string]bool{}
		oc.eIPC.allocator.Lock()
		for _, eNode := range oc.eIPC.allocator.cache {
			if eNode.isEgressAssignable && eNode.isReady {
				wasReachable := eNode.isReachable
				isReachable := oc.isReachable(eNode)
				if wasReachable && !isReachable {
					reAddOrDelete[eNode.name] = true
				} else if !wasReachable && isReachable {
					reAddOrDelete[eNode.name] = false
				}
				eNode.isReachable = isReachable
			}
		}
		oc.eIPC.allocator.Unlock()
		for nodeName, shouldDelete := range reAddOrDelete {
			node, err := oc.kube.GetNode(nodeName)
			if err != nil {
				klog.Errorf("Node: %s reachability changed, but could not retrieve node from API server, err: %v", node.Name, err)
			}
			if shouldDelete {
				klog.Warningf("Node: %s is detected as unreachable, deleting it from egress assignment", node.Name)
				if err := oc.deleteEgressNode(node); err != nil {
					klog.Errorf("Node: %s is detected as unreachable, but could not re-assign egress IPs, err: %v", node.Name, err)
				}
			} else {
				klog.Infof("Node: %s is detected as reachable and ready again, adding it to egress assignment", node.Name)
				if err := oc.addEgressNode(node); err != nil {
					klog.Errorf("Node: %s is detected as reachable and ready again, but could not re-assign egress IPs, err: %v", node.Name, err)
				}
			}
		}
		time.Sleep(5 * time.Second)
	}
}

func (oc *Controller) isReachable(node *egressNode) bool {
	for _, ip := range node.mgmtIPs {
		if dialer.dial(ip) {
			return true
		}
	}
	return false
}

type egressIPDial struct{}

// Blantant copy from: https://github.com/openshift/sdn/blob/master/pkg/network/common/egressip.go#L499-L505
// Ping a node and return whether or not we think it is online. We do this by trying to
// open a TCP connection to the "discard" service (port 9); if the node is offline, the
// attempt will either time out with no response, or else return "no route to host" (and
// we will return false). If the node is online then we presumably will get a "connection
// refused" error; but the code below assumes that anything other than timeout or "no
// route" indicates that the node is online.
func (e *egressIPDial) dial(ip net.IP) bool {
	timeout := time.Second
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(ip.String(), "9"), timeout)
	if conn != nil {
		conn.Close()
	}
	if opErr, ok := err.(*net.OpError); ok {
		if opErr.Timeout() {
			return false
		}
		if sysErr, ok := opErr.Err.(*os.SyscallError); ok && sysErr.Err == syscall.EHOSTUNREACH {
			return false
		}
	}
	return true
}

func getClusterSubnets() ([]*net.IPNet, []*net.IPNet) {
	var v4ClusterSubnets = []*net.IPNet{}
	var v6ClusterSubnets = []*net.IPNet{}
	for _, clusterSubnet := range config.Default.ClusterSubnets {
		if !utilnet.IsIPv6CIDR(clusterSubnet.CIDR) {
			v4ClusterSubnets = append(v4ClusterSubnets, clusterSubnet.CIDR)
		} else {
			v6ClusterSubnets = append(v6ClusterSubnets, clusterSubnet.CIDR)
		}
	}
	return v4ClusterSubnets, v6ClusterSubnets
}

func getNodeInternalAddrs(node *v1.Node) (net.IP, net.IP) {
	var v4Addr, v6Addr net.IP
	for _, nodeAddr := range node.Status.Addresses {
		if nodeAddr.Type == v1.NodeInternalIP {
			ip := net.ParseIP(nodeAddr.Address)
			if !utilnet.IsIPv6(ip) {
				v4Addr = ip
			} else {
				v6Addr = ip
			}
		}
	}
	return v4Addr, v6Addr
}

// createDefaultNoRerouteServicePolicies ensures service reachability from the
// host network to any service backed by egress IP matching pods
func (oc *Controller) createDefaultNoRerouteServicePolicies(v4ClusterSubnet, v6ClusterSubnet []*net.IPNet) {
	for _, v4Subnet := range v4ClusterSubnet {
		match := fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4Subnet.String(), config.Gateway.V4JoinSubnet)
		if err := oc.createLogicalRouterPolicy(match, types.DefaultNoRereoutePriority); err != nil {
			klog.Errorf("Unable to create IPv4 no-reroute service policies, err: %v", err)
		}
	}
	for _, v6Subnet := range v6ClusterSubnet {
		match := fmt.Sprintf("ip6.src == %s && ip6.dst == %s", v6Subnet.String(), config.Gateway.V6JoinSubnet)
		if err := oc.createLogicalRouterPolicy(match, types.DefaultNoRereoutePriority); err != nil {
			klog.Errorf("Unable to create IPv6 no-reroute service policies, err: %v", err)
		}
	}
}

// createDefaultNoReroutePodPolicies ensures egress pods east<->west traffic with regular pods,
// i.e: ensuring that an egress pod can still communicate with a regular pod / service backed by regular pods
func (oc *Controller) createDefaultNoReroutePodPolicies(v4ClusterSubnet, v6ClusterSubnet []*net.IPNet) {
	for _, v4Subnet := range v4ClusterSubnet {
		match := fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4Subnet.String(), v4Subnet.String())
		if err := oc.createLogicalRouterPolicy(match, types.DefaultNoRereoutePriority); err != nil {
			klog.Errorf("Unable to create IPv4 no-reroute pod policies, err: %v", err)
		}
	}
	for _, v6Subnet := range v6ClusterSubnet {
		match := fmt.Sprintf("ip6.src == %s && ip6.dst == %s", v6Subnet.String(), v6Subnet.String())
		if err := oc.createLogicalRouterPolicy(match, types.DefaultNoRereoutePriority); err != nil {
			klog.Errorf("Unable to create IPv6 no-reroute pod policies, err: %v", err)
		}
	}
}

// createDefaultNoRerouteNodePolicies ensures egress pods east<->west traffic with hostNetwork pods,
// i.e: ensuring that an egress pod can still communicate with a hostNetwork pod / service backed by hostNetwork pods
func (oc *Controller) createDefaultNoRerouteNodePolicies(v4NodeAddr, v6NodeAddr net.IP, v4ClusterSubnet, v6ClusterSubnet []*net.IPNet) error {
	if v4NodeAddr != nil {
		for _, v4Subnet := range v4ClusterSubnet {
			match := fmt.Sprintf("ip4.src == %s && ip4.dst == %s/32", v4Subnet.String(), v4NodeAddr.String())
			if err := oc.createLogicalRouterPolicy(match, types.DefaultNoRereoutePriority); err != nil {
				klog.Errorf("Unable to create IPv4 no-reroute node policies, err: %v", err)
			}
		}
	}
	if v6NodeAddr != nil {
		for _, v6Subnet := range v6ClusterSubnet {
			match := fmt.Sprintf("ip6.src == %s && ip6.dst == %s/128", v6Subnet.String(), v6NodeAddr.String())
			if err := oc.createLogicalRouterPolicy(match, types.DefaultNoRereoutePriority); err != nil {
				klog.Errorf("Unable to create IPv6 no-reroute node policies, err: %v", err)
			}
		}
	}
	return nil
}

func (oc *Controller) createLogicalRouterPolicy(match string, priority int) error {
	logicalRouter := nbdb.LogicalRouter{}
	logicalRouterPolicy := nbdb.LogicalRouterPolicy{
		Priority: priority,
		Action:   nbdb.LogicalRouterPolicyActionAllow,
		Match:    match,
	}
	opModels := []libovsdbops.OperationModel{
		{
			Model: &logicalRouterPolicy,
			ModelPredicate: func(lrp *nbdb.LogicalRouterPolicy) bool {
				return lrp.Match == match && lrp.Priority == priority
			},
			DoAfter: func() {
				if logicalRouterPolicy.UUID != "" {
					logicalRouter.Policies = []string{logicalRouterPolicy.UUID}
				}
			},
		},
		{
			Model:          &logicalRouter,
			ModelPredicate: func(lr *nbdb.LogicalRouter) bool { return lr.Name == types.OVNClusterRouter },
			OnModelMutations: []interface{}{
				&logicalRouter.Policies,
			},
			ErrNotFound: true,
		},
	}
	if _, err := oc.modelClient.CreateOrUpdate(opModels...); err != nil {
		return fmt.Errorf("unable to create logical router policy, err: %v", err)
	}
	return nil
}

func (oc *Controller) deleteLogicalRouterPolicy(match string, priority int) error {
	logicalRouter := nbdb.LogicalRouter{}
	logicalRouterPolicyRes := []nbdb.LogicalRouterPolicy{}
	opModels := []libovsdbops.OperationModel{
		{
			ModelPredicate: func(lrp *nbdb.LogicalRouterPolicy) bool {
				return lrp.Match == match && lrp.Priority == priority
			},
			ExistingResult: &logicalRouterPolicyRes,
			DoAfter: func() {
				logicalRouter.Policies = libovsdbops.ExtractUUIDsFromModels(&logicalRouterPolicyRes)
			},
			BulkOp: true,
		},
		{
			Model:          &logicalRouter,
			ModelPredicate: func(lr *nbdb.LogicalRouter) bool { return lr.Name == types.OVNClusterRouter },
			OnModelMutations: []interface{}{
				&logicalRouter.Policies,
			},
		},
	}

	if err := oc.modelClient.Delete(opModels...); err != nil {
		return fmt.Errorf("unable to delete logical router policy, err: %v", err)
	}
	return nil
}

func (oc *Controller) deleteDefaultNoRerouteNodePolicies(v4NodeAddr, v6NodeAddr net.IP, v4ClusterSubnet, v6ClusterSubnet []*net.IPNet) error {
	if v4NodeAddr != nil {
		for _, v4Subnet := range v4ClusterSubnet {
			match := fmt.Sprintf("ip4.src == %s && ip4.dst == %s/32", v4Subnet.String(), v4NodeAddr.String())
			if err := oc.deleteLogicalRouterPolicy(match, types.DefaultNoRereoutePriority); err != nil {
				return fmt.Errorf("unable to delete IPv4 no-reroute node policies, err: %v", err)
			}
		}
	}
	if v6NodeAddr != nil {
		for _, v6Subnet := range v6ClusterSubnet {
			match := fmt.Sprintf("ip6.src == %s && ip6.dst == %s/128", v6Subnet.String(), v6NodeAddr.String())
			if err := oc.deleteLogicalRouterPolicy(match, types.DefaultNoRereoutePriority); err != nil {
				return fmt.Errorf("unable to delete IPv6 no-reroute node policies, err: %v", err)
			}
		}
	}
	return nil
}

func buildSNATFromEgressIPStatus(podIP net.IP, status egressipv1.EgressIPStatusItem, egressIPName string) (*nbdb.NAT, error) {
	podIPStr := podIP.String()
	mask := GetIPFullMask(podIPStr)
	_, logicalIP, err := net.ParseCIDR(podIPStr + mask)
	if err != nil {
		return nil, fmt.Errorf("failed to parse podIP: %s, error: %v", podIP.String(), err)
	}
	externalIP := net.ParseIP(status.EgressIP)
	logicalPort := types.K8sPrefix + status.Node
	externalIds := map[string]string{"name": egressIPName}
	nat := libovsdbops.BuildRouterSNAT(&externalIP, logicalIP, logicalPort, externalIds)
	return nat, nil
}

func createNATRuleOps(nbClient libovsdbclient.Client, ops []ovsdb.Operation, podIPs []*net.IPNet, status egressipv1.EgressIPStatusItem, egressIPName string) ([]ovsdb.Operation, error) {
	nats := make([]*nbdb.NAT, 0, len(podIPs))
	var nat *nbdb.NAT
	var err error
	for _, podIP := range podIPs {
		if (utilnet.IsIPv6String(status.EgressIP) && utilnet.IsIPv6(podIP.IP)) || (!utilnet.IsIPv6String(status.EgressIP) && !utilnet.IsIPv6(podIP.IP)) {
			nat, err = buildSNATFromEgressIPStatus(podIP.IP, status, egressIPName)
			if err != nil {
				return nil, err
			}
			nats = append(nats, nat)
		}
	}
	router := &nbdb.LogicalRouter{
		Name: util.GetGatewayRouterFromNode(status.Node),
	}
	ops, err = libovsdbops.AddOrUpdateNatsToRouterOps(nbClient, ops, router, nats...)
	if err != nil {
		return nil, fmt.Errorf("unable to create snat rules, for router: %s, error: %v", router.Name, err)
	}
	return ops, nil
}

func deleteNATRuleOps(nbClient libovsdbclient.Client, ops []ovsdb.Operation, podIPs []*net.IPNet, status egressipv1.EgressIPStatusItem, egressIPName string) ([]ovsdb.Operation, error) {
	nats := make([]*nbdb.NAT, 0, len(podIPs))
	var nat *nbdb.NAT
	var err error
	for _, podIP := range podIPs {
		if (utilnet.IsIPv6String(status.EgressIP) && utilnet.IsIPv6(podIP.IP)) || (!utilnet.IsIPv6String(status.EgressIP) && !utilnet.IsIPv6(podIP.IP)) {
			nat, err = buildSNATFromEgressIPStatus(podIP.IP, status, egressIPName)
			if err != nil {
				return nil, err
			}
			nats = append(nats, nat)
		}
	}
	router := &nbdb.LogicalRouter{
		Name: util.GetGatewayRouterFromNode(status.Node),
	}
	ops, err = libovsdbops.DeleteNatsFromRouterOps(nbClient, ops, router, nats...)
	if err != nil {
		return nil, fmt.Errorf("unable to remove snat rules for router: %s, error: %v", router.Name, err)
	}
	return ops, nil
}

func getEgressIPKey(eIP *egressipv1.EgressIP) string {
	return eIP.Name
}

func getNamespaceKey(namespace *kapi.Namespace) string {
	return namespace.Name
}

func getPodKey(pod *kapi.Pod) string {
	return fmt.Sprintf("%s_%s", pod.Namespace, pod.Name)
}

func getEgressIPAllocationTotalCount(allocator allocator) float64 {
	count := 0
	allocator.Lock()
	defer allocator.Unlock()
	for _, eNode := range allocator.cache {
		count += len(eNode.allocations)
	}
	return float64(count)
}

// reflect.DeepEqual considers the order of elements (plus possible duplication)
// given that neither order nor duplication of nexthop IPs matters, simply comparing sets
// is the better option and will avoid issues with different order within the compared
// slices
func isStringSetEqual(x, y []string) bool {
	s1 := sets.NewString(x...)
	s2 := sets.NewString(y...)
	return s1.Equal(s2)
}
