package apbroute

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	ktypes "k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	coreinformers "k8s.io/client-go/informers/core/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	nettypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	adminpolicybasedrouteapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1"
	adminpolicybasedrouteinformer "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1/apis/informers/externalversions/adminpolicybasedroute/v1"
	adminpolicybasedroutelisters "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1/apis/listers/adminpolicybasedroute/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/controller/apbroute/gateway_info"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/syncmap"
)

const (
	maxRetries = 15
)

func insertSet(s1, s2 sets.Set[string]) {
	for insertItem := range s2 {
		s1.Insert(insertItem)
	}
}

type podInfo struct {
	StaticGateways  *gateway_info.GatewayInfoList
	DynamicGateways *gateway_info.GatewayInfoList
}

func newPodInfo() *podInfo {
	return &podInfo{
		DynamicGateways: gateway_info.NewGatewayInfoList(),
		StaticGateways:  gateway_info.NewGatewayInfoList(),
	}
}

type RouteInfo struct {
	PodName ktypes.NamespacedName
	// PodExternalRoutes is a cache keeping the LR routes added to the GRs when
	// external gateways are used. The first map key is the podIP (src-ip of the route),
	// the second the GW IP (next hop), and the third the GR name
	PodExternalRoutes map[string]map[string]string
}

type ExternalGatewayRouteInfoCache struct {
	// External gateway caches
	routeInfos *syncmap.SyncMapComparableKey[ktypes.NamespacedName, *RouteInfo]
}

func NewExternalGatewayRouteInfoCache() *ExternalGatewayRouteInfoCache {
	return &ExternalGatewayRouteInfoCache{
		routeInfos: syncmap.NewSyncMapComparableKey[ktypes.NamespacedName, *RouteInfo](),
	}
}

// CreateOrLoad provides a mechanism to initialize keys in the cache before calling the argument function `f`. This approach
// hides the logic to initialize and retrieval of the key's routeInfo and allows reusability by exposing a function signature as argument
// that has a routeInfo instance as argument. The function will attempt to retrieve the routeInfo for a given key,
// and create an empty routeInfo structure in the cache when not found. Then it will execute the function argument `f` passing
// the routeInfo as argument.
func (e *ExternalGatewayRouteInfoCache) CreateOrLoad(podName ktypes.NamespacedName, f func(routeInfo *RouteInfo) error) error {
	return e.routeInfos.DoWithLock(podName, func(key ktypes.NamespacedName) error {
		routeInfo := &RouteInfo{
			PodExternalRoutes: make(map[string]map[string]string),
			PodName:           podName,
		}
		routeInfo, _ = e.routeInfos.LoadOrStore(key, routeInfo)
		return f(routeInfo)
	})
}

// Cleanup will lock the key `podName` and use the routeInfo associated to the key to pass it as an argument to function `f`.
// After the function `f` completes, it will delete any empty PodExternalRoutes references for each given podIP in the routeInfo object,
// as well as deleting the key itself if it contains no entries in its `PodExternalRoutes` map.
func (e *ExternalGatewayRouteInfoCache) Cleanup(podName ktypes.NamespacedName, f func(routeInfo *RouteInfo) error) error {
	return e.routeInfos.DoWithLock(podName, func(key ktypes.NamespacedName) error {
		routeInfo, loaded := e.routeInfos.Load(key)
		if !loaded {
			return nil
		}
		err := f(routeInfo)
		for podIP, routes := range routeInfo.PodExternalRoutes {
			if len(routes) == 0 {
				delete(routeInfo.PodExternalRoutes, podIP)
			}
		}
		if err == nil && len(routeInfo.PodExternalRoutes) == 0 {
			e.routeInfos.Delete(key)
		}
		return err
	})
}

// CleanupNamespace wraps the cleanup call for all the pods in a given namespace.
// The routeInfo reference for each pod in the given namespace is processed by the `f` function inside the `Cleanup` function
func (e *ExternalGatewayRouteInfoCache) CleanupNamespace(nsName string, f func(routeInfo *RouteInfo) error) error {
	for _, podName := range e.routeInfos.GetKeys() {
		if podName.Namespace == nsName {
			err := e.Cleanup(podName, f)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// routePolicyState contains current policy state as it was applied.
// Since every config is applied to a pod, podInfo stores current state for every target pod.
type routePolicyState struct {
	// namespaceName: podName: configured gateways for the pod
	targetNamespaces map[string]map[ktypes.NamespacedName]*podInfo
}

func newRoutePolicyState() *routePolicyState {
	return &routePolicyState{
		targetNamespaces: map[string]map[ktypes.NamespacedName]*podInfo{},
	}
}

// Equal compares StaticGateways and DynamicGateways elements for every namespace to be exactly the same,
// including applied status
func (rp *routePolicyState) Equal(rp2 *routePolicyState) bool {
	if len(rp2.targetNamespaces) != len(rp.targetNamespaces) {
		return false
	}
	for nsName, nsInfo := range rp.targetNamespaces {
		nsInfo2, found := rp2.targetNamespaces[nsName]
		if !found {
			return false
		}
		if len(nsInfo) != len(nsInfo2) {
			return false
		}
		for podName, podInfo := range nsInfo {
			podInfo2, found := nsInfo2[podName]
			if !found {
				return false
			}
			if !podInfo.StaticGateways.Equal(podInfo2.StaticGateways) ||
				!podInfo.DynamicGateways.Equal(podInfo2.DynamicGateways) {
				return false
			}
		}
	}
	return true
}

func (rp *routePolicyState) String() string {
	s := strings.Builder{}
	s.WriteString("{")
	for nsName, nsInfo := range rp.targetNamespaces {
		s.WriteString(fmt.Sprintf("%s: map[", nsName))
		for podName, podInfo := range nsInfo {
			s.WriteString(fmt.Sprintf("%s: [StaticGateways: {%s}, DynamicGateways: {%s}],", podName, podInfo.StaticGateways.String(),
				podInfo.DynamicGateways.String()))
		}
		s.WriteString("],")
	}
	s.WriteString("}")
	return s.String()
}

// routePolicyConfig is used to update policy to the latest state, it stores all required information for an
// update.
type routePolicyConfig struct {
	policyName string
	// targetNamespacesWithPods[namespaceName[podNamespacedName] = Pod
	targetNamespacesWithPods map[string]map[ktypes.NamespacedName]*v1.Pod
	// staticGateways contains the processed list of IPs and BFD information defined in the staticHop slice in the policy.
	staticGateways *gateway_info.GatewayInfoList
	// dynamicGateways contains the processed list of IPs and BFD information defined in the dynamicHop slice in the policy.
	dynamicGateways *gateway_info.GatewayInfoList
}

type externalPolicyManager struct {
	stopCh <-chan struct{}

	// policyReferencedObjects should only be accessed with policyReferencedObjectsLock
	policyReferencedObjectsLock sync.RWMutex
	// policyReferencedObjects is a cache of objects every policy has selected for its config.
	// With this cache namespace and pod handlers may fetch affected policies for cleanup.
	// key is policyName.
	policyReferencedObjects map[string]*policyReferencedObjects

	// routePolicySyncCache is a cache of configures states for policies, key is policyName.
	routePolicySyncCache *syncmap.SyncMap[*routePolicyState]
	// networkClient is an interface that exposes add and delete GW IPs. There are 2 structs that implement this contract: one to interface with the north bound DB and another one for the conntrack.
	// the north bound is used by the master controller to add and delete the logical static routes, whilst the conntrack is used by the node controller to ensure that the ECMP entries are removed
	// when a gateway IP is no longer an egress access point.
	netClient networkClient

	// route policies
	routeLister   adminpolicybasedroutelisters.AdminPolicyBasedExternalRouteLister
	routeInformer cache.SharedIndexInformer
	routeQueue    workqueue.TypedRateLimitingInterface[string]

	// Pods
	podLister   corev1listers.PodLister
	podInformer cache.SharedIndexInformer
	podQueue    workqueue.TypedRateLimitingInterface[*v1.Pod]

	// Namespaces
	namespaceQueue    workqueue.TypedRateLimitingInterface[*v1.Namespace]
	namespaceLister   corev1listers.NamespaceLister
	namespaceInformer cache.SharedIndexInformer

	updatePolicyStatusFunc func(policyName string, gwIPs sets.Set[string], processedError error) error
}

type policyReferencedObjects struct {
	targetNamespaces    sets.Set[string]
	dynamicGWNamespaces sets.Set[string]
	dynamicGWPods       sets.Set[ktypes.NamespacedName]
}

func newExternalPolicyManager(
	stopCh <-chan struct{},
	podInformer coreinformers.PodInformer,
	namespaceInformer coreinformers.NamespaceInformer,
	apbRouteInformer adminpolicybasedrouteinformer.AdminPolicyBasedExternalRouteInformer,
	netClient networkClient,
	updatePolicyStatusFunc func(policyName string, gwIPs sets.Set[string], processedError error) error) *externalPolicyManager {

	m := externalPolicyManager{
		stopCh:                      stopCh,
		policyReferencedObjectsLock: sync.RWMutex{},
		policyReferencedObjects:     map[string]*policyReferencedObjects{},
		routePolicySyncCache:        syncmap.NewSyncMap[*routePolicyState](),
		netClient:                   netClient,

		routeLister:   apbRouteInformer.Lister(),
		routeInformer: apbRouteInformer.Informer(),
		routeQueue: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.NewTypedItemFastSlowRateLimiter[string](time.Second, 5*time.Second, 5),
			workqueue.TypedRateLimitingQueueConfig[string]{Name: "adminpolicybasedexternalroutes"},
		),
		podLister:   podInformer.Lister(),
		podInformer: podInformer.Informer(),
		podQueue: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.NewTypedItemFastSlowRateLimiter[*v1.Pod](time.Second, 5*time.Second, 5),
			workqueue.TypedRateLimitingQueueConfig[*v1.Pod]{Name: "apbexternalroutepods"},
		),
		namespaceLister:   namespaceInformer.Lister(),
		namespaceInformer: namespaceInformer.Informer(),
		namespaceQueue: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.NewTypedItemFastSlowRateLimiter[*v1.Namespace](time.Second, 5*time.Second, 5),
			workqueue.TypedRateLimitingQueueConfig[*v1.Namespace]{Name: "apbexternalroutenamespaces"},
		),
		updatePolicyStatusFunc: updatePolicyStatusFunc,
	}

	return &m
}

func (m *externalPolicyManager) Run(wg *sync.WaitGroup, threadiness int) error {
	defer utilruntime.HandleCrash()
	klog.V(4).Info("Starting Admin Policy Based Route Controller")

	_, err := m.namespaceInformer.AddEventHandler(
		factory.WithUpdateHandlingForObjReplace(cache.ResourceEventHandlerFuncs{
			AddFunc:    m.onNamespaceAdd,
			UpdateFunc: m.onNamespaceUpdate,
			DeleteFunc: m.onNamespaceDelete,
		}))
	if err != nil {
		return err
	}

	_, err = m.podInformer.AddEventHandler(
		factory.WithUpdateHandlingForObjReplace(cache.ResourceEventHandlerFuncs{
			AddFunc:    m.onPodAdd,
			UpdateFunc: m.onPodUpdate,
			DeleteFunc: m.onPodDelete,
		}))
	if err != nil {
		return err
	}
	_, err = m.routeInformer.AddEventHandler(
		factory.WithUpdateHandlingForObjReplace(cache.ResourceEventHandlerFuncs{
			AddFunc:    m.onPolicyAdd,
			UpdateFunc: m.onPolicyUpdate,
			DeleteFunc: m.onPolicyDelete,
		}))
	if err != nil {
		return err
	}

	for i := 0; i < threadiness; i++ {
		for _, workerFn := range []func(*sync.WaitGroup){
			// processes route policies
			m.runPolicyWorker,
			// detects gateway pod changes and updates the pod's IP and MAC in the northbound DB
			m.runPodWorker,
			// detects namespace changes and applies polices that match the namespace selector in the `From` policy field
			m.runNamespaceWorker,
		} {
			wg.Add(1)
			go func(fn func(*sync.WaitGroup)) {
				defer wg.Done()
				wait.Until(func() {
					fn(wg)
				}, time.Second, m.stopCh)
			}(workerFn)
		}
	}

	// add shutdown goroutine waiting for m.stopCh
	wg.Add(1)
	go func() {
		defer wg.Done()
		// wait until we're told to stop
		<-m.stopCh

		m.podQueue.ShutDown()
		m.routeQueue.ShutDown()
		m.namespaceQueue.ShutDown()
	}()

	return nil
}

func (m *externalPolicyManager) runPolicyWorker(wg *sync.WaitGroup) {
	for m.processNextPolicyWorkItem(wg) {
	}
}

func (m *externalPolicyManager) processNextPolicyWorkItem(wg *sync.WaitGroup) bool {
	wg.Add(1)
	defer wg.Done()

	key, shutdown := m.routeQueue.Get()

	if shutdown {
		return false
	}

	defer m.routeQueue.Done(key)

	klog.V(4).Infof("Processing policy %s", key)
	gwIPs, err := m.syncRoutePolicy(key)
	if err != nil {
		klog.Errorf("Failed to sync APB policy %s: %v", key, err)
	}

	if m.updatePolicyStatusFunc != nil {
		statusErr := m.updatePolicyStatusFunc(key, gwIPs, err)
		if statusErr != nil {
			klog.Warningf("Failed to update AdminPolicyBasedExternalRoutes %s status: %v", key, statusErr)
		}
	}

	if err != nil {
		if m.routeQueue.NumRequeues(key) < maxRetries {
			klog.V(4).Infof("Error found while processing policy %s: %v", key, err)
			m.routeQueue.AddRateLimited(key)
			return true
		}
		klog.Warningf("Dropping policy %q out of the queue: %v", key, err)
		utilruntime.HandleError(err)
	}
	m.routeQueue.Forget(key)
	return true
}

func (m *externalPolicyManager) onPolicyAdd(obj interface{}) {
	_, ok := obj.(*adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute)
	if !ok {
		utilruntime.HandleError(fmt.Errorf("expecting %T but received %T", &adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute{}, obj))
		return
	}
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", obj, err))
		return
	}
	klog.V(4).Infof("Adding policy %s", key)
	m.routeQueue.Add(key)
}

func (m *externalPolicyManager) onPolicyUpdate(oldObj, newObj interface{}) {
	oldRoutePolicy, ok := oldObj.(*adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute)
	if !ok {
		utilruntime.HandleError(fmt.Errorf("expecting %T but received %T", &adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute{}, oldObj))
		return
	}
	newRoutePolicy, ok := newObj.(*adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute)
	if !ok {
		utilruntime.HandleError(fmt.Errorf("expecting %T but received %T", &adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute{}, newObj))
		return
	}

	if oldRoutePolicy == nil || newRoutePolicy == nil {
		utilruntime.HandleError(errors.New("invalid Admin Policy Based External Route provided to onPolicyUpdate()"))
		return
	}

	if oldRoutePolicy.Generation == newRoutePolicy.Generation ||
		!newRoutePolicy.GetDeletionTimestamp().IsZero() {
		return
	}

	key, err := cache.MetaNamespaceKeyFunc(newObj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", newObj, err))
	}
	m.routeQueue.Add(key)
}

func (m *externalPolicyManager) onPolicyDelete(obj interface{}) {
	_, ok := obj.(*adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("couldn't get object from tomstone %#v", obj))
			return
		}
		_, ok = tombstone.Obj.(*adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("tombstone contained object that is not an Admin Policy Based External Route %#v", tombstone.Obj))
			return
		}
	}
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", obj, err))
		return
	}
	m.routeQueue.Add(key)
}

func (m *externalPolicyManager) onNamespaceAdd(obj interface{}) {
	ns, ok := obj.(*v1.Namespace)
	if !ok {
		utilruntime.HandleError(fmt.Errorf("expecting %T but received %T", &v1.Namespace{}, obj))
		return
	}
	if ns == nil {
		utilruntime.HandleError(errors.New("invalid Namespace provided to onNamespaceAdd()"))
		return
	}
	m.namespaceQueue.Add(ns)
}

func (m *externalPolicyManager) onNamespaceUpdate(oldObj, newObj interface{}) {
	oldNamespace, ok := oldObj.(*v1.Namespace)
	if !ok {
		utilruntime.HandleError(fmt.Errorf("expecting %T but received %T", &v1.Namespace{}, oldObj))
		return
	}
	newNamespace, ok := newObj.(*v1.Namespace)
	if !ok {
		utilruntime.HandleError(fmt.Errorf("expecting %T but received %T", &v1.Namespace{}, newObj))
		return
	}
	if oldNamespace == nil || newNamespace == nil {
		utilruntime.HandleError(errors.New("invalid Namespace provided to onNamespaceUpdate()"))
		return
	}
	if oldNamespace.ResourceVersion == newNamespace.ResourceVersion || !newNamespace.GetDeletionTimestamp().IsZero() {
		return
	}
	m.namespaceQueue.Add(newNamespace)
}

func (m *externalPolicyManager) onNamespaceDelete(obj interface{}) {
	ns, ok := obj.(*v1.Namespace)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("couldn't get object from tombstone %#v", obj))
			return
		}
		ns, ok = tombstone.Obj.(*v1.Namespace)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("tombstone contained object that is not a Namespace: %#v", tombstone.Obj))
			return
		}
	}
	if ns != nil {
		m.namespaceQueue.Add(ns)
	}
}

func (m *externalPolicyManager) runNamespaceWorker(wg *sync.WaitGroup) {
	for m.processNextNamespaceWorkItem(wg) {

	}
}

func (m *externalPolicyManager) processNextNamespaceWorkItem(wg *sync.WaitGroup) bool {
	wg.Add(1)
	defer wg.Done()

	obj, shutdown := m.namespaceQueue.Get()

	if shutdown {
		return false
	}

	defer m.namespaceQueue.Done(obj)

	err := m.syncNamespace(obj, m.routeQueue)
	if err != nil {
		if m.namespaceQueue.NumRequeues(obj) < maxRetries {
			klog.V(4).Infof("Error found while processing namespace %s:%v", obj, err)
			m.namespaceQueue.AddRateLimited(obj)
			return true
		}
		klog.Warningf("Dropping namespace %q out of the queue: %v", obj.Name, err)
		utilruntime.HandleError(err)
	}
	m.namespaceQueue.Forget(obj)
	return true
}

func (m *externalPolicyManager) onPodAdd(obj interface{}) {
	pod, ok := obj.(*v1.Pod)
	if !ok {
		utilruntime.HandleError(fmt.Errorf("expecting %T but received %T", &v1.Pod{}, obj))
		return
	}
	if pod == nil {
		utilruntime.HandleError(errors.New("invalid Pod provided to onPodAdd()"))
		return
	}
	// if the pod does not have IPs AND there are no multus network status annotations found, skip it
	if len(pod.Status.PodIPs) == 0 && len(pod.Annotations[nettypes.NetworkStatusAnnot]) == 0 {
		return
	}
	m.podQueue.Add(pod)
}

func (m *externalPolicyManager) onPodUpdate(oldObj, newObj interface{}) {
	o, ok := oldObj.(*v1.Pod)
	if !ok {
		utilruntime.HandleError(fmt.Errorf("expecting %T but received %T", &v1.Pod{}, o))
		return
	}
	n, ok := newObj.(*v1.Pod)
	if !ok {
		utilruntime.HandleError(fmt.Errorf("expecting %T but received %T", &v1.Pod{}, n))
		return
	}
	if o == nil || n == nil {
		utilruntime.HandleError(errors.New("invalid Pod provided to onPodUpdate()"))
		return
	}
	// if labels AND assigned Pod IPs AND the multus network status annotations are the same, skip processing changes to the pod.
	if reflect.DeepEqual(o.Labels, n.Labels) &&
		reflect.DeepEqual(o.Status.PodIPs, n.Status.PodIPs) &&
		reflect.DeepEqual(o.Annotations[nettypes.NetworkStatusAnnot], n.Annotations[nettypes.NetworkStatusAnnot]) {
		return
	}
	m.podQueue.Add(n)
}

func (m *externalPolicyManager) onPodDelete(obj interface{}) {
	pod, ok := obj.(*v1.Pod)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("couldn't get object from tombstone %#v", obj))
			return
		}
		pod, ok = tombstone.Obj.(*v1.Pod)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("tombstone contained object that is not a Pod: %#v", tombstone.Obj))
			return
		}
	}
	if pod != nil {
		m.podQueue.Add(pod)
	}
}

func (m *externalPolicyManager) runPodWorker(wg *sync.WaitGroup) {
	for m.processNextPodWorkItem(wg) {
	}
}

func (m *externalPolicyManager) processNextPodWorkItem(wg *sync.WaitGroup) bool {
	wg.Add(1)
	defer wg.Done()

	obj, shutdown := m.podQueue.Get()

	if shutdown {
		return false
	}

	defer m.podQueue.Done(obj)

	err := m.syncPod(obj, m.routeQueue)
	if err != nil {
		if m.podQueue.NumRequeues(obj) < maxRetries {
			klog.V(4).Infof("Error found while processing pod %s/%s:%v", obj.Namespace, obj.Name, err)
			m.podQueue.AddRateLimited(obj)
			return true
		}
		klog.Warningf("Dropping pod %s/%s out of the queue: %s", obj.Namespace, obj.Name, err)
		utilruntime.HandleError(err)
	}

	m.podQueue.Forget(obj)
	return true
}

// getPoliciesForNamespaceChange returns a list of policies that should be reconciled because of a given namespace update.
// It consists of 2 stages:
// 1. find policies that select given namespace now and may need update
// 2. find policies that selected given namespace before and may need cleanup
// Step 1 is done by fetching the latest AdminPolicyBasedExternalRoute and checking if selectors match.
// Step 2 is done via policyReferencedObjects, which is a cache of the objects every policy selected last time.
func (m *externalPolicyManager) getPoliciesForNamespaceChange(namespace *v1.Namespace) (sets.Set[string], error) {
	policyNames := sets.Set[string]{}
	// first check which policies currently match given namespace.
	// This should work when namespace is added, or starts matching a label selector
	informerPolicies, err := m.getAllRoutePolicies()
	if err != nil {
		return nil, err
	}

	for _, informerPolicy := range informerPolicies {
		targetNsSel, err := metav1.LabelSelectorAsSelector(&informerPolicy.Spec.From.NamespaceSelector)
		if err != nil {
			return nil, err
		}
		if targetNsSel.Matches(labels.Set(namespace.Labels)) {
			policyNames.Insert(informerPolicy.Name)
			continue
		}

		for _, hop := range informerPolicy.Spec.NextHops.DynamicHops {
			gwNsSel, err := metav1.LabelSelectorAsSelector(&hop.NamespaceSelector)
			if err != nil {
				return nil, err
			}
			if gwNsSel.Matches(labels.Set(namespace.Labels)) {
				policyNames.Insert(informerPolicy.Name)
			}
		}
	}

	// check which namespaces were referenced by policies before
	m.policyReferencedObjectsLock.RLock()
	defer m.policyReferencedObjectsLock.RUnlock()
	for policyName, policyRefs := range m.policyReferencedObjects {
		if policyRefs.targetNamespaces.Has(namespace.Name) {
			policyNames.Insert(policyName)
			continue
		}
		if policyRefs.dynamicGWNamespaces.Has(namespace.Name) {
			policyNames.Insert(policyName)
		}
	}
	return policyNames, nil
}

// getPoliciesForPodChange returns a list of policies that should be reconciled because of a given pod update.
// It consists of 2 stages:
// 1. find policies that select given pod now and may need update
// 2. find policies that selected given pod before and may need cleanup
// Step 1 is done by fetching the latest AdminPolicyBasedExternalRoute and checking if selectors match.
// Step 2 is done via policyReferencedObjects, which is a cache of the objects every policy selected last time.
func (m *externalPolicyManager) getPoliciesForPodChange(pod *v1.Pod) (sets.Set[string], error) {
	policyNames := sets.Set[string]{}
	// first check which policies currently match given namespace.
	// This should work when namespace is added, or starts matching a label selector
	informerPolicies, err := m.getAllRoutePolicies()
	if err != nil {
		return nil, err
	}
	podNs, err := m.namespaceLister.Get(pod.Namespace)
	if err != nil {
		return nil, err
	}

	for _, informerPolicy := range informerPolicies {
		targetNsSel, err := metav1.LabelSelectorAsSelector(&informerPolicy.Spec.From.NamespaceSelector)
		if err != nil {
			return nil, err
		}
		if targetNsSel.Matches(labels.Set(podNs.Labels)) {
			policyNames.Insert(informerPolicy.Name)
			continue
		}

		for _, hop := range informerPolicy.Spec.NextHops.DynamicHops {
			gwNsSel, err := metav1.LabelSelectorAsSelector(&hop.NamespaceSelector)
			if err != nil {
				return nil, err
			}
			gwPodSel, err := metav1.LabelSelectorAsSelector(&hop.PodSelector)
			if err != nil {
				return nil, err
			}
			if gwNsSel.Matches(labels.Set(podNs.Labels)) && gwPodSel.Matches(labels.Set(pod.Labels)) {
				policyNames.Insert(informerPolicy.Name)
			}
		}
	}
	// check which namespaces were referenced by policies before
	m.policyReferencedObjectsLock.RLock()
	defer m.policyReferencedObjectsLock.RUnlock()
	for policyName, policyRefs := range m.policyReferencedObjects {
		// we don't store target pods, because all pods in the target namespace are affected, check namespace
		if policyRefs.targetNamespaces.Has(podNs.Name) {
			policyNames.Insert(policyName)
			continue
		}
		if policyRefs.dynamicGWPods.Has(getPodNamespacedName(pod)) {
			policyNames.Insert(policyName)
		}
	}
	return policyNames, nil
}

func (m *externalPolicyManager) getAllRoutePolicies() ([]*adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute, error) {
	var (
		routePolicies []*adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute
		err           error
	)

	routePolicies, err = m.routeLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("Failed to list Admin Policy Based External Routes:%v", err)
		return nil, err
	}
	return routePolicies, nil
}

// getDynamicGatewayIPsForTargetNamespace is called by the annotation logic to identify if a namespace is managed by an CR.
// Since the call can occur outside the lifecycle of the controller, it cannot rely on the namespace info cache object to have been populated.
// Therefore it has to go through all policies until it identifies one that targets the namespace and retrieve the gateway IPs.
// these IPs are used by the annotation logic to determine which ones to remove from the north bound DB (the ones not included in the list),
// and the ones to keep (the ones that match both the annotation and the CR).
// This logic ensures that both CR and annotations can coexist without duplicating gateway IPs.
func (m *externalPolicyManager) getDynamicGatewayIPsForTargetNamespace(namespaceName string) (sets.Set[string], error) {
	policyGWIPs := sets.New[string]()

	routePolicies, err := m.getAllRoutePolicies()
	if err != nil {
		return nil, err
	}

	for _, routePolicy := range routePolicies {
		targetNamespaces, err := m.listNamespacesBySelector(&routePolicy.Spec.From.NamespaceSelector)
		if err != nil {
			return nil, fmt.Errorf("failed to get APB Policy %s dynamic gateway IPs: failed to list namespaces %v",
				routePolicy.Name, err)
		}
		for _, targetNS := range targetNamespaces {
			if targetNS.Name == namespaceName {
				// only collect the dynamic gateways
				dynamicGWInfo, _, _, err := m.processDynamicHopsGatewayInformation(routePolicy.Spec.NextHops.DynamicHops)
				if err != nil {
					return nil, fmt.Errorf("failed to get APB Policy %s dynamic gateway IPs: failed to process dynamic GW %v",
						routePolicy.Name, err)
				}
				for _, gwInfo := range dynamicGWInfo.Elems() {
					insertSet(policyGWIPs, gwInfo.Gateways)
				}
			}
		}
	}
	return policyGWIPs, nil
}

// getStaticGatewayIPsForTargetNamespace is called by the annotation logic to identify if a namespace is managed by an CR.
// Since the call can occur outside the lifecycle of the controller, it cannot rely on the namespace info cache object to have been populated.
// Therefore it has to go through all policies until it identifies one that targets the namespace and retrieve the gateway IPs.
// these IPs are used by the annotation logic to determine which ones to remove from the north bound DB (the ones not included in the list),
// and the ones to keep (the ones that match both the annotation and the CR).
// This logic ensures that both CR and annotations can coexist without duplicating gateway IPs.
func (m *externalPolicyManager) getStaticGatewayIPsForTargetNamespace(namespaceName string) (sets.Set[string], error) {
	policyGWIPs := sets.New[string]()

	routePolicies, err := m.routeLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("Failed to list Admin Policy Based External Routes:%v", err)
		return nil, err
	}
	for _, routePolicy := range routePolicies {
		targetNamespaces, err := m.listNamespacesBySelector(&routePolicy.Spec.From.NamespaceSelector)
		if err != nil {
			klog.Errorf("Failed to process Admin Policy Based External Route %s: %v", routePolicy.Name, err)
			return nil, err
		}
		for _, targetNS := range targetNamespaces {
			if targetNS.Name == namespaceName {
				// only collect the static gateways
				staticGWInfo, err := m.processStaticHopsGatewayInformation(routePolicy.Spec.NextHops.StaticHops)
				if err != nil {
					klog.Errorf("Failed to process Admin Policy Based External Route %s: %v", routePolicy.Name, err)
					return nil, err
				}
				for _, gwInfo := range staticGWInfo.Elems() {
					insertSet(policyGWIPs, gwInfo.Gateways)
				}
			}
		}
	}
	return policyGWIPs, nil
}
