package util

import (
	"fmt"
	"net"
	"strings"
	"sync"

	houtil "github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"

	"github.com/urfave/cli/v2"

	"k8s.io/klog"
	utilnet "k8s.io/utils/net"
)

const (
	// K8sMgmtIntfName name to be used as an OVS internal port on the node
	K8sMgmtIntfName = "ovn-k8s-mp0"

	// PhysicalNetworkName is the name that maps to an OVS bridge that provides
	// access to physical/external network
	PhysicalNetworkName = "physnet"
)

// StringArg gets the named command-line argument or returns an error if it is empty
func StringArg(context *cli.Context, name string) (string, error) {
	val := context.String(name)
	if val == "" {
		return "", fmt.Errorf("argument --%s should be non-null", name)
	}
	return val, nil
}

// GetLegacyK8sMgmtIntfName returns legacy management ovs-port name
func GetLegacyK8sMgmtIntfName(nodeName string) string {
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
		klog.Errorf("No system-id configured in the local host, "+
			"stderr: %q, error: %v", stderr, err)
		return "", err
	}
	if chassisID == "" {
		return "", fmt.Errorf("No system-id configured in the local host")
	}

	return chassisID, nil
}

var updateNodeSwitchLock sync.Mutex

// UpdateNodeSwitchExcludeIPs should be called after adding the management port
// and after adding the hybrid overlay port, and ensures that each port's IP
// is added to the logical switch's exclude_ips. This prevents ovn-northd log
// spam about duplicate IP addresses.
// See https://github.com/ovn-org/ovn-kubernetes/pull/779
func UpdateNodeSwitchExcludeIPs(nodeName string, subnet *net.IPNet) error {
	if utilnet.IsIPv6CIDR(subnet) {
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
			// we always need to set to false because we do not reserve the IP on the LSP for HO
			haveHybridOverlayPort = false
		}
	}

	mgmtIfAddr := GetNodeManagementIfAddr(subnet)
	hybridOverlayIfAddr := GetNodeHybridOverlayIfAddr(subnet)
	var excludeIPs string
	if config.HybridOverlay.Enabled {
		if haveHybridOverlayPort && haveManagementPort {
			// no excluded IPs required
		} else if !haveHybridOverlayPort && !haveManagementPort {
			// exclude both
			excludeIPs = mgmtIfAddr.IP.String() + ".." + hybridOverlayIfAddr.IP.String()
		} else if haveHybridOverlayPort {
			// exclude management port IP
			excludeIPs = mgmtIfAddr.IP.String()
		} else if haveManagementPort {
			// exclude hybrid overlay port IP
			excludeIPs = hybridOverlayIfAddr.IP.String()
		}
	} else if !haveManagementPort {
		// exclude management port IP
		excludeIPs = mgmtIfAddr.IP.String()
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
