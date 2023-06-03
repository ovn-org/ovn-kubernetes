package admin_network_policy

import (
	"fmt"
	"reflect"
	"sync"
	"time"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	corev1informers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	anpapi "sigs.k8s.io/network-policy-api/apis/v1alpha1"
	anpclientset "sigs.k8s.io/network-policy-api/pkg/client/clientset/versioned"
	anpinformer "sigs.k8s.io/network-policy-api/pkg/client/informers/externalversions/apis/v1alpha1"
	anplister "sigs.k8s.io/network-policy-api/pkg/client/listers/apis/v1alpha1"
)

const (
	// maxRetries is the number of times a object will be retried before it is dropped out of the queue.
	// With the current rate-limiter in use (5ms*2^(maxRetries-1)) the following numbers represent the
	// sequence of delays between successive queuings of an object.
	//
	// 5ms, 10ms, 20ms, 40ms, 80ms, 160ms, 320ms, 640ms, 1.3s, 2.6s, 5.1s, 10.2s, 20.4s, 41s, 82s
	maxRetries = 15

	controllerName = "admin-network-policy-controller"
)

// Controller holds the fields required for ANP controller
// taken from k8s controller guidelines
type Controller struct {
	sync.Mutex
	client       kubernetes.Interface
	anpClientSet anpclientset.Interface

	// libovsdb northbound client interface
	nbClient      libovsdbclient.Client
	eventRecorder record.EventRecorder
	anpCache      sync.Map // map that keeps the cloned value of ANP kapi
	banpCache     sync.Map // map that keeps the cloned value of BANP kapi
	// If more than one ANP is created at the same priority behaviour is undefined in k8s upsteam.
	// If more than one ANP is created at the same priority OVNK will only create the first
	// incoming ANP, rest of them will not be created. This map tracks priority->anpName
	anpPriorityMap sync.Map
	// An address set factory that creates address sets
	addressSetFactory addressset.AddressSetFactory

	// queues for the CRDs where incoming work is placed to de-dup
	anpQueue  workqueue.RateLimitingInterface
	banpQueue workqueue.RateLimitingInterface
	// cached access to anp and banp objects
	anpLister       anplister.AdminNetworkPolicyLister
	banpLister      anplister.BaselineAdminNetworkPolicyLister
	anpCacheSynced  cache.InformerSynced
	banpCacheSynced cache.InformerSynced
	// namespace queue, cache, lister
	anpNamespaceLister corev1listers.NamespaceLister
	anpNamespaceSynced cache.InformerSynced
	anpNamespaceQueue  workqueue.RateLimitingInterface
	// pod queue, cache, lister
	anpPodLister corev1listers.PodLister
	anpPodSynced cache.InformerSynced
	anpPodQueue  workqueue.RateLimitingInterface
}

// NewController returns a new *Controller.
func NewController(
	client kubernetes.Interface,
	nbClient libovsdbclient.Client,
	anpClient anpclientset.Interface,
	anpInformer anpinformer.AdminNetworkPolicyInformer,
	banpInformer anpinformer.BaselineAdminNetworkPolicyInformer,
	namespaceInformer corev1informers.NamespaceInformer,
	podInformer corev1informers.PodInformer,
	addressSetFactory addressset.AddressSetFactory,
	recorder record.EventRecorder) (*Controller, error) {

	c := &Controller{
		client:            client,
		nbClient:          nbClient,
		anpClientSet:      anpClient,
		addressSetFactory: addressSetFactory,
	}

	klog.Info("Setting up event handlers for Admin Network Policy")
	// setup anp informers, listers, queue
	c.anpLister = anpInformer.Lister()
	c.anpCacheSynced = anpInformer.Informer().HasSynced
	c.anpQueue = workqueue.NewNamedRateLimitingQueue(
		workqueue.NewItemFastSlowRateLimiter(1*time.Second, 5*time.Second, 5),
		"adminNetworkPolicy",
	)
	_, err := anpInformer.Informer().AddEventHandler(factory.WithUpdateHandlingForObjReplace(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.onANPAdd,
		UpdateFunc: c.onANPUpdate,
		DeleteFunc: c.onANPDelete,
	}))
	if err != nil {
		return nil, fmt.Errorf("could not add Event Handler for anpInformer during admin network policy controller initialization, %w", err)

	}

	klog.Info("Setting up event handlers for Baseline Admin Network Policy")
	// setup banp informers, listers, queue
	c.banpLister = banpInformer.Lister()
	c.banpCacheSynced = banpInformer.Informer().HasSynced
	c.banpQueue = workqueue.NewNamedRateLimitingQueue(
		workqueue.NewItemFastSlowRateLimiter(1*time.Second, 5*time.Second, 5),
		"baselineAdminNetworkPolicy",
	)
	_, err = banpInformer.Informer().AddEventHandler(factory.WithUpdateHandlingForObjReplace(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.onBANPAdd,
		UpdateFunc: c.onBANPUpdate,
		DeleteFunc: c.onBANPDelete,
	}))
	if err != nil {
		return nil, fmt.Errorf("could not add Event Handler for banpInformer during admin network policy controller initialization, %w", err)
	}

	klog.Info("Setting up event handlers for Namespaces in Admin Network Policy controller")
	c.anpNamespaceLister = namespaceInformer.Lister()
	c.anpNamespaceSynced = namespaceInformer.Informer().HasSynced
	c.anpNamespaceQueue = workqueue.NewNamedRateLimitingQueue(
		workqueue.NewItemFastSlowRateLimiter(1*time.Second, 5*time.Second, 5),
		"anpNamespaces",
	)
	_, err = namespaceInformer.Informer().AddEventHandler(factory.WithUpdateHandlingForObjReplace(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.onANPNamespaceAdd,
		UpdateFunc: c.onANPNamespaceUpdate,
		DeleteFunc: c.onANPNamespaceDelete,
	}))
	if err != nil {
		return nil, fmt.Errorf("could not add Event Handler for namespace Informer during admin network policy controller initialization, %w", err)
	}

	klog.Info("Setting up event handlers for Pods in Admin Network Policy controller")
	c.anpPodLister = podInformer.Lister()
	c.anpPodSynced = podInformer.Informer().HasSynced
	c.anpPodQueue = workqueue.NewNamedRateLimitingQueue(
		workqueue.NewItemFastSlowRateLimiter(1*time.Second, 5*time.Second, 5),
		"anpPods",
	)
	_, err = podInformer.Informer().AddEventHandler(factory.WithUpdateHandlingForObjReplace(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.onANPPodAdd,
		UpdateFunc: c.onANPPodUpdate,
		DeleteFunc: c.onANPPodDelete,
	}))
	if err != nil {
		return nil, fmt.Errorf("could not add Event Handler for pod Informer during admin network policy controller initialization, %w", err)
	}

	c.eventRecorder = recorder

	return c, nil
}

// Run will not return until stopCh is closed. workers determines how many
// endpoints will be handled in parallel.
func (c *Controller) Run(threadiness int, stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()

	klog.Infof("Starting controller %s", controllerName)

	// Wait for the caches to be synced
	klog.Info("Waiting for informer caches to sync")
	if !cache.WaitForNamedCacheSync(controllerName, stopCh, c.anpCacheSynced, c.banpCacheSynced, c.anpNamespaceSynced, c.anpPodSynced) {
		utilruntime.HandleError(fmt.Errorf("timed out waiting for caches to sync"))
		klog.Errorf("error syncing caches for admin network policy and baseline admin network policy")
		return
	}

	klog.Infof("Repairing Admin Network Policies")
	// Run the repair function at startup so that we synchronize KAPI and OVNDBs
	err := c.repairAdminNetworkPolicies()
	if err != nil {
		klog.Errorf("Failed to repair Admin Network Policies: %v", err)
	}
	err = c.repairBaselineAdminNetworkPolicy()
	if err != nil {
		klog.Errorf("Failed to repair Baseline Admin Network Policy: %v", err)
	}

	wg := &sync.WaitGroup{}
	// Start the workers after the repair loop to avoid races
	klog.Info("Starting Admin Network Policy workers")
	for i := 0; i < threadiness; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wait.Until(func() {
				c.runANPWorker(wg)
			}, time.Second, stopCh)
		}()
	}

	klog.Info("Starting Baseline Admin Network Policy workers")
	for i := 0; i < threadiness; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wait.Until(func() {
				c.runBANPWorker(wg)
			}, time.Second, stopCh)
		}()
	}

	klog.Info("Starting Namespace Admin Network Policy workers")
	for i := 0; i < threadiness; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wait.Until(func() {
				c.runANPNamespaceWorker(wg)
			}, time.Second, stopCh)
		}()
	}

	klog.Info("Starting Pod Admin Network Policy workers")
	for i := 0; i < threadiness; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wait.Until(func() {
				c.runANPPodWorker(wg)
			}, time.Second, stopCh)
		}()
	}

	<-stopCh

	klog.Infof("Shutting down controller %s", controllerName)
	c.anpQueue.ShutDown()
	c.banpQueue.ShutDown()
	c.anpNamespaceQueue.ShutDown()
	c.anpPodQueue.ShutDown()
	wg.Wait()
}

// worker runs a worker thread that just dequeues items, processes them, and
// marks them done. You may run as many of these in parallel as you wish; the
// workqueue guarantees that they will not end up processing the same service
// at the same time.
func (c *Controller) runANPWorker(wg *sync.WaitGroup) {
	for c.processNextANPWorkItem(wg) {
	}
}

func (c *Controller) runBANPWorker(wg *sync.WaitGroup) {
	for c.processNextBANPWorkItem(wg) {
	}
}

func (c *Controller) runANPNamespaceWorker(wg *sync.WaitGroup) {
	for c.processNextANPNamespaceWorkItem(wg) {
	}
}

func (c *Controller) runANPPodWorker(wg *sync.WaitGroup) {
	for c.processNextANPPodWorkItem(wg) {
	}
}

func (c *Controller) processNextANPWorkItem(wg *sync.WaitGroup) bool {
	wg.Add(1)
	defer wg.Done()
	anpKey, quit := c.anpQueue.Get()
	if quit {
		return false
	}
	defer c.anpQueue.Done(anpKey)

	err := c.syncAdminNetworkPolicy(anpKey.(string))
	if err == nil {
		c.anpQueue.Forget(anpKey)
		return true
	}
	klog.Infof("SURYA")
	utilruntime.HandleError(fmt.Errorf("%v failed with : %v", anpKey, err))

	if c.anpQueue.NumRequeues(anpKey) < maxRetries {
		c.anpQueue.AddRateLimited(anpKey)
		return true
	}

	c.anpQueue.Forget(anpKey)
	return true
}

func (c *Controller) processNextBANPWorkItem(wg *sync.WaitGroup) bool {
	wg.Add(1)
	defer wg.Done()
	banpKey, quit := c.banpQueue.Get()
	if quit {
		return false
	}
	defer c.banpQueue.Done(banpKey)

	err := c.syncBaselineAdminNetworkPolicy(banpKey.(string))
	if err == nil {
		c.banpQueue.Forget(banpKey)
		return true
	}

	utilruntime.HandleError(fmt.Errorf("%v failed with : %v", banpKey, err))

	if c.banpQueue.NumRequeues(banpKey) < maxRetries {
		c.banpQueue.AddRateLimited(banpKey)
		return true
	}

	c.banpQueue.Forget(banpKey)
	return true
}

func (c *Controller) processNextANPNamespaceWorkItem(wg *sync.WaitGroup) bool {
	wg.Add(1)
	defer wg.Done()
	anpNSKey, quit := c.anpNamespaceQueue.Get()
	if quit {
		return false
	}
	defer c.anpNamespaceQueue.Done(anpNSKey)

	err := c.syncNamespaceAdminNetworkPolicy(anpNSKey.(string))
	if err == nil {
		c.anpNamespaceQueue.Forget(anpNSKey)
		return true
	}

	utilruntime.HandleError(fmt.Errorf("%v failed with : %v", anpNSKey, err))

	if c.anpNamespaceQueue.NumRequeues(anpNSKey) < maxRetries {
		c.anpNamespaceQueue.AddRateLimited(anpNSKey)
		return true
	}

	c.anpNamespaceQueue.Forget(anpNSKey)
	return true
}

func (c *Controller) processNextANPPodWorkItem(wg *sync.WaitGroup) bool {
	wg.Add(1)
	defer wg.Done()
	anpPodKey, quit := c.anpPodQueue.Get()
	if quit {
		return false
	}
	defer c.anpPodQueue.Done(anpPodKey)
	klog.Infof("SURYA")
	err := c.syncPodAdminNetworkPolicy(anpPodKey.(string))
	if err == nil {
		c.anpPodQueue.Forget(anpPodKey)
		return true
	}

	utilruntime.HandleError(fmt.Errorf("%v failed with : %v", anpPodKey, err))

	if c.anpPodQueue.NumRequeues(anpPodKey) < maxRetries {
		klog.Infof("SURYA %v", err)
		c.anpPodQueue.AddRateLimited(anpPodKey)
		return true
	}

	c.anpPodQueue.Forget(anpPodKey)
	return true
}

// handlers

// onANPAdd queues the ANP for processing.
func (c *Controller) onANPAdd(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", obj, err))
		return
	}
	klog.V(4).Infof("Adding Admin Network Policy %s", key)
	c.anpQueue.Add(key)
}

// onANPUpdate updates the ANP Selector in the cache and queues the ANP for processing.
func (c *Controller) onANPUpdate(oldObj, newObj interface{}) {
	oldANP := oldObj.(*anpapi.AdminNetworkPolicy)
	newANP := newObj.(*anpapi.AdminNetworkPolicy)

	// don't process resync or objects that are marked for deletion
	if oldANP.ResourceVersion == newANP.ResourceVersion ||
		!newANP.GetDeletionTimestamp().IsZero() {
		return
	}
	if oldANP.Spec.Priority == newANP.Spec.Priority &&
		reflect.DeepEqual(oldANP.Spec.Subject, newANP.Spec.Subject) &&
		reflect.DeepEqual(oldANP.Spec.Ingress, newANP.Spec.Ingress) &&
		reflect.DeepEqual(oldANP.Spec.Egress, newANP.Spec.Egress) {
		return
	}
	key, err := cache.MetaNamespaceKeyFunc(newObj)
	if err == nil {
		klog.V(4).Infof("Updating Admin Network Policy %s", key)
		c.anpQueue.Add(key)
	}
}

// onANPDelete queues the ANP for processing.
func (c *Controller) onANPDelete(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", obj, err))
		return
	}
	klog.V(4).Infof("Deleting Admin Network Policy %s", key)
	c.anpQueue.Add(key)
}

// onBANPAdd queues the BANP for processing.
func (c *Controller) onBANPAdd(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", obj, err))
		return
	}
	klog.V(4).Infof("Adding Baseline Admin Network Policy %s", key)
	c.banpQueue.Add(key)
}

// onBANPUpdate updates the BANP Selector in the cache and queues the ANP for processing.
func (c *Controller) onBANPUpdate(oldObj, newObj interface{}) {
	oldBANP := oldObj.(*anpapi.BaselineAdminNetworkPolicy)
	newBANP := newObj.(*anpapi.BaselineAdminNetworkPolicy)

	// don't process resync or objects that are marked for deletion
	if oldBANP.ResourceVersion == newBANP.ResourceVersion ||
		!newBANP.GetDeletionTimestamp().IsZero() {
		return
	}

	if reflect.DeepEqual(oldBANP.Spec.Subject, newBANP.Spec.Subject) &&
		reflect.DeepEqual(oldBANP.Spec.Ingress, newBANP.Spec.Ingress) &&
		reflect.DeepEqual(oldBANP.Spec.Egress, newBANP.Spec.Egress) {
		return
	}

	key, err := cache.MetaNamespaceKeyFunc(newObj)
	if err == nil {
		klog.V(4).Infof("Updating Baseline Admin Network Policy %s", key)
		c.banpQueue.Add(key)
	}
}

// onBANPDelete queues the BANP for processing.
func (c *Controller) onBANPDelete(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", obj, err))
		return
	}
	klog.V(4).Infof("Deleting Baseline Admin Network Policy %s", key)
	c.banpQueue.Add(key)
}

// onANPNamespaceAdd queues the namespace for processing.
func (c *Controller) onANPNamespaceAdd(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", obj, err))
		return
	}
	klog.V(4).Infof("Adding Namespace in Admin Network Policy controller %s", key)
	c.anpNamespaceQueue.Add(key)
}

// onANPNamespaceUpdate queues the namespace for processing.
func (c *Controller) onANPNamespaceUpdate(oldObj, newObj interface{}) {
	oldNamespace := oldObj.(*v1.Namespace)
	newNamespace := newObj.(*v1.Namespace)

	// don't process resync or objects that are marked for deletion
	if oldNamespace.ResourceVersion == newNamespace.ResourceVersion ||
		!newNamespace.GetDeletionTimestamp().IsZero() {
		return
	}
	// If the labels have not changed, then there's no change that we care about: return.
	oldNamespaceLabels := labels.Set(oldNamespace.Labels)
	newNamespaceLabels := labels.Set(newNamespace.Labels)
	if labels.Equals(oldNamespaceLabels, newNamespaceLabels) {
		return
	}
	key, err := cache.MetaNamespaceKeyFunc(newObj)
	if err == nil {
		klog.V(4).Infof("Updating Namespace in Admin Network Policy controller %s", key)
		c.anpNamespaceQueue.Add(key)
	}
}

// onANPNamespaceDelete queues the namespace for processing.
func (c *Controller) onANPNamespaceDelete(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", obj, err))
		return
	}
	klog.V(4).Infof("Deleting Namespace in Admin Network Policy %s", key)
	c.anpNamespaceQueue.Add(key)
}

// onANPPodAdd queues the pod for processing.
func (c *Controller) onANPPodAdd(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", obj, err))
		return
	}
	klog.V(4).Infof("Adding Pod in Admin Network Policy controller %s", key)
	c.anpPodQueue.Add(key)
}

// onANPPodUpdate queues the pod for processing.
func (c *Controller) onANPPodUpdate(oldObj, newObj interface{}) {
	oldPod := oldObj.(*v1.Pod)
	newPod := newObj.(*v1.Pod)

	// don't process resync or objects that are marked for deletion
	if oldPod.ResourceVersion == newPod.ResourceVersion ||
		!newPod.GetDeletionTimestamp().IsZero() {
		return
	}
	// We only care about pod's label changes, pod's IP changes
	// and pod going into completed state. Rest of the cases we may return
	oldPodLabels := labels.Set(oldPod.Labels)
	newPodLabels := labels.Set(newPod.Labels)
	oldPodIPs, _ := util.GetPodIPsOfNetwork(oldPod, &util.DefaultNetInfo{})
	newPodIPs, _ := util.GetPodIPsOfNetwork(newPod, &util.DefaultNetInfo{})
	oldPodCompleted := util.PodCompleted(oldPod)
	newPodCompleted := util.PodCompleted(newPod)
	if labels.Equals(oldPodLabels, newPodLabels) &&
		// check for podIP changes to be on safe-side (in case we allocate and deallocate) or for dualstack conversion
		len(oldPodIPs) == len(newPodIPs) &&
		oldPodCompleted == newPodCompleted {
		return
	}
	key, err := cache.MetaNamespaceKeyFunc(newObj)
	if err == nil {
		klog.V(4).Infof("Updating Pod in Admin Network Policy controller %s", key)
		c.anpPodQueue.Add(key)
	}
}

// onANPPodDelete queues the namespace for processing.
func (c *Controller) onANPPodDelete(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", obj, err))
		return
	}
	klog.V(4).Infof("Deleting Pod Admin Network Policy %s", key)
	c.anpPodQueue.Add(key)
}
