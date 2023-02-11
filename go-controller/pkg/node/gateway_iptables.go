//go:build linux
// +build linux

package node

import (
	"fmt"
	"net"
	"strings"

	"github.com/coreos/go-iptables/iptables"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/pkg/errors"
	kapi "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
)

const (
	iptableNodePortChain   = "OVN-KUBE-NODEPORT"   // called from nat-PREROUTING and nat-OUTPUT
	iptableExternalIPChain = "OVN-KUBE-EXTERNALIP" // called from nat-PREROUTING and nat-OUTPUT
	iptableETPChain        = "OVN-KUBE-ETP"        // called from nat-PREROUTING only
	iptableITPChain        = "OVN-KUBE-ITP"        // called from mangle-OUTPUT and nat-OUTPUT
	iptableESVCChain       = "OVN-KUBE-EGRESS-SVC" // called from nat-POSTROUTING
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
		return types.V6HostETPLocalMasqueradeIP
	}
	return types.V4HostETPLocalMasqueradeIP
}

type iptRule struct {
	table    string
	chain    string
	args     []string
	protocol iptables.Protocol
}

func addIptRules(rules []iptRule, append bool) error {
	addErrors := errors.New("")
	var err error
	var ipt util.IPTablesHelper
	var exists bool
	for _, r := range rules {
		klog.V(5).Infof("Adding rule in table: %s, chain: %s with args: \"%s\" for protocol: %v ",
			r.table, r.chain, strings.Join(r.args, " "), r.protocol)
		if ipt, err = util.GetIPTablesHelper(r.protocol); err != nil {
			addErrors = errors.Wrapf(addErrors,
				"Failed to add iptables %s/%s rule %q: %v", r.table, r.chain, strings.Join(r.args, " "), err)
			continue
		}
		if err = ipt.NewChain(r.table, r.chain); err != nil {
			klog.V(5).Infof("Chain: \"%s\" in table: \"%s\" already exists, skipping creation: %v",
				r.chain, r.table, err)
		}
		exists, err = ipt.Exists(r.table, r.chain, r.args...)
		if !exists && err == nil {
			if append {
				err = ipt.Append(r.table, r.chain, r.args...)
			} else {
				err = ipt.Insert(r.table, r.chain, 1, r.args...)
			}
		}
		if err != nil {
			addErrors = errors.Wrapf(addErrors, "failed to add iptables %s/%s rule %q: %v",
				r.table, r.chain, strings.Join(r.args, " "), err)
		}
	}
	if addErrors.Error() == "" {
		addErrors = nil
	}
	return addErrors
}

// insertIptRules adds the provided rules in an insert fashion
// i.e each rule gets added at the first position in the chain
func insertIptRules(rules []iptRule) error {
	return addIptRules(rules, false)
}

// appendIptRules adds the provided rules in an append fashion
// i.e each rule gets added at the last position in the chain
func appendIptRules(rules []iptRule) error {
	return addIptRules(rules, true)
}

func delIptRules(rules []iptRule) error {
	delErrors := errors.New("")
	var err error
	var ipt util.IPTablesHelper
	for _, r := range rules {
		klog.V(5).Infof("Deleting rule in table: %s, chain: %s with args: \"%s\" for protocol: %v ",
			r.table, r.chain, strings.Join(r.args, " "), r.protocol)
		if ipt, err = util.GetIPTablesHelper(r.protocol); err != nil {
			delErrors = errors.Wrapf(delErrors,
				"Failed to delete iptables %s/%s rule %q: %v", r.table, r.chain, strings.Join(r.args, " "), err)
			continue
		}
		if exists, err := ipt.Exists(r.table, r.chain, r.args...); err == nil && exists {
			err := ipt.Delete(r.table, r.chain, r.args...)
			if err != nil {
				delErrors = errors.Wrapf(delErrors, "failed to delete iptables %s/%s rule %q: %v",
					r.table, r.chain, strings.Join(r.args, " "), err)
			}
		}
	}
	if delErrors.Error() == "" {
		delErrors = nil
	}
	return delErrors
}

func getGatewayInitRules(chain string, proto iptables.Protocol) []iptRule {
	iptRules := []iptRule{}
	if chain == iptableESVCChain {
		return []iptRule{
			{
				table:    "nat",
				chain:    "POSTROUTING",
				args:     []string{"-j", chain},
				protocol: proto,
			},
			egressSVCIPTDefaultReturnRule(),
		}
	}
	if chain == iptableITPChain {
		iptRules = append(iptRules,
			iptRule{
				table:    "mangle",
				chain:    "OUTPUT",
				args:     []string{"-j", chain},
				protocol: proto,
			},
		)
	} else {
		iptRules = append(iptRules,
			iptRule{
				table:    "nat",
				chain:    "PREROUTING",
				args:     []string{"-j", chain},
				protocol: proto,
			},
		)
	}
	if chain != iptableETPChain { // ETP chain only meant for external traffic
		iptRules = append(iptRules,
			iptRule{
				table:    "nat",
				chain:    "OUTPUT",
				args:     []string{"-j", chain},
				protocol: proto,
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
//	case1: if svcHasLocalHostNetEndPnt=false + isETPLocal=true targetIP=types.HostETPLocalMasqueradeIP and targetPort=svcPort.NodePort
//	case2: default: targetIP=clusterIP and targetPort=svcPort.Port
//
// `svcHasLocalHostNetEndPnt` is true if this service has at least one host-networked endpoint that is local to this node
// `isETPLocal` is true if the svc.Spec.ExternalTrafficPolicy=Local
func getNodePortIPTRules(svcPort kapi.ServicePort, targetIP string, targetPort int32, svcHasLocalHostNetEndPnt, isETPLocal bool) []iptRule {
	chainName := iptableNodePortChain
	if !svcHasLocalHostNetEndPnt && isETPLocal {
		// DNAT it to the masqueradeIP:nodePort instead of clusterIP:targetPort
		targetIP = getMasqueradeVIP(targetIP)
		chainName = iptableETPChain
	}
	return []iptRule{
		{
			table: "nat",
			chain: chainName,
			args: []string{
				"-p", string(svcPort.Protocol),
				"-m", "addrtype",
				"--dst-type", "LOCAL",
				"--dport", fmt.Sprintf("%d", svcPort.NodePort),
				"-j", "DNAT",
				"--to-destination", util.JoinHostPortInt32(targetIP, targetPort),
			},
			protocol: getIPTablesProtocol(targetIP),
		},
	}
}

// getITPLocalIPTRules returns the IPTable REDIRECT or MARK rules for the provided service
// `svcPort` corresponds to port details for this service as specified in the service object
// `clusterIP` is clusterIP is the VIP of the service to match on
// `svcHasLocalHostNetEndPnt` is true if this service has at least one host-networked endpoint that is local to this node
// NOTE: Currently invoked only for Internal Traffic Policy
func getITPLocalIPTRules(svcPort kapi.ServicePort, clusterIP string, svcHasLocalHostNetEndPnt bool) []iptRule {
	if svcHasLocalHostNetEndPnt {
		return []iptRule{
			{
				table: "nat",
				chain: iptableITPChain,
				args: []string{
					"-p", string(svcPort.Protocol),
					"-d", clusterIP,
					"--dport", fmt.Sprintf("%v", svcPort.Port),
					"-j", "REDIRECT",
					"--to-port", fmt.Sprintf("%v", int32(svcPort.TargetPort.IntValue())),
				},
				protocol: getIPTablesProtocol(clusterIP),
			},
		}
	}
	return []iptRule{
		{
			table: "mangle",
			chain: iptableITPChain,
			args: []string{
				"-p", string(svcPort.Protocol),
				"-d", string(clusterIP),
				"--dport", fmt.Sprintf("%d", svcPort.Port),
				"-j", "MARK",
				"--set-xmark", string(ovnkubeITPMark),
			},
			protocol: getIPTablesProtocol(clusterIP),
		},
	}
}

// getNodePortETPLocalIPTRules returns the IPTable REDIRECT or RETURN rules for a service of type nodePort if ETP=local
// `svcPort` corresponds to port details for this service as specified in the service object
// `targetIP` corresponds to svc.spec.ClusterIP
// This function returns a RETURN rule in iptableMgmPortChain to prevent SNAT of sourceIP
func getNodePortETPLocalIPTRules(svcPort kapi.ServicePort, targetIP string) []iptRule {
	return []iptRule{
		{
			table: "nat",
			chain: iptableMgmPortChain,
			args: []string{
				"-p", string(svcPort.Protocol),
				"--dport", fmt.Sprintf("%d", svcPort.NodePort),
				"-j", "RETURN",
			},
			protocol: getIPTablesProtocol(targetIP),
		},
	}
}

func computeProbability(n, i int) string {
	return fmt.Sprintf("%0.10f", 1.0/float64(n-i+1))
}

func generateIPTRulesForLoadBalancersWithoutNodePorts(svcPort kapi.ServicePort, externalIP string, service *kapi.Service, localEndpoints []string) []iptRule {
	var iptRules []iptRule
	if len(localEndpoints) == 0 {
		// either its smart nic mode; etp&itp not implemented, OR
		// fetching endpointSlices error-ed out prior to reaching here so nothing to do
		return iptRules
	}
	numLocalEndpoints := len(localEndpoints)
	for i, ip := range localEndpoints {
		iptRules = append([]iptRule{
			{
				table: "nat",
				chain: iptableETPChain,
				args: []string{
					"-p", string(svcPort.Protocol),
					"-d", externalIP,
					"--dport", fmt.Sprintf("%v", svcPort.Port),
					"-j", "DNAT",
					"--to-destination", util.JoinHostPortInt32(ip, int32(svcPort.TargetPort.IntValue())),
					"-m", "statistic",
					"--mode", "random",
					"--probability", computeProbability(numLocalEndpoints, i+1),
				},
				protocol: getIPTablesProtocol(externalIP),
			},
			{
				table: "nat",
				chain: iptableMgmPortChain,
				args: []string{
					"-p", string(svcPort.Protocol),
					"-d", ip,
					"--dport", fmt.Sprintf("%v", int32(svcPort.TargetPort.IntValue())),
					"-j", "RETURN",
				},
				protocol: getIPTablesProtocol(externalIP),
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
//	case1: if svcHasLocalHostNetEndPnt=false + isETPLocal=true, dstIP=types.HostETPLocalMasqueradeIP
//	case2: default: dstIP=clusterIP
//
// `svcHasLocalHostNetEndPnt` is true if this service has at least one host-networked endpoint that is local to this node
// `isETPLocal` is true if the svc.Spec.ExternalTrafficPolicy=Local
func getExternalIPTRules(svcPort kapi.ServicePort, externalIP, dstIP string, svcHasLocalHostNetEndPnt, isETPLocal bool) []iptRule {
	targetPort := svcPort.Port
	chainName := iptableExternalIPChain
	if !svcHasLocalHostNetEndPnt && isETPLocal {
		// DNAT it to the masqueradeIP:nodePort instead of clusterIP:targetPort
		dstIP = getMasqueradeVIP(externalIP)
		targetPort = svcPort.NodePort
		chainName = iptableETPChain
	}
	return []iptRule{
		{
			table: "nat",
			chain: chainName,
			args: []string{
				"-p", string(svcPort.Protocol),
				"-d", externalIP,
				"--dport", fmt.Sprintf("%v", svcPort.Port),
				"-j", "DNAT",
				"--to-destination", util.JoinHostPortInt32(dstIP, targetPort),
			},
			protocol: getIPTablesProtocol(externalIP),
		},
	}
}

func getLocalGatewayNATRules(ifname string, cidr *net.IPNet) []iptRule {
	// Allow packets to/from the gateway interface in case defaults deny
	protocol := getIPTablesProtocol(cidr.IP.String())
	masqueradeIP := types.V4OVNMasqueradeIP
	if protocol == iptables.ProtocolIPv6 {
		masqueradeIP = types.V6OVNMasqueradeIP
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
				"-s", cidr.String(),
				"-j", "MASQUERADE",
			},
			protocol: protocol,
		},
		{
			table: "nat",
			chain: "POSTROUTING",
			args: []string{
				"-s", masqueradeIP,
				"-j", "MASQUERADE",
			},
			protocol: protocol,
		},
	}
}

// initLocalGatewayNATRules sets up iptables rules for interfaces
func initLocalGatewayNATRules(ifname string, cidr *net.IPNet) error {
	return insertIptRules(getLocalGatewayNATRules(ifname, cidr))
}

func addChaintoTable(ipt util.IPTablesHelper, tableName, chain string) {
	if err := ipt.NewChain(tableName, chain); err != nil {
		klog.V(5).Infof("Chain: \"%s\" in table: \"%s\" already exists, skipping creation: %v", chain, tableName, err)
	}
}

func handleGatewayIPTables(iptCallback func(rules []iptRule) error, genGatewayChainRules func(chain string, proto iptables.Protocol) []iptRule) error {
	rules := make([]iptRule, 0)
	// (NOTE: Order is important, add jump to iptableETPChain before jump to NP/EIP chains)
	for _, chain := range []string{iptableITPChain, iptableESVCChain, iptableNodePortChain, iptableExternalIPChain, iptableETPChain} {
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

func recreateIPTRules(table, chain string, keepIPTRules []iptRule) error {
	var errors []error
	var err error
	var ipt util.IPTablesHelper
	for _, proto := range clusterIPTablesProtocols() {
		if ipt, err = util.GetIPTablesHelper(proto); err != nil {
			errors = append(errors, err)
			continue
		}
		if err = ipt.ClearChain(table, chain); err != nil {
			errors = append(errors, fmt.Errorf("error clearing chain: %s in table: %s, err: %v", chain, table, err))
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
func getGatewayIPTRules(service *kapi.Service, localEndpoints []string, svcHasLocalHostNetEndPnt bool) []iptRule {
	rules := make([]iptRule, 0)
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
					rules = append(rules, getNodePortIPTRules(svcPort, clusterIP, svcPort.NodePort, svcHasLocalHostNetEndPnt, svcTypeIsETPLocal)...)
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

// Returns all of the SNAT rules that should be created for an egress service with the given endpoints.
func egressSVCIPTRulesForEndpoints(svc *kapi.Service, v4Eps, v6Eps []string) []iptRule {
	rules := []iptRule{}

	comment, _ := cache.MetaNamespaceKeyFunc(svc)
	for _, lb := range svc.Status.LoadBalancer.Ingress {
		lbIPStr := utilnet.ParseIPSloppy(lb.IP).String()
		lbProto := getIPTablesProtocol(lbIPStr)
		epsForProto := v4Eps
		if lbProto == iptables.ProtocolIPv6 {
			epsForProto = v6Eps
		}

		for _, ep := range epsForProto {
			rules = append(rules, iptRule{
				table: "nat",
				chain: iptableESVCChain,
				args: []string{
					"-s", ep,
					"-m", "comment", "--comment", comment,
					"-j", "SNAT",
					"--to-source", lbIPStr,
				},
				protocol: lbProto,
			})
		}
	}

	return rules
}

func egressSVCIPTDefaultReturnRule() iptRule {
	return iptRule{
		table: "nat",
		chain: iptableESVCChain,
		args: []string{
			"-m", "mark", "--mark", string(ovnKubeNodeSNATMark),
			"-m", "comment", "--comment", "Do not SNAT to SVC VIP",
			"-j", "RETURN",
		},
	}
}
