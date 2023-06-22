package apbroute

import (
	"context"
	"errors"
	"fmt"
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
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	adminpolicybasedrouteapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1"
	adminpolicybasedrouteclient "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1/apis/clientset/versioned"
	adminpolicybasedrouteinformer "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1/apis/informers/externalversions"
	adminpolicybasedroutelisters "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1/apis/listers/adminpolicybasedroute/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
)

const (
	resyncInterval = 0
	maxRetries     = 15
)

var (
	controllerName string
)

// Admin Policy Based Route services

type ExternalGatewayMasterController struct {
	client               kubernetes.Interface
	apbRoutePolicyClient adminpolicybasedrouteclient.Interface
	stopCh               <-chan struct{}

	// route policies

	// routerInformer v1apbinformer.AdminPolicyBasedExternalRouteInformer
	routeLister adminpolicybasedroutelisters.AdminPolicyBasedExternalRouteLister
	routeSynced cache.InformerSynced
	routeQueue  workqueue.RateLimitingInterface

	// Pods
	podLister corev1listers.PodLister
	podSynced cache.InformerSynced
	podQueue  workqueue.RateLimitingInterface

	// Namespaces
	namespaceQueue  workqueue.RateLimitingInterface
	namespaceLister corev1listers.NamespaceLister
	namespaceSynced cache.InformerSynced

	// External gateway caches
	// Make them public so that they can be used by the annotation logic to lock on namespaces and share the same external route information
	ExternalGWCache map[ktypes.NamespacedName]*ExternalRouteInfo
	ExGWCacheMutex  *sync.RWMutex

	routePolicyInformer adminpolicybasedrouteinformer.SharedInformerFactory

	mgr      *externalPolicyManager
	nbClient *northBoundClient
}

func NewExternalMasterController(
	parentControllerName string,
	client kubernetes.Interface,
	apbRoutePolicyClient adminpolicybasedrouteclient.Interface,
	stopCh <-chan struct{},
	podInformer coreinformers.PodInformer,
	namespaceInformer coreinformers.NamespaceInformer,
	nodeLister corev1listers.NodeLister,
	nbClient libovsdbclient.Client,
	addressSetFactory addressset.AddressSetFactory,
) (*ExternalGatewayMasterController, error) {

	controllerName = parentControllerName
	routePolicyInformer := adminpolicybasedrouteinformer.NewSharedInformerFactory(apbRoutePolicyClient, resyncInterval)
	externalRouteInformer := routePolicyInformer.K8s().V1().AdminPolicyBasedExternalRoutes()
	externalGWCache := make(map[ktypes.NamespacedName]*ExternalRouteInfo)
	exGWCacheMutex := &sync.RWMutex{}
	nbCli := &northBoundClient{
		routeLister:       externalRouteInformer.Lister(),
		nodeLister:        nodeLister,
		nbClient:          nbClient,
		addressSetFactory: addressSetFactory,
		externalGWCache:   externalGWCache,
		exGWCacheMutex:    exGWCacheMutex,
	}

	c := &ExternalGatewayMasterController{
		client:               client,
		apbRoutePolicyClient: apbRoutePolicyClient,
		stopCh:               stopCh,
		routeLister:          externalRouteInformer.Lister(),
		routeSynced:          externalRouteInformer.Informer().HasSynced,
		routeQueue: workqueue.NewNamedRateLimitingQueue(
			workqueue.NewItemFastSlowRateLimiter(time.Second, 5*time.Second, 5),
			"adminpolicybasedexternalroutes",
		),
		podLister: podInformer.Lister(),
		podSynced: podInformer.Informer().HasSynced,
		podQueue: workqueue.NewNamedRateLimitingQueue(
			workqueue.NewItemFastSlowRateLimiter(time.Second, 5*time.Second, 5),
			"apbexternalroutepods",
		),
		namespaceLister: namespaceInformer.Lister(),
		namespaceSynced: namespaceInformer.Informer().HasSynced,
		namespaceQueue: workqueue.NewNamedRateLimitingQueue(
			workqueue.NewItemFastSlowRateLimiter(time.Second, 5*time.Second, 5),
			"apbexternalroutenamespaces",
		),
		ExternalGWCache:     externalGWCache,
		ExGWCacheMutex:      exGWCacheMutex,
		routePolicyInformer: routePolicyInformer,
		nbClient:            nbCli,
		mgr: newExternalPolicyManager(
			stopCh,
			podInformer.Lister(),
			namespaceInformer.Lister(),
			routePolicyInformer.K8s().V1().AdminPolicyBasedExternalRoutes().Lister(),
			nbCli),
	}

	_, err := namespaceInformer.Informer().AddEventHandler(
		factory.WithUpdateHandlingForObjReplace(cache.ResourceEventHandlerFuncs{
			AddFunc:    c.onNamespaceAdd,
			UpdateFunc: c.onNamespaceUpdate,
			DeleteFunc: c.onNamespaceDelete,
		}))
	if err != nil {
		return nil, err
	}

	_, err = podInformer.Informer().AddEventHandler(
		factory.WithUpdateHandlingForObjReplace(cache.ResourceEventHandlerFuncs{
			AddFunc:    c.onPodAdd,
			UpdateFunc: c.onPodUpdate,
			DeleteFunc: c.onPodDelete,
		}))
	if err != nil {
		return nil, err
	}
	_, err = externalRouteInformer.Informer().AddEventHandler(
		factory.WithUpdateHandlingForObjReplace(cache.ResourceEventHandlerFuncs{
			AddFunc:    c.onPolicyAdd,
			UpdateFunc: c.onPolicyUpdate,
			DeleteFunc: c.onPolicyDelete,
		}))
	if err != nil {
		return nil, err
	}

	return c, nil

}

func (c *ExternalGatewayMasterController) Run(threadiness int) {
	defer utilruntime.HandleCrash()
	klog.Infof("Starting Admin Policy Based Route Controller")

	c.routePolicyInformer.Start(c.stopCh)

	syncWg := &sync.WaitGroup{}

	for _, se := range []struct {
		resourceName string
		syncFn       cache.InformerSynced
	}{
		{"apbexternalroutenamespaces", c.namespaceSynced},
		{"apbexternalroutepods", c.podSynced},
		{"adminpolicybasedexternalroutes", c.routeSynced},
	} {
		syncWg.Add(1)
		go func(resourceName string, syncFn cache.InformerSynced) {
			defer syncWg.Done()
			if !cache.WaitForNamedCacheSync(resourceName, c.stopCh, syncFn) {
				utilruntime.HandleError(fmt.Errorf("timed out waiting for %q caches to sync", resourceName))
			}
		}(se.resourceName, se.syncFn)
	}
	syncWg.Wait()

	klog.Infof("Repairing Admin Policy Based External Route Services")
	c.repair()

	startWg := &sync.WaitGroup{}
	wg := &sync.WaitGroup{}
	for i := 0; i < threadiness; i++ {
		for _, workerFn := range []func(*sync.WaitGroup){
			// processes route policies
			c.runPolicyWorker,
			// detects gateway pod changes and updates the pod's IP and MAC in the northbound DB
			c.runPodWorker,
			// detects namespace changes and applies polices that match the namespace selector in the `From` policy field
			c.runNamespaceWorker,
		} {
			startWg.Add(1)
			wg.Add(1)
			go func(fn func(*sync.WaitGroup)) {
				startWg.Done()
				defer wg.Done()
				wait.Until(func() {
					fn(wg)
				}, time.Second, c.stopCh)
			}(workerFn)
		}
	}
	startWg.Wait()

	// wait until we're told to stop
	<-c.stopCh

	c.podQueue.ShutDown()
	c.routeQueue.ShutDown()
	c.namespaceQueue.ShutDown()

	wg.Wait()
}

func (c *ExternalGatewayMasterController) runPolicyWorker(wg *sync.WaitGroup) {
	for c.processNextPolicyWorkItem(wg) {
	}
}

func (c *ExternalGatewayMasterController) processNextPolicyWorkItem(wg *sync.WaitGroup) bool {
	wg.Add(1)
	defer wg.Done()

	obj, shutdown := c.routeQueue.Get()

	if shutdown {
		return false
	}

	defer c.routeQueue.Done(obj)

	item := obj.(*adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute)
	klog.Infof("Processing policy %s", item.Name)
	err := c.syncRoutePolicy(item)
	if err != nil {
		if c.routeQueue.NumRequeues(item) < maxRetries {
			klog.V(2).InfoS("Error found while processing policy: %w", err)
			c.routeQueue.AddRateLimited(item)
			return true
		}
		klog.Warningf("Dropping policy %q out of the queue: %w", item.Name, err)
		utilruntime.HandleError(err)
	}
	c.routeQueue.Forget(obj)
	return true
}

func (c *ExternalGatewayMasterController) syncRoutePolicy(routePolicy *adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute) error {
	_, err := c.routeLister.Get(routePolicy.Name)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if apierrors.IsNotFound(err) {
		// DELETE use case
		klog.Infof("Deleting policy %s", routePolicy.Name)
		err = c.mgr.processDeletePolicy(routePolicy.Name)
		if err != nil {
			return fmt.Errorf("failed to delete Admin Policy Based External Route %s:%w", routePolicy.Name, err)
		}
		klog.Infof("Policy %s deleted", routePolicy.Name)
		return nil
	}
	currentPolicy, found, markedForDeletion := c.mgr.getRoutePolicyFromCache(routePolicy.Name)
	if markedForDeletion {
		klog.Warningf("Attempting to add or update route policy %s when it has been marked for deletion. Skipping...", routePolicy.Name)
		return nil
	}
	if !found {
		// ADD use case
		klog.Infof("Adding policy %s", routePolicy.Name)
		pp, err := c.mgr.processAddPolicy(routePolicy)
		newErr := c.updateStatusAPBExternalRoute(routePolicy.Name, pp, err)
		if err != nil {
			return fmt.Errorf("failed to create Admin Policy Based External Route %s:%w", routePolicy.Name, err)
		}
		if newErr != nil {
			return fmt.Errorf("failed to update status in Admin Policy Based External Route %s:%w", routePolicy.Name, newErr)
		}
		return nil
	}
	// UPDATE use case
	klog.Infof("Updating policy %s", routePolicy.Name)
	pp, err := c.mgr.processUpdatePolicy(&currentPolicy, routePolicy)
	newErr := c.updateStatusAPBExternalRoute(routePolicy.Name, pp, err)
	if err != nil {
		return fmt.Errorf("failed to update Admin Policy Based External Route %s:%w", routePolicy.Name, err)
	}
	if newErr != nil {
		return fmt.Errorf("failed to update status in Admin Policy Based External Route %s:%w", routePolicy.Name, newErr)
	}
	return nil
}

func (c *ExternalGatewayMasterController) onPolicyAdd(obj interface{}) {
	policy, ok := obj.(*adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute)
	if !ok {
		utilruntime.HandleError(fmt.Errorf("expecting %T but received %T", &adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute{}, obj))
		return
	}
	if policy == nil {
		utilruntime.HandleError(errors.New("invalid Admin Policy Based External Route provided to onPolicyAdd()"))
		return
	}
	c.routeQueue.Add(policy)
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

	c.routeQueue.Add(newObj)
}

func (c *ExternalGatewayMasterController) onPolicyDelete(obj interface{}) {
	policy, ok := obj.(*adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("couldn't get object from tomstone %#v", obj))
			return
		}
		policy, ok = tombstone.Obj.(*adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("tombstone contained object that is not an Admin Policy Based External Route %#v", tombstone.Obj))
			return
		}
	}
	if policy != nil {
		c.routeQueue.Add(policy)
	}
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

	err := c.syncNamespace(obj.(*v1.Namespace))
	if err != nil {
		if c.namespaceQueue.NumRequeues(obj) < maxRetries {
			klog.V(2).InfoS("Error found while processing namespace %s:%w", obj.(*v1.Namespace), err)
			c.namespaceQueue.AddRateLimited(obj)
			return true
		}
		klog.Warningf("Dropping namespace %q out of the queue: %v", obj.(*v1.Namespace).Name, err)
		utilruntime.HandleError(err)
	}
	c.namespaceQueue.Forget(obj)
	return true
}

func (c *ExternalGatewayMasterController) syncNamespace(namespace *v1.Namespace) error {
	_, err := c.namespaceLister.Get(namespace.Name)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if apierrors.IsNotFound(err) || !namespace.DeletionTimestamp.IsZero() {
		// DELETE use case
		klog.Infof("Deleting namespace reference %s", namespace.Name)
		return c.mgr.processDeleteNamespace(namespace.Name)
	}
	matches, err := c.mgr.getPoliciesForNamespace(namespace.Name)
	if err != nil {
		return err
	}
	cacheInfo, found := c.mgr.getNamespaceInfoFromCache(namespace.Name)
	if !found && len(matches) == 0 {
		// it's not a namespace being cached already and it is not a target for policies, nothing to do
		return nil
	}

	defer c.mgr.unlockNamespaceInfoCache(namespace.Name)
	if found && cacheInfo.markForDelete {
		// namespace exists and has been marked for deletion, this means there should be an event to complete deleting the namespace.
		// wait for the namespace to be deleted before recreating it in the cache.
		return fmt.Errorf("cannot add namespace %s because it is currently being deleted", namespace.Name)
	}

	if !found {
		// ADD use case
		// new namespace or namespace updated its labels and now match a routing policy
		cacheInfo = c.mgr.newNamespaceInfoInCache(namespace.Name)
		cacheInfo.Policies = matches
		return c.mgr.processAddNamespace(namespace, cacheInfo)
	}

	if !cacheInfo.Policies.Equal(matches) {
		// UPDATE use case
		// policies differ, need to reconcile them
		err = c.mgr.processUpdateNamespace(namespace.Name, cacheInfo.Policies, matches, cacheInfo)
		if err != nil {
			return err
		}
		return nil
	}
	return nil
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
	err := c.syncPod(p)
	if err != nil {
		if c.podQueue.NumRequeues(obj) < maxRetries {
			klog.V(2).InfoS("Error found while processing pod %s/%s:%w", p.Namespace, p.Name, err)
			c.podQueue.AddRateLimited(obj)
			return true
		}
		klog.Warningf("Dropping pod %s/%s out of the queue: %s", p.Namespace, p.Name, err)
		utilruntime.HandleError(err)
	}

	c.podQueue.Forget(obj)
	return true
}

func (c *ExternalGatewayMasterController) syncPod(pod *v1.Pod) error {

	_, err := c.podLister.Pods(pod.Namespace).Get(pod.Name)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	namespaces := c.mgr.filterNamespacesUsingPodGateway(ktypes.NamespacedName{Namespace: pod.Namespace, Name: pod.Name})
	klog.Infof("Processing pod %s/%s", pod.Namespace, pod.Name)
	if apierrors.IsNotFound(err) || !pod.DeletionTimestamp.IsZero() {
		// DELETE case
		if namespaces.Len() == 0 {
			// nothing to do, this pod is not a gateway pod
			return nil
		}
		klog.Infof("Deleting pod gateway %s/%s", pod.Namespace, pod.Name)
		return c.mgr.processDeletePod(pod, namespaces)
	}
	if namespaces.Len() == 0 {
		// ADD case: new pod or existing pod that is not a gateway pod and could now be one.
		klog.Infof("Adding pod %s/%s", pod.Namespace, pod.Name)
		return c.mgr.processAddPod(pod)
	}
	// UPDATE case
	klog.Infof("Updating pod gateway %s/%s", pod.Namespace, pod.Name)
	return c.mgr.processUpdatePod(pod, namespaces)
}

func (c *ExternalGatewayMasterController) updateStatusAPBExternalRoute(routeName string, processedPolicy *routePolicy, processedError error) error {

	routePolicy, err := c.apbRoutePolicyClient.K8sV1().AdminPolicyBasedExternalRoutes().Get(context.TODO(), routeName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return err
	}

	gwIPs := sets.New[string]()
	if processedError == nil {
		for _, static := range processedPolicy.staticGateways {
			gwIPs = gwIPs.Insert(static.Gateways.UnsortedList()...)
		}
		for _, dynamic := range processedPolicy.dynamicGateways {
			gwIPs = gwIPs.Insert(dynamic.Gateways.UnsortedList()...)
		}
	}
	updateStatus(routePolicy, strings.Join(sets.List(gwIPs), ","), processedError)
	_, err = c.apbRoutePolicyClient.K8sV1().AdminPolicyBasedExternalRoutes().UpdateStatus(context.TODO(), routePolicy, metav1.UpdateOptions{})
	if !apierrors.IsNotFound(err) {
		return err
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
		route.Status.Messages = append(route.Status.Messages, "Failed to apply policy:%w", err.Error())
		return
	}
	route.Status.LastTransitionTime = metav1.Time{Time: time.Now()}
	route.Status.Status = adminpolicybasedrouteapi.SuccessStatus
	route.Status.Messages = append(route.Status.Messages, fmt.Sprintf("Configured external gateway IPs: %s", gwIPs))
	klog.Infof("Updating Admin Policy Based External Route %s with Status: %s, Message: %s", route.Name, route.Status.Status, route.Status.Messages[len(route.Status.Messages)-1])
}
