package factory

import (
	adminpolicybasedrouteinformer "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1/apis/informers/externalversions/adminpolicybasedroute/v1"
	egressipinformer "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/informers/externalversions/egressip/v1"
	userdefinednetworkinformer "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/informers/externalversions/userdefinednetwork/v1"

	nadinformer "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/informers/externalversions/k8s.cni.cncf.io/v1"

	kapi "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/labels"
	coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/tools/cache"
)

// ObjectCacheInterface represents the exported methods for getting
// kubernetes resources from the informer cache
type ObjectCacheInterface interface {
	GetPod(namespace, name string) (*kapi.Pod, error)
	GetAllPods() ([]*kapi.Pod, error)
	GetPods(namespace string) ([]*kapi.Pod, error)
	GetNodes() ([]*kapi.Node, error)
	GetNode(name string) (*kapi.Node, error)
	GetService(namespace, name string) (*kapi.Service, error)
	GetServiceEndpointSlices(namespace, svcName, network string) ([]*discovery.EndpointSlice, error)
	GetNamespace(name string) (*kapi.Namespace, error)
	GetNamespaces() ([]*kapi.Namespace, error)
}

// NodeWatchFactory is an interface that ensures node components only use informers available in a
// node context; under the hood, it's all the same watchFactory.
//
// If you add a new method here, make sure the underlying informer is started
// in factory.go NewNodeWatchFactory
//
//go:generate mockery --name NodeWatchFactory
type NodeWatchFactory interface {
	Shutdownable

	Start() error

	AddServiceHandler(handlerFuncs cache.ResourceEventHandler, processExisting func([]interface{}) error) (*Handler, error)
	AddFilteredServiceHandler(namespace string, handlerFuncs cache.ResourceEventHandler, processExisting func([]interface{}) error) (*Handler, error)
	RemoveServiceHandler(handler *Handler)

	AddFilteredEndpointSliceHandler(namespace string, sel labels.Selector, handlerFuncs cache.ResourceEventHandler, processExisting func([]interface{}) error) (*Handler, error)
	RemoveEndpointSliceHandler(handler *Handler)

	AddPodHandler(handlerFuncs cache.ResourceEventHandler, processExisting func([]interface{}) error) (*Handler, error)
	RemovePodHandler(handler *Handler)

	AddNamespaceHandler(handlerFuncs cache.ResourceEventHandler, processExisting func([]interface{}) error) (*Handler, error)
	RemoveNamespaceHandler(handler *Handler)

	NodeInformer() cache.SharedIndexInformer
	NodeCoreInformer() coreinformers.NodeInformer
	LocalPodInformer() cache.SharedIndexInformer
	NamespaceInformer() coreinformers.NamespaceInformer
	PodCoreInformer() coreinformers.PodInformer
	APBRouteInformer() adminpolicybasedrouteinformer.AdminPolicyBasedExternalRouteInformer
	EgressIPInformer() egressipinformer.EgressIPInformer
	NADInformer() nadinformer.NetworkAttachmentDefinitionInformer
	UserDefinedNetworkInformer() userdefinednetworkinformer.UserDefinedNetworkInformer

	GetPods(namespace string) ([]*kapi.Pod, error)
	GetPod(namespace, name string) (*kapi.Pod, error)
	GetAllPods() ([]*kapi.Pod, error)
	GetNamespaces() ([]*kapi.Namespace, error)
	GetNode(name string) (*kapi.Node, error)
	GetNodes() ([]*kapi.Node, error)
	ListNodes(selector labels.Selector) ([]*kapi.Node, error)

	GetService(namespace, name string) (*kapi.Service, error)
	GetServices() ([]*kapi.Service, error)
	GetEndpointSlice(namespace, name string) (*discovery.EndpointSlice, error)
	GetServiceEndpointSlices(namespace, svcName, network string) ([]*discovery.EndpointSlice, error)

	GetNamespace(name string) (*kapi.Namespace, error)
}

type Shutdownable interface {
	Shutdown()
}
