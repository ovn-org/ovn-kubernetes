package udn

import (
	idallocator "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/allocator/id"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
)

const (
	vrfTableIDName = "vrf-table"
)

// AllocateVRFTable will calculate the table ID to use with the created VRF
// devices to implement local gateway with user defined networks.
func AllocateVRFTable(vrfInterfaceIndex int) (uint, error) {
	return idallocator.CalculateID(vrfTableIDName, uint(vrfInterfaceIndex), config.UserDefinedNetworks.VRFTableBase, config.UserDefinedNetworks.MaxNetworks)
}
