// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package statusmetrics

import (
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
)

// RegisterConfiguredRollers registers resource rollers for features that are
// enabled and implemented.
func RegisterConfiguredRollers(m *Manager) {
	if m == nil {
		return
	}
	if config.OVNKubernetesFeature.EnableEgressFirewall {
		m.RegisterRoller(NewEgressFirewallRoller(
			m.WatchFactory().EgressFirewallInformer().Lister(),
			m.WatchFactory().NodeCoreInformer().Lister(),
			m.OVNClient().EgressFirewallClient,
			m.NetworkManager(),
		))
	}
	if config.OVNKubernetesFeature.EnableEgressQoS {
		m.RegisterRoller(NewEgressQoSRoller(
			m.WatchFactory().EgressQoSInformer().Lister(),
			m.OVNClient().EgressQoSClient,
		))
	}
	if config.OVNKubernetesFeature.EnableNetworkQoS {
		m.RegisterRoller(NewNetworkQoSRoller(
			m.WatchFactory().NetworkQoSInformer().Lister(),
			m.WatchFactory().NodeCoreInformer().Lister(),
			m.OVNClient().NetworkQoSClient,
			m.NetworkManager(),
		))
	}
	if config.OVNKubernetesFeature.EnableMultiExternalGateway {
		m.RegisterRoller(NewAPBExternalRouteRoller(
			m.WatchFactory().APBRouteInformer().Lister(),
			m.OVNClient().AdminPolicyRouteClient,
		))
	}
	if config.OVNKubernetesFeature.EnableAdminNetworkPolicy {
		m.RegisterRoller(NewAdminNetworkPolicyRoller(
			m.WatchFactory().ANPInformer().Lister(),
			m.OVNClient().ANPClient,
		))
		m.RegisterRoller(NewBaselineAdminNetworkPolicyRoller(
			m.WatchFactory().BANPInformer().Lister(),
			m.OVNClient().ANPClient,
		))
	}
}
