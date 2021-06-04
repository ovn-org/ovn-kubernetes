package ovn

import (
	"fmt"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	kapi "k8s.io/api/core/v1"
	"strings"
)

// createNodePortLoadBalancers is just a copy of the current node balancer code that will not be invoked during
// local gateway mode + gateway-interface of "none". So we need to still create them ourselves on for the node
// switches (since they will not be created on the GR)
// See https://github.com/openshift/ovn-kubernetes/pull/281
func createNodePortLoadBalancers(gatewayRouter, nodeName string, sctpSupport bool) error {
	// Create 3 load-balancers for north-south traffic for each gateway
	// router: UDP, TCP, SCTP
	k8sNSLbTCP, k8sNSLbUDP, k8sNSLbSCTP, err := getGatewayLoadBalancers(gatewayRouter)
	if err != nil {
		return err
	}
	protoLBMap := map[kapi.Protocol]string{
		kapi.ProtocolTCP:  k8sNSLbTCP,
		kapi.ProtocolUDP:  k8sNSLbUDP,
		kapi.ProtocolSCTP: k8sNSLbSCTP,
	}
	enabledProtos := []kapi.Protocol{kapi.ProtocolTCP, kapi.ProtocolUDP}
	if sctpSupport {
		enabledProtos = append(enabledProtos, kapi.ProtocolSCTP)
	}
	var stdout, stderr string
	for _, proto := range enabledProtos {
		if protoLBMap[proto] == "" {
			protoLBMap[proto], stderr, err = util.RunOVNNbctl("--", "create",
				"load_balancer",
				fmt.Sprintf("external_ids:%s_lb_gateway_router=%s", proto, gatewayRouter),
				fmt.Sprintf("protocol=%s", strings.ToLower(string(proto))))
			if err != nil {
				return fmt.Errorf("failed to create load balancer for gateway router %s for protocol %s: "+
					"stderr: %q, error: %v", gatewayRouter, proto, stderr, err)
			}
		}
	}

	// Local gateway mode does not use GR for ingress node port traffic, it uses mp0 instead
	if config.Gateway.Mode != config.GatewayModeLocal {
		// Add north-south load-balancers to the gateway router.
		lbString := fmt.Sprintf("%s,%s", protoLBMap[kapi.ProtocolTCP], protoLBMap[kapi.ProtocolUDP])
		if sctpSupport {
			lbString = lbString + "," + protoLBMap[kapi.ProtocolSCTP]
		}
		stdout, stderr, err = util.RunOVNNbctl("set", "logical_router", gatewayRouter, "load_balancer="+lbString)
		if err != nil {
			return fmt.Errorf("failed to set north-south load-balancers to the "+
				"gateway router %s, stdout: %q, stderr: %q, error: %v",
				gatewayRouter, stdout, stderr, err)
		}
	}
	// Also add north-south load-balancers to local switches for pod -> nodePort traffic
	stdout, stderr, err = util.RunOVNNbctl("get", "logical_switch", nodeName, "load_balancer")
	if err != nil {
		return fmt.Errorf("failed to get load-balancers on the node switch %s, stdout: %q, "+
			"stderr: %q, error: %v", nodeName, stdout, stderr, err)
	}
	for _, proto := range enabledProtos {
		if !strings.Contains(stdout, protoLBMap[proto]) {
			stdout, stderr, err = util.RunOVNNbctl("ls-lb-add", nodeName, protoLBMap[proto])
			if err != nil {
				return fmt.Errorf("failed to add north-south load-balancer %s to the "+
					"node switch %s, stdout: %q, stderr: %q, error: %v",
					protoLBMap[proto], nodeName, stdout, stderr, err)
			}
		}
	}
	return nil
}

// gatewayInitMinimal sets up the minimal configuration needed for N/S traffic to work in Local gateway mode. In this
// case we do not need any GR or join switch, just nodePort load balancers on the node switch
// See https://github.com/openshift/ovn-kubernetes/pull/281
func gatewayInitMinimal(nodeName string, l3GatewayConfig *util.L3GatewayConfig, sctpSupport bool) error {
	gatewayRouter := types.GWRouterPrefix + nodeName
	if l3GatewayConfig.NodePortEnable {
		err := createNodePortLoadBalancers(gatewayRouter, nodeName, sctpSupport)
		if err != nil {
			return err
		}
	}
	return nil
}
