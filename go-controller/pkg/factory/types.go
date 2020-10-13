package factory

import kapi "k8s.io/api/core/v1"

// ObjectCacheInterface represents the exported methods for getting
// kubernetes resources from the informer cache
type ObjectCacheInterface interface {
	GetPod(namespace, name string) (*kapi.Pod, error)
	GetPods(namespace string) ([]*kapi.Pod, error)
	GetNodes() ([]*kapi.Node, error)
	GetNode(name string) (*kapi.Node, error)
	GetService(namespace, name string) (*kapi.Service, error)
	GetEndpoints(namespace string) ([]*kapi.Endpoints, error)
	GetEndpoint(namespace, name string) (*kapi.Endpoints, error)
	GetNamespace(name string) (*kapi.Namespace, error)
	GetNamespaces() ([]*kapi.Namespace, error)
}
