// +build linux

package cluster

import (
	"fmt"
	"strings"

	"github.com/coreos/go-iptables/iptables"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/util"
)

const (
	localnetGatewayIP            = "169.254.33.2/24"
	localnetGatewayNextHop       = "169.254.33.1"
	localnetGatewayNextHopSubnet = "169.254.33.1/24"
)

type iptRule struct {
	table string
	chain string
	args  []string
}

func ensureChain(ipt util.IPTablesHelper, table, chain string) error {
	chains, err := ipt.ListChains(table)
	if err != nil {
		return fmt.Errorf("failed to list iptables chains: %v", err)
	}
	for _, ch := range chains {
		if ch == chain {
			return nil
		}
	}

	return ipt.NewChain(table, chain)
}

func generateGatewayNATRules(ifname string, ip string) []iptRule {
	// Allow packets to/from the gateway interface in case defaults deny
	rules := make([]iptRule, 0)
	rules = append(rules, iptRule{
		table: "filter",
		chain: "FORWARD",
		args:  []string{"-i", ifname, "-j", "ACCEPT"},
	})
	rules = append(rules, iptRule{
		table: "filter",
		chain: "FORWARD",
		args: []string{"-o", ifname, "-m", "conntrack", "--ctstate",
			"RELATED,ESTABLISHED", "-j", "ACCEPT"},
	})
	rules = append(rules, iptRule{
		table: "filter",
		chain: "INPUT",
		args:  []string{"-i", ifname, "-m", "comment", "--comment", "from OVN to localhost", "-j", "ACCEPT"},
	})

	// NAT for the interface
	rules = append(rules, iptRule{
		table: "nat",
		chain: "POSTROUTING",
		args:  []string{"-s", ip, "-j", "MASQUERADE"},
	})
	return rules
}

func localnetGatewayNAT(ipt util.IPTablesHelper, ifname, ip string) error {
	rules := generateGatewayNATRules(ifname, ip)
	for _, r := range rules {
		if err := ensureChain(ipt, r.table, r.chain); err != nil {
			return fmt.Errorf("failed to ensure %s/%s: %v", r.table, r.chain, err)
		}
		exists, err := ipt.Exists(r.table, r.chain, r.args...)
		if !exists && err == nil {
			err = ipt.Insert(r.table, r.chain, 1, r.args...)
		}
		if err != nil {
			return fmt.Errorf("failed to add iptables %s/%s rule %q: %v",
				r.table, r.chain, strings.Join(r.args, " "), err)
		}
	}

	return nil
}

func initLocalnetGateway(nodeName string, clusterIPSubnet []string,
	subnet string, nodePortEnable bool) error {
	ipt, err := iptables.NewWithProtocol(iptables.ProtocolIPv4)
	if err != nil {
		return fmt.Errorf("failed to initialize iptables: %v", err)
	}
	return initLocalnetGatewayInternal(nodeName, clusterIPSubnet, subnet, ipt,
		nodePortEnable)
}

func initLocalnetGatewayInternal(nodeName string, clusterIPSubnet []string,
	subnet string, ipt util.IPTablesHelper, nodePortEnable bool) error {
	// Create a localnet OVS bridge.
	localnetBridgeName := "br-local"
	_, stderr, err := util.RunOVSVsctl("--may-exist", "add-br",
		localnetBridgeName)
	if err != nil {
		return fmt.Errorf("Failed to create localnet bridge %s"+
			", stderr:%s (%v)", localnetBridgeName, stderr, err)
	}

	// ovn-bridge-mappings maps a physical network name to a local ovs bridge
	// that provides connectivity to that network.
	_, stderr, err = util.RunOVSVsctl("set", "Open_vSwitch", ".",
		fmt.Sprintf("external_ids:ovn-bridge-mappings=%s:%s", util.PhysicalNetworkName, localnetBridgeName))
	if err != nil {
		return fmt.Errorf("Failed to set ovn-bridge-mappings for ovs bridge %s"+
			", stderr:%s (%v)", localnetBridgeName, stderr, err)
	}

	_, _, err = util.RunIP("link", "set", localnetBridgeName, "up")
	if err != nil {
		return fmt.Errorf("failed to up %s (%v)", localnetBridgeName, err)
	}

	// Create a localnet bridge nexthop
	localnetBridgeNextHop := "br-nexthop"
	_, stderr, err = util.RunOVSVsctl("--may-exist", "add-port",
		localnetBridgeName, localnetBridgeNextHop, "--", "set",
		"interface", localnetBridgeNextHop, "type=internal")
	if err != nil {
		return fmt.Errorf("Failed to create localnet bridge next hop %s"+
			", stderr:%s (%v)", localnetBridgeNextHop, stderr, err)
	}
	_, _, err = util.RunIP("link", "set", localnetBridgeNextHop, "up")
	if err != nil {
		return fmt.Errorf("failed to up %s (%v)", localnetBridgeNextHop, err)
	}

	// Flush IPv4 address of localnetBridgeNextHop.
	_, _, err = util.RunIP("addr", "flush", "dev", localnetBridgeNextHop)
	if err != nil {
		return fmt.Errorf("failed to flush ip address of %s (%v)",
			localnetBridgeNextHop, err)
	}

	// Set localnetBridgeNextHop with an IP address.
	_, _, err = util.RunIP("addr", "add",
		localnetGatewayNextHopSubnet,
		"dev", localnetBridgeNextHop)
	if err != nil {
		return fmt.Errorf("failed to assign ip address to %s (%v)",
			localnetBridgeNextHop, err)
	}

	err = util.GatewayInit(clusterIPSubnet, nodeName,
		localnetGatewayIP, "", localnetBridgeName, localnetGatewayNextHop,
		subnet, 0, nodePortEnable)
	if err != nil {
		return fmt.Errorf("failed to localnet gateway: %v", err)
	}

	err = localnetGatewayNAT(ipt, localnetBridgeNextHop, localnetGatewayIP)
	if err != nil {
		return fmt.Errorf("Failed to add NAT rules for localnet gateway (%v)",
			err)
	}

	return nil
}
