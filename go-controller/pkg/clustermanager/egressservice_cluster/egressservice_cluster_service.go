package egressservice_cluster

import (
	"fmt"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/controller/services"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	corev1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
)

func (c *Controller) onServiceAdd(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", obj, err))
		return
	}

	service := obj.(*corev1.Service)
	// We only care about new LoadBalancer services that have an EgressService
	if !util.ServiceTypeHasLoadBalancer(service) || len(service.Status.LoadBalancer.Ingress) == 0 {
		return
	}

	es, err := c.egressServiceLister.EgressServices(service.Namespace).Get(service.Name)
	if err != nil && !apierrors.IsNotFound(err) {
		// This shouldn't happen, but we queue the service in case we got an unrelated
		// error when the EgressService exists
		c.egressServiceQueue.Add(key)
		return
	}

	// There is no EgressService resource for this service so we don't queue it
	if es == nil {
		return
	}

	klog.V(4).Infof("Adding egress service %s", key)
	c.egressServiceQueue.Add(key)
}

func (c *Controller) onServiceUpdate(oldObj, newObj interface{}) {
	oldService := oldObj.(*corev1.Service)
	newService := newObj.(*corev1.Service)

	// don't process resync or objects that are marked for deletion
	if oldService.ResourceVersion == newService.ResourceVersion ||
		!newService.GetDeletionTimestamp().IsZero() {
		return
	}

	// We only care about LoadBalancer service updates that enable/disable egress service functionality
	if !util.ServiceTypeHasLoadBalancer(oldService) && !util.ServiceTypeHasLoadBalancer(newService) {
		return
	}

	key, err := cache.MetaNamespaceKeyFunc(newObj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", newObj, err))
		return
	}

	es, err := c.egressServiceLister.EgressServices(newService.Namespace).Get(newService.Name)
	if err != nil && !apierrors.IsNotFound(err) {
		// This shouldn't happen, but we queue the service in case we got an unrelated
		// error when the EgressService exists
		c.egressServiceQueue.Add(key)
		return
	}

	// There is no EgressService resource for this service so we don't queue it
	if es == nil {
		return
	}

	c.egressServiceQueue.Add(key)
}

func (c *Controller) onServiceDelete(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", obj, err))
		return
	}

	service := obj.(*corev1.Service)
	// We only care about deletions of LoadBalancer services
	if !util.ServiceTypeHasLoadBalancer(service) {
		return
	}

	klog.V(4).Infof("Deleting egress service %s", key)
	es, err := c.egressServiceLister.EgressServices(service.Namespace).Get(service.Name)
	if err != nil && !apierrors.IsNotFound(err) {
		// This shouldn't happen, but we queue the service in case we got an unrelated
		// error when the EgressService exists
		c.egressServiceQueue.Add(key)
		return
	}

	// There is no EgressService resource for this service so we don't queue it
	if es == nil {
		return
	}

	c.egressServiceQueue.Add(key)
}

// Returns all nodes that host the cluster-networked endpoints for the given service
func (c *Controller) backedNodes(svc *corev1.Service) ([]string, error) {
	endpointSlices, err := c.watchFactory.GetEndpointSlices(svc.Namespace, svc.Name)
	if err != nil {
		return nil, err
	}

	nodes := sets.New[string]()
	for _, eps := range endpointSlices {
		if eps.AddressType == discovery.AddressTypeFQDN {
			continue
		}

		for _, ep := range eps.Endpoints {
			if ep.NodeName != nil {
				for _, ip := range ep.Addresses {
					ipStr := utilnet.ParseIPSloppy(ip).String()
					if !services.IsHostEndpoint(ipStr) {
						nodes.Insert(*ep.NodeName)
						break
					}
				}
			}
		}
	}

	return nodes.UnsortedList(), nil
}
