package egressservice

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	libovsdb "github.com/ovn-org/libovsdb/ovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
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

	services       map[string]*svcState  // svc key -> state
	nodes          map[string]*nodeState // node name -> state, contains nodes that host an egress service
	nodesZoneState map[string]bool       // node name -> is in local zone, contains all nodes in the cluster

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

	zone string
}

type svcState struct {
	node string
	// service endpoints that are hosted in the local zone (if IC is disabled, this holds all service endpoints)
	v4LocalEndpoints sets.Set[string]
	v6LocalEndpoints sets.Set[string]

	// service endpoints that are hosted in a zone remote to the service zone
	// only used when IC is enabled
	v4RemoteEndpoints sets.Set[string]
	v6RemoteEndpoints sets.Set[string]
	stale             bool
}

type nodeState struct {
	name     string
	draining bool
	v4MgmtIP net.IP
	v6MgmtIP net.IP

	// node router IPs in the transit switch subnet, only used when IC is enabled
	transitIPV4 net.IP
	transitIPV6 net.IP
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
	nodeInformer coreinformers.NodeInformer,
	zone string) (*Controller, error) {
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
		nodesZoneState:                           map[string]bool{},
		zone:                                     zone,
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
	svcKeyToLocalV4Endpoints := map[string]sets.Set[string]{}
	svcKeyToLocalV6Endpoints := map[string]sets.Set[string]{}
	svcKeyToRemoteV4Endpoints := map[string]sets.Set[string]{}
	svcKeyToRemoteV6Endpoints := map[string]sets.Set[string]{}

	// all known existing egress services to their endpoints from OVN.
	svcKeyToLocalConfiguredV4Endpoints := map[string][]string{}
	svcKeyToLocalConfiguredV6Endpoints := map[string][]string{}
	svcKeyToRemoteConfiguredV4Endpoints := map[string][]string{}
	svcKeyToRemoteConfiguredV6Endpoints := map[string][]string{}

	// Get all the nodes, determine their zone, and build allNodes map for later use
	nodes, err := c.nodeLister.List(labels.Everything())
	allNodes := map[string]*corev1.Node{}
	if err != nil {
		return err
	}
	for _, n := range nodes {
		c.nodesZoneState[n.Name] = c.isNodeInLocalZone(n)
		allNodes[n.Name] = n
	}

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

		node, found := allNodes[svcHost]
		if !found {
			klog.Errorf("Node %s not found", svcHost, err)
			continue
		}

		if !nodeIsReady(node) {
			klog.Infof("Node %s is not ready, it can not be used for egress service %s", svcHost, key)
			continue
		}

		v4Local, v6Local, v4Remote, v6Remote, err := c.allEndpointsFor(svc)
		if err != nil {
			klog.Errorf("Can't fetch all endpoints for egress service %s, err: %v", key, err)
			continue
		}

		totalEps := len(v4Local) + len(v6Local) + len(v4Remote) + len(v6Remote)
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
		svcKeyToLocalV4Endpoints[key] = v4Local
		svcKeyToLocalV6Endpoints[key] = v6Local
		svcKeyToRemoteV4Endpoints[key] = v4Remote
		svcKeyToRemoteV6Endpoints[key] = v6Remote
		svcKeyToLocalConfiguredV4Endpoints[key] = []string{}
		svcKeyToLocalConfiguredV6Endpoints[key] = []string{}
		svcState := &svcState{
			node:              svcHost,
			v4LocalEndpoints:  sets.New[string](),
			v6LocalEndpoints:  sets.New[string](),
			v4RemoteEndpoints: sets.New[string](),
			v6RemoteEndpoints: sets.New[string](),
		}
		c.nodes[svcHost] = nodeState
		c.services[key] = svcState
	}

	lrpPredicate := func(item *nbdb.LogicalRouterPolicy) bool {
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

		// We only configure LRPs for endpoint IPs local to the zone
		// When IC is not used svcKeyToLocalV[4|6]Endpoints contains all endpoints
		v4Eps := svcKeyToLocalV4Endpoints[svcKey]
		v6Eps := svcKeyToLocalV6Endpoints[svcKey]

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
		nextHopV4 := node.v4MgmtIP.String()
		nextHopV6 := node.v6MgmtIP.String()
		if config.OVNKubernetesFeature.EnableInterconnect {
			svcNodeInLocalZone, zoneKnown := c.nodesZoneState[node.name]
			if !zoneKnown {
				klog.Errorf("Failed to verify whether the svc node: %s is in the local zone, deleting lrp", node.name)
				return true
			}
			if !svcNodeInLocalZone {
				nextHopV4 = node.transitIPV4.String()
				nextHopV6 = node.transitIPV6.String()
			}
		}

		if item.Nexthops[0] != nextHopV4 && item.Nexthops[0] != nextHopV6 {
			klog.Infof("Egress service repair will delete %s because it is uses a stale nexthop for service %s: %v", logicalIP, svcKey, item)
			return true
		}

		if utilnet.IsIPv4String(logicalIP) {
			svcKeyToLocalConfiguredV4Endpoints[svcKey] = append(svcKeyToLocalConfiguredV4Endpoints[svcKey], logicalIP)
			return false
		}

		svcKeyToLocalConfiguredV6Endpoints[svcKey] = append(svcKeyToLocalConfiguredV6Endpoints[svcKey], logicalIP)
		return false
	}

	errorList := []error{}
	ops := []libovsdb.Operation{}
	ops, err = libovsdbops.DeleteLogicalRouterPolicyWithPredicateOps(c.nbClient, ops, ovntypes.OVNClusterRouter, lrpPredicate)
	if err != nil {
		errorList = append(errorList,
			fmt.Errorf("failed to create ops for deleting stale logical router policies from router %s: %v", ovntypes.OVNClusterRouter, err))
	}

	if config.OVNKubernetesFeature.EnableInterconnect {
		lrsrPredicate := func(item *nbdb.LogicalRouterStaticRoute) bool {
			svcKey, found := item.ExternalIDs[svcExternalIDKey]
			if !found {
				return false
			}

			if item.Policy == nil || *item.Policy != nbdb.LogicalRouterStaticRoutePolicySrcIP {
				klog.Infof("Egress service repair will delete lrsr for service %s it has an invalid policy: %v", svcKey, item)
				return true
			}

			svc, found := c.services[svcKey]
			if !found {
				klog.Infof("Egress service repair will delete lrsr for service %s because it is no longer a valid egress service: %v", svcKey, item)
				return true
			}

			node := c.nodes[svc.node]
			svcNodeInLocalZone, zoneKnown := c.nodesZoneState[node.name]
			if !zoneKnown {
				klog.Errorf("Egress service repair failed to verify whether the svc node %s is in the local zone, deleting lrsr", node.name)
				return true
			}
			if !svcNodeInLocalZone {
				klog.Infof("Egress service repair will delete lrsr for service %s because the service is no longer hosted in the local zone: %v", svcKey, item)
				return true
			}

			// We only configure LRSRs for endpoint IPs remote to the zone for local services
			v4RemoteEps := svcKeyToRemoteV4Endpoints[svcKey]
			v6RemoteEps := svcKeyToRemoteV6Endpoints[svcKey]

			logicalIP := item.IPPrefix
			if !v4RemoteEps.Has(logicalIP) && !v6RemoteEps.Has(logicalIP) {
				klog.Infof("Egress service repair will delete lrsr for service %s: Cannot find a valid remote endpoint within match criteria: %v", svcKey, item)
				return true
			}

			if len(item.Nexthop) == 0 {
				klog.Infof("Egress service repair will delete lrsr for service %s because it has an empty nexthop: %v", svcKey, item)
				return true
			}

			if item.Nexthop != node.v4MgmtIP.String() && item.Nexthop != node.v6MgmtIP.String() {
				klog.Infof("Egress service repair will delete %s lrsr because it is uses a stale nexthop for service %s: %v", logicalIP, svcKey, item)
				return true
			}

			if utilnet.IsIPv4String(logicalIP) {
				svcKeyToRemoteConfiguredV4Endpoints[svcKey] = append(svcKeyToLocalConfiguredV4Endpoints[svcKey], logicalIP)
				return false
			}
			svcKeyToRemoteConfiguredV6Endpoints[svcKey] = append(svcKeyToLocalConfiguredV6Endpoints[svcKey], logicalIP)
			return false
		}
		ops, err = libovsdbops.DeleteLogicalRouterStaticRoutesWithPredicateOps(c.nbClient, ops, ovntypes.OVNClusterRouter, lrsrPredicate)
		if err != nil {
			errorList = append(errorList,
				fmt.Errorf("failed to create ops for deleting stale logical router static routes from router %s: %v", ovntypes.OVNClusterRouter, err))
		}
	}

	if _, err := libovsdbops.TransactAndCheck(c.nbClient, ops); err != nil {
		errorList = append(errorList, fmt.Errorf("failed to remove stale egressservice entries, err: %v", err))
	}

	// update caches after transaction completed
	for key, v4ToAdd := range svcKeyToLocalConfiguredV4Endpoints {
		c.services[key].v4LocalEndpoints.Insert(v4ToAdd...)
	}

	for key, v6ToAdd := range svcKeyToLocalConfiguredV6Endpoints {
		c.services[key].v6LocalEndpoints.Insert(v6ToAdd...)
	}

	for key, v4ToAdd := range svcKeyToRemoteConfiguredV4Endpoints {
		c.services[key].v4RemoteEndpoints.Insert(v4ToAdd...)
	}

	for key, v6ToAdd := range svcKeyToRemoteConfiguredV6Endpoints {
		c.services[key].v6RemoteEndpoints.Insert(v6ToAdd...)
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

	v4LocalEndpoints, v6LocalEndpoints, v4RemoteEndpoints, v6RemoteEndpoints, err := c.allEndpointsFor(svc)
	if err != nil {
		return err
	}

	totalEps := len(v4LocalEndpoints) + len(v6LocalEndpoints) + len(v4RemoteEndpoints) + len(v6RemoteEndpoints)

	if totalEps == 0 && state != nil {
		klog.V(4).Infof("EgressService %s/%s does not have any endpoints, removing any existing configuration", namespace, name)
		return c.clearServiceResourcesAndRequeue(key, state)
	}

	if state == nil {
		// The service has a valid EgressService and wasn't configured before.
		nodeName := es.Status.Host
		newState := &svcState{
			node:              nodeName,
			v4LocalEndpoints:  sets.New[string](),
			v6LocalEndpoints:  sets.New[string](),
			v4RemoteEndpoints: sets.New[string](),
			v6RemoteEndpoints: sets.New[string](),
		}
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

	// At this point the states are valid and we should create the proper logical router policies and static routes.
	// We reach the desired state by fetching all of the endpoints associated to the service and comparing
	// to the known state:
	// We need to create policies for endpoints that were fetched but not found in the cache,
	// and delete the policies for those which are found in the cache but were not fetched.
	// We do it in one transaction, if it succeeds we update the cache to reflect the new state.

	v4LocalToAdd := v4LocalEndpoints.Difference(state.v4LocalEndpoints).UnsortedList()
	v6LocalToAdd := v6LocalEndpoints.Difference(state.v6LocalEndpoints).UnsortedList()
	v4LocalToRemove := state.v4LocalEndpoints.Difference(v4LocalEndpoints).UnsortedList()
	v6LocalToRemove := state.v6LocalEndpoints.Difference(v6LocalEndpoints).UnsortedList()

	v4RemoteToAdd := v4RemoteEndpoints.Difference(state.v4RemoteEndpoints).UnsortedList()
	v6RemoteToAdd := v6RemoteEndpoints.Difference(state.v6RemoteEndpoints).UnsortedList()
	v4RemoteToRemove := state.v4RemoteEndpoints.Difference(v4RemoteEndpoints).UnsortedList()
	v6RemoteToRemove := state.v6RemoteEndpoints.Difference(v6RemoteEndpoints).UnsortedList()

	// v[4|6]LocalEndpoints represents endpoints local to the current zone.
	// v[4|6]RemoteEndpoints represents endpoints remote to the current zone.
	// If a service is hosted in the local zone:
	//  - create LRPs for local endpoints with mgmt IP as a nextHop
	//  - create LRSRs for remote endpoints with mgmt IP as a nextHop
	// If a service is hosted in a remote zone:
	//  - create LRPs for local endpoints with node router transit IP as a next hop as a nextHop
	//  - do nothing for remote endpoints
	// When IC is disabled v[4|6]RemoteEndpoints are empty,
	// service is considered to be local and LRSRs are not modified.

	nextHopV4 := node.v4MgmtIP.String()
	nextHopV6 := node.v6MgmtIP.String()
	svcNodeInLocalZone := true
	if config.OVNKubernetesFeature.EnableInterconnect {
		var zoneKnown bool
		svcNodeInLocalZone, zoneKnown = c.nodesZoneState[node.name]
		if !zoneKnown {
			return fmt.Errorf("failed to verify whether the svc node %s is in the local zone", node.name)
		}
		if !svcNodeInLocalZone {
			nextHopV4 = node.transitIPV4.String()
			nextHopV6 = node.transitIPV6.String()
		}
	}

	allOps := []libovsdb.Operation{}
	createOps, err := c.createOrUpdateLogicalRouterPoliciesOps(key, nextHopV4, nextHopV6, v4LocalToAdd, v6LocalToAdd)
	if err != nil {
		return err
	}
	allOps = append(allOps, createOps...)

	if svcNodeInLocalZone && (len(v4RemoteToAdd)+len(v6RemoteToAdd)) > 0 {
		// when IC is disabled v[4|6]RemoteToRemove are empty and no ops are created
		// with IC enabled, when service is hosted in the local zone, create static routes for remote endpoints
		createOps, err = c.createOrUpdateLogicalRouterStaticRoutesOps(key, node.v4MgmtIP.String(), node.v6MgmtIP.String(), v4RemoteToAdd, v6RemoteToAdd)
		if err != nil {
			return err
		}
		allOps = append(allOps, createOps...)
	}

	// update egresssvc-served-pods address set used to ensure egress service
	// does not affect pod -> node ip traffic
	// https://github.com/ovn-org/ovn-kubernetes/blob/master/docs/egress-ip.md#pod-to-node-ip-traffic
	createOps, err = c.addPodIPsToAddressSetOps(createIPAddressNetSlice(v4LocalToAdd, v6LocalToAdd))
	if err != nil {
		return err
	}
	allOps = append(allOps, createOps...)

	deleteOps, err := c.deleteLogicalRouterPoliciesOps(key, v4LocalToRemove, v6LocalToRemove)
	if err != nil {
		return err
	}
	allOps = append(allOps, deleteOps...)

	// when IC is disabled v[4|6]RemoteToRemove are empty and no ops are created
	// with IC enabled, it is safer to avoid checking whether the service is local
	// as we want to remove the static routes configured for the specific remote pods.
	deleteOps, err = c.deleteLogicalRouterStaticRoutesOps(key, v4RemoteToRemove, v6RemoteToRemove)
	if err != nil {
		return err
	}
	allOps = append(allOps, deleteOps...)

	deleteOps, err = c.deletePodIPsFromAddressSetOps(createIPAddressNetSlice(v4LocalToRemove, v6LocalToRemove))
	if err != nil {
		return err
	}
	allOps = append(allOps, deleteOps...)

	if _, err := libovsdbops.TransactAndCheck(c.nbClient, allOps); err != nil {
		return fmt.Errorf("failed to update router policies for %s, err: %v", key, err)
	}

	state.v4LocalEndpoints.Insert(v4LocalToAdd...)
	state.v4LocalEndpoints.Delete(v4LocalToRemove...)
	state.v6LocalEndpoints.Insert(v6LocalToAdd...)
	state.v6LocalEndpoints.Delete(v6LocalToRemove...)

	state.v4RemoteEndpoints.Insert(v4RemoteToAdd...)
	state.v4RemoteEndpoints.Delete(v4RemoteToRemove...)
	state.v6RemoteEndpoints.Insert(v6RemoteToAdd...)
	state.v6RemoteEndpoints.Delete(v6RemoteToRemove...)
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

	if config.OVNKubernetesFeature.EnableInterconnect {
		p := func(item *nbdb.LogicalRouterStaticRoute) bool {
			return item.ExternalIDs[svcExternalIDKey] == key
		}
		deleteOps, err = libovsdbops.DeleteLogicalRouterStaticRoutesWithPredicateOps(c.nbClient, deleteOps, ovntypes.OVNClusterRouter, p)
		if err != nil {
			return err
		}
	}

	if _, err := libovsdbops.TransactAndCheck(c.nbClient, deleteOps); err != nil {
		return fmt.Errorf("failed to clean egress service resources for %s, err: %v", key, err)
	}

	delete(c.services, key)
	c.egressServiceQueue.Add(key)
	return nil
}
