package egressservice

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/healthcheck"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	corev1 "k8s.io/api/core/v1"
	utilnet "k8s.io/utils/net"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

// TODO: https://github.com/ovn-org/ovn-kubernetes/pull/3135#discussion_r960042582
// Currently we are creating another goroutine that pretty much does what EgressIP
// does to monitor nodes' reachability.
// Ideally we should move the healthchecking logic from these controllers and make
// a universal cache that both of them can query to obtain the "health" status of the nodes.

// Similarly to the EgressIP controller, we loop over all of the nodes that have
// allocations and check if they are still usable.
func (c *Controller) checkNodesReachability() {
	timer := time.NewTicker(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
			c.CheckNodesReachabilityIterate()
		case <-c.stopCh:
			klog.V(5).Infof("Stop channel got triggered: will stop CheckNodesReachability")
			return
		}
	}
}

func (c *Controller) CheckNodesReachabilityIterate() {
	c.Lock()
	defer c.Unlock()

	nodesToFree := []*nodeState{}
	for _, node := range c.nodes {
		wasReachable := node.reachable
		isReachable := c.IsReachable(node.name, node.mgmtIPs, node.healthClient)
		node.reachable = isReachable
		if wasReachable && !isReachable {
			// The node is not reachable, we need to drain it and reassign its allocations
			c.nodesQueue.Add(node.name)
			continue
		}

		startedDrain := node.draining
		fullyDrained := len(node.allocations) == 0
		if startedDrain && fullyDrained && isReachable {
			// We make the node usable for new allocations only when
			// it has finished draining and is reachable again.
			// As long it is in the cache and in draining state it can't
			// be chosen for new allocations.
			nodesToFree = append(nodesToFree, node)
		}
	}

	for _, node := range nodesToFree {
		delete(c.nodes, node.name)
		node.healthClient.Disconnect()
		c.nodesQueue.Add(node.name) // Since it is available we queue it as it might match unallocated services
	}
}

type egressSVCDialer interface {
	dial(ip net.IP, timeout time.Duration) bool
}

var dialer egressSVCDialer = &egressSVCDial{}

type egressSVCDial struct{}

// Blantant copy from: https://github.com/openshift/sdn/blob/master/pkg/network/common/egressip.go#L499-L505
// Ping a node and return whether or not we think it is online. We do this by trying to
// open a TCP connection to the "discard" service (port 9); if the node is offline, the
// attempt will either time out with no response, or else return "no route to host" (and
// we will return false). If the node is online then we presumably will get a "connection
// refused" error; but the code below assumes that anything other than timeout or "no
// route" indicates that the node is online.
func (e *egressSVCDial) dial(ip net.IP, timeout time.Duration) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(ip.String(), "9"), timeout)
	if conn != nil {
		conn.Close()
	}
	if opErr, ok := err.(*net.OpError); ok {
		if opErr.Timeout() {
			return false
		}
		if sysErr, ok := opErr.Err.(*os.SyscallError); ok && sysErr.Err == syscall.EHOSTUNREACH {
			return false
		}
	}
	return true
}

func IsReachableViaGRPC(mgmtIPs []net.IP, healthClient healthcheck.EgressIPHealthClient, healthCheckPort, totalTimeout int) bool {
	dialCtx, dialCancel := context.WithTimeout(context.Background(), time.Duration(totalTimeout)*time.Second)
	defer dialCancel()

	if !healthClient.IsConnected() {
		// gRPC session is not up. Attempt to connect and if that suceeds, we will declare node as reacheable.
		return healthClient.Connect(dialCtx, mgmtIPs, healthCheckPort)
	}

	// gRPC session is already established. Send a probe, which will succeed, or close the session.
	return healthClient.Probe(dialCtx)
}

func IsReachableLegacy(node string, mgmtIPs []net.IP, totalTimeout int) bool {
	var retryTimeOut, initialRetryTimeOut time.Duration

	numMgmtIPs := len(mgmtIPs)
	if numMgmtIPs == 0 {
		return false
	}

	switch totalTimeout {
	// Check if we need to do node reachability check
	case 0:
		return true
	case 1:
		// Using time duration for initial retry with 700/numIPs msec and retry of 100/numIPs msec
		// to ensure total wait time will be in range with the configured value including a sleep of 100msec between attempts.
		initialRetryTimeOut = time.Duration(700/numMgmtIPs) * time.Millisecond
		retryTimeOut = time.Duration(100/numMgmtIPs) * time.Millisecond
	default:
		// Using time duration for initial retry with 900/numIPs msec
		// to ensure total wait time will be in range with the configured value including a sleep of 100msec between attempts.
		initialRetryTimeOut = time.Duration(900/numMgmtIPs) * time.Millisecond
		retryTimeOut = initialRetryTimeOut
	}

	timeout := initialRetryTimeOut
	endTime := time.Now().Add(time.Second * time.Duration(totalTimeout))
	for time.Now().Before(endTime) {
		for _, ip := range mgmtIPs {
			if dialer.dial(ip, timeout) {
				return true
			}
		}
		time.Sleep(100 * time.Millisecond)
		timeout = retryTimeOut
	}
	klog.Errorf("Failed reachability check for %s", node)
	return false
}

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

	if n == nil {
		if state != nil {
			// The node was deleted but had allocated services.
			// We mark it as draining and remove all of the allocations from it,
			// queuing them to attempt assigning a new node.
			// Services can't be assigned to a node while it is in draining status.
			state.draining = true
			for svcKey, svcState := range state.allocations {
				if err := c.clearServiceResourcesAndRequeue(svcKey, svcState); err != nil {
					return err
				}
			}
			delete(c.nodes, nodeName)
			state.healthClient.Disconnect()
		}

		// nothing to sync here
		return nil
	}

	nodeReady := nodeIsReady(n)
	nodeLabels := n.Labels
	if state == nil {
		if nodeReady {
			// The node has no allocated services and is ready, this means unallocated services whose labels match
			// the node's labels can be allocated to it.
			for svcKey, selector := range c.unallocatedServices {
				if selector.Matches(labels.Set(nodeLabels)) {
					c.egressServiceQueue.Add(svcKey)
				}
			}
		}

		return nil
	}

	if !nodeReady {
		// The node is not ready but had allocated services, we drain it
		// and attempt reallocating its services, deleting it from our cache
		// because we don't care about its reachability status until it becomes ready.
		state.draining = true
		for svcKey, svcState := range state.allocations {
			if err := c.clearServiceResourcesAndRequeue(svcKey, svcState); err != nil {
				return err
			}
		}
		delete(c.nodes, nodeName)
		state.healthClient.Disconnect()
		return nil
	}

	if !state.reachable || state.draining {
		// The node has allocated services but is not suitable to run them anymore, we drain it
		// and attempt reallocating its services similarly to the "n == nil && state != nil" path.
		// When it is fully drained and reachable again it will be requeued.
		state.draining = true
		for svcKey, svcState := range state.allocations {
			if err := c.clearServiceResourcesAndRequeue(svcKey, svcState); err != nil {
				return err
			}
		}
		return nil
	}

	state.labels = nodeLabels
	// The node's labels might have changed, we verify that it is still suitable
	// to run all of its allocations.
	// If a service's selector no longer matches this node we attempt to reallocate it.
	for svcKey, svcState := range state.allocations {
		if !svcState.selector.Matches(labels.Set(n.Labels)) || svcState.stale {
			if err := c.clearServiceResourcesAndRequeue(svcKey, svcState); err != nil {
				return err
			}
		}
	}

	// Label the node again for all of the current valid allocations in case
	// the user manually changed it.
	for svcKey := range state.allocations {
		namespace, name, _ := cache.SplitMetaNamespaceKey(svcKey)
		err := c.labelNodeForService(namespace, name, state.name)
		if err != nil {
			return err
		}
	}

	// The node might match the selectors of an unallocated service.
	// If it does, we queue that service to attempt allocating it to this node.
	for svcKey, selector := range c.unallocatedServices {
		if selector.Matches(labels.Set(nodeLabels)) {
			c.egressServiceQueue.Add(svcKey)
		}
	}

	return nil
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

	return &nodeState{name: name, mgmtIPs: mgmtIPs, v4MgmtIP: v4IP, v6MgmtIP: v6IP,
		healthClient: healthcheck.NewEgressIPHealthClient(name), allocations: map[string]*svcState{}, labels: node.Labels,
		reachable: true, draining: false}, nil
}

// Returns the names of all of the nodes in the nodes cache that match the given selector
// and a slice of their states which can be sorted later.
func (c *Controller) cachedNodesFor(selector labels.Selector) (sets.Set[string], []*nodeState) {
	names := sets.New[string]()
	states := []*nodeState{}
	for _, n := range c.nodes {
		if selector.Matches(labels.Set(n.labels)) {
			names.Insert(n.name)
			states = append(states, n)
		}
	}

	return names, states
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

// Labels the given node with the 'egress-service.k8s.ovn.org/<svc-namespace>-<svc-name>:""' label
// which marks it as the node who is holding that service.
func (c *Controller) labelNodeForService(namespace, name, node string) error {
	labels := map[string]any{c.nodeLabelForService(namespace, name): ""}
	return c.patchNodeLabels(node, labels)
}

// Removes the 'egress-service.k8s.ovn.org/<svc-namespace>-<svc-name>:""' label from the given node.
func (c *Controller) removeNodeServiceLabel(namespace, name, node string) error {
	labels := map[string]any{c.nodeLabelForService(namespace, name): nil} // Patching with a nil value results in the delete of the key
	return c.patchNodeLabels(node, labels)
}

// Returns the 'egress-service.k8s.ovn.org/<svc-namespace>-<svc-name>' key for the given namespace and name of a service.
func (c *Controller) nodeLabelForService(namespace, name string) string {
	return fmt.Sprintf("%s/%s-%s", egressSVCLabelPrefix, namespace, name)
}

// Patches the node's metadata.labels with the given labels.
func (c *Controller) patchNodeLabels(node string, labels map[string]any) error {
	patch := struct {
		Metadata map[string]any `json:"metadata"`
	}{
		Metadata: map[string]any{
			"labels": labels,
		},
	}

	klog.V(4).Infof("Setting labels %v on node %s", labels, node)
	patchData, err := json.Marshal(&patch)
	if err != nil {
		klog.Errorf("Error in setting labels on node %s: %v", node, err)
		return err
	}

	_, err = c.client.CoreV1().Nodes().Patch(context.TODO(), node, types.MergePatchType, patchData, metav1.PatchOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// Returns the most suitable nodeState of the node for the given selector -
// The most suitable node being one that matches the selector with the
// least amount of allocations and is not in a "draining" state.
func (c *Controller) selectNodeFor(selector labels.Selector) (*nodeState, error) {
	nodes, err := c.nodeLister.List(selector)
	if err != nil {
		return nil, err
	}

	allReadyNodes := sets.New[string]()
	for _, n := range nodes {
		if nodeIsReady(n) {
			allReadyNodes.Insert(n.Name)
		}
	}

	cachedNames, cachedStates := c.cachedNodesFor(selector)

	freeNodes := allReadyNodes.Difference(cachedNames)
	if freeNodes.Len() > 0 {
		// We have a matching node with 0 allocations, we can just use it
		// instead of using one from the cache.
		node, _ := freeNodes.PopAny()
		return c.nodeStateFor(node)
	}

	// We need to use one of the cached nodes, we will pick the one with the least amount of allocations
	// by first sorting the slice by allocations amount, then iterating over it and picking the first
	// node that is not in "draining" state.
	sort.Slice(cachedStates, func(i, j int) bool {
		return len(cachedStates[i].allocations) < len(cachedStates[j].allocations)
	})

	for _, node := range cachedStates {
		if !node.draining {
			return node, nil
		}
	}

	return nil, fmt.Errorf("no suitable node for selector: %s", selector.String())
}
