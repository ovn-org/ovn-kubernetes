// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package infraprovider

import (
	"os/exec"
	"strings"

	"github.com/onsi/ginkgo/v2"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/api"
	"k8s.io/kubernetes/test/e2e/framework"
)

var infraProvider api.Provider

// Set infrastructure provider.
func Set(provider api.Provider) {
	infraProvider = provider
}

// Get infrastructure provider.
func Get() api.Provider {
	if infraProvider == nil {
		panic("infra provider not set")
	}
	return infraProvider
}

// SetupUnderlay skips the spec when its provider has no underlay implementation.
func SetupUnderlay(context api.Context, f *framework.Framework, underlay api.Underlay) error {
	provider, ok := context.(api.ClusterContextProvider)
	if !ok {
		ginkgo.Skip(Get().Name()+" provider does not support underlay setup", 2)
	}
	return provider.SetupUnderlay(f, underlay)
}

// IsKind returns true if cluster provider is KinD
func IsKind() bool {
	_, err := exec.LookPath("kubectl")
	if err != nil {
		framework.Logf("kubectl is not installed: %v", err)
		return false
	}
	currentCtx, err := exec.Command("kubectl", "config", "current-context").CombinedOutput()
	if err != nil {
		framework.Logf("unable to get current cluster context: %v", err)
		return false
	}
	if strings.HasPrefix(strings.TrimSpace(string(currentCtx)), "kind-") {
		return true
	}
	return false
}
