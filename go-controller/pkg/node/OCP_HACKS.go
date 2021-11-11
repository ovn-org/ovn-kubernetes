// +build linux

package node

import (
	"fmt"
	"net"

	"github.com/coreos/go-iptables/iptables"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

// Block MCS Access. https://github.com/openshift/ovn-kubernetes/pull/170
func generateBlockMCSRules(rules *[]iptRule, protocol iptables.Protocol) {
	var delRules []iptRule

	for _, chain := range []string{"FORWARD", "OUTPUT"} {
		for _, port := range []string{"22623", "22624"} {
			*rules = append(*rules, iptRule{
				table:    "filter",
				chain:    chain,
				args:     []string{"-p", "tcp", "-m", "tcp", "--dport", port, "--syn", "-j", "REJECT"},
				protocol: protocol,
			})
			// Delete the old "--syn"-less rules on upgrade
			delRules = append(delRules, iptRule{
				table:    "filter",
				chain:    chain,
				args:     []string{"-p", "tcp", "-m", "tcp", "--dport", port, "-j", "REJECT"},
				protocol: protocol,
			})
		}
	}

	_ = delIptRules(delRules)
}

// initSharedGatewayNoBridge is used in order to run local gateway mode without moving the NIC to an ovs bridge
// https://github.com/openshift/ovn-kubernetes/pull/281
func initSharedGatewayNoBridge(nodeName string, subnets []*net.IPNet, gwNextHops []net.IP, nodeAnnotator kube.Annotator) (postWaitFunc, error) {
	err := setupLocalNodeAccessBridge(nodeName, subnets)
	if err != nil {
		return nil, err
	}
	chassisID, err := util.GetNodeChassisID()
	if err != nil {
		return nil, err
	}
	// get the real default interface
	defaultGatewayIntf, _, err := getDefaultGatewayInterfaceDetails()
	if err != nil {
		return nil, err
	}
	ips, err := getNetworkInterfaceIPAddresses(defaultGatewayIntf)
	if err != nil {
		return nil, fmt.Errorf("failed to get interface details for %s (%v)",
			defaultGatewayIntf, err)
	}
	err = util.SetL3GatewayConfig(nodeAnnotator, &util.L3GatewayConfig{
		ChassisID:      chassisID,
		Mode:           config.GatewayModeLocal,
		IPAddresses:    ips,
		MACAddress:     util.IPAddrToHWAddr(ips[0].IP),
		NextHops:       gwNextHops,
		NodePortEnable: config.Gateway.NodeportEnable,
	})
	if err != nil {
		return nil, err
	} else {
		return func() error { return nil }, nil
	}
}
