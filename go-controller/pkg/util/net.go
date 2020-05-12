package util

import (
	"fmt"
	"math/big"
	"net"
	"runtime"
	"strconv"
	"strings"
)

// NextIP returns IP incremented by 1
func NextIP(ip net.IP) net.IP {
	i := ipToInt(ip)
	return intToIP(i.Add(i, big.NewInt(1)))
}

func ipToInt(ip net.IP) *big.Int {
	if v := ip.To4(); v != nil {
		return big.NewInt(0).SetBytes(v)
	}
	return big.NewInt(0).SetBytes(ip.To16())
}

func intToIP(i *big.Int) net.IP {
	return net.IP(i.Bytes())
}

// GetPortAddresses returns the MAC and IP of the given logical switch port
func GetPortAddresses(portName string) (net.HardwareAddr, net.IP, error) {
	out, stderr, err := RunOVNNbctl("get", "logical_switch_port", portName, "dynamic_addresses", "addresses")
	if err != nil {
		return nil, nil, fmt.Errorf("Error while obtaining dynamic addresses for %s: stdout: %q, stderr: %q, error: %v",
			portName, out, stderr, err)
	}
	// Convert \r\n to \n to support Windows line endings
	out = strings.Replace(out, "\r\n", "\n", -1)
	addresses := strings.Split(out, "\n")
	out = addresses[0]
	if out == "[]" {
		out = addresses[1]
	}
	if out == "[]" || out == "[dynamic]" {
		// No addresses
		return nil, nil, nil
	}

	// dynamic addresses have format "0a:00:00:00:00:01 192.168.1.3"
	// static addresses have format ["0a:00:00:00:00:01 192.168.1.3"]
	outStr := strings.Trim(out, `"[]`)
	addresses = strings.Split(outStr, " ")
	if len(addresses) != 2 {
		return nil, nil, fmt.Errorf("Error while obtaining addresses for %s", portName)
	}
	ip := net.ParseIP(addresses[1])
	if ip == nil {
		return nil, nil, fmt.Errorf("failed to parse logical switch port %q IP %q", portName, addresses[1])
	}
	mac, err := net.ParseMAC(addresses[0])
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse logical switch port %q MAC %q: %v", portName, addresses[0], err)
	}
	return mac, ip, nil
}

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
	if runtime.GOOS == windowsOS && macAddress == "00:00:00:00:00:00" {
		// There is a known issue with OVS not correctly picking up the
		// physical network interface MAC address.
		stdout, stderr, err := RunPowershell("$(Get-NetAdapter", "-IncludeHidden",
			"-InterfaceAlias", fmt.Sprintf("\"%s\"", portName), ").MacAddress")
		if err != nil {
			return nil, fmt.Errorf("failed to get mac address of %q, stderr: %q, error: %v", portName, stderr, err)
		}
		macAddress = stdout
	}
	return net.ParseMAC(macAddress)
}

// GetNodeGatewayIfAddr returns the node logical switch gateway address
// (the ".1" address)
func GetNodeGatewayIfAddr(subnet *net.IPNet) *net.IPNet {
	return &net.IPNet{IP: NextIP(subnet.IP), Mask: subnet.Mask}
}

// GetNodeManagementIfAddr returns the node logical switch management port address
// (the ".2" address)
func GetNodeManagementIfAddr(subnet *net.IPNet) *net.IPNet {
	gwIfAddr := GetNodeGatewayIfAddr(subnet)
	return &net.IPNet{IP: NextIP(gwIfAddr.IP), Mask: subnet.Mask}
}

// GetNodeHybridOverlayIfAddr returns the node logical switch hybrid overlay
// port address (the ".3" address)
func GetNodeHybridOverlayIfAddr(subnet *net.IPNet) *net.IPNet {
	mgmtIfAddr := GetNodeManagementIfAddr(subnet)
	return &net.IPNet{IP: NextIP(mgmtIfAddr.IP), Mask: subnet.Mask}
}

// JoinHostPortInt32 is like net.JoinHostPort(), but with an int32 for the port
func JoinHostPortInt32(host string, port int32) string {
	return net.JoinHostPort(host, strconv.Itoa(int(port)))
}

// IPAddrToHWAddr takes the four octets of IPv4 address (aa.bb.cc.dd, for example) and uses them in creating
// a MAC address (0A:58:AA:BB:CC:DD).  For IPv6, we'll use the first two bytes and last two bytes and hope
// that results in a unique MAC for the scope of where it's used.
func IPAddrToHWAddr(ip net.IP) net.HardwareAddr {
	// Ensure that for IPv4, we are always working with the IP in 4-byte form.
	ip4 := ip.To4()
	if ip4 != nil {
		// safe to use private MAC prefix: 0A:58
		return net.HardwareAddr{0x0A, 0x58, ip4[0], ip4[1], ip4[2], ip4[3]}
	}

	// IPv6 - use the first two and last two bytes.
	return net.HardwareAddr{0x0A, 0x58, ip[0], ip[1], ip[14], ip[15]}
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
