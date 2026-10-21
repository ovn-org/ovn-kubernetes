// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ops

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
)

type portScanTrackingClient struct {
	libovsdbclient.Client
	portScans int
}

func (c *portScanTrackingClient) WhereCache(predicate interface{}) libovsdbclient.ConditionalAPI {
	switch predicate.(type) {
	case ovsPortPredicate, func(*vswitchd.Port) bool:
		c.portScans++
	}
	return c.Client.WhereCache(predicate)
}

func TestTransactAndCheckAndWaitForVSwitchd(t *testing.T) {
	ovsClient, cleanup, err := libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{OVSData: []libovsdbtest.TestData{
		&vswitchd.OpenvSwitch{UUID: "root-ovs", NextCfg: 7, CurCfg: 7},
	}})
	if err != nil {
		t.Fatalf("harness setup: %v", err)
	}
	t.Cleanup(cleanup.Cleanup)

	applied := make(chan error, 1)
	go func() {
		applied <- wait.PollUntilContextTimeout(context.Background(), 10*time.Millisecond, types.OVSDBTimeout, true,
			func(context.Context) (bool, error) {
				ovs, err := GetOpenvSwitch(ovsClient)
				if err != nil || ovs.NextCfg != 8 {
					return false, err
				}
				updated := &vswitchd.OpenvSwitch{UUID: ovs.UUID, CurCfg: ovs.NextCfg}
				ops, err := ovsClient.Where(&vswitchd.OpenvSwitch{UUID: ovs.UUID}).Update(updated, &updated.CurCfg)
				if err == nil {
					_, err = TransactAndCheck(ovsClient, ops)
				}
				return err == nil, err
			})
	}()

	ovs, err := GetOpenvSwitch(ovsClient)
	if err != nil {
		t.Fatalf("get Open_vSwitch: %v", err)
	}
	updated := &vswitchd.OpenvSwitch{UUID: ovs.UUID, ExternalIDs: map[string]string{"updated": "true"}}
	ops, err := ovsClient.Where(&vswitchd.OpenvSwitch{UUID: updated.UUID}).Update(updated, &updated.ExternalIDs)
	if err != nil {
		t.Fatalf("build update operation: %v", err)
	}
	if err := TransactAndCheckAndWaitForVSwitchd(context.Background(), ovsClient, ops); err != nil {
		t.Fatalf("transaction and wait failed: %v", err)
	}
	if err := <-applied; err != nil {
		t.Fatalf("emulating ovs-vswitchd: %v", err)
	}
	ovs, err = GetOpenvSwitch(ovsClient)
	if err != nil || ovs.NextCfg != 8 || ovs.CurCfg != 8 || ovs.ExternalIDs["updated"] != "true" {
		t.Fatalf("Open_vSwitch after transaction = %+v, err=%v", ovs, err)
	}
}

func TestGetPortBridge(t *testing.T) {
	bridgeAUUID := buildNamedUUID()
	bridgeBUUID := buildNamedUUID()
	portUUID := buildNamedUUID()
	orphanPortUUID := buildNamedUUID()

	bridgeA := &vswitchd.Bridge{UUID: bridgeAUUID, Name: "br-a", Ports: []string{portUUID}}
	bridgeB := &vswitchd.Bridge{UUID: bridgeBUUID, Name: "br-b"}
	port := &vswitchd.Port{UUID: portUUID, Name: "p1"}
	orphan := &vswitchd.Port{UUID: orphanPortUUID, Name: "orphan"}
	ovs := &vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{bridgeAUUID, bridgeBUUID}}

	tests := []struct {
		desc       string
		portName   string
		expectErr  error
		expectName string
		initialOvs libovsdbtest.TestSetup
	}{
		{
			desc:       "returns the bridge that owns the port",
			portName:   "p1",
			expectName: "br-a",
			initialOvs: libovsdbtest.TestSetup{OVSData: []libovsdbtest.TestData{
				ovs.DeepCopy(), bridgeA.DeepCopy(), bridgeB.DeepCopy(), port.DeepCopy(),
			}},
		},
		{
			desc:      "returns ErrNotFound when no bridge owns the port",
			portName:  "orphan",
			expectErr: libovsdbclient.ErrNotFound,
			initialOvs: libovsdbtest.TestSetup{OVSData: []libovsdbtest.TestData{
				&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{bridgeBUUID}},
				bridgeB.DeepCopy(),
				orphan.DeepCopy(),
			}},
		},
		{
			desc:      "returns ErrNotFound when the port does not exist",
			portName:  "no-such-port",
			expectErr: libovsdbclient.ErrNotFound,
			initialOvs: libovsdbtest.TestSetup{OVSData: []libovsdbtest.TestData{
				&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{bridgeBUUID}},
				bridgeB.DeepCopy(),
			}},
		},
		{
			desc:      "returns error when multiple bridges own the port",
			portName:  "p1",
			expectErr: errMultipleResults,
			initialOvs: libovsdbtest.TestSetup{OVSData: []libovsdbtest.TestData{
				ovs.DeepCopy(),
				bridgeA.DeepCopy(),
				&vswitchd.Bridge{UUID: bridgeBUUID, Name: "br-b", Ports: []string{portUUID}},
				port.DeepCopy(),
			}},
		},
		{
			// Matches ovs-vsctl port-to-br / list-ports: the bridge local port is hidden.
			desc:      "returns ErrNotFound for a bridge's local port",
			portName:  "br-ex",
			expectErr: libovsdbclient.ErrNotFound,
			initialOvs: libovsdbtest.TestSetup{OVSData: []libovsdbtest.TestData{
				&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{bridgeAUUID}},
				&vswitchd.Bridge{UUID: bridgeAUUID, Name: "br-ex", Ports: []string{portUUID}},
				&vswitchd.Port{UUID: portUUID, Name: "br-ex"},
			}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			ovsClient, cleanup, err := libovsdbtest.NewOVSTestHarness(tt.initialOvs)
			if err != nil {
				t.Fatalf("test: %q failed to set up harness: %v", tt.desc, err)
			}
			t.Cleanup(cleanup.Cleanup)

			got, err := GetPortBridge(ovsClient, tt.portName)
			if tt.expectErr != nil {
				if err == nil {
					t.Fatalf("expected error, got bridge %v", got)
				}
				if !errors.Is(err, tt.expectErr) {
					t.Fatalf("expected %v, got %v", tt.expectErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("GetPortBridge() error = %v", err)
			}
			if got.Name != tt.expectName {
				t.Fatalf("expected bridge %q, got %q", tt.expectName, got.Name)
			}
		})
	}
}

func TestFindPortsForPortOrInterface(t *testing.T) {
	bridgeUUID := buildNamedUUID()
	ownPortUUID := buildNamedUUID()
	ownInterfaceUUID := buildNamedUUID()
	bondPortUUID := buildNamedUUID()
	bondMemberInterfaceUUID := buildNamedUUID()
	otherInterfaceUUID := buildNamedUUID()

	ovs := &vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{bridgeUUID}}
	ownPort := &vswitchd.Port{UUID: ownPortUUID, Name: "pf0vf0", Interfaces: []string{ownInterfaceUUID}}
	ownInterface := &vswitchd.Interface{UUID: ownInterfaceUUID, Name: "pf0vf0"}
	// pf0vf0 enslaved in a bond: the Interface keeps its name under a Port
	// named differently.
	bondPort := &vswitchd.Port{UUID: bondPortUUID, Name: "bond0",
		Interfaces: []string{bondMemberInterfaceUUID, otherInterfaceUUID}}
	bondMemberInterface := &vswitchd.Interface{UUID: bondMemberInterfaceUUID, Name: "pf0vf0"}
	otherInterface := &vswitchd.Interface{UUID: otherInterfaceUUID, Name: "eth7"}

	tests := []struct {
		desc            string
		name            string
		expectPortNames []string
		initialOvs      libovsdbtest.TestSetup
	}{
		{
			desc:            "finds the port named after the interface",
			name:            "pf0vf0",
			expectPortNames: []string{"pf0vf0"},
			initialOvs: libovsdbtest.TestSetup{OVSData: []libovsdbtest.TestData{
				ovs.DeepCopy(),
				&vswitchd.Bridge{UUID: bridgeUUID, Name: "br-x", Ports: []string{ownPortUUID}},
				ownPort.DeepCopy(), ownInterface.DeepCopy(),
			}},
		},
		{
			desc:            "finds the port holding the interface as a member",
			name:            "pf0vf0",
			expectPortNames: []string{"bond0"},
			initialOvs: libovsdbtest.TestSetup{OVSData: []libovsdbtest.TestData{
				ovs.DeepCopy(),
				&vswitchd.Bridge{UUID: bridgeUUID, Name: "br-x", Ports: []string{bondPortUUID}},
				bondPort.DeepCopy(), bondMemberInterface.DeepCopy(), otherInterface.DeepCopy(),
			}},
		},
		{
			desc:            "returns no ports for an unknown name",
			name:            "no-such-netdev",
			expectPortNames: []string{},
			initialOvs: libovsdbtest.TestSetup{OVSData: []libovsdbtest.TestData{
				ovs.DeepCopy(),
				&vswitchd.Bridge{UUID: bridgeUUID, Name: "br-x", Ports: []string{ownPortUUID}},
				ownPort.DeepCopy(), ownInterface.DeepCopy(),
			}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			ovsClient, cleanup, err := libovsdbtest.NewOVSTestHarness(tt.initialOvs)
			if err != nil {
				t.Fatalf("test: %q failed to set up harness: %v", tt.desc, err)
			}
			t.Cleanup(cleanup.Cleanup)

			ports, err := FindPortsForPortOrInterface(ovsClient, tt.name)
			if err != nil {
				t.Fatalf("FindPortsForPortOrInterface() error = %v", err)
			}
			portNames := make([]string, 0, len(ports))
			for _, port := range ports {
				portNames = append(portNames, port.Name)
			}
			sort.Strings(portNames)
			if !reflect.DeepEqual(portNames, tt.expectPortNames) {
				t.Fatalf("expected ports %v, got %v", tt.expectPortNames, portNames)
			}
		})
	}
}

func TestGetOVSInterface(t *testing.T) {
	bridgeUUID := buildNamedUUID()
	portUUID := buildNamedUUID()
	ifaceUUID := buildNamedUUID()

	ovs := vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{bridgeUUID}}
	bridge := vswitchd.Bridge{UUID: bridgeUUID, Name: "br-int", Ports: []string{portUUID}}
	port := vswitchd.Port{UUID: portUUID, Name: "test-port", Interfaces: []string{ifaceUUID}}
	iface := vswitchd.Interface{UUID: ifaceUUID, Name: "test-iface", Type: "internal"}

	tests := []struct {
		desc       string
		ifaceName  string
		expectErr  bool
		initialOvs libovsdbtest.TestSetup
	}{
		{
			desc:      "returns existing interface",
			ifaceName: "test-iface",
			initialOvs: libovsdbtest.TestSetup{OVSData: []libovsdbtest.TestData{
				ovs.DeepCopy(), bridge.DeepCopy(), port.DeepCopy(), iface.DeepCopy(),
			}},
		},
		{
			desc:      "returns error for non-existent interface",
			ifaceName: "no-such-iface",
			expectErr: true,
			initialOvs: libovsdbtest.TestSetup{OVSData: []libovsdbtest.TestData{
				&vswitchd.OpenvSwitch{UUID: "root-ovs"},
			}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			ovsClient, cleanup, err := libovsdbtest.NewOVSTestHarness(tt.initialOvs)
			if err != nil {
				t.Fatalf("test: %q failed to set up harness: %v", tt.desc, err)
			}
			t.Cleanup(cleanup.Cleanup)

			got, err := GetOVSInterface(ovsClient, tt.ifaceName)
			if tt.expectErr {
				if err == nil {
					t.Fatalf("expected error, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("GetOVSInterface() error = %v", err)
			}
			if got.Name != tt.ifaceName {
				t.Fatalf("expected interface %q, got %q", tt.ifaceName, got.Name)
			}
		})
	}
}

func TestCreateOrUpdatePodPort(t *testing.T) {
	bridgeUUID := buildNamedUUID()
	mtu := 1450

	ovs := &vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{bridgeUUID}}
	bridge := &vswitchd.Bridge{UUID: bridgeUUID, Name: "br-int"}

	t.Run("creates new port with Type, MTURequest, and ExternalIDs", func(t *testing.T) {
		ovsClient, cleanup, err := libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{
			OVSData: []libovsdbtest.TestData{ovs.DeepCopy(), bridge.DeepCopy()},
		})
		if err != nil {
			t.Fatalf("harness setup: %v", err)
		}
		t.Cleanup(cleanup.Cleanup)

		port := &vswitchd.Port{OtherConfig: map[string]string{"transient": "true"}}
		iface := &vswitchd.Interface{
			Type:        "dpdk",
			MTURequest:  &mtu,
			ExternalIDs: map[string]string{"iface-id": "ns_pod", "sandbox": "abc123"},
		}
		if err := CreateOrUpdatePodPort(ovsClient, "br-int", "vf0", port, iface); err != nil {
			t.Fatalf("CreateOrUpdatePodPort: %v", err)
		}

		got, err := GetOVSInterface(ovsClient, "vf0")
		if err != nil {
			t.Fatalf("GetOVSInterface: %v", err)
		}
		if got.Type != "dpdk" {
			t.Errorf("Type = %q, want dpdk", got.Type)
		}
		if got.MTURequest == nil || *got.MTURequest != mtu {
			t.Errorf("MTURequest = %v, want %d", got.MTURequest, mtu)
		}
		if got.ExternalIDs["iface-id"] != "ns_pod" || got.ExternalIDs["sandbox"] != "abc123" {
			t.Errorf("Interface.ExternalIDs = %v", got.ExternalIDs)
		}
		gotPort, err := GetOVSPort(ovsClient, "vf0")
		if err != nil {
			t.Fatalf("GetOVSPort: %v", err)
		}
		if gotPort.OtherConfig["transient"] != "true" {
			t.Errorf("Port.OtherConfig[transient] = %q, want true", gotPort.OtherConfig["transient"])
		}
	})

	t.Run("updates owned map keys while preserving unmanaged keys", func(t *testing.T) {
		existingIfaceUUID := buildNamedUUID()
		existingPortUUID := buildNamedUUID()
		existingIface := &vswitchd.Interface{UUID: existingIfaceUUID, Name: "vf1", Type: "system", ExternalIDs: map[string]string{"sandbox": "old-sandbox", "ovn-installed": "true"}}
		existingPort := &vswitchd.Port{UUID: existingPortUUID, Name: "vf1", Interfaces: []string{existingIfaceUUID}, ExternalIDs: map[string]string{"third-party": "keep"}, OtherConfig: map[string]string{"unmanaged": "keep"}}
		ovsClient, cleanup, err := libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{
			OVSData: []libovsdbtest.TestData{
				&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{bridgeUUID}},
				&vswitchd.Bridge{UUID: bridgeUUID, Name: "br-int", Ports: []string{existingPortUUID}},
				existingPort, existingIface,
			},
		})
		if err != nil {
			t.Fatalf("harness setup: %v", err)
		}
		t.Cleanup(cleanup.Cleanup)

		port := &vswitchd.Port{ExternalIDs: map[string]string{"owned": "new"}, OtherConfig: map[string]string{"transient": "true"}}
		iface := &vswitchd.Interface{
			Type:        "system",
			ExternalIDs: map[string]string{"sandbox": "new-sandbox", "iface-id": "ns_pod"},
		}
		trackedClient := &portScanTrackingClient{Client: ovsClient}
		if err := CreateOrUpdatePodPort(trackedClient, "br-int", "vf1", port, iface); err != nil {
			t.Fatalf("CreateOrUpdatePodPort: %v", err)
		}
		if trackedClient.portScans != 0 {
			t.Fatalf("updating an existing pod port scanned all ports %d times", trackedClient.portScans)
		}

		got, err := GetOVSInterface(ovsClient, "vf1")
		if err != nil {
			t.Fatalf("GetOVSInterface: %v", err)
		}
		if got.Type != "system" {
			t.Errorf("Type = %q, want system", got.Type)
		}
		if got.ExternalIDs["sandbox"] != "new-sandbox" || got.ExternalIDs["iface-id"] != "ns_pod" {
			t.Errorf("Interface.ExternalIDs = %v", got.ExternalIDs)
		}
		if got.ExternalIDs["ovn-installed"] != "true" {
			t.Errorf("Interface.ExternalIDs lost unmanaged key: %v", got.ExternalIDs)
		}
		gotPort, err := GetOVSPort(ovsClient, "vf1")
		if err != nil {
			t.Fatalf("GetOVSPort: %v", err)
		}
		if gotPort.ExternalIDs["third-party"] != "keep" || gotPort.ExternalIDs["owned"] != "new" {
			t.Errorf("Port.ExternalIDs = %v", gotPort.ExternalIDs)
		}
		if gotPort.OtherConfig["unmanaged"] != "keep" || gotPort.OtherConfig["transient"] != "true" {
			t.Errorf("Port.OtherConfig = %v", gotPort.OtherConfig)
		}
	})

	t.Run("updates port and removes selected interface external IDs in one transaction", func(t *testing.T) {
		existingIfaceUUID := buildNamedUUID()
		existingPortUUID := buildNamedUUID()
		existingIface := &vswitchd.Interface{
			UUID: existingIfaceUUID,
			Name: "vf-strip",
			ExternalIDs: map[string]string{
				"sandbox":               "old-sandbox",
				types.NetworkExternalID: "stale-network",
				types.NADExternalID:     "stale-nad",
				"ovn-installed":         "true",
			},
		}
		existingPort := &vswitchd.Port{UUID: existingPortUUID, Name: "vf-strip", Interfaces: []string{existingIfaceUUID}}
		ovsClient, cleanup, err := libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{
			OVSData: []libovsdbtest.TestData{
				&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{bridgeUUID}},
				&vswitchd.Bridge{UUID: bridgeUUID, Name: "br-int", Ports: []string{existingPortUUID}},
				existingPort, existingIface,
			},
		})
		if err != nil {
			t.Fatalf("harness setup: %v", err)
		}
		t.Cleanup(cleanup.Cleanup)

		port := &vswitchd.Port{OtherConfig: map[string]string{"transient": "true"}}
		iface := &vswitchd.Interface{ExternalIDs: map[string]string{"sandbox": "new-sandbox", "iface-id": "ns_pod"}}
		ops, err := CreateOrUpdatePodPortOps(ovsClient, nil, "br-int", "vf-strip", port, iface)
		if err != nil {
			t.Fatalf("CreateOrUpdatePodPortOps: %v", err)
		}
		ops, err = RemoveOVSInterfaceExternalIDsOps(ovsClient, ops, "vf-strip", types.NetworkExternalID, types.NADExternalID)
		if err != nil {
			t.Fatalf("RemoveOVSInterfaceExternalIDsOps: %v", err)
		}
		if _, err := TransactAndCheck(ovsClient, ops); err != nil {
			t.Fatalf("TransactAndCheck: %v", err)
		}

		got, err := GetOVSInterface(ovsClient, "vf-strip")
		if err != nil {
			t.Fatalf("GetOVSInterface: %v", err)
		}
		if got.ExternalIDs["sandbox"] != "new-sandbox" || got.ExternalIDs["iface-id"] != "ns_pod" {
			t.Errorf("Interface.ExternalIDs = %v", got.ExternalIDs)
		}
		if _, ok := got.ExternalIDs[types.NetworkExternalID]; ok {
			t.Errorf("Interface.ExternalIDs still contains %q: %v", types.NetworkExternalID, got.ExternalIDs)
		}
		if _, ok := got.ExternalIDs[types.NADExternalID]; ok {
			t.Errorf("Interface.ExternalIDs still contains %q: %v", types.NADExternalID, got.ExternalIDs)
		}
		if got.ExternalIDs["ovn-installed"] != "true" {
			t.Errorf("Interface.ExternalIDs lost unmanaged key: %v", got.ExternalIDs)
		}
	})

	t.Run("repairs existing port interface reference", func(t *testing.T) {
		existingIfaceUUID := buildNamedUUID()
		staleIfaceUUID := buildNamedUUID()
		existingPortUUID := buildNamedUUID()
		existingIface := &vswitchd.Interface{UUID: existingIfaceUUID, Name: "vf2", Type: "system", ExternalIDs: map[string]string{"sandbox": "old-sandbox"}}
		staleIface := &vswitchd.Interface{UUID: staleIfaceUUID, Name: "stale-vf2", Type: "internal"}
		existingPort := &vswitchd.Port{UUID: existingPortUUID, Name: "vf2", Interfaces: []string{staleIfaceUUID}}
		ovsClient, cleanup, err := libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{
			OVSData: []libovsdbtest.TestData{
				&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{bridgeUUID}},
				&vswitchd.Bridge{UUID: bridgeUUID, Name: "br-int", Ports: []string{existingPortUUID}},
				existingPort, existingIface, staleIface,
			},
		})
		if err != nil {
			t.Fatalf("harness setup: %v", err)
		}
		t.Cleanup(cleanup.Cleanup)

		port := &vswitchd.Port{OtherConfig: map[string]string{"transient": "true"}}
		iface := &vswitchd.Interface{
			Type:        "system",
			ExternalIDs: map[string]string{"sandbox": "new-sandbox", "iface-id": "ns_pod"},
		}
		if err := CreateOrUpdatePodPort(ovsClient, "br-int", "vf2", port, iface); err != nil {
			t.Fatalf("CreateOrUpdatePodPort: %v", err)
		}

		gotPort, err := GetOVSPort(ovsClient, "vf2")
		if err != nil {
			t.Fatalf("GetOVSPort: %v", err)
		}
		if len(gotPort.Interfaces) != 1 || gotPort.Interfaces[0] == staleIfaceUUID {
			t.Fatalf("Port.Interfaces = %v, want one non-stale interface", gotPort.Interfaces)
		}
		gotIface := &vswitchd.Interface{UUID: gotPort.Interfaces[0]}
		if err := ovsClient.Get(context.Background(), gotIface); err != nil {
			t.Fatalf("get Interface %s: %v", gotPort.Interfaces[0], err)
		}
		if gotIface.Name != "vf2" {
			t.Fatalf("referenced Interface.Name = %q, want vf2", gotIface.Name)
		}
		if gotIface.ExternalIDs["sandbox"] != "new-sandbox" || gotIface.ExternalIDs["iface-id"] != "ns_pod" {
			t.Errorf("Interface.ExternalIDs = %v", gotIface.ExternalIDs)
		}
	})

	t.Run("returns error when bridge does not exist", func(t *testing.T) {
		ovsClient, cleanup, err := libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{
			OVSData: []libovsdbtest.TestData{&vswitchd.OpenvSwitch{UUID: "root-ovs"}},
		})
		if err != nil {
			t.Fatalf("harness setup: %v", err)
		}
		t.Cleanup(cleanup.Cleanup)

		port := &vswitchd.Port{}
		iface := &vswitchd.Interface{}
		if err := CreateOrUpdatePodPort(ovsClient, "no-such-bridge", "vf0", port, iface); err == nil {
			t.Fatalf("expected error for missing bridge, got nil")
		}
	})

	t.Run("rejects nil port or iface with a clear error", func(t *testing.T) {
		ovsClient, cleanup, err := libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{
			OVSData: []libovsdbtest.TestData{
				&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{bridgeUUID}},
				bridge.DeepCopy(),
			},
		})
		if err != nil {
			t.Fatalf("harness setup: %v", err)
		}
		t.Cleanup(cleanup.Cleanup)

		if err := CreateOrUpdatePodPort(ovsClient, "br-int", "vf0", nil, &vswitchd.Interface{}); err == nil {
			t.Errorf("nil port: expected error, got nil")
		}
		if err := CreateOrUpdatePodPort(ovsClient, "br-int", "vf0", &vswitchd.Port{}, nil); err == nil {
			t.Errorf("nil iface: expected error, got nil")
		}
	})

	t.Run("rejects port already attached to another bridge", func(t *testing.T) {
		otherBridgeUUID := buildNamedUUID()
		otherPortUUID := buildNamedUUID()
		otherIfaceUUID := buildNamedUUID()
		otherIface := &vswitchd.Interface{UUID: otherIfaceUUID, Name: "vf0", Type: "system"}
		otherPort := &vswitchd.Port{UUID: otherPortUUID, Name: "vf0", Interfaces: []string{otherIfaceUUID}}
		ovsClient, cleanup, err := libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{
			OVSData: []libovsdbtest.TestData{
				&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{bridgeUUID, otherBridgeUUID}},
				&vswitchd.Bridge{UUID: bridgeUUID, Name: "br-int"},
				&vswitchd.Bridge{UUID: otherBridgeUUID, Name: "br-other", Ports: []string{otherPortUUID}},
				otherPort, otherIface,
			},
		})
		if err != nil {
			t.Fatalf("harness setup: %v", err)
		}
		t.Cleanup(cleanup.Cleanup)

		port := &vswitchd.Port{}
		iface := &vswitchd.Interface{Type: "system"}
		if err := CreateOrUpdatePodPort(ovsClient, "br-int", "vf0", port, iface); err == nil {
			t.Fatalf("expected error when vf0 lives on br-other, got nil")
		}

		brInt, err := GetBridge(ovsClient, "br-int")
		if err != nil {
			t.Fatalf("GetBridge br-int: %v", err)
		}
		if len(brInt.Ports) != 0 {
			t.Errorf("br-int.Ports = %v, want empty (no cross-bridge port should be attached)", brInt.Ports)
		}
		brOther, err := GetBridge(ovsClient, "br-other")
		if err != nil {
			t.Fatalf("GetBridge br-other: %v", err)
		}
		if len(brOther.Ports) != 1 {
			t.Errorf("br-other.Ports = %v, want exactly the original port", brOther.Ports)
		}
		owner, err := GetPortBridge(ovsClient, "vf0")
		if err != nil {
			t.Fatalf("GetPortBridge: %v", err)
		}
		if owner.Name != "br-other" {
			t.Errorf("vf0 owner = %q, want br-other", owner.Name)
		}
	})

	for _, targetExists := range []bool{false, true} {
		t.Run(fmt.Sprintf("rejects interface owned by another port with targetExists=%t", targetExists), func(t *testing.T) {
			existingIfaceUUID := buildNamedUUID()
			existingPortUUID := buildNamedUUID()
			bridge := &vswitchd.Bridge{UUID: bridgeUUID, Name: "br-int", Ports: []string{existingPortUUID}}
			data := []libovsdbtest.TestData{
				&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{bridgeUUID}},
				bridge,
				&vswitchd.Port{UUID: existingPortUUID, Name: "bond0", Interfaces: []string{existingIfaceUUID}},
				&vswitchd.Interface{UUID: existingIfaceUUID, Name: "vf0", Type: "system"},
			}
			if targetExists {
				bridge.Ports = append(bridge.Ports, "target-port")
				data = append(data,
					&vswitchd.Port{UUID: "target-port", Name: "vf0", Interfaces: []string{"target-iface"}},
					&vswitchd.Interface{UUID: "target-iface", Name: "vf1"},
				)
			}
			ovsClient, cleanup, err := libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{OVSData: data})
			if err != nil {
				t.Fatalf("harness setup: %v", err)
			}
			t.Cleanup(cleanup.Cleanup)

			trackedClient := &portScanTrackingClient{Client: ovsClient}
			err = CreateOrUpdatePodPort(trackedClient, "br-int", "vf0", &vswitchd.Port{}, &vswitchd.Interface{})
			if err == nil || !strings.Contains(err.Error(), `interface "vf0" is already attached to port "bond0"`) {
				t.Fatalf("expected conflict naming the interface owner, got %v", err)
			}
			if trackedClient.portScans != 1 {
				t.Fatalf("resolving conflicting interface owner scanned ports %d times, want 1", trackedClient.portScans)
			}
			if _, err := GetOVSPort(ovsClient, "vf0"); !targetExists && !errors.Is(err, libovsdbclient.ErrNotFound) {
				t.Fatalf("unexpected vf0 port after rejected transaction: %v", err)
			}
		})
	}
}

func TestCreateOrUpdateNicBridge(t *testing.T) {
	t.Run("creates bridge with hwaddr, fail-mode, and bridge-id/bridge-uplink external_ids", func(t *testing.T) {
		ovsClient, cleanup, err := libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{
			OVSData: []libovsdbtest.TestData{&vswitchd.OpenvSwitch{UUID: "root-ovs"}},
		})
		if err != nil {
			t.Fatalf("harness setup: %v", err)
		}
		t.Cleanup(cleanup.Cleanup)

		const hwaddr = "aa:bb:cc:dd:ee:ff"
		if err := CreateOrUpdateNicBridge(ovsClient, "breth0", "eth0", hwaddr); err != nil {
			t.Fatalf("CreateOrUpdateNicBridge: %v", err)
		}

		br, err := GetBridge(ovsClient, "breth0")
		if err != nil {
			t.Fatalf("GetBridge breth0: %v", err)
		}
		if br.FailMode == nil || *br.FailMode != "standalone" {
			t.Errorf("FailMode = %v, want standalone", br.FailMode)
		}
		if br.ExternalIDs["bridge-id"] != "breth0" {
			t.Errorf("ExternalIDs[bridge-id] = %q, want breth0", br.ExternalIDs["bridge-id"])
		}
		if br.ExternalIDs["bridge-uplink"] != "eth0" {
			t.Errorf("ExternalIDs[bridge-uplink] = %q, want eth0", br.ExternalIDs["bridge-uplink"])
		}
		if got := br.OtherConfig["hwaddr"]; got != hwaddr {
			t.Errorf("OtherConfig[hwaddr] = %q, want %q", got, hwaddr)
		}
	})

	t.Run("updates existing bridge metadata and port interface references", func(t *testing.T) {
		bridgeUUID := buildNamedUUID()
		internalIfaceUUID := buildNamedUUID()
		staleInternalIfaceUUID := buildNamedUUID()
		internalPortUUID := buildNamedUUID()
		uplinkIfaceUUID := buildNamedUUID()
		staleUplinkIfaceUUID := buildNamedUUID()
		uplinkPortUUID := buildNamedUUID()

		const hwaddr = "11:22:33:44:55:66"
		ovsClient, cleanup, err := libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{
			OVSData: []libovsdbtest.TestData{
				&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{bridgeUUID}},
				&vswitchd.Bridge{
					UUID:        bridgeUUID,
					Name:        "breth0",
					Ports:       []string{internalPortUUID, uplinkPortUUID},
					ExternalIDs: map[string]string{"keep": "external"},
					OtherConfig: map[string]string{"keep": "other", "hwaddr": "00:00:00:00:00:00"},
				},
				&vswitchd.Interface{UUID: internalIfaceUUID, Name: "breth0", Type: "system"},
				&vswitchd.Interface{UUID: staleInternalIfaceUUID, Name: "stale-breth0", Type: "internal"},
				&vswitchd.Port{UUID: internalPortUUID, Name: "breth0", Interfaces: []string{staleInternalIfaceUUID}},
				&vswitchd.Interface{UUID: uplinkIfaceUUID, Name: "eth0", Type: "system"},
				&vswitchd.Interface{UUID: staleUplinkIfaceUUID, Name: "stale-eth0", Type: "internal"},
				&vswitchd.Port{UUID: uplinkPortUUID, Name: "eth0", Interfaces: []string{staleUplinkIfaceUUID}},
			},
		})
		if err != nil {
			t.Fatalf("harness setup: %v", err)
		}
		t.Cleanup(cleanup.Cleanup)

		if err := CreateOrUpdateNicBridge(ovsClient, "breth0", "eth0", hwaddr); err != nil {
			t.Fatalf("CreateOrUpdateNicBridge: %v", err)
		}

		br, err := GetBridge(ovsClient, "breth0")
		if err != nil {
			t.Fatalf("GetBridge breth0: %v", err)
		}
		if br.ExternalIDs["bridge-id"] != "breth0" || br.ExternalIDs["bridge-uplink"] != "eth0" || br.ExternalIDs["keep"] != "external" {
			t.Errorf("Bridge.ExternalIDs = %v", br.ExternalIDs)
		}
		if br.OtherConfig["hwaddr"] != hwaddr || br.OtherConfig["keep"] != "other" {
			t.Errorf("Bridge.OtherConfig = %v", br.OtherConfig)
		}

		internalPort, err := GetOVSPort(ovsClient, "breth0")
		if err != nil {
			t.Fatalf("GetOVSPort breth0: %v", err)
		}
		if len(internalPort.Interfaces) != 1 || internalPort.Interfaces[0] == staleInternalIfaceUUID {
			t.Errorf("internal Port.Interfaces = %v, want one non-stale interface", internalPort.Interfaces)
		} else {
			internalIface := &vswitchd.Interface{UUID: internalPort.Interfaces[0]}
			if err := ovsClient.Get(context.Background(), internalIface); err != nil {
				t.Fatalf("get internal Interface %s: %v", internalPort.Interfaces[0], err)
			}
			if internalIface.Name != "breth0" || internalIface.Type != "internal" {
				t.Errorf("internal Interface = %+v, want name breth0 type internal", internalIface)
			}
		}
		uplinkPort, err := GetOVSPort(ovsClient, "eth0")
		if err != nil {
			t.Fatalf("GetOVSPort eth0: %v", err)
		}
		if len(uplinkPort.Interfaces) != 1 || uplinkPort.Interfaces[0] == staleUplinkIfaceUUID {
			t.Errorf("uplink Port.Interfaces = %v, want one non-stale interface", uplinkPort.Interfaces)
		} else {
			uplinkIface := &vswitchd.Interface{UUID: uplinkPort.Interfaces[0]}
			if err := ovsClient.Get(context.Background(), uplinkIface); err != nil {
				t.Fatalf("get uplink Interface %s: %v", uplinkPort.Interfaces[0], err)
			}
			if uplinkIface.Name != "eth0" {
				t.Errorf("uplink Interface.Name = %q, want eth0", uplinkIface.Name)
			}
		}
		if uplinkPort.OtherConfig["transient"] != "true" {
			t.Errorf("uplink Port.OtherConfig = %v, want transient=true", uplinkPort.OtherConfig)
		}
	})

	t.Run("rejects uplink interface already attached to a differently named port", func(t *testing.T) {
		bridgeUUID := buildNamedUUID()
		bondPortUUID := buildNamedUUID()
		uplinkIfaceUUID := buildNamedUUID()
		ovsClient, cleanup, err := libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{
			OVSData: []libovsdbtest.TestData{
				&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{bridgeUUID}},
				&vswitchd.Bridge{UUID: bridgeUUID, Name: "br-existing", Ports: []string{bondPortUUID}},
				&vswitchd.Port{UUID: bondPortUUID, Name: "bond0", Interfaces: []string{uplinkIfaceUUID}},
				&vswitchd.Interface{UUID: uplinkIfaceUUID, Name: "eth0", Type: "system"},
			},
		})
		if err != nil {
			t.Fatalf("harness setup: %v", err)
		}
		t.Cleanup(cleanup.Cleanup)

		err = CreateOrUpdateNicBridge(ovsClient, "breth0", "eth0", "00:11:22:33:44:55")
		if err == nil {
			t.Fatal("expected error when interface eth0 belongs to port bond0")
		}
		if _, err := GetBridge(ovsClient, "breth0"); !errors.Is(err, libovsdbclient.ErrNotFound) {
			t.Fatalf("unexpected breth0 bridge after rejected transaction: %v", err)
		}
	})
}

func TestGetOVSPort(t *testing.T) {
	bridgeUUID := buildNamedUUID()
	portUUID := buildNamedUUID()
	ifaceUUID := buildNamedUUID()

	ovs := vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{bridgeUUID}}
	bridge := vswitchd.Bridge{UUID: bridgeUUID, Name: "br-int", Ports: []string{portUUID}}
	port := vswitchd.Port{UUID: portUUID, Name: "test-port", Interfaces: []string{ifaceUUID}}
	iface := vswitchd.Interface{UUID: ifaceUUID, Name: "test-port", Type: "internal"}

	tests := []struct {
		desc       string
		portName   string
		expectErr  bool
		initialOvs libovsdbtest.TestSetup
	}{
		{
			desc:      "returns existing port",
			portName:  "test-port",
			expectErr: false,
			initialOvs: libovsdbtest.TestSetup{
				OVSData: []libovsdbtest.TestData{
					ovs.DeepCopy(), bridge.DeepCopy(), port.DeepCopy(), iface.DeepCopy(),
				},
			},
		},
		{
			desc:      "returns error for non-existent port",
			portName:  "no-such-port",
			expectErr: true,
			initialOvs: libovsdbtest.TestSetup{
				OVSData: []libovsdbtest.TestData{
					&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{bridgeUUID}},
					&vswitchd.Bridge{UUID: bridgeUUID, Name: "br-int"},
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			ovsClient, cleanup, err := libovsdbtest.NewOVSTestHarness(tt.initialOvs)
			if err != nil {
				t.Fatalf("test: \"%s\" failed to set up test harness: %v", tt.desc, err)
			}
			t.Cleanup(cleanup.Cleanup)

			got, err := GetOVSPort(ovsClient, tt.portName)
			if err != nil && !tt.expectErr {
				t.Fatal(fmt.Errorf("GetOVSPort() error = %v", err))
			}
			if err == nil && tt.expectErr {
				t.Fatal("expected error but got nil")
			}
			if !tt.expectErr && got.Name != tt.portName {
				t.Fatalf("expected port name %q, got %q", tt.portName, got.Name)
			}
		})
	}
}

func TestFindOVSPortsWithPredicate(t *testing.T) {
	bridgeUUID := buildNamedUUID()
	port1UUID := buildNamedUUID()
	port2UUID := buildNamedUUID()
	iface1UUID := buildNamedUUID()
	iface2UUID := buildNamedUUID()

	ovs := vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{bridgeUUID}}
	bridge := vswitchd.Bridge{UUID: bridgeUUID, Name: "br-int", Ports: []string{port1UUID, port2UUID}}
	port1 := vswitchd.Port{UUID: port1UUID, Name: "evpn-port", ExternalIDs: map[string]string{"evpn-vtep": "vtep1"}, Interfaces: []string{iface1UUID}}
	port2 := vswitchd.Port{UUID: port2UUID, Name: "other-port", ExternalIDs: map[string]string{"other": "val"}, Interfaces: []string{iface2UUID}}
	iface1 := vswitchd.Interface{UUID: iface1UUID, Name: "evpn-port", Type: "internal"}
	iface2 := vswitchd.Interface{UUID: iface2UUID, Name: "other-port", Type: "internal"}

	tests := []struct {
		desc          string
		predicate     ovsPortPredicate
		expectedCount int
		initialOvs    libovsdbtest.TestSetup
	}{
		{
			desc:          "finds ports matching predicate",
			predicate:     func(p *vswitchd.Port) bool { return p.ExternalIDs["evpn-vtep"] == "vtep1" },
			expectedCount: 1,
			initialOvs: libovsdbtest.TestSetup{
				OVSData: []libovsdbtest.TestData{
					ovs.DeepCopy(), bridge.DeepCopy(),
					port1.DeepCopy(), port2.DeepCopy(),
					iface1.DeepCopy(), iface2.DeepCopy(),
				},
			},
		},
		{
			desc:          "returns empty for no matches",
			predicate:     func(_ *vswitchd.Port) bool { return false },
			expectedCount: 0,
			initialOvs: libovsdbtest.TestSetup{
				OVSData: []libovsdbtest.TestData{
					ovs.DeepCopy(), bridge.DeepCopy(),
					port1.DeepCopy(), port2.DeepCopy(),
					iface1.DeepCopy(), iface2.DeepCopy(),
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			ovsClient, cleanup, err := libovsdbtest.NewOVSTestHarness(tt.initialOvs)
			if err != nil {
				t.Fatalf("test: \"%s\" failed to set up test harness: %v", tt.desc, err)
			}
			t.Cleanup(cleanup.Cleanup)

			ports, err := FindOVSPortsWithPredicate(ovsClient, tt.predicate)
			if err != nil {
				t.Fatal(fmt.Errorf("FindOVSPortsWithPredicate() error = %v", err))
			}
			if len(ports) != tt.expectedCount {
				t.Fatalf("expected %d ports, got %d", tt.expectedCount, len(ports))
			}
		})
	}
}

func TestCreateOrUpdatePortWithInterface(t *testing.T) {
	bridgeUUID := buildNamedUUID()

	ovs := vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{bridgeUUID}}
	bridge := vswitchd.Bridge{UUID: bridgeUUID, Name: "br-int"}

	tests := []struct {
		desc              string
		bridgeName        string
		portName          string
		portExternalIDs   map[string]string
		expectErr         bool
		initialOvs        libovsdbtest.TestSetup
		verifyExternalIDs map[string]string
	}{
		{
			desc:            "creates new port and interface",
			bridgeName:      "br-int",
			portName:        "new-port",
			portExternalIDs: map[string]string{"evpn-vtep": "vtep1"},
			expectErr:       false,
			initialOvs: libovsdbtest.TestSetup{
				OVSData: []libovsdbtest.TestData{
					ovs.DeepCopy(), bridge.DeepCopy(),
				},
			},
			verifyExternalIDs: map[string]string{"evpn-vtep": "vtep1"},
		},
		{
			desc:            "updates existing port external IDs",
			bridgeName:      "br-int",
			portName:        "upd-port",
			portExternalIDs: map[string]string{"k": "v2"},
			expectErr:       false,
			initialOvs: libovsdbtest.TestSetup{
				OVSData: []libovsdbtest.TestData{
					ovs.DeepCopy(), bridge.DeepCopy(),
				},
			},
			verifyExternalIDs: map[string]string{"k": "v2"},
		},
		{
			desc:            "fails for non-existent bridge",
			bridgeName:      "no-bridge",
			portName:        "port",
			portExternalIDs: nil,
			expectErr:       true,
			initialOvs: libovsdbtest.TestSetup{
				OVSData: []libovsdbtest.TestData{
					ovs.DeepCopy(), bridge.DeepCopy(),
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			ovsClient, cleanup, err := libovsdbtest.NewOVSTestHarness(tt.initialOvs)
			if err != nil {
				t.Fatalf("test: \"%s\" failed to set up test harness: %v", tt.desc, err)
			}
			t.Cleanup(cleanup.Cleanup)

			// For update test, create the port first with initial external IDs
			if tt.desc == "updates existing port external IDs" {
				err = CreateOrUpdatePortWithInterface(ovsClient, tt.bridgeName, tt.portName, map[string]string{"k": "v1"}, nil)
				if err != nil {
					t.Fatalf("setup create failed: %v", err)
				}
			}

			err = CreateOrUpdatePortWithInterface(ovsClient, tt.bridgeName, tt.portName, tt.portExternalIDs, nil)
			if err != nil && !tt.expectErr {
				t.Fatal(fmt.Errorf("CreateOrUpdatePortWithInterface() error = %v", err))
			}
			if err == nil && tt.expectErr {
				t.Fatal("expected error but got nil")
			}

			if !tt.expectErr {
				got, err := GetOVSPort(ovsClient, tt.portName)
				if err != nil {
					t.Fatalf("port not found after create: %v", err)
				}
				for k, v := range tt.verifyExternalIDs {
					if got.ExternalIDs[k] != v {
						t.Fatalf("expected external ID %q=%q, got %v", k, v, got.ExternalIDs)
					}
				}
			}
		})
	}
}

func TestDeletePortWithInterfaces(t *testing.T) {
	bridgeUUID := buildNamedUUID()
	portUUID := buildNamedUUID()
	ifaceUUID := buildNamedUUID()

	ovs := vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{bridgeUUID}}
	bridge := vswitchd.Bridge{UUID: bridgeUUID, Name: "br-int", Ports: []string{portUUID}}
	port := vswitchd.Port{UUID: portUUID, Name: "del-port", Interfaces: []string{ifaceUUID}}
	iface := vswitchd.Interface{UUID: ifaceUUID, Name: "del-port", Type: "internal"}

	tests := []struct {
		desc        string
		portName    string
		expectErr   bool
		initialOvs  libovsdbtest.TestSetup
		expectedOvs libovsdbtest.TestSetup
	}{
		{
			desc:      "deletes existing port and interface",
			portName:  "del-port",
			expectErr: false,
			initialOvs: libovsdbtest.TestSetup{
				OVSData: []libovsdbtest.TestData{
					ovs.DeepCopy(), bridge.DeepCopy(), port.DeepCopy(), iface.DeepCopy(),
				},
			},
			expectedOvs: libovsdbtest.TestSetup{
				OVSData: []libovsdbtest.TestData{
					ovs.DeepCopy(),
					&vswitchd.Bridge{UUID: bridgeUUID, Name: "br-int"},
				},
			},
		},
		{
			desc:      "is idempotent for non-existent port",
			portName:  "no-such-port",
			expectErr: false,
			initialOvs: libovsdbtest.TestSetup{
				OVSData: []libovsdbtest.TestData{
					&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{bridgeUUID}},
					&vswitchd.Bridge{UUID: bridgeUUID, Name: "br-int"},
				},
			},
			expectedOvs: libovsdbtest.TestSetup{
				OVSData: []libovsdbtest.TestData{
					&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{bridgeUUID}},
					&vswitchd.Bridge{UUID: bridgeUUID, Name: "br-int"},
				},
			},
		},
		{
			desc:      "resolves an interface name to its containing port",
			portName:  "del-iface",
			expectErr: false,
			initialOvs: libovsdbtest.TestSetup{
				OVSData: []libovsdbtest.TestData{
					ovs.DeepCopy(), bridge.DeepCopy(),
					&vswitchd.Port{UUID: portUUID, Name: "bond0", Interfaces: []string{ifaceUUID}},
					&vswitchd.Interface{UUID: ifaceUUID, Name: "del-iface", Type: "system"},
				},
			},
			expectedOvs: libovsdbtest.TestSetup{
				OVSData: []libovsdbtest.TestData{
					ovs.DeepCopy(),
					&vswitchd.Bridge{UUID: bridgeUUID, Name: "br-int"},
				},
			},
		},
		{
			desc:      "is a no-op when the port lives on a different bridge",
			portName:  "del-port",
			expectErr: false,
			initialOvs: libovsdbtest.TestSetup{
				OVSData: []libovsdbtest.TestData{
					&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{bridgeUUID, "other-bridge-uuid"}},
					&vswitchd.Bridge{UUID: bridgeUUID, Name: "br-int"},
					&vswitchd.Bridge{UUID: "other-bridge-uuid", Name: "br-other", Ports: []string{portUUID}},
					port.DeepCopy(), iface.DeepCopy(),
				},
			},
			expectedOvs: libovsdbtest.TestSetup{
				OVSData: []libovsdbtest.TestData{
					&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{bridgeUUID, "other-bridge-uuid"}},
					&vswitchd.Bridge{UUID: bridgeUUID, Name: "br-int"},
					&vswitchd.Bridge{UUID: "other-bridge-uuid", Name: "br-other", Ports: []string{portUUID}},
					port.DeepCopy(), iface.DeepCopy(),
				},
			},
		},
		{
			desc:      "is a no-op when the named bridge does not exist",
			portName:  "del-port",
			expectErr: false,
			initialOvs: libovsdbtest.TestSetup{
				OVSData: []libovsdbtest.TestData{
					&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{"other-bridge-uuid"}},
					&vswitchd.Bridge{UUID: "other-bridge-uuid", Name: "br-other", Ports: []string{portUUID}},
					port.DeepCopy(), iface.DeepCopy(),
				},
			},
			expectedOvs: libovsdbtest.TestSetup{
				OVSData: []libovsdbtest.TestData{
					&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{"other-bridge-uuid"}},
					&vswitchd.Bridge{UUID: "other-bridge-uuid", Name: "br-other", Ports: []string{portUUID}},
					port.DeepCopy(), iface.DeepCopy(),
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			ovsClient, cleanup, err := libovsdbtest.NewOVSTestHarness(tt.initialOvs)
			if err != nil {
				t.Fatalf("test: \"%s\" failed to set up test harness: %v", tt.desc, err)
			}
			t.Cleanup(cleanup.Cleanup)

			err = DeletePortWithInterfaces(ovsClient, "br-int", tt.portName)
			if err != nil && !tt.expectErr {
				t.Fatal(fmt.Errorf("DeletePortWithInterfaces() error = %v", err))
			}
			if err == nil && tt.expectErr {
				t.Fatal("expected error but got nil")
			}

			matcher := libovsdbtest.HaveData(tt.expectedOvs.OVSData)
			success, err := matcher.Match(ovsClient)
			if !success {
				t.Fatal(fmt.Errorf("test: \"%s\" didn't match expected with actual, err: %v", tt.desc, matcher.FailureMessage(ovsClient)))
			}
			if err != nil {
				t.Fatal(fmt.Errorf("test: \"%s\" encountered error: %v", tt.desc, err))
			}
		})
	}
}

func TestDeleteMultiplePortsWithInterfaces(t *testing.T) {
	bridgeUUID := buildNamedUUID()
	otherBridgeUUID := buildNamedUUID()
	port1UUID := buildNamedUUID()
	port2UUID := buildNamedUUID()
	iface1UUID := buildNamedUUID()
	iface2UUID := buildNamedUUID()
	otherPortUUID := buildNamedUUID()
	otherIfaceUUID := buildNamedUUID()

	port1 := vswitchd.Port{UUID: port1UUID, Name: "del-port-1", Interfaces: []string{iface1UUID}}
	port2 := vswitchd.Port{UUID: port2UUID, Name: "del-port-2", Interfaces: []string{iface2UUID}}
	iface1 := vswitchd.Interface{UUID: iface1UUID, Name: "del-port-1", Type: "internal"}
	iface2 := vswitchd.Interface{UUID: iface2UUID, Name: "del-port-2", Type: "internal"}
	otherPort := vswitchd.Port{UUID: otherPortUUID, Name: "other-port", Interfaces: []string{otherIfaceUUID}}
	otherIface := vswitchd.Interface{UUID: otherIfaceUUID, Name: "other-port", Type: "internal"}

	tests := []struct {
		desc        string
		portNames   []string
		expectErr   bool
		initialOvs  libovsdbtest.TestSetup
		expectedOvs libovsdbtest.TestSetup
	}{
		{
			desc:      "deletes multiple ports from the bridge in one transaction",
			portNames: []string{"del-port-1", "del-port-2"},
			expectErr: false,
			initialOvs: libovsdbtest.TestSetup{
				OVSData: []libovsdbtest.TestData{
					&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{bridgeUUID}},
					&vswitchd.Bridge{UUID: bridgeUUID, Name: "br-int", Ports: []string{port1UUID, port2UUID}},
					port1.DeepCopy(), port2.DeepCopy(), iface1.DeepCopy(), iface2.DeepCopy(),
				},
			},
			expectedOvs: libovsdbtest.TestSetup{
				OVSData: []libovsdbtest.TestData{
					&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{bridgeUUID}},
					&vswitchd.Bridge{UUID: bridgeUUID, Name: "br-int"},
				},
			},
		},
		{
			desc:      "is idempotent for missing ports and deletes existing ones",
			portNames: []string{"del-port-1", "no-such-port"},
			expectErr: false,
			initialOvs: libovsdbtest.TestSetup{
				OVSData: []libovsdbtest.TestData{
					&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{bridgeUUID}},
					&vswitchd.Bridge{UUID: bridgeUUID, Name: "br-int", Ports: []string{port1UUID}},
					port1.DeepCopy(), iface1.DeepCopy(),
				},
			},
			expectedOvs: libovsdbtest.TestSetup{
				OVSData: []libovsdbtest.TestData{
					&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{bridgeUUID}},
					&vswitchd.Bridge{UUID: bridgeUUID, Name: "br-int"},
				},
			},
		},
		{
			desc:      "skips ports attached to another bridge",
			portNames: []string{"del-port-1", "other-port"},
			expectErr: false,
			initialOvs: libovsdbtest.TestSetup{
				OVSData: []libovsdbtest.TestData{
					&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{bridgeUUID, otherBridgeUUID}},
					&vswitchd.Bridge{UUID: bridgeUUID, Name: "br-int", Ports: []string{port1UUID}},
					&vswitchd.Bridge{UUID: otherBridgeUUID, Name: "br-other", Ports: []string{otherPortUUID}},
					port1.DeepCopy(), iface1.DeepCopy(), otherPort.DeepCopy(), otherIface.DeepCopy(),
				},
			},
			expectedOvs: libovsdbtest.TestSetup{
				OVSData: []libovsdbtest.TestData{
					&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{bridgeUUID, otherBridgeUUID}},
					&vswitchd.Bridge{UUID: bridgeUUID, Name: "br-int"},
					&vswitchd.Bridge{UUID: otherBridgeUUID, Name: "br-other", Ports: []string{otherPortUUID}},
					otherPort.DeepCopy(), otherIface.DeepCopy(),
				},
			},
		},
		{
			desc:      "deduplicates repeated port names",
			portNames: []string{"del-port-1", "del-port-2", "del-port-1"},
			expectErr: false,
			initialOvs: libovsdbtest.TestSetup{
				OVSData: []libovsdbtest.TestData{
					&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{bridgeUUID}},
					&vswitchd.Bridge{UUID: bridgeUUID, Name: "br-int", Ports: []string{port1UUID, port2UUID}},
					port1.DeepCopy(), port2.DeepCopy(), iface1.DeepCopy(), iface2.DeepCopy(),
				},
			},
			expectedOvs: libovsdbtest.TestSetup{
				OVSData: []libovsdbtest.TestData{
					&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{bridgeUUID}},
					&vswitchd.Bridge{UUID: bridgeUUID, Name: "br-int"},
				},
			},
		},
		{
			desc:      "is a no-op when the named bridge does not exist",
			portNames: []string{"del-port-1"},
			expectErr: false,
			initialOvs: libovsdbtest.TestSetup{
				OVSData: []libovsdbtest.TestData{
					&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{otherBridgeUUID}},
					&vswitchd.Bridge{UUID: otherBridgeUUID, Name: "br-other", Ports: []string{otherPortUUID}},
					otherPort.DeepCopy(), otherIface.DeepCopy(),
				},
			},
			expectedOvs: libovsdbtest.TestSetup{
				OVSData: []libovsdbtest.TestData{
					&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{otherBridgeUUID}},
					&vswitchd.Bridge{UUID: otherBridgeUUID, Name: "br-other", Ports: []string{otherPortUUID}},
					otherPort.DeepCopy(), otherIface.DeepCopy(),
				},
			},
		},
		{
			desc:      "is a no-op when all requested ports are missing",
			portNames: []string{"no-such-port-1", "no-such-port-2"},
			expectErr: false,
			initialOvs: libovsdbtest.TestSetup{
				OVSData: []libovsdbtest.TestData{
					&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{bridgeUUID}},
					&vswitchd.Bridge{UUID: bridgeUUID, Name: "br-int"},
				},
			},
			expectedOvs: libovsdbtest.TestSetup{
				OVSData: []libovsdbtest.TestData{
					&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{bridgeUUID}},
					&vswitchd.Bridge{UUID: bridgeUUID, Name: "br-int"},
				},
			},
		},
		{
			desc:      "is a no-op when no port names are provided",
			portNames: nil,
			expectErr: false,
			initialOvs: libovsdbtest.TestSetup{
				OVSData: []libovsdbtest.TestData{
					&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{bridgeUUID}},
					&vswitchd.Bridge{UUID: bridgeUUID, Name: "br-int", Ports: []string{port1UUID, port2UUID}},
					port1.DeepCopy(), port2.DeepCopy(), iface1.DeepCopy(), iface2.DeepCopy(),
				},
			},
			expectedOvs: libovsdbtest.TestSetup{
				OVSData: []libovsdbtest.TestData{
					&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{bridgeUUID}},
					&vswitchd.Bridge{UUID: bridgeUUID, Name: "br-int", Ports: []string{port1UUID, port2UUID}},
					port1.DeepCopy(), port2.DeepCopy(), iface1.DeepCopy(), iface2.DeepCopy(),
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			ovsClient, cleanup, err := libovsdbtest.NewOVSTestHarness(tt.initialOvs)
			if err != nil {
				t.Fatalf("test: \"%s\" failed to set up test harness: %v", tt.desc, err)
			}
			t.Cleanup(cleanup.Cleanup)

			err = DeleteMultiplePortsWithInterfaces(ovsClient, "br-int", tt.portNames...)
			if err != nil && !tt.expectErr {
				t.Fatal(fmt.Errorf("DeleteMultiplePortsWithInterfaces() error = %v", err))
			}
			if err == nil && tt.expectErr {
				t.Fatal("expected error but got nil")
			}

			matcher := libovsdbtest.HaveData(tt.expectedOvs.OVSData)
			success, err := matcher.Match(ovsClient)
			if !success {
				t.Fatal(fmt.Errorf("test: \"%s\" didn't match expected with actual, err: %v", tt.desc, matcher.FailureMessage(ovsClient)))
			}
			if err != nil {
				t.Fatal(fmt.Errorf("test: \"%s\" encountered error: %v", tt.desc, err))
			}
		})
	}
}

func TestDeleteBridge(t *testing.T) {
	otherBridgeUUID := buildNamedUUID()
	bridgeUUID := buildNamedUUID()
	port1UUID := buildNamedUUID()
	port2UUID := buildNamedUUID()
	iface1UUID := buildNamedUUID()
	iface2UUID := buildNamedUUID()
	sharedPortUUID := buildNamedUUID()
	sharedIfaceUUID := buildNamedUUID()

	otherBridge := vswitchd.Bridge{UUID: otherBridgeUUID, Name: "br-other"}
	ovs := vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{otherBridgeUUID, bridgeUUID}}
	bridge := vswitchd.Bridge{UUID: bridgeUUID, Name: "br-doomed", Ports: []string{port1UUID, port2UUID}}
	port1 := vswitchd.Port{UUID: port1UUID, Name: "p1", Interfaces: []string{iface1UUID}}
	port2 := vswitchd.Port{UUID: port2UUID, Name: "p2", Interfaces: []string{iface2UUID}}
	iface1 := vswitchd.Interface{UUID: iface1UUID, Name: "p1", Type: "internal"}
	iface2 := vswitchd.Interface{UUID: iface2UUID, Name: "p2", Type: "internal"}
	sharedPort := vswitchd.Port{UUID: sharedPortUUID, Name: "shared", Interfaces: []string{sharedIfaceUUID}}
	sharedIface := vswitchd.Interface{UUID: sharedIfaceUUID, Name: "shared", Type: "internal"}

	tests := []struct {
		desc        string
		bridgeName  string
		expectErr   bool
		initialOvs  libovsdbtest.TestSetup
		expectedOvs libovsdbtest.TestSetup
	}{
		{
			desc:       "deletes bridge with all ports and interfaces",
			bridgeName: "br-doomed",
			initialOvs: libovsdbtest.TestSetup{
				OVSData: []libovsdbtest.TestData{
					ovs.DeepCopy(), otherBridge.DeepCopy(), bridge.DeepCopy(),
					port1.DeepCopy(), port2.DeepCopy(),
					iface1.DeepCopy(), iface2.DeepCopy(),
				},
			},
			expectedOvs: libovsdbtest.TestSetup{
				OVSData: []libovsdbtest.TestData{
					&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{otherBridgeUUID}},
					otherBridge.DeepCopy(),
				},
			},
		},
		{
			desc:       "is idempotent for a non-existent bridge",
			bridgeName: "no-such-bridge",
			initialOvs: libovsdbtest.TestSetup{
				OVSData: []libovsdbtest.TestData{
					&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{otherBridgeUUID}},
					otherBridge.DeepCopy(),
				},
			},
			expectedOvs: libovsdbtest.TestSetup{
				OVSData: []libovsdbtest.TestData{
					&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{otherBridgeUUID}},
					otherBridge.DeepCopy(),
				},
			},
		},
		{
			desc:       "preserves a port that is still referenced by another bridge",
			bridgeName: "br-doomed",
			initialOvs: libovsdbtest.TestSetup{
				OVSData: []libovsdbtest.TestData{
					ovs.DeepCopy(),
					&vswitchd.Bridge{UUID: otherBridgeUUID, Name: "br-other", Ports: []string{sharedPortUUID}},
					&vswitchd.Bridge{UUID: bridgeUUID, Name: "br-doomed", Ports: []string{sharedPortUUID}},
					sharedPort.DeepCopy(),
					sharedIface.DeepCopy(),
				},
			},
			expectedOvs: libovsdbtest.TestSetup{
				OVSData: []libovsdbtest.TestData{
					&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{otherBridgeUUID}},
					&vswitchd.Bridge{UUID: otherBridgeUUID, Name: "br-other", Ports: []string{sharedPortUUID}},
					sharedPort.DeepCopy(),
					sharedIface.DeepCopy(),
				},
			},
		},
		{
			desc:       "deletes empty bridge (no ports)",
			bridgeName: "br-empty",
			initialOvs: libovsdbtest.TestSetup{
				OVSData: []libovsdbtest.TestData{
					&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{bridgeUUID}},
					&vswitchd.Bridge{UUID: bridgeUUID, Name: "br-empty"},
				},
			},
			expectedOvs: libovsdbtest.TestSetup{
				OVSData: []libovsdbtest.TestData{
					&vswitchd.OpenvSwitch{UUID: "root-ovs"},
				},
			},
		},
		{
			desc:       "deletes flow sample collectors that reference the bridge",
			bridgeName: "br-doomed",
			initialOvs: libovsdbtest.TestSetup{
				OVSData: []libovsdbtest.TestData{
					ovs.DeepCopy(), otherBridge.DeepCopy(), bridge.DeepCopy(),
					port1.DeepCopy(), port2.DeepCopy(), iface1.DeepCopy(), iface2.DeepCopy(),
					&vswitchd.FlowSampleCollectorSet{UUID: buildNamedUUID(), ID: 42, Bridge: bridgeUUID},
					&vswitchd.FlowSampleCollectorSet{UUID: "other-collector", ID: 43, Bridge: otherBridgeUUID},
				},
			},
			expectedOvs: libovsdbtest.TestSetup{
				OVSData: []libovsdbtest.TestData{
					&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{otherBridgeUUID}},
					otherBridge.DeepCopy(),
					&vswitchd.FlowSampleCollectorSet{UUID: "other-collector", ID: 43, Bridge: otherBridgeUUID},
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			ovsClient, cleanup, err := libovsdbtest.NewOVSTestHarness(tt.initialOvs)
			if err != nil {
				t.Fatalf("test: \"%s\" failed to set up test harness: %v", tt.desc, err)
			}
			t.Cleanup(cleanup.Cleanup)

			// The production client deletes collectors without caching them.
			// Monitor them here so HaveData also checks the surviving collector.
			_, err = ovsClient.Monitor(context.Background(), ovsClient.NewMonitor(
				libovsdbclient.WithTable(&vswitchd.FlowSampleCollectorSet{})))
			if err != nil {
				t.Fatalf("monitor flow sample collectors: %v", err)
			}

			err = DeleteBridge(ovsClient, tt.bridgeName)
			if err != nil && !tt.expectErr {
				t.Fatal(fmt.Errorf("DeleteBridge() error = %v", err))
			}
			if err == nil && tt.expectErr {
				t.Fatal("expected error but got nil")
			}

			matcher := libovsdbtest.HaveData(tt.expectedOvs.OVSData)
			success, err := matcher.Match(ovsClient)
			if !success {
				t.Fatal(fmt.Errorf("test: \"%s\" didn't match expected with actual, err: %v", tt.desc, matcher.FailureMessage(ovsClient)))
			}
			if err != nil {
				t.Fatal(fmt.Errorf("test: \"%s\" encountered error: %v", tt.desc, err))
			}
		})
	}
}

func TestUpdateOpenvSwitchExternalIDs(t *testing.T) {
	tests := []struct {
		desc                string
		initialExternalIDs  map[string]string
		update              map[string]string
		expectedExternalIDs map[string]string
		setupNoOpenvSwitch  bool
		expectErrIs         error
	}{
		{
			desc:                "sets a new key on empty external_ids",
			initialExternalIDs:  nil,
			update:              map[string]string{"ovn-encap-ip": "10.0.0.1"},
			expectedExternalIDs: map[string]string{"ovn-encap-ip": "10.0.0.1"},
		},
		{
			desc:                "overwrites an existing key, preserves unrelated keys",
			initialExternalIDs:  map[string]string{"ovn-encap-ip": "10.0.0.1", "system-id": "node-a"},
			update:              map[string]string{"ovn-encap-ip": "10.0.0.2"},
			expectedExternalIDs: map[string]string{"ovn-encap-ip": "10.0.0.2", "system-id": "node-a"},
		},
		{
			desc:                "no-op for empty update",
			initialExternalIDs:  map[string]string{"system-id": "node-a"},
			update:              nil,
			expectedExternalIDs: map[string]string{"system-id": "node-a"},
		},
		{
			desc:               "returns ErrNotFound when no Open_vSwitch row exists",
			update:             map[string]string{"ovn-encap-ip": "10.0.0.1"},
			setupNoOpenvSwitch: true,
			expectErrIs:        libovsdbclient.ErrNotFound,
		},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			setup := libovsdbtest.TestSetup{}
			if !tt.setupNoOpenvSwitch {
				setup.OVSData = []libovsdbtest.TestData{
					&vswitchd.OpenvSwitch{UUID: "root-ovs", ExternalIDs: tt.initialExternalIDs},
				}
			}

			ovsClient, cleanup, err := libovsdbtest.NewOVSTestHarness(setup)
			if err != nil {
				t.Fatalf("failed to set up test harness: %v", err)
			}
			t.Cleanup(cleanup.Cleanup)

			err = UpdateOpenvSwitchExternalIDs(ovsClient, tt.update)
			if tt.expectErrIs != nil {
				if !errors.Is(err, tt.expectErrIs) {
					t.Fatalf("expected error %v, got %v", tt.expectErrIs, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("UpdateOpenvSwitchExternalIDs() error = %v", err)
			}

			expected := []libovsdbtest.TestData{
				&vswitchd.OpenvSwitch{UUID: "root-ovs", ExternalIDs: tt.expectedExternalIDs},
			}
			matcher := libovsdbtest.HaveData(expected)
			success, err := matcher.Match(ovsClient)
			if !success {
				t.Fatalf("post-condition mismatch: %v", matcher.FailureMessage(ovsClient))
			}
			if err != nil {
				t.Fatalf("matcher encountered error: %v", err)
			}
		})
	}
}

func TestRemoveOpenvSwitchExternalIDs(t *testing.T) {
	tests := []struct {
		desc                string
		initialExternalIDs  map[string]string
		removeKeys          []string
		expectedExternalIDs map[string]string
		setupNoOpenvSwitch  bool
	}{
		{
			desc:                "removes a single existing key, preserves unrelated keys",
			initialExternalIDs:  map[string]string{"ovn-bridge-mappings": "physnet1:br1", "system-id": "node-a"},
			removeKeys:          []string{"ovn-bridge-mappings"},
			expectedExternalIDs: map[string]string{"system-id": "node-a"},
		},
		{
			desc:                "removes multiple keys",
			initialExternalIDs:  map[string]string{"a": "1", "b": "2", "c": "3"},
			removeKeys:          []string{"a", "c"},
			expectedExternalIDs: map[string]string{"b": "2"},
		},
		{
			desc:                "removing a non-existent key is a no-op",
			initialExternalIDs:  map[string]string{"system-id": "node-a"},
			removeKeys:          []string{"ovn-bridge-mappings"},
			expectedExternalIDs: map[string]string{"system-id": "node-a"},
		},
		{
			desc:                "no-op for empty key list",
			initialExternalIDs:  map[string]string{"system-id": "node-a"},
			removeKeys:          nil,
			expectedExternalIDs: map[string]string{"system-id": "node-a"},
		},
		{
			desc:               "missing Open_vSwitch row is not an error (matches --if-exists)",
			removeKeys:         []string{"ovn-bridge-mappings"},
			setupNoOpenvSwitch: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			setup := libovsdbtest.TestSetup{}
			if !tt.setupNoOpenvSwitch {
				setup.OVSData = []libovsdbtest.TestData{
					&vswitchd.OpenvSwitch{UUID: "root-ovs", ExternalIDs: tt.initialExternalIDs},
				}
			}

			ovsClient, cleanup, err := libovsdbtest.NewOVSTestHarness(setup)
			if err != nil {
				t.Fatalf("failed to set up test harness: %v", err)
			}
			t.Cleanup(cleanup.Cleanup)

			if err := RemoveOpenvSwitchExternalIDs(ovsClient, tt.removeKeys...); err != nil {
				t.Fatalf("RemoveOpenvSwitchExternalIDs() error = %v", err)
			}

			if tt.setupNoOpenvSwitch {
				return
			}
			expected := []libovsdbtest.TestData{
				&vswitchd.OpenvSwitch{UUID: "root-ovs", ExternalIDs: tt.expectedExternalIDs},
			}
			matcher := libovsdbtest.HaveData(expected)
			success, err := matcher.Match(ovsClient)
			if !success {
				t.Fatalf("post-condition mismatch: %v", matcher.FailureMessage(ovsClient))
			}
			if err != nil {
				t.Fatalf("matcher encountered error: %v", err)
			}
		})
	}
}
