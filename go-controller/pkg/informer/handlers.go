package informer

import (
	kapi "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
)

type ServiceEventHandler interface {
	AddService(*kapi.Service) error
	DeleteService(*kapi.Service) error
	UpdateService(old, new *kapi.Service) error
	SyncServices([]interface{}) error
}

type EndpointSliceEventHandler interface {
	AddEndpointSlice(*discovery.EndpointSlice) error
	DeleteEndpointSlice(*discovery.EndpointSlice) error
	UpdateEndpointSlice(old, new *discovery.EndpointSlice) error
}

type ServiceAndEndpointsEventHandler interface {
	ServiceEventHandler
	EndpointSliceEventHandler
}
