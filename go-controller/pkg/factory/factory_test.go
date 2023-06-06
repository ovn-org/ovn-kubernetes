package factory

import (
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	v1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	knet "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	core "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/pointer"

	egressfirewall "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1"
	egressfirewallfake "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/clientset/versioned/fake"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	egressip "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	egressipfake "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/clientset/versioned/fake"

	egressqos "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressqos/v1"
	egressqosfake "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressqos/v1/apis/clientset/versioned/fake"

	egressservice "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressservice/v1"
	egressservicefake "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressservice/v1/apis/clientset/versioned/fake"

	ocpcloudnetworkapi "github.com/openshift/api/cloudnetwork/v1"
	ocpconfigapi "github.com/openshift/api/config/v1"
	ocpcloudnetworkclientsetfake "github.com/openshift/client-go/cloudnetwork/clientset/versioned/fake"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

func TestFactory(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Watch Factory Suite")
}

func newObjectMeta(name, namespace string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:      name,
		UID:       types.UID(name),
		Namespace: namespace,
		Labels: map[string]string{
			"name": name,
		},
	}
}

func newPod(name, namespace string) *v1.Pod {
	return &v1.Pod{
		Status: v1.PodStatus{
			Phase: v1.PodRunning,
		},
		ObjectMeta: newObjectMeta(name, namespace),
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:  "containerName",
					Image: "containerImage",
				},
			},
			NodeName: "mynode",
		},
	}
}

func newNamespace(name string) *v1.Namespace {
	return &v1.Namespace{
		Status: v1.NamespaceStatus{
			Phase: v1.NamespaceActive,
		},
		ObjectMeta: newObjectMeta(name, name),
	}
}

func newNode(name string) *v1.Node {
	return &v1.Node{
		Status: v1.NodeStatus{
			Phase: v1.NodeRunning,
		},
		ObjectMeta: newObjectMeta(name, ""),
	}
}

func newPolicy(name, namespace string) *knet.NetworkPolicy {
	return &knet.NetworkPolicy{
		ObjectMeta: newObjectMeta(name, namespace),
	}
}

func newService(name, namespace string) *v1.Service {
	return &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			UID:       types.UID(name),
			Namespace: namespace,
			Labels: map[string]string{
				"name": name,
			},
		},
	}
}

func newEndpointSlice(name, namespace, service string) *discovery.EndpointSlice {
	return &discovery.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			UID:       types.UID(name),
			Namespace: namespace,
			Labels: map[string]string{
				discovery.LabelServiceName: service,
			},
		},
	}
}

func newEgressFirewall(name, namespace string) *egressfirewall.EgressFirewall {
	return &egressfirewall.EgressFirewall{
		ObjectMeta: newObjectMeta(name, namespace),
		Spec: egressfirewall.EgressFirewallSpec{
			Egress: []egressfirewall.EgressFirewallRule{
				{
					Type: egressfirewall.EgressFirewallRuleAllow,
					To: egressfirewall.EgressFirewallDestination{
						CIDRSelector: "1.2.3.4/32",
					},
				},
			},
		},
	}
}

func newEgressIP(name, namespace string) *egressip.EgressIP {
	return &egressip.EgressIP{
		ObjectMeta: newObjectMeta(name, namespace),
		Spec: egressip.EgressIPSpec{
			EgressIPs: []string{
				"192.168.126.10",
			},
		},
	}

}

func newCloudPrivateIPConfig(name string) *ocpcloudnetworkapi.CloudPrivateIPConfig {
	return &ocpcloudnetworkapi.CloudPrivateIPConfig{
		ObjectMeta: newObjectMeta(name, ""),
		Spec: ocpcloudnetworkapi.CloudPrivateIPConfigSpec{
			Node: "test-node",
		},
	}
}

func newEgressQoS(name, namespace string) *egressqos.EgressQoS {
	return &egressqos.EgressQoS{
		ObjectMeta: newObjectMeta(name, namespace),
		Spec: egressqos.EgressQoSSpec{
			Egress: []egressqos.EgressQoSRule{
				{
					DSCP:    50,
					DstCIDR: pointer.String("1.2.3.4/32"),
				},
			},
		},
	}
}

func newEgressService(name, namespace string) *egressservice.EgressService {
	return &egressservice.EgressService{
		ObjectMeta: newObjectMeta(name, namespace),
		Spec: egressservice.EgressServiceSpec{
			NodeSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					"kubernetes.io/hostname": "node",
				},
			},
		},
	}
}

func objSetup(c *fake.Clientset, objType string, listFn func(core.Action) (bool, runtime.Object, error)) *watch.FakeWatcher {
	w := watch.NewFake()
	c.AddWatchReactor(objType, core.DefaultWatchReactor(w, nil))
	c.AddReactor("list", objType, listFn)
	return w
}

func egressFirewallObjSetup(c *egressfirewallfake.Clientset, objType string, listFn func(core.Action) (bool, runtime.Object, error)) *watch.FakeWatcher {
	w := watch.NewFake()
	c.AddWatchReactor(objType, core.DefaultWatchReactor(w, nil))
	c.AddReactor("list", objType, listFn)
	return w
}

func egressIPObjSetup(c *egressipfake.Clientset, objType string, listFn func(core.Action) (bool, runtime.Object, error)) *watch.FakeWatcher {
	w := watch.NewFake()
	c.AddWatchReactor(objType, core.DefaultWatchReactor(w, nil))
	c.AddReactor("list", objType, listFn)
	return w
}

func cloudPrivateIPConfigObjSetup(c *ocpcloudnetworkclientsetfake.Clientset, objType string, listFn func(core.Action) (bool, runtime.Object, error)) *watch.FakeWatcher {
	w := watch.NewFake()
	c.AddWatchReactor(objType, core.DefaultWatchReactor(w, nil))
	c.AddReactor("list", objType, listFn)
	return w
}

func egressQoSObjSetup(c *egressqosfake.Clientset, objType string, listFn func(core.Action) (bool, runtime.Object, error)) *watch.FakeWatcher {
	w := watch.NewFake()
	c.AddWatchReactor(objType, core.DefaultWatchReactor(w, nil))
	c.AddReactor("list", objType, listFn)
	return w
}

func egressServiceObjSetup(c *egressservicefake.Clientset, objType string, listFn func(core.Action) (bool, runtime.Object, error)) *watch.FakeWatcher {
	w := watch.NewFake()
	c.AddWatchReactor(objType, core.DefaultWatchReactor(w, nil))
	c.AddReactor("list", objType, listFn)
	return w
}

type handlerCalls struct {
	added   int32
	updated int32
	deleted int32
}

func (c *handlerCalls) getAdded() int {
	return int(atomic.LoadInt32(&c.added))
}

func (c *handlerCalls) getUpdated() int {
	return int(atomic.LoadInt32(&c.updated))
}

func (c *handlerCalls) getDeleted() int {
	return int(atomic.LoadInt32(&c.deleted))
}

var _ = Describe("Watch Factory Operations", func() {
	var (
		ovnClientset                        *util.OVNMasterClientset
		fakeClient                          *fake.Clientset
		egressIPFakeClient                  *egressipfake.Clientset
		egressFirewallFakeClient            *egressfirewallfake.Clientset
		cloudNetworkFakeClient              *ocpcloudnetworkclientsetfake.Clientset
		egressQoSFakeClient                 *egressqosfake.Clientset
		egressServiceFakeClient             *egressservicefake.Clientset
		podWatch, namespaceWatch, nodeWatch *watch.FakeWatcher
		policyWatch, serviceWatch           *watch.FakeWatcher
		endpointSliceWatch                  *watch.FakeWatcher
		egressFirewallWatch                 *watch.FakeWatcher
		egressIPWatch                       *watch.FakeWatcher
		cloudPrivateIPConfigWatch           *watch.FakeWatcher
		egressQoSWatch                      *watch.FakeWatcher
		egressServiceWatch                  *watch.FakeWatcher
		pods                                []*v1.Pod
		namespaces                          []*v1.Namespace
		nodes                               []*v1.Node
		policies                            []*knet.NetworkPolicy
		endpointSlices                      []*discovery.EndpointSlice
		services                            []*v1.Service
		egressIPs                           []*egressip.EgressIP
		cloudPrivateIPConfigs               []*ocpcloudnetworkapi.CloudPrivateIPConfig
		wf                                  *WatchFactory
		egressFirewalls                     []*egressfirewall.EgressFirewall
		egressQoSes                         []*egressqos.EgressQoS
		egressServices                      []*egressservice.EgressService
		err                                 error
	)

	const (
		nodeName string = "node1"
	)

	BeforeEach(func() {

		// Restore global default values before each testcase
		config.PrepareTestConfig()
		config.OVNKubernetesFeature.EnableEgressIP = true
		config.OVNKubernetesFeature.EnableEgressFirewall = true
		config.OVNKubernetesFeature.EnableEgressQoS = true
		config.OVNKubernetesFeature.EnableEgressService = true
		config.Kubernetes.PlatformType = string(ocpconfigapi.AWSPlatformType)

		fakeClient = &fake.Clientset{}
		egressFirewallFakeClient = &egressfirewallfake.Clientset{}
		egressIPFakeClient = &egressipfake.Clientset{}
		cloudNetworkFakeClient = &ocpcloudnetworkclientsetfake.Clientset{}
		egressQoSFakeClient = &egressqosfake.Clientset{}
		egressServiceFakeClient = &egressservicefake.Clientset{}

		ovnClientset = &util.OVNMasterClientset{
			KubeClient:           fakeClient,
			EgressIPClient:       egressIPFakeClient,
			EgressFirewallClient: egressFirewallFakeClient,
			CloudNetworkClient:   cloudNetworkFakeClient,
			EgressQoSClient:      egressQoSFakeClient,
			EgressServiceClient:  egressServiceFakeClient,
		}

		pods = make([]*v1.Pod, 0)
		podWatch = objSetup(fakeClient, "pods", func(core.Action) (bool, runtime.Object, error) {
			obj := &v1.PodList{}
			for _, p := range pods {
				obj.Items = append(obj.Items, *p)
			}
			return true, obj, nil
		})

		namespaces = make([]*v1.Namespace, 0)
		namespaceWatch = objSetup(fakeClient, "namespaces", func(core.Action) (bool, runtime.Object, error) {
			obj := &v1.NamespaceList{}
			for _, p := range namespaces {
				obj.Items = append(obj.Items, *p)
			}
			return true, obj, nil
		})

		nodes = make([]*v1.Node, 0)
		nodeWatch = objSetup(fakeClient, "nodes", func(core.Action) (bool, runtime.Object, error) {
			obj := &v1.NodeList{}
			for _, p := range nodes {
				obj.Items = append(obj.Items, *p)
			}
			return true, obj, nil
		})

		policies = make([]*knet.NetworkPolicy, 0)
		policyWatch = objSetup(fakeClient, "networkpolicies", func(core.Action) (bool, runtime.Object, error) {
			obj := &knet.NetworkPolicyList{}
			for _, p := range policies {
				obj.Items = append(obj.Items, *p)
			}
			return true, obj, nil
		})

		services = make([]*v1.Service, 0)
		serviceWatch = objSetup(fakeClient, "services", func(core.Action) (bool, runtime.Object, error) {
			obj := &v1.ServiceList{}
			for _, p := range services {
				obj.Items = append(obj.Items, *p)
			}
			return true, obj, nil
		})

		endpointSlices = make([]*discovery.EndpointSlice, 0)
		endpointSliceWatch = objSetup(fakeClient, "endpointslices", func(core.Action) (bool, runtime.Object, error) {
			obj := &discovery.EndpointSliceList{}
			for _, p := range endpointSlices {
				obj.Items = append(obj.Items, *p)
			}
			return true, obj, nil
		})

		egressFirewalls = make([]*egressfirewall.EgressFirewall, 0)
		egressFirewallWatch = egressFirewallObjSetup(egressFirewallFakeClient, "egressfirewalls", func(core.Action) (bool, runtime.Object, error) {
			obj := &egressfirewall.EgressFirewallList{}
			for _, p := range egressFirewalls {
				obj.Items = append(obj.Items, *p)
			}
			return true, obj, nil
		})

		egressIPs = make([]*egressip.EgressIP, 0)
		egressIPWatch = egressIPObjSetup(egressIPFakeClient, "egressips", func(core.Action) (bool, runtime.Object, error) {
			obj := &egressip.EgressIPList{}
			for _, p := range egressIPs {
				obj.Items = append(obj.Items, *p)
			}
			return true, obj, nil
		})

		cloudPrivateIPConfigs = make([]*ocpcloudnetworkapi.CloudPrivateIPConfig, 0)
		cloudPrivateIPConfigWatch = cloudPrivateIPConfigObjSetup(cloudNetworkFakeClient, "cloudprivateipconfigs", func(core.Action) (bool, runtime.Object, error) {
			obj := &ocpcloudnetworkapi.CloudPrivateIPConfigList{}
			for _, p := range cloudPrivateIPConfigs {
				obj.Items = append(obj.Items, *p)
			}
			return true, obj, nil
		})

		egressQoSes = make([]*egressqos.EgressQoS, 0)
		egressQoSWatch = egressQoSObjSetup(egressQoSFakeClient, "egressqoses", func(core.Action) (bool, runtime.Object, error) {
			obj := &egressqos.EgressQoSList{}
			for _, p := range egressQoSes {
				obj.Items = append(obj.Items, *p)
			}
			return true, obj, nil
		})

		egressServices = make([]*egressservice.EgressService, 0)
		egressServiceWatch = egressServiceObjSetup(egressServiceFakeClient, "egressservices", func(core.Action) (bool, runtime.Object, error) {
			obj := &egressservice.EgressServiceList{}
			for _, p := range egressServices {
				obj.Items = append(obj.Items, *p)
			}
			return true, obj, nil
		})
	})

	AfterEach(func() {
		wf.Shutdown()
	})

	Context("when a processExisting is given", func() {
		testExisting := func(objType reflect.Type, namespace string, sel labels.Selector, priority int) {
			if objType == EndpointSliceType {
				wf, err = NewNodeWatchFactory(ovnClientset.GetNodeClientset(), nodeName)
			} else {
				wf, err = NewMasterWatchFactory(ovnClientset)
			}
			Expect(err).NotTo(HaveOccurred())
			err = wf.Start()
			Expect(err).NotTo(HaveOccurred())
			h, err := wf.addHandler(objType, namespace, sel,
				cache.ResourceEventHandlerFuncs{},
				func(objs []interface{}) error {
					defer GinkgoRecover()
					Expect(len(objs)).To(Equal(1))
					return nil
				}, wf.GetHandlerPriority(objType))
			Expect(h).NotTo(BeNil())
			Expect(err).NotTo(HaveOccurred())
			Expect(h.priority).To(Equal(priority))
			wf.removeHandler(objType, h)
		}

		testExistingFilteredHandler := func(objType reflect.Type, realObj reflect.Type, namespace string, sel labels.Selector, priority int) {
			if objType == EndpointSliceType {
				wf, err = NewNodeWatchFactory(ovnClientset.GetNodeClientset(), nodeName)
			} else {
				wf, err = NewMasterWatchFactory(ovnClientset)
			}
			Expect(err).NotTo(HaveOccurred())
			err = wf.Start()
			Expect(err).NotTo(HaveOccurred())
			h, err := wf.AddFilteredPodHandler(namespace, sel,
				cache.ResourceEventHandlerFuncs{},
				func(objs []interface{}) error {
					defer GinkgoRecover()
					Expect(len(objs)).To(Equal(1))
					return nil
				}, wf.GetHandlerPriority(realObj))
			Expect(h).NotTo(BeNil())
			Expect(err).NotTo(HaveOccurred())
			Expect(h.priority).To(Equal(priority))
			wf.removeHandler(objType, h)
		}

		It("is called for each existing pod", func() {
			pods = append(pods, newPod("pod1", "default"))
			testExisting(PodType, "", nil, defaultHandlerPriority)
		})

		It("is called for each existing namespace", func() {
			namespaces = append(namespaces, newNamespace("default"))
			testExisting(NamespaceType, "", nil, defaultHandlerPriority)
		})

		It("is called for each existing node", func() {
			nodes = append(nodes, newNode("default"))
			testExisting(NodeType, "", nil, defaultHandlerPriority)
		})

		It("is called for each existing policy", func() {
			policies = append(policies, newPolicy("denyall", "default"))
			pods = append(pods, newPod("pod1", "default"))
			testExisting(PolicyType, "", nil, defaultHandlerPriority)
		})

		It("is called for each existing policy: AddressSetPodSelectorType", func() {
			policies = append(policies, newPolicy("denyall", "default"))
			pods = append(pods, newPod("pod1", "default"))
			testExistingFilteredHandler(PodType, AddressSetPodSelectorType, "default", nil, 2)
		})

		It("is called for each existing policy: LocalPodSelectorType", func() {
			policies = append(policies, newPolicy("denyall", "default"))
			pods = append(pods, newPod("pod1", "default"))
			testExistingFilteredHandler(PodType, LocalPodSelectorType, "default", nil, 3)
		})

		It("is called for each existing policy: AddressSetNamespaceAndPodSelectorType", func() {
			policies = append(policies, newPolicy("denyall", "default"))
			pods = append(pods, newPod("pod1", "default"))
			testExistingFilteredHandler(NamespaceType, AddressSetNamespaceAndPodSelectorType, "default", nil, 3)
		})

		It("is called for each existing policy: PeerNamespaceSelectorType", func() {
			policies = append(policies, newPolicy("denyall", "default"))
			pods = append(pods, newPod("pod1", "default"))
			testExistingFilteredHandler(NamespaceType, PeerNamespaceSelectorType, "default", nil, 2)
		})

		It("is called for each existing endpointSlice", func() {
			endpointSlices = append(endpointSlices, newEndpointSlice("myEndpointSlice", "default", "myService"))
			testExisting(EndpointSliceType, "", nil, defaultHandlerPriority)
		})

		It("is called for each existing service", func() {
			services = append(services, newService("myservice", "default"))
			testExisting(ServiceType, "", nil, defaultHandlerPriority)
		})

		It("is called for each existing egressFirewall", func() {
			egressFirewalls = append(egressFirewalls, newEgressFirewall("myEgressFirewall", "default"))
			testExisting(EgressFirewallType, "", nil, defaultHandlerPriority)
		})

		It("is called for each existing egressIP", func() {
			egressIPs = append(egressIPs, newEgressIP("myEgressIP", "default"))
			pods = append(pods, newPod("pod1", "default"))
			testExisting(EgressIPType, "", nil, defaultHandlerPriority)
		})

		It("is called for each existing egressIP: EgressIPPodType", func() {
			egressIPs = append(egressIPs, newEgressIP("myEgressIP", "default"))
			pods = append(pods, newPod("pod1", "default"))
			testExistingFilteredHandler(PodType, EgressIPPodType, "default", nil, 1)
		})

		It("is called for each existing egressIP: EgressIPNamespaceType", func() {
			egressIPs = append(egressIPs, newEgressIP("myEgressIP", "default"))
			pods = append(pods, newPod("pod1", "default"))
			testExistingFilteredHandler(NamespaceType, EgressIPNamespaceType, "default", nil, 1)
		})

		It("is called for each existing cloudPrivateIPConfig", func() {
			cloudPrivateIPConfigs = append(cloudPrivateIPConfigs, newCloudPrivateIPConfig("192.168.176.25"))
			testExisting(CloudPrivateIPConfigType, "", nil, defaultHandlerPriority)
		})
		It("is called for each existing egressQoS", func() {
			egressQoSes = append(egressQoSes, newEgressQoS("myEgressQoS", "default"))
			testExisting(EgressQoSType, "", nil, defaultHandlerPriority)
		})
		It("is called for each existing egressService", func() {
			egressServices = append(egressServices, newEgressService("myEgressService", "default"))
			testExisting(EgressServiceType, "", nil, defaultHandlerPriority)
		})

		It("is called for each existing pod that matches a given namespace and label", func() {
			pod := newPod("pod1", "default")
			pod.ObjectMeta.Labels["blah"] = "foobar"
			pods = append(pods, pod)

			sel, err := metav1.LabelSelectorAsSelector(
				&metav1.LabelSelector{
					MatchLabels: map[string]string{"blah": "foobar"},
				},
			)
			Expect(err).NotTo(HaveOccurred())

			testExisting(PodType, "default", sel, defaultHandlerPriority)
		})
	})

	Context("when existing items are known to the informer", func() {
		testExisting := func(objType reflect.Type) {
			if objType == EndpointSliceType {
				wf, err = NewNodeWatchFactory(ovnClientset.GetNodeClientset(), nodeName)
			} else {
				wf, err = NewMasterWatchFactory(ovnClientset)
			}
			Expect(err).NotTo(HaveOccurred())
			err = wf.Start()
			Expect(err).NotTo(HaveOccurred())
			var addCalls int32
			h, err := wf.addHandler(objType, "", nil,
				cache.ResourceEventHandlerFuncs{
					AddFunc: func(obj interface{}) {
						atomic.AddInt32(&addCalls, 1)
					},
					UpdateFunc: func(old, new interface{}) {},
					DeleteFunc: func(obj interface{}) {},
				}, nil, wf.GetHandlerPriority(objType))
			Expect(int(addCalls)).To(Equal(2))
			Expect(err).NotTo(HaveOccurred())
			wf.removeHandler(objType, h)
		}

		It("calls ADD for each existing pod", func() {
			pods = append(pods, newPod("pod1", "default"))
			pods = append(pods, newPod("pod2", "default"))
			testExisting(PodType)
		})

		It("calls ADD for each existing namespace", func() {
			namespaces = append(namespaces, newNamespace("default"))
			namespaces = append(namespaces, newNamespace("default2"))
			testExisting(NamespaceType)
		})

		It("calls ADD for each existing node", func() {
			nodes = append(nodes, newNode("default"))
			nodes = append(nodes, newNode("default2"))
			testExisting(NodeType)
		})

		It("calls ADD for each existing policy", func() {
			policies = append(policies, newPolicy("denyall", "default"))
			policies = append(policies, newPolicy("denyall2", "default"))
			testExisting(PolicyType)
		})

		It("calls ADD for each existing endpointSlices", func() {
			endpointSlices = append(endpointSlices, newEndpointSlice("myEndpointSlice", "default", "myService"))
			endpointSlices = append(endpointSlices, newEndpointSlice("myEndpointSlice2", "default", "myService"))
			testExisting(EndpointSliceType)
		})

		It("calls ADD for each existing service", func() {
			services = append(services, newService("myservice", "default"))
			services = append(services, newService("myservice2", "default"))
			testExisting(ServiceType)
		})

		It("calls ADD for each existing egressFirewall", func() {
			egressFirewalls = append(egressFirewalls, newEgressFirewall("myFirewall", "default"))
			egressFirewalls = append(egressFirewalls, newEgressFirewall("myFirewall1", "default"))
			testExisting(EgressFirewallType)
		})
		It("calls ADD for each existing egressIP", func() {
			egressIPs = append(egressIPs, newEgressIP("myEgressIP", "default"))
			egressIPs = append(egressIPs, newEgressIP("myEgressIP1", "default"))
			testExisting(EgressIPType)
		})
		It("calls ADD for each existing cloudPrivateIPConfig", func() {
			cloudPrivateIPConfigs = append(cloudPrivateIPConfigs, newCloudPrivateIPConfig("192.168.126.25"))
			cloudPrivateIPConfigs = append(cloudPrivateIPConfigs, newCloudPrivateIPConfig("192.168.126.26"))
			testExisting(CloudPrivateIPConfigType)
		})
		It("calls ADD for each existing egressQoS", func() {
			egressQoSes = append(egressQoSes, newEgressQoS("myEgressQoS", "default"))
			egressQoSes = append(egressQoSes, newEgressQoS("myEgressQoS1", "default"))
			testExisting(EgressQoSType)
		})
		It("calls ADD for each existing egressService", func() {
			egressServices = append(egressServices, newEgressService("myEgressService", "default"))
			egressServices = append(egressServices, newEgressService("myEgressService1", "default"))
			testExisting(EgressServiceType)
		})
	})

	Context("when EgressIP is disabled", func() {
		testExisting := func(objType reflect.Type) {
			wf, err = NewMasterWatchFactory(ovnClientset)
			Expect(err).NotTo(HaveOccurred())
			err = wf.Start()
			Expect(err).NotTo(HaveOccurred())
			Expect(wf.informers).NotTo(HaveKey(objType))
		}
		It("does not contain Egress IP informer", func() {
			config.OVNKubernetesFeature.EnableEgressIP = false
			testExisting(EgressIPType)
		})
	})
	Context("when EgressFirewall is disabled", func() {
		testExisting := func(objType reflect.Type) {
			wf, err = NewMasterWatchFactory(ovnClientset)
			Expect(err).NotTo(HaveOccurred())
			err = wf.Start()
			Expect(err).NotTo(HaveOccurred())
			Expect(wf.informers).NotTo(HaveKey(objType))
		}
		It("does not contain EgressFirewall informer", func() {
			config.OVNKubernetesFeature.EnableEgressFirewall = false
			testExisting(EgressFirewallType)
		})
	})
	Context("when EgressQoS is disabled", func() {
		testExisting := func(objType reflect.Type) {
			wf, err = NewMasterWatchFactory(ovnClientset)
			Expect(err).NotTo(HaveOccurred())
			err = wf.Start()
			Expect(err).NotTo(HaveOccurred())
			Expect(wf.informers).NotTo(HaveKey(objType))
		}
		It("does not contain EgressQoS informer", func() {
			config.OVNKubernetesFeature.EnableEgressQoS = false
			testExisting(EgressQoSType)
		})
	})
	Context("when EgressService is disabled", func() {
		testExisting := func(objType reflect.Type) {
			wf, err = NewMasterWatchFactory(ovnClientset)
			Expect(err).NotTo(HaveOccurred())
			err = wf.Start()
			Expect(err).NotTo(HaveOccurred())
			Expect(wf.informers).NotTo(HaveKey(objType))
		}
		It("does not contain EgressService informer", func() {
			config.OVNKubernetesFeature.EnableEgressService = false
			testExisting(EgressServiceType)
		})
	})

	addFilteredHandler := func(wf *WatchFactory, objType reflect.Type, realObjType reflect.Type, namespace string, sel labels.Selector, funcs cache.ResourceEventHandlerFuncs) (*Handler, *handlerCalls) {
		calls := handlerCalls{}
		h, err := wf.addHandler(objType, namespace, sel, cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				defer GinkgoRecover()
				atomic.AddInt32(&calls.added, 1)
				funcs.AddFunc(obj)
			},
			UpdateFunc: func(old, new interface{}) {
				defer GinkgoRecover()
				atomic.AddInt32(&calls.updated, 1)
				funcs.UpdateFunc(old, new)
			},
			DeleteFunc: func(obj interface{}) {
				defer GinkgoRecover()
				atomic.AddInt32(&calls.deleted, 1)
				funcs.DeleteFunc(obj)
			},
		}, nil, wf.GetHandlerPriority(realObjType))
		Expect(h).NotTo(BeNil())
		Expect(err).NotTo(HaveOccurred())
		return h, &calls
	}

	addHandler := func(wf *WatchFactory, objType reflect.Type, funcs cache.ResourceEventHandlerFuncs) (*Handler, *handlerCalls) {
		return addFilteredHandler(wf, objType, objType, "", nil, funcs)
	}

	addPriorityHandler := func(wf *WatchFactory, objType reflect.Type, realObjType reflect.Type, funcs cache.ResourceEventHandlerFuncs) (*Handler, *handlerCalls) {
		return addFilteredHandler(wf, objType, realObjType, "", nil, funcs)
	}

	It("responds to pod add/update/delete events", func() {
		wf, err = NewMasterWatchFactory(ovnClientset)
		Expect(err).NotTo(HaveOccurred())
		err = wf.Start()
		Expect(err).NotTo(HaveOccurred())

		added := newPod("pod1", "default")
		h, c := addHandler(wf, PodType, cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				pod := obj.(*v1.Pod)
				Expect(reflect.DeepEqual(pod, added)).To(BeTrue())
			},
			UpdateFunc: func(old, new interface{}) {
				newPod := new.(*v1.Pod)
				Expect(reflect.DeepEqual(newPod, added)).To(BeTrue())
				Expect(newPod.Spec.NodeName).To(Equal("foobar"))
			},
			DeleteFunc: func(obj interface{}) {
				pod := obj.(*v1.Pod)
				Expect(reflect.DeepEqual(pod, added)).To(BeTrue())
			},
		})

		pods = append(pods, added)
		podWatch.Add(added)
		Eventually(c.getAdded, 2).Should(Equal(1))
		added.Spec.NodeName = "foobar"
		podWatch.Modify(added)
		Eventually(c.getUpdated, 2).Should(Equal(1))
		pods = pods[:0]
		podWatch.Delete(added)
		Eventually(c.getDeleted, 2).Should(Equal(1))

		wf.RemovePodHandler(h)
	})

	It("responds to pod replace with create/update/delete events", func() {
		wf, err = NewMasterWatchFactory(ovnClientset)
		Expect(err).NotTo(HaveOccurred())
		err = wf.Start()
		Expect(err).NotTo(HaveOccurred())

		added := newPod("pod1", "default")
		added.UID = "mybar"
		h, c := addHandler(wf, PodType, cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				pod := obj.(*v1.Pod)
				Expect(pod.Spec.NodeName).To(Equal("mynode"))
			},
			UpdateFunc: func(old, new interface{}) {
				newPod := new.(*v1.Pod)
				Expect(newPod.UID).To(Equal(types.UID("mybar")))
				Expect(newPod.Spec.NodeName).To(Equal("foobar"))
			},
			DeleteFunc: func(obj interface{}) {
			},
		})

		pods = append(pods, added)
		podWatch.Add(added)
		Eventually(c.getAdded, 2).Should(Equal(1))
		podCopy := added.DeepCopy()
		podCopy.Spec.NodeName = "foobar"
		podWatch.Modify(podCopy)
		Eventually(c.getUpdated, 2).Should(Equal(1))
		podCopy = added.DeepCopy()
		podCopy.UID = "foobar"
		podCopy.Spec.NodeName = "mynode"
		podWatch.Modify(podCopy)
		Eventually(c.getDeleted, 2).Should(Equal(1))
		Eventually(c.getAdded, 2).Should(Equal(2))
		Eventually(c.getUpdated, 2).Should(Equal(1))
		pods = pods[:0]
		podWatch.Delete(added)
		Eventually(c.getDeleted, 2).Should(Equal(2))

		wf.RemovePodHandler(h)
	})

	It("responds to multiple pod add/update/delete events", func() {
		wf, err = NewMasterWatchFactory(ovnClientset)
		Expect(err).NotTo(HaveOccurred())
		err = wf.Start()
		Expect(err).NotTo(HaveOccurred())

		const nodeName string = "mynode"
		type opTest struct {
			mu      sync.Mutex
			pod     *v1.Pod
			added   int
			updated int
			deleted int
		}
		testPods := make(map[string]*opTest)

		for i := 0; i < 5; i++ {
			name := fmt.Sprintf("mypod-%d", i)
			pod := newPod(name, fmt.Sprintf("namespace-%d", i))
			testPods[name] = &opTest{pod: pod}
		}

		h, c := addHandler(wf, PodType, cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				pod := obj.(*v1.Pod)
				ot, ok := testPods[pod.Name]
				Expect(ok).To(BeTrue())
				ot.mu.Lock()
				defer ot.mu.Unlock()
				Expect(ot.added).To(BeNumerically("<", 2))
				ot.added++
			},
			UpdateFunc: func(old, new interface{}) {
				newPod := new.(*v1.Pod)
				ot, ok := testPods[newPod.Name]
				Expect(ok).To(BeTrue())
				ot.mu.Lock()
				defer ot.mu.Unlock()
				Expect(ot.updated).To(BeNumerically("<", 2))
				ot.updated++
				Expect(newPod.Spec.NodeName).To(Equal(nodeName))
			},
			DeleteFunc: func(obj interface{}) {
				pod := obj.(*v1.Pod)
				ot, ok := testPods[pod.Name]
				Expect(ok).To(BeTrue())
				ot.mu.Lock()
				defer ot.mu.Unlock()
				Expect(ot.deleted).To(BeNumerically("<", 2))
				ot.deleted++
			},
		})

		// Add/Update/Delete each pod twice
		for i := 0; i < 2; i++ {
			for _, ot := range testPods {
				pods = append(pods, ot.pod)
				podWatch.Add(ot.pod)
				ot.mu.Lock()
				ot.pod.Spec.NodeName = nodeName
				ot.mu.Unlock()
				podWatch.Modify(ot.pod)
				pods = pods[:0]
				podWatch.Delete(ot.pod)
			}
		}

		// Ensure total number of each operation is 10; and each
		// node's individual operation count is 2
		Eventually(c.getAdded, 2).Should(Equal(10))
		Eventually(c.getUpdated, 2).Should(Equal(10))
		Eventually(c.getDeleted, 2).Should(Equal(10))
		for _, ot := range testPods {
			ot.mu.Lock()
			Expect(ot.added).Should(Equal(2))
			Expect(ot.updated).Should(Equal(2))
			Expect(ot.deleted).Should(Equal(2))
			ot.mu.Unlock()
		}

		wf.RemovePodHandler(h)
	})

	It("responds to namespace add/update/delete events", func() {
		wf, err = NewMasterWatchFactory(ovnClientset)
		Expect(err).NotTo(HaveOccurred())
		err = wf.Start()
		Expect(err).NotTo(HaveOccurred())

		added := newNamespace("default")
		h, c := addHandler(wf, NamespaceType, cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				ns := obj.(*v1.Namespace)
				Expect(reflect.DeepEqual(ns, added)).To(BeTrue())
			},
			UpdateFunc: func(old, new interface{}) {
				newNS := new.(*v1.Namespace)
				Expect(reflect.DeepEqual(newNS, added)).To(BeTrue())
				Expect(newNS.Status.Phase).To(Equal(v1.NamespaceTerminating))
			},
			DeleteFunc: func(obj interface{}) {
				ns := obj.(*v1.Namespace)
				Expect(reflect.DeepEqual(ns, added)).To(BeTrue())
			},
		})

		namespaces = append(namespaces, added)
		namespaceWatch.Add(added)
		Eventually(c.getAdded, 2).Should(Equal(1))
		added.Status.Phase = v1.NamespaceTerminating
		namespaceWatch.Modify(added)
		Eventually(c.getUpdated, 2).Should(Equal(1))
		namespaces = namespaces[:0]
		namespaceWatch.Delete(added)
		Eventually(c.getDeleted, 2).Should(Equal(1))

		wf.RemoveNamespaceHandler(h)
	})

	It("responds to node add/update/delete events", func() {
		wf, err = NewMasterWatchFactory(ovnClientset)
		Expect(err).NotTo(HaveOccurred())
		err = wf.Start()
		Expect(err).NotTo(HaveOccurred())

		added := newNode("mynode")
		h, c := addHandler(wf, NodeType, cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				node := obj.(*v1.Node)
				Expect(reflect.DeepEqual(node, added)).To(BeTrue())
			},
			UpdateFunc: func(old, new interface{}) {
				newNode := new.(*v1.Node)
				Expect(reflect.DeepEqual(newNode, added)).To(BeTrue())
				Expect(newNode.Status.Phase).To(Equal(v1.NodeTerminated))
			},
			DeleteFunc: func(obj interface{}) {
				node := obj.(*v1.Node)
				Expect(reflect.DeepEqual(node, added)).To(BeTrue())
			},
		})

		nodes = append(nodes, added)
		nodeWatch.Add(added)
		Eventually(c.getAdded, 2).Should(Equal(1))
		added.Status.Phase = v1.NodeTerminated
		nodeWatch.Modify(added)
		Eventually(c.getUpdated, 2).Should(Equal(1))
		nodes = nodes[:0]
		nodeWatch.Delete(added)
		Eventually(c.getDeleted, 2).Should(Equal(1))

		wf.RemoveNodeHandler(h)
	})

	It("responds to multiple node add/update/delete events", func() {
		wf, err = NewMasterWatchFactory(ovnClientset)
		Expect(err).NotTo(HaveOccurred())
		err = wf.Start()
		Expect(err).NotTo(HaveOccurred())

		type opTest struct {
			mu      sync.Mutex
			node    *v1.Node
			added   int
			updated int
			deleted int
		}
		testNodes := make(map[string]*opTest)

		for i := 0; i < 5; i++ {
			name := fmt.Sprintf("mynode-%d", i)
			node := newNode(name)
			testNodes[name] = &opTest{node: node}
		}

		h, c := addHandler(wf, NodeType, cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				node := obj.(*v1.Node)
				ot, ok := testNodes[node.Name]
				Expect(ok).To(BeTrue())
				ot.mu.Lock()
				defer ot.mu.Unlock()
				Expect(ot.added).To(BeNumerically("<", 2))
				ot.added++
			},
			UpdateFunc: func(old, new interface{}) {
				newNode := new.(*v1.Node)
				ot, ok := testNodes[newNode.Name]
				Expect(ok).To(BeTrue())
				ot.mu.Lock()
				defer ot.mu.Unlock()
				Expect(ot.updated).To(BeNumerically("<", 2))
				ot.updated++
				Expect(newNode.Status.Phase).To(Equal(v1.NodeTerminated))
			},
			DeleteFunc: func(obj interface{}) {
				node := obj.(*v1.Node)
				ot, ok := testNodes[node.Name]
				Expect(ok).To(BeTrue())
				ot.mu.Lock()
				Expect(ot.deleted).To(BeNumerically("<", 2))
				ot.deleted++
				ot.mu.Unlock()
			},
		})

		// Add/Update/Delete each node twice
		for i := 0; i < 2; i++ {
			for _, ot := range testNodes {
				nodes = append(nodes, ot.node)
				nodeWatch.Add(ot.node)
				ot.mu.Lock()
				ot.node.Status.Phase = v1.NodeTerminated
				ot.mu.Unlock()
				nodeWatch.Modify(ot.node)
				nodes = nodes[:0]
				nodeWatch.Delete(ot.node)
			}
		}

		// Ensure total number of each operation is 10; and each
		// node's individual operation count is 2
		Eventually(c.getAdded, 2).Should(Equal(10))
		Eventually(c.getUpdated, 2).Should(Equal(10))
		Eventually(c.getDeleted, 2).Should(Equal(10))
		for _, ot := range testNodes {
			ot.mu.Lock()
			Expect(ot.added).Should(Equal(2))
			Expect(ot.updated).Should(Equal(2))
			Expect(ot.deleted).Should(Equal(2))
			ot.mu.Unlock()
		}

		wf.RemoveNodeHandler(h)
	})

	It("correctly orders queued informer initial add events and subsequent update events", func() {
		type opTest struct {
			mu      sync.Mutex
			node    *v1.Node
			added   int
			updated int
		}
		testNodes := make(map[string]*opTest)

		for i := 0; i < 600; i++ {
			name := fmt.Sprintf("mynode-%d", i)
			node := newNode(name)
			testNodes[name] = &opTest{node: node}
			// Add all nodes to the initial list
			nodes = append(nodes, node)
		}

		wf, err = NewMasterWatchFactory(ovnClientset)
		Expect(err).NotTo(HaveOccurred())
		err = wf.Start()
		Expect(err).NotTo(HaveOccurred())

		h, c := addHandler(wf, NodeType, cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				defer GinkgoRecover()
				node := obj.(*v1.Node)
				ot, ok := testNodes[node.Name]
				Expect(ok).To(BeTrue())
				ot.mu.Lock()
				defer ot.mu.Unlock()
				Expect(ot.added).To(Equal(0), "add for node %s already run", node.Name)
				ot.added++
			},
			UpdateFunc: func(old, new interface{}) {
				defer GinkgoRecover()
				newNode := new.(*v1.Node)
				ot, ok := testNodes[newNode.Name]
				Expect(ok).To(BeTrue())
				// Expect updates to be processed after Add
				ot.mu.Lock()
				defer ot.mu.Unlock()
				Expect(ot.added).To(Equal(1), "update for node %s processed before initial add!", newNode.Name)
				Expect(ot.updated).To(Equal(0))
				ot.updated++
				Expect(newNode.Status.Phase).To(Equal(v1.NodeTerminated))
			},
			DeleteFunc: func(obj interface{}) {},
		})

		done := make(chan bool)
		go func() {
			// Send an update event for each node
			for _, n := range nodes {
				n.Status.Phase = v1.NodeTerminated
				nodeWatch.Modify(n)
			}
			done <- true
		}()

		// Adds are done synchronously at handler addition time
		for _, ot := range testNodes {
			ot.mu.Lock()
			Expect(ot.added).To(Equal(1), "missing add for node %s", ot.node.Name)
			ot.mu.Unlock()
		}
		Expect(c.getAdded()).To(Equal(len(testNodes)))

		<-done
		// Updates are async and may take a bit longer to finish
		Eventually(c.getUpdated, 10).Should(Equal(len(testNodes)))
		for _, ot := range testNodes {
			ot.mu.Lock()
			Expect(ot.updated).To(Equal(1), "missing update for node %s", ot.node.Name)
			ot.mu.Unlock()
		}

		wf.RemoveNodeHandler(h)
	})

	It("correctly orders serialized informer initial add events and subsequent update events", func() {
		type opTest struct {
			mu        sync.Mutex
			namespace *v1.Namespace
			added     int
			updated   int
		}
		testNamespaces := make(map[string]*opTest)

		for i := 0; i < 598; i++ {
			name := fmt.Sprintf("mynamespace-%d", i)
			namespace := newNamespace(name)
			testNamespaces[name] = &opTest{namespace: namespace}
			// Add all namespaces to the initial list
			namespaces = append(namespaces, namespace)
		}

		wf, err = NewMasterWatchFactory(ovnClientset)
		Expect(err).NotTo(HaveOccurred())
		err = wf.Start()
		Expect(err).NotTo(HaveOccurred())

		startWg := sync.WaitGroup{}
		startWg.Add(1)
		doneWg := sync.WaitGroup{}
		doneWg.Add(1)
		go func() {
			startWg.Done()
			// Send an update event for each namespace
			for _, n := range namespaces {
				n.Status.Phase = v1.NamespaceTerminating
				namespaceWatch.Modify(n)
			}
			doneWg.Done()
		}()
		startWg.Wait()

		h, c := addHandler(wf, NamespaceType, cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				defer GinkgoRecover()
				namespace := obj.(*v1.Namespace)
				ot, ok := testNamespaces[namespace.Name]
				Expect(ok).To(BeTrue())
				ot.mu.Lock()
				defer ot.mu.Unlock()
				Expect(ot.added).To(Equal(0))
				ot.added++
			},
			UpdateFunc: func(old, new interface{}) {
				defer GinkgoRecover()
				newNamespace := new.(*v1.Namespace)
				ot, ok := testNamespaces[newNamespace.Name]
				Expect(ok).To(BeTrue())
				// Expect updates to be processed after Add
				ot.mu.Lock()
				defer ot.mu.Unlock()
				Expect(ot.added).To(Equal(1), "update for namespace %s processed before initial add!", newNamespace.Name)
				Expect(ot.updated).To(Equal(0))
				ot.updated++
				Expect(newNamespace.Status.Phase).To(Equal(v1.NamespaceTerminating))
			},
			DeleteFunc: func(obj interface{}) {},
		})
		doneWg.Wait()

		// Adds are done synchronously at handler addition time
		for _, ot := range testNamespaces {
			ot.mu.Lock()
			Expect(ot.added).To(Equal(1), "missing add for namespace %s", ot.namespace.Name)
			ot.mu.Unlock()
		}
		Expect(c.getAdded()).To(Equal(len(testNamespaces)))

		// Updates are async and may take a bit longer to finish
		Eventually(c.getUpdated, 10).Should(Equal(len(testNamespaces)))
		for _, ot := range testNamespaces {
			ot.mu.Lock()
			Expect(ot.updated).To(Equal(1), "missing update for namespace %s", ot.namespace.Name)
			ot.mu.Unlock()
		}

		wf.RemoveNamespaceHandler(h)
	})

	It("correctly orders add events across prioritized handlers sharing the same object type", func() {
		type opTest struct {
			mu        sync.Mutex
			namespace *v1.Namespace
			added     int
			updated   int
			deleted   int
		}
		testNamespaces := make(map[string]*opTest)

		for i := 0; i < 998; i++ {
			name := fmt.Sprintf("mynamespace-%d", i)
			namespace := newNamespace(name)
			namespace.Status.Phase = ""
			testNamespaces[name] = &opTest{namespace: namespace}
			// Add all namespaces to the initial list
			namespaces = append(namespaces, namespace)
		}

		wf, err = NewMasterWatchFactory(ovnClientset)
		Expect(err).NotTo(HaveOccurred())
		err = wf.Start()
		Expect(err).NotTo(HaveOccurred())

		nsh, c1 := addPriorityHandler(wf, NamespaceType, NamespaceType, cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				defer GinkgoRecover()
				namespace := obj.(*v1.Namespace)
				ot, ok := testNamespaces[namespace.Name]
				Expect(ok).To(BeTrue())
				ot.mu.Lock()
				defer ot.mu.Unlock()
				Expect(ot.added).To(Equal(0))
				ot.added++
				Expect(namespace.Status.Phase).To(BeEmpty())
			},
			UpdateFunc: func(old, new interface{}) {
				defer GinkgoRecover()
				newNamespace := new.(*v1.Namespace)
				ot, ok := testNamespaces[newNamespace.Name]
				Expect(ok).To(BeTrue())
				// Expect updates to be processed after Add
				ot.mu.Lock()
				defer ot.mu.Unlock()
				Expect(ot.added).To(Equal(4), "update for EIP namespace %s processed before add was processed in all handlers!", newNamespace.Name)
				Expect(ot.updated).To(Equal(0))
				ot.updated++
				Expect(newNamespace.Status.Phase).To(Equal(v1.NamespaceActive))
			},
			DeleteFunc: func(obj interface{}) {
				defer GinkgoRecover()
				newNamespace := obj.(*v1.Namespace)
				ot, ok := testNamespaces[newNamespace.Name]
				Expect(ok).To(BeTrue())
				// Verify that deletes were processed after the updates and adds
				ot.mu.Lock()
				defer ot.mu.Unlock()
				Expect(ot.added).To(Equal(4), "delete for EIP namespace %s processed before add was processed in all handlers!", newNamespace.Name)
				Expect(ot.updated).To(Equal(4), "delete for EIP namespace %s processed before update was processed in all handlers!", newNamespace.Name)
				Expect(ot.deleted).To(Equal(8))
				ot.deleted = ot.deleted / 2
				Expect(newNamespace.Status.Phase).To(Equal(v1.NamespaceTerminating))
			},
		})

		eipnsh, c2 := addPriorityHandler(wf, NamespaceType, EgressIPNamespaceType, cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				defer GinkgoRecover()
				namespace := obj.(*v1.Namespace)
				ot, ok := testNamespaces[namespace.Name]
				Expect(ok).To(BeTrue())
				ot.mu.Lock()
				defer ot.mu.Unlock()
				Expect(ot.added).To(Equal(1), "add for EIP namespace %s processed before initial namespace add!", namespace.Name)
				ot.added = ot.added * 10
				Expect(namespace.Status.Phase).To(BeEmpty())
			},
			UpdateFunc: func(old, new interface{}) {
				defer GinkgoRecover()
				newNamespace := new.(*v1.Namespace)
				ot, ok := testNamespaces[newNamespace.Name]
				Expect(ok).To(BeTrue())
				// Expect updates to be processed after Add
				ot.mu.Lock()
				defer ot.mu.Unlock()
				Expect(ot.added).To(Equal(4), "update for EIP namespace %s processed before add was processed in all handlers!", newNamespace.Name)
				Expect(ot.updated).To(Equal(1), "update for EIP namespace %s processed before initial namespace update!", newNamespace.Name)
				ot.updated = ot.updated * 10
				Expect(newNamespace.Status.Phase).To(Equal(v1.NamespaceActive))
			},
			DeleteFunc: func(obj interface{}) {
				defer GinkgoRecover()
				newNamespace := obj.(*v1.Namespace)
				ot, ok := testNamespaces[newNamespace.Name]
				Expect(ok).To(BeTrue())
				// Verify that deletes were processed after the updates and adds
				ot.mu.Lock()
				defer ot.mu.Unlock()
				Expect(ot.added).To(Equal(4), "delete for EIP namespace %s processed before add was processed in all handlers!", newNamespace.Name)
				Expect(ot.updated).To(Equal(4), "delete for EIP namespace %s processed before update was processed in all handlers!", newNamespace.Name)
				Expect(ot.deleted).To(Equal(10))
				ot.deleted = ot.deleted - 2
				Expect(newNamespace.Status.Phase).To(Equal(v1.NamespaceTerminating))
			},
		})
		peernsh, c3 := addPriorityHandler(wf, NamespaceType, PeerNamespaceSelectorType, cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				defer GinkgoRecover()
				namespace := obj.(*v1.Namespace)
				ot, ok := testNamespaces[namespace.Name]
				Expect(ok).To(BeTrue())
				ot.mu.Lock()
				defer ot.mu.Unlock()
				Expect(ot.added).To(Equal(10), "add for peer namespace %s processed before EIP namespace add!", namespace.Name)
				ot.added = ot.added - 2
				Expect(namespace.Status.Phase).To(BeEmpty())
			},
			UpdateFunc: func(old, new interface{}) {
				defer GinkgoRecover()
				newNamespace := new.(*v1.Namespace)
				ot, ok := testNamespaces[newNamespace.Name]
				Expect(ok).To(BeTrue())
				// Expect updates to be processed after Add
				ot.mu.Lock()
				defer ot.mu.Unlock()
				Expect(ot.added).To(Equal(4), "update for EIP namespace %s processed before add was processed in all handlers!", newNamespace.Name)
				Expect(ot.updated).To(Equal(10), "update for peer namespace %s processed before EIP namespace update!", newNamespace.Name)
				ot.updated = ot.updated - 2
				Expect(newNamespace.Status.Phase).To(Equal(v1.NamespaceActive))
			},
			DeleteFunc: func(obj interface{}) {
				defer GinkgoRecover()
				newNamespace := obj.(*v1.Namespace)
				ot, ok := testNamespaces[newNamespace.Name]
				Expect(ok).To(BeTrue())
				// Verify that deletes were processed after the updates and adds
				ot.mu.Lock()
				defer ot.mu.Unlock()
				Expect(ot.added).To(Equal(4), "delete for EIP namespace %s processed before add was processed in all handlers!", newNamespace.Name)
				Expect(ot.updated).To(Equal(4), "delete for EIP namespace %s processed before update was processed in all handlers!", newNamespace.Name)
				Expect(ot.deleted).To(Equal(1))
				ot.deleted = ot.deleted * 10
				Expect(newNamespace.Status.Phase).To(Equal(v1.NamespaceTerminating))
			},
		})
		peerpodnsh, c4 := addPriorityHandler(wf, NamespaceType, AddressSetNamespaceAndPodSelectorType, cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				defer GinkgoRecover()
				namespace := obj.(*v1.Namespace)
				ot, ok := testNamespaces[namespace.Name]
				Expect(ok).To(BeTrue())
				ot.mu.Lock()
				defer ot.mu.Unlock()
				Expect(ot.added).To(Equal(8), "add for peerPod namespace %s processed before peer namespace add!", namespace.Name)
				ot.added = ot.added / 2
				Expect(namespace.Status.Phase).To(BeEmpty())
			},
			UpdateFunc: func(old, new interface{}) {
				defer GinkgoRecover()
				newNamespace := new.(*v1.Namespace)
				ot, ok := testNamespaces[newNamespace.Name]
				Expect(ok).To(BeTrue())
				// Expect updates to be processed after Add
				ot.mu.Lock()
				defer ot.mu.Unlock()
				Expect(ot.added).To(Equal(4), "update for peerPod namespace %s processed before peerPod namespace add!", newNamespace.Name)
				Expect(ot.updated).To(Equal(8), "update for peerPod namespace %s processed before peer namespace update!", newNamespace.Name)
				ot.updated = ot.updated / 2
				Expect(newNamespace.Status.Phase).To(Equal(v1.NamespaceActive))
			},
			DeleteFunc: func(obj interface{}) {
				defer GinkgoRecover()
				newNamespace := obj.(*v1.Namespace)
				ot, ok := testNamespaces[newNamespace.Name]
				Expect(ok).To(BeTrue())
				// Verify that deletes were processed after the updates and adds
				ot.mu.Lock()
				defer ot.mu.Unlock()
				Expect(ot.added).To(Equal(4), "delete for EIP namespace %s processed before add was processed in all handlers!", newNamespace.Name)
				Expect(ot.updated).To(Equal(4), "delete for EIP namespace %s processed before update was processed in all handlers!", newNamespace.Name)
				Expect(ot.deleted).To(Equal(0))
				ot.deleted++
				Expect(newNamespace.Status.Phase).To(Equal(v1.NamespaceTerminating))
			},
		})
		done := make(chan bool)
		go func() {
			// Send an update event for each namespace
			for _, n := range namespaces {
				n.Status.Phase = v1.NamespaceActive
				namespaceWatch.Modify(n)
			}
			done <- true
		}()

		// Adds are done synchronously at handler addition time
		for _, ot := range testNamespaces {
			ot.mu.Lock()
			// ((((0 + 1) * 10) - 2) / 2) = 4
			Expect(ot.added).To(Equal(4), "missing add for namespace %s", ot.namespace.Name)
			ot.mu.Unlock()
		}
		Expect(c1.getAdded()).To(Equal(len(testNamespaces)))
		Expect(c2.getAdded()).To(Equal(len(testNamespaces)))
		Expect(c3.getAdded()).To(Equal(len(testNamespaces)))
		Expect(c4.getAdded()).To(Equal(len(testNamespaces)))
		<-done
		// Updates are async and may take a bit longer to finish
		Eventually(c1.getUpdated, 10).Should(Equal(len(testNamespaces)))
		Eventually(c2.getUpdated, 10).Should(Equal(len(testNamespaces)))
		Eventually(c3.getUpdated, 10).Should(Equal(len(testNamespaces)))
		Eventually(c4.getUpdated, 10).Should(Equal(len(testNamespaces)))

		for _, ot := range testNamespaces {
			ot.mu.Lock()
			// ((((0 + 1) * 10) - 2) / 2) = 4
			Expect(ot.updated).To(Equal(4), "missing update for namespace %s", ot.namespace.Name)
			ot.mu.Unlock()
		}

		go func() {
			// Send a delete event for each namespace
			for _, n := range namespaces {
				n.Status.Phase = v1.NamespaceTerminating
				namespaceWatch.Delete(n)
			}
			done <- true
		}()
		<-done
		// Deletes are async and may take a bit longer to finish
		Eventually(c1.getDeleted, 10).Should(Equal(len(testNamespaces)))
		Eventually(c2.getDeleted, 10).Should(Equal(len(testNamespaces)))
		Eventually(c3.getDeleted, 10).Should(Equal(len(testNamespaces)))
		Eventually(c4.getDeleted, 10).Should(Equal(len(testNamespaces)))

		for _, ot := range testNamespaces {
			ot.mu.Lock()
			// ((((0 + 1) * 10) - 2) / 2) = 4
			Expect(ot.deleted).To(Equal(4), "missing delete for namespace %s", ot.namespace.Name)
			ot.mu.Unlock()
		}

		wf.RemoveNamespaceHandler(nsh)
		wf.RemoveNamespaceHandler(eipnsh)
		wf.RemoveNamespaceHandler(peernsh)
		wf.RemoveNamespaceHandler(peerpodnsh)
	})

	It("responds to policy add/update/delete events", func() {
		wf, err = NewMasterWatchFactory(ovnClientset)
		Expect(err).NotTo(HaveOccurred())
		err = wf.Start()
		Expect(err).NotTo(HaveOccurred())

		added := newPolicy("mypolicy", "default")
		h, c := addHandler(wf, PolicyType, cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				np := obj.(*knet.NetworkPolicy)
				Expect(reflect.DeepEqual(np, added)).To(BeTrue())
			},
			UpdateFunc: func(old, new interface{}) {
				newNP := new.(*knet.NetworkPolicy)
				Expect(reflect.DeepEqual(newNP, added)).To(BeTrue())
				Expect(newNP.Spec.PolicyTypes).To(Equal([]knet.PolicyType{knet.PolicyTypeIngress}))
			},
			DeleteFunc: func(obj interface{}) {
				np := obj.(*knet.NetworkPolicy)
				Expect(reflect.DeepEqual(np, added)).To(BeTrue())
			},
		})

		policies = append(policies, added)
		policyWatch.Add(added)
		Eventually(c.getAdded, 2).Should(Equal(1))
		added.Spec.PolicyTypes = []knet.PolicyType{knet.PolicyTypeIngress}
		policyWatch.Modify(added)
		Eventually(c.getUpdated, 2).Should(Equal(1))
		policies = policies[:0]
		policyWatch.Delete(added)
		Eventually(c.getDeleted, 2).Should(Equal(1))

		wf.RemovePolicyHandler(h)
	})

	It("responds to endpointslices add/update/delete events", func() {
		wf, err = NewNodeWatchFactory(ovnClientset.GetNodeClientset(), nodeName)
		Expect(err).NotTo(HaveOccurred())
		err = wf.Start()
		Expect(err).NotTo(HaveOccurred())

		added := newEndpointSlice("myEndpointSlice", "default", "myService")
		h, c := addHandler(wf, EndpointSliceType, cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				epSlice := obj.(*discovery.EndpointSlice)
				Expect(reflect.DeepEqual(epSlice, added)).To(BeTrue())
			},
			UpdateFunc: func(old, new interface{}) {
				newEpSlice := new.(*discovery.EndpointSlice)
				Expect(reflect.DeepEqual(newEpSlice, added)).To(BeTrue())
				Expect(len(newEpSlice.Endpoints)).To(Equal(1))
			},
			DeleteFunc: func(obj interface{}) {
				epSlice := obj.(*discovery.EndpointSlice)
				Expect(reflect.DeepEqual(epSlice, added)).To(BeTrue())
			},
		})

		endpointSlices = append(endpointSlices, added)
		endpointSliceWatch.Add(added)
		Eventually(c.getAdded, 2).Should(Equal(1))
		added.Endpoints = append(added.Endpoints, discovery.Endpoint{
			Addresses: []string{"1.1.1.1"},
		})
		endpointSliceWatch.Modify(added)
		Eventually(c.getUpdated, 2).Should(Equal(1))
		endpointSlices = endpointSlices[:0]
		endpointSliceWatch.Delete(added)
		Eventually(c.getDeleted, 2).Should(Equal(1))

		wf.RemoveEndpointSliceHandler(h)
	})

	It("responds to service add/update/delete events", func() {
		wf, err = NewMasterWatchFactory(ovnClientset)
		Expect(err).NotTo(HaveOccurred())
		err = wf.Start()
		Expect(err).NotTo(HaveOccurred())

		added := newService("myservice", "default")
		h, c := addHandler(wf, ServiceType, cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				service := obj.(*v1.Service)
				Expect(reflect.DeepEqual(service, added)).To(BeTrue())
			},
			UpdateFunc: func(old, new interface{}) {
				newService := new.(*v1.Service)
				Expect(reflect.DeepEqual(newService, added)).To(BeTrue())
				Expect(newService.Spec.ClusterIP).To(Equal("1.1.1.1"))
			},
			DeleteFunc: func(obj interface{}) {
				service := obj.(*v1.Service)
				Expect(reflect.DeepEqual(service, added)).To(BeTrue())
			},
		})

		services = append(services, added)
		serviceWatch.Add(added)
		Eventually(c.getAdded, 2).Should(Equal(1))
		added.Spec.ClusterIP = "1.1.1.1"
		serviceWatch.Modify(added)
		Eventually(c.getUpdated, 2).Should(Equal(1))
		services = services[:0]
		serviceWatch.Delete(added)
		Eventually(c.getDeleted, 2).Should(Equal(1))

		wf.RemoveServiceHandler(h)
	})

	It("responds to egressFirewall add/update/delete events", func() {
		wf, err = NewMasterWatchFactory(ovnClientset)
		Expect(err).NotTo(HaveOccurred())
		err = wf.Start()
		Expect(err).NotTo(HaveOccurred())

		added := newEgressFirewall("myEgressFirewall", "default")
		h, c := addHandler(wf, EgressFirewallType, cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				egressFirewall := obj.(*egressfirewall.EgressFirewall)
				Expect(reflect.DeepEqual(egressFirewall, added)).To(BeTrue())
			},
			UpdateFunc: func(old, new interface{}) {
				newEgressFirewall := new.(*egressfirewall.EgressFirewall)
				Expect(reflect.DeepEqual(newEgressFirewall, added)).To(BeTrue())
				Expect(newEgressFirewall.Spec.Egress[0].Type).To(Equal(egressfirewall.EgressFirewallRuleDeny))
			},
			DeleteFunc: func(obj interface{}) {
				egressFirewall := obj.(*egressfirewall.EgressFirewall)
				Expect(reflect.DeepEqual(egressFirewall, added)).To(BeTrue())
			},
		})

		egressFirewalls = append(egressFirewalls, added)
		egressFirewallWatch.Add(added)
		Eventually(c.getAdded, 2).Should(Equal(1))
		added.Spec.Egress[0].Type = egressfirewall.EgressFirewallRuleDeny
		egressFirewallWatch.Modify(added)
		Eventually(c.getUpdated, 2).Should(Equal(1))
		egressFirewalls = egressFirewalls[:0]
		egressFirewallWatch.Delete(added)
		Eventually(c.getDeleted, 2).Should(Equal(1))

		wf.RemoveEgressFirewallHandler(h)
	})
	It("responds to egressIP add/update/delete events", func() {
		wf, err = NewMasterWatchFactory(ovnClientset)
		Expect(err).NotTo(HaveOccurred())
		err = wf.Start()
		Expect(err).NotTo(HaveOccurred())

		added := newEgressIP("myEgressIP", "default")
		h, c := addHandler(wf, EgressIPType, cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				egressIP := obj.(*egressip.EgressIP)
				Expect(reflect.DeepEqual(egressIP, added)).To(BeTrue())
			},
			UpdateFunc: func(old, new interface{}) {
				newEgressIP := new.(*egressip.EgressIP)
				Expect(reflect.DeepEqual(newEgressIP, added)).To(BeTrue())
				Expect(newEgressIP.Spec.EgressIPs).To(Equal([]string{"192.168.126.10"}))
			},
			DeleteFunc: func(obj interface{}) {
				egressIP := obj.(*egressip.EgressIP)
				Expect(reflect.DeepEqual(egressIP, added)).To(BeTrue())
			},
		})

		egressIPs = append(egressIPs, added)
		egressIPWatch.Add(added)
		Eventually(c.getAdded, 2).Should(Equal(1))
		added.Spec.EgressIPs = []string{"192.168.126.10"}
		egressIPWatch.Modify(added)
		Eventually(c.getUpdated, 2).Should(Equal(1))
		egressIPs = egressIPs[:0]
		egressIPWatch.Delete(added)
		Eventually(c.getDeleted, 2).Should(Equal(1))

		wf.RemoveEgressIPHandler(h)
	})
	It("responds to cloudPrivateIPConfig add/update/delete events", func() {
		wf, err = NewMasterWatchFactory(ovnClientset)
		Expect(err).NotTo(HaveOccurred())
		err = wf.Start()
		Expect(err).NotTo(HaveOccurred())

		added := newCloudPrivateIPConfig("192.168.126.25")
		h, c := addHandler(wf, CloudPrivateIPConfigType, cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				cloudPrivateIPConfig := obj.(*ocpcloudnetworkapi.CloudPrivateIPConfig)
				Expect(reflect.DeepEqual(cloudPrivateIPConfig, added)).To(BeTrue())
			},
			UpdateFunc: func(old, new interface{}) {
				newCloudPrivateIPConfig := new.(*ocpcloudnetworkapi.CloudPrivateIPConfig)
				Expect(reflect.DeepEqual(newCloudPrivateIPConfig, added)).To(BeTrue())
				Expect(newCloudPrivateIPConfig.Name).To(Equal("192.168.126.25"))
			},
			DeleteFunc: func(obj interface{}) {
				cloudPrivateIPConfig := obj.(*ocpcloudnetworkapi.CloudPrivateIPConfig)
				Expect(reflect.DeepEqual(cloudPrivateIPConfig, added)).To(BeTrue())
			},
		})

		cloudPrivateIPConfigs = append(cloudPrivateIPConfigs, added)
		cloudPrivateIPConfigWatch.Add(added)
		Eventually(c.getAdded, 2).Should(Equal(1))
		added.Spec.Node = "nodeA"
		cloudPrivateIPConfigWatch.Modify(added)
		Eventually(c.getUpdated, 2).Should(Equal(1))
		cloudPrivateIPConfigs = cloudPrivateIPConfigs[:0]
		cloudPrivateIPConfigWatch.Delete(added)
		Eventually(c.getDeleted, 2).Should(Equal(1))

		wf.RemoveCloudPrivateIPConfigHandler(h)
	})
	It("responds to egressQoS add/update/delete events", func() {
		wf, err = NewMasterWatchFactory(ovnClientset)
		Expect(err).NotTo(HaveOccurred())
		err = wf.Start()
		Expect(err).NotTo(HaveOccurred())

		added := newEgressQoS("myEgressQoS", "default")
		h, c := addHandler(wf, EgressQoSType, cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				egressQoS := obj.(*egressqos.EgressQoS)
				Expect(reflect.DeepEqual(egressQoS, added)).To(BeTrue())
			},
			UpdateFunc: func(old, new interface{}) {
				newEgressQoS := new.(*egressqos.EgressQoS)
				Expect(reflect.DeepEqual(newEgressQoS, added)).To(BeTrue())
				Expect(newEgressQoS.Spec.Egress[0].DSCP).To(Equal(40))
			},
			DeleteFunc: func(obj interface{}) {
				egressQoS := obj.(*egressqos.EgressQoS)
				Expect(reflect.DeepEqual(egressQoS, added)).To(BeTrue())
			},
		})

		egressQoSes = append(egressQoSes, added)
		egressQoSWatch.Add(added)
		Eventually(c.getAdded, 2).Should(Equal(1))
		added.Spec.Egress[0].DSCP = 40
		egressQoSWatch.Modify(added)
		Eventually(c.getUpdated, 2).Should(Equal(1))
		egressQoSes = egressQoSes[:0]
		egressQoSWatch.Delete(added)
		Eventually(c.getDeleted, 2).Should(Equal(1))

		wf.RemoveEgressQoSHandler(h)
	})
	It("responds to egressService add/update/delete events", func() {
		wf, err = NewMasterWatchFactory(ovnClientset)
		Expect(err).NotTo(HaveOccurred())
		err = wf.Start()
		Expect(err).NotTo(HaveOccurred())

		added := newEgressService("myEgressService", "default")
		h, c := addHandler(wf, EgressServiceType, cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				egressService := obj.(*egressservice.EgressService)
				Expect(reflect.DeepEqual(egressService, added)).To(BeTrue())
			},
			UpdateFunc: func(old, new interface{}) {
				newEgressService := new.(*egressservice.EgressService)
				Expect(reflect.DeepEqual(newEgressService, added)).To(BeTrue())
				Expect(newEgressService.Spec.NodeSelector).To(Equal(metav1.LabelSelector{
					MatchLabels: map[string]string{
						"kubernetes.io/hostname": "node2",
					},
				}))
			},
			DeleteFunc: func(obj interface{}) {
				egressService := obj.(*egressservice.EgressService)
				Expect(reflect.DeepEqual(egressService, added)).To(BeTrue())
			},
		})

		egressServices = append(egressServices, added)
		egressServiceWatch.Add(added)
		Eventually(c.getAdded, 2).Should(Equal(1))
		added.Spec.NodeSelector = metav1.LabelSelector{
			MatchLabels: map[string]string{
				"kubernetes.io/hostname": "node2",
			},
		}
		egressServiceWatch.Modify(added)
		Eventually(c.getUpdated, 2).Should(Equal(1))
		egressServices = egressServices[:0]
		egressServiceWatch.Delete(added)
		Eventually(c.getDeleted, 2).Should(Equal(1))

		wf.RemoveEgressServiceHandler(h)
	})
	It("stops processing events after the handler is removed", func() {
		wf, err = NewMasterWatchFactory(ovnClientset)
		Expect(err).NotTo(HaveOccurred())
		err = wf.Start()
		Expect(err).NotTo(HaveOccurred())

		added := newNamespace("default")
		h, c := addHandler(wf, NamespaceType, cache.ResourceEventHandlerFuncs{
			AddFunc:    func(obj interface{}) {},
			UpdateFunc: func(old, new interface{}) {},
			DeleteFunc: func(obj interface{}) {},
		})

		namespaces = append(namespaces, added)
		namespaceWatch.Add(added)
		Eventually(c.getAdded, 2).Should(Equal(1))
		wf.RemoveNamespaceHandler(h)

		added2 := newNamespace("other")
		namespaces = append(namespaces, added2)
		namespaceWatch.Add(added2)
		Consistently(c.getAdded, 2).Should(Equal(1))

		added2.Status.Phase = v1.NamespaceTerminating
		namespaceWatch.Modify(added2)
		Consistently(c.getUpdated, 2).Should(Equal(0))
		namespaces = []*v1.Namespace{added}
		namespaceWatch.Delete(added2)
		Consistently(c.getDeleted, 2).Should(Equal(0))
	})

	It("filters correctly by label and namespace", func() {
		wf, err = NewMasterWatchFactory(ovnClientset)
		Expect(err).NotTo(HaveOccurred())
		err = wf.Start()
		Expect(err).NotTo(HaveOccurred())

		passesFilter := newPod("pod1", "default")
		passesFilter.ObjectMeta.Labels["blah"] = "foobar"
		failsFilter := newPod("pod2", "default")
		failsFilter.ObjectMeta.Labels["blah"] = "baz"
		failsFilter2 := newPod("pod3", "otherns")
		failsFilter2.ObjectMeta.Labels["blah"] = "foobar"

		sel, err := metav1.LabelSelectorAsSelector(
			&metav1.LabelSelector{
				MatchLabels: map[string]string{"blah": "foobar"},
			},
		)
		Expect(err).NotTo(HaveOccurred())

		_, c := addFilteredHandler(wf,
			PodType,
			PodType,
			"default",
			sel,
			cache.ResourceEventHandlerFuncs{
				AddFunc: func(obj interface{}) {
					pod := obj.(*v1.Pod)
					Expect(reflect.DeepEqual(pod, passesFilter)).To(BeTrue())
				},
				UpdateFunc: func(old, new interface{}) {
					newPod := new.(*v1.Pod)
					Expect(reflect.DeepEqual(newPod, passesFilter)).To(BeTrue())
				},
				DeleteFunc: func(obj interface{}) {
					pod := obj.(*v1.Pod)
					Expect(reflect.DeepEqual(pod, passesFilter)).To(BeTrue())
				},
			})

		pods = append(pods, passesFilter)
		podWatch.Add(passesFilter)
		Eventually(c.getAdded, 2).Should(Equal(1))

		// numAdded should remain 1
		pods = append(pods, failsFilter)
		podWatch.Add(failsFilter)
		Consistently(c.getAdded, 2).Should(Equal(1))

		// numAdded should remain 1
		pods = append(pods, failsFilter2)
		podWatch.Add(failsFilter2)
		Consistently(c.getAdded, 2).Should(Equal(1))

		passesFilter.Status.Phase = v1.PodFailed
		podWatch.Modify(passesFilter)
		Eventually(c.getUpdated, 2).Should(Equal(1))

		// numAdded should remain 1
		failsFilter.Status.Phase = v1.PodFailed
		podWatch.Modify(failsFilter)
		Consistently(c.getUpdated, 2).Should(Equal(1))

		failsFilter2.Status.Phase = v1.PodFailed
		podWatch.Modify(failsFilter2)
		Consistently(c.getUpdated, 2).Should(Equal(1))

		pods = []*v1.Pod{failsFilter, failsFilter2}
		podWatch.Delete(passesFilter)
		Eventually(c.getDeleted, 2).Should(Equal(1))
	})

	It("correctly handles object updates that cause filter changes", func() {
		wf, err = NewMasterWatchFactory(ovnClientset)
		Expect(err).NotTo(HaveOccurred())
		err = wf.Start()
		Expect(err).NotTo(HaveOccurred())

		pod := newPod("pod1", "default")
		pod.ObjectMeta.Labels["blah"] = "baz"

		sel, err := metav1.LabelSelectorAsSelector(
			&metav1.LabelSelector{
				MatchLabels: map[string]string{"blah": "foobar"},
			},
		)
		Expect(err).NotTo(HaveOccurred())

		equalPod := pod
		h, c := addFilteredHandler(wf,
			PodType,
			PodType,
			"default",
			sel,
			cache.ResourceEventHandlerFuncs{
				AddFunc: func(obj interface{}) {
					p := obj.(*v1.Pod)
					Expect(reflect.DeepEqual(p, equalPod)).To(BeTrue())
				},
				UpdateFunc: func(old, new interface{}) {},
				DeleteFunc: func(obj interface{}) {
					p := obj.(*v1.Pod)
					Expect(reflect.DeepEqual(p, equalPod)).To(BeTrue())
				},
			})

		pods = append(pods, pod)

		// Pod doesn't pass filter; shouldn't be added
		podWatch.Add(pod)
		Consistently(c.getAdded, 2).Should(Equal(0))

		// Update pod to pass filter; should be treated as add.  Need
		// to deep-copy pod when modifying because it's a pointer all
		// the way through when using FakeClient
		podCopy := pod.DeepCopy()
		podCopy.ObjectMeta.Labels["blah"] = "foobar"
		pods = []*v1.Pod{podCopy}
		equalPod = podCopy
		podWatch.Modify(podCopy)
		Eventually(c.getAdded, 2).Should(Equal(1))

		// Update pod to fail filter; should be treated as delete
		pod.ObjectMeta.Labels["blah"] = "baz"
		podWatch.Modify(pod)
		Eventually(c.getDeleted, 2).Should(Equal(1))
		Consistently(c.getAdded, 2).Should(Equal(1))
		Consistently(c.getUpdated, 2).Should(Equal(0))

		wf.RemovePodHandler(h)
	})
})
