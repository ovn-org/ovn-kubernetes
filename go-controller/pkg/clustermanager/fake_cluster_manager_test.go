package clustermanager

import (
	"github.com/onsi/gomega"
	egressip "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	egressipfake "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/clientset/versioned/fake"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"
)

type FakeClusterManager struct {
	fakeClient   *util.OVNClusterManagerClientset
	watcher      *factory.WatchFactory
	eIPC         *egressIPClusterController
	fakeRecorder *record.FakeRecorder
}

func NewFakeClusterManagerOVN() *FakeClusterManager {
	return &FakeClusterManager{
		fakeRecorder: record.NewFakeRecorder(10),
	}
}

func (o *FakeClusterManager) start(objects ...runtime.Object) {
	egressIPObjects := []runtime.Object{}
	v1Objects := []runtime.Object{}
	for _, object := range objects {
		if _, isEgressIPObject := object.(*egressip.EgressIPList); isEgressIPObject {
			egressIPObjects = append(egressIPObjects, object)
		} else {
			v1Objects = append(v1Objects, object)
		}
	}
	o.fakeClient = &util.OVNClusterManagerClientset{
		KubeClient:     fake.NewSimpleClientset(v1Objects...),
		EgressIPClient: egressipfake.NewSimpleClientset(egressIPObjects...),
	}
	o.init()
}

func (o *FakeClusterManager) init() {
	var err error
	o.watcher, err = factory.NewClusterManagerWatchFactory(o.fakeClient)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	err = o.watcher.Start()
	gomega.Expect(err).NotTo(gomega.HaveOccurred())

	o.eIPC = newEgressIPController(o.fakeClient, o.watcher, o.fakeRecorder)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
}

func (o *FakeClusterManager) shutdown() {
	o.watcher.Shutdown()
	o.eIPC.Stop()
}
