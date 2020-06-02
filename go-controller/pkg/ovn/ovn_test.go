package ovn

import (
	. "github.com/onsi/gomega"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	util "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/urfave/cli/v2"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

const (
	k8sTCPLoadBalancerIP  = "k8s_tcp_load_balancer"
	k8sUDPLoadBalancerIP  = "k8s_udp_load_balancer"
	k8sSCTPLoadBalancerIP = "k8s_sctp_load_balancer"
	fakeUUID              = "8a86f6d8-7972-4253-b0bd-ddbef66e9303"
)

type FakeOVN struct {
	fakeClient *fake.Clientset
	watcher    *factory.WatchFactory
	controller *Controller
	stopChan   chan struct{}
	fakeExec   *ovntest.FakeExec
	asf        *fakeAddressSetFactory
}

func NewFakeOVN(fexec *ovntest.FakeExec) *FakeOVN {
	err := util.SetExec(fexec)
	Expect(err).NotTo(HaveOccurred())

	return &FakeOVN{
		fakeExec: fexec,
		asf:      newFakeAddressSetFactory(),
	}
}

func (o *FakeOVN) start(ctx *cli.Context, objects ...runtime.Object) {
	_, err := config.InitConfig(ctx, o.fakeExec, nil)
	Expect(err).NotTo(HaveOccurred())

	o.fakeClient = fake.NewSimpleClientset(objects...)
	o.init()
}

func (o *FakeOVN) restart() {
	o.shutdown()
	o.init()
}

func (o *FakeOVN) shutdown() {
	close(o.stopChan)
}

func (o *FakeOVN) init() {
	var err error

	o.stopChan = make(chan struct{})
	o.watcher, err = factory.NewWatchFactory(o.fakeClient, o.stopChan)
	Expect(err).NotTo(HaveOccurred())

	o.controller = NewOvnController(o.fakeClient, o.watcher, o.stopChan, o.asf)
	o.controller.multicastSupport = true
}
