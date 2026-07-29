// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package util

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"math/big"
	"net"
	"slices"
	"strconv"
	"strings"

	iputils "github.com/containernetworking/plugins/pkg/ip"
	"github.com/vishvananda/netlink"

	utilnet "k8s.io/utils/net"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
)

var ErrorNoIP = errors.New("no IP available")

// GetNodeGatewayIfAddr returns the node logical switch gateway address
// (the ".1" address), return nil if the subnet is invalid
func GetNodeGatewayIfAddr(subnet *net.IPNet) *net.IPNet {
	if subnet == nil {
		return nil
	}
	ip := iputils.NextIP(subnet.IP)
	if ip == nil {
		return nil
	}
	return &net.IPNet{IP: ip, Mask: subnet.Mask}
}

// GetNodeManagementIfAddr returns the node logical switch management port address
// (the ".2" address), return nil if the subnet is invalid
func GetNodeManagementIfAddr(subnet *net.IPNet) *net.IPNet {
	gwIfAddr := GetNodeGatewayIfAddr(subnet)
	if gwIfAddr == nil {
		return nil
	}
	return &net.IPNet{IP: iputils.NextIP(gwIfAddr.IP), Mask: subnet.Mask}
}

// GetNodeHybridOverlayIfAddr returns the node logical switch hybrid overlay
// port address (the ".3" address), return nil if the subnet is invalid
func GetNodeHybridOverlayIfAddr(subnet *net.IPNet) *net.IPNet {
	mgmtIfAddr := GetNodeManagementIfAddr(subnet)
	if mgmtIfAddr == nil {
		return nil
	}
	return &net.IPNet{IP: iputils.NextIP(mgmtIfAddr.IP), Mask: subnet.Mask}
}

// IsNodeHybridOverlayIfAddr returns whether the provided IP is a node hybrid
// overlay address on any of the provided subnets
func IsNodeHybridOverlayIfAddr(ip net.IP, subnets []*net.IPNet) bool {
	for _, subnet := range subnets {
		if ip.Equal(GetNodeHybridOverlayIfAddr(subnet).IP) {
			return true
		}
	}
	return false
}

// JoinHostPortInt32 is like net.JoinHostPort(), but with an int32 for the port
func JoinHostPortInt32(host string, port int32) string {
	return net.JoinHostPort(host, strconv.Itoa(int(port)))
}

// SplitHostPortInt32 splits a vip into its host and port counterparts
func SplitHostPortInt32(vip string) (string, int32, error) {
	ip, portRaw, err := net.SplitHostPort(vip)
	if err != nil {
		return "", 0, err
	}
	port, err := strconv.ParseInt(portRaw, 10, 32)
	if err != nil {
		return "", 0, err
	}
	return ip, int32(port), nil
}

// IPAddrToHWAddr takes the four octets of IPv4 address (aa.bb.cc.dd, for example) and uses them in creating
// a MAC address (0A:58:AA:BB:CC:DD).  For IPv6, create a hash from the IPv6 string and use that for MAC Address.
// Assumption: the caller will ensure that an empty net.IP{} will NOT be passed.
func IPAddrToHWAddr(ip net.IP) net.HardwareAddr {
	// Ensure that for IPv4, we are always working with the IP in 4-byte form.
	ip4 := ip.To4()
	if ip4 != nil {
		// safe to use private MAC prefix: 0A:58
		return net.HardwareAddr{0x0A, 0x58, ip4[0], ip4[1], ip4[2], ip4[3]}
	}

	hash := sha256.Sum256([]byte(ip.String()))
	return net.HardwareAddr{0x0A, 0x58, hash[0], hash[1], hash[2], hash[3]}
}

// HWAddrToIPv6LLA generates the IPv6 link local address from the given hwaddr,
// with prefix 'fe80:/64'.
func HWAddrToIPv6LLA(hwaddr net.HardwareAddr) net.IP {
	return net.IP{
		0xfe,
		0x80,
		0x00,
		0x00,
		0x00,
		0x00,
		0x00,
		0x00,
		(hwaddr[0] ^ 0x02),
		hwaddr[1],
		hwaddr[2],
		0xff,
		0xfe,
		hwaddr[3],
		hwaddr[4],
		hwaddr[5],
	}
}

// JoinIPs joins the string forms of an array of net.IP, as with strings.Join
func JoinIPs(ips []net.IP, sep string) string {
	b := &strings.Builder{}
	for i, ip := range ips {
		if i != 0 {
			b.WriteString(sep)
		}
		b.WriteString(ip.String())
	}
	return b.String()
}

// JoinIPNets joins the string forms of an array of *net.IPNet, as with strings.Join
func JoinIPNets(ipnets []*net.IPNet, sep string) string {
	b := &strings.Builder{}
	for i, ipnet := range ipnets {
		if i != 0 {
			b.WriteString(sep)
		}
		b.WriteString(ipnet.String())
	}
	return b.String()
}

// JoinIPNetIPs joins the string forms of an array of *net.IPNet,
// as with strings.Join, but does not include the IP mask.
func JoinIPNetIPs(ipnets []*net.IPNet, sep string) string {
	b := &strings.Builder{}
	for i, ipnet := range ipnets {
		if i != 0 {
			b.WriteString(sep)
		}
		b.WriteString(ipnet.IP.String())
	}
	return b.String()
}

// IPFamilyName returns IP Family string based on input flag.
func IPFamilyName(isIPv6 bool) string {
	if isIPv6 {
		return "IPv6"
	} else {
		return "IPv4"
	}
}

// MatchIPFamily loops through the array of net.IP and returns a
// slice of addresses in the same IP Family, based on input flag isIPv6.
func MatchIPFamily(isIPv6 bool, ips []net.IP) ([]net.IP, error) {
	var ipAddrs []net.IP
	for _, ip := range ips {
		if utilnet.IsIPv6(ip) == isIPv6 {
			ipAddrs = append(ipAddrs, ip)
		}
	}
	if len(ipAddrs) > 0 {
		return ipAddrs, nil
	}
	return nil, fmt.Errorf("no %s IP available", IPFamilyName(isIPv6))
}

// MatchFirstIPFamily loops through the array of net.IP and returns the first
// entry in the list in the same IP Family, based on input flag isIPv6.
func MatchFirstIPFamily(isIPv6 bool, ips []net.IP) (net.IP, error) {
	for _, ip := range ips {
		if utilnet.IsIPv6(ip) == isIPv6 {
			return ip, nil
		}
	}
	return nil, fmt.Errorf("no %s IP available", IPFamilyName(isIPv6))
}

func SplitIPsByIPFamily(ips []net.IP) (ipsv4, ipsv6 []net.IP) {
	for _, ip := range ips {
		switch {
		case utilnet.IsIPv6(ip):
			ipsv6 = append(ipsv6, ip)
		default:
			ipsv4 = append(ipsv4, ip)
		}
	}
	return
}

// MatchFirstIPNetFamily loops through the array of ipnets and returns the
// first entry in the list in the same IP Family, based on input flag isIPv6.
func MatchFirstIPNetFamily(isIPv6 bool, ipnets []*net.IPNet) (*net.IPNet, error) {
	for _, ipnet := range ipnets {
		if utilnet.IsIPv6CIDR(ipnet) == isIPv6 {
			return ipnet, nil
		}
	}
	return nil, fmt.Errorf("no %s value available", IPFamilyName(isIPv6))
}

// MatchAllIPNetFamily loops through the array of *net.IPNet and returns a
// slice of ipnets with the same IP Family, based on input flag isIPv6.
func MatchAllIPNetFamily(isIPv6 bool, ipnets []*net.IPNet) []*net.IPNet {
	var ret []*net.IPNet
	for _, ipnet := range ipnets {
		if utilnet.IsIPv6CIDR(ipnet) == isIPv6 {
			ret = append(ret, ipnet)
		}
	}
	return ret
}

func SplitIPNetsByIPFamily(ipnets []*net.IPNet) (ipv4, ipv6 []*net.IPNet) {
	for _, ipnet := range ipnets {
		switch {
		case utilnet.IsIPv6CIDR(ipnet):
			ipv6 = append(ipv6, ipnet)
		default:
			ipv4 = append(ipv4, ipnet)
		}
	}
	return
}

// MatchIPStringFamily loops through the array of string and returns the
// first entry in the list in the same IP Family, based on input flag isIPv6.
func MatchIPStringFamily(isIPv6 bool, ipStrings []string) (string, error) {
	for _, ipString := range ipStrings {
		if utilnet.IsIPv6String(ipString) == isIPv6 {
			return ipString, nil
		}
	}
	return "", fmt.Errorf("no %s string available", IPFamilyName(isIPv6))
}

// MatchFirstCIDRStringFamily loops through the array of CIDR strings and returns
// the first entry in the same IP Family, based on input flag isIPv6.
func MatchFirstCIDRStringFamily[T ~string](isIPv6 bool, cidrs []T) T {
	for _, cidr := range cidrs {
		if utilnet.IsIPv6CIDRString(string(cidr)) == isIPv6 {
			return cidr
		}
	}
	var zero T
	return zero
}

// MatchAllIPStringFamily loops through the array of string and returns a slice
// of addresses in the same IP Family, based on input flag isIPv6.
func MatchAllIPStringFamily(isIPv6 bool, ipStrings []string) ([]string, error) {
	var ipAddrs []string
	for _, ipString := range ipStrings {
		if utilnet.IsIPv6String(ipString) == isIPv6 {
			ipAddrs = append(ipAddrs, ipString)
		}
	}
	if len(ipAddrs) > 0 {
		return ipAddrs, nil
	}
	return nil, ErrorNoIP
}

// MatchAllCIDRStringFamily loops through the array of string and returns a slice
// of addresses in the same IP Family, based on input flag isIPv6.
func MatchAllIPNetsStringFamily(isIPv6 bool, ipnets []string) []string {
	var out []string
	for _, ipnet := range ipnets {
		if utilnet.IsIPv6CIDRString(ipnet) == isIPv6 {
			out = append(out, ipnet)
		}
	}
	return out
}

// SplitIPStringByIPFamily splits a slice of IP address strings into IPv4 and IPv6 slices.
func SplitIPStringByIPFamily(ips []string) (ipsv4, ipsv6 []string) {
	for _, ip := range ips {
		switch {
		case utilnet.IsIPv6String(ip):
			ipsv6 = append(ipsv6, ip)
		default:
			ipsv4 = append(ipsv4, ip)
		}
	}
	return
}

// IsIPContainedInAnyCIDR returns true if ip is contained in any of the given ipnets
func IsIPContainedInAnyCIDR(ip net.IP, ipnets ...*net.IPNet) bool {
	for _, ipnet := range ipnets {
		if ipnet.Contains(ip) {
			return true
		}
	}
	return false
}

// IsContainedInAnyCIDR returns true if ipnet is contained in any of ipnets
func IsContainedInAnyCIDR(ipnet *net.IPNet, ipnets ...*net.IPNet) bool {
	for _, container := range ipnets {
		if ContainsCIDR(container, ipnet) {
			return true
		}
	}
	return false
}

// ContainsCIDR returns true if ipnet1 contains ipnet2
func ContainsCIDR(ipnet1, ipnet2 *net.IPNet) bool {
	mask1, _ := ipnet1.Mask.Size()
	mask2, _ := ipnet2.Mask.Size()
	return mask1 <= mask2 && ipnet1.Contains(ipnet2.IP)
}

// IPNetOverlaps returns ipnets that overlap with the ref
func IPNetOverlaps(ref *net.IPNet, ipnets ...*net.IPNet) []*net.IPNet {
	var overlaps []*net.IPNet
	for _, ipnet := range ipnets {
		if ref.Contains(ipnet.IP) || ipnet.Contains(ref.IP) {
			overlaps = append(overlaps, ipnet)
		}
	}
	return overlaps
}

// ParseIPNets parses the provided string formatted CIDRs
func ParseIPNets[T ~string](strs []T) ([]*net.IPNet, error) {
	ipnets := make([]*net.IPNet, len(strs))
	for i := range strs {
		ip, ipnet, err := utilnet.ParseCIDRSloppy(string(strs[i]))
		if err != nil {
			return nil, err
		}
		ipnet.IP = ip
		ipnets[i] = ipnet
	}
	return ipnets, nil
}

// IsUsableEthernetMAC reports whether mac can serve as a unicast interface
// MAC, mirroring the kernel's is_valid_ether_addr: a 6-byte Ethernet address
// that is neither all-zero nor multicast.
func IsUsableEthernetMAC(mac net.HardwareAddr) bool {
	return len(mac) == 6 && mac[0]&1 == 0 && !bytes.Equal(mac, make(net.HardwareAddr, 6))
}

// GenerateRandMAC generates a random unicast and locally administered MAC address.
// LOOTED FROM https://github.com/cilium/cilium/blob/v1.12.6/pkg/mac/mac.go#L106
func GenerateRandMAC() (net.HardwareAddr, error) {
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		return nil, fmt.Errorf("unable to retrieve 6 rnd bytes: %s", err)
	}

	// Set locally administered addresses bit and reset multicast bit
	buf[0] = (buf[0] | 0x02) & 0xfe

	return buf, nil
}

// CopyIPNets copies the provided slice of IPNet
func CopyIPNets(ipnets []*net.IPNet) []*net.IPNet {
	copy := make([]*net.IPNet, len(ipnets))
	for i := range ipnets {
		if ipnets[i] == nil {
			continue
		}
		copy[i] = &net.IPNet{
			IP:   slices.Clone(ipnets[i].IP),
			Mask: slices.Clone(ipnets[i].Mask),
		}
	}
	return copy
}

func isIPNetEqual(ipn1, ipn2 *net.IPNet) bool {
	if ipn1 == ipn2 {
		return true
	}
	if ipn1 == nil || ipn2 == nil {
		return false
	}
	m1, _ := ipn1.Mask.Size()
	m2, _ := ipn2.Mask.Size()
	return m1 == m2 && ipn1.IP.Equal(ipn2.IP)
}

// IsIPsEqual returns true if both IP slices contain the same addresses,
// regardless of order.
func IsIPsEqual(ips1, ips2 []net.IP) bool {
	if len(ips1) != len(ips2) {
		return false
	}
	s1 := make([]string, len(ips1))
	s2 := make([]string, len(ips2))
	for i := range ips1 {
		s1[i] = ips1[i].String()
		s2[i] = ips2[i].String()
	}
	slices.Sort(s1)
	slices.Sort(s2)
	return slices.Equal(s1, s2)
}

// IsIPNetsEqual returns true if both IPNet slices are equal in length and values, regardless of order.
func IsIPNetsEqual(ipn1, ipn2 []*net.IPNet) bool {
	if len(ipn1) != len(ipn2) {
		return false
	}
	used := make([]bool, len(ipn2))
	for i := range ipn1 {
		found := false
		for j := range ipn2 {
			if used[j] {
				continue
			}
			if isIPNetEqual(ipn1[i], ipn2[j]) {
				used[j] = true
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// IPsToNetworkIPs returns the network CIDRs of the provided IP CIDRs
func IPsToNetworkIPs(ips ...*net.IPNet) []*net.IPNet {
	nets := make([]*net.IPNet, len(ips))
	for i := range ips {
		nets[i] = &net.IPNet{
			IP:   ips[i].IP.Mask(ips[i].Mask),
			Mask: ips[i].Mask,
		}
	}
	return nets
}

func IPNetsIPToStringSlice(ips []*net.IPNet) []string {
	ipAddrs := make([]string, 0)
	for _, ip := range ips {
		ipAddrs = append(ipAddrs, ip.IP.String())
	}
	return ipAddrs
}

func IPNetsToStringSlice(ipNets []*net.IPNet) []string {
	ipNetStrings := make([]string, 0, len(ipNets))
	for _, ipNet := range ipNets {
		ipNetStrings = append(ipNetStrings, ipNet.String())
	}
	return ipNetStrings
}

func IPNetsToIPs(ipNets []*net.IPNet) []net.IP {
	ips := make([]net.IP, 0, len(ipNets))
	for _, ipNet := range ipNets {
		ips = append(ips, ipNet.IP)
	}
	return ips
}

// CalculateRouteTableID will calculate route table ID based on the network
// interface index
func CalculateRouteTableID(ifIndex int) int {
	return ifIndex + config.OvnKubeNode.RoutingTableIDStart
}

// RouteEqual compare two routes
func RouteEqual(l, r *netlink.Route) bool {
	if (l == nil) != (r == nil) {
		return false
	}
	if l == r {
		return true
	}
	if !l.Equal(*r) {
		return false
	}
	return l.Family == r.Family &&
		l.MTU == r.MTU &&
		l.Window == r.Window &&
		l.Rtt == r.Rtt &&
		l.RttVar == r.RttVar &&
		l.Ssthresh == r.Ssthresh &&
		l.Cwnd == r.Cwnd &&
		l.AdvMSS == r.AdvMSS &&
		l.Reordering == r.Reordering &&
		l.Hoplimit == r.Hoplimit &&
		l.InitCwnd == r.InitCwnd &&
		l.Features == r.Features &&
		l.RtoMin == r.RtoMin &&
		l.InitRwnd == r.InitRwnd &&
		l.QuickACK == r.QuickACK &&
		l.Congctl == r.Congctl &&
		l.FastOpenNoCookie == r.FastOpenNoCookie
}

// SubnetBroadcastIP returns the IP network's broadcast IP.
func SubnetBroadcastIP(ipnet net.IPNet) net.IP {
	ip := ipnet.IP
	mask := ipnet.Mask

	// Handle IPv4 addresses in 16-byte representation
	if ip4 := ip.To4(); ip4 != nil {
		ip = ip4
	}

	result := make(net.IP, len(ip))

	// broadcastIP = (networkIP) | (inverted mask)
	for i := range ip {
		result[i] = (ip[i] & mask[i]) | (mask[i] ^ 0xff)
	}
	return result
}

// ParseIPList parses a comma-separated string of IP addresses ignoring spaces.
// Returns an empty slice if the input string is empty.
func ParseIPList(ipsStr string) ([]net.IP, error) {
	if strings.TrimSpace(ipsStr) == "" {
		return nil, nil
	}

	var ips []net.IP
	ipStrings := strings.Split(ipsStr, ",")
	for _, ipStr := range ipStrings {
		ipStr = strings.TrimSpace(ipStr)
		if ipStr == "" {
			continue
		}
		ip := net.ParseIP(ipStr)
		if ip == nil {
			return nil, fmt.Errorf("invalid IP address %q", ipStr)
		}
		ips = append(ips, ip)
	}
	return ips, nil
}

// GetLastIPOfSubnet returns the `indexFromLast`th IP address of a given subnet.
// For example, if indexFromLast is 0 and subnet is 10.0.0.0/24, it returns
// 10.0.0.255/24.
func GetLastIPOfSubnet(subnet *net.IPNet, indexFromLast int) *net.IPNet {
	mask, total := subnet.Mask.Size()
	base := big.NewInt(1)
	totalIPs := new(big.Int).Lsh(base, uint(total-mask))
	lastIPIndex := totalIPs.Sub(totalIPs, big.NewInt(int64(indexFromLast+1)))
	// this is copied from utilnet.AddIPOffset but to allow big.Int offset
	r := big.NewInt(0).Add(utilnet.BigForIP(subnet.IP), lastIPIndex).Bytes()
	r = append(make([]byte, 16), r...)
	lastIP := net.IP(r[len(r)-16:])
	return &net.IPNet{IP: lastIP, Mask: subnet.Mask}
}

func NetworksOverlap(n1, n2 []*net.IPNet) bool {
	for _, s1 := range n1 {
		for _, s2 := range n2 {
			if s1.Contains(s2.IP) || s2.Contains(s1.IP) {
				return true
			}
		}
	}
	return false
}
