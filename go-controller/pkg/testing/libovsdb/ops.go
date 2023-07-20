package libovsdb

import (
	"context"
	"hash/fnv"

	libovsdbclient "github.com/ovn-org/libovsdb/client"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/sbdb"
)

func CreateTransitSwitchPortBindings(sbClient libovsdbclient.Client, datapath string, names ...string) error {
	for _, name := range names {
		h := fnv.New32a()
		h.Write([]byte(name))
		pb := &sbdb.PortBinding{
			LogicalPort: name,
			Datapath:    datapath,
			TunnelKey:   int(h.Sum32()),
		}

		ops, err := sbClient.Create(pb)
		if err != nil {
			return err
		}
		_, err = sbClient.Transact(context.Background(), ops...)
		if err != nil {
			return err
		}
	}

	return nil
}
