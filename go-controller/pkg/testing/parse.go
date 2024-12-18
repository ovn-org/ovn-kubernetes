package testing

import (
	"fmt"
	"net"
	"strconv"
)

// MustParseIP is like net.ParseIP but it panics on error; use this for converting
// compile-time constant strings to net.IP
func MustParseIP(ipStr string) net.IP {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		panic(fmt.Sprintf("Could not parse %q as an IP address", ipStr))
	}
	return ip
}

// MustParseIPs is like MustParseIP but returns an array of net.IP
func MustParseIPs(ipStrs ...string) []net.IP {
	ips := make([]net.IP, len(ipStrs))
	for i := range ipStrs {
		ips[i] = MustParseIP(ipStrs[i])
	}
	return ips
}

// MustParseIPNet is like netlink.ParseIPNet or net.ParseCIDR, except that it panics on
// error; use this for converting compile-time constant strings to net.IPNet.
func MustParseIPNet(cidrStr string) *net.IPNet {
	ip, ipNet, err := net.ParseCIDR(cidrStr)
	if err != nil {
		panic(fmt.Sprintf("Could not parse %q as a CIDR: %v", cidrStr, err))
	}
	// To make this compatible both with code that does
	//
	//     _, ipNet, err := net.ParseCIDR(str)
	//
	// and code that does
	//
	//    ipNet, err := netlink.ParseIPNet(str)
	//    ipNet.IP = ip
	//
	// we replace ipNet.IP with ip only if they aren't already equal. This sounds like
	// a no-op but it isn't; in particular, when parsing an IPv4 CIDR, net.ParseCIDR()
	// returns a 4-byte ip but a 16-byte ipNet.IP, so if we just unconditionally
	// replace the latter with the former, it will no longer compare as byte-for-byte
	// equal to the original value.
	if !ipNet.IP.Equal(ip) {
		ipNet.IP = ip
	}
	return ipNet
}

// MustParseIPNets is like MustParseIPNet but returns an array of *net.IPNet
func MustParseIPNets(ipNetStrs ...string) []*net.IPNet {
	ipNets := make([]*net.IPNet, len(ipNetStrs))
	for i := range ipNetStrs {
		ipNets[i] = MustParseIPNet(ipNetStrs[i])
	}
	return ipNets
}

// MustParseMAC is like net.ParseMAC but it panics on error; use this for converting
// compile-time constant strings to net.HardwareAddr.
func MustParseMAC(macStr string) net.HardwareAddr {
	mac, err := net.ParseMAC(macStr)
	if err != nil {
		panic(fmt.Sprintf("Could not parse %q as a MAC: %v", macStr, err))
	}
	return mac
}

func MustAtoi(s string) int {
	i, err := strconv.Atoi(s)
	if err != nil {
		panic(fmt.Sprintf("Could not parse %q as a int: %v", s, err))
	}
	return i
}
