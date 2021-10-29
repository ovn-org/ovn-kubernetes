package unidling

import (
	"fmt"
	"sync"
	"time"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"

	"k8s.io/klog/v2"
)

// unidlingController checks periodically the OVN events db
// and generates a Kubernetes NeedPods events with the Service
// associated to the VIP
type unidlingController struct {
	eventRecorder record.EventRecorder
	// Map of load balancers to service namespace
	serviceVIPToName     map[ServiceVIPKey]types.NamespacedName
	serviceVIPToNameLock sync.Mutex
	sbClient             libovsdbclient.Client
}

// NewController creates a new unidling controller
func NewController(recorder record.EventRecorder, serviceInformer cache.SharedIndexInformer, sbClient libovsdbclient.Client) *unidlingController {
	uc := &unidlingController{
		eventRecorder:    recorder,
		serviceVIPToName: map[ServiceVIPKey]types.NamespacedName{},
		sbClient:         sbClient,
	}

	// we only process events on unidling, there is no reconcilation
	klog.Info("Setting up event handlers for services")
	serviceInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: uc.onServiceAdd,
		UpdateFunc: func(old, new interface{}) {
			uc.onServiceDelete(old)
			uc.onServiceAdd(new)
		},
		DeleteFunc: uc.onServiceDelete,
	})
	return uc
}

func (uc *unidlingController) onServiceAdd(obj interface{}) {
	svc := obj.(*v1.Service)
	if util.ServiceTypeHasClusterIP(svc) && util.IsClusterIPSet(svc) {
		for _, ip := range util.GetClusterIPs(svc) {
			for _, svcPort := range svc.Spec.Ports {
				vip := util.JoinHostPortInt32(ip, svcPort.Port)
				uc.AddServiceVIPToName(vip, svcPort.Protocol, svc.Namespace, svc.Name)
			}
		}
	}
}

func (uc *unidlingController) onServiceDelete(obj interface{}) {
	svc, ok := obj.(*v1.Service)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("couldn't get object from tombstone %#v", obj))
			return
		}
		svc, ok = tombstone.Obj.(*v1.Service)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("tombstone contained object that is not a Service: %#v", obj))
			return
		}
	}

	if util.ServiceTypeHasClusterIP(svc) && util.IsClusterIPSet(svc) {
		for _, ip := range util.GetClusterIPs(svc) {
			for _, svcPort := range svc.Spec.Ports {
				vip := util.JoinHostPortInt32(ip, svcPort.Port)
				uc.DeleteServiceVIPToName(vip, svcPort.Protocol)
			}
		}
	}
}

// ServiceVIPKey is used for looking up service namespace information for a
// particular load balancer
type ServiceVIPKey struct {
	// Load balancer VIP in the form "ip:port"
	vip string
	// Protocol used by the load balancer
	protocol v1.Protocol
}

// AddServiceVIPToName associates a k8s service name with a load balancer VIP
func (uc *unidlingController) AddServiceVIPToName(vip string, protocol v1.Protocol, namespace, name string) {
	uc.serviceVIPToNameLock.Lock()
	defer uc.serviceVIPToNameLock.Unlock()
	uc.serviceVIPToName[ServiceVIPKey{vip, protocol}] = types.NamespacedName{Namespace: namespace, Name: name}
}

// GetServiceVIPToName retrieves the associated k8s service name for a load balancer VIP
func (uc *unidlingController) GetServiceVIPToName(vip string, protocol v1.Protocol) (types.NamespacedName, bool) {
	uc.serviceVIPToNameLock.Lock()
	defer uc.serviceVIPToNameLock.Unlock()
	namespace, ok := uc.serviceVIPToName[ServiceVIPKey{vip, protocol}]
	return namespace, ok
}

// DeleteServiceVIPToName retrieves the associated k8s service name for a load balancer VIP
func (uc *unidlingController) DeleteServiceVIPToName(vip string, protocol v1.Protocol) {
	uc.serviceVIPToNameLock.Lock()
	defer uc.serviceVIPToNameLock.Unlock()
	delete(uc.serviceVIPToName, ServiceVIPKey{vip, protocol})
}

func (uc *unidlingController) Run(stopCh <-chan struct{}) {
	ticker := time.NewTicker(5 * time.Second)

	for {
		select {
		case <-ticker.C:
			out, _, err := util.RunOVNSbctl("--format=json", "list", "controller_event")
			if err != nil {
				continue
			}

			events, err := extractEmptyLBBackendsEvents([]byte(out))
			if err != nil || len(events) == 0 {
				continue
			}

			for _, event := range events {
				_, _, err := util.RunOVNSbctl("destroy", "controller_event", event.uuid)
				if err != nil {
					// Don't unidle until we are able to remove the controller event
					klog.Errorf("Unable to remove controller event %s", event.uuid)
					continue
				}
				if serviceName, ok := uc.GetServiceVIPToName(event.vip, event.protocol); ok {
					serviceRef := v1.ObjectReference{
						Kind:      "Service",
						Namespace: serviceName.Namespace,
						Name:      serviceName.Name,
					}
					klog.V(5).Infof("Sending a NeedPods event for service %s in namespace %s.", serviceName.Name, serviceName.Namespace)
					uc.eventRecorder.Eventf(&serviceRef, v1.EventTypeNormal, "NeedPods", "The service %s needs pods", serviceName.Name)
				}
			}
		case <-stopCh:
			return
		}
	}
}
