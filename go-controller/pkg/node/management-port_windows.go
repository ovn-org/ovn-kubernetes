// +build windows

package node

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	"k8s.io/klog"
	utilnet "k8s.io/utils/net"
)

// createPlatformManagementPort creates a management port attached to the node switch
// that lets the node access its pods via their private IP address. This is used
// for health checking and other management tasks.
func createPlatformManagementPort(interfaceName, interfaceIP, routerIP, routerMAC string,
	stopChan chan struct{}) error {
	// Up the interface.
	_, _, err := util.RunPowershell("Enable-NetAdapter", "-IncludeHidden", interfaceName)
	if err != nil {
		return err
	}

	//check if interface already exists
	ifAlias := fmt.Sprintf("-InterfaceAlias %s", interfaceName)
	_, _, err = util.RunPowershell("Get-NetIPAddress", ifAlias)
	if err == nil {
		//The interface already exists, we should delete the routes and IP
		klog.V(5).Infof("Interface %s exists, removing.", interfaceName)
		_, _, err = util.RunPowershell("Remove-NetIPAddress", ifAlias, "-Confirm:$false")
		if err != nil {
			return err
		}
	}

	// Assign IP address to the internal interface.
	portIP, interfaceIPNet, err := net.ParseCIDR(interfaceIP)
	if err != nil {
		return fmt.Errorf("Failed to parse interfaceIP %v : %v", interfaceIP, err)
	}
	portPrefix, _ := interfaceIPNet.Mask.Size()
	_, _, err = util.RunPowershell("New-NetIPAddress",
		fmt.Sprintf("-IPAddress %s", portIP),
		fmt.Sprintf("-PrefixLength %d", portPrefix),
		ifAlias)
	if err != nil {
		return err
	}

	// Set MTU for the interface
	_, _, err = util.RunNetsh("interface", "ipv4", "set", "subinterface",
		interfaceName, fmt.Sprintf("mtu=%d", config.Default.MTU),
		"store=persistent")
	if err != nil {
		return err
	}

	// Retrieve the interface index
	stdout, stderr, err := util.RunPowershell("$(Get-NetAdapter", "-IncludeHidden", "|", "Where",
		"{", "$_.Name", "-Match", fmt.Sprintf("\"%s\"", interfaceName), "}).ifIndex")
	if err != nil {
		klog.Errorf("Failed to fetch interface index, stderr: %q, error: %v", stderr, err)
		return err
	}
	if _, err = strconv.Atoi(stdout); err != nil {
		klog.Errorf("Failed to parse interface index %q: %v", stdout, err)
		return err
	}
	interfaceIndex := stdout

	for _, subnet := range config.Default.ClusterSubnets {
		err = addRoute(subnet.CIDR, routerIP, interfaceIndex)
		if err != nil {
			return err
		}
	}
	for _, subnet := range config.Kubernetes.ServiceCIDRs {
		err = addRoute(subnet, routerIP, interfaceIndex)
		if err != nil {
			return err
		}
	}

	return nil
}

func addRoute(subnet *net.IPNet, routerIP, interfaceIndex string) error {
	var familyFlag string
	if utilnet.IsIPv6CIDR(subnet) {
		familyFlag = "-6"
	} else {
		familyFlag = "-4"
	}

	// If the route already exists, don't create it again
	stdout, stderr, err := util.RunRoute("print", familyFlag, subnet.IP.String())
	if err != nil {
		klog.V(5).Infof("Failed to run route print, stderr: %q, error: %v", stderr, err)
	}
	if strings.Contains(stdout, subnet.IP.String()) {
		klog.V(5).Infof("Route was found, skipping route add")
		return nil
	}

	// Windows route command requires the mask to be specified in the IP format
	subnetMask := net.IP(subnet.Mask).String()
	// Create a route for the entire subnet.
	_, stderr, err = util.RunRoute("-p", "add",
		subnet.IP.String(), "mask", subnetMask,
		routerIP, "METRIC", "2", "IF", interfaceIndex)
	if err != nil {
		return fmt.Errorf("failed to run route add, stderr: %q, error: %v", stderr, err)
	}
	return nil
}

func DelMgtPortIptRules() {
}
