package ovn

import (
	"fmt"
	"net"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/sirupsen/logrus"
	kapi "k8s.io/api/core/v1"
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
			logrus.Errorf("Spurious object in syncServices: %v",
				serviceInterface)
			continue
		}

		if !util.ServiceTypeHasClusterIP(service) {
			continue
		}

		if !util.IsClusterIPSet(service) {
			logrus.Debugf("Skipping service %s due to clusterIP = %q",
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
			logrus.Errorf("Failed to get load-balancer for %s (%v)",
				kapi.Protocol(protocol), err)
			continue
		}

		loadBalancerVIPS, err := ovn.getLoadBalancerVIPS(loadBalancer)
		if err != nil {
			logrus.Errorf("failed to get load-balancer vips for %s (%v)",
				loadBalancer, err)
			continue
		}
		if loadBalancerVIPS == nil {
			continue
		}

		for vip := range loadBalancerVIPS {
			if !stringSliceMembership(clusterServices[protocol], vip) {
				logrus.Debugf("Deleting stale cluster vip %s in "+
					"loadbalancer %s", vip, loadBalancer)
				ovn.deleteLoadBalancerVIP(loadBalancer, vip)
			}
		}
	}

	// For each gateway, remove any VIP that does not exist in
	// 'nodeportServices'.
	gateways, stderr, err := ovn.getOvnGateways()
	if err != nil {
		logrus.Errorf("failed to get ovn gateways. Not syncing nodeport"+
			"stdout: %q, stderr: %q (%v)", gateways, stderr, err)
		return
	}

	for _, gateway := range gateways {
		for _, protocol := range []string{TCP, UDP} {
			loadBalancer, err := ovn.getGatewayLoadBalancer(gateway, protocol)
			if err != nil {
				logrus.Errorf("physical gateway %s does not have "+
					"load_balancer (%v)", gateway, err)
				continue
			}
			if loadBalancer == "" {
				continue
			}

			loadBalancerVIPS, err := ovn.getLoadBalancerVIPS(loadBalancer)
			if err != nil {
				logrus.Errorf("failed to get load-balancer vips for %s (%v)",
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
					logrus.Errorf("failed to split %s to vip and port (%v)",
						vip, err)
					continue
				}

				if !stringSliceMembership(nodeportServices[protocol], port) && !stringSliceMembership(lbServices[protocol], vip) {
					logrus.Debugf("Deleting stale nodeport vip %s in "+
						"loadbalancer %s", vip, loadBalancer)
					ovn.deleteLoadBalancerVIP(loadBalancer, vip)
				}
			}
		}
	}
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
		if util.ServiceTypeHasNodePort(service) && config.Gateway.NodeportEnable {
			// Delete the 'NodePort' service from a load-balancer instantiated in gateways.
			err := ovn.createGatewaysVIP(string(protocol), port, targetPort, ips)
			if err != nil {
				logrus.Errorf("Error in deleting NodePort gateway entry for service "+
					"%s %+v", util.JoinHostPortInt32(service.Name, port), err)
			}
		}
		if util.ServiceTypeHasClusterIP(service) {
			loadBalancer, err := ovn.getLoadBalancer(protocol)
			if err != nil {
				logrus.Errorf("Failed to get load-balancer for %s (%v)",
					protocol, err)
				break
			}

			err = ovn.createLoadBalancerVIP(loadBalancer,
				service.Spec.ClusterIP, svcPort.Port, ips, targetPort)
			if err != nil {
				logrus.Errorf("Error in deleting load balancer for service "+
					"%s %+v", util.JoinHostPortInt32(service.Name, port), err)
			}
			ovn.handleExternalIPs(service, svcPort, ips, targetPort)
		}
	}
}
