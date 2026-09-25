// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package bridgeconfig

import (
	"fmt"
	"net"
	"strings"
	"testing"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	udngenerator "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/generator/udn"
	nodetypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/types"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
)

func newBridgeFlowTestOVSClient(t *testing.T, bridgeName string) libovsdbclient.Client {
	t.Helper()
	bridgeUUID := bridgeName + "-uuid"
	ovsClient, ovsCleanup, err := libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{
		OVSData: []libovsdbtest.TestData{
			&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{bridgeUUID}},
			&vswitchd.Bridge{UUID: bridgeUUID, Name: bridgeName},
		},
	})
	if err != nil {
		t.Fatalf("failed to create OVS test harness: %v", err)
	}
	t.Cleanup(ovsCleanup.Cleanup)
	return ovsClient
}

func TestHostNetworkNormalActionFlowsRequireLocalnetPort(t *testing.T) {
	if err := config.PrepareTestConfig(); err != nil {
		t.Fatalf("failed to prepare test config: %v", err)
	}
	t.Cleanup(func() {
		_ = config.PrepareTestConfig()
	})
	tests := []struct {
		name                   string
		gatewayMode            config.GatewayMode
		ipv4Mode               bool
		ipv6Mode               bool
		localnetBridge         string
		expectedPriority102Cnt int
	}{
		{
			name:                   "shared gateway IPv4 without localnet topology port",
			gatewayMode:            config.GatewayModeShared,
			ipv4Mode:               true,
			expectedPriority102Cnt: 0,
		},
		{
			name:                   "shared gateway IPv4 with localnet topology port",
			gatewayMode:            config.GatewayModeShared,
			ipv4Mode:               true,
			localnetBridge:         "br0",
			expectedPriority102Cnt: 2,
		},
		{
			name:                   "shared gateway IPv6 without localnet topology port",
			gatewayMode:            config.GatewayModeShared,
			ipv6Mode:               true,
			expectedPriority102Cnt: 0,
		},
		{
			name:                   "shared gateway IPv6 with localnet topology port",
			gatewayMode:            config.GatewayModeShared,
			ipv6Mode:               true,
			localnetBridge:         "br0",
			expectedPriority102Cnt: 6,
		},
		{
			name:                   "local gateway IPv4 without localnet topology port",
			gatewayMode:            config.GatewayModeLocal,
			ipv4Mode:               true,
			expectedPriority102Cnt: 0,
		},
		{
			name:                   "local gateway IPv4 with localnet topology port",
			gatewayMode:            config.GatewayModeLocal,
			ipv4Mode:               true,
			localnetBridge:         "br0",
			expectedPriority102Cnt: 1,
		},
		{
			name:                   "local gateway IPv6 without localnet topology port",
			gatewayMode:            config.GatewayModeLocal,
			ipv6Mode:               true,
			expectedPriority102Cnt: 0,
		},
		{
			name:                   "local gateway IPv6 with localnet topology port",
			gatewayMode:            config.GatewayModeLocal,
			ipv6Mode:               true,
			localnetBridge:         "br0",
			expectedPriority102Cnt: 3,
		},
		{
			name:                   "localnet topology port on another bridge does not enable flows",
			gatewayMode:            config.GatewayModeShared,
			ipv4Mode:               true,
			localnetBridge:         "br-other",
			expectedPriority102Cnt: 0,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config.Gateway.Mode = test.gatewayMode
			config.IPv4Mode = test.ipv4Mode
			config.IPv6Mode = test.ipv6Mode

			br0Ports := []string{"gateway-port-uuid", "cudn-gateway-port-uuid"}
			rootBridges := []string{"br0-uuid"}
			ovsData := []libovsdbtest.TestData{
				&vswitchd.Port{
					UUID:        "gateway-port-uuid",
					Name:        "patch-br0_node-to-br-int",
					Interfaces:  []string{"gateway-interface-uuid"},
					ExternalIDs: map[string]string{"ovn-localnet-port": "br0_node"},
				},
				&vswitchd.Interface{UUID: "gateway-interface-uuid", Name: "patch-br0_node-to-br-int", Type: "patch"},
				&vswitchd.Port{
					UUID:        "cudn-gateway-port-uuid",
					Name:        "patch-br0_blue_node-to-br-int",
					Interfaces:  []string{"cudn-gateway-interface-uuid"},
					ExternalIDs: map[string]string{"ovn-localnet-port": "br0_blue_node"},
				},
				&vswitchd.Interface{UUID: "cudn-gateway-interface-uuid", Name: "patch-br0_blue_node-to-br-int", Type: "patch"},
			}

			if test.localnetBridge != "" {
				localnetPort := &vswitchd.Port{
					UUID:        "localnet-port-uuid",
					Name:        "patch-blue_ovn_localnet_port-to-br-int",
					Interfaces:  []string{"localnet-interface-uuid"},
					ExternalIDs: map[string]string{"ovn-localnet-port": "blue_ovn_localnet_port"},
				}
				ovsData = append(ovsData, localnetPort,
					&vswitchd.Interface{UUID: "localnet-interface-uuid", Name: localnetPort.Name, Type: "patch"})
				if test.localnetBridge == "br0" {
					br0Ports = append(br0Ports, localnetPort.UUID)
				} else {
					rootBridges = append(rootBridges, "br-other-uuid")
					ovsData = append(ovsData, &vswitchd.Bridge{
						UUID:  "br-other-uuid",
						Name:  "br-other",
						Ports: []string{localnetPort.UUID},
					})
				}
			}

			ovsData = append(ovsData,
				&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: rootBridges},
				&vswitchd.Bridge{UUID: "br0-uuid", Name: "br0", Ports: br0Ports},
			)
			ovsClient, ovsCleanup, err := libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{OVSData: ovsData})
			if err != nil {
				t.Fatalf("failed to create OVS test harness: %v", err)
			}
			t.Cleanup(ovsCleanup.Cleanup)

			var bridgeIPs, hostSubnets []*net.IPNet
			if test.ipv4Mode {
				bridgeIPs = append(bridgeIPs, mustParseIPNet(t, "10.1.253.253/16"))
				hostSubnets = append(hostSubnets, mustParseIPNet(t, "10.1.0.0/16"))
			}
			if test.ipv6Mode {
				bridgeIPs = append(bridgeIPs, mustParseIPNet(t, "fd00::253/64"))
				hostSubnets = append(hostSubnets, mustParseIPNet(t, "fd00::/64"))
			}
			bridge := &BridgeConfiguration{
				ovsClient:  ovsClient,
				bridgeName: "br0",
				ofPortPhys: "ens8",
				ofPortHost: nodetypes.OvsLocalPort,
				ips:        bridgeIPs,
				macAddress: mustParseMAC(t, "48:b0:2d:00:00:04"),
				netConfig: map[string]*BridgeUDNConfiguration{
					types.DefaultNetworkName: {
						OfPortPatch: "patch-br0_node-to-br-int",
						MasqCTMark:  nodetypes.CtMarkOVN,
					},
				},
			}

			flows, err := bridge.commonFlows(hostSubnets, types.DefaultNetworkName)
			if err != nil {
				t.Fatalf("failed to render bridge flows: %v", err)
			}
			priority102Cnt := 0
			for _, flow := range flows {
				if strings.Contains(flow, "priority=102") {
					priority102Cnt++
				}
			}
			if priority102Cnt != test.expectedPriority102Cnt {
				t.Fatalf("expected %d priority-102 flows, got %d: %v",
					test.expectedPriority102Cnt, priority102Cnt, flows)
			}
		})
	}
}

func TestSharedNoOverlayNodeIPFlowUsesNATInDefaultConntrackZone(t *testing.T) {
	if err := config.PrepareTestConfig(); err != nil {
		t.Fatalf("failed to prepare test config: %v", err)
	}
	t.Cleanup(func() {
		_ = config.PrepareTestConfig()
	})
	config.IPv4Mode = true
	config.IPv6Mode = true
	config.Gateway.Mode = config.GatewayModeShared

	bridgeMAC := mustParseMAC(t, "62:41:d0:54:3d:64")
	v4NodeIP := mustParseIPNet(t, "172.18.0.3/24")
	v6NodeIP := mustParseIPNet(t, "fd00::3/64")
	v4GatewayMasqIP := mustParseIPNet(t, "169.254.0.11/32")
	v4ManagementMasqIP := mustParseIPNet(t, "169.254.0.12/32")
	v6GatewayMasqIP := mustParseIPNet(t, "fd69::11/128")
	v6ManagementMasqIP := mustParseIPNet(t, "fd69::12/128")

	bridge := &BridgeConfiguration{
		ovsClient:  newBridgeFlowTestOVSClient(t, "breth0"),
		bridgeName: "breth0",
		ofPortPhys: "eth0",
		ofPortHost: nodetypes.OvsLocalPort,
		ips:        []*net.IPNet{v4NodeIP, v6NodeIP},
		macAddress: bridgeMAC,
		netConfig: map[string]*BridgeUDNConfiguration{
			types.DefaultNetworkName: {
				OfPortPatch: "patch-breth0_ov",
				MasqCTMark:  nodetypes.CtMarkOVN,
			},
			"bluenet": {
				OfPortPatch: "patch-breth0_bluenet",
				MasqCTMark:  "0x4",
				PktMark:     "0x3",
				Transport:   types.NetworkTransportNoOverlay,
				V4MasqIPs: &udngenerator.MasqueradeIPs{
					GatewayRouter:  v4GatewayMasqIP,
					ManagementPort: v4ManagementMasqIP,
				},
				V6MasqIPs: &udngenerator.MasqueradeIPs{
					GatewayRouter:  v6GatewayMasqIP,
					ManagementPort: v6ManagementMasqIP,
				},
			},
		},
	}

	flows, err := bridge.commonFlows(nil, types.DefaultNetworkName)
	if err != nil {
		t.Fatalf("failed to render bridge flows: %v", err)
	}

	expectedIPv4 := fmt.Sprintf("cookie=%s, priority=99, in_port=patch-breth0_bluenet, dl_src=%s, ip, nw_src=172.18.0.3, "+
		"actions=ct(commit, zone=%d, nat(src=172.18.0.3), exec(set_field:0x4->ct_mark)), output:eth0",
		nodetypes.DefaultOpenFlowCookie, bridgeMAC, config.Default.ConntrackZone)
	expectedIPv6 := fmt.Sprintf("cookie=%s, priority=99, in_port=patch-breth0_bluenet, dl_src=%s, ipv6, ipv6_src=fd00::3, "+
		"actions=ct(commit, zone=%d, nat(src=fd00::3), exec(set_field:0x4->ct_mark)), output:eth0",
		nodetypes.DefaultOpenFlowCookie, bridgeMAC, config.Default.ConntrackZone)

	expectFlow(t, flows, expectedIPv4)
	expectFlow(t, flows, expectedIPv6)
}

func TestUplinkBridgeServiceFlowsUseUDNMark(t *testing.T) {
	if err := config.PrepareTestConfig(); err != nil {
		t.Fatalf("failed to prepare test config: %v", err)
	}
	t.Cleanup(func() {
		_ = config.PrepareTestConfig()
	})
	config.IPv4Mode = true
	config.IPv6Mode = true
	config.Gateway.Mode = config.GatewayModeShared
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true
	config.Kubernetes.ServiceCIDRs = []*net.IPNet{
		mustParseIPNet(t, "10.96.0.0/16"),
		mustParseIPNet(t, "fd10:96::/112"),
	}

	bridgeMAC := mustParseMAC(t, "62:41:d0:54:3d:64")
	v4NodeIP := mustParseIPNet(t, "172.28.0.3/24")
	v6NodeIP := mustParseIPNet(t, "fd28::3/64")
	v4GatewayMasqIP := mustParseIPNet(t, "169.254.0.11/32")
	v4ManagementMasqIP := mustParseIPNet(t, "169.254.0.12/32")
	v6GatewayMasqIP := mustParseIPNet(t, "fd69::11/128")
	v6ManagementMasqIP := mustParseIPNet(t, "fd69::12/128")

	bridge := &BridgeConfiguration{
		ofPortPhys: "eth1",
		ofPortHost: nodetypes.OvsLocalPort,
		ips:        []*net.IPNet{v4NodeIP, v6NodeIP},
		macAddress: bridgeMAC,
		netConfig: map[string]*BridgeUDNConfiguration{
			"bluenet": {
				OfPortPatch: "patch-ovsbr1_bluenet",
				MasqCTMark:  "0x4",
				PktMark:     "0x1001",
				Transport:   types.NetworkTransportNoOverlay,
				V4MasqIPs: &udngenerator.MasqueradeIPs{
					GatewayRouter:  v4GatewayMasqIP,
					ManagementPort: v4ManagementMasqIP,
				},
				V6MasqIPs: &udngenerator.MasqueradeIPs{
					GatewayRouter:  v6GatewayMasqIP,
					ManagementPort: v6ManagementMasqIP,
				},
			},
		},
	}

	flows, err := bridge.UplinkBridgeFlows(nil, "bluenet")
	if err != nil {
		t.Fatalf("failed to render bridge flows: %v", err)
	}
	// Uplink bridges can carry UDNs without the default network.
	expectFlowContainingAll(t, flows,
		"priority=10, table=0,", "dl_dst=62:41:d0:54:3d:64",
		"actions=output:patch-ovsbr1_bluenet,NORMAL")

	expectedIPv4HostToService := fmt.Sprintf("cookie=%s, priority=500, in_port=LOCAL, ip, "+
		"pkt_mark=0x1001, ip_dst=10.96.0.0/16, "+
		"actions=ct(commit,zone=%d,nat(src=%s),table=2)",
		nodetypes.DefaultOpenFlowCookie, config.Default.HostMasqConntrackZone,
		config.Gateway.MasqueradeIPs.V4HostMasqueradeIP)
	expectedIPv4Dispatch := fmt.Sprintf("cookie=%s, priority=250, table=2, ip, pkt_mark=0x1001, "+
		"actions=set_field:%s->eth_dst,output:patch-ovsbr1_bluenet",
		nodetypes.DefaultOpenFlowCookie, bridgeMAC)
	expectedIPv4ReplyToHost := fmt.Sprintf("cookie=%s, priority=500, in_port=patch-ovsbr1_bluenet, "+
		"ip, ip_src=10.96.0.0/16, ip_dst=%s,actions=ct(zone=%d,nat,table=3)",
		nodetypes.DefaultOpenFlowCookie, config.Gateway.V4MasqueradeSubnet,
		config.Default.HostMasqConntrackZone)
	expectedIPv4ReplyToOVN := fmt.Sprintf("cookie=%s, priority=100, table=1, ip, ct_state=+trk+est, "+
		"ct_mark=0x4, actions=output:patch-ovsbr1_bluenet",
		nodetypes.DefaultOpenFlowCookie)
	expectedIPv6HostToService := fmt.Sprintf("cookie=%s, priority=500, in_port=LOCAL, ipv6, "+
		"pkt_mark=0x1001, ipv6_dst=fd10:96::/112, "+
		"actions=ct(commit,zone=%d,nat(src=%s),table=2)",
		nodetypes.DefaultOpenFlowCookie, config.Default.HostMasqConntrackZone,
		config.Gateway.MasqueradeIPs.V6HostMasqueradeIP)
	expectedIPv6Dispatch := fmt.Sprintf("cookie=%s, priority=250, table=2, ipv6, pkt_mark=0x1001, "+
		"actions=set_field:%s->eth_dst,output:patch-ovsbr1_bluenet",
		nodetypes.DefaultOpenFlowCookie, bridgeMAC)
	expectedIPv6ReplyToHost := fmt.Sprintf("cookie=%s, priority=500, in_port=patch-ovsbr1_bluenet, "+
		"ipv6, ipv6_src=fd10:96::/112, ipv6_dst=%s,actions=ct(zone=%d,nat,table=3)",
		nodetypes.DefaultOpenFlowCookie, config.Gateway.V6MasqueradeSubnet,
		config.Default.HostMasqConntrackZone)
	expectedIPv6ReplyToOVN := fmt.Sprintf("cookie=%s, priority=100, table=1, ipv6, ct_state=+trk+est, "+
		"ct_mark=0x4, actions=output:patch-ovsbr1_bluenet",
		nodetypes.DefaultOpenFlowCookie)
	expectedTable3 := fmt.Sprintf("cookie=%s, table=3,  "+
		"actions=move:NXM_OF_ETH_DST[]->NXM_OF_ETH_SRC[],set_field:%s->eth_dst,output:LOCAL",
		nodetypes.DefaultOpenFlowCookie, bridgeMAC)

	expectFlow(t, flows, expectedIPv4HostToService)
	expectFlow(t, flows, expectedIPv4Dispatch)
	expectFlow(t, flows, expectedIPv4ReplyToHost)
	expectFlow(t, flows, expectedIPv4ReplyToOVN)
	expectFlow(t, flows, expectedIPv6HostToService)
	expectFlow(t, flows, expectedIPv6Dispatch)
	expectFlow(t, flows, expectedIPv6ReplyToHost)
	expectFlow(t, flows, expectedIPv6ReplyToOVN)
	expectFlow(t, flows, expectedTable3)
	expectNoFlow(t, flows, fmt.Sprintf("priority=500, in_port=LOCAL, ip, ip_dst=10.96.0.0/16, "+
		"actions=ct(commit,zone=%d,nat(src=%s),table=2)",
		config.Default.HostMasqConntrackZone, config.Gateway.MasqueradeIPs.V4HostMasqueradeIP))
}

func TestLocalNoOverlayServiceHairpinUsesUDNGatewayMasqueradeIP(t *testing.T) {
	if err := config.PrepareTestConfig(); err != nil {
		t.Fatalf("failed to prepare test config: %v", err)
	}
	t.Cleanup(func() {
		_ = config.PrepareTestConfig()
	})
	config.IPv4Mode = true
	config.IPv6Mode = true
	config.Gateway.Mode = config.GatewayModeLocal

	bridgeMAC := mustParseMAC(t, "62:41:d0:54:3d:64")
	v4NodeIP := mustParseIPNet(t, "172.18.0.3/24")
	v6NodeIP := mustParseIPNet(t, "fd00::3/64")
	v4GatewayMasqIP := mustParseIPNet(t, "169.254.0.11/32")
	v4ManagementMasqIP := mustParseIPNet(t, "169.254.0.12/32")
	v6GatewayMasqIP := mustParseIPNet(t, "fd69::11/128")
	v6ManagementMasqIP := mustParseIPNet(t, "fd69::12/128")

	bridge := &BridgeConfiguration{
		ovsClient:  newBridgeFlowTestOVSClient(t, "breth0"),
		bridgeName: "breth0",
		ofPortHost: nodetypes.OvsLocalPort,
		ips:        []*net.IPNet{v4NodeIP, v6NodeIP},
		macAddress: bridgeMAC,
		netConfig: map[string]*BridgeUDNConfiguration{
			types.DefaultNetworkName: {
				OfPortPatch: "patch-breth0_ov",
				MasqCTMark:  nodetypes.CtMarkOVN,
			},
			"bluenet": {
				OfPortPatch: "patch-breth0_bluenet",
				MasqCTMark:  "0x4",
				PktMark:     "0x3",
				Transport:   types.NetworkTransportNoOverlay,
				Subnets: []config.CIDRNetworkEntry{
					{CIDR: mustParseIPNet(t, "10.128.0.0/16")},
					{CIDR: mustParseIPNet(t, "fd10:128::/64")},
				},
				V4MasqIPs: &udngenerator.MasqueradeIPs{
					GatewayRouter:  v4GatewayMasqIP,
					ManagementPort: v4ManagementMasqIP,
				},
				V6MasqIPs: &udngenerator.MasqueradeIPs{
					GatewayRouter:  v6GatewayMasqIP,
					ManagementPort: v6ManagementMasqIP,
				},
			},
		},
	}

	flows, err := bridge.flowsForDefaultBridge(nil)
	if err != nil {
		t.Fatalf("failed to render bridge flows: %v", err)
	}

	expectedIPv4Hairpin := fmt.Sprintf("cookie=%s, priority=100, table=4, in_port=patch-breth0_bluenet, ip, ip_dst=10.128.0.0/16, "+
		"actions=ct(commit,zone=%d,nat(src=169.254.0.11),exec(set_field:0x4->ct_mark),table=3)",
		nodetypes.DefaultOpenFlowCookie, config.Default.OVNMasqConntrackZone)
	expectedIPv4Reply := fmt.Sprintf("cookie=%s, priority=500, in_port=LOCAL, ip, ip_dst=169.254.0.11,"+
		"actions=ct(zone=%d,nat,table=5)",
		nodetypes.DefaultOpenFlowCookie, config.Default.OVNMasqConntrackZone)
	expectedIPv4Table5 := fmt.Sprintf("cookie=%s, priority=100, table=5, ip, ct_mark=0x4, "+
		"actions=ct(commit,zone=%d,nat),set_field:%s->eth_dst,output:patch-breth0_bluenet",
		nodetypes.DefaultOpenFlowCookie, config.Default.HostMasqConntrackZone, bridgeMAC)

	expectedIPv6Hairpin := fmt.Sprintf("cookie=%s, priority=100, table=4, in_port=patch-breth0_bluenet, ipv6, ipv6_dst=fd10:128::/64, "+
		"actions=ct(commit,zone=%d,nat(src=fd69::11),exec(set_field:0x4->ct_mark),table=3)",
		nodetypes.DefaultOpenFlowCookie, config.Default.OVNMasqConntrackZone)
	expectedIPv6Reply := fmt.Sprintf("cookie=%s, priority=500, in_port=LOCAL, ipv6, ipv6_dst=fd69::11,"+
		"actions=ct(zone=%d,nat,table=5)",
		nodetypes.DefaultOpenFlowCookie, config.Default.OVNMasqConntrackZone)
	expectedIPv6Table5 := fmt.Sprintf("cookie=%s, priority=100, table=5, ipv6, ct_mark=0x4, "+
		"actions=ct(commit,zone=%d,nat),set_field:%s->eth_dst,output:patch-breth0_bluenet",
		nodetypes.DefaultOpenFlowCookie, config.Default.HostMasqConntrackZone, bridgeMAC)

	expectFlow(t, flows, expectedIPv4Hairpin)
	expectFlow(t, flows, expectedIPv4Reply)
	expectFlow(t, flows, expectedIPv4Table5)
	expectFlow(t, flows, expectedIPv6Hairpin)
	expectFlow(t, flows, expectedIPv6Reply)
	expectFlow(t, flows, expectedIPv6Table5)
}

func TestArpFanoutFilterFlowsIncludeVLAN(t *testing.T) {
	if err := config.PrepareTestConfig(); err != nil {
		t.Fatalf("failed to prepare test config: %v", err)
	}
	t.Cleanup(func() {
		_ = config.PrepareTestConfig()
	})
	config.IPv4Mode = true
	config.IPv6Mode = true
	config.Gateway.Mode = config.GatewayModeShared
	config.Gateway.VLANID = 100
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true

	bridgeMAC := mustParseMAC(t, "62:41:d0:54:3d:64")
	v4NodeIP := mustParseIPNet(t, "172.18.0.3/24")
	v6NodeIP := mustParseIPNet(t, "fd00::3/64")
	v4GatewayMasqIP := mustParseIPNet(t, "169.254.0.11/32")
	v4ManagementMasqIP := mustParseIPNet(t, "169.254.0.12/32")
	v6GatewayMasqIP := mustParseIPNet(t, "fd69::11/128")
	v6ManagementMasqIP := mustParseIPNet(t, "fd69::12/128")

	bridge := &BridgeConfiguration{
		ovsClient:  newBridgeFlowTestOVSClient(t, "breth0"),
		bridgeName: "breth0",
		ofPortPhys: "eth0",
		ofPortHost: nodetypes.OvsLocalPort,
		ips:        []*net.IPNet{v4NodeIP, v6NodeIP},
		macAddress: bridgeMAC,
		netConfig: map[string]*BridgeUDNConfiguration{
			types.DefaultNetworkName: {
				OfPortPatch: "patch-breth0_ov",
				MasqCTMark:  nodetypes.CtMarkOVN,
			},
			"bluenet": {
				OfPortPatch: "patch-breth0_bluenet",
				MasqCTMark:  "0x4",
				V4MasqIPs: &udngenerator.MasqueradeIPs{
					GatewayRouter:  v4GatewayMasqIP,
					ManagementPort: v4ManagementMasqIP,
				},
				V6MasqIPs: &udngenerator.MasqueradeIPs{
					GatewayRouter:  v6GatewayMasqIP,
					ManagementPort: v6ManagementMasqIP,
				},
			},
		},
	}

	flows, err := bridge.commonFlows(nil, types.DefaultNetworkName)
	if err != nil {
		t.Fatalf("failed to render bridge flows: %v", err)
	}

	expectedIPv4 := fmt.Sprintf("cookie=%s, priority=12, table=0, dl_vlan=100, arp, arp_op=1, arp_tpa=172.18.0.3, "+
		"actions=output:patch-breth0_ov,NORMAL",
		nodetypes.DefaultOpenFlowCookie)
	expectedIPv6 := fmt.Sprintf("cookie=%s, priority=12, table=0, dl_vlan=100, icmp6, icmpv6_type=%d, nd_target=fd00::3, "+
		"actions=output:patch-breth0_ov,NORMAL",
		nodetypes.DefaultOpenFlowCookie, types.NeighborSolicitationICMPType)

	expectFlow(t, flows, expectedIPv4)
	expectFlow(t, flows, expectedIPv6)
	expectFlowContainingAll(t, flows,
		"priority=10, table=0, dl_vlan=100, dl_dst=62:41:d0:54:3d:64",
		"actions=output:patch-breth0_ov,output:patch-breth0_bluenet,NORMAL")
	// NORMAL reaches default first; only primary UDNs need explicit outputs.
	expectFlowContainingAll(t, flows,
		fmt.Sprintf("cookie=%s", nodetypes.DefaultOpenFlowCookie),
		"priority=11", "in_port=eth0", "dl_vlan=100", "dl_dst=ff:ff:ff:ff:ff:ff", "arp",
		"actions=NORMAL,output:patch-breth0_bluenet")
	expectFlowContainingAll(t, flows,
		fmt.Sprintf("cookie=%s", nodetypes.DefaultOpenFlowCookie),
		"priority=11", "in_port=eth0", "dl_vlan=100", "dl_dst=33:33:00:00:00:01",
		fmt.Sprintf("icmpv6_type=%d", types.NeighborAdvertisementICMPType),
		"actions=NORMAL,output:patch-breth0_bluenet")
	for _, icmpType := range []int{types.RouteAdvertisementICMPType, types.NeighborAdvertisementICMPType} {
		expectFlow(t, flows, fmt.Sprintf("cookie=%s, priority=14, table=1,icmp6,icmpv6_type=%d actions=FLOOD,output:patch-breth0_bluenet",
			nodetypes.DefaultOpenFlowCookie, icmpType))
	}
	// Exercise priority 10 on a bridge carrying only the default network.
	delete(bridge.netConfig, "bluenet")
	flows, err = bridge.commonFlows(nil, types.DefaultNetworkName)
	if err != nil {
		t.Fatalf("failed to render bridge flows: %v", err)
	}
	expectFlow(t, flows, fmt.Sprintf("cookie=%s, priority=10, table=0, dl_vlan=100, dl_dst=62:41:d0:54:3d:64, actions=output:patch-breth0_ov,NORMAL",
		nodetypes.DefaultOpenFlowCookie))
}

// TestARPNDPSteeringFlowsWhenFloodDisabled is the DisableUDNARPNDPFlood twin of
// TestArpFanoutFilterFlowsIncludeVLAN: the priority 52 steering flows replace
// every ARP/NDP fan-out flow rather than sitting alongside them.
func TestARPNDPSteeringFlowsWhenFloodDisabled(t *testing.T) {
	if err := config.PrepareTestConfig(); err != nil {
		t.Fatalf("failed to prepare test config: %v", err)
	}
	t.Cleanup(func() {
		_ = config.PrepareTestConfig()
	})
	config.IPv4Mode = true
	config.IPv6Mode = true
	config.Gateway.Mode = config.GatewayModeShared
	config.Gateway.VLANID = 100
	config.Gateway.DisableUDNARPNDPFlood = true
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true

	bridgeMAC := mustParseMAC(t, "62:41:d0:54:3d:64")
	v4NodeIP := mustParseIPNet(t, "172.18.0.3/24")
	v6NodeIP := mustParseIPNet(t, "fd00::3/64")
	v4GatewayMasqIP := mustParseIPNet(t, "169.254.0.11/32")
	v4ManagementMasqIP := mustParseIPNet(t, "169.254.0.12/32")
	v6GatewayMasqIP := mustParseIPNet(t, "fd69::11/128")
	v6ManagementMasqIP := mustParseIPNet(t, "fd69::12/128")

	bridge := &BridgeConfiguration{
		ovsClient:  newBridgeFlowTestOVSClient(t, "breth0"),
		bridgeName: "breth0",
		ofPortPhys: "eth0",
		ofPortHost: nodetypes.OvsLocalPort,
		ips:        []*net.IPNet{v4NodeIP, v6NodeIP},
		macAddress: bridgeMAC,
		netConfig: map[string]*BridgeUDNConfiguration{
			types.DefaultNetworkName: {
				OfPortPatch: "patch-breth0_ov",
				MasqCTMark:  nodetypes.CtMarkOVN,
			},
			"bluenet": {
				OfPortPatch: "patch-breth0_bluenet",
				MasqCTMark:  "0x4",
				V4MasqIPs: &udngenerator.MasqueradeIPs{
					GatewayRouter:  v4GatewayMasqIP,
					ManagementPort: v4ManagementMasqIP,
				},
				V6MasqIPs: &udngenerator.MasqueradeIPs{
					GatewayRouter:  v6GatewayMasqIP,
					ManagementPort: v6ManagementMasqIP,
				},
			},
		},
	}

	flows, err := bridge.commonFlows(nil, types.DefaultNetworkName)
	if err != nil {
		t.Fatalf("failed to render bridge flows: %v", err)
	}

	expectFlow(t, flows, fmt.Sprintf("cookie=%s, priority=52, table=0, in_port=eth0, dl_vlan=100, dl_dst=%s, arp, "+
		"actions=output:patch-breth0_ov,NORMAL", nodetypes.DefaultOpenFlowCookie, bridgeMAC))
	for _, icmpType := range []int{
		types.NeighborAdvertisementICMPType,
		types.RouteAdvertisementICMPType,
		types.NeighborSolicitationICMPType,
	} {
		expectFlow(t, flows, fmt.Sprintf("cookie=%s, priority=52, table=0, in_port=eth0, dl_vlan=100, dl_dst=%s, "+
			"icmp6, icmpv6_type=%d, actions=output:patch-breth0_ov,NORMAL",
			nodetypes.DefaultOpenFlowCookie, bridgeMAC, icmpType))
	}

	// Every ARP/NDP fan-out flow must be gone, so there is no hidden path left
	// that bypasses no-flood and reaches the CUDN patch ports.
	expectNoFlowContainingAll(t, flows, "priority=10, table=0,", fmt.Sprintf("dl_dst=%s", bridgeMAC))
	expectNoFlowContainingAll(t, flows, "priority=11, table=0,")
	expectNoFlowContainingAll(t, flows, "priority=12, table=0,")
	expectNoFlowContainingAll(t, flows, "priority=14, table=1,")
	expectNoFlowContainingAll(t, flows, "priority=45, table=0,")
}

// TestARPNDPSteeringFlowsOnUplinkBridge covers a bridge with no default network,
// where steering must target the designated source UDN instead.
func TestARPNDPSteeringFlowsOnUplinkBridge(t *testing.T) {
	if err := config.PrepareTestConfig(); err != nil {
		t.Fatalf("failed to prepare test config: %v", err)
	}
	t.Cleanup(func() {
		_ = config.PrepareTestConfig()
	})
	config.IPv4Mode = true
	config.IPv6Mode = false
	config.Gateway.Mode = config.GatewayModeShared
	config.Gateway.DisableUDNARPNDPFlood = true
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true

	bridgeMAC := mustParseMAC(t, "62:41:d0:54:3d:64")
	bridge := &BridgeConfiguration{
		ovsClient:  newBridgeFlowTestOVSClient(t, "ovsbr1"),
		bridgeName: "ovsbr1",
		ofPortPhys: "eth1",
		ofPortHost: nodetypes.OvsLocalPort,
		ips:        []*net.IPNet{mustParseIPNet(t, "172.28.0.3/24")},
		macAddress: bridgeMAC,
		netConfig: map[string]*BridgeUDNConfiguration{
			"bluenet": {
				OfPortPatch: "patch-ovsbr1_bluenet",
				MasqCTMark:  "0x4",
				V4MasqIPs: &udngenerator.MasqueradeIPs{
					GatewayRouter:  mustParseIPNet(t, "169.254.0.11/32"),
					ManagementPort: mustParseIPNet(t, "169.254.0.12/32"),
				},
			},
			"rednet": {
				OfPortPatch: "patch-ovsbr1_rednet",
				MasqCTMark:  "0x5",
				V4MasqIPs: &udngenerator.MasqueradeIPs{
					GatewayRouter:  mustParseIPNet(t, "169.254.0.13/32"),
					ManagementPort: mustParseIPNet(t, "169.254.0.14/32"),
				},
			},
		},
	}

	flows, err := bridge.UplinkBridgeFlows(nil, "rednet")
	if err != nil {
		t.Fatalf("failed to render bridge flows: %v", err)
	}

	expectFlow(t, flows, fmt.Sprintf("cookie=%s, priority=52, table=0, in_port=eth1,  dl_dst=%s, arp, "+
		"actions=output:patch-ovsbr1_rednet,NORMAL", nodetypes.DefaultOpenFlowCookie, bridgeMAC))
}

// TestARPNDPSteeringFlowsCoverLocalnetPorts checks that secondary localnet patch
// ports are part of the external-ingress set, and that one without an assigned
// ofport is skipped rather than rendering a broken flow.
func TestARPNDPSteeringFlowsCoverLocalnetPorts(t *testing.T) {
	if err := config.PrepareTestConfig(); err != nil {
		t.Fatalf("failed to prepare test config: %v", err)
	}
	t.Cleanup(func() {
		_ = config.PrepareTestConfig()
	})
	config.IPv4Mode = true
	config.IPv6Mode = false
	config.Gateway.Mode = config.GatewayModeShared
	config.Gateway.DisableUDNARPNDPFlood = true
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true

	localnetOfPort := 7
	ovsClient, ovsCleanup, err := libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{
		OVSData: []libovsdbtest.TestData{
			&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{"breth0-uuid"}},
			&vswitchd.Bridge{UUID: "breth0-uuid", Name: "breth0", Ports: []string{"localnet-port-uuid", "pending-port-uuid"}},
			&vswitchd.Port{
				UUID:        "localnet-port-uuid",
				Name:        "patch-blue_ovn_localnet_port-to-br-int",
				Interfaces:  []string{"localnet-iface-uuid"},
				ExternalIDs: map[string]string{"ovn-localnet-port": "blue_ovn_localnet_port"},
			},
			&vswitchd.Interface{
				UUID:   "localnet-iface-uuid",
				Name:   "patch-blue_ovn_localnet_port-to-br-int",
				Type:   "patch",
				Ofport: &localnetOfPort,
			},
			// no ofport assigned yet: must be skipped, not rendered as in_port=
			&vswitchd.Port{
				UUID:        "pending-port-uuid",
				Name:        "patch-red_ovn_localnet_port-to-br-int",
				Interfaces:  []string{"pending-iface-uuid"},
				ExternalIDs: map[string]string{"ovn-localnet-port": "red_ovn_localnet_port"},
			},
			&vswitchd.Interface{UUID: "pending-iface-uuid", Name: "patch-red_ovn_localnet_port-to-br-int", Type: "patch"},
		},
	})
	if err != nil {
		t.Fatalf("failed to create OVS test harness: %v", err)
	}
	t.Cleanup(ovsCleanup.Cleanup)

	bridgeMAC := mustParseMAC(t, "62:41:d0:54:3d:64")
	bridge := &BridgeConfiguration{
		ovsClient:  ovsClient,
		bridgeName: "breth0",
		ofPortPhys: "eth0",
		ofPortHost: nodetypes.OvsLocalPort,
		ips:        []*net.IPNet{mustParseIPNet(t, "172.18.0.3/24")},
		macAddress: bridgeMAC,
		netConfig: map[string]*BridgeUDNConfiguration{
			types.DefaultNetworkName: {
				OfPortPatch: "patch-breth0_ov",
				MasqCTMark:  nodetypes.CtMarkOVN,
			},
		},
	}

	flows, err := bridge.commonFlows(nil, types.DefaultNetworkName)
	if err != nil {
		t.Fatalf("failed to render bridge flows: %v", err)
	}

	for _, inPort := range []string{"eth0", "7"} {
		expectFlow(t, flows, fmt.Sprintf("cookie=%s, priority=52, table=0, in_port=%s,  dl_dst=%s, arp, "+
			"actions=output:patch-breth0_ov,NORMAL", nodetypes.DefaultOpenFlowCookie, inPort, bridgeMAC))
	}
	expectNoFlowContainingAll(t, flows, "priority=52", "in_port=,")
}

func mustParseMAC(t *testing.T, value string) net.HardwareAddr {
	t.Helper()
	mac, err := net.ParseMAC(value)
	if err != nil {
		t.Fatalf("failed to parse MAC %q: %v", value, err)
	}
	return mac
}

func mustParseIPNet(t *testing.T, value string) *net.IPNet {
	t.Helper()
	ip, ipNet, err := net.ParseCIDR(value)
	if err != nil {
		t.Fatalf("failed to parse CIDR %q: %v", value, err)
	}
	ipNet.IP = ip
	return ipNet
}

func expectFlow(t *testing.T, flows []string, expected string) {
	t.Helper()
	for _, flow := range flows {
		if flow == expected {
			return
		}
	}
	t.Fatalf("expected flow not found:\n%s\n\nall flows:\n%v", expected, flows)
}

func expectFlowContainingAll(t *testing.T, flows []string, substrings ...string) {
	t.Helper()
	for _, flow := range flows {
		allFound := true
		for _, s := range substrings {
			if !strings.Contains(flow, s) {
				allFound = false
				break
			}
		}
		if allFound {
			return
		}
	}
	t.Fatalf("no flow contains all substrings %v\n\nall flows:\n%v", substrings, flows)
}

func expectNoFlowContainingAll(t *testing.T, flows []string, substrings ...string) {
	t.Helper()
	for _, flow := range flows {
		allFound := true
		for _, s := range substrings {
			if !strings.Contains(flow, s) {
				allFound = false
				break
			}
		}
		if allFound {
			t.Fatalf("unexpected flow containing all substrings %v:\n%s", substrings, flow)
		}
	}
}

func expectNoFlow(t *testing.T, flows []string, unexpectedSubstring string) {
	t.Helper()
	for _, flow := range flows {
		if strings.Contains(flow, unexpectedSubstring) {
			t.Fatalf("unexpected flow found:\n%s\n\nall flows:\n%v", flow, flows)
		}
	}
}
