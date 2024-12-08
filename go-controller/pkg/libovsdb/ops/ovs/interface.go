package ovs

import (
	"context"
	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/vswitchd"
)

type interfacePredicate func(*vswitchd.Interface) bool

// ListInterfaces looks up all ovs interfaces from the cache
func ListInterfaces(ovsClient libovsdbclient.Client) ([]*vswitchd.Interface, error) {
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	searchedInterfaces := []*vswitchd.Interface{}
	err := ovsClient.List(ctx, &searchedInterfaces)
	return searchedInterfaces, err
}

// FindInterfacesWithPredicate returns all the ovs interfaces in the cache
// that matches the lookup function
func FindInterfacesWithPredicate(ovsClient libovsdbclient.Client, p interfacePredicate) ([]*vswitchd.Interface, error) {
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	searchedInterfaces := []*vswitchd.Interface{}

	err := ovsClient.WhereCache(p).List(ctx, &searchedInterfaces)
	return searchedInterfaces, err
}
