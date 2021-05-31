package ovn

import (
	"fmt"
	"net"
	"reflect"
	"strconv"
	"strings"
	"sync"

	goovn "github.com/ebay/go-ovn"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	kapi "k8s.io/api/core/v1"
	knet "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
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

	portGroupUUID string //uuid for OVN port_group
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

	err := oc.addressSetFactory.ProcessEachAddressSet(func(addrSetName, namespaceName, policyName string) {
		if policyName != "" && !expectedPolicies[namespaceName][policyName] {
			// policy doesn't exist on k8s. Delete the port group
			portGroupName := fmt.Sprintf("%s_%s", namespaceName, policyName)
			hashedLocalPortGroup := hashedPortGroup(portGroupName)
			err := deletePortGroup(oc.ovnNBClient, hashedLocalPortGroup)
			if err != nil {
				klog.Errorf("%v", err)
			}

			// delete the address sets for this old policy from OVN
			if err := oc.addressSetFactory.DestroyAddressSetInBackingStore(addrSetName); err != nil {
				klog.Errorf(err.Error())
			}
		}
	})
	if err != nil {
		klog.Errorf("Error in syncing network policies: %v", err)
	}
}

func addAllowACLFromNode(logicalSwitch string, mgmtPortIP net.IP, ovnNBClient goovn.Client) error {
	ipFamily := "ip4"
	if utilnet.IsIPv6(mgmtPortIP) {
		ipFamily = "ip6"
	}
	match := fmt.Sprintf("%s.src==%s", ipFamily, mgmtPortIP.String())

	priority, _ := strconv.Atoi(types.DefaultAllowPriority)
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

	return "match=\"" + aclMatch + "\""
}

func getACLPortGroupUUID(match, action string, policyType knet.PolicyType) (string, error) {
	uuid, stderr, err := util.RunOVNNbctl("--data=bare", "--no-heading",
		"--columns=_uuid", "find", "ACL", match, "action="+action,
		fmt.Sprintf("external-ids:default-deny-policy-type=%s", policyType))
	if err != nil {
		return "", fmt.Errorf("find failed to get the default deny rule for "+
			"policy type %s stderr: %q (%v)", policyType, stderr, err)
	}

	return uuid, nil
}

func addACLPortGroup(policyNamespace, portGroupUUID, direction, priority, match, action string, policyType knet.PolicyType, aclLogging string, policyName string) error {
	uuid, err := getACLPortGroupUUID(match, action, policyType)
	if err != nil {
		return err
	}
	if uuid != "" {
		return nil
	}
	if policyNamespace == "" {
		policyNamespace = portGroupUUID
	}
	if policyName != "" {
		policyName = fmt.Sprintf("%s_%s", policyNamespace, policyName)
	} else {
		policyName = policyNamespace
	}
	_, stderr, err := util.RunOVNNbctl("--id=@acl", "create", "acl",
		fmt.Sprintf("priority=%s", priority),
		fmt.Sprintf("direction=%s", direction), match, "action="+action,
		fmt.Sprintf("log=%t", aclLogging != ""), fmt.Sprintf("severity=%s", getACLLoggingSeverity(aclLogging)),
		fmt.Sprintf("meter=%s", types.OvnACLLoggingMeter),
		fmt.Sprintf("name=%.63s", policyName),
		fmt.Sprintf("external-ids:default-deny-policy-type=%s", policyType),
		"--", "add", "port_group", portGroupUUID,
		"acls", "@acl")
	if err != nil {
		return fmt.Errorf("error executing create ACL command for "+
			"policy type %s stderr: %q (%v)", policyType, stderr, err)
	}
	return nil
}

func deleteACLPortGroup(portGroupName, direction, priority, match, action string, policyType knet.PolicyType) error {
	match = getACLMatch(portGroupName, match, policyType)
	uuid, err := getACLPortGroupUUID(match, action, policyType)
	if err != nil {
		return err
	}

	if uuid == "" {
		return nil
	}

	_, stderr, err := util.RunOVNNbctl("remove", "port_group",
		portGroupName, "acls", uuid)
	if err != nil {
		return fmt.Errorf("remove failed to delete the rule for "+
			"port_group=%s, stderr: %q (%v)", portGroupName, stderr, err)
	}

	return nil
}

func addToPortGroup(ovnNBClient goovn.Client, portGroupName string, ports ...*lpInfo) error {
	cmds := make([]*goovn.OvnCommand, 0, len(ports))
	for _, portInfo := range ports {
		cmd, err := ovnNBClient.PortGroupAddPort(portGroupName, portInfo.uuid)
		if err != nil {
			return fmt.Errorf("error preparing adding port %v to port group %s (%v)", portInfo.name, portGroupName, err)
		}
		cmds = append(cmds, cmd)
	}

	if err := ovnNBClient.Execute(cmds...); err != nil {
		names := []string{}
		for _, portInfo := range ports {
			names = append(names, portInfo.name)
		}
		return fmt.Errorf("error committing adding ports (%v) to port group %s (%v)", strings.Join(names, ","), portGroupName, err)
	}
	return nil
}

func deleteFromPortGroup(ovnNBClient goovn.Client, portGroupName string, ports ...*lpInfo) error {
	cmds := make([]*goovn.OvnCommand, 0, len(ports))
	for _, portInfo := range ports {
		cmd, err := ovnNBClient.PortGroupRemovePort(portGroupName, portInfo.uuid)
		if err != nil {
			// PortGroup already deleted...
			if err == goovn.ErrorNotFound {
				return nil
			} else {
				return fmt.Errorf("error preparing removing port %v from port group %s (%v)", portInfo.name, portGroupName, err)
			}
		}
		cmds = append(cmds, cmd)
	}

	if err := ovnNBClient.Execute(cmds...); err != nil {
		names := []string{}
		for _, portInfo := range ports {
			names = append(names, portInfo.name)
		}
		return fmt.Errorf("error committing removing ports (%v) from port group %s (%v)", strings.Join(names, ","), portGroupName, err)
	}
	return nil
}

// setPortGroup declaratively sets a port group to have a specific set of ports.
func setPortGroup(ovnNBClient goovn.Client, portGroupName string, ports ...*lpInfo) error {

	uuids := make([]string, 0, len(ports))
	for _, port := range ports {
		uuids = append(uuids, port.uuid)
	}

	cmd, err := ovnNBClient.PortGroupUpdate(portGroupName, uuids, nil)
	if err != nil {
		return err
	}

	return ovnNBClient.Execute(cmd)
}

func defaultDenyPortGroup(namespace, gressSuffix string) string {
	return hashedPortGroup(namespace) + "_" + gressSuffix
}

func (oc *Controller) createDefaultDenyPortGroup(ns string, nsInfo *namespaceInfo, policyType knet.PolicyType, aclLogging string, policyName string) error {
	var portGroupName string
	if policyType == knet.PolicyTypeIngress {
		portGroupName = defaultDenyPortGroup(ns, "ingressDefaultDeny")
	} else if policyType == knet.PolicyTypeEgress {
		portGroupName = defaultDenyPortGroup(ns, "egressDefaultDeny")
	}
	portGroupUUID, err := createPortGroup(oc.ovnNBClient, portGroupName, portGroupName)
	if err != nil {
		return fmt.Errorf("failed to create port_group for %s (%v)",
			portGroupName, err)
	}
	match := getACLMatch(portGroupName, "", policyType)
	err = addACLPortGroup(ns, portGroupUUID, types.DirectionToLPort,
		types.DefaultDenyPriority, match, "drop", policyType, aclLogging, policyName)
	if err != nil {
		return fmt.Errorf("failed to create default deny ACL for port group %v", err)
	}

	match = getACLMatch(portGroupName, "arp", policyType)
	err = addACLPortGroup(ns, portGroupUUID, types.DirectionToLPort,
		types.DefaultAllowPriority, match, "allow", policyType, "", "ARPallowPolicy")
	if err != nil {
		return fmt.Errorf("failed to create default allow ARP ACL for port group %v", err)
	}

	if policyType == knet.PolicyTypeIngress {
		nsInfo.portGroupIngressDenyUUID = portGroupUUID
		nsInfo.portGroupIngressDenyName = portGroupName
	} else if policyType == knet.PolicyTypeEgress {
		nsInfo.portGroupEgressDenyUUID = portGroupUUID
		nsInfo.portGroupEgressDenyName = portGroupName
	}
	return nil
}

// modify ACL logging
func (oc *Controller) setACLDenyLogging(ns string, nsInfo *namespaceInfo, aclLogging string) error {

	aclLoggingSev := getACLLoggingSeverity(aclLogging)

	match := getACLMatch(defaultDenyPortGroup(ns, "ingressDefaultDeny"), "", knet.PolicyTypeIngress)
	uuid, err := getACLPortGroupUUID(match, "drop", knet.PolicyTypeIngress)
	if err != nil {
		return err
	}
	if uuid == "" {
		// not suppose to happen
		return fmt.Errorf("failed to find the ACL for pg=%s: %v", nsInfo.portGroupIngressDenyUUID, err)
	}
	if _, stderr, err := util.RunOVNNbctl("set", "acl", uuid,
		fmt.Sprintf("log=%t", aclLogging != ""), fmt.Sprintf("severity=%s", aclLoggingSev)); err != nil {
		return fmt.Errorf("failed to modify the pg=%s, stderr: %q (%v)", nsInfo.portGroupIngressDenyUUID, stderr, err)
	}

	match = getACLMatch(defaultDenyPortGroup(ns, "egressDefaultDeny"), "", knet.PolicyTypeEgress)
	uuid, err = getACLPortGroupUUID(match, "drop", knet.PolicyTypeEgress)
	if err != nil {
		return err
	}
	if uuid == "" {
		// not suppose to happen
		return fmt.Errorf("failed to find the ACL for pg=%s: %v", nsInfo.portGroupEgressDenyUUID, err)
	}
	if _, stderr, err := util.RunOVNNbctl("set", "acl", uuid,
		fmt.Sprintf("log=%t", aclLogging != ""), fmt.Sprintf("severity=%s", aclLoggingSev)); err != nil {
		return fmt.Errorf("failed to modify the pg=%s, stderr: %q (%v)", nsInfo.portGroupEgressDenyUUID, stderr, err)
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
	err := nsInfo.updateNamespacePortGroup(oc.ovnNBClient, ns)
	if err != nil {
		return err
	}

	portGroupName := hashedPortGroup(ns)
	match := getACLMatch(portGroupName, getMulticastACLEgrMatch(),
		knet.PolicyTypeEgress)
	err = addACLPortGroup(ns, nsInfo.portGroupUUID, types.DirectionFromLPort,
		types.DefaultMcastAllowPriority, match, "allow", knet.PolicyTypeEgress, "", "MulticastAllowEgress")
	if err != nil {
		return fmt.Errorf("failed to create allow egress multicast ACL for %s (%v)",
			ns, err)
	}

	match = getACLMatch(portGroupName, getMulticastACLIgrMatch(nsInfo),
		knet.PolicyTypeIngress)
	err = addACLPortGroup(ns, nsInfo.portGroupUUID, types.DirectionToLPort,
		types.DefaultMcastAllowPriority, match, "allow", knet.PolicyTypeIngress, "", "MulticastAllowIngress")
	if err != nil {
		return fmt.Errorf("failed to create allow ingress multicast ACL for %s (%v)",
			ns, err)
	}

	// Add all ports from this namespace to the multicast allow group.
	pods, err := oc.watchFactory.GetPods(ns)
	if err != nil {
		klog.Warningf("Failed to get pods for namespace %q: %v", ns, err)
	}
	for _, pod := range pods {
		portName := podLogicalPortName(pod)
		if portInfo, err := oc.logicalPortCache.get(portName); err != nil {
			klog.Errorf(err.Error())
		} else if err := podAddAllowMulticastPolicy(oc.ovnNBClient, ns, portInfo); err != nil {
			klog.Warningf("Failed to add port %s to port group ACL: %v", portName, err)
		}
	}

	return nil
}

func deleteMulticastACLs(ns, portGroupHash string, nsInfo *namespaceInfo) error {
	err := deleteACLPortGroup(portGroupHash, types.DirectionFromLPort,
		types.DefaultMcastAllowPriority, getMulticastACLEgrMatch(), "allow",
		knet.PolicyTypeEgress)
	if err != nil {
		return fmt.Errorf("failed to delete allow egress multicast ACL for %s (%v)",
			ns, err)
	}

	err = deleteACLPortGroup(portGroupHash, types.DirectionToLPort, types.DefaultMcastAllowPriority,
		getMulticastACLIgrMatch(nsInfo), "allow", knet.PolicyTypeIngress)
	if err != nil {
		return fmt.Errorf("failed to delete allow ingress multicast ACL for %s (%v)",
			ns, err)
	}

	return nil
}

// Delete the policy to allow multicast traffic within 'ns'.
func deleteMulticastAllowPolicy(ovnNBClient goovn.Client, ns string, nsInfo *namespaceInfo) error {
	portGroupHash := hashedPortGroup(ns)

	err := deleteMulticastACLs(ns, portGroupHash, nsInfo)
	if err != nil {
		return err
	}

	_ = nsInfo.updateNamespacePortGroup(ovnNBClient, ns)
	return nil
}

// Creates a global default deny multicast policy:
// - one ACL dropping egress multicast traffic from all pods: this is to
//   protect OVN controller from processing IP multicast reports from nodes
//   that are not allowed to receive multicast raffic.
// - one ACL dropping ingress multicast traffic to all pods.
// Caller must hold the namespace's namespaceInfo object lock.
func (oc *Controller) createDefaultDenyMulticastPolicy() error {
	// By default deny any egress multicast traffic from any pod. This drops
	// IP multicast membership reports therefore denying any multicast traffic
	// to be forwarded to pods.
	match := "match=\"" + getMulticastACLMatch() + "\""
	err := addACLPortGroup("", oc.clusterPortGroupUUID, types.DirectionFromLPort,
		types.DefaultMcastDenyPriority, match, "drop", knet.PolicyTypeEgress, "", "DefaultDenyMulticastEgress")
	if err != nil {
		return fmt.Errorf("failed to create default deny multicast egress ACL: %v", err)
	}

	// By default deny any ingress multicast traffic to any pod.
	err = addACLPortGroup("", oc.clusterPortGroupUUID, types.DirectionToLPort,
		types.DefaultMcastDenyPriority, match, "drop", knet.PolicyTypeIngress, "", "DefaultDenyMulticastIngress")
	if err != nil {
		return fmt.Errorf("failed to create default deny multicast ingress ACL: %v", err)
	}

	// Remove old multicastDefaultDeny port group now that all ports
	// have been added to the clusterPortGroup by WatchPods()
	err = deletePortGroup(oc.ovnNBClient, legacyMulticastDefaultDenyPortGroup)
	if err != nil {
		klog.Errorf("%v", err)
	}
	return nil
}

// Creates a global default allow multicast policy:
// - one ACL allowing multicast traffic from cluster router ports
// - one ACL allowing multicast traffic to cluster router ports.
// Caller must hold the namespace's namespaceInfo object lock.
func (oc *Controller) createDefaultAllowMulticastPolicy() error {
	mcastMatch := getMulticastACLMatch()
	match := getACLMatch(clusterRtrPortGroupName, mcastMatch, knet.PolicyTypeEgress)
	err := addACLPortGroup("", oc.clusterRtrPortGroupUUID, types.DirectionFromLPort,
		types.DefaultRoutedMcastAllowPriority, match, "allow", knet.PolicyTypeEgress, "", "DefaultAllowMulticastEgress")
	if err != nil {
		return fmt.Errorf("failed to create default allow multicast egress ACL: %v", err)
	}

	match = getACLMatch(clusterRtrPortGroupName, mcastMatch, knet.PolicyTypeIngress)
	err = addACLPortGroup("", oc.clusterRtrPortGroupUUID, types.DirectionToLPort,
		types.DefaultRoutedMcastAllowPriority, match, "allow", knet.PolicyTypeIngress, "", "DefaultAllowMulticastIngress")
	if err != nil {
		return fmt.Errorf("failed to create default allow multicast ingress ACL: %v", err)
	}
	return nil
}

// podAddAllowMulticastPolicy adds the pod's logical switch port to the namespace's
// multicast port group. Caller must hold the namespace's namespaceInfo object
// lock.
func podAddAllowMulticastPolicy(ovnNBClient goovn.Client, ns string, portInfo *lpInfo) error {
	return addToPortGroup(ovnNBClient, hashedPortGroup(ns), portInfo)
}

// podDeleteAllowMulticastPolicy removes the pod's logical switch port from the
// namespace's multicast port group. Caller must hold the namespace's
// namespaceInfo object lock.
func podDeleteAllowMulticastPolicy(ovnNBClient goovn.Client, ns string, portInfo *lpInfo) error {
	return deleteFromPortGroup(ovnNBClient, hashedPortGroup(ns), portInfo)
}

// localPodAddDefaultDeny ensures ports (i.e. pods) are in the correct
// default-deny portgroups. Whether or not pods are in default-deny depends
// on whether or not any policies select this pod, so there is a reference
// count to ensure we don't accidentally open up a pod.
func (oc *Controller) localPodAddDefaultDeny(nsInfo *namespaceInfo,
	policy *knet.NetworkPolicy, ports ...*lpInfo) {
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

	addIngressPorts := []*lpInfo{}
	addEgressPorts := []*lpInfo{}

	// Handle condition 1 above.
	if !(len(policy.Spec.PolicyTypes) == 1 && policy.Spec.PolicyTypes[0] == knet.PolicyTypeEgress) {
		for _, portInfo := range ports {
			// if this is the first NP referencing this pod, then we
			// need to add it to the port group.
			if oc.lspIngressDenyCache[portInfo.name] == 0 {
				addIngressPorts = append(addIngressPorts, portInfo)
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
				addEgressPorts = append(addEgressPorts, portInfo)
			}

			// bump reference count
			oc.lspEgressDenyCache[portInfo.name]++
		}
	}

	// we're done with the lsp cache - release the lock before transacting
	oc.lspMutex.Unlock()

	// Generate a single OVN transaction that adds all ports to the
	// appropriate port groups.
	commands := make([]*goovn.OvnCommand, 0, len(addIngressPorts)+len(addEgressPorts))

	for _, portInfo := range addIngressPorts {
		cmd, err := oc.ovnNBClient.PortGroupAddPort(nsInfo.portGroupIngressDenyName, portInfo.uuid)
		if err != nil {
			klog.Warningf("Failed to create command: add port %s to ingress deny portgroup %s: %v",
				portInfo.name, nsInfo.portGroupIngressDenyName, err)
			continue
		}
		commands = append(commands, cmd)
	}

	for _, portInfo := range addEgressPorts {
		cmd, err := oc.ovnNBClient.PortGroupAddPort(nsInfo.portGroupEgressDenyName, portInfo.uuid)
		if err != nil {
			klog.Warningf("Failed to create command: add port %s to egress deny portgroup %s: %v",
				portInfo.name, nsInfo.portGroupEgressDenyName, err)
			continue
		}
		commands = append(commands, cmd)
	}

	err := oc.ovnNBClient.Execute(commands...)
	if err != nil {
		klog.Warningf("Failed to execute add-to-default-deny-portgroup transaction: %v", err)
	}
}

// localPodDelDefaultDeny decrements a pod's policy reference count and removes a pod
// from the default-deny portgroups if the reference count for the pod is 0
func (oc *Controller) localPodDelDefaultDeny(
	np *networkPolicy, nsInfo *namespaceInfo, ports ...*lpInfo) {
	oc.lspMutex.Lock()

	delIngressPorts := []*lpInfo{}
	delEgressPorts := []*lpInfo{}

	// Remove port from ingress deny port-group for [Ingress] and [ingress,egress] PolicyTypes
	// If NOT [egress] PolicyType
	if !(len(np.policyTypes) == 1 && np.policyTypes[0] == knet.PolicyTypeEgress) {
		for _, portInfo := range ports {
			if oc.lspIngressDenyCache[portInfo.name] > 0 {
				oc.lspIngressDenyCache[portInfo.name]--
				if oc.lspIngressDenyCache[portInfo.name] == 0 {
					delIngressPorts = append(delIngressPorts, portInfo)
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
					delEgressPorts = append(delEgressPorts, portInfo)
					delete(oc.lspEgressDenyCache, portInfo.name)
				}
			}
		}
	}
	oc.lspMutex.Unlock()

	commands := make([]*goovn.OvnCommand, 0, len(delIngressPorts)+len(delEgressPorts))

	for _, portInfo := range delIngressPorts {
		cmd, err := oc.ovnNBClient.PortGroupRemovePort(nsInfo.portGroupIngressDenyName, portInfo.uuid)
		if err != nil {
			klog.Warningf("Failed to create command: remove port %s from ingress deny portgroup %s: %v",
				portInfo.name, nsInfo.portGroupIngressDenyName, err)
			continue
		}
		commands = append(commands, cmd)
	}

	for _, portInfo := range delEgressPorts {
		cmd, err := oc.ovnNBClient.PortGroupRemovePort(nsInfo.portGroupEgressDenyName, portInfo.uuid)
		if err != nil {
			klog.Warningf("Failed to create command: remove port %s from egress deny portgroup %s: %v",
				portInfo.name, nsInfo.portGroupEgressDenyName, err)
			continue
		}
		commands = append(commands, cmd)
	}

	err := oc.ovnNBClient.Execute(commands...)
	if err != nil {
		klog.Warningf("Failed to execute add-to-default-deny-portgroup transaction: %v", err)
	}
}

// handleLocalPodSelectorAddFunc adds a new pod to an existing NetworkPolicy
//
// THIS MUST BE KEPT CONSISTENT WITH handleLocalPodSelectorSetPods!
func (oc *Controller) handleLocalPodSelectorAddFunc(
	policy *knet.NetworkPolicy, np *networkPolicy, nsInfo *namespaceInfo,
	obj interface{}) {
	pod := obj.(*kapi.Pod)

	if pod.Spec.NodeName == "" {
		return
	}

	// Get the logical port info
	logicalPort := podLogicalPortName(pod)
	portInfo, err := oc.logicalPortCache.get(logicalPort)
	if err != nil {
		klog.Errorf(err.Error())
		return
	}

	// If we've already processed this pod, shortcut.
	if _, ok := np.localPods.Load(logicalPort); ok {
		return
	}

	np.RLock()
	defer np.RUnlock()
	if np.deleted {
		return
	}

	oc.localPodAddDefaultDeny(nsInfo, policy, portInfo)

	if np.portGroupUUID == "" {
		return
	}

	err = addToPortGroup(oc.ovnNBClient, np.portGroupName, portInfo)

	if err != nil {
		klog.Errorf("Failed to add logicalPort %s to portGroup %s (%v)",
			logicalPort, np.portGroupUUID, err)
	}

	np.localPods.Store(logicalPort, portInfo)
}

// handleLocalPodSelectorSetPods is a more efficient way of
// bulk-setting the local pods in a newly-created network policy
//
// THIS MUST BE KEPT CONSISTENT WITH AddPod!
func (oc *Controller) handleLocalPodSelectorSetPods(
	policy *knet.NetworkPolicy, np *networkPolicy, nsInfo *namespaceInfo,
	objs []interface{}) {

	// Take the write lock since this is called once and we will want to bulk-update
	// localPods
	np.Lock()
	defer np.Unlock()
	if np.deleted {
		return
	}

	klog.Infof("Setting NetworkPolicy %s/%s to have %d local pods...",
		np.namespace, np.name, len(objs))

	// get list of pods and their logical ports to add
	// theoretically this should never filter any pods but it's always good to be
	// paranoid.
	portsToAdd := make([]*lpInfo, 0, len(objs))
	for _, obj := range objs {
		pod := obj.(*kapi.Pod)

		if pod.Spec.NodeName == "" {
			continue
		}

		portInfo, err := oc.logicalPortCache.get(podLogicalPortName(pod))
		// pod is not yet handled
		// no big deal, we'll get the update when it is.
		if err != nil {
			continue
		}

		// this pod is somehow already added to this policy, then skip
		if _, ok := np.localPods.Load(portInfo.name); ok {
			continue
		}

		portsToAdd = append(portsToAdd, portInfo)
	}

	// add all ports to default deny
	oc.localPodAddDefaultDeny(nsInfo, policy, portsToAdd...)

	if np.portGroupUUID == "" {
		return
	}

	err := setPortGroup(oc.ovnNBClient, np.portGroupName, portsToAdd...)
	if err != nil {
		klog.Errorf("Failed to set ports in PortGroup for network policy %s/%s: %v", np.namespace, np.name, err)
	}

	for _, portInfo := range portsToAdd {
		np.localPods.Store(portInfo.name, portInfo)
	}

	klog.Infof("Done setting NetworkPolicy %s/%s local pods",
		np.namespace, np.name)

}

func (oc *Controller) handleLocalPodSelectorDelFunc(
	policy *knet.NetworkPolicy, np *networkPolicy, nsInfo *namespaceInfo,
	obj interface{}) {
	pod := obj.(*kapi.Pod)

	if pod.Spec.NodeName == "" {
		return
	}

	// Get the logical port info
	logicalPort := podLogicalPortName(pod)
	portInfo, err := oc.logicalPortCache.get(logicalPort)
	if err != nil {
		klog.Errorf(err.Error())
		return
	}

	// If we never saw this pod, short-circuit
	if _, ok := np.localPods.LoadAndDelete(logicalPort); !ok {
		return
	}

	np.RLock()
	defer np.RUnlock()
	if np.deleted {
		return
	}

	oc.localPodDelDefaultDeny(np, nsInfo, portInfo)

	if np.portGroupUUID == "" {
		return
	}

	err = deleteFromPortGroup(oc.ovnNBClient, np.portGroupName, portInfo)
	if err != nil {
		klog.Errorf("Failed to delete logicalPort %s from portGroup %s (%v)", portInfo.name, np.name, err)
	}
}

func (oc *Controller) handleLocalPodSelector(
	policy *knet.NetworkPolicy, np *networkPolicy, nsInfo *namespaceInfo) {

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
			oc.handleLocalPodSelectorSetPods(policy, np, nsInfo, objs)
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

	nsInfo, err := oc.waitForNamespaceLocked(policy.Namespace)
	if err != nil {
		klog.Errorf("Failed to wait for namespace %s event (%v)",
			policy.Namespace, err)
		return
	}
	_, alreadyExists := nsInfo.networkPolicies[policy.Name]
	if alreadyExists {
		nsInfo.Unlock()
		return
	}

	np := NewNetworkPolicy(policy)

	if len(nsInfo.networkPolicies) == 0 {
		err := oc.createDefaultDenyPortGroup(policy.Namespace, nsInfo, knet.PolicyTypeIngress, nsInfo.aclLogging.Deny, policy.Name)
		if err != nil {
			nsInfo.Unlock()
			klog.Errorf(err.Error())
			return
		}
		err = oc.createDefaultDenyPortGroup(policy.Namespace, nsInfo, knet.PolicyTypeEgress, nsInfo.aclLogging.Deny, policy.Name)
		if err != nil {
			nsInfo.Unlock()
			klog.Errorf(err.Error())
			return
		}
	}
	nsInfo.networkPolicies[policy.Name] = np

	nsInfo.Unlock()
	np.Lock()

	// Create a port group for the policy. All the pods that this policy
	// selects will be eventually added to this port group.
	readableGroupName := fmt.Sprintf("%s_%s", policy.Namespace, policy.Name)
	np.portGroupName = hashedPortGroup(readableGroupName)

	np.portGroupUUID, err = createPortGroup(oc.ovnNBClient, readableGroupName, np.portGroupName)
	if err != nil {
		klog.Errorf("Failed to create port_group for network policy %s in "+
			"namespace %s", policy.Name, policy.Namespace)
		return
	}

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

	// For all the pods in the local namespace that this policy
	// effects, add them to the port group.
	oc.handleLocalPodSelector(policy, np, nsInfo)

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

	// Finally, make sure that all ACLs are set
	oc.addNetworkPolicyACL(np, nsInfo.aclLogging.Allow)
}

func (oc *Controller) addNetworkPolicyACL(np *networkPolicy, aclLogging string) {
	np.Lock()
	defer np.Unlock()
	if !np.deleted {
		for _, gp := range np.ingressPolicies {
			gp.localPodSetACL(np.portGroupName, np.portGroupUUID, aclLogging)
		}
		for _, gp := range np.egressPolicies {
			gp.localPodSetACL(np.portGroupName, np.portGroupUUID, aclLogging)
		}
	}
}

func (oc *Controller) deleteNetworkPolicy(policy *knet.NetworkPolicy) {
	klog.Infof("Deleting network policy %s in namespace %s",
		policy.Name, policy.Namespace)

	nsInfo := oc.getNamespaceLocked(policy.Namespace)
	if nsInfo == nil {
		klog.V(5).Infof("Failed to get namespace lock when deleting policy %s in namespace %s",
			policy.Name, policy.Namespace)
		return
	}
	defer nsInfo.Unlock()

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

	oc.localPodDelDefaultDeny(np, nsInfo, ports...)

	if len(nsInfo.networkPolicies) == 0 {
		err := deletePortGroup(oc.ovnNBClient, nsInfo.portGroupIngressDenyName)
		if err != nil {
			klog.Errorf("%v", err)
		}
		err = deletePortGroup(oc.ovnNBClient, nsInfo.portGroupEgressDenyName)
		if err != nil {
			klog.Errorf("%v", err)
		}
	}

	// Delete the port group
	err := deletePortGroup(oc.ovnNBClient, np.portGroupName)
	if err != nil {
		klog.Errorf("%v", err)
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
		gp.localPodSetACL(np.portGroupName, np.portGroupUUID, aclLoggingLevels.Allow)
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
