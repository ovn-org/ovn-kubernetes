package kubevirt

import (
	"encoding/json"
	"fmt"
	"time"

	v1 "kubevirt.io/api/core/v1"
)

func RetrieveAllGlobalAddressesFromGuest(vmi *v1.VirtualMachineInstance) ([]string, error) {
	ifaces := []struct {
		Name      string `json:"ifname"`
		Addresses []struct {
			Family    string `json:"family"`
			Scope     string `json:"scope"`
			Local     string `json:"local"`
			PrefixLen uint   `json:"prefixlen"`
		} `json:"addr_info"`
	}{}

	output, err := RunCommand(vmi, "ip -j a show", 2*time.Second)
	if err != nil {
		return nil, fmt.Errorf("%s: %v", output, err)
	}
	if err := json.Unmarshal([]byte(output), &ifaces); err != nil {
		return nil, fmt.Errorf("%s: %v", output, err)
	}
	addresses := []string{}
	for _, iface := range ifaces {
		if iface.Name == "lo" {
			continue
		}
		for _, address := range iface.Addresses {
			// Skip non DHCPv6 address
			if address.Family == "inet6" && address.PrefixLen != 128 {
				continue
			}
			addresses = append(addresses, address.Local)
		}
	}
	return addresses, nil
}

func RetrieveIPv6GatwayPaths(vmi *v1.VirtualMachineInstance) ([]string, error) {
	routes := []struct {
		Destination string `json:"ifname"`
		Nexthops    []struct {
			Gateway string `json:"gateway"`
		} `json:"nexthops"`
	}{}

	output, err := RunCommand(vmi, "ip -6 -j route list default", 2*time.Second)
	if err != nil {
		return nil, fmt.Errorf("%s: %v", output, err)
	}
	if err := json.Unmarshal([]byte(output), &routes); err != nil {
		return nil, fmt.Errorf("%s: %v", output, err)
	}
	paths := []string{}
	for _, route := range routes {
		for _, nexthop := range route.Nexthops {
			paths = append(paths, nexthop.Gateway)
		}
	}
	return paths, nil
}
