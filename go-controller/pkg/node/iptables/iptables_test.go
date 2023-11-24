package iptables

import (
	"github.com/coreos/go-iptables/iptables"
	"github.com/onsi/ginkgo"
	"github.com/onsi/gomega"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

var _ = ginkgo.Context("IPTables", func() {
	ginkgo.Describe("Equals", func() {
		// Our version of ginkgo does not ship with DescribeTable, so use the test case pattern instead.
		tcs := []struct {
			it         string
			isEqual    bool
			inputRule  Rule
			outputRule Rule
		}{
			{
				"Equals receives 2 equal rules",
				true,
				Rule{
					Protocol: 0,
					Table:    "filter",
					Chain:    "FORWARD",
					Args:     []string{"-p", "tcp", "--dport", "443", "-j", "REJECT"},
				},
				Rule{
					Protocol: 0,
					Table:    "filter",
					Chain:    "FORWARD",
					Args:     []string{"-p", "tcp", "--dport", "443", "-j", "REJECT"},
				},
			},
			{
				"Equals receives 2 rules which differ",
				false,
				Rule{
					Protocol: 0,
					Table:    "filter",
					Chain:    "FORWARD",
					Args:     []string{"-p", "tcp", "--dport", "443", "-j", "REJECT"},
				},
				Rule{
					Protocol: 0,
					Table:    "filter",
					Chain:    "FORWARD",
					Args:     []string{"-p", "tcp", "--dport", "443", "REJECT", "-j"},
				},
			},
		}
		for _, tc := range tcs {
			ginkgo.It(tc.it, func() {
				gomega.Expect(tc.inputRule.Equals(tc.outputRule)).To(gomega.Equal(tc.isEqual))
			})
		}
	})

	ginkgo.Describe("ParseAppendRule", func() {
		// Our version of ginkgo does not ship with DescribeTable, so use the test case pattern instead.
		tcs := []struct {
			it         string
			protocol   iptables.Protocol
			table      string
			input      string
			isValid    bool
			outputRule Rule
		}{
			{
				"ParseRule receives a valid input string",
				0,
				"filter",
				"-A FORWARD -p tcp --dport 443 -j REJECT",
				true,
				Rule{
					Protocol: 0,
					Table:    "filter",
					Chain:    "FORWARD",
					Args:     []string{"-p", "tcp", "--dport", "443", "-j", "REJECT"},
				},
			},
			{
				"ParseRule receives an invalid input string",
				0,
				"filter",
				"-N NEW-TABLE",
				false,
				Rule{},
			},
			{
				"ParseRule receives another invalid input string",
				0,
				"filter",
				"-P FORWARD ACCEPT",
				false,
				Rule{},
			},
		}
		for _, tc := range tcs {
			ginkgo.It(tc.it, func() {
				inputRule, err := ParseAppendRule(tc.protocol, tc.table, tc.input)
				if !tc.isValid {
					gomega.Expect(err).To(gomega.HaveOccurred())
					return
				}
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(tc.outputRule.Equals(inputRule))
			})
		}
	})

	ginkgo.Describe("ListRules", func() {
		ginkgo.When("A set of IPTables rules is provided", func() {
			var err error
			iptV4, iptV6 := util.SetFakeIPTablesHelpers()

			// Populate chains and rules.
			inputRules := []Rule{
				{
					Table:    "filter",
					Chain:    "FORWARD",
					Args:     []string{"-i", "breth0", "-j", "DROP"},
					Protocol: iptables.ProtocolIPv4,
				},
				{
					Table:    "filter",
					Chain:    "OVN-KUBE-FORWARD-DROP",
					Args:     []string{"-i", "eth1", "-j", "DROP"},
					Protocol: iptables.ProtocolIPv6,
				},
				{
					Table:    "filter",
					Chain:    "OVN-KUBE-FORWARD-DROP",
					Args:     []string{"-o", "eth1", "-j", "DROP"},
					Protocol: iptables.ProtocolIPv6,
				},
			}
			chains := [][]string{
				{"filter", "FORWARD"},
				{"filter", "OVN-KUBE-FORWARD-DROP"},
			}
			for _, chain := range chains {
				err = iptV4.NewChain(chain[0], chain[1])
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = iptV6.NewChain(chain[0], chain[1])
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			}
			for _, i := range inputRules {
				if i.Protocol == iptables.ProtocolIPv4 {
					err = iptV4.Append(i.Table, i.Chain, i.Args...)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
				}
				if i.Protocol == iptables.ProtocolIPv6 {
					err = iptV6.Append(i.Table, i.Chain, i.Args...)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
				}
			}

			countMatches := func(inputRules, listedRules []Rule, protocol iptables.Protocol, table, chain string) int {
				matches := 0
				for _, i := range inputRules {
					if i.Protocol == protocol && i.Table == table && i.Chain == chain {
						for _, l := range listedRules {
							if i.Equals(l) {
								matches++
								break
							}
						}
					}
				}
				return matches
			}

			ginkgo.It("Parses the FORWARD chain for IPv4", func() {
				proto := iptables.ProtocolIPv4
				table := "filter"
				chain := "FORWARD"
				listedRules, err := ListRules(proto, table, chain)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(countMatches(inputRules, listedRules, proto, table, chain)).
					To(gomega.Equal(len(listedRules)))
			})

			ginkgo.It("Parses the OVN-KUBE-FORWARD-DROP chain for IPv6", func() {
				proto := iptables.ProtocolIPv6
				table := "filter"
				chain := "OVN-KUBE-FORWARD-DROP"
				listedRules, err := ListRules(proto, table, chain)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(countMatches(inputRules, listedRules, proto, table, chain)).
					To(gomega.Equal(len(listedRules)))
			})
		})
	})
})
