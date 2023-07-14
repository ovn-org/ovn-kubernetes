package apbroute

import (
	"fmt"
	"strings"
	"sync"

	adminpolicybasedrouteapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1"
	adminpolicybasedroutelisters "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1/apis/listers/adminpolicybasedroute/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/controller/apbroute/gateway_info"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/syncmap"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"
)

func insertSet(s1, s2 sets.Set[string]) {
	for insertItem := range s2 {
		s1.Insert(insertItem)
	}
}

type podInfo struct {
	StaticGateways  *gateway_info.GatewayInfoList
	DynamicGateways *gateway_info.GatewayInfoList
}

func newPodInfo() *podInfo {
	return &podInfo{
		DynamicGateways: gateway_info.NewGatewayInfoList(),
		StaticGateways:  gateway_info.NewGatewayInfoList(),
	}
}

type ExternalRouteInfo struct {
	sync.Mutex
	Deleted bool
	PodName ktypes.NamespacedName
	// PodExternalRoutes is a cache keeping the LR routes added to the GRs when
	// external gateways are used. The first map key is the podIP (src-ip of the route),
	// the second the GW IP (next hop), and the third the GR name
	PodExternalRoutes map[string]map[string]string
}

// routePolicyState contains current policy state as it was applied.
// Since every config is applied to a pod, podInfo stores current state for every target pod.
type routePolicyState struct {
	// namespaceName: podName: configured gateways for the pod
	targetNamespaces map[string]map[ktypes.NamespacedName]*podInfo
}

func newRoutePolicyState() *routePolicyState {
	return &routePolicyState{
		targetNamespaces: map[string]map[ktypes.NamespacedName]*podInfo{},
	}
}

// Equal compares StaticGateways and DynamicGateways elements for every namespace to be exactly the same,
// including applied status
func (rp *routePolicyState) Equal(rp2 *routePolicyState) bool {
	for nsName, nsInfo := range rp.targetNamespaces {
		nsInfo2, found := rp2.targetNamespaces[nsName]
		if !found {
			return false
		}
		for podName, podInfo := range nsInfo {
			podInfo2, found := nsInfo2[podName]
			if !found {
				return false
			}
			if !podInfo.StaticGateways.Equal(podInfo2.StaticGateways) ||
				!podInfo.DynamicGateways.Equal(podInfo2.DynamicGateways) {
				return false
			}
		}
	}
	return true
}

func (rp *routePolicyState) String() string {
	s := strings.Builder{}
	s.WriteString("{")
	for nsName, nsInfo := range rp.targetNamespaces {
		s.WriteString(fmt.Sprintf("%s: map[", nsName))
		for podName, podInfo := range nsInfo {
			s.WriteString(fmt.Sprintf("%s: [StaticGateways: {%s}, DynamicGateways: {%s}],", podName, podInfo.StaticGateways.String(),
				podInfo.DynamicGateways.String()))
		}
		s.WriteString("],")
	}
	s.WriteString("}")
	return s.String()
}

// routePolicyConfig is used to update policy to the latest state, it stores all required information for an
// update.
type routePolicyConfig struct {
	policyName string
	// targetNamespacesWithPods[namespaceName[podNamespacedName] = Pod
	targetNamespacesWithPods map[string]map[ktypes.NamespacedName]*v1.Pod
	// staticGateways contains the processed list of IPs and BFD information defined in the staticHop slice in the policy.
	staticGateways *gateway_info.GatewayInfoList
	// dynamicGateways contains the processed list of IPs and BFD information defined in the dynamicHop slice in the policy.
	dynamicGateways *gateway_info.GatewayInfoList
}

type externalPolicyManager struct {
	stopCh <-chan struct{}
	// route policies
	routeLister adminpolicybasedroutelisters.AdminPolicyBasedExternalRouteLister
	// Pods
	podLister corev1listers.PodLister
	// Namespaces
	namespaceLister corev1listers.NamespaceLister
	// policyReferencedObjects should only be accessed with policyReferencedObjectsLock
	policyReferencedObjectsLock sync.RWMutex
	// policyReferencedObjects is a cache of objects every policy has selected for its config.
	// With this cache namespace and pod handlers may fetch affected policies for cleanup.
	// key is policyName.
	policyReferencedObjects map[string]*policyReferencedObjects
	// routePolicySyncCache is a cache of configures states for policies, key is policyName.
	routePolicySyncCache *syncmap.SyncMap[*routePolicyState]
	// networkClient is an interface that exposes add and delete GW IPs. There are 2 structs that implement this contract: one to interface with the north bound DB and another one for the conntrack.
	// the north bound is used by the master controller to add and delete the logical static routes, whilst the conntrack is used by the node controller to ensure that the ECMP entries are removed
	// when a gateway IP is no longer an egress access point.
	netClient networkClient
}

type policyReferencedObjects struct {
	targetNamespaces    sets.Set[string]
	dynamicGWNamespaces sets.Set[string]
	dynamicGWPods       sets.Set[ktypes.NamespacedName]
}

func newExternalPolicyManager(
	stopCh <-chan struct{},
	podLister corev1listers.PodLister,
	namespaceLister corev1listers.NamespaceLister,
	routeLister adminpolicybasedroutelisters.AdminPolicyBasedExternalRouteLister,
	netClient networkClient) *externalPolicyManager {

	m := externalPolicyManager{
		stopCh:                      stopCh,
		routeLister:                 routeLister,
		podLister:                   podLister,
		namespaceLister:             namespaceLister,
		policyReferencedObjectsLock: sync.RWMutex{},
		policyReferencedObjects:     map[string]*policyReferencedObjects{},
		routePolicySyncCache:        syncmap.NewSyncMap[*routePolicyState](),
		netClient:                   netClient,
	}

	return &m
}

// getPoliciesForNamespaceChange returns a list of policies that should be reconciled because of a given namespace update.
// It consists of 2 stages:
// 1. find policies that select given namespace now and may need update
// 2. find policies that selected given namespace before and may need cleanup
// Step 1 is done by fetching the latest AdminPolicyBasedExternalRoute and checking if selectors match.
// Step 2 is done via policyReferencedObjects, which is a cache of the objects every policy selected last time.
func (m *externalPolicyManager) getPoliciesForNamespaceChange(namespace *v1.Namespace) (sets.Set[string], error) {
	policyNames := sets.Set[string]{}
	// first check which policies currently match given namespace.
	// This should work when namespace is added, or starts matching a label selector
	informerPolicies, err := m.getAllRoutePolicies()
	if err != nil {
		return nil, err
	}

	for _, informerPolicy := range informerPolicies {
		targetNsSel, _ := metav1.LabelSelectorAsSelector(&informerPolicy.Spec.From.NamespaceSelector)
		if targetNsSel.Matches(labels.Set(namespace.Labels)) {
			policyNames.Insert(informerPolicy.Name)
			continue
		}

		for _, hop := range informerPolicy.Spec.NextHops.DynamicHops {
			// if NamespaceSelector is not set, it means all namespaces
			gwNsSel := labels.Everything()
			if hop.NamespaceSelector != nil {
				gwNsSel, _ = metav1.LabelSelectorAsSelector(hop.NamespaceSelector)
			}
			if gwNsSel.Matches(labels.Set(namespace.Labels)) {
				policyNames.Insert(informerPolicy.Name)
			}
		}
	}

	// check which namespaces were referenced by policies before
	m.policyReferencedObjectsLock.RLock()
	defer m.policyReferencedObjectsLock.RUnlock()
	for policyName, policyRefs := range m.policyReferencedObjects {
		if policyRefs.targetNamespaces.Has(namespace.Name) {
			policyNames.Insert(policyName)
			continue
		}
		if policyRefs.dynamicGWNamespaces.Has(namespace.Name) {
			policyNames.Insert(policyName)
		}
	}
	return policyNames, nil
}

// getPoliciesForPodChange returns a list of policies that should be reconciled because of a given pod update.
// It consists of 2 stages:
// 1. find policies that select given pod now and may need update
// 2. find policies that selected given pod before and may need cleanup
// Step 1 is done by fetching the latest AdminPolicyBasedExternalRoute and checking if selectors match.
// Step 2 is done via policyReferencedObjects, which is a cache of the objects every policy selected last time.
func (m *externalPolicyManager) getPoliciesForPodChange(pod *v1.Pod) (sets.Set[string], error) {
	policyNames := sets.Set[string]{}
	// first check which policies currently match given namespace.
	// This should work when namespace is added, or starts matching a label selector
	informerPolicies, err := m.getAllRoutePolicies()
	if err != nil {
		return nil, err
	}
	podNs, err := m.namespaceLister.Get(pod.Namespace)
	if err != nil {
		return nil, err
	}

	for _, informerPolicy := range informerPolicies {
		targetNsSel, _ := metav1.LabelSelectorAsSelector(&informerPolicy.Spec.From.NamespaceSelector)
		if targetNsSel.Matches(labels.Set(podNs.Labels)) {
			policyNames.Insert(informerPolicy.Name)
			continue
		}

		for _, hop := range informerPolicy.Spec.NextHops.DynamicHops {
			// if NamespaceSelector is not set, it means all namespaces
			gwNsSel := labels.Everything()
			if hop.NamespaceSelector != nil {
				gwNsSel, _ = metav1.LabelSelectorAsSelector(hop.NamespaceSelector)
			}
			gwPodSel, _ := metav1.LabelSelectorAsSelector(&hop.PodSelector)

			if gwNsSel.Matches(labels.Set(podNs.Labels)) && gwPodSel.Matches(labels.Set(pod.Labels)) {
				policyNames.Insert(informerPolicy.Name)
			}
		}
	}
	// check which namespaces were referenced by policies before
	m.policyReferencedObjectsLock.RLock()
	defer m.policyReferencedObjectsLock.RUnlock()
	for policyName, policyRefs := range m.policyReferencedObjects {
		// we don't store target pods, because all pods in the target namespace are affected, check namespace
		if policyRefs.targetNamespaces.Has(podNs.Name) {
			policyNames.Insert(policyName)
			continue
		}
		if policyRefs.dynamicGWPods.Has(getPodNamespacedName(pod)) {
			policyNames.Insert(policyName)
		}
	}
	return policyNames, nil
}

func (m *externalPolicyManager) getAllRoutePolicies() ([]*adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute, error) {
	var (
		routePolicies []*adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute
		err           error
	)

	routePolicies, err = m.routeLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("Failed to list Admin Policy Based External Routes:%v", err)
		return nil, err
	}
	return routePolicies, nil
}

// getDynamicGatewayIPsForTargetNamespace is called by the annotation logic to identify if a namespace is managed by an CR.
// Since the call can occur outside the lifecycle of the controller, it cannot rely on the namespace info cache object to have been populated.
// Therefore it has to go through all policies until it identifies one that targets the namespace and retrieve the gateway IPs.
// these IPs are used by the annotation logic to determine which ones to remove from the north bound DB (the ones not included in the list),
// and the ones to keep (the ones that match both the annotation and the CR).
// This logic ensures that both CR and annotations can coexist without duplicating gateway IPs.
func (m *externalPolicyManager) getDynamicGatewayIPsForTargetNamespace(namespaceName string) (sets.Set[string], error) {
	policyGWIPs := sets.New[string]()

	routePolicies, err := m.getAllRoutePolicies()
	if err != nil {
		return nil, err
	}

	for _, routePolicy := range routePolicies {
		targetNamespaces, err := m.listNamespacesBySelector(&routePolicy.Spec.From.NamespaceSelector)
		if err != nil {
			return nil, fmt.Errorf("failed to get APB Policy %s dynamic gateway IPs: failed to list namespaces %v",
				routePolicy.Name, err)
		}
		for _, targetNS := range targetNamespaces {
			if targetNS.Name == namespaceName {
				// only collect the dynamic gateways
				dynamicGWInfo, _, _, err := m.processDynamicHopsGatewayInformation(routePolicy.Spec.NextHops.DynamicHops)
				if err != nil {
					return nil, fmt.Errorf("failed to get APB Policy %s dynamic gateway IPs: failed to process dynamic GW %v",
						routePolicy.Name, err)
				}
				for _, gwInfo := range dynamicGWInfo.Elems() {
					insertSet(policyGWIPs, gwInfo.Gateways)
				}
			}
		}
	}
	return policyGWIPs, nil
}

// getStaticGatewayIPsForTargetNamespace is called by the annotation logic to identify if a namespace is managed by an CR.
// Since the call can occur outside the lifecycle of the controller, it cannot rely on the namespace info cache object to have been populated.
// Therefore it has to go through all policies until it identifies one that targets the namespace and retrieve the gateway IPs.
// these IPs are used by the annotation logic to determine which ones to remove from the north bound DB (the ones not included in the list),
// and the ones to keep (the ones that match both the annotation and the CR).
// This logic ensures that both CR and annotations can coexist without duplicating gateway IPs.
func (m *externalPolicyManager) getStaticGatewayIPsForTargetNamespace(namespaceName string) (sets.Set[string], error) {
	policyGWIPs := sets.New[string]()

	routePolicies, err := m.routeLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("Failed to list Admin Policy Based External Routes:%v", err)
		return nil, err
	}
	for _, routePolicy := range routePolicies {
		targetNamespaces, err := m.listNamespacesBySelector(&routePolicy.Spec.From.NamespaceSelector)
		if err != nil {
			klog.Errorf("Failed to process Admin Policy Based External Route %s: %v", routePolicy.Name, err)
			return nil, err
		}
		for _, targetNS := range targetNamespaces {
			if targetNS.Name == namespaceName {
				// only collect the static gateways
				staticGWInfo, err := m.processStaticHopsGatewayInformation(routePolicy.Spec.NextHops.StaticHops)
				if err != nil {
					klog.Errorf("Failed to process Admin Policy Based External Route %s: %v", routePolicy.Name, err)
					return nil, err
				}
				for _, gwInfo := range staticGWInfo.Elems() {
					insertSet(policyGWIPs, gwInfo.Gateways)
				}
			}
		}
	}
	return policyGWIPs, nil
}
