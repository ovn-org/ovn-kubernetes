// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package cudn

import "github.com/ovn-kubernetes/ovn-kubernetes/test/crd-integration/testscenario"

var EVPNCUDNValid = []testscenario.ValidateCRScenario{
	{
		Description: "valid Layer2 Primary EVPN with macVRF",
		Name:        "l2-evpn-primary",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: l2-evpn-primary
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
`,
	},
	{
		Description: "valid Layer2 Primary EVPN with macVRF and routeTarget",
		Name:        "l2-evpn-with-rt",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: l2-evpn-with-rt
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
        routeTarget: "65000:100"
`,
	},
	{
		Description: "valid Layer2 Primary EVPN with both macVRF and ipVRF",
		Name:        "l2-evpn-mac-ip-vrf",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: l2-evpn-mac-ip-vrf
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
        vni: 101
`,
	},
	{
		Description: "valid Layer3 Primary EVPN with ipVRF",
		Name:        "l3-evpn-primary",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: l3-evpn-primary
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
      ipVRF:
        vni: 200
`,
	},
	{
		Description: "valid Layer3 Primary EVPN with ipVRF and routeTarget",
		Name:        "l3-evpn-with-rt",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: l3-evpn-with-rt
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
      ipVRF:
        vni: 200
        routeTarget: "65000:200"
`,
	},
	{
		Description: "valid Layer2 Primary EVPN dual-stack",
		Name:        "l2-evpn-dual-stack",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: l2-evpn-dual-stack
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer2
    layer2:
      role: Primary
      subnets: ["10.20.100.0/24", "2001:db8::/64"]
    transport: EVPN
    evpn:
      vtep: evpn-vtep
      macVRF:
        vni: 300
`,
	},
	{
		Description: "valid EVPN with max VNI value",
		Name:        "evpn-max-vni",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: evpn-max-vni
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
        vni: 16777215
`,
	},
	{
		Description: "valid EVPN with 4-byte ASN in routeTarget (2-byte local admin)",
		Name:        "evpn-4byte-asn",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: evpn-4byte-asn
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
        routeTarget: "4200000000:100"
`,
	},
	{
		Description: "valid EVPN with 2-byte ASN and 4-byte local admin",
		Name:        "evpn-2byte-asn-4byte-local",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: evpn-2byte-asn-4byte-local
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
        routeTarget: "65000:16777215"
`,
	},
	{
		Description: "valid EVPN with IPv4 address format routeTarget",
		Name:        "evpn-ipv4-rt",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: evpn-ipv4-rt
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
        routeTarget: "192.168.1.1:100"
`,
	},
	{
		Description: "valid EVPN with wildcard AS routeTarget",
		Name:        "evpn-wildcard-rt",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: evpn-wildcard-rt
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
        routeTarget: "*:100"
`,
	},
	{
		Description: "valid EVPN with wildcard AS and large local admin",
		Name:        "evpn-wildcard-large-local",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: evpn-wildcard-large-local
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
        routeTarget: "*:16777215"
`,
	},
	{
		Description: "valid EVPN with AS=0 (FRR allows)",
		Name:        "evpn-as-zero",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: evpn-as-zero
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
        routeTarget: "0:100"
`,
	},
	{
		Description: "valid EVPN with local admin=0 (FRR allows)",
		Name:        "evpn-local-zero",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: evpn-local-zero
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
        routeTarget: "65000:0"
`,
	},
	{
		Description: "valid EVPN with wildcard and local admin=0 (FRR allows)",
		Name:        "evpn-wildcard-zero",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: evpn-wildcard-zero
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
        routeTarget: "*:0"
`,
	},
	{
		Description: "valid EVPN with max 2-byte AS boundary (65535)",
		Name:        "evpn-2byte-as-max",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: evpn-2byte-as-max
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
        routeTarget: "65535:100"
`,
	},
	{
		Description: "valid EVPN with min 4-byte AS boundary (65536)",
		Name:        "evpn-4byte-as-min",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: evpn-4byte-as-min
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
        routeTarget: "65536:100"
`,
	},
	{
		Description: "valid EVPN with max local admin for 2-byte AS (4294967295)",
		Name:        "evpn-2byte-as-max-local",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: evpn-2byte-as-max-local
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
        routeTarget: "65000:4294967295"
`,
	},
	{
		Description: "valid EVPN with max length routeTarget (21 chars: 255.255.255.255:65535)",
		Name:        "evpn-max-length-rt",
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: evpn-max-length-rt
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
        routeTarget: "255.255.255.255:65535"
`,
	},
}
