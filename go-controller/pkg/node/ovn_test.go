package node

import (
	"context"
	"sync"

	nadfake "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/fake"
	. "github.com/onsi/gomega"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	adminpolicybasedrouteclient "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1/apis/clientset/versioned/fake"
	egressserviceapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressservice/v1"
	egressservicefake "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressservice/v1/apis/clientset/versioned/fake"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/routemanager"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	util "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/urfave/cli/v2"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"
)

var fakeNodeName = "node"

type FakeOVNNode struct {
	nc         *DefaultNodeNetworkController
	watcher    factory.NodeWatchFactory
	stopChan   chan struct{}
	recorder   *record.FakeRecorder
	fakeClient *util.OVNNodeClientset
	fakeExec   *ovntest.FakeExec
	wg         *sync.WaitGroup
}

func NewFakeOVNNode(fexec *ovntest.FakeExec) *FakeOVNNode {
	err := util.SetExec(fexec)
	Expect(err).NotTo(HaveOccurred())

	return &FakeOVNNode{
		fakeExec: fexec,
		recorder: record.NewFakeRecorder(1),
	}
}

func (o *FakeOVNNode) start(ctx *cli.Context, objects ...runtime.Object) {
	egressServiceObjects := []runtime.Object{}
	v1Objects := []runtime.Object{}
	for _, object := range objects {
		if _, isEgressServiceObject := object.(*egressserviceapi.EgressServiceList); isEgressServiceObject {
			egressServiceObjects = append(egressServiceObjects, object)
		} else {
			v1Objects = append(v1Objects, object)
		}
	}

	_, err := config.InitConfig(ctx, o.fakeExec, nil)
	Expect(err).NotTo(HaveOccurred())

	o.fakeClient = &util.OVNNodeClientset{
		KubeClient:             fake.NewSimpleClientset(v1Objects...),
		EgressServiceClient:    egressservicefake.NewSimpleClientset(egressServiceObjects...),
		AdminPolicyRouteClient: adminpolicybasedrouteclient.NewSimpleClientset(),
		NetworkAttchDefClient:  nadfake.NewSimpleClientset(),
	}
	o.init() // initializes the node
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
	o.wg = &sync.WaitGroup{}

	o.watcher, err = factory.NewNodeWatchFactory(o.fakeClient, fakeNodeName)
	Expect(err).NotTo(HaveOccurred())

	cnnci := NewCommonNodeNetworkControllerInfo(o.fakeClient.KubeClient, o.fakeClient.AdminPolicyRouteClient, o.watcher, o.recorder, fakeNodeName, routemanager.NewController())
	o.nc = newDefaultNodeNetworkController(cnnci, o.stopChan, o.wg, routemanager.NewController())
	// watcher is started by nodeControllerManager, not by nodeNetworkController, so start it here.
	o.watcher.Start()
	o.nc.PreStart(context.TODO())
	o.nc.Start(context.TODO())
}
