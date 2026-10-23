// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ops

import (
	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/sbdb"
)

// GetPortBinding looks up a PortBinding by its index (LogicalPort) from the cache.
func GetPortBinding(sbClient libovsdbclient.Client, pb *sbdb.PortBinding) (*sbdb.PortBinding, error) {
	found := []*sbdb.PortBinding{}
	opModel := operationModel{
		Model:          pb,
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
