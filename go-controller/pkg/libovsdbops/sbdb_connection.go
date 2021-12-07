package libovsdbops

import (
	"context"
	"fmt"

	libovsdbclient "github.com/ovn-org/libovsdb/client"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/sbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
)

// findSBDBConnectionByPredicate Looks up connections in the cache based on the provided predicate
func findSBDBConnectionsByPredicate(sbClient libovsdbclient.Client, lookupFunction func(item *sbdb.Connection) bool) ([]sbdb.Connection, error) {
	connections := []sbdb.Connection{}
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	err := sbClient.WhereCache(lookupFunction).List(ctx, &connections)
	if err != nil {
		return nil, fmt.Errorf("can't find connections: %v", err)
	}

	return connections, nil
}

// FindSBDBConnectionsWithUnsetTargets returns connections with targets!=_
func FindSBDBConnectionsWithUnsetTargets(sbClient libovsdbclient.Client) ([]sbdb.Connection, error) {
	connectionPred := func(conn *sbdb.Connection) bool {
		return conn.Target != "_"
	}

	connections, err := findSBDBConnectionsByPredicate(sbClient, connectionPred)
	if err != nil {
		return nil, fmt.Errorf("unable to lookup connections by predicate error: %v", err)
	}

	return connections, nil
}
