package networkqos

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"k8s.io/klog/v2"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	libovsdb "github.com/ovn-org/libovsdb/ovsdb"

	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

func (c *Controller) reconcileNetworkQoS(qosState *networkQoSState) ([]*nbdb.QoS, []libovsdb.Operation, error) {
	allQoSes := []*nbdb.QoS{}
	ops := []libovsdb.Operation{}
	ipv4Enabled, ipv6Enabled := c.IPMode()
	for index, rule := range qosState.EgressRules {
		dbIDs := qosState.getDbObjectIDs(c.controllerName, index)
		qos := &nbdb.QoS{
			Action:      map[string]int{},
			Bandwidth:   map[string]int{},
			Direction:   nbdb.QoSDirectionToLport,
			ExternalIDs: dbIDs.GetExternalIDs(),
			Match:       generateNetworkQoSMatch(qosState, rule, ipv4Enabled, ipv6Enabled),
			Priority:    rule.Priority,
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
		allQoSes = append(allQoSes, qos)
	}
	ops, err := libovsdbops.CreateOrUpdateQoSesOps(c.nbClient, ops, allQoSes...)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create QoS operations for %s/%s: %w", qosState.namespace, qosState.name, err)
	}
	return allQoSes, ops, nil
}

// update existing QoS only, no creation
func (c *Controller) updateNetworkQoS(qosState *networkQoSState) error {
	_, ops, err := c.reconcileNetworkQoS(qosState)
	if err != nil {
		return err
	}
	updateOps := []libovsdb.Operation{}
	for _, op := range ops {
		if op.Op != libovsdb.OperationUpdate {
			continue
		}
		updateOps = append(updateOps, op)
	}
	if _, err := libovsdbops.TransactAndCheck(c.nbClient, updateOps); err != nil {
		return err
	}
	return nil
}

func (c *Controller) addQoSToLogicalSwitch(qosState *networkQoSState, switchName string) error {
	qoses, ops, err := c.reconcileNetworkQoS(qosState)
	if err != nil {
		return err
	}
	lsw, err := libovsdbops.FindLogicalSwitchesWithPredicate(c.nbClient, func(item *nbdb.LogicalSwitch) bool {
		return item.Name == switchName
	})
	if err != nil {
		return fmt.Errorf("failed to look up logical switch %s: %w", switchName, err)
	}
	newQoSes := []*nbdb.QoS{}
	for _, qos := range qoses {
		if slices.Contains(lsw[0].QOSRules, qos.UUID) {
			continue
		}
		newQoSes = append(newQoSes, qos)
	}
	ops, err = libovsdbops.AddQoSesToLogicalSwitchOps(c.nbClient, ops, switchName, newQoSes...)
	if err != nil {
		return fmt.Errorf("failed to create operations to add QoS to switch %s: %w", switchName, err)
	}
	if _, err := libovsdbops.TransactAndCheck(c.nbClient, ops); err != nil {
		return fmt.Errorf("failed to execute ops to add QoSes to switch %s, err: %w", switchName, err)
	}

	return nil
}

func (c *Controller) removeUnusedQoSes(qosState *networkQoSState) error {
	qoses, err := libovsdbops.FindQoSesWithPredicate(c.nbClient, func(qos *nbdb.QoS) bool {
		return qos.ExternalIDs[libovsdbops.OwnerControllerKey.String()] == c.controllerName &&
			qos.ExternalIDs[libovsdbops.ObjectNameKey.String()] == qosState.getObjectNameKey()
	})
	if err != nil {
		return fmt.Errorf("failed to look up QoSes for %s/%s: %v", qosState.namespace, qosState.name, err)
	}
	qosMap := map[string]bool{}
	for _, qos := range qoses {
		qosMap[qos.UUID] = true
	}
	switches, err := libovsdbops.FindLogicalSwitchesWithPredicate(c.nbClient, func(ls *nbdb.LogicalSwitch) bool {
		expectedNetworkName := ""
		if c.IsSecondary() {
			expectedNetworkName = c.GetNetworkName()
		}
		if ls.ExternalIDs[types.NetworkExternalID] != expectedNetworkName {
			return false
		}
		hasNodeRef := slices.ContainsFunc(ls.QOSRules, func(switchQosUUID string) bool {
			_, exists := qosMap[switchQosUUID]
			return exists
		})
		if !hasNodeRef {
			return false
		}
		nameParts := strings.Split(ls.Name, "_")
		nodeName := nameParts[0]
		if len(nameParts) > 1 {
			nodeName = nameParts[1]
		}
		_, loaded := qosState.NodeRefs.Load(nodeName)
		return !loaded
	})
	if err != nil {
		return fmt.Errorf("failed to look up unused qoses: %w", err)
	}
	unbindQoSOps := []libovsdb.Operation{}
	for _, ls := range switches {
		ops, err := libovsdbops.RemoveQoSesFromLogicalSwitchOps(c.nbClient, nil, ls.Name, qoses...)
		if err != nil {
			return fmt.Errorf("failed to create ops to remove QoSes from switches %s for NetworkQoS %s/%s: %w", ls.Name, qosState.namespace, qosState.name, err)
		}
		unbindQoSOps = append(unbindQoSOps, ops...)
	}
	if _, err := libovsdbops.TransactAndCheck(c.nbClient, unbindQoSOps); err != nil {
		return fmt.Errorf("failed to execute ops to remove QoSes from logical switches, err: %w", err)
	}
	return nil
}

// remove qos from a list of logical switches
func (c *Controller) removeQoSFromLogicalSwitches(qosState *networkQoSState, switchNames []string) error {
	qoses, err := libovsdbops.FindQoSesWithPredicate(c.nbClient, func(qos *nbdb.QoS) bool {
		return qos.ExternalIDs[libovsdbops.OwnerControllerKey.String()] == c.controllerName &&
			qos.ExternalIDs[libovsdbops.ObjectNameKey.String()] == qosState.getObjectNameKey()
	})
	if err != nil {
		return fmt.Errorf("failed to look up QoSes for %s/%s: %v", qosState.namespace, qosState.name, err)
	}
	unbindQoSOps := []libovsdb.Operation{}
	// remove qos rules from logical switches
	for _, lsName := range switchNames {
		ops, err := libovsdbops.RemoveQoSesFromLogicalSwitchOps(c.nbClient, nil, lsName, qoses...)
		if err != nil {
			return fmt.Errorf("failed to get ops to remove QoSes from switches %s for NetworkQoS %s/%s: %w", lsName, qosState.namespace, qosState.name, err)
		}
		unbindQoSOps = append(unbindQoSOps, ops...)
	}
	if _, err := libovsdbops.TransactAndCheck(c.nbClient, unbindQoSOps); err != nil {
		return fmt.Errorf("failed to execute ops to remove QoSes from logical switches, err: %w", err)
	}
	return nil
}

func (c *Controller) deleteStaleQoSes(qosState *networkQoSState) error {
	existingQoSes, err := libovsdbops.FindQoSesWithPredicate(c.nbClient, func(qos *nbdb.QoS) bool {
		return qos.ExternalIDs[libovsdbops.OwnerControllerKey.String()] == c.controllerName &&
			qos.ExternalIDs[libovsdbops.ObjectNameKey.String()] == qosState.getObjectNameKey()
	})
	if err != nil {
		return fmt.Errorf("error looking up existing QoSes for %s/%s: %v", qosState.namespace, qosState.name, err)
	}
	staleAddressSetNames, err := c.findStaleAddressSets(qosState)
	if err != nil {
		return fmt.Errorf("error looking up stale address sets for %s/%s: %w", qosState.namespace, qosState.name, err)
	}
	switchQoSMap := map[string][]*nbdb.QoS{}
	totalNumOfRules := len(qosState.EgressRules)
	for _, qos := range existingQoSes {
		index := qos.ExternalIDs[libovsdbops.RuleIndex.String()]
		numIndex, convError := strconv.Atoi(index)
		if index != "" && convError == nil && numIndex < totalNumOfRules {
			// rule index is valid and within range
			continue
		}
		// qos is considered stale since the index is out of range
		// get switches that reference to the stale qos
		switches, err := libovsdbops.FindLogicalSwitchesWithPredicate(c.nbClient, func(ls *nbdb.LogicalSwitch) bool {
			return util.SliceHasStringItem(ls.QOSRules, qos.UUID)
		})
		if err != nil {
			if !errors.Is(err, libovsdbclient.ErrNotFound) {
				return fmt.Errorf("error looking up logical switches by qos: %w", err)
			}
			continue
		}
		// build map of switch->list(qos)
		for _, ls := range switches {
			qosList := switchQoSMap[ls.UUID]
			if qosList == nil {
				qosList = []*nbdb.QoS{}
			}
			qosList = append(qosList, qos)
			switchQoSMap[ls.Name] = qosList
		}
		staleAddressSetNames = append(staleAddressSetNames, parseAddressSetNames(qos.Match)...)
	}
	// remove stale address sets
	allOps := []libovsdb.Operation{}
	var addrsetOps []libovsdb.Operation
	addrsetOps, err = libovsdbops.DeleteAddressSetsWithPredicateOps(c.nbClient, addrsetOps, func(item *nbdb.AddressSet) bool {
		return item.ExternalIDs[libovsdbops.OwnerControllerKey.String()] == c.controllerName &&
			item.ExternalIDs[libovsdbops.ObjectNameKey.String()] == qosState.getObjectNameKey() &&
			util.SliceHasStringItem(staleAddressSetNames, item.Name)
	})
	if err != nil {
		return fmt.Errorf("failed to get ops to delete stale address sets for NetworkQoS %s/%s: %w", qosState.namespace, qosState.name, err)
	}
	allOps = append(allOps, addrsetOps...)
	// remove stale qos rules from logical switches
	for lsName, qoses := range switchQoSMap {
		var switchOps []libovsdb.Operation
		switchOps, err = libovsdbops.RemoveQoSesFromLogicalSwitchOps(c.nbClient, switchOps, lsName, qoses...)
		if err != nil {
			return fmt.Errorf("failed to get ops to remove stale QoSes from switches %s for NetworkQoS %s/%s: %w", lsName, qosState.namespace, qosState.name, err)
		}
		allOps = append(allOps, switchOps...)
	}
	// commit allOps
	if _, err := libovsdbops.TransactAndCheck(c.nbClient, allOps); err != nil {
		return fmt.Errorf("failed to execute ops to clean up stale QoSes, err: %w", err)
	}
	return nil
}

// delete ovn QoSes generated from network qos
func (c *Controller) deleteByNetworkQoS(qosState *networkQoSState) error {
	qoses, err := libovsdbops.FindQoSesWithPredicate(c.nbClient, func(qos *nbdb.QoS) bool {
		return qos.ExternalIDs[libovsdbops.OwnerControllerKey.String()] == c.controllerName &&
			qos.ExternalIDs[libovsdbops.ObjectNameKey.String()] == qosState.getObjectNameKey()
	})
	if err != nil {
		return fmt.Errorf("failed to look up QoSes for %s/%s: %v", qosState.namespace, qosState.name, err)
	}
	if err = c.deleteOvnQoSes(qoses); err != nil {
		return fmt.Errorf("error cleaning up OVN QoSes for %s/%s: %v", qosState.namespace, qosState.name, err)
	}
	// remove address sets
	if err = c.deleteAddressSet(qosState.getObjectNameKey()); err != nil {
		return fmt.Errorf("error cleaning up address sets for %s/%s: %w", qosState.namespace, qosState.name, err)
	}
	return nil
}

// delete a list of ovn QoSes
func (c *Controller) deleteOvnQoSes(qoses []*nbdb.QoS) error {
	switchQoSMap := map[string][]*nbdb.QoS{}
	for _, qos := range qoses {
		switches, err := libovsdbops.FindLogicalSwitchesWithPredicate(c.nbClient, func(ls *nbdb.LogicalSwitch) bool {
			return util.SliceHasStringItem(ls.QOSRules, qos.UUID)
		})
		if err != nil {
			if !errors.Is(err, libovsdbclient.ErrNotFound) {
				return fmt.Errorf("failed to look up logical switches by qos: %w", err)
			}
			continue
		}
		// get switches that reference to the stale qoses
		for _, ls := range switches {
			qosList := switchQoSMap[ls.Name]
			if qosList == nil {
				qosList = []*nbdb.QoS{}
			}
			qosList = append(qosList, qos)
			switchQoSMap[ls.Name] = qosList
		}
	}
	unbindQoSOps := []libovsdb.Operation{}
	// remove qos rules from logical switches
	for lsName, qoses := range switchQoSMap {
		ops, err := libovsdbops.RemoveQoSesFromLogicalSwitchOps(c.nbClient, nil, lsName, qoses...)
		if err != nil {
			return fmt.Errorf("failed to get ops to remove QoSes from switch %s: %w", lsName, err)
		}
		unbindQoSOps = append(unbindQoSOps, ops...)
	}
	if _, err := libovsdbops.TransactAndCheck(c.nbClient, unbindQoSOps); err != nil {
		return fmt.Errorf("failed to execute ops to remove QoSes from logical switches, err: %w", err)
	}
	// delete qos
	delQoSOps, err := libovsdbops.DeleteQoSesOps(c.nbClient, nil, qoses...)
	if err != nil {
		return fmt.Errorf("failed to get ops to delete QoSes: %w", err)
	}
	if _, err := libovsdbops.TransactAndCheck(c.nbClient, delQoSOps); err != nil {
		return fmt.Errorf("failed to execute ops to delete QoSes, err: %w", err)
	}
	return nil
}

func (c *Controller) deleteAddressSet(qosName string) error {
	// remove address sets
	delAddrSetOps, err := libovsdbops.DeleteAddressSetsWithPredicateOps(c.nbClient, nil, func(item *nbdb.AddressSet) bool {
		return item.ExternalIDs[libovsdbops.OwnerControllerKey.String()] == c.controllerName &&
			item.ExternalIDs[libovsdbops.ObjectNameKey.String()] == qosName
	})
	if err != nil {
		return fmt.Errorf("failed to get ops to delete address sets: %w", err)
	}
	if _, err := libovsdbops.TransactAndCheck(c.nbClient, delAddrSetOps); err != nil {
		return fmt.Errorf("failed to execute ops to delete address sets, err: %w", err)
	}
	return nil
}

func (c *Controller) findStaleAddressSets(qosState *networkQoSState) ([]string, error) {
	staleAddressSets := []string{}
	addrsets, err := libovsdbops.FindAddressSetsWithPredicate(c.nbClient, func(item *nbdb.AddressSet) bool {
		return item.ExternalIDs[libovsdbops.OwnerControllerKey.String()] == c.controllerName &&
			item.ExternalIDs[libovsdbops.ObjectNameKey.String()] == qosState.getObjectNameKey() &&
			item.ExternalIDs[libovsdbops.RuleIndex.String()] != "src"
	})
	if err != nil {
		return nil, fmt.Errorf("failed to look up address sets: %w", err)
	}
	for _, addrset := range addrsets {
		ruleIndexStr := addrset.ExternalIDs[libovsdbops.RuleIndex.String()]
		ruleIndex, convErr := strconv.Atoi(ruleIndexStr)
		if convErr != nil {
			klog.Errorf("Unable to convert address set's rule-index %s to number: %v", ruleIndexStr, convErr)
			staleAddressSets = append(staleAddressSets, addrset.GetName())
			continue
		}
		destIndexStr := addrset.ExternalIDs[libovsdbops.IpBlockIndexKey.String()]
		destIndex, convErr := strconv.Atoi(destIndexStr)
		if convErr != nil {
			klog.Errorf("Unable to convert address set's ip-block-index %s to number: %v", destIndexStr, convErr)
			staleAddressSets = append(staleAddressSets, addrset.GetName())
			continue
		}
		if ruleIndex >= len(qosState.EgressRules) {
			klog.Errorf("Address set's rule-index %d exceeds total number of rules %d", ruleIndex, len(qosState.EgressRules))
			staleAddressSets = append(staleAddressSets, addrset.GetName())
			continue
		}
		rule := qosState.EgressRules[ruleIndex]
		if rule.Classifier != nil && destIndex >= len(rule.Classifier.Destinations) {
			klog.Errorf("Address set's ip-block-index %d exceeds total number %d", destIndex, len(rule.Classifier.Destinations))
			staleAddressSets = append(staleAddressSets, addrset.GetName())
		}
	}
	return staleAddressSets, nil
}
