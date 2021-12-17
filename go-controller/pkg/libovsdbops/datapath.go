package libovsdbops

import (
	"context"

	libovsdbclient "github.com/ovn-org/libovsdb/client"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/sbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
)

func findDatapathByPredicate(sbClient libovsdbclient.Client, lookupFcn func(dp *sbdb.DatapathBinding) bool) ([]sbdb.DatapathBinding, error) {
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	datapathBindings := []sbdb.DatapathBinding{}
	err := sbClient.WhereCache(lookupFcn).List(ctx, &datapathBindings)
	if err != nil {
		return nil, err
	}

	return datapathBindings, nil
}

func FindDatapathByExternalIDs(sbClient libovsdbclient.Client, externalIDs map[string]string) ([]sbdb.DatapathBinding, error) {
	datapathLookupFcn := func(dp *sbdb.DatapathBinding) bool {
		for k, v := range externalIDs {
			if itemVal, ok := dp.ExternalIDs[k]; ok {
				if itemVal != v {
					return false
				}
			} else {
				return false
			}
		}
		return true
	}

	return findDatapathByPredicate(sbClient, datapathLookupFcn)
}
