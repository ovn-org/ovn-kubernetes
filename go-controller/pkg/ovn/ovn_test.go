package ovn

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/onsi/ginkgo/v2"

	fakeipamclaimclient "github.com/k8snetworkplumbingwg/ipamclaims/pkg/crd/ipamclaims/v1alpha1/apis/clientset/versioned/fake"
	mnpapi "github.com/k8snetworkplumbingwg/multi-networkpolicy/pkg/apis/k8s.cni.cncf.io/v1beta1"
	mnpfake "github.com/k8snetworkplumbingwg/multi-networkpolicy/pkg/client/clientset/versioned/fake"
	nettypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	fakenadclient "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/fake"
	"github.com/onsi/gomega"
	ocpnetworkapiv1alpha1 "github.com/openshift/api/network/v1alpha1"
	ocpnetworkfake "github.com/openshift/client-go/network/clientset/versioned/fake"
	libovsdbclient "github.com/ovn-org/libovsdb/client"

	ovncnitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	adminpolicybasedrouteapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1"
	adminpolicybasedroutefake "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1/apis/clientset/versioned/fake"
	egressfirewall "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1"
	egressfirewallfake "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/clientset/versioned/fake"
	egressip "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	egressipfake "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/clientset/versioned/fake"
	egressqos "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressqos/v1"
	egressqosfake "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressqos/v1/apis/clientset/versioned/fake"
	egressservice "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressservice/v1"
	egressservicefake "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressservice/v1/apis/clientset/versioned/fake"
	udnclientfake "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/clientset/versioned/fake"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/networkmanager"
	testnm "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/networkmanager"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"
	anpapi "sigs.k8s.io/network-policy-api/apis/v1alpha1"
	anpfake "sigs.k8s.io/network-policy-api/pkg/client/clientset/versioned/fake"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	libovsdbutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	util "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
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
)

type secondaryControllerInfo struct {
	bnc *BaseSecondaryNetworkController
	asf *addressset.FakeAddressSetFactory
}

type FakeOVN struct {
	fakeClient     *util.OVNMasterClientset
	watcher        *factory.WatchFactory
	controller     *DefaultNetworkController
	stopChan       chan struct{}
	wg             *sync.WaitGroup
	asf            *addressset.FakeAddressSetFactory
	fakeRecorder   *record.FakeRecorder
	nbClient       libovsdbclient.Client
	sbClient       libovsdbclient.Client
	dbSetup        libovsdbtest.TestSetup
	nbsbCleanup    *libovsdbtest.Context
	egressQoSWg    *sync.WaitGroup
	egressSVCWg    *sync.WaitGroup
	anpWg          *sync.WaitGroup
	networkManager networkmanager.Controller
	eIPController  *EgressIPController
	portCache      *PortCache

	// information map of all secondary network controllers
	secondaryControllers map[string]secondaryControllerInfo
}

// NOTE: the FakeAddressSetFactory is no longer needed and should no longer be used. starting to phase out FakeAddressSetFactory
func NewFakeOVN(useFakeAddressSet bool) *FakeOVN {
	var asf *addressset.FakeAddressSetFactory
	if useFakeAddressSet {
		asf = addressset.NewFakeAddressSetFactory(DefaultNetworkControllerName)
	}
	return &FakeOVN{
		asf:          asf,
		fakeRecorder: record.NewFakeRecorder(10),
		egressQoSWg:  &sync.WaitGroup{},
		egressSVCWg:  &sync.WaitGroup{},
		anpWg:        &sync.WaitGroup{},

		secondaryControllers: map[string]secondaryControllerInfo{},
	}
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
			// https://github.com/ovn-org/ovn-kubernetes/blob/65c79af35b2c22f90c863debefa15c4fb1f088cb/go-controller/vendor/k8s.io/client-go/testing/fixture.go#L341
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
		default:
			v1Objects = append(v1Objects, object)
		}
	}
	o.fakeClient = &util.OVNMasterClientset{
		KubeClient:               fake.NewSimpleClientset(v1Objects...),
		ANPClient:                anpfake.NewSimpleClientset(anpObjects...),
		EgressIPClient:           egressipfake.NewSimpleClientset(egressIPObjects...),
		EgressFirewallClient:     egressfirewallfake.NewSimpleClientset(egressFirewallObjects...),
		OCPNetworkClient:         ocpnetworkfake.NewSimpleClientset(dnsNameResolverObjects...),
		EgressQoSClient:          egressqosfake.NewSimpleClientset(egressQoSObjects...),
		MultiNetworkPolicyClient: mnpfake.NewSimpleClientset(multiNetworkPolicyObjects...),
		EgressServiceClient:      egressservicefake.NewSimpleClientset(egressServiceObjects...),
		AdminPolicyRouteClient:   adminpolicybasedroutefake.NewSimpleClientset(apbExternalRouteObjects...),
		IPAMClaimsClient:         fakeipamclaimclient.NewSimpleClientset(),
		NetworkAttchDefClient:    nadClient,
		UserDefinedNetworkClient: udnclientfake.NewSimpleClientset(),
	}
	o.init(nads)
}

func (o *FakeOVN) startWithDBSetup(dbSetup libovsdbtest.TestSetup, objects ...runtime.Object) {
	o.dbSetup = dbSetup
	o.start(objects...)
}

func (o *FakeOVN) shutdown() {
	o.watcher.Shutdown()
	close(o.stopChan)
	o.controller.cancelableCtx.Cancel()
	o.wg.Wait()
	o.egressQoSWg.Wait()
	o.egressSVCWg.Wait()
	o.anpWg.Wait()
	o.nbsbCleanup.Cleanup()
	for _, ocInfo := range o.secondaryControllers {
		close(ocInfo.bnc.stopChan)
		ocInfo.bnc.cancelableCtx.Cancel()
		ocInfo.bnc.wg.Wait()
	}
}

func (o *FakeOVN) init(nadList []nettypes.NetworkAttachmentDefinition) {
	var err error
	o.watcher, err = factory.NewMasterWatchFactory(o.fakeClient)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())

	o.nbClient, o.sbClient, o.nbsbCleanup, err = libovsdbtest.NewNBSBTestHarness(o.dbSetup)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())

	o.stopChan = make(chan struct{})
	o.wg = &sync.WaitGroup{}

	o.networkManager = networkmanager.Default()
	if config.OVNKubernetesFeature.EnableMultiNetwork {
		o.networkManager, err = networkmanager.NewForZone("test", &testnm.FakeControllerManager{}, o.watcher)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	}

	o.portCache = NewPortCache(o.stopChan)
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
		config.IPv4Mode,
		config.IPv6Mode,
		"",
		DefaultNetworkControllerName,
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
	)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	o.controller.multicastSupport = config.EnableMulticast
	o.eIPController.zone = o.controller.zone

	setupCOPP := false
	setupClusterController(o.controller, setupCOPP)
	for _, n := range nadList {
		err := o.NewSecondaryNetworkController(&n)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	}

	err = o.watcher.Start()
	gomega.Expect(err).NotTo(gomega.HaveOccurred())

	err = o.eIPController.SyncLocalNodeZonesCache()
	gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "syncing Nodes OVN zones status must succeed to support EgressIP")

	existingNodes, err := o.controller.kube.GetNodes()
	if err == nil {
		for _, node := range existingNodes {
			o.controller.localZoneNodes.Store(node.Name, true)
			for _, secondaryController := range o.secondaryControllers {
				if secondaryController.bnc.localZoneNodes != nil {
					secondaryController.bnc.localZoneNodes.Store(node.Name, true)
				}
			}
		}
	}

}

func setupClusterController(clusterController *DefaultNetworkController, setupCOPP bool) {
	var err error
	clusterController.SCTPSupport = true

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
	ovnClient *util.OVNMasterClientset,
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
) (*DefaultNetworkController, error) {

	fakeAddr, ok := addressSetFactory.(*addressset.FakeAddressSetFactory)
	if addressSetFactory == nil || (ok && fakeAddr == nil) {
		addressSetFactory = addressset.NewOvnAddressSetFactory(libovsdbOvnNBClient, config.IPv4Mode, config.IPv6Mode)
	}

	podRecorder := metrics.NewPodRecorder()

	nbZoneFailed := false
	// Try to get the NBZone.  If there is an error, create NB_Global record.
	// Otherwise NewCommonNetworkControllerInfo() will return error since it
	// calls libovsdbutil.GetNBZone().
	_, err := libovsdbutil.GetNBZone(libovsdbOvnNBClient)
	if err != nil {
		nbZoneFailed = true
		err = createTestNBGlobal(libovsdbOvnNBClient, "global")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	}
	cnci, err := NewCommonNetworkControllerInfo(
		ovnClient.KubeClient,
		&kube.KubeOVN{
			Kube:                 kube.Kube{KClient: ovnClient.KubeClient},
			ANPClient:            ovnClient.ANPClient,
			EIPClient:            ovnClient.EgressIPClient,
			EgressFirewallClient: ovnClient.EgressFirewallClient,
			EgressServiceClient:  ovnClient.EgressServiceClient,
			APBRouteClient:       ovnClient.AdminPolicyRouteClient,
			EgressQoSClient:      ovnClient.EgressQoSClient,
		},
		wf,
		recorder,
		libovsdbOvnNBClient,
		libovsdbOvnSBClient,
		&podRecorder,
		false, // sctp support
		false, // multicast support
		true,  // templates support
	)
	if err != nil {
		return nil, err
	}

	dnc, err := newDefaultNetworkControllerCommon(cnci, stopChan, wg, addressSetFactory, networkManager, nil, eIPController, portCache)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())

	if nbZoneFailed {
		// Delete the NBGlobal row as this function created it.  Otherwise many tests would fail while
		// checking the expectedData in the NBDB.
		err = deleteTestNBGlobal(libovsdbOvnNBClient)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	}

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

func deleteTestNBGlobal(nbClient libovsdbclient.Client) error {
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

func newNetworkAttachmentDefinition(namespace, name string, netconf ovncnitypes.NetConf) (*nettypes.NetworkAttachmentDefinition, error) {
	bytes, err := json.Marshal(netconf)
	if err != nil {
		return nil, fmt.Errorf("failed marshaling podNetworks map %v", netconf)
	}
	return &nettypes.NetworkAttachmentDefinition{
		ObjectMeta: newObjectMeta(name, namespace),
		Spec: nettypes.NetworkAttachmentDefinitionSpec{
			Config: string(bytes),
		},
	}, nil
}

func (o *FakeOVN) NewSecondaryNetworkController(netattachdef *nettypes.NetworkAttachmentDefinition) error {
	var ocInfo secondaryControllerInfo
	var secondaryController *BaseSecondaryNetworkController
	var ok bool

	nadName := util.GetNADName(netattachdef.Namespace, netattachdef.Name)
	nInfo, err := util.ParseNADInfo(netattachdef)
	if err != nil {
		return err
	}
	netName := nInfo.GetNetworkName()
	topoType := nInfo.TopologyType()
	ocInfo, ok = o.secondaryControllers[netName]
	if !ok {
		nbZoneFailed := false
		// Try to get the NBZone.  If there is an error, create NB_Global record.
		// Otherwise NewCommonNetworkControllerInfo() will return error since it
		// calls libovsdbutil.GetNBZone().
		_, err := libovsdbutil.GetNBZone(o.nbClient)
		if err != nil {
			nbZoneFailed = true
			err = createTestNBGlobal(o.nbClient, "global")
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}

		podRecorder := metrics.NewPodRecorder()
		cnci, err := NewCommonNetworkControllerInfo(
			o.fakeClient.KubeClient,
			&kube.KubeOVN{
				Kube:                 kube.Kube{KClient: o.fakeClient.KubeClient},
				EIPClient:            o.fakeClient.EgressIPClient,
				EgressFirewallClient: o.fakeClient.EgressFirewallClient,
			},
			o.watcher,
			o.fakeRecorder,
			o.nbClient,
			o.sbClient,
			&podRecorder,
			false, // sctp support
			false, // multicast support
			true,  // templates support
		)
		if err != nil {
			return err
		}

		asf := addressset.NewFakeAddressSetFactory(getNetworkControllerName(netName))

		switch topoType {
		case types.Layer3Topology:
			l3Controller, err := NewSecondaryLayer3NetworkController(cnci, nInfo, o.networkManager.Interface(), o.eIPController, o.portCache)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			if o.asf != nil { // use fake asf only when enabled
				l3Controller.addressSetFactory = asf
			}
			secondaryController = &l3Controller.BaseSecondaryNetworkController
		case types.Layer2Topology:
			l2Controller, err := NewSecondaryLayer2NetworkController(cnci, nInfo, o.networkManager.Interface(), o.eIPController, o.portCache)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			if o.asf != nil { // use fake asf only when enabled
				l2Controller.addressSetFactory = asf
			}
			secondaryController = &l2Controller.BaseSecondaryNetworkController
		case types.LocalnetTopology:
			localnetController := NewSecondaryLocalnetNetworkController(cnci, nInfo, o.networkManager.Interface())
			if o.asf != nil { // use fake asf only when enabled
				localnetController.addressSetFactory = asf
			}
			secondaryController = &localnetController.BaseSecondaryNetworkController
		default:
			return fmt.Errorf("topology type %s not supported", topoType)
		}
		ocInfo = secondaryControllerInfo{bnc: secondaryController, asf: asf}
		o.secondaryControllers[netName] = ocInfo

		if nbZoneFailed {
			// Delete the NBGlobal row as this function created it.  Otherwise many tests would fail while
			// checking the expectedData in the NBDB.
			err = deleteTestNBGlobal(o.nbClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}
	} else {
		secondaryController = ocInfo.bnc
	}

	ginkgo.By(fmt.Sprintf("OVN test init: add NAD %s to secondary network controller of %s network %s", nadName, topoType, netName))
	mutableNetInfo := util.NewMutableNetInfo(secondaryController.GetNetInfo())
	mutableNetInfo.AddNADs(nadName)
	_ = util.ReconcileNetInfo(secondaryController.ReconcilableNetInfo, mutableNetInfo)
	return nil
}

func (o *FakeOVN) patchEgressIPObj(nodeName, egressIPName, egressIP, network string) {
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
