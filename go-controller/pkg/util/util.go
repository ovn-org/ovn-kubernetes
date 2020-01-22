package util

import (
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"

	houtil "github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"

	"github.com/sirupsen/logrus"
	"github.com/urfave/cli"
)

// StringArg gets the named command-line argument or returns an error if it is empty
func StringArg(context *cli.Context, name string) (string, error) {
	val := context.String(name)
	if val == "" {
		return "", fmt.Errorf("argument --%s should be non-null", name)
	}
	return val, nil
}

// GetK8sMgmtIntfName returns the correct length interface name to be used
// as an OVS internal port on the node
func GetK8sMgmtIntfName(nodeName string) string {
	if len(nodeName) > 11 {
		return "k8s-" + (nodeName[:11])
	}
	return "k8s-" + nodeName
}

// GetNodeChassisID returns the machine's OVN chassis ID
func GetNodeChassisID() (string, error) {
	chassisID, stderr, err := RunOVSVsctl("--if-exists", "get",
		"Open_vSwitch", ".", "external_ids:system-id")
	if err != nil {
		logrus.Errorf("No system-id configured in the local host, "+
			"stderr: %q, error: %v", stderr, err)
		return "", err
	}
	if chassisID == "" {
		return "", fmt.Errorf("No system-id configured in the local host")
	}

	return chassisID, nil
}

var updateNodeSwitchLock sync.Mutex

func UpdateNodeSwitchExcludeIPs(nodeName string, subnet *net.IPNet) error {
	if config.IPv6Mode {
		// We don't exclude any IPs in IPv6
		return nil
	}

	updateNodeSwitchLock.Lock()
	defer updateNodeSwitchLock.Unlock()

	stdout, stderr, err := RunOVNNbctl("lsp-list", nodeName)
	if err != nil {
		return fmt.Errorf("failed to list logical switch %q ports: stderr: %q, error: %v", nodeName, stderr, err)
	}

	var haveManagementPort, haveHybridOverlayPort bool
	lines := strings.Split(stdout, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "(k8s-"+nodeName+")") {
			haveManagementPort = true
		} else if strings.Contains(line, "("+houtil.GetHybridOverlayPortName(nodeName)+")") {
			haveHybridOverlayPort = true
		}
	}

	_, managementPortCIDR := GetNodeWellKnownAddresses(subnet)
	hybridOverlayIP := NextIP(managementPortCIDR.IP)

	// Only exclude the hybrid overlay IP if host subnets are big enough
	excludeHybridOverlayIP := true
	for _, clusterEntry := range config.Default.ClusterSubnets {
		if clusterEntry.HostSubnetLength > 24 {
			excludeHybridOverlayIP = false
			break
		}
	}

	var excludeIPs string
	if excludeHybridOverlayIP {
		if haveHybridOverlayPort && haveManagementPort {
			// no excluded IPs required
		} else if !haveHybridOverlayPort && !haveManagementPort {
			// exclude both
			excludeIPs = managementPortCIDR.IP.String() + ".." + hybridOverlayIP.String()
		} else if haveHybridOverlayPort {
			// exclude management port IP
			excludeIPs = managementPortCIDR.IP.String()
		} else if haveManagementPort {
			// exclude hybrid overlay port IP
			excludeIPs = hybridOverlayIP.String()
		}
	} else if !haveManagementPort {
		// exclude management port IP
		excludeIPs = managementPortCIDR.IP.String()
	}

	args := []string{"--", "--if-exists", "remove", "logical_switch", nodeName, "other-config", "exclude_ips"}
	if len(excludeIPs) > 0 {
		args = []string{"--", "--if-exists", "set", "logical_switch", nodeName, "other-config:exclude_ips=" + excludeIPs}
	}

	_, stderr, err = RunOVNNbctl(args...)
	if err != nil {
		return fmt.Errorf("failed to set node %q switch exclude_ips, "+
			"stderr: %q, error: %v", nodeName, stderr, err)
	}
	return nil
}

const (
	// OvnPodAnnotationLegacyName is the old POD annotation key string, kept for backward compatibility only
	OvnPodAnnotationLegacyName = "ovn"
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
		var nh string
		if r.NextHop != nil {
			nh = r.NextHop.String()
		}
		pa.Routes = append(pa.Routes, podRoute{
			Dest:    r.Dest.String(),
			NextHop: nh,
		})
	}

	// We need to annotate pod with both the legacy and new annotation name. This is in case
	// if there are some nodes that have not been upgraded to understand the new Pod annotation
	bytes, err := json.Marshal(pa)
	if err != nil {
		logrus.Errorf("failed marshaling podAnnotation structure %v", pa)
		return nil, err
	}
	legacyValue := string(bytes)
	podNetworks := map[string]podAnnotation{
		OvnPodDefaultNetwork: pa,
	}
	bytes, err = json.Marshal(podNetworks)
	if err != nil {
		logrus.Errorf("failed marshaling podNetworks map %v", podNetworks)
		return nil, err
	}
	return map[string]string{
		OvnPodAnnotationLegacyName: legacyValue,
		OvnPodAnnotationName:       string(bytes),
	}, nil
}

// UnmarshalPodAnnotation returns a the unmarshalled pod annotation
func UnmarshalPodAnnotation(annotations map[string]string) (*PodAnnotation, error) {
	a := &podAnnotation{}

	ovnAnnotation, ok := annotations[OvnPodAnnotationName]
	if !ok {
		ovnAnnotation, ok = annotations[OvnPodAnnotationLegacyName]
		if !ok {
			return nil, fmt.Errorf("could not find OVN pod annotation in %v", annotations)
		}
		if err := json.Unmarshal([]byte(ovnAnnotation), a); err != nil {
			return nil, err
		}
	} else {
		podNetworks := make(map[string]podAnnotation)
		if err := json.Unmarshal([]byte(ovnAnnotation), &podNetworks); err != nil {
			return nil, err
		}
		tempA := podNetworks[OvnPodDefaultNetwork]
		a = &tempA
	}

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
		if r.NextHop != "" {
			route.NextHop = net.ParseIP(r.NextHop)
			if route.NextHop == nil {
				return nil, fmt.Errorf("failed to parse pod route next hop %q", a.GW)
			}
		}
		podAnnotation.Routes = append(podAnnotation.Routes, route)
	}

	return podAnnotation, nil
}
