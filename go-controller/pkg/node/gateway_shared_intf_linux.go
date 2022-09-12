//go:build linux
// +build linux

package node

import (
	"fmt"
	"strings"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	kapi "k8s.io/api/core/v1"
	v1 "k8s.io/api/discovery/v1"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
)

// deletes the local bridge used for DGP and removes the corresponding iface, as well as OVS bridge mappings
func deleteLocalNodeAccessBridge() error {
	// remove br-local bridge
	_, stderr, err := util.RunOVSVsctl("--if-exists", "del-br", types.LocalBridgeName)
	if err != nil {
		return fmt.Errorf("failed to delete bridge %s, stderr:%s (%v)",
			types.LocalBridgeName, stderr, err)
	}
	// ovn-bridge-mappings maps a physical network name to a local ovs bridge
	// that provides connectivity to that network. It is in the form of physnet1:br1,physnet2:br2.
	stdout, stderr, err := util.RunOVSVsctl("--if-exists", "get", "Open_vSwitch", ".",
		"external_ids:ovn-bridge-mappings")
	if err != nil {
		return fmt.Errorf("failed to get ovn-bridge-mappings stderr:%s (%v)", stderr, err)
	}
	if len(stdout) > 0 {
		locnetMapping := fmt.Sprintf("%s:%s", types.LocalNetworkName, types.LocalBridgeName)
		if strings.Contains(stdout, locnetMapping) {
			var newMappings string
			bridgeMappings := strings.Split(stdout, ",")
			for _, bridgeMapping := range bridgeMappings {
				if bridgeMapping != locnetMapping {
					if len(newMappings) != 0 {
						newMappings += ","
					}
					newMappings += bridgeMapping
				}
			}
			_, stderr, err = util.RunOVSVsctl("set", "Open_vSwitch", ".",
				fmt.Sprintf("external_ids:ovn-bridge-mappings=%s", newMappings))
			if err != nil {
				return fmt.Errorf("failed to set ovn-bridge-mappings, stderr:%s, error: (%v)", stderr, err)
			}
		}
	}

	klog.Info("Local Node Access bridge removed")
	return nil
}

// addGatewayIptRules adds the necessary iptable rules for a service on the node
func addGatewayIptRules(service *kapi.Service, svcHasLocalHostNetEndPnt bool) {
	rules := getGatewayIPTRules(service, svcHasLocalHostNetEndPnt)

	if err := addIptRules(rules); err != nil {
		klog.Errorf("Failed to add iptables rules for service %s/%s: %v", service.Namespace, service.Name, err)
	}
}

// delGatewayIptRules removes the iptable rules for a service from the node
func delGatewayIptRules(service *kapi.Service, svcHasLocalHostNetEndPnt bool) {
	rules := getGatewayIPTRules(service, svcHasLocalHostNetEndPnt)

	if err := delIptRules(rules); err != nil {
		klog.Errorf("Failed to delete iptables rules for service %s/%s: %v", service.Namespace, service.Name, err)
	}
}

func updateEgressSVCIptRules(svc *kapi.Service, npw *nodePortWatcher) {
	if !shouldConfigureEgressSVC(svc, npw) {
		return
	}

	npw.egressServiceInfoLock.Lock()
	defer npw.egressServiceInfoLock.Unlock()

	key := ktypes.NamespacedName{Namespace: svc.Namespace, Name: svc.Name}
	cachedEps := npw.egressServiceInfo[key]
	if cachedEps == nil {
		cachedEps = &serviceEps{sets.NewString(), sets.NewString()}
		npw.egressServiceInfo[key] = cachedEps
	}

	epSlices, err := npw.watchFactory.GetEndpointSlices(svc.Namespace, svc.Name)
	if err != nil {
		klog.V(5).Infof("No endpointslice found for egress service %s in namespace %s during update", svc.Name, svc.Namespace)
		return
	}

	v4Eps := sets.NewString() // All current v4 eps
	v6Eps := sets.NewString() // All current v6 eps
	for _, epSlice := range epSlices {
		if epSlice.AddressType == v1.AddressTypeFQDN {
			continue
		}
		epsToInsert := v4Eps
		if epSlice.AddressType == v1.AddressTypeIPv6 {
			epsToInsert = v6Eps
		}

		for _, ep := range epSlice.Endpoints {
			for _, ip := range ep.Addresses {
				if !isHostEndpoint(ip) {
					epsToInsert.Insert(ip)
				}
			}
		}
	}

	v4ToAdd := v4Eps.Difference(cachedEps.v4).UnsortedList()
	v6ToAdd := v6Eps.Difference(cachedEps.v6).UnsortedList()
	v4ToDelete := cachedEps.v4.Difference(v4Eps).UnsortedList()
	v6ToDelete := cachedEps.v6.Difference(v6Eps).UnsortedList()

	// Add rules for endpoints without one.
	addRules := egressSVCIPTRulesForEndpoints(svc, v4ToAdd, v6ToAdd)
	if err := addIptRules(addRules); err != nil {
		klog.Errorf("Failed to add iptables rules for service %s/%s: %v", svc.Namespace, svc.Name, err)
		return
	}

	// Update the cache with the added endpoints.
	cachedEps.v4.Insert(v4ToAdd...)
	cachedEps.v6.Insert(v6ToAdd...)

	// Delete rules for endpoints that should not have one.
	delRules := egressSVCIPTRulesForEndpoints(svc, v4ToDelete, v6ToDelete)
	if err := delIptRules(delRules); err != nil {
		klog.Errorf("Failed to delete iptables rules for service %s/%s: %v", svc.Namespace, svc.Name, err)
		return
	}

	// Update the cache with the deleted endpoints.
	cachedEps.v4.Delete(v4ToDelete...)
	cachedEps.v6.Delete(v6ToDelete...)
}

func delAllEgressSVCIptRules(svc *kapi.Service, npw *nodePortWatcher) {
	npw.egressServiceInfoLock.Lock()
	defer npw.egressServiceInfoLock.Unlock()
	key := ktypes.NamespacedName{Namespace: svc.Namespace, Name: svc.Name}
	allEps, found := npw.egressServiceInfo[key]
	if !found {
		return
	}

	v4ToDelete := make([]string, len(allEps.v4))
	v6ToDelete := make([]string, len(allEps.v6))
	for addr := range allEps.v4 {
		v4ToDelete = append(v4ToDelete, addr)
	}
	for addr := range allEps.v6 {
		v6ToDelete = append(v6ToDelete, addr)
	}

	delRules := egressSVCIPTRulesForEndpoints(svc, v4ToDelete, v6ToDelete)
	if err := delIptRules(delRules); err != nil {
		klog.Errorf("Failed to delete iptables rules for service %s/%s: %v", svc.Namespace, svc.Name, err)
		return
	}

	delete(npw.egressServiceInfo, key)
}

func shouldConfigureEgressSVC(svc *kapi.Service, npw *nodePortWatcher) bool {
	svcHost, _ := util.GetEgressSVCHost(svc)

	return util.HasEgressSVCAnnotation(svc) &&
		svcHost == npw.nodeName &&
		svc.Spec.Type == kapi.ServiceTypeLoadBalancer &&
		len(svc.Status.LoadBalancer.Ingress) > 0
}
