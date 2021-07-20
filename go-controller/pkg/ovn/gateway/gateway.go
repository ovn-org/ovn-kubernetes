package gateway

import (
	"fmt"
	"strings"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/pkg/errors"
)

const (
	// OvnGatewayLoadBalancerIds represent the OVN loadbalancers used on nodes
	OvnGatewayLoadBalancerIds = "lb_gateway_router"
)

var (
	// It is perfectly normal to have OVN GW routers to not to have LB rules. This happens
	// when NodePort is disabled for that node.
	OVNGatewayLBIsEmpty = errors.New("load balancer item in OVN DB is an empty string")
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
