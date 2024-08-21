package adminnetworkpolicy

import (
	"fmt"
	"sync"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
)

func (c *Controller) processNextANPPodWorkItem(wg *sync.WaitGroup) bool {
	wg.Add(1)
	defer wg.Done()
	anpPodKey, quit := c.anpPodQueue.Get()
	if quit {
		return false
	}
	defer c.anpPodQueue.Done(anpPodKey)
	err := c.syncAdminNetworkPolicyPod(anpPodKey)
	if err == nil {
		c.anpPodQueue.Forget(anpPodKey)
		return true
	}

	utilruntime.HandleError(fmt.Errorf("%v failed with: %v", anpPodKey, err))

	if c.anpPodQueue.NumRequeues(anpPodKey) < maxRetries {
		c.anpPodQueue.AddRateLimited(anpPodKey)
		return true
	}

	c.anpPodQueue.Forget(anpPodKey)
	return true
}

// syncAdminNetworkPolicyPod decides the main logic everytime
// we dequeue a key from the anpPodQueue cache
func (c *Controller) syncAdminNetworkPolicyPod(key string) error {
	c.Lock()
	defer c.Unlock()
	startTime := time.Now()
	// Iterate all ANPs and check if this namespace start/stops matching
	// any ANP and add/remove the setup accordingly. Namespaces can match multiple
	// ANPs objects, so continue iterating all ANP objects before finishing.
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}
	klog.V(5).Infof("Processing sync for Pod %s/%s in Admin Network Policy controller", namespace, name)

	defer func() {
		klog.V(5).Infof("Finished syncing Pod %s/%s Admin Network Policy controller: took %v", namespace, name, time.Since(startTime))
	}()
	ns, err := c.anpNamespaceLister.Get(namespace)
	if err != nil {
		return err
	}
	namespaceLabels := labels.Set(ns.Labels)
	podNamespaceLister := c.anpPodLister.Pods(namespace)
	pod, err := podNamespaceLister.Get(name)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	// (i) pod add
	// (ii) pod update because LSP and IPAM is done OR pod's labels changed
	// (iii) pod update because pod went into completed state
	// (iv) pod delete
	// In all these cases check which ANPs were managing this pod and enqueue them back to anpQueue
	existingANPs, err := c.anpLister.List(labels.Everything())
	if err != nil {
		return err
	}
	// case(iii)/(iv)
	if pod == nil || util.PodCompleted(pod) {
		for _, anp := range existingANPs {
			anpObj, loaded := c.anpCache[anp.Name]
			if !loaded {
				continue
			}
			c.clearPodForANP(namespace, name, anpObj, c.anpQueue)
		}
		banpObj := c.banpCache
		if banpObj.name == "" { // empty struct, BANP is not setup yet
			return nil
		}
		c.clearPodForANP(namespace, name, banpObj, c.banpQueue)
		return nil
	}
	// We don't want to shortcuit only local zone pods here since peer pods
	// whether local or remote need to be dealt with. So we let the main
	// ANP controller take care of the local zone pods logic for the policy subjects
	if !util.PodScheduled(pod) || util.PodWantsHostNetwork(pod) {
		// we don't support ANP with host-networked pods
		// if pod is no scheduled yet, return and we can process it on its next update
		// because anyways at that stage pod is considered to belong to remote zone
		return nil
	}
	// case (i)/(ii)
	for _, anp := range existingANPs {
		anpObj, loaded := c.anpCache[anp.Name]
		if !loaded {
			continue
		}
		c.setPodForANP(pod, anpObj, namespaceLabels, c.anpQueue)
	}
	banpObj := c.banpCache
	if banpObj.name == "" { // empty struct, BANP is not setup yet
		return nil
	}
	c.setPodForANP(pod, banpObj, namespaceLabels, c.banpQueue)

	return nil
}

// clearPodForANP will handle the logic for figuring out if the provided pod name
// used to match the given anpCache.name policy. If so, it will requeue the anpCache.name key back
// into the main (b)anpQueue cache for reconciling the db objects. If not, function is a no-op.
func (c *Controller) clearPodForANP(namespace, name string, anpCache *adminNetworkPolicyState, queue workqueue.TypedRateLimitingInterface[string]) {
	// (i) if this pod used to match this ANP's .Spec.Subject requeue it and return
	// (ii) if (i) is false, check if it used to match any of the .Spec.Ingress.Peers requeue it and return
	// (iii) if (i) & (ii) are false, check if it used to match any of the .Spec.Egress.Peers requeue it and return
	if podCache, ok := anpCache.subject.namespaces[namespace]; ok {
		if podCache.Has(name) {
			klog.V(4).Infof("Pod %s/%s used to match ANP %s subject, requeuing...", namespace, name, anpCache.name)
			queue.Add(anpCache.name)
			return
		}
	}
	for _, rule := range anpCache.ingressRules {
		for _, peer := range rule.peers {
			if podCache, ok := peer.namespaces[namespace]; ok {
				if podCache.Has(name) {
					klog.V(4).Infof("Pod %s/%s used to match ANP %s ingress rule %d peer, requeuing...", namespace, name, anpCache.name, rule.priority)
					queue.Add(anpCache.name)
					return
				}
			}
		}
	}
	for _, rule := range anpCache.egressRules {
		for _, peer := range rule.peers {
			if podCache, ok := peer.namespaces[namespace]; ok {
				if podCache.Has(name) {
					klog.V(4).Infof("Pod %s/%s used to match ANP %s egress rule %d peer, requeuing...", namespace, name, anpCache.name, rule.priority)
					queue.Add(anpCache.name)
					return
				}
			}
		}
	}
	// if nothing matched this pod, then its a no-op
}

// setPodForANP will handle the logic for figuring out if the provided pod name
// used to match the given anpCache.name policy or if it started matching the given anpCache.name.
// If so, it will requeue the anpCache.name key back into the main (b)anpQueue cache for reconciling
// the db objects. If not, function is a no-op.
func (c *Controller) setPodForANP(pod *v1.Pod, anpCache *adminNetworkPolicyState, namespaceLabels labels.Labels, queue workqueue.TypedRateLimitingInterface[string]) {
	// (i) if this pod used to match this ANP's .Spec.Subject requeue it and return OR
	// (ii) if this pod started to match this ANP's .Spec.Subject requeue it and return OR
	// (iii) if above conditions are are false, check if it used to match any of the .Spec.Ingress.Peers requeue it and return OR
	// (iv) check if it started to match any of the .Spec.Ingress.Peers requeue it and return OR
	// (v) if above conditions are are false, check if it used to match any of the .Spec.Egress.Peers requeue it and return OR
	// (vi) check if it started to match any of the .Spec.Egress.Peers requeue it and return
	// The goal is to check if this pod matches the ANP in at least one of the above ways, we immediately add key
	// and return. We don't really need to process all the rules and all the combinations. But worst case is if
	// pod matches the last rule's peer in egress in which we case we go through everything (max: 20,000 gress rules).
	// case(i)
	if podCache, ok := anpCache.subject.namespaces[pod.Namespace]; ok {
		if podCache.Has(pod.Name) {
			klog.V(5).Infof("Pod %s/%s used to match ANP %s subject, requeuing...", pod.Namespace, pod.Name, anpCache.name)
			queue.Add(anpCache.name)
			return
		}
	}
	// case(ii)
	podLabels := labels.Set(pod.Labels)
	if anpCache.subject.namespaceSelector.Matches(namespaceLabels) && anpCache.subject.podSelector.Matches(podLabels) {
		klog.V(4).Infof("Pod %s/%s started to match ANP %s subject, requeuing...", pod.Namespace, pod.Name, anpCache.name)
		queue.Add(anpCache.name)
		return
	}
	// case(iii)/(iv)
	for _, rule := range anpCache.ingressRules {
		for _, peer := range rule.peers {
			// case(iii)
			if podCache, ok := peer.namespaces[pod.Namespace]; ok {
				if podCache.Has(pod.Name) {
					klog.V(5).Infof("Pod %s/%s used to match ANP %s ingress rule %d peer, requeuing...", pod.Namespace, pod.Name, anpCache.name, rule.priority)
					queue.Add(anpCache.name)
					return
				}
			}
			// case(iv)
			if peer.namespaceSelector.Matches(namespaceLabels) && peer.podSelector.Matches(podLabels) {
				klog.V(4).Infof("Pod %s/%s started to match ANP %s ingress rule %d peer, requeuing...", pod.Namespace, pod.Name, anpCache.name, rule.priority)
				queue.Add(anpCache.name)
				return
			}
		}
	}
	// case(v)/(vi)
	for _, rule := range anpCache.egressRules {
		for _, peer := range rule.peers {
			// case(v)
			if podCache, ok := peer.namespaces[pod.Namespace]; ok {
				if podCache.Has(pod.Name) {
					klog.V(5).Infof("Pod %s/%s used to match ANP %s egress rule %d peer, requeuing...", pod.Namespace, pod.Name, anpCache.name, rule.priority)
					queue.Add(anpCache.name)
					return
				}
			}
			// case(vi)
			if peer.namespaceSelector.Matches(namespaceLabels) && peer.podSelector.Matches(podLabels) {
				klog.V(4).Infof("Pod %s/%s started to match ANP %s egress rule %d peer, requeuing...", pod.Namespace, pod.Name, anpCache.name, rule.priority)
				queue.Add(anpCache.name)
				return
			}
		}
	}
	// if nothing matched this pod, then its a no-op
}
