package ovn

import (
	"fmt"
	"net"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	kapi "k8s.io/api/core/v1"
	"k8s.io/klog"
)

func (ovn *Controller) syncServices(services []interface{}) {
	// For all clusterIP in k8s, we will populate the below slice with
	// IP:port. In OVN's database those are the keys. We need to
	// have separate slice for TCP and UDP load-balancers (hence the dict).
	clusterServices := make(map[string][]string)

	// For all nodePorts in k8s, we will populate the below slice with
	// nodePort. In OVN's database, nodeIP:nodePort is the key.
	// We have separate slice for TCP and UDP nodePort load-balancers.
	// We will get nodeIP separately later.
	nodeportServices := make(map[string][]string)

	// For all externalIPs in k8s, we will populate the below map of slices
	// with loadbalancer type services based on each protocol.
	lbServices := make(map[string][]string)

	// Go through the k8s services and populate 'clusterServices',
	// 'nodeportServices' and 'lbServices'
	for _, serviceInterface := range services {
		service, ok := serviceInterface.(*kapi.Service)
		if !ok {
			klog.Errorf("Spurious object in syncServices: %v",
				serviceInterface)
			continue
		}

		if !util.ServiceTypeHasClusterIP(service) {
			continue
		}

		if !util.IsClusterIPSet(service) {
			klog.V(5).Infof("Skipping service %s due to clusterIP = %q",
				service.Name, service.Spec.ClusterIP)
			continue
		}

		for _, svcPort := range service.Spec.Ports {
			protocol := svcPort.Protocol
			if protocol == "" || (protocol != TCP && protocol != UDP) {
				protocol = TCP
			}

			if util.ServiceTypeHasNodePort(service) {
				port := fmt.Sprintf("%d", svcPort.NodePort)
				if protocol == TCP {
					nodeportServices[TCP] = append(nodeportServices[TCP], port)
				} else {
					nodeportServices[UDP] = append(nodeportServices[UDP], port)
				}
			}

			if svcPort.Port == 0 {
				continue
			}

			key := util.JoinHostPortInt32(service.Spec.ClusterIP, svcPort.Port)
			if protocol == TCP {
				clusterServices[TCP] = append(clusterServices[TCP], key)
			} else {
				clusterServices[UDP] = append(clusterServices[UDP], key)
			}

			if len(service.Spec.ExternalIPs) == 0 {
				continue
			}
			for _, extIP := range service.Spec.ExternalIPs {
				key := util.JoinHostPortInt32(extIP, svcPort.Port)
				if protocol == TCP {
					lbServices[TCP] = append(lbServices[TCP], key)
				} else {
					lbServices[UDP] = append(lbServices[UDP], key)
				}
			}
		}
	}

	// Get OVN's current cluster load-balancer VIPs and delete them if they
	// are stale.
	for _, protocol := range []string{TCP, UDP} {
		loadBalancer, err := ovn.getLoadBalancer(kapi.Protocol(protocol))
		if err != nil {
			klog.Errorf("Failed to get load-balancer for %s (%v)",
				kapi.Protocol(protocol), err)
			continue
		}

		loadBalancerVIPS, err := ovn.getLoadBalancerVIPS(loadBalancer)
		if err != nil {
			klog.Errorf("failed to get load-balancer vips for %s (%v)",
				loadBalancer, err)
			continue
		}
		if loadBalancerVIPS == nil {
			continue
		}

		for vip := range loadBalancerVIPS {
			if !stringSliceMembership(clusterServices[protocol], vip) {
				klog.V(5).Infof("Deleting stale cluster vip %s in "+
					"loadbalancer %s", vip, loadBalancer)
				ovn.deleteLoadBalancerVIP(loadBalancer, vip)
			}
		}
	}

	// For each gateway, remove any VIP that does not exist in
	// 'nodeportServices'.
	gateways, stderr, err := ovn.getOvnGateways()
	if err != nil {
		klog.Errorf("failed to get ovn gateways. Not syncing nodeport"+
			"stdout: %q, stderr: %q (%v)", gateways, stderr, err)
		return
	}

	for _, gateway := range gateways {
		for _, protocol := range []kapi.Protocol{kapi.ProtocolTCP, kapi.ProtocolUDP} {
			loadBalancer, err := ovn.getGatewayLoadBalancer(gateway, protocol)
			if err != nil {
				klog.Errorf("physical gateway %s does not have "+
					"load_balancer (%v)", gateway, err)
				continue
			}
			if loadBalancer == "" {
				continue
			}

			loadBalancerVIPS, err := ovn.getLoadBalancerVIPS(loadBalancer)
			if err != nil {
				klog.Errorf("failed to get load-balancer vips for %s (%v)",
					loadBalancer, err)
				continue
			}
			if loadBalancerVIPS == nil {
				continue
			}

			for vip := range loadBalancerVIPS {
				_, port, err := net.SplitHostPort(vip)
				if err != nil {
					// In a OVN load-balancer, we should always have vip:port.
					// In the unlikely event that it is not the case, skip it.
					klog.Errorf("failed to split %s to vip and port (%v)",
						vip, err)
					continue
				}

				if !stringSliceMembership(nodeportServices[string(protocol)], port) && !stringSliceMembership(lbServices[string(protocol)], vip) {
					klog.V(5).Infof("Deleting stale nodeport vip %s in "+
						"loadbalancer %s", vip, loadBalancer)
					ovn.deleteLoadBalancerVIP(loadBalancer, vip)
				}
			}
		}
	}
}

func (ovn *Controller) createService(service *kapi.Service) error {
	klog.V(5).Infof("Creating service %s", service.Name)
	if !util.IsClusterIPSet(service) {
		klog.V(5).Infof("Skipping service create: No cluster IP for service %s found", service.Name)
		return nil
	} else if len(service.Spec.Ports) == 0 {
		klog.V(5).Info("Skipping service create: No Ports specified for service")
		return nil
	}

	for _, svcPort := range service.Spec.Ports {
		var port int32
		if util.ServiceTypeHasNodePort(service) {
			port = svcPort.NodePort
		} else {
			port = svcPort.Port
		}
		if port == 0 {
			continue
		}

		protocol := svcPort.Protocol
		if protocol == "" || (protocol != TCP && protocol != UDP) {
			protocol = TCP
		}

		if util.ServiceTypeHasNodePort(service) {
			// Each gateway has a separate load-balancer for N/S traffic

			physicalGateways, _, err := ovn.getOvnGateways()
			if err != nil {
				return err
			}

			for _, physicalGateway := range physicalGateways {
				loadBalancer, err := ovn.getGatewayLoadBalancer(physicalGateway, protocol)
				if err != nil {
					klog.Errorf("physical gateway %s does not have load_balancer "+
						"(%v)", physicalGateway, err)
					continue
				}
				if loadBalancer == "" {
					continue
				}
				physicalIP, err := ovn.getGatewayPhysicalIP(physicalGateway)
				if err != nil {
					klog.Errorf("physical gateway %s does not have physical ip (%v)",
						physicalGateway, err)
					continue
				}
				// With the physical_ip:port as the VIP, add an entry in
				// 'load_balancer'.
				vip := util.JoinHostPortInt32(physicalIP, port)
				// Skip creating LB if endpoints watcher already did it
				if _, hasEps := ovn.getServiceLBInfo(loadBalancer, vip); hasEps {
					klog.V(5).Infof("Load Balancer already configured for %s, %s", loadBalancer, vip)
				} else if ovn.svcQualifiesForReject(service) {
					aclUUID, err := ovn.createLoadBalancerRejectACL(loadBalancer, physicalIP, port, protocol)
					if err != nil {
						return fmt.Errorf("failed to create service ACL")
					}
					klog.V(5).Infof("Service Reject ACL created for physical gateway: %s", aclUUID)
				}
			}
		}
		if util.ServiceTypeHasClusterIP(service) {
			loadBalancer, err := ovn.getLoadBalancer(protocol)
			if err != nil {
				klog.Errorf("Failed to get load-balancer for %s (%v)",
					protocol, err)
				break
			}
			if ovn.svcQualifiesForReject(service) {
				vip := util.JoinHostPortInt32(service.Spec.ClusterIP, svcPort.Port)
				// Skip creating LB if endpoints watcher already did it
				if _, hasEps := ovn.getServiceLBInfo(loadBalancer, vip); hasEps {
					klog.V(5).Infof("Load Balancer already configured for %s, %s", loadBalancer, vip)
				} else {
					aclUUID, err := ovn.createLoadBalancerRejectACL(loadBalancer, service.Spec.ClusterIP,
						svcPort.Port, protocol)
					if err != nil {
						return fmt.Errorf("failed to create service ACL")
					} else {
						klog.V(5).Infof("Service Reject ACL created for cluster IP: %s", aclUUID)
					}
				}
				for _, extIP := range service.Spec.ExternalIPs {
					exLoadBalancer := ovn.getDefaultGatewayLoadBalancer(svcPort.Protocol)
					if exLoadBalancer == "" {
						klog.Warningf("No default gateway found for protocol %s\n\tNote: 'nodeport'"+
							"flag needs to be enabled for default gateway", svcPort.Protocol)
						continue
					}
					vip := util.JoinHostPortInt32(extIP, svcPort.Port)
					// Skip creating LB if endpoints watcher already did it
					if _, hasEps := ovn.getServiceLBInfo(loadBalancer, vip); hasEps {
						klog.V(5).Infof("Load Balancer already configured for %s, %s", loadBalancer, vip)
					} else {
						aclUUID, err := ovn.createLoadBalancerRejectACL(exLoadBalancer, extIP, svcPort.Port, protocol)
						if err != nil {
							return fmt.Errorf("failed to create service ACL for external IP")
						} else {
							klog.V(5).Infof("Service Reject ACL created for external IP: %s", aclUUID)
						}
					}
				}
			}
		}
	}
	return nil
}

func (ovn *Controller) updateService(oldSvc, newSvc *kapi.Service) error {
	klog.V(5).Infof("Updating service is a noop: %s", newSvc.Name)
	// Service update needs to check for port change, protocol, etc and update the ACLs, or and OVN LBs
	// This only really matters when a service is updated and has no endpoints. If the service has endpoints
	// the endpoints watcher will handle the OVN config
	// TODO (trozet) implement this
	return nil
}

func (ovn *Controller) deleteService(service *kapi.Service) {
	if !util.IsClusterIPSet(service) || len(service.Spec.Ports) == 0 {
		return
	}

	ips := make([]string, 0)

	for _, svcPort := range service.Spec.Ports {
		var port int32
		if util.ServiceTypeHasNodePort(service) {
			port = svcPort.NodePort
		} else {
			port = svcPort.Port
		}
		if port == 0 {
			continue
		}

		protocol := svcPort.Protocol
		if protocol == "" || (protocol != TCP && protocol != UDP) {
			protocol = TCP
		}

		// targetPort can be anything, the deletion logic does not use it
		var targetPort int32
		if util.ServiceTypeHasNodePort(service) {
			// Delete the 'NodePort' service from a load-balancer instantiated in gateways.
			ovn.deleteGatewaysVIP(protocol, port)
		}
		if util.ServiceTypeHasClusterIP(service) {
			loadBalancer, err := ovn.getLoadBalancer(protocol)
			if err != nil {
				klog.Errorf("Failed to get load-balancer for %s (%v)",
					protocol, err)
				break
			}
			vip := util.JoinHostPortInt32(service.Spec.ClusterIP, svcPort.Port)
			ovn.deleteLoadBalancerVIP(loadBalancer, vip)
			ovn.handleExternalIPs(service, svcPort, ips, targetPort, true)
		}
	}
}

// svcQualifiesForReject determines if a service should have a reject ACL on it when it has no endpoints
// The reject ACL is only applied to terminate incoming connections immediately when idling is not used
// or OVNEmptyLbEvents are not enabled. When idilng or empty LB events are enabled, we want to ensure we
// receive these packets and not reject them.
func (ovn *Controller) svcQualifiesForReject(service *kapi.Service) bool {
	_, ok := service.Annotations[OvnServiceIdledAt]
	return !(config.Kubernetes.OVNEmptyLbEvents && ok)
}
