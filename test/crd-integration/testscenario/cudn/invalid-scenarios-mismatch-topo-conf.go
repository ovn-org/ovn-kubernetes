// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package cudn

import "github.com/ovn-kubernetes/ovn-kubernetes/test/crd-integration/testscenario"

var MismatchTopologyConfig = []testscenario.ValidateCRScenario{
	{
		Description: "topology is localnet but topology config is layer2",
		ExpectedErr: `spec.localnet is required when topology is Localnet and forbidden otherwise`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: topology-localnet-mismatch-topology-config-layer2-fail
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Localnet
    layer2:
`,
	},
	{
		Description: "topology is localnet but topology config is layer3",
		ExpectedErr: `spec.localnet is required when topology is Localnet and forbidden otherwise`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: topology-localnet-mismatch-topology-config-layer3-fail
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Localnet
    layer3:
`,
	},
	{
		Description: "topology is layer2 but topology config is localnet",
		ExpectedErr: `spec.layer2 is required when topology is Layer2 and forbidden otherwise`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: topology-layer2-mismatch-topology-config-localnet-fail
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer2
    localnet:
`,
	},
	{
		Description: "topology is layer2 but topology config is layer3",
		ExpectedErr: `spec.layer2 is required when topology is Layer2 and forbidden otherwise`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: topology-layer2-mismatch-topology-config-layer3-fail
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer2
    layer3:
`,
	},
	{
		Description: "topology is layer3 but topology config is localnet",
		ExpectedErr: `spec.layer3 is required when topology is Layer3 and forbidden otherwise`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: topology-layer3-mismatch-topology-config-localnet-fail
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer3
    localnet:
`,
	},
	{
		Description: "topology is layer3 but topology config is layer2",
		ExpectedErr: `spec.layer3 is required when topology is Layer3 and forbidden otherwise`,
		Manifest: `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: topology-layer3-mismatch-topology-config-layer2-fail
spec:
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: red}}
  network:
    topology: Layer3
    layer2:
`,
	},
}
