package ovn

import (
	"fmt"
	"net"
	"reflect"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/acl"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/gateway"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/loadbalancer"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	kapi "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/reference"
	"k8s.io/klog/v2"
)

func (ovn *Controller) syncServices(services []interface{}) {
	// For all clusterIP in k8s, we will populate the below slice with
	// IP:port. In OVN's database those are the keys. We need to
	// have separate slice for TCP, SCTP, and UDP load-balancers (hence the dict).
	clusterServices := make(map[kapi.Protocol][]string)

	// For all nodePorts in k8s, we will populate the below slice with
	// nodePort. In OVN's database, nodeIP:nodePort is the key.
	// We have separate slice for TCP, SCTP, and UDP nodePort load-balancers.
	// We will get nodeIP separately later.
	nodeportServices := make(map[kapi.Protocol][]string)

	// For all externalIPs in k8s, we will populate the below map of slices
	// with load balancer type services based on each protocol.
	lbServices := make(map[kapi.Protocol][]string)

	// Go through the k8s services and populate 'clusterServices',
	// 'nodeportServices' and 'lbServices'
	for _, serviceInterface := range services {
		service, ok := serviceInterface.(*kapi.Service)
		if !ok {
			klog.Errorf("Spurious object in syncServices: %v", serviceInterface)
			continue
		}

		if !util.ServiceTypeHasClusterIP(service) {
			continue
		}

		if !util.IsClusterIPSet(service) {
			klog.V(5).Infof("Skipping service %s due to clusterIP = %q", service.Name, service.Spec.ClusterIP)
			continue
		}

		for _, svcPort := range service.Spec.Ports {
			if err := util.ValidatePort(svcPort.Protocol, svcPort.Port); err != nil {
				klog.Errorf("Error validating port %s: %v", svcPort.Name, err)
				continue
			}

			if util.ServiceTypeHasNodePort(service) {
				port := fmt.Sprintf("%d", svcPort.NodePort)
				nodeportServices[svcPort.Protocol] = append(nodeportServices[svcPort.Protocol], port)
			}

			key := util.JoinHostPortInt32(service.Spec.ClusterIP, svcPort.Port)
			clusterServices[svcPort.Protocol] = append(clusterServices[svcPort.Protocol], key)

			for _, extIP := range service.Spec.ExternalIPs {
				key := util.JoinHostPortInt32(extIP, svcPort.Port)
				lbServices[svcPort.Protocol] = append(lbServices[svcPort.Protocol], key)
			}
		}
	}

	// Get OVN's current cluster load balancer VIPs and delete them if they
	// are stale.
	for _, protocol := range []kapi.Protocol{kapi.ProtocolTCP, kapi.ProtocolUDP, kapi.ProtocolSCTP} {
		loadBalancer, err := ovn.getLoadBalancer(protocol)
		if err != nil {
			klog.Errorf("Failed to get load balancer for %s (%v)", kapi.Protocol(protocol), err)
			continue
		}

		loadBalancerVIPs, err := ovn.getLoadBalancerVIPs(loadBalancer)
		if err != nil {
			klog.Errorf("Failed to get load balancer vips for %s (%v)", loadBalancer, err)
			continue
		}
		for vip := range loadBalancerVIPs {
			if !stringSliceMembership(clusterServices[protocol], vip) {
				klog.V(5).Infof("Deleting stale cluster vip %s in load balancer %s", vip, loadBalancer)
				if err := ovn.deleteLoadBalancerVIP(loadBalancer, vip); err != nil {
					klog.Error(err)
				}
			}
		}
	}

	// For each gateway, remove any VIP that does not exist in
	// 'nodeportServices'.
	gateways, stderr, err := ovn.getOvnGateways()
	if err != nil {
		klog.Errorf("Failed to get ovn gateways. Not syncing nodeport stdout: %q, stderr: %q (%v)", gateways, stderr, err)
		return
	}

	for _, gateway := range gateways {
		for _, protocol := range []kapi.Protocol{kapi.ProtocolTCP, kapi.ProtocolUDP, kapi.ProtocolSCTP} {
			loadBalancer, err := ovn.getGatewayLoadBalancer(gateway, protocol)
			if err != nil {
				klog.Errorf("Gateway router %s does not have load balancer (%v)", gateway, err)
				continue
			}
			loadBalancerVIPs, err := ovn.getLoadBalancerVIPs(loadBalancer)
			if err != nil {
				klog.Errorf("Failed to get load balancer vips for %s (%v)", loadBalancer, err)
				continue
			}
			for vip := range loadBalancerVIPs {
				_, port, err := net.SplitHostPort(vip)
				if err != nil {
					// In a OVN load-balancer, we should always have vip:port.
					// In the unlikely event that it is not the case, skip it.
					klog.Errorf("Failed to split %s to vip and port (%v)", vip, err)
					continue
				}

				if !stringSliceMembership(nodeportServices[protocol], port) && !stringSliceMembership(lbServices[protocol], vip) {
					klog.V(5).Infof("Deleting stale nodeport vip %s in load balancer %s", vip, loadBalancer)
					if err := ovn.deleteLoadBalancerVIP(loadBalancer, vip); err != nil {
						klog.Error(err)
					}
				}
			}
		}
	}
	// Remove existing reject rules. They are not used anymore
	// given the introduction of idling loadbalancers
	err = acl.PurgeRejectRules(ovn.clusterPortGroupUUID)
	if err != nil {
		klog.Errorf("Failed to purge existing reject rules: %v", err)
	}
}

func (ovn *Controller) createService(service *kapi.Service) error {
	klog.Infof("Creating service %s", service.Name)
	if !util.IsClusterIPSet(service) {
		klog.V(5).Infof("Skipping service create: No cluster IP for service %s found", service.Name)
		return nil
	} else if len(service.Spec.Ports) == 0 {
		klog.V(5).Info("Skipping service create: No Ports specified for service")
		return nil
	}

	// We can end up in a situation where the endpoint creation is triggered before the service creation.
	// NOTE: we can also end up in a situation where a service matching no pods is created. Such a service still has an endpoint, but with no subsets.
	ep, err := ovn.watchFactory.GetEndpoint(service.Namespace, service.Name)
	if err == nil {
		if len(ep.Subsets) > 0 {
			klog.V(5).Infof("service: %s has endpoint, will create load balancer VIPs", service.Name)
		} else {
			klog.V(5).Infof("service: %s has empty endpoint", service.Name)
			// No need to go further. AddEndpoints will add the service to the idling LB
			if err := ovn.AddEndpoints(ep, true); err != nil {
				return err
			}
			return nil
		}
	}

	for _, svcPort := range service.Spec.Ports {
		var port int32
		if util.ServiceTypeHasNodePort(service) {
			port = svcPort.NodePort
		} else {
			port = svcPort.Port
		}

		if err := util.ValidatePort(svcPort.Protocol, port); err != nil {
			klog.Errorf("Error validating port %s: %v", svcPort.Name, err)
			continue
		}

		if !ovn.SCTPSupport && svcPort.Protocol == kapi.ProtocolSCTP {
			ref, err := reference.GetReference(scheme.Scheme, service)
			if err != nil {
				klog.Errorf("Could not get reference for pod %v: %v\n", service.Name, err)
			} else {
				ovn.recorder.Event(ref, kapi.EventTypeWarning, "Unsupported protocol error", "SCTP protocol is unsupported by this version of OVN")
			}
			return fmt.Errorf("invalid service port %s: SCTP is unsupported by this version of OVN", svcPort.Name)
		}

		if util.ServiceTypeHasNodePort(service) {
			// Each gateway has a separate load-balancer for N/S traffic

			gatewayRouters, _, err := ovn.getOvnGateways()
			if err != nil {
				return err
			}

			for _, gatewayRouter := range gatewayRouters {
				loadBalancer, err := ovn.getGatewayLoadBalancer(gatewayRouter, svcPort.Protocol)
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
					// With the physical_ip:port as the VIP, add an entry in
					// 'load balancer'.
					vip := util.JoinHostPortInt32(physicalIP, port)
					// Skip creating LB if endpoints watcher already did it
					if _, hasEps := ovn.getServiceLBInfo(loadBalancer, vip); hasEps {
						klog.V(5).Infof("Load balancer already configured for %s, %s", loadBalancer, vip)
					} else if ep != nil {
						if err := ovn.AddEndpoints(ep, true); err != nil {
							return err
						}
					}
				}
			}
		}
		if util.ServiceTypeHasClusterIP(service) {
			loadBalancer, err := ovn.getLoadBalancer(svcPort.Protocol)
			if err != nil {
				klog.Errorf("Failed to get load balancer for %s (%v)", svcPort.Protocol, err)
				break
			}
			vip := util.JoinHostPortInt32(service.Spec.ClusterIP, svcPort.Port)
			// Skip creating LB if endpoints watcher already did it
			if _, hasEps := ovn.getServiceLBInfo(loadBalancer, vip); hasEps {
				klog.V(5).Infof("Load balancer already configured for %s, %s", loadBalancer, vip)
			} else if ep != nil {
				if err := ovn.AddEndpoints(ep, true); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (ovn *Controller) updateService(oldSvc, newSvc *kapi.Service) error {
	if reflect.DeepEqual(newSvc.Spec.Ports, oldSvc.Spec.Ports) &&
		reflect.DeepEqual(newSvc.Spec.ExternalIPs, oldSvc.Spec.ExternalIPs) &&
		reflect.DeepEqual(newSvc.Spec.ClusterIP, oldSvc.Spec.ClusterIP) &&
		reflect.DeepEqual(newSvc.Spec.Type, oldSvc.Spec.Type) &&
		reflect.DeepEqual(newSvc.Status.LoadBalancer.Ingress, oldSvc.Status.LoadBalancer.Ingress) {
		klog.V(5).Infof("Skipping service update for: %s as change does not apply to any of .Spec.Ports, "+
			".Spec.ExternalIP, .Spec.ClusterIP, .Spec.Type, .Status.LoadBalancer.Ingress", newSvc.Name)
		return nil
	}

	klog.V(5).Infof("Updating service from: %v to: %v", oldSvc, newSvc)

	ovn.deleteService(oldSvc)
	return ovn.createService(newSvc)
}

func (ovn *Controller) deleteService(service *kapi.Service) {
	ovn.deleteServiceFromBalancers(service)
	ovn.deleteServiceFromIdlingBalancer(service)
}

func (ovn *Controller) deleteServiceFromIdlingBalancer(service *kapi.Service) {
	if !config.Kubernetes.OVNEmptyLbEvents {
		return
	}
	if !util.IsClusterIPSet(service) {
		return
	}
	klog.Infof("Deleting service %s from idling balancer", service.Name)
	for _, svcPort := range service.Spec.Ports {
		lb, err := loadbalancer.GetOVNKubeIdlingLoadBalancer(svcPort.Protocol)
		if err != nil {
			klog.Errorf("Failed to get idling loadbalancer for protocol %s %v", svcPort.Protocol, err)
			continue
		}

		if util.ServiceTypeHasNodePort(service) {
			nodeIPs, err := ovn.getNodeportIPs()
			if err != nil {
				klog.Error(err)
			}
			for _, nodeIP := range nodeIPs {
				vip := util.JoinHostPortInt32(nodeIP, svcPort.NodePort)
				if err := ovn.deleteLoadBalancerVIP(lb, vip); err != nil {
					klog.Error(err)
				}
			}
		}
		if util.ServiceTypeHasClusterIP(service) {
			vip := util.JoinHostPortInt32(service.Spec.ClusterIP, svcPort.Port)
			if err := ovn.deleteLoadBalancerVIP(lb, vip); err != nil {
				klog.Error(err)
			}
			for _, ing := range service.Status.LoadBalancer.Ingress {
				if ing.IP == "" {
					continue
				}
				vip := util.JoinHostPortInt32(ing.IP, svcPort.Port)
				if err := ovn.deleteLoadBalancerVIP(lb, vip); err != nil {
					klog.Error(err)
				}
			}
			for _, extIP := range service.Spec.ExternalIPs {
				vip := util.JoinHostPortInt32(extIP, svcPort.Port)
				if err := ovn.deleteLoadBalancerVIP(lb, vip); err != nil {
					klog.Error(err)
				}
			}
		}
	}
}

func (ovn *Controller) deleteServiceFromBalancers(service *kapi.Service) {
	klog.Infof("Deleting service %s", service.Name)
	if !util.IsClusterIPSet(service) {
		return
	}
	for _, svcPort := range service.Spec.Ports {
		var port int32
		if util.ServiceTypeHasNodePort(service) {
			port = svcPort.NodePort
		} else {
			port = svcPort.Port
		}

		if err := util.ValidatePort(svcPort.Protocol, port); err != nil {
			klog.Errorf("Skipping delete for service port %s: %v", svcPort.Name, err)
			continue
		}
		if util.ServiceTypeHasNodePort(service) {
			// Delete the 'NodePort' service from a load balancer instantiated in gateways.
			ovn.deleteNodeVIPs(nil, svcPort.Protocol, port)
		}
		if util.ServiceTypeHasClusterIP(service) {
			loadBalancer, err := ovn.getLoadBalancer(svcPort.Protocol)
			if err != nil {
				klog.Errorf("Failed to get load balancer for %s (%v)", svcPort.Protocol, err)
				continue
			}
			vip := util.JoinHostPortInt32(service.Spec.ClusterIP, svcPort.Port)
			if err := ovn.deleteLoadBalancerVIP(loadBalancer, vip); err != nil {
				klog.Error(err)
			}
			ovn.deleteNodeVIPs([]string{service.Spec.ClusterIP}, svcPort.Protocol, svcPort.Port)
			// Cloud load balancers
			if err := ovn.deleteIngressVIPs(service, svcPort); err != nil {
				klog.Error(err)
			}
			if err := ovn.deleteExternalVIPs(service, svcPort); err != nil {
				klog.Error(err)
			}
		}
	}
}

func (ovn *Controller) addServiceToIdlingBalancer(service *kapi.Service) {
	// when adding to idling, it's because the service has no endpoints
	targets := make([]string, 0)
	for _, svcPort := range service.Spec.Ports {
		var port int32
		if util.ServiceTypeHasNodePort(service) {
			port = svcPort.NodePort
		} else {
			port = svcPort.Port
		}
		lb, err := loadbalancer.GetOVNKubeIdlingLoadBalancer(svcPort.Protocol)
		if err != nil {
			klog.Errorf("Failed to get idling loadbalancer for protocol %s %v", svcPort.Protocol, err)
			continue
		}

		if err := util.ValidatePort(svcPort.Protocol, port); err != nil {
			klog.Errorf("Skipping delete for service port %s: %v", svcPort.Name, err)
			continue
		}
		if util.ServiceTypeHasNodePort(service) {
			nodeIPs, err := ovn.getNodeportIPs()
			if err != nil {
				klog.Error(err)
			}
			for _, nodeIP := range nodeIPs {
				vip := util.JoinHostPortInt32(nodeIP, port)
				if err := loadbalancer.UpdateLoadBalancer(lb, vip, targets); err != nil {
					klog.Error(err)
				}
			}
		}
		if util.ServiceTypeHasClusterIP(service) {
			vip := util.JoinHostPortInt32(service.Spec.ClusterIP, svcPort.Port)
			if err := loadbalancer.UpdateLoadBalancer(lb, vip, targets); err != nil {
				klog.Error(err)
			}
			for _, ing := range service.Status.LoadBalancer.Ingress {
				if ing.IP == "" {
					continue
				}
				vip := util.JoinHostPortInt32(ing.IP, svcPort.Port)
				if err := loadbalancer.UpdateLoadBalancer(lb, vip, targets); err != nil {
					klog.Error(err)
				}
			}
			for _, extIP := range service.Spec.ExternalIPs {
				vip := util.JoinHostPortInt32(extIP, svcPort.Port)
				if err := loadbalancer.UpdateLoadBalancer(lb, vip, targets); err != nil {
					klog.Error(err)
				}
			}
		}
	}
}

// SVC can be of types 1. clusterIP, 2. NodePort, 3. LoadBalancer,
// or 4.ExternalIP
// TODO adjust for upstream patch when it lands:
// https://bugzilla.redhat.com/show_bug.cgi?id=1908540
func getSvcVips(service *kapi.Service) []net.IP {
	ips := make([]net.IP, 0)

	if util.ServiceTypeHasNodePort(service) {
		gatewayRouters, _, err := gateway.GetOvnGateways()
		if err != nil {
			klog.Errorf("Cannot get gateways: %s", err)
		}
		for _, gatewayRouter := range gatewayRouters {
			// VIPs would be the physical IPS of the GRs(IPs of the node) in this case
			physicalIPs, err := gateway.GetGatewayPhysicalIPs(gatewayRouter)
			if err != nil {
				klog.Errorf("Unable to get gateway router %s physical ip, error: %v", gatewayRouter, err)
				continue
			}

			for _, physicalIP := range physicalIPs {
				ip := net.ParseIP(physicalIP)
				if ip == nil {
					klog.Errorf("Failed to parse pod IP %q", physicalIP)
					continue
				}
				ips = append(ips, ip)
			}
		}
	}
	if util.ServiceTypeHasClusterIP(service) {
		if util.IsClusterIPSet(service) {
			ip := net.ParseIP(service.Spec.ClusterIP)
			if ip == nil {
				klog.Errorf("Failed to parse pod IP %q", service.Spec.ClusterIP)
			}
			ips = append(ips, ip)
		}

		for _, ing := range service.Status.LoadBalancer.Ingress {
			ip := net.ParseIP(ing.IP)
			if ip == nil {
				klog.Errorf("Failed to parse pod IP %q", ing)
				continue
			}
			klog.V(5).Infof("Adding ingress IPs from Service: %s to VIP set", service.Name)
			ips = append(ips, ip)
		}

		if len(service.Spec.ExternalIPs) > 0 {
			for _, extIP := range service.Spec.ExternalIPs {
				ip := net.ParseIP(extIP)
				if ip == nil {
					klog.Errorf("Failed to parse pod IP %q", extIP)
					continue
				}
				klog.V(5).Infof("Adding external IPs from Service: %s to VIP set", service.Name)
				ips = append(ips, ip)
			}
		}
	}
	if len(ips) == 0 {
		klog.V(5).Infof("Service has no VIPs")
		return nil
	}
	return ips
}
