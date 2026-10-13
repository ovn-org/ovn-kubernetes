// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package zoneinterconnect

import (
	"context"
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	ops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/sbdb"
	dbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func TestChassisOwnership(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%t", legacy), func(t *testing.T) {
			if err := config.PrepareTestConfig(); err != nil {
				t.Fatal(err)
			}
			victim := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "victim", Annotations: map[string]string{util.OvnNodeChassisID: "shared-chassis", util.OVNNodeEncapIPs: `["192.0.2.10"]`}}}
			attacker := victim.DeepCopy()
			attacker.Name = "attacker"
			attacker.Annotations[util.OVNNodeEncapIPs] = `["192.0.2.20"]`
			initial := []dbtest.TestData{}
			if legacy {
				initial = append(initial, &sbdb.Chassis{UUID: "victim-chassis", Name: "shared-chassis", Hostname: "victim", OtherConfig: map[string]string{"is-remote": "true"}, Encaps: []string{"victim-encap"}}, &sbdb.Encap{UUID: "victim-encap", ChassisName: "shared-chassis", IP: "192.0.2.10", Type: "geneve"})
			}
			sb, cleanup, err := dbtest.NewSBTestHarness(dbtest.TestSetup{SBData: initial}, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(cleanup.Cleanup)
			nodes := []*corev1.Node{victim}
			handler := NewZoneChassisHandler(sb, "victim", func() ([]*corev1.Node, error) { return nodes, nil })
			if !legacy {
				if err := handler.AddRemoteZoneNode(victim); err != nil {
					t.Fatal(err)
				}
			}
			original, err := ops.GetChassis(sb, &sbdb.Chassis{Name: "shared-chassis"})
			if err != nil {
				t.Fatal(err)
			}
			// Check both the current node cache and a stale cache missing the victim.
			for _, visible := range [][]*corev1.Node{{victim, attacker}, {attacker}} {
				nodes = visible
				if err := handler.AddRemoteZoneNode(attacker); err == nil {
					t.Fatal("accepted conflicting remote chassis")
				}
				if err := handler.AddLocalZoneNode(attacker); err == nil {
					t.Fatal("accepted conflicting local chassis")
				}
				if err := handler.DeleteRemoteZoneNode(attacker); err != nil {
					t.Fatal(err)
				}
				noID := attacker.DeepCopy()
				delete(noID.Annotations, util.OvnNodeChassisID)
				if err := handler.DeleteRemoteZoneNode(noID); err != nil {
					t.Fatal(err)
				}
				current, err := ops.GetChassis(sb, &sbdb.Chassis{Name: "shared-chassis"})
				if err != nil {
					t.Fatal(err)
				}
				if !original.Equals(current) {
					t.Fatalf("victim chassis changed: %+v", current)
				}
				var encaps []sbdb.Encap
				if err := sb.List(context.Background(), &encaps); err != nil {
					t.Fatal(err)
				}
				if len(encaps) != 1 || encaps[0].IP != "192.0.2.10" {
					t.Fatalf("victim encap changed: %+v", encaps)
				}
			}
			nodes = []*corev1.Node{victim}
			// Owner updates and local/remote transitions must keep working after adoption.
			victim.Annotations[util.OVNNodeEncapIPs] = `["192.0.2.11","2001:db8::11"]`
			if err := handler.AddLocalZoneNode(victim); err != nil {
				t.Fatal(err)
			}
			if err := handler.DeleteRemoteZoneNode(victim); err != nil {
				t.Fatal(err)
			}
			if _, err := ops.GetChassis(sb, &sbdb.Chassis{Name: "shared-chassis"}); err != nil {
				t.Fatal("deleted local chassis", err)
			}
			if err := handler.AddRemoteZoneNode(victim); err != nil {
				t.Fatal(err)
			}
			current, err := ops.GetChassis(sb, &sbdb.Chassis{Name: "shared-chassis"})
			if err != nil {
				t.Fatal(err)
			}
			if current.ExternalIDs[ops.ChassisNodeName] != "victim" || len(current.Encaps) != 2 {
				t.Fatalf("owner update failed: %+v", current)
			}
			// Recreating the same Kubernetes node name keeps its chassis association.
			victim.UID = "recreated-node"
			if err := handler.AddRemoteZoneNode(victim); err != nil {
				t.Fatal(err)
			}
			if err := handler.DeleteRemoteZoneNode(victim); err != nil {
				t.Fatal(err)
			}
			if _, err := ops.GetChassis(sb, &sbdb.Chassis{Name: "shared-chassis"}); err == nil {
				t.Fatal("owner failed to delete chassis")
			}
		})
	}
}

func TestLocalChassisAdoptionIdentity(t *testing.T) {
	for _, tt := range []struct {
		name           string
		controllerNode string
		ovsID          string
		ovsError       error
		remote         bool
		wantError      bool
		wantOVSRead    bool
	}{
		{name: "verified DPU", controllerNode: "node", ovsID: "dpu-chassis", wantOVSRead: true},
		{name: "different controller node", controllerNode: "other", ovsID: "dpu-chassis", wantError: true},
		{name: "different OVS identity", controllerNode: "node", ovsID: "other-chassis", wantError: true, wantOVSRead: true},
		{name: "empty OVS identity", controllerNode: "node", wantError: true, wantOVSRead: true},
		{name: "OVS lookup fails", controllerNode: "node", ovsError: fmt.Errorf("OVS unavailable"), wantError: true, wantOVSRead: true},
		{name: "remote cannot adopt", controllerNode: "node", ovsID: "dpu-chassis", remote: true, wantError: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := config.PrepareTestConfig(); err != nil {
				t.Fatal(err)
			}
			sb, cleanup, err := dbtest.NewSBTestHarness(dbtest.TestSetup{SBData: []dbtest.TestData{
				&sbdb.Chassis{UUID: "dpu", Name: "dpu-chassis", Hostname: "dpu-hostname", Encaps: []string{"encap"}},
				&sbdb.Encap{UUID: "encap", ChassisName: "dpu-chassis", IP: "192.0.2.10", Type: "geneve"},
			}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(cleanup.Cleanup)
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node", Annotations: map[string]string{util.OvnNodeChassisID: "dpu-chassis", util.OVNNodeEncapIPs: `["192.0.2.20"]`}}}
			handler := NewZoneChassisHandler(sb, tt.controllerNode, func() ([]*corev1.Node, error) { return []*corev1.Node{node}, nil })
			ovsRead := false
			handler.getLocalChassisID = func() (string, error) { ovsRead = true; return tt.ovsID, tt.ovsError }
			original, err := ops.GetChassis(sb, &sbdb.Chassis{Name: "dpu-chassis"})
			if err != nil {
				t.Fatal(err)
			}
			if tt.remote {
				err = handler.AddRemoteZoneNode(node)
			} else {
				err = handler.AddLocalZoneNode(node)
			}
			if (err != nil) != tt.wantError {
				t.Fatalf("error = %v, wantError %t", err, tt.wantError)
			}
			if ovsRead != tt.wantOVSRead {
				t.Fatalf("OVS lookup = %t, want %t", ovsRead, tt.wantOVSRead)
			}
			current, err := ops.GetChassis(sb, &sbdb.Chassis{Name: "dpu-chassis"})
			if err != nil {
				t.Fatal(err)
			}
			if tt.wantError {
				if !original.Equals(current) {
					t.Fatal("rejected adoption modified chassis")
				}
				var encaps []sbdb.Encap
				if err := sb.List(context.Background(), &encaps); err != nil {
					t.Fatal(err)
				}
				if len(encaps) != 1 || encaps[0].IP != "192.0.2.10" {
					t.Fatal("rejected adoption modified encaps")
				}
			} else if current.ExternalIDs[ops.ChassisNodeName] != node.Name || current.Hostname != "dpu-hostname" {
				t.Fatalf("incorrect DPU ownership: %+v", current)
			}
		})
	}
}
