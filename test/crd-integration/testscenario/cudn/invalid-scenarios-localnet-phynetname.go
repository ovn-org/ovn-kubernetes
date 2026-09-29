// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package cudn

import "github.com/ovn-kubernetes/ovn-kubernetes/test/crd-integration/testscenario"

var LocalnetInvalidPhyNetName = []testscenario.ValidateCRScenario{
	{
		Description: "unset PhysicalNetworkName",
		ExpectedErr: `spec.network.localnet.physicalNetworkName: Required value`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: localnet-phynetname-unset-fail
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Localnet
    localnet:
      role: Secondary
      subnets: [10.0.0.0/24]`,
	},
	{
		Description: "invalid PhysicalNetworkName - empty string",
		ExpectedErr: `spec.network.localnet.physicalNetworkName in body should be at least 1 chars long`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: localnet-invalid-phynetname-empty-string-fail
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Localnet
    localnet:
      role: Secondary
      physicalNetworkName: ""
      subnets: [10.0.0.0/24]`,
	},
	{
		Description: "invalid PhysicalNetworkName - too long",
		ExpectedErr: `spec.network.localnet.physicalNetworkName: Too long: may not be more than 253 bytes`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: localnet-invalid-phynetname-exceed-253-chars-fail
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Localnet
    localnet:
      role: Secondary
      physicalNetworkName: "ycx7b6dhkhytzva3wrma0cu6mjhqpo2ty20cmpdg9ptvmt1mo9dfnrs56nr0bvg6z6zha5y208js6e2iwk6xb97sp2sojg48lu9d5vxbzzq40rwj1wchae3ju3dpj6qfsjbzjzmc0k489bloe49z4857kds43rqpeca3p5z2dfz562qu59qqb8qa3vo6pmwuaume581dqhlsz57yvbvgu5hmmmzremac7w7l4rmuirkk91767llw0vskanlc33"
      subnets: [10.0.0.0/24]`,
	},
	{
		Description: "invalid PhysicalNetworkName - contain `:` chars",
		ExpectedErr: "physicalNetworkName cannot contain `,` or `:` characters",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: localnet-invalid-phynetname-has-colon-fail
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Localnet
    localnet:
      role: Secondary
      physicalNetworkName: "t:est"
      subnets: [10.0.0.0/24]`,
	},
	{
		Description: "invalid PhysicalNetworkName - contain `,` chars",
		ExpectedErr: "physicalNetworkName cannot contain `,` or `:` characters",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: localnet-invalid-phynetname-has-comma-fail
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Localnet
    localnet:
      role: Secondary
      physicalNetworkName: "tes,t"
      subnets: [10.0.0.0/24]
`,
	},
	{
		Description: "invalid PhysicalNetworkName - start with `:` char",
		ExpectedErr: "physicalNetworkName cannot contain `,` or `:` characters",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: localnet-invalid-phynetname-start-with-colon-fail
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Localnet
    localnet:
      role: Secondary
      physicalNetworkName: ":test"
      subnets: [10.0.0.0/24]
`,
	},
	{
		Description: "invalid PhysicalNetworkName - start with `,` char",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: localnet-invalid-phynetname-start-with-comma-fail
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Localnet
    localnet:
      role: Secondary
      physicalNetworkName: ",test"
      subnets: [10.0.0.0/24]
`,
		ExpectedErr: "physicalNetworkName cannot contain `,` or `:` characters",
	},
	{
		Description: "invalid PhysicalNetworkName - ends with `:` char",
		ExpectedErr: "physicalNetworkName cannot contain `,` or `:` characters",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: localnet-invalid-phynetname-end-with-colon-fail
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Localnet
    localnet:
      role: Secondary
      physicalNetworkName: "test:"
      subnets: [10.0.0.0/24]`,
	},
	{
		Description: "invalid PhysicalNetworkName - ends with `,` char",
		ExpectedErr: "physicalNetworkName cannot contain `,` or `:` characters",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: localnet-invalid-phynetname-end-with-comma-fail
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Localnet
    localnet:
      role: Secondary
      physicalNetworkName: "test,"
      subnets: [10.0.0.0/24]`,
	},
}
