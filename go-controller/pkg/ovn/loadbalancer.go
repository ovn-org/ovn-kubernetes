package ovn

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	kapi "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
)

func (ovn *Controller) getLoadBalancer(protocol kapi.Protocol) (string, error) {
	if outStr, ok := ovn.loadbalancerClusterCache[protocol]; ok {
		return outStr, nil
	}
	var out string
	var err error
	if protocol == kapi.ProtocolTCP {
		out, _, err = util.FindOVNLoadBalancer(types.ClusterLBTCP, "yes")
	} else if protocol == kapi.ProtocolUDP {
		out, _, err = util.FindOVNLoadBalancer(types.ClusterLBUDP, "yes")
	} else if protocol == kapi.ProtocolSCTP {
		out, _, err = util.FindOVNLoadBalancer(types.ClusterLBSCTP, "yes")
	}
	if err != nil {
		return "", err
	}
	if out == "" {
		return "", fmt.Errorf("no load balancer found in the database")
	}
	ovn.loadbalancerClusterCache[protocol] = out
	return out, nil
}

// getLoadBalancerVIPs returns a map whose keys are VIPs (IP:port) on loadBalancer
func (ovn *Controller) getLoadBalancerVIPs(loadBalancer string) (map[string]interface{}, error) {
	outStr, _, err := util.RunOVNNbctl("--data=bare", "--no-heading",
		"get", "load_balancer", loadBalancer, "vips")
	if err != nil {
		return nil, err
	}
	if outStr == "" {
		return nil, fmt.Errorf("load balancer vips in OVN DB is an empty string")
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

// deleteLoadBalancerVIP removes the VIP
func (ovn *Controller) deleteLoadBalancerVIP(loadBalancer, vip string) error {
	vipQuotes := fmt.Sprintf("\"%s\"", vip)
	stdout, stderr, err := util.RunOVNNbctl("--if-exists", "remove", "load_balancer", loadBalancer, "vips", vipQuotes)
	if err != nil {
		return fmt.Errorf("error in deleting load balancer vip %s for %s"+
			"stdout: %q, stderr: %q, error: %v",
			vip, loadBalancer, stdout, stderr, err)
	}
	ovn.removeServiceEndpoints(loadBalancer, vip)
	ovn.removeServiceLB(loadBalancer, vip)
	return nil
}

// configureLoadBalancer updates the VIP for sourceIP:sourcePort to point to targets (an
// array of IP:port strings)
func (ovn *Controller) configureLoadBalancer(lb, sourceIP string, sourcePort int32, targets []string) error {
	ovn.serviceLBLock.Lock()
	defer ovn.serviceLBLock.Unlock()

	vip := util.JoinHostPortInt32(sourceIP, sourcePort)
	lbTarget := fmt.Sprintf(`vips:"%s"="%s"`, vip, strings.Join(targets, ","))

	out, stderr, err := util.RunOVNNbctl("set", "load_balancer", lb, lbTarget)
	if err != nil {
		return fmt.Errorf("error in configuring load balancer: %s "+
			"stdout: %q, stderr: %q, error: %v", lb, out, stderr, err)
	}
	ovn.setServiceEndpointsToLB(lb, vip, targets)
	klog.V(5).Infof("LB entry set for %s, %s, %v", lb, lbTarget,
		ovn.serviceLBMap[lb][vip])
	return nil
}

// createLoadBalancerVIPs either creates or updates a set of load balancer VIPs mapping
// from sourcePort on each IP of a given address family in sourceIPs, to targetPort on
// each IP of the same address family in targetIPs
func (ovn *Controller) createLoadBalancerVIPs(lb string,
	sourceIPs []string, sourcePort int32,
	targetIPs []string, targetPort int32) error {
	klog.V(5).Infof("Creating lb with %s, [%v], %d, [%v], %d", lb, sourceIPs, sourcePort, targetIPs, targetPort)

	for _, sourceIP := range sourceIPs {
		isIPv6 := utilnet.IsIPv6String(sourceIP)

		var targets []string
		for _, targetIP := range targetIPs {
			if utilnet.IsIPv6String(targetIP) == isIPv6 {
				targets = append(targets, util.JoinHostPortInt32(targetIP, targetPort))
			}
		}
		err := ovn.configureLoadBalancer(lb, sourceIP, sourcePort, targets)
		if err != nil {
			return err
		}
	}
	return nil
}
