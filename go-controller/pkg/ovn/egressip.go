package ovn

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/libovsdb/ovsdb"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/controller"
	egressipv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/networkmanager"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	egresssvc "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/controller/egressservice"
	ovnretry "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/retry"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/syncmap"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/egressip"
	utilerrors "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/errors"

	kapi "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/util/retry"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
)

type egressIPAddrSetName string
type egressIPFamilyValue string
type egressIPNoReroutePolicyName string
type egressIPQoSRuleName string

const (
	NodeIPAddrSetName             egressIPAddrSetName = "node-ips"
	EgressIPServedPodsAddrSetName egressIPAddrSetName = "egressip-served-pods"
	// the possible values for LRP DB objects for EIPs
	IPFamilyValueV4       egressIPFamilyValue         = "ip4"
	IPFamilyValueV6       egressIPFamilyValue         = "ip6"
	IPFamilyValue         egressIPFamilyValue         = "ip" // use it when its dualstack
	ReplyTrafficNoReroute egressIPNoReroutePolicyName = "EIP-No-Reroute-reply-traffic"
	ReplyTrafficMark      egressIPQoSRuleName         = "EgressIP-Mark-Reply-Traffic"
)

func getEgressIPAddrSetDbIDs(name egressIPAddrSetName, controller string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.AddressSetEgressIP, controller, map[libovsdbops.ExternalIDKey]string{
		// egress ip creates cluster-wide address sets with egressIpAddrSetName
		libovsdbops.ObjectNameKey: string(name),
	})
}

func getEgressIPLRPNoReRouteDbIDs(priority int, uniqueName egressIPNoReroutePolicyName, ipFamily egressIPFamilyValue) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.LogicalRouterPolicyEgressIP, DefaultNetworkControllerName, map[libovsdbops.ExternalIDKey]string{
		// egress ip creates global no-reroute policies at 102 priority
		libovsdbops.ObjectNameKey: string(uniqueName),
		libovsdbops.PriorityKey:   fmt.Sprintf("%d", priority),
		libovsdbops.IPFamilyKey:   string(ipFamily),
	})
}

func getEgressIPQoSRuleDbIDs(ipFamily egressIPFamilyValue) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.LogicalRouterPolicyEgressIP, DefaultNetworkControllerName, map[libovsdbops.ExternalIDKey]string{
		// egress ip creates reply traffic marker rule at 103 priority
		libovsdbops.ObjectNameKey: string(ReplyTrafficMark),
		libovsdbops.PriorityKey:   fmt.Sprintf("%d", types.EgressIPRerouteQoSRulePriority),
		libovsdbops.IPFamilyKey:   string(ipFamily),
	})
}

// main reconcile functions begin here

// reconcileEgressIP reconciles the database configuration
// setup in nbdb based on the received egressIP objects
// CASE 1: if old == nil && new != nil {add event, we do a full setup for all statuses}
// CASE 2: if old != nil && new == nil {delete event, we do a full teardown for all statuses}
// CASE 3: if old != nil && new != nil {update event,
//
//	  CASE 3.1: we calculate based on difference between old and new statuses
//	            which ones need teardown and which ones need setup
//	            this ensures there is no disruption for things that did not change
//	  CASE 3.2: Only Namespace selectors on Spec changed
//	  CASE 3.3: Only Pod Selectors on Spec changed
//	  CASE 3.4: Both Namespace && Pod Selectors on Spec changed
//	}
//
// NOTE: `Spec.EgressIPs“ updates for EIP object are not processed here, that is the job of cluster manager
//
//	We only care about `Spec.NamespaceSelector`, `Spec.PodSelector` and `Status` field
func (oc *DefaultNetworkController) reconcileEgressIP(old, new *egressipv1.EgressIP) (err error) {
	// CASE 1: EIP object deletion, we need to teardown database configuration for all the statuses
	if old != nil && new == nil {
		removeStatus := old.Status.Items
		if len(removeStatus) > 0 {
			if err := oc.deleteEgressIPAssignments(old.Name, removeStatus); err != nil {
				return err
			}
		}
	}
	// CASE 2: EIP object addition, we need to setup database configuration for all the statuses
	if old == nil && new != nil {
		addStatus := new.Status.Items
		if len(addStatus) > 0 {
			if err := oc.addEgressIPAssignments(new.Name, addStatus, new.Spec.NamespaceSelector, new.Spec.PodSelector); err != nil {
				return err
			}
		}
	}
	// CASE 3: EIP object update
	if old != nil && new != nil {
		oldEIP := old
		newEIP := new
		// CASE 3.1: we need to see which statuses
		//        1) need teardown
		//        2) need setup
		//        3) need no-op
		if !reflect.DeepEqual(oldEIP.Status.Items, newEIP.Status.Items) {
			statusToRemove := make(map[string]egressipv1.EgressIPStatusItem, 0)
			statusToKeep := make(map[string]egressipv1.EgressIPStatusItem, 0)
			for _, status := range oldEIP.Status.Items {
				statusToRemove[status.EgressIP] = status
			}
			for _, status := range newEIP.Status.Items {
				statusToKeep[status.EgressIP] = status
			}
			// only delete items that were in the oldSpec but cannot be found in the newSpec
			statusToDelete := make([]egressipv1.EgressIPStatusItem, 0)
			for eIP, oldStatus := range statusToRemove {
				if newStatus, ok := statusToKeep[eIP]; ok && newStatus.Node == oldStatus.Node {
					continue
				}
				statusToDelete = append(statusToDelete, oldStatus)
			}
			if len(statusToDelete) > 0 {
				if err := oc.deleteEgressIPAssignments(old.Name, statusToDelete); err != nil {
					return err
				}
			}
			// only add items that were NOT in the oldSpec but can be found in the newSpec
			statusToAdd := make([]egressipv1.EgressIPStatusItem, 0)
			for eIP, newStatus := range statusToKeep {
				if oldStatus, ok := statusToRemove[eIP]; ok && oldStatus.Node == newStatus.Node {
					continue
				}
				statusToAdd = append(statusToAdd, newStatus)
			}
			if len(statusToAdd) > 0 {
				if err := oc.addEgressIPAssignments(new.Name, statusToAdd, new.Spec.NamespaceSelector, new.Spec.PodSelector); err != nil {
					return err
				}
			}
		}

		oldNamespaceSelector, err := metav1.LabelSelectorAsSelector(&oldEIP.Spec.NamespaceSelector)
		if err != nil {
			return fmt.Errorf("invalid old namespaceSelector, err: %v", err)
		}
		newNamespaceSelector, err := metav1.LabelSelectorAsSelector(&newEIP.Spec.NamespaceSelector)
		if err != nil {
			return fmt.Errorf("invalid new namespaceSelector, err: %v", err)
		}
		oldPodSelector, err := metav1.LabelSelectorAsSelector(&oldEIP.Spec.PodSelector)
		if err != nil {
			return fmt.Errorf("invalid old podSelector, err: %v", err)
		}
		newPodSelector, err := metav1.LabelSelectorAsSelector(&newEIP.Spec.PodSelector)
		if err != nil {
			return fmt.Errorf("invalid new podSelector, err: %v", err)
		}
		// CASE 3.2: Only Namespace selectors on Spec changed
		// Only the namespace selector changed: remove the setup for all pods
		// matching the old and not matching the new, and add setup for the pod
		// matching the new and which didn't match the old.
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
			// CASE 3.3: Only Pod Selectors on Spec changed
			// Only the pod selector changed: remove the setup for all pods
			// matching the old and not matching the new, and add setup for the pod
			// matching the new and which didn't match the old.
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
					if util.PodCompleted(pod) {
						continue
					}
					if newPodSelector.Matches(podLabels) && !oldPodSelector.Matches(podLabels) {
						if err := oc.addPodEgressIPAssignmentsWithLock(newEIP.Name, newEIP.Status.Items, pod); err != nil {
							return err
						}
					}
				}
			}
			// CASE 3.4: Both Namespace && Pod Selectors on Spec changed
			// Both selectors changed: remove the setup for pods matching the
			// old ones and not matching the new ones, and add setup for all
			// matching the new ones but which didn't match the old ones.
		} else if !reflect.DeepEqual(newNamespaceSelector, oldNamespaceSelector) && !reflect.DeepEqual(newPodSelector, oldPodSelector) {
			namespaces, err := oc.watchFactory.GetNamespaces()
			if err != nil {
				return err
			}
			for _, namespace := range namespaces {
				namespaceLabels := labels.Set(namespace.Labels)
				// If the namespace does not match anymore then there's no
				// reason to look at the pod selector.
				if !newNamespaceSelector.Matches(namespaceLabels) && oldNamespaceSelector.Matches(namespaceLabels) {
					if err := oc.deleteNamespaceEgressIPAssignment(oldEIP.Name, oldEIP.Status.Items, namespace, oldEIP.Spec.PodSelector); err != nil {
						return err
					}
				}
				// If the namespace starts matching, look at the pods selector
				// and pods in that namespace and perform the setup for the pods
				// which match the new pod selector or if the podSelector is empty
				// then just perform the setup.
				if newNamespaceSelector.Matches(namespaceLabels) && !oldNamespaceSelector.Matches(namespaceLabels) {
					pods, err := oc.watchFactory.GetPods(namespace.Name)
					if err != nil {
						return err
					}
					for _, pod := range pods {
						podLabels := labels.Set(pod.Labels)
						if newPodSelector.Matches(podLabels) {
							if err := oc.addPodEgressIPAssignmentsWithLock(newEIP.Name, newEIP.Status.Items, pod); err != nil {
								return err
							}
						}
					}
				}
				// If the namespace continues to match, look at the pods
				// selector and pods in that namespace.
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
						if util.PodCompleted(pod) {
							continue
						}
						if newPodSelector.Matches(podLabels) && !oldPodSelector.Matches(podLabels) {
							if err := oc.addPodEgressIPAssignmentsWithLock(newEIP.Name, newEIP.Status.Items, pod); err != nil {
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

// reconcileEgressIPNamespace reconciles the database configuration setup in nbdb
// based on received namespace objects.
// NOTE: we only care about namespace label updates
func (oc *DefaultNetworkController) reconcileEgressIPNamespace(old, new *v1.Namespace) error {
	// Same as for reconcileEgressIP: labels play nicely with empty object, not
	// nil ones.
	oldNamespace, newNamespace := &v1.Namespace{}, &v1.Namespace{}
	if old != nil {
		oldNamespace = old
	}
	if new != nil {
		newNamespace = new
	}

	// If the labels have not changed, then there's no change that we care
	// about: return.
	oldLabels := labels.Set(oldNamespace.Labels)
	newLabels := labels.Set(newNamespace.Labels)
	if reflect.DeepEqual(newLabels.AsSelector(), oldLabels.AsSelector()) {
		return nil
	}

	// Iterate all EgressIPs and check if this namespace start/stops matching
	// any and add/remove the setup accordingly. Namespaces can match multiple
	// EgressIP objects (ex: users can chose to have one EgressIP object match
	// all "blue" pods in namespace A, and a second EgressIP object match all
	// "red" pods in namespace A), so continue iterating all EgressIP objects
	// before finishing.
	egressIPs, err := oc.watchFactory.GetEgressIPs()
	if err != nil {
		return err
	}
	for _, egressIP := range egressIPs {
		namespaceSelector, err := metav1.LabelSelectorAsSelector(&egressIP.Spec.NamespaceSelector)
		if err != nil {
			return err
		}
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

// reconcileEgressIPPod reconciles the database configuration setup in nbdb
// based on received pod objects.
// NOTE: we only care about pod label updates
func (oc *DefaultNetworkController) reconcileEgressIPPod(old, new *v1.Pod) (err error) {
	oldPod, newPod := &v1.Pod{}, &v1.Pod{}
	namespace := &v1.Namespace{}
	if old != nil {
		oldPod = old
		namespace, err = oc.watchFactory.GetNamespace(oldPod.Namespace)
		if err != nil {
			// when the whole namespace gets removed, we can ignore the NotFound error here
			// any potential configuration will get removed in reconcileEgressIPNamespace
			if new == nil && apierrors.IsNotFound(err) {
				klog.V(5).Infof("Namespace %s no longer exists for the deleted pod: %s", oldPod.Namespace, oldPod.Name)
				return nil
			}
			return err
		}
	}
	if new != nil {
		newPod = new
		namespace, err = oc.watchFactory.GetNamespace(newPod.Namespace)
		if err != nil {
			return err
		}
	}

	newPodLabels := labels.Set(newPod.Labels)
	oldPodLabels := labels.Set(oldPod.Labels)

	// If the namespace the pod belongs to does not have any labels, just return
	// it can't match any EgressIP object
	namespaceLabels := labels.Set(namespace.Labels)
	if namespaceLabels.AsSelector().Empty() {
		return nil
	}

	// Iterate all EgressIPs and check if this pod start/stops matching any and
	// add/remove the setup accordingly. Pods should not match multiple EgressIP
	// objects: that is considered a user error and is undefined. However, in
	// such events iterate all EgressIPs and clean up as much as possible. By
	// iterating all EgressIPs we also cover the case where a pod has its labels
	// changed from matching one EgressIP to another, ex: EgressIP1 matching
	// "blue pods" and EgressIP2 matching "red pods". If a pod with a red label
	// gets changed to a blue label: we need add and remove the set up for both
	// EgressIP obejcts - since we can't be sure of which EgressIP object we
	// process first, always iterate all.
	egressIPs, err := oc.watchFactory.GetEgressIPs()
	if err != nil {
		return err
	}
	for _, egressIP := range egressIPs {
		namespaceSelector, err := metav1.LabelSelectorAsSelector(&egressIP.Spec.NamespaceSelector)
		if err != nil {
			return err
		}
		if namespaceSelector.Matches(namespaceLabels) {
			// If the namespace the pod belongs to matches this object then
			// check the if there's a podSelector defined on the EgressIP
			// object. If there is one: the user intends the EgressIP object to
			// match only a subset of pods in the namespace, and we'll have to
			// check that. If there is no podSelector: the user intends it to
			// match all pods in the namespace.
			podSelector, err := metav1.LabelSelectorAsSelector(&egressIP.Spec.PodSelector)
			if err != nil {
				return err
			}
			if !podSelector.Empty() {
				// Use "new" and "old" instead of "newPod" and "oldPod" to determine whether
				// pods was created or is being deleted.
				newMatches := new != nil && podSelector.Matches(newPodLabels)
				oldMatches := old != nil && podSelector.Matches(oldPodLabels)
				// If the podSelector doesn't match the pod, then continue
				// because this EgressIP intends to match other pods in that
				// namespace and not this one. Other EgressIP objects might
				// match the pod though so we need to check that.
				if !newMatches && !oldMatches {
					continue
				}
				// Check if the pod stopped matching. If the pod was deleted,
				// "new" will be nil, so this must account for that case.
				if !newMatches && oldMatches {
					if err := oc.deletePodEgressIPAssignments(egressIP.Name, egressIP.Status.Items, oldPod); err != nil {
						return err
					}
					continue
				}
				// If the pod starts matching the podSelector or continues to
				// match: add the pod. The reason as to why we need to continue
				// adding it if it continues to match, as opposed to once when
				// it started matching, is because the pod might not have pod
				// IPs assigned at that point and we need to continue trying the
				// pod setup for every pod update as to make sure we process the
				// pod IP assignment.
				if err := oc.addPodEgressIPAssignmentsWithLock(egressIP.Name, egressIP.Status.Items, newPod); err != nil {
					return err
				}
				continue
			}
			// If the podSelector is empty (i.e: the EgressIP object is intended
			// to match all pods in the namespace) and the pod has been deleted:
			// "new" will be nil and we need to remove the setup
			if new == nil {
				if err := oc.deletePodEgressIPAssignments(egressIP.Name, egressIP.Status.Items, oldPod); err != nil {
					return err
				}
				continue
			}
			// For all else, perform a setup for the pod
			if err := oc.addPodEgressIPAssignmentsWithLock(egressIP.Name, egressIP.Status.Items, newPod); err != nil {
				return err
			}
		}
	}
	return nil
}

// main reconcile functions end here and local zone controller functions begin

func (oc *DefaultNetworkController) addEgressIPAssignments(name string, statusAssignments []egressipv1.EgressIPStatusItem, namespaceSelector, podSelector metav1.LabelSelector) error {
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

func (oc *DefaultNetworkController) addNamespaceEgressIPAssignments(name string, statusAssignments []egressipv1.EgressIPStatusItem, namespace *kapi.Namespace, podSelector metav1.LabelSelector) error {
	var pods []*kapi.Pod
	var err error
	selector, err := metav1.LabelSelectorAsSelector(&podSelector)
	if err != nil {
		return err
	}
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
		if err := oc.addPodEgressIPAssignmentsWithLock(name, statusAssignments, pod); err != nil {
			return err
		}
	}
	return nil
}

func (oc *DefaultNetworkController) addPodEgressIPAssignmentsWithLock(name string, statusAssignments []egressipv1.EgressIPStatusItem, pod *kapi.Pod) error {
	oc.eIPC.podAssignmentMutex.Lock()
	defer oc.eIPC.podAssignmentMutex.Unlock()
	return oc.addPodEgressIPAssignments(name, statusAssignments, pod)
}

// addPodEgressIPAssignments tracks the setup made for each egress IP matching
// pod w.r.t to each status. This is mainly done to avoid a lot of duplicated
// work on ovnkube-master restarts when all egress IP handlers will most likely
// match and perform the setup for the same pod and status multiple times over.
func (oc *DefaultNetworkController) addPodEgressIPAssignments(name string, statusAssignments []egressipv1.EgressIPStatusItem, pod *kapi.Pod) error {
	podKey := getPodKey(pod)
	// If pod is already in succeeded or failed state, return it without proceeding further.
	if util.PodCompleted(pod) {
		klog.Infof("Pod %s is already in completed state, skipping egress ip assignment", podKey)
		return nil
	}
	// If statusAssignments is empty just return, not doing this will delete the
	// external GW set up, even though there might be no egress IP set up to
	// perform.
	if len(statusAssignments) == 0 {
		return nil
	}
	// We need to proceed with add only under two conditions
	// 1) egressNode present in at least one status is local to this zone
	// (NOTE: The relation between egressIPName and nodeName is 1:1 i.e in the same object the given node will be present only in one status)
	// 2) the pod being added is local to this zone
	proceed := false
	for _, status := range statusAssignments {
		oc.eIPC.nodeZoneState.LockKey(status.Node)
		isLocalZoneEgressNode, loadedEgressNode := oc.eIPC.nodeZoneState.Load(status.Node)
		if loadedEgressNode && isLocalZoneEgressNode {
			proceed = true
			oc.eIPC.nodeZoneState.UnlockKey(status.Node)
			break
		}
		oc.eIPC.nodeZoneState.UnlockKey(status.Node)
	}
	if !proceed && !oc.isPodScheduledinLocalZone(pod) {
		return nil // nothing to do if none of the status nodes are local to this master and pod is also remote
	}
	var remainingAssignments []egressipv1.EgressIPStatusItem
	var podIPs []*net.IPNet
	var err error
	if oc.isPodScheduledinLocalZone(pod) {
		// Retrieve the pod's networking configuration from the
		// logicalPortCache. The reason for doing this: a) only normal network
		// pods are placed in this cache, b) once the pod is placed here we know
		// addLogicalPort has finished successfully setting up networking for
		// the pod, so we can proceed with retrieving its IP and deleting the
		// external GW configuration created in addLogicalPort for the pod.
		logicalPort, err := oc.logicalPortCache.get(pod, types.DefaultNetworkName)
		if err != nil {
			return nil
		}
		// Since the logical switch port cache removes entries only 60 seconds
		// after deletion, its possible that when pod is recreated with the same name
		// within the 60seconds timer, stale info gets used to create SNATs and reroutes
		// for the eip pods. Checking if the expiry is set for the port or not can indicate
		// if the port is scheduled for deletion.
		if !logicalPort.expires.IsZero() {
			klog.Warningf("Stale LSP %s for pod %s found in cache refetching",
				logicalPort.name, podKey)
			return nil
		}
		podIPs = logicalPort.ips
	} else { // means this is egress node's local master
		podIPs, err = util.GetPodCIDRsWithFullMask(pod, oc.GetNetInfo())
		if err != nil {
			return err
		}
	}
	podState, exists := oc.eIPC.podAssignment[podKey]
	if !exists {
		remainingAssignments = statusAssignments
		podState = &podAssignmentState{
			egressIPName:         name,
			egressStatuses:       egressStatuses{make(map[egressipv1.EgressIPStatusItem]string)},
			standbyEgressIPNames: sets.New[string](),
		}
		oc.eIPC.podAssignment[podKey] = podState
	} else if podState.egressIPName == name || podState.egressIPName == "" {
		// We do the setup only if this egressIP object is the one serving this pod OR
		// podState.egressIPName can be empty if no re-routes were found in
		// syncPodAssignmentCache for the existing pod, we will treat this case as a new add
		for _, status := range statusAssignments {
			if exists := podState.egressStatuses.contains(status); !exists {
				remainingAssignments = append(remainingAssignments, status)
			}
		}
		podState.egressIPName = name
		podState.standbyEgressIPNames.Delete(name)
	} else if podState.egressIPName != name {
		klog.Warningf("EgressIP object %s will not be configured for pod %s "+
			"since another egressIP object %s is serving it", name, podKey, podState.egressIPName)
		eIPRef := kapi.ObjectReference{
			Kind: "EgressIP",
			Name: name,
		}
		oc.recorder.Eventf(
			&eIPRef,
			kapi.EventTypeWarning,
			"UndefinedRequest",
			"EgressIP object %s will not be configured for pod %s since another egressIP object %s is serving it, this is undefined", name, podKey, podState.egressIPName,
		)
		podState.standbyEgressIPNames.Insert(name)
		return nil
	}
	for _, status := range remainingAssignments {
		klog.V(2).Infof("Adding pod egress IP status: %v for EgressIP: %s and pod: %s/%s/%v", status, name, pod.Namespace, pod.Name, podIPs)
		err = oc.eIPC.nodeZoneState.DoWithLock(status.Node, func(key string) error {
			if status.Node == pod.Spec.NodeName {
				// we are safe, no need to grab lock again
				if err := oc.eIPC.addPodEgressIPAssignment(name, status, pod, podIPs); err != nil {
					return fmt.Errorf("unable to create egressip configuration for pod %s/%s/%v, err: %w", pod.Namespace, pod.Name, podIPs, err)
				}
				podState.egressStatuses.statusMap[status] = ""
				return nil
			}
			return oc.eIPC.nodeZoneState.DoWithLock(pod.Spec.NodeName, func(key string) error {
				// we need to grab lock again for pod's node
				if err := oc.eIPC.addPodEgressIPAssignment(name, status, pod, podIPs); err != nil {
					return fmt.Errorf("unable to create egressip configuration for pod %s/%s/%v, err: %w", pod.Namespace, pod.Name, podIPs, err)
				}
				podState.egressStatuses.statusMap[status] = ""
				return nil
			})
		})
		if err != nil {
			return err
		}
	}
	if oc.isPodScheduledinLocalZone(pod) {
		// add the podIP to the global egressIP address set
		addrSetIPs := make([]net.IP, len(podIPs))
		for i, podIP := range podIPs {
			copyPodIP := *podIP
			addrSetIPs[i] = copyPodIP.IP
		}
		if err := oc.addPodIPsToAddressSet(addrSetIPs); err != nil {
			return fmt.Errorf("cannot add egressPodIPs for the pod %s/%s to the address set: err: %v", pod.Namespace, pod.Name, err)
		}
	}
	return nil
}

// deleteEgressIPAssignments performs a full egress IP setup deletion on a per
// (egress IP name - status) basis. The idea is thus to list the full content of
// the NB DB for that egress IP object and delete everything which match the
// status. We also need to update the podAssignment cache and finally re-add the
// external GW setup in case the pod still exists.
func (oc *DefaultNetworkController) deleteEgressIPAssignments(name string, statusesToRemove []egressipv1.EgressIPStatusItem) error {
	oc.eIPC.podAssignmentMutex.Lock()
	defer oc.eIPC.podAssignmentMutex.Unlock()
	var podIPs []net.IP
	var err error
	for _, statusToRemove := range statusesToRemove {
		removed := false
		for podKey, podStatus := range oc.eIPC.podAssignment {
			if podStatus.egressIPName != name {
				// we can continue here since this pod was not managed by this EIP object
				podStatus.standbyEgressIPNames.Delete(name)
				continue
			}
			if ok := podStatus.egressStatuses.contains(statusToRemove); !ok {
				// we can continue here since this pod was not managed by this statusToRemove
				continue
			}
			err = oc.eIPC.nodeZoneState.DoWithLock(statusToRemove.Node, func(key string) error {
				// this statusToRemove was managing at least one pod, hence let's tear down the setup for this status
				if !removed {
					klog.V(2).Infof("Deleting pod egress IP status: %v for EgressIP: %s", statusToRemove, name)
					if podIPs, err = oc.eIPC.deleteEgressIPStatusSetup(name, statusToRemove); err != nil {
						return err
					}
					removed = true // we should only tear down once and not per pod since tear down is based on externalIDs
				}
				// this pod was managed by statusToRemove.EgressIP; we need to try and add its SNAT back towards nodeIP
				podNamespace, podName := getPodNamespaceAndNameFromKey(podKey)
				if err = oc.eIPC.addExternalGWPodSNAT(podNamespace, podName, statusToRemove); err != nil {
					return err
				}
				podStatus.egressStatuses.delete(statusToRemove)
				return nil
			})
			if err != nil {
				return err
			}
			if len(podStatus.egressStatuses.statusMap) == 0 && len(podStatus.standbyEgressIPNames) == 0 {
				// pod could be managed by more than one egressIP
				// so remove the podKey from cache only if we are sure
				// there are no more egressStatuses managing this pod
				klog.V(5).Infof("Deleting pod key %s from assignment cache", podKey)
				// delete the podIP from the global egressIP address set since its no longer managed by egressIPs
				// NOTE(tssurya): There is no way to infer if pod was local to this zone or not,
				// so we try to nuke the IP from address-set anyways - it will be a no-op for remote pods
				if err := oc.deletePodIPsFromAddressSet(podIPs); err != nil {
					return fmt.Errorf("cannot delete egressPodIPs for the pod %s from the address set: err: %v", podKey, err)
				}
				delete(oc.eIPC.podAssignment, podKey)
			} else if len(podStatus.egressStatuses.statusMap) == 0 && len(podStatus.standbyEgressIPNames) > 0 {
				klog.V(2).Infof("Pod %s has standby egress IP %+v", podKey, podStatus.standbyEgressIPNames.UnsortedList())
				podStatus.egressIPName = "" // we have deleted the current egressIP that was managing the pod
				if err := oc.addStandByEgressIPAssignment(podKey, podStatus); err != nil {
					klog.Errorf("Adding standby egressIPs for pod %s with status %v failed: %v", podKey, podStatus, err)
					// We are not returning the error on purpose, this will be best effort without any retries because
					// retrying deleteEgressIPAssignments for original EIP because addStandByEgressIPAssignment failed is useless.
					// Since we delete statusToRemove from podstatus.egressStatuses the first time we call this function,
					// later when the operation is retried we will never continue down the loop
					// since we add SNAT to node only if this pod was managed by statusToRemove
					return nil
				}
			}
		}
	}
	return nil
}

func (oc *DefaultNetworkController) deleteNamespaceEgressIPAssignment(name string, statusAssignments []egressipv1.EgressIPStatusItem, namespace *kapi.Namespace, podSelector metav1.LabelSelector) error {
	var pods []*kapi.Pod
	var err error
	selector, err := metav1.LabelSelectorAsSelector(&podSelector)
	if err != nil {
		return err
	}
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

func (oc *DefaultNetworkController) deletePodEgressIPAssignments(name string, statusesToRemove []egressipv1.EgressIPStatusItem, pod *kapi.Pod) error {
	oc.eIPC.podAssignmentMutex.Lock()
	defer oc.eIPC.podAssignmentMutex.Unlock()
	podKey := getPodKey(pod)
	podStatus, exists := oc.eIPC.podAssignment[podKey]
	if !exists {
		return nil
	} else if podStatus.egressIPName != name {
		// standby egressIP no longer matches this pod, update cache
		podStatus.standbyEgressIPNames.Delete(name)
		return nil
	}
	podIPs, err := util.GetPodCIDRsWithFullMask(pod, oc.GetNetInfo())
	// FIXME(trozet): this error can be ignored if ErrNoPodIPFound, but unit test:
	// egressIP pod recreate with same name (stateful-sets) shouldn't use stale logicalPortCache entries AND stale podAssignment cache entries
	// heavily relies on this error happening.
	if err != nil {
		return err
	}
	for _, statusToRemove := range statusesToRemove {
		if ok := podStatus.egressStatuses.contains(statusToRemove); !ok {
			// we can continue here since this pod was not managed by this statusToRemove
			continue
		}
		klog.V(2).Infof("Deleting pod egress IP status: %v for EgressIP: %s and pod: %s/%s", statusToRemove, name, pod.Name, pod.Namespace)
		err = oc.eIPC.nodeZoneState.DoWithLock(statusToRemove.Node, func(key string) error {
			if statusToRemove.Node == pod.Spec.NodeName {
				// we are safe, no need to grab lock again
				if err := oc.eIPC.deletePodEgressIPAssignment(name, statusToRemove, pod, podIPs); err != nil {
					return err
				}
				podStatus.egressStatuses.delete(statusToRemove)
				return nil
			}
			return oc.eIPC.nodeZoneState.DoWithLock(pod.Spec.NodeName, func(key string) error {
				if err := oc.eIPC.deletePodEgressIPAssignment(name, statusToRemove, pod, podIPs); err != nil {
					return err
				}
				podStatus.egressStatuses.delete(statusToRemove)
				return nil
			})
		})
		if err != nil {
			return err
		}
	}
	// Delete the key if there are no more status assignments to keep
	// for the pod.
	if len(podStatus.egressStatuses.statusMap) == 0 {
		// pod could be managed by more than one egressIP
		// so remove the podKey from cache only if we are sure
		// there are no more egressStatuses managing this pod
		klog.V(5).Infof("Deleting pod key %s from assignment cache", podKey)
		if oc.isPodScheduledinLocalZone(pod) {
			// delete the podIP from the global egressIP address set
			addrSetIPs := make([]net.IP, len(podIPs))
			for i, podIP := range podIPs {
				copyPodIP := *podIP
				addrSetIPs[i] = copyPodIP.IP
			}
			if err := oc.deletePodIPsFromAddressSet(addrSetIPs); err != nil {
				return fmt.Errorf("cannot delete egressPodIPs for the pod %s from the address set: err: %v", podKey, err)
			}
		}
		delete(oc.eIPC.podAssignment, podKey)
	}
	return nil
}

type egressIPCacheEntry struct {
	// egressLocalPods will contain all the pods that
	// are local to this zone being served by thie egressIP
	// object. This will help sync LRP & LRSR.
	egressLocalPods map[string]sets.Set[string]
	// egressRemotePods will contain all the remote pods
	// that are being served by this egressIP object
	// This will help sync SNATs.
	egressRemotePods map[string]sets.Set[string] // will be used only when multizone IC is enabled
	gatewayRouterIPs sets.Set[string]
	egressIPs        map[string]string
	// egressLocalNodes will contain all nodes that are local
	// to this zone which are serving this egressIP object..
	// This will help sync SNATs
	egressLocalNodes sets.Set[string]
}

func (oc *DefaultNetworkController) syncEgressIPs(namespaces []interface{}) error {
	// This part will take of syncing stale data which we might have in OVN if
	// there's no ovnkube-master running for a while, while there are changes to
	// pods/egress IPs.
	// It will sync:
	// - Egress IPs which have been deleted while ovnkube-master was down
	// - pods/namespaces which have stopped matching on egress IPs while
	//   ovnkube-master was down
	// - create an address-set that can hold all the egressIP pods and sync the address set by
	//   resetting pods that are managed by egressIPs based on the constructed kapi cache
	// This function is called when handlers for EgressIPNamespaceType are started
	// since namespaces is the first object that egressIP feature starts watching

	// update localZones cache of eIPCZoneController
	// WatchNodes() is called before WatchEgressIPNamespaces() so the oc.localZones cache
	// will be updated whereas WatchEgressNodes() is called after WatchEgressIPNamespaces()
	// and so we must update the cache to ensure we are not stale.
	if err := oc.syncLocalNodeZonesCache(); err != nil {
		return fmt.Errorf("syncLocalNodeZonesCache unable to update the local zones node cache: %v", err)
	}
	egressIPCache, err := oc.generateCacheForEgressIP()
	if err != nil {
		return fmt.Errorf("syncEgressIPs unable to generate cache for egressip: %v", err)
	}
	if err = oc.syncStaleEgressReroutePolicy(egressIPCache); err != nil {
		return fmt.Errorf("syncEgressIPs unable to remove stale reroute policies: %v", err)
	}
	if err = oc.syncStaleSNATRules(egressIPCache); err != nil {
		return fmt.Errorf("syncEgressIPs unable to remove stale nats: %v", err)
	}
	if err = oc.syncPodAssignmentCache(egressIPCache); err != nil {
		return fmt.Errorf("syncEgressIPs unable to sync internal pod assignment cache: %v", err)
	}
	if err = oc.syncStaleAddressSetIPs(egressIPCache); err != nil {
		return fmt.Errorf("syncEgressIPs unable to reset stale address IPs: %v", err)
	}
	return nil
}

func (oc *DefaultNetworkController) syncLocalNodeZonesCache() error {
	nodes, err := oc.watchFactory.GetNodes()
	if err != nil {
		return fmt.Errorf("unable to fetch nodes from watch factory %w", err)
	}
	for _, node := range nodes {
		// NOTE: Even at this stage, there can be race; the bnc.zone might be the nodeName
		// while the node's annotations are not yet set, so it still shows global.
		// The EgressNodeType events (which are basically all node updates) should
		// constantly update this cache as nodes get added, updated and removed
		oc.eIPC.nodeZoneState.LockKey(node.Name)
		oc.eIPC.nodeZoneState.Store(node.Name, oc.isLocalZoneNode(node))
		oc.eIPC.nodeZoneState.UnlockKey(node.Name)
	}
	return nil
}

func (oc *DefaultNetworkController) syncStaleAddressSetIPs(egressIPCache map[string]egressIPCacheEntry) error {
	dbIDs := getEgressIPAddrSetDbIDs(EgressIPServedPodsAddrSetName, oc.controllerName)
	as, err := oc.addressSetFactory.EnsureAddressSet(dbIDs)
	if err != nil {
		return fmt.Errorf("cannot ensure that addressSet for egressIP pods %s exists %v", EgressIPServedPodsAddrSetName, err)
	}
	var allEIPServedPodIPs []net.IP
	// we only care about local zone pods for the address-set since
	// traffic from remote pods towards nodeIP won't even reach this zone
	for eipName := range egressIPCache {
		for _, podIPs := range egressIPCache[eipName].egressLocalPods {
			for podIP := range podIPs {
				allEIPServedPodIPs = append(allEIPServedPodIPs, net.ParseIP(podIP))
			}
		}
	}
	// we replace all IPs in the address-set based on eIP cache constructed from kapi
	// note that setIPs is not thread-safe
	if err = as.SetAddresses(util.StringSlice(allEIPServedPodIPs)); err != nil {
		return fmt.Errorf("cannot reset egressPodIPs in address set %v: err: %v", EgressIPServedPodsAddrSetName, err)
	}
	return nil
}

// syncPodAssignmentCache rebuilds the internal pod cache used by the egressIP feature.
// We use the existing kapi and ovn-db information to populate oc.eIPC.podAssignment cache for
// all the pods that are managed by egressIPs.
// NOTE: This is done mostly to handle the corner case where one pod has more than one
// egressIP object matching it, in which case we do the ovn setup only for one of the objects.
// This corner case  of same pod matching more than one object will not work for IC deployments
// since internal cache based logic will be different for different ovnkube-controllers
// zone can think objA is active while zoneb can think objB is active if both have multiple choice options
func (oc *DefaultNetworkController) syncPodAssignmentCache(egressIPCache map[string]egressIPCacheEntry) error {
	oc.eIPC.podAssignmentMutex.Lock()
	defer oc.eIPC.podAssignmentMutex.Unlock()
	for egressIPName, state := range egressIPCache {
		p1 := func(item *nbdb.LogicalRouterPolicy) bool {
			return item.Priority == types.EgressIPReroutePriority && item.ExternalIDs["name"] == egressIPName
		}
		reRoutePolicies, err := libovsdbops.FindLogicalRouterPoliciesWithPredicate(oc.nbClient, p1)
		if err != nil {
			return err
		}
		p2 := func(item *nbdb.NAT) bool {
			return item.ExternalIDs["name"] == egressIPName
		}
		egressIPSNATs, err := libovsdbops.FindNATsWithPredicate(oc.nbClient, p2)
		if err != nil {
			return err
		}
		// Because of how we do generateCacheForEgressIP, we will only have pods that are
		// either local to zone (in which case reRoutePolicies will work) OR pods that are
		// managed by local egressIP nodes (in which case egressIPSNATs will work)
		egressPods := make(map[string]sets.Set[string])
		for podKey, podIPs := range state.egressLocalPods {
			egressPods[podKey] = podIPs
		}
		for podKey, podIPs := range state.egressRemotePods {
			egressPods[podKey] = podIPs
		}
		for podKey, podIPs := range egressPods {
			podState, ok := oc.eIPC.podAssignment[podKey]
			if !ok {
				podState = &podAssignmentState{
					egressStatuses:       egressStatuses{make(map[egressipv1.EgressIPStatusItem]string)},
					standbyEgressIPNames: sets.New[string](),
				}
			}

			podState.standbyEgressIPNames.Insert(egressIPName)
			for _, policy := range reRoutePolicies {
				splitMatch := strings.Split(policy.Match, " ")
				if len(splitMatch) <= 0 {
					continue
				}
				logicalIP := splitMatch[len(splitMatch)-1]
				parsedLogicalIP := net.ParseIP(logicalIP)
				if parsedLogicalIP == nil {
					continue
				}

				if podIPs.Has(parsedLogicalIP.String()) { // should match for only one egressIP object
					podState.egressIPName = egressIPName
					podState.standbyEgressIPNames.Delete(egressIPName)
					klog.Infof("EgressIP %s is managing pod %s", egressIPName, podKey)
				}
			}
			for _, snat := range egressIPSNATs {
				if podIPs.Has(snat.LogicalIP) { // should match for only one egressIP object
					podState.egressIPName = egressIPName
					podState.standbyEgressIPNames.Delete(egressIPName)
					klog.Infof("EgressIP %s is managing pod %s", egressIPName, podKey)
				}
			}
			oc.eIPC.podAssignment[podKey] = podState
		}
	}

	return nil
}

// This function implements a portion of syncEgressIPs.
// It removes OVN logical router policies used by EgressIPs deleted while ovnkube-master was down.
// It also removes stale nexthops from router policies used by EgressIPs.
// Upon failure, it may be invoked multiple times in order to avoid a pod restart.
func (oc *DefaultNetworkController) syncStaleEgressReroutePolicy(egressIPCache map[string]egressIPCacheEntry) error {
	logicalRouterPolicyStaleNexthops := []*nbdb.LogicalRouterPolicy{}
	p := func(item *nbdb.LogicalRouterPolicy) bool {
		if item.Priority != types.EgressIPReroutePriority {
			return false
		}
		egressIPName := item.ExternalIDs["name"]
		cacheEntry, exists := egressIPCache[egressIPName]
		splitMatch := strings.Split(item.Match, " ")
		logicalIP := splitMatch[len(splitMatch)-1]
		parsedLogicalIP := net.ParseIP(logicalIP)
		egressPodIPs := sets.NewString()
		if exists {
			// Since LRPs are created only for pods local to this zone
			// we need to care about only those pods. Nexthop for them will
			// either be transit switch IP or join switch IP or mp0 IP.
			// FIXME: LRPs are also created for remote pods to route them
			// correctly but we do not handling cleaning for them now
			for _, podIPs := range cacheEntry.egressLocalPods {
				egressPodIPs.Insert(podIPs.UnsortedList()...)
			}
		}
		if !exists || cacheEntry.gatewayRouterIPs.Len() == 0 || !egressPodIPs.Has(parsedLogicalIP.String()) {
			klog.Infof("syncStaleEgressReroutePolicy will delete %s due to no nexthop or stale logical ip: %v", egressIPName, item)
			return true
		}
		// Check for stale nexthops that may exist in the logical router policy and store that in logicalRouterPolicyStaleNexthops.
		// Note: adding missing nexthop(s) to the logical router policy is done outside the scope of this function.
		staleNextHops := []string{}
		for _, nexthop := range item.Nexthops {
			if !cacheEntry.gatewayRouterIPs.Has(nexthop) {
				staleNextHops = append(staleNextHops, nexthop)
			}
		}
		if len(staleNextHops) > 0 {
			lrp := nbdb.LogicalRouterPolicy{
				UUID:     item.UUID,
				Nexthops: staleNextHops,
			}
			logicalRouterPolicyStaleNexthops = append(logicalRouterPolicyStaleNexthops, &lrp)
		}
		return false
	}

	err := libovsdbops.DeleteLogicalRouterPoliciesWithPredicate(oc.nbClient, oc.GetNetworkScopedClusterRouterName(), p)
	if err != nil {
		return fmt.Errorf("error deleting stale logical router policies from router %s: %v", oc.GetNetworkScopedClusterRouterName(), err)
	}

	// Update Logical Router Policies that have stale nexthops. Notice that we must do this separately
	// because logicalRouterPolicyStaleNexthops must be populated first
	klog.Infof("syncStaleEgressReroutePolicy will remove stale nexthops: %+v", logicalRouterPolicyStaleNexthops)
	err = libovsdbops.DeleteNextHopsFromLogicalRouterPolicies(oc.nbClient, oc.GetNetworkScopedClusterRouterName(), logicalRouterPolicyStaleNexthops...)
	if err != nil {
		return fmt.Errorf("unable to remove stale next hops from logical router policies: %v", err)
	}

	return nil
}

// This function implements a portion of syncEgressIPs.
// It removes OVN NAT rules used by EgressIPs deleted while ovnkube-master was down.
// Upon failure, it may be invoked multiple times in order to avoid a pod restart.
func (oc *DefaultNetworkController) syncStaleSNATRules(egressIPCache map[string]egressIPCacheEntry) error {
	predicate := func(item *nbdb.NAT) bool {
		egressIPName, exists := item.ExternalIDs["name"]
		// Exclude rows that have no name or are not the right type
		if !exists || item.Type != nbdb.NATTypeSNAT {
			return false
		}
		parsedLogicalIP := net.ParseIP(item.LogicalIP).String()
		cacheEntry, exists := egressIPCache[egressIPName]
		egressPodIPs := sets.NewString()
		if exists {
			// since SNATs can be present either if status.Node was local to
			// the zone or pods were local to the zone, we need to check both
			for _, podIPs := range cacheEntry.egressLocalPods {
				egressPodIPs.Insert(podIPs.UnsortedList()...)
			}
			for _, podIPs := range cacheEntry.egressRemotePods {
				egressPodIPs.Insert(podIPs.UnsortedList()...)
			}
		}
		if !exists || !egressPodIPs.Has(parsedLogicalIP) {
			klog.Infof("syncStaleSNATRules will delete %s due to logical ip: %v", egressIPName, item)
			return true
		}
		if node, ok := cacheEntry.egressIPs[item.ExternalIP]; !ok || !cacheEntry.egressLocalNodes.Has(node) ||
			item.LogicalPort == nil || *item.LogicalPort != oc.GetNetworkScopedK8sMgmtIntfName(node) {
			klog.Infof("syncStaleSNATRules will delete %s due to external ip or stale logical port: %v", egressIPName, item)
			return true
		}
		return false
	}

	nats, err := libovsdbops.FindNATsWithPredicate(oc.nbClient, predicate)
	if err != nil {
		return fmt.Errorf("unable to sync egress IPs err: %v", err)
	}

	if len(nats) == 0 {
		// No stale nat entries to deal with: noop.
		return nil
	}

	natIds := sets.Set[string]{}
	for _, nat := range nats {
		natIds.Insert(nat.UUID)
	}
	p := func(item *nbdb.LogicalRouter) bool {
		return natIds.HasAny(item.Nat...)
	}
	routers, err := libovsdbops.FindLogicalRoutersWithPredicate(oc.nbClient, p)
	if err != nil {
		return fmt.Errorf("unable to sync egress IPs, err: %v", err)
	}

	var errors []error
	ops := []ovsdb.Operation{}
	for _, router := range routers {
		ops, err = libovsdbops.DeleteNATsOps(oc.nbClient, ops, router, nats...)
		if err != nil {
			errors = append(errors, fmt.Errorf("error deleting stale NAT from router %s: %v", router.Name, err))
			continue
		}
	}
	if len(errors) > 0 {
		return utilerrors.Join(errors...)
	}
	// The routers length 0 check is needed because some of ovnk master restart unit tests have
	// router object referring to SNAT's UUID string instead of actual UUID (though it may not
	// happen in real scenario). Hence this check is needed to delete those stale SNATs as well.
	if len(routers) == 0 {
		predicate := func(item *nbdb.NAT) bool {
			return natIds.Has(item.UUID)
		}
		ops, err = libovsdbops.DeleteNATsWithPredicateOps(oc.nbClient, ops, predicate)
		if err != nil {
			return fmt.Errorf("unable to delete stale SNATs err: %v", err)
		}
	}

	_, err = libovsdbops.TransactAndCheck(oc.nbClient, ops)
	if err != nil {
		return fmt.Errorf("error deleting stale NATs: %v", err)
	}
	return nil
}

// generateCacheForEgressIP builds a cache of egressIP name -> podIPs for fast
// access when syncing egress IPs. The Egress IP setup will return a lot of
// atomic items with the same general information repeated across most (egressIP
// name, logical IP defined for that name), hence use a cache to avoid round
// trips to the API server per item.
func (oc *DefaultNetworkController) generateCacheForEgressIP() (map[string]egressIPCacheEntry, error) {
	egressIPCache := make(map[string]egressIPCacheEntry)
	egressIPs, err := oc.watchFactory.GetEgressIPs()
	if err != nil {
		return nil, err
	}
	for _, egressIP := range egressIPs {
		egressIPCache[egressIP.Name] = egressIPCacheEntry{
			egressLocalPods:  make(map[string]sets.Set[string]),
			egressRemotePods: make(map[string]sets.Set[string]),
			gatewayRouterIPs: sets.New[string](), // can be transit switchIPs for interconnect multizone setup
			egressIPs:        map[string]string{},
			egressLocalNodes: sets.New[string](),
		}
		for _, status := range egressIP.Status.Items {
			var nextHopIP string
			isEgressIPv6 := utilnet.IsIPv6String(status.EgressIP)
			_, isLocalZoneEgressNode := oc.localZoneNodes.Load(status.Node)
			if isLocalZoneEgressNode {
				gatewayRouterIP, err := oc.eIPC.getGatewayRouterJoinIP(status.Node, isEgressIPv6)
				if err != nil {
					klog.Errorf("Unable to retrieve gateway IP for node: %s, protocol is IPv6: %v, err: %v", status.Node, isEgressIPv6, err)
					continue
				}
				nextHopIP = gatewayRouterIP.String()
				egressIPCache[egressIP.Name].egressLocalNodes.Insert(status.Node)
			} else {
				nextHopIP, err = oc.eIPC.getTransitIP(status.Node, isEgressIPv6)
				if err != nil {
					klog.Errorf("Unable to fetch transit switch IP for node %s: %v", status.Node, err)
					continue
				}
			}
			egressIPCache[egressIP.Name].gatewayRouterIPs.Insert(nextHopIP)
			egressIPCache[egressIP.Name].egressIPs[status.EgressIP] = status.Node
		}
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
				if util.PodCompleted(pod) {
					continue
				}
				if len(egressIPCache[egressIP.Name].egressLocalNodes) == 0 && !oc.isPodScheduledinLocalZone(pod) {
					continue // don't process anything on master's that have nothing to do with the pod
				}
				// FIXME(trozet): potential race where pod is not yet added in the cache by the pod handler
				logicalPort, err := oc.logicalPortCache.get(pod, types.DefaultNetworkName)
				if err != nil {
					klog.Errorf("Error getting logical port %s, err: %v", util.GetLogicalPortName(pod.Namespace, pod.Name), err)
					continue
				}
				podKey := getPodKey(pod)
				if oc.isPodScheduledinLocalZone(pod) {
					_, ok := egressIPCache[egressIP.Name].egressLocalPods[podKey]
					if !ok {
						egressIPCache[egressIP.Name].egressLocalPods[podKey] = sets.New[string]()
					}
					for _, ipNet := range logicalPort.ips {
						egressIPCache[egressIP.Name].egressLocalPods[podKey].Insert(ipNet.IP.String())
					}
				} else if len(egressIPCache[egressIP.Name].egressLocalNodes) > 0 {
					// it means this controller has at least one egressNode that is in localZone but matched pod is remote
					_, ok := egressIPCache[egressIP.Name].egressRemotePods[podKey]
					if !ok {
						egressIPCache[egressIP.Name].egressRemotePods[podKey] = sets.New[string]()
					}
					for _, ipNet := range logicalPort.ips {
						egressIPCache[egressIP.Name].egressRemotePods[podKey].Insert(ipNet.IP.String())
					}
				}
			}
		}
	}

	return egressIPCache, nil
}

type EgressIPPatchStatus struct {
	Op    string                    `json:"op"`
	Path  string                    `json:"path"`
	Value egressipv1.EgressIPStatus `json:"value"`
}

// patchReplaceEgressIPStatus performs a replace patch operation of the egress
// IP status by replacing the status with the provided value. This allows us to
// update only the status field, without overwriting any other. This is
// important because processing egress IPs can take a while (when running on a
// public cloud and in the worst case), hence we don't want to perform a full
// object update which risks resetting the EgressIP object's fields to the state
// they had when we started processing the change.
// used for UNIT TESTING only
func (oc *DefaultNetworkController) patchReplaceEgressIPStatus(name string, statusItems []egressipv1.EgressIPStatusItem) error {
	klog.Infof("Patching status on EgressIP %s: %v", name, statusItems)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		t := []EgressIPPatchStatus{
			{
				Op:   "replace",
				Path: "/status",
				Value: egressipv1.EgressIPStatus{
					Items: statusItems,
				},
			},
		}
		op, err := json.Marshal(&t)
		if err != nil {
			return fmt.Errorf("error serializing status patch operation: %+v, err: %v", statusItems, err)
		}
		return oc.kube.PatchEgressIP(name, op)
	})
}

func (oc *DefaultNetworkController) addEgressNode(node *v1.Node) error {
	if node == nil {
		return nil
	}
	if oc.isLocalZoneNode(node) {
		klog.V(5).Infof("Egress node: %s about to be initialized", node.Name)
		if config.OVNKubernetesFeature.EnableInterconnect && oc.zone != types.OvnDefaultZone {
			// NOTE: EgressIP is not supported on multi-nodes-in-same-zone case
			// NOTE2: We don't want this route for all-nodes-in-same-zone (almost nonIC a.k.a single zone) case because
			// it makes no sense - all nodes are connected via the same ovn_cluster_router
			// NOTE3: When the node gets deleted we do not remove this route intentionally because
			// on IC if the node is gone, then the ovn_cluster_router is also gone along with all
			// the routes on it.
			if err := libovsdbutil.CreateDefaultRouteToExternal(oc.nbClient, oc.GetNetworkScopedClusterRouterName(), oc.GetNetworkScopedGWRouterName(node.Name)); err != nil {
				return err
			}
		}
	}
	return nil
}

// initClusterEgressPolicies will initialize the default allow policies for
// east<->west traffic. Egress IP is based on routing egress traffic to specific
// egress nodes, we don't want to route any other traffic however and these
// policies will take care of that. Also, we need to initialize the go-routine
// which verifies if egress nodes are ready and reachable, typically if an
// egress node experiences problems we want to move all egress IP assignment
// away from that node elsewhere so that the pods using the egress IP can
// continue to do so without any issues.
func (oc *DefaultNetworkController) initClusterEgressPolicies(nodes []interface{}) error {
	if err := InitClusterEgressPolicies(oc.nbClient, oc.addressSetFactory, oc.controllerName, oc.GetNetworkScopedClusterRouterName()); err != nil {
		return err
	}
	for _, node := range nodes {
		node := node.(*kapi.Node)

		if err := DeleteLegacyDefaultNoRerouteNodePolicies(oc.nbClient, oc.GetNetworkScopedClusterRouterName(), node.Name); err != nil {
			return err
		}
	}
	return nil
}

// InitClusterEgressPolicies creates the global no reroute policies and address-sets
// required by the egressIP and egressServices features.
func InitClusterEgressPolicies(nbClient libovsdbclient.Client, addressSetFactory addressset.AddressSetFactory,
	controllerName, clusterRouter string) error {
	v4ClusterSubnet, v6ClusterSubnet := util.GetClusterSubnets()
	if err := createDefaultNoReroutePodPolicies(nbClient, clusterRouter, v4ClusterSubnet, v6ClusterSubnet); err != nil {
		return err
	}
	if err := createDefaultNoRerouteServicePolicies(nbClient, clusterRouter, v4ClusterSubnet, v6ClusterSubnet); err != nil {
		return err
	}
	if err := createDefaultNoRerouteReplyTrafficPolicy(nbClient, clusterRouter); err != nil {
		return err
	}

	// ensure the address-set for storing nodeIPs exists
	dbIDs := getEgressIPAddrSetDbIDs(NodeIPAddrSetName, controllerName)
	if _, err := addressSetFactory.EnsureAddressSet(dbIDs); err != nil {
		return fmt.Errorf("cannot ensure that addressSet %s exists %v", NodeIPAddrSetName, err)
	}

	// ensure the address-set for storing egressIP pods exists
	dbIDs = getEgressIPAddrSetDbIDs(EgressIPServedPodsAddrSetName, controllerName)
	_, err := addressSetFactory.EnsureAddressSet(dbIDs)
	if err != nil {
		return fmt.Errorf("cannot ensure that addressSet for egressIP pods %s exists %v", EgressIPServedPodsAddrSetName, err)
	}

	// ensure the address-set for storing egressservice pod backends exists
	dbIDs = egresssvc.GetEgressServiceAddrSetDbIDs(controllerName)
	_, err = addressSetFactory.EnsureAddressSet(dbIDs)
	if err != nil {
		return fmt.Errorf("cannot ensure that addressSet for egressService pods %s exists %v", egresssvc.EgressServiceServedPodsAddrSetName, err)
	}

	return nil
}

type statusMap map[egressipv1.EgressIPStatusItem]string

type egressStatuses struct {
	statusMap
}

func (e egressStatuses) contains(potentialStatus egressipv1.EgressIPStatusItem) bool {
	// handle the case where the all of the status fields are populated
	if _, exists := e.statusMap[potentialStatus]; exists {
		return true
	}
	return false
}

func (e egressStatuses) delete(deleteStatus egressipv1.EgressIPStatusItem) {
	delete(e.statusMap, deleteStatus)
}

// podAssignmentState keeps track of which egressIP object is serving
// the related pod.
// NOTE: At a given time only one object will be configured. This is
// transparent to the user
type podAssignmentState struct {
	// the name of the egressIP object that is currently serving this pod
	egressIPName string
	// the list of egressIPs within the above egressIP object that are serving this pod

	egressStatuses

	// list of other egressIP object names that also match this pod but are on standby
	standbyEgressIPNames sets.Set[string]
}

// Clone deep-copies and returns the copied podAssignmentState
func (pas *podAssignmentState) Clone() *podAssignmentState {
	clone := &podAssignmentState{
		egressIPName:         pas.egressIPName,
		standbyEgressIPNames: pas.standbyEgressIPNames.Clone(),
	}
	clone.egressStatuses = egressStatuses{make(map[egressipv1.EgressIPStatusItem]string, len(pas.egressStatuses.statusMap))}
	for k, v := range pas.statusMap {
		clone.statusMap[k] = v
	}
	return clone
}

type egressIPZoneController struct {
	// network information
	util.NetInfo

	// podAssignmentMutex is used to ensure safe access to podAssignment.
	// Currently WatchEgressIP, WatchEgressNamespace and WatchEgressPod could
	// all access that map simultaneously, hence why this guard is needed.
	podAssignmentMutex *sync.Mutex
	// nodeUpdateMutex is used for two reasons:
	// (1) to ensure safe handling of node ip address updates. VIP addresses are
	// dynamic and might move across nodes.
	// (2) used in ensureDefaultNoRerouteQoSRules function to ensure
	// creating QoS rules is thread safe since otherwise when two nodes are added
	// at the same time by two different threads we end up creating duplicate
	// QoS rules in database due to libovsdb cache race
	nodeUpdateMutex *sync.Mutex
	// podAssignment is a cache used for keeping track of which egressIP status
	// has been setup for each pod. The key is defined by getPodKey
	podAssignment map[string]*podAssignmentState
	// libovsdb northbound client interface
	nbClient libovsdbclient.Client
	// watchFactory watching k8s objects
	watchFactory      *factory.WatchFactory
	retryEgressIPs    *ovnretry.RetryFramework
	networkManager    networkmanager.Interface
	networkReconciler controller.Reconciler
	// A cache that maintains all nodes in the cluster,
	// value will be true if local to this zone and false otherwise
	nodeZoneState *syncmap.SyncMap[bool]
}

func newEgressIPZoneController(
	nbClient libovsdbclient.Client,
	watchFactory *factory.WatchFactory,
	networkManager networkmanager.Interface,
	retryEgressIPs *ovnretry.RetryFramework,
) *egressIPZoneController {
	c := &egressIPZoneController{
		NetInfo:            &util.DefaultNetInfo{},
		nodeUpdateMutex:    &sync.Mutex{},
		podAssignmentMutex: &sync.Mutex{},
		podAssignment:      make(map[string]*podAssignmentState),
		nodeZoneState:      syncmap.NewSyncMap[bool](),
		nbClient:           nbClient,
		watchFactory:       watchFactory,
		networkManager:     networkManager,
		retryEgressIPs:     retryEgressIPs,
	}

	if !egressip.AdvertisementsEnabled() {
		return c
	}

	config := &controller.ReconcilerConfig{
		RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:   c.reconcileNetwork,
		Threadiness: 1,
	}
	c.networkReconciler = controller.NewReconciler(
		"EgressIP zone network controller",
		config,
	)

	return c
}

// addStandByEgressIPAssignment does the same setup that is done by addPodEgressIPAssignments but for
// the standby egressIP. This must always be called with a lock on podAssignmentState mutex
// This is special case function called only from deleteEgressIPAssignments, don't use this for normal setup
// Any failure from here will not be retried, its a corner case undefined behaviour
func (oc *DefaultNetworkController) addStandByEgressIPAssignment(podKey string, podStatus *podAssignmentState) error {
	podNamespace, podName := getPodNamespaceAndNameFromKey(podKey)
	pod, err := oc.watchFactory.GetPod(podNamespace, podName)
	if err != nil {
		return err
	}
	eipsToAssign := podStatus.standbyEgressIPNames.UnsortedList()
	var eipToAssign string
	var eip *egressipv1.EgressIP
	for _, eipName := range eipsToAssign {
		eip, err = oc.watchFactory.GetEgressIP(eipName)
		if err != nil {
			klog.Warningf("There seems to be a stale standby egressIP %s for pod %s "+
				"which doesn't exist: %v; removing this standby egressIP from cache...", eipName, podKey, err)
			podStatus.standbyEgressIPNames.Delete(eipName)
			continue
		}
		eipToAssign = eipName // use the first EIP we find successfully
		break
	}
	if eipToAssign == "" {
		klog.Infof("No standby egressIP's found for pod %s", podKey)
		return nil
	}

	podState := &podAssignmentState{
		egressStatuses:       egressStatuses{make(map[egressipv1.EgressIPStatusItem]string)},
		standbyEgressIPNames: podStatus.standbyEgressIPNames,
	}
	oc.eIPC.podAssignment[podKey] = podState
	// NOTE: We let addPodEgressIPAssignments take care of setting egressIPName and egressStatuses and removing it from standBy
	err = oc.addPodEgressIPAssignments(eipToAssign, eip.Status.Items, pod)
	if err != nil {
		return err
	}
	return nil
}

// addPodEgressIPAssignment will program OVN with logical router policies
// (routing pod traffic to the egress node) and NAT objects on the egress node
// (SNAT-ing to the egress IP).
// This function should be called with lock on nodeZoneState cache key status.Node and pod.Spec.NodeName
func (e *egressIPZoneController) addPodEgressIPAssignment(egressIPName string, status egressipv1.EgressIPStatusItem, pod *kapi.Pod, podIPs []*net.IPNet) (err error) {
	if config.Metrics.EnableScaleMetrics {
		start := time.Now()
		defer func() {
			if err != nil {
				return
			}
			duration := time.Since(start)
			metrics.RecordEgressIPAssign(duration)
		}()
	}
	eIPIP := net.ParseIP(status.EgressIP)
	isLocalZoneEgressNode, loadedEgressNode := e.nodeZoneState.Load(status.Node)
	isLocalZonePod, loadedPodNode := e.nodeZoneState.Load(pod.Spec.NodeName)
	eNode, err := e.watchFactory.GetNode(status.Node)
	if err != nil {
		return fmt.Errorf("failed to add pod %s/%s because failed to lookup node %s: %v", pod.Namespace, pod.Name,
			pod.Spec.NodeName, err)
	}
	parsedNodeEIPConfig, err := egressip.GetNodeEIPConfig(eNode)
	if err != nil {
		return fmt.Errorf("failed to get node %s egress IP config: %w", eNode.Name, err)
	}
	isOVNManaged, err := egressip.IsEgressIPPrimaryOrAdvertised(e.watchFactory, e.networkManager, parsedNodeEIPConfig, eNode, egressIPName, eIPIP)
	if err != nil {
		return err
	}
	nextHopIP, err := e.getNextHop(status.Node, status.EgressIP, egressIPName, isLocalZoneEgressNode, isOVNManaged)
	if err != nil || nextHopIP == "" {
		return fmt.Errorf("failed to determine next hop for pod %s/%s when configuring egress IP %s"+
			" IP %s: %v", pod.Namespace, pod.Name, egressIPName, status.EgressIP, err)
	}
	var ops []ovsdb.Operation
	if loadedEgressNode && isLocalZoneEgressNode {
		if isOVNManaged {
			ops, err = e.createNATRuleOps(nil, podIPs, status, egressIPName)
			if err != nil {
				return fmt.Errorf("unable to create NAT rule ops for status: %v, err: %v", status, err)
			}
		}
		if config.OVNKubernetesFeature.EnableInterconnect && !isOVNManaged && (loadedPodNode && !isLocalZonePod) {
			// configure reroute for non-local-zone pods on egress nodes
			ops, err = e.createReroutePolicyOps(ops, podIPs, status, egressIPName, nextHopIP)
			if err != nil {
				return fmt.Errorf("unable to create logical router policy ops %v, err: %v", status, err)
			}
		}
	}

	// exec when node is local OR when pods are local
	// don't add a reroute policy if the egress node towards which we are adding this doesn't exist
	if loadedEgressNode && loadedPodNode && isLocalZonePod {
		ops, err = e.createReroutePolicyOps(ops, podIPs, status, egressIPName, nextHopIP)
		if err != nil {
			return fmt.Errorf("unable to create logical router policy ops, err: %v", err)
		}
		ops, err = e.deleteExternalGWPodSNATOps(ops, pod, podIPs, status, isOVNManaged)
		if err != nil {
			return err
		}
	}
	_, err = libovsdbops.TransactAndCheck(e.nbClient, ops)
	return err
}

// deletePodEgressIPAssignment deletes the OVN programmed egress IP
// configuration mentioned for addPodEgressIPAssignment.
// This function should be called with lock on nodeZoneState cache key status.Node and pod.Spec.NodeName
func (e *egressIPZoneController) deletePodEgressIPAssignment(egressIPName string, status egressipv1.EgressIPStatusItem, pod *kapi.Pod, podIPs []*net.IPNet) (err error) {
	if config.Metrics.EnableScaleMetrics {
		start := time.Now()
		defer func() {
			if err != nil {
				return
			}
			duration := time.Since(start)
			metrics.RecordEgressIPUnassign(duration)
		}()
	}
	isLocalZoneEgressNode, loadedEgressNode := e.nodeZoneState.Load(status.Node)
	isLocalZonePod, loadedPodNode := e.nodeZoneState.Load(pod.Spec.NodeName)
	var nextHopIP string
	var isOVNManaged bool
	// node may not exist - attempt to retrieve it
	eNode, err := e.watchFactory.GetNode(status.Node)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to delete pod %s/%s egress IP config because failed to lookup node %s obj: %v", pod.Namespace, pod.Name,
			pod.Spec.NodeName, err)
	}
	if err == nil {
		eIPIP := net.ParseIP(status.EgressIP)
		parsedEIPConfig, err := egressip.GetNodeEIPConfig(eNode)
		if err != nil {
			klog.Warningf("Unable to get node %s egress IP config: %v", eNode.Name, err)
		} else {
			isOVNManaged, err = egressip.IsEgressIPPrimaryOrAdvertised(e.watchFactory, e.networkManager, parsedEIPConfig, eNode, egressIPName, eIPIP)
			if err != nil {
				return err
			}
			nextHopIP, err = e.getNextHop(status.Node, status.EgressIP, egressIPName, isLocalZoneEgressNode, isOVNManaged)
			if err != nil {
				klog.Warningf("Unable to determine next hop for egress IP %s IP %s assigned to node %s: %v", egressIPName,
					status.EgressIP, status.Node, err)
			}
		}
	}

	var ops []ovsdb.Operation
	if !loadedPodNode || isLocalZonePod { // node is deleted (we can't determine zone so we always try and nuke OR pod is local to zone)
		ops, err = e.addExternalGWPodSNATOps(nil, pod.Namespace, pod.Name, status)
		if err != nil {
			return err
		}
		ops, err = e.deleteReroutePolicyOps(ops, podIPs, status, egressIPName, nextHopIP)
		if errors.Is(err, libovsdbclient.ErrNotFound) {
			// if the gateway router join IP setup is already gone, then don't count it as error.
			klog.Warningf("Unable to delete logical router policy, err: %v", err)
		} else if err != nil {
			return fmt.Errorf("unable to delete logical router policy, err: %v", err)
		}
	}

	if loadedEgressNode && isLocalZoneEgressNode {
		if config.OVNKubernetesFeature.EnableInterconnect && !isOVNManaged && (!loadedPodNode || !isLocalZonePod) { // node is deleted (we can't determine zone so we always try and nuke OR pod is remote to zone)
			// delete reroute for non-local-zone pods on egress nodes
			ops, err = e.deleteReroutePolicyOps(ops, podIPs, status, egressIPName, nextHopIP)
			if err != nil {
				return fmt.Errorf("unable to delete logical router static route ops %v, err: %v", status, err)
			}
		}
		ops, err = e.deleteNATRuleOps(ops, podIPs, status, egressIPName)
		if err != nil {
			return fmt.Errorf("unable to delete NAT rule for status: %v, err: %v", status, err)
		}
	}
	_, err = libovsdbops.TransactAndCheck(e.nbClient, ops)
	return err
}

// addExternalGWPodSNAT performs the required external GW setup in two particular
// cases:
// - An egress IP matching pod stops matching by means of EgressIP object
// deletion
// - An egress IP matching pod stops matching by means of changed EgressIP
// selector change.
// In both cases we should re-add the external GW setup. We however need to
// guard against a third case, which is: pod deletion, for that it's enough to
// check the informer cache since on pod deletion the event handlers are
// triggered after the update to the informer cache. We should not re-add the
// external GW setup in those cases.
func (e *egressIPZoneController) addExternalGWPodSNAT(podNamespace, podName string, status egressipv1.EgressIPStatusItem) error {
	ops, err := e.addExternalGWPodSNATOps(nil, podNamespace, podName, status)
	if err != nil {
		return fmt.Errorf("error creating ops for adding external gw pod snat: %+v", err)
	}
	_, err = libovsdbops.TransactAndCheck(e.nbClient, ops)
	if err != nil {
		return fmt.Errorf("error trasnsacting ops %+v: %v", ops, err)
	}
	return nil
}

// addExternalGWPodSNATOps returns ovsdb ops that perform the required external GW setup in two particular
// cases:
// - An egress IP matching pod stops matching by means of EgressIP object
// deletion
// - An egress IP matching pod stops matching by means of changed EgressIP
// selector change.
// In both cases we should re-add the external GW setup. We however need to
// guard against a third case, which is: pod deletion, for that it's enough to
// check the informer cache since on pod deletion the event handlers are
// triggered after the update to the informer cache. We should not re-add the
// external GW setup in those cases.
// This function should be called with lock on nodeZoneState cache key pod.Spec.Name
func (e *egressIPZoneController) addExternalGWPodSNATOps(ops []ovsdb.Operation, podNamespace, podName string, status egressipv1.EgressIPStatusItem) ([]ovsdb.Operation, error) {
	if config.Gateway.DisableSNATMultipleGWs {
		pod, err := e.watchFactory.GetPod(podNamespace, podName)
		if err != nil {
			return nil, nil // nothing to do.
		}

		if util.IsPodNetworkAdvertisedAtNode(e, pod.Spec.NodeName) {
			// network is advertised so don't setup the SNAT
			return ops, nil
		}

		isLocalZonePod, loadedPodNode := e.nodeZoneState.Load(pod.Spec.NodeName)
		if pod.Spec.NodeName == status.Node && loadedPodNode && isLocalZonePod && util.PodNeedsSNAT(pod) {
			// if the pod still exists, add snats to->nodeIP (on the node where the pod exists) for these podIPs after deleting the snat to->egressIP
			// NOTE: This needs to be done only if the pod was on the same node as egressNode
			extIPs, err := getExternalIPsGR(e.watchFactory, pod.Spec.NodeName)
			if err != nil {
				return nil, err
			}
			podIPs, err := util.GetPodCIDRsWithFullMask(pod, &util.DefaultNetInfo{})
			if err != nil {
				return nil, err
			}
			ops, err = addOrUpdatePodSNATOps(e.nbClient, e.GetNetworkScopedGWRouterName(pod.Spec.NodeName), extIPs, podIPs, "", ops)
			if err != nil {
				return nil, err
			}
			klog.V(5).Infof("Adding SNAT on %s since egress node managing %s/%s was the same: %s", pod.Spec.NodeName, pod.Namespace, pod.Name, status.Node)
		}
	}
	return ops, nil
}

// deleteExternalGWPodSNATOps creates ops for the required external GW teardown for the given pod
func (e *egressIPZoneController) deleteExternalGWPodSNATOps(ops []ovsdb.Operation, pod *kapi.Pod, podIPs []*net.IPNet, status egressipv1.EgressIPStatusItem, isOVNManaged bool) ([]ovsdb.Operation, error) {
	if config.Gateway.DisableSNATMultipleGWs && status.Node == pod.Spec.NodeName && isOVNManaged {
		// remove snats to->nodeIP (from the node where pod exists if that node is also serving
		// as an egress node for this pod) for these podIPs before adding the snat to->egressIP
		extIPs, err := getExternalIPsGR(e.watchFactory, pod.Spec.NodeName)
		if err != nil {
			return nil, err
		}
		ops, err = deletePodSNATOps(e.nbClient, ops, e.GetNetworkScopedGWRouterName(pod.Spec.NodeName), extIPs, podIPs, "")
		if err != nil {
			return nil, err
		}
	} else if config.Gateway.DisableSNATMultipleGWs {
		// it means the pod host is different from the egressNode that is managing the pod
		klog.V(5).Infof("Not deleting SNAT on %s since egress node managing %s/%s is %s or Egress IP is not SNAT'd by OVN", pod.Spec.NodeName, pod.Namespace, pod.Name, status.Node)
	}
	return ops, nil
}

func (e *egressIPZoneController) getGatewayRouterJoinIP(node string, wantsIPv6 bool) (net.IP, error) {
	gatewayIPs, err := libovsdbutil.GetLRPAddrs(e.nbClient, types.GWRouterToJoinSwitchPrefix+e.GetNetworkScopedGWRouterName(node))
	if err != nil {
		return nil, fmt.Errorf("attempt at finding node gateway router network information failed, err: %w", err)
	}
	if gatewayIP, err := util.MatchFirstIPNetFamily(wantsIPv6, gatewayIPs); err != nil {
		return nil, fmt.Errorf("could not find gateway IP for node %s with family %v: %v", node, wantsIPv6, err)
	} else {
		return gatewayIP.IP, nil
	}
}

// ipFamilyName returns IP family name based on the provided flag
func ipFamilyName(isIPv6 bool) string {
	if isIPv6 {
		return string(IPFamilyValueV6)
	}
	return string(IPFamilyValueV4)
}

func (e *egressIPZoneController) getTransitIP(nodeName string, wantsIPv6 bool) (string, error) {
	// fetch node annotation of the egress node
	node, err := e.watchFactory.GetNode(nodeName)
	if err != nil {
		return "", fmt.Errorf("failed to get node %s: %w", nodeName, err)
	}
	nodeTransitIPs, err := util.ParseNodeTransitSwitchPortAddrs(node)
	if err != nil {
		return "", fmt.Errorf("unable to fetch transit switch IP for node %s: %w", nodeName, err)
	}
	nodeTransitIP, err := util.MatchFirstIPNetFamily(wantsIPv6, nodeTransitIPs)
	if err != nil {
		return "", fmt.Errorf("could not find transit switch IP of node %v for this family %v: %v", node, wantsIPv6, err)
	}
	return nodeTransitIP.IP.String(), nil
}

// getNextHop attempts to determine whether an egress IP should be routed through the primary OVN network or through
// a secondary host network. If we failed to look up the information required to determine this, an error will be returned
// however if we are able to lookup the information, but it doesnt exist, called must be able to tolerate a blank next hop
// and no error returned. This means we searched successfully but could not find the information required to generate the next hop.
func (e *egressIPZoneController) getNextHop(egressNodeName, egressIP, egressIPName string, isLocalZoneEgressNode, isOVNManaged bool) (string, error) {
	var nextHopIP string
	var err error
	isEgressIPv6 := utilnet.IsIPv6String(egressIP)
	// NOTE: No need to check if status.node exists or not in the cache, we are calling this function only if it
	// is present in the nodeZoneState cache. Since we call it with lock on cache, we are safe here.
	if isLocalZoneEgressNode {
		if isOVNManaged {
			gatewayRouterIP, err := e.getGatewayRouterJoinIP(egressNodeName, isEgressIPv6)
			if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
				return "", fmt.Errorf("unable to retrieve gateway IP for node: %s, protocol is IPv6: %v, err: %w",
					egressNodeName, isEgressIPv6, err)
			} else if err != nil {
				klog.Warningf("While attempting to get next hop for Egress IP %s (%s), unable to get gateway "+
					"router join IP: %v", egressIPName, egressIP, err)
				return "", nil
			}
			nextHopIP = gatewayRouterIP.String()
		} else {
			mgmtPort := &nbdb.LogicalSwitchPort{Name: e.GetNetworkScopedK8sMgmtIntfName(egressNodeName)}
			mgmtPort, err := libovsdbops.GetLogicalSwitchPort(e.nbClient, mgmtPort)
			if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
				return "", fmt.Errorf("failed to get next hop IP for secondary host network and egress IP %s for node %s "+
					"because unable to get management port: %v", egressIPName, egressNodeName, err)
			} else if err != nil {
				klog.Warningf("While attempting to get next hop for Egress IP %s (%s), unable to get management switch port: %v",
					egressIPName, egressIP, err)
				return "", nil
			}
			mgmtPortAddresses := mgmtPort.GetAddresses()
			if len(mgmtPortAddresses) == 0 {
				return "", fmt.Errorf("failed to get next hop IP for secondary host network and egress IP %s for node %s"+
					"because management switch port does not contain any addresses", egressIPName, egressNodeName)
			}
			for _, mgmtPortAddress := range mgmtPortAddresses {
				mgmtPortAddressesStr := strings.Fields(mgmtPortAddress)
				mgmtPortIP := net.ParseIP(mgmtPortAddressesStr[1])
				if isEgressIPv6 && utilnet.IsIPv6(mgmtPortIP) {
					nextHopIP = mgmtPortIP.To16().String()
				} else {
					nextHopIP = mgmtPortIP.To4().String()
				}
			}
		}
	} else if config.OVNKubernetesFeature.EnableInterconnect {
		// fetch node annotation of the egress node
		nextHopIP, err = e.getTransitIP(egressNodeName, isEgressIPv6)
		if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
			return "", fmt.Errorf("unable to fetch transit switch IP for node %s: %v", egressNodeName, err)
		} else if err != nil {
			klog.Warningf("While attempting to get next hop for Egress IP %s (%s), unable to get transit switch IP: %v",
				egressIPName, egressIP, err)
		}
	}
	return nextHopIP, nil
}

// createReroutePolicyOps creates an operation that does idempotent updates of the
// LogicalRouterPolicy corresponding to the egressIP status item, according to the
// following update procedure for when EIP is hosted by a OVN network:
// - if the LogicalRouterPolicy does not exist: it adds it by creating the
// reference to it from ovn_cluster_router and specifying the array of nexthops
// to equal [gatewayRouterIP]
// - if the LogicalRouterPolicy does exist: it adds the gatewayRouterIP to the
// array of nexthops
// For EIP hosted on secondary host network, logical route policies are needed
// to redirect the pods to the appropriate management port or if interconnect is
// enabled, the appropriate transit switch port.
// This function should be called with lock on nodeZoneState cache key status.Node
func (e *egressIPZoneController) createReroutePolicyOps(ops []ovsdb.Operation, podIPNets []*net.IPNet, status egressipv1.EgressIPStatusItem, egressIPName, nextHopIP string) ([]ovsdb.Operation, error) {
	isEgressIPv6 := utilnet.IsIPv6String(status.EgressIP)
	var err error
	// Handle all pod IPs that match the egress IP address family
	for _, podIPNet := range util.MatchAllIPNetFamily(isEgressIPv6, podIPNets) {
		lrp := nbdb.LogicalRouterPolicy{
			Match:    fmt.Sprintf("%s.src == %s", ipFamilyName(isEgressIPv6), podIPNet.IP.String()),
			Priority: types.EgressIPReroutePriority,
			Nexthops: []string{nextHopIP},
			Action:   nbdb.LogicalRouterPolicyActionReroute,
			ExternalIDs: map[string]string{
				"name": egressIPName,
			},
		}
		p := func(item *nbdb.LogicalRouterPolicy) bool {
			return item.Match == lrp.Match && item.Priority == lrp.Priority && item.ExternalIDs["name"] == lrp.ExternalIDs["name"]
		}

		ops, err = libovsdbops.CreateOrAddNextHopsToLogicalRouterPolicyWithPredicateOps(e.nbClient, ops, e.GetNetworkScopedClusterRouterName(), &lrp, p)
		if err != nil {
			return nil, fmt.Errorf("error creating logical router policy %+v on router %s: %v", lrp, e.GetNetworkScopedClusterRouterName(), err)
		}
	}
	return ops, nil
}

// deleteReroutePolicyOps creates an operation that does idempotent updates of the
// LogicalRouterPolicy corresponding to the egressIP object, according to the
// following update procedure:
// - if the LogicalRouterPolicy exist and has the len(nexthops) > 1: it removes
// the specified gatewayRouterIP from nexthops
// - if the LogicalRouterPolicy exist and has the len(nexthops) == 1: it removes
// the LogicalRouterPolicy completely
// if caller fails to find a next hop, we clear the LRPs for that specific Egress IP
// which will break HA momentarily
// This function should be called with lock on nodeZoneState cache key status.Node
func (e *egressIPZoneController) deleteReroutePolicyOps(ops []ovsdb.Operation, podIPNets []*net.IPNet, status egressipv1.EgressIPStatusItem, egressIPName, nextHopIP string) ([]ovsdb.Operation, error) {
	isEgressIPv6 := utilnet.IsIPv6String(status.EgressIP)
	var err error
	// Handle all pod IPs that match the egress IP address family
	for _, podIPNet := range util.MatchAllIPNetFamily(isEgressIPv6, podIPNets) {
		filterOption := fmt.Sprintf("%s.src == %s", ipFamilyName(isEgressIPv6), podIPNet.IP.String())
		p := func(item *nbdb.LogicalRouterPolicy) bool {
			return item.Match == filterOption && item.Priority == types.EgressIPReroutePriority && item.ExternalIDs["name"] == egressIPName
		}
		if nextHopIP != "" {
			ops, err = libovsdbops.DeleteNextHopFromLogicalRouterPoliciesWithPredicateOps(e.nbClient, ops, e.GetNetworkScopedClusterRouterName(), p, nextHopIP)
			if err != nil {
				return nil, fmt.Errorf("error removing nexthop IP %s from egress ip %s policies on router %s: %v",
					nextHopIP, egressIPName, e.GetNetworkScopedClusterRouterName(), err)
			}
		} else {
			klog.Errorf("Caller failed to pass next hop for EgressIP %s and IP %s. Deleting all LRPs. This will break HA momentarily",
				egressIPName, status.EgressIP)
			// since next hop was not found, delete everything to ensure no stale entries however this will break load
			// balancing between hops, but we offer no guarantees except one of the EIPs will work
			ops, err = libovsdbops.DeleteLogicalRouterPolicyWithPredicateOps(e.nbClient, ops, e.GetNetworkScopedClusterRouterName(), p)
			if err != nil {
				return nil, fmt.Errorf("failed to create logical router policy operations on ovn_cluster_router: %v", err)
			}
		}
	}
	return ops, nil
}

// deleteEgressIPStatusSetup deletes the entire set up in the NB DB for an
// EgressIPStatusItem. The set up in the NB DB gets tagged with the name of the
// EgressIP, hence lookup the LRP and NAT objects which match that as well as
// the attributes of the EgressIPStatusItem. Keep in mind: the LRP should get
// completely deleted once the remaining and last nexthop equals the
// gatewayRouterIP corresponding to the node in the EgressIPStatusItem, else
// just remove the gatewayRouterIP from the list of nexthops
// It also returns the list of podIPs whose routes and SNAT's were deleted
// This function should be called with a lock on e.nodeZoneState.status.Node
func (e *egressIPZoneController) deleteEgressIPStatusSetup(name string, status egressipv1.EgressIPStatusItem) ([]net.IP, error) {
	var err error
	var ops []ovsdb.Operation
	nextHopIP, err := e.attemptToGetNextHopIP(name, status)
	if err != nil {
		return nil, fmt.Errorf("failed to delete egress IP %s (%s) because unable to determine next hop: %v",
			name, status.EgressIP, err)
	}

	if nextHopIP != "" {
		policyPred := func(item *nbdb.LogicalRouterPolicy) bool {
			hasIPNexthop := false
			for _, nexthop := range item.Nexthops {
				if nexthop == nextHopIP {
					hasIPNexthop = true
					break
				}
			}
			return item.Priority == types.EgressIPReroutePriority && item.ExternalIDs["name"] == name && hasIPNexthop
		}
		ops, err = libovsdbops.DeleteNextHopFromLogicalRouterPoliciesWithPredicateOps(e.nbClient, ops, e.GetNetworkScopedClusterRouterName(), policyPred, nextHopIP)
		if err != nil {
			return nil, fmt.Errorf("error removing nexthop IP %s from egress ip %s policies on router %s: %v",
				nextHopIP, name, e.GetNetworkScopedClusterRouterName(), err)
		}
	} else if ops, err = e.ensureOnlyValidNextHops(name, ops); err != nil {
		return nil, err
	}

	isLocalZoneEgressNode, loadedEgressNode := e.nodeZoneState.Load(status.Node)
	var nats []*nbdb.NAT
	if loadedEgressNode && isLocalZoneEgressNode {
		routerName := e.GetNetworkScopedGWRouterName(status.Node)
		natPred := func(nat *nbdb.NAT) bool {
			// We should delete NATs only from the status.Node that was passed into this function
			return nat.ExternalIDs["name"] == name && nat.ExternalIP == status.EgressIP && nat.LogicalPort != nil && *nat.LogicalPort == e.GetNetworkScopedK8sMgmtIntfName(status.Node)
		}
		nats, err = libovsdbops.FindNATsWithPredicate(e.nbClient, natPred) // save the nats to get the podIPs before that nats get deleted
		if err != nil {
			return nil, fmt.Errorf("error removing egress ip pods from adress set %s: %v", EgressIPServedPodsAddrSetName, err)
		}
		ops, err = libovsdbops.DeleteNATsWithPredicateOps(e.nbClient, ops, natPred)
		if err != nil {
			return nil, fmt.Errorf("error removing egress ip %s nats on router %s: %v", name, routerName, err)
		}
	}
	_, err = libovsdbops.TransactAndCheck(e.nbClient, ops)
	if err != nil {
		return nil, fmt.Errorf("error transacting ops %+v: %v", ops, err)
	}
	var podIPs []net.IP
	for i := range nats {
		nat := nats[i]
		podIP := net.ParseIP(nat.LogicalIP)
		podIPs = append(podIPs, podIP)
	}

	return podIPs, nil
}

func (e *egressIPZoneController) ensureOnlyValidNextHops(name string, ops []ovsdb.Operation) ([]ovsdb.Operation, error) {
	// When no nextHopIP is found, This may happen when node object is already deleted.
	// So compare validNextHopIPs associated with current eIP.Status and Nexthops present
	// in the LogicalRouterPolicy, then delete nexthop(s) from LogicalRouterPolicy if
	// it doesn't match with nexthops derived from eIP.Status.
	policyPred := func(item *nbdb.LogicalRouterPolicy) bool {
		return item.Priority == types.EgressIPReroutePriority && item.ExternalIDs["name"] == name
	}
	eIP, err := e.watchFactory.GetEgressIP(name)
	if err != nil && !apierrors.IsNotFound(err) {
		return ops, fmt.Errorf("error retrieving EgressIP %s object for updating logical router policy nexthops, err: %w", name, err)
	} else if err != nil && apierrors.IsNotFound(err) {
		// EgressIP object is not found, so delete LRP associated with it.
		ops, err = libovsdbops.DeleteLogicalRouterPolicyWithPredicateOps(e.nbClient, ops, e.GetNetworkScopedClusterRouterName(), policyPred)
		if err != nil {
			return ops, fmt.Errorf("error creating ops to remove logical router policy for EgressIP %s from router %s: %v",
				name, e.GetNetworkScopedClusterRouterName(), err)
		}
	} else {
		validNextHopIPs := make(sets.Set[string])
		for _, validStatus := range eIP.Status.Items {
			nextHopIP, err := e.attemptToGetNextHopIP(name, validStatus)
			if err != nil {
				return ops, fmt.Errorf("failed to delete EgressIP %s (%s) because unable to determine next hop: %v",
					name, validStatus.EgressIP, err)
			}
			validNextHopIPs.Insert(nextHopIP)
		}

		reRoutePolicies, err := libovsdbops.FindLogicalRouterPoliciesWithPredicate(e.nbClient, policyPred)
		if err != nil {
			return ops, fmt.Errorf("error finding logical router policy for EgressIP %s: %v", name, err)
		}
		if len(validNextHopIPs) == 0 {
			ops, err = libovsdbops.DeleteLogicalRouterPoliciesOps(e.nbClient, ops, e.GetNetworkScopedClusterRouterName(), reRoutePolicies...)
			if err != nil {
				return ops, fmt.Errorf("error creating ops to remove logical router policy for EgressIP %s from router %s: %v",
					name, e.GetNetworkScopedClusterRouterName(), err)
			}
			return ops, nil
		}
		for _, policy := range reRoutePolicies {
			for _, nextHop := range policy.Nexthops {
				if validNextHopIPs.Has(nextHop) {
					continue
				}
				ops, err = libovsdbops.DeleteNextHopsFromLogicalRouterPolicyOps(e.nbClient, ops, e.GetNetworkScopedClusterRouterName(), []*nbdb.LogicalRouterPolicy{policy}, nextHop)
				if err != nil {
					return ops, fmt.Errorf("error creating ops to remove stale next hop IP %s from logical router policy for EgressIP %s from router %s: %v",
						nextHop, name, e.GetNetworkScopedClusterRouterName(), err)
				}
			}
		}
	}
	return ops, nil
}

// attemptToGetNextHopIP this function attempts to retrieve nexthops associated with logical router policy for the given
// EgressIP's status. It ensures the following conditions are met.
// 1) When node is not found, then it must return empty nexthop without an error.
// 2) When EgressIP belongs to OVN network and node is local, then it must return node's gateway router IP address.
// 3) When EgressIP belongs to non OVN network and node is local, then it must return node's management port IP address.
// 4) When EgressIP belongs to remote node in interconnect zone, then it return node's transit switch IP address.
func (e *egressIPZoneController) attemptToGetNextHopIP(name string, status egressipv1.EgressIPStatusItem) (string, error) {
	isLocalZoneEgressNode, _ := e.nodeZoneState.Load(status.Node)
	eNode, err := e.watchFactory.GetNode(status.Node)
	if err != nil && !apierrors.IsNotFound(err) {
		return "", fmt.Errorf("unable to get node for Egress IP %s: %v", status.EgressIP, err)
	} else if err != nil {
		klog.Errorf("Node is not found for Egress IP %s", status.EgressIP)
		return "", nil
	}
	var nextHopIP string
	eIPIP := net.ParseIP(status.EgressIP)
	if eIPConfig, err := egressip.GetNodeEIPConfig(eNode); err != nil {
		klog.Warningf("Failed to get Egress IP config from node annotation %s: %v", status.Node, err)
	} else {
		isOVNManaged, err := egressip.IsEgressIPPrimaryOrAdvertised(e.watchFactory, e.networkManager, eIPConfig, eNode, name, eIPIP)
		if err != nil {
			return "", err
		}
		nextHopIP, err = e.getNextHop(status.Node, status.EgressIP, name, isLocalZoneEgressNode, isOVNManaged)
		if err != nil {
			return "", err
		}
	}
	return nextHopIP, nil
}

func (oc *DefaultNetworkController) addPodIPsToAddressSet(addrSetIPs []net.IP) error {
	dbIDs := getEgressIPAddrSetDbIDs(EgressIPServedPodsAddrSetName, oc.controllerName)
	as, err := oc.addressSetFactory.GetAddressSet(dbIDs)
	if err != nil {
		return fmt.Errorf("cannot ensure that addressSet %s exists %v", EgressIPServedPodsAddrSetName, err)
	}
	if err := as.AddAddresses(util.StringSlice(addrSetIPs)); err != nil {
		return fmt.Errorf("cannot add egressPodIPs %v from the address set %v: err: %v", addrSetIPs, EgressIPServedPodsAddrSetName, err)
	}
	return nil
}

func (oc *DefaultNetworkController) deletePodIPsFromAddressSet(addrSetIPs []net.IP) error {
	dbIDs := getEgressIPAddrSetDbIDs(EgressIPServedPodsAddrSetName, oc.controllerName)
	as, err := oc.addressSetFactory.GetAddressSet(dbIDs)
	if err != nil {
		return fmt.Errorf("cannot ensure that addressSet %s exists %v", EgressIPServedPodsAddrSetName, err)
	}
	if err := as.DeleteAddresses(util.StringSlice(addrSetIPs)); err != nil {
		return fmt.Errorf("cannot delete egressPodIPs %v from the address set %v: err: %v", addrSetIPs, EgressIPServedPodsAddrSetName, err)
	}
	return nil
}

// createDefaultNoRerouteServicePolicies ensures service reachability from the
// host network to any service (except ETP=local) backed by egress IP matching pods
func createDefaultNoRerouteServicePolicies(nbClient libovsdbclient.Client, clusterRouter string, v4ClusterSubnet, v6ClusterSubnet []*net.IPNet) error {
	for _, v4Subnet := range v4ClusterSubnet {
		match := fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4Subnet.String(), config.Gateway.V4JoinSubnet)
		if err := createLogicalRouterPolicy(nbClient, clusterRouter, match, types.DefaultNoRereoutePriority, nil, nil); err != nil {
			return fmt.Errorf("unable to create IPv4 no-reroute service policies, err: %v", err)
		}
	}
	for _, v6Subnet := range v6ClusterSubnet {
		match := fmt.Sprintf("ip6.src == %s && ip6.dst == %s", v6Subnet.String(), config.Gateway.V6JoinSubnet)
		if err := createLogicalRouterPolicy(nbClient, clusterRouter, match, types.DefaultNoRereoutePriority, nil, nil); err != nil {
			return fmt.Errorf("unable to create IPv6 no-reroute service policies, err: %v", err)
		}
	}
	return nil
}

// createDefaultNoRerouteReplyTrafficPolicies ensures any traffic which is a response/reply from the egressIP pods
// will not be re-routed to egress-nodes. This ensures EIP can work well with ETP=local
// this policy is ipFamily neutral
func createDefaultNoRerouteReplyTrafficPolicy(nbClient libovsdbclient.Client, clusterRouter string) error {
	match := fmt.Sprintf("pkt.mark == %d", types.EgressIPReplyTrafficConnectionMark)
	externalIDs := getEgressIPLRPNoReRouteDbIDs(types.DefaultNoRereoutePriority, ReplyTrafficNoReroute, IPFamilyValue).GetExternalIDs()
	if err := createLogicalRouterPolicy(nbClient, clusterRouter, match, types.DefaultNoRereoutePriority, externalIDs, nil); err != nil {
		return fmt.Errorf("unable to create no-reroute reply traffic policies, err: %v", err)
	}
	return nil
}

// createDefaultNoReroutePodPolicies ensures egress pods east<->west traffic with regular pods,
// i.e: ensuring that an egress pod can still communicate with a regular pod / service backed by regular pods
func createDefaultNoReroutePodPolicies(nbClient libovsdbclient.Client, clusterRouter string, v4ClusterSubnet, v6ClusterSubnet []*net.IPNet) error {
	for _, v4Subnet := range v4ClusterSubnet {
		match := fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4Subnet.String(), v4Subnet.String())
		if err := createLogicalRouterPolicy(nbClient, clusterRouter, match, types.DefaultNoRereoutePriority, nil, nil); err != nil {
			return fmt.Errorf("unable to create IPv4 no-reroute pod policies, err: %v", err)
		}
	}
	for _, v6Subnet := range v6ClusterSubnet {
		match := fmt.Sprintf("ip6.src == %s && ip6.dst == %s", v6Subnet.String(), v6Subnet.String())
		if err := createLogicalRouterPolicy(nbClient, clusterRouter, match, types.DefaultNoRereoutePriority, nil, nil); err != nil {
			return fmt.Errorf("unable to create IPv6 no-reroute pod policies, err: %v", err)
		}
	}
	return nil
}

// createDefaultReRouteQoSRule builds QoS rule ops to be created on every node's switch that let's us
// mark packets that are tracked in conntrack and replies emerging from the pod.
// This mark is then matched on the reroute policies to determine if its a reply packet
// in which case we do not need to reRoute to other nodes and if its a service response it is
// not rerouted since mark will be present.
func createDefaultReRouteQoSRuleOps(nbClient libovsdbclient.Client, addressSetFactory addressset.AddressSetFactory,
	controllerName string) ([]*nbdb.QoS, []ovsdb.Operation, error) {
	// fetch the egressIP pods address-set
	dbIDs := getEgressIPAddrSetDbIDs(EgressIPServedPodsAddrSetName, controllerName)
	var as addressset.AddressSet
	var err error
	var ops []ovsdb.Operation
	qoses := []*nbdb.QoS{}
	if as, err = addressSetFactory.GetAddressSet(dbIDs); err != nil {
		return nil, nil, fmt.Errorf("cannot ensure that addressSet %s exists %v", EgressIPServedPodsAddrSetName, err)
	}
	ipv4EgressIPServedPodsAS, ipv6EgressIPServedPodsAS := as.GetASHashNames()
	qosRule := nbdb.QoS{
		Priority:  types.EgressIPRerouteQoSRulePriority,
		Action:    map[string]int{"mark": types.EgressIPReplyTrafficConnectionMark},
		Direction: nbdb.QoSDirectionFromLport,
	}
	if config.IPv4Mode {
		qosV4Rule := qosRule
		qosV4Rule.Match = fmt.Sprintf(`ip4.src == $%s && ct.trk && ct.rpl`, ipv4EgressIPServedPodsAS)
		qosV4Rule.ExternalIDs = getEgressIPQoSRuleDbIDs(IPFamilyValueV4).GetExternalIDs()
		ops, err = libovsdbops.CreateOrUpdateQoSesOps(nbClient, nil, &qosV4Rule)
		if err != nil {
			return nil, nil, fmt.Errorf("cannot create v4 QoS rule ops for egressIP feature on controller %s, %v", controllerName, err)
		}
		qoses = append(qoses, &qosV4Rule)
	}
	if config.IPv6Mode {
		qosV6Rule := qosRule
		qosV6Rule.Match = fmt.Sprintf(`ip6.src == $%s && ct.trk && ct.rpl`, ipv6EgressIPServedPodsAS)
		qosV6Rule.ExternalIDs = getEgressIPQoSRuleDbIDs(IPFamilyValueV6).GetExternalIDs()
		ops, err = libovsdbops.CreateOrUpdateQoSesOps(nbClient, ops, &qosV6Rule)
		if err != nil {
			return nil, nil, fmt.Errorf("cannot create v6 QoS rule ops for egressIP feature on controller %s, %v", controllerName, err)
		}
		qoses = append(qoses, &qosV6Rule)
	}
	return qoses, ops, nil
}

func (oc *DefaultNetworkController) ensureDefaultNoRerouteQoSRules(nodeName string) error {
	oc.eIPC.nodeUpdateMutex.Lock()
	defer oc.eIPC.nodeUpdateMutex.Unlock()
	var ops []ovsdb.Operation
	// since this function is called from node update event, let us check
	// libovsdb cache before trying to create insert/update ops so that it
	// doesn't cause no-op construction spams at scale (kubelet sends node
	// update events every 10seconds so we don't want to cause unnecessary
	// no-op transacts that often and lookup is less expensive)
	predicateIDs := libovsdbops.NewDbObjectIDs(libovsdbops.LogicalRouterPolicyEgressIP, DefaultNetworkControllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: string(ReplyTrafficMark),
		})
	qosPredicate := libovsdbops.GetPredicate[*nbdb.QoS](predicateIDs, nil)
	existingQoSes, err := libovsdbops.FindQoSesWithPredicate(oc.nbClient, qosPredicate)
	if err != nil {
		return err
	}
	qosExists := false
	if config.IPv4Mode && config.IPv6Mode && len(existingQoSes) == 2 {
		// no need to create QoS Rule ops; already exists; dualstack
		qosExists = true
	}
	if len(existingQoSes) == 1 && ((config.IPv4Mode && !config.IPv6Mode) || (config.IPv6Mode && !config.IPv4Mode)) {
		// no need to create QoS Rule ops; already exists; single stack
		qosExists = true
	}
	if !qosExists {
		existingQoSes, ops, err = createDefaultReRouteQoSRuleOps(oc.nbClient, oc.addressSetFactory, oc.controllerName)
		if err != nil {
			return fmt.Errorf("cannot create QoS rule ops: %v", err)
		}
	}
	if len(existingQoSes) > 0 {
		nodeSwitchName := oc.GetNetworkScopedSwitchName(nodeName)
		if qosExists {
			// check if these rules were already added to the existing switch or not
			addQoSToSwitch := false
			nodeSwitch, err := libovsdbops.GetLogicalSwitch(oc.nbClient, &nbdb.LogicalSwitch{Name: nodeSwitchName})
			if err != nil {
				return fmt.Errorf("cannot fetch switch for node %s: %v", nodeSwitchName, err)
			}
			for _, qos := range existingQoSes {
				if slices.Contains(nodeSwitch.QOSRules, qos.UUID) {
					continue
				} else { // rule doesn't exist on switch; we should update switch
					addQoSToSwitch = true
					break
				}
			}
			if !addQoSToSwitch {
				return nil
			}
		}
		ops, err = libovsdbops.AddQoSesToLogicalSwitchOps(oc.nbClient, ops, nodeSwitchName, existingQoSes...)
		if err != nil {
			return err
		}
	}
	if _, err := libovsdbops.TransactAndCheck(oc.nbClient, ops); err != nil {
		return fmt.Errorf("unable to add EgressIP QoS to switch on zone %s, err: %v", oc.zone, err)
	}
	return nil
}

func (oc *DefaultNetworkController) ensureDefaultNoRerouteNodePolicies() error {
	oc.eIPC.nodeUpdateMutex.Lock()
	defer oc.eIPC.nodeUpdateMutex.Unlock()
	nodeLister := listers.NewNodeLister(oc.watchFactory.NodeInformer().GetIndexer())
	return ensureDefaultNoRerouteNodePolicies(oc.nbClient, oc.addressSetFactory, oc.controllerName, oc.GetNetworkScopedClusterRouterName(), nodeLister)
}

// ensureDefaultNoRerouteNodePolicies ensures egress pods east<->west traffic with hostNetwork pods,
// i.e: ensuring that an egress pod can still communicate with a hostNetwork pod / service backed by hostNetwork pods
// without using egressIPs.
// sample: 101 ip4.src == $a12749576804119081385 && ip4.dst == $a11079093880111560446 allow pkt_mark=1008
// All the cluster node's addresses are considered. This is to avoid race conditions after a VIP moves from one node
// to another where we might process events out of order. For the same reason this function needs to be called under
// lock.
func ensureDefaultNoRerouteNodePolicies(nbClient libovsdbclient.Client, addressSetFactory addressset.AddressSetFactory, controllerName, clusterRouter string, nodeLister listers.NodeLister) error {
	nodes, err := nodeLister.List(labels.Everything())
	if err != nil {
		return err
	}

	v4NodeAddrs, v6NodeAddrs, err := util.GetNodeAddresses(config.IPv4Mode, config.IPv6Mode, nodes...)
	if err != nil {
		return err
	}
	allAddresses := make([]net.IP, 0, len(v4NodeAddrs)+len(v6NodeAddrs))
	allAddresses = append(allAddresses, v4NodeAddrs...)
	allAddresses = append(allAddresses, v6NodeAddrs...)

	var as addressset.AddressSet
	dbIDs := getEgressIPAddrSetDbIDs(NodeIPAddrSetName, controllerName)
	if as, err = addressSetFactory.GetAddressSet(dbIDs); err != nil {
		return fmt.Errorf("cannot ensure that addressSet %s exists %v", NodeIPAddrSetName, err)
	}

	if err = as.SetAddresses(util.StringSlice(allAddresses)); err != nil {
		return fmt.Errorf("unable to set IPs to no re-route address set %s: %w", NodeIPAddrSetName, err)
	}

	ipv4ClusterNodeIPAS, ipv6ClusterNodeIPAS := as.GetASHashNames()
	// fetch the egressIP pods address-set
	dbIDs = getEgressIPAddrSetDbIDs(EgressIPServedPodsAddrSetName, controllerName)
	if as, err = addressSetFactory.GetAddressSet(dbIDs); err != nil {
		return fmt.Errorf("cannot ensure that addressSet %s exists %v", EgressIPServedPodsAddrSetName, err)
	}
	ipv4EgressIPServedPodsAS, ipv6EgressIPServedPodsAS := as.GetASHashNames()

	// fetch the egressService pods address-set
	dbIDs = egresssvc.GetEgressServiceAddrSetDbIDs(controllerName)
	if as, err = addressSetFactory.GetAddressSet(dbIDs); err != nil {
		return fmt.Errorf("cannot ensure that addressSet %s exists %v", egresssvc.EgressServiceServedPodsAddrSetName, err)
	}
	ipv4EgressServiceServedPodsAS, ipv6EgressServiceServedPodsAS := as.GetASHashNames()

	var matchV4, matchV6 string
	// construct the policy match
	if len(v4NodeAddrs) > 0 {
		matchV4 = fmt.Sprintf(`(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s`,
			ipv4EgressIPServedPodsAS, ipv4EgressServiceServedPodsAS, ipv4ClusterNodeIPAS)
	}
	if len(v6NodeAddrs) > 0 {
		matchV6 = fmt.Sprintf(`(ip6.src == $%s || ip6.src == $%s) && ip6.dst == $%s`,
			ipv6EgressIPServedPodsAS, ipv6EgressServiceServedPodsAS, ipv6ClusterNodeIPAS)
	}
	options := map[string]string{"pkt_mark": types.EgressIPNodeConnectionMark}
	// Create global allow policy for node traffic
	if matchV4 != "" {
		if err := createLogicalRouterPolicy(nbClient, clusterRouter, matchV4, types.DefaultNoRereoutePriority, nil, options); err != nil {
			return fmt.Errorf("unable to create IPv4 no-reroute node policies, err: %v", err)
		}
	}

	if matchV6 != "" {
		if err := createLogicalRouterPolicy(nbClient, clusterRouter, matchV6, types.DefaultNoRereoutePriority, nil, options); err != nil {
			return fmt.Errorf("unable to create IPv6 no-reroute node policies, err: %v", err)
		}
	}
	return nil
}

func createLogicalRouterPolicy(nbClient libovsdbclient.Client, clusterRouter, match string, priority int, externalIDs, options map[string]string) error {
	lrp := nbdb.LogicalRouterPolicy{
		Priority:    priority,
		Action:      nbdb.LogicalRouterPolicyActionAllow,
		Match:       match,
		ExternalIDs: externalIDs,
		Options:     options,
	}
	p := func(item *nbdb.LogicalRouterPolicy) bool {
		return item.Match == lrp.Match && item.Priority == lrp.Priority
	}
	err := libovsdbops.CreateOrUpdateLogicalRouterPolicyWithPredicate(nbClient, clusterRouter, &lrp, p)
	if err != nil {
		return fmt.Errorf("error creating logical router policy %+v on router %s: %v", lrp, clusterRouter, err)
	}
	return nil
}

// DeleteLegacyDefaultNoRerouteNodePolicies deletes the older EIP node reroute policies
// called from syncFunction and is a one time operation
// sample: 101 ip4.src == 10.244.0.0/16 && ip4.dst == 172.18.0.2/32           allow
func DeleteLegacyDefaultNoRerouteNodePolicies(nbClient libovsdbclient.Client, clusterRouter, node string) error {
	p := func(item *nbdb.LogicalRouterPolicy) bool {
		if item.Priority != types.DefaultNoRereoutePriority {
			return false
		}
		nodeName, ok := item.ExternalIDs["node"]
		if !ok {
			return false
		}
		return nodeName == node
	}
	return libovsdbops.DeleteLogicalRouterPoliciesWithPredicate(nbClient, clusterRouter, p)
}

func (e *egressIPZoneController) buildSNATFromEgressIPStatus(podIP net.IP, status egressipv1.EgressIPStatusItem, egressIPName string) (*nbdb.NAT, error) {
	logicalIP := &net.IPNet{
		IP:   podIP,
		Mask: util.GetIPFullMask(podIP),
	}
	externalIP := net.ParseIP(status.EgressIP)
	logicalPort := e.GetNetworkScopedK8sMgmtIntfName(status.Node)
	externalIds := map[string]string{"name": egressIPName}
	nat := libovsdbops.BuildSNAT(&externalIP, logicalIP, logicalPort, externalIds)
	return nat, nil
}

func (e *egressIPZoneController) createNATRuleOps(ops []ovsdb.Operation, podIPs []*net.IPNet, status egressipv1.EgressIPStatusItem, egressIPName string) ([]ovsdb.Operation, error) {
	nats := make([]*nbdb.NAT, 0, len(podIPs))
	var nat *nbdb.NAT
	var err error
	for _, podIP := range podIPs {
		if (utilnet.IsIPv6String(status.EgressIP) && utilnet.IsIPv6(podIP.IP)) || (!utilnet.IsIPv6String(status.EgressIP) && !utilnet.IsIPv6(podIP.IP)) {
			nat, err = e.buildSNATFromEgressIPStatus(podIP.IP, status, egressIPName)
			if err != nil {
				return nil, err
			}
			nats = append(nats, nat)
		}
	}
	router := &nbdb.LogicalRouter{
		Name: e.GetNetworkScopedGWRouterName(status.Node),
	}
	ops, err = libovsdbops.CreateOrUpdateNATsOps(e.nbClient, ops, router, nats...)
	if err != nil {
		return nil, fmt.Errorf("unable to create snat rules, for router: %s, error: %v", router.Name, err)
	}
	return ops, nil
}

func (e *egressIPZoneController) deleteNATRuleOps(ops []ovsdb.Operation, podIPs []*net.IPNet, status egressipv1.EgressIPStatusItem, egressIPName string) ([]ovsdb.Operation, error) {
	nats := make([]*nbdb.NAT, 0, len(podIPs))
	var nat *nbdb.NAT
	var err error
	for _, podIP := range podIPs {
		if (utilnet.IsIPv6String(status.EgressIP) && utilnet.IsIPv6(podIP.IP)) || (!utilnet.IsIPv6String(status.EgressIP) && !utilnet.IsIPv6(podIP.IP)) {
			nat, err = e.buildSNATFromEgressIPStatus(podIP.IP, status, egressIPName)
			if err != nil {
				return nil, err
			}
			nats = append(nats, nat)
		}
	}
	router := &nbdb.LogicalRouter{
		Name: e.GetNetworkScopedGWRouterName(status.Node),
	}
	ops, err = libovsdbops.DeleteNATsOps(e.nbClient, ops, router, nats...)
	if err != nil {
		return nil, fmt.Errorf("unable to remove snat rules for router: %s, error: %v", router.Name, err)
	}
	return ops, nil
}

func getPodKey(pod *kapi.Pod) string {
	return fmt.Sprintf("%s_%s", pod.Namespace, pod.Name)
}

func getPodNamespaceAndNameFromKey(podKey string) (string, string) {
	parts := strings.Split(podKey, "_")
	return parts[0], parts[1]
}

func (c *egressIPZoneController) ShouldReconcileNetworkChange(nodes []string, old, new util.NetInfo) bool {
	if !egressip.AdvertisementsEnabled() {
		return false
	}

	return egressip.ReconcileEgressIPNetworkChangeOnNodes(nodes, old, new)
}

func (c *egressIPZoneController) ReconcileNetwork(name string) {
	if c.networkReconciler != nil {
		c.networkReconciler.Reconcile(name)
	}
}

func (c *egressIPZoneController) reconcileNetwork(name string) error {
	eips, err := c.watchFactory.EgressIPInformer().Lister().List(labels.Everything())
	if err != nil {
		return err
	}

	isActiveNetworkForAnyNamespace := func(namespaces []*v1.Namespace) bool {
		for _, ns := range namespaces {
			network := c.networkManager.GetActiveNetworkForNamespaceFast(ns.Name)
			if network.GetNetworkName() == name {
				return true
			}
		}
		return false
	}

	reconcileEIPs := map[string]*egressipv1.EgressIP{}
	for _, eip := range eips {
		selector, err := metav1.LabelSelectorAsSelector(&eip.Spec.NamespaceSelector)
		if err != nil {
			return err
		}
		selectedNs, err := c.watchFactory.NamespaceCoreInformer().Lister().List(selector)
		if err != nil {
			return err
		}
		if isActiveNetworkForAnyNamespace(selectedNs) {
			reconcileEIPs[eip.Name] = eip
			break
		}
	}

	if len(reconcileEIPs) == 0 {
		return nil
	}

	for _, eip := range reconcileEIPs {
		err := c.retryEgressIPs.AddRetryObjWithAddNoBackoff(eip)
		if err != nil {
			return err
		}
	}

	c.retryEgressIPs.RequestRetryObjs()
	return nil
}

func (c *egressIPZoneController) Start() error {
	if c.networkReconciler != nil {
		return controller.Start(c.networkReconciler)
	}
	return nil
}

func (c *egressIPZoneController) Stop() {
	if c.networkReconciler != nil {
		controller.Stop(c.networkReconciler)
	}
}
