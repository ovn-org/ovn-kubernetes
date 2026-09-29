// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package cudn

import "github.com/ovn-kubernetes/ovn-kubernetes/test/crd-integration/testscenario"

var Layer3InvalidSubnets = []testscenario.ValidateCRScenario{
	{
		Description: "modifying Secondary network's subnets is not allowed",
		ExpectedErr: `Subnets is immutable for Secondary role`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: secondary-modify-subnet-ipv4
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer3
    layer3:
      role: Secondary
      subnets:
      - cidr: 10.1.0.0/16
        hostSubnet: 24
---
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: secondary-modify-subnet-ipv4
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer3
    layer3:
      role: Secondary
      subnets:
      - cidr: 10.1.0.0/16
        hostSubnet: 24
      - cidr: 10.2.0.0/16
        hostSubnet: 24
`,
	},
	{
		Description: "Secondary network with more than 2 subnets is not allowed",
		ExpectedErr: `Secondary networks may define at most 2 subnets`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: secondary-more-than-two-subnets
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer3
    layer3:
      role: Secondary
      subnets:
      - cidr: 10.1.0.0/16
        hostSubnet: 24
      - cidr: 2001:db8:1::/48
        hostSubnet: 64
      - cidr: 10.2.0.0/16
        hostSubnet: 24
`,
	},
	{
		Description: "Secondary network with 2 same-family subnets is not allowed",
		ExpectedErr: `When 2 CIDRs are set for Secondary networks, they must be from different IP families`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: secondary-same-family-subnets
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer3
    layer3:
      role: Secondary
      subnets:
      - cidr: 10.1.0.0/16
        hostSubnet: 24
      - cidr: 10.2.0.0/16
        hostSubnet: 24
`,
	},
	{
		Description: "IPv4: remove subnet is not allowed",
		ExpectedErr: `Removing existing subnets is not allowed`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: primary-remove-subnet-ipv4
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
---
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: primary-remove-subnet-ipv4
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer3
    layer3:
      role: Primary
      subnets:
      - cidr: 10.1.0.0/16
        hostSubnet: 24
`,
	},
	{
		Description: "IPv4: overlap subnets are not allowed",
		ExpectedErr: `Subnets must not overlap or contain each other`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: primary-overlap-subnets-ipv4
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer3
    layer3:
      role: Primary
      subnets:
      - cidr: 10.1.0.0/16
        hostSubnet: 24
      - cidr: 10.1.128.0/17
        hostSubnet: 24
`,
	},
	{
		Description: "IPv4: same subnets are not allowed",
		ExpectedErr: `Subnets with same CIDR are not allowed`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: primary-same-subnets-ipv4
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer3
    layer3:
      role: Primary
      subnets:
      - cidr: 10.1.0.0/16
        hostSubnet: 24
      - cidr: 10.1.0.0/16
        hostSubnet: 24
`,
	},
	{
		Description: "IPv4: mutate hostSubnet is not allowed",
		ExpectedErr: `hostSubnet is immutable`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: primary-mutate-host-subnet-ipv4
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer3
    layer3:
      role: Primary
      subnets:
      - cidr: 10.1.0.0/16
        hostSubnet: 24
---
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: primary-mutate-host-subnet-ipv4
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer3
    layer3:
      role: Primary
      subnets:
      - cidr: 10.1.0.0/16
        hostSubnet: 18
`,
	},
	{
		Description: "IPv4: subnets with different hostSubnet is not allowed",
		ExpectedErr: `Subnets from the same IP family must use the same hostSubnet value`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: primary-different-host-subnet-ipv4
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
        hostSubnet: 27
`,
	},
	{
		Description: "IPv6: remove subnet is not allowed",
		ExpectedErr: `Removing existing subnets is not allowed`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: primary-remove-subnet-ipv6
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
---
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: primary-remove-subnet-ipv6
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer3
    layer3:
      role: Primary
      subnets:
      - cidr: 2001:db8:1::/48
        hostSubnet: 64
`,
	},
	{
		Description: "IPv6: overlap subnets are not allowed",
		ExpectedErr: `Subnets must not overlap or contain each other`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: primary-overlap-subnets-ipv6
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer3
    layer3:
      role: Primary
      subnets:
      - cidr: 2001:db8:1::/48
        hostSubnet: 64
      - cidr: 2001:db8:1:8000::/49
        hostSubnet: 64
`,
	},
	{
		Description: "IPv6: same subnets are not allowed",
		ExpectedErr: `Subnets with same CIDR are not allowed`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: primary-same-subnets-ipv6
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer3
    layer3:
      role: Primary
      subnets:
      - cidr: 2001:db8:1::/48
        hostSubnet: 64
      - cidr: 2001:db8:1::/48
        hostSubnet: 64
`,
	},
	{
		Description: "IPv6: mutate hostSubnet is not allowed",
		ExpectedErr: `hostSubnet is immutable`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: primary-mutate-host-subnet-ipv6
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer3
    layer3:
      role: Primary
      subnets:
      - cidr: 2001:db8:1::/48
        hostSubnet: 64
---
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: primary-mutate-host-subnet-ipv6
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer3
    layer3:
      role: Primary
      subnets:
      - cidr: 2001:db8:1::/48
        hostSubnet: 60
`,
	},
	{
		Description: "IPv6: subnets with different hostSubnet is not allowed",
		ExpectedErr: `Subnets from the same IP family must use the same hostSubnet value`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: primary-different-host-subnet-ipv6
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
        hostSubnet: 60
`,
	},
}
