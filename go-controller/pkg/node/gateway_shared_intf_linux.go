//go:build linux
// +build linux

package node

import (
	"fmt"

	nodeipt "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/iptables"
	kapi "k8s.io/api/core/v1"
)

// addGatewayIptRules adds the necessary iptable rules for a service on the node
func addGatewayIptRules(service *kapi.Service, localEndpoints []string, svcHasLocalHostNetEndPnt bool) error {
	rules := getGatewayIPTRules(service, localEndpoints, svcHasLocalHostNetEndPnt)

	if err := insertIptRules(rules); err != nil {
		return fmt.Errorf("failed to add iptables rules for service %s/%s: %v",
			service.Namespace, service.Name, err)
	}
	return nil
}

// delGatewayIptRules removes the iptable rules for a service from the node
func delGatewayIptRules(service *kapi.Service, localEndpoints []string, svcHasLocalHostNetEndPnt bool) error {
	rules := getGatewayIPTRules(service, localEndpoints, svcHasLocalHostNetEndPnt)

	if err := nodeipt.DelRules(rules); err != nil {
		return fmt.Errorf("failed to delete iptables rules for service %s/%s: %v", service.Namespace, service.Name, err)
	}
	return nil
}
