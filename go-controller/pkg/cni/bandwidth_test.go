// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package cni

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	kexec "k8s.io/utils/exec"

	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	ovsops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	mock_k8s_io_utils_exec "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/mocks/k8s.io/utils/exec"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
)

func TestClearPodBandwidth(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)

	tests := []struct {
		desc                string
		expectedErr         bool
		onRetArgsKexecIface []ovntest.TestifyMockHelper
		onRetArgsCmdList    []ovntest.TestifyMockHelper
		runnerInstance      kexec.Interface
	}{
		{
			desc:        "Test error code path when ovsFind attempts to retrieve interfaces",
			expectedErr: true,
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
			},
			onRetArgsCmdList: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, fmt.Errorf("mock: failed to run ovsFind")}},
			},
			runnerInstance: mockKexecIface,
		},
		{
			desc:        "Test code path when ovsClear returns an error",
			expectedErr: true,
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
			},
			onRetArgsCmdList: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{[]byte{1}, nil}},
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, fmt.Errorf("mock: failed to run ovsClear")}},
			},
			runnerInstance: mockKexecIface,
		},
		{
			desc:        "Test error code path when ovsFind attempts to retrieve qos instances",
			expectedErr: true,
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
			},
			onRetArgsCmdList: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, nil}},
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, fmt.Errorf("mock: failed to run ovsFind")}},
			},
			runnerInstance: mockKexecIface,
		},
		{
			desc:        "Test code path when ovsDestroy returns an error",
			expectedErr: true,
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
			},
			onRetArgsCmdList: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{[]byte{1}, nil}},
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, nil}},
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{[]byte{1}, nil}},
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, fmt.Errorf("mock: failed to run ovsDestroy")}},
			},
			runnerInstance: mockKexecIface,
		},
		{
			desc: "Positive test code path",
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
			},
			onRetArgsCmdList: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{[]byte{1}, nil}},
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, nil}},
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{[]byte{1}, nil}},
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, nil}},
			},
			runnerInstance: mockKexecIface,
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			if tc.onRetArgsKexecIface != nil {
				ovntest.ProcessMockFnList(&mockKexecIface.Mock, tc.onRetArgsKexecIface)
			}
			if tc.onRetArgsCmdList != nil {
				ovntest.ProcessMockFnList(&mockCmd.Mock, tc.onRetArgsCmdList)
			}
			// note runner is defined in pkg/cni/ovs.go file
			runner = tc.runnerInstance

			e := clearPodBandwidth(nil, "sandboxID")

			if tc.expectedErr {
				require.Error(t, e)
			} else {
				require.NoError(t, e)
			}

			mockCmd.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestClearPodBandwidthWithOVSClient(t *testing.T) {
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
			&vswitchd.Interface{UUID: sandboxIfaceUUID, Name: "sandbox-port", ExternalIDs: map[string]string{"sandbox": "sandboxID"}},
			&vswitchd.QoS{UUID: qosUUID, Type: "linux-htb", ExternalIDs: map[string]string{"sandbox": "sandboxID"}},
			&vswitchd.Port{UUID: otherPortUUID, Name: "other-port", Interfaces: []string{otherIfaceUUID}, QOS: &otherQOSUUID},
			&vswitchd.Interface{UUID: otherIfaceUUID, Name: "other-port", ExternalIDs: map[string]string{"sandbox": "other-sandbox"}},
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

	otherPort := &vswitchd.Port{UUID: otherPortUUID}
	require.NoError(t, ovsClient.Get(context.Background(), otherPort))
	require.NotNil(t, otherPort.QOS)
	require.Equal(t, otherQOSUUID, *otherPort.QOS)
	require.True(t, qosExists(otherQOSUUID), "expected unrelated QoS to be preserved")
}

func TestSetPodBandwidthWithOVSClient(t *testing.T) {
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

			require.NoError(t, setPodBandwidth(
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

func TestSetPodBandwidth(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)

	tests := []struct {
		desc                string
		expectedErr         bool
		onRetArgsKexecIface []ovntest.TestifyMockHelper
		onRetArgsCmdList    []ovntest.TestifyMockHelper
		runnerInstance      kexec.Interface
		egressBPS           int64
	}{
		{
			desc:        "Test code path when both ingressBPS is greater than zero and ovsCreate returns an error",
			expectedErr: true,
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}}},
			onRetArgsCmdList: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, fmt.Errorf("mock: failed to run ovsCreate")}}},
			runnerInstance: mockKexecIface,
			egressBPS:      0,
		},
		{
			desc:        "Test code path when inressBPS is greater than zero and ovsSet returns an error",
			expectedErr: true,
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
			},
			onRetArgsCmdList: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, nil}},
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, fmt.Errorf("mock: failed to run ovsSet")}},
			},
			runnerInstance: mockKexecIface,
			egressBPS:      0,
		},
		{
			desc: "Positive test code path when ingressBPS is greater than zero",
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
			},
			onRetArgsCmdList: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, nil}},
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, nil}},
			},
			runnerInstance: mockKexecIface,
			egressBPS:      0,
		},
		{
			desc:        "Negative test code path when setting ingress_policing_rate",
			expectedErr: true,
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
			},
			onRetArgsCmdList: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, nil}},
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, nil}},
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, fmt.Errorf("mock: failed to run ovsSet")}},
			},
			runnerInstance: mockKexecIface,
			egressBPS:      3,
		},
		{
			desc:        "Negative test code path when setting ingress_policing_burst",
			expectedErr: true,
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
			},
			onRetArgsCmdList: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, nil}},
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, nil}},
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, nil}},
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, fmt.Errorf("mock: failed to run ovsSet")}},
			},
			runnerInstance: mockKexecIface,
			egressBPS:      3,
		},
		{
			desc: "Positive test code path when both ingressBPS and egressBPS are greater than zero",
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
			},
			onRetArgsCmdList: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, nil}},
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, nil}},
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, nil}},
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, nil}},
			},
			runnerInstance: mockKexecIface,
			egressBPS:      3,
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			if tc.onRetArgsKexecIface != nil {
				ovntest.ProcessMockFnList(&mockKexecIface.Mock, tc.onRetArgsKexecIface)
			}

			if tc.onRetArgsCmdList != nil {
				ovntest.ProcessMockFnList(&mockCmd.Mock, tc.onRetArgsCmdList)
			}
			// note runner is defined in pkg/cni/ovs.go file
			runner = tc.runnerInstance

			e := setPodBandwidth(nil, "sandboxID", "ifname", 1, tc.egressBPS)

			if tc.expectedErr {
				require.Error(t, e)
			} else {
				require.NoError(t, e)
			}

			mockCmd.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}
