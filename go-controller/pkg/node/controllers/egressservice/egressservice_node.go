package egressservice

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-iptables/iptables"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	egressserviceapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressservice/v1"
	egressserviceinformer "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressservice/v1/apis/informers/externalversions/egressservice/v1"
	egressservicelisters "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressservice/v1/apis/listers/egressservice/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	nodeipt "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/iptables"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/controller/services"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/errors"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	corelisters "k8s.io/client-go/listers/core/v1"
	discoverylisters "k8s.io/client-go/listers/discovery/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
)

const (
	Chain          = "OVN-KUBE-EGRESS-SVC" // called from nat-POSTROUTING
	IPRulePriority = 5000                  // the priority of the ip rules created by the controller
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
	egressServiceQueue  workqueue.RateLimitingInterface

	serviceLister  corelisters.ServiceLister
	servicesSynced cache.InformerSynced

	endpointSliceLister  discoverylisters.EndpointSliceLister
	endpointSlicesSynced cache.InformerSynced

	services map[string]*svcState // svc key -> state
}

type svcState struct {
	v4LB   string           // IPv4 ingress of the service
	v4Eps  sets.Set[string] // v4 endpoints that have an SNAT rule configured
	v6LB   string           // IPv6 ingress of the service
	v6Eps  sets.Set[string] // v6 endpoints that have an SNAT rule configured
	net    string           // net corresponding to the spec.Network
	netEps sets.Set[string] // All endpoints that have an ip rule configured

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
	_, err = endpointSliceInformer.AddEventHandler(factory.WithUpdateHandlingForObjReplace(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.onEndpointSliceAdd,
		UpdateFunc: c.onEndpointSliceUpdate,
		DeleteFunc: c.onEndpointSliceDelete,
	}))
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

	klog.Infof("Repairing Egress Services")
	err := c.repair()
	if err != nil {
		klog.Errorf("Failed to repair Egress Services entries: %v", err)
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

	// wait until we're told to stop
	<-c.stopCh

	klog.Infof("Shutting down Egress Services controller")
	c.egressServiceQueue.ShutDown()

	wg.Wait()
}

// Removes stale iptables/ip rules, updates the controller cache with the correct existing ones.
func (c *Controller) repair() error {
	c.Lock()
	defer c.Unlock()

	// all the current endpoints to valid egress services keys
	v4EndpointsToSvcKey := map[string]string{}
	v6EndpointsToSvcKey := map[string]string{}

	// all the current cluster ips to valid egress services keys
	cipsToSvcKey := map[string]string{}

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

		v4, v6, err := c.allEndpointsFor(svc)
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
		for _, ip := range svc.Status.LoadBalancer.Ingress {
			if utilnet.IsIPv4String(ip.IP) {
				v4LB = ip.IP
				continue
			}
			v6LB = ip.IP
		}

		for _, cip := range util.GetClusterIPs(svc) {
			cipsToSvcKey[cip] = key
		}

		c.services[key] = &svcState{
			v4LB:   v4LB,
			v4Eps:  sets.New[string](),
			v6LB:   v6LB,
			v6Eps:  sets.New[string](),
			net:    es.Spec.Network,
			netEps: sets.New[string](),
			stale:  false,
		}
	}

	errorList := []error{}
	err = c.repairIPRules(v4EndpointsToSvcKey, v6EndpointsToSvcKey, cipsToSvcKey)
	if err != nil {
		errorList = append(errorList, err)
	}

	err = c.repairIPTables(v4EndpointsToSvcKey, v6EndpointsToSvcKey)
	if err != nil {
		errorList = append(errorList, err)
	}

	return errors.NewAggregate(errorList)
}

// Remove stale ip rules, update caches with valid existing ones.
// Valid ip rules in this context are those that belong to an existing EgressService, their
// src points to either an existing ep or cip of the service and the routing table matches the
// Network field of the service.
func (c *Controller) repairIPRules(v4EpsToServices, v6EpsToServices, cipsToServices map[string]string) error {
	type IPRule struct {
		Priority int32  `json:"priority"`
		Src      string `json:"src"`
		Table    string `json:"table"`
	}

	repairRules := func(family string) error {
		epsToSvcKey := v4EpsToServices
		if family == "-6" {
			epsToSvcKey = v6EpsToServices
		}

		allIPRules := []IPRule{}
		ipRulesToDelete := []IPRule{}

		stdout, stderr, err := util.RunIP(family, "--json", "rule", "show")
		if err != nil {
			return fmt.Errorf("could not list %s rules - stdout: %s, stderr: %s, err: %v", family, stdout, stderr, err)
		}

		err = json.Unmarshal([]byte(stdout), &allIPRules)
		if err != nil {
			return err
		}

		for _, rule := range allIPRules {
			if rule.Priority != IPRulePriority {
				// the priority isn't the fixed one for the controller
				continue
			}

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

		errorList := []error{}
		for _, rule := range ipRulesToDelete {
			err := deleteIPRule(family, rule.Priority, rule.Src, rule.Table)
			if err != nil {
				errorList = append(errorList, err)
			}
		}

		return errors.NewAggregate(errorList)
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

	return errors.NewAggregate(errorList)
}

// Remove stale iptables rules, update caches with valid existing ones.
// In addition, verify that the first rule in the Chain matches the "returnMark":
// packets coming with that mark should not be evaluated for SNATing = we make sure
// the first rule in the Chain is a RETURN one for this mark.
// Valid iptables rules in this context are those that belong to an existing EgressService, their
// source points to an existing ep the service and the SNAT matches the service's LB.
func (c *Controller) repairIPTables(v4EpsToServices, v6EpsToServices map[string]string) error {
	type esvcIPTRule struct {
		svcKey string
		ep     string
		lb     string
		mark   int32
	}

	parseIPTRule := func(rule string) (*esvcIPTRule, error) {
		split := strings.Fields(rule)
		if len(split)%2 != 0 {
			return nil, fmt.Errorf("expected rule %s to have a key-value format", rule)
		}

		args := map[string]string{}
		for i := 0; i < len(split); i += 2 {
			args[split[i]] = split[i+1]
		}

		svcKey := args["--comment"]
		ep := args["-s"]
		lb := args["--to-source"]

		strMark := args["--mark"]
		mark := int32(0)
		if strMark != "" {
			parsedFWMark, err := strconv.ParseInt(strMark, 0, 32)
			if err != nil {
				return nil, fmt.Errorf("could not parse fwmark for rule %s, err: %w", rule, err)
			}
			mark = int32(parsedFWMark)
		}

		return &esvcIPTRule{svcKey: svcKey, ep: ep, lb: lb, mark: mark}, nil
	}

	snatRepair := func(proto iptables.Protocol, epsToSvcs map[string]string) error {
		defaultFirstRule := c.defaultReturnRule(proto) // fetch the rule that should be first in the chain

		ipt, err := util.GetIPTablesHelper(proto)
		if err != nil {
			return err
		}

		snatRules, err := ipt.List("nat", Chain)
		if err != nil {
			return err
		}

		// we verify that the first rule is the "do not snat" one
		if len(snatRules) == 0 {
			return nodeipt.AddRules([]nodeipt.Rule{defaultFirstRule}, true)
		}

		parsedFWMark, err := strconv.ParseInt(c.returnMark, 0, 0)
		if err != nil {
			return fmt.Errorf("could not parse %s as default return mark for egress services, err: %w", c.returnMark, err)
		}
		mark := int32(parsedFWMark)

		firstRule := snatRules[0]
		parsed, err := parseIPTRule(firstRule)
		doNotSNATAdded := false
		if err != nil || parsed.mark != mark {
			// The first rule is either malformed (err != nil) or it does not match
			// the correct mark. In either case, we should add the correct rule anyways
			// to be sure it is the first one.
			err := nodeipt.AddRules([]nodeipt.Rule{defaultFirstRule}, false)
			if err != nil {
				return err
			}
			doNotSNATAdded = true
		}
		if !doNotSNATAdded {
			// the do not snat rule was already present at the first position
			snatRules = snatRules[1:]
		}

		// now we run over the existing rules to determine which should be deleted
		// and update the cache accordingly
		rulesToDel := []string{}
		for _, rule := range snatRules {
			parsed, err := parseIPTRule(rule)
			if err != nil {
				// the rule is malformed
				rulesToDel = append(rulesToDel, rule)
				continue
			}

			svcKey := epsToSvcs[parsed.ep]
			if svcKey != parsed.svcKey {
				// the rule matches the wrong service
				rulesToDel = append(rulesToDel, rule)
				continue
			}

			svcState, found := c.services[svcKey]
			if !found {
				// the rule matches a service that is no longer valid
				rulesToDel = append(rulesToDel, rule)
				continue
			}

			lbToCompare := svcState.v4LB
			epsToAdd := svcState.v4Eps
			if proto == iptables.ProtocolIPv6 {
				lbToCompare = svcState.v6LB
				epsToAdd = svcState.v6Eps
			}

			if lbToCompare != parsed.lb {
				// the rule SNATs to the wrong IP
				rulesToDel = append(rulesToDel, rule)
				continue
			}

			// the rule is valid, we update the service's cache to not reconfigure it later.
			epsToAdd.Insert(parsed.ep)
		}

		errorList := []error{}
		for _, rule := range rulesToDel {
			args := strings.Fields(rule)
			err := ipt.Delete("nat", Chain, args...)
			if err != nil {
				errorList = append(errorList, err)
			}
		}

		return errors.NewAggregate(errorList)
	}

	errorList := []error{}
	if config.IPv4Mode {
		ipt, err := util.GetIPTablesHelper(iptables.ProtocolIPv4)
		if err != nil {
			errorList = append(errorList, err)
		}

		err = ipt.NewChain("nat", Chain)
		if err != nil {
			klog.V(5).Infof("Could not create egress service nat chain: %v", err)
		}

		err = snatRepair(iptables.ProtocolIPv4, v4EpsToServices)
		if err != nil {
			errorList = append(errorList, err)
		}

	}

	if config.IPv6Mode {
		ipt, err := util.GetIPTablesHelper(iptables.ProtocolIPv4)
		if err != nil {
			errorList = append(errorList, err)
		}

		err = ipt.NewChain("nat", Chain)
		if err != nil {
			klog.V(5).Infof("Could not create egress service nat chain: %v", err)
		}

		err = snatRepair(iptables.ProtocolIPv6, v6EpsToServices)
		if err != nil {
			errorList = append(errorList, err)
		}
	}

	return errors.NewAggregate(errorList)
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
	for _, ip := range svc.Status.LoadBalancer.Ingress {
		if utilnet.IsIPv4String(ip.IP) {
			v4LB = ip.IP
			continue
		}
		v6LB = ip.IP
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
			v4Eps:  sets.New[string](),
			v6Eps:  sets.New[string](),
			netEps: sets.New[string](),
			stale:  false,
		}
		c.services[key] = cachedState
	}
	cachedState.v4LB = v4LB
	cachedState.v6LB = v6LB

	v4Eps, v6Eps, err := c.allEndpointsFor(svc)
	if err != nil {
		return err
	}

	v4ToAdd := v4Eps.Difference(cachedState.v4Eps)
	v6ToAdd := v6Eps.Difference(cachedState.v6Eps)
	v4ToDelete := cachedState.v4Eps.Difference(v4Eps)
	v6ToDelete := cachedState.v6Eps.Difference(v6Eps)

	if cachedState.v4LB != "" {
		for ep := range v4ToAdd {
			err := nodeipt.AddRules([]nodeipt.Rule{snatIPTRuleFor(key, cachedState.v4LB, ep)}, true)
			if err != nil {
				return err
			}
			cachedState.v4Eps.Insert(ep)
		}

		for ep := range v4ToDelete {
			err := nodeipt.DelRules([]nodeipt.Rule{snatIPTRuleFor(key, cachedState.v4LB, ep)})
			if err != nil {
				return err
			}

			cachedState.v4Eps.Delete(ep)
		}
	}

	if cachedState.v6LB != "" {
		for ep := range v6ToAdd {
			err := nodeipt.AddRules([]nodeipt.Rule{snatIPTRuleFor(key, cachedState.v6LB, ep)}, true)
			if err != nil {
				return err
			}

			cachedState.v6Eps.Insert(ep)
		}

		for ep := range v6ToDelete {
			err := nodeipt.DelRules([]nodeipt.Rule{snatIPTRuleFor(key, cachedState.v6LB, ep)})
			if err != nil {
				return err
			}

			cachedState.v6Eps.Delete(ep)
		}
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

	return nil
}

// Returns all of the non-host endpoints for the given service grouped by IPv4/IPv6.
func (c *Controller) allEndpointsFor(svc *corev1.Service) (sets.Set[string], sets.Set[string], error) {
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
	for ip := range state.v4Eps {
		err := nodeipt.DelRules([]nodeipt.Rule{snatIPTRuleFor(key, state.v4LB, ip)})
		if err != nil {
			return err
		}

		state.v4Eps.Delete(ip)
	}
	state.v4LB = ""

	for ip := range state.v6Eps {
		err := nodeipt.DelRules([]nodeipt.Rule{snatIPTRuleFor(key, state.v6LB, ip)})
		if err != nil {
			return err
		}

		state.v6Eps.Delete(ip)
	}
	state.v6LB = ""

	return nil
}

// Clears all of the FWMark rules of the service.
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

	return errors.NewAggregate(errorList)
}

// Clears all of the iptables rules that relate to the service and removes it from the cache.
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
	return svcHost == c.thisNode &&
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

// Delete ip rule with the given fields.
func deleteIPRule(family string, priority int32, src, table string) error {
	prio := fmt.Sprintf("%d", priority)
	stdout, stderr, err := util.RunIP(family, "rule", "del", "prio", prio, "from", src, "table", table)
	if err != nil && !strings.Contains(stderr, "No such file or directory") {
		return fmt.Errorf("could not delete rule for src %s table %s - stdout: %s, stderr: %s, err: %v", src, table, stdout, stderr, err)
	}

	return nil
}

// Returns the SNAT rule that should be created for the given lb/endpoint
func snatIPTRuleFor(comment string, lb, ip string) nodeipt.Rule {
	return nodeipt.Rule{
		Table: "nat",
		Chain: Chain,
		Args: []string{
			"-s", ip,
			"-m", "comment", "--comment", comment,
			"-j", "SNAT",
			"--to-source", lb,
		},
		Protocol: getIPTablesProtocol(ip),
	}
}

// getIPTablesProtocol returns the IPTables protocol matching the protocol (v4/v6) of provided IP string
func getIPTablesProtocol(ip string) iptables.Protocol {
	if utilnet.IsIPv6String(ip) {
		return iptables.ProtocolIPv6
	}
	return iptables.ProtocolIPv4
}

// Returns the rule that should be first in the Chain.
// Packets coming with the controller's "returnMark" should not be evaluated for SNATing.
// The rule here is a "RETURN" for these packets.
func (c *Controller) defaultReturnRule(proto iptables.Protocol) nodeipt.Rule {
	return nodeipt.Rule{
		Table: "nat",
		Chain: Chain,
		Args: []string{
			"-m", "mark", "--mark", string(c.returnMark),
			"-m", "comment", "--comment", "DoNotSNAT",
			"-j", "RETURN",
		},
		Protocol: proto,
	}
}
