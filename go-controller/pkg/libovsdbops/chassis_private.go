package libovsdbops

import (
	"context"
	"fmt"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/sbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
)

// FindChassisPrivate returns a list of ChassisPrivate from cache. Returned objects are read-only
// because getUUID and setUUID are not configured for ChassisPrivate table in model.go.
func FindChassisPrivate(sbClient libovsdbclient.Client) ([]sbdb.ChassisPrivate, error) {
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	searchedChassisPrivate := []sbdb.ChassisPrivate{}
	err := sbClient.List(ctx, &searchedChassisPrivate)
	if err != nil {
		return nil, fmt.Errorf("failed listing chassis private err: %v", err)
	}
	return searchedChassisPrivate, nil
}
