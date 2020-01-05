package cluster

import (
	"fmt"
	"net"
	"strings"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/sirupsen/logrus"
)

func createManagementPortGeneric(nodeName string, localSubnet *net.IPNet) (string, string, string, string, string, error) {
	// Retrieve the routerIP and mangementPortIP for a given localSubnet
	routerIP, portIP := util.GetNodeWellKnownAddresses(localSubnet)
	routerMac := util.IPAddrToHWAddr(routerIP.IP)

	// Kubernetes emits events when pods are created. The event will contain
	// only lowercase letters of the hostname even though the kubelet is
	// started with a hostname that contains lowercase and uppercase letters.
	// When the kubelet is started with a hostname containing lowercase and
	// uppercase letters, this causes a mismatch between what the watcher
	// will try to fetch and what kubernetes provides, thus failing to
	// create the port on the logical switch.
	// Until the above is changed, switch to a lowercase hostname for
	// initMinion.
	nodeName = strings.ToLower(nodeName)

	// Make sure br-int is created.
	stdout, stderr, err := util.RunOVSVsctl("--", "--may-exist", "add-br", "br-int")
	if err != nil {
		logrus.Errorf("Failed to create br-int, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
		return "", "", "", "", "", err
	}

	// Create a OVS internal interface.
	interfaceName := util.GetK8sMgmtIntfName(nodeName)

	stdout, stderr, err = util.RunOVSVsctl("--", "--may-exist", "add-port",
		"br-int", interfaceName, "--", "set", "interface", interfaceName,
		"type=internal", "mtu_request="+fmt.Sprintf("%d", config.Default.MTU),
		"external-ids:iface-id=k8s-"+nodeName)
	if err != nil {
		logrus.Errorf("Failed to add port to br-int, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
		return "", "", "", "", "", err
	}
	macAddress, err := util.GetOVSPortMACAddress(interfaceName)
	if err != nil {
		logrus.Errorf("Failed to get management port MAC address: %v", err)
		return "", "", "", "", "", err
	}
	// persist the MAC address so that upon node reboot we get back the same mac address.
	_, stderr, err = util.RunOVSVsctl("set", "interface", interfaceName,
		fmt.Sprintf("mac=%s", strings.ReplaceAll(macAddress, ":", "\\:")))
	if err != nil {
		logrus.Errorf("failed to persist MAC address %q for %q: stderr:%s (%v)", macAddress,
			interfaceName, stderr, err)
		return "", "", "", "", "", err
	}

	return interfaceName, portIP.String(), macAddress, routerIP.IP.String(), routerMac, nil
}

// ManagementPortReady will check to see if OpenFlow rules for management port has been created
func ManagementPortReady(nodeName string, portName string) (bool, error) {
	// Get the OVS interface name for the Management Port
	interfaceName := util.GetK8sMgmtIntfName(nodeName)
	ofport, _, err := util.RunOVSVsctl("--if-exists", "get", "interface", interfaceName, "ofport")
	if err != nil {
		return false, nil
	}

	// OpenFlow table 65 performs logical-to-physical translation. It matches the packet’s logical
	// egress  port. Its actions output the packet to the port attached to the OVN integration bridge
	// that represents that logical  port.
	stdout, _, err := util.RunOVSOfctl("--no-stats", "--no-names", "dump-flows", "br-int",
		"table=65,out_port="+ofport)
	if err != nil {
		return false, nil
	}
	if !strings.Contains(stdout, "actions=output:"+ofport) {
		return false, nil
	}
	return true, nil
}
