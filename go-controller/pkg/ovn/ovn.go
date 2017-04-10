package ovn

import (
	"github.com/golang/glog"

	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/kube"
	kapi "k8s.io/client-go/pkg/api/v1"
	"k8s.io/client-go/tools/cache"
)

type OvnController struct {
	Kube kube.KubeInterface

	StartPodWatch      func(handler cache.ResourceEventHandler)
	StartEndpointWatch func(handler cache.ResourceEventHandler)

	gatewayCache map[string]string
}

const (
	OVN_NBCTL = "ovn-nbctl"
)

func (oc *OvnController) Run() {
	oc.gatewayCache = make(map[string]string)
	oc.WatchPods()
	oc.WatchEndpoints()
}

func (oc *OvnController) WatchPods() {
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
					glog.Errorf("couldn't get object from tombstone %+v", obj)
					return
				}
				pod, ok = tombstone.Obj.(*kapi.Pod)
				if !ok {
					glog.Errorf("tombstone contained object that is not a pod %#v", obj)
					return
				}
			}
			oc.deleteLogicalPort(pod)
			return
		},
	})
}

func (oc *OvnController) WatchEndpoints() {
	oc.StartEndpointWatch(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			ep := obj.(*kapi.Endpoints)
			err := oc.addEndpoints(ep)
			if err != nil {
				glog.Errorf("Error in adding load balancer: %v", err)
			}
		},
		UpdateFunc: func(old, new interface{}) { return },
		DeleteFunc: func(obj interface{}) {
			ep, ok := obj.(*kapi.Endpoints)
			if !ok {
				tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
				if !ok {
					glog.Errorf("couldn't get object from tombstone %+v", obj)
					return
				}
				ep, ok = tombstone.Obj.(*kapi.Endpoints)
				if !ok {
					glog.Errorf("tombstone contained object that is not a pod %#v", obj)
					return
				}
			}
			err := oc.deleteEndpoints(ep)
			if err != nil {
				glog.Errorf("Error in deleting endpoints - %v", err)
			}
			return
		},
	})
}
