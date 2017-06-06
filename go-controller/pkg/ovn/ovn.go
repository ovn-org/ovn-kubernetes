package ovn

import (
	"github.com/Sirupsen/logrus"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/kube"
	"reflect"

	kapi "k8s.io/client-go/pkg/api/v1"
	"k8s.io/client-go/tools/cache"
)

// Controller structure is the object which holds the controls for starting
// and reacting upon the watched resources (e.g. pods, endpoints)
type Controller struct {
	Kube kube.Interface

	StartPodWatch      func(handler cache.ResourceEventHandler)
	StartServiceWatch  func(handler cache.ResourceEventHandler)
	StartEndpointWatch func(handler cache.ResourceEventHandler)

	gatewayCache map[string]string
}

const (
	// OvnNbctl is the constant string for the ovn nbctl shell command
	OvnNbctl = "ovn-nbctl"
)

// Run starts the actual watching. Also initializes any local structures needed.
func (oc *Controller) Run() {
	oc.gatewayCache = make(map[string]string)
	oc.WatchPods()
	oc.WatchServices()
	oc.WatchEndpoints()
}

// WatchPods starts the watching of Pod resource and calls back the appropriate handler logic
func (oc *Controller) WatchPods() {
	oc.StartPodWatch(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			pod := obj.(*kapi.Pod)
			oc.addLogicalPort(pod)
			return
		},
		UpdateFunc: func(old, new interface{}) {
			return
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
			return
		},
	})
}

// WatchServices starts the watching of Service resource and calls back the
// appropriate handler logic
func (oc *Controller) WatchServices() {
	oc.StartServiceWatch(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			return
		},
		UpdateFunc: func(old, new interface{}) {
			return
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
			return
		},
	})
}

// WatchEndpoints starts the watching of Endpoint resource and calls back the appropriate handler logic
func (oc *Controller) WatchEndpoints() {
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
			return
		},
	})
}
