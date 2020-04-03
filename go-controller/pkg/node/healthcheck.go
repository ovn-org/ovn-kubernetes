package node

import (
	"strings"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube/healthcheck"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	kapi "k8s.io/api/core/v1"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog"
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

// check for OVS internal ports without any ofport assigned, they are stale ports that must be deleted
func checkForStaleOVSInterfaces(stopChan chan struct{}) {
	for {
		select {
		case <-time.After(60 * time.Second):
			stdout, _, err := util.RunOVSVsctl("--data=bare", "--no-headings", "--columns=name", "find",
				"interface", "ofport=-1")
			if err != nil {
				klog.Errorf("failed to list OVS interfaces with ofport set to -1")
				continue
			}
			if len(stdout) == 0 {
				continue
			}
			values := strings.Split(stdout, "\n\n")
			for _, val := range values {
				klog.Warningf("found stale interface %s, so deleting it", val)
				_, stderr, err := util.RunOVSVsctl("--if-exists", "--with-iface", "del-port", val)
				if err != nil {
					klog.Errorf("failed to delete OVS port/interface %s: stderr: %s (%v)",
						val, stderr, err)
				}
			}
		case <-stopChan:
			return
		}
	}
}
