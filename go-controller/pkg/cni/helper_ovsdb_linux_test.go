// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package cni

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	ovsops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
)

func TestAddOrUpdatePodPortCanceled(t *testing.T) {
	ovsClient, cleanup, err := libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{
		OVSData: []libovsdbtest.TestData{
			&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{"bridge"}},
			&vswitchd.Bridge{UUID: "bridge", Name: "br-int"},
		},
	})
	require.NoError(t, err)
	t.Cleanup(cleanup.Cleanup)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = addOrUpdatePodPort(ctx, ovsClient, "veth-canceled", map[string]string{"iface-id": "ns_pod"}, false, 1400, false)
	require.ErrorIs(t, err, context.Canceled)

	_, err = ovsops.GetOVSPort(ovsClient, "veth-canceled")
	require.ErrorIs(t, err, libovsdbclient.ErrNotFound)
	ovs, err := ovsops.GetOpenvSwitch(ovsClient)
	require.NoError(t, err)
	require.Zero(t, ovs.NextCfg, "a canceled request must not submit the port transaction")
}

func TestOVSClientInterfaceLookups(t *testing.T) {
	const (
		rootUUID           = "00000000-0000-0000-0000-000000000001"
		bridgeUUID         = "00000000-0000-0000-0000-000000000002"
		defaultPortUUID    = "00000000-0000-0000-0000-000000000003"
		defaultIfaceUUID   = "00000000-0000-0000-0000-000000000004"
		secondaryPortUUID  = "00000000-0000-0000-0000-000000000005"
		secondaryIfaceUUID = "00000000-0000-0000-0000-000000000006"
		otherPortUUID      = "00000000-0000-0000-0000-000000000007"
		otherIfaceUUID     = "00000000-0000-0000-0000-000000000008"
	)
	ovsClient, cleanup, err := libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{
		OVSData: []libovsdbtest.TestData{
			&vswitchd.OpenvSwitch{UUID: rootUUID, Bridges: []string{bridgeUUID}},
			&vswitchd.Bridge{UUID: bridgeUUID, Name: "br-int", Ports: []string{
				defaultPortUUID,
				secondaryPortUUID,
				otherPortUUID,
			}},
			&vswitchd.Port{UUID: defaultPortUUID, Name: "veth-default", Interfaces: []string{defaultIfaceUUID}},
			&vswitchd.Interface{
				UUID: defaultIfaceUUID,
				Name: "veth-default",
				ExternalIDs: map[string]string{
					"sandbox":     "sandbox-id",
					"pod-if-name": "eth0",
					"test-key":    "default-value",
				},
			},
			&vswitchd.Port{UUID: secondaryPortUUID, Name: "veth-blue", Interfaces: []string{secondaryIfaceUUID}},
			&vswitchd.Interface{
				UUID: secondaryIfaceUUID,
				Name: "veth-blue",
				ExternalIDs: map[string]string{
					"sandbox":           "sandbox-id",
					"pod-if-name":       "net1",
					types.NADExternalID: "ns/blue",
					"test-key":          "secondary-value",
				},
			},
			&vswitchd.Port{UUID: otherPortUUID, Name: "veth-other", Interfaces: []string{otherIfaceUUID}},
			&vswitchd.Interface{
				UUID: otherIfaceUUID,
				Name: "veth-other",
				ExternalIDs: map[string]string{
					"sandbox":     "other-sandbox",
					"pod-if-name": "eth0",
				},
			},
		},
	})
	require.NoError(t, err)
	t.Cleanup(cleanup.Cleanup)

	names, err := findInterfacesWithSandbox(ovsClient, "sandbox-id")
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"veth-default", "veth-blue"}, names)

	names, err = findPodInterfaces(ovsClient, "sandbox-id", "eth0", types.DefaultNetworkName, types.DefaultNetworkName)
	require.NoError(t, err)
	require.Equal(t, []string{"veth-default"}, names)

	names, err = findPodInterfaces(ovsClient, "sandbox-id", "missing", "blue", "ns/blue")
	require.NoError(t, err)
	require.Equal(t, []string{"veth-blue"}, names)

	names, err = findPodInterfaces(ovsClient, "sandbox-id", "missing", types.DefaultNetworkName, types.DefaultNetworkName)
	require.NoError(t, err)
	require.Equal(t, []string{"veth-default"}, names)

	value, err := getInterfaceExternalID(ovsClient, "veth-blue", "test-key")
	require.NoError(t, err)
	require.Equal(t, "secondary-value", value)
}
