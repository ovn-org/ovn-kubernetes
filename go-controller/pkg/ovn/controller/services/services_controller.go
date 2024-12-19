package services

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	libovsdb "github.com/ovn-org/libovsdb/ovsdb"
	"golang.org/x/time/rate"

	globalconfig "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	v1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	ktypes "k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	discoverylisters "k8s.io/client-go/listers/discovery/v1"
	utilnet "k8s.io/utils/net"

	coreinformers "k8s.io/client-go/informers/core/v1"
	discoveryinformers "k8s.io/client-go/informers/discovery/v1"
	clientset "k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"

	"k8s.io/klog/v2"
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
)

var NoServiceLabelError = fmt.Errorf("endpointSlice missing the service name label")

// NewController returns a new *Controller.
func NewController(client clientset.Interface,
	nbClient libovsdbclient.Client,
	serviceInformer coreinformers.ServiceInformer,
	endpointSliceInformer discoveryinformers.EndpointSliceInformer,
	nodeInformer coreinformers.NodeInformer,
	networkManager networkmanager.Interface,
	recorder record.EventRecorder,
	netInfo util.NetInfo,
) (*Controller, error) {
	klog.V(4).Infof("Creating services controller for network=%s", netInfo.GetNetworkName())
	c := &Controller{
		client:   client,
		nbClient: nbClient,
		queue: workqueue.NewTypedRateLimitingQueueWithConfig(
			newRatelimiter(100),
			workqueue.TypedRateLimitingQueueConfig[string]{Name: controllerName},
		),
		workerLoopPeriod:      time.Second,
		alreadyApplied:        map[string][]LB{},
		nodeIPv4Templates:     NewNodeIPsTemplates(v1.IPv4Protocol),
		nodeIPv6Templates:     NewNodeIPsTemplates(v1.IPv6Protocol),
		serviceInformer:       serviceInformer,
		serviceLister:         serviceInformer.Lister(),
		endpointSliceInformer: endpointSliceInformer,
		endpointSliceLister:   endpointSliceInformer.Lister(),
		networkManager:        networkManager,

		eventRecorder: recorder,
		repair:        newRepair(serviceInformer.Lister(), nbClient),
		nodeInformer:  nodeInformer,
		nodesSynced:   nodeInformer.Informer().HasSynced,
		netInfo:       netInfo,
	}
	zone, err := libovsdbutil.GetNBZone(c.nbClient)
	if err != nil {
		return nil, fmt.Errorf("unable to get the NB Zone : err - %w", err)
	}
	// load balancers need to be applied to nodes, so
	// we need to watch Node objects for changes.
	// Need to re-sync all services when a node gains its switch or GWR
	c.nodeTracker = newNodeTracker(zone, c.RequestFullSync, netInfo)
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

	// repair contains a controller that keeps in sync OVN and Kubernetes services
	repair *repair

	nodeInformer coreinformers.NodeInformer
	// nodeTracker
	nodeTracker *nodeTracker

	// startupDone is false up until the node tracker, service and endpointslice initial sync
	// in Run() is completed
	startupDone     bool
	startupDoneLock sync.RWMutex

	// Per node information and template variables.  The latter expand to each
	// chassis' node IP (v4 and v6).
	// Must be accessed only with the nodeInfo mutex taken.
	// These are written in RequestFullSync().
	nodeInfos         []nodeInfo
	nodeIPv4Templates *NodeIPsTemplates
	nodeIPv6Templates *NodeIPsTemplates
	nodeInfoRWLock    sync.RWMutex

	// alreadyApplied is a map of service key -> already applied configuration, so we can short-circuit
	// if a service's config hasn't changed
	alreadyApplied       map[string][]LB
	alreadyAppliedRWLock sync.RWMutex

	// Lock order considerations: if both nodeInfoRWLock and alreadyAppliedRWLock
	// need to be taken for some reason then the order in which they're taken is
	// always: first nodeInfoRWLock and then alreadyAppliedRWLock.

	// 'true' if Load_Balancer_Group is supported.
	useLBGroups bool

	// 'true' if Chassis_Template_Var is supported.
	useTemplates bool

	netInfo util.NetInfo
}

// Run will not return until stopCh is closed. workers determines how many
// endpoints will be handled in parallel.
func (c *Controller) Run(workers int, stopCh <-chan struct{}, runRepair, useLBGroups, useTemplates bool) error {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()

	c.useLBGroups = useLBGroups
	c.useTemplates = useTemplates
	klog.Infof("Starting controller %s for network=%s", controllerName, c.netInfo.GetNetworkName())
	defer klog.Infof("Shutting down controller %s for network=%s", controllerName, c.netInfo.GetNetworkName())

	nodeHandler, err := c.nodeTracker.Start(c.nodeInformer)
	if err != nil {
		return err
	}
	// We need the node tracker to be synced first, as we rely on it to properly reprogram initial per node load balancers
	klog.Infof("Waiting for node tracker handler to sync for network=%s", c.netInfo.GetNetworkName())
	c.startupDoneLock.Lock()
	c.startupDone = false
	c.startupDoneLock.Unlock()
	if !util.WaitForHandlerSyncWithTimeout(nodeControllerName, stopCh, types.HandlerSyncTimeout, nodeHandler.HasSynced) {
		return fmt.Errorf("error syncing node tracker handler")
	}

	klog.Infof("Setting up event handlers for services for network=%s", c.netInfo.GetNetworkName())
	svcHandler, err := c.serviceInformer.Informer().AddEventHandler(factory.WithUpdateHandlingForObjReplace(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.onServiceAdd,
		UpdateFunc: c.onServiceUpdate,
		DeleteFunc: c.onServiceDelete,
	}))
	if err != nil {
		return err
	}

	klog.Infof("Setting up event handlers for endpoint slices for network=%s", c.netInfo.GetNetworkName())
	endpointHandler, err := c.endpointSliceInformer.Informer().AddEventHandler(factory.WithUpdateHandlingForObjReplace(
		// Filter out endpointslices that don't belong to this network (i.e. keep only kube-generated endpointslices if
		// on default network, keep only mirrored endpointslices for this network if on UDN)
		util.GetEndpointSlicesEventHandlerForNetwork(
			cache.ResourceEventHandlerFuncs{
				AddFunc:    c.onEndpointSliceAdd,
				UpdateFunc: c.onEndpointSliceUpdate,
				DeleteFunc: c.onEndpointSliceDelete,
			},
			c.netInfo)))
	if err != nil {
		return err
	}

	klog.Infof("Waiting for service and endpoint handlers to sync for network=%s", c.netInfo.GetNetworkName())
	if !util.WaitForHandlerSyncWithTimeout(controllerName, stopCh, types.HandlerSyncTimeout, svcHandler.HasSynced, endpointHandler.HasSynced) {
		return fmt.Errorf("error syncing service and endpoint handlers")
	}

	if runRepair {
		// Run the repair controller only once
		// it keeps in sync Kubernetes and OVN
		// and handles removal of stale data on upgrades
		c.repair.runBeforeSync(c.useTemplates, c.netInfo, c.nodeTracker.nodes)
	}

	if err := c.initTopLevelCache(); err != nil {
		return fmt.Errorf("error initializing alreadyApplied cache: %w", err)
	}

	c.startupDoneLock.Lock()
	c.startupDone = true
	c.startupDoneLock.Unlock()

	// Start the workers after the repair loop to avoid races
	klog.Infof("Starting workers for network=%s", c.netInfo.GetNetworkName())
	for i := 0; i < workers; i++ {
		go wait.Until(c.worker, c.workerLoopPeriod, stopCh)
	}

	<-stopCh
	return nil
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
	ns, name, keyErr := cache.SplitMetaNamespaceKey(key)
	if keyErr != nil {
		klog.ErrorS(err, "Failed to split meta namespace cache key", "key", key)
	}
	if err == nil {
		metrics.GetConfigDurationRecorder().End("service", ns, name)
		c.queue.Forget(key)
		return
	}

	metrics.MetricRequeueServiceCount.Inc()

	if c.queue.NumRequeues(key) < maxRetries {
		klog.V(2).InfoS("Error syncing service, retrying", "service", klog.KRef(ns, name), "err", err)
		c.queue.AddRateLimited(key)
		return
	}

	klog.Warningf("Dropping service %q out of the queue for network=%s: %v", key, c.netInfo.GetNetworkName(), err)
	metrics.GetConfigDurationRecorder().End("service", ns, name)
	c.queue.Forget(key)
	utilruntime.HandleError(err)
}

// initTopLevelCache will take load balancer data currently applied in OVN and populate the cache.
// An important caveat here is that no effort is made towards populating some details of LB here.
// That is because such work will be performed in syncService, so all that is needed here is the ability
// to distinguish what is present in ovn database and this 'dirty' initial value.
func (c *Controller) initTopLevelCache() error {
	var err error

	c.alreadyAppliedRWLock.Lock()
	defer c.alreadyAppliedRWLock.Unlock()

	// First list all the templates.
	allTemplates := TemplateMap{}

	if c.useTemplates {
		allTemplates, err = listSvcTemplates(c.nbClient)
		if err != nil {
			return fmt.Errorf("failed to load templates: %w", err)
		}
	}

	// Then list all load balancers and their respective services.
	services, lbs, err := getServiceLBsForNetwork(c.nbClient, allTemplates, c.netInfo)
	if err != nil {
		return fmt.Errorf("failed to load balancers: %w", err)
	}

	c.alreadyApplied = make(map[string][]LB, len(services))

	for _, lb := range lbs {
		service := lb.ExternalIDs[types.LoadBalancerOwnerExternalID]
		c.alreadyApplied[service] = append(c.alreadyApplied[service], *lb)
	}

	klog.Infof("Controller cache of %d load balancers initialized for %d services for network=%s",
		len(lbs), len(c.alreadyApplied), c.netInfo.GetNetworkName())

	return nil
}

// syncService ensures a given Service is correctly reflected in OVN. It does this by
// 1. Generating a high-level desired configuration
// 2. Converting the high-level configuration in to a list of exact OVN Load_Balancer objects
// 3. Reconciling those desired objects against the database.
//
// All Load_Balancer objects are tagged with their owner, so it's easy to find stale objects.
func (c *Controller) syncService(key string) error {
	startTime := time.Now()
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}
	klog.V(5).Infof("Processing sync for service %s/%s for network=%s", namespace, name, c.netInfo.GetNetworkName())
	metrics.MetricSyncServiceCount.Inc()

	defer func() {
		klog.V(5).Infof("Finished syncing service %s on namespace %s for network=%s : %v", name, namespace, c.netInfo.GetNetworkName(), time.Since(startTime))
		metrics.MetricSyncServiceLatency.Observe(time.Since(startTime).Seconds())
	}()

	// Shared node information (c.nodeInfos, c.nodeIPv4Template, c.nodeIPv6Template)
	// needs to be accessed with the nodeInfoRWLock taken for read.
	c.nodeInfoRWLock.RLock()
	defer c.nodeInfoRWLock.RUnlock()

	// Get current Service from the cache
	service, err := c.serviceLister.Services(namespace).Get(name)
	// It´s unlikely that we have an error different that "Not Found Object"
	// because we are getting the object from the informer´s cache
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}

	// Handle default network services enabled for UDN in shared gateway mode
	if c.netInfo.IsPrimaryNetwork() &&
		util.IsUDNEnabledService(key) {

		if service == nil {
			return c.cleanupUDNEnabledServiceRoute(key)
		}

		err = c.configureUDNEnabledServiceRoute(service)
		if err != nil {
			return fmt.Errorf("failed to configure the UDN enabled service route: %v", err)
		}
		return nil
	}

	// Delete the Service's LB(s) from OVN if:
	// - the Service was deleted from the cache (doesn't exist in Kubernetes anymore)
	// - the Service mutated to a new service Type that we don't handle (ExternalName, Headless)
	if err != nil || service == nil || !util.ServiceTypeHasClusterIP(service) || !util.IsClusterIPSet(service) {
		service = &v1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: namespace,
				Name:      name,
			},
		}

		c.alreadyAppliedRWLock.RLock()
		alreadyAppliedLbs, alreadyAppliedKeyExists := c.alreadyApplied[key]
		var existingLBs []LB
		if alreadyAppliedKeyExists {
			existingLBs = make([]LB, len(alreadyAppliedLbs))
			copy(existingLBs, alreadyAppliedLbs)
		}
		c.alreadyAppliedRWLock.RUnlock()

		if alreadyAppliedKeyExists {
			//
			// The controller's alreadyApplied functions as the cache for the service controller to map into OVN
			// load balancers. While EnsureLBs may be concurrently called by this controller's workers, only a single
			// worker will be operating at a given service. That is why it is safe to have changes to this cache
			// from multiple workers, because the `key` is always uniquely hashed to the same worker thread.

			if err := EnsureLBs(c.nbClient, service, existingLBs, nil, c.netInfo); err != nil {
				return fmt.Errorf("failed to delete load balancers for service %s/%s: %w",
					namespace, name, err)
			}

			c.alreadyAppliedRWLock.Lock()
			delete(c.alreadyApplied, key)
			c.alreadyAppliedRWLock.Unlock()
		}

		c.repair.serviceSynced(key)
		return nil
	}

	// The Service exists in the cache: update it in OVN
	klog.V(5).Infof("Service %s/%s retrieved from lister for network=%s: %v", service.Namespace, service.Name, c.netInfo.GetNetworkName(), service)

	endpointSlices, err := util.GetServiceEndpointSlices(namespace, service.Name, c.netInfo.GetNetworkName(), c.endpointSliceLister)
	if err != nil {
		return fmt.Errorf("service %s/%s for network=%s, %w", service.Namespace, service.Name, c.netInfo.GetNetworkName(), err)
	}

	// Build the abstract LB configs for this service
	perNodeConfigs, templateConfigs, clusterConfigs := buildServiceLBConfigs(service, endpointSlices, c.nodeInfos, c.useLBGroups, c.useTemplates, c.netInfo.GetNetworkName())
	klog.V(5).Infof("Built service %s LB cluster-wide configs for network=%s: %#v", key, c.netInfo.GetNetworkName(), clusterConfigs)
	klog.V(5).Infof("Built service %s LB per-node configs for network=%s:  %#v", key, c.netInfo.GetNetworkName(), perNodeConfigs)
	klog.V(5).Infof("Built service %s LB template configs for network=%s: %#v", key, c.netInfo.GetNetworkName(), templateConfigs)

	// Convert the LB configs in to load-balancer objects
	clusterLBs := buildClusterLBs(service, clusterConfigs, c.nodeInfos, c.useLBGroups, c.netInfo)
	templateLBs := buildTemplateLBs(service, templateConfigs, c.nodeInfos, c.nodeIPv4Templates, c.nodeIPv6Templates, c.netInfo)
	perNodeLBs := buildPerNodeLBs(service, perNodeConfigs, c.nodeInfos, c.netInfo)
	klog.V(5).Infof("Built service %s cluster-wide LB for network=%s: %#v", key, c.netInfo.GetNetworkName(), clusterLBs)
	klog.V(5).Infof("Built service %s per-node LB for network=%s: %#v", key, c.netInfo.GetNetworkName(), perNodeLBs)
	klog.V(5).Infof("Built service %s template LB for network=%s:  %#v", key, c.netInfo.GetNetworkName(), templateLBs)
	klog.V(5).Infof("Service %s for network=%s has %d cluster-wide, %d per-node configs, %d template configs, making %d (cluster) %d (per node) and %d (template) load balancers",
		key, c.netInfo.GetNetworkName(), len(clusterConfigs), len(perNodeConfigs), len(templateConfigs),
		len(clusterLBs), len(perNodeLBs), len(templateLBs))
	lbs := append(clusterLBs, templateLBs...)
	lbs = append(lbs, perNodeLBs...)

	// Short-circuit if nothing has changed
	c.alreadyAppliedRWLock.RLock()
	alreadyAppliedLbs, alreadyAppliedKeyExists := c.alreadyApplied[key]
	var existingLBs []LB
	if alreadyAppliedKeyExists {
		existingLBs = make([]LB, len(alreadyAppliedLbs))
		copy(existingLBs, alreadyAppliedLbs)
	}
	c.alreadyAppliedRWLock.RUnlock()

	if alreadyAppliedKeyExists && LoadBalancersEqualNoUUID(existingLBs, lbs) {
		klog.V(5).Infof("Skipping no-op change for service %s for network=%s", key, c.netInfo.GetNetworkName())
	} else {
		klog.V(5).Infof("Services do not match for network=%s, existing lbs: %#v, built lbs: %#v", c.netInfo.GetNetworkName(), existingLBs, lbs)
		// Actually apply load-balancers to OVN.
		//
		// Note: this may fail if a node was deleted between listing nodes and applying.
		// If so, this will fail and we will resync.
		if err := EnsureLBs(c.nbClient, service, existingLBs, lbs, c.netInfo); err != nil {
			return fmt.Errorf("failed to ensure service %s load balancers for network=%s: %w", key, c.netInfo.GetNetworkName(), err)
		}

		c.alreadyAppliedRWLock.Lock()
		c.alreadyApplied[key] = lbs
		c.alreadyAppliedRWLock.Unlock()
	}

	c.repair.serviceSynced(key)
	return nil
}

func (c *Controller) syncNodeInfos(nodeInfos []nodeInfo) {
	c.nodeInfoRWLock.Lock()
	defer c.nodeInfoRWLock.Unlock()

	c.nodeInfos = nodeInfos
	if !c.useTemplates {
		return
	}

	// Compute the nodeIP template values.
	c.nodeIPv4Templates = NewNodeIPsTemplates(v1.IPv4Protocol)
	c.nodeIPv6Templates = NewNodeIPsTemplates(v1.IPv6Protocol)

	for _, nodeInfo := range c.nodeInfos {
		if nodeInfo.chassisID == "" {
			continue
		}

		if globalconfig.IPv4Mode {
			ips, err := util.MatchIPFamily(false, nodeInfo.hostAddresses)
			if err != nil {
				klog.Warningf("Error while searching for IPv4 host addresses in %v for node[%s] for network=%s: %v",
					nodeInfo.hostAddresses, nodeInfo.name, c.netInfo.GetNetworkName(), err)
				continue
			}

			for _, ip := range ips {
				c.nodeIPv4Templates.AddIP(nodeInfo.chassisID, ip)
			}
		}

		if globalconfig.IPv6Mode {
			ips, err := util.MatchIPFamily(true, nodeInfo.hostAddresses)
			if err != nil {
				klog.Warningf("Error while searching for IPv6 host addresses in %v for node[%s] for network=%s: %v",
					nodeInfo.hostAddresses, nodeInfo.name, c.netInfo.GetNetworkName(), err)
				continue
			}

			for _, ip := range ips {
				c.nodeIPv6Templates.AddIP(nodeInfo.chassisID, ip)
			}
		}
	}

	// Sync the nodeIP template values to the DB.
	nodeIPTemplates := []TemplateMap{
		c.nodeIPv4Templates.AsTemplateMap(),
		c.nodeIPv6Templates.AsTemplateMap(),
	}
	if err := svcCreateOrUpdateTemplateVar(c.nbClient, nodeIPTemplates); err != nil {
		klog.Errorf("Could not sync node IP templates for network=%s", c.netInfo.GetNetworkName())
		return
	}
}

// RequestFullSync re-syncs every service that currently exists
func (c *Controller) RequestFullSync(nodeInfos []nodeInfo) {
	klog.Infof("Full service sync requested for network=%s", c.netInfo.GetNetworkName())

	// Resync node infos and node IP templates.
	c.syncNodeInfos(nodeInfos)

	// Resync all services unless we're processing the initial node tracker sync (in which case
	// the service add will happen at the next step in the services controller Run() and workers
	// aren't up yet anyway: no need to do it during node tracker startup then)
	c.startupDoneLock.RLock()
	defer c.startupDoneLock.RUnlock()
	if c.startupDone {
		services, err := c.serviceLister.List(labels.Everything())
		if err != nil {
			klog.Errorf("Cached lister failed (network=%s)!? %v", c.netInfo.GetNetworkName(), err)
			return
		}

		for _, service := range services {
			c.onServiceAdd(service)
		}
	}
}

// handlers

// skipService is used when UDN is enabled to know which services are to be skipped because they don't
// belong to the network that this service controller is responsible for.
func (c *Controller) skipService(name, namespace string) bool {
	if util.IsNetworkSegmentationSupportEnabled() {
		serviceNetwork, err := c.networkManager.GetActiveNetworkForNamespace(namespace)
		if err != nil {
			utilruntime.HandleError(fmt.Errorf("failed to retrieve network for service %s/%s: %w",
				namespace, name, err))
			return true
		}

		// Do not skip default network services enabled for UDN
		if serviceNetwork.IsDefault() &&
			c.netInfo.IsPrimaryNetwork() &&
			globalconfig.Gateway.Mode == globalconfig.GatewayModeShared &&
			util.IsUDNEnabledService(ktypes.NamespacedName{Namespace: namespace, Name: name}.String()) {
			return false
		}

		if serviceNetwork.GetNetworkName() != c.netInfo.GetNetworkName() {
			return true
		}
	}

	return false
}

// onServiceAdd queues the Service for processing.
func (c *Controller) onServiceAdd(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v for network=%s: %v", obj, c.netInfo.GetNetworkName(), err))
		return
	}

	service := obj.(*v1.Service)
	if c.skipService(service.Name, service.Namespace) {
		return
	}
	metrics.GetConfigDurationRecorder().Start("service", service.Namespace, service.Name)
	klog.V(5).Infof("Adding service %s for network=%s", key, c.netInfo.GetNetworkName())
	c.queue.Add(key)
}

// onServiceUpdate updates the Service Selector in the cache and queues the Service for processing.
func (c *Controller) onServiceUpdate(oldObj, newObj interface{}) {
	oldService := oldObj.(*v1.Service)
	newService := newObj.(*v1.Service)

	// don't process resync or objects that are marked for deletion
	if oldService.ResourceVersion == newService.ResourceVersion ||
		!newService.GetDeletionTimestamp().IsZero() {
		return
	}

	key, err := cache.MetaNamespaceKeyFunc(newObj)
	if err == nil {
		if c.skipService(newService.Name, newService.Namespace) {
			return
		}

		metrics.GetConfigDurationRecorder().Start("service", newService.Namespace, newService.Name)
		c.queue.Add(key)
	}
}

// onServiceDelete queues the Service for processing.
func (c *Controller) onServiceDelete(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", obj, err))
		return
	}
	service := obj.(*v1.Service)

	if c.skipService(service.Name, service.Namespace) {
		return
	}

	klog.V(4).Infof("Deleting service %s for network=%s", key, c.netInfo.GetNetworkName())

	metrics.GetConfigDurationRecorder().Start("service", service.Namespace, service.Name)
	c.queue.Add(key)
}

// onEndpointSliceAdd queues a sync for the relevant Service for a sync
func (c *Controller) onEndpointSliceAdd(obj interface{}) {
	endpointSlice := obj.(*discovery.EndpointSlice)
	if endpointSlice == nil {
		utilruntime.HandleError(fmt.Errorf("invalid EndpointSlice provided to onEndpointSliceAdd()"))
		return
	}
	c.queueServiceForEndpointSlice(endpointSlice)
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
	c.queueServiceForEndpointSlice(endpointSlice)
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
		c.queueServiceForEndpointSlice(endpointSlice)
	}
}

// queueServiceForEndpointSlice attempts to queue the corresponding Service for
// the provided EndpointSlice.
func (c *Controller) queueServiceForEndpointSlice(endpointSlice *discovery.EndpointSlice) {
	serviceNamespacedName, err := c.getServiceNamespacedNameFromEndpointSlice(endpointSlice)
	if err != nil {
		// Do not log endpointsSlices missing service labels as errors.
		// Once the service label is eventually added, we will get this event
		// and re-process.
		if errors.Is(err, NoServiceLabelError) {
			klog.V(5).Infof("network=%s, error=%s", c.netInfo.GetNetworkName(), err.Error())
		} else {
			utilruntime.HandleError(fmt.Errorf("network=%s, couldn't get key for EndpointSlice %+v: %v", c.netInfo.GetNetworkName(), endpointSlice, err))
		}
		return
	}

	if c.skipService(serviceNamespacedName.Name, serviceNamespacedName.Namespace) {
		return
	}
	c.queue.Add(serviceNamespacedName.String())
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

func (c *Controller) getServiceNamespacedNameFromEndpointSlice(endpointSlice *discovery.EndpointSlice) (ktypes.NamespacedName, error) {
	if c.netInfo.IsDefault() {
		return _getServiceNameFromEndpointSlice(endpointSlice, true)
	} else {
		return _getServiceNameFromEndpointSlice(endpointSlice, false)
	}
}

func (c *Controller) cleanupUDNEnabledServiceRoute(key string) error {
	klog.Infof("Removing UDN enabled service route for service %s in network: %s", key, c.netInfo.GetNetworkName())
	delPredicate := func(route *nbdb.LogicalRouterStaticRoute) bool {
		return route.ExternalIDs[types.NetworkExternalID] == c.netInfo.GetNetworkName() &&
			route.ExternalIDs[types.TopologyExternalID] == c.netInfo.TopologyType() &&
			route.ExternalIDs[types.UDNEnabledServiceExternalID] == key
	}

	var ops []libovsdb.Operation
	var err error
	if c.netInfo.TopologyType() == types.Layer2Topology {
		for _, node := range c.nodeInfos {
			if ops, err = libovsdbops.DeleteLogicalRouterStaticRoutesWithPredicateOps(c.nbClient, ops, c.netInfo.GetNetworkScopedGWRouterName(node.name), delPredicate); err != nil {
				return err
			}
		}
	} else {
		if ops, err = libovsdbops.DeleteLogicalRouterStaticRoutesWithPredicateOps(c.nbClient, ops, c.netInfo.GetNetworkScopedClusterRouterName(), delPredicate); err != nil {
			return err
		}
	}
	_, err = libovsdbops.TransactAndCheck(c.nbClient, ops)
	return err
}

func (c *Controller) configureUDNEnabledServiceRoute(service *v1.Service) error {
	klog.Infof("Configuring UDN enabled service route for service %s/%s in network: %s", service.Namespace, service.Name, c.netInfo.GetNetworkName())

	extIDs := map[string]string{
		types.NetworkExternalID:           c.netInfo.GetNetworkName(),
		types.TopologyExternalID:          c.netInfo.TopologyType(),
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
	var ops []libovsdb.Operation
	for _, nodeInfo := range c.nodeInfos {
		var mgmtPortIPs []net.IP
		for _, subnet := range nodeInfo.podSubnets {
			mgmtPortIPs = append(mgmtPortIPs, util.GetNodeManagementIfAddr(&subnet).IP)
		}
		mgmtIP, err := util.MatchFirstIPFamily(utilnet.IsIPv6String(service.Spec.ClusterIP), mgmtPortIPs)
		if err != nil {
			return err
		}
		staticRoute := nbdb.LogicalRouterStaticRoute{
			Policy:      &nbdb.LogicalRouterStaticRoutePolicyDstIP,
			IPPrefix:    service.Spec.ClusterIP,
			Nexthop:     mgmtIP.String(),
			ExternalIDs: extIDs,
		}
		routerName := c.netInfo.GetNetworkScopedClusterRouterName()
		if c.netInfo.TopologyType() == types.Layer2Topology {
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
	errTemplate := NoServiceLabelError
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
