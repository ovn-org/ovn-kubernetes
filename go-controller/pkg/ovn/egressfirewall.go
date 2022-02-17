package ovn

import (
	"fmt"
	"net"
	"strings"
	"sync"

	"github.com/pkg/errors"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	egressfirewallapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"

	kapi "k8s.io/api/core/v1"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
)

const (
	egressFirewallAppliedCorrectly = "EgressFirewall Rules applied"
	egressFirewallAddError         = "EgressFirewall Rules not correctly added"
)

type egressFirewall struct {
	sync.Mutex
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

// cloneEgressFirewall shallow copies the egressfirewallapi.EgressFirewall object provided.
// This concretely means that it create a new egressfirewallapi.EgressFirewall with the name and
// namespace set, but without any rules specified.
func cloneEgressFirewall(originalEgressfirewall *egressfirewallapi.EgressFirewall) *egressFirewall {
	ef := &egressFirewall{
		name:        originalEgressfirewall.Name,
		namespace:   originalEgressfirewall.Namespace,
		egressRules: make([]*egressFirewallRule, 0),
	}
	return ef
}

// newEgressFirewallRule creates a new egressFirewallRule. For the logging level, it will pick either of
// aclLoggingAllow or aclLoggingDeny depending if this is an allow or deny rule.
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

// This function is used to sync egress firewall setup. It does three "cleanups"

// - 	Cleanup the old implementation (using LRP) in local GW mode -> new implementation (using ACLs) local GW mode
//  	For this it just deletes all LRP setup done for egress firewall
//  	And also convert all old ACLs which specifed from-lport to specifying to-lport

// -	Cleanup the new local GW mode implementation (using ACLs on the node switch) -> shared GW mode implementation (using ACLs on the join switch)
//  	For this it just deletes all ACL setup done for egress firewall on the node switches

// -	Cleanup the old implementation (using LRP) in local GW mode -> shared GW mode implementation (using ACLs on the join switch)
//  	For this it just deletes all LRP setup done for egress firewall

// -    Cleanup for migration from shared GW mode -> local GW mode
//      For this it just deletes all the ACLs on the distributed join switch

// NOTE: Utilize the fact that we know that all egress firewall related setup must have a priority: types.MinimumReservedEgressFirewallPriority <= priority <= types.EgressFirewallStartPriority
// Upon failure, it may be invoked multiple times in order to avoid a pod restart.
func (oc *Controller) syncEgressFirewall(egressFirewalls []interface{}) error {
	// Lookup all ACLs used for egress Firewalls
	aclPred := func(item *nbdb.ACL) bool {
		return item.Priority >= types.MinimumReservedEgressFirewallPriority && item.Priority <= types.EgressFirewallStartPriority
	}
	egressFirewallACLs, err := libovsdbops.FindACLsWithPredicate(oc.nbClient, aclPred)
	if err != nil {
		return fmt.Errorf("unable to list egress firewall ACLs, cannot cleanup old stale data, err: %v", err)
	}

	if len(egressFirewallACLs) != 0 {
		var p func(item *nbdb.LogicalSwitch) bool
		if config.Gateway.Mode == config.GatewayModeShared {
			p = func(item *nbdb.LogicalSwitch) bool {
				// Ignore external and Join switches(both legacy and current)
				return !(strings.HasPrefix(item.Name, types.JoinSwitchPrefix) || item.Name == "join" || strings.HasPrefix(item.Name, types.ExternalSwitchPrefix))
			}
		} else if config.Gateway.Mode == config.GatewayModeLocal {
			p = func(item *nbdb.LogicalSwitch) bool {
				// Return only join switch (the per node ones if its old topology & distributed one if its new topology)
				return (strings.HasPrefix(item.Name, types.JoinSwitchPrefix) || item.Name == "join")
			}
		}
		err = libovsdbops.RemoveACLsFromLogicalSwitchesWithPredicate(oc.nbClient, p, egressFirewallACLs...)
		if err != nil {
			return fmt.Errorf("failed to remove reject acl from node logical switches: %v", err)
		}
	}

	// Update the direction of each egressFirewallACL if needed.
	// Update the egressFirewallACL name if needed.
	// Update logging related information if needed.
	for i := range egressFirewallACLs {
		egressFirewallACLs[i].Direction = types.DirectionToLPort
		if namespace, ok := egressFirewallACLs[i].ExternalIDs["egressFirewall"]; ok && namespace != "" {
			aclName := buildEgressFwAclName(namespace, egressFirewallACLs[i].Priority)
			log, meterName, logSeverity := oc.getLogMeterSeverity(namespace, egressFirewallACLs[i].Action)
			egressFirewallACLs[i].Meter = &meterName
			egressFirewallACLs[i].Name = &aclName
			egressFirewallACLs[i].Log = log
			egressFirewallACLs[i].Severity = &logSeverity
		} else {
			klog.Warningf("Could not find namespace for egress firewall ACL during refresh operation: %v", egressFirewallACLs[i])
		}
	}
	err = libovsdbops.CreateOrUpdateACLs(oc.nbClient, egressFirewallACLs...)
	if err != nil {
		return fmt.Errorf("unable to update ACL information (direction and logging) during resync operation, err: %v", err)
	}

	// In any gateway mode, make sure to delete all LRPs on ovn_cluster_router.
	// This covers migration from LGW mode that used LRPs for EFW to using ACLs in SGW/LGW modes
	p := func(item *nbdb.LogicalRouterPolicy) bool {
		return item.Priority <= types.EgressFirewallStartPriority && item.Priority >= types.MinimumReservedEgressFirewallPriority
	}
	err = libovsdbops.DeleteLogicalRouterPoliciesWithPredicate(oc.nbClient, types.OVNClusterRouter, p)
	if err != nil {
		return fmt.Errorf("error deleting egress firewall policies on router %s: %v", types.OVNClusterRouter, err)
	}

	// sync the ovn and k8s egressFirewall states
	// represents the namespaces that have firewalls according to  ovn
	ovnEgressFirewalls := make(map[string]struct{})

	for _, egressFirewallACL := range egressFirewallACLs {
		if ns, ok := egressFirewallACL.ExternalIDs["egressFirewall"]; ok {
			// Most egressFirewalls will have more then one ACL but we only need to know if there is one for the namespace
			// so a map is fine and we will add an entry every iteration but because it is a map will overwrite the previous
			// entry if it already existed
			ovnEgressFirewalls[ns] = struct{}{}
		}
	}

	// get all the k8s EgressFirewall Objects
	egressFirewallList, err := oc.kube.GetEgressFirewalls()
	if err != nil {
		return fmt.Errorf("cannot reconcile the state of egressfirewalls in ovn database and k8s. err: %v", err)
	}
	// delete entries from the map that exist in k8s and ovn
	for _, egressFirewall := range egressFirewallList.Items {
		delete(ovnEgressFirewalls, egressFirewall.Namespace)
	}
	// any that are left are spurious and should be cleaned up
	for spuriousEF := range ovnEgressFirewalls {
		err := oc.deleteEgressFirewallRules(spuriousEF)
		if err != nil {
			return fmt.Errorf("cannot fully reconcile the state of egressfirewalls ACLs for namespace %s still exist in ovn db: %v", spuriousEF, err)
		}
	}
	return nil
}

func (oc *Controller) addEgressFirewall(egressFirewall *egressfirewallapi.EgressFirewall) error {
	klog.Infof("Adding egressFirewall %s in namespace %s", egressFirewall.Name, egressFirewall.Namespace)

	ef := cloneEgressFirewall(egressFirewall)
	ef.Lock()
	defer ef.Unlock()
	// there should not be an item already in egressFirewall map for the given Namespace
	if _, loaded := oc.egressFirewalls.Load(egressFirewall.Namespace); loaded {
		return fmt.Errorf("error attempting to add egressFirewall %s to namespace %s when it already has an egressFirewall",
			egressFirewall.Name, egressFirewall.Namespace)
	}

	var addErrors error
	for i, egressFirewallRule := range egressFirewall.Spec.Egress {
		// process Rules into egressFirewallRules for egressFirewall struct
		if i > types.EgressFirewallStartPriority-types.MinimumReservedEgressFirewallPriority {
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

	// EgressFirewall needs to make sure that the address_set for the namespace exists independently of the namespace object
	// so that OVN doesn't get unresolved references to the address_set.
	// TODO: This should go away once we do something like refcounting for address_sets.
	_, err := oc.addressSetFactory.EnsureAddressSet(egressFirewall.Namespace)
	if err != nil {
		return fmt.Errorf("cannot Ensure that addressSet for namespace %s exists %v", egressFirewall.Namespace, err)
	}
	ipv4HashedAS, ipv6HashedAS := addressset.MakeAddressSetHashNames(egressFirewall.Namespace)
	if err := oc.addEgressFirewallRules(ef, ipv4HashedAS, ipv6HashedAS, types.EgressFirewallStartPriority); err != nil {
		return err
	}
	oc.egressFirewalls.Store(egressFirewall.Namespace, ef)
	return nil
}

func (oc *Controller) deleteEgressFirewall(egressFirewallObj *egressfirewallapi.EgressFirewall) error {
	klog.Infof("Deleting egress Firewall %s in namespace %s", egressFirewallObj.Name, egressFirewallObj.Namespace)
	deleteDNS := false
	obj, loaded := oc.egressFirewalls.Load(egressFirewallObj.Namespace)
	if !loaded {
		return fmt.Errorf("there is no egressFirewall found in namespace %s",
			egressFirewallObj.Namespace)
	}

	ef, ok := obj.(*egressFirewall)
	if !ok {
		return fmt.Errorf("deleteEgressFirewall failed: type assertion to *egressFirewall"+
			" failed for EgressFirewall %s of type %T in namespace %s",
			egressFirewallObj.Name, obj, egressFirewallObj.Namespace)
	}

	ef.Lock()
	defer ef.Unlock()
	for _, rule := range ef.egressRules {
		if len(rule.to.dnsName) > 0 {
			deleteDNS = true
			break
		}
	}
	if deleteDNS {
		if err := oc.egressFirewallDNS.Delete(egressFirewallObj.Namespace); err != nil {
			return err
		}
	}

	if err := oc.deleteEgressFirewallRules(egressFirewallObj.Namespace); err != nil {
		return err
	}
	oc.egressFirewalls.Delete(egressFirewallObj.Namespace)
	return nil
}

func (oc *Controller) updateEgressFirewallStatusWithRetry(egressfirewall *egressfirewallapi.EgressFirewall) error {
	retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		return oc.kube.UpdateEgressFirewall(egressfirewall)
	})
	if retryErr != nil {
		return fmt.Errorf("error in updating status on EgressFirewall %s/%s: %v",
			egressfirewall.Namespace, egressfirewall.Name, retryErr)
	}
	return nil
}

func (oc *Controller) addEgressFirewallRules(ef *egressFirewall, hashedAddressSetNameIPv4, hashedAddressSetNameIPv6 string, efStartPriority int) error {
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
			dnsNameIPv4ASHashName, dnsNameIPv6ASHashName := dnsNameAddressSets.GetASHashNames()
			if dnsNameIPv4ASHashName != "" {
				matchTargets = append(matchTargets, matchTarget{matchKindV4AddressSet, dnsNameIPv4ASHashName})
			}
			if dnsNameIPv6ASHashName != "" {
				matchTargets = append(matchTargets, matchTarget{matchKindV6AddressSet, dnsNameIPv6ASHashName})
			}
		}
		match := generateMatch(hashedAddressSetNameIPv4, hashedAddressSetNameIPv6, matchTargets, rule.ports)
		err := oc.createEgressFirewallRules(efStartPriority-rule.id, match, action, ef.namespace)
		if err != nil {
			return err
		}
	}
	return nil
}

// createEgressFirewallRules uses the previously generated elements and creates the
// logical_router_policy/join_switch_acl for a specific egressFirewallRouter
func (oc *Controller) createEgressFirewallRules(priority int, match, action, externalID string) error {
	logicalSwitches := []string{}
	if config.Gateway.Mode == config.GatewayModeLocal {
		// Find all node switches
		p := func(item *nbdb.LogicalSwitch) bool {
			// Ignore external and Join switches(both legacy and current)
			return !(strings.HasPrefix(item.Name, types.JoinSwitchPrefix) || item.Name == "join" || strings.HasPrefix(item.Name, types.ExternalSwitchPrefix))
		}
		nodeLocalSwitches, err := libovsdbops.FindLogicalSwitchesWithPredicate(oc.nbClient, p)
		if err != nil {
			return fmt.Errorf("unable to setup egress firewall ACLs on cluster nodes, err: %v", err)
		}
		for _, nodeLocalSwitch := range nodeLocalSwitches {
			logicalSwitches = append(logicalSwitches, nodeLocalSwitch.Name)
		}
	} else {
		logicalSwitches = append(logicalSwitches, types.OVNJoinSwitch)
	}

	// a name is needed for logging purposes - the name must be unique, so make it
	// egressFirewall_<namespace name>_<priority>
	aclName := buildEgressFwAclName(externalID, priority)
	log, meter, severity := oc.getLogMeterSeverity(externalID, action)

	egressFirewallACL := libovsdbops.BuildACL(
		aclName,
		types.DirectionToLPort,
		priority,
		match,
		action,
		meter,
		severity,
		log,
		map[string]string{"egressFirewall": externalID},
		nil,
	)
	ops, err := libovsdbops.CreateOrUpdateACLsOps(oc.nbClient, nil, egressFirewallACL)
	if err != nil {
		return fmt.Errorf("failed to create egressFirewall ACL %v: %v", egressFirewallACL, err)
	}

	for _, logicalSwitchName := range logicalSwitches {
		ops, err = libovsdbops.AddACLsToLogicalSwitchOps(oc.nbClient, ops, logicalSwitchName, egressFirewallACL)
		if err != nil {
			return fmt.Errorf("failed to add egressFirewall ACL %v to switch %s: %v", egressFirewallACL, logicalSwitchName, err)
		}
	}

	_, err = libovsdbops.TransactAndCheck(oc.nbClient, ops)
	if err != nil {
		return fmt.Errorf("failed to transact egressFirewall ACL: %v", err)
	}

	return nil
}

// deleteEgressFirewallRules delete the specific logical router policy/join switch Acls
func (oc *Controller) deleteEgressFirewallRules(externalID string) error {
	// Find ACLs for a given egressFirewall
	pACL := func(item *nbdb.ACL) bool {
		return item.ExternalIDs["egressFirewall"] == externalID
	}
	egressFirewallACLs, err := libovsdbops.FindACLsWithPredicate(oc.nbClient, pACL)
	if err != nil {
		return fmt.Errorf("unable to list egress firewall ACLs, cannot cleanup old stale data, err: %v", err)
	}

	if len(egressFirewallACLs) == 0 {
		klog.Warningf("No egressFirewall ACLs to delete in ns: %s", externalID)
		return nil
	}

	// delete egress firewall acls off any logical switch which has it
	pSwitch := func(item *nbdb.LogicalSwitch) bool { return true }
	err = libovsdbops.RemoveACLsFromLogicalSwitchesWithPredicate(oc.nbClient, pSwitch, egressFirewallACLs...)
	if err != nil {
		return fmt.Errorf("failed to remove reject acl from logical switches: %v", err)
	}

	// Manually remove the egressFirewall ACLs instead of relying on ovsdb garbage collection to do so
	err = libovsdbops.DeleteACLs(oc.nbClient, egressFirewallACLs...)
	if err != nil {
		return err
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
	var extraMatch string
	switch {
	case config.IPv4Mode && config.IPv6Mode:
		src = fmt.Sprintf("(ip4.src == $%s || ip6.src == $%s)", ipv4Source, ipv6Source)
	case config.IPv4Mode:
		src = fmt.Sprintf("ip4.src == $%s", ipv4Source)
	case config.IPv6Mode:
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

	match := fmt.Sprintf("(%s) && %s", dst, src)
	if len(dstPorts) > 0 {
		match = fmt.Sprintf("%s && %s", match, egressGetL4Match(dstPorts))
	}

	if config.Gateway.Mode == config.GatewayModeLocal {
		extraMatch = getClusterSubnetsExclusion()
	} else {
		extraMatch = fmt.Sprintf("inport == \"%s%s\"", types.JoinSwitchToGWRouterPrefix, types.OVNClusterRouter)
	}
	return fmt.Sprintf("%s && %s", match, extraMatch)
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

func getEgressFirewallNamespacedName(egressFirewall *egressfirewallapi.EgressFirewall) string {
	return fmt.Sprintf("%v/%v", egressFirewall.Namespace, egressFirewall.Name)
}

// getLogMeterSeverity determines if logging shall be enabled for a given ACL, what the name of the meter is and the
// severity level for logging.
func (oc *Controller) getLogMeterSeverity(namespaceName, action string) (log bool, meter string, severity string) {
	aclLogging := ""
	// ok should always be true. However, to avoid nil pointer derefence panics here, it's
	// best to add this verification and in the worst case to resort to sane default values.
	if nsInfo, ok := oc.namespaces[namespaceName]; ok {
		if action == "allow" || action == "allow-related" {
			aclLogging = nsInfo.aclLogging.Allow
		} else if action == "drop" || action == "drop-related" {
			aclLogging = nsInfo.aclLogging.Deny
		}
	} else {
		klog.Warningf("No nsInfo object found for namespace %s", namespaceName)
	}
	log = aclLogging != ""
	severity = getACLLoggingSeverity(aclLogging)
	meter = types.OvnACLLoggingMeter

	return
}

// refreshEgressFirewallLogging updates logging related configuration for all rules of this specific firewall in OVN.
// This method can be called for example from the Namespaces Watcher methods to reload firewall rules' logging  when
// namespace annotations change.
// Return values are: bool - if the egressFirewall's ACL was updated or not, error in case of errors. If a namespace
// does not contain an egress firewall ACL, then this returns false, nil instead of a NotFound error.
func (oc *Controller) refreshEgressFirewallLogging(egressFirewallNamespace string) (bool, error) {
	// Retrieve the egress firewall object from cache and lock it.
	obj, loaded := oc.egressFirewalls.Load(egressFirewallNamespace)
	if !loaded {
		return false, nil
	}

	ef, ok := obj.(*egressFirewall)
	if !ok {
		return false, fmt.Errorf("refreshEgressFirewallLogging failed: type assertion to *egressFirewall"+
			" failed for EgressFirewall of type %T in namespace %s",
			obj, ef.namespace)
	}

	ef.Lock()
	defer ef.Unlock()

	// Find ACLs for a given egressFirewall
	p := func(item *nbdb.ACL) bool {
		return item.ExternalIDs["egressFirewall"] == ef.namespace
	}
	egressFirewallACLs, err := libovsdbops.FindACLsWithPredicate(oc.nbClient, p)
	if err != nil {
		return false, fmt.Errorf("unable to list egress firewall ACLs, err: %v", err)
	}
	if len(egressFirewallACLs) == 0 {
		klog.Warningf("No egressFirewall ACLs to update in ns: %s", ef.namespace)
		return false, nil
	}

	for i := range egressFirewallACLs {
		// Set logging and severity
		log, meterName, logSeverity := oc.getLogMeterSeverity(ef.namespace, egressFirewallACLs[i].Action)
		egressFirewallACLs[i].Log = log
		egressFirewallACLs[i].Severity = &logSeverity
		egressFirewallACLs[i].Meter = &meterName
	}
	// CreateOrUpdateACLs will update all provided (non zero value) fields
	err = libovsdbops.CreateOrUpdateACLs(oc.nbClient, egressFirewallACLs...)
	if err != nil {
		return false, fmt.Errorf("unable to update ACL logging in ns %s, err: %v", ef.namespace, err)
	}

	return true, nil
}

func buildEgressFwAclName(namespace string, priority int) string {
	return fmt.Sprintf("egressFirewall_%s_%d", namespace, priority)
}
