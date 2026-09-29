// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package cudn

import "github.com/ovn-kubernetes/ovn-kubernetes/test/crd-integration/testscenario"

var LocalnetInvalidMACSecurity = []testscenario.ValidateCRScenario{
	{
		Description: "mac-security disabled requires ipam disabled",
		ExpectedErr: `macSecurity.mode Disabled requires ipam.mode to be Disabled`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: mac-security-localnet-ipam-enabled-fail
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Localnet
    localnet:
      role: Secondary
      physicalNetworkName: test
      macSecurity: { mode: Disabled }
`,
	},
}

var LocalnetUpdatesRejected = []testscenario.UpdateCRScenario{
	{
		ValidateCRScenario: testscenario.ValidateCRScenario{
			Description: "localnet: enabling macSecurity after creation is rejected",
			Name:        "mac-security-localnet-update-immutable",
			ExpectedErr: "Localnet is immutable",
			Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: mac-security-localnet-update-immutable
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Localnet
    localnet:
      role: Secondary
      physicalNetworkName: test
      ipam:
        mode: Disabled
      macSecurity:
        mode: Disabled
`,
		},
		InitialManifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: mac-security-localnet-update-immutable
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Localnet
    localnet:
      role: Secondary
      physicalNetworkName: test
      ipam: { mode: Disabled }
`,
	},
}
