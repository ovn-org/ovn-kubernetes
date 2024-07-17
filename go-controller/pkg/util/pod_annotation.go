package util

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"

	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	nadutils "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/utils"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"

	v1 "k8s.io/api/core/v1"
	listers "k8s.io/client-go/listers/core/v1"
	utilnet "k8s.io/utils/net"
)

// This handles the "k8s.ovn.org/pod-networks" annotation on Pods, used to pass
// information about networking from the master to the nodes. (The util.PodAnnotation
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
	// OvnPodAnnotationName is the constant string representing the POD annotation key
	OvnPodAnnotationName = "k8s.ovn.org/pod-networks"
	// DefNetworkAnnotation is the pod annotation for the cluster-wide default network
	DefNetworkAnnotation = "v1.multus-cni.io/default-network"
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

	IP      string `json:"ip_address,omitempty"`
	Gateway string `json:"gateway_ip,omitempty"`

	TunnelID int    `json:"tunnel_id,omitempty"`
	Role     string `json:"role,omitempty"`
}

// Internal struct used to marshal PodRoute to the pod annotation
type podRoute struct {
	Dest    string `json:"dest"`
	NextHop string `json:"nextHop"`
}

// MarshalPodAnnotation adds the pod's network details of the specified network to the corresponding pod annotation.
func MarshalPodAnnotation(annotations map[string]string, podInfo *PodAnnotation, nadName string) (map[string]string, error) {
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

	existingPa, ok := podNetworks[nadName]
	if ok {
		if len(pa.IPs) != len(existingPa.IPs) {
			return nil, ErrOverridePodIPs
		}
		for _, ip := range pa.IPs {
			if !SliceHasStringItem(existingPa.IPs, ip) {
				return nil, ErrOverridePodIPs
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
	podNetworks[nadName] = pa
	bytes, err := json.Marshal(podNetworks)
	if err != nil {
		return nil, fmt.Errorf("failed marshaling podNetworks map %v", podNetworks)
	}
	annotations[OvnPodAnnotationName] = string(bytes)
	return annotations, nil
}

// UnmarshalPodAnnotation returns the Pod's network info of the given network from pod.Annotations
func UnmarshalPodAnnotation(annotations map[string]string, nadName string) (*PodAnnotation, error) {
	var err error
	ovnAnnotation, ok := annotations[OvnPodAnnotationName]
	if !ok {
		return nil, newAnnotationNotSetError("could not find OVN pod annotation in %v", annotations)
	}

	podNetworks, err := UnmarshalPodAnnotationAllNetworks(annotations)
	if err != nil {
		return nil, err
	}

	tempA, ok := podNetworks[nadName]
	if !ok {
		return nil, fmt.Errorf("no ovn pod annotation for network %s: %q",
			nadName, ovnAnnotation)
	}

	a := &tempA

	podAnnotation := &PodAnnotation{
		TunnelID: a.TunnelID,
		Role:     a.Role,
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

	return podAnnotation, nil
}

func UnmarshalPodAnnotationAllNetworks(annotations map[string]string) (map[string]podAnnotation, error) {
	podNetworks := make(map[string]podAnnotation)
	ovnAnnotation, ok := annotations[OvnPodAnnotationName]
	if ok {
		if err := json.Unmarshal([]byte(ovnAnnotation), &podNetworks); err != nil {
			return nil, fmt.Errorf("failed to unmarshal ovn pod annotation %q: %v",
				ovnAnnotation, err)
		}
	}
	return podNetworks, nil
}

// GetPodCIDRsWithFullMask returns the pod's IP addresses in a CIDR with FullMask format
// Internally it calls GetPodIPsOfNetwork
func GetPodCIDRsWithFullMask(pod *v1.Pod, nInfo NetInfo) ([]*net.IPNet, error) {
	podIPs, err := GetPodIPsOfNetwork(pod, nInfo)
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
func GetPodIPsOfNetwork(pod *v1.Pod, nInfo NetInfo) ([]net.IP, error) {
	if nInfo.IsSecondary() {
		return SecondaryNetworkPodIPs(pod, nInfo)
	}
	return DefaultNetworkPodIPs(pod)
}

func DefaultNetworkPodIPs(pod *v1.Pod) ([]net.IP, error) {
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

func SecondaryNetworkPodIPs(pod *v1.Pod, networkInfo NetInfo) ([]net.IP, error) {
	ips := []net.IP{}
	podNadNames, err := PodNadNames(pod, networkInfo)
	if err != nil {
		return nil, err
	}
	for _, nadName := range podNadNames {
		ips = append(ips, getAnnotatedPodIPs(pod, nadName)...)
	}
	return ips, nil
}

func PodNadNames(pod *v1.Pod, netinfo NetInfo) ([]string, error) {
	on, networkMap, err := GetPodNADToNetworkMapping(pod, netinfo)
	// skip pods that are not on this network
	if err != nil {
		return nil, err
	} else if !on {
		return []string{}, nil
	}
	nadNames := make([]string, 0, len(networkMap))
	for nadName := range networkMap {
		nadNames = append(nadNames, nadName)
	}
	return nadNames, nil
}

func getAnnotatedPodIPs(pod *v1.Pod, nadName string) []net.IP {
	var ips []net.IP
	annotation, _ := UnmarshalPodAnnotation(pod.Annotations, nadName)
	if annotation != nil {
		// Use the OVN annotation if valid
		for _, ip := range annotation.IPs {
			ips = append(ips, ip.IP)
		}
	}
	return ips
}

// GetK8sPodDefaultNetworkSelection get pod default network from annotations
func GetK8sPodDefaultNetworkSelection(pod *v1.Pod) (*nadapi.NetworkSelectionElement, error) {
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
func GetK8sPodAllNetworkSelections(pod *v1.Pod) ([]*nadapi.NetworkSelectionElement, error) {
	networks, err := nadutils.ParsePodNetworkAnnotation(pod)
	if err != nil {
		if _, ok := err.(*nadapi.NoK8sNetworkError); !ok {
			return nil, fmt.Errorf("failed to get all NetworkSelectionElements for pod %s/%s: %v", pod.Namespace, pod.Name, err)
		}
		networks = []*nadapi.NetworkSelectionElement{}
	}
	return networks, nil
}

// UpdatePodAnnotationWithRetry updates the pod annotation on the pod retrying
// on conflict
func UpdatePodAnnotationWithRetry(podLister listers.PodLister, kube kube.Interface, pod *v1.Pod, podAnnotation *PodAnnotation, nadName string) error {
	updatePodAnnotationNoRollback := func(pod *v1.Pod) (*v1.Pod, func(), error) {
		var err error
		pod.Annotations, err = MarshalPodAnnotation(pod.Annotations, podAnnotation, nadName)
		if err != nil {
			return nil, nil, err
		}
		return pod, nil, nil
	}

	return UpdatePodWithRetryOrRollback(
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

func joinSubnetToRoute(netinfo NetInfo, isIPv6 bool, gatewayIP net.IP) PodRoute {
	joinSubnet := netinfo.JoinSubnetV4()
	if isIPv6 {
		joinSubnet = netinfo.JoinSubnetV6()
	}
	return PodRoute{
		Dest:    joinSubnet,
		NextHop: gatewayIP,
	}
}

func serviceCIDRToRoute(isIPv6 bool, gatewayIP net.IP) []PodRoute {
	var podRoutes []PodRoute
	for _, serviceSubnet := range config.Kubernetes.ServiceCIDRs {
		if isIPv6 == utilnet.IsIPv6CIDR(serviceSubnet) {
			podRoutes = append(podRoutes, PodRoute{
				Dest:    serviceSubnet,
				NextHop: gatewayIP,
			})
		}
	}
	return podRoutes
}

// addRoutesGatewayIP updates the provided pod annotation for the provided pod
// with the gateways derived from the allocated IPs
func AddRoutesGatewayIP(
	netinfo NetInfo,
	pod *v1.Pod,
	podAnnotation *PodAnnotation,
	network *nadapi.NetworkSelectionElement) error {

	// generate the nodeSubnets from the allocated IPs
	nodeSubnets := IPsToNetworkIPs(podAnnotation.IPs...)

	if netinfo.IsSecondary() {
		// for secondary network, see if its network-attachment's annotation has default-route key.
		// If present, then we need to add default route for it
		podAnnotation.Gateways = append(podAnnotation.Gateways, network.GatewayRequest...)
		topoType := netinfo.TopologyType()
		switch topoType {
		case types.LocalnetTopology:
			// no route needed for directly connected subnets
			return nil
		case types.Layer2Topology:
			if !IsNetworkSegmentationSupportEnabled() || !netinfo.IsPrimaryNetwork() {
				return nil
			}
			for _, podIfAddr := range podAnnotation.IPs {
				isIPv6 := utilnet.IsIPv6CIDR(podIfAddr)
				nodeSubnet, err := MatchFirstIPNetFamily(isIPv6, nodeSubnets)
				if err != nil {
					return err
				}
				gatewayIPnet := GetNodeGatewayIfAddr(nodeSubnet)
				// Ensure default service network traffic always goes to OVN
				podAnnotation.Routes = append(podAnnotation.Routes, serviceCIDRToRoute(isIPv6, gatewayIPnet.IP)...)
				// Ensure UDN join subnet traffic always goes to UDN LSP
				podAnnotation.Routes = append(podAnnotation.Routes, joinSubnetToRoute(netinfo, isIPv6, gatewayIPnet.IP))
				if network != nil && len(network.GatewayRequest) == 0 { // if specific default route for pod was not requested then add gatewayIP
					podAnnotation.Gateways = append(podAnnotation.Gateways, gatewayIPnet.IP)
				}
			}
			return nil
		case types.Layer3Topology:
			for _, podIfAddr := range podAnnotation.IPs {
				isIPv6 := utilnet.IsIPv6CIDR(podIfAddr)
				nodeSubnet, err := MatchFirstIPNetFamily(isIPv6, nodeSubnets)
				if err != nil {
					return err
				}
				gatewayIPnet := GetNodeGatewayIfAddr(nodeSubnet)
				for _, clusterSubnet := range netinfo.Subnets() {
					if isIPv6 == utilnet.IsIPv6CIDR(clusterSubnet.CIDR) {
						podAnnotation.Routes = append(podAnnotation.Routes, PodRoute{
							Dest:    clusterSubnet.CIDR,
							NextHop: gatewayIPnet.IP,
						})
					}
				}
				if !IsNetworkSegmentationSupportEnabled() || !netinfo.IsPrimaryNetwork() {
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
	networks, err := GetK8sPodAllNetworkSelections(pod)
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
		nodeSubnet, err := MatchFirstIPNetFamily(isIPv6, nodeSubnets)
		if err != nil {
			return err
		}

		gatewayIPnet := GetNodeGatewayIfAddr(nodeSubnet)

		// Ensure default pod network traffic always goes to OVN
		for _, clusterSubnet := range config.Default.ClusterSubnets {
			if isIPv6 == utilnet.IsIPv6CIDR(clusterSubnet.CIDR) {
				podAnnotation.Routes = append(podAnnotation.Routes, PodRoute{
					Dest:    clusterSubnet.CIDR,
					NextHop: gatewayIPnet.IP,
				})
			}
		}

		if podAnnotation.Role == types.NetworkRolePrimary {
			// Ensure default service network traffic always goes to OVN
			podAnnotation.Routes = append(podAnnotation.Routes, serviceCIDRToRoute(isIPv6, gatewayIPnet.IP)...)

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
