package egressservice

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	libovsdb "github.com/ovn-org/libovsdb/ovsdb"
	egressserviceapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressservice/v1"
	egressserviceinformer "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressservice/v1/apis/informers/externalversions/egressservice/v1"
	egressservicelisters "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressservice/v1/apis/listers/egressservice/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/errors"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	coreinformers "k8s.io/client-go/informers/core/v1"
	discoveryinformers "k8s.io/client-go/informers/discovery/v1"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	discoverylisters "k8s.io/client-go/listers/discovery/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
)

const (
	maxRetries       = 10
	svcExternalIDKey = "EgressSVC" // key set on lrps to identify to which egress service it belongs
)

type InitClusterEgressPoliciesFunc func(client libovsdbclient.Client, addressSetFactory addressset.AddressSetFactory,
	controllerName string) error
type EnsureNoRerouteNodePoliciesFunc func(client libovsdbclient.Client, addressSetFactory addressset.AddressSetFactory,
	controllerName string, nodeLister corelisters.NodeLister) error
type DeleteLegacyDefaultNoRerouteNodePoliciesFunc func(libovsdbclient.Client, string) error

type Controller struct {
	controllerName string
	client         kubernetes.Interface
	nbClient       libovsdbclient.Client
	stopCh         <-chan struct{}
	sync.Mutex

	initClusterEgressPolicies                InitClusterEgressPoliciesFunc
	ensureNoRerouteNodePolicies              EnsureNoRerouteNodePoliciesFunc
	deleteLegacyDefaultNoRerouteNodePolicies DeleteLegacyDefaultNoRerouteNodePoliciesFunc

	services map[string]*svcState  // svc key -> state
	nodes    map[string]*nodeState // node name -> state, contains nodes that host an egress service

	egressServiceLister egressservicelisters.EgressServiceLister
	egressServiceSynced cache.InformerSynced
	egressServiceQueue  workqueue.RateLimitingInterface

	serviceLister  corelisters.ServiceLister
	servicesSynced cache.InformerSynced

	endpointSliceLister  discoverylisters.EndpointSliceLister
	endpointSlicesSynced cache.InformerSynced

	nodeLister  corelisters.NodeLister
	nodesSynced cache.InformerSynced
	nodesQueue  workqueue.RateLimitingInterface

	// An address set factory that creates address sets
	addressSetFactory addressset.AddressSetFactory
}

type svcState struct {
	node        string
	v4Endpoints sets.Set[string]
	v6Endpoints sets.Set[string]
	stale       bool
}

type nodeState struct {
	name     string
	draining bool
	v4MgmtIP net.IP
	v6MgmtIP net.IP
}

func NewController(
	controllerName string,
	client kubernetes.Interface,
	nbClient libovsdbclient.Client,
	addressSetFactory addressset.AddressSetFactory,
	initClusterEgressPolicies InitClusterEgressPoliciesFunc,
	ensureNoRerouteNodePolicies EnsureNoRerouteNodePoliciesFunc,
	deleteLegacyDefaultNoRerouteNodePolicies DeleteLegacyDefaultNoRerouteNodePoliciesFunc,
	stopCh <-chan struct{},
	esInformer egressserviceinformer.EgressServiceInformer,
	serviceInformer coreinformers.ServiceInformer,
	endpointSliceInformer discoveryinformers.EndpointSliceInformer,
	nodeInformer coreinformers.NodeInformer) (*Controller, error) {
	klog.Info("Setting up event handlers for Egress Services")

	c := &Controller{
		controllerName:                           controllerName,
		client:                                   client,
		nbClient:                                 nbClient,
		addressSetFactory:                        addressSetFactory,
		initClusterEgressPolicies:                initClusterEgressPolicies,
		ensureNoRerouteNodePolicies:              ensureNoRerouteNodePolicies,
		deleteLegacyDefaultNoRerouteNodePolicies: deleteLegacyDefaultNoRerouteNodePolicies,
		stopCh:                                   stopCh,
		services:                                 map[string]*svcState{},
		nodes:                                    map[string]*nodeState{},
	}

	c.egressServiceLister = esInformer.Lister()
	c.egressServiceSynced = esInformer.Informer().HasSynced
	c.egressServiceQueue = workqueue.NewNamedRateLimitingQueue(
		workqueue.NewItemFastSlowRateLimiter(1*time.Second, 5*time.Second, 5),
		"egressservices",
	)
	_, err := esInformer.Informer().AddEventHandler(factory.WithUpdateHandlingForObjReplace(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.onEgressServiceAdd,
		UpdateFunc: c.onEgressServiceUpdate,
		DeleteFunc: c.onEgressServiceDelete,
	}))
	if err != nil {
		return nil, err
	}

	c.serviceLister = serviceInformer.Lister()
	c.servicesSynced = serviceInformer.Informer().HasSynced
	_, err = serviceInformer.Informer().AddEventHandler(factory.WithUpdateHandlingForObjReplace(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.onServiceAdd,
		UpdateFunc: c.onServiceUpdate,
		DeleteFunc: c.onServiceDelete,
	}))
	if err != nil {
		return nil, err
	}

	c.endpointSliceLister = endpointSliceInformer.Lister()
	c.endpointSlicesSynced = endpointSliceInformer.Informer().HasSynced
	_, err = endpointSliceInformer.Informer().AddEventHandler(factory.WithUpdateHandlingForObjReplace(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.onEndpointSliceAdd,
		UpdateFunc: c.onEndpointSliceUpdate,
		DeleteFunc: c.onEndpointSliceDelete,
	}))
	if err != nil {
		return nil, err
	}

	c.nodeLister = nodeInformer.Lister()
	c.nodesSynced = nodeInformer.Informer().HasSynced
	c.nodesQueue = workqueue.NewNamedRateLimitingQueue(
		workqueue.NewItemFastSlowRateLimiter(1*time.Second, 5*time.Second, 5),
		"egressservicenodes",
	)
	_, err = nodeInformer.Informer().AddEventHandler(factory.WithUpdateHandlingForObjReplace(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.onNodeAdd,
		UpdateFunc: c.onNodeUpdate,
		DeleteFunc: c.onNodeDelete,
	}))
	if err != nil {
		return nil, err
	}

	return c, nil
}

func (c *Controller) Run(threadiness int) {
	defer utilruntime.HandleCrash()

	klog.Infof("Starting Egress Services Controller")

	if !cache.WaitForNamedCacheSync("egressservices", c.stopCh, c.egressServiceSynced) {
		utilruntime.HandleError(fmt.Errorf("timed out waiting for caches to sync"))
		klog.Infof("Synchronization failed")
		return
	}

	if !cache.WaitForNamedCacheSync("egressservices_services", c.stopCh, c.servicesSynced) {
		utilruntime.HandleError(fmt.Errorf("timed out waiting for caches to sync"))
		klog.Infof("Synchronization failed")
		return
	}

	if !cache.WaitForNamedCacheSync("egressservices_endpointslices", c.stopCh, c.endpointSlicesSynced) {
		utilruntime.HandleError(fmt.Errorf("timed out waiting for caches to sync"))
		klog.Infof("Synchronization failed")
		return
	}

	if !cache.WaitForNamedCacheSync("egressservices_nodes", c.stopCh, c.nodesSynced) {
		utilruntime.HandleError(fmt.Errorf("timed out waiting for caches to sync"))
		klog.Infof("Synchronization failed")
		return
	}

	klog.Infof("Repairing Egress Services")
	err := c.repair()
	if err != nil {
		klog.Errorf("Failed to repair Egress Services entries: %v", err)
	}

	err = c.initClusterEgressPolicies(c.nbClient, c.addressSetFactory, c.controllerName)
	if err != nil {
		klog.Errorf("Failed to init Egress Services cluster policies: %v", err)
	}

	wg := &sync.WaitGroup{}
	for i := 0; i < threadiness; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wait.Until(func() {
				c.runEgressServiceWorker(wg)
			}, time.Second, c.stopCh)
		}()
	}

	for i := 0; i < threadiness; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wait.Until(func() {
				c.runNodeWorker(wg)
			}, time.Second, c.stopCh)
		}()
	}

	// wait until we're told to stop
	<-c.stopCh

	klog.Infof("Shutting down Egress Services controller")
	c.egressServiceQueue.ShutDown()
	c.nodesQueue.ShutDown()

	wg.Wait()
}

// This takes care of syncing stale data which we might have in OVN if
// there's no ovnkube-master running for a while.
// It deletes all logical router policies from OVN that belong to services which are no longer
// egress services, and the policies of endpoints that do not belong to an egress service.
func (c *Controller) repair() error {
	c.Lock()
	defer c.Unlock()

	// all the current valid egress services keys to their endpoints from the listers.
	svcKeyToAllV4Endpoints := map[string]sets.Set[string]{}
	svcKeyToAllV6Endpoints := map[string]sets.Set[string]{}

	// all known existing egress services to their endpoints from OVN.
	svcKeyToConfiguredV4Endpoints := map[string][]string{}
	svcKeyToConfiguredV6Endpoints := map[string][]string{}

	services, err := c.serviceLister.List(labels.Everything())
	if err != nil {
		return err
	}
	allServices := map[string]*corev1.Service{}
	for _, s := range services {
		key, err := cache.MetaNamespaceKeyFunc(s)
		if err != nil {
			klog.Errorf("Failed to read Service key: %v", err)
			continue
		}
		allServices[key] = s
	}

	egressServices, err := c.egressServiceLister.List(labels.Everything())
	if err != nil {
		return err
	}
	for _, es := range egressServices {
		key, err := cache.MetaNamespaceKeyFunc(es)
		if err != nil {
			klog.Errorf("Failed to read EgressService key: %v", err)
			continue
		}
		svc := allServices[key]
		if svc == nil {
			continue
		}

		if !util.ServiceTypeHasLoadBalancer(svc) || len(svc.Status.LoadBalancer.Ingress) == 0 {
			continue
		}

		svcHost := es.Status.Host
		if svcHost == "" {
			continue
		}

		node, err := c.nodeLister.Get(svcHost)
		if err != nil {
			klog.Errorf("Node %s could not be retrieved from lister, err: %v", svcHost, err)
			continue
		}
		if !nodeIsReady(node) {
			klog.Infof("Node %s is not ready, it can not be used for egress service %s", svcHost, key)
			continue
		}

		v4, v6, err := c.allEndpointsFor(svc)
		if err != nil {
			klog.Errorf("Can't fetch all local endpoints for egress service %s, err: %v", key, err)
			continue
		}

		totalEps := len(v4) + len(v6)
		if totalEps == 0 {
			klog.Infof("Egress service %s has no endpoints", key)
			continue
		}

		nodeState, ok := c.nodes[svcHost]
		if !ok {
			nodeState, err = c.nodeStateFor(svcHost)
			if err != nil {
				klog.Errorf("Can't fetch egress service %s node %s state, err: %v", key, svcHost, err)
				continue
			}
		}
		svcKeyToAllV4Endpoints[key] = v4
		svcKeyToAllV6Endpoints[key] = v6
		svcKeyToConfiguredV4Endpoints[key] = []string{}
		svcKeyToConfiguredV6Endpoints[key] = []string{}
		svcState := &svcState{node: svcHost, v4Endpoints: sets.New[string](), v6Endpoints: sets.New[string](), stale: false}
		c.nodes[svcHost] = nodeState
		c.services[key] = svcState
	}

	p := func(item *nbdb.LogicalRouterPolicy) bool {
		if item.Priority != ovntypes.EgressSVCReroutePriority {
			return false
		}

		svcKey, found := item.ExternalIDs[svcExternalIDKey]
		if !found {
			klog.Infof("Egress service repair will delete lrp because it uses the egress service priority but does not belong to one: %v", item)
			return true
		}

		svc, found := c.services[svcKey]
		if !found {
			klog.Infof("Egress service repair will delete lrp for service %s because it is no longer a valid egress service: %v", svcKey, item)
			return true
		}

		v4Eps := svcKeyToAllV4Endpoints[svcKey]
		v6Eps := svcKeyToAllV6Endpoints[svcKey]

		// we extract the IP from the match: "ip4.src == IP" / "ip6.src == IP"
		splitMatch := strings.Split(item.Match, " ")
		logicalIP := splitMatch[len(splitMatch)-1]
		if !v4Eps.Has(logicalIP) && !v6Eps.Has(logicalIP) {
			klog.Infof("Egress service repair will delete lrp for service %s: Cannot find a valid endpoint within match criteria: %v", svcKey, item)
			return true
		}

		if len(item.Nexthops) != 1 {
			klog.Infof("Egress service repair will delete lrp for service %s because it has more than one nexthop: %v", svcKey, item)
			return true
		}

		node := c.nodes[svc.node]
		if item.Nexthops[0] != node.v4MgmtIP.String() && item.Nexthops[0] != node.v6MgmtIP.String() {
			klog.Infof("Egress service repair will delete %s because it is uses a stale nexthop for service %s: %v", logicalIP, svcKey, item)
			return true
		}

		if utilnet.IsIPv4String(logicalIP) {
			svcKeyToConfiguredV4Endpoints[svcKey] = append(svcKeyToConfiguredV4Endpoints[svcKey], logicalIP)
			return false
		}

		svcKeyToConfiguredV6Endpoints[svcKey] = append(svcKeyToConfiguredV6Endpoints[svcKey], logicalIP)
		return false
	}

	errorList := []error{}
	err = libovsdbops.DeleteLogicalRouterPoliciesWithPredicate(c.nbClient, ovntypes.OVNClusterRouter, p)
	if err != nil {
		errorList = append(errorList,
			fmt.Errorf("error deleting stale logical router policies from router %s: %v", ovntypes.OVNClusterRouter, err))
	}

	// update caches after transaction completed
	for key, v4ToAdd := range svcKeyToConfiguredV4Endpoints {
		c.services[key].v4Endpoints.Insert(v4ToAdd...)
	}

	for key, v6ToAdd := range svcKeyToConfiguredV6Endpoints {
		c.services[key].v6Endpoints.Insert(v6ToAdd...)
	}

	return errors.NewAggregate(errorList)
}

// onEgressServiceAdd queues the EgressService for processing.
func (c *Controller) onEgressServiceAdd(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", obj, err))
		return
	}
	c.egressServiceQueue.Add(key)
}

// onEgressServiceUpdate queues the EgressService for processing.
func (c *Controller) onEgressServiceUpdate(oldObj, newObj interface{}) {
	oldEQ := oldObj.(*egressserviceapi.EgressService)
	newEQ := newObj.(*egressserviceapi.EgressService)

	if oldEQ.ResourceVersion == newEQ.ResourceVersion ||
		!newEQ.GetDeletionTimestamp().IsZero() {
		return
	}

	key, err := cache.MetaNamespaceKeyFunc(newObj)
	if err == nil {
		c.egressServiceQueue.Add(key)
	}
}

// onEgressServiceDelete queues the EgressService for processing.
func (c *Controller) onEgressServiceDelete(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", obj, err))
		return
	}
	c.egressServiceQueue.Add(key)
}

func (c *Controller) runEgressServiceWorker(wg *sync.WaitGroup) {
	for c.processNextEgressServiceWorkItem(wg) {
	}
}

func (c *Controller) processNextEgressServiceWorkItem(wg *sync.WaitGroup) bool {
	wg.Add(1)
	defer wg.Done()

	key, quit := c.egressServiceQueue.Get()
	if quit {
		return false
	}

	defer c.egressServiceQueue.Done(key)

	err := c.syncEgressService(key.(string))
	if err == nil {
		c.egressServiceQueue.Forget(key)
		return true
	}

	utilruntime.HandleError(fmt.Errorf("%v failed with : %v", key, err))

	if c.egressServiceQueue.NumRequeues(key) < maxRetries {
		c.egressServiceQueue.AddRateLimited(key)
		return true
	}

	c.egressServiceQueue.Forget(key)
	return true
}

func (c *Controller) syncEgressService(key string) error {
	c.Lock()
	defer c.Unlock()

	startTime := time.Now()
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}
	klog.Infof("Processing sync for Egress Service %s/%s", namespace, name)

	defer func() {
		klog.V(4).Infof("Finished syncing Egress Service %s/%s : %v", namespace, name, time.Since(startTime))
	}()

	es, err := c.egressServiceLister.EgressServices(namespace).Get(name)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}

	svc, err := c.serviceLister.Services(namespace).Get(name)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}

	state := c.services[key]

	// Clean up the service if it is not assigned to any host or was removed
	if es == nil || len(es.Status.Host) == 0 {
		klog.V(5).Infof("Egress service %s was removed or is not assigned to any host", key)
		if state == nil {
			// The egress service was not configured, nothing to do
			return nil
		}
		// The egress service is configured, but it no longer exists/is assigned to a node,
		// meaning we should clear all of its resources.
		return c.clearServiceResourcesAndRequeue(key, state)
	}

	if svc == nil {
		klog.V(5).Infof("Service %s doesn't exist", key)
		if state == nil {
			// The service object was deleted, and the egress service was not configured, nothing to do.
			return nil
		}
		// The egress service is configured, but the service object was deleted,
		// meaning we should clear all of its resources.
		return c.clearServiceResourcesAndRequeue(key, state)
	}

	// At this point both the EgressService is assigned and the Service != nil

	if state != nil && state.stale {
		// The service is marked stale because something failed when trying to delete it.
		// We try to delete it again before doing anything else.
		klog.Warningf("Cleaning up stale egress service %s", key)
		return c.clearServiceResourcesAndRequeue(key, state)
	}

	if len(svc.Status.LoadBalancer.Ingress) == 0 {
		klog.Infof("EgressService %s/%s does not have an ingress IP")
		if state == nil {
			// The service object doesn't have an ingress IP, and the egress service was not configured, nothing to do.
			return nil
		}
		// The egress service is configured, but the service object doesn't have an ingress IP,
		// meaning we should clear all of its resources.
		return c.clearServiceResourcesAndRequeue(key, state)
	}

	v4Endpoints, v6Endpoints, err := c.allEndpointsFor(svc)
	if err != nil {
		return err
	}

	totalEps := len(v4Endpoints) + len(v6Endpoints)

	if totalEps == 0 && state != nil {
		klog.V(4).Infof("EgressService %s/%s does not have any local endpoints, removing any existing configuration", namespace, name)
		return c.clearServiceResourcesAndRequeue(key, state)
	}

	if state == nil {
		// The service has a valid EgressService and wasn't configured before.
		nodeName := es.Status.Host
		newState := &svcState{node: nodeName, v4Endpoints: sets.New[string](), v6Endpoints: sets.New[string](), stale: false}
		c.services[key] = newState
		if _, exists := c.nodes[nodeName]; !exists {
			nodeState, err := c.nodeStateFor(nodeName)
			if err != nil {
				return err
			}
			c.nodes[nodeName] = nodeState
		}
		state = newState
	}

	if state.node != es.Status.Host {
		klog.Errorf("EgressService %s/%s is configured for %s instead of %s, removing any existing configuration", namespace, name, state.node, es.Status.Host)
		return c.clearServiceResourcesAndRequeue(key, state)
	}

	node, ok := c.nodes[state.node]
	if !ok || node.draining {
		klog.Warningf("EgressService %s/%s is configured on non-existing or not ready node %s, removing", namespace, name, state.node)
		return c.clearServiceResourcesAndRequeue(key, state)
	}

	// At this point the states are valid and we should create the proper logical router policies.
	// We reach the desired state by fetching all of the endpoints associated to the service and comparing
	// to the known state:
	// We need to create policies for endpoints that were fetched but not found in the cache,
	// and delete the policies for those which are found in the cache but were not fetched.
	// We do it in one transaction, if it succeeds we update the cache to reflect the new state.

	v4ToAdd := v4Endpoints.Difference(state.v4Endpoints).UnsortedList()
	v6ToAdd := v6Endpoints.Difference(state.v6Endpoints).UnsortedList()
	v4ToRemove := state.v4Endpoints.Difference(v4Endpoints).UnsortedList()
	v6ToRemove := state.v6Endpoints.Difference(v6Endpoints).UnsortedList()

	allOps := []libovsdb.Operation{}
	createOps, err := c.createLogicalRouterPoliciesOps(key, node.v4MgmtIP.String(), node.v6MgmtIP.String(), v4ToAdd, v6ToAdd)
	if err != nil {
		return err
	}
	allOps = append(allOps, createOps...)

	// update egresssvc-served-pods address set used to ensure egress service
	// does not affect pod -> node ip traffic
	// https://github.com/ovn-org/ovn-kubernetes/blob/master/docs/egress-ip.md#pod-to-node-ip-traffic
	createOps, err = c.addPodIPsToAddressSetOps(createIPAddressNetSlice(v4ToAdd, v6ToAdd))
	if err != nil {
		return err
	}
	allOps = append(allOps, createOps...)

	deleteOps, err := c.deleteLogicalRouterPoliciesOps(key, v4ToRemove, v6ToRemove)
	if err != nil {
		return err
	}
	allOps = append(allOps, deleteOps...)

	deleteOps, err = c.deletePodIPsFromAddressSetOps(createIPAddressNetSlice(v4ToRemove, v6ToRemove))
	if err != nil {
		return err
	}
	allOps = append(allOps, deleteOps...)

	if _, err := libovsdbops.TransactAndCheck(c.nbClient, allOps); err != nil {
		return fmt.Errorf("failed to update router policies for %s, err: %v", key, err)
	}

	state.v4Endpoints.Insert(v4ToAdd...)
	state.v4Endpoints.Delete(v4ToRemove...)
	state.v6Endpoints.Insert(v6ToAdd...)
	state.v6Endpoints.Delete(v6ToRemove...)

	return nil
}

// Removes all the logical router policies that belong to the egress service.
// This also requeues the service after cleaning up to be sure we are not
// missing an event after marking it as stale that should be handled.
// This should only be called with the controller locked.
func (c *Controller) clearServiceResourcesAndRequeue(key string, svcState *svcState) error {
	svcState.stale = true

	p := func(item *nbdb.LogicalRouterPolicy) bool {
		return item.ExternalIDs[svcExternalIDKey] == key
	}

	deleteOps := []libovsdb.Operation{}
	deleteOps, err := libovsdbops.DeleteLogicalRouterPolicyWithPredicateOps(c.nbClient, deleteOps, ovntypes.OVNClusterRouter, p)
	if err != nil {
		return err
	}

	if _, err := libovsdbops.TransactAndCheck(c.nbClient, deleteOps); err != nil {
		return fmt.Errorf("failed to clean router policies for %s, err: %v", key, err)
	}

	delete(c.services, key)
	c.egressServiceQueue.Add(key)
	return nil
}
