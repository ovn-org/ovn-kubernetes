package admin_network_policy

import (
	"fmt"
	"sync"
	"time"

	"github.com/ovn-org/libovsdb/ovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/pkg/errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	anpapi "sigs.k8s.io/network-policy-api/apis/v1alpha1"
)

// NOTE: A cluster can have only BANP at a given time as defined by upstream KEP.
const (
	BANPFlowPriority         = 750 // down to 651 (both inclusive, note that these ACLs will be in tier3)
	BANPMaxPriorityPerObject = 100
	BANPIngressPrefix        = "BANPIngress"
	BANPEgressPrefix         = "BANPEgress"
	BANPExternalIDKey        = "BaselineAdminNetworkPolicy" // key set on port-groups to identify which BANP it belongs to
)

// TODO: Double check how empty selector means all labels match works
type baselineAdminNetworkPolicy struct {
	sync.RWMutex
	name         string
	subject      *adminNetworkPolicySubject
	ingressRules []*baselineGressRule
	egressRules  []*baselineGressRule
	stale        bool
}

type baselineGressRule struct {
	name        string
	priority    int32 // determined based on order in the list
	gressPrefix string
	action      anpapi.BaselineAdminNetworkPolicyRuleAction
	peers       []*adminNetworkPolicyPeer
	ports       []*adminNetworkPolicyPort
	addrSet     addressset.AddressSet
}

func cloneBANP(raw *anpapi.BaselineAdminNetworkPolicy) (*baselineAdminNetworkPolicy, error) {
	banp := &baselineAdminNetworkPolicy{
		name:         raw.Name,
		ingressRules: make([]*baselineGressRule, 0),
		egressRules:  make([]*baselineGressRule, 0),
	}
	var err error
	banp.subject, err = cloneANPSubject(raw.Spec.Subject)
	if err != nil {
		return nil, err
	}
	addErrors := errors.New("")
	for i, rule := range raw.Spec.Ingress {
		banpRule, err := cloneBANPIngressRule(rule, BANPFlowPriority-int32(i))
		if err != nil {
			addErrors = errors.Wrapf(addErrors, "error: cannot create banp ingress Rule %d in ANP %s - %v",
				i, raw.Name, err)
			continue
		}
		banp.ingressRules = append(banp.ingressRules, banpRule)
	}
	for i, rule := range raw.Spec.Egress {
		banpRule, err := cloneBANPEgressRule(rule, BANPFlowPriority-int32(i))
		if err != nil {
			addErrors = errors.Wrapf(addErrors, "error: cannot create banp egress Rule %d in ANP %s - %v",
				i, raw.Name, err)
			continue
		}
		banp.egressRules = append(banp.egressRules, banpRule)
	}

	if addErrors.Error() == "" {
		addErrors = nil
	}
	return banp, addErrors
}

// shallow copies the BANPRule objects provided.
func cloneBANPIngressRule(raw anpapi.BaselineAdminNetworkPolicyIngressRule, priority int32) (*baselineGressRule, error) {
	banpRule := &baselineGressRule{
		name:        raw.Name,
		priority:    priority,
		action:      raw.Action,
		gressPrefix: BANPIngressPrefix,
		peers:       make([]*adminNetworkPolicyPeer, 0),
		ports:       make([]*adminNetworkPolicyPort, 0),
	}
	for _, peer := range raw.From {
		anpPeer, err := cloneANPPeer(peer)
		if err != nil {
			return nil, err
		}
		banpRule.peers = append(banpRule.peers, anpPeer)
	}
	if raw.Ports != nil {
		for _, port := range *raw.Ports {
			anpPort := cloneANPPort(port)
			banpRule.ports = append(banpRule.ports, anpPort)
		}
	}

	return banpRule, nil
}

// shallow copies the BANPRule objects provided.
func cloneBANPEgressRule(raw anpapi.BaselineAdminNetworkPolicyEgressRule, priority int32) (*baselineGressRule, error) {
	banpRule := &baselineGressRule{
		name:        raw.Name,
		priority:    priority,
		action:      raw.Action,
		gressPrefix: BANPEgressPrefix,
		peers:       make([]*adminNetworkPolicyPeer, 0),
		ports:       make([]*adminNetworkPolicyPort, 0),
	}
	for _, peer := range raw.To {
		banpPeer, err := cloneANPPeer(peer)
		if err != nil {
			return nil, err
		}
		banpRule.peers = append(banpRule.peers, banpPeer)
	}
	if raw.Ports != nil {
		for _, port := range *raw.Ports {
			banpPort := cloneANPPort(port)
			banpRule.ports = append(banpRule.ports, banpPort)
		}
	}
	return banpRule, nil
}

func (c *Controller) syncBaselineAdminNetworkPolicy(key string) error {
	startTime := time.Now()
	_, banpName, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}
	klog.Infof("Processing sync for Baseline Admin Network Policy %s", banpName)

	defer func() {
		klog.V(4).Infof("Finished syncing Baseline Admin Network Policy %s : %v", banpName, time.Since(startTime))
	}()

	banp, err := c.banpLister.Get(banpName)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	klog.Infof("SURYA %s", banp)
	// TODO; Make it more efficient if possible by comparing with cache if we do need to change anything or not.
	// delete existing setup if any and recreate
	err = c.clearBaselineAdminNetworkPolicy(banpName)
	if err != nil {
		return err
	}

	if banp == nil { // it was deleted no need to process further
		return nil
	}

	return c.setBaselineAdminNetworkPolicy(banp)
}

func (c *Controller) clearBaselineAdminNetworkPolicy(banpName string) error {
	obj, loaded := c.banpCache.Load(BANPFlowPriority)
	if !loaded {
		// there is no existing ANP configured with this priority, nothing to clean
		klog.V(4).Infof("BANP %s for priority %d not found in cache", banpName, BANPFlowPriority)
		return nil
	}

	banp := obj.(*baselineAdminNetworkPolicy)
	klog.Infof("SURYA %v", banp)
	banp.Lock()
	defer banp.Unlock()

	// clear NBDB objects for the given ANP (PG, ACLs on that PG, AddrSets used by the ACLs)
	var err error
	// remove PG for Subject (ACLs will get cleaned up automatically)
	portGroupName, readableGroupName := getBaselineAdminNetworkPolicyPGName(banp.name)
	// no need to batch this with address-set deletes since this itself will contain a bunch of ACLs that need to be deleted which is heavy enough.
	err = libovsdbops.DeletePortGroups(c.nbClient, portGroupName)
	if err != nil {
		return fmt.Errorf("unable to delete PG %s for BANP %s: %w", readableGroupName, banp.name, err)
	}
	// remove address-sets that were created for the peers of each rule
	err = c.clearASForPeers(banp.name, libovsdbops.AddressSetBaselineAdminNetworkPolicy)
	if err != nil {
		return fmt.Errorf("failed to delete address-sets for BANP %s: %w", banp.name, err)
	}
	klog.Infof("SURYA %v", err)
	// we can delete the object from the cache now.
	// we also mark it as stale to prevent pod processing if RLock
	// acquired after removal from cache.
	c.banpCache.Delete(BANPFlowPriority)
	banp.stale = true

	return nil
}

func (c *Controller) setBaselineAdminNetworkPolicy(banpObj *anpapi.BaselineAdminNetworkPolicy) error {
	banp, err := cloneBANP(banpObj)
	if err != nil {
		return err
	}

	banp.Lock()
	defer banp.Unlock()
	banp.stale = true // until we finish processing successfully

	// If more than one BANP is created OVNK will only create the first
	// incoming BANP, rest of them will not be created.
	// there should not be an item in the cache for the given priority
	// as we first attempt to delete before create.
	if _, loaded := c.banpCache.LoadOrStore(BANPFlowPriority, banp); loaded {
		// we can ignore the error if status update doesn't succeed; best effort
		_ = c.updateBANPStatusToNotReady(banpObj, PolicyAlreadyExistsInCache)
		return fmt.Errorf("error attempting to add BANP %s when, "+
			"the cluster already has an existing BANP", banpObj.Name)
	}
	klog.Infof("SURYA %v", len(banpObj.Spec.Egress))

	portGroupName, readableGroupName := getBaselineAdminNetworkPolicyPGName(banp.name)
	lsps, err := c.getPortsOfSubject(banp.subject)
	if err != nil {
		// we can ignore the error if status update doesn't succeed; best effort
		_ = c.updateBANPStatusToNotReady(banpObj, PolicyBuildPortGroupFailed)
		return fmt.Errorf("unable to fetch ports for anp %s: %w", banp.name, err)
	}
	ops := []ovsdb.Operation{}
	acls, err := c.buildBANPACLs(banp, portGroupName)
	if err != nil {
		// we can ignore the error if status update doesn't succeed; best effort
		_ = c.updateBANPStatusToNotReady(banpObj, PolicyBuildACLFailed)
		return fmt.Errorf("unable to build acls for anp %s: %w", banp.name, err)
	}
	ops, err = libovsdbops.CreateOrUpdateACLsOps(c.nbClient, ops, acls...)
	if err != nil {
		// we can ignore the error if status update doesn't succeed; best effort
		_ = c.updateBANPStatusToNotReady(banpObj, PolicyCreateUpdateACLFailed)
		return fmt.Errorf("failed to create ACL ops: %v", err)
	}
	pgExternalIDs := map[string]string{BANPExternalIDKey: readableGroupName}
	pg := libovsdbops.BuildPortGroup(portGroupName, lsps, acls, pgExternalIDs)
	ops, err = libovsdbops.CreateOrUpdatePortGroupsOps(c.nbClient, ops, pg)
	if err != nil {
		// we can ignore the error if status update doesn't succeed; best effort
		_ = c.updateBANPStatusToNotReady(banpObj, PolicyCreateUpdatePortGroupFailed)
		return fmt.Errorf("failed to create ops to add port to a port group: %v", err)
	}
	_, err = libovsdbops.TransactAndCheck(c.nbClient, ops)
	if err != nil {
		// we can ignore the error if status update doesn't succeed; best effort
		_ = c.updateBANPStatusToNotReady(banpObj, PolicyTransactFailed)
		return fmt.Errorf("failed to run ovsdb txn to add ports to port group: %v", err)
	}
	// we can ignore the error if status update doesn't succeed; best effort
	_ = c.updateBANPStatusToReady(banpObj)
	banp.stale = false // we can mark it as "ready" now
	return nil
}

func getBaselineAdminNetworkPolicyPGName(name string) (hashedPGName, readablePGName string) {
	return util.HashForOVN(name), name
}

func (c *Controller) buildBANPACLs(banp *baselineAdminNetworkPolicy, pgName string) ([]*nbdb.ACL, error) {
	acls := []*nbdb.ACL{}
	for _, ingressRule := range banp.ingressRules {
		acl, err := c.convertBANPRuleToACL(ingressRule, pgName, banp.name)
		if err != nil {
			return nil, err
		}
		acls = append(acls, acl...)
	}
	for _, egressRule := range banp.egressRules {
		acl, err := c.convertBANPRuleToACL(egressRule, pgName, banp.name)
		if err != nil {
			return nil, err
		}
		acls = append(acls, acl...)
	}

	return acls, nil
}

func (c *Controller) convertBANPRuleToACL(rule *baselineGressRule, pgName, banpName string) ([]*nbdb.ACL, error) {
	// create address-set
	// TODO (tssurya): Revisit this logic to see if its better to do one address-set per peer
	// and join them with OR if that is more perf efficient. Had briefly discussed this OVN team
	// We are not yet clear which is better since both have advantages and disadvantages.
	// Decide this after doing some scale runs.
	var err error
	rule.addrSet, err = c.createASForPeers(rule.peers, rule.priority, rule.gressPrefix, banpName, libovsdbops.AddressSetBaselineAdminNetworkPolicy)
	if err != nil {
		return nil, fmt.Errorf("unable to create address set for "+
			" rule %s with priority %d: %w", rule.name, rule.priority, err)
	}
	// create match based on direction and address-set
	l3Match := constructMatchFromAddressSet(rule.gressPrefix, rule.addrSet)
	// create match based on rule type (ingress/egress) and port-group
	var lportMatch, match, direction string
	var options, extIDs map[string]string
	if rule.gressPrefix == BANPIngressPrefix {
		lportMatch = fmt.Sprintf("(outport == @%s)", pgName)
		direction = nbdb.ACLDirectionToLport
	} else {
		lportMatch = fmt.Sprintf("(inport == @%s)", pgName)
		direction = nbdb.ACLDirectionFromLport
		options = map[string]string{
			"apply-after-lb": "true",
		}
	}
	acls := []*nbdb.ACL{}
	if len(rule.ports) == 0 {
		extIDs = getANPRuleACLDbIDs(banpName, rule.gressPrefix, fmt.Sprintf("%d", rule.priority), controllerName, libovsdbops.ACLBaselineAdminNetworkPolicy).GetExternalIDs()
		match = fmt.Sprintf("%s && %s", l3Match, lportMatch)
		acl := libovsdbops.BuildACL(
			getANPGressPolicyACLName(banpName, rule.gressPrefix, fmt.Sprintf("%d", rule.priority)),
			direction,
			int(rule.priority),
			match,
			getACLActionForBANP(rule.action),
			types.OvnACLLoggingMeter,
			nbdb.ACLSeverityDebug, // TODO: FIX THIS LATER
			false,                 // TODO: FIX THIS LATER
			extIDs,
			options,
			types.DefaultBANPACLTier,
		)
		acls = append(acls, acl)
	} else {
		for i, port := range rule.ports {
			extIDs = getANPRuleACLDbIDs(banpName, rule.gressPrefix, fmt.Sprintf("%d.%d", rule.priority, i), controllerName, libovsdbops.ACLBaselineAdminNetworkPolicy).GetExternalIDs()
			l4Match := constructMatchFromPorts(port)
			match = fmt.Sprintf("%s && %s && %s", l3Match, lportMatch, l4Match)
			acl := libovsdbops.BuildACL(
				getANPGressPolicyACLName(banpName, rule.gressPrefix, fmt.Sprintf("%d.%d", rule.priority, i)),
				direction,
				int(rule.priority),
				match,
				getACLActionForBANP(rule.action),
				types.OvnACLLoggingMeter,
				nbdb.ACLSeverityDebug, // TODO: FIX THIS LATER
				false,                 // TODO: FIX THIS LATER
				extIDs,
				options,
				types.DefaultBANPACLTier,
			)
			acls = append(acls, acl)
		}
	}

	return acls, nil
}

func getACLActionForBANP(action anpapi.BaselineAdminNetworkPolicyRuleAction) string {
	var ovnACLAction string
	switch action {
	case anpapi.BaselineAdminNetworkPolicyRuleActionAllow:
		ovnACLAction = nbdb.ACLActionAllowRelated
	case anpapi.BaselineAdminNetworkPolicyRuleActionDeny:
		ovnACLAction = nbdb.ACLActionDrop
	default:
		panic(fmt.Sprintf("Failed to build BANP ACL: unknown acl action %s", action))
	}
	return ovnACLAction
}
