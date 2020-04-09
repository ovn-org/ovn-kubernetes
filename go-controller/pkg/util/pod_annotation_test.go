package util

import (
	"net"

	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("Pod annotation tests", func() {
	It("marshals network info to pod annotations", func() {
		type testcase struct {
			name string
			in   *PodAnnotation
			out  map[string]string
		}

		testcases := []testcase{
			{
				name: "Single-stack IPv4",
				in: &PodAnnotation{
					IPs:      []*net.IPNet{ovntest.MustParseIPNet("192.168.0.5/24")},
					MAC:      ovntest.MustParseMAC("0A:58:FD:98:00:01"),
					Gateways: []net.IP{net.ParseIP("192.168.0.1")},
				},
				out: map[string]string{
					"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["192.168.0.5/24"],"mac_address":"0a:58:fd:98:00:01","gateway_ips":["192.168.0.1"],"ip_address":"192.168.0.5/24","gateway_ip":"192.168.0.1"}}`,
				},
			},
			{
				name: "No GW",
				in: &PodAnnotation{
					IPs: []*net.IPNet{ovntest.MustParseIPNet("192.168.0.5/24")},
					MAC: ovntest.MustParseMAC("0A:58:FD:98:00:01"),
				},
				out: map[string]string{
					"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["192.168.0.5/24"],"mac_address":"0a:58:fd:98:00:01","ip_address":"192.168.0.5/24"}}`,
				},
			},
			{
				name: "Routes",
				in: &PodAnnotation{
					IPs:      []*net.IPNet{ovntest.MustParseIPNet("192.168.0.5/24")},
					MAC:      ovntest.MustParseMAC("0A:58:FD:98:00:01"),
					Gateways: []net.IP{net.ParseIP("192.168.0.1")},
					Routes: []PodRoute{
						{
							Dest:    ovntest.MustParseIPNet("192.168.1.0/24"),
							NextHop: net.ParseIP("192.168.1.1"),
						},
					},
				},
				out: map[string]string{
					"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["192.168.0.5/24"],"mac_address":"0a:58:fd:98:00:01","gateway_ips":["192.168.0.1"],"routes":[{"dest":"192.168.1.0/24","nextHop":"192.168.1.1"}],"ip_address":"192.168.0.5/24","gateway_ip":"192.168.0.1"}}`,
				},
			},
			{
				name: "Single-stack IPv6",
				in: &PodAnnotation{
					IPs:      []*net.IPNet{ovntest.MustParseIPNet("fd01::1234/64")},
					MAC:      ovntest.MustParseMAC("0A:58:FD:98:00:01"),
					Gateways: []net.IP{net.ParseIP("fd01::1")},
				},
				out: map[string]string{
					"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["fd01::1234/64"],"mac_address":"0a:58:fd:98:00:01","gateway_ips":["fd01::1"],"ip_address":"fd01::1234/64","gateway_ip":"fd01::1"}}`,
				},
			},
			{
				name: "Dual-stack",
				in: &PodAnnotation{
					IPs: []*net.IPNet{
						ovntest.MustParseIPNet("192.168.0.5/24"),
						ovntest.MustParseIPNet("fd01::1234/64"),
					},
					MAC: ovntest.MustParseMAC("0A:58:FD:98:00:01"),
					Gateways: []net.IP{
						net.ParseIP("192.168.1.0"),
						net.ParseIP("fd01::1"),
					},
				},
				out: map[string]string{
					"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["192.168.0.5/24","fd01::1234/64"],"mac_address":"0a:58:fd:98:00:01","gateway_ips":["192.168.1.0","fd01::1"]}}`,
				},
			},
		}

		for _, tc := range testcases {
			marshalled, err := MarshalPodAnnotation(tc.in)
			Expect(err).NotTo(HaveOccurred(), "test case %q got unexpected marshalling error", tc.name)
			Expect(marshalled).To(Equal(tc.out), "test case %q marshalled to wrong value", tc.name)
			unmarshalled, err := UnmarshalPodAnnotation(marshalled)
			Expect(err).NotTo(HaveOccurred(), "test case %q got unexpected unmarshalling error", tc.name)
			Expect(unmarshalled).To(Equal(tc.in), "test case %q unmarshalled to wrong value", tc.name)
		}
	})
})
