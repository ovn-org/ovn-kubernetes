// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build linux
// +build linux

package node

import (
	"fmt"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Gateway Init EXGW", func() {
	var (
		fexec      *ovntest.FakeExec
		ovsCleanup *libovsdbtest.Context
	)

	BeforeEach(func() {
		Expect(config.PrepareTestConfig()).To(Succeed())
		fexec = ovntest.NewFakeExec()
		Expect(util.SetExec(fexec)).To(Succeed())
	})

	AfterEach(func() {
		if ovsCleanup != nil {
			ovsCleanup.Cleanup()
			ovsCleanup = nil
		}
	})

	Context("interfaceForEXGW", func() {
		It("returns intfName if intfName is a bridge", func() {
			ovsClient, cleanup, err := libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{
				OVSData: []libovsdbtest.TestData{
					&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{"br-ex-uuid"}},
					&vswitchd.Bridge{UUID: "br-ex-uuid", Name: "br-ex"},
				},
			})
			Expect(err).NotTo(HaveOccurred())
			ovsCleanup = cleanup

			result, err := interfaceForEXGW(ovsClient, "br-ex")
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal("br-ex"))
		})

		It("returns bridgeName if bridge exists and port is correctly attached", func() {
			ovsClient, cleanup, err := libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{
				OVSData: []libovsdbtest.TestData{
					&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{"breth1-uuid"}},
					&vswitchd.Bridge{UUID: "breth1-uuid", Name: "breth1", Ports: []string{"eth1-port-uuid"}},
					&vswitchd.Port{UUID: "eth1-port-uuid", Name: "eth1", Interfaces: []string{"eth1-iface-uuid"}},
					&vswitchd.Interface{UUID: "eth1-iface-uuid", Name: "eth1"},
				},
			})
			Expect(err).NotTo(HaveOccurred())
			ovsCleanup = cleanup
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovs-vsctl --timeout=15 port-to-br eth1",
				Output: "breth1",
			})

			result, err := interfaceForEXGW(ovsClient, "eth1")
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal("breth1"))
			Expect(fexec.CalledMatchesExpected()).To(BeTrue())
		})

		It("returns intfName if bridge exists but port is NOT attached (Issue #6111)", func() {
			ovsClient, cleanup, err := libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{
				OVSData: []libovsdbtest.TestData{
					&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{"breth1-uuid"}},
					&vswitchd.Bridge{UUID: "breth1-uuid", Name: "breth1"},
				},
			})
			Expect(err).NotTo(HaveOccurred())
			ovsCleanup = cleanup
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd: "ovs-vsctl --timeout=15 port-to-br eth1",
				Err: fmt.Errorf("ovs-vsctl: no port named eth1"),
			})

			result, err := interfaceForEXGW(ovsClient, "eth1")
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal("eth1"))
			Expect(fexec.CalledMatchesExpected()).To(BeTrue())
		})

		It("returns intfName if bridge exists but port is attached to DIFFERENT bridge", func() {
			ovsClient, cleanup, err := libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{
				OVSData: []libovsdbtest.TestData{
					&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{"breth1-uuid", "br-other-uuid"}},
					&vswitchd.Bridge{UUID: "breth1-uuid", Name: "breth1"},
					&vswitchd.Bridge{UUID: "br-other-uuid", Name: "br-other", Ports: []string{"eth1-port-uuid"}},
					&vswitchd.Port{UUID: "eth1-port-uuid", Name: "eth1", Interfaces: []string{"eth1-iface-uuid"}},
					&vswitchd.Interface{UUID: "eth1-iface-uuid", Name: "eth1"},
				},
			})
			Expect(err).NotTo(HaveOccurred())
			ovsCleanup = cleanup
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovs-vsctl --timeout=15 port-to-br eth1",
				Output: "br-other",
			})

			result, err := interfaceForEXGW(ovsClient, "eth1")
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal("eth1"))
			Expect(fexec.CalledMatchesExpected()).To(BeTrue())
		})

		It("returns intfName if neither intf nor bridge exists", func() {
			ovsClient, cleanup, err := libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{
				OVSData: []libovsdbtest.TestData{},
			})
			Expect(err).NotTo(HaveOccurred())
			ovsCleanup = cleanup

			result, err := interfaceForEXGW(ovsClient, "eth1")
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal("eth1"))
		})
	})
})
