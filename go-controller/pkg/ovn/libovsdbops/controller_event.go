package libovsdbops

import (
	"context"
	"fmt"

	libovsdbclient "github.com/ovn-org/libovsdb/client"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/sbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
)

// ListControllerEvent lists all the entries in sbdb's controller_event table
func ListControllerEvent(sbClient libovsdbclient.Client) ([]sbdb.ControllerEvent, error) {
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	searchedControllerEvent := []sbdb.ControllerEvent{}
	err := sbClient.List(ctx, &sbdb.ControllerEvent{})
	if err != nil {
		return nil, fmt.Errorf("failed listing Controller Events err: %v", err)
	}

	return searchedControllerEvent, nil
}

// DeleteControllerEvent Deletes the provided controller Event entry
func DeleteControllerEvent(sbClient libovsdbclient.Client, event *sbdb.ControllerEvent) error {
	opModel := OperationModel{
		Model:       event,
		ErrNotFound: true,
	}

	m := NewModelClient(sbClient)
	if err := m.Delete(opModel); err != nil {
		return err
	}
	return nil
}
