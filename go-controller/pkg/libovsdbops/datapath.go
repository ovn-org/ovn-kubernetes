package libovsdbops

import (
	"context"
	"fmt"

	libovsdbclient "github.com/ovn-org/libovsdb/client"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/sbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
)

func findDatapathByPredicate(sbClient libovsdbclient.Client, lookupFcn func(dp *sbdb.DatapathBinding) bool) (*sbdb.DatapathBinding, error) {
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	datapathBindings := []sbdb.DatapathBinding{}
	err := sbClient.WhereCache(lookupFcn).List(ctx, &datapathBindings)
	if err != nil {
		return nil, err
	}

	if len(datapathBindings) != 1 {
		return nil, fmt.Errorf("multiple datapath bindings found for a given lookup function")
	}

	return &datapathBindings[0], nil
}

func FindDatapathByExternalIDs(sbClient libovsdbclient.Client, externalIDs map[string]string) (*sbdb.DatapathBinding, error) {
	datapathLookupFcn := func(dp *sbdb.DatapathBinding) bool {
		datapathMatch := false
		for k, v := range externalIDs {
			if itemVal, ok := dp.ExternalIDs[k]; ok {
				if itemVal == v {
					datapathMatch = true
				}
			}
		}

		return datapathMatch
	}

	return findDatapathByPredicate(sbClient, datapathLookupFcn)
}
