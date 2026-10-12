// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package factory

import (
	nadinformer "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/informers/externalversions/k8s.cni.cncf.io/v1"

	corev1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/labels"
	coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/tools/cache"

	adminpolicybasedrouteinformer "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1/apis/informers/externalversions/adminpolicybasedroute/v1"
	networkconnectinformer "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/clusternetworkconnect/v1/apis/informers/externalversions/clusternetworkconnect/v1"
	egressipinformer "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/informers/externalversions/egressip/v1"
	routeadvertisementsinformer "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1/apis/informers/externalversions/routeadvertisements/v1"
	uplinkinformer "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/informers/externalversions/uplink/v1alpha1"
	userdefinednetworkinformer "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/informers/externalversions/userdefinednetwork/v1"
	vtepinformer "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1/apis/informers/externalversions/vtep/v1"
)

// ObjectCacheInterface represents the exported methods for getting
// kubernetes resources from the informer cache
type ObjectCacheInterface interface {
	GetPod(namespace, name string) (*corev1.Pod, error)
	GetAllPods() ([]*corev1.Pod, error)
	GetPods(namespace string) ([]*corev1.Pod, error)
	GetNodes() ([]*corev1.Node, error)
	GetNode(name string) (*corev1.Node, error)
	GetService(namespace, name string) (*corev1.Service, error)
	GetServiceEndpointSlices(namespace, svcName, network string) ([]*discovery.EndpointSlice, error)
	GetNamespace(name string) (*corev1.Namespace, error)
	GetNamespaces() ([]*corev1.Namespace, error)
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
	ClusterNetworkConnectInformer() networkconnectinformer.ClusterNetworkConnectInformer
	UserDefinedNetworkInformer() userdefinednetworkinformer.UserDefinedNetworkInformer
	ClusterUserDefinedNetworkInformer() userdefinednetworkinformer.ClusterUserDefinedNetworkInformer
	UplinkInformer() uplinkinformer.UplinkInformer
	UplinkStateInformer() uplinkinformer.UplinkStateInformer
	RouteAdvertisementsInformer() routeadvertisementsinformer.RouteAdvertisementsInformer
	VTEPInformer() vtepinformer.VTEPInformer
	ServiceInformer() cache.SharedIndexInformer
	EndpointSliceInformer() cache.SharedIndexInformer

	GetPods(namespace string) ([]*corev1.Pod, error)
	GetPod(namespace, name string) (*corev1.Pod, error)
	GetAllPods() ([]*corev1.Pod, error)
	GetNamespaces() ([]*corev1.Namespace, error)
	GetNode(name string) (*corev1.Node, error)
	GetNodes() ([]*corev1.Node, error)
	ListNodes(selector labels.Selector) ([]*corev1.Node, error)

	GetService(namespace, name string) (*corev1.Service, error)
	GetServices() ([]*corev1.Service, error)
	GetEndpointSlice(namespace, name string) (*discovery.EndpointSlice, error)
	GetServiceEndpointSlices(namespace, svcName, network string) ([]*discovery.EndpointSlice, error)

	GetNamespace(name string) (*corev1.Namespace, error)
}

type Shutdownable interface {
	Shutdown()
}
