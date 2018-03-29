package factory

import (
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sirupsen/logrus"

	kapi "k8s.io/api/core/v1"
	knet "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	informerfactory "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

type informer struct {
	sync.Mutex
	oType    reflect.Type
	inf      cache.SharedIndexInformer
	handlers map[uint64]cache.ResourceEventHandler
}

func (i *informer) forEachHandler(obj interface{}, f func(id uint64, handler cache.ResourceEventHandler)) {
	i.Lock()
	defer i.Unlock()

	objType := reflect.TypeOf(obj)
	if objType != i.oType {
		logrus.Errorf("object type %v did not match expected %v", objType, i.oType)
		return
	}

	for id, handler := range i.handlers {
		f(id, handler)
	}
}

// WatchFactory initializes and manages common kube watches
type WatchFactory struct {
	iFactory       informerfactory.SharedInformerFactory
	informers      map[reflect.Type]*informer
	handlerCounter uint64
}

const (
	resyncInterval = 12 * time.Hour
)

func newInformer(oType reflect.Type, inf cache.SharedIndexInformer) *informer {
	return &informer{
		oType:    oType,
		inf:      inf,
		handlers: make(map[uint64]cache.ResourceEventHandler),
	}
}

var (
	podType       reflect.Type = reflect.TypeOf(&kapi.Pod{})
	serviceType   reflect.Type = reflect.TypeOf(&kapi.Service{})
	endpointsType reflect.Type = reflect.TypeOf(&kapi.Endpoints{})
	policyType    reflect.Type = reflect.TypeOf(&knet.NetworkPolicy{})
	namespaceType reflect.Type = reflect.TypeOf(&kapi.Namespace{})
	nodeType      reflect.Type = reflect.TypeOf(&kapi.Node{})
)

// NewWatchFactory initializes a new watch factory
func NewWatchFactory(c kubernetes.Interface, stopChan <-chan struct{}) (*WatchFactory, error) {
	// resync time is 12 hours, none of the resources being watched in ovn-kubernetes have
	// any race condition where a resync may be required e.g. cni executable on node watching for
	// events on pods and assuming that an 'ADD' event will contain the annotations put in by
	// ovnkube master (currently, it is just a 'get' loop)
	// the downside of making it tight (like 10 minutes) is needless spinning on all resources
	wf := &WatchFactory{
		iFactory:  informerfactory.NewSharedInformerFactory(c, resyncInterval),
		informers: make(map[reflect.Type]*informer),
	}

	// Create shared informers we know we'll use
	wf.informers[podType] = newInformer(podType, wf.iFactory.Core().V1().Pods().Informer())
	wf.informers[serviceType] = newInformer(serviceType, wf.iFactory.Core().V1().Services().Informer())
	wf.informers[endpointsType] = newInformer(endpointsType, wf.iFactory.Core().V1().Endpoints().Informer())
	wf.informers[policyType] = newInformer(policyType, wf.iFactory.Networking().V1().NetworkPolicies().Informer())
	wf.informers[namespaceType] = newInformer(namespaceType, wf.iFactory.Core().V1().Namespaces().Informer())
	wf.informers[nodeType] = newInformer(nodeType, wf.iFactory.Core().V1().Nodes().Informer())

	wf.iFactory.Start(stopChan)
	res := wf.iFactory.WaitForCacheSync(stopChan)
	for oType, synced := range res {
		if !synced {
			return nil, fmt.Errorf("error in syncing cache for %v informer", oType)
		}
		informer := wf.informers[oType]
		informer.inf.AddEventHandler(wf.newFederatedHandler(informer))
	}

	return wf, nil
}

func (wf *WatchFactory) newFederatedHandler(inf *informer) cache.ResourceEventHandlerFuncs {
	return cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			inf.forEachHandler(obj, func(id uint64, handler cache.ResourceEventHandler) {
				logrus.Debugf("running %v ADD event for handler %d", inf.oType, id)
				handler.OnAdd(obj)
			})
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			inf.forEachHandler(newObj, func(id uint64, handler cache.ResourceEventHandler) {
				logrus.Debugf("running %v UPDATE event for handler %d", inf.oType, id)
				handler.OnUpdate(oldObj, newObj)
			})
		},
		DeleteFunc: func(obj interface{}) {
			if inf.oType != reflect.TypeOf(obj) {
				tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
				if !ok {
					logrus.Errorf("couldn't get object from tombstone: %+v", obj)
					return
				}
				obj = tombstone.Obj
				objType := reflect.TypeOf(obj)
				if inf.oType != objType {
					logrus.Errorf("expected tombstone object resource type %v but got %v", inf.oType, objType)
					return
				}
			}
			inf.forEachHandler(obj, func(id uint64, handler cache.ResourceEventHandler) {
				logrus.Debugf("running %v DELETE event for handler %d", inf.oType, id)
				handler.OnDelete(obj)
			})
		},
	}
}

func getObjectMeta(objType reflect.Type, obj interface{}) (*metav1.ObjectMeta, error) {
	switch objType {
	case podType:
		if pod, ok := obj.(*kapi.Pod); ok {
			return &pod.ObjectMeta, nil
		}
	case serviceType:
		if service, ok := obj.(*kapi.Service); ok {
			return &service.ObjectMeta, nil
		}
	case endpointsType:
		if endpoints, ok := obj.(*kapi.Endpoints); ok {
			return &endpoints.ObjectMeta, nil
		}
	case policyType:
		if policy, ok := obj.(*knet.NetworkPolicy); ok {
			return &policy.ObjectMeta, nil
		}
	case namespaceType:
		if namespace, ok := obj.(*kapi.Namespace); ok {
			return &namespace.ObjectMeta, nil
		}
	case nodeType:
		if node, ok := obj.(*kapi.Node); ok {
			return &node.ObjectMeta, nil
		}
	}
	return nil, fmt.Errorf("cannot get ObjectMeta from type %v", objType)
}

func (wf *WatchFactory) addHandler(objType reflect.Type, namespace string, lsel *metav1.LabelSelector, funcs cache.ResourceEventHandler, processExisting func([]interface{})) (uint64, error) {
	inf, ok := wf.informers[objType]
	if !ok {
		return 0, fmt.Errorf("unknown object type %v", objType)
	}

	sel, err := metav1.LabelSelectorAsSelector(lsel)
	if err != nil {
		return 0, fmt.Errorf("error creating label selector: %v", err)
	}

	filterFunc := func(obj interface{}) bool {
		if namespace == "" && lsel == nil {
			// Unfiltered handler
			return true
		}
		meta, err := getObjectMeta(objType, obj)
		if err != nil {
			logrus.Errorf("watch handler filter error: %v", err)
			return false
		}
		if namespace != "" && meta.Namespace != namespace {
			return false
		}
		if lsel != nil && !sel.Matches(labels.Set(meta.Labels)) {
			return false
		}
		return true
	}

	// Process existing items as a set so the caller can clean up
	// after a restart or whatever
	existingItems := inf.inf.GetStore().List()
	if processExisting != nil {
		items := make([]interface{}, 0)
		for _, obj := range existingItems {
			if filterFunc(obj) {
				items = append(items, obj)
			}
		}
		processExisting(items)
	}

	handlerID := atomic.AddUint64(&wf.handlerCounter, 1)

	inf.Lock()
	defer inf.Unlock()

	inf.handlers[handlerID] = cache.FilteringResourceEventHandler{
		FilterFunc: filterFunc,
		Handler:    funcs,
	}
	logrus.Debugf("added %v event handler %d", objType, handlerID)

	// Send existing items to the handler's add function; informers usually
	// do this but since we share informers, it's long-since happened so
	// we must emulate that here
	for _, obj := range existingItems {
		inf.handlers[handlerID].OnAdd(obj)
	}

	return handlerID, nil
}

func (wf *WatchFactory) removeHandler(objType reflect.Type, handlerID uint64) error {
	inf, ok := wf.informers[objType]
	if !ok {
		return fmt.Errorf("tried to remove unknown object type %v event handler", objType)
	}

	inf.Lock()
	defer inf.Unlock()
	if _, ok := inf.handlers[handlerID]; !ok {
		return fmt.Errorf("tried to remove unknown object type %v event handler %d", objType, handlerID)
	}
	delete(inf.handlers, handlerID)
	logrus.Debugf("removed %v event handler %d", objType, handlerID)
	return nil
}

// AddPodHandler adds a handler function that will be executed on Pod object changes
func (wf *WatchFactory) AddPodHandler(handlerFuncs cache.ResourceEventHandler, processExisting func([]interface{})) (uint64, error) {
	return wf.addHandler(podType, "", nil, handlerFuncs, processExisting)
}

// AddFilteredPodHandler adds a handler function that will be executed when Pod objects that match the given filters change
func (wf *WatchFactory) AddFilteredPodHandler(namespace string, lsel *metav1.LabelSelector, handlerFuncs cache.ResourceEventHandler, processExisting func([]interface{})) (uint64, error) {
	return wf.addHandler(podType, namespace, lsel, handlerFuncs, processExisting)
}

// RemovePodHandler removes a Pod object event handler function
func (wf *WatchFactory) RemovePodHandler(handlerID uint64) error {
	return wf.removeHandler(podType, handlerID)
}

// AddServiceHandler adds a handler function that will be executed on Service object changes
func (wf *WatchFactory) AddServiceHandler(handlerFuncs cache.ResourceEventHandler, processExisting func([]interface{})) (uint64, error) {
	return wf.addHandler(serviceType, "", nil, handlerFuncs, processExisting)
}

// RemoveServiceHandler removes a Service object event handler function
func (wf *WatchFactory) RemoveServiceHandler(handlerID uint64) error {
	return wf.removeHandler(serviceType, handlerID)
}

// AddEndpointsHandler adds a handler function that will be executed on Endpoints object changes
func (wf *WatchFactory) AddEndpointsHandler(handlerFuncs cache.ResourceEventHandler, processExisting func([]interface{})) (uint64, error) {
	return wf.addHandler(endpointsType, "", nil, handlerFuncs, processExisting)
}

// RemoveEndpointsHandler removes a Endpoints object event handler function
func (wf *WatchFactory) RemoveEndpointsHandler(handlerID uint64) error {
	return wf.removeHandler(endpointsType, handlerID)
}

// AddPolicyHandler adds a handler function that will be executed on NetworkPolicy object changes
func (wf *WatchFactory) AddPolicyHandler(handlerFuncs cache.ResourceEventHandler, processExisting func([]interface{})) (uint64, error) {
	return wf.addHandler(policyType, "", nil, handlerFuncs, processExisting)
}

// RemovePolicyHandler removes a NetworkPolicy object event handler function
func (wf *WatchFactory) RemovePolicyHandler(handlerID uint64) error {
	return wf.removeHandler(policyType, handlerID)
}

// AddNamespaceHandler adds a handler function that will be executed on Namespace object changes
func (wf *WatchFactory) AddNamespaceHandler(handlerFuncs cache.ResourceEventHandler, processExisting func([]interface{})) (uint64, error) {
	return wf.addHandler(namespaceType, "", nil, handlerFuncs, processExisting)
}

// AddFilteredNamespaceHandler adds a handler function that will be executed when Namespace objects that match the given filters change
func (wf *WatchFactory) AddFilteredNamespaceHandler(namespace string, lsel *metav1.LabelSelector, handlerFuncs cache.ResourceEventHandler, processExisting func([]interface{})) (uint64, error) {
	return wf.addHandler(namespaceType, namespace, lsel, handlerFuncs, processExisting)
}

// RemoveNamespaceHandler removes a Namespace object event handler function
func (wf *WatchFactory) RemoveNamespaceHandler(handlerID uint64) error {
	return wf.removeHandler(namespaceType, handlerID)
}

// AddNodeHandler adds a handler function that will be executed on Node object changes
func (wf *WatchFactory) AddNodeHandler(handlerFuncs cache.ResourceEventHandler, processExisting func([]interface{})) (uint64, error) {
	return wf.addHandler(nodeType, "", nil, handlerFuncs, processExisting)
}

// RemoveNodeHandler removes a Node object event handler function
func (wf *WatchFactory) RemoveNodeHandler(handlerID uint64) error {
	return wf.removeHandler(nodeType, handlerID)
}
