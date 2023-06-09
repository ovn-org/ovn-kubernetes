package libovsdbops

import (
	"context"
	"fmt"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/vswitchdb"
)

type InterfacePredicate func(*vswitchdb.Interface) bool

// FindInterfacesWithPredicate looks up Interfaces from the cache based on a
// given predicate
func FindInterfacesWithPredicate(vsClient libovsdbclient.Client, p InterfacePredicate) ([]*vswitchdb.Interface, error) {
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	found := []*vswitchdb.Interface{}
	err := vsClient.WhereCache(p).List(ctx, &found)
	return found, err
}

// FindInterfaceByName looks up an Interface from the cache by name
func FindInterfaceByName(vsClient libovsdbclient.Client, ifaceName string) (*vswitchdb.Interface, error) {
	found := []*vswitchdb.Interface{}
	opModel := operationModel{
		Model:          &vswitchdb.Interface{Name: ifaceName},
		ExistingResult: &found,
		ErrNotFound:    true,
		BulkOp:         false,
	}

	m := newModelClient(vsClient)
	if err := m.Lookup(opModel); err != nil {
		return nil, fmt.Errorf("error looking up Interface %q: %w", ifaceName, err)
	}

	return found[0], nil
}

// FindPortByName looks up a Port from the cache by name
func FindPortByName(vsClient libovsdbclient.Client, portName string) (*vswitchdb.Port, error) {
	found := []*vswitchdb.Port{}
	opModel := operationModel{
		Model:          &vswitchdb.Port{Name: portName},
		ExistingResult: &found,
		ErrNotFound:    true,
		BulkOp:         false,
	}

	m := newModelClient(vsClient)
	if err := m.Lookup(opModel); err != nil {
		return nil, fmt.Errorf("error looking up Port %q: %w", portName, err)
	}

	return found[0], nil
}

// CreateQoS deletes QoS records with a "sandbox" ExternalID that
// matches the given sandbox ID
func CreateQoS(vsClient libovsdbclient.Client, sandboxID string, maxRateBPS int64) (*vswitchdb.QoS, error) {
	qos := &vswitchdb.QoS{
		Type: "linux-htb",
		ExternalIDs: map[string]string{
			"sandbox": sandboxID,
		},
		OtherConfig: map[string]string{
			"max-rate": fmt.Sprintf("%d", maxRateBPS),
		},
	}
	opModel := operationModel{
		Model:       qos,
		ErrNotFound: false,
		BulkOp:      true,
	}

	m := newModelClient(vsClient)
	if _, err := m.CreateOrUpdate(opModel); err != nil {
		return nil, err
	}
	return qos, nil
}

// DeleteQoSBySandboxID deletes QoS records with a "sandbox" ExternalID that
// matches the given sandbox ID
func DeleteQoSBySandboxID(vsClient libovsdbclient.Client, sandboxID string) error {
	opModel := operationModel{
		Model: &vswitchdb.QoS{},
		ModelPredicate: func(item *vswitchdb.QoS) bool {
			foundID, ok := item.ExternalIDs["sandbox"]
			return ok && foundID == sandboxID
		},
		ErrNotFound: false,
		BulkOp:      true,
	}

	m := newModelClient(vsClient)
	return m.Delete(opModel)
}

// CreateOrUpdatePortAndAddToBridge creates or updates the provided Interface
// Interfae template, provided Port template, and adds the Port to the given bridge
func CreateOrUpdatePortAndAddToBridge(vsClient libovsdbclient.Client, bridgeUUID string, portTemplate *vswitchdb.Port, ifaceTemplate *vswitchdb.Interface) error {
	ifaceTemplate.Name = portTemplate.Name
	bridge := &vswitchdb.Bridge{
		UUID: bridgeUUID,
	}
	opModels := []operationModel{
		{
			Model:          ifaceTemplate,
			OnModelUpdates: onModelUpdatesAllNonDefault(),
			DoAfter: func() {
				if ifaceTemplate.UUID != "" {
					portTemplate.Interfaces = []string{ifaceTemplate.UUID}
				}
			},
			ErrNotFound: false,
			BulkOp:      false,
		},
		{
			Model:            portTemplate,
			OnModelMutations: []interface{}{&portTemplate.Interfaces},
			DoAfter: func() {
				if portTemplate.UUID != "" {
					bridge.Ports = []string{portTemplate.UUID}
				}
			},
			ErrNotFound: false,
			BulkOp:      false,
		},
		{
			Model:            bridge,
			OnModelMutations: []interface{}{&bridge.Ports},
			ErrNotFound:      true,
			BulkOp:           false,
		},
	}

	m := newModelClient(vsClient)
	if _, err := m.CreateOrUpdate(opModels...); err != nil {
		return fmt.Errorf("failed to create/update ports: %v", err)
	}

	return nil
}

// DeleteInterfacesWithPredicate looks up interfaces from the cache based on a
// given predicate and deletes them
func DeleteInterfacesWithPredicate(vsClient libovsdbclient.Client, p InterfacePredicate) error {
	deleted := []*vswitchdb.Interface{}
	opModel := operationModel{
		ModelPredicate: p,
		ExistingResult: &deleted,
		ErrNotFound:    false,
		BulkOp:         true,
	}

	m := newModelClient(vsClient)
	return m.Delete(opModel)
}

// FindBridgeByName finds a bridge by name
func FindBridgeByName(vsClient libovsdbclient.Client, bridgeName string) (*vswitchdb.Bridge, error) {
	m := newModelClient(vsClient)
	bridge := &vswitchdb.Bridge{Name: bridgeName}
	if err := m.Lookup(operationModel{
		Model:       bridge,
		ErrNotFound: true,
	}); err != nil {
		return nil, err
	}
	return bridge, nil
}

// DeletePorts deletes the given OVS ports by name
func DeletePort(vsClient libovsdbclient.Client, bridgeName, portName string) error {
	m := newModelClient(vsClient)

	bridge := &vswitchdb.Bridge{Name: bridgeName}
	foundPorts := []*vswitchdb.Port{}
	ops, err := m.LookupOps(nil, operationModel{
		Model:          &vswitchdb.Port{Name: portName},
		ExistingResult: &foundPorts,
		DoAfter: func() {
			if len(foundPorts) > 0 {
				bridge.Ports = []string{foundPorts[0].UUID}
			}
		},
		ErrNotFound: false,
	})
	if err != nil {
		return err
	}

	opModels := []operationModel{
		{
			Model:            bridge,
			OnModelMutations: []interface{}{&bridge.Ports},
			ErrNotFound:      true,
			BulkOp:           false,
		},
		{
			Model: &vswitchdb.Interface{Name: portName},
		},
		{
			Model: &vswitchdb.Port{Name: portName},
		},
	}
	ops, err = m.DeleteOps(ops, opModels...)
	if err != nil {
		return err
	}

	_, err = TransactAndCheck(vsClient, ops)
	return err
}

// ClearPortQoSBySandboxID clears QoS for any port that has a corresponding interface
// with the given sandbox ID
func ClearPortQoSBySandboxID(vsClient libovsdbclient.Client, sandboxID string) error {
	portNames := []string{}
	opModels := make([]operationModel, 0, 2)
	opModels = append(opModels, operationModel{
		ExistingResult: &[]*vswitchdb.Interface{},
		ModelPredicate: func(item *vswitchdb.Interface) bool {
			foundID, ok := item.ExternalIDs["sandbox"]
			if ok && foundID == sandboxID {
				portNames = append(portNames, item.Name)
				return true
			}
			return false
		},
		ErrNotFound: false,
		BulkOp:      true,
	})

	portNoQoS := vswitchdb.Port{}
	opModels = append(opModels, operationModel{
		ExistingResult: &[]*vswitchdb.Port{},
		ModelPredicate: func(item *vswitchdb.Port) bool {
			for _, name := range portNames {
				if item.Name == name {
					return true
				}
			}
			return false
		},
		OnModelUpdates: []interface{}{&portNoQoS.QOS},
		ErrNotFound:    false,
		BulkOp:         true,
	})

	m := newModelClient(vsClient)
	_, err := m.Update(opModels...)
	return err
}

// SetInterfaceOVNInstalled sets an OVS Interface's ovn-installed ExternalID.
// Should only be used from testcases.
func SetInterfaceOVNInstalled(vsClient libovsdbclient.Client, name string, ovnInstalled bool) error {
	val := "false"
	if ovnInstalled {
		val = "true"
	}
	model := &vswitchdb.Interface{
		Name:        name,
		ExternalIDs: map[string]string{"ovn-installed": val},
	}
	opModel := operationModel{
		Model:            model,
		OnModelMutations: []interface{}{&model.ExternalIDs},
		ErrNotFound:      false,
	}

	m := newModelClient(vsClient)
	_, err := m.Update(opModel)
	return err
}
