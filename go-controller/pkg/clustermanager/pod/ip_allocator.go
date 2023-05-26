package pod

import (
	"fmt"
	"net"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"

	nettypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/allocator/ip/subnet"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/allocator/pod"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

// PodIPAllocator acts on pods events handed off by the cluster network
// controller and allocates or releases IPs for them updating the pod annotation
// as necessary with all the additional information derived from those IPs.
type PodIPAllocator struct {
	netInfo util.NetInfo

	// allocator of IPs within subnets
	allocator subnet.Allocator

	// An utility to allocate the PodAnnotation to pods
	podAnnotationAllocator *pod.PodAnnotationAllocator

	// track pods that have been released but not deleted yet so that we don't
	// release more than once
	releasedPods      map[string]sets.Set[string]
	releasedPodsMutex sync.Mutex
}

// NewPodIPAllocator builds a new PodIPAllocator
func NewPodIPAllocator(netInfo util.NetInfo, podLister listers.PodLister, kube kube.Interface) *PodIPAllocator {
	podAnnotationAllocator := pod.NewPodAnnotationAllocator(
		netInfo,
		podLister,
		kube,
	)

	podIPAllocator := &PodIPAllocator{
		netInfo:                netInfo,
		releasedPods:           map[string]sets.Set[string]{},
		releasedPodsMutex:      sync.Mutex{},
		podAnnotationAllocator: podAnnotationAllocator,
	}

	// this network might not have IPAM, we will just allocate MAC addresses
	if util.DoesNetworkRequireIPAM(netInfo) {
		podIPAllocator.allocator = subnet.NewAllocator()
	}

	return podIPAllocator
}

// InitRanges initializes the allocator with the subnets configured for the
// network
func (a *PodIPAllocator) InitRanges() error {
	if a.netInfo.TopologyType() != types.LocalnetTopology {
		return fmt.Errorf("topology %s not supported", a.netInfo.TopologyType())
	}

	subnets := a.netInfo.Subnets()
	ipNets := make([]*net.IPNet, 0, len(subnets))
	for _, subnet := range subnets {
		ipNets = append(ipNets, subnet.CIDR)
	}
	return a.allocator.AddOrUpdateSubnet(a.netInfo.GetNetworkName(), ipNets, a.netInfo.ExcludeSubnets()...)
}

// Reconcile allocates or releases IPs for pods updating the pod annotation
// as necessary with all the additional information derived from those IPs
func (a *PodIPAllocator) Reconcile(old, new *corev1.Pod) error {
	releaseIPsFromAllocator := true
	return a.reconcile(old, new, releaseIPsFromAllocator)
}

// Sync initializes the allocator with pods that already exist on the cluster
func (a *PodIPAllocator) Sync(objs []interface{}) error {
	// on sync, we don't release IPs from the allocator, we are just trying to
	// allocate annotated IPs; specifically we don't want to release IPs of
	// completed pods that might be being used by other pods
	releaseIPsFromAllocator := false

	for _, obj := range objs {
		pod, ok := obj.(*corev1.Pod)
		if !ok {
			klog.Errorf("Could not cast %T object to *corev1.Pod", obj)
			continue
		}
		err := a.reconcile(nil, pod, releaseIPsFromAllocator)
		if err != nil {
			klog.Errorf("Failed to sync pod %s/%s: %w", pod.Namespace, pod.Name, err)
		}
	}

	return nil
}

func (a *PodIPAllocator) reconcile(old, new *corev1.Pod, releaseIPsFromAllocator bool) error {
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

	onNetwork, networkMap, err := util.GetPodNADToNetworkMapping(pod, a.netInfo)
	if err != nil {
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
		err = a.reconcileForNAD(old, new, nadName, network, releaseIPsFromAllocator)
		if err != nil {
			return err
		}
	}

	return nil
}

func (a *PodIPAllocator) reconcileForNAD(old, new *corev1.Pod, nad string, network *nettypes.NetworkSelectionElement, releaseIPsFromAllocator bool) error {
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
		return a.releasePodOnNAD(pod, nad, podDeleted, releaseIPsFromAllocator)
	}

	return a.allocatePodOnNAD(pod, nad, network)
}

func (a *PodIPAllocator) releasePodOnNAD(pod *corev1.Pod, nad string, podDeleted, releaseIPsFromAllocator bool) error {
	if !util.DoesNetworkRequireIPAM(a.netInfo) {
		// no need to release if no IPAM
		return nil
	}

	podAnnotation, _ := util.UnmarshalPodAnnotation(pod.Annotations, nad)
	if podAnnotation == nil {
		// track release pods even if they have no annotation in case a user
		// might have removed it manually
		podAnnotation = &util.PodAnnotation{}
	}

	uid := string(pod.UID)

	// do not release IPs from the allocator if not flaged to do so or if they
	// werea already previosuly released
	if releaseIPsFromAllocator && !a.isPodReleased(nad, uid) {
		err := a.allocator.ReleaseIPs(a.netInfo.GetNetworkName(), podAnnotation.IPs)
		if err != nil {
			return fmt.Errorf("failed to release ips %v for pod %s/%s and nad %s: %w",
				util.StringSlice(podAnnotation.IPs),
				pod.Name,
				pod.Namespace,
				nad,
				err,
			)
		}
	}

	if podDeleted {
		a.deleteReleasedPod(nad, string(pod.UID))
	} else {
		a.addReleasedPod(nad, string(pod.UID))
	}

	return nil
}

func (a *PodIPAllocator) allocatePodOnNAD(pod *corev1.Pod, nad string, network *nettypes.NetworkSelectionElement) error {
	var ipAllocator subnet.NamedAllocator
	if util.DoesNetworkRequireIPAM(a.netInfo) {
		ipAllocator = a.allocator.ForSubnet(a.netInfo.GetNetworkName())
	}

	// don't reallocate to new IPs if currently annotated IPs fail to alloccate
	reallocate := false

	updatedPod, podAnnotation, err := a.podAnnotationAllocator.AllocatePodAnnotation(
		ipAllocator,
		pod,
		network,
		reallocate,
	)

	if err != nil {
		return err
	}

	if updatedPod != nil {
		klog.V(5).Infof("Allocated IP addresses %v, mac address %s, gateways %v and routes %s for pod %s/%s on nad %s",
			util.StringSlice(podAnnotation.IPs),
			podAnnotation.MAC,
			util.StringSlice(podAnnotation.Gateways),
			util.StringSlice(podAnnotation.Routes),
			pod.Namespace, pod.Name, nad,
		)
	}

	return err
}

func (a *PodIPAllocator) addReleasedPod(nad, uid string) {
	a.releasedPodsMutex.Lock()
	defer a.releasedPodsMutex.Unlock()
	releasedPods := a.releasedPods[nad]
	if releasedPods == nil {
		a.releasedPods[nad] = sets.New(uid)
		return
	}
	releasedPods.Insert(uid)
}

func (a *PodIPAllocator) deleteReleasedPod(nad, uid string) {
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

func (a *PodIPAllocator) isPodReleased(nad, uid string) bool {
	a.releasedPodsMutex.Lock()
	defer a.releasedPodsMutex.Unlock()
	releasedPods := a.releasedPods[nad]
	if releasedPods != nil {
		return releasedPods.Has(uid)
	}
	return false
}
