package cluster

import (
	"fmt"

	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/util"
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
	err = util.GatewayInit(clusterIPSubnet, nodeName, ipAddress,
		gwIntf, "", gwNextHop, subnet, gwVLANId, nodeportEnable)
	if err != nil {
		return fmt.Errorf("failed to init spare interface gateway: %v", err)
	}

	return nil
}
