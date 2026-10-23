// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package deploymentconfig

import (
	"strings"

	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
)

// GetTemplateContainerEnv gets the value of an environment variable in a
// container template. A missing variable returns an empty string; kubectl
// failures fail the test rather than being treated as an unset variable.
func GetTemplateContainerEnv(namespace, resource, container, key string) string {
	args := []string{"get", resource,
		"-o=jsonpath='{.spec.template.spec.containers[?(@.name==\"" + container + "\")].env[?(@.name==\"" + key + "\")].value}'"}
	value := e2ekubectl.RunKubectlOrDie(namespace, args...)
	return strings.Trim(value, "'")
}
