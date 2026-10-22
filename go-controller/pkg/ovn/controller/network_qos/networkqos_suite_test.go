// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package networkqos

import (
	"context"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	nadclientset "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned"
	nadinformers "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/informers/externalversions/k8s.cni.cncf.io/v1"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/workqueue"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	nqosapi "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1alpha1"
	networkqosclientset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1alpha1/apis/clientset/versioned"
	nqosfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1alpha1/apis/clientset/versioned/fake"
	crdtypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	addressset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/addresssetmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/syncmap"
	ovnk8stesting "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const (
	testNode             = "node1"
	testNamespace        = "network-qos-test"
	testPodName          = "client-pod"
	testPodDefaultIP     = "10.192.177.4"
	testPodStreamIP      = "10.128.2.3"
	streamNADKey         = "default/stream"
	streamNetwork        = "stream"
	defaultControllerNam = types.DefaultNetworkControllerName
	streamControllerName = streamNetwork + "-network-controller"
	defaultNADKey        = "default/default-network"
)

// Fix the network roles so these tests do not depend on the roles inferred from NADs.
type secondaryNetInfoWrapper struct {
	util.NetInfo
}

func (n *secondaryNetInfoWrapper) IsPrimaryNetwork() bool { return false }

func (n *secondaryNetInfoWrapper) IsUserDefinedNetwork() bool { return true }

func (n *secondaryNetInfoWrapper) IsDefault() bool { return false }

func (n *secondaryNetInfoWrapper) GetNetInfo() util.NetInfo { return n }

var (
	watchFactory   *factory.WatchFactory
	nbClient       libovsdbclient.Client
	nbCleanup      *libovsdbtest.Context
	fakeKubeClient kubernetes.Interface
	fakeNQoSClient networkqosclientset.Interface
	fakeNADClient  nadclientset.Interface
	nqosController *Controller
	addrSetManager *addresssetmanager.AddressSetManager
)

func init() {
	config.IPv4Mode = true
	config.IPv6Mode = false
	config.OVNKubernetesFeature.EnableNetworkQoS = true
	config.OVNKubernetesFeature.EnableMultiNetwork = true
}

func TestNetworkQoS(t *testing.T) {
	// The NAD fake client lacks WatchList support and hangs waiting for bookmarks.
	t.Setenv("KUBE_FEATURE_WatchListClient", "false")
	RegisterFailHandler(Fail)
	SetDefaultEventuallyTimeout(10 * time.Second)
	SetDefaultEventuallyPollingInterval(100 * time.Millisecond)
	RunSpecs(t, "NetworkQoS Controller (multi-network)")
}

var _ = AfterEach(func() {
	if nqosController != nil {
		nqosController.Stop()
		nqosController = nil
	}
	if addrSetManager != nil {
		addrSetManager.Stop()
		addrSetManager = nil
	}
	if watchFactory != nil {
		watchFactory.Shutdown()
		watchFactory = nil
	}
	if nbCleanup != nil {
		nbCleanup.Cleanup()
		nbCleanup = nil
	}
})

func initialDB() *libovsdbtest.TestSetup {
	return &libovsdbtest.TestSetup{
		NBData: []libovsdbtest.TestData{
			&nbdb.LogicalSwitch{Name: testNode},
			&nbdb.LogicalSwitch{Name: streamNetwork + "_" + testNode},
		},
	}
}

// testNodeObj deliberately has a hostname label different from its name.
func testNodeObj() *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   testNode,
			Labels: map[string]string{corev1.LabelHostname: testNode + ".example.test"},
		},
	}
}

func testNamespaceObj() *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   testNamespace,
			Labels: map[string]string{"app": "client"},
		},
	}
}

func testPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: testNamespace,
			Name:      testPodName,
			Labels:    map[string]string{"app": "client"},
			Annotations: map[string]string{
				types.OvnPodAnnotationName: fmt.Sprintf(
					`{"%s":{"ip_addresses":["%s/26"],"mac_address":"0a:58:0a:80:02:03"},`+
						`"default":{"ip_addresses":["%s/26"],"mac_address":"0a:58:0a:c0:b1:04","gateway_ips":["10.192.177.1"],"mtu":"1500","ip_address":"%s/26","gateway_ip":"10.192.177.1"}}`,
					streamNADKey, testPodStreamIP, testPodDefaultIP, testPodDefaultIP),
				nadapi.NetworkAttachmentAnnot: fmt.Sprintf(`[{"interface":"net1","name":"%s","namespace":"default"}]`, streamNetwork),
			},
		},
		Spec: corev1.PodSpec{NodeName: testNode},
	}
}

func defaultNetworkPod(name, namespace, node, ip, app string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: map[string]string{"app": app},
			Annotations: map[string]string{types.OvnPodAnnotationName: fmt.Sprintf(
				`{"default":{"ip_addresses":["%s/24"],"mac_address":"0a:58:0a:c0:b1:05"}}`, ip)}},
		Spec: corev1.PodSpec{NodeName: node},
	}
}

func newNetworkQoS(name string, networkSelectors crdtypes.NetworkSelectors) *nqosapi.NetworkQoS {
	return &nqosapi.NetworkQoS{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: testNamespace,
			Name:      name,
		},
		Spec: nqosapi.Spec{
			NetworkSelectors: networkSelectors,
			Priority:         100,
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "client"},
			},
			Egress: []nqosapi.Rule{
				{
					DSCP:      50,
					Bandwidth: nqosapi.Bandwidth{Rate: 10000, Burst: 100000},
				},
			},
		},
	}
}

func nadSelector(nadLabels map[string]string) crdtypes.NetworkSelector {
	return crdtypes.NetworkSelector{
		NetworkSelectionType: crdtypes.NetworkAttachmentDefinitions,
		NetworkAttachmentDefinitionSelector: &crdtypes.NetworkAttachmentDefinitionSelector{
			NetworkSelector: metav1.LabelSelector{MatchLabels: nadLabels},
		},
	}
}

func defaultNetworkNAD(nadLabels map[string]string) *nadapi.NetworkAttachmentDefinition {
	return &nadapi.NetworkAttachmentDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "default-network", Namespace: "default", Labels: nadLabels},
		Spec: nadapi.NetworkAttachmentDefinitionSpec{
			Config: `{"cniVersion":"0.4.0","name":"default","type":"ovn-k8s-cni-overlay"}`,
		},
	}
}

func streamNAD(nadLabels map[string]string) *nadapi.NetworkAttachmentDefinition {
	nad := ovnk8stesting.GenerateNAD(streamNetwork, streamNetwork, "default", types.Layer3Topology,
		"10.128.2.0/16/24", types.NetworkRoleSecondary)
	nad.Labels = nadLabels
	return nad
}

func startController(db *libovsdbtest.TestSetup, objects ...runtime.Object) *networkmanager.FakeNetworkManager {
	return startControllerWithRateLimiter(db, nil, objects...)
}

func startControllerWithRateLimiter(db *libovsdbtest.TestSetup, rateLimiter workqueue.TypedRateLimiter[string], objects ...runtime.Object) *networkmanager.FakeNetworkManager {
	clientset := util.GetOVNClientset(append([]runtime.Object{testNodeObj()}, objects...)...)
	fakeKubeClient = clientset.KubeClient
	fakeNADClient = clientset.NetworkAttchDefClient
	// The default fake does not track field ownership, so applying an empty status would
	// not remove this node's condition.
	nqosObjects := []runtime.Object{}
	for _, object := range objects {
		if nqos, isNQoS := object.(*nqosapi.NetworkQoS); isNQoS {
			nqosObjects = append(nqosObjects, nqos)
		}
	}
	fakeNQoSClient = nqosfake.NewClientset(nqosObjects...)
	clientset.NetworkQoSClient = fakeNQoSClient

	var err error
	watchFactory, err = factory.NewOVNKubeControllerWatchFactory(&util.OVNKubeControllerClientset{
		KubeClient:            clientset.KubeClient,
		NetworkQoSClient:      clientset.NetworkQoSClient,
		NetworkAttchDefClient: clientset.NetworkAttchDefClient,
	}, testNode)
	Expect(err).NotTo(HaveOccurred())

	nbClient, nbCleanup, err = libovsdbtest.NewNBTestHarness(*db, nil)
	Expect(err).NotTo(HaveOccurred())

	streamNetInfo, err := util.ParseNADInfo(streamNAD(nil))
	Expect(err).NotTo(HaveOccurred())
	mutableStreamNetInfo := util.NewMutableNetInfo(streamNetInfo)
	mutableStreamNetInfo.AddNADs(streamNADKey)
	networkManagerMock := &networkmanager.FakeNetworkManager{
		NADNetworks: map[string]util.NetInfo{
			streamNADKey:  &secondaryNetInfoWrapper{NetInfo: mutableStreamNetInfo},
			defaultNADKey: &util.DefaultNetInfo{},
		},
	}

	addrSetManager = addresssetmanager.NewAddressSetManager(watchFactory.PodCoreInformer(),
		watchFactory.NamespaceCoreInformer(), watchFactory.NodeCoreInformer(), nbClient,
		networkManagerMock.GetNetworkNameForNADKey)

	// Like the default network controller, pass no NAD informer without multiple networks.
	var nadInformer nadinformers.NetworkAttachmentDefinitionInformer
	if config.OVNKubernetesFeature.EnableMultiNetwork {
		nadInformer = watchFactory.NADInformer()
	}
	nqosController, err = NewController(
		testNode,
		nbClient,
		fakeNQoSClient,
		watchFactory.NetworkQoSInformer(),
		watchFactory.NamespaceCoreInformer(),
		nadInformer,
		networkManagerMock,
		addrSetManager,
		1,
	)
	Expect(err).NotTo(HaveOccurred())
	if rateLimiter != nil {
		controller.Stop(nqosController.nqosController)
		cfg := nqosController.networkQoSControllerConfig(watchFactory.NetworkQoSInformer(), 1)
		cfg.RateLimiter = rateLimiter
		nqosController.nqosController = controller.NewController("networkqos_networkqos_test", cfg)
	}
	Expect(watchFactory.Start()).To(Succeed())
	Expect(addrSetManager.Start()).To(Succeed())
	Expect(nqosController.Start()).To(Succeed())
	return networkManagerMock
}

// Use the test address set manager without starting controller workers.
func newDirectController(client libovsdbclient.Client) *Controller {
	return &Controller{
		nodeName:           testNode,
		nbClient:           client,
		networkManager:     &networkmanager.FakeNetworkManager{},
		addressSetManager:  addrSetManager,
		cache:              syncmap.NewSyncMap[*nqosCacheEntry](),
		reportedFailures:   sets.New[string](),
		unreportedFailures: sets.New[string](),
	}
}

type countingClient struct {
	libovsdbclient.Client
	transactions atomic.Int32
}

func (c *countingClient) Transact(ctx context.Context, ops ...ovsdb.Operation) ([]ovsdb.OperationResult, error) {
	c.transactions.Add(1)
	return c.Client.Transact(ctx, ops...)
}

type failQoSUnbindClient struct {
	libovsdbclient.Client
	failed bool
}

func (c *failQoSUnbindClient) Transact(ctx context.Context, ops ...ovsdb.Operation) ([]ovsdb.OperationResult, error) {
	for _, op := range ops {
		if !c.failed && op.Op == ovsdb.OperationMutate && op.Table == nbdb.LogicalSwitchTable {
			c.failed = true
			return nil, fmt.Errorf("temporary QoS unbind failure")
		}
	}
	return c.Client.Transact(ctx, ops...)
}

func conditions(name string) []metav1.Condition {
	nqos, err := fakeNQoSClient.K8sV1alpha1().NetworkQoSes(testNamespace).Get(context.Background(), name,
		metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	return nqos.Status.Conditions
}

func findQoS(controllerName, namespace, name string, index int) *nbdb.QoS {
	return findQoSByRuleID(controllerName, namespace, name, ruleStringID(index))
}

// The row of a rule for the packets routers send into the switch.
func findReturnPassQoS(controllerName, namespace, name string, index int) *nbdb.QoS {
	return findQoSByRuleID(controllerName, namespace, name, returnPassRuleID(index))
}

func findQoSByRuleID(controllerName, namespace, name, id string) *nbdb.QoS {
	objectName := ovnObjectName(namespace, name)
	qoses, err := libovsdbops.FindQoSesWithPredicate(nbClient, func(qos *nbdb.QoS) bool {
		return qos.ExternalIDs[libovsdbops.OwnerControllerKey.String()] == controllerName &&
			qos.ExternalIDs[libovsdbops.OwnerTypeKey.String()] == string(libovsdbops.NetworkQoSOwnerType) &&
			qos.ExternalIDs[libovsdbops.ObjectNameKey.String()] == objectName &&
			qos.ExternalIDs[libovsdbops.RuleIndex.String()] == id
	})
	Expect(err).NotTo(HaveOccurred())
	if len(qoses) == 0 {
		return nil
	}
	return qoses[0]
}

func eventuallyQoS(controllerName, namespace, name string, index int) *nbdb.QoS {
	var qos *nbdb.QoS
	Eventually(func() *nbdb.QoS {
		qos = findQoS(controllerName, namespace, name, index)
		return qos
	}).WithTimeout(10*time.Second).WithPolling(500*time.Millisecond).ShouldNot(BeNil(),
		fmt.Sprintf("no QoS found for %s/%s on %s", namespace, name, controllerName))
	return qos
}

func eventuallyNoQoS(controllerName, namespace, name string, index int) {
	Eventually(func() *nbdb.QoS {
		return findQoS(controllerName, namespace, name, index)
	}).WithTimeout(10*time.Second).WithPolling(500*time.Millisecond).Should(BeNil(),
		fmt.Sprintf("unexpected QoS found for %s/%s on %s", namespace, name, controllerName))
}

func eventuallySwitchHasQoS(switchName string, qos *nbdb.QoS) {
	Eventually(func() bool {
		ls, err := libovsdbops.GetLogicalSwitch(nbClient, &nbdb.LogicalSwitch{Name: switchName})
		if err != nil {
			return false
		}
		return slices.Contains(ls.QOSRules, qos.UUID)
	}).WithTimeout(10*time.Second).WithPolling(500*time.Millisecond).Should(BeTrue(),
		fmt.Sprintf("QoS rule %s not bound to switch %s", qos.UUID, switchName))
}

func sourceAddressSetIDs(controllerName string) *libovsdbops.DbObjectIDs {
	return addresssetmanager.GetPodSelectorAddrSetDbIDs(
		&metav1.LabelSelector{MatchLabels: map[string]string{"app": "client"}},
		nil,
		nil,
		testNamespace, controllerName, false, addresssetmanager.ExcludeHostNetworkPods(), addresssetmanager.WithNodeName(testNode))
}

func sourceAddresses(controllerName string) []string {
	addrSetFactory := addressset.NewOvnAddressSetFactory(nbClient, config.IPv4Mode, config.IPv6Mode)
	addrSet, err := addrSetFactory.GetAddressSet(sourceAddressSetIDs(controllerName))
	if err != nil {
		return nil
	}
	v4, v6 := addrSet.GetAddresses()
	return append(v4, v6...)
}

func findAddressSets(ids *libovsdbops.DbObjectIDs) []*nbdb.AddressSet {
	addrSets, err := libovsdbops.FindAddressSetsWithPredicate(nbClient, libovsdbops.GetPredicate[*nbdb.AddressSet](ids, nil))
	Expect(err).NotTo(HaveOccurred())
	return addrSets
}

func findSourceAddressSets(controllerName string) []*nbdb.AddressSet {
	return findAddressSets(sourceAddressSetIDs(controllerName))
}

func eventuallyAddressSetHas(controllerName, address string) {
	Eventually(func() []string {
		return sourceAddresses(controllerName)
	}).WithTimeout(10*time.Second).WithPolling(500*time.Millisecond).Should(ContainElement(address),
		fmt.Sprintf("address %s missing from the source address set of %s", address, controllerName))
}

func eventuallyNoAddressSet(controllerName string) {
	Eventually(func() []*nbdb.AddressSet {
		return findSourceAddressSets(controllerName)
	}).WithTimeout(10*time.Second).WithPolling(500*time.Millisecond).Should(BeEmpty(),
		fmt.Sprintf("the source address set on %s was not deleted", controllerName))
}
