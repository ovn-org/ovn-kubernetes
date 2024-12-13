package egressservice

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	egressserviceapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressservice/v1"
	egressserviceinformer "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressservice/v1/apis/informers/externalversions/egressservice/v1"
	egressservicelisters "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressservice/v1/apis/listers/egressservice/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	nodenft "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/nftables"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/controller/services"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	utilerrors "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/errors"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	corelisters "k8s.io/client-go/listers/core/v1"
	discoverylisters "k8s.io/client-go/listers/discovery/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
	"sigs.k8s.io/knftables"
)

const (
	IPRulePriority = 5000 // the priority of the ip rules created by the controller. Egress IP priority is 6000.

	// chain for egress service NAT rules
	NFTablesChainName = "egress-services"

	// map for IPv4 SNAT mappings
	NFTablesMapV4 = "egress-service-snat-v4"

	// map for IPv6 SNAT mappings
	NFTablesMapV6 = "egress-service-snat-v6"
)

type Controller struct {
	stopCh <-chan struct{}
	sync.Mutex
	// Packets coming with this mark should not be SNATed.
	// In particular, this mark is set by an ovn lrp when the pod is local to the
	// host node and tries to reach another node and in that case we don't want
	// to snat its traffic, which matches the behavior of pods on different nodes
	// when they try to reach a different node but hit the 102 "allow" lrp.
	// See https://github.com/ovn-org/ovn-kubernetes/pull/3064 for more details.
	returnMark string
	thisNode   string // name of the node we're running on

	egressServiceLister egressservicelisters.EgressServiceLister
	egressServiceSynced cache.InformerSynced
	egressServiceQueue  workqueue.TypedRateLimitingInterface[string]

	serviceLister  corelisters.ServiceLister
	servicesSynced cache.InformerSynced

	endpointSliceLister  discoverylisters.EndpointSliceLister
	endpointSlicesSynced cache.InformerSynced

	services map[string]*svcState // svc key -> state
}

type svcState struct {
	v4LB        string           // IPv4 ingress of the service
	v4Eps       sets.Set[string] // v4 endpoints that have an SNAT rule configured
	v6LB        string           // IPv6 ingress of the service
	v6Eps       sets.Set[string] // v6 endpoints that have an SNAT rule configured
	net         string           // net corresponding to the spec.Network
	netEps      sets.Set[string] // All endpoints that have an ip rule configured
	v4NodePorts sets.Set[int32]  // All v4 nodeports that have an ip rule configured, relevant when ETP=Local
	v6NodePorts sets.Set[int32]  // All v6 nodeports that have an ip rule configured, relevant when ETP=Local

	stale bool
}

func NewController(stopCh <-chan struct{}, returnMark, thisNode string,
	esInformer egressserviceinformer.EgressServiceInformer,
	serviceInformer cache.SharedIndexInformer,
	endpointSliceInformer cache.SharedIndexInformer) (*Controller, error) {
	klog.Info("Setting up event handlers for Egress Services")

	c := &Controller{
		stopCh:     stopCh,
		returnMark: returnMark,
		thisNode:   thisNode,
		services:   map[string]*svcState{},
	}

	c.egressServiceLister = esInformer.Lister()
	c.egressServiceSynced = esInformer.Informer().HasSynced
	c.egressServiceQueue = workqueue.NewTypedRateLimitingQueueWithConfig(
		workqueue.NewTypedItemFastSlowRateLimiter[string](1*time.Second, 5*time.Second, 5),
		workqueue.TypedRateLimitingQueueConfig[string]{Name: "egressservices"},
	)
	_, err := esInformer.Informer().AddEventHandler(factory.WithUpdateHandlingForObjReplace(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.onEgressServiceAdd,
		UpdateFunc: c.onEgressServiceUpdate,
		DeleteFunc: c.onEgressServiceDelete,
	}))
	if err != nil {
		return nil, err
	}

	c.serviceLister = corelisters.NewServiceLister(serviceInformer.GetIndexer())
	c.servicesSynced = serviceInformer.HasSynced
	_, err = serviceInformer.AddEventHandler(factory.WithUpdateHandlingForObjReplace(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.onServiceAdd,
		UpdateFunc: c.onServiceUpdate,
		DeleteFunc: c.onServiceDelete,
	}))
	if err != nil {
		return nil, err
	}

	c.endpointSliceLister = discoverylisters.NewEndpointSliceLister(endpointSliceInformer.GetIndexer())
	c.endpointSlicesSynced = endpointSliceInformer.HasSynced
	_, err = endpointSliceInformer.AddEventHandler(factory.WithUpdateHandlingForObjReplace(
		// TODO: Stop ignoring mirrored EndpointSlices and add support for user-defined networks
		util.GetDefaultEndpointSlicesEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc:    c.onEndpointSliceAdd,
			UpdateFunc: c.onEndpointSliceUpdate,
			DeleteFunc: c.onEndpointSliceDelete,
		})))
	if err != nil {
		return nil, err
	}

	return c, nil
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

func (c *Controller) Run(wg *sync.WaitGroup, threadiness int) error {
	defer utilruntime.HandleCrash()

	klog.Infof("Starting Egress Services Controller")

	if !util.WaitForInformerCacheSyncWithTimeout("egressservices", c.stopCh, c.egressServiceSynced) {
		return fmt.Errorf("timed out waiting for egress service caches to sync")
	}

	if !util.WaitForInformerCacheSyncWithTimeout("egressservices_services", c.stopCh, c.servicesSynced) {
		return fmt.Errorf("timed out waiting for service caches (for egress services) to sync")
	}

	if !util.WaitForInformerCacheSyncWithTimeout("egressservices_endpointslices", c.stopCh, c.endpointSlicesSynced) {
		return fmt.Errorf("timed out waiting for endpoint slice caches (for egress services) to sync")
	}

	klog.Infof("Repairing Egress Services")
	err := c.repair()
	if err != nil {
		return fmt.Errorf("failed to repair Egress Services entries: %v", err)
	}

	for i := 0; i < threadiness; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wait.Until(func() {
				c.runEgressServiceWorker(wg)
			}, time.Second, c.stopCh)
		}()
	}

	// add shutdown goroutine waiting for c.stopCh
	wg.Add(1)
	go func() {
		defer wg.Done()
		// wait until we're told to stop
		<-c.stopCh

		klog.Infof("Shutting down Egress Services controller")
		c.egressServiceQueue.ShutDown()
	}()

	return nil
}

// Removes stale nftables/ip rules, updates the controller cache with the correct existing ones.
func (c *Controller) repair() error {
	c.Lock()
	defer c.Unlock()

	// all the current endpoints to valid egress services keys
	v4EndpointsToSvcKey := map[string]string{}
	v6EndpointsToSvcKey := map[string]string{}

	// all the current cluster ips to valid egress services keys
	cipsToSvcKey := map[string]string{}

	// all the current node ports of ETP=Local services to valid egress services keys
	nodePortsToSvcKey := map[int32]string{}

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

		if !c.shouldConfigureEgressSVC(svc, es.Status.Host) {
			continue
		}

		v4, v6, err := c.allEndpointsFor(svc, es.Status.Host == types.EgressServiceNoSNATHost)
		if err != nil {
			klog.Errorf("Failed to fetch endpoints: %v", err)
			continue
		}

		for _, ep := range v4.UnsortedList() {
			v4EndpointsToSvcKey[ep] = key
		}

		for _, ep := range v6.UnsortedList() {
			v6EndpointsToSvcKey[ep] = key
		}

		v4LB, v6LB := "", ""
		// the host being the noSNAT one means that we should not
		// configure anything related to the lbs, so we set the
		// cached lbs only if it is strictly our host.
		if es.Status.Host != types.EgressServiceNoSNATHost {
			for _, ip := range svc.Status.LoadBalancer.Ingress {
				if utilnet.IsIPv4String(ip.IP) {
					v4LB = ip.IP
					continue
				}
				v6LB = ip.IP
			}
		}

		for _, cip := range util.GetClusterIPs(svc) {
			cipsToSvcKey[cip] = key
		}

		// We care about node ports only when the return traffic involves the MASQUERADE IPs.
		if svc.Spec.ExternalTrafficPolicy == corev1.ServiceExternalTrafficPolicyLocal {
			for _, p := range svc.Spec.Ports {
				if p.NodePort == 0 {
					continue
				}
				nodePortsToSvcKey[p.NodePort] = key
			}
		}

		c.services[key] = &svcState{
			v4LB:        v4LB,
			v4Eps:       sets.New[string](),
			v6LB:        v6LB,
			v6Eps:       sets.New[string](),
			net:         es.Spec.Network,
			netEps:      sets.New[string](),
			v4NodePorts: sets.New[int32](),
			v6NodePorts: sets.New[int32](),
			stale:       false,
		}
	}

	errorList := []error{}
	err = c.repairIPRules(v4EndpointsToSvcKey, v6EndpointsToSvcKey, cipsToSvcKey, nodePortsToSvcKey)
	if err != nil {
		errorList = append(errorList, err)
	}

	err = c.repairNFTables(v4EndpointsToSvcKey, v6EndpointsToSvcKey)
	if err != nil {
		errorList = append(errorList, err)
	}

	return utilerrors.Join(errorList...)
}

// Remove stale ip rules, update caches with valid existing ones.
// Valid ip rules in this context are those that belong to an existing EgressService, their
// src points to either an existing ep or cip of the service and the routing table matches the
// Network field of the service.
func (c *Controller) repairIPRules(v4EpsToServices, v6EpsToServices, cipsToServices map[string]string, nodePortsToServices map[int32]string) error {
	type IPRule struct {
		Priority int32  `json:"priority"`
		Src      string `json:"src"`
		SrcPort  int32  `json:"sport"`
		Table    string `json:"table"`
	}

	repairRules := func(family string) error {
		epsToSvcKey := v4EpsToServices
		if family == "-6" {
			epsToSvcKey = v6EpsToServices
		}

		allIPRules := []IPRule{}
		stdout, stderr, err := util.RunIP(family, "--json", "rule", "show")
		if err != nil {
			return fmt.Errorf("could not list %s rules - stdout: %s, stderr: %s, err: %v", family, stdout, stderr, err)
		}

		err = json.Unmarshal([]byte(stdout), &allIPRules)
		if err != nil {
			return err
		}

		currEpsIPRules := []IPRule{}
		currNodePortIPRules := []IPRule{}
		for _, rule := range allIPRules {
			if rule.Priority != IPRulePriority {
				// the priority isn't the fixed one for the controller
				continue
			}

			if rule.SrcPort != 0 { // we configure source port only for ETP=Local NodePorts
				currNodePortIPRules = append(currNodePortIPRules, rule)
				continue
			}

			currEpsIPRules = append(currEpsIPRules, rule)
		}

		ipRulesToDelete := []IPRule{}
		for _, rule := range currEpsIPRules {
			svcKey, found := epsToSvcKey[rule.Src]
			if !found {
				svcKey, found = cipsToServices[rule.Src]
				if !found {
					// no service matches this ep
					ipRulesToDelete = append(ipRulesToDelete, rule)
					continue
				}
			}

			state := c.services[svcKey]
			if state == nil {
				// the rule belongs to a service that is no longer valid
				ipRulesToDelete = append(ipRulesToDelete, rule)
				continue
			}

			if state.net != rule.Table {
				// the rule points to the wrong routing table
				ipRulesToDelete = append(ipRulesToDelete, rule)
				continue
			}

			// the rule is valid, we update the service's cache to not reconfigure it later.
			state.netEps.Insert(rule.Src)
		}

		nodePortIPRulesToDelete := []IPRule{}
		for _, rule := range currNodePortIPRules {
			svcKey, found := nodePortsToServices[rule.SrcPort]
			if !found {
				nodePortIPRulesToDelete = append(nodePortIPRulesToDelete, rule)
				continue
			}
			srcIsV4Masquerade := rule.Src == config.Gateway.MasqueradeIPs.V4HostETPLocalMasqueradeIP.String()
			srcIsV6Masquerade := rule.Src == config.Gateway.MasqueradeIPs.V6HostETPLocalMasqueradeIP.String()
			if !srcIsV4Masquerade && !srcIsV6Masquerade {
				nodePortIPRulesToDelete = append(nodePortIPRulesToDelete, rule)
				continue
			}

			state := c.services[svcKey]
			if state == nil {
				// the rule belongs to a service that is no longer valid
				nodePortIPRulesToDelete = append(nodePortIPRulesToDelete, rule)
				continue
			}

			if state.net != rule.Table {
				// the rule points to the wrong routing table
				nodePortIPRulesToDelete = append(nodePortIPRulesToDelete, rule)
				continue
			}

			if srcIsV4Masquerade {
				state.v4NodePorts.Insert(rule.SrcPort)
				continue
			}
			state.v6NodePorts.Insert(rule.SrcPort)
		}

		errorList := []error{}
		for _, rule := range ipRulesToDelete {
			err := deleteIPRule(family, rule.Priority, rule.Src, rule.Table)
			if err != nil {
				errorList = append(errorList, err)
			}
		}

		for _, rule := range nodePortIPRulesToDelete {
			err := deleteNodePortIPRule(family, rule.Priority, rule.Src, rule.SrcPort, rule.Table)
			if err != nil {
				errorList = append(errorList, err)
			}
		}

		return utilerrors.Join(errorList...)
	}

	errorList := []error{}
	if config.IPv4Mode {
		err := repairRules("-4")
		if err != nil {
			errorList = append(errorList, err)
		}
	}

	if config.IPv6Mode {
		err := repairRules("-6")
		if err != nil {
			errorList = append(errorList, err)
		}
	}

	return utilerrors.Join(errorList...)
}

// Remove stale nftables map elements, update caches with valid existing ones. Ensure that
// the chain exists and checks the correct returnMark before SNATting. Valid nftables
// elements in this context are those that belong to an existing EgressService; their
// source points to an existing endpoint of the service and the SNAT matches the service's
// LB.
func (c *Controller) repairNFTables(v4EpsToServices, v6EpsToServices map[string]string) error {
	snatRepair := func(tx *knftables.Transaction, family utilnet.IPFamily, existingElements []*knftables.Element, epsToSvcs map[string]string) {
		for _, elem := range existingElements {
			if len(elem.Key) != 1 || len(elem.Value) != 1 || elem.Comment == nil {
				// the element is malformed
				tx.Delete(elem)
				continue
			}

			ep := elem.Key[0]
			lb := elem.Value[0]
			svcKey := *elem.Comment

			if svcKey != epsToSvcs[ep] {
				// the element matches the wrong service
				tx.Delete(elem)
				continue
			}

			svcState, found := c.services[svcKey]
			if !found {
				// the element matches a service that is no longer valid
				tx.Delete(elem)
				continue
			}

			lbToCompare := svcState.v4LB
			epsToAdd := svcState.v4Eps
			if family == utilnet.IPv6 {
				lbToCompare = svcState.v6LB
				epsToAdd = svcState.v6Eps
			}

			if lbToCompare != lb {
				// the element SNATs to the wrong IP
				tx.Delete(elem)
				continue
			}

			// the element is valid, we update the service's cache to not reconfigure it later.
			epsToAdd.Insert(ep)
		}
	}

	nft, err := nodenft.GetNFTablesHelper()
	if err != nil {
		return err
	}

	tx := nft.NewTransaction()
	tx.Add(&knftables.Chain{
		Name: NFTablesChainName,

		Type:     knftables.PtrTo(knftables.NATType),
		Hook:     knftables.PtrTo(knftables.PostroutingHook),
		Priority: knftables.PtrTo(knftables.SNATPriority),
	})
	tx.Flush(&knftables.Chain{
		Name: NFTablesChainName,
	})
	tx.Add(&knftables.Rule{
		Chain: NFTablesChainName,
		Rule: knftables.Concat(
			"mark", "==", c.returnMark,
			"return",
		),
		Comment: knftables.PtrTo("DoNotSNAT"),
	})

	if config.IPv4Mode {
		tx.Add(&knftables.Map{
			Name: NFTablesMapV4,
			Type: "ipv4_addr : ipv4_addr",
		})
		tx.Add(&knftables.Rule{
			Chain: NFTablesChainName,
			Rule: knftables.Concat(
				"snat to", "ip saddr map", "@", NFTablesMapV4,
			),
		})

		existing, err := nft.ListElements(context.TODO(), "map", NFTablesMapV4)
		if err != nil && !knftables.IsNotFound(err) {
			return fmt.Errorf("could not list existing egress service map elements: %w", err)
		}
		snatRepair(tx, utilnet.IPv4, existing, v4EpsToServices)
	}

	if config.IPv6Mode {
		tx.Add(&knftables.Map{
			Name: NFTablesMapV6,
			Type: "ipv6_addr : ipv6_addr",
		})
		tx.Add(&knftables.Rule{
			Chain: NFTablesChainName,
			Rule: knftables.Concat(
				"snat to", "ip6 saddr map", "@", NFTablesMapV6,
			),
		})

		existing, err := nft.ListElements(context.TODO(), "map", NFTablesMapV6)
		if err != nil && !knftables.IsNotFound(err) {
			return fmt.Errorf("could not list existing egress service map elements: %w", err)
		}
		snatRepair(tx, utilnet.IPv6, existing, v6EpsToServices)
	}

	err = nft.Run(context.TODO(), tx)
	if err != nil {
		return err
	}
	return nil
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

	err := c.syncEgressService(key)
	if err == nil {
		c.egressServiceQueue.Forget(key)
		return true
	}

	utilruntime.HandleError(fmt.Errorf("%v failed with : %v", key, err))

	if c.egressServiceQueue.NumRequeues(key) < 10 {
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
	klog.Infof("Processing sync for EgressService %s/%s", namespace, name)

	defer func() {
		klog.V(4).Infof("Finished syncing EgressService %s on namespace %s : %v", name, namespace, time.Since(startTime))
	}()

	es, err := c.egressServiceLister.EgressServices(namespace).Get(name)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}

	svc, err := c.serviceLister.Services(namespace).Get(name)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}

	cachedState := c.services[key]
	if svc == nil && cachedState == nil {
		return nil
	}

	if svc == nil && cachedState != nil {
		return c.clearServiceRulesAndRequeue(key, cachedState)
	}

	if es == nil && cachedState == nil {
		return nil
	}

	if es == nil && cachedState != nil {
		return c.clearServiceRulesAndRequeue(key, cachedState)
	}

	if cachedState != nil && cachedState.stale {
		// The service is marked stale because something failed when trying to delete it.
		// We try to delete it again before doing anything else.
		return c.clearServiceRulesAndRequeue(key, cachedState)
	}

	// At this point both the svc and es are not nil
	shouldConfigure := c.shouldConfigureEgressSVC(svc, es.Status.Host)
	if cachedState == nil && !shouldConfigure {
		return nil
	}

	if cachedState != nil && !shouldConfigure {
		return c.clearServiceRulesAndRequeue(key, cachedState)
	}

	lbsChanged := false
	v4LB, v6LB := "", ""
	// the host being the noSNAT one means that we should not
	// configure anything related to the lbs, so we set the
	// cached lbs only if it is strictly our host.
	if es.Status.Host != types.EgressServiceNoSNATHost {
		for _, ip := range svc.Status.LoadBalancer.Ingress {
			if utilnet.IsIPv4String(ip.IP) {
				v4LB = ip.IP
				continue
			}
			v6LB = ip.IP
		}
	}

	if cachedState != nil {
		lbsChanged = v4LB != cachedState.v4LB || v6LB != cachedState.v6LB
	}

	if lbsChanged {
		err := c.clearServiceSNATRules(key, cachedState)
		if err != nil {
			return err
		}
	}

	if cachedState == nil {
		cachedState = &svcState{
			v4Eps:       sets.New[string](),
			v6Eps:       sets.New[string](),
			netEps:      sets.New[string](),
			v4NodePorts: sets.New[int32](),
			v6NodePorts: sets.New[int32](),
			stale:       false,
		}
		c.services[key] = cachedState
	}
	cachedState.v4LB = v4LB
	cachedState.v6LB = v6LB

	v4Eps, v6Eps, err := c.allEndpointsFor(svc, es.Status.Host == types.EgressServiceNoSNATHost)
	if err != nil {
		return err
	}

	v4ToAdd := v4Eps.Difference(cachedState.v4Eps)
	v6ToAdd := v6Eps.Difference(cachedState.v6Eps)
	v4ToDelete := cachedState.v4Eps.Difference(v4Eps)
	v6ToDelete := cachedState.v6Eps.Difference(v6Eps)

	nft, err := nodenft.GetNFTablesHelper()
	if err != nil {
		return err
	}
	tx := nft.NewTransaction()

	if cachedState.v4LB != "" {
		for ep := range v4ToAdd {
			tx.Add(&knftables.Element{
				Map:     NFTablesMapV4,
				Key:     []string{ep},
				Value:   []string{cachedState.v4LB},
				Comment: knftables.PtrTo(key),
			})
			cachedState.v4Eps.Insert(ep)
		}

		for ep := range v4ToDelete {
			tx.Delete(&knftables.Element{
				Map: NFTablesMapV4,
				Key: []string{ep},
			})
			cachedState.v4Eps.Delete(ep)
		}
	}

	if cachedState.v6LB != "" {
		for ep := range v6ToAdd {
			tx.Add(&knftables.Element{
				Map:     NFTablesMapV6,
				Key:     []string{ep},
				Value:   []string{cachedState.v6LB},
				Comment: knftables.PtrTo(key),
			})
			cachedState.v6Eps.Insert(ep)
		}

		for ep := range v6ToDelete {
			tx.Delete(&knftables.Element{
				Map: NFTablesMapV6,
				Key: []string{ep},
			})
			cachedState.v6Eps.Delete(ep)
		}
	}

	err = nft.Run(context.TODO(), tx)
	if err != nil {
		return err
	}

	// At this point we finished handling the SNAT rules
	// Now we create the relevant ip rules according to the object's "Network"

	if es.Spec.Network != cachedState.net {
		err := c.clearServiceIPRules(cachedState)
		if err != nil {
			return err
		}
	}
	cachedState.net = es.Spec.Network

	if cachedState.net == "" {
		return nil
	}

	allEps := v4Eps.Union(v6Eps)

	for _, cip := range util.GetClusterIPs(svc) {
		allEps.Insert(cip)
	}

	ipRulesToAdd := allEps.Difference(cachedState.netEps)
	ipRulesToDelete := cachedState.netEps.Difference(allEps)

	for ip := range ipRulesToAdd {
		family := "-4"
		if utilnet.IsIPv6String(ip) {
			family = "-6"
		}

		err := createIPRule(family, IPRulePriority, ip, cachedState.net)
		if err != nil {
			return err
		}

		cachedState.netEps.Insert(ip)
	}

	for ip := range ipRulesToDelete {
		family := "-4"
		if utilnet.IsIPv6String(ip) {
			family = "-6"
		}

		err := deleteIPRule(family, IPRulePriority, ip, cachedState.net)
		if err != nil {
			return err
		}

		cachedState.netEps.Delete(ip)
	}

	// Now we create the ip rules for the MASQUERADE+NodePort.
	// This is needed only when ETP=Local, and its purpose is to ensure
	// that reply for ingress traffic uses the correct network.

	v4NodePortIPRulesToAdd := sets.New[int32]()
	v6NodePortIPRulesToAdd := sets.New[int32]()
	v4NodePortIPRulesToDelete := cachedState.v4NodePorts
	v6NodePortIPRulesToDelete := cachedState.v6NodePorts
	if svc.Spec.ExternalTrafficPolicy == corev1.ServiceExternalTrafficPolicyLocal {
		allNodePorts := sets.New[int32]()
		for _, p := range svc.Spec.Ports {
			allNodePorts.Insert(p.NodePort)
		}

		hasV4Ingress, hasV6Ingress := false, false
		for _, ip := range svc.Status.LoadBalancer.Ingress {
			if utilnet.IsIPv4String(ip.IP) {
				hasV4Ingress = true
				continue
			}
			hasV6Ingress = true
		}

		if hasV4Ingress {
			v4NodePortIPRulesToAdd = allNodePorts.Difference(cachedState.v4NodePorts)
			v4NodePortIPRulesToDelete = cachedState.v4NodePorts.Difference(allNodePorts)
		}

		if hasV6Ingress {
			v6NodePortIPRulesToAdd = allNodePorts.Difference(cachedState.v6NodePorts)
			v6NodePortIPRulesToDelete = cachedState.v6NodePorts.Difference(allNodePorts)
		}
	}

	for port := range v4NodePortIPRulesToAdd {
		err := createNodePortIPRule("-4", IPRulePriority, config.Gateway.MasqueradeIPs.V4HostETPLocalMasqueradeIP.String(), port, cachedState.net)
		if err != nil {
			return err
		}
		cachedState.v4NodePorts.Insert(port)
	}

	for port := range v4NodePortIPRulesToDelete {
		err := deleteNodePortIPRule("-4", IPRulePriority, config.Gateway.MasqueradeIPs.V4HostETPLocalMasqueradeIP.String(), port, cachedState.net)
		if err != nil {
			return err
		}
		cachedState.v4NodePorts.Delete(port)
	}

	for port := range v6NodePortIPRulesToAdd {
		err := createNodePortIPRule("-6", IPRulePriority, config.Gateway.MasqueradeIPs.V6HostETPLocalMasqueradeIP.String(), port, cachedState.net)
		if err != nil {
			return err
		}
		cachedState.v6NodePorts.Insert(port)
	}

	for port := range v6NodePortIPRulesToDelete {
		err := deleteNodePortIPRule("-6", IPRulePriority, config.Gateway.MasqueradeIPs.V6HostETPLocalMasqueradeIP.String(), port, cachedState.net)
		if err != nil {
			return err
		}
		cachedState.v6NodePorts.Delete(port)
	}

	return nil
}

// Returns all of the non-host endpoints for the given service grouped by IPv4/IPv6.
func (c *Controller) allEndpointsFor(svc *corev1.Service, localOnly bool) (sets.Set[string], sets.Set[string], error) {
	// Get the endpoint slices associated to the Service
	esLabelSelector := labels.Set(map[string]string{
		discoveryv1.LabelServiceName: svc.Name,
	}).AsSelectorPreValidated()

	endpointSlices, err := c.endpointSliceLister.EndpointSlices(svc.Namespace).List(esLabelSelector)
	if err != nil {
		return nil, nil, err
	}

	v4Endpoints := sets.New[string]()
	v6Endpoints := sets.New[string]()

	for _, eps := range endpointSlices {
		if eps.AddressType == discoveryv1.AddressTypeFQDN {
			continue
		}

		epsToInsert := v4Endpoints
		if eps.AddressType == discoveryv1.AddressTypeIPv6 {
			epsToInsert = v6Endpoints
		}

		for _, ep := range eps.Endpoints {
			if localOnly && ep.NodeName != nil && *ep.NodeName != c.thisNode {
				continue
			}
			for _, ip := range ep.Addresses {
				ipStr := utilnet.ParseIPSloppy(ip).String()
				if !services.IsHostEndpoint(ipStr) {
					epsToInsert.Insert(ipStr)
				}
			}
		}
	}

	return v4Endpoints, v6Endpoints, nil
}

// Clears all of the SNAT rules of the service.
func (c *Controller) clearServiceSNATRules(key string, state *svcState) error {
	nft, err := nodenft.GetNFTablesHelper()
	if err != nil {
		return err
	}
	tx := nft.NewTransaction()

	for ip := range state.v4Eps {
		tx.Delete(&knftables.Element{
			Map: NFTablesMapV4,
			Key: []string{ip},
		})
		state.v4Eps.Delete(ip)
	}
	state.v4LB = ""

	for ip := range state.v6Eps {
		tx.Delete(&knftables.Element{
			Map: NFTablesMapV6,
			Key: []string{ip},
		})
		state.v6Eps.Delete(ip)
	}
	state.v6LB = ""

	err = nft.Run(context.TODO(), tx)
	if err != nil {
		return err
	}
	return nil
}

// Clears all of the ip rules of the service.
func (c *Controller) clearServiceIPRules(state *svcState) error {
	errorList := []error{}
	for ip := range state.netEps {
		family := "-4"
		if utilnet.IsIPv6String(ip) {
			family = "-6"
		}

		err := deleteIPRule(family, IPRulePriority, ip, state.net)
		if err != nil {
			errorList = append(errorList, err)
			continue
		}

		state.netEps.Delete(ip)
	}

	for port := range state.v4NodePorts {
		err := deleteNodePortIPRule("-4", IPRulePriority, config.Gateway.MasqueradeIPs.V4HostETPLocalMasqueradeIP.String(), port, state.net)
		if err != nil {
			errorList = append(errorList, err)
			continue
		}
		state.v4NodePorts.Delete(port)
	}
	for port := range state.v6NodePorts {
		err := deleteNodePortIPRule("-6", IPRulePriority, config.Gateway.MasqueradeIPs.V6HostETPLocalMasqueradeIP.String(), port, state.net)
		if err != nil {
			errorList = append(errorList, err)
			continue
		}
		state.v6NodePorts.Delete(port)
	}

	return utilerrors.Join(errorList...)
}

// Clears all of the nftables rules that relate to the service and removes it from the cache.
func (c *Controller) clearServiceRulesAndRequeue(key string, state *svcState) error {
	state.stale = true

	err := c.clearServiceSNATRules(key, state)
	if err != nil {
		return err
	}

	err = c.clearServiceIPRules(state)
	if err != nil {
		return err
	}

	delete(c.services, key)
	c.egressServiceQueue.Add(key)

	return nil
}

// Returns true if the controller should configure the given service as an "Egress Service"
func (c *Controller) shouldConfigureEgressSVC(svc *corev1.Service, svcHost string) bool {
	return (svcHost == c.thisNode || svcHost == types.EgressServiceNoSNATHost) &&
		svc.Spec.Type == corev1.ServiceTypeLoadBalancer &&
		len(svc.Status.LoadBalancer.Ingress) > 0
}

// Create ip rule with the given fields.
func createIPRule(family string, priority int32, src, table string) error {
	prio := fmt.Sprintf("%d", priority)
	stdout, stderr, err := util.RunIP(family, "rule", "add", "prio", prio, "from", src, "table", table)
	if err != nil && !strings.Contains(stderr, "File exists") {
		return fmt.Errorf("could not add rule for src %s table %s - stdout: %s, stderr: %s, err: %v", src, table, stdout, stderr, err)
	}

	return nil
}

// Create ip rule with the given fields.
func createNodePortIPRule(family string, priority int32, src string, srcPort int32, table string) error {
	prio := fmt.Sprintf("%d", priority)
	sPort := fmt.Sprintf("%d", srcPort)
	stdout, stderr, err := util.RunIP(family, "rule", "add", "prio", prio, "from", src, "sport", sPort, "table", table)
	if err != nil && !strings.Contains(stderr, "File exists") {
		return fmt.Errorf("could not add rule for src %s sport %s table %s - stdout: %s, stderr: %s, err: %v", src, sPort, table, stdout, stderr, err)
	}

	return nil
}

// Delete ip rule with the given fields.
func deleteIPRule(family string, priority int32, src, table string) error {
	prio := fmt.Sprintf("%d", priority)
	stdout, stderr, err := util.RunIP(family, "rule", "del", "prio", prio, "from", src, "table", table)
	if err != nil && !strings.Contains(stderr, "No such file or directory") {
		return fmt.Errorf("could not delete rule for src %s table %s - stdout: %s, stderr: %s, err: %v", src, table, stdout, stderr, err)
	}

	return nil
}

// Delete ip rule with the given fields.
func deleteNodePortIPRule(family string, priority int32, src string, srcPort int32, table string) error {
	prio := fmt.Sprintf("%d", priority)
	sPort := fmt.Sprintf("%d", srcPort)
	stdout, stderr, err := util.RunIP(family, "rule", "del", "prio", prio, "from", src, "sport", sPort, "table", table)
	if err != nil && !strings.Contains(stderr, "No such file or directory") {
		return fmt.Errorf("could not delete rule for src %s sport %s table %s - stdout: %s, stderr: %s, err: %v", src, sPort, table, stdout, stderr, err)
	}

	return nil
}
