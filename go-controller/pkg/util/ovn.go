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
	p := func(item *sbdb.DatapathBinding) bool {
		return item.ExternalIDs["name"] == datapathName
	}
	datapath, err := libovsdbops.GetDatapathBindingWithPredicate(sbClient, p)
	if err != nil {
		return fmt.Errorf("error getting datapath %s: %v", datapathName, err)
	}

	// find Create mac_binding if needed
	mb := sbdb.MACBinding{
		LogicalPort: logicalPort,
		MAC:         portMAC.String(),
		Datapath:    datapath.UUID,
		IP:          nextHop.String(),
	}

	err = libovsdbops.CreateOrUpdateMacBinding(sbClient, &mb)
	if err != nil {
		return fmt.Errorf("failed to create mac binding %+v: %v", mb, err)
	}

	return nil
}

func PlatformTypeIsEgressIPCloudProvider() bool {
	return config.Kubernetes.PlatformType == string(ocpconfigapi.AWSPlatformType) ||
		config.Kubernetes.PlatformType == string(ocpconfigapi.GCPPlatformType) ||
		config.Kubernetes.PlatformType == string(ocpconfigapi.AzurePlatformType)
}
