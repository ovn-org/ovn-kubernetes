package apbroute

import (
	"encoding/json"
	"fmt"
	"net"
	"strings"

	nettypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	adminpolicybasedrouteapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
)

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
func (m *externalPolicyManager) processAddPod(newPod *v1.Pod) error {

	// the pod can either be a gateway pod or a standard pod that requires no processing from the external controller.
	// to determine either way, find out which matching dynamic hops include this pod. If none applies, then this is
	// a standard pod and all is needed is to update it's logical routes to include all the external gateways, if they exist.
	podPolicies, err := m.findMatchingDynamicPolicies(newPod)
	if err != nil {
		return err
	}
	if len(podPolicies) > 0 {
		// this is a gateway pod
		klog.Infof("Adding pod gateway %s/%s for policy %+v", newPod.Namespace, newPod.Name, podPolicies)
		return m.applyPodGWPolicies(newPod, podPolicies)
	}
	cacheInfo, found := m.getNamespaceInfoFromCache(newPod.Namespace)
	if !found || (found && cacheInfo.policies.Len() == 0) {
		// this is a standard pod and there are no external gateway policies applicable to the pod's namespace. Nothing to do
		if !found {
			return nil
		}
		m.unlockNamespaceInfoCache(newPod.Namespace)
		return nil
	}
	defer m.unlockNamespaceInfoCache(newPod.Namespace)
	// there are external gateway policies applicable to the pod's namespace.
	klog.Infof("Applying policies to new pod %s/%s %+v", newPod.Namespace, newPod.Name, cacheInfo.policies)
	return m.applyGatewayInfoToPod(newPod, cacheInfo.staticGateways, cacheInfo.dynamicGateways)
}

func (m *externalPolicyManager) applyPodGWPolicies(pod *v1.Pod, externalRoutePolicies []*adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute) error {
	for _, erp := range externalRoutePolicies {
		err := m.applyPodGWPolicy(pod, erp)
		if err != nil {
			return err
		}
	}
	return nil
}

func (m *externalPolicyManager) applyPodGWPolicy(pod *v1.Pod, externalRoutePolicy *adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute) error {
	klog.Infof("Processing policy %s for pod %s/%s", externalRoutePolicy.Name, pod.Namespace, pod.Name)
	routePolicy, err := m.getRoutePolicyForPodGateway(pod, externalRoutePolicy)
	if err != nil {
		return err
	}
	// update all namespaces targeted by this pod's policy to include the new pod IP as their external GW
	err = m.applyProcessedPolicy(externalRoutePolicy.Name, routePolicy)
	if err != nil {
		return err
	}
	gwInfoMap, err := m.aggregateDynamicRouteGatewayInformation(pod, routePolicy)
	if err != nil {
		return err
	}
	key := ktypes.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}
	// update the namespace information for each targeted namespace to reflect the gateway IPs that handle external traffic
	for ns, gwInfo := range gwInfoMap {
		cacheInfo, found := m.getNamespaceInfoFromCache(ns)
		if !found {
			klog.Warningf("Attempting to update the dynamic gateway information for pod %s in a namespace that does not exist %s", pod.Name, ns)
			continue
		}
		// update the gwInfo in the namespace cache
		cacheInfo.dynamicGateways[key] = gwInfo
		m.unlockNamespaceInfoCache(ns)
		if err != nil {
			return err
		}
	}
	return nil
}

func (m *externalPolicyManager) removePodGatewayFromNamespace(nsName string, podNamespacedName ktypes.NamespacedName) error {
	// retrieve the gateway information from the impacted namespace's cache
	cacheInfo, found := m.getNamespaceInfoFromCache(nsName)
	if !found {
		klog.Warningf("Attempting to remove pod gateway %s/%s from a namespace that does not exist %s", podNamespacedName.Namespace, podNamespacedName.Name, nsName)
		return nil
	}
	defer m.unlockNamespaceInfoCache(nsName)

	gateways, found := cacheInfo.dynamicGateways[podNamespacedName]
	if !found {
		klog.Warningf("Pod %s/%s not found in dynamic cacheInfo for namespace %s", podNamespacedName.Namespace, podNamespacedName.Name, nsName)
		return nil
	}
	annotatedGWIPs, err := m.calculateAnnotatedNamespaceGatewayIPsForNamespace(nsName)
	if err != nil {
		return err
	}
	// it is safe to pass the current policies and not to expect the pod IP in the coexisting list of IPs since the pod will no longer match the dynamic hop selectors in any of the policies
	coexistingIPs, err := m.retrieveDynamicGatewayIPsForPolicies(cacheInfo.policies)
	if err != nil {
		return err
	}
	coexistingIPs = coexistingIPs.Union(annotatedGWIPs)
	// Filter out the IPs that are not in coexisting. Those IPs are to be deleted.
	invalidGWIPs := gateways.gws.Difference(coexistingIPs)
	// Filter out the IPs from the coexisting list that are to be kept by calculating the difference between the coexising and those IPs that are to be deleted and not coexisting at the same time.
	ipsToKeep := coexistingIPs.Difference(invalidGWIPs)
	klog.Infof("Coexisting %s, invalid %s, ipsToKeep %s", strings.Join(sets.List(coexistingIPs), ","), strings.Join(sets.List(invalidGWIPs), ","), strings.Join(sets.List(ipsToKeep), ","))
	err = m.netClient.deleteGatewayIPs(nsName, invalidGWIPs, ipsToKeep)
	if err != nil {
		return err
	}
	gateways.gws.Delete(invalidGWIPs.UnsortedList()...)
	if gateways.gws.Len() == 0 {
		// remove pod from namespace cache
		delete(cacheInfo.dynamicGateways, podNamespacedName)
	}
	return nil
}

func (m *externalPolicyManager) addPodGatewayToNamespace(podNamespacedName ktypes.NamespacedName, namespaceName string, processedPolicies []*routePolicy) error {
	// the pod's gatewayInfo is unique to a namespace as the networkName field can differ depending on the policy definition of that field
	// so we retrieve the correct one for the given target namespace from the pre-processed policies. It uses
	// the target namespace and the key (pod_namespace,pod_name) as keys.
	gatewayInfo, err := m.findGatewayInfoForPodInTargetNamespace(podNamespacedName, namespaceName, processedPolicies)
	if err != nil {
		return err
	}
	// use the pod's gatewayInfo to update the logical routes for all the pod's in the target namespace
	err = m.addGWRoutesForNamespace(namespaceName, gatewayInfoList{gatewayInfo})
	if err != nil {
		return err
	}
	cacheInfo, found := m.getNamespaceInfoFromCache(namespaceName)
	defer m.unlockNamespaceInfoCache(namespaceName)
	if !found {
		cacheInfo = m.newNamespaceInfoInCache(namespaceName)
	}
	// add pod gateway information to the namespace cache
	cacheInfo.dynamicGateways[podNamespacedName] = gatewayInfo
	return nil
}

// processUpdatePod takes in an updated gateway pod and the list of old namespaces where the pod was used as egress gateway and proceeds as follows
//   - Finds the matching policies that apply to the pod based on the dynamic hop pod and namespace selectors. If the labels in the pod have not changed, the policies will match to the existing one.
//   - Based on the policies that use the pod IP as gateway, determine the namespaces where the pod IP will be used as egress gateway. If the namespaces match, return without error
//   - Remove the pod IP as egress gateway from the namespaces that are no longer impacted by the pod. This is determined by calculating the difference between the old namespaces and the new ones based on the policies
//     applicable to the updated pod.
//   - Add the pod IP as egress gateway to the namespaces that are now being impacted by the changes in the pod.
func (m *externalPolicyManager) processUpdatePod(updatedPod *v1.Pod, oldTargetNs sets.Set[string]) error {

	// find the policies that apply to this new pod. Unless there are changes to the labels, they should be identical.
	newPodPolicies, err := m.findMatchingDynamicPolicies(updatedPod)
	if err != nil {
		return err
	}
	key := ktypes.NamespacedName{Namespace: updatedPod.Namespace, Name: updatedPod.Name}
	// aggregate the expected target namespaces based on the new pod's labels and current policies
	// if the labels have not changed, the new targeted namespaces and the old ones should be identical
	newTargetNs, err := m.aggregateTargetNamespacesByPolicies(key, newPodPolicies)
	if err != nil {
		return err
	}
	if oldTargetNs.Equal(newTargetNs) {
		// targeting the same namespaces. Nothing to do
		return nil
	}
	// the pods have changed and they don't target the same sets of namespaces, delete its reference on the ones that don't apply
	// and add to the new ones, if necessary
	nsToRemove := oldTargetNs.Difference(newTargetNs)
	nsToAdd := newTargetNs.Difference(oldTargetNs)
	klog.Infof("Removing pod gateway %s/%s from namespace(s): %s", updatedPod.Namespace, updatedPod.Name, strings.Join(sets.List(nsToRemove), ","))
	klog.Infof("Adding pod gateway %s/%s to namespace(s): %s", updatedPod.Namespace, updatedPod.Name, strings.Join(sets.List(nsToAdd), ","))
	// retrieve the gateway information for the pod
	for ns := range nsToRemove {
		err = m.removePodGatewayFromNamespace(ns, ktypes.NamespacedName{Namespace: updatedPod.Namespace, Name: updatedPod.Name})
		if err != nil {
			return err
		}
	}

	// pre-process the policies so we can apply them this process extracts from the CR the contents of the policies
	// into an internal structure that contains the static and dynamic hops information.
	pp, err := m.processExternalRoutePolicies(newPodPolicies)
	if err != nil {
		return err
	}

	for ns := range nsToAdd {
		err = m.addPodGatewayToNamespace(ktypes.NamespacedName{Namespace: updatedPod.Namespace, Name: updatedPod.Name}, ns, pp)
		if err != nil {
			return err
		}
	}

	return nil
}

func (m *externalPolicyManager) aggregateTargetNamespacesByPolicies(podName ktypes.NamespacedName, externalRoutePolicies []*adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute) (sets.Set[string], error) {
	targetNamespaces := sets.New[string]()
	for _, erp := range externalRoutePolicies {
		namespaces, err := m.listNamespacesBySelector(&erp.Spec.From.NamespaceSelector)
		if err != nil {
			return nil, err
		}
		for _, ns := range namespaces {
			if targetNamespaces.Has(ns.Name) {
				klog.Warningf("External gateway pod %s targets namespace %s more than once", podName.Namespace, podName.Name)
				continue
			}
			targetNamespaces = targetNamespaces.Insert(ns.Name)
		}
	}
	return targetNamespaces, nil
}

func (m *externalPolicyManager) findGatewayInfoForPodInTargetNamespace(key ktypes.NamespacedName, targetNamespace string, processedPolicies []*routePolicy) (*gatewayInfo, error) {
	for _, p := range processedPolicies {
		namespaces, err := m.listNamespacesBySelector(p.targetNamespacesSelector)
		if err != nil {
			return nil, err
		}
		for _, targetNs := range namespaces {
			if targetNs.Name == targetNamespace {
				return p.dynamicGateways[key], nil
			}
		}
	}
	return nil, fmt.Errorf("gateway information for pod %s/%s not found", key.Namespace, key.Name)
}

// processDeletePod removes the gateway IP derived from the pod. The IP is then removed from all the pods found in the namespaces by the
// network client (north bound as logical static route or in conntrack).
func (m *externalPolicyManager) processDeletePod(pod *v1.Pod, namespaces sets.Set[string]) error {
	err := m.deletePodGatewayInNamespaces(pod, namespaces)
	if err != nil {
		return err
	}
	return nil
}

func (m *externalPolicyManager) deletePodGatewayInNamespaces(pod *v1.Pod, targetNamespaces sets.Set[string]) error {

	for nsName := range targetNamespaces {
		err := m.deletePodGatewayInNamespace(pod, nsName)
		if err != nil {
			return err
		}
	}
	return nil
}

func (m *externalPolicyManager) deletePodGatewayInNamespace(pod *v1.Pod, targetNamespace string) error {

	key := ktypes.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}
	cacheInfo, found := m.getNamespaceInfoFromCache(targetNamespace)
	if !found {
		klog.Warningf("Attempting to delete pod gateway %s/%s from a namespace that does not exist %s", pod.Namespace, pod.Name, targetNamespace)
		return nil
	}
	defer m.unlockNamespaceInfoCache(targetNamespace)
	gwInfo, ok := cacheInfo.dynamicGateways[key]
	if !ok {
		return fmt.Errorf("unable to find cached pod %s/%s external gateway information in namespace %s", pod.Namespace, pod.Name, targetNamespace)
	}
	annotatedGWIPs, err := m.calculateAnnotatedNamespaceGatewayIPsForNamespace(targetNamespace)
	if err != nil {
		return err
	}
	coexistingIPs, err := m.retrieveDynamicGatewayIPsForPolicies(cacheInfo.policies)
	if err != nil {
		return err
	}
	coexistingIPs = coexistingIPs.Union(annotatedGWIPs)
	// Filter out the IPs that are not in coexisting. Those IPs are to be deleted.
	invalidGWIPs := gwInfo.gws.Difference(coexistingIPs)
	// Filter out the IPs from the coexisting list that are to be kept by calculating the difference between the coexising and those IPs that are to be deleted and not coexisting at the same time.
	ipsToKeep := coexistingIPs.Difference(invalidGWIPs)
	klog.Infof("Coexisting %s, invalid %s, ipsToKeep %s", strings.Join(sets.List(coexistingIPs), ","), strings.Join(sets.List(invalidGWIPs), ","), strings.Join(sets.List(ipsToKeep), ","))
	err = m.netClient.deleteGatewayIPs(targetNamespace, invalidGWIPs, ipsToKeep)
	if err != nil {
		return err
	}
	gwInfo.gws.Delete(invalidGWIPs.UnsortedList()...)
	if cacheInfo.dynamicGateways[key].gws.Len() == 0 {
		delete(cacheInfo.dynamicGateways, key)
	}
	return nil
}

// processAddPodRoutes applies the policies associated to the pod's namespace to the pod logical route
func (m *externalPolicyManager) applyGatewayInfoToPod(newPod *v1.Pod, static gatewayInfoList, dynamic map[ktypes.NamespacedName]*gatewayInfo) error {
	err := m.netClient.addGatewayIPs(newPod, static)
	if err != nil {
		return err
	}
	for _, egress := range dynamic {
		err := m.netClient.addGatewayIPs(newPod, gatewayInfoList{egress})
		if err != nil {
			return err
		}
	}
	return nil
}

// getRoutePolicyForPodGateway iterates through the dynamic hops of a given external route policy spec to determine the pod's GW information.
// Note that a pod can match multiple policies with different configuration at the same time, with the condition
// that the pod can only target the same namespace once at most. That's a 1-1 pod to namespace match.
func (m *externalPolicyManager) getRoutePolicyForPodGateway(newPod *v1.Pod, externalRoutePolicy *adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute) (*routePolicy, error) {

	key := ktypes.NamespacedName{Namespace: newPod.Namespace, Name: newPod.Name}

	pp, err := m.processExternalRoutePolicy(externalRoutePolicy)
	if err != nil {
		return nil, err
	}
	if _, ok := pp.dynamicGateways[key]; !ok {
		return nil, fmt.Errorf("pod %s not found while processing dynamic hops", key)
	}
	// store only the information needed
	return &routePolicy{
		targetNamespacesSelector: pp.targetNamespacesSelector,
		dynamicGateways:          map[ktypes.NamespacedName]*gatewayInfo{key: pp.dynamicGateways[key]},
	}, nil

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

func (m *externalPolicyManager) filterNamespacesUsingPodGateway(key ktypes.NamespacedName) sets.Set[string] {
	namespaces := sets.New[string]()
	nsList := m.listNamespaceInfoCache()
	for _, namespaceName := range nsList {
		cacheInfo, found := m.getNamespaceInfoFromCache(namespaceName)
		if !found {
			continue
		}
		if _, ok := cacheInfo.dynamicGateways[key]; ok {
			namespaces = namespaces.Insert(namespaceName)
		}
		m.unlockNamespaceInfoCache(namespaceName)
	}
	return namespaces
}

func (m *externalPolicyManager) listPodsInNamespaceWithSelector(namespace string, selector *metav1.LabelSelector) ([]*v1.Pod, error) {

	s, err := metav1.LabelSelectorAsSelector(selector)
	if err != nil {
		return nil, err
	}
	return m.podLister.Pods(namespace).List(s)
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
