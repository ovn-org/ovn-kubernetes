package ovn

import (
	"strings"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	kapi "k8s.io/api/core/v1"
	"k8s.io/klog"
)

func (ovn *Controller) getOvnGateways() ([]string, string, error) {
	// Return all created gateways.
	out, stderr, err := util.RunOVNNbctl("--data=bare", "--no-heading",
		"--columns=name", "find",
		"logical_router",
		"options:chassis!=null")
	return strings.Fields(out), stderr, err
}

func (ovn *Controller) getGatewayPhysicalIPs(physicalGateway string) ([]string, error) {
	physicalIPs, _, err := util.RunOVNNbctl("get", "logical_router",
		physicalGateway, "external_ids:physical_ips")
	if err == nil {
		return strings.Split(physicalIPs, ","), nil
	}

	physicalIP, _, err := util.RunOVNNbctl("get", "logical_router",
		physicalGateway, "external_ids:physical_ip")
	if err != nil {
		return nil, err
	}

	return []string{physicalIP}, nil
}

func (ovn *Controller) getGatewayLoadBalancer(physicalGateway string, protocol kapi.Protocol) (string, error) {
	externalIDKey := string(protocol) + "_lb_gateway_router"
	loadBalancer, _, err := util.RunOVNNbctl("--data=bare", "--no-heading",
		"--columns=_uuid", "find", "load_balancer",
		"external_ids:"+externalIDKey+"="+
			physicalGateway)
	if err != nil {
		return "", err
	}
	return loadBalancer, nil
}

func (ovn *Controller) createGatewayVIPs(protocol kapi.Protocol, sourcePort int32, targetIPs []string, targetPort int32) error {
	klog.V(5).Infof("Creating Gateway VIPs - %s, %d, [%v], %d", protocol, sourcePort, targetIPs, targetPort)

	// Each gateway has a separate load-balancer for N/S traffic

	physicalGateways, _, err := ovn.getOvnGateways()
	if err != nil {
		return err
	}

	for _, physicalGateway := range physicalGateways {
		loadBalancer, err := ovn.getGatewayLoadBalancer(physicalGateway, protocol)
		if err != nil {
			klog.Errorf("physical gateway %s does not have load_balancer (%v)",
				physicalGateway, err)
			continue
		}
		if loadBalancer == "" {
			continue
		}
		physicalIPs, err := ovn.getGatewayPhysicalIPs(physicalGateway)
		if err != nil {
			klog.Errorf("physical gateway %s does not have physical ip (%v)",
				physicalGateway, err)
			continue
		}
		// With the physical_ip:sourcePort as the VIP, add an entry in
		// 'load_balancer'.
		err = ovn.createLoadBalancerVIPs(loadBalancer, physicalIPs, sourcePort, targetIPs, targetPort)
		if err != nil {
			klog.Errorf("Failed to create VIP in load balancer %s - %v", loadBalancer, err)
			continue
		}
	}
	return nil
}

func (ovn *Controller) deleteGatewayVIPs(protocol kapi.Protocol, sourcePort int32) {
	klog.V(5).Infof("Searching to remove Gateway VIPs - %s, %d", protocol, sourcePort)
	physicalGateways, _, err := ovn.getOvnGateways()
	if err != nil {
		klog.Errorf("Error while searching for gateways: %v", err)
		return
	}

	for _, physicalGateway := range physicalGateways {
		loadBalancer, err := ovn.getGatewayLoadBalancer(physicalGateway, protocol)
		if err != nil {
			klog.Errorf("physical gateway %s does not have load_balancer (%v)",
				physicalGateway, err)
			continue
		}
		if loadBalancer == "" {
			continue
		}
		physicalIPs, err := ovn.getGatewayPhysicalIPs(physicalGateway)
		if err != nil {
			klog.Errorf("physical gateway %s does not have physical ip (%v)",
				physicalGateway, err)
			continue
		}
		for _, physicalIP := range physicalIPs {
			// With the physical_ip:sourcePort as the VIP, delete an entry in 'load_balancer'.
			vip := util.JoinHostPortInt32(physicalIP, sourcePort)
			klog.V(5).Infof("Removing gateway VIP: %s from loadbalancer: %s", vip, loadBalancer)
			ovn.deleteLoadBalancerVIP(loadBalancer, vip)
		}
	}
}
