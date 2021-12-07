package util

// Contains helper functions for OVN
// Eventually these should all be migrated to go-ovn bindings

import (
	"fmt"
	"net"

	libovsdbclient "github.com/ovn-org/libovsdb/client"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/sbdb"
)

// CreateMACBinding Creates MAC binding in OVN SBDB
func CreateMACBinding(sbClient libovsdbclient.Client, logicalPort, datapathName string, portMAC net.HardwareAddr, nextHop net.IP) error {
	datapath, err := libovsdbops.FindDatapathByExternalIDs(sbClient, map[string]string{"name": datapathName})
	if err != nil {
		return err
	}

	// find Create mac_binding if needed
	mb := &sbdb.MACBinding{
		LogicalPort: logicalPort,
		MAC:         string(portMAC),
		Datapath:    datapath.UUID,
		IP:          string(nextHop),
	}

	err = libovsdbops.CreateOrUpdateMacBinding(sbClient, mb)
	if err != nil {
		return fmt.Errorf("failed to create/update MAC_Binding entry of (%s, %s, %s, %s)"+
			"error: %v", datapath.UUID, logicalPort, portMAC, nextHop, err)
	}

	return nil
}
