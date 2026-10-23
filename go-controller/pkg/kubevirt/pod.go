// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package kubevirt

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strings"

	kubevirtv1 "kubevirt.io/api/core/v1"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/validate/content"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	ktypes "k8s.io/apimachinery/pkg/types"
	listersv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/util/retry"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	logicalswitchmanager "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/logical_switch_manager"
	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

var (
	virtLauncherPodLabel = map[string]string{
		kubevirtv1.AppLabel: "virt-launcher",
	}
)

// IsPodLiveMigratable returns true if the pod should use KubeVirt live migration features.
func IsPodLiveMigratable(pod *corev1.Pod) bool {
	_, ok := pod.Annotations[kubevirtv1.AllowPodBridgeNetworkLiveMigrationAnnotation]
	return ok
}

// findVMRelatedPods returns pods that belong to the same VM as pod and filters out pod.
func findVMRelatedPods(client *factory.WatchFactory, pod *corev1.Pod) ([]*corev1.Pod, error) {
	vmDescription, err := NewVMDescriptionFromPod(pod)
	if err != nil {
		return nil, err
	}
	vmPods, err := vmDescription.OwnedPods(client.PodCoreInformer().Lister())
	if err != nil {
		return nil, err
	}
	filteredOutVMPods := []*corev1.Pod{}
	for _, vmPod := range vmPods {
		// The purpose of this function is to return the "other" pods related
		// to a VM.
		if vmPod.UID == pod.UID {
			continue
		}
		filteredOutVMPods = append(filteredOutVMPods, vmPod)
	}

	return filteredOutVMPods, nil
}

// findPodAnnotation returns the OVN pod annotation from any other pod annotated with the same VM as pod.
func findPodAnnotation(client *factory.WatchFactory, pod *corev1.Pod, nadKey string) (*util.PodAnnotation, error) {
	vmPods, err := findVMRelatedPods(client, pod)
	if err != nil {
		return nil, fmt.Errorf("failed finding related pods for pod %s/%s when looking for network info: %v", pod.Namespace, pod.Name, err)
	}
	// The virtual machine is not being live migrated, so there are no other
	// VM pods.
	if len(vmPods) == 0 {
		return nil, nil
	}

	for _, vmPod := range vmPods {
		podAnnotation, err := util.UnmarshalPodAnnotation(vmPod.Annotations, nadKey)
		if err == nil {
			return podAnnotation, nil
		}
	}
	return nil, fmt.Errorf("missing virtual machine pod annotation at stale pods for %s/%s", pod.Namespace, pod.Name)
}

// EnsurePodAnnotationForVM extracts OVN pod annotations from the source VM pod
// during live migration and copies them to the target VM pod so IP addresses
// follow the VM. This must happen before creating the LSP to ensure the target
// VM pod LSP Address field is configured correctly.
func EnsurePodAnnotationForVM(ctx context.Context, watchFactory *factory.WatchFactory, kube *kube.KubeOVN, pod *corev1.Pod, nadKey string) (*util.PodAnnotation, error) {
	if !IsPodLiveMigratable(pod) {
		return nil, nil
	}

	if podAnnotation, err := util.UnmarshalPodAnnotation(pod.Annotations, nadKey); err == nil {
		return podAnnotation, nil
	}

	podAnnotation, err := findPodAnnotation(watchFactory, pod, nadKey)
	if err != nil {
		return nil, err
	}
	if podAnnotation == nil {
		return nil, nil
	}

	var modifiedPod *corev1.Pod
	resultErr := retry.OnError(util.OvnConflictBackoff, util.IsPodAnnotationUpdateRetryable, func() error {
		// Informer cache should not be mutated, so get a copy of the object
		pod, err := watchFactory.GetPod(pod.Namespace, pod.Name)
		if err != nil {
			return err
		}
		// Informer cache should not be mutated, so get a copy of the object
		modifiedPod = pod.DeepCopy()
		if podAnnotation != nil {
			modifiedPod.Annotations, err = util.MarshalPodAnnotation(modifiedPod.Annotations, podAnnotation, nadKey)
			if err != nil {
				return err
			}
		}
		return kube.PatchPodStatusAnnotations(ctx, pod, modifiedPod)
	})
	if resultErr != nil {
		return nil, fmt.Errorf("failed to update labels and annotations on pod %s/%s: %v", pod.Namespace, pod.Name, resultErr)
	}
	return podAnnotation, nil
}

// AllVMPodsAreCompleted returns true if all VM pods are completed.
func AllVMPodsAreCompleted(podLister listersv1.PodLister, pod *corev1.Pod) (bool, error) {
	if !util.PodCompleted(pod) {
		return false, nil
	}

	vmDescription, err := NewVMDescriptionFromPod(pod)
	if err != nil {
		return false, err
	}

	vmPods, err := vmDescription.OwnedPods(podLister)
	if err != nil {
		return false, fmt.Errorf("failed finding related pods for pod %s/%s when checking if they are completed: %v", pod.Namespace, pod.Name, err)
	}

	for _, vmPod := range vmPods {
		if !util.PodCompleted(vmPod) {
			return false, nil
		}
	}
	return true, nil
}

// IsMigratedSourcePodStale returns true if the live-migratable pod is completed
// or has a newer sibling pod for the same VM.
func IsMigratedSourcePodStale(client *factory.WatchFactory, pod *corev1.Pod) (bool, error) {
	if !IsPodLiveMigratable(pod) {
		return false, nil
	}

	if util.PodCompleted(pod) {
		return true, nil
	}

	vmPods, err := findVMRelatedPods(client, pod)
	if err != nil {
		return false, fmt.Errorf("failed finding related pods for pod %s/%s when checking live migration left overs: %v", pod.Namespace, pod.Name, err)
	}

	for _, vmPod := range vmPods {
		if vmPod.CreationTimestamp.After(pod.CreationTimestamp.Time) {
			return true, nil
		}
	}

	return false, nil
}

// ZoneContainsPodSubnet returns the switch name and true if the logical switch
// contains the pod subnet, meaning this zone owns that subnet.
func ZoneContainsPodSubnet(lsManager *logicalswitchmanager.LogicalSwitchManager, ips []*net.IPNet) (string, bool) {
	return lsManager.GetSubnetName(ips)
}

// nodeContainsPodSubnet returns true if the node subnet annotation contains the argument subnets.
func nodeContainsPodSubnet(watchFactory *factory.WatchFactory, nodeName string, podAnnotation *util.PodAnnotation, netName string) (bool, error) {
	node, err := watchFactory.GetNode(nodeName)
	if err != nil {
		return false, err
	}
	nodeHostSubNets, err := util.ParseNodeHostSubnetAnnotation(node, netName)
	if err != nil {
		return false, err
	}
	for _, subnet := range podAnnotation.IPs {
		for _, nodeHostSubNet := range nodeHostSubNets {
			if nodeHostSubNet.Contains(subnet.IP) {
				return true, nil
			}
		}
	}
	return false, nil
}

// VMDescription holds the identity and ownership information for a KubeVirt
// virtual machine derived from one of its virt-launcher pods. The key field
// stores the namespaced name of the VM, and ownedPodsFn is a closure that
// lists all pods belonging to the same VM via label or annotation selectors.
type VMDescription struct {
	key         ktypes.NamespacedName
	ownedPodsFn func(podLister listersv1.PodLister) ([]*corev1.Pod, error)
}

// Key returns the namespaced name of the VM.
func (vm VMDescription) Key() ktypes.NamespacedName {
	return vm.key
}

// OwnedPods returns pods owned by the VM.
func (vm VMDescription) OwnedPods(podLister listersv1.PodLister) ([]*corev1.Pod, error) {
	return vm.ownedPodsFn(podLister)
}

func vmNameFromPod(pod *corev1.Pod) (string, error) {
	vmName, ok := pod.Annotations[kubevirtv1.DomainAnnotation]
	if !ok {
		return "", fmt.Errorf("virtual machine pod %s/%s is missing the mandatory kubevirt annotation %s", pod.Namespace, pod.Name, kubevirtv1.DomainAnnotation)
	}
	return vmName, nil
}

// nameToLabel truncates name to fit within the Kubernetes label value
// maximum length of 63 characters (content.LabelValueMaxLength). If
// name already fits, it is returned unchanged.
func nameToLabel(name string) string {
	if len(name) <= content.LabelValueMaxLength {
		return name
	}
	return name[:content.LabelValueMaxLength]
}

// NewVMDescriptionFromPod builds a VMDescription from a virt-launcher pod.
// Returns (nil, nil) if the pod is not owned by a virtual machine.
// The VM name is extracted from the KubeVirt domain annotation on the pod,
// and the returned VMDescription.ownedPodsFn lists all sibling virt-launcher
// pods belonging to the same VM. When the VM name label matches the
// (possibly truncated) VM name, the lookup uses that label for efficiency;
// otherwise it falls back to the generic virt-launcher label and filters
// by the domain annotation to avoid false matches across VMs with long or
// colliding names.
func NewVMDescriptionFromPod(pod *corev1.Pod) (*VMDescription, error) {
	if !IsPodOwnedByVirtualMachine(pod) {
		return nil, nil
	}

	vmName, err := vmNameFromPod(pod)
	if err != nil {
		return nil, fmt.Errorf("failed to get VM name from pod %s/%s: %w", pod.Namespace, pod.Name, err)
	}
	vmDescription := VMDescription{
		key: ktypes.NamespacedName{Namespace: pod.Namespace, Name: vmName},
	}

	// By default, filter pods first by "virt-launcher" pods.
	labelSelector := virtLauncherPodLabel

	// If the label is the same, truncating it if it is longer than 63 characters,
	// search directly by label so we iterate less.
	vmNameLabel, ok := pod.Labels[kubevirtv1.VirtualMachineNameLabel]
	if ok && nameToLabel(vmName) == vmNameLabel {
		labelSelector = map[string]string{kubevirtv1.VirtualMachineNameLabel: vmNameLabel}
	}

	vmDescription.ownedPodsFn = func(podLister listersv1.PodLister) ([]*corev1.Pod, error) {
		labelMatchedPods, err := podLister.Pods(vmDescription.key.Namespace).List(labels.SelectorFromSet(labelSelector))
		if err != nil {
			return nil, err
		}
		// Filter by DomainAnnotation to ensure we only include pods that actually
		// belong to this VM. Different VMs can have the same label if one VM's
		// name matches another VM's hostname, or if VM names are very long and
		// match their truncated form.
		ownedPods := []*corev1.Pod{}
		for _, virtLauncherPod := range labelMatchedPods {
			podVMName, err := vmNameFromPod(virtLauncherPod)
			if err != nil {
				return nil, err
			}
			if podVMName == vmDescription.key.Name {
				ownedPods = append(ownedPods, virtLauncherPod)
			}
		}
		return ownedPods, nil
	}

	return &vmDescription, nil
}

// CleanUpLiveMigratablePod removes routing and OVN DHCP resources
// when all the pods for the same VM as `pod` argument are completed.
func CleanUpLiveMigratablePod(nbClient libovsdbclient.Client, watchFactory *factory.WatchFactory, pod *corev1.Pod) error {
	if !IsPodLiveMigratable(pod) {
		return nil
	}

	allVMPodsCompleted, err := AllVMPodsAreCompleted(watchFactory.PodCoreInformer().Lister(), pod)
	if err != nil {
		return fmt.Errorf("failed cleaning up VM when checking if pod is leftover: %v", err)
	}

	// Do cleanup only if all the pods related to the VM are completed
	if !allVMPodsCompleted {
		return nil
	}

	if err := DeleteDHCPOptions(nbClient, pod); err != nil {
		return err
	}
	if err := DeleteRoutingForMigratedPod(nbClient, pod); err != nil {
		return err
	}
	return nil
}

// SyncVirtualMachines deletes stale OVN resources for missing VMs or VMs in the wrong zone.
func SyncVirtualMachines(nbClient libovsdbclient.Client, vms map[ktypes.NamespacedName]bool, controllerName string) error {
	if err := libovsdbops.DeleteLogicalRouterStaticRoutesWithPredicate(nbClient, ovntypes.OVNClusterRouter, func(item *nbdb.LogicalRouterStaticRoute) bool {
		return ownsItAndIsOrphanOrWrongZone(item.ExternalIDs, vms, controllerName)
	}); err != nil {
		return fmt.Errorf("failed deleting stale vm static routes: %v", err)
	}
	if err := libovsdbops.DeleteLogicalRouterPoliciesWithPredicate(nbClient, ovntypes.OVNClusterRouter, func(item *nbdb.LogicalRouterPolicy) bool {
		return ownsItAndIsOrphanOrWrongZone(item.ExternalIDs, vms, controllerName)
	}); err != nil {
		return fmt.Errorf("failed deleting stale vm policies: %v", err)
	}
	if err := libovsdbops.DeleteDHCPOptionsWithPredicate(nbClient, func(item *nbdb.DHCPOptions) bool {
		return ownsItAndIsOrphanOrWrongZone(item.ExternalIDs, vms, controllerName)
	}); err != nil {
		return fmt.Errorf("failed deleting stale dhcp options: %v", err)
	}
	return nil
}

// FindLiveMigratablePods returns virt-launcher pods filtered by the
// `kubevirt.io/allow-pod-bridge-network-live-migration` annotation.
func FindLiveMigratablePods(watchFactory *factory.WatchFactory) ([]*corev1.Pod, error) {
	vmPods, err := watchFactory.GetAllPodsBySelector(
		metav1.LabelSelector{
			MatchLabels: virtLauncherPodLabel,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("failed looking for live migratable pods: %v", err)
	}
	liveMigratablePods := []*corev1.Pod{}
	for _, vmPod := range vmPods {
		if IsPodLiveMigratable(vmPod) {
			liveMigratablePods = append(liveMigratablePods, vmPod)
		}
	}
	return liveMigratablePods, nil
}

// allocateSyncMigratablePodIPs refills the IP pool when the node has taken over
// the VM subnet for live-migrated VMs.
func allocateSyncMigratablePodIPs(watchFactory *factory.WatchFactory, lsManager *logicalswitchmanager.LogicalSwitchManager, nodeName, nadKey string, pod *corev1.Pod, allocatePodIPsOnSwitch func(*corev1.Pod, []*net.IPNet, string, string) (string, error)) (*ktypes.NamespacedName, string, *util.PodAnnotation, error) {
	isStale, err := IsMigratedSourcePodStale(watchFactory, pod)
	if err != nil {
		return nil, "", nil, err
	}

	// We care only for Running virt-launcher pods
	if isStale {
		return nil, "", nil, nil
	}

	vmDescription, err := NewVMDescriptionFromPod(pod)
	if err != nil {
		return nil, "", nil, err
	}
	if vmDescription == nil {
		return nil, "", nil, nil
	}
	vmKey := vmDescription.Key()

	annotation, err := util.UnmarshalPodAnnotation(pod.Annotations, nadKey)
	if err != nil {
		return nil, "", nil, nil
	}
	switchName, zoneContainsPodSubnet := ZoneContainsPodSubnet(lsManager, annotation.IPs)
	// If this zone does not own the subnet or the passed node does not match the
	// switch, they should not be deallocated.
	if !zoneContainsPodSubnet || (nodeName != "" && switchName != nodeName) {
		return &vmKey, "", annotation, nil
	}
	expectedLogicalPortName, err := allocatePodIPsOnSwitch(pod, lsManager.FilterIPsForSwitch(switchName, annotation.IPs), nadKey, switchName)
	if err != nil {
		return &vmKey, "", nil, err
	}
	return &vmKey, expectedLogicalPortName, annotation, nil
}

// AllocateSyncMigratablePodIPsOnZone refills the IP pool with the pod's IPs if
// those IPs belong to the zone.
func AllocateSyncMigratablePodIPsOnZone(watchFactory *factory.WatchFactory, lsManager *logicalswitchmanager.LogicalSwitchManager, nadKey string, pod *corev1.Pod, allocatePodIPsOnSwitch func(*corev1.Pod, []*net.IPNet, string, string) (string, error)) (*ktypes.NamespacedName, string, *util.PodAnnotation, error) {
	// We care about the whole zone so we pass the nodeName empty
	return allocateSyncMigratablePodIPs(watchFactory, lsManager, "", nadKey, pod, allocatePodIPsOnSwitch)
}

// IsPodOwnedByVirtualMachine returns true if the pod is owned by a
// KubeVirt virtual machine, false otherwise.
func IsPodOwnedByVirtualMachine(pod *corev1.Pod) bool {
	for k, v := range virtLauncherPodLabel {
		if pod.Labels[k] != v {
			return false
		}
	}
	return true
}

// IsPodAllowedForMigration determines whether a given pod is eligible for live migration.
func IsPodAllowedForMigration(pod *corev1.Pod, netInfo util.NetInfo) bool {
	return IsPodOwnedByVirtualMachine(pod) &&
		(netInfo.TopologyType() == ovntypes.Layer2Topology ||
			netInfo.TopologyType() == ovntypes.LocalnetTopology)
}

// LiveMigrationStatusChanged returns true if the live migration status
// changed between the old and new pod. This is a lightweight check suitable
// for use in informer event filters to detect when a migration completes.
func LiveMigrationStatusChanged(oldPod, newPod *corev1.Pod) bool {
	if oldPod == nil || newPod == nil {
		return false
	}
	return oldPod.Annotations[kubevirtv1.MigrationTargetReadyTimestamp] !=
		newPod.Annotations[kubevirtv1.MigrationTargetReadyTimestamp]
}

func isTargetPodReady(targetPod *corev1.Pod) bool {
	if targetPod == nil {
		return false
	}

	// This annotation only appears on live migration scenarios, and it signals
	// that target VM pod is ready to receive traffic, so we can route
	// traffic to it.
	targetReadyTimestamp := targetPod.Annotations[kubevirtv1.MigrationTargetReadyTimestamp]

	// VM is ready to receive traffic
	return targetReadyTimestamp != ""
}

func filterNotComplete(vmPods []*corev1.Pod) []*corev1.Pod {
	var notCompletePods []*corev1.Pod
	for _, vmPod := range vmPods {
		if !util.PodCompleted(vmPod) {
			notCompletePods = append(notCompletePods, vmPod)
		}
	}

	return notCompletePods
}

func tooManyPodsError(livingPods []*corev1.Pod) error {
	var podNames = make([]string, len(livingPods))
	for i := range livingPods {
		podNames[i] = livingPods[i].Namespace + "/" + livingPods[i].Name
	}
	return fmt.Errorf("unexpected live migration state at pods: %s", strings.Join(podNames, ","))
}

// LiveMigrationState represents the various states of a live migration process.
type LiveMigrationState string

const (
	// LiveMigrationInProgress indicates that a live migration is currently ongoing.
	LiveMigrationInProgress LiveMigrationState = "InProgress"

	// LiveMigrationTargetDomainReady indicates that the target domain is ready to take over.
	LiveMigrationTargetDomainReady LiveMigrationState = "TargetDomainReady"

	// LiveMigrationFailed indicates that the live migration process has failed.
	LiveMigrationFailed LiveMigrationState = "Failed"
)

// LiveMigrationStatus provides details about the current status of a live migration.
// It includes information about the source and target pods as well as the migration state.
type LiveMigrationStatus struct {
	SourcePod *corev1.Pod        // SourcePod is the original pod.
	TargetPod *corev1.Pod        // TargetPod is the destination pod.
	State     LiveMigrationState // State is the current state of the live migration.
}

// IsTarget returns whether pod is the migration target.
func (lm LiveMigrationStatus) IsTarget(pod *corev1.Pod) bool {
	return lm.TargetPod.UID == pod.UID
}

// IsTargetDomainReady returns true if the target domain in the live migration process is ready.
func (lm LiveMigrationStatus) IsTargetDomainReady() bool {
	return lm.State == LiveMigrationTargetDomainReady
}

// DiscoverLiveMigrationStatus determines the status of a live migration for a given pod.
// It analyzes the state of pods associated with a VirtualMachine (VM) to identify whether
// a live migration is in progress, the target domain is ready, or the migration has failed.
//
// Note: The function assumes that the pod is part of a VirtualMachine resource managed
// by KubeVirt.
func DiscoverLiveMigrationStatus(podLister listersv1.PodLister, pod *corev1.Pod) (*LiveMigrationStatus, error) {
	vmDescription, err := NewVMDescriptionFromPod(pod)
	if err != nil {
		return nil, err
	}

	if vmDescription == nil {
		return nil, nil
	}

	vmPods, err := vmDescription.OwnedPods(podLister)
	if err != nil {
		return nil, err
	}

	// no migration
	if len(vmPods) < 2 {
		// If the only remaining pod has the migration target ready
		// annotation, the migration completed and the source pod is gone.
		if len(vmPods) == 1 && isTargetPodReady(vmPods[0]) {
			return &LiveMigrationStatus{
				TargetPod: vmPods[0],
				State:     LiveMigrationTargetDomainReady,
			}, nil
		}
		return nil, nil
	}

	// Sort vmPods by creation time
	sort.Slice(vmPods, func(i, j int) bool {
		return vmPods[j].CreationTimestamp.After(vmPods[i].CreationTimestamp.Time)
	})

	targetPod := vmPods[len(vmPods)-1]
	livingPods := filterNotComplete(vmPods)

	// If there is no living pod we should state no live migration status
	if len(livingPods) == 0 {
		return nil, nil
	}

	// There is a living pod but is not the target one so the migration
	// has failed.
	if util.PodCompleted(targetPod) {
		return &LiveMigrationStatus{
			SourcePod: livingPods[0],
			TargetPod: targetPod,
			State:     LiveMigrationFailed,
		}, nil
	}

	// Source pod completed but target is still living. If the target has the
	// migration ready annotation, the migration completed successfully.
	if len(livingPods) < 2 {
		if isTargetPodReady(targetPod) {
			return &LiveMigrationStatus{
				TargetPod: targetPod,
				State:     LiveMigrationTargetDomainReady,
			}, nil
		}
		return nil, nil
	}

	if len(livingPods) > 2 {
		return nil, tooManyPodsError(livingPods)
	}

	status := LiveMigrationStatus{
		SourcePod: livingPods[0],
		TargetPod: livingPods[1],
		State:     LiveMigrationInProgress,
	}

	if isTargetPodReady(status.TargetPod) {
		status.State = LiveMigrationTargetDomainReady
	}
	return &status, nil
}
