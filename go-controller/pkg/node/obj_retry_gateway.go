package node

import (
	"fmt"
	"reflect"

	kapi "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	cache "k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	egressipv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/retry"
)

type gwEventHandler struct {
	retry.DefaultEventHandler

	objType  reflect.Type
	g        *gateway
	syncFunc func([]interface{}) error
}

// Create a retry framework for the service and endpointslice controller in gateway.go
func (g *gateway) newRetryFrameworkNodeWithParameters(
	objectType reflect.Type,
	syncFunc func([]interface{}) error) *retry.RetryFramework {
	klog.Infof("[newRetryFrameworkNodeWithParameters] g.watchFactory=%v", g.watchFactory)
	resourceHandler := &retry.ResourceHandler{
		HasUpdateFunc:          true,
		NeedsUpdateDuringRetry: false,
		ObjType:                objectType,
		EventHandler: &gwEventHandler{
			objType:  objectType,
			g:        g,
			syncFunc: syncFunc,
		},
	}
	r := retry.NewRetryFramework(g.stopChan, g.wg, g.watchFactory, resourceHandler)

	return r
}

func (g *gateway) newRetryFrameworkNode(objectType reflect.Type) *retry.RetryFramework {
	return g.newRetryFrameworkNodeWithParameters(objectType, nil)
}

func (h *gwEventHandler) AreResourcesEqual(obj1, obj2 interface{}) (bool, error) {
	// switch based on type
	switch h.objType {
	case factory.ServiceForGatewayType,
		factory.EndpointSliceForGatewayType:
		// always run update code
		return false, nil

	case factory.EgressIPType:
		// we don't care about pkt mark annotation changing as we assume its value should not change and if it does, we don't
		// want to react to that.
		oldEIP, newEIP := obj1.(*egressipv1.EgressIP), obj2.(*egressipv1.EgressIP)
		if reflect.DeepEqual(oldEIP.Status.Items, newEIP.Status.Items) && reflect.DeepEqual(oldEIP.Spec.EgressIPs, newEIP.Spec.EgressIPs) {
			return true, nil
		}
		return false, nil

	default:
		return false, fmt.Errorf("no object comparison for type %s", h.objType)
	}

}

// Given an object key and its type, GetResourceFromInformerCache returns the latest state of
// the object from the informers cache.
func (h *gwEventHandler) GetResourceFromInformerCache(key string) (interface{}, error) {
	var obj interface{}
	var namespace, name string
	var err error

	namespace, name, err = cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return nil, fmt.Errorf("failed to split key %s: %v", key, err)
	}

	switch h.objType {

	case factory.EndpointSliceForGatewayType:
		obj, err = h.g.watchFactory.GetEndpointSlice(namespace, name)

	case factory.ServiceForGatewayType:
		obj, err = h.g.watchFactory.GetService(namespace, name)

	case factory.EgressIPType:
		obj, err = h.g.watchFactory.GetEgressIP(name)

	default:
		err = fmt.Errorf("object type %s not supported, cannot retrieve it from informers cache",
			h.objType)
	}

	return obj, err
}

// Given a *RetryFramework instance, an object to add and a boolean specifying if
// the function was executed from iterateRetryResources, AddResource adds the
// specified object to the cluster according to its type and returns the error,
// if any, yielded during object creation.
func (h *gwEventHandler) AddResource(obj interface{}, fromRetryLoop bool) error {
	switch h.objType {
	case factory.ServiceForGatewayType:
		svc := obj.(*kapi.Service)
		return h.g.AddService(svc)

	case factory.EndpointSliceForGatewayType:
		endpointSlice := obj.(*discovery.EndpointSlice)
		return h.g.AddEndpointSlice(endpointSlice)

	case factory.EgressIPType:
		eip := obj.(*egressipv1.EgressIP)
		return h.g.AddEgressIP(eip)

	default:
		return fmt.Errorf("no add function for object type %s", h.objType)
	}
}

// Given a *RetryFramework instance, an old and a new object, UpdateResource updates
// the specified object in the cluster to its version in newObj according to its type
// and returns the error, if any, yielded during the object update. The inRetryCache
// boolean argument is to indicate if the given resource is in the retryCache or not.
func (h *gwEventHandler) UpdateResource(oldObj, newObj interface{}, inRetryCache bool) error {
	switch h.objType {
	case factory.ServiceForGatewayType:
		oldSvc := oldObj.(*kapi.Service)
		newSvc := newObj.(*kapi.Service)
		return h.g.UpdateService(oldSvc, newSvc)

	case factory.EndpointSliceForGatewayType:
		oldEndpointSlice := oldObj.(*discovery.EndpointSlice)
		newEndpointSlice := newObj.(*discovery.EndpointSlice)
		return h.g.UpdateEndpointSlice(oldEndpointSlice, newEndpointSlice)

	case factory.EgressIPType:
		oldEIP := oldObj.(*egressipv1.EgressIP)
		newEIP := newObj.(*egressipv1.EgressIP)
		return h.g.UpdateEgressIP(oldEIP, newEIP)

	default:
		return fmt.Errorf("no update function for object type %s", h.objType)
	}
}

// Given a *RetryFramework instance, an object and optionally a cachedObj, DeleteResource
// deletes the object from the cluster according to the delete logic of its resource type.
// cachedObj is the internal cache entry for this object, used for now for pods and network
// policies.
func (h *gwEventHandler) DeleteResource(obj, cachedObj interface{}) error {
	switch h.objType {
	case factory.ServiceForGatewayType:
		svc := obj.(*kapi.Service)
		return h.g.DeleteService(svc)

	case factory.EndpointSliceForGatewayType:
		endpointSlice := obj.(*discovery.EndpointSlice)
		return h.g.DeleteEndpointSlice(endpointSlice)

	case factory.EgressIPType:
		eip := obj.(*egressipv1.EgressIP)
		return h.g.DeleteEgressIP(eip)

	default:
		return fmt.Errorf("no delete function for object type %s", h.objType)
	}
}

func (h *gwEventHandler) SyncFunc(objs []interface{}) error {
	var syncFunc func([]interface{}) error

	if h.syncFunc != nil {
		// syncFunc was provided explicitly
		syncFunc = h.syncFunc
	} else {

		switch h.objType {
		case factory.EndpointSliceForGatewayType:
			// no sync needed
			syncFunc = nil

		case factory.ServiceForGatewayType:
			syncFunc = h.g.SyncServices

		case factory.EgressIPType:
			syncFunc = h.g.SyncEgressIP

		default:
			return fmt.Errorf("no sync function for object type %s", h.objType)
		}
	}
	if syncFunc == nil {
		return nil
	}
	return syncFunc(objs)
}
