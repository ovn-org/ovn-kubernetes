package ovn

import (
	"context"
	"sync"

	"github.com/onsi/gomega"
	libovsdbclient "github.com/ovn-org/libovsdb/client"
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
}

func NewFakeOVN() *FakeOVN {
	return &FakeOVN{
		asf:          addressset.NewFakeAddressSetFactory(),
		fakeRecorder: record.NewFakeRecorder(10),
		egressQoSWg:  &sync.WaitGroup{},
		egressSVCWg:  &sync.WaitGroup{},
	}
}

func (o *FakeOVN) start(objects ...runtime.Object) {
	fexec := ovntest.NewFakeExec()
	err := util.SetExec(fexec)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())

	egressIPObjects := []runtime.Object{}
	egressFirewallObjects := []runtime.Object{}
	egressQoSObjects := []runtime.Object{}
	v1Objects := []runtime.Object{}
	for _, object := range objects {
		if _, isEgressIPObject := object.(*egressip.EgressIPList); isEgressIPObject {
			egressIPObjects = append(egressIPObjects, object)
		} else if _, isEgressFirewallObject := object.(*egressfirewall.EgressFirewallList); isEgressFirewallObject {
			egressFirewallObjects = append(egressFirewallObjects, object)
		} else if _, isEgressQoSObject := object.(*egressqos.EgressQoSList); isEgressQoSObject {
			egressQoSObjects = append(egressQoSObjects, object)
		} else {
			v1Objects = append(v1Objects, object)
		}
	}
	o.fakeClient = &util.OVNMasterClientset{
		KubeClient:           fake.NewSimpleClientset(v1Objects...),
		EgressIPClient:       egressipfake.NewSimpleClientset(egressIPObjects...),
		EgressFirewallClient: egressfirewallfake.NewSimpleClientset(egressFirewallObjects...),
		EgressQoSClient:      egressqosfake.NewSimpleClientset(egressQoSObjects...),
	}
	o.init()
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

func (o *FakeOVN) init() {
	var err error
	o.watcher, err = factory.NewMasterWatchFactory(o.fakeClient)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	err = o.watcher.Start()
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	o.nbClient, o.sbClient, o.nbsbCleanup, err = libovsdbtest.NewNBSBTestHarness(o.dbSetup)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())

	o.stopChan = make(chan struct{})
	o.wg = &sync.WaitGroup{}
	o.controller = NewOvnController(o.fakeClient, o.watcher,
		o.stopChan, o.asf,
		o.nbClient, o.sbClient,
		o.fakeRecorder, o.wg)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	o.controller.multicastSupport = true
	o.controller.loadBalancerGroupUUID = types.ClusterLBGroupName + "-UUID"
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
	libovsdbOvnSBClient libovsdbclient.Client, recorder record.EventRecorder, wg *sync.WaitGroup) *DefaultNetworkController {
	if addressSetFactory == nil {
		addressSetFactory = addressset.NewOvnAddressSetFactory(libovsdbOvnNBClient)
	}

	podRecorder := metrics.NewPodRecorder()
	cnci := NewCommonNetworkControllerInfo(
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
		false,
		false,
	)

	return newDefaultNetworkControllerCommon(cnci, stopChan, wg, addressSetFactory)
}
