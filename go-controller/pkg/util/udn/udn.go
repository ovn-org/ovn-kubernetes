package udn

import userdefinednetworkv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"

type specGetter interface {
	GetTopology() userdefinednetworkv1.NetworkTopology
	GetLayer3() *userdefinednetworkv1.Layer3Config
	GetLayer2() *userdefinednetworkv1.Layer2Config
}

func IsPrimaryNetwork(spec specGetter) bool {
	var role userdefinednetworkv1.NetworkRole
	switch spec.GetTopology() {
	case userdefinednetworkv1.NetworkTopologyLayer3:
		role = spec.GetLayer3().Role
	case userdefinednetworkv1.NetworkTopologyLayer2:
		role = spec.GetLayer2().Role
	}

	return role == userdefinednetworkv1.NetworkRolePrimary
}
