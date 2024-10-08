package ovn

import (
	"context"
	"fmt"
	"runtime"
	"time"

	"github.com/onsi/ginkgo/v2"

	"github.com/onsi/gomega"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"

	v1 "k8s.io/api/core/v1"
	knet "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func getPolicyKeyWithKind(policy *knet.NetworkPolicy) string {
	return fmt.Sprintf("%v/%v/%v", "NetworkPolicy", policy.Namespace, policy.Name)
}

func eventuallyExpectAddressSetsWithIP(fakeOvn *FakeOVN, peer knet.NetworkPolicyPeer, namespace, ip string) {
	if peer.PodSelector != nil {
		dbIDs := getPodSelectorAddrSetDbIDs(getPodSelectorKey(peer.PodSelector, peer.NamespaceSelector, namespace), DefaultNetworkControllerName)
		fakeOvn.asf.EventuallyExpectAddressSetWithAddresses(dbIDs, []string{ip})
	}
}

func eventuallyExpectEmptyAddressSetsExist(fakeOvn *FakeOVN, peer knet.NetworkPolicyPeer, namespace string) {
	if peer.PodSelector != nil {
		dbIDs := getPodSelectorAddrSetDbIDs(getPodSelectorKey(peer.PodSelector, peer.NamespaceSelector, namespace), DefaultNetworkControllerName)
		fakeOvn.asf.EventuallyExpectEmptyAddressSetExist(dbIDs)
	}
}

var _ = ginkgo.Describe("OVN PodSelectorAddressSet", func() {
	const (
		namespaceName1 = "namespace1"
		namespaceName2 = "namespace2"
		netPolicyName1 = "networkpolicy1"
		netPolicyName2 = "networkpolicy2"
		nodeName       = "node1"
		podLabelKey    = "podLabel"
		ip1            = "10.128.1.1"
		ip2            = "10.128.1.2"
		ip3            = "10.128.1.3"
		ip4            = "10.128.1.4"
	)
	var (
		fakeOvn   *FakeOVN
		initialDB libovsdbtest.TestSetup
	)

	ginkgo.BeforeEach(func() {
		// Restore global default values before each testcase
		config.PrepareTestConfig()
		fakeOvn = NewFakeOVN(true)
		initialData := getHairpinningACLsV4AndPortGroup()
		initialDB = libovsdbtest.TestSetup{
			NBData: initialData,
		}
	})

	ginkgo.AfterEach(func() {
		if fakeOvn.watcher != nil {
			// fakeOVN was inited
			fakeOvn.shutdown()
		}
	})

	startOvn := func(dbSetup libovsdbtest.TestSetup, namespaces []v1.Namespace, networkPolicies []knet.NetworkPolicy,
		pods []testPod, podLabels map[string]string) {
		var podsList []v1.Pod
		for _, testPod := range pods {
			knetPod := newPod(testPod.namespace, testPod.podName, testPod.nodeName, testPod.podIP)
			if len(podLabels) > 0 {
				knetPod.Labels = podLabels
			}
			podsList = append(podsList, *knetPod)
		}
		fakeOvn.startWithDBSetup(dbSetup,
			&v1.NamespaceList{
				Items: namespaces,
			},
			&v1.PodList{
				Items: podsList,
			},
			&knet.NetworkPolicyList{
				Items: networkPolicies,
			},
		)
		for _, testPod := range pods {
			testPod.populateLogicalSwitchCache(fakeOvn)
		}
		var err error
		if namespaces != nil {
			err = fakeOvn.controller.WatchNamespaces()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}
		if pods != nil {
			err = fakeOvn.controller.WatchPods()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}
		err = fakeOvn.controller.WatchNetworkPolicy()
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	}

	ginkgo.It("validates selectors", func() {
		// start ovn without any objects
		startOvn(initialDB, nil, nil, nil, nil)
		namespace := *newNamespace(namespaceName1)
		networkPolicy := getMatchLabelsNetworkPolicy(netPolicyName1, namespace.Name,
			"", "label1", true, true)
		// create peer with invalid Operator
		peer := knet.NetworkPolicyPeer{
			PodSelector: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{{
					Key:      "key",
					Operator: "",
					Values:   []string{"value"},
				}},
			},
		}
		// try to add invalid peer
		peerASKey, _, _, err := fakeOvn.controller.EnsurePodSelectorAddressSet(
			peer.PodSelector, peer.NamespaceSelector, networkPolicy.Namespace, getPolicyKeyWithKind(networkPolicy))
		// error should happen on handler add
		gomega.Expect(err.Error()).To(gomega.ContainSubstring("is not a valid label selector operator"))
		// address set will not be created
		peerASIDs := getPodSelectorAddrSetDbIDs(peerASKey, DefaultNetworkControllerName)
		fakeOvn.asf.EventuallyExpectNoAddressSet(peerASIDs)

		// add nil pod selector
		peerASKey, _, _, err = fakeOvn.controller.EnsurePodSelectorAddressSet(
			nil, peer.NamespaceSelector, networkPolicy.Namespace, getPolicyKeyWithKind(networkPolicy))
		// error should happen on handler add
		gomega.Expect(err.Error()).To(gomega.ContainSubstring("pod selector is nil"))
		// address set will not be created
		peerASIDs = getPodSelectorAddrSetDbIDs(peerASKey, DefaultNetworkControllerName)
		fakeOvn.asf.EventuallyExpectNoAddressSet(peerASIDs)

		// namespace selector is nil and namespace is empty
		peerASKey, _, _, err = fakeOvn.controller.EnsurePodSelectorAddressSet(
			peer.PodSelector, nil, "", getPolicyKeyWithKind(networkPolicy))
		// error should happen on handler add
		gomega.Expect(err.Error()).To(gomega.ContainSubstring("namespace selector is nil and namespace is empty"))
		// address set will not be created
		peerASIDs = getPodSelectorAddrSetDbIDs(peerASKey, DefaultNetworkControllerName)
		fakeOvn.asf.EventuallyExpectNoAddressSet(peerASIDs)
	})
	ginkgo.It("creates one address set for multiple users with the same selector", func() {
		namespace1 := *newNamespace(namespaceName1)
		networkPolicy1 := getMatchLabelsNetworkPolicy(netPolicyName1, namespace1.Name,
			"", "label1", true, true)
		networkPolicy2 := getMatchLabelsNetworkPolicy(netPolicyName2, namespace1.Name,
			"", "label1", true, true)
		startOvn(initialDB, []v1.Namespace{namespace1}, []knet.NetworkPolicy{*networkPolicy1, *networkPolicy2},
			nil, nil)

		fakeOvn.asf.ExpectEmptyAddressSet(namespaceName1)
		peer := networkPolicy1.Spec.Ingress[0].From[0]
		peerASKey := getPodSelectorKey(peer.PodSelector, peer.NamespaceSelector, namespace1.Name)
		peerASIDs := getPodSelectorAddrSetDbIDs(peerASKey, DefaultNetworkControllerName)
		fakeOvn.asf.EventuallyExpectEmptyAddressSetExist(peerASIDs)
		// expect namespace and peer address sets only
		fakeOvn.asf.ExpectNumberOfAddressSets(2)
	})
	ginkgo.DescribeTable("adds selected pod ips to the address set",
		func(peer knet.NetworkPolicyPeer, staticNamespace string, addrSetIPs []string) {
			namespace1 := *newNamespace(namespaceName1)
			namespace2 := *newNamespace(namespaceName2)
			ns1pod1 := newPod(namespace1.Name, "ns1pod1", nodeName, ip1)
			ns1pod2 := newPod(namespace1.Name, "ns1pod2", nodeName, ip2)
			ns2pod1 := newPod(namespace2.Name, "ns2pod1", nodeName, ip3)
			ns2pod2 := newPod(namespace2.Name, "ns2pod2", nodeName, ip4)
			podsList := []v1.Pod{}
			for _, pod := range []*v1.Pod{ns1pod1, ns1pod2, ns2pod1, ns2pod2} {
				pod.Labels = map[string]string{podLabelKey: pod.Name}
				podsList = append(podsList, *pod)
			}
			fakeOvn.startWithDBSetup(libovsdbtest.TestSetup{},
				&v1.NamespaceList{
					Items: []v1.Namespace{namespace1, namespace2},
				},
				&v1.PodList{
					Items: podsList,
				},
			)

			peerASKey, _, _, err := fakeOvn.controller.EnsurePodSelectorAddressSet(
				peer.PodSelector, peer.NamespaceSelector, staticNamespace, "backRef")
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			// address set should be created and pod ips added
			peerASIDs := getPodSelectorAddrSetDbIDs(peerASKey, DefaultNetworkControllerName)
			fakeOvn.asf.ExpectAddressSetWithAddresses(peerASIDs, addrSetIPs)
		},
		ginkgo.Entry("all pods from a static namespace", knet.NetworkPolicyPeer{
			PodSelector:       &metav1.LabelSelector{},
			NamespaceSelector: nil,
		}, namespaceName1, []string{ip1, ip2}),
		ginkgo.Entry("selected pods from a static namespace", knet.NetworkPolicyPeer{
			PodSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{podLabelKey: "ns1pod1"},
			},
			NamespaceSelector: nil,
		}, namespaceName1, []string{ip1}),
		ginkgo.Entry("all pods from all namespaces", knet.NetworkPolicyPeer{
			PodSelector:       &metav1.LabelSelector{},
			NamespaceSelector: &metav1.LabelSelector{},
		}, namespaceName1, []string{ip1, ip2, ip3, ip4}),
		ginkgo.Entry("selected pods from all namespaces", knet.NetworkPolicyPeer{
			PodSelector: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{
						Key:      podLabelKey,
						Operator: metav1.LabelSelectorOpIn,
						Values:   []string{"ns1pod1", "ns2pod1"},
					},
				},
			},
			NamespaceSelector: &metav1.LabelSelector{},
		}, namespaceName1, []string{ip1, ip3}),
		ginkgo.Entry("all pods from selected namespaces", knet.NetworkPolicyPeer{
			PodSelector: &metav1.LabelSelector{},
			NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"name": namespaceName2,
				},
			},
		}, namespaceName1, []string{ip3, ip4}),
		ginkgo.Entry("selected pods from selected namespaces", knet.NetworkPolicyPeer{
			PodSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{podLabelKey: "ns2pod1"},
			},
			NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"name": namespaceName2,
				},
			},
		}, namespaceName1, []string{ip3}),
	)
	ginkgo.It("is cleaned up with DeletePodSelectorAddressSet call", func() {
		// start ovn without any objects
		startOvn(initialDB, nil, nil, nil, nil)
		namespace := *newNamespace(namespaceName1)
		networkPolicy := getMatchLabelsNetworkPolicy(netPolicyName1, namespace.Name,
			"", "label1", true, true)
		peer := networkPolicy.Spec.Ingress[0].From[0]

		// make asf return error on the next NewAddressSet call
		fakeOvn.asf.ErrOnNextNewASCall()
		peerASKey, _, _, err := fakeOvn.controller.EnsurePodSelectorAddressSet(
			peer.PodSelector, peer.NamespaceSelector, networkPolicy.Namespace, getPolicyKeyWithKind(networkPolicy))
		// error should happen on address set add
		gomega.Expect(err.Error()).To(gomega.ContainSubstring(addressset.FakeASFError))
		// address set should not be created
		peerASIDs := getPodSelectorAddrSetDbIDs(peerASKey, DefaultNetworkControllerName)
		fakeOvn.asf.EventuallyExpectNoAddressSet(peerASIDs)
		// peerAddressSet should be present in the map with needsCleanup=true
		asObj, loaded := fakeOvn.controller.podSelectorAddressSets.Load(peerASKey)
		gomega.Expect(loaded).To(gomega.BeTrue())
		gomega.Expect(asObj.needsCleanup).To(gomega.BeTrue())
		// delete invalid peer, check all resources are cleaned up
		err = fakeOvn.controller.DeletePodSelectorAddressSet(peerASKey, getPolicyKeyWithKind(networkPolicy))
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		// address set should still not exist
		fakeOvn.asf.EventuallyExpectNoAddressSet(peerASIDs)
		fakeOvn.asf.ExpectNumberOfAddressSets(0)
		// peerAddressSet should be deleted from the map
		_, loaded = fakeOvn.controller.podSelectorAddressSets.Load(peerASKey)
		gomega.Expect(loaded).To(gomega.BeFalse())
	})
	ginkgo.It("is cleaned up with second GetPodSelectorAddressSet call", func() {
		// start ovn without any objects
		startOvn(initialDB, nil, nil, nil, nil)
		namespace := *newNamespace(namespaceName1)
		networkPolicy := getMatchLabelsNetworkPolicy(netPolicyName1, namespace.Name,
			"", "label1", true, true)
		peer := networkPolicy.Spec.Ingress[0].From[0]

		// make asf return error on the next NewAddressSet call
		fakeOvn.asf.ErrOnNextNewASCall()
		peerASKey, _, _, err := fakeOvn.controller.EnsurePodSelectorAddressSet(
			peer.PodSelector, peer.NamespaceSelector, networkPolicy.Namespace, getPolicyKeyWithKind(networkPolicy))
		// error should happen on address set add
		gomega.Expect(err.Error()).To(gomega.ContainSubstring(addressset.FakeASFError))
		// address set should not be created
		peerASIDs := getPodSelectorAddrSetDbIDs(peerASKey, DefaultNetworkControllerName)
		fakeOvn.asf.EventuallyExpectNoAddressSet(peerASIDs)
		// peerAddressSet should be present in the map with needsCleanup=true
		asObj, loaded := fakeOvn.controller.podSelectorAddressSets.Load(peerASKey)
		gomega.Expect(loaded).To(gomega.BeTrue())
		gomega.Expect(asObj.needsCleanup).To(gomega.BeTrue())
		// run add again, NewAddressSet should succeed this time
		peerASKey, _, _, err = fakeOvn.controller.EnsurePodSelectorAddressSet(
			peer.PodSelector, peer.NamespaceSelector, networkPolicy.Namespace, getPolicyKeyWithKind(networkPolicy))
		// address set should be created
		fakeOvn.asf.ExpectEmptyAddressSet(peerASIDs)
		fakeOvn.asf.ExpectNumberOfAddressSets(1)
		// peerAddressSet should be present in the map with needsCleanup=false
		asObj, loaded = fakeOvn.controller.podSelectorAddressSets.Load(peerASKey)
		gomega.Expect(loaded).To(gomega.BeTrue())
		gomega.Expect(asObj.needsCleanup).To(gomega.BeFalse())
	})
	ginkgo.It("on cleanup deletes unreferenced and leaves referenced address sets", func() {
		namespaceName := "namespace1"
		policyName := "networkpolicy1"
		staleNetpolIDs := getStaleNetpolAddrSetDbIDs(namespaceName, policyName, "ingress", "0", DefaultNetworkControllerName)
		staleNetpolAS, _ := addressset.GetTestDbAddrSets(staleNetpolIDs, []string{"1.1.1.1"})
		unusedPodSelIDs := getPodSelectorAddrSetDbIDs("pasName", DefaultNetworkControllerName)
		unusedPodSelAS, _ := addressset.GetTestDbAddrSets(unusedPodSelIDs, []string{"1.1.1.2"})
		refNetpolIDs := getStaleNetpolAddrSetDbIDs(namespaceName, policyName, "egress", "0", DefaultNetworkControllerName)
		refNetpolAS, _ := addressset.GetTestDbAddrSets(refNetpolIDs, []string{"1.1.1.3"})
		netpolACL := libovsdbops.BuildACL(
			"netpolACL",
			nbdb.ACLDirectionFromLport,
			types.EgressFirewallStartPriority,
			fmt.Sprintf("ip4.src == {$%s} && outport == @a13757631697825269621", refNetpolAS.Name),
			nbdb.ACLActionAllowRelated,
			types.OvnACLLoggingMeter,
			"",
			false,
			nil,
			map[string]string{
				"apply-after-lb": "true",
			},
			types.DefaultACLTier,
		)
		netpolACL.UUID = "netpolACL-UUID"
		refPodSelIDs := getPodSelectorAddrSetDbIDs("pasName2", DefaultNetworkControllerName)
		refPodSelAS, _ := addressset.GetTestDbAddrSets(refPodSelIDs, []string{"1.1.1.4"})
		podSelACL := libovsdbops.BuildACL(
			"podSelACL",
			nbdb.ACLDirectionFromLport,
			types.EgressFirewallStartPriority,
			fmt.Sprintf("ip4.src == {$%s} && outport == @a13757631697825269621", refPodSelAS.Name),
			nbdb.ACLActionAllowRelated,
			types.OvnACLLoggingMeter,
			"",
			false,
			nil,
			map[string]string{
				"apply-after-lb": "true",
			},
			types.DefaultACLTier,
		)
		podSelACL.UUID = "podSelACL-UUID"

		initialDb := []libovsdbtest.TestData{
			staleNetpolAS,
			unusedPodSelAS,
			refNetpolAS,
			netpolACL,
			refPodSelAS,
			podSelACL,
			&nbdb.LogicalSwitch{
				UUID: "node",
				ACLs: []string{podSelACL.UUID, netpolACL.UUID},
			},
		}
		dbSetup := libovsdbtest.TestSetup{NBData: initialDb}
		fakeOvn.startWithDBSetup(dbSetup)

		err := fakeOvn.controller.cleanupPodSelectorAddressSets()
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		finalDB := []libovsdbtest.TestData{
			refNetpolAS,
			netpolACL,
			refPodSelAS,
			podSelACL,
			&nbdb.LogicalSwitch{
				UUID: "node",
				ACLs: []string{podSelACL.UUID, netpolACL.UUID},
			},
		}
		gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(finalDB))
	})
	ginkgo.It("reconciles a completed and deleted pod whose IP has been assigned to a running pod", func() {
		namespace1 := *newNamespace(namespaceName1)
		nodeName := "node1"
		podIP := "10.128.1.3"
		peer := knet.NetworkPolicyPeer{
			PodSelector: &metav1.LabelSelector{},
		}

		fakeOvn.startWithDBSetup(initialDB,
			&v1.NamespaceList{
				Items: []v1.Namespace{
					namespace1,
				},
			},
		)

		_, _, _, err := fakeOvn.controller.EnsurePodSelectorAddressSet(
			peer.PodSelector, peer.NamespaceSelector, namespace1.Name, "backRef")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		// Start a pod
		completedPod, err := fakeOvn.fakeClient.KubeClient.CoreV1().Pods(namespace1.Name).
			Create(
				context.TODO(),
				newPod(namespace1.Name, "completed-pod", nodeName, podIP),
				metav1.CreateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		// pod should be added to the address set
		eventuallyExpectAddressSetsWithIP(fakeOvn, peer, namespace1.Name, podIP)

		// Spawn a pod with an IP address that collides with a completed pod (we don't watch pods in this test,
		// therefore the same ip is allowed)
		_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(namespace1.Name).
			Create(
				context.TODO(),
				newPod(namespace1.Name, "running-pod", nodeName, podIP),
				metav1.CreateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		// Mark the pod as Completed, so delete event will be generated
		completedPod.Status.Phase = v1.PodSucceeded
		completedPod, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(completedPod.Namespace).
			Update(context.TODO(), completedPod, metav1.UpdateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		// make sure the delete event is handled and address set is not changed
		time.Sleep(100 * time.Millisecond)
		// Running pod policy should not be affected by pod deletions
		eventuallyExpectAddressSetsWithIP(fakeOvn, peer, namespace1.Name, podIP)
	})
	ginkgo.It("reconciles a completed pod whose IP has been assigned to a running pod with non-matching namespace selector", func() {
		namespace1 := *newNamespace(namespaceName1)
		namespace2 := *newNamespace(namespaceName2)
		nodeName := "node1"
		podIP := "10.128.1.3"
		peer := knet.NetworkPolicyPeer{
			PodSelector: &metav1.LabelSelector{},
			NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"name": namespaceName1,
				},
			},
		}

		fakeOvn.startWithDBSetup(initialDB,
			&v1.NamespaceList{
				Items: []v1.Namespace{
					namespace1,
					namespace2,
				},
			},
		)

		_, _, _, err := fakeOvn.controller.EnsurePodSelectorAddressSet(
			peer.PodSelector, peer.NamespaceSelector, namespace1.Name, "backRef")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		// Start a pod
		completedPod, err := fakeOvn.fakeClient.KubeClient.CoreV1().Pods(namespace1.Name).
			Create(
				context.TODO(),
				newPod(namespace1.Name, "completed-pod", nodeName, podIP),
				metav1.CreateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		// pod should be added to the address set
		eventuallyExpectAddressSetsWithIP(fakeOvn, peer, namespace1.Name, podIP)

		// Spawn a pod with an IP address that collides with a completed pod (we don't watch pods in this test,
		// therefore the same ip is allowed). This pod has another namespace that is not matched by the address set
		_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(namespace2.Name).
			Create(
				context.TODO(),
				newPod(namespace2.Name, "running-pod", nodeName, podIP),
				metav1.CreateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		// Mark the pod as Completed, so delete event will be generated
		completedPod.Status.Phase = v1.PodSucceeded
		completedPod, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(completedPod.Namespace).
			Update(context.TODO(), completedPod, metav1.UpdateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		// IP should be deleted from the address set on delete event, since the new pod with the same ip
		// should not be present in given address set
		eventuallyExpectEmptyAddressSetsExist(fakeOvn, peer, namespace1.Name)
	})
	ginkgo.It("cleans up retryFramework resources", func() {
		namespace1 := *newNamespace(namespaceName1)
		namespace1.Labels = map[string]string{"key": "value"}
		startOvn(initialDB, []v1.Namespace{namespace1}, nil, nil, nil)
		selector := &metav1.LabelSelector{
			MatchLabels: map[string]string{"key": "value"},
		}

		// let the system settle down before counting goroutines
		time.Sleep(100 * time.Millisecond)
		goroutinesNumInit := runtime.NumGoroutine()
		// namespace selector will be run because it is not empty.
		// one namespace should match the label and start a pod watchFactory.
		// that gives us 2 retryFrameworks, so 2 periodicallyRetryResources goroutines.
		peerASKey, _, _, err := fakeOvn.controller.EnsurePodSelectorAddressSet(
			selector, selector, namespaceName1, "backRef")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Eventually(func() int {
			return runtime.NumGoroutine()
		}).Should(gomega.Equal(goroutinesNumInit + 2))

		err = fakeOvn.controller.DeletePodSelectorAddressSet(peerASKey, "backRef")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		// expect goroutines number to get back
		gomega.Eventually(func() int {
			return runtime.NumGoroutine()
		}).Should(gomega.Equal(goroutinesNumInit))
	})
})

var _ = ginkgo.Describe("shortLabelSelectorString function", func() {
	ginkgo.It("handles LabelSelectorRequirement.Values order", func() {
		ls1 := &metav1.LabelSelector{
			MatchExpressions: []metav1.LabelSelectorRequirement{{
				Key:      "key",
				Operator: "",
				Values:   []string{"v1", "v2", "v3"},
			}},
		}
		ls2 := &metav1.LabelSelector{
			MatchExpressions: []metav1.LabelSelectorRequirement{{
				Key:      "key",
				Operator: "",
				Values:   []string{"v3", "v2", "v1"},
			}},
		}
		gomega.Expect(shortLabelSelectorString(ls1)).To(gomega.Equal(shortLabelSelectorString(ls2)))
	})
	ginkgo.It("handles MatchExpressions order", func() {
		ls1 := &metav1.LabelSelector{
			MatchExpressions: []metav1.LabelSelectorRequirement{
				{
					Key:      "key1",
					Operator: "",
					Values:   []string{"v1", "v2", "v3"},
				},
				{
					Key:      "key2",
					Operator: "",
					Values:   []string{"v1", "v2", "v3"},
				},
				{
					Key:      "key3",
					Operator: "",
					Values:   []string{"v1", "v2", "v3"},
				},
			},
		}
		ls2 := &metav1.LabelSelector{
			MatchExpressions: []metav1.LabelSelectorRequirement{
				{
					Key:      "key2",
					Operator: "",
					Values:   []string{"v1", "v2", "v3"},
				},
				{
					Key:      "key1",
					Operator: "",
					Values:   []string{"v1", "v2", "v3"},
				},
				{
					Key:      "key3",
					Operator: "",
					Values:   []string{"v1", "v2", "v3"},
				},
			},
		}
		gomega.Expect(shortLabelSelectorString(ls1)).To(gomega.Equal(shortLabelSelectorString(ls2)))
	})
	ginkgo.It("handles MatchLabels order", func() {
		ls1 := &metav1.LabelSelector{
			MatchLabels: map[string]string{"k1": "v1", "k2": "v2", "k3": "v3"},
		}
		ls2 := &metav1.LabelSelector{
			MatchLabels: map[string]string{"k2": "v2", "k1": "v1", "k3": "v3"},
		}
		gomega.Expect(shortLabelSelectorString(ls1)).To(gomega.Equal(shortLabelSelectorString(ls2)))
	})
})
