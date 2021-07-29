package libovsdbops

import (
	"fmt"
	"hash/fnv"

	"github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/libovsdb/ovsdb"
)

const (
	namedUUIDPrefix = 'u'
)

func isNamedUUID(id string) bool {
	return id != "" && id[0] == namedUUIDPrefix
}

func buildNamedUUID(id string) string {
	h := fnv.New64a()
	h.Write([]byte(id)) //nolint:errcheck
	return fmt.Sprintf("%c%d", namedUUIDPrefix, h.Sum64())
}

func TransactAndCheck(client client.Client, ops []ovsdb.Operation) ([]ovsdb.OperationResult, error) {
	if len(ops) <= 0 {
		return []ovsdb.OperationResult{{}}, nil
	}

	results, err := client.Transact(ops...)
	if err != nil {
		return nil, fmt.Errorf("error in transact with ops %+v: %v", ops, err)
	}

	opErrors, err := ovsdb.CheckOperationResults(results, ops)
	if err != nil {
		return nil, fmt.Errorf("error in transact with results %+v and errors %+v: %v", results, opErrors, err)
	}

	return results, nil
}
