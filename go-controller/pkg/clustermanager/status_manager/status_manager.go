// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package status_manager

import (
	"fmt"
	"reflect"
	"sync"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/clustermanager/status_manager/zone_tracker"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	adminpolicybasedrouteapi "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1"
	egressfirewallapi "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1"
	egressqosapi "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressqos/v1"
	networkqosapi "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1alpha1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// clusterManagerName should be different from any zone name
const (
	clusterManagerName = "cluster-manager"
	readyInZonePrefix  = "Ready-In-Zone-"
)

type resourceManager[T any] interface {
	// cluster-scoped resources should ignore namespace
	get(namespace, name string) (*T, error)
	// messages should be built using types.GetZoneStatus, zone will be extracted by types.GetZoneFromStatus
	getMessages(*T) []string
	// getManagedFields returns the list of managedFields entries from the object
	getManagedFields(*T) []metav1.ManagedFieldsEntry
	// updateStatus should update obj status using given applyOpts. If applyEmptyOrFailed is true, we can't be sure about
	// success, but can be sure about failure. So if at least 1 message is a failure, we can apply failed status, but if
	// all messages are successful, empty patch should be sent.
	// updateStatus should handle nil in case of pointer type.
	updateStatus(obj *T, applyOpts *metav1.ApplyOptions, applyEmptyOrFailed bool) error
	// cleanupStatus should send empty update with given applyOpts.
	cleanupStatus(obj *T, applyOpts *metav1.ApplyOptions) error
}

type relevantZoneProvider[T any] interface {
	getRelevantZones(obj *T, zones sets.Set[string]) (sets.Set[string], error)
}

// typedStatusManager manages status for a resource of type T.
type typedStatusManager[T any] struct {
	name string

	objController  controller.Controller
	resource       resourceManager[T]
	withZonesRLock func(f func(zones sets.Set[string]) error) error
	lister         func(selector labels.Selector) (ret []*T, err error)
}

func newStatusManager[T any](name string, informer cache.SharedIndexInformer,
	lister func(selector labels.Selector) (ret []*T, err error), resource resourceManager[T],
	withZonesRLock func(f func(zones sets.Set[string]) error) error) *typedStatusManager[T] {
	m := &typedStatusManager[T]{
		name:           name,
		resource:       resource,
		withZonesRLock: withZonesRLock,
		lister:         lister,
	}
	controllerConfig := &controller.ControllerConfig[T]{
		Informer:       informer,
		Lister:         lister,
		ObjNeedsUpdate: m.needsUpdate,
		Reconcile:      m.updateStatus,
		Threadiness:    1,
	}
	m.objController = controller.NewController[T](name, controllerConfig)
	return m
}

func (m *typedStatusManager[T]) needsUpdate(oldObj, newObj *T) bool {
	if oldObj == nil || newObj == nil {
		return true
	}
	return !reflect.DeepEqual(m.resource.getMessages(oldObj), m.resource.getMessages(newObj))
}

func (m *typedStatusManager[T]) Start() error {
	// Perform one-time startup cleanup of stale managedFields (upgrade scenario)
	initialSync := func() error {
		return m.withZonesRLock(func(zones sets.Set[string]) error {
			// Run cleanup immediately with whatever zones we have (even if empty).
			// The cleanup only removes empty-status managedFields from managers
			// that do not correspond to an existing zone.
			return m.doStartupCleanup(zones)
		})
	}
	return controller.StartWithInitialSync(initialSync, m.objController)
}

func (m *typedStatusManager[T]) Stop() {
	controller.Stop(m.objController)
}

// doStartupCleanup performs a one-time cleanup of stale managedFields for all objects.
// This handles the upgrade scenario where previous code left stale managedFields.
// Returns an error if any cleanup operations failed.
func (m *typedStatusManager[T]) doStartupCleanup(zones sets.Set[string]) error {
	klog.Infof("StatusManager %s: performing a one-time startup cleanup of stale managedFields", m.name)

	objects, err := m.lister(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list objects for startup cleanup: %w", err)
	}

	var cleanupErrors []error
	for _, obj := range objects {
		managedFields := m.resource.getManagedFields(obj)

		// Check for stale status managedFields
		for _, mf := range managedFields {
			// Skip cluster-manager (global aggregator)
			if mf.Manager == clusterManagerName {
				continue
			}
			if !zones.Has(mf.Manager) && mf.Subresource == "status" {
				klog.V(5).Infof("StatusManager %s: cleaning up stale status managedField for manager: %s", m.name, mf.Manager)
				applyAsZoneController := &metav1.ApplyOptions{
					Force:        true,
					FieldManager: mf.Manager,
				}
				err := m.resource.cleanupStatus(obj, applyAsZoneController)
				if err != nil {
					cleanupErrors = append(cleanupErrors, fmt.Errorf("failed to cleanup status for stale manager %s: %w", mf.Manager, err))
				}
			}
		}
	}

	if len(cleanupErrors) > 0 {
		return fmt.Errorf("startup cleanup encountered %d error(s): %v", len(cleanupErrors), cleanupErrors)
	}

	klog.Infof("StatusManager %s: startup cleanup complete", m.name)
	return nil
}

func (m *typedStatusManager[T]) updateStatus(key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		klog.Errorf("StatusManager %s: failed to split meta namespace cache key %s: %v", m.name, key, err)
		return nil
	}
	obj, err := m.resource.get(namespace, name)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("StatusManager %s: expecting %T but received %T", m.name, *new(T), obj)
	}

	if obj == nil {
		// obj was deleted, nothing to do
		return nil
	}

	return m.withZonesRLock(func(zones sets.Set[string]) error {
		if zones.Len() == 0 {
			// Wait until at least one OVN-managed node exists.
			return nil
		}

		relevantZones := zones.Clone()
		if provider, ok := any(m.resource).(relevantZoneProvider[T]); ok {
			relevantZones, err = provider.getRelevantZones(obj, zones)
			if err != nil {
				return fmt.Errorf("StatusManager %s: failed to determine relevant zones for %s/%s: %w", m.name, namespace, name, err)
			}
		}

		messages := m.resource.getMessages(obj)

		// First, make sure no stale zones are present. This includes deleted zones and
		// zones that are no longer relevant for the object's current active topology.
		staleZonesCleaned := false
		for _, message := range messages {
			if zoneID := types.GetZoneFromStatus(message); !relevantZones.Has(zoneID) {
				// stale zone, remove
				// use zoneID as fieldManager to reset fields owned by that zone
				applyAsZoneController := &metav1.ApplyOptions{
					Force:        true,
					FieldManager: zoneID,
				}
				klog.Infof("StatusManager %s: delete stale zone %s", m.name, zoneID)
				err = m.resource.cleanupStatus(obj, applyAsZoneController)
				if err != nil {
					return fmt.Errorf("StatusManager %s: failed to cleanup status for stale zone %s: %w", m.name, zoneID, err)
				}
				staleZonesCleaned = true
			}
		}
		if staleZonesCleaned {
			// The cleanup patches will trigger a new reconcile with a fresh view of per-zone
			// messages. Avoid aggregating with the stale in-memory status.
			return nil
		}

		// now calculate accumulated status.
		// if not all relevant zones reported status, clean it up, since the status is
		// considered unknown until all relevant zones report results.
		applyAsStatusManager := &metav1.ApplyOptions{
			Force:        true,
			FieldManager: clusterManagerName,
		}
		applyEmptyOrFailed := relevantZones.Len() == 0 || len(messages) < relevantZones.Len()
		return m.resource.updateStatus(obj, applyAsStatusManager, applyEmptyOrFailed)
	})
}

func (m *typedStatusManager[T]) ReconcileAll() {
	m.objController.ReconcileAll()
}

func (m *typedStatusManager[T]) cleanupAllShards() error {
	return m.doStartupCleanup(sets.New[string]())
}

// StatusManager updates cumulative status for different object types based on features enabled in config
type StatusManager struct {
	typedManagers map[string]resourceReconciler
	zonesLock     sync.RWMutex
	zones         sets.Set[string]
	zoneTracker   *zone_tracker.ZoneTracker
	ovnClient     *util.OVNClusterManagerClientset
	// metricsMode is true when EnableStatusMetrics is set: skip shard-based
	// rollup controllers and only run one-time cleanup of per-node status shards.
	metricsMode bool
}

type resourceReconciler interface {
	Start() error
	Stop()
	ReconcileAll()
	// cleanupAllShards removes every non-cluster-manager status managedFields
	// entry (used when switching to EnableStatusMetrics).
	cleanupAllShards() error
}

func NewStatusManager(wf *factory.WatchFactory, ovnClient *util.OVNClusterManagerClientset, networkManager networkmanager.Interface) *StatusManager {
	sm := &StatusManager{
		typedManagers: map[string]resourceReconciler{},
		zonesLock:     sync.RWMutex{},
		zones:         sets.New[string](),
		ovnClient:     ovnClient,
		metricsMode:   config.OVNKubernetesFeature.EnableStatusMetrics,
	}
	zoneTracker := zone_tracker.NewZoneTracker(wf.NodeCoreInformer(), sm.onZoneUpdate)
	sm.zoneTracker = zoneTracker

	if config.OVNKubernetesFeature.EnableMultiExternalGateway {
		apbRouteManager := newStatusManager[adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute](
			"adminpolicybasedexternalroutes_statusmanager",
			wf.APBRouteInformer().Informer(),
			wf.APBRouteInformer().Lister().List,
			newAPBRouteManager(wf.APBRouteInformer().Lister(), ovnClient.AdminPolicyRouteClient),
			sm.withZonesRLock,
		)
		sm.typedManagers["adminpolicybasedexternalroutes"] = apbRouteManager
	}
	if config.OVNKubernetesFeature.EnableEgressFirewall {
		egressFirewallManager := newStatusManager[egressfirewallapi.EgressFirewall](
			"egressfirewalls_statusmanager",
			wf.EgressFirewallInformer().Informer(),
			wf.EgressFirewallInformer().Lister().List,
			newEgressFirewallManager(wf.EgressFirewallInformer().Lister(), wf.NodeCoreInformer().Lister(), ovnClient.EgressFirewallClient, networkManager),
			sm.withZonesRLock,
		)
		sm.typedManagers["egressfirewalls"] = egressFirewallManager
	}
	if config.OVNKubernetesFeature.EnableEgressQoS {
		egressQoSManager := newStatusManager[egressqosapi.EgressQoS](
			"egressqoses_statusmanager",
			wf.EgressQoSInformer().Informer(),
			wf.EgressQoSInformer().Lister().List,
			newEgressQoSManager(wf.EgressQoSInformer().Lister(), ovnClient.EgressQoSClient),
			sm.withZonesRLock,
		)
		sm.typedManagers["egressqoses"] = egressQoSManager
	}
	if config.OVNKubernetesFeature.EnableNetworkQoS {
		networkQoSManager := newStatusManager[networkqosapi.NetworkQoS](
			"networkqoses_statusmanager",
			wf.NetworkQoSInformer().Informer(),
			wf.NetworkQoSInformer().Lister().List,
			newNetworkQoSManager(wf.NetworkQoSInformer().Lister(), ovnClient.NetworkQoSClient),
			sm.withZonesRLock,
		)
		sm.typedManagers["networkqoses"] = networkQoSManager
	}
	return sm
}

func (sm *StatusManager) Start() error {
	if sm.metricsMode {
		klog.Infof("StatusManager running in metrics mode: cleaning per-node status shards, skipping shard rollup")
		return sm.cleanupShardsForMetricsMode()
	}

	klog.Infof("Starting StatusManager with typed managers: %v", sm.typedManagers)
	if len(sm.typedManagers) > 0 {
		if err := sm.zoneTracker.Start(); err != nil {
			return err
		}
	}
	for managerName, manager := range sm.typedManagers {
		err := manager.Start()
		if err != nil {
			return fmt.Errorf("failed to start %s: %w", managerName, err)
		}
	}

	// Perform one-time startup cleanup for ANP/BANP managedFields
	// This handles the upgrade scenario where nodes were deleted before cluster-manager restart
	if config.OVNKubernetesFeature.EnableAdminNetworkPolicy {
		sm.zonesLock.RLock()
		zones := sm.zones.Clone()
		sm.zonesLock.RUnlock()

		anpManager := newANPManager(sm.ovnClient.ANPClient)
		if err := anpManager.doStartupCleanup(zones); err != nil {
			return fmt.Errorf("failed to run ANP/BANP startup cleanup: %w", err)
		}
	}

	return nil
}

// cleanupShardsForMetricsMode removes all per-node status managedFields shards
// so they do not linger after switching to EnableStatusMetrics.
func (sm *StatusManager) cleanupShardsForMetricsMode() error {
	for managerName, manager := range sm.typedManagers {
		if err := manager.cleanupAllShards(); err != nil {
			return fmt.Errorf("metrics-mode cleanup for %s: %w", managerName, err)
		}
	}
	if config.OVNKubernetesFeature.EnableAdminNetworkPolicy {
		anpManager := newANPManager(sm.ovnClient.ANPClient)
		if err := anpManager.doStartupCleanup(sets.New[string]()); err != nil {
			return fmt.Errorf("metrics-mode ANP/BANP cleanup: %w", err)
		}
	}
	return nil
}

func (sm *StatusManager) Stop() {
	if !sm.metricsMode {
		sm.zoneTracker.Stop()
		for _, manager := range sm.typedManagers {
			manager.Stop()
		}
	}
	sm.typedManagers = map[string]resourceReconciler{}
}

func (sm *StatusManager) onZoneUpdate(newZones sets.Set[string]) {
	klog.Infof("StatusManager got zones update: %v", newZones)
	sm.zonesLock.Lock()
	deletedZones := sm.zones.Difference(newZones)
	sm.zones = newZones
	sm.zonesLock.Unlock()

	for _, typedManager := range sm.typedManagers {
		typedManager.ReconcileAll()
	}
	if len(deletedZones) > 0 && config.OVNKubernetesFeature.EnableAdminNetworkPolicy {
		klog.Infof("Zones that got deleted are %s", deletedZones.UnsortedList())
		// there are zones that got deleted, this is expensive because we do a list from kapi server directly but
		// we don't anticipate too many node deletes in an env, it should be a rare operation which is why we are
		// not maintaining local caches for ANP/BANP
		// we must try to clean up statuses across all ANPs that were managed by that zone
		anpZoneDeleteCleanupManager := newANPManager(sm.ovnClient.ANPClient)
		anpZoneDeleteCleanupManager.cleanupDeletedZoneStatuses(deletedZones)
	}
}

func (sm *StatusManager) withZonesRLock(f func(zones sets.Set[string]) error) error {
	sm.zonesLock.RLock()
	defer sm.zonesLock.RUnlock()
	return f(sm.zones)
}
