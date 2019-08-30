package ovn

import (
	"fmt"
	"net"
	"strings"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/sirupsen/logrus"
)

func createManagementPortGeneric(nodeName, localSubnet string, clusterSubnet []string) (string, string, string, string, error) {
	// Determine the IP of the node switch's logical router port on the cluster router
	ip, subnet, err := net.ParseCIDR(localSubnet)
	if err != nil {
		return "", "", "", "", fmt.Errorf("Failed to parse local subnet %s: %v", localSubnet, err)
	}
	ip = util.NextIP(ip)
	routerIP := ip.String()

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
		return "", "", "", "", err
	}

	// Create a OVS internal interface.
	interfaceName := util.GetK8sMgmtIntfName(nodeName)

	stdout, stderr, err = util.RunOVSVsctl("--", "--may-exist", "add-port",
		"br-int", interfaceName, "--", "set", "interface", interfaceName,
		"type=internal", "mtu_request="+fmt.Sprintf("%d", config.Default.MTU),
		"external-ids:iface-id=k8s-"+nodeName)
	if err != nil {
		logrus.Errorf("Failed to add port to br-int, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
		return "", "", "", "", err
	}
	macAddress, err := util.GetOVSPortMACAddress(interfaceName)
	if err != nil {
		logrus.Errorf("Failed to get management port MAC address: %v", err)
		return "", "", "", "", err
	}

	// Create this node's management logical port on the node switch. Now that the second subnet IP is
	// statically allocated to the management logical port, we can safely remove the other_config:exclude_ips
	// in the same transaction to avoid "Duplicate IP set" warning messages in ovn-northd.
	ip = util.NextIP(ip)
	portIP := ip.String()
	n, _ := subnet.Mask.Size()
	portIPMask := fmt.Sprintf("%s/%d", portIP, n)
	stdout, stderr, err = util.RunOVNNbctl("--", "--may-exist", "lsp-add", nodeName, "k8s-"+nodeName,
		"--", "lsp-set-addresses", "k8s-"+nodeName, macAddress+" "+portIP,
		"--", "--if-exists", "remove", "logical_switch", nodeName, "other-config", "exclude_ips")
	if err != nil {
		logrus.Errorf("Failed to add logical port to switch, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
		return "", "", "", "", err
	}
	// switch-to-router ports only have MAC address and nothing else.
	routerMac, stderr, err := util.RunOVNNbctl("lsp-get-addresses", "stor-"+nodeName)
	if err != nil {
		logrus.Errorf("Failed to retrieve the MAC address of the logical port, stderr: %q, error: %v",
			stderr, err)
		return "", "", "", "", err
	}

	return interfaceName, portIPMask, routerIP, routerMac, nil
}
