// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package routeadvertisements

import (
	"net/netip"
	"testing"

	frrtypes "github.com/metallb/frr-k8s/api/v1beta1"
)

func TestGenerateRawConfig(t *testing.T) {
	tests := []struct {
		name         string
		selected     *selectedNetworks
		vrfNeighbors map[string][]string
		vrfASNs      map[string]uint32
		want         string
	}{
		{
			name:         "empty input",
			selected:     &selectedNetworks{},
			vrfNeighbors: map[string][]string{},
			vrfASNs:      map[string]uint32{},
			want:         "",
		},
		{
			name:         "default and non-default VRF unicast only",
			selected:     &selectedNetworks{},
			vrfNeighbors: map[string][]string{"": {"192.168.1.1", "fd00::1"}, "red": {"10.0.0.1", "fd00::2"}},
			vrfASNs:      map[string]uint32{"": 65000, "red": 65000},
			want: `router bgp 65000
 address-family ipv4 unicast
  neighbor 192.168.1.1 allowas-in origin
 exit-address-family
 address-family ipv6 unicast
  neighbor fd00::1 allowas-in origin
 exit-address-family
exit
!
router bgp 65000 vrf red
 address-family ipv4 unicast
  neighbor 10.0.0.1 allowas-in origin
 exit-address-family
 address-family ipv6 unicast
  neighbor fd00::2 allowas-in origin
 exit-address-family
exit
!
`,
		},
		{
			name:         "unnumbered neighbor identified by interface name",
			selected:     &selectedNetworks{},
			vrfNeighbors: map[string][]string{"": {"enp4s0f0np0", "192.168.1.1"}},
			vrfASNs:      map[string]uint32{"": 4200000001},
			want: `router bgp 4200000001
 address-family ipv4 unicast
  neighbor 192.168.1.1 allowas-in origin
  neighbor enp4s0f0np0 allowas-in origin
 exit-address-family
 address-family ipv6 unicast
  neighbor enp4s0f0np0 allowas-in origin
 exit-address-family
exit
!
`,
		},
		{
			name: "MAC-VRF without route target",
			selected: &selectedNetworks{
				macVRFConfigs: []*vrfConfig{
					{vni: 1000},
				},
			},
			vrfNeighbors: map[string][]string{"": {"192.168.1.1"}},
			vrfASNs:      map[string]uint32{"": 65000},
			want: `router bgp 65000
 address-family ipv4 unicast
  neighbor 192.168.1.1 allowas-in origin
 exit-address-family
 address-family l2vpn evpn
  neighbor 192.168.1.1 activate
  neighbor 192.168.1.1 allowas-in origin
  advertise-all-vni
 exit-address-family
exit
!
`,
		},
		{
			name: "MAC-VRF with route target",
			selected: &selectedNetworks{
				macVRFConfigs: []*vrfConfig{
					{vni: 1000, routeTarget: "65000:1000"},
				},
			},
			vrfNeighbors: map[string][]string{"": {"192.168.1.1"}},
			vrfASNs:      map[string]uint32{"": 65000},
			want: `router bgp 65000
 address-family ipv4 unicast
  neighbor 192.168.1.1 allowas-in origin
 exit-address-family
 address-family l2vpn evpn
  neighbor 192.168.1.1 activate
  neighbor 192.168.1.1 allowas-in origin
  advertise-all-vni
  vni 1000
   route-target import 65000:1000
   route-target export 65000:1000
  exit-vni
 exit-address-family
exit
!
`,
		},
		{
			name: "IP-VRF IPv6",
			selected: &selectedNetworks{
				ipVRFConfigs: []*ipVRFConfig{
					{
						vrfConfig: vrfConfig{vni: 2000, routeTarget: "65000:2000"},
						vrfName:   "blue",
						hasIPv6:   true,
					},
				},
			},
			vrfNeighbors: map[string][]string{"": {"192.168.1.1"}},
			vrfASNs:      map[string]uint32{"": 65000, "blue": 65000},
			want: `router bgp 65000
 address-family ipv4 unicast
  neighbor 192.168.1.1 allowas-in origin
 exit-address-family
 address-family l2vpn evpn
  neighbor 192.168.1.1 activate
  neighbor 192.168.1.1 allowas-in origin
  advertise-all-vni
 exit-address-family
exit
!
vrf blue
 vni 2000
exit-vrf
!
router bgp 65000 vrf blue
 address-family l2vpn evpn
  advertise ipv6 unicast
  route-target import 65000:2000
  route-target export 65000:2000
 exit-address-family
exit
!
`,
		},
		{
			name: "IP-VRF dual stack",
			selected: &selectedNetworks{
				ipVRFConfigs: []*ipVRFConfig{
					{
						vrfConfig: vrfConfig{vni: 2000, routeTarget: "65000:2000"},
						vrfName:   "blue",
						hasIPv4:   true,
						hasIPv6:   true,
					},
				},
			},
			vrfNeighbors: map[string][]string{"": {"192.168.1.1"}},
			vrfASNs:      map[string]uint32{"": 65000, "blue": 65000},
			want: `router bgp 65000
 address-family ipv4 unicast
  neighbor 192.168.1.1 allowas-in origin
 exit-address-family
 address-family l2vpn evpn
  neighbor 192.168.1.1 activate
  neighbor 192.168.1.1 allowas-in origin
  advertise-all-vni
 exit-address-family
exit
!
vrf blue
 vni 2000
exit-vrf
!
router bgp 65000 vrf blue
 address-family l2vpn evpn
  advertise ipv4 unicast
  advertise ipv6 unicast
  route-target import 65000:2000
  route-target export 65000:2000
 exit-address-family
exit
!
`,
		},
		{
			name: "MAC-VRF and IP-VRF combined",
			selected: &selectedNetworks{
				macVRFConfigs: []*vrfConfig{
					{vni: 1000, routeTarget: "65000:1000"},
				},
				ipVRFConfigs: []*ipVRFConfig{
					{
						vrfConfig: vrfConfig{vni: 2000, routeTarget: "65000:2000"},
						vrfName:   "blue",
						hasIPv4:   true,
					},
				},
			},
			vrfNeighbors: map[string][]string{"": {"192.168.1.2", "192.168.1.1"}},
			vrfASNs:      map[string]uint32{"": 65000, "blue": 65000},
			want: `router bgp 65000
 address-family ipv4 unicast
  neighbor 192.168.1.1 allowas-in origin
  neighbor 192.168.1.2 allowas-in origin
 exit-address-family
 address-family l2vpn evpn
  neighbor 192.168.1.1 activate
  neighbor 192.168.1.1 allowas-in origin
  neighbor 192.168.1.2 activate
  neighbor 192.168.1.2 allowas-in origin
  advertise-all-vni
  vni 1000
   route-target import 65000:1000
   route-target export 65000:1000
  exit-vni
 exit-address-family
exit
!
vrf blue
 vni 2000
exit-vrf
!
router bgp 65000 vrf blue
 address-family l2vpn evpn
  advertise ipv4 unicast
  route-target import 65000:2000
  route-target export 65000:2000
 exit-address-family
exit
!
`,
		},
		{
			name: "non-default VRF with neighbors and IP-VRF",
			selected: &selectedNetworks{
				ipVRFConfigs: []*ipVRFConfig{
					{
						vrfConfig: vrfConfig{vni: 3000, routeTarget: "65000:3000"},
						vrfName:   "green",
						hasIPv4:   true,
					},
				},
			},
			vrfNeighbors: map[string][]string{"": {"192.168.1.1"}, "green": {"10.0.0.1"}},
			vrfASNs:      map[string]uint32{"": 65000, "green": 65000},
			want: `router bgp 65000
 address-family ipv4 unicast
  neighbor 192.168.1.1 allowas-in origin
 exit-address-family
 address-family l2vpn evpn
  neighbor 192.168.1.1 activate
  neighbor 192.168.1.1 allowas-in origin
  advertise-all-vni
 exit-address-family
exit
!
vrf green
 vni 3000
exit-vrf
!
router bgp 65000 vrf green
 address-family ipv4 unicast
  neighbor 10.0.0.1 allowas-in origin
 exit-address-family
 address-family l2vpn evpn
  advertise ipv4 unicast
  route-target import 65000:3000
  route-target export 65000:3000
 exit-address-family
exit
!
`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vrfNeighbors := map[string][]frrNeighbor{}
			for vrf, neighbors := range tt.vrfNeighbors {
				for _, raw := range neighbors {
					// the table identifies unnumbered neighbors by interface name
					in := frrtypes.Neighbor{Address: raw}
					if _, err := netip.ParseAddr(raw); err != nil {
						in = frrtypes.Neighbor{Interface: raw}
					}
					neighbor, err := newFRRNeighbor(in)
					if err != nil {
						t.Fatalf("newFRRNeighbor(%q) unexpected error: %v", raw, err)
					}
					vrfNeighbors[vrf] = append(vrfNeighbors[vrf], neighbor)
				}
			}
			got := generateRawConfig(tt.selected, vrfNeighbors, tt.vrfASNs)
			if got != tt.want {
				t.Errorf("generateRawConfig() mismatch\nGot:\n%s\nWant:\n%s", got, tt.want)
			}
		})
	}
}
