package gateway

import (
	"fmt"
	"strings"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/pkg/errors"

	kapi "k8s.io/api/core/v1"
)

// GetOvnGateways return all created gateways.
func GetOvnGateways() ([]string, string, error) {
	out, stderr, err := util.RunOVNNbctl("--data=bare", "--no-heading",
		"--columns=name", "find",
		"logical_router",
		"options:chassis!=null")
	if err != nil {
		return nil, stderr, err
	}
	return strings.Fields(out), stderr, err
}

// GetGatewayPhysicalIP return gateway physical IP
func GetGatewayPhysicalIP(gatewayRouter string) (string, error) {
	physicalIP, stderr, err := util.RunOVNNbctl("get", "logical_router",
		gatewayRouter, "external_ids:physical_ip")
	if err != nil {
		return "", errors.Wrapf(err, "error to obtain physical IP on router %s, stderr: %v", gatewayRouter, stderr)
	}
	if physicalIP == "" {
		return "", fmt.Errorf("not physical IP found for gateway %s", gatewayRouter)
	}
	return physicalIP, nil
}

// GetGatewayPhysicalIPs return gateway physical IPs
func GetGatewayPhysicalIPs(gatewayRouter string) ([]string, error) {
	physicalIPs, _, err := util.RunOVNNbctl("get", "logical_router",
		gatewayRouter, "external_ids:physical_ips")
	if err == nil && physicalIPs != "" {
		return strings.Split(physicalIPs, ","), nil
	}

	physicalIP, err := GetGatewayPhysicalIP(gatewayRouter)
	if err != nil {
		return nil, err
	}
	return []string{physicalIP}, nil
}

// GetGatewayLoadBalancer return the gateway load balancer
func GetGatewayLoadBalancer(gatewayRouter string, protocol kapi.Protocol) (string, error) {
	externalIDKey := string(protocol) + "_lb_gateway_router"
	loadBalancer, _, err := util.RunOVNNbctl("--data=bare", "--no-heading",
		"--columns=_uuid", "find", "load_balancer",
		"external_ids:"+externalIDKey+"="+
			gatewayRouter)
	if err != nil {
		return "", err
	}
	if loadBalancer == "" {
		return "", fmt.Errorf("load balancer item in OVN DB is an empty string")
	}
	return loadBalancer, nil
}

// GetGatewayLoadBalancers find TCP, SCTP, UDP load-balancers from gateway router.
func GetGatewayLoadBalancers(gatewayRouter string) (string, string, string, error) {
	lbTCP, stderr, err := util.RunOVNNbctl("--data=bare", "--no-heading",
		"--columns=_uuid", "find", "load_balancer",
		"external_ids:TCP_lb_gateway_router="+gatewayRouter)
	if err != nil {
		return "", "", "", errors.Wrapf(err, "failed to get gateway router %q TCP "+
			"load balancer, stderr: %q", gatewayRouter, stderr)
	}

	lbUDP, stderr, err := util.RunOVNNbctl("--data=bare", "--no-heading",
		"--columns=_uuid", "find", "load_balancer",
		"external_ids:UDP_lb_gateway_router="+gatewayRouter)
	if err != nil {
		return "", "", "", errors.Wrapf(err, "failed to get gateway router %q UDP "+
			"load balancer, stderr: %q", gatewayRouter, stderr)
	}

	lbSCTP, stderr, err := util.RunOVNNbctl("--data=bare", "--no-heading",
		"--columns=_uuid", "find", "load_balancer",
		"external_ids:SCTP_lb_gateway_router="+gatewayRouter)
	if err != nil {
		return "", "", "", errors.Wrapf(err, "failed to get gateway router %q SCTP "+
			"load balancer, stderr: %q", gatewayRouter, stderr)
	}
	return lbTCP, lbUDP, lbSCTP, nil
}
