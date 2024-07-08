package networkqos

import (
	"fmt"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/klog/v2"
)

func (c *Controller) processNextNQOSNamespaceWorkItem(wg *sync.WaitGroup) bool {
	wg.Add(1)
	defer wg.Done()
	nqosNSKey, quit := c.nqosNamespaceQueue.Get()
	if quit {
		return false
	}
	defer c.nqosNamespaceQueue.Done(nqosNSKey)

	err := c.syncNetworkQoSNamespace(nqosNSKey)
	if err == nil {
		c.nqosNamespaceQueue.Forget(nqosNSKey)
		return true
	}

	utilruntime.HandleError(fmt.Errorf("%v failed with: %v", nqosNSKey, err))

	if c.nqosNamespaceQueue.NumRequeues(nqosNSKey) < maxRetries {
		c.nqosNamespaceQueue.AddRateLimited(nqosNSKey)
		return true
	}

	c.nqosNamespaceQueue.Forget(nqosNSKey)
	return true
}

// syncNetworkQoSNamespace decides the main logic everytime
// we dequeue a key from the nqosNamespaceQueue cache
func (c *Controller) syncNetworkQoSNamespace(key string) error {
	startTime := time.Now()
	klog.V(5).Infof("Processing sync for Namespace %s in Network QoS controller", key)
	defer func() {
		klog.V(5).Infof("Finished syncing Namespace %s Network QoS controller: took %v", key, time.Since(startTime))
	}()
	namespace, err := c.nqosNamespaceLister.Get(key)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	// (i) namespace add
	// (ii) namespace update because namespace's labels changed
	// (iii) namespace delete
	// case (iii)
	if namespace == nil {
		for _, cachedKey := range c.nqosCache.GetKeys() {
			err := c.nqosCache.DoWithLock(cachedKey, func(nqosKey string) error {
				if nqosObj, loaded := c.nqosCache.Load(nqosKey); loaded {
					return c.clearNamespaceForNQOS(key, nqosObj)
				}
				return nil
			})
			if err != nil {
				return err
			}
		}
		recordNamespaceReconcileDuration(c.controllerName, time.Since(startTime).Milliseconds())
		return nil
	}
	// case (i)/(ii)
	for _, cachedKey := range c.nqosCache.GetKeys() {
		err := c.nqosCache.DoWithLock(cachedKey, func(nqosKey string) error {
			if nqosObj, loaded := c.nqosCache.Load(nqosKey); loaded {
				return c.setNamespaceForNQOS(namespace, nqosObj)
			} else {
				klog.Warningf("NetworkQoS not synced yet: %s", nqosKey)
				// requeue nqos key to sync it
				c.nqosQueue.Add(nqosKey)
				// requeue namespace key 3 seconds later, allow NetworkQoS to be handled
				c.nqosNamespaceQueue.AddAfter(key, 3*time.Second)
				return nil
			}
		})
		if err != nil {
			return err
		}
	}
	recordNamespaceReconcileDuration(c.controllerName, time.Since(startTime).Milliseconds())
	return nil
}

// clearNamespaceForNQOS will handle the logic for figuring out if the provided namespace name
// has pods that affect address sets of the cached network qoses. If so, remove them.
func (c *Controller) clearNamespaceForNQOS(namespace string, nqosState *networkQoSState) error {
	for _, rule := range nqosState.EgressRules {
		if rule.Classifier == nil {
			continue
		}
		for _, dest := range rule.Classifier.Destinations {
			if err := dest.removePodsInNamespace(namespace); err != nil {
				return fmt.Errorf("error removing IPs from dest address set %s: %v", dest.DestAddrSet.GetName(), err)
			}
		}
	}
	return nil
}

// setNamespaceForNQOS will handle the logic for figuring out if the provided namespace name
// has pods that need to populate or removed from the address sets of the network qoses.
func (c *Controller) setNamespaceForNQOS(namespace *v1.Namespace, nqosState *networkQoSState) error {
	for _, rule := range nqosState.EgressRules {
		if rule.Classifier == nil {
			continue
		}
		for index, dest := range rule.Classifier.Destinations {
			if dest.PodSelector == nil && dest.NamespaceSelector == nil {
				// no selectors, no address set
				continue
			}
			if !dest.matchNamespace(namespace, nqosState.namespace) {
				if err := dest.removePodsInNamespace(namespace.Name); err != nil {
					return fmt.Errorf("error removing pods in namespace %s from NetworkQoS %s/%s rule %d: %v", namespace.Name, nqosState.namespace, nqosState.name, index, err)
				}
				continue
			}
			// add matching pods in the namespace to dest
			if err := dest.addPodsInNamespace(c, namespace.Name); err != nil {
				return err
			}
			klog.V(5).Infof("Added pods in namespace %s for NetworkQoS %s/%s rule %d", namespace.Name, nqosState.namespace, nqosState.name, index)
		}
	}
	return nil
}
