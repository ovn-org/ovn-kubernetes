package kubevirt

import "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"

const (
	OvnZoneExternalIDKey = types.OvnK8sPrefix + "/zone"
	OvnRemoteZone        = "remote"
	OvnLocalZone         = "local"

	NamespaceExternalIDsKey      = "k8s.ovn.org/namespace"
	VirtualMachineExternalIDsKey = "k8s.ovn.org/vm"
)
