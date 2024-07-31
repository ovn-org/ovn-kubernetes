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

func (c *Controller) processNextNQOSNodeWorkItem(wg *sync.WaitGroup) bool {
	wg.Add(1)
	defer wg.Done()
	nqosNodeKey, quit := c.nqosNodeQueue.Get()
	if quit {
		return false
	}
	defer c.nqosNodeQueue.Done(nqosNodeKey)
	err := c.syncNetworkQoSNode(nqosNodeKey.(string))
	if err == nil {
		c.nqosNodeQueue.Forget(nqosNodeKey)
		return true
	}

	utilruntime.HandleError(fmt.Errorf("%v failed with: %v", nqosNodeKey, err))

	if c.nqosNodeQueue.NumRequeues(nqosNodeKey) < maxRetries {
		c.nqosNodeQueue.AddRateLimited(nqosNodeKey)
		return true
	}

	c.nqosNodeQueue.Forget(nqosNodeKey)
	return true
}

// syncNetworkQoSNode decides the main logic everytime
// we dequeue a key from the nqosNodeQueue cache
func (c *Controller) syncNetworkQoSNode(key string) error {
	c.Lock()
	defer c.Unlock()
	startTime := time.Now()
	// Iterate all NQOSes and check if this node start/stops matching
	// any NQOS and add/remove the setup accordingly. Nodes can match multiple
	// NQOS objects, so continue iterating all NQOS objects before finishing.
	_, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}
	klog.V(5).Infof("Processing sync for Node %s in Network QoS controller", name)

	defer func() {
		klog.V(5).Infof("Finished syncing Node %s Network QoS controller: took %v", name, time.Since(startTime))
	}()
	node, err := c.nqosNodeLister.Get(name)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	// (i) node add
	// (ii) node update because node's labels changed OR node's HostCIDR annotation changed
	// (iii) node delete
	// In all these cases check which NQOSes were managing this pod and enqueue them back to nqosQueue
	existingNQOSes, err := c.nqosNodeLister.List(labels.Everything())
	if err != nil {
		return err
	}
	// case(iii)
	if node == nil {
		for _, nqos := range existingNQOSes {
			cachedName := joinMetaNamespaceAndName(nqos.Namespace, nqos.Name)
			nqosObj, loaded := c.nqosCache[cachedName]
			if !loaded {
				continue
			}
			c.clearNodeForNQOS(name, nqosObj, c.nqosQueue)
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
		c.setNodeForNQOS(node, nqosObj, c.nqosQueue)
	}
	return nil
}

// clearNodeForNQOS will handle the logic for figuring out if the provided node name
// TODO...
func (c *Controller) clearNodeForNQOS(name string, nqosCache *networkQoSState, queue workqueue.RateLimitingInterface) {
	// TODO: Implement-me!
}

// setNodeForNQOS will handle the logic for figuring out if the provided node name
// TODO...
func (c *Controller) setNodeForNQOS(node *v1.Node, nqosCache *networkQoSState, queue workqueue.RateLimitingInterface) {
	// TODO: Implement-me!
}
