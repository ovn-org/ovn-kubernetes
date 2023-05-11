package egressservice

import (
	"fmt"
	"net"
	"sync"
	"time"

	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
)

func (c *Controller) onNodeAdd(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", obj, err))
		return
	}
	c.nodesQueue.Add(key)
}

func (c *Controller) onNodeUpdate(oldObj, newObj interface{}) {
	oldNode := oldObj.(*corev1.Node)
	newNode := newObj.(*corev1.Node)

	// Don't process resync or objects that are marked for deletion
	if oldNode.ResourceVersion == newNode.ResourceVersion ||
		!newNode.GetDeletionTimestamp().IsZero() {
		return
	}
	oldNodeLabels := labels.Set(oldNode.Labels)
	newNodeLabels := labels.Set(newNode.Labels)
	oldNodeReady := nodeIsReady(oldNode)
	newNodeReady := nodeIsReady(newNode)

	// We only care about node updates that relate to readiness, labels or
	// addresses
	if labels.Equals(oldNodeLabels, newNodeLabels) &&
		oldNodeReady == newNodeReady &&
		!util.NodeHostAddressesAnnotationChanged(oldNode, newNode) {
		return
	}

	key, err := cache.MetaNamespaceKeyFunc(newObj)
	if err == nil {
		c.nodesQueue.Add(key)
	}
}

func (c *Controller) onNodeDelete(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", obj, err))
		return
	}
	c.nodesQueue.Add(key)
}

func (c *Controller) runNodeWorker(wg *sync.WaitGroup) {
	for c.processNextNodeWorkItem(wg) {
	}
}

func (c *Controller) processNextNodeWorkItem(wg *sync.WaitGroup) bool {
	wg.Add(1)
	defer wg.Done()

	key, quit := c.nodesQueue.Get()
	if quit {
		return false
	}

	defer c.nodesQueue.Done(key)

	err := c.syncNode(key.(string))
	if err == nil {
		c.nodesQueue.Forget(key)
		return true
	}

	utilruntime.HandleError(fmt.Errorf("%v failed with : %v", key, err))

	if c.nodesQueue.NumRequeues(key) < maxRetries {
		c.nodesQueue.AddRateLimited(key)
		return true
	}

	c.nodesQueue.Forget(key)
	return true
}

func (c *Controller) syncNode(key string) error {
	c.Lock()
	defer c.Unlock()

	startTime := time.Now()
	_, nodeName, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}
	klog.V(4).Infof("Processing sync for Egress Service node %s", nodeName)

	defer func() {
		klog.V(4).Infof("Finished syncing Egress Service node %s: %v", nodeName, time.Since(startTime))
	}()

	if err := c.deleteLegacyDefaultNoRerouteNodePolicies(c.nbClient, nodeName); err != nil {
		return err
	}

	// We ensure node no re-route policies contemplating possible node IP
	// address changes regardless of allocated services.
	err = c.ensureNoRerouteNodePolicies(c.nbClient, c.addressSetFactory, c.controllerName, c.nodeLister)
	if err != nil {
		return err
	}

	n, err := c.nodeLister.Get(nodeName)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}

	state := c.nodes[nodeName]

	if n == nil || !nodeIsReady(n) {
		if state != nil {
			// The node hosting an egress service was deleted or is no longer ready.
			// We mark it as draining and remove all the service configurations made for it,
			// Services can't be configured for a node while it is in draining status.
			state.draining = true
			for svcKey, svcState := range c.services {
				if svcState.node == state.name {
					if err := c.clearServiceResourcesAndRequeue(svcKey, svcState); err != nil {
						return err
					}
				}
			}
			delete(c.nodes, nodeName)
		}
		return nil
	}

	// At this point the node exists and is ready

	// If the node is used by any service but is not in cache enqueue it
	if state == nil {
		for svcKey, svcState := range c.services {
			if svcState.node == n.Name {
				c.egressServiceQueue.Add(svcKey)
			}
		}
		return nil
	}

	if state.draining {
		// The node hosting an egress service is draining and is not usable.
		// We remove all the service configurations made for it,
		// Services can't be configured for a node while it is in draining status.
		for svcKey, svcState := range c.services {
			if svcState.node == state.name {
				if err := c.clearServiceResourcesAndRequeue(svcKey, svcState); err != nil {
					return err
				}
			}
		}
		delete(c.nodes, nodeName)
	}

	return nil
}

// Returns if the given node is in "Ready" state.
func nodeIsReady(n *corev1.Node) bool {
	for _, condition := range n.Status.Conditions {
		if condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// Returns a new nodeState for a node given its name.
func (c *Controller) nodeStateFor(name string) (*nodeState, error) {
	node, err := c.nodeLister.Get(name)
	if err != nil {
		return nil, err
	}

	nodeSubnets, err := util.ParseNodeHostSubnetAnnotation(node, ovntypes.DefaultNetworkName)
	if err != nil {
		return nil, fmt.Errorf("failed to parse node %s subnets annotation %v", node.Name, err)
	}

	mgmtIPs := make([]net.IP, len(nodeSubnets))
	for i, subnet := range nodeSubnets {
		mgmtIPs[i] = util.GetNodeManagementIfAddr(subnet).IP
	}

	var v4IP, v6IP net.IP
	for _, ip := range mgmtIPs {
		if utilnet.IsIPv4(ip) {
			v4IP = ip
			continue
		}
		v6IP = ip
	}

	return &nodeState{name: name, v4MgmtIP: v4IP, v6MgmtIP: v6IP}, nil
}
