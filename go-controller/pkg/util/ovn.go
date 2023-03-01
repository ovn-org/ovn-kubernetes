package util

// Contains helper functions for OVN
// Eventually these should all be migrated to go-ovn bindings

import (
	"fmt"
	"net"
	"strings"

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

	err = libovsdbops.CreateOrUpdateMacBinding(sbClient, &mb, &mb.Datapath, &mb.LogicalPort, &mb.IP, &mb.MAC)
	if err != nil {
		return fmt.Errorf("failed to create mac binding %+v: %v", mb, err)
	}

	return nil
}

func PlatformTypeIsEgressIPCloudProvider() bool {
	return config.Kubernetes.PlatformType == string(ocpconfigapi.AWSPlatformType) ||
		config.Kubernetes.PlatformType == string(ocpconfigapi.GCPPlatformType) ||
		config.Kubernetes.PlatformType == string(ocpconfigapi.AzurePlatformType) ||
		config.Kubernetes.PlatformType == string(ocpconfigapi.OpenStackPlatformType)
}

// GetOVNIfUpCheckMode returns true if OVN supports Port_Binding.up
//
// Starting with v21.03.0 OVN sets OVS.Interface.external-id:ovn-installed
// and OVNSB.Port_Binding.up when all OVS flows associated to a
// logical port have been successfully programmed.
// OVS.Interface.external-id:ovn-installed can only be used correctly
// in a combination with OVS.Interface.external-id:iface-id-ver
func GetOVNIfUpCheckMode() (bool, error) {
	if _, stderr, err := RunOVNSbctl("--columns=up", "list", "Port_Binding"); err != nil {
		if strings.Contains(stderr, "does not contain a column") {
			return false, nil
		}
		return false, fmt.Errorf("failed to check if port_binding is supported in OVN, stderr: %q, error: %v",
			stderr, err)
	}
	return true, nil
}
