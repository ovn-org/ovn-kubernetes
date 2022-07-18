package informer

import (
	kapi "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
)

type ServiceEventHandler interface {
	AddService(*kapi.Service)
	DeleteService(*kapi.Service)
	UpdateService(old, new *kapi.Service)
	SyncServices([]interface{}) error
}

type EndpointSliceEventHandler interface {
	AddEndpointSlice(*discovery.EndpointSlice)
	DeleteEndpointSlice(*discovery.EndpointSlice)
	UpdateEndpointSlice(old, new *discovery.EndpointSlice)
}

type ServiceAndEndpointsEventHandler interface {
	ServiceEventHandler
	EndpointSliceEventHandler
}
