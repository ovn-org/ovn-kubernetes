package ovn

import (
	"fmt"

	util "github.com/openvswitch/ovn-kubernetes/go-controller/pkg/util"
	"github.com/sirupsen/logrus"
	kapi "k8s.io/api/core/v1"
)

type lbEndpoints struct {
	IPs  []string
	Port int32
}

// AddEndpoints adds endpoints and creates corresponding resources in OVN
func (ovn *Controller) AddEndpoints(ep *kapi.Endpoints) error {
	// get service
	// TODO: cache the service
	svc, err := ovn.kube.GetService(ep.Namespace, ep.Name)
	if err != nil {
		// This is not necessarily an error. For e.g when there are endpoints
		// without a corresponding service.
		logrus.Debugf("no service found for endpoint %s in namespace %s",
			ep.Name, ep.Namespace)
		return nil
	}
	tcpPortMap := make(map[string]lbEndpoints)
	udpPortMap := make(map[string]lbEndpoints)
	for _, s := range ep.Subsets {
		for _, ip := range s.Addresses {
			for _, port := range s.Ports {
				var ips []string
				var portMap map[string]lbEndpoints
				if port.Protocol == kapi.ProtocolUDP {
					portMap = udpPortMap
				} else if port.Protocol == kapi.ProtocolTCP {
					portMap = tcpPortMap
				}
				if lbEps, ok := portMap[port.Name]; ok {
					ips = lbEps.IPs
				} else {
					ips = make([]string, 0)
				}
				ips = append(ips, ip.IP)
				portMap[port.Name] = lbEndpoints{IPs: ips, Port: port.Port}
			}
		}
	}

	logrus.Debugf("Tcp table: %v\nUdp table: %v", tcpPortMap, udpPortMap)

	for svcPortName, lbEps := range tcpPortMap {
		ips := lbEps.IPs
		targetPort := lbEps.Port
		for _, svcPort := range svc.Spec.Ports {
			if svcPort.Protocol == kapi.ProtocolTCP && svcPort.Name == svcPortName {
				if svc.Spec.Type == kapi.ServiceTypeNodePort && ovn.nodePortEnable {
					logrus.Debugf("Creating Gateways IP for NodePort: %d, %v", svcPort.NodePort, ips)
					err = ovn.createGatewaysVIP(string(svcPort.Protocol), svcPort.NodePort, targetPort, ips)
					if err != nil {
						logrus.Errorf("Error in creating Node Port for svc %s, node port: %d - %v\n", svc.Name, svcPort.NodePort, err)
						continue
					}
				}
				if svc.Spec.Type == kapi.ServiceTypeClusterIP || svc.Spec.Type == kapi.ServiceTypeNodePort {
					var loadBalancer string
					loadBalancer, err = ovn.getLoadBalancer(svcPort.Protocol)
					if err != nil {
						logrus.Errorf("Failed to get loadbalancer for %s (%v)",
							svcPort.Protocol, err)
						continue
					}
					err = ovn.createLoadBalancerVIP(loadBalancer,
						svc.Spec.ClusterIP, svcPort.Port, ips, targetPort)
					if err != nil {
						logrus.Errorf("Error in creating Cluster IP for svc %s, target port: %d - %v\n", svc.Name, targetPort, err)
						continue
					}
				}
			}
		}
	}
	for svcPortName, lbEps := range udpPortMap {
		ips := lbEps.IPs
		targetPort := lbEps.Port
		for _, svcPort := range svc.Spec.Ports {
			if svcPort.Protocol == kapi.ProtocolUDP && svcPort.Name == svcPortName {
				if svc.Spec.Type == kapi.ServiceTypeNodePort && ovn.nodePortEnable {
					err = ovn.createGatewaysVIP(string(svcPort.Protocol), svcPort.NodePort, targetPort, ips)
					if err != nil {
						logrus.Errorf("Error in creating Node Port for svc %s, node port: %d - %v\n", svc.Name, svcPort.NodePort, err)
						continue
					}
				} else if svc.Spec.Type == kapi.ServiceTypeNodePort || svc.Spec.Type == kapi.ServiceTypeClusterIP {
					var loadBalancer string
					loadBalancer, err = ovn.getLoadBalancer(svcPort.Protocol)
					if err != nil {
						logrus.Errorf("Failed to get loadbalancer for %s (%v)",
							svcPort.Protocol, err)
						continue
					}
					err = ovn.createLoadBalancerVIP(loadBalancer,
						svc.Spec.ClusterIP, svcPort.Port, ips, targetPort)
					if err != nil {
						logrus.Errorf("Error in creating Cluster IP for svc %s, target port: %d - %v\n", svc.Name, targetPort, err)
						continue
					}
				}
			}
		}
	}
	return nil
}

func (ovn *Controller) deleteEndpoints(ep *kapi.Endpoints) error {
	svc, err := ovn.kube.GetService(ep.Namespace, ep.Name)
	if err != nil {
		// This is not necessarily an error. For e.g when a service is deleted,
		// you will get endpoint delete event and the call to fetch service
		// will fail.
		logrus.Debugf("no service found for endpoint %s in namespace %s",
			ep.Name, ep.Namespace)
		return nil
	}
	for _, svcPort := range svc.Spec.Ports {
		var lb string
		lb, err = ovn.getLoadBalancer(svcPort.Protocol)
		if err != nil {
			logrus.Errorf("Failed to get load-balancer for %s (%v)",
				lb, err)
			continue
		}
		key := fmt.Sprintf("\"%s:%d\"", svc.Spec.ClusterIP, svcPort.Port)
		_, stderr, err := util.RunOVNNbctlHA("remove", "load_balancer", lb,
			"vips", key)
		if err != nil {
			logrus.Errorf("Error in deleting endpoints for lb %s, "+
				"stderr: %q (%v)", lb, stderr, err)
		}
	}
	return nil
}
