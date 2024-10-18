package adminnetworkpolicy

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

func (c *Controller) processNextANPNodeWorkItem(wg *sync.WaitGroup) bool {
	wg.Add(1)
	defer wg.Done()
	anpNodeKey, quit := c.anpNodeQueue.Get()
	if quit {
		return false
	}
	defer c.anpNodeQueue.Done(anpNodeKey)
	err := c.syncAdminNetworkPolicyNode(anpNodeKey)
	if err == nil {
		c.anpNodeQueue.Forget(anpNodeKey)
		return true
	}

	utilruntime.HandleError(fmt.Errorf("%v failed with: %v", anpNodeKey, err))

	if c.anpNodeQueue.NumRequeues(anpNodeKey) < maxRetries {
		c.anpNodeQueue.AddRateLimited(anpNodeKey)
		return true
	}

	c.anpNodeQueue.Forget(anpNodeKey)
	return true
}

// syncAdminNetworkPolicyNode decides the main logic everytime
// we dequeue a key from the anpNodeQueue cache
func (c *Controller) syncAdminNetworkPolicyNode(key string) error {
	c.Lock()
	defer c.Unlock()
	startTime := time.Now()
	// Iterate all ANPs and check if this node start/stops matching
	// any ANP and add/remove the setup accordingly. Nodes can match multiple
	// ANPs objects, so continue iterating all ANP objects before finishing.
	_, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}
	klog.V(5).Infof("Processing sync for Node %s in Admin Network Policy controller", name)

	defer func() {
		klog.V(5).Infof("Finished syncing Node %s Admin Network Policy controller: took %v", name, time.Since(startTime))
	}()
	node, err := c.anpNodeLister.Get(name)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	// (i) node add
	// (ii) node update because node's labels changed OR node's HostCIDR annotation changed
	// (iii) node delete
	// In all these cases check which ANPs were managing this pod and enqueue them back to anpQueue
	existingANPs, err := c.anpLister.List(labels.Everything())
	if err != nil {
		return err
	}
	// case(iii)
	if node == nil {
		for _, anp := range existingANPs {
			anpObj, loaded := c.anpCache[anp.Name]
			if !loaded {
				continue
			}
			c.clearNodeForANP(name, anpObj, c.anpQueue)
		}
		banpObj := c.banpCache
		if banpObj.name == "" { // empty struct, BANP is not setup yet
			return nil
		}
		c.clearNodeForANP(name, banpObj, c.banpQueue)
		return nil
	}
	// case (i)/(ii)
	for _, anp := range existingANPs {
		anpObj, loaded := c.anpCache[anp.Name]
		if !loaded {
			continue
		}
		c.setNodeForANP(node, anpObj, c.anpQueue)
	}
	banpObj := c.banpCache
	if banpObj.name == "" { // empty struct, BANP is not setup yet
		return nil
	}
	c.setNodeForANP(node, banpObj, c.banpQueue)
	return nil
}

// clearNodeFor(B)ANP will handle the logic for figuring out if the provided node name
// used to match the given anpCache.name policy. If so, it will requeue the anpCache.name key back
// into the main (b)anpQueue cache for reconciling the db objects. If not, function is a no-op.
func (c *Controller) clearNodeForANP(name string, anpCache *adminNetworkPolicyState, queue workqueue.TypedRateLimitingInterface[string]) {
	// (i) check if it used to match any of the .Spec.Egress.Peers requeue it and return
	for _, rule := range anpCache.egressRules {
		for _, peer := range rule.peers {
			if peer.nodes.Has(name) {
				klog.V(4).Infof("Node %s used to match ANP %s egress rule %d peer, requeuing...", name, anpCache.name, rule.priority)
				queue.Add(anpCache.name)
				return
			}
		}
	}
}

// setNodeForANP will handle the logic for figuring out if the provided node name
// used to match the given anpCache.name policy or if it started matching the given anpCache.name.
// If so, it will requeue the anpCache.name key back into the main (b)anpQueue cache for reconciling
// the db objects. If not, function is a no-op.
func (c *Controller) setNodeForANP(node *v1.Node, anpCache *adminNetworkPolicyState, queue workqueue.TypedRateLimitingInterface[string]) {
	// (i) if above conditions are are false, check if it used to match any of the .Spec.Egress.Peers requeue it and return OR
	// (ii) check if it started to match any of the .Spec.Egress.Peers requeue it and return
	// The goal is to check if this node matches the ANP in at least one of the above ways, we immediately add key
	// and return. We don't really need to process all the rules and all the combinations. But worst case is if
	// node matches the last rule's peer in egress in which we case we go through everything (max: 20,000 gress rules).
	// case(i)/(ii)
	nodeLabels := labels.Set(node.Labels)
	// case(i)/(ii)
	for _, rule := range anpCache.egressRules {
		for _, peer := range rule.peers {
			// case(i)
			if peer.nodes.Has(node.Name) {
				klog.V(5).Infof("Node %s used to match ANP %s egress rule %d peer, requeuing...", node.Name, anpCache.name, rule.priority)
				queue.Add(anpCache.name)
				return
			}
			// case(ii)
			if peer.nodeSelector.Matches(nodeLabels) {
				klog.V(4).Infof("Node %s  started to match ANP %s egress rule %d peer, requeuing...", node.Name, anpCache.name, rule.priority)
				queue.Add(anpCache.name)
				return
			}
		}
	}
	// if nothing matched this node, then its a no-op
}
