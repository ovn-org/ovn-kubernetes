// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package routeadvertisements

import (
	"fmt"
	"strings"
	"testing"

	frrtypes "github.com/metallb/frr-k8s/api/v1beta1"
)

func TestValidateVNI(t *testing.T) {
	for _, vni := range []int32{minVNI, 100, maxVNI} {
		if err := validateVNI(vni); err != nil {
			t.Errorf("validateVNI(%d) unexpected error: %v", vni, err)
		}
	}
	for _, vni := range []int32{-1, 0, maxVNI + 1} {
		if err := validateVNI(vni); err == nil {
			t.Errorf("validateVNI(%d) expected error", vni)
		}
	}
}

func TestNewVRFConfig(t *testing.T) {
	type input struct {
		vni         int32
		routeTarget string
	}
	valid := []input{
		{vni: 100},
		{vni: 100, routeTarget: "65000:100"},
	}
	for _, in := range valid {
		cfg, err := newVRFConfig("MAC-VRF", "red", in.vni, in.routeTarget)
		if err != nil {
			t.Errorf("newVRFConfig(%+v) unexpected error: %v", in, err)
			continue
		}
		if cfg.networkName != "red" || cfg.vni != in.vni || cfg.routeTarget != in.routeTarget {
			t.Errorf("newVRFConfig(%+v) = %+v", in, cfg)
		}
	}

	invalid := []input{
		{vni: 100, routeTarget: "65000:100\n router bgp 1"},
		{vni: 100, routeTarget: "65000:100\r"},
		{vni: 100, routeTarget: "65000:100 auto"},
		{vni: 0, routeTarget: "65000:100"},
		{vni: maxVNI + 1},
	}
	for _, in := range invalid {
		if cfg, err := newVRFConfig("IP-VRF", "red", in.vni, in.routeTarget); err == nil {
			t.Errorf("newVRFConfig(%+v) expected error, got %+v", in, cfg)
		}
	}
}

func TestNewIPVRFConfig(t *testing.T) {
	cfg, err := newIPVRFConfig("red", "red", 100, "65000:100", true, false)
	if err != nil {
		t.Fatalf("newIPVRFConfig() unexpected error: %v", err)
	}
	if cfg.networkName != "red" || cfg.vrfName != "red" || cfg.vni != 100 || cfg.routeTarget != "65000:100" || !cfg.hasIPv4 || cfg.hasIPv6 {
		t.Errorf("newIPVRFConfig() = %+v", cfg)
	}
	for _, vrf := range []string{"", "red\nexit-vrf", "red vrf", "a234567890123456"} {
		if cfg, err := newIPVRFConfig("red", vrf, 100, "", true, false); err == nil {
			t.Errorf("newIPVRFConfig(vrf=%q) expected error, got %+v", vrf, cfg)
		}
	}
	if cfg, err := newIPVRFConfig("red", "red", 100, "65000:100\n", true, false); err == nil {
		t.Errorf("newIPVRFConfig(newline route target) expected error, got %+v", cfg)
	}
}

func TestValidateNetworkVRFName(t *testing.T) {
	if err := validateNetworkVRFName("red", "red"); err != nil {
		t.Errorf("validateNetworkVRFName(red) unexpected error: %v", err)
	}
	for _, vrf := range []string{"", "red\nexit-vrf", "red vrf", "a234567890123456"} {
		if err := validateNetworkVRFName("red", vrf); err == nil {
			t.Errorf("validateNetworkVRFName(%q) expected error", vrf)
		}
	}
}

func TestNewVRFConfigTruncatesQuotedValue(t *testing.T) {
	longRT := "65000:" + strings.Repeat("1", 300) + "\nrouter bgp 1"
	_, err := newVRFConfig("MAC-VRF", "red", 100, longRT)
	if err == nil {
		t.Fatal("newVRFConfig() expected error")
	}
	if strings.Contains(err.Error(), longRT[:maxQuotedValueLength+1]) {
		t.Errorf("newVRFConfig() error quotes more than %d bytes of the value: %v", maxQuotedValueLength, err)
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("%q...", longRT[:maxQuotedValueLength])) {
		t.Errorf("newVRFConfig() error does not quote the truncated value: %v", err)
	}
}

func TestNewFRRNeighbor(t *testing.T) {
	valid := []struct {
		neighbor       frrtypes.Neighbor
		text           string
		isIPv4, isIPv6 bool
	}{
		{neighbor: frrtypes.Neighbor{Address: "192.168.1.1"}, text: "192.168.1.1", isIPv4: true},
		{neighbor: frrtypes.Neighbor{Address: "fd00::1"}, text: "fd00::1", isIPv6: true},
		{neighbor: frrtypes.Neighbor{Address: "fe80::1"}, text: "fe80::1", isIPv6: true},
		{neighbor: frrtypes.Neighbor{Address: "::ffff:192.0.2.1"}, text: "::ffff:192.0.2.1", isIPv4: true},
		{neighbor: frrtypes.Neighbor{Address: "10.0.0.1", Interface: "eth0"}, text: "10.0.0.1", isIPv4: true},
		{neighbor: frrtypes.Neighbor{Interface: "eth0"}, text: "eth0"},
		{neighbor: frrtypes.Neighbor{Interface: "enp4s0f0np0"}, text: "enp4s0f0np0"},
		{neighbor: frrtypes.Neighbor{Interface: "eth0.100"}, text: "eth0.100"},
		{neighbor: frrtypes.Neighbor{Interface: "010.0.0.1"}, text: "010.0.0.1"},
	}
	for _, tt := range valid {
		neighbor, err := newFRRNeighbor(tt.neighbor)
		if err != nil {
			t.Errorf("newFRRNeighbor(%+v) unexpected error: %v", tt.neighbor, err)
			continue
		}
		if neighbor.text != tt.text || neighbor.isIPv4() != tt.isIPv4 || neighbor.isIPv6() != tt.isIPv6 {
			t.Errorf("newFRRNeighbor(%+v) = %+v, isIPv4 %v, isIPv6 %v", tt.neighbor, neighbor, neighbor.isIPv4(), neighbor.isIPv6())
		}
	}

	invalid := []frrtypes.Neighbor{
		{},
		{Address: "fe80::1%eth0"},
		{Address: "010.0.0.1"},
		{Address: "eth0"},
		{Address: "192.168.1.1 remote-as 1"},
		{Address: "192.168.1.1\nrouter bgp 1"},
		{Interface: "eth0\nrouter bgp 1"},
		{Interface: "eth0 activate"},
		{Interface: "eth0/1"},
		{Interface: "a234567890123456"},
	}
	for _, in := range invalid {
		if neighbor, err := newFRRNeighbor(in); err == nil {
			t.Errorf("newFRRNeighbor(%+v) expected error, got %+v", in, neighbor)
		}
	}
}

func TestValidateRouteTarget(t *testing.T) {
	valid := []string{
		"65000:100",         // 2-byte ASN, 4-byte local administrator
		"65535:4294967295",  // 2-byte ASN, maximum local administrator
		"65536:100",         // 4-byte ASN, 2-byte local administrator
		"4294967295:65535",  // maximum 4-byte ASN, maximum local administrator
		"10.0.0.1:100",      // IPv4, 2-byte local administrator
		"192.168.1.1:65535", // IPv4, maximum local administrator
		"*:100",             // wildcard, 2-byte local administrator
		"*:4294967295",      // wildcard, 4-byte local administrator
	}
	for _, rt := range valid {
		if err := validateRouteTarget(rt); err != nil {
			t.Errorf("validateRouteTarget(%q) unexpected error: %v", rt, err)
		}
	}

	invalid := []string{
		"",
		"65000",
		"65000:",
		":100",
		"65000:100:1",
		"65000:1a",
		"a:100",
		"65000: 100",
		"65000:100\n",
		"65000:100 ",
		"65000:100 auto",
		"4294967296:1",
		"65536:65536",
		"10.0.0.1:65536",
		"10.0.0.1.1:1",
		"010.0.0.1:100",
		"*:4294967296",
		"1111111111:1111111111",  // 21 characters, local administrator overflows 2 bytes
		"111111111:11111111111",  // 21 characters, local administrator overflows 4 bytes
		"65000:0000000000000001", // 22 characters
	}
	for _, rt := range invalid {
		if err := validateRouteTarget(rt); err == nil {
			t.Errorf("validateRouteTarget(%q) expected error", rt)
		}
	}
}

func TestValidateInterfaceName(t *testing.T) {
	valid := []string{"eth0", "enp4s0f0np0", "br-ex", "vrf_red.100", "a23456789012345"}
	for _, name := range valid {
		if err := validateInterfaceName(name); err != nil {
			t.Errorf("validateInterfaceName(%q) unexpected error: %v", name, err)
		}
	}

	invalid := []string{"", ".", "..", "eth 0", "eth0\n", "eth0\r", "eth0\t", "eth0/1", "eth0:1", "a234567890123456", "eth\x000"}
	for _, name := range invalid {
		if err := validateInterfaceName(name); err == nil {
			t.Errorf("validateInterfaceName(%q) expected error", name)
		}
	}
}
