package logical_router_policy

import (
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/batching"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	libovsdb "github.com/ovn-org/libovsdb/ovsdb"
	"k8s.io/klog/v2"
	utilsnet "k8s.io/utils/net"
)

type LRPSyncer struct {
	nbClient       libovsdbclient.Client
	controllerName string
}

type egressIPNoReroutePolicyName string // the following are redefined from ovn pkg to prevent circ import violation
type egressIPFamilyValue string

var (
	replyTrafficNoReroute egressIPNoReroutePolicyName = "EIP-No-Reroute-reply-traffic"
	noReRoutePodToPod     egressIPNoReroutePolicyName = "EIP-No-Reroute-Pod-To-Pod"
	noReRoutePodToJoin    egressIPNoReroutePolicyName = "EIP-No-Reroute-Pod-To-Join"
	NoReRoutePodToNode    egressIPNoReroutePolicyName = "EIP-No-Reroute-Pod-To-Node"
	v4IPFamilyValue       egressIPFamilyValue         = "ip4"
	v6IPFamilyValue       egressIPFamilyValue         = "ip6"
	ipFamilyValue         egressIPFamilyValue         = "ip" // use it when its dualstack
	defaultNetworkName                                = "default"
)

// NewLRPSyncer adds owner references to a limited subnet of LRPs. controllerName is the name of the new controller that should own all LRPs without controller
func NewLRPSyncer(nbClient libovsdbclient.Client, controllerName string) *LRPSyncer {
	return &LRPSyncer{
		nbClient:       nbClient,
		controllerName: controllerName,
	}
}

func (syncer *LRPSyncer) Sync() error {
	v4JoinSubnet, v6JoinSubnet := getJoinSubnets()
	v4ClusterSubnets, v6ClusterSubnets := getClusterSubnets()
	if (len(v4ClusterSubnets) != 0 && v4JoinSubnet == nil) || (v4JoinSubnet != nil && len(v4ClusterSubnets) == 0) {
		return fmt.Errorf("invalid IPv4 config - v4 cluster and join subnet must be valid")
	}
	if (len(v6ClusterSubnets) != 0 && v6JoinSubnet == nil) || (v6JoinSubnet != nil && len(v6ClusterSubnets) == 0) {
		return fmt.Errorf("invalid IPv6 config - v6 cluster and join subnet must be valid")
	}
	if len(v4ClusterSubnets) == 0 && len(v6ClusterSubnets) == 0 {
		return fmt.Errorf("IPv4 or IPv6 cluster subnets must be defined")
	}
	if err := syncer.syncEgressIPNoReroutes(v4ClusterSubnets, v6ClusterSubnets, v4JoinSubnet, v6JoinSubnet); err != nil {
		return fmt.Errorf("failed to sync Logical Router Policies no reroutes: %v", err)
	}
	if err := syncer.syncEgressIPReRoutes(); err != nil {
		return fmt.Errorf("failed to sync Logical Router Policies reroutes: %v", err)
	}
	return nil
}

func (syncer *LRPSyncer) syncEgressIPReRoutes() error {
	v4PodsNetInfo, v6PodsNetInfo, err := syncer.buildCDNPodCache()
	if err != nil {
		return fmt.Errorf("failed to build CDN pod cache from NB DB: %v", err)
	}
	noOwnerFn := libovsdbops.GetNoOwnerPredicate[*nbdb.LogicalRouterPolicy]()
	p := func(item *nbdb.LogicalRouterPolicy) bool {
		return item.Priority == ovntypes.EgressIPReroutePriority && noOwnerFn(item) && item.Match != "" && item.ExternalIDs["name"] != ""
	}
	lrpList, err := libovsdbops.FindALogicalRouterPoliciesWithPredicate(syncer.nbClient, ovntypes.OVNClusterRouter, p)
	if err != nil {
		if errors.Is(err, libovsdbclient.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("failed to find Logical Router Policies for %s: %v", ovntypes.OVNClusterRouter, err)
	}

	err = batching.Batch[*nbdb.LogicalRouterPolicy](50, lrpList, func(batchLRPs []*nbdb.LogicalRouterPolicy) error {
		var ops []libovsdb.Operation
		for _, lrp := range lrpList {
			eipName := lrp.ExternalIDs["name"]
			podIP := extractIPFromEIPReRouteMatch(lrp.Match)
			if podIP == nil {
				return fmt.Errorf("failed to extract IP from LRP: %v", lrp)
			}
			isIPv6 := utilsnet.IsIPv6(podIP)
			cache := v4PodsNetInfo
			if isIPv6 {
				cache = v6PodsNetInfo
			}
			podInfo, err := cache.getPod(podIP)
			if err != nil {
				klog.Infof("Failed to find Logical Switch Port cache entry for pod IP %s: %v", podIP.String(), err)
				continue
			}
			ipFamily := getIPFamily(isIPv6)
			lrp.ExternalIDs = getEgressIPLRPReRouteDbIDs(eipName, podInfo.namespace, podInfo.name, ipFamily, defaultNetworkName, syncer.controllerName).GetExternalIDs()
			ops, err = libovsdbops.UpdateLogicalRouterPoliciesOps(syncer.nbClient, ops, lrp)
			if err != nil {
				return fmt.Errorf("failed to create logical router policy update ops: %v", err)
			}
		}
		_, err = libovsdbops.TransactAndCheck(syncer.nbClient, ops)
		if err != nil {
			return fmt.Errorf("failed to transact EgressIP LRP reroute sync ops: %v", err)
		}
		return nil
	})
	return err
}

type podNetInfo struct {
	ip        net.IP
	namespace string
	name      string
}

type podsNetInfo []podNetInfo

func (ps podsNetInfo) getPod(ip net.IP) (podNetInfo, error) {
	for _, p := range ps {
		if p.ip.Equal(ip) {
			return p, nil
		}
	}
	return podNetInfo{}, fmt.Errorf("failed to find a pod matching IP %v", ip)
}

func (syncer *LRPSyncer) buildCDNPodCache() (podsNetInfo, podsNetInfo, error) {
	p := func(item *nbdb.LogicalSwitchPort) bool {
		return item.ExternalIDs["pod"] == "true" && item.ExternalIDs[ovntypes.NADExternalID] == "" // ignore secondary network LSPs
	}
	lsps, err := libovsdbops.FindLogicalSwitchPortWithPredicate(syncer.nbClient, p)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get logical switch ports: %v", err)
	}
	v4Pods, v6Pods := make(podsNetInfo, 0), make(podsNetInfo, 0)
	for _, lsp := range lsps {
		namespaceName, podName := util.GetNamespacePodFromCDNPortName(lsp.Name)
		if namespaceName == "" || podName == "" {
			klog.Errorf("Failed to extract namespace / pod from logical switch port %s", lsp.Name)
			continue
		}
		if len(lsp.Addresses) == 0 {
			klog.Errorf("Address(es) not set for pod %s/%s", namespaceName, podName)
			continue
		}
		for i := 1; i < len(lsp.Addresses); i++ {
			// CIDR is supported field within OVN, but for CDN we only set IP
			ip := net.ParseIP(lsp.Addresses[i])
			if ip == nil {
				klog.Errorf("Failed to extract IP %q from logical switch port for pod %s/%s", lsp.Addresses[i], namespaceName, podName)
				continue
			}
			if utilsnet.IsIPv6(ip) {
				v6Pods = append(v6Pods, podNetInfo{ip: ip, namespace: namespaceName, name: podName})
			} else {
				v4Pods = append(v4Pods, podNetInfo{ip: ip, namespace: namespaceName, name: podName})
			}
		}
	}

	return v4Pods, v6Pods, nil
}

// syncEgressIPNoReroutes syncs egress IP LRPs at priority 102 associated with CDN ovn_cluster_router and adds owner references.
// The aforementioned priority LRPs are used to skip egress IP for pod to pod, pod to join and pod to node traffic.
// There is also a no reroute reply traffic which ensures any traffic which is a response/reply from the egressIP pods
// will not be re-routed to egress-nodes. This LRP already contains owner references and does not need to be sync'd.
// Sample of the 3 IPv4 LRPs before sync that do not contain owner references:
// pod to pod:
// _uuid               : 3ef68e05-8ed6-4cb7-847a-39dd1b190804
// action              : allow
// bfd_sessions        : []
// external_ids        : {}
// match               : "ip4.src == 10.244.0.0/16 && ip4.dst == 10.244.0.0/16"
// nexthop             : []
// nexthops            : []
// options             : {}
// priority            : 102
// pod to join:
// _uuid               : c7341960-188d-465c-9c39-020af9e60033
// action              : allow
// bfd_sessions        : []
// external_ids        : {}
// match               : "ip4.src == 10.244.0.0/16 && ip4.dst == 100.64.0.0/16"
// nexthop             : []
// nexthops            : []
// options             : {}
// priority            : 102
// pod to node
// _uuid               : 59b61efc-2a60-4fcc-ae60-391c3d6f5c40
// action              : allow
// bfd_sessions        : []
// external_ids        : {}
// match               : "(ip4.src == $a4548040316634674295 || ip4.src == $a13607449821398607916) && ip4.dst == $a14918748166599097711"
// nexthop             : []
// nexthops            : []
// options             : {pkt_mark="1008"}
// priority            : 102
//
// Following sync, owner references are added.
func (syncer *LRPSyncer) syncEgressIPNoReroutes(v4ClusterSubnets, v6ClusterSubnets []*net.IPNet, v4JoinSubnet, v6JoinSubnet *net.IPNet) error {
	if err := syncer.syncEgressIPNoReroutePodToJoin(v4ClusterSubnets, v6ClusterSubnets, v4JoinSubnet, v6JoinSubnet); err != nil {
		return fmt.Errorf("failed to sync pod to join subnets: %v", err)
	}
	if err := syncer.syncEgressIPNoReroutePodToPod(v4ClusterSubnets, v6ClusterSubnets); err != nil {
		return fmt.Errorf("failed to sync pod to pod subnets: %v", err)
	}
	if err := syncer.syncEgressIPNoReroutePodToNode(); err != nil {
		return fmt.Errorf("failed to sync pod to node subnets: %v", err)
	}

	return nil
}

func (syncer *LRPSyncer) syncEgressIPNoReroutePodToJoin(v4ClusterSubnets, v6ClusterSubnets []*net.IPNet, v4JoinSubnet, v6JoinSubnet *net.IPNet) error {
	noOwnerFn := libovsdbops.GetNoOwnerPredicate[*nbdb.LogicalRouterPolicy]()
	p := func(item *nbdb.LogicalRouterPolicy) bool {
		return item.Priority == ovntypes.DefaultNoRereoutePriority && noOwnerFn(item) && item.Match != "" && !strings.Contains(item.Match, "$")
	}
	lrpList, err := libovsdbops.FindALogicalRouterPoliciesWithPredicate(syncer.nbClient, ovntypes.OVNClusterRouter, p)
	if err != nil {
		if errors.Is(err, libovsdbclient.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("failed to find Logical Router Policies for %s: %v", ovntypes.OVNClusterRouter, err)
	}
	var ops []libovsdb.Operation
	for _, lrp := range lrpList {
		ipNets := extractCIDRs(lrp.Match)
		if len(ipNets) != 2 {
			continue
		}
		clusterCIDR := ipNets[0]
		joinCIDR := ipNets[1]
		isIPV6 := utilsnet.IsIPv6CIDR(clusterCIDR)
		ipFamily := getIPFamily(isIPV6)
		if isIPV6 {
			if !containsIPNet(v6ClusterSubnets, clusterCIDR) || !util.IsIPNetEqual(v6JoinSubnet, joinCIDR) {
				continue
			}
		} else {
			if !containsIPNet(v4ClusterSubnets, clusterCIDR) || !util.IsIPNetEqual(v4JoinSubnet, joinCIDR) {
				continue
			}
		}
		lrp.ExternalIDs = getEgressIPLRPNoReRoutePodToJoinDbIDs(ipFamily, defaultNetworkName, syncer.controllerName).GetExternalIDs()
		ops, err = libovsdbops.UpdateLogicalRouterPoliciesOps(syncer.nbClient, ops, lrp)
		if err != nil {
			return fmt.Errorf("failed to create logical router policy update ops: %v", err)
		}
	}
	_, err = libovsdbops.TransactAndCheck(syncer.nbClient, ops)
	if err != nil {
		return fmt.Errorf("failed to transact pod to join subnet sync ops: %v", err)
	}
	return nil
}

func (syncer *LRPSyncer) syncEgressIPNoReroutePodToPod(v4ClusterSubnets, v6ClusterSubnets []*net.IPNet) error {
	noOwnerFn := libovsdbops.GetNoOwnerPredicate[*nbdb.LogicalRouterPolicy]()
	p := func(item *nbdb.LogicalRouterPolicy) bool {
		return item.Priority == ovntypes.DefaultNoRereoutePriority && noOwnerFn(item) && item.Match != "" && !strings.Contains(item.Match, "$")
	}
	lrpList, err := libovsdbops.FindALogicalRouterPoliciesWithPredicate(syncer.nbClient, ovntypes.OVNClusterRouter, p)
	if err != nil {
		if errors.Is(err, libovsdbclient.ErrNotFound) {
			return nil
		}
		return err
	}
	var ops []libovsdb.Operation
	for _, lrp := range lrpList {
		ipNets := extractCIDRs(lrp.Match)
		if len(ipNets) != 2 {
			continue
		}
		clusterCIDR1 := ipNets[0]
		clusterCIDR2 := ipNets[1]
		if !util.IsIPNetEqual(clusterCIDR1, clusterCIDR2) {
			continue
		}
		isIPV6 := utilsnet.IsIPv6CIDR(clusterCIDR1)
		var ipFamily egressIPFamilyValue
		if isIPV6 {
			ipFamily = v6IPFamilyValue
			if !containsIPNet(v6ClusterSubnets, clusterCIDR1) {
				continue
			}
		} else {
			ipFamily = v4IPFamilyValue
			if !containsIPNet(v4ClusterSubnets, clusterCIDR1) {
				continue
			}
		}
		lrp.ExternalIDs = getEgressIPLRPNoReRoutePodToPodDbIDs(ipFamily, defaultNetworkName, syncer.controllerName).GetExternalIDs()
		ops, err = libovsdbops.UpdateLogicalRouterPoliciesOps(syncer.nbClient, ops, lrp)
		if err != nil {
			return fmt.Errorf("failed to create logical router policy ops: %v", err)
		}
	}
	_, err = libovsdbops.TransactAndCheck(syncer.nbClient, ops)
	if err != nil {
		return fmt.Errorf("failed to transact pod to pod subnet sync ops: %v", err)
	}
	return nil
}

func (syncer *LRPSyncer) syncEgressIPNoReroutePodToNode() error {
	noOwnerFn := libovsdbops.GetNoOwnerPredicate[*nbdb.LogicalRouterPolicy]()
	p := func(item *nbdb.LogicalRouterPolicy) bool {
		return item.Priority == ovntypes.DefaultNoRereoutePriority && noOwnerFn(item) && strings.Contains(item.Match, "$") && item.Options["pkt_mark"] != ""
	}
	lrpList, err := libovsdbops.FindALogicalRouterPoliciesWithPredicate(syncer.nbClient, ovntypes.OVNClusterRouter, p)
	if err != nil {
		if errors.Is(err, libovsdbclient.ErrNotFound) {
			return nil
		}
		return err
	}
	var ops []libovsdb.Operation
	for _, lrp := range lrpList {
		isIPV6 := strings.Contains(lrp.Match, string(v6IPFamilyValue))
		ipFamily := getIPFamily(isIPV6)
		lrp.ExternalIDs = getEgressIPLRPNoReRoutePodToNodeDbIDs(ipFamily, defaultNetworkName, syncer.controllerName).GetExternalIDs()
		ops, err = libovsdbops.UpdateLogicalRouterPoliciesOps(syncer.nbClient, ops, lrp)
		if err != nil {
			return fmt.Errorf("failed to create logical router pololicy update ops: %v", err)
		}
	}
	_, err = libovsdbops.TransactAndCheck(syncer.nbClient, ops)
	if err != nil {
		return fmt.Errorf("failed to transact pod to node subnet sync ops: %v", err)
	}
	return nil
}

func getJoinSubnets() (*net.IPNet, *net.IPNet) {
	var v4JoinSubnet *net.IPNet
	var v6JoinSubnet *net.IPNet
	var err error
	if config.IPv4Mode {
		_, v4JoinSubnet, err = net.ParseCIDR(config.Gateway.V4JoinSubnet)
		if err != nil {
			klog.Errorf("Failed to parse IPv4 Join subnet")
		}
	}
	if config.IPv6Mode {
		_, v6JoinSubnet, err = net.ParseCIDR(config.Gateway.V6JoinSubnet)
		if err != nil {
			klog.Errorf("Failed to parse IPv6 Join subnet")
		}
	}
	return v4JoinSubnet, v6JoinSubnet
}

func getClusterSubnets() ([]*net.IPNet, []*net.IPNet) {
	var v4ClusterSubnets []*net.IPNet
	var v6ClusterSubnets []*net.IPNet

	for _, subnet := range config.Default.ClusterSubnets {
		if utilsnet.IsIPv6CIDR(subnet.CIDR) {
			if config.IPv6Mode {
				v6ClusterSubnets = append(v6ClusterSubnets, subnet.CIDR)
			}
		} else {
			if config.IPv4Mode {
				v4ClusterSubnets = append(v4ClusterSubnets, subnet.CIDR)
			}
		}
	}
	return v4ClusterSubnets, v6ClusterSubnets
}

func getEgressIPLRPReRouteDbIDs(egressIPName, podNamespace, podName string, ipFamily egressIPFamilyValue, network, controller string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.LogicalRouterPolicyEgressIP, controller, map[libovsdbops.ExternalIDKey]string{
		libovsdbops.ObjectNameKey: fmt.Sprintf("%s_%s/%s", egressIPName, podNamespace, podName),
		libovsdbops.PriorityKey:   fmt.Sprintf("%d", ovntypes.EgressIPReroutePriority),
		libovsdbops.IPFamilyKey:   string(ipFamily),
		libovsdbops.NetworkKey:    network,
	})
}

func getEgressIPLRPNoReRoutePodToJoinDbIDs(ipFamily egressIPFamilyValue, network, controller string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.LogicalRouterPolicyEgressIP, controller, map[libovsdbops.ExternalIDKey]string{
		libovsdbops.ObjectNameKey: string(noReRoutePodToJoin),
		libovsdbops.PriorityKey:   fmt.Sprintf("%d", ovntypes.DefaultNoRereoutePriority),
		libovsdbops.IPFamilyKey:   string(ipFamily),
		libovsdbops.NetworkKey:    network,
	})
}

func getEgressIPLRPNoReRoutePodToPodDbIDs(ipFamily egressIPFamilyValue, network, controller string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.LogicalRouterPolicyEgressIP, controller, map[libovsdbops.ExternalIDKey]string{
		libovsdbops.ObjectNameKey: string(noReRoutePodToPod),
		libovsdbops.PriorityKey:   fmt.Sprintf("%d", ovntypes.DefaultNoRereoutePriority),
		libovsdbops.IPFamilyKey:   string(ipFamily),
		libovsdbops.NetworkKey:    network,
	})
}

func getEgressIPLRPNoReRoutePodToNodeDbIDs(ipFamily egressIPFamilyValue, network, controller string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.LogicalRouterPolicyEgressIP, controller, map[libovsdbops.ExternalIDKey]string{
		libovsdbops.ObjectNameKey: string(NoReRoutePodToNode),
		libovsdbops.PriorityKey:   fmt.Sprintf("%d", ovntypes.DefaultNoRereoutePriority),
		libovsdbops.IPFamilyKey:   string(ipFamily),
		libovsdbops.NetworkKey:    network,
	})
}

func extractCIDRs(line string) []*net.IPNet {
	ipNets := make([]*net.IPNet, 0)
	strs := strings.Split(line, " ")
	for _, str := range strs {
		if !strings.Contains(str, "/") {
			continue
		}
		_, ipNet, err := net.ParseCIDR(str)
		if err != nil {
			klog.Errorf("Failed to parse CIDR from string %q: %v", str, err)
			continue
		}
		if ipNet != nil {
			ipNets = append(ipNets, ipNet)
		}
	}
	return ipNets
}

func extractIPFromEIPReRouteMatch(match string) net.IP {
	strs := strings.Split(match, " ")
	var ip net.IP
	if len(strs) != 3 {
		return ip
	}
	return net.ParseIP(strs[2])
}

func containsIPNet(ipNets []*net.IPNet, candidate *net.IPNet) bool {
	for _, ipNet := range ipNets {
		if util.IsIPNetEqual(ipNet, candidate) {
			return true
		}
	}
	return false
}

func getIPFamily(isIPv6 bool) egressIPFamilyValue {
	if isIPv6 {
		return v6IPFamilyValue
	}
	return v4IPFamilyValue
}
