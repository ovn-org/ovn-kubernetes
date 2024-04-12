package healthcheck

import (
	"context"
	"sync"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"

	coreinformers "k8s.io/client-go/informers/core/v1"
)

type HealthState int

const (
	// UNKNOWN is the health state of any node that is not being monitored.
	// Services should not make any determination based on this state and should
	// consider the node potentially available for scheduling.
	UNKNOWN HealthState = iota

	// AVAILABLE is the health state of a node that is reachable and thought to
	// have a healthy ovn-kube running. Services should consider the node
	// available for scheduling.
	AVAILABLE

	// UNAVAILABLE is the health state of a node that is reachable but might not
	// have a healthy ovn-kube running. Services should not consider the node
	// available for scheduling but might not unschedule from it either as
	// already scheduled services might still be working fine. Only an AVAILABLE
	// node can become UNAVAILABLE.
	UNAVAILABLE

	// UNREACHABLE is the health state of a node that is not reachable. Services
	// should not consider the node available for scheduling and should attempt
	// to unschedule from it as well.
	UNREACHABLE
)

func (s HealthState) String() string {
	switch s {
	case AVAILABLE:
		return "AVAILABLE"
	case UNAVAILABLE:
		return "UNAVAILABLE"
	case UNREACHABLE:
		return "UNREACHABLE"
	}
	return "UNKNOWN"
}

// Provider of node health state information
type Provider interface {
	// Register a consumer to be informed about node health state changes
	Register(consumer Consumer)
	// GetHealthState of the node
	GetHealthState(node string) HealthState
	// MonitorHealthState notifies interest of a consumer to monitor the health
	// state of a node.
	MonitorHealthState(node string)
}

// Consumer of node health state information
type Consumer interface {
	// Name the consumer goes by for logging purposes
	Name() string
	// HealthStateChanged informs consumers of node health state changes. A
	// consumer might be informed about a node it is not interested in and
	// should be able to ignore it in that case.
	HealthStateChanged(node string)
	// HealthStateChanged queries the interest of the consumer of being informed
	// about health state changes of the node. A node is monitored if any
	// consumer is interested. Monitoring for a node might terminate as soon as
	// no consumer is interested.
	MonitorHealthState(node string) bool
}

// ConsumerAdaptor is an adaptor of consumer functions to the consumer interface
func ConsumerAdaptor(name string, healthStateChanged func(s string), monitorHealthState func(s string) bool) Consumer {
	return &consumerAdaptor{
		name:               name,
		healthStateChanged: healthStateChanged,
		monitorHealthState: monitorHealthState,
	}
}

// GetProvider of health state information
func GetProvider() Provider {
	return getProvider()
}

// Start health check of nodes that consumers are interested in
func Start(ctx context.Context, nodeInformer coreinformers.NodeInformer) error {
	return start(ctx, nodeInformer)
}

type provider interface {
	Provider
	start(ctx context.Context, nodeInformer coreinformers.NodeInformer) error
}

var l sync.Mutex
var singleton provider
var started bool

func getProvider() provider {
	l.Lock()
	defer l.Unlock()
	if singleton != nil {
		return singleton
	}
	singleton = &inhibited{}
	port := config.OVNKubernetesFeature.EgressIPNodeHealthCheckPort
	timeout := config.OVNKubernetesFeature.EgressIPReachabiltyTotalTimeout
	if timeout != 0 {
		// enabled
		singleton = newController(port, timeout)
	}
	return singleton
}

func start(ctx context.Context, nodeInformer coreinformers.NodeInformer) error {
	provider := getProvider()
	l.Lock()
	defer l.Unlock()
	if started {
		return nil
	}
	started = true
	return provider.start(ctx, nodeInformer)
}

// inhibited is a provider that always reports nodes as AVAILABLE used when
// health check is disabled
type inhibited struct{}

func (i inhibited) start(ctx context.Context, nodeInformer coreinformers.NodeInformer) error {
	return nil
}

func (i inhibited) Register(consumer Consumer) {}

func (i inhibited) GetHealthState(node string) HealthState {
	return AVAILABLE
}

func (i inhibited) MonitorHealthState(node string) {}

// consumerAdaptor is an adaptor of consumer functions to the consumer interface
type consumerAdaptor struct {
	name               string
	healthStateChanged func(s string)
	monitorHealthState func(s string) bool
}

func (c *consumerAdaptor) Name() string {
	return c.name
}

func (c *consumerAdaptor) HealthStateChanged(node string) {
	c.healthStateChanged(node)
}

func (c *consumerAdaptor) MonitorHealthState(node string) bool {
	return c.monitorHealthState(node)
}
