// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ops

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/util/sets"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/model"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/sbdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
)

// ChassisNodeName identifies the Kubernetes node that owns a chassis. Unlike
// hostname, it also applies to DPUs whose hostname differs from the node name.
const ChassisNodeName = "k8s.ovn.org/node-name"

// ChassisOwnedByNode checks ownership, including remote chassis created before
// the explicit owner was recorded, using their Kubernetes node hostname.
func ChassisOwnedByNode(chassis *sbdb.Chassis, nodeName string) bool {
	if owner := chassis.ExternalIDs[ChassisNodeName]; owner != "" {
		return owner == nodeName
	}
	return chassis.Hostname == nodeName
}

// chassisSnapshotOps prevents a cached ownership or zone-state check from being
// invalidated before the transaction commits.
func chassisSnapshotOps(sbClient libovsdbclient.Client, chassis *sbdb.Chassis) ([]ovsdb.Operation, error) {
	timeout := types.OVSDBWaitTimeout
	return sbClient.WhereAll(chassis, model.Condition{Field: &chassis.UUID, Function: ovsdb.ConditionEqual, Value: chassis.UUID}).Wait(
		ovsdb.WaitConditionEqual, &timeout, chassis, &chassis.UUID, &chassis.Hostname, &chassis.ExternalIDs, &chassis.OtherConfig)
}

// ListChassis looks up all chassis from the cache
func ListChassis(sbClient libovsdbclient.Client) ([]*sbdb.Chassis, error) {
	ctx, cancel := context.WithTimeout(context.Background(), config.Default.OVSDBTxnTimeout)
	defer cancel()
	searchedChassis := []*sbdb.Chassis{}
	err := sbClient.List(ctx, &searchedChassis)
	return searchedChassis, err
}

// ListChassisPrivate looks up all chassis private models from the cache
func ListChassisPrivate(sbClient libovsdbclient.Client) ([]*sbdb.ChassisPrivate, error) {
	ctx, cancel := context.WithTimeout(context.Background(), config.Default.OVSDBTxnTimeout)
	defer cancel()
	found := []*sbdb.ChassisPrivate{}
	err := sbClient.List(ctx, &found)
	return found, err
}

// GetChassis looks up a chassis from the cache using the 'Name' column which is an indexed
// column.
func GetChassis(sbClient libovsdbclient.Client, chassis *sbdb.Chassis) (*sbdb.Chassis, error) {
	found := []*sbdb.Chassis{}
	opModel := operationModel{
		Model:          chassis,
		ExistingResult: &found,
		ErrNotFound:    true,
		BulkOp:         false,
	}

	m := newModelClient(sbClient)
	err := m.Lookup(opModel)
	if err != nil {
		return nil, err
	}

	return found[0], nil
}

// DeleteChassis deletes the provided chassis and associated private chassis
func DeleteChassis(sbClient libovsdbclient.Client, chassis ...*sbdb.Chassis) error {
	opModels := make([]operationModel, 0, len(chassis))
	for i := range chassis {
		foundChassis := []*sbdb.Chassis{}
		chassisPrivate := sbdb.ChassisPrivate{
			Name: chassis[i].Name,
		}
		chassisUUID := ""
		opModel := []operationModel{
			{
				Model:          chassis[i],
				ExistingResult: &foundChassis,
				ErrNotFound:    false,
				BulkOp:         false,
				DoAfter: func() {
					if len(foundChassis) > 0 {
						chassisPrivate.Name = foundChassis[0].Name
						chassisUUID = foundChassis[0].UUID
					}
				},
			},
			{
				Model:       &chassisPrivate,
				ErrNotFound: false,
				BulkOp:      false,
			},
			// IGMPGroup has a weak link to chassis, deleting multiple chassis may result in IGMP_Groups
			// with identical values on columns "address", "datapath", and "chassis", when "chassis" goes empty
			{
				Model: &sbdb.IGMPGroup{},
				ModelPredicate: func(group *sbdb.IGMPGroup) bool {
					return group.Chassis != nil && chassisUUID != "" && *group.Chassis == chassisUUID
				},
				ErrNotFound: false,
				BulkOp:      true,
			},
		}
		opModels = append(opModels, opModel...)
	}

	m := newModelClient(sbClient)
	err := m.Delete(opModels...)
	return err
}

type chassisPredicate func(*sbdb.Chassis) bool

// DeleteChassisWithPredicate looks up chassis from the cache based on a given
// predicate and deletes them as well as the associated private chassis
func DeleteChassisWithPredicate(sbClient libovsdbclient.Client, p chassisPredicate) error {
	foundChassis := []*sbdb.Chassis{}
	foundChassisNames := sets.NewString()
	foundChassisUUIDS := sets.NewString()
	opModels := []operationModel{
		{
			Model:          &sbdb.Chassis{},
			ModelPredicate: p,
			ExistingResult: &foundChassis,
			ErrNotFound:    false,
			BulkOp:         true,
			DoAfter: func() {
				for _, chassis := range foundChassis {
					foundChassisNames.Insert(chassis.Name)
					foundChassisUUIDS.Insert(chassis.UUID)
				}
			},
		},
		{
			Model:          &sbdb.ChassisPrivate{},
			ModelPredicate: func(item *sbdb.ChassisPrivate) bool { return foundChassisNames.Has(item.Name) },
			ErrNotFound:    false,
			BulkOp:         true,
		},
		// IGMPGroup has a weak link to chassis, deleting multiple chassis may result in IGMP_Groups
		// with identical values on columns "address", "datapath", and "chassis", when "chassis" goes empty
		{
			Model:          &sbdb.IGMPGroup{},
			ModelPredicate: func(group *sbdb.IGMPGroup) bool { return group.Chassis != nil && foundChassisUUIDS.Has(*group.Chassis) },
			ErrNotFound:    false,
			BulkOp:         true,
		},
	}
	m := newModelClient(sbClient)
	ops, err := m.DeleteOps(nil, opModels...)
	if err != nil {
		return err
	}
	var guards []ovsdb.Operation
	for _, chassis := range foundChassis {
		guard, err := chassisSnapshotOps(sbClient, chassis)
		if err != nil {
			return err
		}
		guards = append(guards, guard...)
	}
	_, err = TransactAndCheck(sbClient, append(guards, ops...))
	return err
}

// CreateOrUpdateChassis creates or updates the chassis record along with the encap record
func CreateOrUpdateChassis(sbClient libovsdbclient.Client, chassis *sbdb.Chassis, encaps ...*sbdb.Encap) error {
	return createOrUpdateChassis(sbClient, chassis, false, encaps...)
}

// CreateOrUpdateLocalChassis permits legacy DPU adoption only for the configured
// controller node and the chassis ID read from its local OVS instance. Neither
// identity argument may be derived from the node's chassis annotation.
func CreateOrUpdateLocalChassis(sbClient libovsdbclient.Client, chassis *sbdb.Chassis, localNodeName, localChassisID string, encaps ...*sbdb.Encap) error {
	if localNodeName == "" || localChassisID == "" ||
		chassis.ExternalIDs[ChassisNodeName] != localNodeName || chassis.Name != localChassisID ||
		chassis.OtherConfig["is-remote"] != "false" {
		return fmt.Errorf("chassis %q does not match the local node and OVS identity", chassis.Name)
	}
	return createOrUpdateChassis(sbClient, chassis, true, encaps...)
}

func createOrUpdateChassis(sbClient libovsdbclient.Client, chassis *sbdb.Chassis, verifiedLocal bool, encaps ...*sbdb.Encap) error {
	var guards []ovsdb.Operation
	if owner := chassis.ExternalIDs[ChassisNodeName]; owner != "" {
		existing, err := GetChassis(sbClient, &sbdb.Chassis{Name: chassis.Name})
		switch {
		case errors.Is(err, libovsdbclient.ErrNotFound):
			timeout := types.OVSDBWaitTimeout
			guards, err = sbClient.WhereAll(chassis, model.Condition{Field: &chassis.Name, Function: ovsdb.ConditionEqual, Value: chassis.Name}).Wait(
				ovsdb.WaitConditionNotEqual, &timeout, chassis, &chassis.Name)
		case err != nil:
			return err
		default:
			// A verified local OVS identity permits adoption of a legacy DPU
			// chassis even when its hostname differs from the Kubernetes node.
			adoptLocal := verifiedLocal && existing.ExternalIDs[ChassisNodeName] == "" &&
				!strings.EqualFold(existing.OtherConfig["is-remote"], "true") &&
				chassis.OtherConfig["is-remote"] == "false"
			if !ChassisOwnedByNode(existing, owner) && !adoptLocal {
				return fmt.Errorf("chassis %q belongs to another node (owner %q, hostname %q), not %q", chassis.Name, existing.ExternalIDs[ChassisNodeName], existing.Hostname, owner)
			}
			guards, err = chassisSnapshotOps(sbClient, existing)
		}
		if err != nil {
			return err
		}
	}
	m := newModelClient(sbClient)
	opModels := make([]operationModel, 0, len(encaps)+1)
	for i := range encaps {
		encap := encaps[i]
		opModel := operationModel{
			Model: encap,
			DoAfter: func() {
				encapsList := append(chassis.Encaps, encap.UUID)
				chassis.Encaps = sets.New(encapsList...).UnsortedList()
			},
			OnModelUpdates: onModelUpdatesNone(),
			ErrNotFound:    false,
			BulkOp:         false,
		}
		opModels = append(opModels, opModel)
	}

	opModel := operationModel{
		Model:            chassis,
		OnModelMutations: []interface{}{&chassis.OtherConfig, &chassis.ExternalIDs},
		OnModelUpdates:   []interface{}{&chassis.Encaps},
		ErrNotFound:      false,
		BulkOp:           false,
	}

	opModels = append(opModels, opModel)
	created, ops, err := m.createOrUpdateOps(guards, opModels...)
	if err != nil {
		return err
	}
	_, err = TransactAndCheckAndSetUUIDs(sbClient, created, ops)
	return err
}
