package util

import (
	"net"

	"github.com/urfave/cli"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"

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
				expectedMAC: "0A:58:0A:01:02:03",
			},
			{
				name:        "IPv6 to MAC",
				IP:          "fd98::1",
				expectedMAC: "0A:58:FD:98:00:01",
			},
		}

		for _, tc := range testcases {
			ip := net.ParseIP(tc.IP)
			mac := IPAddrToHWAddr(ip)
			Expect(mac).To(Equal(tc.expectedMAC), " test case \"%s\" returned %s instead of %s from IP %s", tc.name, mac, tc.expectedMAC, ip.String())
		}
	})
})
