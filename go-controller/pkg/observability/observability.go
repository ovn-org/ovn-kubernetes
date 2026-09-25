// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package observability

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	observabilityconfigv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/observabilityconfig/v1alpha1"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
)

// OVN SamplingApp IDs; must be < 255. Add new apps at the end.
const (
	DropSamplingID = iota + 1
	ACLNewTrafficSamplingID
	ACLEstTrafficSamplingID
)

// maxCollectorID is the OVN Sample_Collector row id limit (table column id).
const maxCollectorID = 255

const collectorFeaturesExternalID = "sample-features"

// maxCollectorsCleanupRetries bounds how many times stale-collector cleanup is retried
// before giving up. Combined with unusedCollectorsRetryInterval (default 1 minute) this
// allows roughly one hour for all controllers to complete their initial sync and stop
// referencing stale collectors.
const maxCollectorsCleanupRetries = 60

// collectorConfig holds the configuration for a collector.
// It is allowed to set different probabilities for every feature.
// collectorSetID is used to set up sampling via OVSDB.
type collectorConfig struct {
	collectorSetID int
	// probability in percent, 0 to 100
	featuresProbability map[libovsdbops.SampleFeature]int
}

// featureResolution captures, for a single feature, the collectors that apply cluster-wide
// (from configs with no Filter.Namespaces) and the per-namespace collectors (from
// namespace-scoped configs). Collector identity is a UUID; storing sets both deduplicates
// automatically (the same collector may be declared by several configs) and lets an
// ObservabilityConfig change be diffed directly for targeted resync (see resync.go).
type featureResolution struct {
	cluster sets.Set[string]            // collectors applied to every namespace
	perNS   map[string]sets.Set[string] // namespace -> collectors applied only there
}

type Manager struct {
	nbClient libovsdbclient.Client
	// resolution is the published per-feature collector assignment for this node, rebuilt
	// on every applyConfigs/clearConfig. It is both what SamplingConfigForContext resolves
	// against and the basis for computing resync deltas. nil means nothing applies.
	resolution     map[libovsdbops.SampleFeature]*featureResolution
	collectorsLock sync.RWMutex
	// nbdb Collectors have probability. To allow different probabilities for different features,
	// multiple nbdb Collectors will be created, one per probability.
	// getCollectorKey() => collector.UUID
	dbCollectors map[string]string
	// cleaning up unused collectors may take time and multiple retries, as all referencing samples must be removed first.
	// Therefore, we need to save state between those retries.
	// getCollectorKey() => collector.SetID
	unusedCollectors              map[string]int
	unusedCollectorsRetryInterval time.Duration
	// Only maxCollectorID collectors are allowed, each should have unique ID.
	// this set is tracking already assigned IDs.
	takenCollectorIDs sets.Set[int]

	// Stale-collector cleanup retry state. A single timer is armed at a time;
	// cleanupLock guards all of these fields (including collectorsCleanupRetries).
	// cleanupLock and collectorsLock are never held simultaneously.
	cleanupLock              sync.Mutex
	cleanupTimer             *time.Timer
	collectorsCleanupRetries int
	stopped                  bool

	// resyncHandlers lets the controller that owns a feature's sampled ACLs (network policy,
	// egress firewall, ANP/BANP, ...) be notified when a config change alters the collectors
	// resolved for that feature, so it can re-apply only the affected ACLs. Each feature is owned
	// by exactly one controller, so there is a single handler per feature. Guarded by
	// resyncHandlersLock, which is never held together with collectorsLock.
	resyncHandlersLock sync.RWMutex
	resyncHandlers     map[libovsdbops.SampleFeature]SamplingResyncHandler
	// pendingResync buffers the resync scope for a feature whose handler was not yet registered
	// when a config change was dispatched. Controllers register their handlers only after their
	// initial sync completes (see RegisterResyncHandler), so a config change during that window
	// would otherwise be dropped by dispatchResync and leave those ACLs with stale collectors. The
	// buffered scope is replayed when the handler registers. Guarded by resyncHandlersLock.
	pendingResync map[libovsdbops.SampleFeature]resyncScope
}

func NewManager(nbClient libovsdbclient.Client) *Manager {
	return &Manager{
		nbClient:                      nbClient,
		collectorsLock:                sync.RWMutex{},
		dbCollectors:                  make(map[string]string),
		unusedCollectors:              make(map[string]int),
		unusedCollectorsRetryInterval: time.Minute,
		takenCollectorIDs:             sets.New[int](),
		resyncHandlers:                make(map[libovsdbops.SampleFeature]SamplingResyncHandler),
		pendingResync:                 make(map[libovsdbops.SampleFeature]resyncScope),
	}
}

// SamplingConfigForContext returns the sampling config to use for an ACL in the given
// namespace and feature. All configs that apply are merged: both namespace-scoped
// configs (Filter.Namespaces containing namespace) and cluster-scoped configs (no
// Filter.Namespaces), so the ACL's Sample references every matching collector and
// each receives samples at its configured probability. Returns nil if no config
// applies for that context.
func (m *Manager) SamplingConfigForContext(namespace string, feature libovsdbops.SampleFeature) *libovsdbops.SamplingConfig {
	m.collectorsLock.RLock()
	defer m.collectorsLock.RUnlock()
	fc := m.resolveForContextLocked(namespace, feature)
	if fc == nil {
		return nil
	}
	return libovsdbops.NewSamplingConfig(fc)
}

// resolveForContextLocked returns a featureCollectors map for (namespace, feature):
// the feature's cluster-wide collectors (from configs with no Filter.Namespaces) merged
// with the collectors scoped to this namespace. Collector UUIDs land in the
// Sample.Collectors OVSDB set column, so storing them as sets deduplicates naturally;
// the result is sorted for a deterministic Sample.Collectors. Returns nil if nothing
// applies. Caller must hold m.collectorsLock (at least RLock).
func (m *Manager) resolveForContextLocked(namespace string, feature libovsdbops.SampleFeature) map[libovsdbops.SampleFeature][]string {
	fr := m.resolution[feature]
	if fr == nil {
		return nil
	}
	merged := fr.cluster.Union(fr.perNS[namespace])
	if merged.Len() == 0 {
		return nil
	}
	return map[libovsdbops.SampleFeature][]string{feature: sets.List(merged)}
}

// Init sets up sampling app IDs and loads existing collector state from the DB.
// It does not apply any ObservabilityConfig; config is applied when StartWatching
// receives an ObservabilityConfig that applies to this node.
func (m *Manager) Init() error {
	if err := m.setSamplingAppIDs(); err != nil {
		return err
	}
	m.collectorsLock.Lock()
	defer m.collectorsLock.Unlock()
	return m.retrieveDbCollectorsLocked()
}

// StartWatching watches ObservabilityConfig CRs and applies all that apply to this node.
// In details: configs whose Filter.NodeSelector matches the local node labels apply;
// configs with no Filter.NodeSelector always apply. nodeWatcher is required (non-nil): a change
// to the local node's labels re-triggers reconciliation so a NodeSelector that starts or stops
// matching is honored. If the local node cannot be resolved, reconciliation is requeued.
// Multiple configs can apply (e.g. one cluster-wide, one namespace-scoped); use SamplingConfigForContext
// when creating ACLs so the correct config is chosen per (namespace, feature). Call after Init().
//
// The k8s watching and reconcile loop live in configReconciler (config_reconciler.go); this
// Manager provides the apply/clear engine it drives.
func (m *Manager) StartWatching(informer ObservabilityConfigInformer, nodeWatcher NodeWatcher, nodeName string, stopChan <-chan struct{}) {
	if informer == nil {
		return
	}
	if nodeWatcher == nil || nodeWatcher.NodeInformer() == nil {
		klog.Errorf("Observability: cannot start ObservabilityConfig reconciler: node watcher/informer is nil")
		return
	}
	r := newConfigReconciler(m, informer, nodeWatcher, nodeName)
	if err := r.start(); err != nil {
		klog.Errorf("Observability: failed to start ObservabilityConfig reconciler: %v", err)
		return
	}
	// Tear everything down when the stop channel closes: the reconciler (event handler +
	// worker) and this Manager's stale-collector cleanup timer.
	go func() {
		<-stopChan
		r.stop()
		m.stopCleanupTimer()
	}()
}

// clearConfig clears the published resolution and triggers cleanup of collectors.
// SamplingConfig() and SamplingConfigForContext() will return nil until configs are applied
// again. Any feature that had collectors is dispatched for resync so its ACLs drop the
// now-removed Sample references.
func (m *Manager) clearConfig() {
	m.collectorsLock.Lock()
	oldResolution := m.resolution
	m.resolution = nil
	delta := diffResolutions(oldResolution, nil)
	// Rebuild the DB snapshot so every existing collector is marked unused, then
	// delete them. Done under the same lock as retrieval so a concurrent reader
	// never observes a half-cleared state.
	staleErr := m.retrieveDbCollectorsLocked()
	if staleErr == nil {
		staleErr = m.deleteStaleCollectorsLocked()
	}
	m.collectorsLock.Unlock()
	m.dispatchResync(delta)
	m.scheduleStaleCleanupRetry(staleErr)
}

func collectorConfigFromCR(cr *observabilityconfigv1alpha1.ObservabilityConfig) *collectorConfig {
	c := &collectorConfig{
		collectorSetID:      int(cr.Spec.CollectorID),
		featuresProbability: make(map[libovsdbops.SampleFeature]int),
	}
	for _, f := range cr.Spec.Features {
		sf := observabilityFeatureToSampleFeature(f.Feature)
		if sf != "" {
			c.featuresProbability[sf] = int(f.Probability)
		} else {
			klog.Warningf("Observability: unknown feature in %s: %s", cr.Name, f.Feature)
		}
	}
	return c
}

func observabilityFeatureToSampleFeature(f observabilityconfigv1alpha1.ObservabilityFeature) libovsdbops.SampleFeature {
	switch f {
	case observabilityconfigv1alpha1.NetworkPolicy:
		return libovsdbops.NetworkPolicySample
	case observabilityconfigv1alpha1.AdminNetworkPolicy:
		return libovsdbops.AdminNetworkPolicySample
	case observabilityconfigv1alpha1.EgressFirewall:
		return libovsdbops.EgressFirewallSample
	case observabilityconfigv1alpha1.UDNIsolation:
		return libovsdbops.UDNIsolationSample
	case observabilityconfigv1alpha1.MulticastIsolation:
		return libovsdbops.MulticastSample
	default:
		return ""
	}
}

// namespacedObservabilityFeatures are the only features that support Filter.Namespaces (per-namespace filtering).
var namespacedObservabilityFeatures = map[observabilityconfigv1alpha1.ObservabilityFeature]struct{}{
	observabilityconfigv1alpha1.NetworkPolicy:  {},
	observabilityconfigv1alpha1.EgressFirewall: {},
}

// validateObservabilityConfig returns an error if the CR fails validation (e.g. collectorID/set_id out of range or probability not 0..100).
// API server CRD validation should enforce these too; this is defense in depth.
func validateObservabilityConfig(cr *observabilityconfigv1alpha1.ObservabilityConfig) error {
	if cr.Spec.CollectorID < 1 || cr.Spec.CollectorID > math.MaxUint32 {
		return fmt.Errorf("ObservabilityConfig %s: collectorID (set_id) must be between 1 and %d, got %d", cr.Name, int64(math.MaxUint32), cr.Spec.CollectorID)
	}
	for _, f := range cr.Spec.Features {
		if f.Probability < 0 || f.Probability > 100 {
			return fmt.Errorf("ObservabilityConfig %s: feature %s probability must be 0..100, got %d", cr.Name, f.Feature, f.Probability)
		}
	}
	// Namespace filter only applies to namespaced features (NetworkPolicy, EgressFirewall). Reject if any feature is cluster-scoped.
	if cr.Spec.Filter != nil && len(cr.Spec.Filter.Namespaces) > 0 {
		for _, f := range cr.Spec.Features {
			if _, ok := namespacedObservabilityFeatures[f.Feature]; !ok {
				return fmt.Errorf("ObservabilityConfig %s: Filter.Namespaces can only be used with namespaced features (NetworkPolicy, EgressFirewall); feature %s is cluster-scoped", cr.Name, f.Feature)
			}
		}
	}
	return nil
}

// applyConfigs applies all given ObservabilityConfigs: ensures collectors exist for each,
// stores them for context resolution, and publishes the resolved set for context lookups.
//
// Retrieval of the current DB collectors, selection of active collectors and stale cleanup
// all happen under a single hold of collectorsLock, so a concurrent stale-cleanup pass can
// never delete a collector between retrieval and reuse (items 6/7/11).
//
// A single invalid or failing config does not abort the others, and the two failure classes
// are treated differently:
//   - Invalid configs are skipped with a warning and do NOT fail the reconcile: they can only
//     be fixed by the user editing the CR, so requeuing would never converge. Surfacing these
//     to the user via status conditions is planned as a follow-up.
//   - Transient failures (e.g. OVSDB errors) are collected and returned joined, so the reconcile
//     path requeues them with backoff.
//
// Either way the published set reflects every config that could be applied.
func (m *Manager) applyConfigs(configs []*observabilityconfigv1alpha1.ObservabilityConfig) error {
	m.collectorsLock.Lock()

	// Retrieve current active collectors to rebuild the unused list. A failure here is
	// retriable and leaves the previously published set untouched.
	if err := m.retrieveDbCollectorsLocked(); err != nil {
		m.collectorsLock.Unlock()
		return err
	}

	var applyErrs []error
	newResolution := make(map[libovsdbops.SampleFeature]*featureResolution)
	for _, cr := range configs {
		if err := validateObservabilityConfig(cr); err != nil {
			// Non-convergent: skip and log, but don't fail the reconcile.
			klog.Warningf("Observability: skipping invalid ObservabilityConfig %s: %v", cr.Name, err)
			continue
		}
		conf := collectorConfigFromCR(cr)
		featureCollectors, err := m.addCollectorLocked(conf)
		if err != nil {
			// Transient: collect so the reconcile requeues.
			applyErrs = append(applyErrs, fmt.Errorf("ObservabilityConfig %s: %w", cr.Name, err))
			continue
		}
		var namespaces []string
		if cr.Spec.Filter != nil {
			namespaces = cr.Spec.Filter.Namespaces
		}
		addToResolution(newResolution, namespaces, featureCollectors)
	}
	// Publish the fully-built resolution atomically: readers never observe a partially-applied
	// state. Diff against the previous resolution so only features whose collectors actually
	// changed get their ACLs resynced.
	oldResolution := m.resolution
	m.resolution = newResolution
	delta := diffResolutions(oldResolution, newResolution)
	staleErr := m.deleteStaleCollectorsLocked()
	m.collectorsLock.Unlock()

	m.dispatchResync(delta)
	m.scheduleStaleCleanupRetry(staleErr)
	return errors.Join(applyErrs...)
}

// retrieveDbCollectorsLocked rebuilds the DB collector snapshot (dbCollectors,
// takenCollectorIDs) and marks every collector as unused until active configs claim
// them. Only observability-owned collectors (those carrying the collectorFeaturesExternalID
// external ID) are considered, so collectors owned by other components are never marked
// unused nor swept by deleteStaleCollectorsLocked. Caller must hold m.collectorsLock.
func (m *Manager) retrieveDbCollectorsLocked() error {
	clear(m.dbCollectors)
	collectors, err := libovsdbops.FindSampleCollectorWithPredicate(m.nbClient, func(c *nbdb.SampleCollector) bool {
		_, ok := c.ExternalIDs[collectorFeaturesExternalID]
		return ok
	})
	if err != nil {
		return fmt.Errorf("error getting sample collectors: %w", err)
	}
	for _, collector := range collectors {
		collectorKey := getCollectorKey(collector.SetID, collector.Probability)
		m.dbCollectors[collectorKey] = collector.UUID
		m.takenCollectorIDs.Insert(collector.ID)
		// all collectors are unused, until we update existing configs
		m.unusedCollectors[collectorKey] = collector.ID
	}
	return nil
}

// Stale collectors can't be deleted until all referencing Samples are deleted.
// Samples are deleted asynchronously by different controllers as they reconcile against the
// latest observability config, so cleanup is retried on a single timer until it succeeds.
//
// scheduleStaleCleanupRetry records the outcome of the most recent cleanup pass and (re)arms
// at most one retry timer. err == nil means the last pass fully succeeded. It must be called
// without holding collectorsLock; cleanupLock and collectorsLock are never held together.
func (m *Manager) scheduleStaleCleanupRetry(err error) {
	m.cleanupLock.Lock()
	defer m.cleanupLock.Unlock()
	// Only one pending retry timer at a time.
	if m.cleanupTimer != nil {
		m.cleanupTimer.Stop()
		m.cleanupTimer = nil
	}
	if err == nil {
		if m.collectorsCleanupRetries > 0 {
			klog.Infof("Observability: stale collector cleanup succeeded after %d retries", m.collectorsCleanupRetries)
		}
		m.collectorsCleanupRetries = 0
		return
	}
	m.collectorsCleanupRetries++
	// allow retries for ~1 hour, hopefully enough for all handlers to complete initial sync
	if m.collectorsCleanupRetries > maxCollectorsCleanupRetries {
		m.collectorsCleanupRetries = 0
		klog.Errorf("Observability: giving up cleaning up stale collectors after %d retries: %v", maxCollectorsCleanupRetries, err)
		return
	}
	if m.stopped {
		return
	}
	m.cleanupTimer = time.AfterFunc(m.unusedCollectorsRetryInterval, m.retryStaleCleanup)
}

// retryStaleCleanup runs one stale-collector cleanup pass and reschedules based on its
// outcome. It is invoked from the cleanup timer.
func (m *Manager) retryStaleCleanup() {
	m.collectorsLock.Lock()
	err := m.deleteStaleCollectorsLocked()
	m.collectorsLock.Unlock()
	m.scheduleStaleCleanupRetry(err)
}

// stopCleanupTimer cancels any pending cleanup retry and prevents future ones.
func (m *Manager) stopCleanupTimer() {
	m.cleanupLock.Lock()
	defer m.cleanupLock.Unlock()
	m.stopped = true
	if m.cleanupTimer != nil {
		m.cleanupTimer.Stop()
		m.cleanupTimer = nil
	}
}

// deleteStaleCollectorsLocked deletes every collector currently marked unused, continuing
// past individual failures and returning the last error. Caller must hold m.collectorsLock.
func (m *Manager) deleteStaleCollectorsLocked() error {
	var lastErr error
	for collectorKey, collectorSetID := range m.unusedCollectors {
		collectorUUID := m.dbCollectors[collectorKey]
		err := libovsdbops.DeleteSampleCollector(m.nbClient, &nbdb.SampleCollector{
			UUID: collectorUUID,
		})
		if err != nil {
			lastErr = err
			klog.Infof("Error deleting collector with ID=%d: %v", collectorSetID, lastErr)
			continue
		}
		delete(m.unusedCollectors, collectorKey)
		delete(m.dbCollectors, collectorKey)
		delete(m.takenCollectorIDs, collectorSetID)
	}
	return lastErr
}

// Cleanup must be called when observability is no longer needed.
// It will return an error if some samples still exist in the db.
// This is expected, and Cleanup may be retried on the next restart.
func Cleanup(nbClient libovsdbclient.Client) error {
	// Do the opposite of init
	err := libovsdbops.DeleteSamplingAppsWithPredicate(nbClient, func(_ *nbdb.SamplingApp) bool {
		return true
	})
	if err != nil {
		return fmt.Errorf("error deleting sampling apps: %w", err)
	}

	err = libovsdbops.DeleteSampleCollectorWithPredicate(nbClient, func(_ *nbdb.SampleCollector) bool {
		return true
	})
	if err != nil {
		return fmt.Errorf("error deleting sample collectors: %w", err)
	}
	return nil
}

func (m *Manager) setSamplingAppIDs() error {
	var ops []ovsdb.Operation
	var err error
	for _, appConfig := range []struct {
		id      int
		appType nbdb.SamplingAppType
	}{
		{
			id:      DropSamplingID,
			appType: nbdb.SamplingAppTypeDrop,
		},
		{
			id:      ACLNewTrafficSamplingID,
			appType: nbdb.SamplingAppTypeACLNew,
		},
		{
			id:      ACLEstTrafficSamplingID,
			appType: nbdb.SamplingAppTypeACLEst,
		},
	} {
		samplingApp := &nbdb.SamplingApp{
			ID:   appConfig.id,
			Type: appConfig.appType,
		}
		ops, err = libovsdbops.CreateOrUpdateSamplingAppsOps(m.nbClient, ops, samplingApp)
		if err != nil {
			return fmt.Errorf("error creating or updating sampling app %s: %w", appConfig.appType, err)
		}
	}
	_, err = libovsdbops.TransactAndCheck(m.nbClient, ops)
	return err
}

func groupByProbability(c *collectorConfig) map[int][]libovsdbops.SampleFeature {
	probabilities := make(map[int][]libovsdbops.SampleFeature)
	for feature, percentProbability := range c.featuresProbability {
		probability := percentToProbability(percentProbability)
		probabilities[probability] = append(probabilities[probability], feature)
	}
	return probabilities
}

func getCollectorKey(collectorID int, probability int) string {
	return fmt.Sprintf("%d-%d", collectorID, probability)
}

func (m *Manager) getFreeCollectorID() (int, error) {
	for i := 1; i <= maxCollectorID; i++ {
		if !m.takenCollectorIDs.Has(i) {
			return i, nil
		}
	}
	return 0, fmt.Errorf("no free collector IDs")
}

// addCollectorLocked ensures collectors exist in the DB for the config and returns feature->collector UUIDs.
// Caller must hold m.collectorsLock.
func (m *Manager) addCollectorLocked(conf *collectorConfig) (map[libovsdbops.SampleFeature][]string, error) {
	sampleFeaturesConfig := make(map[libovsdbops.SampleFeature][]string)
	probabilityConfig := groupByProbability(conf)

	for probability, features := range probabilityConfig {
		collectorKey := getCollectorKey(conf.collectorSetID, probability)
		var collectorUUID string
		var ok bool
		// ensure predictable externalID
		slices.Sort(features)
		collectorFeatures := strings.Join(features, ",")
		if collectorUUID, ok = m.dbCollectors[collectorKey]; !ok {
			collectorID, err := m.getFreeCollectorID()
			if err != nil {
				return sampleFeaturesConfig, err
			}
			collector := &nbdb.SampleCollector{
				ID:          collectorID,
				SetID:       conf.collectorSetID,
				Probability: probability,
				ExternalIDs: map[string]string{
					collectorFeaturesExternalID: collectorFeatures,
				},
			}
			err = libovsdbops.CreateOrUpdateSampleCollector(m.nbClient, collector)
			if err != nil {
				return sampleFeaturesConfig, err
			}
			collectorUUID = collector.UUID
			m.dbCollectors[collectorKey] = collectorUUID
			m.takenCollectorIDs.Insert(collectorID)
		} else {
			// The collector already exists, so it is used regardless of what follows: mark it
			// used up front. Otherwise a later failure (e.g. the metadata update below) would
			// leave it flagged unused and the stale-cleanup sweep at the end of applyConfigs
			// would delete a collector we still want (only to recreate it on the next retry).
			delete(m.unusedCollectors, collectorKey)
			// update collector's features
			collector := &nbdb.SampleCollector{
				UUID: collectorUUID,
				ExternalIDs: map[string]string{
					collectorFeaturesExternalID: collectorFeatures,
				},
			}
			if err := libovsdbops.UpdateSampleCollectorExternalIDs(m.nbClient, collector); err != nil {
				return sampleFeaturesConfig, err
			}
		}
		for _, feature := range features {
			sampleFeaturesConfig[feature] = append(sampleFeaturesConfig[feature], collectorUUID)
		}
	}
	return sampleFeaturesConfig, nil
}

func percentToProbability(percent int) int {
	return 65535 * percent / 100
}
