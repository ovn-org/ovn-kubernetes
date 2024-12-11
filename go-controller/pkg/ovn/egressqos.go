package ovn

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/ovn-org/libovsdb/ovsdb"
	kapi "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	metaapplyv1 "k8s.io/client-go/applyconfigurations/meta/v1"
	v1coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
	"k8s.io/utils/ptr"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	egressqosapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressqos/v1"
	egressqosapply "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressqos/v1/apis/applyconfiguration/egressqos/v1"
	egressqosinformer "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressqos/v1/apis/informers/externalversions/egressqos/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	utilerrors "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/errors"
)

const (
	defaultEgressQoSName       = "default"
	EgressQoSFlowStartPriority = 1000
	egressQoSAppliedCorrectly  = "EgressQoS Rules applied"
	egressQoSReadyStatusType   = "Ready-In-Zone-"
	egressQoSReadyReason       = "SetupSucceeded"
	egressQoSNotReadyReason    = "SetupFailed"
)

var maxEgressQoSRetries = 10

type egressQoS struct {
	sync.RWMutex
	name      string
	namespace string
	rules     []*egressQoSRule
	stale     bool
}

type egressQoSRule struct {
	priority    int
	dscp        int
	destination string
	addrSet     addressset.AddressSet
	pods        *sync.Map // pods name -> ips in the addrSet
	podSelector metav1.LabelSelector
}

func getEgressQosAddrSetDbIDs(namespace, priority, controller string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.AddressSetEgressQoS, controller, map[libovsdbops.ExternalIDKey]string{
		libovsdbops.ObjectNameKey: namespace,
		// priority is the unique id for address set within given namespace
		libovsdbops.PriorityKey: priority,
	})
}

func getEgressQoSRuleDbIDs(namespace string, rulePriority int) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.QoSEgressQoS, DefaultNetworkControllerName, map[libovsdbops.ExternalIDKey]string{
		libovsdbops.ObjectNameKey: namespace,
		libovsdbops.PriorityKey:   fmt.Sprintf("%d", rulePriority),
	})
}

// shallow copies the EgressQoS object provided.
func (oc *DefaultNetworkController) cloneEgressQoS(raw *egressqosapi.EgressQoS) (*egressQoS, error) {
	eq := &egressQoS{
		name:      raw.Name,
		namespace: raw.Namespace,
		rules:     make([]*egressQoSRule, 0),
	}

	if len(raw.Spec.Egress) > EgressQoSFlowStartPriority {
		return nil, fmt.Errorf("cannot create EgressQoS with %d rules - maximum is %d", len(raw.Spec.Egress), EgressQoSFlowStartPriority)
	}

	var errs []error
	for i, rule := range raw.Spec.Egress {
		eqr, err := oc.cloneEgressQoSRule(rule, EgressQoSFlowStartPriority-i)
		if err != nil {
			dst := "any"
			if rule.DstCIDR != nil {
				dst = *rule.DstCIDR
			}
			err = fmt.Errorf("cannot create egressqos Rule to destination %s for namespace %s: %w", dst, eq.namespace, err)
			errs = append(errs, err)
			continue
		}
		eq.rules = append(eq.rules, eqr)
	}

	return eq, utilerrors.Join(errs...)
}

// shallow copies the EgressQoSRule object provided.
func (oc *DefaultNetworkController) cloneEgressQoSRule(raw egressqosapi.EgressQoSRule, priority int) (*egressQoSRule, error) {
	dst := ""
	if raw.DstCIDR != nil {
		_, _, err := net.ParseCIDR(*raw.DstCIDR)
		if err != nil {
			return nil, err
		}
		dst = *raw.DstCIDR
	}

	_, err := metav1.LabelSelectorAsSelector(&raw.PodSelector)
	if err != nil {
		return nil, err
	}

	eqr := &egressQoSRule{
		priority:    priority,
		dscp:        raw.DSCP,
		destination: dst,
		podSelector: raw.PodSelector,
	}

	return eqr, nil
}

func (oc *DefaultNetworkController) createASForEgressQoSRule(podSelector metav1.LabelSelector, namespace string, priority int) (addressset.AddressSet, *sync.Map, error) {
	var addrSet addressset.AddressSet

	selector, err := metav1.LabelSelectorAsSelector(&podSelector)
	if err != nil {
		return nil, nil, err
	}
	if selector.Empty() { // empty selector means that the rule applies to all pods in the namespace
		asIndex := getNamespaceAddrSetDbIDs(namespace, oc.controllerName)
		addrSet, err := oc.addressSetFactory.EnsureAddressSet(asIndex)
		if err != nil {
			return nil, nil, fmt.Errorf("cannot ensure that addressSet for namespace %s exists %v", namespace, err)
		}
		return addrSet, &sync.Map{}, nil
	}

	podsCache := sync.Map{}

	pods, err := oc.watchFactory.GetPodsBySelector(namespace, podSelector)
	if err != nil {
		return nil, nil, err
	}
	asIndex := getEgressQosAddrSetDbIDs(namespace, fmt.Sprintf("%d", priority), oc.controllerName)
	addrSet, err = oc.addressSetFactory.EnsureAddressSet(asIndex)
	if err != nil {
		return nil, nil, err
	}
	podsIps := []net.IP{}
	for _, pod := range pods {
		// we don't handle HostNetworked or completed pods or not-scheduled pods or remote-zone pods
		if !util.PodWantsHostNetwork(pod) && !util.PodCompleted(pod) && util.PodScheduled(pod) && oc.isPodScheduledinLocalZone(pod) {
			podIPs, err := util.GetPodIPsOfNetwork(pod, oc.GetNetInfo())
			if err != nil && !errors.Is(err, util.ErrNoPodIPFound) {
				return nil, nil, err
			}
			podsCache.Store(pod.Name, podIPs)
			podsIps = append(podsIps, podIPs...)
		}
	}
	err = addrSet.SetAddresses(util.StringSlice(podsIps))
	if err != nil {
		return nil, nil, err
	}

	return addrSet, &podsCache, nil
}

// initEgressQoSController initializes the EgressQoS controller.
func (oc *DefaultNetworkController) initEgressQoSController(
	eqInformer egressqosinformer.EgressQoSInformer,
	podInformer v1coreinformers.PodInformer,
	nodeInformer v1coreinformers.NodeInformer) error {
	klog.Info("Setting up event handlers for EgressQoS")
	oc.egressQoSLister = eqInformer.Lister()
	oc.egressQoSSynced = eqInformer.Informer().HasSynced
	oc.egressQoSQueue = workqueue.NewTypedRateLimitingQueueWithConfig(
		workqueue.NewTypedItemFastSlowRateLimiter[string](1*time.Second, 5*time.Second, 5),
		workqueue.TypedRateLimitingQueueConfig[string]{Name: "egressqos"},
	)
	_, err := eqInformer.Informer().AddEventHandler(factory.WithUpdateHandlingForObjReplace(cache.ResourceEventHandlerFuncs{
		AddFunc:    oc.onEgressQoSAdd,
		UpdateFunc: oc.onEgressQoSUpdate,
		DeleteFunc: oc.onEgressQoSDelete,
	}))
	if err != nil {
		return fmt.Errorf("could not add Event Handler for eqInformer during egressqosController initialization, %w", err)

	}

	oc.egressQoSPodLister = podInformer.Lister()
	oc.egressQoSPodSynced = podInformer.Informer().HasSynced
	oc.egressQoSPodQueue = workqueue.NewTypedRateLimitingQueueWithConfig(
		workqueue.NewTypedItemFastSlowRateLimiter[string](1*time.Second, 5*time.Second, 5),
		workqueue.TypedRateLimitingQueueConfig[string]{Name: "egressqospods"},
	)
	_, err = podInformer.Informer().AddEventHandler(factory.WithUpdateHandlingForObjReplace(cache.ResourceEventHandlerFuncs{
		AddFunc:    oc.onEgressQoSPodAdd,
		UpdateFunc: oc.onEgressQoSPodUpdate,
		DeleteFunc: oc.onEgressQoSPodDelete,
	}))
	if err != nil {
		return fmt.Errorf("could not add Event Handler for podInformer during egressqosController initialization, %w", err)
	}

	oc.egressQoSNodeLister = nodeInformer.Lister()
	oc.egressQoSNodeSynced = nodeInformer.Informer().HasSynced
	oc.egressQoSNodeQueue = workqueue.NewTypedRateLimitingQueueWithConfig(
		workqueue.NewTypedItemFastSlowRateLimiter[string](1*time.Second, 5*time.Second, 5),
		workqueue.TypedRateLimitingQueueConfig[string]{Name: "egressqosnodes"},
	)
	_, err = nodeInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    oc.onEgressQoSNodeAdd,    // we only care about new logical switches being added
		UpdateFunc: oc.onEgressQoSNodeUpdate, // we care about node's zone changes so that if add event didn't do anything update can take care of it
		DeleteFunc: func(obj interface{}) {},
	})
	if err != nil {
		return fmt.Errorf("could not add Event Handler for nodeInformer during egressqosController initialization, %w", err)
	}
	return nil
}

func (oc *DefaultNetworkController) runEgressQoSController(wg *sync.WaitGroup, threadiness int, stopCh <-chan struct{}) error {
	defer utilruntime.HandleCrash()

	klog.Infof("Starting EgressQoS Controller")

	if !util.WaitForInformerCacheSyncWithTimeout("egressqosnodes", stopCh, oc.egressQoSNodeSynced) {
		return fmt.Errorf("timed out waiting for egress QoS node caches to sync")
	}

	if !util.WaitForInformerCacheSyncWithTimeout("egressqospods", stopCh, oc.egressQoSPodSynced) {
		return fmt.Errorf("timed out waiting for egress QoS pods caches to sync")
	}

	if !util.WaitForInformerCacheSyncWithTimeout("egressqos", stopCh, oc.egressQoSSynced) {
		return fmt.Errorf("timed out waiting for egress QoS caches to sync")
	}

	klog.Infof("Repairing EgressQoSes")
	err := oc.repairEgressQoSes()
	if err != nil {
		return fmt.Errorf("failed to delete stale EgressQoS entries: %v", err)
	}

	for i := 0; i < threadiness; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wait.Until(func() {
				oc.runEgressQoSWorker(wg)
			}, time.Second, stopCh)
		}()
	}

	for i := 0; i < threadiness; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wait.Until(func() {
				oc.runEgressQoSPodWorker(wg)
			}, time.Second, stopCh)
		}()
	}

	for i := 0; i < threadiness; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wait.Until(func() {
				oc.runEgressQoSNodeWorker(wg)
			}, time.Second, stopCh)
		}()
	}

	// add shutdown goroutine waiting for stopCh
	wg.Add(1)
	go func() {
		defer wg.Done()
		// wait until we're told to stop
		<-stopCh

		klog.Infof("Shutting down EgressQoS controller")
		oc.egressQoSQueue.ShutDown()
		oc.egressQoSPodQueue.ShutDown()
		oc.egressQoSNodeQueue.ShutDown()
	}()

	return nil
}

// onEgressQoSAdd queues the EgressQoS for processing.
func (oc *DefaultNetworkController) onEgressQoSAdd(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", obj, err))
		return
	}
	oc.egressQoSQueue.Add(key)
}

// onEgressQoSUpdate queues the EgressQoS for processing.
func (oc *DefaultNetworkController) onEgressQoSUpdate(oldObj, newObj interface{}) {
	oldEQ := oldObj.(*egressqosapi.EgressQoS)
	newEQ := newObj.(*egressqosapi.EgressQoS)

	if oldEQ.ResourceVersion == newEQ.ResourceVersion ||
		!newEQ.GetDeletionTimestamp().IsZero() {
		return
	}

	key, err := cache.MetaNamespaceKeyFunc(newObj)
	if err == nil {
		oc.egressQoSQueue.Add(key)
	}
}

// onEgressQoSDelete queues the EgressQoS for processing.
func (oc *DefaultNetworkController) onEgressQoSDelete(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", obj, err))
		return
	}
	oc.egressQoSQueue.Add(key)
}

func (oc *DefaultNetworkController) runEgressQoSWorker(wg *sync.WaitGroup) {
	for oc.processNextEgressQoSWorkItem(wg) {
	}
}

func (oc *DefaultNetworkController) processNextEgressQoSWorkItem(wg *sync.WaitGroup) bool {
	wg.Add(1)
	defer wg.Done()

	key, quit := oc.egressQoSQueue.Get()
	if quit {
		return false
	}

	defer oc.egressQoSQueue.Done(key)

	eq, err := oc.getEgressQoS(key)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("failed to retrieve %s qos object: %v", key, err))
		oc.egressQoSQueue.Forget(key)
		return true
	}

	err = oc.syncEgressQoS(key, eq)
	if err == nil {
		oc.egressQoSQueue.Forget(key)
		if err = oc.updateEgressQoSZoneStatusToReady(eq); err != nil {
			utilruntime.HandleError(fmt.Errorf("failed to update EgressQoS object %s with status: %v", key, err))
		}
		return true
	}

	utilruntime.HandleError(fmt.Errorf("%v failed with : %v", key, err))

	if oc.egressQoSQueue.NumRequeues(key) < maxEgressQoSRetries {
		oc.egressQoSQueue.AddRateLimited(key)
		return true
	}

	if err = oc.updateEgressQoSZoneStatusToNotReady(eq, err); err != nil {
		utilruntime.HandleError(fmt.Errorf("failed to update EgressQoS object %s with status: %v", key, err))
	}

	oc.egressQoSQueue.Forget(key)
	return true
}

// This takes care of syncing stale data which we might have in OVN if
// there's no ovnkube-master running for a while.
// It deletes all QoSes and Address Sets from OVN that belong to deleted EgressQoSes.
func (oc *DefaultNetworkController) repairEgressQoSes() error {
	startTime := time.Now()
	klog.V(4).Infof("Starting repairing loop for egressqos")
	defer func() {
		klog.V(4).Infof("Finished repairing loop for egressqos: %v", time.Since(startTime))
	}()

	existing, err := oc.egressQoSLister.List(labels.Everything())
	if err != nil {
		return err
	}

	nsWithQoS := map[string]bool{}
	for _, q := range existing {
		nsWithQoS[q.Namespace] = true
	}
	predicateIDs := libovsdbops.NewDbObjectIDs(libovsdbops.QoSEgressQoS, oc.controllerName, nil)
	predicateQoSFunc := func(q *nbdb.QoS) bool {
		// ObjectNameKey is namespace
		return !nsWithQoS[q.ExternalIDs[libovsdbops.ObjectNameKey.String()]]
	}
	qPredicate := libovsdbops.GetPredicate[*nbdb.QoS](predicateIDs, predicateQoSFunc)
	existingQoSes, err := libovsdbops.FindQoSesWithPredicate(oc.nbClient, qPredicate)
	if err != nil {
		return err
	}

	if len(existingQoSes) > 0 {
		allOps := []ovsdb.Operation{}

		logicalSwitches, err := oc.egressQoSSwitches()
		if err != nil {
			return err
		}

		for _, sw := range logicalSwitches {
			ops, err := libovsdbops.RemoveQoSesFromLogicalSwitchOps(oc.nbClient, nil, sw, existingQoSes...)
			if err != nil {
				return err
			}
			allOps = append(allOps, ops...)
		}

		if _, err := libovsdbops.TransactAndCheck(oc.nbClient, allOps); err != nil {
			return fmt.Errorf("unable to remove stale qoses, err: %v", err)
		}
	}
	predicateIDs = libovsdbops.NewDbObjectIDs(libovsdbops.AddressSetEgressQoS, oc.controllerName, nil)
	predicateFunc := func(as *nbdb.AddressSet) bool {
		// ObjectNameKey is namespace
		return !nsWithQoS[as.ExternalIDs[libovsdbops.ObjectNameKey.String()]]
	}
	asPredicate := libovsdbops.GetPredicate[*nbdb.AddressSet](predicateIDs, predicateFunc)
	if err := libovsdbops.DeleteAddressSetsWithPredicate(oc.nbClient, asPredicate); err != nil {
		return fmt.Errorf("failed to remove stale egress qos address sets, err: %v", err)
	}

	return nil
}

func (oc *DefaultNetworkController) syncEgressQoS(key string, eq *egressqosapi.EgressQoS) error {
	startTime := time.Now()
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}
	klog.Infof("Processing sync for EgressQoS %s/%s", namespace, name)

	defer func() {
		klog.V(4).Infof("Finished syncing EgressQoS %s on namespace %s : %v", name, namespace, time.Since(startTime))
	}()

	if name != defaultEgressQoSName {
		klog.Errorf("EgressQoS name %s is invalid, must be %s", name, defaultEgressQoSName)
		return nil // Return nil to avoid requeues
	}

	// TODO: we should reconcile better by cleaning and creating in one transaction.
	// that should minimize the window of lost DSCP markings on packets.
	err = oc.cleanEgressQoSNS(namespace)
	if err != nil {
		return fmt.Errorf("unable to delete EgressQoS %s/%s, err: %v", namespace, name, err)
	}

	if eq == nil { // it was deleted no need to process further
		return nil
	}

	klog.V(5).Infof("EgressQoS %s retrieved from lister: %v", eq.Name, eq)

	return oc.addEgressQoS(eq)
}

func (oc *DefaultNetworkController) getEgressQoS(key string) (*egressqosapi.EgressQoS, error) {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return nil, err
	}
	var eq *egressqosapi.EgressQoS
	eq, err = oc.egressQoSLister.EgressQoSes(namespace).Get(name)
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, err
	}
	return eq, nil
}

func (oc *DefaultNetworkController) cleanEgressQoSNS(namespace string) error {
	obj, loaded := oc.egressQoSCache.Load(namespace)
	if !loaded {
		// the namespace is clean
		klog.V(4).Infof("EgressQoS for namespace %s not found in cache", namespace)
		return nil
	}

	eq := obj.(*egressQoS)

	eq.Lock()
	defer eq.Unlock()
	predicateIDs := libovsdbops.NewDbObjectIDs(libovsdbops.QoSEgressQoS, oc.controllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: eq.namespace,
		})
	qPredicate := libovsdbops.GetPredicate[*nbdb.QoS](predicateIDs, nil)
	existingQoSes, err := libovsdbops.FindQoSesWithPredicate(oc.nbClient, qPredicate)
	if err != nil {
		return err
	}

	if len(existingQoSes) > 0 {
		allOps := []ovsdb.Operation{}

		ops, err := libovsdbops.DeleteQoSesOps(oc.nbClient, nil, existingQoSes...)
		if err != nil {
			return err
		}
		allOps = append(allOps, ops...)

		logicalSwitches, err := oc.egressQoSSwitches()
		if err != nil {
			return err
		}

		for _, sw := range logicalSwitches {
			ops, err := libovsdbops.RemoveQoSesFromLogicalSwitchOps(oc.nbClient, nil, sw, existingQoSes...)
			if err != nil {
				return err
			}
			allOps = append(allOps, ops...)
		}

		if _, err := libovsdbops.TransactAndCheck(oc.nbClient, allOps); err != nil {
			return fmt.Errorf("failed to delete qos, err: %s", err)
		}
	}
	predicateIDs = libovsdbops.NewDbObjectIDs(libovsdbops.AddressSetEgressQoS, oc.controllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: eq.namespace,
		})
	asPredicate := libovsdbops.GetPredicate[*nbdb.AddressSet](predicateIDs, nil)
	if err := libovsdbops.DeleteAddressSetsWithPredicate(oc.nbClient, asPredicate); err != nil {
		return fmt.Errorf("failed to remove egress qos address sets, err: %v", err)
	}

	// we can delete the object from the cache now.
	// we also mark it as stale to prevent pod processing if RLock
	// acquired after removal from cache.
	oc.egressQoSCache.Delete(namespace)
	eq.stale = true

	return nil
}

func (oc *DefaultNetworkController) addEgressQoS(eqObj *egressqosapi.EgressQoS) error {
	eq, err := oc.cloneEgressQoS(eqObj)
	if err != nil {
		return err
	}

	eq.Lock()
	defer eq.Unlock()
	eq.stale = true // until we finish processing successfully

	// there should not be an item in the cache for the given namespace
	// as we first attempt to delete before create.
	if _, loaded := oc.egressQoSCache.LoadOrStore(eq.namespace, eq); loaded {
		return fmt.Errorf("error attempting to add egressQoS %s to namespace %s when it already has an EgressQoS",
			eq.name, eq.namespace)
	}

	for _, rule := range eq.rules {
		rule.addrSet, rule.pods, err = oc.createASForEgressQoSRule(rule.podSelector, eq.namespace, rule.priority)
		if err != nil {
			return err
		}
	}

	logicalSwitches, err := oc.egressQoSSwitches()
	if err != nil {
		return err
	}

	allOps := []ovsdb.Operation{}
	qoses := []*nbdb.QoS{}
	for _, r := range eq.rules {
		hashedIPv4, hashedIPv6 := r.addrSet.GetASHashNames()
		match := generateEgressQoSMatch(r, hashedIPv4, hashedIPv6)
		qos := &nbdb.QoS{
			Direction:   nbdb.QoSDirectionToLport,
			Match:       match,
			Priority:    r.priority,
			Action:      map[string]int{nbdb.QoSActionDSCP: r.dscp},
			ExternalIDs: getEgressQoSRuleDbIDs(eq.namespace, r.priority).GetExternalIDs(),
		}
		qoses = append(qoses, qos)
	}

	ops, err := libovsdbops.CreateOrUpdateQoSesOps(oc.nbClient, nil, qoses...)
	if err != nil {
		return err
	}
	allOps = append(allOps, ops...)

	for _, sw := range logicalSwitches {
		ops, err := libovsdbops.AddQoSesToLogicalSwitchOps(oc.nbClient, nil, sw, qoses...)
		if err != nil {
			return err
		}
		allOps = append(allOps, ops...)
	}

	if _, err := libovsdbops.TransactAndCheck(oc.nbClient, allOps); err != nil {
		return fmt.Errorf("failed to create qos, err: %s", err)
	}

	eq.stale = false // we can mark it as "ready" now
	return nil
}

func generateEgressQoSMatch(eq *egressQoSRule, hashedAddressSetNameIPv4, hashedAddressSetNameIPv6 string) string {
	var src string
	var dst string

	switch {
	case config.IPv4Mode && config.IPv6Mode:
		src = fmt.Sprintf("(ip4.src == $%s || ip6.src == $%s)", hashedAddressSetNameIPv4, hashedAddressSetNameIPv6)
	case config.IPv4Mode:
		src = fmt.Sprintf("ip4.src == $%s", hashedAddressSetNameIPv4)
	case config.IPv6Mode:
		src = fmt.Sprintf("ip6.src == $%s", hashedAddressSetNameIPv6)
	}

	dst = "ip4.dst == 0.0.0.0/0 || ip6.dst == ::/0" // if the dstCIDR field was not set we treat it as "any" destination
	if eq.destination != "" {
		dst = fmt.Sprintf("ip4.dst == %s", eq.destination)
		if utilnet.IsIPv6CIDRString(eq.destination) {
			dst = fmt.Sprintf("ip6.dst == %s", eq.destination)
		}
	}

	return fmt.Sprintf("(%s) && %s", dst, src)
}

func (oc *DefaultNetworkController) egressQoSSwitches() ([]string, error) {
	logicalSwitches := []string{}

	// Find all node switches
	p := func(item *nbdb.LogicalSwitch) bool {
		// Ignore external and Join switches(both legacy and current)
		return !(strings.HasPrefix(item.Name, types.JoinSwitchPrefix) || oc.RemoveNetworkScopeFromName(item.Name) == types.OVNJoinSwitch || item.Name == types.TransitSwitch || strings.HasPrefix(item.Name, types.ExternalSwitchPrefix))
	}

	nodeLocalSwitches, err := libovsdbops.FindLogicalSwitchesWithPredicate(oc.nbClient, p)
	if err != nil {
		return nil, fmt.Errorf("unable to fetch local switches for EgressQoS, err: %v", err)
	}

	for _, nodeLocalSwitch := range nodeLocalSwitches {
		logicalSwitches = append(logicalSwitches, nodeLocalSwitch.Name)
	}

	return logicalSwitches, nil
}

type mapOp int

const (
	mapInsert mapOp = iota
	mapDelete
)

type mapAndOp struct {
	m  *sync.Map
	op mapOp
}

func (oc *DefaultNetworkController) syncEgressQoSPod(key string) error {
	startTime := time.Now()
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}

	obj, loaded := oc.egressQoSCache.Load(namespace)
	if !loaded { // no EgressQoS in the namespace
		return nil
	}

	klog.V(5).Infof("Processing sync for EgressQoS pod %s/%s", namespace, name)

	defer func() {
		klog.V(4).Infof("Finished syncing EgressQoS pod %s on namespace %s : %v", name, namespace, time.Since(startTime))
	}()

	eq := obj.(*egressQoS)
	eq.RLock() // allow multiple pods to sync
	defer eq.RUnlock()
	if eq.stale { // was deleted or not created properly
		return nil
	}

	pod, err := oc.egressQoSPodLister.Pods(namespace).Get(name)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}

	allOps := []ovsdb.Operation{}

	// on delete/complete we remove the pod from the relevant address sets
	if pod == nil || util.PodCompleted(pod) {
		podsCaches := []*sync.Map{}
		for _, rule := range eq.rules {
			obj, loaded := rule.pods.Load(name)
			if !loaded {
				continue
			}
			ips := obj.([]net.IP)
			ops, err := rule.addrSet.DeleteAddressesReturnOps(util.StringSlice(ips))
			if err != nil {
				return err
			}
			podsCaches = append(podsCaches, rule.pods)
			allOps = append(allOps, ops...)
		}
		_, err = libovsdbops.TransactAndCheck(oc.nbClient, allOps)
		if err != nil {
			return err
		}

		for _, pc := range podsCaches {
			pc.Delete(name)
		}

		return nil
	}

	klog.V(5).Infof("Pod %s retrieved from lister: %v", pod.Name, pod)

	if util.PodWantsHostNetwork(pod) || !oc.isPodScheduledinLocalZone(pod) { // we don't handle HostNetworked or remote zone pods
		return nil
	}

	podIPs, err := util.GetPodIPsOfNetwork(pod, oc.GetNetInfo())
	if errors.Is(err, util.ErrNoPodIPFound) {
		return nil // reprocess it when it is updated with an IP
	}
	if err != nil {
		return err
	}

	podLabels := labels.Set(pod.Labels)
	podMapOps := []mapAndOp{}
	for _, r := range eq.rules {
		selector, err := metav1.LabelSelectorAsSelector(&r.podSelector)
		if err != nil {
			return err
		}
		if selector.Empty() { // rule applies to all pods in the namespace, no need to modify address set
			continue
		}

		_, loaded := r.pods.Load(pod.Name)
		if selector.Matches(podLabels) && !loaded {
			ops, err := r.addrSet.AddAddressesReturnOps(util.StringSlice(podIPs))
			if err != nil {
				return err
			}
			allOps = append(allOps, ops...)
			podMapOps = append(podMapOps, mapAndOp{r.pods, mapInsert})
		} else if !selector.Matches(podLabels) && loaded {
			ops, err := r.addrSet.DeleteAddressesReturnOps(util.StringSlice(podIPs))
			if err != nil {
				return err
			}
			allOps = append(allOps, ops...)
			podMapOps = append(podMapOps, mapAndOp{r.pods, mapDelete})
		}
	}

	_, err = libovsdbops.TransactAndCheck(oc.nbClient, allOps)
	if err != nil {
		return err
	}

	for _, mapOp := range podMapOps {
		switch mapOp.op {
		case mapInsert:
			mapOp.m.Store(pod.Name, podIPs)
		case mapDelete:
			mapOp.m.Delete(pod.Name)
		}
	}

	return nil
}

// onEgressQoSPodAdd queues the pod for processing.
func (oc *DefaultNetworkController) onEgressQoSPodAdd(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", obj, err))
		return
	}
	pod := obj.(*kapi.Pod)
	// only process this pod if it is local to this zone
	if !oc.isPodScheduledinLocalZone(pod) {
		// NOTE: This means we don't handle the case where pod goes from
		// being local to remote. So far there is no use case for this to happen.
		// Also when we think about a pod going from local to remote - what does that mean?
		// It means the node on which the pod lived suddenly stopped being local to this zone
		// That either means node changed zones - which will involve a full delete and recreate
		// the OVN objects in a new zone's DB and/or node is gone etc. All those scenarios don't
		// need this controller to take any action.
		// NOTE2: During upgrades when the legacy ovnkube-master is still running it will detect
		// nodes have gone remote which for this feature means deleting the switches totally and
		// based on OVN db schema this will remove all referenced QoS rules created on the switch
		return // not local to this zone, nothing to do; no-op
	}
	oc.egressQoSPodQueue.Add(key)
}

// onEgressQoSPodUpdate queues the pod for processing.
func (oc *DefaultNetworkController) onEgressQoSPodUpdate(oldObj, newObj interface{}) {
	oldPod := oldObj.(*kapi.Pod)
	newPod := newObj.(*kapi.Pod)

	if oldPod.ResourceVersion == newPod.ResourceVersion ||
		!newPod.GetDeletionTimestamp().IsZero() {
		return
	}

	oldPodLabels := labels.Set(oldPod.Labels)
	newPodLabels := labels.Set(newPod.Labels)
	oldPodIPs, _ := util.GetPodIPsOfNetwork(oldPod, oc.GetNetInfo())
	newPodIPs, _ := util.GetPodIPsOfNetwork(newPod, oc.GetNetInfo())
	isOldPodLocal := oc.isPodScheduledinLocalZone(oldPod)
	isNewPodLocal := oc.isPodScheduledinLocalZone(newPod)
	oldPodCompleted := util.PodCompleted(oldPod)
	newPodCompleted := util.PodCompleted(newPod)
	if labels.Equals(oldPodLabels, newPodLabels) &&
		len(oldPodIPs) == len(newPodIPs) &&
		// NOTE: We only expect remote pods to become local when they are scheduled; not vice versa
		isOldPodLocal == isNewPodLocal &&
		oldPodCompleted == newPodCompleted {
		return
	}

	key, err := cache.MetaNamespaceKeyFunc(newObj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", newObj, err))
		return
	}

	oc.egressQoSPodQueue.Add(key)
}

func (oc *DefaultNetworkController) onEgressQoSPodDelete(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", obj, err))
		return
	}
	pod := obj.(*kapi.Pod)
	// only process this pod if it is local to this zone
	if !oc.isPodScheduledinLocalZone(pod) {
		// NOTE: This means we don't handle the case where pod goes from
		// being local to remote. So far there is no use case for this to happen.
		// Also when we think about a pod going from local to remote - what does that mean?
		// It means the node on which the pod lived suddenly stopped being local to this zone
		// That either means node changed zones - which will involve a full delete and recreate
		// the OVN objects in a new zone's DB and/or node is gone etc. All those scenarios don't
		// need this controller to take any action.
		// NOTE2: During upgrades when the legacy ovnkube-master is still running it will detect
		// nodes have gone remote which for this feature means deleting the switches totally and
		// based on OVN db schema this will remove all referenced QoS rules created on the switch
		return // not local to this zone, nothing to do; no-op
	}
	oc.egressQoSPodQueue.Add(key)
}

func (oc *DefaultNetworkController) runEgressQoSPodWorker(wg *sync.WaitGroup) {
	for oc.processNextEgressQoSPodWorkItem(wg) {
	}
}

func (oc *DefaultNetworkController) processNextEgressQoSPodWorkItem(wg *sync.WaitGroup) bool {
	wg.Add(1)
	defer wg.Done()
	key, quit := oc.egressQoSPodQueue.Get()
	if quit {
		return false
	}
	defer oc.egressQoSPodQueue.Done(key)

	err := oc.syncEgressQoSPod(key)
	if err == nil {
		oc.egressQoSPodQueue.Forget(key)
		return true
	}

	utilruntime.HandleError(fmt.Errorf("%v failed with : %v", key, err))

	if oc.egressQoSPodQueue.NumRequeues(key) < maxEgressQoSRetries {
		oc.egressQoSPodQueue.AddRateLimited(key)
		return true
	}

	oc.egressQoSPodQueue.Forget(key)
	return true
}

// onEgressQoSAdd queues the node for processing.
func (oc *DefaultNetworkController) onEgressQoSNodeAdd(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", obj, err))
		return
	}
	node := obj.(*kapi.Node)
	if util.GetNodeZone(node) != oc.zone {
		return
	}
	oc.egressQoSNodeQueue.Add(key)
}

// onEgressQoSNodeUpdate queues the node for processing if it changed zones
func (oc *DefaultNetworkController) onEgressQoSNodeUpdate(oldObj, newObj interface{}) {
	oldNode := oldObj.(*kapi.Node)
	newNode := newObj.(*kapi.Node)
	if oldNode.ResourceVersion == newNode.ResourceVersion ||
		!newNode.GetDeletionTimestamp().IsZero() {
		return
	}
	// During a nodeAdd event, the ovnkube-node can take some time to add the zone
	// annotation to the node, during that interim time we might consider the node
	// as remote and hence the addNode event might not do anything. So we need to
	// watch for node updates. We also ensure we only process local node zones by
	// comparing to the controller's zone. That will cover the remote->local case.
	// The local->remote case is not covered or handled here because in that
	// scenario the addUpdateRemoteNodeEvent function which calls the cleanupNodeResources
	// will just cleanup the switch resource for the node.
	oldNodeZone := util.GetNodeZone(oldNode)
	newNodeZone := util.GetNodeZone(newNode)
	if oldNodeZone == newNodeZone || newNodeZone != oc.zone {
		return
	}
	key, err := cache.MetaNamespaceKeyFunc(newObj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", newObj, err))
		return
	}
	oc.egressQoSNodeQueue.Add(key)
}

func (oc *DefaultNetworkController) runEgressQoSNodeWorker(wg *sync.WaitGroup) {
	for oc.processNextEgressQoSNodeWorkItem(wg) {
	}
}

func (oc *DefaultNetworkController) processNextEgressQoSNodeWorkItem(wg *sync.WaitGroup) bool {
	wg.Add(1)
	defer wg.Done()
	key, quit := oc.egressQoSNodeQueue.Get()
	if quit {
		return false
	}
	defer oc.egressQoSNodeQueue.Done(key)

	err := oc.syncEgressQoSNode(key)
	if err == nil {
		oc.egressQoSNodeQueue.Forget(key)
		return true
	}

	utilruntime.HandleError(fmt.Errorf("%v failed with: %v", key, err))

	if oc.egressQoSNodeQueue.NumRequeues(key) < maxEgressQoSRetries {
		oc.egressQoSNodeQueue.AddRateLimited(key)
		return true
	}

	oc.egressQoSNodeQueue.Forget(key)
	return true
}

func (oc *DefaultNetworkController) syncEgressQoSNode(key string) error {
	startTime := time.Now()
	_, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}
	klog.Infof("Processing sync for EgressQoS node %s", name)

	defer func() {
		klog.V(4).Infof("Finished syncing EgressQoS node %s : %v", name, time.Since(startTime))
	}()

	n, err := oc.egressQoSNodeLister.Get(name)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}

	if n == nil { // we don't process node deletions, its logical switch will be deleted.
		return nil
	}

	klog.V(5).Infof("EgressQoS %s node retrieved from lister: %v", n.Name, n)

	nodeSw := &nbdb.LogicalSwitch{
		Name: oc.GetNetworkScopedSwitchName(n.Name),
	}
	nodeSw, err = libovsdbops.GetLogicalSwitch(oc.nbClient, nodeSw)
	if err != nil {
		return err
	}
	predicateIDs := libovsdbops.NewDbObjectIDs(libovsdbops.QoSEgressQoS, oc.controllerName, nil)
	qPredicate := libovsdbops.GetPredicate[*nbdb.QoS](predicateIDs, nil)
	existingQoSes, err := libovsdbops.FindQoSesWithPredicate(oc.nbClient, qPredicate)
	if err != nil {
		return err
	}

	if len(existingQoSes) == 0 {
		return nil
	}

	ops, err := libovsdbops.AddQoSesToLogicalSwitchOps(oc.nbClient, nil, nodeSw.Name, existingQoSes...)
	if err != nil {
		return err
	}

	if _, err := libovsdbops.TransactAndCheck(oc.nbClient, ops); err != nil {
		return fmt.Errorf("unable to add existing qoses to new node, err: %v", err)
	}

	return nil
}

// updateEgressQoSZoneStatusToReady updates the status of the EgressQoS to reflect that it is ready
// Each zone's ovnkube-controller will call this, hence let's update status using server side apply.
func (oc *DefaultNetworkController) updateEgressQoSZoneStatusToReady(egressQoS *egressqosapi.EgressQoS) error {
	if egressQoS == nil {
		return nil
	}
	readyCondition := metav1.Condition{
		Type:               egressQoSReadyStatusType + oc.zone,
		Status:             metav1.ConditionTrue,
		LastTransitionTime: metav1.NewTime(time.Now()),
		Reason:             egressQoSReadyReason,
		Message:            egressQoSAppliedCorrectly,
	}
	return oc.updateEgressQoSZoneStatusCondition(readyCondition, egressQoS.Namespace, egressQoS.Name)
}

// updateEgressQoSZoneStatusToNotReady updates the status of the EgressQoS to reflect that it is not ready
// Each zone's ovnkube-controller will call this, hence let's update status using server side apply.
func (oc *DefaultNetworkController) updateEgressQoSZoneStatusToNotReady(egressQoS *egressqosapi.EgressQoS,
	handlerErr error) error {
	if egressQoS == nil {
		return nil
	}
	notReadyCondition := metav1.Condition{
		Type:               egressQoSReadyStatusType + oc.zone,
		Status:             metav1.ConditionFalse,
		LastTransitionTime: metav1.NewTime(time.Now()),
		Reason:             egressQoSNotReadyReason,
		Message:            types.EgressQoSErrorMsg + ": " + handlerErr.Error(),
	}
	return oc.updateEgressQoSZoneStatusCondition(notReadyCondition, egressQoS.Namespace, egressQoS.Name)
}

func (oc *DefaultNetworkController) updateEgressQoSZoneStatusCondition(newCondition metav1.Condition,
	namespace, name string) error {
	eq, err := oc.egressQoSLister.EgressQoSes(namespace).Get(name)
	if err != nil {
		return err
	}

	newConditionApply := &metaapplyv1.ConditionApplyConfiguration{
		Type:    &newCondition.Type,
		Status:  &newCondition.Status,
		Reason:  &newCondition.Reason,
		Message: &newCondition.Message,
	}

	existingCondition := meta.FindStatusCondition(eq.Status.Conditions, newCondition.Type)
	if existingCondition == nil || existingCondition.Status != newCondition.Status {
		newConditionApply.LastTransitionTime = ptr.To(metav1.NewTime(time.Now()))
	}

	applyObj := egressqosapply.EgressQoS(name, namespace).
		WithStatus(egressqosapply.EgressQoSStatus().WithConditions(newConditionApply))
	_, err = oc.kube.EgressQoSClient.K8sV1().EgressQoSes(namespace).ApplyStatus(context.TODO(),
		applyObj, metav1.ApplyOptions{FieldManager: oc.zone, Force: true})
	return err
}
