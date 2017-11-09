package ovn

import (
	"fmt"
	"github.com/Sirupsen/logrus"
	kapi "k8s.io/client-go/pkg/api/v1"
	"os/exec"
	"strings"
)

var dnsInSchema string
var dnsRecord string

func getDNSTable() string {
	if dnsInSchema == "" {
		_, err := exec.Command(OvnNbctl, "list", "dns").Output()
		if err != nil {
			dnsInSchema = "no"
		}
	}

	if dnsInSchema == "no" {
		return ""
	}

	if dnsRecord != "" {
		return dnsRecord
	}

	uuid, err := exec.Command(OvnNbctl, "--data=bare", "--no-heading",
		"--columns=_uuid", "find", "DNS", "external_ids:k8s-dns=yes").Output()
	if err != nil || string(uuid) == "" {
		logrus.Errorf("find failed to get the dns record (%v)", err)
		return ""
	}
	dnsRecord = strings.TrimSpace(string(uuid))

	return dnsRecord
}

func setDNSRecord(name, namespace, IP string) {
	if name == "" || IP == "" || namespace == "" {
		return
	}
	uuid := getDNSTable()
	if uuid == "" {
		return
	}
	dnsName := fmt.Sprintf("%s.%s", namespace, name)
	value := fmt.Sprintf("records:%s=%s", dnsName, IP)
	_, err := exec.Command(OvnNbctl, "set", "DNS", uuid,
		value).CombinedOutput()
	if err != nil {
		logrus.Errorf("error in creating DNS record for %s=%s (%v)",
			dnsName, IP, err)
		return
	}
}

func deleteDNSRecord(name, namespace string) {
	if name == "" || namespace == "" {
		return
	}
	uuid := getDNSTable()
	if uuid == "" {
		return
	}

	dnsName := fmt.Sprintf("%s.%s", namespace, name)
	_, err := exec.Command(OvnNbctl, "--if-exists", "remove", "dns", uuid,
		"records", dnsName).CombinedOutput()
	if err != nil {
		logrus.Errorf("failed to remove dns record for %s", dnsName)
	}
}

func (ovn *Controller) addService(service *kapi.Service) {
	logrus.Infof("addService received event: %+v", service)

	setDNSRecord(service.Name, service.Namespace, service.Spec.ClusterIP)
}

func (ovn *Controller) deleteService(service *kapi.Service) {
	deleteDNSRecord(service.Name, service.Namespace)

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
