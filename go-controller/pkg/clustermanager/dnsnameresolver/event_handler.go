package dnsnameresolver

import (
	"fmt"
	"reflect"

	ocpnetworkapiv1alpha1 "github.com/openshift/api/network/v1alpha1"
	egressfirewall "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/retry"
	"k8s.io/client-go/tools/cache"
)

// EventHandler handles DNS name related events.
type EventHandler struct {
	retry.DefaultEventHandler
	watchFactory *factory.WatchFactory
	objType      reflect.Type
	c            *Controller
}

// SyncFunc syncs the existing objects according to its type and returns an error, if any, yielded
// during the sync process.
func (eh *EventHandler) SyncFunc(objs []interface{}) error {
	var syncFunc func([]interface{}) error

	switch eh.objType {
	case factory.EgressFirewallType:
		syncFunc = nil

	case factory.DNSNameResolverType:
		syncFunc = eh.c.syncDNSNames

	default:
		return fmt.Errorf("no sync function for object type %s", eh.objType)
	}

	if syncFunc == nil {
		return nil
	}
	return syncFunc(objs)
}

// AddResource adds the specified object to the cluster according to its type and returns the error,
// if any, yielded during object creation. Given a *RetryFramework instance, an object to add and a
// boolean specifying if the function was executed from iterateRetryResources.
func (eh *EventHandler) AddResource(obj interface{}, fromRetryLoop bool) error {
	// switch based on type
	switch eh.objType {

	case factory.EgressFirewallType:
		egressFirewall := obj.(*egressfirewall.EgressFirewall)
		return eh.c.egressFirewallChanged(nil, egressFirewall)

	case factory.DNSNameResolverType:
		dnsNameResolver := obj.(*ocpnetworkapiv1alpha1.DNSNameResolver)
		return eh.c.dnsNameResolverChanged(nil, dnsNameResolver)
	}

	return fmt.Errorf("no object comparison for type %s", eh.objType)
}

// DeleteResource deletes the object from the cluster according to the delete logic of its resource type.
// Given an object and optionally a cachedObj; cachedObj is the internal cache entry for this object,
// used for now for pods and network policies.
func (eh *EventHandler) DeleteResource(obj, cachedObj interface{}) error {
	// switch based on type
	switch eh.objType {

	case factory.EgressFirewallType:
		egressFirewall := obj.(*egressfirewall.EgressFirewall)
		return eh.c.egressFirewallChanged(egressFirewall, nil)

	case factory.DNSNameResolverType:
		dnsNameResolver := obj.(*ocpnetworkapiv1alpha1.DNSNameResolver)
		return eh.c.dnsNameResolverChanged(dnsNameResolver, nil)
	}

	return fmt.Errorf("no object comparison for type %s", eh.objType)
}

// UpdateResource updates the specified object in the cluster to its version in newObj according to its
// type and returns the error, if any, yielded during the object update. Given an old and a new object;
// The inRetryCache boolean argument is to indicate if the given resource is in the retryCache or not.
func (eh *EventHandler) UpdateResource(oldObj, newObj interface{}, inRetryCache bool) error {
	// switch based on type
	switch eh.objType {

	case factory.EgressFirewallType:
		oldEgressFirewall := oldObj.(*egressfirewall.EgressFirewall)
		newEgressFirewall := newObj.(*egressfirewall.EgressFirewall)
		return eh.c.egressFirewallChanged(oldEgressFirewall, newEgressFirewall)

	case factory.DNSNameResolverType:
		oldDNSNameResolver := oldObj.(*ocpnetworkapiv1alpha1.DNSNameResolver)
		newDNSNameResolver := oldObj.(*ocpnetworkapiv1alpha1.DNSNameResolver)
		return eh.c.dnsNameResolverChanged(oldDNSNameResolver, newDNSNameResolver)
	}

	return fmt.Errorf("no object comparison for type %s", eh.objType)
}

// GetResourceFromInformerCache returns the latest state of the object, given an object key and its type.
// from the informers cache.
func (eh *EventHandler) GetResourceFromInformerCache(key string) (interface{}, error) {
	var obj interface{}
	var namespace, name string
	var err error

	namespace, name, err = cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return nil, fmt.Errorf("failed to split key %s: %v", key, err)
	}

	// switch based on type
	switch eh.objType {

	case factory.EgressFirewallType:
		obj, err = eh.watchFactory.GetEgressFirewall(namespace, name)

	case factory.DNSNameResolverType:
		obj, err = eh.watchFactory.GetDNSNameResolver(namespace, name)

	default:
		return nil, fmt.Errorf("object type %s not supported, cannot retrieve it from informers cache", eh.objType)
	}
	return obj, err
}

// AreResourcesEqual returns true if, given two objects of a known resource type, the update logic for this resource
// type considers them equal and therefore no update is needed. It returns false when the two objects are not considered
// equal and an update needs be executed. This is regardless of how the update is carried out (whether with a dedicated
// update function or with a delete on the old obj followed by an add on the new obj).
func (eh *EventHandler) AreResourcesEqual(obj1, obj2 interface{}) (bool, error) {
	// switch based on type
	switch eh.objType {

	case factory.EgressFirewallType:
		oldEgressFirewall, ok := obj1.(*egressfirewall.EgressFirewall)
		if !ok {
			return false, fmt.Errorf("could not cast obj1 of type %T to *egressfirewall.EgressFirewall", obj1)
		}
		newEgressFirewall, ok := obj2.(*egressfirewall.EgressFirewall)
		if !ok {
			return false, fmt.Errorf("could not cast obj2 of type %T to *egressfirewall.EgressFirewall", obj2)
		}
		return reflect.DeepEqual(oldEgressFirewall.Spec, newEgressFirewall.Spec), nil

	case factory.DNSNameResolverType:
		oldDNSNameResolver, ok := obj1.(*ocpnetworkapiv1alpha1.DNSNameResolver)
		if !ok {
			return false, fmt.Errorf("could not cast obj1 of type %T to *ocpnetworkapiv1alpha1.DNSNameResolver", obj1)
		}
		newDNSNameResolver, ok := obj2.(*ocpnetworkapiv1alpha1.DNSNameResolver)
		if !ok {
			return false, fmt.Errorf("could not cast obj2 of type %T to *ocpnetworkapiv1alpha1.DNSNameResolver", obj2)
		}
		return reflect.DeepEqual(oldDNSNameResolver.Status, newDNSNameResolver.Status), nil

	}

	return false, fmt.Errorf("no object comparison for type %s", eh.objType)
}
