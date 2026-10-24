// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package services

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	corev1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	ktypes "k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	coreinformers "k8s.io/client-go/informers/core/v1"
	discoveryinformers "k8s.io/client-go/informers/discovery/v1"
	clientset "k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	discoverylisters "k8s.io/client-go/listers/discovery/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	globalconfig "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/metrics/recorders"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	syncmap "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/syncmap"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

const (
	// maxRetries is the number of times a object will be retried before it is dropped out of the queue.
	// With the current rate-limiter in use (5ms*2^(maxRetries-1)) the following numbers represent the
	// sequence of delays between successive queuings of an object.
	//
	// 5ms, 10ms, 20ms, 40ms, 80ms, 160ms, 320ms, 640ms, 1.3s, 2.6s, 5.1s, 10.2s, 20.4s, 41s, 82s
	maxRetries = 15

	controllerName     = "ovn-lb-controller"
	nodeControllerName = "node-tracker-controller"

	scopedServiceQueueKeySeparator = "|"
)

var ErrMissingServiceLabel = fmt.Errorf("endpointSlice missing the service name label")

func scopedServiceQueueKey(networkName, serviceKey string) string {
	return networkName + scopedServiceQueueKeySeparator + serviceKey
}

func parseScopedServiceQueueKey(key string) (networkName, serviceKey string) {
	networkName, serviceKey, found := strings.Cut(key, scopedServiceQueueKeySeparator)
	if !found {
		return "", key
	}
	return networkName, serviceKey
}

// NetworkOptions configures per-network services controller behavior at registration time.
// These options preserve the existing Run(..., runRepair, useLBGroups, useTemplates)
// knobs while the controller moves toward shared multi-network ownership.
// TODO: some of these options may be consolidated/removed in the future
type NetworkOptions struct {
	// RunRepair controls whether registration performs stale OVN service cleanup before syncing.
	RunRepair bool
	// UseLBGroups reflects whether the registering network controller created LB groups.
	UseLBGroups bool
	// UseTemplates is per-network: default network may use templates, while UDNs currently disable
	// them because of https://issues.redhat.com/browse/FDP-988.
	UseTemplates bool
}

type networkState struct {
	netInfo util.NetInfo

	// repair contains per-network service repair bookkeeping.
	repair *repair

	// Local node information and template variables. The latter expand to the
	// local chassis' node IP (v4 and v6).
	// Must be accessed only with the nodeInfo mutex taken.
	nodeInfosByName   map[string]nodeInfo
	nodeInfo          *nodeInfo
	nodeIPv4Templates *NodeIPsTemplates
	nodeIPv6Templates *NodeIPsTemplates
	nodeInfoRWLock    sync.RWMutex

	// alreadyApplied is a map of service key -> already applied configuration, so we can short-circuit
	// if a service's config hasn't changed.
	alreadyApplied       map[string][]LB
	alreadyAppliedRWLock sync.RWMutex

	// Lock order considerations: if both nodeInfoRWLock and alreadyAppliedRWLock
	// need to be taken for some reason then the order in which they're taken is
	// always: first nodeInfoRWLock and then alreadyAppliedRWLock.

	// 'true' if Load_Balancer_Group is supported.
	useLBGroups bool

	// 'true' if Chassis_Template_Var is supported.
	useTemplates bool
}

func newNetworkState(netInfo util.NetInfo, svcRepair *repair, opts NetworkOptions) *networkState {
	return &networkState{
		netInfo:           netInfo,
		repair:            svcRepair,
		alreadyApplied:    map[string][]LB{},
		nodeInfosByName:   map[string]nodeInfo{},
		nodeIPv4Templates: NewNodeIPsTemplates(corev1.IPv4Protocol),
		nodeIPv6Templates: NewNodeIPsTemplates(corev1.IPv6Protocol),
		useLBGroups:       opts.UseLBGroups,
		useTemplates:      opts.UseTemplates,
	}
}

// NewController returns a new *Controller.
func NewController(client clientset.Interface,
	nbClient libovsdbclient.Client,
	serviceInformer coreinformers.ServiceInformer,
	endpointSliceInformer discoveryinformers.EndpointSliceInformer,
	nodeInformer coreinformers.NodeInformer,
	networkManager networkmanager.Interface,
	recorder record.EventRecorder,
	netInfo util.NetInfo,
	nodeName string,
) (*Controller, error) {
	klog.V(4).Infof("Creating services controller for network=%s", netInfo.GetNetworkName())
	state := newNetworkState(netInfo, newRepair(serviceInformer.Lister(), nbClient), NetworkOptions{})
	c := &Controller{
		client:   client,
		nbClient: nbClient,
		queue: workqueue.NewTypedRateLimitingQueueWithConfig(
			newRatelimiter(100),
			workqueue.TypedRateLimitingQueueConfig[string]{Name: controllerName},
		),
		workerLoopPeriod:      time.Second,
		serviceInformer:       serviceInformer,
		serviceLister:         serviceInformer.Lister(),
		endpointSliceInformer: endpointSliceInformer,
		endpointSliceLister:   endpointSliceInformer.Lister(),
		networkManager:        networkManager,

		eventRecorder: recorder,
		nodeInformer:  nodeInformer,
		nodesSynced:   nodeInformer.Informer().HasSynced,
		state:         state,
		networkStates: syncmap.NewSyncMap[*networkState](),
		nodeName:      nodeName,
	}
	c.networkStates.Store(netInfo.GetNetworkName(), state)
	return c, nil
}

// Controller manages selector-based service endpoints.
type Controller struct {
	client clientset.Interface

	// libovsdb northbound client interface
	nbClient      libovsdbclient.Client
	eventRecorder record.EventRecorder

	serviceInformer coreinformers.ServiceInformer
	// serviceLister is able to list/get services and is populated by the shared informer passed to
	serviceLister corelisters.ServiceLister

	endpointSliceInformer discoveryinformers.EndpointSliceInformer
	endpointSliceLister   discoverylisters.EndpointSliceLister

	networkManager networkmanager.Interface

	nodesSynced cache.InformerSynced

	// Services that need to be updated. A channel is inappropriate here,
	// because it allows services with lots of pods to be serviced much
	// more often than services with few pods; it also would cause a
	// service that's inserted multiple times to be processed more than
	// necessary.
	queue workqueue.TypedRateLimitingInterface[string]

	// workerLoopPeriod is the time between worker runs. The workers process the queue of service and pod changes.
	workerLoopPeriod time.Duration

	nodeInformer coreinformers.NodeInformer

	// startupDone is false up until the node, service and endpointslice initial sync
	// in Run() is completed
	startupDone     bool
	startupDoneLock sync.RWMutex

	// state is the default network state owned by this controller. It is kept
	// alongside networkStates for legacy single-network call sites and Run()
	// initialization.
	state *networkState
	// networkStates maps each registered network to its mutable service-controller state.
	networkStates *syncmap.SyncMap[*networkState]
	nodeName      string

	// handlers stored for shutdown
	nodeHandler     cache.ResourceEventHandlerRegistration
	svcHandler      cache.ResourceEventHandlerRegistration
	endpointHandler cache.ResourceEventHandlerRegistration
}

// Run will not return until stopCh is closed. workers determines how many
// endpoints will be handled in parallel.
func (c *Controller) Run(workers int, stopCh <-chan struct{}, wg *sync.WaitGroup, runRepair, useLBGroups, useTemplates bool) error {
	wg.Add(1)
	go func() {
		defer utilruntime.HandleCrash()
		defer wg.Done()
		// wait until we're told to stop
		<-stopCh

		c.Cleanup()
	}()

	c.state.useLBGroups = useLBGroups
	c.state.useTemplates = useTemplates
	klog.Infof("Starting controller %s for network=%s", controllerName, c.state.netInfo.GetNetworkName())

	var err error
	c.nodeHandler, err = c.nodeInformer.Informer().AddEventHandler(factory.WithUpdateHandlingForObjReplace(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.onNodeAdd,
		UpdateFunc: c.onNodeUpdate,
		DeleteFunc: c.onNodeDelete,
	}))
	if err != nil {
		return err
	}
	// We need node events to be synced first, as we rely on node information to properly reprogram initial per-node load balancers.
	klog.Infof("Waiting for node handler to sync for network=%s", c.state.netInfo.GetNetworkName())
	c.startupDoneLock.Lock()
	c.startupDone = false
	c.startupDoneLock.Unlock()
	if !util.WaitForHandlerSyncWithTimeout(nodeControllerName, stopCh, types.HandlerSyncTimeout, c.nodeHandler.HasSynced) {
		return fmt.Errorf("error syncing node handler")
	}

	klog.Infof("Setting up event handlers for services for network=%s", c.state.netInfo.GetNetworkName())
	c.svcHandler, err = c.serviceInformer.Informer().AddEventHandler(factory.WithUpdateHandlingForObjReplace(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.onServiceAdd,
		UpdateFunc: c.onServiceUpdate,
		DeleteFunc: c.onServiceDelete,
	}))
	if err != nil {
		return err
	}

	klog.Infof("Setting up event handlers for endpoint slices for network=%s", c.state.netInfo.GetNetworkName())
	c.endpointHandler, err = c.endpointSliceInformer.Informer().AddEventHandler(factory.WithUpdateHandlingForObjReplace(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.onEndpointSliceAdd,
		UpdateFunc: c.onEndpointSliceUpdate,
		DeleteFunc: c.onEndpointSliceDelete,
	}))
	if err != nil {
		return err
	}

	klog.Infof("Waiting for service and endpoint handlers to sync for network=%s", c.state.netInfo.GetNetworkName())
	if !util.WaitForHandlerSyncWithTimeout(controllerName, stopCh, types.HandlerSyncTimeout, c.svcHandler.HasSynced, c.endpointHandler.HasSynced) {
		return fmt.Errorf("error syncing service and endpoint handlers")
	}

	if runRepair {
		// Run the repair controller only once
		// it keeps in sync Kubernetes and OVN
		// and handles removal of stale data on upgrades
		c.state.repair.runBeforeSync(c.state.useTemplates, c.state.netInfo, c.nodeInfoMapForNetwork(c.state))
	}

	if err := c.initTopLevelCache(); err != nil {
		return fmt.Errorf("error initializing alreadyApplied cache: %w", err)
	}

	c.startupDoneLock.Lock()
	c.startupDone = true
	c.startupDoneLock.Unlock()
	if err := c.forEachNetworkState(func(_ string, state *networkState) error {
		c.enqueueAllServicesForNetwork(state)
		return nil
	}); err != nil {
		return err
	}

	// Start the workers after the repair loop to avoid races
	klog.Infof("Starting workers for network=%s", c.state.netInfo.GetNetworkName())
	for i := 0; i < workers; i++ {
		go wait.Until(c.worker, c.workerLoopPeriod, stopCh)
	}

	return nil
}

func (c *Controller) Cleanup() {
	klog.Infof("Shutting down controller %s for network=%s", controllerName, c.state.netInfo.GetNetworkName())
	c.queue.ShutDown()

	if c.nodeHandler != nil {
		if err := c.nodeInformer.Informer().RemoveEventHandler(c.nodeHandler); err != nil {
			klog.Errorf("Failed to remove node handler for network %s: %v", c.state.netInfo.GetNetworkName(), err)
		}
	}
	if c.svcHandler != nil {
		if err := c.serviceInformer.Informer().RemoveEventHandler(c.svcHandler); err != nil {
			klog.Errorf("Failed to remove service handler for network %s: %v", c.state.netInfo.GetNetworkName(), err)
		}
	}
	if c.endpointHandler != nil {
		if err := c.endpointSliceInformer.Informer().RemoveEventHandler(c.endpointHandler); err != nil {
			klog.Errorf("Failed to remove endpoint handler for network %s: %v", c.state.netInfo.GetNetworkName(), err)
		}
	}
}

// RegisterNetwork adds a network to the shared services controller and bootstraps
// its per-network state.
func (c *Controller) RegisterNetwork(netInfo util.NetInfo, opts NetworkOptions) error {
	if netInfo == nil {
		return fmt.Errorf("cannot register nil network with services controller")
	}
	networkName := netInfo.GetNetworkName()
	klog.Infof("Registering network=%s with services controller", networkName)
	return c.networkStates.DoWithLock(networkName, func(key string) error {
		if _, ok := c.networkStates.Load(key); ok {
			return fmt.Errorf("network %q is already registered with services controller", key)
		}

		return c.registerNetworkLocked(key, netInfo, opts)
	})
}

// ReconcileNetwork updates the registered network view and requeues existing services.
// This is used for level-driven network/NAD changes where Service objects themselves
// may not receive an event.
func (c *Controller) ReconcileNetwork(netInfo util.NetInfo, opts NetworkOptions) error {
	if netInfo == nil {
		return fmt.Errorf("cannot reconcile nil network with services controller")
	}
	networkName := netInfo.GetNetworkName()
	return c.networkStates.DoWithLock(networkName, func(key string) error {
		state, ok := c.networkStates.Load(key)
		if !ok {
			klog.V(4).Infof("Skipping services controller network reconcile for unregistered network=%s", key)
			return nil
		}
		nodeInfo, _, err := c.nodeInfosForNetwork(netInfo)
		if err != nil {
			return err
		}
		state.netInfo = netInfo
		state.useLBGroups = opts.UseLBGroups
		state.useTemplates = opts.UseTemplates
		c.syncNodeInfoForNetwork(state, nodeInfo)
		c.enqueueAllServicesForNetwork(state)
		return nil
	})
}

func (c *Controller) registerNetworkLocked(key string, netInfo util.NetInfo, opts NetworkOptions) error {
	state := newNetworkState(netInfo, newRepair(c.serviceLister, c.nbClient), opts)

	// Store the state before bootstrap while the network key is locked. This lets
	// concurrent shared node/service handlers discover the registering network and
	// then block on the same key until bootstrap finishes, instead of missing it.
	c.networkStates.Store(key, state)
	if err := c.bootstrapNetworkState(state, opts.RunRepair); err != nil {
		c.networkStates.Delete(key)
		return fmt.Errorf("failed to bootstrap services controller for network %s: %w", key, err)
	}

	c.enqueueAllServicesForNetwork(state)
	return nil
}

// DeregisterNetwork removes a network from the shared services controller.
func (c *Controller) DeregisterNetwork(networkName string) error {
	klog.Infof("Deregistering network=%s from services controller", networkName)
	return c.networkStates.DoWithLock(networkName, func(key string) error {
		c.networkStates.Delete(key)
		return nil
	})
}

func (c *Controller) getNetworkNames() []string {
	networkNames := c.networkStates.GetKeys()
	sort.Strings(networkNames)
	return networkNames
}

func (c *Controller) forEachNetworkState(f func(networkName string, state *networkState) error) error {
	for _, networkName := range c.getNetworkNames() {
		if err := c.networkStates.DoWithLock(networkName, func(key string) error {
			state, ok := c.networkStates.Load(key)
			if !ok {
				return nil
			}
			return f(key, state)
		}); err != nil {
			return err
		}
	}
	return nil
}

func (c *Controller) bootstrapNetworkState(state *networkState, runRepair bool) error {
	nodeInfo, nodes, err := c.nodeInfosForNetwork(state.netInfo)
	if err != nil {
		return err
	}
	c.syncNodeInfoForNetwork(state, nodeInfo)

	if runRepair {
		state.repair.runBeforeSync(state.useTemplates, state.netInfo, nodes)
	}

	if err := c.initTopLevelCacheForNetwork(state); err != nil {
		return err
	}

	return nil
}

func (c *Controller) nodeInfosForNetwork(netInfo util.NetInfo) (*nodeInfo, map[string]nodeInfo, error) {
	nodes, err := c.nodeInformer.Lister().List(labels.Everything())
	if err != nil {
		return nil, nil, err
	}

	nodeMap := make(map[string]nodeInfo, len(nodes))
	for _, node := range nodes {
		ni, err := nodeInfoForNetwork(node, netInfo)
		if err != nil || ni == nil {
			continue
		}
		nodeMap[node.Name] = *ni
	}

	return localNodeInfo(c.nodeName, nodeMap), nodeMap, nil
}

func (c *Controller) nodeInfoMapForNetwork(state *networkState) map[string]nodeInfo {
	state.nodeInfoRWLock.RLock()
	defer state.nodeInfoRWLock.RUnlock()

	nodeInfoByName := make(map[string]nodeInfo, len(state.nodeInfosByName))
	for nodeName, nodeInfo := range state.nodeInfosByName {
		nodeInfoByName[nodeName] = nodeInfo
	}
	return nodeInfoByName
}

func localNodeInfo(nodeName string, nodeInfoByName map[string]nodeInfo) *nodeInfo {
	if nodeInfo, ok := nodeInfoByName[nodeName]; ok {
		return &nodeInfo
	}
	return nil
}

func (c *Controller) enqueueAllServicesForNetwork(state *networkState) {
	c.startupDoneLock.RLock()
	defer c.startupDoneLock.RUnlock()
	if !c.startupDone {
		return
	}

	services, err := c.servicesForNetwork(state)
	if err != nil {
		klog.Errorf("Cached lister failed (network=%s)!? %v", state.netInfo.GetNetworkName(), err)
		return
	}

	for _, service := range services {
		c.enqueueServiceForNetwork(state, service)
	}
}

func (c *Controller) servicesForNetwork(state *networkState) ([]*corev1.Service, error) {
	if !util.IsNetworkSegmentationSupportEnabled() || !state.netInfo.IsPrimaryNetwork() {
		return c.serviceLister.List(labels.Everything())
	}

	namespaces := state.netInfo.GetNADNamespaces()
	services := make([]*corev1.Service, 0)
	seen := map[string]struct{}{}
	addService := func(service *corev1.Service) {
		key := ktypes.NamespacedName{Namespace: service.Namespace, Name: service.Name}.String()
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		services = append(services, service)
	}
	for _, namespace := range namespaces {
		namespaceServices, err := c.serviceLister.Services(namespace).List(labels.Everything())
		if err != nil {
			return nil, fmt.Errorf("failed to list services in namespace %s: %w", namespace, err)
		}
		for _, service := range namespaceServices {
			addService(service)
		}
	}

	if globalconfig.Gateway.Mode == globalconfig.GatewayModeShared {
		for _, serviceKey := range globalconfig.Default.UDNAllowedDefaultServices {
			namespace, name, err := cache.SplitMetaNamespaceKey(serviceKey)
			if err != nil {
				return nil, fmt.Errorf("failed to split UDN enabled service key %s: %w", serviceKey, err)
			}
			service, err := c.serviceLister.Services(namespace).Get(name)
			if apierrors.IsNotFound(err) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("failed to get UDN enabled service %s: %w", serviceKey, err)
			}
			addService(service)
		}
	}
	return services, nil
}

func (c *Controller) enqueueServiceForNetwork(state *networkState, service *corev1.Service) {
	key, err := cache.MetaNamespaceKeyFunc(service)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v for network=%s: %v", service, state.netInfo.GetNetworkName(), err))
		return
	}
	c.enqueueServiceKeyForNetwork(state, key, true)
}

func (c *Controller) enqueueServiceKeyForNetwork(state *networkState, key string, recordDuration bool) {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't split service key %s for network=%s: %v", key, state.netInfo.GetNetworkName(), err))
		return
	}
	if c.skipServiceForNetwork(state, name, namespace) {
		return
	}

	if recordDuration {
		recorders.GetConfigDurationRecorder().Start("service", namespace, name)
	}
	klog.V(5).Infof("Queueing service %s for network=%s", key, state.netInfo.GetNetworkName())
	c.queue.Add(scopedServiceQueueKey(state.netInfo.GetNetworkName(), key))
}

func (c *Controller) enqueueServiceKeyForNetworks(key string, recordDuration bool) {
	if err := c.forEachNetworkState(func(_ string, state *networkState) error {
		c.enqueueServiceKeyForNetwork(state, key, recordDuration)
		return nil
	}); err != nil {
		utilruntime.HandleError(err)
	}
}

func (c *Controller) enqueueServiceKeyForNetworkName(networkName, key string, recordDuration bool) {
	if err := c.networkStates.DoWithLock(networkName, func(lockedNetworkName string) error {
		state, ok := c.networkStates.Load(lockedNetworkName)
		if !ok {
			return nil
		}
		c.enqueueServiceKeyForNetwork(state, key, recordDuration)
		return nil
	}); err != nil {
		utilruntime.HandleError(err)
	}
}

// worker runs a worker thread that just dequeues items, processes them, and
// marks them done. You may run as many of these in parallel as you wish; the
// workqueue guarantees that they will not end up processing the same service
// at the same time.
func (c *Controller) worker() {
	for c.processNextWorkItem() {
	}
}

func (c *Controller) processNextWorkItem() bool {
	eKey, quit := c.queue.Get()
	if quit {
		return false
	}
	defer c.queue.Done(eKey)

	err := c.syncService(eKey)
	c.handleErr(err, eKey)

	return true
}

func (c *Controller) handleErr(err error, key string) {
	networkName, serviceKey := parseScopedServiceQueueKey(key)
	if networkName == "" {
		networkName = c.state.netInfo.GetNetworkName()
	}
	ns, name, keyErr := cache.SplitMetaNamespaceKey(serviceKey)
	if keyErr != nil {
		klog.ErrorS(err, "Failed to split meta namespace cache key", "key", key)
	}
	if err == nil {
		recorders.GetConfigDurationRecorder().End("service", ns, name)
		c.queue.Forget(key)
		return
	}

	metrics.MetricRequeueServiceCount.Inc()

	if c.queue.NumRequeues(key) < maxRetries {
		klog.V(2).InfoS("Error syncing service, retrying", "service", klog.KRef(ns, name), "err", err)
		c.queue.AddRateLimited(key)
		return
	}

	klog.Warningf("Dropping service %q out of the queue for network=%s: %v", serviceKey, networkName, err)
	recorders.GetConfigDurationRecorder().End("service", ns, name)
	c.queue.Forget(key)
	utilruntime.HandleError(err)
}

// initTopLevelCache will take load balancer data currently applied in OVN and populate the cache.
// An important caveat here is that no effort is made towards populating some details of LB here.
// That is because such work will be performed in syncService, so all that is needed here is the ability
// to distinguish what is present in ovn database and this 'dirty' initial value.
func (c *Controller) initTopLevelCache() error {
	return c.initTopLevelCacheForNetwork(c.state)
}

func (c *Controller) initTopLevelCacheForNetwork(state *networkState) error {
	var err error

	state.alreadyAppliedRWLock.Lock()
	defer state.alreadyAppliedRWLock.Unlock()

	// First list all the templates.
	allTemplates := TemplateMap{}

	if state.useTemplates {
		allTemplates, err = listSvcTemplates(c.nbClient)
		if err != nil {
			return fmt.Errorf("failed to load templates: %w", err)
		}
	}

	// Then list all load balancers and their respective services.
	services, lbs, err := getServiceLBsForNetwork(c.nbClient, allTemplates, state.netInfo)
	if err != nil {
		return fmt.Errorf("failed to load balancers: %w", err)
	}

	state.alreadyApplied = make(map[string][]LB, len(services))

	for _, lb := range lbs {
		service := lb.ExternalIDs[types.LoadBalancerOwnerExternalID]
		state.alreadyApplied[service] = append(state.alreadyApplied[service], *lb)
	}

	klog.Infof("Controller cache of %d load balancers initialized for %d services for network=%s",
		len(lbs), len(state.alreadyApplied), state.netInfo.GetNetworkName())

	return nil
}

// syncService ensures a given Service is correctly reflected in OVN. It does this by
// 1. Generating a high-level desired configuration
// 2. Converting the high-level configuration in to a list of exact OVN Load_Balancer objects
// 3. Reconciling those desired objects against the database.
//
// All Load_Balancer objects are tagged with their owner, so it's easy to find stale objects.
func (c *Controller) syncService(key string) error {
	networkName, serviceKey := parseScopedServiceQueueKey(key)
	if networkName == "" {
		return fmt.Errorf("service sync key %q is missing network scope", key)
	}
	return c.networkStates.DoWithLock(networkName, func(key string) error {
		state, ok := c.networkStates.Load(key)
		if !ok {
			klog.V(4).Infof("Skipping service %s sync for deregistered network=%s", serviceKey, key)
			return nil
		}
		return c.syncServiceForNetwork(state, serviceKey)
	})
}

func (c *Controller) syncServiceForNetwork(state *networkState, key string) error {
	startTime := time.Now()
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}
	klog.V(5).Infof("Processing sync for service %s/%s for network=%s", namespace, name, state.netInfo.GetNetworkName())
	metrics.MetricSyncServiceCount.Inc()

	defer func() {
		klog.V(5).Infof("Finished syncing service %s on namespace %s for network=%s : %v", name, namespace, state.netInfo.GetNetworkName(), time.Since(startTime))
		metrics.MetricSyncServiceLatency.Observe(time.Since(startTime).Seconds())
	}()

	// Shared node information (state.nodeInfo, state.nodeIPv4Template, state.nodeIPv6Template)
	// needs to be accessed with the nodeInfoRWLock taken for read.
	state.nodeInfoRWLock.RLock()
	defer state.nodeInfoRWLock.RUnlock()

	// Get current Service from the cache
	service, err := c.serviceLister.Services(namespace).Get(name)
	// It´s unlikely that we have an error different that "Not Found Object"
	// because we are getting the object from the informer´s cache
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}

	// Handle default network services enabled for UDN in shared gateway mode
	if state.netInfo.IsPrimaryNetwork() &&
		util.IsUDNEnabledService(key) {

		if service == nil {
			return c.cleanupUDNEnabledServiceRoute(state, key)
		}

		err = c.configureUDNEnabledServiceRoute(state, service)
		if err != nil {
			return fmt.Errorf("failed to configure the UDN enabled service route: %v", err)
		}
		return nil
	}

	// Delete the Service's LB(s) from OVN if:
	// - the Service was deleted from the cache (doesn't exist in Kubernetes anymore)
	// - the Service mutated to a new service Type that we don't handle (ExternalName, Headless)
	if err != nil || service == nil || !util.ServiceTypeHasClusterIP(service) || !util.IsClusterIPSet(service) {
		service = &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: namespace,
				Name:      name,
			},
		}

		state.alreadyAppliedRWLock.RLock()
		alreadyAppliedLbs, alreadyAppliedKeyExists := state.alreadyApplied[key]
		var existingLBs []LB
		if alreadyAppliedKeyExists {
			existingLBs = make([]LB, len(alreadyAppliedLbs))
			copy(existingLBs, alreadyAppliedLbs)
		}
		state.alreadyAppliedRWLock.RUnlock()

		if alreadyAppliedKeyExists {
			//
			// The controller's alreadyApplied functions as the cache for the service controller to map into OVN
			// load balancers. While EnsureLBs may be concurrently called by this controller's workers, only a single
			// worker will be operating at a given service. That is why it is safe to have changes to this cache
			// from multiple workers, because the `key` is always uniquely hashed to the same worker thread.

			if err := EnsureLBs(c.nbClient, service, existingLBs, nil, state.netInfo); err != nil {
				return fmt.Errorf("failed to delete load balancers for service %s/%s: %w",
					namespace, name, err)
			}

			state.alreadyAppliedRWLock.Lock()
			delete(state.alreadyApplied, key)
			state.alreadyAppliedRWLock.Unlock()
		}

		state.repair.serviceSynced(key)
		return nil
	}

	// The Service exists in the cache: update it in OVN
	klog.V(5).Infof("Service %s/%s retrieved from lister for network=%s: %v", service.Namespace, service.Name, state.netInfo.GetNetworkName(), service)

	endpointSlices, err := util.GetServiceEndpointSlices(namespace, service.Name, state.netInfo.GetNetworkName(), c.endpointSliceLister)
	if err != nil {
		return fmt.Errorf("service %s/%s for network=%s, %w", service.Namespace, service.Name, state.netInfo.GetNetworkName(), err)
	}

	var nodeInfos []nodeInfo
	if state.nodeInfo != nil {
		nodeInfos = []nodeInfo{*state.nodeInfo}
	}

	// Build the abstract LB configs for this service
	perNodeConfigs, templateConfigs, clusterConfigs := buildServiceLBConfigs(service, endpointSlices, nodeInfos, state.useLBGroups, state.useTemplates, state.netInfo)
	klog.V(5).Infof("Built service %s LB cluster-wide configs for network=%s: %#v", key, state.netInfo.GetNetworkName(), clusterConfigs)
	klog.V(5).Infof("Built service %s LB per-node configs for network=%s:  %#v", key, state.netInfo.GetNetworkName(), perNodeConfigs)
	klog.V(5).Infof("Built service %s LB template configs for network=%s: %#v", key, state.netInfo.GetNetworkName(), templateConfigs)

	// Convert the LB configs in to load-balancer objects
	clusterLBs := buildClusterLBs(service, clusterConfigs, nodeInfos, state.useLBGroups, state.netInfo)
	templateLBs := buildTemplateLBs(service, templateConfigs, nodeInfos, state.nodeIPv4Templates, state.nodeIPv6Templates, state.netInfo)
	perNodeLBs := buildPerNodeLBs(service, perNodeConfigs, nodeInfos, state.netInfo)
	klog.V(5).Infof("Built service %s cluster-wide LB for network=%s: %#v", key, state.netInfo.GetNetworkName(), clusterLBs)
	klog.V(5).Infof("Built service %s per-node LB for network=%s: %#v", key, state.netInfo.GetNetworkName(), perNodeLBs)
	klog.V(5).Infof("Built service %s template LB for network=%s:  %#v", key, state.netInfo.GetNetworkName(), templateLBs)
	klog.V(5).Infof("Service %s for network=%s has %d cluster-wide, %d per-node configs, %d template configs, making %d (cluster) %d (per node) and %d (template) load balancers",
		key, state.netInfo.GetNetworkName(), len(clusterConfigs), len(perNodeConfigs), len(templateConfigs),
		len(clusterLBs), len(perNodeLBs), len(templateLBs))
	lbs := append(clusterLBs, templateLBs...)
	lbs = append(lbs, perNodeLBs...)

	// Short-circuit if nothing has changed
	state.alreadyAppliedRWLock.RLock()
	alreadyAppliedLbs, alreadyAppliedKeyExists := state.alreadyApplied[key]
	var existingLBs []LB
	if alreadyAppliedKeyExists {
		existingLBs = make([]LB, len(alreadyAppliedLbs))
		copy(existingLBs, alreadyAppliedLbs)
	}
	state.alreadyAppliedRWLock.RUnlock()

	if alreadyAppliedKeyExists && LoadBalancersEqualNoUUID(existingLBs, lbs) {
		klog.V(5).Infof("Skipping no-op change for service %s for network=%s", key, state.netInfo.GetNetworkName())
	} else {
		klog.V(5).Infof("Services do not match for network=%s, existing lbs: %#v, built lbs: %#v", state.netInfo.GetNetworkName(), existingLBs, lbs)
		// Actually apply load-balancers to OVN.
		//
		// Note: this may fail if a node was deleted between listing nodes and applying.
		// If so, this will fail and we will resync.
		if err := EnsureLBs(c.nbClient, service, existingLBs, lbs, state.netInfo); err != nil {
			return fmt.Errorf("failed to ensure service %s load balancers for network=%s: %w", key, state.netInfo.GetNetworkName(), err)
		}

		state.alreadyAppliedRWLock.Lock()
		state.alreadyApplied[key] = lbs
		state.alreadyAppliedRWLock.Unlock()
	}

	state.repair.serviceSynced(key)
	return nil
}

func (c *Controller) syncNodeInfoForNetwork(state *networkState, localNodeInfo *nodeInfo) {
	nodeInfoByName := make(map[string]nodeInfo, 1)
	if localNodeInfo != nil {
		nodeInfoByName[localNodeInfo.name] = *localNodeInfo
	}
	c.syncNodeInfoMapForNetwork(state, nodeInfoByName)
}

func (c *Controller) syncNodeInfoMapForNetwork(state *networkState, nodeInfoByName map[string]nodeInfo) {
	state.nodeInfoRWLock.Lock()
	defer state.nodeInfoRWLock.Unlock()

	state.nodeInfosByName = make(map[string]nodeInfo, len(nodeInfoByName))
	for nodeName, nodeInfo := range nodeInfoByName {
		state.nodeInfosByName[nodeName] = nodeInfo
	}

	state.nodeInfo = localNodeInfo(c.nodeName, state.nodeInfosByName)
	if !state.useTemplates {
		return
	}

	// Compute the nodeIP template values.
	state.nodeIPv4Templates = NewNodeIPsTemplates(corev1.IPv4Protocol)
	state.nodeIPv6Templates = NewNodeIPsTemplates(corev1.IPv6Protocol)

	if state.nodeInfo != nil && state.nodeInfo.chassisID != "" {
		nodeInfo := state.nodeInfo
		validNodeInfo := true

		if globalconfig.IPv4Mode {
			ips, err := util.MatchIPFamily(false, nodeInfo.hostAddresses)
			if err != nil {
				klog.Warningf("Error while searching for IPv4 host addresses in %v for node[%s] for network=%s: %v",
					nodeInfo.hostAddresses, nodeInfo.name, state.netInfo.GetNetworkName(), err)
				validNodeInfo = false
			} else {
				ips = globalconfig.Gateway.NodePortAddresses.FilterHostAddresses(ips, nodeInfo.l3gatewayAddresses)
				for _, ip := range ips {
					state.nodeIPv4Templates.AddIP(nodeInfo.chassisID, ip)
				}
			}
		}

		if validNodeInfo && globalconfig.IPv6Mode {
			ips, err := util.MatchIPFamily(true, nodeInfo.hostAddresses)
			if err != nil {
				klog.Warningf("Error while searching for IPv6 host addresses in %v for node[%s] for network=%s: %v",
					nodeInfo.hostAddresses, nodeInfo.name, state.netInfo.GetNetworkName(), err)
			} else {
				ips = globalconfig.Gateway.NodePortAddresses.FilterHostAddresses(ips, nodeInfo.l3gatewayAddresses)
				for _, ip := range ips {
					state.nodeIPv6Templates.AddIP(nodeInfo.chassisID, ip)
				}
			}
		}
	}

	// Sync the nodeIP template values to the DB.
	nodeIPTemplates := []TemplateMap{
		state.nodeIPv4Templates.AsTemplateMap(),
		state.nodeIPv6Templates.AsTemplateMap(),
	}
	if err := svcCreateOrUpdateTemplateVar(c.nbClient, nodeIPTemplates); err != nil {
		klog.Errorf("Could not sync node IP templates for network=%s", state.netInfo.GetNetworkName())
		return
	}
}

// RequestFullSync re-syncs every service that currently exists
func (c *Controller) RequestFullSync(localNodeInfo *nodeInfo) {
	c.requestFullSyncForNetwork(c.state, localNodeInfo)
}

func (c *Controller) requestFullSyncForNetwork(state *networkState, localNodeInfo *nodeInfo) {
	klog.Infof("Full service sync requested for network=%s", state.netInfo.GetNetworkName())

	// Resync local node information and node IP templates.
	c.syncNodeInfoForNetwork(state, localNodeInfo)

	// Resync all services unless we're processing the initial node sync (in which case
	// the service add will happen at the next step in the services controller Run() and workers
	// aren't up yet anyway: no need to do it during node startup then)
	c.enqueueAllServicesForNetwork(state)
}

// handlers

func (c *Controller) onNodeAdd(obj interface{}) {
	node, ok := obj.(*corev1.Node)
	if !ok {
		return
	}
	if err := c.forEachNetworkState(func(_ string, state *networkState) error {
		c.updateNodeForNetwork(state, node)
		return nil
	}); err != nil {
		utilruntime.HandleError(err)
	}
}

func (c *Controller) onNodeUpdate(oldObj, newObj interface{}) {
	oldNode, ok := oldObj.(*corev1.Node)
	if !ok {
		return
	}
	newNode, ok := newObj.(*corev1.Node)
	if !ok {
		return
	}
	if oldNode.GetResourceVersion() == newNode.GetResourceVersion() || !newNode.GetDeletionTimestamp().IsZero() {
		return
	}

	if !nodeChangedForAnyNetwork(oldNode, newNode) {
		return
	}

	if err := c.forEachNetworkState(func(_ string, state *networkState) error {
		if !nodeChangedForNetwork(oldNode, newNode, state.netInfo) {
			return nil
		}
		c.updateNodeForNetwork(state, newNode)
		return nil
	}); err != nil {
		utilruntime.HandleError(err)
	}
}

func (c *Controller) onNodeDelete(obj interface{}) {
	node, ok := obj.(*corev1.Node)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			klog.Errorf("Couldn't understand non-tombstone object")
			return
		}
		node, ok = tombstone.Obj.(*corev1.Node)
		if !ok {
			klog.Errorf("Couldn't understand tombstone object")
			return
		}
	}

	if err := c.forEachNetworkState(func(_ string, state *networkState) error {
		c.removeNodeForNetwork(state, node.Name)
		return nil
	}); err != nil {
		utilruntime.HandleError(err)
	}
}

func nodeChangedForNetwork(oldNode, newNode *corev1.Node, netInfo util.NetInfo) bool {
	return util.NodeSubnetAnnotationChangedForNetwork(oldNode, newNode, netInfo.GetNetworkName()) ||
		nodeChangedForAllNetworks(oldNode, newNode)
}

func nodeChangedForAnyNetwork(oldNode, newNode *corev1.Node) bool {
	return util.NodeSubnetAnnotationChanged(oldNode, newNode) ||
		nodeChangedForAllNetworks(oldNode, newNode)
}

func nodeChangedForAllNetworks(oldNode, newNode *corev1.Node) bool {
	return util.NodeL3GatewayAnnotationChanged(oldNode, newNode) ||
		oldNode.Name != newNode.Name ||
		util.NodeHostCIDRsAnnotationChanged(oldNode, newNode) ||
		util.NoHostSubnet(oldNode) != util.NoHostSubnet(newNode)
}

func (c *Controller) updateNodeForNetwork(state *networkState, node *corev1.Node) {
	ni, err := nodeInfoForNetwork(node, state.netInfo)
	if err != nil || ni == nil {
		klog.Infof("Node %s has invalid / no HostSubnet annotations for network=%s (probably waiting on initialization), or it's a hybrid overlay node: %v",
			node.Name, state.netInfo.GetNetworkName(), err)
		c.removeNodeForNetwork(state, node.Name)
		return
	}

	nodeInfoByName, changed := c.updatedNodeInfoMapForNetwork(state, node.Name, ni)
	if !changed {
		return
	}
	klog.Infof("Node %s switch + router changed, syncing services in network %q", node.Name, state.netInfo.GetNetworkName())
	c.syncNodeInfoMapForNetwork(state, nodeInfoByName)
	c.enqueueAllServicesForNetwork(state)
}

func (c *Controller) removeNodeForNetwork(state *networkState, nodeName string) {
	nodeInfoByName, changed := c.updatedNodeInfoMapForNetwork(state, nodeName, nil)
	if !changed {
		return
	}
	c.syncNodeInfoMapForNetwork(state, nodeInfoByName)
	c.enqueueAllServicesForNetwork(state)
}

func (c *Controller) updatedNodeInfoMapForNetwork(state *networkState, nodeName string, newNodeInfo *nodeInfo) (map[string]nodeInfo, bool) {
	state.nodeInfoRWLock.RLock()
	defer state.nodeInfoRWLock.RUnlock()

	existing, ok := state.nodeInfosByName[nodeName]
	if newNodeInfo != nil && ok && reflect.DeepEqual(existing, *newNodeInfo) {
		return nil, false
	}
	if newNodeInfo == nil && !ok {
		return nil, false
	}

	nodeInfoByName := make(map[string]nodeInfo, len(state.nodeInfosByName)+1)
	for existingNodeName, existingNodeInfo := range state.nodeInfosByName {
		nodeInfoByName[existingNodeName] = existingNodeInfo
	}
	if newNodeInfo == nil {
		delete(nodeInfoByName, nodeName)
	} else {
		nodeInfoByName[nodeName] = *newNodeInfo
	}
	return nodeInfoByName, true
}

func (c *Controller) skipServiceForNetwork(state *networkState, name, namespace string) bool {
	if util.IsNetworkSegmentationSupportEnabled() {
		serviceNAD, err := c.networkManager.GetPrimaryNADForNamespace(namespace)
		if err != nil {
			// If the corresponding namespace is not found in the informer cache, then we should
			// not skip the service. The correct controller will process the service once the
			// namespace is in the informer cache.
			if apierrors.IsNotFound(err) {
				return false
			}
			// If the namespace's primary NAD state is unknown (e.g., NAD deleted during
			// network recreation), all controllers must skip. The correct controller
			// will process the service once the NAD is re-established and triggers a re-sync.
			if util.IsInvalidPrimaryNetworkError(err) {
				return true
			}
			utilruntime.HandleError(fmt.Errorf("failed to retrieve network for service %s/%s: %w",
				namespace, name, err))
			return true
		}

		serviceNetworkName := types.DefaultNetworkName
		isDefaultNetwork := serviceNAD == types.DefaultNetworkName
		if !isDefaultNetwork {
			serviceNetworkName = c.networkManager.GetNetworkNameForNADKey(serviceNAD)
			if serviceNetworkName == "" {
				return true
			}
		}

		// Do not skip default network services enabled for UDN
		if isDefaultNetwork &&
			state.netInfo.IsPrimaryNetwork() &&
			globalconfig.Gateway.Mode == globalconfig.GatewayModeShared &&
			util.IsUDNEnabledService(ktypes.NamespacedName{Namespace: namespace, Name: name}.String()) {
			return false
		}

		if serviceNetworkName != state.netInfo.GetNetworkName() {
			return true
		}
	}

	return false
}

// onServiceAdd queues the Service for processing.
func (c *Controller) onServiceAdd(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", obj, err))
		return
	}
	c.enqueueServiceKeyForNetworks(key, true)
}

// onServiceUpdate updates the Service Selector in the cache and queues the Service for processing.
func (c *Controller) onServiceUpdate(oldObj, newObj interface{}) {
	oldService := oldObj.(*corev1.Service)
	newService := newObj.(*corev1.Service)

	// don't process resync or objects that are marked for deletion
	if oldService.ResourceVersion == newService.ResourceVersion ||
		!newService.GetDeletionTimestamp().IsZero() {
		return
	}

	key, err := cache.MetaNamespaceKeyFunc(newObj)
	if err == nil {
		c.enqueueServiceKeyForNetworks(key, true)
	}
}

// onServiceDelete queues the Service for processing.
func (c *Controller) onServiceDelete(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", obj, err))
		return
	}
	c.enqueueServiceKeyForNetworks(key, true)
}

// onEndpointSliceAdd queues a sync for the relevant Service for a sync
func (c *Controller) onEndpointSliceAdd(obj interface{}) {
	endpointSlice := obj.(*discovery.EndpointSlice)
	if endpointSlice == nil {
		utilruntime.HandleError(fmt.Errorf("invalid EndpointSlice provided to onEndpointSliceAdd()"))
		return
	}
	c.enqueueServiceForEndpointSlice(endpointSlice)
}

// onEndpointSliceUpdate queues a sync for the relevant Service for a sync
func (c *Controller) onEndpointSliceUpdate(prevObj, obj interface{}) {
	prevEndpointSlice := prevObj.(*discovery.EndpointSlice)
	endpointSlice := obj.(*discovery.EndpointSlice)

	// don't process resync or objects that are marked for deletion
	if prevEndpointSlice.ResourceVersion == endpointSlice.ResourceVersion ||
		!endpointSlice.GetDeletionTimestamp().IsZero() {
		return
	}
	c.enqueueServiceForEndpointSlice(endpointSlice)
}

// onEndpointSliceDelete queues a sync for the relevant Service for a sync if the
// EndpointSlice resource version does not match the expected version in the
// endpointSliceTracker.
func (c *Controller) onEndpointSliceDelete(obj interface{}) {
	endpointSlice, ok := obj.(*discovery.EndpointSlice)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("couldn't get object from tombstone %#v", obj))
			return
		}
		endpointSlice, ok = tombstone.Obj.(*discovery.EndpointSlice)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("tombstone contained object that is not a EndpointSlice: %#v", obj))
			return
		}
	}

	if endpointSlice != nil {
		c.enqueueServiceForEndpointSlice(endpointSlice)
	}
}

// enqueueServiceForEndpointSlice attempts to queue the corresponding Service for
// the provided EndpointSlice.
func (c *Controller) enqueueServiceForEndpointSlice(endpointSlice *discovery.EndpointSlice) {
	if util.IsDefaultEndpointSlice(endpointSlice) {
		serviceNamespacedName, err := _getServiceNameFromEndpointSlice(endpointSlice, true)
		if err != nil {
			c.handleEndpointSliceServiceNameError(endpointSlice, types.DefaultNetworkName, err)
			return
		}
		c.enqueueServiceKeyForNetworkName(types.DefaultNetworkName, serviceNamespacedName.String(), false)
		return
	}

	networkName := endpointSlice.Annotations[types.UserDefinedNetworkEndpointSliceAnnotation]
	if networkName == "" {
		klog.V(5).Infof("Skipping EndpointSlice %s/%s because it is neither default nor mirrored for a network", endpointSlice.Namespace, endpointSlice.Name)
		return
	}

	serviceNamespacedName, err := _getServiceNameFromEndpointSlice(endpointSlice, false)
	if err != nil {
		c.handleEndpointSliceServiceNameError(endpointSlice, networkName, err)
		return
	}

	c.enqueueServiceKeyForNetworkName(networkName, serviceNamespacedName.String(), false)
}

func (c *Controller) handleEndpointSliceServiceNameError(endpointSlice *discovery.EndpointSlice, networkName string, err error) {
	// Do not log endpointsSlices missing service labels as errors.
	// Once the service label is eventually added, we will get this event
	// and re-process.
	if errors.Is(err, ErrMissingServiceLabel) {
		klog.V(5).Infof("network=%s, error=%s", networkName, err.Error())
	} else {
		utilruntime.HandleError(fmt.Errorf("network=%s, couldn't get key for EndpointSlice %+v: %v", networkName, endpointSlice, err))
	}
}

// GetServiceKeyFromEndpointSliceForDefaultNetwork returns a controller key for a Service but derived from
// an EndpointSlice.
// Not UDN-aware, is used for egress services
func GetServiceKeyFromEndpointSliceForDefaultNetwork(endpointSlice *discovery.EndpointSlice) (string, error) {
	var key string
	nsn, err := _getServiceNameFromEndpointSlice(endpointSlice, true)
	if err == nil {
		key = nsn.String()
	}
	return key, err
}

func (c *Controller) cleanupUDNEnabledServiceRoute(state *networkState, key string) error {
	klog.Infof("Removing UDN enabled service route for service %s in network: %s", key, state.netInfo.GetNetworkName())
	delPredicate := func(route *nbdb.LogicalRouterStaticRoute) bool {
		return route.ExternalIDs[types.NetworkExternalID] == state.netInfo.GetNetworkName() &&
			route.ExternalIDs[types.TopologyExternalID] == state.netInfo.TopologyType() &&
			route.ExternalIDs[types.UDNEnabledServiceExternalID] == key
	}

	var ops []ovsdb.Operation
	var err error
	if state.netInfo.TopologyType() == types.Layer2Topology && !globalconfig.Layer2UsesTransitRouter {
		if state.nodeInfo != nil {
			if ops, err = libovsdbops.DeleteLogicalRouterStaticRoutesWithPredicateOps(c.nbClient, ops, state.netInfo.GetNetworkScopedGWRouterName(state.nodeInfo.name), delPredicate); err != nil {
				return err
			}
		}
	} else {
		if ops, err = libovsdbops.DeleteLogicalRouterStaticRoutesWithPredicateOps(c.nbClient, ops, state.netInfo.GetNetworkScopedClusterRouterName(), delPredicate); err != nil {
			return err
		}
	}
	_, err = libovsdbops.TransactAndCheck(c.nbClient, ops)
	return err
}

func (c *Controller) configureUDNEnabledServiceRoute(state *networkState, service *corev1.Service) error {
	klog.Infof("Configuring UDN enabled service route for service %s/%s in network: %s", service.Namespace, service.Name, state.netInfo.GetNetworkName())

	extIDs := map[string]string{
		types.NetworkExternalID:           state.netInfo.GetNetworkName(),
		types.TopologyExternalID:          state.netInfo.TopologyType(),
		types.UDNEnabledServiceExternalID: ktypes.NamespacedName{Namespace: service.Namespace, Name: service.Name}.String(),
	}
	routesEqual := func(a, b *nbdb.LogicalRouterStaticRoute) bool {
		return a.IPPrefix == b.IPPrefix &&
			a.ExternalIDs[types.NetworkExternalID] == b.ExternalIDs[types.NetworkExternalID] &&
			a.ExternalIDs[types.TopologyExternalID] == b.ExternalIDs[types.TopologyExternalID] &&
			a.ExternalIDs[types.UDNEnabledServiceExternalID] == b.ExternalIDs[types.UDNEnabledServiceExternalID] &&
			libovsdbops.PolicyEqualPredicate(a.Policy, b.Policy) &&
			a.Nexthop == b.Nexthop

	}
	var ops []ovsdb.Operation
	if state.nodeInfo != nil {
		nodeInfo := state.nodeInfo
		mgmtIP, err := util.MatchFirstIPFamily(utilnet.IsIPv6String(service.Spec.ClusterIP), nodeInfo.mgmtIPs)
		if err != nil {
			return err
		}
		staticRoute := nbdb.LogicalRouterStaticRoute{
			Policy:      &nbdb.LogicalRouterStaticRoutePolicyDstIP,
			IPPrefix:    service.Spec.ClusterIP,
			Nexthop:     mgmtIP.String(),
			ExternalIDs: extIDs,
		}
		routerName := state.netInfo.GetNetworkScopedClusterRouterName()
		if state.netInfo.TopologyType() == types.Layer2Topology && !globalconfig.Layer2UsesTransitRouter {
			routerName = nodeInfo.gatewayRouterName
		}
		ops, err = libovsdbops.CreateOrUpdateLogicalRouterStaticRoutesWithPredicateOps(c.nbClient, nil, routerName, &staticRoute, func(item *nbdb.LogicalRouterStaticRoute) bool {
			return routesEqual(item, &staticRoute)
		})
		if err != nil {
			return err
		}
	}

	_, err := libovsdbops.TransactAndCheck(c.nbClient, ops)
	return err
}

func _getServiceNameFromEndpointSlice(endpointSlice *discovery.EndpointSlice, inDefaultNetwork bool) (ktypes.NamespacedName, error) {
	if endpointSlice == nil {
		return ktypes.NamespacedName{}, fmt.Errorf("nil EndpointSlice passed to _getServiceNameFromEndpointSlice()")
	}

	label := discovery.LabelServiceName
	errTemplate := ErrMissingServiceLabel
	if !inDefaultNetwork {
		label = types.LabelUserDefinedServiceName
	}

	serviceName, ok := endpointSlice.Labels[label]
	if !ok || serviceName == "" {
		return ktypes.NamespacedName{}, fmt.Errorf("%w: endpointSlice: %s/%s",
			errTemplate, endpointSlice.Namespace, endpointSlice.Name)
	}
	return ktypes.NamespacedName{Namespace: endpointSlice.Namespace, Name: serviceName}, nil
}

// newRateLimiter makes a queue rate limiter. This limits re-queues somewhat more significantly than base qps.
// the client-go default qps is 10, but this is low for our level of scale.
func newRatelimiter(qps int) workqueue.TypedRateLimiter[string] {
	return workqueue.NewTypedMaxOfRateLimiter(
		workqueue.NewTypedItemExponentialFailureRateLimiter[string](5*time.Millisecond, 1000*time.Second),
		&workqueue.TypedBucketRateLimiter[string]{Limiter: rate.NewLimiter(rate.Limit(qps), qps*5)},
	)
}
