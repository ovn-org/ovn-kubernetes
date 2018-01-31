package ovn

import (
	"fmt"
	"github.com/Sirupsen/logrus"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/kube"
	"k8s.io/api/core/v1"
	kapi "k8s.io/api/core/v1"
	kapisnetworking "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"net"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

type namespacePolicy struct {
	name                 string
	namespace            string
	ingressPolicies      []*gressPolicy
	egressPolicies       []*gressPolicy
	stop                 chan bool
	namespacePolicyMutex *sync.Mutex
	localPods            map[string]bool //pods effected by this policy
}

type gressPolicy struct {
	// peerAddressSets points to all the addressSets that hold
	// the peer pod's IP addresses. We will have one addressSet for
	// local pods and multiple addressSets that each represent a
	// peer namespace
	peerAddressSets map[string]bool

	// sortedPeerAddressSets has the sorted peerAddressSets
	sortedPeerAddressSets []string

	// portPolicies represents all the ports to which traffic is allowed for
	// the ingress rule in question.
	portPolicies []*portPolicy

	// ipBlock represents the CIDR IP block from which traffic is allowed
	// except the IP block in the except, which should be dropped.
	ipBlockCidr   string
	ipBlockExcept []string
}

type portPolicy struct {
	protocol string
	port     int32
}

const (
	ingressPolicyType = "Ingress"
	egressPolicyType  = "Egress"
	toLport           = "to-lport"
	fromLport         = "from-lport"
	addACL            = "add"
	deleteACL         = "delete"
	noneMatch         = "None"
	// Default deny acl rule priority
	defaultDenyPriority = "1000"
	// Default allow acl rule priority
	defaultAllowPriority = "1001"
	// IP Block except deny acl rule priority
	ipBlockDenyPriority = "1010"
)

func (oc *Controller) addAllowACLFromNode(logicalSwitch string) {
	uuid, err := exec.Command(OvnNbctl, "--data=bare", "--no-heading",
		"--columns=_uuid", "find", "ACL",
		fmt.Sprintf("external-ids:logical_switch=%s", logicalSwitch),
		"external-ids:node-acl=yes").Output()
	if err != nil {
		logrus.Errorf("find failed to get the node acl for "+
			"logical_switch=%s (%v)", logicalSwitch, err)
		return
	}

	if string(uuid) != "" {
		return
	}

	subnetRaw, err := exec.Command(OvnNbctl, "get", "logical_switch",
		logicalSwitch, "other-config:subnet").Output()
	if err != nil {
		logrus.Errorf("failed to get the logical_switch %s subent (%v)",
			logicalSwitch, err)
		return
	}

	if string(subnetRaw) == "" {
		return
	}

	subnet := strings.TrimFunc(string(subnetRaw), unicode.IsSpace)
	subnet = strings.Trim(subnet, `"`)

	ip, _, err := net.ParseCIDR(subnet)
	if err != nil {
		logrus.Errorf("failed to parse subnet %s", subnet)
		return
	}

	// K8s only supports IPv4 right now. The second IP address of the
	// network is the node IP address.
	ip = ip.To4()
	ip[3] = ip[3] + 2
	address := ip.String()

	match := fmt.Sprintf("match=\"ip4.src == %s\"", address)

	_, err = exec.Command(OvnNbctl, "--id=@acl", "create", "acl",
		fmt.Sprintf("priority=%s", defaultAllowPriority),
		"direction=to-lport", match, "action=allow-related",
		fmt.Sprintf("external-ids:logical_switch=%s", logicalSwitch),
		"external-ids:node-acl=yes",
		"--", "add", "logical_switch", logicalSwitch, "acls", "@acl").Output()
	if err != nil {
		logrus.Errorf("failed to create the node acl for "+
			"logical_switch=%s (%v)", logicalSwitch, err)
		return
	}
}

func (oc *Controller) addACLAllow(namespace, policy, logicalSwitch,
	logicalPort, match, l4Match string, ipBlockCidr bool, gressNum int,
	policyType string) {
	var direction string
	if policyType == ingressPolicyType {
		direction = toLport
	} else {
		direction = fromLport
	}

	uuid, err := exec.Command(OvnNbctl, "--data=bare", "--no-heading",
		"--columns=_uuid", "find", "ACL",
		fmt.Sprintf("external-ids:l4Match=\"%s\"", l4Match),
		fmt.Sprintf("external-ids:ipblock_cidr=%t", ipBlockCidr),
		fmt.Sprintf("external-ids:namespace=%s", namespace),
		fmt.Sprintf("external-ids:policy=%s", policy),
		fmt.Sprintf("external-ids:%s_num=%d", policyType, gressNum),
		fmt.Sprintf("external-ids:allow_direction=%s", policyType),
		fmt.Sprintf("external-ids:logical_switch=%s", logicalSwitch),
		fmt.Sprintf("external-ids:logical_port=%s", logicalPort)).Output()
	if err != nil {
		logrus.Errorf("find failed to get the allow rule for "+
			"namespace=%s, logical_port=%s (%v)", namespace, logicalPort, err)
		return
	}

	if string(uuid) != "" {
		return
	}

	_, err = exec.Command(OvnNbctl, "--id=@acl", "create", "acl",
		fmt.Sprintf("priority=%s", defaultAllowPriority),
		fmt.Sprintf("direction=%s", direction), match,
		"action=allow-related",
		fmt.Sprintf("external-ids:l4Match=\"%s\"", l4Match),
		fmt.Sprintf("external-ids:ipblock_cidr=%t", ipBlockCidr),
		fmt.Sprintf("external-ids:namespace=%s", namespace),
		fmt.Sprintf("external-ids:policy=%s", policy),
		fmt.Sprintf("external-ids:%s_num=%d", policyType, gressNum),
		fmt.Sprintf("external-ids:allow_direction=%s", policyType),
		fmt.Sprintf("external-ids:logical_switch=%s", logicalSwitch),
		fmt.Sprintf("external-ids:logical_port=%s", logicalPort),
		"--", "add", "logical_switch", logicalSwitch, "acls", "@acl").Output()
	if err != nil {
		logrus.Errorf("failed to create the allow-from rule for "+
			"namespace=%s, logical_port=%s (%v)", namespace, logicalPort, err)
		return
	}
}

func (oc *Controller) modifyACLAllow(namespace, policy, logicalPort,
	oldMatch string, newMatch string, gressNum int, policyType string) {
	uuid, err := exec.Command(OvnNbctl, "--data=bare", "--no-heading",
		"--columns=_uuid", "find", "ACL", oldMatch,
		fmt.Sprintf("external-ids:namespace=%s", namespace),
		fmt.Sprintf("external-ids:policy=%s", policy),
		fmt.Sprintf("external-ids:%s_num=%d", policyType, gressNum),
		fmt.Sprintf("external-ids:allow_direction=%s", policyType),
		fmt.Sprintf("external-ids:logical_port=%s", logicalPort)).Output()
	if err != nil {
		logrus.Errorf("find failed to get the allow rule for "+
			"namespace=%s, logical_port=%s (%v)", namespace, logicalPort, err)
		return
	}

	if string(uuid) != "" {
		// We already have an ACL. We will update it.
		uuidTrim := strings.TrimSpace(string(uuid))
		_, err = exec.Command(OvnNbctl, "set", "acl", uuidTrim,
			fmt.Sprintf("%s", newMatch)).Output()
		if err != nil {
			logrus.Errorf("failed to modify the allow-from rule for "+
				"namespace=%s, logical_port=%s (%v)", namespace, logicalPort,
				err)
		}
		return
	}
}

func (oc *Controller) deleteACLAllow(namespace, policy, logicalSwitch,
	logicalPort, match, l4Match string, ipBlockCidr bool, gressNum int,
	policyType string) {
	uuid, err := exec.Command(OvnNbctl, "--data=bare", "--no-heading",
		"--columns=_uuid", "find", "ACL",
		fmt.Sprintf("external-ids:l4Match=\"%s\"", l4Match),
		fmt.Sprintf("external-ids:ipblock_cidr=%t", ipBlockCidr),
		fmt.Sprintf("external-ids:namespace=%s", namespace),
		fmt.Sprintf("external-ids:policy=%s", policy),
		fmt.Sprintf("external-ids:%s_num=%d", policyType, gressNum),
		fmt.Sprintf("external-ids:allow_direction=%s", policyType),
		fmt.Sprintf("external-ids:logical_switch=%s", logicalSwitch),
		fmt.Sprintf("external-ids:logical_port=%s", logicalPort)).Output()
	if err != nil {
		logrus.Errorf("find failed to get the allow rule for "+
			"namespace=%s, logical_port=%s (%v)", namespace, logicalPort, err)
		return
	}

	if string(uuid) == "" {
		logrus.Infof("deleteACLAllow: returning because find returned empty")
		return
	}

	uuidTrim := strings.TrimSpace(string(uuid))

	_, err = exec.Command(OvnNbctl, "remove", "logical_switch", logicalSwitch,
		"acls", uuidTrim).Output()
	if err != nil {
		logrus.Errorf("remove failed to delete the allow-from rule for "+
			"namespace=%s, logical_port=%s (%v)", namespace, logicalPort, err)
		return
	}
}

func (oc *Controller) addIPBlockACLDeny(namespace, policy, logicalSwitch,
	logicalPort, policyType, except, priority string) {
	var match, l3Match, direction, lportMatch string
	if policyType == ingressPolicyType {
		lportMatch = fmt.Sprintf("outport == \\\"%s\\\"", logicalPort)
		l3Match = fmt.Sprintf("ip4.src == %s", except)
		match = fmt.Sprintf("match=\"%s && %s\"", lportMatch, l3Match)
		direction = toLport
	} else {
		lportMatch = fmt.Sprintf("inport == \\\"%s\\\"", logicalPort)
		l3Match = fmt.Sprintf("ip4.dst == %s", except)
		match = fmt.Sprintf("match=\"%s && %s\"", lportMatch, l3Match)
		direction = fromLport
	}

	uuid, err := exec.Command(OvnNbctl, "--data=bare", "--no-heading",
		"--columns=_uuid", "find", "ACL", match, "action=drop",
		fmt.Sprintf("external-ids:ipblock-deny-direction=%s", direction),
		fmt.Sprintf("external-ids:namespace=%s", namespace),
		fmt.Sprintf("external-ids:policy=%s", policy),
		fmt.Sprintf("external-ids:logical_switch=%s", logicalSwitch),
		fmt.Sprintf("external-ids:logical_port=%s", logicalPort)).Output()
	if err != nil {
		logrus.Errorf("find failed to get the default deny rule for "+
			"namespace=%s, logical_port=%s (%v)", namespace, logicalPort, err)
		return
	}

	if string(uuid) != "" {
		return
	}

	_, err = exec.Command("ovn-nbctl", "--id=@acl", "create", "acl",
		fmt.Sprintf("priority=%s", priority),
		fmt.Sprintf("direction=%s", direction), match, "action=drop",
		fmt.Sprintf("external-ids:ipblock-deny-direction=%s", direction),
		fmt.Sprintf("external-ids:namespace=%s", namespace),
		fmt.Sprintf("external-ids:policy=%s", policy),
		fmt.Sprintf("external-ids:logical_switch=%s", logicalSwitch),
		fmt.Sprintf("external-ids:logical_port=%s", logicalPort),
		"--", "add", "logical_switch", logicalSwitch,
		"acls", "@acl").Output()
	if err != nil {
		logrus.Errorf("error executing create ACL command %+v", err)
	}
	return
}

func (oc *Controller) deleteIPBlockACLDeny(namespace, policy,
	logicalSwitch, logicalPort, policyType, except string) {
	var match, direction, lportMatch, l3Match string
	if policyType == ingressPolicyType {
		lportMatch = fmt.Sprintf("outport == \\\"%s\\\"", logicalPort)
		l3Match = fmt.Sprintf("ip4.src == %s", except)
		match = fmt.Sprintf("match=\"%s && %s\"", lportMatch, l3Match)
		direction = toLport
	} else {
		lportMatch = fmt.Sprintf("inport == \\\"%s\\\"", logicalPort)
		l3Match = fmt.Sprintf("ip4.dst == %s", except)
		match = fmt.Sprintf("match=\"%s && %s\"", lportMatch, l3Match)
		direction = fromLport
	}

	uuid, err := exec.Command(OvnNbctl, "--data=bare", "--no-heading",
		"--columns=_uuid", "find", "ACL", match, "action=drop",
		fmt.Sprintf("external-ids:ipblock-deny-direction=%s", direction),
		fmt.Sprintf("external-ids:namespace=%s", namespace),
		fmt.Sprintf("external-ids:policy=%s", policy),
		fmt.Sprintf("external-ids:logical_switch=%s", logicalSwitch),
		fmt.Sprintf("external-ids:logical_port=%s", logicalPort)).Output()
	if err != nil {
		logrus.Errorf("find failed to get the default deny rule for "+
			"namespace=%s, logical_port=%s (%v)", namespace, logicalPort, err)
		return
	}

	if string(uuid) == "" {
		return
	}

	uuidTrim := strings.TrimSpace(string(uuid))

	_, err = exec.Command(OvnNbctl, "remove", "logical_switch", logicalSwitch,
		"acls", uuidTrim).Output()
	if err != nil {
		logrus.Errorf("remove failed to delete the deny rule for "+
			"namespace=%s, logical_port=%s (%v)", namespace, logicalPort, err)
		return
	}
	return
}

func (oc *Controller) addACLDeny(namespace, logicalSwitch, logicalPort,
	policyType, priority string) {
	var match, direction string
	if policyType == ingressPolicyType {
		match = fmt.Sprintf("match=\"outport == \\\"%s\\\"\"", logicalPort)
		direction = toLport
	} else {
		match = fmt.Sprintf("match=\"inport == \\\"%s\\\"\"", logicalPort)
		direction = fromLport
	}

	uuid, err := exec.Command(OvnNbctl, "--data=bare", "--no-heading",
		"--columns=_uuid", "find", "ACL", match, "action=drop",
		fmt.Sprintf("external-ids:default-deny-direction=%s", direction),
		fmt.Sprintf("external-ids:namespace=%s", namespace),
		fmt.Sprintf("external-ids:logical_switch=%s", logicalSwitch),
		fmt.Sprintf("external-ids:logical_port=%s", logicalPort)).Output()
	if err != nil {
		logrus.Errorf("find failed to get the default deny rule for "+
			"namespace=%s, logical_port=%s (%v)", namespace, logicalPort, err)
		return
	}

	if string(uuid) != "" {
		return
	}

	_, err = exec.Command("ovn-nbctl", "--id=@acl", "create", "acl",
		fmt.Sprintf("priority=%s", priority),
		fmt.Sprintf("direction=%s", direction), match, "action=drop",
		fmt.Sprintf("external-ids:default-deny-direction=%s", direction),
		fmt.Sprintf("external-ids:namespace=%s", namespace),
		fmt.Sprintf("external-ids:logical_switch=%s", logicalSwitch),
		fmt.Sprintf("external-ids:logical_port=%s", logicalPort),
		"--", "add", "logical_switch", logicalSwitch,
		"acls", "@acl").Output()
	if err != nil {
		logrus.Errorf("error executing create ACL command %+v", err)
	}
	return
}

func (oc *Controller) deleteACLDeny(namespace, logicalSwitch, logicalPort,
	policyType string) {
	var match, direction string
	if policyType == ingressPolicyType {
		match = fmt.Sprintf("match=\"outport == \\\"%s\\\"\"", logicalPort)
		direction = toLport
	} else {
		match = fmt.Sprintf("match=\"inport == \\\"%s\\\"\"", logicalPort)
		direction = fromLport
	}

	uuid, err := exec.Command(OvnNbctl, "--data=bare", "--no-heading",
		"--columns=_uuid", "find", "ACL", match, "action=drop",
		fmt.Sprintf("external-ids:default-deny-direction=%s", direction),
		fmt.Sprintf("external-ids:namespace=%s", namespace),
		fmt.Sprintf("external-ids:logical_switch=%s", logicalSwitch),
		fmt.Sprintf("external-ids:logical_port=%s", logicalPort)).Output()
	if err != nil {
		logrus.Errorf("find failed to get the default deny rule for "+
			"namespace=%s, logical_port=%s (%v)", namespace, logicalPort, err)
		return
	}

	if string(uuid) == "" {
		return
	}

	uuidTrim := strings.TrimSpace(string(uuid))

	_, err = exec.Command(OvnNbctl, "remove", "logical_switch", logicalSwitch,
		"acls", uuidTrim).Output()
	if err != nil {
		logrus.Errorf("remove failed to delete the deny rule for "+
			"namespace=%s, logical_port=%s (%v)", namespace, logicalPort, err)
		return
	}
	return
}

func (oc *Controller) deleteAclsPolicy(namespace, policy string) {
	uuids, err := exec.Command(OvnNbctl, "--data=bare", "--no-heading",
		"--columns=_uuid", "find", "ACL",
		fmt.Sprintf("external-ids:namespace=%s", namespace),
		fmt.Sprintf("external-ids:policy=%s", policy)).Output()
	if err != nil {
		logrus.Errorf("find failed to get the allow rule for "+
			"namespace=%s, policy=%s (%v)", namespace, policy, err)
		return
	}

	if string(uuids) == "" {
		logrus.Debugf("deleteAclsPolicy: returning because find " +
			"returned no ACLs")
		return
	}

	uuidSlice := strings.Fields(string(uuids))
	for _, uuid := range uuidSlice {
		// Get logical switch
		out, err := exec.Command(OvnNbctl, "--data=bare",
			"--no-heading", "--columns=_uuid", "find", "logical_switch",
			fmt.Sprintf("acls{>=}%s", uuid)).Output()
		if err != nil {
			logrus.Errorf("find failed to get the logical_switch of acl"+
				"uuid=%s (%v)", uuid, err)
			continue
		}

		if string(out) == "" {
			continue
		}
		logicalSwitch := strings.TrimSpace(string(out))

		_, err = exec.Command(OvnNbctl, "remove", "logical_switch",
			logicalSwitch, "acls", uuid).Output()
		if err != nil {
			logrus.Errorf("remove failed to delete the allow-from rule %s for"+
				" namespace=%s, policy=%s, logical_switch=%s (%s)",
				uuid, namespace, policy, logicalSwitch, err)
			continue
		}
	}
}

func newListWatchFromClient(c cache.Getter, resource string, namespace string,
	labelSelector string) *cache.ListWatch {
	listFunc := func(options metav1.ListOptions) (runtime.Object, error) {
		options.LabelSelector = labelSelector
		return c.Get().
			Namespace(namespace).
			Resource(resource).
			VersionedParams(&options, metav1.ParameterCodec).
			Do().
			Get()
	}
	watchFunc := func(options metav1.ListOptions) (watch.Interface, error) {
		options.Watch = true
		options.LabelSelector = labelSelector
		return c.Get().
			Namespace(namespace).
			Resource(resource).
			VersionedParams(&options, metav1.ParameterCodec).
			Watch()
	}
	return &cache.ListWatch{ListFunc: listFunc, WatchFunc: watchFunc}
}

func newNamespaceListWatchFromClient(c cache.Getter,
	labelSelector string) *cache.ListWatch {
	listFunc := func(options metav1.ListOptions) (runtime.Object, error) {
		options.LabelSelector = labelSelector
		return c.Get().
			Resource("namespaces").
			VersionedParams(&options, metav1.ParameterCodec).
			Do().
			Get()
	}
	watchFunc := func(options metav1.ListOptions) (watch.Interface, error) {
		options.Watch = true
		options.LabelSelector = labelSelector
		return c.Get().
			Resource("namespaces").
			VersionedParams(&options, metav1.ParameterCodec).
			Watch()
	}
	return &cache.ListWatch{ListFunc: listFunc, WatchFunc: watchFunc}
}

func getL3MatchFromAddressSet(addressSets []string, policyType string) string {
	var l3Match, addresses string
	for _, addressSet := range addressSets {
		if addresses == "" {
			addresses = fmt.Sprintf("$%s", addressSet)
			continue
		}
		addresses = fmt.Sprintf("%s, $%s", addresses,
			addressSet)
	}
	if addresses == "" {
		l3Match = "ip4"
	} else {
		if policyType == ingressPolicyType {
			l3Match = fmt.Sprintf("ip4.src == {%s}", addresses)
		} else {
			l3Match = fmt.Sprintf("ip4.dst == {%s}", addresses)
		}
	}
	return l3Match
}

func getMatchFromIPBlock(ipBlockCidr, lportMatch, l4Match,
	policyType string) string {
	var match string
	if policyType == ingressPolicyType {
		if l4Match == noneMatch {
			match = fmt.Sprintf("match=\"ip4.src == {%s} && %s\"",
				ipBlockCidr, lportMatch)
		} else {
			match = fmt.Sprintf("match=\"ip4.src == {%s} && %s && %s\"",
				ipBlockCidr, l4Match, lportMatch)
		}
	} else {
		if l4Match == noneMatch {
			match = fmt.Sprintf("match=\"ip4.dst == {%s} && %s\"",
				ipBlockCidr, lportMatch)
		} else {
			match = fmt.Sprintf("match=\"ip4.dst == {%s} && %s && %s\"",
				ipBlockCidr, l4Match, lportMatch)
		}
	}
	return match
}

func (oc *Controller) localPodAddOrDelACL(addDel string,
	policy *kapisnetworking.NetworkPolicy, pod *kapi.Pod, gress *gressPolicy,
	gressNum int, policyType, logicalSwitch string) {
	logicalPort := fmt.Sprintf("%s_%s", pod.Namespace, pod.Name)
	l3Match := getL3MatchFromAddressSet(gress.sortedPeerAddressSets,
		policyType)

	var lportMatch, cidrMatch string
	if policyType == ingressPolicyType {
		lportMatch = fmt.Sprintf("outport == \\\"%s\\\"", logicalPort)
	} else {
		lportMatch = fmt.Sprintf("inport == \\\"%s\\\"", logicalPort)
	}

	// If IPBlock CIDR is not empty and except string [] is not empty,
	// add deny acl rule with priority ipBlockDenyPriority (1010).
	if len(gress.ipBlockCidr) > 0 && len(gress.ipBlockExcept) > 0 {
		except := fmt.Sprintf("{%s}", strings.Join(gress.ipBlockExcept, ", "))
		if addDel == addACL {
			oc.addIPBlockACLDeny(policy.Namespace, policy.Name, logicalSwitch,
				logicalPort, policyType, except, ipBlockDenyPriority)
		} else {
			oc.deleteIPBlockACLDeny(policy.Namespace, policy.Name,
				logicalSwitch, logicalPort, policyType, except)
		}
	}

	if len(gress.portPolicies) == 0 {
		match := fmt.Sprintf("match=\"%s && %s\"", l3Match,
			lportMatch)
		l4Match := noneMatch

		if addDel == addACL {
			if len(gress.ipBlockCidr) > 0 {
				// Add ACL allow rule for IPBlock CIDR
				cidrMatch = getMatchFromIPBlock(gress.ipBlockCidr,
					lportMatch, l4Match, policyType)
				oc.addACLAllow(policy.Namespace, policy.Name,
					logicalSwitch, logicalPort, cidrMatch, l4Match,
					true, gressNum, policyType)
			}
			oc.addACLAllow(policy.Namespace, policy.Name,
				logicalSwitch, logicalPort, match, l4Match,
				false, gressNum, policyType)
		} else {
			if len(gress.ipBlockCidr) > 0 {
				// Delete ACL allow rule for IPBlock CIDR
				cidrMatch = getMatchFromIPBlock(gress.ipBlockCidr,
					lportMatch, l4Match, policyType)
				oc.deleteACLAllow(policy.Namespace, policy.Name,
					logicalSwitch, logicalPort, cidrMatch, l4Match,
					true, gressNum, policyType)
			}
			oc.deleteACLAllow(policy.Namespace, policy.Name,
				logicalSwitch, logicalPort, match, l4Match,
				false, gressNum, policyType)
		}
	}
	for _, port := range gress.portPolicies {
		var l4Match string
		if port.protocol == TCP {
			l4Match = fmt.Sprintf("tcp && tcp.dst==%d",
				port.port)
		} else if port.protocol == UDP {
			l4Match = fmt.Sprintf("udp && udp.dst==%d",
				port.port)
		} else {
			continue
		}
		match := fmt.Sprintf("match=\"%s && %s && %s\"",
			l3Match, l4Match, lportMatch)
		if addDel == addACL {
			if len(gress.ipBlockCidr) > 0 {
				// Add ACL allow rule for IPBlock CIDR
				cidrMatch = getMatchFromIPBlock(gress.ipBlockCidr,
					lportMatch, l4Match, policyType)
				oc.addACLAllow(policy.Namespace, policy.Name,
					logicalSwitch, logicalPort, cidrMatch, l4Match,
					true, gressNum, policyType)
			}
			oc.addACLAllow(policy.Namespace, policy.Name,
				pod.Spec.NodeName, logicalPort, match, l4Match,
				false, gressNum, policyType)
		} else {
			if len(gress.ipBlockCidr) > 0 {
				// Delete ACL allow rule for IPBlock CIDR
				cidrMatch = getMatchFromIPBlock(gress.ipBlockCidr,
					lportMatch, l4Match, policyType)
				oc.deleteACLAllow(policy.Namespace, policy.Name,
					logicalSwitch, logicalPort, cidrMatch, l4Match,
					true, gressNum, policyType)
			}
			oc.deleteACLAllow(policy.Namespace, policy.Name,
				pod.Spec.NodeName, logicalPort, match, l4Match,
				false, gressNum, policyType)
		}
	}
}

func (oc *Controller) localPodAddDefaultDeny(
	policy *kapisnetworking.NetworkPolicy,
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
	if !(len(policy.Spec.PolicyTypes) == 1 && policy.Spec.PolicyTypes[0] == egressPolicyType) {
		if oc.lspIngressDenyCache[logicalPort] == 0 {
			oc.addACLDeny(policy.Namespace, logicalSwitch, logicalPort,
				ingressPolicyType, defaultDenyPriority)
		}
		oc.lspIngressDenyCache[logicalPort]++
	}

	// Handle condition 2 above.
	if (len(policy.Spec.PolicyTypes) == 1 && policy.Spec.PolicyTypes[0] == egressPolicyType) ||
		len(policy.Spec.Egress) > 0 || len(policy.Spec.PolicyTypes) == 2 {
		if oc.lspEgressDenyCache[logicalPort] == 0 {
			oc.addACLDeny(policy.Namespace, logicalSwitch, logicalPort,
				egressPolicyType, defaultDenyPriority)
		}
		oc.lspEgressDenyCache[logicalPort]++
	}
	oc.lspMutex.Unlock()
}

func (oc *Controller) localPodDelDefaultDeny(
	policy *kapisnetworking.NetworkPolicy,
	logicalPort, logicalSwitch string) {
	oc.lspMutex.Lock()

	if !(len(policy.Spec.PolicyTypes) == 1 && policy.Spec.PolicyTypes[0] == egressPolicyType) {
		if oc.lspIngressDenyCache[logicalPort] > 0 {
			oc.lspIngressDenyCache[logicalPort]--
			if oc.lspIngressDenyCache[logicalPort] == 0 {
				oc.deleteACLDeny(policy.Namespace, logicalSwitch,
					logicalPort, ingressPolicyType)
			}
		}
	}

	if (len(policy.Spec.PolicyTypes) == 1 && policy.Spec.PolicyTypes[0] == egressPolicyType) ||
		len(policy.Spec.Egress) > 0 || len(policy.Spec.PolicyTypes) == 2 {
		if oc.lspEgressDenyCache[logicalPort] > 0 {
			oc.lspEgressDenyCache[logicalPort]--
			if oc.lspEgressDenyCache[logicalPort] == 0 {
				oc.deleteACLDeny(policy.Namespace, logicalSwitch,
					logicalPort, egressPolicyType)
			}
		}
	}
	oc.lspMutex.Unlock()
}

func (oc *Controller) handleLocalPodSelectorAddFunc(
	policy *kapisnetworking.NetworkPolicy, np *namespacePolicy,
	obj interface{}) {
	pod := obj.(*kapi.Pod)

	ipAddress := oc.getIPFromOvnAnnotation(pod.Annotations["ovn"])
	if ipAddress == "" {
		return
	}

	logicalSwitch := pod.Spec.NodeName
	if logicalSwitch == "" {
		return
	}

	// Get the logical port name.
	logicalPort := fmt.Sprintf("%s_%s", pod.Namespace, pod.Name)

	np.namespacePolicyMutex.Lock()
	defer np.namespacePolicyMutex.Unlock()

	if np.localPods[logicalPort] {
		return
	}

	oc.localPodAddDefaultDeny(policy, logicalPort, logicalSwitch)

	// For each ingress rule, add a ACL
	for i, ingress := range np.ingressPolicies {
		oc.localPodAddOrDelACL(addACL, policy, pod, ingress, i,
			ingressPolicyType, logicalSwitch)
	}
	// For each egress rule, add a ACL
	for i, egress := range np.egressPolicies {
		oc.localPodAddOrDelACL(addACL, policy, pod, egress, i,
			egressPolicyType, logicalSwitch)
	}

	np.localPods[logicalPort] = true
}

func (oc *Controller) handleLocalPodSelectorDelFunc(
	policy *kapisnetworking.NetworkPolicy, np *namespacePolicy,
	obj interface{}) {
	pod := obj.(*kapi.Pod)

	logicalSwitch := pod.Spec.NodeName
	if logicalSwitch == "" {
		return
	}

	// Get the logical port name.
	logicalPort := fmt.Sprintf("%s_%s", pod.Namespace, pod.Name)

	np.namespacePolicyMutex.Lock()
	defer np.namespacePolicyMutex.Unlock()

	if !np.localPods[logicalPort] {
		return
	}
	delete(np.localPods, logicalPort)
	oc.localPodDelDefaultDeny(policy, logicalPort, logicalSwitch)

	// For each ingress rule, remove the ACL
	for i, ingress := range np.ingressPolicies {
		oc.localPodAddOrDelACL(deleteACL, policy, pod, ingress, i,
			ingressPolicyType, logicalSwitch)
	}
	// For each egress rule, remove the ACL
	for i, egress := range np.egressPolicies {
		oc.localPodAddOrDelACL(deleteACL, policy, pod, egress, i,
			egressPolicyType, logicalSwitch)
	}
}

func (oc *Controller) handleLocalPodSelector(
	policy *kapisnetworking.NetworkPolicy, np *namespacePolicy) {

	client, _ := oc.Kube.(*kube.Kube)
	clientset, _ := client.KClient.(*kubernetes.Clientset)
	podSelectorAsSelector := metav1.FormatLabelSelector(
		&policy.Spec.PodSelector)

	watchlist := newListWatchFromClient(clientset.Core().RESTClient(), "pods",
		policy.Namespace, podSelectorAsSelector)

	_, controller := cache.NewInformer(
		watchlist,
		&v1.Pod{},
		time.Second*0,
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
		},
	)
	channel := make(chan struct{})
	go controller.Run(channel)

	// Wait for signal to stop.
	<-np.stop
	channel <- struct{}{}
	close(channel)
}

func (oc *Controller) handlePeerPodSelector(
	policy *kapisnetworking.NetworkPolicy, podSelector *metav1.LabelSelector,
	addressSet string, stop chan bool) {

	// TODO: Do we need a lock to protect this from concurrent delete and add
	// events?
	addressMap := make(map[string]bool)

	client, _ := oc.Kube.(*kube.Kube)
	clientset, _ := client.KClient.(*kubernetes.Clientset)
	podSelectorAsSelector := metav1.FormatLabelSelector(podSelector)

	watchlist := newListWatchFromClient(clientset.Core().RESTClient(), "pods",
		policy.Namespace, podSelectorAsSelector)

	_, controller := cache.NewInformer(
		watchlist,
		&v1.Pod{},
		time.Second*0,
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				pod := obj.(*kapi.Pod)
				ipAddress := oc.getIPFromOvnAnnotation(pod.Annotations["ovn"])
				if ipAddress == "" || addressMap[ipAddress] {
					return
				}
				addressMap[ipAddress] = true
				addresses := make([]string, 0, len(addressMap))
				for k := range addressMap {
					addresses = append(addresses, k)
				}
				oc.setAddressSet(addressSet, addresses)
			},
			DeleteFunc: func(obj interface{}) {
				pod := obj.(*kapi.Pod)

				ipAddress := oc.getIPFromOvnAnnotation(pod.Annotations["ovn"])
				if ipAddress == "" {
					return
				}

				if !addressMap[ipAddress] {
					return
				}

				delete(addressMap, ipAddress)

				addresses := make([]string, 0, len(addressMap))
				for k := range addressMap {
					addresses = append(addresses, k)
				}
				oc.setAddressSet(addressSet, addresses)
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				pod := newObj.(*kapi.Pod)
				ipAddress := oc.getIPFromOvnAnnotation(pod.Annotations["ovn"])
				if ipAddress == "" || addressMap[ipAddress] {
					return
				}
				addressMap[ipAddress] = true
				addresses := make([]string, 0, len(addressMap))
				for k := range addressMap {
					addresses = append(addresses, k)
				}
				oc.setAddressSet(addressSet, addresses)
			},
		},
	)
	channel := make(chan struct{})
	go controller.Run(channel)

	// Wait for signal to stop
	<-stop
	channel <- struct{}{}
	close(channel)
}

func (oc *Controller) handlePeerNamespaceSelectorModify(
	namespace *kapi.Namespace, gress *gressPolicy, gressNum int,
	np *namespacePolicy, oldl3Match, newl3Match, policyType string) {

	for logicalPort := range np.localPods {
		var lportMatch string
		if policyType == ingressPolicyType {
			lportMatch = fmt.Sprintf("outport == \\\"%s\\\"", logicalPort)
		} else {
			lportMatch = fmt.Sprintf("inport == \\\"%s\\\"", logicalPort)
		}
		if len(gress.portPolicies) == 0 {
			oldMatch := fmt.Sprintf("match=\"%s && %s\"", oldl3Match,
				lportMatch)
			newMatch := fmt.Sprintf("match=\"%s && %s\"", newl3Match,
				lportMatch)
			oc.modifyACLAllow(np.namespace, np.name, logicalPort,
				oldMatch, newMatch, gressNum, policyType)
		}
		for _, port := range gress.portPolicies {
			var l4Match string
			if port.protocol == TCP {
				l4Match = fmt.Sprintf("tcp && tcp.dst==%d", port.port)
			} else if port.protocol == UDP {
				l4Match = fmt.Sprintf("udp && udp.dst==%d", port.port)
			} else {
				continue
			}
			oldMatch := fmt.Sprintf("match=\"%s && %s && %s\"",
				oldl3Match, l4Match, lportMatch)
			newMatch := fmt.Sprintf("match=\"%s && %s && %s\"",
				newl3Match, l4Match, lportMatch)
			oc.modifyACLAllow(np.namespace, np.name, logicalPort,
				oldMatch, newMatch, gressNum, policyType)
		}
	}
}

func (oc *Controller) handlePeerNamespaceSelector(
	policy *kapisnetworking.NetworkPolicy,
	namespaceSelector *metav1.LabelSelector,
	gress *gressPolicy, gressNum int, policyType string, np *namespacePolicy) {

	client, _ := oc.Kube.(*kube.Kube)
	clientset, _ := client.KClient.(*kubernetes.Clientset)
	nsSelectorAsSelector := metav1.FormatLabelSelector(
		namespaceSelector)

	watchlist := newNamespaceListWatchFromClient(clientset.Core().RESTClient(),
		nsSelectorAsSelector)

	_, controller := cache.NewInformer(
		watchlist,
		&v1.Namespace{},
		time.Second*0,
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				namespace := obj.(*kapi.Namespace)
				np.namespacePolicyMutex.Lock()
				defer np.namespacePolicyMutex.Unlock()
				hashedAddressSet := hashedAddressSet(namespace.Name)
				if gress.peerAddressSets[hashedAddressSet] {
					return
				}

				oldL3Match := getL3MatchFromAddressSet(
					gress.sortedPeerAddressSets, policyType)

				gress.sortedPeerAddressSets = append(
					gress.sortedPeerAddressSets, hashedAddressSet)
				sort.Strings(gress.sortedPeerAddressSets)

				newL3Match := getL3MatchFromAddressSet(
					gress.sortedPeerAddressSets, policyType)

				oc.handlePeerNamespaceSelectorModify(namespace, gress,
					gressNum, np, oldL3Match, newL3Match, policyType)
				gress.peerAddressSets[hashedAddressSet] = true

			},
			DeleteFunc: func(obj interface{}) {
				namespace := obj.(*kapi.Namespace)
				np.namespacePolicyMutex.Lock()
				defer np.namespacePolicyMutex.Unlock()
				hashedAddressSet := hashedAddressSet(namespace.Name)
				if !gress.peerAddressSets[hashedAddressSet] {
					return
				}
				oldL3Match := getL3MatchFromAddressSet(
					gress.sortedPeerAddressSets, policyType)

				for i, addressSet := range gress.sortedPeerAddressSets {
					if addressSet == hashedAddressSet {
						gress.sortedPeerAddressSets = append(
							gress.sortedPeerAddressSets[:i],
							gress.sortedPeerAddressSets[i+1:]...)
						break
					}
				}

				newL3Match := getL3MatchFromAddressSet(
					gress.sortedPeerAddressSets, policyType)

				oc.handlePeerNamespaceSelectorModify(namespace, gress,
					gressNum, np, oldL3Match, newL3Match, policyType)

				delete(gress.peerAddressSets, hashedAddressSet)
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				return
			},
		},
	)
	channel := make(chan struct{})
	go controller.Run(channel)

	// Wait for signal to stop
	<-np.stop
	channel <- struct{}{}
	close(channel)
}

func (oc *Controller) addNetworkPolicy(policy *kapisnetworking.NetworkPolicy) {
	logrus.Infof("Adding network policy %s in namespace %s", policy.Name,
		policy.Namespace)

	if oc.namespacePolicies[policy.Namespace] != nil &&
		oc.namespacePolicies[policy.Namespace][policy.Name] != nil {
		return
	}

	oc.waitForNamespaceEvent(policy.Namespace)

	np := &namespacePolicy{}
	np.name = policy.Name
	np.namespace = policy.Namespace
	np.ingressPolicies = make([]*gressPolicy, 0)
	np.egressPolicies = make([]*gressPolicy, 0)
	np.stop = make(chan bool, 1)
	np.localPods = make(map[string]bool)
	np.namespacePolicyMutex = &sync.Mutex{}

	// Go through each ingress rule.  For each ingress rule, create an
	// addressSet for the peer pods.
	for i, ingressJSON := range policy.Spec.Ingress {
		logrus.Debugf("Network policy ingress is %+v", ingressJSON)

		ingress := &gressPolicy{}
		ingress.peerAddressSets = make(map[string]bool)
		ingress.sortedPeerAddressSets = make([]string, 0)

		// Each ingress rule can have multiple ports to which we allow traffic.
		ingress.portPolicies = make([]*portPolicy, 0)
		for _, portJSON := range ingressJSON.Ports {
			port := &portPolicy{}
			port.protocol = string(*portJSON.Protocol)
			port.port = portJSON.Port.IntVal
			ingress.portPolicies = append(ingress.portPolicies, port)
		}

		hashedLocalAddressSet := ""
		if len(ingressJSON.From) != 0 {
			// localPeerPods represents all the peer pods in the same
			// namespace from which we need to allow traffic.
			localPeerPods := fmt.Sprintf("%s.%s.%s.%d", policy.Namespace,
				policy.Name, "ingress", i)

			hashedLocalAddressSet = hashedAddressSet(localPeerPods)
			ingress.peerAddressSets[hashedLocalAddressSet] = true
			oc.createAddressSet(localPeerPods, hashedLocalAddressSet, nil)
			ingress.sortedPeerAddressSets = append(
				ingress.sortedPeerAddressSets, hashedLocalAddressSet)
		}

		for _, fromJSON := range ingressJSON.From {
			// Add IPBlock to ingress network policy
			if fromJSON.IPBlock != nil {
				ingress.ipBlockCidr = fromJSON.IPBlock.CIDR
				ingress.ipBlockExcept = append([]string{},
					fromJSON.IPBlock.Except...)
			}
			if fromJSON.NamespaceSelector != nil {
				// For each peer namespace selector, we create a watcher that
				// populates ingress.peerAddressSets
				go oc.handlePeerNamespaceSelector(policy,
					fromJSON.NamespaceSelector, ingress, i, ingressPolicyType,
					np)
			}

			if fromJSON.PodSelector != nil {
				// For each peer pod selector, we create a watcher that
				// populates the addressSet
				go oc.handlePeerPodSelector(policy, fromJSON.PodSelector,
					hashedLocalAddressSet, np.stop)
			}
		}
		np.ingressPolicies = append(np.ingressPolicies, ingress)
	}

	// Go through each egress rule.  For each egress rule, create an
	// addressSet for the peer pods.
	for i, egressJSON := range policy.Spec.Egress {
		logrus.Debugf("Network policy egress is %+v", egressJSON)

		egress := &gressPolicy{}
		egress.peerAddressSets = make(map[string]bool)
		egress.sortedPeerAddressSets = make([]string, 0)

		// Each egress rule can have multiple ports to which we allow traffic.
		egress.portPolicies = make([]*portPolicy, 0)
		for _, portJSON := range egressJSON.Ports {
			port := &portPolicy{}
			port.protocol = string(*portJSON.Protocol)
			port.port = portJSON.Port.IntVal
			egress.portPolicies = append(egress.portPolicies, port)
		}

		hashedLocalAddressSet := ""
		if len(egressJSON.To) != 0 {
			// localPeerPods represents all the peer pods in the same
			// namespace to which we need to allow traffic.
			localPeerPods := fmt.Sprintf("%s.%s.%s.%d", policy.Namespace,
				policy.Name, "egress", i)

			hashedLocalAddressSet = hashedAddressSet(localPeerPods)
			egress.peerAddressSets[hashedLocalAddressSet] = true
			oc.createAddressSet(localPeerPods, hashedLocalAddressSet, nil)
			egress.sortedPeerAddressSets = append(
				egress.sortedPeerAddressSets, hashedLocalAddressSet)
		}

		for _, toJSON := range egressJSON.To {
			// Add IPBlock to egress network policy
			if toJSON.IPBlock != nil {
				egress.ipBlockCidr = toJSON.IPBlock.CIDR
				egress.ipBlockExcept = append([]string{},
					toJSON.IPBlock.Except...)
			}
			if toJSON.NamespaceSelector != nil {
				// For each peer namespace selector, we create a watcher that
				// populates egress.peerAddressSets
				go oc.handlePeerNamespaceSelector(policy,
					toJSON.NamespaceSelector, egress, i, egressPolicyType,
					np)
			}

			if toJSON.PodSelector != nil {
				// For each peer pod selector, we create a watcher that
				// populates the addressSet
				go oc.handlePeerPodSelector(policy, toJSON.PodSelector,
					hashedLocalAddressSet, np.stop)
			}
		}
		np.egressPolicies = append(np.egressPolicies, egress)
	}

	oc.namespacePolicies[policy.Namespace][policy.Name] = np

	// For all the pods in the local namespace that this policy
	// effects, add ACL rules.
	go oc.handleLocalPodSelector(policy, np)

	return
}

func (oc *Controller) getLogicalSwitchForLogicalPort(
	logicalPort string) string {
	if oc.logicalPortCache[logicalPort] != "" {
		return oc.logicalPortCache[logicalPort]
	}

	out, err := exec.Command(OvnNbctl, "get", "logical_switch_port",
		logicalPort, "external-ids:logical_switch").Output()
	if err != nil {
		logrus.Errorf("Error obtaining logical switch for %s",
			logicalPort)
		return ""
	}
	logicalSwitch := strings.TrimSpace(string(out))
	logicalSwitch = strings.Trim(logicalSwitch, `"`)
	if logicalSwitch == "" {
		logrus.Errorf("Error obtaining logical switch for %s",
			logicalPort)
		return ""
	}
	return logicalSwitch
}

func (oc *Controller) deleteNetworkPolicy(
	policy *kapisnetworking.NetworkPolicy) {
	logrus.Infof("Deleting network policy %s in namespace %s",
		policy.Name, policy.Namespace)

	if oc.namespacePolicies[policy.Namespace] == nil ||
		oc.namespacePolicies[policy.Namespace][policy.Name] == nil {
		return
	}
	np := oc.namespacePolicies[policy.Namespace][policy.Name]

	np.namespacePolicyMutex.Lock()
	defer np.namespacePolicyMutex.Unlock()

	// Go through each ingress rule.  For each ingress rule, delete the
	// addressSet for the local peer pods.
	for i := range np.ingressPolicies {
		localPeerPods := fmt.Sprintf("%s.%s.%s.%d", policy.Namespace,
			policy.Name, "ingress", i)
		hashedAddressSet := hashedAddressSet(localPeerPods)
		oc.deleteAddressSet(hashedAddressSet)
	}
	// Go through each egress rule.  For each egress rule, delete the
	// addressSet for the local peer pods.
	for i := range np.egressPolicies {
		localPeerPods := fmt.Sprintf("%s.%s.%s.%d", policy.Namespace,
			policy.Name, "egress", i)
		hashedAddressSet := hashedAddressSet(localPeerPods)
		oc.deleteAddressSet(hashedAddressSet)
	}

	// We should now stop all the go routines.
	close(np.stop)

	for logicalPort := range np.localPods {
		logicalSwitch := oc.getLogicalSwitchForLogicalPort(
			logicalPort)
		oc.localPodDelDefaultDeny(policy, logicalPort, logicalSwitch)
	}
	oc.namespacePolicies[policy.Namespace][policy.Name] = nil

	//TODO: We need to wait for all the goroutines to die to
	// prevent race conditions from a older goroutine adding
	// newer ACLs after we delete the ACLs below.

	// We should now delete all the ACLs added by this network policy.
	oc.deleteAclsPolicy(policy.Namespace, policy.Name)

	return
}
