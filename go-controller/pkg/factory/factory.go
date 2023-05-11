package factory

import (
	"fmt"
	"reflect"
	"sync/atomic"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	egressfirewallapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1"
	egressfirewallscheme "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/clientset/versioned/scheme"
	egressfirewallinformerfactory "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/informers/externalversions"
	egressfirewalllister "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/listers/egressfirewall/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	egressipapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	egressipscheme "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/clientset/versioned/scheme"
	egressipinformerfactory "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/informers/externalversions"
	egressiplister "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/listers/egressip/v1"

	ocpcloudnetworkapi "github.com/openshift/api/cloudnetwork/v1"
	ocpcloudnetworkinformerfactory "github.com/openshift/client-go/cloudnetwork/informers/externalversions"
	ocpcloudnetworklister "github.com/openshift/client-go/cloudnetwork/listers/cloudnetwork/v1"

	egressqosapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressqos/v1"
	egressqosscheme "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressqos/v1/apis/clientset/versioned/scheme"
	egressqosinformerfactory "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressqos/v1/apis/informers/externalversions"
	egressqosinformer "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressqos/v1/apis/informers/externalversions/egressqos/v1"

	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	nadscheme "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/scheme"

	mnpapi "github.com/k8snetworkplumbingwg/multi-networkpolicy/pkg/apis/k8s.cni.cncf.io/v1beta1"
	mnpscheme "github.com/k8snetworkplumbingwg/multi-networkpolicy/pkg/client/clientset/versioned/scheme"
	mnpinformerfactory "github.com/k8snetworkplumbingwg/multi-networkpolicy/pkg/client/informers/externalversions"
	mnplister "github.com/k8snetworkplumbingwg/multi-networkpolicy/pkg/client/listers/k8s.cni.cncf.io/v1beta1"

	egressserviceapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressservice/v1"
	egressservicescheme "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressservice/v1/apis/clientset/versioned/scheme"
	egressserviceinformerfactory "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressservice/v1/apis/informers/externalversions"
	egressserviceinformer "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressservice/v1/apis/informers/externalversions/egressservice/v1"

	kapi "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	knet "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	utilwait "k8s.io/apimachinery/pkg/util/wait"
	informerfactory "k8s.io/client-go/informers"
	v1coreinformers "k8s.io/client-go/informers/core/v1"
	discoveryinformers "k8s.io/client-go/informers/discovery/v1"
	"k8s.io/client-go/kubernetes"
	listers "k8s.io/client-go/listers/core/v1"
	discoverylisters "k8s.io/client-go/listers/discovery/v1"
	netlisters "k8s.io/client-go/listers/networking/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

// WatchFactory initializes and manages common kube watches
type WatchFactory struct {
	// Must be first member in the struct due to Golang ARM/x86 32-bit
	// requirements with atomic accesses
	handlerCounter uint64

	iFactory             informerfactory.SharedInformerFactory
	eipFactory           egressipinformerfactory.SharedInformerFactory
	efFactory            egressfirewallinformerfactory.SharedInformerFactory
	cpipcFactory         ocpcloudnetworkinformerfactory.SharedInformerFactory
	egressQoSFactory     egressqosinformerfactory.SharedInformerFactory
	mnpFactory           mnpinformerfactory.SharedInformerFactory
	egressServiceFactory egressserviceinformerfactory.SharedInformerFactory
	informers            map[reflect.Type]*informer

	stopChan chan struct{}
}

// WatchFactory implements the ObjectCacheInterface interface.
var _ ObjectCacheInterface = &WatchFactory{}

const (
	// resync time is 0, none of the resources being watched in ovn-kubernetes have
	// any race condition where a resync may be required e.g. cni executable on node watching for
	// events on pods and assuming that an 'ADD' event will contain the annotations put in by
	// ovnkube master (currently, it is just a 'get' loop)
	// the downside of making it tight (like 10 minutes) is needless spinning on all resources
	// However, AddEventHandlerWithResyncPeriod can specify a per handler resync period
	resyncInterval        = 0
	handlerAlive   uint32 = 0
	handlerDead    uint32 = 1

	// namespace, node, and pod handlers
	defaultNumEventQueues uint32 = 15

	// default priorities for various handlers (also the highest priority)
	defaultHandlerPriority int = 0
	// lowest priority among various handlers (See GetHandlerPriority for more information)
	minHandlerPriority int = 4
)

// types for dynamic handlers created when adding a network policy
type addressSetNamespaceAndPodSelector struct{}
type peerNamespaceSelector struct{}
type addressSetPodSelector struct{}
type localPodSelector struct{}

// types for handlers related to egress IP
type egressIPPod struct{}
type egressIPNamespace struct{}
type egressNode struct{}

// types for handlers related to egress Firewall
type egressFwNode struct{}

// types for handlers in use by ovn-k node
type namespaceExGw struct{}
type endpointSliceForStaleConntrackRemoval struct{}
type serviceForGateway struct{}
type endpointSliceForGateway struct{}
type serviceForFakeNodePortWatcher struct{} // only for unit tests

var (
	// Resource types used in ovnk master
	PodType                               reflect.Type = reflect.TypeOf(&kapi.Pod{})
	ServiceType                           reflect.Type = reflect.TypeOf(&kapi.Service{})
	EndpointSliceType                     reflect.Type = reflect.TypeOf(&discovery.EndpointSlice{})
	PolicyType                            reflect.Type = reflect.TypeOf(&knet.NetworkPolicy{})
	NamespaceType                         reflect.Type = reflect.TypeOf(&kapi.Namespace{})
	NodeType                              reflect.Type = reflect.TypeOf(&kapi.Node{})
	EgressFirewallType                    reflect.Type = reflect.TypeOf(&egressfirewallapi.EgressFirewall{})
	EgressIPType                          reflect.Type = reflect.TypeOf(&egressipapi.EgressIP{})
	EgressIPNamespaceType                 reflect.Type = reflect.TypeOf(&egressIPNamespace{})
	EgressIPPodType                       reflect.Type = reflect.TypeOf(&egressIPPod{})
	EgressNodeType                        reflect.Type = reflect.TypeOf(&egressNode{})
	EgressFwNodeType                      reflect.Type = reflect.TypeOf(&egressFwNode{})
	CloudPrivateIPConfigType              reflect.Type = reflect.TypeOf(&ocpcloudnetworkapi.CloudPrivateIPConfig{})
	EgressQoSType                         reflect.Type = reflect.TypeOf(&egressqosapi.EgressQoS{})
	EgressServiceType                     reflect.Type = reflect.TypeOf(&egressserviceapi.EgressService{})
	AddressSetNamespaceAndPodSelectorType reflect.Type = reflect.TypeOf(&addressSetNamespaceAndPodSelector{})
	PeerNamespaceSelectorType             reflect.Type = reflect.TypeOf(&peerNamespaceSelector{})
	AddressSetPodSelectorType             reflect.Type = reflect.TypeOf(&addressSetPodSelector{})
	LocalPodSelectorType                  reflect.Type = reflect.TypeOf(&localPodSelector{})
	NetworkAttachmentDefinitionType       reflect.Type = reflect.TypeOf(&nadapi.NetworkAttachmentDefinition{})
	MultiNetworkPolicyType                reflect.Type = reflect.TypeOf(&mnpapi.MultiNetworkPolicy{})

	// Resource types used in ovnk node
	NamespaceExGwType                         reflect.Type = reflect.TypeOf(&namespaceExGw{})
	EndpointSliceForStaleConntrackRemovalType reflect.Type = reflect.TypeOf(&endpointSliceForStaleConntrackRemoval{})
	ServiceForGatewayType                     reflect.Type = reflect.TypeOf(&serviceForGateway{})
	EndpointSliceForGatewayType               reflect.Type = reflect.TypeOf(&endpointSliceForGateway{})
	ServiceForFakeNodePortWatcherType         reflect.Type = reflect.TypeOf(&serviceForFakeNodePortWatcher{}) // only for unit tests
)

// NewMasterWatchFactory initializes a new watch factory for the network controller manager
// or network controller manager+cluster manager or network controller manager+node processes.
func NewMasterWatchFactory(ovnClientset *util.OVNMasterClientset) (*WatchFactory, error) {
	// resync time is 12 hours, none of the resources being watched in ovn-kubernetes have
	// any race condition where a resync may be required e.g. cni executable on node watching for
	// events on pods and assuming that an 'ADD' event will contain the annotations put in by
	// ovnkube master (currently, it is just a 'get' loop)
	// the downside of making it tight (like 10 minutes) is needless spinning on all resources
	// However, AddEventHandlerWithResyncPeriod can specify a per handler resync period
	wf := &WatchFactory{
		iFactory:             informerfactory.NewSharedInformerFactory(ovnClientset.KubeClient, resyncInterval),
		eipFactory:           egressipinformerfactory.NewSharedInformerFactory(ovnClientset.EgressIPClient, resyncInterval),
		efFactory:            egressfirewallinformerfactory.NewSharedInformerFactory(ovnClientset.EgressFirewallClient, resyncInterval),
		egressQoSFactory:     egressqosinformerfactory.NewSharedInformerFactory(ovnClientset.EgressQoSClient, resyncInterval),
		mnpFactory:           mnpinformerfactory.NewSharedInformerFactory(ovnClientset.MultiNetworkPolicyClient, resyncInterval),
		egressServiceFactory: egressserviceinformerfactory.NewSharedInformerFactory(ovnClientset.EgressServiceClient, resyncInterval),
		informers:            make(map[reflect.Type]*informer),
		stopChan:             make(chan struct{}),
	}

	if err := egressipapi.AddToScheme(egressipscheme.Scheme); err != nil {
		return nil, err
	}
	if err := egressfirewallapi.AddToScheme(egressfirewallscheme.Scheme); err != nil {
		return nil, err
	}
	if err := egressqosapi.AddToScheme(egressqosscheme.Scheme); err != nil {
		return nil, err
	}
	if err := egressserviceapi.AddToScheme(egressservicescheme.Scheme); err != nil {
		return nil, err
	}

	if err := nadapi.AddToScheme(nadscheme.Scheme); err != nil {
		return nil, err
	}

	if err := mnpapi.AddToScheme(mnpscheme.Scheme); err != nil {
		return nil, err
	}

	// For Services and Endpoints, pre-populate the shared Informer with one that
	// has a label selector excluding headless services.
	wf.iFactory.InformerFor(&kapi.Service{}, func(c kubernetes.Interface, resyncPeriod time.Duration) cache.SharedIndexInformer {
		return v1coreinformers.NewFilteredServiceInformer(
			c,
			kapi.NamespaceAll,
			resyncPeriod,
			cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
			noAlternateProxySelector())
	})

	wf.iFactory.InformerFor(&discovery.EndpointSlice{}, func(c kubernetes.Interface, resyncPeriod time.Duration) cache.SharedIndexInformer {
		return discoveryinformers.NewFilteredEndpointSliceInformer(
			c,
			kapi.NamespaceAll,
			resyncPeriod,
			cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
			withServiceNameAndNoHeadlessServiceSelector())
	})

	var err error
	// Create our informer-wrapper informer (and underlying shared informer) for types we need
	wf.informers[PodType], err = newQueuedInformer(PodType, wf.iFactory.Core().V1().Pods().Informer(), wf.stopChan,
		defaultNumEventQueues)
	if err != nil {
		return nil, err
	}
	wf.informers[ServiceType], err = newInformer(ServiceType, wf.iFactory.Core().V1().Services().Informer())
	if err != nil {
		return nil, err
	}
	wf.informers[PolicyType], err = newInformer(PolicyType, wf.iFactory.Networking().V1().NetworkPolicies().Informer())
	if err != nil {
		return nil, err
	}
	wf.informers[NamespaceType], err = newQueuedInformer(NamespaceType, wf.iFactory.Core().V1().Namespaces().Informer(),
		wf.stopChan, defaultNumEventQueues)
	if err != nil {
		return nil, err
	}
	wf.informers[NodeType], err = newQueuedInformer(NodeType, wf.iFactory.Core().V1().Nodes().Informer(), wf.stopChan,
		defaultNumEventQueues)
	if err != nil {
		return nil, err
	}
	wf.informers[EndpointSliceType], err = newInformer(EndpointSliceType, wf.iFactory.Discovery().V1().EndpointSlices().Informer())
	if err != nil {
		return nil, err
	}
	if config.OVNKubernetesFeature.EnableEgressIP {
		wf.informers[EgressIPType], err = newInformer(EgressIPType, wf.eipFactory.K8s().V1().EgressIPs().Informer())
		if err != nil {
			return nil, err
		}
	}
	if config.OVNKubernetesFeature.EnableEgressFirewall {
		wf.informers[EgressFirewallType], err = newInformer(EgressFirewallType, wf.efFactory.K8s().V1().EgressFirewalls().Informer())
		if err != nil {
			return nil, err
		}
	}
	if config.OVNKubernetesFeature.EnableEgressQoS {
		wf.informers[EgressQoSType], err = newInformer(EgressQoSType, wf.egressQoSFactory.K8s().V1().EgressQoSes().Informer())
		if err != nil {
			return nil, err
		}
	}
	if config.OVNKubernetesFeature.EnableEgressService {
		wf.informers[EgressServiceType], err = newInformer(EgressServiceType, wf.egressServiceFactory.K8s().V1().EgressServices().Informer())
		if err != nil {
			return nil, err
		}
	}

	if util.IsMultiNetworkPoliciesSupportEnabled() {
		wf.informers[MultiNetworkPolicyType], err = newInformer(MultiNetworkPolicyType, wf.mnpFactory.K8sCniCncfIo().V1beta1().MultiNetworkPolicies().Informer())
		if err != nil {
			return nil, err
		}
	}

	return wf, nil
}

// Start starts the factory and begins processing events
func (wf *WatchFactory) Start() error {
	wf.iFactory.Start(wf.stopChan)
	for oType, synced := range wf.iFactory.WaitForCacheSync(wf.stopChan) {
		if !synced {
			return fmt.Errorf("error in syncing cache for %v informer", oType)
		}
	}
	if config.OVNKubernetesFeature.EnableEgressIP && wf.eipFactory != nil {
		wf.eipFactory.Start(wf.stopChan)
		for oType, synced := range wf.eipFactory.WaitForCacheSync(wf.stopChan) {
			if !synced {
				return fmt.Errorf("error in syncing cache for %v informer", oType)
			}
		}
	}
	if config.OVNKubernetesFeature.EnableEgressFirewall && wf.efFactory != nil {
		wf.efFactory.Start(wf.stopChan)
		for oType, synced := range wf.efFactory.WaitForCacheSync(wf.stopChan) {
			if !synced {
				return fmt.Errorf("error in syncing cache for %v informer", oType)
			}
		}
	}
	if util.PlatformTypeIsEgressIPCloudProvider() && wf.cpipcFactory != nil {
		wf.cpipcFactory.Start(wf.stopChan)
		for oType, synced := range wf.cpipcFactory.WaitForCacheSync(wf.stopChan) {
			if !synced {
				return fmt.Errorf("error in syncing cache for %v informer", oType)
			}
		}
	}
	if config.OVNKubernetesFeature.EnableEgressQoS && wf.egressQoSFactory != nil {
		wf.egressQoSFactory.Start(wf.stopChan)
		for oType, synced := range wf.egressQoSFactory.WaitForCacheSync(wf.stopChan) {
			if !synced {
				return fmt.Errorf("error in syncing cache for %v informer", oType)
			}
		}
	}

	if util.IsMultiNetworkPoliciesSupportEnabled() && wf.mnpFactory != nil {
		wf.mnpFactory.Start(wf.stopChan)
		for oType, synced := range wf.mnpFactory.WaitForCacheSync(wf.stopChan) {
			if !synced {
				return fmt.Errorf("error in syncing cache for %v informer", oType)
			}
		}
	}

	if config.OVNKubernetesFeature.EnableEgressService && wf.egressServiceFactory != nil {
		wf.egressServiceFactory.Start(wf.stopChan)
		for oType, synced := range wf.egressServiceFactory.WaitForCacheSync(wf.stopChan) {
			if !synced {
				return fmt.Errorf("error in syncing cache for %v informer", oType)
			}
		}
	}

	return nil
}

// NewNodeWatchFactory initializes a watch factory with significantly fewer
// informers to save memory + bandwidth. It is to be used by the node-only process.
func NewNodeWatchFactory(ovnClientset *util.OVNNodeClientset, nodeName string) (*WatchFactory, error) {
	wf := &WatchFactory{
		iFactory:             informerfactory.NewSharedInformerFactory(ovnClientset.KubeClient, resyncInterval),
		egressServiceFactory: egressserviceinformerfactory.NewSharedInformerFactory(ovnClientset.EgressServiceClient, resyncInterval),
		informers:            make(map[reflect.Type]*informer),
		stopChan:             make(chan struct{}),
	}

	if err := egressserviceapi.AddToScheme(egressservicescheme.Scheme); err != nil {
		return nil, err
	}

	var err error
	wf.informers[PodType], err = newQueuedInformer(PodType, wf.iFactory.Core().V1().Pods().Informer(), wf.stopChan,
		defaultNumEventQueues)
	if err != nil {
		return nil, err
	}

	// For Services and Endpoints, pre-populate the shared Informer with one that
	// has a label selector excluding headless services.
	wf.iFactory.InformerFor(&kapi.Service{}, func(c kubernetes.Interface, resyncPeriod time.Duration) cache.SharedIndexInformer {
		return v1coreinformers.NewFilteredServiceInformer(
			c,
			kapi.NamespaceAll,
			resyncPeriod,
			cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
			noAlternateProxySelector())
	})

	// For Pods, only select pods scheduled to this node
	wf.iFactory.InformerFor(&kapi.Pod{}, func(c kubernetes.Interface, resyncPeriod time.Duration) cache.SharedIndexInformer {
		return v1coreinformers.NewFilteredPodInformer(
			c,
			kapi.NamespaceAll,
			resyncPeriod,
			cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
			func(opts *metav1.ListOptions) {
				opts.FieldSelector = fields.OneTermEqualSelector("spec.nodeName", nodeName).String()
			})
	})

	// For namespaces
	wf.iFactory.InformerFor(&kapi.Namespace{}, func(c kubernetes.Interface, resyncPeriod time.Duration) cache.SharedIndexInformer {
		return v1coreinformers.NewNamespaceInformer(
			c,
			resyncPeriod,
			cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	})

	wf.iFactory.InformerFor(&discovery.EndpointSlice{}, func(c kubernetes.Interface, resyncPeriod time.Duration) cache.SharedIndexInformer {
		return discoveryinformers.NewFilteredEndpointSliceInformer(
			c,
			kapi.NamespaceAll,
			resyncPeriod,
			cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
			withServiceNameAndNoHeadlessServiceSelector())
	})

	wf.informers[NamespaceType], err = newInformer(NamespaceType, wf.iFactory.Core().V1().Namespaces().Informer())
	if err != nil {
		return nil, err
	}
	wf.informers[PodType], err = newQueuedInformer(PodType, wf.iFactory.Core().V1().Pods().Informer(), wf.stopChan,
		defaultNumEventQueues)
	if err != nil {
		return nil, err
	}
	wf.informers[ServiceType], err = newInformer(
		ServiceType,
		wf.iFactory.Core().V1().Services().Informer())
	if err != nil {
		return nil, err
	}
	wf.informers[EndpointSliceType], err = newInformer(
		EndpointSliceType,
		wf.iFactory.Discovery().V1().EndpointSlices().Informer())
	if err != nil {
		return nil, err
	}

	wf.informers[NodeType], err = newInformer(NodeType, wf.iFactory.Core().V1().Nodes().Informer())
	if err != nil {
		return nil, err
	}

	if config.OVNKubernetesFeature.EnableEgressService {
		wf.informers[EgressServiceType], err = newInformer(EgressServiceType, wf.egressServiceFactory.K8s().V1().EgressServices().Informer())
		if err != nil {
			return nil, err
		}
	}

	return wf, nil
}

// NewClusterManagerWatchFactory initializes a watch factory with significantly fewer
// informers to save memory + bandwidth. It is to be used by the cluster manager only
// mode process.
func NewClusterManagerWatchFactory(ovnClientset *util.OVNClusterManagerClientset) (*WatchFactory, error) {
	wf := &WatchFactory{
		iFactory:             informerfactory.NewSharedInformerFactory(ovnClientset.KubeClient, resyncInterval),
		eipFactory:           egressipinformerfactory.NewSharedInformerFactory(ovnClientset.EgressIPClient, resyncInterval),
		cpipcFactory:         ocpcloudnetworkinformerfactory.NewSharedInformerFactory(ovnClientset.CloudNetworkClient, resyncInterval),
		egressServiceFactory: egressserviceinformerfactory.NewSharedInformerFactoryWithOptions(ovnClientset.EgressServiceClient, resyncInterval),
		informers:            make(map[reflect.Type]*informer),
		stopChan:             make(chan struct{}),
	}
	if err := egressipapi.AddToScheme(egressipscheme.Scheme); err != nil {
		return nil, err
	}

	if err := egressserviceapi.AddToScheme(egressservicescheme.Scheme); err != nil {
		return nil, err
	}

	// For Services and Endpoints, pre-populate the shared Informer with one that
	// has a label selector excluding headless services.
	wf.iFactory.InformerFor(&kapi.Service{}, func(c kubernetes.Interface, resyncPeriod time.Duration) cache.SharedIndexInformer {
		return v1coreinformers.NewFilteredServiceInformer(
			c,
			kapi.NamespaceAll,
			resyncPeriod,
			cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
			noAlternateProxySelector())
	})

	wf.iFactory.InformerFor(&discovery.EndpointSlice{}, func(c kubernetes.Interface, resyncPeriod time.Duration) cache.SharedIndexInformer {
		return discoveryinformers.NewFilteredEndpointSliceInformer(
			c,
			kapi.NamespaceAll,
			resyncPeriod,
			cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
			withServiceNameAndNoHeadlessServiceSelector())
	})

	var err error
	// Create our informer-wrapper informer (and underlying shared informer) for types we need
	wf.informers[ServiceType], err = newInformer(ServiceType, wf.iFactory.Core().V1().Services().Informer())
	if err != nil {
		return nil, err
	}

	wf.informers[EndpointSliceType], err = newInformer(
		EndpointSliceType,
		wf.iFactory.Discovery().V1().EndpointSlices().Informer())
	if err != nil {
		return nil, err
	}

	wf.informers[NodeType], err = newInformer(NodeType, wf.iFactory.Core().V1().Nodes().Informer())
	if err != nil {
		return nil, err
	}
	if config.OVNKubernetesFeature.EnableEgressIP {
		wf.informers[EgressIPType], err = newInformer(EgressIPType, wf.eipFactory.K8s().V1().EgressIPs().Informer())
		if err != nil {
			return nil, err
		}
	}
	if util.PlatformTypeIsEgressIPCloudProvider() {
		wf.informers[CloudPrivateIPConfigType], err = newInformer(CloudPrivateIPConfigType, wf.cpipcFactory.Cloud().V1().CloudPrivateIPConfigs().Informer())
		if err != nil {
			return nil, err
		}
	}

	if config.OVNKubernetesFeature.EnableEgressService {
		wf.informers[EgressServiceType], err = newInformer(EgressServiceType, wf.egressServiceFactory.K8s().V1().EgressServices().Informer())
		if err != nil {
			return nil, err
		}
	}

	return wf, nil
}

func (wf *WatchFactory) Shutdown() {
	close(wf.stopChan)

	// Remove all informer handlers and wait for them to terminate before continuing
	for _, inf := range wf.informers {
		inf.shutdown()
	}
}

func getObjectMeta(objType reflect.Type, obj interface{}) (*metav1.ObjectMeta, error) {
	switch objType {
	case PodType:
		if pod, ok := obj.(*kapi.Pod); ok {
			return &pod.ObjectMeta, nil
		}
	case ServiceType:
		if service, ok := obj.(*kapi.Service); ok {
			return &service.ObjectMeta, nil
		}
	case PolicyType:
		if policy, ok := obj.(*knet.NetworkPolicy); ok {
			return &policy.ObjectMeta, nil
		}
	case NamespaceType:
		if namespace, ok := obj.(*kapi.Namespace); ok {
			return &namespace.ObjectMeta, nil
		}
	case NodeType:
		if node, ok := obj.(*kapi.Node); ok {
			return &node.ObjectMeta, nil
		}
	case EgressFirewallType:
		if egressFirewall, ok := obj.(*egressfirewallapi.EgressFirewall); ok {
			return &egressFirewall.ObjectMeta, nil
		}
	case EgressIPType:
		if egressIP, ok := obj.(*egressipapi.EgressIP); ok {
			return &egressIP.ObjectMeta, nil
		}
	case CloudPrivateIPConfigType:
		if cloudPrivateIPConfig, ok := obj.(*ocpcloudnetworkapi.CloudPrivateIPConfig); ok {
			return &cloudPrivateIPConfig.ObjectMeta, nil
		}
	case EndpointSliceType:
		if endpointSlice, ok := obj.(*discovery.EndpointSlice); ok {
			return &endpointSlice.ObjectMeta, nil
		}
	case NetworkAttachmentDefinitionType:
		if networkAttachmentDefinition, ok := obj.(*nadapi.NetworkAttachmentDefinition); ok {
			return &networkAttachmentDefinition.ObjectMeta, nil
		}
	case MultiNetworkPolicyType:
		if multinetworkpolicy, ok := obj.(*mnpapi.MultiNetworkPolicy); ok {
			return &multinetworkpolicy.ObjectMeta, nil
		}
	}
	return nil, fmt.Errorf("cannot get ObjectMeta from type %v", objType)
}

type AddHandlerFuncType func(namespace string, sel labels.Selector, funcs cache.ResourceEventHandler, processExisting func([]interface{}) error) (*Handler, error)

// GetHandlerPriority returns the priority of each objType's handler
// Priority of the handler is what determine which handler would get an event first
// This is relevant only for handlers that are sharing the same resources:
// Pods: shared by PodType (0), EgressIPPodType (1), AddressSetPodSelectorType (2), LocalPodSelectorType (3)
// Namespaces: shared by NamespaceType (0), EgressIPNamespaceType (1), PeerNamespaceSelectorType (3), AddressSetNamespaceAndPodSelectorType (4)
// Nodes: shared by NodeType (0), EgressNodeType (1), EgressFwNodeType (1)
// By default handlers get the defaultHandlerPriority which is 0 (highest priority). Higher the number, lower the priority to get an event.
// Example: EgressIPPodType will always get the pod event after PodType and AddressSetPodSelectorType will always get the event after PodType and EgressIPPodType
// NOTE: If you are touching this function to add a new object type that uses shared objects, please make sure to update `minHandlerPriority` if needed
func (wf *WatchFactory) GetHandlerPriority(objType reflect.Type) (priority int) {
	switch objType {
	case EgressIPPodType:
		return 1
	case AddressSetPodSelectorType:
		return 2
	case LocalPodSelectorType:
		return 3
	case EgressIPNamespaceType:
		return 1
	case PeerNamespaceSelectorType:
		return 2
	case AddressSetNamespaceAndPodSelectorType:
		return 3
	case EgressNodeType:
		return 1
	case EgressFwNodeType:
		return 1
	default:
		return defaultHandlerPriority
	}
}

func (wf *WatchFactory) GetResourceHandlerFunc(objType reflect.Type) (AddHandlerFuncType, error) {
	priority := wf.GetHandlerPriority(objType)
	switch objType {
	case NamespaceType, NamespaceExGwType:
		return func(namespace string, sel labels.Selector,
			funcs cache.ResourceEventHandler, processExisting func([]interface{}) error) (*Handler, error) {
			return wf.AddNamespaceHandler(funcs, processExisting)
		}, nil

	case PolicyType:
		return func(namespace string, sel labels.Selector,
			funcs cache.ResourceEventHandler, processExisting func([]interface{}) error) (*Handler, error) {
			return wf.AddPolicyHandler(funcs, processExisting)
		}, nil

	case MultiNetworkPolicyType:
		return func(namespace string, sel labels.Selector,
			funcs cache.ResourceEventHandler, processExisting func([]interface{}) error) (*Handler, error) {
			return wf.AddMultiNetworkPolicyHandler(funcs, processExisting)
		}, nil

	case NodeType, EgressNodeType, EgressFwNodeType:
		return func(namespace string, sel labels.Selector,
			funcs cache.ResourceEventHandler, processExisting func([]interface{}) error) (*Handler, error) {
			return wf.AddNodeHandler(funcs, processExisting, priority)
		}, nil

	case ServiceForGatewayType, ServiceForFakeNodePortWatcherType:
		return func(namespace string, sel labels.Selector,
			funcs cache.ResourceEventHandler, processExisting func([]interface{}) error) (*Handler, error) {
			return wf.AddFilteredServiceHandler(namespace, funcs, processExisting)
		}, nil

	case AddressSetPodSelectorType, LocalPodSelectorType, PodType, EgressIPPodType:
		return func(namespace string, sel labels.Selector,
			funcs cache.ResourceEventHandler, processExisting func([]interface{}) error) (*Handler, error) {
			return wf.AddFilteredPodHandler(namespace, sel, funcs, processExisting, priority)
		}, nil

	case AddressSetNamespaceAndPodSelectorType, PeerNamespaceSelectorType, EgressIPNamespaceType:
		return func(namespace string, sel labels.Selector,
			funcs cache.ResourceEventHandler, processExisting func([]interface{}) error) (*Handler, error) {
			return wf.AddFilteredNamespaceHandler(namespace, sel, funcs, processExisting, priority)
		}, nil

	case EgressFirewallType:
		return func(namespace string, sel labels.Selector,
			funcs cache.ResourceEventHandler, processExisting func([]interface{}) error) (*Handler, error) {
			return wf.AddEgressFirewallHandler(funcs, processExisting)
		}, nil

	case EgressIPType:
		return func(namespace string, sel labels.Selector,
			funcs cache.ResourceEventHandler, processExisting func([]interface{}) error) (*Handler, error) {
			return wf.AddEgressIPHandler(funcs, processExisting)
		}, nil

	case CloudPrivateIPConfigType:
		return func(namespace string, sel labels.Selector,
			funcs cache.ResourceEventHandler, processExisting func([]interface{}) error) (*Handler, error) {
			return wf.AddCloudPrivateIPConfigHandler(funcs, processExisting)
		}, nil

	case EndpointSliceForStaleConntrackRemovalType, EndpointSliceForGatewayType:
		return func(namespace string, sel labels.Selector,
			funcs cache.ResourceEventHandler, processExisting func([]interface{}) error) (*Handler, error) {
			return wf.AddEndpointSliceHandler(funcs, processExisting)
		}, nil

	}
	return nil, fmt.Errorf("cannot get ObjectMeta from type %v", objType)
}

func (wf *WatchFactory) addHandler(objType reflect.Type, namespace string, sel labels.Selector, funcs cache.ResourceEventHandler, processExisting func([]interface{}) error, priority int) (*Handler, error) {
	inf, ok := wf.informers[objType]
	if !ok {
		klog.Fatalf("Tried to add handler of unknown object type %v", objType)
	}

	filterFunc := func(obj interface{}) bool {
		if namespace == "" && sel == nil {
			// Unfiltered handler
			return true
		}
		meta, err := getObjectMeta(objType, obj)
		if err != nil {
			klog.Errorf("Watch handler filter error: %v", err)
			return false
		}
		if namespace != "" && meta.Namespace != namespace {
			return false
		}
		if sel != nil && !sel.Matches(labels.Set(meta.Labels)) {
			return false
		}
		return true
	}

	inf.Lock()
	defer inf.Unlock()

	items := make([]interface{}, 0)
	for _, obj := range inf.inf.GetStore().List() {
		if filterFunc(obj) {
			items = append(items, obj)
		}
	}
	if processExisting != nil {
		// Process existing items as a set so the caller can clean up
		// after a restart or whatever. We will wrap it with retries to ensure it succeeds.
		// Being so, processExisting is expected to be idem-potent!
		err := utilwait.PollImmediate(500*time.Millisecond, 60*time.Second, func() (bool, error) {
			if err := processExisting(items); err != nil {
				klog.Errorf("Failed (will retry) in processExisting %v: %v", items, err)
				return false, nil
			}
			return true, nil
		})
		if err != nil {
			return nil, err
		}
	}

	handlerID := atomic.AddUint64(&wf.handlerCounter, 1)
	handler := inf.addHandler(handlerID, priority, filterFunc, funcs, items)
	klog.V(5).Infof("Added %v event handler %d", objType, handler.id)
	return handler, nil
}

func (wf *WatchFactory) removeHandler(objType reflect.Type, handler *Handler) {
	wf.informers[objType].removeHandler(handler)
}

// AddPodHandler adds a handler function that will be executed on Pod object changes
func (wf *WatchFactory) AddPodHandler(handlerFuncs cache.ResourceEventHandler, processExisting func([]interface{}) error) (*Handler, error) {
	return wf.addHandler(PodType, "", nil, handlerFuncs, processExisting, defaultHandlerPriority)
}

// AddFilteredPodHandler adds a handler function that will be executed when Pod objects that match the given filters change
func (wf *WatchFactory) AddFilteredPodHandler(namespace string, sel labels.Selector, handlerFuncs cache.ResourceEventHandler, processExisting func([]interface{}) error, priority int) (*Handler, error) {
	return wf.addHandler(PodType, namespace, sel, handlerFuncs, processExisting, priority)
}

// RemovePodHandler removes a Pod object event handler function
func (wf *WatchFactory) RemovePodHandler(handler *Handler) {
	wf.removeHandler(PodType, handler)
}

// AddServiceHandler adds a handler function that will be executed on Service object changes
func (wf *WatchFactory) AddServiceHandler(handlerFuncs cache.ResourceEventHandler, processExisting func([]interface{}) error) (*Handler, error) {
	return wf.addHandler(ServiceType, "", nil, handlerFuncs, processExisting, defaultHandlerPriority)
}

// AddFilteredServiceHandler adds a handler function that will be executed on all Service object changes for a specific namespace
func (wf *WatchFactory) AddFilteredServiceHandler(namespace string, handlerFuncs cache.ResourceEventHandler, processExisting func([]interface{}) error) (*Handler, error) {
	return wf.addHandler(ServiceType, namespace, nil, handlerFuncs, processExisting, defaultHandlerPriority)
}

// RemoveServiceHandler removes a Service object event handler function
func (wf *WatchFactory) RemoveServiceHandler(handler *Handler) {
	wf.removeHandler(ServiceType, handler)
}

// AddEndpointSliceHandler adds a handler function that will be executed on EndpointSlice object changes
func (wf *WatchFactory) AddEndpointSliceHandler(handlerFuncs cache.ResourceEventHandler, processExisting func([]interface{}) error) (*Handler, error) {
	return wf.addHandler(EndpointSliceType, "", nil, handlerFuncs, processExisting, defaultHandlerPriority)
}

// RemoveEndpointSliceHandler removes a EndpointSlice object event handler function
func (wf *WatchFactory) RemoveEndpointSliceHandler(handler *Handler) {
	wf.removeHandler(EndpointSliceType, handler)
}

// AddPolicyHandler adds a handler function that will be executed on NetworkPolicy object changes
func (wf *WatchFactory) AddPolicyHandler(handlerFuncs cache.ResourceEventHandler, processExisting func([]interface{}) error) (*Handler, error) {
	return wf.addHandler(PolicyType, "", nil, handlerFuncs, processExisting, defaultHandlerPriority)
}

// RemovePolicyHandler removes a NetworkPolicy object event handler function
func (wf *WatchFactory) RemovePolicyHandler(handler *Handler) {
	wf.removeHandler(PolicyType, handler)
}

// AddEgressFirewallHandler adds a handler function that will be executed on EgressFirewall object changes
func (wf *WatchFactory) AddEgressFirewallHandler(handlerFuncs cache.ResourceEventHandler, processExisting func([]interface{}) error) (*Handler, error) {
	return wf.addHandler(EgressFirewallType, "", nil, handlerFuncs, processExisting, defaultHandlerPriority)
}

// RemoveEgressFirewallHandler removes an EgressFirewall object event handler function
func (wf *WatchFactory) RemoveEgressFirewallHandler(handler *Handler) {
	wf.removeHandler(EgressFirewallType, handler)
}

// RemoveEgressQoSHandler removes an EgressQoS object event handler function
func (wf *WatchFactory) RemoveEgressQoSHandler(handler *Handler) {
	wf.removeHandler(EgressQoSType, handler)
}

func (wf *WatchFactory) RemoveEgressServiceHandler(handler *Handler) {
	wf.removeHandler(EgressServiceType, handler)
}

// AddNetworkAttachmentDefinitionHandler adds a handler function that will be executed on NetworkAttachmentDefinition object changes
func (wf *WatchFactory) AddNetworkAttachmentDefinitionHandler(handlerFuncs cache.ResourceEventHandler, processExisting func([]interface{}) error) (*Handler, error) {
	return wf.addHandler(NetworkAttachmentDefinitionType, "", nil, handlerFuncs, processExisting, defaultHandlerPriority)
}

// RemoveNetworkAttachmentDefinitionHandler removes an NetworkAttachmentDefinition object event handler function
func (wf *WatchFactory) RemoveNetworkAttachmentDefinitionHandler(handler *Handler) {
	wf.removeHandler(NetworkAttachmentDefinitionType, handler)
}

// AddEgressIPHandler adds a handler function that will be executed on EgressIP object changes
func (wf *WatchFactory) AddEgressIPHandler(handlerFuncs cache.ResourceEventHandler, processExisting func([]interface{}) error) (*Handler, error) {
	return wf.addHandler(EgressIPType, "", nil, handlerFuncs, processExisting, defaultHandlerPriority)
}

// RemoveEgressIPHandler removes an EgressIP object event handler function
func (wf *WatchFactory) RemoveEgressIPHandler(handler *Handler) {
	wf.removeHandler(EgressIPType, handler)
}

// AddCloudPrivateIPConfigHandler adds a handler function that will be executed on CloudPrivateIPConfig object changes
func (wf *WatchFactory) AddCloudPrivateIPConfigHandler(handlerFuncs cache.ResourceEventHandler, processExisting func([]interface{}) error) (*Handler, error) {
	return wf.addHandler(CloudPrivateIPConfigType, "", nil, handlerFuncs, processExisting, defaultHandlerPriority)
}

// RemoveCloudPrivateIPConfigHandler removes an CloudPrivateIPConfig object event handler function
func (wf *WatchFactory) RemoveCloudPrivateIPConfigHandler(handler *Handler) {
	wf.removeHandler(CloudPrivateIPConfigType, handler)
}

// AddMultiNetworkPolicyHandler adds a handler function that will be executed on MultiNetworkPolicy object changes
func (wf *WatchFactory) AddMultiNetworkPolicyHandler(handlerFuncs cache.ResourceEventHandler, processExisting func([]interface{}) error) (*Handler, error) {
	return wf.addHandler(MultiNetworkPolicyType, "", nil, handlerFuncs, processExisting, defaultHandlerPriority)
}

// RemoveMultiNetworkPolicyHandler removes an MultiNetworkPolicy object event handler function
func (wf *WatchFactory) RemoveMultiNetworkPolicyHandler(handler *Handler) {
	wf.removeHandler(MultiNetworkPolicyType, handler)
}

// AddNamespaceHandler adds a handler function that will be executed on Namespace object changes
func (wf *WatchFactory) AddNamespaceHandler(handlerFuncs cache.ResourceEventHandler, processExisting func([]interface{}) error) (*Handler, error) {
	return wf.addHandler(NamespaceType, "", nil, handlerFuncs, processExisting, defaultHandlerPriority)
}

// AddFilteredNamespaceHandler adds a handler function that will be executed when Namespace objects that match the given filters change
func (wf *WatchFactory) AddFilteredNamespaceHandler(namespace string, sel labels.Selector, handlerFuncs cache.ResourceEventHandler, processExisting func([]interface{}) error, priority int) (*Handler, error) {
	return wf.addHandler(NamespaceType, namespace, sel, handlerFuncs, processExisting, priority)
}

// RemoveNamespaceHandler removes a Namespace object event handler function
func (wf *WatchFactory) RemoveNamespaceHandler(handler *Handler) {
	wf.removeHandler(NamespaceType, handler)
}

// AddNodeHandler adds a handler function that will be executed on Node object changes
func (wf *WatchFactory) AddNodeHandler(handlerFuncs cache.ResourceEventHandler, processExisting func([]interface{}) error, priority int) (*Handler, error) {
	return wf.addHandler(NodeType, "", nil, handlerFuncs, processExisting, priority)
}

// AddFilteredNodeHandler dds a handler function that will be executed when Node objects that match the given label selector
func (wf *WatchFactory) AddFilteredNodeHandler(sel labels.Selector, handlerFuncs cache.ResourceEventHandler, processExisting func([]interface{}) error) (*Handler, error) {
	return wf.addHandler(NodeType, "", sel, handlerFuncs, processExisting, defaultHandlerPriority)
}

// RemoveNodeHandler removes a Node object event handler function
func (wf *WatchFactory) RemoveNodeHandler(handler *Handler) {
	wf.removeHandler(NodeType, handler)
}

// GetPod returns the pod spec given the namespace and pod name
func (wf *WatchFactory) GetPod(namespace, name string) (*kapi.Pod, error) {
	podLister := wf.informers[PodType].lister.(listers.PodLister)
	return podLister.Pods(namespace).Get(name)
}

// GetAllPods returns all the pods in the cluster
func (wf *WatchFactory) GetAllPods() ([]*kapi.Pod, error) {
	podLister := wf.informers[PodType].lister.(listers.PodLister)
	return podLister.List(labels.Everything())
}

// GetPods returns all the pods in a given namespace
func (wf *WatchFactory) GetPods(namespace string) ([]*kapi.Pod, error) {
	podLister := wf.informers[PodType].lister.(listers.PodLister)
	return podLister.Pods(namespace).List(labels.Everything())
}

// GetPodsBySelector returns all the pods in a given namespace by the label selector
func (wf *WatchFactory) GetPodsBySelector(namespace string, labelSelector metav1.LabelSelector) ([]*kapi.Pod, error) {
	podLister := wf.informers[PodType].lister.(listers.PodLister)
	selector, err := metav1.LabelSelectorAsSelector(&labelSelector)
	if err != nil {
		return nil, err
	}
	return podLister.Pods(namespace).List(selector)
}

// GetNodes returns the node specs of all the nodes
func (wf *WatchFactory) GetNodes() ([]*kapi.Node, error) {
	return wf.ListNodes(labels.Everything())
}

// ListNodes returns nodes that match a selector
func (wf *WatchFactory) ListNodes(selector labels.Selector) ([]*kapi.Node, error) {
	nodeLister := wf.informers[NodeType].lister.(listers.NodeLister)
	return nodeLister.List(selector)
}

// GetNode returns the node spec of a given node by name
func (wf *WatchFactory) GetNode(name string) (*kapi.Node, error) {
	nodeLister := wf.informers[NodeType].lister.(listers.NodeLister)
	return nodeLister.Get(name)
}

// GetNodesByLabelSelector returns all the nodes selected by the label selector
func (wf *WatchFactory) GetNodesByLabelSelector(labelSelector metav1.LabelSelector) ([]*kapi.Node, error) {
	selector, err := metav1.LabelSelectorAsSelector(&labelSelector)
	if err != nil {
		return nil, err
	}
	return wf.GetNodesBySelector(selector)
}

// GetNodesBySelector returns all the nodes selected by the selector
func (wf *WatchFactory) GetNodesBySelector(selector labels.Selector) ([]*kapi.Node, error) {
	return wf.ListNodes(selector)
}

// GetService returns the service spec of a service in a given namespace
func (wf *WatchFactory) GetService(namespace, name string) (*kapi.Service, error) {
	serviceLister := wf.informers[ServiceType].lister.(listers.ServiceLister)
	return serviceLister.Services(namespace).Get(name)
}

// GetServices returns all services
func (wf *WatchFactory) GetServices() ([]*kapi.Service, error) {
	serviceLister := wf.informers[ServiceType].lister.(listers.ServiceLister)
	return serviceLister.List(labels.Everything())
}

func (wf *WatchFactory) GetCloudPrivateIPConfig(name string) (*ocpcloudnetworkapi.CloudPrivateIPConfig, error) {
	cloudPrivateIPConfigLister := wf.informers[CloudPrivateIPConfigType].lister.(ocpcloudnetworklister.CloudPrivateIPConfigLister)
	return cloudPrivateIPConfigLister.Get(name)
}

func (wf *WatchFactory) GetEgressIP(name string) (*egressipapi.EgressIP, error) {
	egressIPLister := wf.informers[EgressIPType].lister.(egressiplister.EgressIPLister)
	return egressIPLister.Get(name)
}

func (wf *WatchFactory) GetEgressIPs() ([]*egressipapi.EgressIP, error) {
	egressIPLister := wf.informers[EgressIPType].lister.(egressiplister.EgressIPLister)
	return egressIPLister.List(labels.Everything())
}

// GetNamespace returns a specific namespace
func (wf *WatchFactory) GetNamespace(name string) (*kapi.Namespace, error) {
	namespaceLister := wf.informers[NamespaceType].lister.(listers.NamespaceLister)
	return namespaceLister.Get(name)
}

// GetEndpointSlice returns the endpointSlice indexed by the given namespace and name
func (wf *WatchFactory) GetEndpointSlice(namespace, name string) (*discovery.EndpointSlice, error) {
	endpointSliceLister := wf.informers[EndpointSliceType].lister.(discoverylisters.EndpointSliceLister)
	return endpointSliceLister.EndpointSlices(namespace).Get(name)
}

// GetServiceEndpointSlice returns the endpointSlice associated with a service
func (wf *WatchFactory) GetEndpointSlices(namespace, svcName string) ([]*discovery.EndpointSlice, error) {
	esLabelSelector := labels.Set(map[string]string{
		discovery.LabelServiceName: svcName,
	}).AsSelectorPreValidated()
	endpointSliceLister := wf.informers[EndpointSliceType].lister.(discoverylisters.EndpointSliceLister)
	return endpointSliceLister.EndpointSlices(namespace).List(esLabelSelector)
}

// GetNamespaces returns a list of namespaces in the cluster
func (wf *WatchFactory) GetNamespaces() ([]*kapi.Namespace, error) {
	namespaceLister := wf.informers[NamespaceType].lister.(listers.NamespaceLister)
	return namespaceLister.List(labels.Everything())
}

// GetNamespacesBySelector returns a list of namespaces in the cluster by the label selector
func (wf *WatchFactory) GetNamespacesBySelector(labelSelector metav1.LabelSelector) ([]*kapi.Namespace, error) {
	namespaceLister := wf.informers[NamespaceType].lister.(listers.NamespaceLister)
	selector, err := metav1.LabelSelectorAsSelector(&labelSelector)
	if err != nil {
		return nil, err
	}
	return namespaceLister.List(selector)
}

// GetNetworkPolicy gets a specific network policy by the namespace/name
func (wf *WatchFactory) GetNetworkPolicy(namespace, name string) (*knet.NetworkPolicy, error) {
	networkPolicyLister := wf.informers[PolicyType].lister.(netlisters.NetworkPolicyLister)
	return networkPolicyLister.NetworkPolicies(namespace).Get(name)
}

// GetMultinetworkPolicy gets a specific multinetwork policy by the namespace/name
func (wf *WatchFactory) GetMultiNetworkPolicy(namespace, name string) (*mnpapi.MultiNetworkPolicy, error) {
	multinetworkPolicyLister := wf.informers[MultiNetworkPolicyType].lister.(mnplister.MultiNetworkPolicyLister)
	return multinetworkPolicyLister.MultiNetworkPolicies(namespace).Get(name)
}

func (wf *WatchFactory) GetEgressFirewall(namespace, name string) (*egressfirewallapi.EgressFirewall, error) {
	egressFirewallLister := wf.informers[EgressFirewallType].lister.(egressfirewalllister.EgressFirewallLister)
	return egressFirewallLister.EgressFirewalls(namespace).Get(name)
}

func (wf *WatchFactory) NodeInformer() cache.SharedIndexInformer {
	return wf.informers[NodeType].inf
}

func (wf *WatchFactory) NodeCoreInformer() v1coreinformers.NodeInformer {
	return wf.iFactory.Core().V1().Nodes()
}

// LocalPodInformer returns a shared Informer that may or may not only
// return pods running on the local node.
func (wf *WatchFactory) LocalPodInformer() cache.SharedIndexInformer {
	return wf.informers[PodType].inf
}

func (wf *WatchFactory) PodInformer() cache.SharedIndexInformer {
	return wf.informers[PodType].inf
}

func (wf *WatchFactory) PodCoreInformer() v1coreinformers.PodInformer {
	return wf.iFactory.Core().V1().Pods()
}

func (wf *WatchFactory) NamespaceInformer() v1coreinformers.NamespaceInformer {
	return wf.iFactory.Core().V1().Namespaces()
}

func (wf *WatchFactory) ServiceInformer() cache.SharedIndexInformer {
	return wf.informers[ServiceType].inf
}

func (wf *WatchFactory) EndpointSliceInformer() cache.SharedIndexInformer {
	return wf.informers[EndpointSliceType].inf
}

func (wf *WatchFactory) EgressQoSInformer() egressqosinformer.EgressQoSInformer {
	return wf.egressQoSFactory.K8s().V1().EgressQoSes()
}

func (wf *WatchFactory) EgressServiceInformer() egressserviceinformer.EgressServiceInformer {
	return wf.egressServiceFactory.K8s().V1().EgressServices()
}

// withServiceNameAndNoHeadlessServiceSelector returns a LabelSelector (added to the
// watcher for EndpointSlices) that will only choose EndpointSlices with a non-empty
// "kubernetes.io/service-name" label and without "service.kubernetes.io/headless"
// label.
func withServiceNameAndNoHeadlessServiceSelector() func(options *metav1.ListOptions) {
	// LabelServiceName must exist
	svcNameLabel, err := labels.NewRequirement(discovery.LabelServiceName, selection.Exists, nil)
	if err != nil {
		// cannot occur
		panic(err)
	}
	// LabelServiceName value must be non-empty
	notEmptySvcName, err := labels.NewRequirement(discovery.LabelServiceName, selection.NotEquals, []string{""})
	if err != nil {
		// cannot occur
		panic(err)
	}
	// headless service label must not be there
	noHeadlessService, err := labels.NewRequirement(kapi.IsHeadlessService, selection.DoesNotExist, nil)
	if err != nil {
		// cannot occur
		panic(err)
	}

	selector := labels.NewSelector().Add(*svcNameLabel, *notEmptySvcName, *noHeadlessService)

	return func(options *metav1.ListOptions) {
		options.LabelSelector = selector.String()
	}
}

// noAlternateProxySelector is a LabelSelector added to the watch for
// services that excludes services with a well-known label indicating
// proxying is via an alternate proxy.
// This matches the behavior of kube-proxy
func noAlternateProxySelector() func(options *metav1.ListOptions) {
	// if the proxy-name annotation is set, skip this service
	noProxyName, err := labels.NewRequirement("service.kubernetes.io/service-proxy-name", selection.DoesNotExist, nil)
	if err != nil {
		// cannot occur
		panic(err)
	}

	labelSelector := labels.NewSelector().Add(*noProxyName)

	return func(options *metav1.ListOptions) {
		options.LabelSelector = labelSelector.String()
	}
}

// WithUpdateHandlingForObjReplace decorates given cache.ResourceEventHandler with checking object
// replace case in the update event. when old and new object have different UIDs, then consider it
// as a replace and invoke delete handler for old object followed by add handler for new object.
func WithUpdateHandlingForObjReplace(funcs cache.ResourceEventHandler) cache.ResourceEventHandlerFuncs {
	return cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			funcs.OnAdd(obj)
		},
		UpdateFunc: func(old, new interface{}) {
			oldObj := old.(metav1.Object)
			newObj := new.(metav1.Object)
			if oldObj.GetUID() == newObj.GetUID() {
				funcs.OnUpdate(old, new)
				return
			}
			// This occurs not so often, so log this occurance.
			klog.Infof("Object %s/%s is replaced, invoking delete followed by add handler", newObj.GetNamespace(), newObj.GetName())
			funcs.OnDelete(old)
			funcs.OnAdd(new)
		},
		DeleteFunc: func(obj interface{}) {
			funcs.OnDelete(obj)
		},
	}
}
