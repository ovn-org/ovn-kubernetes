package informer

import (
	kapi "k8s.io/api/core/v1"
)

type ServiceEventHandler interface {
	AddService(*kapi.Service) error
	DeleteService(*kapi.Service) error
	UpdateService(old, new *kapi.Service) error
	SyncServices([]interface{}) error
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
