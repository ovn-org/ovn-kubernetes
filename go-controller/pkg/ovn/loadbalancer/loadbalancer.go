package loadbalancer

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/pkg/errors"
	utilnet "k8s.io/utils/net"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	kapi "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

const (
	// used to indicate if the service is running on node load balancers
	NodeLoadBalancer = "NodeLoadBalancer"
	// used to indicate if the service is running on idling load balancers
	IdlingLoadBalancer = "IdlingLoadBalancer"
)

type NotFoundError struct {
	What string
}

func (e NotFoundError) Error() string {
	return e.What + " not found"
}

var LBNotFound = NotFoundError{What: "Load balancer"}

// finds any OVN Load Balancer based on external ID and value
func findOVNLoadBalancer(externalID, externalValue string) (string, error) {
	out, _, err := util.RunOVNNbctl("--data=bare",
		"--no-heading", "--columns=_uuid", "find", "load_balancer",
		"external_ids:"+externalID+"="+externalValue)
	if err != nil {
		return "", err
	}
	if out == "" {
		return "", LBNotFound
	}
	return out, nil
}

// getClusterExternalId gets the cluster LB's externalId and its value for a proto
func getClusterExternalId(protocol kapi.Protocol) (string, string) {
	return fmt.Sprintf("%s-%s", types.ClusterLBPrefix, strings.ToLower(string(protocol))), "yes"
}

// getIdlingExternalId returns the idling LB's externalId and its value for a proto
func getIdlingExternalId(protocol kapi.Protocol) (string, string) {
	return fmt.Sprintf("%s-%s", types.ClusterIdlingLBPrefix, strings.ToLower(string(protocol))), "yes"
}

// getGrExternalId returns the GR LB's externalId and its value for a proto
func getGrExternalId(protocol kapi.Protocol, gatewayRouter string) (string, string) {
	return fmt.Sprintf("%s_lb_gateway_router", protocol), gatewayRouter
}

// getWkrExternalId returns the Worker LB's externalId and its value for a proto
func getWkrExternalId(protocol kapi.Protocol, nodeName string) (string, string) {
	return fmt.Sprintf("%s-%s", types.WorkerLBPrefix, strings.ToLower(string(protocol))), nodeName
}

// GetClusterLoadBalancer returns the Cluster LB matching the protocol
func GetClusterLoadBalancer(protocol kapi.Protocol) (string, error) {
	externalId, externalValue := getClusterExternalId(protocol)
	return findOVNLoadBalancer(externalId, externalValue)
}

// GetIdlingLoadBalancer returns the Idling LoadBalancer matching the protocol
func GetIdlingLoadBalancer(protocol kapi.Protocol) (string, error) {
	externalId, externalValue := getIdlingExternalId(protocol)
	return findOVNLoadBalancer(externalId, externalValue)
}

// GetGatewayLoadBalancer returns the GR load balancer matching the protocol
func GetGatewayLoadBalancer(gatewayRouter string, protocol kapi.Protocol) (string, error) {
	externalId, externalValue := getGrExternalId(protocol, gatewayRouter)
	return findOVNLoadBalancer(externalId, externalValue)
}

// GetWorkerLoadBalancer returns the worker load balancer matching the protocol
func GetWorkerLoadBalancer(nodeName string, protocol kapi.Protocol) (string, error) {
	externalId, externalValue := getWkrExternalId(protocol, nodeName)
	return findOVNLoadBalancer(externalId, externalValue)
}

// Createk8sClusterLoadBalancer creates the k8s-cluster-lb if it doesn´t exist
// to avoid duplicate creation of loadbalancers with the same externalID
// If it exists it ensures the correct options are set
func CreateK8sClusterLoadBalancer(protocol kapi.Protocol, idkey string) (string, error) {
	var externalId, externalValue string
	// check if the reject option should be true for k8's cluster lb
	reject := true
	if idkey == types.ClusterIdlingLBPrefix {
		reject = false
		externalId, externalValue = getIdlingExternalId(protocol)
	} else {
		externalId, externalValue = getClusterExternalId(protocol)
	}
	optionsReject := fmt.Sprintf("options:reject=%t", reject)

	lbUUID, err := findOVNLoadBalancer(externalId, externalValue)
	if err != nil && !errors.Is(err, LBNotFound) {
		return "", errors.Wrapf(err, "Failed to find OVN load balancer for externalId %s=%s and protocol %s", externalId, externalValue, protocol)
	}

	// create the LB if it doesn't exist yet
	if lbUUID == "" {
		// Create the external ID for the K8's cluster Lbs
		externalIdStr := fmt.Sprintf("external_ids:%s=%s", externalId, externalValue)
		lbUUID, err = createLoadBalancer(protocol, externalIdStr, optionsReject, types.LBHairpinOptions)
		if err != nil {
			return "", errors.Wrapf(err, "Failed to create OVN load balancer for externalId %s=%s and protocol %s", externalId, externalValue, protocol)
		}
	} else {
		// set expected options for LB
		err = UpdateLoadBalancerOptions(lbUUID, optionsReject, types.LBHairpinOptions)
		if err != nil {
			return "", errors.Wrapf(err, "Failed to update OVN load balancer for externalId %s=%s and protocol %s", externalId, externalValue, protocol)
		}
	}

	return lbUUID, nil
}

// CreatePerNodeLoadBalancer creates the per node GR OR Worker loadbalancers
// (if nodeName is "" we make the GR Lbs) if they don't exist to avoid duplicate
// creation of loadbalancers with the same externalID
func CreatePerNodeLoadBalancer(protocol kapi.Protocol, gatewayRouter string, nodeName string) (string, error) {
	// Create the external ID for the per node cluster Lbs
	var externalId, externalValue string

	if nodeName == "" {
		externalId, externalValue = getGrExternalId(protocol, gatewayRouter)
	} else {
		externalId, externalValue = getWkrExternalId(protocol, nodeName)
	}

	lbUUID, err := findOVNLoadBalancer(externalId, externalValue)
	if err != nil && !errors.Is(err, LBNotFound) {
		return "", errors.Wrapf(err, "Failed to find OVN load balancer for externalId %s=%s and protocol %s", externalId, externalValue, protocol)
	}

	// create the LB if it dosn't exist yet, leave it alone if it does
	if lbUUID == "" {
		externalIdStr := fmt.Sprintf("external_ids:%s=%s", externalId, externalValue)
		lbUUID, err = createLoadBalancer(protocol, externalIdStr)
		if err != nil {
			return "", errors.Wrapf(err, "Failed to create Per node load balancer for externalId %s=%s and protocol %s", externalId, externalValue, protocol)
		}
		// For per node LBs no options are currently set, if there are in the future
		// set them here for upgrade safety
	}

	return lbUUID, nil
}

// createLoadBalancer creates a loadbalancer for the specified protocol
// all loadbalancers but idling ones reject packets for vips without endpoints by default
func createLoadBalancer(protocol kapi.Protocol, externalId string, options ...string) (string, error) {
	proto := fmt.Sprintf("protocol=%s", strings.ToLower(string(protocol)))

	cmd := []string{"create", "load_balancer", externalId, proto}
	cmd = append(cmd, options...)

	lbID, stderr, err := util.RunOVNNbctl(cmd...)
	if err != nil {
		klog.Errorf("Failed to create %s load balancer, stderr: %q, error: %v", protocol, stderr, err)
		return "", err
	}
	return lbID, nil
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
func DeleteLoadBalancerVIP(txn *util.NBTxn, loadBalancer, vip string) error {
	vipQuotes := fmt.Sprintf("\"%s\"", vip)
	request := []string{"--if-exists", "remove", "load_balancer", loadBalancer, "vips", vipQuotes}
	stdout, stderr, err := txn.AddOrCommit(request)
	if err != nil {
		// if we hit an error and fail to remove load balancer, we skip removing the rejectACL
		return fmt.Errorf("error in deleting load balancer vip %s for %s"+
			"stdout: %q, stderr: %q, error: %v",
			vip, loadBalancer, stdout, stderr, err)
	}
	return nil
}

// DeleteLoadBalancerVIPs removes the VIPs across lbs in a single shot
func DeleteLoadBalancerVIPs(txn *util.NBTxn, loadBalancers, vips []string) error {
	for _, loadBalancer := range loadBalancers {
		for _, vip := range vips {
			if err := DeleteLoadBalancerVIP(txn, loadBalancer, vip); err != nil {
				return err
			}
		}
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

func UpdateLoadBalancerOptions(lb string, options ...string) error {
	cmd := []string{"set", "load_balancer", lb}
	cmd = append(cmd, options...)

	out, stderr, err := util.RunOVNNbctl(cmd...)
	if err != nil {
		return fmt.Errorf("error in configuring options for load balancer: %s "+
			"stdout: %q, stderr: %q, error: %v", lb, out, stderr, err)
	}

	return nil
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

// CreateLoadBalancerVIPs either creates or updates a set of load balancer VIPs mapping
// from SourcePort on each IP of a given address family in sourceIPs, to TargetPort on
// each IP of the same address family in TargetIPs
func CreateLoadBalancerVIPs(lb string,
	sourceIPs []string, sourcePort int32,
	targetIPs []string, targetPort int32) error {
	klog.V(5).Infof("Creating lb with %s, [%v], %d, [%v], %d", lb, sourceIPs, sourcePort, targetIPs, targetPort)
	txn := util.NewNBTxn()
	for _, sourceIP := range sourceIPs {
		isIPv6 := utilnet.IsIPv6String(sourceIP)

		var targets []string
		for _, targetIP := range targetIPs {
			if utilnet.IsIPv6String(targetIP) == isIPv6 {
				targets = append(targets, util.JoinHostPortInt32(targetIP, targetPort))
			}
		}
		vip := util.JoinHostPortInt32(sourceIP, sourcePort)
		lbTarget := fmt.Sprintf(`vips:"%s"="%s"`, vip, strings.Join(targets, ","))
		request := []string{"set", "load_balancer", lb, lbTarget}
		_, stderr, err := txn.AddOrCommit(request)
		if err != nil {
			return fmt.Errorf("unable to create load balancer: stderr: %s, err: %v", stderr, err)
		}
	}
	_, stderr, err := txn.Commit()
	if err != nil {
		return fmt.Errorf("unable to create load balancer: stderr: %s, err: %v", stderr, err)
	}
	return nil
}

type Entry struct {
	LoadBalancer string
	SourceIPS    []string
	SourcePort   int32
	TargetIPs    []string
	TargetPort   int32
}

// BundleCreateLoadBalancerVIPs is the same as CreateLoadBalancerVIPs but batches multiple load balancer config
// together into a single transaction
// creates or updates a set of load balancer VIPs mapping
// from SourcePort on each IP of a given address family in sourceIPs, to TargetPort on
// each IP of the same address family in TargetIPs
func BundleCreateLoadBalancerVIPs(lbEntries []Entry) error {
	txn := util.NewNBTxn()
	for _, entry := range lbEntries {
		for _, sourceIP := range entry.SourceIPS {
			isIPv6 := utilnet.IsIPv6String(sourceIP)
			var targets []string
			for _, targetIP := range entry.TargetIPs {
				if utilnet.IsIPv6String(targetIP) == isIPv6 {
					targets = append(targets, util.JoinHostPortInt32(targetIP, entry.TargetPort))
				}
			}
			vip := util.JoinHostPortInt32(sourceIP, entry.SourcePort)
			lbTarget := fmt.Sprintf(`vips:"%s"="%s"`, vip, strings.Join(targets, ","))
			request := []string{"set", "load_balancer", entry.LoadBalancer, lbTarget}
			_, stderr, err := txn.AddOrCommit(request)
			if err != nil {
				return fmt.Errorf("unable to create load balancer bundle: stderr: %s, err: %v", stderr, err)
			}
		}
	}
	_, stderr, err := txn.Commit()
	if err != nil {
		return fmt.Errorf("unable to create load balancer bundle: stderr: %s, err: %v", stderr, err)
	}
	return nil
}
