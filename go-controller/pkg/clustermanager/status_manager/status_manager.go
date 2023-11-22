package status_manager

import (
	"fmt"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/controller"
	adminpolicybasedrouteapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
)

// clusterManagerName should be different from any zone name
const clusterManagerName = "cluster-manager"

type resourceManager[T any] interface {
	// cluster-scoped resources should ignore namespace
	get(namespace, name string) (*T, error)
	// statusChanged is used to decide if an event should trigger status update
	statusChanged(oldObj, newObj *T) bool
	// updateStatus should handle nil in case of pointer type, and return (patchBytes, needsUpdate).
	updateStatus(obj *T, applyOpts *metav1.ApplyOptions) error
}

type typedStatusManager[T any] struct {
	name string

	objController controller.Controller
	resource      resourceManager[T]
}

func newStatusManager[T any](name string, informer cache.SharedIndexInformer,
	lister func(selector labels.Selector) (ret []*T, err error), resource resourceManager[T]) *typedStatusManager[T] {
	m := &typedStatusManager[T]{
		name:     name,
		resource: resource,
	}

	controllerConfig := &controller.Config[T]{
		Informer:       informer,
		Lister:         lister,
		RateLimiter:    workqueue.NewItemFastSlowRateLimiter(time.Second, 5*time.Second, 5),
		ObjNeedsUpdate: m.needsUpdate,
		Reconcile:      m.updateStatus,
	}
	m.objController = controller.NewController[T](name, controllerConfig)
	return m
}

func (m *typedStatusManager[T]) Start() error {
	return m.objController.Start(1)
}

func (m *typedStatusManager[T]) Stop() {
	m.objController.Stop()
}

func (m *typedStatusManager[T]) needsUpdate(oldObj, newObj *T) bool {
	if oldObj == nil || newObj == nil {
		return true
	}
	return m.resource.statusChanged(oldObj, newObj)
}

func (m *typedStatusManager[T]) updateStatus(key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		klog.Errorf("Failed to split meta namespace cache key %s: %v", key, err)
		return nil
	}
	obj, err := m.resource.get(namespace, name)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("expecting %T but received %T", *new(T), obj)
	}

	if obj == nil {
		// obj was deleted, nothing to do
		return nil
	}

	applyOptions := &metav1.ApplyOptions{
		Force:        true,
		FieldManager: clusterManagerName,
	}
	return m.resource.updateStatus(obj, applyOptions)
}

type StatusManager struct {
	typedManagers map[string]startStoppable
}

type startStoppable interface {
	Start() error
	Stop()
}

func NewStatusManager(wf *factory.WatchFactory, ovnClient *util.OVNClusterManagerClientset) *StatusManager {
	sm := &StatusManager{
		typedManagers: map[string]startStoppable{},
	}
	if config.OVNKubernetesFeature.EnableMultiExternalGateway {
		apbRouteManager := newStatusManager[adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute](
			"adminpolicybasedexternalroutes_statusmanager",
			wf.APBRouteInformer().Informer(),
			wf.APBRouteInformer().Lister().List,
			newAPBRouteManager(wf.APBRouteInformer().Lister(), ovnClient.AdminPolicyRouteClient),
		)
		sm.typedManagers["adminpolicybasedexternalroutes"] = apbRouteManager
	}
	return sm
}

func (sm *StatusManager) Start() error {
	klog.Infof("Starting StatusManager with typed managers: %v", sm.typedManagers)
	for managerName, manager := range sm.typedManagers {
		err := manager.Start()
		if err != nil {
			return fmt.Errorf("failed to start %s: %w", managerName, err)
		}
	}
	return nil
}

func (sm *StatusManager) Stop() {
	for _, manager := range sm.typedManagers {
		manager.Stop()
	}
	sm.typedManagers = map[string]startStoppable{}
}
