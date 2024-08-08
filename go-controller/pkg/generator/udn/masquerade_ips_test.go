package udn

import (
	"testing"

	. "github.com/onsi/gomega"

	"k8s.io/utils/ptr"
)

func TestAllocateMasqueradeIPs(t *testing.T) {
	type masqueradeIPs struct {
		GatewayRouter, ManagementPort string
	}
	var testCases = []struct {
		description   string
		networkID     int
		subnet        string
		expectedError *string
		expectedIPs   masqueradeIPs
	}{
		{
			description: "with proper network id 2 should return expected subnets",
			networkID:   2,
			subnet:      "169.254.0.0/17",
			expectedIPs: masqueradeIPs{GatewayRouter: "169.254.0.13/17", ManagementPort: "169.254.0.14/17"},
		},
		{
			description: "with proper network id 3 should return expected subnets",
			networkID:   3,
			subnet:      "169.254.0.0/17",
			expectedIPs: masqueradeIPs{GatewayRouter: "169.254.0.15/17", ManagementPort: "169.254.0.16/17"},
		},
		{
			description:   "with one of the two address beyond the subne should return an error",
			networkID:     9,
			subnet:        "169.254.169.0/29",
			expectedError: ptr.To("failed generating network id '9' test gateway router ip: generated ip 169.254.169.27 from the idx 27 is out of range in the network 169.254.169.0/29"),
		},
		{
			description:   "with network id 0 should return an error",
			networkID:     0,
			subnet:        "169.254.169.0/29",
			expectedError: ptr.To("invalid argument: network ID should be bigger that 0"),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.description, func(t *testing.T) {
			g := NewWithT(t)
			obtainedIPs, err := allocateMasqueradeIPs("test", tc.subnet, tc.networkID)
			if tc.expectedError != nil {
				g.Expect(err).To(MatchError(*tc.expectedError))
			} else {
				g.Expect(obtainedIPs).To(WithTransform(func(in *MasqueradeIPs) masqueradeIPs {
					return masqueradeIPs{
						GatewayRouter:  in.GatewayRouter.String(),
						ManagementPort: in.ManagementPort.String(),
					}
				}, Equal(tc.expectedIPs)))
			}
		})
	}
}
