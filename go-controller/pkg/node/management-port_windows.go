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
)

// createPlatformManagementPort creates a management port attached to the node switch
// that lets the node access its pods via their private IP address. This is used
// for health checking and other management tasks.
func createPlatformManagementPort(interfaceName, interfaceIP, routerIP, routerMAC string) error {
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
		// Checking if the route already exists, in which case it will not be created again
		stdout, stderr, err = util.RunRoute("print", "-4", subnet.CIDR.IP.String())
		if err != nil {
			klog.V(5).Infof("Failed to run route print, stderr: %q, error: %v", stderr, err)
		}

		if strings.Contains(stdout, subnet.CIDR.IP.String()) {
			klog.V(5).Infof("Route was found, skipping route add")
		} else {
			// Windows route command requires the mask to be specified in the IP format
			subnetMask := net.IP(subnet.CIDR.Mask).String()
			// Create a route for the entire subnet.
			_, stderr, err = util.RunRoute("-p", "add",
				subnet.CIDR.IP.String(), "mask", subnetMask,
				routerIP, "METRIC", "2", "IF", interfaceIndex)
			if err != nil {
				klog.Errorf("failed to run route add, stderr: %q, error: %v", stderr, err)
				return err
			}
		}
	}

	clusterServiceIP, clusterServiceIPNet, err := net.ParseCIDR(config.Kubernetes.ServiceCIDR)
	if err != nil {
		return fmt.Errorf("failed to parse ServiceCIDR %v : %v", config.Kubernetes.ServiceCIDR, err)
	}
	// Checking if the route already exists, in which case it will not be created again
	stdout, stderr, err = util.RunRoute("print", "-4", clusterServiceIP.String())
	if err != nil {
		klog.V(5).Infof("Failed to run route print, stderr: %q, error: %v", stderr, err)
	}

	if strings.Contains(stdout, clusterServiceIP.String()) {
		klog.V(5).Infof("Route was found, skipping route add")
	} else {
		// Windows route command requires the mask to be specified in the IP format
		clusterServiceMask := net.IP(clusterServiceIPNet.Mask).String()
		// Create a route for the entire subnet.
		_, stderr, err = util.RunRoute("-p", "add",
			clusterServiceIP.String(), "mask", clusterServiceMask,
			routerIP, "METRIC", "2", "IF", interfaceIndex)
		if err != nil {
			klog.Errorf("failed to run route add, stderr: %q, error: %v", stderr, err)
			return err
		}
	}

	return nil
}

func DelMgtPortIptRules(nodeName string) {
}
