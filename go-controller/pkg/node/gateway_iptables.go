//go:build linux
// +build linux

package node

import (
	"fmt"
	"net"

	kapi "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	"github.com/coreos/go-iptables/iptables"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/controllers/egressservice"
	nodeipt "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/iptables"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	utilerrors "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/errors"
)

const (
	iptableNodePortChain      = "OVN-KUBE-NODEPORT"       // called from nat-PREROUTING and nat-OUTPUT
	iptableExternalIPChain    = "OVN-KUBE-EXTERNALIP"     // called from nat-PREROUTING and nat-OUTPUT
	iptableETPChain           = "OVN-KUBE-ETP"            // called from nat-PREROUTING only
	iptableITPChain           = "OVN-KUBE-ITP"            // called from mangle-OUTPUT and nat-OUTPUT
	iptableUDNMasqueradeChain = "OVN-KUBE-UDN-MASQUERADE" // called from nat-POSTROUTING
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

// restoreIptRulesFiltered restores the provided rules in an insert fashion with a filter for table/chain
// i.e each rule gets added at the first position in the chain
// filter is defined as a map of table/chains. Only rules matching this filter will be restored.
// If no rules match the filter, the chain will still be restored as empty as specified in the filter.
func restoreIptRulesFiltered(rules []nodeipt.Rule, filter map[string]map[string]struct{}) error {
	return nodeipt.RestoreRulesFiltered(rules, filter)
}

// appendIptRules adds the provided rules in an append fashion
// i.e each rule gets added at the last position in the chain
func appendIptRules(rules []nodeipt.Rule) error {
	return nodeipt.AddRules(rules, true)
}

// deleteIptRules removes provided rules from the chain
func deleteIptRules(rules []nodeipt.Rule) error {
	return nodeipt.DelRules(rules)
}

// ensureChain ensures that a chain exists within a table
func ensureChain(table, chain string) error {
	for _, proto := range clusterIPTablesProtocols() {
		ipt, err := util.GetIPTablesHelper(proto)
		if err != nil {
			return fmt.Errorf("failed to get IPTables helper to add UDN chain: %v", err)
		}
		addChaintoTable(ipt, table, chain)
	}
	return nil
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

// getNodePortETPLocalIPTRule returns the IPTable REDIRECT or RETURN rules for a service of type nodePort if ETP=local
// `svcPort` corresponds to port details for this service as specified in the service object
// `targetIP` corresponds to svc.spec.ClusterIP
// This function returns a RETURN rule in iptableMgmPortChain to prevent SNAT of sourceIP
func getNodePortETPLocalIPTRule(svcPort kapi.ServicePort, targetIP string) nodeipt.Rule {
	return getSkipMgmtSNATRule(string(svcPort.Protocol), fmt.Sprintf("%d", svcPort.NodePort), "", getIPTablesProtocol(targetIP))
}

// getSkipMgmtSNATRule generates the return iptables rule for avoiding SNAT to mgmt port
func getSkipMgmtSNATRule(protocol, port, destIP string, ipFamily iptables.Protocol) nodeipt.Rule {
	args := make([]string, 0, 8)
	args = append(args, "-p", protocol)
	if len(destIP) > 0 {
		args = append(args, "-d", destIP)
	}
	args = append(args, "--dport", port, "-j", "RETURN")
	n := nodeipt.Rule{
		Table:    "nat",
		Chain:    iptableMgmPortChain,
		Args:     args,
		Protocol: ipFamily,
	}
	return n
}

func computeProbability(n, i int) string {
	return fmt.Sprintf("%0.10f", 1.0/float64(n-i+1))
}

func generateSkipMgmtForLocalEndpoints(svcPort kapi.ServicePort, externalIP string, localEndpoints []string) []nodeipt.Rule {
	iptRules := make([]nodeipt.Rule, 0, len(localEndpoints))
	for _, localEndpoint := range localEndpoints {
		if len(localEndpoint) == 0 {
			continue
		}
		iptRules = append([]nodeipt.Rule{getSkipMgmtSNATRule(
			string(svcPort.Protocol),
			fmt.Sprintf("%v", int32(svcPort.TargetPort.IntValue())),
			localEndpoint,
			getIPTablesProtocol(externalIP),
		)}, iptRules...)
	}
	return iptRules
}

func generateIPTRulesForLoadBalancersWithoutNodePorts(svcPort kapi.ServicePort, externalIP string, localEndpoints []string) []nodeipt.Rule {
	iptRules := make([]nodeipt.Rule, 0, len(localEndpoints))
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
		returnRules = append(returnRules, getMasqueradeIpTablesForwardRules(masqueradeIP, protocol)...)
	}

	return returnRules
}

// getStaleMasqueradeIptablesRules returns all iptables rules may get added for a given masquerade IP.
func getStaleMasqueradeIptablesRules(masqueradeIP net.IP) []nodeipt.Rule {
	return append(getMasqueradeIpTablesForwardRules(masqueradeIP, getIPTablesProtocol(masqueradeIP.String())),
		getMasqueradeIpTablesNATRules(masqueradeIP, getIPTablesProtocol(masqueradeIP.String()))...)
}

func getMasqueradeIpTablesForwardRules(masqueradeIP net.IP, protocol iptables.Protocol) []nodeipt.Rule {
	return []nodeipt.Rule{
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
	}
}

func getMasqueradeIpTablesNATRules(masqueradeIP net.IP, protocol iptables.Protocol) []nodeipt.Rule {
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
	}
}

// initExternalBridgeForwardingRules sets up iptables rules for br-* interface svc traffic forwarding
// -A FORWARD -s 10.96.0.0/16 -j ACCEPT
// -A FORWARD -d 10.96.0.0/16 -j ACCEPT
// -A FORWARD -s 169.254.169.1 -j ACCEPT
// -A FORWARD -d 169.254.169.1 -j ACCEPT
func initExternalBridgeServiceForwardingRules(cidrs []*net.IPNet) error {
	return insertIptRules(getGatewayForwardRules(cidrs))
}

// delExternalBridgeServiceForwardingRules removes iptables rules which might
// have been added to disable forwarding
func delExternalBridgeServiceForwardingRules(cidrs []*net.IPNet) error {
	return deleteIptRules(getGatewayForwardRules(cidrs))
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

func getLocalGatewayNATRules(cidr *net.IPNet) []nodeipt.Rule {
	// Allow packets to/from the gateway interface in case defaults deny
	protocol := getIPTablesProtocol(cidr.IP.String())
	masqueradeIP := config.Gateway.MasqueradeIPs.V4OVNMasqueradeIP
	if protocol == iptables.ProtocolIPv6 {
		masqueradeIP = config.Gateway.MasqueradeIPs.V6OVNMasqueradeIP
	}
	rules := []nodeipt.Rule{
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
	// FIXME(tssurya): If the feature is disabled we should be removing
	// these rules
	if util.IsNetworkSegmentationSupportEnabled() {
		rules = append(rules, getUDNMasqueradeRules(protocol)...)
	}
	return rules
}

// getUDNMasqueradeRules is only called for local-gateway-mode
func getUDNMasqueradeRules(protocol iptables.Protocol) []nodeipt.Rule {
	// the following rules are actively used only for the UDN Feature:
	// -A POSTROUTING -j OVN-KUBE-UDN-MASQUERADE
	// -A OVN-KUBE-UDN-MASQUERADE -s 169.254.0.0/29 -j RETURN
	// -A OVN-KUBE-UDN-MASQUERADE -d 10.96.0.0/16 -j RETURN
	// -A OVN-KUBE-UDN-MASQUERADE -s 169.254.0.0/17 -j MASQUERADE
	// NOTE: Ordering is important here, the RETURN must come before
	// the MASQUERADE rule. Please don't change the ordering.
	srcUDNMasqueradePrefix := config.Gateway.V4MasqueradeSubnet
	// defaultNetworkReservedMasqueradePrefix contains the first 6IPs in the masquerade
	// range that shouldn't be MASQUERADED. Hence /29 and /125 is intentionally hardcoded here
	defaultNetworkReservedMasqueradePrefix := config.Gateway.MasqueradeIPs.V4HostMasqueradeIP.String() + "/29"
	ipFamily := utilnet.IPv4
	if protocol == iptables.ProtocolIPv6 {
		srcUDNMasqueradePrefix = config.Gateway.V6MasqueradeSubnet
		defaultNetworkReservedMasqueradePrefix = config.Gateway.MasqueradeIPs.V6HostMasqueradeIP.String() + "/125"
		ipFamily = utilnet.IPv6
	}
	rules := []nodeipt.Rule{
		{
			Table:    "nat",
			Chain:    "POSTROUTING",
			Args:     []string{"-j", iptableUDNMasqueradeChain}, // NOTE: AddRules will take care of creating the chain
			Protocol: protocol,
		},
		{
			Table: "nat",
			Chain: iptableUDNMasqueradeChain,
			Args: []string{
				"-s", defaultNetworkReservedMasqueradePrefix,
				"-j", "RETURN",
			},
			Protocol: protocol,
		},
	}
	for _, svcCIDR := range config.Kubernetes.ServiceCIDRs {
		if utilnet.IPFamilyOfCIDR(svcCIDR) != ipFamily {
			continue
		}
		rules = append(rules,
			nodeipt.Rule{
				Table: "nat",
				Chain: iptableUDNMasqueradeChain,
				Args: []string{
					"-d", svcCIDR.String(),
					"-j", "RETURN",
				},
				Protocol: protocol,
			},
		)
	}
	rules = append(rules,
		nodeipt.Rule{
			Table: "nat",
			Chain: iptableUDNMasqueradeChain,
			Args: []string{
				"-s", srcUDNMasqueradePrefix,
				"-j", "MASQUERADE",
			},
			Protocol: protocol,
		},
	)
	return rules
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
	return appendIptRules(getLocalGatewayNATRules(cidr))
}

func addChaintoTable(ipt util.IPTablesHelper, tableName, chain string) {
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
			addChaintoTable(ipt, "nat", chain)
			if chain == iptableITPChain {
				addChaintoTable(ipt, "mangle", chain)
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
	klog.Infof("Recreating iptables rules for table: %s, chain: %s", table, chain)
	// filter is a map of the table/chain to program rules for, as all rules are included in keepIPTRules
	filter := map[string]map[string]struct{}{table: {chain: {}}}
	if err = restoreIptRulesFiltered(keepIPTRules, filter); err != nil {
		errors = append(errors, err)
	}
	return utilerrors.Join(errors...)
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
					rules = append(rules, getNodePortETPLocalIPTRule(svcPort, clusterIP))
				}
				// case2 (see function description for details)
				rules = append(rules, getNodePortIPTRules(svcPort, clusterIP, svcPort.Port, svcHasLocalHostNetEndPnt, false)...)
			}
		}

		externalIPs := util.GetExternalAndLBIPs(service)

		snatRulesCreated := false
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
						rules = append(rules, generateIPTRulesForLoadBalancersWithoutNodePorts(svcPort, externalIP, localEndpoints)...)
						// These rules are per endpoint and should only be created one time per endpoint and port combination
						if !snatRulesCreated {
							rules = append(rules, generateSkipMgmtForLocalEndpoints(svcPort, externalIP, localEndpoints)...)
							snatRulesCreated = true
						}
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
