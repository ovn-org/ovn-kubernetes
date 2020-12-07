package ovn

import (
	"fmt"
	"net"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/gateway"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	kapi "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

func (ovn *Controller) getOvnGateways() ([]string, string, error) {
	return gateway.GetOvnGateways()
}

func (ovn *Controller) getGatewayPhysicalIPs(gatewayRouter string) ([]string, error) {
	return gateway.GetGatewayPhysicalIPs(gatewayRouter)
}

func (ovn *Controller) getGatewayLoadBalancer(gatewayRouter string, protocol kapi.Protocol) (string, error) {
	return gateway.GetGatewayLoadBalancer(gatewayRouter, protocol)
}

// getGatewayLoadBalancers find TCP, SCTP, UDP load-balancers from gateway router.
func getGatewayLoadBalancers(gatewayRouter string) (string, string, string, error) {
	return gateway.GetGatewayLoadBalancers(gatewayRouter)
}

func (ovn *Controller) createGatewayVIPs(protocol kapi.Protocol, sourcePort int32, targetIPs []string, targetPort int32) error {
	klog.V(5).Infof("Creating Gateway VIPs - %s, %d, [%v], %d", protocol, sourcePort, targetIPs, targetPort)
	// Each gateway has a separate load-balancer for N/S traffic
	gatewayRouters, _, err := ovn.getOvnGateways()
	if err != nil {
		return err
	}

	for _, gatewayRouter := range gatewayRouters {
		loadBalancer, err := ovn.getGatewayLoadBalancer(gatewayRouter, protocol)
		if err != nil {
			klog.Errorf("Gateway router %s does not have load balancer (%v)",
				gatewayRouter, err)
			continue
		}
		physicalIPs, err := ovn.getGatewayPhysicalIPs(gatewayRouter)
		if err != nil {
			klog.Errorf("Gateway router %s does not have physical ip (%v)", gatewayRouter, err)
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
	gatewayRouters, _, err := ovn.getOvnGateways()
	if err != nil {
		klog.Errorf("Error while searching for gateways: %v", err)
		return
	}

	for _, gatewayRouter := range gatewayRouters {
		loadBalancer, err := ovn.getGatewayLoadBalancer(gatewayRouter, protocol)
		if err != nil {
			klog.Errorf("Gateway router %s does not have load balancer (%v)", gatewayRouter, err)
			continue
		}
		physicalIPs, err := ovn.getGatewayPhysicalIPs(gatewayRouter)
		if err != nil {
			klog.Errorf("Gateway router %s does not have physical ip (%v)", gatewayRouter, err)
			continue
		}
		for _, physicalIP := range physicalIPs {
			// With the physical_ip:sourcePort as the VIP, delete an entry in 'load_balancer'.
			vip := util.JoinHostPortInt32(physicalIP, sourcePort)
			klog.V(5).Infof("Removing gateway VIP: %s from load balancer: %s", vip, loadBalancer)
			if err := ovn.deleteLoadBalancerVIP(loadBalancer, vip); err != nil {
				klog.Error(err)
			}
		}
	}
}

func (ovn *Controller) deleteExternalVIPs(service *kapi.Service, svcPort kapi.ServicePort) error {
	if len(service.Spec.ExternalIPs) == 0 {
		return nil
	}
	gateways, stderr, err := ovn.getOvnGateways()
	if err != nil {
		return fmt.Errorf("error: failed to get ovn gateways, stderr: %s, err: %v)", stderr, err)
	}
	for _, extIP := range service.Spec.ExternalIPs {
		klog.V(5).Infof("Searching to remove ExternalIP VIPs - %s, %d", svcPort.Protocol, svcPort.Port)
		for _, gateway := range gateways {
			loadBalancer, err := ovn.getGatewayLoadBalancer(gateway, svcPort.Protocol)
			if err != nil {
				klog.Errorf("Gateway router: %s does not have load balancer, err: %v", gateway, err)
				continue
			}
			vip := util.JoinHostPortInt32(extIP, svcPort.Port)
			if err := ovn.deleteLoadBalancerVIP(loadBalancer, vip); err != nil {
				klog.Error(err)
			}
		}
	}
	return nil
}

func (ovn *Controller) deleteIngressVIPs(service *kapi.Service, svcPort kapi.ServicePort) error {
	gateways, stderr, err := ovn.getOvnGateways()
	if err != nil {
		return fmt.Errorf("error: failed to get ovn gateways, stderr: %s, err: %v)", stderr, err)
	}
	for _, ing := range service.Status.LoadBalancer.Ingress {
		if ing.IP == "" {
			continue
		}
		klog.V(5).Infof("Searching to remove Ingress VIPs - %s, %d", svcPort.Protocol, svcPort.Port)
		ingressVIP := util.JoinHostPortInt32(ing.IP, svcPort.Port)
		for _, gw := range gateways {
			loadBalancer, err := ovn.getGatewayLoadBalancer(gw, svcPort.Protocol)
			if err != nil {
				klog.Errorf("Gateway router %s does not have load balancer (%v)", gw, err)
				continue
			}
			if err := ovn.deleteLoadBalancerVIP(loadBalancer, ingressVIP); err != nil {
				klog.Error(err)
			}
		}
	}
	return nil
}

// getJoinLRPAddresses check if IPs of gateway logical router port are within the join switch IP range, and return them if true.
func (oc *Controller) getJoinLRPAddresses(nodeName string) []*net.IPNet {
	// try to get the IPs from the logical router port
	gwLRPIPs := []*net.IPNet{}
	gwLrpName := types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + nodeName
	joinSubnets := oc.joinSwIPManager.lsm.GetSwitchSubnets(nodeName)
	ifAddrs, err := util.GetLRPAddrs(gwLrpName)
	if err == nil {
		for _, ifAddr := range ifAddrs {
			for _, subnet := range joinSubnets {
				if subnet.Contains(ifAddr.IP) {
					gwLRPIPs = append(gwLRPIPs, &net.IPNet{IP: ifAddr.IP, Mask: subnet.Mask})
					break
				}
			}
		}
	}

	if len(gwLRPIPs) != len(joinSubnets) {
		var errStr string
		if len(gwLRPIPs) == 0 {
			errStr = fmt.Sprintf("Failed to get IPs for logical router port %s", gwLrpName)
		} else {
			errStr = fmt.Sprintf("Invalid IPs %s (possibly not in the range of subnet %s)",
				util.JoinIPNetIPs(gwLRPIPs, " "), util.JoinIPNetIPs(joinSubnets, " "))
		}
		klog.Warningf("%s for logical router port %s", errStr, gwLrpName)
		return []*net.IPNet{}
	}
	return gwLRPIPs
}
