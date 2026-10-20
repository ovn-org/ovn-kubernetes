// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

// implements NetworkQoS for all networks on a node. The address set manager
// watches pods and namespaces for it.
package networkqos

import (
	"fmt"
	"strings"
	"sync"

	nadinformers "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/informers/externalversions/k8s.cni.cncf.io/v1"
	nadlisters "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/listers/k8s.cni.cncf.io/v1"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	coreinformers "k8s.io/client-go/informers/core/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"

	libovsdbcache "github.com/ovn-kubernetes/libovsdb/cache"
	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/model"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	networkqosapi "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1alpha1"
	networkqosclientset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1alpha1/apis/clientset/versioned"
	networkqosinformer "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1alpha1/apis/informers/externalversions/networkqos/v1alpha1"
	networkqoslister "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1alpha1/apis/listers/networkqos/v1alpha1"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/addresssetmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/syncmap"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
)

// Controller applies NetworkQoS to all networks on a node.
type Controller struct {
	nodeName string
	// cache is used to store the NetworkQoS state for each NetworkQoS key.
	cache                *syncmap.SyncMap[*nqosCacheEntry]
	nbClient             libovsdbclient.Client
	nqosClientSet        networkqosclientset.Interface
	nqosController       controller.Controller
	nqosLister           networkqoslister.NetworkQoSLister
	namespaceController  controller.Controller
	namespaceLister      corelisters.NamespaceLister
	nadLister            nadlisters.NetworkAttachmentDefinitionLister
	nadReconciler        controller.Reconciler
	nadReconcilerID      uint64
	networkManager       networkmanager.Interface
	addressSetManager    *addresssetmanager.AddressSetManager
	watchAddressSetsOnce sync.Once
	legacyAddressSets    legacyAddressSets
	switchReconciler     controller.Reconciler
	watchSwitchesOnce    sync.Once
	watchSwitchesError   error
	// Remember writes so we can remove a condition before the informer sees it.
	reportedFailures sets.Set[string]
	// Failures left unreported because another zone reported them or maxConditions was reached.
	unreportedFailures    sets.Set[string]
	reportedFailuresMutex sync.Mutex
	// Keep a count computed earlier from overwriting a newer one.
	countMutex sync.Mutex
}

// NewController creates a NetworkQoS controller. Call Start to run it and Stop to shut it
// down.
func NewController(
	nodeName string,
	nbClient libovsdbclient.Client,
	nqosClient networkqosclientset.Interface,
	nqosInformer networkqosinformer.NetworkQoSInformer,
	namespaceInformer coreinformers.NamespaceInformer,
	nadInformer nadinformers.NetworkAttachmentDefinitionInformer,
	netManager networkmanager.Interface,
	addressSetManager *addresssetmanager.AddressSetManager,
	threadiness int,
) (*Controller, error) {
	if netManager == nil {
		return nil, fmt.Errorf("network manager is required by the NetworkQoS controller")
	}
	if addressSetManager == nil {
		return nil, fmt.Errorf("address set manager is required by the NetworkQoS controller")
	}
	if threadiness < 1 {
		threadiness = 1
	}
	c := &Controller{
		nodeName:           nodeName,
		nbClient:           nbClient,
		nqosClientSet:      nqosClient,
		networkManager:     netManager,
		addressSetManager:  addressSetManager,
		nqosLister:         nqosInformer.Lister(),
		namespaceLister:    namespaceInformer.Lister(),
		cache:              syncmap.NewSyncMap[*nqosCacheEntry](),
		reportedFailures:   sets.New[string](),
		unreportedFailures: sets.New[string](),
	}
	if nadInformer != nil {
		c.nadLister = nadInformer.Lister()
	}

	c.nqosController = controller.NewController("networkqos_networkqos", c.networkQoSControllerConfig(nqosInformer, threadiness))

	c.namespaceController = controller.NewController("networkqos_namespace", &controller.ControllerConfig[corev1.Namespace]{
		Informer:       namespaceInformer.Informer(),
		Lister:         c.namespaceLister.List,
		Reconcile:      c.reconcileNamespace,
		ObjNeedsUpdate: namespaceNeedsUpdate,
		Threadiness:    threadiness,
	})

	c.nadReconciler = controller.NewReconciler("networkqos_nad", &controller.ReconcilerConfig{
		Reconcile:   c.reconcileNAD,
		Threadiness: 1,
	})

	c.switchReconciler = controller.NewReconciler("networkqos_logical_switch", &controller.ReconcilerConfig{
		Reconcile:   c.reconcileLogicalSwitch,
		Threadiness: 1,
	})

	return c, nil
}

func (c *Controller) networkQoSControllerConfig(nqosInformer networkqosinformer.NetworkQoSInformer, threadiness int) *controller.ControllerConfig[networkqosapi.NetworkQoS] {
	return &controller.ControllerConfig[networkqosapi.NetworkQoS]{
		Informer:       nqosInformer.Informer(),
		Lister:         c.nqosLister.List,
		Reconcile:      c.syncNetworkQoS,
		ObjNeedsUpdate: c.networkQoSNeedsUpdate,
		Threadiness:    threadiness,
		// Invalid specs do not retry; transient failures keep retrying
		MaxAttempts: controller.InfiniteAttempts,
	}
}

// Start registers event handlers, repairs stale state, and starts workers.
func (c *Controller) Start() (err error) {
	klog.Infof("Starting the NetworkQoS controller on node %s", c.nodeName)
	if err := c.watchLogicalSwitches(); err != nil {
		return err
	}
	c.watchAddressSetsOnce.Do(func() {
		c.addressSetManager.OnAddressSetErrorChange(c.requeueAddressSetUsers)
	})
	c.nadReconcilerID = c.networkManager.RegisterNADReconciler(c.nadReconciler)
	defer func() {
		if err != nil {
			c.networkManager.DeRegisterNADReconciler(c.nadReconcilerID)
		}
	}()
	// Register handlers before repair so no events are missed.
	// Start workers afterward so they cannot race with repair.
	return controller.StartWithInitialSync(
		c.repairNetworkQoSes,
		c.nqosController,
		c.namespaceController,
		c.nadReconciler,
		c.switchReconciler,
	)
}

func (c *Controller) Stop() {
	klog.Infof("Stopping the NetworkQoS controller on node %s", c.nodeName)
	c.networkManager.DeRegisterNADReconciler(c.nadReconcilerID)
	controller.Stop(
		c.nqosController,
		c.namespaceController,
		c.nadReconciler,
		c.switchReconciler,
	)
}

// If the logical switch has a router port, each Match of QoS rule on the switch has router MAC.
// When a router port changes, all the NetworkQoS on the switch needs to be reconciled.
const requeueAllNetworkQoS = "REQUEUE_ALL_NETWORK_QOS"

// Watch for new switches so we can restore the rules when a switch is recreated.
func (c *Controller) watchLogicalSwitches() error {
	c.watchSwitchesOnce.Do(func() {
		nbCache := c.nbClient.Cache()
		if nbCache == nil {
			// The client cache is nil until the client connects.
			c.watchSwitchesError = fmt.Errorf("the NB client is not connected, cannot watch logical switches")
			return
		}
		// Do not block the client event handler.
		nbCache.AddEventHandler(&libovsdbcache.EventHandlerFuncs{
			AddFunc: func(table string, m model.Model) {
				if ls, ok := m.(*nbdb.LogicalSwitch); ok && table == nbdb.LogicalSwitchTable {
					c.switchReconciler.Reconcile(ls.Name)
				} else if isRouterPort(m) {
					c.switchReconciler.Reconcile(requeueAllNetworkQoS)
				}
			},
			UpdateFunc: func(_ string, oldModel, newModel model.Model) {
				if routerPortChanged(oldModel, newModel) {
					c.switchReconciler.Reconcile(requeueAllNetworkQoS)
				}
			},
			DeleteFunc: func(_ string, m model.Model) {
				if isRouterPort(m) {
					c.switchReconciler.Reconcile(requeueAllNetworkQoS)
				}
			},
		})
	})
	return c.watchSwitchesError
}

func isRouterPort(m model.Model) bool {
	switch port := m.(type) {
	case *nbdb.LogicalSwitchPort:
		return port.Type == logicalSwitchPortTypeRouter
	case *nbdb.LogicalRouterPort:
		return true
	default:
		return false
	}
}

// Only the peer and the MAC of a router port change the rules.
func routerPortChanged(oldModel, newModel model.Model) bool {
	switch oldPort := oldModel.(type) {
	case *nbdb.LogicalSwitchPort:
		newPort, ok := newModel.(*nbdb.LogicalSwitchPort)
		return ok && (isRouterPort(oldPort) || isRouterPort(newPort)) &&
			(oldPort.Type != newPort.Type || oldPort.Options[libovsdbops.RouterPort] != newPort.Options[libovsdbops.RouterPort])
	case *nbdb.LogicalRouterPort:
		newPort, ok := newModel.(*nbdb.LogicalRouterPort)
		return ok && oldPort.MAC != newPort.MAC
	default:
		return false
	}
}

// List every policy, then lock its key before reading the cache.
func (c *Controller) reconcileLogicalSwitch(switchName string) error {
	nqoses, err := c.nqosLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list NetworkQoS: %w", err)
	}
	for _, nqos := range nqoses {
		key := nqosKey(nqos.Namespace, nqos.Name)
		affected := false
		_ = c.cache.DoWithLock(key, func(key string) error {
			entry, loaded := c.cache.Load(key)
			if !loaded || entry == nil {
				return nil
			}
			for _, state := range entry.networks {
				if switchName == requeueAllNetworkQoS || state.switchName == switchName {
					affected = true
					break
				}
			}
			return nil
		})
		if !affected {
			continue
		}
		if switchName == requeueAllNetworkQoS {
			klog.V(4).Infof("Router ports changed, requeueing NetworkQoS %s", key)
		} else {
			klog.V(4).Infof("Logical switch %s was created, requeueing NetworkQoS %s", switchName, key)
		}
		c.nqosController.Reconcile(key)
	}
	return nil
}

// Status changes matter when this zone needs to report a failure or remove an old success
// condition. Ignore other status updates to avoid requeuing our own writes.
func (c *Controller) networkQoSNeedsUpdate(oldObj, newObj *networkqosapi.NetworkQoS) bool {
	if oldObj == nil || newObj == nil {
		return true
	}
	if oldObj.Generation != newObj.Generation || !oldObj.DeletionTimestamp.Equal(newObj.DeletionTimestamp) {
		return true
	}
	if c.failureUnreported(nqosKey(newObj.Namespace, newObj.Name)) &&
		!apiequality.Semantic.DeepEqual(oldObj.Status.Conditions, newObj.Status.Conditions) {
		return true
	}
	return oldObj.Status.Status != newObj.Status.Status && newObj.Status.Status == types.NetworkQoSNoErrorsMsg &&
		c.holdsLegacySuccess(newObj)
}

// Only the old per-network controllers wrote success conditions.
func (c *Controller) holdsLegacySuccess(nqos *networkqosapi.NetworkQoS) bool {
	condition := meta.FindStatusCondition(nqos.Status.Conditions, conditionTypeReady+c.nodeName)
	return condition != nil && condition.Status == metav1.ConditionTrue
}

func (c *Controller) requeueAddressSetUsers(backRefs []string) {
	for _, backRef := range backRefs {
		if key, isNetworkQoS := strings.CutPrefix(backRef, addrSetBackRefPrefix); isNetworkQoS {
			c.nqosController.Reconcile(key)
		}
	}
}

func namespaceNeedsUpdate(oldObj, newObj *corev1.Namespace) bool {
	if oldObj == nil || newObj == nil {
		return true
	}
	return !labels.Equals(labels.Set(oldObj.Labels), labels.Set(newObj.Labels)) ||
		!oldObj.DeletionTimestamp.Equal(newObj.DeletionTimestamp)
}
