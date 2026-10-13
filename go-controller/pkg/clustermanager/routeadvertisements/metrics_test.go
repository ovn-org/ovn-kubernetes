// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package routeadvertisements

import (
	"testing"

	frrtypes "github.com/metallb/frr-k8s/api/v1beta1"
)

func TestCountAdvertisedPrefixes(t *testing.T) {
	config := func(routers ...frrtypes.Router) *frrtypes.FRRConfiguration {
		c := &frrtypes.FRRConfiguration{}
		c.Spec.BGP.Routers = routers
		return c
	}

	tests := []struct {
		name      string
		generated []*frrtypes.FRRConfiguration
		wantV4    int
		wantV6    int
	}{
		{
			name:      "no generated configurations",
			generated: nil,
		},
		{
			name:      "generated configuration with no routers",
			generated: []*frrtypes.FRRConfiguration{config()},
		},
		{
			name:      "router with no prefixes",
			generated: []*frrtypes.FRRConfiguration{config(frrtypes.Router{ASN: 1})},
		},
		{
			name: "one prefix per node, layer3 style",
			generated: []*frrtypes.FRRConfiguration{
				config(frrtypes.Router{ASN: 1, Prefixes: []string{"10.244.0.0/24"}}),
				config(frrtypes.Router{ASN: 1, Prefixes: []string{"10.244.1.0/24"}}),
				config(frrtypes.Router{ASN: 1, Prefixes: []string{"10.244.2.0/24"}}),
			},
			wantV4: 3,
		},
		{
			name: "same prefix from every node counts once, layer2 anycast style",
			generated: []*frrtypes.FRRConfiguration{
				config(frrtypes.Router{ASN: 1, Prefixes: []string{"10.210.0.0/16"}}),
				config(frrtypes.Router{ASN: 1, Prefixes: []string{"10.210.0.0/16"}}),
				config(frrtypes.Router{ASN: 1, Prefixes: []string{"10.210.0.0/16"}}),
			},
			wantV4: 1,
		},
		{
			name: "ipv6 only",
			generated: []*frrtypes.FRRConfiguration{
				config(frrtypes.Router{ASN: 1, Prefixes: []string{"fd01::/64", "fd02::/64"}}),
			},
			wantV6: 2,
		},
		{
			name: "dual stack is split by family",
			generated: []*frrtypes.FRRConfiguration{
				config(frrtypes.Router{ASN: 1, Prefixes: []string{"10.244.0.0/24", "fd01::/64"}}),
				config(frrtypes.Router{ASN: 1, Prefixes: []string{"10.244.1.0/24", "fd02::/64"}}),
			},
			wantV4: 2,
			wantV6: 2,
		},
		{
			name: "host routes, as advertised for EgressIPs and EVPN VTEPs",
			generated: []*frrtypes.FRRConfiguration{
				config(frrtypes.Router{ASN: 1, Prefixes: []string{"172.18.0.3/32", "fd00::3/128"}}),
			},
			wantV4: 1,
			wantV6: 1,
		},
		{
			name: "prefixes are counted across all routers of a configuration",
			generated: []*frrtypes.FRRConfiguration{
				config(
					frrtypes.Router{ASN: 1, Prefixes: []string{"172.18.0.3/32"}},
					frrtypes.Router{ASN: 1, VRF: "red", Prefixes: []string{"10.210.0.0/16"}},
				),
			},
			wantV4: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v4, v6 := countAdvertisedPrefixes(tt.generated)
			if v4 != tt.wantV4 || v6 != tt.wantV6 {
				t.Errorf("countAdvertisedPrefixes() = (v4=%d, v6=%d), want (v4=%d, v6=%d)",
					v4, v6, tt.wantV4, tt.wantV6)
			}
		})
	}
}
