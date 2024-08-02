package acl

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	libovsdb "github.com/ovn-org/libovsdb/ovsdb"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/batching"

	v1 "k8s.io/api/core/v1"
	knet "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
)

const (
	defaultDenyPolicyTypeACLExtIdKey = "default-deny-policy-type"
	mcastDefaultDenyID               = "DefaultDeny"
	mcastAllowInterNodeID            = "AllowInterNode"
	l4MatchACLExtIdKey               = "l4Match"
	ipBlockCIDRACLExtIdKey           = "ipblock_cidr"
	namespaceACLExtIdKey             = "namespace"
	policyACLExtIdKey                = "policy"
	policyTypeACLExtIdKey            = "policy_type"
	policyTypeNumACLExtIdKey         = "%s_num"
	emptyIdx                         = -1
	defaultDenyACL                   = "defaultDeny"
	arpAllowACL                      = "arpAllow"
	// staleArpAllowPolicyMatch "was" the old match used when creating default allow ARP ACLs for a namespace
	// NOTE: This is succeed by arpAllowPolicyMatch to allow support for IPV6. This is currently only
	// used when removing stale ACLs from the syncNetworkPolicy function and should NOT be used in any main logic.
	staleArpAllowPolicyMatch  = "arp"
	egressFirewallACLExtIdKey = "egressFirewall"
)

type ACLSyncer struct {
	nbClient       libovsdbclient.Client
	controllerName string
	// txnBatchSize is used to control how many acls will be updated with 1 db transaction.
	txnBatchSize int
}

// controllerName is the name of the new controller that should own all acls without controller
func NewACLSyncer(nbClient libovsdbclient.Client, controllerName string) *ACLSyncer {
	return &ACLSyncer{
		nbClient:       nbClient,
		controllerName: controllerName,
		// create time (which is the upper bound of how much time an update can take) for 20K ACLs
		// (gress ACL were used for testing as the ones that have the biggest number of ExternalIDs)
		// is ~4 sec, which is safe enough to not exceed 10 sec transaction timeout.
		txnBatchSize: 20000,
	}
}

func (syncer *ACLSyncer) SyncACLs(existingNodes []*v1.Node) error {
	// stale acls don't have controller ID
	legacyAclPred := libovsdbops.GetNoOwnerPredicate[*nbdb.ACL]()
	legacyACLs, err := libovsdbops.FindACLsWithPredicate(syncer.nbClient, legacyAclPred)
	if err != nil {
		return fmt.Errorf("unable to find stale ACLs, cannot update stale data: %v", err)
	}

	if len(legacyACLs) > 0 {
		var updatedACLs []*nbdb.ACL
		multicastACLs := syncer.updateStaleMulticastACLsDbIDs(legacyACLs)
		klog.Infof("Found %d stale multicast ACLs", len(multicastACLs))
		updatedACLs = append(updatedACLs, multicastACLs...)

		allowFromNodeACLs := syncer.updateStaleNetpolNodeACLs(legacyACLs, existingNodes)
		klog.Infof("Found %d stale allow from node ACLs", len(allowFromNodeACLs))
		updatedACLs = append(updatedACLs, allowFromNodeACLs...)

		gressPolicyACLs, err := syncer.updateStaleGressPolicies(legacyACLs)
		if err != nil {
			return fmt.Errorf("failed to update gress policy ACLs: %w", err)
		}
		klog.Infof("Found %d stale gress ACLs", len(gressPolicyACLs))
		updatedACLs = append(updatedACLs, gressPolicyACLs...)

		defaultDenyACLs, deleteACLs, err := syncer.updateStaleDefaultDenyNetpolACLs(legacyACLs)
		if err != nil {
			return fmt.Errorf("failed to update stale default deny netpol ACLs: %w", err)
		}
		klog.Infof("Found %d stale default deny netpol ACLs", len(defaultDenyACLs))
		updatedACLs = append(updatedACLs, defaultDenyACLs...)

		egressFirewallACLs := syncer.updateStaleEgressFirewallACLs(legacyACLs)
		klog.Infof("Found %d stale egress firewall ACLs", len(gressPolicyACLs))
		updatedACLs = append(updatedACLs, egressFirewallACLs...)

		// delete stale duplicating acls first
		_, err = libovsdbops.TransactAndCheck(syncer.nbClient, deleteACLs)
		if err != nil {
			return fmt.Errorf("faile to trasact db ops: %v", err)
		}

		// make sure there is only 1 ACL with any given primary ID
		// 1. collect all existing primary IDs via predicate that will update IDs set, but always return false
		existingACLPrimaryIDs := sets.Set[string]{}
		_, err = libovsdbops.FindACLsWithPredicate(syncer.nbClient, func(acl *nbdb.ACL) bool {
			if acl.ExternalIDs[libovsdbops.PrimaryIDKey.String()] != "" {
				existingACLPrimaryIDs.Insert(acl.ExternalIDs[libovsdbops.PrimaryIDKey.String()])
			}
			return false
		})
		if err != nil {
			return fmt.Errorf("failed to find exisitng primary ID acls: %w", err)
		}
		// 2. Check to-be-updated ACLs don't have the same PrimaryID between themselves and with the existingACLPrimaryIDs
		uniquePrimaryIDACLs := []*nbdb.ACL{}
		for _, acl := range updatedACLs {
			primaryID := acl.ExternalIDs[libovsdbops.PrimaryIDKey.String()]
			if existingACLPrimaryIDs.Has(primaryID) {
				// don't update that acl, otherwise 2 ACLs with the same primary ID will be in the db
				klog.Warningf("Skip updating ACL %+v to the new ExternalIDs, since there is another ACL with the same primary ID", acl)
			} else {
				existingACLPrimaryIDs.Insert(primaryID)
				uniquePrimaryIDACLs = append(uniquePrimaryIDACLs, acl)
			}
		}

		// update acls with new ExternalIDs
		err = batching.Batch[*nbdb.ACL](syncer.txnBatchSize, uniquePrimaryIDACLs, func(batchACLs []*nbdb.ACL) error {
			return libovsdbops.CreateOrUpdateACLs(syncer.nbClient, nil, batchACLs...)
		})
		if err != nil {
			return fmt.Errorf("cannot update stale ACLs: %v", err)
		}

		// There may be very old acls that are not selected by any of the syncers, delete them.
		// One example is stale multicast ACLs with the old priority that was accidentally changed by
		// https://github.com/ovn-org/ovn-kubernetes/commit/f68d302664e64093c867c0b9efe08d1d757d6780#diff-cc83e19af1c257d5a09b711d5977d8f8c20beb34b7b5d3eb37b2f2c53ded1bf7L537-R462
		leftoverACLs, err := libovsdbops.FindACLsWithPredicate(syncer.nbClient, legacyAclPred)
		if err != nil {
			return fmt.Errorf("unable to find leftover ACLs, cannot update stale data: %v", err)
		}
		p := func(item *nbdb.LogicalSwitch) bool { return true }
		err = libovsdbops.RemoveACLsFromLogicalSwitchesWithPredicate(syncer.nbClient, p, leftoverACLs...)
		if err != nil {
			return fmt.Errorf("unable delete leftover ACLs from switches: %v", err)
		}
		err = libovsdbops.DeleteACLsFromAllPortGroups(syncer.nbClient, leftoverACLs...)
		if err != nil {
			return fmt.Errorf("unable delete leftover ACLs from port groups: %v", err)
		}
	}

	// Once all the staleACLs are deleted and the externalIDs have been updated (externalIDs update should be a one-time
	// upgrade operation), let us now update the tier's of all existing ACLs to types.DefaultACLTier. During upgrades after
	// the OVN schema changes are applied, the nbdb.ACL.Tier column will be added and every row will be updated to 0 by
	// default (types.PrimaryACLTier). For all features using ACLs (egressFirewall, NetworkPolicy, NodeACLs) we want to
	// move them to Tier2. We need to do this in reverse order of ACL priority to avoid network traffic disruption during
	// upgrades window (if not done according to priorities we might end up in a window where the ACL with priority 1000
	// for default deny is in tier0 while 1001 ACL for allow-ing traffic is in tier2 for a given namespace network policy).
	// NOTE: This is a one-time operation as no ACLs should ever be created in types.PrimaryACLTier moving forward.
	// Fetch all ACLs in types.PrimaryACLTier (Tier0); update their Tier to 2 and batch the ACL update.
	klog.Info("Updating Tier of existing ACLs...")
	start := time.Now()
	aclPred := func(item *nbdb.ACL) bool {
		return item.Tier == types.PrimaryACLTier
	}
	aclsInTier0, err := libovsdbops.FindACLsWithPredicate(syncer.nbClient, aclPred)
	if err != nil {
		return fmt.Errorf("unable to fetch Tier0 ACLs: %v", err)
	}
	if len(aclsInTier0) > 0 {
		sort.Slice(aclsInTier0, func(i, j int) bool {
			return aclsInTier0[i].Priority < aclsInTier0[j].Priority
		}) // O(nlogn); unstable sort
		for _, acl := range aclsInTier0 {
			acl := acl
			acl.Tier = types.DefaultACLTier // move tier to 2
		}
		// batch ACLs together in order of their priority: lowest first and then highest
		err = batching.Batch[*nbdb.ACL](syncer.txnBatchSize, aclsInTier0, func(batchACLs []*nbdb.ACL) error {
			return libovsdbops.CreateOrUpdateACLs(syncer.nbClient, nil, batchACLs...)
		})
		if err != nil {
			return fmt.Errorf("cannot update ACLs to tier2: %v", err)
		}
	}
	klog.Infof("Updating tier's of all ACLs in cluster took %v", time.Since(start))
	return nil
}

func (syncer *ACLSyncer) getDefaultMcastACLDbIDs(mcastType, policyDirection string) *libovsdbops.DbObjectIDs {
	// there are 2 types of default multicast ACLs in every direction (Ingress/Egress)
	// DefaultDeny = deny multicast by default
	// AllowInterNode = allow inter-node multicast
	return libovsdbops.NewDbObjectIDs(libovsdbops.ACLMulticastCluster, syncer.controllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.TypeKey:            mcastType,
			libovsdbops.PolicyDirectionKey: policyDirection,
		})

}

func (syncer *ACLSyncer) getNamespaceMcastACLDbIDs(ns, policyDirection string) *libovsdbops.DbObjectIDs {
	// namespaces ACL
	return libovsdbops.NewDbObjectIDs(libovsdbops.ACLMulticastNamespace, syncer.controllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey:      ns,
			libovsdbops.PolicyDirectionKey: policyDirection,
		})
}

// updateStaleMulticastACLsDbIDs updates multicast ACLs that don't have new ExternalIDs set.
// Must be run before WatchNamespace, since namespaceSync function uses syncNsMulticast, which relies on the new IDs.
func (syncer *ACLSyncer) updateStaleMulticastACLsDbIDs(legacyACLs []*nbdb.ACL) []*nbdb.ACL {
	updatedACLs := []*nbdb.ACL{}
	for _, acl := range legacyACLs {
		var dbIDs *libovsdbops.DbObjectIDs
		if acl.Priority == types.DefaultMcastDenyPriority {
			// there is only 1 type acl type with this priority: default deny
			dbIDs = syncer.getDefaultMcastACLDbIDs(mcastDefaultDenyID, acl.ExternalIDs[defaultDenyPolicyTypeACLExtIdKey])
		} else if acl.Priority == types.DefaultMcastAllowPriority {
			// there are 4 multicast allow types
			if acl.ExternalIDs[defaultDenyPolicyTypeACLExtIdKey] == string(knet.PolicyTypeIngress) {
				// ingress allow acl
				// either default of namespaced
				if strings.Contains(acl.Match, types.ClusterRtrPortGroupNameBase) {
					// default allow ingress
					dbIDs = syncer.getDefaultMcastACLDbIDs(mcastAllowInterNodeID, string(knet.PolicyTypeIngress))
				} else {
					// namespace allow ingress
					// acl Name can be truncated (max length 64), but k8s namespace is limited to 63 symbols,
					// therefore it is safe to extract it from the name
					ns := strings.Split(libovsdbops.GetACLName(acl), "_")[0]
					dbIDs = syncer.getNamespaceMcastACLDbIDs(ns, string(knet.PolicyTypeIngress))
				}
			} else if acl.ExternalIDs[defaultDenyPolicyTypeACLExtIdKey] == string(knet.PolicyTypeEgress) {
				// egress allow acl
				// either default of namespaced
				if strings.Contains(acl.Match, types.ClusterRtrPortGroupNameBase) {
					// default allow egress
					dbIDs = syncer.getDefaultMcastACLDbIDs(mcastAllowInterNodeID, string(knet.PolicyTypeEgress))
				} else {
					// namespace allow egress
					// acl Name can be truncated (max length 64), but k8s namespace is limited to 63 symbols,
					// therefore it is safe to extract it from the name
					ns := strings.Split(libovsdbops.GetACLName(acl), "_")[0]
					dbIDs = syncer.getNamespaceMcastACLDbIDs(ns, string(knet.PolicyTypeEgress))
				}
			} else {
				// unexpected, acl with multicast priority should have ExternalIDs[defaultDenyPolicyTypeACLExtIdKey] set
				klog.Warningf("Found stale ACL with multicast priority %d, but without expected ExternalID[%s]: %+v",
					acl.Priority, defaultDenyPolicyTypeACLExtIdKey, acl)
				continue
			}
		} else {
			//non-multicast acl
			continue
		}
		// update externalIDs
		acl.ExternalIDs = dbIDs.GetExternalIDs()
		updatedACLs = append(updatedACLs, acl)
	}
	return updatedACLs
}

func (syncer *ACLSyncer) getAllowFromNodeACLDbIDs(nodeName, mgmtPortIP string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.ACLNetpolNode, syncer.controllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: nodeName,
			libovsdbops.IpKey:         mgmtPortIP,
		})
}

// updateStaleNetpolNodeACLs updates allow from node ACLs, that don't have new ExternalIDs based
// on DbObjectIDs set. Allow from node acls are applied on the node switch, therefore the cleanup for deleted is not needed,
// since acl will be deleted toegther with the node switch.
func (syncer *ACLSyncer) updateStaleNetpolNodeACLs(legacyACLs []*nbdb.ACL, existingNodes []*v1.Node) []*nbdb.ACL {
	// ACL to allow traffic from host via management port has no name or ExternalIDs
	// The only way to find it is by exact match
	type aclInfo struct {
		nodeName string
		ip       string
	}
	matchToNode := map[string]aclInfo{}
	for _, node := range existingNodes {
		node := *node
		hostSubnets, err := util.ParseNodeHostSubnetAnnotation(&node, types.DefaultNetworkName)
		if err != nil {
			klog.Warningf("Couldn't parse hostSubnet annotation for node %s: %v", node.Name, err)
			continue
		}
		for _, hostSubnet := range hostSubnets {
			mgmtIfAddr := util.GetNodeManagementIfAddr(hostSubnet)
			ipFamily := "ip4"
			if utilnet.IsIPv6(mgmtIfAddr.IP) {
				ipFamily = "ip6"
			}
			match := fmt.Sprintf("%s.src==%s", ipFamily, mgmtIfAddr.IP.String())
			matchToNode[match] = aclInfo{
				nodeName: node.Name,
				ip:       mgmtIfAddr.IP.String(),
			}
		}
	}
	updatedACLs := []*nbdb.ACL{}
	for _, acl := range legacyACLs {
		if _, ok := matchToNode[acl.Match]; ok {
			aclInfo := matchToNode[acl.Match]
			dbIDs := syncer.getAllowFromNodeACLDbIDs(aclInfo.nodeName, aclInfo.ip)
			// Update ExternalIDs and Name based on new dbIndex
			acl.ExternalIDs = dbIDs.GetExternalIDs()
			updatedACLs = append(updatedACLs, acl)
		}
	}
	return updatedACLs
}

func (syncer *ACLSyncer) getNetpolGressACLDbIDs(policyNamespace, policyName, policyType string,
	gressIdx, portPolicyIdx, ipBlockIdx int) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.ACLNetworkPolicyPortIndex, syncer.controllerName,
		map[libovsdbops.ExternalIDKey]string{
			// policy namespace+name
			libovsdbops.ObjectNameKey: policyNamespace + ":" + policyName,
			// egress or ingress
			libovsdbops.PolicyDirectionKey: policyType,
			// gress rule index
			libovsdbops.GressIdxKey: strconv.Itoa(gressIdx),
			// acls are created for every gp.portPolicies:
			// - for empty policy (no selectors and no ip blocks) - empty ACL
			// OR
			// - all selector-based peers ACL
			// - for every IPBlock +1 ACL
			// Therefore unique id for given gressPolicy is portPolicy idx + IPBlock idx
			// (empty policy and all selector-based peers ACLs will have idx=-1)
			libovsdbops.PortPolicyIndexKey: strconv.Itoa(portPolicyIdx),
			libovsdbops.IpBlockIndexKey:    strconv.Itoa(ipBlockIdx),
		})
}

func (syncer *ACLSyncer) updateStaleGressPolicies(legacyACLs []*nbdb.ACL) (updatedACLs []*nbdb.ACL, err error) {
	if len(legacyACLs) == 0 {
		return
	}
	// for every gress policy build mapping to count port policies.
	// l4MatchACLExtIdKey was previously assigned based on port policy,
	// we can just assign idx to every port Policy and make sure there are no equal ACLs.
	gressPolicyPortCount := map[string]map[string]int{}
	for _, acl := range legacyACLs {
		if acl.ExternalIDs[policyTypeACLExtIdKey] == "" {
			// not gress ACL
			continue
		}
		policyNamespace := acl.ExternalIDs[namespaceACLExtIdKey]
		policyName := acl.ExternalIDs[policyACLExtIdKey]
		policyType := acl.ExternalIDs[policyTypeACLExtIdKey]
		idxKey := fmt.Sprintf(policyTypeNumACLExtIdKey, policyType)
		idx, err := strconv.Atoi(acl.ExternalIDs[idxKey])
		if err != nil {
			return nil, fmt.Errorf("unable to parse gress policy idx %s: %v",
				acl.ExternalIDs[idxKey], err)
		}
		var ipBlockIdx int
		// ipBlockCIDRACLExtIdKey is "false" for non-ipBlock ACLs.
		// Then for the first ipBlock in a given gress policy it is "true",
		// and for the rest of them is idx+1
		if acl.ExternalIDs[ipBlockCIDRACLExtIdKey] == "true" {
			ipBlockIdx = 0
		} else if acl.ExternalIDs[ipBlockCIDRACLExtIdKey] == "false" {
			ipBlockIdx = emptyIdx
		} else {
			ipBlockIdx, err = strconv.Atoi(acl.ExternalIDs[ipBlockCIDRACLExtIdKey])
			if err != nil {
				return nil, fmt.Errorf("unable to parse gress policy ipBlockCIDRACLExtIdKey %s: %v",
					acl.ExternalIDs[ipBlockCIDRACLExtIdKey], err)
			}
			ipBlockIdx -= 1
		}
		gressACLID := strings.Join([]string{policyNamespace, policyName, policyType, fmt.Sprintf("%d", idx)}, "_")
		if gressPolicyPortCount[gressACLID] == nil {
			gressPolicyPortCount[gressACLID] = map[string]int{}
		}
		var portIdx int
		l4Match := acl.ExternalIDs[l4MatchACLExtIdKey]
		if l4Match == libovsdbutil.UnspecifiedL4Match {
			portIdx = emptyIdx
		} else {
			if _, ok := gressPolicyPortCount[gressACLID][l4Match]; !ok {
				// this l4MatchACLExtIdKey is new for given gressPolicy, assign the next idx to it
				gressPolicyPortCount[gressACLID][l4Match] = len(gressPolicyPortCount[gressACLID])
			}
			portIdx = gressPolicyPortCount[gressACLID][l4Match]
		}
		dbIDs := syncer.getNetpolGressACLDbIDs(policyNamespace, policyName,
			policyType, idx, portIdx, ipBlockIdx)
		acl.ExternalIDs = dbIDs.GetExternalIDs()
		updatedACLs = append(updatedACLs, acl)
	}
	return
}

func (syncer *ACLSyncer) getDefaultDenyPolicyACLIDs(ns, policyType, defaultACLType string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.ACLNetpolNamespace, syncer.controllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: ns,
			// in the same namespace there can be 2 default deny port groups, egress and ingress,
			// every port group has default deny and arp allow acl.
			libovsdbops.PolicyDirectionKey: policyType,
			libovsdbops.TypeKey:            defaultACLType,
		})
}

func (syncer *ACLSyncer) updateStaleDefaultDenyNetpolACLs(legacyACLs []*nbdb.ACL) (updatedACLs []*nbdb.ACL,
	deleteOps []libovsdb.Operation, err error) {
	for _, acl := range legacyACLs {
		// sync default Deny policies
		// defaultDenyPolicyTypeACLExtIdKey ExternalID was used by default deny and multicast acls,
		// but multicast acls have specific DefaultMcast priority, filter them out.
		if acl.ExternalIDs[defaultDenyPolicyTypeACLExtIdKey] == "" || (acl.Priority != types.DefaultAllowPriority &&
			acl.Priority != types.DefaultDenyPriority) {
			// not default deny policy
			continue
		}

		// remove stale egress and ingress allow arp ACLs that were leftover as a result
		// of ACL migration for "ARPallowPolicy" when the match changed from "arp" to "(arp || nd)"
		if strings.Contains(acl.Match, " && "+staleArpAllowPolicyMatch) {
			pgName := ""
			if strings.Contains(acl.Match, "inport") {
				// egress default ARP allow policy ("inport == @a16323395479447859119_egressDefaultDeny && arp")
				pgName = strings.TrimPrefix(acl.Match, "inport == @")
			} else if strings.Contains(acl.Match, "outport") {
				// ingress default ARP allow policy ("outport == @a16323395479447859119_ingressDefaultDeny && arp")
				pgName = strings.TrimPrefix(acl.Match, "outport == @")
			}
			pgName = strings.TrimSuffix(pgName, " && "+staleArpAllowPolicyMatch)
			deleteOps, err = libovsdbops.DeleteACLsFromPortGroupOps(syncer.nbClient, deleteOps, pgName, acl)
			if err != nil {
				err = fmt.Errorf("failed getting delete acl ops: %w", err)
				return
			}
			// acl will be deleted, no need to update it
			continue
		}

		// acl Name can be truncated (max length 64), but k8s namespace is limited to 63 symbols,
		// therefore it is safe to extract it from the name.
		// works for both older name <namespace>_<policyname> and newer
		// <namespace>_egressDefaultDeny OR <namespace>_ingressDefaultDeny
		ns := strings.Split(libovsdbops.GetACLName(acl), "_")[0]

		// distinguish ARPAllowACL from DefaultDeny
		var defaultDenyACLType string
		if strings.Contains(acl.Match, "(arp || nd)") {
			defaultDenyACLType = arpAllowACL
		} else {
			defaultDenyACLType = defaultDenyACL
		}
		dbIDs := syncer.getDefaultDenyPolicyACLIDs(ns, acl.ExternalIDs[defaultDenyPolicyTypeACLExtIdKey], defaultDenyACLType)
		acl.ExternalIDs = dbIDs.GetExternalIDs()
		updatedACLs = append(updatedACLs, acl)
	}
	return
}

func (syncer *ACLSyncer) getEgressFirewallACLDbIDs(namespace string, ruleIdx int) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.ACLEgressFirewall, syncer.controllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: namespace,
			libovsdbops.RuleIndex:     strconv.Itoa(ruleIdx),
		})
}

func (syncer *ACLSyncer) updateStaleEgressFirewallACLs(legacyACLs []*nbdb.ACL) []*nbdb.ACL {
	updatedACLs := []*nbdb.ACL{}
	for _, acl := range legacyACLs {
		if acl.Priority < types.MinimumReservedEgressFirewallPriority || acl.Priority > types.EgressFirewallStartPriority {
			// not egress firewall acl
			continue
		}
		namespace, ok := acl.ExternalIDs[egressFirewallACLExtIdKey]
		if !ok || namespace == "" {
			klog.Errorf("Failed to sync stale egress firewall acl: expected non-empty %s key in ExternalIDs %+v",
				egressFirewallACLExtIdKey, acl.ExternalIDs)
			continue
		}
		// egress firewall ACL.priority = types.EgressFirewallStartPriority - rule.idx =>
		// rule.idx = types.EgressFirewallStartPriority - ACL.priority
		dbIDs := syncer.getEgressFirewallACLDbIDs(namespace, types.EgressFirewallStartPriority-acl.Priority)
		acl.ExternalIDs = dbIDs.GetExternalIDs()
		updatedACLs = append(updatedACLs, acl)
	}
	return updatedACLs
}
