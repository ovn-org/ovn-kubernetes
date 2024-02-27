package transaction

import (
	"fmt"

	"github.com/ovn-org/libovsdb/cache"
)

func newIndexExistsDetails(err cache.ErrIndexExists) string {
	return fmt.Sprintf("operation would cause rows in the \"%s\" table to have identical values (%v) for index on column \"%s\". First row, with UUID %s, was inserted by this transaction. Second row, with UUID %s, existed in the database before this operation and was not modified",
		err.Table,
		err.Value,
		err.Index,
		err.New,
		err.Existing,
	)
}
