// +build linux

package node

import (
	"fmt"
	"net"
	"reflect"
	"strings"

	"github.com/coreos/go-iptables/iptables"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"k8s.io/klog"

	kapi "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/cache"
	utilnet "k8s.io/utils/net"
)

const (
	v4localnetGatewayIP           = "169.254.33.2"
	v4localnetGatewayNextHop      = "169.254.33.1"
	v4localnetGatewaySubnetPrefix = "/24"

	v6localnetGatewayIP           = "fd99::2"
	v6localnetGatewayNextHop      = "fd99::1"
	v6localnetGatewaySubnetPrefix = "/64"

	// localnetGatewayNextHopPort is the name of the gateway port on the host to which all
	// the packets leaving the OVN logical topology will be forwarded
	localnetGatewayNextHopPort       = "ovn-k8s-gw0"
	legacyLocalnetGatewayNextHopPort = "br-nexthop"
	// fixed MAC address for the br-nexthop interface. the last 4 hex bytes
	// translates to the br-nexthop's IP address
	localnetGatewayNextHopMac = "00:00:a9:fe:21:01"
	iptableNodePortChain      = "OVN-KUBE-NODEPORT"
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
			klog.Warningf("failed to delete iptables %s/%s rule %q: %v", r.table, r.chain,
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

func initLocalnetGateway(nodeName string, subnet string, wf *factory.WatchFactory, nodeAnnotator kube.Annotator) error {
	// Create a localnet OVS bridge.
	localnetBridgeName := "br-local"
	_, stderr, err := util.RunOVSVsctl("--may-exist", "add-br",
		localnetBridgeName)
	if err != nil {
		return fmt.Errorf("Failed to create localnet bridge %s"+
			", stderr:%s (%v)", localnetBridgeName, stderr, err)
	}

	ifaceID, macAddress, err := bridgedGatewayNodeSetup(nodeName, localnetBridgeName, localnetBridgeName, true)
	if err != nil {
		return fmt.Errorf("failed to set up shared interface gateway: %v", err)
	}
	_, err = util.LinkSetUp(localnetBridgeName)
	if err != nil {
		return err
	}

	// Create a localnet bridge gateway port
	_, stderr, err = util.RunOVSVsctl(
		"--if-exists", "del-port", localnetBridgeName, legacyLocalnetGatewayNextHopPort,
		"--", "--may-exist", "add-port", localnetBridgeName, localnetGatewayNextHopPort,
		"--", "set", "interface", localnetGatewayNextHopPort, "type=internal",
		"mtu_request="+fmt.Sprintf("%d", config.Default.MTU),
		fmt.Sprintf("mac=%s", strings.ReplaceAll(localnetGatewayNextHopMac, ":", "\\:")))
	if err != nil {
		return fmt.Errorf("Failed to create localnet bridge gateway port %s"+
			", stderr:%s (%v)", localnetGatewayNextHopPort, stderr, err)
	}
	link, err := util.LinkSetUp(localnetGatewayNextHopPort)
	if err != nil {
		return err
	}

	var gatewayIP, gatewayNextHop, gatewaySubnetPrefix string
	if utilnet.IsIPv6CIDRString(subnet) {
		gatewayIP = v6localnetGatewayIP
		gatewayNextHop = v6localnetGatewayNextHop
		gatewaySubnetPrefix = v6localnetGatewaySubnetPrefix
	} else {
		gatewayIP = v4localnetGatewayIP
		gatewayNextHop = v4localnetGatewayNextHop
		gatewaySubnetPrefix = v4localnetGatewaySubnetPrefix
	}

	// Flush any addresses on localnetBridgeNextHopPort and add the new IP address.
	if err = util.LinkAddrFlush(link); err == nil {
		err = util.LinkAddrAdd(link, gatewayNextHop+gatewaySubnetPrefix)
	}
	if err != nil {
		return err
	}

	err = util.SetLocalL3GatewayConfig(nodeAnnotator, ifaceID, macAddress,
		gatewayIP+gatewaySubnetPrefix, gatewayNextHop,
		config.Gateway.NodeportEnable)
	if err != nil {
		return err
	}

	if utilnet.IsIPv6CIDRString(subnet) {
		// TODO - IPv6 hack ... for some reason neighbor discovery isn't working here, so hard code a
		// MAC binding for the gateway IP address for now - need to debug this further
		err = util.LinkNeighAdd(link, gatewayIP, macAddress)
		if err == nil {
			klog.Infof("Added MAC binding for %s on %s", gatewayIP, localnetGatewayNextHopPort)
		} else {
			klog.Errorf("Error in adding MAC binding for %s on %s: %v", gatewayIP, localnetGatewayNextHopPort, err)
		}
	}

	ipt, err := localnetIPTablesHelper(subnet)
	if err != nil {
		return err
	}

	err = localnetGatewayNAT(ipt, localnetGatewayNextHopPort, gatewayIP)
	if err != nil {
		return fmt.Errorf("Failed to add NAT rules for localnet gateway (%v)", err)
	}

	if config.Gateway.NodeportEnable {
		err = localnetNodePortWatcher(ipt, wf, gatewayIP)
	}

	return err
}

// localnetIPTablesHelper gets an IPTablesHelper for IPv4 or IPv6 as appropriate
func localnetIPTablesHelper(subnet string) (util.IPTablesHelper, error) {
	var ipt util.IPTablesHelper
	var err error
	if utilnet.IsIPv6CIDRString(subnet) {
		ipt, err = util.GetIPTablesHelper(iptables.ProtocolIPv6)
	} else {
		ipt, err = util.GetIPTablesHelper(iptables.ProtocolIPv4)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to initialize iptables: %v", err)
	}
	return ipt, nil
}

func localnetIptRules(svc *kapi.Service, gatewayIP string) []iptRule {
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
			args: []string{
				"-p", string(protocol), "--dport", nodePort,
				"-j", "DNAT", "--to-destination", net.JoinHostPort(gatewayIP, nodePort),
			},
		})
		rules = append(rules, iptRule{
			table: "filter",
			chain: iptableNodePortChain,
			args: []string{
				"-p", string(protocol), "--dport", nodePort,
				"-j", "ACCEPT",
			},
		})
	}
	return rules
}

type localnetNodePortWatcherData struct {
	ipt       util.IPTablesHelper
	gatewayIP string
}

func (npw *localnetNodePortWatcherData) addService(svc *kapi.Service) error {
	if !util.ServiceTypeHasNodePort(svc) {
		return nil
	}
	rules := localnetIptRules(svc, npw.gatewayIP)
	klog.V(5).Infof("Add rules %v for service %v", rules, svc.Name)
	return addIptRules(npw.ipt, rules)
}

func (npw *localnetNodePortWatcherData) deleteService(svc *kapi.Service) error {
	if !util.ServiceTypeHasNodePort(svc) {
		return nil
	}
	rules := localnetIptRules(svc, npw.gatewayIP)
	klog.V(5).Infof("Delete rules %v for service %v", rules, svc.Name)
	delIptRules(npw.ipt, rules)
	return nil
}

func localnetNodePortWatcher(ipt util.IPTablesHelper, wf *factory.WatchFactory, gatewayIP string) error {
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

	npw := &localnetNodePortWatcherData{ipt: ipt, gatewayIP: gatewayIP}
	_, err := wf.AddServiceHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			svc := obj.(*kapi.Service)
			err := npw.addService(svc)
			if err != nil {
				klog.Errorf("Error in adding service: %v", err)
			}
		},
		UpdateFunc: func(old, new interface{}) {
			svcNew := new.(*kapi.Service)
			svcOld := old.(*kapi.Service)
			if reflect.DeepEqual(svcNew.Spec, svcOld.Spec) {
				return
			}
			err := npw.deleteService(svcOld)
			if err != nil {
				klog.Errorf("Error in deleting service - %v", err)
			}
			err = npw.addService(svcNew)
			if err != nil {
				klog.Errorf("Error in modifying service: %v", err)
			}
		},
		DeleteFunc: func(obj interface{}) {
			svc := obj.(*kapi.Service)
			err := npw.deleteService(svc)
			if err != nil {
				klog.Errorf("Error in deleting service - %v", err)
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
