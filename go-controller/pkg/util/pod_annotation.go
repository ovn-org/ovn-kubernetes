package util

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"

	netattachdefapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	netattachdefutils "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/utils"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
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
}

func (pa *PodAnnotation) ensureSingleStackSingleGW() error {
	if len(pa.IPs) == 1 && len(pa.Gateways) > 1 {
		return fmt.Errorf("single-stack network can only have a single gateway")
	}
	return nil
}

// PodRoute describes any routes to be added to the pod's network namespace
type PodRoute struct {
	// Dest is the route destination
	Dest *net.IPNet
	// NextHop is the IP address of the next hop for traffic destined for Dest
	NextHop net.IP
}

// Internal struct used to marshal PodAnnotation to the pod annotation
type podAnnotation struct {
	IPs      []string   `json:"ip_addresses"`
	MAC      string     `json:"mac_address"`
	Gateways []string   `json:"gateway_ips,omitempty"`
	Routes   []podRoute `json:"routes,omitempty"`

	IP      string `json:"ip_address,omitempty"`
	Gateway string `json:"gateway_ip,omitempty"`
}

func (pa *podAnnotation) setMAC(newMAC string) error {
	if pa.MAC != "" {
		return fmt.Errorf("update of MAC not allowed")
	}
	if _, err := net.ParseMAC(newMAC); err != nil {
		return fmt.Errorf("unable to determine any valid MAC: %v", err)
	}
	pa.MAC = newMAC
	return nil
}

func (pa *podAnnotation) setIP(newIPs []*net.IPNet) error {
	if pa.IP != "" {
		return fmt.Errorf("update of IP not allowed")
	}
	if len(newIPs) == 1 {
		pa.IP = newIPs[0].String()
	}
	return nil
}

func (pa *podAnnotation) setGateway(newPA *PodAnnotation) error {
	if pa.Gateway != "" {
		return fmt.Errorf("update of gateway not allowed")
	}
	if len(newPA.IPs) == 1 && len(newPA.Gateways) == 1 {
		newGateway := newPA.Gateways[0]
		if newGateway.IsUnspecified() {
			return fmt.Errorf("gateway must be valid")
		}
		pa.Gateway = newPA.Gateways[0].String()
	}
	return nil
}

func (pa *podAnnotation) setIPs(newIPs []*net.IPNet) error {
	if len(pa.IPs) != 0 {
		return fmt.Errorf("update of IPs not allowed")
	}
	for _, ip := range newIPs {
		pa.IPs = append(pa.IPs, ip.String())
	}
	return nil
}

func (pa *podAnnotation) setGateways(newGateways []net.IP) error {
	if len(pa.Gateways) > 0 {
		return fmt.Errorf("update of gateways not allowed")
	}
	for _, gateway := range newGateways {
		pa.Gateways = append(pa.Gateways, gateway.String())
	}
	return nil
}

func (pa *podAnnotation) setRoutes(newRoutes []PodRoute) error {
	if len(pa.Routes) != 0 {
		return nil
	}
	for _, candidateRoute := range newRoutes {
		if candidateRoute.Dest == nil {
			klog.Errorf("Route %v should have destination specified", candidateRoute)
			continue
		}
		if candidateRoute.Dest.IP.IsUnspecified() {
			return fmt.Errorf("route %+v should have valid destination", candidateRoute)
		}
		var nh string
		if candidateRoute.NextHop != nil {
			nh = candidateRoute.NextHop.String()
		}
		routeDestStr := candidateRoute.Dest.String()
		newPodRoute := podRoute{
			Dest:    routeDestStr,
			NextHop: nh,
		}
		pa.Routes = append(pa.Routes, newPodRoute)
	}
	return nil
}

func (pa *podAnnotation) ValidateAndMerge(newPA *PodAnnotation) error {
	if newPA == nil {
		return fmt.Errorf("nil pod network annotation is not allowed")
	}
	var err error
	if err = newPA.ensureSingleStackSingleGW(); err != nil {
		return err
	}
	if err = pa.setMAC(newPA.MAC.String()); err != nil {
		return err
	}
	if err = pa.setIP(newPA.IPs); err != nil {
		return err
	}
	if err = pa.setGateway(newPA); err != nil {
		return err
	}
	if err = pa.setIPs(newPA.IPs); err != nil {
		return err
	}
	if err = pa.setGateways(newPA.Gateways); err != nil {
		return err
	}
	if err = pa.setRoutes(newPA.Routes); err != nil {
		return err
	}
	return nil
}

// Internal struct used to marshal PodRoute to the pod annotation
type podRoute struct {
	Dest    string `json:"dest"`
	NextHop string `json:"nextHop"`
}

// MarshalPodAnnotation adds the pod's network details of the specified network to the corresponding pod annotation.
func MarshalPodAnnotation(annotations map[string]string, podInfo *PodAnnotation, netName string, replaceExisting bool) (map[string]string, error) {
	if annotations == nil {
		annotations = make(map[string]string)
	}
	podNetworks, err := unmarshalPodAnnotationAllNetworks(annotations)
	if err != nil {
		return nil, err
	}
	network, ok := podNetworks[netName]
	if !ok || replaceExisting {
		network = podAnnotation{}
	}
	if err = network.ValidateAndMerge(podInfo); err != nil {
		return nil, fmt.Errorf("failed merging pod network annotation (%+v) with new pod network "+
			"annotation (%+v) for network %s: %v", network, podInfo, netName, err)
	}
	podNetworks[netName] = network
	bytes, err := json.Marshal(podNetworks)
	if err != nil {
		return nil, fmt.Errorf("failed marshaling pod networks map %v", podNetworks)
	}
	annotations[OvnPodAnnotationName] = string(bytes)
	return annotations, nil
}

// UnmarshalPodAnnotation returns the Pod's network info of the given network from pod.Annotations
func UnmarshalPodAnnotation(annotations map[string]string, netName string) (*PodAnnotation, error) {
	var err error
	ovnAnnotation, ok := annotations[OvnPodAnnotationName]
	if !ok {
		return nil, newAnnotationNotSetError("could not find OVN pod annotation in %v", annotations)
	}

	podNetworks, err := unmarshalPodAnnotationAllNetworks(annotations)
	if err != nil {
		return nil, err
	}

	tempA, ok := podNetworks[netName]
	if !ok {
		return nil, fmt.Errorf("no ovn pod annotation for network %s: %q",
			netName, ovnAnnotation)
	}

	a := &tempA

	podAnnotation := &PodAnnotation{}
	podAnnotation.MAC, err = net.ParseMAC(a.MAC)
	if err != nil {
		return nil, fmt.Errorf("failed to parse pod MAC %q: %v", a.MAC, err)
	}

	if len(a.IPs) == 0 {
		if a.IP == "" {
			return nil, fmt.Errorf("bad annotation data (neither ip_address nor ip_addresses is set)")
		}
		a.IPs = append(a.IPs, a.IP)
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

func unmarshalPodAnnotationAllNetworks(annotations map[string]string) (map[string]podAnnotation, error) {
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
// Internally it calls GetAllPodIPs
func GetPodCIDRsWithFullMask(pod *v1.Pod) ([]*net.IPNet, error) {
	podIPs, err := GetAllPodIPs(pod)
	if err != nil {
		return nil, err
	}
	ips := make([]*net.IPNet, 0, len(podIPs))
	for _, podIP := range podIPs {
		podIPStr := podIP.String()
		mask := GetIPFullMask(podIPStr)
		_, ipnet, err := net.ParseCIDR(podIPStr + mask)
		if err != nil {
			// this should not happen;
			klog.Warningf("Failed to parse pod IP %v err: %v", podIP, err)
			continue
		}
		ips = append(ips, ipnet)
	}
	return ips, nil
}

// GetAllPodIPs returns the pod's IP addresses, first from the OVN annotation
// and then falling back to the Pod Status IPs. This function is intended to
// also return IPs for HostNetwork and other non-OVN-IPAM-ed pods.
func GetAllPodIPs(pod *v1.Pod) ([]net.IP, error) {
	annotation, err := UnmarshalPodAnnotation(pod.Annotations, types.DefaultNetworkName)
	if err == nil && annotation != nil {
		// Use the OVN annotation if valid
		ips := make([]net.IP, 0, len(annotation.IPs))
		for _, ip := range annotation.IPs {
			ips = append(ips, ip.IP)
		}
		// An OVN annotation should never have empty IPs, but just in case
		if len(ips) > 0 {
			return ips, nil
		}
		klog.Warningf("No IPs found in existing OVN annotation! Pod Name: %s, Annotation: %#v",
			pod.Name, annotation)
	}

	// Otherwise if the annotation is not valid try to use Kube API pod IPs
	ips := make([]net.IP, 0, len(pod.Status.PodIPs))
	for _, podIP := range pod.Status.PodIPs {
		ip := utilnet.ParseIPSloppy(podIP.IP)
		if ip == nil {
			klog.Warningf("Failed to parse pod IP %q", podIP)
			continue
		}
		ips = append(ips, ip)
	}

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

// GetK8sPodDefaultNetwork get pod default network from annotations
func GetK8sPodDefaultNetwork(pod *v1.Pod) (*netattachdefapi.NetworkSelectionElement, error) {
	var netAnnot string

	netAnnot, ok := pod.Annotations[DefNetworkAnnotation]
	if !ok {
		return nil, nil
	}

	networks, err := netattachdefutils.ParseNetworkAnnotation(netAnnot, pod.Namespace)
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

// GetK8sPodAllNetworks get pod's all network NetworkSelectionElement from k8s.v1.cni.cncf.io/networks annotation
func GetK8sPodAllNetworks(pod *v1.Pod) ([]*netattachdefapi.NetworkSelectionElement, error) {
	networks, err := netattachdefutils.ParsePodNetworkAnnotation(pod)
	if err != nil {
		if _, ok := err.(*netattachdefapi.NoK8sNetworkError); !ok {
			return nil, fmt.Errorf("failed to get all NetworkSelectionElements for pod %s/%s: %v", pod.Namespace, pod.Name, err)
		}
		networks = []*netattachdefapi.NetworkSelectionElement{}
	}
	return networks, nil
}
