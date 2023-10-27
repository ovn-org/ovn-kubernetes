package kubevirt

import (
	"encoding/json"
	"fmt"
	"time"

	v1 "kubevirt.io/api/core/v1"
)

type Address struct {
	Ip           string `json:"ip"`
	PrefixLength uint   `json:"prefix-length"`
}

type IP struct {
	Address []Address `json:"address"`
}

type Interface struct {
	Name string `json:"name"`
	IPv4 IP     `json:"ipv4"`
	IPv6 IP     `json:"ipv6"`
}

type NetworkState struct {
	Interfaces []Interface `json:"interfaces"`
}

func RetrieveNetworkState(vmi *v1.VirtualMachineInstance) (*NetworkState, error) {
	output, err := RunCommand(vmi, "nmstatectl show --json", 2*time.Second)
	if err != nil {
		return nil, fmt.Errorf("%s: %v", output, err)
	}
	networkState := &NetworkState{}
	if err := json.Unmarshal([]byte(output), networkState); err != nil {
		return nil, fmt.Errorf("%s: %v", output, err)
	}
	return networkState, nil
}
