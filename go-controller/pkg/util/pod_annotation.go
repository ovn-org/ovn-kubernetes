package util

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"

	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	nadutils "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/utils"
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
	// SkipIPConfig if set to true will skip the pod's interface ip
	// configuration
	SkipIPConfig *bool
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

	SkipIPConfig *bool `json:"skip_ip_config,omitempty"`
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
		MAC:          podInfo.MAC.String(),
		SkipIPConfig: podInfo.SkipIPConfig,
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
		if len(existingPa.IPs) > 0 {
			if len(pa.IPs) != len(existingPa.IPs) {
				return nil, ErrOverridePodIPs
			}
			for _, ip := range pa.IPs {
				if !SliceHasStringItem(existingPa.IPs, ip) {
					return nil, ErrOverridePodIPs
				}
			}
		}
		if pa.SkipIPConfig == nil {
			pa.SkipIPConfig = existingPa.SkipIPConfig
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

	podAnnotation := &PodAnnotation{}
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

	podAnnotation.SkipIPConfig = a.SkipIPConfig

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

// GetPodIPsOfNetwork returns the pod's IP addresses, first from the OVN annotation
// and then falling back to the Pod Status IPs. This function is intended to
// also return IPs for HostNetwork and other non-OVN-IPAM-ed pods.
func GetPodIPsOfNetwork(pod *v1.Pod, nInfo NetInfo) ([]net.IP, error) {
	ips := []net.IP{}
	networkMap := map[string]*nadapi.NetworkSelectionElement{}
	if !nInfo.IsSecondary() {
		// default network, Pod annotation is under the name of DefaultNetworkName
		networkMap[types.DefaultNetworkName] = nil
	} else {
		var err error
		var on bool

		on, networkMap, err = GetPodNADToNetworkMapping(pod, nInfo)
		if err != nil {
			return nil, err
		} else if !on {
			// the pod is not attached to this specific network, don't return error
			return []net.IP{}, nil
		}
	}
	for nadName := range networkMap {
		annotation, _ := UnmarshalPodAnnotation(pod.Annotations, nadName)
		if annotation != nil {
			// Use the OVN annotation if valid
			for _, ip := range annotation.IPs {
				ips = append(ips, ip.IP)
			}
			// An OVN annotation should never have empty IPs, but just in case
			if len(ips) == 0 {
				klog.Warningf("No IPs found in existing OVN annotation for NAD %s! Pod Name: %s, Annotation: %#v",
					nadName, pod.Name, annotation)
			}
		}
	}
	if len(ips) != 0 {
		return ips, nil
	}

	if nInfo.IsSecondary() {
		return []net.IP{}, fmt.Errorf("no pod annotation of pod %s/%s found for network %s",
			pod.Namespace, pod.Name, nInfo.GetNetworkName())
	}

	// Otherwise, default network, if the annotation is not valid try to use Kube API pod IPs
	ips = make([]net.IP, 0, len(pod.Status.PodIPs))
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
