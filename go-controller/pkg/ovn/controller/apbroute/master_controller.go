package apbroute

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"strings"
	"sync"
	"time"

	nettypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	libovsdbclient "github.com/ovn-org/libovsdb/client"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ktypes "k8s.io/apimachinery/pkg/types"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/retry"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	adminpolicybasedrouteapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1"
	adminpolicybasedrouteclient "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1/apis/clientset/versioned"
	adminpolicybasedrouteinformer "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1/apis/informers/externalversions"
	adminpolicybasedroutelisters "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1/apis/listers/adminpolicybasedroute/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	libovsdbutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

const (
	resyncInterval = 0
	maxRetries     = 15
	ControllerName = "apb-external-route-controller"
)

// Admin Policy Based Route services

type ExternalGatewayMasterController struct {
	client               kubernetes.Interface
	apbRoutePolicyClient adminpolicybasedrouteclient.Interface
	stopCh               <-chan struct{}

	// route policies
	routeLister   adminpolicybasedroutelisters.AdminPolicyBasedExternalRouteLister
	routeInformer cache.SharedIndexInformer
	routeQueue    workqueue.RateLimitingInterface

	// Pods
	podLister   corev1listers.PodLister
	podInformer cache.SharedIndexInformer
	podQueue    workqueue.RateLimitingInterface

	// Namespaces
	namespaceQueue    workqueue.RateLimitingInterface
	namespaceLister   corev1listers.NamespaceLister
	namespaceInformer cache.SharedIndexInformer

	// External gateway caches
	// Make them public so that they can be used by the annotation logic to lock on namespaces and share the same external route information
	ExternalGWCache map[ktypes.NamespacedName]*ExternalRouteInfo
	ExGWCacheMutex  *sync.RWMutex

	routePolicyFactory adminpolicybasedrouteinformer.SharedInformerFactory

	mgr      *externalPolicyManager
	nbClient *northBoundClient
}

func NewExternalMasterController(
	client kubernetes.Interface,
	apbRoutePolicyClient adminpolicybasedrouteclient.Interface,
	stopCh <-chan struct{},
	podInformer coreinformers.PodInformer,
	namespaceInformer coreinformers.NamespaceInformer,
	nodeLister corev1listers.NodeLister,
	nbClient libovsdbclient.Client,
	addressSetFactory addressset.AddressSetFactory,
) (*ExternalGatewayMasterController, error) {

	routePolicyFactory := adminpolicybasedrouteinformer.NewSharedInformerFactory(apbRoutePolicyClient, resyncInterval)
	externalRouteInformer := routePolicyFactory.K8s().V1().AdminPolicyBasedExternalRoutes()
	externalGWCache := make(map[ktypes.NamespacedName]*ExternalRouteInfo)
	exGWCacheMutex := &sync.RWMutex{}
	zone, err := libovsdbutil.GetNBZone(nbClient)
	if err != nil {
		return nil, err
	}
	nbCli := &northBoundClient{
		routeLister:       externalRouteInformer.Lister(),
		nodeLister:        nodeLister,
		podLister:         podInformer.Lister(),
		nbClient:          nbClient,
		addressSetFactory: addressSetFactory,
		externalGWCache:   externalGWCache,
		exGWCacheMutex:    exGWCacheMutex,
		controllerName:    ControllerName,
		zone:              zone,
	}

	c := &ExternalGatewayMasterController{
		client:               client,
		apbRoutePolicyClient: apbRoutePolicyClient,
		stopCh:               stopCh,
		routeLister:          externalRouteInformer.Lister(),
		routeInformer:        externalRouteInformer.Informer(),
		routeQueue: workqueue.NewNamedRateLimitingQueue(
			workqueue.NewItemFastSlowRateLimiter(time.Second, 5*time.Second, 5),
			"adminpolicybasedexternalroutes",
		),
		podLister:   podInformer.Lister(),
		podInformer: podInformer.Informer(),
		podQueue: workqueue.NewNamedRateLimitingQueue(
			workqueue.NewItemFastSlowRateLimiter(time.Second, 5*time.Second, 5),
			"apbexternalroutepods",
		),
		namespaceLister:   namespaceInformer.Lister(),
		namespaceInformer: namespaceInformer.Informer(),
		namespaceQueue: workqueue.NewNamedRateLimitingQueue(
			workqueue.NewItemFastSlowRateLimiter(time.Second, 5*time.Second, 5),
			"apbexternalroutenamespaces",
		),
		ExternalGWCache:    externalGWCache,
		ExGWCacheMutex:     exGWCacheMutex,
		routePolicyFactory: routePolicyFactory,
		nbClient:           nbCli,
		mgr: newExternalPolicyManager(
			stopCh,
			podInformer.Lister(),
			namespaceInformer.Lister(),
			routePolicyFactory.K8s().V1().AdminPolicyBasedExternalRoutes().Lister(),
			nbCli),
	}
	return c, nil
}

func (c *ExternalGatewayMasterController) Run(wg *sync.WaitGroup, threadiness int) error {
	defer utilruntime.HandleCrash()
	klog.V(4).Info("Starting Admin Policy Based Route Controller")

	_, err := c.namespaceInformer.AddEventHandler(
		factory.WithUpdateHandlingForObjReplace(cache.ResourceEventHandlerFuncs{
			AddFunc:    c.onNamespaceAdd,
			UpdateFunc: c.onNamespaceUpdate,
			DeleteFunc: c.onNamespaceDelete,
		}))
	if err != nil {
		return err
	}

	_, err = c.podInformer.AddEventHandler(
		factory.WithUpdateHandlingForObjReplace(cache.ResourceEventHandlerFuncs{
			AddFunc:    c.onPodAdd,
			UpdateFunc: c.onPodUpdate,
			DeleteFunc: c.onPodDelete,
		}))
	if err != nil {
		return err
	}
	_, err = c.routeInformer.AddEventHandler(
		factory.WithUpdateHandlingForObjReplace(cache.ResourceEventHandlerFuncs{
			AddFunc:    c.onPolicyAdd,
			UpdateFunc: c.onPolicyUpdate,
			DeleteFunc: c.onPolicyDelete,
		}))
	if err != nil {
		return err
	}

	c.routePolicyFactory.Start(c.stopCh)

	syncWg := &sync.WaitGroup{}
	syncErrs := []error{}
	for _, se := range []struct {
		resourceName string
		syncFn       cache.InformerSynced
	}{
		{"apbexternalroutenamespaces", c.namespaceInformer.HasSynced},
		{"apbexternalroutepods", c.podInformer.HasSynced},
		{"adminpolicybasedexternalroutes", c.routeInformer.HasSynced},
	} {
		syncWg.Add(1)
		go func(resourceName string, syncFn cache.InformerSynced) {
			defer syncWg.Done()
			if !util.WaitForNamedCacheSyncWithTimeout(resourceName, c.stopCh, syncFn) {
				syncErrs = append(syncErrs, fmt.Errorf("timed out waiting for %q caches to sync", resourceName))
			}
		}(se.resourceName, se.syncFn)
	}
	syncWg.Wait()
	if len(syncErrs) != 0 {
		return kerrors.NewAggregate(syncErrs)
	}

	for i := 0; i < threadiness; i++ {
		for _, workerFn := range []func(*sync.WaitGroup){
			// processes route policies
			c.runPolicyWorker,
			// detects gateway pod changes and updates the pod's IP and MAC in the northbound DB
			c.runPodWorker,
			// detects namespace changes and applies polices that match the namespace selector in the `From` policy field
			c.runNamespaceWorker,
		} {
			wg.Add(1)
			go func(fn func(*sync.WaitGroup)) {
				defer wg.Done()
				wait.Until(func() {
					fn(wg)
				}, time.Second, c.stopCh)
			}(workerFn)
		}
	}

	// add shutdown goroutine waiting for c.stopCh
	wg.Add(1)
	go func() {
		defer wg.Done()
		// wait until we're told to stop
		<-c.stopCh

		c.podQueue.ShutDown()
		c.routeQueue.ShutDown()
		c.namespaceQueue.ShutDown()
	}()

	return nil
}

func (c *ExternalGatewayMasterController) runPolicyWorker(wg *sync.WaitGroup) {
	for c.processNextPolicyWorkItem(wg) {
	}
}

func (c *ExternalGatewayMasterController) processNextPolicyWorkItem(wg *sync.WaitGroup) bool {
	wg.Add(1)
	defer wg.Done()

	key, shutdown := c.routeQueue.Get()

	if shutdown {
		return false
	}

	defer c.routeQueue.Done(key)

	klog.V(4).Infof("Processing policy %s", key)
	gwIPs, err := c.mgr.syncRoutePolicy(key.(string))
	if err != nil {
		klog.Errorf("Failed to sync APB policy %s: %v", key, err)
	}
	// capture the error from processing the sync in the statuses message field
	//TODO decide how to merge statuses from multiple ovnkube-controllers
	statusErr := c.updateStatusAPBExternalRoute(key.(string), gwIPs, err)
	if statusErr != nil {
		klog.Warningf("Failed to update AdminPolicyBasedExternalRoutes %s status: %v", key, statusErr)
	}
	if err != nil {
		if c.routeQueue.NumRequeues(key) < maxRetries {
			klog.V(4).Infof("Error found while processing policy %s: %w", key, err)
			c.routeQueue.AddRateLimited(key)
			return true
		}
		klog.Warningf("Dropping policy %q out of the queue: %w", key, err)
		utilruntime.HandleError(err)
	}
	c.routeQueue.Forget(key)
	return true
}

func (c *ExternalGatewayMasterController) onPolicyAdd(obj interface{}) {
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
	c.routeQueue.Add(key)
}

func (c *ExternalGatewayMasterController) onPolicyUpdate(oldObj, newObj interface{}) {
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
	c.routeQueue.Add(key)
}

func (c *ExternalGatewayMasterController) onPolicyDelete(obj interface{}) {
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
	c.routeQueue.Add(key)
}

func (c *ExternalGatewayMasterController) onNamespaceAdd(obj interface{}) {
	ns, ok := obj.(*v1.Namespace)
	if !ok {
		utilruntime.HandleError(fmt.Errorf("expecting %T but received %T", &v1.Namespace{}, obj))
		return
	}
	if ns == nil {
		utilruntime.HandleError(errors.New("invalid Namespace provided to onNamespaceAdd()"))
		return
	}
	c.namespaceQueue.Add(ns)
}

func (c *ExternalGatewayMasterController) onNamespaceUpdate(oldObj, newObj interface{}) {
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
	c.namespaceQueue.Add(newNamespace)
}

func (c *ExternalGatewayMasterController) onNamespaceDelete(obj interface{}) {
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
		c.namespaceQueue.Add(ns)
	}
}

func (c *ExternalGatewayMasterController) runNamespaceWorker(wg *sync.WaitGroup) {
	for c.processNextNamespaceWorkItem(wg) {

	}
}

func (c *ExternalGatewayMasterController) processNextNamespaceWorkItem(wg *sync.WaitGroup) bool {
	wg.Add(1)
	defer wg.Done()

	obj, shutdown := c.namespaceQueue.Get()

	if shutdown {
		return false
	}

	defer c.namespaceQueue.Done(obj)

	err := c.mgr.syncNamespace(obj.(*v1.Namespace), c.routeQueue)
	if err != nil {
		if c.namespaceQueue.NumRequeues(obj) < maxRetries {
			klog.V(4).Infof("Error found while processing namespace %s:%w", obj.(*v1.Namespace), err)
			c.namespaceQueue.AddRateLimited(obj)
			return true
		}
		klog.Warningf("Dropping namespace %q out of the queue: %v", obj.(*v1.Namespace).Name, err)
		utilruntime.HandleError(err)
	}
	c.namespaceQueue.Forget(obj)
	return true
}

func (c *ExternalGatewayMasterController) onPodAdd(obj interface{}) {
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
	c.podQueue.Add(pod)
}

func (c *ExternalGatewayMasterController) onPodUpdate(oldObj, newObj interface{}) {
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
	c.podQueue.Add(newObj)
}

func (c *ExternalGatewayMasterController) onPodDelete(obj interface{}) {
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
		c.podQueue.Add(pod)
	}
}

func (c *ExternalGatewayMasterController) runPodWorker(wg *sync.WaitGroup) {
	for c.processNextPodWorkItem(wg) {
	}
}

func (c *ExternalGatewayMasterController) processNextPodWorkItem(wg *sync.WaitGroup) bool {
	wg.Add(1)
	defer wg.Done()

	obj, shutdown := c.podQueue.Get()

	if shutdown {
		return false
	}

	defer c.podQueue.Done(obj)

	p := obj.(*v1.Pod)
	err := c.mgr.syncPod(p, c.routeQueue)
	if err != nil {
		if c.podQueue.NumRequeues(obj) < maxRetries {
			klog.V(4).Infof("Error found while processing pod %s/%s:%v", p.Namespace, p.Name, err)
			c.podQueue.AddRateLimited(obj)
			return true
		}
		klog.Warningf("Dropping pod %s/%s out of the queue: %s", p.Namespace, p.Name, err)
		utilruntime.HandleError(err)
	}

	c.podQueue.Forget(obj)
	return true
}

// updateStatusAPBExternalRoute updates the CR with the current status of the CR instance, including errors captured while processing the CR during its lifetime
func (c *ExternalGatewayMasterController) updateStatusAPBExternalRoute(policyName string, gwIPs sets.Set[string],
	processedError error) error {
	if gwIPs == nil {
		// policy doesn't exist anymore, nothing to do
		return nil
	}

	resultErr := retry.RetryOnConflict(util.OvnConflictBackoff, func() error {
		routePolicy, err := c.apbRoutePolicyClient.K8sV1().AdminPolicyBasedExternalRoutes().Get(context.TODO(), policyName, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				// policy doesn't exist, no need to update status
				return nil
			}
			return err
		}

		updateStatus(routePolicy, strings.Join(sets.List(gwIPs), ","), processedError)

		_, err = c.apbRoutePolicyClient.K8sV1().AdminPolicyBasedExternalRoutes().UpdateStatus(context.TODO(), routePolicy, metav1.UpdateOptions{})
		if !apierrors.IsNotFound(err) {
			return err
		}
		return nil
	})
	if resultErr != nil {
		return fmt.Errorf("failed to update AdminPolicyBasedExternalRoutes %s status: %v", policyName, resultErr)
	}
	return nil
}

func (c *ExternalGatewayMasterController) GetDynamicGatewayIPsForTargetNamespace(namespaceName string) (sets.Set[string], error) {
	return c.mgr.getDynamicGatewayIPsForTargetNamespace(namespaceName)
}

func (c *ExternalGatewayMasterController) GetStaticGatewayIPsForTargetNamespace(namespaceName string) (sets.Set[string], error) {
	return c.mgr.getStaticGatewayIPsForTargetNamespace(namespaceName)
}

func updateStatus(route *adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute, gwIPs string, err error) {
	if err != nil {
		route.Status.Status = adminpolicybasedrouteapi.FailStatus
		route.Status.Messages = append(route.Status.Messages, fmt.Sprintf("Failed to apply policy: %v", err.Error()))
		return
	}
	route.Status.LastTransitionTime = metav1.Time{Time: time.Now()}
	route.Status.Status = adminpolicybasedrouteapi.SuccessStatus
	route.Status.Messages = append(route.Status.Messages, fmt.Sprintf("Configured external gateway IPs: %s", gwIPs))
	klog.V(4).Infof("Updating Admin Policy Based External Route %s with Status: %s, Message: %s", route.Name, route.Status.Status, route.Status.Messages[len(route.Status.Messages)-1])
}

// AddHybridRoutePolicyForPod exposes the function addHybridRoutePolicyForPod
func (c *ExternalGatewayMasterController) AddHybridRoutePolicyForPod(podIP net.IP, node string) error {
	return c.nbClient.addHybridRoutePolicyForPod(podIP, node)
}

// DelHybridRoutePolicyForPod exposes the function delHybridRoutePolicyForPod
func (c *ExternalGatewayMasterController) DelHybridRoutePolicyForPod(podIP net.IP, node string) error {
	return c.nbClient.delHybridRoutePolicyForPod(podIP, node)
}

// DelAllHybridRoutePolicies exposes the function delAllHybridRoutePolicies
func (c *ExternalGatewayMasterController) DelAllHybridRoutePolicies() error {
	return c.nbClient.delAllHybridRoutePolicies()
}

// DelAllLegacyHybridRoutePolicies exposes the function delAllLegacyHybridRoutePolicies
func (c *ExternalGatewayMasterController) DelAllLegacyHybridRoutePolicies() error {
	return c.nbClient.delAllLegacyHybridRoutePolicies()
}

// DeletePodSNAT exposes the function deletePodSNAT
func (c *ExternalGatewayMasterController) DeletePodSNAT(nodeName string, extIPs, podIPNets []*net.IPNet) error {
	return c.nbClient.deletePodSNAT(nodeName, extIPs, podIPNets)
}

func (c *ExternalGatewayMasterController) GetAPBRoutePolicyStatus(policyName string) (*adminpolicybasedrouteapi.AdminPolicyBasedRouteStatus, error) {
	pol, err := c.routeLister.Get(policyName)
	if err != nil {
		return nil, err
	}
	return &pol.Status, nil
}
