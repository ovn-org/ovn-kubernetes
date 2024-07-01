package udn

import (
	"net"
	"testing"

	. "github.com/onsi/gomega"

	"k8s.io/utils/ptr"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
)

func TestAllocateMasqueradeIPs(t *testing.T) {
	var testCases = []struct {
		description            string
		networkID              int
		subnet                 string
		maxUserDefinedNetworks uint
		expectedError          *string
		expectedIPs            []string
	}{
		{
			description:            "with proper network id should return expected subnets",
			networkID:              2,
			subnet:                 "169.254.169.0/19",
			maxUserDefinedNetworks: 10,
			expectedIPs:            []string{"169.254.169.13", "169.254.169.14"},
		},
		{
			description:            "with one of the two address beyond the subne should return an error",
			networkID:              9,
			subnet:                 "169.254.169.0/29",
			maxUserDefinedNetworks: 20,
			expectedError:          ptr.To("failed calculating user defined network test masquerade IPs: ip 169.254.169.27 out of bound for subnet 169.254.169.0/29"),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.description, func(t *testing.T) {
			g := NewWithT(t)
			config.UserDefinedNetworks.MaxNetworks = tc.maxUserDefinedNetworks
			obtainedIPs, err := allocateMasqueradeIPs("test", tc.subnet, uint(tc.networkID), 2)
			if tc.expectedError != nil {
				g.Expect(err).To(MatchError(*tc.expectedError))
			} else {
				g.Expect(obtainedIPs).To(WithTransform(func(in []net.IP) []string {
					ips := []string{}
					for _, ip := range in {
						ips = append(ips, ip.String())
					}
					return ips
				}, Equal(tc.expectedIPs)))
			}
		})
	}
}
