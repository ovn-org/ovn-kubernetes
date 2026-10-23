// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package managementport

import (
	"testing"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	nodenft "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/nftables"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func TestAdder(t *testing.T) {
	gomega.RegisterFailHandler(ginkgo.Fail)
	util.SetFakeIPTablesHelpers()
	nodenft.SetFakeNFTablesHelper()
	util.SetSupportsIPv6InterfaceForwarding(false)
	ginkgo.RunSpecs(t, "Management Port Suite")
}

// Every spec writes sysctls into its own scratch directory, so a spec sees
// only the values it set itself.
var _ = ginkgo.BeforeEach(func() {
	ginkgo.DeferCleanup(util.SetProcSysNetForTesting(ginkgo.GinkgoT().TempDir()))
})
