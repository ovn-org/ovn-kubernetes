package apbroute

import (
	"encoding/json"
	"fmt"
	"net"

	nettypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	adminpolicybasedrouteapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
)

func (m *externalPolicyManager) syncPod(pod *v1.Pod, podLister corev1listers.PodLister, routeQueue workqueue.RateLimitingInterface) error {

	_, err := podLister.Pods(pod.Namespace).Get(pod.Name)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}

	// Only queues policies affected by gateway pods
	policies, pErr := m.listPoliciesInNamespacesUsingPodGateway(ktypes.NamespacedName{Namespace: pod.Namespace, Name: pod.Name})
	if pErr != nil {
		return pErr
	}
	if policies.Len() == 0 {
		// this is not a gateway pod
		cacheInfo, found := m.getNamespaceInfoFromCache(pod.Namespace)
		if found {
			// its namespace is a target for policies.
			policies = policies.Union(cacheInfo.Policies)
			m.unlockNamespaceInfoCache(pod.Namespace)
		}
	}
	klog.V(4).InfoS("Processing gateway pod %s/%s with matching policies %+v", pod.Namespace, pod.Name, policies.UnsortedList())
	for policyName := range policies {
		klog.V(5).InfoS("Queueing policy %s", policyName)
		routeQueue.Add(policyName)
	}

	return nil
}

func getExGwPodIPs(gatewayPod *v1.Pod, networkName string) (sets.Set[string], error) {
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

func getPodIPs(pod *v1.Pod) sets.Set[string] {
	foundGws := sets.New[string]()
	for _, podIP := range pod.Status.PodIPs {
		ip := utilnet.ParseIPSloppy(podIP.IP)
		if ip != nil {
			foundGws.Insert(ip.String())
		}
	}
	return foundGws
}

func getMultusIPsFromNetworkName(pod *v1.Pod, networkName string) (sets.Set[string], error) {
	foundGws := sets.New[string]()
	var multusNetworks []nettypes.NetworkStatus
	err := json.Unmarshal([]byte(pod.ObjectMeta.Annotations[nettypes.NetworkStatusAnnot]), &multusNetworks)
	if err != nil {
		return nil, fmt.Errorf("unable to unmarshall annotation on pod %s k8s.v1.cni.cncf.io/network-status '%s': %v",
			pod.Name, pod.ObjectMeta.Annotations[nettypes.NetworkStatusAnnot], err)
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

func (m *externalPolicyManager) listPoliciesUsingPodGateway(key ktypes.NamespacedName) ([]*adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute, error) {

	ret := make([]*adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute, 0)
	policies, err := m.routeLister.List(labels.Everything())
	if err != nil {
		return nil, err
	}
	for _, p := range policies {
		klog.V(5).InfoS("Checking for policy %s to have pod %s", p.Name, key)
		pp, err := m.processExternalRoutePolicy(p)
		if err != nil {
			return nil, err
		}
		if _, found := pp.dynamicGateways[key]; found {
			klog.V(5).InfoS("Policy %s has pod %s", p.Name, key)
			ret = append(ret, p)
		}

	}
	return ret, nil
}

func (m *externalPolicyManager) listPoliciesInNamespacesUsingPodGateway(key ktypes.NamespacedName) (sets.Set[string], error) {
	policies := sets.New[string]()
	// Iterate through all current namespaces that contain the pod. This is needed in case the pod is deleted from an existing namespace, in which case
	// if we iterated applying the namespace selector in the policies, we would miss the fact that a pod was part of a namespace that is no longer
	// and we'd miss updating that namespace and removing the pod through the reconciliation of the policy in that namespace.
	nsList := m.listNamespaceInfoCache()
	for _, namespaceName := range nsList {
		cacheInfo, found := m.getNamespaceInfoFromCache(namespaceName)
		if !found {
			continue
		}
		if _, ok := cacheInfo.DynamicGateways[key]; ok {
			policies = policies.Union(cacheInfo.Policies)
		}
		m.unlockNamespaceInfoCache(namespaceName)
	}
	// List all namespaces that match the policy, for those new namespaces where the pod now applies
	p, err := m.listPoliciesUsingPodGateway(key)
	if err != nil {
		return nil, err
	}
	for _, policy := range p {
		policies.Insert(policy.Name)
	}
	return policies, nil
}
