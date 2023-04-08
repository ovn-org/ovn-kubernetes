package apbroute

import (
	"fmt"
	"net"
	"reflect"
	"strings"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	ktypes "k8s.io/apimachinery/pkg/types"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"

	adminpolicybasedrouteapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

func (m *externalPolicyManager) processAddPolicy(routePolicy *adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute) (*routePolicy, error) {

	// it's a new policy
	processedPolicies, err := m.processExternalRoutePolicy(routePolicy)
	if err != nil {
		return nil, err
	}
	err = m.applyProcessedPolicy(routePolicy.Name, processedPolicies)
	if err != nil {
		return nil, err
	}
	m.updateRoutePolicyInCache(routePolicy)
	klog.Infof("Added Admin Policy Based External Route %s", routePolicy.Name)
	return processedPolicies, nil
}

func (m *externalPolicyManager) applyProcessedPolicy(policyName string, routePolicy *routePolicy) error {
	targetNs, err := m.listNamespacesBySelector(routePolicy.labelSelector)
	if err != nil {
		return err
	}
	for _, ns := range targetNs {
		cacheInfo, found := m.getNamespaceInfoFromCache(ns.Name)
		if !found {
			cacheInfo = m.newNamespaceInfoInCache(ns.Name)
		}
		err = m.applyProcessedPolicyToNamespace(ns.Name, policyName, routePolicy, cacheInfo)
		m.unlockNamespaceInfoCache(ns.Name)
		if err != nil {
			return err
		}
	}
	return nil
}

func (m *externalPolicyManager) processDeletePolicy(routePolicy *adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute) error {
	targetNs, err := m.listNamespacesBySelector(&routePolicy.Spec.From.NamespaceSelector)
	if err != nil {
		return err
	}
	for _, ns := range targetNs {
		cacheInfo, found := m.getNamespaceInfoFromCache(ns.Name)
		if !found {
			klog.Warningf("Attempting to delete policy %s from a namespace that does not exist %s", routePolicy.Name, ns.Name)
			continue
		}
		err = m.removePolicyFromNamespace(ns.Name, routePolicy, cacheInfo)
		if err != nil {
			m.unlockNamespaceInfoCache(ns.Name)
			return err
		}
		if cacheInfo.policies.Len() == 0 {
			m.deleteNamespaceInfoInCache(ns.Name)
		}
		m.unlockNamespaceInfoCache(ns.Name)
	}
	m.deleteRoutePolicyFromCache(routePolicy.Name)
	klog.Infof("Deleted Admin Policy Based External Route %s", routePolicy.Name)
	return nil
}

func (m *externalPolicyManager) calculateAnnotatedNamespaceGatewayIPsForNamespace(targetNamespace string) (sets.Set[string], error) {
	namespace, err := m.namespaceLister.Get(targetNamespace)
	if err != nil {
		return nil, err
	}

	if annotation, ok := namespace.Annotations[util.RoutingExternalGWsAnnotation]; ok {
		exGateways, err := util.ParseRoutingExternalGWAnnotation(annotation)
		if err != nil {
			return nil, err
		}
		return exGateways, nil
	}
	return sets.New[string](), nil

}

func (m *externalPolicyManager) calculateAnnotatedPodGatewayIPsForNamespace(targetNamespace string) (sets.Set[string], error) {
	gwIPs := sets.New[string]()
	podList, err := m.podLister.List(labels.Everything())
	if err != nil {
		return nil, err
	}

	for _, pod := range podList {
		networkName, ok := pod.Annotations[util.RoutingNetworkAnnotation]
		if !ok {
			continue
		}
		targetNamespaces, ok := pod.Annotations[util.RoutingNamespaceAnnotation]
		if !ok {
			continue
		}
		foundGws, err := getExGwPodIPs(pod, networkName)
		if err != nil {
			klog.Errorf("Error getting exgw IPs for pod: %s, error: %v", pod.Name, err)
			return nil, err
		}
		if foundGws.Len() == 0 {
			klog.Errorf("No pod IPs found for pod %s/%s", pod.Namespace, pod.Name)
			continue
		}
		tmpNs := sets.New(strings.Split(targetNamespaces, ",")...)
		if tmpNs.Has(targetNamespaces) {
			gwIPs = gwIPs.Union(foundGws)
		}
	}
	return gwIPs, nil
}

func (m *externalPolicyManager) deleteProcessedPolicyInNamespace(namespaceName, policyName string, routePolicy *routePolicy, cacheInfo *namespaceInfo) error {
	coexistingPolicies := cacheInfo.policies.Clone().Delete(policyName)
	annotatedGWIPs, err := m.calculateAnnotatedNamespaceGatewayIPsForNamespace(namespaceName)
	if err != nil {
		return err
	}
	coexistingIPs, err := m.retrieveCoexistingStaticGatewayIPsToPoliciesInNamespace(namespaceName, coexistingPolicies)
	if err != nil {
		return err
	}
	for _, gwInfo := range routePolicy.staticGateways {
		invalidGWIPs := gwInfo.gws.Difference(annotatedGWIPs.Union(coexistingIPs))
		err := m.netClient.deleteGatewayIPs(namespaceName, invalidGWIPs)
		if err != nil {
			return err
		}
		if gwInfo.gws.Equal(invalidGWIPs) {
			cacheInfo.staticGateways = cacheInfo.staticGateways.Delete(gwInfo)
			continue
		}
		gwInfo.gws = gwInfo.gws.Delete(invalidGWIPs.UnsortedList()...)
	}

	annotatedGWIPs, err = m.calculateAnnotatedPodGatewayIPsForNamespace(namespaceName)
	if err != nil {
		return err
	}

	coexistingIPs, err = m.retrieveCoexistingDynamicGatewayIPsToPoliciesInNamespace(namespaceName, coexistingPolicies)
	if err != nil {
		return err
	}
	for pod, gwInfo := range routePolicy.dynamicGateways {
		invalidGWIPs := gwInfo.gws.Difference(annotatedGWIPs.Union(coexistingIPs))
		err := m.netClient.deleteGatewayIPs(namespaceName, invalidGWIPs)
		if err != nil {
			return err
		}
		if gwInfo.gws.Equal(invalidGWIPs) {
			// delete cached information for the pod gateway
			delete(cacheInfo.dynamicGateways, pod)
			continue
		}
		gwInfo.gws = gwInfo.gws.Delete(invalidGWIPs.UnsortedList()...)
	}
	return nil
}

func (m *externalPolicyManager) applyProcessedPolicyToNamespace(namespaceName, policyName string, routePolicy *routePolicy, cacheInfo *namespaceInfo) error {

	if routePolicy.staticGateways.Len() > 0 {
		err := m.addGWRoutesForNamespace(namespaceName, routePolicy.staticGateways)
		if err != nil {
			return err
		}
		cacheInfo.staticGateways = cacheInfo.staticGateways.Insert(routePolicy.staticGateways...)
	}
	for pod, info := range routePolicy.dynamicGateways {
		err := m.addGWRoutesForNamespace(namespaceName, gatewayInfoList{info})
		if err != nil {
			return err
		}
		cacheInfo.dynamicGateways[pod] = info
	}
	cacheInfo.policies = cacheInfo.policies.Insert(policyName)
	return nil
}

func (m *externalPolicyManager) processUpdatePolicy(currentPolicy, updatedPolicy *adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute) (*routePolicy, error) {
	klog.Infof("Processing update for Admin Policy Based External Route '%s'", currentPolicy.Name)

	//To update the policies, first we'll process the diff between old and new and remove the discrepancies that are not found in the new object.
	//Afterwards, we'll process the diff between the new and the old and apply the new policies not found in the old policy, ensuring that we are no reduplicating the gatewayInfo.
	//The last step is to update the Status information in the new CR to reflect the status of the actions performed
	err := m.removeDiscrepanciesInRoutePolicy(currentPolicy, updatedPolicy)
	if err != nil {
		return nil, err
	}
	// At this point we have removed all the aspects of the current policy that no longer applies. Next step is to apply the parts of the new policy that are not in the current one.
	err = m.applyDiscrepanciesInRoutePolicy(currentPolicy, updatedPolicy)
	if err != nil {
		return nil, err
	}

	// update the cache to ensure it reflects the latest copy
	m.updateRoutePolicyInCache(updatedPolicy)
	klog.Infof("Updated Admin Policy Based External Route %s", currentPolicy.Name)
	return m.processExternalRoutePolicy(updatedPolicy)
}

func (m *externalPolicyManager) applyDiscrepanciesInRoutePolicy(currentPolicy, newPolicy *adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute) error {
	additionalNamespaces, additionalStaticHops, additionalDynamicHops, err := m.calculatePolicyDifferences(newPolicy, currentPolicy)
	if err != nil {
		return err
	}
	// apply the new policy to the new namespaces where the policy now applies
	for additionalNs := range additionalNamespaces {
		cacheInfo, found := m.getNamespaceInfoFromCache(additionalNs)
		if !found {
			// if not found create a new one
			cacheInfo = m.newNamespaceInfoInCache(additionalNs)
		}
		err := m.applyPolicyToNamespace(additionalNs, newPolicy, cacheInfo)
		m.unlockNamespaceInfoCache(additionalNs)
		if err != nil {
			return err
		}
	}

	processedStaticHops, err := m.processStaticHopsGatewayInformation(additionalStaticHops)
	if err != nil {
		return err
	}
	processedDynamicHops, err := m.processDynamicHopsGatewayInformation(additionalDynamicHops)
	if err != nil {
		return err
	}
	// retrieve all new namespaces
	nsList, err := m.listNamespacesBySelector(&newPolicy.Spec.From.NamespaceSelector)
	if err != nil {
		return err
	}
	for _, ns := range nsList {
		if additionalNamespaces.Has(ns.Name) {
			// policy has already been fully applied to this namespace by the previous operation
			continue
		}
		cacheInfo, found := m.getNamespaceInfoFromCache(ns.Name)
		if !found {
			cacheInfo = m.newNamespaceInfoInCache(ns.Name)
		}
		err = m.applyProcessedPolicyToNamespace(ns.Name, currentPolicy.Name, &routePolicy{dynamicGateways: processedDynamicHops, staticGateways: processedStaticHops}, cacheInfo)
		m.unlockNamespaceInfoCache(ns.Name)
		if err != nil {
			return err
		}
	}

	return nil
}

func (m *externalPolicyManager) removeDiscrepanciesInRoutePolicy(currentPolicy, updatedPolicy *adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute) error {
	unmachingNamespaces, unmatchingStaticHops, unmatchingDynamicHops, err := m.calculatePolicyDifferences(currentPolicy, updatedPolicy)
	if err != nil {
		return err
	}
	// delete the namespaces where this policy no longer applies
	for unmatchNs := range unmachingNamespaces {
		cacheInfo, found := m.getNamespaceInfoFromCache(unmatchNs)
		if !found {
			klog.Warningf("Attempting to delete policy %s from a namespace that does not exist %s", currentPolicy.Name, unmatchNs)
			continue
		}
		err := m.removePolicyFromNamespace(unmatchNs, currentPolicy, cacheInfo)
		if err != nil {
			m.unlockNamespaceInfoCache(unmatchNs)
			return err
		}
		if cacheInfo.policies.Len() == 0 {
			m.deleteNamespaceInfoInCache(unmatchNs)
		}
		m.unlockNamespaceInfoCache(unmatchNs)
	}

	// delete the hops that no longer apply from all the current policy's applicable namespaces
	processedStaticHops, err := m.processStaticHopsGatewayInformation(unmatchingStaticHops)
	if err != nil {
		return err
	}
	processedDynamicHops, err := m.processDynamicHopsGatewayInformation(unmatchingDynamicHops)
	if err != nil {
		return err
	}
	// retrieve all current namespaces
	nsList, err := m.listNamespacesBySelector(&currentPolicy.Spec.From.NamespaceSelector)
	if err != nil {
		return err
	}
	for _, ns := range nsList {
		if unmachingNamespaces.Has(ns.Name) {
			// policy has already been deleted in this namespace by the previous operation
			continue
		}
		cacheInfo, found := m.getNamespaceInfoFromCache(ns.Name)
		if !found {
			klog.Warningf("Attempting to update policy %s for a namespace that does not exist %s", currentPolicy.Name, ns.Name)
			continue
		}
		err = m.deleteProcessedPolicyInNamespace(ns.Name, currentPolicy.Name, &routePolicy{dynamicGateways: processedDynamicHops, staticGateways: processedStaticHops}, cacheInfo)
		if err != nil {
			m.unlockNamespaceInfoCache(ns.Name)
			return err
		}
		if cacheInfo.policies.Len() == 0 {
			m.deleteNamespaceInfoInCache(ns.Name)
		}
		m.unlockNamespaceInfoCache(ns.Name)
	}
	return nil
}

// addGWRoutesForNamespace handles adding routes for all existing pods in namespace
func (m *externalPolicyManager) addGWRoutesForNamespace(namespace string, egress gatewayInfoList) error {
	existingPods, err := m.podLister.Pods(namespace).List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to get all the pods (%v)", err)
	}
	for _, pod := range existingPods {
		err := m.netClient.addGatewayIPs(pod, egress)
		if err != nil {
			return err
		}
	}
	return nil
}

func (m *externalPolicyManager) processStaticHopsGatewayInformation(hops []*adminpolicybasedrouteapi.StaticHop) (gatewayInfoList, error) {
	gwList := gatewayInfoList{}

	// collect all the static gateway information from the nextHops slice
	for _, h := range hops {
		ip := net.ParseIP(h.IP)
		if ip == nil {
			return nil, fmt.Errorf("could not parse routing external gw annotation value '%s'", h.IP)
		}
		gwList = append(gwList, &gatewayInfo{gws: sets.New(ip.String()), bfdEnabled: h.BFDEnabled})
	}
	return gwList, nil
}

func (m *externalPolicyManager) processDynamicHopsGatewayInformation(hops []*adminpolicybasedrouteapi.DynamicHop) (map[ktypes.NamespacedName]*gatewayInfo, error) {
	podsInfo := map[ktypes.NamespacedName]*gatewayInfo{}
	for _, h := range hops {
		podNS, err := m.listNamespacesBySelector(h.NamespaceSelector)
		if err != nil {
			return nil, err
		}
		for _, ns := range podNS {
			s, err := metav1.LabelSelectorAsSelector(&h.PodSelector)
			if err != nil {
				return nil, err
			}
			pods, err := m.podLister.Pods(ns.Name).List(s)
			if err != nil {
				return nil, err
			}
			for _, pod := range pods {
				foundGws, err := getExGwPodIPs(pod, h.NetworkAttachmentName)
				if err != nil {
					return nil, err
				}
				// if we found any gateways then we need to update current pods routing in the relevant namespace
				if len(foundGws) == 0 {
					klog.Warningf("No valid gateway IPs found for requested external gateway pod %s/%s", pod.Namespace, pod.Name)
					continue
				}
				key := ktypes.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}
				if _, ok := podsInfo[key]; ok {
					klog.Warningf("Found overlapping dynamic hop policy for pod %s, discarding match entry", key)
					continue
				}
				podsInfo[key] = &gatewayInfo{gws: foundGws, bfdEnabled: h.BFDEnabled}
			}
		}
	}
	return podsInfo, nil
}

func (m *externalPolicyManager) processExternalRoutePolicy(policy *adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute) (*routePolicy, error) {
	var (
		errors []error
	)
	staticGWInfo, err := m.processStaticHopsGatewayInformation(policy.Spec.NextHops.StaticHops)
	if err != nil {
		errors = append(errors, err)
	}

	dynamicGWInfo, err := m.processDynamicHopsGatewayInformation(policy.Spec.NextHops.DynamicHops)
	if err != nil {
		errors = append(errors, err)
	}
	if len(errors) > 0 {
		return nil, kerrors.NewAggregate(errors)
	}
	return &routePolicy{
		labelSelector:   &policy.Spec.From.NamespaceSelector,
		staticGateways:  staticGWInfo,
		dynamicGateways: dynamicGWInfo,
	}, nil

}

func (m *externalPolicyManager) processExternalRoutePolicies(externalRoutePolicies []*adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute) ([]*routePolicy, error) {
	routePolicies := make([]*routePolicy, 0)
	for _, erp := range externalRoutePolicies {
		processedPolicies, err := m.processExternalRoutePolicy(erp)
		if err != nil {
			return nil, err
		}
		routePolicies = append(routePolicies, processedPolicies)
	}
	return routePolicies, nil
}

func (m *externalPolicyManager) findMatchingDynamicPolicies(pod *v1.Pod) ([]*adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute, error) {
	var routePolicies []*adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute
	crs, err := m.routeLister.List(labels.Everything())
	if err != nil {
		return nil, err
	}
	for _, cr := range crs {
		policySpec := adminpolicybasedrouteapi.AdminPolicyBasedExternalRouteSpec{
			From:     cr.Spec.From,
			NextHops: adminpolicybasedrouteapi.ExternalNextHops{DynamicHops: []*adminpolicybasedrouteapi.DynamicHop{}}}
		for _, dp := range cr.Spec.NextHops.DynamicHops {
			nss, err := m.listNamespacesBySelector(dp.NamespaceSelector)
			if err != nil {
				return nil, err
			}
			if !containsNamespaceInSlice(nss, pod.Namespace) {
				continue
			}
			nsPods, err := m.listPodsInNamespaceWithSelector(pod.Namespace, &dp.PodSelector)
			if err != nil {
				return nil, err
			}
			if containsPodInSlice(nsPods, pod.Name) {
				// add only the hop information that intersects with the pod
				policySpec.NextHops.DynamicHops = append(policySpec.NextHops.DynamicHops, dp)
			}
		}
		if len(policySpec.NextHops.DynamicHops) > 0 {
			routePolicies = append(routePolicies, &adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute{
				ObjectMeta: metav1.ObjectMeta{
					Name: cr.Name,
				},
				Spec: policySpec,
			})
		}

	}
	return routePolicies, nil
}

func (m *externalPolicyManager) getPoliciesForNamespace(namespaceName string) (sets.Set[string], error) {
	matches := sets.New[string]()
	policies, err := m.routeLister.List(labels.Everything())
	if err != nil {
		return nil, err
	}

	for _, policy := range policies {
		targetNamespaces, err := m.listNamespacesBySelector(&policy.Spec.From.NamespaceSelector)
		if err != nil {
			return nil, err
		}
		for _, ns := range targetNamespaces {
			if namespaceName == ns.Name {
				matches = matches.Insert(policy.Name)
			}
		}
	}

	return matches, nil
}

func (m *externalPolicyManager) agregateDynamicRouteGatewayInformation(pod *v1.Pod, routePolicy *routePolicy) (map[string]*gatewayInfo, error) {
	key := ktypes.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}
	gwInfoMap := make(map[string]*gatewayInfo)
	targetNs, err := m.listNamespacesBySelector(routePolicy.labelSelector)
	if err != nil {
		return nil, err
	}
	for _, ns := range targetNs {
		if _, ok := gwInfoMap[ns.Name]; ok {
			return nil, fmt.Errorf("duplicated target namespace '%s ' while processing external policies for pod %s/%s", ns.Name, pod.Namespace, pod.Name)
		}
		gwInfoMap[ns.Name] = routePolicy.dynamicGateways[key]
	}
	return gwInfoMap, nil
}

func (m *externalPolicyManager) calculatePolicyDifferences(policy1, policy2 *adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute) (sets.Set[string], []*adminpolicybasedrouteapi.StaticHop, []*adminpolicybasedrouteapi.DynamicHop, error) {
	mismatchingNamespaces, err := m.calculateNamespaceSelectorDifferences(&policy1.Spec.From.NamespaceSelector, &policy2.Spec.From.NamespaceSelector)
	if err != nil {
		return nil, nil, nil, err
	}
	mismatchingStaticHops := m.calculateStaticHopDifferences(policy1.Spec.NextHops.StaticHops, policy2.Spec.NextHops.StaticHops)
	mismatchingDynamicHops, err := m.calculateDynamicHopDifferences(policy1.Spec.NextHops.DynamicHops, policy2.Spec.NextHops.DynamicHops)
	if err != nil {
		return nil, nil, nil, err
	}

	return mismatchingNamespaces, mismatchingStaticHops, mismatchingDynamicHops, nil
}

// calculateNamespaceSelectorDifferences determines the difference between the first and the second selector. The outcome is a set that contains
// those namespace names that are in the first selector but not found in the second selector.
func (m *externalPolicyManager) calculateNamespaceSelectorDifferences(nsSelector1, nsSelector2 *metav1.LabelSelector) (sets.Set[string], error) {
	unmatchingNamespaces := sets.New[string]()
	if !reflect.DeepEqual(nsSelector1, nsSelector2) {
		nsList1, err := m.listNamespacesBySelector(nsSelector1)
		if err != nil {
			return nil, err
		}
		nsList2, err := m.listNamespacesBySelector(nsSelector2)
		if err != nil {
			return nil, err
		}
		for _, ns1 := range nsList1 {
			var found bool
			for _, ns2 := range nsList2 {
				if ns1.Name == ns2.Name {
					found = true
					break
				}
			}
			if !found {
				unmatchingNamespaces.Insert(ns1.Name)
			}
		}
	}
	return unmatchingNamespaces, nil
}

func (m *externalPolicyManager) calculateStaticHopDifferences(staticHops1, staticHops2 []*adminpolicybasedrouteapi.StaticHop) []*adminpolicybasedrouteapi.StaticHop {
	unmatchingStatic := make([]*adminpolicybasedrouteapi.StaticHop, 0)
	for _, staticHop1 := range staticHops1 {
		var found bool
		for _, staticHop2 := range staticHops2 {
			if reflect.DeepEqual(staticHop1, staticHop2) {
				found = true
				break
			}
		}
		if !found {
			unmatchingStatic = append(unmatchingStatic, staticHop1)
		}
	}
	return unmatchingStatic
}
func (m *externalPolicyManager) calculateDynamicHopDifferences(dynamicHops1, dynamicHops2 []*adminpolicybasedrouteapi.DynamicHop) ([]*adminpolicybasedrouteapi.DynamicHop, error) {
	unmatchingDynamic := make([]*adminpolicybasedrouteapi.DynamicHop, 0)
	for _, dynamicHop1 := range dynamicHops1 {
		var found bool
		for _, dynamicHop2 := range dynamicHops2 {

			if reflect.DeepEqual(dynamicHop1, dynamicHop2) {
				found = true
				break
			}
		}
		if !found {
			unmatchingDynamic = append(unmatchingDynamic, dynamicHop1)
		}
	}
	return unmatchingDynamic, nil
}

// retrieveCoexistingDynamicGatewayIPsToPoliciesInNamespace returns a set that contains the gatewayIPs that belong to policies that coexist in a given
// namespace.
func (m *externalPolicyManager) retrieveCoexistingDynamicGatewayIPsToPoliciesInNamespace(namespace string, coexistingPolicies sets.Set[string]) (sets.Set[string], error) {
	coexistingDynamicIPs := sets.New[string]()

	for name := range coexistingPolicies {
		policy, err := m.routeLister.Get(name)
		if err != nil {
			klog.Warningf("Unable to find route policy %s:%+v", name, err)
			continue
		}
		pp, err := m.processDynamicHopsGatewayInformation(policy.Spec.NextHops.DynamicHops)
		if err != nil {
			return nil, err
		}
		for _, gatewayInfo := range pp {
			coexistingDynamicIPs = coexistingDynamicIPs.Union(gatewayInfo.gws)
		}
	}
	return coexistingDynamicIPs, nil
}

// retrieveCoexistingStaticGatewayIPsToPoliciesInNamespace returns a set that contains the gatewayIPs that belong to policies that coexist in a given
// namespace.
func (m *externalPolicyManager) retrieveCoexistingStaticGatewayIPsToPoliciesInNamespace(namespace string, policies sets.Set[string]) (sets.Set[string], error) {
	coexistingStaticIPs := sets.New[string]()

	for name := range policies {
		policy, err := m.routeLister.Get(name)
		if err != nil {
			klog.Warningf("Unable to find route policy %s:%+v", name, err)
			continue
		}
		pp, err := m.processStaticHopsGatewayInformation(policy.Spec.NextHops.StaticHops)
		if err != nil {
			return nil, err
		}
		for _, gatewayInfo := range pp {
			coexistingStaticIPs = coexistingStaticIPs.Union(gatewayInfo.gws)
		}
	}
	return coexistingStaticIPs, nil
}
