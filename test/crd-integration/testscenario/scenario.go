// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package testscenario

// ValidateCRScenario represent test scenario where a manifest is applied and failed with the expected error.
// Name, if non-empty, is the resource name used by cleanup to verify the resource was deleted.
// It should be set for valid scenarios (which actually create resources) and left empty for invalid ones.
type ValidateCRScenario struct {
	Description string
	Name        string
	Manifest    string
	ExpectedErr string
}

// UpdateCRScenario represents a test scenario where a resource is first created with InitialManifest,
// then updated with the embedded Manifest. If ExpectedErr is non-empty, the update is expected to
// fail with that error substring; otherwise it is expected to succeed.
type UpdateCRScenario struct {
	InitialManifest string
	ValidateCRScenario
}
