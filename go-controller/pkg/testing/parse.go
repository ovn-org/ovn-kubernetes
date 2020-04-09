package testing

import (
	"fmt"
	"net"
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

// MustParseMAC is like net.ParseMAC but it panics on error; use this for converting
// compile-time constant strings to net.HardwareAddr.
func MustParseMAC(macStr string) net.HardwareAddr {
	mac, err := net.ParseMAC(macStr)
	if err != nil {
		panic(fmt.Sprintf("Could not parse %q as a MAC: %v", macStr, err))
	}
	return mac
}
