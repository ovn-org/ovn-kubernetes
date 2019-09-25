package util

import (
	"encoding/json"
	"fmt"
	"net"

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

// PodAnnotation describes the pod's assigned network details
type PodAnnotation struct {
	// IP is the pod's assigned IP address
	IP string `json:"ip_address"`
	// MAC is the pod's assigned MAC address
	MAC string `json:"mac_address"`
	// GW is the pod's gateway IP address
	GW string `json:"gateway_ip"`
	// Routes are routes to add to the pod's network namespace
	Routes []PodRoute `json:"routes,omitempty"`
}

// PodRoute describes any routes to be added to the pod's network namespace
type PodRoute struct {
	// Dest is the route destination
	Dest string `json:"dest"`
	// NextHop is the IP address of the next hop for traffic destined for Dest
	NextHop string `json:"nextHop"`
}

// MarshalPodAnnotation returns a JSON-formatted annotation describing the pod's
// network details
func MarshalPodAnnotation(ip, mac, gw fmt.Stringer, routes []PodRoute) (string, error) {
	if net.ParseIP(gw.String()) == nil {
		// Ensure we are never passed a net.IPNet
		return "", fmt.Errorf("invalid IP %s", gw)
	}
	bytes, err := json.Marshal(PodAnnotation{
		IP:     ip.String(),
		MAC:    mac.String(),
		GW:     gw.String(),
		Routes: routes,
	})
	return string(bytes), err
}

// UnmarshalPodAnnotation returns a the unmarshalled pod annotation
func UnmarshalPodAnnotation(annotation string) (*PodAnnotation, error) {
	a := &PodAnnotation{}
	if err := json.Unmarshal([]byte(annotation), a); err != nil {
		return nil, err
	}
	return a, nil
}
