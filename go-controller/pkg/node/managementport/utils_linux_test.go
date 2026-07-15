// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package managementport

import (
	"context"
	"testing"

	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
)

func TestCreateManagementPortOVSRepresentorWithClient(t *testing.T) {
	const bridgeUUID = "br-int-uuid"
	ovsClient, cleanup, err := libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{OVSData: []libovsdbtest.TestData{
		&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{bridgeUUID}},
		&vswitchd.Bridge{UUID: bridgeUUID, Name: "br-int"},
	}})
	if err != nil {
		t.Fatalf("failed to create OVS test harness: %v", err)
	}
	t.Cleanup(cleanup.Cleanup)

	err = createManagementPortOVSRepresentor(ovsClient, "blue", "rep0", "k8s-node", 1400, []string{
		types.NetworkExternalID + "=blue",
		types.OvnManagementPortNameExternalID + "=ovn-k8s-mp1",
	})
	if err != nil {
		t.Fatalf("createManagementPortOVSRepresentor() error = %v", err)
	}
	iface := &vswitchd.Interface{Name: "rep0"}
	if err := ovsClient.Get(context.Background(), iface); err != nil {
		t.Fatalf("failed to get created interface: %v", err)
	}
	if iface.ExternalIDs["iface-id"] != "k8s-node" || iface.ExternalIDs[types.NetworkExternalID] != "blue" {
		t.Fatalf("unexpected interface external IDs: %#v", iface.ExternalIDs)
	}
}
