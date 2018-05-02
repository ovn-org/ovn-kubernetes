package ovn

import (
	"fmt"

	util "github.com/openvswitch/ovn-kubernetes/go-controller/pkg/util"
	"github.com/sirupsen/logrus"
	kapi "k8s.io/api/core/v1"
)

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
	tcpPortMap := make(map[int32]([]string))
	udpPortMap := make(map[int32]([]string))
	for _, s := range ep.Subsets {
		for _, ip := range s.Addresses {
			for _, port := range s.Ports {
				var ips []string
				var portMap map[int32]([]string)
				var ok bool
				if port.Protocol == kapi.ProtocolUDP {
					portMap = udpPortMap
				} else if port.Protocol == kapi.ProtocolTCP {
					portMap = tcpPortMap
				}
				if ips, ok = portMap[port.Port]; !ok {
					ips = make([]string, 0)
				}
				ips = append(ips, ip.IP)
				portMap[port.Port] = ips
			}
		}
	}

	logrus.Debugf("Tcp table: %v\nUdp table: %v", tcpPortMap, udpPortMap)

	for targetPort, ips := range tcpPortMap {
		for _, svcPort := range svc.Spec.Ports {
			if svcPort.Protocol == kapi.ProtocolTCP && svcPort.TargetPort.IntVal == targetPort {
				if svc.Spec.Type == kapi.ServiceTypeNodePort && ovn.nodePortEnable {
					logrus.Debugf("Creating Gateways IP for NodePort: %d, %d, %v", svcPort.NodePort, targetPort, ips)
					err = ovn.createGatewaysVIP(string(svcPort.Protocol), svcPort.NodePort, targetPort, ips)
					if err != nil {
						logrus.Errorf("Error in creating Node Port for svc %s, node port: %d - %v\n", svc.Name, svcPort.NodePort, err)
						continue
					}
				}
				if svc.Spec.Type == kapi.ServiceTypeClusterIP || svc.Spec.Type == kapi.ServiceTypeNodePort {
					err = ovn.createLoadBalancerVIP(ovn.getLoadBalancer(svcPort.Protocol), svc.Spec.ClusterIP, svcPort.Port, ips, targetPort)
					if err != nil {
						logrus.Errorf("Error in creating Cluster IP for svc %s, target port: %d - %v\n", svc.Name, targetPort, err)
						return err
					}
				}
			}
		}
	}
	for targetPort, ips := range udpPortMap {
		for _, svcPort := range svc.Spec.Ports {
			if svcPort.Protocol == kapi.ProtocolUDP && svcPort.TargetPort.IntVal == targetPort {
				if svc.Spec.Type == kapi.ServiceTypeNodePort && ovn.nodePortEnable {
					err = ovn.createGatewaysVIP(string(svcPort.Protocol), svcPort.NodePort, targetPort, ips)
					if err != nil {
						logrus.Errorf("Error in creating Node Port for svc %s, node port: %d - %v\n", svc.Name, svcPort.NodePort, err)
						continue
					}
				} else if svc.Spec.Type == kapi.ServiceTypeNodePort || svc.Spec.Type == kapi.ServiceTypeClusterIP {
					err = ovn.createLoadBalancerVIP(ovn.getLoadBalancer(svcPort.Protocol), svc.Spec.ClusterIP, svcPort.Port, ips, targetPort)
					if err != nil {
						logrus.Errorf("Error in creating Cluster IP for svc %s, target port: %d - %v\n", svc.Name, targetPort, err)
						return err
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
		lb := ovn.getLoadBalancer(svcPort.Protocol)
		key := fmt.Sprintf("\"%s:%d\"", svc.Spec.ClusterIP, svcPort.Port)
		_, stderr, err := util.RunOVNNbctlUnix("remove", "load_balancer", lb,
			"vips", key)
		if err != nil {
			logrus.Errorf("Error in deleting endpoints for lb %s, "+
				"stderr: %q (%v)", lb, stderr, err)
		}
	}
	return nil
}
