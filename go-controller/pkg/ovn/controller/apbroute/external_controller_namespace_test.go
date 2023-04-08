package apbroute

import (
	"context"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/fake"

	adminpolicybasedrouteapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1"
	adminpolicybasedrouteclient "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1/apis/clientset/versioned/fake"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"k8s.io/apimachinery/pkg/runtime"
)

const (
	annotatedPodIP         = "192.168.2.1"
	dynamicHopHostNetPodIP = "192.168.1.1"
	staticHopGWIP          = "10.10.10.1"
)

func newPolicy(policyName string, fromNSSelector *v1.LabelSelector, staticHopsGWIPs sets.Set[string], dynamicHopsNSSelector *v1.LabelSelector, dynamicHopsPodSelector *v1.LabelSelector, bfdEnabled bool) *adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute {
	p := adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute{
		ObjectMeta: v1.ObjectMeta{Name: policyName},
		Spec: adminpolicybasedrouteapi.AdminPolicyBasedExternalRouteSpec{
			From: adminpolicybasedrouteapi.ExternalNetworkSource{
				NamespaceSelector: *fromNSSelector,
			},
			NextHops: adminpolicybasedrouteapi.ExternalNextHops{},
		},
	}

	if staticHopsGWIPs.Len() > 0 {
		p.Spec.NextHops.StaticHops = []*adminpolicybasedrouteapi.StaticHop{}
		for ip := range staticHopsGWIPs {
			p.Spec.NextHops.StaticHops = append(p.Spec.NextHops.StaticHops, &adminpolicybasedrouteapi.StaticHop{IP: ip, BFDEnabled: bfdEnabled})
		}
	}
	if dynamicHopsNSSelector != nil && dynamicHopsPodSelector != nil {
		p.Spec.NextHops.DynamicHops = []*adminpolicybasedrouteapi.DynamicHop{
			{NamespaceSelector: dynamicHopsNSSelector,
				PodSelector: *dynamicHopsPodSelector,
				BFDEnabled:  bfdEnabled},
		}
	}
	return &p
}

func deletePolicy(policyName string, fakeRouteClient *adminpolicybasedrouteclient.Clientset) {
	err = fakeRouteClient.K8sV1().AdminPolicyBasedExternalRoutes().Delete(context.TODO(), policyName, v1.DeleteOptions{})
	Expect(err).NotTo(HaveOccurred())
}

func deleteNamespace(namespaceName string, fakeClient *fake.Clientset) {

	err = fakeClient.CoreV1().Namespaces().Delete(context.Background(), namespaceName, v1.DeleteOptions{})
	Expect(err).NotTo(HaveOccurred())
}

func updateNamespaceLabel(namespaceName string, labels map[string]string, fakeClient *fake.Clientset) {
	ns, err := fakeClient.CoreV1().Namespaces().Get(context.TODO(), namespaceName, v1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	incrementResourceVersion(ns)
	ns.Labels = labels
	_, err = fakeClient.CoreV1().Namespaces().Update(context.Background(), ns, v1.UpdateOptions{})
	Expect(err).NotTo(HaveOccurred())
}

func getNamespaceInfo(namespaceName string) *namespaceInfo {
	f, found := mgr.getNamespaceInfoFromCache(namespaceName)
	if found {
		cp := &namespaceInfo{}
		deepCopyNamespaceInfo(f, cp)
		mgr.unlockNamespaceInfoCache(namespaceName)
		return cp
	}
	return f
}
func listNamespaceInfo() []string {
	return mgr.namespaceInfoSyncCache.GetKeys()
}

func deepCopyNamespaceInfo(source, destination *namespaceInfo) {
	destination.policies = sets.New(source.policies.UnsortedList()...)
	destination.staticGateways, _ = gatewayInfoList.Insert(source.staticGateways)
	destination.dynamicGateways = make(map[ktypes.NamespacedName]*gatewayInfo)
	for key, value := range source.dynamicGateways {
		destination.dynamicGateways[key] = value
	}
}

var _ = Describe("OVN External Gateway namespace", func() {

	var (
		dynamicPolicy = newPolicy(
			"dynamic",
			&v1.LabelSelector{MatchLabels: map[string]string{"name": "test"}},
			nil,
			&v1.LabelSelector{MatchLabels: map[string]string{"name": "default"}},
			&v1.LabelSelector{MatchLabels: map[string]string{"name": "pod"}},
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

		annotatedPodGW = &corev1.Pod{
			ObjectMeta: v1.ObjectMeta{Name: "annotatedPod", Namespace: "default",
				Labels:      map[string]string{"name": "annotatedPod"},
				Annotations: map[string]string{"k8s.ovn.org/routing-namespaces": "test", "k8s.ovn.org/routing-network": ""},
			},
			Spec:   corev1.PodSpec{HostNetwork: true},
			Status: corev1.PodStatus{PodIPs: []corev1.PodIP{{IP: annotatedPodIP}}, Phase: corev1.PodRunning},
		}

		podGW = &corev1.Pod{
			ObjectMeta: v1.ObjectMeta{Name: "pod", Namespace: "default",
				Labels: map[string]string{"name": "pod"}},
			Spec:   corev1.PodSpec{HostNetwork: true},
			Status: corev1.PodStatus{PodIPs: []corev1.PodIP{{IP: dynamicHopHostNetPodIP}}, Phase: corev1.PodRunning},
		}
		namespaceDefault = &corev1.Namespace{
			ObjectMeta: v1.ObjectMeta{Name: "default",
				Labels: map[string]string{"name": "default"}}}
		namespaceTest = &corev1.Namespace{
			ObjectMeta: v1.ObjectMeta{Name: "test",
				Labels: map[string]string{"name": "test"}},
		}
		namespaceTest2 = &corev1.Namespace{
			ObjectMeta: v1.ObjectMeta{Name: "test2",
				Labels: map[string]string{"name": "test2"}},
		}
	)
	AfterEach(func() {
		nbsbCleanup.Cleanup()
	})

	BeforeEach(func() {
		initialDB = libovsdbtest.TestSetup{
			NBData: []libovsdbtest.TestData{
				&nbdb.LogicalSwitch{
					Name: "node1",
				},
			},
		}
		nbClient, _, nbsbCleanup, err = libovsdbtest.NewNBSBTestHarness(initialDB)
		Expect(err).NotTo(HaveOccurred())
		stopChan = make(chan struct{})

	})

	var _ = Context("When no pod or namespace routing network annotations coexist with the policies", func() {

		var _ = Context("When creating new namespaces", func() {

			It("registers the new namespace with no matching policies", func() {
				initController([]runtime.Object{namespaceTest2}, []runtime.Object{dynamicPolicy})

				Eventually(func() []string { return listRoutePolicyInCache() }, 5).Should(HaveLen(1))
				Eventually(func() adminpolicybasedrouteapi.AdminPolicyBasedExternalRouteSpec {
					p, found := externalController.mgr.routePolicySyncCache.Load(dynamicPolicy.Name)
					if !found {
						return adminpolicybasedrouteapi.AdminPolicyBasedExternalRouteSpec{}
					}
					return p.policy.Spec
				}, 5).Should(Equal(dynamicPolicy.Spec))
				Eventually(func() []string { return listNamespaceInfo() }, 5).Should(HaveLen(0))
			})

			It("registers the new namespace with one matching policy containing one static gateway", func() {
				initController([]runtime.Object{namespaceTest}, []runtime.Object{staticPolicy})

				Eventually(func() []string { return listRoutePolicyInCache() }, 5).Should(HaveLen(1))
				Eventually(func() adminpolicybasedrouteapi.AdminPolicyBasedExternalRouteSpec {
					p, found := externalController.mgr.routePolicySyncCache.Load(staticPolicy.Name)
					if !found {
						return adminpolicybasedrouteapi.AdminPolicyBasedExternalRouteSpec{}
					}
					return p.policy.Spec
				}, 5).Should(Equal(staticPolicy.Spec))
				Eventually(func() []string { return listNamespaceInfo() }, 5).Should(HaveLen(1))
				Eventually(func() *namespaceInfo { return getNamespaceInfo(namespaceTest.Name) }, 5).Should(
					BeEquivalentTo(
						&namespaceInfo{
							policies:        sets.New(staticPolicy.Name),
							staticGateways:  gatewayInfoList{{gws: sets.New(staticHopGWIP)}},
							dynamicGateways: make(map[ktypes.NamespacedName]*gatewayInfo)}))
			})
			It("registers a new namespace with one policy that includes a dynamic GW", func() {
				initController([]runtime.Object{namespaceTest, namespaceDefault, podGW}, []runtime.Object{dynamicPolicy})

				By("validating that the namespace cache contains the test namespace and that it reflect the applicable policy")
				Eventually(func() []string { return listNamespaceInfo() }, 5).Should(HaveLen(1))
				Eventually(func() *namespaceInfo { return getNamespaceInfo(namespaceTest.Name) }, 5).Should(
					BeEquivalentTo(
						&namespaceInfo{
							policies:        sets.New(dynamicPolicy.Name),
							staticGateways:  gatewayInfoList{},
							dynamicGateways: map[ktypes.NamespacedName]*gatewayInfo{{Namespace: podGW.Namespace, Name: podGW.Name}: {gws: sets.New(dynamicHopHostNetPodIP)}}}))
			})

			It("registers a new namespace with one policy with dynamic GWs and the IP of an annotated pod", func() {

				initController([]runtime.Object{namespaceTest, namespaceDefault, podGW, annotatedPodGW}, []runtime.Object{dynamicPolicy})

				By("validating that the namespace cache contains the test namespace and that it reflect the applicable policy")
				Eventually(func() []string { return listNamespaceInfo() }, 5).Should(HaveLen(1))
				Eventually(func() *namespaceInfo { return getNamespaceInfo(namespaceTest.Name) }, 5).Should(
					BeEquivalentTo(
						&namespaceInfo{
							policies:        sets.New(dynamicPolicy.Name),
							staticGateways:  gatewayInfoList{},
							dynamicGateways: map[ktypes.NamespacedName]*gatewayInfo{{Namespace: podGW.Namespace, Name: podGW.Name}: {gws: sets.New(dynamicHopHostNetPodIP)}}}))
			})

			It("registers a new namespace with one policy and validates that the deleted field is set to false", func() {
				initController([]runtime.Object{namespaceTest, namespaceDefault, podGW, annotatedPodGW}, []runtime.Object{dynamicPolicy})

				deleteNamespace(namespaceTest.Name, fakeClient)
				By("validating that the namespace cache no longer contains the test namespace")
				Eventually(func() []string { return listNamespaceInfo() }, 5).Should(HaveLen(0))

				_, err = fakeClient.CoreV1().Namespaces().Create(context.TODO(), namespaceTest, v1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())
				By("validating that the namespace cache is contained in the namespace info cache and it reflects the correct policy")

				Eventually(func() []string { return listNamespaceInfo() }, 5).Should(HaveLen(1))
				Eventually(func() *namespaceInfo { return getNamespaceInfo(namespaceTest.Name) }, 5).Should(
					BeEquivalentTo(
						&namespaceInfo{
							policies:        sets.New(dynamicPolicy.Name),
							staticGateways:  gatewayInfoList{},
							dynamicGateways: map[ktypes.NamespacedName]*gatewayInfo{{Namespace: podGW.Namespace, Name: podGW.Name}: {gws: sets.New(dynamicHopHostNetPodIP)}}}))
			})
		})
	})

	var _ = Context("When deleting a namespace", func() {

		It("validates that the namespace cache is empty and marked as deleted when the namespace was a recipient for policies", func() {
			initController([]runtime.Object{namespaceTest}, []runtime.Object{staticPolicy})

			Expect(externalController.mgr.namespaceInfoSyncCache.GetKeys()).To(HaveLen(0))
			Eventually(func() []string { return listNamespaceInfo() }, 5).Should(HaveLen(1))
			Eventually(func() *namespaceInfo { return getNamespaceInfo(namespaceTest.Name) }, 5).Should(
				Equal(
					&namespaceInfo{
						policies:        sets.New(staticPolicy.Name),
						staticGateways:  gatewayInfoList{{gws: sets.New(staticHopGWIP)}},
						dynamicGateways: make(map[ktypes.NamespacedName]*gatewayInfo)}))

			deleteNamespace(namespaceTest.Name, fakeClient)
			Eventually(func() []string { return listNamespaceInfo() }, 5).Should(HaveLen(0))

		})
		It("validates that the namespace cache is empty when the namespace that is recipient for any policy is deleted", func() {
			initController([]runtime.Object{namespaceDefault}, []runtime.Object{staticPolicy})

			Eventually(func() []string { return listNamespaceInfo() }, 5).Should(HaveLen(0))
			deleteNamespace(namespaceDefault.Name, fakeClient)
			Eventually(func() []string { return listNamespaceInfo() }, 5).Should(HaveLen(0))
		})

	})

	var _ = Context("When updating an existing namespace", func() {

		var (
			dynamicPolicyTest2 = newPolicy(
				"dynamicPolicyTest2",
				&v1.LabelSelector{MatchLabels: map[string]string{"key": "test"}},
				nil,
				&v1.LabelSelector{MatchLabels: map[string]string{"name": "default"}},
				&v1.LabelSelector{MatchLabels: map[string]string{"name": "pod"}},
				false,
			)
		)
		It("validates that a namespace is targeted by an existing policy after its labels are updated to match the policy's label selector", func() {
			initController([]runtime.Object{namespaceDefault, namespaceTest2}, []runtime.Object{staticPolicy})

			Eventually(func() []string { return listNamespaceInfo() }, 15).Should(HaveLen(0))
			updateNamespaceLabel(namespaceTest2.Name, staticPolicy.Spec.From.NamespaceSelector.MatchLabels, fakeClient)
			Eventually(func() []string { return listNamespaceInfo() }, 15).Should(HaveLen(1))
			Eventually(func() *namespaceInfo { return getNamespaceInfo(namespaceTest2.Name) }, 15).Should(
				Equal(
					&namespaceInfo{
						policies:        sets.New(staticPolicy.Name),
						staticGateways:  gatewayInfoList{{gws: sets.New(staticHopGWIP)}},
						dynamicGateways: make(map[ktypes.NamespacedName]*gatewayInfo)}))
		})
		It("validates that a namespace is no longer targeted by an existing policy when its labels are updated so that they don't match the policy's label selector", func() {
			initController([]runtime.Object{namespaceDefault, namespaceTest}, []runtime.Object{staticPolicy})
			Eventually(func() []string { return listNamespaceInfo() }, 5).Should(HaveLen(1))
			Eventually(func() *namespaceInfo { return getNamespaceInfo(namespaceTest.Name) }, 5).Should(
				Equal(
					&namespaceInfo{
						policies:        sets.New(staticPolicy.Name),
						staticGateways:  gatewayInfoList{{gws: sets.New(staticHopGWIP)}},
						dynamicGateways: make(map[ktypes.NamespacedName]*gatewayInfo)}))
			updateNamespaceLabel(namespaceTest.Name, dynamicPolicyTest2.Spec.From.NamespaceSelector.MatchLabels, fakeClient)
			Eventually(func() []string { return listNamespaceInfo() }, 5).Should(HaveLen(0))
			Eventually(func() *namespaceInfo { return getNamespaceInfo(namespaceTest.Name) }, 5).Should(BeNil())
		})

		It("validates that a namespace changes its policies when its labels are changed to match a different policy, resulting in the later on being the only policy applied to the namespace", func() {
			initController([]runtime.Object{namespaceDefault, namespaceTest, podGW}, []runtime.Object{staticPolicy, dynamicPolicyTest2})
			Eventually(func() []string { return listNamespaceInfo() }, 5).Should(HaveLen(1))
			Eventually(func() *namespaceInfo { return getNamespaceInfo(namespaceTest.Name) }, 5).Should(
				Equal(
					&namespaceInfo{
						policies:        sets.New(staticPolicy.Name),
						staticGateways:  gatewayInfoList{{gws: sets.New(staticHopGWIP)}},
						dynamicGateways: make(map[ktypes.NamespacedName]*gatewayInfo)}))
			updateNamespaceLabel(namespaceTest.Name, dynamicPolicyTest2.Spec.From.NamespaceSelector.MatchLabels, fakeClient)
			Eventually(func() []string { return listNamespaceInfo() }, 5).Should(HaveLen(1))
			Eventually(func() *namespaceInfo { return getNamespaceInfo(namespaceTest.Name) }, 5).Should(
				BeEquivalentTo(
					&namespaceInfo{
						policies:        sets.New(dynamicPolicyTest2.Name),
						staticGateways:  gatewayInfoList{},
						dynamicGateways: map[ktypes.NamespacedName]*gatewayInfo{{Namespace: podGW.Namespace, Name: podGW.Name}: {gws: sets.New(dynamicHopHostNetPodIP)}}}))

		})

		It("validates that a namespace is now targeted by a second policy once its labels are updated to match the first and second policy", func() {
			initController([]runtime.Object{namespaceDefault, namespaceTest, podGW}, []runtime.Object{staticPolicy, dynamicPolicyTest2})
			Eventually(func() []string { return listNamespaceInfo() }, 5).Should(HaveLen(1))
			Eventually(func() *namespaceInfo { return getNamespaceInfo(namespaceTest.Name) }, 5).Should(
				Equal(
					&namespaceInfo{
						policies:        sets.New(staticPolicy.Name),
						staticGateways:  gatewayInfoList{{gws: sets.New(staticHopGWIP)}},
						dynamicGateways: make(map[ktypes.NamespacedName]*gatewayInfo)}))
			aggregatedLabels := map[string]string{"name": "test", "key": "test"}
			updateNamespaceLabel(namespaceTest.Name, aggregatedLabels, fakeClient)
			Eventually(func() []string { return listNamespaceInfo() }, 5).Should(HaveLen(1))
			Eventually(func() *namespaceInfo { return getNamespaceInfo(namespaceTest.Name) }, 5).Should(
				BeEquivalentTo(
					&namespaceInfo{
						policies:        sets.New(staticPolicy.Name, dynamicPolicyTest2.Name),
						staticGateways:  gatewayInfoList{{gws: sets.New(staticHopGWIP)}},
						dynamicGateways: map[ktypes.NamespacedName]*gatewayInfo{{Namespace: podGW.Namespace, Name: podGW.Name}: {gws: sets.New(dynamicHopHostNetPodIP)}}}))
		})
	})

})
