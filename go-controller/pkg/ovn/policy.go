package ovn

import (
	"fmt"
	"net"
	"reflect"
	"strconv"
	"strings"
	"sync"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	ovsdb "github.com/ovn-org/libovsdb/ovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	kapi "k8s.io/api/core/v1"
	knet "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	kerrorsutil "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
)

const (
	// defaultDenyPolicyTypeACLExtIdKey external ID key for default deny policy type
	defaultDenyPolicyTypeACLExtIdKey = "default-deny-policy-type"
	// l4MatchACLExtIdKey external ID key for L4 Match on 'gress policy ACLs
	l4MatchACLExtIdKey = "l4Match"
	// ipBlockCIDRACLExtIdKey external ID key for IP block CIDR on 'gress policy ACLs
	ipBlockCIDRACLExtIdKey = "ipblock_cidr"
	// namespaceExtIdKey external ID key for namespace on 'gress policy ACLs
	namespaceExtIdKey = "namespace"
	// policyACLExtIdKey external ID key for policy name on 'gress policy ACLs
	policyACLExtIdKey = "policy"
	// policyACLExtKey external ID key for policy type on 'gress policy ACLs
	policyTypeACLExtIdKey = "policy_type"
	// policyTypeNumACLExtIdKey external ID key for policy index by type on 'gress policy ACLs
	policyTypeNumACLExtIdKey = "%s_num"
	// sharePortGroupExtIdKey external ID indicate the port group is for shared port group
	sharePortGroupExtIdKey = "sharedPortGroup"
	// ingressDefaultDenySuffix is the suffix used when creating the ingress port group for a namespace
	ingressDefaultDenySuffix = "ingressDefaultDeny"
	// egressDefaultDenySuffix is the suffix used when creating the ingress port group for a namespace
	egressDefaultDenySuffix = "egressDefaultDeny"
	// arpAllowPolicySuffix is the suffix used when creating default ACLs for a namespace
	arpAllowPolicySuffix = "ARPallowPolicy"
	// arpAllowPolicyMatch is the match used when creating default allow ARP ACLs for a namespace
	arpAllowPolicyMatch = "(arp || nd)"
	// staleArpAllowPolicyMatch "was" the old match used when creating default allow ARP ACLs for a namespace
	// NOTE: This is succeed by arpAllowPolicyMatch to allow support for IPV6. This is currently only
	// used when removing stale ACLs from the syncNetworkPolicies function and should NOT be used in any main logic.
	staleArpAllowPolicyMatch = "arp"
)

type networkPolicy struct {
	// For now networkPolicy has
	// 3 types of global events (those use oc.networkPolicies to get networkPolicy object)
	// 1. Create network policy - create networkPolicy resources,
	// enable local events, and Update namespace loglevel event
	// 2. Update namespace loglevel - update ACLs for defaultDenyPortGroups and portGroup
	// 3. Delete network policy - disable local events, and Update namespace loglevel event,
	// send deletion signal to already running event handlers, delete resources
	//
	// 5 types of local events (those use the same networkPolicy object there were created for):
	// 1. localPod events - update portGroup, defaultDenyPortGroups and localPods
	// 2. peerNamespaceAndPod events - update namespacedPodHandlers
	// 3. peerNamespace events - add/delete gressPolicy address set, update ACLs for portGroup
	// 4. peerPod events - update gressPolicy address set
	// 5. peerSvc events - update gressPolicy address set
	//
	// Delete network policy conflict with all other handlers, therefore we need to make sure it only runs
	// when no other handlers are executing, and that no other handlers will try to work with networkPolicy after
	// Delete network policy was called. This can be done with RWLock, if Delete network policy takes Write lock
	// and sets deleted field to true, and all other handlers take RLock and return immediately if deleted is true.
	// Create network Policy can also take Write lock while it is creating required resources.
	//
	// The only other conflict between handlers here is Update namespace loglevel and peerNamespace, since they both update
	// portGroup ACLs, but this conflict is handled with namespace lock, because both these functions need to lock
	// namespace to create/update ACLs with correct loglevel.
	//
	// We also need to make sure handlers of the same type can be executed in parallel, if this is not true, every
	// event handler can have it own additional lock to sync handlers of the same type.
	//
	// Allowed order of locking is namespace Lock -> oc.networkPolicies key Lock -> networkPolicy.Lock
	// Don't take namespace Lock while holding networkPolicy key lock to avoid deadlock.
	// Don't take RLock from the same goroutine twice, it can lead to deadlock.
	sync.RWMutex

	name            string
	namespace       string
	ingressPolicies []*gressPolicy
	egressPolicies  []*gressPolicy
	isIngress       bool
	isEgress        bool
	// handlers that don't require synchronization, since they are updated sequentially during network policy creation.
	// local and peer pod handlers
	podHandlerList []*factory.Handler
	// peer namespace handlers
	nsHandlerList []*factory.Handler
	// peer namespaced pod handlers, the only type of handler that can be dynamically deleted without deleting the whole
	// networkPolicy. When namespace is deleted, podHandler for that namespace should be deleted too.
	// Can be used by multiple PeerNamespace handlers in parallel for different keys
	// namespace(string): *factory.Handler
	namespacedPodHandlers sync.Map

	// hashed shared port group name of this network policy
	portGroupName string

	// this is a signal for related event handlers that they are/should be stopped.
	// it will be set to true before any networkPolicy infrastructure is deleted,
	// therefore every handler can either do its work and be sure all required resources are there,
	// or this value will be set to true and handler can't proceed.
	// Use networkPolicy.RLock to read this field and hold it for the whole event handling.
	deleted bool
}

func NewNetworkPolicy(policy *knet.NetworkPolicy) *networkPolicy {
	policyTypeIngress, policyTypeEgress := getPolicyType(policy)
	np := &networkPolicy{
		name:                  policy.Name,
		namespace:             policy.Namespace,
		ingressPolicies:       make([]*gressPolicy, 0),
		egressPolicies:        make([]*gressPolicy, 0),
		isIngress:             policyTypeIngress,
		isEgress:              policyTypeEgress,
		podHandlerList:        make([]*factory.Handler, 0),
		nsHandlerList:         make([]*factory.Handler, 0),
		namespacedPodHandlers: sync.Map{},
	}
	return np
}

func (oc *DefaultNetworkController) syncNetworkPolicies(networkPolicies []interface{}) error {
	// expected shared port groups for all existing network policies
	type expectedPortGroupInfo struct {
		selector         metav1.LabelSelector // localPodSelecor
		lspIngressRefCnt int                  // ingress default deny policy reference count for this shared port group
		lspEgressRefCnt  int                  // egress default deny policy reference count for this shared port group
	}

	// existing shared port groups in the ovn-nb DB
	type existingPortGroupInfo struct {
		pgName     string                // readable port group name
		pgHashName string                // port group name
		selector   *metav1.LabelSelector // localPodSelector, derived from external-ids:name
		aclUUIDs   map[string]bool       // map of acls in the port group
	}

	stalePGs := sets.NewString()

	// find all existing shared port groups and legacy default-deny port groups in the ovn-nb DB
	existingSharedPortGroupsInfoMap := make(map[string]map[string]existingPortGroupInfo)
	pPortGroup := func(item *nbdb.PortGroup) bool {
		if _, ok := item.ExternalIDs[sharePortGroupExtIdKey]; ok {
			return true
		}
		pgName := item.Name
		if strings.HasSuffix(pgName, egressDefaultDenySuffix) || strings.HasSuffix(pgName, ingressDefaultDenySuffix) {
			return true
		}
		return false
	}
	portGroups, err := libovsdbops.FindPortGroupsWithPredicate(oc.nbClient, pPortGroup)
	if err != nil {
		return fmt.Errorf("failed to find port groups: %v", err)
	}
	for _, portGroup := range portGroups {
		spgExtId, ok := portGroup.ExternalIDs[sharePortGroupExtIdKey]
		if !ok {
			// must be legacy default-deny port groups
			klog.Warningf("Stale port group %s", portGroup.Name)
			stalePGs.Insert(portGroup.Name)
			continue
		}

		ns, ok := portGroup.ExternalIDs[namespaceExtIdKey]
		if !ok {
			klog.Warningf("Invalid shared port group %s: no %s external-id", portGroup.Name, namespaceExtIdKey)
			stalePGs.Insert(portGroup.Name)
			continue
		}

		prefix := getSharedPortGroupNamePrefix(ns)
		if !strings.HasPrefix(spgExtId, prefix) {
			klog.Warningf("Invalid shared port group %s: external-id:name %s, expect to be prefixed with %s",
				portGroup.Name, spgExtId, prefix)
			stalePGs.Insert(portGroup.Name)
			continue
		}
		selString := strings.TrimPrefix(spgExtId, prefix)
		sel, err := metav1.ParseToLabelSelector(selString)
		if err != nil {
			klog.Warningf("Invalid shared port group %s: invalid external-id:%s %s, failed to parse to selector",
				portGroup.Name, sharePortGroupExtIdKey, spgExtId)
			stalePGs.Insert(portGroup.Name)
			continue
		}
		pgInfo := existingPortGroupInfo{
			pgName:     spgExtId,
			pgHashName: portGroup.Name,
			selector:   sel,
			aclUUIDs:   map[string]bool{},
		}
		for _, uuid := range portGroup.ACLs {
			pgInfo.aclUUIDs[uuid] = true
		}
		if sharedPortGroups, ok := existingSharedPortGroupsInfoMap[ns]; ok {
			sharedPortGroups[portGroup.Name] = pgInfo
		} else {
			existingSharedPortGroupsInfoMap[ns] = map[string]existingPortGroupInfo{portGroup.Name: pgInfo}
		}
	}
	klog.V(5).Infof("Found all existing SharedPortGroups %v", existingSharedPortGroupsInfoMap)

	// based on all existing policy acls, get all possible legacy policy port group name and default deny policy group names.
	// it is ok that these portGroups do not exist, deletion would be no-op.
	pAcl := func(item *nbdb.ACL) bool {
		_, ok1 := item.ExternalIDs[namespaceExtIdKey]
		_, ok2 := item.ExternalIDs[policyACLExtIdKey]
		if ok1 && ok2 {
			return true
		}
		return false
	}
	acls, err := libovsdbops.FindACLsWithPredicate(oc.nbClient, pAcl)
	if err != nil {
		return err
	}
	for _, acl := range acls {
		staleLegacyPortGroupName, _ := getLegacyNetworkPolicyPGName(acl.ExternalIDs[namespaceExtIdKey], acl.ExternalIDs[policyACLExtIdKey])
		stalePGs.Insert(staleLegacyPortGroupName)
		stalePGs.Insert(legacyDefaultDenyPortGroupName(acl.ExternalIDs[namespaceExtIdKey], ingressDefaultDenySuffix))
		stalePGs.Insert(legacyDefaultDenyPortGroupName(acl.ExternalIDs[namespaceExtIdKey], egressDefaultDenySuffix))
	}

	// now delete all possible stale port groups pre shared-port-group support
	klog.V(5).Infof("Delete stale port groups %v", stalePGs)
	stalePGsArray := make([]string, 0, len(stalePGs))
	for stalePG := range stalePGs {
		stalePGsArray = append(stalePGsArray, stalePG)
	}
	err = libovsdbops.DeletePortGroups(oc.nbClient, stalePGsArray...)
	if err != nil {
		return fmt.Errorf("error getting ops of removing stale port group %v: %v", stalePGsArray, err)
	}

	// all ports groups that are created before shared-port-group support are deleted, now focus on
	// stale logical entities created after shared-port-group support.
	//
	// get all expected shared port groups
	expectedPolicies := make(map[string]map[string]bool)
	expectedPgInfosMap := make(map[string][]*expectedPortGroupInfo)
	for _, npInterface := range networkPolicies {
		policy, ok := npInterface.(*knet.NetworkPolicy)
		if !ok {
			return fmt.Errorf("spurious object in syncNetworkPolicies: %v", npInterface)
		}

		np := NewNetworkPolicy(policy)

		if nsMap, ok := expectedPolicies[policy.Namespace]; ok {
			nsMap[policy.Name] = true
		} else {
			expectedPolicies[policy.Namespace] = map[string]bool{
				policy.Name: true,
			}
		}

		found := false
		var pgInfo *expectedPortGroupInfo
		pgInfos, ok := expectedPgInfosMap[np.namespace]
		if ok {
			for _, pgInfo = range pgInfos {
				if reflect.DeepEqual(pgInfo.selector, policy.Spec.PodSelector) {
					found = true
					break
				}
			}
		}
		if !found {
			pgInfo = &expectedPortGroupInfo{selector: policy.Spec.PodSelector}
			if !ok {
				expectedPgInfosMap[np.namespace] = []*expectedPortGroupInfo{pgInfo}
			} else {
				expectedPgInfosMap[np.namespace] = append(expectedPgInfosMap[np.namespace], pgInfo)
			}
		}
		if np.isIngress {
			pgInfo.lspEgressRefCnt++
		}
		if np.isEgress {
			pgInfo.lspIngressRefCnt++
		}
	}
	klog.V(5).Infof("Expect shared port groups %v for all existing network policies %v", expectedPgInfosMap, expectedPolicies)

	err = oc.addressSetFactory.ProcessEachAddressSet(func(_, addrSetName string) error {
		// netpol name has format fmt.Sprintf("%s.%s.%s.%d", gp.policyNamespace, gp.policyName, direction, gp.idx)
		s := strings.Split(addrSetName, ".")
		sLen := len(s)
		// priority should be a number
		_, numErr := strconv.Atoi(s[sLen-1])
		if sLen < 4 || (s[sLen-2] != "ingress" && s[sLen-2] != "egress") || numErr != nil {
			// address set name is not formatted by network policy
			return nil
		}

		// address set is owned by network policy
		// namespace doesn't have dots
		namespaceName := s[0]
		// policyName may have dots, join in that case
		policyName := strings.Join(s[1:sLen-2], ".")
		if !expectedPolicies[namespaceName][policyName] {
			// delete the address sets for this old policy from OVN
			if err := oc.addressSetFactory.DestroyAddressSetInBackingStore(addrSetName); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("error in syncing network policies: %v", err)
	}

	// get all per-policy acls and default deny acls.
	pAcl = func(item *nbdb.ACL) bool {
		_, ok1 := item.ExternalIDs[namespaceExtIdKey]
		_, ok2 := item.ExternalIDs[policyACLExtIdKey]
		if ok1 && ok2 {
			return true
		}
		_, ok := item.ExternalIDs[defaultDenyPolicyTypeACLExtIdKey]
		if ok {
			if strings.Contains(item.Match, ingressDefaultDenySuffix) || strings.Contains(item.Match, egressDefaultDenySuffix) {
				return true
			}
		}
		return false
	}
	acls, err = libovsdbops.FindACLsWithPredicate(oc.nbClient, pAcl)
	if err != nil {
		return err
	}

	// found all stale localPod acls for those policies deleted during ovnkube-master shutdown;
	// also collect all default deny acls in the allDefDenyAclsMap, this will be used when
	// deleting stale default deny acls.
	staleAcls := make([]*nbdb.ACL, 0, len(acls))
	allDefDenyAclsMap := make(map[string]*nbdb.ACL)
	for index := range acls {
		acl := acls[index]
		if _, ok := acl.ExternalIDs[defaultDenyPolicyTypeACLExtIdKey]; ok && acl.Name != nil {
			// default deny acls, note that there could be duplicate acl.Name for ingress/egress,
			// so ingress/egress as part of map key
			allDefDenyAclsMap[*acl.Name+"_"+acl.ExternalIDs[defaultDenyPolicyTypeACLExtIdKey]] = acl
		} else {
			if _, ok := expectedPolicies[acl.ExternalIDs[namespaceExtIdKey]][acl.ExternalIDs[policyACLExtIdKey]]; !ok {
				staleAcls = append(staleAcls, acl)
			}
		}
	}
	klog.V(5).Infof("Found stale per-policy acls %v; and all existing default deny acls %v", staleAcls, allDefDenyAclsMap)

	// delete stale acl from shared port group it belongs
	var ops []ovsdb.Operation
	for _, staleAcl := range staleAcls {
		ns := staleAcl.ExternalIDs[namespaceExtIdKey]
		pgHashName := ""
		portGroupInfos, ok := existingSharedPortGroupsInfoMap[ns]
		if ok {
		out:
			for pgHashName = range portGroupInfos {
				portGroupInfo := portGroupInfos[pgHashName]
				if _, ok := portGroupInfo.aclUUIDs[staleAcl.UUID]; ok {
					break out
				}
			}
		}
		if pgHashName != "" {
			klog.V(5).Infof("Delete stale per-policy acl %v from port group %v", staleAcl, pgHashName)
			if ops, err = libovsdbops.DeleteACLsFromPortGroupOps(oc.nbClient, ops, pgHashName, staleAcl); err != nil {
				return fmt.Errorf("failed to get ops to delete ACLs for stale policy %s/%s: %v", ns,
					staleAcl.ExternalIDs[policyACLExtIdKey], err)
			}
		}
	}

	// if the egress/ingress default deny reference count drops to 0, delete the default deny acls if they exist.
	for ns, pgInfos := range existingSharedPortGroupsInfoMap {
		for pgHashName, pgInfo := range pgInfos {
			pgNeeded := false
			staleDefDenyAclKeys := []string{}
			// if the no acls left, delete the port group as well
			if expectedPgInfos, ok := expectedPgInfosMap[ns]; ok {
				for _, expectedPgInfo := range expectedPgInfos {
					if reflect.DeepEqual(pgInfo.selector, expectedPgInfo.selector) {
						pgNeeded = true
						// it is possible egress/ingress default deny reference count has changed, we'd need to delete
						// default deny acls if they are no longer needed.
						if expectedPgInfo.lspEgressRefCnt == 0 {
							klog.V(5).Infof("Egress default deny reference count drops to 0 for port group %s", pgInfo.pgName)
							aclName := getDefaultDenyPolicyACLName(pgHashName, lportEgressAfterLB)
							staleDefDenyAclKeys = append(staleDefDenyAclKeys, aclName+"_"+string(knet.PolicyTypeEgress))
							aclName = getARPAllowACLName(pgHashName)
							staleDefDenyAclKeys = append(staleDefDenyAclKeys, aclName+"_"+string(knet.PolicyTypeEgress))
						}

						if expectedPgInfo.lspIngressRefCnt == 0 {
							klog.V(5).Infof("Ingress default deny reference count drops to 0 for port group %s", pgInfo.pgName)
							aclName := getDefaultDenyPolicyACLName(pgHashName, lportIngress)
							staleDefDenyAclKeys = append(staleDefDenyAclKeys, aclName+"_"+string(knet.PolicyTypeIngress))
							aclName = getARPAllowACLName(pgHashName)
							staleDefDenyAclKeys = append(staleDefDenyAclKeys, aclName+"_"+string(knet.PolicyTypeIngress))
						}
					}
				}
			}

			for _, staleDefDenyAclKey := range staleDefDenyAclKeys {
				if staleDefDenyAcl, ok := allDefDenyAclsMap[staleDefDenyAclKey]; ok {
					klog.V(5).Infof("Delete stale default deny acl %s from port group %s", staleDefDenyAclKey, pgInfo.pgName)
					if ops, err = libovsdbops.DeleteACLsFromPortGroupOps(oc.nbClient, ops, pgInfo.pgHashName, staleDefDenyAcl); err != nil {
						return fmt.Errorf("failed to get ops to delete default deny ACLs from shared port group %s: %v", pgInfo.pgName, err)
					}
				}
			}

			if !pgNeeded {
				klog.Infof("Delete stale shared port group %s", pgInfo.pgName)
				ops, err = libovsdbops.DeletePortGroupsOps(oc.nbClient, ops, pgInfo.pgHashName)
				if err != nil {
					return fmt.Errorf("error getting ops of removing stale port groups %v: %v", pgInfo.pgHashName, err)
				}
			}
		}
	}

	// Update existing egress network policies to use the updated ACLs
	// Note that the default multicast egress acls were created with the correct direction, but
	// we'd still need to update its apply-after-lb=true option, so that the ACL priorities can apply properly;
	// If acl's option["apply-after-lb"] is already set to true, then its direction should be also correct.
	p := func(item *nbdb.ACL) bool {
		return (item.ExternalIDs[policyTypeACLExtIdKey] == string(knet.PolicyTypeEgress) ||
			item.ExternalIDs[defaultDenyPolicyTypeACLExtIdKey] == string(knet.PolicyTypeEgress)) &&
			item.Options["apply-after-lb"] != "true"
	}
	egressACLs, err := libovsdbops.FindACLsWithPredicate(oc.nbClient, p)
	if err != nil {
		return fmt.Errorf("cannot find NetworkPolicy Egress ACLs: %v", err)
	}

	if len(egressACLs) > 0 {
		for _, acl := range egressACLs {
			acl.Direction = nbdb.ACLDirectionFromLport
			if acl.Options == nil {
				acl.Options = map[string]string{"apply-after-lb": "true"}
			} else {
				acl.Options["apply-after-lb"] = "true"
			}
		}
		ops, err := libovsdbops.CreateOrUpdateACLsOps(oc.nbClient, ops, egressACLs...)
		if err != nil {
			return fmt.Errorf("cannot create ops to update old Egress NetworkPolicy ACLs: %v", err)
		}
		_, err = libovsdbops.TransactAndCheck(oc.nbClient, ops)
		if err != nil {
			return fmt.Errorf("cannot update old Egress NetworkPolicy ACLs: %v", err)
		}
	}

	return nil
}

func getAllowFromNodeACLName() string {
	return ""
}

func addAllowACLFromNode(nodeName string, mgmtPortIP net.IP, nbClient libovsdbclient.Client) error {
	ipFamily := "ip4"
	if utilnet.IsIPv6(mgmtPortIP) {
		ipFamily = "ip6"
	}
	match := fmt.Sprintf("%s.src==%s", ipFamily, mgmtPortIP.String())

	nodeACL := BuildACL(getAllowFromNodeACLName(), types.DefaultAllowPriority, match,
		nbdb.ACLActionAllowRelated, nil, lportIngress, nil)

	ops, err := libovsdbops.CreateOrUpdateACLsOps(nbClient, nil, nodeACL)
	if err != nil {
		return fmt.Errorf("failed to create or update ACL %v: %v", nodeACL, err)
	}

	ops, err = libovsdbops.AddACLsToLogicalSwitchOps(nbClient, ops, nodeName, nodeACL)
	if err != nil {
		return fmt.Errorf("failed to add ACL %v to switch %s: %v", nodeACL, nodeName, err)
	}

	_, err = libovsdbops.TransactAndCheck(nbClient, ops)
	if err != nil {
		return err
	}

	return nil
}

func getDefaultDenyPolicyACLName(pgHashName string, aclT aclType) string {
	var defaultDenySuffix string
	switch aclT {
	case lportIngress:
		defaultDenySuffix = ingressDefaultDenySuffix
	case lportEgressAfterLB:
		defaultDenySuffix = egressDefaultDenySuffix
	default:
		panic(fmt.Sprintf("Unknown acl type %s", aclT))
	}
	return joinACLName(pgHashName, defaultDenySuffix)
}

func getDefaultDenyPolicyExternalIDs(aclT aclType, sharedPortGroupName string) map[string]string {
	extIds := map[string]string{defaultDenyPolicyTypeACLExtIdKey: string(aclTypeToPolicyType(aclT))}
	if sharedPortGroupName != "" {
		extIds[sharePortGroupExtIdKey] = sharedPortGroupName
	}
	return extIds
}

func getARPAllowACLName(ns string) string {
	return joinACLName(ns, arpAllowPolicySuffix)
}

func legacyDefaultDenyPortGroupName(namespace, gressSuffix string) string {
	return hashedPortGroup(namespace) + "_" + gressSuffix
}

func buildDenyACLs(pgHashName string, aclLogging *ACLLoggingLevels, aclT aclType) (denyACL, allowACL *nbdb.ACL) {
	denyMatch := getACLMatch(pgHashName, "", aclT)
	allowMatch := getACLMatch(pgHashName, arpAllowPolicyMatch, aclT)
	denyACL = BuildACL(getDefaultDenyPolicyACLName(pgHashName, aclT), types.DefaultDenyPriority, denyMatch,
		nbdb.ACLActionDrop, aclLogging, aclT, getDefaultDenyPolicyExternalIDs(aclT, pgHashName))
	allowACL = BuildACL(getARPAllowACLName(pgHashName), types.DefaultAllowPriority, allowMatch,
		nbdb.ACLActionAllow, aclLogging, aclT, getDefaultDenyPolicyExternalIDs(aclT, pgHashName))
	return
}

// must be called with namespace lock
func (oc *DefaultNetworkController) updateACLLoggingForPolicy(np *networkPolicy, aclLogging *ACLLoggingLevels, updateAllow, updateDeny bool) error {
	np.RLock()
	defer np.RUnlock()
	if np.deleted || np.portGroupName == "" {
		return nil
	}

	// Predicate for given network policy ACLs its shared port group ACLs
	p := func(item *nbdb.ACL) bool {
		allowAclCond := item.ExternalIDs[namespaceExtIdKey] == np.namespace && item.ExternalIDs[policyACLExtIdKey] == np.name
		denyAclCond := item.ExternalIDs[sharePortGroupExtIdKey] == np.portGroupName && item.ExternalIDs[defaultDenyPolicyTypeACLExtIdKey] != ""

		if updateAllow && allowAclCond {
			return true
		}
		if updateDeny && denyAclCond {
			return true
		}
		return false
	}
	return UpdateACLLoggingWithPredicate(oc.nbClient, p, aclLogging)
}

// handleNetPolNamespaceUpdate should update all network policies related to given namespace.
// Must be called with namespace Lock, should be retriable
func (oc *DefaultNetworkController) handleNetPolNamespaceUpdate(namespace string, nsInfo *namespaceInfo) error {
	// now update network policy specific ACLs
	klog.V(5).Infof("Setting network policy ACLs for ns: %s", namespace)
	for npKey := range nsInfo.relatedNetworkPolicies {
		err := oc.networkPolicies.DoWithLock(npKey, func(key string) error {
			np, found := oc.networkPolicies.Load(npKey)
			if !found {
				klog.Errorf("Netpol was deleted from cache, but not from namespace related objects")
				return nil
			}
			return oc.updateACLLoggingForPolicy(np, &nsInfo.aclLogging, true, true)
		})
		if err != nil {
			return fmt.Errorf("unable to update ACL for network policy %s: %v", npKey, err)
		}
		klog.Infof("ACL for network policy: %s, updated to new log level: %s", npKey, nsInfo.aclLogging.Allow)
	}
	return nil
}

// getPolicyType returns whether the policy is of type ingress and/or egress
func getPolicyType(policy *knet.NetworkPolicy) (bool, bool) {
	var policyTypeIngress bool
	var policyTypeEgress bool

	for _, policyType := range policy.Spec.PolicyTypes {
		if policyType == knet.PolicyTypeIngress {
			policyTypeIngress = true
		} else if policyType == knet.PolicyTypeEgress {
			policyTypeEgress = true
		}
	}

	return policyTypeIngress, policyTypeEgress
}

// getNewLocalPolicyPorts will find and return port info for every given pod obj, that is not found in
// np.localPods.
// if there are problems with fetching port info from logicalPortCache, pod will be added to errObjs.
func (oc *DefaultNetworkController) getNewLocalPolicyPorts(spgInfo *sharedPortGroupInfo,
	objs ...interface{}) (policyPortsToUUIDs map[string]string, policyPortUUIDs []string, errObjs []interface{}) {

	klog.Infof("Processing shared port group %s to have %d local pods...", spgInfo.pgName, len(objs))
	policyPortUUIDs = make([]string, 0, len(objs))
	policyPortsToUUIDs = map[string]string{}

	for _, obj := range objs {
		pod := obj.(*kapi.Pod)

		logicalPortName := util.GetLogicalPortName(pod.Namespace, pod.Name)
		if _, ok := spgInfo.localPods.Load(logicalPortName); ok {
			// port is already added for this policy
			continue
		}

		if pod.Spec.NodeName == "" {
			// pod is not yet scheduled, will receive update event for it
			continue
		}

		// Skip pods that will never be present in logicalPortCache,
		// e.g. hostNetwork pods, overlay node pods, or completed pods
		if !oc.podExpectedInLogicalCache(pod) {
			continue
		}

		// Add pod to errObjs for retry if
		// 1. getting pod LSP from the cache fails,
		// 2. the gotten LSP is scheduled for removal (stateful-sets).
		portInfo, err := oc.logicalPortCache.get(logicalPortName)
		if err != nil {
			klog.Warningf("Failed to get LSP for pod %s/%s for shared port group %s, err: %v",
				pod.Namespace, pod.Name, spgInfo.pgName, err)
			errObjs = append(errObjs, pod)
			continue
		}

		// Add pod to errObjs if LSP is scheduled for deletion
		if !portInfo.expires.IsZero() {
			klog.Warningf("Stale LSP %s for shared port group %s found in cache",
				portInfo.name, spgInfo.pgName)
			errObjs = append(errObjs, pod)
			continue
		}

		// LSP get succeeded and LSP is up to fresh
		klog.V(5).Infof("Fresh LSP %s for shared port group %s found in cache",
			portInfo.name, spgInfo.pgName)

		policyPortUUIDs = append(policyPortUUIDs, portInfo.uuid)
		policyPortsToUUIDs[portInfo.name] = portInfo.uuid
	}
	return
}

// getExistingLocalPolicyPorts will find and return port info for every given pod obj, that is present in np.localPods.
// if there are problems with fetching port info from logicalPortCache, pod will be added to errObjs.
func (oc *DefaultNetworkController) getExistingLocalPolicyPorts(spgInfo *sharedPortGroupInfo,
	objs ...interface{}) (policyPortsToUUIDs map[string]string, policyPortUUIDs []string, errObjs []interface{}) {
	klog.Infof("Processing shared port group %s to delete %d local pods...", spgInfo.pgName, len(objs))

	policyPortUUIDs = make([]string, 0, len(objs))
	policyPortsToUUIDs = map[string]string{}
	for _, obj := range objs {
		pod := obj.(*kapi.Pod)

		logicalPortName := util.GetLogicalPortName(pod.Namespace, pod.Name)
		loadedPortUUID, ok := spgInfo.localPods.Load(logicalPortName)
		if !ok {
			// port is already deleted for this policy
			continue
		}
		portUUID := loadedPortUUID.(string)

		policyPortsToUUIDs[logicalPortName] = portUUID
		policyPortUUIDs = append(policyPortUUIDs, portUUID)
	}
	return
}

func (oc *DefaultNetworkController) buildLocalNetworkPolicyAcls(spgInfo *sharedPortGroupInfo, np *networkPolicy, aclLogging *ACLLoggingLevels) []*nbdb.ACL {
	acls := []*nbdb.ACL{}
	if np.isIngress {
		if spgInfo.lspIngressRefCnt == 0 {
			ingressDenyACL, ingressAllowACL := buildDenyACLs(spgInfo.pgHashName, aclLogging, lportIngress)
			acls = append(acls, ingressDenyACL)
			acls = append(acls, ingressAllowACL)
		}
	}
	if np.isEgress {
		if spgInfo.lspEgressRefCnt == 0 {
			egressDenyACL, egressAllowACL := buildDenyACLs(spgInfo.pgHashName, aclLogging, lportEgressAfterLB)
			acls = append(acls, egressDenyACL)
			acls = append(acls, egressAllowACL)
		}
	}

	policyAcls := oc.buildNetworkPolicyACLs(spgInfo.pgHashName, np, aclLogging)
	klog.Infof("Build local policy acls %+v for network policy %s/%s sharing portGroup %s",
		policyAcls, np.namespace, np.name, spgInfo.pgName)
	acls = append(acls, policyAcls...)
	return acls
}

func (oc *DefaultNetworkController) handleInitialSelectedLocalPods(spgInfo *sharedPortGroupInfo, np *networkPolicy, aclLogging *ACLLoggingLevels, objs ...interface{}) error {
	acls := oc.buildLocalNetworkPolicyAcls(spgInfo, np, aclLogging)
	ops, err := libovsdbops.CreateOrUpdateACLsOps(oc.nbClient, nil, acls...)
	if err != nil {
		return fmt.Errorf("failed to create acls ops for network policy %s/%s: %v", np.namespace, np.name, err)
	}

	sharedPG := libovsdbops.BuildPortGroup(spgInfo.pgHashName, spgInfo.pgName, nil, acls)
	sharedPG.ExternalIDs[sharePortGroupExtIdKey] = spgInfo.pgName
	sharedPG.ExternalIDs[namespaceExtIdKey] = np.namespace

	ops, err = libovsdbops.CreateOrUpdatePortGroupsOps(oc.nbClient, ops, sharedPG)
	if err != nil {
		return err
	}

	recordOps, txOkCallBack, _, err := metrics.GetConfigDurationRecorder().AddOVN(oc.nbClient, "networkpolicy",
		np.namespace, np.name)
	if err != nil {
		klog.Warningf("Failed to record config duration: %v", err)
	}
	ops = append(ops, recordOps...)

	err = oc.handleLocalPodSelectorAddFunc(spgInfo, ops, txOkCallBack, objs...)
	if err != nil {
		// ignore returned handleLocalPodSelectorAddFunc error, since any pod that wasn't properly handled will be retried individually.
		klog.Warningf(err.Error())

		// run ovsdb txn prior to handleLocalPodSelectorAddFunc
		_, err = libovsdbops.TransactAndCheck(oc.nbClient, ops)
		if err != nil {
			return fmt.Errorf("failed to run ovsdb txn to create shared port group %s: %v", spgInfo.pgName, err)
		}
		txOkCallBack()
	}
	return nil
}

// handleLocalPodSelectorAddFunc adds a new pod to an existing NetworkPolicy, should be retriable.
func (oc *DefaultNetworkController) handleLocalPodSelectorAddFunc(spgInfo *sharedPortGroupInfo, ops []ovsdb.Operation, txOkCallBack func(), objs ...interface{}) error {
	// get info for new pods that are not listed in np.localPods
	portNamesToUUIDs, policyPortUUIDs, errPods := oc.getNewLocalPolicyPorts(spgInfo, objs...)
	// for multiple objects, try to update the ones that were fetched successfully
	// return error for errPods in the end
	if len(portNamesToUUIDs) > 0 {
		var err error
		// add pods to policy port group
		if !PortGroupHasPorts(oc.nbClient, spgInfo.pgHashName, policyPortUUIDs) {
			ops, err = libovsdbops.AddPortsToPortGroupOps(oc.nbClient, ops, spgInfo.pgHashName, policyPortUUIDs...)
			if err != nil {
				return fmt.Errorf("unable to get ops to add new pod to shared port group %s: %v", spgInfo.pgName, err)
			}
		}
	}
	_, err := libovsdbops.TransactAndCheck(oc.nbClient, ops)
	if err != nil {
		return fmt.Errorf("failed to run ovsdb txn to add ports to shared port group %s: %v", spgInfo.pgName, err)
	}
	if txOkCallBack != nil {
		txOkCallBack()
	}

	// all operations were successful, update spgInfo.localPods
	for portName, portUUID := range portNamesToUUIDs {
		spgInfo.localPods.Store(portName, portUUID)
	}

	if len(errPods) > 0 {
		var errs []error
		for _, errPod := range errPods {
			pod := errPod.(*kapi.Pod)
			errs = append(errs, fmt.Errorf("unable to get port info for pod %s/%s", pod.Namespace, pod.Name))
		}
		return kerrorsutil.NewAggregate(errs)
	}
	return nil
}

// handleLocalPodSelectorDelFunc handles delete event for local pod, should be retriable
func (oc *DefaultNetworkController) handleLocalPodSelectorDelFunc(spgInfo *sharedPortGroupInfo, objs ...interface{}) error {
	portNamesToUUIDs, policyPortUUIDs, errPods := oc.getExistingLocalPolicyPorts(spgInfo, objs...)

	if len(portNamesToUUIDs) > 0 {
		// del pods from policy port group
		err := libovsdbops.DeletePortsFromPortGroup(oc.nbClient, spgInfo.pgHashName, policyPortUUIDs...)
		if err != nil {
			return fmt.Errorf("unable to get ops to add new pod to policy port group: %v", err)
		}

		// all operations were successful, update np.localPods
		for portName := range portNamesToUUIDs {
			spgInfo.localPods.Delete(portName)
		}
	}

	if len(errPods) > 0 {
		pod := errPods[0].(*kapi.Pod)
		return fmt.Errorf("unable to get port info for pod %s/%s", pod.Namespace, pod.Name)
	}
	return nil
}

// we only need to create an address set if there is a podSelector or namespaceSelector
func hasAnyLabelSelector(peers []knet.NetworkPolicyPeer) bool {
	for _, peer := range peers {
		if peer.PodSelector != nil || peer.NamespaceSelector != nil {
			return true
		}
	}
	return false
}

func getLegacyNetworkPolicyPGName(namespace, name string) (pgName, readablePGName string) {
	readableGroupName := fmt.Sprintf("%s_%s", namespace, name)
	return hashedPortGroup(readableGroupName), readableGroupName
}

type policyHandler struct {
	gress             *gressPolicy
	namespaceSelector *metav1.LabelSelector
	podSelector       *metav1.LabelSelector
}

// createNetworkPolicy creates a network policy, should be retriable.
// If network policy with given key exists, it will try to clean it up first, and return an error if it fails.
// No need to log network policy key here, because caller of createNetworkPolicy should prepend error message with
// that information.
func (oc *DefaultNetworkController) createNetworkPolicy(policy *knet.NetworkPolicy, aclLogging *ACLLoggingLevels) (*networkPolicy, error) {
	// To avoid existing connections disruption, make sure to apply allow ACLs before applying deny ACLs.
	// This requires to start peer handlers before local pod handlers.
	// 1. Cleanup old policy if it failed to be created
	// 2. Build gress policies, create addressSets for peers
	// 3. Add policy to default deny port group.
	// 4. Build policy ACLs and port group. All the local pods that this policy
	// selects will be eventually added to this port group.
	// Pods are not added to default deny port groups yet, this is just a preparation step
	// 5. Unlock networkPolicy before starting pod handlers to avoid deadlock
	// since pod handlers take np.RLock
	// 6. Start peer handlers to update all allow rules first
	// 7. Start local pod handlers, that will update networkPolicy and default deny port groups with selected pods.

	npKey := getPolicyKey(policy)
	var np *networkPolicy
	var policyHandlers []policyHandler

	err := oc.networkPolicies.DoWithLock(npKey, func(npKey string) error {
		oldNP, found := oc.networkPolicies.Load(npKey)
		if found {
			// 1. Cleanup old policy if it failed to be created
			if cleanupErr := oc.cleanupNetworkPolicy(oldNP); cleanupErr != nil {
				return fmt.Errorf("cleanup for retrying network policy create failed: %v", cleanupErr)
			}
		}
		np, found = oc.networkPolicies.LoadOrStore(npKey, NewNetworkPolicy(policy))
		if found {
			// that should never happen, because successful cleanup will delete np from oc.networkPolicies
			return fmt.Errorf("network policy is found in the system, "+
				"while it should've been cleaned up, obj: %+v", np)
		}
		np.Lock()
		npLocked := true
		// we unlock np in the middle of this function, use npLocked to track if it was already unlocked explicitly
		defer func() {
			if npLocked {
				np.Unlock()
			}
		}()
		// no need to check np.deleted, since the object has just been created
		// now we have a new np stored in oc.networkPolicies
		var err error

		if aclLogging.Deny != "" || aclLogging.Allow != "" {
			klog.Infof("ACL logging for network policy %s in namespace %s set to deny=%s, allow=%s",
				policy.Name, policy.Namespace, aclLogging.Deny, aclLogging.Allow)
		}

		// 2. Build gress policies, create addressSets for peers

		// Consider both ingress and egress rules of the policy regardless of this
		// policy type. A pod is isolated as long as as it is selected by any
		// namespace policy. Since we don't process all namespace policies on a
		// given policy update that might change the isolation status of a selected
		// pod, we have create the allow ACLs derived from the policy rules in case
		// the selected pods become isolated in the future even if that is not their
		// current status.

		// Go through each ingress rule.  For each ingress rule, create an
		// addressSet for the peer pods.
		for i, ingressJSON := range policy.Spec.Ingress {
			klog.V(5).Infof("Network policy ingress is %+v", ingressJSON)

			ingress := newGressPolicy(knet.PolicyTypeIngress, i, policy.Namespace, policy.Name)
			// append ingress policy to be able to cleanup created address set
			// see cleanupNetworkPolicy for details
			np.ingressPolicies = append(np.ingressPolicies, ingress)

			// Each ingress rule can have multiple ports to which we allow traffic.
			for _, portJSON := range ingressJSON.Ports {
				ingress.addPortPolicy(&portJSON)
			}
			if hasAnyLabelSelector(ingressJSON.From) {
				klog.V(5).Infof("Network policy %s with ingress rule %s has a selector", npKey, ingress.policyName)
				if err = ingress.ensurePeerAddressSet(oc.addressSetFactory); err != nil {
					return err
				}
			}
			for _, fromJSON := range ingressJSON.From {
				// Add IPBlock to ingress network policy
				if fromJSON.IPBlock != nil {
					ingress.addIPBlock(fromJSON.IPBlock)
				}

				policyHandlers = append(policyHandlers, policyHandler{
					gress:             ingress,
					namespaceSelector: fromJSON.NamespaceSelector,
					podSelector:       fromJSON.PodSelector,
				})
			}
		}

		// Go through each egress rule.  For each egress rule, create an
		// addressSet for the peer pods.
		for i, egressJSON := range policy.Spec.Egress {
			klog.V(5).Infof("Network policy egress is %+v", egressJSON)

			egress := newGressPolicy(knet.PolicyTypeEgress, i, policy.Namespace, policy.Name)
			// append ingress policy to be able to cleanup created address set
			// see cleanupNetworkPolicy for details
			np.egressPolicies = append(np.egressPolicies, egress)

			// Each egress rule can have multiple ports to which we allow traffic.
			for _, portJSON := range egressJSON.Ports {
				egress.addPortPolicy(&portJSON)
			}

			if hasAnyLabelSelector(egressJSON.To) {
				klog.V(5).Infof("Network policy %s with egress rule %s has a selector", npKey, egress.policyName)
				if err = egress.ensurePeerAddressSet(oc.addressSetFactory); err != nil {
					return err
				}
			}
			for _, toJSON := range egressJSON.To {
				// Add IPBlock to egress network policy
				if toJSON.IPBlock != nil {
					egress.addIPBlock(toJSON.IPBlock)
				}

				policyHandlers = append(policyHandlers, policyHandler{
					gress:             egress,
					namespaceSelector: toJSON.NamespaceSelector,
					podSelector:       toJSON.PodSelector,
				})
			}
		}

		np.Unlock()
		npLocked = false

		// 3. Build local pod policies, create shared port group if needed
		err = oc.createNetworkLocalPolicy(np, policy, aclLogging)
		if err != nil {
			return fmt.Errorf("failed to create network local policies: %v", err)
		}

		// 4. Start peer handlers to update all allow rules first
		for _, handler := range policyHandlers {
			if handler.namespaceSelector != nil && handler.podSelector != nil {
				// For each rule that contains both peer namespace selector and
				// peer pod selector, we create a watcher for each matching namespace
				// that populates the addressSet
				nsSel, _ := metav1.LabelSelectorAsSelector(handler.namespaceSelector)
				if nsSel.Empty() {
					// namespace is not limited by a selector, just use pod selector with empty namespace
					err = oc.addPeerPodHandler(handler.podSelector, handler.gress, np, "")
				} else {
					err = oc.addPeerNamespaceAndPodHandler(handler.namespaceSelector, handler.podSelector, handler.gress, np)
				}
			} else if handler.namespaceSelector != nil {
				// For each peer namespace selector, we create a watcher that
				// populates ingress.peerAddressSets
				err = oc.addPeerNamespaceHandler(handler.namespaceSelector, handler.gress, np)
			} else if handler.podSelector != nil {
				// For each peer pod selector, we create a watcher that
				// populates the addressSet
				err = oc.addPeerPodHandler(handler.podSelector, handler.gress, np, np.namespace)
			}
			if err != nil {
				return fmt.Errorf("failed to start peer handler: %v", err)
			}
		}

		return nil
	})
	return np, err
}

// addNetworkPolicy creates and applies OVN ACLs to pod logical switch
// ports from Kubernetes NetworkPolicy objects using OVN Port Groups
// if addNetworkPolicy fails, create or delete operation can be retried
func (oc *DefaultNetworkController) addNetworkPolicy(policy *knet.NetworkPolicy) error {
	klog.Infof("Adding network policy %s", getPolicyKey(policy))

	// To not hold nsLock for the whole process on network policy creation, we do the following:
	// 1. save required namespace information to use for netpol create
	// 2. create network policy without ns Lock
	// 3. lock namespace
	// 4. check if namespace information related to network policy has changed, run the same function as on namespace update
	// 5. subscribe to namespace update events
	// 6. unlock namespace

	// 1. save required namespace information to use for netpol create,
	npKey := getPolicyKey(policy)
	nsInfo, nsUnlock := oc.getNamespaceLocked(policy.Namespace, true)
	if nsInfo == nil {
		return fmt.Errorf("unable to get namespace for network policy %s: namespace doesn't exist", npKey)
	}
	aclLogging := nsInfo.aclLogging
	nsUnlock()

	// 2. create network policy without ns Lock, cleanup on failure
	var np *networkPolicy
	var err error

	np, err = oc.createNetworkPolicy(policy, &aclLogging)
	defer func() {
		if err != nil {
			klog.Infof("Create network policy %s failed, try to cleanup", npKey)
			// try to cleanup network policy straight away
			// it will be retried later with add/delete network policy handlers if it fails
			cleanupErr := oc.networkPolicies.DoWithLock(npKey, func(npKey string) error {
				np, ok := oc.networkPolicies.Load(npKey)
				if !ok {
					klog.Infof("Deleting policy %s that is already deleted", npKey)
					return nil
				}
				return oc.cleanupNetworkPolicy(np)
			})
			if cleanupErr != nil {
				klog.Infof("Cleanup for failed create network policy %s returned an error: %v",
					npKey, cleanupErr)
			}
		}
	}()
	if err != nil {
		return fmt.Errorf("failed to create Network Policy %s: %v", npKey, err)
	}
	klog.Infof("Create network policy %s resources completed, update namespace loglevel", npKey)

	// 3. lock namespace
	nsInfo, nsUnlock = oc.getNamespaceLocked(policy.Namespace, false)
	if nsInfo == nil {
		// namespace was deleted while we were adding network policy,
		// try to cleanup network policy
		// expect retry to be handled by delete event that should come
		err = fmt.Errorf("unable to get namespace at the end of network policy %s creation: %v", npKey, err)
		return err
	}
	// 6. defer unlock namespace
	defer nsUnlock()

	// 4. check if namespace information related to network policy has changed,
	// network policy only reacts to namespace update ACL log level.
	// Run handleNetPolNamespaceUpdate sequence, but only for 1 newly added policy.
	err = oc.updateACLLoggingForPolicy(np, &nsInfo.aclLogging, nsInfo.aclLogging.Allow != aclLogging.Allow,
		nsInfo.aclLogging.Deny != aclLogging.Deny)
	if err != nil {
		return fmt.Errorf("network policy %s failed to be created: update policy ACLs failed: %v", npKey, err)
	} else {
		klog.Infof("Policy %s: ACL logging setting updated to deny=%s allow=%s",
			npKey, nsInfo.aclLogging.Deny, nsInfo.aclLogging.Allow)
	}

	// 5. subscribe to namespace update events
	nsInfo.relatedNetworkPolicies[npKey] = true
	return nil
}

// buildNetworkPolicyACLs builds the ACLS associated with the 'gress policies
// of the provided network policy.
func (oc *DefaultNetworkController) buildNetworkPolicyACLs(pgHashName string, np *networkPolicy, aclLogging *ACLLoggingLevels) []*nbdb.ACL {
	acls := []*nbdb.ACL{}
	for _, gp := range np.ingressPolicies {
		acl := gp.buildLocalPodACLs(pgHashName, aclLogging)
		acls = append(acls, acl...)
	}
	for _, gp := range np.egressPolicies {
		acl := gp.buildLocalPodACLs(pgHashName, aclLogging)
		acls = append(acls, acl...)
	}

	return acls
}

// deleteNetworkPolicy removes a network policy
// It only uses Namespace and Name from given network policy
func (oc *DefaultNetworkController) deleteNetworkPolicy(policy *knet.NetworkPolicy) error {
	npKey := getPolicyKey(policy)
	klog.Infof("Deleting network policy %s", npKey)

	// First lock and update namespace
	nsInfo, nsUnlock := oc.getNamespaceLocked(policy.Namespace, false)
	if nsInfo != nil {
		// unsubscribe from namespace events
		delete(nsInfo.relatedNetworkPolicies, npKey)
		nsUnlock()
	}
	// Next cleanup network policy
	err := oc.networkPolicies.DoWithLock(npKey, func(npKey string) error {
		np, ok := oc.networkPolicies.Load(npKey)
		if !ok {
			klog.Infof("Deleting policy %s that is already deleted", npKey)
			return nil
		}
		if err := oc.cleanupNetworkPolicy(np); err != nil {
			return fmt.Errorf("deleting policy %s failed: %v", npKey, err)
		}
		return nil
	})
	return err
}

// cleanupNetworkPolicy should be retriable
// It takes and releases networkPolicy lock.
// It updates oc.networkPolicies on success, should be called with oc.networkPolicies key locked.
// No need to log network policy key here, because caller of cleanupNetworkPolicy should prepend error message with
// that information.
func (oc *DefaultNetworkController) cleanupNetworkPolicy(np *networkPolicy) error {
	klog.Infof("Cleanup network policy %s", np.getKey())
	np.Lock()
	defer np.Unlock()

	// signal to local peer handlers to ignore new events
	np.deleted = true

	// stop handlers, retriable
	oc.shutdownHandlers(np)

	err := oc.deleteNetworkLocalPolicy(np)
	if err != nil {
		return err
	}

	// Delete ingress/egress address sets, idempotent
	for i, policy := range np.ingressPolicies {
		err = policy.destroy()
		if err != nil {
			// remove deleted policies from the list
			np.ingressPolicies = np.ingressPolicies[i:]
			return fmt.Errorf("failed to delete network policy ingress address sets: %v", err)
		}
	}
	np.ingressPolicies = make([]*gressPolicy, 0)
	for i, policy := range np.egressPolicies {
		err = policy.destroy()
		if err != nil {
			// remove deleted policies from the list
			np.egressPolicies = np.egressPolicies[i:]
			return fmt.Errorf("failed to delete network policy egress address sets: %v", err)
		}
	}
	np.egressPolicies = make([]*gressPolicy, 0)

	// finally, delete netpol from existing networkPolicies
	// this is the signal that cleanup was successful
	oc.networkPolicies.Delete(np.getKey())
	return nil
}

// handlePeerPodSelectorAddUpdate adds the IP address of a pod that has been
// selected as a peer by a NetworkPolicy's ingress/egress section to that
// ingress/egress address set
func (oc *DefaultNetworkController) handlePeerPodSelectorAddUpdate(np *networkPolicy, gp *gressPolicy, objs ...interface{}) error {
	np.RLock()
	defer np.RUnlock()
	if np.deleted {
		return nil
	}
	pods := make([]*kapi.Pod, 0, len(objs))
	for _, obj := range objs {
		pod := obj.(*kapi.Pod)
		if pod.Spec.NodeName == "" {
			// update event will be received for this pod later, no ips should be assigned yet
			continue
		}
		pods = append(pods, pod)
	}
	// gressPolicy.addPeerPods must be called with networkPolicy RLock.
	return gp.addPeerPods(pods...)
}

// handlePeerPodSelectorDelete removes the IP address of a pod that no longer
// matches a NetworkPolicy ingress/egress section's selectors from that
// ingress/egress address set
func (oc *DefaultNetworkController) handlePeerPodSelectorDelete(np *networkPolicy, gp *gressPolicy, obj interface{}) error {
	np.RLock()
	defer np.RUnlock()
	if np.deleted {
		return nil
	}
	pod := obj.(*kapi.Pod)
	if pod.Spec.NodeName == "" {
		klog.Infof("Pod %s/%s not scheduled on any node, skipping it", pod.Namespace, pod.Name)
		return nil
	}
	// gressPolicy.deletePeerPod must be called with networkPolicy RLock.
	if err := gp.deletePeerPod(pod); err != nil {
		return err
	}
	return nil
}

type NetworkPolicyExtraParameters struct {
	np          *networkPolicy
	gp          *gressPolicy
	spgInfo     *sharedPortGroupInfo
	podSelector labels.Selector
}

// addPeerPodHandler starts a watcher for PeerPodSelectorType.
// Sync function and Add event for every existing namespace will be executed sequentially first, and an error will be
// returned if something fails.
// PeerPodSelectorType uses handlePeerPodSelectorAddUpdate on Add and Update,
// and handlePeerPodSelectorDelete on Delete.
func (oc *DefaultNetworkController) addPeerPodHandler(podSelector *metav1.LabelSelector,
	gp *gressPolicy, np *networkPolicy, namespace string) error {

	// NetworkPolicy is validated by the apiserver; this can't fail.
	sel, _ := metav1.LabelSelectorAsSelector(podSelector)

	// start watching pods in the same namespace as the network policy and selected by the
	// label selector
	syncFunc := func(objs []interface{}) error {
		// ignore returned error, since any pod that wasn't properly handled will be retried individually.
		_ = oc.handlePeerPodSelectorAddUpdate(np, gp, objs...)
		return nil
	}
	retryPeerPods := oc.newRetryFrameworkWithParameters(
		factory.PeerPodSelectorType,
		syncFunc,
		&NetworkPolicyExtraParameters{
			np: np,
			gp: gp,
		})

	podHandler, err := retryPeerPods.WatchResourceFiltered(namespace, sel)
	if err != nil {
		klog.Errorf("Failed WatchResource for addPeerPodHandler: %v", err)
		return err
	}

	np.podHandlerList = append(np.podHandlerList, podHandler)
	return nil
}

func (oc *DefaultNetworkController) handlePeerNamespaceAndPodAdd(np *networkPolicy, gp *gressPolicy,
	podSelector labels.Selector, obj interface{}) error {
	namespace := obj.(*kapi.Namespace)
	np.RLock()
	locked := true
	defer func() {
		if locked {
			np.RUnlock()
		}
	}()
	if np.deleted {
		return nil
	}

	// start watching pods in this namespace and selected by the label selector in extraParameters.podSelector
	syncFunc := func(objs []interface{}) error {
		// ignore returned error, since any pod that wasn't properly handled will be retried individually.
		_ = oc.handlePeerPodSelectorAddUpdate(np, gp, objs...)
		return nil
	}
	retryPeerPods := oc.newRetryFrameworkWithParameters(
		factory.PeerPodForNamespaceAndPodSelectorType,
		syncFunc,
		&NetworkPolicyExtraParameters{
			gp: gp,
			np: np,
		},
	)
	// syncFunc and factory.PeerPodForNamespaceAndPodSelectorType add event handler also take np.RLock,
	// and will be called form the same thread. The same thread shouldn't take the same rlock twice.
	// unlock
	np.RUnlock()
	locked = false
	podHandler, err := retryPeerPods.WatchResourceFiltered(namespace.Name, podSelector)
	if err != nil {
		klog.Errorf("Failed WatchResource for PeerNamespaceAndPodSelectorType: %v", err)
		return err
	}
	// lock networkPolicy again to update namespacedPodHandlers
	np.RLock()
	locked = true
	if np.deleted {
		oc.watchFactory.RemovePodHandler(podHandler)
		return nil
	}
	np.namespacedPodHandlers.Store(namespace.Name, podHandler)
	return nil
}

func (oc *DefaultNetworkController) handlePeerNamespaceAndPodDel(np *networkPolicy, gp *gressPolicy, obj interface{}) error {
	np.RLock()
	defer np.RUnlock()
	if np.deleted {
		return nil
	}

	// when the namespace labels no longer apply
	// stop pod handler,
	// remove the namespaces pods from the address_set
	var errs []error
	namespace := obj.(*kapi.Namespace)

	if handler, ok := np.namespacedPodHandlers.Load(namespace.Name); ok {
		oc.watchFactory.RemovePodHandler(handler.(*factory.Handler))
		np.namespacedPodHandlers.Delete(namespace.Name)
	}

	pods, err := oc.watchFactory.GetPods(namespace.Name)
	if err != nil {
		return fmt.Errorf("failed to get namespace %s pods: %v", namespace.Namespace, err)
	}
	for _, pod := range pods {
		// call functions from handlePeerPodSelectorDelete
		// gressPolicy.deletePeerPod must be called with networkPolicy RLock.
		if err = gp.deletePeerPod(pod); err != nil {
			errs = append(errs, err)
		}
	}
	return kerrorsutil.NewAggregate(errs)
}

// addPeerNamespaceAndPodHandler starts a watcher for PeerNamespaceAndPodSelectorType.
// Add event for every existing namespace will be executed sequentially first, and an error will be
// returned if something fails.
// PeerNamespaceAndPodSelectorType uses handlePeerNamespaceAndPodAdd on Add,
// and handlePeerNamespaceAndPodDel on Delete.
func (oc *DefaultNetworkController) addPeerNamespaceAndPodHandler(namespaceSelector *metav1.LabelSelector,
	podSelector *metav1.LabelSelector, gp *gressPolicy, np *networkPolicy) error {
	// NetworkPolicy is validated by the apiserver; this can't fail.
	nsSel, _ := metav1.LabelSelectorAsSelector(namespaceSelector)
	podSel, _ := metav1.LabelSelectorAsSelector(podSelector)

	// start watching namespaces selected by the namespace selector nsSel;
	// upon namespace add event, start watching pods in that namespace selected
	// by the label selector podSel
	retryPeerNamespaces := oc.newRetryFrameworkWithParameters(
		factory.PeerNamespaceAndPodSelectorType,
		nil,
		&NetworkPolicyExtraParameters{
			gp:          gp,
			np:          np,
			podSelector: podSel}, // will be used in the addFunc to create a pod handler
	)

	namespaceHandler, err := retryPeerNamespaces.WatchResourceFiltered("", nsSel)
	if err != nil {
		klog.Errorf("Failed WatchResource for addPeerNamespaceAndPodHandler: %v", err)
		return err
	}

	np.nsHandlerList = append(np.nsHandlerList, namespaceHandler)
	return nil
}

func (oc *DefaultNetworkController) handlePeerNamespaceSelectorAdd(np *networkPolicy, gp *gressPolicy, objs ...interface{}) error {
	np.RLock()
	if np.deleted {
		np.RUnlock()
		return nil
	}
	updated := false
	for _, obj := range objs {
		namespace := obj.(*kapi.Namespace)
		// addNamespaceAddressSet is safe for concurrent use, doesn't require additional synchronization
		if gp.addNamespaceAddressSet(namespace.Name) {
			updated = true
		}
	}
	np.RUnlock()
	// unlock networkPolicy, before calling peerNamespaceUpdate
	if updated {
		return oc.peerNamespaceUpdate(np, gp)
	}
	return nil
}

func (oc *DefaultNetworkController) handlePeerNamespaceSelectorDel(np *networkPolicy, gp *gressPolicy, objs ...interface{}) error {
	np.RLock()
	if np.deleted {
		np.RUnlock()
		return nil
	}
	updated := false
	for _, obj := range objs {
		namespace := obj.(*kapi.Namespace)
		// delNamespaceAddressSet is safe for concurrent use, doesn't require additional synchronization
		if gp.delNamespaceAddressSet(namespace.Name) {
			updated = true
		}
	}
	np.RUnlock()
	// unlock networkPolicy, before calling peerNamespaceUpdate
	if updated {
		return oc.peerNamespaceUpdate(np, gp)
	}
	return nil
}

// peerNamespaceUpdate updates gress ACLs, for this purpose it need to take nsInfo lock and np.RLock
// make sure to pass unlocked networkPolicy
func (oc *DefaultNetworkController) peerNamespaceUpdate(np *networkPolicy, gp *gressPolicy) error {
	// Lock namespace before locking np
	// this is to make sure we don't miss update acl loglevel event for namespace.
	// The order of locking is strict: namespace first, then network policy, otherwise deadlock may happen
	nsInfo, nsUnlock := oc.getNamespaceLocked(np.namespace, true)
	var aclLogging *ACLLoggingLevels
	if nsInfo == nil {
		aclLogging = &ACLLoggingLevels{
			Allow: "",
			Deny:  "",
		}
	} else {
		defer nsUnlock()
		aclLogging = &nsInfo.aclLogging
	}
	np.RLock()
	defer np.RUnlock()
	if np.deleted {
		return nil
	}
	// buildLocalPodACLs is safe for concurrent use, see function comment for details
	acls := gp.buildLocalPodACLs(np.portGroupName, aclLogging)
	ops, err := libovsdbops.CreateOrUpdateACLsOps(oc.nbClient, nil, acls...)
	if err != nil {
		return err
	}
	ops, err = libovsdbops.AddACLsToPortGroupOps(oc.nbClient, ops, np.portGroupName, acls...)
	if err != nil {
		return err
	}
	_, err = libovsdbops.TransactAndCheck(oc.nbClient, ops)
	return err
}

// addPeerNamespaceHandler starts a watcher for PeerNamespaceSelectorType.
// Sync function and Add event for every existing namespace will be executed sequentially first, and an error will be
// returned if something fails.
// PeerNamespaceSelectorType uses handlePeerNamespaceSelectorAdd on Add,
// and handlePeerNamespaceSelectorDel on Delete.
func (oc *DefaultNetworkController) addPeerNamespaceHandler(
	namespaceSelector *metav1.LabelSelector,
	gress *gressPolicy, np *networkPolicy) error {

	// NetworkPolicy is validated by the apiserver; this can't fail.
	sel, _ := metav1.LabelSelectorAsSelector(namespaceSelector)

	// start watching namespaces selected by the namespace selector
	syncFunc := func(objs []interface{}) error {
		// ignore returned error, since any namespace that wasn't properly handled will be retried individually.
		_ = oc.handlePeerNamespaceSelectorAdd(np, gress, objs...)
		return nil
	}
	retryPeerNamespaces := oc.newRetryFrameworkWithParameters(
		factory.PeerNamespaceSelectorType,
		syncFunc,
		&NetworkPolicyExtraParameters{gp: gress, np: np},
	)

	namespaceHandler, err := retryPeerNamespaces.WatchResourceFiltered("", sel)
	if err != nil {
		klog.Errorf("WatchResource failed for addPeerNamespaceHandler: %v", err)
		return err
	}

	np.nsHandlerList = append(np.nsHandlerList, namespaceHandler)
	return nil
}

func (oc *DefaultNetworkController) shutdownHandlers(np *networkPolicy) {
	for _, handler := range np.podHandlerList {
		oc.watchFactory.RemovePodHandler(handler)
	}
	np.podHandlerList = make([]*factory.Handler, 0)
	for _, handler := range np.nsHandlerList {
		oc.watchFactory.RemoveNamespaceHandler(handler)
	}
	np.nsHandlerList = make([]*factory.Handler, 0)
	np.namespacedPodHandlers.Range(func(_, value interface{}) bool {
		oc.watchFactory.RemovePodHandler(value.(*factory.Handler))
		return true
	})
	np.namespacedPodHandlers = sync.Map{}
}

// The following 2 functions should return the same key for network policy based on k8s on internal networkPolicy object
func getPolicyKey(policy *knet.NetworkPolicy) string {
	return fmt.Sprintf("%v/%v", policy.Namespace, policy.Name)
}

func (np *networkPolicy) getKey() string {
	return fmt.Sprintf("%v/%v", np.namespace, np.name)
}

// PortGroupHasPorts returns true if a port group contains all given ports
func PortGroupHasPorts(nbClient libovsdbclient.Client, pgName string, portUUIDs []string) bool {
	pg := &nbdb.PortGroup{
		Name: pgName,
	}
	pg, err := libovsdbops.GetPortGroup(nbClient, pg)
	if err != nil {
		return false
	}

	return sets.NewString(pg.Ports...).HasAll(portUUIDs...)
}

type sharedPortGroupInfo struct {
	pgName           string          // readable name of this shared port group
	pgHashName       string          // shared port group name
	policies         map[string]bool // policies sharing this port group
	lspIngressRefCnt int             // requires ingress default deny acl in this port group is it is greater than 0
	lspEgressRefCnt  int             // requires egress default deny acl in this port group is it is greater than 0
	podHandler       *factory.Handler

	// localPods is a map of pods affected by this shared port group.
	// It is used to update shared port group port counters, when deleting network policy.
	// Port should only be added here if it was successfully added to shared deny port group,
	// and local port group in db.
	// localPods may be updated by multiple pod handlers at the same time,
	// therefore it uses a sync map to handle simultaneous access.
	// map of portName(string): portUUID(string)
	localPods sync.Map
}

func getSharedPortGroupNamePrefix(ns string) string {
	return ns + "_" + "shared_port_group" + "_"
}

// now we only support portGroup sharing if the policy's local pod selector is empty. This can be changed later.
func getSharedPortGroupName(policy *knet.NetworkPolicy) string {
	sel, _ := metav1.LabelSelectorAsSelector(&policy.Spec.PodSelector)
	return getSharedPortGroupNamePrefix(policy.Namespace) + sel.String()
}

// addNetworkPolicyToSharedPortGroup create shared port group if required, then add policy acls to the shared port group
func (oc *DefaultNetworkController) addNetworkPolicyToSharedPortGroup(spgInfo *sharedPortGroupInfo, aclLogging *ACLLoggingLevels, np *networkPolicy, policy *knet.NetworkPolicy) error {
	klog.Infof("Add network policy %s to shared portGroup readable name %s hashName %s", np.getKey(), spgInfo.pgName, spgInfo.pgHashName)

	_, ok := spgInfo.policies[np.getKey()]
	if ok {
		return nil
	}

	// create shared portGroup if this is the first policy in this shared port group
	if len(spgInfo.policies) == 0 {
		// NetworkPolicy is validated by the apiserver
		sel, err := metav1.LabelSelectorAsSelector(&policy.Spec.PodSelector)
		if err != nil {
			klog.Errorf("Could not set up watcher for local pods: %v", err)
			return err
		}

		// Add all local pods in a syncFunction to minimize db ops.
		syncFunc := func(objs []interface{}) error {
			return oc.handleInitialSelectedLocalPods(spgInfo, np, aclLogging, objs...)
		}
		retryLocalPods := oc.newRetryFrameworkWithParameters(
			factory.LocalPodSelectorType,
			syncFunc,
			&NetworkPolicyExtraParameters{
				spgInfo: spgInfo,
			})

		podHandler, err := retryLocalPods.WatchResourceFiltered(np.namespace, sel)
		if err != nil {
			klog.Errorf("WatchResource failed for addLocalPodHandler: %v", err)
			return err
		}

		spgInfo.podHandler = podHandler
	} else {
		acls := oc.buildLocalNetworkPolicyAcls(spgInfo, np, aclLogging)
		ops, err := libovsdbops.CreateOrUpdateACLsOps(oc.nbClient, nil, acls...)
		if err != nil {
			return fmt.Errorf("failed to create acls ops for network policy %s/%s: %v", np.namespace, np.name, err)
		}

		ops, err = libovsdbops.AddACLsToPortGroupOps(oc.nbClient, ops, spgInfo.pgHashName, acls...)
		if err != nil {
			return fmt.Errorf("failed to create ops to add acls of network policy %s/%s to shared port group %s: %v", np.namespace, np.name, spgInfo.pgName, err)
		}
		_, err = libovsdbops.TransactAndCheck(oc.nbClient, ops)
		if err != nil {
			return fmt.Errorf("failed to run ovsdb txn to add acls of network policy %s/%s to shared port group %s: %v", np.namespace, np.name, spgInfo.pgName, err)
		}
	}
	if np.isIngress {
		spgInfo.lspIngressRefCnt++
	}
	if np.isEgress {
		spgInfo.lspEgressRefCnt++
	}
	spgInfo.policies[np.getKey()] = true
	np.portGroupName = spgInfo.pgHashName
	return nil
}

// deleteNetworkPolicyFromSharedPortGroup remove policy acls from the shared port group and delete shared port group if this is the last policy in the portgroup
func (oc *DefaultNetworkController) deleteNetworkPolicyFromSharedPortGroup(spgInfo *sharedPortGroupInfo, np *networkPolicy) error {
	klog.Infof("Remove network policy %s from shared portGroup readable name %s hashName %s", np.getKey(), spgInfo.pgName, spgInfo.pgHashName)
	var err error
	var matchAcls []*nbdb.ACL

	err = nil
	_, ok := spgInfo.policies[np.getKey()]
	if !ok {
		return err
	}

	if np.isIngress {
		spgInfo.lspIngressRefCnt--
		defer func() {
			if err != nil {
				spgInfo.lspIngressRefCnt++
			}
		}()
	}
	if np.isEgress {
		spgInfo.lspEgressRefCnt--
		defer func() {
			if err != nil {
				spgInfo.lspEgressRefCnt++
			}
		}()
	}

	acls := oc.buildLocalNetworkPolicyAcls(spgInfo, np, nil)
	matchAcls, err = libovsdbops.FindACLsWithUUID(oc.nbClient, acls)
	if err != nil {
		return fmt.Errorf("failed to find acls %+v for network policy %s/%s sharing portGroup %s: %v",
			acls, np.namespace, np.name, spgInfo.pgName, err)
	}

	var ops []ovsdb.Operation
	ops, err = libovsdbops.DeleteACLsFromPortGroupOps(oc.nbClient, nil, spgInfo.pgHashName, matchAcls...)
	if err != nil {
		return fmt.Errorf("failed to create ops for remove acls %+v from shared port group %s: %v", matchAcls, spgInfo.pgName, err)
	}

	// create shared portGroup if this is the first policy in this shared port group
	if len(spgInfo.policies) == 1 {
		if spgInfo.podHandler != nil {
			oc.watchFactory.RemovePodHandler(spgInfo.podHandler)
		}
		spgInfo.podHandler = nil
		ops, err = libovsdbops.DeletePortGroupsOps(oc.nbClient, ops, spgInfo.pgHashName)
		if err != nil {
			return fmt.Errorf("failed to create ops to delete shared port group %s: %v", spgInfo.pgName, err)
		}
	}

	recordOps, txOkCallBack, _, err1 := metrics.GetConfigDurationRecorder().AddOVN(oc.nbClient, "networkpolicy",
		np.namespace, np.name)
	if err1 != nil {
		klog.Errorf("Failed to record config duration: %v", err1)
	}
	ops = append(ops, recordOps...)

	_, err = libovsdbops.TransactAndCheck(oc.nbClient, ops)
	if err != nil {
		return fmt.Errorf("failed to run ovsdb txn to delete network policy %s/%s from shared port group %s: %v", np.namespace, np.name, spgInfo.pgName, err)
	}
	txOkCallBack()
	delete(spgInfo.policies, np.getKey())
	np.portGroupName = ""
	return nil
}

func (oc *DefaultNetworkController) createNetworkLocalPolicy(np *networkPolicy, policy *knet.NetworkPolicy, aclLogging *ACLLoggingLevels) (err error) {
	var sharedPortGroupName string

	klog.Infof("Policy %s/%s to be added to shared portGroup", np.namespace, np.name)
	err = oc.localPodSelectorsPerNamespace.DoWithLock(np.namespace, func(namespace string) error {
		localPodSelectorInfos, loaded := oc.localPodSelectorsPerNamespace.LoadOrStore(namespace,
			map[string]*metav1.LabelSelector{})
		if loaded {
			for k, selector := range localPodSelectorInfos {
				if reflect.DeepEqual(*selector, policy.Spec.PodSelector) {
					sharedPortGroupName = k
					break
				}
			}
		}
		if sharedPortGroupName == "" {
			sharedPortGroupName = getSharedPortGroupName(policy)
		}
		pgHashName := hashedPortGroup(sharedPortGroupName)
		err = oc.sharedPortGroupInfos.DoWithLock(pgHashName, func(pgName string) error {
			spgInfo, loaded := oc.sharedPortGroupInfos.LoadOrStore(pgHashName, &sharedPortGroupInfo{
				pgName:           sharedPortGroupName,
				pgHashName:       pgHashName,
				policies:         map[string]bool{},
				lspIngressRefCnt: 0,
				lspEgressRefCnt:  0,
				podHandler:       nil,
				localPods:        sync.Map{},
			})
			err = oc.addNetworkPolicyToSharedPortGroup(spgInfo, aclLogging, np, policy)
			if err != nil && !loaded {
				oc.sharedPortGroupInfos.Delete(pgHashName)
			}
			return err
		})
		if err != nil {
			if !loaded {
				oc.localPodSelectorsPerNamespace.Delete(namespace)
			}
		} else {
			localPodSelectorInfos[sharedPortGroupName] = &policy.Spec.PodSelector
		}
		return err
	})
	return err
}

func (oc *DefaultNetworkController) deleteNetworkLocalPolicy(np *networkPolicy) (err error) {
	var readableSharedPortGroupName string

	if np.portGroupName == "" {
		return nil
	}

	klog.Infof("Policy %s/%s to be deleted from shared portGroup %s", np.namespace, np.name, np.portGroupName)
	err = oc.localPodSelectorsPerNamespace.DoWithLock(np.namespace, func(namespace string) error {
		localPodSelectorInfos, loaded := oc.localPodSelectorsPerNamespace.Load(namespace)
		if !loaded {
			return fmt.Errorf("failed to find shared port group in namespace %s, this should never happen", namespace)
		}
		err = oc.sharedPortGroupInfos.DoWithLock(np.portGroupName, func(pgName string) error {
			spgInfo, loaded := oc.sharedPortGroupInfos.Load(pgName)
			if !loaded {
				return fmt.Errorf("failed to find shared port group %s, this should never happen", pgName)
			}
			readableSharedPortGroupName = spgInfo.pgName
			err = oc.deleteNetworkPolicyFromSharedPortGroup(spgInfo, np)
			if err != nil {
				return err
			}
			if len(spgInfo.policies) == 0 {
				oc.sharedPortGroupInfos.Delete(pgName)
			}
			return nil
		})
		if err == nil && len(oc.sharedPortGroupInfos.GetKeys()) == 0 {
			delete(localPodSelectorInfos, readableSharedPortGroupName)
			if len(localPodSelectorInfos) == 0 {
				oc.localPodSelectorsPerNamespace.Delete(namespace)
			}
		}
		return err
	})
	return err
}
