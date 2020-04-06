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

// MustParseIPNet is like netlink.ParseIPNet except that it panics on error; use this for
// converting compile-time constant strings to net.IPNet. (Note that as compared with
// net.ParseCIDR, netlink.ParseIPNet returns the full IP from the input string, without
// masking out any of the bits.)
func MustParseIPNet(cidrStr string) *net.IPNet {
	ip, ipNet, err := net.ParseCIDR(cidrStr)
	if err != nil {
		panic(fmt.Sprintf("Could not parse %q as a CIDR: %v", cidrStr, err))
	}
	if len(ipNet.IP) == 4 {
		ipNet.IP = ip.To4()
	} else {
		ipNet.IP = ip.To16()
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
