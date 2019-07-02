package cluster

import (
	"k8s.io/client-go/tools/cache"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube/healthcheck"

	kapi "k8s.io/api/core/v1"
	ktypes "k8s.io/apimachinery/pkg/types"
)

// initLoadBalancerHealthChecker initializes the health check server for
// ServiceTypeLoadBalancer services
func initLoadBalancerHealthChecker(nodeName string, wf *factory.WatchFactory) error {
	server := healthcheck.NewServer(nodeName, nil, nil, nil)
	services := make(map[ktypes.NamespacedName]uint16)
	endpoints := make(map[ktypes.NamespacedName]int)

	_, err := wf.AddServiceHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			svc := obj.(*kapi.Service)
			if svc.Spec.HealthCheckNodePort != 0 {
				name := ktypes.NamespacedName{Namespace: svc.Namespace, Name: svc.Name}
				services[name] = uint16(svc.Spec.HealthCheckNodePort)
				_ = server.SyncServices(services)
			}
		},
		UpdateFunc: func(old, new interface{}) {
			// HealthCheckNodePort can't be changed on update
		},
		DeleteFunc: func(obj interface{}) {
			svc := obj.(*kapi.Service)
			if svc.Spec.HealthCheckNodePort != 0 {
				name := ktypes.NamespacedName{Namespace: svc.Namespace, Name: svc.Name}
				delete(services, name)
				delete(endpoints, name)
				_ = server.SyncServices(services)
			}
		},
	}, nil)
	if err != nil {
		return err
	}

	_, err = wf.AddEndpointsHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			ep := obj.(*kapi.Endpoints)
			name := ktypes.NamespacedName{Namespace: ep.Namespace, Name: ep.Name}
			if _, exists := services[name]; exists {
				endpoints[name] = countLocalEndpoints(ep, nodeName)
				_ = server.SyncEndpoints(endpoints)
			}
		},
		UpdateFunc: func(old, new interface{}) {
			ep := new.(*kapi.Endpoints)
			name := ktypes.NamespacedName{Namespace: ep.Namespace, Name: ep.Name}
			if _, exists := services[name]; exists {
				endpoints[name] = countLocalEndpoints(ep, nodeName)
				_ = server.SyncEndpoints(endpoints)
			}
		},
		DeleteFunc: func(obj interface{}) {
			ep := obj.(*kapi.Endpoints)
			name := ktypes.NamespacedName{Namespace: ep.Namespace, Name: ep.Name}
			delete(endpoints, name)
			_ = server.SyncEndpoints(endpoints)
		},
	}, nil)
	return err
}

func countLocalEndpoints(ep *kapi.Endpoints, nodeName string) int {
	num := 0
	for i := range ep.Subsets {
		ss := &ep.Subsets[i]
		for i := range ss.Addresses {
			addr := &ss.Addresses[i]
			if addr.NodeName != nil && *addr.NodeName == nodeName {
				num++
			}
		}
	}
	return num
}
