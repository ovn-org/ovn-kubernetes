package ovn

import (
	"encoding/json"
	"fmt"
	"strings"

	util "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/sirupsen/logrus"
	kapi "k8s.io/api/core/v1"
)

func (ovn *Controller) getLoadBalancer(protocol kapi.Protocol) (string,
	error) {
	if outStr, ok := ovn.loadbalancerClusterCache[string(protocol)]; ok {
		return outStr, nil
	}

	var out string
	var err error
	if protocol == kapi.ProtocolTCP {
		out, _, err = util.RunOVNNbctl("--data=bare",
			"--no-heading", "--columns=_uuid", "find", "load_balancer",
			"external_ids:k8s-cluster-lb-tcp=yes")
	} else if protocol == kapi.ProtocolUDP {
		out, _, err = util.RunOVNNbctl("--data=bare", "--no-heading",
			"--columns=_uuid", "find", "load_balancer",
			"external_ids:k8s-cluster-lb-udp=yes")
	}
	if err != nil {
		return "", err
	}
	if out == "" {
		return "", fmt.Errorf("no load-balancer found in the database")
	}
	ovn.loadbalancerClusterCache[string(protocol)] = out
	return out, nil
}

func (ovn *Controller) getDefaultGatewayLoadBalancer(protocol kapi.Protocol) string {
	if outStr, ok := ovn.loadbalancerGWCache[string(protocol)]; ok {
		return outStr
	}

	gw, _, err := util.GetDefaultGatewayRouterIP()
	if err != nil {
		logrus.Errorf(err.Error())
		return ""
	}

	externalIDKey := string(protocol) + "_lb_gateway_router"
	lb, _, _ := util.RunOVNNbctl("--data=bare",
		"--no-heading", "--columns=_uuid", "find", "load_balancer",
		"external_ids:"+externalIDKey+"="+gw)
	if len(lb) != 0 {
		ovn.loadbalancerGWCache[string(protocol)] = lb
		ovn.defGatewayRouter = gw
	}
	return lb
}

func (ovn *Controller) getLoadBalancerVIPS(
	loadBalancer string) (map[string]interface{}, error) {
	outStr, _, err := util.RunOVNNbctl("--data=bare", "--no-heading",
		"get", "load_balancer", loadBalancer, "vips")
	if err != nil {
		return nil, err
	}
	if outStr == "" {
		return nil, nil
	}
	// sample outStr:
	// - {"192.168.0.1:80"="10.1.1.1:80,10.2.2.2:80"}
	// - {"[fd01::]:80"="[fd02::]:80,[fd03::]:80"}
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
	stdout, stderr, err := util.RunOVNNbctl("--if-exists", "remove",
		"load_balancer", loadBalancer, "vips", vipQuotes)
	if err != nil {
		logrus.Errorf("Error in deleting load balancer vip %s for %s"+
			"stdout: %q, stderr: %q, error: %v",
			vip, loadBalancer, stdout, stderr, err)
	}
}

func (ovn *Controller) createLoadBalancerVIP(lb string, serviceIP string, port int32, ips []string, targetPort int32) error {
	logrus.Debugf("Creating lb with %s, %s, %d, [%v], %d", lb, serviceIP, port, ips, targetPort)

	// With service_ip:port as a VIP, create an entry in 'load_balancer'
	// key is of the form "IP:port" (with quotes around)
	key := fmt.Sprintf(`"%s"`, util.JoinHostPortInt32(serviceIP, port))

	if len(ips) == 0 {
		_, _, err := util.RunOVNNbctl("remove", "load_balancer", lb,
			"vips", key)
		return err
	}

	var commaSeparatedEndpoints string
	for i, ep := range ips {
		comma := ","
		if i == 0 {
			comma = ""
		}
		commaSeparatedEndpoints += fmt.Sprintf("%s%s", comma, util.JoinHostPortInt32(ep, targetPort))
	}
	target := fmt.Sprintf(`vips:"%s"="%s"`, util.JoinHostPortInt32(serviceIP, port), commaSeparatedEndpoints)

	out, stderr, err := util.RunOVNNbctl("set", "load_balancer", lb,
		target)
	if err != nil {
		logrus.Errorf("Error in creating load balancer: %s "+
			"stdout: %q, stderr: %q, error: %v", lb, out, stderr, err)
	}
	return err
}
