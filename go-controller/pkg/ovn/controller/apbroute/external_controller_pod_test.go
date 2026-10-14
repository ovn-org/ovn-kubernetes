// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package apbroute

import (
	"context"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("OVN External Gateway pod", func() {

	var (
		gatewayNamespaceName  = "gateway"
		gatewayNamespaceMatch = map[string]string{"name": gatewayNamespaceName}

		targetNamespaceName   = "target1"
		targetNamespaceName2  = "target2"
		targetNamespaceLabel  = "target"
		targetNamespace1Match = map[string]string{"name": targetNamespaceName}
		targetNamespace2Match = map[string]string{"name": targetNamespaceName2}

		namespaceGW = &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: gatewayNamespaceName,
				Labels: gatewayNamespaceMatch}}
		namespaceTarget = &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: targetNamespaceName,
				Labels: map[string]string{"name": targetNamespaceName, "match": targetNamespaceLabel}},
		}
		targetPod1 = newPod("pod_target1", namespaceTarget.Name, "192.169.10.1",
			map[string]string{"key": "pod", "name": "pod_target1"})

		namespaceTarget2 = &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: targetNamespaceName2,
				Labels: map[string]string{"name": targetNamespaceName2, "match": targetNamespaceLabel}},
		}
		targetPod2 = newPod("pod_target2", namespaceTarget2.Name, "192.169.10.2",
			map[string]string{"key": "pod", "name": "pod_target2"})

		dynamicPolicy = newPolicy(
			"dynamic",
			&metav1.LabelSelector{MatchLabels: targetNamespace2Match},
			nil,
			&metav1.LabelSelector{MatchLabels: gatewayNamespaceMatch},
			&metav1.LabelSelector{MatchLabels: map[string]string{"key": "pod"}},
			false,
		)

		dynamicPolicyDiffTargetNS = newPolicy(
			"dynamic2",
			&metav1.LabelSelector{MatchLabels: targetNamespace1Match},
			nil,
			&metav1.LabelSelector{MatchLabels: gatewayNamespaceMatch},
			&metav1.LabelSelector{MatchLabels: map[string]string{"key": "pod"}},
			false,
		)

		dynamicPolicyDiffTargetNSAndPodSel = newPolicy(
			"dynamic2",
			&metav1.LabelSelector{MatchLabels: targetNamespace1Match},
			nil,
			&metav1.LabelSelector{MatchLabels: gatewayNamespaceMatch},
			&metav1.LabelSelector{MatchLabels: map[string]string{"duplicated": "true"}},
			false,
		)

		pod1 = newPod("pod_1", namespaceGW.Name, "192.168.10.1", map[string]string{"key": "pod", "name": "pod1", "duplicated": "true"})
		pod2 = newPod("pod_2", namespaceGW.Name, "192.168.20.1", map[string]string{"key": "pod", "name": "pod2"})
		pod3 = newPod("pod_3", namespaceGW.Name, "192.168.30.1", map[string]string{"key": "pod", "name": "pod3"})

		namespaceTargetWithPod, namespaceTargetWithoutPod, namespaceTarget2WithPod, namespaceGWWithPod, namespaceGWWithoutPod *namespaceWithPods
	)
	AfterEach(func() {
		shutdownController()
		nbsbCleanup.Cleanup()
	})

	BeforeEach(func() {
		Expect(config.PrepareTestConfig()).To(Succeed())
		config.OVNKubernetesFeature.EnableMultiExternalGateway = true
		initialDB = libovsdbtest.TestSetup{
			NBData: []libovsdbtest.TestData{
				&nbdb.LogicalSwitch{
					UUID: "node",
					Name: "node",
				},
				&nbdb.LogicalRouter{
					UUID: "GR_node-UUID",
					Name: "GR_node",
				},
			},
		}
		nbClient, _, nbsbCleanup, err = libovsdbtest.NewNBSBTestHarness(initialDB)
		Expect(err).NotTo(HaveOccurred())
		stopChan = make(chan struct{})

		namespaceTargetWithPod = newNamespaceWithPods(namespaceTarget.Name, targetPod1)
		namespaceTargetWithoutPod = newNamespaceWithPods(namespaceTarget.Name)
		namespaceTarget2WithPod = newNamespaceWithPods(namespaceTarget2.Name, targetPod2)
		//namespaceTarget2WithoutPod = newNamespaceWithPods(namespaceTarget2.Name)
		namespaceGWWithPod = newNamespaceWithPods(namespaceGW.Name, pod1)
		namespaceGWWithoutPod = newNamespaceWithPods(namespaceGW.Name)

	})

	var _ = Context("When adding a new pod", func() {

		It("processes the pod that is a pod gateway with multiples matching policies", func() {
			initController([]runtime.Object{namespaceGW, namespaceTarget, namespaceTarget2, targetPod1, targetPod2},
				[]runtime.Object{dynamicPolicy, dynamicPolicyDiffTargetNS})

			expectedPolicy1, expectedRefs1 := expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTarget2WithPod},
				nil,
				[]*namespaceWithPods{namespaceGWWithoutPod}, false)

			expectedPolicy2, expectedRefs2 := expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTargetWithPod},
				nil,
				[]*namespaceWithPods{namespaceGWWithoutPod}, false)

			eventuallyExpectNumberOfPolicies(2)
			eventuallyExpectConfig(dynamicPolicy.Name, expectedPolicy1, expectedRefs1)
			eventuallyExpectConfig(dynamicPolicyDiffTargetNS.Name, expectedPolicy2, expectedRefs2)

			_, err := fakeClient.CoreV1().Pods(pod1.Namespace).Create(context.Background(), pod1, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			expectedPolicy1, expectedRefs1 = expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTarget2WithPod},
				nil,
				[]*namespaceWithPods{namespaceGWWithPod}, false)

			expectedPolicy2, expectedRefs2 = expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTargetWithPod},
				nil,
				[]*namespaceWithPods{namespaceGWWithPod}, false)

			eventuallyExpectNumberOfPolicies(2)
			eventuallyExpectConfig(dynamicPolicy.Name, expectedPolicy1, expectedRefs1)
			eventuallyExpectConfig(dynamicPolicyDiffTargetNS.Name, expectedPolicy2, expectedRefs2)

		})

		It("processes the pod that has no policy match", func() {
			noMatchPolicy := newPolicy(
				"noMatchPolicy",
				&metav1.LabelSelector{MatchLabels: targetNamespace1Match},
				nil,
				&metav1.LabelSelector{MatchLabels: gatewayNamespaceMatch},
				&metav1.LabelSelector{MatchLabels: map[string]string{"key": "nomatch"}},
				false,
			)
			initController([]runtime.Object{namespaceGW, namespaceTarget}, []runtime.Object{noMatchPolicy})

			expectedPolicy, expectedRefs := expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTargetWithoutPod},
				nil,
				[]*namespaceWithPods{namespaceGWWithoutPod}, false)

			eventuallyExpectNumberOfPolicies(1)
			eventuallyExpectConfig(noMatchPolicy.Name, expectedPolicy, expectedRefs)

			createPod(pod1, fakeClient)
			//  make sure pod event is handled
			time.Sleep(100 * time.Millisecond)

			eventuallyExpectNumberOfPolicies(1)
			eventuallyExpectConfig(noMatchPolicy.Name, expectedPolicy, expectedRefs)
		})
		It("processes the pod that is in Pending phase and then changes to Running with an assigned IP", func() {

			targetPodPending := newPodWithPhaseAndIP("pod_pending", namespaceTarget.Name, corev1.PodPending, "",
				map[string]string{"key": "pod", "name": "pod_pending"})
			namespaceTargetWithPendingPod := newNamespaceWithPods(namespaceTarget.Name, targetPodPending)

			initController([]runtime.Object{namespaceGW, namespaceTarget, pod1, targetPodPending}, []runtime.Object{dynamicPolicyDiffTargetNS})

			expectedPolicy, expectedRefs := expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTargetWithoutPod},
				nil,
				[]*namespaceWithPods{namespaceGWWithPod}, false)

			eventuallyExpectNumberOfPolicies(1)
			eventuallyExpectConfig(dynamicPolicyDiffTargetNS.Name, expectedPolicy, expectedRefs)
			By("Updating the pod to be in Running phase")
			updatePodStatus(targetPodPending, pod1.Status)
			//  make sure pod event is handled
			time.Sleep(100 * time.Millisecond)
			expectedPolicy, expectedRefs = expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTargetWithPendingPod},
				nil,
				[]*namespaceWithPods{namespaceGWWithPod}, false)
			eventuallyExpectNumberOfPolicies(1)
			eventuallyExpectConfig(dynamicPolicyDiffTargetNS.Name, expectedPolicy, expectedRefs)
		})
	})

	var _ = Context("When deleting a pod", func() {

		It("deletes a pod gateway that matches two policies", func() {
			initController([]runtime.Object{namespaceGW, namespaceTarget, namespaceTarget2, targetPod1, targetPod2, pod1},
				[]runtime.Object{dynamicPolicy, dynamicPolicyDiffTargetNS})

			expectedPolicy1, expectedRefs1 := expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTarget2WithPod},
				nil,
				[]*namespaceWithPods{namespaceGWWithPod}, false)

			expectedPolicy2, expectedRefs2 := expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTargetWithPod},
				nil,
				[]*namespaceWithPods{namespaceGWWithPod}, false)

			eventuallyExpectNumberOfPolicies(2)
			eventuallyExpectConfig(dynamicPolicy.Name, expectedPolicy1, expectedRefs1)
			eventuallyExpectConfig(dynamicPolicyDiffTargetNS.Name, expectedPolicy2, expectedRefs2)

			deletePod(pod1, fakeClient)

			expectedPolicy1, expectedRefs1 = expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTarget2WithPod},
				nil,
				[]*namespaceWithPods{namespaceGWWithoutPod}, false)

			expectedPolicy2, expectedRefs2 = expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTargetWithPod},
				nil,
				[]*namespaceWithPods{namespaceGWWithoutPod}, false)

			eventuallyExpectNumberOfPolicies(2)
			eventuallyExpectConfig(dynamicPolicy.Name, expectedPolicy1, expectedRefs1)
			eventuallyExpectConfig(dynamicPolicyDiffTargetNS.Name, expectedPolicy2, expectedRefs2)
		})

		It("deletes a gateway pod that does not match any policy", func() {
			noMatchPolicy := newPolicy(
				"noMatchPolicy",
				&metav1.LabelSelector{MatchLabels: targetNamespace1Match},
				nil,
				&metav1.LabelSelector{MatchLabels: gatewayNamespaceMatch},
				&metav1.LabelSelector{MatchLabels: map[string]string{"key": "nomatch"}},
				false,
			)
			initController([]runtime.Object{namespaceGW, namespaceTarget, pod1}, []runtime.Object{noMatchPolicy})

			expectedPolicy, expectedRefs := expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTargetWithoutPod},
				nil,
				[]*namespaceWithPods{namespaceGWWithoutPod}, false)

			eventuallyExpectNumberOfPolicies(1)
			eventuallyExpectConfig(noMatchPolicy.Name, expectedPolicy, expectedRefs)

			deletePod(pod1, fakeClient)
			//  make sure pod event is handled
			time.Sleep(100 * time.Millisecond)

			eventuallyExpectNumberOfPolicies(1)
			eventuallyExpectConfig(noMatchPolicy.Name, expectedPolicy, expectedRefs)
		})

		It("deletes a pod gateway that is one of two pods that matches two policies", func() {
			initController([]runtime.Object{namespaceGW, namespaceTarget, namespaceTarget2, targetPod1, targetPod2, pod1, pod2},
				[]runtime.Object{dynamicPolicy, dynamicPolicyDiffTargetNS})
			namespaceGWWith2Pods := newNamespaceWithPods(namespaceGW.Name, pod1, pod2)
			expectedPolicy1, expectedRefs1 := expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTarget2WithPod},
				nil,
				[]*namespaceWithPods{namespaceGWWith2Pods}, false)

			expectedPolicy2, expectedRefs2 := expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTargetWithPod},
				nil,
				[]*namespaceWithPods{namespaceGWWith2Pods}, false)

			eventuallyExpectNumberOfPolicies(2)
			eventuallyExpectConfig(dynamicPolicy.Name, expectedPolicy1, expectedRefs1)
			eventuallyExpectConfig(dynamicPolicyDiffTargetNS.Name, expectedPolicy2, expectedRefs2)

			deletePod(pod1, fakeClient)

			namespaceGWWith1Pod := newNamespaceWithPods(namespaceGW.Name, pod2)

			expectedPolicy1, expectedRefs1 = expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTarget2WithPod},
				nil,
				[]*namespaceWithPods{namespaceGWWith1Pod}, false)

			expectedPolicy2, expectedRefs2 = expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTargetWithPod},
				nil,
				[]*namespaceWithPods{namespaceGWWith1Pod}, false)

			eventuallyExpectNumberOfPolicies(2)
			eventuallyExpectConfig(dynamicPolicy.Name, expectedPolicy1, expectedRefs1)
			eventuallyExpectConfig(dynamicPolicyDiffTargetNS.Name, expectedPolicy2, expectedRefs2)
		})

		It("deletes a target pod that matches a policy and creates it again", func() {
			initController([]runtime.Object{namespaceGW, namespaceTarget2, targetPod2, pod1},
				[]runtime.Object{dynamicPolicy})

			expectedPolicy1, expectedRefs1 := expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTarget2WithPod},
				nil,
				[]*namespaceWithPods{namespaceGWWithPod}, false)

			eventuallyExpectNumberOfPolicies(1)
			eventuallyExpectConfig(dynamicPolicy.Name, expectedPolicy1, expectedRefs1)

			By("delete one of the target pods")
			deletePod(targetPod2, fakeClient)
			expectedPolicy1, expectedRefs1 = expectedPolicyStateAndRefs(
				[]*namespaceWithPods{{nsName: targetNamespaceName2}},
				nil,
				[]*namespaceWithPods{namespaceGWWithPod}, false)

			eventuallyExpectNumberOfPolicies(1)
			eventuallyExpectConfig(dynamicPolicy.Name, expectedPolicy1, expectedRefs1)

			By("create the deleted target pod")

			createPod(targetPod2, fakeClient)
			expectedPolicy1, expectedRefs1 = expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTarget2WithPod},
				nil,
				[]*namespaceWithPods{namespaceGWWithPod}, false)

			eventuallyExpectNumberOfPolicies(1)
			eventuallyExpectConfig(dynamicPolicy.Name, expectedPolicy1, expectedRefs1)
		})
	})

	var _ = Context("When updating a pod", func() {
		It("updates an existing pod gateway to match an additional new policy to a new target namespace", func() {
			unmatchPod := newPod("unmatchPod", namespaceGW.Name, "192.168.100.1", map[string]string{"name": "unmatchPod"})

			initController([]runtime.Object{namespaceTarget2, namespaceGW, targetPod2, unmatchPod}, []runtime.Object{dynamicPolicy})
			expectedPolicy, expectedRefs := expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTarget2WithPod},
				nil,
				[]*namespaceWithPods{namespaceGWWithoutPod}, false)

			eventuallyExpectNumberOfPolicies(1)
			eventuallyExpectConfig(dynamicPolicy.Name, expectedPolicy, expectedRefs)

			updatePodLabels(unmatchPod, pod1.Labels, fakeClient)
			expectedPolicy, expectedRefs = expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTarget2WithPod},
				nil,
				[]*namespaceWithPods{newNamespaceWithPods(namespaceGW.Name, unmatchPod)}, false)

			eventuallyExpectNumberOfPolicies(1)
			eventuallyExpectConfig(dynamicPolicy.Name, expectedPolicy, expectedRefs)
		})

		It("updates an existing pod gateway to match a new policy that targets a different namespace", func() {
			initController([]runtime.Object{namespaceGW, namespaceTarget, namespaceTarget2, targetPod1, targetPod2, pod2, pod3},
				[]runtime.Object{dynamicPolicyDiffTargetNSAndPodSel, dynamicPolicy})

			expectedPolicy1, expectedRefs1 := expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTarget2WithPod},
				nil,
				[]*namespaceWithPods{newNamespaceWithPods(namespaceGW.Name, pod2, pod3)}, false)
			expectedPolicy2, expectedRefs2 := expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTargetWithPod},
				nil,
				[]*namespaceWithPods{namespaceGWWithoutPod}, false)

			eventuallyExpectNumberOfPolicies(2)
			eventuallyExpectConfig(dynamicPolicy.Name, expectedPolicy1, expectedRefs1)
			eventuallyExpectConfig(dynamicPolicyDiffTargetNSAndPodSel.Name, expectedPolicy2, expectedRefs2)

			updatePodLabels(pod2, map[string]string{"duplicated": "true"}, fakeClient)

			expectedPolicy1, expectedRefs1 = expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTarget2WithPod},
				nil,
				[]*namespaceWithPods{newNamespaceWithPods(namespaceGW.Name, pod3)}, false)
			expectedPolicy2, expectedRefs2 = expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTargetWithPod},
				nil,
				[]*namespaceWithPods{newNamespaceWithPods(namespaceGW.Name, pod2)}, false)

			eventuallyExpectNumberOfPolicies(2)
			eventuallyExpectConfig(dynamicPolicy.Name, expectedPolicy1, expectedRefs1)
			eventuallyExpectConfig(dynamicPolicyDiffTargetNSAndPodSel.Name, expectedPolicy2, expectedRefs2)
		})
		It("updates an existing pod gateway to match no policies", func() {
			initController([]runtime.Object{namespaceGW, namespaceTarget2, targetPod2, pod1, pod2}, []runtime.Object{dynamicPolicy})
			expectedPolicy, expectedRefs := expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTarget2WithPod},
				nil,
				[]*namespaceWithPods{newNamespaceWithPods(namespaceGW.Name, pod1, pod2)}, false)

			eventuallyExpectNumberOfPolicies(1)
			eventuallyExpectConfig(dynamicPolicy.Name, expectedPolicy, expectedRefs)

			updatePodLabels(pod1, map[string]string{}, fakeClient)

			expectedPolicy, expectedRefs = expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTarget2WithPod},
				nil,
				[]*namespaceWithPods{newNamespaceWithPods(namespaceGW.Name, pod2)}, false)

			eventuallyExpectNumberOfPolicies(1)
			eventuallyExpectConfig(dynamicPolicy.Name, expectedPolicy, expectedRefs)
		})

		It("updates a pod to match a policy to a single namespace", func() {
			initController([]runtime.Object{namespaceGW, namespaceTarget, namespaceTarget2, targetPod1, targetPod2, pod1},
				[]runtime.Object{dynamicPolicyDiffTargetNSAndPodSel, dynamicPolicy})

			expectedPolicy1, expectedRefs1 := expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTarget2WithPod},
				nil,
				[]*namespaceWithPods{namespaceGWWithPod}, false)
			expectedPolicy2, expectedRefs2 := expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTargetWithPod},
				nil,
				[]*namespaceWithPods{namespaceGWWithPod}, false)

			eventuallyExpectNumberOfPolicies(2)
			eventuallyExpectConfig(dynamicPolicy.Name, expectedPolicy1, expectedRefs1)
			eventuallyExpectConfig(dynamicPolicyDiffTargetNSAndPodSel.Name, expectedPolicy2, expectedRefs2)

			updatePodLabels(pod1, map[string]string{"key": "pod"}, fakeClient)

			expectedPolicy2, expectedRefs2 = expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTargetWithPod},
				nil,
				[]*namespaceWithPods{namespaceGWWithoutPod}, false)

			eventuallyExpectNumberOfPolicies(2)
			eventuallyExpectConfig(dynamicPolicy.Name, expectedPolicy1, expectedRefs1)
			eventuallyExpectConfig(dynamicPolicyDiffTargetNSAndPodSel.Name, expectedPolicy2, expectedRefs2)
		})

	})

	var _ = Context("When pod goes into terminating or not ready state", func() {

		DescribeTable("reconciles a pod gateway in terminating or not ready state that matches two policies", func(
			terminating bool,
		) {
			initController([]runtime.Object{namespaceGW, namespaceTarget, namespaceTarget2, targetPod1, targetPod2, pod1},
				[]runtime.Object{dynamicPolicy, dynamicPolicyDiffTargetNS})

			expectedPolicy1, expectedRefs1 := expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTarget2WithPod},
				nil,
				[]*namespaceWithPods{namespaceGWWithPod}, false)

			expectedPolicy2, expectedRefs2 := expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTargetWithPod},
				nil,
				[]*namespaceWithPods{namespaceGWWithPod}, false)

			eventuallyExpectNumberOfPolicies(2)
			eventuallyExpectConfig(dynamicPolicy.Name, expectedPolicy1, expectedRefs1)
			eventuallyExpectConfig(dynamicPolicyDiffTargetNS.Name, expectedPolicy2, expectedRefs2)

			if terminating {
				By("Setting deletion timestamp for the ex gw pod")
				setPodDeletionTimestamp(pod1, &metav1.Time{Time: time.Now().Add(1000 * time.Second)}, fakeClient)
			} else {
				By("Updating the ex gw pod status to mark it as not ready")
				setPodConditionReady(pod1, corev1.ConditionFalse, fakeClient)
			}

			expectedPolicy1, expectedRefs1 = expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTarget2WithPod},
				nil,
				[]*namespaceWithPods{namespaceGWWithoutPod}, false)

			expectedPolicy2, expectedRefs2 = expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTargetWithPod},
				nil,
				[]*namespaceWithPods{namespaceGWWithoutPod}, false)

			eventuallyExpectNumberOfPolicies(2)
			eventuallyExpectConfig(dynamicPolicy.Name, expectedPolicy1, expectedRefs1)
			eventuallyExpectConfig(dynamicPolicyDiffTargetNS.Name, expectedPolicy2, expectedRefs2)
		},
			Entry("Gateway pod in terminating state", true),
			Entry("Gateway pod in not ready state", false),
		)

		DescribeTable("reconciles a pod gateway in terminating or not ready state that does not match any policy", func(
			terminating bool,
		) {
			noMatchPolicy := newPolicy(
				"noMatchPolicy",
				&metav1.LabelSelector{MatchLabels: targetNamespace1Match},
				nil,
				&metav1.LabelSelector{MatchLabels: gatewayNamespaceMatch},
				&metav1.LabelSelector{MatchLabels: map[string]string{"key": "nomatch"}},
				false,
			)
			initController([]runtime.Object{namespaceGW, namespaceTarget, pod1}, []runtime.Object{noMatchPolicy})

			expectedPolicy, expectedRefs := expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTargetWithoutPod},
				nil,
				[]*namespaceWithPods{namespaceGWWithoutPod}, false)

			eventuallyExpectNumberOfPolicies(1)
			eventuallyExpectConfig(noMatchPolicy.Name, expectedPolicy, expectedRefs)

			if terminating {
				By("Setting deletion timestamp for the ex gw pod")
				setPodDeletionTimestamp(pod1, &metav1.Time{Time: time.Now().Add(1000 * time.Second)}, fakeClient)
			} else {
				By("Updating the ex gw pod status to mark it as not ready")
				setPodConditionReady(pod1, corev1.ConditionFalse, fakeClient)
			}
			//  make sure pod event is handled
			time.Sleep(100 * time.Millisecond)

			eventuallyExpectNumberOfPolicies(1)
			eventuallyExpectConfig(noMatchPolicy.Name, expectedPolicy, expectedRefs)
		},
			Entry("Gateway pod in terminating state", true),
			Entry("Gateway pod in not ready state", false),
		)

		DescribeTable("reconciles a pod gateway in terminating or not ready state that is one of two pods that matches two policies", func(
			terminating bool,
		) {
			initController([]runtime.Object{namespaceGW, namespaceTarget, namespaceTarget2, targetPod1, targetPod2, pod1, pod2},
				[]runtime.Object{dynamicPolicy, dynamicPolicyDiffTargetNS})
			namespaceGWWith2Pods := newNamespaceWithPods(namespaceGW.Name, pod1, pod2)
			expectedPolicy1, expectedRefs1 := expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTarget2WithPod},
				nil,
				[]*namespaceWithPods{namespaceGWWith2Pods}, false)

			expectedPolicy2, expectedRefs2 := expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTargetWithPod},
				nil,
				[]*namespaceWithPods{namespaceGWWith2Pods}, false)

			eventuallyExpectNumberOfPolicies(2)
			eventuallyExpectConfig(dynamicPolicy.Name, expectedPolicy1, expectedRefs1)
			eventuallyExpectConfig(dynamicPolicyDiffTargetNS.Name, expectedPolicy2, expectedRefs2)

			if terminating {
				By("Setting deletion timestamp for the ex gw pod")
				setPodDeletionTimestamp(pod1, &metav1.Time{Time: time.Now().Add(1000 * time.Second)}, fakeClient)
			} else {
				By("Updating the ex gw pod status to mark it as not ready")
				setPodConditionReady(pod1, corev1.ConditionFalse, fakeClient)
			}

			namespaceGWWith1Pod := newNamespaceWithPods(namespaceGW.Name, pod2)

			expectedPolicy1, expectedRefs1 = expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTarget2WithPod},
				nil,
				[]*namespaceWithPods{namespaceGWWith1Pod}, false)

			expectedPolicy2, expectedRefs2 = expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTargetWithPod},
				nil,
				[]*namespaceWithPods{namespaceGWWith1Pod}, false)

			eventuallyExpectNumberOfPolicies(2)
			eventuallyExpectConfig(dynamicPolicy.Name, expectedPolicy1, expectedRefs1)
			eventuallyExpectConfig(dynamicPolicyDiffTargetNS.Name, expectedPolicy2, expectedRefs2)

			if terminating {
				By("Removing deletion timestamp for the ex gw pod")
				setPodDeletionTimestamp(pod1, nil, fakeClient)
			} else {
				By("Updating the ex gw pod status to mark it as ready")
				setPodConditionReady(pod1, corev1.ConditionTrue, fakeClient)
			}

			expectedPolicy1, expectedRefs1 = expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTarget2WithPod},
				nil,
				[]*namespaceWithPods{namespaceGWWith2Pods}, false)

			expectedPolicy2, expectedRefs2 = expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTargetWithPod},
				nil,
				[]*namespaceWithPods{namespaceGWWith2Pods}, false)

			eventuallyExpectNumberOfPolicies(2)
			eventuallyExpectConfig(dynamicPolicy.Name, expectedPolicy1, expectedRefs1)
			eventuallyExpectConfig(dynamicPolicyDiffTargetNS.Name, expectedPolicy2, expectedRefs2)
		},
			Entry("Gateway pod in terminating state", true),
			Entry("Gateway pod in not ready state", false),
		)
	})

	var _ = Context("When a gateway pod's requested network attachment disappears", func() {
		It("cleans up stale routes instead of leaving the last known good config in place", func() {
			initController([]runtime.Object{namespaceGW, namespaceTarget2, targetPod2, pod1}, []runtime.Object{dynamicPolicy})

			expectedPolicy, expectedRefs := expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTarget2WithPod},
				nil,
				[]*namespaceWithPods{namespaceGWWithPod}, false)

			eventuallyExpectNumberOfPolicies(1)
			eventuallyExpectConfig(dynamicPolicy.Name, expectedPolicy, expectedRefs)
			eventuallyCheckAPBRouteStatus(dynamicPolicy.Name, false)

			By("removing the network attachment the dynamic hop relies on, simulating the referenced resource being deleted")
			updatePodAnnotations(pod1, map[string]string{}, fakeClient)

			expectedPolicy, expectedRefs = expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTarget2WithPod},
				nil,
				[]*namespaceWithPods{namespaceGWWithoutPod}, false)

			eventuallyExpectNumberOfPolicies(1)
			eventuallyExpectConfig(dynamicPolicy.Name, expectedPolicy, expectedRefs)
		})
	})
})

func deletePod(pod *corev1.Pod, fakeClient *fake.Clientset) {
	err = fakeClient.CoreV1().Pods(pod.Namespace).Delete(context.Background(), pod.Name, metav1.DeleteOptions{})
	Expect(err).NotTo(HaveOccurred())
}

func createPod(pod *corev1.Pod, fakeClient *fake.Clientset) {
	_, err = fakeClient.CoreV1().Pods(pod.Namespace).Create(context.Background(), pod, metav1.CreateOptions{})
	Expect(err).NotTo(HaveOccurred())
}

func updatePodLabels(pod *corev1.Pod, newLabels map[string]string, fakeClient *fake.Clientset) {
	p, err := fakeClient.CoreV1().Pods(pod.Namespace).Get(context.TODO(), pod.Name, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	incrementResourceVersion(p)
	p.Labels = newLabels
	_, err = fakeClient.CoreV1().Pods(pod.Namespace).Update(context.Background(), p, metav1.UpdateOptions{})
	Expect(err).NotTo(HaveOccurred())
}

func updatePodAnnotations(pod *corev1.Pod, newAnnotations map[string]string, fakeClient *fake.Clientset) {
	p, err := fakeClient.CoreV1().Pods(pod.Namespace).Get(context.TODO(), pod.Name, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	incrementResourceVersion(p)
	p.Annotations = newAnnotations
	_, err = fakeClient.CoreV1().Pods(pod.Namespace).Update(context.Background(), p, metav1.UpdateOptions{})
	Expect(err).NotTo(HaveOccurred())
}

func updatePodStatus(pod *corev1.Pod, podStatus corev1.PodStatus) {
	p, err := fakeClient.CoreV1().Pods(pod.Namespace).Get(context.TODO(), pod.Name, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	incrementResourceVersion(p)
	p.Status = podStatus
	_, err = fakeClient.CoreV1().Pods(pod.Namespace).Update(context.Background(), p, metav1.UpdateOptions{})
	Expect(err).NotTo(HaveOccurred())
}

func setPodDeletionTimestamp(pod *corev1.Pod, deletionTimestamp *metav1.Time, fakeClient *fake.Clientset) {
	p, err := fakeClient.CoreV1().Pods(pod.Namespace).Get(context.TODO(), pod.Name, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	incrementResourceVersion(p)
	p.DeletionTimestamp = deletionTimestamp
	_, err = fakeClient.CoreV1().Pods(pod.Namespace).Update(context.Background(), p, metav1.UpdateOptions{})
	Expect(err).NotTo(HaveOccurred())
}

func setPodConditionReady(pod *corev1.Pod, condStatus corev1.ConditionStatus, fakeClient *fake.Clientset) {
	p, err := fakeClient.CoreV1().Pods(pod.Namespace).Get(context.TODO(), pod.Name, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	incrementResourceVersion(p)
	if p.Status.Conditions != nil {
		for i := range p.Status.Conditions {
			if p.Status.Conditions[i].Type == corev1.PodReady {
				p.Status.Conditions[i].Status = condStatus
			}
		}
	} else {
		notReadyCondition := corev1.PodCondition{
			Type:   corev1.PodReady,
			Status: corev1.ConditionFalse,
		}
		p.Status.Conditions = []corev1.PodCondition{notReadyCondition}
	}
	_, err = fakeClient.CoreV1().Pods(pod.Namespace).Update(context.Background(), p, metav1.UpdateOptions{})
	Expect(err).NotTo(HaveOccurred())
}

func incrementResourceVersion(obj metav1.Object) {
	var rs int64
	if obj.GetResourceVersion() != "" {
		rs, err = strconv.ParseInt(obj.GetResourceVersion(), 10, 64)
		Expect(err).NotTo(HaveOccurred())
	}
	rs++
	obj.SetResourceVersion(strconv.FormatInt(rs, 10))
}
