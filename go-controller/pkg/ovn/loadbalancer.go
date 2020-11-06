package ovn

import (
	"fmt"
	"net"
	"strings"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/acl"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/loadbalancer"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	kapi "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
)

// getLoadBalancer is a convenience wrapper over GetOVNKubeLoadBalancer to use a cache
// it will return an error if it can not find a load balancer
func (ovn *Controller) getLoadBalancer(protocol kapi.Protocol) (string, error) {
	ovn.loadbalancerClusterCacheMutex.Lock()
	defer ovn.loadbalancerClusterCacheMutex.Unlock()
	if outStr, ok := ovn.loadbalancerClusterCache[protocol]; ok {
		return outStr, nil
	}

	out, err := loadbalancer.GetOVNKubeLoadBalancer(protocol)
	// GetOVNKubeLoadBalancer will return an err if out is empty
	if err != nil {
		return "", err
	}
	ovn.loadbalancerClusterCache[protocol] = out
	return out, nil
}

// deleteLoadBalancerVIP removes the VIP as well as any reject ACLs associated to the LB
func (ovn *Controller) deleteLoadBalancerVIP(loadBalancer, vip string) error {
	err := loadbalancer.DeleteLoadBalancerVIP(loadBalancer, vip)
	if err != nil {
		return err
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
	err := loadbalancer.UpdateLoadBalancer(lb, vip, targets)
	if err != nil {
		return err
	}
	ovn.setServiceEndpointsToLB(lb, vip, targets)
	klog.V(5).Infof("LB entry set for %s, %v, %v", lb, targets, ovn.serviceLBMap[lb][vip])
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

func (ovn *Controller) getLogicalSwitchesForLoadBalancer(lb string) ([]string, error) {
	switches, err := loadbalancer.GetLogicalSwitchesForLoadBalancer(lb)
	if err != nil {
		return nil, err
	}
	return switches, nil
}

// getGRLogicalSwitchForLoadBalancer returns the external switch name if the load balancer is on a GR
func (ovn *Controller) getGRLogicalSwitchForLoadBalancer(lb string) (string, error) {
	routers, err := loadbalancer.GetLogicalRoutersForLoadBalancer(lb)
	if err != nil {
		return "", err
	}
	if len(routers) == 0 {
		return "", nil
	}

	// if this is a GR we know the corresponding join and external switches, otherwise this is an unhandled
	// case
	for _, r := range routers {
		if strings.HasPrefix(r, types.GWRouterPrefix) {
			routerName := strings.TrimPrefix(r, types.GWRouterPrefix)
			return types.ExternalSwitchPrefix + routerName, nil
		}
	}
	return "", fmt.Errorf("router detected with load balancer that is not a GR")
}

func (ovn *Controller) createLoadBalancerRejectACL(lb, sourceIP string, sourcePort int32, proto kapi.Protocol) (string, error) {
	applyToPortGroup := false
	ovn.serviceLBLock.Lock()
	defer ovn.serviceLBLock.Unlock()
	switches, err := ovn.getLogicalSwitchesForLoadBalancer(lb)
	if err != nil {
		return "", fmt.Errorf("error finding logical switch that contains load balancer %s: %v", lb, err)
	}

	if len(switches) > 0 {
		applyToPortGroup = true
	} else {
		klog.V(5).Infof("Ignoring creating reject ACL for port group with load balancer %s. It has no logical switches", lb)
	}

	// check if the load balancer is on a GR, if so we need to get the external switches
	gwRouterExtSwitch, err := ovn.getGRLogicalSwitchForLoadBalancer(lb)
	if err != nil {
		return "", fmt.Errorf("unable to query logical switches for GR with load balancer: %s, error: %v", lb, err)
	}

	if len(switches) == 0 && gwRouterExtSwitch == "" {
		return "", fmt.Errorf("load balancer %s does not apply to any switches in the cluster. Will not create "+
			"Reject ACL", lb)
	}

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
	aclName := loadbalancer.GenerateACLNameForOVNCommand(lb, sourceIP, sourcePort)
	// If ovn-k8s was restarted, we lost the cache, and an ACL may already exist in OVN. In that case we need to check
	// using ACL name
	aclUUID, err := acl.GetACLByName(aclName)
	if err != nil {
		klog.Errorf("Error while querying ACLs by name: %v", err)
	} else if len(aclUUID) > 0 {
		klog.Infof("Existing Service Reject ACL found: %s for %s", aclUUID, aclName)
		var cmd []string
		if applyToPortGroup {
			cmd = append(cmd, "--", "add", "port_group", ovn.clusterPortGroupUUID, "acls", aclUUID)
		}
		if len(gwRouterExtSwitch) > 0 {
			cmd = append(cmd, "--", "add", "logical_switch", gwRouterExtSwitch, "acls", aclUUID)
		}
		if len(cmd) > 0 {
			_, _, err = util.RunOVNNbctl(cmd...)
			if err != nil {
				klog.Errorf("Failed to add LB %s, ACL %s, %q, to cluster port group/switches, error: %v", lb, aclUUID, aclName, err)
			}
		}

		ovn.setServiceACLToLB(lb, vip, aclUUID)

		// If reject ACL exist, ensures that the _uuid is removed from logical_switch acls list.
		// This step is required to ensure the clean-up when ovn upgrades from logical_switch acls
		// to port_group based acls.
		ovn.removeACLFromNodeSwitches(switches, aclUUID)
		return aclUUID, nil
	}

	aclMatch = fmt.Sprintf("match=\"%s.dst==%s && %s && %s.dst==%d\"", l3Prefix, sourceIP,
		strings.ToLower(string(proto)), strings.ToLower(string(proto)), sourcePort)
	cmd := []string{"--id=@reject-acl", "create", "acl", "direction=from-lport", "priority=1000", aclMatch, "action=reject",
		fmt.Sprintf("name=%s", aclName)}
	if applyToPortGroup {
		cmd = append(cmd, "--", "add", "port_group", ovn.clusterPortGroupUUID, "acls", "@reject-acl")
	}
	if len(gwRouterExtSwitch) > 0 {
		cmd = append(cmd, "--", "add", "logical_switch", gwRouterExtSwitch, "acls", "@reject-acl")
	}
	aclUUID, stderr, err := util.RunOVNNbctl(cmd...)
	if err != nil {
		klog.Errorf("Failed to add LB: %s ACL: %s, %q, to cluster port group/switches, stderr: %s error: %v",
			lb, aclUUID, aclName, stderr, err)
		return "", err
	}

	// Associate ACL UUID with load balancer and ip+port so we can remove this ACL if
	// backends are re-added.
	ovn.setServiceACLToLB(lb, vip, aclUUID)

	return aclUUID, nil
}

func (ovn *Controller) deleteLoadBalancerRejectACL(lb, vip string) {
	aclUUID, hasEndpoints := ovn.getServiceLBInfo(lb, vip)
	if aclUUID == "" && !hasEndpoints {
		// If no ACL and does not have endpoints, we can assume there is no valid entry in the cache here as
		// this is an illegal state.
		// Determine and remove ACL by name.
		ip, port, err := util.SplitHostPortInt32(vip)
		if err != nil {
			klog.Errorf("Unable to parse vip for Reject ACL deletion: %v", err)
			return
		}
		aclName := loadbalancer.GenerateACLNameForOVNCommand(lb, ip, port)
		aclUUID, err = acl.GetACLByName(aclName)
		if err != nil {
			klog.Infof("Unable to delete Reject ACL for load-balancer: %s, vip: %s. No entry in cache and "+
				"error occurred while trying to find the ACL by name in OVN, error: %v", lb, vip, err)
			return
		}
		if aclUUID == "" {
			klog.Infof("No reject ACL found in cache or in OVN to remove for load-balancer: %s, vip: %s", lb, vip)
			return
		}
	} else if aclUUID == "" {
		// Must have endpoints and no reject ACL to remove
		klog.V(5).Infof("No reject ACL found to remove for load balancer: %s, vip: %s", lb, vip)
		return
	}
	// check if the load balancer is on a GR, if so we need to get the join/external switches
	gwRouterSwitch, err := ovn.getGRLogicalSwitchForLoadBalancer(lb)
	if err != nil {
		klog.Errorf("Unable to query logical switches for GR with load balancer: %s, error: %v", lb, err)
	} else {
		ovn.removeACLFromNodeSwitches([]string{gwRouterSwitch}, aclUUID)
	}
	ovn.removeACLFromPortGroup(lb, aclUUID)
	ovn.removeServiceACL(lb, vip)
}

// Remove the ACL uuid entry from Logical Switch acl's list.
func (ovn *Controller) removeACLFromNodeSwitches(switches []string, aclUUID string) {
	err := acl.RemoveACLFromNodeSwitches(switches, aclUUID)
	if err != nil {
		klog.Errorf("Failed to remove ACL %s from switches %v, error: %v", aclUUID, switches, err)
	}
}

func (ovn *Controller) removeACLFromPortGroup(lb, aclUUID string) {
	err := acl.RemoveACLFromPortGroup(aclUUID, ovn.clusterPortGroupUUID)
	if err != nil {
		klog.Errorf("Failed to remove reject ACL %s from LB %s: error: %v", aclUUID, lb, err)
	}
}
