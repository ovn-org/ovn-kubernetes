package ovn

import (
	"fmt"
	"net"
	"strings"
	"sync"

	"github.com/pkg/errors"

	"github.com/ovn-org/libovsdb/ovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	egressfirewallapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"

	kapi "k8s.io/api/core/v1"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
)

const (
	egressFirewallAppliedCorrectly = "EgressFirewall Rules applied"
	egressFirewallAddError         = "EgressFirewall Rules not correctly added"
	// egressFirewallPortGroupSuffix is the suffix used when creating
	egressFirewallPortGroupSuffix = "egress_firewall"
	// egressFirewallACLExtIdKey external ID key for egress firewall ACLs
	egressFirewallACLExtIdKey = "egressFirewall"
)

type egressFirewall struct {
	sync.Mutex
	name        string
	namespace   string
	egressRules []*egressFirewallRule
	hasDNS      bool
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

func (oc *Controller) deleteLegacyACLsFromDb(egressFirewallACLs []*nbdb.ACL) error {
	// egress firewall ACLs were moved from switches to port groups
	// delete ACLs from all switches
	p := func(item *nbdb.LogicalSwitch) bool {
		return true
	}
	err := libovsdbops.RemoveACLsFromLogicalSwitchesWithPredicate(oc.nbClient, p, egressFirewallACLs...)
	if err != nil {
		return fmt.Errorf("failed to remove ACLs from node logical switches: %v", err)
	}
	// Delete ACLs
	err = libovsdbops.DeleteACLs(oc.nbClient, egressFirewallACLs...)
	if err != nil {
		return fmt.Errorf("failed to delete ACLs: %v", err)
	}
	return nil
}

// This function is used to sync egress firewall setup. Egress firewall implementation had many versions,
// the latest one makes no difference for gateway modes, and moves EF ACLs from switches to port groups.
// Together with that, match was changed to use port_group and also priority range was changed to ensure
// proper priorities between features using ACLs (multicast, network policy, egress firewall).
// Therefore, the easiest way to sync egress firewalls is to delete the old rules and create them from scratch
//
// 1. Delete stale EF ACLs and their references from switches with deleteLegacyACLsFromDb function that knows old db structure
// 2. Delete EFs that are not present in the k8s anymore
// 3. Keep track of address sets referenced by EF ACLs, delete the ones that don't have references after ACL cleanup
// This is required, because EgressDNS after restart doesn't know which address sets were created for dns entries before
// 4. Cleanup the old implementation (using LRP) in local GW mode
// For this it just deletes all LRP setup done for egress firewall
//
// Upon failure, it may be invoked multiple times in order to avoid a pod restart.
func (oc *Controller) syncEgressFirewall(egressFirewalls []interface{}) error {
	k8sEgressFirewalls := map[string]*egressfirewallapi.EgressFirewall{}
	for _, efInterface := range egressFirewalls {
		ef, ok := efInterface.(*egressfirewallapi.EgressFirewall)
		if !ok {
			return fmt.Errorf("spurious object in syncEgressFirewall: %v", efInterface)
		}
		k8sEgressFirewalls[ef.Namespace] = ef
	}
	// remember all DNS address sets referenced by EF ACLs in the db before any cleanup
	efAclPred := func(item *nbdb.ACL) bool {
		return item.ExternalIDs[egressFirewallACLExtIdKey] != ""
	}
	egressFirewallACLs, err := libovsdbops.FindACLsWithPredicate(oc.nbClient, efAclPred)
	if err != nil {
		return fmt.Errorf("unable to find egress firewall ACLs, err: %v", err)
	}
	referencedASBefore := oc.getReferencedDNSAddressSets(egressFirewallACLs)

	// find and delete legacy ACLs and their references.
	// priority range for egress firewall was moved, but the ranges intersect.
	// use the fact, that old ef ACL priority start at LegacyEgressFirewallStartPriority
	// (skipping priorities was not possible before nodeSelector) and every old ef will have ACL with that priority.
	// then find ef namespace, and try to recreate ef
	staleEFAclPred := func(item *nbdb.ACL) bool {
		_, ok := item.ExternalIDs[egressFirewallACLExtIdKey]
		if !ok {
			return false
		}
		return item.Priority == types.LegacyEgressFirewallStartPriority
	}
	egressFirewallACLs, err = libovsdbops.FindACLsWithPredicate(oc.nbClient, staleEFAclPred)
	if err != nil {
		return fmt.Errorf("unable to find stale egress firewall ACLs, err: %v", err)
	}

	if len(egressFirewallACLs) != 0 {
		// only 1 ACL for an ef can be present here, because every ef can have no more than 1 rule with specified priority
		// to allow applying this function again in case of error, fist save stale ACLs for ef
		// then try to create new ACLs for it
		// delete stale ACLs only if creating a new ones succeeds
		for _, ef := range egressFirewallACLs {
			if ns := ef.ExternalIDs[egressFirewallACLExtIdKey]; ns != "" {
				// Find and save stale ACLs for a given egressFirewall
				pACL := func(item *nbdb.ACL) bool {
					return item.ExternalIDs[egressFirewallACLExtIdKey] == ns
				}
				staleACLs, err := libovsdbops.FindACLsWithPredicate(oc.nbClient, pACL)
				if err != nil {
					return fmt.Errorf("unable to find ACLs for stale egress firewall in namespace %s, err: %v", ns, err)
				}
				if _, ok := k8sEgressFirewalls[ns]; ok {
					// ef still exists in k8s, update
					klog.Infof("re-create stale egress firewall in namespace %s", ns)
					// create new ACLs
					// ensureEgressFirewall doesn't clean up db in case of error, so stale ACLs should stay
					err = oc.ensureEgressFirewall(k8sEgressFirewalls[ns])
					if err != nil {
						return fmt.Errorf("failed to recreate stale egress firewall in namespace %s: %v", ns, err)
					}
					// delete saved stale ACLs only if update succeeds
					err = oc.deleteLegacyACLsFromDb(staleACLs)
					if err != nil {
						return fmt.Errorf("failed to delete stale egress firewall in namespace %s: %v", ns, err)
					}
				} else {
					klog.Infof("Delete stale egress firewall in namespace %s", ns)
					// ef doesn't exist in k8s, just delete
					err = oc.deleteLegacyACLsFromDb(staleACLs)
					if err != nil {
						return fmt.Errorf("failed to delete stale egress firewall in namespace %s: %v", ns, err)
					}
				}
			}
		}
	}

	// delete egressFirewalls that are present in ovn but deleted from k8s
	// represents the namespaces that have firewalls according to ovn
	// don't touch default ACL for cluster network
	klog.Infof("Sync egress firewalls")
	egressFirewallACLs, err = libovsdbops.FindACLsWithPredicate(oc.nbClient, efAclPred)
	if err != nil {
		return fmt.Errorf("unable to find existing egress firewall ACLs, err: %v", err)
	}
	ovnEgressFirewalls := make(map[string]bool)
	for _, egressFirewallACL := range egressFirewallACLs {
		if ns := egressFirewallACL.ExternalIDs[egressFirewallACLExtIdKey]; ns != "" {
			// Most egressFirewalls will have more than one ACL, but we only need to know if there is one for the namespace
			// so a map is fine, and we will add an entry every iteration but because it is a map will overwrite the previous
			// entry if it already existed
			ovnEgressFirewalls[ns] = true
		}
	}

	// delete entries from the map that exist in k8s and ovn
	for k8sEgressFirewallNamespace := range k8sEgressFirewalls {
		delete(ovnEgressFirewalls, k8sEgressFirewallNamespace)
	}
	// any that are left are spurious and should be cleaned up
	if len(ovnEgressFirewalls) > 0 {
		klog.Infof("Cleanup deleted egress firewalls: %+v", ovnEgressFirewalls)
	}
	for spuriousEF := range ovnEgressFirewalls {
		// stale egressFirewalls should already be deleted/update on the previous step
		// use deleteEgressFirewallFromDb that can only clear new-style egress firewalls
		err := oc.deleteEgressFirewallFromDb(spuriousEF)
		if err != nil {
			return fmt.Errorf("cannot fully reconcile the state of egressfirewalls ACLs for namespace %s still exist in ovn db: %v", spuriousEF, err)
		}
	}

	// now see which DNS address sets are references after cleanup
	egressFirewallACLs, err = libovsdbops.FindACLsWithPredicate(oc.nbClient, efAclPred)
	if err != nil {
		return fmt.Errorf("unable to find egress firewall ACLs after cleanup, err: %v", err)
	}
	referencedASAfter := oc.getReferencedDNSAddressSets(egressFirewallACLs)

	// delete all DNS address sets that are not referenced anymore
	for asName := range referencedASAfter {
		delete(referencedASBefore, asName)
	}
	asPred := func(item *nbdb.AddressSet) bool {
		for staleAsName := range referencedASBefore {
			if item.Name == staleAsName {
				return true
			}
		}
		return false
	}
	klog.Infof("Deleting stale DNS address sets %v", referencedASBefore)
	err = libovsdbops.DeleteAddressSetsWithPredicate(oc.nbClient, asPred)
	if err != nil {
		return fmt.Errorf("failed to cleanup DNS address sets: %v", err)
	}

	// In any gateway mode, make sure to delete all LRPs on ovn_cluster_router.
	// This covers migration from LGW mode that used LRPs for EFW to using ACLs in SGW/LGW modes
	// Using Legacy priorities is safe, since this is applied to LRP Policy and not to ACLs
	p := func(item *nbdb.LogicalRouterPolicy) bool {
		return types.LegacyMinimumReservedEgressFirewallPriority <= item.Priority && item.Priority <= types.LegacyEgressFirewallStartPriority
	}
	err = libovsdbops.DeleteLogicalRouterPoliciesWithPredicate(oc.nbClient, types.OVNClusterRouter, p)
	if err != nil {
		return fmt.Errorf("error deleting egress firewall policies on router %s: %v", types.OVNClusterRouter, err)
	}
	klog.Infof("Sync egress firewalls: done")
	return nil
}

func (oc *Controller) getReferencedDNSAddressSets(egressFirewallACLs []*nbdb.ACL) map[string]bool {
	usedASNames := map[string]bool{}
	for _, acl := range egressFirewallACLs {
		// address sets in match used as $<hashed as name>
		// egress firewall only uses address sets for dns
		asStartIdx := strings.Index(acl.Match, "$")
		if asStartIdx >= 0 {
			var hashedASName string
			// address set match has a format "(ip<x>.dst == $<address set>)"
			asEndIdx := strings.Index(acl.Match[asStartIdx+1:], ")")
			if asEndIdx < 0 {
				// shouldn't happen, just in case
				hashedASName = acl.Match[asStartIdx+1:]
			} else {
				hashedASName = acl.Match[asStartIdx+1 : asStartIdx+1+asEndIdx]
			}
			usedASNames[hashedASName] = true
		}
	}
	return usedASNames
}

func (oc *Controller) disableEgressFirewall() error {
	// delete all EF ACLs, port groups and DNS address sets
	pACL := func(item *nbdb.ACL) bool {
		return item.ExternalIDs[egressFirewallACLExtIdKey] != ""
	}
	egressFirewallACLs, err := libovsdbops.FindACLsWithPredicate(oc.nbClient, pACL)
	if err != nil {
		return fmt.Errorf("unable to list egress firewall ACLs, err: %v", err)
	}
	// find and delete all referenced address sets
	referencedAS := oc.getReferencedDNSAddressSets(egressFirewallACLs)
	asPred := func(item *nbdb.AddressSet) bool {
		for staleAsName := range referencedAS {
			if item.Name == staleAsName {
				return true
			}
		}
		return false
	}
	err = libovsdbops.DeleteAddressSetsWithPredicate(oc.nbClient, asPred)
	if err != nil {
		return fmt.Errorf("failed to cleanup DNS address sets: %v", err)
	}
	// find and delete all EF port groups
	var pgNames []string
	for _, ef := range egressFirewallACLs {
		if ns := ef.ExternalIDs[egressFirewallACLExtIdKey]; ns != "" {
			pgNames = append(pgNames, getEgressFirewallPortGroupName(ns))
		}
	}
	ops, err := libovsdbops.DeletePortGroupsOps(oc.nbClient, nil, pgNames...)
	if err != nil {
		return fmt.Errorf("unable to get deleting port group ops: %v", err)
	}

	// finally, delete all ACLs
	ops, err = libovsdbops.DeleteACLsOps(oc.nbClient, ops, egressFirewallACLs...)
	if err != nil {
		return fmt.Errorf("unable to get deleting ACL ops: %v", err)
	}

	_, err = libovsdbops.TransactAndCheck(oc.nbClient, ops)
	return err
}

// getEgressFirewallLocked loads egress firewall by namespace and locks it.
// When errIfNotFound is false and egress firewall is not found, `nil, nil` will be returned
// Egress Firewall can be unlocked just by calling Unlock() on returned object
func (oc *Controller) getEgressFirewallLocked(namespace string, errIfNotFound bool) (*egressFirewall, error) {
	obj, loaded := oc.egressFirewalls.Load(namespace)
	if loaded {
		ef, ok := obj.(*egressFirewall)
		if !ok {
			return nil, fmt.Errorf("type assertion to *egressFirewall failed for EgressFirewall in namespace %s",
				namespace)
		}
		ef.Lock()
		// make sure ef wasn't deleted while we waited for lock
		if _, loaded = oc.egressFirewalls.Load(namespace); loaded {
			return ef, nil
		} else {
			ef.Unlock()
		}
	}
	// not found
	if errIfNotFound {
		return nil, fmt.Errorf("there is no egressFirewall found in namespace %s",
			namespace)
	} else {
		return nil, nil
	}
}

func (oc *Controller) ensureEgressFirewall(egressFirewallObj *egressfirewallapi.EgressFirewall) error {
	klog.Infof("Adding egressFirewall %s in namespace %s", egressFirewallObj.Name, egressFirewallObj.Namespace)
	ef := cloneEgressFirewall(egressFirewallObj)
	var addErrors error
	for i, egressFirewallRule := range egressFirewallObj.Spec.Egress {
		// process Rules into egressFirewallRules for egressFirewall struct
		if i > types.EgressFirewallStartPriority-types.MinimumReservedEgressFirewallPriority {
			klog.Warningf("egressFirewall for namespace %s has too many rules, the rest will be ignored",
				egressFirewallObj.Namespace)
			break
		}
		efr, err := newEgressFirewallRule(egressFirewallRule, i)
		if err != nil {
			addErrors = errors.Wrapf(addErrors, "error: cannot create EgressFirewall Rule to destination %s for namespace %s - %v",
				egressFirewallRule.To.CIDRSelector, egressFirewallObj.Namespace, err)
			continue

		}
		ef.egressRules = append(ef.egressRules, efr)
	}
	if addErrors != nil {
		return addErrors
	}

	var ops []ovsdb.Operation
	// err is used in defer, make sure not to reassign
	var err error
	var acls []*nbdb.ACL
	var lsps []*nbdb.LogicalSwitchPort
	// no db transactions for now, just build ops
	pgName := getEgressFirewallPortGroupName(ef.namespace)
	aclLoggingLevels := oc.GetNamespaceACLLogging(ef.namespace)
	// create ACLs referencing port group first
	if ops, acls, err = oc.updateEgressFirewallRulesOps(ef, pgName, types.EgressFirewallStartPriority, aclLoggingLevels, ops); err != nil {
		return fmt.Errorf("error getting acls create ops: %v", err)
	}
	// lock and expose ef object before getting namespace ports for ef port group.
	// newly added pods will hold that lock to update port group
	ef.Lock()
	defer ef.Unlock()
	// there should not be an item already in egressFirewall map for the given Namespace
	if obj, loaded := oc.egressFirewalls.LoadOrStore(egressFirewallObj.Namespace, ef); loaded {
		existingEF, ok := obj.(*egressFirewall)
		if !ok {
			return fmt.Errorf("type assertion to *egressFirewall failed for EgressFirewall in namespace %s",
				egressFirewallObj.Namespace)
		}
		if existingEF.name != ef.name {
			return fmt.Errorf("error attempting to add egressFirewall %s to namespace %s when it already has an egressFirewall",
				egressFirewallObj.Name, egressFirewallObj.Namespace)
		}
	}
	defer func() {
		if err != nil {
			// no need to clear db, since it's the last operation in this function
			_ = oc.cleanupEgressFirewall(ef, false, true)
		}
	}()

	lsps, err = oc.getNamespacePorts(ef.namespace)
	if err != nil {
		return fmt.Errorf("failed to get logical ports for ns %s: %v", ef.namespace, err)
	}
	// now create port group with acls and ports
	pg := libovsdbops.BuildPortGroup(pgName, getEgressFirewallPortGroupExternalIdName(ef.namespace), lsps, acls)
	if ops, err = libovsdbops.CreateOrUpdatePortGroupsOps(oc.nbClient, ops, pg); err != nil {
		return fmt.Errorf("error getting create port group ops: %v", err)
	}
	_, err = libovsdbops.TransactAndCheck(oc.nbClient, ops)
	return err
}

func (oc *Controller) cleanupEgressFirewall(ef *egressFirewall, clearDb, ignoreErr bool) error {
	if clearDb {
		if err := oc.deleteEgressFirewallFromDb(ef.namespace); err != nil {
			if ignoreErr {
				klog.Warningf("Failed deleting egress firewall for namespace %s from db: %v", ef.namespace, err)
			} else {
				return err
			}
		}
	}
	if ef.hasDNS {
		if err := oc.egressFirewallDNS.DeleteNamespace(ef.namespace); err != nil {
			if ignoreErr {
				klog.Warningf("Failed deleting DNS entries for ns %s: %v", ef.namespace, err)
			} else {
				return err
			}
		}
	}
	oc.egressFirewalls.Delete(ef.namespace)
	return nil
}

func (oc *Controller) deleteEgressFirewall(egressFirewallObj *egressfirewallapi.EgressFirewall) error {
	klog.Infof("Deleting egress Firewall %s in namespace %s", egressFirewallObj.Name, egressFirewallObj.Namespace)
	ef, err := oc.getEgressFirewallLocked(egressFirewallObj.Namespace, true)
	if err != nil {
		return err
	}
	defer ef.Unlock()
	return oc.cleanupEgressFirewall(ef, true, false)
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

func (oc *Controller) createDefaultACLs(namespace, portGroupName string, aclLogging *ACLLoggingLevels,
	ops []ovsdb.Operation) ([]ovsdb.Operation, []*nbdb.ACL, error) {
	var err error
	// Management ports will be blocked by egress firewall rules.
	// To block pod to node traffic in case such traffic is forbidden by egress firewall rules via node ips.
	// The priority should be higher than EgressFirewallAllowClusterSubnetsPriority to exclude management ports
	// from cluster subnets.
	// All management ports are added to the ClusterPortGroupName when created, use it.
	defaultDenyMgmtPortACL := BuildACL(
		buildEgressFwAclName(namespace, types.EgressFirewallDenyMgmtPortsPriority),
		nbdb.ACLDirectionFromLport,
		types.EgressFirewallDenyMgmtPortsPriority,
		fmt.Sprintf("outport == @%s && inport == @%s", types.ClusterPortGroupName, portGroupName),
		nbdb.ACLActionDrop,
		aclLogging,
		map[string]string{egressFirewallACLExtIdKey: namespace},
		map[string]string{"apply-after-lb": "true"},
	)
	acls := []*nbdb.ACL{defaultDenyMgmtPortACL}
	ops, err = libovsdbops.CreateOrUpdateACLsOps(oc.nbClient, ops, defaultDenyMgmtPortACL)
	if err != nil {
		return ops, acls, err
	}
	allowClusterSubnetACL := BuildACL(
		buildEgressFwAclName(namespace, types.EgressFirewallAllowClusterSubnetsPriority),
		nbdb.ACLDirectionFromLport,
		types.EgressFirewallAllowClusterSubnetsPriority,
		fmt.Sprintf("%s && inport == @%s", getDstClusterSubnetsMatch(), portGroupName),
		nbdb.ACLActionAllowRelated,
		aclLogging,
		map[string]string{egressFirewallACLExtIdKey: namespace},
		map[string]string{"apply-after-lb": "true"},
	)
	acls = append(acls, allowClusterSubnetACL)
	ops, err = libovsdbops.CreateOrUpdateACLsOps(oc.nbClient, ops, allowClusterSubnetACL)
	if err != nil {
		return ops, acls, err
	}
	return ops, acls, nil
}

func (oc *Controller) updateEgressFirewallRulesOps(ef *egressFirewall, portGroupName string, efStartPriority int,
	aclLogging *ACLLoggingLevels, ops []ovsdb.Operation) ([]ovsdb.Operation, []*nbdb.ACL, error) {
	var err error
	var acls []*nbdb.ACL
	var priority int
	// create 2 default ACLs to handle clusterSubnet traffic first
	ops, acls, err = oc.createDefaultACLs(ef.namespace, portGroupName, aclLogging, ops)
	if err != nil {
		return ops, acls, fmt.Errorf("failed to create default EF acls: %v", err)
	}
	dnsNames := map[string]bool{}
	for _, rule := range ef.egressRules {
		var action string
		var matchTar matchTarget
		if rule.access == egressfirewallapi.EgressFirewallRuleAllow {
			action = nbdb.ACLActionAllowRelated
		} else {
			action = nbdb.ACLActionDrop
		}
		if rule.to.cidrSelector != "" {
			if utilnet.IsIPv6CIDRString(rule.to.cidrSelector) {
				matchTar = matchTarget{matchKindV6CIDR, rule.to.cidrSelector}
			} else {
				matchTar = matchTarget{matchKindV4CIDR, rule.to.cidrSelector}
			}
		} else {
			// rule based on DNS NAME
			dnsNameAddressSets, err := oc.egressFirewallDNS.Add(ef.namespace, rule.to.dnsName)
			if err != nil {
				return ops, acls, fmt.Errorf("error with EgressFirewallDNS - %v", err)
			}
			dnsNames[rule.to.dnsName] = true
			dnsNameIPv4ASHashName, dnsNameIPv6ASHashName := dnsNameAddressSets.GetASHashNames()
			if dnsNameIPv4ASHashName != "" {
				matchTar = matchTarget{matchKindV4AddressSet, dnsNameIPv4ASHashName}
			}
			if dnsNameIPv6ASHashName != "" {
				matchTar = matchTarget{matchKindV6AddressSet, dnsNameIPv6ASHashName}
			}
		}
		match := generateMatch(portGroupName, matchTar, rule.ports)
		priority = efStartPriority - rule.id
		// a name is needed for logging purposes - the name must be unique, so make it
		// egressFirewall_<namespace name>_<priority>
		aclName := buildEgressFwAclName(ef.namespace, priority)
		egressFirewallACL := BuildACL(
			aclName,
			nbdb.ACLDirectionFromLport,
			priority,
			match,
			action,
			aclLogging,
			map[string]string{egressFirewallACLExtIdKey: ef.namespace},
			map[string]string{"apply-after-lb": "true"},
		)
		acls = append(acls, egressFirewallACL)
		ops, err = libovsdbops.CreateOrUpdateACLsOps(oc.nbClient, ops, egressFirewallACL)
		if err != nil {
			return ops, acls, err
		}
	}

	// now cleanup stale DNS
	if len(dnsNames) > 0 {
		// hasDNS is needed for cleanup on add failure
		ef.hasDNS = true
		if err = oc.egressFirewallDNS.UpdateNamespace(ef.namespace, dnsNames); err != nil {
			return ops, acls, fmt.Errorf("update egress firewall (ns %s) ACLs failed: "+
				"DNS entry cleanup failed: %v", ef.namespace, err)
		}
	}
	// find and delete old ACLs with lower priorities that won't be updated
	// the lowest priority for updated ef is saved in `priority` variable
	staleEFAclPred := func(item *nbdb.ACL) bool {
		id, ok := item.ExternalIDs[egressFirewallACLExtIdKey]
		return ok && id == ef.namespace && item.Priority < priority
	}
	staleACLs, err := libovsdbops.FindACLsWithPredicate(oc.nbClient, staleEFAclPred)
	if err != nil {
		return ops, acls, fmt.Errorf("update egress firewall (ns %s) ACLs failed: "+
			"cannot cleanup stale ACLs, err: %v", ef.namespace, err)
	}
	if len(staleACLs) > 0 {
		ops, err = libovsdbops.DeleteACLsFromPortGroupOps(oc.nbClient, ops, getEgressFirewallPortGroupName(ef.namespace), staleACLs...)
		if err != nil {
			return ops, acls, fmt.Errorf("update egress firewall (ns %s) ACLs failed: "+
				"unable to get deleting ACL from port group ops: %v", ef.namespace, err)
		}
		ops, err = libovsdbops.DeleteACLsOps(oc.nbClient, ops, staleACLs...)
		if err != nil {
			return ops, acls, fmt.Errorf("update egress firewall (ns %s) ACLs failed: "+
				"unable to get deleting ACL ops: %v", ef.namespace, err)
		}
	}
	return ops, acls, nil
}

// deleteEgressFirewallFromDb delete egress firewall acls and port group
func (oc *Controller) deleteEgressFirewallFromDb(externalID string) error {
	// Delete port group
	ops, err := libovsdbops.DeletePortGroupsOps(oc.nbClient, nil, getEgressFirewallPortGroupName(externalID))
	if err != nil {
		return fmt.Errorf("unable to get deleting port group ops: %v", err)
	}
	// Find ACLs for a given egressFirewall
	pACL := func(item *nbdb.ACL) bool {
		return item.ExternalIDs[egressFirewallACLExtIdKey] == externalID
	}
	egressFirewallACLs, err := libovsdbops.FindACLsWithPredicate(oc.nbClient, pACL)
	if err != nil {
		return fmt.Errorf("unable to list egress firewall ACLs, cannot cleanup old stale data, err: %v", err)
	}
	// Delete ACLs
	ops, err = libovsdbops.DeleteACLsOps(oc.nbClient, ops, egressFirewallACLs...)
	if err != nil {
		return fmt.Errorf("unable to get deleting ACL ops: %v", err)
	}
	_, err = libovsdbops.TransactAndCheck(oc.nbClient, ops)
	return err
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
// match=\"(ip4.dst == 1.2.3.4/32) && inport == @portGroup\
// Be careful about updating match - getReferencedDNSAddressSets() depends on it for parsing address set name
func generateMatch(inportPortGroup string, destination matchTarget, dstPorts []egressfirewallapi.EgressFirewallPort) string {
	var src string
	var dst string
	src = fmt.Sprintf("inport == @%s", inportPortGroup)

	dst, err := destination.toExpr()
	if err != nil {
		klog.Error(err)
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

func getDstClusterSubnetsMatch() string {
	var match string
	for _, clusterSubnet := range config.Default.ClusterSubnets {
		if match != "" {
			match += " && "
		}
		if utilnet.IsIPv6CIDR(clusterSubnet.CIDR) {
			match += fmt.Sprintf("%s.dst == %s", "ip6", clusterSubnet.CIDR)
		} else {
			match += fmt.Sprintf("%s.dst == %s", "ip4", clusterSubnet.CIDR)
		}
	}
	return match
}

func getEgressFirewallNamespacedName(egressFirewall *egressfirewallapi.EgressFirewall) string {
	return fmt.Sprintf("%v/%v", egressFirewall.Namespace, egressFirewall.Name)
}

// updateACLLoggingForEgressFirewall updates logging related configuration for all rules of this specific firewall in OVN.
// This method can be called for example from the Namespaces Watcher methods to reload firewall rules' logging  when
// namespace annotations change.
// Return values are: bool - if the egressFirewall's ACL was updated or not, error in case of errors. If a namespace
// does not contain an egress firewall ACL, then this returns false, nil instead of a NotFound error.
func (oc *Controller) updateACLLoggingForEgressFirewall(egressFirewallNamespace string, nsInfo *namespaceInfo) (bool, error) {
	// Retrieve the egress firewall object from cache and lock it.
	ef, err := oc.getEgressFirewallLocked(egressFirewallNamespace, false)
	if err != nil {
		return false, err
	}
	if ef == nil {
		return false, nil
	}
	defer ef.Unlock()

	// Predicate for given egress firewall ACLs
	p := func(item *nbdb.ACL) bool {
		return item.ExternalIDs[egressFirewallACLExtIdKey] == ef.namespace
	}
	if err := UpdateACLLoggingWithPredicate(oc.nbClient, p, &nsInfo.aclLogging); err != nil {
		return false, fmt.Errorf("unable to update ACL logging in ns %s, err: %v", ef.namespace, err)
	}
	return true, nil
}

func (oc *Controller) podAddEgressFirewall(namespace, portUUID string, ops []ovsdb.Operation) ([]ovsdb.Operation, error) {
	ef, _ := oc.getEgressFirewallLocked(namespace, false)
	if ef != nil {
		defer ef.Unlock()
		var err error
		ops, err = libovsdbops.AddPortsToPortGroupOps(oc.nbClient, ops, getEgressFirewallPortGroupName(namespace), portUUID)
		if err != nil {
			return ops, fmt.Errorf("failed to get add ports to port group ops: %v", err)
		}
	}
	return ops, nil
}

func (oc *Controller) podDeleteEgressFirewall(namespace, portUUID string, ops []ovsdb.Operation) ([]ovsdb.Operation, error) {
	ef, _ := oc.getEgressFirewallLocked(namespace, false)
	if ef != nil {
		defer ef.Unlock()
		var err error
		ops, err = libovsdbops.DeletePortsFromPortGroupOps(oc.nbClient, ops, getEgressFirewallPortGroupName(namespace), portUUID)
		if err != nil {
			return nil, err
		}
	}
	return ops, nil
}

func buildEgressFwAclName(namespace string, priority int) string {
	return fmt.Sprintf("egressFirewall_%s_%d", namespace, priority)
}

func getEgressFirewallPortGroupName(ns string) string {
	return hashedPortGroup(ns) + "_" + egressFirewallPortGroupSuffix
}

func getEgressFirewallPortGroupExternalIdName(ns string) string {
	return ns + "_" + egressFirewallPortGroupSuffix
}
