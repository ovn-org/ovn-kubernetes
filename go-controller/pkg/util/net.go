package util

import (
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	iputils "github.com/containernetworking/plugins/pkg/ip"
	utilnet "k8s.io/utils/net"
)

const (
	routingTableIDStart = 1000
)

var ErrorNoIP = errors.New("no IP available")

// GetOVSPortMACAddress returns the MAC address of a given OVS port
func GetOVSPortMACAddress(portName string) (net.HardwareAddr, error) {
	macAddress, stderr, err := RunOVSVsctl("--if-exists", "get",
		"interface", portName, "mac_in_use")
	if err != nil {
		return nil, fmt.Errorf("failed to get MAC address for %q, stderr: %q, error: %v",
			portName, stderr, err)
	}
	if macAddress == "[]" {
		return nil, fmt.Errorf("no mac_address found for %q", portName)
	}
	return net.ParseMAC(macAddress)
}

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

// ParseIPNets parses the provided string formatted CIDRs
func ParseIPNets(strs []string) ([]*net.IPNet, error) {
	ipnets := make([]*net.IPNet, len(strs))
	for i := range strs {
		ip, ipnet, err := utilnet.ParseCIDRSloppy(strs[i])
		if err != nil {
			return nil, err
		}
		ipnet.IP = ip
		ipnets[i] = ipnet
	}
	return ipnets, nil
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
		ipnet := *ipnets[i]
		copy[i] = &ipnet
	}
	return copy
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

// CalculateRouteTableID will calculate route table ID based on the network
// interface index
func CalculateRouteTableID(ifIndex int) int {
	return ifIndex + routingTableIDStart
}
