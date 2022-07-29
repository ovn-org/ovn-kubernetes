package node

import (
	"fmt"
	"github.com/coreos/go-iptables/iptables"
	"github.com/onsi/ginkgo/extensions/table"
	"net"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("IPTables rules helpers", func() {
	const (
		ifaceIP   = "10.10.11.12"
		ifaceName = "eth0"
	)

	table.DescribeTable("equal rules are compared", func(expectedRule iptRule, kernelRule string) {
		Expect(expectedRule.IsEqual(kernelRule)).To(BeTrue())
	},
		table.Entry(
			"SNAT management port rule",
			generateNATMgmtChainSNATRule(ifaceName, net.ParseIP(ifaceIP)),
			snatRuleForMgmtChainFromKernel(ifaceName, ifaceIP),
		),
		table.Entry(
			"SNAT management port rule",
			generateNATPostRoutingJumpToMgmtPortChain(ifaceName, iptables.ProtocolIPv4),
			jumpToMnmtChainFromKernel(ifaceName),
		),
	)

	table.DescribeTable("different rules are identified", func(inputIfaceName, inputIP string) {
		Expect(
			generateNATMgmtChainSNATRule(ifaceName, net.ParseIP(ifaceIP)).IsEqual(
				snatRuleForMgmtChainFromKernel(inputIfaceName, inputIP))).To(BeFalse())
	},
		table.Entry("Successfully compares a different rule as false", "fake-iface-name", ifaceIP),
		table.Entry("Successfully compares a different rule as false", ifaceName, "10.11.12.13"),
	)

	It("Parse rules from the kernel iptables", func() {
		const (
			ifaceIP   = "10.10.11.12"
			ifaceName = "eth0"
		)

		Expect(
			parseRule(
				snatRuleForMgmtChainFromKernel(ifaceName, ifaceIP))).To(
			ConsistOf("-o", ifaceName, "-m", "comment", "--comment", "OVN SNAT to Management Port", "-j", "SNAT", "--to-source", ifaceIP))
	})
})

func snatRuleForMgmtChainFromKernel(ifaceName string, ifaceIP string) string {
	return fmt.Sprintf("-A OVN-KUBE-SNAT-MGMTPORT -o %s -m comment --comment \"OVN SNAT to Management Port\" -j SNAT --to-source %s", ifaceName, net.ParseIP(ifaceIP))
}

func jumpToMnmtChainFromKernel(ifaceName string) string {
	return fmt.Sprintf("-I POSTROUTING -o %s -j %s", ifaceName, iptableMgmPortChain)
}
