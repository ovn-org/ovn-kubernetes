package libovsdbops

import (
	libovsdbclient "github.com/ovn-org/libovsdb/client"
	libovsdb "github.com/ovn-org/libovsdb/ovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
)

type dhcpOptionsPredicate func(*nbdb.DHCPOptions) bool

func CreateOrUpdateDhcpv4OptionsOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, lsp *nbdb.LogicalSwitchPort, dhcpOptions *nbdb.DHCPOptions) ([]libovsdb.Operation, error) {
	opModels := []operationModel{}
	opModel := operationModel{
		Model:          dhcpOptions,
		ModelPredicate: func(item *nbdb.DHCPOptions) bool { return item.Cidr == dhcpOptions.Cidr },
		OnModelUpdates: onModelUpdatesAllNonDefault(),
		DoAfter:        func() { lsp.Dhcpv4Options = &dhcpOptions.UUID },
		ErrNotFound:    false,
		BulkOp:         false,
	}
	opModels = append(opModels, opModel)
	opModel = operationModel{
		Model:          lsp,
		ModelPredicate: func(item *nbdb.LogicalSwitchPort) bool { return item.Name == lsp.Name },
		OnModelUpdates: onModelUpdatesAllNonDefault(),
		ErrNotFound:    true,
		BulkOp:         false,
	}
	opModels = append(opModels, opModel)

	m := newModelClient(nbClient)
	return m.CreateOrUpdateOps(ops, opModels...)
}

func CreateOrUpdateDhcpv4Options(nbClient libovsdbclient.Client, lsp *nbdb.LogicalSwitchPort, dhcpOptions *nbdb.DHCPOptions) error {
	ops, err := CreateOrUpdateDhcpv4OptionsOps(nbClient, nil, lsp, dhcpOptions)
	if err != nil {
		return err
	}
	_, err = TransactAndCheck(nbClient, ops)
	return err
}

func DeleteDHCPOptionsWithPredicate(nbClient libovsdbclient.Client, p dhcpOptionsPredicate) error {
	opModel := operationModel{
		Model:          &nbdb.DHCPOptions{},
		ModelPredicate: p,
		ErrNotFound:    false,
		BulkOp:         true,
	}

	m := newModelClient(nbClient)
	return m.Delete(opModel)

}
