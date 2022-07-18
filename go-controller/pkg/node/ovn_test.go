package node

import (
	"context"
	"sync"

	. "github.com/onsi/gomega"
	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	util "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/urfave/cli/v2"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"
)

var fakeNodeName = "node"

type FakeOVNNode struct {
	node       *OvnNode
	watcher    factory.NodeWatchFactory
	stopChan   chan struct{}
	recorder   *record.FakeRecorder
	fakeClient *util.OVNClientset
	fakeExec   *ovntest.FakeExec
	wg         *sync.WaitGroup
	sbClient   libovsdbclient.Client
}

func NewFakeOVNNode(fexec *ovntest.FakeExec, sbclient libovsdbclient.Client) *FakeOVNNode {
	err := util.SetExec(fexec)
	Expect(err).NotTo(HaveOccurred())

	return &FakeOVNNode{
		fakeExec: fexec,
		recorder: record.NewFakeRecorder(1),
		wg:       &sync.WaitGroup{},
		sbClient: sbclient,
	}
}

func (o *FakeOVNNode) start(ctx *cli.Context, objects ...runtime.Object) {
	v1Objects := []runtime.Object{}
	for _, object := range objects {
		v1Objects = append(v1Objects, object)
	}
	_, err := config.InitConfig(ctx, o.fakeExec, nil)
	Expect(err).NotTo(HaveOccurred())

	o.fakeClient = &util.OVNClientset{
		KubeClient: fake.NewSimpleClientset(v1Objects...),
	}
	o.init()
}

func (o *FakeOVNNode) restart() {
	o.shutdown()
	o.init()
}

func (o *FakeOVNNode) shutdown() {
	close(o.stopChan)
	o.wg.Wait()
}

func (o *FakeOVNNode) init() {
	var err error

	o.stopChan = make(chan struct{})

	o.watcher, err = factory.NewNodeWatchFactory(o.fakeClient, fakeNodeName)
	Expect(err).NotTo(HaveOccurred())

	o.node = NewNode(o.fakeClient.KubeClient, o.watcher, fakeNodeName, o.sbClient, o.stopChan, o.recorder)
	o.node.Start(context.TODO(), o.wg)
}
