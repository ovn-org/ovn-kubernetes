package services

import (
	"errors"
	"fmt"
	"sync"
	"time"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	ovnlb "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/loadbalancer"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"golang.org/x/time/rate"

	v1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
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
)

const (
	// maxRetries is the number of times a object will be retried before it is dropped out of the queue.
	// With the current rate-limiter in use (5ms*2^(maxRetries-1)) the following numbers represent the
	// sequence of delays between successive queuings of an object.
	//
	// 5ms, 10ms, 20ms, 40ms, 80ms, 160ms, 320ms, 640ms, 1.3s, 2.6s, 5.1s, 10.2s, 20.4s, 41s, 82s
	maxRetries = 15

	controllerName = "ovn-lb-controller"
)

var NoServiceLabelError = fmt.Errorf("endpointSlice missing %s label", discovery.LabelServiceName)

// NewController returns a new *Controller.
func NewController(client clientset.Interface,
	nbClient libovsdbclient.Client,
	serviceInformer coreinformers.ServiceInformer,
	endpointSliceInformer discoveryinformers.EndpointSliceInformer,
	nodeInformer coreinformers.NodeInformer,
	recorder record.EventRecorder,
) *Controller {
	klog.V(4).Info("Creating event broadcaster")

	c := &Controller{
		client:           client,
		nbClient:         nbClient,
		queue:            workqueue.NewNamedRateLimitingQueue(newRatelimiter(100), controllerName),
		workerLoopPeriod: time.Second,
		alreadyApplied:   map[string][]ovnlb.LB{},
	}

	// services
	klog.Info("Setting up event handlers for services")
	serviceInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.onServiceAdd,
		UpdateFunc: c.onServiceUpdate,
		DeleteFunc: c.onServiceDelete,
	})
	c.serviceLister = serviceInformer.Lister()
	c.servicesSynced = serviceInformer.Informer().HasSynced

	// endpoints slices
	klog.Info("Setting up event handlers for endpoint slices")
	endpointSliceInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.onEndpointSliceAdd,
		UpdateFunc: c.onEndpointSliceUpdate,
		DeleteFunc: c.onEndpointSliceDelete,
	})

	c.endpointSliceLister = endpointSliceInformer.Lister()
	c.endpointSlicesSynced = endpointSliceInformer.Informer().HasSynced

	c.eventRecorder = recorder

	// repair controller
	c.repair = newRepair(serviceInformer.Lister(), nbClient)

	// load balancers need to be applied to nodes, so
	// we need to watch Node objects for changes.
	c.nodeTracker = newNodeTracker(nodeInformer)
	c.nodeTracker.resyncFn = c.RequestFullSync // Need to re-sync all services when a node gains its switch or GWR
	c.nodesSynced = nodeInformer.Informer().HasSynced

	return c
}

// Controller manages selector-based service endpoints.
type Controller struct {
	client clientset.Interface

	// libovsdb northbound client interface
	nbClient      libovsdbclient.Client
	eventRecorder record.EventRecorder

	// serviceLister is able to list/get services and is populated by the shared informer passed to
	serviceLister corelisters.ServiceLister
	// servicesSynced returns true if the service shared informer has been synced at least once.
	servicesSynced cache.InformerSynced

	// endpointSliceLister is able to list/get endpoint slices and is populated
	// by the shared informer passed to NewController
	endpointSliceLister discoverylisters.EndpointSliceLister
	// endpointSlicesSynced returns true if the endpoint slice shared informer
	// has been synced at least once. Added as a member to the struct to allow
	// injection for testing.
	endpointSlicesSynced cache.InformerSynced

	nodesSynced cache.InformerSynced

	// Services that need to be updated. A channel is inappropriate here,
	// because it allows services with lots of pods to be serviced much
	// more often than services with few pods; it also would cause a
	// service that's inserted multiple times to be processed more than
	// necessary.
	queue workqueue.RateLimitingInterface

	// workerLoopPeriod is the time between worker runs. The workers process the queue of service and pod changes.
	workerLoopPeriod time.Duration

	// repair contains a controller that keeps in sync OVN and Kubernetes services
	repair *repair

	// nodeTracker
	nodeTracker *nodeTracker

	// alreadyApplied is a map of service key -> already applied configuration, so we can short-circuit
	// if a service's config hasn't changed
	alreadyApplied     map[string][]ovnlb.LB
	alreadyAppliedLock sync.Mutex

	// 'true' if Load_Balancer_Group is supported.
	useLBGroups bool
}

// Run will not return until stopCh is closed. workers determines how many
// endpoints will be handled in parallel.
func (c *Controller) Run(workers int, stopCh <-chan struct{}, runRepair, useLBGroups bool) error {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()

	klog.Infof("Starting controller %s", controllerName)
	defer klog.Infof("Shutting down controller %s", controllerName)

	c.useLBGroups = useLBGroups

	// Wait for the caches to be synced
	klog.Info("Waiting for informer caches to sync")
	if !cache.WaitForNamedCacheSync(controllerName, stopCh, c.servicesSynced, c.endpointSlicesSynced, c.nodesSynced) {
		return fmt.Errorf("error syncing cache")
	}

	if runRepair {
		// Run the repair controller only once
		// it keeps in sync Kubernetes and OVN
		// and handles removal of stale data on upgrades
		c.repair.runBeforeSync()
	}
	// Start the workers after the repair loop to avoid races
	klog.Info("Starting workers")
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

	err := c.syncService(eKey.(string))
	c.handleErr(err, eKey)

	return true
}

func (c *Controller) handleErr(err error, key interface{}) {
	ns, name, keyErr := cache.SplitMetaNamespaceKey(key.(string))
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

	klog.Warningf("Dropping service %q out of the queue: %v", key, err)
	metrics.GetConfigDurationRecorder().End("service", ns, name)
	c.queue.Forget(key)
	utilruntime.HandleError(err)
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
	klog.Infof("Processing sync for service %s/%s", namespace, name)
	metrics.MetricSyncServiceCount.Inc()

	defer func() {
		klog.V(4).Infof("Finished syncing service %s on namespace %s : %v", name, namespace, time.Since(startTime))
		metrics.MetricSyncServiceLatency.Observe(time.Since(startTime).Seconds())
	}()

	// Get current Service from the cache
	service, err := c.serviceLister.Services(namespace).Get(name)
	// It´s unlikely that we have an error different that "Not Found Object"
	// because we are getting the object from the informer´s cache
	if err != nil && !apierrors.IsNotFound(err) {
		return err
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

		if err := ovnlb.EnsureLBs(c.nbClient, service, nil); err != nil {
			return fmt.Errorf("failed to delete load balancers for service %s/%s: %w",
				namespace, name, err)
		}

		c.repair.serviceSynced(key)
		return nil
	}

	//
	// The Service exists in the cache: update it in OVN

	klog.V(5).Infof("Service %s retrieved from lister: %v", service.Name, service)

	// Get the endpoint slices associated to the Service
	esLabelSelector := labels.Set(map[string]string{
		discovery.LabelServiceName: name,
	}).AsSelectorPreValidated()
	endpointSlices, err := c.endpointSliceLister.EndpointSlices(namespace).List(esLabelSelector)
	if err != nil {
		// Since we're getting stuff from a local cache, it is basically impossible to get this error.
		c.eventRecorder.Eventf(service, v1.EventTypeWarning, "FailedToListEndpointSlices",
			"Error listing Endpoint Slices for Service %s/%s: %v", namespace, name, err)
		return err
	}

	// Build the abstract LB configs for this service
	perNodeConfigs, clusterConfigs := buildServiceLBConfigs(service, endpointSlices)
	klog.V(5).Infof("Built service %s LB cluster-wide configs %#v", key, clusterConfigs)
	klog.V(5).Infof("Built service %s LB per-node configs %#v", key, perNodeConfigs)

	// Convert the LB configs in to load-balancer objects
	nodeInfos := c.nodeTracker.allNodes()
	clusterLBs := buildClusterLBs(service, clusterConfigs, nodeInfos, c.useLBGroups)
	perNodeLBs := buildPerNodeLBs(service, perNodeConfigs, nodeInfos)
	klog.V(5).Infof("Built service %s cluster-wide LB %#v", key, clusterLBs)
	klog.V(5).Infof("Built service %s per-node LB %#v", key, perNodeLBs)
	klog.V(3).Infof("Service %s has %d cluster-wide and %d per-node configs, making %d and %d load balancers",
		key, len(clusterConfigs), len(perNodeConfigs), len(clusterLBs), len(perNodeLBs))
	lbs := append(clusterLBs, perNodeLBs...)

	// Short-circuit if nothing has changed
	c.alreadyAppliedLock.Lock()
	existingLBs, ok := c.alreadyApplied[key]
	c.alreadyAppliedLock.Unlock()
	if ok && ovnlb.LoadBalancersEqualNoUUID(existingLBs, lbs) {
		klog.V(3).Infof("Skipping no-op change for service %s", key)
	} else {
		klog.V(5).Infof("Services do not match, existing lbs: %#v, built lbs: %#v", existingLBs, lbs)
		// Actually apply load-balancers to OVN.
		//
		// Note: this may fail if a node was deleted between listing nodes and applying.
		// If so, this will fail and we will resync.
		if err := ovnlb.EnsureLBs(c.nbClient, service, lbs); err != nil {
			return fmt.Errorf("failed to ensure service %s load balancers: %w", key, err)
		}

		c.alreadyAppliedLock.Lock()
		c.alreadyApplied[key] = lbs
		c.alreadyAppliedLock.Unlock()
	}

	if !c.repair.legacyLBsDeleted() {
		if err := deleteServiceFromLegacyLBs(c.nbClient, service); err != nil {
			klog.Warningf("Failed to delete legacy vips for service %s: %v", key)
			// Continue anyways, because once all services are synced, we'll delete
			// the legacy load balancers
		}
	}

	c.repair.serviceSynced(key)
	return nil
}

// RequestFullSync re-syncs every service that currently exists
func (c *Controller) RequestFullSync() {
	klog.Info("Full service sync requested")
	services, err := c.serviceLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("Cached lister failed!? %v", err)
		return
	}
	for _, service := range services {
		c.onServiceAdd(service)
	}
}

// handlers

// onServiceAdd queues the Service for processing.
func (c *Controller) onServiceAdd(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", obj, err))
		return
	}
	klog.V(4).Infof("Adding service %s", key)
	service := obj.(*v1.Service)
	metrics.GetConfigDurationRecorder().Start("service", service.Namespace, service.Name)
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
	klog.V(4).Infof("Deleting service %s", key)
	service := obj.(*v1.Service)
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
	key, err := ServiceControllerKey(endpointSlice)
	if err != nil {
		// Do not log endpointsSlices missing service labels as errors.
		// Once the service label is eventually added, we will get this event
		// and re-process.
		if errors.Is(err, NoServiceLabelError) {
			klog.V(5).Infof(err.Error())
		} else {
			utilruntime.HandleError(fmt.Errorf("couldn't get key for EndpointSlice %+v: %v", endpointSlice, err))
		}
		return
	}

	c.queue.Add(key)
}

// serviceControllerKey returns a controller key for a Service but derived from
// an EndpointSlice.
func ServiceControllerKey(endpointSlice *discovery.EndpointSlice) (string, error) {
	if endpointSlice == nil {
		return "", fmt.Errorf("nil EndpointSlice passed to serviceControllerKey()")
	}
	serviceName, ok := endpointSlice.Labels[discovery.LabelServiceName]
	if !ok || serviceName == "" {
		return "", fmt.Errorf("%w: endpointSlice: %s/%s", NoServiceLabelError, endpointSlice.Namespace,
			endpointSlice.Name)
	}
	return fmt.Sprintf("%s/%s", endpointSlice.Namespace, serviceName), nil
}

// newRateLimiter makes a queue rate limiter. This limits re-queues somewhat more significantly than base qps.
// the client-go default qps is 10, but this is low for our level of scale.
func newRatelimiter(qps int) workqueue.RateLimiter {
	return workqueue.NewMaxOfRateLimiter(
		workqueue.NewItemExponentialFailureRateLimiter(5*time.Millisecond, 1000*time.Second),
		&workqueue.BucketRateLimiter{Limiter: rate.NewLimiter(rate.Limit(qps), qps*5)},
	)
}
