package apbroute

import (
	"context"
	"reflect"
	"strconv"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes/fake"
)

var _ = Describe("OVN External Gateway policy", func() {

	var (
		namespaceDefault = &corev1.Namespace{
			ObjectMeta: v1.ObjectMeta{Name: "default",
				Labels: map[string]string{"name": "default"}}}
		namespaceTest = &corev1.Namespace{
			ObjectMeta: v1.ObjectMeta{Name: "test",
				Labels: map[string]string{"name": "test", "match": "test", "multiple": "true"}},
		}
		namespaceTest2 = &corev1.Namespace{
			ObjectMeta: v1.ObjectMeta{Name: "test2",
				Labels: map[string]string{"name": "test2", "match": "test2", "multiple": "true"}},
		}

		dynamicPolicy = newPolicy(
			"dynamic",
			&v1.LabelSelector{MatchLabels: map[string]string{"name": "test"}},
			nil,
			&v1.LabelSelector{MatchLabels: map[string]string{"name": "default"}},
			&v1.LabelSelector{MatchLabels: map[string]string{"key": "pod"}},
			false,
		)

		dynamicPolicyForTest2Only = newPolicy(
			"policyForTest2",
			&v1.LabelSelector{MatchLabels: map[string]string{"match": "test2"}},
			nil,
			&v1.LabelSelector{MatchLabels: map[string]string{"name": "default"}},
			&v1.LabelSelector{MatchLabels: map[string]string{"duplicated": "true"}},
			false,
		)

		overlappingPolicy = newPolicy(
			"overlapping",
			&v1.LabelSelector{MatchLabels: map[string]string{"match": "test"}},
			nil,
			&v1.LabelSelector{MatchLabels: map[string]string{"name": "default"}},
			&v1.LabelSelector{MatchLabels: map[string]string{"duplicated": "true"}},
			false,
		)

		multipleNamespacesPolicy = newPolicy(
			"multipleNamespaces",
			&v1.LabelSelector{MatchLabels: map[string]string{"multiple": "true"}},
			nil,
			&v1.LabelSelector{MatchLabels: map[string]string{"name": "default"}},
			&v1.LabelSelector{MatchLabels: map[string]string{"key": "pod"}},
			false,
		)

		pod1 = newPod("pod_1", "default", "192.168.10.1", map[string]string{"key": "pod", "name": "pod1", "duplicated": "true"})
		pod2 = newPod("pod_2", "default", "192.168.20.1", map[string]string{"key": "pod", "name": "pod2"})
		pod3 = newPod("pod_3", "default", "192.168.30.1", map[string]string{"key": "pod", "name": "pod3"})
	)
	AfterEach(func() {
		nbsbCleanup.Cleanup()
	})

	BeforeEach(func() {
		initialDB = libovsdbtest.TestSetup{
			NBData: []libovsdbtest.TestData{
				&nbdb.LogicalSwitch{
					UUID: "node1",
					Name: "node1",
				},
			},
		}
		nbClient, _, nbsbCleanup, err = libovsdbtest.NewNBSBTestHarness(initialDB)
		Expect(err).NotTo(HaveOccurred())
		stopChan = make(chan struct{})

	})

	var _ = Context("When adding a new pod", func() {

		It("processes the pod that is a pod gateway with multiples matching policies each in a different namespaces", func() {

			initController([]runtime.Object{namespaceDefault, namespaceTest, namespaceTest2}, []runtime.Object{multipleNamespacesPolicy})
			Eventually(func() []string { return listRoutePolicyInCache() }, 5).Should(HaveLen(1))
			Eventually(func() []string { return listNamespaceInfo() }, 5).Should(HaveLen(2))
			Eventually(func() *namespaceInfo { return getNamespaceInfo(namespaceTest.Name) }, 5).Should(
				Equal(
					&namespaceInfo{
						policies:        sets.New(multipleNamespacesPolicy.Name),
						staticGateways:  gatewayInfoList{},
						dynamicGateways: map[types.NamespacedName]*gatewayInfo{},
					}))
			Eventually(func() *namespaceInfo { return getNamespaceInfo(namespaceTest2.Name) }, 5).Should(
				Equal(
					&namespaceInfo{
						policies:        sets.New(multipleNamespacesPolicy.Name),
						staticGateways:  gatewayInfoList{},
						dynamicGateways: map[types.NamespacedName]*gatewayInfo{},
					}))
			_, err := fakeClient.CoreV1().Pods(pod1.Namespace).Create(context.Background(), pod1, v1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			Eventually(func() *namespaceInfo { return getNamespaceInfo(namespaceTest.Name) }, 5).Should(
				Equal(
					&namespaceInfo{
						policies:       sets.New(multipleNamespacesPolicy.Name),
						staticGateways: gatewayInfoList{},
						dynamicGateways: map[types.NamespacedName]*gatewayInfo{
							{Namespace: "default", Name: pod1.Name}: {
								gws: sets.New(pod1.Status.PodIPs[0].IP),
							},
						},
					}))
			Eventually(func() *namespaceInfo { return getNamespaceInfo(namespaceTest2.Name) }, 5).Should(
				Equal(
					&namespaceInfo{
						policies:       sets.New(multipleNamespacesPolicy.Name),
						staticGateways: gatewayInfoList{},
						dynamicGateways: map[types.NamespacedName]*gatewayInfo{
							{Namespace: "default", Name: pod1.Name}: {
								gws: sets.New(pod1.Status.PodIPs[0].IP),
							},
						}}))

		})

		It("processes the pod that has no policy match", func() {
			noMatchPolicy := newPolicy(
				"noMatchPolicy",
				&v1.LabelSelector{MatchLabels: map[string]string{"match": "test"}},
				nil,
				&v1.LabelSelector{MatchLabels: map[string]string{"name": "default"}},
				&v1.LabelSelector{MatchLabels: map[string]string{"key": "nomatch"}},
				false,
			)
			initController([]runtime.Object{namespaceDefault, namespaceTest}, []runtime.Object{noMatchPolicy})
			Eventually(func() []string { return listRoutePolicyInCache() }, 5).Should(HaveLen(1))
			_, err := fakeClient.CoreV1().Pods(pod1.Namespace).Create(context.Background(), pod1, v1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			Eventually(func() []string { return listNamespaceInfo() }, 5).Should(HaveLen(1))
			Eventually(func() *namespaceInfo { return getNamespaceInfo(namespaceTest.Name) }, 5).Should(
				Equal(
					&namespaceInfo{
						policies:        sets.New(noMatchPolicy.Name),
						staticGateways:  gatewayInfoList{},
						dynamicGateways: map[types.NamespacedName]*gatewayInfo{},
					}))

		})

		It("processes a pod gateway that matches a two policies to the same target namespace", func() {

			initController([]runtime.Object{namespaceDefault, namespaceTest, namespaceTest2}, []runtime.Object{overlappingPolicy, dynamicPolicy})
			Eventually(func() []string { return listRoutePolicyInCache() }, 5).Should(HaveLen(2))
			Eventually(func() []string { return listNamespaceInfo() }, 5).Should(HaveLen(1))
			Eventually(func() *namespaceInfo { return getNamespaceInfo(namespaceTest.Name) }, 5).Should(
				Equal(
					&namespaceInfo{
						policies:        sets.New(overlappingPolicy.Name, dynamicPolicy.Name),
						staticGateways:  gatewayInfoList{},
						dynamicGateways: map[types.NamespacedName]*gatewayInfo{},
					}))
			_, err := fakeClient.CoreV1().Pods(pod1.Namespace).Create(context.Background(), pod1, v1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			Eventually(func() *namespaceInfo { return getNamespaceInfo(namespaceTest.Name) }, 5).Should(
				Equal(
					&namespaceInfo{
						policies:       sets.New(overlappingPolicy.Name, dynamicPolicy.Name),
						staticGateways: gatewayInfoList{},
						dynamicGateways: map[types.NamespacedName]*gatewayInfo{
							{Namespace: "default", Name: pod1.Name}: {
								gws: sets.New(pod1.Status.PodIPs[0].IP),
							},
						}}))

		})
	})

	var _ = Context("When deleting a pod", func() {
		It("deletes a pod gateway that matches two policies, each targeting a different namespace", func() {
			dynamicPolicyTest2 := newPolicy(
				"dynamicTest2",
				&v1.LabelSelector{MatchLabels: map[string]string{"name": "test2"}},
				nil,
				&v1.LabelSelector{MatchLabels: map[string]string{"name": "default"}},
				&v1.LabelSelector{MatchLabels: map[string]string{"key": "pod"}},
				false,
			)
			initController([]runtime.Object{namespaceDefault, namespaceTest, namespaceTest2, pod1}, []runtime.Object{dynamicPolicyTest2, dynamicPolicy})
			Eventually(func() []string { return listRoutePolicyInCache() }, 5).Should(HaveLen(2))
			Eventually(func() []string { return listNamespaceInfo() }, 5).Should(HaveLen(2))
			Eventually(func() *namespaceInfo { return getNamespaceInfo(namespaceTest.Name) }, 5).Should(Equal(
				&namespaceInfo{
					policies:       sets.New(dynamicPolicy.Name),
					staticGateways: gatewayInfoList{},
					dynamicGateways: map[types.NamespacedName]*gatewayInfo{
						{Namespace: "default", Name: pod1.Name}: {
							gws: sets.New(pod1.Status.PodIPs[0].IP),
						},
					},
				}))
			Eventually(func() *namespaceInfo { return getNamespaceInfo(namespaceTest2.Name) }, 5).Should(Equal(
				&namespaceInfo{
					policies:       sets.New(dynamicPolicyTest2.Name),
					staticGateways: gatewayInfoList{},
					dynamicGateways: map[types.NamespacedName]*gatewayInfo{
						{Namespace: "default", Name: pod1.Name}: {
							gws: sets.New(pod1.Status.PodIPs[0].IP),
						},
					},
				}))
			deletePod(pod1, fakeClient)
			Eventually(func() []string { return listNamespaceInfo() }, 5).Should(HaveLen(2))
			Eventually(func() *namespaceInfo { return getNamespaceInfo(namespaceTest.Name) }, 5).Should(Equal(
				&namespaceInfo{
					policies:        sets.New(dynamicPolicy.Name),
					staticGateways:  gatewayInfoList{},
					dynamicGateways: map[types.NamespacedName]*gatewayInfo{},
				}))
			Eventually(func() *namespaceInfo { return getNamespaceInfo(namespaceTest2.Name) }, 5).Should(Equal(
				&namespaceInfo{
					policies:        sets.New(dynamicPolicyTest2.Name),
					staticGateways:  gatewayInfoList{},
					dynamicGateways: map[types.NamespacedName]*gatewayInfo{},
				}))
		})

		It("deletes a pod that does not match any policy", func() {
			noMatchPolicy := newPolicy(
				"nomatch",
				&v1.LabelSelector{MatchLabels: map[string]string{"match": "test"}},
				nil,
				&v1.LabelSelector{MatchLabels: map[string]string{"name": "default"}},
				&v1.LabelSelector{MatchLabels: map[string]string{"key": "nomatch"}},
				false,
			)
			initController([]runtime.Object{namespaceDefault, namespaceTest, pod1}, []runtime.Object{noMatchPolicy})
			Eventually(func() []string { return listRoutePolicyInCache() }, 5).Should(HaveLen(1))
			Eventually(func() []string { return listNamespaceInfo() }, 5).Should(HaveLen(1))
			Eventually(func() *namespaceInfo { return getNamespaceInfo(namespaceTest.Name) }, 5).Should(Equal(
				&namespaceInfo{
					policies:        sets.New(noMatchPolicy.Name),
					staticGateways:  gatewayInfoList{},
					dynamicGateways: map[types.NamespacedName]*gatewayInfo{},
				}))
			deletePod(pod1, fakeClient)
			Eventually(func() bool {
				_, err := fakeClient.CoreV1().Pods(pod1.Namespace).Get(context.Background(), pod1.Name, v1.GetOptions{})
				return apierrors.IsNotFound(err)
			}).Should(BeTrue())
			Eventually(func() []string { return listRoutePolicyInCache() }, 5).Should(HaveLen(1))
			Eventually(func() []string { return listNamespaceInfo() }, 5).Should(HaveLen(1))
			Eventually(func() *namespaceInfo { return getNamespaceInfo(namespaceTest.Name) }, 5).Should(Equal(
				&namespaceInfo{
					policies:        sets.New(noMatchPolicy.Name),
					staticGateways:  gatewayInfoList{},
					dynamicGateways: map[types.NamespacedName]*gatewayInfo{},
				}))
		})

		It("deletes a pod gateway that is one of two pods that matches two policies to the same target namespace", func() {
			initController([]runtime.Object{namespaceDefault, namespaceTest, pod1, pod2}, []runtime.Object{overlappingPolicy, dynamicPolicy})
			Eventually(func() []string { return listRoutePolicyInCache() }, 5).Should(HaveLen(2))
			Eventually(func() []string { return listNamespaceInfo() }, 5).Should(HaveLen(1))
			deletePod(pod1, fakeClient)

			Eventually(func() *namespaceInfo { return getNamespaceInfo(namespaceTest.Name) }, 5).Should(Equal(
				&namespaceInfo{
					policies:       sets.New(overlappingPolicy.Name, dynamicPolicy.Name),
					staticGateways: gatewayInfoList{},
					dynamicGateways: map[types.NamespacedName]*gatewayInfo{
						{Namespace: "default", Name: pod2.Name}: {
							gws: sets.New(pod2.Status.PodIPs[0].IP),
						},
					},
				}))
		})
	})

	var _ = Context("When updating a pod", func() {
		It("updates an existing pod gateway to match an additional new policy to a new target namespace", func() {
			unmatchPod := newPod("unmatchPod", "default", "192.168.100.1", map[string]string{"name": "unmatchPod"})
			initController([]runtime.Object{namespaceDefault, namespaceTest, unmatchPod}, []runtime.Object{dynamicPolicy})
			Eventually(func() []string { return listRoutePolicyInCache() }, 5).Should(HaveLen(1))
			Eventually(func() []string { return listNamespaceInfo() }, 5).Should(HaveLen(1))
			Eventually(func() *namespaceInfo { return getNamespaceInfo(namespaceTest.Name) }, 5).Should(Equal(
				&namespaceInfo{
					policies:        sets.New(dynamicPolicy.Name),
					staticGateways:  gatewayInfoList{},
					dynamicGateways: map[types.NamespacedName]*gatewayInfo{},
				}))
			updatePodLabels(unmatchPod, pod1.Labels, fakeClient)
			Eventually(func() *namespaceInfo { return getNamespaceInfo(namespaceTest.Name) }, 5).Should(Equal(
				&namespaceInfo{
					policies:       sets.New(dynamicPolicy.Name),
					staticGateways: gatewayInfoList{},
					dynamicGateways: map[types.NamespacedName]*gatewayInfo{
						{Namespace: "default", Name: unmatchPod.Name}: {
							gws: sets.New(unmatchPod.Status.PodIPs[0].IP),
						},
					},
				}))
		})

		It("updates an existing pod gateway to match a new policy that targets the same namespace", func() {

			initController([]runtime.Object{namespaceDefault, namespaceTest, pod2}, []runtime.Object{overlappingPolicy, dynamicPolicy})
			Eventually(func() []string { return listRoutePolicyInCache() }, 5).Should(HaveLen(2))
			Eventually(func() []string { return listNamespaceInfo() }, 5).Should(HaveLen(1))
			Eventually(func() *namespaceInfo { return getNamespaceInfo(namespaceTest.Name) }, 5).Should(Equal(
				&namespaceInfo{
					policies:       sets.New(dynamicPolicy.Name, overlappingPolicy.Name),
					staticGateways: gatewayInfoList{},
					dynamicGateways: map[types.NamespacedName]*gatewayInfo{
						{Namespace: "default", Name: pod2.Name}: {
							gws: sets.New(pod2.Status.PodIPs[0].IP),
						},
					},
				}))
			updatePodLabels(pod2, map[string]string{"duplicated": "true"}, fakeClient)
			// wait for 2 second to ensure that the pod changed have been reconciled. We are doing this because the outcome of the change should not impact the list of dynamic IPs and
			// there is no way to know which of the policies apply specifically to the pod.
			Eventually(func() bool {
				p, err := fakeClient.CoreV1().Pods(pod2.Namespace).Get(context.TODO(), pod2.Name, v1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				return reflect.DeepEqual(p.Labels, map[string]string{"duplicated": "true"})
			}, 2, 2).Should(BeTrue())
			Eventually(func() *namespaceInfo { return getNamespaceInfo(namespaceTest.Name) }, 5).Should(Equal(
				&namespaceInfo{
					policies:       sets.New(dynamicPolicy.Name, overlappingPolicy.Name),
					staticGateways: gatewayInfoList{},
					dynamicGateways: map[types.NamespacedName]*gatewayInfo{
						{Namespace: "default", Name: pod2.Name}: {
							gws: sets.New(pod2.Status.PodIPs[0].IP),
						},
					},
				}))
		})

		It("updates an existing pod gateway to match a new policy that targets a different namespace", func() {

			initController([]runtime.Object{namespaceDefault, namespaceTest, namespaceTest2, pod2, pod3}, []runtime.Object{dynamicPolicyForTest2Only, dynamicPolicy})
			Eventually(func() []string { return listNamespaceInfo() }, 5).Should(HaveLen(2))
			Eventually(func() *namespaceInfo { return getNamespaceInfo(namespaceTest.Name) }, 5).Should(Equal(
				&namespaceInfo{
					policies:       sets.New(dynamicPolicy.Name),
					staticGateways: gatewayInfoList{},
					dynamicGateways: map[types.NamespacedName]*gatewayInfo{
						{Namespace: "default", Name: pod2.Name}: {
							gws: sets.New(pod2.Status.PodIPs[0].IP),
						},
						{Namespace: "default", Name: pod3.Name}: {
							gws: sets.New(pod3.Status.PodIPs[0].IP),
						},
					},
				}))
			Eventually(func() *namespaceInfo { return getNamespaceInfo(namespaceTest2.Name) }, 5).Should(Equal(
				&namespaceInfo{
					policies:        sets.New(dynamicPolicyForTest2Only.Name),
					staticGateways:  gatewayInfoList{},
					dynamicGateways: map[types.NamespacedName]*gatewayInfo{},
				}))
			updatePodLabels(pod2, map[string]string{"duplicated": "true"}, fakeClient)
			Eventually(func() *namespaceInfo { return getNamespaceInfo(namespaceTest.Name) }, 5).Should(Equal(
				&namespaceInfo{
					policies:       sets.New(dynamicPolicy.Name),
					staticGateways: gatewayInfoList{},
					dynamicGateways: map[types.NamespacedName]*gatewayInfo{
						{Namespace: "default", Name: pod3.Name}: {
							gws: sets.New(pod3.Status.PodIPs[0].IP),
						},
					},
				}))
			Eventually(func() *namespaceInfo { return getNamespaceInfo(namespaceTest2.Name) }, 5).Should(Equal(
				&namespaceInfo{
					policies:       sets.New(dynamicPolicyForTest2Only.Name),
					staticGateways: gatewayInfoList{},
					dynamicGateways: map[types.NamespacedName]*gatewayInfo{
						{Namespace: "default", Name: pod2.Name}: {
							gws: sets.New(pod2.Status.PodIPs[0].IP),
						},
					},
				}))
		})
		It("updates an existing pod gateway to match no policies", func() {
			initController([]runtime.Object{namespaceDefault, namespaceTest, pod1, pod2}, []runtime.Object{dynamicPolicy})
			Eventually(func() []string { return listRoutePolicyInCache() }, time.Minute).Should(HaveLen(1))
			Eventually(func() []string { return listNamespaceInfo() }, 5).Should(HaveLen(1))
			Eventually(func() *namespaceInfo { return getNamespaceInfo(namespaceTest.Name) }, 5).Should(Equal(
				&namespaceInfo{
					policies:       sets.New(dynamicPolicy.Name),
					staticGateways: gatewayInfoList{},
					dynamicGateways: map[types.NamespacedName]*gatewayInfo{
						{Namespace: "default", Name: pod1.Name}: {
							gws: sets.New(pod1.Status.PodIPs[0].IP),
						},
						{Namespace: "default", Name: pod2.Name}: {
							gws: sets.New(pod2.Status.PodIPs[0].IP),
						},
					},
				}))
			updatePodLabels(pod1, map[string]string{}, fakeClient)
			Eventually(func() *namespaceInfo { return getNamespaceInfo(namespaceTest.Name) }, 5, 1).Should(Equal(
				&namespaceInfo{
					policies:       sets.New(dynamicPolicy.Name),
					staticGateways: gatewayInfoList{},
					dynamicGateways: map[types.NamespacedName]*gatewayInfo{
						{Namespace: "default", Name: pod2.Name}: {
							gws: sets.New(pod2.Status.PodIPs[0].IP),
						},
					},
				}))
		})

		It("updates a pod to match a policy to a single namespace", func() {
			initController([]runtime.Object{namespaceDefault, namespaceTest, namespaceTest2, pod1}, []runtime.Object{dynamicPolicyForTest2Only, dynamicPolicy})
			Eventually(func() []string { return listNamespaceInfo() }, 5).Should(HaveLen(2))
			Eventually(func() *namespaceInfo { return getNamespaceInfo(namespaceTest.Name) }, 5).Should(Equal(
				&namespaceInfo{
					policies:       sets.New(dynamicPolicy.Name),
					staticGateways: gatewayInfoList{},
					dynamicGateways: map[types.NamespacedName]*gatewayInfo{
						{Namespace: "default", Name: pod1.Name}: {
							gws: sets.New(pod1.Status.PodIPs[0].IP),
						},
					},
				}))
			Eventually(func() *namespaceInfo { return getNamespaceInfo(namespaceTest2.Name) }, 5).Should(Equal(
				&namespaceInfo{
					policies:       sets.New(dynamicPolicyForTest2Only.Name),
					staticGateways: gatewayInfoList{},
					dynamicGateways: map[types.NamespacedName]*gatewayInfo{
						{Namespace: "default", Name: pod1.Name}: {
							gws: sets.New(pod1.Status.PodIPs[0].IP),
						},
					},
				}))
			updatePodLabels(pod1, map[string]string{"key": "pod"}, fakeClient)
			Eventually(func() *namespaceInfo { return getNamespaceInfo(namespaceTest.Name) }, 5).Should(Equal(
				&namespaceInfo{
					policies:       sets.New(dynamicPolicy.Name),
					staticGateways: gatewayInfoList{},
					dynamicGateways: map[types.NamespacedName]*gatewayInfo{
						{Namespace: "default", Name: pod1.Name}: {
							gws: sets.New(pod1.Status.PodIPs[0].IP),
						},
					},
				}))
			Eventually(func() *namespaceInfo { return getNamespaceInfo(namespaceTest2.Name) }, 5).Should(Equal(
				&namespaceInfo{
					policies:        sets.New(dynamicPolicyForTest2Only.Name),
					staticGateways:  gatewayInfoList{},
					dynamicGateways: map[types.NamespacedName]*gatewayInfo{},
				}))
		})

	})
})

func deletePod(pod *corev1.Pod, fakeClient *fake.Clientset) {

	err = fakeClient.CoreV1().Pods(pod.Namespace).Delete(context.Background(), pod.Name, v1.DeleteOptions{})
	Expect(err).NotTo(HaveOccurred())
}

func updatePodLabels(pod *corev1.Pod, newLabels map[string]string, fakeClient *fake.Clientset) {

	p, err := fakeClient.CoreV1().Pods(pod.Namespace).Get(context.TODO(), pod.Name, v1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	incrementResourceVersion(p)
	p.Labels = newLabels
	_, err = fakeClient.CoreV1().Pods(pod.Namespace).Update(context.Background(), p, v1.UpdateOptions{})
	Expect(err).NotTo(HaveOccurred())
}

func incrementResourceVersion(obj v1.Object) {
	var rs int64
	if obj.GetResourceVersion() != "" {
		rs, err = strconv.ParseInt(obj.GetResourceVersion(), 10, 64)
		Expect(err).NotTo(HaveOccurred())
	}
	rs++
	obj.SetResourceVersion(strconv.FormatInt(rs, 10))
}
