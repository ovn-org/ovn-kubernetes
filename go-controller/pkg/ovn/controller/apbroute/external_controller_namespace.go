package apbroute

import (
	"fmt"
	"sync"

	adminpolicybasedrouteapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
)

func (m *externalPolicyManager) processAddNamespace(new *v1.Namespace, cacheInfo *namespaceInfo) error {
	// populate the cache with the policies that apply to this namespace as well as the static and dynamic gateway IPs

	staticGateways, dynamicGateways, err := m.aggregateNamespaceInfo(new.Name, cacheInfo.policies)
	if err != nil {
		return err
	}
	cacheInfo.staticGateways = staticGateways
	cacheInfo.dynamicGateways = dynamicGateways
	return nil
}

func (m *externalPolicyManager) getNamespaceInfoFromCache(namespaceName string) (*namespaceInfo, bool) {
	m.namespaceLockMapMutex.Lock()
	mux, ok := m.namespaceLockMap[namespaceName]
	m.namespaceLockMapMutex.Unlock()
	if !ok {
		return nil, false
	}
	m.namespaceInfoCacheMutex.Lock()
	defer m.namespaceInfoCacheMutex.Unlock()
	mux.Lock()
	nsInfo, ok := m.namespaceInfoCache[namespaceName]
	if !ok {
		mux.Unlock()
		return nil, false
	}
	return nsInfo, true
}

func (m *externalPolicyManager) deleteNamespaceInfoInCache(namespaceName string) {
	m.namespaceLockMapMutex.Lock()
	_, ok := m.namespaceLockMap[namespaceName]
	m.namespaceLockMapMutex.Unlock()
	if !ok {
		klog.Infof("Unable to delete a namespace that has no mutex lock")
		return
	}
	m.namespaceInfoCacheMutex.Lock()
	delete(m.namespaceInfoCache, namespaceName)
	m.namespaceInfoCacheMutex.Unlock()
}

func (m *externalPolicyManager) unlockNamespaceInfoCache(namespaceName string) {
	m.namespaceLockMapMutex.Lock()
	mux, ok := m.namespaceLockMap[namespaceName]
	m.namespaceLockMapMutex.Unlock()
	if ok {
		mux.Unlock()
	}
}

func (m *externalPolicyManager) newNamespaceInfoInCache(namespaceName string) *namespaceInfo {
	m.namespaceLockMapMutex.Lock()
	mux, ok := m.namespaceLockMap[namespaceName]
	if !ok {
		mux = &sync.Mutex{}
		m.namespaceLockMap[namespaceName] = mux
	}
	m.namespaceLockMapMutex.Unlock()
	m.namespaceInfoCacheMutex.Lock()
	mux.Lock()
	_, found := m.namespaceInfoCache[namespaceName]
	if !found {
		m.namespaceInfoCache[namespaceName] = newNamespaceInfo()
	}
	mux.Unlock()
	m.namespaceInfoCacheMutex.Unlock()
	nsInfo, _ := m.getNamespaceInfoFromCache(namespaceName)
	return nsInfo
}

func (m *externalPolicyManager) listNamespaceInfoCache() []string {
	m.namespaceInfoCacheMutex.Lock()
	keys := make([]string, 0)
	defer m.namespaceInfoCacheMutex.Unlock()
	for key := range m.namespaceInfoCache {
		if key == "" {
			continue
		}
		keys = append(keys, key)
	}
	return keys
}

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
		policy, found := m.getRoutePolicyFromCache(policyName)
		if !found {
			return fmt.Errorf("failed to find external route policy %s in cache", policyName)
		}
		err := m.applyPolicyToNamespace(namespaceName, policy, cacheInfo)
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
	err = m.deleteProcessedPolicyInNamespace(targetNamespace, policy.Name, processedPolicy, cacheInfo)
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

func (m *externalPolicyManager) aggregateNamespaceInfo(namespaceName string, policies sets.Set[string]) (gatewayInfoList, map[ktypes.NamespacedName]*gatewayInfo, error) {

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
		static = static.Insert(processedPolicy.staticGateways...)
		for podName, gatewayInfo := range processedPolicy.dynamicGateways {
			dynamic[podName] = gatewayInfo
		}
	}
	return static, dynamic, nil
}
