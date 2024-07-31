package networkqos

import (
	"fmt"

	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
)

func joinMetaNamespaceAndName(namespace, name string) string {
	if namespace == "" {
		return name
	}
	return namespace + "/" + name
}

func GetNetworkQoSAddrSetDbIDs(nqosNamespace, nqosName, gressIndex, controller string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.AddressSetNetworkQoS, controller,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: fmt.Sprintf("%s:%s", nqosNamespace, nqosName),
			// gressidx is the unique id for address set within given objectName
			libovsdbops.GressIdxKey: gressIndex,
		})
}
