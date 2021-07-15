package ovn

import (
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/pkg/errors"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	egressfirewallapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
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

// NOTE: Utilize the fact that we know that all egress firewall related setup must have a priority: types.MinimumReservedEgressFirewallPriority <= priority <= types.EgressFirewallStartPriority
func (oc *Controller) syncEgressFirewall(egressFirwalls []interface{}) {
	if config.Gateway.Mode == config.GatewayModeShared {
		// Mode is shared gateway mode, make sure to delete all ACLs on the node switches
		egressFirewallACLIDs, stderr, err := util.RunOVNNbctl(
			"--data=bare",
			"--no-heading",
			"--columns=_uuid",
			"--format=table",
			"find",
			"acl",
			fmt.Sprintf("priority<=%s", types.EgressFirewallStartPriority),
			fmt.Sprintf("priority>=%s", types.MinimumReservedEgressFirewallPriority),
		)
		if err != nil {
			klog.Errorf("Unable to list egress firewall logical router policies, cannot cleanup old stale data, stderr: %s, err: %v", stderr, err)
			return
		}
		if egressFirewallACLIDs != "" {
			nodes, err := oc.watchFactory.GetNodes()
			if err != nil {
				klog.Errorf("Unable to cleanup egress firewall ACLs remaining from local gateway mode, cannot list nodes, err: %v", err)
				return
			}
			logicalSwitches := []string{}
			for _, node := range nodes {
				logicalSwitches = append(logicalSwitches, node.Name)
			}
			for _, logicalSwitch := range logicalSwitches {
				switchACLs, stderr, err := util.RunOVNNbctl(
					"--data=bare",
					"--no-heading",
					"--columns=acls",
					"list",
					"logical_switch",
					logicalSwitch,
				)
				if err != nil {
					klog.Errorf("Unable to remove egress firewall acl, cannot list ACLs on switch: %s, stderr: %s, err: %v", logicalSwitch, stderr, err)
				}
				for _, egressFirewallACLID := range strings.Split(egressFirewallACLIDs, "\n") {
					if strings.Contains(switchACLs, egressFirewallACLID) {
						_, stderr, err := util.RunOVNNbctl(
							"remove",
							"logical_switch",
							logicalSwitch,
							"acls",
							egressFirewallACLID,
						)
						if err != nil {
							klog.Errorf("Unable to remove egress firewall acl: %s on %s, cannot cleanup old stale data, stderr: %s, err: %v", egressFirewallACLID, logicalSwitch, stderr, err)
						}
					}
				}
			}
		}
	}
	egressFirewallACLIDs, stderr, err := util.RunOVNNbctl(
		"--data=bare",
		"--no-heading",
		"--columns=_uuid",
		"--format=table",
		"find",
		"acl",
		fmt.Sprintf("priority<=%s", types.EgressFirewallStartPriority),
		fmt.Sprintf("priority>=%s", types.MinimumReservedEgressFirewallPriority),
		fmt.Sprintf("direction=%s", types.DirectionFromLPort),
	)
	if err != nil {
		klog.Errorf("Unable to list egress firewall logical router policies, cannot convert old ACL data, stderr: %s, err: %v", stderr, err)
		return
	}
	if egressFirewallACLIDs != "" {
		for _, egressFirewallACLID := range strings.Split(egressFirewallACLIDs, "\n") {
			_, stderr, err := util.RunOVNNbctl(
				"set",
				"acl",
				egressFirewallACLID,
				fmt.Sprintf("direction=%s", types.DirectionToLPort),
			)
			if err != nil {
				klog.Errorf("Unable to set ACL direction on egress firewall acl: %s, cannot convert old ACL data, stderr: %s, err: %v", egressFirewallACLID, stderr, err)
			}
		}
	}
	// In any gateway mode, make sure to delete all LRPs on ovn_cluster_router.
	// This covers old local GW mode -> shared GW and old local GW mode -> new local GW mode
	egressFirewallPolicyIDs, stderr, err := util.RunOVNNbctl(
		"--data=bare",
		"--no-heading",
		"--columns=_uuid",
		"--format=table",
		"find",
		"logical_router_policy",
		fmt.Sprintf("priority<=%s", types.EgressFirewallStartPriority),
		fmt.Sprintf("priority>=%s", types.MinimumReservedEgressFirewallPriority),
	)
	if err != nil {
		klog.Errorf("Unable to list egress firewall logical router policies, cannot cleanup old stale data, stderr: %s, err: %v", stderr, err)
		return
	}
	if egressFirewallPolicyIDs != "" {
		for _, egressFirewallPolicyID := range strings.Split(egressFirewallPolicyIDs, "\n") {
			_, stderr, err := util.RunOVNNbctl(
				"remove",
				"logical_router",
				types.OVNClusterRouter,
				"policies",
				egressFirewallPolicyID,
			)
			if err != nil {
				klog.Errorf("Unable to remove egress firewall policy: %s on %s, cannot cleanup old stale data, stderr: %s, err: %v", egressFirewallPolicyID, types.OVNClusterRouter, stderr, err)
			}
		}
	}

	// sync the ovn and k8s egressFirewall states
	ovnEgressFirewallExternalIDs, stderr, err := util.RunOVNNbctl(
		"--data=bare",
		"--no-heading",
		"--columns=external_id",
		"--format=table",
		"find",
		"acl",
		fmt.Sprintf("priority<=%s", types.EgressFirewallStartPriority),
		fmt.Sprintf("priority>=%s", types.MinimumReservedEgressFirewallPriority),
	)
	if err != nil {
		klog.Errorf("Cannot reconcile the state of egressfirewalls in ovn database and k8s. stderr: %s, err: %v", stderr, err)
	}
	splitOVNEgressFirewallExternalIDs := strings.Split(ovnEgressFirewallExternalIDs, "\n")

	// represents the namespaces that have firewalls according to  ovn
	ovnEgressFirewalls := make(map[string]struct{})

	for _, externalID := range splitOVNEgressFirewallExternalIDs {
		if strings.Contains(externalID, "egressFirewall=") {
			// Most egressFirewalls will have more then one ACL but we only need to know if there is one for the namespace
			// so a map is fine and we will add an entry every iteration but because it is a map will overwrite the previous
			// entry if it already existed
			ovnEgressFirewalls[strings.Split(externalID, "egressFirewall=")[1]] = struct{}{}
		}
	}

	// get all the k8s EgressFirewall Objects
	egressFirewallList, err := oc.kube.GetEgressFirewalls()
	if err != nil {
		klog.Errorf("Cannot reconcile the state of egressfirewalls in ovn database and k8s. err: %v", err)
	}
	// delete entries from the map that exist in k8s and ovn
	txn := util.NewNBTxn()
	for _, egressFirewall := range egressFirewallList.Items {
		delete(ovnEgressFirewalls, egressFirewall.Namespace)
	}
	// any that are left are spurious and should be cleaned up
	for spuriousEF := range ovnEgressFirewalls {
		err := oc.deleteEgressFirewallRules(spuriousEF, txn)
		if err != nil {
			klog.Errorf("Cannot fully reconcile the state of egressfirewalls ACLs for namespace %s still exist in ovn db: %v", spuriousEF, err)
			return
		}
		_, stderr, err := txn.Commit()
		if err != nil {
			klog.Errorf("Cannot fully reconcile the state of egressfirewalls ACLs that still exist in ovn db: stderr: %q, err: %+v", stderr, err)
		}

	}
}

func (oc *Controller) addEgressFirewall(egressFirewall *egressfirewallapi.EgressFirewall, txn *util.NBTxn) error {
	klog.Infof("Adding egressFirewall %s in namespace %s", egressFirewall.Name, egressFirewall.Namespace)

	ef := cloneEgressFirewall(egressFirewall)
	ef.Lock()
	defer ef.Unlock()
	// there should not be an item already in egressFirewall map for the given Namespace
	if _, loaded := oc.egressFirewalls.LoadOrStore(egressFirewall.Namespace, ef); loaded {
		return fmt.Errorf("error attempting to add egressFirewall %s to namespace %s when it already has an egressFirewall",
			egressFirewall.Name, egressFirewall.Namespace)
	}

	var addErrors error
	egressFirewallStartPriorityInt, err := strconv.Atoi(types.EgressFirewallStartPriority)
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

	// EgressFirewall needs to make sure that the address_set for the namespace exists independently of the namespace object
	// so that OVN doesn't get unresolved references to the address_set.
	// TODO: This should go away once we do something like refcounting for address_sets.
	err = oc.addressSetFactory.EnsureAddressSet(egressFirewall.Namespace)
	if err != nil {
		return fmt.Errorf("cannot Ensure that addressSet for namespace %s exists %v", egressFirewall.Namespace, err)
	}
	ipv4HashedAS, ipv6HashedAS := addressset.MakeAddressSetHashNames(egressFirewall.Namespace)
	err = oc.addEgressFirewallRules(ef, ipv4HashedAS, ipv6HashedAS, egressFirewallStartPriorityInt, txn)
	if err != nil {
		return err
	}

	return nil
}

func (oc *Controller) updateEgressFirewall(oldEgressFirewall, newEgressFirewall *egressfirewallapi.EgressFirewall, txn *util.NBTxn) error {
	updateErrors := oc.deleteEgressFirewall(oldEgressFirewall, txn)
	if updateErrors != nil {
		return updateErrors
	}
	updateErrors = oc.addEgressFirewall(newEgressFirewall, txn)
	return updateErrors
}

func (oc *Controller) deleteEgressFirewall(egressFirewallObj *egressfirewallapi.EgressFirewall, txn *util.NBTxn) error {
	klog.Infof("Deleting egress Firewall %s in namespace %s", egressFirewallObj.Name, egressFirewallObj.Namespace)
	deleteDNS := false
	obj, loaded := oc.egressFirewalls.LoadAndDelete(egressFirewallObj.Namespace)
	if !loaded {
		return fmt.Errorf("there is no egressFirewall found in namespace %s", egressFirewallObj.Namespace)
	}

	ef, _ := obj.(egressFirewall)

	ef.Lock()
	defer ef.Unlock()
	for _, rule := range ef.egressRules {
		if len(rule.to.dnsName) > 0 {
			deleteDNS = true
			break
		}
	}
	if deleteDNS {
		oc.egressFirewallDNS.Delete(egressFirewallObj.Namespace)
	}

	return oc.deleteEgressFirewallRules(egressFirewallObj.Namespace, txn)
}

func (oc *Controller) updateEgressFirewallWithRetry(egressfirewall *egressfirewallapi.EgressFirewall) error {
	retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		return oc.kube.UpdateEgressFirewall(egressfirewall)
	})
	if retryErr != nil {
		return fmt.Errorf("error in updating status on EgressFirewall %s/%s: %v",
			egressfirewall.Namespace, egressfirewall.Name, retryErr)
	}
	return nil
}

func (oc *Controller) addEgressFirewallRules(ef *egressFirewall, hashedAddressSetNameIPv4, hashedAddressSetNameIPv6 string, efStartPriority int, txn *util.NBTxn) error {
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
		err := oc.createEgressFirewallRules(efStartPriority-rule.id, match, action, ef.namespace, txn)
		if err != nil {
			return err
		}
	}
	return nil
}

// createEgressFirewallRules uses the previously generated elements and creates the
// logical_router_policy/join_switch_acl for a specific egressFirewallRouter
func (oc *Controller) createEgressFirewallRules(priority int, match, action, externalID string, txn *util.NBTxn) error {
	logicalSwitches := []string{}
	if config.Gateway.Mode == config.GatewayModeLocal {
		nodes, err := oc.watchFactory.GetNodes()
		if err != nil {
			return fmt.Errorf("unable to setup egress firewall ACLs on cluster nodes, err: %v", err)
		}
		for _, node := range nodes {
			logicalSwitches = append(logicalSwitches, node.Name)
		}
	} else {
		logicalSwitches = append(logicalSwitches, types.OVNJoinSwitch)
	}
	uuids, stderr, err := util.RunOVNNbctl("--data=bare", "--no-heading",
		"--columns=_uuid", "find", "ACL", match, "action="+action,
		fmt.Sprintf("external-ids:egressFirewall=%s", externalID))
	if err != nil {
		return fmt.Errorf("error executing find ACL command, stderr: %q, %+v", stderr, err)
	}
	sort.Strings(logicalSwitches)
	for _, logicalSwitch := range logicalSwitches {
		if uuids == "" {
			id := fmt.Sprintf("%s-%d", logicalSwitch, priority)
			_, stderr, err := txn.AddOrCommit([]string{"--id=@" + id, "create", "acl",
				fmt.Sprintf("priority=%d", priority),
				fmt.Sprintf("direction=%s", types.DirectionToLPort), match, "action=" + action,
				fmt.Sprintf("external-ids:egressFirewall=%s", externalID),
				"--", "add", "logical_switch", logicalSwitch,
				"acls", "@" + id})
			if err != nil {
				return fmt.Errorf("failed to commit db changes for egressFirewall  stderr: %q, err: %+v", stderr, err)

			}

		} else {
			for _, uuid := range strings.Split(uuids, "\n") {
				_, stderr, err := txn.AddOrCommit([]string{"add", "logical_switch", logicalSwitch, "acls", uuid})
				if err != nil {
					return fmt.Errorf("failed to commit db changes for egressFirewall stderr: %q, err: %+v", stderr, err)
				}
			}
		}
	}
	return nil
}

// deleteEgressFirewallRules delete the specific logical router policy/join switch Acls
func (oc *Controller) deleteEgressFirewallRules(externalID string, txn *util.NBTxn) error {
	logicalSwitches := []string{}
	if config.Gateway.Mode == config.GatewayModeLocal {
		nodes, err := oc.watchFactory.GetNodes()
		if err != nil {
			return fmt.Errorf("unable to setup egress firewall ACLs on cluster nodes, err: %v", err)
		}
		for _, node := range nodes {
			logicalSwitches = append(logicalSwitches, node.Name)
		}
	} else {
		logicalSwitches = []string{types.OVNJoinSwitch}
	}
	sort.Strings(logicalSwitches)
	stdout, stderr, err := util.RunOVNNbctl("--data=bare", "--no-heading", "--columns=_uuid", "find", "ACL",
		fmt.Sprintf("external-ids:egressFirewall=%s", externalID))
	if err != nil {
		return fmt.Errorf("error deleting egressFirewall with external-ids %s, cannot get ACL policies - %s:%s",
			externalID, err, stderr)
	}
	uuids := strings.Fields(stdout)
	for _, logicalSwitch := range logicalSwitches {
		for _, uuid := range uuids {
			_, stderr, err = txn.AddOrCommit([]string{"remove", "logical_switch", logicalSwitch, "acls", uuid})
			if err != nil {
				return fmt.Errorf("failed to commit db changes for egressFirewall stderr: %q, err: %+v", stderr, err)
			}
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

	match := fmt.Sprintf("match=\"(%s) && %s", dst, src)
	if len(dstPorts) > 0 {
		match = fmt.Sprintf("%s && %s", match, egressGetL4Match(dstPorts))
	}

	if config.Gateway.Mode == config.GatewayModeLocal {
		extraMatch = getClusterSubnetsExclusion()
	} else {
		extraMatch = fmt.Sprintf("inport == \\\"%s%s\\\"", types.JoinSwitchToGWRouterPrefix, types.OVNClusterRouter)
	}
	return fmt.Sprintf("%s && %s\"", match, extraMatch)
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
