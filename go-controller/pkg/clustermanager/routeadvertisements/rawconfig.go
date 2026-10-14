// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package routeadvertisements

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// generateRawConfig generates raw FRR configuration. Main purpose is to
// generates EVPN config sections based on the provided selected networks
// IP-VRFs and MAC-VRFs. Also adds allowas-in origin to all neighbors, which is
// needed for eBGP peers sharing our ASN and is a no-op for iBGP.
//
// Inputs are built through the constructors in validation.go, so every value
// they carry is a single FRR token.
//
// Generated config structure:
//
//	router bgp <asn>                    <- genDefaultVRFSection
//	 address-family ipv4 unicast        <- genUnicastSection (only if default VRF has neighbors)
//	  neighbor <ip> allowas-in origin
//	 exit-address-family
//	 address-family ipv6 unicast
//	  neighbor <ip> allowas-in origin
//	 exit-address-family
//	 address-family l2vpn evpn          <- genDefaultVRFEVPNSection (only if networks have MAC-VRFs/IP-VRFs)
//	  neighbor <ip> activate
//	  neighbor <ip> allowas-in origin
//	  advertise-all-vni
//	  vni <id>                          <- genVRFVNISection (one per MAC-VRF with RT, section only added when MAC-VRF RT is set)
//	   route-target import <rt>
//	   route-target export <rt>
//	  exit-vni
//	 exit-address-family
//	exit
//	!
//	vrf <name>                          <- genVRFVNISection (one per IP-VRF)
//	 vni <id>
//	exit-vrf
//	!
//	router bgp <asn> vrf <name>         <- genNonDefaultVRFSection (one per non-default VRF)
//	 address-family ipv4 unicast        <- genUnicastSection (only if VRF has neighbors i.e. VRF-Lite)
//	  neighbor <ip> allowas-in origin
//	 exit-address-family
//	 address-family ipv6 unicast
//	  neighbor <ip> allowas-in origin
//	 exit-address-family
//	 address-family l2vpn evpn          <- genNonDefaultVRFEVPNSection (only if network has IP-VRF)
//	  advertise ipv4 unicast
//	  advertise ipv6 unicast
//	  route-target import <rt>
//	  route-target export <rt>
//	 exit-address-family
//	exit
//	!
func generateRawConfig(selected *selectedNetworks, vrfNeighbors map[string][]frrNeighbor, vrfASNs map[string]uint32) string {
	var buf strings.Builder

	// handle default VRF router, neighbors sorted for deterministic config
	// generation
	neighbors := slices.SortedFunc(slices.Values(vrfNeighbors[""]), compareNeighbors)
	buf.WriteString(genDefaultVRFSection(vrfASNs[""], neighbors, selected))

	// handle VRF<->VNI mappings
	ipVRFConfigMap := make(map[string]*ipVRFConfig, len(selected.ipVRFConfigs))
	for _, cfg := range selected.ipVRFConfigs {
		ipVRFConfigMap[cfg.vrfName] = cfg
		buf.WriteString(genVRFVNISection(cfg))
	}

	// handle non default VRFs
	for _, vrf := range slices.Sorted(maps.Keys(vrfASNs)) {
		if vrf == "" {
			continue
		}
		// neighbors sorted for deterministic config generation
		neighbors := slices.SortedFunc(slices.Values(vrfNeighbors[vrf]), compareNeighbors)
		buf.WriteString(genNonDefaultVRFSection(vrf, vrfASNs[vrf], neighbors, ipVRFConfigMap[vrf]))
	}

	return buf.String()
}

// compareNeighbors orders neighbors by identifier for deterministic config
// generation.
func compareNeighbors(a, b frrNeighbor) int {
	return strings.Compare(a.text, b.text)
}

// genVRFVNISection generates VRF-to-VNI mapping.
//
//	vrf <name>
//	 vni <id>
//	exit-vrf
//	!
func genVRFVNISection(cfg *ipVRFConfig) string {
	return fmt.Sprintf(`vrf %s
 vni %d
exit-vrf
!
`, cfg.vrfName, cfg.vni)
}

// genDefaultVRFSection generates the default VRF router's unicast and EVPN address-families.
//
//	router bgp <asn>
//	 ...
//	exit
//	!
func genDefaultVRFSection(asn uint32, neighbors []frrNeighbor, selected *selectedNetworks) string {
	if asn == 0 || len(neighbors) == 0 {
		return ""
	}

	var buf strings.Builder

	fmt.Fprintf(&buf, "router bgp %d\n", asn)

	neighbors4, neighbors6 := splitNeighborsByIPFamily(neighbors)
	buf.WriteString(genUnicastSection(neighbors4, neighbors6))
	buf.WriteString(genDefaultVRFEVPNSection(neighbors, selected))

	buf.WriteString("exit\n!\n")

	return buf.String()
}

// genNonDefaultVRFSection generates a non-default VRF router's unicast and EVPN address-families.
//
//	router bgp <asn> vrf red
//	 ...
//	exit
//	!
func genNonDefaultVRFSection(vrf string, asn uint32, neighbors []frrNeighbor, cfg *ipVRFConfig) string {
	if vrf == "" || asn == 0 {
		return ""
	}
	if len(neighbors) == 0 && cfg == nil {
		return ""
	}

	var buf strings.Builder

	fmt.Fprintf(&buf, "router bgp %d vrf %s\n", asn, vrf)

	neighbors4, neighbors6 := splitNeighborsByIPFamily(neighbors)
	buf.WriteString(genUnicastSection(neighbors4, neighbors6))
	buf.WriteString(genNonDefaultVRFEVPNSection(cfg))

	buf.WriteString("exit\n!\n")

	return buf.String()
}

// splitNeighborsByIPFamily splits neighbor identifiers by IP family. Unnumbered
// neighbors are identified by interface name rather than by address: FRR
// establishes their session over an IPv4 address derived from a /30 or /31 on
// the interface or, failing that, over the peer's IPv6 link-local address, and
// in either case the session can carry prefixes of both families (RFC 8950),
// so they are included in both results.
func splitNeighborsByIPFamily(neighbors []frrNeighbor) (neighbors4, neighbors6 []frrNeighbor) {
	for _, neighbor := range neighbors {
		switch {
		case neighbor.isIPv6():
			neighbors6 = append(neighbors6, neighbor)
		case neighbor.isIPv4():
			neighbors4 = append(neighbors4, neighbor)
		default:
			neighbors4 = append(neighbors4, neighbor)
			neighbors6 = append(neighbors6, neighbor)
		}
	}
	return
}

// genUnicastSection generates unicast address-family sections for IPv4 and/or IPv6 neighbors.
//
//	address-family ipv4 unicast
//	 neighbor <ip> allowas-in origin
//	exit-address-family
//	address-family ipv6 unicast
//	 neighbor <ip> allowas-in origin
//	exit-address-family
func genUnicastSection(neighbors4, neighbors6 []frrNeighbor) string {
	var buf strings.Builder

	if len(neighbors4) > 0 {
		buf.WriteString(" address-family ipv4 unicast\n")
		for _, neighbor := range neighbors4 {
			fmt.Fprintf(&buf, "  neighbor %s allowas-in origin\n", neighbor.text)
		}
		buf.WriteString(" exit-address-family\n")
	}
	if len(neighbors6) > 0 {
		buf.WriteString(" address-family ipv6 unicast\n")
		for _, neighbor := range neighbors6 {
			fmt.Fprintf(&buf, "  neighbor %s allowas-in origin\n", neighbor.text)
		}
		buf.WriteString(" exit-address-family\n")
	}
	return buf.String()
}

// genDefaultVRFEVPNSection generates the l2vpn evpn address-family for the default VRF.
//
//	address-family l2vpn evpn
//	 neighbor <ip> activate
//	 neighbor <ip> allowas-in origin
//	 advertise-all-vni
//	 vni <id>                           <- (Section only added when MAC-VRF RT is set)
//	  route-target import <rt>
//	  route-target export <rt>
//	 exit-vni
//	exit-address-family
func genDefaultVRFEVPNSection(neighbors []frrNeighbor, selected *selectedNetworks) string {
	hasEVPN := len(selected.ipVRFConfigs) > 0 || len(selected.macVRFConfigs) > 0
	if !hasEVPN {
		return ""
	}

	var buf strings.Builder

	buf.WriteString(" address-family l2vpn evpn\n")

	for _, neighbor := range neighbors {
		fmt.Fprintf(&buf, "  neighbor %s activate\n", neighbor.text)
		// Needed for eBGP peers sharing our ASN; no-op for iBGP. Applied
		// unconditionally because the peer type may be unknown (e.g.
		// `remote-as auto`, not yet supported by frr-k8s but anticipated).
		fmt.Fprintf(&buf, "  neighbor %s allowas-in origin\n", neighbor.text)
	}
	buf.WriteString("  advertise-all-vni\n")

	for _, cfg := range selected.macVRFConfigs {
		if cfg.routeTarget == "" {
			continue
		}
		fmt.Fprintf(&buf, "  vni %d\n", cfg.vni)
		fmt.Fprintf(&buf, "   route-target import %s\n", cfg.routeTarget)
		fmt.Fprintf(&buf, "   route-target export %s\n", cfg.routeTarget)
		buf.WriteString("  exit-vni\n")
	}

	buf.WriteString(" exit-address-family\n")

	return buf.String()
}

// genNonDefaultVRFEVPNSection generates the l2vpn evpn address-family for a non-default VRF.
//
//	address-family l2vpn evpn
//	 advertise ipv4 unicast
//	 advertise ipv6 unicast
//	 route-target import <rt>
//	 route-target export <rt>
//	exit-address-family
func genNonDefaultVRFEVPNSection(cfg *ipVRFConfig) string {
	if cfg == nil {
		return ""
	}

	var buf strings.Builder
	buf.WriteString(" address-family l2vpn evpn\n")

	if cfg.hasIPv4 {
		buf.WriteString("  advertise ipv4 unicast\n")
	}
	if cfg.hasIPv6 {
		buf.WriteString("  advertise ipv6 unicast\n")
	}
	if cfg.routeTarget != "" {
		fmt.Fprintf(&buf, "  route-target import %s\n", cfg.routeTarget)
		fmt.Fprintf(&buf, "  route-target export %s\n", cfg.routeTarget)
	}

	buf.WriteString(" exit-address-family\n")

	return buf.String()
}
