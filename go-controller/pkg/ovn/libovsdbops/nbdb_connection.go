package libovsdbops

import (
	"context"
	"fmt"

	libovsdbclient "github.com/ovn-org/libovsdb/client"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
)

// findNBDBConnectionByPredicate Looks up connections in the cache based on the provided predicate
func findNBDBConnectionsByPredicate(nbClient libovsdbclient.Client, lookupFunction func(item *nbdb.Connection) bool) ([]nbdb.Connection, error) {
	connections := []nbdb.Connection{}
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	err := nbClient.WhereCache(lookupFunction).List(ctx, &connections)
	if err != nil {
		return nil, fmt.Errorf("can't find connections: %v", err)
	}

	return connections, nil
}

// FindNBDBConnectionsWithUnsetTargets returns connections with targets!=_
func FindNBDBConnectionsWithUnsetTargets(nbClient libovsdbclient.Client) ([]nbdb.Connection, error) {
	connectionPred := func(conn *nbdb.Connection) bool {
		return conn.Target != "_"
	}

	connections, err := findNBDBConnectionsByPredicate(nbClient, connectionPred)
	if err != nil {
		return nil, fmt.Errorf("unable to lookup connections by predicate error: %v", err)
	}

	return connections, nil
}
