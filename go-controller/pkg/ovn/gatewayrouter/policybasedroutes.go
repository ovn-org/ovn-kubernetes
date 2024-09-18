package gatewayrouter

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	utilnet "k8s.io/utils/net"

	"github.com/ovn-org/libovsdb/client"

	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

type PolicyBasedRoutesManager struct {
	clusterRouterName string
	netInfo           util.NetInfo
	nbClient          client.Client
}

func NewPolicyBasedRoutesManager(nbClient client.Client, clusterRouterName string, netInfo util.NetInfo) *PolicyBasedRoutesManager {
	return &PolicyBasedRoutesManager{
		clusterRouterName: clusterRouterName,
		netInfo:           netInfo,
		nbClient:          nbClient,
	}
}

func (pbr *PolicyBasedRoutesManager) AddSameNodeIPPolicy(nodeName, mgmtPortIP string, hostIfCIDR *net.IPNet, otherHostAddrs []string) error {
	if hostIfCIDR == nil {
		return fmt.Errorf("<nil> host interface CIDR")
	}
	if mgmtPortIP == "" || net.ParseIP(mgmtPortIP) == nil {
		return fmt.Errorf("invalid management port IP address: %q", mgmtPortIP)
	}
	if len(hostIfCIDR.IP) == 0 {
		return fmt.Errorf("invalid host address: %v", hostIfCIDR.String())
	}
	if !isHostIPsValid(otherHostAddrs) {
		return fmt.Errorf("invalid other host address(es): %v", otherHostAddrs)
	}
	l3Prefix := getIPCIDRPrefix(hostIfCIDR)
	matches := sets.New[string]()
	for _, hostIP := range append(otherHostAddrs, hostIfCIDR.IP.String()) {
		// embed nodeName as comment so that it is easier to delete these rules later on.
		// logical router policy doesn't support external_ids to stash metadata
		networkScopedSwitchName := pbr.netInfo.GetNetworkScopedSwitchName(nodeName)
		matchStr := generateNodeIPMatch(networkScopedSwitchName, l3Prefix, hostIP)
		matches = matches.Insert(matchStr)
	}

	if err := pbr.sync(
		nodeName,
		matches,
		ovntypes.NodeSubnetPolicyPriority,
		mgmtPortIP,
	); err != nil {
		return fmt.Errorf("unable to sync node subnet policies, err: %v", err)
	}

	return nil
}

// AddHostCIDRPolicy adds the following policy in local-gateway-mode for UDN L2 topology to the GR
// 99 ip4.dst == 172.18.0.0/16 && ip4.src == 10.100.200.0/24         reroute              10.100.200.2
// Since rtoe of GR is directly connected to the hostCIDR range in LGW even with the following
// reroute to mp0 src-ip route on GR that we add from syncNodeManagementPort:
// 10.100.200.0/24    10.100.200.2 src-ip
// the dst-ip based default OVN route takes precedence because the primary nodeCIDR range is a
// directly attached network to the OVN router and sends the traffic destined for other nodes to rtoe
// and via br-ex to outside in LGW which is not desired.
// Hence we need a LRP that sends all traffic destined to that primary nodeCIDR range that reroutes
// it to mp0 in LGW mode to override this directly attached network OVN route.
func (pbr *PolicyBasedRoutesManager) AddHostCIDRPolicy(node *v1.Node, mgmtPortIP, clusterPodSubnet string) error {
	if mgmtPortIP == "" || net.ParseIP(mgmtPortIP) == nil {
		return fmt.Errorf("invalid management port IP address: %q", mgmtPortIP)
	}
	// we only care about the primary node family since GR's port has that IP
	// we don't care about secondary nodeIPs here which is why we are not using
	// the hostCIDR annotation
	primaryIfAddrs, err := util.GetNodeIfAddrAnnotation(node)
	if err != nil {
		return fmt.Errorf("failed to get primaryIP for node %s, err: %v", node.Name, err)
	}
	nodePrimaryStringPrefix := primaryIfAddrs.IPv4
	if utilnet.IsIPv6String(mgmtPortIP) {
		nodePrimaryStringPrefix = primaryIfAddrs.IPv6
	}
	_, nodePrimaryCIDRPrefix, err := net.ParseCIDR(nodePrimaryStringPrefix)
	if nodePrimaryStringPrefix == "" || err != nil || nodePrimaryCIDRPrefix == nil {
		return fmt.Errorf("invalid host CIDR prefix: prefixString: %q, prefixCIDR: %q, error: %v",
			nodePrimaryStringPrefix, nodePrimaryCIDRPrefix, err)
	}
	ovnPrefix := getIPCIDRPrefix(nodePrimaryCIDRPrefix)
	matchStr := generateHostCIDRMatch(ovnPrefix, nodePrimaryCIDRPrefix.String(), clusterPodSubnet)
	if err := pbr.createPolicyBasedRoutes(matchStr, ovntypes.UDNHostCIDRPolicyPriority, mgmtPortIP); err != nil {
		return fmt.Errorf("failed to add host-cidr policy route '%s' on host %q on %s "+
			"error: %v", matchStr, node.Name, pbr.clusterRouterName, err)
	}

	return nil
}

// This function syncs logical router policies given various criteria
// This function compares the following ovn-nbctl output:

// either

// 		72db5e49-0949-4d00-93e3-fe94442dd861,ip4.src == 10.244.0.2 && ip4.dst == 172.18.0.2 /* ovn-worker2 */,169.254.0.1
// 		6465e223-053c-4c74-a5f0-5f058c9f7a3e,ip4.src == 10.244.2.2 && ip4.dst == 172.18.0.3 /* ovn-worker */,169.254.0.1
// 		7debdcc6-ad5e-4825-9978-74bfbf8b7c27,ip4.src == 10.244.1.2 && ip4.dst == 172.18.0.4 /* ovn-control-plane */,169.254.0.1

// or

// 		c20ac671-704a-428a-a32b-44da2eec8456,"inport == ""rtos-ovn-worker2"" && ip4.dst == 172.18.0.2 /* ovn-worker2 */",10.244.0.2
// 		be7c8b53-f8ac-4051-b8f1-bfdb007d0956,"inport == ""rtos-ovn-worker"" && ip4.dst == 172.18.0.3 /* ovn-worker */",10.244.2.2
// 		fa8cf55d-a96c-4a53-9bf2-1c1fb1bc7a42,"inport == ""rtos-ovn-control-plane"" && ip4.dst == 172.18.0.4 /* ovn-control-plane */",10.244.1.2

// or

// 		822ab242-cce5-47b2-9c6f-f025f47e766a,ip4.src == 10.244.2.2  && ip4.dst != 10.244.0.0/16 /* inter-ovn-worker */,169.254.0.1
// 		a1b876f6-5ed4-4f88-b09c-7b4beed3b75f,ip4.src == 10.244.1.2  && ip4.dst != 10.244.0.0/16 /* inter-ovn-control-plane */,169.254.0.1
// 		0f5af297-74c8-4551-b10e-afe3b74bb000,ip4.src == 10.244.0.2  && ip4.dst != 10.244.0.0/16 /* inter-ovn-worker2 */,169.254.0.1

// The function checks to see if the mgmtPort IP has changed, or if match criteria has changed
// and removes stale policies for a node for the NodeSubnetPolicy in SGW.
// TODO: Fix the MGMTPortPolicy's and InterNodePolicy's ip4.src fields if the mgmtPort IP has changed in LGW.
// It also adds new policies for a node at a specific priority.
// This is ugly (since the node is encoded as a comment in the match),
// but a necessary evil as any change to this would break upgrades and
// possible downgrades. We could make sure any upgrade encodes the node in
// the external_id, but since ovn-kubernetes isn't versioned, we won't ever
// know which version someone is running of this and when the switch to version
// N+2 is fully made.
func (pbr *PolicyBasedRoutesManager) sync(nodeName string, matches sets.Set[string], priority, nexthop string) error {
	// create a map to track matches found
	matchTracker := sets.New(sets.List(matches)...)

	if priority == ovntypes.NodeSubnetPolicyPriority {
		policies, err := pbr.findPolicyBasedRoutes(priority)
		if err != nil {
			return fmt.Errorf("unable to list policies, err: %v", err)
		}

		// sync and remove unknown policies for this node/priority
		// also flag if desired policies are already found
		for _, policy := range policies {
			if strings.Contains(policy.Match, fmt.Sprintf("%s\"", nodeName)) {
				// if the policy is for this node and has the wrong mgmtPortIP as nexthop, remove it
				// FIXME we currently assume that foundNexthops is a single ip, this may
				// change in the future.
				if len(policy.Nexthops) == 0 {
					return fmt.Errorf("invalid policy without a next hop")
				}
				if utilnet.IsIPv6String(policy.Nexthops[0]) != utilnet.IsIPv6String(nexthop) {
					continue
				}
				if policy.Nexthops[0] != nexthop {
					if err := pbr.deletePolicyBasedRoutes(policy.UUID); err != nil {
						return fmt.Errorf("failed to delete policy route '%s' for host %q on %s "+
							"error: %v", policy.UUID, nodeName, pbr.clusterRouterName, err)
					}
					continue
				}
				desiredMatchFound := false
				for match := range matchTracker {
					if strings.Contains(policy.Match, match) {
						desiredMatchFound = true
						break
					}
				}
				// if the policy is for this node/priority and does not contain a valid match, remove it
				if !desiredMatchFound {
					if err := pbr.deletePolicyBasedRoutes(policy.UUID); err != nil {
						return fmt.Errorf("failed to delete policy route '%s' for host %q on %s "+
							"error: %v", policy.UUID, nodeName, pbr.clusterRouterName, err)
					}
					continue
				}
				// now check if the existing policy matches, remove it
				matchTracker.Delete(policy.Match)
			}
		}
	}

	// cycle through all of the not found match criteria and create new policies
	for match := range matchTracker {
		if err := pbr.createPolicyBasedRoutes(match, priority, nexthop); err != nil {
			return fmt.Errorf("failed to add policy route '%s' for host %q on %s "+
				"error: %v", match, nodeName, pbr.clusterRouterName, err)
		}
	}
	return nil
}

func (pbr *PolicyBasedRoutesManager) findPolicyBasedRoutes(priority string) ([]*nbdb.LogicalRouterPolicy, error) {
	intPriority, _ := strconv.Atoi(priority)
	networkName := pbr.netInfo.GetNetworkName()
	p := func(item *nbdb.LogicalRouterPolicy) bool {
		itemNetworkName, isSecondaryNetwork := item.ExternalIDs[ovntypes.NetworkExternalID]
		if !isSecondaryNetwork {
			itemNetworkName = ovntypes.DefaultNetworkName
		}
		return itemNetworkName == networkName && item.Priority == intPriority
	}
	logicalRouterStaticPolicies, err := libovsdbops.FindLogicalRouterPoliciesWithPredicate(pbr.nbClient, p)
	if err != nil {
		return nil, fmt.Errorf("unable to find logical router policy: %v", err)
	}

	return logicalRouterStaticPolicies, nil
}

func (pbr *PolicyBasedRoutesManager) deletePolicyBasedRoutes(policyID string) error {
	lrp := nbdb.LogicalRouterPolicy{UUID: policyID}
	err := libovsdbops.DeleteLogicalRouterPolicies(pbr.nbClient, pbr.clusterRouterName, &lrp)
	if err != nil {
		return fmt.Errorf("error deleting policy %s: %v", policyID, err)
	}

	return nil
}

func (pbr *PolicyBasedRoutesManager) createPolicyBasedRoutes(match, priority, nexthops string) error {
	intPriority, err := strconv.Atoi(priority)
	if err != nil {
		return fmt.Errorf("failed to convert priority %q to string: %v", priority, err)
	}
	lrp := nbdb.LogicalRouterPolicy{
		Priority: intPriority,
		Match:    match,
		Nexthops: []string{nexthops},
		Action:   nbdb.LogicalRouterPolicyActionReroute,
	}
	if pbr.netInfo.IsSecondary() {
		lrp.ExternalIDs = map[string]string{
			ovntypes.NetworkExternalID:  pbr.netInfo.GetNetworkName(),
			ovntypes.TopologyExternalID: pbr.netInfo.TopologyType(),
		}
	}

	p := func(item *nbdb.LogicalRouterPolicy) bool {
		// the match criteria being passed around already features the LRP name scoped to the network
		return item.Priority == lrp.Priority && item.Match == lrp.Match
	}

	err = libovsdbops.CreateOrUpdateLogicalRouterPolicyWithPredicate(pbr.nbClient, pbr.clusterRouterName, &lrp, p,
		&lrp.Nexthops, &lrp.Action)
	if err != nil {
		return fmt.Errorf("error creating policy %+v on router %s: %v", lrp, pbr.clusterRouterName, err)
	}

	return nil
}

func generateNodeIPMatch(switchName, ipPrefix, hostIP string) string {
	return fmt.Sprintf(`inport == "%s%s" && %s.dst == %s /* %s */`, ovntypes.RouterToSwitchPrefix, switchName, ipPrefix, hostIP, switchName)
}

func generateHostCIDRMatch(ipPrefix, nodePrimaryCIDRPrefix, clusterPodSubnetPrefix string) string {
	return fmt.Sprintf(`%s.dst == %s && %s.src == %s`, ipPrefix, nodePrimaryCIDRPrefix, ipPrefix, clusterPodSubnetPrefix)
}

func getIPCIDRPrefix(cidr *net.IPNet) string {
	if utilnet.IsIPv6CIDR(cidr) {
		return "ip6"
	}
	return "ip4"
}

func isHostIPsValid(ips []string) bool {
	for _, ipStr := range ips {
		ip := net.ParseIP(ipStr)
		if len(ip) == 0 {
			return false
		}
	}
	return true
}
