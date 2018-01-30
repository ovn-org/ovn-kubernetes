package factory

import (
	"fmt"
	"time"

	informerfactory "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// WatchFactory initializes and manages common kube watches
type WatchFactory struct {
	iFactory  informerfactory.SharedInformerFactory
	informers map[string]cache.SharedIndexInformer
}

const (
	typePods       = "pods"
	typeServices   = "services"
	typeEndpoints  = "endpoints"
	typePolicies   = "policies"
	typeNamespaces = "namespaces"
	typeNodes      = "nodes"
)

// NewWatchFactory initializes a new watch factory
func NewWatchFactory(c kubernetes.Interface, stopChan <-chan struct{}) (*WatchFactory, error) {
	iFactory := informerfactory.NewSharedInformerFactory(c, 10*time.Minute)
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

	return wf, nil
}

func (wf *WatchFactory) addHandler(informerType string, handler cache.ResourceEventHandler, processExisting func([]interface{})) {
	inf, ok := wf.informers[informerType]
	if !ok {
		panic("unknown informer type " + informerType)
	}
	if processExisting != nil {
		// cache has synced, lets process the list
		processExisting(inf.GetStore().List())
	}
	// now register the event handler
	inf.AddEventHandler(handler)
}

// AddPodHandler adds a handler function that will be executed on Pod object changes
func (wf *WatchFactory) AddPodHandler(handler cache.ResourceEventHandler, processExisting func([]interface{})) {
	wf.addHandler(typePods, handler, processExisting)
}

// AddServiceHandler adds a handler function that will be executed on Service object changes
func (wf *WatchFactory) AddServiceHandler(handler cache.ResourceEventHandler, processExisting func([]interface{})) {
	wf.addHandler(typeServices, handler, processExisting)
}

// AddEndpointHandler adds a handler function that will be executed on Endpoint object changes
func (wf *WatchFactory) AddEndpointHandler(handler cache.ResourceEventHandler, processExisting func([]interface{})) {
	wf.addHandler(typeEndpoints, handler, processExisting)
}

// AddPolicyHandler adds a handler function that will be executed on NetworkPolicy object changes
func (wf *WatchFactory) AddPolicyHandler(handler cache.ResourceEventHandler, processExisting func([]interface{})) {
	wf.addHandler(typePolicies, handler, processExisting)
}

// AddNamespaceHandler adds a handler function that will be executed on Namespace object changes
func (wf *WatchFactory) AddNamespaceHandler(handler cache.ResourceEventHandler, processExisting func([]interface{})) {
	wf.addHandler(typeNamespaces, handler, processExisting)
}

// AddNodeHandler adds a handler function that will be executed on Node object changes
func (wf *WatchFactory) AddNodeHandler(handler cache.ResourceEventHandler, processExisting func([]interface{})) {
	wf.addHandler(typeNodes, handler, processExisting)
}
