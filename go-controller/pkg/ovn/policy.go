package ovn

import (
	"fmt"
	"net"
	"reflect"
	"sync"

	goovn "github.com/ebay/go-ovn"
	libovsdbclient "github.com/ovn-org/libovsdb/client"
	ovsdb "github.com/ovn-org/libovsdb/ovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	kapi "k8s.io/api/core/v1"
	knet "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
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
	// namespaceACLExtIdKey external ID key for namespace on 'gress policy ACLs
	namespaceACLExtIdKey = "namespace"
	// policyACLExtIdKey external ID key for policy name on 'gress policy ACLs
	policyACLExtIdKey = "policy"
	// policyACLExtKey external ID key for policy type on 'gress policy ACLs
	policyTypeACLExtIdKey = "policy_type"
	// policyTypeNumACLExtIdKey external ID key for policy index by type on 'gress policy ACLs
	policyTypeNumACLExtIdKey = "%s_num"
)

type networkPolicy struct {
	// RWMutex synchronizes operations on the policy.
	// Operations that change local and peer pods take a RLock,
	// whereas operations that affect the policy take a Lock.
	sync.RWMutex
	name            string
	namespace       string
	policyTypes     []knet.PolicyType
	ingressPolicies []*gressPolicy
	egressPolicies  []*gressPolicy
	podHandlerList  []*factory.Handler
	svcHandlerList  []*factory.Handler
	nsHandlerList   []*factory.Handler

	// localPods is a list of pods affected by ths policy
	// this is a sync map so we can handle multiple pods at once
	// map of string -> *lpInfo
	localPods sync.Map

	portGroupName string
	deleted       bool //deleted policy
}

func NewNetworkPolicy(policy *knet.NetworkPolicy) *networkPolicy {
	np := &networkPolicy{
		name:            policy.Name,
		namespace:       policy.Namespace,
		policyTypes:     policy.Spec.PolicyTypes,
		ingressPolicies: make([]*gressPolicy, 0),
		egressPolicies:  make([]*gressPolicy, 0),
		podHandlerList:  make([]*factory.Handler, 0),
		svcHandlerList:  make([]*factory.Handler, 0),
		nsHandlerList:   make([]*factory.Handler, 0),
		localPods:       sync.Map{},
	}
	return np
}

const (
	noneMatch = "None"
	// Default ACL logging severity
	defaultACLLoggingSeverity = "info"
	// IPv6 multicast traffic destined to dynamic groups must have the "T" bit
	// set to 1: https://tools.ietf.org/html/rfc3307#section-4.3
	ipv6DynamicMulticastMatch = "(ip6.dst[120..127] == 0xff && ip6.dst[116] == 1)"
	// Legacy multicastDefaultDeny port group removed by commit 40a90f0
	legacyMulticastDefaultDenyPortGroup = "mcastPortGroupDeny"
)

func getACLLoggingSeverity(aclLogging string) string {
	if aclLogging != "" {
		return aclLogging
	}
	return defaultACLLoggingSeverity
}

// hash the provided input to make it a valid portGroup name.
func hashedPortGroup(s string) string {
	return util.HashForOVN(s)
}

func (oc *Controller) syncNetworkPolicies(networkPolicies []interface{}) {
	expectedPolicies := make(map[string]map[string]bool)
	for _, npInterface := range networkPolicies {
		policy, ok := npInterface.(*knet.NetworkPolicy)
		if !ok {
			klog.Errorf("Spurious object in syncNetworkPolicies: %v",
				npInterface)
			continue
		}

		if nsMap, ok := expectedPolicies[policy.Namespace]; ok {
			nsMap[policy.Name] = true
		} else {
			expectedPolicies[policy.Namespace] = map[string]bool{
				policy.Name: true,
			}
		}
	}

	stalePGs := []string{}
	err := oc.addressSetFactory.ProcessEachAddressSet(func(addrSetName, namespaceName, policyName string) {
		if policyName != "" && !expectedPolicies[namespaceName][policyName] {
			// policy doesn't exist on k8s. Delete the port group
			portGroupName := fmt.Sprintf("%s_%s", namespaceName, policyName)
			hashedLocalPortGroup := hashedPortGroup(portGroupName)
			stalePGs = append(stalePGs, hashedLocalPortGroup)
			// delete the address sets for this old policy from OVN
			if err := oc.addressSetFactory.DestroyAddressSetInBackingStore(addrSetName); err != nil {
				klog.Errorf(err.Error())
			}
		}
	})
	if err != nil {
		klog.Errorf("Error in syncing network policies: %v", err)
	}

	if len(stalePGs) > 0 {
		err = libovsdbops.DeletePortGroups(oc.nbClient, stalePGs...)
		if err != nil {
			klog.Errorf("Error removing stale port groups %v: %v", stalePGs, err)
		}
	}
}

func addAllowACLFromNode(logicalSwitch string, mgmtPortIP net.IP, ovnNBClient goovn.Client) error {
	ipFamily := "ip4"
	if utilnet.IsIPv6(mgmtPortIP) {
		ipFamily = "ip6"
	}
	match := fmt.Sprintf("%s.src==%s", ipFamily, mgmtPortIP.String())

	priority := types.DefaultAllowPriority

	// TODO use libovsdb client once logical switch libovsdb work is done
	aclcmd, err := ovnNBClient.ACLAdd(logicalSwitch, types.DirectionToLPort, match, "allow-related", priority, nil, false, "", "")

	// NOTE: goovn.ErrorExist is returned if the ACL already exists, in such a case ignore that error.
	// Additional Context-> Per Tim Rozet's review comments, there could be scenarios where ovnkube restarts, in which
	// case, it would use kubernetes events to reconstruct ACLs and there is a possibility that some of the ACLs may
	// already be present in the NBDB.
	if err == nil {
		if err = ovnNBClient.Execute(aclcmd); err != nil {
			return fmt.Errorf("failed to create the node acl for logical_switch: %s, %v", logicalSwitch, err)
		}
	} else if err != goovn.ErrorExist {
		return fmt.Errorf("ACLAdd() error when creating node acl for logical switch: %s, %v", logicalSwitch, err)
	}

	return nil
}

func getACLMatch(portGroupName, match string, policyType knet.PolicyType) string {
	var aclMatch string
	if policyType == knet.PolicyTypeIngress {
		aclMatch = "outport == @" + portGroupName
	} else {
		aclMatch = "inport == @" + portGroupName
	}

	if match != "" {
		aclMatch += " && " + match
	}

	return aclMatch
}

func namespacePortGroupACLName(namespace, portGroup, name string) string {
	policyNamespace := namespace
	if policyNamespace == "" {
		policyNamespace = portGroup
	}
	if name == "" {
		return policyNamespace

	}
	return fmt.Sprintf("%s_%s", policyNamespace, name)
}

func buildACL(namespace, portGroup, name, direction string, priority int, match, action, aclLogging string, policyType knet.PolicyType) *nbdb.ACL {
	aclName := namespacePortGroupACLName(namespace, portGroup, name)
	log := aclLogging != ""
	severity := getACLLoggingSeverity(aclLogging)
	meter := types.OvnACLLoggingMeter
	var externalIds map[string]string
	if policyType != "" {
		externalIds = map[string]string{
			defaultDenyPolicyTypeACLExtIdKey: string(policyType),
		}
	}

	return libovsdbops.BuildACL(aclName, direction, priority, match, action, meter, severity, log, externalIds)
}

func defaultDenyPortGroup(namespace, gressSuffix string) string {
	return hashedPortGroup(namespace) + "_" + gressSuffix
}

func buildDenyACLs(namespace, policy, pg, aclLogging string, policyType knet.PolicyType) (denyACL, allowACL *nbdb.ACL) {
	denyMatch := getACLMatch(pg, "", policyType)
	denyACL = buildACL(namespace, pg, policy, nbdb.ACLDirectionToLport, types.DefaultDenyPriority, denyMatch, nbdb.ACLActionDrop, aclLogging, policyType)
	allowMatch := getACLMatch(pg, "arp", policyType)
	allowACL = buildACL(namespace, pg, "ARPallowPolicy", nbdb.ACLDirectionToLport, types.DefaultAllowPriority, allowMatch, nbdb.ACLActionAllow, "", policyType)
	return
}

func (oc *Controller) createDefaultDenyPGAndACLs(namespace, policy string, nsInfo *namespaceInfo) error {
	aclLogging := nsInfo.aclLogging.Deny

	ingressPGName := defaultDenyPortGroup(namespace, "ingressDefaultDeny")
	ingressDenyACL, ingressAllowACL := buildDenyACLs(namespace, policy, ingressPGName, aclLogging, knet.PolicyTypeIngress)
	egressPGName := defaultDenyPortGroup(namespace, "egressDefaultDeny")
	egressDenyACL, egressAllowACL := buildDenyACLs(namespace, policy, egressPGName, aclLogging, knet.PolicyTypeEgress)
	ops, err := libovsdbops.CreateOrUpdateACLsOps(oc.nbClient, nil, ingressDenyACL, ingressAllowACL, egressDenyACL, egressAllowACL)
	if err != nil {
		return err
	}

	ingressPG := libovsdbops.BuildPortGroup(ingressPGName, ingressPGName, nil, []*nbdb.ACL{ingressDenyACL, ingressAllowACL})
	egressPG := libovsdbops.BuildPortGroup(egressPGName, egressPGName, nil, []*nbdb.ACL{egressDenyACL, egressAllowACL})
	ops, err = libovsdbops.CreateOrUpdatePortGroupsOps(oc.nbClient, ops, ingressPG, egressPG)
	if err != nil {
		return err
	}

	_, err = libovsdbops.TransactAndCheck(oc.nbClient, ops)
	if err != nil {
		return err
	}

	nsInfo.portGroupEgressDenyName = egressPGName
	nsInfo.portGroupIngressDenyName = ingressPGName

	return nil
}

// modify ACL logging
func (oc *Controller) setACLDenyLogging(ns string, nsInfo *namespaceInfo, aclLogging string) error {
	ingressDenyACL, _ := buildDenyACLs(ns, "", nsInfo.portGroupIngressDenyName, aclLogging, knet.PolicyTypeIngress)
	egressDenyACL, _ := buildDenyACLs(ns, "", nsInfo.portGroupEgressDenyName, aclLogging, knet.PolicyTypeEgress)

	ops, err := libovsdbops.UpdateACLLoggingOps(oc.nbClient, nil, ingressDenyACL)
	if err != nil {
		return err
	}

	ops, err = libovsdbops.UpdateACLLoggingOps(oc.nbClient, ops, egressDenyACL)
	if err != nil {
		return err
	}

	_, err = libovsdbops.TransactAndCheck(oc.nbClient, ops)
	if err != nil {
		return err
	}

	return nil
}

func getACLMatchAF(ipv4Match, ipv6Match string) string {
	if config.IPv4Mode && config.IPv6Mode {
		return "(" + ipv4Match + " || " + ipv6Match + ")"
	} else if config.IPv4Mode {
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
func getMulticastACLIgrMatch(nsInfo *namespaceInfo) string {
	var ipv4Match, ipv6Match string
	addrSetNameV4, addrSetNameV6 := nsInfo.addressSet.GetASHashNames()
	if config.IPv4Mode {
		ipv4Match = getMulticastACLIgrMatchV4(addrSetNameV4)
	}
	if config.IPv6Mode {
		ipv6Match = getMulticastACLIgrMatchV6(addrSetNameV6)
	}
	return getACLMatchAF(ipv4Match, ipv6Match)
}

// Creates the match string used for ACLs allowing outgoing multicast from a
// namespace.
func getMulticastACLEgrMatch() string {
	var ipv4Match, ipv6Match string
	if config.IPv4Mode {
		ipv4Match = "ip4.mcast"
	}
	if config.IPv6Mode {
		ipv6Match = "(mldv1 || mldv2 || " + ipv6DynamicMulticastMatch + ")"
	}
	return getACLMatchAF(ipv4Match, ipv6Match)
}

// Creates a policy to allow multicast traffic within 'ns':
// - a port group containing all logical ports associated with 'ns'
// - one "from-lport" ACL allowing egress multicast traffic from the pods
//   in 'ns'
// - one "to-lport" ACL allowing ingress multicast traffic to pods in 'ns'.
//   This matches only traffic originated by pods in 'ns' (based on the
//   namespace address set).
func (oc *Controller) createMulticastAllowPolicy(ns string, nsInfo *namespaceInfo) error {
	portGroupName := hashedPortGroup(ns)

	egressMatch := getACLMatch(portGroupName, getMulticastACLEgrMatch(), knet.PolicyTypeEgress)
	egressACL := buildACL(ns, portGroupName, "MulticastAllowEgress", nbdb.ACLDirectionFromLport, types.DefaultMcastAllowPriority, egressMatch, nbdb.ACLActionAllow, "", knet.PolicyTypeEgress)
	ingressMatch := getACLMatch(portGroupName, getMulticastACLIgrMatch(nsInfo), knet.PolicyTypeIngress)
	ingressACL := buildACL(ns, portGroupName, "MulticastAllowIngress", nbdb.ACLDirectionToLport, types.DefaultMcastAllowPriority, ingressMatch, nbdb.ACLActionAllow, "", knet.PolicyTypeIngress)
	acls := []*nbdb.ACL{egressACL, ingressACL}
	ops, err := libovsdbops.CreateOrUpdateACLsOps(oc.nbClient, nil, acls...)
	if err != nil {
		return err
	}

	// Add all ports from this namespace to the multicast allow group.
	ports := []*nbdb.LogicalSwitchPort{}
	pods, err := oc.watchFactory.GetPods(ns)
	if err != nil {
		klog.Warningf("Failed to get pods for namespace %q: %v", ns, err)
	}
	for _, pod := range pods {
		portName := util.GetLogicalPortName(pod.Namespace, pod.Name)
		if portInfo, err := oc.logicalPortCache.get(portName); err != nil {
			klog.Errorf(err.Error())
		} else {
			ports = append(ports, &nbdb.LogicalSwitchPort{UUID: portInfo.uuid})
		}
	}

	pg := libovsdbops.BuildPortGroup(portGroupName, ns, ports, acls)
	ops, err = libovsdbops.CreateOrUpdatePortGroupsOps(oc.nbClient, ops, pg)
	if err != nil {
		return err
	}

	_, err = libovsdbops.TransactAndCheck(oc.nbClient, ops)
	if err != nil {
		return err
	}

	return nil
}

func deleteMulticastAllowPolicy(nbClient libovsdbclient.Client, ns string, nsInfo *namespaceInfo) error {
	portGroupName := hashedPortGroup(ns)

	egressMatch := getACLMatch(portGroupName, getMulticastACLEgrMatch(), knet.PolicyTypeEgress)
	egressACL := buildACL(ns, portGroupName, "MulticastAllowEgress", nbdb.ACLDirectionFromLport, types.DefaultMcastAllowPriority, egressMatch, nbdb.ACLActionAllow, "", knet.PolicyTypeEgress)

	ingressMatch := getACLMatch(portGroupName, getMulticastACLIgrMatch(nsInfo), knet.PolicyTypeIngress)
	ingressACL := buildACL(ns, portGroupName, "MulticastAllowIngress", nbdb.ACLDirectionToLport, types.DefaultMcastAllowPriority, ingressMatch, nbdb.ACLActionAllow, "", knet.PolicyTypeIngress)

	ops, err := libovsdbops.DeleteACLsFromPortGroupOps(nbClient, nil, portGroupName, egressACL, ingressACL)
	if err != nil {
		return err
	}

	ops, err = libovsdbops.DeletePortGroupsOps(nbClient, ops, portGroupName)
	if err != nil {
		return err
	}

	_, err = libovsdbops.TransactAndCheck(nbClient, ops)
	if err != nil {
		return err
	}

	return nil
}

// Creates a global default deny multicast policy:
// - one ACL dropping egress multicast traffic from all pods: this is to
//   protect OVN controller from processing IP multicast reports from nodes
//   that are not allowed to receive multicast raffic.
// - one ACL dropping ingress multicast traffic to all pods.
// Caller must hold the namespace's namespaceInfo object lock.
func (oc *Controller) createDefaultDenyMulticastPolicy() error {
	match := getMulticastACLMatch()

	// By default deny any egress multicast traffic from any pod. This drops
	// IP multicast membership reports therefore denying any multicast traffic
	// to be forwarded to pods.
	egressACL := buildACL("", types.ClusterPortGroupName, "DefaultDenyMulticastEgress", nbdb.ACLDirectionFromLport, types.DefaultMcastDenyPriority, match, nbdb.ACLActionDrop, "", knet.PolicyTypeEgress)

	// By default deny any ingress multicast traffic to any pod.
	ingressACL := buildACL("", types.ClusterPortGroupName, "DefaultDenyMulticastIngress", nbdb.ACLDirectionToLport, types.DefaultMcastDenyPriority, match, nbdb.ACLActionDrop, "", knet.PolicyTypeIngress)

	ops, err := libovsdbops.CreateOrUpdateACLsOps(oc.nbClient, nil, egressACL, ingressACL)
	if err != nil {
		return err
	}

	ops, err = libovsdbops.AddACLsToPortGroupOps(oc.nbClient, ops, types.ClusterPortGroupName, egressACL, ingressACL)
	if err != nil {
		return err
	}

	// Remove old multicastDefaultDeny port group now that all ports
	// have been added to the clusterPortGroup by WatchPods()
	ops, err = libovsdbops.DeletePortGroupsOps(oc.nbClient, ops, legacyMulticastDefaultDenyPortGroup)
	if err != nil {
		return err
	}

	_, err = libovsdbops.TransactAndCheck(oc.nbClient, ops)
	if err != nil {
		return err
	}

	return nil
}

// Creates a global default allow multicast policy:
// - one ACL allowing multicast traffic from cluster router ports
// - one ACL allowing multicast traffic to cluster router ports.
// Caller must hold the namespace's namespaceInfo object lock.
func (oc *Controller) createDefaultAllowMulticastPolicy() error {
	mcastMatch := getMulticastACLMatch()

	egressMatch := getACLMatch(types.ClusterRtrPortGroupName, mcastMatch, knet.PolicyTypeEgress)
	egressACL := buildACL("", types.ClusterRtrPortGroupName, "DefaultAllowMulticastEgress", nbdb.ACLDirectionFromLport, types.DefaultMcastAllowPriority, egressMatch, nbdb.ACLActionAllow, "", knet.PolicyTypeEgress)

	ingressMatch := getACLMatch(types.ClusterRtrPortGroupName, mcastMatch, knet.PolicyTypeIngress)
	ingressACL := buildACL("", types.ClusterRtrPortGroupName, "DefaultAllowMulticastIngress", nbdb.ACLDirectionToLport, types.DefaultMcastAllowPriority, ingressMatch, nbdb.ACLActionAllow, "", knet.PolicyTypeEgress)

	ops, err := libovsdbops.CreateOrUpdateACLsOps(oc.nbClient, nil, egressACL, ingressACL)
	if err != nil {
		return err
	}

	ops, err = libovsdbops.AddACLsToPortGroupOps(oc.nbClient, ops, types.ClusterRtrPortGroupName, egressACL, ingressACL)
	if err != nil {
		return err
	}

	_, err = libovsdbops.TransactAndCheck(oc.nbClient, ops)
	if err != nil {
		return err
	}

	return nil
}

// podAddAllowMulticastPolicy adds the pod's logical switch port to the namespace's
// multicast port group. Caller must hold the namespace's namespaceInfo object
// lock.
func podAddAllowMulticastPolicy(nbClient libovsdbclient.Client, ns string, portInfo *lpInfo) error {
	return libovsdbops.AddPortsToPortGroup(nbClient, hashedPortGroup(ns), portInfo.uuid)
}

// podDeleteAllowMulticastPolicy removes the pod's logical switch port from the
// namespace's multicast port group. Caller must hold the namespace's
// namespaceInfo object lock.
func podDeleteAllowMulticastPolicy(nbClient libovsdbclient.Client, ns string, portInfo *lpInfo) error {
	return libovsdbops.DeletePortsFromPortGroup(nbClient, hashedPortGroup(ns), portInfo.uuid)
}

// localPodAddDefaultDeny ensures ports (i.e. pods) are in the correct
// default-deny portgroups. Whether or not pods are in default-deny depends
// on whether or not any policies select this pod, so there is a reference
// count to ensure we don't accidentally open up a pod.
func (oc *Controller) localPodAddDefaultDeny(nsInfo *namespaceInfo,
	policy *knet.NetworkPolicy, ports ...*lpInfo) (ingressDenyPorts, egressDenyPorts []string) {
	oc.lspMutex.Lock()

	// Default deny rule.
	// 1. Any pod that matches a network policy should get a default
	// ingress deny rule.  This is irrespective of whether there
	// is a ingress section in the network policy. But, if
	// PolicyTypes in the policy has only "egress" in it, then
	// it is a 'egress' only network policy and we should not
	// add any default deny rule for ingress.
	// 2. If there is any "egress" section in the policy or
	// the PolicyTypes has 'egress' in it, we add a default
	// egress deny rule.

	ingressDenyPorts = []string{}
	egressDenyPorts = []string{}

	// Handle condition 1 above.
	if !(len(policy.Spec.PolicyTypes) == 1 && policy.Spec.PolicyTypes[0] == knet.PolicyTypeEgress) {
		for _, portInfo := range ports {
			// if this is the first NP referencing this pod, then we
			// need to add it to the port group.
			if oc.lspIngressDenyCache[portInfo.name] == 0 {
				ingressDenyPorts = append(ingressDenyPorts, portInfo.uuid)
			}

			// increment the reference count.
			oc.lspIngressDenyCache[portInfo.name]++
		}
	}

	// Handle condition 2 above.
	if (len(policy.Spec.PolicyTypes) == 1 && policy.Spec.PolicyTypes[0] == knet.PolicyTypeEgress) ||
		len(policy.Spec.Egress) > 0 || len(policy.Spec.PolicyTypes) == 2 {
		for _, portInfo := range ports {
			if oc.lspEgressDenyCache[portInfo.name] == 0 {
				// again, reference count is 0, so add to port
				egressDenyPorts = append(egressDenyPorts, portInfo.uuid)
			}

			// bump reference count
			oc.lspEgressDenyCache[portInfo.name]++
		}
	}
	oc.lspMutex.Unlock()

	return
}

// localPodDelDefaultDeny decrements a pod's policy reference count and removes a pod
// from the default-deny portgroups if the reference count for the pod is 0
func (oc *Controller) localPodDelDefaultDeny(
	np *networkPolicy, nsInfo *namespaceInfo, ports ...*lpInfo) (ingressDenyPorts, egressDenyPorts []string) {
	oc.lspMutex.Lock()

	ingressDenyPorts = []string{}
	egressDenyPorts = []string{}

	// Remove port from ingress deny port-group for [Ingress] and [ingress,egress] PolicyTypes
	// If NOT [egress] PolicyType
	if !(len(np.policyTypes) == 1 && np.policyTypes[0] == knet.PolicyTypeEgress) {
		for _, portInfo := range ports {
			if oc.lspIngressDenyCache[portInfo.name] > 0 {
				oc.lspIngressDenyCache[portInfo.name]--
				if oc.lspIngressDenyCache[portInfo.name] == 0 {
					ingressDenyPorts = append(ingressDenyPorts, portInfo.uuid)
					delete(oc.lspIngressDenyCache, portInfo.name)
				}
			}
		}
	}

	// Remove port from egress deny port group for [egress] and [ingress,egress] PolicyTypes
	// if [egress] PolicyType OR there are any egress rules OR [ingress,egress] PolicyType
	if (len(np.policyTypes) == 1 && np.policyTypes[0] == knet.PolicyTypeEgress) ||
		len(np.egressPolicies) > 0 || len(np.policyTypes) == 2 {
		for _, portInfo := range ports {
			if oc.lspEgressDenyCache[portInfo.name] > 0 {
				oc.lspEgressDenyCache[portInfo.name]--
				if oc.lspEgressDenyCache[portInfo.name] == 0 {
					egressDenyPorts = append(egressDenyPorts, portInfo.uuid)
					delete(oc.lspEgressDenyCache, portInfo.name)
				}
			}
		}
	}
	oc.lspMutex.Unlock()

	return
}

func (oc *Controller) processLocalPodSelectorSetPods(policy *knet.NetworkPolicy, np *networkPolicy, nsInfo *namespaceInfo, objs ...interface{}) (policyPorts, ingressDenyPorts, egressDenyPorts []string) {
	klog.Infof("Processing NetworkPolicy %s/%s to have %d local pods...", np.namespace, np.name, len(objs))

	// get list of pods and their logical ports to add
	// theoretically this should never filter any pods but it's always good to be
	// paranoid.
	policyPorts = make([]string, 0, len(objs))
	policyPortsInfo := make([]*lpInfo, 0, len(objs))
	for _, obj := range objs {
		pod := obj.(*kapi.Pod)

		if pod.Spec.NodeName == "" {
			continue
		}

		// Get the logical port info
		logicalPort := util.GetLogicalPortName(pod.Namespace, pod.Name)
		portInfo, err := oc.logicalPortCache.get(logicalPort)
		// pod is not yet handled
		// no big deal, we'll get the update when it is.
		if err != nil {
			continue
		}

		// this pod is somehow already added to this policy, then skip
		if _, ok := np.localPods.LoadOrStore(portInfo.name, portInfo); ok {
			continue
		}

		policyPortsInfo = append(policyPortsInfo, portInfo)
		policyPorts = append(policyPorts, portInfo.uuid)
	}

	ingressDenyPorts, egressDenyPorts = oc.localPodAddDefaultDeny(nsInfo, policy, policyPortsInfo...)

	return
}

func (oc *Controller) processLocalPodSelectorDelPods(policy *knet.NetworkPolicy, np *networkPolicy, nsInfo *namespaceInfo, objs ...interface{}) (policyPorts, ingressDenyPorts, egressDenyPorts []string) {
	klog.Infof("Processing NetworkPolicy %s/%s to delete %d local pods...", np.namespace, np.name, len(objs))

	policyPorts = make([]string, 0, len(objs))
	policyPortsInfo := make([]*lpInfo, 0, len(objs))
	for _, obj := range objs {
		pod := obj.(*kapi.Pod)

		if pod.Spec.NodeName == "" {
			continue
		}

		logicalPort := util.GetLogicalPortName(pod.Namespace, pod.Name)
		portInfo, err := oc.logicalPortCache.get(logicalPort)
		if err != nil {
			klog.Errorf(err.Error())
			return
		}

		// If we never saw this pod, short-circuit
		if _, ok := np.localPods.LoadAndDelete(logicalPort); !ok {
			continue
		}

		policyPortsInfo = append(policyPortsInfo, portInfo)
		policyPorts = append(policyPorts, portInfo.uuid)
	}

	ingressDenyPorts, egressDenyPorts = oc.localPodDelDefaultDeny(np, nsInfo, policyPortsInfo...)

	return
}

// handleLocalPodSelectorAddFunc adds a new pod to an existing NetworkPolicy
func (oc *Controller) handleLocalPodSelectorAddFunc(policy *knet.NetworkPolicy, np *networkPolicy, nsInfo *namespaceInfo, obj interface{}) {
	np.RLock()
	defer np.RUnlock()
	if np.deleted {
		return
	}

	policyPorts, ingressDenyPorts, egressDenyPorts := oc.processLocalPodSelectorSetPods(policy, np, nsInfo, obj)

	ops, err := libovsdbops.AddPortsToPortGroupOps(oc.nbClient, nil, nsInfo.portGroupIngressDenyName, ingressDenyPorts...)
	if err != nil {
		oc.processLocalPodSelectorDelPods(policy, np, nsInfo, obj)
		klog.Errorf(err.Error())
	}

	ops, err = libovsdbops.AddPortsToPortGroupOps(oc.nbClient, ops, nsInfo.portGroupEgressDenyName, egressDenyPorts...)
	if err != nil {
		oc.processLocalPodSelectorDelPods(policy, np, nsInfo, obj)
		klog.Errorf(err.Error())
	}

	ops, err = libovsdbops.AddPortsToPortGroupOps(oc.nbClient, ops, np.portGroupName, policyPorts...)
	if err != nil {
		oc.processLocalPodSelectorDelPods(policy, np, nsInfo, obj)
		klog.Errorf(err.Error())
	}

	_, err = libovsdbops.TransactAndCheck(oc.nbClient, ops)
	if err != nil {
		oc.processLocalPodSelectorDelPods(policy, np, nsInfo, obj)
		klog.Errorf(err.Error())
		return
	}
}

func (oc *Controller) handleLocalPodSelectorDelFunc(policy *knet.NetworkPolicy, np *networkPolicy, nsInfo *namespaceInfo, obj interface{}) {
	np.RLock()
	defer np.RUnlock()
	if np.deleted {
		return
	}

	policyPorts, ingressDenyPorts, egressDenyPorts := oc.processLocalPodSelectorDelPods(policy, np, nsInfo, obj)

	ops, err := libovsdbops.DeletePortsFromPortGroupOps(oc.nbClient, nil, nsInfo.portGroupIngressDenyName, ingressDenyPorts...)
	if err != nil {
		oc.processLocalPodSelectorSetPods(policy, np, nsInfo, obj)
		klog.Errorf(err.Error())
		return
	}

	ops, err = libovsdbops.DeletePortsFromPortGroupOps(oc.nbClient, ops, nsInfo.portGroupEgressDenyName, egressDenyPorts...)
	if err != nil {
		oc.processLocalPodSelectorSetPods(policy, np, nsInfo, obj)
		klog.Errorf(err.Error())
		return
	}

	ops, err = libovsdbops.DeletePortsFromPortGroupOps(oc.nbClient, ops, np.portGroupName, policyPorts...)
	if err != nil {
		oc.processLocalPodSelectorSetPods(policy, np, nsInfo, obj)
		klog.Errorf(err.Error())
	}

	_, err = libovsdbops.TransactAndCheck(oc.nbClient, ops)
	if err != nil {
		oc.processLocalPodSelectorSetPods(policy, np, nsInfo, obj)
		klog.Errorf(err.Error())
		return
	}
}

func (oc *Controller) handleLocalPodSelector(
	policy *knet.NetworkPolicy, np *networkPolicy, nsInfo *namespaceInfo,
	handleInitialItems func([]interface{})) {

	// NetworkPolicy is validated by the apiserver; this can't fail.
	sel, _ := metav1.LabelSelectorAsSelector(&policy.Spec.PodSelector)

	h := oc.watchFactory.AddFilteredPodHandler(policy.Namespace, sel,
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				oc.handleLocalPodSelectorAddFunc(policy, np, nsInfo, obj)
			},
			DeleteFunc: func(obj interface{}) {
				oc.handleLocalPodSelectorDelFunc(policy, np, nsInfo, obj)
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				oc.handleLocalPodSelectorAddFunc(policy, np, nsInfo, newObj)
			},
		}, func(objs []interface{}) {
			handleInitialItems(objs)
		})

	np.podHandlerList = append(np.podHandlerList, h)
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

// addNetworkPolicy creates and applies OVN ACLs to pod logical switch
// ports from Kubernetes NetworkPolicy objects using OVN Port Groups
func (oc *Controller) addNetworkPolicy(policy *knet.NetworkPolicy) {
	klog.Infof("Adding network policy %s in namespace %s", policy.Name,
		policy.Namespace)

	nsInfo, nsUnlock, err := oc.waitForNamespaceLocked(policy.Namespace, false)
	if err != nil {
		klog.Errorf("Failed to wait for namespace %s event (%v)",
			policy.Namespace, err)
		return
	}
	_, alreadyExists := nsInfo.networkPolicies[policy.Name]
	if alreadyExists {
		nsUnlock()
		return
	}

	np := NewNetworkPolicy(policy)

	if len(nsInfo.networkPolicies) == 0 {
		err = oc.createDefaultDenyPGAndACLs(policy.Namespace, policy.Name, nsInfo)
		if err != nil {
			klog.Errorf(err.Error())
			return
		}
	}

	nsInfo.networkPolicies[policy.Name] = np

	nsUnlock()
	np.Lock()

	if nsInfo.aclLogging.Deny != "" || nsInfo.aclLogging.Allow != "" {
		klog.Infof("ACL logging for network policy %s in namespace %s set to deny=%s, allow=%s",
			policy.Name, policy.Namespace, nsInfo.aclLogging.Deny, nsInfo.aclLogging.Allow)
	}

	type policyHandler struct {
		gress             *gressPolicy
		namespaceSelector *metav1.LabelSelector
		podSelector       *metav1.LabelSelector
	}
	var policyHandlers []policyHandler
	// Go through each ingress rule.  For each ingress rule, create an
	// addressSet for the peer pods.
	for i, ingressJSON := range policy.Spec.Ingress {
		klog.V(5).Infof("Network policy ingress is %+v", ingressJSON)

		ingress := newGressPolicy(knet.PolicyTypeIngress, i, policy.Namespace, policy.Name)

		// Each ingress rule can have multiple ports to which we allow traffic.
		for _, portJSON := range ingressJSON.Ports {
			ingress.addPortPolicy(&portJSON)
		}

		if hasAnyLabelSelector(ingressJSON.From) {
			klog.V(5).Infof("Network policy %s with ingress rule %s has a selector", policy.Name, ingress.policyName)
			if err := ingress.ensurePeerAddressSet(oc.addressSetFactory); err != nil {
				klog.Errorf(err.Error())
				continue
			}
			// Start service handlers ONLY if there's an ingress Address Set
			oc.handlePeerService(policy, ingress, np)
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
		np.ingressPolicies = append(np.ingressPolicies, ingress)
	}

	// Go through each egress rule.  For each egress rule, create an
	// addressSet for the peer pods.
	for i, egressJSON := range policy.Spec.Egress {
		klog.V(5).Infof("Network policy egress is %+v", egressJSON)

		egress := newGressPolicy(knet.PolicyTypeEgress, i, policy.Namespace, policy.Name)

		// Each egress rule can have multiple ports to which we allow traffic.
		for _, portJSON := range egressJSON.Ports {
			egress.addPortPolicy(&portJSON)
		}

		if hasAnyLabelSelector(egressJSON.To) {
			klog.V(5).Infof("Network policy %s with egress rule %s has a selector", policy.Name, egress.policyName)
			if err := egress.ensurePeerAddressSet(oc.addressSetFactory); err != nil {
				klog.Errorf(err.Error())
				continue
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
		np.egressPolicies = append(np.egressPolicies, egress)
	}
	np.Unlock()

	for _, handler := range policyHandlers {
		if handler.namespaceSelector != nil && handler.podSelector != nil {
			// For each rule that contains both peer namespace selector and
			// peer pod selector, we create a watcher for each matching namespace
			// that populates the addressSet
			oc.handlePeerNamespaceAndPodSelector(policy,
				handler.namespaceSelector, handler.podSelector,
				handler.gress, np)
		} else if handler.namespaceSelector != nil {
			// For each peer namespace selector, we create a watcher that
			// populates ingress.peerAddressSets
			oc.handlePeerNamespaceSelector(policy,
				handler.namespaceSelector, handler.gress, np)
		} else if handler.podSelector != nil {
			// For each peer pod selector, we create a watcher that
			// populates the addressSet
			oc.handlePeerPodSelector(policy, handler.podSelector,
				handler.gress, np)
		}
	}

	readableGroupName := fmt.Sprintf("%s_%s", policy.Namespace, policy.Name)
	np.portGroupName = hashedPortGroup(readableGroupName)
	ops := []ovsdb.Operation{}

	// Build policy ACLs
	acls := oc.buildNetworkPolicyACLs(np, nsInfo.aclLogging.Allow)
	ops, err = libovsdbops.CreateOrUpdateACLsOps(oc.nbClient, ops, acls...)
	if err != nil {
		klog.Errorf(err.Error())
		return
	}

	// Build a port group for the policy. All the pods that this policy
	// selects will be eventually added to this port group.
	pg := libovsdbops.BuildPortGroup(np.portGroupName, readableGroupName, nil, acls)

	// Add a handler to update the policy and deny port groups with the pods
	// this policy applies to.
	// Handle initial items locally to minimize DB ops.
	var selectedPods []interface{}
	handleInitialSelectedPods := func(objs []interface{}) {
		selectedPods = objs
		policyPorts, ingressDenyPorts, egressDenyPorts := oc.processLocalPodSelectorSetPods(policy, np, nsInfo, selectedPods...)
		pg.Ports = append(pg.Ports, policyPorts...)
		ops, err = libovsdbops.AddPortsToPortGroupOps(oc.nbClient, ops, nsInfo.portGroupIngressDenyName, ingressDenyPorts...)
		if err != nil {
			oc.processLocalPodSelectorDelPods(policy, np, nsInfo, selectedPods...)
			klog.Errorf(err.Error())
		}
		ops, err = libovsdbops.AddPortsToPortGroupOps(oc.nbClient, ops, nsInfo.portGroupEgressDenyName, egressDenyPorts...)
		if err != nil {
			oc.processLocalPodSelectorDelPods(policy, np, nsInfo, selectedPods...)
			klog.Errorf(err.Error())
		}
	}
	oc.handleLocalPodSelector(policy, np, nsInfo, handleInitialSelectedPods)

	np.Lock()
	defer np.Unlock()
	if np.deleted {
		oc.processLocalPodSelectorDelPods(policy, np, nsInfo, selectedPods...)
		return
	}

	ops, err = libovsdbops.CreateOrUpdatePortGroupsOps(oc.nbClient, ops, pg)
	if err != nil {
		oc.processLocalPodSelectorDelPods(policy, np, nsInfo, selectedPods...)
		klog.Errorf(err.Error())
		return
	}

	_, err = libovsdbops.TransactAndCheck(oc.nbClient, ops)
	if err != nil {
		oc.processLocalPodSelectorDelPods(policy, np, nsInfo, selectedPods...)
		klog.Errorf(err.Error())
		return
	}
}

// buildNetworkPolicyACLs builds the ACLS associated with the 'gress policies
// of the provided network policy.
func (oc *Controller) buildNetworkPolicyACLs(np *networkPolicy, aclLogging string) []*nbdb.ACL {
	acls := []*nbdb.ACL{}
	for _, gp := range np.ingressPolicies {
		acl := gp.buildLocalPodACLs(np.portGroupName, aclLogging)
		acls = append(acls, acl...)
	}
	for _, gp := range np.egressPolicies {
		acl := gp.buildLocalPodACLs(np.portGroupName, aclLogging)
		acls = append(acls, acl...)
	}

	return acls
}

func (oc *Controller) deleteNetworkPolicy(policy *knet.NetworkPolicy) {
	klog.Infof("Deleting network policy %s in namespace %s",
		policy.Name, policy.Namespace)

	nsInfo, nsUnlock := oc.getNamespaceLocked(policy.Namespace, false)
	if nsInfo == nil {
		klog.V(5).Infof("Failed to get namespace lock when deleting policy %s in namespace %s",
			policy.Name, policy.Namespace)
		return
	}
	defer nsUnlock()

	np := nsInfo.networkPolicies[policy.Name]
	if np == nil {
		return
	}

	delete(nsInfo.networkPolicies, policy.Name)

	oc.destroyNetworkPolicy(np, nsInfo)
}

func (oc *Controller) destroyNetworkPolicy(np *networkPolicy, nsInfo *namespaceInfo) {
	np.Lock()
	defer np.Unlock()
	np.deleted = true
	oc.shutdownHandlers(np)

	ports := []*lpInfo{}
	np.localPods.Range(func(_, value interface{}) bool {
		portInfo := value.(*lpInfo)
		ports = append(ports, portInfo)
		return true
	})

	ingressDenyPorts, egressDenyPorts := oc.localPodDelDefaultDeny(np, nsInfo, ports...)

	ops := []ovsdb.Operation{}
	var err error
	if len(nsInfo.networkPolicies) == 0 {
		ops, err = libovsdbops.DeletePortGroupsOps(oc.nbClient, ops, nsInfo.portGroupIngressDenyName)
		if err != nil {
			klog.Errorf("%v", err)
		}
		ops, err = libovsdbops.DeletePortGroupsOps(oc.nbClient, ops, nsInfo.portGroupEgressDenyName)
		if err != nil {
			klog.Errorf("%v", err)
		}
	} else {
		ops, err = libovsdbops.DeletePortsFromPortGroupOps(oc.nbClient, ops, nsInfo.portGroupIngressDenyName, ingressDenyPorts...)
		if err != nil {
			klog.Errorf(err.Error())
		}
		ops, err = libovsdbops.DeletePortsFromPortGroupOps(oc.nbClient, ops, nsInfo.portGroupEgressDenyName, egressDenyPorts...)
		if err != nil {
			klog.Errorf(err.Error())
		}
	}

	// Delete the port group
	ops, err = libovsdbops.DeletePortGroupsOps(oc.nbClient, ops, np.portGroupName)
	if err != nil {
		klog.Errorf("%v", err)
	}

	_, err = libovsdbops.TransactAndCheck(oc.nbClient, ops)
	if err != nil {
		klog.Errorf(err.Error())
		return
	}

	// Delete ingress/egress address sets
	for _, policy := range np.ingressPolicies {
		if err := policy.destroy(); err != nil {
			klog.Errorf(err.Error())
		}
	}
	for _, policy := range np.egressPolicies {
		if err := policy.destroy(); err != nil {
			klog.Errorf(err.Error())
		}
	}
}

// handlePeerPodSelectorAddUpdate adds the IP address of a pod that has been
// selected as a peer by a NetworkPolicy's ingress/egress section to that
// ingress/egress address set
func (oc *Controller) handlePeerPodSelectorAddUpdate(gp *gressPolicy, objs ...interface{}) {
	pods := make([]*kapi.Pod, 0, len(objs))
	for _, obj := range objs {
		pod := obj.(*kapi.Pod)
		if pod.Spec.NodeName == "" {
			continue
		}
		pods = append(pods, pod)
	}
	if err := gp.addPeerPods(pods...); err != nil {
		klog.Errorf(err.Error())
	}
}

// handlePeerPodSelectorDelete removes the IP address of a pod that no longer
// matches a NetworkPolicy ingress/egress section's selectors from that
// ingress/egress address set
func (oc *Controller) handlePeerPodSelectorDelete(gp *gressPolicy, obj interface{}) {
	pod := obj.(*kapi.Pod)
	if pod.Spec.NodeName == "" {
		return
	}
	if err := gp.deletePeerPod(pod); err != nil {
		klog.Errorf(err.Error())
	}
}

// handlePeerServiceSelectorAddUpdate adds the VIP of a service that selects
// pods that are selected by the Network Policy
func (oc *Controller) handlePeerServiceAdd(gp *gressPolicy, obj interface{}) {
	service := obj.(*kapi.Service)
	klog.V(5).Infof("A Service: %s matches the namespace as the gress policy: %s", service.Name, gp.policyName)
	if err := gp.addPeerSvcVip(service); err != nil {
		klog.Errorf(err.Error())
	}
}

// handlePeerServiceDelete removes the VIP of a service that selects
// pods that are selected by the Network Policy
func (oc *Controller) handlePeerServiceDelete(gp *gressPolicy, obj interface{}) {
	service := obj.(*kapi.Service)
	if err := gp.deletePeerSvcVip(service); err != nil {
		klog.Errorf(err.Error())
	}
}

// Watch Services that are in the same Namespace as the NP
// To account for hairpined traffic
func (oc *Controller) handlePeerService(
	policy *knet.NetworkPolicy, gp *gressPolicy, np *networkPolicy) {

	h := oc.watchFactory.AddFilteredServiceHandler(policy.Namespace,
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				// Service is matched so add VIP to addressSet
				oc.handlePeerServiceAdd(gp, obj)
			},
			DeleteFunc: func(obj interface{}) {
				// If Service that has matched pods are deleted remove VIP
				oc.handlePeerServiceDelete(gp, obj)
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				// If Service Is updated make sure same pods are still matched
				oldSvc := oldObj.(*kapi.Service)
				newSvc := newObj.(*kapi.Service)
				if reflect.DeepEqual(newSvc.Spec.ExternalIPs, oldSvc.Spec.ExternalIPs) &&
					reflect.DeepEqual(newSvc.Spec.ClusterIP, oldSvc.Spec.ClusterIP) &&
					reflect.DeepEqual(newSvc.Spec.Type, oldSvc.Spec.Type) &&
					reflect.DeepEqual(newSvc.Status.LoadBalancer.Ingress, oldSvc.Status.LoadBalancer.Ingress) {

					klog.V(5).Infof("Skipping service update for: %s as change does not apply to any of .Spec.Ports, "+
						".Spec.ExternalIP, .Spec.ClusterIP, .Spec.Type, .Status.LoadBalancer.Ingress", newSvc.Name)
					return
				}

				oc.handlePeerServiceDelete(gp, oldObj)
				oc.handlePeerServiceAdd(gp, newObj)
			},
		}, nil)
	np.svcHandlerList = append(np.svcHandlerList, h)
}

func (oc *Controller) handlePeerPodSelector(
	policy *knet.NetworkPolicy, podSelector *metav1.LabelSelector,
	gp *gressPolicy, np *networkPolicy) {

	// NetworkPolicy is validated by the apiserver; this can't fail.
	sel, _ := metav1.LabelSelectorAsSelector(podSelector)

	h := oc.watchFactory.AddFilteredPodHandler(policy.Namespace, sel,
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				oc.handlePeerPodSelectorAddUpdate(gp, obj)
			},
			DeleteFunc: func(obj interface{}) {
				oc.handlePeerPodSelectorDelete(gp, obj)
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				oc.handlePeerPodSelectorAddUpdate(gp, newObj)
			},
		}, func(objs []interface{}) {
			oc.handlePeerPodSelectorAddUpdate(gp, objs...)
		})
	np.podHandlerList = append(np.podHandlerList, h)
}

func (oc *Controller) handlePeerNamespaceAndPodSelector(
	policy *knet.NetworkPolicy,
	namespaceSelector *metav1.LabelSelector,
	podSelector *metav1.LabelSelector,
	gp *gressPolicy,
	np *networkPolicy) {

	// NetworkPolicy is validated by the apiserver; this can't fail.
	nsSel, _ := metav1.LabelSelectorAsSelector(namespaceSelector)
	podSel, _ := metav1.LabelSelectorAsSelector(podSelector)

	namespaceHandler := oc.watchFactory.AddFilteredNamespaceHandler("", nsSel,
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				namespace := obj.(*kapi.Namespace)
				np.RLock()
				alreadyDeleted := np.deleted
				np.RUnlock()
				if alreadyDeleted {
					return
				}

				// The AddFilteredPodHandler call might call handlePeerPodSelectorAddUpdate
				// on existing pods so we can't be holding the lock at this point
				podHandler := oc.watchFactory.AddFilteredPodHandler(namespace.Name, podSel,
					cache.ResourceEventHandlerFuncs{
						AddFunc: func(obj interface{}) {
							oc.handlePeerPodSelectorAddUpdate(gp, obj)
						},
						DeleteFunc: func(obj interface{}) {
							oc.handlePeerPodSelectorDelete(gp, obj)
						},
						UpdateFunc: func(oldObj, newObj interface{}) {
							oc.handlePeerPodSelectorAddUpdate(gp, newObj)
						},
					}, func(objs []interface{}) {
						oc.handlePeerPodSelectorAddUpdate(gp, objs...)
					})
				np.Lock()
				defer np.Unlock()
				if np.deleted {
					oc.watchFactory.RemovePodHandler(podHandler)
					return
				}
				np.podHandlerList = append(np.podHandlerList, podHandler)
			},
			DeleteFunc: func(obj interface{}) {
				// when the namespace labels no longer apply
				// remove the namespaces pods from the address_set
				namespace := obj.(*kapi.Namespace)
				pods, _ := oc.watchFactory.GetPods(namespace.Name)

				for _, pod := range pods {
					oc.handlePeerPodSelectorDelete(gp, pod)
				}

			},
			UpdateFunc: func(oldObj, newObj interface{}) {
			},
		}, nil)
	np.nsHandlerList = append(np.nsHandlerList, namespaceHandler)
}

func (oc *Controller) handlePeerNamespaceSelectorOnUpdate(np *networkPolicy, gp *gressPolicy, doUpdate func() bool) {
	aclLoggingLevels := oc.GetNetworkPolicyACLLogging(np.namespace)
	np.Lock()
	defer np.Unlock()
	// This needs to be a write lock because there's no locking around 'gress policies
	if !np.deleted && doUpdate() {
		acls := gp.buildLocalPodACLs(np.portGroupName, aclLoggingLevels.Allow)
		ops, err := libovsdbops.CreateOrUpdateACLsOps(oc.nbClient, nil, acls...)
		if err != nil {
			klog.Errorf(err.Error())
		}
		ops, err = libovsdbops.AddACLsToPortGroupOps(oc.nbClient, ops, np.portGroupName, acls...)
		if err != nil {
			klog.Errorf(err.Error())
		}
		_, err = libovsdbops.TransactAndCheck(oc.nbClient, ops)
		if err != nil {
			klog.Errorf(err.Error())
		}
	}
}

func (oc *Controller) handlePeerNamespaceSelector(
	policy *knet.NetworkPolicy,
	namespaceSelector *metav1.LabelSelector,
	gress *gressPolicy, np *networkPolicy) {

	// NetworkPolicy is validated by the apiserver; this can't fail.
	sel, _ := metav1.LabelSelectorAsSelector(namespaceSelector)

	h := oc.watchFactory.AddFilteredNamespaceHandler("", sel,
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				namespace := obj.(*kapi.Namespace)
				// Update the ACL ...
				oc.handlePeerNamespaceSelectorOnUpdate(np, gress, func() bool {
					// ... on condition that the added address set was not already in the 'gress policy
					return gress.addNamespaceAddressSet(namespace.Name)
				})
			},
			DeleteFunc: func(obj interface{}) {
				namespace := obj.(*kapi.Namespace)
				// Update the ACL ...
				oc.handlePeerNamespaceSelectorOnUpdate(np, gress, func() bool {
					// ... on condition that the removed address set was in the 'gress policy
					return gress.delNamespaceAddressSet(namespace.Name)
				})
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
			},
		}, func(i []interface{}) {
			// This needs to be a write lock because there's no locking around 'gress policies
			np.Lock()
			defer np.Unlock()
			// We load the existing address set into the 'gress policy.
			// Notice that this will make the AddFunc for this initial
			// address set a noop.
			// The ACL must be set explicitly after setting up this handler
			// for the address set to be considered.
			gress.addNamespaceAddressSets(i)
		})
	np.nsHandlerList = append(np.nsHandlerList, h)
}

func (oc *Controller) shutdownHandlers(np *networkPolicy) {
	for _, handler := range np.podHandlerList {
		oc.watchFactory.RemovePodHandler(handler)
	}
	for _, handler := range np.nsHandlerList {
		oc.watchFactory.RemoveNamespaceHandler(handler)
	}
	for _, handler := range np.svcHandlerList {
		oc.watchFactory.RemoveServiceHandler(handler)
	}
}
