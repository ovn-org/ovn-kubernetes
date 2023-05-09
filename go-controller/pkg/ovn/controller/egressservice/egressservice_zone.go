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
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/healthcheck"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	maxRetries           = 10
	svcExternalIDKey     = "EgressSVC" // key set on lrps to identify to which egress service it belongs
	egressSVCLabelPrefix = "egress-service.k8s.ovn.org"
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
	IsReachable                              func(nodeName string, mgmtIPs []net.IP, healthClient healthcheck.EgressIPHealthClient) bool // TODO: make a universal cache instead
	setEgressServiceStatus                   func(ns, name, host string) error

	services map[string]*svcState  // svc key -> state
	nodes    map[string]*nodeState // node name -> state

	// A map of the services we attempted to allocate but could not.
	// When a node is updated we check this map to see if a service can
	// be allocated on it - if it does we queue the service again.
	// We also check this cache when an ep is added, as the service might
	// got to this cache by having no eps.
	unallocatedServices map[string]labels.Selector // svc key -> its node selector

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
	selector    labels.Selector
	v4Endpoints sets.Set[string]
	v6Endpoints sets.Set[string]
	stale       bool
}

type nodeState struct {
	name         string
	labels       map[string]string
	mgmtIPs      []net.IP
	v4MgmtIP     net.IP
	v6MgmtIP     net.IP
	healthClient healthcheck.EgressIPHealthClient
	allocations  map[string]*svcState // svc key -> state
	reachable    bool
	draining     bool
}

func NewController(
	controllerName string,
	client kubernetes.Interface,
	nbClient libovsdbclient.Client,
	addressSetFactory addressset.AddressSetFactory,
	initClusterEgressPolicies InitClusterEgressPoliciesFunc,
	ensureNoRerouteNodePolicies EnsureNoRerouteNodePoliciesFunc,
	deleteLegacyDefaultNoRerouteNodePolicies DeleteLegacyDefaultNoRerouteNodePoliciesFunc,
	setEgressServiceStatus func(ns, name, host string) error,
	isReachable func(nodeName string, mgmtIPs []net.IP, healthClient healthcheck.EgressIPHealthClient) bool,
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
		IsReachable:                              isReachable,
		setEgressServiceStatus:                   setEgressServiceStatus,
		stopCh:                                   stopCh,
		services:                                 map[string]*svcState{},
		nodes:                                    map[string]*nodeState{},
		unallocatedServices:                      map[string]labels.Selector{},
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

	go c.checkNodesReachability()

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
// In addition, it removes the egress service labels of deleted services from the nodes.
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

		nodeSelector := &es.Spec.NodeSelector
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

		v4, v6, epsNodes, err := c.allEndpointsFor(svc)
		if err != nil {
			klog.Errorf("Can't fetch all endpoints for egress service %s, err: %v", key, err)
			continue
		}

		totalEps := len(v4) + len(v6)
		if totalEps == 0 {
			klog.Infof("Egress service %s has no endpoints", key)
			continue
		}

		if len(epsNodes) != 0 && svc.Spec.ExternalTrafficPolicy == corev1.ServiceExternalTrafficPolicyTypeLocal {
			// If the service is ETP=Local only a node with local eps can be used.
			// We want to verify that the current selected node has a local ep.
			matchEpsNodes := metav1.LabelSelectorRequirement{
				Key:      "kubernetes.io/hostname",
				Operator: metav1.LabelSelectorOpIn,
				Values:   epsNodes,
			}
			nodeSelector.MatchExpressions = append(nodeSelector.MatchExpressions, matchEpsNodes)
		}

		selector, err := metav1.LabelSelectorAsSelector(nodeSelector)
		if err != nil {
			klog.Errorf("Selector %s is invalid for EgressService %s, err: %v", selector.String(), key, err)
			continue
		}

		if !selector.Matches(labels.Set(node.Labels)) {
			klog.Infof("Node %s does no longer match service %s selectors %s", svcHost, key, selector.String())
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
		svcState := &svcState{node: svcHost, selector: selector, v4Endpoints: sets.New[string](), v6Endpoints: sets.New[string](), stale: false}
		nodeState.allocations[key] = svcState
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

	// remove stale host entries from EgressServices without a valid service
	for _, es := range egressServices {
		key, err := cache.MetaNamespaceKeyFunc(es)
		if err != nil {
			klog.Errorf("Failed to read EgressService key: %v", err)
			continue
		}
		_, found := c.services[key]
		if !found {
			err := c.setEgressServiceHost(es.Namespace, es.Name, "")
			if err != nil {
				errorList = append(errorList,
					fmt.Errorf("failed to remove stale host entry from EgressService %s, err: %v", key, err))
			}
		}
	}

	// now remove any stale egress service labels on nodes
	nodes, _ := c.nodeLister.List(labels.Everything())
	svcLabelToNode := map[string]string{}
	for key, state := range c.services {
		namespace, name, _ := cache.SplitMetaNamespaceKey(key)
		svcLabelToNode[c.nodeLabelForService(namespace, name)] = state.node
	}

	for _, node := range nodes {
		labelsToRemove := map[string]any{}
		for labelKey := range node.Labels {
			if strings.HasPrefix(labelKey, egressSVCLabelPrefix) && svcLabelToNode[labelKey] != node.Name {
				labelsToRemove[labelKey] = nil // Patching with a nil value results in the delete of the key
			}
		}
		err := c.patchNodeLabels(node.Name, labelsToRemove)
		if err != nil {
			errorList = append(errorList,
				fmt.Errorf("failed to remove stale labels %v from node %s, err: %v", labelsToRemove, node.Name, err))
		}
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
	if es == nil {
		if state == nil {
			// The EgressService was deleted and wasn't configured before.
			// We delete it from the unallocated service cache just in case.
			delete(c.unallocatedServices, key)
			return nil
		}
		// The service is configured but does no longer have the egress service resource,
		// meaning we should clear all of its resources.
		return c.clearServiceResourcesAndRequeue(key, state)
	}

	if svc == nil {
		if state == nil {
			// The service object was deleted and was not an allocated egress service.
			// We delete it from the unallocated service cache just in case.
			delete(c.unallocatedServices, key)
			return c.setEgressServiceHost(namespace, name, "")
		}
		// The service was deleted and was an egress service.
		// We delete all of its relevant resources to avoid leaving stale configuration.
		return c.clearServiceResourcesAndRequeue(key, state)
	}

	// At this point both the EgressService resource and the Service != nil

	if state != nil && state.stale {
		// The service is marked stale because something failed when trying to delete it.
		// We try to delete it again before doing anything else.
		return c.clearServiceResourcesAndRequeue(key, state)
	}

	if state == nil && len(svc.Status.LoadBalancer.Ingress) == 0 {
		// The service wasn't configured before and does not have an ingress ip.
		// we don't need to configure it and make sure it does not have a stale host value or unallocated entry.
		klog.V(4).Infof("EgressService %s/%s does not have an ingress ip, will not attempt configuring it", namespace, name)
		delete(c.unallocatedServices, key)
		return c.setEgressServiceHost(namespace, name, "")
	}

	if state != nil && len(svc.Status.LoadBalancer.Ingress) == 0 {
		// The service has no ingress ips so it is not considered valid anymore.
		klog.V(4).Infof("EgressService %s/%s does not have an ingress ip anymore, removing its existing configuration", namespace, name)
		return c.clearServiceResourcesAndRequeue(key, state)
	}

	nodeSelector := es.Spec.NodeSelector
	v4Endpoints, v6Endpoints, epsNodes, err := c.allEndpointsFor(svc)
	if err != nil {
		return err
	}

	// If the service is ETP=Local we'd like to add an additional constraint to the selector
	// that only a node with local eps can be selected. Otherwise new ingress traffic will break.
	if len(epsNodes) != 0 && svc.Spec.ExternalTrafficPolicy == corev1.ServiceExternalTrafficPolicyTypeLocal {
		matchEpsNodes := metav1.LabelSelectorRequirement{
			Key:      "kubernetes.io/hostname",
			Operator: metav1.LabelSelectorOpIn,
			Values:   epsNodes,
		}
		nodeSelector.MatchExpressions = append(nodeSelector.MatchExpressions, matchEpsNodes)
	}
	selector, err := metav1.LabelSelectorAsSelector(&nodeSelector)
	if err != nil {
		return err
	}
	totalEps := len(v4Endpoints) + len(v6Endpoints)

	// We don't want to select a node for a service without endpoints to not "waste" an
	// allocation on a node.
	if totalEps == 0 && state == nil {
		klog.V(4).Infof("EgressService %s/%s does not have any endpoints, will not attempt configuring it", namespace, name)
		c.unallocatedServices[key] = selector
		return c.setEgressServiceHost(namespace, name, "")
	}

	if totalEps == 0 && state != nil {
		klog.V(4).Infof("EgressService %s/%s does not have any endpoints, removing its existing configuration", namespace, name)
		c.unallocatedServices[key] = selector
		return c.clearServiceResourcesAndRequeue(key, state)
	}

	if state == nil {
		// The service has a valid EgressService and wasn't configured before.
		// This means we need to select a node for it that matches its selector.
		c.unallocatedServices[key] = selector

		node, err := c.selectNodeFor(selector)
		if err != nil {
			return err
		}

		// We found a node - update the caches with the new objects.
		delete(c.unallocatedServices, key)
		newState := &svcState{node: node.name, selector: selector, v4Endpoints: sets.New[string](), v6Endpoints: sets.New[string](), stale: false}
		c.services[key] = newState
		node.allocations[key] = newState
		c.nodes[node.name] = node
		state = newState
	}

	state.selector = selector
	node := c.nodes[state.node]

	if !state.selector.Matches(labels.Set(node.labels)) {
		// The node no longer matches the selector.
		// We clear its configured resources and requeue it to attempt
		// selecting a new node for it.
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

	// We configured OVN - the last step is to label the node and set the status
	// to mark it as the node holding the service.

	err = c.setEgressServiceHost(namespace, name, state.node) // set the EgressService status, will also override manual changes
	if err != nil {
		return err
	}

	return c.labelNodeForService(namespace, name, node.name)
}

// Removes all of the resources that belong to the egress service.
// This includes removing the host status value, the logical router policies,
// the label from the node and updating the caches.
// This also requeues the service after cleaning up to be sure we are not
// missing an event after marking it as stale that should be handled.
// This should only be called with the controller locked.
func (c *Controller) clearServiceResourcesAndRequeue(key string, svcState *svcState) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}

	svcState.stale = true

	p := func(item *nbdb.LogicalRouterPolicy) bool {
		return item.ExternalIDs[svcExternalIDKey] == key
	}

	deleteOps := []libovsdb.Operation{}
	deleteOps, err = libovsdbops.DeleteLogicalRouterPolicyWithPredicateOps(c.nbClient, deleteOps, ovntypes.OVNClusterRouter, p)
	if err != nil {
		return err
	}

	if _, err := libovsdbops.TransactAndCheck(c.nbClient, deleteOps); err != nil {
		return fmt.Errorf("failed to clean router policies for %s, err: %v", key, err)
	}

	if err := c.setEgressServiceHost(namespace, name, ""); err != nil {
		return err
	}

	nodeState, found := c.nodes[svcState.node]
	if found {
		if err := c.removeNodeServiceLabel(namespace, name, svcState.node); err != nil {
			return fmt.Errorf("failed to remove svc node label for %s, err: %v", svcState.node, err)
		}
		delete(nodeState.allocations, key)
	}

	delete(c.services, key)
	c.egressServiceQueue.Add(key)
	return nil
}

func (c *Controller) setEgressServiceHost(namespace, name, host string) error {
	err := c.setEgressServiceStatus(namespace, name, host)
	if err != nil {
		if host != "" {
			return err
		}

		if !apierrors.IsNotFound(err) {
			return err
		}
	}

	// We return nil if either we got no error or the host="" and we got a "resource missing err".
	// This makes stuff easier when cleaning the resources of the service as EgressService deleted
	// and EgressService having an empty host means the same in that context.
	return nil
}
