package ops

import (
	"context"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	libovsdb "github.com/ovn-org/libovsdb/ovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
)

type portGroupPredicate func(group *nbdb.PortGroup) bool

// FindPortGroupsWithPredicate looks up port groups from the cache based on a
// given predicate
func FindPortGroupsWithPredicate(nbClient libovsdbclient.Client, p portGroupPredicate) ([]*nbdb.PortGroup, error) {
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	found := []*nbdb.PortGroup{}
	err := nbClient.WhereCache(p).List(ctx, &found)
	return found, err
}

// CreateOrUpdatePortGroupsOps creates or updates the provided port groups
// returning the corresponding ops
func CreateOrUpdatePortGroupsOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, pgs ...*nbdb.PortGroup) ([]libovsdb.Operation, error) {
	opModels := make([]operationModel, 0, len(pgs))
	for i := range pgs {
		pg := pgs[i]
		opModel := operationModel{
			Model:          pg,
			OnModelUpdates: getAllUpdatableFields(pg),
			ErrNotFound:    false,
			BulkOp:         false,
		}
		opModels = append(opModels, opModel)
	}

	m := newModelClient(nbClient)
	return m.CreateOrUpdateOps(ops, opModels...)
}

// CreateOrUpdatePortGroups creates or updates the provided port groups
func CreateOrUpdatePortGroups(nbClient libovsdbclient.Client, pgs ...*nbdb.PortGroup) error {
	ops, err := CreateOrUpdatePortGroupsOps(nbClient, nil, pgs...)
	if err != nil {
		return err
	}

	_, err = TransactAndCheck(nbClient, ops)
	return err
}

// CreatePortGroup creates the provided port group if it doesn't exist
func CreatePortGroup(nbClient libovsdbclient.Client, portGroup *nbdb.PortGroup) error {
	opModel := operationModel{
		Model:          portGroup,
		OnModelUpdates: onModelUpdatesNone(),
		ErrNotFound:    false,
		BulkOp:         false,
	}

	m := newModelClient(nbClient)
	_, err := m.CreateOrUpdate(opModel)
	return err
}

// GetPortGroup looks up a port group from the cache
func GetPortGroup(nbClient libovsdbclient.Client, pg *nbdb.PortGroup) (*nbdb.PortGroup, error) {
	found := []*nbdb.PortGroup{}
	opModel := operationModel{
		Model:          pg,
		ExistingResult: &found,
		ErrNotFound:    true,
		BulkOp:         false,
	}

	m := newModelClient(nbClient)
	err := m.Lookup(opModel)
	if err != nil {
		return nil, err
	}

	return found[0], nil
}

func AddPortsToPortGroupOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, name string, ports ...string) ([]libovsdb.Operation, error) {
	if len(ports) == 0 {
		return ops, nil
	}

	pg := nbdb.PortGroup{
		Name:  name,
		Ports: ports,
	}

	opModel := operationModel{
		Model:            &pg,
		OnModelMutations: []interface{}{&pg.Ports},
		ErrNotFound:      true,
		BulkOp:           false,
	}

	m := newModelClient(nbClient)
	return m.CreateOrUpdateOps(ops, opModel)
}

// AddPortsToPortGroup adds the provided ports to the provided port group
func AddPortsToPortGroup(nbClient libovsdbclient.Client, name string, ports ...string) error {
	ops, err := AddPortsToPortGroupOps(nbClient, nil, name, ports...)
	if err != nil {
		return err
	}

	_, err = TransactAndCheck(nbClient, ops)
	return err
}

// DeletePortsFromPortGroupOps removes the provided ports from the provided port
// group and returns the corresponding ops
func DeletePortsFromPortGroupOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, name string, ports ...string) ([]libovsdb.Operation, error) {
	if len(ports) == 0 {
		return ops, nil
	}

	pg := nbdb.PortGroup{
		Name:  name,
		Ports: ports,
	}

	opModel := operationModel{
		Model:            &pg,
		OnModelMutations: []interface{}{&pg.Ports},
		ErrNotFound:      false,
		BulkOp:           false,
	}

	m := newModelClient(nbClient)
	return m.DeleteOps(ops, opModel)
}

// DeletePortsFromPortGroup removes the provided ports from the provided port
// group
func DeletePortsFromPortGroup(nbClient libovsdbclient.Client, name string, ports ...string) error {
	ops, err := DeletePortsFromPortGroupOps(nbClient, nil, name, ports...)
	if err != nil {
		return err
	}

	_, err = TransactAndCheck(nbClient, ops)
	return err
}

// AddACLsToPortGroupOps adds the provided ACLs to the provided port group and
// returns the corresponding ops
func AddACLsToPortGroupOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, name string, acls ...*nbdb.ACL) ([]libovsdb.Operation, error) {
	if len(acls) == 0 {
		return ops, nil
	}

	pg := nbdb.PortGroup{
		Name: name,
		ACLs: make([]string, 0, len(acls)),
	}

	for _, acl := range acls {
		pg.ACLs = append(pg.ACLs, acl.UUID)
	}

	opModel := operationModel{
		Model:            &pg,
		OnModelMutations: []interface{}{&pg.ACLs},
		ErrNotFound:      true,
		BulkOp:           false,
	}

	m := newModelClient(nbClient)
	return m.CreateOrUpdateOps(ops, opModel)
}

// UpdatePortGroupSetACLsOps updates the provided ACLs on the provided port group and
// returns the corresponding ops. It entirely replaces the existing ACLs on the PG with
// the newly provided list
func UpdatePortGroupSetACLsOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, name string, acls []*nbdb.ACL) ([]libovsdb.Operation, error) {
	pg := nbdb.PortGroup{
		Name: name,
		ACLs: make([]string, 0, len(acls)),
	}
	for _, acl := range acls {
		pg.ACLs = append(pg.ACLs, acl.UUID)
	}
	opModel := operationModel{
		Model:          &pg,
		OnModelUpdates: []interface{}{&pg.ACLs},
		ErrNotFound:    true,
		BulkOp:         false,
	}

	m := newModelClient(nbClient)
	return m.CreateOrUpdateOps(ops, opModel)
}

// DeleteACLsFromPortGroupOps removes the provided ACLs from the provided port
// group and returns the corresponding ops
func DeleteACLsFromPortGroupOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, name string, acls ...*nbdb.ACL) ([]libovsdb.Operation, error) {
	if len(acls) == 0 {
		return ops, nil
	}

	pg := nbdb.PortGroup{
		Name: name,
		ACLs: make([]string, 0, len(acls)),
	}

	for _, acl := range acls {
		pg.ACLs = append(pg.ACLs, acl.UUID)
	}

	opModel := operationModel{
		Model:            &pg,
		OnModelMutations: []interface{}{&pg.ACLs},
		ErrNotFound:      false,
		BulkOp:           false,
	}

	m := newModelClient(nbClient)
	return m.DeleteOps(ops, opModel)
}

func DeleteACLsFromPortGroups(nbClient libovsdbclient.Client, names []string, acls ...*nbdb.ACL) error {
	var err error
	var ops []libovsdb.Operation
	for _, pgName := range names {
		ops, err = DeleteACLsFromPortGroupOps(nbClient, ops, pgName, acls...)
		if err != nil {
			return err
		}
	}
	_, err = TransactAndCheck(nbClient, ops)
	return err
}

func DeleteACLsFromAllPortGroups(nbClient libovsdbclient.Client, acls ...*nbdb.ACL) error {
	if len(acls) == 0 {
		return nil
	}

	pg := nbdb.PortGroup{
		ACLs: make([]string, 0, len(acls)),
	}

	for _, acl := range acls {
		pg.ACLs = append(pg.ACLs, acl.UUID)
	}

	opModel := operationModel{
		Model:            &pg,
		ModelPredicate:   func(item *nbdb.PortGroup) bool { return true },
		OnModelMutations: []interface{}{&pg.ACLs},
		ErrNotFound:      false,
		BulkOp:           true,
	}

	m := newModelClient(nbClient)
	ops, err := m.DeleteOps(nil, opModel)
	if err != nil {
		return err
	}
	_, err = TransactAndCheck(nbClient, ops)
	return err
}

// DeletePortGroupsOps deletes the provided port groups and returns the
// corresponding ops
func DeletePortGroupsOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, names ...string) ([]libovsdb.Operation, error) {
	opModels := make([]operationModel, 0, len(names))
	for _, name := range names {
		pg := nbdb.PortGroup{
			Name: name,
		}
		opModel := operationModel{
			Model:       &pg,
			ErrNotFound: false,
			BulkOp:      false,
		}
		opModels = append(opModels, opModel)
	}

	m := newModelClient(nbClient)
	return m.DeleteOps(ops, opModels...)
}

// DeletePortGroups deletes the provided port groups and returns the
// corresponding ops
func DeletePortGroups(nbClient libovsdbclient.Client, names ...string) error {
	ops, err := DeletePortGroupsOps(nbClient, nil, names...)
	if err != nil {
		return err
	}

	_, err = TransactAndCheck(nbClient, ops)
	return err
}

// DeletePortGroupsWithPredicateOps returns the corresponding ops to delete port groups based on
// a given predicate
func DeletePortGroupsWithPredicateOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, p portGroupPredicate) ([]libovsdb.Operation, error) {
	deleted := []*nbdb.PortGroup{}
	opModel := operationModel{
		ModelPredicate: p,
		ExistingResult: &deleted,
		ErrNotFound:    false,
		BulkOp:         true,
	}

	m := newModelClient(nbClient)
	return m.DeleteOps(ops, opModel)
}

// DeletePortGroupsWithPredicate deletes the port groups based on the provided predicate
func DeletePortGroupsWithPredicate(nbClient libovsdbclient.Client, p portGroupPredicate) error {
	ops, err := DeletePortGroupsWithPredicateOps(nbClient, nil, p)
	if err != nil {
		return err
	}

	_, err = TransactAndCheck(nbClient, ops)
	return err
}
