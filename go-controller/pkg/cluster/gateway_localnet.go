// +build linux

package cluster

import (
	"fmt"
	"net"
	"reflect"
	"strings"

	"k8s.io/client-go/tools/cache"

	"github.com/coreos/go-iptables/iptables"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/sirupsen/logrus"

	kapi "k8s.io/api/core/v1"
)

const (
	localnetGatewayIP            = "169.254.33.2/24"
	localnetGatewayNextHop       = "169.254.33.1"
	localnetGatewayNextHopSubnet = "169.254.33.1/24"
	iptableNodePortChain         = "OVN-KUBE-NODEPORT"
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

func addIptRules(ipt util.IPTablesHelper, rules []iptRule) error {
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

func delIptRules(ipt util.IPTablesHelper, rules []iptRule) {
	for _, r := range rules {
		err := ipt.Delete(r.table, r.chain, r.args...)
		if err != nil {
			logrus.Warningf("failed to delete iptables %s/%s rule %q: %v", r.table, r.chain,
				strings.Join(r.args, " "), err)
		}
	}
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
	return addIptRules(ipt, rules)
}

func initLocalnetGateway(nodeName string,
	subnet string, wf *factory.WatchFactory) (map[string]string, error) {
	ipt, err := util.GetIPTablesHelper(iptables.ProtocolIPv4)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize iptables: %v", err)
	}

	// Create a localnet OVS bridge.
	localnetBridgeName := "br-local"
	_, stderr, err := util.RunOVSVsctl("--may-exist", "add-br",
		localnetBridgeName)
	if err != nil {
		return nil, fmt.Errorf("Failed to create localnet bridge %s"+
			", stderr:%s (%v)", localnetBridgeName, stderr, err)
	}

	_, _, err = util.RunIP("link", "set", localnetBridgeName, "up")
	if err != nil {
		return nil, fmt.Errorf("failed to up %s (%v)", localnetBridgeName, err)
	}

	// Create a localnet bridge nexthop
	localnetBridgeNextHop := "br-nexthop"
	_, stderr, err = util.RunOVSVsctl("--may-exist", "add-port",
		localnetBridgeName, localnetBridgeNextHop, "--", "set",
		"interface", localnetBridgeNextHop, "type=internal")
	if err != nil {
		return nil, fmt.Errorf("Failed to create localnet bridge next hop %s"+
			", stderr:%s (%v)", localnetBridgeNextHop, stderr, err)
	}
	_, _, err = util.RunIP("link", "set", localnetBridgeNextHop, "up")
	if err != nil {
		return nil, fmt.Errorf("failed to up %s (%v)", localnetBridgeNextHop, err)
	}

	// Flush IPv4 address of localnetBridgeNextHop.
	_, _, err = util.RunIP("addr", "flush", "dev", localnetBridgeNextHop)
	if err != nil {
		return nil, fmt.Errorf("failed to flush ip address of %s (%v)",
			localnetBridgeNextHop, err)
	}

	// Set localnetBridgeNextHop with an IP address.
	_, _, err = util.RunIP("addr", "add",
		localnetGatewayNextHopSubnet,
		"dev", localnetBridgeNextHop)
	if err != nil {
		return nil, fmt.Errorf("failed to assign ip address to %s (%v)",
			localnetBridgeNextHop, err)
	}

	ifaceID, macAddress, err := bridgedGatewayNodeSetup(nodeName, localnetBridgeName, localnetBridgeName, true)
	if err != nil {
		return nil, fmt.Errorf("failed to set up shared interface gateway: %v", err)
	}

	annotations := map[string]string{
		ovn.OvnNodeGatewayMode:       string(config.Gateway.Mode),
		ovn.OvnNodeGatewayVlanID:     string(config.Gateway.VLANID),
		ovn.OvnNodeGatewayIfaceID:    ifaceID,
		ovn.OvnNodeGatewayMacAddress: macAddress,
		ovn.OvnNodeGatewayIP:         localnetGatewayIP,
		ovn.OvnNodeGatewayNextHop:    localnetGatewayNextHop,
	}

	err = localnetGatewayNAT(ipt, localnetBridgeNextHop, localnetGatewayIP)
	if err != nil {
		return nil, fmt.Errorf("Failed to add NAT rules for localnet gateway (%v)",
			err)
	}

	if config.Gateway.NodeportEnable {
		err = localnetNodePortWatcher(ipt, wf)
	}

	return annotations, err
}

func localnetIptRules(svc *kapi.Service) []iptRule {
	rules := make([]iptRule, 0)
	for _, svcPort := range svc.Spec.Ports {
		protocol := svcPort.Protocol
		if protocol != kapi.ProtocolUDP && protocol != kapi.ProtocolTCP {
			protocol = kapi.ProtocolTCP
		}

		nodePort := fmt.Sprintf("%d", svcPort.NodePort)
		rules = append(rules, iptRule{
			table: "nat",
			chain: iptableNodePortChain,
			args: []string{"-p", string(protocol), "--dport", nodePort, "-j", "DNAT", "--to-destination",
				net.JoinHostPort(strings.Split(localnetGatewayIP, "/")[0], nodePort)},
		})
		rules = append(rules, iptRule{
			table: "filter",
			chain: iptableNodePortChain,
			args:  []string{"-p", string(protocol), "--dport", nodePort, "-j", "ACCEPT"},
		})
	}
	return rules
}

// AddService adds service and creates corresponding resources in OVN
func localnetAddService(svc *kapi.Service) error {
	if !util.ServiceTypeHasNodePort(svc) {
		return nil
	}
	ipt, err := util.GetIPTablesHelper(iptables.ProtocolIPv4)
	if err != nil {
		return fmt.Errorf("failed to initialize iptables: %v", err)
	}
	rules := localnetIptRules(svc)
	logrus.Debugf("Add rules %v for service %v", rules, svc.Name)
	return addIptRules(ipt, rules)
}

func localnetDeleteService(svc *kapi.Service) error {
	if !util.ServiceTypeHasNodePort(svc) {
		return nil
	}
	ipt, err := util.GetIPTablesHelper(iptables.ProtocolIPv4)
	if err != nil {
		return fmt.Errorf("failed to initialize iptables: %v", err)
	}
	rules := localnetIptRules(svc)
	logrus.Debugf("Delete rules %v for service %v", rules, svc.Name)
	delIptRules(ipt, rules)
	return nil
}

func localnetNodePortWatcher(ipt util.IPTablesHelper, wf *factory.WatchFactory) error {
	// delete all the existing OVN-NODEPORT rules
	// TODO: Add a localnetSyncService method to remove the stale entries only
	_ = ipt.ClearChain("nat", iptableNodePortChain)
	_ = ipt.ClearChain("filter", iptableNodePortChain)

	rules := make([]iptRule, 0)
	rules = append(rules, iptRule{
		table: "nat",
		chain: "PREROUTING",
		args:  []string{"-j", iptableNodePortChain},
	})
	rules = append(rules, iptRule{
		table: "nat",
		chain: "OUTPUT",
		args:  []string{"-j", iptableNodePortChain},
	})
	rules = append(rules, iptRule{
		table: "filter",
		chain: "FORWARD",
		args:  []string{"-j", iptableNodePortChain},
	})

	if err := addIptRules(ipt, rules); err != nil {
		return err
	}

	_, err := wf.AddServiceHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			svc := obj.(*kapi.Service)
			err := localnetAddService(svc)
			if err != nil {
				logrus.Errorf("Error in adding service: %v", err)
			}
		},
		UpdateFunc: func(old, new interface{}) {
			svcNew := new.(*kapi.Service)
			svcOld := old.(*kapi.Service)
			if reflect.DeepEqual(svcNew.Spec, svcOld.Spec) {
				return
			}
			err := localnetDeleteService(svcOld)
			if err != nil {
				logrus.Errorf("Error in deleting service - %v", err)
			}
			err = localnetAddService(svcNew)
			if err != nil {
				logrus.Errorf("Error in modifying service: %v", err)
			}
		},
		DeleteFunc: func(obj interface{}) {
			svc := obj.(*kapi.Service)
			err := localnetDeleteService(svc)
			if err != nil {
				logrus.Errorf("Error in deleting service - %v", err)
			}
		},
	}, nil)
	return err
}

// cleanupLocalnetGateway cleans up Localnet Gateway
func cleanupLocalnetGateway() error {
	// get bridgeName from ovn-bridge-mappings.
	stdout, stderr, err := util.RunOVSVsctl("--if-exists", "get", "Open_vSwitch", ".",
		"external_ids:ovn-bridge-mappings")
	if err != nil {
		return fmt.Errorf("Failed to get ovn-bridge-mappings stderr:%s (%v)", stderr, err)
	}
	bridgeName := strings.Split(stdout, ":")[1]
	_, stderr, err = util.RunOVSVsctl("--", "--if-exists", "del-br", bridgeName)
	if err != nil {
		return fmt.Errorf("Failed to ovs-vsctl del-br %s stderr:%s (%v)", bridgeName, stderr, err)
	}
	return err
}
