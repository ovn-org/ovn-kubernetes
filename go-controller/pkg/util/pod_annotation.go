// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package util

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"

	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	nadutils "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/utils"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	listers "k8s.io/client-go/listers/core/v1"
	utilnet "k8s.io/utils/net"
	"sigs.k8s.io/yaml"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
)

// This handles the "k8s.ovn.org/pod-networks" annotation on Pods, used to pass
// information about networking from the ovnkube-controller to the nodes. (The util.PodAnnotation
// struct is also embedded in the cni.PodInterfaceInfo type that is passed from the
// cniserver to the CNI shim.)
//
// The annotation looks like:
//
//   annotations:
//     k8s.ovn.org/pod-networks: |
//       {
//         "default": {
//           "ip_addresses": ["192.168.0.5/24"],
//           "mac_address": "0a:58:fd:98:00:01",
//           "gateway_ips": ["192.168.0.1"]
//
//           # for backward compatibility
//           "ip_address": "192.168.0.5/24",
//           "gateway_ip": "192.168.0.1"
//         }
//       }
//
// (With optional additional "routes" also indicated; in particular, if a pod has an
// additional network attachment that claims the default route, then the "default" network
// will have explicit routes to the cluster and service subnets.)
//
// The "ip_address" and "gateway_ip" fields are deprecated and will eventually go away.
// (And they are not output when "ip_addresses" or "gateway_ips" contains multiple
// values.)

const (
	// DefNetworkAnnotation is the pod annotation for the cluster-wide active network
	DefNetworkAnnotation = "v1.multus-cni.io/default-network"
	// UDNOpenPortsAnnotationName is the pod annotation to open default network pods on UDN pods.
	UDNOpenPortsAnnotationName = "k8s.ovn.org/open-default-ports"

	// DeprecatedOvnUDNIPAMClaimName is used for workload owners to instruct OVN-K which
	// IPAMClaim will hold the allocation for the workload.
	// Deprecated: Use 'v1.multus-cni.io/default-network' annotation instead, specifying the 'ipam-claim-reference' attribute.
	DeprecatedOvnUDNIPAMClaimName = "k8s.ovn.org/primary-udn-ipamclaim"
)

var ErrNoPodIPFound = errors.New("no pod IPs found")
var ErrOverridePodIPs = errors.New("requested pod IPs trying to override IPs exists in pod annotation")

// PodAnnotation describes the assigned network details for a single pod network. (The
// actual annotation may include the equivalent of multiple PodAnnotations.)
type PodAnnotation struct {
	// IPs are the pod's assigned IP addresses/prefixes
	IPs []*net.IPNet
	// MAC is the pod's assigned MAC address
	MAC net.HardwareAddr
	// Gateways are the pod's gateway IP addresses; note that there may be
	// fewer Gateways than IPs.
	Gateways []net.IP

	// GatewayIPv6LLA is the IPv6 Link Local Address for the pod's gateway, that is the address
	// that will be set as gateway with router advertisements
	// generated from the network cluster wide gateway IPv6 LLA.
	GatewayIPv6LLA net.IP

	// Routes are additional routes to add to the pod's network namespace
	Routes []PodRoute

	// TunnelID assigned to each pod for layer2 secondary networks
	TunnelID int

	// Role defines what role this network plays for the given pod.
	// Expected values are:
	// (1) "primary" if this network is the primary network of the pod.
	//     The "default" network is the primary network of any pod usually
	//     unless user-defined-network-segmentation feature has been activated.
	//     If network segmentation feature is enabled then any user defined
	//     network can be the primary network of the pod.
	// (2) "secondary" if this network is the secondary network of the pod.
	//     Only user defined networks can be secondary networks for a pod.
	// (3) "infrastructure-locked" is applicable only to "default" network if
	//     a user defined network is the "primary" network for this pod. This
	//     signifies the "default" network is only used for probing and
	//     is otherwise locked for all intents and purposes.
	// At a given time a pod can have only 1 network with role:"primary"
	Role string

	// IPAMMode describes the IPAM mode configured for this network.
	// Empty covers the CRD's non-DHCP modes:
	//   - "Enabled" (OVN-K assigns the IPs from the subnet)
	//   - "Disabled" (OVN-K assigns just the MAC and leaves IP configuration to the user).
	// "dhcp" means the workload (typically a VM) obtains its IP from an
	// external DHCP server. This field is meant to be consumed by downstream consumers
	// (e.g. cloud-init generators) to render the guest-side network config accordingly.
	//
	// "dhcp" relaxes the usual invariant that a network's annotation entry
	// never changes once written: the entry is first written MAC-only, gains
	// IPs once the CNI patches the DHCP lease in, and the IPs can change
	// again on every sandbox recreation before the pod reaches Running (each
	// repeat CNI ADD runs a fresh DHCP exchange). Consumers of this
	// annotation must therefore handle pod UPDATE events for such entries
	// instead of treating the first read as final. DHCP IPAM is restricted
	// to localnet secondary networks, so the consumers affected are exactly
	// the features that run there: the pod (logical switch port) controller,
	// MultiNetworkPolicy and NetworkQoS.
	IPAMMode string
}

// PodRoute describes any routes to be added to the pod's network namespace
type PodRoute struct {
	// Dest is the route destination
	Dest *net.IPNet
	// NextHop is the IP address of the next hop for traffic destined for Dest
	NextHop net.IP
}

func (r PodRoute) String() string {
	return fmt.Sprintf("%s %s", r.Dest, r.NextHop)
}

// Internal struct used to marshal PodAnnotation to the pod annotation
type podAnnotation struct {
	IPs      []string   `json:"ip_addresses"`
	MAC      string     `json:"mac_address"`
	Gateways []string   `json:"gateway_ips,omitempty"`
	Routes   []podRoute `json:"routes,omitempty"`

	IP             string `json:"ip_address,omitempty"`
	Gateway        string `json:"gateway_ip,omitempty"`
	GatewayIPv6LLA string `json:"ipv6_lla_gateway_ip,omitempty"`

	TunnelID int    `json:"tunnel_id,omitempty"`
	Role     string `json:"role,omitempty"`
	IPAMMode string `json:"ipam_mode,omitempty"`
}

// Internal struct used to marshal PodRoute to the pod annotation
type podRoute struct {
	Dest    string `json:"dest"`
	NextHop string `json:"nextHop"`
}

type OpenPort struct {
	// valid values are tcp, udp, sctp, icmp
	Protocol string `json:"protocol"`
	Port     *int   `json:"port,omitempty"`
}

// MarshalPodAnnotation adds the pod's network details of the specified network to the corresponding pod annotation.
func MarshalPodAnnotation(annotations map[string]string, podInfo *PodAnnotation, nadKey string) (map[string]string, error) {
	if annotations == nil {
		annotations = make(map[string]string)
	}
	podNetworks, err := UnmarshalPodAnnotationAllNetworks(annotations)
	if err != nil {
		return nil, err
	}
	pa := podAnnotation{
		TunnelID: podInfo.TunnelID,
		MAC:      podInfo.MAC.String(),
		Role:     podInfo.Role,
		IPAMMode: podInfo.IPAMMode,
	}

	if len(podInfo.IPs) == 1 {
		pa.IP = podInfo.IPs[0].String()
		if len(podInfo.Gateways) == 1 {
			pa.Gateway = podInfo.Gateways[0].String()
		} else if len(podInfo.Gateways) > 1 {
			return nil, fmt.Errorf("bad podNetwork data: single-stack network can only have a single gateway")
		}
	}
	for _, ip := range podInfo.IPs {
		pa.IPs = append(pa.IPs, ip.String())
	}

	existingPa, ok := podNetworks[nadKey]
	if ok {
		// DHCP entry IPs are owned by the external DHCP server, not OVN-K
		// IPAM: the CNI fills them in after creating the entry and may
		// replace them with a new lease on a repeat CNI_ADD, so allow the
		// overwrite.
		if existingPa.IPAMMode != types.IPAMTypeDHCP {
			if len(pa.IPs) != len(existingPa.IPs) {
				return nil, ErrOverridePodIPs
			}
			for _, ip := range pa.IPs {
				if !SliceHasStringItem(existingPa.IPs, ip) {
					return nil, ErrOverridePodIPs
				}
			}
		}
	}

	for _, gw := range podInfo.Gateways {
		pa.Gateways = append(pa.Gateways, gw.String())
	}

	for _, r := range podInfo.Routes {
		if r.Dest.IP.IsUnspecified() {
			return nil, fmt.Errorf("bad podNetwork data: default route %v should be specified as gateway", r)
		}
		var nh string
		if r.NextHop != nil {
			nh = r.NextHop.String()
		}
		pa.Routes = append(pa.Routes, podRoute{
			Dest:    r.Dest.String(),
			NextHop: nh,
		})
	}

	if podInfo.GatewayIPv6LLA != nil {
		pa.GatewayIPv6LLA = podInfo.GatewayIPv6LLA.String()
	}

	podNetworks[nadKey] = pa
	bytes, err := json.Marshal(podNetworks)
	if err != nil {
		return nil, fmt.Errorf("failed marshaling podNetworks map %v", podNetworks)
	}
	annotations[types.OvnPodAnnotationName] = string(bytes)
	return annotations, nil
}

// UnmarshalPodAnnotation returns the Pod's network info of the given network from pod.Annotations
func UnmarshalPodAnnotation(annotations map[string]string, nadKey string) (*PodAnnotation, error) {
	var err error
	ovnAnnotation, ok := annotations[types.OvnPodAnnotationName]
	if !ok {
		return nil, newAnnotationNotSetError("could not find OVN pod annotation in %v", annotations)
	}

	podNetworks, err := UnmarshalPodAnnotationAllNetworks(annotations)
	if err != nil {
		return nil, err
	}

	tempA, ok := podNetworks[nadKey]
	if !ok {
		return nil, newAnnotationNotSetError("no ovn pod annotation for NAD key %s: %q",
			nadKey, ovnAnnotation)
	}

	a := &tempA

	podAnnotation := &PodAnnotation{
		TunnelID: a.TunnelID,
		Role:     a.Role,
		IPAMMode: a.IPAMMode,
	}
	podAnnotation.MAC, err = net.ParseMAC(a.MAC)
	if err != nil {
		return nil, fmt.Errorf("failed to parse pod MAC %q: %v", a.MAC, err)
	}

	if len(a.IPs) == 0 {
		if a.IP != "" {
			a.IPs = append(a.IPs, a.IP)
		}
	} else if a.IP != "" && a.IP != a.IPs[0] {
		return nil, fmt.Errorf("bad annotation data (ip_address and ip_addresses conflict)")
	}
	for _, ipstr := range a.IPs {
		ip, ipnet, err := net.ParseCIDR(ipstr)
		if err != nil {
			return nil, fmt.Errorf("failed to parse pod IP %q: %v", ipstr, err)
		}
		ipnet.IP = ip
		podAnnotation.IPs = append(podAnnotation.IPs, ipnet)
	}

	if len(a.Gateways) == 0 {
		if a.Gateway != "" {
			a.Gateways = append(a.Gateways, a.Gateway)
		}
	} else if a.Gateway != "" && a.Gateway != a.Gateways[0] {
		return nil, fmt.Errorf("bad annotation data (gateway_ip and gateway_ips conflict)")
	}
	for _, gwstr := range a.Gateways {
		gw := net.ParseIP(gwstr)
		if gw == nil {
			return nil, fmt.Errorf("failed to parse pod gateway %q", gwstr)
		}
		podAnnotation.Gateways = append(podAnnotation.Gateways, gw)
	}

	for _, r := range a.Routes {
		route := PodRoute{}
		_, route.Dest, err = net.ParseCIDR(r.Dest)
		if err != nil {
			return nil, fmt.Errorf("failed to parse pod route dest %q: %v", r.Dest, err)
		}
		if route.Dest.IP.IsUnspecified() {
			return nil, fmt.Errorf("bad podNetwork data: default route %v should be specified as gateway", route)
		}
		if r.NextHop != "" {
			route.NextHop = net.ParseIP(r.NextHop)
			if route.NextHop == nil {
				return nil, fmt.Errorf("failed to parse pod route next hop %q", r.NextHop)
			} else if utilnet.IsIPv6(route.NextHop) != utilnet.IsIPv6CIDR(route.Dest) {
				return nil, fmt.Errorf("pod route %s has next hop %s of different family", r.Dest, r.NextHop)
			}
		}
		podAnnotation.Routes = append(podAnnotation.Routes, route)
	}

	if a.GatewayIPv6LLA != "" {
		llaGW := net.ParseIP(a.GatewayIPv6LLA)
		if !isIPv6LLA(llaGW) {
			return nil, fmt.Errorf("failed to parse pod ipv6 lla gateway, or non ipv6 lla %q", a.GatewayIPv6LLA)
		}
		podAnnotation.GatewayIPv6LLA = llaGW
	}

	return podAnnotation, nil
}

func UnmarshalPodAnnotationAllNetworks(annotations map[string]string) (map[string]podAnnotation, error) {
	podNetworks := make(map[string]podAnnotation)
	ovnAnnotation, ok := annotations[types.OvnPodAnnotationName]
	if ok {
		if err := json.Unmarshal([]byte(ovnAnnotation), &podNetworks); err != nil {
			return nil, fmt.Errorf("failed to unmarshal ovn pod annotation %q: %v",
				ovnAnnotation, err)
		}
	}
	return podNetworks, nil
}

// GetPodCIDRsWithFullMask returns the pod's IP addresses in a CIDR with FullMask format.
// Internally it calls GetPodIPsOfNetwork.
func GetPodCIDRsWithFullMask(pod *corev1.Pod, nInfo NetInfo, getNetworkNameForNADKey func(nadKey string) string) ([]*net.IPNet, error) {
	podIPs, err := GetPodIPsOfNetwork(pod, nInfo, getNetworkNameForNADKey)
	if err != nil {
		return nil, err
	}
	ips := make([]*net.IPNet, 0, len(podIPs))
	for _, podIP := range podIPs {
		ipNet := net.IPNet{
			IP:   podIP,
			Mask: GetIPFullMask(podIP),
		}
		ips = append(ips, &ipNet)
	}
	return ips, nil
}

// GetPodIPsOfNetwork returns the pod's IP addresses, first from the OVN annotation
// and then falling back to the Pod Status IPs. This function is intended to
// also return IPs for HostNetwork and other non-OVN-IPAM-ed pods.
// getNetworkNameForNADKey is required for user defined networks.
func GetPodIPsOfNetwork(pod *corev1.Pod, nInfo NetInfo, getNetworkNameForNADKey func(nadKey string) string) ([]net.IP, error) {
	if nInfo.IsUserDefinedNetwork() {
		if getNetworkNameForNADKey == nil {
			return nil, fmt.Errorf("missing NAD resolver for network %q", nInfo.GetNetworkName())
		}
		return SecondaryNetworkPodIPs(pod, nInfo, getNetworkNameForNADKey)
	}
	return DefaultNetworkPodIPs(pod)
}

// GetPodCIDRsWithFullMaskOfNetwork returns the pod's IP addresses in a CIDR with FullMask format
// from a pod network annotation 'k8s.ovn.org/pod-networks' using key nadName.
func GetPodCIDRsWithFullMaskOfNetwork(pod *corev1.Pod, nadKey string) []*net.IPNet {
	ips := getAnnotatedPodIPs(pod, nadKey)
	ipNets := make([]*net.IPNet, 0, len(ips))
	for _, ip := range ips {
		ipNet := net.IPNet{
			IP:   ip,
			Mask: GetIPFullMask(ip),
		}
		ipNets = append(ipNets, &ipNet)
	}
	return ipNets
}

func DefaultNetworkPodIPs(pod *corev1.Pod) ([]net.IP, error) {
	// Try to use Kube API pod IPs for default network first
	// This is much faster than trying to unmarshal annotations
	ips := make([]net.IP, 0, len(pod.Status.PodIPs))
	for _, podIP := range pod.Status.PodIPs {
		ip := utilnet.ParseIPSloppy(podIP.IP)
		if ip == nil {
			continue
		}
		ips = append(ips, ip)
	}

	if len(ips) > 0 {
		return ips, nil
	}

	ips = getAnnotatedPodIPs(pod, types.DefaultNetworkName)
	if len(ips) > 0 {
		return ips, nil
	}

	// Fallback check pod.Status.PodIP
	// Kubelet < 1.16 only set podIP
	ip := utilnet.ParseIPSloppy(pod.Status.PodIP)
	if ip == nil {
		return nil, fmt.Errorf("pod %s/%s: %w ", pod.Namespace, pod.Name, ErrNoPodIPFound)
	}

	return []net.IP{ip}, nil
}

func SecondaryNetworkPodIPs(pod *corev1.Pod, networkInfo NetInfo, getNetworkNameForNADKey func(nadKey string) string) ([]net.IP, error) {
	ips := []net.IP{}
	if getNetworkNameForNADKey == nil {
		return nil, fmt.Errorf("missing NAD resolver for network %q", networkInfo.GetNetworkName())
	}
	if networkInfo.IsPrimaryNetwork() {
		podNetworks, err := UnmarshalPodAnnotationAllNetworks(pod.Annotations)
		if err != nil {
			return nil, err
		}
		for nadKey, ann := range podNetworks {
			networkName := getNetworkNameForNADKey(nadKey)
			if networkName == "" || networkName != networkInfo.GetNetworkName() {
				continue
			}
			annIPs, err := podAnnotationIPs(&ann)
			if err != nil {
				return nil, err
			}
			ips = append(ips, annIPs...)
		}
		return ips, nil
	}

	on, networkMap, err := getPodNADToNetworkMapping(pod, networkInfo, getNetworkNameForNADKey)
	if err != nil {
		return nil, err
	} else if !on {
		return []net.IP{}, nil
	}

	podNetworks, err := UnmarshalPodAnnotationAllNetworks(pod.Annotations)
	if err != nil {
		return nil, err
	}

	for nadKey := range networkMap {
		ann, ok := podNetworks[nadKey]
		if !ok {
			continue
		}
		annIPs, err := podAnnotationIPs(&ann)
		if err != nil {
			return nil, err
		}
		ips = append(ips, annIPs...)
	}
	return ips, nil
}

// PodNADKeys returns pod's NAD keys associated with given network specified by netconf.
// For primary UDNs, retrieve NAD names for the pod based on its OVN annotations.
// For secondary UDNs, retrieve NAD names for the pod based on NetworkSelectionElement.
func PodNADKeys(pod *corev1.Pod, netinfo NetInfo, getNetworkNameForNADKey func(nadKey string) string) ([]string, error) {
	if netinfo.IsUserDefinedNetwork() && getNetworkNameForNADKey == nil {
		return nil, fmt.Errorf("missing NAD resolver for network %q", netinfo.GetNetworkName())
	}

	if netinfo.IsPrimaryNetwork() {
		podNetworks, err := UnmarshalPodAnnotationAllNetworks(pod.Annotations)
		if err != nil {
			return nil, err
		}
		nadKeys := make([]string, 0, len(podNetworks))
		for nadKey := range podNetworks {
			networkName := getNetworkNameForNADKey(nadKey)
			if networkName == "" || networkName != netinfo.GetNetworkName() {
				continue
			}
			nadKeys = append(nadKeys, nadKey)
		}
		return nadKeys, nil
	}

	on, networkMap, err := getPodNADToNetworkMapping(pod, netinfo, getNetworkNameForNADKey)
	// skip pods that are not on this network
	if err != nil {
		return nil, err
	} else if !on {
		return []string{}, nil
	}
	nadKeys := make([]string, 0, len(networkMap))
	for nadKey := range networkMap {
		nadKeys = append(nadKeys, nadKey)
	}
	return nadKeys, nil
}

func podAnnotationIPs(ann *podAnnotation) ([]net.IP, error) {
	ipStrs := ann.IPs
	if len(ipStrs) > 0 && ann.IP != "" && ann.IP != ipStrs[0] {
		return nil, fmt.Errorf("bad annotation data (ip_address and ip_addresses conflict)")
	}
	if len(ipStrs) == 0 && ann.IP != "" {
		ipStrs = []string{ann.IP}
	}
	ips := make([]net.IP, 0, len(ipStrs))
	for _, ipStr := range ipStrs {
		ip, _, err := net.ParseCIDR(ipStr)
		if err != nil {
			return nil, fmt.Errorf("failed to parse pod IP %q: %v", ipStr, err)
		}
		ips = append(ips, ip)
	}
	return ips, nil
}

func getAnnotatedPodIPs(pod *corev1.Pod, nadKey string) []net.IP {
	var ips []net.IP
	annotation, _ := UnmarshalPodAnnotation(pod.Annotations, nadKey)
	if annotation != nil {
		// Use the OVN annotation if valid
		for _, ip := range annotation.IPs {
			ips = append(ips, ip.IP)
		}
	}
	return ips
}

// GetK8sPodDefaultNetworkSelection get pod default network from annotations
func GetK8sPodDefaultNetworkSelection(pod *corev1.Pod) (*nadapi.NetworkSelectionElement, error) {
	var netAnnot string

	netAnnot, ok := pod.Annotations[DefNetworkAnnotation]
	if !ok {
		return nil, nil
	}

	networks, err := nadutils.ParseNetworkAnnotation(netAnnot, pod.Namespace)
	if err != nil {
		return nil, fmt.Errorf("GetK8sPodDefaultNetwork: failed to parse CRD object: %v", err)
	}
	if len(networks) > 1 {
		return nil, fmt.Errorf("GetK8sPodDefaultNetwork: more than one default network is specified: %s", netAnnot)
	}

	if len(networks) == 1 {
		return networks[0], nil
	}

	return nil, nil
}

// GetK8sPodAllNetworkSelections get pod's all network NetworkSelectionElement from k8s.v1.cni.cncf.io/networks annotation
func GetK8sPodAllNetworkSelections(pod *corev1.Pod) ([]*nadapi.NetworkSelectionElement, error) {
	networks, err := nadutils.ParsePodNetworkAnnotation(pod)
	if err != nil {
		if _, ok := err.(*nadapi.NoK8sNetworkError); !ok {
			return nil, fmt.Errorf("failed to get all NetworkSelectionElements for pod %s/%s: %v", pod.Namespace, pod.Name, err)
		}
		networks = []*nadapi.NetworkSelectionElement{}
	}
	return networks, nil
}

// GetK8sPodNetworkSelection returns the pod's NetworkSelectionElement for the
// given indexed NAD key, deriving the index the same way GetIndexedNADKey
// does: the n-th selection of the same NAD maps to the key nadName[/n].
// Returns nil (and no error) when the pod has no matching selection.
func GetK8sPodNetworkSelection(pod *corev1.Pod, nadKey string) (*nadapi.NetworkSelectionElement, error) {
	networks, err := GetK8sPodAllNetworkSelections(pod)
	if err != nil {
		return nil, err
	}
	counts := map[string]int{}
	for _, network := range networks {
		nadNamespace := network.Namespace
		if nadNamespace == "" {
			nadNamespace = pod.Namespace
		}
		nadName := GetNADName(nadNamespace, network.Name)
		if GetIndexedNADKey(nadName, counts[nadName]) == nadKey {
			return network, nil
		}
		counts[nadName]++
	}
	return nil, nil
}

// UpdatePodAnnotationWithRetry updates the pod annotation on the pod retrying
// on conflict
func UpdatePodAnnotationWithRetry(ctx context.Context, podLister listers.PodLister, kube kube.Interface, pod *corev1.Pod, podAnnotation *PodAnnotation, nadKey string) error {
	updatePodAnnotationNoRollback := func(pod *corev1.Pod) (*corev1.Pod, func(), error) {
		var err error
		pod.Annotations, err = MarshalPodAnnotation(pod.Annotations, podAnnotation, nadKey)
		if err != nil {
			return nil, nil, err
		}
		return pod, nil, nil
	}

	return UpdatePodWithRetryOrRollback(
		ctx,
		podLister,
		kube,
		pod,
		updatePodAnnotationNoRollback,
	)
}

// IsValidPodAnnotation tests whether the PodAnnotation is valid, currently true
// for any PodAnnotation with a MAC which is the only thing required to attach a
// pod.
func IsValidPodAnnotation(podAnnotation *PodAnnotation) bool {
	return podAnnotation != nil && len(podAnnotation.MAC) > 0
}

// UnmarshalUDNOpenPortsAnnotation returns the OpenPorts from the pod annotation. If annotation is not present,
// empty list with no error is returned.
func UnmarshalUDNOpenPortsAnnotation(annotations map[string]string) ([]*OpenPort, error) {
	result := []*OpenPort{}
	ports, ok := annotations[UDNOpenPortsAnnotationName]
	if !ok {
		return result, nil
	}
	if err := yaml.Unmarshal([]byte(ports), &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal UDN open ports annotation %s: %v", ports, err)
	}
	allowedProtocols := sets.New("tcp", "udp", "sctp", "icmp")

	for _, portDef := range result {
		if !allowedProtocols.Has(portDef.Protocol) {
			return nil, fmt.Errorf("invalid protocol %s", portDef.Protocol)
		}
		if portDef.Protocol == "icmp" {
			if portDef.Port != nil {
				return nil, fmt.Errorf("invalid port %v for icmp protocol, should be empty", *portDef.Port)
			}
		} else if portDef.Port == nil {
			return nil, fmt.Errorf("port is required for %s protocol", portDef.Protocol)
		}
		if portDef.Port != nil && (*portDef.Port > 65535 || *portDef.Port < 0) {
			return nil, fmt.Errorf("invalid port %v", *portDef.Port)
		}
	}
	return result, nil
}

// Ensure the IP is a valid IPv6 LLA
func isIPv6LLA(ip net.IP) bool {
	return utilnet.IsIPv6(ip) && ip.IsLinkLocalUnicast()
}
