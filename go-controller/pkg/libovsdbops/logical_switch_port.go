package libovsdbops

import (
	"fmt"

	libovsdbclient "github.com/ovn-org/libovsdb/client"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
)

// CreateLSPOrMutateMac creates a given LSP or mutates its MAC address and adds it to a logical Switch
func CreateLSPOrMutateMac(nbClient libovsdbclient.Client, nodeName, portName, portMAC string) error {
	existingNodeSwitch := &nbdb.LogicalSwitch{
		Name: nodeName,
	}

	existingLSP := []nbdb.LogicalSwitchPort{}

	newLSP := &nbdb.LogicalSwitchPort{
		Name:      portName,
		Addresses: []string{portMAC},
	}

	opModels := []OperationModel{
		// For LSP's Name is a valid index, so no predicate is needed
		{
			Model:          newLSP,
			ExistingResult: &existingLSP,
			OnModelMutations: []interface{}{
				newLSP.Addresses,
			},
			DoAfter: func() {
				// Bulkop is false, so we won't get here if len existingLSP > 1
				if len(existingLSP) == 1 {
					existingNodeSwitch.Ports = []string{existingLSP[0].UUID}
				} else {
					existingNodeSwitch.Ports = []string{newLSP.UUID}
				}
			},
		},
		{
			Model:          existingNodeSwitch,
			ModelPredicate: func(ls *nbdb.LogicalSwitch) bool { return ls.Name == nodeName },
			OnModelMutations: []interface{}{
				&existingNodeSwitch.Ports,
			},
			ErrNotFound: true,
		},
	}

	m := NewModelClient(nbClient)
	if _, err := m.CreateOrUpdate(opModels...); err != nil {
		return fmt.Errorf("failed to create or Mutate LSP %v and add it to Node %s , error: %v", newLSP, nodeName, err)
	}

	return nil
}

// LSPDelete performs an idepotent delete of the LSP provided by it's name
func LSPDelete(nbClient libovsdbclient.Client, LSPName string) error {
	opModel := OperationModel{
		Model: &nbdb.LogicalSwitchPort{
			Name: LSPName,
		},
	}

	m := NewModelClient(nbClient)
	if err := m.Delete(opModel); err != nil {
		return fmt.Errorf("failed to delete LSP %s, error: %v", LSPName, err)
	}

	return nil
}
