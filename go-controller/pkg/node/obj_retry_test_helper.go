package node

import (
	"fmt"
	"reflect"
	"sync"

	kapi "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/retry"
)

type nodePortWatcherEventHandler struct {
	retry.DefaultEventHandler

	objType  reflect.Type
	n        *nodePortWatcher
	syncFunc func([]interface{}) error
}

func (h *nodePortWatcherEventHandler) FilterResource(obj interface{}) bool {
	return true
}

// used only in unit tests to narrow the scope to just the nodeport iptables changes and not all the other parts that
// go along with the gateway type (port claim, openflow, healthchecker)
func (n *nodePortWatcher) newRetryFrameworkForTests(objectType reflect.Type, stopChan <-chan struct{}, wg *sync.WaitGroup) *retry.RetryFramework {
	resourceHandler := &retry.ResourceHandler{
		HasUpdateFunc:          true,
		NeedsUpdateDuringRetry: false,
		ObjType:                objectType,
		EventHandler: &nodePortWatcherEventHandler{
			objType: objectType,
			n:       n,
		},
	}
	r := retry.NewRetryFramework(stopChan, wg, n.watchFactory.(*factory.WatchFactory), resourceHandler)

	return r
}

func (h *nodePortWatcherEventHandler) AreResourcesEqual(obj1, obj2 interface{}) (bool, error) {
	// switch based on type
	switch h.objType {
	case factory.ServiceForFakeNodePortWatcherType:
		// always run update code
		return false, nil

	default:
		return false, fmt.Errorf("no object comparison for type %s", h.objType)
	}

}

// Given an object key and its type, GetResourceFromInformerCache returns the latest state of the object
// from the informers cache.
func (h *nodePortWatcherEventHandler) GetResourceFromInformerCache(key string) (interface{}, error) {
	var obj interface{}
	var namespace, name string
	var err error

	namespace, name, err = cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return nil, fmt.Errorf("failed to split key %s: %v", key, err)
	}

	switch h.objType {
	case factory.ServiceForFakeNodePortWatcherType:
		obj, err = h.n.watchFactory.GetService(namespace, name)

	default:
		err = fmt.Errorf("object type %s not supported, cannot retrieve it from informers cache",
			h.objType)
	}

	return obj, err
}

// Given a *RetryFramework instance, an object to add and a boolean specifying if the function was executed from
// iterateRetryResources, AddResource adds the specified object to the cluster according to its type and
// returns the error, if any, yielded during object creation.
func (h *nodePortWatcherEventHandler) AddResource(obj interface{}, fromRetryLoop bool) error {
	switch h.objType {
	case factory.ServiceForFakeNodePortWatcherType:
		svc := obj.(*kapi.Service)
		return h.n.AddService(svc)

	default:
		return fmt.Errorf("no add function for object type %s", h.objType)
	}
}

// Given a *RetryFramework instance, an old and a new object, UpdateResource updates the specified object in the cluster
// to its version in newObj according to its type and returns the error, if any, yielded during the object update.
// The inRetryCache boolean argument is to indicate if the given resource is in the retryCache or not.
func (h *nodePortWatcherEventHandler) UpdateResource(oldObj, newObj interface{}, inRetryCache bool) error {
	switch h.objType {
	case factory.ServiceForFakeNodePortWatcherType:
		oldSvc := oldObj.(*kapi.Service)
		newSvc := newObj.(*kapi.Service)
		return h.n.UpdateService(oldSvc, newSvc)

	default:
		return fmt.Errorf("no update function for object type %s", h.objType)
	}
}

// Given a *RetryFramework instance, an object and optionally a cachedObj, DeleteResource deletes the object from the cluster
// according to the delete logic of its resource type. cachedObj is the internal cache entry for this object,
// used for now for pods and network policies.
func (h *nodePortWatcherEventHandler) DeleteResource(obj, cachedObj interface{}) error {
	switch h.objType {
	case factory.ServiceForFakeNodePortWatcherType:
		svc := obj.(*kapi.Service)
		return h.n.DeleteService(svc)

	default:
		return fmt.Errorf("no delete function for object type %s", h.objType)
	}
}

func (h *nodePortWatcherEventHandler) SyncFunc(objs []interface{}) error {
	var syncFunc func([]interface{}) error

	if h.syncFunc != nil {
		// syncFunc was provided explicitly
		syncFunc = h.syncFunc
	} else {
		switch h.objType {
		case factory.ServiceForFakeNodePortWatcherType:
			// no sync needed
			syncFunc = nil
		default:
			return fmt.Errorf("no sync function for object type %s", h.objType)
		}
	}
	if syncFunc == nil {
		return nil
	}
	return syncFunc(objs)
}
