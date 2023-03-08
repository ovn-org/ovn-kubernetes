package kubevirt

import (
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	ktypes "k8s.io/apimachinery/pkg/types"
)

func extractVMFromExternalIDs(externalIDs map[string]string) *ktypes.NamespacedName {
	namespace, ok := externalIDs[string(libovsdbops.NamespaceKey)]
	if !ok {
		return nil
	}
	vmName, ok := externalIDs[string(libovsdbops.VirtualMachineKey)]
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
