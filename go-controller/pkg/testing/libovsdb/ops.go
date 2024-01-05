package libovsdb

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"

	libovsdbclient "github.com/ovn-org/libovsdb/client"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/sbdb"
)

func CreateTransitSwitchPortBindings(sbClient libovsdbclient.Client, datapath string, names ...string) error {
	h := fnv.New32a()
	h.Write([]byte(datapath))
	dp := &sbdb.DatapathBinding{
		TunnelKey: int(h.Sum32()),
	}

	err := sbClient.Get(context.Background(), dp)
	datapathUUID := dp.UUID
	if errors.Is(err, libovsdbclient.ErrNotFound) {
		ops, err := sbClient.Create(dp)
		if err != nil {
			return err
		}
		r, err := sbClient.Transact(context.Background(), ops...)
		if err != nil {
			return err
		}
		if len(r) != 1 {
			return fmt.Errorf("expected single result when creating datapath binding but got %v", r)
		}
		datapathUUID = r[0].UUID.GoUUID
	}

	for _, name := range names {
		h := fnv.New32a()
		h.Write([]byte(name))
		pb := &sbdb.PortBinding{
			LogicalPort: name,
			Datapath:    datapathUUID,
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
