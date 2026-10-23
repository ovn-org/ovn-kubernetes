// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package cudn

import "github.com/ovn-kubernetes/ovn-kubernetes/test/crd-integration/testscenario"

var NoOverlayInvalid = []testscenario.ValidateCRScenario{
	{
		Description: "NoOverlay transport is only supported for Layer3 primary networks - Layer2 network",
		ExpectedErr: `transport 'NoOverlay' is only supported for Layer3 primary networks`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: no-overlay-layer2-fail
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer2
    layer2:
      role: Primary
      subnets:
      - 10.10.0.0/16
    transport: NoOverlay
    noOverlay:
      outboundSNAT: Enabled
      routing: Managed
`,
	},
	{
		Description: "NoOverlay transport is only supported for Layer3 primary networks - Layer3 secondary network",
		ExpectedErr: `transport 'NoOverlay' is only supported for Layer3 primary networks`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: no-overlay-layer3-secondary-fail
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer3
    layer3:
      role: Secondary
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
		Description: "NoOverlay transport is only supported for Layer3 primary networks - Localnet network",
		ExpectedErr: `transport 'NoOverlay' is only supported for Layer3 primary networks`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: no-overlay-localnet-fail
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Localnet
    localnet:
      role: Secondary
      physicalNetworkName: physnet1
      subnets:
      - 10.10.0.0/16
    transport: NoOverlay
    noOverlay:
      outboundSNAT: Enabled
      routing: Managed
`,
	},
	{
		Description: "noOverlay is required when transport is NoOverlay",
		ExpectedErr: `spec.noOverlay is required when type transport is 'NoOverlay'`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: no-overlay-missing-options-fail
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer3
    layer3:
      role: Primary
      subnets:
      - cidr: 10.10.0.0/16
        hostSubnet: 24
    transport: NoOverlay
`,
	},
	{
		Description: "noOverlay is forbidden when transport is not set",
		ExpectedErr: `spec.noOverlay is forbidden when transport type is not 'NoOverlay'`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: no-overlay-options-without-transport-fail
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer3
    layer3:
      role: Primary
      subnets:
      - cidr: 10.10.0.0/16
        hostSubnet: 24
    noOverlay:
      outboundSNAT: Enabled
      routing: Managed
`,
	},
}
