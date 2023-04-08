package apbroute

import (
	"fmt"

	adminpolicybasedrouteapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
)

// processAddNamespace takes in a namespace and applies the policies that are applicable to the namespace, previously stored in the cacheInfo object argument.
// The logic goes through all the policies and applies the gateway IPs derived from the static and dynamic hop to all the pods in the namespace.
// Lastly, it updates the cacheInfo to contain the static and dynamic gateway IPs generated from the previous action to keep track of the gateway IPs applied in the namespace.
func (m *externalPolicyManager) processAddNamespace(new *v1.Namespace, cacheInfo *namespaceInfo) error {
	staticGateways, dynamicGateways, err := m.aggregateNamespaceInfo(cacheInfo.policies)
	if err != nil {
		return err
	}
	cacheInfo.staticGateways = staticGateways
	cacheInfo.dynamicGateways = dynamicGateways
	return nil
}

// processUpdateNamespace takes in a namespace name, current policies applied to the namespace, policies that are now expected to be applied to the namespace and the cache info
// that contains all the current gateway IPs and policies for that namespace. It follows this logic:
// * Calculate the difference between current and expected policies and proceed to remove the gateway IPs from the policies that are no longer applicable to this namespace
// * Calculate the difference between the expected and current ones to determine the new policies to be applied and proceed to apply them.
// * Update the cache info with the new list of policies, as well as the static and dynamic gateway IPs derived from executing the previous logic.
func (m *externalPolicyManager) processUpdateNamespace(namespaceName string, currentPolicies, newPolicies sets.Set[string], cacheInfo *namespaceInfo) error {

	// some differences apply, let's figure out if previous policies have been removed first
	policiesNotValid := currentPolicies.Difference(newPolicies)
	// iterate through the policies that no longer apply to this namespace
	for policyName := range policiesNotValid {
		err := m.removePolicyFromNamespaceWithName(namespaceName, policyName, cacheInfo)
		if err != nil {
			return err
		}
	}

	// policies that now apply to this namespace
	newPoliciesDiff := newPolicies.Difference(currentPolicies)
	for policyName := range newPoliciesDiff {
		policy, found, markedForDeletion := m.getRoutePolicyFromCache(policyName)
		if !found {
			return fmt.Errorf("failed to find external route policy %s in cache", policyName)
		}
		if markedForDeletion {
			klog.Infof("Skipping route policy %s as it has been marked for deletion", policyName)
			continue
		}
		err := m.applyPolicyToNamespace(namespaceName, &policy, cacheInfo)
		if err != nil {
			return err
		}
	}
	// at least one policy apply, let's update the cache
	cacheInfo.policies = newPolicies
	return nil

}

func (m *externalPolicyManager) applyPolicyToNamespace(namespaceName string, policy *adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute, cacheInfo *namespaceInfo) error {

	processedPolicy, err := m.processExternalRoutePolicy(policy)
	if err != nil {
		return err
	}
	err = m.applyProcessedPolicyToNamespace(namespaceName, policy.Name, processedPolicy, cacheInfo)
	if err != nil {
		return err
	}
	return nil
}

func (m *externalPolicyManager) removePolicyFromNamespaceWithName(targetNamespace, policyName string, cacheInfo *namespaceInfo) error {
	policy, err := m.routeLister.Get(policyName)
	if err != nil {
		return err
	}
	return m.removePolicyFromNamespace(targetNamespace, policy, cacheInfo)
}
func (m *externalPolicyManager) removePolicyFromNamespace(targetNamespace string, policy *adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute, cacheInfo *namespaceInfo) error {

	processedPolicy, err := m.processExternalRoutePolicy(policy)
	if err != nil {
		return err
	}
	err = m.deletePolicyInNamespace(targetNamespace, policy.Name, processedPolicy, cacheInfo)
	if err != nil {
		return err
	}
	cacheInfo.policies.Delete(policy.Name)
	return nil
}

func (m *externalPolicyManager) listNamespacesBySelector(selector *metav1.LabelSelector) ([]*v1.Namespace, error) {
	s, err := metav1.LabelSelectorAsSelector(selector)
	if err != nil {
		return nil, err
	}
	ns, err := m.namespaceLister.List(s)
	if err != nil {
		return nil, err
	}
	return ns, nil

}

func (m *externalPolicyManager) aggregateNamespaceInfo(policies sets.Set[string]) (gatewayInfoList, map[ktypes.NamespacedName]*gatewayInfo, error) {

	static := gatewayInfoList{}
	dynamic := make(map[ktypes.NamespacedName]*gatewayInfo)
	for policyName := range policies {
		externalPolicy, err := m.routeLister.Get(policyName)
		if err != nil {
			klog.Warningf("Unable to find route policy %s:%+v", policyName, err)
			continue
		}
		processedPolicy, err := m.processExternalRoutePolicy(externalPolicy)
		if err != nil {
			return nil, nil, err
		}
		var duplicated sets.Set[string]
		static, duplicated = static.Insert(processedPolicy.staticGateways...)
		if duplicated.Len() > 0 {
			klog.Warningf("Found duplicated gateway IP(s) %+s in policy(s) %+s", sets.List(duplicated), sets.List(policies))
		}
		for podName, gatewayInfo := range processedPolicy.dynamicGateways {
			dynamic[podName] = gatewayInfo
		}
	}
	return static, dynamic, nil
}
