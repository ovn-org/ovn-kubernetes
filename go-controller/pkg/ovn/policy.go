package ovn

import (
	"fmt"
	"strings"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/sirupsen/logrus"
	kapi "k8s.io/api/core/v1"
	knet "k8s.io/api/networking/v1"
	"k8s.io/client-go/tools/cache"
)

func (oc *Controller) syncNetworkPoliciesPortGroup(
	networkPolicies []interface{}) {
	expectedPolicies := make(map[string]map[string]bool)
	for _, npInterface := range networkPolicies {
		policy, ok := npInterface.(*knet.NetworkPolicy)
		if !ok {
			logrus.Errorf("Spurious object in syncNetworkPolicies: %v",
				npInterface)
			continue
		}
		expectedPolicies[policy.Namespace] = map[string]bool{
			policy.Name: true}
	}

	err := oc.forEachAddressSetUnhashedName(func(addrSetName, namespaceName,
		policyName string) {
		if policyName != "" &&
			!expectedPolicies[namespaceName][policyName] {
			// policy doesn't exist on k8s. Delete the port group
			portGroupName := fmt.Sprintf("%s_%s", namespaceName, policyName)
			hashedLocalPortGroup := hashedPortGroup(portGroupName)
			deletePortGroup(hashedLocalPortGroup)

			// delete the address sets for this policy from OVN
			deleteAddressSet(hashedAddressSet(addrSetName))
		}
	})
	if err != nil {
		logrus.Errorf("Error in syncing network policies: %v", err)
	}
}

func addACLAllow(np *namespacePolicy, match, l4Match string, ipBlockCidr bool, gressNum int, policyType knet.PolicyType) {
	var direction, action string
	direction = toLport
	if policyType == knet.PolicyTypeIngress {
		action = "allow-related"
	} else {
		action = "allow"
	}

	uuid, stderr, err := util.RunOVNNbctl("--data=bare", "--no-heading",
		"--columns=_uuid", "find", "ACL",
		fmt.Sprintf("external-ids:l4Match=\"%s\"", l4Match),
		fmt.Sprintf("external-ids:ipblock_cidr=%t", ipBlockCidr),
		fmt.Sprintf("external-ids:namespace=%s", np.namespace),
		fmt.Sprintf("external-ids:policy=%s", np.name),
		fmt.Sprintf("external-ids:%s_num=%d", policyType, gressNum),
		fmt.Sprintf("external-ids:policy_type=%s", policyType))
	if err != nil {
		logrus.Errorf("find failed to get the allow rule for "+
			"namespace=%s, policy=%s, stderr: %q (%v)",
			np.namespace, np.name, stderr, err)
		return
	}

	if uuid != "" {
		return
	}

	_, stderr, err = util.RunOVNNbctl("--id=@acl", "create",
		"acl", fmt.Sprintf("priority=%s", defaultAllowPriority),
		fmt.Sprintf("direction=%s", direction), match,
		fmt.Sprintf("action=%s", action),
		fmt.Sprintf("external-ids:l4Match=\"%s\"", l4Match),
		fmt.Sprintf("external-ids:ipblock_cidr=%t", ipBlockCidr),
		fmt.Sprintf("external-ids:namespace=%s", np.namespace),
		fmt.Sprintf("external-ids:policy=%s", np.name),
		fmt.Sprintf("external-ids:%s_num=%d", policyType, gressNum),
		fmt.Sprintf("external-ids:policy_type=%s", policyType),
		"--", "add", "port_group", np.portGroupUUID, "acls", "@acl")
	if err != nil {
		logrus.Errorf("failed to create the acl allow rule for "+
			"namespace=%s, policy=%s, stderr: %q (%v)", np.namespace,
			np.name, stderr, err)
		return
	}
}

func modifyACLAllow(namespace, policy, oldMatch string, newMatch string, gressNum int, policyType knet.PolicyType) {
	uuid, stderr, err := util.RunOVNNbctl("--data=bare", "--no-heading",
		"--columns=_uuid", "find", "ACL", oldMatch,
		fmt.Sprintf("external-ids:namespace=%s", namespace),
		fmt.Sprintf("external-ids:policy=%s", policy),
		fmt.Sprintf("external-ids:%s_num=%d", policyType, gressNum),
		fmt.Sprintf("external-ids:policy_type=%s", policyType))
	if err != nil {
		logrus.Errorf("find failed to get the allow rule for "+
			"namespace=%s, policy=%s, stderr: %q (%v)",
			namespace, policy, stderr, err)
		return
	}

	if uuid != "" {
		// We already have an ACL. We will update it.
		_, stderr, err = util.RunOVNNbctl("set", "acl", uuid,
			newMatch)
		if err != nil {
			logrus.Errorf("failed to modify the allow-from rule for "+
				"namespace=%s, policy=%s, stderr: %q (%v)",
				namespace, policy, stderr, err)
		}
		return
	}
}

func addIPBlockACLDeny(np *namespacePolicy, except, priority string, gressNum int, policyType knet.PolicyType) {
	var match, l3Match, direction, lportMatch string
	direction = toLport
	if policyType == knet.PolicyTypeIngress {
		lportMatch = fmt.Sprintf("outport == @%s", np.portGroupName)
		l3Match = fmt.Sprintf("%s.src == %s", ipMatch(), except)
		match = fmt.Sprintf("match=\"%s && %s\"", lportMatch, l3Match)
	} else {
		lportMatch = fmt.Sprintf("inport == @%s", np.portGroupName)
		l3Match = fmt.Sprintf("%s.dst == %s", ipMatch(), except)
		match = fmt.Sprintf("match=\"%s && %s\"", lportMatch, l3Match)
	}

	uuid, stderr, err := util.RunOVNNbctl("--data=bare", "--no-heading",
		"--columns=_uuid", "find", "ACL", match, "action=drop",
		fmt.Sprintf("external-ids:ipblock-deny-policy-type=%s", policyType),
		fmt.Sprintf("external-ids:namespace=%s", np.namespace),
		fmt.Sprintf("external-ids:%s_num=%d", policyType, gressNum),
		fmt.Sprintf("external-ids:policy=%s", np.name))
	if err != nil {
		logrus.Errorf("find failed to get the ipblock default deny rule for "+
			"namespace=%s, policy=%s stderr: %q, (%v)",
			np.namespace, np.name, stderr, err)
		return
	}

	if uuid != "" {
		return
	}

	_, stderr, err = util.RunOVNNbctl("--id=@acl", "create", "acl",
		fmt.Sprintf("priority=%s", priority),
		fmt.Sprintf("direction=%s", direction), match, "action=drop",
		fmt.Sprintf("external-ids:ipblock-deny-policy-type=%s", policyType),
		fmt.Sprintf("external-ids:%s_num=%d", policyType, gressNum),
		fmt.Sprintf("external-ids:namespace=%s", np.namespace),
		fmt.Sprintf("external-ids:policy=%s", np.name),
		"--", "add", "port_group", np.portGroupUUID,
		"acls", "@acl")
	if err != nil {
		logrus.Errorf("error executing create ACL command, stderr: %q, %+v",
			stderr, err)
	}
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

func addACLPortGroup(portGroupUUID, portGroupName, direction, priority, match, action string, policyType knet.PolicyType) error {
	match = getACLMatch(portGroupName, match, policyType)
	uuid, stderr, err := util.RunOVNNbctl("--data=bare", "--no-heading",
		"--columns=_uuid", "find", "ACL", match, "action="+action,
		fmt.Sprintf("external-ids:default-deny-policy-type=%s", policyType))
	if err != nil {
		return fmt.Errorf("find failed to get the default deny rule for "+
			"policy type %s stderr: %q (%v)", policyType, stderr, err)
	}

	if uuid != "" {
		return nil
	}

	_, stderr, err = util.RunOVNNbctl("--id=@acl", "create", "acl",
		fmt.Sprintf("priority=%s", priority),
		fmt.Sprintf("direction=%s", direction), match, "action="+action,
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
	uuid, stderr, err := util.RunOVNNbctl("--data=bare", "--no-heading",
		"--columns=_uuid", "find", "ACL", match, "action="+action,
		fmt.Sprintf("external-ids:default-deny-policy-type=%s", policyType))
	if err != nil {
		return fmt.Errorf("find failed to get the rule for "+
			"policy type %s stderr: %q (%v)", policyType, stderr, err)
	}

	if uuid == "" {
		return nil
	}

	_, stderr, err = util.RunOVNNbctl("remove", "port_group",
		portGroupName, "acls", uuid)
	if err != nil {
		return fmt.Errorf("remove failed to delete the rule for "+
			"port_group=%s, stderr: %q (%v)", portGroupName, stderr, err)
	}

	return nil
}

func (oc *Controller) addToACL(portGroup, logicalPort string) {
	logicalPortUUID := oc.getLogicalPortUUID(logicalPort)
	if logicalPortUUID == "" {
		return
	}

	_, stderr, err := util.RunOVNNbctl("--if-exists", "remove",
		"port_group", portGroup, "ports", logicalPortUUID, "--",
		"add", "port_group", portGroup, "ports", logicalPortUUID)
	if err != nil {
		logrus.Errorf("Failed to add logicalPort %s to portGroup %s "+
			"stderr: %q (%v)", logicalPort, portGroup, stderr, err)
	}
}

func (oc *Controller) deleteFromACL(portGroup, logicalPort string) {
	logicalPortUUID := oc.getLogicalPortUUID(logicalPort)
	if logicalPortUUID == "" {
		return
	}

	_, stderr, err := util.RunOVNNbctl("--if-exists", "remove",
		"port_group", portGroup, "ports", logicalPortUUID)
	if err != nil {
		logrus.Errorf("Failed to delete logicalPort %s to portGroup %s "+
			"stderr: %q (%v)", logicalPort, portGroup, stderr, err)
	}
}

func localPodAddACL(np *namespacePolicy, gress *gressPolicy) {
	l3Match := gress.getL3MatchFromAddressSet()

	var lportMatch, cidrMatch string
	if gress.policyType == knet.PolicyTypeIngress {
		lportMatch = fmt.Sprintf("outport == @%s", np.portGroupName)
	} else {
		lportMatch = fmt.Sprintf("inport == @%s", np.portGroupName)
	}

	// If IPBlock CIDR is not empty and except string [] is not empty,
	// add deny acl rule with priority ipBlockDenyPriority (1010).
	if len(gress.ipBlockCidr) > 0 && len(gress.ipBlockExcept) > 0 {
		except := fmt.Sprintf("{%s}", strings.Join(gress.ipBlockExcept, ", "))
		addIPBlockACLDeny(np, except, ipBlockDenyPriority, gress.idx, gress.policyType)
	}

	if len(gress.portPolicies) == 0 {
		match := fmt.Sprintf("match=\"%s && %s\"", l3Match,
			lportMatch)
		l4Match := noneMatch

		if len(gress.ipBlockCidr) > 0 {
			// Add ACL allow rule for IPBlock CIDR
			cidrMatch = gress.getMatchFromIPBlock(lportMatch, l4Match)
			addACLAllow(np, cidrMatch, l4Match, true, gress.idx, gress.policyType)
		}
		addACLAllow(np, match, l4Match, false, gress.idx, gress.policyType)
	}
	for _, port := range gress.portPolicies {
		l4Match, err := port.getL4Match()
		if err != nil {
			continue
		}
		match := fmt.Sprintf("match=\"%s && %s && %s\"",
			l3Match, l4Match, lportMatch)
		if len(gress.ipBlockCidr) > 0 {
			// Add ACL allow rule for IPBlock CIDR
			cidrMatch = gress.getMatchFromIPBlock(lportMatch, l4Match)
			addACLAllow(np, cidrMatch, l4Match, true, gress.idx, gress.policyType)
		}
		addACLAllow(np, match, l4Match, false, gress.idx, gress.policyType)
	}
}

func (oc *Controller) createDefaultDenyPortGroup(policyType knet.PolicyType) error {
	var portGroupName string
	if policyType == knet.PolicyTypeIngress {
		if oc.portGroupIngressDeny != "" {
			return nil
		}
		portGroupName = "ingressDefaultDeny"
	} else if policyType == knet.PolicyTypeEgress {
		if oc.portGroupEgressDeny != "" {
			return nil
		}
		portGroupName = "egressDefaultDeny"
	}
	portGroupUUID, err := createPortGroup(portGroupName, portGroupName)
	if err != nil {
		return fmt.Errorf("Failed to create port_group for %s (%v)",
			portGroupName, err)
	}
	err = addACLPortGroup(portGroupUUID, portGroupName, toLport,
		defaultDenyPriority, "", "drop", policyType)
	if err != nil {
		return fmt.Errorf("Failed to create default deny port group %v", err)
	}

	if policyType == knet.PolicyTypeIngress {
		oc.portGroupIngressDeny = portGroupUUID
	} else if policyType == knet.PolicyTypeEgress {
		oc.portGroupEgressDeny = portGroupUUID
	}
	return nil
}

// Creates the match string used for ACLs allowing incoming multicast into a
// namespace, that is, from IPs that are in the namespace's address set.
func getMulticastACLMatch(ns string) string {
	nsAddressSet := hashedAddressSet(ns)
	return "ip4.src == $" + nsAddressSet + " && ip4.mcast"
}

// Returns the multicast port group name and hash for namespace 'ns'.
func getMulticastPortGroup(ns string) (string, string) {
	portGroupName := "mcastPortGroup-" + ns
	return portGroupName, hashedPortGroup(portGroupName)
}

// Creates a policy to allow multicast traffic within 'ns':
// - a port group containing all logical ports associated with 'ns'
// - one "from-lport" ACL allowing egress multicast traffic from the pods
//   in 'ns'
// - one "to-lport" ACL allowing ingress multicast traffic to pods in 'ns'.
//   This matches only traffic originated by pods in 'ns' (based on the
//   namespace address set).
func (oc *Controller) createMulticastAllowPolicy(ns string) error {
	portGroupName, portGroupHash := getMulticastPortGroup(ns)
	portGroupUUID, err := createPortGroup(portGroupName, portGroupHash)
	if err != nil {
		return fmt.Errorf("Failed to create port_group for %s (%v)",
			portGroupName, err)
	}

	err = addACLPortGroup(portGroupUUID, portGroupHash, fromLport,
		defaultMcastAllowPriority, "ip4.mcast", "allow",
		knet.PolicyTypeEgress)
	if err != nil {
		return fmt.Errorf("Failed to create allow egress multicast ACL for %s (%v)",
			ns, err)
	}

	err = addACLPortGroup(portGroupUUID, portGroupHash, toLport,
		defaultMcastAllowPriority, getMulticastACLMatch(ns), "allow",
		knet.PolicyTypeIngress)
	if err != nil {
		return fmt.Errorf("Failed to create allow ingress multicast ACL for %s (%v)",
			ns, err)
	}

	// Add all ports from this namespace to the multicast allow group.
	for _, portName := range oc.namespaceAddressSet[ns] {
		oc.podAddAllowMulticastPolicy(ns, portName)
	}

	return nil
}

// Delete the policy to allow multicast traffic within 'ns'.
func deleteMulticastAllowPolicy(ns string) error {
	_, portGroupHash := getMulticastPortGroup(ns)

	err := deleteACLPortGroup(portGroupHash, fromLport,
		defaultMcastAllowPriority, "ip4.mcast", "allow",
		knet.PolicyTypeEgress)
	if err != nil {
		return fmt.Errorf("Failed to delete allow egress multicast ACL for %s (%v)",
			ns, err)
	}

	err = deleteACLPortGroup(portGroupHash, toLport,
		defaultMcastAllowPriority, getMulticastACLMatch(ns), "allow",
		knet.PolicyTypeIngress)
	if err != nil {
		return fmt.Errorf("Failed to delete allow ingress multicast ACL for %s (%v)",
			ns, err)
	}

	deletePortGroup(portGroupHash)
	return nil
}

// Creates a global default deny multicast policy:
// - one ACL dropping egress multicast traffic from all pods: this is to
//   protect OVN controller from processing IP multicast reports from nodes
//   that are not allowed to receive multicast raffic.
// - one ACL dropping ingress multicast traffic to all pods.
func createDefaultDenyMulticastPolicy() error {
	portGroupName := "mcastPortGroupDeny"
	portGroupUUID, err := createPortGroup(portGroupName, portGroupName)
	if err != nil {
		return fmt.Errorf("Failed to create port_group for %s (%v)",
			portGroupName, err)
	}

	// By default deny any egress multicast traffic from any pod. This drops
	// IP multicast membership reports therefore denying any multicast traffic
	// to be forwarded to pods.
	err = addACLPortGroup(portGroupUUID, portGroupName, fromLport,
		defaultMcastDenyPriority, "ip4.mcast", "drop", knet.PolicyTypeEgress)
	if err != nil {
		return fmt.Errorf("Failed to create default deny multicast egress ACL (%v)",
			err)
	}

	// By default deny any ingress multicast traffic to any pod.
	err = addACLPortGroup(portGroupUUID, portGroupName, toLport,
		defaultMcastDenyPriority, "ip4.mcast", "drop", knet.PolicyTypeIngress)
	if err != nil {
		return fmt.Errorf("Failed to create default deny multicast ingress ACL (%v)",
			err)
	}

	return nil
}

func (oc *Controller) podAddDefaultDenyMulticastPolicy(logicalPort string) {
	oc.addToACL("mcastPortGroupDeny", logicalPort)
}

func (oc *Controller) podDeleteDefaultDenyMulticastPolicy(logicalPort string) {
	oc.deleteFromACL("mcastPortGroupDeny", logicalPort)
}

func (oc *Controller) podAddAllowMulticastPolicy(ns, logicalPort string) {
	_, portGroupHash := getMulticastPortGroup(ns)
	oc.addToACL(portGroupHash, logicalPort)
}

func (oc *Controller) podDeleteAllowMulticastPolicy(ns, logicalPort string) {
	_, portGroupHash := getMulticastPortGroup(ns)
	oc.deleteFromACL(portGroupHash, logicalPort)
}

func (oc *Controller) localPodAddDefaultDeny(
	policy *knet.NetworkPolicy, logicalPort string) {
	oc.lspMutex.Lock()
	defer oc.lspMutex.Unlock()

	err := oc.createDefaultDenyPortGroup(knet.PolicyTypeIngress)
	if err != nil {
		logrus.Errorf(err.Error())
		return
	}
	err = oc.createDefaultDenyPortGroup(knet.PolicyTypeEgress)
	if err != nil {
		logrus.Errorf(err.Error())
		return
	}

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

	// Handle condition 1 above.
	if !(len(policy.Spec.PolicyTypes) == 1 && policy.Spec.PolicyTypes[0] == knet.PolicyTypeEgress) {
		if oc.lspIngressDenyCache[logicalPort] == 0 {
			oc.addToACL(oc.portGroupIngressDeny, logicalPort)
		}
		oc.lspIngressDenyCache[logicalPort]++
	}

	// Handle condition 2 above.
	if (len(policy.Spec.PolicyTypes) == 1 && policy.Spec.PolicyTypes[0] == knet.PolicyTypeEgress) ||
		len(policy.Spec.Egress) > 0 || len(policy.Spec.PolicyTypes) == 2 {
		if oc.lspEgressDenyCache[logicalPort] == 0 {
			oc.addToACL(oc.portGroupEgressDeny, logicalPort)
		}
		oc.lspEgressDenyCache[logicalPort]++
	}
}

func (oc *Controller) localPodDelDefaultDeny(
	policy *knet.NetworkPolicy, logicalPort string) {
	oc.lspMutex.Lock()
	defer oc.lspMutex.Unlock()

	if !(len(policy.Spec.PolicyTypes) == 1 && policy.Spec.PolicyTypes[0] == knet.PolicyTypeEgress) {
		if oc.lspIngressDenyCache[logicalPort] > 0 {
			oc.lspIngressDenyCache[logicalPort]--
			if oc.lspIngressDenyCache[logicalPort] == 0 {
				oc.deleteFromACL(oc.portGroupIngressDeny, logicalPort)
			}
		}
	}

	if (len(policy.Spec.PolicyTypes) == 1 && policy.Spec.PolicyTypes[0] == knet.PolicyTypeEgress) ||
		len(policy.Spec.Egress) > 0 || len(policy.Spec.PolicyTypes) == 2 {
		if oc.lspEgressDenyCache[logicalPort] > 0 {
			oc.lspEgressDenyCache[logicalPort]--
			if oc.lspEgressDenyCache[logicalPort] == 0 {
				oc.deleteFromACL(oc.portGroupEgressDeny, logicalPort)
			}
		}
	}
}

func (oc *Controller) handleLocalPodSelectorAddFunc(
	policy *knet.NetworkPolicy, np *namespacePolicy,
	obj interface{}) {
	pod := obj.(*kapi.Pod)

	if _, err := util.UnmarshalPodAnnotation(pod.Annotations); err != nil {
		return
	}

	logicalSwitch := pod.Spec.NodeName
	if logicalSwitch == "" {
		return
	}

	// Get the logical port name.
	logicalPort := podLogicalPortName(pod)
	logicalPortUUID := oc.getLogicalPortUUID(logicalPort)
	if logicalPortUUID == "" {
		return
	}

	np.Lock()
	defer np.Unlock()

	if np.deleted {
		return
	}

	if np.localPods[logicalPort] {
		return
	}

	oc.localPodAddDefaultDeny(policy, logicalPort)

	if np.portGroupUUID == "" {
		return
	}

	_, stderr, err := util.RunOVNNbctl("--if-exists", "remove",
		"port_group", np.portGroupUUID, "ports", logicalPortUUID, "--",
		"add", "port_group", np.portGroupUUID, "ports", logicalPortUUID)
	if err != nil {
		logrus.Errorf("Failed to add logicalPort %s to portGroup %s "+
			"stderr: %q (%v)", logicalPort, np.portGroupUUID, stderr, err)
	}

	np.localPods[logicalPort] = true
}

func (oc *Controller) handleLocalPodSelectorDelFunc(
	policy *knet.NetworkPolicy, np *namespacePolicy,
	obj interface{}) {
	pod := obj.(*kapi.Pod)

	logicalSwitch := pod.Spec.NodeName
	if logicalSwitch == "" {
		return
	}

	// Get the logical port name.
	logicalPort := podLogicalPortName(pod)
	logicalPortUUID := oc.getLogicalPortUUID(logicalPort)

	np.Lock()
	defer np.Unlock()

	if np.deleted {
		return
	}

	if !np.localPods[logicalPort] {
		return
	}
	delete(np.localPods, logicalPort)
	oc.localPodDelDefaultDeny(policy, logicalPort)

	oc.lspMutex.Lock()
	delete(oc.lspIngressDenyCache, logicalPort)
	delete(oc.lspEgressDenyCache, logicalPort)
	oc.lspMutex.Unlock()

	if logicalPortUUID == "" || np.portGroupUUID == "" {
		return
	}

	_, stderr, err := util.RunOVNNbctl("--if-exists", "remove",
		"port_group", np.portGroupUUID, "ports", logicalPortUUID)
	if err != nil {
		logrus.Errorf("Failed to delete logicalPort %s from portGroup %s "+
			"stderr: %q (%v)", logicalPort, np.portGroupUUID, stderr, err)
	}
}

func (oc *Controller) handleLocalPodSelector(
	policy *knet.NetworkPolicy, np *namespacePolicy) {

	h, err := oc.watchFactory.AddFilteredPodHandler(policy.Namespace,
		&policy.Spec.PodSelector,
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				oc.handleLocalPodSelectorAddFunc(policy, np, obj)
			},
			DeleteFunc: func(obj interface{}) {
				oc.handleLocalPodSelectorDelFunc(policy, np, obj)
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				oc.handleLocalPodSelectorAddFunc(policy, np, newObj)
			},
		}, nil)
	if err != nil {
		logrus.Errorf("error watching local pods for policy %s in namespace %s: %v",
			policy.Name, policy.Namespace, err)
		return
	}

	np.podHandlerList = append(np.podHandlerList, h)
}

func (oc *Controller) handlePeerNamespaceSelectorModify(
	gress *gressPolicy, np *namespacePolicy, oldl3Match, newl3Match string) {

	var lportMatch string
	if gress.policyType == knet.PolicyTypeIngress {
		lportMatch = fmt.Sprintf("outport == @%s", np.portGroupName)
	} else {
		lportMatch = fmt.Sprintf("inport == @%s", np.portGroupName)
	}
	if len(gress.portPolicies) == 0 {
		oldMatch := fmt.Sprintf("match=\"%s && %s\"", oldl3Match,
			lportMatch)
		newMatch := fmt.Sprintf("match=\"%s && %s\"", newl3Match,
			lportMatch)
		modifyACLAllow(np.namespace, np.name, oldMatch, newMatch, gress.idx, gress.policyType)
	}
	for _, port := range gress.portPolicies {
		l4Match, err := port.getL4Match()
		if err != nil {
			continue
		}
		oldMatch := fmt.Sprintf("match=\"%s && %s && %s\"",
			oldl3Match, l4Match, lportMatch)
		newMatch := fmt.Sprintf("match=\"%s && %s && %s\"",
			newl3Match, l4Match, lportMatch)
		modifyACLAllow(np.namespace, np.name, oldMatch, newMatch, gress.idx, gress.policyType)
	}
}

// addNetworkPolicyPortGroup creates and applies OVN ACLs to pod logical switch
// ports from Kubernetes NetworkPolicy objects using OVN Port Groups
func (oc *Controller) addNetworkPolicyPortGroup(policy *knet.NetworkPolicy) {
	logrus.Infof("Adding network policy %s in namespace %s", policy.Name,
		policy.Namespace)

	if oc.namespacePolicies[policy.Namespace] != nil &&
		oc.namespacePolicies[policy.Namespace][policy.Name] != nil {
		return
	}

	err := oc.waitForNamespaceEvent(policy.Namespace)
	if err != nil {
		logrus.Errorf("failed to wait for namespace %s event (%v)",
			policy.Namespace, err)
		return
	}

	np := NewNamespacePolicy(policy)

	// Create a port group for the policy. All the pods that this policy
	// selects will be eventually added to this port group.
	readableGroupName := fmt.Sprintf("%s_%s", policy.Namespace, policy.Name)
	np.portGroupName = hashedPortGroup(readableGroupName)

	np.portGroupUUID, err = createPortGroup(readableGroupName, np.portGroupName)
	if err != nil {
		logrus.Errorf("Failed to create port_group for network policy %s in "+
			"namespace %s", policy.Name, policy.Namespace)
		return
	}

	// Go through each ingress rule.  For each ingress rule, create an
	// addressSet for the peer pods.
	for i, ingressJSON := range policy.Spec.Ingress {
		logrus.Debugf("Network policy ingress is %+v", ingressJSON)

		ingress := newGressPolicy(knet.PolicyTypeIngress, i)

		// Each ingress rule can have multiple ports to which we allow traffic.
		for _, portJSON := range ingressJSON.Ports {
			ingress.addPortPolicy(&portJSON)
		}

		hashedLocalAddressSet := ""
		// peerPodAddressMap represents the IP addresses of all the peer pods
		// for this ingress.
		peerPodAddressMap := make(map[string]bool)
		if len(ingressJSON.From) != 0 {
			// localPeerPods represents all the peer pods in the same
			// namespace from which we need to allow traffic.
			localPeerPods := fmt.Sprintf("%s.%s.%s.%d", policy.Namespace,
				policy.Name, "ingress", i)

			hashedLocalAddressSet = hashedAddressSet(localPeerPods)
			createAddressSet(localPeerPods, hashedLocalAddressSet, nil)
			ingress.addAddressSet(hashedLocalAddressSet)
		}

		for _, fromJSON := range ingressJSON.From {
			// Add IPBlock to ingress network policy
			if fromJSON.IPBlock != nil {
				ingress.addIPBlock(fromJSON.IPBlock)
			}
		}

		localPodAddACL(np, ingress)

		for _, fromJSON := range ingressJSON.From {
			if fromJSON.NamespaceSelector != nil && fromJSON.PodSelector != nil {
				// For each rule that contains both peer namespace selector and
				// peer pod selector, we create a watcher for each matching namespace
				// that populates the addressSet
				oc.handlePeerNamespaceAndPodSelector(policy, ingress,
					fromJSON.NamespaceSelector, fromJSON.PodSelector,
					hashedLocalAddressSet, peerPodAddressMap, np)

			} else if fromJSON.NamespaceSelector != nil {
				// For each peer namespace selector, we create a watcher that
				// populates ingress.peerAddressSets
				oc.handlePeerNamespaceSelector(policy,
					fromJSON.NamespaceSelector, ingress, np,
					oc.handlePeerNamespaceSelectorModify)
			} else if fromJSON.PodSelector != nil {
				// For each peer pod selector, we create a watcher that
				// populates the addressSet
				oc.handlePeerPodSelector(policy, fromJSON.PodSelector,
					hashedLocalAddressSet, peerPodAddressMap, np)
			}
		}
		np.ingressPolicies = append(np.ingressPolicies, ingress)
	}

	// Go through each egress rule.  For each egress rule, create an
	// addressSet for the peer pods.
	for i, egressJSON := range policy.Spec.Egress {
		logrus.Debugf("Network policy egress is %+v", egressJSON)

		egress := newGressPolicy(knet.PolicyTypeEgress, i)

		// Each egress rule can have multiple ports to which we allow traffic.
		for _, portJSON := range egressJSON.Ports {
			egress.addPortPolicy(&portJSON)
		}

		hashedLocalAddressSet := ""
		// peerPodAddressMap represents the IP addresses of all the peer pods
		// for this egress.
		peerPodAddressMap := make(map[string]bool)
		if len(egressJSON.To) != 0 {
			// localPeerPods represents all the peer pods in the same
			// namespace to which we need to allow traffic.
			localPeerPods := fmt.Sprintf("%s.%s.%s.%d", policy.Namespace,
				policy.Name, "egress", i)

			hashedLocalAddressSet = hashedAddressSet(localPeerPods)
			createAddressSet(localPeerPods, hashedLocalAddressSet, nil)
			egress.addAddressSet(hashedLocalAddressSet)
		}

		for _, toJSON := range egressJSON.To {
			// Add IPBlock to egress network policy
			if toJSON.IPBlock != nil {
				egress.addIPBlock(toJSON.IPBlock)
			}
		}

		localPodAddACL(np, egress)

		for _, toJSON := range egressJSON.To {
			if toJSON.NamespaceSelector != nil && toJSON.PodSelector != nil {
				// For each rule that contains both peer namespace selector and
				// peer pod selector, we create a watcher for each matching namespace
				// that populates the addressSet
				oc.handlePeerNamespaceAndPodSelector(policy, egress,
					toJSON.NamespaceSelector, toJSON.PodSelector,
					hashedLocalAddressSet, peerPodAddressMap, np)

			} else if toJSON.NamespaceSelector != nil {
				// For each peer namespace selector, we create a watcher that
				// populates egress.peerAddressSets
				oc.handlePeerNamespaceSelector(policy,
					toJSON.NamespaceSelector, egress, np,
					oc.handlePeerNamespaceSelectorModify)
			} else if toJSON.PodSelector != nil {
				// For each peer pod selector, we create a watcher that
				// populates the addressSet
				oc.handlePeerPodSelector(policy, toJSON.PodSelector,
					hashedLocalAddressSet, peerPodAddressMap, np)
			}
		}
		np.egressPolicies = append(np.egressPolicies, egress)
	}

	oc.namespacePolicies[policy.Namespace][policy.Name] = np

	// For all the pods in the local namespace that this policy
	// effects, add them to the port group.
	oc.handleLocalPodSelector(policy, np)
}

func (oc *Controller) deleteNetworkPolicyPortGroup(
	policy *knet.NetworkPolicy) {
	logrus.Infof("Deleting network policy %s in namespace %s",
		policy.Name, policy.Namespace)

	if oc.namespacePolicies[policy.Namespace] == nil ||
		oc.namespacePolicies[policy.Namespace][policy.Name] == nil {
		logrus.Errorf("Delete network policy %s in namespace %s "+
			"received without getting a create event",
			policy.Name, policy.Namespace)
		return
	}
	np := oc.namespacePolicies[policy.Namespace][policy.Name]

	np.Lock()
	defer np.Unlock()

	// Mark the policy as deleted.
	np.deleted = true

	// Go through each ingress rule.  For each ingress rule, delete the
	// addressSet for the local peer pods.
	for i := range np.ingressPolicies {
		localPeerPods := fmt.Sprintf("%s.%s.%s.%d", policy.Namespace,
			policy.Name, "ingress", i)
		hashedAddressSet := hashedAddressSet(localPeerPods)
		deleteAddressSet(hashedAddressSet)
	}
	// Go through each egress rule.  For each egress rule, delete the
	// addressSet for the local peer pods.
	for i := range np.egressPolicies {
		localPeerPods := fmt.Sprintf("%s.%s.%s.%d", policy.Namespace,
			policy.Name, "egress", i)
		hashedAddressSet := hashedAddressSet(localPeerPods)
		deleteAddressSet(hashedAddressSet)
	}

	// We should now stop all the handlers go routines.
	oc.shutdownHandlers(np)

	for logicalPort := range np.localPods {
		oc.localPodDelDefaultDeny(policy, logicalPort)
	}

	// Delete the port group
	deletePortGroup(np.portGroupName)

	oc.namespacePolicies[policy.Namespace][policy.Name] = nil
}
