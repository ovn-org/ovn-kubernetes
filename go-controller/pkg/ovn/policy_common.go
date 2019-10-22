package ovn

import (
	"fmt"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/sirupsen/logrus"
	kapi "k8s.io/api/core/v1"
	knet "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
	"net"
	"sort"
	"strings"
	"sync"
)

type namespacePolicy struct {
	sync.Mutex
	name            string
	namespace       string
	ingressPolicies []*gressPolicy
	egressPolicies  []*gressPolicy
	podHandlerList  []*factory.Handler
	nsHandlerList   []*factory.Handler
	localPods       map[string]bool //pods effected by this policy
	portGroupUUID   string          //uuid for OVN port_group
	portGroupName   string
	deleted         bool //deleted policy
}

type gressPolicy struct {
	policyType knet.PolicyType
	idx        int

	// peerAddressSets points to all the addressSets that hold
	// the peer pod's IP addresses. We will have one addressSet for
	// local pods and multiple addressSets that each represent a
	// peer namespace
	peerAddressSets map[string]bool

	// sortedPeerAddressSets has the sorted peerAddressSets
	sortedPeerAddressSets []string

	// portPolicies represents all the ports to which traffic is allowed for
	// the rule in question.
	portPolicies []*portPolicy

	// ipBlockCidr represents the CIDR from which traffic is allowed
	// except the IP block in the except, which should be dropped.
	ipBlockCidr   []string
	ipBlockExcept []string
}

type portPolicy struct {
	protocol string
	port     int32
}

func (pp *portPolicy) getL4Match() (string, error) {
	if pp.protocol == TCP {
		return fmt.Sprintf("tcp && tcp.dst==%d", pp.port), nil
	} else if pp.protocol == UDP {
		return fmt.Sprintf("udp && udp.dst==%d", pp.port), nil
	}
	return "", fmt.Errorf("unknown port protocol %v", pp.protocol)
}

func newGressPolicy(policyType knet.PolicyType, idx int) *gressPolicy {
	return &gressPolicy{
		policyType:            policyType,
		idx:                   idx,
		peerAddressSets:       make(map[string]bool),
		sortedPeerAddressSets: make([]string, 0),
		portPolicies:          make([]*portPolicy, 0),
		ipBlockCidr:           make([]string, 0),
		ipBlockExcept:         make([]string, 0),
	}
}

func (gp *gressPolicy) addPortPolicy(portJSON *knet.NetworkPolicyPort) {
	gp.portPolicies = append(gp.portPolicies, &portPolicy{
		protocol: string(*portJSON.Protocol),
		port:     portJSON.Port.IntVal,
	})
}

func (gp *gressPolicy) addIPBlock(ipblockJSON *knet.IPBlock) {
	gp.ipBlockCidr = append(gp.ipBlockCidr, ipblockJSON.CIDR)
	gp.ipBlockExcept = append(gp.ipBlockExcept, ipblockJSON.Except...)
}

func (gp *gressPolicy) getL3MatchFromAddressSet() string {
	var l3Match, addresses string
	for _, addressSet := range gp.sortedPeerAddressSets {
		if addresses == "" {
			addresses = fmt.Sprintf("$%s", addressSet)
			continue
		}
		addresses = fmt.Sprintf("%s, $%s", addresses, addressSet)
	}
	if addresses == "" {
		l3Match = "ip4"
	} else {
		if gp.policyType == knet.PolicyTypeIngress {
			l3Match = fmt.Sprintf("ip4.src == {%s}", addresses)
		} else {
			l3Match = fmt.Sprintf("ip4.dst == {%s}", addresses)
		}
	}
	return l3Match
}

func (gp *gressPolicy) getMatchFromIPBlock(lportMatch, l4Match string) string {
	var match string
	ipBlockCidr := fmt.Sprintf("{%s}", strings.Join(gp.ipBlockCidr, ", "))
	if gp.policyType == knet.PolicyTypeIngress {
		if l4Match == noneMatch {
			match = fmt.Sprintf("match=\"ip4.src == %s && %s\"",
				ipBlockCidr, lportMatch)
		} else {
			match = fmt.Sprintf("match=\"ip4.src == %s && %s && %s\"",
				ipBlockCidr, l4Match, lportMatch)
		}
	} else {
		if l4Match == noneMatch {
			match = fmt.Sprintf("match=\"ip4.dst == %s && %s\"",
				ipBlockCidr, lportMatch)
		} else {
			match = fmt.Sprintf("match=\"ip4.dst == %s && %s && %s\"",
				ipBlockCidr, l4Match, lportMatch)
		}
	}
	return match
}

func (gp *gressPolicy) addAddressSet(hashedAddressSet string) (string, string, bool) {
	if gp.peerAddressSets[hashedAddressSet] {
		return "", "", false
	}

	oldL3Match := gp.getL3MatchFromAddressSet()

	gp.sortedPeerAddressSets = append(gp.sortedPeerAddressSets, hashedAddressSet)
	sort.Strings(gp.sortedPeerAddressSets)
	gp.peerAddressSets[hashedAddressSet] = true

	return oldL3Match, gp.getL3MatchFromAddressSet(), true
}

func (gp *gressPolicy) delAddressSet(hashedAddressSet string) (string, string, bool) {
	if !gp.peerAddressSets[hashedAddressSet] {
		return "", "", false
	}

	oldL3Match := gp.getL3MatchFromAddressSet()

	for i, addressSet := range gp.sortedPeerAddressSets {
		if addressSet == hashedAddressSet {
			gp.sortedPeerAddressSets = append(
				gp.sortedPeerAddressSets[:i],
				gp.sortedPeerAddressSets[i+1:]...)
			break
		}
	}
	delete(gp.peerAddressSets, hashedAddressSet)

	return oldL3Match, gp.getL3MatchFromAddressSet(), true
}

// handlePeerPodSelectorAddUpdate adds the IP address of a pod that has been
// selected as a peer by a NetworkPolicy's ingress/egress section to that
// ingress/egress address set
func (oc *Controller) handlePeerPodSelectorAddUpdate(np *namespacePolicy,
	addressMap map[string]bool, addressSet string, obj interface{}) {

	pod := obj.(*kapi.Pod)
	podAnnotation, err := util.UnmarshalPodAnnotation(pod.Annotations["ovn"])
	if err != nil {
		return
	}
	ipAddress := podAnnotation.IP.IP.String()
	if addressMap[ipAddress] {
		return
	}

	np.Lock()
	defer np.Unlock()
	if np.deleted {
		return
	}

	addressMap[ipAddress] = true
	addresses := make([]string, 0, len(addressMap))
	for k := range addressMap {
		addresses = append(addresses, k)
	}
	oc.setAddressSet(addressSet, addresses)

}

// handlePeerPodSelectorDelete removes the IP address of a pod that no longer
// matches a NetworkPolicy ingress/egress section's selectors from that
// ingress/egress address set
func (oc *Controller) handlePeerPodSelectorDelete(np *namespacePolicy,
	addressMap map[string]bool, addressSet string, obj interface{}) {

	pod := obj.(*kapi.Pod)
	podAnnotation, err := util.UnmarshalPodAnnotation(pod.Annotations["ovn"])
	if err != nil {
		return
	}
	ipAddress := podAnnotation.IP.IP.String()

	np.Lock()
	defer np.Unlock()
	if np.deleted {
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
}

func (oc *Controller) handlePeerPodSelector(
	policy *knet.NetworkPolicy, podSelector *metav1.LabelSelector,
	addressSet string, addressMap map[string]bool, np *namespacePolicy) {

	h, err := oc.watchFactory.AddFilteredPodHandler(policy.Namespace,
		podSelector,
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				oc.handlePeerPodSelectorAddUpdate(np, addressMap, addressSet, obj)
			},
			DeleteFunc: func(obj interface{}) {
				oc.handlePeerPodSelectorDelete(np, addressMap, addressSet, obj)
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				oc.handlePeerPodSelectorAddUpdate(np, addressMap, addressSet, newObj)
			},
		}, nil)
	if err != nil {
		logrus.Errorf("error watching peer pods for policy %s in namespace %s: %v",
			policy.Name, policy.Namespace, err)
		return
	}

	np.podHandlerList = append(np.podHandlerList, h)
}

func (oc *Controller) handlePeerNamespaceAndPodSelector(
	policy *knet.NetworkPolicy,
	namespaceSelector *metav1.LabelSelector,
	podSelector *metav1.LabelSelector,
	addressSet string,
	addressMap map[string]bool,
	np *namespacePolicy) {

	namespaceHandler, err := oc.watchFactory.AddFilteredNamespaceHandler("",
		namespaceSelector,
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				namespace := obj.(*kapi.Namespace)
				np.Lock()
				alreadyDeleted := np.deleted
				np.Unlock()
				if alreadyDeleted {
					return
				}

				// The AddFilteredPodHandler call might call handlePeerPodSelectorAddUpdate
				// on existing pods so we can't be holding the lock at this point
				podHandler, err := oc.watchFactory.AddFilteredPodHandler(namespace.Name,
					podSelector,
					cache.ResourceEventHandlerFuncs{
						AddFunc: func(obj interface{}) {
							oc.handlePeerPodSelectorAddUpdate(np, addressMap, addressSet, obj)
						},
						DeleteFunc: func(obj interface{}) {
							oc.handlePeerPodSelectorDelete(np, addressMap, addressSet, obj)
						},
						UpdateFunc: func(oldObj, newObj interface{}) {
							oc.handlePeerPodSelectorAddUpdate(np, addressMap, addressSet, newObj)
						},
					}, nil)
				if err != nil {
					logrus.Errorf("error watching pods in namespace %s for policy %s: %v", namespace.Name, policy.Name, err)
					return
				}
				np.Lock()
				defer np.Unlock()
				if np.deleted {
					_ = oc.watchFactory.RemovePodHandler(podHandler)
					return
				}
				np.podHandlerList = append(np.podHandlerList, podHandler)
			},
			DeleteFunc: func(obj interface{}) {
				return
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				return
			},
		}, nil)
	if err != nil {
		logrus.Errorf("error watching namespaces for policy %s: %v",
			policy.Name, err)
		return
	}
	np.nsHandlerList = append(np.nsHandlerList, namespaceHandler)
}

type peerNamespaceSelectorModifyFn func(*gressPolicy, *namespacePolicy, string, string)

func (oc *Controller) handlePeerNamespaceSelector(
	policy *knet.NetworkPolicy,
	namespaceSelector *metav1.LabelSelector,
	gress *gressPolicy, np *namespacePolicy,
	modifyFn peerNamespaceSelectorModifyFn) {

	h, err := oc.watchFactory.AddFilteredNamespaceHandler("",
		namespaceSelector,
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				namespace := obj.(*kapi.Namespace)
				np.Lock()
				defer np.Unlock()
				if np.deleted {
					return
				}
				hashedAddressSet := hashedAddressSet(namespace.Name)
				oldL3Match, newL3Match, added := gress.addAddressSet(hashedAddressSet)
				if added {
					modifyFn(gress, np, oldL3Match, newL3Match)
				}
			},
			DeleteFunc: func(obj interface{}) {
				namespace := obj.(*kapi.Namespace)
				np.Lock()
				defer np.Unlock()
				if np.deleted {
					return
				}
				hashedAddressSet := hashedAddressSet(namespace.Name)
				oldL3Match, newL3Match, removed := gress.delAddressSet(hashedAddressSet)
				if removed {
					modifyFn(gress, np, oldL3Match, newL3Match)
				}
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				return
			},
		}, nil)
	if err != nil {
		logrus.Errorf("error watching namespaces for policy %s: %v",
			policy.Name, err)
		return
	}

	np.nsHandlerList = append(np.nsHandlerList, h)
}

const (
	toLport   = "to-lport"
	addACL    = "add"
	deleteACL = "delete"
	noneMatch = "None"
	// Default deny acl rule priority
	defaultDenyPriority = "1000"
	// Default allow acl rule priority
	defaultAllowPriority = "1001"
	// IP Block except deny acl rule priority
	ipBlockDenyPriority = "1010"
)

func (oc *Controller) addAllowACLFromNode(logicalSwitch string) error {
	subnet, stderr, err := util.RunOVNNbctl("get", "logical_switch",
		logicalSwitch, "other-config:subnet")
	if err != nil {
		logrus.Errorf("failed to get the logical_switch %s subnet, "+
			"stderr: %q (%v)", logicalSwitch, stderr, err)
		return err
	}

	if subnet == "" {
		return fmt.Errorf("logical_switch %q had no subnet", logicalSwitch)
	}

	ip, _, err := net.ParseCIDR(subnet)
	if err != nil {
		logrus.Errorf("failed to parse subnet %s", subnet)
		return err
	}

	// K8s only supports IPv4 right now. The second IP address of the
	// network is the node IP address.
	ip = ip.To4()
	ip[3] = ip[3] + 2
	address := ip.String()

	match := fmt.Sprintf("ip4.src==%s", address)
	_, stderr, err = util.RunOVNNbctl("--may-exist", "acl-add", logicalSwitch,
		"to-lport", defaultAllowPriority, match, "allow-related")
	if err != nil {
		logrus.Errorf("failed to create the node acl for "+
			"logical_switch=%s, stderr: %q (%v)", logicalSwitch, stderr, err)
	}

	return err
}

func (oc *Controller) syncNetworkPolicies(networkPolicies []interface{}) {
	if oc.portGroupSupport {
		oc.syncNetworkPoliciesPortGroup(networkPolicies)
	} else {
		oc.syncNetworkPoliciesOld(networkPolicies)
	}
}

// AddNetworkPolicy creates and applies OVN ACLs to pod logical switch ports
// from Kubernetes NetworkPolicy objects
func (oc *Controller) addNetworkPolicy(policy *knet.NetworkPolicy) {
	if oc.portGroupSupport {
		oc.addNetworkPolicyPortGroup(policy)
	} else {
		oc.addNetworkPolicyOld(policy)
	}
}

func (oc *Controller) deleteNetworkPolicy(
	policy *knet.NetworkPolicy) {
	if oc.portGroupSupport {
		oc.deleteNetworkPolicyPortGroup(policy)
	} else {
		oc.deleteNetworkPolicyOld(policy)
	}

}

func (oc *Controller) shutdownHandlers(np *namespacePolicy) {
	for _, handler := range np.podHandlerList {
		_ = oc.watchFactory.RemovePodHandler(handler)
	}
	for _, handler := range np.nsHandlerList {
		_ = oc.watchFactory.RemoveNamespaceHandler(handler)
	}
}
