package libovsdbops

import (
	"context"
	"fmt"
	"math/rand"
	"sync/atomic"

	"github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/libovsdb/ovsdb"
)

const (
	namedUUIDPrefix = 'u'
)

var namedUUIDCounter = rand.Uint32()

// IsNamedUUID checks if the passed id is a named-uuid built with
// BuildNamedUUID
func IsNamedUUID(id string) bool {
	return id != "" && id[0] == namedUUIDPrefix
}

// BuildNamedUUID builds an id that can be used as a named-uuid
// as per OVSDB rfc 7047 section 5.1
func BuildNamedUUID() string {
	return fmt.Sprintf("%c%010d", namedUUIDPrefix, atomic.AddUint32(&namedUUIDCounter, 1))
}

// TransactAndCheck transacts the given ops againts client and returns
// results of no error ocurred or an error otherwise.
func TransactAndCheck(client client.Client, ops []ovsdb.Operation) ([]ovsdb.OperationResult, error) {
	if len(ops) <= 0 {
		return []ovsdb.OperationResult{{}}, nil
	}

	results, err := client.Transact(context.TODO(), ops...)
	if err != nil {
		return nil, fmt.Errorf("error in transact with ops %+v: %v", ops, err)
	}

	opErrors, err := ovsdb.CheckOperationResults(results, ops)
	if err != nil {
		return nil, fmt.Errorf("error in transact with results %+v and errors %+v: %v", results, opErrors, err)
	}

	return results, nil
}
