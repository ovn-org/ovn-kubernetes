// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package userdefinednetwork

import (
	"testing"

	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var (
	testEnv *envtest.Environment
	restCfg *rest.Config
)

var _ = BeforeSuite(func() {
	testEnv = &envtest.Environment{
		CRDDirectoryPaths: []string{"testdata/crds"},
	}
	var err error
	restCfg, err = testEnv.Start()
	Expect(err).NotTo(HaveOccurred())
	Expect(restCfg).NotTo(BeNil())
})

var _ = AfterSuite(func() {
	if testEnv != nil {
		Expect(testEnv.Stop()).To(Succeed())
	}
})

func TestUserDefinedNetworkController(t *testing.T) {
	// Disable WatchListClient feature gate for tests.
	// Fake clientsets from third-party libraries don't yet support WatchList semantics
	// introduced in K8s 1.35, causing informers to hang waiting for bookmark events.
	// See: https://github.com/kubernetes/kubernetes/issues/135895
	t.Setenv("KUBE_FEATURE_WatchListClient", "false")
	RegisterFailHandler(Fail)
	RunSpecs(t, "User Defined Network Controller Suite")
}
