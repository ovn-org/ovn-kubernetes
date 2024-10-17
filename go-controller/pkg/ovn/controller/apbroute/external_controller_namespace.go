package apbroute

import (
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
)

func (m *externalPolicyManager) syncNamespace(namespace *v1.Namespace, routeQueue workqueue.TypedRateLimitingInterface[string]) error {
	policyKeys, err := m.getPoliciesForNamespaceChange(namespace)
	if err != nil {
		return err
	}

	klog.V(5).Infof("APB queuing policies: %v for namespace: %s", policyKeys, namespace.Name)
	for policyKey := range policyKeys {
		routeQueue.Add(policyKey)
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
