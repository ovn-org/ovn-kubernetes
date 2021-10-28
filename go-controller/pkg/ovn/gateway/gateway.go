package gateway

import (
	"fmt"
	"strings"

	"github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
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
func GetOvnGateways(nbClient client.Client) ([]string, error) {
	logicalRouterRes := []nbdb.LogicalRouter{}
	if err := nbClient.WhereCache(func(lr *nbdb.LogicalRouter) bool {
		return lr.Options["chassis"] != "null"
	}).List(&logicalRouterRes); err != nil {
		return nil, err
	}
	result := []string{}
	for _, logicalRouter := range logicalRouterRes {
		result = append(result, logicalRouter.Name)
	}
	return result, nil
}

// GetGatewayPhysicalIP return gateway physical IP
func GetGatewayPhysicalIP(nbClient client.Client, gatewayRouter string) (string, error) {
	logicalRouterRes := []nbdb.LogicalRouter{}
	if err := nbClient.WhereCache(func(lr *nbdb.LogicalRouter) bool {
		physicalIP, exists := lr.ExternalIDs["physical_ip"]
		return lr.Name == gatewayRouter && exists && physicalIP != ""
	}).List(&logicalRouterRes); err != nil {
		return "", errors.Wrapf(err, "error to obtain physical IP on router %s, err: %v", gatewayRouter, err)
	}
	if len(logicalRouterRes) == 0 {
		return "", fmt.Errorf("no physical IP found for gateway %s", gatewayRouter)
	}
	return logicalRouterRes[0].ExternalIDs["physical_ip"], nil
}

// GetGatewayPhysicalIPs return gateway physical IPs
func GetGatewayPhysicalIPs(nbClient client.Client, gatewayRouter string) ([]string, error) {
	logicalRouterRes := []nbdb.LogicalRouter{}
	if err := nbClient.WhereCache(func(lr *nbdb.LogicalRouter) bool {
		physicalIPs, exists := lr.ExternalIDs["physical_ips"]
		return lr.Name == gatewayRouter && exists && physicalIPs != ""
	}).List(&logicalRouterRes); err != nil {
		return nil, errors.Wrapf(err, "error to obtain physical IP on router %s, err: %v", gatewayRouter, err)
	}
	if len(logicalRouterRes) == 1 {
		return strings.Split(logicalRouterRes[0].ExternalIDs["physical_ips"], ","), nil
	}
	physicalIP, err := GetGatewayPhysicalIP(nbClient, gatewayRouter)
	if err != nil {
		return nil, err
	}
	return []string{physicalIP}, nil
}
