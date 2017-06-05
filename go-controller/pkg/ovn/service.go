package ovn

import (
	"github.com/Sirupsen/logrus"
	kapi "k8s.io/client-go/pkg/api/v1"
)

func (ovn *Controller) deleteService(service *kapi.Service) {
	if service.Spec.ClusterIP == "" || len(service.Spec.Ports) == 0 {
		return
	}

	// TODO: Change this after supporting gateways.
	if service.Spec.Type == "NodePort" {
		return
	}
	ips := make([]string, 0)

	for _, svcPort := range service.Spec.Ports {
		if svcPort.Port == 0 {
			continue
		}

		protocol := svcPort.Protocol
		if protocol == "" || (protocol != "TCP" && protocol != "UDP") {
			protocol = "TCP"
		}

		// TODO: Support named ports.
		if svcPort.TargetPort.Type == 1 {
			continue
		}

		targetPort := svcPort.TargetPort.IntVal
		if targetPort == 0 {
			targetPort = svcPort.Port
		}

		err := ovn.createLoadBalancerVIP(ovn.getLoadBalancer(protocol),
			service.Spec.ClusterIP, svcPort.Port, ips, targetPort)
		if err != nil {
			logrus.Errorf("Error in deleting load balancer for service "+
				"%s:%d %+v", service.Name, svcPort.Port, err)
		}
	}
}
