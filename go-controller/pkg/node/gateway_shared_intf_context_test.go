// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package node

import (
	"context"
	"errors"
	"fmt"
	"testing"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	libovsdbmodel "github.com/ovn-kubernetes/libovsdb/model"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	ovsops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
)

type gatewayCleanupWaitClient struct {
	libovsdbclient.Client
	cancel           context.CancelFunc
	applied          bool
	failLookupAt     int
	interfaceLookups int
	failedInterface  string
	lookupErr        error
}

func (c *gatewayCleanupWaitClient) WhereCacheByUUIDs(predicate any, uuids ...string) libovsdbclient.ConditionalAPI {
	conditional := c.Client.WhereCacheByUUIDs(predicate, uuids...)
	c.interfaceLookups++
	if c.interfaceLookups == c.failLookupAt {
		c.failedInterface = uuids[0]
		return &gatewayCleanupLookupFailure{ConditionalAPI: conditional, err: c.lookupErr}
	}
	return conditional
}

type gatewayCleanupLookupFailure struct {
	libovsdbclient.ConditionalAPI
	err error
}

func (c *gatewayCleanupLookupFailure) List(context.Context, any) error {
	return c.err
}

func (c *gatewayCleanupWaitClient) Get(ctx context.Context, model libovsdbmodel.Model) error {
	if c.cancel != nil {
		// The first acknowledgement read happens after the deletion committed.
		c.cancel()
		return ctx.Err()
	}
	if err := c.Client.Get(ctx, model); err != nil {
		return err
	}
	ovs := model.(*vswitchd.OpenvSwitch)
	ovs.CurCfg = ovs.NextCfg
	c.applied = true
	return nil
}

func TestCleanupSharedGatewayContinuesAfterLookupFailure(t *testing.T) {
	if err := config.PrepareTestConfig(); err != nil {
		t.Fatalf("prepare config: %v", err)
	}
	t.Cleanup(func() { _ = config.PrepareTestConfig() })
	config.OvnKubeNode.Mode = types.NodeModeFull
	bridge := &vswitchd.Bridge{UUID: "bridge", Name: "br-ex"}
	data := []libovsdbtest.TestData{
		&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{bridge.UUID},
			ExternalIDs: map[string]string{"ovn-bridge-mappings": types.PhysicalNetworkName + ":br-ex"}},
		bridge,
	}
	for i := 0; i < 3; i++ {
		name := fmt.Sprintf("patch-%d", i)
		bridge.Ports = append(bridge.Ports, name)
		data = append(data,
			&vswitchd.Port{UUID: name, Name: name, Interfaces: []string{name + "-iface"},
				ExternalIDs: map[string]string{"ovn-localnet-port": name}},
			&vswitchd.Interface{UUID: name + "-iface", Name: name, Type: "patch"})
	}
	ovsClient, cleanup, err := libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{OVSData: data})
	if err != nil {
		t.Fatalf("harness setup: %v", err)
	}
	t.Cleanup(cleanup.Cleanup)
	fexec := ovntest.NewFakeExec()
	if err := util.SetExec(fexec); err != nil {
		t.Fatalf("set fake exec: %v", err)
	}
	t.Cleanup(util.ResetRunner)

	lookupErr := errors.New("injected interface lookup failure")
	c := &gatewayCleanupWaitClient{Client: ovsClient, failLookupAt: 2, lookupErr: lookupErr}
	if err := cleanupSharedGateway(context.Background(), c); !errors.Is(err, lookupErr) {
		t.Fatalf("cleanup returned %v, want lookup failure", err)
	}
	if c.interfaceLookups != 3 || !c.applied {
		t.Fatalf("cleanup did not finish and acknowledge the other deletions: lookups=%d applied=%t",
			c.interfaceLookups, c.applied)
	}
	ports, err := ovsops.FindOVSPortsWithPredicate(ovsClient, func(*vswitchd.Port) bool { return true })
	if err != nil || len(ports) != 1 || len(ports[0].Interfaces) != 1 || ports[0].Interfaces[0] != c.failedInterface {
		t.Fatalf("expected only the failed port to remain, got %+v: %v", ports, err)
	}
	if !fexec.CalledMatchesExpected() {
		t.Fatalf("restored NORMAL flows after incomplete cleanup: %s", fexec.ErrorDesc())
	}

	fexec.AddFakeCmd(&ovntest.ExpectedCmd{Cmd: "ovs-ofctl -O OpenFlow13 replace-flows br-ex -"})
	c.failLookupAt = 0
	if err := cleanupSharedGateway(context.Background(), c); err != nil {
		t.Fatalf("retry cleanup: %v", err)
	}
	ports, err = ovsops.FindOVSPortsWithPredicate(ovsClient, func(*vswitchd.Port) bool { return true })
	if err != nil || len(ports) != 0 {
		t.Fatalf("retry left ports %+v: %v", ports, err)
	}
	if !fexec.CalledMatchesExpected() {
		t.Fatalf("retry did not restore NORMAL flows: %s", fexec.ErrorDesc())
	}
}

func TestCleanupSharedGatewayWait(t *testing.T) {
	for _, cancelWait := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel=%t", cancelWait), func(t *testing.T) {
			if err := config.PrepareTestConfig(); err != nil {
				t.Fatalf("prepare config: %v", err)
			}
			t.Cleanup(func() { _ = config.PrepareTestConfig() })
			config.OvnKubeNode.Mode = types.NodeModeFull
			ovsClient, cleanup, err := libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{OVSData: []libovsdbtest.TestData{
				&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{"bridge"},
					ExternalIDs: map[string]string{"ovn-bridge-mappings": types.PhysicalNetworkName + ":br-ex"}},
				&vswitchd.Bridge{UUID: "bridge", Name: "br-ex", Ports: []string{"patch-port"}},
				&vswitchd.Port{UUID: "patch-port", Name: "patch-ovn", Interfaces: []string{"patch-iface"},
					ExternalIDs: map[string]string{"ovn-localnet-port": "localnet"}},
				&vswitchd.Interface{UUID: "patch-iface", Name: "patch-ovn", Type: "patch"},
			}})
			if err != nil {
				t.Fatalf("harness setup: %v", err)
			}
			t.Cleanup(cleanup.Cleanup)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			waitClient := &gatewayCleanupWaitClient{Client: ovsClient}
			if cancelWait {
				waitClient.cancel = cancel
			}

			fexec := ovntest.NewFakeExec()
			if !cancelWait {
				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: "ovs-ofctl -O OpenFlow13 replace-flows br-ex -",
					Action: func() error {
						if !waitClient.applied {
							return errors.New("restored NORMAL flows before ovs-vswitchd acknowledged removal")
						}
						return nil
					},
				})
			}
			if err := util.SetExec(fexec); err != nil {
				t.Fatalf("set fake exec: %v", err)
			}
			t.Cleanup(util.ResetRunner)

			err = cleanupSharedGateway(ctx, waitClient)
			if cancelWait {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cleanup returned %v, want context.Canceled", err)
				}
			} else if err != nil {
				t.Fatalf("cleanup failed: %v", err)
			}
			if !fexec.CalledMatchesExpected() {
				t.Fatalf("unexpected flow restoration: %s", fexec.ErrorDesc())
			}
			if _, err := ovsops.GetOVSPort(ovsClient, "patch-ovn"); !errors.Is(err, libovsdbclient.ErrNotFound) {
				t.Fatalf("committed patch-port deletion was not retained: %v", err)
			}
		})
	}
}
