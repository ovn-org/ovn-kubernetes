package cluster

import (
	"fmt"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

func initSpareGateway(nodeName string, clusterIPSubnet []string,
	subnet, gwNextHop, gwIntf string, gwVLANId uint, nodeportEnable bool) error {

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
		gwIntf, "external-ids:iface-id=" + ifaceID}
	if gwVLANId != 0 {
		addPortCmdArgs = append(addPortCmdArgs, "--", "set", "port", gwIntf,
			fmt.Sprintf("tag=%d", gwVLANId))
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
		macAddress, gwNextHop, subnet, false, 0, nodeportEnable)
	if err != nil {
		return fmt.Errorf("failed to init spare interface gateway: %v", err)
	}

	return nil
}
