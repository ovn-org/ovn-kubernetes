package util

import (
	"encoding/json"
	"fmt"
	"net"

	"k8s.io/klog"
)

const (
	// OvnPodAnnotationName is the constant string representing the POD annotation key
	OvnPodAnnotationName = "k8s.ovn.org/pod-networks"
	// OvnPodDefaultNetwork is the constant string representing the first OVN interface to the Pod
	OvnPodDefaultNetwork = "default"
)

// PodAnnotation describes the pod's assigned network details
type PodAnnotation struct {
	// IP is the pod's assigned IP address and prefix
	IP *net.IPNet
	// MAC is the pod's assigned MAC address
	MAC net.HardwareAddr
	// GW is the pod's gateway IP address
	GW net.IP
	// Routes are routes to add to the pod's network namespace
	Routes []PodRoute
}

// PodRoute describes any routes to be added to the pod's network namespace
type PodRoute struct {
	// Dest is the route destination
	Dest *net.IPNet
	// NextHop is the IP address of the next hop for traffic destined for Dest
	NextHop net.IP
}

// Internal struct used to correctly marshal IPs to JSON
type podAnnotation struct {
	IP     string     `json:"ip_address"`
	MAC    string     `json:"mac_address"`
	GW     string     `json:"gateway_ip"`
	Routes []podRoute `json:"routes,omitempty"`
}

// Internal struct used to correctly marshal IPs to JSON
type podRoute struct {
	Dest    string `json:"dest"`
	NextHop string `json:"nextHop"`
}

// MarshalPodAnnotation returns a JSON-formatted annotation describing the pod's
// network details
func MarshalPodAnnotation(podInfo *PodAnnotation) (map[string]string, error) {
	var gw string
	if podInfo.GW != nil {
		gw = podInfo.GW.String()
	}
	pa := podAnnotation{
		IP:  podInfo.IP.String(),
		MAC: podInfo.MAC.String(),
		GW:  gw,
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

	podNetworks := map[string]podAnnotation{
		OvnPodDefaultNetwork: pa,
	}
	bytes, err := json.Marshal(podNetworks)
	if err != nil {
		klog.Errorf("failed marshaling podNetworks map %v", podNetworks)
		return nil, err
	}
	return map[string]string{
		OvnPodAnnotationName: string(bytes),
	}, nil
}

// UnmarshalPodAnnotation returns a the unmarshalled pod annotation
func UnmarshalPodAnnotation(annotations map[string]string) (*PodAnnotation, error) {
	ovnAnnotation, ok := annotations[OvnPodAnnotationName]
	if !ok {
		return nil, fmt.Errorf("could not find OVN pod annotation in %v", annotations)
	}

	podNetworks := make(map[string]podAnnotation)
	if err := json.Unmarshal([]byte(ovnAnnotation), &podNetworks); err != nil {
		return nil, fmt.Errorf("failed to unmarshal ovn pod annotation %q: %v",
			ovnAnnotation, err)
	}
	tempA := podNetworks[OvnPodDefaultNetwork]
	a := &tempA

	podAnnotation := &PodAnnotation{}
	// Minimal validation
	ip, ipnet, err := net.ParseCIDR(a.IP)
	if err != nil {
		return nil, fmt.Errorf("failed to parse pod IP %q: %v", a.IP, err)
	}
	ipnet.IP = ip
	podAnnotation.IP = ipnet

	podAnnotation.MAC, err = net.ParseMAC(a.MAC)
	if err != nil {
		return nil, fmt.Errorf("failed to parse pod MAC %q: %v", a.MAC, err)
	}

	if a.GW != "" {
		podAnnotation.GW = net.ParseIP(a.GW)
		if podAnnotation.GW == nil {
			return nil, fmt.Errorf("failed to parse pod gateway %q", a.GW)
		}
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
			}
		}
		podAnnotation.Routes = append(podAnnotation.Routes, route)
	}

	return podAnnotation, nil
}
