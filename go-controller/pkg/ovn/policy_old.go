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

func (oc *Controller) syncNetworkPoliciesOld(networkPolicies []interface{}) {
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
			// policy doesn't exist on k8s. Delete acl rules from OVN
			deleteAclsPolicyOld(namespaceName, policyName)
			// delete the address sets for this policy from OVN
			deleteAddressSet(hashedAddressSet(addrSetName))
		}
	})
	if err != nil {
		logrus.Errorf("Error in syncing network policies: %v", err)
	}
}

func addACLAllowOld(namespace, policy, logicalSwitch, logicalPort, match, l4Match string,
	ipBlockCidr bool, gressNum int, policyType knet.PolicyType) {
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
		fmt.Sprintf("external-ids:namespace=%s", namespace),
		fmt.Sprintf("external-ids:policy=%s", policy),
		fmt.Sprintf("external-ids:%s_num=%d", policyType, gressNum),
		fmt.Sprintf("external-ids:policy_type=%s", policyType),
		fmt.Sprintf("external-ids:logical_switch=%s", logicalSwitch),
		fmt.Sprintf("external-ids:logical_port=%s", logicalPort))
	if err != nil {
		logrus.Errorf("find failed to get the allow rule for "+
			"namespace=%s, logical_port=%s, stderr: %q (%v)",
			namespace, logicalPort, stderr, err)
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
		fmt.Sprintf("external-ids:namespace=%s", namespace),
		fmt.Sprintf("external-ids:policy=%s", policy),
		fmt.Sprintf("external-ids:%s_num=%d", policyType, gressNum),
		fmt.Sprintf("external-ids:policy_type=%s", policyType),
		fmt.Sprintf("external-ids:logical_switch=%s", logicalSwitch),
		fmt.Sprintf("external-ids:logical_port=%s", logicalPort),
		"--", "add", "logical_switch", logicalSwitch, "acls", "@acl")
	if err != nil {
		logrus.Errorf("failed to create the allow-from rule for "+
			"namespace=%s, logical_port=%s, stderr: %q (%v)", namespace,
			logicalPort, stderr, err)
		return
	}
}

func modifyACLAllowOld(namespace, policy, logicalPort, oldMatch string, newMatch string,
	gressNum int, policyType knet.PolicyType) {
	uuid, stderr, err := util.RunOVNNbctl("--data=bare", "--no-heading",
		"--columns=_uuid", "find", "ACL", oldMatch,
		fmt.Sprintf("external-ids:namespace=%s", namespace),
		fmt.Sprintf("external-ids:policy=%s", policy),
		fmt.Sprintf("external-ids:%s_num=%d", policyType, gressNum),
		fmt.Sprintf("external-ids:policy_type=%s", policyType),
		fmt.Sprintf("external-ids:logical_port=%s", logicalPort))
	if err != nil {
		logrus.Errorf("find failed to get the allow rule for "+
			"namespace=%s, logical_port=%s, stderr: %q (%v)",
			namespace, logicalPort, stderr, err)
		return
	}

	if uuid != "" {
		// We already have an ACL. We will update it.
		_, stderr, err = util.RunOVNNbctl("set", "acl", uuid,
			newMatch)
		if err != nil {
			logrus.Errorf("failed to modify the allow-from rule for "+
				"namespace=%s, logical_port=%s, stderr: %q (%v)",
				namespace, logicalPort, stderr, err)
		}
		return
	}
}

func deleteACLAllowOld(namespace, policy, logicalSwitch, logicalPort, match, l4Match string,
	ipBlockCidr bool, gressNum int, policyType knet.PolicyType) {
	uuid, stderr, err := util.RunOVNNbctl("--data=bare", "--no-heading",
		"--columns=_uuid", "find", "ACL",
		fmt.Sprintf("external-ids:l4Match=\"%s\"", l4Match),
		fmt.Sprintf("external-ids:ipblock_cidr=%t", ipBlockCidr),
		fmt.Sprintf("external-ids:namespace=%s", namespace),
		fmt.Sprintf("external-ids:policy=%s", policy),
		fmt.Sprintf("external-ids:%s_num=%d", policyType, gressNum),
		fmt.Sprintf("external-ids:policy_type=%s", policyType),
		fmt.Sprintf("external-ids:logical_switch=%s", logicalSwitch),
		fmt.Sprintf("external-ids:logical_port=%s", logicalPort))
	if err != nil {
		logrus.Errorf("find failed to get the allow rule for "+
			"namespace=%s, logical_port=%s, stderr: %q, (%v)",
			namespace, logicalPort, stderr, err)
		return
	}

	if uuid == "" {
		logrus.Infof("deleteACLAllow: returning because find returned empty")
		return
	}

	_, stderr, err = util.RunOVNNbctl("remove", "logical_switch",
		logicalSwitch, "acls", uuid)
	if err != nil {
		logrus.Errorf("remove failed to delete the allow-from rule for "+
			"namespace=%s, logical_port=%s, stderr: %q (%v)", namespace,
			logicalPort, stderr, err)
		return
	}
}

func addIPBlockACLDenyOld(namespace, policy, logicalSwitch, logicalPort, except, priority string,
	policyType knet.PolicyType) {
	var match, l3Match, direction, lportMatch string
	direction = toLport
	if policyType == knet.PolicyTypeIngress {
		lportMatch = fmt.Sprintf("outport == \\\"%s\\\"", logicalPort)
		l3Match = fmt.Sprintf("ip4.src == %s", except)
		match = fmt.Sprintf("match=\"%s && %s\"", lportMatch, l3Match)
	} else {
		lportMatch = fmt.Sprintf("inport == \\\"%s\\\"", logicalPort)
		l3Match = fmt.Sprintf("ip4.dst == %s", except)
		match = fmt.Sprintf("match=\"%s && %s\"", lportMatch, l3Match)
	}

	uuid, stderr, err := util.RunOVNNbctl("--data=bare", "--no-heading",
		"--columns=_uuid", "find", "ACL", match, "action=drop",
		fmt.Sprintf("external-ids:ipblock-deny-policy-type=%s", policyType),
		fmt.Sprintf("external-ids:namespace=%s", namespace),
		fmt.Sprintf("external-ids:policy=%s", policy),
		fmt.Sprintf("external-ids:logical_switch=%s", logicalSwitch),
		fmt.Sprintf("external-ids:logical_port=%s", logicalPort))
	if err != nil {
		logrus.Errorf("find failed to get the default deny rule for "+
			"namespace=%s, logical_port=%s stderr: %q, (%v)",
			namespace, logicalPort, stderr, err)
		return
	}

	if uuid != "" {
		return
	}

	_, stderr, err = util.RunOVNNbctl("--id=@acl", "create", "acl",
		fmt.Sprintf("priority=%s", priority),
		fmt.Sprintf("direction=%s", direction), match, "action=drop",
		fmt.Sprintf("external-ids:ipblock-deny-policy-type=%s", policyType),
		fmt.Sprintf("external-ids:namespace=%s", namespace),
		fmt.Sprintf("external-ids:policy=%s", policy),
		fmt.Sprintf("external-ids:logical_switch=%s", logicalSwitch),
		fmt.Sprintf("external-ids:logical_port=%s", logicalPort),
		"--", "add", "logical_switch", logicalSwitch,
		"acls", "@acl")
	if err != nil {
		logrus.Errorf("error executing create ACL command, stderr: %q, %+v",
			stderr, err)
	}
}

func deleteIPBlockACLDenyOld(namespace, policy, logicalSwitch, logicalPort, except string,
	policyType knet.PolicyType) {
	var match, lportMatch, l3Match string
	if policyType == knet.PolicyTypeIngress {
		lportMatch = fmt.Sprintf("outport == \\\"%s\\\"", logicalPort)
		l3Match = fmt.Sprintf("ip4.src == %s", except)
		match = fmt.Sprintf("match=\"%s && %s\"", lportMatch, l3Match)
	} else {
		lportMatch = fmt.Sprintf("inport == \\\"%s\\\"", logicalPort)
		l3Match = fmt.Sprintf("ip4.dst == %s", except)
		match = fmt.Sprintf("match=\"%s && %s\"", lportMatch, l3Match)
	}

	uuid, stderr, err := util.RunOVNNbctl("--data=bare", "--no-heading",
		"--columns=_uuid", "find", "ACL", match, "action=drop",
		fmt.Sprintf("external-ids:ipblock-deny-policy-type=%s", policyType),
		fmt.Sprintf("external-ids:namespace=%s", namespace),
		fmt.Sprintf("external-ids:policy=%s", policy),
		fmt.Sprintf("external-ids:logical_switch=%s", logicalSwitch),
		fmt.Sprintf("external-ids:logical_port=%s", logicalPort))
	if err != nil {
		logrus.Errorf("find failed to get the default deny rule for "+
			"namespace=%s, logical_port=%s, stderr: %q. (%v)",
			namespace, logicalPort, stderr, err)
		return
	}

	if uuid == "" {
		return
	}

	_, stderr, err = util.RunOVNNbctl("remove", "logical_switch",
		logicalSwitch, "acls", uuid)
	if err != nil {
		logrus.Errorf("remove failed to delete the deny rule for "+
			"namespace=%s, logical_port=%s, stderr: %q (%v)",
			namespace, logicalPort, stderr, err)
		return
	}
}

func addACLDenyOld(namespace, logicalSwitch, logicalPort, priority string, policyType knet.PolicyType) {
	var match, direction string
	direction = toLport
	if policyType == knet.PolicyTypeIngress {
		match = fmt.Sprintf("match=\"outport == \\\"%s\\\"\"", logicalPort)
	} else {
		match = fmt.Sprintf("match=\"inport == \\\"%s\\\"\"", logicalPort)
	}

	uuid, stderr, err := util.RunOVNNbctl("--data=bare", "--no-heading",
		"--columns=_uuid", "find", "ACL", match, "action=drop",
		fmt.Sprintf("external-ids:default-deny-policy-type=%s", policyType),
		fmt.Sprintf("external-ids:namespace=%s", namespace),
		fmt.Sprintf("external-ids:logical_switch=%s", logicalSwitch),
		fmt.Sprintf("external-ids:logical_port=%s", logicalPort))
	if err != nil {
		logrus.Errorf("find failed to get the default deny rule for "+
			"namespace=%s, logical_port=%s, stderr: %q (%v)", namespace,
			logicalPort, stderr, err)
		return
	}

	if uuid != "" {
		return
	}

	_, stderr, err = util.RunOVNNbctl("--id=@acl", "create", "acl",
		fmt.Sprintf("priority=%s", priority),
		fmt.Sprintf("direction=%s", direction), match, "action=drop",
		fmt.Sprintf("external-ids:default-deny-policy-type=%s", policyType),
		fmt.Sprintf("external-ids:namespace=%s", namespace),
		fmt.Sprintf("external-ids:logical_switch=%s", logicalSwitch),
		fmt.Sprintf("external-ids:logical_port=%s", logicalPort),
		"--", "add", "logical_switch", logicalSwitch,
		"acls", "@acl")
	if err != nil {
		logrus.Errorf("error executing create ACL command, stderr: %q, %+v",
			stderr, err)
	}
}

func deleteACLDenyOld(namespace, logicalSwitch, logicalPort string, policyType knet.PolicyType) {
	var match string
	if policyType == knet.PolicyTypeIngress {
		match = fmt.Sprintf("match=\"outport == \\\"%s\\\"\"", logicalPort)
	} else {
		match = fmt.Sprintf("match=\"inport == \\\"%s\\\"\"", logicalPort)
	}

	uuid, stderr, err := util.RunOVNNbctl("--data=bare", "--no-heading",
		"--columns=_uuid", "find", "ACL", match, "action=drop",
		fmt.Sprintf("external-ids:default-deny-policy-type=%s", policyType),
		fmt.Sprintf("external-ids:namespace=%s", namespace),
		fmt.Sprintf("external-ids:logical_switch=%s", logicalSwitch),
		fmt.Sprintf("external-ids:logical_port=%s", logicalPort))
	if err != nil {
		logrus.Errorf("find failed to get the default deny rule for "+
			"namespace=%s, logical_port=%s, stderr: %q, (%v)",
			namespace, logicalPort, stderr, err)
		return
	}

	if uuid == "" {
		return
	}

	_, stderr, err = util.RunOVNNbctl("remove", "logical_switch",
		logicalSwitch, "acls", uuid)
	if err != nil {
		logrus.Errorf("remove failed to delete the deny rule for "+
			"namespace=%s, logical_port=%s, stderr: %q (%v)",
			namespace, logicalPort, stderr, err)
		return
	}
}

func deleteAclsPolicyOld(namespace, policy string) {
	uuids, stderr, err := util.RunOVNNbctl("--data=bare", "--no-heading",
		"--columns=_uuid", "find", "ACL",
		fmt.Sprintf("external-ids:namespace=%s", namespace),
		fmt.Sprintf("external-ids:policy=%s", policy))
	if err != nil {
		logrus.Errorf("find failed to get the allow rule for "+
			"namespace=%s, policy=%s, stderr: %q (%v)",
			namespace, policy, stderr, err)
		return
	}

	if uuids == "" {
		logrus.Debugf("deleteAclsPolicy: returning because find " +
			"returned no ACLs")
		return
	}

	uuidSlice := strings.Fields(uuids)
	for _, uuid := range uuidSlice {
		// Get logical switch
		logicalSwitch, stderr, err := util.RunOVNNbctl("--data=bare",
			"--no-heading", "--columns=_uuid", "find", "logical_switch",
			fmt.Sprintf("acls{>=}%s", uuid))
		if err != nil {
			logrus.Errorf("find failed to get the logical_switch of acl"+
				"uuid=%s, stderr: %q (%v)", uuid, stderr, err)
			continue
		}

		if logicalSwitch == "" {
			continue
		}

		_, stderr, err = util.RunOVNNbctl("remove", "logical_switch",
			logicalSwitch, "acls", uuid)
		if err != nil {
			logrus.Errorf("remove failed to delete the allow-from rule %s for"+
				" namespace=%s, policy=%s, logical_switch=%s, stderr: %q (%v)",
				uuid, namespace, policy, logicalSwitch, stderr, err)
			continue
		}
	}
}

func localPodAddOrDelACLOld(addDel string, policy *knet.NetworkPolicy, pod *kapi.Pod,
	gress *gressPolicy, logicalSwitch string) {
	logicalPort := podLogicalPortName(pod)
	l3Match := gress.getL3MatchFromAddressSet()

	var lportMatch, cidrMatch string
	if gress.policyType == knet.PolicyTypeIngress {
		lportMatch = fmt.Sprintf("outport == \\\"%s\\\"", logicalPort)
	} else {
		lportMatch = fmt.Sprintf("inport == \\\"%s\\\"", logicalPort)
	}

	// If IPBlock CIDR is not empty and except string [] is not empty,
	// add deny acl rule with priority ipBlockDenyPriority (1010).
	if len(gress.ipBlockCidr) > 0 && len(gress.ipBlockExcept) > 0 {
		except := fmt.Sprintf("{%s}", strings.Join(gress.ipBlockExcept, ", "))
		if addDel == addACL {
			addIPBlockACLDenyOld(policy.Namespace, policy.Name, logicalSwitch,
				logicalPort, except, ipBlockDenyPriority, gress.policyType)
		} else {
			deleteIPBlockACLDenyOld(policy.Namespace, policy.Name,
				logicalSwitch, logicalPort, except, gress.policyType)
		}
	}

	if len(gress.portPolicies) == 0 {
		match := fmt.Sprintf("match=\"%s && %s\"", l3Match,
			lportMatch)
		l4Match := noneMatch

		if addDel == addACL {
			if len(gress.ipBlockCidr) > 0 {
				// Add ACL allow rule for IPBlock CIDR
				cidrMatch = gress.getMatchFromIPBlock(lportMatch, l4Match)
				addACLAllowOld(policy.Namespace, policy.Name,
					logicalSwitch, logicalPort, cidrMatch, l4Match,
					true, gress.idx, gress.policyType)
			}
			addACLAllowOld(policy.Namespace, policy.Name,
				logicalSwitch, logicalPort, match, l4Match,
				false, gress.idx, gress.policyType)
		} else {
			if len(gress.ipBlockCidr) > 0 {
				// Delete ACL allow rule for IPBlock CIDR
				cidrMatch = gress.getMatchFromIPBlock(lportMatch, l4Match)
				deleteACLAllowOld(policy.Namespace, policy.Name,
					logicalSwitch, logicalPort, cidrMatch, l4Match,
					true, gress.idx, gress.policyType)
			}
			deleteACLAllowOld(policy.Namespace, policy.Name,
				logicalSwitch, logicalPort, match, l4Match,
				false, gress.idx, gress.policyType)
		}
	}
	for _, port := range gress.portPolicies {
		l4Match, err := port.getL4Match()
		if err != nil {
			continue
		}
		match := fmt.Sprintf("match=\"%s && %s && %s\"",
			l3Match, l4Match, lportMatch)
		if addDel == addACL {
			if len(gress.ipBlockCidr) > 0 {
				// Add ACL allow rule for IPBlock CIDR
				cidrMatch = gress.getMatchFromIPBlock(lportMatch, l4Match)
				addACLAllowOld(policy.Namespace, policy.Name,
					logicalSwitch, logicalPort, cidrMatch, l4Match,
					true, gress.idx, gress.policyType)
			}
			addACLAllowOld(policy.Namespace, policy.Name,
				pod.Spec.NodeName, logicalPort, match, l4Match,
				false, gress.idx, gress.policyType)
		} else {
			if len(gress.ipBlockCidr) > 0 {
				// Delete ACL allow rule for IPBlock CIDR
				cidrMatch = gress.getMatchFromIPBlock(lportMatch, l4Match)
				deleteACLAllowOld(policy.Namespace, policy.Name,
					logicalSwitch, logicalPort, cidrMatch, l4Match,
					true, gress.idx, gress.policyType)
			}
			deleteACLAllowOld(policy.Namespace, policy.Name,
				pod.Spec.NodeName, logicalPort, match, l4Match,
				false, gress.idx, gress.policyType)
		}
	}
}

func (oc *Controller) localPodAddDefaultDenyOld(
	policy *knet.NetworkPolicy,
	logicalPort, logicalSwitch string) {
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

	// Handle condition 1 above.
	if !(len(policy.Spec.PolicyTypes) == 1 && policy.Spec.PolicyTypes[0] == knet.PolicyTypeEgress) {
		if oc.lspIngressDenyCache[logicalPort] == 0 {
			addACLDenyOld(policy.Namespace, logicalSwitch, logicalPort,
				defaultDenyPriority, knet.PolicyTypeIngress)
		}
		oc.lspIngressDenyCache[logicalPort]++
	}

	// Handle condition 2 above.
	if (len(policy.Spec.PolicyTypes) == 1 && policy.Spec.PolicyTypes[0] == knet.PolicyTypeEgress) ||
		len(policy.Spec.Egress) > 0 || len(policy.Spec.PolicyTypes) == 2 {
		if oc.lspEgressDenyCache[logicalPort] == 0 {
			addACLDenyOld(policy.Namespace, logicalSwitch, logicalPort,
				defaultDenyPriority, knet.PolicyTypeEgress)
		}
		oc.lspEgressDenyCache[logicalPort]++
	}
	oc.lspMutex.Unlock()
}

func (oc *Controller) localPodDelDefaultDenyOld(
	policy *knet.NetworkPolicy,
	logicalPort, logicalSwitch string) {
	oc.lspMutex.Lock()

	if !(len(policy.Spec.PolicyTypes) == 1 && policy.Spec.PolicyTypes[0] == knet.PolicyTypeEgress) {
		if oc.lspIngressDenyCache[logicalPort] > 0 {
			oc.lspIngressDenyCache[logicalPort]--
			if oc.lspIngressDenyCache[logicalPort] == 0 {
				deleteACLDenyOld(policy.Namespace, logicalSwitch,
					logicalPort, knet.PolicyTypeIngress)
			}
		}
	}

	if (len(policy.Spec.PolicyTypes) == 1 && policy.Spec.PolicyTypes[0] == knet.PolicyTypeEgress) ||
		len(policy.Spec.Egress) > 0 || len(policy.Spec.PolicyTypes) == 2 {
		if oc.lspEgressDenyCache[logicalPort] > 0 {
			oc.lspEgressDenyCache[logicalPort]--
			if oc.lspEgressDenyCache[logicalPort] == 0 {
				deleteACLDenyOld(policy.Namespace, logicalSwitch,
					logicalPort, knet.PolicyTypeEgress)
			}
		}
	}
	oc.lspMutex.Unlock()
}

func (oc *Controller) handleLocalPodSelectorAddFuncOld(
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

	np.Lock()
	defer np.Unlock()

	if np.deleted {
		return
	}

	if np.localPods[logicalPort] {
		return
	}

	oc.localPodAddDefaultDenyOld(policy, logicalPort, logicalSwitch)

	// For each ingress rule, add a ACL
	for _, ingress := range np.ingressPolicies {
		localPodAddOrDelACLOld(addACL, policy, pod, ingress, logicalSwitch)
	}
	// For each egress rule, add a ACL
	for _, egress := range np.egressPolicies {
		localPodAddOrDelACLOld(addACL, policy, pod, egress, logicalSwitch)
	}

	np.localPods[logicalPort] = true
}

func (oc *Controller) handleLocalPodSelectorDelFuncOld(
	policy *knet.NetworkPolicy, np *namespacePolicy,
	obj interface{}) {
	pod := obj.(*kapi.Pod)

	logicalSwitch := pod.Spec.NodeName
	if logicalSwitch == "" {
		return
	}

	// Get the logical port name.
	logicalPort := podLogicalPortName(pod)

	np.Lock()
	defer np.Unlock()

	if np.deleted {
		return
	}

	if !np.localPods[logicalPort] {
		return
	}
	delete(np.localPods, logicalPort)
	oc.localPodDelDefaultDenyOld(policy, logicalPort, logicalSwitch)

	oc.lspMutex.Lock()
	delete(oc.lspIngressDenyCache, logicalPort)
	delete(oc.lspEgressDenyCache, logicalPort)
	oc.lspMutex.Unlock()

	// For each ingress rule, remove the ACL
	for _, ingress := range np.ingressPolicies {
		localPodAddOrDelACLOld(deleteACL, policy, pod, ingress, logicalSwitch)
	}
	// For each egress rule, remove the ACL
	for _, egress := range np.egressPolicies {
		localPodAddOrDelACLOld(deleteACL, policy, pod, egress, logicalSwitch)
	}
}

func (oc *Controller) handleLocalPodSelectorOld(
	policy *knet.NetworkPolicy, np *namespacePolicy) {

	h, err := oc.watchFactory.AddFilteredPodHandler(policy.Namespace,
		&policy.Spec.PodSelector,
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				oc.handleLocalPodSelectorAddFuncOld(policy, np, obj)
			},
			DeleteFunc: func(obj interface{}) {
				oc.handleLocalPodSelectorDelFuncOld(policy, np, obj)
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				oc.handleLocalPodSelectorAddFuncOld(policy, np, newObj)
			},
		}, nil)
	if err != nil {
		logrus.Errorf("error watching local pods for policy %s in namespace %s: %v",
			policy.Name, policy.Namespace, err)
		return
	}

	np.podHandlerList = append(np.podHandlerList, h)

}

func (oc *Controller) handlePeerNamespaceSelectorModifyOld(
	gress *gressPolicy, np *namespacePolicy, oldl3Match, newl3Match string) {

	for logicalPort := range np.localPods {
		var lportMatch string
		if gress.policyType == knet.PolicyTypeIngress {
			lportMatch = fmt.Sprintf("outport == \\\"%s\\\"", logicalPort)
		} else {
			lportMatch = fmt.Sprintf("inport == \\\"%s\\\"", logicalPort)
		}
		if len(gress.portPolicies) == 0 {
			oldMatch := fmt.Sprintf("match=\"%s && %s\"", oldl3Match,
				lportMatch)
			newMatch := fmt.Sprintf("match=\"%s && %s\"", newl3Match,
				lportMatch)
			modifyACLAllowOld(np.namespace, np.name, logicalPort,
				oldMatch, newMatch, gress.idx, gress.policyType)
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
			modifyACLAllowOld(np.namespace, np.name, logicalPort,
				oldMatch, newMatch, gress.idx, gress.policyType)
		}
	}
}

// addNetworkPolicyOld creates and applies OVN ACLs to pod logical switch
// ports from Kubernetes NetworkPolicy objects without using OVN Port Groups
func (oc *Controller) addNetworkPolicyOld(policy *knet.NetworkPolicy) {
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
					oc.handlePeerNamespaceSelectorModifyOld)
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
					oc.handlePeerNamespaceSelectorModifyOld)
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
	// effects, add ACL rules.
	oc.handleLocalPodSelectorOld(policy, np)
}

func (oc *Controller) getLogicalSwitchForLogicalPort(
	logicalPort string) string {
	if oc.logicalPortCache[logicalPort] != "" {
		return oc.logicalPortCache[logicalPort]
	}

	logicalSwitch, stderr, err := util.RunOVNNbctl("get",
		"logical_switch_port", logicalPort, "external-ids:logical_switch")
	if err != nil {
		logrus.Errorf("Error obtaining logical switch for %s, stderr: %q (%v)",
			logicalPort, stderr, err)
		return ""
	}
	if logicalSwitch == "" {
		logrus.Errorf("Error obtaining logical switch for %s",
			logicalPort)
		return ""
	}
	return logicalSwitch
}

func (oc *Controller) deleteNetworkPolicyOld(
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
		logicalSwitch := oc.getLogicalSwitchForLogicalPort(
			logicalPort)
		oc.localPodDelDefaultDenyOld(policy, logicalPort, logicalSwitch)
	}
	oc.namespacePolicies[policy.Namespace][policy.Name] = nil

	// We should now delete all the ACLs added by this network policy.
	deleteAclsPolicyOld(policy.Namespace, policy.Name)
}
