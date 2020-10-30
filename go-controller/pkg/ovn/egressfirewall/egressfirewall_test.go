package egressfirewall

import (
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	egressfirewallapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1"
)

var _ = Describe("OVN test basic functions", func() {

	It("computes correct L4Match", func() {
		type testcase struct {
			ports         []egressfirewallapi.EgressFirewallPort
			expectedMatch string
		}
		testcases := []testcase{
			{
				expectedMatch: "",
			},
			{
				ports: []egressfirewallapi.EgressFirewallPort{
					{
						Protocol: "TCP",
					},
				},
				expectedMatch: "(tcp)",
			},
			{
				ports: []egressfirewallapi.EgressFirewallPort{
					{
						Protocol: "UDP",
					},
				},
				expectedMatch: "(udp)",
			},
			{
				ports: []egressfirewallapi.EgressFirewallPort{
					{
						Protocol: "SCTP",
					},
				},
				expectedMatch: "(sctp)",
			},
			{
				ports: []egressfirewallapi.EgressFirewallPort{
					{
						Protocol: "TCP",
						Port:     100,
					},
				},
				expectedMatch: "(tcp && ( tcp.dst == 100 ))",
			},
			{
				ports: []egressfirewallapi.EgressFirewallPort{
					{
						Protocol: "TCP",
						Port:     100,
					},
					{
						Protocol: "UDP",
					},
				},
				expectedMatch: "(udp) || (tcp && ( tcp.dst == 100 ))",
			},
			{
				ports: []egressfirewallapi.EgressFirewallPort{
					{
						Protocol: "TCP",
						Port:     100,
					},
					{
						Protocol: "SCTP",
						Port:     13,
					},
					{
						Protocol: "TCP",
						Port:     102,
					},
					{
						Protocol: "UDP",
						Port:     400,
					},
				},
				expectedMatch: "(udp && ( udp.dst == 400 )) || (tcp && ( tcp.dst == 100 || tcp.dst == 102 )) || (sctp && ( sctp.dst == 13 ))",
			},
		}
		for _, test := range testcases {
			l4Match := egressGetL4Match(test.ports)
			Expect(test.expectedMatch).To(Equal(l4Match))
		}
	})
})
