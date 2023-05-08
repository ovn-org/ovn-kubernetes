package ovn

import (
	"fmt"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
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

// Creates the match string used for ACLs allowing incoming multicast into a
// namespace, that is, from IPs that are in the namespace's address set.
func (bnc *BaseNetworkController) getMulticastACLIgrMatch(nsInfo *namespaceInfo) string {
	var ipv4Match, ipv6Match string
	addrSetNameV4, addrSetNameV6 := nsInfo.addressSet.GetASHashNames()
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

func getDefaultMcastACLDbIDs(mcastType defaultMcastACLTypeID, aclDir aclDirection, controller string) *libovsdbops.DbObjectIDs {
	// there are 2 types of default multicast ACLs in every direction (Ingress/Egress)
	// DefaultDeny = deny multicast by default
	// AllowInterNode = allow inter-node multicast
	return libovsdbops.NewDbObjectIDs(libovsdbops.ACLMulticastCluster, controller,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.TypeKey:            string(mcastType),
			libovsdbops.PolicyDirectionKey: string(aclDir),
		})

}

func getNamespaceMcastACLDbIDs(ns string, aclDir aclDirection, controller string) *libovsdbops.DbObjectIDs {
	// namespaces ACL
	return libovsdbops.NewDbObjectIDs(libovsdbops.ACLMulticastNamespace, controller,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey:      ns,
			libovsdbops.PolicyDirectionKey: string(aclDir),
		})
}

func getNamespaceMcastPortGroupDbIDs(ns string, controller string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.PortGroupMulticast, controller,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: ns,
		})
}

func (bnc *BaseNetworkController) getMulticastPortGroupName(namespace string) string {
	return libovsdbops.GetPortGroupName(getNamespaceMcastPortGroupDbIDs(namespace, bnc.controllerName))
}

// Creates a policy to allow multicast traffic within 'ns':
//   - a port group containing all logical ports associated with 'ns'
//   - one "from-lport" ACL allowing egress multicast traffic from the pods
//     in 'ns'
//   - one "to-lport" ACL allowing ingress multicast traffic to pods in 'ns'.
//     This matches only traffic originated by pods in 'ns' (based on the
//     namespace address set).
func (bnc *BaseNetworkController) createMulticastAllowPolicy(ns string, nsInfo *namespaceInfo) error {
	pgIDs := getNamespaceMcastPortGroupDbIDs(ns, bnc.controllerName)
	portGroupName := libovsdbops.GetPortGroupName(pgIDs)

	aclDir := aclEgress
	egressMatch := getACLMatch(portGroupName, bnc.getMulticastACLEgrMatch(), aclDir)
	dbIDs := getNamespaceMcastACLDbIDs(ns, aclDir, bnc.controllerName)
	aclPipeline := aclDirectionToACLPipeline(aclDir)
	egressACL := BuildACL(dbIDs, types.DefaultMcastAllowPriority, egressMatch, nbdb.ACLActionAllow, nil, aclPipeline)

	aclDir = aclIngress
	ingressMatch := getACLMatch(portGroupName, bnc.getMulticastACLIgrMatch(nsInfo), aclDir)
	dbIDs = getNamespaceMcastACLDbIDs(ns, aclDir, bnc.controllerName)
	aclPipeline = aclDirectionToACLPipeline(aclDir)
	ingressACL := BuildACL(dbIDs, types.DefaultMcastAllowPriority, ingressMatch, nbdb.ACLActionAllow, nil, aclPipeline)

	acls := []*nbdb.ACL{egressACL, ingressACL}
	ops, err := libovsdbops.CreateOrUpdateACLsOps(bnc.nbClient, nil, acls...)
	if err != nil {
		return err
	}

	// Add all ports from this namespace to the multicast allow group.
	ports := []*nbdb.LogicalSwitchPort{}
	pods, err := bnc.watchFactory.GetPods(ns)
	if err != nil {
		klog.Warningf("Failed to get pods for namespace %q: %v", ns, err)
	}
	for _, pod := range pods {
		if util.PodCompleted(pod) {
			continue
		}
		portInfoMap, err := bnc.logicalPortCache.getAll(pod)
		if err != nil {
			klog.Errorf(err.Error())
		} else {
			for _, portInfo := range portInfoMap {
				ports = append(ports, &nbdb.LogicalSwitchPort{UUID: portInfo.uuid})
			}
		}
	}

	pg := libovsdbops.BuildPortGroup(pgIDs, ports, acls)
	ops, err = libovsdbops.CreateOrUpdatePortGroupsOps(bnc.nbClient, ops, pg)
	if err != nil {
		return err
	}

	_, err = libovsdbops.TransactAndCheck(bnc.nbClient, ops)
	if err != nil {
		return err
	}

	return nil
}

func (bnc *BaseNetworkController) deleteMulticastAllowPolicy(nbClient libovsdbclient.Client, ns string) error {
	portGroupName := bnc.getMulticastPortGroupName(ns)
	// ACLs referenced by the port group wil be deleted by db if there are no other references
	err := libovsdbops.DeletePortGroups(nbClient, portGroupName)
	if err != nil {
		return fmt.Errorf("failed deleting port group %s: %v", portGroupName, err)
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
	for _, aclDir := range []aclDirection{aclEgress, aclIngress} {
		dbIDs := getDefaultMcastACLDbIDs(mcastDefaultDenyID, aclDir, bnc.controllerName)
		aclPipeline := aclDirectionToACLPipeline(aclDir)
		acl := BuildACL(dbIDs, types.DefaultMcastDenyPriority, match, nbdb.ACLActionDrop, nil, aclPipeline)
		acls = append(acls, acl)
	}
	ops, err := libovsdbops.CreateOrUpdateACLsOps(bnc.nbClient, nil, acls...)
	if err != nil {
		return err
	}

	ops, err = libovsdbops.AddACLsToPortGroupOps(bnc.nbClient, ops, bnc.getClusterPortGroupName(types.ClusterPortGroupNameBase), acls...)
	if err != nil {
		return err
	}

	if !bnc.IsSecondary() {
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
	for _, aclDir := range []aclDirection{aclEgress, aclIngress} {
		match := getACLMatch(rtrPGName, mcastMatch, aclDir)
		dbIDs := getDefaultMcastACLDbIDs(mcastAllowInterNodeID, aclDir, bnc.controllerName)
		aclPipeline := aclDirectionToACLPipeline(aclDir)
		acl := BuildACL(dbIDs, types.DefaultMcastAllowPriority, match, nbdb.ACLActionAllow, nil, aclPipeline)
		acls = append(acls, acl)
	}

	ops, err := libovsdbops.CreateOrUpdateACLsOps(bnc.nbClient, nil, acls...)
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
	return nil
}

// podAddAllowMulticastPolicy adds the pod's logical switch port to the namespace's
// multicast port group. Caller must hold the namespace's namespaceInfo object
// lock.
func (bnc *BaseNetworkController) podAddAllowMulticastPolicy(ns string, portInfo *lpInfo) error {
	return libovsdbops.AddPortsToPortGroup(bnc.nbClient, bnc.getMulticastPortGroupName(ns), portInfo.uuid)
}

// podDeleteAllowMulticastPolicy removes the pod's logical switch port from the
// namespace's multicast port group. Caller must hold the namespace's
// namespaceInfo object lock.
func (bnc *BaseNetworkController) podDeleteAllowMulticastPolicy(ns string, portUUID string) error {
	return libovsdbops.DeletePortsFromPortGroup(bnc.nbClient, bnc.getMulticastPortGroupName(ns), portUUID)
}

// syncNsMulticast finds and deletes stale multicast db entries for namespaces that don't exist anymore
func (bnc *BaseNetworkController) syncNsMulticast(k8sNamespaces map[string]bool) error {
	// to find namespaces that have multicast enabled, we need to find corresponding port groups.
	// since we can't filter multicast port groups specifically, find multicast ACLs, and then find
	// port groups they are referenced from.
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

	// pg.ExternalIDs["name"] contains namespace (and pg.Name has hashed namespace)
	pgPred := func(item *nbdb.PortGroup) bool {
		for _, aclUUID := range item.ACLs {
			if mcastAclUUIDs.Has(aclUUID) {
				// add namespace to the stale list
				if !k8sNamespaces[item.ExternalIDs["name"]] {
					staleNamespaces = append(staleNamespaces, item.ExternalIDs["name"])
				}
			}
		}
		return false
	}
	_, err = libovsdbops.FindPortGroupsWithPredicate(bnc.nbClient, pgPred)
	if err != nil {
		return fmt.Errorf("unable to find multicast port groups: %v", err)
	}

	for _, staleNs := range staleNamespaces {
		if err = bnc.deleteMulticastAllowPolicy(bnc.nbClient, staleNs); err != nil {
			return fmt.Errorf("unable to delete multicast allow policy for stale ns %s: %v", staleNs, err)
		}
	}
	klog.Infof("Sync multicast removed ACLs for %d stale namespaces", len(staleNamespaces))

	return nil
}
