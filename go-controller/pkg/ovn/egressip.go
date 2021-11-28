package ovn

import (
	"errors"
	"fmt"
	"net"
	"os"
	"reflect"
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
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	kapi "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	utilwait "k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
)

type egressIPDialer interface {
	dial(ip net.IP) bool
}

var dialer egressIPDialer = &egressIPDial{}

func (oc *Controller) reconcileEgressIP(old, new *egressipv1.EgressIP) (err error) {
	oc.eIPC.egressIPAssignmentMutex.Lock()
	defer oc.eIPC.egressIPAssignmentMutex.Unlock()

	oldEIP, newEIP := &egressipv1.EgressIP{}, &egressipv1.EgressIP{}

	newNamespaceSelector, _ := metav1.LabelSelectorAsSelector(nil)
	oldNamespaceSelector, _ := metav1.LabelSelectorAsSelector(nil)
	if old != nil {
		oldEIP = old
		oldNamespaceSelector, err = metav1.LabelSelectorAsSelector(&oldEIP.Spec.NamespaceSelector)
		if err != nil {
			return fmt.Errorf("invalid old namespaceSelector, err: %v", err)
		}
	}
	if new != nil {
		newEIP = new
		newNamespaceSelector, err = metav1.LabelSelectorAsSelector(&newEIP.Spec.NamespaceSelector)
		if err != nil {
			return fmt.Errorf("invalid new namespaceSelector, err: %v", err)
		}
	}

	newPodSelector, err := metav1.LabelSelectorAsSelector(&newEIP.Spec.PodSelector)
	if err != nil {
		return fmt.Errorf("invalid new podSelector, err: %v", err)
	}
	oldPodSelector, err := metav1.LabelSelectorAsSelector(&oldEIP.Spec.PodSelector)
	if err != nil {
		return fmt.Errorf("invalid old podSelector, err: %v", err)
	}

	validSpecIPs, err := oc.validateEgressIPSpec(newEIP.Name, newEIP.Spec.EgressIPs)
	if err != nil {
		return fmt.Errorf("invalid EgressIP spec, err: %v", err)
	}

	validStatuses, statusToRemove := oc.validateEgressIPStatus(newEIP.Name, newEIP.Status.Items)
	statusToKeep := []egressipv1.EgressIPStatusItem{}
	statusToAdd := []egressipv1.EgressIPStatusItem{}
	validStatusIPs := sets.NewString()
	for _, validStatus := range validStatuses {
		validStatusIPs.Insert(validStatus.EgressIP)
		if !validSpecIPs.Has(validStatus.EgressIP) {
			statusToRemove = append(statusToRemove, validStatus)
		} else {
			statusToKeep = append(statusToKeep, validStatus)
		}
	}
	for _, oldStatus := range oldEIP.Status.Items {
		if !validStatusIPs.Has(oldStatus.EgressIP) {
			statusToRemove = append(statusToRemove, oldStatus)
		}
	}

	ipsToAdd := validSpecIPs.Difference(validStatusIPs)
	if len(statusToRemove) > 0 {
		if err := oc.deleteEgressIPAssignments(oldEIP.Name, statusToRemove, oldEIP.Spec.NamespaceSelector, oldEIP.Spec.PodSelector); err != nil {
			return err
		}
	}
	if len(ipsToAdd) > 0 {
		statusToAdd = oc.assignEgressIPs(newEIP.Name, ipsToAdd.List())
		if err := oc.addEgressIPAssignments(newEIP.Name, statusToAdd, newEIP.Spec.NamespaceSelector, newEIP.Spec.PodSelector); err != nil {
			return err
		}
		statusToKeep = append(statusToKeep, statusToAdd...)
	}
	if len(statusToAdd) > 0 || (len(statusToRemove) > 0 && new != nil) {
		if err := oc.updateEgressIPStatus(newEIP.Name, statusToKeep); err != nil {
			return err
		}
	}
	metrics.RecordEgressIPCount(getEgressIPAllocationTotalCount(oc.eIPC.allocator))

	if len(ipsToAdd) == 0 &&
		len(statusToRemove) == 0 {
		if !reflect.DeepEqual(newNamespaceSelector, oldNamespaceSelector) && reflect.DeepEqual(newPodSelector, oldPodSelector) {
			namespaces, err := oc.watchFactory.GetNamespaces()
			if err != nil {
				return err
			}
			for _, namespace := range namespaces {
				namespaceLabels := labels.Set(namespace.Labels)
				if !newNamespaceSelector.Matches(namespaceLabels) && oldNamespaceSelector.Matches(namespaceLabels) {
					if err := oc.deleteNamespaceEgressIPAssignment(oldEIP.Name, oldEIP.Status.Items, namespace, oldEIP.Spec.PodSelector); err != nil {
						return err
					}
				}
				if newNamespaceSelector.Matches(namespaceLabels) && !oldNamespaceSelector.Matches(namespaceLabels) {
					if err := oc.addNamespaceEgressIPAssignments(newEIP.Name, newEIP.Status.Items, namespace, newEIP.Spec.PodSelector); err != nil {
						return err
					}
				}
			}
		} else if reflect.DeepEqual(newNamespaceSelector, oldNamespaceSelector) && !reflect.DeepEqual(newPodSelector, oldPodSelector) {
			namespaces, err := oc.watchFactory.GetNamespacesBySelector(newEIP.Spec.NamespaceSelector)
			if err != nil {
				return err
			}
			for _, namespace := range namespaces {
				pods, err := oc.watchFactory.GetPods(namespace.Name)
				if err != nil {
					return err
				}
				for _, pod := range pods {
					podLabels := labels.Set(pod.Labels)
					if !newPodSelector.Matches(podLabels) && oldPodSelector.Matches(podLabels) {
						if err := oc.deletePodEgressIPAssignments(oldEIP.Name, oldEIP.Status.Items, pod); err != nil {
							return err
						}
					}
					if newPodSelector.Matches(podLabels) && !oldPodSelector.Matches(podLabels) {
						if err := oc.addPodEgressIPAssignments(newEIP.Name, newEIP.Status.Items, pod); err != nil {
							return err
						}
					}
				}
			}
		} else if !reflect.DeepEqual(newNamespaceSelector, oldNamespaceSelector) && !reflect.DeepEqual(newPodSelector, oldPodSelector) {
			namespaces, err := oc.watchFactory.GetNamespaces()
			if err != nil {
				return err
			}
			for _, namespace := range namespaces {
				namespaceLabels := labels.Set(namespace.Labels)
				if !newNamespaceSelector.Matches(namespaceLabels) && oldNamespaceSelector.Matches(namespaceLabels) {
					if err := oc.deleteNamespaceEgressIPAssignment(oldEIP.Name, oldEIP.Status.Items, namespace, oldEIP.Spec.PodSelector); err != nil {
						return err
					}
				}
				if newNamespaceSelector.Matches(namespaceLabels) && !oldNamespaceSelector.Matches(namespaceLabels) {
					pods, err := oc.watchFactory.GetPods(namespace.Name)
					if err != nil {
						return err
					}
					for _, pod := range pods {
						podLabels := labels.Set(pod.Labels)
						if newPodSelector.Matches(podLabels) {
							if err := oc.addPodEgressIPAssignments(newEIP.Name, newEIP.Status.Items, pod); err != nil {
								return err
							}
						}
					}
				}
				if newNamespaceSelector.Matches(namespaceLabels) && oldNamespaceSelector.Matches(namespaceLabels) {
					pods, err := oc.watchFactory.GetPods(namespace.Name)
					if err != nil {
						return err
					}
					for _, pod := range pods {
						podLabels := labels.Set(pod.Labels)
						if !newPodSelector.Matches(podLabels) && oldPodSelector.Matches(podLabels) {
							if err := oc.deletePodEgressIPAssignments(oldEIP.Name, oldEIP.Status.Items, pod); err != nil {
								return err
							}
						}
						if newPodSelector.Matches(podLabels) && !oldPodSelector.Matches(podLabels) {
							if err := oc.addPodEgressIPAssignments(newEIP.Name, newEIP.Status.Items, pod); err != nil {
								return err
							}
						}
					}
				}
			}
		}
	}
	return nil
}

func (oc *Controller) reconcileEgressIPNamespace(old, new *v1.Namespace) error {
	oldNamespace, newNamespace := &v1.Namespace{}, &v1.Namespace{}
	if old != nil {
		oldNamespace = old
	}
	if new != nil {
		newNamespace = new
	}

	oldLabels := labels.Set(oldNamespace.Labels)
	newLabels := labels.Set(newNamespace.Labels)
	if reflect.DeepEqual(newLabels.AsSelector(), oldLabels.AsSelector()) {
		return nil
	}

	egressIPs, err := oc.watchFactory.GetEgressIPs()
	if err != nil {
		return err
	}
	for _, egressIP := range egressIPs {
		namespaceSelector, _ := metav1.LabelSelectorAsSelector(&egressIP.Spec.NamespaceSelector)
		if namespaceSelector.Matches(oldLabels) && !namespaceSelector.Matches(newLabels) {
			if err := oc.deleteNamespaceEgressIPAssignment(egressIP.Name, egressIP.Status.Items, oldNamespace, egressIP.Spec.PodSelector); err != nil {
				return err
			}
		}
		if !namespaceSelector.Matches(oldLabels) && namespaceSelector.Matches(newLabels) {
			if err := oc.addNamespaceEgressIPAssignments(egressIP.Name, egressIP.Status.Items, newNamespace, egressIP.Spec.PodSelector); err != nil {
				return err
			}
		}
	}
	return nil
}

func (oc *Controller) reconcileEgressIPPod(old, new *v1.Pod) error {
	oldPod, newPod := &v1.Pod{}, &v1.Pod{}
	if old != nil {
		oldPod = old
	}
	if new != nil {
		newPod = new
	}
	newPodLabels := labels.Set(newPod.Labels)
	oldPodLabels := labels.Set(oldPod.Labels)
	namespace, err := oc.watchFactory.GetNamespace(newPod.Namespace)
	if err != nil {
		return err
	}
	namespaceLabels := labels.Set(namespace.Labels)
	if namespaceLabels.AsSelector().Empty() {
		return nil
	}

	egressIPs, err := oc.watchFactory.GetEgressIPs()
	if err != nil {
		return err
	}
	for _, egressIP := range egressIPs {
		namespaceSelector, _ := metav1.LabelSelectorAsSelector(&egressIP.Spec.NamespaceSelector)
		if namespaceSelector.Matches(namespaceLabels) {
			podSelector, _ := metav1.LabelSelectorAsSelector(&egressIP.Spec.PodSelector)
			if !podSelector.Empty() && !podSelector.Matches(newPodLabels) && podSelector.Matches(oldPodLabels) {
				return oc.deletePodEgressIPAssignments(egressIP.Name, egressIP.Status.Items, oldPod)
			}
			if podSelector.Empty() && new == nil {
				return oc.deletePodEgressIPAssignments(egressIP.Name, egressIP.Status.Items, oldPod)
			}
			return oc.addPodEgressIPAssignments(egressIP.Name, egressIP.Status.Items, newPod)
		}
	}
	return nil
}

func (oc *Controller) validateEgressIPSpec(name string, egressIPs []string) (sets.String, error) {
	validatedEgressIPs := sets.NewString()
	for _, egressIP := range egressIPs {
		ip := net.ParseIP(egressIP)
		if ip == nil {
			eIPRef := kapi.ObjectReference{
				Kind: "EgressIP",
				Name: name,
			}
			oc.recorder.Eventf(&eIPRef, kapi.EventTypeWarning, "InvalidEgressIP", "egress IP: %s for object EgressIP: %s is not a valid IP address", egressIP, name)
			return nil, fmt.Errorf("unable to parse provided EgressIP: %s, invalid", egressIP)
		}
		validatedEgressIPs.Insert(ip.String())
	}
	return validatedEgressIPs, nil
}

func (oc *Controller) validateEgressIPStatus(name string, items []egressipv1.EgressIPStatusItem) ([]egressipv1.EgressIPStatusItem, []egressipv1.EgressIPStatusItem) {
	oc.eIPC.allocator.Lock()
	defer oc.eIPC.allocator.Unlock()
	var valid, invalid []egressipv1.EgressIPStatusItem
	for _, eIPStatus := range items {
		validAssignment := true
		eNode, exists := oc.eIPC.allocator.cache[eIPStatus.Node]
		if !exists {
			klog.Errorf("Allocator error: EgressIP: %s claims to have an allocation on a node which is unassignable for egress IP: %s", name, eIPStatus.Node)
			validAssignment = false
		} else {
			if eNode.getAllocationCountForEgressIP(name) > 1 {
				klog.Errorf("Allocator error: EgressIP: %s claims multiple egress IPs on same node: %s, will attempt rebalancing", name, eIPStatus.Node)
				validAssignment = false
			}
			if !eNode.isEgressAssignable {
				klog.Errorf("Allocator error: EgressIP: %s assigned to node: %s which does not have egress label, will attempt rebalancing", name, eIPStatus.Node)
				validAssignment = false
			}
			if !eNode.isReachable {
				klog.Errorf("Allocator error: EgressIP: %s assigned to node: %s which is not reachable, will attempt rebalancing", name, eIPStatus.Node)
				validAssignment = false
			}
			if !eNode.isReady {
				klog.Errorf("Allocator error: EgressIP: %s assigned to node: %s which is not ready, will attempt rebalancing", name, eIPStatus.Node)
				validAssignment = false
			}
			ip := net.ParseIP(eIPStatus.EgressIP)
			if ip == nil {
				klog.Errorf("Allocator error: EgressIP allocation contains unparsable IP address: %s", eIPStatus.EgressIP)
				validAssignment = false
			}
			if node := oc.isAnyClusterNodeIP(ip); node != nil {
				klog.Errorf("Allocator error: EgressIP allocation: %s is the IP of node: %s ", ip.String(), node.name)
				validAssignment = false
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
				validAssignment = false
			}
		}
		if validAssignment {
			valid = append(valid, eIPStatus)
		} else {
			invalid = append(invalid, eIPStatus)
		}
	}
	return valid, invalid
}

// addAllocatorEgressIPAssignments adds the allocation to the cache, so that
// they are tracked during the life-cycle of ovnkube-master
func (oc *Controller) addAllocatorEgressIPAssignments(name string, statusAssignments []egressipv1.EgressIPStatusItem) {
	oc.eIPC.allocator.Lock()
	defer oc.eIPC.allocator.Unlock()
	for _, status := range statusAssignments {
		if eNode, exists := oc.eIPC.allocator.cache[status.Node]; exists {
			eNode.allocations[status.EgressIP] = name
		}
	}
}

func (oc *Controller) addEgressIPAssignments(name string, statusAssignments []egressipv1.EgressIPStatusItem, namespaceSelector, podSelector metav1.LabelSelector) error {
	oc.addAllocatorEgressIPAssignments(name, statusAssignments)
	namespaces, err := oc.watchFactory.GetNamespacesBySelector(namespaceSelector)
	if err != nil {
		return err
	}
	for _, namespace := range namespaces {
		if err := oc.addNamespaceEgressIPAssignments(name, statusAssignments, namespace, podSelector); err != nil {
			return err
		}
	}
	return nil
}

func (oc *Controller) addNamespaceEgressIPAssignments(name string, statusAssignments []egressipv1.EgressIPStatusItem, namespace *kapi.Namespace, podSelector metav1.LabelSelector) error {
	var pods []*kapi.Pod
	var err error
	selector, _ := metav1.LabelSelectorAsSelector(&podSelector)
	if !selector.Empty() {
		pods, err = oc.watchFactory.GetPodsBySelector(namespace.Name, podSelector)
		if err != nil {
			return err
		}
	} else {
		pods, err = oc.watchFactory.GetPods(namespace.Name)
		if err != nil {
			return err
		}
	}
	for _, pod := range pods {
		if err := oc.addPodEgressIPAssignments(name, statusAssignments, pod); err != nil {
			return err
		}
	}
	return nil
}

func (oc *Controller) addPodEgressIPAssignments(name string, statusAssignments []egressipv1.EgressIPStatusItem, pod *kapi.Pod) error {
	oc.eIPC.podAssignmentMutex.Lock()
	defer oc.eIPC.podAssignmentMutex.Unlock()
	var remainingAssignments, successfulAssignments, existingAssignments []egressipv1.EgressIPStatusItem
	existing, exists := oc.eIPC.podAssignment[getPodKey(pod)]
	if !exists {
		remainingAssignments = statusAssignments
	} else {
		for _, status := range statusAssignments {
			for _, existingAssignment := range existing {
				if status != existingAssignment {
					remainingAssignments = append(remainingAssignments, status)
				}
			}
		}
	}
	for _, status := range remainingAssignments {
		klog.V(5).Infof("Adding pod egress IP status: %v for EgressIP: %s and pod: %s/%s", status, name, pod.Name, pod.Namespace)
		if err := oc.eIPC.addPodEgressIPAssignment(name, status, pod); err != nil && !errors.Is(err, skippedPodError) {
			return err
		} else if err == nil {
			successfulAssignments = append(successfulAssignments, status)
		}
	}
	if len(successfulAssignments) > 0 {
		existingAssignments = append(existingAssignments, successfulAssignments...)
		oc.eIPC.podAssignment[getPodKey(pod)] = existingAssignments
	}
	return nil
}

func (oc *Controller) deleteAllocatorEgressIPAssignments(statusAssignments []egressipv1.EgressIPStatusItem) {
	oc.eIPC.allocator.Lock()
	defer oc.eIPC.allocator.Unlock()
	for _, status := range statusAssignments {
		if eNode, exists := oc.eIPC.allocator.cache[status.Node]; exists {
			delete(eNode.allocations, status.EgressIP)
		}
	}
}

func (oc *Controller) deleteEgressIPAssignments(name string, statusAssignments []egressipv1.EgressIPStatusItem, namespaceSelector, podSelector metav1.LabelSelector) error {
	oc.deleteAllocatorEgressIPAssignments(statusAssignments)
	namespaces, err := oc.watchFactory.GetNamespacesBySelector(namespaceSelector)
	if err != nil {
		return err
	}
	for _, namespace := range namespaces {
		if err := oc.deleteNamespaceEgressIPAssignment(name, statusAssignments, namespace, podSelector); err != nil {
			return err
		}
	}
	return nil
}

func (oc *Controller) deleteNamespaceEgressIPAssignment(name string, statusAssignments []egressipv1.EgressIPStatusItem, namespace *kapi.Namespace, podSelector metav1.LabelSelector) error {
	var pods []*kapi.Pod
	var err error
	selector, _ := metav1.LabelSelectorAsSelector(&podSelector)
	if !selector.Empty() {
		pods, err = oc.watchFactory.GetPodsBySelector(namespace.Name, podSelector)
		if err != nil {
			return err
		}
	} else {
		pods, err = oc.watchFactory.GetPods(namespace.Name)
		if err != nil {
			return err
		}
	}
	for _, pod := range pods {
		if err := oc.deletePodEgressIPAssignments(name, statusAssignments, pod); err != nil {
			return err
		}
	}
	return nil
}

func (oc *Controller) deletePodEgressIPAssignments(name string, statusAssignments []egressipv1.EgressIPStatusItem, pod *kapi.Pod) error {
	oc.eIPC.podAssignmentMutex.Lock()
	defer oc.eIPC.podAssignmentMutex.Unlock()
	for _, status := range statusAssignments {
		klog.V(5).Infof("Deleting pod egress IP status: %v for EgressIP: %s and pod: %s/%s", status, name, pod.Name, pod.Namespace)
		if err := oc.eIPC.deletePodEgressIPAssignment(name, status, pod); err != nil {
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

func (oc *Controller) updateEgressIPStatus(name string, statusItems []egressipv1.EgressIPStatusItem) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		egressIP, err := oc.watchFactory.GetEgressIP(name)
		if err != nil {
			return err
		}
		updateEgressIP := egressIP.DeepCopy()
		updateEgressIP.Status.Items = statusItems
		return oc.kube.UpdateEgressIP(updateEgressIP)
	})
}

func (oc *Controller) assignEgressIPs(name string, egressIPs []string) []egressipv1.EgressIPStatusItem {
	oc.eIPC.allocator.Lock()
	defer oc.eIPC.allocator.Unlock()
	assignments := []egressipv1.EgressIPStatusItem{}
	assignableNodes, existingAllocations := oc.getSortedEgressData()
	if len(assignableNodes) == 0 {
		eIPRef := kapi.ObjectReference{
			Kind: "EgressIP",
			Name: name,
		}
		oc.recorder.Eventf(&eIPRef, kapi.EventTypeWarning, "NoMatchingNodeFound", "no assignable nodes for EgressIP: %s, please tag at least one node with label: %s", name, util.GetNodeEgressLabel())
		klog.Errorf("No assignable nodes found for EgressIP: %s and requested IPs: %v", name, egressIPs)
		return assignments
	}
	klog.V(5).Infof("Current assignments are: %+v", existingAllocations)
	for _, egressIP := range egressIPs {
		klog.V(5).Infof("Will attempt assignment for egress IP: %s", egressIP)
		eIPC := net.ParseIP(egressIP)
		if _, exists := existingAllocations[eIPC.String()]; exists {
			eIPRef := kapi.ObjectReference{
				Kind: "EgressIP",
				Name: name,
			}
			oc.recorder.Eventf(&eIPRef, kapi.EventTypeWarning, "InvalidEgressIP", "egress IP: %s for object EgressIP: %s is already referenced by another EgressIP object", egressIP, name)
			klog.Errorf("Egress IP: %q for EgressIP: %s is already allocated, invalid", egressIP, name)
			return assignments
		}
		if node := oc.isAnyClusterNodeIP(eIPC); node != nil {
			eIPRef := kapi.ObjectReference{
				Kind: "EgressIP",
				Name: name,
			}
			oc.recorder.Eventf(
				&eIPRef,
				kapi.EventTypeWarning,
				"UnsupportedRequest",
				"Egress IP: %v for object EgressIP: %s is the IP address of node: %s, this is unsupported", eIPC, name, node.name,
			)
			klog.Errorf("Egress IP: %v is the IP address of node: %s", eIPC, node.name)
			return assignments
		}
		for _, eNode := range assignableNodes {
			klog.V(5).Infof("Attempting assignment on egress node: %+v", eNode)
			if eNode.getAllocationCountForEgressIP(name) > 0 {
				klog.V(5).Infof("Node: %s is already in use by another egress IP for this EgressIP: %s, trying another node", eNode.name, name)
				continue
			}
			if (eNode.v6Subnet != nil && eNode.v6Subnet.Contains(eIPC)) ||
				(eNode.v4Subnet != nil && eNode.v4Subnet.Contains(eIPC)) {
				assignments = append(assignments, egressipv1.EgressIPStatusItem{
					Node:     eNode.name,
					EgressIP: eIPC.String(),
				})
				klog.V(5).Infof("Successful assignment of egress IP: %s on node: %+v", egressIP, eNode)
				eNode.allocations[eIPC.String()] = name
				break
			}
		}
	}
	if len(assignments) == 0 {
		eIPRef := kapi.ObjectReference{
			Kind: "EgressIP",
			Name: name,
		}
		oc.recorder.Eventf(&eIPRef, kapi.EventTypeWarning, "NoMatchingNodeFound", "No matching nodes found, which can host any of the egress IPs: %v for object EgressIP: %s", egressIPs, name)
		klog.Errorf("No matching host found for EgressIP: %s", name)
		return assignments
	}
	if len(assignments) < len(egressIPs) {
		eIPRef := kapi.ObjectReference{
			Kind: "EgressIP",
			Name: name,
		}
		oc.recorder.Eventf(&eIPRef, kapi.EventTypeWarning, "UnassignedRequest", "Not all egress IPs for EgressIP: %s could be assigned, please tag more nodes", name)
	}
	return assignments
}

func (oc *Controller) getSortedEgressData() ([]*egressNode, map[string]bool) {
	assignableNodes := []*egressNode{}
	allAllocations := make(map[string]bool)
	for _, eNode := range oc.eIPC.allocator.cache {
		if eNode.isEgressAssignable && eNode.isReady && eNode.isReachable {
			assignableNodes = append(assignableNodes, eNode)
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
		if !isAssignable {
			eNode.allocations = make(map[string]string)
		}
	}
}

func (oc *Controller) setNodeEgressReady(nodeName string, isReady bool) {
	oc.eIPC.allocator.Lock()
	defer oc.eIPC.allocator.Unlock()
	if eNode, exists := oc.eIPC.allocator.cache[nodeName]; exists {
		eNode.isReady = isReady
		if !isReady {
			eNode.allocations = make(map[string]string)
		}
	}
}

func (oc *Controller) setNodeEgressReachable(nodeName string, isReachable bool) {
	oc.eIPC.allocator.Lock()
	defer oc.eIPC.allocator.Unlock()
	if eNode, exists := oc.eIPC.allocator.cache[nodeName]; exists {
		eNode.isReachable = isReachable
		if !isReachable {
			eNode.allocations = make(map[string]string)
		}
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

	egressIPs, err := oc.watchFactory.GetEgressIPs()
	if err != nil {
		return fmt.Errorf("unable to list EgressIPs, err: %v", err)
	}
	for _, egressIP := range egressIPs {
		if len(egressIP.Spec.EgressIPs) != len(egressIP.Status.Items) {
			// Send a "synthetic update" on all egress IPs which are not fully
			// assigned, the reconciliation loop for WatchEgressIP will try to
			// assign stuff to this new node. The workqueue's delta FIFO
			// implementation will not trigger a watch event for updates on
			// objects which have no semantic difference, hence: call the
			// reconciliation function directly.
			if err := oc.reconcileEgressIP(egressIP, egressIP); err != nil {
				klog.Errorf("Synthetic update for EgressIP: %s failed, err: %v", egressIP.Name, err)
			}
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

	egressIPs, err := oc.watchFactory.GetEgressIPs()
	if err != nil {
		return fmt.Errorf("unable to list EgressIPs, err: %v", err)
	}
	for _, eIP := range egressIPs {
		keepStatusItems := []egressipv1.EgressIPStatusItem{}
		for _, status := range eIP.Status.Items {
			if status.Node != egressNode.Name {
				keepStatusItems = append(keepStatusItems, status)
			}
		}
		if len(keepStatusItems) != len(eIP.Status.Items) {
			// Update the egress IP. The reconciliation loop for WatchEgressIP
			// will try to assign the un-assigned IPs to whatever nodes remain.
			// Perform a real update here. If we would call the reconciliation
			// loop's function directly we'll need to update the object in that
			// function and have the update recursively call the reconciliation
			// loop again, i.e: perform two reconciliations for the same change.
			if err := oc.updateEgressIPStatus(eIP.Name, keepStatusItems); err != nil {
				klog.Errorf("Re-assignment for EgressIP: %s failed, unable to update object, err: %v", eIP.Name, err)
			}
		}
	}
	return nil
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
			allocations: make(map[string]string),
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
	allocations        map[string]string
	isReady            bool
	isReachable        bool
	isEgressAssignable bool
	name               string
}

func (e *egressNode) getAllocationCountForEgressIP(name string) (count int) {
	for _, egressIPName := range e.allocations {
		if egressIPName == name {
			count++
		}
	}
	return
}

type allocator struct {
	*sync.Mutex
	// A cache used for egress IP assignments containing data for all cluster nodes
	// used for egress IP assignments
	cache map[string]*egressNode
}

type egressIPController struct {
	egressIPAssignmentMutex *sync.Mutex

	podAssignmentMutex *sync.Mutex
	// Cache used for keeping track of egressIP assigned pods
	podAssignment map[string][]egressipv1.EgressIPStatusItem

	// Cache of gateway join router IPs, usefull since these should not change often
	gatewayIPCache sync.Map

	allocator allocator

	// libovsdb northbound client interface
	nbClient libovsdbclient.Client

	// modelClient for performing idempotent NB operations
	modelClient libovsdbops.ModelClient

	// watchFactory watching k8s objects
	watchFactory *factory.WatchFactory
}

var skippedPodError = errors.New("pod has been skipped")

func (e *egressIPController) addPodEgressIPAssignment(egressIPName string, status egressipv1.EgressIPStatusItem, pod *kapi.Pod) (err error) {
	if pod.Spec.HostNetwork {
		return skippedPodError
	}
	podIPs, err := e.getPodIPs(pod)
	if err != nil || len(podIPs) == 0 {
		return skippedPodError
	}
	if err := e.handleEgressReroutePolicy(podIPs, status, egressIPName, e.createEgressReroutePolicy); err != nil {
		return fmt.Errorf("unable to create logical router policy, err: %v", err)
	}
	if config.Gateway.DisableSNATMultipleGWs {
		// remove snats to->nodeIP (from the node where pod exists) for these podIPs before adding the snat to->egressIP
		err = deletePerPodGRSNAT(e.nbClient, pod.Spec.NodeName, podIPs)
		if err != nil {
			return err
		}
	}
	ops, err := createNATRuleOps(e.nbClient, podIPs, status, egressIPName)
	if err != nil {
		return fmt.Errorf("unable to create NAT rule for status: %v, err: %v", status, err)
	}
	_, err = libovsdbops.TransactAndCheck(e.nbClient, ops)
	return err
}

func (e *egressIPController) deletePodEgressIPAssignment(egressIPName string, status egressipv1.EgressIPStatusItem, pod *kapi.Pod) error {
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
	defer func() {
		if err == nil {
			delete(e.podAssignment, getPodKey(pod))
		}
	}()
	if err := e.handleEgressReroutePolicy(podIPs, status, egressIPName, e.deleteEgressReroutePolicy); err != nil {
		return fmt.Errorf("unable to delete logical router policy, err: %v", err)
	}

	ops, err := deleteNATRuleOps(e.nbClient, []ovsdb.Operation{}, podIPs, status, egressIPName)
	if err != nil {
		return fmt.Errorf("unable to delete NAT rule for status: %v, err: %v", status, err)
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

// createEgressReroutePolicy uses logical router policies to force egress traffic to the egress node, for that we need
// to retrive the internal gateway router IP attached to the egress node. This method handles both the shared and
// local gateway mode case
func (e *egressIPController) handleEgressReroutePolicy(podIPNets []*net.IPNet, status egressipv1.EgressIPStatusItem, egressIPName string, cb func(filterOption, egressIPName string, gatewayRouterIP string) error) error {
	gatewayRouterIPv4, gatewayRouterIPv6 := "", ""
	isEgressIPv6 := utilnet.IsIPv6String(status.EgressIP)
	gatewayRouterIP, err := e.getGatewayRouterJoinIP(status.Node, isEgressIPv6)
	if err != nil {
		return fmt.Errorf("unable to retrieve gateway IP for node: %s, protocol is IPv6: %v, err: %v", status.Node, isEgressIPv6, err)
	}
	if isEgressIPv6 {
		gatewayRouterIPv6 = gatewayRouterIP.String()
	} else {
		gatewayRouterIPv4 = gatewayRouterIP.String()
	}
	for _, podIPNet := range podIPNets {
		podIP := podIPNet.IP
		if utilnet.IsIPv6(podIP) && gatewayRouterIPv6 != "" {
			filterOption := fmt.Sprintf("ip6.src == %s", podIP.String())
			if err := cb(filterOption, egressIPName, gatewayRouterIPv6); err != nil {
				return err
			}
		} else if !utilnet.IsIPv6(podIP) && gatewayRouterIPv4 != "" {
			filterOption := fmt.Sprintf("ip4.src == %s", podIP.String())
			if err := cb(filterOption, egressIPName, gatewayRouterIPv4); err != nil {
				return err
			}
		}
	}
	return nil
}

func (e *egressIPController) createEgressReroutePolicy(filterOption, egressIPName string, gatewayRouterIP string) error {
	logicalRouter := nbdb.LogicalRouter{}
	logicalRouterPolicy := nbdb.LogicalRouterPolicy{
		Match:    filterOption,
		Priority: types.EgressIPReroutePriority,
		Nexthops: []string{gatewayRouterIP},
		Action:   nbdb.LogicalRouterPolicyActionReroute,
		ExternalIDs: map[string]string{
			"name": egressIPName,
		},
	}
	opsModel := []libovsdbops.OperationModel{
		{
			Model: &logicalRouterPolicy,
			ModelPredicate: func(lrp *nbdb.LogicalRouterPolicy) bool {
				return lrp.Match == filterOption && lrp.Priority == types.EgressIPReroutePriority && lrp.ExternalIDs["name"] == egressIPName
			},
			OnModelMutations: []interface{}{
				&logicalRouterPolicy.Nexthops,
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

func (e *egressIPController) deleteEgressReroutePolicy(filterOption, egressIPName string, gatewayRouterIP string) error {
	logicalRouter := nbdb.LogicalRouter{}
	logicalRouterPolicy := nbdb.LogicalRouterPolicy{
		Nexthops: []string{gatewayRouterIP},
	}
	logicalRouterPolicyRes := []nbdb.LogicalRouterPolicy{}
	opsModel := []libovsdbops.OperationModel{
		{
			Model: &logicalRouterPolicy,
			ModelPredicate: func(lrp *nbdb.LogicalRouterPolicy) bool {
				return lrp.Match == filterOption && lrp.Priority == types.EgressIPReroutePriority && lrp.ExternalIDs["name"] == egressIPName
			},
			OnModelMutations: []interface{}{
				&logicalRouterPolicy.Nexthops,
			},
			ExistingResult: &logicalRouterPolicyRes,
			DoAfter: func() {
				if len(logicalRouterPolicyRes) == 1 && len(logicalRouterPolicyRes[0].Nexthops) == 1 && logicalRouterPolicyRes[0].Nexthops[0] == gatewayRouterIP {
					logicalRouter.Policies = libovsdbops.ExtractUUIDsFromModels(&logicalRouterPolicyRes)
				}
			},
		},
		{
			Model: &logicalRouter,
			ModelPredicate: func(lr *nbdb.LogicalRouter) bool {
				if len(logicalRouterPolicyRes) == 1 && len(logicalRouterPolicyRes[0].Nexthops) == 1 && logicalRouterPolicyRes[0].Nexthops[0] == gatewayRouterIP {
					return lr.Name == types.OVNClusterRouter
				}
				return false
			},
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
			node, err := oc.watchFactory.GetNode(nodeName)
			if err != nil {
				klog.Errorf("Node: %s reachability changed, but could not retrieve node from cache, err: %v", node.Name, err)
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

func createNATRuleOps(nbClient libovsdbclient.Client, podIPs []*net.IPNet, status egressipv1.EgressIPStatusItem, egressIPName string) ([]ovsdb.Operation, error) {
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
	ops, err := libovsdbops.AddOrUpdateNatsToRouterOps(nbClient, []ovsdb.Operation{}, router, nats...)
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
