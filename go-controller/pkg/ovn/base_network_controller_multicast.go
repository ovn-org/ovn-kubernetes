// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"

	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
)

type defaultMcastACLTypeID string

const (
	// IPv6 multicast traffic destined to dynamic groups must have the "T" bit
	// set to 1: https://tools.ietf.org/html/rfc3307#section-4.3
	ipv6DynamicMulticastMatch                       = "(ip6.dst[120..127] == 0xff && ip6.dst[116] == 1)"
	mcastDefaultDenyID        defaultMcastACLTypeID = "DefaultDeny"
	mcastAllowInterNodeID     defaultMcastACLTypeID = "AllowInterNode"
)

// Legacy const, should only be used in sync and tests
const (
	// Legacy multicastDefaultDeny port group removed by commit 40a90f0
	legacyMulticastDefaultDenyPortGroup = "mcastPortGroupDeny"
)

func getACLMatchAF(ipv4Match, ipv6Match string, ipv4Mode, ipv6Mode bool) string {
	if ipv4Mode && ipv6Mode {
		return "(" + ipv4Match + " || " + ipv6Match + ")"
	} else if ipv4Mode {
		return ipv4Match
	} else {
		return ipv6Match
	}
}

// Creates the match string used for ACLs matching on multicast traffic.
func getMulticastACLMatch() string {
	return "(ip4.mcast || mldv1 || mldv2 || " + ipv6DynamicMulticastMatch + ")"
}

// Allow IGMP traffic (e.g., IGMP queries) and namespace multicast traffic
// towards pods.
func getMulticastACLIgrMatchV4(addrSetName string) string {
	return "(igmp || (ip4.src == $" + addrSetName + " && ip4.mcast))"
}

// Allow MLD traffic (e.g., MLD queries) and namespace multicast traffic
// towards pods.
func getMulticastACLIgrMatchV6(addrSetName string) string {
	return "(mldv1 || mldv2 || (ip6.src == $" + addrSetName + " && " + ipv6DynamicMulticastMatch + "))"
}

func getMulticastAddrsetBackref(namespace string) string {
	return fmt.Sprintf("%v/%v", "Multicast", namespace)
}

// Creates the match string used for ACLs allowing incoming multicast into a
// namespace, that is, from IPs that are in the namespace's address set.
func (bnc *BaseNetworkController) getMulticastACLIgrMatch(addrSetNameV4, addrSetNameV6 string) string {
	var ipv4Match, ipv6Match string
	ipv4Mode, ipv6Mode := bnc.IPMode()
	if ipv4Mode {
		ipv4Match = getMulticastACLIgrMatchV4(addrSetNameV4)
	}
	if ipv6Mode {
		ipv6Match = getMulticastACLIgrMatchV6(addrSetNameV6)
	}
	return getACLMatchAF(ipv4Match, ipv6Match, ipv4Mode, ipv6Mode)
}

// Creates the match string used for ACLs allowing outgoing multicast from a
// namespace.
func (bnc *BaseNetworkController) getMulticastACLEgrMatch() string {
	var ipv4Match, ipv6Match string
	ipv4Mode, ipv6Mode := bnc.IPMode()
	if ipv4Mode {
		ipv4Match = "ip4.mcast"
	}
	if ipv6Mode {
		ipv6Match = "(mldv1 || mldv2 || " + ipv6DynamicMulticastMatch + ")"
	}
	return getACLMatchAF(ipv4Match, ipv6Match, ipv4Mode, ipv6Mode)
}

func getDefaultMcastACLDbIDs(mcastType defaultMcastACLTypeID, aclDir libovsdbutil.ACLDirection, controller string) *libovsdbops.DbObjectIDs {
	// there are 2 types of default multicast ACLs in every direction (Ingress/Egress)
	// DefaultDeny = deny multicast by default
	// AllowInterNode = allow inter-node multicast
	return libovsdbops.NewDbObjectIDs(libovsdbops.ACLMulticastCluster, controller,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.TypeKey:            string(mcastType),
			libovsdbops.PolicyDirectionKey: string(aclDir),
		})

}

func getNamespaceMcastACLDbIDs(ns string, aclDir libovsdbutil.ACLDirection, controller string) *libovsdbops.DbObjectIDs {
	// namespaces ACL
	return libovsdbops.NewDbObjectIDs(libovsdbops.ACLMulticastNamespace, controller,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey:      ns,
			libovsdbops.PolicyDirectionKey: string(aclDir),
		})
}

// Creates a policy to allow multicast traffic within 'ns':
//   - a port group containing all logical ports associated with 'ns'
//   - one "from-lport" ACL allowing egress multicast traffic from the pods
//     in 'ns'
//   - one "to-lport" ACL allowing ingress multicast traffic to pods in 'ns'.
//     This matches only traffic originated by pods in 'ns' (based on the
//     namespace address set).
func (bnc *BaseNetworkController) createMulticastAllowPolicy(ns string, nsInfo *namespaceInfo) error {
	portGroupName := bnc.getNamespacePortGroupName(ns)
	// we use legacyNetpolMode to avoid adding hostNetwork pods (aka node) IPs.
	// Another side-effect of using legacyNetpolMode is that HostNetworkNamespace could be matched,
	// but since we are using static namespace and not a selector, this will never happen
	// (unless multicast is enabled on HostNetworkNamespace, which should never happen)
	asKey, addrSetNameV4, addrSetNameV6, err := bnc.addressSetManager.EnsureAddressSet(
		&metav1.LabelSelector{}, nil, nil, ns, getMulticastAddrsetBackref(ns),
		bnc.controllerName, bnc.GetNetInfo(), true)
	// even if EnsureAddressSet failed, add key for future cleanup or retry.
	nsInfo.addrSetOwnerBackref = asKey
	nsInfo.addrSetNameV4 = addrSetNameV4
	nsInfo.addrSetNameV6 = addrSetNameV6
	if err != nil {
		return fmt.Errorf("unable to ensure address set for namespace %s: %v", ns, err)
	}

	aclDir := libovsdbutil.ACLEgress
	egressMatch := libovsdbutil.GetACLMatch(portGroupName, bnc.getMulticastACLEgrMatch(), aclDir)
	dbIDs := getNamespaceMcastACLDbIDs(ns, aclDir, bnc.controllerName)
	aclPipeline := libovsdbutil.ACLDirectionToACLPipeline(aclDir)
	egressACL := libovsdbutil.BuildACLWithDefaultTier(dbIDs, types.DefaultMcastAllowPriority, egressMatch, nbdb.ACLActionAllow, nil, aclPipeline)

	aclDir = libovsdbutil.ACLIngress
	ingressMatch := libovsdbutil.GetACLMatch(portGroupName, bnc.getMulticastACLIgrMatch(addrSetNameV4, addrSetNameV6), aclDir)
	dbIDs = getNamespaceMcastACLDbIDs(ns, aclDir, bnc.controllerName)
	aclPipeline = libovsdbutil.ACLDirectionToACLPipeline(aclDir)
	ingressACL := libovsdbutil.BuildACLWithDefaultTier(dbIDs, types.DefaultMcastAllowPriority, ingressMatch, nbdb.ACLActionAllow, nil, aclPipeline)

	acls := []*nbdb.ACL{egressACL, ingressACL}
	ops, err := libovsdbops.CreateOrUpdateACLsOps(bnc.nbClient, nil, bnc.GetSamplingConfig(ns, libovsdbops.MulticastSample), acls...)
	if err != nil {
		return err
	}

	ops, err = libovsdbops.AddACLsToPortGroupOps(bnc.nbClient, ops, portGroupName, acls...)
	if err != nil {
		return err
	}

	_, err = libovsdbops.TransactAndCheck(bnc.nbClient, ops)
	if err != nil {
		return err
	}

	return nil
}

func (bnc *BaseNetworkController) deleteMulticastAllowPolicy(ns string, nsInfo *namespaceInfo) error {
	portGroupName := bnc.getNamespacePortGroupName(ns)

	// mcast acls have ACLMulticastNamespace owner
	// searching by namespace will return ACLs in all existing directions.
	predicateIDs := libovsdbops.NewDbObjectIDs(libovsdbops.ACLMulticastNamespace, bnc.controllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: ns,
		})
	mcastAclPred := libovsdbops.GetPredicate[*nbdb.ACL](predicateIDs, nil)
	mcastACLs, err := libovsdbops.FindACLsWithPredicate(bnc.nbClient, mcastAclPred)
	if err != nil {
		return fmt.Errorf("unable to find default multicast ACLs: %v", err)
	}
	// ACLs referenced by the port group will be deleted by db if there are no other references
	err = libovsdbops.DeleteACLsFromPortGroups(bnc.nbClient, []string{portGroupName},
		mcastACLs...)
	if err != nil {
		return fmt.Errorf("unable to delete multicast acls for namespace %s: %w", ns, err)
	}
	// nsInfo is nil on initial sync, there is no need to cleanup address set in that case
	if nsInfo != nil {
		if err := bnc.addressSetManager.DeleteAddressSet(nsInfo.addrSetOwnerBackref, getMulticastAddrsetBackref(ns)); err != nil {
			return fmt.Errorf("unable to delete address set for namespace %s: %v", ns, err)
		}
		nsInfo.addrSetOwnerBackref = ""
		nsInfo.addrSetNameV4 = ""
		nsInfo.addrSetNameV6 = ""
	}

	return nil
}

// Creates a global default deny multicast policy:
//   - one ACL dropping egress multicast traffic from all pods: this is to
//     protect OVN controller from processing IP multicast reports from nodes
//     that are not allowed to receive multicast traffic.
//   - one ACL dropping ingress multicast traffic to all pods.
//
// Caller must hold the namespace's namespaceInfo object lock.
func (bnc *BaseNetworkController) createDefaultDenyMulticastPolicy() error {
	// By default deny any egress multicast traffic from any pod. This drops
	// IP multicast membership reports therefore denying any multicast traffic
	// to be forwarded to pods.
	// By default deny any ingress multicast traffic to any pod.
	match := getMulticastACLMatch()
	acls := make([]*nbdb.ACL, 0, 2)
	for _, aclDir := range []libovsdbutil.ACLDirection{libovsdbutil.ACLEgress, libovsdbutil.ACLIngress} {
		dbIDs := getDefaultMcastACLDbIDs(mcastDefaultDenyID, aclDir, bnc.controllerName)
		aclPipeline := libovsdbutil.ACLDirectionToACLPipeline(aclDir)
		acl := libovsdbutil.BuildACLWithDefaultTier(dbIDs, types.DefaultMcastDenyPriority, match, nbdb.ACLActionDrop, nil, aclPipeline)
		acls = append(acls, acl)
	}
	ops, err := libovsdbops.CreateOrUpdateACLsOps(bnc.nbClient, nil, bnc.GetSamplingConfig("", libovsdbops.MulticastSample), acls...)
	if err != nil {
		return err
	}

	ops, err = libovsdbops.AddACLsToPortGroupOps(bnc.nbClient, ops, bnc.getClusterPortGroupName(types.ClusterPortGroupNameBase), acls...)
	if err != nil {
		return err
	}

	if !bnc.IsUserDefinedNetwork() {
		// Remove old multicastDefaultDeny port group now that all ports
		// have been added to the clusterPortGroup by WatchPods()
		ops, err = libovsdbops.DeletePortGroupsOps(bnc.nbClient, ops, legacyMulticastDefaultDenyPortGroup)
		if err != nil {
			return err
		}
	}

	_, err = libovsdbops.TransactAndCheck(bnc.nbClient, ops)
	if err != nil {
		return err
	}

	return nil
}

// Creates a global default allow multicast policy:
// - one ACL allowing multicast traffic from cluster router ports
// - one ACL allowing multicast traffic to cluster router ports.
// Caller must hold the namespace's namespaceInfo object lock.
func (bnc *BaseNetworkController) createDefaultAllowMulticastPolicy() error {
	mcastMatch := getMulticastACLMatch()
	acls := make([]*nbdb.ACL, 0, 2)
	rtrPGName := bnc.getClusterPortGroupName(types.ClusterRtrPortGroupNameBase)
	for _, aclDir := range []libovsdbutil.ACLDirection{libovsdbutil.ACLEgress, libovsdbutil.ACLIngress} {
		match := libovsdbutil.GetACLMatch(rtrPGName, mcastMatch, aclDir)
		dbIDs := getDefaultMcastACLDbIDs(mcastAllowInterNodeID, aclDir, bnc.controllerName)
		aclPipeline := libovsdbutil.ACLDirectionToACLPipeline(aclDir)
		acl := libovsdbutil.BuildACLWithDefaultTier(dbIDs, types.DefaultMcastAllowPriority, match, nbdb.ACLActionAllow, nil, aclPipeline)
		acls = append(acls, acl)
	}

	ops, err := libovsdbops.CreateOrUpdateACLsOps(bnc.nbClient, nil, bnc.GetSamplingConfig("", libovsdbops.MulticastSample), acls...)
	if err != nil {
		return err
	}

	ops, err = libovsdbops.AddACLsToPortGroupOps(bnc.nbClient, ops, bnc.getClusterPortGroupName(types.ClusterRtrPortGroupNameBase), acls...)
	if err != nil {
		return err
	}

	_, err = libovsdbops.TransactAndCheck(bnc.nbClient, ops)
	if err != nil {
		return err
	}

	return nil
}

func (bnc *BaseNetworkController) disableMulticast() error {
	// default mcast acls have ACLMulticastCluster type
	predicateIDs := libovsdbops.NewDbObjectIDs(libovsdbops.ACLMulticastCluster, bnc.controllerName, nil)
	mcastAclPred := libovsdbops.GetPredicate[*nbdb.ACL](predicateIDs, nil)
	mcastACLs, err := libovsdbops.FindACLsWithPredicate(bnc.nbClient, mcastAclPred)
	if err != nil {
		return fmt.Errorf("unable to find default multicast ACLs: %v", err)
	}
	err = libovsdbops.DeleteACLsFromPortGroups(bnc.nbClient, []string{
		bnc.getClusterPortGroupName(types.ClusterRtrPortGroupNameBase),
		bnc.getClusterPortGroupName(types.ClusterPortGroupNameBase)},
		mcastACLs...)
	if err != nil {
		return fmt.Errorf("unable to delete default multicast acls: %v", err)
	}
	// run sync for empty namespaces list, this should delete namespaces objects
	err = bnc.syncNsMulticast(map[string]bool{})
	if err != nil {
		return fmt.Errorf("unable to delete namespaced multicast objects: %v", err)
	}
	// disableMulticast is only called on controller creation to clean up stale db entries.
	// there is no need to call cleanups on the addresssetManager, because no calls to EnsureAddressSet
	// were done if multicast is disabled.
	return nil
}

// syncNsMulticast finds and deletes stale multicast db entries for namespaces that don't exist anymore
// or have multicast disabled
func (bnc *BaseNetworkController) syncNsMulticast(k8sNamespaces map[string]bool) error {
	// to find namespaces that have multicast enabled, we need to find namespace-owned port groups with multicast acls.
	predicateIDs := libovsdbops.NewDbObjectIDs(libovsdbops.ACLMulticastNamespace, bnc.controllerName, nil)
	mcastAclPred := libovsdbops.GetPredicate[*nbdb.ACL](predicateIDs, nil)
	mcastACLs, err := libovsdbops.FindACLsWithPredicate(bnc.nbClient, mcastAclPred)
	if err != nil {
		return fmt.Errorf("unable to find multicast ACLs for namespaces: %v", err)
	}
	if len(mcastACLs) == 0 {
		return nil
	}

	mcastAclUUIDs := sets.Set[string]{}
	for _, acl := range mcastACLs {
		mcastAclUUIDs.Insert(acl.UUID)
	}
	staleNamespaces := []string{}

	pgPredIDs := libovsdbops.NewDbObjectIDs(libovsdbops.PortGroupNamespace, bnc.controllerName, nil)
	pgPred := libovsdbops.GetPredicate[*nbdb.PortGroup](pgPredIDs, func(item *nbdb.PortGroup) bool {
		for _, aclUUID := range item.ACLs {
			if mcastAclUUIDs.Has(aclUUID) {
				// multicast is enabled on this port group.
				// add namespace to the stale list if namespace is not present in k8sNamespaces
				namespaceName := item.ExternalIDs[libovsdbops.ObjectNameKey.String()]
				if !k8sNamespaces[namespaceName] {
					staleNamespaces = append(staleNamespaces, namespaceName)
				}
			}
		}
		return false
	})

	_, err = libovsdbops.FindPortGroupsWithPredicate(bnc.nbClient, pgPred)
	if err != nil {
		return fmt.Errorf("unable to find multicast port groups: %v", err)
	}

	for _, staleNs := range staleNamespaces {
		if err = bnc.deleteMulticastAllowPolicy(staleNs, nil); err != nil {
			return fmt.Errorf("unable to delete multicast allow policy for stale ns %s: %v", staleNs, err)
		}
	}
	klog.Infof("Sync multicast removed ACLs for %d stale namespaces", len(staleNamespaces))

	return nil
}

func (bnc *BaseNetworkController) syncDefaultMulticastPolicies() error {
	// If supported, enable IGMP relay on the router to forward multicast
	// traffic between nodes.
	if bnc.multicastSupport {
		// Drop IP multicast globally. Multicast is allowed only if explicitly
		// enabled in a namespace.
		if err := bnc.createDefaultDenyMulticastPolicy(); err != nil {
			klog.Errorf("Failed to create default deny multicast policy, error: %v", err)
			return err
		}

		// Allow IP multicast from node switch to cluster router and from
		// cluster router to node switch.
		if err := bnc.createDefaultAllowMulticastPolicy(); err != nil {
			klog.Errorf("Failed to create default deny multicast policy, error: %v", err)
			return err
		}
	} else {
		if err := bnc.disableMulticast(); err != nil {
			return fmt.Errorf("failed to delete default multicast policy, error: %v", err)
		}
	}
	return nil
}
