// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package cni

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	ovncnitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb"
	ovsops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	utilmocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/mocks"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
)

func newShimOVSClientForTest(t *testing.T, serverClient client.Client, cmd command) client.Client {
	t.Helper()
	stopCh := make(chan struct{})
	t.Cleanup(func() { close(stopCh) })
	c, err := libovsdb.NewOVSClientWithEndpoint(serverClient.CurrentEndpoint(), stopCh, shimOVSMonitorOptions(cmd)...)
	require.NoError(t, err)
	t.Cleanup(c.Close)
	return c
}

func TestShimOVSMonitorScope(t *testing.T) {
	serverClient, realServer := newShimOVSTestServer(t, []libovsdbtest.TestData{
		&vswitchd.OpenvSwitch{UUID: "ovs", Bridges: []string{"br_int", "br_ex"}, CurCfg: 3, NextCfg: 3,
			ExternalIDs: map[string]string{"ovn-pf-encap-ip-mapping": "pf0:192.0.2.1"}},
		&vswitchd.Bridge{UUID: "br_int", Name: "br-int", DatapathType: "netdev", Ports: []string{"port"}},
		&vswitchd.Bridge{UUID: "br_ex", Name: "br-ex"},
		&vswitchd.Port{UUID: "port", Name: "pod-port", Interfaces: []string{"iface"}, OtherConfig: map[string]string{"transient": "true"}},
		&vswitchd.Interface{UUID: "iface", Name: "pod-port", ExternalIDs: map[string]string{"sandbox": "sandbox"},
			Statistics: map[string]int{"rx_packets": 99}},
	})
	for _, cmd := range []command{CNIAdd, CNIDel} {
		t.Run(string(cmd), func(t *testing.T) {
			c := newShimOVSClientForTest(t, serverClient, cmd)
			monitor := c.NewMonitor(shimOVSMonitorOptions(cmd)...)
			require.Empty(t, monitor.Errors)
			columns := map[string][]string{}
			for _, table := range monitor.Tables {
				columns[table.Table] = table.Fields
				if cmd == CNIDel && table.Table == vswitchd.BridgeTable {
					require.Equal(t, []ovsdb.Condition{ovsdb.NewCondition("name", ovsdb.ConditionEqual, "br-int")}, table.Conditions)
				} else {
					require.Empty(t, table.Conditions)
				}
			}
			wantColumns := map[string][]string{
				vswitchd.PortTable: {"name", "interfaces"}, vswitchd.InterfaceTable: {"name", "external_ids"},
				vswitchd.BridgeTable: {"name", "ports"},
			}
			if cmd == CNIAdd {
				wantColumns[vswitchd.OpenvSwitchTable] = []string{"external_ids"}
				wantColumns[vswitchd.BridgeTable] = append(wantColumns[vswitchd.BridgeTable], "datapath_type")
			}
			require.Equal(t, wantColumns, columns)
			iface, err := ovsops.GetOVSInterface(c, "pod-port")
			require.NoError(t, err)
			require.Equal(t, "sandbox", iface.ExternalIDs["sandbox"])
			if realServer {
				require.Empty(t, iface.Statistics)
			}
			port, err := ovsops.GetOVSPort(c, "pod-port")
			require.NoError(t, err)
			require.Equal(t, []string{iface.UUID}, port.Interfaces)
			if realServer {
				require.Empty(t, port.OtherConfig)
			}
			bridge, err := ovsops.GetBridge(c, "br-int")
			require.NoError(t, err)
			require.Equal(t, []string{port.UUID}, bridge.Ports)
			if cmd == CNIAdd {
				require.Equal(t, "netdev", bridge.DatapathType)
				_, err = ovsops.GetBridge(c, "br-ex")
				require.NoError(t, err)
				ovs, err := ovsops.GetOpenvSwitch(c)
				require.NoError(t, err)
				if realServer {
					require.Zero(t, ovs.CurCfg)
				}
				require.Equal(t, "pf0:192.0.2.1", ovs.ExternalIDs["ovn-pf-encap-ip-mapping"])
			} else {
				if realServer {
					require.Empty(t, bridge.DatapathType)
					_, err = ovsops.GetBridge(c, "br-ex")
					require.ErrorIs(t, err, client.ErrNotFound)
				}
				_, err = ovsops.GetOpenvSwitch(c)
				require.ErrorIs(t, err, client.ErrNotFound)
			}
		})
	}
	// The long-lived client's default monitor remains unchanged.
	iface, err := ovsops.GetOVSInterface(serverClient, "pod-port")
	require.NoError(t, err)
	require.Equal(t, 99, iface.Statistics["rx_packets"])
}

func TestShimOVSOwnershipAndCleanup(t *testing.T) {
	serverClient, _ := newShimOVSTestServer(t, []libovsdbtest.TestData{
		&vswitchd.OpenvSwitch{UUID: "ovs", Bridges: []string{"br_int", "br_ex"}},
		&vswitchd.Bridge{UUID: "br_int", Name: "br-int", Ports: []string{"default", "primary", "stale", "bond"}},
		&vswitchd.Bridge{UUID: "br_ex", Name: "br-ex", Ports: []string{"foreign"}},
		&vswitchd.Port{UUID: "default", Name: "default", Interfaces: []string{"default_iface"}},
		&vswitchd.Interface{UUID: "default_iface", Name: "default", ExternalIDs: map[string]string{
			"sandbox": "sandbox", "iface-id": "ns_pod", "pod-if-name": "eth0"}},
		&vswitchd.Port{UUID: "primary", Name: "primary", Interfaces: []string{"primary_iface"}},
		&vswitchd.Interface{UUID: "primary_iface", Name: "primary", ExternalIDs: map[string]string{
			"sandbox": "sandbox", "iface-id": "ns_pod_ns_primary", types.NADExternalID: "ns/primary"}},
		&vswitchd.Port{UUID: "stale", Name: "stale", Interfaces: []string{"stale_iface"}},
		&vswitchd.Interface{UUID: "stale_iface", Name: "stale", ExternalIDs: map[string]string{
			"sandbox": "old-sandbox", "iface-id": "ns_pod"}},
		&vswitchd.Port{UUID: "foreign", Name: "foreign", Interfaces: []string{"foreign_iface"}},
		&vswitchd.Interface{UUID: "foreign_iface", Name: "foreign", ExternalIDs: map[string]string{"iface-id": "ns_pod"}},
		&vswitchd.Port{UUID: "bond", Name: "bond", Interfaces: []string{"member1", "member2"}},
		&vswitchd.Interface{UUID: "member1", Name: "member1"},
		&vswitchd.Interface{UUID: "member2", Name: "member2"},
	})
	c := newShimOVSClientForTest(t, serverClient, CNIAdd)
	_, err := ovsops.CreateOrUpdatePodPortOps(c, nil, "br-int", "foreign", &vswitchd.Port{}, &vswitchd.Interface{})
	require.ErrorContains(t, err, `already attached to bridge "br-ex"`)
	_, err = ovsops.CreateOrUpdatePodPortOps(c, nil, "br-int", "member1", &vswitchd.Port{}, &vswitchd.Interface{})
	require.ErrorContains(t, err, `already attached to port "bond"`)
	ifaceID, nadKey, exists, err := getExistingIfaceMeta(c, "primary")
	require.NoError(t, err)
	require.True(t, exists)
	require.Equal(t, "ns_pod_ns_primary", ifaceID)
	require.Equal(t, "ns/primary", nadKey)
	require.NoError(t, deleteStalePodPorts(c, "ns_pod", "default"))
	_, err = ovsops.GetOVSPort(c, "stale")
	require.ErrorIs(t, err, client.ErrNotFound)

	delClient := newShimOVSClientForTest(t, serverClient, CNIDel)
	names, err := findInterfacesWithSandbox(delClient, "sandbox")
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"default", "primary"}, names)
	names, err = findPodInterfaces(delClient, "sandbox", "missing", "primary", "ns/primary")
	require.NoError(t, err)
	require.Equal(t, []string{"primary"}, names)
	for _, name := range []string{"default", "primary", "member1", "foreign"} {
		require.NoError(t, ovsops.DeletePortWithInterfaces(delClient, "br-int", name))
	}
	_, err = ovsops.GetOVSPort(delClient, "foreign")
	require.NoError(t, err, "foreign port must survive both stale cleanup and DEL")
	bridge, err := ovsops.GetBridge(delClient, "br-int")
	require.NoError(t, err)
	require.Empty(t, bridge.Ports)
	for _, name := range []string{"default", "primary", "member1", "member2"} {
		_, err = ovsops.GetOVSInterface(delClient, name)
		require.ErrorIs(t, err, client.ErrNotFound)
	}
}

func TestShimOVSPortLifecycle(t *testing.T) {
	serverClient, _ := newShimOVSTestServer(t, []libovsdbtest.TestData{
		&vswitchd.OpenvSwitch{UUID: "ovs", Bridges: []string{"br_int"}},
		&vswitchd.Bridge{UUID: "br_int", Name: "br-int"},
	})
	c := newShimOVSClientForTest(t, serverClient, CNIAdd)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	extIDs := map[string]string{
		"sandbox": "sandbox", "iface-id": "ns_pod", "keep": "value",
		types.NetworkExternalID: "old-network", types.NADExternalID: "ns/old",
	}
	require.NoError(t, addOrUpdatePodPort(ctx, c, "pod-port", extIDs, true, 1400, false))
	require.Eventually(t, func() bool {
		iface, err := ovsops.GetOVSInterface(serverClient, "pod-port")
		return err == nil && iface.Type == "dpdk" && iface.MTURequest != nil && *iface.MTURequest == 1400
	}, time.Second, 10*time.Millisecond)
	port := &vswitchd.Port{Name: "pod-port", OtherConfig: map[string]string{"keep": "value"}}
	ops, err := serverClient.Where(&vswitchd.Port{Name: port.Name}).Update(port, &port.OtherConfig)
	require.NoError(t, err)
	_, err = ovsops.TransactAndCheck(serverClient, ops)
	require.NoError(t, err)
	// Repeat ADD, including removal of stale network IDs, through the narrow cache.
	require.NoError(t, addOrUpdatePodPort(ctx, c, "pod-port", map[string]string{
		"sandbox": "sandbox", "iface-id": "ns_pod",
	}, false, 1400, true))
	iface, err := ovsops.GetOVSInterface(c, "pod-port")
	require.NoError(t, err)
	require.Equal(t, "value", iface.ExternalIDs["keep"])
	require.NotContains(t, iface.ExternalIDs, types.NADExternalID)
	require.NotContains(t, iface.ExternalIDs, types.NetworkExternalID)
	require.Eventually(t, func() bool {
		port, err := ovsops.GetOVSPort(serverClient, "pod-port")
		return err == nil && port.OtherConfig["keep"] == "value" && port.OtherConfig["transient"] == "true"
	}, time.Second, 10*time.Millisecond)
	// Simulate ovn-controller reporting readiness through a separate connection.
	iface.ExternalIDs["ovn-installed"] = "true"
	ops, err = serverClient.Where(&vswitchd.Interface{UUID: iface.UUID}).Update(iface, &iface.ExternalIDs)
	require.NoError(t, err)
	_, err = ovsops.TransactAndCheck(serverClient, ops)
	require.NoError(t, err)
	require.NoError(t, waitForPodInterface(ctx, c, &PodInterfaceInfo{}, "pod-port", "ns_pod", nil, "ns", "pod", "uid"))

	require.NoError(t, setPodBandwidthForTest(c, "sandbox", "pod-port", 10_000_000, 2_000_000))
	require.Eventually(t, func() bool {
		port, err := ovsops.GetOVSPort(serverClient, "pod-port")
		if err != nil || port.QOS == nil {
			return false
		}
		iface, err := ovsops.GetOVSInterface(serverClient, "pod-port")
		return err == nil && iface.IngressPolicingRate == 2000 && iface.IngressPolicingBurst == 200
	}, time.Second, 10*time.Millisecond)
	delClient := newShimOVSClientForTest(t, serverClient, CNIDel)
	require.NoError(t, clearPodBandwidth(delClient, "sandbox"))
	require.Eventually(t, func() bool {
		port, err := ovsops.GetOVSPort(serverClient, "pod-port")
		if err != nil || port.QOS != nil {
			return false
		}
		iface, err := ovsops.GetOVSInterface(serverClient, "pod-port")
		return err == nil && iface.IngressPolicingRate == 2000 && iface.IngressPolicingBurst == 200
	}, time.Second, 10*time.Millisecond)
	results, err := ovsops.TransactAndCheck(delClient, []ovsdb.Operation{{Op: ovsdb.OperationSelect, Table: vswitchd.QoSTable}})
	require.NoError(t, err)
	require.Empty(t, results[0].Rows)
	require.NoError(t, ovsops.DeletePortWithInterfaces(delClient, "br-int", "pod-port"))
	_, err = ovsops.GetOVSInterface(delClient, "pod-port")
	require.ErrorIs(t, err, client.ErrNotFound)
}

func TestShimOVSConfigureAndCleanup(t *testing.T) {
	for _, attachments := range []int{1, 2} {
		t.Run(fmt.Sprintf("attachments=%d", attachments), func(t *testing.T) {
			server, _ := newShimOVSTestServer(t, []libovsdbtest.TestData{
				&vswitchd.OpenvSwitch{UUID: "ovs", Bridges: []string{"br_int"}},
				&vswitchd.Bridge{UUID: "br_int", Name: "br-int"},
			})
			emulateShimOVSReadiness(t, server)
			c := &trackingOVSClient{Client: newShimOVSClientForTest(t, server, CNIAdd)}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			checkEgress := func(name string, rate int) {
				t.Helper()
				require.Eventually(t, func() bool {
					iface, err := ovsops.GetOVSInterface(server, name)
					return err == nil && iface.IngressPolicingRate == rate && iface.IngressPolicingBurst == rate/10
				}, time.Second, 10*time.Millisecond, "unexpected egress policing on %s", name)
			}
			for i := 0; i < attachments; i++ {
				info := &PodInterfaceInfo{
					PodUID: "pod-uid", NetName: types.DefaultNetworkName, NADKey: types.DefaultNetworkName,
					PodAnnotation: util.PodAnnotation{
						MAC: net.HardwareAddr{0x0a, 0x58, 0x0a, 0x80, 0, 1},
						IPs: ovntest.MustParseIPNets("10.128.0.2/23", "fd00::2/64"),
					},
				}
				if i == 1 {
					info.NetName, info.NADKey = "blue", "ns/blue"
				}
				name := fmt.Sprintf("pod-port-%d", i)
				for attempt := 0; attempt < 2; attempt++ {
					c.transactions = 0
					require.NoError(t, ConfigureOVS(ctx, c, "ns", "pod", fmt.Sprintf("eth%d", i), name,
						info, "sandbox", "", false, nil))
					require.Equal(t, 2, c.transactions, "ADD should batch bandwidth cleanup and the policing reset")
					// Re-ADD without bandwidth annotations clears only this interface's limit.
					checkEgress(name, 0)
					require.Eventually(t, func() bool {
						port, err := ovsops.GetOVSPort(server, name)
						return err == nil && port.QOS == nil
					}, time.Second, 10*time.Millisecond)
					require.NoError(t, setPodBandwidthForTest(c, "sandbox", name, 10_000_000, 2_000_000))
					checkEgress(name, 2000)
				}
				iface, err := ovsops.GetOVSInterface(c, name)
				require.NoError(t, err)
				require.Equal(t, "pod-uid", iface.ExternalIDs["iface-id-ver"])
				require.Equal(t, "10.128.0.2/23,fd00::2/64", iface.ExternalIDs["ip_addresses"])
				if i == 0 {
					require.NotContains(t, iface.ExternalIDs, types.NADExternalID)
				} else {
					require.Equal(t, info.NADKey, iface.ExternalIDs[types.NADExternalID])
				}
			}
			// Configuring another attachment must preserve the earlier one's egress limit.
			for i := 0; i < attachments; i++ {
				checkEgress(fmt.Sprintf("pod-port-%d", i), 2000)
			}
			c.Close()

			// Exercise host-side port cleanup without changing host networking.
			netlinkOps := new(utilmocks.NetLinkOps)
			netlinkOps.On("LinkByName", mock.Anything).Return(nil, netlink.LinkNotFoundError{})
			original := util.GetNetLinkOps()
			util.SetNetLinkOpMockInst(netlinkOps)
			t.Cleanup(func() { util.SetNetLinkOpMockInst(original) })
			del := newShimOVSClientForTest(t, server, CNIDel)
			names, err := findInterfacesWithSandbox(del, "sandbox")
			require.NoError(t, err)
			require.Len(t, names, attachments)
			pr := &PodRequest{CNIConf: &ovncnitypes.NetConf{}}
			for attempt := 0; attempt < 2; attempt++ {
				pr.deletePorts(del, names, "ns", "pod")
				require.NoError(t, clearPodBandwidth(del, "sandbox"))
			}
			require.Empty(t, del.Cache().Table(vswitchd.InterfaceTable).Rows())
			require.Empty(t, del.Cache().Table(vswitchd.PortTable).Rows())
			results, err := ovsops.TransactAndCheck(del, []ovsdb.Operation{{Op: ovsdb.OperationSelect, Table: vswitchd.QoSTable}})
			require.NoError(t, err)
			require.Empty(t, results[0].Rows)
		})
	}
}

func TestShimOVSWaitForBinding(t *testing.T) {
	for _, tc := range []struct {
		name, ifaceID, installed, wantErr string
	}{
		{name: "ready", ifaceID: "ns_pod", installed: "true"},
		{name: "replaced", ifaceID: "ns_other", installed: "true", wantErr: "no longer active"},
		{name: "not ready", ifaceID: "ns_pod", wantErr: "timed out"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, _ := newShimOVSTestServer(t, []libovsdbtest.TestData{
				&vswitchd.OpenvSwitch{UUID: "ovs", Bridges: []string{"br_int"}},
				&vswitchd.Bridge{UUID: "br_int", Name: "br-int", Ports: []string{"port"}},
				&vswitchd.Port{UUID: "port", Name: "pod-port", Interfaces: []string{"iface"}},
				&vswitchd.Interface{UUID: "iface", Name: "pod-port", ExternalIDs: map[string]string{
					"iface-id": tc.ifaceID, "ovn-installed": tc.installed,
				}},
			})
			c := newShimOVSClientForTest(t, server, CNIAdd)
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			err := waitForPodInterface(ctx, c, &PodInterfaceInfo{}, "pod-port", "ns_pod", nil, "ns", "pod", "uid")
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestShimOVSPortDoesNotWaitForVSwitchd(t *testing.T) {
	for _, dpdk := range []bool{false, true} {
		t.Run(fmt.Sprintf("dpdk=%t", dpdk), func(t *testing.T) {
			server, _ := newShimOVSTestServer(t, []libovsdbtest.TestData{
				&vswitchd.OpenvSwitch{UUID: "ovs", Bridges: []string{"br_int"}, NextCfg: 7, CurCfg: 6},
				&vswitchd.Bridge{UUID: "br_int", Name: "br-int"},
			})
			c := newShimOVSClientForTest(t, server, CNIAdd)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			// No vswitchd emulator: an unrelated pending configuration must not
			// prevent submitting the pod's port transaction.
			require.NoError(t, addOrUpdatePodPort(ctx, c, "pod-port",
				map[string]string{"iface-id": "ns_pod"}, dpdk, 1400, true))
			iface, err := ovsops.GetOVSInterface(c, "pod-port")
			require.NoError(t, err)
			if dpdk {
				// These fields are intentionally outside the shim monitor.
				require.Eventually(t, func() bool {
					actual, err := ovsops.GetOVSInterface(server, "pod-port")
					return err == nil && actual.Type == "dpdk" &&
						actual.MTURequest != nil && *actual.MTURequest == 1400
				}, time.Second, time.Millisecond)
			}
			require.NotContains(t, iface.ExternalIDs, "ovn-installed")
			root, err := ovsops.GetOpenvSwitch(server)
			require.NoError(t, err)
			require.Equal(t, 7, root.NextCfg)
			require.Equal(t, 6, root.CurCfg)
		})
	}
}

type shimReadinessPodGetter struct {
	pod     *corev1.Pod
	waiting chan struct{}
}

func (g *shimReadinessPodGetter) getPod(_, _ string) (*corev1.Pod, error) {
	select {
	case g.waiting <- struct{}{}:
	default:
	}
	return g.pod, nil
}

func TestShimOVSConfigureStillWaitsForBinding(t *testing.T) {
	for _, cancelBeforeReady := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel=%t", cancelBeforeReady), func(t *testing.T) {
			server, _ := newShimOVSTestServer(t, []libovsdbtest.TestData{
				&vswitchd.OpenvSwitch{UUID: "ovs", Bridges: []string{"br_int"}, NextCfg: 7, CurCfg: 6},
				&vswitchd.Bridge{UUID: "br_int", Name: "br-int"},
			})
			c := newShimOVSClientForTest(t, server, CNIAdd)
			info := &PodInterfaceInfo{PodUID: "uid", NetName: types.DefaultNetworkName, NADKey: types.DefaultNetworkName,
				PodAnnotation: util.PodAnnotation{MAC: net.HardwareAddr{0x0a, 0x58, 0x0a, 0x80, 0, 1}}}
			annotations, err := util.MarshalPodAnnotation(nil, &info.PodAnnotation, info.NADKey)
			require.NoError(t, err)
			getter := &shimReadinessPodGetter{
				pod:     &corev1.Pod{ObjectMeta: metav1.ObjectMeta{UID: "uid", Annotations: annotations}},
				waiting: make(chan struct{}, 1),
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			done := make(chan error, 1)
			finished := make(chan struct{})
			t.Cleanup(func() {
				cancel()
				<-finished
			})
			go func() {
				defer close(finished)
				done <- ConfigureOVS(ctx, c, "ns", "pod", "eth0", "pod-port",
					info, "sandbox", "", false, getter)
			}()
			// Observing the port alone does not mean the transaction has returned.
			// The sandbox lookup proves ConfigureOVS reached the readiness wait.
			select {
			case <-getter.waiting:
			case <-time.After(time.Second):
				t.Fatal("ConfigureOVS did not reach the readiness wait")
			}
			var iface *vswitchd.Interface
			require.Eventually(t, func() bool {
				var err error
				iface, err = ovsops.GetOVSInterface(server, "pod-port")
				return err == nil
			}, time.Second, time.Millisecond)
			select {
			case err := <-done:
				t.Fatalf("ConfigureOVS returned before OVN readiness: %v", err)
			default:
			}
			if cancelBeforeReady {
				cancel()
			} else {
				iface.ExternalIDs["ovn-installed"] = "true"
				ops, err := server.Where(&vswitchd.Interface{UUID: iface.UUID}).Update(iface, &iface.ExternalIDs)
				require.NoError(t, err)
				_, err = ovsops.TransactAndCheck(server, ops)
				require.NoError(t, err)
			}
			select {
			case err := <-done:
				if cancelBeforeReady {
					require.ErrorContains(t, err, "canceled while waiting for OVS port binding")
				} else {
					require.NoError(t, err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("ConfigureOVS did not finish after readiness or cancellation")
			}
			root, err := ovsops.GetOpenvSwitch(server)
			require.NoError(t, err)
			require.Equal(t, 7, root.NextCfg)
			require.Equal(t, 6, root.CurCfg)
		})
	}
}

func TestShimOVSInterfaceNotifications(t *testing.T) {
	server, _ := newShimOVSTestServer(t, []libovsdbtest.TestData{
		&vswitchd.OpenvSwitch{UUID: "ovs", Bridges: []string{"bridge"}},
		&vswitchd.Bridge{UUID: "bridge", Name: "br-int"},
	})
	c := newShimOVSClientForTest(t, server, CNIAdd)
	watcher, ok := c.(libovsdb.OVSInterfaceWatcher)
	require.True(t, ok)
	changes, stop := watcher.WatchOVSInterface("pod-port")
	defer stop()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	waitForChange := func(ch <-chan struct{}) {
		t.Helper()
		select {
		case <-ch:
		case <-ctx.Done():
			t.Fatal("no interface notification received")
		}
	}

	require.NoError(t, addOrUpdatePodPort(ctx, server, "pod-port", map[string]string{"iface-id": "ns_pod"}, false, 1400, false))
	waitForChange(changes)
	iface, err := ovsops.GetOVSInterface(c, "pod-port")
	require.NoError(t, err, "the cache must be updated before the callback")
	iface.ExternalIDs["ovn-installed"] = "true"
	ops, err := server.Where(iface).Update(iface, &iface.ExternalIDs)
	require.NoError(t, err)
	_, err = ovsops.TransactAndCheck(server, ops)
	require.NoError(t, err)
	waitForChange(changes)
	require.NoError(t, waitForPodInterface(ctx, c, &PodInterfaceInfo{}, "pod-port", "ns_pod", nil, "ns", "pod", "uid"))

	// A ready interface reassigned to another sandbox must wake the old waiter.
	iface.ExternalIDs["iface-id"] = "ns_other"
	ops, err = server.Where(iface).Update(iface, &iface.ExternalIDs)
	require.NoError(t, err)
	_, err = ovsops.TransactAndCheck(server, ops)
	require.NoError(t, err)
	waitForChange(changes)
	err = waitForPodInterface(ctx, c, &PodInterfaceInfo{}, "pod-port", "ns_pod", nil, "ns", "pod", "uid")
	require.ErrorContains(t, err, "no longer active")

	require.NoError(t, ovsops.DeleteMultiplePortsWithInterfaces(server, "br-int", "pod-port"))
	waitForChange(changes)
	_, err = ovsops.GetOVSInterface(c, "pod-port")
	require.ErrorIs(t, err, client.ErrNotFound)
}
