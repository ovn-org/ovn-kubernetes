package libovsdbops

import (
	"context"
	"errors"
	"fmt"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
	"time"

	"github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/libovsdb/model"
	"github.com/ovn-org/libovsdb/ovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
)

// TransactWithRetry will attempt a transaction several times if it receives an error indicating that the client
// was not connected when the transaction occurred.
func TransactWithRetryTime(ctx context.Context, c client.Client, ops []ovsdb.Operation) ([]ovsdb.OperationResult, error, time.Duration) {
	var results []ovsdb.OperationResult
	var rpcTime time.Duration
	resultErr := wait.PollImmediateUntilWithContext(ctx, 200*time.Millisecond, func(ctx context.Context) (bool, error) {
		var err error
		results, err, rpcTime = c.TransactTime(ctx, ops...)
		if err == nil {
			return true, nil
		}
		if err != nil && errors.Is(err, client.ErrNotConnected) {
			klog.V(4).Infof("Unable to execute transaction: %+v. Client is disconnected, will retry...", ops[0])
			return false, nil
		}
		return false, err
	})
	return results, resultErr, rpcTime
}

func TransactWithRetry(ctx context.Context, c client.Client, ops []ovsdb.Operation) ([]ovsdb.OperationResult, error) {
	res, err, _ := TransactWithRetryTime(ctx, c, ops)
	return res, err
}

func TransactAndCheckTime(c client.Client, ops []ovsdb.Operation) ([]ovsdb.OperationResult, error, time.Duration) {
	if len(ops) <= 0 {
		return []ovsdb.OperationResult{{}}, nil, 0
	}
	startTime := time.Now()
	defer func() {
		klog.V(4).Infof("Finished TransactAndCheckTime: %v ops: %+v", time.Since(startTime), ops[0])
	}()

	klog.Infof("Configuring OVN: %+v", ops)

	ctx, cancel := context.WithTimeout(context.TODO(), types.OVSDBTimeout)
	defer cancel()

	results, err, rpcTime := TransactWithRetryTime(ctx, c, ops)
	if err != nil {
		return nil, fmt.Errorf("error in transact with ops %+v: %v", ops, err), 0
	}

	opErrors, err := ovsdb.CheckOperationResults(results, ops)
	if err != nil {
		return nil, fmt.Errorf("error in transact with ops %+v results %+v and errors %+v: %v", ops, results, opErrors, err), 0
	}

	return results, nil, rpcTime
}

func TransactAndCheck(c client.Client, ops []ovsdb.Operation) ([]ovsdb.OperationResult, error) {
	startTime := time.Now()
	defer func() {
		klog.V(4).Infof("Finished TransactAndCheck: %v", time.Since(startTime))
	}()
	res, err, _ := TransactAndCheckTime(c, ops)
	return res, err
}

// TransactAndCheckAndSetUUIDs transacts the given ops against client and returns
// results if no error occurred or an error otherwise. It sets the real uuids for
// the passed models if they were inserted and have a named-uuid (as built by
// BuildNamedUUID)
func TransactAndCheckAndSetUUIDsTime(client client.Client, models interface{}, ops []ovsdb.Operation) ([]ovsdb.OperationResult, error, time.Duration) {
	startTime := time.Now()
	defer func() {
		klog.V(4).Infof("Finished TransactAndCheckAndSetUUIDsTime: %v", time.Since(startTime))
	}()
	results, err, rpcTime := TransactAndCheckTime(client, ops)
	if err != nil {
		return nil, err, 0
	}

	namedModelMap := map[string]model.Model{}
	_ = onModels(models, func(model interface{}) error {
		uuid := getUUID(model)
		if IsNamedUUID(uuid) {
			namedModelMap[uuid] = model
		}
		return nil
	})

	if len(namedModelMap) == 0 {
		return results, nil, rpcTime
	}

	for i, op := range ops {
		if op.Op != ovsdb.OperationInsert {
			continue
		}

		if !IsNamedUUID(op.UUIDName) {
			continue
		}

		if model, ok := namedModelMap[op.UUIDName]; ok {
			setUUID(model, results[i].UUID.GoUUID)
		}
	}

	return results, nil, rpcTime
}

// TransactAndCheckAndSetUUIDs transacts the given ops against client and returns
// results if no error occurred or an error otherwise. It sets the real uuids for
// the passed models if they were inserted and have a named-uuid (as built by
// BuildNamedUUID)
func TransactAndCheckAndSetUUIDs(client client.Client, models interface{}, ops []ovsdb.Operation) ([]ovsdb.OperationResult, error) {
	startTime := time.Now()
	defer func() {
		klog.V(4).Infof("Finished TransactAndCheckAndSetUUIDs: %v", time.Since(startTime))
	}()
	res, err, _ := TransactAndCheckAndSetUUIDsTime(client, models, ops)
	return res, err
}
