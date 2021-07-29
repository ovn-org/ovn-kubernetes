package libovsdbops

import (
	"fmt"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/libovsdb/model"
	libovsdb "github.com/ovn-org/libovsdb/ovsdb"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
)

func findPortGroup(nbClient libovsdbclient.Client, pg *nbdb.PortGroup) (string, error) {
	if pg.UUID != "" && !isNamedUUID(pg.UUID) {
		return pg.UUID, nil
	}

	err := nbClient.Get(pg)
	if err != nil && err != libovsdbclient.ErrNotFound {
		return "", fmt.Errorf("can't find port group %s: %v", pg.Name, err)
	}

	return pg.UUID, nil
}

func BuildPortGroup(hashName, name string, ports []*nbdb.LogicalSwitchPort, acls []*nbdb.ACL) *nbdb.PortGroup {
	aclIds := make([]string, 0, len(acls))
	for _, acl := range acls {
		aclIds = append(aclIds, acl.UUID)
	}

	portIds := make([]string, 0, len(ports))
	for _, port := range ports {
		portIds = append(portIds, port.UUID)
	}

	return &nbdb.PortGroup{
		Name:        hashName,
		ACLs:        aclIds,
		Ports:       portIds,
		ExternalIDs: map[string]string{"name": name},
	}
}

func CreateOrUpdatePortGroupOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, pg *nbdb.PortGroup) ([]libovsdb.Operation, error) {
	if ops == nil {
		ops = []libovsdb.Operation{}
	}

	uuid, err := findPortGroup(nbClient, pg)
	if err != nil {
		return nil, err
	}

	if uuid == "" {
		op, err := nbClient.Create(pg)
		if err != nil {
			return nil, err
		}
		ops = append(ops, op...)
		return ops, nil
	}

	pg.UUID = uuid
	mutations := []model.Mutation{}
	if len(pg.Ports) > 0 {
		mutations = append(mutations, model.Mutation{
			Field:   &pg.Ports,
			Mutator: libovsdb.MutateOperationInsert,
			Value:   pg.Ports,
		})
	}
	if len(pg.ACLs) > 0 {
		mutations = append(mutations, model.Mutation{
			Field:   &pg.ACLs,
			Mutator: libovsdb.MutateOperationInsert,
			Value:   pg.ACLs,
		})
	}
	if len(pg.ExternalIDs) > 0 {
		mutations = append(mutations, model.Mutation{
			Field:   &pg.ExternalIDs,
			Mutator: libovsdb.MutateOperationInsert,
			Value:   pg.ExternalIDs,
		})
	}
	if len(mutations) > 0 {
		op, err := nbClient.Where(pg).Mutate(pg, mutations...)
		if err != nil {
			return nil, err
		}
		ops = append(ops, op...)
		return ops, nil
	}

	return ops, nil
}

func CreateOrUpdatePortGroup(nbClient libovsdbclient.Client, pg *nbdb.PortGroup) error {
	ops, err := CreateOrUpdatePortGroupOps(nbClient, nil, pg)
	if err != nil {
		return err
	}

	_, err = TransactAndCheck(nbClient, ops)
	if err != nil {
		return err
	}
	return nil
}

func CreateOrUpdatePortGroupsOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, pgs ...*nbdb.PortGroup) ([]libovsdb.Operation, error) {
	if ops == nil {
		ops = []libovsdb.Operation{}
	}
	for _, pg := range pgs {
		var err error
		ops, err = CreateOrUpdatePortGroupOps(nbClient, ops, pg)
		if err != nil {
			return nil, err
		}
	}

	return ops, nil
}

func CreatePortGroups(nbClient libovsdbclient.Client, pgs ...*nbdb.PortGroup) error {
	ops, err := CreateOrUpdatePortGroupsOps(nbClient, nil, pgs...)
	if err != nil {
		return err
	}

	_, err = TransactAndCheck(nbClient, ops)
	return err
}

func AddPortsToPortGroupOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, name string, ports ...string) ([]libovsdb.Operation, error) {
	if ops == nil {
		ops = []libovsdb.Operation{}
	}

	pg := &nbdb.PortGroup{
		Name: name,
	}
	op, err := nbClient.Where(pg).Mutate(pg, model.Mutation{
		Field:   &pg.Ports,
		Mutator: libovsdb.MutateOperationInsert,
		Value:   ports,
	})
	if err != nil {
		return nil, err
	}
	ops = append(ops, op...)

	return ops, nil
}

func AddPortsToPortGroup(nbClient libovsdbclient.Client, name string, ports ...string) error {
	ops, err := AddPortsToPortGroupOps(nbClient, nil, name, ports...)
	if err != nil {
		return err
	}

	_, err = TransactAndCheck(nbClient, ops)
	return err
}

func DeletePortsFromPortGroupOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, name string, ports ...string) ([]libovsdb.Operation, error) {
	if ops == nil {
		ops = []libovsdb.Operation{}
	}

	pg := &nbdb.PortGroup{
		Name: name,
	}
	op, err := nbClient.Where(pg).Mutate(pg, model.Mutation{
		Field:   &pg.Ports,
		Mutator: libovsdb.MutateOperationDelete,
		Value:   ports,
	})
	if err != nil {
		return nil, err
	}
	ops = append(ops, op...)

	return ops, nil
}

func DeletePortsFromPortGroup(nbClient libovsdbclient.Client, name string, ports ...string) error {
	ops, err := DeletePortsFromPortGroupOps(nbClient, nil, name, ports...)
	if err != nil {
		return err
	}

	_, err = TransactAndCheck(nbClient, ops)
	return err
}

func AddACLsToPortGroupOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, name string, acls ...*nbdb.ACL) ([]libovsdb.Operation, error) {
	if ops == nil {
		ops = []libovsdb.Operation{}
	}

	pg := &nbdb.PortGroup{
		Name: name,
	}

	aclUUIDs := make([]string, 0, len(acls))
	for _, acl := range acls {
		aclUUIDs = append(aclUUIDs, acl.UUID)
	}

	op, err := nbClient.Where(pg).Mutate(pg, model.Mutation{
		Field:   &pg.ACLs,
		Mutator: libovsdb.MutateOperationInsert,
		Value:   aclUUIDs,
	})

	if err != nil {
		return nil, err
	}
	ops = append(ops, op...)

	return ops, nil
}

func AddACLsToPortGroup(nbClient libovsdbclient.Client, name string, acls ...*nbdb.ACL) error {
	ops, err := AddACLsToPortGroupOps(nbClient, nil, name, acls...)
	if err != nil {
		return err
	}

	_, err = TransactAndCheck(nbClient, ops)
	return err
}

func DeleteACLsFromPortGroupOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, name string, acls ...*nbdb.ACL) ([]libovsdb.Operation, error) {
	if ops == nil {
		ops = []libovsdb.Operation{}
	}

	pg := &nbdb.PortGroup{
		Name: name,
	}

	uuids := make([]string, 0, len(acls))
	for _, acl := range acls {
		uuid, err := findACL(nbClient, acl)
		if err != nil {
			return nil, err
		}
		if uuid == "" {
			return nil, fmt.Errorf("error, acl not found %+v", acl)
		}
		uuids = append(uuids, uuid)
	}

	op, err := nbClient.Where(pg).Mutate(pg, model.Mutation{
		Field:   &pg.ACLs,
		Mutator: libovsdb.MutateOperationDelete,
		Value:   uuids,
	})

	if err != nil {
		return nil, err
	}
	ops = append(ops, op...)

	return ops, nil
}

func DeleteACLsFromPortGroup(nbClient libovsdbclient.Client, name string, acls ...*nbdb.ACL) error {
	ops, err := DeleteACLsFromPortGroupOps(nbClient, nil, name, acls...)
	if err != nil {
		return err
	}

	_, err = TransactAndCheck(nbClient, ops)
	return err
}

func DeletePortGroupOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, name string) ([]libovsdb.Operation, error) {
	if ops == nil {
		ops = []libovsdb.Operation{}
	}
	pg := &nbdb.PortGroup{
		Name: name,
	}

	op, err := nbClient.Where(pg).Delete()
	if err != nil {
		return nil, err
	}
	ops = append(ops, op...)

	return ops, nil
}

func DeletePortGroup(nbClient libovsdbclient.Client, name string) error {
	ops, err := DeletePortGroupOps(nbClient, nil, name)
	if err != nil {
		return err
	}

	_, err = TransactAndCheck(nbClient, ops)
	return err
}

func DeletePortGroupsOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, names ...string) ([]libovsdb.Operation, error) {
	if ops == nil {
		ops = []libovsdb.Operation{}
	}
	var err error
	for _, name := range names {
		ops, err = DeletePortGroupOps(nbClient, ops, name)
		if err != nil {
			return nil, err
		}
	}

	return ops, nil
}

func DeletePortGroups(nbClient libovsdbclient.Client, names ...string) error {
	ops, err := DeletePortGroupsOps(nbClient, nil, names...)
	if err != nil {
		return err
	}

	_, err = TransactAndCheck(nbClient, ops)
	return err
}
