package egressservice

import (
	"errors"
	"fmt"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/controller/services"
	discovery "k8s.io/api/discovery/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

/*
	Pretty much a copy of what the services controller does with endpointslices.
	The main difference is that we queue an endpointslice's service only if
	it is already in our local cache of known egress services:
	If it is not there it is either not an egress service or it was not reconciled
	yet and when it does the endpoint slice change will be included.
*/

func (c *Controller) onEndpointSliceAdd(obj interface{}) {
	endpointSlice := obj.(*discovery.EndpointSlice)
	if endpointSlice == nil {
		utilruntime.HandleError(fmt.Errorf("invalid EndpointSlice provided to onEndpointSliceAdd()"))
		return
	}
	c.queueServiceForEndpointSlice(endpointSlice)
}

func (c *Controller) onEndpointSliceUpdate(prevObj, obj interface{}) {
	prevEndpointSlice := prevObj.(*discovery.EndpointSlice)
	endpointSlice := obj.(*discovery.EndpointSlice)

	// don't process resync or objects that are marked for deletion
	if prevEndpointSlice.ResourceVersion == endpointSlice.ResourceVersion ||
		!endpointSlice.GetDeletionTimestamp().IsZero() {
		return
	}
	c.queueServiceForEndpointSlice(endpointSlice)
}

func (c *Controller) onEndpointSliceDelete(obj interface{}) {
	endpointSlice, ok := obj.(*discovery.EndpointSlice)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("couldn't get object from tombstone %#v", obj))
			return
		}
		endpointSlice, ok = tombstone.Obj.(*discovery.EndpointSlice)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("tombstone contained object that is not a EndpointSlice: %#v", obj))
			return
		}
	}

	if endpointSlice != nil {
		c.queueServiceForEndpointSlice(endpointSlice)
	}
}

func (c *Controller) queueServiceForEndpointSlice(endpointSlice *discovery.EndpointSlice) {
	key, err := services.ServiceControllerKey(endpointSlice)
	if err != nil {
		// Do not log endpointsSlices missing service labels as errors.
		// Once the service label is eventually added, we will get this event
		// and re-process.
		if errors.Is(err, services.NoServiceLabelError) {
			klog.V(5).Infof("EgressService endpoint slice missing service label: %v", err)
		} else {
			utilruntime.HandleError(fmt.Errorf("couldn't get key for EndpointSlice %+v: %v", endpointSlice, err))
		}
		return
	}
	c.Lock()
	defer c.Unlock()
	_, cached := c.services[key]
	_, unallocated := c.unallocatedServices[key]
	if !cached && !unallocated {
		klog.V(5).Infof("Ignoring updating %s for endpointslice %s/%s as it is not a known egress service",
			key, endpointSlice.Namespace, endpointSlice.Name)
		return // we queue a service only if it's in the local caches
	}

	c.egressServiceQueue.Add(key)
}
