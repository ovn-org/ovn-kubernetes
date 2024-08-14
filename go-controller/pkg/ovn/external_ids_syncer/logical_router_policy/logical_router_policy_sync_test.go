package logical_router_policy

import (
	"fmt"
	"net"
	"strings"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

type lrpSync struct {
	initialLRPs      []*nbdb.LogicalRouterPolicy
	finalLRPs        []*nbdb.LogicalRouterPolicy
	v4ClusterSubnets []*net.IPNet
	v6ClusterSubnets []*net.IPNet
	v4JoinSubnet     *net.IPNet
	v6JoinSubnet     *net.IPNet
	pods             podsNetInfo
}

var _ = ginkgo.Describe("OVN Logical Router Syncer", func() {
	const (
		egressIPName                 = "testeip"
		v4PodClusterSubnetStr        = "10.244.0.0/16"
		podName                      = "pod"
		podNamespace                 = "test"
		v4PodIPStr                   = "10.244.0.5"
		v6PodIPStr                   = "2001:db8:abcd:12::5"
		pod2Name                     = "pod2"
		pod2Namespace                = "test2"
		v4Pod2IPStr                  = "10.244.0.6"
		v4PodNextHop                 = "100.64.0.2"
		v6PodNextHop                 = "fe70::2"
		v6PodClusterSubnetStr        = "2001:db8:abcd:12::/64"
		v4JoinSubnetStr              = "100.64.0.0/16"
		v6JoinSubnetStr              = "fe80::/64"
		v4PodToNodeMatch             = "(ip4.src == $a4548040316634674295 || ip4.src == $a13607449821398607916) && ip4.dst == $a14918748166599097711"
		v6PodToNodeMatch             = "(ip6.src == $a4548040316634674295 || ip6.src == $a13607449821398607916) && ip6.dst == $a14918748166599097711"
		defaultNetworkControllerName = "default-network-controller"
	)

	var (
		_, v4PodClusterSubnet, _ = net.ParseCIDR(v4PodClusterSubnetStr)
		_, v6PodClusterSubnet, _ = net.ParseCIDR(v6PodClusterSubnetStr)
		_, v4JoinSubnet, _       = net.ParseCIDR(v4JoinSubnetStr)
		_, v6JoinSubnet, _       = net.ParseCIDR(v6JoinSubnetStr)
		v4PodNextHops            = []string{v4PodNextHop}
		v6PodNextHops            = []string{v6PodNextHop}
		v4PodIP                  = net.ParseIP(v4PodIPStr)
		v6PodIP                  = net.ParseIP(v6PodIPStr)
		v4Pod2IP                 = net.ParseIP(v4Pod2IPStr)
		v4PodNetInfo             = podNetInfo{v4PodIP, podNamespace, podName}
		v6PodNetInfo             = podNetInfo{v6PodIP, podNamespace, podName}
		v4Pod2NetInfo            = podNetInfo{v4Pod2IP, pod2Namespace, pod2Name}
		defaultNetwork           = util.DefaultNetInfo{}
		defaultNetworkName       = defaultNetwork.GetNetworkName()
	)

	ginkgo.Context("EgressIP", func() {
		ginkgo.DescribeTable("reroutes", func(sync lrpSync) {
			// pod reroutes may not have any owner references besides 'name' which equals EgressIP name
			performTest(defaultNetworkControllerName, sync.initialLRPs, sync.finalLRPs, sync.v4ClusterSubnets, sync.v6ClusterSubnets,
				sync.v4JoinSubnet, sync.v6JoinSubnet, sync.pods)
		},
			ginkgo.Entry("add reference to IPv4 LRP with no reference", lrpSync{
				initialLRPs: []*nbdb.LogicalRouterPolicy{getReRouteLRP(podNamespace, podName, v4PodIPStr, 0, v4IPFamilyValue,
					v4PodNextHops, map[string]string{"name": egressIPName}, defaultNetworkControllerName)},
				finalLRPs: []*nbdb.LogicalRouterPolicy{getReRouteLRP(podNamespace, podName, v4PodIPStr, 0, v4IPFamilyValue, v4PodNextHops,
					getEgressIPLRPReRouteDbIDs(egressIPName, podNamespace, podName, v4IPFamilyValue, defaultNetwork.GetNetworkName(), defaultNetworkControllerName).GetExternalIDs(),
					defaultNetworkControllerName),
				},
				v4ClusterSubnets: []*net.IPNet{v4PodClusterSubnet},
				v4JoinSubnet:     v4JoinSubnet,
				pods:             podsNetInfo{v4PodNetInfo, v6PodNetInfo, v4Pod2NetInfo},
			}),
			ginkgo.Entry("add reference to IPv6 LRP with no reference", lrpSync{
				initialLRPs: []*nbdb.LogicalRouterPolicy{getReRouteLRP(podNamespace, podName, v6PodIPStr, 0, v6IPFamilyValue,
					v6PodNextHops, map[string]string{"name": egressIPName}, defaultNetworkControllerName)},
				finalLRPs: []*nbdb.LogicalRouterPolicy{getReRouteLRP(podNamespace, podName, v6PodIPStr, 0, v6IPFamilyValue, v6PodNextHops,
					getEgressIPLRPReRouteDbIDs(egressIPName, podNamespace, podName, v6IPFamilyValue, defaultNetworkName, defaultNetworkControllerName).GetExternalIDs(),
					defaultNetworkControllerName)},
				v6ClusterSubnets: []*net.IPNet{v6PodClusterSubnet},
				v6JoinSubnet:     v6JoinSubnet,
				pods:             podsNetInfo{v4PodNetInfo, v6PodNetInfo, v4Pod2NetInfo},
			}),
			ginkgo.Entry("add references to IPv4 & IPv6 (dual) LRP with no references", lrpSync{
				initialLRPs: []*nbdb.LogicalRouterPolicy{
					getReRouteLRP(podNamespace, podName, v4PodIPStr, 0, v4IPFamilyValue, v4PodNextHops, map[string]string{"name": egressIPName}, defaultNetworkControllerName),
					getReRouteLRP(podNamespace, podName, v6PodIPStr, 0, v6IPFamilyValue, v6PodNextHops, map[string]string{"name": egressIPName}, defaultNetworkControllerName)},
				finalLRPs: []*nbdb.LogicalRouterPolicy{
					getReRouteLRP(podNamespace, podName, v4PodIPStr, 0, v4IPFamilyValue, v4PodNextHops,
						getEgressIPLRPReRouteDbIDs(egressIPName, podNamespace, podName, v4IPFamilyValue, defaultNetworkName, defaultNetworkControllerName).GetExternalIDs(), defaultNetworkControllerName),
					getReRouteLRP(podNamespace, podName, v6PodIPStr, 0, v6IPFamilyValue, v6PodNextHops,
						getEgressIPLRPReRouteDbIDs(egressIPName, podNamespace, podName, v6IPFamilyValue, defaultNetworkName, defaultNetworkControllerName).GetExternalIDs(), defaultNetworkControllerName)},
				v4ClusterSubnets: []*net.IPNet{v4PodClusterSubnet},
				v4JoinSubnet:     v4JoinSubnet,
				v6ClusterSubnets: []*net.IPNet{v6PodClusterSubnet},
				v6JoinSubnet:     v6JoinSubnet,
				pods:             podsNetInfo{v4PodNetInfo, v6PodNetInfo, v4Pod2NetInfo},
			}),
			ginkgo.Entry("does not modify IPv4 LRP which contains a reference", lrpSync{
				initialLRPs: []*nbdb.LogicalRouterPolicy{getReRouteLRP(podNamespace, podName, v4PodIPStr, 0, v4IPFamilyValue, v4PodNextHops,
					getEgressIPLRPReRouteDbIDs(egressIPName, podNamespace, podName, v4IPFamilyValue, defaultNetworkName, defaultNetworkControllerName).GetExternalIDs(),
					defaultNetworkControllerName)},
				finalLRPs: []*nbdb.LogicalRouterPolicy{getReRouteLRP(podNamespace, podName, v4PodIPStr, 0, v4IPFamilyValue, v4PodNextHops,
					getEgressIPLRPReRouteDbIDs(egressIPName, podNamespace, podName, v4IPFamilyValue, defaultNetworkName, defaultNetworkControllerName).GetExternalIDs(),
					defaultNetworkControllerName)},
				v4ClusterSubnets: []*net.IPNet{v4PodClusterSubnet},
				v4JoinSubnet:     v4JoinSubnet,
				pods:             podsNetInfo{v4PodNetInfo},
			}),
			ginkgo.Entry("does not return error when unable to build a reference because of failed pod IP lookup", lrpSync{
				initialLRPs: []*nbdb.LogicalRouterPolicy{getReRouteLRP(podNamespace, podName, v4PodIPStr, 0, v4IPFamilyValue, v4PodNextHops,
					map[string]string{"name": egressIPName},
					defaultNetworkControllerName)},
				finalLRPs: []*nbdb.LogicalRouterPolicy{getReRouteLRP(podNamespace, podName, v4PodIPStr, 0, v4IPFamilyValue, v4PodNextHops,
					map[string]string{"name": egressIPName},
					defaultNetworkControllerName)},
				v4ClusterSubnets: []*net.IPNet{v4PodClusterSubnet},
				v4JoinSubnet:     v4JoinSubnet,
				pods:             podsNetInfo{},
			}),
		)

		ginkgo.DescribeTable("pod to join, pod to pod, pod to node aka 'no reroutes'", func(sync lrpSync) {
			// pod to join LRPs may not have any owner references specified
			performTest(defaultNetworkControllerName, sync.initialLRPs, sync.finalLRPs, sync.v4ClusterSubnets, sync.v6ClusterSubnets,
				sync.v4JoinSubnet, sync.v6JoinSubnet, nil)
		},
			ginkgo.Entry("does not modify LRP with owner references", lrpSync{
				initialLRPs: []*nbdb.LogicalRouterPolicy{getNoReRouteLRP(fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4PodClusterSubnetStr, v4JoinSubnetStr),
					getEgressIPLRPNoReRoutePodToJoinDbIDs(v4IPFamilyValue, defaultNetworkName, "controller").GetExternalIDs(), nil),
				},
				finalLRPs: []*nbdb.LogicalRouterPolicy{getNoReRouteLRP(fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4PodClusterSubnetStr, v4JoinSubnetStr),
					getEgressIPLRPNoReRoutePodToJoinDbIDs(v4IPFamilyValue, defaultNetworkName, "controller").GetExternalIDs(),
					nil),
				},
				v4ClusterSubnets: []*net.IPNet{v4PodClusterSubnet},
				v4JoinSubnet:     v4JoinSubnet,
			}),
			ginkgo.Entry("updates IPv4 pod to pod, pod to join, pod to node with no references and does not modify LRP with references", lrpSync{
				initialLRPs: []*nbdb.LogicalRouterPolicy{
					{
						Priority: types.DefaultNoRereoutePriority,
						Match:    fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4PodClusterSubnetStr, v4PodClusterSubnetStr),
						Action:   nbdb.LogicalRouterPolicyActionAllow,
						UUID:     "default-no-reroute-UUID",
					},
					{
						Priority: types.DefaultNoRereoutePriority,
						Match:    fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4PodClusterSubnetStr, v4JoinSubnetStr),
						Action:   nbdb.LogicalRouterPolicyActionAllow,
						UUID:     "no-reroute-service-UUID",
					},
					{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("pkt.mark == %d", types.EgressIPReplyTrafficConnectionMark),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "egressip-no-reroute-reply-traffic-UUID",
						ExternalIDs: getEgressIPLRPNoReRouteDbIDs(types.DefaultNoRereoutePriority, string(replyTrafficNoReroute), string(ipFamilyValue), defaultNetworkName, defaultNetworkControllerName).GetExternalIDs(),
					},
					{
						Priority: types.DefaultNoRereoutePriority,
						Match:    v4PodToNodeMatch,
						Action:   nbdb.LogicalRouterPolicyActionAllow,
						UUID:     "egressip-no-reroute-pod-node-UUID",
						Options:  map[string]string{"pkt_mark": "1008"},
					},
				},
				finalLRPs: []*nbdb.LogicalRouterPolicy{
					{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4PodClusterSubnetStr, v4PodClusterSubnetStr),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-no-reroute-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(v4IPFamilyValue, defaultNetworkName, defaultNetworkControllerName).GetExternalIDs(),
					},
					{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4PodClusterSubnetStr, v4JoinSubnetStr),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-service-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(v4IPFamilyValue, defaultNetworkName, defaultNetworkControllerName).GetExternalIDs(),
					},
					{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("pkt.mark == %d", types.EgressIPReplyTrafficConnectionMark),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "egressip-no-reroute-reply-traffic-UUID",
						ExternalIDs: getEgressIPLRPNoReRouteDbIDs(types.DefaultNoRereoutePriority, string(replyTrafficNoReroute), string(ipFamilyValue), defaultNetworkName, defaultNetworkControllerName).GetExternalIDs(),
					},
					{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       v4PodToNodeMatch,
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "egressip-no-reroute-pod-node-UUID",
						Options:     map[string]string{"pkt_mark": "1008"},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(v4IPFamilyValue, defaultNetworkName, defaultNetworkControllerName).GetExternalIDs(),
					},
				},
				v4ClusterSubnets: []*net.IPNet{v4PodClusterSubnet},
				v4JoinSubnet:     v4JoinSubnet,
				v6ClusterSubnets: []*net.IPNet{v6PodClusterSubnet},
				v6JoinSubnet:     v6JoinSubnet,
			}),
			ginkgo.Entry("updates IPv6 pod to pod, pod to join with no references and does not modify LRP with references", lrpSync{
				initialLRPs: []*nbdb.LogicalRouterPolicy{
					{
						Priority: types.DefaultNoRereoutePriority,
						Match:    fmt.Sprintf("ip6.src == %s && ip6.dst == %s", v6PodClusterSubnetStr, v6PodClusterSubnetStr),
						Action:   nbdb.LogicalRouterPolicyActionAllow,
						UUID:     "default-no-reroute-UUID",
					},
					{
						Priority: types.DefaultNoRereoutePriority,
						Match:    fmt.Sprintf("ip6.src == %s && ip6.dst == %s", v6PodClusterSubnetStr, v6JoinSubnetStr),
						Action:   nbdb.LogicalRouterPolicyActionAllow,
						UUID:     "no-reroute-service-UUID",
					},
					{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("pkt.mark == %d", types.EgressIPReplyTrafficConnectionMark),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "egressip-no-reroute-reply-traffic",
						ExternalIDs: getEgressIPLRPNoReRouteDbIDs(types.DefaultNoRereoutePriority, string(replyTrafficNoReroute), string(ipFamilyValue), defaultNetworkName, defaultNetworkControllerName).GetExternalIDs(),
					},
					{
						Priority: types.DefaultNoRereoutePriority,
						Match:    v6PodToNodeMatch,
						Action:   nbdb.LogicalRouterPolicyActionAllow,
						UUID:     "egressip-no-reroute-pod-node-UUID",
						Options:  map[string]string{"pkt_mark": "1008"},
					},
				},
				finalLRPs: []*nbdb.LogicalRouterPolicy{
					{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip6.src == %s && ip6.dst == %s", v6PodClusterSubnetStr, v6PodClusterSubnetStr),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-no-reroute-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(v6IPFamilyValue, defaultNetworkName, defaultNetworkControllerName).GetExternalIDs(),
					},
					{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip6.src == %s && ip6.dst == %s", v6PodClusterSubnetStr, v6JoinSubnetStr),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-service-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(v6IPFamilyValue, defaultNetworkName, defaultNetworkControllerName).GetExternalIDs(),
					},
					{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("pkt.mark == %d", types.EgressIPReplyTrafficConnectionMark),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "egressip-no-reroute-reply-traffic",
						ExternalIDs: getEgressIPLRPNoReRouteDbIDs(types.DefaultNoRereoutePriority, string(replyTrafficNoReroute), string(ipFamilyValue), defaultNetworkName, defaultNetworkControllerName).GetExternalIDs(),
					},
					{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       v6PodToNodeMatch,
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "egressip-no-reroute-pod-node-UUID",
						Options:     map[string]string{"pkt_mark": "1008"},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(v6IPFamilyValue, defaultNetworkName, defaultNetworkControllerName).GetExternalIDs(),
					},
				},
				v6ClusterSubnets: []*net.IPNet{v6PodClusterSubnet},
				v6JoinSubnet:     v6JoinSubnet,
			}),
			ginkgo.Entry("updates IPv4 & IPv6 pod to pod, pod to join with no references and does not modify LRP with references", lrpSync{
				initialLRPs: []*nbdb.LogicalRouterPolicy{
					{
						Priority: types.DefaultNoRereoutePriority,
						Match:    fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4PodClusterSubnetStr, v4PodClusterSubnetStr),
						Action:   nbdb.LogicalRouterPolicyActionAllow,
						UUID:     "v4-default-no-reroute-UUID",
					},
					{
						Priority: types.DefaultNoRereoutePriority,
						Match:    fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4PodClusterSubnetStr, v4JoinSubnetStr),
						Action:   nbdb.LogicalRouterPolicyActionAllow,
						UUID:     "v4-no-reroute-service-UUID",
					},
					{
						Priority: types.DefaultNoRereoutePriority,
						Match:    v4PodToNodeMatch,
						Action:   nbdb.LogicalRouterPolicyActionAllow,
						UUID:     "v4-egressip-no-reroute-pod-node-UUID",
						Options:  map[string]string{"pkt_mark": "1008"},
					},
					{
						Priority: types.DefaultNoRereoutePriority,
						Match:    fmt.Sprintf("ip6.src == %s && ip6.dst == %s", v6PodClusterSubnetStr, v6PodClusterSubnetStr),
						Action:   nbdb.LogicalRouterPolicyActionAllow,
						UUID:     "v6-default-no-reroute-UUID",
					},
					{
						Priority: types.DefaultNoRereoutePriority,
						Match:    fmt.Sprintf("ip6.src == %s && ip6.dst == %s", v6PodClusterSubnetStr, v6JoinSubnetStr),
						Action:   nbdb.LogicalRouterPolicyActionAllow,
						UUID:     "v6-no-reroute-service-UUID",
					},
					{
						Priority: types.DefaultNoRereoutePriority,
						Match:    v6PodToNodeMatch,
						Action:   nbdb.LogicalRouterPolicyActionAllow,
						UUID:     "v6-egressip-no-reroute-pod-node-UUID",
						Options:  map[string]string{"pkt_mark": "1008"},
					},
					{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("pkt.mark == %d", types.EgressIPReplyTrafficConnectionMark),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "egressip-no-reroute-reply-traffic",
						ExternalIDs: getEgressIPLRPNoReRouteDbIDs(types.DefaultNoRereoutePriority, string(replyTrafficNoReroute), string(ipFamilyValue), defaultNetworkName, defaultNetworkControllerName).GetExternalIDs(),
					},
				},
				finalLRPs: []*nbdb.LogicalRouterPolicy{
					{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4PodClusterSubnetStr, v4PodClusterSubnetStr),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "v4-default-no-reroute-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(v4IPFamilyValue, defaultNetworkName, defaultNetworkControllerName).GetExternalIDs(),
					},
					{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4PodClusterSubnetStr, v4JoinSubnetStr),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "v4-no-reroute-service-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(v4IPFamilyValue, defaultNetworkName, defaultNetworkControllerName).GetExternalIDs(),
					},
					{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       v4PodToNodeMatch,
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "v4-egressip-no-reroute-pod-node-UUID",
						Options:     map[string]string{"pkt_mark": "1008"},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(v4IPFamilyValue, defaultNetworkName, defaultNetworkControllerName).GetExternalIDs(),
					},
					{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip6.src == %s && ip6.dst == %s", v6PodClusterSubnetStr, v6PodClusterSubnetStr),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "v6-default-no-reroute-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(v6IPFamilyValue, defaultNetworkName, defaultNetworkControllerName).GetExternalIDs(),
					},
					{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip6.src == %s && ip6.dst == %s", v6PodClusterSubnetStr, v6JoinSubnetStr),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "v6-no-reroute-service-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(v6IPFamilyValue, defaultNetworkName, defaultNetworkControllerName).GetExternalIDs(),
					},
					{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       v6PodToNodeMatch,
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "v6-egressip-no-reroute-pod-node-UUID",
						Options:     map[string]string{"pkt_mark": "1008"},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(v6IPFamilyValue, defaultNetworkName, defaultNetworkControllerName).GetExternalIDs(),
					},
					{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("pkt.mark == %d", types.EgressIPReplyTrafficConnectionMark),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "egressip-no-reroute-reply-traffic",
						ExternalIDs: getEgressIPLRPNoReRouteDbIDs(types.DefaultNoRereoutePriority, string(replyTrafficNoReroute), string(ipFamilyValue), defaultNetworkName, defaultNetworkControllerName).GetExternalIDs(),
					},
				},
				v4ClusterSubnets: []*net.IPNet{v4PodClusterSubnet},
				v4JoinSubnet:     v4JoinSubnet,
				v6ClusterSubnets: []*net.IPNet{v6PodClusterSubnet},
				v6JoinSubnet:     v6JoinSubnet,
			}),
		)
	})
})

func performTest(controllerName string, initialLRPs, finalLRPs []*nbdb.LogicalRouterPolicy, v4Cluster, v6Cluster []*net.IPNet, v4Join, v6Join *net.IPNet, pods podsNetInfo) {
	// build LSPs
	var lspDB []libovsdbtest.TestData
	podToIPs := map[string][]string{}
	for _, pod := range pods {
		key := composeNamespaceName(pod.namespace, pod.name)
		entry, ok := podToIPs[key]
		if !ok {
			entry = []string{}
		}
		podToIPs[key] = append(entry, pod.ip.String())

	}
	var lspUUIDs []string
	for namespaceName, ips := range podToIPs {
		namespace, name := decomposeNamespaceName(namespaceName)
		lsp := getLSP(namespace, name, ips...)
		lspUUIDs = append(lspUUIDs, lsp.UUID)
		lspDB = addLSPToTestData(lspDB, lsp)
	}
	// build switch
	ls := getLS(lspUUIDs)
	// build cluster router
	clusterRouter := getClusterRouter(getLRPUUIDs(initialLRPs)...)
	// build initiam DB
	initialDB := addLSsToTestData([]libovsdbtest.TestData{clusterRouter}, ls)
	initialDB = append(initialDB, lspDB...)
	initialDB = addLRPsToTestData(initialDB, initialLRPs...)

	// build expected DB
	expectedDB := addLSsToTestData([]libovsdbtest.TestData{clusterRouter}, ls)
	expectedDB = addLRPsToTestData(expectedDB, finalLRPs...)
	expectedDB = append(expectedDB, lspDB...)

	dbSetup := libovsdbtest.TestSetup{NBData: initialDB}
	nbClient, cleanup, err := libovsdbtest.NewNBTestHarness(dbSetup, nil)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	defer cleanup.Cleanup()

	syncer := NewLRPSyncer(nbClient, controllerName)
	config.IPv4Mode = false
	config.IPv6Mode = false
	config.Gateway.V4JoinSubnet = ""
	config.Gateway.V6JoinSubnet = ""
	config.Default.ClusterSubnets = []config.CIDRNetworkEntry{}

	if len(v4Cluster) != 0 && v4Join != nil {
		config.IPv4Mode = true
		config.Gateway.V4JoinSubnet = v4Join.String()
		for _, clusterSubnet := range v4Cluster {
			config.Default.ClusterSubnets = append(config.Default.ClusterSubnets, config.CIDRNetworkEntry{CIDR: clusterSubnet})
		}
	}
	if len(v6Cluster) != 0 && v6Join != nil {
		config.IPv6Mode = true
		config.Gateway.V6JoinSubnet = v6Join.String()
		for _, clusterSubnet := range v6Cluster {
			config.Default.ClusterSubnets = append(config.Default.ClusterSubnets, config.CIDRNetworkEntry{CIDR: clusterSubnet})
		}
	}
	gomega.Expect(syncer.Sync()).Should(gomega.Succeed())
	gomega.Eventually(nbClient).Should(libovsdbtest.HaveData(expectedDB))
}

func getNoReRouteLRP(match string, externalIDs map[string]string, options map[string]string) *nbdb.LogicalRouterPolicy {
	return &nbdb.LogicalRouterPolicy{
		UUID:        "test-lrp",
		Action:      nbdb.LogicalRouterPolicyActionAllow,
		Match:       match,
		Priority:    types.DefaultNoRereoutePriority,
		ExternalIDs: externalIDs,
		Options:     options,
	}
}

func getReRouteLRP(podNamespace, podName, podIP string, mark int, ipFamily egressIPFamilyValue, nextHops []string,
	externalIDs map[string]string, controller string) *nbdb.LogicalRouterPolicy {
	lrp := &nbdb.LogicalRouterPolicy{
		Priority:    types.EgressIPReroutePriority,
		Match:       fmt.Sprintf("%s.src == %s", ipFamily, podIP),
		Action:      nbdb.LogicalRouterPolicyActionReroute,
		Nexthops:    nextHops,
		ExternalIDs: externalIDs,
		UUID:        getReRoutePolicyUUID(podNamespace, podName, ipFamily, controller),
	}
	if mark != 0 {
		lrp.Options = getMarkOptions(mark)
	}
	return lrp
}

func getReRoutePolicyUUID(podNamespace, podName string, ipFamily egressIPFamilyValue, controller string) string {
	return fmt.Sprintf("%s-reroute-%s-%s-%s", controller, podNamespace, podName, ipFamily)
}

func getMarkOptions(mark int) map[string]string {
	return map[string]string{"pkt_mark": fmt.Sprintf("%d", mark)}
}

func getEgressIPLRPNoReRouteDbIDs(priority int, uniqueName, ipFamily, network, controller string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.LogicalRouterPolicyEgressIP, controller, map[libovsdbops.ExternalIDKey]string{
		// egress ip creates global no-reroute policies at 102 priority
		libovsdbops.ObjectNameKey: uniqueName,
		libovsdbops.PriorityKey:   fmt.Sprintf("%d", priority),
		libovsdbops.IPFamilyKey:   ipFamily,
		libovsdbops.NetworkKey:    network,
	})
}

func getClusterRouter(policies ...string) *nbdb.LogicalRouter {
	if policies == nil {
		policies = make([]string, 0)
	}
	return &nbdb.LogicalRouter{
		UUID:     types.OVNClusterRouter + "-UUID",
		Name:     types.OVNClusterRouter,
		Policies: policies,
	}
}

func getLS(ports []string) *nbdb.LogicalSwitch {
	return &nbdb.LogicalSwitch{
		UUID:  "switch-UUID",
		Name:  "switch",
		Ports: ports,
	}
}

func getLSP(podNamespace, podName string, ips ...string) *nbdb.LogicalSwitchPort {
	return &nbdb.LogicalSwitchPort{
		UUID:        fmt.Sprintf("%s-%s-UUID", podNamespace, podName),
		Addresses:   append([]string{"0a:58:0a:f4:00:04"}, ips...),
		ExternalIDs: map[string]string{"namespace": podNamespace, "pod": "true"},
		Name:        util.GetLogicalPortName(podNamespace, podName),
	}
}

func getLRPUUIDs(lrps []*nbdb.LogicalRouterPolicy) []string {
	lrpUUIDs := make([]string, 0)
	for _, lrp := range lrps {
		lrpUUIDs = append(lrpUUIDs, lrp.UUID)
	}
	return lrpUUIDs
}

func addLRPsToTestData(data []libovsdbtest.TestData, lrps ...*nbdb.LogicalRouterPolicy) []libovsdbtest.TestData {
	for _, lrp := range lrps {
		data = append(data, lrp)
	}
	return data
}

func addLSsToTestData(data []libovsdbtest.TestData, lss ...*nbdb.LogicalSwitch) []libovsdbtest.TestData {
	for _, ls := range lss {
		data = append(data, ls)
	}
	return data
}

func addLSPToTestData(data []libovsdbtest.TestData, lsps ...*nbdb.LogicalSwitchPort) []libovsdbtest.TestData {
	for _, lsp := range lsps {
		data = append(data, lsp)
	}
	return data
}

func composeNamespaceName(namespace, name string) string {
	return fmt.Sprintf("%s/%s", namespace, name)
}

func decomposeNamespaceName(str string) (string, string) {
	split := strings.Split(str, "/")
	return split[0], split[1]
}
