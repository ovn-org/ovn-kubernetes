// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package cudn

import "github.com/ovn-kubernetes/ovn-kubernetes/test/crd-integration/testscenario"

var Layer3ValidSubnets = []testscenario.ValidateCRScenario{
	{
		Description: "IPv4: valid Primary network with multiple subnets",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: primary-with-multiple-subnets-ipv4
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer3
    layer3:
      role: Primary
      subnets:
      - cidr: 10.1.0.0/16
        hostSubnet: 24
      - cidr: 10.2.0.0/16
        hostSubnet: 24
`,
	},
	{
		Description: "IPv4: valid Primary network with multiple subnets - add subnet",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: primary-with-multiple-subnets-ipv4
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer3
    layer3:
      role: Primary
      subnets:
      - cidr: 10.1.0.0/16
        hostSubnet: 24
      - cidr: 10.2.0.0/16
        hostSubnet: 24
      - cidr: 10.3.0.0/16
        hostSubnet: 24
`,
	},
	{
		Description: "IPv6: valid Primary network with multiple subnets",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: primary-with-multiple-subnets-ipv6
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer3
    layer3:
      role: Primary
      subnets:
      - cidr: 2001:db8:1::/48
        hostSubnet: 64
      - cidr: 2001:db8:2::/48
        hostSubnet: 64
`,
	},
	{
		Description: "IPv6: valid Primary network with multiple subnets - add subnet",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: primary-with-multiple-subnets-ipv6
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer3
    layer3:
      role: Primary
      subnets:
      - cidr: 2001:db8:1::/48
        hostSubnet: 64
      - cidr: 2001:db8:2::/48
        hostSubnet: 64
      - cidr: 2001:db8:3::/48
        hostSubnet: 64
`,
	},
	{
		Description: "dual-stack: valid Primary network with multiple subnets",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: primary-with-multiple-subnets-dual-stack
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer3
    layer3:
      role: Primary
      subnets:
      - cidr: 10.1.0.0/16
        hostSubnet: 24
      - cidr: 10.2.0.0/16
        hostSubnet: 24
      - cidr: 2001:db8:1::/48
        hostSubnet: 64
      - cidr: 2001:db8:2::/48
        hostSubnet: 64
`,
	},
}
