package apbroute

import (
	"reflect"
	"sync"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
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
	matches := sets.NewString()
	matches, err := c.filterPoliciesByNamespace(new.Name)
	if err != nil {
		return err
	}
	// set of policies that apply to this namespace based on the label selector of the policies
	c.namespacePoliciesCache[new.Name] = matches
	return err
}

func (c *ExternalController) filterPoliciesByNamespace(namespaceName string) (sets.String, error) {
	var errors []error
	matches := sets.NewString()

	policies, err := c.routeLister.List(labels.Everything())
	if err != nil {
		errors = append(errors, err)
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

func (c *ExternalController) processUpdateNamespace(old, new *v1.Namespace) error {

	// list the policies that apply to the new object
	newPolicies, err := c.getPoliciesForTargetNamespace(new.Name)
	if err != nil {
		return err
	}
	if c.namespacePoliciesCache[new.Name].Equal(newPolicies) {
		// same policies apply
		return nil
	}
	// some differences apply, let's figure out if previous policies have been removed first
	oldPoliciesDiff := c.namespacePoliciesCache[new.Name].Difference(newPolicies)
	// iterate through the policies that no longer apply to this namespace
	for policyName := range oldPoliciesDiff {
		err = c.removePolicyFromNamespace(new.Name, policyName)
		if err != nil {
			return err
		}
	}
	newPoliciesDiff := newPolicies.Difference(c.namespacePoliciesCache[new.Name])
	for policyName := range newPoliciesDiff {
		err = c.applyPolicyToNamespace(new.Name, policyName)
		if err != nil {
			return err
		}
	}
	if len(newPolicies) == 0 {
		// no policies apply to this namespace anymore, delete it from the cache
		delete(c.namespacePoliciesCache, new.Name)
		return nil
	}
	// at least one policy apply, let's update the cache
	c.namespacePoliciesCache[new.Name] = newPolicies
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
	delete(c.namespacePoliciesCache, ns.Name)
	return nil
}
