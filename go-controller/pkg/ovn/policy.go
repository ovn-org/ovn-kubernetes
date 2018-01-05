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
	ingressPolicies      []*ingressPolicy
	stop                 chan bool
	namespacePolicyMutex *sync.Mutex
	localPods            map[string]bool //pods effected by this policy
}

type ingressPolicy struct {
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
}

type portPolicy struct {
	protocol string
	port     int32
}

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
		"priority=1001", "direction=to-lport", match, "action=allow-related",
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
	logicalPort, match string, ingressNum int) {
	uuid, err := exec.Command(OvnNbctl, "--data=bare", "--no-heading",
		"--columns=_uuid", "find", "ACL", match,
		fmt.Sprintf("external-ids:namespace=%s", namespace),
		fmt.Sprintf("external-ids:policy=%s", policy),
		fmt.Sprintf("external-ids:ingress_num=%d", ingressNum),
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
		"priority=1001", "direction=to-lport", match, "action=allow-related",
		fmt.Sprintf("external-ids:namespace=%s", namespace),
		fmt.Sprintf("external-ids:policy=%s", policy),
		fmt.Sprintf("external-ids:ingress_num=%d", ingressNum),
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
	oldMatch string, newMatch string, ingressNum int) {
	uuid, err := exec.Command(OvnNbctl, "--data=bare", "--no-heading",
		"--columns=_uuid", "find", "ACL", oldMatch,
		fmt.Sprintf("external-ids:namespace=%s", namespace),
		fmt.Sprintf("external-ids:policy=%s", policy),
		fmt.Sprintf("external-ids:ingress_num=%d", ingressNum),
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
	logicalPort, match string, ingressNum int) {
	uuid, err := exec.Command(OvnNbctl, "--data=bare", "--no-heading",
		"--columns=_uuid", "find", "ACL", match,
		fmt.Sprintf("external-ids:namespace=%s", namespace),
		fmt.Sprintf("external-ids:policy=%s", policy),
		fmt.Sprintf("external-ids:ingress_num=%d", ingressNum),
		fmt.Sprintf("external-ids:logical_switch=%s", logicalSwitch),
		fmt.Sprintf("external-ids:logical_port=%s", logicalPort)).Output()
	if err != nil {
		logrus.Errorf("find failed to get the allow rule for "+
			"namespace=%s, logical_port=%s (%v)", namespace, logicalPort, err)
		return
	}

	if string(uuid) == "" {
		logrus.Infof("deleteACLAllow: returning because find failed")
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

func (oc *Controller) addACLDeny(namespace, logicalSwitch,
	logicalPort string) {
	match := fmt.Sprintf("match=\"outport == \\\"%s\\\"\"", logicalPort)

	uuid, err := exec.Command(OvnNbctl, "--data=bare", "--no-heading",
		"--columns=_uuid", "find", "ACL", match, "action=drop",
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
		"priority=1000",
		"direction=to-lport", match, "action=drop",
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

func (oc *Controller) deleteACLDeny(namespace, logicalSwitch,
	logicalPort string) {
	match := fmt.Sprintf("match=\"outport == \\\"%s\\\"\"", logicalPort)

	uuid, err := exec.Command(OvnNbctl, "--data=bare", "--no-heading",
		"--columns=_uuid", "find", "ACL", match, "action=drop",
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
		logrus.Debugf("deleteAclsPolicy: returning because find failed")
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

func getL3MatchFromAddressSet(addressSets []string) string {
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
		l3Match = fmt.Sprintf("ip4.src == {%s}", addresses)
	}
	return l3Match
}

func (oc *Controller) handleLocalPodSelectorAddFunc(
	policy *kapisnetworking.NetworkPolicy, np *namespacePolicy,
	obj interface{}) {
	pod := obj.(*kapi.Pod)
	if pod.Status.PodIP == "" {
		return
	}

	logicalSwitch := pod.Spec.NodeName
	if logicalSwitch == "" {
		return
	}

	// Get the logical port name.
	logicalPort := fmt.Sprintf("%s_%s", pod.Namespace, pod.Name)

	oc.lspMutex.Lock()
	if oc.lspIngressDenyCache[logicalPort] == 0 {
		oc.addACLDeny(pod.Namespace, logicalSwitch, logicalPort)
	}
	oc.lspIngressDenyCache[logicalPort]++
	oc.lspMutex.Unlock()

	np.namespacePolicyMutex.Lock()
	defer np.namespacePolicyMutex.Unlock()

	np.localPods[logicalPort] = true

	// For each ingress rule, add a ACL
	for i, ingress := range np.ingressPolicies {
		l3Match := getL3MatchFromAddressSet(ingress.sortedPeerAddressSets)
		outputMatch := fmt.Sprintf("outport == \\\"%s\\\"", logicalPort)
		if len(ingress.portPolicies) == 0 {
			match := fmt.Sprintf("match=\"%s && %s\"", l3Match,
				outputMatch)

			oc.addACLAllow(policy.Namespace, policy.Name,
				pod.Spec.NodeName, logicalPort, match, i)
		}
		for _, port := range ingress.portPolicies {
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
				l3Match, l4Match, outputMatch)
			oc.addACLAllow(policy.Namespace, policy.Name,
				pod.Spec.NodeName, logicalPort, match, i)
		}
	}
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

	if np.localPods[logicalPort] {
		np.localPods[logicalPort] = false
	}

	oc.lspMutex.Lock()
	oc.lspIngressDenyCache[logicalPort]--
	if oc.lspIngressDenyCache[logicalPort] == 0 {
		oc.deleteACLDeny(pod.Namespace, logicalSwitch, logicalPort)
	}
	oc.lspMutex.Unlock()

	// For each ingress rule, remove the ACL
	for i, ingress := range np.ingressPolicies {
		l3Match := getL3MatchFromAddressSet(ingress.sortedPeerAddressSets)
		outputMatch := fmt.Sprintf("outport == \\\"%s\\\"", logicalPort)
		if len(ingress.portPolicies) == 0 {
			match := fmt.Sprintf("match=\"%s && %s\"", l3Match,
				outputMatch)
			oc.deleteACLAllow(policy.Namespace, policy.Name,
				pod.Spec.NodeName, logicalPort, match, i)
		}
		for _, port := range ingress.portPolicies {
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
				l3Match, l4Match, outputMatch)
			oc.deleteACLAllow(policy.Namespace, policy.Name,
				pod.Spec.NodeName, logicalPort, match, i)
		}
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
				if pod.Status.PodIP == "" ||
					addressMap[pod.Status.PodIP] {
					return
				}
				addressMap[pod.Status.PodIP] = true
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
				if pod.Status.PodIP == "" ||
					addressMap[pod.Status.PodIP] {
					return
				}
				addressMap[pod.Status.PodIP] = true
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
}

func (oc *Controller) handlePeerNamespaceSelectorModify(
	namespace *kapi.Namespace, ingress *ingressPolicy, ingressNum int,
	np *namespacePolicy, oldl3Match string, newl3Match string) {

	for logicalPort := range np.localPods {
		outputMatch := fmt.Sprintf("outport == \\\"%s\\\"", logicalPort)
		if len(ingress.portPolicies) == 0 {
			oldMatch := fmt.Sprintf("match=\"%s && %s\"", oldl3Match,
				outputMatch)
			newMatch := fmt.Sprintf("match=\"%s && %s\"", newl3Match,
				outputMatch)
			oc.modifyACLAllow(np.namespace, np.name, logicalPort,
				oldMatch, newMatch, ingressNum)
		}
		for _, port := range ingress.portPolicies {
			var l4Match string
			if port.protocol == TCP {
				l4Match = fmt.Sprintf("tcp && tcp.dst==%d", port.port)
			} else if port.protocol == UDP {
				l4Match = fmt.Sprintf("udp && udp.dst==%d", port.port)
			} else {
				continue
			}
			oldMatch := fmt.Sprintf("match=\"%s && %s && %s\"",
				oldl3Match, l4Match, outputMatch)
			newMatch := fmt.Sprintf("match=\"%s && %s && %s\"",
				newl3Match, l4Match, outputMatch)
			oc.modifyACLAllow(np.namespace, np.name, logicalPort,
				oldMatch, newMatch, ingressNum)
		}
	}
}

func (oc *Controller) handlePeerNamespaceSelector(
	policy *kapisnetworking.NetworkPolicy,
	namespaceSelector *metav1.LabelSelector,
	ingress *ingressPolicy, ingressNum int, np *namespacePolicy) {

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
				if ingress.peerAddressSets[hashedAddressSet] {
					return
				}

				oldL3Match := getL3MatchFromAddressSet(
					ingress.sortedPeerAddressSets)

				ingress.sortedPeerAddressSets = append(
					ingress.sortedPeerAddressSets, hashedAddressSet)
				sort.Strings(ingress.sortedPeerAddressSets)

				newL3Match := getL3MatchFromAddressSet(
					ingress.sortedPeerAddressSets)

				oc.handlePeerNamespaceSelectorModify(namespace, ingress,
					ingressNum, np, oldL3Match, newL3Match)
				ingress.peerAddressSets[hashedAddressSet] = true

			},
			DeleteFunc: func(obj interface{}) {
				namespace := obj.(*kapi.Namespace)
				np.namespacePolicyMutex.Lock()
				defer np.namespacePolicyMutex.Unlock()
				hashedAddressSet := hashedAddressSet(namespace.Name)
				if !ingress.peerAddressSets[hashedAddressSet] {
					return
				}
				oldL3Match := getL3MatchFromAddressSet(
					ingress.sortedPeerAddressSets)

				for i, addressSet := range ingress.sortedPeerAddressSets {
					if addressSet == hashedAddressSet {
						ingress.sortedPeerAddressSets = append(
							ingress.sortedPeerAddressSets[:i],
							ingress.sortedPeerAddressSets[i+1:]...)
						break
					}
				}

				newL3Match := getL3MatchFromAddressSet(
					ingress.sortedPeerAddressSets)

				oc.handlePeerNamespaceSelectorModify(namespace, ingress,
					ingressNum, np, oldL3Match, newL3Match)

				delete(ingress.peerAddressSets, hashedAddressSet)
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
	np.ingressPolicies = make([]*ingressPolicy, 0)
	np.stop = make(chan bool, 1)
	np.localPods = make(map[string]bool)
	np.namespacePolicyMutex = &sync.Mutex{}

	// Go through each ingress rule.  For each ingress rule, create an
	// addressSet for the peer pods.
	for i, ingressJSON := range policy.Spec.Ingress {
		logrus.Debugf("Network policy ingress is %+v", ingressJSON)

		ingress := &ingressPolicy{}
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
			if fromJSON.NamespaceSelector != nil {
				// For each peer namespace selector, we create a watcher that
				// populates ingress.peerAddressSets
				go oc.handlePeerNamespaceSelector(policy,
					fromJSON.NamespaceSelector, ingress, i,
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

	oc.namespacePolicies[policy.Namespace][policy.Name] = np

	// For all the pods in the local namespace that this policy
	// effects, add ACL rules.
	go oc.handleLocalPodSelector(policy, np)

	return
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

	// We should now stop all the go routines.
	close(np.stop)

	oc.namespacePolicies[policy.Namespace][policy.Name] = nil

	//TODO: We need to wait for all the goroutines to die to
	// prevent race conditions from a older goroutine adding
	// newer ACLs after we delete the ACLs below.

	// We should now delete all the ACLs added by this network policy.
	oc.deleteAclsPolicy(policy.Namespace, policy.Name)

	return
}
