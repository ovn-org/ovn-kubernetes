package util

import (
	"fmt"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
)

// GetNBZone returns the zone name configured in the OVN Northbound database.
// If the zone name is not configured, it returns the default zone name - "global"
// It retuns error if there is no NBGlobal row.
func GetNBZone(nbClient libovsdbclient.Client) (string, error) {
	nbGlobal := &nbdb.NBGlobal{}
	nbGlobal, err := libovsdbops.GetNBGlobal(nbClient, nbGlobal)
	if err != nil {
		return "", fmt.Errorf("error in getting the NBGlobal row  from Northbound db : err - %w", err)
	}

	if nbGlobal.Name == "" {
		return types.OvnDefaultZone, nil
	}

	return nbGlobal.Name, nil
}
