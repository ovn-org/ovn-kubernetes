package status_manager

import (
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/clustermanager/status_manager/zone_tracker"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/controller"
	adminpolicybasedrouteapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1"
	egressfirewallapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1"
	egressqosapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressqos/v1"
	networkqosapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
)

// clusterManagerName should be different from any zone name
const clusterManagerName = "cluster-manager"

type resourceManager[T any] interface {
	// cluster-scoped resources should ignore namespace
	get(namespace, name string) (*T, error)
	// messages should be built using types.GetZoneStatus, zone will be extracted by types.GetZoneFromStatus
	getMessages(*T) []string
	// updateStatus should update obj status using given applyOpts. If applyEmptyOrFailed is true, we can't be sure about
	// success, but can be sure about failure. So if at least 1 message is a failure, we can apply failed status, but if
	// all messages are successful, empty patch should be sent.
	// updateStatus should handle nil in case of pointer type.
	updateStatus(obj *T, applyOpts *metav1.ApplyOptions, applyEmptyOrFailed bool) error
	// cleanupStatus should send empty update with given applyOpts.
	cleanupStatus(obj *T, applyOpts *metav1.ApplyOptions) error
}

// typedStatusManager manages status for a resource of type T.
type typedStatusManager[T any] struct {
	name string

	objController  controller.Controller
	resource       resourceManager[T]
	withZonesRLock func(f func(zones sets.Set[string]) error) error
}

func newStatusManager[T any](name string, informer cache.SharedIndexInformer,
	lister func(selector labels.Selector) (ret []*T, err error), resource resourceManager[T],
	withZonesRLock func(f func(zones sets.Set[string]) error) error) *typedStatusManager[T] {
	m := &typedStatusManager[T]{
		name:           name,
		resource:       resource,
		withZonesRLock: withZonesRLock,
	}
	controllerConfig := &controller.ControllerConfig[T]{
		Informer:       informer,
		Lister:         lister,
		RateLimiter:    workqueue.NewTypedItemFastSlowRateLimiter[string](time.Second, 5*time.Second, 5),
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
	return controller.Start(m.objController)
}

func (m *typedStatusManager[T]) Stop() {
	controller.Stop(m.objController)
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
		if zones.Has(zone_tracker.UnknownZone) || zones.Len() == 0 {
			// zones are not in a consistent state, wait for more information to be populated
			return nil
		}

		messages := m.resource.getMessages(obj)
		if len(messages) == 0 {
			return nil
		}

		// first, make sure no stale zones are present.
		// every zone has exactly 1 status message
		if len(messages) > zones.Len() {
			for _, message := range messages {
				if zoneID := types.GetZoneFromStatus(message); !zones.Has(zoneID) {
					// stale zone, remove
					// use zoneID as fieldManager to reset fields owned by that zone
					applyAsZoneController := &metav1.ApplyOptions{
						Force:        true,
						FieldManager: zoneID,
					}
					klog.Infof("StatusManager %s: delete stale zone %s", m.name, zoneID)
					err = m.resource.cleanupStatus(obj, applyAsZoneController)
					if err != nil {
						return err
					}
				}
			}
		}

		// now calculate accumulated status.
		// if not all zones reported status, clean it up, since the status is considered unknown until all zone report results.
		applyAsStatusManager := &metav1.ApplyOptions{
			Force:        true,
			FieldManager: clusterManagerName,
		}
		applyEmptyOrFailed := len(messages) < zones.Len()
		return m.resource.updateStatus(obj, applyAsStatusManager, applyEmptyOrFailed)
	})
}

func (m *typedStatusManager[T]) ReconcileAll() {
	m.objController.ReconcileAll()
}

// StatusManager updates cumulative status for different object types based on features enabled in config
type StatusManager struct {
	typedManagers map[string]resourceReconciler
	zonesLock     sync.RWMutex
	zones         sets.Set[string]
	zoneTracker   *zone_tracker.ZoneTracker
	ovnClient     *util.OVNClusterManagerClientset
}

type resourceReconciler interface {
	Start() error
	Stop()
	ReconcileAll()
}

func NewStatusManager(wf *factory.WatchFactory, ovnClient *util.OVNClusterManagerClientset) *StatusManager {
	sm := &StatusManager{
		typedManagers: map[string]resourceReconciler{},
		zonesLock:     sync.RWMutex{},
		zones:         sets.New[string](),
		ovnClient:     ovnClient,
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
			newEgressFirewallManager(wf.EgressFirewallInformer().Lister(), ovnClient.EgressFirewallClient),
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
	return nil
}

func (sm *StatusManager) Stop() {
	sm.zoneTracker.Stop()
	for _, manager := range sm.typedManagers {
		manager.Stop()
	}
	sm.typedManagers = map[string]resourceReconciler{}
}

func (sm *StatusManager) onZoneUpdate(newZones sets.Set[string]) {
	klog.Infof("StatusManager got zones update: %v", newZones)
	sm.zonesLock.Lock()
	deletedZones := sm.zones.Difference(newZones)
	sm.zones = newZones
	sm.zonesLock.Unlock()

	if newZones.Has(zone_tracker.UnknownZone) {
		// no need to trigger full reconcile, since it can't figure out a final status without knowing all zones
		return
	}
	for _, typedManager := range sm.typedManagers {
		typedManager.ReconcileAll()
	}
	deletedZones.Delete(zone_tracker.UnknownZone) // delete the unknown zone, but proceed to cleanup for other known zones if any
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
