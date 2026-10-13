// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ops

import (
	"context"
	"testing"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/sbdb"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
)

// Interleave another writer after operations have been built from the cache.
type chassisInterleavingClient struct {
	libovsdbclient.Client
	beforeTransact func()
}

func (c *chassisInterleavingClient) Transact(ctx context.Context, operations ...ovsdb.Operation) ([]ovsdb.OperationResult, error) {
	c.beforeTransact()
	return c.Client.Transact(ctx, operations...)
}

func TestChassisOwnershipTransactionGuards(t *testing.T) {
	for _, action := range []string{"create", "update", "delete", "adopt"} {
		t.Run(action, func(t *testing.T) {
			initial := []libovsdbtest.TestData{}
			if action != "create" {
				initial = append(initial, &sbdb.Chassis{Name: "shared", Hostname: "node-a", ExternalIDs: map[string]string{ChassisNodeName: "node-a"}, OtherConfig: map[string]string{"is-remote": "true"}})
			}
			if action == "adopt" {
				// Keep a non-owner external ID: the in-memory server skips default
				// values when comparing wait rows, unlike an OVSDB server.
				initial = []libovsdbtest.TestData{&sbdb.Chassis{Name: "shared", Hostname: "dpu-hostname", ExternalIDs: map[string]string{"unrelated": "preserved"}}}
			}
			sb, cleanup, err := libovsdbtest.NewSBTestHarness(libovsdbtest.TestSetup{SBData: initial}, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(cleanup.Cleanup)
			intercepted := &chassisInterleavingClient{Client: sb, beforeTransact: func() {
				if action == "create" {
					// Both callers observed an unused chassis name, but node-b commits first.
					err := CreateOrUpdateChassis(sb, &sbdb.Chassis{Name: "shared", Hostname: "node-b", ExternalIDs: map[string]string{ChassisNodeName: "node-b"}, OtherConfig: map[string]string{"is-remote": "true"}}, &sbdb.Encap{ChassisName: "shared", IP: "192.0.2.20", Type: "geneve"})
					if err != nil {
						t.Fatal(err)
					}
				} else {
					// Simulate an ownership change after the caller checked its cached row.
					changed := &sbdb.Chassis{Name: "shared", ExternalIDs: map[string]string{ChassisNodeName: "node-b"}}
					operations, err := sb.Where(changed).Update(changed, &changed.ExternalIDs)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := TransactAndCheck(sb, operations); err != nil {
						t.Fatal(err)
					}
				}
			}}
			if action == "adopt" {
				err = CreateOrUpdateLocalChassis(intercepted, &sbdb.Chassis{Name: "shared", ExternalIDs: map[string]string{ChassisNodeName: "node-a"}, OtherConfig: map[string]string{"is-remote": "false"}}, "node-a", "shared", &sbdb.Encap{ChassisName: "shared", IP: "192.0.2.10", Type: "geneve"})
			} else if action == "delete" {
				err = DeleteChassisWithPredicate(intercepted, func(chassis *sbdb.Chassis) bool { return ChassisOwnedByNode(chassis, "node-a") })
			} else {
				err = CreateOrUpdateChassis(intercepted, &sbdb.Chassis{Name: "shared", Hostname: "node-a", ExternalIDs: map[string]string{ChassisNodeName: "node-a"}, OtherConfig: map[string]string{"is-remote": "true"}}, &sbdb.Encap{ChassisName: "shared", IP: "192.0.2.10", Type: "geneve"})
			}
			if err == nil {
				t.Fatal("stale transaction was accepted")
			}
			current, err := GetChassis(sb, &sbdb.Chassis{Name: "shared"})
			if err != nil {
				t.Fatal(err)
			}
			if current.ExternalIDs[ChassisNodeName] != "node-b" {
				t.Fatalf("owner changed: %+v", current)
			}
			var encaps []sbdb.Encap
			if err := sb.List(context.Background(), &encaps); err != nil {
				t.Fatal(err)
			}
			for _, encap := range encaps {
				if encap.IP == "192.0.2.10" {
					t.Fatal("failed transaction leaked attacker encap")
				}
			}
		})
	}
}

func TestAdoptLocalDPUChassis(t *testing.T) {
	sb, cleanup, err := libovsdbtest.NewSBTestHarness(libovsdbtest.TestSetup{SBData: []libovsdbtest.TestData{&sbdb.Chassis{Name: "dpu-chassis", Hostname: "dpu-hostname"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup.Cleanup)
	chassis := &sbdb.Chassis{Name: "dpu-chassis", ExternalIDs: map[string]string{ChassisNodeName: "kubernetes-node"}, OtherConfig: map[string]string{"is-remote": "false"}}
	if err := CreateOrUpdateChassis(sb, chassis); err == nil {
		t.Fatal("unverified local adoption succeeded")
	}
	for _, identity := range [][2]string{{"another-node", "dpu-chassis"}, {"kubernetes-node", "another-chassis"}, {"kubernetes-node", ""}} {
		if err := CreateOrUpdateLocalChassis(sb, chassis, identity[0], identity[1]); err == nil {
			t.Fatal("mismatched local identity accepted")
		}
	}
	if err := CreateOrUpdateLocalChassis(sb, chassis, "kubernetes-node", "dpu-chassis"); err != nil {
		t.Fatal(err)
	}
	for _, remote := range []string{"false", "true", "false"} {
		chassis := &sbdb.Chassis{Name: "dpu-chassis", ExternalIDs: map[string]string{ChassisNodeName: "kubernetes-node"}, OtherConfig: map[string]string{"is-remote": remote}}
		if err := CreateOrUpdateChassis(sb, chassis); err != nil {
			t.Fatal(err)
		}
		current, err := GetChassis(sb, &sbdb.Chassis{Name: "dpu-chassis"})
		if err != nil {
			t.Fatal(err)
		}
		if current.Hostname != "dpu-hostname" || current.ExternalIDs[ChassisNodeName] != "kubernetes-node" {
			t.Fatalf("DPU ownership lost: %+v", current)
		}
	}
}
