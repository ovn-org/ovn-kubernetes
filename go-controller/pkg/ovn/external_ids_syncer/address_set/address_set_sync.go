package address_set

import (
	"fmt"
	"strconv"
	"strings"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	libovsdb "github.com/ovn-org/libovsdb/ovsdb"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/batching"

	"k8s.io/klog/v2"
)

const (
	// legacy constants for address set names
	legacyIPv4AddressSetSuffix = "_v4"
	legacyIPv6AddressSetSuffix = "_v6"

	// OVN-K8S legacy Address Sets Names
	hybridRoutePolicyPrefix = "hybrid-route-pods-"
	egressQoSRulePrefix     = "egress-qos-pods-"
	rulePriorityDelimeter   = "-"
	clusterNodeIP           = "cluster-node-ips"
	egressIPServedPods      = "egressip-served-pods"
	egressServiceServedPods = "egresssvc-served-pods"

	// constants that are still used by the handlers.
	// They are copied here to make sure address set sync has a pre-defined format of objects.
	// If some owner needs to change the ids it is using at some point, it should run after AddressSetsSyncer,
	// and update object based on the syncer format.
	egressFirewallACLExtIdKey          = "egressFirewall"
	egressServiceServedPodsAddrSetName = "egresssvc-served-pods"
	nodeIPAddrSetName                  = "node-ips"
	egressIPServedPodsAddrSetName      = "egressip-served-pods"
	ipv4AddressSetFactoryID            = "v4"
	ipv6AddressSetFactoryID            = "v6"
)

func truncateSuffixFromAddressSet(asName string) (string, string) {
	// Legacy address set names will not have v4 or v6 suffixes.
	// truncate them for the new ones
	if strings.HasSuffix(asName, legacyIPv4AddressSetSuffix) {
		return strings.TrimSuffix(asName, legacyIPv4AddressSetSuffix), legacyIPv4AddressSetSuffix
	}
	if strings.HasSuffix(asName, legacyIPv6AddressSetSuffix) {
		return strings.TrimSuffix(asName, legacyIPv6AddressSetSuffix), legacyIPv6AddressSetSuffix
	}
	return asName, ""
}

type updateAddrSetInfo struct {
	acls       []*nbdb.ACL
	qoses      []*nbdb.QoS
	lrps       []*nbdb.LogicalRouterPolicy
	oldAddrSet *nbdb.AddressSet
	newAddrSet *nbdb.AddressSet
}

type AddressSetsSyncer struct {
	nbClient       libovsdbclient.Client
	controllerName string
	// txnBatchSize is used to control how many address sets will be updated with 1 db transaction.
	txnBatchSize       int
	ignoredAddressSets int
}

// controllerName is the name of the new controller that should own all address sets without controller
func NewAddressSetSyncer(nbClient libovsdbclient.Client, controllerName string) *AddressSetsSyncer {
	return &AddressSetsSyncer{
		nbClient:       nbClient,
		controllerName: controllerName,
		txnBatchSize:   50,
	}
}

// return if address set is owned by network policy and its namespace, name, internal id
func checkIfNetpol(asName string) (netpolOwned bool, namespace, name, direction, idx string) {
	// old format fmt.Sprintf("%s.%s.%s.%d", gp.policyNamespace, gp.policyName, direction, gp.idx)
	// namespace doesn't have dots
	s := strings.Split(asName, ".")
	sLen := len(s)
	// index should be a number
	_, numErr := strconv.Atoi(s[sLen-1])
	if sLen >= 4 && (s[sLen-2] == "ingress" || s[sLen-2] == "egress") && numErr == nil {
		// address set is owned by network policy
		netpolOwned = true
		// namespace doesn't have dots
		namespace = s[0]
		// policyName may have dots, join in that case
		name = strings.Join(s[1:sLen-2], ".")
		direction = s[sLen-2]
		idx = s[sLen-1]
	}
	return
}

func (syncer *AddressSetsSyncer) getEgressIPAddrSetDbIDs(name, network string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.AddressSetEgressIP, syncer.controllerName, map[libovsdbops.ExternalIDKey]string{
		// egress ip creates cluster-wide address sets with egressIpAddrSetName
		libovsdbops.ObjectNameKey: name,
		libovsdbops.NetworkKey:    network,
	})
}

func (syncer *AddressSetsSyncer) getEgressServiceAddrSetDbIDs() *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.AddressSetEgressService, syncer.controllerName, map[libovsdbops.ExternalIDKey]string{
		// egressService has 1 cluster-wide address set
		libovsdbops.ObjectNameKey: egressServiceServedPodsAddrSetName,
	})
}

func (syncer *AddressSetsSyncer) getHybridRouteAddrSetDbIDs(nodeName string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.AddressSetHybridNodeRoute, syncer.controllerName,
		map[libovsdbops.ExternalIDKey]string{
			// there is only 1 address set of this type per node
			libovsdbops.ObjectNameKey: nodeName,
		})
}

func (syncer *AddressSetsSyncer) getEgressQosAddrSetDbIDs(namespace, priority string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.AddressSetEgressQoS, syncer.controllerName, map[libovsdbops.ExternalIDKey]string{
		libovsdbops.ObjectNameKey: namespace,
		// priority is the unique id for address set within given namespace
		libovsdbops.PriorityKey: priority,
	})
}

func (syncer *AddressSetsSyncer) getNetpolAddrSetDbIDs(policyNamespace, policyName, direction, idx string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.AddressSetNetworkPolicy, syncer.controllerName, map[libovsdbops.ExternalIDKey]string{
		libovsdbops.ObjectNameKey: policyNamespace + "_" + policyName,
		// direction and idx uniquely identify address set (= gress policy rule)
		libovsdbops.PolicyDirectionKey: direction,
		libovsdbops.GressIdxKey:        idx,
	})
}

func (syncer *AddressSetsSyncer) getEgressFirewallDNSAddrSetDbIDs(dnsName string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.AddressSetEgressFirewallDNS, syncer.controllerName,
		map[libovsdbops.ExternalIDKey]string{
			// dns address sets are cluster-wide objects, they have unique names
			libovsdbops.ObjectNameKey: dnsName,
		})
}

func (syncer *AddressSetsSyncer) getNamespaceAddrSetDbIDs(namespaceName string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.AddressSetNamespace, syncer.controllerName, map[libovsdbops.ExternalIDKey]string{
		// namespace has only 1 address set, no additional ids are required
		libovsdbops.ObjectNameKey: namespaceName,
	})
}

func buildNewAddressSet(dbIDs *libovsdbops.DbObjectIDs, ipFamily string) *nbdb.AddressSet {
	dbIDsWithIPFam := dbIDs.AddIDs(map[libovsdbops.ExternalIDKey]string{libovsdbops.IPFamilyKey: ipFamily})
	externalIDs := dbIDsWithIPFam.GetExternalIDs()
	name := externalIDs[libovsdbops.PrimaryIDKey.String()]
	as := &nbdb.AddressSet{
		Name:        util.HashForOVN(name),
		ExternalIDs: externalIDs,
	}
	return as
}

// getReferencingObjsAndNewDbIDs finds all object that reference stale address set and tries to create a new dbIDs
// based on referencing objects
func (syncer *AddressSetsSyncer) getReferencingObjsAndNewDbIDs(oldHash, oldName string) (acls []*nbdb.ACL,
	qoses []*nbdb.QoS, lrps []*nbdb.LogicalRouterPolicy, dbIDs *libovsdbops.DbObjectIDs, err error) {
	// find all referencing objects
	aclPred := func(acl *nbdb.ACL) bool {
		return strings.Contains(acl.Match, "$"+oldHash)
	}
	acls, err = libovsdbops.FindACLsWithPredicate(syncer.nbClient, aclPred)
	if err != nil {
		err = fmt.Errorf("failed to find acls for address set %s: %v", oldHash, err)
		return
	}
	qosPred := func(qos *nbdb.QoS) bool {
		return strings.Contains(qos.Match, "$"+oldHash)
	}
	qoses, err = libovsdbops.FindQoSesWithPredicate(syncer.nbClient, qosPred)
	if err != nil {
		err = fmt.Errorf("failed to find qoses for address set %s: %v", oldHash, err)
		return
	}
	lrpPred := func(lrp *nbdb.LogicalRouterPolicy) bool {
		return strings.Contains(lrp.Match, "$"+oldHash)
	}
	lrps, err = libovsdbops.FindLogicalRouterPoliciesWithPredicate(syncer.nbClient, lrpPred)
	if err != nil {
		err = fmt.Errorf("failed to find lrps for address set %s: %v", oldHash, err)
		return
	}
	// build dbIDs
	switch {
	// Filter address sets with pre-defined names
	case oldName == clusterNodeIP:
		dbIDs = syncer.getEgressIPAddrSetDbIDs(nodeIPAddrSetName, "default")
	case oldName == egressIPServedPods:
		dbIDs = syncer.getEgressIPAddrSetDbIDs(egressIPServedPodsAddrSetName, "default")
	case oldName == egressServiceServedPods:
		dbIDs = syncer.getEgressServiceAddrSetDbIDs()
	// HybridNodeRoute and EgressQoS address sets have specific prefixes
	// Try to parse dbIDs from address set name
	case strings.HasPrefix(oldName, hybridRoutePolicyPrefix):
		// old name has format types.hybridRoutePolicyPrefix + node
		nodeName := oldName[len(hybridRoutePolicyPrefix):]
		dbIDs = syncer.getHybridRouteAddrSetDbIDs(nodeName)
	case strings.HasPrefix(oldName, egressQoSRulePrefix):
		// oldName has format fmt.Sprintf("%s%s%s%d", egressQoSRulePrefix, namespace, rulePriorityDelimeter, priority)
		// we extract the namespace from the id by removing the prefix and the priority suffix
		// egress-qos-pods-my-namespace-123 -> my-namespace
		namespaceWithPrio := oldName[len(egressQoSRulePrefix):]
		// namespaceWithPrio = my-namespace-123
		delIndex := strings.LastIndex(namespaceWithPrio, rulePriorityDelimeter)
		ns := namespaceWithPrio[:delIndex]
		priority := namespaceWithPrio[delIndex+1:]
		dbIDs = syncer.getEgressQosAddrSetDbIDs(ns, priority)
	default:
		// netpol address set has a specific name format <namespace>.<name>.<direction>.<idx>
		netpolOwned, namespace, name, direction, idx := checkIfNetpol(oldName)
		if netpolOwned {
			dbIDs = syncer.getNetpolAddrSetDbIDs(namespace, name, direction, idx)
		} else {
			// we have only egress firewall dns and namespace address sets left
			// try to distinguish them by referencing acls
			if len(acls) > 0 {
				// if given address set is owned by egress firewall, all ACLs will be owned by the same object
				acl := acls[0]
				// check if egress firewall dns is the owner
				// the only address set that may be referenced in egress firewall destination is dns address set
				if acl.ExternalIDs[egressFirewallACLExtIdKey] != "" && strings.Contains(acl.Match, ".dst == $"+oldHash) {
					dbIDs = syncer.getEgressFirewallDNSAddrSetDbIDs(oldName)
				}
			}
			if dbIDs == nil {
				// we failed to find the owner, assume everything else to be owned by namespace,
				// since it doesn't have any specific-linked objects,
				// oldName is just namespace name
				dbIDs = syncer.getNamespaceAddrSetDbIDs(oldName)
			}
		}
	}
	// dbIDs is set
	return
}

func (syncer *AddressSetsSyncer) getUpdateAddrSetOps(addrSetsInfo []*updateAddrSetInfo) (ops []libovsdb.Operation, err error) {
	// one referencing object may contain multiple references that need to be updated
	// these maps are used to track referenced that need to be replaced for every object type
	aclsToUpdate := map[string]*nbdb.ACL{}
	qosesToUpdate := map[string]*nbdb.QoS{}
	lrpsToUpdate := map[string]*nbdb.LogicalRouterPolicy{}

	for _, addrSetInfo := range addrSetsInfo {
		if addrSetInfo.newAddrSet == nil {
			// new address set wasn't built
			if len(addrSetInfo.acls) == 0 && len(addrSetInfo.qoses) == 0 && len(addrSetInfo.lrps) == 0 {
				// address set is stale and not referenced, clean up
				ops, err = libovsdbops.DeleteAddressSetsOps(syncer.nbClient, ops, addrSetInfo.oldAddrSet)
			} else {
				syncer.ignoredAddressSets += 1
			}
			continue
		}

		oldName := addrSetInfo.oldAddrSet.ExternalIDs["name"]
		// create updated address set
		ops, err = libovsdbops.CreateOrUpdateAddressSetsOps(syncer.nbClient, ops, addrSetInfo.newAddrSet)
		if err != nil {
			return nil, fmt.Errorf("failed to get update address set ops for address set %s: %v", oldName, err)
		}
		// delete old address set
		ops, err = libovsdbops.DeleteAddressSetsOps(syncer.nbClient, ops, addrSetInfo.oldAddrSet)
		if err != nil {
			return nil, fmt.Errorf("failed to get update address set ops for address set %s: %v", oldName, err)
		}
		oldHash := "$" + addrSetInfo.oldAddrSet.Name
		newHash := "$" + addrSetInfo.newAddrSet.Name

		for _, acl := range addrSetInfo.acls {
			if _, ok := aclsToUpdate[acl.UUID]; !ok {
				aclsToUpdate[acl.UUID] = acl
			}
			aclsToUpdate[acl.UUID].Match = strings.ReplaceAll(aclsToUpdate[acl.UUID].Match, oldHash, newHash)
		}

		for _, qos := range addrSetInfo.qoses {
			if _, ok := qosesToUpdate[qos.UUID]; !ok {
				qosesToUpdate[qos.UUID] = qos
			}
			qosesToUpdate[qos.UUID].Match = strings.ReplaceAll(qosesToUpdate[qos.UUID].Match, oldHash, newHash)
		}

		for _, lrp := range addrSetInfo.lrps {
			if _, ok := lrpsToUpdate[lrp.UUID]; !ok {
				lrpsToUpdate[lrp.UUID] = lrp
			}
			lrpsToUpdate[lrp.UUID].Match = strings.ReplaceAll(lrpsToUpdate[lrp.UUID].Match, oldHash, newHash)
		}
	}

	for _, acl := range aclsToUpdate {
		ops, err = libovsdbops.UpdateACLsOps(syncer.nbClient, ops, acl)
		if err != nil {
			return nil, fmt.Errorf("failed to get update acl ops: %v", err)
		}
	}
	for _, qos := range qosesToUpdate {
		ops, err = libovsdbops.UpdateQoSesOps(syncer.nbClient, ops, qos)
		if err != nil {
			return nil, fmt.Errorf("failed to get update qos ops: %v", err)
		}
	}
	for _, lrp := range lrpsToUpdate {
		ops, err = libovsdbops.UpdateLogicalRouterPoliciesOps(syncer.nbClient, ops, lrp)
		if err != nil {
			return nil, fmt.Errorf("failed to get update LRPs ops: %v", err)
		}
	}
	return
}

// getAddrSetUpdateInfo adds db ops to update address set and objects that reference it
func (syncer *AddressSetsSyncer) getAddrSetUpdateInfo(as *nbdb.AddressSet) (*updateAddrSetInfo, error) {
	oldName, ipSuffix := truncateSuffixFromAddressSet(as.ExternalIDs["name"])
	// oldName may be empty if address set doesn't have ExternalID set
	acls, qoses, lrps, dbIDs, err := syncer.getReferencingObjsAndNewDbIDs(as.Name, oldName)
	if err != nil {
		return nil, fmt.Errorf("failed to get new dbIDs for address set %s: %v", oldName, err)
	}
	updateAddrSet := &updateAddrSetInfo{acls, qoses, lrps, as, nil}

	if oldName == "" {
		klog.Infof("external_ids->name missing, stale address set %s", as.Name)
		return updateAddrSet, nil
	}
	if ipSuffix == "" {
		klog.Infof("Found stale address set %s without ip family suffix and empty ips list", oldName)
		return updateAddrSet, nil
	}
	var nbdbAS *nbdb.AddressSet
	if ipSuffix == legacyIPv4AddressSetSuffix {
		nbdbAS = buildNewAddressSet(dbIDs, ipv4AddressSetFactoryID)
	} else {
		nbdbAS = buildNewAddressSet(dbIDs, ipv6AddressSetFactoryID)
	}
	// since we need to update addressSet.Name, which is an index and not listed in getNonZeroAddressSetMutableFields,
	// we copy existing addressSet, update it address set needed, and replace (delete and create) existing address set with the updated
	newAS := as.DeepCopy()
	// reset UUID
	newAS.UUID = ""
	// update address set Name
	newAS.Name = nbdbAS.Name
	// remove old "name" ExternalID
	delete(newAS.ExternalIDs, "name")
	// Insert new externalIDs generated by address set factory
	for key, value := range nbdbAS.ExternalIDs {
		newAS.ExternalIDs[key] = value
	}
	updateAddrSet.newAddrSet = newAS
	return updateAddrSet, nil
}

func (syncer *AddressSetsSyncer) SyncAddressSets() error {
	// stale address sets don't have controller ID
	p := libovsdbops.GetNoOwnerPredicate[*nbdb.AddressSet]()
	addrSetList, err := libovsdbops.FindAddressSetsWithPredicate(syncer.nbClient, p)
	if err != nil {
		return fmt.Errorf("failed to find stale address sets: %v", err)
	}

	err = batching.Batch[*nbdb.AddressSet](syncer.txnBatchSize, addrSetList, func(batchAddrSets []*nbdb.AddressSet) error {
		addrSetInfos := []*updateAddrSetInfo{}
		for _, addrSet := range batchAddrSets {
			updateInfo, err := syncer.getAddrSetUpdateInfo(addrSet)
			if err != nil {
				return err
			}
			addrSetInfos = append(addrSetInfos, updateInfo)
		}
		// generate update ops
		ops, err := syncer.getUpdateAddrSetOps(addrSetInfos)
		if err != nil {
			return fmt.Errorf("failed to get update address sets ops: %w", err)
		}
		_, err = libovsdbops.TransactAndCheck(syncer.nbClient, ops)
		if err != nil {
			return fmt.Errorf("failed to transact address set sync ops: %v", err)
		}
		return nil
	})
	klog.Infof("SyncAddressSets found %d stale address sets, %d of them were ignored",
		len(addrSetList), syncer.ignoredAddressSets)
	return err
}
