package util

import (
	"net"

	"github.com/urfave/cli/v2"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("Util tests", func() {
	var app *cli.App

	BeforeEach(func() {
		// Restore global default values before each testcase
		config.PrepareTestConfig()

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags
	})

	It("test IPAddrToHWAddr()", func() {

		type testcase struct {
			name        string
			IP          string
			expectedMAC string
		}

		testcases := []testcase{
			{
				name:        "IPv4 to MAC",
				IP:          "10.1.2.3",
				expectedMAC: "0a:58:0a:01:02:03",
			},
			{
				name:        "IPv6 to MAC",
				IP:          "fd98::1",
				expectedMAC: "0a:58:fd:98:00:01",
			},
		}

		for _, tc := range testcases {
			ip := ovntest.MustParseIP(tc.IP)
			mac := IPAddrToHWAddr(ip)
			Expect(mac.String()).To(Equal(tc.expectedMAC), " test case \"%s\" returned %s instead of %s from IP %s", tc.name, mac.String(), tc.expectedMAC, ip.String())
		}
	})

	It("test JoinIPs", func() {
		type testcase struct {
			name string
			ips  []net.IP
			out  string
		}

		testcases := []testcase{
			{
				name: "empty list",
				ips:  nil,
				out:  "",
			},
			{
				name: "single IP",
				ips:  []net.IP{ovntest.MustParseIP("10.1.2.3")},
				out:  "10.1.2.3",
			},
			{
				name: "two IPs",
				ips: []net.IP{
					ovntest.MustParseIP("10.1.2.3"),
					ovntest.MustParseIP("10.4.5.6"),
				},
				out: "10.1.2.3, 10.4.5.6",
			},
			{
				name: "three IPs, mixed families",
				ips: []net.IP{
					ovntest.MustParseIP("10.1.2.3"),
					ovntest.MustParseIP("fd01::1234"),
					ovntest.MustParseIP("10.4.5.6"),
				},
				out: "10.1.2.3, fd01::1234, 10.4.5.6",
			},
		}

		for _, tc := range testcases {
			result := JoinIPs(tc.ips, ", ")
			Expect(result).To(Equal(tc.out), " test case \"%s\" returned wrong results for %#v", tc.name, tc.ips)
		}
	})

	It("test JoinIPNets", func() {
		type testcase struct {
			name  string
			cidrs []*net.IPNet
			out   string
		}

		testcases := []testcase{
			{
				name:  "empty list",
				cidrs: nil,
				out:   "",
			},
			{
				name:  "single CIDR",
				cidrs: []*net.IPNet{ovntest.MustParseIPNet("10.1.2.3/24")},
				out:   "10.1.2.3/24",
			},
			{
				name: "two CIDRs",
				cidrs: []*net.IPNet{
					ovntest.MustParseIPNet("10.1.2.3/24"),
					ovntest.MustParseIPNet("10.4.5.6/24"),
				},
				out: "10.1.2.3/24, 10.4.5.6/24",
			},
			{
				name: "three CIDRs, mixed families",
				cidrs: []*net.IPNet{
					ovntest.MustParseIPNet("10.1.2.3/24"),
					ovntest.MustParseIPNet("fd01::1234/64"),
					ovntest.MustParseIPNet("10.4.5.6/24"),
				},
				out: "10.1.2.3/24, fd01::1234/64, 10.4.5.6/24",
			},
		}

		for _, tc := range testcases {
			result := JoinIPNets(tc.cidrs, ", ")
			Expect(result).To(Equal(tc.out), " test case \"%s\" returned wrong results for %#v", tc.name, tc.cidrs)
		}
	})
})
