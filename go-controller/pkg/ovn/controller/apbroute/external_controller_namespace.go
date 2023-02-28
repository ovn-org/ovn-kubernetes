package apbroute

import (
	"fmt"
	"reflect"
	"sync"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	ktypes "k8s.io/apimachinery/pkg/types"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
)

func (c *ExternalController) onNamespaceAdd(obj interface{}) {
	c.namespaceQueue.Add(
		handlerObj{
			newObj: obj,
			op:     addOp,
		})

}

func (c *ExternalController) onNamespaceUpdate(oldObj, newObj interface{}) {
	oldNamespace := oldObj.(*v1.Namespace)
	newNamespace := newObj.(*v1.Namespace)

	if oldNamespace.Generation == newNamespace.Generation ||
		!newNamespace.GetDeletionTimestamp().IsZero() {
		return
	}

	if !reflect.DeepEqual(oldNamespace.Labels, newNamespace.Labels) {
		// we only care about changes to the labels that could impact the policy selectors
		c.namespaceQueue.Add(handlerObj{
			oldObj: oldObj,
			newObj: newObj,
			op:     updateOp,
		})
	}
}

func (c *ExternalController) onNamespaceDelete(obj interface{}) {
	c.namespaceQueue.Add(
		handlerObj{
			op:     deleteOp,
			newObj: obj,
		})
}

func (c *ExternalController) runNamespaceWorker(wg *sync.WaitGroup) {
	for c.processNextNamespaceWorkItem(wg) {

	}
}

func (c *ExternalController) processNextNamespaceWorkItem(wg *sync.WaitGroup) bool {
	wg.Add(1)
	defer wg.Done()

	key, shutdown := c.namespaceQueue.Get()

	if shutdown {
		return true
	}

	defer c.namespaceQueue.Done(key)

	item := key.(handlerObj)
	var err error
	switch item.op {
	case addOp:
		err = c.processAddNamespace(item.newObj.(*v1.Namespace))
	case updateOp:
		oldObj := item.oldObj.(*v1.Namespace)
		newObj := item.newObj.(*v1.Namespace)
		err = c.processUpdateNamespace(oldObj, newObj)
	case deleteOp:
		err = c.processDeleteNamespace(item.newObj.(*v1.Namespace))
	}
	if err == nil {
		c.namespaceQueue.Forget(key)
	}
	return false
}

func (c *ExternalController) processAddNamespace(new *v1.Namespace) error {
	matches, err := c.getPoliciesForTargetNamespace(new.Name)
	if err != nil {
		return err
	}
	nsInfo := namespaceInfo{
		policies:        matches,
		dynamicGateways: make(map[ktypes.NamespacedName]*gatewayInfo),
		staticGateways:  sets.NewString(),
	}
	// populate the cache with the policies that apply to this namespace as well as the static and dynamic gateway IPs
	nsInfo.staticGateways, nsInfo.dynamicGateways, err = c.populateNamespaceInfo(new.Name, matches)
	if err != nil {
		return err
	}
	c.namespaceInfoCache[new.Name] = &nsInfo
	return c.addGatewayAnnotation(new.Name, nsInfo.dynamicGateways)
}

func (c *ExternalController) populateNamespaceInfo(namespaceName string, policies sets.String) (sets.String, map[ktypes.NamespacedName]*gatewayInfo, error) {

	static := sets.NewString()
	dynamic := make(map[ktypes.NamespacedName]*gatewayInfo)
	for policyName := range policies {
		externalPolicy, err := c.routeLister.Get(policyName)
		if err != nil {
			return nil, nil, err
		}
		routePolicyList, err := c.processPolicies(externalPolicy)
		if err != nil {
			return nil, nil, err
		}
		for _, pp := range routePolicyList {
			for _, gatewayInfo := range pp.staticGateways {
				static.Insert(gatewayInfo.gws.UnsortedList()...)
			}
			for podName, gatewayInfo := range pp.dynamicGateways {
				dynamic[podName] = gatewayInfo
			}
		}
	}
	return static, dynamic, nil
}
func (c *ExternalController) addGatewayAnnotation(namespaceName string, dynamicGateways map[ktypes.NamespacedName]*gatewayInfo) error {

	egressIPs := sets.NewString()
	for _, gwInfo := range dynamicGateways {
		for ip := range gwInfo.gws {
			if egressIPs.Has(ip) {
				return fmt.Errorf("duplicated IP %s found while iterating through the list of pod gateways for namespace %s", ip, namespaceName)
			}
		}
		egressIPs.Insert(gwInfo.gws.UnsortedList()...)
	}
	// add the exgw podIP to the namespace's k8s.ovn.org/external-gw-pod-ips list
	if err := util.UpdateExternalGatewayPodIPsAnnotation(c.kube, namespaceName, egressIPs.List()); err != nil {
		klog.Errorf("Unable to update %s/%v annotation for namespace %s: %v", util.ExternalGatewayPodIPsAnnotation, egressIPs, namespaceName, err)
	}
	return nil
}

func (c *ExternalController) processUpdateNamespace(old, new *v1.Namespace) error {

	// list the policies that apply to the new object
	newPolicies, err := c.getPoliciesForTargetNamespace(new.Name)
	if err != nil {
		return err
	}
	if c.namespaceInfoCache[new.Name].policies.Equal(newPolicies) {
		// same policies apply
		return nil
	}
	// some differences apply, let's figure out if previous policies have been removed first
	oldPoliciesDiff := c.namespaceInfoCache[new.Name].policies.Difference(newPolicies)
	// iterate through the policies that no longer apply to this namespace
	for policyName := range oldPoliciesDiff {
		err = c.removePolicyFromNamespace(new.Name, policyName)
		if err != nil {
			return err
		}
	}
	newPoliciesDiff := newPolicies.Difference(c.namespaceInfoCache[new.Name].policies)
	for policyName := range newPoliciesDiff {
		err = c.applyPolicyToNamespace(new.Name, policyName)
		if err != nil {
			return err
		}
	}
	if len(newPolicies) == 0 {
		// no policies apply to this namespace anymore, delete it from the cache
		delete(c.namespaceInfoCache, new.Name)
		return nil
	}
	// at least one policy apply, let's update the cache
	c.namespaceInfoCache[new.Name].policies = newPolicies
	return nil

}

func (c *ExternalController) applyPolicyToNamespace(namespaceName, policyName string) error {

	var errors []error
	routePolicy, err := c.routeLister.Get(policyName)
	if err != nil {
		return err
	}
	ns, err := c.namespaceLister.Get(namespaceName)
	if err != nil {
		return err
	}
	nsSlice := []*v1.Namespace{ns}
	for _, routePolicy := range routePolicy.Spec.Policies {
		errors = append(errors, c.addStaticHops(routePolicy.NextHops.StaticHops, nsSlice))
		errors = append(errors, c.addDynamicHops(routePolicy.NextHops.DynamicHops, nsSlice))
	}
	return kerrors.NewAggregate(errors)
}

func (c *ExternalController) getPoliciesForTargetNamespace(namespaceName string) (sets.String, error) {
	matches := sets.NewString()
	var errors []error
	policies, err := c.routeLister.List(labels.Everything())
	if err != nil {
		errors = append(errors, err)
	}

	if len(errors) > 0 {
		return nil, kerrors.NewAggregate(errors)
	}
	for _, policy := range policies {
		for _, p := range policy.Spec.Policies {
			targetNamespaces, err := c.GetNamespacesBySelector(&p.From.NamespaceSelector)
			if err != nil {
				errors = append(errors, err)
			}
			for _, ns := range targetNamespaces {
				if namespaceName == ns.Name {
					// this policy's from namespace selector includes this namespace.
					matches = matches.Insert(policy.Name)
				}
			}
		}
	}
	return matches, kerrors.NewAggregate(errors)
}

func (c *ExternalController) removePolicyFromNamespace(targetNamespace, policyName string) error {

	var errors []error
	routePolicy, err := c.routeLister.Get(policyName)
	if err != nil {
		return err
	}
	ns, err := c.namespaceLister.Get(targetNamespace)
	if err != nil {
		return err
	}
	nsSlice := []*v1.Namespace{ns}
	for _, routePolicy := range routePolicy.Spec.Policies {
		errors = append(errors, c.deleteStaticHops(routePolicy.NextHops.StaticHops, nsSlice))
		errors = append(errors, c.deleteDynamicHops(routePolicy.NextHops.DynamicHops, nsSlice))
	}
	return kerrors.NewAggregate(errors)
}

func (c *ExternalController) processDeleteNamespace(ns *v1.Namespace) error {
	delete(c.namespaceInfoCache, ns.Name)
	return nil
}
