package pod

import (
	"fmt"
	"sync"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ref "k8s.io/client-go/tools/reference"
	"k8s.io/klog/v2"

	nettypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/allocator/id"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/allocator/ip/subnet"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/allocator/pod"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/persistentips"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

// PodAllocator acts on pods events handed off by the cluster network controller
// and allocates or releases resources (IPs and tunnel IDs at the time of this
// writing) to pods on behalf of cluster manager.
type PodAllocator struct {
	netInfo util.NetInfo

	// ipAllocator of IPs within subnets
	ipAllocator subnet.Allocator

	// idAllocator of IDs within the network
	idAllocator id.Allocator

	// An utility to allocate the PodAnnotation to pods
	podAnnotationAllocator *pod.PodAnnotationAllocator

	ipamClaimsReconciler persistentips.PersistentAllocations

	networkManager networkmanager.Interface

	// event recorder used to post events to k8s
	recorder record.EventRecorder

	// track pods that have been released but not deleted yet so that we don't
	// release more than once
	releasedPods      map[string]sets.Set[string]
	releasedPodsMutex sync.Mutex
}

// NewPodAllocator builds a new PodAllocator
func NewPodAllocator(
	netInfo util.NetInfo,
	podAnnotationAllocator *pod.PodAnnotationAllocator,
	ipAllocator subnet.Allocator,
	claimsReconciler persistentips.PersistentAllocations,
	networkManager networkmanager.Interface,
	recorder record.EventRecorder,
	idAllocator id.Allocator,
) *PodAllocator {
	podAllocator := &PodAllocator{
		netInfo:                netInfo,
		releasedPods:           map[string]sets.Set[string]{},
		releasedPodsMutex:      sync.Mutex{},
		podAnnotationAllocator: podAnnotationAllocator,
		networkManager:         networkManager,
		recorder:               recorder,
		idAllocator:            idAllocator,
	}

	// this network might not have IPAM, we will just allocate MAC addresses
	if util.DoesNetworkRequireIPAM(netInfo) {
		podAllocator.ipAllocator = ipAllocator
		if config.OVNKubernetesFeature.EnablePersistentIPs && netInfo.AllowsPersistentIPs() {
			podAllocator.ipamClaimsReconciler = claimsReconciler
		}
	}

	return podAllocator
}

// Init checks if persistentIPs controller elements are correctly configured for the network
func (a *PodAllocator) Init() error {
	if a.netInfo.AllowsPersistentIPs() && a.ipamClaimsReconciler == nil {
		return fmt.Errorf(
			"network %q allows persistent IPs but missing the claims reconciler",
			a.netInfo.GetNetworkName(),
		)
	}

	return nil
}

// getActiveNetworkForPod returns the active network for the given pod's namespace
// and is a wrapper around GetActiveNetworkForNamespace
func (a *PodAllocator) getActiveNetworkForPod(pod *corev1.Pod) (util.NetInfo, error) {
	activeNetwork, err := a.networkManager.GetActiveNetworkForNamespace(pod.Namespace)
	if err != nil {
		if util.IsUnprocessedActiveNetworkError(err) {
			a.recordPodErrorEvent(pod, err)
		}
		return nil, err
	}
	return activeNetwork, nil

}

// GetNetworkRole returns the role of this controller's
// network for the given pod
// Expected values are:
// (1) "primary" if this network is the primary network of the pod.
//
//	The "default" network is the primary network of any pod usually
//	unless user-defined-network-segmentation feature has been activated.
//	If network segmentation feature is enabled then any user defined
//	network can be the primary network of the pod.
//
// (2) "secondary" if this network is the secondary network of the pod.
//
//	Only user defined networks can be secondary networks for a pod.
//
// (3) "infrastructure-locked" is applicable only to "default" network if
//
//	a user defined network is the "primary" network for this pod. This
//	signifies the "default" network is only used for probing and
//	is otherwise locked for all intents and purposes.
//
// NOTE: Like in other places, expectation is this function is always called
// from controller's that have some relation to the given pod, unrelated
// networks are treated as secondary networks so caller has to be careful
func (a *PodAllocator) GetNetworkRole(pod *corev1.Pod) (string, error) {
	if !util.IsNetworkSegmentationSupportEnabled() {
		// if user defined network segmentation is not enabled
		// then we know pod's primary network is "default" and
		// pod's secondary network is NOT its primary network
		if a.netInfo.IsDefault() {
			return types.NetworkRolePrimary, nil
		}
		return types.NetworkRoleSecondary, nil
	}
	activeNetwork, err := a.getActiveNetworkForPod(pod)
	if err != nil {
		return "", err
	}
	if activeNetwork.GetNetworkName() == a.netInfo.GetNetworkName() {
		return types.NetworkRolePrimary, nil
	}
	if a.netInfo.IsDefault() {
		// if default network was not the primary network,
		// then when UDN is turned on, default network is the
		// infrastructure-locked network forthis pod
		return types.NetworkRoleInfrastructure, nil
	}
	return types.NetworkRoleSecondary, nil
}

// Reconcile allocates or releases IPs for pods updating the pod annotation
// as necessary with all the additional information derived from those IPs
func (a *PodAllocator) Reconcile(old, new *corev1.Pod) error {
	releaseFromAllocator := true
	return a.reconcile(old, new, releaseFromAllocator)
}

// Sync initializes the allocator with pods that already exist on the cluster
func (a *PodAllocator) Sync(objs []interface{}) error {
	// on sync, we don't release IPs from the allocator, we are just trying to
	// allocate annotated IPs; specifically we don't want to release IPs of
	// completed pods that might be being used by other pods
	releaseFromAllocator := false

	for _, obj := range objs {
		pod, ok := obj.(*corev1.Pod)
		if !ok {
			klog.Errorf("Could not cast %T object to *corev1.Pod", obj)
			continue
		}
		err := a.reconcile(nil, pod, releaseFromAllocator)
		if err != nil {
			klog.Errorf("Failed to sync pod %s/%s: %v", pod.Namespace, pod.Name, err)
		}
	}

	return nil
}

func (a *PodAllocator) reconcile(old, new *corev1.Pod, releaseFromAllocator bool) error {
	var pod *corev1.Pod
	if old != nil {
		pod = old
	}
	if new != nil {
		pod = new
	}

	podScheduled := util.PodScheduled(pod)
	podWantsHostNetwork := util.PodWantsHostNetwork(pod)

	// nothing to do for a unscheduled or host network pods
	if !podScheduled || podWantsHostNetwork {
		return nil
	}

	activeNetwork, err := a.getActiveNetworkForPod(pod)
	if err != nil {
		return fmt.Errorf("failed looking for an active network: %w", err)
	}

	onNetwork, networkMap, err := util.GetPodNADToNetworkMappingWithActiveNetwork(pod, a.netInfo, activeNetwork)
	if err != nil {
		a.recordPodErrorEvent(pod, err)
		return fmt.Errorf("failed to get NAD to network mapping: %w", err)
	}

	// nothing to do if not on this network
	// Note: we are not considering a hotplug scenario where we would have to
	// release IPs if the pod was unplugged from the network
	if !onNetwork {
		return nil
	}

	// reconcile for each NAD
	for nadName, network := range networkMap {
		err = a.reconcileForNAD(old, new, nadName, network, releaseFromAllocator)
		if err != nil {
			return err
		}
	}

	return nil
}

func (a *PodAllocator) reconcileForNAD(old, new *corev1.Pod, nad string, network *nettypes.NetworkSelectionElement, releaseIPsFromAllocator bool) error {
	var pod *corev1.Pod
	if old != nil {
		pod = old
	}
	if new != nil {
		pod = new
	}
	podDeleted := new == nil
	podCompleted := util.PodCompleted(pod)

	if podCompleted || podDeleted {
		return a.releasePodOnNAD(pod, nad, network, podDeleted, releaseIPsFromAllocator)
	}

	return a.allocatePodOnNAD(pod, nad, network)
}

func (a *PodAllocator) releasePodOnNAD(pod *corev1.Pod, nad string, network *nettypes.NetworkSelectionElement,
	podDeleted, releaseFromAllocator bool) error {
	podAnnotation, _ := util.UnmarshalPodAnnotation(pod.Annotations, nad)
	if podAnnotation == nil {
		// track release pods even if they have no annotation in case a user
		// might have removed it manually
		podAnnotation = &util.PodAnnotation{}
	}

	uid := string(pod.UID)

	hasIPAM := util.DoesNetworkRequireIPAM(a.netInfo)
	hasIDAllocation := util.DoesNetworkRequireTunnelIDs(a.netInfo)

	hasPersistentIPs := a.netInfo.AllowsPersistentIPs() && hasIPAM
	hasIPAMClaim := network != nil && network.IPAMClaimReference != ""
	if hasIPAMClaim && !hasPersistentIPs {
		klog.Errorf(
			"Pod %s/%s referencing an IPAMClaim on network %q which does not honor it",
			pod.GetNamespace(),
			pod.GetName(),
			a.netInfo.GetNetworkName(),
		)
		hasIPAMClaim = false
	}
	if hasIPAMClaim {
		ipamClaim, err := a.ipamClaimsReconciler.FindIPAMClaim(network.IPAMClaimReference, network.Namespace)
		hasIPAMClaim = ipamClaim != nil && len(ipamClaim.Status.IPs) > 0
		if apierrors.IsNotFound(err) {
			klog.Errorf("Failed to retrieve IPAMClaim %q but will release IPs: %v", network.IPAMClaimReference, err)
		} else if err != nil {
			return fmt.Errorf("failed to get IPAMClaim %s/%s: %w", network.Namespace, network.IPAMClaimReference, err)
		}
	}

	if !hasIPAM && !hasIDAllocation {
		// we only take care of IP and tunnel ID allocation, if neither were
		// allocated we have nothing to do
		return nil
	}

	// do not release from the allocators if not flaged to do so or if they
	// were already previosuly released
	doRelease := releaseFromAllocator && !a.isPodReleased(nad, uid)
	doReleaseIDs := doRelease && hasIDAllocation
	doReleaseIPs := doRelease && hasIPAM && !hasIPAMClaim

	if doReleaseIDs {
		name := podIdAllocationName(nad, uid)
		a.idAllocator.ReleaseID(name)
		klog.V(5).Infof("Released ID %d", podAnnotation.TunnelID)
	}

	if doReleaseIPs {
		err := a.ipAllocator.ReleaseIPs(a.netInfo.GetNetworkName(), podAnnotation.IPs)
		if err != nil {
			return fmt.Errorf("failed to release ips %v for pod %s/%s and nad %s: %w",
				util.StringSlice(podAnnotation.IPs),
				pod.Name,
				pod.Namespace,
				nad,
				err,
			)
		}
		klog.V(5).Infof("Released IPs %v", util.StringSlice(podAnnotation.IPs))
	}

	if podDeleted {
		a.deleteReleasedPod(nad, string(pod.UID))
	} else {
		a.addReleasedPod(nad, string(pod.UID))
	}

	return nil
}

func (a *PodAllocator) allocatePodOnNAD(pod *corev1.Pod, nad string, network *nettypes.NetworkSelectionElement) error {
	var ipAllocator subnet.NamedAllocator
	if util.DoesNetworkRequireIPAM(a.netInfo) {
		ipAllocator = a.ipAllocator.ForSubnet(a.netInfo.GetNetworkName())
	}

	var idAllocator id.NamedAllocator
	if util.DoesNetworkRequireTunnelIDs(a.netInfo) {
		name := podIdAllocationName(nad, string(pod.UID))
		idAllocator = a.idAllocator.ForName(name)
	}

	// don't reallocate to new IPs if currently annotated IPs fail to alloccate
	reallocate := false
	networkRole, err := a.GetNetworkRole(pod)
	if err != nil {
		return err
	}
	updatedPod, podAnnotation, err := a.podAnnotationAllocator.AllocatePodAnnotationWithTunnelID(
		ipAllocator,
		idAllocator,
		pod,
		network,
		reallocate,
		networkRole,
	)

	if err != nil {
		return err
	}

	if updatedPod != nil {
		klog.V(5).Infof("Allocated IP addresses %v, mac address %s, gateways %v, routes %s and tunnel id %d for pod %s/%s on nad %s",
			util.StringSlice(podAnnotation.IPs),
			podAnnotation.MAC,
			util.StringSlice(podAnnotation.Gateways),
			util.StringSlice(podAnnotation.Routes),
			podAnnotation.TunnelID,
			pod.Namespace, pod.Name, nad,
		)
	}

	return err
}

func (a *PodAllocator) addReleasedPod(nad, uid string) {
	a.releasedPodsMutex.Lock()
	defer a.releasedPodsMutex.Unlock()
	releasedPods := a.releasedPods[nad]
	if releasedPods == nil {
		a.releasedPods[nad] = sets.New(uid)
		return
	}
	releasedPods.Insert(uid)
}

func (a *PodAllocator) deleteReleasedPod(nad, uid string) {
	a.releasedPodsMutex.Lock()
	defer a.releasedPodsMutex.Unlock()
	releasedPods := a.releasedPods[nad]
	if releasedPods != nil {
		releasedPods.Delete(uid)
		if releasedPods.Len() == 0 {
			delete(a.releasedPods, nad)
		}
	}
}

func (a *PodAllocator) isPodReleased(nad, uid string) bool {
	a.releasedPodsMutex.Lock()
	defer a.releasedPodsMutex.Unlock()
	releasedPods := a.releasedPods[nad]
	if releasedPods != nil {
		return releasedPods.Has(uid)
	}
	return false
}

func (a *PodAllocator) recordPodErrorEvent(pod *corev1.Pod, podErr error) {
	podRef, err := ref.GetReference(scheme.Scheme, pod)
	if err != nil {
		klog.Errorf("Couldn't get a reference to pod %s/%s to post an event: '%v'",
			pod.Namespace, pod.Name, err)
	} else {
		klog.V(5).Infof("Posting a %s event for Pod %s/%s", corev1.EventTypeWarning, pod.Namespace, pod.Name)
		a.recorder.Eventf(podRef, corev1.EventTypeWarning, "ErrorAllocatingPod", podErr.Error())
	}
}

func podIdAllocationName(nad, uid string) string {
	return fmt.Sprintf("%s/%s", nad, uid)
}
