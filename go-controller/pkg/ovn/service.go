package ovn

import (
	"github.com/Sirupsen/logrus"
	kapi "k8s.io/client-go/pkg/api/v1"
)

func (ovn *Controller) deleteService(service *kapi.Service) {
	if service.Spec.ClusterIP == "" || len(service.Spec.Ports) == 0 {
		return
	}

	ips := make([]string, 0)

	for _, svcPort := range service.Spec.Ports {
		var port int32
		if service.Spec.Type == kapi.ServiceTypeNodePort {
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

		// TODO: Support named ports.
		if svcPort.TargetPort.Type == 1 {
			continue
		}

		targetPort := svcPort.TargetPort.IntVal
		if targetPort == 0 {
			targetPort = svcPort.Port
		}

		if service.Spec.Type == kapi.ServiceTypeNodePort {
			// Delete the 'NodePort' service from a load-balancer instantiated in gateways.
			err := ovn.createGatewaysVIP(string(protocol), port, targetPort, ips)
			if err != nil {
				logrus.Errorf("Error in deleting NodePort gateway entry for service "+
					"%s:%d %+v", service.Name, port, err)
			}
		}
		if service.Spec.Type == kapi.ServiceTypeNodePort || service.Spec.Type == kapi.ServiceTypeClusterIP {
			err := ovn.createLoadBalancerVIP(ovn.getLoadBalancer(protocol),
				service.Spec.ClusterIP, svcPort.Port, ips, targetPort)
			if err != nil {
				logrus.Errorf("Error in deleting load balancer for service "+
					"%s:%d %+v", service.Name, port, err)
			}
		}
	}
}
