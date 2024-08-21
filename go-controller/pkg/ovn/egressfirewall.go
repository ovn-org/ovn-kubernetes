package ovn

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	libovsdb "github.com/ovn-org/libovsdb/ovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/controller"
	egressfirewallapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1"
	egressfirewallapply "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/applyconfiguration/egressfirewall/v1"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/batching"
	utilerrors "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/errors"

	kapi "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
)

const (
	egressFirewallAppliedCorrectly = "EgressFirewall Rules applied"
	aclDeleteBatchSize             = 1000
	// transaction time to delete 80K ACLs from 2 port groups is ~4.5 sec.
	// transaction time to delete 80K acls from one port group and add them to another
	// is ~3 sec
	// Therefore this limit is safe enough to not exceed 10 sec transaction timeout.
	aclChangePGBatchSize = 80000
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
	// clusterSubnetIntersection is true, if egress firewall rule CIDRSelector intersects with clusterSubnet.
	// Based on this flag we can omit clusterSubnet exclusion from the related ACL.
	// For dns-based rules, EgressDNS won't add ips from clusterSubnet to the address set.
	clusterSubnetIntersection bool
	// nodeName: nodeIPs
	nodeAddrs    map[string][]string
	nodeSelector *metav1.LabelSelector
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
func (oc *DefaultNetworkController) newEgressFirewallRule(rawEgressFirewallRule egressfirewallapi.EgressFirewallRule, id int) (*egressFirewallRule, error) {
	efr := &egressFirewallRule{
		id:     id,
		access: rawEgressFirewallRule.Type,
	}

	// Validate the egress firewall rule destination and update the appropriate
	// fields of efr.
	var err error
	efr.to.cidrSelector, efr.to.dnsName, efr.to.clusterSubnetIntersection, efr.to.nodeSelector, err =
		util.ValidateAndGetEgressFirewallDestination(rawEgressFirewallRule.To)
	if err != nil {
		return efr, err
	}
	// If nodeSelector is set then fetch the node addresses.
	if efr.to.nodeSelector != nil {
		efr.to.nodeAddrs = map[string][]string{}
		nodes, err := oc.watchFactory.GetNodesByLabelSelector(*rawEgressFirewallRule.To.NodeSelector)
		if err != nil {
			return efr, fmt.Errorf("unable to query nodes for egress firewall: %w", err)
		}
		for _, node := range nodes {
			hostAddresses, err := util.GetNodeHostAddrs(node)
			if err != nil {
				return efr, fmt.Errorf("unable to get node host CIDRs for egress firewall, node: %s: %w", node.Name, err)
			}
			efr.to.nodeAddrs[node.Name] = hostAddresses
		}
	}
	efr.ports = rawEgressFirewallRule.Ports

	return efr, nil
}

// syncEgressFirewall deletes stale db entries for previous versions of Egress Firewall implementation and removes
// stale db entries for Egress Firewalls that don't exist anymore.
// Egress firewall implementation had many versions, the latest one makes no difference for gateway modes, and creates
// ACLs on namespaced port groups.
func (oc *DefaultNetworkController) syncEgressFirewall(egressFirewalls []interface{}) error {
	err := oc.deleteStaleACLs()
	if err != nil {
		return err
	}

	existingEFNamespaces := map[string]bool{}
	for _, efInterface := range egressFirewalls {
		ef, ok := efInterface.(*egressfirewallapi.EgressFirewall)
		if !ok {
			return fmt.Errorf("spurious object in syncEgressFirewall: %v", efInterface)
		}
		existingEFNamespaces[ef.Namespace] = true
	}

	// find all existing egress firewall ACLs
	predicateIDs := libovsdbops.NewDbObjectIDs(libovsdbops.ACLEgressFirewall, oc.controllerName, nil)
	aclP := libovsdbops.GetPredicate[*nbdb.ACL](predicateIDs, nil)
	efACLs, err := libovsdbops.FindACLsWithPredicate(oc.nbClient, aclP)
	if err != nil {
		return fmt.Errorf("cannot find Egress Firewall ACLs: %v", err)
	}

	// another sync to move ACLs to the right port group
	err = oc.moveACLsToNamespacedPortGroups(existingEFNamespaces, efACLs)
	if err != nil {
		return err
	}

	var deletedNSACLs = map[string][]*nbdb.ACL{}
	for _, acl := range efACLs {
		namespace := acl.ExternalIDs[libovsdbops.ObjectNameKey.String()]
		if !existingEFNamespaces[namespace] {
			deletedNSACLs[namespace] = append(deletedNSACLs[namespace], acl)
		}
	}

	err = batching.BatchMap[*nbdb.ACL](aclChangePGBatchSize, deletedNSACLs, func(batchNsACLs map[string][]*nbdb.ACL) error {
		var ops []libovsdb.Operation
		var err error
		for namespace, acls := range batchNsACLs {
			pgName := oc.getNamespacePortGroupName(namespace)
			// delete stale ACLs from namespaced port group
			// both port group and acls may not exist after moveACLsToNamespacedPortGroups,
			// but DeleteACLsFromPortGroupOps doesn't return error in these cases
			ops, err = libovsdbops.DeleteACLsFromPortGroupOps(oc.nbClient, ops, pgName, acls...)
			if err != nil {
				return fmt.Errorf("failed to build cleanup ops: %w", err)
			}
		}
		_, err = libovsdbops.TransactAndCheck(oc.nbClient, ops)
		if err != nil {
			return fmt.Errorf("failed to clean up egress firewall ACLs: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}

	// Delete stale address sets related to EgressFirewallDNS which are not referenced by any ACL.
	return oc.dnsNameResolver.DeleteStaleAddrSets(oc.nbClient)
}

// deleteStaleACLs cleans up 2 previous implementations:
//   - Cleanup the old implementation (using LRP) in local GW mode
//     For this it just deletes all LRP setup done for egress firewall
//   - Cleanup the old implementation (using ACLs on the join and node switches)
//     For this it deletes all the ACLs on the join and node switches, they will be created from scratch later.
func (oc *DefaultNetworkController) deleteStaleACLs() error {
	// In any gateway mode, make sure to delete all LRPs on ovn_cluster_router.
	p := func(item *nbdb.LogicalRouterPolicy) bool {
		return item.Priority <= types.EgressFirewallStartPriority && item.Priority >= types.MinimumReservedEgressFirewallPriority
	}
	err := libovsdbops.DeleteLogicalRouterPoliciesWithPredicate(oc.nbClient, oc.GetNetworkScopedClusterRouterName(), p)
	if err != nil {
		return fmt.Errorf("error deleting egress firewall policies on router %s: %v", oc.GetNetworkScopedClusterRouterName(), err)
	}

	// delete acls from all switches, they reside on the port group now
	// Lookup all ACLs used for egress Firewalls
	aclPred := func(item *nbdb.ACL) bool {
		return item.Priority >= types.MinimumReservedEgressFirewallPriority && item.Priority <= types.EgressFirewallStartPriority
	}
	egressFirewallACLs, err := libovsdbops.FindACLsWithPredicate(oc.nbClient, aclPred)
	if err != nil {
		return fmt.Errorf("unable to list egress firewall ACLs, cannot cleanup old stale data, err: %v", err)
	}
	if len(egressFirewallACLs) != 0 {
		err = batching.Batch[*nbdb.ACL](aclDeleteBatchSize, egressFirewallACLs, func(batchACLs []*nbdb.ACL) error {
			// optimize the predicate to exclude switches that don't reference deleting acls.
			aclsToDelete := sets.NewString()
			for _, acl := range batchACLs {
				aclsToDelete.Insert(acl.UUID)
			}
			swWithACLsPred := func(sw *nbdb.LogicalSwitch) bool {
				return aclsToDelete.HasAny(sw.ACLs...)
			}
			return libovsdbops.RemoveACLsFromLogicalSwitchesWithPredicate(oc.nbClient, swWithACLsPred, batchACLs...)
		})
		if err != nil {
			return fmt.Errorf("failed to remove egress firewall acls from node logical switches: %v", err)
		}
	}
	return nil
}

// moveACLsToNamespacedPortGroups syncs db from the previous version where all ACLs were attached to the ClusterPortGroup
// to the new version where ACLs are attached to the namespace port groups.
func (oc *DefaultNetworkController) moveACLsToNamespacedPortGroups(existingEFNamespaces map[string]bool, efACLs []*nbdb.ACL) error {
	// find stale ACLs attached to a cluster port group, and move them to namespaced port groups
	clusterPG, err := libovsdbops.GetPortGroup(oc.nbClient, &nbdb.PortGroup{
		Name: oc.getClusterPortGroupName(types.ClusterPortGroupNameBase),
	})
	if err != nil {
		return fmt.Errorf("failed to get cluster port gorup: %w", err)
	}
	staleUUIDs := sets.NewString(clusterPG.ACLs...)

	// move ACLs from cluster port group to per-namespace port group
	// old acl matches on the address set, which is still valid. match change from address set to port group will be
	// performed by the egress firewall handler.
	staleNamespaces := map[string][]*nbdb.ACL{}
	for _, acl := range efACLs {
		namespace := acl.ExternalIDs[libovsdbops.ObjectNameKey.String()]
		// no key will be treated as an empty namespace and ACLs will just be deleted
		if staleUUIDs.Has(acl.UUID) {
			staleNamespaces[namespace] = append(staleNamespaces[namespace], acl)
		}
	}
	if len(staleNamespaces) == 0 {
		return nil
	}

	err = batching.BatchMap[*nbdb.ACL](aclChangePGBatchSize, staleNamespaces, func(batchNsACLs map[string][]*nbdb.ACL) error {
		var ops []libovsdb.Operation
		var err error
		for namespace, acls := range batchNsACLs {
			if namespace != "" && existingEFNamespaces[namespace] {
				pgName := oc.getNamespacePortGroupName(namespace)
				// re-attach from ClusterPortGroupNameBase to namespaced port group.
				// port group should exist, because namespace handler will create it.
				ops, err = libovsdbops.AddACLsToPortGroupOps(oc.nbClient, ops, pgName, acls...)
				if err != nil {
					return fmt.Errorf("failed to build cleanup ops: %w", err)
				}
			}
			// delete all EF ACLs from ClusterPortGroupNameBase
			ops, err = libovsdbops.DeleteACLsFromPortGroupOps(oc.nbClient, ops,
				oc.getClusterPortGroupName(types.ClusterPortGroupNameBase), acls...)
			if err != nil {
				return fmt.Errorf("failed to build cleanup from ClusterPortGroup ops: %w", err)
			}
		}
		_, err = libovsdbops.TransactAndCheck(oc.nbClient, ops)
		if err != nil {
			return fmt.Errorf("failed to clean up egress firewall ACLs: %w", err)
		}
		return nil
	})
	return err
}

func (oc *DefaultNetworkController) addEgressFirewall(egressFirewall *egressfirewallapi.EgressFirewall) error {
	klog.Infof("Adding egressFirewall %s in namespace %s", egressFirewall.Name, egressFirewall.Namespace)

	ef := cloneEgressFirewall(egressFirewall)
	ef.Lock()
	defer ef.Unlock()
	// egressFirewall may already exist, if previous add failed, cleanup
	if _, loaded := oc.egressFirewalls.Load(egressFirewall.Namespace); loaded {
		klog.Infof("Egress firewall in namespace %s already exists, cleanup", egressFirewall.Namespace)
		err := oc.deleteEgressFirewall(egressFirewall)
		if err != nil {
			return fmt.Errorf("failed to cleanup existing egress firewall %s on add: %v", egressFirewall.Namespace, err)
		}
	}

	var errorList []error
	for i, egressFirewallRule := range egressFirewall.Spec.Egress {
		// process Rules into egressFirewallRules for egressFirewall struct
		if i > types.EgressFirewallStartPriority-types.MinimumReservedEgressFirewallPriority {
			errorList = append(errorList, fmt.Errorf("egressFirewall for namespace %s has too many rules, max allowed number is %v",
				egressFirewall.Namespace, types.EgressFirewallStartPriority-types.MinimumReservedEgressFirewallPriority))
			break
		}
		efr, err := oc.newEgressFirewallRule(egressFirewallRule, i)
		if err != nil {
			errorList = append(errorList, fmt.Errorf("cannot create EgressFirewall Rule to destination %s for namespace %s: %w",
				egressFirewallRule.To.CIDRSelector, egressFirewall.Namespace, err))
			continue

		}
		ef.egressRules = append(ef.egressRules, efr)
	}
	if len(errorList) > 0 {
		return utilerrors.Join(errorList...)
	}

	pgName := oc.getNamespacePortGroupName(egressFirewall.Namespace)
	aclLoggingLevels := oc.GetNamespaceACLLogging(ef.namespace)
	// store egress firewall before calling addEgressFirewallRules, since it doesn't have a cleanup, and oc.egressFirewalls
	// object will be used on retry to cleanup
	oc.egressFirewalls.Store(egressFirewall.Namespace, ef)
	if err := oc.addEgressFirewallRules(ef, pgName, aclLoggingLevels); err != nil {
		return err
	}
	return nil
}

func (oc *DefaultNetworkController) deleteEgressFirewall(egressFirewallObj *egressfirewallapi.EgressFirewall) error {
	klog.Infof("Deleting egress Firewall %s in namespace %s", egressFirewallObj.Name, egressFirewallObj.Namespace)
	deleteDNS := false
	obj, loaded := oc.egressFirewalls.Load(egressFirewallObj.Namespace)
	if !loaded {
		return nil
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
	// delete acls first, then dns address set that is referenced in these acls
	if err := oc.deleteEgressFirewallRules(egressFirewallObj.Namespace); err != nil {
		return err
	}
	if deleteDNS {
		if err := oc.dnsNameResolver.Delete(egressFirewallObj.Namespace); err != nil {
			return err
		}
	}
	oc.egressFirewalls.Delete(egressFirewallObj.Namespace)
	return nil
}

func (oc *DefaultNetworkController) addEgressFirewallRules(ef *egressFirewall, pgName string,
	aclLogging *libovsdbutil.ACLLoggingLevels, ruleIDs ...int) error {
	var ops []libovsdb.Operation
	var err error
	for _, rule := range ef.egressRules {
		// check if only specific rule ids are requested to be added
		if len(ruleIDs) > 0 {
			found := false
			for _, providedID := range ruleIDs {
				if rule.id == providedID {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}
		var action string
		var matchTargets []matchTarget
		if rule.access == egressfirewallapi.EgressFirewallRuleAllow {
			action = nbdb.ACLActionAllow
		} else {
			action = nbdb.ACLActionDrop
		}
		if len(rule.to.nodeAddrs) > 0 {
			// sort node ips to ensure the same order when no changes are present
			// this ensure ACL recalculation won't happen just because of the order changes
			allIPs := []string{}
			for _, nodeIPs := range rule.to.nodeAddrs {
				allIPs = append(allIPs, nodeIPs...)
			}
			slices.Sort(allIPs)

			for _, addr := range allIPs {
				if utilnet.IsIPv6String(addr) {
					matchTargets = append(matchTargets, matchTarget{matchKindV6CIDR, addr, false})
				} else {
					matchTargets = append(matchTargets, matchTarget{matchKindV4CIDR, addr, false})
				}
			}
		} else if rule.to.cidrSelector != "" {
			if utilnet.IsIPv6CIDRString(rule.to.cidrSelector) {
				matchTargets = []matchTarget{{matchKindV6CIDR, rule.to.cidrSelector, rule.to.clusterSubnetIntersection}}
			} else {
				matchTargets = []matchTarget{{matchKindV4CIDR, rule.to.cidrSelector, rule.to.clusterSubnetIntersection}}
			}
		} else if len(rule.to.dnsName) > 0 {
			// rule based on DNS NAME
			dnsName := rule.to.dnsName
			// If DNSNameResolver is enabled, then use the egressFirewallExternalDNS to get the address
			// set corresponding to the DNS name, otherwise use the egressFirewallDNS
			// to get the address set.
			if config.OVNKubernetesFeature.EnableDNSNameResolver {
				// Convert the DNS name to lower case fully qualified domain name.
				dnsName = util.LowerCaseFQDN(rule.to.dnsName)
			}
			dnsNameAddressSets, err := oc.dnsNameResolver.Add(ef.namespace, dnsName)
			if err != nil {
				return fmt.Errorf("error with DNSNameResolver - %v", err)
			}
			dnsNameIPv4ASHashName, dnsNameIPv6ASHashName := dnsNameAddressSets.GetASHashNames()
			if dnsNameIPv4ASHashName != "" {
				matchTargets = append(matchTargets, matchTarget{matchKindV4AddressSet, dnsNameIPv4ASHashName, rule.to.clusterSubnetIntersection})
			}
			if dnsNameIPv6ASHashName != "" {
				matchTargets = append(matchTargets, matchTarget{matchKindV6AddressSet, dnsNameIPv6ASHashName, rule.to.clusterSubnetIntersection})
			}
		}

		if len(matchTargets) == 0 {
			klog.Warningf("Egress Firewall rule: %#v has no destination...ignoring", *rule)
			// ensure the ACL is removed from OVN
			if err := oc.deleteEgressFirewallRule(ef.namespace, pgName, rule.id); err != nil {
				return err
			}
			continue
		}

		match := generateMatch(pgName, matchTargets, rule.ports)
		ops, err = oc.createEgressFirewallACLOps(ops, rule.id, match, action, ef.namespace, pgName, aclLogging)
		if err != nil {
			return err
		}
	}
	_, err = libovsdbops.TransactAndCheck(oc.nbClient, ops)
	if err != nil {
		return fmt.Errorf("failed to transact egressFirewall ACL: %v", err)
	}
	return nil
}

// createEgressFirewallACLOps uses the previously generated elements and creates the
// acls for all node switches
func (oc *DefaultNetworkController) createEgressFirewallACLOps(ops []libovsdb.Operation, ruleIdx int, match, action, namespace, pgName string, aclLogging *libovsdbutil.ACLLoggingLevels) ([]libovsdb.Operation, error) {
	aclIDs := oc.getEgressFirewallACLDbIDs(namespace, ruleIdx)
	priority := types.EgressFirewallStartPriority - ruleIdx
	egressFirewallACL := libovsdbutil.BuildACL(
		aclIDs,
		priority,
		match,
		action,
		aclLogging,
		// since egressFirewall has direction to-lport, set type to ingress
		libovsdbutil.LportIngress,
	)
	var err error
	ops, err = libovsdbops.CreateOrUpdateACLsOps(oc.nbClient, ops, oc.GetSamplingConfig(), egressFirewallACL)
	if err != nil {
		return ops, fmt.Errorf("failed to create egressFirewall ACL %v: %v", egressFirewallACL, err)
	}

	ops, err = libovsdbops.AddACLsToPortGroupOps(oc.nbClient, ops, pgName, egressFirewallACL)
	if err != nil {
		return ops, fmt.Errorf("failed to add egressFirewall ACL %v to port group %s: %v",
			egressFirewallACL, pgName, err)
	}

	return ops, nil
}

func (oc *DefaultNetworkController) deleteEgressFirewallRule(namespace, pgName string, ruleIdx int) error {
	// Find ACLs for a given egressFirewall
	aclIDs := oc.getEgressFirewallACLDbIDs(namespace, ruleIdx)
	pACL := libovsdbops.GetPredicate[*nbdb.ACL](aclIDs, nil)
	egressFirewallACLs, err := libovsdbops.FindACLsWithPredicate(oc.nbClient, pACL)
	if err != nil {
		return fmt.Errorf("unable to list egress firewall ACLs, cannot cleanup old stale data, err: %v", err)
	}

	if len(egressFirewallACLs) == 0 {
		return nil
	}

	if len(egressFirewallACLs) > 1 {
		klog.Errorf("Duplicate ACL found for egress firewall %s, ruleIdx: %d", namespace, ruleIdx)
	}

	err = libovsdbops.DeleteACLsFromPortGroups(oc.nbClient, []string{pgName}, egressFirewallACLs...)
	return err
}

// deleteEgressFirewallRules delete egress firewall Acls
func (oc *DefaultNetworkController) deleteEgressFirewallRules(namespace string) error {
	// Find ACLs for a given egressFirewall
	predicateIDs := libovsdbops.NewDbObjectIDs(libovsdbops.ACLEgressFirewall, oc.controllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: namespace,
		})
	pACL := libovsdbops.GetPredicate[*nbdb.ACL](predicateIDs, nil)
	egressFirewallACLs, err := libovsdbops.FindACLsWithPredicate(oc.nbClient, pACL)
	if err != nil {
		return fmt.Errorf("unable to list egress firewall ACLs, cannot cleanup old stale data, err: %v", err)
	}

	if len(egressFirewallACLs) == 0 {
		klog.Warningf("No egressFirewall ACLs to delete in ns: %s", namespace)
		return nil
	}
	pgName := oc.getNamespacePortGroupName(namespace)
	err = libovsdbops.DeleteACLsFromPortGroups(oc.nbClient, []string{pgName}, egressFirewallACLs...)
	if err != nil {
		return err
	}

	return nil
}

type matchTarget struct {
	kind  matchKind
	value string
	// clusterSubnetIntersection is inherited from the egressFirewallRule destination.
	// clusterSubnetIntersection is true, if egress firewall rule CIDRSelector intersects with clusterSubnet.
	// Based on this flag we can omit clusterSubnet exclusion from the related ACL.
	// For dns-based rules, EgressDNS won't add ips from clusterSubnet to the address set.
	clusterSubnetIntersection bool
}

type matchKind int

const (
	matchKindV4CIDR matchKind = iota
	matchKindV6CIDR
	matchKindV4AddressSet
	matchKindV6AddressSet
)

func (m *matchTarget) toExpr() (string, error) {
	var match string
	switch m.kind {
	case matchKindV4CIDR:
		match = fmt.Sprintf("ip4.dst == %s", m.value)
		if m.clusterSubnetIntersection {
			match = fmt.Sprintf("%s && %s", match, getV4ClusterSubnetsExclusion())
		}
	case matchKindV6CIDR:
		match = fmt.Sprintf("ip6.dst == %s", m.value)
		if m.clusterSubnetIntersection {
			match = fmt.Sprintf("%s && %s", match, getV6ClusterSubnetsExclusion())
		}
	case matchKindV4AddressSet:
		if m.value != "" {
			match = fmt.Sprintf("ip4.dst == $%s", m.value)
		}
	case matchKindV6AddressSet:
		if m.value != "" {
			match = fmt.Sprintf("ip6.dst == $%s", m.value)
		}
	default:
		return "", fmt.Errorf("invalid MatchKind")
	}
	return match, nil
}

// generateMatch generates the "match" section of ACL generation for egressFirewallRules.
// It is referentially transparent as all the elements have been validated before this function is called
// sample output:
// match=\"(ip4.dst == 1.2.3.4/32) && ip4.src == $testv4 && ip4.dst != 10.128.0.0/14\
func generateMatch(pgName string, destinations []matchTarget, dstPorts []egressfirewallapi.EgressFirewallPort) string {
	var dst string
	src := "inport == @" + pgName

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
	return match
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

func getV4ClusterSubnetsExclusion() string {
	var exclusions []string
	for _, clusterSubnet := range config.Default.ClusterSubnets {
		if utilnet.IsIPv4CIDR(clusterSubnet.CIDR) {
			exclusions = append(exclusions, fmt.Sprintf("%s.dst != %s", "ip4", clusterSubnet.CIDR))
		}
	}
	return strings.Join(exclusions, "&&")
}

func getV6ClusterSubnetsExclusion() string {
	var exclusions []string
	for _, clusterSubnet := range config.Default.ClusterSubnets {
		if utilnet.IsIPv6CIDR(clusterSubnet.CIDR) {
			exclusions = append(exclusions, fmt.Sprintf("%s.dst != %s", "ip6", clusterSubnet.CIDR))
		}
	}
	return strings.Join(exclusions, "&&")
}

func getEgressFirewallNamespacedName(egressFirewall *egressfirewallapi.EgressFirewall) string {
	return fmt.Sprintf("%v/%v", egressFirewall.Namespace, egressFirewall.Name)
}

// updateACLLoggingForEgressFirewall updates logging related configuration for all rules of this specific firewall in OVN.
// This method can be called for example from the Namespaces Watcher methods to reload firewall rules' logging  when
// namespace annotations change.
// Return values are: bool - if the egressFirewall's ACL was updated or not, error in case of errors. If a namespace
// does not contain an egress firewall ACL, then this returns false, nil instead of a NotFound error.
func (oc *DefaultNetworkController) updateACLLoggingForEgressFirewall(egressFirewallNamespace string, nsInfo *namespaceInfo) (bool, error) {
	// Retrieve the egress firewall object from cache and lock it.
	obj, loaded := oc.egressFirewalls.Load(egressFirewallNamespace)
	if !loaded {
		return false, nil
	}

	ef, ok := obj.(*egressFirewall)
	if !ok {
		return false, fmt.Errorf("updateACLLoggingForEgressFirewall failed: type assertion to *egressFirewall"+
			" failed for EgressFirewall of type %T in namespace %s",
			obj, ef.namespace)
	}

	ef.Lock()
	defer ef.Unlock()

	// Predicate for given egress firewall ACLs
	predicateIDs := libovsdbops.NewDbObjectIDs(libovsdbops.ACLEgressFirewall, oc.controllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: ef.namespace,
		})
	p := libovsdbops.GetPredicate[*nbdb.ACL](predicateIDs, nil)
	if err := libovsdbutil.UpdateACLLoggingWithPredicate(oc.nbClient, p, &nsInfo.aclLogging); err != nil {
		return false, fmt.Errorf("unable to update ACL logging in ns %s, err: %v", ef.namespace, err)
	}
	return true, nil
}

func (oc *DefaultNetworkController) getEgressFirewallACLDbIDs(namespace string, ruleIdx int) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.ACLEgressFirewall, oc.controllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: namespace,
			libovsdbops.RuleIndex:     strconv.Itoa(ruleIdx),
		})
}

func (oc *DefaultNetworkController) newEFNodeController(nodeInformer coreinformers.NodeInformer) controller.Controller {
	controllerConfig := &controller.ControllerConfig[kapi.Node]{
		RateLimiter:    workqueue.NewTypedItemFastSlowRateLimiter[string](time.Second, 5*time.Second, 5),
		Informer:       nodeInformer.Informer(),
		Lister:         nodeInformer.Lister().List,
		ObjNeedsUpdate: oc.efNodeNeedsUpdate,
		Reconcile:      oc.updateEgressFirewallForNode,
		Threadiness:    1,
	}
	return controller.NewController[kapi.Node]("ef_node_controller", controllerConfig)
}

func (oc *DefaultNetworkController) efNodeNeedsUpdate(oldNode, newNode *kapi.Node) bool {
	if oldNode == nil || newNode == nil {
		return true
	}
	return !reflect.DeepEqual(oldNode.Labels, newNode.Labels) ||
		util.NodeHostCIDRsAnnotationChanged(oldNode, newNode)
}

func (oc *DefaultNetworkController) updateEgressFirewallForNode(nodeName string) error {
	node, err := oc.watchFactory.GetNode(nodeName)
	// It´s unlikely that we have an error different that "Not Found Object"
	// because we are getting the object from the informer´s cache
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}

	var nodeIPs []string
	if node != nil {
		nodeIPs, err = util.GetNodeHostAddrs(node)
		if err != nil {
			return err
		}
	}

	// cycle through egress firewalls and check if any match this node's labels
	var efErr error
	oc.egressFirewalls.Range(func(k, v interface{}) bool {
		ef := v.(*egressFirewall)
		namespace := k.(string)
		ef.Lock()
		defer ef.Unlock()
		var modifiedRuleIDs []int
		for _, rule := range ef.egressRules {
			// nodeSelector will always have a value, but it is mutually exclusive from cidrSelector and dnsName
			if len(rule.to.cidrSelector) != 0 || len(rule.to.dnsName) != 0 {
				continue
			}
			selector, err := metav1.LabelSelectorAsSelector(rule.to.nodeSelector)
			if err != nil {
				klog.Errorf("Error while parsing label selector %#v for egress firewall in namespace %s",
					rule.to.nodeSelector, namespace)
				continue
			}
			// no need to check selector on old node here, ips are unique and regardless of if selector
			// matches or not we shouldn't have those addresses anymore
			delete(rule.to.nodeAddrs, nodeName)
			// check if selector matches
			if node != nil && selector.Matches(labels.Set(node.Labels)) {
				rule.to.nodeAddrs[nodeName] = nodeIPs
			}
			modifiedRuleIDs = append(modifiedRuleIDs, rule.id)
		}
		if len(modifiedRuleIDs) == 0 {
			return true
		}
		// update egress firewall rules
		pgName := oc.getNamespacePortGroupName(ef.namespace)
		aclLoggingLevels := oc.GetNamespaceACLLogging(ef.namespace)
		if err := oc.addEgressFirewallRules(ef, pgName,
			aclLoggingLevels, modifiedRuleIDs...); err != nil {
			efErr = fmt.Errorf("failed to add egress firewall for namespace: %s, error: %w", namespace, err)
			return false
		}
		return true
	})

	return efErr
}

func (oc *DefaultNetworkController) setEgressFirewallStatus(egressFirewall *egressfirewallapi.EgressFirewall, handlerErr error) error {
	var newMsg string
	if handlerErr != nil {
		newMsg = types.EgressFirewallErrorMsg + ": " + handlerErr.Error()
	} else {
		newMsg = egressFirewallAppliedCorrectly
		metrics.UpdateEgressFirewallRuleCount(float64(len(egressFirewall.Spec.Egress)))
		metrics.IncrementEgressFirewallCount()
	}

	newMsg = types.GetZoneStatus(oc.zone, newMsg)
	needsUpdate := true
	for _, message := range egressFirewall.Status.Messages {
		if message == newMsg {
			// found previous status
			needsUpdate = false
			break
		}
	}
	if !needsUpdate {
		return nil
	}

	applyOptions := metav1.ApplyOptions{
		Force:        true,
		FieldManager: oc.zone,
	}

	applyObj := egressfirewallapply.EgressFirewall(egressFirewall.Name, egressFirewall.Namespace).
		WithStatus(egressfirewallapply.EgressFirewallStatus().
			WithMessages(newMsg))
	_, err := oc.kube.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewall.Namespace).ApplyStatus(context.TODO(), applyObj, applyOptions)

	return err
}
