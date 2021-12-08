package libovsdbops

import (
	"context"
	"fmt"

	libovsdbclient "github.com/ovn-org/libovsdb/client"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/sbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
)

// ListChassis returns all the logical chassis
func ListChassis(sbClient libovsdbclient.Client) ([]sbdb.Chassis, error) {
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	searchedChassis := []sbdb.Chassis{}
	err := sbClient.List(ctx, &searchedChassis)
	if err != nil {
		return nil, fmt.Errorf("failed listing chassis err: %v", err)
	}

	return searchedChassis, nil
}

func DeleteChassis(sbClient libovsdbclient.Client, chassisNames ...string) error {
	var opModels []OperationModel

	for _, chassisName := range chassisNames {
		opModels = append(opModels, OperationModel{
			Model: &sbdb.Chassis{
				Name: chassisName,
			},
		})
	}

	m := NewModelClient(sbClient)
	if err := m.Delete(opModels...); err != nil {
		return err
	}
	return nil
}

func DeleteNodeChassis(sbClient libovsdbclient.Client, nodeNames ...string) error {
	var opModels []OperationModel

	for _, nodeName := range nodeNames {
		opModels = append(opModels, OperationModel{
			Model: &sbdb.Chassis{},
			// we must use a predicate here since chassis are not indexed by hostname
			ModelPredicate: func(chass *sbdb.Chassis) bool { return chass.Hostname == nodeName },
		})
	}

	m := NewModelClient(sbClient)
	if err := m.Delete(opModels...); err != nil {
		return err
	}
	return nil
}
