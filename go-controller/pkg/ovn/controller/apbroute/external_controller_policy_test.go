package apbroute

import (
	"context"
	"sort"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/fake"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	adminpolicybasedrouteapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1"
	adminpolicybasedrouteclient "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1/apis/clientset/versioned/fake"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"k8s.io/apimachinery/pkg/runtime"
)

func newPod(podName, namespace, hostIP string, labels map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: v1.ObjectMeta{Name: podName, Namespace: namespace,
			Labels: labels},
		Spec:   corev1.PodSpec{HostNetwork: true},
		Status: corev1.PodStatus{PodIPs: []corev1.PodIP{{IP: hostIP}}, Phase: corev1.PodRunning},
	}
}

func listRoutePolicyInCache() []string {
	return externalController.mgr.routePolicySyncCache.GetKeys()
}

var (
	externalController *ExternalGatewayMasterController
	iFactory           *factory.WatchFactory
	stopChan           chan (struct{})
	initialDB          libovsdbtest.TestSetup
	nbClient           libovsdbclient.Client
	nbsbCleanup        *libovsdbtest.Cleanup
	fakeRouteClient    *adminpolicybasedrouteclient.Clientset
	fakeClient         *fake.Clientset
	mgr                *externalPolicyManager
	err                error
)

func initController(k8sObjects, routePolicyObjects []runtime.Object) {
	stopChan = make(chan struct{})
	fakeClient = fake.NewSimpleClientset(k8sObjects...)
	fakeRouteClient = adminpolicybasedrouteclient.NewSimpleClientset(routePolicyObjects...)
	iFactory, err = factory.NewMasterWatchFactory(&util.OVNMasterClientset{KubeClient: fakeClient})
	Expect(err).NotTo(HaveOccurred())
	iFactory.Start()
	externalController, err = NewExternalMasterController(controllerName, fakeClient,
		fakeRouteClient,
		stopChan,
		iFactory.PodCoreInformer(),
		iFactory.NamespaceInformer(),
		iFactory.NodeCoreInformer().Lister(),
		nbClient,
		addressset.NewFakeAddressSetFactory(controllerName))
	Expect(err).NotTo(HaveOccurred())
	mgr = externalController.mgr
	go func() {
		externalController.Run(5)
	}()
}

var _ = Describe("OVN External Gateway policy", func() {

	var (
		namespaceDefault = &corev1.Namespace{
			ObjectMeta: v1.ObjectMeta{Name: "default",
				Labels: map[string]string{"name": "default"}}}
		namespaceTest = &corev1.Namespace{
			ObjectMeta: v1.ObjectMeta{Name: "test",
				Labels: map[string]string{"name": "test", "match": "test"}},
		}
		namespaceTest2 = &corev1.Namespace{
			ObjectMeta: v1.ObjectMeta{Name: "test2",
				Labels: map[string]string{"name": "test2", "match": "test"}},
		}

		dynamicPolicy = newPolicy(
			"dynamic",
			&v1.LabelSelector{MatchLabels: map[string]string{"name": "test"}},
			nil,
			&v1.LabelSelector{MatchLabels: map[string]string{"name": "default"}},
			&v1.LabelSelector{MatchLabels: map[string]string{"key": "pod"}},
			false,
		)

		staticPolicy = newPolicy(
			"static",
			&v1.LabelSelector{MatchLabels: map[string]string{"name": "test"}},
			sets.New(staticHopGWIP),
			nil,
			nil,
			false,
		)

		pod1 = newPod("pod_1", "default", "192.168.10.1", map[string]string{"key": "pod", "name": "pod1", "duplicated": "true"})
		pod2 = newPod("pod_2", "default", "192.168.20.1", map[string]string{"key": "pod", "name": "pod2"})
		pod3 = newPod("pod_3", "default", "192.168.30.1", map[string]string{"key": "pod", "name": "pod3"})
		pod4 = newPod("pod_4", "default", "192.168.40.1", map[string]string{"key": "pod", "name": "pod4"})
		pod5 = newPod("pod_5", "default", "192.168.50.1", map[string]string{"key": "pod", "name": "pod5"})
		pod6 = newPod("pod_6", "default", "192.168.60.1", map[string]string{"key": "pod", "name": "pod6"})
	)
	AfterEach(func() {
		close(stopChan)
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

	var _ = Context("When adding new policies", func() {

		var (
			namespaceTest3 = &corev1.Namespace{
				ObjectMeta: v1.ObjectMeta{Name: "test3",
					Labels: map[string]string{"name": "test3", "match": "test"}},
			}
			multipleMatchPolicy = newPolicy(
				"multiple",
				&v1.LabelSelector{MatchLabels: map[string]string{"match": "test"}},
				sets.New("10.10.10.1"),
				&v1.LabelSelector{MatchLabels: map[string]string{"name": "default"}},
				&v1.LabelSelector{MatchLabels: map[string]string{"key": "pod"}},
				false,
			)
		)
		It("registers the new policy with multiple namespace matching", func() {

			initController([]runtime.Object{namespaceDefault, namespaceTest, namespaceTest2, namespaceTest3, pod1}, []runtime.Object{multipleMatchPolicy})

			Eventually(func() []string { return listRoutePolicyInCache() }, 5).Should(HaveLen(1))
			Eventually(func(g Gomega) {
				p, found, _ := externalController.mgr.getRoutePolicyFromCache(multipleMatchPolicy.Name)
				g.Expect(found).To(BeTrue())
				g.Expect(p.Spec).To(BeEquivalentTo(multipleMatchPolicy.Spec))
			}, 5).Should(Succeed())
			Eventually(listNamespaceInfo(), 5).Should(HaveLen(3))
			Eventually(func() *namespaceInfo { return getNamespaceInfo(namespaceTest.Name) }, 5).Should(
				BeEquivalentTo(
					&namespaceInfo{
						policies:       sets.New(multipleMatchPolicy.Name),
						staticGateways: gatewayInfoList{{gws: sets.New(staticHopGWIP)}},
						dynamicGateways: map[types.NamespacedName]*gatewayInfo{
							{Namespace: "default", Name: "pod_1"}: {
								gws: sets.New("192.168.10.1"),
							},
						}}))

			Eventually(func() *namespaceInfo { return getNamespaceInfo(namespaceTest2.Name) }, 5).Should(
				BeEquivalentTo(
					&namespaceInfo{
						policies:       sets.New(multipleMatchPolicy.Name),
						staticGateways: gatewayInfoList{{gws: sets.New(staticHopGWIP)}},
						dynamicGateways: map[types.NamespacedName]*gatewayInfo{
							{Namespace: "default", Name: "pod_1"}: {
								gws: sets.New("192.168.10.1"),
							},
						}}))

			Eventually(func() *namespaceInfo { return getNamespaceInfo(namespaceTest3.Name) }, 5).Should(
				BeEquivalentTo(
					&namespaceInfo{
						policies:       sets.New(multipleMatchPolicy.Name),
						staticGateways: gatewayInfoList{{gws: sets.New(staticHopGWIP)}},
						dynamicGateways: map[types.NamespacedName]*gatewayInfo{
							{Namespace: "default", Name: "pod_1"}: {
								gws: sets.New("192.168.10.1"),
							},
						}}))
		})

		It("registers a new policy with no namespace match", func() {
			initController([]runtime.Object{namespaceTest2, namespaceDefault, pod1}, []runtime.Object{dynamicPolicy})

			Eventually(func() []string { return listRoutePolicyInCache() }, 5).Should(HaveLen(1))
			Eventually(listNamespaceInfo(), 5).Should(HaveLen(0))
		})

		It("registers a new policy with multiple dynamic and static GWs and bfd enabled on all gateways", func() {

			staticMultiIPPolicy := newPolicy("multiIPPolicy",
				&v1.LabelSelector{MatchLabels: map[string]string{"name": "test"}},
				sets.New("10.10.10.1", "10.10.10.2", "10.10.10.3", "10.10.10.3", "10.10.10.4"),
				&v1.LabelSelector{MatchLabels: map[string]string{"name": "default"}},
				&v1.LabelSelector{MatchLabels: map[string]string{"key": "pod"}},
				true,
			)
			initController([]runtime.Object{namespaceDefault, namespaceTest, pod1, pod2, pod3, pod4, pod5, pod6}, []runtime.Object{staticMultiIPPolicy})

			Eventually(func() []string { return listRoutePolicyInCache() }, 5).Should(HaveLen(1))
			Eventually(listNamespaceInfo(), 5).Should(HaveLen(1))
			Eventually(func() *namespaceInfo {
				f := getNamespaceInfo(namespaceTest.Name)
				sort.Sort(f.staticGateways)
				return f
			}, 5).
				Should(BeEquivalentTo(
					&namespaceInfo{
						policies: sets.New(staticMultiIPPolicy.Name),
						staticGateways: gatewayInfoList{
							{
								gws:        sets.New("10.10.10.1"),
								bfdEnabled: true,
							},
							{
								gws:        sets.New("10.10.10.2"),
								bfdEnabled: true,
							},
							{
								gws:        sets.New("10.10.10.3"),
								bfdEnabled: true,
							},
							{
								gws:        sets.New("10.10.10.4"),
								bfdEnabled: true,
							},
						},
						dynamicGateways: map[types.NamespacedName]*gatewayInfo{

							{Namespace: "default", Name: pod3.Name}: {
								gws:        sets.New(pod3.Status.PodIPs[0].IP),
								bfdEnabled: true,
							},
							{Namespace: "default", Name: pod4.Name}: {
								gws:        sets.New(pod4.Status.PodIPs[0].IP),
								bfdEnabled: true,
							},
							{Namespace: "default", Name: pod5.Name}: {
								gws:        sets.New(pod5.Status.PodIPs[0].IP),
								bfdEnabled: true,
							},
							{Namespace: "default", Name: pod6.Name}: {
								gws:        sets.New(pod6.Status.PodIPs[0].IP),
								bfdEnabled: true,
							},
							{Namespace: "default", Name: pod1.Name}: {
								gws:        sets.New(pod1.Status.PodIPs[0].IP),
								bfdEnabled: true,
							},
							{Namespace: "default", Name: pod2.Name}: {
								gws:        sets.New(pod2.Status.PodIPs[0].IP),
								bfdEnabled: true,
							},
						}}))

		})

		It("registers a second policy with no overlaping IPs", func() {

			initController([]runtime.Object{namespaceDefault, namespaceTest, pod1}, []runtime.Object{staticPolicy, dynamicPolicy})
			Eventually(func() []string { return listRoutePolicyInCache() }, 5).
				Should(HaveLen(2))
			Eventually(listNamespaceInfo(), 5).Should(HaveLen(1))
			Eventually(func() *namespaceInfo { return getNamespaceInfo(namespaceTest.Name) }, 5).Should(
				BeEquivalentTo(
					&namespaceInfo{
						policies: sets.New(staticPolicy.Name, dynamicPolicy.Name),
						staticGateways: gatewayInfoList{
							{
								gws: sets.New(staticHopGWIP),
							},
						},
						dynamicGateways: map[types.NamespacedName]*gatewayInfo{

							{Namespace: "default", Name: "pod_1"}: {
								gws: sets.New("192.168.10.1"),
							},
						}}))
		})
		It("registers policies with overlaping IPs for static and dynamic hops", func() {
			duplicatedStatic := newPolicy("overlappingStatic",
				&v1.LabelSelector{MatchLabels: map[string]string{"name": "test"}},
				sets.New(staticHopGWIP, "172.1.1.1"),
				nil,
				nil,
				false)
			duplicatedDynamic := newPolicy(
				"duplicatedDynamic",
				&v1.LabelSelector{MatchLabels: map[string]string{"name": "test"}},
				nil,
				&v1.LabelSelector{MatchLabels: map[string]string{"name": "default"}},
				&v1.LabelSelector{MatchLabels: map[string]string{"duplicated": "true"}},
				false,
			)
			initController([]runtime.Object{namespaceDefault, namespaceTest, pod1}, []runtime.Object{staticPolicy, duplicatedStatic, dynamicPolicy, duplicatedDynamic})

			Eventually(func() []string { return listRoutePolicyInCache() }, 5).
				Should(HaveLen(4))
			Eventually(listNamespaceInfo(), 5).Should(HaveLen(1))
			Eventually(func() *namespaceInfo {
				f := getNamespaceInfo(namespaceTest.Name)
				sort.Sort(f.staticGateways)
				return f
			}, 5).Should(
				BeEquivalentTo(
					&namespaceInfo{
						policies: sets.New(staticPolicy.Name, dynamicPolicy.Name, duplicatedStatic.Name, duplicatedDynamic.Name),
						staticGateways: gatewayInfoList{
							{
								gws: sets.New(staticHopGWIP),
							},
							{
								gws: sets.New("172.1.1.1"),
							},
						},
						dynamicGateways: map[types.NamespacedName]*gatewayInfo{

							{Namespace: "default", Name: "pod_1"}: {
								gws: sets.New("192.168.10.1"),
							},
						}}))
		})
	})

	var _ = Context("when deleting a policy", func() {

		var (
			duplicatedStatic = newPolicy("duplicatedStatic",
				&v1.LabelSelector{MatchLabels: map[string]string{"name": "test"}},
				sets.New(staticHopGWIP, "172.1.1.1"),
				nil,
				nil,
				false)
			duplicatedDynamic = newPolicy(
				"duplicatedDynamic",
				&v1.LabelSelector{MatchLabels: map[string]string{"name": "test"}},
				nil,
				&v1.LabelSelector{MatchLabels: map[string]string{"name": "default"}},
				&v1.LabelSelector{MatchLabels: map[string]string{"duplicated": "true"}},
				false,
			)
		)
		It("validates that the IPs of the policy are no longer reflected on the targeted namespaces when the policy is deleted an no other policy overlaps", func() {
			initController([]runtime.Object{namespaceDefault, namespaceTest, pod1}, []runtime.Object{staticPolicy, dynamicPolicy})

			Eventually(func() []string { return listRoutePolicyInCache() }, 5).Should(HaveLen(2))
			Eventually(func() []string { return listNamespaceInfo() }, 5).Should(HaveLen(1))
			deletePolicy(staticPolicy.Name, fakeRouteClient)
			Eventually(func() []string { return listRoutePolicyInCache() }, 5).Should(HaveLen(1))
			Eventually(func() *namespaceInfo { return getNamespaceInfo(namespaceTest.Name) }, 5).Should(
				BeEquivalentTo(
					&namespaceInfo{
						policies:       sets.New(dynamicPolicy.Name),
						staticGateways: gatewayInfoList{},
						dynamicGateways: map[types.NamespacedName]*gatewayInfo{

							{Namespace: "default", Name: "pod_1"}: {
								gws: sets.New("192.168.10.1"),
							},
						}}))
		})
		It("validates that the IPs of a deleted policy won't show up in a non-matching namespace after the policy is deleted", func() {
			initController([]runtime.Object{namespaceDefault, namespaceTest2, pod1}, []runtime.Object{staticPolicy, dynamicPolicy})

			Eventually(func() []string { return listRoutePolicyInCache() }, 5).Should(HaveLen(2))
			Eventually(func() []string { return listNamespaceInfo() }, 5).Should(HaveLen(0))
			deletePolicy(dynamicPolicy.Name, fakeRouteClient)
			Eventually(func() []string { return listRoutePolicyInCache() }, 5).Should(HaveLen(1))
			Eventually(func() []string { return listNamespaceInfo() }, 5).Should(HaveLen(0))
		})

		It("validates that an overlapping IP from another policy will not be deleted when one of the overlaping policies is deleted", func() {

			initController([]runtime.Object{namespaceTest, namespaceDefault, pod1}, []runtime.Object{staticPolicy, duplicatedStatic, dynamicPolicy, duplicatedDynamic})

			Eventually(func() []string { return listRoutePolicyInCache() }, 5).Should(HaveLen(4))
			deletePolicy(staticPolicy.Name, fakeRouteClient)
			deletePolicy(dynamicPolicy.Name, fakeRouteClient)
			Eventually(func() []string { return listRoutePolicyInCache() }, 5).Should(HaveLen(2))
			Eventually(func() *namespaceInfo {
				f := getNamespaceInfo(namespaceTest.Name)
				sort.Sort(f.staticGateways)
				return f
			}, 5).Should(
				BeEquivalentTo(
					&namespaceInfo{
						policies: sets.New(duplicatedStatic.Name, duplicatedDynamic.Name),
						staticGateways: gatewayInfoList{
							{
								gws: sets.New(staticHopGWIP),
							},
							{
								gws: sets.New("172.1.1.1"),
							},
						},
						dynamicGateways: map[types.NamespacedName]*gatewayInfo{

							{Namespace: "default", Name: "pod_1"}: {
								gws: sets.New("192.168.10.1"),
							},
						}}))
		})
	})

	var _ = Context("when updating a policy", func() {

		It("validates that changing the from selector will retarget the new namespaces", func() {
			initController([]runtime.Object{namespaceDefault, namespaceTest, namespaceTest2, pod1}, []runtime.Object{dynamicPolicy})

			Eventually(func() []string { return listRoutePolicyInCache() }, 5).Should(HaveLen(1))
			Eventually(func() []string { return listNamespaceInfo() }, 5).Should(HaveLen(1))

			Eventually(func() *namespaceInfo { return getNamespaceInfo(namespaceTest.Name) }, 5).Should(
				BeEquivalentTo(
					&namespaceInfo{
						policies:       sets.New(dynamicPolicy.Name),
						staticGateways: gatewayInfoList{},
						dynamicGateways: map[types.NamespacedName]*gatewayInfo{

							{Namespace: "default", Name: "pod_1"}: {
								gws: sets.New("192.168.10.1"),
							},
						}}))

			p, err := fakeRouteClient.K8sV1().AdminPolicyBasedExternalRoutes().Get(context.TODO(), dynamicPolicy.Name, v1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			p.Spec.From.NamespaceSelector = v1.LabelSelector{MatchLabels: namespaceTest2.Labels}
			p.Generation++
			_, err = fakeRouteClient.K8sV1().AdminPolicyBasedExternalRoutes().Update(context.Background(), p, v1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())

			Eventually(func() []string { return listNamespaceInfo() }, 5).Should(HaveLen(1))
			Eventually(func() *namespaceInfo { return getNamespaceInfo(namespaceTest.Name) }, 5).Should(BeNil())
			Eventually(func() *namespaceInfo { return getNamespaceInfo(namespaceTest2.Name) }, 5).Should(
				BeEquivalentTo(
					&namespaceInfo{
						policies:       sets.New(dynamicPolicy.Name),
						staticGateways: gatewayInfoList{},
						dynamicGateways: map[types.NamespacedName]*gatewayInfo{

							{Namespace: "default", Name: "pod_1"}: {
								gws: sets.New("192.168.10.1"),
							},
						}}))
		})
		It("validates that changing a static hop from an existing policy will be applied to the target namespaces", func() {
			newStaticIP := "10.30.20.1"
			staticPolicy := newPolicy(
				"static",
				&v1.LabelSelector{MatchLabels: map[string]string{"name": "test"}},
				sets.New(staticHopGWIP, newStaticIP),
				nil,
				nil,
				false,
			)
			initController([]runtime.Object{namespaceDefault, namespaceTest}, []runtime.Object{staticPolicy})

			Eventually(func() []string { return listRoutePolicyInCache() }, 5).Should(HaveLen(1))
			Eventually(func() []string { return listNamespaceInfo() }, 5).Should(HaveLen(1))
			Eventually(func() *namespaceInfo {
				nsInfo := getNamespaceInfo(namespaceTest.Name)
				sort.Sort(nsInfo.staticGateways)
				return nsInfo
			}, 5).Should(
				BeEquivalentTo(
					&namespaceInfo{
						policies: sets.New(staticPolicy.Name),
						staticGateways: gatewayInfoList{
							{
								gws: sets.New(staticHopGWIP),
							},
							{
								gws: sets.New(newStaticIP),
							},
						},
						dynamicGateways: make(map[types.NamespacedName]*gatewayInfo, 0),
					}))

			p, err := fakeRouteClient.K8sV1().AdminPolicyBasedExternalRoutes().Get(context.TODO(), staticPolicy.Name, v1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			p.Spec.NextHops.StaticHops = []*adminpolicybasedrouteapi.StaticHop{
				{IP: newStaticIP},
			}
			p.Generation++
			_, err = fakeRouteClient.K8sV1().AdminPolicyBasedExternalRoutes().Update(context.Background(), p, v1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())

			Eventually(func() *namespaceInfo { return getNamespaceInfo(namespaceTest.Name) }, 5*time.Hour).Should(
				BeEquivalentTo(
					&namespaceInfo{
						policies: sets.New(staticPolicy.Name),
						staticGateways: gatewayInfoList{
							{
								gws: sets.New(newStaticIP),
							},
						},
						dynamicGateways: make(map[types.NamespacedName]*gatewayInfo, 0),
					}))

		})
		It("validates that changes to a dynamic hop from an existing policy will be applied to the target namespaces", func() {
			singlePodDynamicPolicy := newPolicy(
				"singlePod",
				&v1.LabelSelector{MatchLabels: map[string]string{"name": "test"}},
				nil,
				&v1.LabelSelector{MatchLabels: map[string]string{"name": "default"}},
				&v1.LabelSelector{MatchLabels: map[string]string{"name": "pod1"}},
				false,
			)
			initController([]runtime.Object{namespaceDefault, namespaceTest, pod1, pod2}, []runtime.Object{singlePodDynamicPolicy})

			Eventually(func() []string { return listRoutePolicyInCache() }, 5).Should(HaveLen(1))
			Eventually(func() []string { return listNamespaceInfo() }, 5).Should(HaveLen(1))
			Eventually(func() *namespaceInfo { return getNamespaceInfo(namespaceTest.Name) }, 5).Should(
				BeEquivalentTo(
					&namespaceInfo{
						policies:       sets.New(singlePodDynamicPolicy.Name),
						staticGateways: gatewayInfoList{},
						dynamicGateways: map[types.NamespacedName]*gatewayInfo{
							{Namespace: "default", Name: "pod_1"}: {
								gws: sets.New(pod1.Status.PodIPs[0].IP),
							},
						}}))

			p, err := fakeRouteClient.K8sV1().AdminPolicyBasedExternalRoutes().Get(context.TODO(), singlePodDynamicPolicy.Name, v1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			p.Spec.NextHops.DynamicHops[0].PodSelector = v1.LabelSelector{MatchLabels: map[string]string{"name": "pod2"}}
			p.Generation++
			_, err = fakeRouteClient.K8sV1().AdminPolicyBasedExternalRoutes().Update(context.Background(), p, v1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())
			Eventually(func() *namespaceInfo { return getNamespaceInfo(namespaceTest.Name) }, 5).Should(
				BeEquivalentTo(
					&namespaceInfo{
						policies:       sets.New(singlePodDynamicPolicy.Name),
						staticGateways: gatewayInfoList{},
						dynamicGateways: map[types.NamespacedName]*gatewayInfo{
							{Namespace: "default", Name: "pod_2"}: {
								gws: sets.New(pod2.Status.PodIPs[0].IP),
							},
						}}))
		})
		It("validates that removing one of the static hop IPs will be reflected in the route policy", func() {

			staticMultiIPPolicy := newPolicy("multiIPPolicy",
				&v1.LabelSelector{MatchLabels: map[string]string{"name": "test"}},
				sets.New("10.10.10.1", "10.10.10.2", "10.10.10.3", "10.10.10.3", "10.10.10.4"),
				nil, nil,
				true,
			)
			initController([]runtime.Object{namespaceDefault, namespaceTest, pod1, pod2, pod3, pod4, pod5, pod6}, []runtime.Object{staticMultiIPPolicy})

			Eventually(func() []string { return listRoutePolicyInCache() }, 5).Should(HaveLen(1))
			Eventually(listNamespaceInfo(), 5).Should(HaveLen(1))
			Eventually(func() *namespaceInfo {
				f := getNamespaceInfo(namespaceTest.Name)
				sort.Sort(f.staticGateways)
				return f
			}, 5).
				Should(BeEquivalentTo(
					&namespaceInfo{
						policies: sets.New(staticMultiIPPolicy.Name),
						staticGateways: gatewayInfoList{
							{
								gws:        sets.New("10.10.10.1"),
								bfdEnabled: true,
							},
							{
								gws:        sets.New("10.10.10.2"),
								bfdEnabled: true,
							},
							{
								gws:        sets.New("10.10.10.3"),
								bfdEnabled: true,
							},
							{
								gws:        sets.New("10.10.10.4"),
								bfdEnabled: true,
							},
						},
						dynamicGateways: map[types.NamespacedName]*gatewayInfo{},
					}))
			p, err := fakeRouteClient.K8sV1().AdminPolicyBasedExternalRoutes().Get(context.TODO(), staticMultiIPPolicy.Name, v1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			p.Spec.NextHops.StaticHops = []*adminpolicybasedrouteapi.StaticHop{
				{
					IP:         "10.10.10.1",
					BFDEnabled: true,
				},
				{
					IP:         "10.10.10.2",
					BFDEnabled: true,
				},
				{
					IP:         "10.10.10.3",
					BFDEnabled: true,
				},
			}
			p.Generation++
			_, err = fakeRouteClient.K8sV1().AdminPolicyBasedExternalRoutes().Update(context.Background(), p, v1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())
			By("Validating the static refernces don't contain the last element")
			Eventually(func() *namespaceInfo {
				f := getNamespaceInfo(namespaceTest.Name)
				sort.Sort(f.staticGateways)
				return f
			}, 5).
				Should(BeEquivalentTo(
					&namespaceInfo{
						policies: sets.New(staticMultiIPPolicy.Name),
						staticGateways: gatewayInfoList{
							{
								gws:        sets.New("10.10.10.1"),
								bfdEnabled: true,
							},
							{
								gws:        sets.New("10.10.10.2"),
								bfdEnabled: true,
							},
							{
								gws:        sets.New("10.10.10.3"),
								bfdEnabled: true,
							},
						},
						dynamicGateways: map[types.NamespacedName]*gatewayInfo{}}))
		})
		It("validates that removing a duplicated static hop IP from an overlapping policy static hop will keep the static IP in the route policy", func() {

			staticMultiIPPolicy := newPolicy("multiIPPolicy",
				&v1.LabelSelector{MatchLabels: map[string]string{"name": "test"}},
				sets.New("20.10.10.1", "20.10.10.2", "20.10.10.3", "20.10.10.4", staticHopGWIP),
				nil, nil,
				false,
			)
			initController([]runtime.Object{namespaceDefault, namespaceTest, pod1, pod2, pod3, pod4, pod5, pod6}, []runtime.Object{staticMultiIPPolicy, staticPolicy})

			Eventually(func() []string { return listRoutePolicyInCache() }, 5).Should(HaveLen(2))
			Eventually(listNamespaceInfo(), 5).Should(HaveLen(1))
			Eventually(func() gatewayInfoList {
				f := getNamespaceInfo(namespaceTest.Name)
				sort.Sort(f.staticGateways)
				return f.staticGateways
			}, 5).
				Should(BeEquivalentTo(
					gatewayInfoList{
						{
							gws: sets.New(staticHopGWIP),
						},
						{
							gws: sets.New("20.10.10.1"),
						},
						{
							gws: sets.New("20.10.10.2"),
						},
						{
							gws: sets.New("20.10.10.3"),
						},
						{
							gws: sets.New("20.10.10.4"),
						},
					}))
			Eventually(getNamespaceInfo(namespaceTest.Name).policies).Should(BeEquivalentTo(sets.New(staticMultiIPPolicy.Name, staticPolicy.Name)))
			p, err := fakeRouteClient.K8sV1().AdminPolicyBasedExternalRoutes().Get(context.TODO(), staticMultiIPPolicy.Name, v1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			p.Spec.NextHops.StaticHops = []*adminpolicybasedrouteapi.StaticHop{
				{
					IP: "20.10.10.2",
				},
				{
					IP: "20.10.10.3",
				},
				{
					IP: "20.10.10.4",
				},
				{
					IP: "20.10.20.1",
				},
			}
			p.Generation++
			_, err = fakeRouteClient.K8sV1().AdminPolicyBasedExternalRoutes().Update(context.Background(), p, v1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())
			By("Validating the static refernces don't contain the last element")
			Eventually(func() gatewayInfoList {
				f := getNamespaceInfo(namespaceTest.Name)
				sort.Sort(f.staticGateways)
				return f.staticGateways
			}, 5).
				Should(BeEquivalentTo(
					gatewayInfoList{
						{
							gws: sets.New(staticHopGWIP),
						},
						{
							gws: sets.New("20.10.10.1"),
						},
						{
							gws: sets.New("20.10.10.2"),
						},
						{
							gws: sets.New("20.10.10.3"),
						},
						{
							gws: sets.New("20.10.10.4"),
						},
					}))
			Eventually(getNamespaceInfo(namespaceTest.Name).policies).Should(BeEquivalentTo(sets.New(staticMultiIPPolicy.Name, staticPolicy.Name)))
		})
	})
})
