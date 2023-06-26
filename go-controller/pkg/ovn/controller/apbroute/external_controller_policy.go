package apbroute

import (
	"fmt"
	"net"
	"reflect"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	ktypes "k8s.io/apimachinery/pkg/types"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	adminpolicybasedrouteapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

func (m *externalPolicyManager) syncRoutePolicy(policyName string, routeQueue workqueue.RateLimitingInterface) (*adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute, error) {
	klog.V(5).InfoS("Processing sync for APB %s", policyName)
	routePolicy, err := m.routeLister.Get(policyName)
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, err
	}
	if apierrors.IsNotFound(err) || !routePolicy.DeletionTimestamp.IsZero() {
		// DELETE use case
		klog.V(4).InfoS("Deleting policy %s", policyName)
		err = m.processDeletePolicy(policyName)
		if err != nil {
			return nil, fmt.Errorf("failed to delete Admin Policy Based External Route %s:%w", policyName, err)
		}
		return nil, nil
	}
	currentPolicy, found, _ := m.getRoutePolicyFromCache(routePolicy.Name)
	if !found {
		// ADD use case
		klog.V(4).InfoS("Adding policy %s", routePolicy.Name)
		err := m.processAddPolicy(routePolicy)
		if err != nil {
			return routePolicy, fmt.Errorf("failed to create Admin Policy Based External Route %s:%w", routePolicy.Name, err)
		}
		return routePolicy, nil
	}

	if reflect.DeepEqual(currentPolicy.Spec, routePolicy.Spec) {
		// Reconcile changes to namespace or pod
		klog.V(5).InfoS("Reconciling policy %s with updates to namespace or pods", routePolicy.Name)
		err := m.reconcilePolicyWithNamespacesAndPods(currentPolicy)
		if err != nil {
			return routePolicy, fmt.Errorf("failed to create Admin Policy Based External Route %s:%w", routePolicy.Name, err)
		}
		return routePolicy, nil
	}
	// UPDATE policy use case
	klog.V(4).InfoS("Updating policy %s", routePolicy.Name)
	err = m.processUpdatePolicy(currentPolicy, routePolicy)
	if err != nil {
		return routePolicy, fmt.Errorf("failed to update Admin Policy Based External Route %s:%w", routePolicy.Name, err)
	}

	return routePolicy, nil
}

// processAddPolicy takes in an existing policy and reconciles it. To do that, it aggregates the IPs from the static hops and retrieves the IPs from the pods resulting from applying the
// namespace and pod selectors in the dynamic hops.
func (m *externalPolicyManager) processAddPolicy(routePolicy *adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute) error {

	// it's a new policy
	processedPolicies, err := m.processExternalRoutePolicy(routePolicy)
	if err != nil {
		return err
	}
	err = m.applyProcessedPolicy(routePolicy.Name, processedPolicies)
	if err != nil {
		return err
	}
	err = m.storeRoutePolicyInCache(routePolicy)
	if err != nil {
		return err
	}
	klog.Infof("Added Admin Policy Based External Route %s", routePolicy.Name)
	return nil
}

// listNamespacesWithPolicy returns a slice containing the names of the namespaces where a given policy is applied to.
func (m *externalPolicyManager) listNamespacesWithPolicy(policyName string) (sets.Set[string], error) {
	nss := m.getAllNamespacesNamesInCache()
	ret := sets.New[string]()
	for _, ns := range nss {
		if m.hasPolicyInNamespace(policyName, ns) {
			ret.Insert(ns)
		}
	}
	return ret, nil
}

func (m *externalPolicyManager) hasPolicyInNamespace(policyName, namespaceName string) bool {

	nsInfo, found := m.getNamespaceInfoFromCache(namespaceName)
	if !found {
		klog.V(5).InfoS("Namespace %s not found while consolidating namespaces using policy %s", namespaceName, policyName)
		return false
	}
	defer m.unlockNamespaceInfoCache(namespaceName)
	return nsInfo.Policies.Has(policyName)
}

// locks the namespace cache and executes discrepancyFunc
func (m *externalPolicyManager) processPolicyDiscrepancyInNamespace(nsName string, routePolicy *adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute,
	discrepancyFunc func(string, *adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute, *namespaceInfo) error) error {
	cacheInfo, found := m.getNamespaceInfoFromCache(nsName)
	if !found {
		klog.V(5).InfoS("Namespace %s not found in cache, creating", nsName)
		cacheInfo = m.newNamespaceInfoInCache(nsName)
	}
	defer m.unlockNamespaceInfoCache(nsName)
	return discrepancyFunc(nsName, routePolicy, cacheInfo)
}

func (m *externalPolicyManager) reconcilePolicyWithNamespacesAndPods(routePolicy *adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute) error {

	nss, err := m.listNamespacesBySelector(&routePolicy.Spec.From.NamespaceSelector)
	if err != nil {
		return err
	}
	targetNs := sets.New[string]()
	for _, ns := range nss {
		targetNs.Insert(ns.Name)
	}

	klog.V(4).InfoS("Reconciling namespaces %+v for policy %s", strings.Join(targetNs.UnsortedList(), ", "), routePolicy.Name)
	// reconcile Namespaces
	currentNs, err := m.listNamespacesWithPolicy(routePolicy.Name)
	if err != nil {
		return err
	}
	klog.V(5).Infof("List of existing namespaces with policy %s: %s", routePolicy.Name, strings.Join(currentNs.UnsortedList(), ", "))
	if len(currentNs.Difference(targetNs)) > 0 {
		klog.V(4).Infof("Removing APB policy %s in namespaces %+v", routePolicy.Name, currentNs.Difference(targetNs))
	}
	// namespaces where the policy should not be applied anymore.
	for nsName := range currentNs.Difference(targetNs) {
		err = m.processPolicyDiscrepancyInNamespace(nsName, routePolicy, m.removePolicyFromNamespace)
		if err != nil {
			return err
		}
	}
	if len(targetNs.Difference(currentNs)) > 0 {
		klog.V(4).Infof("Adding policy %s to namespaces %+v", routePolicy.Name, targetNs.Difference(currentNs))
	}
	// namespaces where the policy should now be applied
	for nsName := range targetNs.Difference(currentNs) {
		err = m.processPolicyDiscrepancyInNamespace(nsName, routePolicy, m.applyPolicyToNamespace)
		if err != nil {
			return err
		}
	}
	klog.V(4).Infof("Reconciling policy %s against matching namespaces %+v", routePolicy.Name, targetNs.Intersection(currentNs))
	// namespaces where the policy still applies. In this case validate the dynamic hops
	for nsName := range targetNs.Intersection(currentNs) {
		err = m.processReconciliationWithNamespace(nsName)
		if err != nil {
			return err
		}
	}
	return nil
}

func (m *externalPolicyManager) processReconciliationWithNamespace(nsName string) error {
	cacheInfo, found := m.getNamespaceInfoFromCache(nsName)
	if !found {
		klog.V(5).InfoS("Namespace %s not found in cache", nsName)
		return nil
	}
	defer m.unlockNamespaceInfoCache(nsName)
	policies := make([]*adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute, 0)
	for policy := range cacheInfo.Policies {
		cachedPolicy, found, markedForDeletion := m.getRoutePolicyFromCache(policy)
		if !found {
			klog.V(5).InfoS("Policy %s not found while calculating all policies in a namespace", policy)
			continue
		}
		if markedForDeletion {
			klog.Warningf("Attempting to add or update route policy %s when it has been marked for deletion. Skipping...", policy)
			continue
		}
		policies = append(policies, cachedPolicy)
	}
	processedPolicies, err := m.processExternalRoutePolicies(policies)
	if err != nil {
		return err
	}
	allProcessedGWIPs := map[ktypes.NamespacedName]*gatewayInfo{}
	// Consolidate all dynamic gateway IPs from the policies into a single map
	for _, pp := range processedPolicies {
		for k, v := range pp.dynamicGateways {
			allProcessedGWIPs[k] = v
		}
	}
	newDynamicGateways, invalidGWIPs, ipsToKeep := m.calculateDynamicGateways(allProcessedGWIPs, cacheInfo.DynamicGateways)
	// Consolidate all static gateway IPs from the policies into a single set
	allStaticGWIPs := make(gatewayInfoList, 0)
	for _, pp := range processedPolicies {
		allStaticGWIPs = append(allStaticGWIPs, pp.staticGateways...)
	}
	newStaticGateways, invalidStaticGWIPs, staticIPsToKeep := m.calculateStaticGateways(allStaticGWIPs, cacheInfo.StaticGateways)

	// delete all invalid GW IP references in the NorthBoundDB (master controller) or conntrack (node_controller)
	// provide all valid IPs, static as well as dynamic, since conntrack is using a white listed approach when deleting entries
	err = m.netClient.deleteGatewayIPs(nsName, invalidGWIPs.Union(invalidStaticGWIPs), ipsToKeep.Union(staticIPsToKeep))
	if err != nil {
		return err
	}

	// proceed to add the dynamic GW IPs
	for _, gatewayInfo := range newDynamicGateways {
		err = m.addGWRoutesForNamespace(nsName, gatewayInfoList{gatewayInfo})
		if err != nil {
			return err
		}
	}
	// add the static GW IPs
	err = m.addGWRoutesForNamespace(nsName, newStaticGateways)
	if err != nil {
		return err
	}
	// Update namespace cacheInfo with the new static and dynamic gateways
	cacheInfo.StaticGateways = newStaticGateways
	cacheInfo.DynamicGateways = newDynamicGateways
	return nil
}

func (m *externalPolicyManager) calculateStaticGateways(allProcessedGWIPs, cachedStaticGWInfo gatewayInfoList) (gatewayInfoList, sets.Set[string], sets.Set[string]) {
	klog.V(5).InfoS("Processed static policies: %+v", allProcessedGWIPs)
	ipsToKeep := sets.New[string]()
	invalidGWIPs := sets.New[string]()
	newGateways := make(gatewayInfoList, 0)

	for _, gwInfo := range allProcessedGWIPs {
		info := newGatewayInfo(sets.New[string](), gwInfo.BFDEnabled)
		for ip := range gwInfo.Gateways.items {
			if !cachedStaticGWInfo.HasIP(ip) {
				klog.V(5).InfoS("PP to cacheInfo: static GW IP %s not found in namespace cache ", ip)
				invalidGWIPs.Insert(ip)
				continue
			}
			ipsToKeep.Insert(ip)
			info.Gateways.items.Insert(ip)
		}
		if info.Gateways.items.Len() > 0 {
			newGateways = append(newGateways, info)
		}
	}

	// Compare all elements in the cacheInfo map against the consolidated map: those that are not in the consolidated map are to be deleted.
	// The previous loop covers for the static IPs that exist in both slices but contain different gateway infos, and thus to be deleted
	for _, cachedInfo := range cachedStaticGWInfo {
		for ip := range cachedInfo.Gateways.items {
			if !allProcessedGWIPs.HasIP(ip) {
				klog.V(5).InfoS("CacheInfo-> static GW IP %v not found in processed policies", ip)
				invalidGWIPs.Insert(ip)
			}
		}
	}
	return newGateways, invalidGWIPs, ipsToKeep
}

func (m *externalPolicyManager) calculateDynamicGateways(allProcessedGWIPs, cachedDynamicGWInfo map[ktypes.NamespacedName]*gatewayInfo) (map[ktypes.NamespacedName]*gatewayInfo, sets.Set[string], sets.Set[string]) {
	klog.V(5).InfoS("Processed dynamic policies: %+v", allProcessedGWIPs)
	// In order to delete the invalid GWs, the logic has to collect all the valid GW IPs as well as the invalid ones.
	// This is due to implementation requirements by the network clients: for the NB interaction (master controller), only the invalid GWs is needed
	// but when interacting with the conntrack (node controller), it requires to use only the valid GWs due to a white listing approach
	// (delete any entry that does not reference any of these IPs) when deleting its entries.
	ipsToKeep := sets.New[string]()
	invalidGWIPs := sets.New[string]()
	// this map will be used to store all valid gateway info references as they are processed in the next two loops.
	newGateways := map[ktypes.NamespacedName]*gatewayInfo{}
	for k, v1 := range allProcessedGWIPs {
		v2, ok := cachedDynamicGWInfo[k]
		if ok && !v1.Equal(v2) {
			// podGW not found or its gatewayInfo does not match, remove it
			klog.V(5).InfoS("PP to cacheInfo: invalid GW IP %+v compared to %+v", v2, v1)
			invalidGWIPs.Insert(v2.Gateways.UnsortedList()...)
			continue
		}
		// store th gatewayInfo in the map
		klog.V(5).InfoS("Storing %s when removing pod GWs ", k)
		newGateways[k] = v1
		ipsToKeep.Insert(v1.Gateways.UnsortedList()...)
	}

	// Compare all elements in the cacheInfo map against the consolidated map: those that are not in the consolidated map are to be deleted.
	// The previous loop covers for the pods that exist in both maps but contain different gateway infos, and thus to be deleted
	for k, v := range cachedDynamicGWInfo {
		if _, ok := allProcessedGWIPs[k]; !ok {
			// IP not found in the processed policies, it means the pod gateway information is no longer applicable
			klog.V(5).InfoS("CacheInfo-> GW IP %+v not found in processed policies", k)
			invalidGWIPs.Insert(v.Gateways.UnsortedList()...)
		}
	}
	return newGateways, invalidGWIPs, ipsToKeep
}

// applyProcessedPolicy takes in a route policy and applies it to each of the namespaces defined in the namespaces selector in the route policy.
// As part of the process, it also updates the namespace info cache with the new gatway information derived from the route policy, so that it keeps
// track for each namespace of the gateway IPs that are being applied and the names of the policies impacting the namespace.
func (m *externalPolicyManager) applyProcessedPolicy(policyName string, routePolicy *routePolicy) error {
	targetNs, err := m.listNamespacesBySelector(routePolicy.targetNamespacesSelector)
	if err != nil {
		return err
	}
	for _, ns := range targetNs {
		cacheInfo, found := m.getNamespaceInfoFromCache(ns.Name)
		if !found {
			cacheInfo = m.newNamespaceInfoInCache(ns.Name)
		}
		// ensure namespace still exists before processing it
		namespace, err := m.namespaceLister.Get(ns.Name)
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		// if the namespace no longer exists or it is being deleted then skip it
		if apierrors.IsNotFound(err) || !namespace.DeletionTimestamp.IsZero() {
			if !found {
				// remove it as we are responsible for creating it.
				m.namespaceInfoSyncCache.Delete(ns.Name)
			}
			m.unlockNamespaceInfoCache(ns.Name)
			continue
		}
		err = m.applyProcessedPolicyToNamespace(ns.Name, policyName, routePolicy, cacheInfo)
		m.unlockNamespaceInfoCache(ns.Name)
		if err != nil {
			return err
		}
	}
	return nil
}

// processDeletePolicy takes in a policy, marks it for deletion and proceeds to delete the gateway IPs derived from the static and dynamic hops from the namespaces impacted by the policy, as defined by the namespace
// selector in the from field. The last step is to delete it from the cache.
func (m *externalPolicyManager) processDeletePolicy(policyName string) error {

	// mark the policy for deletion.
	// if it's already marked continue processing the delete action as this could be a retry attempt from a previous failed delete run.
	// if it's no longer in the cache, return nil
	klog.V(5).InfoS("Getting route %s and marking it for deletion", policyName)
	routePolicy, found := m.getAndMarkRoutePolicyForDeletionInCache(policyName)
	if !found {
		klog.V(5).InfoS("Policy %s not found", policyName)
		return nil
	}
	for _, ns := range m.getAllNamespacesNamesInCache() {
		cacheInfo, found := m.getNamespaceInfoFromCache(ns)
		if !found {
			klog.V(5).InfoS("Attempting to delete policy %s from a namespace that does not exist %s", routePolicy.Name, ns)
			continue
		}
		var err error
		if cacheInfo.Policies.Has(routePolicy.Name) {
			err = m.removePolicyFromNamespace(ns, &routePolicy, cacheInfo)
		}
		m.unlockNamespaceInfoCache(ns)
		if err != nil {
			return err
		}
	}
	klog.V(5).InfoS("Proceeding to delete route %s from cache", routePolicy.Name)
	err := m.deleteRoutePolicyFromCache(routePolicy.Name)
	if err != nil {
		return err
	}
	klog.V(4).InfoS("Deleted Admin Policy Based External Route %s", routePolicy.Name)
	return nil
}

// calculateAnnotatedNamespaceGatewayIPsForNamespace retrieves the list of IPs defined by the legacy annotation gateway logic for namespaces.
// this function is used when deleting gateway IPs to ensure that IPs that overlap with the annotation logic are not deleted from the network resource
// (north bound or conntrack) when the given IP is deleted when removing the policy that references them.
func (m *externalPolicyManager) calculateAnnotatedNamespaceGatewayIPsForNamespace(targetNamespace string) (sets.Set[string], error) {
	namespace, err := m.namespaceLister.Get(targetNamespace)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return sets.New[string](), nil
		}
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

// calculateAnnotatedPodGatewayIPsForNamespace retrieves the list of IPs defined by the legacy annotation gateway logic for pods.
// this function is used when deleting gateway IPs to ensure that IPs that overlap with the annotation logic are not deleted from the network resource
// (north bound or conntrack) when the given IP is deleted when removing the policy that references them.
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

// deletePolicyInNamespace removes the gateway IPs derived from a policy in a namespace. It takes into account the gateway IPs from the legacy
// annotations and other policies impacting the same namespace to avoid deleting IPs that coexist in other resources.
// In a nutshell, if a gateway IP is only found in the policy being deleted, then the IP is removed from the network resource. But if the IP is
// found in at least a legacy annotation or another policy impacting the namespace, then the IP is not removed from the cache or the network resource (north bound or conntrack)
func (m *externalPolicyManager) deletePolicyInNamespace(namespaceName, policyName string, routePolicy *routePolicy, cacheInfo *namespaceInfo) error {
	coexistingPolicies := cacheInfo.Policies.Clone().Delete(policyName)

	// don't care if the route is flagged for deletion, delete any gw IPs related to the policy
	policy, found, _ := m.getRoutePolicyFromCache(policyName)
	if !found {
		return fmt.Errorf("policy %s not found", policyName)
	}
	allGWIPsToDelete := sets.New[string]()
	allGWIPsToKeep := sets.New[string]()

	pp, err := m.processExternalRoutePolicy(policy)
	if err != nil {
		return err
	}

	// Static Hops
	annotatedGWIPs, err := m.calculateAnnotatedNamespaceGatewayIPsForNamespace(namespaceName)
	if err != nil {
		return err
	}
	coexistingIPs, err := m.retrieveStaticGatewayIPsForPolicies(coexistingPolicies)
	if err != nil {
		return err
	}

	static := sets.New[string]()
	// consolidate all IPs from the static hops of the policy
	for _, gatewayInfo := range pp.staticGateways {
		static = static.Insert(gatewayInfo.Gateways.UnsortedList()...)
	}
	// delete the subset of IPs that are meant to be deleted from the static hops defined by the policy
	for _, gwInfo := range routePolicy.staticGateways {
		static = static.Delete(gwInfo.Gateways.UnsortedList()...)
	}
	// Consolidate all IPs to keep:
	// * IPs from other policies that target the namespace.
	// * Annotated IPs coming from the legacy gateway API.
	// * Remaining IPs from the policy after removing those IPs that are no longer applicable to the policy. This can happen when a policy has changed its spec and now a subset of the old IPs are
	//   no longer valid based on the new spec. This set contains only the subset of original IPs that are still valid.
	//
	coexistingIPs = coexistingIPs.Union(annotatedGWIPs).Union(static)

	for _, gwInfo := range routePolicy.staticGateways {
		// Filter out the IPs that are not in coexisting. Those IPs are to be deleted.
		invalidGWIPs := gwInfo.Gateways.Difference(coexistingIPs)
		// Filter out the IPs from the coexisting list that are to be kept by calculating the difference between the coexising and those IPs that are to be deleted and not coexisting at the same time.
		ipsToKeep := coexistingIPs.Difference(invalidGWIPs)
		klog.V(4).InfoS("Coexisting %s, invalid %s, ipsToKeep %s", strings.Join(sets.List(coexistingIPs), ","), strings.Join(sets.List(invalidGWIPs), ","), strings.Join(sets.List(ipsToKeep), ","))
		if len(invalidGWIPs) == 0 {
			continue
		}
		allGWIPsToDelete = allGWIPsToDelete.Union(invalidGWIPs)
		allGWIPsToKeep = allGWIPsToKeep.Union(ipsToKeep)

		if gwInfo.Gateways.Difference(invalidGWIPs).Len() == 0 {
			cacheInfo.StaticGateways = cacheInfo.StaticGateways.Delete(gwInfo)
			continue
		}
		gwInfo.Gateways.Delete(invalidGWIPs.UnsortedList()...)
	}

	// Dynamic Hops
	annotatedGWIPs, err = m.calculateAnnotatedPodGatewayIPsForNamespace(namespaceName)
	if err != nil {
		return err
	}

	coexistingIPs, err = m.retrieveDynamicGatewayIPsForPolicies(coexistingPolicies)
	if err != nil {
		return err
	}

	dynamic := sets.New[string]()
	for _, gatewayInfo := range pp.dynamicGateways {
		dynamic = static.Insert(gatewayInfo.Gateways.UnsortedList()...)
	}
	for _, gwInfo := range routePolicy.dynamicGateways {
		dynamic = dynamic.Delete(gwInfo.Gateways.UnsortedList()...)
	}
	// Consolidate all IPs to keep:
	// * IPs from other policies that target the namespace.
	// * Annotated IPs coming from the legacy gateway API.
	// * Remaining IPs from the policy after removing those IPs that are no longer applicable to the policy. This can happen when a policy has changed its spec and now a subset of the old IPs are
	//   no longer valid based on the new spec. This set contains only the subset of original IPs that are still valid.
	//
	coexistingIPs = coexistingIPs.Union(annotatedGWIPs).Union(dynamic)

	for gwPodNamespacedName, gwInfo := range routePolicy.dynamicGateways {
		// Filter out the IPs that are not in coexisting. Those IPs are to be deleted.
		invalidGWIPs := gwInfo.Gateways.Difference(coexistingIPs)
		// Filter out the IPs from the coexisting list that are to be kept by calculating the difference between the coexising and those IPs that are to be deleted and not coexisting at the same time.
		ipsToKeep := coexistingIPs.Difference(invalidGWIPs)
		klog.V(4).InfoS("Coexisting %s, invalid %s, ipsToKeep %s", strings.Join(sets.List(coexistingIPs), ","), strings.Join(sets.List(invalidGWIPs), ","), strings.Join(sets.List(ipsToKeep), ","))
		if len(invalidGWIPs) == 0 {
			continue
		}
		allGWIPsToDelete = allGWIPsToDelete.Union(invalidGWIPs)
		allGWIPsToKeep = allGWIPsToKeep.Union(ipsToKeep)

		if gwInfo.Gateways.Difference(invalidGWIPs).Len() == 0 {
			// delete cached information for the pod gateway
			klog.V(5).InfoS("Deleting cache entry for dynamic hop %s", gwPodNamespacedName)
			delete(cacheInfo.DynamicGateways, gwPodNamespacedName)
			continue
		}
		klog.V(4).InfoS("Deleting dynamic hop IPs in gateway info %s", strings.Join(sets.List(invalidGWIPs), ","))
		gwInfo.Gateways.Delete(invalidGWIPs.UnsortedList()...)
	}
	// Processing IPs
	// Delete them in bulk since the conntrack is using a white list approach (IPs in AllGWIPsToKeep) to filter out the entries that need to be removed.
	// This is not a non-issue for the north bound DB since the client uses the allGWIPsToDelete set to remove the IPs.
	return m.netClient.deleteGatewayIPs(namespaceName, allGWIPsToDelete, allGWIPsToKeep)
}

// applyProcessedPolicyToNamespace applies the gateway IPs derived from the processed policy to a namespace and updates the cache information for the namespace.
func (m *externalPolicyManager) applyProcessedPolicyToNamespace(namespaceName, policyName string, routePolicy *routePolicy, cacheInfo *namespaceInfo) error {

	if routePolicy.staticGateways.Len() > 0 {
		err := m.addGWRoutesForNamespace(namespaceName, routePolicy.staticGateways)
		if err != nil {
			return err
		}
		var duplicated sets.Set[string]
		cacheInfo.StaticGateways, duplicated, err = cacheInfo.StaticGateways.Insert(routePolicy.staticGateways...)
		if err != nil {
			return err
		}
		if duplicated.Len() > 0 {
			klog.Warningf("Found duplicated gateway IP(s) %+s in policy %s", sets.List(duplicated), policyName)
		}
	}
	for pod, info := range routePolicy.dynamicGateways {
		err := m.addGWRoutesForNamespace(namespaceName, gatewayInfoList{info})
		if err != nil {
			return err
		}
		cacheInfo.DynamicGateways[pod] = newGatewayInfo(info.Gateways.items, info.BFDEnabled)

	}
	klog.V(4).InfoS("Adding policy %s to namespace %s", policyName, namespaceName)
	cacheInfo.Policies = cacheInfo.Policies.Insert(policyName)
	return nil
}

// processUpdatePolicy takes in the current and updated version of a given policy and applies the following logic:
// * Determine the changes between the current and updated version.
// * Remove the static and dynamic hop entries in the namespaces impacted by the current version of the policy that are in the current policy but not in the updated version.
// * Apply the static and dynamic hop entries in the namespaces impacted by the updated version of the policy that are in the updated version but not in the current version.
// * Store the updated policy in the route policy cache.
func (m *externalPolicyManager) processUpdatePolicy(currentPolicy, updatedPolicy *adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute) error {
	klog.V(5).InfoS("Processing update for Admin Policy Based External Route '%s'", currentPolicy.Name)

	// To update the policies, first we'll process the diff between old and new and remove the discrepancies that are not found in the new object.
	// Afterwards, we'll process the diff between the new and the old and apply the new policies not found in the old policy, ensuring that we are not reduplicating the gatewayInfo.
	err := m.removeDiscrepanciesInRoutePolicy(currentPolicy, updatedPolicy)
	if err != nil {
		return err
	}
	// At this point we have removed all the aspects of the current policy that no longer applies. Next step is to apply the parts of the new policy that are not in the current one.
	err = m.applyUpdatesInRoutePolicy(currentPolicy, updatedPolicy)
	if err != nil {
		return err
	}

	// update the cache to ensure it reflects the latest copy
	return m.storeRoutePolicyInCache(updatedPolicy)
}

func (m *externalPolicyManager) applyUpdatesInRoutePolicy(currentPolicy, newPolicy *adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute) error {
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
	// Retrieve all new namespaces
	nsList, err := m.listNamespacesBySelector(&newPolicy.Spec.From.NamespaceSelector)
	if err != nil {
		return err
	}
	for _, ns := range nsList {
		if additionalNamespaces.Has(ns.Name) {
			// Policy has already been fully applied to this namespace by the previous operation
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
	unmatchingNamespaces, unmatchingStaticHops, unmatchingDynamicHops, err := m.calculatePolicyDifferences(currentPolicy, updatedPolicy)
	if err != nil {
		return err
	}
	klog.V(4).InfoS("Removing discrepancies between current and updated policy: namespaces %+v, static hop IPs %+v, dynamic hop IPs %+v", unmatchingNamespaces, unmatchingStaticHops, unmatchingDynamicHops)
	// Delete the namespaces where this policy no longer applies
	for unmatchNs := range unmatchingNamespaces {
		cacheInfo, found := m.getNamespaceInfoFromCache(unmatchNs)
		if !found {
			klog.Warningf("Attempting to delete policy %s from a namespace that does not exist %s", currentPolicy.Name, unmatchNs)
			continue
		}
		err := m.removePolicyFromNamespace(unmatchNs, currentPolicy, cacheInfo)
		m.unlockNamespaceInfoCache(unmatchNs)
		if err != nil {
			return err
		}
	}

	// Delete the hops that no longer apply from all the current policy's applicable namespaces
	processedStaticHops, err := m.processStaticHopsGatewayInformation(unmatchingStaticHops)
	if err != nil {
		return err
	}
	processedDynamicHops, err := m.processDynamicHopsGatewayInformation(unmatchingDynamicHops)
	if err != nil {
		return err
	}
	// Retrieve all current namespaces
	nsList, err := m.listNamespacesBySelector(&currentPolicy.Spec.From.NamespaceSelector)
	if err != nil {
		return err
	}
	for _, ns := range nsList {
		if unmatchingNamespaces.Has(ns.Name) {
			// Policy has already been deleted in this namespace by the previous operation
			continue
		}
		cacheInfo, found := m.getNamespaceInfoFromCache(ns.Name)
		if !found {
			klog.Warningf("Attempting to update policy %s for a namespace that does not exist %s", currentPolicy.Name, ns.Name)
			continue
		}
		err = m.deletePolicyInNamespace(ns.Name, currentPolicy.Name, &routePolicy{dynamicGateways: processedDynamicHops, staticGateways: processedStaticHops}, cacheInfo)
		m.unlockNamespaceInfoCache(ns.Name)
		if err != nil {
			return err
		}
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
		gwList = append(gwList, newGatewayInfo(sets.New(ip.String()), h.BFDEnabled))
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
				// If we found any gateways then we need to update current pods routing in the relevant namespace
				if len(foundGws) == 0 {
					klog.Warningf("No valid gateway IPs found for requested external gateway pod %s/%s", pod.Namespace, pod.Name)
					continue
				}
				key := ktypes.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}
				if _, ok := podsInfo[key]; ok {
					klog.Warningf("Found overlapping dynamic hop policy for pod %s, discarding match entry", key)
					continue
				}
				podsInfo[key] = newGatewayInfo(foundGws, h.BFDEnabled)
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
	if len(staticGWInfo) > 0 {
		klog.V(5).InfoS("Found static hops for policy %s:%+v", policy.Name, staticGWInfo)
	}
	dynamicGWInfo, err := m.processDynamicHopsGatewayInformation(policy.Spec.NextHops.DynamicHops)
	if err != nil {
		errors = append(errors, err)
	}
	if len(dynamicGWInfo) > 0 {
		klog.V(5).InfoS("Found dynamic hops for policy %s:%+v", policy.Name, dynamicGWInfo)
	}
	if len(errors) > 0 {
		return nil, kerrors.NewAggregate(errors)
	}
	return &routePolicy{
		targetNamespacesSelector: &policy.Spec.From.NamespaceSelector,
		staticGateways:           staticGWInfo,
		dynamicGateways:          dynamicGWInfo,
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

// calculatePolicyDifferences determines the differences between two policies in terms of namespaces where the policy applies, and the differences in static and dynamic hops.
// The return values are the namespaces, static hops and dynamic hops that are in the first policy but not in the second instance.
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

// calculateStaticHopDifferences determines the difference between the first slice and the second staticHops slice. The outcome is a slice
// of static hops that are in the staticHop1 slice but not in the staticHop2 slice.
func (m *externalPolicyManager) calculateStaticHopDifferences(staticHops1, staticHops2 []*adminpolicybasedrouteapi.StaticHop) []*adminpolicybasedrouteapi.StaticHop {
	diffStatic := make([]*adminpolicybasedrouteapi.StaticHop, 0)
	for _, staticHop1 := range staticHops1 {
		var found bool
		for _, staticHop2 := range staticHops2 {
			if reflect.DeepEqual(staticHop1, staticHop2) {
				found = true
				break
			}
		}
		if !found {
			diffStatic = append(diffStatic, staticHop1)
		}
	}
	return diffStatic
}

// calculateDynamicHopDifferences determines the difference between the first slice and the second dynamicHop slice. The return value is a slice
// of dynamic hops that are in the first slice but not in the second.
func (m *externalPolicyManager) calculateDynamicHopDifferences(dynamicHops1, dynamicHops2 []*adminpolicybasedrouteapi.DynamicHop) ([]*adminpolicybasedrouteapi.DynamicHop, error) {
	diffDynamic := make([]*adminpolicybasedrouteapi.DynamicHop, 0)
	for _, dynamicHop1 := range dynamicHops1 {
		var found bool
		for _, dynamicHop2 := range dynamicHops2 {

			if reflect.DeepEqual(dynamicHop1, dynamicHop2) {
				found = true
				break
			}
		}
		if !found {
			diffDynamic = append(diffDynamic, dynamicHop1)
		}
	}
	return diffDynamic, nil
}

// retrieveDynamicGatewayIPsForPolicies returns all the gateway IPs from the dynamic hops of all the policies in the set. This function is used
// to retrieve the dynamic gateway IPs from all the policies applicable to a specific namespace.
func (m *externalPolicyManager) retrieveDynamicGatewayIPsForPolicies(coexistingPolicies sets.Set[string]) (sets.Set[string], error) {
	coexistingDynamicIPs := sets.New[string]()

	for name := range coexistingPolicies {
		policy, found, markedForDeletion := m.getRoutePolicyFromCache(name)
		if !found {
			return nil, fmt.Errorf("failed to find external route policy %s in cache", name)
		}
		if markedForDeletion {
			continue
		}
		pp, err := m.processDynamicHopsGatewayInformation(policy.Spec.NextHops.DynamicHops)
		if err != nil {
			return nil, err
		}
		for _, gatewayInfo := range pp {
			coexistingDynamicIPs = coexistingDynamicIPs.Insert(gatewayInfo.Gateways.UnsortedList()...)
		}
	}
	return coexistingDynamicIPs, nil
}

// retrieveStaticGatewayIPsForPolicies returns all the gateway IPs from the static hops of all the policies in the set. This function is used
// to retrieve the static gateway IPs from all the policies applicable to a specific namespace.
func (m *externalPolicyManager) retrieveStaticGatewayIPsForPolicies(policies sets.Set[string]) (sets.Set[string], error) {
	coexistingStaticIPs := sets.New[string]()

	for name := range policies {
		policy, found, markedForDeletion := m.getRoutePolicyFromCache(name)
		if !found {
			return nil, fmt.Errorf("unable to find route policy %s in cache", name)
		}
		if markedForDeletion {
			continue
		}
		pp, err := m.processStaticHopsGatewayInformation(policy.Spec.NextHops.StaticHops)
		if err != nil {
			return nil, err
		}
		for _, gatewayInfo := range pp {
			coexistingStaticIPs = coexistingStaticIPs.Insert(gatewayInfo.Gateways.UnsortedList()...)
		}
	}
	return coexistingStaticIPs, nil
}
