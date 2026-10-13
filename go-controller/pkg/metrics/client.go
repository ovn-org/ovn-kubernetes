// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"context"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"k8s.io/client-go/tools/metrics"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
)

// client-go exposes its request instrumentation through package-level hooks
// that default to no-ops, and nothing in ovnkube ever installed an adapter, so
// the time a controller spends waiting on the apiserver was not observable at
// all. That is the dominant term for controllers that write many objects per
// reconcile: the RouteAdvertisements controller spends about a second per
// reconcile generating one FRRConfiguration per node while using a fraction of
// a percent of a core, and without these metrics the only way to see that was
// to subtract CPU from workqueue work duration.
//
// The names carry no subsystem segment on purpose. client-go allows exactly
// one registration per process, so in the combined binary these cover the
// node, the controller and the cluster manager together and labelling them as
// any one of those would be wrong.
// Shared by every vector observed through restClientLatencyAdapter. Declared
// once so the adapter and its vectors cannot drift apart.
var restClientLatencyLabels = []string{"verb", "resource", "host"}

var (
	metricRestClientRequestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: types.MetricOvnkubeNamespace,
		Name:      "rest_client_request_duration_seconds",
		Help:      "Latency of requests to the apiserver, by verb, resource and host.",
		Buckets:   prometheus.ExponentialBuckets(.001, 2, 16)},
		restClientLatencyLabels,
	)

	// Must carry the same label set as the request duration histogram: both are
	// observed through restClientLatencyAdapter, so a vector with fewer labels
	// panics on the first observation.
	metricRestClientRateLimiterDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: types.MetricOvnkubeNamespace,
		Name:      "rest_client_rate_limiter_duration_seconds",
		Help: "Time requests spent blocked by the client-side rate limiter, by verb, resource and host. " +
			"Non-trivial values mean the client QPS and burst settings, not the apiserver, are the limit.",
		Buckets: prometheus.ExponentialBuckets(.001, 2, 16)},
		restClientLatencyLabels,
	)

	metricRestClientRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: types.MetricOvnkubeNamespace,
		Name:      "rest_client_requests_total",
		Help:      "Number of requests to the apiserver, by status code, method and host."},
		[]string{"code", "method", "host"},
	)
)

type restClientLatencyAdapter struct {
	observer *prometheus.HistogramVec
}

func (a restClientLatencyAdapter) Observe(_ context.Context, verb string, u url.URL, latency time.Duration) {
	a.observer.WithLabelValues(verb, resourceFromPath(u.Path), u.Host).Observe(latency.Seconds())
}

type restClientResultAdapter struct{}

// Not labelled by resource: client-go calls the result hook with no URL and
// puts nothing in the context to recover one, so a status code cannot be
// attributed to a resource from here without wrapping the RoundTripper. Use
// the apiserver's own apiserver_request_total{code,resource,verb} for that; it
// is already scraped and it is authoritative.
func (restClientResultAdapter) Increment(_ context.Context, code, method, host string) {
	metricRestClientRequests.WithLabelValues(code, method, host).Inc()
}

// resourceFromPath turns a client-go templated request path into a bounded
// label. The template already substitutes {name} and {namespace}, so what is
// left is the group, the resource and an optional subresource, all of which
// come from a fixed set.
//
//	/api/v1/namespaces/{namespace}/pods/{name}          -> pods
//	/apis/k8s.ovn.org/v1/routeadvertisements/{name}      -> k8s.ovn.org/routeadvertisements
//	/apis/frrk8s.metallb.io/v1beta1/.../{name}/status    -> frrk8s.metallb.io/frrconfigurations/status
func resourceFromPath(p string) string {
	segments := strings.Split(strings.Trim(p, "/"), "/")
	var group string
	var rest []string
	switch {
	case len(segments) >= 2 && segments[0] == "api":
		rest = segments[2:]
	case len(segments) >= 3 && segments[0] == "apis":
		group = segments[1]
		rest = segments[3:]
	default:
		return "unknown"
	}
	// Drop a namespace scope if present: it is always the templated pair
	// "namespaces/{namespace}".
	if len(rest) >= 2 && rest[0] == "namespaces" && strings.HasPrefix(rest[1], "{") {
		rest = rest[2:]
	}
	if len(rest) == 0 {
		return "unknown"
	}
	out := rest[0]
	// Keep a subresource, which is a closed set, but never a name.
	if len(rest) >= 3 && !strings.HasPrefix(rest[2], "{") {
		out += "/" + rest[2]
	}
	if group != "" {
		out = group + "/" + out
	}
	return out
}

var registerClientMetricsOnce sync.Once

// registerClientGoMetrics installs the prometheus adapters for client-go's
// request hooks. Safe to call from more than one component: both this
// sync.Once and client-go's own one make every call after the first a no-op.
// Registration order does not matter, because client-go reads the hooks at
// request time rather than when a client is built.
func registerClientGoMetrics() {
	registerClientMetricsOnce.Do(func() {
		prometheus.MustRegister(metricRestClientRequestDuration)
		prometheus.MustRegister(metricRestClientRateLimiterDuration)
		prometheus.MustRegister(metricRestClientRequests)

		metrics.Register(metrics.RegisterOpts{
			RequestLatency:     restClientLatencyAdapter{observer: metricRestClientRequestDuration},
			RateLimiterLatency: restClientLatencyAdapter{observer: metricRestClientRateLimiterDuration},
			RequestResult:      restClientResultAdapter{},
		})
	})
}
