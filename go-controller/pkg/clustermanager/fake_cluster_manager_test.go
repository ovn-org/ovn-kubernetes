package clustermanager

import (
	"net"

	"github.com/onsi/gomega"
	ocpcloudnetworkapi "github.com/openshift/api/cloudnetwork/v1"
	ocpnetworkapiv1alpha1 "github.com/openshift/api/network/v1alpha1"
	cloudservicefake "github.com/openshift/client-go/cloudnetwork/clientset/versioned/fake"
	ocpnetworkclientfake "github.com/openshift/client-go/network/clientset/versioned/fake"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/clustermanager/dnsnameresolver"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/clustermanager/egressservice"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	egressfirewallapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1"
	egressfirewallfake "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/clientset/versioned/fake"
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
	dnrC         *dnsnameresolver.Controller
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
	cloudObjects := []runtime.Object{}
	egressFirewallObjects := []runtime.Object{}
	dnsNameResolverObjects := []runtime.Object{}
	for _, object := range objects {
		if _, isEgressIPObject := object.(*egressip.EgressIPList); isEgressIPObject {
			egressIPObjects = append(egressIPObjects, object)
		} else if _, isEgressSVCObj := object.(*egresssvc.EgressServiceList); isEgressSVCObj {
			egressSvcObjects = append(egressSvcObjects, object)
		} else if _, isCloudPrivateIPConfig := object.(*ocpcloudnetworkapi.CloudPrivateIPConfigList); isCloudPrivateIPConfig {
			cloudObjects = append(cloudObjects, object)
		} else if _, isEgressFirewallObject := object.(*egressfirewallapi.EgressFirewall); isEgressFirewallObject {
			egressFirewallObjects = append(egressFirewallObjects, object)
		} else if _, isDNSNameResolverObject := object.(*ocpnetworkapiv1alpha1.DNSNameResolver); isDNSNameResolverObject {
			dnsNameResolverObjects = append(dnsNameResolverObjects, object)
		} else {
			v1Objects = append(v1Objects, object)
		}
	}
	o.fakeClient = &util.OVNClusterManagerClientset{
		KubeClient:           fake.NewSimpleClientset(v1Objects...),
		EgressIPClient:       egressipfake.NewSimpleClientset(egressIPObjects...),
		EgressServiceClient:  egresssvcfake.NewSimpleClientset(egressSvcObjects...),
		CloudNetworkClient:   cloudservicefake.NewSimpleClientset(cloudObjects...),
		EgressFirewallClient: egressfirewallfake.NewSimpleClientset(egressFirewallObjects...),
		NetworkClient:        ocpnetworkclientfake.NewSimpleClientset(dnsNameResolverObjects...),
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
	if config.OVNKubernetesFeature.EnableEgressFirewall && config.OVNKubernetesFeature.EnableDNSNameResolver {
		o.dnrC = dnsnameresolver.NewController(o.fakeClient, o.watcher)

		err = o.dnrC.Start()
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
	if config.OVNKubernetesFeature.EnableEgressFirewall && config.OVNKubernetesFeature.EnableDNSNameResolver {
		o.dnrC.Stop()
	}
}
