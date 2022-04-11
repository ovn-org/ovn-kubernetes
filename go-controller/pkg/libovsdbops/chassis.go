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
		opModels = append(opModels, OperationModel{
			Model: &sbdb.ChassisPrivate{
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

	allChassis, err := ListChassis(sbClient)
	if err != nil {
		return err
	}

	// As we find the chassis to be deleted via their node name, assemble a slice
	// with chassis names, so we can remove the associated rows in chassis private table
	chassisNamesToRemove := make([]string, 0, len(nodeNames))

	for _, nodeName := range nodeNames {
		for _, chassis := range allChassis {
			if chassis.Hostname == nodeName {
				opModels = append(opModels, OperationModel{
					Model: &sbdb.Chassis{UUID: chassis.UUID},
				})
				chassisNamesToRemove = append(chassisNamesToRemove, chassis.Name)
				break
			}
		}
	}

	for _, chassisName := range chassisNamesToRemove {
		opModels = append(opModels, OperationModel{
			Model: &sbdb.ChassisPrivate{
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
