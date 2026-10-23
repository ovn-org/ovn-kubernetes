// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package cudn

import "github.com/ovn-kubernetes/ovn-kubernetes/test/crd-integration/testscenario"

var LocalnetInvalidVLAN = []testscenario.ValidateCRScenario{
	{
		Description: "invalid VLAN - invalid mode",
		ExpectedErr: `spec.network.localnet.vlan.mode: Unsupported value: "Disabled": supported values: "Access`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: localnet-vlan-invalid-mode-fail
spec:
  namespaceSelector: {matchLabels: { kubernetes.io/metadata.name: red}}
  network:
    topology: Localnet
    localnet:
      role: Secondary
      physicalNetworkName: test
      subnets: [192.168.0.0/16]
      vlan:
        mode: Disabled
`,
	},
	{
		Description: "invalid VLAN -mode is 'Access' but vlan access config is unset",
		ExpectedErr: `vlan access config is required when vlan mode is 'Access', and forbidden otherwise`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: localnet-vlan--no-access-config-fail
spec:
  namespaceSelector: {matchLabels: { kubernetes.io/metadata.name: red}}
  network:
    topology: Localnet
    localnet:
      role: Secondary
      physicalNetworkName: test
      subnets: [192.168.0.0/16]
      vlan:
        mode: Access
`,
	},
	{
		Description: "invalid VLAN - vlan access config is unset",
		ExpectedErr: `spec.network.localnet.vlan.access.id: Required value`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: localnet-vlan-access-config-unset-fail
spec:
  namespaceSelector: {matchLabels: { kubernetes.io/metadata.name: red}}
  network:
    topology: Localnet
    localnet:
      role: Secondary
      physicalNetworkName: test
      subnets: [192.168.0.0/16]
      vlan:
        mode: Access
        access: {} 
`,
	},
	{
		Description: "invalid VLAN - vlan access id is 0",
		ExpectedErr: `spec.network.localnet.vlan.access.id in body should be greater than or equal to 1`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: localnet-vlan-id-lower-then-2-fail
spec:
  namespaceSelector: {matchLabels: { kubernetes.io/metadata.name: red}}
  network:
    topology: Localnet
    localnet:
      role: Secondary
      physicalNetworkName: test
      subnets: [192.168.0.0/16]
      vlan:
        mode: Access
        access: {id: 0} 
`,
	},
	{
		Description: "invalid VLAN - vlan access id is 4095",
		ExpectedErr: `spec.network.localnet.vlan.access.id in body should be less than or equal to 4094`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: localnet-vlan-id-higher-then-4094-fail
spec:
  namespaceSelector: {matchLabels: { kubernetes.io/metadata.name: red}}
  network:
    topology: Localnet
    localnet:
      role: Secondary
      physicalNetworkName: test
      subnets: [192.168.0.0/16]
      vlan:
        mode: Access
        access: {id: 4095} 
`,
	},
}
