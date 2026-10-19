// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"fmt"
	"net"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	utilerrors "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/errors"
)

// registerObservabilityResyncHandlers registers the default network controller as the
// observability resync handler for the features whose ACLs it owns directly (network policy and
// multicast). Only the default network controller has a non-nil observManager (secondary/UDN
// controllers leave it nil), so this is the only place these features get resync wiring.
//
// Registration happens after the informers have synced, so it may race with an ObservabilityConfig
// change: the initial ACL creation reads the current config through GetSamplingConfig, and any
// change dispatched in the window before this registration is buffered by the Manager and replayed
// on RegisterResyncHandler, so no resync is lost.
func (oc *DefaultNetworkController) registerObservabilityResyncHandlers() {
	if oc.observManager == nil {
		return
	}
	oc.observManager.RegisterResyncHandler(libovsdbops.NetworkPolicySample, oc)
	oc.observManager.RegisterResyncHandler(libovsdbops.MulticastSample, oc)
	oc.observManager.RegisterResyncHandler(libovsdbops.UDNIsolationSample, oc)
}

// ResyncSampling re-applies the observability Sample.Collectors on the ACLs owned by the default
// network controller for the given feature after an ObservabilityConfig change. A nil namespaces
// set means "all namespaces" (the feature's cluster-wide collectors changed, or the feature was
// fully added/removed); otherwise only the listed namespaces are affected. It implements
// observability.SamplingResyncHandler.
func (oc *DefaultNetworkController) ResyncSampling(feature libovsdbops.SampleFeature, namespaces sets.Set[string]) error {
	switch feature {
	case libovsdbops.NetworkPolicySample:
		return oc.resyncNetworkPolicySampling(namespaces)
	case libovsdbops.MulticastSample:
		return oc.resyncMulticastSampling(namespaces)
	case libovsdbops.UDNIsolationSample:
		return oc.resyncUDNIsolationSampling()
	default:
		return fmt.Errorf("observability resync requested for unhandled feature %q", feature)
	}
}

// resyncNetworkPolicySampling refreshes the sampling config on the ACLs owned by network policy.
// NetworkPolicy is a namespaced object and its ACLs resolve sampling per-namespace, so when a
// specific namespace set is given only that namespace's objects are refreshed.
//
// Two kinds of ACLs need refreshing:
//   - The shared per-namespace default-deny ACLs. These are created once, when the first policy in a
//     namespace is added (addPolicyToDefaultPortGroups skips them once the shared port group exists),
//     so re-enqueuing policies alone leaves their Sample.Collectors stale until the last policy is
//     removed. They are refreshed directly here via refreshDefaultDenySampling.
//   - The policy-specific ACLs. The affected policies are re-enqueued so addNetworkPolicy rebuilds
//     them with the current sampling config. Re-enqueuing through the retry framework keeps the
//     namespace/policy locking identical to the normal reconcile path (addNetworkPolicy acquires
//     those locks itself).
func (oc *DefaultNetworkController) resyncNetworkPolicySampling(namespaces sets.Set[string]) error {
	var errs []error

	// Refresh the shared default-deny ACLs for every affected namespace that has policies. When a
	// namespace set is given, only those namespaces are refreshed; otherwise every namespace with a
	// shared default-deny port group is.
	denyTargets := oc.sharedNetpolPortGroups.GetKeys()
	for _, ns := range denyTargets {
		if namespaces != nil && !namespaces.Has(ns) {
			continue
		}
		if err := oc.refreshDefaultDenySampling(ns); err != nil {
			errs = append(errs, err)
		}
	}

	requeued := false
	for _, key := range oc.networkPolicies.GetKeys() {
		ns, name, err := cache.SplitMetaNamespaceKey(key)
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to split network policy key %q: %w", key, err))
			continue
		}
		if namespaces != nil && !namespaces.Has(ns) {
			continue
		}
		np, err := oc.watchFactory.GetNetworkPolicy(ns, name)
		if err != nil {
			if apierrors.IsNotFound(err) {
				// The policy was deleted since it was cached; nothing to resync.
				klog.V(5).Infof("Observability resync: skipping deleted network policy %s", key)
				continue
			}
			errs = append(errs, fmt.Errorf("failed to get network policy %s for observability resync: %w", key, err))
			continue
		}
		if err := oc.retryNetworkPolicies.AddRetryObjWithAddNoBackoff(np); err != nil {
			errs = append(errs, err)
			continue
		}
		requeued = true
	}
	if requeued {
		oc.retryNetworkPolicies.RequestRetryObjs()
	}
	return utilerrors.Join(errs...)
}

// refreshDefaultDenySampling re-applies the current sampling config on a namespace's shared
// default-deny ACLs, preserving the namespace's ACL-logging configuration. These ACLs are shared by
// every policy in the namespace and are only (re)built when the first policy is added, so a sampling
// config change would otherwise leave them with stale collectors until the last policy is deleted.
// It mirrors the normal add path's lock ordering (namespace lock, then shared port group lock),
// holding the namespace read lock across the OVN update so aclLogging cannot change under it.
func (oc *DefaultNetworkController) refreshDefaultDenySampling(ns string) error {
	nsInfo, unlock := oc.getNamespaceLocked(ns, true)
	if nsInfo == nil {
		// Namespace is not tracked; nothing to refresh.
		return nil
	}
	defer unlock()
	aclLogging := nsInfo.aclLogging

	return oc.sharedNetpolPortGroups.DoWithLock(ns, func(pgKey string) error {
		if _, loaded := oc.sharedNetpolPortGroups.Load(pgKey); !loaded {
			// No policies in this namespace, so the shared default-deny ACLs don't exist.
			return nil
		}
		if err := oc.createDefaultDenyPGAndACLs(ns, "", &aclLogging); err != nil {
			return fmt.Errorf("failed to refresh default deny ACLs sampling for namespace %s: %w", ns, err)
		}
		return nil
	})
}

// resyncMulticastSampling re-applies the sampling config on multicast ACLs. The cluster-scoped
// multicast ACLs are namespace-independent, so they are refreshed via syncDefaultMulticastPolicies
// only when the whole feature is in scope (nil namespaces). Per-namespace multicast allow ACLs are
// refreshed by re-running createMulticastAllowPolicy for each multicast-enabled namespace in scope.
func (oc *DefaultNetworkController) resyncMulticastSampling(namespaces sets.Set[string]) error {
	if !oc.multicastSupport {
		return nil
	}
	var errs []error
	// Cluster-wide multicast ACLs only need refreshing when the feature's cluster-wide collectors
	// changed, which is signalled by a nil namespaces set.
	if namespaces == nil {
		if err := oc.syncDefaultMulticastPolicies(); err != nil {
			errs = append(errs, fmt.Errorf("failed to resync default multicast policies: %w", err))
		}
	}
	// Determine which namespaces to refresh: every tracked namespace when nil, else the given set.
	var targets []string
	if namespaces == nil {
		oc.namespacesMutex.Lock()
		targets = make([]string, 0, len(oc.namespaces))
		for ns := range oc.namespaces {
			targets = append(targets, ns)
		}
		oc.namespacesMutex.Unlock()
	} else {
		targets = namespaces.UnsortedList()
	}
	for _, ns := range targets {
		if err := oc.refreshMulticastNamespaceSampling(ns); err != nil {
			errs = append(errs, err)
		}
	}
	return utilerrors.Join(errs...)
}

// refreshMulticastNamespaceSampling re-applies the sampling config on a namespace's multicast allow
// ACLs, if multicast is enabled for it. The nsInfo write lock is held while re-running
// createMulticastAllowPolicy, matching the normal multicastUpdateNamespace path.
func (oc *DefaultNetworkController) refreshMulticastNamespaceSampling(ns string) error {
	nsInfo, unlock := oc.getNamespaceLocked(ns, false)
	if nsInfo == nil {
		// Namespace was deleted or is not tracked; nothing to resync.
		return nil
	}
	defer unlock()
	if !nsInfo.multicastEnabled {
		return nil
	}
	if err := oc.createMulticastAllowPolicy(ns, nsInfo); err != nil {
		return fmt.Errorf("failed to resync multicast policy for namespace %s: %w", ns, err)
	}
	return nil
}

// resyncUDNIsolationSampling re-applies the sampling config on the UDN isolation ACLs. These ACLs
// are cluster-scoped (a single "SecondaryPods" port group, keyed by ACL name/direction with no
// namespace), so the namespaces argument is irrelevant and this resyncs once. setupUDNACLs needs
// the management port IPs to rebuild the ARP/allow-host matches; they are cached from the node
// management-port reconcile (see syncNodeManagementPortDefault). If no node has been synced yet the
// isolation ACLs do not exist, so there is nothing to resync.
func (oc *DefaultNetworkController) resyncUDNIsolationSampling() error {
	mgmtPortIPs := oc.getUDNMgmtPortIPs()
	if len(mgmtPortIPs) == 0 {
		return nil
	}
	return oc.setupUDNACLs(mgmtPortIPs)
}

// setUDNMgmtPortIPs caches the management port IPs used to build the UDN isolation ACLs so the
// observability resync handler can re-apply them later.
func (oc *DefaultNetworkController) setUDNMgmtPortIPs(mgmtPortIPs []net.IP) {
	oc.udnMgmtPortIPsMutex.Lock()
	defer oc.udnMgmtPortIPsMutex.Unlock()
	oc.udnMgmtPortIPs = mgmtPortIPs
}

// getUDNMgmtPortIPs returns the cached management port IPs, or nil if no node has been synced yet.
func (oc *DefaultNetworkController) getUDNMgmtPortIPs() []net.IP {
	oc.udnMgmtPortIPsMutex.Lock()
	defer oc.udnMgmtPortIPsMutex.Unlock()
	return oc.udnMgmtPortIPs
}
