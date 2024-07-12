package apbroute

import (
	"context"
	"fmt"
	"strings"
	"sync"

	nettypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	adminpolicybasedrouteapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1"
	adminpolicybasedrouteclient "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1/apis/clientset/versioned/fake"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	libovsdbutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/klog/v2"
)

func newPod(podName, namespace, podIP string, labels map[string]string) *corev1.Pod {
	return newPodWithPhaseAndIP(podName, namespace, corev1.PodRunning, podIP, labels)
}

func newPodWithPhaseAndIP(podName, namespace string, phase corev1.PodPhase, podIP string, labels map[string]string) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: v1.ObjectMeta{Name: podName, Namespace: namespace,
			Labels: labels},
		Spec:   corev1.PodSpec{NodeName: "node"},
		Status: corev1.PodStatus{Phase: phase},
	}
	if len(podIP) > 0 {
		p.Annotations = map[string]string{nettypes.NetworkStatusAnnot: fmt.Sprintf(network_status, podIP)}
		p.Status.PodIPs = []corev1.PodIP{{IP: podIP}}
	}
	return p
}

func listRoutePolicyInCache() []string {
	return externalController.mgr.routePolicySyncCache.GetKeys()
}

func refObjectsLen() int {
	externalController.mgr.policyReferencedObjectsLock.Lock()
	defer externalController.mgr.policyReferencedObjectsLock.Unlock()
	return len(externalController.mgr.policyReferencedObjects)
}

var (
	externalController *ExternalGatewayMasterController
	iFactory           *factory.WatchFactory
	stopChan           chan (struct{})
	wg                 *sync.WaitGroup
	initialDB          libovsdbtest.TestSetup
	nbClient           libovsdbclient.Client
	nbsbCleanup        *libovsdbtest.Context
	fakeRouteClient    *adminpolicybasedrouteclient.Clientset
	fakeClient         *fake.Clientset
	mgr                *externalPolicyManager
	err                error

	node = &corev1.Node{
		ObjectMeta: v1.ObjectMeta{
			Name: "node",
			Annotations: map[string]string{
				"k8s.ovn.org/l3-gateway-config": `{"default":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"169.254.33.2/24", "next-hop":"169.254.33.1"}}`,
				"k8s.ovn.org/node-chassis-id":   "79fdcfc4-6fe6-4cd3-8242-c0f85a4668ec",
			},
		}}
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
	fakeClient = fake.NewSimpleClientset(append(k8sObjects, node)...)
	fakeRouteClient = adminpolicybasedrouteclient.NewSimpleClientset(routePolicyObjects...)
	iFactory, err = factory.NewMasterWatchFactory(&util.OVNMasterClientset{
		KubeClient:             fakeClient,
		AdminPolicyRouteClient: fakeRouteClient,
	})
	Expect(err).NotTo(HaveOccurred())
	// Try to get the NBZone.  If there is an error, create NB_Global record.
	// Otherwise NewController() will return error since it
	// calls libovsdbutil.GetNBZone().
	_, err = libovsdbutil.GetNBZone(nbClient)
	if err != nil {
		nbZoneFailed = true
		err = createTestNBGlobal(nbClient, "global")
		Expect(err).NotTo(HaveOccurred())
	}
	// this package tests apbRoute controller separately from the legacy functionality, therefore
	// it is not necessary to pass DefaultNetworkController name
	controllerName := "test-controller"
	externalController, err = NewExternalMasterController(
		fakeRouteClient,
		stopChan,
		iFactory.PodCoreInformer(),
		iFactory.NamespaceInformer(),
		iFactory.APBRouteInformer(),
		iFactory.NodeCoreInformer().Lister(),
		nbClient,
		addressset.NewFakeAddressSetFactory(controllerName),
		controllerName,
		"single-zone")
	Expect(err).NotTo(HaveOccurred())

	if nbZoneFailed {
		// Delete the NBGlobal row as this function created it.  Otherwise, many tests would fail while
		// checking the expectedData in the NBDB.
		err = deleteTestNBGlobal(nbClient, "global")
		Expect(err).NotTo(HaveOccurred())
	}

	err = iFactory.Start()
	Expect(err).NotTo(HaveOccurred())

	mgr = externalController.mgr
	wg = &sync.WaitGroup{}
	err = externalController.Run(wg, 5)
	Expect(err).NotTo(HaveOccurred())
}

func shutdownController() {
	if iFactory != nil {
		iFactory.Shutdown()
	}
	if stopChan != nil {
		close(stopChan)
	}
	if wg != nil {
		wg.Wait()
	}
}

func eventuallyExpectNumberOfPolicies(n int) {
	Eventually(listRoutePolicyInCache).Should(HaveLen(n))
	Eventually(refObjectsLen).Should(Equal(n))
}

func eventuallyExpectConfig(policyName string, expectedPolicy *routePolicyState, expectedRefs *policyReferencedObjects) {
	Eventually(func() bool {
		result := false
		_ = externalController.mgr.routePolicySyncCache.DoWithLock(policyName, func(policyName string) error {
			p, found := externalController.mgr.routePolicySyncCache.Load(policyName)
			if !found {
				return nil
			}
			result = p.Equal(expectedPolicy)
			return nil
		})
		return result

	}, 5).Should(BeTrue(), fmt.Sprintf("expected policy %s to match '%s'", policyName, expectedPolicy.String()))
	Eventually(func() *policyReferencedObjects { return listRefObjects(policyName) }, 5).
		Should(Equal(expectedRefs))
}

var _ = Describe("OVN External Gateway policy", func() {

	var (
		gatewayNamespaceName  = "gateway"
		gatewayNamespaceMatch = map[string]string{"name": gatewayNamespaceName}

		targetNamespaceName          = "target1"
		targetNamespaceName2         = "target2"
		targetNamespaceLabel         = "target"
		targetNamespace1Match        = map[string]string{"name": targetNamespaceName}
		targetNamespace2Match        = map[string]string{"name": targetNamespaceName2}
		targetMultipleNamespaceMatch = map[string]string{"match": targetNamespaceLabel}

		namespaceGW = &corev1.Namespace{
			ObjectMeta: v1.ObjectMeta{Name: gatewayNamespaceName,
				Labels: gatewayNamespaceMatch}}
		namespaceTarget = &corev1.Namespace{
			ObjectMeta: v1.ObjectMeta{Name: targetNamespaceName,
				Labels: map[string]string{"name": targetNamespaceName, "match": targetNamespaceLabel}},
		}
		targetPod1 = newPod("pod_target1", namespaceTarget.Name, "192.169.10.1",
			map[string]string{"key": "pod", "name": "pod_target1"})

		namespaceTarget2 = &corev1.Namespace{
			ObjectMeta: v1.ObjectMeta{Name: targetNamespaceName2,
				Labels: map[string]string{"name": targetNamespaceName2, "match": targetNamespaceLabel}},
		}
		targetPod2 = newPod("pod_target2", namespaceTarget2.Name, "192.169.10.2",
			map[string]string{"key": "pod", "name": "pod_target2"})

		dynamicPolicy = newPolicy(
			"dynamic",
			&v1.LabelSelector{MatchLabels: targetNamespace2Match},
			nil,
			&v1.LabelSelector{MatchLabels: gatewayNamespaceMatch},
			&v1.LabelSelector{MatchLabels: map[string]string{"key": "pod"}},
			false,
		)

		staticPolicy = newPolicy(
			"static",
			&v1.LabelSelector{MatchLabels: targetNamespace1Match},
			sets.New(staticHopGWIP),
			nil,
			nil,
			false,
		)

		pod1 = newPod("pod_1", namespaceGW.Name, "192.168.10.1", map[string]string{"key": "pod", "name": "pod1", "duplicated": "true"})
		pod2 = newPod("pod_2", namespaceGW.Name, "192.168.20.1", map[string]string{"key": "pod", "name": "pod2"})
		pod3 = newPod("pod_3", namespaceGW.Name, "192.168.30.1", map[string]string{"key": "pod", "name": "pod3"})
		pod4 = newPod("pod_4", namespaceGW.Name, "192.168.40.1", map[string]string{"key": "pod", "name": "pod4"})
		pod5 = newPod("pod_5", namespaceGW.Name, "192.168.50.1", map[string]string{"key": "pod", "name": "pod5"})
		pod6 = newPod("pod_6", namespaceGW.Name, "192.168.60.1", map[string]string{"key": "pod", "name": "pod6"})

		namespaceTargetWithPod, namespaceTarget2WithPod, namespaceTarget2WithoutPod, namespaceGWWithPod *namespaceWithPods
	)
	AfterEach(func() {
		shutdownController()
		nbsbCleanup.Cleanup()
	})

	BeforeEach(func() {
		// Restore global default values before each testcase
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
		namespaceTarget2WithPod = newNamespaceWithPods(namespaceTarget2.Name, targetPod2)
		namespaceTarget2WithoutPod = newNamespaceWithPods(namespaceTarget2.Name)
		namespaceGWWithPod = newNamespaceWithPods(namespaceGW.Name, pod1)
	})

	var _ = Context("When adding new policies", func() {

		var (
			namespaceTarget3 = &corev1.Namespace{
				ObjectMeta: v1.ObjectMeta{Name: "target3",
					Labels: map[string]string{"name": "target3", "match": targetNamespaceLabel}},
			}
			multipleMatchPolicy = newPolicy(
				"multiple",
				&v1.LabelSelector{MatchLabels: targetMultipleNamespaceMatch},
				sets.New(staticHopGWIP),
				&v1.LabelSelector{MatchLabels: gatewayNamespaceMatch},
				&v1.LabelSelector{MatchLabels: map[string]string{"key": "pod"}},
				false,
			)
		)
		It("registers the new policy with multiple namespace matching", func() {

			initController([]runtime.Object{namespaceGW, namespaceTarget, namespaceTarget2, namespaceTarget3, targetPod1, pod1},
				[]runtime.Object{multipleMatchPolicy})
			policyName := multipleMatchPolicy.Name

			expectedPolicy, expectedRefs := expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTargetWithPod, namespaceTarget2WithoutPod, newNamespaceWithPods(namespaceTarget3.Name)},
				[]string{staticHopGWIP},
				[]*namespaceWithPods{namespaceGWWithPod}, false)

			eventuallyExpectNumberOfPolicies(1)
			eventuallyExpectConfig(policyName, expectedPolicy, expectedRefs)
		})

		It("registers a new policy with no namespace match", func() {
			initController([]runtime.Object{namespaceTarget, namespaceGW, pod1}, []runtime.Object{dynamicPolicy})
			policyName := dynamicPolicy.Name
			expectedPolicy, expectedRefs := expectedPolicyStateAndRefs(
				nil,
				nil,
				[]*namespaceWithPods{namespaceGWWithPod}, false)

			eventuallyExpectNumberOfPolicies(1)
			eventuallyExpectConfig(policyName, expectedPolicy, expectedRefs)
		})

		It("registers a new policy with multiple dynamic and static GWs and bfd enabled on all gateways", func() {

			staticMultiIPPolicy := newPolicy("multiIPPolicy",
				&v1.LabelSelector{MatchLabels: targetNamespace1Match},
				sets.New("10.10.10.1", "10.10.10.2", "10.10.10.3", "10.10.10.3", "10.10.10.4"),
				&v1.LabelSelector{MatchLabels: gatewayNamespaceMatch},
				&v1.LabelSelector{MatchLabels: map[string]string{"key": "pod"}},
				true,
			)
			namespaceGWWithPods := newNamespaceWithPods(namespaceGW.Name, pod1, pod2, pod3, pod4, pod5, pod6)
			initController([]runtime.Object{namespaceGW, namespaceTarget, targetPod1, pod1, pod2, pod3, pod4, pod5, pod6},
				[]runtime.Object{staticMultiIPPolicy})
			policyName := staticMultiIPPolicy.Name

			expectedPolicy, expectedRefs := expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTargetWithPod},
				[]string{"10.10.10.1", "10.10.10.2", "10.10.10.3", "10.10.10.4"},
				[]*namespaceWithPods{namespaceGWWithPods}, true)

			eventuallyExpectNumberOfPolicies(1)
			eventuallyExpectConfig(policyName, expectedPolicy, expectedRefs)
		})

		It("registers a second policy with no overlaping IPs", func() {
			initController([]runtime.Object{namespaceGW, namespaceTarget, targetPod1, namespaceTarget2, targetPod2, pod1},
				[]runtime.Object{staticPolicy, dynamicPolicy})
			policyName1 := staticPolicy.Name
			policyName2 := dynamicPolicy.Name

			expectedPolicy1, expectedRefs1 := expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTargetWithPod},
				[]string{staticHopGWIP},
				nil, false)

			expectedPolicy2, expectedRefs2 := expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTarget2WithPod},
				nil,
				[]*namespaceWithPods{namespaceGWWithPod}, false)

			eventuallyExpectNumberOfPolicies(2)
			eventuallyExpectConfig(policyName1, expectedPolicy1, expectedRefs1)
			eventuallyExpectConfig(policyName2, expectedPolicy2, expectedRefs2)

		})

		It("registers policies with overlaping IPs for static and dynamic hops", func() {
			duplicatedStatic := newPolicy(
				"duplicatedstatic",
				&v1.LabelSelector{MatchLabels: targetNamespace1Match},
				sets.New(staticHopGWIP),
				nil,
				nil,
				false,
			)
			duplicatedStatic.Spec.NextHops.StaticHops = append(duplicatedStatic.Spec.NextHops.StaticHops,
				&adminpolicybasedrouteapi.StaticHop{IP: staticHopGWIP, BFDEnabled: false})

			duplicatedDynamic := newPolicy(
				"duplicatedDynamic",
				&v1.LabelSelector{MatchLabels: targetNamespace2Match},
				nil,
				&v1.LabelSelector{MatchLabels: gatewayNamespaceMatch},
				&v1.LabelSelector{MatchLabels: map[string]string{"key": "pod"}},
				false,
			)
			duplicatedDynamic.Spec.NextHops.DynamicHops = append(duplicatedDynamic.Spec.NextHops.DynamicHops,
				&adminpolicybasedrouteapi.DynamicHop{
					NamespaceSelector: v1.LabelSelector{MatchLabels: gatewayNamespaceMatch},
					PodSelector:       v1.LabelSelector{MatchLabels: map[string]string{"key": "duplicated"}},
					BFDEnabled:        false,
				})

			initController([]runtime.Object{namespaceGW, namespaceTarget, targetPod1, namespaceTarget2, targetPod2, pod1},
				[]runtime.Object{duplicatedStatic, duplicatedDynamic})

			policyName1 := duplicatedStatic.Name
			policyName2 := duplicatedDynamic.Name

			expectedPolicy1, expectedRefs1 := expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTargetWithPod},
				[]string{staticHopGWIP},
				nil, false)

			expectedPolicy2, expectedRefs2 := expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTarget2WithPod},
				nil,
				[]*namespaceWithPods{namespaceGWWithPod}, false)

			eventuallyExpectNumberOfPolicies(2)
			eventuallyExpectConfig(policyName1, expectedPolicy1, expectedRefs1)
			eventuallyExpectConfig(policyName2, expectedPolicy2, expectedRefs2)
		})

		It("validates that only 1 policy for the same target namespace is handled", func() {
			initController([]runtime.Object{namespaceGW, namespaceTarget, targetPod1, pod1},
				[]runtime.Object{staticPolicy})

			policyName1 := staticPolicy.Name
			expectedPolicy1, expectedRefs1 := expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTargetWithPod},
				[]string{staticHopGWIP},
				nil, false)

			eventuallyExpectNumberOfPolicies(1)
			eventuallyExpectConfig(policyName1, expectedPolicy1, expectedRefs1)

			// staticPolicy2 selects the same namespace
			staticPolicy2 := newPolicy(
				"static2",
				&v1.LabelSelector{MatchLabels: targetNamespace1Match},
				sets.New(staticHopGWIP),
				nil,
				nil,
				false,
			)

			_, err = fakeRouteClient.K8sV1().AdminPolicyBasedExternalRoutes().Create(context.TODO(), staticPolicy2, v1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			eventuallyCheckAPBRouteStatus(staticPolicy2.Name, true)

			eventuallyExpectNumberOfPolicies(1)
			eventuallyExpectConfig(policyName1, expectedPolicy1, expectedRefs1)
		})
	})

	var _ = Context("when deleting a policy", func() {

		It("validates that the IPs of the policy are no longer reflected on the targeted namespaces when "+
			"the policy is deleted and no other policy overlaps", func() {
			initController([]runtime.Object{namespaceGW, namespaceTarget, targetPod1, namespaceTarget2, targetPod2, pod1},
				[]runtime.Object{staticPolicy, dynamicPolicy})

			policyName1 := staticPolicy.Name
			policyName2 := dynamicPolicy.Name

			expectedPolicy1, expectedRefs1 := expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTargetWithPod},
				[]string{staticHopGWIP},
				nil, false)

			expectedPolicy2, expectedRefs2 := expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTarget2WithPod},
				nil,
				[]*namespaceWithPods{namespaceGWWithPod}, false)

			eventuallyExpectNumberOfPolicies(2)
			eventuallyExpectConfig(policyName1, expectedPolicy1, expectedRefs1)
			eventuallyExpectConfig(policyName2, expectedPolicy2, expectedRefs2)

			// delete policy
			deletePolicy(staticPolicy.Name, fakeRouteClient)
			Eventually(func() []string { return listRoutePolicyInCache() }, 5, 1).Should(HaveLen(1))

			eventuallyExpectNumberOfPolicies(1)
			eventuallyExpectConfig(policyName2, expectedPolicy2, expectedRefs2)
		})

		It("validates that policy for the same target namespace is handled after the first one is deleted", func() {
			initController([]runtime.Object{namespaceGW, namespaceTarget, targetPod1, pod1},
				[]runtime.Object{staticPolicy})

			policyName1 := staticPolicy.Name
			expectedPolicy1, expectedRefs1 := expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTargetWithPod},
				[]string{staticHopGWIP},
				nil, false)

			eventuallyExpectNumberOfPolicies(1)
			eventuallyExpectConfig(policyName1, expectedPolicy1, expectedRefs1)

			// staticPolicy2 selects the same namespace
			staticPolicy2 := newPolicy(
				"static2",
				&v1.LabelSelector{MatchLabels: targetNamespace1Match},
				sets.New(staticHopGWIP),
				nil,
				nil,
				false,
			)
			policyName2 := staticPolicy2.Name

			_, err = fakeRouteClient.K8sV1().AdminPolicyBasedExternalRoutes().Create(context.TODO(), staticPolicy2, v1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			eventuallyCheckAPBRouteStatus(staticPolicy2.Name, true)
			eventuallyExpectNumberOfPolicies(1)
			eventuallyExpectConfig(policyName1, expectedPolicy1, expectedRefs1)

			deletePolicy(staticPolicy.Name, fakeRouteClient)
			eventuallyCheckAPBRouteStatus(staticPolicy2.Name, false)

			expectedPolicy2, expectedRefs2 := expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTargetWithPod},
				[]string{staticHopGWIP},
				nil, false)

			eventuallyExpectNumberOfPolicies(1)
			eventuallyExpectConfig(policyName2, expectedPolicy2, expectedRefs2)
		})
	})

	var _ = Context("when updating a policy", func() {

		It("validates that changing the from selector will retarget the new namespaces", func() {
			initController([]runtime.Object{namespaceGW, namespaceTarget, namespaceTarget2, targetPod1, targetPod2, pod1},
				[]runtime.Object{dynamicPolicy})

			policyName := dynamicPolicy.Name
			expectedPolicy, expectedRefs := expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTarget2WithPod},
				nil,
				[]*namespaceWithPods{namespaceGWWithPod}, false)

			eventuallyExpectNumberOfPolicies(1)
			eventuallyExpectConfig(policyName, expectedPolicy, expectedRefs)

			p, err := fakeRouteClient.K8sV1().AdminPolicyBasedExternalRoutes().Get(context.TODO(), dynamicPolicy.Name, v1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			p.Spec.From.NamespaceSelector = v1.LabelSelector{MatchLabels: targetMultipleNamespaceMatch}
			p.Generation++
			By("updating policy selector")
			_, err = fakeRouteClient.K8sV1().AdminPolicyBasedExternalRoutes().Update(context.Background(), p, v1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())

			expectedPolicy, expectedRefs = expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTargetWithPod, namespaceTarget2WithPod},
				nil,
				[]*namespaceWithPods{namespaceGWWithPod}, false)

			eventuallyExpectNumberOfPolicies(1)
			eventuallyExpectConfig(policyName, expectedPolicy, expectedRefs)
		})
		It("validates that changing a static hop from an existing policy will be applied to the target namespaces", func() {
			newStaticIP := "10.30.20.1"
			staticPolicy := newPolicy(
				"static",
				&v1.LabelSelector{MatchLabels: targetNamespace1Match},
				sets.New(staticHopGWIP, newStaticIP),
				nil,
				nil,
				false,
			)
			initController([]runtime.Object{namespaceGW, namespaceTarget, targetPod1}, []runtime.Object{staticPolicy})

			policyName := staticPolicy.Name
			expectedPolicy, expectedRefs := expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTargetWithPod},
				[]string{staticHopGWIP, newStaticIP},
				nil, false)

			eventuallyExpectNumberOfPolicies(1)
			eventuallyExpectConfig(policyName, expectedPolicy, expectedRefs)

			p, err := fakeRouteClient.K8sV1().AdminPolicyBasedExternalRoutes().Get(context.TODO(), staticPolicy.Name, v1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			p.Spec.NextHops.StaticHops = []*adminpolicybasedrouteapi.StaticHop{
				{IP: newStaticIP},
			}
			p.Generation++
			_, err = fakeRouteClient.K8sV1().AdminPolicyBasedExternalRoutes().Update(context.Background(), p, v1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())

			expectedPolicy, expectedRefs = expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTargetWithPod},
				[]string{newStaticIP},
				nil, false)

			eventuallyExpectNumberOfPolicies(1)
			eventuallyExpectConfig(policyName, expectedPolicy, expectedRefs)
		})
		It("validates that changes to a dynamic hop from an existing policy will be applied to the target namespaces", func() {
			singlePodDynamicPolicy := newPolicy(
				"singlePod",
				&v1.LabelSelector{MatchLabels: targetNamespace1Match},
				nil,
				&v1.LabelSelector{MatchLabels: gatewayNamespaceMatch},
				&v1.LabelSelector{MatchLabels: map[string]string{"name": "pod1"}},
				false,
			)
			initController([]runtime.Object{namespaceGW, namespaceTarget, targetPod1, pod1, pod2}, []runtime.Object{singlePodDynamicPolicy})

			policyName := singlePodDynamicPolicy.Name
			expectedPolicy, expectedRefs := expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTargetWithPod},
				nil,
				[]*namespaceWithPods{namespaceGWWithPod}, false)

			eventuallyExpectNumberOfPolicies(1)
			eventuallyExpectConfig(policyName, expectedPolicy, expectedRefs)

			p, err := fakeRouteClient.K8sV1().AdminPolicyBasedExternalRoutes().Get(context.TODO(), singlePodDynamicPolicy.Name, v1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			p.Spec.NextHops.DynamicHops[0].PodSelector = v1.LabelSelector{MatchLabels: map[string]string{"name": "pod2"}}
			p.Generation++
			_, err = fakeRouteClient.K8sV1().AdminPolicyBasedExternalRoutes().Update(context.Background(), p, v1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())

			expectedPolicy, expectedRefs = expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTargetWithPod},
				nil,
				[]*namespaceWithPods{newNamespaceWithPods(namespaceGW.Name, pod2)}, false)

			eventuallyExpectNumberOfPolicies(1)
			eventuallyExpectConfig(policyName, expectedPolicy, expectedRefs)
		})
		It("validates that removing one of the static hop IPs will be reflected in the route policy", func() {

			staticMultiIPPolicy := newPolicy("multiIPPolicy",
				&v1.LabelSelector{MatchLabels: targetNamespace1Match},
				sets.New("10.10.10.1", "10.10.10.2", "10.10.10.3", "10.10.10.4"),
				nil, nil,
				true,
			)
			initController([]runtime.Object{namespaceGW, namespaceTarget, targetPod1}, []runtime.Object{staticMultiIPPolicy})

			policyName := staticMultiIPPolicy.Name
			expectedPolicy, expectedRefs := expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTargetWithPod},
				[]string{"10.10.10.1", "10.10.10.2", "10.10.10.3", "10.10.10.4"},
				nil, true)

			eventuallyExpectNumberOfPolicies(1)
			eventuallyExpectConfig(policyName, expectedPolicy, expectedRefs)

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

			expectedPolicy, expectedRefs = expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTargetWithPod},
				[]string{"10.10.10.1", "10.10.10.2", "10.10.10.3"},
				nil, true)

			eventuallyExpectNumberOfPolicies(1)
			eventuallyExpectConfig(policyName, expectedPolicy, expectedRefs)
		})
		It("validates that removing a duplicated static hop IP from an overlapping policy static hop will keep the static IP in the route policy", func() {

			staticMultiIPPolicy := newPolicy("multiIPPolicy",
				&v1.LabelSelector{MatchLabels: targetNamespace2Match},
				sets.New(staticHopGWIP, "20.10.10.1", "20.10.10.2", "20.10.10.3", "20.10.10.4"),
				nil, nil,
				false,
			)
			staticMultiIPPolicy.Spec.NextHops.StaticHops = append(staticMultiIPPolicy.Spec.NextHops.StaticHops,
				&adminpolicybasedrouteapi.StaticHop{IP: "20.10.10.1", BFDEnabled: false})

			initController([]runtime.Object{namespaceGW, namespaceTarget, namespaceTarget2, targetPod1, targetPod2},
				[]runtime.Object{staticMultiIPPolicy, staticPolicy})

			expectedPolicy1, expectedRefs1 := expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTarget2WithPod},
				[]string{staticHopGWIP, "20.10.10.1", "20.10.10.2", "20.10.10.3", "20.10.10.4"},
				nil, false)

			expectedPolicy2, expectedRefs2 := expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTargetWithPod},
				[]string{staticHopGWIP},
				nil, false)

			eventuallyExpectNumberOfPolicies(2)
			eventuallyExpectConfig(staticMultiIPPolicy.Name, expectedPolicy1, expectedRefs1)
			eventuallyExpectConfig(staticPolicy.Name, expectedPolicy2, expectedRefs2)

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
			_, err = fakeRouteClient.K8sV1().AdminPolicyBasedExternalRoutes().Update(context.Background(), p, v1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())

			By("Validating the static references don't contain the last element")
			expectedPolicy1, expectedRefs1 = expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTarget2WithPod},
				[]string{"20.10.10.1", "20.10.10.2", "20.10.10.3", "20.10.10.4"},
				nil, false)

			eventuallyExpectNumberOfPolicies(2)
			eventuallyExpectConfig(staticMultiIPPolicy.Name, expectedPolicy1, expectedRefs1)
			eventuallyExpectConfig(staticPolicy.Name, expectedPolicy2, expectedRefs2)
		})

		It("validates that changing the BFD setting in a static hop will trigger an update", func() {
			initController([]runtime.Object{namespaceGW, namespaceTarget, targetPod1}, []runtime.Object{staticPolicy})
			policyName := staticPolicy.Name

			expectedPolicy, expectedRefs := expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTargetWithPod},
				[]string{staticHopGWIP},
				nil, false)

			eventuallyExpectNumberOfPolicies(1)
			eventuallyExpectConfig(policyName, expectedPolicy, expectedRefs)

			By("set BDF to true in the static hop")
			p, err := fakeRouteClient.K8sV1().AdminPolicyBasedExternalRoutes().Get(context.Background(), staticPolicy.Name, v1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			p.Spec.NextHops.StaticHops[0].BFDEnabled = true
			p.Generation++

			_, err = fakeRouteClient.K8sV1().AdminPolicyBasedExternalRoutes().Update(context.Background(), p, v1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())

			expectedPolicy, expectedRefs = expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTargetWithPod},
				[]string{staticHopGWIP},
				nil, true)

			eventuallyExpectNumberOfPolicies(1)
			eventuallyExpectConfig(policyName, expectedPolicy, expectedRefs)
		})

		It("validates that changing the BFD setting in a dynamic hop will trigger an update", func() {
			initController([]runtime.Object{namespaceGW, namespaceTarget2, targetPod2, pod1}, []runtime.Object{dynamicPolicy})

			policyName := dynamicPolicy.Name
			expectedPolicy, expectedRefs := expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTarget2WithPod},
				nil,
				[]*namespaceWithPods{namespaceGWWithPod}, false)

			eventuallyExpectNumberOfPolicies(1)
			eventuallyExpectConfig(policyName, expectedPolicy, expectedRefs)

			By("set BDF to true in the dynamic hop")
			p, err := fakeRouteClient.K8sV1().AdminPolicyBasedExternalRoutes().Get(context.Background(), dynamicPolicy.Name, v1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			p.Spec.NextHops.DynamicHops[0].BFDEnabled = true
			p.Generation++
			_, err = fakeRouteClient.K8sV1().AdminPolicyBasedExternalRoutes().Update(context.Background(), p, v1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())

			expectedPolicy, expectedRefs = expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTarget2WithPod},
				nil,
				[]*namespaceWithPods{namespaceGWWithPod}, true)

			eventuallyExpectNumberOfPolicies(1)
			eventuallyExpectConfig(policyName, expectedPolicy, expectedRefs)
		})
	})
})

func eventuallyCheckAPBRouteStatus(policyName string, expectFailure bool) {
	Eventually(func() bool {
		pol, err := fakeRouteClient.K8sV1().AdminPolicyBasedExternalRoutes().Get(context.TODO(), policyName, v1.GetOptions{})
		if err != nil {
			klog.Infof("Failed getting policy: %v", err)
			return false
		}
		if len(pol.Status.Messages) != 1 {
			klog.Infof("Policy has unexpected number of Status.Messages: %v", pol.Status.Messages)
			return false
		}
		// as soon as status is set, it should match expected status, without extra retries
		if expectFailure {
			return strings.Contains(pol.Status.Messages[0], types.APBRouteErrorMsg)
		} else {
			return !strings.Contains(pol.Status.Messages[0], types.APBRouteErrorMsg)
		}
	}, 5).Should(BeTrue())
}
