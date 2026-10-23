// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package node

import (
	"testing"

	"github.com/spf13/afero"

	utilfeature "k8s.io/apiserver/pkg/util/feature"

	nodenft "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/nftables"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestNodeSuite(t *testing.T) {
	// Disable WatchListClient feature gate for tests.
	// Fake clientsets from third-party libraries don't yet support WatchList semantics
	// introduced in K8s 1.35, causing informers to hang waiting for bookmark events.
	// The KUBE_FEATURE_WatchListClient env var alone is insufficient because
	// k8s.io/kubernetes/pkg/features replaces client-go's env-var-based feature gates
	// with utilfeature.DefaultMutableFeatureGate.
	// See: https://github.com/kubernetes/kubernetes/issues/135895
	if err := utilfeature.DefaultMutableFeatureGate.SetFromMap(map[string]bool{"WatchListClient": false}); err != nil {
		t.Fatalf("Failed to disable WatchListClient feature gate: %v", err)
	}
	RegisterFailHandler(Fail)
	// Specs delete the Node object their controller runs on behalf of; keep
	// that from terminating the test binary.
	ownNodeDeletedExit = func(nodeName string) {
		GinkgoWriter.Printf("ovnkube-node would exit: node %q was deleted\n", nodeName)
	}
	util.SetFakeIPTablesHelpers()
	nodenft.SetFakeNFTablesHelper()
	util.SetSupportsIPv6InterfaceForwarding(false)
	RunSpecs(t, "Node Suite")
}

var _ = BeforeEach(func() {
	util.AppFs = afero.NewMemMapFs()
})
