// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ops

import (
	"testing"

	"github.com/onsi/gomega"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
)

// TestIsMacBindingControllerOwned covers the ownership predicate: a binding is
// owned only on a gateway router external port and only for a non-masquerade IP.
func TestIsMacBindingControllerOwned(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())

	gwPort := types.GWRouterToExtSwitchPrefix + "GR_udnA_node1"
	masqIP := config.Gateway.MasqueradeIPs.V4HostMasqueradeIP.String()

	tests := []struct {
		name string
		port string
		ip   string
		want bool
	}{
		{"gateway external port, regular IP is owned", gwPort, "10.0.0.10", true},
		{"gateway external port, gateway masquerade IP is not owned", gwPort, masqIP, false},
		{"non-gateway port, regular IP is not owned", "some-other-port", "10.0.0.10", false},
		{"non-gateway port, masquerade IP is not owned", "some-other-port", masqIP, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			g.Expect(IsMacBindingControllerOwned(tt.port, tt.ip)).To(gomega.Equal(tt.want))
		})
	}
}

// TestCreateOrUpdateStaticMacBinding verifies the guard rejects controller-owned
// bindings (which must go through the MAC binding controller) while still writing
// the gateway's own masquerade bindings.
func TestCreateOrUpdateStaticMacBinding(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())

	nbClient, cleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{}, nil)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer cleanup.Cleanup()

	gwPort := types.GWRouterToExtSwitchPrefix + "GR_udnA_node1"
	masqIP := config.Gateway.MasqueradeIPs.V4HostMasqueradeIP.String()

	// A controller-owned binding (gateway external port, non-masquerade IP) is rejected.
	err = CreateOrUpdateStaticMacBinding(nbClient, &nbdb.StaticMACBinding{
		LogicalPort: gwPort, IP: "10.0.0.10", MAC: "0a:00:00:00:00:01", OverrideDynamicMAC: true,
	})
	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("managed by the MAC binding controller")))

	// The gateway's own masquerade binding on the same port is written.
	g.Expect(CreateOrUpdateStaticMacBinding(nbClient, &nbdb.StaticMACBinding{
		LogicalPort: gwPort, IP: masqIP, MAC: "0a:00:00:00:00:02", OverrideDynamicMAC: true,
	})).To(gomega.Succeed())
	g.Eventually(nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(
		&nbdb.StaticMACBinding{LogicalPort: gwPort, IP: masqIP, MAC: "0a:00:00:00:00:02", OverrideDynamicMAC: true},
	))
}
