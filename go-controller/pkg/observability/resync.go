// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package observability

import (
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"

	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
)

// SamplingResyncHandler is implemented by controllers that own sampled ACLs (network policy,
// egress firewall, ANP/BANP, ...). When an ObservabilityConfig change alters the collectors
// resolved for a feature, the Manager calls ResyncSampling so the controller re-applies the
// affected ACLs' Sample references. namespaces is the set of namespaces whose collectors
// changed; a nil set means "all namespaces" (the feature's cluster-wide collectors changed,
// or the feature was fully added/removed), and the handler should resync every object it owns.
type SamplingResyncHandler interface {
	ResyncSampling(feature libovsdbops.SampleFeature, namespaces sets.Set[string]) error
}

// RegisterResyncHandler registers the handler to be notified of collector changes for feature.
//
// Handlers are invoked outside collectorsLock, so a handler may safely call back into the
// Manager (e.g. SamplingConfigForContext). Handlers register during controller startup, which
// happens after StartWatching.
// A controller's initial sync reads the current config directly via SamplingConfigForContext, so
// the config state at registration time needs no notification; but any config change dispatched in
// the window between StartWatching and registration is buffered in pendingResync and replayed here,
// so the handler does not miss it.
func (m *Manager) RegisterResyncHandler(feature libovsdbops.SampleFeature, handler SamplingResyncHandler) {
	m.resyncHandlersLock.Lock()
	if _, exists := m.resyncHandlers[feature]; exists {
		klog.Errorf("Observability: resync handler for feature %v registered more than once; overwriting", feature)
	}
	m.resyncHandlers[feature] = handler
	pending, hasPending := m.pendingResync[feature]
	delete(m.pendingResync, feature)
	m.resyncHandlersLock.Unlock()

	if !hasPending {
		return
	}
	// Replay the buffered scope outside the lock, matching dispatchResync's contract that a nil
	// namespaces set means "all namespaces".
	var namespaces sets.Set[string]
	if !pending.all {
		namespaces = pending.namespaces
	}
	if err := handler.ResyncSampling(feature, namespaces); err != nil {
		klog.Errorf("Observability: replaying buffered resync for feature %v failed: %v", feature, err)
	}
}

// resyncScope describes what to resync for a single feature. all=true means every object of
// the feature must be resynced (its cluster-wide collectors changed); otherwise only the
// listed namespaces changed.
type resyncScope struct {
	all        bool
	namespaces sets.Set[string]
}

// addToResolution folds one config's resolved feature->collectors into res, routing collectors
// to the cluster-wide set (no namespaces) or to each listed namespace's set. Sets deduplicate
// collectors declared by several configs.
func addToResolution(res map[libovsdbops.SampleFeature]*featureResolution, namespaces []string, featureCollectors map[libovsdbops.SampleFeature][]string) {
	for feature, collectors := range featureCollectors {
		if len(collectors) == 0 {
			continue
		}
		fr := res[feature]
		if fr == nil {
			fr = &featureResolution{cluster: sets.New[string](), perNS: map[string]sets.Set[string]{}}
			res[feature] = fr
		}
		if len(namespaces) == 0 {
			fr.cluster.Insert(collectors...)
			continue
		}
		for _, ns := range namespaces {
			s := fr.perNS[ns]
			if s == nil {
				s = sets.New[string]()
				fr.perNS[ns] = s
			}
			s.Insert(collectors...)
		}
	}
}

// diffResolutions computes, per feature, what changed between the old and new resolution.
// A feature is included only if its collectors changed. If the cluster-wide collectors
// changed, the whole feature is scoped for resync (all=true), since those collectors apply
// to every namespace; otherwise only the namespaces whose collectors differ are listed.
func diffResolutions(old, new map[libovsdbops.SampleFeature]*featureResolution) map[libovsdbops.SampleFeature]resyncScope {
	delta := map[libovsdbops.SampleFeature]resyncScope{}
	features := sets.New[libovsdbops.SampleFeature]()
	for f := range old {
		features.Insert(f)
	}
	for f := range new {
		features.Insert(f)
	}
	for feature := range features {
		oldFR := old[feature]
		newFR := new[feature]
		if clusterOf(oldFR).Equal(clusterOf(newFR)) {
			// Cluster-wide collectors unchanged: only per-namespace differences matter.
			changed := changedNamespaces(oldFR, newFR)
			if changed.Len() > 0 {
				delta[feature] = resyncScope{namespaces: changed}
			}
			continue
		}
		delta[feature] = resyncScope{all: true}
	}
	return delta
}

// clusterOf returns the cluster-wide collector set of fr, tolerating a nil resolution.
func clusterOf(fr *featureResolution) sets.Set[string] {
	if fr == nil {
		return nil
	}
	return fr.cluster
}

// changedNamespaces returns the namespaces whose per-namespace collector set differs between
// old and new (added, removed, or altered).
func changedNamespaces(old, new *featureResolution) sets.Set[string] {
	changed := sets.New[string]()
	namespaces := sets.New[string]()
	if old != nil {
		for ns := range old.perNS {
			namespaces.Insert(ns)
		}
	}
	if new != nil {
		for ns := range new.perNS {
			namespaces.Insert(ns)
		}
	}
	for ns := range namespaces {
		var oldNS, newNS sets.Set[string]
		if old != nil {
			oldNS = old.perNS[ns]
		}
		if new != nil {
			newNS = new.perNS[ns]
		}
		if !oldNS.Equal(newNS) {
			changed.Insert(ns)
		}
	}
	return changed
}

// dispatchResync notifies the registered handler of the computed delta. It snapshots the
// handler for each feature under resyncHandlersLock and invokes it outside the lock, so the
// handler may call back into the Manager. Per the handler contract, a nil namespaces set means
// "all". A feature whose handler has not registered yet has its scope buffered in pendingResync
// (merged with anything already buffered) and replayed when the handler registers, so a config
// change during controller startup is not lost.
func (m *Manager) dispatchResync(delta map[libovsdbops.SampleFeature]resyncScope) {
	if len(delta) == 0 {
		return
	}
	type dispatch struct {
		feature    libovsdbops.SampleFeature
		namespaces sets.Set[string]
		handler    SamplingResyncHandler
	}
	m.resyncHandlersLock.Lock()
	dispatches := make([]dispatch, 0, len(delta))
	for feature, scope := range delta {
		handler, ok := m.resyncHandlers[feature]
		if !ok {
			// No handler yet: buffer the scope so it can be replayed on registration.
			m.pendingResync[feature] = mergeScopes(m.pendingResync[feature], scope)
			continue
		}
		var namespaces sets.Set[string]
		if !scope.all {
			namespaces = scope.namespaces
		}
		dispatches = append(dispatches, dispatch{feature: feature, namespaces: namespaces, handler: handler})
	}
	m.resyncHandlersLock.Unlock()

	for _, d := range dispatches {
		if err := d.handler.ResyncSampling(d.feature, d.namespaces); err != nil {
			klog.Errorf("Observability: resync handler for feature %v failed: %v", d.feature, err)
		}
	}
}

// mergeScopes combines two resync scopes for the same feature. "all" dominates: if either
// scope resyncs the whole feature, so does the result; otherwise the namespace sets are unioned.
func mergeScopes(a, b resyncScope) resyncScope {
	if a.all || b.all {
		return resyncScope{all: true}
	}
	merged := sets.New[string]()
	merged.Insert(sets.List(a.namespaces)...)
	merged.Insert(sets.List(b.namespaces)...)
	return resyncScope{namespaces: merged}
}
