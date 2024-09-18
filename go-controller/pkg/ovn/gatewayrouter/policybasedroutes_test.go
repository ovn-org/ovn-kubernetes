package gatewayrouter

import (
	"fmt"
	"net"
	"strconv"
	"testing"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	types2 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilnet "k8s.io/utils/net"
)

// policy represents the policy to be added
type policy struct {
	nodeName          string
	hostInfCIDR       *net.IPNet // primary host interface CIDR
	otherHostInfAddrs []string   // other addresses attached to primary host interface
	targetNetwork     string     // network name
}

// network represents the state of an L3 network. It may include LRP state before a test is performed.
type network struct {
	initialLRPs []*nbdb.LogicalRouterPolicy // LRPs attach to networks distributed cluster router before policy is added
	info        util.NetInfo
	mgntIPv4    string // management port IPv4 for the network
	mgntIPv6    string // management port IPv6 for the network
}

func (n network) copyNetworkAndSetLRPs(lrps ...*nbdb.LogicalRouterPolicy) network {
	nCopy := n
	nCopy.initialLRPs = lrps
	return nCopy
}

func (n network) generateTestData(nodeName string) []libovsdbtest.TestData {
	data := make([]libovsdbtest.TestData, 0, 0)
	lrpUUIDs := make([]string, 0, len(n.initialLRPs))
	for _, lrp := range n.initialLRPs {
		lrpUUIDs = append(lrpUUIDs, lrp.UUID)
		var extID map[string]string
		if n.info.IsSecondary() {
			extID = map[string]string{
				types.NetworkExternalID:  n.info.GetNetworkName(),
				types.TopologyExternalID: n.info.TopologyType(),
			}
			lrp.ExternalIDs = extID
		}
		data = append(data, lrp)
	}
	lr := &nbdb.LogicalRouter{
		UUID:     getLRUUID(n.info.GetNetworkScopedClusterRouterName()),
		Name:     n.info.GetNetworkScopedClusterRouterName(),
		Policies: lrpUUIDs,
	}
	if n.info.TopologyType() == types.Layer2Topology {
		lr.Name = n.info.GetNetworkScopedGWRouterName(nodeName)
		lr.UUID = getLRUUID(n.info.GetNetworkScopedGWRouterName(nodeName))
	}
	return append(data, lr)
}

type networks []network

func (ns networks) generateTestData(nodeName string) []libovsdbtest.TestData {
	data := make([]libovsdbtest.TestData, 0)
	for _, n := range ns {
		data = append(data, n.generateTestData(nodeName)...)
	}
	return data
}

func (ns networks) getNetwork(name string) network {
	for _, n := range ns {
		if n.info.GetNetworkName() == name {
			return n
		}
	}
	panic(fmt.Sprintf("network %s is not defined", name))
}

func getLRUUID(networkName string) string {
	return fmt.Sprintf("%s-UUID", networkName)
}

type test struct {
	desc        string
	addPolicies []policy
	initialDB   networks
	expectedDB  []libovsdbtest.TestData
	expectErr   bool
}

func TestAddSameNodeIPPolicy(t *testing.T) {
	const (
		node1Name                 = "node1"
		node1HostIPv4Str          = "192.168.1.10"
		node1HostCIDRIPv4Str      = node1HostIPv4Str + "/32"
		node1HostOtherAddrIPv4Str = "172.18.0.5"
		node1HostIPv6Str          = "fc00:f853:ccd:e793::3"
		node1HostCIDR128IPv6Str   = node1HostIPv6Str + "/128"
		node1HostCIDR64IPv6Str    = node1HostIPv6Str + "/64"
		joinSubnetIPv4Str         = "100.10.1.0/24"
		clusterSubnetIPv4Str      = "10.128.0.0/16"
		node1CDNMgntIPv4Str       = "10.244.1.2"
		node1CDNMgntIPv6Str       = "fd00:10:244::2"
		node1UDNMgntIPv4Str       = "10.200.1.2"
		node1UDNMgntIPv6Str       = "fd00:20:244::2"
		v4Prefix                  = "ip4"
		v6Prefix                  = "ip6"
		udnNetworkName            = "network1"
	)

	var (
		nodeSubNetPrio, _          = strconv.Atoi(types.NodeSubnetPolicyPriority)
		_, node1HostCIDRIPv4, _    = net.ParseCIDR(node1HostCIDRIPv4Str)
		_, node1HostCIDR128IPv6, _ = net.ParseCIDR(node1HostCIDR128IPv6Str)
		otherHostAddrsIPv4         = []string{node1HostOtherAddrIPv4Str}
		invalidOtherHostAddrsIPv4  = []string{"<nil>"}
		cdnL3Network               = network{
			initialLRPs: nil,
			info:        &util.DefaultNetInfo{},
			mgntIPv4:    node1CDNMgntIPv4Str,
			mgntIPv6:    node1CDNMgntIPv6Str,
		}
		l3NetInfo, _ = util.NewNetInfo(&types2.NetConf{
			NetConf:    cnitypes.NetConf{Name: udnNetworkName},
			Topology:   types.Layer3Topology,
			JoinSubnet: joinSubnetIPv4Str,    // not required, but adding so NewNetInfo doesn't fail
			Subnets:    clusterSubnetIPv4Str, // not required, but adding so NewNetInfo doesn't fail
		})
		udnL3Network = network{
			initialLRPs: nil,
			info:        l3NetInfo,
			mgntIPv4:    node1UDNMgntIPv4Str,
			mgntIPv6:    node1UDNMgntIPv6Str,
		}
	)

	tests := []test{
		{
			desc: "[cdn][ipv4] no additional addresses",
			addPolicies: []policy{
				{

					nodeName:          node1Name,
					hostInfCIDR:       node1HostCIDRIPv4,
					otherHostInfAddrs: nil,
					targetNetwork:     cdnL3Network.info.GetNetworkName(),
				},
			},
			initialDB: networks{cdnL3Network},
			expectedDB: []libovsdbtest.TestData{
				&nbdb.LogicalRouter{
					UUID:     "cdn-cr-uuid",
					Name:     cdnL3Network.info.GetNetworkScopedClusterRouterName(),
					Policies: []string{"node-ip-lrp-uuid"},
				},
				&nbdb.LogicalRouterPolicy{
					UUID:     "node-ip-lrp-uuid",
					Priority: nodeSubNetPrio,
					Match:    generateNodeIPMatch(cdnL3Network.info.GetNetworkScopedSwitchName(node1Name), v4Prefix, node1HostIPv4Str),
					Action:   nbdb.LogicalRouterPolicyActionReroute,
					Nexthops: []string{node1CDNMgntIPv4Str},
				},
			},
		},
		{
			desc: "[cdn][ipv4] additional addresses",
			addPolicies: []policy{
				{

					nodeName:          node1Name,
					hostInfCIDR:       node1HostCIDRIPv4,
					otherHostInfAddrs: otherHostAddrsIPv4,
					targetNetwork:     cdnL3Network.info.GetNetworkName(),
				},
			},
			initialDB: networks{cdnL3Network},
			expectedDB: []libovsdbtest.TestData{
				&nbdb.LogicalRouter{
					UUID:     "cdn-cr-uuid",
					Name:     cdnL3Network.info.GetNetworkScopedClusterRouterName(),
					Policies: []string{"node-ip-lrp-uuid", "node-ip2-lrp-uuid"},
				},
				&nbdb.LogicalRouterPolicy{
					UUID:     "node-ip-lrp-uuid",
					Priority: nodeSubNetPrio,
					Match:    generateNodeIPMatch(cdnL3Network.info.GetNetworkScopedSwitchName(node1Name), v4Prefix, node1HostIPv4Str),
					Action:   nbdb.LogicalRouterPolicyActionReroute,
					Nexthops: []string{node1CDNMgntIPv4Str},
				},
				&nbdb.LogicalRouterPolicy{
					UUID:     "node-ip2-lrp-uuid",
					Priority: nodeSubNetPrio,
					Match:    generateNodeIPMatch(cdnL3Network.info.GetNetworkScopedSwitchName(node1Name), v4Prefix, node1HostOtherAddrIPv4Str),
					Action:   nbdb.LogicalRouterPolicyActionReroute,
					Nexthops: []string{node1CDNMgntIPv4Str},
				},
			},
		},
		{
			desc: "[cdn][ipv4] invalid additional addresses causes error",
			addPolicies: []policy{
				{

					nodeName:          node1Name,
					hostInfCIDR:       node1HostCIDRIPv4,
					otherHostInfAddrs: invalidOtherHostAddrsIPv4,
					targetNetwork:     cdnL3Network.info.GetNetworkName(),
				},
			},
			initialDB: networks{cdnL3Network},
			expectErr: true,
		},
		{
			desc: "[cdn][ipv6] no additional addresses",
			addPolicies: []policy{
				{

					nodeName:          node1Name,
					hostInfCIDR:       node1HostCIDR128IPv6,
					otherHostInfAddrs: nil,
					targetNetwork:     cdnL3Network.info.GetNetworkName(),
				},
			},
			initialDB: networks{cdnL3Network},
			expectedDB: []libovsdbtest.TestData{
				&nbdb.LogicalRouter{
					UUID:     "cdn-cr-uuid",
					Name:     cdnL3Network.info.GetNetworkScopedClusterRouterName(),
					Policies: []string{"node-ip-lrp-uuid"},
				},
				&nbdb.LogicalRouterPolicy{
					UUID:     "node-ip-lrp-uuid",
					Priority: nodeSubNetPrio,
					Match:    generateNodeIPMatch(cdnL3Network.info.GetNetworkScopedSwitchName(node1Name), v6Prefix, node1HostIPv6Str),
					Action:   nbdb.LogicalRouterPolicyActionReroute,
					Nexthops: []string{node1CDNMgntIPv6Str},
				},
			},
		},
		{
			desc: "[cdn][ipv4][ipv6 no additional addresses",
			addPolicies: []policy{
				{
					nodeName:          node1Name,
					hostInfCIDR:       node1HostCIDRIPv4,
					otherHostInfAddrs: nil,
					targetNetwork:     cdnL3Network.info.GetNetworkName(),
				},
				{

					nodeName:          node1Name,
					hostInfCIDR:       node1HostCIDR128IPv6,
					otherHostInfAddrs: nil,
					targetNetwork:     cdnL3Network.info.GetNetworkName(),
				},
			},
			initialDB: networks{cdnL3Network},
			expectedDB: []libovsdbtest.TestData{
				&nbdb.LogicalRouter{
					UUID:     "cdn-cr-uuid",
					Name:     cdnL3Network.info.GetNetworkScopedClusterRouterName(),
					Policies: []string{"node-ip-lrp-v4-uuid", "node-ip-lrp-v6-uuid"},
				},
				&nbdb.LogicalRouterPolicy{
					UUID:     "node-ip-lrp-v4-uuid",
					Priority: nodeSubNetPrio,
					Match:    generateNodeIPMatch(cdnL3Network.info.GetNetworkScopedSwitchName(node1Name), v4Prefix, node1HostIPv4Str),
					Action:   nbdb.LogicalRouterPolicyActionReroute,
					Nexthops: []string{node1CDNMgntIPv4Str},
				},
				&nbdb.LogicalRouterPolicy{
					UUID:     "node-ip-lrp-v6-uuid",
					Priority: nodeSubNetPrio,
					Match:    generateNodeIPMatch(cdnL3Network.info.GetNetworkScopedSwitchName(node1Name), v6Prefix, node1HostIPv6Str),
					Action:   nbdb.LogicalRouterPolicyActionReroute,
					Nexthops: []string{node1CDNMgntIPv6Str},
				},
			},
		},
		{
			desc: "[cdn][udn][ipv4][ipv6] no additional addresses",
			addPolicies: []policy{
				{
					nodeName:          node1Name,
					hostInfCIDR:       node1HostCIDRIPv4,
					otherHostInfAddrs: nil,
					targetNetwork:     cdnL3Network.info.GetNetworkName(),
				},
				{

					nodeName:          node1Name,
					hostInfCIDR:       node1HostCIDR128IPv6,
					otherHostInfAddrs: nil,
					targetNetwork:     udnL3Network.info.GetNetworkName(),
				},
			},
			initialDB: networks{cdnL3Network, udnL3Network},
			expectedDB: []libovsdbtest.TestData{
				&nbdb.LogicalRouter{
					UUID:     "cdn-cr-uuid",
					Name:     cdnL3Network.info.GetNetworkScopedClusterRouterName(),
					Policies: []string{"node-ip-lrp-v4-uuid"},
				},
				&nbdb.LogicalRouter{
					UUID:     "udn-cr-uuid",
					Name:     udnL3Network.info.GetNetworkScopedClusterRouterName(),
					Policies: []string{"node-ip-lrp-v6-uuid"},
				},
				&nbdb.LogicalRouterPolicy{
					UUID:     "node-ip-lrp-v4-uuid",
					Priority: nodeSubNetPrio,
					Match:    generateNodeIPMatch(cdnL3Network.info.GetNetworkScopedSwitchName(node1Name), v4Prefix, node1HostIPv4Str),
					Action:   nbdb.LogicalRouterPolicyActionReroute,
					Nexthops: []string{node1CDNMgntIPv4Str},
				},
				&nbdb.LogicalRouterPolicy{
					UUID:     "node-ip-lrp-v6-uuid",
					Priority: nodeSubNetPrio,
					Match:    generateNodeIPMatch(udnL3Network.info.GetNetworkScopedSwitchName(node1Name), v6Prefix, node1HostIPv6Str),
					Action:   nbdb.LogicalRouterPolicyActionReroute,
					Nexthops: []string{node1UDNMgntIPv6Str},
					ExternalIDs: map[string]string{
						types.NetworkExternalID:  udnL3Network.info.GetNetworkName(),
						types.TopologyExternalID: udnL3Network.info.TopologyType(),
					},
				},
			},
		},
		{
			desc: "[cdn][ipv4] doesn't alter existing entry",
			addPolicies: []policy{
				{

					nodeName:          node1Name,
					hostInfCIDR:       node1HostCIDRIPv4,
					otherHostInfAddrs: nil,
					targetNetwork:     cdnL3Network.info.GetNetworkName(),
				},
			},
			initialDB: networks{cdnL3Network.copyNetworkAndSetLRPs(
				&nbdb.LogicalRouterPolicy{
					UUID:     "node-ip-lrp-uuid",
					Priority: nodeSubNetPrio,
					Match:    generateNodeIPMatch(cdnL3Network.info.GetNetworkScopedSwitchName(node1Name), v4Prefix, node1HostIPv4Str),
					Action:   nbdb.LogicalRouterPolicyActionReroute,
					Nexthops: []string{node1CDNMgntIPv4Str},
				})},
			expectedDB: []libovsdbtest.TestData{
				&nbdb.LogicalRouter{
					UUID:     "cdn-cr-uuid",
					Name:     cdnL3Network.info.GetNetworkScopedClusterRouterName(),
					Policies: []string{"node-ip-lrp-uuid"},
				},
				&nbdb.LogicalRouterPolicy{
					UUID:     "node-ip-lrp-uuid",
					Priority: nodeSubNetPrio,
					Match:    generateNodeIPMatch(cdnL3Network.info.GetNetworkScopedSwitchName(node1Name), v4Prefix, node1HostIPv4Str),
					Action:   nbdb.LogicalRouterPolicyActionReroute,
					Nexthops: []string{node1CDNMgntIPv4Str},
				},
			},
		},
		{
			desc: "[cdn][ipv4] doesn't alter existing entry",
			addPolicies: []policy{
				{

					nodeName:          node1Name,
					hostInfCIDR:       node1HostCIDRIPv4,
					otherHostInfAddrs: nil,
					targetNetwork:     cdnL3Network.info.GetNetworkName(),
				},
			},
			initialDB: networks{cdnL3Network.copyNetworkAndSetLRPs(
				&nbdb.LogicalRouterPolicy{
					UUID:     "node-ip-lrp-uuid",
					Priority: nodeSubNetPrio,
					Match:    generateNodeIPMatch(cdnL3Network.info.GetNetworkScopedSwitchName(node1Name), v4Prefix, node1HostIPv4Str),
					Action:   nbdb.LogicalRouterPolicyActionReroute,
					Nexthops: []string{node1CDNMgntIPv4Str},
				})},
			expectedDB: []libovsdbtest.TestData{
				&nbdb.LogicalRouter{
					UUID:     "cdn-cr-uuid",
					Name:     cdnL3Network.info.GetNetworkScopedClusterRouterName(),
					Policies: []string{"node-ip-lrp-uuid"},
				},
				&nbdb.LogicalRouterPolicy{
					UUID:     "node-ip-lrp-uuid",
					Priority: nodeSubNetPrio,
					Match:    generateNodeIPMatch(cdnL3Network.info.GetNetworkScopedSwitchName(node1Name), v4Prefix, node1HostIPv4Str),
					Action:   nbdb.LogicalRouterPolicyActionReroute,
					Nexthops: []string{node1CDNMgntIPv4Str},
				},
			},
		},
		{
			desc: "[cdn][udn][ipv4] doesn't alter existing and appends new entry",
			addPolicies: []policy{
				{

					nodeName:          node1Name,
					hostInfCIDR:       node1HostCIDRIPv4,
					otherHostInfAddrs: nil,
					targetNetwork:     cdnL3Network.info.GetNetworkName(),
				},
				{

					nodeName:          node1Name,
					hostInfCIDR:       node1HostCIDRIPv4,
					otherHostInfAddrs: nil,
					targetNetwork:     udnL3Network.info.GetNetworkName(),
				},
			},
			initialDB: networks{cdnL3Network.copyNetworkAndSetLRPs(
				&nbdb.LogicalRouterPolicy{
					UUID:     "node-ip-lrp-uuid",
					Priority: nodeSubNetPrio,
					Match:    generateNodeIPMatch(cdnL3Network.info.GetNetworkScopedSwitchName(node1Name), v4Prefix, node1HostIPv4Str),
					Action:   nbdb.LogicalRouterPolicyActionReroute,
					Nexthops: []string{node1CDNMgntIPv4Str},
				}),
				udnL3Network},
			expectedDB: []libovsdbtest.TestData{
				&nbdb.LogicalRouter{
					UUID:     "cdn-cr-uuid",
					Name:     cdnL3Network.info.GetNetworkScopedClusterRouterName(),
					Policies: []string{"node-ip-lrp-uuid"},
				},
				&nbdb.LogicalRouter{
					UUID:     "udn-cr-uuid",
					Name:     udnL3Network.info.GetNetworkScopedClusterRouterName(),
					Policies: []string{"node-ip-lrp2-uuid"},
				},
				&nbdb.LogicalRouterPolicy{
					UUID:     "node-ip-lrp-uuid",
					Priority: nodeSubNetPrio,
					Match:    generateNodeIPMatch(cdnL3Network.info.GetNetworkScopedSwitchName(node1Name), v4Prefix, node1HostIPv4Str),
					Action:   nbdb.LogicalRouterPolicyActionReroute,
					Nexthops: []string{node1CDNMgntIPv4Str},
				},
				&nbdb.LogicalRouterPolicy{
					UUID:     "node-ip-lrp2-uuid",
					Priority: nodeSubNetPrio,
					Match:    generateNodeIPMatch(udnL3Network.info.GetNetworkScopedSwitchName(node1Name), v4Prefix, node1HostIPv4Str),
					Action:   nbdb.LogicalRouterPolicyActionReroute,
					Nexthops: []string{node1UDNMgntIPv4Str},
					ExternalIDs: map[string]string{
						types.NetworkExternalID:  udnL3Network.info.GetNetworkName(),
						types.TopologyExternalID: udnL3Network.info.TopologyType(),
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			dbSetup := libovsdbtest.TestSetup{
				NBData: tt.initialDB.generateTestData(node1Name),
			}
			nbdbClient, cleanup, err := libovsdbtest.NewNBTestHarness(dbSetup, nil)
			if err != nil {
				t.Errorf("libovsdb client error: %v", err)
				return
			}
			t.Cleanup(cleanup.Cleanup)
			netToMgr := map[string]*PolicyBasedRoutesManager{}
			for _, net := range tt.initialDB {
				netToMgr[net.info.GetNetworkName()] = NewPolicyBasedRoutesManager(nbdbClient, net.info.GetNetworkScopedClusterRouterName(), net.info)
			}
			// verify all polices have a valid network name
			for _, p := range tt.addPolicies {
				mgr, ok := netToMgr[p.targetNetwork]
				if !ok {
					t.Errorf("policy defined a network %q but no associated network defined with this name", p.targetNetwork)
					return
				}
				targetNet := tt.initialDB.getNetwork(p.targetNetwork)
				mgntIP := targetNet.mgntIPv4
				if utilnet.IsIPv6(p.hostInfCIDR.IP) {
					mgntIP = targetNet.mgntIPv6
				}
				err = mgr.AddSameNodeIPPolicy(p.nodeName, mgntIP, p.hostInfCIDR, p.otherHostInfAddrs)
				if tt.expectErr && err == nil {
					t.Fatalf("test: \"%s\", expected error but none occured", tt.desc)
				}
				if tt.expectErr && err != nil {
					return
				}
			}
			matcher := libovsdbtest.HaveData(tt.expectedDB)
			success, err := matcher.Match(nbdbClient)
			if !success {
				t.Fatal(fmt.Errorf("test: \"%s\" didn't match expected with actual, err: %v", tt.desc, matcher.FailureMessage(nbdbClient)))
			}
			if err != nil {
				t.Fatal(fmt.Errorf("test: \"%s\" encountered error: %v", tt.desc, err))
			}
		})
	}
}

func TestAddHostCIDRPolicy(t *testing.T) {
	const (
		node1Name              = "node1"
		hostCIDRV4RangeStr     = "192.168.1.0/24"
		hostCIDRV6RangeStr     = "fc00:f853:ccd:e793::/64"
		node1HostIPv4Str       = "192.168.1.10"
		node1HostCIDR24IPv4Str = node1HostIPv4Str + "/24"
		node1HostIPv6Str       = "fc00:f853:ccd:e793::3"
		node1HostCIDR64IPv6Str = node1HostIPv6Str + "/64"
		joinSubnetIPv4Str      = "100.10.1.0/24"
		clusterSubnetIPv4Str   = "10.128.0.0/16"
		clusterSubnetIPv6Str   = "2002:0:0:1234::/64"
		node1UDNMgntIPv4Str    = "10.200.1.2"
		node1UDNMgntIPv6Str    = "fd00:20:244::2"
		v4Prefix               = "ip4"
		v6Prefix               = "ip6"
		udnNetworkName         = "network1"
	)

	var (
		hostCIDRPolicyPrio, _ = strconv.Atoi(types.UDNHostCIDRPolicyPriority)
		_, hostCIDRV4Range, _ = net.ParseCIDR(hostCIDRV4RangeStr)
		_, hostCIDRV6Range, _ = net.ParseCIDR(hostCIDRV6RangeStr)
		l2NetInfo, _          = util.NewNetInfo(&types2.NetConf{
			NetConf:    cnitypes.NetConf{Name: udnNetworkName},
			Topology:   types.Layer2Topology,
			JoinSubnet: joinSubnetIPv4Str,                                 // not required, but adding so NewNetInfo doesn't fail
			Subnets:    clusterSubnetIPv4Str + "," + clusterSubnetIPv6Str, // not required, but adding so NewNetInfo doesn't fail
		})
		udnL2Network = network{
			initialLRPs: nil,
			info:        l2NetInfo,
			mgntIPv4:    node1UDNMgntIPv4Str,
			mgntIPv6:    node1UDNMgntIPv6Str,
		}
		node = &v1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: node1Name,
				Annotations: map[string]string{
					"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}",
						node1HostCIDR24IPv4Str, node1HostCIDR64IPv6Str),
				},
			},
		}
	)

	tests := []test{
		{
			desc: "[udn][l2][ipv4][ipv6] add hostCIDR policy for L2",
			addPolicies: []policy{
				{
					targetNetwork: udnL2Network.info.GetNetworkName(),
					hostInfCIDR:   hostCIDRV4Range,
				},
				{
					targetNetwork: udnL2Network.info.GetNetworkName(),
					hostInfCIDR:   hostCIDRV6Range,
				},
			},
			initialDB: networks{udnL2Network},
			expectedDB: []libovsdbtest.TestData{
				&nbdb.LogicalRouter{
					UUID:     "udn-gr-uuid",
					Name:     udnL2Network.info.GetNetworkScopedGWRouterName(node1Name),
					Policies: []string{"node-ip-lrp-v4-uuid", "node-ip-lrp-v6-uuid"},
				},
				&nbdb.LogicalRouterPolicy{
					UUID:     "node-ip-lrp-v4-uuid",
					Priority: hostCIDRPolicyPrio,
					Match:    generateHostCIDRMatch(v4Prefix, hostCIDRV4RangeStr, clusterSubnetIPv4Str),
					Action:   nbdb.LogicalRouterPolicyActionReroute,
					Nexthops: []string{node1UDNMgntIPv4Str},
					ExternalIDs: map[string]string{
						types.NetworkExternalID:  udnL2Network.info.GetNetworkName(),
						types.TopologyExternalID: udnL2Network.info.TopologyType(),
					},
				},
				&nbdb.LogicalRouterPolicy{
					UUID:     "node-ip-lrp-v6-uuid",
					Priority: hostCIDRPolicyPrio,
					Match:    generateHostCIDRMatch(v6Prefix, hostCIDRV6RangeStr, clusterSubnetIPv6Str),
					Action:   nbdb.LogicalRouterPolicyActionReroute,
					Nexthops: []string{node1UDNMgntIPv6Str},
					ExternalIDs: map[string]string{
						types.NetworkExternalID:  udnL2Network.info.GetNetworkName(),
						types.TopologyExternalID: udnL2Network.info.TopologyType(),
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			dbSetup := libovsdbtest.TestSetup{
				NBData: tt.initialDB.generateTestData(node1Name),
			}
			nbdbClient, cleanup, err := libovsdbtest.NewNBTestHarness(dbSetup, nil)
			if err != nil {
				t.Errorf("libovsdb client error: %v", err)
				return
			}
			t.Cleanup(cleanup.Cleanup)
			netToMgr := map[string]*PolicyBasedRoutesManager{}
			for _, net := range tt.initialDB {
				netToMgr[net.info.GetNetworkName()] = NewPolicyBasedRoutesManager(nbdbClient, net.info.GetNetworkScopedGWRouterName(node1Name), net.info)
			}
			// verify all polices have a valid network name
			for _, p := range tt.addPolicies {
				mgr, ok := netToMgr[p.targetNetwork]
				if !ok {
					t.Errorf("policy defined a network %q but no associated network defined with this name", p.targetNetwork)
					return
				}
				targetNet := tt.initialDB.getNetwork(p.targetNetwork)
				mgntIP := targetNet.mgntIPv4
				clustersubnet := clusterSubnetIPv4Str
				if utilnet.IsIPv6(p.hostInfCIDR.IP) {
					mgntIP = targetNet.mgntIPv6
					clustersubnet = clusterSubnetIPv6Str
				}
				err = mgr.AddHostCIDRPolicy(node, mgntIP, clustersubnet)
				if err != nil {
					t.Fatal(fmt.Errorf("test: \"%s\" encountered error: %v", tt.desc, err))
				}
			}
			matcher := libovsdbtest.HaveData(tt.expectedDB)
			success, err := matcher.Match(nbdbClient)
			if !success {
				t.Fatal(fmt.Errorf("test: \"%s\" didn't match expected with actual, err: %v", tt.desc, matcher.FailureMessage(nbdbClient)))
			}
			if err != nil {
				t.Fatal(fmt.Errorf("test: \"%s\" encountered error: %v", tt.desc, err))
			}
		})
	}
}
