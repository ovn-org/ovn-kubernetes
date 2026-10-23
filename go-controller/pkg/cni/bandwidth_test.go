// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package cni

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	kexec "k8s.io/utils/exec"
	"k8s.io/utils/ptr"

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

func TestSetPodBandwidthWithOVSClient(t *testing.T) {
	const (
		ovsUUID          = "00000000-0000-0000-0000-000000000001"
		bridgeUUID       = "00000000-0000-0000-0000-000000000002"
		sandboxPortUUID  = "00000000-0000-0000-0000-000000000003"
		sandboxIfaceUUID = "00000000-0000-0000-0000-000000000004"
	)

	tests := []struct {
		desc                  string
		portExists            bool
		ingressBPS            int64
		egressBPS             int64
		expectedErr           bool
		expectedMaxRate       string
		expectedPolicingRate  int
		expectedPolicingBurst int
	}{
		{
			desc:                  "Positive test code path when both ingressBPS and egressBPS are greater than zero",
			portExists:            true,
			ingressBPS:            10000000,
			egressBPS:             5000000,
			expectedMaxRate:       "10000000",
			expectedPolicingRate:  5000,
			expectedPolicingBurst: 500,
		},
		{
			desc:            "Positive test code path when only ingressBPS is greater than zero",
			portExists:      true,
			ingressBPS:      10000000,
			expectedMaxRate: "10000000",
		},
		{
			desc:                  "Positive test code path when only egressBPS is greater than zero",
			portExists:            true,
			egressBPS:             5000000,
			expectedPolicingRate:  5000,
			expectedPolicingBurst: 500,
		},
		{
			desc:       "Positive test code path when neither ingressBPS nor egressBPS is set",
			portExists: true,
		},
		{
			desc:        "Negative test code path when the port does not exist",
			ingressBPS:  10000000,
			expectedErr: true,
		},
		{
			desc:        "Negative test code path when the interface does not exist",
			egressBPS:   5000000,
			expectedErr: true,
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			bridge := &vswitchd.Bridge{UUID: bridgeUUID, Name: "br-int"}
			data := []libovsdbtest.TestData{
				&vswitchd.OpenvSwitch{UUID: ovsUUID, Bridges: []string{bridgeUUID}},
				bridge,
			}
			if tc.portExists {
				bridge.Ports = []string{sandboxPortUUID}
				data = append(data,
					&vswitchd.Port{UUID: sandboxPortUUID, Name: "sandbox-port", Interfaces: []string{sandboxIfaceUUID}},
					&vswitchd.Interface{UUID: sandboxIfaceUUID, Name: "sandbox-port", ExternalIDs: map[string]string{"sandbox": "sandboxID"}},
				)
			}

			ovsClient, cleanup, err := libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{OVSData: data})
			require.NoError(t, err)
			t.Cleanup(cleanup.Cleanup)

			err = setPodBandwidth(ovsClient, "sandboxID", "sandbox-port", tc.ingressBPS, tc.egressBPS)
			if tc.expectedErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)

			port := &vswitchd.Port{UUID: sandboxPortUUID}
			require.NoError(t, ovsClient.Get(context.Background(), port))
			if tc.expectedMaxRate == "" {
				require.Nil(t, port.QOS)
			} else {
				require.NotNil(t, port.QOS)
				qos := &vswitchd.QoS{UUID: *port.QOS}
				require.NoError(t, ovsClient.Get(context.Background(), qos))
				assert.Equal(t, "linux-htb", qos.Type)
				assert.Equal(t, tc.expectedMaxRate, qos.OtherConfig["max-rate"])
				assert.Equal(t, "sandboxID", qos.ExternalIDs["sandbox"])
			}

			iface := &vswitchd.Interface{UUID: sandboxIfaceUUID}
			require.NoError(t, ovsClient.Get(context.Background(), iface))
			assert.Equal(t, tc.expectedPolicingRate, iface.IngressPolicingRate)
			assert.Equal(t, tc.expectedPolicingBurst, iface.IngressPolicingBurst)
		})
	}
}

func TestGetIngressPodBandwidth(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)

	tests := []struct {
		desc                string
		expectedErr         bool
		expectedNotFound    bool
		onRetArgsKexecIface []ovntest.TestifyMockHelper
		onRetArgsCmdList    []ovntest.TestifyMockHelper
		runnerInstance      kexec.Interface
		bps                 int64
	}{
		{
			desc: "Positive test code path when ingressBPS is correctly set",
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
			},
			onRetArgsCmdList: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{[]byte{1}, nil}},
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{[]byte("\"10000000\""), nil}},
			},
			runnerInstance: mockKexecIface,
			bps:            10000000,
		},
		{
			desc: "Positive test code path when ingressBPS is not set",
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
			},
			onRetArgsCmdList: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, nil}},
			},
			runnerInstance:   mockKexecIface,
			expectedNotFound: true,
		},
		{
			desc: "Positive test code path when ingressBPS is not set (no max-rate)",
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
			},
			onRetArgsCmdList: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{[]byte{1}, nil}},
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, nil}},
			},
			runnerInstance:   mockKexecIface,
			expectedNotFound: true,
		},
		{
			desc:        "Negative test code path when ovsGet 'port' returns error",
			expectedErr: true,
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
			},
			onRetArgsCmdList: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, fmt.Errorf("mock: failed to run ovsSet")}},
			},
			runnerInstance: mockKexecIface,
		},
		{
			desc:        "Negative test code path when ovsGet 'qos' returns error",
			expectedErr: true,
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
			},
			onRetArgsCmdList: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{[]byte{1}, nil}},
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, fmt.Errorf("mock: failed to run ovsSet")}},
			},
			runnerInstance: mockKexecIface,
		},
		{
			desc:        "Negative test code path when max-rate value cannot be transfer to integer",
			expectedErr: true,
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
			},
			onRetArgsCmdList: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{[]byte{1}, nil}},
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{[]byte("test"), nil}},
			},
			runnerInstance: mockKexecIface,
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			if tc.onRetArgsKexecIface != nil {
				for _, item := range tc.onRetArgsKexecIface {
					ifaceCall := mockKexecIface.On(item.OnCallMethodName)
					for _, arg := range item.OnCallMethodArgType {
						ifaceCall.Arguments = append(ifaceCall.Arguments, mock.AnythingOfType(arg))
					}
					for _, ret := range item.RetArgList {
						ifaceCall.ReturnArguments = append(ifaceCall.ReturnArguments, ret)
					}
					ifaceCall.Once()
				}
			}

			if tc.onRetArgsCmdList != nil {
				for _, item := range tc.onRetArgsCmdList {
					mockCall := mockCmd.On(item.OnCallMethodName)
					for _, arg := range item.OnCallMethodArgType {
						mockCall.Arguments = append(mockCall.Arguments, mock.AnythingOfType(arg))
					}
					for _, ret := range item.RetArgList {
						mockCall.ReturnArguments = append(mockCall.ReturnArguments, ret)
					}
					mockCall.Once()
				}
			}
			// note runner is defined in pkg/cni/ovs.go file
			runner = tc.runnerInstance
			bandwidth, e := getOvsPortBandwidth(nil, "ifname", Ingress)
			switch {
			case tc.expectedErr:
				require.Error(t, e)
			case tc.expectedNotFound:
				assert.Equal(t, e, BandwidthNotFound)
			default:
				require.NoError(t, e)
				assert.Equal(t, bandwidth, tc.bps)
			}
			mockCmd.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestGetIngressPodBandwidthWithOVSClient(t *testing.T) {
	const (
		ovsUUID    = "00000000-0000-0000-0000-000000000001"
		bridgeUUID = "00000000-0000-0000-0000-000000000002"
		portUUID   = "00000000-0000-0000-0000-000000000003"
		ifaceUUID  = "00000000-0000-0000-0000-000000000004"
		qosUUID    = "00000000-0000-0000-0000-000000000005"
	)

	tests := []struct {
		desc             string
		portExists       bool
		qos              *vswitchd.QoS
		expectedErr      bool
		expectedNotFound bool
		bps              int64
	}{
		{
			desc:       "Positive test code path when ingressBPS is correctly set",
			portExists: true,
			qos: &vswitchd.QoS{
				UUID:        qosUUID,
				Type:        "linux-htb",
				OtherConfig: map[string]string{"max-rate": "10000000"},
			},
			bps: 10000000,
		},
		{
			desc:             "Not found code path when the port has no QoS",
			portExists:       true,
			expectedNotFound: true,
		},
		{
			desc:             "Not found code path when the QoS has no max-rate",
			portExists:       true,
			qos:              &vswitchd.QoS{UUID: qosUUID, Type: "linux-htb"},
			expectedNotFound: true,
		},
		{
			desc:             "Not found code path when the port does not exist",
			expectedNotFound: true,
		},
		{
			desc:       "Negative test code path when max-rate value cannot be transfer to integer",
			portExists: true,
			qos: &vswitchd.QoS{
				UUID:        qosUUID,
				Type:        "linux-htb",
				OtherConfig: map[string]string{"max-rate": "test"},
			},
			expectedErr: true,
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			bridge := &vswitchd.Bridge{UUID: bridgeUUID, Name: "br-int"}
			data := []libovsdbtest.TestData{
				&vswitchd.OpenvSwitch{UUID: ovsUUID, Bridges: []string{bridgeUUID}},
				bridge,
			}
			if tc.portExists {
				port := &vswitchd.Port{UUID: portUUID, Name: "ifname", Interfaces: []string{ifaceUUID}}
				if tc.qos != nil {
					port.QOS = ptr.To(tc.qos.UUID)
					data = append(data, tc.qos)
				}
				bridge.Ports = []string{portUUID}
				data = append(data, port, &vswitchd.Interface{UUID: ifaceUUID, Name: "ifname"})
			}

			ovsClient, cleanup, err := libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{OVSData: data})
			require.NoError(t, err)
			t.Cleanup(cleanup.Cleanup)

			bandwidth, e := getOvsPortBandwidth(ovsClient, "ifname", Ingress)
			switch {
			case tc.expectedErr:
				require.Error(t, e)
				require.NotErrorIs(t, e, BandwidthNotFound)
			case tc.expectedNotFound:
				require.ErrorIs(t, e, BandwidthNotFound)
			default:
				require.NoError(t, e)
				assert.Equal(t, tc.bps, bandwidth)
			}
		})
	}
}

func TestGetEgressPodBandwidth(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)

	tests := []struct {
		desc                string
		expectedErr         bool
		expectedNotFound    bool
		onRetArgsKexecIface []ovntest.TestifyMockHelper
		onRetArgsCmdList    []ovntest.TestifyMockHelper
		runnerInstance      kexec.Interface
		bps                 int64
	}{
		{
			desc: "Positive test code path when egressBPS is correctly set",
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
			},
			onRetArgsCmdList: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{[]byte("10000"), nil}},
			},
			runnerInstance: mockKexecIface,
			bps:            10000000,
		},
		{
			desc: "Positive test code path when egressBPS is not set (no ingress_policing_rate)",
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
			},
			onRetArgsCmdList: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, nil}},
			},
			runnerInstance:   mockKexecIface,
			expectedNotFound: true,
		},
		{
			desc: "Positive test code path when egressBPS is not set",
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
			},
			onRetArgsCmdList: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{[]byte("0"), nil}},
			},
			runnerInstance:   mockKexecIface,
			expectedNotFound: true,
		},
		{
			desc:        "Negative test code path when ovsGet 'interface' returns error",
			expectedErr: true,
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
			},
			onRetArgsCmdList: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, fmt.Errorf("mock: failed to run ovsSet")}},
			},
			runnerInstance: mockKexecIface,
		},
		{ // cannot happen
			desc:        "Negative test code path when ingress_policing_rate cannot be transfer to integer ",
			expectedErr: true,
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
			},
			onRetArgsCmdList: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{[]byte("test"), nil}},
			},
			runnerInstance: mockKexecIface,
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			if tc.onRetArgsKexecIface != nil {
				for _, item := range tc.onRetArgsKexecIface {
					ifaceCall := mockKexecIface.On(item.OnCallMethodName)
					for _, arg := range item.OnCallMethodArgType {
						ifaceCall.Arguments = append(ifaceCall.Arguments, mock.AnythingOfType(arg))
					}
					for _, ret := range item.RetArgList {
						ifaceCall.ReturnArguments = append(ifaceCall.ReturnArguments, ret)
					}
					ifaceCall.Once()
				}
			}

			if tc.onRetArgsCmdList != nil {
				for _, item := range tc.onRetArgsCmdList {
					mockCall := mockCmd.On(item.OnCallMethodName)
					for _, arg := range item.OnCallMethodArgType {
						mockCall.Arguments = append(mockCall.Arguments, mock.AnythingOfType(arg))
					}
					for _, ret := range item.RetArgList {
						mockCall.ReturnArguments = append(mockCall.ReturnArguments, ret)
					}
					mockCall.Once()
				}
			}
			// note runner is defined in pkg/cni/ovs.go file
			runner = tc.runnerInstance
			bandwidth, e := getOvsPortBandwidth(nil, "ifname", Egress)
			switch {
			case tc.expectedErr:
				require.Error(t, e)
			case tc.expectedNotFound:
				assert.Equal(t, e, BandwidthNotFound)
			default:
				require.NoError(t, e)
				assert.Equal(t, bandwidth, tc.bps)
			}
			mockCmd.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestGetEgressPodBandwidthWithOVSClient(t *testing.T) {
	const (
		ovsUUID    = "00000000-0000-0000-0000-000000000001"
		bridgeUUID = "00000000-0000-0000-0000-000000000002"
		portUUID   = "00000000-0000-0000-0000-000000000003"
		ifaceUUID  = "00000000-0000-0000-0000-000000000004"
	)

	tests := []struct {
		desc                string
		ifaceExists         bool
		ingressPolicingRate int
		expectedNotFound    bool
		bps                 int64
	}{
		{
			desc:                "Positive test code path when egressBPS is correctly set",
			ifaceExists:         true,
			ingressPolicingRate: 10000,
			bps:                 10000000,
		},
		{
			desc:             "Not found code path when ingress_policing_rate is not set",
			ifaceExists:      true,
			expectedNotFound: true,
		},
		{
			desc:             "Not found code path when the interface does not exist",
			expectedNotFound: true,
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			bridge := &vswitchd.Bridge{UUID: bridgeUUID, Name: "br-int"}
			data := []libovsdbtest.TestData{
				&vswitchd.OpenvSwitch{UUID: ovsUUID, Bridges: []string{bridgeUUID}},
				bridge,
			}
			if tc.ifaceExists {
				bridge.Ports = []string{portUUID}
				data = append(data,
					&vswitchd.Port{UUID: portUUID, Name: "ifname", Interfaces: []string{ifaceUUID}},
					&vswitchd.Interface{UUID: ifaceUUID, Name: "ifname", IngressPolicingRate: tc.ingressPolicingRate},
				)
			}

			ovsClient, cleanup, err := libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{OVSData: data})
			require.NoError(t, err)
			t.Cleanup(cleanup.Cleanup)

			bandwidth, e := getOvsPortBandwidth(ovsClient, "ifname", Egress)
			if tc.expectedNotFound {
				require.ErrorIs(t, e, BandwidthNotFound)
				return
			}
			require.NoError(t, e)
			assert.Equal(t, tc.bps, bandwidth)
		})
	}
}
