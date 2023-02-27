package apbroute

import (
	"fmt"
	"sync"

	adminpolicybasedrouteapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/cache"
)

func (c *ExternalController) onPodAdd(obj interface{}) {
	c.podQueue.Add(
		handlerObj{
			op:     addOp,
			newObj: obj,
		})
}

func (c *ExternalController) onPodUpdate(oldObj, newObj interface{}) {
	oldPod := oldObj.(*v1.Pod)
	newPod := newObj.(*v1.Pod)

	if oldPod.ResourceVersion == newPod.ResourceVersion ||
		!newPod.GetDeletionTimestamp().IsZero() {
		return
	}

	c.podQueue.Add(
		handlerObj{
			op:     updateOp,
			oldObj: oldObj,
			newObj: newObj,
		},
	)
}

func (c *ExternalController) onPodDelete(obj interface{}) {
	c.podQueue.Add(handlerObj{newObj: obj, op: deleteOp})
}

func (c *ExternalController) runPodWorker(wg *sync.WaitGroup) {
	for c.processNextPodWorkItem(wg) {
	}
}

func (c *ExternalController) processNextPodWorkItem(wg *sync.WaitGroup) bool {
	wg.Add(1)
	defer wg.Done()

	key, shutdown := c.podQueue.Get()

	if shutdown {
		return true
	}

	defer c.podQueue.Done(key)

	item := key.(handlerObj)
	var err error
	switch item.op {
	case addOp:
		err = c.processAddPod(item.newObj.(*v1.Pod))
	case updateOp:
		oldObj := item.oldObj.(*v1.Pod)
		newObj := item.newObj.(*v1.Pod)
		err = c.processUpdatePod(oldObj, newObj)
	case deleteOp:
		err = c.processDeletePod(item.newObj.(*v1.Pod))
	}
	if err == nil {
		c.podQueue.Forget(key)
	}
	return false
}

// processAddPod covers 2 scenarios:
// 1) The pod is an external gateway, in which case it needs to propagate its IP to a set of pods in the cluster.
// Determining which namespaces to update is determined by matching the pod's namespace and label selector against
// all the existing Admin Policy Based External route CRs. It's a reverse lookup:
//
//	pod GW -> dynamic hop -> APB External Route CR -> target namespaces (label selector in the CR's `From`` field) -> pods in namespace
//
// 2) The pod belongs to a namespace impacted by at least one APB External Route CR, in which case its logical routes need to be
// updated to reflect the external routes.
//
// A pod can only be either an external gateway or a consumer of an external route policy.
func (c *ExternalController) processAddPod(newPod *v1.Pod) error {
	if _, ok := c.namespacePoliciesCache[newPod.Namespace]; ok {
		// pod's namespace is a target to at least one policy, adding all external GWs to the new pod
		return c.processAddPodRoutes(newPod)
	}

	// the pod can either be a gateway pod or a standard pod that requires no processing from the external controller.
	// to determine either way, we find out which matching dynamic hops include this pod. If none applies, then this is
	// a standard pod.
	podPolicies, err := c.findMatchingAPBExternalDynamicPolicies(newPod)
	if err != nil {
		return err
	}
	if len(podPolicies) > 0 {
		return c.applyPodGWPolicies(newPod, podPolicies)
	}
	return nil
}

func (c *ExternalController) applyPodGWPolicies(pod *v1.Pod, podPolicies []*adminpolicybasedrouteapi.ExternalPolicy) error {
	// routePolicies contain the gateway information of the pod for each dynamic hop that covers this pod
	// and the list of namespaces targeted by the namespace selector in the dynamic hop
	routePolicies, err := c.getRoutePoliciesForPodGateway(pod, podPolicies)
	if err != nil {
		return err
	}
	// update all namespaces targeted by this pod's policy to include the new pod IP as their external GW
	nsSet, err := c.processRoutePolicies(routePolicies)
	if err != nil {
		return err
	}
	// update pod cache with the list of namespaces that the pod is serving as Gateway
	if _, ok := c.gatewayPodsNamespaceCache[pod.Namespace]; !ok {
		c.gatewayPodsNamespaceCache[pod.Namespace] = make(map[string]sets.String)
	}
	c.gatewayPodsNamespaceCache[pod.Namespace][pod.Name] = nsSet
	return nil

}

// processUpdatePod has to tackle a different set of scenarios:
// case 1: Old and new pods are gateways: Need to update the new Pod IP to all impacted namespaces. First delete the old one and then add the new one
// case 2: Old is a gateway but the new is not: Delete the old pod's IP from all impacted namespaces
// case 3: Old is not a gateway but the new one is: Add the new pod's IP to all impacted namespaces defined by the matching policies
// case 4: Old and new belong to a namespace impacted by a policy: Update the logical routes to the pod as defined by the external routes.
// case 5: Old and new are not gateways and are not impacted by a policy: nothing to do
func (c *ExternalController) processUpdatePod(oldPod, newPod *v1.Pod) error {
	if _, ok := c.namespacePoliciesCache[newPod.Namespace]; ok {
		// case 4:Old and new belong to a namespace impacted by a policy: Update the logical routes to the pod as defined by the external routes.
		return c.processAddPodRoutes(newPod)
	}
	// TODO (jordigilh): instead of deleting the old gateway info and adding the new one, check if both pod's IP have changed and if the target namespaces have also changed
	// before applying this logic. By performing the check first, we can avoid disrupting the logical routes if there has been no impact between the two pod instances.

	if c.gatewayPodsNamespaceCache[oldPod.Namespace] != nil && c.gatewayPodsNamespaceCache[oldPod.Namespace][oldPod.Name] != nil {
		// delete the old pod gateway information
		err := c.deletePodGateway(oldPod)
		if err != nil {
			return err
		}
	}

	// find the policies that apply to this new pod. Unless there are changes to the labels, they should be identical.
	newPodPolicies, err := c.findMatchingAPBExternalDynamicPolicies(newPod)
	if err != nil {
		return err
	}
	// add the new pod's gatewa information only if matches a route policy
	if len(newPodPolicies) > 0 {
		return c.applyPodGWPolicies(newPod, newPodPolicies)
	}
	return nil
}

func (c *ExternalController) processDeletePod(pod *v1.Pod) error {
	if c.gatewayPodsNamespaceCache[pod.Namespace] == nil ||
		c.gatewayPodsNamespaceCache[pod.Namespace] != nil && c.gatewayPodsNamespaceCache[pod.Namespace][pod.Name] == nil {
		// nothing to do, this pod is not a gateway pod
		return nil
	}
	return c.deletePodGateway(pod)
}

func (c *ExternalController) deletePodGateway(pod *v1.Pod) error {
	var errors []error
	for ns := range c.gatewayPodsNamespaceCache[pod.Namespace][pod.Name] {
		// remove the IP from the pods in the namespaces where the pod gateway was used
		err := c.deletePodGWRoutesForNamespace(pod, ns)
		if err != nil {
			errors = append(errors, err)
		}
	}
	// delete the cache reference
	delete(c.gatewayPodsNamespaceCache[pod.Namespace], pod.Name)
	if len(c.gatewayPodsNamespaceCache[pod.Namespace]) == 0 {
		delete(c.gatewayPodsNamespaceCache, pod.Namespace)
	}
	return kerrors.NewAggregate(errors)
}

// processAddPodRoutes applies the policies associated to the pod's namespace to the pod logical route
func (c *ExternalController) processAddPodRoutes(newPod *v1.Pod) error {
	policyNames := c.namespacePoliciesCache[newPod.Namespace]
	var errors []error
	for policyName := range policyNames {
		routePolicy, err := c.routeLister.Get(policyName)
		if err != nil {
			errors = append(errors, err)
			continue
		}
		pp, err := c.processPolicies(routePolicy)
		for _, p := range pp {
			c.addGWRoutesForPodInNamespace(newPod, p.staticGateways)
			for _, egress := range p.dynamicGateways {
				c.addGWRoutesForPodInNamespace(newPod, gatewayInfoList{egress})
			}
		}

	}
	return nil
}

// processPodRoutePolicies iterates through the dynamic hops to determine the pod's GW information.
// Note that a pod can match multiple policies with different configuration at the same time, with the condition
// that the pod can only target the same namespace once at most. That's a 1-1 pod to namespace match.
func (c *ExternalController) getRoutePoliciesForPodGateway(newPod *v1.Pod, policies []*adminpolicybasedrouteapi.ExternalPolicy) ([]*routePolicy, error) {

	var rp []*routePolicy
	key, err := cache.MetaNamespaceKeyFunc(newPod)
	if err != nil {
		return nil, err
	}
	for _, p := range policies {
		pp, err := c.processPolicy(p)
		if err != nil {
			return nil, err
		}
		if _, ok := pp.dynamicGateways[key]; !ok {
			return nil, fmt.Errorf("pod %s not found while processing dynamic hops", key)
		}
		// store only the information we need
		rp = append(rp, &routePolicy{
			labelSelector:   pp.labelSelector,
			dynamicGateways: map[string]*gatewayInfo{key: pp.dynamicGateways[key]},
		})
	}
	return rp, nil

}

func (c *ExternalController) findMatchingAPBExternalDynamicPolicies(pod *v1.Pod) ([]*adminpolicybasedrouteapi.ExternalPolicy, error) {
	var policies []*adminpolicybasedrouteapi.ExternalPolicy
	crs, err := c.routeLister.List(labels.Everything())
	if err != nil {
		return nil, err
	}
	for _, cr := range crs {
		for _, p := range cr.Spec.Policies {
			policy := &adminpolicybasedrouteapi.ExternalPolicy{
				From:     p.From,
				NextHops: adminpolicybasedrouteapi.ExternalNextHops{DynamicHops: []*adminpolicybasedrouteapi.DynamicHop{}}}
			for _, dp := range p.NextHops.DynamicHops {
				nss, err := c.listNamespacesBySelector(dp.NamespaceSelector)
				if err != nil {
					return nil, err
				}
				if !containsNamespaceInSlice(nss, pod.Namespace) {
					continue
				}
				nsPods, err := c.listPodsInNamespaceWithSelector(pod.Namespace, &dp.PodSelector)
				if containsPodInSlice(nsPods, pod.Name) {
					// add only the hop information that intersects with the pod
					policy.NextHops.DynamicHops = append(policy.NextHops.DynamicHops, dp)
				}
			}
			if len(policy.NextHops.DynamicHops) > 0 {
				policies = append(policies, policy)
			}

		}
	}
	return policies, nil
}

func containsNamespaceInSlice(nss []*v1.Namespace, podNs string) bool {
	for _, ns := range nss {
		if ns.Name == podNs {
			return true
		}
	}
	return false
}

func containsPodInSlice(pods []*v1.Pod, podName string) bool {
	for _, pod := range pods {
		if pod.Name == podName {
			return true
		}
	}
	return false
}
