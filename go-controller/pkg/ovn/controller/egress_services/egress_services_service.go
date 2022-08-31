package egress_services

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	libovsdb "github.com/ovn-org/libovsdb/ovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/controller/services"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	corev1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

func (c *Controller) onServiceAdd(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", obj, err))
		return
	}

	service := obj.(*corev1.Service)
	// We only care about new LoadBalancer services that have the egress-service config annotation
	if !util.ServiceTypeHasLoadBalancer(service) || len(service.Status.LoadBalancer.Ingress) == 0 {
		return
	}

	if !util.HasEgressSVCAnnotation(service) && !util.HasEgressSVCHostAnnotation(service) {
		return
	}

	klog.V(4).Infof("Adding egress service %s", key)
	c.servicesQueue.Add(key)
}

func (c *Controller) onServiceUpdate(oldObj, newObj interface{}) {
	oldService := oldObj.(*corev1.Service)
	newService := newObj.(*corev1.Service)

	// don't process resync or objects that are marked for deletion
	if oldService.ResourceVersion == newService.ResourceVersion ||
		!newService.GetDeletionTimestamp().IsZero() {
		return
	}

	// We only care about LoadBalancer service updates that enable/disable egress service functionality
	if !util.HasEgressSVCAnnotation(oldService) && !util.HasEgressSVCAnnotation(newService) &&
		!util.HasEgressSVCHostAnnotation(oldService) && !util.HasEgressSVCHostAnnotation(newService) &&
		!util.ServiceTypeHasLoadBalancer(oldService) && !util.ServiceTypeHasLoadBalancer(newService) {
		return
	}

	key, err := cache.MetaNamespaceKeyFunc(newObj)
	if err == nil {
		c.servicesQueue.Add(key)
	}
}

func (c *Controller) onServiceDelete(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", obj, err))
		return
	}

	service := obj.(*corev1.Service)
	// We only care about deletions of LoadBalancer services with the annotations to cleanup
	if !util.ServiceTypeHasLoadBalancer(service) {
		return
	}

	if !util.HasEgressSVCAnnotation(service) && !util.HasEgressSVCHostAnnotation(service) {
		return
	}

	klog.V(4).Infof("Deleting egress service %s", key)
	c.servicesQueue.Add(key)
}

func (c *Controller) runServiceWorker(wg *sync.WaitGroup) {
	for c.processNextServiceWorkItem(wg) {
	}
}

func (c *Controller) processNextServiceWorkItem(wg *sync.WaitGroup) bool {
	wg.Add(1)
	defer wg.Done()

	key, quit := c.servicesQueue.Get()
	if quit {
		return false
	}

	defer c.servicesQueue.Done(key)

	err := c.syncService(key.(string))
	if err == nil {
		c.servicesQueue.Forget(key)
		return true
	}

	utilruntime.HandleError(fmt.Errorf("%v failed with : %v", key, err))

	if c.servicesQueue.NumRequeues(key) < maxRetries {
		c.servicesQueue.AddRateLimited(key)
		return true
	}

	c.servicesQueue.Forget(key)
	return true
}

func (c *Controller) syncService(key string) error {
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

	svc, err := c.serviceLister.Services(namespace).Get(name)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}

	state := c.services[key]
	if svc == nil && state == nil {
		// The service was deleted and was not an allocated egress service.
		// We delete it from the unallocated service cache just in case.
		delete(c.unallocatedServices, key)
		return nil
	}

	if svc == nil && state != nil {
		// The service was deleted and was an egress service.
		// We delete all of its relevant resources to avoid leaving stale configuration.
		return c.clearServiceResources(key, state)
	}

	if state != nil && state.stale {
		// The service is marked stale because something failed when trying to delete it.
		// We try to delete it again before doing anything else.
		return c.clearServiceResources(key, state)
	}

	if state == nil && len(svc.Status.LoadBalancer.Ingress) == 0 {
		// The service wasn't configured before and does not have an ingress ip.
		// we don't need to configure it and make sure it does not have a stale host annotation or unallocated entry.
		delete(c.unallocatedServices, key)
		return c.removeServiceNodeAnnotation(namespace, name)
	}

	if state != nil && len(svc.Status.LoadBalancer.Ingress) == 0 {
		// The service has no ingress ips so it is not considered valid anymore.
		return c.clearServiceResources(key, state)
	}

	conf, err := util.ParseEgressSVCAnnotation(svc)
	if err != nil && !util.IsAnnotationNotSetError(err) {
		return err
	}

	if conf == nil && state == nil {
		// The service does not have the config annotation and wasn't configured before.
		// We make sure it does not have a stale host annotation or unallocated entry.
		delete(c.unallocatedServices, key)
		return c.removeServiceNodeAnnotation(namespace, name)
	}

	if conf == nil && state != nil {
		// The service is configured but does no longer have the config annotation,
		// meaning we should clear all of its resources.
		return c.clearServiceResources(key, state)
	}

	// At this point conf != nil

	nodeSelector := &conf.NodeSelector
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

	selector, _ := metav1.LabelSelectorAsSelector(nodeSelector)
	totalEps := len(v4Endpoints) + len(v6Endpoints)

	// We don't want to select a node for a service without endpoints to not "waste" an
	// allocation on a node.
	if totalEps == 0 && state == nil {
		c.unallocatedServices[key] = selector
		return c.removeServiceNodeAnnotation(namespace, name)
	}

	if totalEps == 0 && state != nil {
		c.unallocatedServices[key] = selector
		return c.clearServiceResources(key, state)
	}

	if state == nil {
		// The service has a valid config annotation and wasn't configured before.
		// This means we need to select a node for it that matches its selector.
		c.unallocatedServices[key] = selector

		node, err := c.selectNodeFor(selector)
		if err != nil {
			return err
		}

		// We found a node - update the caches with the new objects.
		delete(c.unallocatedServices, key)
		newState := &svcState{node: node.name, selector: selector, v4Endpoints: sets.NewString(), v6Endpoints: sets.NewString(), stale: false}
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
		return c.clearServiceResources(key, state)
	}

	// At this point the states are valid and we should create the proper logical router policies.
	// We reach the desired state by fetching all of the endpoints associated to the service and comparing
	// to the known state:
	// We need to create policies for endpoints that were fetched but not found in the cache,
	// and delete the policies for those which are found in the cache but were not fetched.
	// We do it in one transaction, if it succeeds we update the cache to reflect the new state.

	err = c.annotateServiceWithNode(namespace, name, state.node) // annotate the service, will also override manual changes
	if err != nil {
		return err
	}

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

	deleteOps, err := c.deleteLogicalRouterPoliciesOps(key, v4ToRemove, v6ToRemove)
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

	// We configured OVN - the last step is to label the node
	// to mark it as the node holding the service.
	return c.labelNodeForService(namespace, name, node.name)
}

// Removes all of the resources that belong to the egress service.
// This includes removing the host annotation, the logical router policies,
// the label from the node and updating the caches.
// This also requeues the service after cleaning up to be sure we are not
// missing an event after marking it as stale that should be handled.
// This should only be called with the controller locked.
func (c *Controller) clearServiceResources(key string, svcState *svcState) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}

	svcState.stale = true
	if err := c.removeServiceNodeAnnotation(namespace, name); err != nil {
		return err
	}

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

	nodeState, found := c.nodes[svcState.node]
	if found {
		if err := c.removeNodeServiceLabel(namespace, name, svcState.node); err != nil {
			return fmt.Errorf("failed to remove svc node label for %s, err: %v", svcState.node, err)
		}
		delete(nodeState.allocations, key)
	}

	delete(c.services, key)
	c.servicesQueue.Add(key)
	return nil
}

// Annotates the given service with the 'k8s.ovn.org/egress-service-host=<node>' annotation
func (c *Controller) annotateServiceWithNode(namespace, name string, node string) error {
	annotations := map[string]any{util.EgressSVCHostAnnotation: node}
	return c.patchServiceAnnotations(namespace, name, annotations)
}

// Removes the 'k8s.ovn.org/egress-service-host=<node>' annotation from the given service.
func (c *Controller) removeServiceNodeAnnotation(namespace, name string) error {
	annotations := map[string]any{util.EgressSVCHostAnnotation: nil} // Patching with a nil value results in the delete of the key
	return c.patchServiceAnnotations(namespace, name, annotations)
}

// Patches the service's metadata.annotations with the given annotations.
func (c *Controller) patchServiceAnnotations(namespace, name string, annotations map[string]any) error {
	patch := struct {
		Metadata map[string]any `json:"metadata"`
	}{
		Metadata: map[string]any{
			"annotations": annotations,
		},
	}

	klog.V(4).Infof("Setting annotations %v on service %s/%s", annotations, namespace, name)
	patchData, err := json.Marshal(&patch)
	if err != nil {
		klog.Errorf("Error in setting annotations on service %s/%s: %v", namespace, name, err)
		return err
	}

	_, err = c.client.CoreV1().Services(namespace).Patch(context.TODO(), name, types.MergePatchType, patchData, metav1.PatchOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}

	return nil
}

// Returns all of the non-host endpoints for the given service grouped by IPv4/IPv6.
func (c *Controller) allEndpointsFor(svc *corev1.Service) (sets.String, sets.String, []string, error) {
	// Get the endpoint slices associated to the Service
	esLabelSelector := labels.Set(map[string]string{
		discovery.LabelServiceName: svc.Name,
	}).AsSelectorPreValidated()

	endpointSlices, err := c.endpointSliceLister.EndpointSlices(svc.Namespace).List(esLabelSelector)
	if err != nil {
		return nil, nil, nil, err
	}

	v4Endpoints := sets.NewString()
	v6Endpoints := sets.NewString()
	nodes := sets.NewString()
	for _, eps := range endpointSlices {
		if eps.AddressType == discovery.AddressTypeFQDN {
			continue
		}

		epsToInsert := v4Endpoints
		if eps.AddressType == discovery.AddressTypeIPv6 {
			epsToInsert = v6Endpoints
		}

		for _, ep := range eps.Endpoints {
			for _, ip := range ep.Addresses {
				if !services.IsHostEndpoint(ip) {
					epsToInsert.Insert(ip)
				}
			}
			if ep.NodeName != nil {
				nodes.Insert(*ep.NodeName)
			}
		}
	}

	return v4Endpoints, v6Endpoints, nodes.UnsortedList(), nil
}

// Returns the libovsdb operations to create the logical router policies for the service,
// given its key, the nexthops (mgmt ips) and endpoints to add.
func (c *Controller) createLogicalRouterPoliciesOps(key, v4MgmtIP, v6MgmtIP string, v4Endpoints, v6Endpoints []string) ([]libovsdb.Operation, error) {
	allOps := []libovsdb.Operation{}
	var err error

	for _, addr := range v4Endpoints {
		lrp := &nbdb.LogicalRouterPolicy{
			Match:    fmt.Sprintf("ip4.src == %s", addr),
			Priority: ovntypes.EgressSVCReroutePriority,
			Nexthops: []string{v4MgmtIP},
			Action:   nbdb.LogicalRouterPolicyActionReroute,
			ExternalIDs: map[string]string{
				svcExternalIDKey: key,
			},
		}
		p := func(item *nbdb.LogicalRouterPolicy) bool {
			return item.Match == lrp.Match && item.Priority == lrp.Priority && item.ExternalIDs[svcExternalIDKey] == key
		}

		allOps, err = libovsdbops.CreateOrUpdateLogicalRouterPolicyWithPredicateOps(c.nbClient, allOps, ovntypes.OVNClusterRouter, lrp, p)
		if err != nil {
			return nil, err
		}
	}

	for _, addr := range v6Endpoints {
		lrp := &nbdb.LogicalRouterPolicy{
			Match:    fmt.Sprintf("ip6.src == %s", addr),
			Priority: ovntypes.EgressSVCReroutePriority,
			Nexthops: []string{v6MgmtIP},
			Action:   nbdb.LogicalRouterPolicyActionReroute,
			ExternalIDs: map[string]string{
				svcExternalIDKey: key,
			},
		}
		p := func(item *nbdb.LogicalRouterPolicy) bool {
			return item.Match == lrp.Match && item.Priority == lrp.Priority && item.ExternalIDs[svcExternalIDKey] == key
		}

		allOps, err = libovsdbops.CreateOrUpdateLogicalRouterPolicyWithPredicateOps(c.nbClient, allOps, ovntypes.OVNClusterRouter, lrp, p)
		if err != nil {
			return nil, err
		}
	}

	return allOps, nil
}

// Returns the libovsdb operations to delete the logical router policies for the service,
// given its key and endpoints to delete.
func (c *Controller) deleteLogicalRouterPoliciesOps(key string, v4Endpoints, v6Endpoints []string) ([]libovsdb.Operation, error) {
	allOps := []libovsdb.Operation{}
	var err error

	for _, addr := range v4Endpoints {
		match := fmt.Sprintf("ip4.src == %s", addr)
		p := func(item *nbdb.LogicalRouterPolicy) bool {
			return item.Match == match && item.Priority == ovntypes.EgressSVCReroutePriority && item.ExternalIDs[svcExternalIDKey] == key
		}

		allOps, err = libovsdbops.DeleteLogicalRouterPolicyWithPredicateOps(c.nbClient, allOps, ovntypes.OVNClusterRouter, p)
		if err != nil {
			return nil, err
		}
	}

	for _, addr := range v6Endpoints {
		match := fmt.Sprintf("ip6.src == %s", addr)
		p := func(item *nbdb.LogicalRouterPolicy) bool {
			return item.Match == match && item.Priority == ovntypes.EgressSVCReroutePriority && item.ExternalIDs[svcExternalIDKey] == key
		}

		allOps, err = libovsdbops.DeleteLogicalRouterPolicyWithPredicateOps(c.nbClient, allOps, ovntypes.OVNClusterRouter, p)
		if err != nil {
			return nil, err
		}
	}

	return allOps, nil
}
