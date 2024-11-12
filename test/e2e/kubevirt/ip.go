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
		return nil, fmt.Errorf("failed retrieving adresses with ip command: %s: %w", output, err)
	}
	if err := json.Unmarshal([]byte(output), &ifaces); err != nil {
		return nil, fmt.Errorf("failed unmarshaling ip command addresses: %s: %w", output, err)
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
