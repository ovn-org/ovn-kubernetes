// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package node

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/utils/ptr"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/model"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/bridgeconfig"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
)

func TestOpenFlowManagerLocalnetPortEvents(t *testing.T) {
	ofm := &openflowManager{
		defaultBridge:    newOpenflowBridge(bridgeconfig.TestDefaultBridgeConfig()),
		uplinkBridges:    map[string]*openflowBridge{},
		localnetPortChan: make(chan struct{}, 1),
	}

	assertNotified := func(want bool) {
		t.Helper()
		select {
		case <-ofm.localnetPortChan:
			if !want {
				t.Fatal("unexpected localnet port notification")
			}
		default:
			if want {
				t.Fatal("expected localnet port notification")
			}
		}
	}

	gatewayPort := &vswitchd.Port{
		ExternalIDs: map[string]string{"ovn-localnet-port": "breth0_node"},
	}
	ofm.handleLocalnetPortEvent(vswitchd.PortTable, nil, gatewayPort)
	assertNotified(false)

	localnetPort := &vswitchd.Port{
		ExternalIDs: map[string]string{"ovn-localnet-port": "blue_ovn_localnet_port"},
	}
	ofm.handleLocalnetPortEvent(vswitchd.PortTable, nil, localnetPort)
	assertNotified(true)

	// Updates that don't change whether the row is a localnet topology port
	// must not trigger flow regeneration, for example statistics updates.
	ofm.handleLocalnetPortEvent(vswitchd.PortTable, localnetPort, &vswitchd.Port{
		ExternalIDs: map[string]string{"ovn-localnet-port": "blue_ovn_localnet_port"},
		Statistics:  map[string]int{"rx_packets": 1},
	})
	assertNotified(false)

	ofm.handleLocalnetPortEvent(vswitchd.BridgeTable,
		&vswitchd.Bridge{Name: "br-int", Ports: []string{"port-1"}},
		&vswitchd.Bridge{Name: "br-int", Ports: []string{"port-1", "port-2"}})
	assertNotified(false)

	ofm.handleLocalnetPortEvent(vswitchd.BridgeTable,
		&vswitchd.Bridge{Name: "breth0", Ports: []string{"port-1"}},
		&vswitchd.Bridge{Name: "breth0", Ports: []string{"port-1"}})
	assertNotified(false)

	ofm.handleLocalnetPortEvent(vswitchd.BridgeTable,
		&vswitchd.Bridge{Name: "breth0", Ports: []string{"port-1"}},
		&vswitchd.Bridge{Name: "breth0", Ports: []string{"port-1", "localnet-port"}})
	assertNotified(true)
}

func TestOpenFlowManagerLocalnetPortFlowLifecycle(t *testing.T) {
	if err := config.PrepareTestConfig(); err != nil {
		t.Fatalf("failed to prepare test config: %v", err)
	}
	t.Cleanup(func() {
		_ = config.PrepareTestConfig()
	})
	config.IPv4Mode = true
	config.IPv6Mode = false
	config.Gateway.Mode = config.GatewayModeShared

	const (
		bridgeName       = "breth0"
		bridgeUUID       = "breth0-uuid"
		localnetPortName = "patch-blue_ovn_localnet_port-to-br-int"
		localnetPortUUID = "localnet-port-uuid"
	)
	ovsClient, ovsCleanup, err := libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{
		OVSData: []libovsdbtest.TestData{
			&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{bridgeUUID}},
			&vswitchd.Bridge{UUID: bridgeUUID, Name: bridgeName},
		},
	})
	if err != nil {
		t.Fatalf("failed to create OVS test harness: %v", err)
	}
	t.Cleanup(ovsCleanup.Cleanup)

	_, bridgeIPNet, err := net.ParseCIDR("10.1.253.253/16")
	if err != nil {
		t.Fatalf("failed to parse bridge IP: %v", err)
	}
	bridgeIPNet.IP = net.ParseIP("10.1.253.253")
	bridgeMAC, err := net.ParseMAC("48:b0:2d:00:00:04")
	if err != nil {
		t.Fatalf("failed to parse bridge MAC: %v", err)
	}
	bridge := bridgeconfig.TestDefaultBridgeConfigWithOVSClient(
		ovsClient, []*net.IPNet{bridgeIPNet}, bridgeMAC)
	ofManager, err := newGatewayOpenFlowManager(bridge, nil, ovsClient)
	if err != nil {
		t.Fatalf("failed to create OpenFlow manager: %v", err)
	}

	_, hostSubnet, err := net.ParseCIDR("10.1.0.0/16")
	if err != nil {
		t.Fatalf("failed to parse host subnet: %v", err)
	}
	if err := ofManager.updateBridgeFlowCache(nil, []*net.IPNet{hostSubnet}); err != nil {
		t.Fatalf("failed to initialize bridge flow cache: %v", err)
	}

	countPriority102Flows := func() int {
		count := 0
		for _, flow := range ofManager.getFlowsByKey("DEFAULT") {
			if strings.Contains(flow, "priority=102") {
				count++
			}
		}
		return count
	}
	if count := countPriority102Flows(); count != 0 {
		t.Fatalf("expected no priority-102 flows before adding a localnet port, got %d", count)
	}

	fexec := ovntest.NewFakeExec()
	fexec.AddRepeatedFakeCmd(&ovntest.ExpectedCmd{
		Cmd: "ovs-ofctl -O OpenFlow13 --bundle replace-flows breth0 -",
	}, 3)
	if err := util.SetExec(fexec); err != nil {
		t.Fatalf("failed to set fake exec: %v", err)
	}
	t.Cleanup(util.ResetRunner)

	stopChan := make(chan struct{})
	var doneWg sync.WaitGroup
	ofManager.Run(stopChan, &doneWg)
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			close(stopChan)
			doneWg.Wait()
		}
	})

	localnetInterface := &vswitchd.Interface{
		UUID: "localnet-interface-uuid",
		Name: localnetPortName,
		Type: "patch",
	}
	interfaceOps, err := ovsClient.Create(localnetInterface)
	if err != nil {
		t.Fatalf("failed to create localnet interface operations: %v", err)
	}
	localnetPort := &vswitchd.Port{
		UUID:        localnetPortUUID,
		Name:        localnetPortName,
		Interfaces:  []string{localnetInterface.UUID},
		ExternalIDs: map[string]string{"ovn-localnet-port": "blue_ovn_localnet_port"},
	}
	portOps, err := ovsClient.Create(localnetPort)
	if err != nil {
		t.Fatalf("failed to create localnet port operations: %v", err)
	}
	ovsBridge := &vswitchd.Bridge{Name: bridgeName}
	if err := ovsClient.Get(context.Background(), ovsBridge); err != nil {
		t.Fatalf("failed to get gateway bridge: %v", err)
	}
	bridgeOps, err := ovsClient.Where(ovsBridge).Mutate(ovsBridge, model.Mutation{
		Field:   &ovsBridge.Ports,
		Mutator: ovsdb.MutateOperationInsert,
		Value:   []string{localnetPort.UUID},
	})
	if err != nil {
		t.Fatalf("failed to create bridge mutation operations: %v", err)
	}
	ovsOperations := append(interfaceOps, portOps...)
	ovsOperations = append(ovsOperations, bridgeOps...)
	if _, err := ops.TransactAndCheck(ovsClient, ovsOperations); err != nil {
		t.Fatalf("failed to add localnet port to gateway bridge: %v", err)
	}

	waitForCondition := func(description string, condition func() bool) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if condition() {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s", description)
	}
	waitForCondition("localnet flows to be installed", func() bool {
		return countPriority102Flows() == 2 && fexec.CalledMatchesExpectedAtLeastN(1)
	})

	if err := ops.DeletePortWithInterfaces(ovsClient, bridgeName, localnetPortName); err != nil {
		t.Fatalf("failed to remove localnet port from gateway bridge: %v", err)
	}
	waitForCondition("localnet flows to be removed", func() bool {
		return countPriority102Flows() == 0 && fexec.CalledMatchesExpectedAtLeastN(2)
	})

	close(stopChan)
	doneWg.Wait()
	stopped = true
	if !fexec.CalledMatchesExpected() {
		t.Fatal(fexec.ErrorDesc())
	}
}

func TestStringListsEqual(t *testing.T) {
	tests := []struct {
		name string
		a    []string
		b    []string
		want bool
	}{
		{name: "same order", a: []string{"flow-a", "flow-b"}, b: []string{"flow-a", "flow-b"}, want: true},
		{name: "different order", a: []string{"flow-a", "flow-b"}, b: []string{"flow-b", "flow-a"}, want: true},
		{name: "different flow", a: []string{"flow-a", "flow-b"}, b: []string{"flow-a", "flow-c"}, want: false},
		{name: "different duplicate count", a: []string{"flow-a", "flow-a"}, b: []string{"flow-a", "flow-b"}, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := stringListsEqual(test.a, test.b); got != test.want {
				t.Fatalf("stringListsEqual() = %t, want %t", got, test.want)
			}
		})
	}
}

// newUplinkBridgeOVSClient returns an OVSDB client whose database holds the
// given bridges, each with a single physical port of the given ofport.
func newUplinkBridgeOVSClient(t *testing.T, bridgePhysPorts map[string]struct {
	physIntf string
	ofport   int
}) libovsdbclient.Client {
	t.Helper()
	root := &vswitchd.OpenvSwitch{UUID: "root-ovs"}
	data := []libovsdbtest.TestData{root}
	for bridgeName, phys := range bridgePhysPorts {
		root.Bridges = append(root.Bridges, bridgeName+"-uuid")
		data = append(data,
			&vswitchd.Bridge{UUID: bridgeName + "-uuid", Name: bridgeName, Ports: []string{phys.physIntf + "-port-uuid"}},
			&vswitchd.Port{UUID: phys.physIntf + "-port-uuid", Name: phys.physIntf, Interfaces: []string{phys.physIntf + "-iface-uuid"}},
			&vswitchd.Interface{UUID: phys.physIntf + "-iface-uuid", Name: phys.physIntf, Type: "system", Ofport: ptr.To(phys.ofport)},
		)
	}
	ovsClient, cleanup, err := libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{OVSData: data})
	if err != nil {
		t.Fatalf("failed to create OVS test harness: %v", err)
	}
	t.Cleanup(cleanup.Cleanup)
	return ovsClient
}

// The last network of an Uplink bridge is torn down after the admin deleted
// the bridge: the flow cleanup cannot run, but there is nothing left to clean
// up and the bridge must be deregistered so the port check stops tracking it.
func TestOpenFlowManagerDropsVanishedUplinkBridgeOnCleanup(t *testing.T) {
	fexec := ovntest.NewFakeExec()
	if err := util.SetExec(fexec); err != nil {
		t.Fatalf("failed to set fake exec: %v", err)
	}
	t.Cleanup(util.ResetRunner)
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd: "ovs-ofctl -O OpenFlow13 --bundle replace-flows uup1 -",
		Err: errors.New("ovs-ofctl: uup1 is not a bridge or a socket"),
	})

	bridge := newOpenflowBridge(bridgeconfig.TestUplinkBridgeConfig("uup1", "eth2", "1"))
	if err := bridge.AddNetworkConfig(&util.DefaultNetInfo{}, nil, nil, 0, 0, nil, nil); err != nil {
		t.Fatalf("failed to add network config: %v", err)
	}
	ofm := &openflowManager{
		defaultBridge: newOpenflowBridge(bridgeconfig.TestDefaultBridgeConfig()),
		uplinkBridges: map[string]*openflowBridge{"uup1": bridge},
		ovsClient: newUplinkBridgeOVSClient(t, map[string]struct {
			physIntf string
			ofport   int
		}{}),
	}

	if err := ofm.delNetwork(&util.DefaultNetInfo{}, "uup1"); err != nil {
		t.Fatalf("expected cleanup of a vanished uplink bridge to succeed, got %v", err)
	}
	if _, found := ofm.getUplinkBridge("uup1"); found {
		t.Fatal("expected vanished uplink bridge to be deregistered")
	}
	if !fexec.CalledMatchesExpected() {
		t.Fatal("expected cleanup to attempt a flow replacement first")
	}
}

// A flow cleanup failure on a bridge that still exists is a real error: the
// bridge stays registered so the next cleanup attempt can retry it.
func TestOpenFlowManagerKeepsExistingUplinkBridgeOnCleanupFailure(t *testing.T) {
	fexec := ovntest.NewFakeExec()
	if err := util.SetExec(fexec); err != nil {
		t.Fatalf("failed to set fake exec: %v", err)
	}
	t.Cleanup(util.ResetRunner)
	expectedErr := errors.New("ovs-vswitchd not responding")
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd: "ovs-ofctl -O OpenFlow13 --bundle replace-flows uup1 -",
		Err: expectedErr,
	})

	bridge := newOpenflowBridge(bridgeconfig.TestUplinkBridgeConfig("uup1", "eth2", "1"))
	if err := bridge.AddNetworkConfig(&util.DefaultNetInfo{}, nil, nil, 0, 0, nil, nil); err != nil {
		t.Fatalf("failed to add network config: %v", err)
	}
	ofm := &openflowManager{
		defaultBridge: newOpenflowBridge(bridgeconfig.TestDefaultBridgeConfig()),
		uplinkBridges: map[string]*openflowBridge{"uup1": bridge},
		ovsClient: newUplinkBridgeOVSClient(t, map[string]struct {
			physIntf string
			ofport   int
		}{"uup1": {physIntf: "eth2", ofport: 1}}),
	}

	err := ofm.delNetwork(&util.DefaultNetInfo{}, "uup1")
	if !errors.Is(err, expectedErr) {
		t.Fatalf("expected flow replacement error, got %v", err)
	}
	if _, found := ofm.getUplinkBridge("uup1"); !found {
		t.Fatal("expected existing uplink bridge to stay registered after a cleanup failure")
	}
}

// An Uplink bridge the admin deleted skips its port check and every flow sync
// instead of exiting the process on its vanished ports, like a bridge whose
// check failed; it stays registered until its last network's cleanup.
func TestOpenFlowManagerPortCheckSkipsDeletedUplinkBridge(t *testing.T) {
	if err := config.PrepareTestConfig(); err != nil {
		t.Fatalf("failed to prepare test config: %v", err)
	}
	fexec := ovntest.NewLooseCompareFakeExec()
	if err := util.SetExec(fexec); err != nil {
		t.Fatalf("failed to set fake exec: %v", err)
	}
	t.Cleanup(util.ResetRunner)

	type physPort = struct {
		physIntf string
		ofport   int
	}
	ofm := &openflowManager{
		defaultBridge: newOpenflowBridge(bridgeconfig.TestDefaultBridgeConfig()),
		uplinkBridges: map[string]*openflowBridge{
			"uup1": newOpenflowBridge(bridgeconfig.TestUplinkBridgeConfig("uup1", "eth2", "1")),
			// uup2's physical port lost its netdevice: OVS reports ofport -1.
			"uup2": newOpenflowBridge(bridgeconfig.TestUplinkBridgeConfig("uup2", "eth3", "1")),
			// uup3 was deleted.
			"uup3": newOpenflowBridge(bridgeconfig.TestUplinkBridgeConfig("uup3", "eth4", "1")),
		},
		ovsClient: newUplinkBridgeOVSClient(t, map[string]physPort{
			"uup1": {physIntf: "eth2", ofport: 1},
			"uup2": {physIntf: "eth3", ofport: -1},
		}),
	}

	ofm.checkUplinkBridgePorts()
	if _, found := ofm.getUplinkBridge("uup3"); !found {
		t.Fatal("expected the deleted bridge to stay registered until its network cleanup")
	}
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{Cmd: "ovs-ofctl -O OpenFlow13 --bundle replace-flows breth0 -"})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{Cmd: "ovs-ofctl -O OpenFlow13 --bundle replace-flows uup1 -"})
	ofm.syncFlows()
	for _, bridgeName := range []string{"uup2", "uup3"} {
		if found, err := ofm.syncUplinkBridgeFlows(bridgeName); found || err != nil {
			t.Fatalf("expected the gateway sync of %s to be skipped and reported as not found, got found=%v err=%v",
				bridgeName, found, err)
		}
	}
	if !fexec.CalledMatchesExpected() {
		t.Fatalf("expected both syncs to skip the failed bridges: %s", fexec.ErrorDesc())
	}

	// The deleted bridge is back with the same ports: the next check clears it.
	ofm.ovsClient = newUplinkBridgeOVSClient(t, map[string]physPort{
		"uup1": {physIntf: "eth2", ofport: 1},
		"uup3": {physIntf: "eth4", ofport: 1},
	})
	ofm.checkUplinkBridgePorts()
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{Cmd: "ovs-ofctl -O OpenFlow13 --bundle replace-flows uup3 -"})
	if found, err := ofm.syncUplinkBridgeFlows("uup3"); !found || err != nil {
		t.Fatalf("expected the gateway sync of the recreated bridge to succeed, got found=%v err=%v", found, err)
	}
	if !fexec.CalledMatchesExpected() {
		t.Fatalf("expected the recreated bridge flows to be replaced: %s", fexec.ErrorDesc())
	}
}

func TestOpenFlowManagerDeletesGroupCacheWithFlowCache(t *testing.T) {
	ofm := &openflowManager{
		defaultBridge: newOpenflowBridge(bridgeconfig.TestDefaultBridgeConfig()),
		uplinkBridges: map[string]*openflowBridge{},
	}
	key := "NodePort_namespace1_service1_tcp_31111"

	ofm.updateFlowCacheEntry(key, []string{"cookie=0x123, priority=110, actions=group:100"})
	ofm.updateGroupCacheEntry(key, []string{"group_id=100,type=select,bucket=actions=output:LOCAL"})

	ofm.deleteFlowsByKey(key)

	if flows := ofm.getFlowsByKey(key); flows != nil {
		t.Fatalf("expected flow cache entry to be deleted, got %#v", flows)
	}
	if groups := ofm.getGroupsByKey(key); groups != nil {
		t.Fatalf("expected group cache entry to be deleted, got %#v", groups)
	}
}

func TestOpenFlowManagerCleansUnusedUplinkBridgeFlows(t *testing.T) {
	fexec := ovntest.NewFakeExec()
	if err := util.SetExec(fexec); err != nil {
		t.Fatalf("failed to set fake exec: %v", err)
	}
	t.Cleanup(util.ResetRunner)
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd: "ovs-ofctl -O OpenFlow13 --bundle replace-flows breth0 -",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd: "ovs-ofctl -O OpenFlow13 del-groups breth0 group_id=100",
	})

	bridge := newOpenflowBridge(bridgeconfig.TestDefaultBridgeConfig())
	bridge.updateFlowCacheEntry("stale", []string{"table=0,priority=100,actions=drop"})
	bridge.updateGroupCacheEntry("stale", []string{"group_id=100,type=select,bucket=actions=drop"})
	bridge.installedGroups = map[string]struct{}{"100": {}}
	ofm := &openflowManager{
		defaultBridge: newOpenflowBridge(bridgeconfig.TestDefaultBridgeConfig()),
		uplinkBridges: map[string]*openflowBridge{
			"breth0": bridge,
		},
	}

	if err := ofm.delNetwork(&util.DefaultNetInfo{}, "breth0"); err != nil {
		t.Fatalf("failed to delete network from uplink bridge: %v", err)
	}

	if len(ofm.uplinkBridges) != 0 {
		t.Fatalf("expected unused uplink bridge to be removed, got %d entries", len(ofm.uplinkBridges))
	}
	if flows := bridge.getFlowsByKey("NORMAL"); len(flows) != 1 {
		t.Fatalf("expected NORMAL flow to remain, got %#v", flows)
	}
	if flows := bridge.getFlowsByKey("stale"); flows != nil {
		t.Fatalf("expected stale flow cache entry to be deleted, got %#v", flows)
	}
	if groups := bridge.getGroupsByKey("stale"); groups != nil {
		t.Fatalf("expected stale group cache entry to be deleted, got %#v", groups)
	}
	if len(bridge.installedGroups) != 0 {
		t.Fatalf("expected stale installed groups to be deleted, got %#v", bridge.installedGroups)
	}
	if bridge.GetNetConfigLen() != 0 {
		t.Fatalf("expected %s network config to be removed", types.DefaultNetworkName)
	}
	if !fexec.CalledMatchesExpected() {
		t.Fatalf("expected cleanup to replace flows on the unused uplink bridge")
	}
}

func TestOpenFlowManagerSyncsUplinkBridgeFlows(t *testing.T) {
	if err := config.PrepareTestConfig(); err != nil {
		t.Fatalf("failed to prepare test config: %v", err)
	}
	fexec := ovntest.NewFakeExec()
	if err := util.SetExec(fexec); err != nil {
		t.Fatalf("failed to set fake exec: %v", err)
	}
	t.Cleanup(util.ResetRunner)
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd: "ovs-ofctl -O OpenFlow13 --bundle replace-flows breth0 -",
	})

	ofBridge := newOpenflowBridge(bridgeconfig.TestDefaultBridgeConfig())
	ofBridge.updateFlowCacheEntry("NORMAL", []string{"table=0,priority=0,actions=NORMAL"})
	ofm := &openflowManager{
		defaultBridge: newOpenflowBridge(bridgeconfig.TestDefaultBridgeConfig()),
		uplinkBridges: map[string]*openflowBridge{
			"breth0": ofBridge,
		},
	}

	found, err := ofm.syncUplinkBridgeFlows("breth0")
	if err != nil {
		t.Fatalf("failed to sync uplink bridge flows: %v", err)
	}
	if !found {
		t.Fatal("expected uplink bridge to be found")
	}
	if !fexec.CalledMatchesExpected() {
		t.Fatal("expected uplink bridge flows to be replaced")
	}

	expectedErr := errors.New("failed to replace flows")
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd: "ovs-ofctl -O OpenFlow13 --bundle replace-flows breth0 -",
		Err: expectedErr,
	})
	found, err = ofm.syncUplinkBridgeFlows("breth0")
	if !found {
		t.Fatal("expected uplink bridge to be found when flow sync fails")
	}
	if !errors.Is(err, expectedErr) {
		t.Fatalf("expected flow replacement error, got %v", err)
	}

	found, err = ofm.syncUplinkBridgeFlows("missing")
	if err != nil {
		t.Fatalf("unexpected error for missing uplink bridge: %v", err)
	}
	if found {
		t.Fatal("expected missing uplink bridge not to be found")
	}
}

func TestOpenFlowManagerDefaultTargetUsesDefaultBridgeSet(t *testing.T) {
	defaultBridge := bridgeconfig.TestDefaultBridgeConfig()
	externalGatewayBridge := bridgeconfig.TestDefaultBridgeConfig()
	defaultBridge.DelNetworkConfig(&util.DefaultNetInfo{})
	externalGatewayBridge.DelNetworkConfig(&util.DefaultNetInfo{})
	ofm := &openflowManager{
		defaultBridge:         newOpenflowBridge(defaultBridge),
		externalGatewayBridge: newOpenflowBridge(externalGatewayBridge),
		uplinkBridges:         map[string]*openflowBridge{},
	}

	if err := ofm.addNetwork(defaultOpenFlowBridgeSetName, nil, &util.DefaultNetInfo{}, nil, nil, 0, 0, nil, nil); err != nil {
		t.Fatalf("failed to add network to default bridge set: %v", err)
	}

	if netConfig := ofm.defaultBridge.GetNetworkConfig(types.DefaultNetworkName); netConfig == nil {
		t.Fatalf("expected default bridge network config to be added")
	}
	if netConfig := ofm.externalGatewayBridge.GetNetworkConfig(types.DefaultNetworkName); netConfig == nil {
		t.Fatalf("expected external gateway bridge network config to be added")
	}
	if len(ofm.uplinkBridges) != 0 {
		t.Fatalf("expected default bridge target to avoid uplink bridge cache, got %d entries", len(ofm.uplinkBridges))
	}

	if err := ofm.delNetwork(&util.DefaultNetInfo{}, defaultOpenFlowBridgeSetName); err != nil {
		t.Fatalf("failed to delete network from default bridge set: %v", err)
	}
	if netConfig := ofm.defaultBridge.GetNetworkConfig(types.DefaultNetworkName); netConfig != nil {
		t.Fatalf("expected default bridge network config to be removed")
	}
	if netConfig := ofm.externalGatewayBridge.GetNetworkConfig(types.DefaultNetworkName); netConfig != nil {
		t.Fatalf("expected external gateway bridge network config to be removed")
	}
}

func TestOpenFlowManagerDefaultNetOVSBridgeFinder(t *testing.T) {
	const nodeName = "multi-homing-worker-0.maiqueb.org"

	testCases := []struct {
		name                  string
		desc                  string
		inputPortInfo         string
		expectedBridgeName    string
		expectedPatchPortName string
	}{
		{
			name:                  "empty input ports",
			inputPortInfo:         "",
			expectedBridgeName:    "",
			expectedPatchPortName: "",
		},
		{
			name:                  "input ports without patch ports",
			inputPortInfo:         "port1",
			expectedBridgeName:    "",
			expectedPatchPortName: "",
		},
		{
			name: "input ports with a patch port",
			inputPortInfo: `
port1
port2
patch-br-ex_multi-homing-worker-0.maiqueb.org-to-br-int`,
			expectedBridgeName:    "br-ex",
			expectedPatchPortName: "patch-br-ex_multi-homing-worker-0.maiqueb.org-to-br-int",
		},
		{
			name: "input ports with a patch port for a localnet network",
			inputPortInfo: `
port1
port2
patch-vlan2003_ovn_localnet_port-to-br-int`,
			expectedBridgeName:    "",
			expectedPatchPortName: "",
		},
		{
			name: "input ports with a patch port for the default network and a localnet",
			inputPortInfo: `
port1
port2
patch-vlan2003_ovn_localnet_port-to-br-int
patch-br-ex_multi-homing-worker-0.maiqueb.org-to-br-int`,
			expectedBridgeName:    "br-ex",
			expectedPatchPortName: "patch-br-ex_multi-homing-worker-0.maiqueb.org-to-br-int",
		},
		{
			name: "input ports with a patch port for the default network, a localnet, and an extra primary UDN",
			inputPortInfo: `
port1
port2
patch-vlan2003_ovn_localnet_port-to-br-int
patch-br-ex_tenant-blue_multi-homing-worker-0.maiqueb.org-to-br-int
patch-br-ex_multi-homing-worker-0.maiqueb.org-to-br-int`,
			expectedBridgeName:    "br-ex",
			expectedPatchPortName: "patch-br-ex_multi-homing-worker-0.maiqueb.org-to-br-int",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			bridgeName, patchPortName := localnetPortInfo(nodeName, tc.inputPortInfo)
			if bridgeName != tc.expectedBridgeName {
				t.Errorf("Expected bridge name %q got %q", tc.expectedBridgeName, bridgeName)
			}
			if patchPortName != tc.expectedPatchPortName {
				t.Errorf("Expected patch port name %q got %q", tc.expectedPatchPortName, patchPortName)
			}
		})
	}
}

// --allow-no-uplink can leave physIntf empty. checkPorts must skip the phys
// ofport check in that case (GetOVSInterface("") cannot look up a zero Name).
func TestCheckPortsAllowNoUplink(t *testing.T) {
	if err := config.PrepareTestConfig(); err != nil {
		t.Fatalf("PrepareTestConfig: %v", err)
	}
	t.Cleanup(func() {
		_ = config.PrepareTestConfig()
	})
	config.Gateway.AllowNoUplink = true
	if err := checkPorts(nil, nil, "", ""); err != nil {
		t.Fatalf("checkPorts with AllowNoUplink and empty physIntf: %v", err)
	}
}
