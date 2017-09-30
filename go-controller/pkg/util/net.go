package util

import (
	"fmt"
	"math/big"
	"math/rand"
	"net"
)

// GenerateMac generates mac address.
func GenerateMac() string {
	prefix := "00:00:00"
	mac := fmt.Sprintf("%s:%02X:%02X:%02X", prefix, rand.Intn(255), rand.Intn(255), rand.Intn(255))
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
