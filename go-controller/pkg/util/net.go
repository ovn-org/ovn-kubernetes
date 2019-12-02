package util

import (
	"fmt"
	"math/big"
	"math/rand"
	"net"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// GenerateMac generates mac address.
func GenerateMac() string {
	prefix := "00:00:00"
	newRand := rand.New(rand.NewSource(time.Now().UnixNano()))
	mac := fmt.Sprintf("%s:%02X:%02X:%02X", prefix, newRand.Intn(255), newRand.Intn(255), newRand.Intn(255))
	return mac
}

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
	out, stderr, err := RunOVNNbctl("get", "logical_switch_port", portName, "dynamic_addresses")
	if err != nil {
		return nil, nil, fmt.Errorf("Error while obtaining dynamic addresses for %s: stdout: %q, stderr: %q, error: %v", portName, out, stderr, err)
	}
	if out == "[]" {
		out, stderr, err = RunOVNNbctl("get", "logical_switch_port", portName, "addresses")
		if err != nil {
			return nil, nil, fmt.Errorf("Error while obtaining static addresses for %s: stdout: %q, stderr: %q, error: %v", portName, out, stderr, err)
		}
	}
	if out == "[]" || out == "[dynamic]" {
		// No addresses
		return nil, nil, nil
	}

	// dynamic addresses have format "0a:00:00:00:00:01 192.168.1.3"
	// static addresses have format ["0a:00:00:00:00:01 192.168.1.3"]
	outStr := strings.Trim(out, `"[]`)
	addresses := strings.Split(outStr, " ")
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
func GetOVSPortMACAddress(portName string) (string, error) {
	macAddress, stderr, err := RunOVSVsctl("--if-exists", "get",
		"interface", portName, "mac_in_use")
	if err != nil {
		return "", fmt.Errorf("failed to get MAC address for %q, stderr: %q, error: %v",
			portName, stderr, err)
	}
	if macAddress == "[]" {
		return "", fmt.Errorf("no mac_address found for %q", portName)
	}
	if runtime.GOOS == windowsOS && macAddress == "00:00:00:00:00:00" {
		// There is a known issue with OVS not correctly picking up the
		// physical network interface MAC address.
		stdout, stderr, err := RunPowershell("$(Get-NetAdapter", "-IncludeHidden",
			"-InterfaceAlias", fmt.Sprintf("\"%s\"", portName), ").MacAddress")
		if err != nil {
			return "", fmt.Errorf("failed to get mac address of %q, stderr: %q, error: %v", portName, stderr, err)
		}
		// Windows returns it in 00-00-00-00-00-00 format, we want ':' instead of '-'
		macAddress = strings.ToLower(strings.Replace(stdout, "-", ":", -1))
	}
	return macAddress, nil
}

// GetNodeWellKnownAddresses returns routerIP, Management Port IP and prefix len
// for a given subnet
func GetNodeWellKnownAddresses(subnet *net.IPNet) (*net.IPNet, *net.IPNet) {
	routerIP := NextIP(subnet.IP)
	return &net.IPNet{IP: routerIP, Mask: subnet.Mask},
		&net.IPNet{IP: NextIP(routerIP), Mask: subnet.Mask}
}

// JoinHostPortInt32 is like net.JoinHostPort(), but with an int32 for the port
func JoinHostPortInt32(host string, port int32) string {
	return net.JoinHostPort(host, strconv.Itoa(int(port)))
}
