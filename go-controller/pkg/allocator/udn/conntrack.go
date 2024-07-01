package udn

import (
	idallocator "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/allocator/id"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
)

const (
	ctMarkIDName = "ct-mark"
)

// AllocateConntrackMark will calculate a conntrack mark ubased on the passed
// networkID argument
func AllocateConntrackMark(networkID int) (uint, error) {
	return idallocator.CalculateID(ctMarkIDName, uint(networkID), config.UserDefinedNetworks.ConntrackMarkBase, config.UserDefinedNetworks.MaxNetworks)
}
