package apbroute

import (
	"context"

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
	nbsbCleanup        *libovsdbtest.Context
	fakeRouteClient    *adminpolicybasedrouteclient.Clientset
	fakeClient         *fake.Clientset
	mgr                *externalPolicyManager
	err                error
)

func createTestNBGlobal(nbClient libovsdbclient.Client, zone string) error {
	nbGlobal := &nbdb.NBGlobal{Name: zone}
	ops, err := nbClient.Create(nbGlobal)
	if err != nil {
		return err
	}

	_, err = nbClient.Transact(context.Background(), ops...)
	if err != nil {
		return err
	}

	return nil
}

func deleteTestNBGlobal(nbClient libovsdbclient.Client, zone string) error {
	p := func(nbGlobal *nbdb.NBGlobal) bool {
		return true
	}

	ops, err := nbClient.WhereCache(p).Delete()
	if err != nil {
		return err
	}

	_, err = nbClient.Transact(context.Background(), ops...)
	if err != nil {
		return err
	}

	return nil
}

func initController(k8sObjects, routePolicyObjects []runtime.Object) {
	var nbZoneFailed bool
	stopChan = make(chan struct{})
	fakeClient = fake.NewSimpleClientset(k8sObjects...)
	fakeRouteClient = adminpolicybasedrouteclient.NewSimpleClientset(routePolicyObjects...)
	iFactory, err = factory.NewMasterWatchFactory(&util.OVNMasterClientset{KubeClient: fakeClient})
	Expect(err).NotTo(HaveOccurred())
	iFactory.Start()
	// Try to get the NBZone.  If there is an error, create NB_Global record.
	// Otherwise NewController() will return error since it
	// calls util.GetNBZone().
	_, err = util.GetNBZone(nbClient)
	if err != nil {
		nbZoneFailed = true
		err = createTestNBGlobal(nbClient, "global")
		Expect(err).NotTo(HaveOccurred())
	}
	controllerName := "test-controller"
	externalController, err = NewExternalMasterController(fakeClient,
		fakeRouteClient,
		stopChan,
		iFactory.PodCoreInformer(),
		iFactory.NamespaceInformer(),
		iFactory.NodeCoreInformer().Lister(),
		nbClient,
		addressset.NewFakeAddressSetFactory(controllerName),
		controllerName)
	Expect(err).NotTo(HaveOccurred())

	if nbZoneFailed {
		// Delete the NBGlobal row as this function created it.  Otherwise many tests would fail while
		// checking the expectedData in the NBDB.
		err = deleteTestNBGlobal(nbClient, "global")
		Expect(err).NotTo(HaveOccurred())
	}

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
				sets.New(staticHopGWIP),
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
				g.Expect(p.Spec).To(BeComparableTo(multipleMatchPolicy.Spec, cmpOpts...))
			}, 5).Should(Succeed())
			Eventually(listNamespaceInfo, 5).Should(HaveLen(3))
			expected := &namespaceInfo{
				Policies:       sets.New(multipleMatchPolicy.Name),
				StaticGateways: gatewayInfoList{newGatewayInfo(sets.New(staticHopGWIP), false)},
				DynamicGateways: map[types.NamespacedName]*gatewayInfo{
					{Namespace: "default", Name: pod1.Name}: newGatewayInfo(sets.New(pod1.Status.PodIPs[0].IP), false),
				}}
			Eventually(func() *namespaceInfo {
				return getNamespaceInfo(namespaceTest.Name)
			}, 5).Should(BeComparableTo(expected, cmpOpts...))
			Eventually(func() *namespaceInfo {
				return getNamespaceInfo(namespaceTest2.Name)
			}, 5).Should(BeComparableTo(expected, cmpOpts...))
			Eventually(func() *namespaceInfo {
				return getNamespaceInfo(namespaceTest3.Name)
			}, 5).Should(BeComparableTo(expected, cmpOpts...))
		})

		It("registers a new policy with no namespace match", func() {
			initController([]runtime.Object{namespaceTest2, namespaceDefault, pod1}, []runtime.Object{dynamicPolicy})

			Eventually(func() []string { return listRoutePolicyInCache() }, 5).Should(HaveLen(1))
			Eventually(listNamespaceInfo, 5).Should(HaveLen(0))
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
			Eventually(listNamespaceInfo, 5).Should(HaveLen(1))

			Eventually(func() *namespaceInfo {
				return getNamespaceInfo(namespaceTest.Name)
			}, 5).Should(BeComparableTo(
				&namespaceInfo{
					Policies: sets.New(staticMultiIPPolicy.Name),
					StaticGateways: gatewayInfoList{
						newGatewayInfo(sets.New("10.10.10.1"), true),
						newGatewayInfo(sets.New("10.10.10.2"), true),
						newGatewayInfo(sets.New("10.10.10.3"), true),
						newGatewayInfo(sets.New("10.10.10.4"), true),
					},
					DynamicGateways: map[types.NamespacedName]*gatewayInfo{
						{Namespace: "default", Name: pod3.Name}: newGatewayInfo(sets.New(pod3.Status.PodIPs[0].IP), true),
						{Namespace: "default", Name: pod4.Name}: newGatewayInfo(sets.New(pod4.Status.PodIPs[0].IP), true),
						{Namespace: "default", Name: pod5.Name}: newGatewayInfo(sets.New(pod5.Status.PodIPs[0].IP), true),
						{Namespace: "default", Name: pod6.Name}: newGatewayInfo(sets.New(pod6.Status.PodIPs[0].IP), true),
						{Namespace: "default", Name: pod1.Name}: newGatewayInfo(sets.New(pod1.Status.PodIPs[0].IP), true),
						{Namespace: "default", Name: pod2.Name}: newGatewayInfo(sets.New(pod2.Status.PodIPs[0].IP), true),
					}},
				cmpOpts...))

		})

		It("registers a second policy with no overlaping IPs", func() {

			initController([]runtime.Object{namespaceDefault, namespaceTest, pod1}, []runtime.Object{staticPolicy, dynamicPolicy})
			Eventually(func() []string { return listRoutePolicyInCache() }, 5).
				Should(HaveLen(2))
			Eventually(listNamespaceInfo, 5).Should(HaveLen(1))
			Eventually(func() *namespaceInfo {
				return getNamespaceInfo(namespaceTest.Name)
			}, 5).Should(BeComparableTo(
				&namespaceInfo{Policies: sets.New(staticPolicy.Name, dynamicPolicy.Name),
					StaticGateways: gatewayInfoList{
						newGatewayInfo(sets.New(staticHopGWIP), false),
					},
					DynamicGateways: map[types.NamespacedName]*gatewayInfo{
						{Namespace: "default", Name: pod1.Name}: newGatewayInfo(sets.New(pod1.Status.PodIPs[0].IP), false),
					}},
				cmpOpts...))
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
			Eventually(listNamespaceInfo, 5).Should(HaveLen(1))

			Eventually(func() *namespaceInfo {
				return getNamespaceInfo(namespaceTest.Name)
			}, 5).Should(BeComparableTo(
				&namespaceInfo{
					Policies: sets.New(staticPolicy.Name, dynamicPolicy.Name, duplicatedStatic.Name, duplicatedDynamic.Name),
					StaticGateways: gatewayInfoList{
						newGatewayInfo(sets.New(staticHopGWIP), false),
						newGatewayInfo(sets.New("172.1.1.1"), false),
					},
					DynamicGateways: map[types.NamespacedName]*gatewayInfo{
						{Namespace: "default", Name: pod1.Name}: newGatewayInfo(sets.New(pod1.Status.PodIPs[0].IP), false),
					}},
				cmpOpts...))
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
			Eventually(func() []string { return listRoutePolicyInCache() }, 5, 1).Should(HaveLen(1))

			Eventually(func() *namespaceInfo {
				return getNamespaceInfo(namespaceTest.Name)
			}, 5).Should(BeComparableTo(
				&namespaceInfo{
					Policies:       sets.New(dynamicPolicy.Name),
					StaticGateways: gatewayInfoList{},
					DynamicGateways: map[types.NamespacedName]*gatewayInfo{
						{Namespace: "default", Name: pod1.Name}: newGatewayInfo(sets.New(pod1.Status.PodIPs[0].IP), false),
					}},
				cmpOpts...))
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
			Eventually(func() []string { return listRoutePolicyInCache() }, 5, 1).Should(HaveLen(4))

			deletePolicy(staticPolicy.Name, fakeRouteClient)
			deletePolicy(dynamicPolicy.Name, fakeRouteClient)
			Eventually(func() []string { return listRoutePolicyInCache() }, 5, 1).Should(HaveLen(2))

			Eventually(func() *namespaceInfo {
				return getNamespaceInfo(namespaceTest.Name)
			}, 5, 1).Should(BeComparableTo(
				&namespaceInfo{
					Policies: sets.New(duplicatedStatic.Name, duplicatedDynamic.Name),
					StaticGateways: gatewayInfoList{
						newGatewayInfo(sets.New(staticHopGWIP), false),
						newGatewayInfo(sets.New("172.1.1.1"), false),
					},
					DynamicGateways: map[types.NamespacedName]*gatewayInfo{
						{Namespace: "default", Name: pod1.Name}: newGatewayInfo(sets.New(pod1.Status.PodIPs[0].IP), false),
					}},
				cmpOpts...))
		})
	})

	var _ = Context("when updating a policy", func() {

		It("validates that changing the from selector will retarget the new namespaces", func() {
			initController([]runtime.Object{namespaceDefault, namespaceTest, namespaceTest2, pod1}, []runtime.Object{dynamicPolicy})

			Eventually(func() []string { return listRoutePolicyInCache() }, 5).Should(HaveLen(1))
			Eventually(func() []string { return listNamespaceInfo() }, 5).Should(HaveLen(1))
			expected := &namespaceInfo{
				Policies:       sets.New(dynamicPolicy.Name),
				StaticGateways: gatewayInfoList{},
				DynamicGateways: map[types.NamespacedName]*gatewayInfo{
					{Namespace: "default", Name: pod1.Name}: newGatewayInfo(sets.New(pod1.Status.PodIPs[0].IP), false),
				}}
			Eventually(func() *namespaceInfo {
				return getNamespaceInfo(namespaceTest.Name)
			}, 5).Should(BeComparableTo(expected, cmpOpts...))

			p, err := fakeRouteClient.K8sV1().AdminPolicyBasedExternalRoutes().Get(context.TODO(), dynamicPolicy.Name, v1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			p.Spec.From.NamespaceSelector = v1.LabelSelector{MatchLabels: namespaceTest2.Labels}
			p.Generation++
			lastUpdate := p.Status.LastTransitionTime
			_, err = fakeRouteClient.K8sV1().AdminPolicyBasedExternalRoutes().Update(context.Background(), p, v1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())
			Eventually(func() v1.Time {
				p, err = fakeRouteClient.K8sV1().AdminPolicyBasedExternalRoutes().Get(context.TODO(), dynamicPolicy.Name, v1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				return p.Status.LastTransitionTime
			}).Should(Not(Equal(lastUpdate)))
			Eventually(func() []string { return listNamespaceInfo() }, 5).Should(HaveLen(1))
			Eventually(func() *namespaceInfo {
				return getNamespaceInfo(namespaceTest2.Name)
			}, 5).Should(BeComparableTo(expected, cmpOpts...))

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
				return getNamespaceInfo(namespaceTest.Name)
			}, 5).Should(BeComparableTo(
				&namespaceInfo{
					Policies: sets.New(staticPolicy.Name),
					StaticGateways: gatewayInfoList{
						newGatewayInfo(sets.New(staticHopGWIP), false),
						newGatewayInfo(sets.New(newStaticIP), false),
					},
					DynamicGateways: make(map[types.NamespacedName]*gatewayInfo, 0),
				},
				cmpOpts...))

			p, err := fakeRouteClient.K8sV1().AdminPolicyBasedExternalRoutes().Get(context.TODO(), staticPolicy.Name, v1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			p.Spec.NextHops.StaticHops = []*adminpolicybasedrouteapi.StaticHop{
				{IP: newStaticIP},
			}
			p.Generation++
			lastUpdate := p.Status.LastTransitionTime
			_, err = fakeRouteClient.K8sV1().AdminPolicyBasedExternalRoutes().Update(context.Background(), p, v1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())
			Eventually(func() v1.Time {
				p, err = fakeRouteClient.K8sV1().AdminPolicyBasedExternalRoutes().Get(context.TODO(), staticPolicy.Name, v1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				return p.Status.LastTransitionTime
			}).Should(Not(Equal(lastUpdate)))

			Eventually(func() *namespaceInfo {
				return getNamespaceInfo(namespaceTest.Name)
			}, 5).Should(BeComparableTo(
				&namespaceInfo{
					Policies: sets.New(staticPolicy.Name),
					StaticGateways: gatewayInfoList{
						newGatewayInfo(sets.New(newStaticIP), false),
					},
					DynamicGateways: make(map[types.NamespacedName]*gatewayInfo, 0),
				},
				cmpOpts...))

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
			Eventually(func() *namespaceInfo {
				return getNamespaceInfo(namespaceTest.Name)
			}, 5).Should(BeComparableTo(
				&namespaceInfo{
					Policies:       sets.New(singlePodDynamicPolicy.Name),
					StaticGateways: gatewayInfoList{},
					DynamicGateways: map[types.NamespacedName]*gatewayInfo{
						{Namespace: pod1.Namespace, Name: pod1.Name}: newGatewayInfo(sets.New(pod1.Status.PodIPs[0].IP), false),
					}},
				cmpOpts...))

			p, err := fakeRouteClient.K8sV1().AdminPolicyBasedExternalRoutes().Get(context.TODO(), singlePodDynamicPolicy.Name, v1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			p.Spec.NextHops.DynamicHops[0].PodSelector = v1.LabelSelector{MatchLabels: map[string]string{"name": "pod2"}}
			p.Generation++
			lastUpdate := p.Status.LastTransitionTime
			_, err = fakeRouteClient.K8sV1().AdminPolicyBasedExternalRoutes().Update(context.Background(), p, v1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())
			Eventually(func() v1.Time {
				p, err = fakeRouteClient.K8sV1().AdminPolicyBasedExternalRoutes().Get(context.TODO(), singlePodDynamicPolicy.Name, v1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				return p.Status.LastTransitionTime
			}).Should(Not(Equal(lastUpdate)))

			Eventually(func() *namespaceInfo {
				return getNamespaceInfo(namespaceTest.Name)
			}, 5).Should(BeComparableTo(
				&namespaceInfo{
					Policies:       sets.New(singlePodDynamicPolicy.Name),
					StaticGateways: gatewayInfoList{},
					DynamicGateways: map[types.NamespacedName]*gatewayInfo{
						{Namespace: pod2.Namespace, Name: pod2.Name}: newGatewayInfo(sets.New(pod2.Status.PodIPs[0].IP), false),
					}},
				cmpOpts...))

		})
		It("validates that removing one of the static hop IPs will be reflected in the route policy", func() {

			staticMultiIPPolicy := newPolicy("multiIPPolicy",
				&v1.LabelSelector{MatchLabels: map[string]string{"name": "test"}},
				sets.New("10.10.10.1", "10.10.10.2", "10.10.10.3", "10.10.10.4"),
				nil, nil,
				true,
			)
			initController([]runtime.Object{namespaceDefault, namespaceTest}, []runtime.Object{staticMultiIPPolicy})

			Eventually(func() []string { return listRoutePolicyInCache() }, 5).Should(HaveLen(1))
			Eventually(listNamespaceInfo, 5).Should(HaveLen(1))

			Eventually(func() *namespaceInfo {
				return getNamespaceInfo(namespaceTest.Name)
			}, 5).Should(BeComparableTo(
				&namespaceInfo{
					Policies: sets.New(staticMultiIPPolicy.Name),
					StaticGateways: gatewayInfoList{
						newGatewayInfo(sets.New("10.10.10.1"), true),
						newGatewayInfo(sets.New("10.10.10.2"), true),
						newGatewayInfo(sets.New("10.10.10.3"), true),
						newGatewayInfo(sets.New("10.10.10.4"), true),
					},
					DynamicGateways: map[types.NamespacedName]*gatewayInfo{},
				},
				cmpOpts...))
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
			lastUpdate := p.Status.LastTransitionTime
			_, err = fakeRouteClient.K8sV1().AdminPolicyBasedExternalRoutes().Update(context.Background(), p, v1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())
			Eventually(func() v1.Time {
				p, err = fakeRouteClient.K8sV1().AdminPolicyBasedExternalRoutes().Get(context.TODO(), staticMultiIPPolicy.Name, v1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				return p.Status.LastTransitionTime
			}).Should(Not(Equal(lastUpdate)))

			By("Validating the static refernces don't contain the last element")
			Eventually(func() *namespaceInfo {
				return getNamespaceInfo(namespaceTest.Name)
			}, 5).Should(BeComparableTo(
				&namespaceInfo{
					Policies: sets.New(staticMultiIPPolicy.Name),
					StaticGateways: gatewayInfoList{
						newGatewayInfo(sets.New("10.10.10.1"), true),
						newGatewayInfo(sets.New("10.10.10.2"), true),
						newGatewayInfo(sets.New("10.10.10.3"), true),
					},
					DynamicGateways: map[types.NamespacedName]*gatewayInfo{}},
				cmpOpts...))
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
			Eventually(listNamespaceInfo, 5).Should(HaveLen(1))
			expected := &namespaceInfo{
				Policies: sets.New(staticMultiIPPolicy.Name, staticPolicy.Name),
				StaticGateways: gatewayInfoList{
					newGatewayInfo(sets.New(staticHopGWIP), false),
					newGatewayInfo(sets.New("20.10.10.1"), false),
					newGatewayInfo(sets.New("20.10.10.2"), false),
					newGatewayInfo(sets.New("20.10.10.3"), false),
					newGatewayInfo(sets.New("20.10.10.4"), false),
				},
				DynamicGateways: map[types.NamespacedName]*gatewayInfo{}}
			Eventually(func() *namespaceInfo {
				return getNamespaceInfo(namespaceTest.Name)
			}, 5).Should(BeComparableTo(expected, cmpOpts...))

			p, err := fakeRouteClient.K8sV1().AdminPolicyBasedExternalRoutes().Get(context.TODO(), staticMultiIPPolicy.Name, v1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			p.Spec.NextHops.StaticHops = []*adminpolicybasedrouteapi.StaticHop{
				{
					IP: "20.10.10.1",
				},
				{
					IP: "20.10.10.2",
				},
				{
					IP: "20.10.10.3",
				},
				{
					IP: "20.10.10.4",
				},
			}
			p.Generation++
			lastUpdate := p.Status.LastTransitionTime
			_, err = fakeRouteClient.K8sV1().AdminPolicyBasedExternalRoutes().Update(context.Background(), p, v1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())

			Eventually(func() v1.Time {
				// Retrieve the CR and ensure the last update timestamp is different before comparing it against the slice.
				p, err = fakeRouteClient.K8sV1().AdminPolicyBasedExternalRoutes().Get(context.TODO(), staticMultiIPPolicy.Name, v1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				return p.Status.LastTransitionTime
			}).ShouldNot(Equal(lastUpdate))

			By("Validating the static refernces don't contain the last element")
			Eventually(func() *namespaceInfo {
				return getNamespaceInfo(namespaceTest.Name)
			}, 5).Should(BeComparableTo(expected, cmpOpts...))
		})

		It("validates that changing the BFD setting in a static hop will trigger an update", func() {
			initController([]runtime.Object{namespaceDefault, namespaceTest}, []runtime.Object{staticPolicy})
			Eventually(func() []string { return listRoutePolicyInCache() }, 5).Should(HaveLen(1))
			Eventually(listNamespaceInfo, 5).Should(HaveLen(1))

			Eventually(func() *namespaceInfo {
				return getNamespaceInfo(namespaceTest.Name)
			}, 5).Should(BeComparableTo(
				&namespaceInfo{
					Policies: sets.New(staticPolicy.Name),
					StaticGateways: gatewayInfoList{
						newGatewayInfo(sets.New(staticHopGWIP), false),
					},
					DynamicGateways: map[types.NamespacedName]*gatewayInfo{}},
				cmpOpts...))

			By("set BDF to true in the static hop")
			p, err := fakeRouteClient.K8sV1().AdminPolicyBasedExternalRoutes().Get(context.Background(), staticPolicy.Name, v1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			p.Spec.NextHops.StaticHops[0].BFDEnabled = true
			p.Generation++
			lastUpdate := p.Status.LastTransitionTime
			_, err = fakeRouteClient.K8sV1().AdminPolicyBasedExternalRoutes().Update(context.Background(), p, v1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())
			Eventually(func() v1.Time {
				p, err = fakeRouteClient.K8sV1().AdminPolicyBasedExternalRoutes().Get(context.TODO(), staticPolicy.Name, v1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				return p.Status.LastTransitionTime
			}).Should(Not(Equal(lastUpdate)))

			Eventually(func() *namespaceInfo {
				return getNamespaceInfo(namespaceTest.Name)
			}, 5).Should(BeComparableTo(
				&namespaceInfo{
					Policies: sets.New(staticPolicy.Name),
					StaticGateways: gatewayInfoList{
						newGatewayInfo(sets.New(staticHopGWIP), true),
					},
					DynamicGateways: map[types.NamespacedName]*gatewayInfo{}},
				cmpOpts...))

		})

		It("validates that changing the BFD setting in a dynamic hop will trigger an update", func() {
			initController([]runtime.Object{namespaceDefault, namespaceTest, pod1}, []runtime.Object{dynamicPolicy})
			Eventually(func() []string { return listRoutePolicyInCache() }, 5).Should(HaveLen(1))
			Eventually(listNamespaceInfo, 5).Should(HaveLen(1))
			Eventually(func() *namespaceInfo {
				return getNamespaceInfo(namespaceTest.Name)
			}, 5).Should(BeComparableTo(
				&namespaceInfo{
					Policies:       sets.New(dynamicPolicy.Name),
					StaticGateways: gatewayInfoList{},
					DynamicGateways: map[types.NamespacedName]*gatewayInfo{
						{Namespace: pod1.Namespace, Name: pod1.Name}: newGatewayInfo(sets.New(pod1.Status.PodIPs[0].IP), false),
					}},
				cmpOpts...))
			By("set BDF to true in the dynamic hop")
			p, err := fakeRouteClient.K8sV1().AdminPolicyBasedExternalRoutes().Get(context.Background(), dynamicPolicy.Name, v1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			p.Spec.NextHops.DynamicHops[0].BFDEnabled = true
			p.Generation++
			lastUpdate := p.Status.LastTransitionTime
			_, err = fakeRouteClient.K8sV1().AdminPolicyBasedExternalRoutes().Update(context.Background(), p, v1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())
			Eventually(func() v1.Time {
				p, err = fakeRouteClient.K8sV1().AdminPolicyBasedExternalRoutes().Get(context.TODO(), dynamicPolicy.Name, v1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				return p.Status.LastTransitionTime
			}).Should(Not(Equal(lastUpdate)))
			Eventually(func() *namespaceInfo {
				return getNamespaceInfo(namespaceTest.Name)
			}, 5).Should(BeComparableTo(
				&namespaceInfo{
					Policies:       sets.New(dynamicPolicy.Name),
					StaticGateways: gatewayInfoList{},
					DynamicGateways: map[types.NamespacedName]*gatewayInfo{
						{Namespace: pod1.Namespace, Name: pod1.Name}: newGatewayInfo(sets.New(pod1.Status.PodIPs[0].IP), true),
					}},
				cmpOpts...))

		})
	})
})
