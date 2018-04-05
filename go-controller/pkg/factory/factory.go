package factory

import (
	"fmt"
	"time"

	informerfactory "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	kapisnetworking "k8s.io/api/networking/v1"
	kapi "k8s.io/api/core/v1"
	"github.com/sirupsen/logrus"
	"sync"
)

// WatchFactory initializes and manages common kube watches
type WatchFactory struct {
	iFactory  informerfactory.SharedInformerFactory
	informers map[string]cache.SharedIndexInformer
	handlers  map[string]federatedHandler
}

type federatedHandler struct {
	mainHandler	cache.ResourceEventHandler
	handlerMap	map[string]cache.ResourceEventHandler
	handlerMutex    *sync.Mutex
}

const (
	typePods       = "pods"
	typeServices   = "services"
	typeEndpoints  = "endpoints"
	typePolicies   = "policies"
	typeNamespaces = "namespaces"
	typeNodes      = "nodes"
	resyncInterval = 12 * time.Hour
)

// NewWatchFactory initializes a new watch factory
func NewWatchFactory(c kubernetes.Interface, stopChan <-chan struct{}) (*WatchFactory, error) {
	// resync time is 12 hours, none of the resources being watched in ovn-kubernetes have
	// any race condition where a resync may be required e.g. cni executable on node watching for
	// events on pods and assuming that an 'ADD' event will contain the annotations put in by
	// ovnkube master (currently, it is just a 'get' loop)
	// the downside of making it tight (like 10 minutes) is needless spinning on all resources
	iFactory := informerfactory.NewSharedInformerFactory(c, resyncInterval)
	wf := &WatchFactory{
		iFactory:  iFactory,
		informers: make(map[string]cache.SharedIndexInformer),
	}

	wf.informers[typePods] = iFactory.Core().V1().Pods().Informer()
	wf.informers[typeServices] = iFactory.Core().V1().Services().Informer()
	wf.informers[typeEndpoints] = iFactory.Core().V1().Endpoints().Informer()
	wf.informers[typePolicies] = iFactory.Networking().V1().NetworkPolicies().Informer()
	wf.informers[typeNamespaces] = iFactory.Core().V1().Namespaces().Informer()
	wf.informers[typeNodes] = iFactory.Core().V1().Nodes().Informer()

	for _, inf := range wf.informers {
		go inf.Run(stopChan)
		if !cache.WaitForCacheSync(stopChan, inf.HasSynced) {
			return nil, fmt.Errorf("error in syncing cache for %T informer", inf)
		}
	}

	wf.initializeHandlers()

	return wf, nil
}

func (wf *WatchFactory) defaultHandlerFuncs(informerType string) cache.ResourceEventHandler {
	return cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			wf.FederateAddEvent(informerType, obj)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			wf.FederateUpdateEvent(informerType, oldObj, newObj)
		},
		DeleteFunc: func(obj interface{}) {
			var returnObj interface{}
			var ok bool
			switch informerType {
				case typePods:
					returnObj, ok = obj.(*kapi.Pod)
				case typeServices:
					returnObj, ok = obj.(*kapi.Service)
				case typeEndpoints:
					returnObj, ok = obj.(*kapi.Endpoints)
				case typePolicies:
					returnObj, ok = obj.(*kapisnetworking.NetworkPolicy)
				case typeNamespaces:
					returnObj, ok = obj.(*kapi.Namespace)
				case typeNodes:
					returnObj, ok = obj.(*kapi.Node)
			}
			if !ok {
				tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
				if !ok {
					logrus.Errorf("couldn't get object from tombstone %+v", obj)
					return
				}
				switch informerType {
					case typePods:
						returnObj, ok = tombstone.Obj.(*kapi.Pod)
					case typeServices:
						returnObj, ok = tombstone.Obj.(*kapi.Service)
					case typeEndpoints:
						returnObj, ok = tombstone.Obj.(*kapi.Endpoints)
					case typePolicies:
						returnObj, ok = tombstone.Obj.(*kapisnetworking.NetworkPolicy)
					case typeNamespaces:
						returnObj, ok = tombstone.Obj.(*kapi.Namespace)
					case typeNodes:
						returnObj, ok = tombstone.Obj.(*kapi.Node)
				}
				if !ok {
					logrus.Errorf("tombstone contained object that is not an object of type '%s' : %#v", informerType, obj)
					return
				}
			}
			wf.FederateDeleteEvent(informerType, returnObj)
		},
	}
}

func (wf *WatchFactory) initializeHandlers() {
	informerTypes := []string {typePods, typeServices, typeEndpoints, typePolicies, typeNamespaces, typeNodes}
	for _, informerType := range informerTypes {
		fh := federatedHandler{
					mainHandler: wf.defaultHandlerFuncs(informerType),
					handlerMap: make(map[string]cache.ResourceEventHandler),
					handlerMutex: &sync.Mutex{},
				}
		wf.handlers[informerType] = fh
		wf.informers[informerType].AddEventHandler(fh.mainHandler)
	}
}

func (wf *WatchFactory) FederateAddEvent(informerType string, obj interface {}) {
	handler := wf.handlers[informerType]
	handler.handlerMutex.Lock()
	defer handler.handlerMutex.Unlock()
	for key, handler := range handler.handlerMap {
		logrus.Debugf("Issuing Add '%s' event for handler key '%s'", informerType, key)
		handler.OnAdd(obj)
	}
}

func (wf *WatchFactory) FederateUpdateEvent(informerType string, oldObj, newObj interface {}) {
	handler := wf.handlers[informerType]
	handler.handlerMutex.Lock()
	defer handler.handlerMutex.Unlock()
	for key, handler := range handler.handlerMap {
		logrus.Debugf("Issuing Update '%s' event for handler key '%s'", informerType, key)
		handler.OnUpdate(oldObj, newObj)
	}
}

func (wf *WatchFactory) FederateDeleteEvent(informerType string, obj interface {}) {
	handler := wf.handlers[informerType]
	handler.handlerMutex.Lock()
	defer handler.handlerMutex.Unlock()
	for key, handler := range handler.handlerMap {
		logrus.Debugf("Issuing Delete '%s' event for handler key '%s'", informerType, key)
		handler.OnDelete(obj)
	}
}

func (wf *WatchFactory) addHandler(informerType string, handlerKey string, handler cache.ResourceEventHandler, processExisting func([]interface{})) {
	inf, ok := wf.informers[informerType]
	if !ok {
		panic("unknown informer type " + informerType)
	}
	if processExisting != nil {
		// cache has synced, lets process the list
		processExisting(inf.GetStore().List())
	}
	// now register the event handler
	wf.handlers[informerType].handlerMutex.Lock()
	defer wf.handlers[informerType].handlerMutex.Unlock()
	wf.handlers[informerType].handlerMap[handlerKey] = handler
}

// AddPodHandler adds a handler function that will be executed on Pod object changes
func (wf *WatchFactory) AddPodHandler(handlerKey string, handler cache.ResourceEventHandler, processExisting func([]interface{})) {
	wf.addHandler(typePods, handlerKey, handler, processExisting)
}

// AddServiceHandler adds a handler function that will be executed on Service object changes
func (wf *WatchFactory) AddServiceHandler(handlerKey string, handler cache.ResourceEventHandler, processExisting func([]interface{})) {
	wf.addHandler(typeServices, handlerKey, handler, processExisting)
}

// AddEndpointHandler adds a handler function that will be executed on Endpoint object changes
func (wf *WatchFactory) AddEndpointHandler(handlerKey string, handler cache.ResourceEventHandler, processExisting func([]interface{})) {
	wf.addHandler(typeEndpoints, handlerKey, handler, processExisting)
}

// AddPolicyHandler adds a handler function that will be executed on NetworkPolicy object changes
func (wf *WatchFactory) AddPolicyHandler(handlerKey string, handler cache.ResourceEventHandler, processExisting func([]interface{})) {
	wf.addHandler(typePolicies, handlerKey, handler, processExisting)
}

// AddNamespaceHandler adds a handler function that will be executed on Namespace object changes
func (wf *WatchFactory) AddNamespaceHandler(handlerKey string, handler cache.ResourceEventHandler, processExisting func([]interface{})) {
	wf.addHandler(typeNamespaces, handlerKey, handler, processExisting)
}

// AddNodeHandler adds a handler function that will be executed on Node object changes
func (wf *WatchFactory) AddNodeHandler(handlerKey string, handler cache.ResourceEventHandler, processExisting func([]interface{})) {
	wf.addHandler(typeNodes, handlerKey, handler, processExisting)
}
