// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package cudn

import "github.com/ovn-kubernetes/ovn-kubernetes/test/crd-integration/testscenario"

var EVPNCUDNInvalid = []testscenario.ValidateCRScenario{
	{
		Description: "EVPN transport requires evpn configuration field",
		ExpectedErr: `spec.evpn field is required when transport is 'EVPN'`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: evpn-no-config
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer2
    layer2:
      role: Primary
      subnets: ["10.20.100.0/24"]
    transport: EVPN
`,
	},
	{
		Description: "evpn configuration field is forbidden when transport is not 'EVPN'",
		ExpectedErr: `spec.evpn field is forbidden when transport is not 'EVPN'`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: evpn-config-no-transport
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer2
    layer2:
      role: Primary
      subnets: ["10.20.100.0/24"]
    evpn:
      vtep: evpn-vtep
      macVRF:
        vni: 100
`,
	},
	{
		Description: "EVPN is not supported for Secondary networks",
		ExpectedErr: `transport 'EVPN' is only supported for Layer2 or Layer3 primary networks`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: evpn-secondary
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer2
    layer2:
      role: Secondary
      subnets: ["10.20.100.0/24"]
    transport: EVPN
    evpn:
      vtep: evpn-vtep
      macVRF:
        vni: 100
`,
	},
	{
		Description: "EVPN is not supported for Localnet topology",
		ExpectedErr: `transport 'EVPN' is only supported for Layer2 or Layer3 primary networks`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: evpn-localnet
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Localnet
    localnet:
      role: Secondary
      physicalNetworkName: physnet1
    transport: EVPN
    evpn:
      vtep: evpn-vtep
      macVRF:
        vni: 100
`,
	},
	{
		Description: "Layer2 EVPN requires macVRF",
		ExpectedErr: `spec.evpn.macVRF field is required for Layer2 topology when transport is 'EVPN'`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: l2-evpn-no-macvrf
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer2
    layer2:
      role: Primary
      subnets: ["10.20.100.0/24"]
    transport: EVPN
    evpn:
      vtep: evpn-vtep
      ipVRF:
        vni: 100
`,
	},
	{
		Description: "Layer3 EVPN requires ipVRF",
		ExpectedErr: `spec.evpn.ipVRF field is required for Layer3 topology when transport is 'EVPN'`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: l3-evpn-no-ipvrf
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer3
    layer3:
      role: Primary
      subnets:
      - cidr: "10.20.100.0/16"
        hostSubnet: 24
    transport: EVPN
    evpn:
      vtep: evpn-vtep
      macVRF:
        vni: 100
`,
	},
	{
		Description: "Layer3 EVPN forbids macVRF",
		ExpectedErr: `spec.evpn.macVRF field is forbidden for Layer3 topology when transport is 'EVPN'`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: l3-evpn-with-macvrf
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer3
    layer3:
      role: Primary
      subnets:
      - cidr: "10.20.100.0/16"
        hostSubnet: 24
    transport: EVPN
    evpn:
      vtep: evpn-vtep
      macVRF:
        vni: 100
      ipVRF:
        vni: 101
`,
	},
	{
		Description: "evpn requires at least macVRF or ipVRF",
		ExpectedErr: `at least one of macVRF or ipVRF must be specified`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: evpn-no-vrf
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer2
    layer2:
      role: Primary
      subnets: ["10.20.100.0/24"]
    transport: EVPN
    evpn:
      vtep: evpn-vtep
`,
	},
	{
		Description: "VNI must be at least 1",
		ExpectedErr: `spec.network.evpn.macVRF.vni: Invalid value`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: evpn-vni-zero
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer2
    layer2:
      role: Primary
      subnets: ["10.20.100.0/24"]
    transport: EVPN
    evpn:
      vtep: evpn-vtep
      macVRF:
        vni: 0
`,
	},
	{
		Description: "VNI must not exceed 16777215",
		ExpectedErr: `spec.network.evpn.macVRF.vni: Invalid value`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: evpn-vni-too-large
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer2
    layer2:
      role: Primary
      subnets: ["10.20.100.0/24"]
    transport: EVPN
    evpn:
      vtep: evpn-vtep
      macVRF:
        vni: 16777216
`,
	},
	{
		Description: "routeTarget must be in valid format",
		ExpectedErr: `RT must contain exactly one colon`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: evpn-invalid-rt-format
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer2
    layer2:
      role: Primary
      subnets: ["10.20.100.0/24"]
    transport: EVPN
    evpn:
      vtep: evpn-vtep
      macVRF:
        vni: 100
        routeTarget: "invalid"
`,
	},
	{
		Description: "routeTarget 4-byte AS requires 2-byte local admin (6-byte constraint)",
		ExpectedErr: `RT with 4-byte ASN global administrator must have format GHJK:MN where GHJK <= 4294967295 and MN <= 65535`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: evpn-invalid-rt-4byte-as
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer2
    layer2:
      role: Primary
      subnets: ["10.20.100.0/24"]
    transport: EVPN
    evpn:
      vtep: evpn-vtep
      macVRF:
        vni: 100
        routeTarget: "4200000000:70000"
`,
	},
	{
		Description: "routeTarget IPv4 format requires valid IPv4 address",
		ExpectedErr: `RT global administrator must be either '*', an IPv4 address, or a number`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: evpn-invalid-rt-ipv4
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer2
    layer2:
      role: Primary
      subnets: ["10.20.100.0/24"]
    transport: EVPN
    evpn:
      vtep: evpn-vtep
      macVRF:
        vni: 100
        routeTarget: "999.999.999.999:100"
`,
	},
	{
		Description: "routeTarget IPv4 format requires 2-byte local admin",
		ExpectedErr: `RT with IPv4 global administrator must have format A.B.C.D:MN where MN <= 65535`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: evpn-invalid-rt-ipv4-local
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer2
    layer2:
      role: Primary
      subnets: ["10.20.100.0/24"]
    transport: EVPN
    evpn:
      vtep: evpn-vtep
      macVRF:
        vni: 100
        routeTarget: "192.168.1.1:70000"
`,
	},
	{
		Description: "routeTarget format must have exactly one colon",
		ExpectedErr: `RT must contain exactly one colon`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: evpn-invalid-rt-multiple-colons
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer2
    layer2:
      role: Primary
      subnets: ["10.20.100.0/24"]
    transport: EVPN
    evpn:
      vtep: evpn-vtep
      macVRF:
        vni: 100
        routeTarget: "65000:100:200"
`,
	},
	{
		Description: "routeTarget format must include colon separator",
		ExpectedErr: `RT must contain exactly one colon`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: evpn-invalid-rt-no-colon
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer2
    layer2:
      role: Primary
      subnets: ["10.20.100.0/24"]
    transport: EVPN
    evpn:
      vtep: evpn-vtep
      macVRF:
        vni: 100
        routeTarget: "65000"
`,
	},
	{
		Description: "VTEP name is required in evpn",
		ExpectedErr: `spec.network.evpn.vtep`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: evpn-no-vtep
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer2
    layer2:
      role: Primary
      subnets: ["10.20.100.0/24"]
    transport: EVPN
    evpn:
      macVRF:
        vni: 100
`,
	},
	{
		Description: "VTEP name cannot be empty",
		ExpectedErr: `spec.network.evpn.vtep`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: evpn-empty-vtep
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer2
    layer2:
      role: Primary
      subnets: ["10.20.100.0/24"]
    transport: EVPN
    evpn:
      vtep: ""
      macVRF:
        vni: 100
`,
	},
	{
		Description: "routeTarget local administrator must be a number",
		ExpectedErr: `RT local administrator must be a number`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: evpn-invalid-rt-local-nan
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer2
    layer2:
      role: Primary
      subnets: ["10.20.100.0/24"]
    transport: EVPN
    evpn:
      vtep: evpn-vtep
      macVRF:
        vni: 100
        routeTarget: "65000:abc"
`,
	},
	{
		Description: "routeTarget wildcard format local admin must not exceed 4294967295",
		ExpectedErr: `RT with wildcard global administrator must have format *:OPQR where OPQR <= 4294967295`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: evpn-invalid-rt-wildcard-overflow
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer2
    layer2:
      role: Primary
      subnets: ["10.20.100.0/24"]
    transport: EVPN
    evpn:
      vtep: evpn-vtep
      macVRF:
        vni: 100
        routeTarget: "*:5000000000"
`,
	},
	{
		Description: "macVRF and ipVRF must use different VNIs",
		ExpectedErr: `macVRF and ipVRF must use different VNIs`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: evpn-same-vni
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer2
    layer2:
      role: Primary
      subnets: ["10.20.100.0/24"]
    transport: EVPN
    evpn:
      vtep: evpn-vtep
      macVRF:
        vni: 100
      ipVRF:
        vni: 100
`,
	},
	{
		Description: "routeTarget exceeds maximum length of 21 characters",
		ExpectedErr: `Too long`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: evpn-invalid-rt-too-long
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer2
    layer2:
      role: Primary
      subnets: ["10.20.100.0/24"]
    transport: EVPN
    evpn:
      vtep: evpn-vtep
      macVRF:
        vni: 100
        routeTarget: "255.255.255.255:655350"
`,
	},
}
