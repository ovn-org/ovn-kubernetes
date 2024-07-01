package udn

import (
	"testing"

	. "github.com/onsi/gomega"

	"k8s.io/utils/ptr"
)

func TestAllocateMasqueradeIPs(t *testing.T) {
	type masqueradeIPs struct {
		Shared, Local string
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
			subnet:      "169.254.169.0/19",
			expectedIPs: masqueradeIPs{Shared: "169.254.169.13", Local: "169.254.169.14"},
		},
		{
			description: "with proper network id 3 should return expected subnets",
			networkID:   3,
			subnet:      "169.254.169.0/19",
			expectedIPs: masqueradeIPs{Shared: "169.254.169.15", Local: "169.254.169.16"},
		},
		{
			description:   "with one of the two address beyond the subne should return an error",
			networkID:     9,
			subnet:        "169.254.169.0/29",
			expectedError: ptr.To("failed calculating user defined network test masquerade IPs: IP 169.254.169.27 is not within subnet 169.254.169.0/29"),
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
						Shared: in.Shared.String(),
						Local:  in.Local.String(),
					}
				}, Equal(tc.expectedIPs)))
			}
		})
	}
}
