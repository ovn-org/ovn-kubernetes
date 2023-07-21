package client

import (
	"crypto/tls"
	"net/url"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	defaultTCPEndpoint  = "tcp:127.0.0.1:6640"
	defaultSSLEndpoint  = "ssl:127.0.0.1:6640"
	defaultUnixEndpoint = "unix:/var/run/openvswitch/ovsdb.sock"
)

type options struct {
	endpoints             []string
	tlsConfig             *tls.Config
	reconnect             bool
	leaderOnly            bool
	timeout               time.Duration
	backoff               backoff.BackOff
	logger                *logr.Logger
	registry              prometheus.Registerer
	shouldRegisterMetrics bool   // in case metrics are changed after-the-fact
	metricNamespace       string // prometheus metric namespace
	metricSubsystem       string // prometheus metric subsystem
	inactivityTimeout     time.Duration
}

type Option func(o *options) error

func newOptions(opts ...Option) (*options, error) {
	o := &options{}
	for _, opt := range opts {
		if err := opt(o); err != nil {
			return nil, err
		}
	}
	// if no endpoints are supplied, use the default unix socket
	if len(o.endpoints) == 0 {
		o.endpoints = []string{defaultUnixEndpoint}
	}
	return o, nil
}

// WithTLSConfig sets the tls.Config for use by the client
func WithTLSConfig(cfg *tls.Config) Option {
	return func(o *options) error {
		o.tlsConfig = cfg
		return nil
	}
}

// WithEndpoint sets the endpoint to be used by the client
// It can be used multiple times, and the first endpoint that
// successfully connects will be used.
// Endpoints are specified in OVSDB Connection Format
// For more details, see the ovsdb(7) man page
func WithEndpoint(endpoint string) Option {
	return func(o *options) error {
		ep, err := url.Parse(endpoint)
		if err != nil {
			return err
		}
		switch ep.Scheme {
		case UNIX:
			if len(ep.Path) == 0 {
				o.endpoints = append(o.endpoints, defaultUnixEndpoint)
				return nil
			}
		case TCP:
			if len(ep.Opaque) == 0 {
				o.endpoints = append(o.endpoints, defaultTCPEndpoint)
				return nil
			}
		case SSL:
			if len(ep.Opaque) == 0 {
				o.endpoints = append(o.endpoints, defaultSSLEndpoint)
				return nil
			}
		}
		o.endpoints = append(o.endpoints, endpoint)
		return nil
	}
}

// WithLeaderOnly tells the client to treat endpoints that are clustered
// and not the leader as down.
func WithLeaderOnly(leaderOnly bool) Option {
	return func(o *options) error {
		o.leaderOnly = leaderOnly
		return nil
	}
}

// WithReconnect tells the client to automatically reconnect when
// disconnected. The timeout is used to construct the context on
// each call to Connect, while backoff dictates the backoff
// algorithm to use. Using WithReconnect implies that
// requested transactions will block until the client has fully reconnected,
// rather than immediately returning an error if there is no connection.
func WithReconnect(timeout time.Duration, backoff backoff.BackOff) Option {
	return func(o *options) error {
		o.reconnect = true
		o.timeout = timeout
		o.backoff = backoff
		return nil
	}
}

// WithInactivityCheck tells the client to send Echo request to ovsdb server periodically
// upon inactivityTimeout. When Echo request fails, then it attempts to reconnect
// with server. The inactivity check is performed as long as the connection is established.
// The reconnectTimeout argument is used to construct the context on each call to Connect,
// while reconnectBackoff dictates the backoff algorithm to use.
func WithInactivityCheck(inactivityTimeout, reconnectTimeout time.Duration,
	reconnectBackoff backoff.BackOff) Option {
	return func(o *options) error {
		o.reconnect = true
		o.timeout = reconnectTimeout
		o.backoff = reconnectBackoff
		o.inactivityTimeout = inactivityTimeout
		return nil
	}
}

// WithLogger allows setting a specific log sink. Otherwise, the default
// go log package is used.
func WithLogger(l *logr.Logger) Option {
	return func(o *options) error {
		o.logger = l
		return nil
	}
}

// WithMetricsRegistry allows the user to specify a Prometheus metrics registry.
// If supplied, the metrics as defined in metrics.go will be registered.
func WithMetricsRegistry(r prometheus.Registerer) Option {
	return func(o *options) error {
		o.registry = r
		o.shouldRegisterMetrics = (r != nil)
		return nil
	}
}

// WithMetricsRegistryNamespaceSubsystem allows the user to specify a Prometheus metrics registry
// and Prometheus metric namespace and subsystem of the component utilizing libovsdb.
// If supplied, the metrics as defined in metrics.go will be registered.
func WithMetricsRegistryNamespaceSubsystem(r prometheus.Registerer, namespace, subsystem string) Option {
	if namespace == "" || subsystem == "" {
		panic("libovsdb function WithMetricsRegistryNamespaceSubsystem arguments 'namespace' and 'subsystem' must not be empty")
	}
	return func(o *options) error {
		o.registry = r
		o.shouldRegisterMetrics = (r != nil)
		o.metricNamespace = namespace
		o.metricSubsystem = subsystem
		return nil
	}
}
