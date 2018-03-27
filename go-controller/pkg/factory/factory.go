package factory

import (
	"fmt"
	"reflect"
	"time"

	"github.com/sirupsen/logrus"

	kapi "k8s.io/api/core/v1"
	knet "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/runtime"
	informerfactory "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// WatchFactory initializes and manages common kube watches
type WatchFactory struct {
	iFactory informerfactory.SharedInformerFactory
}

const (
	resyncInterval = 12 * time.Hour
)

// NewWatchFactory initializes a new watch factory
func NewWatchFactory(c kubernetes.Interface, stopChan <-chan struct{}) (*WatchFactory, error) {
	// resync time is 12 hours, none of the resources being watched in ovn-kubernetes have
	// any race condition where a resync may be required e.g. cni executable on node watching for
	// events on pods and assuming that an 'ADD' event will contain the annotations put in by
	// ovnkube master (currently, it is just a 'get' loop)
	// the downside of making it tight (like 10 minutes) is needless spinning on all resources
	wf := &WatchFactory{
		iFactory: informerfactory.NewSharedInformerFactory(c, resyncInterval),
	}

	// Create shared informers we know we'll use
	wf.iFactory.Core().V1().Pods().Informer()
	wf.iFactory.Core().V1().Services().Informer()
	wf.iFactory.Core().V1().Endpoints().Informer()
	wf.iFactory.Networking().V1().NetworkPolicies().Informer()
	wf.iFactory.Core().V1().Namespaces().Informer()
	wf.iFactory.Core().V1().Nodes().Informer()

	wf.iFactory.Start(stopChan)
	res := wf.iFactory.WaitForCacheSync(stopChan)
	for oType, synced := range res {
		if !synced {
			return nil, fmt.Errorf("error in syncing cache for %v informer", oType)
		}
	}

	return wf, nil
}

func (wf *WatchFactory) addHandler(objTemplate runtime.Object, inf cache.SharedIndexInformer, handlerFuncs cache.ResourceEventHandlerFuncs, processExisting func([]interface{})) {
	if processExisting != nil {
		// cache has synced, lets process the list
		processExisting(inf.GetStore().List())
	}

	// Generically handle DeletedFinalStateUnknown objects
	newHandlerFuncs := handlerFuncs
	if newHandlerFuncs.DeleteFunc != nil {
		newHandlerFuncs.DeleteFunc = func(obj interface{}) {
			templateType := reflect.TypeOf(objTemplate)
			if templateType != reflect.TypeOf(obj) {
				tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
				if !ok {
					logrus.Errorf("couldn't get object from tombstone: %+v", obj)
					return
				}
				obj = tombstone.Obj
				if templateType != reflect.TypeOf(obj) {
					logrus.Errorf("expected tombstone object resource type %v but got %v", templateType, reflect.TypeOf(obj))
					return
				}
			}
			handlerFuncs.DeleteFunc(obj)
		}
	}

	// now register the event handler
	inf.AddEventHandler(newHandlerFuncs)
}

// AddPodHandler adds a handler function that will be executed on Pod object changes
func (wf *WatchFactory) AddPodHandler(handlerFuncs cache.ResourceEventHandlerFuncs, processExisting func([]interface{})) {
	wf.addHandler(&kapi.Pod{}, wf.iFactory.Core().V1().Pods().Informer(), handlerFuncs, processExisting)
}

// AddServiceHandler adds a handler function that will be executed on Service object changes
func (wf *WatchFactory) AddServiceHandler(handlerFuncs cache.ResourceEventHandlerFuncs, processExisting func([]interface{})) {
	wf.addHandler(&kapi.Service{}, wf.iFactory.Core().V1().Services().Informer(), handlerFuncs, processExisting)
}

// AddEndpointHandler adds a handler function that will be executed on Endpoint object changes
func (wf *WatchFactory) AddEndpointHandler(handlerFuncs cache.ResourceEventHandlerFuncs, processExisting func([]interface{})) {
	wf.addHandler(&kapi.Endpoints{}, wf.iFactory.Core().V1().Endpoints().Informer(), handlerFuncs, processExisting)
}

// AddPolicyHandler adds a handler function that will be executed on NetworkPolicy object changes
func (wf *WatchFactory) AddPolicyHandler(handlerFuncs cache.ResourceEventHandlerFuncs, processExisting func([]interface{})) {
	wf.addHandler(&knet.NetworkPolicy{}, wf.iFactory.Networking().V1().NetworkPolicies().Informer(), handlerFuncs, processExisting)
}

// AddNamespaceHandler adds a handler function that will be executed on Namespace object changes
func (wf *WatchFactory) AddNamespaceHandler(handlerFuncs cache.ResourceEventHandlerFuncs, processExisting func([]interface{})) {
	wf.addHandler(&kapi.Namespace{}, wf.iFactory.Core().V1().Namespaces().Informer(), handlerFuncs, processExisting)
}

// AddNodeHandler adds a handler function that will be executed on Node object changes
func (wf *WatchFactory) AddNodeHandler(handlerFuncs cache.ResourceEventHandlerFuncs, processExisting func([]interface{})) {
	wf.addHandler(&kapi.Node{}, wf.iFactory.Core().V1().Nodes().Informer(), handlerFuncs, processExisting)
}
