// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package cudn

import "github.com/ovn-kubernetes/ovn-kubernetes/test/crd-integration/testscenario"

var NoOverlayValid = []testscenario.ValidateCRScenario{
	{
		Description: "NoOverlay transport with managed routing and enabled SNAT",
		Name:        "no-overlay-managed-enabled-snat",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: no-overlay-managed-enabled-snat
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer3
    layer3:
      role: Primary
      mtu: 1500
      subnets:
      - cidr: 10.10.0.0/16
        hostSubnet: 24
    transport: NoOverlay
    noOverlay:
      outboundSNAT: Enabled
      routing: Managed
`,
	},
	{
		Description: "NoOverlay transport with unmanaged routing and disabled SNAT",
		Name:        "no-overlay-unmanaged-disabled-snat",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: no-overlay-unmanaged-disabled-snat
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: blue}}
  network:
    topology: Layer3
    layer3:
      role: Primary
      mtu: 1500
      subnets:
      - cidr: 10.20.0.0/16
        hostSubnet: 24
    transport: NoOverlay
    noOverlay:
      outboundSNAT: Disabled
      routing: Unmanaged
`,
	},
	{
		Description: "NoOverlay transport with managed routing and disabled SNAT",
		Name:        "no-overlay-managed-disabled-snat",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: no-overlay-managed-disabled-snat
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: green}}
  network:
    topology: Layer3
    layer3:
      role: Primary
      mtu: 1500
      subnets:
      - cidr: 10.30.0.0/16
        hostSubnet: 24
    transport: NoOverlay
    noOverlay:
      outboundSNAT: Disabled
      routing: Managed
`,
	},
	{
		Description: "NoOverlay transport with unmanaged routing and enabled SNAT",
		Name:        "no-overlay-unmanaged-enabled-snat",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: no-overlay-unmanaged-enabled-snat
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: yellow}}
  network:
    topology: Layer3
    layer3:
      role: Primary
      mtu: 1500
      subnets:
      - cidr: 10.40.0.0/16
        hostSubnet: 24
    transport: NoOverlay
    noOverlay:
      outboundSNAT: Enabled
      routing: Unmanaged
`,
	},
	{
		Description: "NoOverlay transport with dual-stack subnets",
		Name:        "no-overlay-dual-stack",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: no-overlay-dual-stack
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: purple}}
  network:
    topology: Layer3
    layer3:
      role: Primary
      mtu: 1500
      subnets:
      - cidr: 10.50.0.0/16
        hostSubnet: 24
      - cidr: fd00:10:50::/48
        hostSubnet: 64
    transport: NoOverlay
    noOverlay:
      outboundSNAT: Enabled
      routing: Managed
`,
	},
	{
		Description: "Layer3 primary network with unset default transport (no transport field set)",
		Name:        "unset-default-transport",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: unset-default-transport
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: orange}}
  network:
    topology: Layer3
    layer3:
      role: Primary
      mtu: 1400
      subnets:
      - cidr: 10.60.0.0/16
        hostSubnet: 24
`,
	},
}
