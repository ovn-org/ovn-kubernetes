//go:build linux
// +build linux

package node

import (
	"fmt"
	"strings"

	nodeipt "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/iptables"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	kapi "k8s.io/api/core/v1"
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
