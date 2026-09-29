// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package cudn

import "github.com/ovn-kubernetes/ovn-kubernetes/test/crd-integration/testscenario"

var Layer2CUDNInvalid = []testscenario.ValidateCRScenario{
	{
		Description: "defaultGatewayIPs is not allowed for Secondary network",
		ExpectedErr: `defaultGatewayIPs is only supported for Primary network`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: default-gateway-secondary-fail
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer2
    layer2:
      role: Secondary
      subnets: ["192.168.1.0/24"]
      defaultGatewayIPs: ["192.168.1.1"]
`,
	},
	{
		Description: "defaultGatewayIPs must belong to subnets",
		ExpectedErr: `defaultGatewayIPs must belong to one of the subnets specified in the subnets field`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: default-gateway-outside-subnet-fail
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer2
    layer2:
      role: Primary
      subnets: ["192.168.1.0/24"]
      defaultGatewayIPs: ["10.0.0.1"]
`,
	},
	{
		Description: "defaultGatewayIPs must belong to infrastructureSubnets when specified",
		ExpectedErr: `defaultGatewayIPs have to belong to infrastructureSubnets`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: default-gateway-outside-infra-fail
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer2
    layer2:
      role: Primary
      subnets: ["192.168.1.0/24"]
      infrastructureSubnets: ["192.168.1.0/28"]
      defaultGatewayIPs: ["192.168.1.20"]
`,
	},
	{
		Description: "dual-stack defaultGatewayIPs one IP outside subnet",
		ExpectedErr: `defaultGatewayIPs must belong to one of the subnets specified in the subnets field`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: default-gateway-dual-stack-fail
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer2
    layer2:
      role: Primary
      subnets: ["192.168.1.0/24", "2001:db8::/64"]
      defaultGatewayIPs: ["10.0.0.1", "2001:db8::1"]
`,
	},

	{
		Description: "reservedSubnets must be unset when subnets is unset",
		ExpectedErr: `reservedSubnets must be unset when subnets is unset`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: reserved-subnets-no-subnets-fail
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer2
    layer2:
      role: Secondary
      reservedSubnets: ["192.168.1.0/28"]
      ipam:
        mode: Disabled
`,
	},
	{
		Description: "infrastructureSubnets must be unset when subnets is unset",
		ExpectedErr: `infrastructureSubnets must be unset when subnets is unset`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: infra-subnets-no-subnets-fail
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer2
    layer2:
      role: Primary
      infrastructureSubnets: ["192.168.1.0/28"]
      ipam:
        mode: Disabled
`,
	},
	{
		Description: "infrastructureSubnets and reservedSubnets must not overlap",
		ExpectedErr: `infrastructureSubnets and reservedSubnets must not overlap`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: infra-reserved-overlap-fail
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer2
    layer2:
      role: Primary
      subnets: ["192.168.1.0/24"]
      infrastructureSubnets: ["192.168.1.0/28"]
      reservedSubnets: ["192.168.1.8/29"]
`,
	},
	{
		Description: "dual-stack infrastructureSubnets and reservedSubnets overlap",
		ExpectedErr: `infrastructureSubnets and reservedSubnets must not overlap`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: dual-stack-overlap-fail
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer2
    layer2:
      role: Primary
      subnets: ["192.168.1.0/24", "2001:db8::/64"]
      infrastructureSubnets: ["192.168.1.0/28", "2001:db8::/80"]
      reservedSubnets: ["192.168.1.8/29", "2001:db8::/80"]
`,
	},
	{
		Description: "reservedSubnets must be subnetworks of subnets",
		ExpectedErr: `reservedSubnets must be subnetworks of the networks specified in the subnets field`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: reserved-subnets-outside-fail
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer2
    layer2:
      role: Secondary
      subnets: ["192.168.1.0/24"]
      reservedSubnets: ["10.0.0.0/28"]
`,
	},
	{
		Description: "infrastructureSubnets must be subnetworks of subnets",
		ExpectedErr: `infrastructureSubnets must be subnetworks of the networks specified in the subnets field`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: infra-subnets-outside-fail
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer2
    layer2:
      role: Primary
      subnets: ["192.168.1.0/24"]
      infrastructureSubnets: ["10.0.0.0/28"]
`,
	},
	{
		Description: "IPv6 reservedSubnet outside main subnet",
		ExpectedErr: `reservedSubnets must be subnetworks of the networks specified in the subnets field`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: ipv6-reserved-subnet-fail
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer2
    layer2:
      role: Secondary
      subnets: ["2001:db8::/64"]
      reservedSubnets: ["2001:db9::/80"]
`,
	},
	{
		Description: "IPv6 defaultGatewayIP outside subnet",
		ExpectedErr: `defaultGatewayIPs must belong to one of the subnets specified in the subnets field`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: ipv6-default-gateway-fail
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer2
    layer2:
      role: Primary
      subnets: ["2001:db8::/64"]
      defaultGatewayIPs: ["2001:db9::1"]
`,
	},
	{
		Description: "mac-security mode Disabled is not allowed for Primary networks",
		ExpectedErr: `macSecurity.mode Disabled is only supported for Secondary networks`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: mac-security-disabled-layer2-primary-fail
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer2
    layer2:
      role: Primary
      ipam: { mode: Disabled }
      macSecurity: { mode: Disabled }
`,
	},
	{
		Description: "mac-security disabled requires ipam disabled",
		ExpectedErr: `macSecurity.mode Disabled requires ipam.mode to be Disabled`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: mac-security-layer2-ipam-enabled-fail
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer2
    layer2:
      role: Secondary
      subnets: ["192.168.1.0/24"]
      macSecurity: { mode: Disabled }
`,
	},
}

var Layer2CUDNUpdatesRejected = []testscenario.UpdateCRScenario{
	{
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "enable macSecurity after creation is rejected",
			Name:        "mac-security-layer2-update-immutable-disable-to-enable",
			ExpectedErr: "Layer2 is immutable",
			Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: mac-security-layer2-update-immutable-disable-to-enable
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer2
    layer2:
      role: Secondary
      ipam: { mode: Disabled } 
      macSecurity: { mode: Disabled }
`,
		},
		InitialManifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: mac-security-layer2-update-immutable-disable-to-enable
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer2
    layer2:
      role: Secondary
      ipam: { mode: Disabled }
`,
	},
	{
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "disable macSecurity after creation is rejected",
			Name:        "mac-security-layer2-update-immutable-enable-to-disable",
			ExpectedErr: "Layer2 is immutable",
			Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: mac-security-layer2-update-immutable-enable-to-disable
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer2
    layer2:
      role: Secondary
      ipam: { mode: Disabled }
`,
		},
		InitialManifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: mac-security-layer2-update-immutable-enable-to-disable
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer2
    layer2:
      role: Secondary
      ipam: { mode: Disabled }
      macSecurity: { mode: Disabled }
`,
	},
}

var Layer2UDNInvalid = []testscenario.ValidateCRScenario{
	{
		Description: "defaultGatewayIPs is not allowed for Secondary network",
		ExpectedErr: `defaultGatewayIPs is only supported for Primary network`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: UserDefinedNetwork
metadata:
  namespace: default
  name: default-gateway-secondary-fail
spec:
  topology: Layer2
  layer2:
    role: Secondary
    subnets: ["192.168.1.0/24"]
    defaultGatewayIPs: ["192.168.1.1"]
`,
	},
	{
		Description: "defaultGatewayIPs must belong to subnets",
		ExpectedErr: `defaultGatewayIPs must belong to one of the subnets specified in the subnets field`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: UserDefinedNetwork
metadata:
  namespace: default
  name: default-gateway-outside-subnet-fail
spec:
  topology: Layer2
  layer2:
    role: Primary
    subnets: ["192.168.1.0/24"]
    defaultGatewayIPs: ["10.0.0.1"]
`,
	},
	{
		Description: "defaultGatewayIPs must belong to infrastructureSubnets when specified",
		ExpectedErr: `defaultGatewayIPs have to belong to infrastructureSubnets`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: UserDefinedNetwork
metadata:
  namespace: default
  name: default-gateway-outside-infra-fail
spec:
  topology: Layer2
  layer2:
    role: Primary
    subnets: ["192.168.1.0/24"]
    infrastructureSubnets: ["192.168.1.0/28"]
    defaultGatewayIPs: ["192.168.1.20"]
`,
	},
	{
		Description: "reservedSubnets must be unset when subnets is unset",
		ExpectedErr: `reservedSubnets must be unset when subnets is unset`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: UserDefinedNetwork
metadata:
  namespace: default
  name: reserved-subnets-no-subnets-fail
spec:
  topology: Layer2
  layer2:
    role: Secondary
    reservedSubnets: ["192.168.1.0/28"]
    ipam:
      mode: Disabled
`,
	},
	{
		Description: "infrastructureSubnets must be unset when subnets is unset",
		ExpectedErr: `infrastructureSubnets must be unset when subnets is unset`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: UserDefinedNetwork
metadata:
  namespace: default
  name: infra-subnets-no-subnets-fail
spec:
  topology: Layer2
  layer2:
    role: Primary
    infrastructureSubnets: ["192.168.1.0/28"]
    ipam:
      mode: Disabled
`,
	},
	{
		Description: "mac-security mode Disabled is not supported for Primary networks",
		ExpectedErr: `macSecurity.mode Disabled is only supported for Secondary networks`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: UserDefinedNetwork
metadata:
  namespace: default
  name: mac-security-disabled-layer2-primary-fail
spec:
  topology: Layer2
  layer2:
    role: Primary
    ipam: { mode: Disabled }
    macSecurity: { mode: Disabled }
`,
	},
	{
		Description: "mac-security disabled requires ipam disabled",
		ExpectedErr: `macSecurity.mode Disabled requires ipam.mode to be Disabled`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: UserDefinedNetwork
metadata:
  namespace: default
  name: mac-security-layer2-ipam-enabled-fail
spec:
  topology: Layer2
  layer2:
    role: Secondary
    subnets: ["192.168.1.0/24"]
    macSecurity: { mode: Disabled }
`,
	},
}

var Layer2UDNUpdatesRejected = []testscenario.UpdateCRScenario{
	{
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "enable macSecurity after creation is rejected",
			Name:        "mac-security-layer2-update-immutable-disable-to-enable",
			ExpectedErr: "Layer2 is immutable",
			Manifest: `
apiVersion: k8s.ovn.org/v1
kind: UserDefinedNetwork
metadata:
  namespace: default
  name: mac-security-layer2-update-immutable-disable-to-enable
spec:
  topology: Layer2
  layer2:
    role: Secondary
    ipam: { mode: Disabled } 
    macSecurity: { mode: Disabled }
`,
		},
		InitialManifest: `
apiVersion: k8s.ovn.org/v1
kind: UserDefinedNetwork
metadata:
  namespace: default
  name: mac-security-layer2-update-immutable-disable-to-enable
spec:
  topology: Layer2
  layer2:
    role: Secondary
    ipam: { mode: Disabled }
`,
	},
	{
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "disable macSecurity after creation is rejected",
			Name:        "mac-security-layer2-update-immutable-enable-to-disable",
			ExpectedErr: "Layer2 is immutable",
			Manifest: `
apiVersion: k8s.ovn.org/v1
kind: UserDefinedNetwork
metadata:
  namespace: default
  name: mac-security-layer2-update-immutable-enable-to-disable
spec:
  topology: Layer2
  layer2:
    role: Secondary
    ipam: { mode: Disabled }
`,
		},
		InitialManifest: `
apiVersion: k8s.ovn.org/v1
kind: UserDefinedNetwork
metadata:
  namespace: default
  name: mac-security-layer2-update-immutable-enable-to-disable
spec:
  topology: Layer2
  layer2:
    role: Secondary
    ipam: { mode: Disabled }
    macSecurity: { mode: Disabled }
`,
	},
}
