package ovn

import (
	"sync"

	"github.com/onsi/gomega"
	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	util "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	"github.com/urfave/cli/v2"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"

	egressfirewall "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1"
	egressfirewallfake "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/clientset/versioned/fake"
	egressip "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	egressipfake "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/clientset/versioned/fake"
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
	fakeClient   *util.OVNClientset
	watcher      *factory.WatchFactory
	controller   *Controller
	stopChan     chan struct{}
	fakeExec     *ovntest.FakeExec
	asf          *addressset.FakeAddressSetFactory
	fakeRecorder *record.FakeRecorder
	nbClient     libovsdbclient.Client
	sbClient     libovsdbclient.Client
	dbSetup      libovsdbtest.TestSetup
	wg           *sync.WaitGroup
	nbsbCleanup  *libovsdbtest.Cleanup
}

func NewFakeOVN(fexec *ovntest.FakeExec) *FakeOVN {
	err := util.SetExec(fexec)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	return &FakeOVN{
		fakeExec:     fexec,
		asf:          addressset.NewFakeAddressSetFactory(),
		fakeRecorder: record.NewFakeRecorder(10),
		wg:           &sync.WaitGroup{},
	}
}

func (o *FakeOVN) start(ctx *cli.Context, objects ...runtime.Object) {
	egressIPObjects := []runtime.Object{}
	egressFirewallObjects := []runtime.Object{}
	v1Objects := []runtime.Object{}
	for _, object := range objects {
		if _, isEgressIPObject := object.(*egressip.EgressIPList); isEgressIPObject {
			egressIPObjects = append(egressIPObjects, object)
		} else if _, isEgressFirewallObject := object.(*egressfirewall.EgressFirewallList); isEgressFirewallObject {
			egressFirewallObjects = append(egressFirewallObjects, object)
		} else {
			v1Objects = append(v1Objects, object)
		}
	}
	_, err := config.InitConfig(ctx, o.fakeExec, nil)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	o.fakeClient = &util.OVNClientset{
		KubeClient:           fake.NewSimpleClientset(v1Objects...),
		EgressIPClient:       egressipfake.NewSimpleClientset(egressIPObjects...),
		EgressFirewallClient: egressfirewallfake.NewSimpleClientset(egressFirewallObjects...),
	}
	o.init()
}

func (o *FakeOVN) startWithDBSetup(ctx *cli.Context, dbSetup libovsdbtest.TestSetup, objects ...runtime.Object) {
	o.dbSetup = dbSetup
	o.start(ctx, objects...)
}

func (o *FakeOVN) shutdown() {
	o.watcher.Shutdown()
	close(o.stopChan)
	o.nbsbCleanup.Cleanup()
	o.wg.Wait()
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
	o.controller = NewOvnController(o.fakeClient, o.watcher,
		o.stopChan, o.asf,
		o.nbClient, o.sbClient,
		o.fakeRecorder)
	o.controller.multicastSupport = true
}
