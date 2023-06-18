package clustermanager

import (
	"net"

	"github.com/onsi/gomega"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/clustermanager/egressservice"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	egressip "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	egressipfake "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/clientset/versioned/fake"
	egresssvc "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressservice/v1"
	egresssvcfake "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressservice/v1/apis/clientset/versioned/fake"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/healthcheck"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"
)

type FakeClusterManager struct {
	fakeClient   *util.OVNClusterManagerClientset
	watcher      *factory.WatchFactory
	eIPC         *egressIPClusterController
	esvc         *egressservice.Controller
	fakeRecorder *record.FakeRecorder
}

var isReachable = func(nodeName string, mgmtIPs []net.IP, healthClient healthcheck.EgressIPHealthClient) bool {
	return true
}

func NewFakeClusterManagerOVN() *FakeClusterManager {
	return &FakeClusterManager{
		fakeRecorder: record.NewFakeRecorder(10),
	}
}

func (o *FakeClusterManager) start(objects ...runtime.Object) {
	egressIPObjects := []runtime.Object{}
	egressSvcObjects := []runtime.Object{}
	v1Objects := []runtime.Object{}
	for _, object := range objects {
		if _, isEgressIPObject := object.(*egressip.EgressIPList); isEgressIPObject {
			egressIPObjects = append(egressIPObjects, object)
		} else if _, isEgressSVCObj := object.(*egresssvc.EgressServiceList); isEgressSVCObj {
			egressSvcObjects = append(egressSvcObjects, object)
		} else {
			v1Objects = append(v1Objects, object)
		}
	}
	o.fakeClient = &util.OVNClusterManagerClientset{
		KubeClient:          fake.NewSimpleClientset(v1Objects...),
		EgressIPClient:      egressipfake.NewSimpleClientset(egressIPObjects...),
		EgressServiceClient: egresssvcfake.NewSimpleClientset(egressSvcObjects...),
	}
	o.init()
}

func (o *FakeClusterManager) init() {
	var err error
	o.watcher, err = factory.NewClusterManagerWatchFactory(o.fakeClient)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	err = o.watcher.Start()
	gomega.Expect(err).NotTo(gomega.HaveOccurred())

	if config.OVNKubernetesFeature.EnableEgressIP {
		o.eIPC = newEgressIPController(o.fakeClient, o.watcher, o.fakeRecorder)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	}
	if config.OVNKubernetesFeature.EnableEgressService {
		o.esvc, err = egressservice.NewController(o.fakeClient, o.watcher, isReachable)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		err = o.esvc.Start(1)
		gomega.Expect(err).ToNot(gomega.HaveOccurred())
	}
}

func (o *FakeClusterManager) shutdown() {
	o.watcher.Shutdown()
	if config.OVNKubernetesFeature.EnableEgressIP {
		o.eIPC.Stop()
	}
	if config.OVNKubernetesFeature.EnableEgressService {
		o.esvc.Stop()
	}
}
