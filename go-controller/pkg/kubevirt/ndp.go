package kubevirt

import (
	"net"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	corev1 "k8s.io/api/core/v1"
)

func generateRouterAdvertisment(srcMAC, dstMAC net.HardwareAddr, srcIP, dstIP net.IP, lifetime uint16) ([]byte, error) {
	ethernetLayer := layers.Ethernet{
		DstMAC:       dstMAC,
		SrcMAC:       srcMAC,
		EthernetType: layers.EthernetTypeIPv6,
	}
	ip6Layer := layers.IPv6{
		Version:    6,
		NextHeader: layers.IPProtocolICMPv6,
		HopLimit:   255,
		SrcIP:      srcIP,
		DstIP:      dstIP,
	}
	icmp6Layer := layers.ICMPv6{
		TypeCode: layers.CreateICMPv6TypeCode(layers.ICMPv6TypeRouterAdvertisement, 0),
	}
	if err := icmp6Layer.SetNetworkLayerForChecksum(&ip6Layer); err != nil {
		return nil, err
	}
	managedAddressFlag := uint8(0x80)
	defaultRoutePreferenceFlag := uint8(0x08)
	raLayer := layers.ICMPv6RouterAdvertisement{
		HopLimit:       255,
		Flags:          managedAddressFlag | defaultRoutePreferenceFlag,
		RouterLifetime: lifetime,
		ReachableTime:  0,
		RetransTimer:   0,
		Options: layers.ICMPv6Options{{
			Type: layers.ICMPv6OptSourceAddress,
			Data: srcMAC,
		}},
	}
	serializeBuffer := gopacket.NewSerializeBuffer()
	if err := gopacket.SerializeLayers(serializeBuffer, gopacket.SerializeOptions{ComputeChecksums: true, FixLengths: true},
		&ethernetLayer,
		&ip6Layer,
		&icmp6Layer,
		&raLayer,
	); err != nil {
		return nil, err
	}
	return serializeBuffer.Bytes(), nil
}

func generateRouterAdvertismentForNode(node *corev1.Node, networkName string, raDstMAC net.HardwareAddr, raDstIP net.IP, lifetime uint16) ([]byte, error) {
	lrpAddress, err := util.ParseNodeGatewayRouterJoinNetwork(node, networkName)
	if err != nil {
		return nil, err
	}
	if lrpAddress.IPv6 == "" {
		return nil, nil
	}

	//TODO: Support ipv6 single stack (mac is generated with ipv6)
	// LRP's mac is calcualted from the join subnet IPv4
	lrpAddressIPv4, _, err := net.ParseCIDR(lrpAddress.IPv4)
	if err != nil {
		return nil, err
	}
	raSrcMAC := util.IPAddrToHWAddr(lrpAddressIPv4)
	raSrcIP := util.HWAddrToIPv6LLA(raSrcMAC)
	return generateRouterAdvertisment(raSrcMAC, raDstMAC, raSrcIP, raDstIP, lifetime)
}
