//go:build linux
// +build linux

package node

import (
	kapi "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

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
