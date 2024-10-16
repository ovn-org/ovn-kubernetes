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

func (c *Controller) processNextANPNamespaceWorkItem(wg *sync.WaitGroup) bool {
	wg.Add(1)
	defer wg.Done()
	anpNSKey, quit := c.anpNamespaceQueue.Get()
	if quit {
		return false
	}
	defer c.anpNamespaceQueue.Done(anpNSKey)

	err := c.syncAdminNetworkPolicyNamespace(anpNSKey)
	if err == nil {
		c.anpNamespaceQueue.Forget(anpNSKey)
		return true
	}

	utilruntime.HandleError(fmt.Errorf("%v failed with: %v", anpNSKey, err))

	if c.anpNamespaceQueue.NumRequeues(anpNSKey) < maxRetries {
		c.anpNamespaceQueue.AddRateLimited(anpNSKey)
		return true
	}

	c.anpNamespaceQueue.Forget(anpNSKey)
	return true
}

// syncAdminNetworkPolicyNamespace decides the main logic everytime
// we dequeue a key from the anpNamespaceQueue cache
func (c *Controller) syncAdminNetworkPolicyNamespace(key string) error {
	c.Lock()
	defer c.Unlock()
	startTime := time.Now()
	_, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}
	klog.V(5).Infof("Processing sync for Namespace %s in Admin Network Policy controller", name)

	defer func() {
		klog.V(5).Infof("Finished syncing Namespace %s Admin Network Policy controller: took %v", name, time.Since(startTime))
	}()
	namespace, err := c.anpNamespaceLister.Get(name)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	// (i) namespace add
	// (ii) namespace update because namespace's labels changed
	// (iii) namespace delete
	// In all these cases check which ANPs were managing this namespace and enqueue them back to anpQueue
	existingANPs, err := c.anpLister.List(labels.Everything())
	if err != nil {
		return err
	}
	// case (iii)
	if namespace == nil {
		for _, anp := range existingANPs {
			anpObj, loaded := c.anpCache[anp.Name]
			if !loaded {
				continue
			}
			c.clearNamespaceForANP(name, anpObj, c.anpQueue)
		}
		banpObj := c.banpCache
		if banpObj.name == "" {
			return nil
		}
		c.clearNamespaceForANP(name, banpObj, c.banpQueue)
		return nil
	}
	// case (i)/(ii)
	for _, anp := range existingANPs {
		anpObj, loaded := c.anpCache[anp.Name]
		if !loaded {
			continue
		}
		c.setNamespaceForANP(namespace, anpObj, c.anpQueue)
	}
	banpObj := c.banpCache
	if banpObj.name == "" { // empty struct, BANP is not setup yet
		return nil
	}
	c.setNamespaceForANP(namespace, banpObj, c.banpQueue)
	return nil
}

// clearNamespaceFor(B)ANP will handle the logic for figuring out if the provided namespace name
// used to match the given anpCache.name policy. If so, it will requeue the anpCache.name key back
// into the main (b)anpQueue cache for reconciling the db objects. If not, function is a no-op.
func (c *Controller) clearNamespaceForANP(name string, anpCache *adminNetworkPolicyState, queue workqueue.TypedRateLimitingInterface[string]) {
	// (i) if this namespace used to match this ANP's .Spec.Subject requeue it and return
	// (ii) if (i) is false, check if it used to match any of the .Spec.Ingress.Peers requeue it and return
	// (iii) if (i) & (ii) are false, check if it used to match any of the .Spec.Egress.Peers requeue it and return
	if _, ok := anpCache.subject.namespaces[name]; ok {
		klog.V(4).Infof("Namespace %s used to match ANP %s subject, requeuing...", name, anpCache.name)
		queue.Add(anpCache.name)
		return
	}
	for _, rule := range anpCache.ingressRules {
		for _, peer := range rule.peers {
			if _, ok := peer.namespaces[name]; ok {
				klog.V(4).Infof("Namespace %s used to match ANP %s ingress rule %d peer, requeuing...", name, anpCache.name, rule.priority)
				queue.Add(anpCache.name)
				return
			}
		}
	}
	for _, rule := range anpCache.egressRules {
		for _, peer := range rule.peers {
			if _, ok := peer.namespaces[name]; ok {
				klog.V(4).Infof("Namespace %s used to match ANP %s egress rule %d peer, requeuing...", name, anpCache.name, rule.priority)
				queue.Add(anpCache.name)
				return
			}
		}
	}
}

// setNamespaceForANP will handle the logic for figuring out if the provided namespace name
// used to match the given anpCache.name policy or if it started matching the given anpCache.name.
// If so, it will requeue the anpCache.name key back into the main (b)anpQueue cache for reconciling
// the db objects. If not, function is a no-op.
func (c *Controller) setNamespaceForANP(namespace *v1.Namespace, anpCache *adminNetworkPolicyState, queue workqueue.TypedRateLimitingInterface[string]) {
	// (i) if this namespace used to match this ANP's .Spec.Subject requeue it and return OR
	// (ii) if this namespace started to match this ANP's .Spec.Subject requeue it and return OR
	// (iii) if above conditions are are false, check if it used to match any of the .Spec.Ingress.Peers requeue it and return OR
	// (iv) check if it started to match any of the .Spec.Ingress.Peers requeue it and return OR
	// (v) if above conditions are are false, check if it used to match any of the .Spec.Egress.Peers requeue it and return OR
	// (vi) check if it started to match any of the .Spec.Egress.Peers requeue it and return
	// The goal is to check if this namespace matches the ANP in at least one of the above ways, we immediately add key
	// and return. We don't really need to process all the rules and all the combinations. But worst case is if
	// namespace matches the last rule's peer in egress in which we case we go through everything (max: 20,000 gress rules).
	// case(i)
	if _, ok := anpCache.subject.namespaces[namespace.Name]; ok {
		klog.V(5).Infof("Namespace %s used to match ANP %s subject, requeuing...", namespace.Name, anpCache.name)
		queue.Add(anpCache.name)
		return
	}
	// case(ii)
	nsLabels := labels.Set(namespace.Labels)
	if anpCache.subject.namespaceSelector.Matches(nsLabels) {
		klog.V(4).Infof("Namespace %s started to match ANP %s subject, requeuing...", namespace.Name, anpCache.name)
		queue.Add(anpCache.name)
		return
	}
	// case(iii)/(iv)
	for _, rule := range anpCache.ingressRules {
		for _, peer := range rule.peers {
			// case(iii)
			if _, ok := peer.namespaces[namespace.Name]; ok {
				klog.V(5).Infof("Namespace %s used to match ANP %s ingress rule %d peer, requeuing...", namespace.Name, anpCache.name, rule.priority)
				queue.Add(anpCache.name)
				return
			}
			// case(iv)
			if peer.namespaceSelector.Matches(nsLabels) {
				klog.V(4).Infof("Namespace %s  started to match ANP %s ingress rule %d peer, requeuing...", namespace.Name, anpCache.name, rule.priority)
				queue.Add(anpCache.name)
				return
			}
		}
	}
	// case(v)/(vi)
	for _, rule := range anpCache.egressRules {
		for _, peer := range rule.peers {
			// case(v)
			if _, ok := peer.namespaces[namespace.Name]; ok {
				klog.V(5).Infof("Namespace %s used to match ANP %s egress rule %d peer, requeuing...", namespace.Name, anpCache.name, rule.priority)
				queue.Add(anpCache.name)
				return
			}
			// case(vi)
			if peer.namespaceSelector.Matches(nsLabels) {
				klog.V(4).Infof("Namespace %s  started to match ANP %s egress rule %d peer, requeuing...", namespace.Name, anpCache.name, rule.priority)
				queue.Add(anpCache.name)
				return
			}
		}
	}
	// if nothing matched this namespace, then its a no-op
}
