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

func (ovn *Controller) getLoadBalancer(protocol kapi.Protocol) (string, error) {
	if outStr, ok := ovn.loadbalancerClusterCache[protocol]; ok {
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
	} else if protocol == kapi.ProtocolSCTP {
		out, _, err = util.RunOVNNbctl("--data=bare", "--no-heading",
			"--columns=_uuid", "find", "load_balancer",
			"external_ids:k8s-cluster-lb-sctp=yes")
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

// deleteLoadBalancerVIP removes the VIP as well as any reject ACLs associated to the LB
func (ovn *Controller) deleteLoadBalancerVIP(loadBalancer, vip string) error {
	vipQuotes := fmt.Sprintf("\"%s\"", vip)
	stdout, stderr, err := util.RunOVNNbctl("--if-exists", "remove", "load_balancer", loadBalancer, "vips", vipQuotes)
	if err != nil {
		// if we hit an error and fail to remove load balancer, we skip removing the rejectACL
		return fmt.Errorf("error in deleting load balancer vip %s for %s"+
			"stdout: %q, stderr: %q, error: %v",
			vip, loadBalancer, stdout, stderr, err)
	}
	ovn.removeServiceEndpoints(loadBalancer, vip)
	ovn.deleteLoadBalancerRejectACL(loadBalancer, vip)
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
// each IP of the same address family in targetIPs, removing the reject ACL for any
// source IP that is now in use.
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
		if len(targets) > 0 {
			// ensure the ACL is removed if it exists
			ovn.deleteLoadBalancerRejectACL(lb, util.JoinHostPortInt32(sourceIP, sourcePort))
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// TODO: Add unittest for function.
func generateACLName(lb string, sourceIP string, sourcePort int32) string {
	aclName := fmt.Sprintf("%s-%s:%d", lb, sourceIP, sourcePort)
	aclName = strings.ReplaceAll(aclName, ":", "\\:")
	// ACL names are limited to 63 characters
	if len(aclName) > 63 {
		var ipPortLen int
		srcPortStr := fmt.Sprintf("%d", sourcePort)
		if utilnet.IsIPv6String(sourceIP) {
			// Add the length of the IP (max 39 with colons),
			// plus 14 for '\\' for each ':' in IP,
			// plus length of sourcePort (max 5 char),
			// plus 3 for additional '\\:' to separate,
			// plus 1 for '-' between lb and IP.
			// With full IPv6 address and 5 char port, max ipPortLen is 62.
			ipPortLen = len(sourceIP) + 14 + len(srcPortStr) + 3 + 1
		} else {
			// Add the length of the IP (max 15 with periods),
			// plus length of sourcePort (max 5 char),
			// plus 3 for additional '\\:' to separate,
			// plus 1 for '-' between lb and IP.
			// With full IPv4 address and 5 char port, max ipPortLen is 24.
			ipPortLen = len(sourceIP) + len(srcPortStr) + 3 + 1
		}
		lbTrim := 63 - ipPortLen
		// Shorten the Load Balancer name to allow full IP:port
		tmpLb := lb[:lbTrim]
		klog.Infof("Limiting ACL Name from %s to %s-%s:%d to keep under 63 characters", aclName, tmpLb, sourceIP, sourcePort)
		aclName = fmt.Sprintf("%s-%s:%d", tmpLb, sourceIP, sourcePort)
		aclName = strings.ReplaceAll(aclName, ":", "\\:")
	}
	return aclName
}

func (ovn *Controller) createLoadBalancerRejectACL(lb, sourceIP string, sourcePort int32, proto kapi.Protocol) (string, error) {
	ovn.serviceLBLock.Lock()
	defer ovn.serviceLBLock.Unlock()

	ip := net.ParseIP(sourceIP)
	if ip == nil {
		return "", fmt.Errorf("cannot create reject ACL, invalid source IP: %s", sourceIP)
	}
	var l3Prefix, aclMatch string
	if utilnet.IsIPv6(ip) {
		l3Prefix = "ip6"
	} else {
		l3Prefix = "ip4"
	}
	vip := util.JoinHostPortInt32(sourceIP, sourcePort)
	// NOTE: doesn't use vip, to avoid having brackets in the name with IPv6
	aclName := generateACLName(lb, sourceIP, sourcePort)
	// If ovn-k8s was restarted, we lost the cache, and an ACL may already exist in OVN. In that case we need to check
	// using ACL name
	aclUUID, stderr, err := util.RunOVNNbctl("--data=bare", "--no-heading", "--columns=_uuid", "find", "acl",
		fmt.Sprintf("name=%s", aclName))
	if err != nil {
		klog.Errorf("Error while querying ACLs by name: %s, %v", stderr, err)
	} else if len(aclUUID) > 0 {
		klog.Infof("Existing Service Reject ACL found: %s for %s", aclUUID, aclName)
		_, stderr, err = util.RunOVNNbctl("--", "add", "port_group", ovn.clusterPortGroupUUID, "acls", aclUUID)
		if err != nil {
			klog.Errorf("Failed to add LB %s ACL %s %q to cluster port group: %q (%v)",
				lb, aclUUID, aclName, stderr, err)
			return "", err
		}
		ovn.setServiceACLToLB(lb, vip, aclUUID)
		return aclUUID, nil
	}

	aclMatch = fmt.Sprintf("match=\"%s.dst==%s && %s && %s.dst==%d\"", l3Prefix, sourceIP,
		strings.ToLower(string(proto)), strings.ToLower(string(proto)), sourcePort)
	cmd := []string{"--id=@acl", "create", "acl", "direction=from-lport", "priority=1000", aclMatch, "action=reject",
		fmt.Sprintf("name=%s", aclName)}
	cmd = append(cmd, "--", "add", "port_group", ovn.clusterPortGroupUUID, "acls", "@acl")

	aclUUID, stderr, err = util.RunOVNNbctl(cmd...)
	if err != nil {
		klog.Errorf("Failed to create LB %s ACL %q and add to cluster port group: %q (%v)",
			lb, aclName, stderr, err)
		return "", err
	}
	// Associate ACL UUID with load balancer and ip+port so we can remove this ACL if
	// backends are re-added.
	ovn.setServiceACLToLB(lb, vip, aclUUID)
	return aclUUID, nil
}

func (ovn *Controller) deleteLoadBalancerRejectACL(lb, vip string) {
	acl, _ := ovn.getServiceLBInfo(lb, vip)
	if acl == "" {
		klog.V(5).Infof("No reject ACL found to remove for load balancer: %s, vip: %s", lb, vip)
		return
	}

	_, stderr, err := util.RunOVNNbctl("remove", "port_group", ovn.clusterPortGroupUUID, "acls", acl)
	if err != nil {
		klog.Errorf("Failed to remove reject ACL for %s from LB %s: stderr: %q, error: %v", vip, lb, stderr, err)
		return
	}

	ovn.removeServiceACL(lb, vip)
}
