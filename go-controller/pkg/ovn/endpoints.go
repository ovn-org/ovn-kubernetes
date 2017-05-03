package ovn

import (
	"fmt"
	"os/exec"
	"strings"
	"unicode"

	"github.com/Sirupsen/logrus"
	kapi "k8s.io/client-go/pkg/api/v1"
)

func (ovn *Controller) getLoadBalancer(protocol kapi.Protocol) string {
	// TODO: add a cache here for the load balancer lookup, so that multiple calls to nbctl can be avoided
	var out []byte
	if protocol == kapi.ProtocolTCP {
		out, _ = exec.Command(OvnNbctl, "--data=bare", "--no-heading",
			"--columns=_uuid", "find", "load_balancer",
			"external_ids:k8s-cluster-lb-tcp=yes").CombinedOutput()
	} else if protocol == kapi.ProtocolUDP {
		out, _ = exec.Command(OvnNbctl, "--data=bare", "--no-heading",
			"--columns=_uuid", "find", "load_balancer",
			"external_ids:k8s-cluster-lb-udp=yes").CombinedOutput()
	}
	outStr := strings.TrimFunc(string(out), unicode.IsSpace)
	return outStr
}

func (ovn *Controller) createLoadBalancerVIP(lb string, serviceIP string, port int32, ips []string, targetPort int32) error {
	logrus.Debugf("Creating lb with %s, %s, %d, [%v], %d", lb, serviceIP, port, ips, targetPort)

	// With service_ip:port as a VIP, create an entry in 'load_balancer'
	// key is of the form "IP:port" (with quotes around)
	key := fmt.Sprintf("\"%s:%d\"", serviceIP, port)

	if len(ips) == 0 {
		_, err := exec.Command(OvnNbctl, "remove", "load_balancer", lb, "vips", key).CombinedOutput()
		return err
	}

	var commaSeparatedEndpoints string
	for i, ep := range ips {
		comma := ","
		if i == 0 {
			comma = ""
		}
		commaSeparatedEndpoints += fmt.Sprintf("%s%s:%d", comma, ep, targetPort)
	}
	target := fmt.Sprintf("vips:\"%s:%d\"=\"%s\"", serviceIP, port, commaSeparatedEndpoints)

	out, err := exec.Command(OvnNbctl, "set", "load_balancer", lb, target).CombinedOutput()
	if err != nil {
		logrus.Errorf("Error in creating load balancer: %v(%v)", string(out), err)
	}
	return err
}

func (ovn *Controller) addEndpoints(ep *kapi.Endpoints) error {
	// get service
	svc, err := ovn.Kube.GetService(ep.Namespace, ep.Name)
	if err != nil {
		return err
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
				err = ovn.createLoadBalancerVIP(ovn.getLoadBalancer(svcPort.Protocol), svc.Spec.ClusterIP, svcPort.Port, ips, targetPort)
				if err != nil {
					return err
				}
			}
		}
	}
	for targetPort, ips := range udpPortMap {
		for _, svcPort := range svc.Spec.Ports {
			if svcPort.Protocol == kapi.ProtocolUDP && svcPort.TargetPort.IntVal == targetPort {
				err := ovn.createLoadBalancerVIP(ovn.getLoadBalancer(svcPort.Protocol), svc.Spec.ClusterIP, svcPort.Port, ips, targetPort)
				if err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (ovn *Controller) deleteEndpoints(ep *kapi.Endpoints) error {
	svc, err := ovn.Kube.GetService(ep.Namespace, ep.Name)
	if err != nil {
		return err
	}
	for _, svcPort := range svc.Spec.Ports {
		lb := ovn.getLoadBalancer(svcPort.Protocol)
		key := fmt.Sprintf("\"%s:%d\"", svc.Spec.ClusterIP, svcPort.Port)
		_, err := exec.Command(OvnNbctl, "remove", "load_balancer", lb, "vips", key).CombinedOutput()
		if err != nil {
			logrus.Errorf("Error in deleting endpoints: %v", err)
		}
	}
	return nil
}
