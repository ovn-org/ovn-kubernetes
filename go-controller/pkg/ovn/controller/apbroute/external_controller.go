package apbroute

import (
	"encoding/json"
	"fmt"
	"net"
	"reflect"
	"strings"
	"sync"
	"time"

	nettypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/libovsdb/ovsdb"
	"github.com/pkg/errors"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
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
	utilnet "k8s.io/utils/net"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	adminpolicybasedrouteapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1"
	adminpolicybasedrouteinformer "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1/apis/informers/externalversions/adminpolicybasedroute/v1"
	adminpolicybasedroutelisters "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1/apis/listers/adminpolicybasedroute/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
)

const (
	controllerName = "ovn-external-route-controller"
)

type gatewayInfoList []*gatewayInfo

func (g gatewayInfoList) Has(item *gatewayInfo) bool {
	for _, i := range g {
		if i.gws.Difference(item.gws).Len() == 0 {
			return true
		}
	}
	return false
}

func (g gatewayInfoList) HasIP(ip string) bool {
	for _, i := range g {
		if i.gws.Has(ip) {
			return true
		}
	}
	return false
}

func (g gatewayInfoList) Delete(item *gatewayInfo) {
	for index, i := range g {
		if i.gws.Equal(item.gws) {
			g = append(g[:index], g[index+1:]...)
		}
	}
}

func (g gatewayInfoList) Len() int {
	return len(g)
}

type gatewayInfo struct {
	gws        sets.String
	bfdEnabled bool
}

type externalRouteInfo struct {
	sync.Mutex
	deleted bool
	podName ktypes.NamespacedName
	// podExternalRoutes is a cache keeping the LR routes added to the GRs when
	// external gateways are used. The first map key is the podIP (src-ip of the route),
	// the second the GW IP (next hop), and the third the GR name
	podExternalRoutes map[string]map[string]string
}

// Admin Policy Based Route services

type ExternalController struct {
	client kubernetes.Interface
	kube   kube.InterfaceOVN
	stopCh <-chan struct{}
	sync.Mutex

	nodeLister corev1listers.NodeLister

	// route policies
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

	// cache for set of policies impacting a given namespace
	namespaceInfoCache map[string]*namespaceInfo
	// cache for namespaces where pod gateways reside.
	// it contains type.NamespacedName(namespace,podName)->set of namespaces where the pod IP is used as a gateway
	// Used when veryfing updates to pods.
	gatewayPodsNamespaceCache map[ktypes.NamespacedName]sets.String

	// NorthBound client
	nbClient libovsdbclient.Client

	//external gateway caches
	externalGWCache map[ktypes.NamespacedName]*externalRouteInfo
	exGWCacheMutex  sync.RWMutex

	// An address set factory that creates address sets
	addressSetFactory addressset.AddressSetFactory
	controllerName    string
}

type namespaceInfo struct {
	policies        sets.String
	staticGateways  sets.String
	dynamicGateways map[ktypes.NamespacedName]*gatewayInfo
}

func NewExternalController(
	client kubernetes.Interface,
	kube kube.InterfaceOVN,
	stopCh <-chan struct{},
	externalRouteInformer adminpolicybasedrouteinformer.AdminPolicyBasedExternalRouteInformer,
	podInformer coreinformers.PodInformer,
	namespaceInformer coreinformers.NamespaceInformer,
	nodeLister corev1listers.NodeLister,
	nbClient libovsdbclient.Client,
	addressSetFactory addressset.AddressSetFactory,
) *ExternalController {

	c := &ExternalController{
		client:      client,
		kube:        kube,
		stopCh:      stopCh,
		routeLister: externalRouteInformer.Lister(),
		routeSynced: externalRouteInformer.Informer().HasSynced,
		routeQueue: workqueue.NewNamedRateLimitingQueue(
			workqueue.NewItemFastSlowRateLimiter(1*time.Second, 5*time.Second, 5),
			"adminpolicybasedexternalroutes",
		),
		podLister: podInformer.Lister(),
		podSynced: podInformer.Informer().HasSynced,
		podQueue: workqueue.NewNamedRateLimitingQueue(
			workqueue.NewItemFastSlowRateLimiter(1*time.Second, 5*time.Second, 5),
			"apbexternalroutepods",
		),
		namespaceLister: namespaceInformer.Lister(),
		namespaceSynced: podInformer.Informer().HasSynced,
		namespaceQueue: workqueue.NewNamedRateLimitingQueue(
			workqueue.NewItemFastSlowRateLimiter(1*time.Second, 5*time.Second, 5),
			"apbexternalroutenamespaces",
		),
		nodeLister:        nodeLister,
		nbClient:          nbClient,
		addressSetFactory: addressSetFactory,
	}

	externalRouteInformer.Informer().AddEventHandler(
		factory.WithUpdateHandlingForObjReplace(cache.ResourceEventHandlerFuncs{
			AddFunc:    c.onRouteAdd,
			UpdateFunc: c.onRouteUpdate,
			DeleteFunc: c.onRouteDelete,
		}))

	podInformer.Informer().AddEventHandler(
		factory.WithUpdateHandlingForObjReplace(cache.ResourceEventHandlerFuncs{
			AddFunc:    c.onPodAdd,
			UpdateFunc: c.onPodUpdate,
			DeleteFunc: c.onPodDelete,
		}))

	namespaceInformer.Informer().AddEventHandler(
		factory.WithUpdateHandlingForObjReplace(cache.ResourceEventHandlerFuncs{
			AddFunc:    c.onNamespaceAdd,
			UpdateFunc: c.onNamespaceUpdate,
			DeleteFunc: c.onNamespaceDelete,
		}))

	return c

}

type opType int

const (
	addOp = iota
	updateOp
	deleteOp
)

type handlerObj struct {
	oldObj interface{}
	newObj interface{}
	op     opType
}

func (c *ExternalController) Run(threadiness int) {
	defer utilruntime.HandleCrash()
	klog.Infof("Starting Admin Policy Based Route Controller")

	if !cache.WaitForNamedCacheSync("adminpolicybasedexternalroutes", c.stopCh, c.routeSynced) {
		utilruntime.HandleError(fmt.Errorf("timed out waiting for caches to sync"))
		klog.Infof("Synchronization failed")
		return
	}

	if !cache.WaitForNamedCacheSync("apbexternalroutepods", c.stopCh, c.podSynced) {
		utilruntime.HandleError(fmt.Errorf("timed out waiting for caches to sync"))
		klog.Infof("Synchronization failed")
		return
	}

	if !cache.WaitForNamedCacheSync("apbexternalroutenamespaces", c.stopCh, c.namespaceSynced) {
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

func (c *ExternalController) repair() error {

	return nil
}

func (c *ExternalController) runPolicyWorker(wg *sync.WaitGroup) {
	for c.processNextPolicyWorkItem(wg) {

	}
}

func (c *ExternalController) processNextPolicyWorkItem(wg *sync.WaitGroup) bool {
	wg.Add(1)
	defer wg.Done()

	key, shutdown := c.routeQueue.Get()

	if shutdown {
		return true
	}

	defer c.routeQueue.Done(key)

	item := key.(handlerObj)
	var err error
	switch item.op {
	case addOp:
		err = c.processAddRoute(item.newObj.(*adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute))
	case updateOp:
		oldObj := item.oldObj.(*adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute)
		newObj := item.newObj.(*adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute)
		err = c.processUpdateRoute(oldObj, newObj)
	case deleteOp:
		err = c.processDeleteRoute(item.newObj.(*adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute))
	}
	if err == nil {
		c.routeQueue.Forget(key)
	}
	return false
}

func (c *ExternalController) onRouteAdd(obj interface{}) {
	c.routeQueue.Add(
		handlerObj{
			newObj: obj,
			op:     addOp,
		})
}

func (c *ExternalController) onRouteUpdate(oldObj, newObj interface{}) {
	oldRoutePolicy := oldObj.(*adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute)
	newRoutePolicy := newObj.(*adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute)

	if oldRoutePolicy.Generation == newRoutePolicy.Generation ||
		!newRoutePolicy.GetDeletionTimestamp().IsZero() {
		return
	}

	c.routeQueue.Add(handlerObj{
		oldObj: oldObj,
		newObj: newObj,
		op:     updateOp,
	})
}

func (c *ExternalController) onRouteDelete(obj interface{}) {

	c.routeQueue.Add(
		handlerObj{
			op:     deleteOp,
			newObj: obj,
		})
}

func (c *ExternalController) processAddRoute(routePolicy *adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute) error {
	// it's a new policy
	var errors []error
	for _, policy := range routePolicy.Spec.Policies {
		err := c.applyPolicy(policy)
		if err != nil {
			errors = append(errors, err)
		}
	}
	return kerrors.NewAggregate(errors)
}

func (c *ExternalController) applyPolicy(policy *adminpolicybasedrouteapi.ExternalPolicy) error {
	var errors []error
	targetNamespaces, err := c.listNamespacesBySelector(&policy.From.NamespaceSelector)
	if err != nil {
		errors = append(errors, err)
	}
	err = c.addStaticHops(policy.NextHops.StaticHops, targetNamespaces)
	if err != nil {
		errors = append(errors, err)
	}
	err = c.addDynamicHops(policy.NextHops.DynamicHops, targetNamespaces)
	if err != nil {
		errors = append(errors, err)
	}
	return kerrors.NewAggregate(errors)
}

func (c *ExternalController) processRoutePolicies(routePolicies []*routePolicy) (map[string]*gatewayInfo, error) {

	nsGwInfo := map[string]*gatewayInfo{}
	for _, pp := range routePolicies {
		targetNs, err := c.namespaceLister.List(*pp.labelSelector)
		if err != nil {
			return nil, err
		}
		for _, ns := range targetNs {
			for _, info := range pp.staticGateways {
				c.addGWRoutesForNamespace(ns.Name, gatewayInfoList{info})
			}
			for pod, info := range pp.dynamicGateways {
				c.addPodExternalGWForNamespace(ns.Name, pod.Name, info)
				nsGwInfo[ns.Name] = info
			}
		}
	}
	return nsGwInfo, nil
}

func (c *ExternalController) GetNamespacesBySelector(selector *metav1.LabelSelector) ([]*v1.Namespace, error) {

	l, err := metav1.LabelSelectorAsSelector(selector)
	if err != nil {
		return nil, err
	}
	return c.namespaceLister.List(l)
}

func (c *ExternalController) processUpdateRoute(old, new *adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute) error {
	klog.Infof("Processing update for Admin Policy Based External Route resource %s", new.Name)
	var errors []error

	//To update the policies, first we'll process the diff between old and new and remove the old policies that are not found in the new object.
	//Afterwards, we'll process the diff between the new and the old and apply the diff policies, ensuring that we are no reduplicating the gatewayInfo.
	//The last step is to update the Status information in the new CR to reflect the status of the actions performed
	for _, oldPolicy := range old.Spec.Policies {
		oldPolicyNamespaces, err := c.GetNamespacesBySelector(&oldPolicy.From.NamespaceSelector)
		errors = append(errors, err)
		// something changed, we need to identify which of the policy fields has changed.
		if !containsFrom(new.Spec.Policies, &oldPolicy.From) {
			// Old namespace selector for policy does no longer exist, delete the old gateway information from the old policy
			err := c.deletePolicy(oldPolicy)
			if err != nil {
				errors = append(errors, err)
			}
			// We have removed all static and dynamic hops, no need to continue evaluating this old policy
			continue
		}
		if diff := getDiffStaticHops(oldPolicy.NextHops.StaticHops, new.Spec.Policies); len(diff) > 0 {
			// There are differences between the static hops, we proceed to remove the old static hops that are not in the new object
			errors = append(errors, c.deleteStaticHops(diff, oldPolicyNamespaces))
		}
		if diff := getDiffDynamicHops(oldPolicy.NextHops.DynamicHops, new.Spec.Policies); len(diff) > 0 {
			// there are differences between in the dynamic hops, so we proceed to remove those that are not in the new object
			errors = append(errors, c.deleteDynamicHops(diff, oldPolicyNamespaces))
		}
	}

	// add the diff from new to old.
	for _, p := range new.Spec.Policies {
		targetNamespaces, err := c.GetNamespacesBySelector(&p.From.NamespaceSelector)
		errors = append(errors, err)
		// something changed, we need to identify which of the policy fields has changed.
		if !containsFrom(old.Spec.Policies, &p.From) {
			err = c.applyPolicy(p)
			if err != nil {
				errors = append(errors, err)
			}
			// We have applied the new policy, no need to continue evaluating this policy
			continue
		}
		if diff := getDiffStaticHops(p.NextHops.StaticHops, old.Spec.Policies); len(diff) > 0 {
			// There are differences between the static hops, we proceed to add the new static hops that are not in the old object
			errors = append(errors, c.addStaticHops(diff, targetNamespaces))
		}
		if diff := getDiffDynamicHops(p.NextHops.DynamicHops, new.Spec.Policies); len(diff) > 0 {
			// there are differences in the dynamic hops, so we proceed to add those that are not in the old object
			errors = append(errors, c.addDynamicHops(diff, targetNamespaces))
		}
	}

	// proceed to add the new policy to ensure the new policies are applied.
	updateStatus(new, errors)
	err := c.kube.UpdateStatusAPBExternalRoute(new)
	return kerrors.NewAggregate(append(errors, err))
}

func (c *ExternalController) parseStaticGatewayIPs(hops []*adminpolicybasedrouteapi.StaticHop) (gatewayInfoList, error) {
	gwList := gatewayInfoList{}

	// collect all the static gateway information from the nextHops slice
	for _, h := range hops {
		ip := net.ParseIP(h.IP)
		if ip == nil {
			return nil, fmt.Errorf("could not parse routing external gw annotation value '%s'", h.IP)
		}
		gwList = append(gwList, &gatewayInfo{gws: sets.NewString(ip.String()), bfdEnabled: h.BFDEnabled})
	}
	return gwList, nil
}

func (c *ExternalController) addStaticHops(hops []*adminpolicybasedrouteapi.StaticHop, targetNamespaces []*v1.Namespace) error {

	gwList, err := c.parseStaticGatewayIPs(hops)
	if err != nil {
		return err
	}
	// update each of the target's namespace's routing information
	for _, ns := range targetNamespaces {
		err = c.addGWRoutesForNamespace(ns.Name, gwList)
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *ExternalController) addDynamicHops(hops []*adminpolicybasedrouteapi.DynamicHop, targetNamespaces []*v1.Namespace) error {
	podsInfo, err := c.getDynamicRoutePods(hops)
	if err != nil {
		return err
	}
	// update each of the target's namespace's routing information
	for _, ns := range targetNamespaces {

		for podNamespaceName, gwInfo := range podsInfo {
			err := c.addPodExternalGWForNamespace(ns.Name, podNamespaceName.Name, gwInfo)
			if err != nil {
				return err
			}
		}

	}
	return nil
}

func updateStatus(route *adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute, errors []error) {
	if len(errors) > 0 {
		route.Status.Status = adminpolicybasedrouteapi.FailStatus
		route.Status.Messages = append(route.Status.Messages, "Failed to apply policy %s:%w", route.Name, kerrors.NewAggregate(errors).Error())
		return
	}
	route.Status.Status = adminpolicybasedrouteapi.SuccessStatus
	route.Status.Messages = append(route.Status.Messages, "Successfully applied all policies")
}

func containsFrom(slice []*adminpolicybasedrouteapi.ExternalPolicy, from *adminpolicybasedrouteapi.ExternalNetworkSource) bool {
	for _, p := range slice {
		if reflect.DeepEqual(from, p.From) {
			return true
		}
	}
	return false
}

func getDiffStaticHops(oldHops []*adminpolicybasedrouteapi.StaticHop, newHops []*adminpolicybasedrouteapi.ExternalPolicy) []*adminpolicybasedrouteapi.StaticHop {
	diff := []*adminpolicybasedrouteapi.StaticHop{}
	for _, old := range oldHops {
		for _, policies := range newHops {
			for _, new := range policies.NextHops.StaticHops {
				if reflect.DeepEqual(old, new) {
					break
				}
			}
			diff = append(diff, old)
		}
	}
	return diff
}

func getDiffDynamicHops(oldHops []*adminpolicybasedrouteapi.DynamicHop, newHops []*adminpolicybasedrouteapi.ExternalPolicy) []*adminpolicybasedrouteapi.DynamicHop {
	diff := []*adminpolicybasedrouteapi.DynamicHop{}
	for _, old := range oldHops {
		for _, policies := range newHops {
			for _, new := range policies.NextHops.DynamicHops {
				if reflect.DeepEqual(old, new) {
					break
				}
			}
			diff = append(diff, old)
		}
	}
	return diff
}

func (c *ExternalController) processDeleteRoute(policy *adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute) error {

	var errors []error
	for _, p := range policy.Spec.Policies {
		err := c.deletePolicy(p)
		if err != nil {
			errors = append(errors, err)
		}
	}
	return kerrors.NewAggregate(errors)
}

func (c *ExternalController) deletePolicy(policy *adminpolicybasedrouteapi.ExternalPolicy) error {
	var errors []error
	targetNamespaces, err := c.listNamespacesBySelector(&policy.From.NamespaceSelector)
	if err != nil {
		errors = append(errors, err)
	}
	err = c.deleteStaticHops(policy.NextHops.StaticHops, targetNamespaces)
	if err != nil {
		errors = append(errors, err)
	}
	err = c.deleteDynamicHops(policy.NextHops.DynamicHops, targetNamespaces)
	if err != nil {
		errors = append(errors, err)
	}
	return kerrors.NewAggregate(errors)
}

func (c *ExternalController) deleteStaticHops(hops []*adminpolicybasedrouteapi.StaticHop, targetNamespaces []*v1.Namespace) error {
	gwList, err := c.parseStaticGatewayIPs(hops)
	if err != nil {
		return err
	}
	// update each of the target's namespace's routing information
	for _, ns := range targetNamespaces {
		for _, gw := range gwList {
			err = c.deleteGWRoutesForNamespace(ns.Name, gw.gws)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *ExternalController) deleteDynamicHops(hops []*adminpolicybasedrouteapi.DynamicHop, targetNamespaces []*v1.Namespace) error {
	podsInfo, err := c.getDynamicRoutePods(hops)
	if err != nil {
		return err
	}
	// update each of the target's namespace's routing information
	for _, ns := range targetNamespaces {
		for _, gwInfo := range podsInfo {
			err = c.deleteGWRoutesForNamespace(ns.Name, gwInfo.gws)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

type routePolicy struct {
	labelSelector   *labels.Selector
	staticGateways  gatewayInfoList
	dynamicGateways map[ktypes.NamespacedName]*gatewayInfo
}

func (c *ExternalController) processPolicies(route *adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute) ([]*routePolicy, error) {

	klog.Infof("Processing sync for Admin Policy Based External Route resource %s", route.Name)
	var (
		errors   []error
		policies []*routePolicy
	)
	for _, p := range route.Spec.Policies {
		pp, err := c.processPolicy(p)
		if err != nil {
			errors = append(errors, err)
			continue
		}
		policies = append(policies, pp)
	}
	return policies, kerrors.NewAggregate(errors)
}

func (c *ExternalController) processPolicy(policy *adminpolicybasedrouteapi.ExternalPolicy) (*routePolicy, error) {
	var (
		errors        []error
		staticGWInfo  gatewayInfoList
		dynamicGWInfo map[ktypes.NamespacedName]*gatewayInfo
	)
	l, err := metav1.LabelSelectorAsSelector(&policy.From.NamespaceSelector)
	if err != nil {
		errors = append(errors, err)
	}

	staticGWInfo, err = c.parseStaticGatewayIPs(policy.NextHops.StaticHops)
	if err != nil {
		errors = append(errors, err)
	}

	dynamicGWInfo, err = c.getDynamicRoutePods(policy.NextHops.DynamicHops)
	if err != nil {
		errors = append(errors, err)
	}
	return &routePolicy{
		labelSelector:   &l,
		staticGateways:  staticGWInfo,
		dynamicGateways: dynamicGWInfo,
	}, kerrors.NewAggregate(errors)

}

func (c *ExternalController) listNamespacesBySelector(selector *metav1.LabelSelector) ([]*v1.Namespace, error) {
	s, err := metav1.LabelSelectorAsSelector(selector)
	if err != nil {
		return nil, err
	}
	ns, err := c.namespaceLister.List(s)
	if err != nil {
		return nil, err
	}
	return ns, nil

}

func (c *ExternalController) listPodsInNamespaceWithSelector(namespace string, selector *metav1.LabelSelector) ([]*v1.Pod, error) {

	s, err := metav1.LabelSelectorAsSelector(selector)
	if err != nil {
		return nil, err
	}
	return c.podLister.Pods(namespace).List(s)
}

func (c *ExternalController) getDynamicRoutePods(hops []*adminpolicybasedrouteapi.DynamicHop) (map[ktypes.NamespacedName]*gatewayInfo, error) {
	allNamespaces, err := c.namespaceLister.List(labels.Everything())
	if err != nil {
		return nil, err
	}
	podsInfo := map[ktypes.NamespacedName]*gatewayInfo{}
	for _, h := range hops {
		podNS := allNamespaces
		if h.NamespaceSelector != nil {
			podNS, err = c.listNamespacesBySelector(h.NamespaceSelector)
			if err != nil {
				return nil, err
			}
		}
		for _, ns := range podNS {
			s, err := metav1.LabelSelectorAsSelector(&h.PodSelector)
			if err != nil {
				return nil, err
			}
			pods, err := c.podLister.Pods(ns.Name).List(s)
			if err != nil {
				return nil, err
			}
			for _, pod := range pods {
				foundGws, err := getExGwPodIPs(pod, h.NetworkAttachmentName)
				if err != nil {
					return nil, err
				}
				// if we found any gateways then we need to update current pods routing in the relevant namespace
				if len(foundGws) == 0 {
					klog.Warningf("No valid gateway IPs found for requested external gateway pod %s/%s", pod.Namespace, pod.Name)
					continue
				}
				key := ktypes.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}
				if _, ok := podsInfo[key]; ok {
					klog.Warningf("Found overlapping dynamic hop policy for pod %s, discarding match entry", key)
					continue
				}
				podsInfo[key] = &gatewayInfo{gws: foundGws, bfdEnabled: h.BFDEnabled}
			}
		}
	}
	return podsInfo, nil
}

func getExGwPodIPs(gatewayPod *v1.Pod, networkName string) (sets.String, error) {
	if networkName != "" {
		return getMultusIPsFromNetworkName(gatewayPod, networkName)
	}
	if gatewayPod.Spec.HostNetwork {
		return getPodIPs(gatewayPod), nil
	}
	return nil, fmt.Errorf("ignoring pod %s as an external gateway candidate. Invalid combination "+
		"of host network: %t and routing-network annotation: %s", gatewayPod.Name, gatewayPod.Spec.HostNetwork,
		networkName)
}

func getPodIPs(pod *v1.Pod) sets.String {
	foundGws := sets.NewString()
	for _, podIP := range pod.Status.PodIPs {
		ip := utilnet.ParseIPSloppy(podIP.IP)
		if ip != nil {
			foundGws.Insert(ip.String())
		}
	}
	return foundGws
}

func getMultusIPsFromNetworkName(pod *v1.Pod, networkName string) (sets.String, error) {
	foundGws := sets.NewString()
	var multusNetworks []nettypes.NetworkStatus
	err := json.Unmarshal([]byte(pod.ObjectMeta.Annotations[nettypes.NetworkStatusAnnot]), &multusNetworks)
	if err != nil {
		return nil, fmt.Errorf("unable to unmarshall annotation k8s.v1.cni.cncf.io/network-status on pod %s: %v",
			pod.Name, err)
	}
	for _, multusNetwork := range multusNetworks {
		if multusNetwork.Name == networkName {
			for _, gwIP := range multusNetwork.IPs {
				ip := net.ParseIP(gwIP)
				if ip != nil {
					foundGws.Insert(ip.String())
				}
			}
			return foundGws, nil
		}
	}
	return nil, fmt.Errorf("unable to find multus network %s in pod %s/%s", networkName, pod.Namespace, pod.Name)
}

// wrapper function to log if there are duplicate gateway IPs present in the cache
func validateRoutingPodGWs(podGWs map[string]gatewayInfo) error {
	// map to hold IP/podName
	ipTracker := make(map[string]string)
	for podName, gwInfo := range podGWs {
		for _, gwIP := range gwInfo.gws.UnsortedList() {
			if foundPod, ok := ipTracker[gwIP]; ok {
				return fmt.Errorf("duplicate IP found in ECMP Pod route cache! IP: %q, first pod: %q, second "+
					"pod: %q", gwIP, podName, foundPod)
			}
			ipTracker[gwIP] = podName
		}
	}
	return nil
}

// addPodExternalGWForNamespace handles adding routes to all pods in that namespace for a pod GW
func (c *ExternalController) addPodExternalGWForNamespace(targetNamespace, podGWName string, egress *gatewayInfo) error {

	err := c.validateRoutingPodGWs(targetNamespace, podGWName, egress)
	if err != nil {
		return err
	}
	err = c.addGWRoutesForNamespace(targetNamespace, gatewayInfoList{egress})
	if err != nil {
		return err
	}
	return nil

}

func (c *ExternalController) validateRoutingPodGWs(namespace, podGWName string, egress *gatewayInfo) error {
	for name := range c.namespaceInfoCache[namespace].policies {
		policy, err := c.routeLister.Get(name)
		if err != nil {
			return err
		}
		pp, err := c.processPolicies(policy)
		if err != nil {
			return err
		}
		for _, p := range pp {
			for ip := range egress.gws {
				if p.staticGateways.HasIP(ip) {
					return fmt.Errorf("duplicate IP %s for pod %s/%s found in static gateway route policy %s", ip, namespace, podGWName, name)
				}
				for gwPodNamespacedName, info := range p.dynamicGateways {
					if info.gws.Has(ip) {
						return fmt.Errorf("duplicate IP %s for pod %s/%s found in pod gateway %s for route policy %s ", ip, namespace, podGWName, gwPodNamespacedName, name)
					}
				}
			}
		}
	}
	return nil
}

// addGWRoutesForNamespace handles adding routes for all existing pods in namespace
func (c *ExternalController) addGWRoutesForNamespace(namespace string, egress gatewayInfoList) error {
	existingPods, err := c.podLister.Pods(namespace).List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to get all the pods (%v)", err)
	}
	for _, pod := range existingPods {
		err := c.addGWRoutesForPodInNamespace(pod, egress)
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *ExternalController) addGWRoutesForPodInNamespace(pod *v1.Pod, egress gatewayInfoList) error {
	if util.PodCompleted(pod) || !util.PodWantsHostNetwork(pod) {
		return nil
	}
	podIPs := make([]*net.IPNet, 0)
	for _, podIP := range pod.Status.PodIPs {
		podIPStr := utilnet.ParseIPSloppy(podIP.IP).String()
		cidr := podIPStr + util.GetIPFullMask(podIPStr)
		_, ipNet, err := net.ParseCIDR(cidr)
		if err != nil {
			return fmt.Errorf("failed to parse CIDR: %s, error: %v", cidr, err)
		}
		podIPs = append(podIPs, ipNet)
	}
	if len(podIPs) == 0 {
		klog.Warningf("Will not add gateway routes pod %s/%s. IPs not found!", pod.Namespace, pod.Name)
		return nil
	}
	if config.Gateway.DisableSNATMultipleGWs {
		// delete all perPodSNATs (if this pod was controlled by egressIP controller, it will stop working since
		// a pod cannot be used for multiple-external-gateways and egressIPs at the same time)
		if err := c.deletePodSNAT(pod.Spec.NodeName, []*net.IPNet{}, podIPs); err != nil {
			klog.Error(err.Error())
		}
	}
	podNsName := ktypes.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}
	return c.addGWRoutesForPod(egress, podIPs, podNsName, pod.Spec.NodeName)
}

// deletePodSNAT removes per pod SNAT rules towards the nodeIP that are applied to the GR where the pod resides
// if allSNATs flag is set, then all the SNATs (including against egressIPs if any) for that pod will be deleted
// used when disableSNATMultipleGWs=true
func (c *ExternalController) deletePodSNAT(nodeName string, extIPs, podIPNets []*net.IPNet) error {
	nats, err := buildPodSNAT(extIPs, podIPNets)
	if err != nil {
		return err
	}
	logicalRouter := nbdb.LogicalRouter{
		Name: types.GWRouterPrefix + nodeName,
	}
	err = libovsdbops.DeleteNATs(c.nbClient, &logicalRouter, nats...)
	if err != nil {
		return fmt.Errorf("failed to delete SNAT rule for pod on gateway router %s: %v", logicalRouter.Name, err)
	}
	return nil
}

// buildPodSNAT builds per pod SNAT rules towards the nodeIP that are applied to the GR where the pod resides
// if allSNATs flag is set, then all the SNATs (including against egressIPs if any) for that pod will be returned
func buildPodSNAT(extIPs, podIPNets []*net.IPNet) ([]*nbdb.NAT, error) {
	nats := make([]*nbdb.NAT, 0, len(extIPs)*len(podIPNets))
	var nat *nbdb.NAT

	for _, podIPNet := range podIPNets {
		podIP := podIPNet.IP.String()
		mask := util.GetIPFullMask(podIP)
		_, fullMaskPodNet, err := net.ParseCIDR(podIP + mask)
		if err != nil {
			return nil, fmt.Errorf("invalid IP: %s and mask: %s combination, error: %v", podIP, mask, err)
		}
		if len(extIPs) == 0 {
			nat = libovsdbops.BuildSNAT(nil, fullMaskPodNet, "", nil)
		} else {
			for _, gwIPNet := range extIPs {
				gwIP := gwIPNet.IP.String()
				if utilnet.IsIPv6String(gwIP) != utilnet.IsIPv6String(podIP) {
					continue
				}
				nat = libovsdbops.BuildSNAT(&gwIPNet.IP, fullMaskPodNet, "", nil)
			}
		}
		nats = append(nats, nat)
	}
	return nats, nil
}

// addEgressGwRoutesForPod handles adding all routes to gateways for a pod on a specific GR
func (c *ExternalController) addGWRoutesForPod(gateways gatewayInfoList, podIfAddrs []*net.IPNet, podNsName ktypes.NamespacedName, node string) error {
	gr := util.GetGatewayRouterFromNode(node)

	routesAdded := 0
	portPrefix, err := c.extSwitchPrefix(node)
	if err != nil {
		klog.Infof("Failed to find ext switch prefix for %s %v", node, err)
		return err
	}

	port := portPrefix + types.GWRouterToExtSwitchPrefix + gr
	routeInfo, err := c.ensureRouteInfoLocked(podNsName)
	if err != nil {
		return fmt.Errorf("failed to ensure routeInfo for %s, error: %v", podNsName, err)
	}
	defer routeInfo.Unlock()
	for _, podIPNet := range podIfAddrs {
		for _, gateway := range gateways {
			// TODO (trozet): use the go bindings here and batch commands
			// validate the ip and gateway belong to the same address family
			gws, err := util.MatchAllIPStringFamily(utilnet.IsIPv6(podIPNet.IP), gateway.gws.UnsortedList())
			if err != nil {
				klog.Warningf("Address families for the pod address %s and gateway %s did not match", podIPNet.IP.String(), gateway.gws)
				continue
			}
			podIP := podIPNet.IP.String()
			for _, gw := range gws {
				// if route was already programmed, skip it
				if foundGR, ok := routeInfo.podExternalRoutes[podIP][gw]; ok && foundGR == gr {
					routesAdded++
					continue
				}
				mask := util.GetIPFullMask(podIP)

				if err := c.createBFDStaticRoute(gateway.bfdEnabled, gw, podIP, gr, port, mask); err != nil {
					return err
				}
				if routeInfo.podExternalRoutes[podIP] == nil {
					routeInfo.podExternalRoutes[podIP] = make(map[string]string)
				}
				routeInfo.podExternalRoutes[podIP][gw] = gr
				routesAdded++
				if len(routeInfo.podExternalRoutes[podIP]) == 1 {
					if err := c.addHybridRoutePolicyForPod(podIPNet.IP, node); err != nil {
						return err
					}
				}
			}

		}
	}
	// if no routes are added return an error
	if routesAdded < 1 {
		return fmt.Errorf("gateway specified for namespace %s with gateway addresses %v but no valid routes exist for pod: %s",
			podNsName.Namespace, podIfAddrs, podNsName.Name)
	}
	return nil
}

// extSwitchPrefix returns the prefix of the external switch to use for
// external gateway routes. In case no second bridge is configured, we
// use the default one and the prefix is empty.
func (c *ExternalController) extSwitchPrefix(nodeName string) (string, error) {
	node, err := c.nodeLister.Get(nodeName)
	if err != nil {
		return "", errors.Wrapf(err, "extSwitchPrefix: failed to find node %s", nodeName)
	}
	l3GatewayConfig, err := util.ParseNodeL3GatewayAnnotation(node)
	if err != nil {
		return "", errors.Wrapf(err, "extSwitchPrefix: failed to parse l3 gateway annotation for node %s", nodeName)
	}

	if l3GatewayConfig.EgressGWInterfaceID != "" {
		return types.EgressGWSwitchPrefix, nil
	}
	return "", nil
}

// ensureRouteInfoLocked either gets the current routeInfo in the cache with a lock, or creates+locks a new one if missing
func (c *ExternalController) ensureRouteInfoLocked(podName ktypes.NamespacedName) (*externalRouteInfo, error) {
	// We don't want to hold the cache lock while we try to lock the routeInfo (unless we are creating it, then we know
	// no one else is using it). This could lead to dead lock. Therefore the steps here are:
	// 1. Get the cache lock, try to find the routeInfo
	// 2. If routeInfo existed, release the cache lock
	// 3. If routeInfo did not exist, safe to hold the cache lock while we create the new routeInfo
	c.exGWCacheMutex.Lock()
	routeInfo, ok := c.externalGWCache[podName]
	if !ok {
		routeInfo = &externalRouteInfo{
			podExternalRoutes: make(map[string]map[string]string),
			podName:           podName,
		}
		// we are creating routeInfo and going to set it in podExternalRoutes map
		// so safe to hold the lock while we create and add it
		defer c.exGWCacheMutex.Unlock()
		c.externalGWCache[podName] = routeInfo
	} else {
		// if we found an existing routeInfo, do not hold the cache lock
		// while waiting for routeInfo to Lock
		c.exGWCacheMutex.Unlock()
	}

	// 4. Now lock the routeInfo
	routeInfo.Lock()

	// 5. If routeInfo was deleted between releasing the cache lock and grabbing
	// the routeInfo lock, return an error so the caller doesn't use it and
	// retries the operation later
	if routeInfo.deleted {
		routeInfo.Unlock()
		return nil, fmt.Errorf("routeInfo for pod %s, was altered during ensure route info", podName)
	}

	return routeInfo, nil
}

func (c *ExternalController) createBFDStaticRoute(bfdEnabled bool, gw string, podIP, gr, port, mask string) error {
	lrsr := nbdb.LogicalRouterStaticRoute{
		Policy: &nbdb.LogicalRouterStaticRoutePolicySrcIP,
		Options: map[string]string{
			"ecmp_symmetric_reply": "true",
		},
		Nexthop:    gw,
		IPPrefix:   podIP + mask,
		OutputPort: &port,
	}

	ops := []ovsdb.Operation{}
	var err error
	if bfdEnabled {
		bfd := nbdb.BFD{
			DstIP:       gw,
			LogicalPort: port,
		}
		ops, err = libovsdbops.CreateOrUpdateBFDOps(c.nbClient, ops, &bfd)
		if err != nil {
			return fmt.Errorf("error creating or updating BFD %+v: %v", bfd, err)
		}
		lrsr.BFD = &bfd.UUID
	}

	p := func(item *nbdb.LogicalRouterStaticRoute) bool {
		return item.IPPrefix == lrsr.IPPrefix &&
			item.Nexthop == lrsr.Nexthop &&
			item.OutputPort != nil &&
			*item.OutputPort == *lrsr.OutputPort &&
			item.Policy == lrsr.Policy
	}
	ops, err = libovsdbops.CreateOrUpdateLogicalRouterStaticRoutesWithPredicateOps(c.nbClient, ops, gr, &lrsr, p,
		&lrsr.Options)
	if err != nil {
		return fmt.Errorf("error creating or updating static route %+v on router %s: %v", lrsr, gr, err)
	}

	_, err = libovsdbops.TransactAndCheck(c.nbClient, ops)
	if err != nil {
		return fmt.Errorf("error transacting static route: %v", err)
	}

	return nil
}

func getHybridRouteAddrSetDbIDs(nodeName, controller string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.AddressSetHybridNodeRoute, controller,
		map[libovsdbops.ExternalIDKey]string{
			// there is only 1 address set of this type per node
			libovsdbops.ObjectNameKey: nodeName,
		})
}

// addHybridRoutePolicyForPod handles adding a higher priority allow policy to allow traffic to be routed normally
// by ecmp routes
func (c *ExternalController) addHybridRoutePolicyForPod(podIP net.IP, node string) error {
	if config.Gateway.Mode == config.GatewayModeLocal {
		// Add podIP to the node's address_set.
		asIndex := getHybridRouteAddrSetDbIDs(node, controllerName)
		as, err := c.addressSetFactory.EnsureAddressSet(asIndex)
		if err != nil {
			return fmt.Errorf("cannot ensure that addressSet for node %s exists %v", node, err)
		}
		err = as.AddIPs([]net.IP{(podIP)})
		if err != nil {
			return fmt.Errorf("unable to add PodIP %s: to the address set %s, err: %v", podIP.String(), node, err)
		}

		// add allow policy to bypass lr-policy in GR
		ipv4HashedAS, ipv6HashedAS := as.GetASHashNames()
		var l3Prefix string
		var matchSrcAS string
		isIPv6 := utilnet.IsIPv6(podIP)
		if isIPv6 {
			l3Prefix = "ip6"
			matchSrcAS = ipv6HashedAS
		} else {
			l3Prefix = "ip4"
			matchSrcAS = ipv4HashedAS
		}

		// get the GR to join switch ip address
		grJoinIfAddrs, err := util.GetLRPAddrs(c.nbClient, types.GWRouterToJoinSwitchPrefix+types.GWRouterPrefix+node)
		if err != nil {
			return fmt.Errorf("unable to find IP address for node: %s, %s port, err: %v", node, types.GWRouterToJoinSwitchPrefix, err)
		}
		grJoinIfAddr, err := util.MatchFirstIPNetFamily(utilnet.IsIPv6(podIP), grJoinIfAddrs)
		if err != nil {
			return fmt.Errorf("failed to match gateway router join interface IPs: %v, err: %v", grJoinIfAddr, err)
		}

		var matchDst string
		var clusterL3Prefix string
		for _, clusterSubnet := range config.Default.ClusterSubnets {
			if utilnet.IsIPv6CIDR(clusterSubnet.CIDR) {
				clusterL3Prefix = "ip6"
			} else {
				clusterL3Prefix = "ip4"
			}
			if l3Prefix != clusterL3Prefix {
				continue
			}
			matchDst += fmt.Sprintf(" && %s.dst != %s", clusterL3Prefix, clusterSubnet.CIDR)
		}

		// traffic destined outside of cluster subnet go to GR
		matchStr := fmt.Sprintf(`inport == "%s%s" && %s.src == $%s`, types.RouterToSwitchPrefix, node, l3Prefix, matchSrcAS)
		matchStr += matchDst

		logicalRouterPolicy := nbdb.LogicalRouterPolicy{
			Priority: types.HybridOverlayReroutePriority,
			Action:   nbdb.LogicalRouterPolicyActionReroute,
			Nexthops: []string{grJoinIfAddr.IP.String()},
			Match:    matchStr,
		}
		p := func(item *nbdb.LogicalRouterPolicy) bool {
			return item.Priority == logicalRouterPolicy.Priority && strings.Contains(item.Match, matchSrcAS)
		}
		err = libovsdbops.CreateOrUpdateLogicalRouterPolicyWithPredicate(c.nbClient, types.OVNClusterRouter,
			&logicalRouterPolicy, p, &logicalRouterPolicy.Nexthops, &logicalRouterPolicy.Match, &logicalRouterPolicy.Action)
		if err != nil {
			return fmt.Errorf("failed to add policy route %+v to %s: %v", logicalRouterPolicy, types.OVNClusterRouter, err)
		}
	}
	return nil
}

// deletePodGWRoute deletes all associated gateway routing resources for one
// pod gateway route
func (c *ExternalController) deletePodGWRoute(routeInfo *externalRouteInfo, podIP, gw, gr string) error {
	if utilnet.IsIPv6String(gw) != utilnet.IsIPv6String(podIP) {
		return nil
	}

	mask := util.GetIPFullMask(podIP)
	if err := c.deleteLogicalRouterStaticRoute(podIP, mask, gw, gr); err != nil {
		return fmt.Errorf("unable to delete pod %s ECMP route to GR %s, GW: %s: %w",
			routeInfo.podName, gr, gw, err)
	}

	klog.V(5).Infof("ECMP route deleted for pod: %s, on gr: %s, to gw: %s",
		routeInfo.podName, gr, gw)

	node := util.GetWorkerFromGatewayRouter(gr)
	// The gw is deleted from the routes cache after this func is called, length 1
	// means it is the last gw for the pod and the hybrid route policy should be deleted.
	if entry := routeInfo.podExternalRoutes[podIP]; len(entry) == 1 {
		if err := c.delHybridRoutePolicyForPod(net.ParseIP(podIP), node); err != nil {
			return fmt.Errorf("unable to delete hybrid route policy for pod %s: err: %v", routeInfo.podName, err)
		}
	}

	portPrefix, err := c.extSwitchPrefix(node)
	if err != nil {
		return err
	}
	return c.cleanUpBFDEntry(gw, gr, portPrefix)
}

func (c *ExternalController) deleteLogicalRouterStaticRoute(podIP, mask, gw, gr string) error {
	p := func(item *nbdb.LogicalRouterStaticRoute) bool {
		return item.Policy != nil &&
			*item.Policy == nbdb.LogicalRouterStaticRoutePolicySrcIP &&
			item.IPPrefix == podIP+mask &&
			item.Nexthop == gw
	}
	err := libovsdbops.DeleteLogicalRouterStaticRoutesWithPredicate(c.nbClient, gr, p)
	if err != nil {
		return fmt.Errorf("error deleting static route from router %s: %v", gr, err)
	}

	return nil
}

// delHybridRoutePolicyForPod handles deleting a logical route policy that
// forces pod egress traffic to be rerouted to a gateway router for local gateway mode.
func (c *ExternalController) delHybridRoutePolicyForPod(podIP net.IP, node string) error {
	if config.Gateway.Mode == config.GatewayModeLocal {
		// Delete podIP from the node's address_set.
		asIndex := getHybridRouteAddrSetDbIDs(node, controllerName)
		as, err := c.addressSetFactory.EnsureAddressSet(asIndex)
		if err != nil {
			return fmt.Errorf("cannot Ensure that addressSet for node %s exists %v", node, err)
		}
		err = as.DeleteIPs([]net.IP{(podIP)})
		if err != nil {
			return fmt.Errorf("unable to remove PodIP %s: to the address set %s, err: %v", podIP.String(), node, err)
		}

		// delete hybrid policy to bypass lr-policy in GR, only if there are zero pods on this node.
		ipv4HashedAS, ipv6HashedAS := as.GetASHashNames()
		ipv4PodIPs, ipv6PodIPs := as.GetIPs()
		deletePolicy := false
		var l3Prefix string
		var matchSrcAS string
		if utilnet.IsIPv6(podIP) {
			l3Prefix = "ip6"
			if len(ipv6PodIPs) == 0 {
				deletePolicy = true
			}
			matchSrcAS = ipv6HashedAS
		} else {
			l3Prefix = "ip4"
			if len(ipv4PodIPs) == 0 {
				deletePolicy = true
			}
			matchSrcAS = ipv4HashedAS
		}
		if deletePolicy {
			var matchDst string
			var clusterL3Prefix string
			for _, clusterSubnet := range config.Default.ClusterSubnets {
				if utilnet.IsIPv6CIDR(clusterSubnet.CIDR) {
					clusterL3Prefix = "ip6"
				} else {
					clusterL3Prefix = "ip4"
				}
				if l3Prefix != clusterL3Prefix {
					continue
				}
				matchDst += fmt.Sprintf(" && %s.dst != %s", l3Prefix, clusterSubnet.CIDR)
			}
			matchStr := fmt.Sprintf(`inport == "%s%s" && %s.src == $%s`, types.RouterToSwitchPrefix, node, l3Prefix, matchSrcAS)
			matchStr += matchDst

			p := func(item *nbdb.LogicalRouterPolicy) bool {
				return item.Priority == types.HybridOverlayReroutePriority && item.Match == matchStr
			}
			err := libovsdbops.DeleteLogicalRouterPoliciesWithPredicate(c.nbClient, types.OVNClusterRouter, p)
			if err != nil {
				return fmt.Errorf("error deleting policy %s on router %s: %v", matchStr, types.OVNClusterRouter, err)
			}
		}
		if len(ipv4PodIPs) == 0 && len(ipv6PodIPs) == 0 {
			// delete address set.
			err := as.Destroy()
			if err != nil {
				return fmt.Errorf("failed to remove address set: %s, on: %s, err: %v",
					as.GetName(), node, err)
			}
		}
	}
	return nil
}

// cleanUpBFDEntry checks if the BFD table entry related to the associated
// gw router / port / gateway ip is referenced by other routing rules, and if
// not removes the entry to avoid having dangling BFD entries.
func (c *ExternalController) cleanUpBFDEntry(gatewayIP, gatewayRouter, prefix string) error {
	portName := prefix + types.GWRouterToExtSwitchPrefix + gatewayRouter
	p := func(item *nbdb.LogicalRouterStaticRoute) bool {
		return item.OutputPort != nil && *item.OutputPort == portName && item.Nexthop == gatewayIP && item.BFD != nil && *item.BFD != ""
	}
	logicalRouterStaticRoutes, err := libovsdbops.FindLogicalRouterStaticRoutesWithPredicate(c.nbClient, p)
	if err != nil {
		return fmt.Errorf("cleanUpBFDEntry failed to list routes for %s: %w", portName, err)
	}

	if len(logicalRouterStaticRoutes) > 0 {
		return nil
	}

	bfd := nbdb.BFD{
		LogicalPort: portName,
		DstIP:       gatewayIP,
	}

	err = libovsdbops.DeleteBFDs(c.nbClient, &bfd)
	if err != nil {
		return fmt.Errorf("error deleting BFD %+v: %v", bfd, err)
	}

	return nil
}

// deleteGwRoutesForNamespace handles deleting routes to gateways for a pod on a specific GR.
// If a set of gateways is given, only routes for that gateway are deleted. If no gateways
// are given, all routes for the namespace are deleted.
func (c *ExternalController) deleteGWRoutesForNamespace(namespace string, matchGWs sets.String) error {
	deleteAll := (matchGWs == nil || matchGWs.Len() == 0)
	for _, routeInfo := range c.getRouteInfosForNamespace(namespace) {
		routeInfo.Lock()
		if routeInfo.deleted {
			routeInfo.Unlock()
			continue
		}
		for podIP, routes := range routeInfo.podExternalRoutes {
			for gw, gr := range routes {
				if deleteAll || matchGWs.Has(gw) {
					if err := c.deletePodGWRoute(routeInfo, podIP, gw, gr); err != nil {
						// if we encounter error while deleting routes for one pod; we return and don't try subsequent pods
						routeInfo.Unlock()
						return fmt.Errorf("delete pod GW route failed: %w", err)
					}
					delete(routes, gw)
				}
			}
		}
		routeInfo.Unlock()
	}
	return nil
}

// getRouteInfosForNamespace returns all routeInfos for a specific namespace
func (c *ExternalController) getRouteInfosForNamespace(namespace string) []*externalRouteInfo {
	c.exGWCacheMutex.RLock()
	defer c.exGWCacheMutex.RUnlock()

	routes := make([]*externalRouteInfo, 0)
	for namespacedName, routeInfo := range c.externalGWCache {
		if namespacedName.Namespace == namespace {
			routes = append(routes, routeInfo)
		}
	}

	return routes
}
