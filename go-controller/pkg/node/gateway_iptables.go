//go:build linux
// +build linux

package node

import (
	"fmt"
	"net"

	"github.com/coreos/go-iptables/iptables"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/controllers/egressservice"
	nodeipt "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/iptables"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	kapi "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
)

const (
	iptableNodePortChain    = "OVN-KUBE-NODEPORT"     // called from nat-PREROUTING and nat-OUTPUT
	iptableExternalIPChain  = "OVN-KUBE-EXTERNALIP"   // called from nat-PREROUTING and nat-OUTPUT
	iptableETPChain         = "OVN-KUBE-ETP"          // called from nat-PREROUTING only
	iptableITPChain         = "OVN-KUBE-ITP"          // called from mangle-OUTPUT and nat-OUTPUT
	iptableForwardDropChain = "OVN-KUBE-FORWARD-DROP" // Called from FORWARD.
	iptableFilterTable      = "filter"
	iptableForwardChain     = "FORWARD"
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

// getIPTablesProtocol returns the IPTables protocol matching the protocol (v4/v6) of provided IP string
func getIPTablesProtocol(ip string) iptables.Protocol {
	if utilnet.IsIPv6String(ip) {
		return iptables.ProtocolIPv6
	}
	return iptables.ProtocolIPv4
}

// getMasqueradeVIP returns the .3 masquerade VIP based on the protocol (v4/v6) of provided IP string
func getMasqueradeVIP(ip string) string {
	if utilnet.IsIPv6String(ip) {
		return config.Gateway.MasqueradeIPs.V6HostETPLocalMasqueradeIP.String()
	}
	return config.Gateway.MasqueradeIPs.V4HostETPLocalMasqueradeIP.String()
}

// insertIptRules adds the provided rules in an insert fashion
// i.e each rule gets added at the first position in the chain
func insertIptRules(rules []nodeipt.Rule) error {
	return nodeipt.AddRules(rules, false)
}

// appendIptRules adds the provided rules in an append fashion
// i.e each rule gets added at the last position in the chain
func appendIptRules(rules []nodeipt.Rule) error {
	return nodeipt.AddRules(rules, true)
}

// deleteIptRules deletes the provided rules.
func deleteIptRules(rules []nodeipt.Rule) error {
	return nodeipt.DelRules(rules)
}

// listIptRules lists rules for the given tuple of protocol, table and chain.
func listIptRules(protocol iptables.Protocol, table, chain string) ([]nodeipt.Rule, error) {
	return nodeipt.ListRules(protocol, table, chain)
}

func getGatewayInitRules(chain string, proto iptables.Protocol) []nodeipt.Rule {
	iptRules := []nodeipt.Rule{}
	if chain == egressservice.Chain {
		return []nodeipt.Rule{
			{
				Table:    "nat",
				Chain:    "POSTROUTING",
				Args:     []string{"-j", chain},
				Protocol: proto,
			},
		}
	}
	if chain == iptableITPChain {
		iptRules = append(iptRules,
			nodeipt.Rule{
				Table:    "mangle",
				Chain:    "OUTPUT",
				Args:     []string{"-j", chain},
				Protocol: proto,
			},
		)
	} else {
		iptRules = append(iptRules,
			nodeipt.Rule{
				Table:    "nat",
				Chain:    "PREROUTING",
				Args:     []string{"-j", chain},
				Protocol: proto,
			},
		)
	}
	if chain != iptableETPChain { // ETP chain only meant for external traffic
		iptRules = append(iptRules,
			nodeipt.Rule{
				Table:    "nat",
				Chain:    "OUTPUT",
				Args:     []string{"-j", chain},
				Protocol: proto,
			},
		)
	}
	return iptRules
}

// getNodePortIPTRules returns the IPTable DNAT rules for a service of type nodePort
// `svcPort` corresponds to port details for this service as specified in the service object
// `targetIP` is clusterIP towards which the DNAT of nodePort service is to be added
// `targetPort` is the port towards which the DNAT of the nodePort service is to be added
//
//	case1: if svcHasLocalHostNetEndPnt=false + isETPLocal=true targetIP=config.masqueradeIP["HostETPLocalMasqueradeIP"] and targetPort=svcPort.NodePort
//	case2: default: targetIP=clusterIP and targetPort=svcPort.Port
//
// `svcHasLocalHostNetEndPnt` is true if this service has at least one host-networked endpoint that is local to this node
// `isETPLocal` is true if the svc.Spec.ExternalTrafficPolicy=Local
func getNodePortIPTRules(svcPort kapi.ServicePort, targetIP string, targetPort int32, svcHasLocalHostNetEndPnt, isETPLocal bool) []nodeipt.Rule {
	chainName := iptableNodePortChain
	if !svcHasLocalHostNetEndPnt && isETPLocal {
		// DNAT it to the masqueradeIP:nodePort instead of clusterIP:targetPort
		targetIP = getMasqueradeVIP(targetIP)
		chainName = iptableETPChain
	}
	return []nodeipt.Rule{
		{
			Table: "nat",
			Chain: chainName,
			Args: []string{
				"-p", string(svcPort.Protocol),
				"-m", "addrtype",
				"--dst-type", "LOCAL",
				"--dport", fmt.Sprintf("%d", svcPort.NodePort),
				"-j", "DNAT",
				"--to-destination", util.JoinHostPortInt32(targetIP, targetPort),
			},
			Protocol: getIPTablesProtocol(targetIP),
		},
	}
}

// getITPLocalIPTRules returns the IPTable REDIRECT or MARK rules for the provided service
// `svcPort` corresponds to port details for this service as specified in the service object
// `clusterIP` is clusterIP is the VIP of the service to match on
// `svcHasLocalHostNetEndPnt` is true if this service has at least one host-networked endpoint that is local to this node
// NOTE: Currently invoked only for Internal Traffic Policy
func getITPLocalIPTRules(svcPort kapi.ServicePort, clusterIP string, svcHasLocalHostNetEndPnt bool) []nodeipt.Rule {
	if svcHasLocalHostNetEndPnt {
		return []nodeipt.Rule{
			{
				Table: "nat",
				Chain: iptableITPChain,
				Args: []string{
					"-p", string(svcPort.Protocol),
					"-d", clusterIP,
					"--dport", fmt.Sprintf("%v", svcPort.Port),
					"-j", "REDIRECT",
					"--to-port", fmt.Sprintf("%v", int32(svcPort.TargetPort.IntValue())),
				},
				Protocol: getIPTablesProtocol(clusterIP),
			},
		}
	}
	return []nodeipt.Rule{
		{
			Table: "mangle",
			Chain: iptableITPChain,
			Args: []string{
				"-p", string(svcPort.Protocol),
				"-d", string(clusterIP),
				"--dport", fmt.Sprintf("%d", svcPort.Port),
				"-j", "MARK",
				"--set-xmark", string(ovnkubeITPMark),
			},
			Protocol: getIPTablesProtocol(clusterIP),
		},
	}
}

// getNodePortETPLocalIPTRules returns the IPTable REDIRECT or RETURN rules for a service of type nodePort if ETP=local
// `svcPort` corresponds to port details for this service as specified in the service object
// `targetIP` corresponds to svc.spec.ClusterIP
// This function returns a RETURN rule in iptableMgmPortChain to prevent SNAT of sourceIP
func getNodePortETPLocalIPTRules(svcPort kapi.ServicePort, targetIP string) []nodeipt.Rule {
	return []nodeipt.Rule{
		{
			Table: "nat",
			Chain: iptableMgmPortChain,
			Args: []string{
				"-p", string(svcPort.Protocol),
				"--dport", fmt.Sprintf("%d", svcPort.NodePort),
				"-j", "RETURN",
			},
			Protocol: getIPTablesProtocol(targetIP),
		},
	}
}

func computeProbability(n, i int) string {
	return fmt.Sprintf("%0.10f", 1.0/float64(n-i+1))
}

func generateIPTRulesForLoadBalancersWithoutNodePorts(svcPort kapi.ServicePort, externalIP string, service *kapi.Service, localEndpoints []string) []nodeipt.Rule {
	var iptRules []nodeipt.Rule
	if len(localEndpoints) == 0 {
		// either its smart nic mode; etp&itp not implemented, OR
		// fetching endpointSlices error-ed out prior to reaching here so nothing to do
		return iptRules
	}
	numLocalEndpoints := len(localEndpoints)
	for i, ip := range localEndpoints {
		iptRules = append([]nodeipt.Rule{
			{
				Table: "nat",
				Chain: iptableETPChain,
				Args: []string{
					"-p", string(svcPort.Protocol),
					"-d", externalIP,
					"--dport", fmt.Sprintf("%v", svcPort.Port),
					"-j", "DNAT",
					"--to-destination", util.JoinHostPortInt32(ip, int32(svcPort.TargetPort.IntValue())),
					"-m", "statistic",
					"--mode", "random",
					"--probability", computeProbability(numLocalEndpoints, i+1),
				},
				Protocol: getIPTablesProtocol(externalIP),
			},
			{
				Table: "nat",
				Chain: iptableMgmPortChain,
				Args: []string{
					"-p", string(svcPort.Protocol),
					"-d", ip,
					"--dport", fmt.Sprintf("%v", int32(svcPort.TargetPort.IntValue())),
					"-j", "RETURN",
				},
				Protocol: getIPTablesProtocol(externalIP),
			},
		}, iptRules...)
	}
	return iptRules
}

// getExternalIPTRules returns the IPTable DNAT rules for a service of type LB or ExternalIP
// `svcPort` corresponds to port details for this service as specified in the service object
// `externalIP` can either be the externalIP or LB.status.ingressIP
// `dstIP` corresponds to the IP to which the provided externalIP needs to be DNAT-ed to
//
//	case1: if svcHasLocalHostNetEndPnt=false + isETPLocal=true, dstIP=config.MasqueradeIP["HostETPLocalMasqueradeIP"]
//	case2: default: dstIP=clusterIP
//
// `svcHasLocalHostNetEndPnt` is true if this service has at least one host-networked endpoint that is local to this node
// `isETPLocal` is true if the svc.Spec.ExternalTrafficPolicy=Local
func getExternalIPTRules(svcPort kapi.ServicePort, externalIP, dstIP string, svcHasLocalHostNetEndPnt, isETPLocal bool) []nodeipt.Rule {
	targetPort := svcPort.Port
	chainName := iptableExternalIPChain
	if !svcHasLocalHostNetEndPnt && isETPLocal {
		// DNAT it to the masqueradeIP:nodePort instead of clusterIP:targetPort
		dstIP = getMasqueradeVIP(externalIP)
		targetPort = svcPort.NodePort
		chainName = iptableETPChain
	}
	return []nodeipt.Rule{
		{
			Table: "nat",
			Chain: chainName,
			Args: []string{
				"-p", string(svcPort.Protocol),
				"-d", externalIP,
				"--dport", fmt.Sprintf("%v", svcPort.Port),
				"-j", "DNAT",
				"--to-destination", util.JoinHostPortInt32(dstIP, targetPort),
			},
			Protocol: getIPTablesProtocol(externalIP),
		},
	}
}

func getGatewayForwardRules(cidrs []*net.IPNet) []nodeipt.Rule {
	var returnRules []nodeipt.Rule
	protocols := make(map[iptables.Protocol]struct{})

	// Add rules for all CIDRs.
	for _, cidr := range cidrs {
		protocol := getIPTablesProtocol(cidr.IP.String())
		protocols[protocol] = struct{}{}

		returnRules = append(returnRules, []nodeipt.Rule{
			{
				Table: "filter",
				Chain: "FORWARD",
				Args: []string{
					"-s", cidr.String(),
					"-j", "ACCEPT",
				},
				Protocol: protocol,
			},
			{
				Table: "filter",
				Chain: "FORWARD",
				Args: []string{
					"-d", cidr.String(),
					"-j", "ACCEPT",
				},
				Protocol: protocol,
			},
		}...)
	}

	// Add rules for MasqueraIPs.
	for protocol := range protocols {
		masqueradeIP := config.Gateway.MasqueradeIPs.V4OVNMasqueradeIP
		if protocol == iptables.ProtocolIPv6 {
			masqueradeIP = config.Gateway.MasqueradeIPs.V6OVNMasqueradeIP
		}
		returnRules = append(returnRules, []nodeipt.Rule{
			{
				Table: "filter",
				Chain: "FORWARD",
				Args: []string{
					"-s", masqueradeIP.String(),
					"-j", "ACCEPT",
				},
				Protocol: protocol,
			},
			{
				Table: "filter",
				Chain: "FORWARD",
				Args: []string{
					"-d", masqueradeIP.String(),
					"-j", "ACCEPT",
				},
				Protocol: protocol,
			},
		}...)
	}

	return returnRules
}

// getTableChainDropRules returns inbound and outbound IPTables DROP rules for the provided tuple of table, chain
// and interface name. It does so for all enabled IP address modes (IPv4 and/or IPv6).
func getTableChainDropRules(table, chain, ifName string) []nodeipt.Rule {
	var dropRules []nodeipt.Rule
	for _, protocol := range clusterIPTablesProtocols() {
		dropRules = append(dropRules, []nodeipt.Rule{
			{
				Table: table,
				Chain: chain,
				Args: []string{
					"-i", ifName,
					"-j", "DROP",
				},
				Protocol: protocol,
			},
			{
				Table: table,
				Chain: chain,
				Args: []string{
					"-o", ifName,
					"-j", "DROP",
				},
				Protocol: protocol,
			},
		}...)
	}
	return dropRules
}

// getGatewayDropRules returns inbound and outbound IPTables DROP rules for the provided interface. It does so for all
// enabled IP address modes (IPv4 and/or IPv6).
func getGatewayDropRules(ifName string) []nodeipt.Rule {
	return getTableChainDropRules("filter", "FORWARD", ifName)

}

// initExternalBridgeForwardingRules sets up iptables rules for br-* interface svc traffic forwarding
// -A FORWARD -s 10.96.0.0/16 -j ACCEPT
// -A FORWARD -d 10.96.0.0/16 -j ACCEPT
// -A FORWARD -s 169.254.169.1 -j ACCEPT
// -A FORWARD -d 169.254.169.1 -j ACCEPT
func initExternalBridgeServiceForwardingRules(cidrs []*net.IPNet) error {
	return insertIptRules(getGatewayForwardRules(cidrs))
}

// initExternalBridgeDropRules sets up iptables rules to block forwarding
// in br-* interfaces (also for 2ndary bridge) - we block for v4 and v6 based on clusterStack
// -A FORWARD -i breth1 -j DROP
// -A FORWARD -o breth1 -j DROP
func initExternalBridgeDropForwardingRules(ifName string) error {
	return appendIptRules(getGatewayDropRules(ifName))
}

// syncPhysicalInterfaceDropRules sets up iptables rules to block forwarding in all physical interfaces.
// syncPhysicalInterfaceDropForwardingRules adds a jump to chain iptableForwardDropChain from filter and add
// iptables block rules per physical interface to iptableForwardDropChain. We define a physical interface as any
// interface of types "device" and "vlan" (exluding the loopback). When an interface is removed,
// syncEnforcePhysicalInterfaceDropRules will make sure to delete the iptables rules for it:
// -A FORWARD -j OVN-KUBE-FORWARD-DROP
// -A OVN-KUBE-FORWARD-DROP -i eth1 -j DROP
// -A OVN-KUBE-FORWARD-DROP -o eth1 -j DROP
// -A OVN-KUBE-FORWARD-DROP -i eth2 -j DROP
// -A OVN-KUBE-FORWARD-DROP -o eth2 -j DROP
// -A OVN-KUBE-FORWARD-DROP -i eth2.100 -j DROP
// -A OVN-KUBE-FORWARD-DROP -o eth2.100 -j DROP
// If interfaceFilterOverride != "", use the provided regex to filter for physical interfaces (instead of the default search
// for physical and VLAN interfaces).
func syncPhysicalInterfaceDropForwardingRules(interfaceFilterOverride string) error {
	var existingRules []nodeipt.Rule
	var generatedRules []nodeipt.Rule
	var rulesToDelete []nodeipt.Rule

	// Get list of all physical interfaces (physical devices and VLANs).
	interfaces, err := listPhysicalInterfaces(interfaceFilterOverride)
	if err != nil {
		return fmt.Errorf("could not list interfaces during attempt to sync forwarding drop rules, err: %q", err)
	}

	// Create forwardDropChains and jump to them from the FORWARDING chain.
	for _, protocol := range clusterIPTablesProtocols() {
		ipt, err := util.GetIPTablesHelper(protocol)
		if err != nil {
			return fmt.Errorf("could not get IPTables helper during attempt to sync forwarding drop rules, err: %q",
				err)
		}
		addChainToTable(ipt, iptableFilterTable, iptableForwardDropChain)
		jumpToDropChain := nodeipt.Rule{
			Protocol: protocol,
			Table:    iptableFilterTable,
			Chain:    iptableForwardChain,
			Args:     []string{"-j", iptableForwardDropChain},
		}
		if err := appendIptRules([]nodeipt.Rule{jumpToDropChain}); err != nil {
			return fmt.Errorf("could append IPTables rule for chain %q during attempt to sync forwarding drop rules, "+
				"err: %q", jumpToDropChain, err)
		}
	}

	// List all IPTables rules inside jumpToDropChain for each protocol.
	for _, protocol := range clusterIPTablesProtocols() {
		existingRulesForProto, err := listIptRules(protocol, iptableFilterTable, iptableForwardDropChain)
		if err != nil {
			return fmt.Errorf("could not list existing IPTables rules during attempt to sync forwarding drop rules, "+
				"protocol: %d, table: %q, chain: %q, err: %q", protocol, iptableFilterTable, iptableForwardDropChain, err)
		}
		existingRules = append(existingRules, existingRulesForProto...)
	}

	// Generate all rules that we need for the current list of physical interfaces.
	for _, intf := range interfaces {
		generatedRules = append(generatedRules, getTableChainDropRules(iptableFilterTable, iptableForwardDropChain, intf)...)
	}

	// Find all rules inside forwardDropChain but that are not part of the generatedRules. Those rules must be deleted.
outer:
	for _, existingRule := range existingRules {
		for _, generatedRule := range generatedRules {
			if existingRule.Equals(generatedRule) {
				continue outer
			}
		}
		rulesToDelete = append(rulesToDelete, existingRule)
	}

	// Send rules for deletion. Only log errors, as this is not critical.
	if err := deleteIptRules(rulesToDelete); err != nil {
		klog.Infof("Could not clean up all IPTables rules during sync of forwarding drop rules, err: %q", err)
	}

	// Send all rules for creation. Rules that already exist will be ignored.
	return appendIptRules(generatedRules)
}

// syncCleanupPhysicalInterfaceDropForwardingRules cleans up IPTables rules for the interface FORWARD DROP. This
// has to happen when ovnkube-node is first started with --disable-forwarding, and is then restarted without the
// CLI parameter.
func syncCleanupPhysicalInterfaceDropForwardingRules() error {
	for _, protocol := range clusterIPTablesProtocols() {
		ipt, err := util.GetIPTablesHelper(protocol)
		if err != nil {
			return fmt.Errorf("could not get IPTables helper during attempt to cleanup forwarding drop rules, err: %q",
				err)
		}
		jumpToDropChain := nodeipt.Rule{
			Protocol: protocol,
			Table:    iptableFilterTable,
			Chain:    iptableForwardChain,
			Args:     []string{"-j", iptableForwardDropChain},
		}
		if err := deleteIptRules([]nodeipt.Rule{jumpToDropChain}); err != nil {
			return fmt.Errorf("could delete IPTables rule for chain %q during attempt to cleanup forwarding drop rules, "+
				"err: %q", jumpToDropChain, err)
		}
		if err = ipt.ClearChain(iptableFilterTable, iptableForwardDropChain); err != nil {
			return fmt.Errorf("could not clear chain %q during attempt to cleanup forwarding drop rules, err: %q",
				iptableForwardDropChain, err)
		}
		if err = ipt.DeleteChain(iptableFilterTable, iptableForwardDropChain); err != nil {
			return fmt.Errorf("could not remove chain %q during attempt to cleanup forwarding drop rules, err: %q",
				iptableForwardDropChain, err)
		}
	}
	return nil
}

func getLocalGatewayFilterRules(ifname string, cidr *net.IPNet) []nodeipt.Rule {
	// Allow packets to/from the gateway interface in case defaults deny
	protocol := getIPTablesProtocol(cidr.IP.String())
	return []nodeipt.Rule{
		{
			Table: "filter",
			Chain: "FORWARD",
			Args: []string{
				"-o", ifname,
				"-j", "ACCEPT",
			},
			Protocol: protocol,
		},
		{
			Table: "filter",
			Chain: "FORWARD",
			Args: []string{
				"-i", ifname,
				"-j", "ACCEPT",
			},
			Protocol: protocol,
		},
		{
			Table: "filter",
			Chain: "INPUT",
			Args: []string{
				"-i", ifname,
				"-m", "comment", "--comment", "from OVN to localhost",
				"-j", "ACCEPT",
			},
			Protocol: protocol,
		},
	}
}

func getLocalGatewayNATRules(ifname string, cidr *net.IPNet) []nodeipt.Rule {
	// Allow packets to/from the gateway interface in case defaults deny
	protocol := getIPTablesProtocol(cidr.IP.String())
	masqueradeIP := config.Gateway.MasqueradeIPs.V4OVNMasqueradeIP
	if protocol == iptables.ProtocolIPv6 {
		masqueradeIP = config.Gateway.MasqueradeIPs.V6OVNMasqueradeIP
	}
	return []nodeipt.Rule{
		{
			Table: "nat",
			Chain: "POSTROUTING",
			Args: []string{
				"-s", masqueradeIP.String(),
				"-j", "MASQUERADE",
			},
			Protocol: protocol,
		},
		{
			Table: "nat",
			Chain: "POSTROUTING",
			Args: []string{
				"-s", cidr.String(),
				"-j", "MASQUERADE",
			},
			Protocol: protocol,
		},
	}
}

// initLocalGatewayNATRules sets up iptables rules for interfaces
func initLocalGatewayNATRules(ifname string, cidr *net.IPNet) error {
	// Insert the filter table rules because they need to be evaluated BEFORE the DROP rules
	// we have for forwarding. DO NOT change the ordering; specially important
	// during SGW->LGW rollouts and restarts.
	err := insertIptRules(getLocalGatewayFilterRules(ifname, cidr))
	if err != nil {
		return fmt.Errorf("unable to insert forwarding rules %v", err)
	}
	// append the masquerade rules in POSTROUTING table since that needs to be
	// evaluated last.
	return appendIptRules(getLocalGatewayNATRules(ifname, cidr))
}

func addChainToTable(ipt util.IPTablesHelper, tableName, chain string) {
	if err := ipt.NewChain(tableName, chain); err != nil {
		klog.V(5).Infof("Chain: \"%s\" in table: \"%s\" already exists, skipping creation: %v", chain, tableName, err)
	}
}

func handleGatewayIPTables(iptCallback func(rules []nodeipt.Rule) error, genGatewayChainRules func(chain string, proto iptables.Protocol) []nodeipt.Rule) error {
	rules := make([]nodeipt.Rule, 0)
	// (NOTE: Order is important, add jump to iptableETPChain before jump to NP/EIP chains)
	for _, chain := range []string{iptableITPChain, egressservice.Chain, iptableNodePortChain, iptableExternalIPChain, iptableETPChain} {
		for _, proto := range clusterIPTablesProtocols() {
			ipt, err := util.GetIPTablesHelper(proto)
			if err != nil {
				return err
			}
			addChainToTable(ipt, "nat", chain)
			if chain == iptableITPChain {
				addChainToTable(ipt, "mangle", chain)
			}
			rules = append(rules, genGatewayChainRules(chain, proto)...)
		}
	}
	if err := iptCallback(rules); err != nil {
		return fmt.Errorf("failed to handle iptables rules %v: %v", rules, err)
	}
	return nil
}

func initSharedGatewayIPTables() error {
	if err := handleGatewayIPTables(insertIptRules, getGatewayInitRules); err != nil {
		return err
	}
	return nil
}

func initLocalGatewayIPTables() error {
	if err := handleGatewayIPTables(insertIptRules, getGatewayInitRules); err != nil {
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
			_ = ipt.DeleteChain("nat", chain)
		}
	}
}

func recreateIPTRules(table, chain string, keepIPTRules []nodeipt.Rule) error {
	var errors []error
	var err error
	var ipt util.IPTablesHelper
	for _, proto := range clusterIPTablesProtocols() {
		if ipt, err = util.GetIPTablesHelper(proto); err != nil {
			errors = append(errors, err)
			continue
		}
		if err = ipt.ClearChain(table, chain); err != nil {
			errors = append(errors, fmt.Errorf("error clearing Chain: %s in Table: %s, err: %v", chain, table, err))
		}
	}
	if err = insertIptRules(keepIPTRules); err != nil {
		errors = append(errors, err)
	}
	return apierrors.NewAggregate(errors)
}

// getGatewayIPTRules returns ClusterIP, NodePort, ExternalIP and LoadBalancer iptables rules for service.
// case1: If !svcHasLocalHostNetEndPnt and svcTypeIsETPLocal rules that redirect traffic
// to ovn-k8s-mp0 preserving sourceIP are added.
//
// case2: (default) A DNAT rule towards clusterIP svc is added ALWAYS.
//
// case3: if svcHasLocalHostNetEndPnt and svcTypeIsITPLocal, rule that redirects clusterIP traffic to host targetPort is added.
//
//	if !svcHasLocalHostNetEndPnt and svcTypeIsITPLocal, rule that marks clusterIP traffic to steer it to ovn-k8s-mp0 is added.
func getGatewayIPTRules(service *kapi.Service, localEndpoints []string, svcHasLocalHostNetEndPnt bool) []nodeipt.Rule {
	rules := make([]nodeipt.Rule, 0)
	clusterIPs := util.GetClusterIPs(service)
	svcTypeIsETPLocal := util.ServiceExternalTrafficPolicyLocal(service)
	svcTypeIsITPLocal := util.ServiceInternalTrafficPolicyLocal(service)
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
			for _, clusterIP := range clusterIPs {
				if svcTypeIsETPLocal && !svcHasLocalHostNetEndPnt {
					// case1 (see function description for details)
					// A DNAT rule to masqueradeIP is added that takes priority over DNAT to clusterIP.
					if config.Gateway.Mode == config.GatewayModeLocal {
						rules = append(rules, getNodePortIPTRules(svcPort, clusterIP, svcPort.NodePort, svcHasLocalHostNetEndPnt, svcTypeIsETPLocal)...)
					}
					// add a skip SNAT rule to OVN-KUBE-SNAT-MGMTPORT to preserve sourceIP for etp=local traffic.
					rules = append(rules, getNodePortETPLocalIPTRules(svcPort, clusterIP)...)
				}
				// case2 (see function description for details)
				rules = append(rules, getNodePortIPTRules(svcPort, clusterIP, svcPort.Port, svcHasLocalHostNetEndPnt, false)...)
			}
		}

		externalIPs := util.GetExternalAndLBIPs(service)

		for _, externalIP := range externalIPs {
			err := util.ValidatePort(svcPort.Protocol, svcPort.Port)
			if err != nil {
				klog.Errorf("Skipping service: %s, invalid service port %v", svcPort.Name, err)
				continue
			}
			if clusterIP, err := util.MatchIPStringFamily(utilnet.IsIPv6String(externalIP), clusterIPs); err == nil {
				if svcTypeIsETPLocal && !svcHasLocalHostNetEndPnt {
					// case1 (see function description for details)
					// DNAT traffic to masqueradeIP:nodePort instead of clusterIP:Port. We are leveraging the existing rules for NODEPORT
					// service so no need to add skip SNAT rule to OVN-KUBE-SNAT-MGMTPORT since the corresponding nodePort svc would have one.
					if !util.ServiceTypeHasNodePort(service) {
						rules = append(rules, generateIPTRulesForLoadBalancersWithoutNodePorts(svcPort, externalIP, service, localEndpoints)...)
					} else {
						rules = append(rules, getExternalIPTRules(svcPort, externalIP, "", svcHasLocalHostNetEndPnt, svcTypeIsETPLocal)...)
					}
				}
				// case2 (see function description for details)
				rules = append(rules, getExternalIPTRules(svcPort, externalIP, clusterIP, svcHasLocalHostNetEndPnt, false)...)
			}
		}
		if svcTypeIsITPLocal {
			// case3 (see function decription for details)
			for _, clusterIP := range clusterIPs {
				rules = append(rules, getITPLocalIPTRules(svcPort, clusterIP, svcHasLocalHostNetEndPnt)...)
			}
		}
	}
	return rules
}
