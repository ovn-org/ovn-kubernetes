package ovn

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/pkg/errors"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	egressfirewallapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	kapi "k8s.io/api/core/v1"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
)

const (
	egressFirewallAppliedCorrectly = "EgressFirewall Rules applied"
	egressFirewallAddError         = "EgressFirewall Rules not correctly added"
	egressFirewallUpdateError      = "EgressFirewall Rules not correctly updated"
)

type egressFirewall struct {
	name        string
	namespace   string
	egressRules []*egressFirewallRule
}

type egressFirewallRule struct {
	id     int
	access egressfirewallapi.EgressFirewallRuleType
	ports  []egressfirewallapi.EgressFirewallPort
	to     destination
}

type destination struct {
	cidrSelector string
	dnsName      string
}

func newEgressFirewall(egressFirewallPolicy *egressfirewallapi.EgressFirewall) *egressFirewall {
	ef := &egressFirewall{
		name:        egressFirewallPolicy.Name,
		namespace:   egressFirewallPolicy.Namespace,
		egressRules: make([]*egressFirewallRule, 0),
	}
	return ef
}

func newEgressFirewallRule(rawEgressFirewallRule egressfirewallapi.EgressFirewallRule, id int) (*egressFirewallRule, error) {
	efr := &egressFirewallRule{
		id:     id,
		access: rawEgressFirewallRule.Type,
	}

	if rawEgressFirewallRule.To.DNSName != "" {
		efr.to.dnsName = rawEgressFirewallRule.To.DNSName
	} else {

		_, _, err := net.ParseCIDR(rawEgressFirewallRule.To.CIDRSelector)
		if err != nil {
			return nil, err
		}
		efr.to.cidrSelector = rawEgressFirewallRule.To.CIDRSelector
	}
	efr.ports = rawEgressFirewallRule.Ports

	return efr, nil
}

func (oc *Controller) addEgressFirewall(egressFirewall *egressfirewallapi.EgressFirewall) error {
	klog.Infof("Adding egressFirewall %s in namespace %s", egressFirewall.Name, egressFirewall.Namespace)
	nsInfo, err := oc.waitForNamespaceLocked(egressFirewall.Namespace)
	if err != nil {
		return fmt.Errorf("failed to wait for namespace %s event (%v)",
			egressFirewall.Namespace, err)
	}
	defer nsInfo.Unlock()

	if nsInfo.egressFirewallPolicy != nil {
		return fmt.Errorf("error attempting to add egressFirewall %s to namespace %s when it already has an egressFirewall",
			egressFirewall.Name, egressFirewall.Namespace)
	}

	ef := newEgressFirewall(egressFirewall)
	nsInfo.egressFirewallPolicy = ef
	var addErrors error
	//the highest priority rule is reserved blocking all external traffic during update
	egressFirewallStartPriorityInt, err := strconv.Atoi(types.EgressFirewallStartPriority)
	egressFirewallStartPriorityInt = egressFirewallStartPriorityInt - 1
	if err != nil {
		return fmt.Errorf("failed to convert egressFirewallStartPriority to Integer: cannot add egressFirewall for namespace %s", egressFirewall.Namespace)
	}
	minimumReservedEgressFirewallPriorityInt, err := strconv.Atoi(types.MinimumReservedEgressFirewallPriority)
	if err != nil {
		return fmt.Errorf("failed to convert minumumReservedEgressFirewallPriority to Integer: cannot add egressFirewall for namespace %s", egressFirewall.Namespace)
	}
	for i, egressFirewallRule := range egressFirewall.Spec.Egress {
		// process Rules into egressFirewallRules for egressFirewall struct
		if i > egressFirewallStartPriorityInt-minimumReservedEgressFirewallPriorityInt {
			klog.Warningf("egressFirewall for namespace %s has too many rules, the rest will be ignored",
				egressFirewall.Namespace)
			break
		}
		efr, err := newEgressFirewallRule(egressFirewallRule, i)
		if err != nil {
			addErrors = errors.Wrapf(addErrors, "error: cannot create EgressFirewall Rule to destination %s for namespace %s - %v",
				egressFirewallRule.To.CIDRSelector, egressFirewall.Namespace, err)
			continue

		}
		ef.egressRules = append(ef.egressRules, efr)
	}
	if addErrors != nil {
		return addErrors
	}

	if nsInfo.addressSet == nil {
		// TODO(trozet): remove dependency on nsInfo object and just determine hash names to create Egress FW with
		return fmt.Errorf("unable to add egress firewall policy, namespace: %s has no address set", egressFirewall.Namespace)
	}

	err = oc.addLogicalRouterPolicyToClusterRouter(nsInfo.addressSet.GetIPv4HashName(), nsInfo.addressSet.GetIPv6HashName(), egressFirewall.Namespace, egressFirewallStartPriorityInt)
	if err != nil {
		return err
	}

	return nil
}

func (oc *Controller) updateEgressFirewall(oldEgressFirewall, newEgressFirewall *egressfirewallapi.EgressFirewall) error {
	// block all external traffic in this namespace
	nsInfo, err := oc.waitForNamespaceLocked(newEgressFirewall.Namespace)
	if err != nil {
		return fmt.Errorf("cannot update egressfirewall in %s:%v", newEgressFirewall.Namespace, err)
	}
	addressSet := nsInfo.addressSet
	nsInfo.Unlock()
	priority, err := strconv.Atoi(types.EgressFirewallStartPriority)
	if err != nil {
		return fmt.Errorf("cannot update egressfirewall in %s:%v", newEgressFirewall.Namespace, err)
	}

	match := generateMatch(addressSet.GetIPv4HashName(), addressSet.GetIPv6HashName(), []matchTarget{
		{matchKindV4CIDR, "0.0.0.0/0"},
		{matchKindV6CIDR, "::/0"},
	},
		[]egressfirewallapi.EgressFirewallPort{},
	)

	err = createLogicalRouterPolicy(priority, match, "drop", newEgressFirewall.Namespace+"-blockAll")
	if err != nil {
		return fmt.Errorf("cannot update egressfirewall in %s:%v", newEgressFirewall.Namespace, err)
	}

	updateErrors := oc.deleteEgressFirewall(oldEgressFirewall)
	// add the new egressfirewall

	updateErrors = errors.Wrapf(updateErrors, "%v", oc.addEgressFirewall(newEgressFirewall))
	// delete policy blocking all external traffic
	stdout, stderr, err := util.RunOVNNbctl("--data=bare", "--no-heading", "--columns=_uuid", "find",
		"logical_router_policy", fmt.Sprintf("external-ids:egressFirewall=%s-blockAll", newEgressFirewall.Namespace))
	if err != nil {
		updateErrors = errors.Wrapf(updateErrors,
			"error deleting blockAll policy for namespace %s, cannot get logical router policies from LR %s - %s:%s",
			newEgressFirewall.Namespace, types.OVNClusterRouter, err, stderr)
	}
	_, stderr, err = util.RunOVNNbctl("lr-policy-del", types.OVNClusterRouter, stdout)
	if err != nil {
		updateErrors = errors.Wrapf(updateErrors, "failed to delete the blockAll rule for "+
			"egressFirewall in namespace %s on logical switch %s during update, stderr: %q (%v)",
			newEgressFirewall.Namespace, types.OVNClusterRouter, stderr, err)
	}
	return updateErrors
}

func (oc *Controller) deleteEgressFirewall(egressFirewall *egressfirewallapi.EgressFirewall) error {
	klog.Infof("Deleting egress Firewall %s in namespace %s", egressFirewall.Name, egressFirewall.Namespace)
	deleteDNS := false

	nsInfo := oc.getNamespaceLocked(egressFirewall.Namespace)
	if nsInfo != nil {
		// clear it so an error does not prevent future egressFirewalls
		for _, rule := range nsInfo.egressFirewallPolicy.egressRules {
			if len(rule.to.dnsName) > 0 {
				deleteDNS = true
				break
			}
		}
		nsInfo.egressFirewallPolicy = nil
		nsInfo.Unlock()
	}
	if deleteDNS {
		oc.egressFirewallDNS.Delete(egressFirewall.Namespace)
	}
	stdout, stderr, err := util.RunOVNNbctl("--data=bare", "--no-heading", "--columns=_uuid", "find", "logical_router_policy", fmt.Sprintf("external-ids:egressFirewall=%s", egressFirewall.Namespace))
	if err != nil {
		return fmt.Errorf("error deleting egressFirewall for namespace %s, cannot get logical router policies from LR %s - %s:%s",
			egressFirewall.Namespace, types.OVNClusterRouter, err, stderr)
	}
	var deletionErrors error

	uuids := strings.Fields(stdout)
	for _, uuid := range uuids {
		_, stderr, err := util.RunOVNNbctl("lr-policy-del", types.OVNClusterRouter, uuid)
		if err != nil {
			deletionErrors = errors.Wrapf(deletionErrors, "failed to delete the rules for "+
				"egressFirewall in namespace %s on logical switch %s, stderr: %q (%v)", egressFirewall.Namespace, types.OVNClusterRouter, stderr, err)
		}
	}
	return deletionErrors
}

func (oc *Controller) updateEgressFirewallWithRetry(egressfirewall *egressfirewallapi.EgressFirewall) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		return oc.kube.UpdateEgressFirewall(egressfirewall)
	})
}

func (oc *Controller) addLogicalRouterPolicyToClusterRouter(hashedAddressSetNameIPv4, hashedAddressSetNameIPv6, namespace string, efStartPriority int) error {
	ef := oc.namespaces[namespace].egressFirewallPolicy
	for _, rule := range ef.egressRules {
		var action string
		var matchTargets []matchTarget
		if rule.access == egressfirewallapi.EgressFirewallRuleAllow {
			action = "allow"
		} else {
			action = "drop"
		}
		if rule.to.cidrSelector != "" {
			if utilnet.IsIPv6CIDRString(rule.to.cidrSelector) {
				matchTargets = []matchTarget{{matchKindV6CIDR, rule.to.cidrSelector}}
			} else {
				matchTargets = []matchTarget{{matchKindV4CIDR, rule.to.cidrSelector}}
			}
		} else {
			// rule based on DNS NAME
			dnsNameAddressSets, err := oc.egressFirewallDNS.Add(ef.namespace, rule.to.dnsName)
			if err != nil {
				return fmt.Errorf("error with EgressFirewallDNS - %v", err)
			}
			if dnsNameAddressSets.GetIPv4HashName() != "" {
				matchTargets = append(matchTargets, matchTarget{matchKindV4AddressSet, dnsNameAddressSets.GetIPv4HashName()})
			}
			if dnsNameAddressSets.GetIPv6HashName() != "" {
				matchTargets = append(matchTargets, matchTarget{matchKindV6AddressSet, dnsNameAddressSets.GetIPv6HashName()})
			}
		}
		match := generateMatch(hashedAddressSetNameIPv4, hashedAddressSetNameIPv6, matchTargets, rule.ports)
		err := createLogicalRouterPolicy(efStartPriority-rule.id, match, action, ef.namespace)
		if err != nil {
			return err
		}
	}
	return nil
}

// createLogicalRouterPolicy uses the previously generated elements and creates the logical_router_policy
// for a specific egressFirewallRouter
func createLogicalRouterPolicy(priority int, match, action, namespace string) error {
	_, stderr, err := util.RunOVNNbctl("--id=@logical_router_policy", "create", "logical_router_policy",
		fmt.Sprintf("priority=%d", priority),
		match, "action="+action, fmt.Sprintf("external-ids:egressFirewall=%s", namespace),
		"--", "add", "logical_router", types.OVNClusterRouter, "policies", "@logical_router_policy")
	if err != nil {
		// TODO: lr-policy-add doesn't support --may-exist, resort to this workaround for now.
		// Have raised an issue against ovn repository (https://github.com/ovn-org/ovn/issues/49)
		if !strings.Contains(stderr, "already existed") {
			return fmt.Errorf("failed to add policy route '%s' to %s "+
				"stderr: %s, error: %v", match, types.OVNClusterRouter, stderr, err)
		}
	}
	return nil
}

type matchTarget struct {
	kind  matchKind
	value string
}

type matchKind int

const (
	matchKindV4CIDR matchKind = iota
	matchKindV6CIDR
	matchKindV4AddressSet
	matchKindV6AddressSet
)

func (m *matchTarget) toExpr() (string, error) {
	switch m.kind {
	case matchKindV4CIDR:
		return fmt.Sprintf("ip4.dst == %s", m.value), nil
	case matchKindV6CIDR:
		return fmt.Sprintf("ip6.dst == %s", m.value), nil
	case matchKindV4AddressSet:
		if m.value != "" {
			return fmt.Sprintf("ip4.dst == $%s", m.value), nil
		}
		return "", nil
	case matchKindV6AddressSet:
		if m.value != "" {
			return fmt.Sprintf("ip6.dst == $%s", m.value), nil
		}
		return "", nil
	}
	return "", fmt.Errorf("invalid MatchKind")
}

// generateMatch generates the "match" section of ACL generation for egressFirewallRules.
// It is referentially transparent as all the elements have been validated before this function is called
// sample output:
// match=\"(ip4.dst == 1.2.3.4/32) && ip4.src == $testv4 && ip4.dst != 10.128.0.0/14\
func generateMatch(ipv4Source, ipv6Source string, destinations []matchTarget, dstPorts []egressfirewallapi.EgressFirewallPort) string {
	var src string
	var dst string
	switch {
	case len(ipv4Source) > 0 && len(ipv6Source) > 0:
		src = fmt.Sprintf("(ip4.src == $%s || ip6.src == $%s)", ipv4Source, ipv6Source)
	case len(ipv4Source) > 0:
		src = fmt.Sprintf("ip4.src == $%s", ipv4Source)
	case len(ipv6Source) > 0:
		src = fmt.Sprintf("ip6.src == $%s", ipv6Source)
	}

	for _, entry := range destinations {
		if entry.value == "" {
			continue
		}
		ipDst, err := entry.toExpr()
		if err != nil {
			klog.Error(err)
			continue
		}
		if dst == "" {
			dst = ipDst
		} else {
			dst = strings.Join([]string{dst, ipDst}, " || ")
		}
	}

	match := fmt.Sprintf("match=\"(%s) && %s", dst, src)
	if len(dstPorts) > 0 {
		match = fmt.Sprintf("%s && %s", match, egressGetL4Match(dstPorts))
	}

	return fmt.Sprintf("%s && %s\"", match, getClusterSubnetsExclusion())
}

// egressGetL4Match generates the rules for when ports are specified in an egressFirewall Rule
// since the ports can be specified in any order in an egressFirewallRule the best way to build up
// a single rule is to build up each protocol as you walk through the list and place the appropriate logic
// between the elements.
func egressGetL4Match(ports []egressfirewallapi.EgressFirewallPort) string {
	var udpString string
	var tcpString string
	var sctpString string
	for _, port := range ports {
		if kapi.Protocol(port.Protocol) == kapi.ProtocolUDP && udpString != "udp" {
			if port.Port == 0 {
				udpString = "udp"
			} else {
				udpString = fmt.Sprintf("%s udp.dst == %d ||", udpString, port.Port)
			}
		} else if kapi.Protocol(port.Protocol) == kapi.ProtocolTCP && tcpString != "tcp" {
			if port.Port == 0 {
				tcpString = "tcp"
			} else {
				tcpString = fmt.Sprintf("%s tcp.dst == %d ||", tcpString, port.Port)
			}
		} else if kapi.Protocol(port.Protocol) == kapi.ProtocolSCTP && sctpString != "sctp" {
			if port.Port == 0 {
				sctpString = "sctp"
			} else {
				sctpString = fmt.Sprintf("%s sctp.dst == %d ||", sctpString, port.Port)
			}
		}
	}
	// build the l4 match
	var l4Match string
	type tuple struct {
		protocolName     string
		protocolFormated string
	}
	list := []tuple{
		{
			protocolName:     "udp",
			protocolFormated: udpString,
		},
		{
			protocolName:     "tcp",
			protocolFormated: tcpString,
		},
		{
			protocolName:     "sctp",
			protocolFormated: sctpString,
		},
	}
	for _, entry := range list {
		if entry.protocolName == entry.protocolFormated {
			if l4Match == "" {
				l4Match = fmt.Sprintf("(%s)", entry.protocolName)
			} else {
				l4Match = fmt.Sprintf("%s || (%s)", l4Match, entry.protocolName)
			}
		} else {
			if l4Match == "" && entry.protocolFormated != "" {
				l4Match = fmt.Sprintf("(%s && (%s))", entry.protocolName, entry.protocolFormated[:len(entry.protocolFormated)-2])
			} else if entry.protocolFormated != "" {
				l4Match = fmt.Sprintf("%s || (%s && (%s))", l4Match, entry.protocolName, entry.protocolFormated[:len(entry.protocolFormated)-2])
			}
		}
	}
	return fmt.Sprintf("(%s)", l4Match)
}

func getClusterSubnetsExclusion() string {
	var exclusion string
	for _, clusterSubnet := range config.Default.ClusterSubnets {
		if exclusion != "" {
			exclusion += " && "
		}
		if utilnet.IsIPv6CIDR(clusterSubnet.CIDR) {
			exclusion += fmt.Sprintf("%s.dst != %s", "ip6", clusterSubnet.CIDR)
		} else {
			exclusion += fmt.Sprintf("%s.dst != %s", "ip4", clusterSubnet.CIDR)
		}
	}
	return exclusion
}
