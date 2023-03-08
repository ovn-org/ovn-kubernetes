package kubevirt

import (
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
)

const (
	OriginalSwitchNameLabel = types.OvnK8sPrefix + "/original-switch-name"
)

// NetworkInfo is the network information common to all the pods involve
// on the same vm
type NetworkInfo struct {
	// OriginalSwitchName is the switch name where the vm was created
	OriginalSwitchName string

	// Status is the OVN network annotations
	Status string
}
