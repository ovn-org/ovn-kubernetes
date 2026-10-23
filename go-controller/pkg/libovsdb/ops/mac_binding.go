// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ops

import (
	"fmt"
	"strings"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
)

// IsMacBindingControllerOwned reports whether a static MAC binding for ip on
// the given logical port is one the node MAC binding controller manages: any
// binding on a gateway router external port other than the gateway's own
// masquerade IPs. The controller may reap any such binding it does not
// currently recognize, and CreateOrUpdateStaticMacBinding rejects any other
// component writing one there. This goes away with the MACBindingController
// (hopefully soon) or when ExternalIDs are supported on the Static_Mac_Binding
// table.
func IsMacBindingControllerOwned(port, ip string) bool {
	return strings.HasPrefix(port, types.GWRouterToExtSwitchPrefix) &&
		!config.IsGatewayStaticMACBindingIP(ip)
}

// CreateOrUpdateStaticMacBinding creates or updates the provided static mac
// binding.
func CreateOrUpdateStaticMacBinding(nbClient libovsdbclient.Client, smbs ...*nbdb.StaticMACBinding) error {
	opModels := make([]operationModel, len(smbs))
	for i := range smbs {
		if IsMacBindingControllerOwned(smbs[i].LogicalPort, smbs[i].IP) {
			return fmt.Errorf("program error: static MAC binding for IP %q on gateway port %q should be managed by the MAC binding controller",
				smbs[i].IP, smbs[i].LogicalPort)
		}
		opModel := operationModel{
			Model:          smbs[i],
			OnModelUpdates: onModelUpdatesAllNonDefault(),
			ErrNotFound:    false,
			BulkOp:         false,
		}
		opModels[i] = opModel
	}

	m := newModelClient(nbClient)
	_, err := m.CreateOrUpdate(opModels...)
	return err
}

// DeleteStaticMacBindings deletes the provided static mac bindings
func DeleteStaticMacBindings(nbClient libovsdbclient.Client, smbs ...*nbdb.StaticMACBinding) error {
	opModels := make([]operationModel, len(smbs))
	for i := range smbs {
		opModel := operationModel{
			Model:       smbs[i],
			ErrNotFound: false,
			BulkOp:      false,
		}
		opModels[i] = opModel
	}

	m := newModelClient(nbClient)
	return m.Delete(opModels...)
}

type staticMACBindingPredicate func(*nbdb.StaticMACBinding) bool

// DeleteStaticMACBindingWithPredicateOps returns ops to delete Static MAC entries matching the predicate
func DeleteStaticMACBindingWithPredicateOps(nbClient libovsdbclient.Client, ops []ovsdb.Operation, p staticMACBindingPredicate) ([]ovsdb.Operation, error) {
	found := []*nbdb.StaticMACBinding{}
	opModel := operationModel{
		ModelPredicate: p,
		ExistingResult: &found,
		ErrNotFound:    false,
		BulkOp:         false,
	}

	m := newModelClient(nbClient)
	return m.DeleteOps(ops, opModel)
}

// DeleteStaticMACBindingWithPredicate deletes a Static MAC entry for a logical port from the cache
func DeleteStaticMACBindingWithPredicate(nbClient libovsdbclient.Client, p staticMACBindingPredicate) error {
	ops, err := DeleteStaticMACBindingWithPredicateOps(nbClient, nil, p)
	if err != nil {
		return err
	}
	_, err = TransactAndCheck(nbClient, ops)
	return err
}
