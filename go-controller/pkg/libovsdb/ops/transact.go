// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ops

import (
	"context"
	"errors"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/model"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
)

// TransactWithRetry will attempt a transaction several times if it receives an error indicating that the client
// was not connected when the transaction occurred.
func TransactWithRetry(ctx context.Context, c client.Client, ops []ovsdb.Operation) ([]ovsdb.OperationResult, error) {
	var results []ovsdb.OperationResult
	resultErr := wait.PollUntilContextCancel(ctx, 200*time.Millisecond, true, func(ctx context.Context) (bool, error) {
		var err error
		results, err = c.Transact(ctx, ops...)
		if err == nil {
			return true, nil
		}
		if errors.Is(err, client.ErrNotConnected) {
			klog.V(5).Infof("Unable to execute transaction: %+v. Client is disconnected, will retry...", ops)
			return false, nil
		}
		return false, err
	})
	return results, resultErr
}

func TransactAndCheck(c client.Client, ops []ovsdb.Operation) ([]ovsdb.OperationResult, error) {
	return TransactAndCheckWithContext(context.Background(), c, ops)
}

// TransactAndCheckWithContext transacts ops and checks their results, honoring
// caller cancellation and limiting the transaction to OVSDBTxnTimeout.
// Cancellation does not roll back a transaction that has already committed.
func TransactAndCheckWithContext(ctx context.Context, c client.Client, ops []ovsdb.Operation) ([]ovsdb.OperationResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(ops) <= 0 {
		return []ovsdb.OperationResult{{}}, nil
	}

	klog.V(5).Infof("Configuring OVN: %+v", ops)

	ctx, cancel := context.WithTimeout(ctx, config.Default.OVSDBTxnTimeout)
	defer cancel()

	results, err := TransactWithRetry(ctx, c, ops)
	if err != nil {
		return nil, fmt.Errorf("error in transact with ops %+v: %w", ops, err)
	}

	opErrors, err := ovsdb.CheckOperationResults(results, ops)
	if err != nil {
		return nil, fmt.Errorf("error in transact with ops %+v results %+v and errors %+v: %v", ops, results, opErrors, err)
	}

	return results, nil
}

// TransactAndCheckAndSetUUIDs transacts the given ops against client and returns
// results if no error occurred or an error otherwise. It sets the real uuids for
// the passed models if they were inserted and have a named-uuid (as built by
// BuildNamedUUID)
func TransactAndCheckAndSetUUIDs(client client.Client, models interface{}, ops []ovsdb.Operation) ([]ovsdb.OperationResult, error) {
	results, err := TransactAndCheck(client, ops)
	if err != nil {
		return nil, err
	}

	namedModelMap := map[string]model.Model{}
	_ = onModels(models, func(model interface{}) error {
		uuid := getUUID(model)
		if isNamedUUID(uuid) {
			namedModelMap[uuid] = model
		}
		return nil
	})

	if len(namedModelMap) == 0 {
		return results, nil
	}

	for i, op := range ops {
		if op.Op != ovsdb.OperationInsert {
			continue
		}

		if !isNamedUUID(op.UUIDName) {
			continue
		}

		if model, ok := namedModelMap[op.UUIDName]; ok {
			setUUID(model, results[i].UUID.GoUUID)
		}
	}

	return results, nil
}
