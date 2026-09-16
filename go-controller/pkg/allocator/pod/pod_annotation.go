// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package pod

import (
	"errors"
	"fmt"
	"net"

	ipamclaimsapi "github.com/k8snetworkplumbingwg/ipamclaims/pkg/crd/ipamclaims/v1alpha1"
	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	corev1 "k8s.io/api/core/v1"
	listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/allocator/id"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/allocator/ip"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/allocator/ip/subnet"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/allocator/mac"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/generator/udn"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/persistentips"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// PodAnnotationAllocator is a utility to handle allocation of the PodAnnotation to Pods.
type PodAnnotationAllocator struct {
	podLister listers.PodLister
	kube      kube.InterfaceOVN

	netInfo              util.NetInfo
	ipamClaimsReconciler persistentips.PersistentAllocations
	macRegistry          mac.Register
}

type AllocatorOption func(*PodAnnotationAllocator)

func NewPodAnnotationAllocator(
	netInfo util.NetInfo,
	podLister listers.PodLister,
	kube kube.InterfaceOVN,
	claimsReconciler persistentips.PersistentAllocations,
	opts ...AllocatorOption,
) *PodAnnotationAllocator {
	p := &PodAnnotationAllocator{
		podLister:            podLister,
		kube:                 kube,
		netInfo:              netInfo,
		ipamClaimsReconciler: claimsReconciler,
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

func WithMACRegistry(m mac.Register) AllocatorOption {
	return func(p *PodAnnotationAllocator) {
		p.macRegistry = m
	}
}

// AllocatePodAnnotation allocates the PodAnnotation which includes IPs, a mac
// address, routes and gateways. Returns the allocated pod annotation and the
// updated pod. Returns a nil pod and the existing PodAnnotation if no updates
// are warranted to the pod.
//
// The allocation can be requested through the network selection element or
// derived from the allocator provided IPs. If the requested IPs cannot be
// honored, a new set of IPs will be allocated unless reallocateIP is set to
// false. When requireIPAMReservation is true, an ErrAllocated response is not
// treated as proof that an existing annotation still owns its IPs.
func (allocator *PodAnnotationAllocator) AllocatePodAnnotation(
	ipAllocator subnet.NamedAllocator,
	node *corev1.Node,
	pod *corev1.Pod,
	nadKey string,
	network *nadapi.NetworkSelectionElement,
	reallocateIP bool,
	requireIPAMReservation bool,
	networkRole string) (
	*corev1.Pod,
	*util.PodAnnotation,
	error) {

	return allocatePodAnnotation(
		allocator.podLister,
		allocator.kube,
		ipAllocator,
		allocator.netInfo,
		node,
		pod,
		nadKey,
		network,
		allocator.ipamClaimsReconciler,
		allocator.macRegistry,
		reallocateIP,
		requireIPAMReservation,
		networkRole,
	)
}

func allocatePodAnnotation(
	podLister listers.PodLister,
	kube kube.Interface,
	ipAllocator subnet.NamedAllocator,
	netInfo util.NetInfo,
	node *corev1.Node,
	pod *corev1.Pod,
	nadKey string,
	network *nadapi.NetworkSelectionElement,
	claimsReconciler persistentips.PersistentAllocations,
	macRegistry mac.Register,
	reallocateIP bool,
	requireIPAMReservation bool,
	networkRole string) (
	updatedPod *corev1.Pod,
	podAnnotation *util.PodAnnotation,
	err error) {

	// no id allocation
	var idAllocator id.NamedAllocator

	allocateToPodWithRollback := func(currentPod *corev1.Pod) (*corev1.Pod, func(), error) {
		var rollback func()
		updatedPod, podAnnotation, rollback, err = allocatePodAnnotationWithRollback(
			ipAllocator,
			idAllocator,
			netInfo,
			node,
			currentPod,
			nadKey,
			network,
			claimsReconciler,
			macRegistry,
			reallocateIP,
			requireIPAMReservation,
			networkRole,
		)
		return updatedPod, rollback, err
	}

	err = util.UpdatePodWithRetryOrRollback(
		podLister,
		kube,
		pod,
		allocateToPodWithRollback,
	)

	if err != nil {
		return nil, nil, err
	}

	return updatedPod, podAnnotation, nil
}

// AllocatePodAnnotationWithTunnelID allocates the PodAnnotation which includes
// IPs, a mac address, routes, gateways and a tunnel ID. Returns the allocated
// pod annotation and the updated pod. Returns a nil pod and the existing
// PodAnnotation if no updates are warranted to the pod.
//
// The allocation can be requested through the network selection element or
// derived from the allocator provided IPs. If the requested IPs cannot be
// honored, a new set of IPs will be allocated unless reallocateIP is set to
// false.
func (allocator *PodAnnotationAllocator) AllocatePodAnnotationWithTunnelID(
	ipAllocator subnet.NamedAllocator,
	idAllocator id.NamedAllocator,
	node *corev1.Node,
	pod *corev1.Pod,
	nadKey string,
	network *nadapi.NetworkSelectionElement,
	reallocateIP bool,
	networkRole string) (
	*corev1.Pod,
	*util.PodAnnotation,
	error) {

	return allocatePodAnnotationWithTunnelID(
		allocator.podLister,
		allocator.kube,
		ipAllocator,
		idAllocator,
		allocator.netInfo,
		node,
		pod,
		nadKey,
		network,
		allocator.ipamClaimsReconciler,
		allocator.macRegistry,
		reallocateIP,
		networkRole,
	)
}

func allocatePodAnnotationWithTunnelID(
	podLister listers.PodLister,
	kube kube.Interface,
	ipAllocator subnet.NamedAllocator,
	idAllocator id.NamedAllocator,
	netInfo util.NetInfo,
	node *corev1.Node,
	pod *corev1.Pod,
	nadKey string,
	network *nadapi.NetworkSelectionElement,
	claimsReconciler persistentips.PersistentAllocations,
	macRegistry mac.Register,
	reallocateIP bool,
	networkRole string) (
	updatedPod *corev1.Pod,
	podAnnotation *util.PodAnnotation,
	err error) {

	allocateToPodWithRollback := func(currentPod *corev1.Pod) (*corev1.Pod, func(), error) {
		var rollback func()
		updatedPod, podAnnotation, rollback, err = allocatePodAnnotationWithRollback(
			ipAllocator,
			idAllocator,
			netInfo,
			node,
			currentPod,
			nadKey,
			network,
			claimsReconciler,
			macRegistry,
			reallocateIP,
			false,
			networkRole,
		)
		return updatedPod, rollback, err
	}

	err = util.UpdatePodWithRetryOrRollback(
		podLister,
		kube,
		pod,
		allocateToPodWithRollback,
	)

	if err != nil {
		return nil, nil, err
	}

	return updatedPod, podAnnotation, nil
}

// validateStaticIPRequest checks if a static IP request can be honored when IPAM is enabled for the given network.
func validateStaticIPRequest(netInfo util.NetInfo, network *nadapi.NetworkSelectionElement, ipamClaim *ipamclaimsapi.IPAMClaim, podDesc string) error {
	// Allow static IPs with IPAM only for primary networks with layer2 topology when EnablePreconfiguredUDNAddresses is enabled
	// Feature gate integration: EnablePreconfiguredUDNAddresses controls static IP allocation with IPAM
	if !util.IsPreconfiguredUDNAddressesEnabled() {
		// Feature is disabled, reject static IPs with IPAM
		return fmt.Errorf("cannot allocate a static IP request with IPAM for pod %s (custom network configuration disabled)", podDesc)
	}
	if !netInfo.IsPrimaryNetwork() {
		// Static IP requests with IPAM are only supported on primary networks
		return fmt.Errorf("cannot allocate a static IP request with IPAM for pod %s: only supported on primary networks", podDesc)
	}
	if netInfo.TopologyType() != types.Layer2Topology {
		// Static IP requests with IPAM are only supported on layer2 topology networks.
		// On other topologies, we cannot distinguish between already allocated IPs and
		// IPs excluded from allocation, making it impossible to safely honor static IP
		// requests when IPAM is enabled.
		return fmt.Errorf("cannot allocate a static IP request with IPAM for pod %s: layer2 topology is required, but network has topology %q", podDesc, netInfo.TopologyType())
	}
	if ipamClaim != nil && len(ipamClaim.Status.IPs) > 0 {
		for _, ipRequest := range network.IPRequest {
			if !util.IsItemInSlice(ipamClaim.Status.IPs, ipRequest) {
				return fmt.Errorf("cannot allocate a static IP request with IPAM for pod %q: the pod references an ipam claim with IPs not containing the requested IP %q", podDesc, ipRequest)
			}
		}
	}

	if err := validateIPFamilyMatchesNetwork(netInfo, network.IPRequest); err != nil {
		return err
	}

	return nil
}

var (
	ErrIPFamilyMismatch = errors.New("requested IPs family types must match network's IP family configuration")
)

func validateIPFamilyMatchesNetwork(netInfo util.NetInfo, ipRequests []string) error {
	if len(ipRequests) == 0 {
		return nil
	}

	if len(ipRequests) > 2 {
		return fmt.Errorf("layer2 network expects at most 2 IPs, got %d: %w", len(ipRequests), ErrIPFamilyMismatch)
	}

	if len(ipRequests) != len(netInfo.Subnets()) {
		return fmt.Errorf("layer2 network expects %d IP(s), got %d: %w", len(netInfo.Subnets()), len(ipRequests), ErrIPFamilyMismatch)
	}

	requestedIPs, err := util.ParseIPNets(ipRequests)
	if err != nil {
		return fmt.Errorf("failed to parse IP requests: %w", err)
	}

	var requestedIPv4, requestedIPv6 bool
	for _, ipNet := range requestedIPs {
		if utilnet.IsIPv6CIDR(ipNet) {
			requestedIPv6 = true
		} else {
			requestedIPv4 = true
		}
	}

	ipv4Mode, ipv6Mode := netInfo.IPMode()
	if ipv4Mode != requestedIPv4 || ipv6Mode != requestedIPv6 {
		return fmt.Errorf("layer2 network IP family mismatch: network supports IPv4=%t IPv6=%t, but requested types IPv4=%t IPv6=%t: %w",
			ipv4Mode, ipv6Mode, requestedIPv4, requestedIPv6, ErrIPFamilyMismatch)
	}

	return nil
}

// allocatePodAnnotationWithRollback allocates the PodAnnotation which includes
// IPs, a mac address, routes, gateways and an ID. Returns the allocated pod
// annotation and a pod with that annotation set. Returns a nil pod and the existing
// PodAnnotation if no updates are warranted to the pod.

// The allocation of network information can be requested through the network
// selection element or derived from the allocator provided IPs. If no IP
// allocation is required, set allocateIP to false. If the requested IPs cannot
// be honored, a new set of IPs will be allocated unless reallocateIP is set to
// false.

// A rollback function is returned to rollback the IP allocation if there was
// any.

// This function is designed to be used in AllocateToPodWithRollbackFunc
// implementations. Use an inlined implementation if you want to extract
// information from it as a side-effect.
func allocatePodAnnotationWithRollback(
	ipAllocator subnet.NamedAllocator,
	idAllocator id.NamedAllocator,
	netInfo util.NetInfo,
	node *corev1.Node,
	pod *corev1.Pod,
	nadKey string,
	network *nadapi.NetworkSelectionElement,
	claimsReconciler persistentips.PersistentAllocations,
	macRegistry mac.Register,
	reallocateIP bool,
	requireIPAMReservation bool,
	networkRole string) (
	updatedPod *corev1.Pod,
	podAnnotation *util.PodAnnotation,
	rollback func(),
	err error) {

	if !netInfo.IsUserDefinedNetwork() {
		nadKey = types.DefaultNetworkName
	}
	podDesc := fmt.Sprintf("%s/%s/%s", nadKey, pod.Namespace, pod.Name)
	macOwnerID := macOwner(pod)
	networkName := netInfo.GetNetworkName()

	// the IPs we allocate in this function need to be released back to the IPAM
	// pool if there is some error in any step past the point the IPs were
	// assigned via the IPAM manager. Note we are using a named return variable
	// for defer to work correctly.
	var releaseIPs []*net.IPNet
	var releaseID int
	var releaseMAC net.HardwareAddr
	rollback = func() {
		if releaseID != 0 {
			idAllocator.ReleaseID()
			klog.V(5).Infof("Released ID %d", releaseID)
			releaseID = 0
		}

		if len(releaseMAC) > 0 && macRegistry != nil {
			if rerr := macRegistry.Release(macOwnerID, releaseMAC); rerr != nil {
				klog.Errorf("Failed to release MAC %q on rollback, owner: %q, network: %q: %v", releaseMAC.String(), macOwnerID, networkName, rerr)
			} else {
				klog.V(5).Infof("Released MAC %q on rollback, owner: %q, network: %q", releaseMAC.String(), macOwnerID, networkName)
			}
			releaseMAC = nil
		}

		if len(releaseIPs) == 0 {
			return
		}
		err := ipAllocator.ReleaseIPs(releaseIPs)
		if err != nil {
			klog.Errorf("Error when releasing IPs %v: %v", util.StringSlice(releaseIPs), err)
			releaseIPs = nil
			return
		}
		klog.V(5).Infof("Released IPs %v", util.StringSlice(releaseIPs))
		releaseIPs = nil
	}
	defer func() {
		if err != nil {
			rollback()
		}
	}()

	podAnnotation, _ = util.UnmarshalPodAnnotation(pod.Annotations, nadKey)
	isNetworkAllocated := podAnnotation != nil
	if podAnnotation == nil {
		podAnnotation = &util.PodAnnotation{}
	}

	// work on a tentative pod annotation based on the existing one
	tentative := &util.PodAnnotation{
		IPs:      podAnnotation.IPs,
		MAC:      podAnnotation.MAC,
		TunnelID: podAnnotation.TunnelID,
		Role:     networkRole,
		IPAMMode: netInfo.IPAMType(),
	}

	hasIDAllocation := util.DoesNetworkRequireTunnelIDs(netInfo)
	needsID := tentative.TunnelID == 0 && hasIDAllocation

	if hasIDAllocation {
		if needsID {
			tentative.TunnelID, err = idAllocator.AllocateID()
		} else {
			err = idAllocator.ReserveID(tentative.TunnelID)
		}

		if err != nil {
			err = fmt.Errorf("failed to assign pod id for %s: %w", podDesc, err)
			return
		}

		releaseID = tentative.TunnelID
	}

	hasIPAM := util.DoesNetworkRequireIPAM(netInfo)
	hasIPRequest := network != nil && len(network.IPRequest) > 0
	hasStaticIPRequest := hasIPRequest && !reallocateIP

	var ipamClaim *ipamclaimsapi.IPAMClaim
	hasPersistentIPs := netInfo.AllowsPersistentIPs() && hasIPAM && claimsReconciler != nil
	hasIPAMClaim := network != nil && network.IPAMClaimReference != ""
	if hasIPAMClaim && !hasPersistentIPs {
		klog.Errorf(
			"Pod %s/%s referencing an IPAMClaim on network %q which does not honor it",
			pod.GetNamespace(),
			pod.GetName(),
			netInfo.GetNetworkName(),
		)
		hasIPAMClaim = false
	}
	if hasIPAMClaim {
		ipamClaim, err = claimsReconciler.FindIPAMClaim(network.IPAMClaimReference, network.Namespace)
		if err != nil {
			err = fmt.Errorf("error retrieving IPAMClaim for pod %s/%s: %w", pod.GetNamespace(), pod.GetName(), err)
			return
		}
		hasIPAMClaim = ipamClaim != nil && len(ipamClaim.Status.IPs) > 0
	}

	defer func() {
		if ipamClaim == nil || claimsReconciler == nil {
			return
		}
		updatedClaim := claimsReconciler.UpdateIPAMClaimStatus(ipamClaim, podAnnotation, pod.Name, err)
		if reconcileErr := claimsReconciler.Reconcile(ipamClaim, updatedClaim, ipAllocator); reconcileErr != nil {
			err = errors.Join(err, fmt.Errorf("failed to reconcile IPAM claim %s/%s: %w", ipamClaim.Namespace, ipamClaim.Name, reconcileErr))
		}
	}()

	if hasIPAM && hasStaticIPRequest {
		if err = validateStaticIPRequest(netInfo, network, ipamClaim, podDesc); err != nil {
			return
		}
	}

	// we need to update the annotation if it is missing IPs or MAC
	needsIPOrMAC := len(tentative.IPs) == 0 && (hasIPAM || hasIPRequest)
	needsIPOrMAC = needsIPOrMAC || len(tentative.MAC) == 0
	reallocateOnNonStaticIPRequest := len(tentative.IPs) == 0 && hasIPRequest && !hasStaticIPRequest

	if len(tentative.IPs) == 0 {
		if hasIPRequest {
			tentative.IPs, err = util.ParseIPNets(network.IPRequest)
			if err != nil {
				klog.Warningf("Failed parsing IPRequest %+v for pod %s: %v", network.IPRequest, podDesc, err)
				return
			}
		} else if hasIPAMClaim {
			tentative.IPs, err = util.ParseIPNets(ipamClaim.Status.IPs)
			if err != nil {
				return
			}
		}
	}

	if hasIPAM {
		if len(tentative.IPs) > 0 {
			if err = ipAllocator.AllocateIPs(tentative.IPs); err != nil &&
				!shouldSkipAllocateIPsError(err, isNetworkAllocated, ipamClaim, requireIPAMReservation) {
				err = fmt.Errorf("failed to ensure requested or annotated IPs %v for %s: %w",
					util.StringSlice(tentative.IPs), podDesc, err)
				if !reallocateOnNonStaticIPRequest {
					return
				}
				klog.Warning(err.Error())
				needsIPOrMAC = true
				tentative.IPs = nil
			}

			if err == nil && (!hasIPAMClaim || !isNetworkAllocated) {
				// copy the IPs that would need to be released
				releaseIPs = util.CopyIPNets(tentative.IPs)
			}

			// IPs allocated or we will allocate a new set of IPs, reset the error
			err = nil
		}

		if len(tentative.IPs) == 0 {
			tentative.IPs, err = ipAllocator.AllocateNextIPs()
			if err != nil {
				err = fmt.Errorf("failed to assign pod addresses for %s: %w", podDesc, err)
				return
			}

			// copy the IPs that would need to be released
			releaseIPs = util.CopyIPNets(tentative.IPs)
		}
	}

	if needsIPOrMAC {
		// handle mac address
		if network != nil && network.MacRequest != "" {
			tentative.MAC, err = net.ParseMAC(network.MacRequest)
		} else if len(tentative.IPs) > 0 {
			tentative.MAC = util.IPAddrToHWAddr(tentative.IPs[0].IP)
		} else {
			tentative.MAC, err = util.GenerateRandMAC()
		}
		if err != nil {
			return
		}
		if macRegistry != nil {
			if rerr := macRegistry.Reserve(macOwnerID, tentative.MAC); rerr != nil {
				// repeated requests are no-op because mac already reserved
				if !errors.Is(rerr, mac.ErrMACReserved) {
					// avoid leaking the network name because this error may reflect of a pod event, which is visible to non-admins.
					err = fmt.Errorf("failed to reserve MAC address %q for owner %q on NAD key %q: %w",
						tentative.MAC, macOwnerID, nadKey, rerr)
					klog.Errorf("%v, network-name: %q", err, networkName)
					return
				}
			} else {
				klog.V(5).Infof("Reserved MAC %q for owner %q on network %q NAD key %q", tentative.MAC, macOwnerID, networkName, nadKey)
				releaseMAC = tentative.MAC
			}
		}

		// handle routes & gateways
		err = AddRoutesGatewayIP(netInfo, node, pod, tentative, network)
		if err != nil {
			return
		}
	}

	needsAnnotationUpdate := needsIPOrMAC || needsID

	if needsAnnotationUpdate {
		updatedPod = pod
		updatedPod.Annotations, err = util.MarshalPodAnnotation(updatedPod.Annotations, tentative, nadKey)
		podAnnotation = tentative
	}

	return
}

func joinSubnetToRoute(netinfo util.NetInfo, isIPv6 bool, gatewayIP net.IP) util.PodRoute {
	joinSubnet := netinfo.JoinSubnetV4()
	if isIPv6 {
		joinSubnet = netinfo.JoinSubnetV6()
	}
	return util.PodRoute{
		Dest:    joinSubnet,
		NextHop: gatewayIP,
	}
}

func serviceCIDRToRoute(isIPv6 bool, gatewayIP net.IP) []util.PodRoute {
	var podRoutes []util.PodRoute
	for _, serviceSubnet := range config.Kubernetes.ServiceCIDRs {
		if isIPv6 == utilnet.IsIPv6CIDR(serviceSubnet) {
			podRoutes = append(podRoutes, util.PodRoute{
				Dest:    serviceSubnet,
				NextHop: gatewayIP,
			})
		}
	}
	return podRoutes
}

func hairpinMasqueradeIPToRoute(isIPv6 bool, gatewayIP net.IP) util.PodRoute {
	ip := config.Gateway.MasqueradeIPs.V4OVNServiceHairpinMasqueradeIP
	if isIPv6 {
		ip = config.Gateway.MasqueradeIPs.V6OVNServiceHairpinMasqueradeIP
	}
	return util.PodRoute{
		Dest: &net.IPNet{
			IP:   ip,
			Mask: util.GetIPFullMask(ip),
		},
		NextHop: gatewayIP,
	}
}

// addRoutesGatewayIP updates the provided pod annotation for the provided pod
// with the gateways derived from the allocated IPs
func AddRoutesGatewayIP(
	netinfo util.NetInfo,
	node *corev1.Node,
	pod *corev1.Pod,
	podAnnotation *util.PodAnnotation,
	network *nadapi.NetworkSelectionElement) error {

	// generate the nodeSubnets from the allocated IPs
	nodeSubnets := util.IPsToNetworkIPs(podAnnotation.IPs...)

	if netinfo.IsUserDefinedNetwork() {
		// for secondary network, see if its network-attachment's annotation has default-route key.
		// If present, then we need to add default route for it
		podAnnotation.Gateways = append(podAnnotation.Gateways, network.GatewayRequest...)
		topoType := netinfo.TopologyType()
		switch topoType {
		case types.LocalnetTopology:
			// no route needed for directly connected subnets
			return nil
		case types.Layer2Topology:
			if !util.IsNetworkSegmentationSupportEnabled() || !netinfo.IsPrimaryNetwork() {
				return nil
			}
			// logical router port MAC is based on IPv4 subnet if there is one, else IPv6
			// hasV4 is used to ensure that if ipv4 address was found, it is not overridden by an ipv6 address
			var nodeLRPMAC net.HardwareAddr
			var hasV4 bool
			for _, podIfAddr := range podAnnotation.IPs {
				isIPv6 := utilnet.IsIPv6CIDR(podIfAddr)
				nodeSubnet, err := util.MatchFirstIPNetFamily(isIPv6, nodeSubnets)
				if err != nil {
					return err
				}
				gatewayIPnet := netinfo.GetNodeGatewayIP(nodeSubnet)
				// Ensure default service network traffic always goes to OVN
				podAnnotation.Routes = append(podAnnotation.Routes, serviceCIDRToRoute(isIPv6, gatewayIPnet.IP)...)
				// Ensure UDN join subnet traffic always goes to UDN LSP
				podAnnotation.Routes = append(podAnnotation.Routes, joinSubnetToRoute(netinfo, isIPv6, gatewayIPnet.IP))
				if network != nil && len(network.GatewayRequest) == 0 { // if specific default route for pod was not requested then add gatewayIP
					podAnnotation.Gateways = append(podAnnotation.Gateways, gatewayIPnet.IP)
				}
				if !isIPv6 {
					hasV4 = true
					nodeLRPMAC = util.IPAddrToHWAddr(gatewayIPnet.IP)
				} else if !hasV4 {
					// only use IPv6 address to derive MAC if IPv4 address hasn't been found yet
					nodeLRPMAC = util.IPAddrToHWAddr(gatewayIPnet.IP)
				}
			}
			if _, isIPv6Mode := netinfo.IPMode(); isIPv6Mode {
				var routerPortMac net.HardwareAddr
				if !util.UDNLayer2NodeUsesTransitRouter(node) {
					joinAddrs, err := udn.GetGWRouterIPs(node, netinfo.GetNetInfo())
					if err != nil {
						if util.IsAnnotationNotSetError(err) {
							return types.NewSuppressedError(err)
						}
						return fmt.Errorf("failed parsing node gateway router join addresses, network %q, %w", netinfo.GetNetworkName(), err)
					}
					routerPortMac = util.IPAddrToHWAddr(joinAddrs[0].IP)
				} else {
					routerPortMac = nodeLRPMAC
				}
				podAnnotation.GatewayIPv6LLA = util.HWAddrToIPv6LLA(routerPortMac)
			}
			return nil
		case types.Layer3Topology:
			for _, podIfAddr := range podAnnotation.IPs {
				isIPv6 := utilnet.IsIPv6CIDR(podIfAddr)
				nodeSubnet, err := util.MatchFirstIPNetFamily(isIPv6, nodeSubnets)
				if err != nil {
					return err
				}
				gatewayIPnet := netinfo.GetNodeGatewayIP(nodeSubnet)
				for _, clusterSubnet := range netinfo.Subnets() {
					if isIPv6 == utilnet.IsIPv6CIDR(clusterSubnet.CIDR) {
						podAnnotation.Routes = append(podAnnotation.Routes, util.PodRoute{
							Dest:    clusterSubnet.CIDR,
							NextHop: gatewayIPnet.IP,
						})
					}
				}
				if !util.IsNetworkSegmentationSupportEnabled() || !netinfo.IsPrimaryNetwork() {
					continue
				}
				// Ensure default service network traffic always goes to OVN
				podAnnotation.Routes = append(podAnnotation.Routes, serviceCIDRToRoute(isIPv6, gatewayIPnet.IP)...)
				// Ensure UDN join subnet traffic always goes to UDN LSP
				podAnnotation.Routes = append(podAnnotation.Routes, joinSubnetToRoute(netinfo, isIPv6, gatewayIPnet.IP))
				if network != nil && len(network.GatewayRequest) == 0 { // if specific default route for pod was not requested then add gatewayIP
					podAnnotation.Gateways = append(podAnnotation.Gateways, gatewayIPnet.IP)
				}
			}
			return nil
		}
		return fmt.Errorf("topology type %s not supported", topoType)
	}

	// if there are other network attachments for the pod, then check if those network-attachment's
	// annotation has default-route key. If present, then we need to skip adding default route for
	// OVN interface
	networks, err := util.GetK8sPodAllNetworkSelections(pod)
	if err != nil {
		return fmt.Errorf("error while getting network attachment definition for [%s/%s]: %v",
			pod.Namespace, pod.Name, err)
	}
	otherDefaultRouteV4 := false
	otherDefaultRouteV6 := false
	for _, network := range networks {
		for _, gatewayRequest := range network.GatewayRequest {
			if utilnet.IsIPv6(gatewayRequest) {
				otherDefaultRouteV6 = true
			} else {
				otherDefaultRouteV4 = true
			}
		}
	}

	for _, podIfAddr := range podAnnotation.IPs {
		isIPv6 := utilnet.IsIPv6CIDR(podIfAddr)
		nodeSubnet, err := util.MatchFirstIPNetFamily(isIPv6, nodeSubnets)
		if err != nil {
			return err
		}

		gatewayIPnet := netinfo.GetNodeGatewayIP(nodeSubnet)

		// Ensure default pod network traffic always goes to OVN
		for _, clusterSubnet := range config.Default.ClusterSubnets {
			if isIPv6 == utilnet.IsIPv6CIDR(clusterSubnet.CIDR) {
				podAnnotation.Routes = append(podAnnotation.Routes, util.PodRoute{
					Dest:    clusterSubnet.CIDR,
					NextHop: gatewayIPnet.IP,
				})
			}
		}

		if podAnnotation.Role == types.NetworkRolePrimary {
			// Ensure default service network traffic always goes to OVN
			podAnnotation.Routes = append(podAnnotation.Routes, serviceCIDRToRoute(isIPv6, gatewayIPnet.IP)...)
			// Ensure service hairpin masquerade traffic always goes to OVN
			podAnnotation.Routes = append(podAnnotation.Routes, hairpinMasqueradeIPToRoute(isIPv6, gatewayIPnet.IP))
			otherDefaultRoute := otherDefaultRouteV4
			if isIPv6 {
				otherDefaultRoute = otherDefaultRouteV6
			}
			if !otherDefaultRoute {
				podAnnotation.Gateways = append(podAnnotation.Gateways, gatewayIPnet.IP)
			}
		}

		// Ensure default join subnet traffic always goes to OVN
		podAnnotation.Routes = append(podAnnotation.Routes, joinSubnetToRoute(netinfo, isIPv6, gatewayIPnet.IP))
	}

	return nil
}

// shouldSkipAllocateIPsError determines whether to skip/ignore IP allocation errors
// in scenarios where IPs may already be legitimately allocated.
// Returns false if the error is not ErrAllocated or if none of the skip conditions are met. True otherwise.
func shouldSkipAllocateIPsError(err error, networkAllocated bool, ipamClaim *ipamclaimsapi.IPAMClaim, requireIPAMReservation bool) bool {
	// Only skip if it's an "already allocated" error
	if !ip.IsErrAllocated(err) || requireIPAMReservation {
		return false
	}

	// If PreconfiguredUDNAddressesEnabled is disabled, always skip ErrAllocated
	if !util.IsPreconfiguredUDNAddressesEnabled() {
		return true
	}

	// Always skip ErrAllocated if network annotation already persisted on pod
	if networkAllocated {
		return true
	}

	// For persistent IP VM/Pods, if IPAMClaim already has IPs allocated, then ip already allocated, skip ErrAllocated
	if ipamClaim != nil && len(ipamClaim.Status.IPs) > 0 {
		return true
	}

	return false
}
