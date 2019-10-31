// +build windows

package cluster

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/sirupsen/logrus"
)

// CreateManagementPort creates a management port attached to the node switch
// that lets the node access its pods via their private IP address. This is used
// for health checking and other management tasks.
func CreateManagementPort(nodeName string, localSubnet *net.IPNet, clusterSubnet []string) (map[string]string, error) {
	interfaceName, interfaceIP, macAddress, routerIP, _, err :=
		createManagementPortGeneric(nodeName, localSubnet)
	if err != nil {
		return nil, err
	}

	// Up the interface.
	_, _, err = util.RunPowershell("Enable-NetAdapter", "-IncludeHidden", interfaceName)
	if err != nil {
		return nil, err
	}

	//check if interface already exists
	ifAlias := fmt.Sprintf("-InterfaceAlias %s", interfaceName)
	_, _, err = util.RunPowershell("Get-NetIPAddress", ifAlias)
	if err == nil {
		//The interface already exists, we should delete the routes and IP
		logrus.Debugf("Interface %s exists, removing.", interfaceName)
		_, _, err = util.RunPowershell("Remove-NetIPAddress", ifAlias, "-Confirm:$false")
		if err != nil {
			return nil, err
		}
	}

	// Assign IP address to the internal interface.
	portIP, interfaceIPNet, err := net.ParseCIDR(interfaceIP)
	if err != nil {
		return nil, fmt.Errorf("Failed to parse interfaceIP %v : %v", interfaceIP, err)
	}
	portPrefix, _ := interfaceIPNet.Mask.Size()
	_, _, err = util.RunPowershell("New-NetIPAddress",
		fmt.Sprintf("-IPAddress %s", portIP),
		fmt.Sprintf("-PrefixLength %d", portPrefix),
		ifAlias)
	if err != nil {
		return nil, err
	}

	// Set MTU for the interface
	_, _, err = util.RunNetsh("interface", "ipv4", "set", "subinterface",
		interfaceName, fmt.Sprintf("mtu=%d", config.Default.MTU),
		"store=persistent")
	if err != nil {
		return nil, err
	}

	// Retrieve the interface index
	stdout, stderr, err := util.RunPowershell("$(Get-NetAdapter", "-IncludeHidden", "|", "Where",
		"{", "$_.Name", "-Match", fmt.Sprintf("\"%s\"", interfaceName), "}).ifIndex")
	if err != nil {
		logrus.Errorf("Failed to fetch interface index, stderr: %q, error: %v", stderr, err)
		return nil, err
	}
	if _, err = strconv.Atoi(stdout); err != nil {
		logrus.Errorf("Failed to parse interface index %q: %v", stdout, err)
		return nil, err
	}
	interfaceIndex := stdout

	for _, subnet := range clusterSubnet {
		var subnetIP net.IP
		var subnetIPNet *net.IPNet
		subnetIP, subnetIPNet, err = net.ParseCIDR(subnet)
		if err != nil {
			return nil, fmt.Errorf("failed to parse clusterSubnet %v : %v", subnet, err)
		}
		// Checking if the route already exists, in which case it will not be created again
		stdout, stderr, err = util.RunRoute("print", "-4", subnetIP.String())
		if err != nil {
			logrus.Debugf("Failed to run route print, stderr: %q, error: %v", stderr, err)
		}

		if strings.Contains(stdout, subnetIP.String()) {
			logrus.Debugf("Route was found, skipping route add")
		} else {
			// Windows route command requires the mask to be specified in the IP format
			subnetMask := net.IP(subnetIPNet.Mask).String()
			// Create a route for the entire subnet.
			_, stderr, err = util.RunRoute("-p", "add",
				subnetIP.String(), "mask", subnetMask,
				routerIP, "METRIC", "2", "IF", interfaceIndex)
			if err != nil {
				logrus.Errorf("failed to run route add, stderr: %q, error: %v", stderr, err)
				return nil, err
			}
		}
	}

	clusterServiceIP, clusterServiceIPNet, err := net.ParseCIDR(config.Kubernetes.ServiceCIDR)
	if err != nil {
		return nil, fmt.Errorf("Failed to parse clusterServicesSubnet %v : %v", config.Kubernetes.ServiceCIDR, err)
	}
	// Checking if the route already exists, in which case it will not be created again
	stdout, stderr, err = util.RunRoute("print", "-4", clusterServiceIP.String())
	if err != nil {
		logrus.Debugf("Failed to run route print, stderr: %q, error: %v", stderr, err)
	}

	if strings.Contains(stdout, clusterServiceIP.String()) {
		logrus.Debugf("Route was found, skipping route add")
	} else {
		// Windows route command requires the mask to be specified in the IP format
		clusterServiceMask := net.IP(clusterServiceIPNet.Mask).String()
		// Create a route for the entire subnet.
		_, stderr, err = util.RunRoute("-p", "add",
			clusterServiceIP.String(), "mask", clusterServiceMask,
			routerIP, "METRIC", "2", "IF", interfaceIndex)
		if err != nil {
			logrus.Errorf("failed to run route add, stderr: %q, error: %v", stderr, err)
			return nil, err
		}
	}

	return map[string]string{ovn.OvnNodeManagementPortMacAddress: macAddress}, nil
}

func DelMgtPortIptRules(nodeName string) {
}
