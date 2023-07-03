package apbroute

import (
	adminpolicybasedrouteapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
)

func (m *externalPolicyManager) syncNamespace(namespace *v1.Namespace, routeQueue workqueue.RateLimitingInterface) error {
	klog.V(5).Infof("APB processing namespace: %s", namespace)

	keysToBeQueued := sets.New[string]()

	// Get a copy of all policies at this time and see if they include this namespace
	policyKeys := m.routePolicySyncCache.GetKeys()

	for _, policyName := range policyKeys {
		err := m.routePolicySyncCache.DoWithLock(policyName,
			func(key string) error {
				ri, found := m.routePolicySyncCache.Load(policyName)
				if found {
					nsSel, _ := metav1.LabelSelectorAsSelector(&ri.policy.Spec.From.NamespaceSelector)
					if nsSel.Matches(labels.Set(namespace.Labels)) {
						keysToBeQueued.Insert(policyName)
						return nil
					}
					for _, hop := range ri.policy.Spec.NextHops.DynamicHops {
						gwNs, err := m.listNamespacesBySelector(hop.NamespaceSelector)
						if err != nil {
							return err
						}
						for _, ns := range gwNs {
							if ns.Name == namespace.Name {
								keysToBeQueued.Insert(policyName)
								return nil
							}
						}
					}
				}
				return nil
			})
		if err != nil {
			return err
		}
	}

	// Check if this namespace is being tracked by policy in its namespace cache
	cacheInfo, found := m.getNamespaceInfoFromCache(namespace.Name)
	if found {
		for policyName := range cacheInfo.Policies {
			keysToBeQueued.Insert(policyName)
		}
		m.unlockNamespaceInfoCache(namespace.Name)
	}

	for policyKey := range keysToBeQueued {
		klog.V(5).Infof("APB queuing policy: %s for namespace: %s", policyKey, namespace)
		routeQueue.Add(policyKey)
	}

	return nil
}

// Must be called with lock on namespaceInfo cache
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

// Must be called with lock on namespaceInfo cache
func (m *externalPolicyManager) removePolicyFromNamespace(targetNamespace string, policy *adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute, cacheInfo *namespaceInfo) error {
	processedPolicy, err := m.processExternalRoutePolicy(policy)
	if err != nil {
		return err
	}
	err = m.deletePolicyInNamespace(targetNamespace, policy.Name, processedPolicy, cacheInfo)
	if err != nil {
		return err
	}

	klog.V(4).Infof("Deleting APB policy %s in namespace cache %s", policy.Name, targetNamespace)
	cacheInfo.Policies = cacheInfo.Policies.Delete(policy.Name)
	if len(cacheInfo.Policies) == 0 {
		m.namespaceInfoSyncCache.Delete(targetNamespace)
	}

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
