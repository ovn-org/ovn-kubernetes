// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"context"
	"net/url"
	"testing"
	"time"
)

func TestResourceFromPath(t *testing.T) {
	// Paths as client-go's finalURLTemplate renders them: {name} and
	// {namespace} are already substituted, so the label stays bounded.
	for _, tc := range []struct {
		path string
		want string
	}{
		{"/api/v1/namespaces/{namespace}/pods/{name}", "pods"},
		{"/api/v1/nodes", "nodes"},
		{"/api/v1/nodes/{name}", "nodes"},
		{"/api/v1/nodes/{name}/status", "nodes/status"},
		{"/apis/k8s.ovn.org/v1/routeadvertisements/{name}", "k8s.ovn.org/routeadvertisements"},
		{"/apis/k8s.ovn.org/v1/routeadvertisements/{name}/status", "k8s.ovn.org/routeadvertisements/status"},
		{"/apis/frrk8s.metallb.io/v1beta1/namespaces/{namespace}/frrconfigurations", "frrk8s.metallb.io/frrconfigurations"},
		{"/apis/frrk8s.metallb.io/v1beta1/namespaces/{namespace}/frrconfigurations/{name}", "frrk8s.metallb.io/frrconfigurations"},
		{"/apis/apps/v1/namespaces/{namespace}/deployments/{name}/status", "apps/deployments/status"},
		// Anything that is not an API path must not become a label value.
		{"/healthz", "unknown"},
		{"/", "unknown"},
		{"", "unknown"},
	} {
		if got := resourceFromPath(tc.path); got != tc.want {
			t.Errorf("resourceFromPath(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

// TestRestClientAdaptersObserve drives every client-go hook the way client-go
// drives it. It exists because a HistogramVec panics on a label-cardinality
// mismatch at observation time, not at registration time: the rate limiter
// vector was once declared with two labels while the shared adapter passed
// three, which built and linted cleanly and then crashed ovnkube on the first
// throttled request.
func TestRestClientAdaptersObserve(t *testing.T) {
	u, err := url.Parse("https://10.0.0.1:6443/api/v1/nodes/{name}/status")
	if err != nil {
		t.Fatal(err)
	}
	latency := restClientLatencyAdapter{observer: metricRestClientRequestDuration}
	rateLimiter := restClientLatencyAdapter{observer: metricRestClientRateLimiterDuration}

	// Each of these panics rather than failing if a vector and the adapter
	// disagree on cardinality.
	latency.Observe(context.Background(), "PATCH", *u, time.Millisecond)
	rateLimiter.Observe(context.Background(), "PATCH", *u, time.Millisecond)
	restClientResultAdapter{}.Increment(context.Background(), "200", "PATCH", u.Host)
}

// TestRestClientLatencyVectorsShareLabels pins the invariant directly, so a new
// vector wired to the shared adapter has to adopt the same labels.
func TestRestClientLatencyVectorsShareLabels(t *testing.T) {
	if got := len(restClientLatencyLabels); got != 3 {
		t.Fatalf("restClientLatencyLabels has %d labels, adapter passes 3", got)
	}
}
