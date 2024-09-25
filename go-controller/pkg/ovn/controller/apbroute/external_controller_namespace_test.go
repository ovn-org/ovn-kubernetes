package apbroute

import (
	"context"
	"fmt"
	"time"

	nettypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	adminpolicybasedrouteapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1"
	adminpolicybasedrouteclient "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1/apis/clientset/versioned/fake"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/controller/apbroute/gateway_info"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"

	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes/fake"
)

const (
	annotatedPodIP         = "192.168.2.1"
	dynamicHopHostNetPodIP = "192.168.1.1"
	staticHopGWIP          = "10.10.10.1"
	networkAttachementName = "foo"
	network_status         = `[{"name":"foo","interface":"net1","ips":["%s"],"mac":"01:23:45:67:89:10"}]`
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
			{NamespaceSelector: *dynamicHopsNSSelector,
				PodSelector:           *dynamicHopsPodSelector,
				NetworkAttachmentName: networkAttachementName,
				BFDEnabled:            bfdEnabled},
		}
	}
	return &p
}

func deletePolicy(policyName string, fakeRouteClient *adminpolicybasedrouteclient.Clientset) {
	err = fakeRouteClient.K8sV1().AdminPolicyBasedExternalRoutes().Delete(context.TODO(), policyName, v1.DeleteOptions{})
	Expect(err).NotTo(HaveOccurred())
}

func deleteNamespace(namespaceName string, fakeClient *fake.Clientset) {
	ns, err := fakeClient.CoreV1().Namespaces().Get(context.Background(), namespaceName, v1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	ns.ObjectMeta.DeletionTimestamp = &v1.Time{Time: time.Now()}
	_, err = fakeClient.CoreV1().Namespaces().Update(context.Background(), ns, v1.UpdateOptions{})
	Expect(err).NotTo(HaveOccurred())
	err = fakeClient.CoreV1().Namespaces().Delete(context.Background(), namespaceName, v1.DeleteOptions{})
	Expect(err).NotTo(HaveOccurred())
}

func createNamespace(namespace *corev1.Namespace) {
	_, err := fakeClient.CoreV1().Namespaces().Create(context.Background(), namespace, v1.CreateOptions{})
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

func listRefObjects(policyName string) *policyReferencedObjects {
	mgr.policyReferencedObjectsLock.RLock()
	defer mgr.policyReferencedObjectsLock.RUnlock()
	return mgr.policyReferencedObjects[policyName]
}

type namespaceWithPods struct {
	nsName string
	pods   []*corev1.Pod
}

func newNamespaceWithPods(nsName string, pods ...*corev1.Pod) *namespaceWithPods {
	return &namespaceWithPods{
		nsName: nsName,
		pods:   pods,
	}
}
func expectedPolicyStateAndRefs(targetNamespaces []*namespaceWithPods, staticGWIPs []string,
	dynamicGws []*namespaceWithPods, bfdEnabled bool) (*routePolicyState, *policyReferencedObjects) {
	routeState := newRoutePolicyState()
	staticGWs := gateway_info.NewGatewayInfoList()
	for _, ip := range staticGWIPs {
		staticGWs.InsertOverwrite(gateway_info.NewGatewayInfo(sets.New(ip), bfdEnabled))
	}
	dynamicGWs := gateway_info.NewGatewayInfoList()
	dynamicGWNamespaces := sets.Set[string]{}
	dynamicGWPods := sets.Set[ktypes.NamespacedName]{}

	for _, dynamicGWNamespace := range dynamicGws {
		dynamicGWNamespaces.Insert(dynamicGWNamespace.nsName)
		for _, pod := range dynamicGWNamespace.pods {
			dynamicGWs.InsertOverwrite(gateway_info.NewGatewayInfo(sets.New(pod.Status.PodIPs[0].IP), bfdEnabled))
			dynamicGWPods.Insert(getPodNamespacedName(pod))
		}
	}

	targetNamespaceNames := sets.Set[string]{}
	for _, targetNS := range targetNamespaces {
		nsState := map[ktypes.NamespacedName]*podInfo{}
		for _, pod := range targetNS.pods {
			podInfo := &podInfo{
				staticGWs,
				dynamicGWs,
			}
			nsState[getPodNamespacedName(pod)] = podInfo
		}
		routeState.targetNamespaces[targetNS.nsName] = nsState
		targetNamespaceNames.Insert(targetNS.nsName)
	}

	return routeState, &policyReferencedObjects{
		targetNamespaces:    targetNamespaceNames,
		dynamicGWNamespaces: dynamicGWNamespaces,
		dynamicGWPods:       dynamicGWPods,
	}
}

var _ = Describe("OVN External Gateway namespace", func() {

	var (
		gatewayNamespaceName  = "gateway"
		gatewayNamespaceMatch = map[string]string{"name": gatewayNamespaceName}

		targetNamespaceName   = "target1"
		targetNamespaceName2  = "target2"
		targetNamespaceLabel  = "target"
		targetNamespace1Match = map[string]string{"name": targetNamespaceName}
		targetNamespace2Match = map[string]string{"name": targetNamespaceName2}

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
			&v1.LabelSelector{MatchLabels: map[string]string{"name": "pod"}},
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

		annotatedPodGW = &corev1.Pod{
			ObjectMeta: v1.ObjectMeta{Name: "annotatedPod", Namespace: namespaceGW.Name,
				Labels: map[string]string{"name": "annotatedPod"},
				Annotations: map[string]string{"k8s.ovn.org/routing-namespaces": "test",
					"k8s.ovn.org/routing-network": "",
					nettypes.NetworkStatusAnnot:   fmt.Sprintf(network_status, annotatedPodIP)},
			},
			Status: corev1.PodStatus{PodIPs: []corev1.PodIP{{IP: annotatedPodIP}}, Phase: corev1.PodRunning},
		}

		podGW = &corev1.Pod{
			ObjectMeta: v1.ObjectMeta{Name: "pod", Namespace: namespaceGW.Name,
				Labels:      map[string]string{"name": "pod"},
				Annotations: map[string]string{nettypes.NetworkStatusAnnot: fmt.Sprintf(network_status, dynamicHopHostNetPodIP)}},
			Status: corev1.PodStatus{PodIPs: []corev1.PodIP{{IP: dynamicHopHostNetPodIP}}, Phase: corev1.PodRunning},
		}
		namespaceTargetWithPod, namespaceTarget2WithPod, namespaceTarget2WithoutPod, namespaceGWWithPod *namespaceWithPods
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
		namespaceTarget2WithPod = newNamespaceWithPods(namespaceTarget2.Name, targetPod2)
		namespaceTarget2WithoutPod = newNamespaceWithPods(namespaceTarget2.Name)
		namespaceGWWithPod = newNamespaceWithPods(namespaceGW.Name, podGW)

	})

	var _ = Context("When no pod or namespace routing network annotations coexist with the policies", func() {

		var _ = Context("When creating new namespaces", func() {

			It("registers the new policy with no matching namespaces", func() {
				initController([]runtime.Object{namespaceTarget, targetPod1}, []runtime.Object{dynamicPolicy})
				policyName := dynamicPolicy.Name
				expectedPolicy, expectedRefs := expectedPolicyStateAndRefs(nil, nil, nil, false)

				eventuallyExpectNumberOfPolicies(1)
				eventuallyExpectConfig(policyName, expectedPolicy, expectedRefs)
			})

			It("registers the new namespace with one matching policy containing one static gateway", func() {
				initController([]runtime.Object{namespaceTarget, targetPod1}, []runtime.Object{staticPolicy})
				policyName := staticPolicy.Name
				expectedPolicy, expectedRefs := expectedPolicyStateAndRefs(
					[]*namespaceWithPods{namespaceTargetWithPod},
					[]string{staticHopGWIP},
					nil, false)

				eventuallyExpectNumberOfPolicies(1)
				eventuallyExpectConfig(policyName, expectedPolicy, expectedRefs)
			})
			It("registers a new namespace with one policy that includes a dynamic GW", func() {
				initController([]runtime.Object{namespaceTarget2, targetPod2, namespaceGW, podGW}, []runtime.Object{dynamicPolicy})

				policyName := dynamicPolicy.Name
				expectedPolicy, expectedRefs := expectedPolicyStateAndRefs(
					[]*namespaceWithPods{namespaceTarget2WithPod},
					nil,
					[]*namespaceWithPods{namespaceGWWithPod}, false)

				By("validating that the namespace cache contains the test namespace and that it reflect the applicable policy")
				eventuallyExpectNumberOfPolicies(1)
				eventuallyExpectConfig(policyName, expectedPolicy, expectedRefs)
			})

			It("registers a new namespace with one policy with dynamic GWs and the IP of an annotated pod", func() {
				dynamicPolicy = newPolicy(
					"dynamic",
					&v1.LabelSelector{MatchLabels: targetNamespace2Match},
					nil,
					&v1.LabelSelector{MatchLabels: gatewayNamespaceMatch},
					&v1.LabelSelector{},
					false,
				)
				initController([]runtime.Object{namespaceTarget2, targetPod2, namespaceGW, podGW, annotatedPodGW}, []runtime.Object{dynamicPolicy})
				namespaceGWWith2Pods := newNamespaceWithPods(namespaceGW.Name, podGW, annotatedPodGW)
				policyName := dynamicPolicy.Name

				expectedPolicy, expectedRefs := expectedPolicyStateAndRefs(
					[]*namespaceWithPods{namespaceTarget2WithPod},
					nil,
					[]*namespaceWithPods{namespaceGWWith2Pods}, false)

				By("validating that the namespace cache contains the test namespace and that it reflect the applicable policy")
				eventuallyExpectNumberOfPolicies(1)
				eventuallyExpectConfig(policyName, expectedPolicy, expectedRefs)
			})

		})
	})

	var _ = Context("When deleting a namespace", func() {

		It("validates that the namespace cache is empty and marked as deleted when the namespace was a recipient for policies", func() {
			initController([]runtime.Object{namespaceTarget, targetPod1}, []runtime.Object{staticPolicy})
			policyName := staticPolicy.Name
			expectedPolicy, expectedRefs := expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTargetWithPod},
				[]string{staticHopGWIP},
				nil, false)

			eventuallyExpectNumberOfPolicies(1)
			eventuallyExpectConfig(policyName, expectedPolicy, expectedRefs)

			deleteNamespace(namespaceTarget.Name, fakeClient)
			expectedPolicy, expectedRefs = expectedPolicyStateAndRefs(nil, nil, nil, false)

			eventuallyExpectNumberOfPolicies(1)
			eventuallyExpectConfig(policyName, expectedPolicy, expectedRefs)
		})
		It("validates that the namespace cache is empty when the namespace that is not a recipient for any policy is deleted", func() {
			// namespaceGW is not target namespace
			initController([]runtime.Object{namespaceGW}, []runtime.Object{staticPolicy})

			policyName := staticPolicy.Name
			expectedPolicy, expectedRefs := expectedPolicyStateAndRefs(nil, nil, nil, false)

			eventuallyExpectNumberOfPolicies(1)
			eventuallyExpectConfig(policyName, expectedPolicy, expectedRefs)

			deleteNamespace(namespaceGW.Name, fakeClient)

			eventuallyExpectNumberOfPolicies(1)
			eventuallyExpectConfig(policyName, expectedPolicy, expectedRefs)
		})

		It("validates that the namespace is only recreated after deletion when all the pods in the "+
			"namespace are deleted after a new event to create the namespace ", func() {

			initController([]runtime.Object{namespaceGW, namespaceTarget2, targetPod2, podGW}, []runtime.Object{dynamicPolicy})

			policyName := dynamicPolicy.Name
			expectedPolicy, expectedRefs := expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTarget2WithPod},
				nil,
				[]*namespaceWithPods{namespaceGWWithPod}, false)

			eventuallyExpectNumberOfPolicies(1)
			eventuallyExpectConfig(policyName, expectedPolicy, expectedRefs)

			By("delete the namespace whilst a pod still remains")
			deleteNamespace(namespaceTarget2.Name, fakeClient)
			By("create the namespace test again while it is being deleted")
			createNamespace(namespaceTarget2)

			By("delete the remaining pod in the namespace to proceed on deleting the namespace itself")
			deletePod(targetPod2, fakeClient)

			// at this point namespace exists, but without any pods
			expectedPolicy, expectedRefs = expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTarget2WithoutPod},
				nil,
				[]*namespaceWithPods{namespaceGWWithPod}, false)

			eventuallyExpectNumberOfPolicies(1)
			eventuallyExpectConfig(policyName, expectedPolicy, expectedRefs)
		})

		It("deletes an existing namespace with one policy and then creates it again and validates the policy has been applied to the new one with equal values", func() {
			initController([]runtime.Object{namespaceTarget2, targetPod2, namespaceGW, podGW},
				[]runtime.Object{dynamicPolicy})
			policyName := dynamicPolicy.Name
			expectedPolicy, expectedRefs := expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTarget2WithPod},
				nil,
				[]*namespaceWithPods{namespaceGWWithPod}, false)

			eventuallyExpectNumberOfPolicies(1)
			eventuallyExpectConfig(policyName, expectedPolicy, expectedRefs)

			deleteNamespace(namespaceTarget2.Name, fakeClient)
			By("validating that the cache no longer contains the test namespace")
			expectedPolicy2, expectedRefs2 := expectedPolicyStateAndRefs(
				nil,
				nil,
				[]*namespaceWithPods{namespaceGWWithPod}, false)

			eventuallyExpectNumberOfPolicies(1)
			eventuallyExpectConfig(policyName, expectedPolicy2, expectedRefs2)

			createNamespace(namespaceTarget2)
			By("validating that the namespace is contained in the namespace info cache and it reflects the correct policy")

			eventuallyExpectNumberOfPolicies(1)
			eventuallyExpectConfig(policyName, expectedPolicy, expectedRefs)
		})

	})

	var _ = Context("When updating an existing namespace", func() {

		It("validates that a namespace is targeted by an existing policy after its labels are updated to match "+
			"the policy's label selector", func() {
			namespaceTarget := &corev1.Namespace{
				ObjectMeta: v1.ObjectMeta{Name: "test",
					Labels: map[string]string{"name": "test"}},
			}
			targetPod := newPod("pod_target1", namespaceTarget.Name, "192.169.10.1",
				map[string]string{"key": "pod", "name": "pod_target1"})
			targetNamespaceWithPod := newNamespaceWithPods(namespaceTarget.Name, targetPod)
			initController([]runtime.Object{namespaceTarget, namespaceGW, targetPod}, []runtime.Object{staticPolicy})

			policyName := staticPolicy.Name
			expectedPolicy, expectedRefs := expectedPolicyStateAndRefs(nil, nil, nil, false)

			eventuallyExpectNumberOfPolicies(1)
			eventuallyExpectConfig(policyName, expectedPolicy, expectedRefs)

			updateNamespaceLabel(namespaceTarget.Name, staticPolicy.Spec.From.NamespaceSelector.MatchLabels, fakeClient)

			expectedPolicy, expectedRefs = expectedPolicyStateAndRefs(
				[]*namespaceWithPods{targetNamespaceWithPod},
				[]string{staticHopGWIP},
				nil, false)

			eventuallyExpectNumberOfPolicies(1)
			eventuallyExpectConfig(policyName, expectedPolicy, expectedRefs)
		})
		It("validates that a namespace is no longer targeted by an existing policy when its labels "+
			"are updated so that they don't match the policy's label selector", func() {
			initController([]runtime.Object{namespaceTarget, targetPod1}, []runtime.Object{staticPolicy})
			policyName := staticPolicy.Name
			expectedPolicy, expectedRefs := expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTargetWithPod},
				[]string{staticHopGWIP},
				nil, false)

			eventuallyExpectNumberOfPolicies(1)
			eventuallyExpectConfig(policyName, expectedPolicy, expectedRefs)

			updateNamespaceLabel(namespaceTarget.Name, dynamicPolicy.Spec.From.NamespaceSelector.MatchLabels, fakeClient)

			expectedPolicy, expectedRefs = expectedPolicyStateAndRefs(nil, nil, nil, false)

			eventuallyExpectNumberOfPolicies(1)
			eventuallyExpectConfig(policyName, expectedPolicy, expectedRefs)
		})

		It("validates that a namespace changes its policies when its labels are changed to match a different policy, "+
			"resulting in the later on being the only policy applied to the namespace", func() {
			initController([]runtime.Object{namespaceTarget, namespaceGW, targetPod1, podGW}, []runtime.Object{staticPolicy, dynamicPolicy})

			policyName1 := staticPolicy.Name
			expectedPolicy1, expectedRefs1 := expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTargetWithPod},
				[]string{staticHopGWIP},
				nil, false)

			policyName2 := dynamicPolicy.Name
			expectedPolicy2, expectedRefs2 := expectedPolicyStateAndRefs(
				nil,
				nil,
				[]*namespaceWithPods{namespaceGWWithPod}, false)

			eventuallyExpectNumberOfPolicies(2)
			eventuallyExpectConfig(policyName1, expectedPolicy1, expectedRefs1)
			eventuallyExpectConfig(policyName2, expectedPolicy2, expectedRefs2)

			updateNamespaceLabel(namespaceTarget.Name, dynamicPolicy.Spec.From.NamespaceSelector.MatchLabels, fakeClient)

			expectedPolicy1, expectedRefs1 = expectedPolicyStateAndRefs(nil, nil, nil, false)
			expectedPolicy2, expectedRefs2 = expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTargetWithPod},
				nil,
				[]*namespaceWithPods{namespaceGWWithPod}, false)

			eventuallyExpectNumberOfPolicies(2)
			eventuallyExpectConfig(policyName1, expectedPolicy1, expectedRefs1)
			eventuallyExpectConfig(policyName2, expectedPolicy2, expectedRefs2)
		})

		It("validates that a namespace can't be targeted by 2 policies", func() {
			dynamicPolicy = newPolicy(
				"dynamic",
				&v1.LabelSelector{MatchLabels: map[string]string{"extra": "label"}},
				nil,
				&v1.LabelSelector{MatchLabels: gatewayNamespaceMatch},
				&v1.LabelSelector{MatchLabels: map[string]string{"name": "pod"}},
				false,
			)
			initController([]runtime.Object{namespaceTarget, namespaceGW, targetPod1, podGW}, []runtime.Object{staticPolicy, dynamicPolicy})

			policyName1 := staticPolicy.Name
			expectedPolicy1, expectedRefs1 := expectedPolicyStateAndRefs(
				[]*namespaceWithPods{namespaceTargetWithPod},
				[]string{staticHopGWIP},
				nil, false)

			policyName2 := dynamicPolicy.Name
			expectedPolicy2, expectedRefs2 := expectedPolicyStateAndRefs(
				nil,
				[]string{staticHopGWIP},
				[]*namespaceWithPods{namespaceGWWithPod}, false)

			eventuallyExpectNumberOfPolicies(2)
			eventuallyExpectConfig(policyName1, expectedPolicy1, expectedRefs1)
			eventuallyExpectConfig(policyName2, expectedPolicy2, expectedRefs2)

			eventuallyCheckAPBRouteStatus(dynamicPolicy.Name, false)

			aggregatedLabels := map[string]string{"name": targetNamespaceName, "extra": "label"}
			updateNamespaceLabel(namespaceTarget.Name, aggregatedLabels, fakeClient)

			eventuallyCheckAPBRouteStatus(dynamicPolicy.Name, true)

			eventuallyExpectNumberOfPolicies(2)
			eventuallyExpectConfig(policyName1, expectedPolicy1, expectedRefs1)
			eventuallyExpectConfig(policyName2, expectedPolicy2, expectedRefs2)
		})
	})

})
