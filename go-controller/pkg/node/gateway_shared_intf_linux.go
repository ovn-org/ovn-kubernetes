package node

import (
	"fmt"
	"net"

	"github.com/coreos/go-iptables/iptables"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	kapi "k8s.io/api/core/v1"
	"k8s.io/klog"
	utilnet "k8s.io/utils/net"
)

func createNodePortIptableChain() error {
	for _, proto := range []iptables.Protocol{iptables.ProtocolIPv4, iptables.ProtocolIPv6} {
		ipt, err := util.GetIPTablesHelper(proto)
		if err != nil {
			return err
		}
		// delete all the existing OVN-KUBE-NODEPORT rules
		_ = ipt.ClearChain("nat", iptableNodePortChain)
		_ = ipt.ClearChain("filter", iptableNodePortChain)

		rules := make([]iptRule, 0)
		rules = append(rules, iptRule{
			table: "nat",
			chain: "OUTPUT",
			args:  []string{"-j", iptableNodePortChain},
		})
		rules = append(rules, iptRule{
			table: "nat",
			chain: "PREROUTING",
			args:  []string{"-j", iptableNodePortChain},
		})
		rules = append(rules, iptRule{
			table: "filter",
			chain: "OUTPUT",
			args:  []string{"-j", iptableNodePortChain},
		})
		rules = append(rules, iptRule{
			table: "filter",
			chain: "FORWARD",
			args:  []string{"-j", iptableNodePortChain},
		})

		if err := addIptRules(ipt, rules); err != nil {
			return fmt.Errorf("failed to add iptable rules %v: %v", rules, err)
		}
	}
	return nil
}

func deleteNodePortIptableChain() {
	for _, proto := range []iptables.Protocol{iptables.ProtocolIPv4, iptables.ProtocolIPv6} {
		ipt, err := util.GetIPTablesHelper(proto)
		if err != nil {
			return
		}
		// delete all the existing OVN-NODEPORT rules
		_ = ipt.ClearChain("nat", iptableNodePortChain)
		_ = ipt.ClearChain("filter", iptableNodePortChain)
		_ = ipt.DeleteChain("nat", iptableNodePortChain)
		_ = ipt.DeleteChain("filter", iptableNodePortChain)
	}
}

func getSharedGatewayIptRules(service *kapi.Service, nodeIP *net.IPNet) []iptRule {
	rules := make([]iptRule, 0)

	for _, svcPort := range service.Spec.Ports {
		protocol, err := util.ValidateProtocol(svcPort.Protocol)
		if err != nil {
			klog.Errorf("Skipping service add. Invalid service port %s: %v", svcPort.Name, err)
			continue
		}
		nodePort := fmt.Sprintf("%d", svcPort.NodePort)
		port := fmt.Sprintf("%d", svcPort.Port)

		rules = append(rules, iptRule{
			table: "nat",
			chain: iptableNodePortChain,
			args: []string{
				"-p", string(protocol), "--dport", nodePort, "-d", nodeIP.IP.String(),
				"-j", "DNAT", "--to-destination", net.JoinHostPort(service.Spec.ClusterIP, port),
			},
		})
		rules = append(rules, iptRule{
			table: "filter",
			chain: iptableNodePortChain,
			args: []string{
				"-p", string(protocol), "--dport", nodePort, "-d", nodeIP.IP.String(),
				"-j", "ACCEPT",
			},
		})
	}
	return rules
}

func addSharedGatewayIptRules(service *kapi.Service, nodeIP *net.IPNet) {
	var ipt util.IPTablesHelper

	rules := getSharedGatewayIptRules(service, nodeIP)
	// we've already checked/created iptableHelper in initNodePortIptableChain, no need to check error here.
	if utilnet.IsIPv6String(service.Spec.ClusterIP) {
		ipt, _ = util.GetIPTablesHelper(iptables.ProtocolIPv6)
	} else {
		ipt, _ = util.GetIPTablesHelper(iptables.ProtocolIPv4)
	}
	if err := addIptRules(ipt, rules); err != nil {
		klog.Errorf("Failed to set up iptables rules for nodePort service %s/%s: %v",
			service.Namespace, service.Name, err)
	}
}

func delSharedGatewayIptRules(service *kapi.Service, nodeIP *net.IPNet) {
	var ipt util.IPTablesHelper

	rules := getSharedGatewayIptRules(service, nodeIP)
	// we've already checked/created iptableHelper in initNodePortIptableChain, no need to check error here.
	if utilnet.IsIPv6String(service.Spec.ClusterIP) {
		ipt, _ = util.GetIPTablesHelper(iptables.ProtocolIPv6)
	} else {
		ipt, _ = util.GetIPTablesHelper(iptables.ProtocolIPv4)
	}
	delIptRules(ipt, rules)
}
