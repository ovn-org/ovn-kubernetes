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

	// switch-to-router ports only have MAC address and nothing else.
	routerMac, stderr, err := util.RunOVNNbctl("lsp-get-addresses", "stor-"+nodeName)
	if err != nil {
		logrus.Errorf("Failed to retrieve the MAC address of the logical port, stderr: %q, error: %v",
			stderr, err)
		return "", "", "", "", "", err
	}

	return interfaceName, portIP.String(), macAddress, routerIP.IP.String(), routerMac, nil
}

// ManagementPortReady will check to see if the portMac was created
func ManagementPortReady(nodeName string, portName string) (bool, error) {
	portMac, portIP, err := util.GetPortAddresses(portName)
	if err != nil {
		logrus.Errorf("Error while obtaining addresses for %s on node %s - %v", portName, nodeName, err)
		return false, err
	}
	if portMac == nil || portIP == nil {
		return false, nil
	}
	return true, nil
}
