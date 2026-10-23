// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package pod

import (
	"context"
	"errors"
	"fmt"
	"sync"

	nettypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes/scheme"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/record"
	ref "k8s.io/client-go/tools/reference"
	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/allocator/id"
	ipallocator "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/allocator/ip"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/allocator/ip/subnet"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/allocator/mac"
	podallocator "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/allocator/pod"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/persistentips"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

const (
	// First tunnel key available for pods. Keys 1-9 are reserved for platform
	// ports (e.g. transit router to switch port). Matches
	// transitRouterPortFirstTunnelKey in transit_router.go.
	transitRouterFirstPodTunnelKey = 10
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
	podAnnotationAllocator *podallocator.PodAnnotationAllocator

	ipamClaimsReconciler persistentips.PersistentAllocations

	networkManager networkmanager.Interface

	// event recorder used to post events to k8s
	recorder record.EventRecorder

	// track pods that have been released but not deleted yet so that we don't
	// release more than once
	releasedPods      map[string]sets.Set[string]
	releasedPodsMutex sync.Mutex

	nodeLister corev1listers.NodeLister
}

// NewPodAllocator builds a new PodAllocator
func NewPodAllocator(
	netInfo util.NetInfo,
	podAnnotationAllocator *podallocator.PodAnnotationAllocator,
	ipAllocator subnet.Allocator,
	claimsReconciler persistentips.PersistentAllocations,
	networkManager networkmanager.Interface,
	recorder record.EventRecorder,
	idAllocator id.Allocator,
	nodeLister corev1listers.NodeLister,
) *PodAllocator {
	podAllocator := &PodAllocator{
		netInfo:                netInfo,
		releasedPods:           map[string]sets.Set[string]{},
		releasedPodsMutex:      sync.Mutex{},
		podAnnotationAllocator: podAnnotationAllocator,
		networkManager:         networkManager,
		recorder:               recorder,
		idAllocator:            idAllocator,
		nodeLister:             nodeLister,
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

	klog.Infof("Initializing network %s pod annotation allocator MAC registry", a.netInfo.GetNetworkName())
	if err := a.podAnnotationAllocator.InitializeMACRegistry(); err != nil {
		return fmt.Errorf("failed to initialize MAC addresses registry: %w", err)
	}

	return nil
}

// getActiveNetworkForPod returns the active network for the given pod's namespace
// and is a wrapper around GetActiveNetworkForNamespace
func (a *PodAllocator) getActiveNetworkForPod(pod *corev1.Pod) (util.NetInfo, error) {
	activeNetwork, err := a.networkManager.GetActiveNetworkForNamespace(pod.Namespace)
	if err != nil {
		if util.IsInvalidPrimaryNetworkError(err) {
			a.recordPodErrorEvent(pod, err)
		}
		return nil, err
	}
	// Cluster manager pod allocation should always have an active network
	if activeNetwork == nil {
		newErr := fmt.Errorf("no active network found for pod %s/%s", pod.Namespace, pod.Name)
		a.recordPodErrorEvent(pod, newErr)
		return nil, newErr
	}
	return activeNetwork, nil

}

// GetNetworkRole returns the role of this controller's network for the given pod
func (a *PodAllocator) GetNetworkRole(pod *corev1.Pod) (string, error) {
	role, err := util.GetNetworkRole(
		a.netInfo,
		a.networkManager.GetPrimaryNADForNamespace,
		a.networkManager.GetNetworkNameForNADKey,
		pod,
	)
	if err != nil {
		if util.IsInvalidPrimaryNetworkError(err) {
			a.recordPodErrorEvent(pod, err)
		}
		return "", err
	}

	return role, nil
}

// Reconcile allocates or releases IPs for pods updating the pod annotation
// as necessary with all the additional information derived from those IPs
func (a *PodAllocator) Reconcile(ctx context.Context, old, new *corev1.Pod) error {
	releaseFromAllocator := true
	return a.reconcile(ctx, old, new, releaseFromAllocator)
}

// Sync initializes the allocator with pods that already exist on the cluster
func (a *PodAllocator) Sync(ctx context.Context, objs []interface{}) error {
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
		err := a.reconcile(ctx, nil, pod, releaseFromAllocator)
		if err != nil {
			klog.Errorf("Failed to sync pod %s/%s: %v", pod.Namespace, pod.Name, err)
		}
	}

	// Reserve tunnel IDs used by OVN platform ports so that they are not
	// allocated to pods. Done after syncing existing pods: on upgrade, pods
	// may already hold these IDs; those are skipped and reclaimed on delete.
	if a.idAllocator != nil && config.Layer2UsesTransitRouter {
		for i := 1; i < transitRouterFirstPodTunnelKey; i++ {
			if err := a.idAllocator.ReserveID(fmt.Sprintf("reserved-platform-port-%d", i), i); err != nil {
				klog.Infof("Platform port tunnel key %d already in use: %v", i, err)
			}
		}
	}

	return nil
}

func (a *PodAllocator) reconcile(ctx context.Context, old, new *corev1.Pod, releaseFromAllocator bool) error {
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

	var activeNetwork util.NetInfo
	var err error

	if new != nil {
		if a.netInfo.IsPrimaryNetwork() {
			activeNetwork, err = a.getActiveNetworkForPod(pod)
			if err != nil {
				return fmt.Errorf("failed looking for an active network: %w", err)
			}
		}
	} else if a.netInfo.IsPrimaryNetwork() {
		// During pod deletion, the UDN might already be removed. To handle this, check if the activeNetwork
		// is managed by the current allocator using pod annotations. If not, exit as there is nothing to do.
		// There is no need to handle non-primary networks as they are already present in the network-selection
		// annotations handled in GetPodNADToNetworkMappingWithActiveNetwork.
		podNetworks, err := util.UnmarshalPodAnnotationAllNetworks(pod.Annotations)
		if err != nil {
			return err
		}
		for nadKey := range podNetworks {
			networkName := a.networkManager.GetNetworkNameForNADKey(nadKey)
			if networkName != "" && networkName == a.netInfo.GetNetworkName() {
				activeNetwork = a.netInfo
				break
			}
		}
		if activeNetwork == nil {
			klog.V(5).Infof("Skipping pod cleanup for %s/%s on network %s", pod.Namespace, pod.Name, a.netInfo.GetNetworkName())
			return nil
		}
	}

	onNetwork, networkMap, err := util.GetPodNADToNetworkMappingWithActiveNetwork(
		pod,
		a.netInfo,
		activeNetwork,
		a.networkManager.GetNetworkNameForNADKey,
		a.networkManager.GetPrimaryNADForNamespace,
	)
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
	for nadKey, network := range networkMap {
		err = a.reconcileForNAD(ctx, old, new, nadKey, network, releaseFromAllocator)
		if err != nil {
			return err
		}
	}

	return nil
}

func (a *PodAllocator) reconcileForNAD(ctx context.Context, old, new *corev1.Pod, nadKey string, network *nettypes.NetworkSelectionElement, releaseIPsFromAllocator bool) error {
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
		return a.releasePodOnNAD(ctx, pod, nadKey, network, podDeleted, releaseIPsFromAllocator)
	}

	return a.allocatePodOnNAD(ctx, pod, nadKey, network)
}

func (a *PodAllocator) releasePodOnNAD(_ context.Context, pod *corev1.Pod, nadKey string, network *nettypes.NetworkSelectionElement,
	podDeleted, releaseFromAllocator bool) error {
	podAnnotation, _ := util.UnmarshalPodAnnotation(pod.Annotations, nadKey)
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
		var err error
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
	doRelease := releaseFromAllocator && !a.isPodReleased(nadKey, uid)
	doReleaseIDs := doRelease && hasIDAllocation
	doReleaseIPs := doRelease && hasIPAM && !hasIPAMClaim

	if doReleaseIDs {
		name := podIdAllocationName(nadKey, uid)
		a.idAllocator.ReleaseID(name)
		klog.V(5).Infof("Released ID %d", podAnnotation.TunnelID)
	}

	if doReleaseIPs {
		err := a.ipAllocator.ReleaseIPs(a.netInfo.GetNetworkName(), podIdAllocationName(nadKey, string(pod.UID)), podAnnotation.IPs)
		if err != nil {
			return fmt.Errorf("failed to release ips %v for pod %s/%s and NAD key %s: %w",
				util.StringSlice(podAnnotation.IPs),
				pod.Name,
				pod.Namespace,
				nadKey,
				err,
			)
		}
		klog.V(5).Infof("Released IPs %v", util.StringSlice(podAnnotation.IPs))
	}

	if doRelease {
		if err := a.podAnnotationAllocator.ReleasePodReservedMacAddress(pod, podAnnotation.MAC); err != nil {
			return fmt.Errorf(`failed to release pod "%s/%s" mac %q: %v`,
				pod.Namespace, pod.Name, podAnnotation.MAC, err)
		}
	}

	if podDeleted {
		a.deleteReleasedPod(nadKey, string(pod.UID))
	} else {
		a.addReleasedPod(nadKey, string(pod.UID))
	}

	return nil
}

func (a *PodAllocator) allocatePodOnNAD(ctx context.Context, pod *corev1.Pod, nadKey string, network *nettypes.NetworkSelectionElement) error {
	var ipAllocator subnet.IPAllocator
	if util.DoesNetworkRequireIPAM(a.netInfo) {
		ipAllocator = a.ipAllocator
	}

	var idAllocator id.NamedAllocator
	if util.DoesNetworkRequireTunnelIDs(a.netInfo) {
		name := podIdAllocationName(nadKey, string(pod.UID))
		idAllocator = a.idAllocator.ForName(name)
	}

	// don't reallocate to new IPs if currently annotated IPs fail to allocate
	reallocate := false
	networkRole, err := a.GetNetworkRole(pod)
	if err != nil {
		return err
	}

	if networkRole == types.NetworkRoleNone {
		// pod not on this controller, nothing to do
		return nil
	}

	updatedPod, podAnnotation, err := a.podAnnotationAllocator.AllocatePodAnnotationWithTunnelID(
		ctx,
		ipAllocator,
		a.netInfo.GetNetworkName(), podIdAllocationName(nadKey, string(pod.UID)),
		idAllocator,
		pod,
		nadKey,
		network,
		reallocate,
		networkRole,
	)

	if err != nil {
		if errors.Is(err, ipallocator.ErrFull) ||
			ipallocator.IsErrAllocated(err) ||
			errors.Is(err, mac.ErrReserveMACConflict) ||
			errors.Is(err, podallocator.ErrIPFamilyMismatch) {
			a.recordPodErrorEvent(pod, err)
		}
		return err
	}

	if updatedPod != nil {
		klog.V(5).Infof("Allocated IP addresses %v, mac address %s, gateways %v, routes %s and tunnel id %d for pod %s/%s on NAD key %s",
			util.StringSlice(podAnnotation.IPs),
			podAnnotation.MAC,
			util.StringSlice(podAnnotation.Gateways),
			util.StringSlice(podAnnotation.Routes),
			podAnnotation.TunnelID,
			pod.Namespace, pod.Name, nadKey,
		)
	}

	return err
}

func (a *PodAllocator) addReleasedPod(nadKey, uid string) {
	a.releasedPodsMutex.Lock()
	defer a.releasedPodsMutex.Unlock()
	releasedPods := a.releasedPods[nadKey]
	if releasedPods == nil {
		a.releasedPods[nadKey] = sets.New(uid)
		return
	}
	releasedPods.Insert(uid)
}

func (a *PodAllocator) deleteReleasedPod(nadKey, uid string) {
	a.releasedPodsMutex.Lock()
	defer a.releasedPodsMutex.Unlock()
	releasedPods := a.releasedPods[nadKey]
	if releasedPods != nil {
		releasedPods.Delete(uid)
		if releasedPods.Len() == 0 {
			delete(a.releasedPods, nadKey)
		}
	}
}

func (a *PodAllocator) isPodReleased(nadKey, uid string) bool {
	a.releasedPodsMutex.Lock()
	defer a.releasedPodsMutex.Unlock()
	releasedPods := a.releasedPods[nadKey]
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
		a.recorder.Eventf(podRef, corev1.EventTypeWarning, "ErrorAllocatingPod", "%s", podErr.Error())
	}
}

func podIdAllocationName(nadKey, uid string) string {
	return fmt.Sprintf("%s/%s", nadKey, uid)
}
