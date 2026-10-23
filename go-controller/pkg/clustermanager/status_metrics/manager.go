// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package statusmetrics

import (
	"context"
	"fmt"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/clustermanager/status_manager/zone_tracker"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

const (
	// DefaultRollupInterval is how often cluster-manager queries Prometheus
	// and patches coarse CR summaries when EnableStatusMetrics is true.
	DefaultRollupInterval = 30 * time.Second
	clusterManagerName    = "cluster-manager"
)

// ResourceKey identifies a Kubernetes object.
type ResourceKey struct {
	Namespace string
	Name      string
}

// String returns namespace/name or name for cluster-scoped resources.
func (k ResourceKey) String() string {
	if k.Namespace == "" {
		return k.Name
	}
	return k.Namespace + "/" + k.Name
}

// ResourceRoller rolls up metrics into a coarse CR summary for one resource type.
type ResourceRoller interface {
	// Name is a short identifier for logging.
	Name() string
	// ListKeys returns all resource instances to roll up.
	ListKeys() ([]ResourceKey, error)
	// RelevantNodes returns the live node set that must report sync success.
	RelevantNodes(key ResourceKey, zones sets.Set[string]) (sets.Set[string], error)
	// MetricName is the full Prometheus metric name (e.g. ovnkube_egressfirewall_sync_succeeded).
	MetricName() string
	// PatchSummary writes the coarse summary when the outcome changed.
	// failingNodes may be included (capped) in the summary text on failure.
	PatchSummary(key ResourceKey, outcome AggregateOutcome, failingNodes []string) error
}

// Manager periodically queries Prometheus and patches coarse CR summaries.
type Manager struct {
	client         *Client
	availability   *Availability
	zoneTracker    *zone_tracker.ZoneTracker
	rollers        []ResourceRoller
	interval       time.Duration
	stopCh         chan struct{}
	zonesLock      sync.RWMutex
	zones          sets.Set[string]
	networkManager networkmanager.Interface
	ovnClient      *util.OVNClusterManagerClientset
	wf             *factory.WatchFactory
}

// NewManager builds a metrics-status rollup manager. Call RegisterRoller for
// each resource type, then Start.
func NewManager(wf *factory.WatchFactory, ovnClient *util.OVNClusterManagerClientset, networkManager networkmanager.Interface) *Manager {
	m := &Manager{
		interval:       DefaultRollupInterval,
		stopCh:         make(chan struct{}),
		zones:          sets.New[string](),
		networkManager: networkManager,
		ovnClient:      ovnClient,
		wf:             wf,
	}
	url := config.Metrics.StatusMetricsPrometheusURL
	if url != "" {
		m.client = NewClient(url)
		m.availability = NewAvailability(m.client, defaultProbeTTL)
	}
	m.zoneTracker = zone_tracker.NewZoneTracker(wf.NodeCoreInformer(), m.onZoneUpdate)
	return m
}

// RegisterRoller adds a resource type to the periodic rollup loop.
func (m *Manager) RegisterRoller(r ResourceRoller) {
	m.rollers = append(m.rollers, r)
}

// NetworkManager returns the network manager for rollers that need UDN awareness.
func (m *Manager) NetworkManager() networkmanager.Interface {
	return m.networkManager
}

// WatchFactory returns the shared watch factory.
func (m *Manager) WatchFactory() *factory.WatchFactory {
	return m.wf
}

// OVNClient returns the cluster-manager clientset.
func (m *Manager) OVNClient() *util.OVNClusterManagerClientset {
	return m.ovnClient
}

// Start begins zone tracking and the periodic rollup loop.
func (m *Manager) Start() error {
	if err := m.zoneTracker.Start(); err != nil {
		return fmt.Errorf("failed to start zone tracker for status metrics: %w", err)
	}
	go wait.Until(m.rollupOnce, m.interval, m.stopCh)
	klog.Infof("Started status metrics rollup manager (interval=%s, prometheusURL=%q, rollers=%d)",
		m.interval, config.Metrics.StatusMetricsPrometheusURL, len(m.rollers))
	return nil
}

// Stop stops the rollup manager.
func (m *Manager) Stop() {
	close(m.stopCh)
	m.zoneTracker.Stop()
}

func (m *Manager) onZoneUpdate(newZones sets.Set[string]) {
	m.zonesLock.Lock()
	m.zones = newZones
	m.zonesLock.Unlock()
}

func (m *Manager) withZonesRLock(f func(zones sets.Set[string]) error) error {
	m.zonesLock.RLock()
	defer m.zonesLock.RUnlock()
	return f(m.zones)
}

func (m *Manager) rollupOnce() {
	if m.availability == nil || !m.availability.Available(context.Background()) {
		klog.V(5).Info("Status metrics rollup skipped: Prometheus unavailable")
		return
	}
	_ = m.withZonesRLock(func(zones sets.Set[string]) error {
		for _, roller := range m.rollers {
			if err := m.rollupResource(roller, zones); err != nil {
				klog.Errorf("Status metrics rollup for %s failed: %v", roller.Name(), err)
			}
		}
		return nil
	})
}

func (m *Manager) rollupResource(roller ResourceRoller, zones sets.Set[string]) error {
	keys, err := roller.ListKeys()
	if err != nil {
		return err
	}
	for _, key := range keys {
		relevant, err := roller.RelevantNodes(key, zones)
		if err != nil {
			klog.Errorf("Status metrics rollup %s %s: relevant nodes: %v", roller.Name(), key, err)
			continue
		}
		query := BuildSyncSucceededQuery(roller.MetricName(), key.Namespace, key.Name)
		samples, err := m.client.Query(context.Background(), query)
		if err != nil {
			klog.V(4).Infof("Status metrics rollup %s %s: query failed: %v", roller.Name(), key, err)
			continue
		}
		outcome, failing := AggregateFromSamples(samples, relevant)
		if err := roller.PatchSummary(key, outcome, failing); err != nil {
			klog.Errorf("Status metrics rollup %s %s: patch failed: %v", roller.Name(), key, err)
		}
	}
	return nil
}

// Zones returns a clone of the current OVN-managed zone set (for tests/helpers).
func (m *Manager) Zones() sets.Set[string] {
	m.zonesLock.RLock()
	defer m.zonesLock.RUnlock()
	return m.zones.Clone()
}

// ParseKey splits a "namespace/name" or "name" key.
func ParseKey(key string) (ResourceKey, error) {
	ns, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return ResourceKey{}, err
	}
	return ResourceKey{Namespace: ns, Name: name}, nil
}
