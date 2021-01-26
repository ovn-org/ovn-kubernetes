package gateway

import (
	"fmt"
	"strings"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/pkg/errors"

	kapi "k8s.io/api/core/v1"
)

const (
	// OvnGatewayLoadBalancerIds represent the OVN loadbalancers used for NodePorts
	// Default behaviour to reject traffic for VIPs without backends
	OvnGatewayLoadBalancerIds = "lb_gateway_router"

	// OvnGatewayIdlingIds represent the OVN loadbalancers used for NodePorts
	// Default behaviour to send an event and drop the packet for VIPs without backends
	OvnGatewayIdlingIds = "lb_gateway_idling"
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

// createGatewayLoadBalancer creates a  gateway load balancer
func createGatewayLoadBalancer(gatewayRouter string, protocol kapi.Protocol, idkey string) (string, error) {
	externalIDKey := fmt.Sprintf("%s_%s", string(protocol), idkey)
	reject := true
	if idkey == OvnGatewayIdlingIds {
		reject = false
	}
	options := fmt.Sprintf("options:reject=%t", reject)
	loadBalancer, stderr, err := util.RunOVNNbctl("--", "create",
		"load_balancer",
		fmt.Sprintf("external_ids:%s=%s", externalIDKey, gatewayRouter),
		fmt.Sprintf("protocol=%s", strings.ToLower(string(protocol))),
		options,
	)
	if err != nil {
		return "", errors.Wrapf(err, "error creating loadbalancer for protocol %s: %v", protocol, stderr)
	}

	return loadBalancer, nil
}

// CreateGatewayLoadBalancer creates a gateway load balancer if it doesn´t exist and returns its uuid
func CreateGatewayLoadBalancer(gatewayRouter string, protocol kapi.Protocol, idkey string) (string, error) {
	var lbUUID string
	lbUUID, err := GetGatewayLoadBalancer(gatewayRouter, protocol, idkey)
	if err != nil && err.Error() != "load balancer item in OVN DB is an empty string" {
		return "", err
	}
	// create the load balancer if it doesn't exist yet
	if lbUUID == "" {
		lbUUID, err = createGatewayLoadBalancer(gatewayRouter, protocol, idkey)
		if err != nil {
			return "", errors.Wrapf(err, "Failed to create OVN load balancer for protocol %s", protocol)
		}
	}

	return lbUUID, nil
}

// GetGatewayLoadBalancer return the gateway load balancer
func GetGatewayLoadBalancer(gatewayRouter string, protocol kapi.Protocol, idkey string) (string, error) {
	externalIDKey := fmt.Sprintf("%s_%s", string(protocol), idkey)
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
	protoLBMap := map[kapi.Protocol]string{}
	enabledProtos := []kapi.Protocol{kapi.ProtocolTCP, kapi.ProtocolUDP, kapi.ProtocolSCTP}
	for _, protocol := range enabledProtos {
		lbID, err := GetGatewayLoadBalancer(gatewayRouter, protocol, OvnGatewayLoadBalancerIds)
		if err != nil && err.Error() != "load balancer item in OVN DB is an empty string" {
			return "", "", "", err
		}
		protoLBMap[protocol] = lbID
	}

	return protoLBMap[kapi.ProtocolTCP], protoLBMap[kapi.ProtocolUDP], protoLBMap[kapi.ProtocolSCTP], nil
}
