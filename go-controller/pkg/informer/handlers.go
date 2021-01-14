package informer

import (
	"fmt"

	kapi "k8s.io/api/core/v1"
)

type ServiceEventHandler interface {
	AddService(*kapi.Service) error
	DeleteService(*kapi.Service) error
	UpdateService(old, new *kapi.Service) error
}

type EndpointsEventHandler interface {
	AddEndpoints(*kapi.Endpoints) error
	DeleteEndpoints(*kapi.Endpoints) error
	UpdateEndpoints(old, new *kapi.Endpoints) error
}

type ServiceAndEndpointsEventHandler interface {
	ServiceEventHandler
	EndpointsEventHandler
}

type PodEventHandler interface {
	AddPod(*kapi.Pod) error
	DeletePod(*kapi.Pod) error
	UpdatePod(old, new *kapi.Pod) error
}

type NodeEventHandler interface {
	AddNode(*kapi.Node) error
	DeleteNode(*kapi.Node) error
	UpdateNode(old, new *kapi.Node) error
}

type NamespaceEventHandler interface {
	AddNamespace(*kapi.Namespace) error
	DeleteNamespace(*kapi.Namespace) error
	UpdateNamespace(old, new *kapi.Namespace) error
}

func ServiceFromObject(obj interface{}) (*kapi.Service, error) {
	svc, ok := obj.(*kapi.Service)
	if !ok {
		return nil, fmt.Errorf("object is not a service")
	}
	return svc, nil
}

func PodFromObject(obj interface{}) (*kapi.Pod, error) {
	svc, ok := obj.(*kapi.Pod)
	if !ok {
		return nil, fmt.Errorf("object is not a pod")
	}
	return svc, nil
}

func EndpointsFromObject(obj interface{}) (*kapi.Endpoints, error) {
	svc, ok := obj.(*kapi.Endpoints)
	if !ok {
		return nil, fmt.Errorf("object is not an endpoint")
	}
	return svc, nil
}

func NodeFromObject(obj interface{}) (*kapi.Node, error) {
	svc, ok := obj.(*kapi.Node)
	if !ok {
		return nil, fmt.Errorf("object is not a node")
	}
	return svc, nil
}

func NamespaceFromObject(obj interface{}) (*kapi.Namespace, error) {
	svc, ok := obj.(*kapi.Namespace)
	if !ok {
		return nil, fmt.Errorf("object is not a namespace")
	}
	return svc, nil
}
