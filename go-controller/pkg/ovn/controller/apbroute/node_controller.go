package apbroute

import (
	"fmt"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ktypes "k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	coreinformers "k8s.io/client-go/informers/core/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	adminpolicybasedrouteapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1"
	adminpolicybasedrouteclient "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1/apis/clientset/versioned"
	adminpolicybasedrouteinformer "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1/apis/informers/externalversions"

	adminpolicybasedroutelisters "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1/apis/listers/adminpolicybasedroute/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
)

// Admin Policy Based Route Node controller

type ExternalGatewayNodeController struct {
	stopCh <-chan struct{}

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

	//external gateway caches
	//make them public so that they can be used by the annotation logic to lock on namespaces and share the same external route information
	ExternalGWCache map[ktypes.NamespacedName]*ExternalRouteInfo
	ExGWCacheMutex  *sync.RWMutex

	routePolicyInformer adminpolicybasedrouteinformer.SharedInformerFactory

	mgr *externalPolicyManager
}

func NewExternalNodeController(
	apbRoutePolicyClient adminpolicybasedrouteclient.Interface,
	podInformer coreinformers.PodInformer,
	namespaceInformer coreinformers.NamespaceInformer,
	stopCh <-chan struct{},
) (*ExternalGatewayNodeController, error) {

	namespaceLister := namespaceInformer.Lister()
	routePolicyInformer := adminpolicybasedrouteinformer.NewSharedInformerFactory(apbRoutePolicyClient, resyncInterval)
	externalRouteInformer := routePolicyInformer.K8s().V1().AdminPolicyBasedExternalRoutes()

	c := &ExternalGatewayNodeController{
		stopCh:              stopCh,
		routePolicyInformer: routePolicyInformer,
		routeLister:         routePolicyInformer.K8s().V1().AdminPolicyBasedExternalRoutes().Lister(),
		routeSynced:         routePolicyInformer.K8s().V1().AdminPolicyBasedExternalRoutes().Informer().HasSynced,
		routeQueue: workqueue.NewNamedRateLimitingQueue(
			workqueue.NewItemFastSlowRateLimiter(1*time.Second, 5*time.Second, 5),
			"apbexternalroutes",
		),
		podLister: podInformer.Lister(),
		podSynced: podInformer.Informer().HasSynced,
		podQueue: workqueue.NewNamedRateLimitingQueue(
			workqueue.NewItemFastSlowRateLimiter(1*time.Second, 5*time.Second, 5),
			"apbexternalroutepods",
		),
		namespaceLister: namespaceLister,
		namespaceSynced: namespaceInformer.Informer().HasSynced,
		namespaceQueue: workqueue.NewNamedRateLimitingQueue(
			workqueue.NewItemFastSlowRateLimiter(1*time.Second, 5*time.Second, 5),
			"apbexternalroutenamespaces",
		),
		mgr: newExternalPolicyManager(
			stopCh,
			podInformer.Lister(),
			namespaceInformer.Lister(),
			routePolicyInformer.K8s().V1().AdminPolicyBasedExternalRoutes().Lister(),
			&conntrackClient{podLister: podInformer.Lister()}),
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

func (c *ExternalGatewayNodeController) Run(threadiness int) {
	defer utilruntime.HandleCrash()
	klog.Infof("Starting Admin Policy Based Route Node Controller")

	c.routePolicyInformer.Start(c.stopCh)

	if !cache.WaitForNamedCacheSync("apbexternalroutenamespaces", c.stopCh, c.namespaceSynced) {
		utilruntime.HandleError(fmt.Errorf("timed out waiting for caches to sync"))
		klog.Infof("Synchronization failed")
		return
	}

	if !cache.WaitForNamedCacheSync("apbexternalroutepods", c.stopCh, c.podSynced) {
		utilruntime.HandleError(fmt.Errorf("timed out waiting for caches to sync"))
		klog.Infof("Synchronization failed")
		return
	}

	if !cache.WaitForNamedCacheSync("adminpolicybasedexternalroutes", c.stopCh, c.routeSynced) {
		utilruntime.HandleError(fmt.Errorf("timed out waiting for caches to sync"))
		klog.Infof("Synchronization failed")
		return
	}

	wg := &sync.WaitGroup{}
	for i := 0; i < threadiness; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wait.Until(func() {
				// processes route policies
				c.runPolicyWorker(wg)
			}, time.Second, c.stopCh)
		}()
	}

	for i := 0; i < threadiness; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wait.Until(func() {
				// detects gateway pod changes and updates the pod's IP and MAC in the northbound DB
				c.runPodWorker(wg)
			}, time.Second, c.stopCh)
		}()
	}

	for i := 0; i < threadiness; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wait.Until(func() {
				// detects namespace changes and applies polices that match the namespace selector in the `From` policy field
				c.runNamespaceWorker(wg)
			}, time.Second, c.stopCh)
		}()
	}

	// wait until we're told to stop
	<-c.stopCh

	c.podQueue.ShutDown()
	c.routeQueue.ShutDown()
	c.namespaceQueue.ShutDown()

	wg.Wait()

}

func (c *ExternalGatewayNodeController) onNamespaceAdd(obj interface{}) {
	c.namespaceQueue.Add(obj)
}

func (c *ExternalGatewayNodeController) onNamespaceUpdate(oldObj, newObj interface{}) {
	oldNamespace := oldObj.(*v1.Namespace)
	newNamespace := newObj.(*v1.Namespace)

	if oldNamespace.ResourceVersion == newNamespace.ResourceVersion || !newNamespace.GetDeletionTimestamp().IsZero() {
		return
	}
	c.namespaceQueue.Add(newObj)
}

func (c *ExternalGatewayNodeController) onNamespaceDelete(obj interface{}) {
	c.namespaceQueue.Add(obj)
}

func (c *ExternalGatewayNodeController) runPolicyWorker(wg *sync.WaitGroup) {
	for c.processNextPolicyWorkItem(wg) {
	}
}

func (c *ExternalGatewayNodeController) processNextPolicyWorkItem(wg *sync.WaitGroup) bool {
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
		klog.Warningf("Failed to process Admin Policy Based External Route %s: %v", item.Name, err.Error())
		return true
	}
	c.routeQueue.Forget(obj)
	return true
}

func (c *ExternalGatewayNodeController) syncRoutePolicy(routePolicy *adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute) error {
	_, err := c.routeLister.Get(routePolicy.Name)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if apierrors.IsNotFound(err) {
		// DELETE use case
		klog.Infof("Deleting policy %s", routePolicy.Name)
		err := c.mgr.processDeletePolicy(routePolicy)
		if err != nil {
			return fmt.Errorf("failed to delete Admin Policy Based External Route %s:%w", routePolicy.Name, err)
		}
		klog.Infof("Policy %s deleted", routePolicy.Name)
		return nil
	}
	currentPolicy, found := c.mgr.getRoutePolicyFromCache(routePolicy.Name)
	if !found {
		// ADD use case
		klog.Infof("Adding policy %s", routePolicy.Name)
		_, err := c.mgr.processAddPolicy(routePolicy)
		if err != nil {
			return fmt.Errorf("failed to create Admin Policy Based External Route %s:%w", routePolicy.Name, err)
		}
		return nil
	}
	// UPDATE use case
	klog.Infof("Updating policy %s", routePolicy.Name)
	_, err = c.mgr.processUpdatePolicy(currentPolicy, routePolicy)
	if err != nil {
		return fmt.Errorf("failed to update Admin Policy Based External Route %s:%w", routePolicy.Name, err)
	}
	return nil
}

func (c *ExternalGatewayNodeController) onPolicyAdd(obj interface{}) {
	c.routeQueue.Add(obj)
}

func (c *ExternalGatewayNodeController) onPolicyUpdate(oldObj, newObj interface{}) {
	oldRoutePolicy := oldObj.(*adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute)
	newRoutePolicy := newObj.(*adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute)

	if oldRoutePolicy.Generation == newRoutePolicy.Generation ||
		!newRoutePolicy.GetDeletionTimestamp().IsZero() {
		return
	}

	c.routeQueue.Add(newObj)
}

func (c *ExternalGatewayNodeController) onPolicyDelete(obj interface{}) {
	c.routeQueue.Add(obj)
}

func (c *ExternalGatewayNodeController) runNamespaceWorker(wg *sync.WaitGroup) {
	for c.processNextNamespaceWorkItem(wg) {

	}
}

func (m *ExternalGatewayNodeController) processNextNamespaceWorkItem(wg *sync.WaitGroup) bool {
	wg.Add(1)
	defer wg.Done()

	obj, shutdown := m.namespaceQueue.Get()

	if shutdown {
		return false
	}

	defer m.namespaceQueue.Done(obj)

	err := m.syncNamespace(obj.(*v1.Namespace))
	if err == nil {
		m.namespaceQueue.Forget(obj)
		return true
	}
	klog.Infof("Error found while processing namespace %s:%w", obj.(*v1.Namespace), err)
	return true
}

func (c *ExternalGatewayNodeController) syncNamespace(namespace *v1.Namespace) error {
	_, err := c.namespaceLister.Get(namespace.Name)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if apierrors.IsNotFound(err) || !namespace.DeletionTimestamp.IsZero() {
		// DELETE use case

		klog.Infof("Deleting namespace reference %s", namespace.Name)
		_, found := c.mgr.getNamespaceInfoFromCache(namespace.Name)
		if !found {
			// namespace is not a recipient for policies
			return nil
		}
		c.mgr.deleteNamespaceInfoInCache(namespace.Name)
		c.mgr.unlockNamespaceInfoCache(namespace.Name)
		return nil
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
	if !found {
		// ADD use case
		// new namespace or namespace updated its labels and now match a routing policy
		defer c.mgr.unlockNamespaceInfoCache(namespace.Name)
		cacheInfo = c.mgr.newNamespaceInfoInCache(namespace.Name)
		cacheInfo.policies = matches
		return c.mgr.processAddNamespace(namespace, cacheInfo)
	}

	if !cacheInfo.policies.Equal(matches) {
		// UPDATE use case
		// policies differ, need to reconcile them
		defer c.mgr.unlockNamespaceInfoCache(namespace.Name)
		err = c.mgr.processUpdateNamespace(namespace.Name, cacheInfo.policies, matches, cacheInfo)
		if err != nil {
			return err
		}
		if cacheInfo.policies.Len() == 0 {
			c.mgr.deleteNamespaceInfoInCache(namespace.Name)
		}
		return nil
	}
	c.mgr.unlockNamespaceInfoCache(namespace.Name)
	return nil

}

func (c *ExternalGatewayNodeController) onPodAdd(obj interface{}) {
	c.podQueue.Add(obj)
}

func (c *ExternalGatewayNodeController) onPodUpdate(oldObj, newObj interface{}) {
	c.podQueue.Add(newObj)
}

func (c *ExternalGatewayNodeController) onPodDelete(obj interface{}) {
	c.podQueue.Add(obj)
}

func (c *ExternalGatewayNodeController) runPodWorker(wg *sync.WaitGroup) {
	for c.processNextPodWorkItem(wg) {
	}
}

func (c *ExternalGatewayNodeController) processNextPodWorkItem(wg *sync.WaitGroup) bool {
	wg.Add(1)
	defer wg.Done()

	obj, shutdown := c.podQueue.Get()

	if shutdown {
		return false
	}

	defer c.podQueue.Done(obj)

	err := c.syncPod(obj.(*v1.Pod))
	if err == nil {
		c.podQueue.Forget(obj)
		return true
	}
	klog.Infof("Requeuing pod %s/%s:%w", obj.(*v1.Pod).GetNamespace(), obj.(*v1.Pod).GetName(), err.Error())
	return true
}

func (c *ExternalGatewayNodeController) syncPod(pod *v1.Pod) error {

	_, err := c.podLister.Pods(pod.Namespace).Get(pod.Name)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	namespaces := c.mgr.filterNamespacesUsingPodGateway(ktypes.NamespacedName{Namespace: pod.Namespace, Name: pod.Name})
	klog.Infof("Processing pod reference %s/%s", pod.Namespace, pod.Name)
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
		klog.Infof("Adding pod reference %s/%s", pod.Namespace, pod.Name)
		return c.mgr.processAddPod(pod)
	}
	// UPDATE case
	klog.Infof("Updating pod gateway %s/%s", pod.Namespace, pod.Name)
	return c.mgr.processUpdatePod(pod, namespaces)
}

func (c *ExternalGatewayNodeController) GetAdminPolicyBasedExternalRouteIPsForTargetNamespace(namespaceName string) (sets.Set[string], error) {
	gwIPs, err := c.mgr.getDynamicGatewayIPsForTargetNamespace(namespaceName)
	if err != nil {
		return nil, err
	}
	tmpIPs, err := c.mgr.getStaticGatewayIPsForTargetNamespace(namespaceName)
	if err != nil {
		return nil, err
	}

	return gwIPs.Union(tmpIPs), nil
}
