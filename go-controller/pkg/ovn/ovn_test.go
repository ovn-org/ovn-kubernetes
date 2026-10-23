// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	ipamclaimsapi "github.com/k8snetworkplumbingwg/ipamclaims/pkg/crd/ipamclaims/v1alpha1"
	fakeipamclaimclient "github.com/k8snetworkplumbingwg/ipamclaims/pkg/crd/ipamclaims/v1alpha1/apis/clientset/versioned/fake"
	mnpapi "github.com/k8snetworkplumbingwg/multi-networkpolicy/pkg/apis/k8s.cni.cncf.io/v1beta1"
	mnpfake "github.com/k8snetworkplumbingwg/multi-networkpolicy/pkg/client/clientset/versioned/fake"
	nettypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	fakenadclient "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/fake"
	"github.com/onsi/gomega"
	ocpnetworkapiv1alpha1 "github.com/openshift/api/network/v1alpha1"
	ocpnetworkfake "github.com/openshift/client-go/network/clientset/versioned/fake"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"
	anpapi "sigs.k8s.io/network-policy-api/apis/v1alpha1"
	anpfake "sigs.k8s.io/network-policy-api/pkg/client/clientset/versioned/fake"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	ovncnitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	nodecontroller "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controllers/node"
	podcontroller "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controllers/pod"
	adminpolicybasedrouteapi "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1"
	adminpolicybasedroutefake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1/apis/clientset/versioned/fake"
	egressfirewall "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1"
	egressfirewallfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/clientset/versioned/fake"
	egressip "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	egressipfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/clientset/versioned/fake"
	egressqos "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressqos/v1"
	egressqosfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressqos/v1/apis/clientset/versioned/fake"
	egressservice "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressservice/v1"
	egressservicefake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressservice/v1/apis/clientset/versioned/fake"
	uplinkv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1"
	uplinkfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/clientset/versioned/fake"
	udnclientfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/clientset/versioned/fake"
	vtepfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1/apis/clientset/versioned/fake"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	addressset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/addresssetmanager"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	util "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

const (
	k8sTCPLoadBalancerIP        = "k8s_tcp_load_balancer"
	k8sUDPLoadBalancerIP        = "k8s_udp_load_balancer"
	k8sSCTPLoadBalancerIP       = "k8s_sctp_load_balancer"
	k8sIdlingTCPLoadBalancerIP  = "k8s_tcp_idling_load_balancer"
	k8sIdlingUDPLoadBalancerIP  = "k8s_udp_idling_load_balancer"
	k8sIdlingSCTPLoadBalancerIP = "k8s_sctp_idling_load_balancer"
	fakeUUID                    = "8a86f6d8-7972-4253-b0bd-ddbef66e9303"
	fakeUUIDv6                  = "8a86f6d8-7972-4253-b0bd-ddbef66e9304"
	fakePgUUID                  = "bf02f460-5058-4689-8fcb-d31a1e484ed2"
	ovnClusterPortGroupUUID     = fakePgUUID
	coppUUID                    = "copp-UUID"
)

type userDefinedNetworkControllerInfo struct {
	bnc *BaseUserDefinedNetworkController
	asf *addressset.FakeAddressSetFactory
}

// testNetInfo is a test helper that wraps util.NetInfo to allow overriding
// specific methods for testing purposes.
type testNetInfo struct {
	util.NetInfo
	topology     string
	outboundSNAT string
	subnets      []config.CIDRNetworkEntry
	transport    string
	// ipMode, when set, overrides IPMode() as {IPv4, IPv6}; otherwise the
	// embedded NetInfo's IPMode() is used.
	ipMode *[2]bool
}

func (ni *testNetInfo) TopologyType() string {
	return ni.topology
}

func (ni *testNetInfo) IPMode() (bool, bool) {
	if ni.ipMode != nil {
		return ni.ipMode[0], ni.ipMode[1]
	}
	return ni.NetInfo.IPMode()
}

func (ni *testNetInfo) Subnets() []config.CIDRNetworkEntry {
	return ni.subnets
}

func (ni *testNetInfo) Transport() string {
	return ni.transport
}

func (ni *testNetInfo) OutboundSNAT() string {
	return ni.outboundSNAT
}

type FakeOVN struct {
	nodeName          string
	fakeClient        *util.OVNKubeControllerClientset
	watcher           *factory.WatchFactory
	controller        *DefaultNetworkController
	stopChan          chan struct{}
	wg                *sync.WaitGroup
	asf               *addressset.FakeAddressSetFactory
	fakeRecorder      *record.FakeRecorder
	nbClient          libovsdbclient.Client
	sbClient          libovsdbclient.Client
	dbSetup           libovsdbtest.TestSetup
	nbsbCleanup       *libovsdbtest.Context
	egressQoSWg       *sync.WaitGroup
	egressSVCWg       *sync.WaitGroup
	anpWg             *sync.WaitGroup
	networkManager    networkmanager.Controller
	eIPController     *EgressIPController
	addressSetManager *addresssetmanager.AddressSetManager
	portCache         *PortCache
	udnNodeController *nodecontroller.NodeController
	udnPodController  *podcontroller.Controller

	// information map of all UDN controllers
	userDefinedNetworkControllers map[string]userDefinedNetworkControllerInfo
	fullL2UDNControllers          map[string]*Layer2UserDefinedNetworkController
	fullL3UDNControllers          map[string]*Layer3UserDefinedNetworkController
	fullLocalnetUDNControllers    map[string]*LocalnetUserDefinedNetworkController
}

// NOTE: the FakeAddressSetFactory is no longer needed and should no longer be used. starting to phase out FakeAddressSetFactory
func NewFakeOVN(useFakeAddressSet bool, nodeName string) *FakeOVN {
	var asf *addressset.FakeAddressSetFactory
	if useFakeAddressSet {
		asf = addressset.NewFakeAddressSetFactory(types.DefaultNetworkControllerName)
	}
	fakeOVN := &FakeOVN{
		nodeName:     nodeName,
		asf:          asf,
		fakeRecorder: record.NewFakeRecorder(10),
		egressQoSWg:  &sync.WaitGroup{},
		egressSVCWg:  &sync.WaitGroup{},
		anpWg:        &sync.WaitGroup{},

		userDefinedNetworkControllers: map[string]userDefinedNetworkControllerInfo{},
		fullL2UDNControllers:          map[string]*Layer2UserDefinedNetworkController{},
		fullL3UDNControllers:          map[string]*Layer3UserDefinedNetworkController{},
		fullLocalnetUDNControllers:    map[string]*LocalnetUserDefinedNetworkController{},
	}
	return fakeOVN
}

func (o *FakeOVN) start(objects ...runtime.Object) {
	fexec := ovntest.NewFakeExec()
	err := util.SetExec(fexec)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())

	egressIPObjects := []runtime.Object{}
	egressFirewallObjects := []runtime.Object{}
	dnsNameResolverObjects := []runtime.Object{}
	egressQoSObjects := []runtime.Object{}
	multiNetworkPolicyObjects := []runtime.Object{}
	egressServiceObjects := []runtime.Object{}
	apbExternalRouteObjects := []runtime.Object{}
	anpObjects := []runtime.Object{}
	ipamClaimObjects := []runtime.Object{}
	uplinkObjects := []runtime.Object{}
	v1Objects := []runtime.Object{}
	nads := []nettypes.NetworkAttachmentDefinition{}
	nadClient := fakenadclient.NewSimpleClientset()
	for _, object := range objects {
		switch o := object.(type) {
		case *egressip.EgressIPList:
			egressIPObjects = append(egressIPObjects, object)
		case *egressfirewall.EgressFirewallList:
			egressFirewallObjects = append(egressFirewallObjects, object)
		case *ocpnetworkapiv1alpha1.DNSNameResolverList:
			dnsNameResolverObjects = append(dnsNameResolverObjects, object)
		case *egressqos.EgressQoSList:
			egressQoSObjects = append(egressQoSObjects, object)
		case *mnpapi.MultiNetworkPolicyList:
			multiNetworkPolicyObjects = append(multiNetworkPolicyObjects, object)
		case *egressservice.EgressServiceList:
			egressServiceObjects = append(egressServiceObjects, object)
		case *nettypes.NetworkAttachmentDefinitionList:
			// must provision the NAD tracker manually, as per
			// https://github.com/ovn-kubernetes/ovn-kubernetes/blob/65c79af35b2c22f90c863debefa15c4fb1f088cb/go-controller/vendor/k8s.io/client-go/testing/fixture.go#L341
			// since the NADs use arbitrary API registration names, which `UnsafeGuessKindToResource` cannot resolve.
			for _, nad := range o.Items {
				if err := nadClient.Tracker().Create(schema.GroupVersionResource(nadGVR()), &nad, nad.Namespace); err != nil {
					panic(err)
				}
			}
			nads = append(nads, o.Items...)
		case *adminpolicybasedrouteapi.AdminPolicyBasedExternalRouteList:
			apbExternalRouteObjects = append(apbExternalRouteObjects, object)
		case *anpapi.AdminNetworkPolicyList:
			anpObjects = append(anpObjects, object)
		case *ipamclaimsapi.IPAMClaimList:
			ipamClaimObjects = append(ipamClaimObjects, object)
		case *uplinkv1alpha1.UplinkList:
			for i := range o.Items {
				uplinkObjects = append(uplinkObjects, &o.Items[i])
			}
		case *uplinkv1alpha1.UplinkStateList:
			for i := range o.Items {
				uplinkObjects = append(uplinkObjects, &o.Items[i])
			}
		default:
			v1Objects = append(v1Objects, object)
		}
	}
	o.fakeClient = &util.OVNKubeControllerClientset{
		KubeClient:               fake.NewSimpleClientset(v1Objects...),
		ANPClient:                anpfake.NewSimpleClientset(anpObjects...),
		EgressIPClient:           egressipfake.NewSimpleClientset(egressIPObjects...),
		EgressFirewallClient:     egressfirewallfake.NewSimpleClientset(egressFirewallObjects...),
		OCPNetworkClient:         ocpnetworkfake.NewSimpleClientset(dnsNameResolverObjects...),
		EgressQoSClient:          egressqosfake.NewSimpleClientset(egressQoSObjects...),
		MultiNetworkPolicyClient: mnpfake.NewSimpleClientset(multiNetworkPolicyObjects...),
		EgressServiceClient:      egressservicefake.NewSimpleClientset(egressServiceObjects...),
		AdminPolicyRouteClient:   adminpolicybasedroutefake.NewSimpleClientset(apbExternalRouteObjects...),
		IPAMClaimsClient:         fakeipamclaimclient.NewSimpleClientset(ipamClaimObjects...),
		NetworkAttchDefClient:    nadClient,
		UserDefinedNetworkClient: udnclientfake.NewSimpleClientset(),
		UplinkClient:             uplinkfake.NewSimpleClientset(uplinkObjects...),
		VTEPClient:               vtepfake.NewSimpleClientset(),
	}
	o.init(nads)
}

func (o *FakeOVN) startWithDBSetup(dbSetup libovsdbtest.TestSetup, objects ...runtime.Object) {
	o.dbSetup = dbSetup
	o.start(objects...)
}

func (o *FakeOVN) shutdown() {
	if o.udnNodeController != nil {
		o.udnNodeController.Stop()
	}
	if o.udnPodController != nil {
		o.udnPodController.Stop()
	}
	if o.controller != nil && o.controller.nodeReconciler != nil {
		o.controller.nodeReconciler.Stop()
	}
	if o.controller != nil && o.controller.podReconciler != nil {
		o.controller.podReconciler.Stop()
	}
	o.watcher.Shutdown()
	close(o.stopChan)
	o.controller.cancelableCtx.Cancel()
	o.wg.Wait()
	o.egressQoSWg.Wait()
	o.egressSVCWg.Wait()
	o.anpWg.Wait()
	if o.networkManager != nil {
		o.networkManager.Stop()
	}
	if o.addressSetManager != nil {
		o.addressSetManager.Stop()
	}
	o.nbsbCleanup.Cleanup()
	for _, ocInfo := range o.userDefinedNetworkControllers {
		close(ocInfo.bnc.stopChan)
		ocInfo.bnc.cancelableCtx.Cancel()
		ocInfo.bnc.wg.Wait()
	}
}

func (o *FakeOVN) init(nadList []nettypes.NetworkAttachmentDefinition) {
	var err error
	// Use shorter event queues for unit tests (reduce to 10 from the default)
	// to avoid running out of resources in constrained CI environments
	// (e.g., on GitHub).
	factory.SetEventQueueSize(10)

	o.watcher, err = factory.NewOVNKubeControllerWatchFactory(o.fakeClient, "test-node")
	gomega.Expect(err).NotTo(gomega.HaveOccurred())

	o.nbClient, o.sbClient, o.nbsbCleanup, err = libovsdbtest.NewNBSBTestHarness(o.dbSetup)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())

	o.stopChan = make(chan struct{})
	o.wg = &sync.WaitGroup{}

	if o.networkManager == nil {
		o.networkManager = networkmanager.Default()
		if config.OVNKubernetesFeature.EnableMultiNetwork {
			o.networkManager, err = networkmanager.NewForNode(o.nodeName, &networkmanager.FakeControllerManager{}, o.watcher)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}
	}

	o.portCache = NewPortCache(o.stopChan)
	o.addressSetManager = addresssetmanager.NewAddressSetManager(o.watcher.PodCoreInformer(),
		o.watcher.NamespaceInformer(), o.watcher.NodeCoreInformer(), o.nbClient, o.networkManager.Interface().GetNetworkNameForNADKey)

	kubeOVN := &kube.KubeOVN{
		Kube:      kube.Kube{KClient: o.fakeClient.KubeClient},
		EIPClient: o.fakeClient.EgressIPClient,
	}
	o.eIPController = NewEIPController(
		o.nbClient,
		kubeOVN,
		o.watcher,
		o.fakeRecorder,
		o.portCache,
		o.networkManager.Interface(),
		o.asf,
		o.addressSetManager,
		config.IPv4Mode,
		config.IPv6Mode,
		o.nodeName,
		types.DefaultNetworkControllerName,
	)

	if o.asf == nil {
		o.eIPController.addressSetFactory = addressset.NewOvnAddressSetFactory(o.nbClient, config.IPv4Mode, config.IPv6Mode)
	}

	o.controller, err = NewOvnController(o.fakeClient,
		o.watcher,
		o.stopChan,
		o.asf,
		o.networkManager.Interface(),
		o.nbClient,
		o.sbClient,
		o.fakeRecorder,
		o.wg,
		o.eIPController,
		o.portCache,
		o.addressSetManager,
		o.nodeName,
	)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	o.controller.multicastSupport = config.EnableMulticast
	o.udnNodeController = o.controller.nodeReconciler
	o.udnPodController = o.controller.podReconciler

	setupCOPP := false
	setupClusterController(o.controller, setupCOPP)
	for _, n := range nadList {
		err := o.NewUserDefinedNetworkController(&n)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	}

	err = o.watcher.Start()
	gomega.Expect(err).NotTo(gomega.HaveOccurred())

	err = o.addressSetManager.Start()
	gomega.Expect(err).NotTo(gomega.HaveOccurred())

	err = o.udnNodeController.Start()
	gomega.Expect(err).NotTo(gomega.HaveOccurred())

	err = o.udnPodController.Start()
	gomega.Expect(err).NotTo(gomega.HaveOccurred())

	err = o.networkManager.Start()
	gomega.Expect(err).NotTo(gomega.HaveOccurred())

	err = o.eIPController.SyncNodeCache()
	gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "syncing EgressIP node state must succeed")

}

// creates the global entities that should remain after a UDN created and removed
func generateUDNPostInitDB(testData []libovsdbtest.TestData) []libovsdbtest.TestData {
	testData = append(testData, &nbdb.MeterBand{
		UUID:   "25-pktps-rate-limiter-UUID",
		Action: types.MeterAction,
		Rate:   int(25),
	})
	meters := map[string]string{
		OVNARPRateLimiter:              getMeterNameForProtocol(OVNARPRateLimiter),
		OVNARPResolveRateLimiter:       getMeterNameForProtocol(OVNARPResolveRateLimiter),
		OVNBFDRateLimiter:              getMeterNameForProtocol(OVNBFDRateLimiter),
		OVNControllerEventsRateLimiter: getMeterNameForProtocol(OVNControllerEventsRateLimiter),
		OVNICMPV4ErrorsRateLimiter:     getMeterNameForProtocol(OVNICMPV4ErrorsRateLimiter),
		OVNICMPV6ErrorsRateLimiter:     getMeterNameForProtocol(OVNICMPV6ErrorsRateLimiter),
		OVNRejectRateLimiter:           getMeterNameForProtocol(OVNRejectRateLimiter),
		OVNTCPRSTRateLimiter:           getMeterNameForProtocol(OVNTCPRSTRateLimiter),
		OVNServiceMonitorLimiter:       getMeterNameForProtocol(OVNServiceMonitorLimiter),
	}
	fairness := true
	for _, v := range meters {
		testData = append(testData, &nbdb.Meter{
			UUID:  v + "-UUID",
			Bands: []string{"25-pktps-rate-limiter-UUID"},
			Name:  v,
			Unit:  types.PacketsPerSecond,
			Fair:  &fairness,
		})
	}

	copp := &nbdb.Copp{
		UUID:   coppUUID,
		Name:   "ovnkube-default",
		Meters: meters,
	}
	testData = append(testData, copp)

	return testData
}

func setupClusterController(clusterController *DefaultNetworkController, setupCOPP bool) {
	var err error

	clusterLBGroup := &nbdb.LoadBalancerGroup{Name: types.ClusterLBGroupName}
	err = clusterController.nbClient.Get(context.Background(), clusterLBGroup)
	gomega.Expect(err).To(gomega.SatisfyAny(gomega.BeNil(), gomega.MatchError(libovsdbclient.ErrNotFound)))
	clusterController.clusterLoadBalancerGroupUUID = clusterLBGroup.UUID

	clusterSwitchLBGroup := &nbdb.LoadBalancerGroup{Name: types.ClusterSwitchLBGroupName}
	err = clusterController.nbClient.Get(context.Background(), clusterSwitchLBGroup)
	gomega.Expect(err).To(gomega.SatisfyAny(gomega.BeNil(), gomega.MatchError(libovsdbclient.ErrNotFound)))
	clusterController.switchLoadBalancerGroupUUID = clusterSwitchLBGroup.UUID

	clusterRouterLBGroup := &nbdb.LoadBalancerGroup{Name: types.ClusterRouterLBGroupName}
	err = clusterController.nbClient.Get(context.Background(), clusterRouterLBGroup)
	gomega.Expect(err).To(gomega.SatisfyAny(gomega.BeNil(), gomega.MatchError(libovsdbclient.ErrNotFound)))
	clusterController.routerLoadBalancerGroupUUID = clusterRouterLBGroup.UUID

	if setupCOPP {
		clusterController.defaultCOPPUUID, err = EnsureDefaultCOPP(clusterController.nbClient)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	}
}

func resetNBClient(ctx context.Context, nbClient libovsdbclient.Client) {
	if nbClient.Connected() {
		nbClient.Close()
	}
	gomega.Eventually(func() bool {
		return nbClient.Connected()
	}).Should(gomega.BeFalse())
	err := nbClient.Connect(ctx)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	gomega.Eventually(func() bool {
		return nbClient.Connected()
	}).Should(gomega.BeTrue())
	_, err = nbClient.MonitorAll(ctx)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
}

// NewOvnController creates a new OVN controller for creating logical network
// infrastructure and policy
func NewOvnController(
	ovnClient *util.OVNKubeControllerClientset,
	wf *factory.WatchFactory,
	stopChan chan struct{},
	addressSetFactory addressset.AddressSetFactory,
	networkManager networkmanager.Interface,
	libovsdbOvnNBClient libovsdbclient.Client,
	libovsdbOvnSBClient libovsdbclient.Client,
	recorder record.EventRecorder,
	wg *sync.WaitGroup,
	eIPController *EgressIPController,
	portCache *PortCache,
	addressSetManager *addresssetmanager.AddressSetManager,
	nodeName string,
) (*DefaultNetworkController, error) {

	fakeAddr, ok := addressSetFactory.(*addressset.FakeAddressSetFactory)
	if addressSetFactory == nil || (ok && fakeAddr == nil) {
		addressSetFactory = addressset.NewOvnAddressSetFactory(libovsdbOvnNBClient, config.IPv4Mode, config.IPv6Mode)
	}

	podRecorder := metrics.NewPodRecorder()

	cnci := NewCommonNetworkControllerInfo(
		ovnClient.KubeClient,
		&kube.KubeOVN{
			Kube:                 kube.Kube{KClient: ovnClient.KubeClient},
			ANPClient:            ovnClient.ANPClient,
			EIPClient:            ovnClient.EgressIPClient,
			EgressFirewallClient: ovnClient.EgressFirewallClient,
			EgressServiceClient:  ovnClient.EgressServiceClient,
			APBRouteClient:       ovnClient.AdminPolicyRouteClient,
			EgressQoSClient:      ovnClient.EgressQoSClient,
			IPAMClaimsClient:     ovnClient.IPAMClaimsClient,
		},
		wf,
		recorder,
		libovsdbOvnNBClient,
		libovsdbOvnSBClient,
		&podRecorder,
		false, // multicast support
		true,  // templates support
		nodeName,
	)

	nodeReconciler := nodecontroller.NewNodeController(wf, networkManager, cnci.nodeName)
	podReconciler := podcontroller.NewPodController(wf)
	dnc, err := newDefaultNetworkControllerCommon(cnci, stopChan, wg, addressSetFactory, networkManager, nil, nil, eIPController, portCache, addressSetManager, nodeReconciler, podReconciler)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())

	return dnc, err
}

func (o *FakeOVN) InitAndRunANPController() {
	err := o.controller.newANPController()
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	o.anpWg.Add(1)
	go func() {
		defer o.anpWg.Done()
		o.controller.anpController.Run(1, o.stopChan)
	}()
}

func newNetworkAttachmentDefinition(namespace, name string, netconf ovncnitypes.NetConf) (*nettypes.NetworkAttachmentDefinition, error) {
	bytes, err := json.Marshal(netconf)
	if err != nil {
		return nil, fmt.Errorf("failed marshaling podNetworks map %v", netconf)
	}
	meta := newObjectMeta(name, namespace)
	meta.Annotations = map[string]string{types.OvnNetworkIDAnnotation: userDefinedNetworkID}
	if netconf.Topology == types.Layer2Topology && netconf.Role == types.NetworkRolePrimary {
		meta.Annotations[types.OvnNetworkTunnelKeysAnnotation] = "[16711685,16715780]"
	}
	return &nettypes.NetworkAttachmentDefinition{
		ObjectMeta: meta,
		Spec: nettypes.NetworkAttachmentDefinitionSpec{
			Config: string(bytes),
		},
	}, nil
}

func (o *FakeOVN) NewUserDefinedNetworkController(netattachdef *nettypes.NetworkAttachmentDefinition) error {
	var ocInfo userDefinedNetworkControllerInfo
	var userDefinedNetworkController *BaseUserDefinedNetworkController
	var ok bool

	nadName := util.GetNADName(netattachdef.Namespace, netattachdef.Name)
	nInfo, err := util.ParseNADInfo(netattachdef)
	if err != nil {
		return err
	}
	netName := nInfo.GetNetworkName()
	topoType := nInfo.TopologyType()
	_, ok = o.userDefinedNetworkControllers[netName]
	if !ok {
		podRecorder := metrics.NewPodRecorder()
		cnci := NewCommonNetworkControllerInfo(
			o.fakeClient.KubeClient,
			&kube.KubeOVN{
				Kube:                 kube.Kube{KClient: o.fakeClient.KubeClient},
				EIPClient:            o.fakeClient.EgressIPClient,
				EgressFirewallClient: o.fakeClient.EgressFirewallClient,
				IPAMClaimsClient:     o.fakeClient.IPAMClaimsClient,
			},
			o.watcher,
			o.fakeRecorder,
			o.nbClient,
			o.sbClient,
			&podRecorder,
			false, // multicast support
			true,  // templates support
			o.nodeName,
		)

		asf := addressset.NewFakeAddressSetFactory(getNetworkControllerName(netName))

		mutableNetInfo := util.NewMutableNetInfo(nInfo)
		mutableNetInfo.AddNADs(nadName)

		switch topoType {
		case types.Layer3Topology:
			l3Controller, err := NewLayer3UserDefinedNetworkController(cnci, mutableNetInfo, o.networkManager.Interface(), nil,
				o.eIPController, o.portCache, o.addressSetManager, o.udnNodeController, o.udnPodController, o.controller.ServiceController())
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			if o.asf != nil { // use fake asf only when enabled
				l3Controller.addressSetFactory = asf
			}
			userDefinedNetworkController = &l3Controller.BaseUserDefinedNetworkController
			o.fullL3UDNControllers[netName] = l3Controller
		case types.Layer2Topology:
			l2Controller, err := NewLayer2UserDefinedNetworkController(cnci, mutableNetInfo, o.networkManager.Interface(), nil,
				o.portCache, o.eIPController, o.addressSetManager, o.udnNodeController, o.udnPodController, o.controller.ServiceController())
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			if o.asf != nil { // use fake asf only when enabled
				l2Controller.addressSetFactory = asf
			}
			userDefinedNetworkController = &l2Controller.BaseUserDefinedNetworkController
			o.fullL2UDNControllers[netName] = l2Controller
		case types.LocalnetTopology:
			localnetController := NewLocalnetUserDefinedNetworkController(cnci, mutableNetInfo, o.networkManager.Interface(), o.addressSetManager,
				o.udnNodeController, o.udnPodController)
			if o.asf != nil { // use fake asf only when enabled
				localnetController.addressSetFactory = asf
			}
			userDefinedNetworkController = &localnetController.BaseUserDefinedNetworkController
			o.fullLocalnetUDNControllers[netName] = localnetController
		default:
			return fmt.Errorf("topology type %s not supported", topoType)
		}
		ocInfo = userDefinedNetworkControllerInfo{bnc: userDefinedNetworkController, asf: asf}
		o.userDefinedNetworkControllers[netName] = ocInfo

	}

	return nil
}

func (o *FakeOVN) registerUDNNodeHandler(networkName string) error {
	if c, ok := o.fullL3UDNControllers[networkName]; ok {
		return c.RegisterNodeHandler()
	}
	if c, ok := o.fullL2UDNControllers[networkName]; ok {
		return c.RegisterNodeHandler()
	}
	if _, ok := o.fullLocalnetUDNControllers[networkName]; ok {
		return nil
	}
	return fmt.Errorf("no concrete UDN controller found for network %s", networkName)
}

func (o *FakeOVN) patchEgressIPObj(nodeName, egressIPName, egressIP string) {
	// NOTE: Cluster manager is the one who patches the egressIP object.
	// For the sake of unit testing egressip zone controller we need to patch egressIP object manually
	// There are tests in cluster-manager package covering the patch logic.
	status := []egressip.EgressIPStatusItem{
		{
			Node:     nodeName,
			EgressIP: egressIP,
		},
	}
	err := o.controller.eIPC.patchReplaceEgressIPStatus(egressIPName, status)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
}

func nadGVR() metav1.GroupVersionResource {
	return metav1.GroupVersionResource{
		Group:    "k8s.cni.cncf.io",
		Version:  "v1",
		Resource: "network-attachment-definitions",
	}
}
