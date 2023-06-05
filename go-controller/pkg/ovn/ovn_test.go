package ovn

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/onsi/ginkgo"
	"sync"

	mnpapi "github.com/k8snetworkplumbingwg/multi-networkpolicy/pkg/apis/k8s.cni.cncf.io/v1beta1"
	mnpfake "github.com/k8snetworkplumbingwg/multi-networkpolicy/pkg/client/clientset/versioned/fake"
	nettypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	"github.com/onsi/gomega"
	libovsdbclient "github.com/ovn-org/libovsdb/client"
	ovncnitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	egressfirewall "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1"
	egressfirewallfake "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/clientset/versioned/fake"
	egressip "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	egressipfake "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/clientset/versioned/fake"
	egressqos "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressqos/v1"
	egressqosfake "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressqos/v1/apis/clientset/versioned/fake"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	util "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"
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
	fakeClient   *util.OVNMasterClientset
	watcher      *factory.WatchFactory
	controller   *DefaultNetworkController
	stopChan     chan struct{}
	wg           *sync.WaitGroup
	asf          *addressset.FakeAddressSetFactory
	fakeRecorder *record.FakeRecorder
	nbClient     libovsdbclient.Client
	sbClient     libovsdbclient.Client
	dbSetup      libovsdbtest.TestSetup
	nbsbCleanup  *libovsdbtest.Cleanup
	egressQoSWg  *sync.WaitGroup
	egressSVCWg  *sync.WaitGroup

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

		secondaryControllers: map[string]secondaryControllerInfo{},
	}
}

func (o *FakeOVN) start(objects ...runtime.Object) {
	fexec := ovntest.NewFakeExec()
	err := util.SetExec(fexec)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())

	egressIPObjects := []runtime.Object{}
	egressFirewallObjects := []runtime.Object{}
	egressQoSObjects := []runtime.Object{}
	multiNetworkPolicyObjects := []runtime.Object{}
	v1Objects := []runtime.Object{}
	nads := []*nettypes.NetworkAttachmentDefinition{}
	for _, object := range objects {
		if _, isEgressIPObject := object.(*egressip.EgressIPList); isEgressIPObject {
			egressIPObjects = append(egressIPObjects, object)
		} else if _, isEgressFirewallObject := object.(*egressfirewall.EgressFirewallList); isEgressFirewallObject {
			egressFirewallObjects = append(egressFirewallObjects, object)
		} else if _, isEgressQoSObject := object.(*egressqos.EgressQoSList); isEgressQoSObject {
			egressQoSObjects = append(egressQoSObjects, object)
		} else if _, isMultiNetworkPolicyObject := object.(*mnpapi.MultiNetworkPolicyList); isMultiNetworkPolicyObject {
			multiNetworkPolicyObjects = append(multiNetworkPolicyObjects, object)
		} else if nadList, isNADObject := object.(*nettypes.NetworkAttachmentDefinitionList); isNADObject {
			for i := range nadList.Items {
				nads = append(nads, &nadList.Items[i])
			}
		} else {
			v1Objects = append(v1Objects, object)
		}
	}
	o.fakeClient = &util.OVNMasterClientset{
		KubeClient:               fake.NewSimpleClientset(v1Objects...),
		EgressIPClient:           egressipfake.NewSimpleClientset(egressIPObjects...),
		EgressFirewallClient:     egressfirewallfake.NewSimpleClientset(egressFirewallObjects...),
		EgressQoSClient:          egressqosfake.NewSimpleClientset(egressQoSObjects...),
		MultiNetworkPolicyClient: mnpfake.NewSimpleClientset(multiNetworkPolicyObjects...),
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
	o.wg.Wait()
	o.egressQoSWg.Wait()
	o.egressSVCWg.Wait()
	o.nbsbCleanup.Cleanup()
}

func (o *FakeOVN) init(nadList []*nettypes.NetworkAttachmentDefinition) {
	var err error
	o.watcher, err = factory.NewMasterWatchFactory(o.fakeClient)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	err = o.watcher.Start()
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	o.nbClient, o.sbClient, o.nbsbCleanup, err = libovsdbtest.NewNBSBTestHarness(o.dbSetup)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())

	o.stopChan = make(chan struct{})
	o.wg = &sync.WaitGroup{}
	o.controller, err = NewOvnController(o.fakeClient, o.watcher,
		o.stopChan, o.asf,
		o.nbClient, o.sbClient,
		o.fakeRecorder, o.wg)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	o.controller.multicastSupport = true
	o.controller.clusterLoadBalancerGroupUUID = types.ClusterLBGroupName + "-UUID"
	o.controller.switchLoadBalancerGroupUUID = types.ClusterSwitchLBGroupName + "-UUID"
	o.controller.routerLoadBalancerGroupUUID = types.ClusterRouterLBGroupName + "-UUID"

	for _, nad := range nadList {
		err := o.NewSecondaryNetworkController(nad)
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
func NewOvnController(ovnClient *util.OVNMasterClientset, wf *factory.WatchFactory, stopChan chan struct{},
	addressSetFactory addressset.AddressSetFactory, libovsdbOvnNBClient libovsdbclient.Client,
	libovsdbOvnSBClient libovsdbclient.Client, recorder record.EventRecorder, wg *sync.WaitGroup) (*DefaultNetworkController, error) {

	fakeAddr, ok := addressSetFactory.(*addressset.FakeAddressSetFactory)
	if addressSetFactory == nil || (ok && fakeAddr == nil) {
		addressSetFactory = addressset.NewOvnAddressSetFactory(libovsdbOvnNBClient, config.IPv4Mode, config.IPv6Mode)
	}

	podRecorder := metrics.NewPodRecorder()
	cnci, err := NewCommonNetworkControllerInfo(
		ovnClient.KubeClient,
		&kube.KubeOVN{
			Kube:                 kube.Kube{KClient: ovnClient.KubeClient},
			EIPClient:            ovnClient.EgressIPClient,
			EgressFirewallClient: ovnClient.EgressFirewallClient,
			CloudNetworkClient:   ovnClient.CloudNetworkClient,
		},
		wf,
		recorder,
		libovsdbOvnNBClient,
		libovsdbOvnSBClient,
		&podRecorder,
		false, // sctp support
		false, // multicast support
		true,  // templates support
		true,  // acl logging enabled
	)
	if err != nil {
		return nil, err
	}

	return newDefaultNetworkControllerCommon(cnci, stopChan, wg, addressSetFactory)
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
	nInfo, netConfInfo, err := util.ParseNADInfo(netattachdef)
	if err != nil {
		return err
	}
	netName := nInfo.GetNetworkName()
	topoType := netConfInfo.TopologyType()
	ocInfo, ok = o.secondaryControllers[netName]
	if !ok {
		podRecorder := metrics.NewPodRecorder()
		cnci, err := NewCommonNetworkControllerInfo(
			o.fakeClient.KubeClient,
			&kube.KubeOVN{
				Kube:                 kube.Kube{KClient: o.fakeClient.KubeClient},
				EIPClient:            o.fakeClient.EgressIPClient,
				EgressFirewallClient: o.fakeClient.EgressFirewallClient,
				CloudNetworkClient:   o.fakeClient.CloudNetworkClient,
			},
			o.watcher,
			o.fakeRecorder,
			o.nbClient,
			o.sbClient,
			&podRecorder,
			false, // sctp support
			false, // multicast support
			true,  // templates support
			true,  // acl logging enabled
		)
		if err != nil {
			return err
		}
		asf := addressset.NewFakeAddressSetFactory(netName + "-network-controller")

		switch topoType {
		case types.Layer3Topology:
			l3Controller := NewSecondaryLayer3NetworkController(cnci, nInfo, netConfInfo, asf)
			secondaryController = &l3Controller.BaseSecondaryNetworkController
		case types.Layer2Topology:
			l2Controller := NewSecondaryLayer2NetworkController(cnci, nInfo, netConfInfo, asf)
			secondaryController = &l2Controller.BaseSecondaryNetworkController
		case types.LocalnetTopology:
			localnetController := NewSecondaryLocalnetNetworkController(cnci, nInfo, netConfInfo, asf)
			secondaryController = &localnetController.BaseSecondaryNetworkController
		default:
			return fmt.Errorf("topoloty type %s not supported", topoType)
		}
		ocInfo = secondaryControllerInfo{bnc: secondaryController, asf: asf}
		o.secondaryControllers[netName] = ocInfo
	} else {
		secondaryController = ocInfo.bnc
	}

	ginkgo.By(fmt.Sprintf("OVN test init: add NAD %s to secondary network controller of %s network %s", nadName, topoType, netName))
	secondaryController.AddNAD(nadName)
	return nil
}
