package ovn

import (
	"encoding/json"
	"fmt"
	"net"
	"strings"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	kapi "k8s.io/api/core/v1"
	"k8s.io/klog"
	utilnet "k8s.io/utils/net"
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
		klog.Errorf(err.Error())
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

// deleteLoadBalancerVIP removes the VIP as well as any reject ACLs associated to the LB
func (ovn *Controller) deleteLoadBalancerVIP(loadBalancer, vip string) {
	vipQuotes := fmt.Sprintf("\"%s\"", vip)
	stdout, stderr, err := util.RunOVNNbctl("--if-exists", "remove", "load_balancer", loadBalancer, "vips", vipQuotes)
	if err != nil {
		klog.Errorf("Error in deleting load balancer vip %s for %s"+
			"stdout: %q, stderr: %q, error: %v",
			vip, loadBalancer, stdout, stderr, err)
		// if we hit an error and fail to remove load balancer, we skip removing the rejectACL
		return
	}
	ovn.removeServiceEndpoints(loadBalancer, vip)
	ovn.deleteLoadBalancerRejectACL(loadBalancer, vip)
	ovn.removeServiceLB(loadBalancer, vip)
}

func (ovn *Controller) configureLoadBalancer(lb, serviceIP string, port int32, endpoints []string) error {
	ovn.serviceLBLock.Lock()
	defer ovn.serviceLBLock.Unlock()
	commaSeparatedEps := strings.Join(endpoints, ",")
	target := fmt.Sprintf(`vips:"%s"="%s"`, util.JoinHostPortInt32(serviceIP, port), commaSeparatedEps)

	out, stderr, err := util.RunOVNNbctl("set", "load_balancer", lb, target)
	if err != nil {
		return fmt.Errorf("error in configuring load balancer: %s "+
			"stdout: %q, stderr: %q, error: %v", lb, out, stderr, err)
	}
	ovn.setServiceEndpointsToLB(lb, util.JoinHostPortInt32(serviceIP, port), endpoints)
	klog.V(5).Infof("lb entry set for %s, %s, %v", lb, target,
		ovn.serviceLBMap[lb][util.JoinHostPortInt32(serviceIP, port)])
	return nil
}

// createLoadBalancerVIP either creates or updates a load balancer VIP
// Calls to this method assume that if ips are passed that those endpoints actually exist
// and thus the reject ACL is removed
func (ovn *Controller) createLoadBalancerVIP(lb, serviceIP string, port int32, ips []string, targetPort int32) error {
	klog.V(5).Infof("Creating lb with %s, %s, %d, [%v], %d", lb, serviceIP, port, ips, targetPort)

	var endpoints []string
	for _, ip := range ips {
		endpoints = append(endpoints, util.JoinHostPortInt32(ip, targetPort))
	}
	err := ovn.configureLoadBalancer(lb, serviceIP, port, endpoints)
	if len(ips) > 0 {
		// ensure the ACL is removed if it exists
		ovn.deleteLoadBalancerRejectACL(lb, util.JoinHostPortInt32(serviceIP, port))
	}
	return err
}

func (ovn *Controller) getLogicalSwitchesForLoadBalancer(lb string) ([]string, error) {
	out, _, err := util.RunOVNNbctl("--data=bare", "--no-heading",
		"--columns=_uuid", "find",
		"logical_switch", fmt.Sprintf("load_balancer{>=}%s", lb))
	if err != nil {
		return nil, err
	}
	if len(strings.Fields(out)) > 0 {
		return strings.Fields(out), nil
	}
	// if load balancer was not on a switch, then it may be on a router
	out, _, err = util.RunOVNNbctl("--data=bare", "--no-heading",
		"--columns=name", "find",
		"logical_router", fmt.Sprintf("load_balancer{>=}%s", lb))
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	// if this is a GR we know the corresponding join and external switches, otherwise this is an unhandled
	// case
	if strings.HasPrefix(out, util.GWRouterPrefix) {
		routerName := strings.TrimPrefix(out, util.GWRouterPrefix)
		return []string{util.JoinSwitchPrefix + routerName, util.ExternalSwitchPrefix + routerName}, nil
	}
	return nil, fmt.Errorf("router detected with load balancer that is not a GR")
}

func (ovn *Controller) createLoadBalancerRejectACL(lb string, serviceIP string, port int32, proto kapi.Protocol) (string, error) {
	ovn.serviceLBLock.Lock()
	defer ovn.serviceLBLock.Unlock()
	switches, err := ovn.getLogicalSwitchesForLoadBalancer(lb)
	if err != nil {
		return "", fmt.Errorf("error finding logical switch that contains load balancer %s: %v", lb, err)
	}

	if len(switches) == 0 {
		klog.V(5).Infof("Ignoring creating reject ACL for load balancer %s. It has no logical switches", lb)
		return "", nil
	}

	ip := net.ParseIP(serviceIP)
	if ip == nil {
		return "", fmt.Errorf("cannot create reject ACL, invalid cluster IP: %s", serviceIP)
	}
	var aclMatch string
	var l3Prefix string
	if utilnet.IsIPv6(ip) {
		l3Prefix = "ip6"
	} else {
		l3Prefix = "ip4"
	}
	vip := util.JoinHostPortInt32(serviceIP, port)
	aclName := fmt.Sprintf("%s-%s", lb, vip)
	// If ovn-k8s was restarted, we lost the cache, and an ACL may already exist in OVN. In that case we need to check
	// using ACL name
	aclUUID, stderr, err := util.RunOVNNbctl("--data=bare", "--no-heading", "--columns=_uuid", "find", "acl",
		fmt.Sprintf("name=%s", strings.ReplaceAll(aclName, ":", "\\:")))
	if err != nil {
		klog.Errorf("Error while querying ACLs by name: %s, %v", stderr, err)
	} else if len(aclUUID) > 0 {
		klog.Infof("Existing Service Reject ACL found: %s for %s", aclUUID, aclName)
		// If we found the ACL exists we need to ensure it applies to all logical switches
		for _, ls := range switches {
			_, _, err := util.RunOVNNbctl("add", "logical_switch", ls, "acls", aclUUID)
			if err != nil {
				klog.Warningf("Unable to add reject ACL: %s for switch: %s", aclUUID, ls)
			}
		}
		ovn.setServiceACLToLB(lb, util.JoinHostPortInt32(serviceIP, port), aclUUID)
		return aclUUID, nil
	}

	aclMatch = fmt.Sprintf("match=\"%s.dst==%s && %s && %s.dst==%d\"", l3Prefix, serviceIP,
		strings.ToLower(string(proto)), strings.ToLower(string(proto)), port)

	cmd := []string{"--id=@acl", "create", "acl", "direction=from-lport", "priority=1000", aclMatch, "action=reject",
		fmt.Sprintf("name=%s", strings.ReplaceAll(aclName, ":", "\\:"))}
	for _, ls := range switches {
		cmd = append(cmd, "--", "add", "logical_switch", ls, "acls", "@acl")
	}

	aclUUID, stderr, err = util.RunOVNNbctl(cmd...)
	if err != nil {
		return "", fmt.Errorf("error creating ACL reject rule: %s for load balancer %s: %s, %s", cmd, lb, stderr,
			err)
	} else {
		// Associate ACL UUID with load balancer and ip+port so we can remove this ACL if
		// backends are re-added.
		ovn.setServiceACLToLB(lb, util.JoinHostPortInt32(serviceIP, port), aclUUID)
	}
	return aclUUID, nil
}

func (ovn *Controller) deleteLoadBalancerRejectACL(lb, vip string) {
	acl, _ := ovn.getServiceLBInfo(lb, vip)
	if acl == "" {
		klog.V(5).Infof("No reject ACL found to remove for load balancer: %s, vip: %s", lb, vip)
		return
	}

	switches, err := ovn.getLogicalSwitchesForLoadBalancer(lb)
	if err != nil {
		klog.Errorf("Could not retrieve logical switches associated with load balancer %s", lb)
		return
	}
	for _, ls := range switches {
		_, _, err := util.RunOVNNbctl("--if-exists", "remove", "logical_switch", ls, "acl", acl)
		if err != nil {
			klog.Errorf("Error while removing ACL: %s, from switch %s, error: %v", acl, ls, err)
		} else {
			klog.V(5).Infof("ACL: %s, removed from switch: %s", acl, ls)
		}
	}

	ovn.removeServiceACL(lb, vip)
}
