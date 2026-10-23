// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package cudn

import "github.com/ovn-kubernetes/ovn-kubernetes/test/crd-integration/testscenario"

var LocalnetInvalidSubnets = []testscenario.ValidateCRScenario{
	{
		Description: "unset subnets, and ipam.mode is unset",
		ExpectedErr: `Subnets is required with ipam.mode is Enabled or unset, and forbidden otherwise`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: localnet-subnets-and-ipam-disabled-unset-fail
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Localnet
    localnet:
      role: Secondary
      physicalNetworkName: test
`,
	},
	{
		Description: "ipam.mode is Enabled but subnets is unset",
		ExpectedErr: `Subnets is required with ipam.mode is Enabled or unset, and forbidden otherwise`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: localnet-ipam-mode-disabled-ipam-lifecycle-persistent-fail
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Localnet
    localnet:
      role: Secondary
      physicalNetworkName: test
      ipam:
        mode: Enabled
`,
	},
	{
		Description: "subnets is set but ipam.mode is Disabled",
		ExpectedErr: `Subnets is required with ipam.mode is Enabled or unset, and forbidden otherwise`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: localnet-subnet-and-ipam-disabled-set-fail
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Localnet
    localnet:
      role: Secondary
      physicalNetworkName: "asd"
      subnets: [10.0.0.0/24]
      ipam: {mode: Disabled}
`,
	},
	{
		Description: "subnets is empty",
		ExpectedErr: `ClusterUserDefinedNetwork.k8s.ovn.org "localnet-empty-subnets-fail" is invalid: spec.network.localnet.subnets: Invalid value: 0: spec.network.localnet.subnets in body should have at least 1 items`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: localnet-empty-subnets-fail
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Localnet
    localnet:
      role: Secondary
      physicalNetworkName: test
      subnets: []
`,
	},
	{
		Description: "excludeSubnets is set, subnets is unset",
		ExpectedErr: `excludeSubnets must be unset when subnets is unset`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: localnet-subnets-unset-and-excludesubnets-set-fail
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Localnet
    localnet:
      role: Secondary
      physicalNetworkName: test
      excludeSubnets: [10.0.0.1/24]
`,
	},
	{
		Description: "excludeSubnets is set, ipam.mode is Disabled",
		ExpectedErr: `excludeSubnets must be unset when subnets is unset`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: localnet-ipam-disabled-and-excludesubnets-set-fail
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Localnet
    localnet:
      role: Secondary
      physicalNetworkName: test
      excludeSubnets: [10.0.0.1/24]
      ipam: {mode: Disabled}
`,
	},
	{
		Description: "subnets & excludeSubnets are set, but ipam.mode is Disabled",
		ExpectedErr: "Subnets is required with ipam.mode is Enabled or unset, and forbidden otherwise",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: localnet-ipam-disabled-subnetes-excludesubnetes-fail
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Localnet
    localnet:
      role: Secondary
      physicalNetworkName: "asd"
      subnets: [10.0.0.0/24]
      excludeSubnets: [10.0.0.1/32]
      ipam: {mode: Disabled}
`,
	},
	{
		Description: "invalid subnets - invalid IPv4 CIDR",
		ExpectedErr: `ClusterUserDefinedNetwork.k8s.ovn.org "localnet-subnets-invalid-ipv4-cidr-fail" is invalid: spec.network.localnet.subnets[0]: Invalid value: "300.0.0.0/24": CIDR is invalid`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: localnet-subnets-invalid-ipv4-cidr-fail
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Localnet
    localnet:
      role: Secondary
      physicalNetworkName: test
      subnets: [300.0.0.0/24]`,
	},
	{
		Description: "invalid subnets - invalid IPv6 CIDR",
		ExpectedErr: `ClusterUserDefinedNetwork.k8s.ovn.org "localnet-subnets-invalid-ipv6-cidr-fail" is invalid: spec.network.localnet.subnets[0]: Invalid value: "2014:100:200::0/300": CIDR is invalid`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: localnet-subnets-invalid-ipv6-cidr-fail
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Localnet
    localnet:
      role: Secondary
      physicalNetworkName: test
      subnets: [2014:100:200::0/300]`,
	},
	{
		Description: "invalid subnets - two IPv4 CIDRs",
		ExpectedErr: `ClusterUserDefinedNetwork.k8s.ovn.org "localnet-subnets-multiple-ipv4-cidrs-fail" is invalid: spec.network.localnet.subnets: Invalid value: When 2 CIDRs are set, they must be from different IP families`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: localnet-subnets-multiple-ipv4-cidrs-fail
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Localnet
    localnet:
      role: Secondary
      physicalNetworkName: test
      subnets: [10.10.0.0/24, 10.20.0.0/24]
`,
	},
	{
		Description: "invalid subnets - two IPv6 CIDRs",
		ExpectedErr: `ClusterUserDefinedNetwork.k8s.ovn.org "localnet-subnets-multiple-ipv6-cidrs-fail" is invalid: spec.network.localnet.subnets: Invalid value: When 2 CIDRs are set, they must be from different IP families`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: localnet-subnets-multiple-ipv6-cidrs-fail
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Localnet
    localnet:
      role: Secondary
      physicalNetworkName: test
      subnets: [2001:aaa::/64, 2001:bbb::/64]
`,
	},
	{
		Description: "invalid excludeSubnets - empty slice",
		ExpectedErr: `ClusterUserDefinedNetwork.k8s.ovn.org "localnet-excludesubnet-empty-slice-fail" is invalid: spec.network.localnet.excludeSubnets: Invalid value: 0: spec.network.localnet.excludeSubnets in body should have at least 1 items`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: localnet-excludesubnet-empty-slice-fail
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Localnet
    localnet:
      role: Secondary
      physicalNetworkName: test
      subnets: [10.0.0.0/24]
      excludeSubnets: []`,
	},
	{
		Description: "invalid excludeSubnets - invalid IPv4 CIDR",
		ExpectedErr: `ClusterUserDefinedNetwork.k8s.ovn.org "localnet-excludesubnet-invalid-ipv4-cidr-fail" is invalid: spec.network.localnet.excludeSubnets[0]: Invalid value: "10.0.0.0/300": CIDR is invalid`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: localnet-excludesubnet-invalid-ipv4-cidr-fail
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Localnet
    localnet:
      role: Secondary
      physicalNetworkName: test
      subnets: [10.0.0.0/24]
      excludeSubnets: [10.0.0.0/300]
`,
	},
	{
		Description: "invalid excludeSubnets - invalid IPv6 CIDR",
		ExpectedErr: `ClusterUserDefinedNetwork.k8s.ovn.org "localnet-excludesubnet-invalid-ipv6-cidr-fail" is invalid: spec.network.localnet.excludeSubnets[0]: Invalid value: "2014:100:200::0/300": CIDR is invalid`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: localnet-excludesubnet-invalid-ipv6-cidr-fail
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Localnet
    localnet:
      role: Secondary
      physicalNetworkName: test
      subnets: [10.0.0.0/24]
      excludeSubnets: [2014:100:200::0/300]
`,
	},
	{
		Description: "ipam.lifecycle is Persistent but ipam.mode is Disabled",
		ExpectedErr: `lifecycle Persistent is only supported when ipam.mode is Enabled`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: localnet-ipam-mode-disabled-ipam-lifecycle-persistent-fail
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Localnet
    localnet:
      role: Secondary
      physicalNetworkName: test
      ipam:
        mode: Disabled
        lifecycle: Persistent
`,
	},
}
