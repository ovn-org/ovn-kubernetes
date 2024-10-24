package networkqos

import (
	"fmt"
	"strings"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

func (c *Controller) processNextNQOSPodWorkItem(wg *sync.WaitGroup) bool {
	wg.Add(1)
	defer wg.Done()
	nqosPodKey, quit := c.nqosPodQueue.Get()
	if quit {
		return false
	}
	defer c.nqosPodQueue.Done(nqosPodKey)
	err := c.syncNetworkQoSPod(nqosPodKey)
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
	podNamespaceLister := c.nqosPodLister.Pods(namespace)
	pod, err := podNamespaceLister.Get(name)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	// (i) pod add
	// (ii) pod update because LSP and IPAM is done OR pod's labels changed
	// (iii) pod update because pod went into completed state
	// (iv) pod delete
	// case(iii)/(iv)
	if pod == nil || util.PodCompleted(pod) {
		for _, cachedKey := range c.nqosCache.GetKeys() {
			err := c.nqosCache.DoWithLock(cachedKey, func(nqosKey string) error {
				if nqosObj, loaded := c.nqosCache.Load(nqosKey); loaded {
					return c.clearPodForNQOS(namespace, name, nqosObj)
				}
				return nil
			})
			if err != nil {
				return err
			}
		}
		recordPodReconcileDuration(c.controllerName, time.Since(startTime).Milliseconds())
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
	for _, cachedKey := range c.nqosCache.GetKeys() {
		err := c.nqosCache.DoWithLock(cachedKey, func(nqosKey string) error {
			if nqosObj, loaded := c.nqosCache.Load(nqosKey); loaded {
				return c.setPodForNQOS(pod, nqosObj, ns)
			} else {
				klog.Warningf("NetworkQoS not synced yet: %s", nqosKey)
				// requeue nqos key to sync it
				c.nqosQueue.Add(nqosKey)
				// requeue pod key in 3 sec
				c.nqosPodQueue.AddAfter(key, 3*time.Second)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	recordPodReconcileDuration(c.controllerName, time.Since(startTime).Milliseconds())
	return nil
}

// clearPodForNQOS will handle the logic for figuring out if the provided pod name
func (c *Controller) clearPodForNQOS(namespace, name string, nqosState *networkQoSState) error {
	fullPodName := joinMetaNamespaceAndName(namespace, name)
	if err := nqosState.removePodFromSource(c, fullPodName, nil); err != nil {
		return err
	}
	// remove pod from destination address set
	for _, rule := range nqosState.EgressRules {
		if rule.Classifier == nil {
			continue
		}
		for _, dest := range rule.Classifier.Destinations {
			if dest.PodSelector == nil && dest.NamespaceSelector == nil {
				continue
			}
			if err := dest.removePod(fullPodName, nil); err != nil {
				return err
			}
		}
	}
	return nil
}

// setPodForNQOS wil lcheck if the pod meets source selector or dest selector
// - match source: add the ip to source address set, bind qos rule to the switch
// - match dest: add the ip to the destination address set
func (c *Controller) setPodForNQOS(pod *v1.Pod, nqosState *networkQoSState, namespace *v1.Namespace) error {
	addresses, err := getPodAddresses(pod, c.NetInfo)
	if err == nil && len(addresses) == 0 {
		// pod hasn't been annotated with addresses yet, return without retry
		klog.V(6).Infof("Pod %s/%s doesn't have addresses on network %s, skip NetworkQoS processing", pod.Namespace, pod.Name, nqosState.networkAttachmentName)
		return nil
	} else if err != nil {
		return fmt.Errorf("failed to parse addresses for pod %s/%s, network %s, err: %v", pod.Namespace, pod.Name, nqosState.networkAttachmentName, err)
	}
	fullPodName := joinMetaNamespaceAndName(pod.Namespace, pod.Name)
	// is pod in this zone
	if c.isPodScheduledinLocalZone(pod) {
		if matchSource := nqosState.matchSourceSelector(pod); matchSource {
			// pod's labels match source selector
			err = nqosState.configureSourcePod(c, pod, addresses)
		} else {
			// pod's labels don't match selector, but it probably matched previously
			err = nqosState.removePodFromSource(c, fullPodName, addresses)
		}
		if err != nil {
			return err
		}
	} else {
		klog.V(4).Infof("Pod %s is not scheduled in local zone, call remove to ensure it's not in source", fullPodName)
		err = nqosState.removePodFromSource(c, fullPodName, addresses)
		if err != nil {
			return err
		}
	}
	return reconcilePodForDestinations(nqosState, namespace, pod, addresses)
}

func reconcilePodForDestinations(nqosState *networkQoSState, podNs *v1.Namespace, pod *v1.Pod, addresses []string) error {
	fullPodName := joinMetaNamespaceAndName(pod.Namespace, pod.Name)
	for _, rule := range nqosState.EgressRules {
		for index, dest := range rule.Classifier.Destinations {
			if dest.PodSelector == nil && dest.NamespaceSelector == nil {
				continue
			}
			if dest.matchPod(podNs, pod, nqosState.namespace) {
				// add pod address to address set
				if err := dest.addPod(pod.Namespace, pod.Name, addresses); err != nil {
					return fmt.Errorf("failed to add addresses {%s} to dest address set %s for NetworkQoS %s/%s, rule index %d: %v", strings.Join(addresses, ","), dest.DestAddrSet.GetName(), nqosState.namespace, nqosState.name, index, err)
				}
			} else {
				// no match, remove the pod if it's previously selected
				if err := dest.removePod(fullPodName, addresses); err != nil {
					return fmt.Errorf("failed to delete addresses {%s} from dest address set %s for NetworkQoS %s/%s, rule index %d: %v", strings.Join(addresses, ","), dest.DestAddrSet.GetName(), nqosState.namespace, nqosState.name, index, err)
				}
			}
		}
	}
	return nil
}
