// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package cudn

import "github.com/ovn-kubernetes/ovn-kubernetes/test/crd-integration/testscenario"

var LocalnetValid = []testscenario.ValidateCRScenario{
	{
		Description: "should create localnet topology successfully - minimal",
		Name:        "localnet-minimal-success",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: localnet-minimal-success
spec:
  namespaceSelector:
    matchLabels:
      kubernetes.io/metadata.name: red
  network:
    topology: Localnet
    localnet:
      role: Secondary
      physicalNetworkName: test
      subnets: ["10.0.0.0/24"]
`,
	},
	{
		Description: "should create localnet topology successfully - ipam persistent",
		Name:        "localnet-ipam-lifecycle-persistent-success",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: localnet-ipam-lifecycle-persistent-success
spec:
  namespaceSelector:
    matchExpressions:
      - key: kubernetes.io/metadata.name
        operator: In
        values: ["red", "blue"]
  network:
    topology: Localnet
    localnet:
      role: Secondary
      physicalNetworkName: test
      ipam: {lifecycle: Persistent}
      subnets: ["192.168.0.0/16", "2001:dbb::/64"]
      excludeSubnets: ["192.168.0.1/32", "2001:dbb::1/128"]
      vlan: 
        mode: Access
        access: {id: 3}
      mtu: 9000
`,
	},
	{
		Description: "should create localnet topology successfully - ipam disabled",
		Name:        "localnet-ipam-disabled-success",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: localnet-ipam-disabled-success
spec:
  namespaceSelector:
    matchExpressions:
      - key: kubernetes.io/metadata.name
        operator: In
        values: ["red", "blue"]
  network:
    topology: Localnet
    localnet:
      role: Secondary
      physicalNetworkName: test
      ipam: {mode: Disabled}
      vlan: 
        mode: Access
        access: {id: 4094}
      mtu: 9000
`,
	},
	{
		Description: "valid disabled mac-security",
		Name:        "localnet-mac-security-disabled-success",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: localnet-mac-security-disabled-success
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Localnet
    localnet:
      role: Secondary
      physicalNetworkName: test
      ipam: {mode: Disabled}
      macSecurity: { mode: Disabled }
`,
	},
	{
		Description: "valid explicitly enabled mac-security",
		Name:        "localnet-mac-security-explicitly-enabled-success",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: localnet-mac-security-explicitly-enabled-success
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Localnet
    localnet:
      role: Secondary
      physicalNetworkName: test
      ipam: {mode: Disabled}
      macSecurity: { mode: Enabled }
`,
	},
}
