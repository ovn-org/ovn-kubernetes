// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package bridgeconfig

import (
	"fmt"
	"net"
	"strings"

	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	nodetypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/types"
	nodeutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

const (
	protoPrefixV4 = "ip"
	protoPrefixV6 = "ipv6"
)

func matchIPNetFamily(isIPv6 bool, ipnets []*net.IPNet) []*net.IPNet {
	var matches []*net.IPNet
	for _, ipnet := range ipnets {
		if utilnet.IsIPv6CIDR(ipnet) == isIPv6 {
			matches = append(matches, ipnet)
		}
	}
	return matches
}

func (b *BridgeConfiguration) DefaultBridgeFlows(hostSubnets []*net.IPNet, extraIPs []net.IP) ([]string, error) {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	dftFlows, err := b.flowsForDefaultBridge(extraIPs)
	if err != nil {
		return nil, err
	}
	dftCommonFlows, err := b.commonFlows(hostSubnets, types.DefaultNetworkName)
	if err != nil {
		return nil, err
	}
	return append(dftFlows, dftCommonFlows...), nil
}

func (b *BridgeConfiguration) ExternalBridgeFlows(hostSubnets []*net.IPNet) ([]string, error) {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	return b.commonFlows(hostSubnets, types.DefaultNetworkName)
}

// UplinkBridgeFlows generates the flows for an uplink bridge. sourceNetwork is
// the network whose gateway router is designated as the source of ARP/NDP
// neighbour resolution for this bridge; unlike br-ex, an uplink bridge has no
// default network to fall back on.
func (b *BridgeConfiguration) UplinkBridgeFlows(hostSubnets []*net.IPNet, sourceNetwork string) ([]string, error) {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	uplinkFlows := b.uplinkHostServiceFlows()
	commonFlows, err := b.commonFlows(hostSubnets, sourceNetwork)
	if err != nil {
		return nil, err
	}
	return append(uplinkFlows, commonFlows...), nil
}

// must be called with bridge.mutex held
func (b *BridgeConfiguration) flowsForDefaultBridge(extraIPs []net.IP) ([]string, error) {
	// CAUTION: when adding new flows where the in_port is ofPortPatch and the out_port is ofPortPhys, ensure
	// that dl_src is included in match criteria!

	ofPortPhys := b.ofPortPhys
	bridgeMacAddress := b.macAddress.String()
	ofPortHost := b.ofPortHost
	bridgeIPs := b.ips

	var dftFlows []string
	// 14 bytes of overhead for ethernet header (does not include VLAN)
	maxPktLength := getMaxFrameLength()

	strip_vlan := ""
	mod_vlan_id := ""
	match_vlan := ""
	if config.Gateway.VLANID != 0 {
		strip_vlan = "strip_vlan,"
		match_vlan = fmt.Sprintf("dl_vlan=%d,", config.Gateway.VLANID)
		mod_vlan_id = fmt.Sprintf("mod_vlan_vid:%d,", config.Gateway.VLANID)
	}

	// Problem: ovn-controller connects to SB DB and then GARPs for any EIPs configured however for IC, SB DB maybe stale if
	// ovnkube-controller is not processing.
	// Solution: add a logical flow on startup to allow GARPs from Node IPs but drop other GARPs and remove when ovnkube-controller
	// has sync'd and changes propagated to OVN SB DB.
	// remove when ovn contains native support for logical router ports to contain an option to silence GARPs on startup of ovn-controller.
	// https://issues.redhat.com/browse/FDP-1537
	if b.dropGARP {
		// priority 499 flows to allow GARP pkts when src IP is a Node IP
		dftFlows = append(dftFlows, b.allowNodeIPGARPFlows(extraIPs)...)
		// priority 498 flows to drop GARP pkts with no regards to src IP
		dftFlows = append(dftFlows, b.dropGARPFlows()...)
	}

	if config.IPv4Mode {
		// table0, Geneve packets coming from external. Skip conntrack and go directly to host
		// if dest mac is the shared mac send directly to host.
		if ofPortPhys != "" {
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=205, in_port=%s, dl_dst=%s, udp, udp_dst=%d, "+
					"actions=output:%s", nodetypes.DefaultOpenFlowCookie, ofPortPhys, bridgeMacAddress, config.Default.EncapPort,
					ofPortHost))
			// perform NORMAL action otherwise.
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=200, in_port=%s, udp, udp_dst=%d, "+
					"actions=NORMAL", nodetypes.DefaultOpenFlowCookie, ofPortPhys, config.Default.EncapPort))

			// table0, Geneve packets coming from LOCAL/Host OFPort. Skip conntrack and go directly to external
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=200, in_port=%s, udp, udp_dst=%d, "+
					"actions=output:%s", nodetypes.DefaultOpenFlowCookie, ofPortHost, config.Default.EncapPort, ofPortPhys))
		}
		physicalIP, err := util.MatchFirstIPNetFamily(false, bridgeIPs)
		if err != nil {
			return nil, fmt.Errorf("unable to determine IPv4 physical IP of host: %v", err)
		}
		for _, netConfig := range b.patchedNetConfigs() {
			// table 0, SVC Hairpin from OVN destined to local host, DNAT and go to table 4
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=500, in_port=%s, %s, %s_dst=%s, %s_src=%s,"+
					"actions=ct(commit,zone=%d,nat(dst=%s),table=4)",
					nodetypes.DefaultOpenFlowCookie, netConfig.OfPortPatch, protoPrefixV4, protoPrefixV4,
					config.Gateway.MasqueradeIPs.V4HostMasqueradeIP.String(), protoPrefixV4, physicalIP.IP,
					config.Default.HostMasqConntrackZone, physicalIP.IP))
		}

		// table 0, hairpin from OVN destined to local host (but an additional node IP), send to table 4
		for _, ip := range extraIPs {
			if ip.To4() == nil {
				continue
			}
			// not needed for the physical IP
			if ip.Equal(physicalIP.IP) {
				continue
			}

			// not needed for special masquerade IP
			if ip.Equal(config.Gateway.MasqueradeIPs.V4HostMasqueradeIP) {
				continue
			}

			for _, netConfig := range b.patchedNetConfigs() {
				dftFlows = append(dftFlows,
					fmt.Sprintf("cookie=%s, priority=500, in_port=%s, %s, %s_dst=%s, %s_src=%s,"+
						"actions=ct(commit,zone=%d,table=4)",
						nodetypes.DefaultOpenFlowCookie, netConfig.OfPortPatch, protoPrefixV4,
						protoPrefixV4, ip.String(), protoPrefixV4, physicalIP.IP,
						config.Default.HostMasqConntrackZone))
			}
		}

		// table 0, Reply SVC traffic from Host -> OVN, unSNAT and goto table 5
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=500, in_port=%s, %s, %s_dst=%s,"+
				"actions=ct(zone=%d,nat,table=5)",
				nodetypes.DefaultOpenFlowCookie, ofPortHost, protoPrefixV4, protoPrefixV4,
				config.Gateway.MasqueradeIPs.V4OVNMasqueradeIP.String(), config.Default.OVNMasqConntrackZone))

		for _, netConfig := range b.patchedNetConfigs() {
			if !isLocalGatewayNoOverlayUDN(netConfig) {
				continue
			}
			// table 0, reply traffic from host back to OVN for no-overlay UDN
			// service hairpins. Table 4 SNATs the forward direction to this
			// UDN's gateway router masquerade IP; use the OVN masquerade CT
			// zone to unSNAT replies and then continue reply service handling
			// in table 5.
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=500, in_port=%s, %s, %s_dst=%s,"+
					"actions=ct(zone=%d,nat,table=5)",
					nodetypes.DefaultOpenFlowCookie, ofPortHost, protoPrefixV4, protoPrefixV4,
					netConfig.V4MasqIPs.GatewayRouter.IP.String(), config.Default.OVNMasqConntrackZone))
		}
	}
	if config.IPv6Mode {
		if ofPortPhys != "" {
			// table0, Geneve packets coming from external. Skip conntrack and go directly to host
			// if dest mac is the shared mac send directly to host.
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=205, in_port=%s, dl_dst=%s, udp6, udp_dst=%d, "+
					"actions=output:%s", nodetypes.DefaultOpenFlowCookie, ofPortPhys, bridgeMacAddress, config.Default.EncapPort,
					ofPortHost))
			// perform NORMAL action otherwise.
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=200, in_port=%s, udp6, udp_dst=%d, "+
					"actions=NORMAL", nodetypes.DefaultOpenFlowCookie, ofPortPhys, config.Default.EncapPort))

			// table0, Geneve packets coming from LOCAL. Skip conntrack and send to external
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=200, in_port=%s, udp6, udp_dst=%d, "+
					"actions=output:%s", nodetypes.DefaultOpenFlowCookie, nodetypes.OvsLocalPort, config.Default.EncapPort, ofPortPhys))
		}

		physicalIP, err := util.MatchFirstIPNetFamily(true, bridgeIPs)
		if err != nil {
			return nil, fmt.Errorf("unable to determine IPv6 physical IP of host: %v", err)
		}
		// table 0, SVC Hairpin from OVN destined to local host, DNAT to host, send to table 4
		for _, netConfig := range b.patchedNetConfigs() {
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=500, in_port=%s, %s, %s_dst=%s, %s_src=%s,"+
					"actions=ct(commit,zone=%d,nat(dst=%s),table=4)",
					nodetypes.DefaultOpenFlowCookie, netConfig.OfPortPatch, protoPrefixV6, protoPrefixV6,
					config.Gateway.MasqueradeIPs.V6HostMasqueradeIP.String(), protoPrefixV6, physicalIP.IP,
					config.Default.HostMasqConntrackZone, physicalIP.IP))
		}

		// table 0, hairpin from OVN destined to local host (but an additional node IP), send to table 4
		for _, ip := range extraIPs {
			if ip.To4() != nil {
				continue
			}
			// not needed for the physical IP
			if ip.Equal(physicalIP.IP) {
				continue
			}

			// not needed for special masquerade IP
			if ip.Equal(config.Gateway.MasqueradeIPs.V6HostMasqueradeIP) {
				continue
			}

			for _, netConfig := range b.patchedNetConfigs() {
				dftFlows = append(dftFlows,
					fmt.Sprintf("cookie=%s, priority=500, in_port=%s, %s, %s_dst=%s, %s_src=%s,"+
						"actions=ct(commit,zone=%d,table=4)",
						nodetypes.DefaultOpenFlowCookie, netConfig.OfPortPatch, protoPrefixV6, protoPrefixV6,
						ip.String(), protoPrefixV6, physicalIP.IP,
						config.Default.HostMasqConntrackZone))
			}
		}

		// table 0, Reply SVC traffic from Host -> OVN, unSNAT and goto table 5
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=500, in_port=%s, %s, %s_dst=%s,"+
				"actions=ct(zone=%d,nat,table=5)",
				nodetypes.DefaultOpenFlowCookie, ofPortHost, protoPrefixV6, protoPrefixV6,
				config.Gateway.MasqueradeIPs.V6OVNMasqueradeIP.String(), config.Default.OVNMasqConntrackZone))

		for _, netConfig := range b.patchedNetConfigs() {
			if !isLocalGatewayNoOverlayUDN(netConfig) {
				continue
			}
			// table 0, reply traffic from host back to OVN for no-overlay UDN
			// service hairpins. Table 4 SNATs the forward direction to this
			// UDN's gateway router masquerade IP; use the OVN masquerade CT
			// zone to unSNAT replies and then continue reply service handling
			// in table 5.
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=500, in_port=%s, %s, %s_dst=%s,"+
					"actions=ct(zone=%d,nat,table=5)",
					nodetypes.DefaultOpenFlowCookie, ofPortHost, protoPrefixV6, protoPrefixV6,
					netConfig.V6MasqIPs.GatewayRouter.IP.String(), config.Default.OVNMasqConntrackZone))
		}
	}

	var protoPrefix, masqIP, masqSubnet string

	// table 0, packets coming from Host -> Service
	for _, svcCIDR := range config.Kubernetes.ServiceCIDRs {
		if utilnet.IsIPv4CIDR(svcCIDR) {
			protoPrefix = protoPrefixV4
			masqIP = config.Gateway.MasqueradeIPs.V4HostMasqueradeIP.String()
			masqSubnet = config.Gateway.V4MasqueradeSubnet
		} else {
			protoPrefix = protoPrefixV6
			masqIP = config.Gateway.MasqueradeIPs.V6HostMasqueradeIP.String()
			masqSubnet = config.Gateway.V6MasqueradeSubnet
		}

		// table 0, Host (default network) -> OVN towards SVC, SNAT to special IP.
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=500, in_port=%s, %s, %s_dst=%s, "+
				"actions=ct(commit,zone=%d,nat(src=%s),table=2)",
				nodetypes.DefaultOpenFlowCookie, ofPortHost, protoPrefix, protoPrefix,
				svcCIDR, config.Default.HostMasqConntrackZone, masqIP))

		if util.IsNetworkSegmentationSupportEnabled() {
			// table 0, Host (UDNs) -> OVN towards SVC, SNAT to special IP.
			// For packets originating from UDN, commit without NATing, those
			// have already been SNATed to the masq IP of the UDN.
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=550, in_port=%s, %s, %s_src=%s, %s_dst=%s, "+
					"actions=ct(commit,zone=%d,table=2)",
					nodetypes.DefaultOpenFlowCookie, ofPortHost, protoPrefix, protoPrefix,
					masqSubnet, protoPrefix, svcCIDR, config.Default.HostMasqConntrackZone))
			if util.IsRouteAdvertisementsEnabled() {
				// If the UDN is advertised then instead of matching on the masqSubnet
				// we match on the UDNPodSubnet itself and we also don't SNAT to 169.254.0.2
				// sample flow: cookie=0xdeff105, duration=1472.742s, table=0, n_packets=9, n_bytes=666, priority=550
				//              ip,in_port=LOCAL,nw_src=103.103.0.0/16,nw_dst=10.96.0.0/16 actions=ct(commit,table=2,zone=64001)
				for _, netConfig := range b.patchedNetConfigs() {
					if netConfig.IsDefaultNetwork() {
						continue
					}
					if netConfig.Advertised.Load() {
						var udnAdvertisedSubnets []*net.IPNet
						for _, clusterEntry := range netConfig.Subnets {
							udnAdvertisedSubnets = append(udnAdvertisedSubnets, clusterEntry.CIDR)
						}
						matchingIPFamilySubnets := matchIPNetFamily(utilnet.IsIPv6CIDR(svcCIDR), udnAdvertisedSubnets)
						if len(matchingIPFamilySubnets) == 0 {
							klog.Infof("Unable to determine UDN subnet for the provided family isIPV6: %t", utilnet.IsIPv6CIDR(svcCIDR))
							continue
						}

						for _, matchingIPFamilySubnet := range matchingIPFamilySubnets {
							// Use the filtered subnet for the flow compute instead of the masqueradeIP
							dftFlows = append(dftFlows,
								fmt.Sprintf("cookie=%s, priority=550, in_port=%s, %s, %s_src=%s, %s_dst=%s, "+
									"actions=ct(commit,zone=%d,table=2)",
									nodetypes.DefaultOpenFlowCookie, ofPortHost, protoPrefix, protoPrefix,
									matchingIPFamilySubnet.String(), protoPrefix, svcCIDR, config.Default.HostMasqConntrackZone))
						}
					}
				}
			}
		}

		masqDst := masqIP
		if util.IsNetworkSegmentationSupportEnabled() {
			// In UDN match on the whole masquerade subnet to handle replies from UDN enabled services
			masqDst = masqSubnet
		}
		for _, netConfig := range b.patchedNetConfigs() {
			// table 0, Reply hairpin traffic to host, coming from OVN, unSNAT
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=500, in_port=%s, %s, %s_src=%s, %s_dst=%s,"+
					"actions=ct(zone=%d,nat,table=3)",
					nodetypes.DefaultOpenFlowCookie, netConfig.OfPortPatch, protoPrefix, protoPrefix, svcCIDR,
					protoPrefix, masqDst, config.Default.HostMasqConntrackZone))
			// table 0, Reply traffic coming from OVN to outside, drop it if the DNAT wasn't done either
			// at the GR load balancer or switch load balancer. It means the correct port wasn't provided.
			// nodeCIDR->serviceCIDR traffic flow is internal and it shouldn't be carried to outside the cluster
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=115, in_port=%s, %s, %s_dst=%s,"+
					"actions=drop", nodetypes.DefaultOpenFlowCookie, netConfig.OfPortPatch, protoPrefix, protoPrefix, svcCIDR))
		}
	}

	// table 0, add IP fragment reassembly flows, only needed in SGW mode with
	// physical interface attached to bridge
	if config.Gateway.Mode == config.GatewayModeShared && ofPortPhys != "" {
		reassemblyFlows := generateIPFragmentReassemblyFlow(ofPortPhys)
		dftFlows = append(dftFlows, reassemblyFlows...)
	}
	if ofPortPhys != "" {
		for _, netConfig := range b.patchedNetConfigs() {
			var actions string
			if config.Gateway.Mode != config.GatewayModeLocal || config.Gateway.DisablePacketMTUCheck {
				actions = fmt.Sprintf("output:%s", netConfig.OfPortPatch)
			} else {
				// packets larger than known acceptable MTU need to go to kernel for
				// potential fragmentation
				// introduced specifically for replies to egress traffic not routed
				// through the host
				actions = fmt.Sprintf("check_pkt_larger(%d)->reg0[0],resubmit(,11)", maxPktLength)
			}

			if config.IPv4Mode {
				// table 1, established and related connections in zone 64000 with ct_mark CtMarkOVN go to OVN
				dftFlows = append(dftFlows,
					fmt.Sprintf("cookie=%s, priority=100, table=1, %s, ct_state=+trk+est, ct_mark=%s, "+
						"actions=%s", nodetypes.DefaultOpenFlowCookie, protoPrefixV4, netConfig.MasqCTMark, actions))

				dftFlows = append(dftFlows,
					fmt.Sprintf("cookie=%s, priority=100, table=1, %s, ct_state=+trk+rel, ct_mark=%s, "+
						"actions=%s", nodetypes.DefaultOpenFlowCookie, protoPrefixV4, netConfig.MasqCTMark, actions))

			}

			if config.IPv6Mode {
				// table 1, established and related connections in zone 64000 with ct_mark CtMarkOVN go to OVN
				dftFlows = append(dftFlows,
					fmt.Sprintf("cookie=%s, priority=100, table=1, %s, ct_state=+trk+est, ct_mark=%s, "+
						"actions=%s", nodetypes.DefaultOpenFlowCookie, protoPrefixV6, netConfig.MasqCTMark, actions))

				dftFlows = append(dftFlows,
					fmt.Sprintf("cookie=%s, priority=100, table=1, %s, ct_state=+trk+rel, ct_mark=%s, "+
						"actions=%s", nodetypes.DefaultOpenFlowCookie, protoPrefixV6, netConfig.MasqCTMark, actions))
			}
		}
		if config.IPv4Mode {
			// table 1, established and related connections in zone 64000 with ct_mark CtMarkHost go to host
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=100, table=1, %s %s, ct_state=+trk+est, ct_mark=%s, "+
					"actions=%soutput:%s",
					nodetypes.DefaultOpenFlowCookie, match_vlan, protoPrefixV4, nodetypes.CtMarkHost, strip_vlan, ofPortHost))

			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=100, table=1, %s %s, ct_state=+trk+rel, ct_mark=%s, "+
					"actions=%soutput:%s",
					nodetypes.DefaultOpenFlowCookie, match_vlan, protoPrefixV4, nodetypes.CtMarkHost, strip_vlan, ofPortHost))

		}
		if config.IPv6Mode {
			// table 1, established and related connections in zone 64000 with ct_mark CtMarkHost go to host
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=100, table=1, %s %s, ct_state=+trk+est, ct_mark=%s, "+
					"actions=%soutput:%s",
					nodetypes.DefaultOpenFlowCookie, match_vlan, protoPrefixV6, nodetypes.CtMarkHost, strip_vlan, ofPortHost))

			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=100, table=1, %s %s, ct_state=+trk+rel, ct_mark=%s, "+
					"actions=%soutput:%s",
					nodetypes.DefaultOpenFlowCookie, match_vlan, protoPrefixV6, nodetypes.CtMarkHost, strip_vlan, ofPortHost))

		}

		// table 1, we check to see if this dest mac is the shared mac, if so send to host
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=10, table=1, %s dl_dst=%s, actions=%soutput:%s",
				nodetypes.DefaultOpenFlowCookie, match_vlan, bridgeMacAddress, strip_vlan, ofPortHost))
	}

	defaultNetConfig := b.netConfig[types.DefaultNetworkName]

	// table 2, dispatch from Host -> OVN
	dftFlows = append(dftFlows,
		fmt.Sprintf("cookie=%s, priority=100, table=2, "+
			"actions=set_field:%s->eth_dst,%soutput:%s", nodetypes.DefaultOpenFlowCookie,
			bridgeMacAddress, mod_vlan_id, defaultNetConfig.OfPortPatch))

	// table 2, priority 200, dispatch from UDN -> Host -> OVN. These packets have
	// already been SNATed to the UDN's masquerade IP or have been marked with the UDN's packet mark.
	if config.IPv4Mode {
		for _, netConfig := range b.patchedNetConfigs() {
			if netConfig.IsDefaultNetwork() {
				continue
			}
			if util.IsRouteAdvertisementsEnabled() && netConfig.Advertised.Load() {
				var udnAdvertisedSubnets []*net.IPNet
				for _, clusterEntry := range netConfig.Subnets {
					udnAdvertisedSubnets = append(udnAdvertisedSubnets, clusterEntry.CIDR)
				}
				matchingIPFamilySubnets := matchIPNetFamily(false, udnAdvertisedSubnets)
				if len(matchingIPFamilySubnets) == 0 {
					klog.Infof("Unable to determine IPV4 UDN subnet")
					continue
				}
				// In addition to the masqueradeIP based flows, we also need the podsubnet based flows for
				// advertised networks since UDN pod to clusterIP is unSNATed and we need this traffic to be taken into
				// the correct patch port of it's own network where it's a deadend if the clusterIP is not part of
				// that UDN network and works if it is part of the UDN network.
				for _, matchingIPFamilySubnet := range matchingIPFamilySubnets {
					dftFlows = append(dftFlows,
						fmt.Sprintf("cookie=%s, priority=200, table=2, %s, %s_src=%s, "+
							"actions=drop",
							nodetypes.DefaultOpenFlowCookie, protoPrefixV4, protoPrefixV4, matchingIPFamilySubnet.String()))
				}
			}
			// Drop traffic coming from the masquerade IP or the UDN subnet(for advertised UDNs) to ensure that
			// isolation between networks is enforced. This handles the case where a pod on the UDN subnet is sending traffic to
			// a service in another UDN.
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=200, table=2, %s, %s_src=%s, "+
					"actions=drop",
					nodetypes.DefaultOpenFlowCookie, protoPrefixV4, protoPrefixV4,
					netConfig.V4MasqIPs.ManagementPort.IP.String()))

			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=250, table=2, %s, pkt_mark=%s, "+
					"actions=set_field:%s->eth_dst,%soutput:%s",
					nodetypes.DefaultOpenFlowCookie, protoPrefixV4, netConfig.PktMark,
					bridgeMacAddress, mod_vlan_id, netConfig.OfPortPatch))
		}
	}

	if config.IPv6Mode {
		for _, netConfig := range b.patchedNetConfigs() {
			if netConfig.IsDefaultNetwork() {
				continue
			}
			if util.IsRouteAdvertisementsEnabled() && netConfig.Advertised.Load() {
				var udnAdvertisedSubnets []*net.IPNet
				for _, clusterEntry := range netConfig.Subnets {
					udnAdvertisedSubnets = append(udnAdvertisedSubnets, clusterEntry.CIDR)
				}
				matchingIPFamilySubnets := matchIPNetFamily(true, udnAdvertisedSubnets)
				if len(matchingIPFamilySubnets) == 0 {
					klog.Infof("Unable to determine IPV6 UDN subnet")
					continue
				}

				for _, matchingIPFamilySubnet := range matchingIPFamilySubnets {
					dftFlows = append(dftFlows,
						fmt.Sprintf("cookie=%s, priority=200, table=2, %s, %s_src=%s, "+
							"actions=drop",
							nodetypes.DefaultOpenFlowCookie, protoPrefixV6, protoPrefixV6,
							matchingIPFamilySubnet.String()))
				}
			}
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=200, table=2, %s, %s_src=%s, "+
					"actions=drop",
					nodetypes.DefaultOpenFlowCookie, protoPrefixV6, protoPrefixV6,
					netConfig.V6MasqIPs.ManagementPort.IP.String()))
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=250, table=2, %s, pkt_mark=%s, "+
					"actions=set_field:%s->eth_dst,%soutput:%s",
					nodetypes.DefaultOpenFlowCookie, protoPrefixV6, netConfig.PktMark,
					bridgeMacAddress, mod_vlan_id, netConfig.OfPortPatch))
		}
	}

	// table 3, dispatch from OVN -> Host
	dftFlows = append(dftFlows,
		fmt.Sprintf("cookie=%s, table=3, %s "+
			"actions=move:NXM_OF_ETH_DST[]->NXM_OF_ETH_SRC[],set_field:%s->eth_dst,%soutput:%s",
			nodetypes.DefaultOpenFlowCookie, match_vlan, bridgeMacAddress, strip_vlan, ofPortHost))

	// table 4, hairpinned pkts that need to go from OVN -> Host
	// We need to SNAT and masquerade OVN GR IP, send to table 3 for dispatch to Host
	if config.IPv4Mode {
		for _, netConfig := range b.patchedNetConfigs() {
			if !isLocalGatewayNoOverlayUDN(netConfig) {
				continue
			}
			// table 4, forward traffic for no-overlay UDN service hairpins
			// from OVN to host. The UDN GR already SNATed to the node IP due to
			// lb_force_snat_ip=router_ip; SNAT again to this UDN's gateway
			// router masquerade IP so replies can be steered back to the same
			// UDN. Match UDN pod CIDRs so ordinary UDN traffic to node IPs, such
			// as NodePort requests on a different node, keeps the default table
			// 4 behavior.
			var udnSubnets []*net.IPNet
			for _, clusterEntry := range netConfig.Subnets {
				udnSubnets = append(udnSubnets, clusterEntry.CIDR)
			}
			for _, subnet := range matchIPNetFamily(false, udnSubnets) {
				dftFlows = append(dftFlows,
					fmt.Sprintf("cookie=%s, priority=100, table=4, in_port=%s, %s, %s_dst=%s, "+
						"actions=ct(commit,zone=%d,nat(src=%s),exec(set_field:%s->ct_mark),table=3)",
						nodetypes.DefaultOpenFlowCookie, netConfig.OfPortPatch, protoPrefixV4, protoPrefixV4,
						subnet.String(), config.Default.OVNMasqConntrackZone,
						netConfig.V4MasqIPs.GatewayRouter.IP.String(), netConfig.MasqCTMark))
			}
		}
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=0, table=4,%s,"+
				"actions=ct(commit,zone=%d,nat(src=%s),table=3)",
				nodetypes.DefaultOpenFlowCookie, protoPrefixV4, config.Default.OVNMasqConntrackZone, config.Gateway.MasqueradeIPs.V4OVNMasqueradeIP.String()))
	}
	if config.IPv6Mode {
		for _, netConfig := range b.patchedNetConfigs() {
			if !isLocalGatewayNoOverlayUDN(netConfig) {
				continue
			}
			// table 4, forward traffic for no-overlay UDN service hairpins
			// from OVN to host. The UDN GR already SNATed to the node IP due to
			// lb_force_snat_ip=router_ip; SNAT again to this UDN's gateway
			// router masquerade IP so replies can be steered back to the same
			// UDN. Match UDN pod CIDRs so ordinary UDN traffic to node IPs, such
			// as NodePort requests on a different node, keeps the default table
			// 4 behavior.
			var udnSubnets []*net.IPNet
			for _, clusterEntry := range netConfig.Subnets {
				udnSubnets = append(udnSubnets, clusterEntry.CIDR)
			}
			for _, subnet := range matchIPNetFamily(true, udnSubnets) {
				dftFlows = append(dftFlows,
					fmt.Sprintf("cookie=%s, priority=100, table=4, in_port=%s, %s, %s_dst=%s, "+
						"actions=ct(commit,zone=%d,nat(src=%s),exec(set_field:%s->ct_mark),table=3)",
						nodetypes.DefaultOpenFlowCookie, netConfig.OfPortPatch, protoPrefixV6, protoPrefixV6,
						subnet.String(), config.Default.OVNMasqConntrackZone,
						netConfig.V6MasqIPs.GatewayRouter.IP.String(), netConfig.MasqCTMark))
			}
		}
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=0, table=4,%s, "+
				"actions=ct(commit,zone=%d,nat(src=%s),table=3)",
				nodetypes.DefaultOpenFlowCookie, protoPrefixV6, config.Default.OVNMasqConntrackZone, config.Gateway.MasqueradeIPs.V6OVNMasqueradeIP.String()))
	}
	// table 5, Host Reply traffic to hairpinned svc, need to unDNAT, send to table 2
	if config.IPv4Mode {
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=0, table=5, %s, "+
				"actions=ct(commit,zone=%d,nat,table=2)",
				nodetypes.DefaultOpenFlowCookie, protoPrefixV4, config.Default.HostMasqConntrackZone))
		for _, netConfig := range b.patchedNetConfigs() {
			if !isLocalGatewayNoOverlayUDN(netConfig) {
				continue
			}
			// table 5, finish reply service unNAT for no-overlay UDN service
			// hairpins and dispatch directly to the UDN patch port. Sending
			// these replies through table 2 would hit the advertised-UDN source
			// isolation drops after unNAT.
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=100, table=5, %s, ct_mark=%s, "+
					"actions=ct(commit,zone=%d,nat),"+
					"set_field:%s->eth_dst,%soutput:%s",
					nodetypes.DefaultOpenFlowCookie, protoPrefixV4, netConfig.MasqCTMark,
					config.Default.HostMasqConntrackZone, bridgeMacAddress, mod_vlan_id, netConfig.OfPortPatch))
		}
	}
	if config.IPv6Mode {
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=0, table=5, %s, "+
				"actions=ct(commit,zone=%d,nat,table=2)",
				nodetypes.DefaultOpenFlowCookie, protoPrefixV6, config.Default.HostMasqConntrackZone))
		for _, netConfig := range b.patchedNetConfigs() {
			if !isLocalGatewayNoOverlayUDN(netConfig) {
				continue
			}
			// table 5, finish reply service unNAT for no-overlay UDN service
			// hairpins and dispatch directly to the UDN patch port. Sending
			// these replies through table 2 would hit the advertised-UDN source
			// isolation drops after unNAT.
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=100, table=5, %s, ct_mark=%s, "+
					"actions=ct(commit,zone=%d,nat),"+
					"set_field:%s->eth_dst,%soutput:%s",
					nodetypes.DefaultOpenFlowCookie, protoPrefixV6, netConfig.MasqCTMark,
					config.Default.HostMasqConntrackZone, bridgeMacAddress, mod_vlan_id, netConfig.OfPortPatch))
		}
	}
	return dftFlows, nil
}

// isLocalGatewayNoOverlayUDN filters UDN-specific service hairpin flows that
// SNAT to the UDN gateway router masquerade IP. Today those flows are only
// needed for local gateway no-overlay UDNs, where service traffic can hairpin
// through breth0 on the underlay. If UDNs later support underlay-routed
// service backends in Geneve mode, such as host-networked pods, this predicate
// should be updated to include those paths.
func isLocalGatewayNoOverlayUDN(netConfig *BridgeUDNConfiguration) bool {
	return !netConfig.IsDefaultNetwork() &&
		netConfig.Transport == types.NetworkTransportNoOverlay &&
		config.Gateway.Mode == config.GatewayModeLocal
}

// uplinkHostServiceFlows must be called with bridge.mutex held
func (b *BridgeConfiguration) uplinkHostServiceFlows() []string {
	if !util.IsNetworkSegmentationSupportEnabled() {
		return nil
	}

	bridgeMacAddress := b.macAddress.String()
	ofPortHost := b.ofPortHost

	stripVLAN := ""
	matchVLAN := ""
	modVLANID := ""
	if config.Gateway.VLANID != 0 {
		stripVLAN = "strip_vlan,"
		matchVLAN = fmt.Sprintf("dl_vlan=%d,", config.Gateway.VLANID)
		modVLANID = fmt.Sprintf("mod_vlan_vid:%d,", config.Gateway.VLANID)
	}

	var flows []string
	for _, svcCIDR := range config.Kubernetes.ServiceCIDRs {
		protoPrefix := protoPrefixV4
		masqIP := config.Gateway.MasqueradeIPs.V4HostMasqueradeIP.String()
		masqDst := config.Gateway.V4MasqueradeSubnet
		if !utilnet.IsIPv4CIDR(svcCIDR) {
			protoPrefix = protoPrefixV6
			masqIP = config.Gateway.MasqueradeIPs.V6HostMasqueradeIP.String()
			masqDst = config.Gateway.V6MasqueradeSubnet
		}

		for _, netConfig := range b.patchedNetConfigs() {
			if netConfig.IsDefaultNetwork() || netConfig.PktMark == "" {
				continue
			}

			// NodePort traffic that enters the host on another interface is
			// DNATed to a service ClusterIP by Linux, marked by nftables, and
			// routed to this Uplink bridge. Keep it in the host service CT path
			// so the reply returns through LOCAL and Linux can reverse the
			// NodePort DNAT.
			flows = append(flows,
				fmt.Sprintf("cookie=%s, priority=500, in_port=%s, %s, pkt_mark=%s, %s_dst=%s, "+
					"actions=ct(commit,zone=%d,nat(src=%s),table=2)",
					nodetypes.DefaultOpenFlowCookie, ofPortHost, protoPrefix, netConfig.PktMark,
					protoPrefix, svcCIDR, config.Default.HostMasqConntrackZone, masqIP))
			flows = append(flows,
				fmt.Sprintf("cookie=%s, priority=500, in_port=%s, %s, %s_src=%s, %s_dst=%s,"+
					"actions=ct(zone=%d,nat,table=3)",
					nodetypes.DefaultOpenFlowCookie, netConfig.OfPortPatch, protoPrefix,
					protoPrefix, svcCIDR, protoPrefix, masqDst,
					config.Default.HostMasqConntrackZone))
			flows = append(flows,
				fmt.Sprintf("cookie=%s, priority=115, in_port=%s, %s, %s_dst=%s,"+
					"actions=drop", nodetypes.DefaultOpenFlowCookie,
					netConfig.OfPortPatch, protoPrefix, protoPrefix, svcCIDR))
		}
	}

	if config.IPv4Mode {
		for _, netConfig := range b.patchedNetConfigs() {
			if netConfig.IsDefaultNetwork() || netConfig.PktMark == "" {
				continue
			}
			flows = append(flows,
				fmt.Sprintf("cookie=%s, priority=250, table=2, %s, pkt_mark=%s, "+
					"actions=set_field:%s->eth_dst,%soutput:%s",
					nodetypes.DefaultOpenFlowCookie, protoPrefixV4, netConfig.PktMark,
					bridgeMacAddress, modVLANID, netConfig.OfPortPatch))
		}
	}
	if config.IPv6Mode {
		for _, netConfig := range b.patchedNetConfigs() {
			if netConfig.IsDefaultNetwork() || netConfig.PktMark == "" {
				continue
			}
			flows = append(flows,
				fmt.Sprintf("cookie=%s, priority=250, table=2, %s, pkt_mark=%s, "+
					"actions=set_field:%s->eth_dst,%soutput:%s",
					nodetypes.DefaultOpenFlowCookie, protoPrefixV6, netConfig.PktMark,
					bridgeMacAddress, modVLANID, netConfig.OfPortPatch))
		}
	}

	flows = append(flows,
		fmt.Sprintf("cookie=%s, table=3, %s "+
			"actions=move:NXM_OF_ETH_DST[]->NXM_OF_ETH_SRC[],set_field:%s->eth_dst,%soutput:%s",
			nodetypes.DefaultOpenFlowCookie, matchVLAN, bridgeMacAddress, stripVLAN, ofPortHost))

	return flows
}

// getMaxFrameLength returns the maximum frame size (ignoring VLAN header) that a gateway can handle
func getMaxFrameLength() int {
	return config.Default.MTU + 14
}

// generateIPFragmentReassemblyFlow adds flows in table 0 that send packets to a
// specific conntrack zone for reassembly with the same priority as node port
// flows that match on L4 fields. After reassembly packets are reinjected to
// table 0 again. This requires a conntrack immplementation that reassembles
// fragments. This reqreuiment is met for the kernel datapath with the netfilter
// module loaded. This reqreuiment is not met for the userspace datapath.
func generateIPFragmentReassemblyFlow(ofPortPhys string) []string {
	flows := make([]string, 0, 2)
	if config.IPv4Mode {
		flows = append(flows,
			fmt.Sprintf("cookie=%s, priority=110, table=0, in_port=%s, %s, nw_frag=yes, actions=ct(table=0,zone=%d)",
				nodetypes.DefaultOpenFlowCookie,
				ofPortPhys,
				protoPrefixV4,
				config.Default.ReassemblyConntrackZone,
			),
		)
	}
	if config.IPv6Mode {
		flows = append(flows,
			fmt.Sprintf("cookie=%s, priority=110, table=0, in_port=%s, %s, nw_frag=yes, actions=ct(table=0,zone=%d)",
				nodetypes.DefaultOpenFlowCookie,
				ofPortPhys,
				protoPrefixV6,
				config.Default.ReassemblyConntrackZone,
			),
		)
	}

	return flows
}

// generateGratuitousARPDropFlow returns a single flow to drop GARPs
// Remove when https://issues.redhat.com/browse/FDP-1537 available
func generateGratuitousARPDropFlow(inPort string, priority int) string {
	// set to op code 1 - see rfc5227 particularly section:
	// Why Are ARP Announcements Performed Using ARP Request Packets and Not ARP Reply Packets?
	// ovn follows this practise of using op code 1
	return fmt.Sprintf("cookie=%s,table=0,priority=%d,in_port=%s,dl_dst=ff:ff:ff:ff:ff:ff,arp,arp_op=1,actions=drop",
		nodetypes.GARPCookie, priority, inPort)
}

// generateGratuitousARPAllowFlow returns a single flow to allow GARP only for a specific source IP.
// Remove when https://issues.redhat.com/browse/FDP-1537 available
func generateGratuitousARPAllowFlow(inPort string, ip net.IP, priority int) string {
	// set to op code 1 - see rfc5227 particularly section:
	// Why Are ARP Announcements Performed Using ARP Request Packets and Not ARP Reply Packets?
	// ovn follows this practise of using op code 1
	return fmt.Sprintf("cookie=%s,table=0,priority=%d,in_port=%s,dl_dst=ff:ff:ff:ff:ff:ff,arp,arp_op=1,arp_spa=%s,actions=output:NORMAL",
		nodetypes.GARPCookie, priority, inPort, ip)
}

// must be called with bridge.mutex held
func (b *BridgeConfiguration) commonFlows(hostSubnets []*net.IPNet, sourceNetwork string) ([]string, error) {
	// CAUTION: when adding new flows where the in_port is ofPortPatch and the out_port is ofPortPhys, ensure
	// that dl_src is included in match criteria!
	ofPortPhys := b.ofPortPhys
	bridgeMacAddress := b.macAddress.String()
	ofPortHost := b.ofPortHost
	bridgeIPs := b.ips
	// Localnet patch ports feed the host-network NORMAL flows (default network
	// only) and, when the ARP/NDP fan-out is disabled, the external-ingress set
	// of the priority 52 steering flows.
	var localnetPatchPorts []string
	_, hasDefaultNetwork := b.netConfig[types.DefaultNetworkName]
	needsSteering := util.IsNetworkSegmentationSupportEnabled() && config.Gateway.DisableUDNARPNDPFlood
	if (hasDefaultNetwork || needsSteering) &&
		(config.Gateway.Mode == config.GatewayModeShared || config.Gateway.Mode == config.GatewayModeLocal) {
		var err error
		localnetPatchPorts, err = b.localnetPatchPortNames()
		if err != nil {
			return nil, err
		}
	}
	hasLocalnetPatchPort := len(localnetPatchPorts) > 0

	var dftFlows []string

	stripVLAN := ""
	matchVLAN := ""
	modVLANID := ""
	if config.Gateway.VLANID != 0 {
		// When VLANID is specified, the gateway interface (i.e. LOCAL port) is considered an untagged
		// access port in the VLAN. The physical port on the bridge is a trunk, carrying tagged VLAN packets,
		// and the patch port to OVN is a tagged access port, where OVN expects to receive packets with the VLANID
		// tag.
		stripVLAN = "strip_vlan,"
		matchVLAN = fmt.Sprintf("dl_vlan=%d,", config.Gateway.VLANID)
		modVLANID = fmt.Sprintf("mod_vlan_vid:%d,", config.Gateway.VLANID)
	}

	if ofPortPhys != "" {
		// table 0, we check to see if this dest mac is the shared mac, if so flood to all ports.
		// When DisableUDNARPNDPFlood is set this fan-out is not rendered at all: the ARP/NDP it
		// used to carry is steered to the source GR by the priority 52 flows below, and the
		// remaining IP traffic is picked up by the priority 50 conntrack flow.
		if !config.Gateway.DisableUDNARPNDPFlood {
			// Process default delivery before primary UDN fan-out can exhaust OVS's
			// per-translation resubmit budget. OVS contains errors from a patch's peer
			// bridge, preserving earlier datapath actions when later UDN branches
			// exhaust the budget.
			var actions []string
			if defaultNetConfig := b.netConfig[types.DefaultNetworkName]; defaultNetConfig != nil && defaultNetConfig.OfPortPatch != "" {
				actions = append(actions, "output:"+defaultNetConfig.OfPortPatch)
			}
			actions = append(actions, b.primaryUDNPatchOutputActions()...)
			actions = append(actions, "NORMAL")
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=10, table=0, %s dl_dst=%s, actions=%s",
					nodetypes.DefaultOpenFlowCookie, matchVLAN, bridgeMacAddress, strings.Join(actions, ",")))
		}

		if util.IsNetworkSegmentationSupportEnabled() {
			dftFlows = append(dftFlows, b.arpFanoutFilterFlows(ofPortPhys, matchVLAN, sourceNetwork, localnetPatchPorts)...)
		}
	}

	// table 0, check packets coming from OVN have the correct mac address. Low priority flows that are a catch all
	// for non-IP packets that would normally be forwarded with NORMAL action (table 0, priority 0 flow).
	for _, netConfig := range b.patchedNetConfigs() {
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=10, table=0, in_port=%s, dl_src=%s, actions=output:NORMAL",
				nodetypes.DefaultOpenFlowCookie, netConfig.OfPortPatch, bridgeMacAddress))
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=9, table=0, in_port=%s, actions=drop",
				nodetypes.DefaultOpenFlowCookie, netConfig.OfPortPatch))
	}

	if config.IPv4Mode {
		physicalIP, err := util.MatchFirstIPNetFamily(false, bridgeIPs)
		if err != nil {
			return nil, fmt.Errorf("unable to determine IPv4 physical IP of host: %v", err)
		}
		if ofPortPhys != "" {
			for _, netConfig := range b.patchedNetConfigs() {
				// table0, for SGW UDN no-overlay networks, handle cross-node traffic
				// that has already been SNATed to node IP by the gateway router.
				if !netConfig.IsDefaultNetwork() && netConfig.Transport == types.NetworkTransportNoOverlay && config.Gateway.Mode == config.GatewayModeShared {
					// Commit to conntrack and send directly to the physical port.
					// SNAT to the node IP may already have happened on the UDN GW
					// router because lb_force_snat_ip=router_ip. SNAT to the same
					// node IP again here so zone 64000 can resolve source-port collisions.
					dftFlows = append(dftFlows,
						fmt.Sprintf("cookie=%s, priority=99, in_port=%s, dl_src=%s, %s, nw_src=%s, "+
							"actions=ct(commit, zone=%d, nat(src=%s), exec(set_field:%s->ct_mark)), output:%s",
							nodetypes.DefaultOpenFlowCookie, netConfig.OfPortPatch, bridgeMacAddress, protoPrefixV4,
							physicalIP.IP, config.Default.ConntrackZone,
							physicalIP.IP,
							netConfig.MasqCTMark, ofPortPhys))
				}

				// table0, packets coming from egressIP pods that have mark 1008 on them
				// will be SNAT-ed a final time into nodeIP to maintain consistency in traffic even if the GR
				// SNATs these into egressIP prior to reaching external bridge.
				// egressService pods will also undergo this SNAT to nodeIP since these features are tied
				// together at the OVN policy level on the distributed router.
				dftFlows = append(dftFlows,
					fmt.Sprintf("cookie=%s, priority=105, in_port=%s, dl_src=%s, %s, pkt_mark=%s "+
						"actions=ct(commit, zone=%d, nat(src=%s), exec(set_field:%s->ct_mark)),output:%s",
						nodetypes.DefaultOpenFlowCookie, netConfig.OfPortPatch, bridgeMacAddress, protoPrefixV4,
						nodetypes.OvnKubeNodeSNATMark, config.Default.ConntrackZone, physicalIP.IP, netConfig.MasqCTMark, ofPortPhys))

				// table 0, packets coming from egressIP pods only from user defined networks. If an egressIP is assigned to
				// this node, then all networks get a flow even if no pods on that network were selected for by this egressIP.
				if util.IsNetworkSegmentationSupportEnabled() &&
					config.Gateway.Mode != config.GatewayModeDisabled && b.eipMarkIPs != nil {
					if netConfig.MasqCTMark != nodetypes.CtMarkOVN {
						for mark, eip := range b.eipMarkIPs.GetIPv4() {
							dftFlows = append(dftFlows,
								fmt.Sprintf("cookie=%s, priority=105, in_port=%s, dl_src=%s, %s, pkt_mark=%d, "+
									"actions=ct(commit, zone=%d, nat(src=%s), exec(set_field:%s->ct_mark)), output:%s",
									nodetypes.DefaultOpenFlowCookie, netConfig.OfPortPatch, bridgeMacAddress, protoPrefixV4, mark,
									config.Default.ConntrackZone, eip, netConfig.MasqCTMark, ofPortPhys))
						}
					}
				}

				// table 0, packets coming from pods headed externally. Commit connections with ct_mark CtMarkOVN
				// so that reverse direction goes back to the pods.
				if netConfig.IsDefaultNetwork() {
					dftFlows = append(dftFlows,
						fmt.Sprintf("cookie=%s, priority=100, in_port=%s, dl_src=%s, %s, "+
							"actions=ct(commit, zone=%d, exec(set_field:%s->ct_mark)), output:%s",
							nodetypes.DefaultOpenFlowCookie, netConfig.OfPortPatch, bridgeMacAddress, protoPrefixV4,
							config.Default.ConntrackZone, netConfig.MasqCTMark, ofPortPhys))

					// Allow OVN and host traffic to reach a localnet endpoint on this bridge.
					if hasLocalnetPatchPort {
						dftFlows = append(dftFlows, hostNetworkNormalActionFlows(netConfig, bridgeMacAddress, hostSubnets, false)...)
					}
				} else {
					//  for UDN we additionally SNAT the packet from masquerade IP -> node IP
					dftFlows = append(dftFlows,
						fmt.Sprintf("cookie=%s, priority=100, in_port=%s, dl_src=%s, %s, %s_src=%s, "+
							"actions=ct(commit, zone=%d, nat(src=%s), exec(set_field:%s->ct_mark)), output:%s",
							nodetypes.DefaultOpenFlowCookie, netConfig.OfPortPatch, bridgeMacAddress, protoPrefixV4, protoPrefixV4,
							netConfig.V4MasqIPs.GatewayRouter.IP, config.Default.ConntrackZone,
							physicalIP.IP, netConfig.MasqCTMark, ofPortPhys))
				}
			}

			// table 0, packets coming from host Commit connections with ct_mark CtMarkHost
			// so that reverse direction goes back to the host.
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=100, in_port=%s, %s, "+
					"actions=ct(commit, zone=%d, exec(set_field:%s->ct_mark)), %soutput:%s",
					nodetypes.DefaultOpenFlowCookie, ofPortHost, protoPrefixV4, config.Default.ConntrackZone,
					nodetypes.CtMarkHost, modVLANID, ofPortPhys))
		}
		if config.Gateway.Mode == config.GatewayModeLocal {
			for _, netConfig := range b.patchedNetConfigs() {
				// table 0, any packet coming from OVN send to host in LGW mode, host will take care of sending it outside if needed.
				// exceptions are traffic for egressIP and egressGW features and ICMP related traffic which will hit the priority 100 flow instead of this.
				dftFlows = append(dftFlows,
					fmt.Sprintf("cookie=%s, priority=175, in_port=%s, tcp, nw_src=%s, "+
						"actions=ct(table=4,zone=%d)",
						nodetypes.DefaultOpenFlowCookie, netConfig.OfPortPatch, physicalIP.IP, config.Default.HostMasqConntrackZone))
				dftFlows = append(dftFlows,
					fmt.Sprintf("cookie=%s, priority=175, in_port=%s, udp, nw_src=%s, "+
						"actions=ct(table=4,zone=%d)",
						nodetypes.DefaultOpenFlowCookie, netConfig.OfPortPatch, physicalIP.IP, config.Default.HostMasqConntrackZone))
				dftFlows = append(dftFlows,
					fmt.Sprintf("cookie=%s, priority=175, in_port=%s, sctp, nw_src=%s, "+
						"actions=ct(table=4,zone=%d)",
						nodetypes.DefaultOpenFlowCookie, netConfig.OfPortPatch, physicalIP.IP, config.Default.HostMasqConntrackZone))
				// We send BFD traffic coming from OVN to outside directly using a higher priority flow
				if ofPortPhys != "" {
					dftFlows = append(dftFlows,
						fmt.Sprintf("cookie=%s, priority=650, table=0, in_port=%s, dl_src=%s, udp, tp_dst=3784, actions=output:%s",
							nodetypes.DefaultOpenFlowCookie, netConfig.OfPortPatch, bridgeMacAddress, ofPortPhys))
				}
			}
		}

		if ofPortPhys != "" {
			// table 0, packets coming from external or other localnet ports and destined to OVN or LOCAL.
			// Send it through conntrack and resubmit to table 1 to know the state and mark of the connection.
			// Note, there are higher priority rules that take care of traffic coming from LOCAL and OVN ports.
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=50, %s, dl_dst=%s, actions=ct(zone=%d, nat, table=1)",
					nodetypes.DefaultOpenFlowCookie, protoPrefixV4, bridgeMacAddress, config.Default.ConntrackZone))
		}
	}

	if config.IPv6Mode {
		physicalIP, err := util.MatchFirstIPNetFamily(true, bridgeIPs)
		if err != nil {
			return nil, fmt.Errorf("unable to determine IPv6 physical IP of host: %v", err)
		}
		if ofPortPhys != "" {
			for _, netConfig := range b.patchedNetConfigs() {
				// table0, for SGW UDN no-overlay networks, handle cross-node traffic
				// that has already been SNATed to node IP by the gateway router.
				if !netConfig.IsDefaultNetwork() && netConfig.Transport == types.NetworkTransportNoOverlay && config.Gateway.Mode == config.GatewayModeShared {
					// Commit to conntrack and send directly to the physical port.
					// SNAT to the node IP may already have happened on the UDN GW
					// router because lb_force_snat_ip=router_ip. SNAT to the same
					// node IP again here so zone 64000 can resolve source-port collisions.
					dftFlows = append(dftFlows,
						fmt.Sprintf("cookie=%s, priority=99, in_port=%s, dl_src=%s, %s, %s_src=%s, "+
							"actions=ct(commit, zone=%d, nat(src=%s), exec(set_field:%s->ct_mark)), output:%s",
							nodetypes.DefaultOpenFlowCookie, netConfig.OfPortPatch, bridgeMacAddress, protoPrefixV6,
							protoPrefixV6, physicalIP.IP, config.Default.ConntrackZone,
							physicalIP.IP,
							netConfig.MasqCTMark, ofPortPhys))
				}

				// table0, packets coming from egressIP pods that have mark 1008 on them
				// will be DNAT-ed a final time into nodeIP to maintain consistency in traffic even if the GR
				// DNATs these into egressIP prior to reaching external bridge.
				// egressService pods will also undergo this SNAT to nodeIP since these features are tied
				// together at the OVN policy level on the distributed router.
				dftFlows = append(dftFlows,
					fmt.Sprintf("cookie=%s, priority=105, in_port=%s, dl_src=%s, %s, pkt_mark=%s "+
						"actions=ct(commit, zone=%d, nat(src=%s), exec(set_field:%s->ct_mark)),output:%s",
						nodetypes.DefaultOpenFlowCookie, netConfig.OfPortPatch, bridgeMacAddress, protoPrefixV6, nodetypes.OvnKubeNodeSNATMark,
						config.Default.ConntrackZone, physicalIP.IP, netConfig.MasqCTMark, ofPortPhys))

				// table 0, packets coming from egressIP pods only from user defined networks. If an egressIP is assigned to
				// this node, then all networks get a flow even if no pods on that network were selected for by this egressIP.
				if util.IsNetworkSegmentationSupportEnabled() &&
					config.Gateway.Mode != config.GatewayModeDisabled && b.eipMarkIPs != nil {
					if netConfig.MasqCTMark != nodetypes.CtMarkOVN {
						for mark, eip := range b.eipMarkIPs.GetIPv6() {
							dftFlows = append(dftFlows,
								fmt.Sprintf("cookie=%s, priority=105, in_port=%s, dl_src=%s, %s, pkt_mark=%d, "+
									"actions=ct(commit, zone=%d, nat(src=%s), exec(set_field:%s->ct_mark)), output:%s",
									nodetypes.DefaultOpenFlowCookie, netConfig.OfPortPatch, bridgeMacAddress, protoPrefixV6, mark,
									config.Default.ConntrackZone, eip, netConfig.MasqCTMark, ofPortPhys))
						}
					}
				}

				// table 0, packets coming from pods headed externally. Commit connections with ct_mark CtMarkOVN
				// so that reverse direction goes back to the pods.
				if netConfig.IsDefaultNetwork() {
					dftFlows = append(dftFlows,
						fmt.Sprintf("cookie=%s, priority=100, in_port=%s, dl_src=%s, %s, "+
							"actions=ct(commit, zone=%d, exec(set_field:%s->ct_mark)), output:%s",
							nodetypes.DefaultOpenFlowCookie, netConfig.OfPortPatch, bridgeMacAddress, protoPrefixV6,
							config.Default.ConntrackZone, netConfig.MasqCTMark, ofPortPhys))

					// Allow OVN and host traffic to reach a localnet endpoint on this bridge.
					if hasLocalnetPatchPort {
						dftFlows = append(dftFlows, hostNetworkNormalActionFlows(netConfig, bridgeMacAddress, hostSubnets, true)...)
					}
				} else {
					//  for UDN we additionally SNAT the packet from masquerade IP -> node IP
					dftFlows = append(dftFlows,
						fmt.Sprintf("cookie=%s, priority=100, in_port=%s, dl_src=%s, %s, %s_src=%s, "+
							"actions=ct(commit, zone=%d, nat(src=%s), exec(set_field:%s->ct_mark)), output:%s",
							nodetypes.DefaultOpenFlowCookie, netConfig.OfPortPatch, bridgeMacAddress, protoPrefixV6, protoPrefixV6,
							netConfig.V6MasqIPs.GatewayRouter.IP, config.Default.ConntrackZone,
							physicalIP.IP, netConfig.MasqCTMark, ofPortPhys))
				}
			}

			// table 0, packets coming from host. Commit connections with ct_mark CtMarkHost
			// so that reverse direction goes back to the host.
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=100, in_port=%s, %s, "+
					"actions=ct(commit, zone=%d, exec(set_field:%s->ct_mark)), %soutput:%s",
					nodetypes.DefaultOpenFlowCookie, ofPortHost, protoPrefixV6,
					config.Default.ConntrackZone, nodetypes.CtMarkHost, modVLANID, ofPortPhys))

		}
		if config.Gateway.Mode == config.GatewayModeLocal {
			for _, netConfig := range b.patchedNetConfigs() {
				// table 0, any packet coming from OVN send to host in LGW mode, host will take care of sending it outside if needed.
				// exceptions are traffic for egressIP and egressGW features and ICMP related traffic which will hit the priority 100 flow instead of this.
				dftFlows = append(dftFlows,
					fmt.Sprintf("cookie=%s, priority=175, in_port=%s, tcp6, %s_src=%s, "+
						"actions=ct(table=4,zone=%d)",
						nodetypes.DefaultOpenFlowCookie, netConfig.OfPortPatch, protoPrefixV6, physicalIP.IP, config.Default.HostMasqConntrackZone))
				dftFlows = append(dftFlows,
					fmt.Sprintf("cookie=%s, priority=175, in_port=%s, udp6, %s_src=%s, "+
						"actions=ct(table=4,zone=%d)",
						nodetypes.DefaultOpenFlowCookie, netConfig.OfPortPatch, protoPrefixV6, physicalIP.IP, config.Default.HostMasqConntrackZone))
				dftFlows = append(dftFlows,
					fmt.Sprintf("cookie=%s, priority=175, in_port=%s, sctp6, %s_src=%s, "+
						"actions=ct(table=4,zone=%d)",
						nodetypes.DefaultOpenFlowCookie, netConfig.OfPortPatch, protoPrefixV6, physicalIP.IP, config.Default.HostMasqConntrackZone))
				if ofPortPhys != "" {
					// We send BFD traffic coming from OVN to outside directly using a higher priority flow
					dftFlows = append(dftFlows,
						fmt.Sprintf("cookie=%s, priority=650, table=0, in_port=%s, dl_src=%s, udp6, tp_dst=3784, actions=output:%s",
							nodetypes.DefaultOpenFlowCookie, netConfig.OfPortPatch, bridgeMacAddress, ofPortPhys))
				}
			}
		}
		if ofPortPhys != "" {
			// table 0, packets coming from external. Send it through conntrack and
			// resubmit to table 1 to know the state and mark of the connection.
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=50, %s, dl_dst=%s, actions=ct(zone=%d, nat, table=1)",
					nodetypes.DefaultOpenFlowCookie, protoPrefixV6, bridgeMacAddress, config.Default.ConntrackZone))
		}
	}
	if ofPortPhys != "" {
		defaultNetConfig, hasDefaultNetConfig := b.netConfig[types.DefaultNetworkName]
		// table 0, Ingress/Egress flows for MEG enabled pods and advertised UDNs
		// priority 300: Ingress traffic to MEG pods and advertised UDNs. The node management port IP is part of <nodeSubnet>
		// and is handled by this flow as well, so it retraces the gateway router instead of being short-circuited to the physical
		// bridge LOCAL port.
		// priority 104: Egress traffic from advertised UDNs or MEG enabled pods
		// priority 103: For egressIP, drop packets coming from pods from other nodes headed externally that were not SNATed.
		// example flows in SGW mode EIP enabled:
		//   table=0, n_packets=0, n_bytes=0, priority=300,ip,in_port=eth0,nw_dst=<nodeSubnet> actions=output:4
		//   table=0, n_packets=0, n_bytes=0, priority=104,ip,in_port=4,dl_src=02:42:ac:12:00:03,nw_src=<nodeSubnet> actions=output:eth0
		//   table=0, n_packets=0, n_bytes=0, priority=103,ip,in_port=4,nw_src=<clusterSubnet> actions=drop
		// example flows in LGW mode EIP enabled:
		//   table=0, n_packets=0, n_bytes=0, priority=300,ip,in_port=eth0,nw_dst=<nodeSubnet> actions=output:LOCAL
		//   table=0, n_packets=0, n_bytes=0, priority=104,ip,in_port=LOCAL,dl_src=02:42:ac:12:00:03,nw_src=<nodeSubnet> actions=output:eth0
		//   table=0, n_packets=0, n_bytes=0, priority=103,ip,in_port=4,nw_src=<clusterSubnet> actions=drop
		// example flows in SGW mode EIP disabled:
		//   table=0, n_packets=0, n_bytes=0, priority=300,ip,in_port=eth0,nw_dst=<nodeSubnet> actions=output:4
		//   table=0, n_packets=0, n_bytes=0, priority=104,ip,in_port=4,dl_src=02:42:ac:12:00:03,nw_src=<nodeSubnet> actions=output:eth0
		// example flows in LGW mode EIP disabled:
		//   table=0, n_packets=0, n_bytes=0, priority=300,ip,in_port=eth0,nw_dst=<nodeSubnet> actions=output:LOCAL
		//   table=0, n_packets=0, n_bytes=0, priority=104,ip,in_port=LOCAL,dl_src=02:42:ac:12:00:03,nw_src=<nodeSubnet> actions=output:eth0
		for _, netConfig := range b.patchedNetConfigs() {
			isNetworkAdvertised := netConfig.Advertised.Load()
			// disableSNATMultipleGWs only applies to default network
			disableSNATMultipleGWs := netConfig.IsDefaultNetwork() && config.Gateway.DisableSNATMultipleGWs

			if config.OVNKubernetesFeature.EnableEgressIP {
				// Due to the fact that ovn-controllers on different nodes apply the changes independently,
				// there is a chance that the pod traffic will reach the egress node before it configures the SNAT flows.
				// Drop pod traffic that is not SNATed
				for _, clusterEntry := range netConfig.Subnets {
					cidr := clusterEntry.CIDR
					ipv := getIPv(cidr)
					// table 0, drop packets coming from pods headed externally that were not SNATed.
					dftFlows = append(dftFlows,
						fmt.Sprintf("cookie=%s, priority=103, in_port=%s, %s, %s_src=%s, actions=drop",
							nodetypes.DefaultOpenFlowCookie, netConfig.OfPortPatch, ipv, ipv, cidr))
				}
			}
			// skip if MEG is disabled for the default network
			// and the network (default or UDN) is not advertised
			if !disableSNATMultipleGWs && !isNetworkAdvertised {
				continue
			}
			output := netConfig.OfPortPatch
			input := netConfig.OfPortPatch
			isAdvertisedLGW := isNetworkAdvertised && config.Gateway.Mode == config.GatewayModeLocal
			if isAdvertisedLGW {
				// except if advertised through BGP, go to kernel
				// TODO: MEG enabled pods should still go through the patch port
				// but holding this until
				// https://issues.redhat.com/browse/FDP-646 is fixed, for now we
				// are assuming MEG & BGP are not used together
				output = nodetypes.OvsLocalPort
				input = nodetypes.OvsLocalPort
			}
			for _, subnet := range netConfig.NodeSubnets {
				ipv := getIPv(subnet)
				dftFlows = append(dftFlows,
					fmt.Sprintf("cookie=%s, priority=300, table=0, in_port=%s, %s, %s_dst=%s, "+
						"actions=output:%s",
						nodetypes.DefaultOpenFlowCookie, ofPortPhys, ipv, ipv, subnet, output))

				if disableSNATMultipleGWs || isNetworkAdvertised {
					// MEG and advertised UDN networks requires that local pod traffic can leave the node without SNAT.
					// We match on the pod subnets and forward the traffic to the physical interface.
					// Select priority 104 for the scenario when both EgressIP and advertised UDN are active:
					// 1. Override egressIP drop flows (priority 103)
					// 2. Still allow egressIP flows at priority 105
					dftFlows = append(dftFlows,
						fmt.Sprintf("cookie=%s, priority=104, in_port=%s, dl_src=%s, %s, %s_src=%s, "+
							"actions=output:%s",
							nodetypes.DefaultOpenFlowCookie, input, bridgeMacAddress, ipv, ipv, subnet, ofPortPhys))
				}
			}
		}

		// table 1, we check to see if this dest mac is the shared mac, if so send to host
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=10, table=1, %s dl_dst=%s, actions=%soutput:%s",
				nodetypes.DefaultOpenFlowCookie, matchVLAN, bridgeMacAddress, stripVLAN, ofPortHost))

		// These BFD mirror flows target the default network patch port. Uplink
		// bridges only carry their selected UDNs, so skip this when the bridge
		// has no default network configuration.
		if config.IPv6Mode {
			// REMOVEME(trozet) when https://bugzilla.kernel.org/show_bug.cgi?id=11797 is resolved
			// must flood icmpv6 Route Advertisement and Neighbor Advertisement traffic as it fails to create a CT entry.
			// FLOOD reaches default and host first. UDN patches are no-flood,
			// so append explicit outputs after the protected delivery.
			// When DisableUDNARPNDPFlood is set these are not rendered: unicast RAs/NAs never reach
			// conntrack because the priority 52 flows steer them to the source GR first, and
			// multicast ones fall to the table 0 catch-all NORMAL.
			if !config.Gateway.DisableUDNARPNDPFlood {
				actions := strings.Join(append([]string{"FLOOD"}, b.primaryUDNPatchOutputActions()...), ",")
				for _, icmpType := range []int{types.RouteAdvertisementICMPType, types.NeighborAdvertisementICMPType} {
					dftFlows = append(dftFlows,
						fmt.Sprintf("cookie=%s, priority=14, table=1,icmp6,icmpv6_type=%d actions=%s",
							nodetypes.DefaultOpenFlowCookie, icmpType, actions))
				}
			}
			if hasDefaultNetConfig {
				// We send BFD traffic both on the host and in ovn
				dftFlows = append(dftFlows,
					fmt.Sprintf("cookie=%s, priority=13, table=1, in_port=%s, udp6, tp_dst=3784, actions=output:%s,output:%s",
						nodetypes.DefaultOpenFlowCookie, ofPortPhys, defaultNetConfig.OfPortPatch, ofPortHost))
			}
		}

		if config.IPv4Mode {
			if hasDefaultNetConfig {
				// We send BFD traffic both on the host and in ovn
				dftFlows = append(dftFlows,
					fmt.Sprintf("cookie=%s, priority=13, table=1, in_port=%s, udp, tp_dst=3784, actions=output:%s,output:%s",
						nodetypes.DefaultOpenFlowCookie, ofPortPhys, defaultNetConfig.OfPortPatch, ofPortHost))
			}
		}

		if !hasDefaultNetConfig {
			for _, netConfig := range b.patchedNetConfigs() {
				if config.IPv4Mode {
					dftFlows = append(dftFlows,
						fmt.Sprintf("cookie=%s, priority=100, table=1, %s, ct_state=+trk+est, ct_mark=%s, "+
							"actions=output:%s",
							nodetypes.DefaultOpenFlowCookie, protoPrefixV4, netConfig.MasqCTMark, netConfig.OfPortPatch))
					dftFlows = append(dftFlows,
						fmt.Sprintf("cookie=%s, priority=100, table=1, %s, ct_state=+trk+rel, ct_mark=%s, "+
							"actions=output:%s",
							nodetypes.DefaultOpenFlowCookie, protoPrefixV4, netConfig.MasqCTMark, netConfig.OfPortPatch))
				}
				if config.IPv6Mode {
					dftFlows = append(dftFlows,
						fmt.Sprintf("cookie=%s, priority=100, table=1, %s, ct_state=+trk+est, ct_mark=%s, "+
							"actions=output:%s",
							nodetypes.DefaultOpenFlowCookie, protoPrefixV6, netConfig.MasqCTMark, netConfig.OfPortPatch))
					dftFlows = append(dftFlows,
						fmt.Sprintf("cookie=%s, priority=100, table=1, %s, ct_state=+trk+rel, ct_mark=%s, "+
							"actions=output:%s",
							nodetypes.DefaultOpenFlowCookie, protoPrefixV6, netConfig.MasqCTMark, netConfig.OfPortPatch))
				}
			}
		}

		// packets larger than known acceptable MTU need to go to kernel for
		// potential fragmentation
		// introduced specifically for replies to egress traffic not routed
		// through the host
		if config.Gateway.Mode == config.GatewayModeLocal && !config.Gateway.DisablePacketMTUCheck && hasDefaultNetConfig {
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=10, table=11, reg0=0x1, "+
					"actions=output:%s", nodetypes.DefaultOpenFlowCookie, ofPortHost))

			// Send UDN destined traffic to right patch port
			for _, netConfig := range b.patchedNetConfigs() {
				if netConfig.MasqCTMark != nodetypes.CtMarkOVN {
					dftFlows = append(dftFlows,
						fmt.Sprintf("cookie=%s, priority=5, table=11, ct_mark=%s, "+
							"actions=output:%s", nodetypes.DefaultOpenFlowCookie, netConfig.MasqCTMark, netConfig.OfPortPatch))
				}
			}

			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=1, table=11, "+
					"actions=output:%s", nodetypes.DefaultOpenFlowCookie, defaultNetConfig.OfPortPatch))
		}

		// table 1, all other connections do normal processing
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=0, table=1, actions=output:NORMAL", nodetypes.DefaultOpenFlowCookie))
	}

	return dftFlows, nil
}

func (b *BridgeConfiguration) PMTUDDropFlows(ipAddrs []string) []string {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	var flows []string
	if config.Gateway.Mode != config.GatewayModeShared {
		return nil
	}
	for _, addr := range ipAddrs {
		for _, netConfig := range b.patchedNetConfigs() {
			flows = append(flows,
				nodeutil.GenerateICMPFragmentationFlow(addr, nodetypes.OutputPortDrop, netConfig.OfPortPatch, nodetypes.PmtudOpenFlowCookie, 700))
		}
	}

	return flows
}

// dropGARPFlows generates the ovs flows for dropping gratuitous ARPs for cluster default network traffic only.
// bridgeConfiguration lock must be held by caller
func (b *BridgeConfiguration) dropGARPFlows() []string {
	if config.Gateway.Mode != config.GatewayModeShared || !config.IPv4Mode {
		return nil
	}
	const priority = 498
	var flows []string

	defaultNetInfo := util.DefaultNetInfo{}
	defaultNetPatchPortName := defaultNetInfo.GetNetworkScopedPatchPortName(b.bridgeName, b.nodeName)

	for _, netConfig := range b.patchedNetConfigs() {
		if netConfig.PatchPort != defaultNetPatchPortName {
			continue
		}
		flows = append(flows, generateGratuitousARPDropFlow(netConfig.OfPortPatch, priority))
	}
	return flows
}

// allowNodeIPGARPFlows generates the OVS flows to allow gratuitous ARPs for Node IP(s) for the cluster default network traffic only.
// bridgeConfiguration lock must be held by caller.
// Remove when https://issues.redhat.com/browse/FDP-1537 is available
func (b *BridgeConfiguration) allowNodeIPGARPFlows(nodeIPs []net.IP) []string {
	if config.Gateway.Mode != config.GatewayModeShared || !config.IPv4Mode {
		return nil
	}
	const priority = 499
	var flows []string

	defaultNetInfo := util.DefaultNetInfo{}
	defaultNetPatchPortName := defaultNetInfo.GetNetworkScopedPatchPortName(b.bridgeName, b.nodeName)

	for _, netConfig := range b.patchedNetConfigs() {
		if netConfig.PatchPort != defaultNetPatchPortName {
			continue
		}
		for _, nodeIP := range nodeIPs {
			if nodeIP == nil || nodeIP.IsUnspecified() || utilnet.IsIPv6(nodeIP) {
				continue
			}
			flows = append(flows, generateGratuitousARPAllowFlow(netConfig.OfPortPatch, nodeIP, priority))
		}

	}
	return flows
}

// arpFanoutFilterFlows prevents ARP requests and IPv6 Neighbor Solicitations
// for local node IPs from reaching primary UDN GR patch ports.
//
// The generic priority-10 fan-out rule replicates all unicast traffic destined
// to the bridge MAC to every patch port. For broadcast ARP and IPv6 multicast
// NS, the priority-0 NORMAL action floods to all ports. In both cases UDN
// GRs receive the request and reply for the node IP. Since all UDN GRs share
// the same node IP on their external interface, each one generates a reply
// (ARP reply or NA). These duplicate replies flood the physical network and
// cause remote nodes to passively learn redundant MAC_Bindings, driving
// ovs-vswitchd CPU up.
//
// This function relies on UDN patch ports being configured with OVS no-flood,
// so that NORMAL's flood action never reaches them.
//
// Priority-12 flows intercept node-IP ARP/NDP and send only to the default
// patch + NORMAL. NORMAL preserves FDB learning while no-flood excludes UDNs.
//
// Priority-11 flows match external broadcast ARP (in_port=<phys>) and
// forward through NORMAL to default and host before explicitly outputting to
// UDN patches for MAC_Binding updates (e.g. gateway MAC changes via GARPs).
// Node-IP ARPs are caught at priority-12 first.
//
// Must be called with bridge.mutex held.
func (b *BridgeConfiguration) arpFanoutFilterFlows(ofPortPhys, matchVLAN, sourceNetwork string, localnetPatchPorts []string) []string {
	if config.Gateway.DisableUDNARPNDPFlood {
		return b.externalIngressARPNDPSteeringFlows(ofPortPhys, matchVLAN, sourceNetwork, localnetPatchPorts)
	}

	defaultNetConfig, found := b.netConfig[types.DefaultNetworkName]
	if !found || defaultNetConfig.OfPortPatch == "" {
		return nil
	}

	var flows []string

	// Priority-12: ARP/NS for node IP → only default patch + NORMAL.
	// NORMAL learns the external source MAC in the FDB and delivers to
	// LOCAL (static FDB entry). No-flood on UDN ports prevents them from
	// receiving the packet via NORMAL's flood.
	for _, ip := range b.ips {
		if ip.IP.To4() != nil {
			flows = append(flows,
				fmt.Sprintf("cookie=%s, priority=12, table=0, %s arp, arp_op=1, arp_tpa=%s, "+
					"actions=output:%s,NORMAL",
					nodetypes.DefaultOpenFlowCookie, matchVLAN, ip.IP,
					defaultNetConfig.OfPortPatch))
		} else {
			flows = append(flows,
				fmt.Sprintf("cookie=%s, priority=12, table=0, %s icmp6, icmpv6_type=%d, nd_target=%s, "+
					"actions=output:%s,NORMAL",
					nodetypes.DefaultOpenFlowCookie, matchVLAN, types.NeighborSolicitationICMPType, ip.IP,
					defaultNetConfig.OfPortPatch))
		}
	}

	// Priority-11: external broadcast ARP (including GARPs) arriving on the
	// physical port → NORMAL followed by explicit UDN outputs so UDNs can
	// update MAC_Bindings when an external entity (e.g. gateway router)
	// announces a new MAC. NORMAL handles FDB learning and delivery to
	// the default network/LOCAL before the UDN fan-out can exhaust the
	// resubmit budget.
	// Without the explicit outputs, no-flood would prevent UDNs from
	// ever seeing external MAC announcements.
	// The in_port match scopes to the physical port so that OVN-originated
	// traffic from patch ports (already handled at priority 10 and above)
	// is not affected.
	// TODO: if localnet ports (VMs) are added to breth0, their GARPs
	// won't reach UDNs (blocked by no-flood). If localnet-to-UDN
	// communication is needed in the future, these flows should be
	// extended to match additional in_ports.
	// Only generated when UDN patches exist (without them, NORMAL already
	// floods to the default patch which is not no-flood).
	udnActions := b.primaryUDNPatchOutputActions()
	if ofPortPhys != "" && len(udnActions) > 0 {
		actions := strings.Join(append([]string{"NORMAL"}, udnActions...), ",")
		if config.IPv4Mode {
			flows = append(flows,
				fmt.Sprintf("cookie=%s, priority=11, table=0, in_port=%s, %s dl_dst=ff:ff:ff:ff:ff:ff, arp, "+
					"actions=%s",
					nodetypes.DefaultOpenFlowCookie, ofPortPhys, matchVLAN, actions))
		}
		if config.IPv6Mode {
			// dl_dst=33:33:00:00:00:01 is the Ethernet multicast MAC for IPv6
			// all-nodes multicast (ff02::1). IPv6 multicast MACs are formed as
			// 33:33 + last 4 bytes of the IPv6 multicast address.
			// This restricts to unsolicited NAs (IPv6 GARP equivalent) only;
			// unicast solicited NAs are handled by the priority-10 fan-out.
			flows = append(flows,
				fmt.Sprintf("cookie=%s, priority=11, table=0, in_port=%s, %s dl_dst=33:33:00:00:00:01, icmp6, icmpv6_type=%d, "+
					"actions=%s",
					nodetypes.DefaultOpenFlowCookie, ofPortPhys, matchVLAN, types.NeighborAdvertisementICMPType, actions))
		}
	}

	return flows
}

// externalIngressARPNDPSteeringFlows returns the priority 52 flows that steer
// inbound unicast ARP and NDP addressed to the shared bridge MAC to the source
// GR's patch port instead of fanning it out to every GR on the bridge.
//
// These sit above the priority 50 conntrack flow so NDP never takes a conntrack
// path that cannot create CT entries for it (kernel bug #11797), and above the
// priority 10 fan-out and table 1 flows, both of which use explicit output
// actions that bypass no-flood. ARP is unified at the same priority to keep the
// pipeline simple.
//
// Broadcast and multicast ARP/NDP (GARP, node IP ARP requests, solicited-node
// multicast NS, unsolicited multicast NA, multicast RA) deliberately do not
// match: they fall to the table 0 catch-all NORMAL, whose flood already reaches
// the source GR and LOCAL while no-flood keeps CUDN patches out.
//
// The trailing NORMAL is load bearing. It performs FDB learning of the external
// source MAC and delivers to LOCAL via the static bridgeMAC->LOCAL entry; the
// kernel's neighbour table depends on it. The explicit output to the source
// patch is equally load bearing, since NORMAL's FDB lookup for a unicast frame
// addressed to the bridge MAC only ever resolves to LOCAL.
//
// The external-ingress set is the physical port plus every secondary localnet
// patch port on the bridge: traffic from a localnet network enters there rather
// than on ofPortPhys. A localnet port whose ofport is not assigned yet is
// skipped; the openflow manager re-renders on localnet port churn and on its
// periodic resync, so it is picked up then.
//
// Must be called with bridge.mutex held.
func (b *BridgeConfiguration) externalIngressARPNDPSteeringFlows(ofPortPhys, matchVLAN, sourceNetwork string, localnetPatchPorts []string) []string {
	sourceNetConfig, found := b.netConfig[sourceNetwork]
	if !found || sourceNetConfig.OfPortPatch == "" {
		return nil
	}

	ingressPorts := make([]string, 0, len(localnetPatchPorts)+1)
	if ofPortPhys != "" {
		ingressPorts = append(ingressPorts, ofPortPhys)
	}
	for _, name := range localnetPatchPorts {
		ofPort := b.ofPortOf(name)
		if ofPort == "" {
			klog.V(5).Infof("Skipping ARP/NDP steering for localnet port %s on bridge %s: no ofport assigned yet",
				name, b.bridgeName)
			continue
		}
		ingressPorts = append(ingressPorts, ofPort)
	}
	if len(ingressPorts) == 0 {
		return nil
	}

	bridgeMacAddress := b.macAddress.String()
	sourcePatch := sourceNetConfig.OfPortPatch

	var flows []string
	for _, ingressPort := range ingressPorts {
		if config.IPv4Mode {
			flows = append(flows,
				fmt.Sprintf("cookie=%s, priority=52, table=0, in_port=%s, %s dl_dst=%s, arp, "+
					"actions=output:%s,NORMAL",
					nodetypes.DefaultOpenFlowCookie, ingressPort, matchVLAN, bridgeMacAddress, sourcePatch))
		}
		if config.IPv6Mode {
			for _, icmpType := range []int{
				types.NeighborAdvertisementICMPType,
				types.RouteAdvertisementICMPType,
				types.NeighborSolicitationICMPType,
			} {
				flows = append(flows,
					fmt.Sprintf("cookie=%s, priority=52, table=0, in_port=%s, %s dl_dst=%s, icmp6, icmpv6_type=%d, "+
						"actions=output:%s,NORMAL",
						nodetypes.DefaultOpenFlowCookie, ingressPort, matchVLAN, bridgeMacAddress, icmpType, sourcePatch))
			}
		}
	}
	return flows
}

// primaryUDNPatchOutputActions returns output actions for ready primary UDN patch ports.
// Callers must arrange default delivery before these actions to protect it
// from UDN fan-out exhausting the resubmit budget.
// Must be called with bridge.mutex held.
func (b *BridgeConfiguration) primaryUDNPatchOutputActions() []string {
	defaultNetConfig := b.netConfig[types.DefaultNetworkName]
	var actions []string
	for _, netConfig := range b.patchedNetConfigs() {
		if defaultNetConfig != nil && netConfig.OfPortPatch == defaultNetConfig.OfPortPatch {
			continue
		}
		actions = append(actions, "output:"+netConfig.OfPortPatch)
	}
	return actions
}

func getIPv(ipnet *net.IPNet) string {
	prefix := protoPrefixV4
	if utilnet.IsIPv6CIDR(ipnet) {
		prefix = protoPrefixV6
	}
	return prefix
}

// hostNetworkNormalActionFlows returns the flows that allow IP{v4,v6} traffic:
// a. from pods in the OVN network to pods in a localnet network, on the same node
// b. from pods on the host to pods in a localnet network, on the same node
// when the localnet is mapped to breth0.
// The expected srcMAC is the MAC address of breth0 and the expected hostSubnets is the host subnets found on the node
// primary interface.
func hostNetworkNormalActionFlows(netConfig *BridgeUDNConfiguration, srcMAC string, hostSubnets []*net.IPNet, isV6 bool) []string {
	var flows []string
	var ipFamily, ipFamilyDest string

	if isV6 {
		ipFamily = protoPrefixV6
		ipFamilyDest = protoPrefixV6 + "_dst"
	} else {
		ipFamily = protoPrefixV4
		ipFamilyDest = "nw_dst"
	}

	formatFlow := func(inPort, destIP, ctMark string) string {
		// Matching IP traffic will be handled by the bridge instead of being output directly
		// to the NIC by the existing flow at prio=100.
		flowTemplate := "cookie=%s, priority=102, in_port=%s, dl_src=%s, %s, %s=%s, " +
			"actions=ct(commit, zone=%d, exec(set_field:%s->ct_mark)), output:NORMAL"
		return fmt.Sprintf(flowTemplate,
			nodetypes.DefaultOpenFlowCookie,
			inPort,
			srcMAC,
			ipFamily,
			ipFamilyDest,
			destIP,
			config.Default.ConntrackZone,
			ctMark)
	}

	// Traffic path (a): OVN->localnet for shared gw mode
	if config.Gateway.Mode == config.GatewayModeShared {
		for _, hostSubnet := range hostSubnets {
			if utilnet.IsIPv6(hostSubnet.IP) != isV6 {
				continue
			}
			flows = append(flows, formatFlow(netConfig.OfPortPatch, hostSubnet.String(), netConfig.MasqCTMark))
		}
	}

	// Traffic path (a): OVN->localnet for local gw mode
	// Traffic path (b): host->localnet for both gw modes
	for _, hostSubnet := range hostSubnets {
		if utilnet.IsIPv6(hostSubnet.IP) != isV6 {
			continue
		}
		flows = append(flows, formatFlow(nodetypes.OvsLocalPort, hostSubnet.String(), nodetypes.CtMarkHost))
	}

	if isV6 {
		// IPv6 neighbor discovery uses ICMPv6 messages sent to a special destination (ff02::1:ff00:0/104)
		// that is unrelated to the host subnets matched in the prio=102 flow above.
		// Allow neighbor discovery by matching against ICMP type and ingress port.
		formatICMPFlow := func(inPort, ctMark string, icmpType int) string {
			icmpFlowTemplate := "cookie=%s, priority=102, in_port=%s, dl_src=%s, icmp6, icmpv6_type=%d, " +
				"actions=ct(commit, zone=%d, exec(set_field:%s->ct_mark)), output:NORMAL"
			return fmt.Sprintf(icmpFlowTemplate,
				nodetypes.DefaultOpenFlowCookie,
				inPort,
				srcMAC,
				icmpType,
				config.Default.ConntrackZone,
				ctMark)
		}

		for _, icmpType := range []int{types.NeighborSolicitationICMPType, types.NeighborAdvertisementICMPType} {
			// Traffic path (a) for ICMP: OVN-> localnet for shared gw mode
			if config.Gateway.Mode == config.GatewayModeShared {
				flows = append(flows,
					formatICMPFlow(netConfig.OfPortPatch, netConfig.MasqCTMark, icmpType))
			}

			// Traffic path (a) for ICMP: OVN->localnet for local gw mode
			// Traffic path (b) for ICMP: host->localnet for both gw modes
			flows = append(flows, formatICMPFlow(nodetypes.OvsLocalPort, nodetypes.CtMarkHost, icmpType))
		}
	}
	return flows
}
