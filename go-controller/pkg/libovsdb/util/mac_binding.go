package util

import (
	"context"
	"fmt"
	"net"
	"time"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/libovsdb/model"
	"github.com/ovn-org/libovsdb/ovsdb"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/sbdb"
	"k8s.io/apimachinery/pkg/util/wait"
)

// CreateMACBinding Creates MAC binding in OVN SBDB
func CreateMACBinding(sbClient libovsdbclient.Client, logicalPort, datapathName string, portMAC net.HardwareAddr, nextHop net.IP) error {
	p := func(item *sbdb.DatapathBinding) bool {
		return item.ExternalIDs["name"] == datapathName
	}

	// It could take some time for the datapath to propagate to SBDB, wait for some time
	maxTimeout := 10 * time.Second
	var datapath *sbdb.DatapathBinding
	var err1 error
	err := wait.PollUntilContextTimeout(context.TODO(), 50*time.Millisecond, maxTimeout, true, func(ctx context.Context) (bool, error) {
		if datapath, err1 = libovsdbops.GetDatapathBindingWithPredicate(sbClient, p); err1 != nil {
			return false, nil
		}
		return true, nil
	})

	if err != nil {
		return fmt.Errorf("failed to find datpath: %s, after %s: %w, %v", datapathName, maxTimeout, err, err1)
	}

	// find Create mac_binding if needed
	mb := sbdb.MACBinding{
		LogicalPort: logicalPort,
		MAC:         portMAC.String(),
		Datapath:    datapath.UUID,
		IP:          nextHop.String(),
	}

	err = libovsdbops.CreateOrUpdateMacBinding(sbClient, &mb, &mb.Datapath, &mb.LogicalPort, &mb.IP, &mb.MAC)
	if err != nil {
		return fmt.Errorf("failed to create mac binding %+v: %v", mb, err)
	}

	return nil
}

// DeleteSbdbMacBindingsWithIPs deletes mac bindings from the SBDB on any of the
// provided IPs. This method should only be used to remove legacy entries, as
// static mac bindings should be created in NBDB instead.
func DeleteSbdbMacBindingsWithIPs(sbClient libovsdbclient.Client, ips ...string) error {
	mb := &sbdb.MACBinding{}
	conditions := make([]model.Condition, len(ips))
	for i := range ips {
		conditions = append(conditions,
			model.Condition{
				Field:    &mb.IP,
				Function: ovsdb.ConditionEqual,
				Value:    ips[i],
			},
		)
	}

	// Delete using the client native API instead of model client API to avoid
	// having to monitor & cache the SBDB mac binding table. As opposed to the
	// model client API, the native client API will successfully build a delete
	// OP regardless of whether there is a cache hit or not. The operation is
	// idempotent and there will be no error if there is nothing to delete.
	ops, err := sbClient.WhereAny(mb, conditions...).Delete()
	if err != nil {
		return fmt.Errorf("failed to delete SBDB mac binding while generating ops: %v", err)
	}

	_, err = libovsdbops.TransactAndCheck(sbClient, ops)
	if err != nil {
		return fmt.Errorf("failed to delete SBDB mac binding while transacting ops: %v", err)
	}

	return nil
}
