// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package networkqos

import (
	"net"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/onsi/gomega"

	knet "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	nqosapi "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1alpha1"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
)

func TestValidatePort(t *testing.T) {
	for _, tt := range []struct {
		name  string
		port  *nqosapi.Port
		valid bool
	}{
		{name: "TCP port", port: &nqosapi.Port{Protocol: "TCP", Port: ptr.To(int32(80))}, valid: true},
		{name: "every UDP port", port: &nqosapi.Port{Protocol: "UDP"}, valid: true},
		{name: "SCTP port", port: &nqosapi.Port{Protocol: "SCTP", Port: ptr.To(int32(9999))}, valid: true},
		{name: "neither protocol nor port", port: &nqosapi.Port{}, valid: true},
		{name: "port without a protocol", port: &nqosapi.Port{Port: ptr.To(int32(80))}, valid: true},
		{name: "unsupported protocol", port: &nqosapi.Port{Protocol: "ICMP"}},
		{name: "protocol with surrounding text", port: &nqosapi.Port{Protocol: "xUDPx"}},
		{name: "protocol with a suffix, which older CRDs admit", port: &nqosapi.Port{Protocol: "TCPX", Port: ptr.To(int32(8080))}},
		{name: "protocol with a prefix, which older CRDs admit", port: &nqosapi.Port{Protocol: "xSCTP"}},
		{name: "lowercase protocol", port: &nqosapi.Port{Protocol: "udp", Port: ptr.To(int32(8080))}},
		{name: "null entry"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			if tt.valid {
				g.Expect(validatePort(tt.port)).To(gomega.Succeed())
			} else {
				g.Expect(validatePort(tt.port)).NotTo(gomega.Succeed())
			}
		})
	}
}

func TestNormalizeIPBlock(t *testing.T) {
	for _, tt := range []struct {
		name      string
		ipBlock   knet.IPBlock
		expected  knet.IPBlock
		errorText string
	}{
		{
			name:     "IPv4 network and exceptions with host bits",
			ipBlock:  knet.IPBlock{CIDR: "10.0.0.7/24", Except: []string{"10.0.0.129/25", "10.0.0.19/32"}},
			expected: knet.IPBlock{CIDR: "10.0.0.0/24", Except: []string{"10.0.0.128/25", "10.0.0.19/32"}},
		},
		{
			name:     "expanded uppercase IPv6 with host bits",
			ipBlock:  knet.IPBlock{CIDR: "2001:0DB8:0001:0002:1234::9/64", Except: []string{"2001:0DB8:1:2:8000::19/65"}},
			expected: knet.IPBlock{CIDR: "2001:db8:1:2::/64", Except: []string{"2001:db8:1:2:8000::/65"}},
		},
		{
			name:     "IPv4 default route",
			ipBlock:  knet.IPBlock{CIDR: "192.0.2.7/0", Except: []string{"198.51.100.19/24"}},
			expected: knet.IPBlock{CIDR: "0.0.0.0/0", Except: []string{"198.51.100.0/24"}},
		},
		{
			name:     "IPv6 default route",
			ipBlock:  knet.IPBlock{CIDR: "2001:db8::1/0", Except: []string{"2001:db8:ffff::42/48"}},
			expected: knet.IPBlock{CIDR: "::/0", Except: []string{"2001:db8:ffff::/48"}},
		},
		{
			name:     "IPv4 host prefix",
			ipBlock:  knet.IPBlock{CIDR: "192.0.2.19/32"},
			expected: knet.IPBlock{CIDR: "192.0.2.19/32"},
		},
		{
			name:     "IPv6 host prefix",
			ipBlock:  knet.IPBlock{CIDR: "2001:0DB8:0:0:0:0:0:19/128"},
			expected: knet.IPBlock{CIDR: "2001:db8::19/128"},
		},
		{
			name:     "IPv4-mapped IPv6 prefix, which stays an IPv6 prefix",
			ipBlock:  knet.IPBlock{CIDR: "::ffff:192.0.2.19/120", Except: []string{"::ffff:192.0.2.254/128"}},
			expected: knet.IPBlock{CIDR: "::ffff:192.0.2.0/120", Except: []string{"::ffff:192.0.2.254/128"}},
		},
		{
			name:     "IPv6 exception within an IPv4-mapped range of an IPv6 default route",
			ipBlock:  knet.IPBlock{CIDR: "::/0", Except: []string{"::ffff:0.0.0.0/96"}},
			expected: knet.IPBlock{CIDR: "::/0", Except: []string{"::ffff:0.0.0.0/96"}},
		},
		{
			name:     "bare IPv4 exceptions, as the per-network controllers accepted",
			ipBlock:  knet.IPBlock{CIDR: "128.116.0.0/17", Except: []string{"128.116.0.0", "128.116.0.255"}},
			expected: knet.IPBlock{CIDR: "128.116.0.0/17", Except: []string{"128.116.0.0/32", "128.116.0.255/32"}},
		},
		{
			name:     "bare IPv6 address",
			ipBlock:  knet.IPBlock{CIDR: "2001:0DB8::19"},
			expected: knet.IPBlock{CIDR: "2001:db8::19/128"},
		},
		{
			name:     "no CIDR",
			ipBlock:  knet.IPBlock{},
			expected: knet.IPBlock{},
		},
		{name: "invalid IPv4 prefix length", ipBlock: knet.IPBlock{CIDR: "10.0.0.1/33"}, errorText: "invalid CIDR"},
		{name: "invalid IPv6 prefix length", ipBlock: knet.IPBlock{CIDR: "2001:db8::1/129"}, errorText: "invalid CIDR"},
		{name: "address with a zone", ipBlock: knet.IPBlock{CIDR: "fe80::1%eth0"}, errorText: "invalid CIDR"},
		{
			name:      "malformed exception",
			ipBlock:   knet.IPBlock{CIDR: "10.0.0.7/24", Except: []string{"bad-prefix"}},
			errorText: "invalid exception 0",
		},
		{
			name:      "IPv4 exception of an IPv6 CIDR",
			ipBlock:   knet.IPBlock{CIDR: "::/0", Except: []string{"10.0.0.0/8"}},
			errorText: `exception 0 "10.0.0.0/8" is not of the IP family of CIDR ::/0`,
		},
		{
			name:      "IPv4-mapped IPv6 exception of an IPv4 CIDR",
			ipBlock:   knet.IPBlock{CIDR: "10.0.0.0/8", Except: []string{"::ffff:10.0.0.0/104"}},
			errorText: "is not of the IP family of CIDR 10.0.0.0/8",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			err := normalizeIPBlock(&tt.ipBlock)
			if tt.errorText != "" {
				g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring(tt.errorText)))
				return
			}
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(tt.ipBlock).To(gomega.Equal(tt.expected))
		})
	}
}

func TestClassifierCornerCases(t *testing.T) {
	for _, tt := range []struct {
		name       string
		classifier Classifier
		match      string
	}{
		{
			name: "unrestricted traffic",
		},
		{
			name: "port without a destination",
			classifier: Classifier{Ports: []*nqosapi.Port{
				{Protocol: "TCP", Port: ptr.To(int32(443))},
			}},
			match: "tcp && tcp.dst == 443",
		},
		{
			name: "multiple protocols without a destination",
			classifier: Classifier{Ports: []*nqosapi.Port{
				{Protocol: "UDP", Port: ptr.To(int32(53))},
				{Protocol: "TCP", Port: ptr.To(int32(443))},
			}},
			match: "((tcp && tcp.dst == 443) || (udp && udp.dst == 53))",
		},
		{
			name: "all TCP ports followed by a specific TCP port",
			classifier: Classifier{Ports: []*nqosapi.Port{
				{Protocol: "TCP"},
				{Protocol: "TCP", Port: ptr.To(int32(443))},
			}},
			match: "tcp",
		},
		{
			name: "specific TCP port followed by all TCP ports",
			classifier: Classifier{Ports: []*nqosapi.Port{
				{Protocol: "TCP", Port: ptr.To(int32(443))},
				{Protocol: "TCP"},
			}},
			match: "tcp",
		},
		{
			name: "protocol wildcard does not broaden another protocol",
			classifier: Classifier{Ports: []*nqosapi.Port{
				{Protocol: "TCP"},
				{Protocol: "TCP", Port: ptr.To(int32(443))},
				{Protocol: "UDP", Port: ptr.To(int32(53))},
			}},
			match: "((tcp) || (udp && udp.dst == 53))",
		},
		{
			name: "port without a protocol, which older CRDs admit",
			classifier: Classifier{Ports: []*nqosapi.Port{
				{Port: ptr.To(int32(8080))},
			}},
			match: "((sctp && sctp.dst == 8080) || (tcp && tcp.dst == 8080) || (udp && udp.dst == 8080))",
		},
		{
			name: "port without a protocol next to a protocol wildcard and a port of another entry",
			classifier: Classifier{Ports: []*nqosapi.Port{
				{Port: ptr.To(int32(8080))},
				{Protocol: "UDP"},
				{Protocol: "TCP", Port: ptr.To(int32(443))},
			}},
			match: "((sctp && sctp.dst == 8080) || (tcp && tcp.dst == {8080,443}) || (udp))",
		},
		{
			name: "protocol wildcard before a port without a protocol",
			classifier: Classifier{Ports: []*nqosapi.Port{
				{Protocol: "TCP"},
				{Port: ptr.To(int32(443))},
			}},
			match: "((sctp && sctp.dst == 443) || (tcp) || (udp && udp.dst == 443))",
		},
		{
			name: "protocol wildcard after a port without a protocol",
			classifier: Classifier{Ports: []*nqosapi.Port{
				{Port: ptr.To(int32(443))},
				{Protocol: "TCP"},
			}},
			match: "((sctp && sctp.dst == 443) || (tcp) || (udp && udp.dst == 443))",
		},
		{
			name:       "port entry without protocol and port, as the per-network controllers did",
			classifier: Classifier{Ports: []*nqosapi.Port{{}}},
		},
		{
			name: "IPv6 exception and protocol-only classifier",
			classifier: Classifier{
				Destinations: []*Destination{{IpBlock: &knet.IPBlock{CIDR: "2001:db8::/64", Except: []string{"2001:db8::1/128"}}}},
				Ports:        []*nqosapi.Port{{Protocol: "SCTP"}},
			},
			match: "ip6.dst == 2001:db8::/64 && ip6.dst != {2001:db8::1/128} && sctp",
		},
		{
			name:       "ipBlock without a CIDR, as the per-network controllers did",
			classifier: Classifier{Destinations: []*Destination{{IpBlock: &knet.IPBlock{}}}},
			match:      "(ip4.dst == 0.0.0.0/0 || ip6.dst == ::/0)",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			g.Expect(tt.classifier.ToQosMatchString(true, true)).To(gomega.Equal(tt.match))
		})
	}
}

func TestRouterReentryExclusion(t *testing.T) {
	_, v4, _ := net.ParseCIDR("10.128.0.0/16")
	_, v4b, _ := net.ParseCIDR("10.129.0.0/16")
	_, v6, _ := net.ParseCIDR("fd00:10:128::/64")
	for _, tt := range []struct {
		name       string
		macs       []string
		subnets    []*net.IPNet
		ipv4, ipv6 bool
		match      string
	}{
		{name: "no router", subnets: []*net.IPNet{v4}, ipv4: true},
		{
			name: "IPv4", macs: []string{"0a:58:0a:80:00:01"}, subnets: []*net.IPNet{v4}, ipv4: true,
			match: "!(eth.dst == 0a:58:0a:80:00:01 && ip4.dst == 10.128.0.0/16)",
		},
		{
			name: "dual-stack with two routers", macs: []string{"0a:58:0a:80:00:01", "0a:58:64:41:00:02"},
			subnets: []*net.IPNet{v4, v6}, ipv4: true, ipv6: true,
			match: "!(eth.dst == {0a:58:0a:80:00:01, 0a:58:64:41:00:02} && (ip4.dst == 10.128.0.0/16 || ip6.dst == fd00:10:128::/64))",
		},
		{
			name: "two IPv4 subnets", macs: []string{"0a:58:0a:80:00:01"}, subnets: []*net.IPNet{v4, v4b}, ipv4: true,
			match: "!(eth.dst == 0a:58:0a:80:00:01 && ip4.dst == {10.128.0.0/16, 10.129.0.0/16})",
		},
		{
			name: "only the enabled family", macs: []string{"0a:58:0a:80:00:01"}, subnets: []*net.IPNet{v4, v6}, ipv6: true,
			match: "!(eth.dst == 0a:58:0a:80:00:01 && ip6.dst == fd00:10:128::/64)",
		},
		{name: "no subnet of an enabled family", macs: []string{"0a:58:0a:80:00:01"}, subnets: []*net.IPNet{v6}, ipv4: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			g.Expect(routerReentryExclusion(tt.macs, tt.subnets, tt.ipv4, tt.ipv6)).To(gomega.Equal(tt.match))
		})
	}
}

func TestRouterReturnExclusion(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(routerReturnExclusion(nil)).To(gomega.BeEmpty())
	g.Expect(routerReturnExclusion([]string{"0a:58:0a:c0:b1:01"})).To(gomega.Equal("!(eth.src == 0a:58:0a:c0:b1:01)"))
	g.Expect(routerReturnExclusion([]string{"0a:58:0a:c0:b1:01", "0a:58:64:40:00:02"})).
		To(gomega.Equal("!(eth.src == {0a:58:0a:c0:b1:01, 0a:58:64:40:00:02})"))
}

func TestRouterReturnMatch(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(routerReturnMatch(nil)).To(gomega.BeEmpty())
	g.Expect(routerReturnMatch([]string{"0a:58:0a:c0:b1:01"})).To(gomega.Equal("eth.src == 0a:58:0a:c0:b1:01"))
	g.Expect(routerReturnMatch([]string{"0a:58:0a:c0:b1:01", "0a:58:64:40:00:02"})).
		To(gomega.Equal("eth.src == {0a:58:0a:c0:b1:01, 0a:58:64:40:00:02}"))
}

func TestAddressSetMatchOnlyUsesHeldFamilies(t *testing.T) {
	g := gomega.NewWithT(t)
	dualStack := addrSetRef{key: "key", hashV4: "a4", hashV6: "a6"}
	ipv4Only := addrSetRef{key: "key", hashV4: "a4"}
	g.Expect(dualStack.matchString(trafficDirSource, true, true)).To(gomega.Equal("(ip4.src == {$a4} || ip6.src == {$a6})"))
	g.Expect(dualStack.matchString(trafficDirDest, false, true)).To(gomega.Equal("ip6.dst == {$a6}"))
	// an address set created before the network gained IPv6 has no IPv6 hash
	g.Expect(ipv4Only.matchString(trafficDirSource, true, true)).To(gomega.Equal("ip4.src == {$a4}"))
	g.Expect(ipv4Only.matchString(trafficDirSource, false, true)).To(gomega.BeEmpty())
}

func TestIPBlockFamily(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(ipBlockFamily("10.0.0.0/8")).To(gomega.Equal("ip4"))
	g.Expect(ipBlockFamily("2001:db8::/64")).To(gomega.Equal("ip6"))
	g.Expect(ipBlockFamily("::ffff:10.0.0.0/104")).To(gomega.Equal("ip6"))
}

func TestQoSRulePrioritiesOfNetworkQoSObjectsDoNotOverlap(t *testing.T) {
	g := gomega.NewWithT(t)
	for priority := 0; priority < 100; priority++ {
		g.Expect(getQoSRulePriority(priority, types.NetworkQoSMaxRules-1)).To(gomega.BeNumerically("<", getQoSRulePriority(priority+1, 0)),
			"the last rule of priority %d must yield to the first rule of priority %d", priority, priority+1)
	}
	g.Expect(getQoSRulePriority(1, 0)).To(gomega.Equal(10020), "the priority documented by OKEP-4380")
	g.Expect(getQoSRulePriority(100, types.NetworkQoSMaxRules-1)).To(gomega.BeNumerically("<=", 32767), "the highest OVN QoS priority")
}

func TestFailureReasonIsStable(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(failureReason([]string{"a", "b"}, nil, nil)).To(gomega.Equal("failed to configure network(s) a, b"))
	g.Expect(failureReason([]string{"a"}, []string{"b"}, []string{"c"})).To(gomega.Equal("failed to configure network(s) a; " +
		"failed to resolve the addresses of the pods it selects on network(s) b; failed to remove it from network(s) c"))
}

func TestCountFailuresIgnoresSuccesses(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(countFailures([]metav1.Condition{
		{Type: conditionTypeReady + "node1", Status: metav1.ConditionTrue},
		{Type: conditionTypeReady + "node2", Status: metav1.ConditionFalse},
		{Type: "Other", Status: metav1.ConditionFalse},
	})).To(gomega.Equal(1))
}

func TestLegacyAddressSetsTakeOnlyTheMatchingOwners(t *testing.T) {
	g := gomega.NewWithT(t)
	owned := func(controller string) *nbdb.AddressSet {
		return &nbdb.AddressSet{UUID: controller, ExternalIDs: map[string]string{libovsdbops.OwnerControllerKey.String(): controller}}
	}
	var index legacyAddressSets
	g.Expect(index.take("ns:qos", func(string) bool { return true })).To(gomega.BeEmpty())
	index.add("ns:qos", owned("a"), owned("b"))
	g.Expect(index.take("ns:qos", func(controller string) bool { return controller == "a" })).To(gomega.ConsistOf(owned("a")))
	g.Expect(index.take("ns:qos", func(string) bool { return true })).To(gomega.ConsistOf(owned("b")))
	g.Expect(index.byObject).To(gomega.BeEmpty())
}

func TestTruncateMessageKeepsCharactersWhole(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(truncateMessage("short")).To(gomega.Equal("short"))
	truncated := truncateMessage(strings.Repeat("é", maxConditionMessageLength))
	g.Expect(len(truncated)).To(gomega.BeNumerically("<=", maxConditionMessageLength))
	g.Expect(utf8.ValidString(truncated)).To(gomega.BeTrue())
	g.Expect(truncated).To(gomega.HaveSuffix("..."))
	g.Expect(truncateMessage(truncated)).To(gomega.Equal(truncated), "a bounded message must be stable")
}
