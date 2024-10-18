package networkqos

import (
	"fmt"
	"sync"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/tools/cache"
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
	err := c.syncNetworkQoSNode(nqosNodeKey)
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
	startTime := time.Now()
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

	if !c.isNodeInLocalZone(node) && c.TopologyType() == types.Layer3Topology {
		// clean up qos/address set for the node
		return c.cleanupQoSFromNode(node.Name)
	}
	// configure qos for pods on the node
	pods, err := c.getPodsByNode(node.Name)
	if err != nil {
		return err
	}
	switchName := c.getLogicalSwitchName(node.Name)
	_, err = c.findLogicalSwitch(switchName)
	if err != nil {
		klog.V(4).Infof("Failed to look up logical switch %s: %v", switchName, err)
		return err
	}
	for _, cachedKey := range c.nqosCache.GetKeys() {
		err := c.nqosCache.DoWithLock(cachedKey, func(nqosKey string) error {
			nqosObj, loaded := c.nqosCache.Load(nqosKey)
			if !loaded {
				klog.Warningf("NetworkQoS not synced yet: %s", nqosKey)
				// requeue nqos key to sync it
				c.nqosQueue.Add(nqosKey)
				// requeue namespace key 3 seconds later, allow NetworkQoS to be handled
				c.nqosNamespaceQueue.AddAfter(key, 3*time.Second)
				return nil
			}
			for _, pod := range pods {
				ns, err := c.nqosNamespaceLister.Get(pod.Namespace)
				if err != nil {
					return fmt.Errorf("failed to look up namespace %s: %w", pod.Namespace, err)
				}
				if err = c.setPodForNQOS(pod, nqosObj, ns); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *Controller) getPodsByNode(nodeName string) ([]*v1.Pod, error) {
	pods, err := c.nqosPodLister.List(labels.Everything())
	if err != nil {
		return nil, fmt.Errorf("failed to list pods: %w", err)
	}
	podsByNode := []*v1.Pod{}
	for _, pod := range pods {
		if util.PodScheduled(pod) && !util.PodWantsHostNetwork(pod) && pod.Spec.NodeName == nodeName {
			podsByNode = append(podsByNode, pod)
		}
	}
	return podsByNode, nil
}

// isNodeInLocalZone returns whether the provided node is in a zone local to the zone controller
func (c *Controller) isNodeInLocalZone(node *v1.Node) bool {
	return util.GetNodeZone(node) == c.zone
}

func (c *Controller) cleanupQoSFromNode(nodeName string) error {
	switchName := c.getLogicalSwitchName(nodeName)
	for _, cachedKey := range c.nqosCache.GetKeys() {
		err := c.nqosCache.DoWithLock(cachedKey, func(nqosKey string) error {
			nqosObj, _ := c.nqosCache.Load(nqosKey)
			if nqosObj == nil {
				klog.V(4).Infof("Expected networkqos %s not found in cache", nqosKey)
				return nil
			}
			pods := []string{}
			if val, _ := nqosObj.SwitchRefs.Load(switchName); val != nil {
				pods = val.([]string)
			}
			for _, pod := range pods {
				addrs, _ := nqosObj.Pods.Load(pod)
				if addrs != nil {
					err := nqosObj.SrcAddrSet.DeleteAddresses(addrs.([]string))
					if err != nil {
						return err
					}
				}
				nqosObj.Pods.Delete(pod)
			}
			err := c.removeQoSFromLogicalSwitches(nqosObj, []string{switchName})
			if err != nil {
				return err
			}
			nqosObj.SwitchRefs.Delete(switchName)
			return nil
		})
		if err != nil {
			return err
		}
		klog.V(4).Infof("Successfully cleaned up qos rules %s from %s", cachedKey, switchName)
	}
	return nil
}
