// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package cni

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	ovsops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
)

func setPodBandwidthForTest(ovsClient client.Client, sandboxID, ifname string, ingressBPS, egressBPS int64) error {
	ops, err := setPodBandwidthOps(ovsClient, nil, sandboxID, ifname, ingressBPS, egressBPS)
	if err != nil {
		return err
	}
	_, err = ovsops.TransactAndCheck(ovsClient, ops)
	return err
}

func TestClearPodBandwidth(t *testing.T) {
	ovsUUID := "00000000-0000-0000-0000-000000000001"
	bridgeUUID := "00000000-0000-0000-0000-000000000002"
	sandboxPortUUID := "00000000-0000-0000-0000-000000000003"
	sandboxIfaceUUID := "00000000-0000-0000-0000-000000000004"
	qosUUID := "00000000-0000-0000-0000-000000000005"
	otherPortUUID := "00000000-0000-0000-0000-000000000006"
	otherIfaceUUID := "00000000-0000-0000-0000-000000000007"
	otherQOSUUID := "00000000-0000-0000-0000-000000000008"
	ovsClient, cleanup, err := libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{
		OVSData: []libovsdbtest.TestData{
			&vswitchd.OpenvSwitch{UUID: ovsUUID, Bridges: []string{bridgeUUID}},
			&vswitchd.Bridge{UUID: bridgeUUID, Name: "br-int", Ports: []string{sandboxPortUUID, otherPortUUID}},
			&vswitchd.Port{UUID: sandboxPortUUID, Name: "sandbox-port", Interfaces: []string{sandboxIfaceUUID}, QOS: &qosUUID},
			&vswitchd.Interface{UUID: sandboxIfaceUUID, Name: "sandbox-port", ExternalIDs: map[string]string{"sandbox": "sandboxID"}, IngressPolicingRate: 2000, IngressPolicingBurst: 200},
			&vswitchd.QoS{UUID: qosUUID, Type: "linux-htb", ExternalIDs: map[string]string{"sandbox": "sandboxID"}},
			&vswitchd.Port{UUID: otherPortUUID, Name: "other-port", Interfaces: []string{otherIfaceUUID}, QOS: &otherQOSUUID},
			&vswitchd.Interface{UUID: otherIfaceUUID, Name: "other-port", ExternalIDs: map[string]string{"sandbox": "other-sandbox"}, IngressPolicingRate: 3000, IngressPolicingBurst: 300},
			&vswitchd.QoS{UUID: otherQOSUUID, Type: "linux-htb", ExternalIDs: map[string]string{"sandbox": "other-sandbox"}},
		},
	})
	require.NoError(t, err)
	t.Cleanup(cleanup.Cleanup)
	qosExists := func(uuid string) bool {
		results, err := ovsops.TransactAndCheck(ovsClient, []ovsdb.Operation{{
			Op:    ovsdb.OperationSelect,
			Table: vswitchd.QoSTable,
			Where: []ovsdb.Condition{
				ovsdb.NewCondition("_uuid", ovsdb.ConditionEqual, ovsdb.UUID{GoUUID: uuid}),
			},
		}})
		require.NoError(t, err)
		require.Len(t, results, 1)
		return len(results[0].Rows) > 0
	}

	require.NoError(t, clearPodBandwidth(ovsClient, "sandboxID"))

	sandboxPort := &vswitchd.Port{UUID: sandboxPortUUID}
	require.NoError(t, ovsClient.Get(context.Background(), sandboxPort))
	require.Nil(t, sandboxPort.QOS)

	require.False(t, qosExists(qosUUID), "expected sandbox QoS to be deleted")
	sandboxIface, err := ovsops.GetOVSInterface(ovsClient, "sandbox-port")
	require.NoError(t, err)
	require.Equal(t, 2000, sandboxIface.IngressPolicingRate)
	require.Equal(t, 200, sandboxIface.IngressPolicingBurst)

	otherPort := &vswitchd.Port{UUID: otherPortUUID}
	require.NoError(t, ovsClient.Get(context.Background(), otherPort))
	require.NotNil(t, otherPort.QOS)
	require.Equal(t, otherQOSUUID, *otherPort.QOS)
	require.True(t, qosExists(otherQOSUUID), "expected unrelated QoS to be preserved")
	otherIface, err := ovsops.GetOVSInterface(ovsClient, "other-port")
	require.NoError(t, err)
	require.Equal(t, 3000, otherIface.IngressPolicingRate)
	require.Equal(t, 300, otherIface.IngressPolicingBurst)
}

func TestClearPodBandwidthWithoutMatchingPort(t *testing.T) {
	for _, hasInterface := range []bool{false, true} {
		t.Run(fmt.Sprintf("interface=%t", hasInterface), func(t *testing.T) {
			bridge := &vswitchd.Bridge{UUID: "bridge", Name: "br-int"}
			data := []libovsdbtest.TestData{
				&vswitchd.OpenvSwitch{UUID: "ovs", Bridges: []string{bridge.UUID}},
				bridge,
				&vswitchd.QoS{UUID: "old_qos", Type: "linux-htb", ExternalIDs: map[string]string{"sandbox": "sandbox"}},
			}
			if hasInterface {
				// An interface may belong to a differently named port. Cleanup
				// must still remove orphaned QoS when its name has no Port row.
				bridge.Ports = []string{"bond"}
				data = append(data,
					&vswitchd.Port{UUID: "bond", Name: "bond", Interfaces: []string{"member"}},
					&vswitchd.Interface{UUID: "member", Name: "member", ExternalIDs: map[string]string{"sandbox": "sandbox"}},
				)
			}
			ovsClient, _ := newShimOVSTestServer(t, data)
			require.NoError(t, clearPodBandwidth(ovsClient, "sandbox"))
			results, err := ovsops.TransactAndCheck(ovsClient, []ovsdb.Operation{{Op: ovsdb.OperationSelect, Table: vswitchd.QoSTable}})
			require.NoError(t, err)
			require.Empty(t, results[0].Rows)
			if hasInterface {
				_, err = ovsops.GetOVSInterface(ovsClient, "member")
				require.NoError(t, err)
				_, err = ovsops.GetOVSPort(ovsClient, "bond")
				require.NoError(t, err)
			}
		})
	}
}

func TestReconfigurePodBandwidth(t *testing.T) {
	for _, test := range []struct {
		name       string
		ingressBPS int64
		egressBPS  int64
	}{
		{name: "remove egress limit", ingressBPS: 10_000_000},
		{name: "remove ingress limit", egressBPS: 3_000_000},
		{name: "remove both limits"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ovsClient, cleanup, err := libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{
				OVSData: []libovsdbtest.TestData{
					&vswitchd.OpenvSwitch{UUID: "root", Bridges: []string{"bridge"}},
					&vswitchd.Bridge{UUID: "bridge", Name: "br-int", Ports: []string{"port"}},
					&vswitchd.Port{UUID: "port", Name: "pod-port", Interfaces: []string{"iface"}},
					&vswitchd.Interface{UUID: "iface", Name: "pod-port", ExternalIDs: map[string]string{"sandbox": "sandboxID"}},
				},
			})
			require.NoError(t, err)
			t.Cleanup(cleanup.Cleanup)
			require.NoError(t, setPodBandwidthForTest(ovsClient, "sandboxID", "pod-port", 5_000_000, 2_000_000))

			// ConfigureOVS applies the new limits even when both annotations are absent.
			require.NoError(t, clearPodBandwidth(ovsClient, "sandboxID"))
			require.NoError(t, setPodBandwidthForTest(ovsClient, "sandboxID", "pod-port", test.ingressBPS, test.egressBPS))

			port, err := ovsops.GetOVSPort(ovsClient, "pod-port")
			require.NoError(t, err)
			require.Equal(t, test.ingressBPS > 0, port.QOS != nil)
			iface, err := ovsops.GetOVSInterface(ovsClient, "pod-port")
			require.NoError(t, err)
			require.Equal(t, int(test.egressBPS/1000), iface.IngressPolicingRate)
			require.Equal(t, int(test.egressBPS/10000), iface.IngressPolicingBurst)
			results, err := ovsops.TransactAndCheck(ovsClient, []ovsdb.Operation{{Op: ovsdb.OperationSelect, Table: vswitchd.QoSTable}})
			require.NoError(t, err)
			if test.ingressBPS > 0 {
				require.Len(t, results[0].Rows, 1, "only the replacement QoS should remain")
			} else {
				require.Empty(t, results[0].Rows)
			}
		})
	}
}

func TestSetPodBandwidth(t *testing.T) {
	tests := []struct {
		name              string
		ingressBPS        int64
		egressBPS         int64
		expectQOS         bool
		expectedRateKBPS  int
		expectedBurstKBPS int
	}{
		{
			name:              "ingress and egress",
			ingressBPS:        10_000_000,
			egressBPS:         2_000_000,
			expectQOS:         true,
			expectedRateKBPS:  2000,
			expectedBurstKBPS: 200,
		},
		{
			name:       "ingress only",
			ingressBPS: 10_000_000,
			expectQOS:  true,
		},
		{
			name:              "egress only",
			egressBPS:         2_000_000,
			expectedRateKBPS:  2000,
			expectedBurstKBPS: 200,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			const (
				ovsUUID    = "00000000-0000-0000-0000-000000000001"
				bridgeUUID = "00000000-0000-0000-0000-000000000002"
				portUUID   = "00000000-0000-0000-0000-000000000003"
				ifaceUUID  = "00000000-0000-0000-0000-000000000004"
			)
			ovsClient, cleanup, err := libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{
				OVSData: []libovsdbtest.TestData{
					&vswitchd.OpenvSwitch{UUID: ovsUUID, Bridges: []string{bridgeUUID}},
					&vswitchd.Bridge{UUID: bridgeUUID, Name: "br-int", Ports: []string{portUUID}},
					&vswitchd.Port{UUID: portUUID, Name: "pod-port", Interfaces: []string{ifaceUUID}},
					&vswitchd.Interface{UUID: ifaceUUID, Name: "pod-port"},
				},
			})
			require.NoError(t, err)
			t.Cleanup(cleanup.Cleanup)

			require.NoError(t, setPodBandwidthForTest(
				ovsClient, "sandboxID", "pod-port", test.ingressBPS, test.egressBPS))

			port := &vswitchd.Port{UUID: portUUID}
			if test.expectQOS {
				require.Eventually(t, func() bool {
					return ovsClient.Get(context.Background(), port) == nil && port.QOS != nil
				}, time.Second, 10*time.Millisecond)
				require.True(t, ovsdb.IsValidUUID(*port.QOS))
				results, err := ovsops.TransactAndCheck(ovsClient, []ovsdb.Operation{{
					Op:    ovsdb.OperationSelect,
					Table: vswitchd.QoSTable,
					Where: []ovsdb.Condition{
						ovsdb.NewCondition("_uuid", ovsdb.ConditionEqual, ovsdb.UUID{GoUUID: *port.QOS}),
					},
				}})
				require.NoError(t, err)
				require.Len(t, results, 1)
				require.Len(t, results[0].Rows, 1)
				qosRow := results[0].Rows[0]
				require.Equal(t, "linux-htb", qosRow["type"])
				otherConfig, ok := qosRow["other_config"].(ovsdb.OvsMap)
				require.True(t, ok)
				require.Equal(t, fmt.Sprint(test.ingressBPS), otherConfig.GoMap["max-rate"])
				externalIDs, ok := qosRow["external_ids"].(ovsdb.OvsMap)
				require.True(t, ok)
				require.Equal(t, "sandboxID", externalIDs.GoMap["sandbox"])
			} else {
				require.NoError(t, ovsClient.Get(context.Background(), port))
				require.Nil(t, port.QOS)
			}

			iface := &vswitchd.Interface{UUID: ifaceUUID}
			require.NoError(t, ovsClient.Get(context.Background(), iface))
			require.Equal(t, test.expectedRateKBPS, iface.IngressPolicingRate)
			require.Equal(t, test.expectedBurstKBPS, iface.IngressPolicingBurst)
		})
	}
}
