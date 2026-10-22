// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package networkqos

import (
	"errors"
	"fmt"
	"maps"
	"net"
	"slices"
	"sync"

	"k8s.io/apimachinery/pkg/util/sets"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// Switch creation requeues policies waiting for it.
var errLogicalSwitchMissing = errors.New("logical switch not created yet")

// Look up rules by their indexed IDs when the network is known.
// An empty ownerController means search all networks.
func (c *Controller) findOwnedQoSes(ownerController, objectName string) ([]*nbdb.QoS, error) {
	if ownerController == "" {
		qoses, err := libovsdbops.FindQoSesWithPredicate(c.nbClient, func(qos *nbdb.QoS) bool {
			return isNetworkQoSObject(qos.ExternalIDs, ownerController, objectName)
		})
		if err != nil {
			return nil, fmt.Errorf("failed to look up the QoS rules of %s: %w", objectName, err)
		}
		return qoses, nil
	}
	var qoses []*nbdb.QoS
	for ruleIndex := range types.NetworkQoSMaxRules {
		for _, id := range []string{ruleStringID(ruleIndex), returnPassRuleID(ruleIndex)} {
			qos, err := libovsdbops.GetQoS(c.nbClient, &nbdb.QoS{
				ExternalIDs: qosDbObjectIDs(ownerController, objectName, id).GetExternalIDs(),
			})
			if err != nil {
				if errors.Is(err, libovsdbclient.ErrNotFound) {
					continue
				}
				return nil, fmt.Errorf("failed to look up QoS rule %s of %s: %w", id, objectName, err)
			}
			qoses = append(qoses, qos)
		}
	}
	return qoses, nil
}

func (c *Controller) switchesReferencingQoSes(qoses []*nbdb.QoS) (map[string][]*nbdb.QoS, error) {
	switchQoSes := map[string][]*nbdb.QoS{}
	if len(qoses) == 0 {
		return switchQoSes, nil
	}
	byUUID := make(map[string]*nbdb.QoS, len(qoses))
	for _, qos := range qoses {
		byUUID[qos.UUID] = qos
	}
	_, err := libovsdbops.FindLogicalSwitchesWithPredicate(c.nbClient, func(ls *nbdb.LogicalSwitch) bool {
		for _, uuid := range ls.QOSRules {
			if qos, found := byUUID[uuid]; found {
				switchQoSes[ls.Name] = append(switchQoSes[ls.Name], qos)
			}
		}
		return false
	})
	if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
		return nil, fmt.Errorf("failed to look up the logical switches referencing QoS rules: %w", err)
	}
	return switchQoSes, nil
}

// Bind rules even with no source pods because an empty source set matches no traffic.
// Returns the rule IDs of the rows bound to the switch.
func (c *Controller) bindQoSToLocalSwitch(qosState *networkQoSState, source addrSetRef, rules []*GressRule,
	existing []*nbdb.QoS, nctx *networkContext) (sets.Set[string], error) {
	bound := sets.New[string]()
	if len(rules) == 0 {
		return bound, nil
	}
	lsw, err := libovsdbops.GetLogicalSwitch(c.nbClient, &nbdb.LogicalSwitch{Name: qosState.switchName})
	if err != nil {
		if errors.Is(err, libovsdbclient.ErrNotFound) {
			return nil, errLogicalSwitchMissing
		}
		return nil, fmt.Errorf("failed to look up logical switch %s: %w", qosState.switchName, err)
	}
	existingByID := map[string]*nbdb.QoS{}
	for _, qos := range existing {
		existingByID[qos.ExternalIDs[libovsdbops.RuleIndex.String()]] = qos
	}
	ipv4Enabled, ipv6Enabled := nctx.netInfo.IPMode()
	routerMACs, err := c.routerMACs(lsw)
	if err != nil {
		return nil, err
	}
	// Every rule skips the same pass. Otherwise a lower-priority rule could match the other
	// pass and override the DSCP of the rule that applies.
	exclusion := routerReturnExclusion(routerMACs)
	// Add retrunPass to avoid DSCP override by lower-priority EgressQoS rules.
	returnPass := ""
	if nctx.qosDirection() == nbdb.QoSDirectionFromLport {
		exclusion = routerReentryExclusion(routerMACs, networkSubnets(nctx.netInfo), ipv4Enabled, ipv6Enabled)
	} else if !nctx.netInfo.IsUserDefinedNetwork() {
		returnPass = routerReturnMatch(routerMACs)
	}
	qoses := make([]*nbdb.QoS, 0, len(rules))
	changed := []*nbdb.QoS{}
	add := func(qos *nbdb.QoS) {
		if nctx.netInfo.IsUserDefinedNetwork() {
			qos.ExternalIDs[types.NetworkExternalID] = nctx.netInfo.GetNetworkName()
		}
		id := qos.ExternalIDs[libovsdbops.RuleIndex.String()]
		if current := existingByID[id]; current != nil && qosEqual(current, qos) {
			qos.UUID = current.UUID
		} else {
			changed = append(changed, qos)
		}
		qoses = append(qoses, qos)
		bound.Insert(id)
	}
	for index, rule := range rules {
		match := generateNetworkQoSMatch(source, rule, ipv4Enabled, ipv6Enabled)
		qos := &nbdb.QoS{
			Action:      map[string]int{},
			Bandwidth:   map[string]int{},
			Direction:   nctx.qosDirection(),
			ExternalIDs: qosState.getDbObjectIDs(ruleStringID(index)).GetExternalIDs(),
			Match:       match,
			Priority:    rule.Priority,
		}
		if exclusion != "" {
			qos.Match += " && " + exclusion
		}
		if rule.Dscp >= 0 {
			qos.Action[nbdb.QoSActionDSCP] = rule.Dscp
		}
		if rule.Rate != nil && *rule.Rate > 0 {
			qos.Bandwidth[nbdb.QoSBandwidthRate] = *rule.Rate
		}
		if rule.Burst != nil && *rule.Burst > 0 {
			qos.Bandwidth[nbdb.QoSBandwidthBurst] = *rule.Burst
		}
		add(qos)
		if returnPass != "" {
			add(&nbdb.QoS{
				Action:      maps.Clone(qos.Action),
				Bandwidth:   map[string]int{},
				Direction:   qos.Direction,
				ExternalIDs: qosState.getDbObjectIDs(returnPassRuleID(index)).GetExternalIDs(),
				Match:       match + " && " + returnPass,
				Priority:    rule.Priority,
			})
		}
	}
	// New rows get named UUIDs so the same transaction can bind them to the switch.
	ops, err := libovsdbops.CreateOrUpdateQoSesOps(c.nbClient, nil, changed...)
	if err != nil {
		return nil, fmt.Errorf("failed to create QoS operations for %s/%s: %w", qosState.namespace, qosState.name, err)
	}
	unbound := []*nbdb.QoS{}
	for _, qos := range qoses {
		if !slices.Contains(lsw.QOSRules, qos.UUID) {
			unbound = append(unbound, qos)
		}
	}
	if len(unbound) > 0 {
		ops, err = libovsdbops.AddQoSesToLogicalSwitchOps(c.nbClient, ops, qosState.switchName, unbound...)
		if err != nil {
			return nil, fmt.Errorf("failed to create operations to add QoS to switch %s: %w", qosState.switchName, err)
		}
	}
	if len(ops) == 0 {
		return bound, nil
	}
	if _, err := libovsdbops.TransactAndCheck(c.nbClient, ops); err != nil {
		return nil, fmt.Errorf("failed to execute ops to add QoSes to switch %s, err: %w", qosState.switchName, err)
	}
	return bound, nil
}

const logicalSwitchPortTypeRouter = "router"

// MACs of the routers attached to the switch. Skip a port whose peer does not exist yet;
// creating the peer requeues the policy.
func (c *Controller) routerMACs(lsw *nbdb.LogicalSwitch) ([]string, error) {
	ports := sets.New(lsw.Ports...)
	routerPorts, err := libovsdbops.FindLogicalSwitchPortWithPredicate(c.nbClient, func(lsp *nbdb.LogicalSwitchPort) bool {
		return lsp.Type == logicalSwitchPortTypeRouter && ports.Has(lsp.UUID)
	})
	if err != nil {
		return nil, fmt.Errorf("failed to look up the router ports of logical switch %s: %w", lsw.Name, err)
	}
	macs := sets.New[string]()
	for _, lsp := range routerPorts {
		lrpName := lsp.Options[libovsdbops.RouterPort]
		if lrpName == "" {
			continue
		}
		lrp, err := libovsdbops.GetLogicalRouterPort(c.nbClient, &nbdb.LogicalRouterPort{Name: lrpName})
		if err != nil {
			if errors.Is(err, libovsdbclient.ErrNotFound) {
				continue
			}
			return nil, fmt.Errorf("failed to look up logical router port %s: %w", lrpName, err)
		}
		if lrp.MAC != "" {
			macs.Insert(lrp.MAC)
		}
	}
	return sets.List(macs), nil
}

func networkSubnets(netInfo util.NetInfo) []*net.IPNet {
	subnets := make([]*net.IPNet, 0, len(netInfo.Subnets()))
	for _, subnet := range netInfo.Subnets() {
		subnets = append(subnets, subnet.CIDR)
	}
	return subnets
}

func qosEqual(current, desired *nbdb.QoS) bool {
	return current.Direction == desired.Direction &&
		current.Match == desired.Match &&
		current.Priority == desired.Priority &&
		maps.Equal(current.Action, desired.Action) &&
		maps.Equal(current.Bandwidth, desired.Bandwidth) &&
		maps.Equal(current.ExternalIDs, desired.ExternalIDs)
}

// `existing` contains the rules read before reconciliation, and bound the rule IDs of the rows
// bound since. With checkBindings, also remove bindings to other switches.
func (c *Controller) cleanupStaleOvnObjects(qosState *networkQoSState, bound sets.Set[string], existing []*nbdb.QoS,
	checkBindings bool) error {
	staleQoSes := []*nbdb.QoS{}
	inUse := sets.New[string]()
	for _, qos := range existing {
		if bound.Has(qos.ExternalIDs[libovsdbops.RuleIndex.String()]) {
			inUse.Insert(qos.UUID)
		} else {
			staleQoSes = append(staleQoSes, qos)
		}
	}
	if len(staleQoSes) == 0 && !checkBindings {
		return nil
	}
	switchQoSes, err := c.switchesReferencingQoSes(existing)
	if err != nil {
		return err
	}
	var ops []ovsdb.Operation
	for _, lsName := range slices.Sorted(maps.Keys(switchQoSes)) {
		unbind := slices.DeleteFunc(slices.Clone(switchQoSes[lsName]), func(qos *nbdb.QoS) bool {
			return lsName == qosState.switchName && inUse.Has(qos.UUID)
		})
		if len(unbind) == 0 {
			continue
		}
		ops, err = libovsdbops.RemoveQoSesFromLogicalSwitchOps(c.nbClient, ops, lsName, unbind...)
		if err != nil {
			return fmt.Errorf("failed to get ops to remove stale QoSes from switch %s for NetworkQoS %s/%s: %w", lsName, qosState.namespace, qosState.name, err)
		}
	}
	ops, err = libovsdbops.DeleteQoSesOps(c.nbClient, ops, staleQoSes...)
	if err != nil {
		return fmt.Errorf("failed to get ops to delete stale QoSes: %w", err)
	}
	if _, err := libovsdbops.TransactAndCheck(c.nbClient, ops); err != nil {
		return fmt.Errorf("failed to execute ops to remove stale QoSes, err: %w", err)
	}
	return nil
}

func (c *Controller) cleanupUnselectedNetworks(objectName string, networks map[string]util.NetInfo) error {
	selectedControllers := sets.New[string]()
	for networkName := range networks {
		selectedControllers.Insert(networkControllerName(networkName))
	}
	qoses, err := libovsdbops.FindQoSesWithPredicate(c.nbClient, func(qos *nbdb.QoS) bool {
		return isNetworkQoSObject(qos.ExternalIDs, "", objectName) &&
			!selectedControllers.Has(qos.ExternalIDs[libovsdbops.OwnerControllerKey.String()])
	})
	if err != nil {
		return fmt.Errorf("failed to look up QoSes on unselected networks for %s: %w", objectName, err)
	}
	if err = c.deleteOvnQoSes(qoses); err != nil {
		return err
	}
	return c.deleteLegacyAddressSets(objectName, func(ownerController string) bool {
		return !selectedControllers.Has(ownerController)
	})
}

// An empty ownerController deletes across all networks.
func (c *Controller) deleteByObjectName(ownerController, objectName string) error {
	qoses, err := c.findOwnedQoSes(ownerController, objectName)
	if err != nil {
		return err
	}
	if err = c.deleteOvnQoSes(qoses); err != nil {
		return fmt.Errorf("error cleaning up OVN QoSes for %s: %w", objectName, err)
	}
	if err = c.deleteLegacyAddressSets(objectName, func(controller string) bool {
		return ownerController == "" || controller == ownerController
	}); err != nil {
		return fmt.Errorf("error cleaning up address sets for %s: %w", objectName, err)
	}
	return nil
}

func (c *Controller) deleteOvnQoSes(qoses []*nbdb.QoS) error {
	if len(qoses) == 0 {
		return nil
	}
	switchQoSes, err := c.switchesReferencingQoSes(qoses)
	if err != nil {
		return err
	}
	var ops []ovsdb.Operation
	for _, lsName := range slices.Sorted(maps.Keys(switchQoSes)) {
		ops, err = libovsdbops.RemoveQoSesFromLogicalSwitchOps(c.nbClient, ops, lsName, switchQoSes[lsName]...)
		if err != nil {
			return fmt.Errorf("failed to get ops to remove QoSes from switch %s: %w", lsName, err)
		}
	}
	ops, err = libovsdbops.DeleteQoSesOps(c.nbClient, ops, qoses...)
	if err != nil {
		return fmt.Errorf("failed to get ops to delete QoSes: %w", err)
	}
	if _, err := libovsdbops.TransactAndCheck(c.nbClient, ops); err != nil {
		return fmt.Errorf("failed to execute ops to delete QoSes, err: %w", err)
	}
	return nil
}

// Use the index built at startup instead of scanning the database on every reconciliation.
func (c *Controller) deleteLegacyAddressSets(objectName string, owned func(ownerController string) bool) error {
	addrSets := c.legacyAddressSets.take(objectName, owned)
	if len(addrSets) == 0 {
		return nil
	}
	ops, err := libovsdbops.DeleteAddressSetsOps(c.nbClient, nil, addrSets...)
	if err == nil {
		_, err = libovsdbops.TransactAndCheck(c.nbClient, ops)
	}
	if err != nil {
		c.legacyAddressSets.add(objectName, addrSets...)
		return fmt.Errorf("failed to delete the address sets the per-network controllers owned for %s: %w", objectName, err)
	}
	return nil
}

// Address sets left by the old per-network controllers, indexed by policy name at startup.
// No new sets are added after startup.
type legacyAddressSets struct {
	sync.Mutex
	byObject map[string][]*nbdb.AddressSet
}

func (l *legacyAddressSets) add(objectName string, addrSets ...*nbdb.AddressSet) {
	l.Lock()
	defer l.Unlock()
	if l.byObject == nil {
		l.byObject = map[string][]*nbdb.AddressSet{}
	}
	l.byObject[objectName] = append(l.byObject[objectName], addrSets...)
}

func (l *legacyAddressSets) take(objectName string, owned func(ownerController string) bool) []*nbdb.AddressSet {
	l.Lock()
	defer l.Unlock()
	var taken, kept []*nbdb.AddressSet
	for _, addrSet := range l.byObject[objectName] {
		if owned(addrSet.ExternalIDs[libovsdbops.OwnerControllerKey.String()]) {
			taken = append(taken, addrSet)
		} else {
			kept = append(kept, addrSet)
		}
	}
	if len(kept) == 0 {
		delete(l.byObject, objectName)
	} else {
		l.byObject[objectName] = kept
	}
	return taken
}
