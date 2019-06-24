package cluster

import (
	"fmt"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

func initSpareGateway(nodeName string, clusterIPSubnet []string,
	subnet, gwNextHop, gwIntf string) error {

	// Now, we get IP address from physical interface. If IP does not
	// exists error out.
	ipAddress, err := getIPv4Address(gwIntf)
	if err != nil {
		return fmt.Errorf("Failed to get interface details for %s (%v)",
			gwIntf, err)
	}
	if ipAddress == "" {
		return fmt.Errorf("%s does not have a ipv4 address", gwIntf)
	}

	// Connect physical interface to br-int. Get its mac address.
	ifaceID := gwIntf + "_" + nodeName
	addPortCmdArgs := []string{"--", "--may-exist", "add-port",
		"br-int", gwIntf, "--", "set", "interface",
		gwIntf, "external-ids:iface-id=" + ifaceID,
		"external-ids:physical-ip=" + ipAddress}
	if config.Gateway.VLANID != 0 {
		addPortCmdArgs = append(addPortCmdArgs, "--", "set", "port", gwIntf,
			fmt.Sprintf("tag=%d", config.Gateway.VLANID))
	}
	stdout, stderr, err := util.RunOVSVsctl(addPortCmdArgs...)
	if err != nil {
		return fmt.Errorf("Failed to add port to br-int, stdout: %q, "+
			"stderr: %q, error: %v", stdout, stderr, err)
	}
	macAddress, stderr, err := util.RunOVSVsctl("--if-exists", "get",
		"interface", gwIntf, "mac_in_use")
	if err != nil {
		return fmt.Errorf("Failed to get macAddress, stderr: %q, error: %v",
			stderr, err)
	}

	// Flush the IP address of the physical interface.
	_, _, err = util.RunIP("addr", "flush", "dev", gwIntf)
	if err != nil {
		return err
	}

	err = util.GatewayInit(clusterIPSubnet, nodeName, ifaceID, ipAddress,
		macAddress, gwNextHop, subnet, false, nil)
	if err != nil {
		return fmt.Errorf("failed to init spare interface gateway: %v", err)
	}

	return nil
}

func cleanupSpareGateway(physicalInterface, nodeName string) error {
	ifaceID := physicalInterface + "_" + nodeName
	stdout, stderr, err := util.RunOVSVsctl("--data=bare", "--columns=name", "--no-heading", "find", "interface",
		"external-ids:iface-id="+ifaceID)
	if err != nil {
		return fmt.Errorf("Failed to get the physical interface %q on br-int stderr:%s (%v)",
			physicalInterface, stderr, err)
	}
	nicIP, stderr, err := util.RunOVSVsctl("--data=bare", "get", "interface", stdout, "external-ids:physical-ip")
	if err != nil {
		return fmt.Errorf("Failed to get the IP associated with the physical interface %q, stderr:%s (%v)",
			stdout, stderr, err)
	}
	_, _, err = util.RunIP("addr", "add", nicIP, "dev", physicalInterface)
	if err != nil {
		return fmt.Errorf("Failed to add IP address %s back to interface %s, error: %v",
			nicIP, physicalInterface, err)
	}

	return nil
}
