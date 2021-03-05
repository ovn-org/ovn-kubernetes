package services

import (
	"fmt"
	"net"
	"strconv"

	"github.com/pkg/errors"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/acl"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/gateway"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/loadbalancer"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	v1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1beta1"
	"k8s.io/apimachinery/pkg/util/sets"

	"k8s.io/klog/v2"
)

type lbEndpoints struct {
	IPs  []string
	Port int32
}

// return the endpoints that belong to the IPFamily as a slice of IPs
func getLbEndpoints(slices []*discovery.EndpointSlice, svcPort v1.ServicePort, family v1.IPFamily) lbEndpoints {
	epsSet := sets.NewString()
	lbEps := lbEndpoints{[]string{}, 0}
	// return an empty object so the caller don't have to check for nil and can use it as an iterator
	if len(slices) == 0 {
		return lbEps
	}

	for _, slice := range slices {
		klog.V(4).Infof("Getting endpoints for slice %s", slice.Name)
		// Only return addresses that belong to the requested IP family
		if slice.AddressType != discovery.AddressType(family) {
			klog.V(4).Infof("Slice %s with different IP Family endpoints, requested: %s received: %s",
				slice.Name, slice.AddressType, family)
			continue
		}

		// build the list of endpoints in the slice
		for _, port := range slice.Ports {
			// If Service port name set it must match the name field in the endpoint
			if svcPort.Name != "" && svcPort.Name != *port.Name {
				klog.V(5).Infof("Slice %s with different Port name, requested: %s received: %s",
					slice.Name, svcPort.Name, *port.Name)
				continue
			}

			// Get the targeted port
			tgtPort := int32(svcPort.TargetPort.IntValue())
			// If this is a string, it will return 0
			// it has to match the port name
			// otherwise, it has to match the port number
			if (tgtPort == 0 && svcPort.TargetPort.String() != *port.Name) ||
				(tgtPort > 0 && tgtPort != *port.Port) {
				continue
			}

			// Skip ports that doesn't match the protocol
			if *port.Protocol != svcPort.Protocol {
				klog.V(5).Infof("Slice %s with different Port protocol, requested: %s received: %s",
					slice.Name, svcPort.Protocol, *port.Protocol)
				continue
			}

			lbEps.Port = *port.Port
			for _, endpoint := range slice.Endpoints {
				// Skip endpoints that are not ready
				if endpoint.Conditions.Ready != nil && !*endpoint.Conditions.Ready {
					klog.V(4).Infof("Slice endpoints Not Ready")
					continue
				}
				for _, ip := range endpoint.Addresses {
					klog.V(4).Infof("Adding slice %s endpoints: %v, port: %d", slice.Name, endpoint.Addresses, *port.Port)
					epsSet.Insert(ip)
				}
			}
		}
	}

	lbEps.IPs = epsSet.List()
	klog.V(4).Infof("LB Endpoints for %s are: %v on port: %d", slices[0].Labels[discovery.LabelServiceName],
		lbEps.IPs, lbEps.Port)
	return lbEps
}

func deleteVIPsFromOVN(vips sets.String, st *serviceTracker, name, namespace, clusterPortGroupUUID string) error {
	// Obtain the VIPs associated to the Service from the Service Tracker
	for vipKey := range vips {
		// the VIP is stored with the format IP:Port/Protocol
		vip, proto := splitVirtualIPKey(vipKey)
		// ClusterIP use a global load balancer per protocol
		lbID, err := loadbalancer.GetOVNKubeLoadBalancer(proto)
		if err != nil {
			klog.Errorf("Error getting OVN LoadBalancer for protocol %s", proto)
			return err
		}
		// Delete the Service VIP from OVN
		klog.Infof("Deleting service %s on namespace %s from OVN", name, namespace)
		if err := loadbalancer.DeleteLoadBalancerVIP(lbID, vip); err != nil {
			klog.Errorf("Error deleting VIP %s on OVN LoadBalancer %s", vip, lbID)
			return err
		}
		// handle reject ACLs for services without endpoints
		// eventually we have to remove this because it will
		// be implemented by OVN
		// https://github.com/ovn-org/ovn-kubernetes/pull/1887
		ip, portString, err := net.SplitHostPort(vip)
		if err != nil {
			return err
		}
		port, err := strconv.Atoi(portString)
		if err != nil {
			return err
		}
		rejectACLName := loadbalancer.GenerateACLNameForOVNCommand(lbID, ip, int32(port))
		aclID, err := acl.GetACLByName(rejectACLName)
		if err != nil {
			klog.Errorf("Error trying to get ACL for Service %s/%s: %v", name, namespace, err)
		}
		// delete the ACL if exist
		if len(aclID) > 0 {
			// remove acl
			err = acl.RemoveACLFromPortGroup(aclID, clusterPortGroupUUID)
			if err != nil {
				klog.Errorf("Error trying to remove ACL for Service %s/%s: %v", name, namespace, err)
			}
		}
		// end of reject ACL code
		// NodePort and ExternalIPs use loadbalancers in each node
		gatewayRouters, _, err := gateway.GetOvnGateways()
		if err != nil {
			return errors.Wrapf(err, "failed to retrieve OVN gateway routers")
		}
		// Configure the NodePort in each Node Gateway Router
		for _, gatewayRouter := range gatewayRouters {
			gatewayLB, err := gateway.GetGatewayLoadBalancer(gatewayRouter, proto)
			if err != nil {
				klog.Warningf("Service Sync: Gateway router %s does not have load balancer (%v)",
					gatewayRouter, err)
				// TODO: why continue? should we error and requeue and retry?
				continue
			}
			workerNode := util.GetWorkerFromGatewayRouter(gatewayRouter)
			workerLB, err := loadbalancer.GetWorkerLoadBalancer(workerNode, proto)
			if err != nil {
				klog.Errorf("Worker switch %s does not have load balancer (%v)", workerNode, err)
				continue
			}
			// Delete the Service VIP from OVN
			klog.Infof("Deleting service %s on namespace %s from OVN", name, namespace)
			for _, lb := range []string{gatewayLB, workerLB} {
				if err := loadbalancer.DeleteLoadBalancerVIP(lb, vip); err != nil {
					klog.Errorf("Error deleting VIP %s on OVN LoadBalancer %s", vip, lbID)
					return err
				}
			}
		}
		// Delete the Service VIP from the Service Tracker
		st.deleteServiceVIP(name, namespace, vip, proto)
	}
	return nil
}

// createPerNodeVIPs adds load balancers on a per node basis for GR and worker switch LBs using service IPs
func createPerNodeVIPs(svcIPs []string, protocol v1.Protocol, sourcePort int32, targetIPs []string, targetPort int32) error {
	if len(svcIPs) == 0 {
		return fmt.Errorf("unable to create per node VIPs...no service IPs provided")
	}
	klog.V(5).Infof("Creating Node VIPs - %s, %d, [%v], %d", protocol, sourcePort, targetIPs, targetPort)
	// Each gateway has a separate load-balancer for N/S traffic
	gatewayRouters, _, err := gateway.GetOvnGateways()
	if err != nil {
		return err
	}

	for _, gatewayRouter := range gatewayRouters {
		gatewayLB, err := gateway.GetGatewayLoadBalancer(gatewayRouter, protocol)
		if err != nil {
			klog.Errorf("Gateway router %s does not have load balancer (%v)",
				gatewayRouter, err)
			continue
		}
		physicalIPs, err := gateway.GetGatewayPhysicalIPs(gatewayRouter)
		if err != nil {
			klog.Errorf("Gateway router %s does not have physical ip (%v)", gatewayRouter, err)
			continue
		}

		// If self ip is in target list, we need to use special IP to allow hairpin back to host
		newTargets := util.UpdateIPsSlice(targetIPs, physicalIPs, []string{types.V4HostMasqueradeIP, types.V6HostMasqueradeIP})

		err = loadbalancer.CreateLoadBalancerVIPs(gatewayLB, svcIPs, sourcePort, newTargets, targetPort)
		if err != nil {
			klog.Errorf("Failed to create VIP in load balancer %s - %v", gatewayLB, err)
			return err
		}

		if config.Gateway.Mode == config.GatewayModeShared {
			workerNode := util.GetWorkerFromGatewayRouter(gatewayRouter)
			workerLB, err := loadbalancer.GetWorkerLoadBalancer(workerNode, protocol)
			if err != nil {
				klog.Errorf("Worker switch %s does not have load balancer (%v)", workerNode, err)
				return err
			}
			err = loadbalancer.CreateLoadBalancerVIPs(workerLB, svcIPs, sourcePort, targetIPs, targetPort)
			if err != nil {
				klog.Errorf("Failed to create VIP in load balancer %s - %v", workerLB, err)
				return err
			}
		}
	}
	return nil
}

// createPerNodePhysicalVIPs adds load balancers on a per node basis for GR and worker switch LBs using physical IPs
func createPerNodePhysicalVIPs(isIPv6 bool, protocol v1.Protocol, sourcePort int32, targetIPs []string, targetPort int32) error {
	klog.V(5).Infof("Creating Node VIPs - %s, %d, [%v], %d", protocol, sourcePort, targetIPs, targetPort)
	// Each gateway has a separate load-balancer for N/S traffic
	gatewayRouters, _, err := gateway.GetOvnGateways()
	if err != nil {
		return err
	}

	for _, gatewayRouter := range gatewayRouters {
		gatewayLB, err := gateway.GetGatewayLoadBalancer(gatewayRouter, protocol)
		if err != nil {
			klog.Errorf("Gateway router %s does not have load balancer (%v)",
				gatewayRouter, err)
			continue
		}
		physicalIPs, err := gateway.GetGatewayPhysicalIPs(gatewayRouter)
		if err != nil {
			klog.Errorf("Gateway router %s does not have physical ip (%v)", gatewayRouter, err)
			continue
		}
		// Filter only phyiscal IPs of the same family
		physicalIPs, err = util.MatchAllIPStringFamily(isIPv6, physicalIPs)
		if err != nil {
			klog.Errorf("Failed to find node physical IPs, for gateway: %s, error: %v", gatewayRouter, err)
			return err
		}

		// If self ip is in target list, we need to use special IP to allow hairpin back to host
		newTargets := util.UpdateIPsSlice(targetIPs, physicalIPs, []string{types.V4HostMasqueradeIP, types.V6HostMasqueradeIP})

		err = loadbalancer.CreateLoadBalancerVIPs(gatewayLB, physicalIPs, sourcePort, newTargets, targetPort)
		if err != nil {
			klog.Errorf("Failed to create VIP in load balancer %s - %v", gatewayLB, err)
			return err
		}

		if config.Gateway.Mode == config.GatewayModeShared {
			workerNode := util.GetWorkerFromGatewayRouter(gatewayRouter)
			workerLB, err := loadbalancer.GetWorkerLoadBalancer(workerNode, protocol)
			if err != nil {
				klog.Errorf("Worker switch %s does not have load balancer (%v)", workerNode, err)
				return err
			}
			err = loadbalancer.CreateLoadBalancerVIPs(workerLB, physicalIPs, sourcePort, targetIPs, targetPort)
			if err != nil {
				klog.Errorf("Failed to create VIP in load balancer %s - %v", workerLB, err)
				return err
			}
		}
	}
	return nil
}

// deleteNodeVIPs removes load balancers on a per node basis for GR and worker switch LBs
// if empty svcIP is provided, then the physical IPs will be used for the node
func deleteNodeVIPs(svcIPs []string, protocol v1.Protocol, sourcePort int32) error {
	klog.V(5).Infof("Searching to remove Gateway VIPs - %s, %d", protocol, sourcePort)
	gatewayRouters, _, err := gateway.GetOvnGateways()
	if err != nil {
		klog.Errorf("Error while searching for gateways: %v", err)
		return err
	}

	for _, gatewayRouter := range gatewayRouters {
		var loadBalancers []string
		gatewayLB, err := gateway.GetGatewayLoadBalancer(gatewayRouter, protocol)
		if err != nil {
			klog.Errorf("Gateway router %s does not have load balancer (%v)", gatewayRouter, err)
			continue
		}
		ips := svcIPs
		if len(ips) == 0 {
			ips, err = gateway.GetGatewayPhysicalIPs(gatewayRouter)
			if err != nil {
				klog.Errorf("Gateway router %s does not have physical ip (%v)", gatewayRouter, err)
				continue
			}
		}
		loadBalancers = append(loadBalancers, gatewayLB)
		if config.Gateway.Mode == config.GatewayModeShared {
			workerNode := util.GetWorkerFromGatewayRouter(gatewayRouter)
			workerLB, err := loadbalancer.GetWorkerLoadBalancer(workerNode, protocol)
			if err != nil {
				klog.Errorf("Worker switch %s does not have load balancer (%v)", workerNode, err)
				continue
			}
			loadBalancers = append(loadBalancers, workerLB)
		}
		for _, loadBalancer := range loadBalancers {
			for _, ip := range ips {
				// With the physical_ip:sourcePort as the VIP, delete an entry in 'load_balancer'.
				vip := util.JoinHostPortInt32(ip, sourcePort)
				klog.V(5).Infof("Removing gateway VIP: %s from load balancer: %s", vip, loadBalancer)
				if err := loadbalancer.DeleteLoadBalancerVIP(loadBalancer, vip); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// hasHostEndpoints determines if a slice of endpoints contains a host networked pod
func hasHostEndpoints(endpointIPs []string) bool {
	for _, endpointIP := range endpointIPs {
		found := false
		for _, clusterNet := range config.Default.ClusterSubnets {
			if clusterNet.CIDR.Contains(net.ParseIP(endpointIP)) {
				found = true
				break
			}
		}
		if !found {
			return true
		}
	}
	return false
}

// getNodeIPs returns the IPs for every node in the cluster for a specific IP family
func getNodeIPs(isIPv6 bool) ([]string, error) {
	nodeIPs := []string{}
	gatewayRouters, _, err := gateway.GetOvnGateways()
	if err != nil {
		return nil, err
	}
	for _, gatewayRouter := range gatewayRouters {
		physicalIPs, err := gateway.GetGatewayPhysicalIPs(gatewayRouter)
		if err != nil {
			klog.Errorf("Gateway router %s does not have physical ip (%v)", gatewayRouter, err)
			continue
		}
		physicalIPs, err = util.MatchAllIPStringFamily(isIPv6, physicalIPs)
		if err != nil {
			klog.Errorf("Failed to find node ips for gateway: %s that match IP family, error: %v",
				gatewayRouter, err)
			continue
		}
		nodeIPs = append(nodeIPs, physicalIPs...)
	}
	return nodeIPs, nil
}
