package libovsdbops

import (
	libovsdbclient "github.com/ovn-org/libovsdb/client"
	libovsdb "github.com/ovn-org/libovsdb/ovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
)

type dhcpOptionsPredicate func(*nbdb.DHCPOptions) bool

// CreateOrUpdateDhcpOptionsOps will configure logical switch port DHCPv4Options and DHCPv6Options fields with
// options at dhcpv4Options and dhcpv6Options arguments and create/update DHCPOptions objects that matches the
// pv4 and pv6 predicates. The DHCP options not provided will be reset to nil the LSP fields.
func CreateOrUpdateDhcpOptionsOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, lsp *nbdb.LogicalSwitchPort, dhcpv4Options, dhcpv6Options *nbdb.DHCPOptions, pv4 dhcpOptionsPredicate, pv6 dhcpOptionsPredicate) ([]libovsdb.Operation, error) {
	opModels := []operationModel{}
	if dhcpv4Options != nil {
		opModels = append(opModels, operationModel{
			Model:          dhcpv4Options,
			ModelPredicate: pv4,
			OnModelUpdates: onModelUpdatesAllNonDefault(),
			DoAfter:        func() { lsp.Dhcpv4Options = &dhcpv4Options.UUID },
			ErrNotFound:    false,
			BulkOp:         false,
		})
	}
	if dhcpv6Options != nil {
		opModels = append(opModels, operationModel{
			Model:          dhcpv6Options,
			ModelPredicate: pv6,
			OnModelUpdates: onModelUpdatesAllNonDefault(),
			DoAfter:        func() { lsp.Dhcpv6Options = &dhcpv6Options.UUID },
			ErrNotFound:    false,
			BulkOp:         false,
		})
	}
	opModels = append(opModels, operationModel{
		Model: lsp,
		OnModelUpdates: []interface{}{
			&lsp.Dhcpv4Options,
			&lsp.Dhcpv6Options,
		},
		ErrNotFound: true,
		BulkOp:      false,
	})

	m := newModelClient(nbClient)
	return m.CreateOrUpdateOps(ops, opModels...)
}

func CreateOrUpdateDhcpOptions(nbClient libovsdbclient.Client, lsp *nbdb.LogicalSwitchPort, dhcpv4Options, dhcpv6Options *nbdb.DHCPOptions, pv4 dhcpOptionsPredicate, pv6 dhcpOptionsPredicate) error {
	ops, err := CreateOrUpdateDhcpOptionsOps(nbClient, nil, lsp, dhcpv4Options, dhcpv6Options, pv4, pv6)
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
