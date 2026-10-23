// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package statusmetrics

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

const (
	defaultQueryTimeout = 10 * time.Second
	defaultProbeTTL     = 30 * time.Second
)

// Sample is a single Prometheus instant-query sample with its labels.
type Sample struct {
	Labels map[string]string
	Value  float64
}

// Client queries a Prometheus HTTP API (/api/v1/query).
type Client struct {
	baseURL    string
	httpClient *http.Client
}

// NewClient creates a Prometheus query client for the given base URL
// (for example http://prometheus.monitoring.svc:9090).
func NewClient(baseURL string) *Client {
	return &Client{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: defaultQueryTimeout,
		},
	}
}

// Query runs an instant PromQL query and returns vector samples.
func (c *Client) Query(ctx context.Context, query string) ([]Sample, error) {
	if c == nil || c.baseURL == "" {
		return nil, fmt.Errorf("prometheus client not configured")
	}
	u, err := url.Parse(c.baseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid prometheus URL: %w", err)
	}
	// JoinPath preserves any path prefix on the configured base URL
	// (for example http://prometheus:9090/prometheus → .../prometheus/api/v1/query).
	u = u.JoinPath("api", "v1", "query")
	q := u.Query()
	q.Set("query", query)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("prometheus query returned status %d: %s", resp.StatusCode, string(body))
	}

	var parsed promQueryResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("failed to decode prometheus response: %w", err)
	}
	if parsed.Status != "success" {
		return nil, fmt.Errorf("prometheus query failed: %s", parsed.Error)
	}
	samples := make([]Sample, 0, len(parsed.Data.Result))
	for _, r := range parsed.Data.Result {
		if len(r.Value) < 2 {
			continue
		}
		valStr, ok := r.Value[1].(string)
		if !ok {
			continue
		}
		val, err := strconv.ParseFloat(valStr, 64)
		if err != nil {
			continue
		}
		samples = append(samples, Sample{Labels: r.Metric, Value: val})
	}
	return samples, nil
}

// Probe runs a lightweight canary query to check Prometheus availability.
func (c *Client) Probe(ctx context.Context) error {
	_, err := c.Query(ctx, "vector(1)")
	return err
}

type promQueryResponse struct {
	Status string `json:"status"`
	Error  string `json:"error"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Value  []any             `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

// Availability caches whether Prometheus is reachable.
type Availability struct {
	client *Client
	ttl    time.Duration

	mu            sync.Mutex
	available     bool
	lastCheckedAt time.Time
}

// NewAvailability creates an availability cache with the given TTL.
func NewAvailability(client *Client, ttl time.Duration) *Availability {
	if ttl <= 0 {
		ttl = defaultProbeTTL
	}
	return &Availability{client: client, ttl: ttl}
}

// Available returns cached availability, refreshing when the TTL has expired.
func (a *Availability) Available(ctx context.Context) bool {
	if a == nil || a.client == nil || a.client.baseURL == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if time.Since(a.lastCheckedAt) < a.ttl {
		return a.available
	}
	err := a.client.Probe(ctx)
	a.available = err == nil
	a.lastCheckedAt = time.Now()
	return a.available
}

// SetAvailableForTest overrides the cached availability (unit tests only).
func (a *Availability) SetAvailableForTest(available bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.available = available
	a.lastCheckedAt = time.Now()
}
