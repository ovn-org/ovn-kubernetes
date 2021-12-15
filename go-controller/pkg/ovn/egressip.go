package ovn

import (
	"encoding/hex"
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

	ocpcloudnetworkapi "github.com/openshift/api/cloudnetwork/v1"
	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/libovsdb/ovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	egressipv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	kapi "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
)

type egressIPDialer interface {
	dial(ip net.IP) bool
}

var dialer egressIPDialer = &egressIPDial{}

var cloudPrivateIPConfigFinalizer = "cloudprivateipconfig.cloud.network.openshift.io/finalizer"

func (oc *Controller) reconcileEgressIP(old, new *egressipv1.EgressIP) (err error) {
	// Lock the assignment, this is needed because this function can end up
	// being called from WatchEgressNodes and WatchEgressIP, i.e: two different
	// go-routines and we need to make sure the assignment is safe.
	oc.eIPC.egressIPAssignmentMutex.Lock()
	defer oc.eIPC.egressIPAssignmentMutex.Unlock()

	// Initialize two empty objects as to avoid SIGSEGV. The code should play
	// nicely with empty objects though.
	oldEIP, newEIP := &egressipv1.EgressIP{}, &egressipv1.EgressIP{}

	// Initialize two "nothing" selectors. Nothing selector are semantically
	// opposed to "empty" selectors, i.e: they select and match nothing, while
	// an empty one matches everything. If old/new are nil, and we don't do
	// this: we would have an empty EgressIP object which would result in two
	// empty selectors, matching everything, whereas we would mean the inverse
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

	// We do not initialize a nothing selector for the podSelector, because
	// these are allowed to be empty (i.e: matching all pods in a namespace), as
	// supposed to the namespaceSelector
	newPodSelector, err := metav1.LabelSelectorAsSelector(&newEIP.Spec.PodSelector)
	if err != nil {
		return fmt.Errorf("invalid new podSelector, err: %v", err)
	}
	oldPodSelector, err := metav1.LabelSelectorAsSelector(&oldEIP.Spec.PodSelector)
	if err != nil {
		return fmt.Errorf("invalid old podSelector, err: %v", err)
	}

	// Validate the spec and use only the valid egress IPs when performing any
	// successive operations, theoretically: the user could specify invalid IP
	// addresses, which would break us.
	validSpecIPs, err := oc.validateEgressIPSpec(newEIP.Name, newEIP.Spec.EgressIPs)
	if err != nil {
		return fmt.Errorf("invalid EgressIP spec, err: %v", err)
	}

	// Validate the status, on restart it could be the case that what might have
	// been assigned when ovnkube-master last ran is not a valid assignment
	// anymore (specifically if ovnkube-master has been crashing for a while).
	// Any invalid status at this point in time needs to be removed and assigned
	// to a valid node.
	validStatuses, statusToRemove := oc.validateEgressIPStatus(newEIP.Name, newEIP.Status.Items)
	statusToKeep := []egressipv1.EgressIPStatusItem{}
	statusToAdd := []egressipv1.EgressIPStatusItem{}
	validStatusIPs := sets.NewString()
	for _, validStatus := range validStatuses {
		validStatusIPs.Insert(validStatus.EgressIP)

		// If the spec has changed and an egress IP has been removed by the
		// user: we need to un-assign that egress IP, else keep it.
		if !validSpecIPs.Has(validStatus.EgressIP) {
			statusToRemove = append(statusToRemove, validStatus)
		} else {
			statusToKeep = append(statusToKeep, validStatus)
		}
	}

	// Check the old egress IP and its status assignments. This is mainly done
	// if a node is detected as not ready/reachable: we'll end up getting called
	// from deleteEgressNode, which will have updated the object with only the
	// statuses to keep. Hence, if the new object does not have the old
	// assignment, we need to remove those.
	for _, oldStatus := range oldEIP.Status.Items {
		if !validStatusIPs.Has(oldStatus.EgressIP) {
			statusToRemove = append(statusToRemove, oldStatus)
		}
	}

	// Add only the diff between what is requested and valid and that which
	// isn't already assigned.
	ipsToAdd := validSpecIPs.Difference(validStatusIPs)
	if !util.PlatformTypeIsEgressIPCloudProvider() {
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
		// Update the object only on an ADD/UPDATE. If we are processing a
		// DELETE, new will be nil and we should not update the object.
		if len(statusToAdd) > 0 || (len(statusToRemove) > 0 && new != nil) {
			if err := oc.updateEgressIPStatus(newEIP.Name, statusToKeep); err != nil {
				return err
			}
			metrics.RecordEgressIPCount(getEgressIPAllocationTotalCount(oc.eIPC.allocator))
		}
	} else {
		// If running on a public cloud we should not program OVN just yet, we
		// need confirmation from the cloud-network-config-controller that it
		// can assign the IPs. reconcileCloudPrivateIPConfig will take care of
		// processing the answer from the requests we make here, and update OVN
		// accordingly when we know what the outcome is.
		if len(ipsToAdd) > 0 {
			statusToAdd = oc.assignEgressIPs(newEIP.Name, ipsToAdd.List())
		}
		if err := oc.executeCloudPrivateIPConfigChange(newEIP.Name, statusToAdd, statusToRemove); err != nil {
			return err
		}
	}

	// If nothing has changed for what concerns the assignments, then check if
	// the namespaceSelector and podSelector have changed. If they have changed
	// then remove the setup for all pods which matched the old and add
	// everything for all pods which match the new.
	if len(ipsToAdd) == 0 &&
		len(statusToRemove) == 0 {
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
					if newPodSelector.Matches(podLabels) && !oldPodSelector.Matches(podLabels) {
						if err := oc.addPodEgressIPAssignments(newEIP.Name, newEIP.Status.Items, pod); err != nil {
							return err
						}
					}
				}
			}
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
							if err := oc.addPodEgressIPAssignments(newEIP.Name, newEIP.Status.Items, pod); err != nil {
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

	// If the namespace the pod belongs to does not have any labels, just return
	// it can't match any EgressIP object
	namespaceLabels := labels.Set(namespace.Labels)
	if namespaceLabels.AsSelector().Empty() {
		return nil
	}

	// Iterate all EgressIPs and check if this pod start/stops matching any and
	// add/remove the setup accordingly. Pods cannot match multiple EgressIP
	// objects: that is considered a user error and is undefined. Hence, just
	// just perform the setup against the first object matched.
	egressIPs, err := oc.watchFactory.GetEgressIPs()
	if err != nil {
		return err
	}
	for _, egressIP := range egressIPs {
		namespaceSelector, _ := metav1.LabelSelectorAsSelector(&egressIP.Spec.NamespaceSelector)
		if namespaceSelector.Matches(namespaceLabels) {
			// If the namespace the pod belongs to matches this object then
			// check the if there's a podSelector defined on the EgressIP
			// object. If there is one: the user intends the EgressIP object to
			// match only a subset of pods in the namespace, and we'll have to
			// check that. If there is no podSelector: the user intends it to
			// match all pods in the namespace.
			podSelector, _ := metav1.LabelSelectorAsSelector(&egressIP.Spec.PodSelector)
			// If the podSelector is not empty then check if the pod stopped
			// matching. If the pod was deleted, "new" will be nil and
			// newPodLabels will not match, so this is should cover that case.
			if !podSelector.Empty() && !podSelector.Matches(newPodLabels) && podSelector.Matches(oldPodLabels) {
				return oc.deletePodEgressIPAssignments(egressIP.Name, egressIP.Status.Items, oldPod)
			}
			// If the podSelector is empty (i.e: the EgressIP object is intended
			// to match all pods in the namespace) and the pod has been deleted:
			// "new" will be nil and we need to remove the setup
			if podSelector.Empty() && new == nil {
				return oc.deletePodEgressIPAssignments(egressIP.Name, egressIP.Status.Items, oldPod)
			}
			// For all else, perform a setup for the pod
			return oc.addPodEgressIPAssignments(egressIP.Name, egressIP.Status.Items, newPod)
		}
	}
	return nil
}

func (oc *Controller) reconcileCloudPrivateIPConfig(old, new *ocpcloudnetworkapi.CloudPrivateIPConfig) error {
	oldCloudPrivateIPConfig, newCloudPrivateIPConfig := &ocpcloudnetworkapi.CloudPrivateIPConfig{}, &ocpcloudnetworkapi.CloudPrivateIPConfig{}
	shouldDelete, shouldAdd := false, false
	nodeToDelete := ""

	if old != nil {
		oldCloudPrivateIPConfig = old
		// We need to handle two types of deletes, A) object UPDATE where the
		// old egress IP <-> node assignment has been removed. This is indicated
		// by the old object having a .status.node set and the new object having
		// .status.node empty and the condition on the new being successful. B)
		// object DELETE, for which the CloudPrivateIPConfig will not have a
		// finalizer anymore
		shouldDelete = oldCloudPrivateIPConfig.Status.Node != "" || !hasFinalizer(oldCloudPrivateIPConfig.Finalizers, cloudPrivateIPConfigFinalizer)
		// On DELETE we need to delete the .spec.node for the old object
		nodeToDelete = oldCloudPrivateIPConfig.Spec.Node
	}
	if new != nil {
		newCloudPrivateIPConfig = new
		// We should only proceed to setting things up for objects where the new
		// object has the same .spec.node and .status.node, and assignment
		// condition being true. This is how the cloud-network-config-controller
		// indicates a successful cloud assignment.
		shouldAdd = newCloudPrivateIPConfig.Status.Node == newCloudPrivateIPConfig.Spec.Node &&
			ocpcloudnetworkapi.CloudPrivateIPConfigConditionType(newCloudPrivateIPConfig.Status.Conditions[0].Type) == ocpcloudnetworkapi.Assigned &&
			kapi.ConditionStatus(newCloudPrivateIPConfig.Status.Conditions[0].Status) == kapi.ConditionTrue
		// See above explanation for the delete
		shouldDelete = shouldDelete && newCloudPrivateIPConfig.Status.Node == "" &&
			ocpcloudnetworkapi.CloudPrivateIPConfigConditionType(newCloudPrivateIPConfig.Status.Conditions[0].Type) == ocpcloudnetworkapi.Assigned &&
			kapi.ConditionStatus(newCloudPrivateIPConfig.Status.Conditions[0].Status) == kapi.ConditionTrue
		// On UPDATE we need to delete the old .status.node
		if shouldDelete {
			nodeToDelete = oldCloudPrivateIPConfig.Status.Node
		}
	}

	if shouldDelete {
		// There can only be one EgressIP object holding a given egress IP. If
		// multiple objects specify the same egress IP the assignment algorithm
		// invalidates it, hence: the following is safe.
		egressIP, err := oc.getEgressIPFromCloudPrivateIPConfigName(oldCloudPrivateIPConfig.Name)
		if err != nil {
			return err
		}
		egressIPString := cloudPrivateIPConfigNameToIPString(oldCloudPrivateIPConfig.Name)
		statusItem := egressipv1.EgressIPStatusItem{
			Node:     nodeToDelete,
			EgressIP: egressIPString,
		}
		if err := oc.deleteEgressIPAssignments(egressIP.Name, []egressipv1.EgressIPStatusItem{statusItem}, egressIP.Spec.NamespaceSelector, egressIP.Spec.PodSelector); err != nil {
			return err
		}
		// Deleting here means updating the object with the statuses we want to
		// keep
		updatedStatus := []egressipv1.EgressIPStatusItem{}
		for _, status := range egressIP.Status.Items {
			if !reflect.DeepEqual(status, statusItem) {
				updatedStatus = append(updatedStatus, status)
			}
		}
		if err := oc.updateEgressIPStatus(egressIP.Name, updatedStatus); err != nil {
			return err
		}
	}
	if shouldAdd {
		// Same explanation as for delete above.
		egressIP, err := oc.getEgressIPFromCloudPrivateIPConfigName(newCloudPrivateIPConfig.Name)
		if err != nil {
			return err
		}
		egressIPString := cloudPrivateIPConfigNameToIPString(newCloudPrivateIPConfig.Name)
		statusItem := egressipv1.EgressIPStatusItem{
			Node:     newCloudPrivateIPConfig.Status.Node,
			EgressIP: egressIPString,
		}
		if err := oc.addEgressIPAssignments(egressIP.Name, []egressipv1.EgressIPStatusItem{statusItem}, egressIP.Spec.NamespaceSelector, egressIP.Spec.PodSelector); err != nil {
			return err
		}
		// Guard against performing the same assignment twice, which might
		// happen when multiple updates come in on the same object.
		hasStatus := false
		for _, status := range egressIP.Status.Items {
			if reflect.DeepEqual(status, statusItem) {
				hasStatus = true
			}
		}
		if !hasStatus {
			if err := oc.updateEgressIPStatusWithItem(egressIP.Name, statusItem); err != nil {
				return err
			}
		}
	}
	metrics.RecordEgressIPCount(getEgressIPAllocationTotalCount(oc.eIPC.allocator))
	return nil
}

// getEgressIPFromCloudPrivateIPConfigName retrieves the EgressIP object holding
// the egress IP represented by the CloudPrivateIPConfig name. Only one egress
// IPs are unique for EgressIP object, so no two objects can reference the same
// egress IP, hence why this should work.
func (oc *Controller) getEgressIPFromCloudPrivateIPConfigName(cloudPrivateIPConfigName string) (*egressipv1.EgressIP, error) {
	egressIPString := cloudPrivateIPConfigNameToIPString(cloudPrivateIPConfigName)
	eIPs, err := oc.watchFactory.GetEgressIPs()
	if err != nil {
		return nil, fmt.Errorf("unable to get EgressIP objects, err: %v", err)
	}
	for _, eIP := range eIPs {
		for _, egressIP := range eIP.Spec.EgressIPs {
			if egressIP == egressIPString {
				return eIP, nil
			}
		}
	}
	return nil, fmt.Errorf("no matching EgressIP found")
}

type cloudPrivateIPConfigOp struct {
	toAdd    string
	toDelete string
}

// executeCloudPrivateIPConfigChange computes a diff between what needs to be
// assigned/removed and executes the object modification afterwards.
// Specifically: if one egress IP is moved from nodeA to nodeB, we actually care
// about an update on the CloudPrivateIPConfig object represented by that egress
// IP, cloudPrivateIPConfigOp is a helper used to determine that sort of
// operations from toAssign/toRemove
func (oc *Controller) executeCloudPrivateIPConfigChange(egressIPName string, toAssign, toRemove []egressipv1.EgressIPStatusItem) error {
	ops := make(map[string]cloudPrivateIPConfigOp, len(toAssign)*len(toRemove))
	for _, assignment := range toAssign {
		ops[assignment.EgressIP] = cloudPrivateIPConfigOp{
			toAdd: assignment.Node,
		}
	}
	for _, removal := range toRemove {
		if op, exists := ops[removal.EgressIP]; exists {
			op.toDelete = removal.Node
		} else {
			ops[removal.EgressIP] = cloudPrivateIPConfigOp{
				toDelete: removal.Node,
			}
		}
	}
	return oc.executeCloudPrivateIPConfigOps(egressIPName, ops)
}

func (oc *Controller) executeCloudPrivateIPConfigOps(egressIPName string, ops map[string]cloudPrivateIPConfigOp) error {
	for egressIP, op := range ops {
		cloudPrivateIPConfigName := ipStringToCloudPrivateIPConfigName(egressIP)
		cloudPrivateIPConfig, err := oc.watchFactory.GetCloudPrivateIPConfig(cloudPrivateIPConfigName)
		// toAdd and toDelete is non-empty, this indicates an UPDATE for which
		// the object **must** exist, if not: that's an error.
		if op.toAdd != "" && op.toDelete != "" {
			if err != nil {
				return fmt.Errorf("cloud update request failed for CloudPrivateIPConfig: %s, could not get item, err: %v", cloudPrivateIPConfigName, err)
			}
			cloudPrivateIPConfig.Spec.Node = op.toAdd
			if _, err := oc.kube.UpdateCloudPrivateIPConfig(cloudPrivateIPConfig); err != nil {
				eIPRef := kapi.ObjectReference{
					Kind: "EgressIP",
					Name: egressIPName,
				}
				oc.recorder.Eventf(&eIPRef, kapi.EventTypeWarning, "CloudUpdateFailed", "egress IP: %s for object EgressIP: %s could not be updated, err: %v", egressIP, egressIPName, err)
				return fmt.Errorf("cloud update request failed for CloudPrivateIPConfig: %s, err: %v", cloudPrivateIPConfigName, err)
			}
			// toAdd is non-empty, this indicates an ADD for which
			// the object **must not** exist, if not: that's an error.
		} else if op.toAdd != "" {
			if err == nil {
				return fmt.Errorf("cloud create request failed for CloudPrivateIPConfig: %s, err: item exists", cloudPrivateIPConfigName)
			}
			cloudPrivateIPConfig := ocpcloudnetworkapi.CloudPrivateIPConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name: cloudPrivateIPConfigName,
				},
				Spec: ocpcloudnetworkapi.CloudPrivateIPConfigSpec{
					Node: op.toAdd,
				},
			}
			if _, err := oc.kube.CreateCloudPrivateIPConfig(&cloudPrivateIPConfig); err != nil {
				eIPRef := kapi.ObjectReference{
					Kind: "EgressIP",
					Name: egressIPName,
				}
				oc.recorder.Eventf(&eIPRef, kapi.EventTypeWarning, "CloudAssignmentFailed", "egress IP: %s for object EgressIP: %s could not be created, err: %v", egressIP, egressIPName, err)
				return fmt.Errorf("cloud add request failed for CloudPrivateIPConfig: %s, err: %v", cloudPrivateIPConfigName, err)
			}
			// toDelete is non-empty, this indicates an DELETE for which
			// the object **must** exist, if not: that's an error.
		} else if op.toDelete != "" {
			if err != nil {
				return fmt.Errorf("cloud deletion request failed for CloudPrivateIPConfig: %s, could not get item, err: %v", cloudPrivateIPConfigName, err)
			}
			if err := oc.kube.DeleteCloudPrivateIPConfig(cloudPrivateIPConfigName); err != nil {
				eIPRef := kapi.ObjectReference{
					Kind: "EgressIP",
					Name: egressIPName,
				}
				oc.recorder.Eventf(&eIPRef, kapi.EventTypeWarning, "CloudDeletionFailed", "egress IP: %s for object EgressIP: %s could not be deleted, err: %v", egressIP, egressIPName, err)
				return fmt.Errorf("cloud deletion request failed for CloudPrivateIPConfig: %s, err: %v", cloudPrivateIPConfigName, err)
			}
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

// validateEgressIPStatus validates if the statuses are valid given what the
// cache knows about all egress nodes. WatchEgressNodes is initialized before
// any other egress IP handler, so te cache should be warm and correct once we
// start going this.
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
			if utilnet.IsIPv6(ip) && eNode.egressIPConfig.V6.Net != nil {
				if !eNode.egressIPConfig.V6.Net.Contains(ip) {
					klog.Errorf("Allocator error: EgressIP allocation: %s on subnet: %s which cannot host it", ip.String(), eNode.egressIPConfig.V4.Net.String())
					break
				}
			} else if !utilnet.IsIPv6(ip) && eNode.egressIPConfig.V4.Net != nil {
				if !eNode.egressIPConfig.V4.Net.Contains(ip) {
					klog.Errorf("Allocator error: EgressIP allocation: %s on subnet: %s which cannot host it", ip.String(), eNode.egressIPConfig.V4.Net.String())
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

// addPodEgressIPAssignments tracks the setup made for each egress IP matching
// pod w.r.t to each status. This is mainly done to avoid a lot of duplicated
// work on ovnkube-master restarts when all egress IP handlers will most likely
// match and perform the setup for the same pod and status multiple times over.
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

// deleteAllocatorEgressIPAssignments deletes the allocation as to keep the
// cache state correct, also see addAllocatorEgressIPAssignments
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

// isAnyClusterNodeIP verifies that the IP is not any node IP.
func (oc *Controller) isAnyClusterNodeIP(ip net.IP) *egressNode {
	for _, eNode := range oc.eIPC.allocator.cache {
		if ip.Equal(eNode.egressIPConfig.V6.IP) || ip.Equal(eNode.egressIPConfig.V4.IP) {
			return eNode
		}
	}
	return nil
}

func (oc *Controller) updateEgressIPStatusWithItem(name string, statusItem egressipv1.EgressIPStatusItem) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Get the latest item from the store before updating.
		egressIP, err := oc.watchFactory.GetEgressIP(name)
		if err != nil {
			return err
		}
		// Copy the item, since the object retrieved from the store (informer
		// cache) will point to the same object we are trying to update, hence
		// not doing this might cause race conditions when running tests.
		updateEgressIP := egressIP.DeepCopy()
		updateEgressIP.Status.Items = append(egressIP.Status.Items, statusItem)
		return oc.kube.UpdateEgressIP(updateEgressIP)
	})
}

func (oc *Controller) updateEgressIPStatus(name string, statusItems []egressipv1.EgressIPStatusItem) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// see: updateEgressIPStatusWithItem
		egressIP, err := oc.watchFactory.GetEgressIP(name)
		if err != nil {
			return err
		}
		updateEgressIP := egressIP.DeepCopy()
		updateEgressIP.Status.Items = statusItems
		return oc.kube.UpdateEgressIP(updateEgressIP)
	})
}

// assignEgressIPs is the main assignment algorithm for egress IPs to nodes.
// Specifically we have a couple of hard constraints: a) the subnet of the node
// must be able to host the egress IP b) the egress IP cannot be a node IP c)
// the IP cannot already be assigned and reference by another EgressIP object d)
// no two egress IPs for the same EgressIP object can be assigned to the same
// node e) (for public clouds) the amount of egress IPs assigned to one node
// must respect its assignment capacity. Moreover there is a soft constraint:
// the assignments need to be balanced across all cluster nodes, so that no node
// becomes a bottleneck. The balancing is achieved by sorting the nodes in
// ascending order following their existing amount of allocations, and trying to
// assign the egress IP to the node with the lowest amount of allocations every
// time, this does not guarantee complete balance, but mostly complete.
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
			if eNode.egressIPConfig.Capacity.IP < util.UnlimitedNodeCapacity {
				if eNode.egressIPConfig.Capacity.IP-len(eNode.allocations) <= 0 {
					klog.V(5).Infof("Additional allocation on Node: %s exhausts it's IP capacity, trying another node", eNode.name)
					continue
				}
			}
			if eNode.egressIPConfig.Capacity.IPv4 < util.UnlimitedNodeCapacity && utilnet.IsIPv4(eIPC) {
				if eNode.egressIPConfig.Capacity.IPv4-getIPFamilyAllocationCount(eNode.allocations, false) <= 0 {
					klog.V(5).Infof("Additional allocation on Node: %s exhausts it's IPv4 capacity, trying another node", eNode.name)
					continue
				}
			}
			if eNode.egressIPConfig.Capacity.IPv6 < util.UnlimitedNodeCapacity && utilnet.IsIPv6(eIPC) {
				if eNode.egressIPConfig.Capacity.IPv6-getIPFamilyAllocationCount(eNode.allocations, true) <= 0 {
					klog.V(5).Infof("Additional allocation on Node: %s exhausts it's IPv6 capacity, trying another node", eNode.name)
					continue
				}
			}
			if (eNode.egressIPConfig.V6.Net != nil && eNode.egressIPConfig.V6.Net.Contains(eIPC)) ||
				(eNode.egressIPConfig.V4.Net != nil && eNode.egressIPConfig.V4.Net.Contains(eIPC)) {
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

func getIPFamilyAllocationCount(allocations map[string]string, isIPv6 bool) (count int) {
	for allocation := range allocations {
		if utilnet.IsIPv4String(allocation) && !isIPv6 {
			count++
		}
		if utilnet.IsIPv6String(allocation) && isIPv6 {
			count++
		}
	}
	return
}

// getSortedEgressData returns a sorted slice of all egressNodes based on the
// amount of allocations found in the cache
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
		// if the node is not assignable/ready/reachable anymore we need to
		// empty all of it's allocations from our cache since we'll clear all
		// assignments from this node later on, because of this.
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
		// see setNodeEgressAssignable
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
		// see setNodeEgressAssignable
		if !isReachable {
			eNode.allocations = make(map[string]string)
		}
	}
}

func (oc *Controller) addEgressNode(egressNode *kapi.Node) error {
	klog.V(5).Infof("Egress node: %s about to be initialized", egressNode.Name)
	// This option will program OVN to start sending GARPs for all external IPS
	// that the logical switch port has been configured to use. This is
	// necessary for egress IP because if an egress IP is moved between two
	// nodes, the nodes need to actively update the ARP cache of all neighbors
	// as to notify them the change. If this is not the case: packets will
	// continue to be routed to the old node which hosted the egress IP before
	// it was moved, and the connections will fail.
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
	// If a node has been labelled for egress IP we need to check if there are any
	// egress IPs which are missing an assignment. If there are, we need to send a
	// synthetic update since reconcileEgressIP will then try to assign those IPs to
	// this node (if possible)
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
	// This will remove the option described in addEgressNode from the logical
	// switch port, since this node will not be used for egress IP assignments
	// from now on.
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
	// Since the node has been labelled as "not usable" for egress IP
	// assignments we need to find all egress IPs which have an assignment to
	// it, and move them elsewhere.
	egressIPs, err := oc.watchFactory.GetEgressIPs()
	if err != nil {
		return fmt.Errorf("unable to list EgressIPs, err: %v", err)
	}
	for _, eIP := range egressIPs {
		keepStatusItems := []egressipv1.EgressIPStatusItem{}
		for _, status := range eIP.Status.Items {
			if status.Node != egressNode.Name {
				// Keep only assignments that are not assigned to this node. Any
				// egress IPs which are missing an assignment will try to be
				// assigned once we've updated the object in reconcileEgressIP
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
		var parsedEgressIPConfig *util.ParsedNodeEgressIPConfiguration
		if util.PlatformTypeIsEgressIPCloudProvider() {
			parsedEgressIPConfig, err = util.ParseCloudEgressIPConfig(node)
			if err != nil {
				return fmt.Errorf("unable to use cloud node for egress assignment, err: %v", err)
			}
		} else {
			parsedEgressIPConfig, err = util.ParseNodePrimaryIfAddr(node)
			if err != nil {
				return fmt.Errorf("unable to use node for egress assignment, err: %v", err)
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
			name:           node.Name,
			egressIPConfig: parsedEgressIPConfig,
			mgmtIPs:        mgmtIPs,
			allocations:    make(map[string]string),
		}
	}
	return nil
}

// addNodeForEgress sets up default logical router policy for every node and
// initiates the allocator cache for the node in question, if the node has the
// necessary annotation.
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

// deleteNodeForEgress remove the default allow logical router policies for the
// node and removes the node from the allocator cache.
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

// initClusterEgressPolicies will initialize the default allow policies for
// east<->west traffic. Egress IP is based on routing egress traffic to specific
// egress nodes, we don't want to route any other traffic however and these
// policies will take care of that. Also, we need to initialize the go-routine
// which verifies if egress nodes are ready and reachable, typically if an
// egress node experiences problems we want to move all egress IP assignment
// away from that node elsewhere so that the pods using the egress IP can
// continue to do so without any issues.
func (oc *Controller) initClusterEgressPolicies(nodes []interface{}) {
	v4ClusterSubnet, v6ClusterSubnet := getClusterSubnets()
	oc.createDefaultNoReroutePodPolicies(v4ClusterSubnet, v6ClusterSubnet)
	oc.createDefaultNoRerouteServicePolicies(v4ClusterSubnet, v6ClusterSubnet)
	go oc.checkEgressNodesReachability()
}

// egressNode is a cache helper used for egress IP assignment, representing an egress node
type egressNode struct {
	egressIPConfig     *util.ParsedNodeEgressIPConfiguration
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
	// egressIPAssignmentMutex is used to ensure a safe updates between
	// concurrent go-routines which could be modifying the egress IP status
	// assignment simultaneously. Currently WatchEgressNodes and WatchEgressIP
	// run two separate go-routines which do this.
	egressIPAssignmentMutex *sync.Mutex
	// podAssignmentMutex is used to ensure safe access to podAssignment.
	// Currently WatchEgressIP, WatchEgressNamespace and WatchEgressPod could
	// all access that map simultaneously, hence why this guard is needed.
	podAssignmentMutex *sync.Mutex
	// podAssignment is a cache used for keeping track of which egressIP status
	// has been setup for each pod. The key is defined by getPodKey
	podAssignment map[string][]egressipv1.EgressIPStatusItem
	// allocator is a cache of egress IP centric data needed to when both route
	// health-checking and tracking allocations made
	allocator allocator
	// libovsdb northbound client interface
	nbClient libovsdbclient.Client
	// modelClient for performing idempotent NB operations
	modelClient libovsdbops.ModelClient
	// watchFactory watching k8s objects
	watchFactory *factory.WatchFactory
}

var skippedPodError = errors.New("pod has been skipped")

// addPodEgressIPAssignment will program OVN with logical router policies
// (routing pod traffic to the egress node) and NAT objects on the egress node
// (SNAT-ing to the egress IP). We can only do that for pods which have an
// assigned IP address though, so if this is not that case at a particular point
// in time just skip it. When the pod gets updates with an IP address we will
// just process that event and do it then.
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

// deletePodEgressIPAssignment deletes the OVN programmed egress IP
// configuration mentioned for addPodEgressIPAssignment. It only does so for
// pods which have a pod IP.
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
	gatewayIPs, err := util.GetLRPAddrs(e.nbClient, types.GWRouterToJoinSwitchPrefix+types.GWRouterPrefix+node)
	if err != nil {
		klog.Errorf("Attempt at finding node gateway router network information failed, err: %v", err)
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

// createEgressReroutePolicy performs idempotent updates of the
// LogicalRouterPolicy corresponding to the egressIP object, according to the
// following update procedure:
// - if the LogicalRouterPolicy does not exist: it adds it by creating the
// reference to it from ovn_cluster_router and specifying the array of nexthops
// to equal [gatewayRouterIP]
// - if the LogicalRouterPolicy does exist: it add the gatewayRouterIP to the
// array of nexthops
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

// deleteEgressReroutePolicy performs idempotent updates of the
// LogicalRouterPolicy corresponding to the egressIP object, according to the
// following update procedure:
// - if the LogicalRouterPolicy exist and has the len(nexthops) > 1: it removes
// the specified gatewayRouterIP from nexthops
// - if the LogicalRouterPolicy exist and has the len(nexthops) == 1: it removes
// the LogicalRouterPolicy completely
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

// checkEgressNodesReachability continuously checks if all nodes used for egress
// IP assignment are reachable, and updates the nodes following the result. This
// is important because egress IP is based upon routing traffic to these nodes,
// and if they aren't reachable we shouldn't be using them for egress IP.
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

// cloudPrivateIPConfigNameToIPString converts the resource name to the string
// representation of net.IP. Given a limitation in the Kubernetes API server
// (see: https://github.com/kubernetes/kubernetes/pull/100950)
// CloudPrivateIPConfig.metadata.name cannot represent an IPv6 address. To
// work-around this limitation it was decided that the network plugin creating
// the CR will fully expand the IPv6 address and replace all colons with dots,
// ex:

// The CloudPrivateIPConfig name fc00.f853.0ccd.e793.0000.0000.0000.0054 will be
// represented as address: fc00:f853:ccd:e793::54

// We thus need to replace every fifth character's dot with a colon.
func cloudPrivateIPConfigNameToIPString(name string) string {
	// Handle IPv4, which will work fine.
	if ip := net.ParseIP(name); ip != nil {
		return name
	}
	// Handle IPv6
	tmp := []byte(name)
	for i := 4; i < len(tmp); i += 5 {
		if len(tmp)-i > 4 {
			tmp[i] = byte(':')
		}
	}
	return string(tmp)
}

// ipStringToCloudPrivateIPConfigName converts the net.IP string representation
// to a CloudPrivateIPConfig compatible name.

// The string representation of the IPv6 address fc00:f853:ccd:e793::54 will be
// represented as: fc00.f853.0ccd.e793.0000.0000.0000.0054

// We thus need to fully expand the IP string and replace every fifth
// character's colon with a dot.
func ipStringToCloudPrivateIPConfigName(ipString string) (name string) {
	ip := net.ParseIP(ipString)
	if ip.To4() != nil {
		return ipString
	}
	dst := make([]byte, hex.EncodedLen(len(ip)))
	hex.Encode(dst, ip)
	for i := 0; i < len(dst); i += 4 {
		if len(dst)-i == 4 {
			name += string(dst[i : i+4])
		} else {
			name += string(dst[i:i+4]) + "."
		}
	}
	return
}

func hasFinalizer(finalizers []string, finalizer string) bool {
	for _, f := range finalizers {
		if f == finalizer {
			return true
		}
	}
	return false
}
