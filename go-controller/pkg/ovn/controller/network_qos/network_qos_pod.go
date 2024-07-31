package networkqos

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

func (c *Controller) processNextNQOSPodWorkItem(wg *sync.WaitGroup) bool {
	wg.Add(1)
	defer wg.Done()
	nqosPodKey, quit := c.nqosPodQueue.Get()
	if quit {
		return false
	}
	defer c.nqosPodQueue.Done(nqosPodKey)
	err := c.syncNetworkQoSPod(nqosPodKey.(string))
	if err == nil {
		c.nqosPodQueue.Forget(nqosPodKey)
		return true
	}

	utilruntime.HandleError(fmt.Errorf("%v failed with: %v", nqosPodKey, err))

	if c.nqosPodQueue.NumRequeues(nqosPodKey) < maxRetries {
		c.nqosPodQueue.AddRateLimited(nqosPodKey)
		return true
	}

	c.nqosPodQueue.Forget(nqosPodKey)
	return true
}

// syncNetworkQoSPod decides the main logic everytime
// we dequeue a key from the nqosPodQueue cache
func (c *Controller) syncNetworkQoSPod(key string) error {
	c.Lock()
	defer c.Unlock()
	startTime := time.Now()
	// Iterate all NQOses and check if this namespace start/stops matching
	// any NQOS and add/remove the setup accordingly. Namespaces can match multiple
	// NQOses objects, so continue iterating all NQOS objects before finishing.
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}
	klog.V(5).Infof("Processing sync for Pod %s/%s in Network QoS controller", namespace, name)

	defer func() {
		klog.V(5).Infof("Finished syncing Pod %s/%s Network QoS controller: took %v", namespace, name, time.Since(startTime))
	}()
	ns, err := c.nqosNamespaceLister.Get(namespace)
	if err != nil {
		return err
	}
	namespaceLabels := labels.Set(ns.Labels)
	podNamespaceLister := c.nqosPodLister.Pods(namespace)
	pod, err := podNamespaceLister.Get(name)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	// (i) pod add
	// (ii) pod update because LSP and IPAM is done OR pod's labels changed
	// (iii) pod update because pod went into completed state
	// (iv) pod delete
	// In all these cases check which NQOSes were managing this pod and enqueue them back to nqosQueue
	existingNQOSses, err := c.nqosLister.List(labels.Everything())
	if err != nil {
		return err
	}
	// case(iii)/(iv)
	if pod == nil || util.PodCompleted(pod) {
		for _, nqos := range existingNQOSses {
			cachedName := joinMetaNamespaceAndName(nqos.Namespace, nqos.Name)
			nqosObj, loaded := c.nqosCache[cachedName]
			if !loaded {
				continue
			}
			c.clearPodForNQOS(namespace, name, nqosObj, c.nqosQueue)
		}
		return nil
	}
	// We don't want to shortcuit only local zone pods here since peer pods
	// whether local or remote need to be dealt with. So we let the main
	// NQOS controller take care of the local zone pods logic for the policy subjects
	if !util.PodScheduled(pod) || util.PodWantsHostNetwork(pod) {
		// we don't support NQOS with host-networked pods
		// if pod is no scheduled yet, return and we can process it on its next update
		// because anyways at that stage pod is considered to belong to remote zone
		return nil
	}
	// case (i)/(ii)
	for _, nqos := range existingNQOSses {
		cachedName := joinMetaNamespaceAndName(nqos.Namespace, nqos.Name)
		nqosObj, loaded := c.nqosCache[cachedName]
		if !loaded {
			continue
		}
		c.setPodForNQOS(pod, nqosObj, namespaceLabels, c.nqosQueue)
	}
	return nil
}

// clearPodForNQOS will handle the logic for figuring out if the provided pod name
// TODO...
func (c *Controller) clearPodForNQOS(namespace, name string, nqosCache *networkQoSState, queue workqueue.RateLimitingInterface) {
	// TODO: Implement-me!
}

// setPodForNQOS will handle the logic for figuring out if the provided pod name
// TODO...
func (c *Controller) setPodForNQOS(pod *v1.Pod, nqosCache *networkQoSState, namespaceLabels labels.Labels, queue workqueue.RateLimitingInterface) {
	// TODO: Implement-me!
}
