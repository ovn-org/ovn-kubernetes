// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package services

import (
	"net"
	"testing"

	"github.com/stretchr/testify/require"

	globalconfig "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
)

func TestNodePortHostAddressesStr(t *testing.T) {
	t.Parallel()

	hostAddresses := []net.IP{net.ParseIP("10.0.0.5"), net.ParseIP("192.168.1.5")}
	primaryAddresses := []net.IP{net.ParseIP("10.0.0.5")}
	ni := &nodeInfo{
		hostAddresses:      hostAddresses,
		l3gatewayAddresses: primaryAddresses,
	}

	t.Run("unrestricted", func(t *testing.T) {
		t.Cleanup(func() {
			globalconfig.Gateway.NodePortAddresses = nil
		})
		cfg, err := globalconfig.ParseNodePortAddresses("")
		require.NoError(t, err, "empty nodeport-addresses should parse successfully")
		globalconfig.Gateway.NodePortAddresses = cfg

		require.Equal(t, []string{"10.0.0.5", "192.168.1.5"}, ni.nodePortHostAddressesStr(),
			"unrestricted configuration should expose all host addresses")
	})

	t.Run("primary", func(t *testing.T) {
		t.Cleanup(func() {
			globalconfig.Gateway.NodePortAddresses = nil
		})
		cfg, err := globalconfig.ParseNodePortAddresses("primary")
		require.NoError(t, err, "primary selector should parse successfully")
		globalconfig.Gateway.NodePortAddresses = cfg

		require.Equal(t, []string{"10.0.0.5"}, ni.nodePortHostAddressesStr(),
			"primary selector should expose only gateway addresses")
	})

	t.Run("cidr", func(t *testing.T) {
		t.Cleanup(func() {
			globalconfig.Gateway.NodePortAddresses = nil
		})
		cfg, err := globalconfig.ParseNodePortAddresses("192.168.0.0/16")
		require.NoError(t, err, "CIDR selector should parse successfully")
		globalconfig.Gateway.NodePortAddresses = cfg

		require.Equal(t, []string{"192.168.1.5"}, ni.nodePortHostAddressesStr(),
			"CIDR selector should expose only matching host addresses")
	})

	t.Run("no match", func(t *testing.T) {
		t.Cleanup(func() {
			globalconfig.Gateway.NodePortAddresses = nil
		})
		cfg, err := globalconfig.ParseNodePortAddresses("172.16.0.0/12")
		require.NoError(t, err, "CIDR selector should parse successfully")
		globalconfig.Gateway.NodePortAddresses = cfg

		require.Empty(t, ni.nodePortHostAddressesStr(),
			"unmatched CIDR selector should expose no NodePort addresses")
	})
}
