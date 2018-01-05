package ovn

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"unicode"

	"github.com/Sirupsen/logrus"
	kapi "k8s.io/api/core/v1"
)

func (ovn *Controller) getLoadBalancer(protocol kapi.Protocol) string {
	if outStr, ok := ovn.loadbalancerClusterCache[string(protocol)]; ok {
		return outStr
	}

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
	ovn.loadbalancerClusterCache[string(protocol)] = outStr
	return outStr
}

func (ovn *Controller) getLoadBalancerVIPS(
	loadBalancer string) (map[string]interface{}, error) {
	output, err := exec.Command(OvnNbctl, "--data=bare", "--no-heading",
		"get", "load_balancer", loadBalancer, "vips").Output()
	if err != nil {
		return nil, err
	}
	outStr := strings.TrimFunc(string(output), unicode.IsSpace)
	if outStr == "" {
		return nil, nil
	}
	outStrMap := strings.Replace(outStr, "=", ":", -1)

	var raw map[string]interface{}
	err = json.Unmarshal([]byte(outStrMap), &raw)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

func (ovn *Controller) deleteLoadBalancerVIP(loadBalancer, vip string) {
	vipQuotes := fmt.Sprintf("\"%s\"", vip)
	_, err := exec.Command(OvnNbctl, "--if-exists", "remove", "load_balancer",
		loadBalancer, "vips", vipQuotes).Output()
	if err != nil {
		logrus.Errorf("Error in deleting load balancer vip %s for %s: %v",
			vip, loadBalancer, err)
	}
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
