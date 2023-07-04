package apbroute

import (
	"fmt"
	"net"
	"strings"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"

	adminpolicybasedrouteapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/controller/apbroute/gateway_info"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

// syncRoutePolicy syncs policy with a given name, returns gwIPS to update policy status and error.
// gwIPS == nil if the policy was deleted, and empty if error happened.
func (m *externalPolicyManager) syncRoutePolicy(policyName string) (sets.Set[string], error) {
	var updatedPolicyObj *adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute
	// gwIPs are used to update policy status with the latest applied config
	gwIPs := sets.New[string]()
	// 1. Take a lock on the existing policy state, as we are going to use it for cleanup and update.
	// 2. Build latest policy config "updatedPolicy". This includes listing referenced namespaces and pods.
	// To make sure there is no race with pod and namespace handlers, policyReferencedObjectsLock is acquired
	// before listing objects, and released when the "updatedPolicy" is built. At this point namespace and pod
	// handler can use policyReferencedObjects to see which objects were considered as related by the policy
	// handler last time.
	// 3. Pass existing policy config "existingPolicy" and latest policy config "updatedPolicy" to updateRoutePolicy
	// function, that will apply "updatedPolicy" config to the "existingPolicy" and update "existingPolicy"
	// status for every applied change.
	// 4. On success, return applied ips from the updatedPolicy and delete policy from the cache
	return gwIPs, m.routePolicySyncCache.DoWithLock(policyName, func(policyName string) error {

		var err error
		updatedPolicyObj, err = m.routeLister.Get(policyName)
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to get AdminPolicyBasedExternalRoute before sync: %w", err)
		}

		var updatedPolicy *routePolicyConfig
		// get updated policy and update policy refs
		if apierrors.IsNotFound(err) || !updatedPolicyObj.DeletionTimestamp.IsZero() {
			// policy deleted
			updatedPolicy = nil
			m.deletePolicyRefObjects(policyName)
		} else {
			updatedPolicy, err = m.getPolicyConfigAndUpdatePolicyRefs(updatedPolicyObj, true)
			if err != nil {
				return fmt.Errorf("failed to build updated policy: %w", err)
			}
		}

		// get existing policy and update routePolicySyncCache
		existingPolicy, found := m.routePolicySyncCache.Load(policyName)
		if !found {
			if updatedPolicy == nil {
				// nothing to do
				return nil
			}
			existingPolicy = newRoutePolicyState()
			m.routePolicySyncCache.Store(policyName, existingPolicy)
		}

		err = m.updateRoutePolicy(existingPolicy, updatedPolicy)
		if err != nil {
			return fmt.Errorf("failed to update policy from %s to %+v: %w", existingPolicy.String(), updatedPolicy, err)
		}
		if updatedPolicy == nil {
			m.routePolicySyncCache.Delete(policyName)
			gwIPs = nil
		} else {
			// update was successful, return ips from updatedPolicy, since existingPolicy will have the same config.
			for _, static := range updatedPolicy.staticGateways.Elems() {
				insertSet(gwIPs, static.Gateways)
			}
			for _, dynamic := range updatedPolicy.dynamicGateways.Elems() {
				insertSet(gwIPs, dynamic.Gateways)
			}
		}
		return nil
	})
}

// updateRoutePolicy cleans up stale gateways that are present in the existingPolicy, but not in the updatedPolicy.
// And creates the new gateways that are present in the updatedPolicy, but not in the existingPolicy
// (or if they were not successfully applied last time). "existingPolicy" will be updated after every db operation with
// the latest changes, and "applied" flag that tells if the operation was successful.
func (m *externalPolicyManager) updateRoutePolicy(existingPolicy *routePolicyState, updatedPolicy *routePolicyConfig) error {
	// cleanup first
	if len(existingPolicy.targetNamespaces) > 0 {
		// track which namespaces should be removed from targetNamespaces
		namespacesToDelete := []string{}
		for targetNamespace, targetPods := range existingPolicy.targetNamespaces {
			allGWIPsToDelete := sets.New[string]()
			allGWIPsToKeep := sets.New[string]()

			// Static Hops
			annotatedNamespaceGWIPs, err := m.calculateAnnotatedNamespaceGatewayIPsForNamespace(targetNamespace)
			if err != nil {
				return err
			}

			// Dynamic Hops
			annotatedPodGWIPs, err := m.calculateAnnotatedPodGatewayIPsForNamespace(targetNamespace)
			if err != nil {
				return err
			}

			allGWIPsToKeep = allGWIPsToKeep.Union(annotatedNamespaceGWIPs).Union(annotatedPodGWIPs)

			// track which pods should be removed from targetPods
			podsToDelete := []ktypes.NamespacedName{}
			for podNamespacedName, existingPodConfig := range targetPods {
				staticGWsToDelete := gateway_info.NewGatewayInfoList()
				dynamicGWsToDelete := gateway_info.NewGatewayInfoList()

				// Static Hops
				// Find gateways that are present in the pod config, but not in the updatedPolicy
				gwIPsToDelete := sets.New[string]()
				gwIPsToLeave := sets.New[string]()
				for _, existingGW := range existingPodConfig.StaticGateways.Elems() {
					// delete pod gateway if
					// 1. policy is deleted
					// 2. it is not present in the updatedPolicy
					// 3. target pod is not listed in the updatedPolicy.targetNamespacesWithPods
					if updatedPolicy == nil || !updatedPolicy.staticGateways.Has(existingGW) ||
						updatedPolicy.targetNamespacesWithPods[targetNamespace][podNamespacedName] == nil {
						staticGWsToDelete.InsertOverwrite(existingGW)
						insertSet(gwIPsToDelete, existingGW.Gateways)
					} else {
						insertSet(gwIPsToLeave, existingGW.Gateways)
					}
				}
				// don't delete ips from annotations
				ipsToDelete := gwIPsToDelete.Difference(annotatedNamespaceGWIPs)

				insertSet(allGWIPsToDelete, ipsToDelete)
				insertSet(allGWIPsToKeep, gwIPsToLeave)

				// Dynamic Hops
				gwIPsToDelete = sets.New[string]()
				gwIPsToLeave = sets.New[string]()
				for _, existingGW := range existingPodConfig.DynamicGateways.Elems() {
					// delete pod gateway if
					// 1. policy is deleted
					// 2. it is not present in the updatedPolicy
					// 3. target pod is not listed in the updatedPolicy.targetNamespacesWithPods
					if updatedPolicy == nil || !updatedPolicy.dynamicGateways.Has(existingGW) ||
						updatedPolicy.targetNamespacesWithPods[targetNamespace][podNamespacedName] == nil {
						dynamicGWsToDelete.InsertOverwrite(existingGW)
						insertSet(gwIPsToDelete, existingGW.Gateways)
					} else {
						insertSet(gwIPsToLeave, existingGW.Gateways)
					}
				}
				// don't delete ips from annotations
				ipsToDelete = gwIPsToDelete.Difference(annotatedPodGWIPs)

				insertSet(allGWIPsToDelete, ipsToDelete)
				insertSet(allGWIPsToKeep, gwIPsToLeave)

				if staticGWsToDelete.Len() > 0 || dynamicGWsToDelete.Len() > 0 {
					// Processing IPs
					// Delete them in bulk since the conntrack is using a white list approach (IPs in AllGWIPsToKeep) to filter out the entries that need to be removed.
					// This is not a non-issue for the north bound DB since the client uses the allGWIPsToDelete set to remove the IPs.
					err := m.netClient.deleteGatewayIPs(podNamespacedName, allGWIPsToDelete, allGWIPsToKeep)
					if err != nil {
						// update pod config with failure status, deletion can be retried next time
						existingPodConfig.StaticGateways.InsertOverwriteFailed(staticGWsToDelete.Elems()...)
						existingPodConfig.DynamicGateways.InsertOverwriteFailed(dynamicGWsToDelete.Elems()...)
						return err
					}
					// remove successfully deleted gateways from the existingPodConfig
					existingPodConfig.StaticGateways.Delete(staticGWsToDelete.Elems()...)
					existingPodConfig.DynamicGateways.Delete(dynamicGWsToDelete.Elems()...)
				}

				if updatedPolicy == nil || updatedPolicy.targetNamespacesWithPods[targetNamespace][podNamespacedName] == nil {
					podsToDelete = append(podsToDelete, podNamespacedName)
				}
			}
			for _, podToDelete := range podsToDelete {
				delete(targetPods, podToDelete)
			}
			if updatedPolicy == nil || updatedPolicy.targetNamespacesWithPods[targetNamespace] == nil {
				namespacesToDelete = append(namespacesToDelete, targetNamespace)
			}
		}
		for _, nsToDelete := range namespacesToDelete {
			delete(existingPolicy.targetNamespaces, nsToDelete)
		}
	}
	// apply new changes
	if updatedPolicy != nil {
		for updatedTargetNS, updatedTargetPod := range updatedPolicy.targetNamespacesWithPods {
			existingNs, found := existingPolicy.targetNamespaces[updatedTargetNS]
			if !found {
				existingNs = map[ktypes.NamespacedName]*podInfo{}
				existingPolicy.targetNamespaces[updatedTargetNS] = existingNs
			}
			for updatedPodNamespacedName, updatedPod := range updatedTargetPod {
				existingTargetPodConfig, found := existingNs[updatedPodNamespacedName]
				if !found {
					existingTargetPodConfig = newPodInfo()
					existingNs[updatedPodNamespacedName] = existingTargetPodConfig
				}
				// applyPodConfig will apply changes and update existingTargetPodConfig with the status
				err := m.applyPodConfig(updatedPod, existingTargetPodConfig, updatedPolicy)
				if err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// applyPodConfig applies the gateway IPs derived from the processed policy to a pod and updates existingPodConfig.
func (m *externalPolicyManager) applyPodConfig(pod *v1.Pod, existingPodConfig *podInfo, updatedPolicy *routePolicyConfig) error {
	// update static gw
	gwsToAdd := gateway_info.NewGatewayInfoList()
	for _, newGW := range updatedPolicy.staticGateways.Elems() {
		if !existingPodConfig.StaticGateways.HasWithoutErr(newGW) {
			gwsToAdd.InsertOverwrite(newGW)
		}
	}
	if gwsToAdd.Len() > 0 {
		err := m.netClient.addGatewayIPs(pod, gwsToAdd)
		if err != nil {
			existingPodConfig.StaticGateways.InsertOverwriteFailed(gwsToAdd.Elems()...)
			return err
		}
		existingPodConfig.StaticGateways.InsertOverwrite(gwsToAdd.Elems()...)
	}
	// update dynamic gw
	gwsToAdd = gateway_info.NewGatewayInfoList()
	for _, newGW := range updatedPolicy.dynamicGateways.Elems() {
		if !existingPodConfig.DynamicGateways.HasWithoutErr(newGW) {
			gwsToAdd.InsertOverwrite(newGW)
		}
	}
	if gwsToAdd.Len() > 0 {
		err := m.netClient.addGatewayIPs(pod, gwsToAdd)
		if err != nil {
			existingPodConfig.DynamicGateways.InsertOverwriteFailed(gwsToAdd.Elems()...)
			return err
		}
		existingPodConfig.DynamicGateways.InsertOverwrite(gwsToAdd.Elems()...)
	}
	klog.V(4).Infof("Applying policy %s to pod %s", updatedPolicy.policyName, getPodNamespacedName(pod))
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
	podList, err := m.podLister.Pods(targetNamespace).List(labels.Everything())
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
			insertSet(gwIPs, foundGws)
		}
	}
	return gwIPs, nil
}

func (m *externalPolicyManager) processStaticHopsGatewayInformation(hops []*adminpolicybasedrouteapi.StaticHop) (*gateway_info.GatewayInfoList, error) {
	gwList := gateway_info.NewGatewayInfoList()

	// collect all the static gateway information from the nextHops slice
	for _, h := range hops {
		ip := net.ParseIP(h.IP)
		if ip == nil {
			return nil, fmt.Errorf("could not parse routing static gw annotation value '%s'", h.IP)
		}
		gwList.InsertOverwrite(gateway_info.NewGatewayInfo(sets.New(ip.String()), h.BFDEnabled))
	}
	return gwList, nil
}

func (m *externalPolicyManager) processDynamicHopsGatewayInformation(hops []*adminpolicybasedrouteapi.DynamicHop) (*gateway_info.GatewayInfoList,
	sets.Set[string], sets.Set[ktypes.NamespacedName], error) {
	podsInfo := gateway_info.NewGatewayInfoList()
	selectedNamespaces := sets.Set[string]{}
	selectedPods := sets.Set[ktypes.NamespacedName]{}
	for _, h := range hops {
		gwNsSel := labels.Everything()
		if h.NamespaceSelector != nil {
			gwNsSel, _ = metav1.LabelSelectorAsSelector(h.NamespaceSelector)
		}

		gwNamespaces, err := m.namespaceLister.List(gwNsSel)
		if err != nil {
			return podsInfo, selectedNamespaces, selectedPods, fmt.Errorf("failed to list namespaces: %w", err)
		}

		for _, gwNamespace := range gwNamespaces {
			gwPodSel, err := metav1.LabelSelectorAsSelector(&h.PodSelector)
			if err != nil {
				return podsInfo, selectedNamespaces, selectedPods, fmt.Errorf("failed to convert pod selector: %w", err)
			}
			gwPods, err := m.podLister.Pods(gwNamespace.Name).List(gwPodSel)
			if err != nil {
				return podsInfo, selectedNamespaces, selectedPods, fmt.Errorf("failed to list pods: %w", err)
			}
			for _, pod := range gwPods {
				foundGws, err := getExGwPodIPs(pod, h.NetworkAttachmentName)
				if err != nil {
					return podsInfo, selectedNamespaces, selectedPods, fmt.Errorf("failed to get external GW pod ips: %w", err)
				}
				// If we found any gateways then we need to update current pods routing in the relevant namespace
				if len(foundGws) == 0 {
					klog.Warningf("No valid gateway IPs found for requested external gateway pod %s/%s", pod.Namespace, pod.Name)
					continue
				}
				key := ktypes.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}
				podsInfo.InsertOverwrite(gateway_info.NewGatewayInfo(foundGws, h.BFDEnabled))
				selectedPods.Insert(key)
			}
			selectedNamespaces.Insert(gwNamespace.Name)
		}
	}
	return podsInfo, selectedNamespaces, selectedPods, nil
}

// getPolicyConfigAndUpdatePolicyRefs lists and updates all referenced objects for a given policy and returns
// routePolicyConfig to perform an update.
// This function should be the only one that lists referenced objects, and updates policyReferencedObjects atomically.
func (m *externalPolicyManager) getPolicyConfigAndUpdatePolicyRefs(policy *adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute,
	updateRefs bool) (*routePolicyConfig, error) {
	staticGWInfo, err := m.processStaticHopsGatewayInformation(policy.Spec.NextHops.StaticHops)
	if err != nil {
		return nil, fmt.Errorf("failed to process static GW: %w", err)
	}
	if staticGWInfo.Len() > 0 {
		klog.V(5).Infof("Found static hops for policy %s:%+v", policy.Name, staticGWInfo)
	}

	// take a lock before listing any objects
	m.policyReferencedObjectsLock.Lock()
	defer m.policyReferencedObjectsLock.Unlock()

	dynamicGWInfo, gwNamespaces, gwPods, err := m.processDynamicHopsGatewayInformation(policy.Spec.NextHops.DynamicHops)
	if err != nil {
		return nil, fmt.Errorf("failed to process dynamic GW: %w", err)
	}
	if dynamicGWInfo.Len() > 0 {
		klog.V(5).Infof("Found dynamic hops for policy %s: %+v", policy.Name, dynamicGWInfo)
	}

	targetNs, err := m.listNamespacesBySelector(&policy.Spec.From.NamespaceSelector)
	if err != nil {
		return nil, fmt.Errorf("failed to list target namespaces: %w", err)
	}

	targetNsNames := sets.Set[string]{}
	targetNamespaces := map[string]map[ktypes.NamespacedName]*v1.Pod{}
	for _, ns := range targetNs {
		targetNsNames.Insert(ns.Name)
		targetPods, err := m.podLister.Pods(ns.Name).List(labels.Everything())
		if err != nil {
			return nil, fmt.Errorf("failed to get all ns %s pods: %v", ns.Name, err)
		}
		podsMap := map[ktypes.NamespacedName]*v1.Pod{}
		for _, pod := range targetPods {
			// Ignore completed pods, host networked pods, pods not scheduled
			if util.PodWantsHostNetwork(pod) || util.PodCompleted(pod) || !util.PodScheduled(pod) {
				continue
			}
			podsMap[getPodNamespacedName(pod)] = pod
		}
		targetNamespaces[ns.Name] = podsMap
	}
	for policyName, refObjs := range m.policyReferencedObjects {
		if policyName == policy.Name {
			continue
		}
		if refObjs.targetNamespaces.Intersection(targetNsNames).Len() > 0 {
			return nil, fmt.Errorf("failed to update policy %s: namespaces %v are already affected by another policy: %s",
				policy.Name, refObjs.targetNamespaces.Intersection(targetNsNames).UnsortedList(), policyName)

		}
	}

	if updateRefs {
		refObjs := &policyReferencedObjects{
			targetNamespaces:    targetNsNames,
			dynamicGWNamespaces: gwNamespaces,
			dynamicGWPods:       gwPods,
		}
		m.policyReferencedObjects[policy.Name] = refObjs
	}

	return &routePolicyConfig{
		policyName:               policy.Name,
		targetNamespacesWithPods: targetNamespaces,
		staticGateways:           staticGWInfo,
		dynamicGateways:          dynamicGWInfo,
	}, nil
}

func (m *externalPolicyManager) deletePolicyRefObjects(policyName string) {
	m.policyReferencedObjectsLock.Lock()
	defer m.policyReferencedObjectsLock.Unlock()
	delete(m.policyReferencedObjects, policyName)
}

func getPodNamespacedName(pod *v1.Pod) ktypes.NamespacedName {
	return ktypes.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}
}
