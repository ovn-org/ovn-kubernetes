package ovn

import (
	"github.com/Sirupsen/logrus"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/kube"
	kapi "k8s.io/client-go/pkg/api/v1"
	kapisnetworking "k8s.io/client-go/pkg/apis/networking/v1"
	"k8s.io/client-go/tools/cache"
	"reflect"
	"sync"
)

// Controller structure is the object which holds the controls for starting
// and reacting upon the watched resources (e.g. pods, endpoints)
type Controller struct {
	Kube kube.Interface

	StartPodWatch       func(handler cache.ResourceEventHandler)
	StartEndpointWatch  func(handler cache.ResourceEventHandler)
	StartServiceWatch   func(handler cache.ResourceEventHandler)
	StartPolicyWatch    func(handler cache.ResourceEventHandler)
	StartNamespaceWatch func(handler cache.ResourceEventHandler)

	gatewayCache map[string]string

	// For TCP and UDP type traffic, cache OVN load-balancers used for the
	// cluster's east-west traffic.
	loadbalancerClusterCache map[string]string

	// A cache of all logical switches seen by the watcher
	logicalSwitchCache map[string]bool

	// For each namespace, an address_set that has all the pod IP
	// address in that namespace
	namespaceAddressSet map[string]map[string]bool

	// For each namespace, a lock to protect critical regions
	namespaceMutex map[string]*sync.Mutex

	// For each namespace, a map of policy name to 'namespacePolicy'.
	namespacePolicies map[string]map[string]*namespacePolicy

	// For each logical port, the number of network policies that want
	// to add a deny rule.
	lspIngressDenyCache map[string]int

	// A mutex for logicalPortIngressDenyCache
	lspMutex *sync.Mutex
}

const (
	// OvnNbctl is the constant string for the ovn-nbctl shell command
	OvnNbctl = "ovn-nbctl"

	// TCP is the constant string for the string "TCP"
	TCP = "TCP"

	// UDP is the constant string for the string "UDP"
	UDP = "UDP"
)

// Run starts the actual watching. Also initializes any local structures needed.
func (oc *Controller) Run() {
	oc.gatewayCache = make(map[string]string)
	oc.WatchPods()
	oc.WatchServices()
	oc.WatchEndpoints()
	oc.WatchNamespaces()

	oc.initializePolicyData()
	oc.WatchNetworkPolicy()
}

func (oc *Controller) initializePolicyData() {
	oc.logicalSwitchCache = make(map[string]bool)
	oc.namespaceAddressSet = make(map[string]map[string]bool)
	oc.namespacePolicies = make(map[string]map[string]*namespacePolicy)
	oc.namespaceMutex = make(map[string]*sync.Mutex)
	oc.lspIngressDenyCache = make(map[string]int)
	oc.lspMutex = &sync.Mutex{}
}

// WatchPods starts the watching of Pod resource and calls back the appropriate handler logic
func (oc *Controller) WatchPods() {
	oc.StartPodWatch(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			pod := obj.(*kapi.Pod)
			oc.addLogicalPort(pod)
		},
		UpdateFunc: func(old, new interface{}) {
		},
		DeleteFunc: func(obj interface{}) {
			pod, ok := obj.(*kapi.Pod)
			if !ok {
				tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
				if !ok {
					logrus.Errorf("couldn't get object from tombstone %+v", obj)
					return
				}
				pod, ok = tombstone.Obj.(*kapi.Pod)
				if !ok {
					logrus.Errorf("tombstone contained object that is not a pod %#v", obj)
					return
				}
			}
			oc.deleteLogicalPort(pod)
		},
	})
}

// WatchServices starts the watching of Service resource and calls back the
// appropriate handler logic
func (oc *Controller) WatchServices() {
	oc.StartServiceWatch(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			service := obj.(*kapi.Service)
			oc.addService(service)
		},
		UpdateFunc: func(old, new interface{}) {
		},
		DeleteFunc: func(obj interface{}) {
			service, ok := obj.(*kapi.Service)
			if !ok {
				tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
				if !ok {
					logrus.Errorf("couldn't get object from tombstone %+v", obj)
					return
				}
				service, ok = tombstone.Obj.(*kapi.Service)
				if !ok {
					logrus.Errorf("tombstone contained object that is not a Service %#v", obj)
					return
				}
			}
			oc.deleteService(service)
		},
	})
}

// WatchEndpoints starts the watching of Endpoint resource and calls back the appropriate handler logic
func (oc *Controller) WatchEndpoints() {
	oc.loadbalancerClusterCache = make(map[string]string)

	oc.StartEndpointWatch(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			ep := obj.(*kapi.Endpoints)
			err := oc.addEndpoints(ep)
			if err != nil {
				logrus.Errorf("Error in adding load balancer: %v", err)
			}
		},
		UpdateFunc: func(old, new interface{}) {
			epNew := new.(*kapi.Endpoints)
			epOld := old.(*kapi.Endpoints)
			if reflect.DeepEqual(epNew.Subsets, epOld.Subsets) {
				return
			}
			if len(epNew.Subsets) == 0 {
				err := oc.deleteEndpoints(epNew)
				if err != nil {
					logrus.Errorf("Error in deleting endpoints - %v", err)
				}
			} else {
				err := oc.addEndpoints(epNew)
				if err != nil {
					logrus.Errorf("Error in modifying endpoints: %v", err)
				}
			}
		},
		DeleteFunc: func(obj interface{}) {
			ep, ok := obj.(*kapi.Endpoints)
			if !ok {
				tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
				if !ok {
					logrus.Errorf("couldn't get object from tombstone %+v", obj)
					return
				}
				ep, ok = tombstone.Obj.(*kapi.Endpoints)
				if !ok {
					logrus.Errorf("tombstone contained object that is not a pod %#v", obj)
					return
				}
			}
			err := oc.deleteEndpoints(ep)
			if err != nil {
				logrus.Errorf("Error in deleting endpoints - %v", err)
			}
		},
	})
}

// WatchNetworkPolicy starts the watching of network policy resource and calls
// back the appropriate handler logic
func (oc *Controller) WatchNetworkPolicy() {
	oc.StartPolicyWatch(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			policy := obj.(*kapisnetworking.NetworkPolicy)
			oc.addNetworkPolicy(policy)
			return
		},
		UpdateFunc: func(old, newer interface{}) {
			oldPolicy := old.(*kapisnetworking.NetworkPolicy)
			newPolicy := newer.(*kapisnetworking.NetworkPolicy)
			oc.deleteNetworkPolicy(oldPolicy)
			oc.addNetworkPolicy(newPolicy)
			return
		},
		DeleteFunc: func(obj interface{}) {
			policy, ok := obj.(*kapisnetworking.NetworkPolicy)
			if !ok {
				tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
				if !ok {
					logrus.Errorf("couldn't get object from tombstone %+v", obj)
					return
				}
				policy, ok = tombstone.Obj.(*kapisnetworking.NetworkPolicy)
				if !ok {
					logrus.Errorf("tombstone contained object that is not a pod %#v", obj)
					return
				}
			}
			oc.deleteNetworkPolicy(policy)
			return
		},
	})
}

// WatchNamespaces starts the watching of namespace resource and calls
// back the appropriate handler logic
func (oc *Controller) WatchNamespaces() {
	oc.StartNamespaceWatch(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			ns := obj.(*kapi.Namespace)
			oc.addNamespace(ns)
			return
		},
		UpdateFunc: func(old, newer interface{}) {
			ns := newer.(*kapi.Namespace)
			oc.addNamespace(ns)
			return
		},
		DeleteFunc: func(obj interface{}) {
			ns, ok := obj.(*kapi.Namespace)
			if !ok {
				tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
				if !ok {
					logrus.Errorf("couldn't get object from tombstone %+v", obj)
					return
				}
				ns, ok = tombstone.Obj.(*kapi.Namespace)
				if !ok {
					logrus.Errorf("tombstone contained object that is not a namespace %#v", obj)
					return
				}
			}
			oc.deleteNamespace(ns)
			return
		},
	})
}
