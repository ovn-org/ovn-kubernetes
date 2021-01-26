package loadbalancer

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/pkg/errors"

	kapi "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

// OVN loadbalancers set the external_ids field to:
// external_ids = ${prefix}-${protocol}

const (
	// OvnLoadBalancerClusterIds represent the OVN loadbalancers
	// used for ClusterIP East-West traffic.
	// Default behaviour to reject traffic for VIPs without backends
	OvnLoadBalancerClusterIds = "k8s-cluster-lb"
	// OvnLoadBalancerIdlingIds represent the OVN loadbalancers
	// used for services that has been idled.
	// Default behaviour to send an event and drop the packet for VIPs without backends
	OvnLoadBalancerIdlingIds = "k8s-idling-lb"
)

// GetOVNKubeLoadBalancer returns the LoadBalancer matching the protocol and ids prefix
func GetOVNKubeLoadBalancer(protocol kapi.Protocol, idkey string) (string, error) {
	id := fmt.Sprintf("external_ids:%s-%s=yes", idkey, strings.ToLower(string(protocol)))
	out, _, err := util.RunOVNNbctl("--data=bare", "--no-heading", "--columns=_uuid",
		"find", "load_balancer", id)
	if err != nil {
		return "", err
	}
	if out == "" {
		return "", fmt.Errorf("no load balancer found in the database")
	}
	return out, nil
}

// createLoadBalancer creates a loadbalancer for the specified protocol
// all loadbalancers but idling ones reject packets for vips without endpoints by default
func createLoadBalancer(protocol kapi.Protocol, idkey string) error {
	id := fmt.Sprintf("external_ids:%s-%s=yes", idkey, strings.ToLower(string(protocol)))
	proto := fmt.Sprintf("protocol=%s", strings.ToLower(string(protocol)))
	reject := true
	if idkey == OvnLoadBalancerIdlingIds {
		reject = false
	}
	options := fmt.Sprintf("options:reject=%t", reject)
	_, stderr, err := util.RunOVNNbctl("--", "create", "load_balancer", id, proto, options)
	if err != nil {
		klog.Errorf("Failed to create %s load balancer, stderr: %q, error: %v", protocol, stderr, err)
		return err
	}
	return nil
}

// CreateLoadBalancer creates the loadbalancer if it doesn´t exist to avoid
// consumers to create duplicate loadbalancers with the same name and externalID
func CreateLoadBalancer(protocol kapi.Protocol, idkey string) error {
	lbUUID, err := GetOVNKubeLoadBalancer(protocol, idkey)
	if err != nil && err.Error() != "no load balancer found in the database" {
		return errors.Wrapf(err, "Failed to get OVN load balancer for protocol %s", protocol)
	}
	// create the load balancer if it doesn't exist yet
	if lbUUID == "" {
		err := createLoadBalancer(protocol, idkey)
		if err != nil {
			return errors.Wrapf(err, "Failed to create OVN load balancer for protocol %s", protocol)
		}
	}
	return nil
}

// GetLoadBalancerVIPs returns a map whose keys are VIPs (IP:port) on loadBalancer
func GetLoadBalancerVIPs(loadBalancer string) (map[string]string, error) {
	var vips map[string]string
	outStr, _, err := util.RunOVNNbctl("--data=bare", "--no-heading",
		"get", "load_balancer", loadBalancer, "vips")
	if err != nil {
		return nil, err
	}
	if outStr == "" {
		return vips, nil
	}
	// sample outStr:
	// - {"192.168.0.1:80"="10.1.1.1:80,10.2.2.2:80"}
	// - {"[fd01::]:80"="[fd02::]:80,[fd03::]:80"}
	outStrMap := strings.Replace(outStr, "=", ":", -1)
	err = json.Unmarshal([]byte(outStrMap), &vips)
	if err != nil {
		return nil, err
	}
	return vips, nil
}

// DeleteLoadBalancerVIP removes the VIP as well as any reject ACLs associated to the LB
func DeleteLoadBalancerVIP(loadBalancer, vip string) error {
	vipQuotes := fmt.Sprintf("\"%s\"", vip)
	stdout, stderr, err := util.RunOVNNbctl("--if-exists", "remove", "load_balancer", loadBalancer, "vips", vipQuotes)
	if err != nil {
		// if we hit an error and fail to remove load balancer, we skip removing the rejectACL
		return fmt.Errorf("error in deleting load balancer vip %s for %s"+
			"stdout: %q, stderr: %q, error: %v",
			vip, loadBalancer, stdout, stderr, err)
	}
	return nil
}

// UpdateLoadBalancer updates the VIP for sourceIP:sourcePort to point to targets (an
// array of IP:port strings)
func UpdateLoadBalancer(lb, vip string, targets []string) error {
	lbTarget := fmt.Sprintf(`vips:"%s"="%s"`, vip, strings.Join(targets, ","))

	out, stderr, err := util.RunOVNNbctl("set", "load_balancer", lb, lbTarget)
	if err != nil {
		return fmt.Errorf("error in configuring load balancer: %s "+
			"stdout: %q, stderr: %q, error: %v", lb, out, stderr, err)
	}

	return nil
}

// MigrateLoadBalancerVIP migrates a VIP from one loadbalancer to other
// regarding if the VIP exists or no in the original loadbalancer it always
// updates the destination loadbalancer.
func MigrateLoadBalancerVIP(lbOrig, lbDest, vip string, targets []string) error {
	// deleting must not fail if the vip doesn´t exist
	err := DeleteLoadBalancerVIP(lbOrig, vip)
	if err != nil {
		return err
	}
	return UpdateLoadBalancer(lbDest, vip, targets)
}

// GetLogicalSwitchesForLoadBalancer get the switches associated to a LoadBalancer
func GetLogicalSwitchesForLoadBalancer(lb string) ([]string, error) {
	out, _, err := util.RunOVNNbctl("--data=bare", "--no-heading",
		"--columns=_uuid", "find",
		"logical_switch", fmt.Sprintf("load_balancer{>=}%s", lb))
	if err != nil {
		return nil, err
	}
	return strings.Fields(out), nil
}

// GetLogicalRoutersForLoadBalancer get the routers associated to a LoadBalancer
func GetLogicalRoutersForLoadBalancer(lb string) ([]string, error) {
	out, _, err := util.RunOVNNbctl("--data=bare", "--no-heading",
		"--columns=name", "find",
		"logical_router", fmt.Sprintf("load_balancer{>=}%s", lb))
	if err != nil {
		return nil, err
	}
	return strings.Fields(out), nil
}

// GetGRLogicalSwitchForLoadBalancer returns the external switch name if the load balancer is on a GR
func GetGRLogicalSwitchForLoadBalancer(lb string) (string, error) {
	routers, err := GetLogicalRoutersForLoadBalancer(lb)
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

// GenerateACLName generates a deterministic ACL name based on the load_balancer parameters
func GenerateACLName(lb string, sourceIP string, sourcePort int32) string {
	aclName := fmt.Sprintf("%s-%s:%d", lb, sourceIP, sourcePort)
	// ACL names are limited to 63 characters
	if len(aclName) > 63 {
		var ipPortLen int
		srcPortStr := fmt.Sprintf("%d", sourcePort)
		// Add the length of the IP (max 15 with periods, max 39 with colons),
		// plus length of sourcePort (max 5 char),
		// plus 1 for additional ':' to separate,
		// plus 1 for '-' between lb and IP.
		// With full IPv6 address and 5 char port, max ipPortLen is 62
		// With full IPv4 address and 5 char port, max ipPortLen is 24.
		ipPortLen = len(sourceIP) + len(srcPortStr) + 1 + 1
		lbTrim := 63 - ipPortLen
		// Shorten the Load Balancer name to allow full IP:port
		tmpLb := lb[:lbTrim]
		klog.Infof("Limiting ACL Name from %s to %s-%s:%d to keep under 63 characters", aclName, tmpLb, sourceIP, sourcePort)
		aclName = fmt.Sprintf("%s-%s:%d", tmpLb, sourceIP, sourcePort)
	}
	return aclName
}

// GenerateACLNameForOVNCommand sanitize the ACL name because the generateACLName
// function was including backslash escapes for the ACL
// name for use in OVN commands that have trouble with literal ":". That
// was causing a mismatch when services were syncing because the name
// actually returned from an OVN command does not include any backslashes
// so the names would not match. #1749
func GenerateACLNameForOVNCommand(lb string, sourceIP string, sourcePort int32) string {
	return strings.ReplaceAll(GenerateACLName(lb, sourceIP, sourcePort), ":", "\\:")
}
