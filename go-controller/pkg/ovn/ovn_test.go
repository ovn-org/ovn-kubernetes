package ovn

import (
	. "github.com/onsi/gomega"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	util "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/urfave/cli"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

const (
	k8sTCPLoadBalancerIP = "k8s_tcp_load_balancer"
	k8sUDPLoadBalancerIP = "k8s_udp_load_balancer"
)

type FakeOVN struct {
	fakeClient *fake.Clientset
	watcher    *factory.WatchFactory
	controller *Controller
	stopChan   chan struct{}
	fakeExec   *ovntest.FakeExec
	portGroups bool
}

func NewFakeOVN(fexec *ovntest.FakeExec, portGroupSupport bool) *FakeOVN {
	err := util.SetExec(fexec)
	Expect(err).NotTo(HaveOccurred())

	return &FakeOVN{
		fakeExec:   fexec,
		portGroups: portGroupSupport,
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

	o.controller = NewOvnController(o.fakeClient, o.watcher, nil)
	o.controller.portGroupSupport = o.portGroups
	// Multicast depends on port groups
	o.controller.multicastSupport = o.portGroups
}
