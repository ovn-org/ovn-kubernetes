package util

// Contains helper functions for OVN
// Eventually these should all be migrated to go-ovn bindings

import (
	"fmt"
	"net"

	ocpconfigapi "github.com/openshift/api/config/v1"
	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/sbdb"
)

// CreateMACBinding Creates MAC binding in OVN SBDB
func CreateMACBinding(sbClient libovsdbclient.Client, logicalPort, datapathName string, portMAC net.HardwareAddr, nextHop net.IP) error {
	datapaths, err := libovsdbops.FindDatapathByExternalIDs(sbClient, map[string]string{"name": datapathName})
	if err != nil {
		return err
	}

	if len(datapaths) == 0 {
		return fmt.Errorf("no datapath entries found for %s", datapathName)
	}

	if len(datapaths) > 1 {
		return fmt.Errorf("multiple datapath entries found for %s", datapathName)
	}

	// find Create mac_binding if needed
	mb := &sbdb.MACBinding{
		LogicalPort: logicalPort,
		MAC:         portMAC.String(),
		Datapath:    datapaths[0].UUID,
		IP:          nextHop.String(),
	}

	err = libovsdbops.CreateOrUpdateMacBinding(sbClient, mb)
	if err != nil {
		return fmt.Errorf("failed to create/update MAC_Binding entry of (%s, %s, %s, %s)"+
			"error: %v", datapaths[0].UUID, logicalPort, portMAC, nextHop, err)
	}

	return nil
}

func PlatformTypeIsEgressIPCloudProvider() bool {
	return config.Kubernetes.PlatformType == string(ocpconfigapi.AWSPlatformType) ||
		config.Kubernetes.PlatformType == string(ocpconfigapi.GCPPlatformType) ||
		config.Kubernetes.PlatformType == string(ocpconfigapi.AzurePlatformType)
}
