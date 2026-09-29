// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package cudn

import "github.com/ovn-kubernetes/ovn-kubernetes/test/crd-integration/testscenario"

var LocalnetInvalidMTU = []testscenario.ValidateCRScenario{
	{
		Description: "invalid MTU - higher than 65536",
		ExpectedErr: `spec.network.localnet.mtu in body should be less than or equal to 65536`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: localnet-mtu-exceed-max-ipam-disabled-fail
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Localnet
    localnet:
      role: Secondary
      physicalNetworkName: test
      mtu: 65537
      ipam: {mode: Disabled}
`,
	},
	{
		Description: "invalid MTU - lower than 576",
		ExpectedErr: `spec.network.localnet.mtu in body should be greater than or equal to 576`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: localnet-ipv4-mtu-below-min-fail
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Localnet
    localnet:
      role: Secondary
      physicalNetworkName: test
      subnets: [10.0.0.0/24]
      mtu: 575
`,
	},
	{
		Description: "invalid MTU - when IPv6 subnet is set, should be at least 1280",
		ExpectedErr: `MTU should be greater than or equal to 1280 when an IPv6 subnet is used`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: localnet-ipv6-mtu-below-min-fail
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Localnet
    localnet:
      role: Secondary
      physicalNetworkName: test
      subnets: [2001:dbb::/64]
      mtu: 1279
`,
	},
	{
		Description: "invalid MTU - when dualstack subnet is set, should be at least 1280",
		ExpectedErr: `MTU should be greater than or equal to 1280 when an IPv6 subnet is used`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: localnet-dualstack-mtu-below-min-fail
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Localnet
    localnet:
      role: Secondary
      physicalNetworkName: test
      subnets: [192.168.0.0/16, 2001:dbb::/64]
      mtu: 1279
`,
	},
}
