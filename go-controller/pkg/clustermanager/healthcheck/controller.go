package healthcheck

import (
	"context"
	"fmt"
	"math/rand"
	"net"
	"sync"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	controllerutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/controller"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/healthcheck"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/timeoutqueue"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
)

const (
	// period in seconds of each node probe
	period = 1
	// jitter in milliseconds added to the period to distribute probes over
	jitter = 200
)

// nodeState contians the latest declared health state for a node and the latest
// time it was available
type nodeState struct {
	health    HealthState
	available time.Time
}

// transitionNodeState controls the transition to a new node state from the
// latest node state and an actual health state, return a string reasoning about
// the state change
func transitionNodeState(old nodeState, health HealthState, on time.Time, timeout time.Duration, reset bool) (new nodeState, reason string) {
	new = old
	reason = "health state changed"

	switch health {
	case AVAILABLE:
		new.health = AVAILABLE
		new.available = on
	case UNREACHABLE:
		// only set UNREACHABLE after timeout
		if time.Since(old.available) > timeout {
			new.health = UNREACHABLE
			reason = fmt.Sprintf("not available since %s", old.available)
		}
	default:
		new.health = health
	}

	if reset {
		reason = "health state reset"
		new.health = health
	}

	return
}

// nodeInfo contains the IPs of the node and a function to cancel the current
// probing task for the node
type nodeInfo struct {
	ips        []net.IP
	cancelTask context.CancelFunc
}

// probeTask represents a probing task for a node including the the context it
// is executed under and the client used for the probe
type probeTask struct {
	node   string
	client healthcheck.EgressIPHealthClient
	ctx    context.Context
}

func (nt probeTask) cancelled() bool {
	select {
	case <-nt.ctx.Done():
		return true
	default:
	}
	return false
}

type controller struct {
	sync.RWMutex

	ctx context.Context

	controller   controllerutil.Reconciler
	nodeInformer coreinformers.NodeInformer

	// consumers interested in node health states
	consumers []Consumer
	// given the complex interactions that can result with consumers, use
	// a specific lock during callbacks
	consumerLock sync.RWMutex

	// per node information
	nodeInfo  map[string]nodeInfo
	nodeState map[string]nodeState

	// queue of probing tasks to be executed on schedule, one task per probed
	// node at most
	probeTaskQueue *timeoutqueue.TimeoutQueue[*probeTask]

	// timeout dictates the number of failed probe attempts (one per period)
	// before declaring a node UNREACHABLE
	timeout time.Duration

	// port used for probes (0 = legacy probe, otherwise GRPC probe)
	port int
}

func (c *controller) GetHealthState(node string) HealthState {
	c.RLock()
	defer c.RUnlock()
	return c.nodeState[node].health
}

func (c *controller) MonitorHealthState(node string) {
	if c.controller == nil {
		panic("health check controller not started")
	}
	// consumer has interest on this node so reconcile to start probing
	c.controller.Reconcile(node)
}

func (c *controller) Register(consumer Consumer) {
	c.consumerLock.Lock()
	defer c.consumerLock.Unlock()
	c.consumers = append(c.consumers, consumer)
}

func newController(port, timeout int) *controller {
	return &controller{
		nodeInfo:       make(map[string]nodeInfo),
		nodeState:      make(map[string]nodeState),
		consumers:      make([]Consumer, 0),
		port:           port,
		timeout:        time.Duration(timeout) * time.Second,
		probeTaskQueue: timeoutqueue.New[*probeTask](),
	}
}

func (c *controller) start(ctx context.Context, nodeInformer coreinformers.NodeInformer) error {
	// we rely on a node controller: nodes need to exist, have the proper
	// configuration details and have interest from consumers for them to be
	// probed. If so, will add the proper probing task to the queue
	config := &controllerutil.Config[corev1.Node]{
		RateLimiter:    workqueue.DefaultControllerRateLimiter(),
		Informer:       nodeInformer.Informer(),
		Lister:         nodeInformer.Lister().List,
		ObjNeedsUpdate: func(oldObj, newObj *corev1.Node) bool { return true },
		Reconcile:      func(key string) error { return c.reconcile(key) },
		Threadiness:    1,
	}
	controller := controllerutil.NewController[corev1.Node]("health check controller", config)

	c.controller = controller
	c.nodeInformer = nodeInformer
	c.ctx = ctx

	err := controllerutil.StartControllers(controller)
	if err != nil {
		return fmt.Errorf("failed to start health check controller: %w", err)
	}

	// probing loop, executes probing tasks from the queue, runs to
	// completion
	go func() {
		c.run()

		// stop & cleanup
		controllerutil.StopControllers(controller)
		c.cleanup()
		klog.Infof("Stopped health check controller")
	}()

	klog.Infof("Started health check controller")
	return nil
}

func (c *controller) run() {
	for {
		// pop the next probe task to execute
		task := c.probeTaskQueue.Pop(c.ctx)
		select {
		case <-c.ctx.Done():
			return
		default:
			// execute on its own goroutine
			go c.probe(task)
		}
	}
}

func (c *controller) probe(task *probeTask) {
	// check if task was cancelled before probing
	nodeInfo, exists := c.isProbedWithNodeInfo(task.node)
	if !exists || task.cancelled() {
		task.client.Disconnect()
		return
	}

	// probe
	klog.V(5).Infof("Probing health state of node %s", task.node)
	state := AVAILABLE
	reachable := IsReachable(task.ctx, task.node, nodeInfo.ips, task.client, c.port, period)
	if !reachable {
		klog.Errorf("Failed health state probe for node %s", task.node)
		state = UNREACHABLE
	}

	// update state & retask
	updated := c.setNodeStateAndRetask(task, state)
	if updated {
		c.informConsumers(task.node)
	}

	// if no interest from consumers, reconcile to cancel the probing task
	if !c.hasInterestedConsumers(task.node, nil) {
		c.controller.Reconcile(task.node)
	}
}

func (c *controller) reconcile(nodeName string) error {
	start := time.Now()
	klog.V(5).Infof("Reconciling health check for node %s", nodeName)
	defer func() {
		klog.V(5).Infof("Finished reconciling health check for node %s: %v", nodeName, time.Since(start))
	}()

	node, err := c.nodeInformer.Lister().Get(nodeName)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}

	if node == nil {
		// node deleted, stop probing
		updated := c.cancelProbeWithState(nodeName, UNKNOWN)
		if updated {
			klog.Infof("Stopping health state probing for node %s: node deleted", nodeName)
			c.informConsumers(nodeName)
		}
		return nil
	}

	names := []string{}
	if !c.hasInterestedConsumers(nodeName, &names) {
		// no interest, stop probing
		updated := c.cancelProbeWithState(nodeName, UNKNOWN)
		if updated {
			klog.Infof("Stopping health state probing for node %s: no interested consumers out of %q", nodeName, names)
			// in case someone just registered
			c.informConsumers(nodeName)
		}
		return nil
	}

	nodeSubnets, err := util.ParseNodeHostSubnetAnnotation(node, types.DefaultNetworkName)
	if err != nil && !util.IsAnnotationNotSetError(err) {
		return fmt.Errorf("failed to parse node %s subnets annotation %v", node.Name, err)
	}

	mgmtIPs := make([]net.IP, len(nodeSubnets))
	for i, subnet := range nodeSubnets {
		mgmtIPs[i] = util.GetNodeManagementIfAddr(subnet).IP
	}

	update := true
	nodeInfo, probed := c.isProbedWithNodeInfo(nodeName)
	if probed {
		less := func(a, b net.IP) bool { return a.String() < b.String() }
		update = !cmp.Equal(nodeInfo.ips, mgmtIPs, cmpopts.SortSlices(less))
	}

	if !update {
		// already probing and no config update, bail out
		return nil
	}

	if len(mgmtIPs) == 0 {
		// no proper config, stop probing and assume unreachable out of
		// precaution
		updated := c.cancelProbeWithState(nodeName, UNREACHABLE)
		if updated {
			klog.Infof("Stopping health state probing for node %s: node management IPs unknown", nodeName)
			c.informConsumers(nodeName)
		}
		return nil
	}

	nodeInfo.ips = mgmtIPs

	// add/update probing task
	klog.Infof("Starting or updating health state probing for node %s on IPs %s", nodeName, util.StringSlice(mgmtIPs))
	c.startProbeWithNodeInfo(nodeName, nodeInfo)

	return nil
}

func (c *controller) informConsumers(node string) {
	c.consumerLock.RLock()
	defer c.consumerLock.RUnlock()
	// note we don't query the interest of the consumers as that might be racey,
	// it is up to the consumers to appropriately filter out uninteresting
	// events
	for _, consumer := range c.consumers {
		consumer.HealthStateChanged(node)
	}
}

func (c *controller) hasInterestedConsumers(node string, names *[]string) bool {
	c.consumerLock.RLock()
	defer c.consumerLock.RUnlock()
	for _, consumer := range c.consumers {
		if names != nil {
			*names = append(*names, consumer.Name())
		}
		if consumer.MonitorHealthState(node) {
			return true
		}
	}
	return false
}

func (c *controller) isProbedWithNodeInfo(node string) (nodeInfo, bool) {
	c.RLock()
	defer c.RUnlock()
	nodeInfo, exists := c.nodeInfo[node]
	return nodeInfo, exists
}

func (c *controller) startProbeWithNodeInfo(node string, nodeInfo nodeInfo) {
	c.Lock()
	defer c.Unlock()

	// cancel any previous task and schedule a new one in case config
	// changed
	if nodeInfo.cancelTask != nil {
		nodeInfo.cancelTask()
	}

	ctx, cancel := context.WithCancel(c.ctx)
	nodeInfo.cancelTask = cancel
	c.nodeInfo[node] = nodeInfo

	task := &probeTask{
		node:   node,
		client: healthcheck.NewEgressIPHealthClient(node),
		ctx:    ctx,
	}

	// schedule task with some jitter
	j := time.Duration(rand.Intn(jitter)) * time.Millisecond
	c.probeTaskQueue.Push(task, j)
}

func (c *controller) setNodeStateAndRetask(task *probeTask, new HealthState) bool {
	on := time.Now()
	c.Lock()
	defer c.Unlock()

	// check if task was cancelled after probing
	if task.cancelled() {
		task.client.Disconnect()
		return false
	}

	updated := c.setNodeState(task.node, new, on, false)

	// reschedule task after period and some jitter
	p := period * time.Second
	j := time.Duration(rand.Intn(jitter)) * time.Millisecond
	c.probeTaskQueue.Push(task, p+j)

	return updated
}

func (c *controller) cancelProbeWithState(node string, state HealthState) bool {
	on := time.Now()
	c.Lock()
	defer c.Unlock()

	nodeInfo, exists := c.nodeInfo[node]
	if exists {
		delete(c.nodeInfo, node)
		nodeInfo.cancelTask()
	}

	return c.setNodeState(node, state, on, true)
}

func (c *controller) setNodeState(node string, health HealthState, on time.Time, reset bool) bool {
	old := c.nodeState[node]
	new, reason := transitionNodeState(old, health, on, c.timeout, reset)

	if new.health == UNKNOWN {
		delete(c.nodeState, node)
	} else {
		c.nodeState[node] = new
	}

	if old.health != new.health {
		klog.Infof("Health state change for node %s: from %s to %s due to [%s]", node, old.health, new.health, reason)
		return true
	}

	return false
}

func (c *controller) cleanup() {
	for _, nodeInfo := range c.nodeInfo {
		nodeInfo.cancelTask()
	}
}
