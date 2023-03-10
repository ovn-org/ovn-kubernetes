package kubevirt

import (
	"fmt"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

const (
	OriginalSwitchNameLabel = types.OvnK8sPrefix + "/original-switch-name"
	OvnZoneExternalIDKey    = types.OvnK8sPrefix + "/zone"
	OvnRemoteZone           = "remote"
	OvnLocalZone            = "local"
)

// NetworkInfo is the network information common to all the pods belonging
// to the same vm
type NetworkInfo struct {
	// Name is the network name
	Name string

	// OriginalSwitchNames is the original and current switch name
	// related to the VM
	SwitchNames SwitchNames

	// OriginalOvnPodAnnotation is the ovn pod annotation from when VM was created.
	OriginalOvnPodAnnotation *util.PodAnnotation
}

// SwitchNames contains the switch name the pod use to allocate an IP and
// the switch name where the pod us currectly running, after live migration
// those two may be different and should be differenciate
type SwitchNames struct {
	// Current the switch name where the pod is attached to
	Current string

	// Original is the switch name used to allocate an ip
	Original string
}

func (s SwitchNames) String() string {
	if s.Current == s.Original || s.Original == "" {
		return s.Current
	}
	return fmt.Sprintf(`{"current": %q, "original": %q}`, s.Current, s.Original)
}
