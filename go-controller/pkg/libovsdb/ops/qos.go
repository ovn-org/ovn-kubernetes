package ops

import (
	"context"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	libovsdb "github.com/ovn-org/libovsdb/ovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
)

func getQoSMutableFields(qos *nbdb.QoS) []interface{} {
	return []interface{}{&qos.Action, &qos.Bandwidth, &qos.Direction, &qos.ExternalIDs,
		&qos.Match, &qos.Priority}
}

type QoSPredicate func(*nbdb.QoS) bool

// FindQoSesWithPredicate looks up QoSes from the cache based on a
// given predicate
func FindQoSesWithPredicate(nbClient libovsdbclient.Client, p QoSPredicate) ([]*nbdb.QoS, error) {
	ctx, cancel := context.WithTimeout(context.Background(), config.Default.OVSDBTxnTimeout)
	defer cancel()
	found := []*nbdb.QoS{}
	err := nbClient.WhereCache(p).List(ctx, &found)
	return found, err
}

// CreateOrUpdateQoSesOps returns the ops to create or update the provided QoSes.
func CreateOrUpdateQoSesOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, qoses ...*nbdb.QoS) ([]libovsdb.Operation, error) {
	opModels := make([]operationModel, 0, len(qoses))
	for i := range qoses {
		// can't use i in the predicate, for loop replaces it in-memory
		qos := qoses[i]
		opModel := operationModel{
			Model:          qos,
			OnModelUpdates: getQoSMutableFields(qos),
			ErrNotFound:    false,
			BulkOp:         false,
		}
		opModels = append(opModels, opModel)
	}

	modelClient := newModelClient(nbClient)
	return modelClient.CreateOrUpdateOps(ops, opModels...)
}

func UpdateQoSesOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, qoses ...*nbdb.QoS) ([]libovsdb.Operation, error) {
	opModels := make([]operationModel, 0, len(qoses))
	for i := range qoses {
		// can't use i in the predicate, for loop replaces it in-memory
		qos := qoses[i]
		opModel := operationModel{
			Model:          qos,
			OnModelUpdates: getQoSMutableFields(qos),
			ErrNotFound:    true,
			BulkOp:         false,
		}
		opModels = append(opModels, opModel)
	}

	modelClient := newModelClient(nbClient)
	return modelClient.CreateOrUpdateOps(ops, opModels...)
}

// AddQoSesToLogicalSwitchOps returns the ops to add the provided QoSes to the switch
func AddQoSesToLogicalSwitchOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, name string, qoses ...*nbdb.QoS) ([]libovsdb.Operation, error) {
	sw := &nbdb.LogicalSwitch{
		Name:     name,
		QOSRules: make([]string, 0, len(qoses)),
	}
	for _, qos := range qoses {
		sw.QOSRules = append(sw.QOSRules, qos.UUID)
	}

	opModels := operationModel{
		Model:            sw,
		OnModelMutations: []interface{}{&sw.QOSRules},
		ErrNotFound:      true,
		BulkOp:           false,
	}

	modelClient := newModelClient(nbClient)
	return modelClient.CreateOrUpdateOps(ops, opModels)
}

// DeleteQoSesOps returns the ops to delete the provided QoSes.
func DeleteQoSesOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, qoses ...*nbdb.QoS) ([]libovsdb.Operation, error) {
	opModels := make([]operationModel, 0, len(qoses))
	for i := range qoses {
		// can't use i in the predicate, for loop replaces it in-memory
		qos := qoses[i]
		opModel := operationModel{
			Model:       qos,
			ErrNotFound: false,
			BulkOp:      false,
		}
		opModels = append(opModels, opModel)
	}

	modelClient := newModelClient(nbClient)
	return modelClient.DeleteOps(ops, opModels...)
}

// RemoveQoSesFromLogicalSwitchOps returns the ops to remove the provided QoSes from the provided switch.
func RemoveQoSesFromLogicalSwitchOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, name string, qoses ...*nbdb.QoS) ([]libovsdb.Operation, error) {
	sw := &nbdb.LogicalSwitch{
		Name:     name,
		QOSRules: make([]string, 0, len(qoses)),
	}
	for _, qos := range qoses {
		sw.QOSRules = append(sw.QOSRules, qos.UUID)
	}

	opModels := operationModel{
		Model:            sw,
		OnModelMutations: []interface{}{&sw.QOSRules},
		ErrNotFound:      false,
		BulkOp:           false,
	}

	modelClient := newModelClient(nbClient)
	return modelClient.DeleteOps(ops, opModels)
}

// DeleteQoSesWithPredicateOps returns the ops to delete QoSes based on a given predicate
func DeleteQoSesWithPredicateOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, p QoSPredicate) ([]libovsdb.Operation, error) {
	deleted := []*nbdb.QoS{}
	opModel := operationModel{
		ModelPredicate: p,
		ExistingResult: &deleted,
		ErrNotFound:    false,
		BulkOp:         true,
	}

	m := newModelClient(nbClient)
	return m.DeleteOps(ops, opModel)
}

// DeleteQoSesWithPredicate looks up QoSes from the cache based on
// a given predicate and deletes them
func DeleteQoSesWithPredicate(nbClient libovsdbclient.Client, p QoSPredicate) error {
	ops, err := DeleteQoSesWithPredicateOps(nbClient, nil, p)
	if err != nil {
		return nil
	}
	_, err = TransactAndCheck(nbClient, ops)
	return err
}
