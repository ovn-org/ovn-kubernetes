package libovsdbops

import (
	"context"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/sbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
)

// ListChassis looks up all chassis from the cache
func ListChassis(sbClient libovsdbclient.Client) ([]*sbdb.Chassis, error) {
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	searchedChassis := []*sbdb.Chassis{}
	err := sbClient.List(ctx, &searchedChassis)
	return searchedChassis, err
}

// ListChassisPrivate looks up all chassis private models from the cache
func ListChassisPrivate(sbClient libovsdbclient.Client) ([]*sbdb.ChassisPrivate, error) {
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
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
	err := m.Delete(opModels...)
	return err
}

// CreateChassis creates a chassis record along with the encap record
// returns error if the chassis record already exists.
// Note: Normally ovn-controller creates a chassis record when it connects to
// OVN Southbound db.  But with multiple zones deployment, ovn-controller will create the
// chassis record in its own local zone Southbound db. ovn-k network controller
// manager needs to create a chassis records for these remote nodes in its zone Southbound db.
func CreateChassis(sbClient libovsdbclient.Client, chassis *sbdb.Chassis, encap *sbdb.Encap) error {
	m := newModelClient(sbClient)
	opModels := []operationModel{
		{
			Model: encap,
			DoAfter: func() {
				if len(chassis.Encaps) == 0 {
					chassis.Encaps = []string{encap.UUID}
				}
			},
		},
		{
			Model: chassis,
		},
	}

	if _, err := m.CreateOrUpdate(opModels...); err != nil {
		return err
	}
	return nil
}

// UpdateChassisOtherConfig sets otherconfig column on the provided chassis record
func UpdateChassisOtherConfig(sbClient libovsdbclient.Client, chassis *sbdb.Chassis) error {
	otherConfig := chassis.OtherConfig
	chassis, err := GetChassis(sbClient, chassis)
	if err != nil {
		return err
	}

	if chassis.OtherConfig == nil {
		chassis.OtherConfig = map[string]string{}
	}

	for k, v := range otherConfig {
		if v == "" {
			delete(chassis.OtherConfig, k)
		} else {
			chassis.OtherConfig[k] = v
		}
	}

	opModel := operationModel{
		Model:          chassis,
		OnModelUpdates: []interface{}{&chassis.OtherConfig},
		ErrNotFound:    true,
		BulkOp:         false,
	}

	m := newModelClient(sbClient)
	_, err = m.CreateOrUpdate(opModel)
	return err
}
