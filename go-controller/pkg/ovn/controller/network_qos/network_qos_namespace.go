package networkqos

import (
	"fmt"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
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

	err := c.syncNetworkQoSNamespace(nqosNSKey.(string))
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
	c.Lock()
	defer c.Unlock()
	startTime := time.Now()
	_, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}
	klog.V(5).Infof("Processing sync for Namespace %s in Network QoS controller", name)

	defer func() {
		klog.V(5).Infof("Finished syncing Namespace %s Network QoS controller: took %v", name, time.Since(startTime))
	}()
	namespace, err := c.nqosNamespaceLister.Get(name)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	// (i) namespace add
	// (ii) namespace update because namespace's labels changed
	// (iii) namespace delete
	// In all these cases check which NQOSes were managing this namespace and enqueue them back to nqosQueue
	existingNQOSes, err := c.nqosLister.List(labels.Everything())
	if err != nil {
		return err
	}
	// case (iii)
	if namespace == nil {
		for _, nqos := range existingNQOSes {
			cachedName := joinMetaNamespaceAndName(nqos.Namespace, nqos.Name)
			nqosObj, loaded := c.nqosCache[cachedName]
			if !loaded {
				continue
			}
			c.clearNamespaceForNQOS(name, nqosObj, c.nqosQueue)
		}
		return nil
	}
	// case (i)/(ii)
	for _, nqos := range existingNQOSes {
		cachedName := joinMetaNamespaceAndName(nqos.Namespace, nqos.Name)
		nqosObj, loaded := c.nqosCache[cachedName]
		if !loaded {
			continue
		}
		c.setNamespaceForNQOS(namespace, nqosObj, c.nqosQueue)
	}
	return nil
}

// clearNamespaceForNQOS will handle the logic for figuring out if the provided namespace name
// TODO...
func (c *Controller) clearNamespaceForNQOS(name string, nqosCache *networkQoSState, queue workqueue.RateLimitingInterface) {
	// TODO: Implement-me!
}

// setNamespaceForNQOS will handle the logic for figuring out if the provided namespace name
// TODO...
func (c *Controller) setNamespaceForNQOS(namespace *v1.Namespace, nqosCache *networkQoSState, queue workqueue.RateLimitingInterface) {
	// TODO: Implement-me!
}
