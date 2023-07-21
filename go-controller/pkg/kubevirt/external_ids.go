package kubevirt

import (
	"strings"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	ktypes "k8s.io/apimachinery/pkg/types"
)

func extractVMFromExternalIDs(externalIDs map[string]string) *ktypes.NamespacedName {
	key, ok := externalIDs[string(libovsdbops.ObjectNameKey)]
	if ok {
		splitKey := strings.Split(key, "/")
		if len(splitKey) != 2 {
			return nil
		}
		return &ktypes.NamespacedName{Namespace: splitKey[0], Name: splitKey[1]}
	}
	namespace, ok := externalIDs[NamespaceExternalIDsKey]
	if !ok {
		return nil
	}
	vmName, ok := externalIDs[VirtualMachineExternalIDsKey]
	if !ok {
		return nil
	}
	return &ktypes.NamespacedName{Namespace: namespace, Name: vmName}
}

// externalIDContainsVM return true if the nbdb ExternalIDs has namespace
// and name entries matching the VM
func externalIDsContainsVM(externalIDs map[string]string, vm *ktypes.NamespacedName) bool {
	if vm == nil {
		return false
	}
	externalIDsVM := extractVMFromExternalIDs(externalIDs)
	if externalIDsVM == nil {
		return false
	}
	return *vm == *externalIDsVM
}

// OwnsItAndIsOrphanOrWrongZone return true if kubevirt owns this OVN NB
// resource by checking if it has the VM name in external_ids and also checks
// if the expected ovn zone corresponds with the one it created via the
// OvnZoneExternalIDKey
func ownsItAndIsOrphanOrWrongZone(externalIDs map[string]string, vms map[ktypes.NamespacedName]bool) bool {
	vm := extractVMFromExternalIDs(externalIDs)
	if vm == nil {
		return false // Not related to kubevirt
	}
	vmIsLocal, vmFound := vms[*vm]
	resourceOvnZone := externalIDs[OvnZoneExternalIDKey]
	// There is no VM that owns it or is at the wrong zone
	return !vmFound || (vmIsLocal && resourceOvnZone != OvnLocalZone)
}
