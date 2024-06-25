package clustermanager

import (
	"sync"

	"github.com/onsi/gomega"
	ocpcloudnetworkapi "github.com/openshift/api/cloudnetwork/v1"
	cloudservicefake "github.com/openshift/client-go/cloudnetwork/clientset/versioned/fake"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/clustermanager/egressservice"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/clustermanager/healthcheck"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	egressip "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	egressipfake "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/clientset/versioned/fake"
	egresssvc "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressservice/v1"
	egresssvcfake "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressservice/v1/apis/clientset/versioned/fake"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"
)

type FakeClusterManager struct {
	fakeClient              *util.OVNClusterManagerClientset
	watcher                 *factory.WatchFactory
	eIPC                    *egressIPClusterController
	esvc                    *egressservice.Controller
	fakeRecorder            *record.FakeRecorder
	FakeHealthCheckProvider *FakeHealthCheckProvider
}

func NewFakeClusterManagerOVN() *FakeClusterManager {
	return &FakeClusterManager{
		fakeRecorder: record.NewFakeRecorder(10),
		FakeHealthCheckProvider: &FakeHealthCheckProvider{
			reportHealthState: healthcheck.AVAILABLE,
		},
	}
}

func (o *FakeClusterManager) start(objects ...runtime.Object) {
	egressIPObjects := []runtime.Object{}
	egressSvcObjects := []runtime.Object{}
	v1Objects := []runtime.Object{}
	cloudObjects := []runtime.Object{}
	for _, object := range objects {
		if _, isEgressIPObject := object.(*egressip.EgressIPList); isEgressIPObject {
			egressIPObjects = append(egressIPObjects, object)
		} else if _, isEgressSVCObj := object.(*egresssvc.EgressServiceList); isEgressSVCObj {
			egressSvcObjects = append(egressSvcObjects, object)
		} else if _, isCloudPrivateIPConfig := object.(*ocpcloudnetworkapi.CloudPrivateIPConfigList); isCloudPrivateIPConfig {
			cloudObjects = append(cloudObjects, object)
		} else {
			v1Objects = append(v1Objects, object)
		}
	}
	o.fakeClient = &util.OVNClusterManagerClientset{
		KubeClient:          fake.NewSimpleClientset(v1Objects...),
		EgressIPClient:      egressipfake.NewSimpleClientset(egressIPObjects...),
		EgressServiceClient: egresssvcfake.NewSimpleClientset(egressSvcObjects...),
		CloudNetworkClient:  cloudservicefake.NewSimpleClientset(cloudObjects...),
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
		o.eIPC = newEgressIPController(o.fakeClient, o.watcher, o.FakeHealthCheckProvider, o.fakeRecorder)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	}
	if config.OVNKubernetesFeature.EnableEgressService {
		o.esvc, err = egressservice.NewController(o.fakeClient, o.watcher, o.FakeHealthCheckProvider)
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

type FakeHealthCheckProvider struct {
	sync.Mutex
	lastConsumer      healthcheck.Consumer
	reportHealthState healthcheck.HealthState
}

func (s *FakeHealthCheckProvider) Register(consumer healthcheck.Consumer) {
	s.Lock()
	defer s.Unlock()
	s.lastConsumer = consumer
}

func (s *FakeHealthCheckProvider) LastConsumer() healthcheck.Consumer {
	s.Lock()
	defer s.Unlock()
	return s.lastConsumer
}

func (s *FakeHealthCheckProvider) GetHealthState(node string) healthcheck.HealthState {
	s.Lock()
	defer s.Unlock()
	return s.reportHealthState
}

func (s *FakeHealthCheckProvider) ReportHealthState(state healthcheck.HealthState) {
	s.Lock()
	defer s.Unlock()
	s.reportHealthState = state
}

func (s *FakeHealthCheckProvider) MonitorHealthState(node string) {}
