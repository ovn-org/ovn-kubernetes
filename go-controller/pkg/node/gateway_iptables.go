// +build linux

package node

import (
	"fmt"
	"net"
	"strings"

	"github.com/coreos/go-iptables/iptables"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	egressipv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	kapi "k8s.io/api/core/v1"
	"k8s.io/klog"
	utilnet "k8s.io/utils/net"
)

const (
	iptableNodePortChain   = "OVN-KUBE-NODEPORT"
	iptableExternalIPChain = "OVN-KUBE-EXTERNALIP"
	iptableEgressIPChain   = "OVN-KUBE-EGRESSIP"
)

func clusterIPTablesProtocols() []iptables.Protocol {
	var protocols []iptables.Protocol
	if config.IPv4Mode {
		protocols = append(protocols, iptables.ProtocolIPv4)
	}
	if config.IPv6Mode {
		protocols = append(protocols, iptables.ProtocolIPv6)
	}
	return protocols
}

type iptRule struct {
	table    string
	chain    string
	args     []string
	protocol iptables.Protocol
}

func addIptRules(rules []iptRule) error {
	for _, r := range rules {
		klog.V(5).Infof("Adding rule in table: %s, chain: %s with args: \"%s\" for protocol: %v ", r.table, r.chain, strings.Join(r.args, " "), r.protocol)
		ipt, _ := util.GetIPTablesHelper(r.protocol)
		if err := ipt.NewChain(r.table, r.chain); err != nil {
			klog.V(5).Infof("Chain: \"%s\" in table: \"%s\" already exists, skipping creation", r.table, r.chain)
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

func delIptRules(rules []iptRule) error {
	for _, r := range rules {
		klog.V(5).Infof("Deleting rule in table: %s, chain: %s with args: \"%s\" for protocol: %v ", r.table, r.chain, strings.Join(r.args, " "), r.protocol)
		ipt, _ := util.GetIPTablesHelper(r.protocol)
		err := ipt.Delete(r.table, r.chain, r.args...)
		if err != nil {
			return fmt.Errorf("failed to delete iptables %s/%s rule %q: %v",
				r.table, r.chain, strings.Join(r.args, " "), err)
		}
	}
	return nil
}

func getSharedGatewayInitRules(chain string, proto iptables.Protocol) []iptRule {
	return []iptRule{
		{
			table:    "nat",
			chain:    "OUTPUT",
			args:     []string{"-j", chain},
			protocol: proto,
		},
		{
			table:    "nat",
			chain:    "PREROUTING",
			args:     []string{"-j", chain},
			protocol: proto,
		},
		{
			table:    "filter",
			chain:    "OUTPUT",
			args:     []string{"-j", chain},
			protocol: proto,
		},
		{
			table:    "filter",
			chain:    "FORWARD",
			args:     []string{"-j", chain},
			protocol: proto,
		},
	}
}

func getLocalGatewayInitRules(chain string, proto iptables.Protocol) []iptRule {
	return []iptRule{
		{
			table:    "nat",
			chain:    "PREROUTING",
			args:     []string{"-j", chain},
			protocol: proto,
		},
		{
			table:    "nat",
			chain:    "OUTPUT",
			args:     []string{"-j", chain},
			protocol: proto,
		},
		{
			table:    "filter",
			chain:    "FORWARD",
			args:     []string{"-j", chain},
			protocol: proto,
		},
	}
}

func getNodePortIPTRules(svcPort kapi.ServicePort, nodeIP *net.IPNet, targetIP string, targetPort int32) []iptRule {
	var protocol iptables.Protocol
	if utilnet.IsIPv6String(targetIP) {
		protocol = iptables.ProtocolIPv6
	} else {
		protocol = iptables.ProtocolIPv4
	}
	var natArgs, filterArgs []string
	if nodeIP != nil {
		natArgs = []string{
			"-p", string(svcPort.Protocol),
			"-d", nodeIP.IP.String(),
			"--dport", fmt.Sprintf("%d", svcPort.NodePort),
			"-j", "DNAT",
			"--to-destination", util.JoinHostPortInt32(targetIP, targetPort),
		}
		filterArgs = []string{
			"-p", string(svcPort.Protocol),
			"-d", nodeIP.IP.String(),
			"--dport", fmt.Sprintf("%d", svcPort.NodePort),
			"-j", "ACCEPT",
		}
	} else {
		natArgs = []string{
			"-p", string(svcPort.Protocol),
			"--dport", fmt.Sprintf("%d", svcPort.NodePort),
			"-j", "DNAT",
			"--to-destination", util.JoinHostPortInt32(targetIP, targetPort),
		}
		filterArgs = []string{
			"-p", string(svcPort.Protocol),
			"--dport", fmt.Sprintf("%d", svcPort.NodePort),
			"-j", "ACCEPT",
		}
	}
	return []iptRule{
		{
			table:    "nat",
			chain:    iptableNodePortChain,
			args:     natArgs,
			protocol: protocol,
		},
		{
			table:    "filter",
			chain:    iptableNodePortChain,
			args:     filterArgs,
			protocol: protocol,
		},
	}
}

func getHybridNodePortIPTRules(svcPort kapi.ServicePort, nodeIP *net.IPNet, targetIP string) []iptRule {
	var protocol iptables.Protocol
	if utilnet.IsIPv6String(targetIP) {
		protocol = iptables.ProtocolIPv6
	} else {
		protocol = iptables.ProtocolIPv4
	}

	natArgs := []string{
		"-p", string(svcPort.Protocol),
		"-s", nodeIP.IP.String(),
		"--dport", fmt.Sprintf("%d", svcPort.NodePort),
		"-j", "DNAT",
		"--to-destination", util.JoinHostPortInt32(targetIP, svcPort.Port),
	}
	return []iptRule{
		{
			table:    "nat",
			chain:    iptableNodePortChain,
			args:     natArgs,
			protocol: protocol,
		},
	}
}

func getEgressIPTRules(eIPStatus egressipv1.EgressIPStatusItem, gatewayRouterIP string) []iptRule {
	var protocol iptables.Protocol
	if utilnet.IsIPv6String(eIPStatus.EgressIP) {
		protocol = iptables.ProtocolIPv6
	} else {
		protocol = iptables.ProtocolIPv4
	}
	return []iptRule{
		{
			table: "nat",
			chain: iptableEgressIPChain,
			args: []string{
				"-s", gatewayRouterIP,
				"-m", "mark",
				"--mark", fmt.Sprintf("0x%x", util.IPToUint32(eIPStatus.EgressIP)),
				"-j", "SNAT",
				"--to-source", eIPStatus.EgressIP,
			},
			protocol: protocol,
		},
		{
			table: "filter",
			chain: iptableEgressIPChain,
			args: []string{
				"-d", eIPStatus.EgressIP,
				"-m", "conntrack",
				"--ctstate", "NEW",
				"-j", "REJECT",
			},
			protocol: protocol,
		},
	}
}

func getExternalIPTRules(svcPort kapi.ServicePort, externalIP, dstIP string) []iptRule {
	var protocol iptables.Protocol
	if utilnet.IsIPv6String(externalIP) {
		protocol = iptables.ProtocolIPv6
	} else {
		protocol = iptables.ProtocolIPv4
	}
	return []iptRule{
		{
			table: "nat",
			chain: iptableExternalIPChain,
			args: []string{
				"-p", string(svcPort.Protocol),
				"-d", externalIP,
				"--dport", fmt.Sprintf("%v", svcPort.Port),
				"-j", "DNAT",
				"--to-destination", util.JoinHostPortInt32(dstIP, svcPort.Port),
			},
			protocol: protocol,
		},
		{
			table: "filter",
			chain: iptableExternalIPChain,
			args: []string{
				"-p", string(svcPort.Protocol),
				"-d", externalIP,
				"--dport", fmt.Sprintf("%v", svcPort.Port),
				"-j", "ACCEPT",
			},
			protocol: protocol,
		},
	}
}

func getLocalGatewayNATRules(ifname string, ip net.IP) []iptRule {
	// Allow packets to/from the gateway interface in case defaults deny
	var protocol iptables.Protocol
	if utilnet.IsIPv6(ip) {
		protocol = iptables.ProtocolIPv6
	} else {
		protocol = iptables.ProtocolIPv4
	}
	return []iptRule{
		{
			table: "filter",
			chain: "FORWARD",
			args: []string{
				"-i", ifname,
				"-j", "ACCEPT",
			},
			protocol: protocol,
		},
		{
			table: "filter",
			chain: "FORWARD",
			args: []string{
				"-o", ifname,
				"-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED",
				"-j", "ACCEPT",
			},
			protocol: protocol,
		},
		{
			table: "filter",
			chain: "INPUT",
			args: []string{
				"-i", ifname,
				"-m", "comment", "--comment", "from OVN to localhost",
				"-j", "ACCEPT",
			},
			protocol: protocol,
		},
		{
			table: "nat",
			chain: "POSTROUTING",
			args: []string{
				"-s", ip.String(),
				"-j", "MASQUERADE",
			},
			protocol: protocol,
		},
	}
}

func initLocalGatewayNATRules(ifname string, ip net.IP) error {
	return addIptRules(getLocalGatewayNATRules(ifname, ip))
}

func initGatewayIPTables(genGatewayChainRules func(chain string, proto iptables.Protocol) []iptRule) error {
	rules := make([]iptRule, 0)
	for _, chain := range []string{iptableNodePortChain, iptableExternalIPChain} {
		for _, proto := range clusterIPTablesProtocols() {
			ipt, err := util.GetIPTablesHelper(proto)
			if err != nil {
				return err
			}
			if err := ipt.NewChain("nat", chain); err != nil {
				klog.V(5).Infof("Chain: \"%s\" in table: \"%s\" already exists, skipping creation", "nat", chain)
			}
			if err := ipt.NewChain("filter", chain); err != nil {
				klog.V(5).Infof("Chain: \"%s\" in table: \"%s\" already exists, skipping creation", "filter", chain)
			}
			rules = append(rules, genGatewayChainRules(chain, proto)...)
		}
	}
	if err := addIptRules(rules); err != nil {
		return fmt.Errorf("failed to add iptables rules %v: %v", rules, err)
	}
	return nil
}

func initSharedGatewayIPTables() error {
	if err := initGatewayIPTables(getSharedGatewayInitRules); err != nil {
		return err
	}
	return nil
}

func initLocalGatewayIPTables() error {
	if err := initGatewayIPTables(getLocalGatewayInitRules); err != nil {
		return err
	}
	return nil
}

func cleanupSharedGatewayIPTChains() {
	for _, chain := range []string{iptableNodePortChain, iptableExternalIPChain} {
		// We clean up both IPv4 and IPv6, regardless of what is currently in use
		for _, proto := range []iptables.Protocol{iptables.ProtocolIPv4, iptables.ProtocolIPv6} {
			ipt, err := util.GetIPTablesHelper(proto)
			if err != nil {
				return
			}
			_ = ipt.ClearChain("nat", chain)
			_ = ipt.ClearChain("filter", chain)
			_ = ipt.DeleteChain("nat", chain)
			_ = ipt.DeleteChain("filter", chain)
		}
	}
}

func recreateIPTRules(table, chain string, keepIPTRules []iptRule) {
	for _, proto := range clusterIPTablesProtocols() {
		ipt, _ := util.GetIPTablesHelper(proto)
		if err := ipt.ClearChain(table, chain); err != nil {
			klog.Errorf("Error clearing chain: %s in table: %s, err: %v", chain, table, err)
		}
	}
	if err := addIptRules(keepIPTRules); err != nil {
		klog.Error(err)
	}
}

// getGatewayIPTRules returns NodePort and ExternalIP iptables rules for service. If nodeIP is non-nil, then
// only incoming traffic on that IP will be accepted for NodePort rules; otherwise incoming traffic on the NodePort
// on all IPs will be accepted. If gatewayIP is "", then NodePort traffic will be DNAT'ed to the service port on
// the service's ClusterIP. Otherwise, it will be DNAT'ed to the NodePort on the gatewayIP.
func getGatewayIPTRules(service *kapi.Service, gatewayIP string, nodeIP *net.IPNet) []iptRule {
	rules := make([]iptRule, 0)
	for _, svcPort := range service.Spec.Ports {
		if util.ServiceTypeHasNodePort(service) {
			err := util.ValidatePort(svcPort.Protocol, svcPort.NodePort)
			if err != nil {
				klog.Errorf("Skipping service: %s, invalid service NodePort: %v", svcPort.Name, err)
				continue
			}
			err = util.ValidatePort(svcPort.Protocol, svcPort.Port)
			if err != nil {
				klog.Errorf("Skipping service: %s, invalid service port %v", svcPort.Name, err)
				continue
			}
			if gatewayIP == "" {
				rules = append(rules, getNodePortIPTRules(svcPort, nodeIP, service.Spec.ClusterIP, svcPort.Port)...)
			} else {
				rules = append(rules, getNodePortIPTRules(svcPort, nodeIP, gatewayIP, svcPort.NodePort)...)
			}
		}
		for _, externalIP := range service.Spec.ExternalIPs {
			err := util.ValidatePort(svcPort.Protocol, svcPort.Port)
			if err != nil {
				klog.Errorf("Skipping service: %s, invalid service port %v", svcPort.Name, err)
				continue
			}
			rules = append(rules, getExternalIPTRules(svcPort, externalIP, service.Spec.ClusterIP)...)
		}
	}
	return rules
}
