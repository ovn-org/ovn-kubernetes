// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package crdintegration

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"

	"github.com/ovn-kubernetes/ovn-kubernetes/test/crd-integration/testscenario"
	testscenariocudn "github.com/ovn-kubernetes/ovn-kubernetes/test/crd-integration/testscenario/cudn"
)

var _ = Describe("Network Segmentation: API validations", func() {
	DescribeTable("api-server should reject invalid CRs",
		func(scenarios []testscenario.ValidateCRScenario) {
			DeferCleanup(func() {
				cleanupValidateCRsTest(scenarios)
			})
			for _, s := range scenarios {
				By(s.Description)
				_, stderr, err := runKubectlInputWithFullOutput("", s.Manifest, "apply", "-f", "-")
				Expect(err).To(HaveOccurred(), "should fail to create invalid CR")
				Expect(stderr).To(ContainSubstring(s.ExpectedErr))
			}
		},
		Entry("ClusterUserDefinedNetwork, mismatch topology and config", testscenariocudn.MismatchTopologyConfig),
		Entry("ClusterUserDefinedNetwork, localnet, invalid role", testscenariocudn.LocalnetInvalidRole),
		Entry("ClusterUserDefinedNetwork, localnet, invalid physicalNetworkName", testscenariocudn.LocalnetInvalidPhyNetName),
		Entry("ClusterUserDefinedNetwork, localnet, invalid subnets", testscenariocudn.LocalnetInvalidSubnets),
		Entry("ClusterUserDefinedNetwork, localnet, invalid mtu", testscenariocudn.LocalnetInvalidMTU),
		Entry("ClusterUserDefinedNetwork, localnet, invalid vlan", testscenariocudn.LocalnetInvalidVLAN),
		Entry("ClusterUserDefinedNetwork, localnet, invalid macSecurity", feature.MACSecurity, testscenariocudn.LocalnetInvalidMACSecurity),
		Entry("ClusterUserDefinedNetwork, layer2", testscenariocudn.Layer2CUDNInvalid),
		Entry("ClusterUserDefinedNetwork, evpn", testscenariocudn.EVPNCUDNInvalid),
		Entry("UserDefinedNetwork, layer2", testscenariocudn.Layer2UDNInvalid),
		Entry("ClusterUserDefinedNetwork, no-overlay, invalid", testscenariocudn.NoOverlayInvalid),
		Entry("ClusterUserDefinedNetwork, layer3, multi-subnets", testscenariocudn.Layer3InvalidSubnets),
	)

	DescribeTable("api-server should accept valid CRs",
		func(scenarios []testscenario.ValidateCRScenario) {
			DeferCleanup(func() {
				cleanupValidateCRsTest(scenarios)
			})
			for _, s := range scenarios {
				By(s.Description)
				_, err := e2ekubectl.RunKubectlInput("", s.Manifest, "apply", "-f", "-")
				Expect(err).NotTo(HaveOccurred(), "should create valid CR successfully")
			}
		},
		Entry("ClusterUserDefinedNetwork, localnet", testscenariocudn.LocalnetValid),
		Entry("ClusterUserDefinedNetwork, layer2", testscenariocudn.Layer2CUDNValid),
		Entry("ClusterUserDefinedNetwork, evpn", testscenariocudn.EVPNCUDNValid),
		Entry("UserDefinedNetwork, layer2", testscenariocudn.Layer2UDNValid),
		Entry("ClusterUserDefinedNetwork, no-overlay, valid", testscenariocudn.NoOverlayValid),
		Entry("ClusterUserDefinedNetwork, layer3, multi-subnets", testscenariocudn.Layer3ValidSubnets),
	)

	DescribeTable("api-server should reject invalid updates",
		func(scenarios []testscenario.UpdateCRScenario) {
			DeferCleanup(func() {
				cleanupUpdateCRsTest(scenarios)
			})
			for _, s := range scenarios {
				By(s.Description)
				By("Creating initial CR")
				_, err := e2ekubectl.RunKubectlInput("", s.InitialManifest, "apply", "-f", "-")
				Expect(err).NotTo(HaveOccurred(), "should create initial CR successfully")

				By("Updating CR")
				_, stderr, err := runKubectlInputWithFullOutput("", s.Manifest, "apply", "-f", "-")
				Expect(err).To(HaveOccurred(), "should fail to update CR")
				Expect(stderr).To(ContainSubstring(s.ExpectedErr))
			}
		},
		Entry("ClusterUserDefinedNetwork, localnet, mac-security", feature.MACSecurity, testscenariocudn.LocalnetUpdatesRejected),
		Entry("ClusterUserDefinedNetwork, layer2, mac-security", feature.MACSecurity, testscenariocudn.Layer2CUDNUpdatesRejected),
		Entry("UserDefinedNetwork, layer2, mac-security", feature.MACSecurity, testscenariocudn.Layer2UDNUpdatesRejected),
	)
})

// runKubectlInputWithFullOutput is a convenience wrapper over kubectlBuilder that takes input to stdin
// It will also return the command's stderr.
func runKubectlInputWithFullOutput(namespace string, data string, args ...string) (string, string, error) {
	return e2ekubectl.NewKubectlCommand(namespace, args...).WithStdinData(data).ExecWithFullOutput()
}

// cleanupUpdateCRsTest deletes CRs created by UpdateCRScenario entries and verifies deletion,
// reusing cleanupValidateCRsTest's ValidateCRScenario-based logic.
func cleanupUpdateCRsTest(scenarios []testscenario.UpdateCRScenario) {
	crScenarios := make([]testscenario.ValidateCRScenario, len(scenarios))
	for i, s := range scenarios {
		crScenarios[i] = s.ValidateCRScenario
	}
	cleanupValidateCRsTest(crScenarios)
}

func cleanupValidateCRsTest(scenarios []testscenario.ValidateCRScenario) {
	for _, s := range scenarios {
		e2ekubectl.RunKubectlInput("", s.Manifest, "delete", "--ignore-not-found", "-f", "-")
	}
	// Verify each named resource is gone individually — a global "no resources found"
	// check is not parallel-safe since other concurrent tests may have live resources.
	for _, s := range scenarios {
		if s.Name == "" {
			continue
		}
		stdout, _, err := runKubectlInputWithFullOutput("", s.Manifest, "get", "--ignore-not-found", "-o", "name", "-f", "-")
		Expect(err).NotTo(HaveOccurred())
		Expect(stdout).To(BeEmpty(), "resource %q should have been deleted", s.Name)
	}
}
