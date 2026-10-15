// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package bridgeconfig

import (
	"fmt"
	"net"
	"strings"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	net2 "k8s.io/utils/net"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	nodetypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
)

func TestDefaultBridgeConfig() *BridgeConfiguration {
	defaultNetConfig := &BridgeUDNConfiguration{
		OfPortPatch: "patch-breth0_ov",
	}
	return &BridgeConfiguration{
		bridgeName: "breth0",
		gwIface:    "breth0",
		uplinkName: "eth0",
		ofPortPhys: "eth0",
		netConfig: map[string]*BridgeUDNConfiguration{
			types.DefaultNetworkName: defaultNetConfig,
		},
	}
}

// TestDefaultBridgeConfigWithOVSClient returns a default bridge test
// configuration backed by the provided OVSDB client and gateway addresses.
func TestDefaultBridgeConfigWithOVSClient(ovsClient libovsdbclient.Client, ips []*net.IPNet, mac net.HardwareAddr) *BridgeConfiguration {
	bridge := TestDefaultBridgeConfig()
	bridge.ovsClient = ovsClient
	bridge.ips = ips
	bridge.macAddress = mac
	bridge.ofPortHost = nodetypes.OvsLocalPort
	bridge.netConfig[types.DefaultNetworkName].MasqCTMark = nodetypes.CtMarkOVN
	return bridge
}

func TestBridgeConfig(brName string) *BridgeConfiguration {
	return &BridgeConfiguration{
		bridgeName: brName,
		gwIface:    brName,
	}
}

// TestUplinkBridgeConfig returns a bridge configuration for an Uplink bridge
// with the given physical port and the ofport its flows were generated with.
func TestUplinkBridgeConfig(brName, uplinkName, ofPortPhys string) *BridgeConfiguration {
	bridge := TestBridgeConfig(brName)
	bridge.uplinkName = uplinkName
	bridge.ofPortPhys = ofPortPhys
	bridge.netConfig = map[string]*BridgeUDNConfiguration{}
	return bridge
}

func TestBridgeConfigWithGatewayRepresentor(brName, gwIfaceRep string) *BridgeConfiguration {
	bridge := TestBridgeConfig(brName)
	bridge.gwIfaceRep = gwIfaceRep
	return bridge
}

func (b *BridgeConfiguration) GetNetConfigLen() int {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	return len(b.netConfig)
}

func CheckUDNSvcIsolationOVSFlows(flows []string, netConfig *BridgeUDNConfiguration, netName string, svcCIDR *net.IPNet, expectedNFlows int) {
	ginkgo.By(fmt.Sprintf("Checking UDN %s service isolation flows for %s; expected %d flows",
		netName, svcCIDR.String(), expectedNFlows))

	var mgmtMasqIP string
	var protoPrefix string
	if net2.IsIPv4CIDR(svcCIDR) {
		mgmtMasqIP = netConfig.V4MasqIPs.ManagementPort.IP.String()
		protoPrefix = protoPrefixV4
	} else {
		mgmtMasqIP = netConfig.V6MasqIPs.ManagementPort.IP.String()
		protoPrefix = protoPrefixV6
	}

	var nFlows int
	for _, flow := range flows {
		if strings.Contains(flow, fmt.Sprintf("priority=200, table=2, %s, %s_src=%s, actions=drop",
			protoPrefix, protoPrefix, mgmtMasqIP)) {
			nFlows++
		}
	}

	gomega.Expect(nFlows).To(gomega.Equal(expectedNFlows))
}

func CheckAdvertisedUDNSvcIsolationOVSFlows(flows []string, netConfig *BridgeUDNConfiguration, netName string, svcCIDR *net.IPNet, expectedNFlows int) {
	ginkgo.By(fmt.Sprintf("Checking advertised UDN %s service isolation flows for %s; expected %d flows",
		netName, svcCIDR.String(), expectedNFlows))

	var protoPrefix string
	var matchingIPFamilySubnets []*net.IPNet
	if net2.IsIPv4CIDR(svcCIDR) {
		protoPrefix = protoPrefixV4
	} else {
		protoPrefix = protoPrefixV6
	}
	for _, clusterEntry := range netConfig.Subnets {
		if net2.IsIPv6CIDR(clusterEntry.CIDR) == net2.IsIPv6CIDR(svcCIDR) {
			matchingIPFamilySubnets = append(matchingIPFamilySubnets, clusterEntry.CIDR)
		}
	}
	gomega.Expect(matchingIPFamilySubnets).NotTo(gomega.BeEmpty())

	var nFlows int
	for _, flow := range flows {
		for _, matchingIPFamilySubnet := range matchingIPFamilySubnets {
			if strings.Contains(flow, fmt.Sprintf("priority=200, table=2, %s, %s_src=%s, actions=drop",
				protoPrefix, protoPrefix, matchingIPFamilySubnet)) {
				nFlows++
			}
			if strings.Contains(flow, fmt.Sprintf("priority=550, in_port=LOCAL, %s, %s_src=%s, %s_dst=%s, actions=ct(commit,zone=64001,table=2)",
				protoPrefix, protoPrefix, matchingIPFamilySubnet, protoPrefix, svcCIDR)) {
				nFlows++
			}
		}
	}

	gomega.Expect(nFlows).To(gomega.Equal(expectedNFlows))
}

func CheckDefaultSvcIsolationOVSFlows(flows []string, defaultConfig *BridgeUDNConfiguration, ofPortHost, bridgeMAC string, svcCIDR *net.IPNet) {
	ginkgo.By(fmt.Sprintf("Checking default service isolation flows for %s", svcCIDR.String()))

	var masqIP string
	var masqSubnet string
	var protoPrefix string
	if net2.IsIPv4CIDR(svcCIDR) {
		protoPrefix = protoPrefixV4
		masqIP = config.Gateway.MasqueradeIPs.V4HostMasqueradeIP.String()
		masqSubnet = config.Gateway.V4MasqueradeSubnet
	} else {
		protoPrefix = protoPrefixV6
		masqIP = config.Gateway.MasqueradeIPs.V6HostMasqueradeIP.String()
		masqSubnet = config.Gateway.V6MasqueradeSubnet
	}

	var nTable0DefaultFlows int
	var nTable0UDNMasqFlows int
	var nTable2Flows int
	for _, flow := range flows {
		if strings.Contains(flow, fmt.Sprintf("priority=500, in_port=%s, %s, %s_dst=%s, actions=ct(commit,zone=%d,nat(src=%s),table=2)",
			ofPortHost, protoPrefix, protoPrefix, svcCIDR, config.Default.HostMasqConntrackZone,
			masqIP)) {
			nTable0DefaultFlows++
		} else if strings.Contains(flow, fmt.Sprintf("priority=550, in_port=%s, %s, %s_src=%s, %s_dst=%s, actions=ct(commit,zone=%d,table=2)",
			ofPortHost, protoPrefix, protoPrefix, masqSubnet, protoPrefix, svcCIDR, config.Default.HostMasqConntrackZone)) {
			nTable0UDNMasqFlows++
		} else if strings.Contains(flow, fmt.Sprintf("priority=100, table=2, actions=set_field:%s->eth_dst,output:%s",
			bridgeMAC, defaultConfig.OfPortPatch)) {
			nTable2Flows++
		}
	}

	gomega.Expect(nTable0DefaultFlows).To(gomega.Equal(1))
	gomega.Expect(nTable0UDNMasqFlows).To(gomega.Equal(1))
	gomega.Expect(nTable2Flows).To(gomega.Equal(1))
}
