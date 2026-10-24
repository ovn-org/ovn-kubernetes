// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package netlinkdevicemanager

import (
	"fmt"
	"net"
	"reflect"
	"strings"

	"github.com/vishvananda/netlink"

	"k8s.io/utils/ptr"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Link", func() {

	DescribeTable("isOurDevice ownership check",
		func(link netlink.Link, expected bool) {
			Expect(isOurDevice(link)).To(Equal(expected))
		},
		Entry("our alias prefix (bridge)",
			&netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Alias: "ovn-k8s-ndm:bridge:br0"}}, true),
		Entry("our alias prefix (vxlan)",
			&netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Alias: "ovn-k8s-ndm:vxlan:vxlan0"}}, true),
		Entry("empty alias",
			&netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Alias: ""}}, false),
		Entry("foreign alias",
			&netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Alias: "external-system:some-device"}}, false),
		Entry("partial prefix",
			&netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Alias: "ovn-k8s:bridge:br0"}}, false),
	)

	DescribeTable("linkImmutableFieldsEqual",
		func(existing netlink.Link, cfg *DeviceConfig, expectCriticalMismatch bool) {
			m := &managedDeviceConfig{DeviceConfig: *cfg}
			normalizedState := normalizeLinkState(m.Link, existing)
			immutableEqual := linkImmutableFieldsEqual(normalizedState, m.Link)
			Expect(!immutableEqual).To(Equal(expectCriticalMismatch))
		},
		// VRF
		Entry("VRF table ID mismatch",
			&netlink.Vrf{LinkAttrs: netlink.LinkAttrs{Name: "vrf0"}, Table: 100},
			&DeviceConfig{Link: &netlink.Vrf{LinkAttrs: netlink.LinkAttrs{Name: "vrf0"}, Table: 200}},
			true),
		Entry("VRF matching table ID",
			&netlink.Vrf{LinkAttrs: netlink.LinkAttrs{Name: "vrf0"}, Table: 100},
			&DeviceConfig{Link: &netlink.Vrf{LinkAttrs: netlink.LinkAttrs{Name: "vrf0"}, Table: 100}},
			false),
		// VXLAN basic
		Entry("VXLAN VNI mismatch",
			&netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, VxlanId: 100},
			&DeviceConfig{Link: &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, VxlanId: 200}},
			true),
		Entry("VXLAN src addr mismatch",
			&netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, VxlanId: 100, SrcAddr: net.ParseIP("10.0.0.1")},
			&DeviceConfig{Link: &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, VxlanId: 100, SrcAddr: net.ParseIP("10.0.0.2")}},
			true),
		Entry("VXLAN port mismatch",
			&netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, VxlanId: 100, Port: 4789},
			&DeviceConfig{Link: &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, VxlanId: 100, Port: 4790}},
			true),
		Entry("VXLAN matching critical attrs",
			&netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, VxlanId: 100, SrcAddr: net.ParseIP("10.0.0.1"), Port: 4789},
			&DeviceConfig{Link: &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, VxlanId: 100, SrcAddr: net.ParseIP("10.0.0.1"), Port: 4789}},
			false),
		// VXLAN EVPN (FlowBased / VniFilter)
		Entry("VXLAN FlowBased true-to-false downgrade",
			&netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, VxlanId: 0, FlowBased: true},
			&DeviceConfig{Link: &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, VxlanId: 0, FlowBased: false}},
			true),
		Entry("VXLAN FlowBased false-to-true upgrade",
			&netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, VxlanId: 100, FlowBased: false},
			&DeviceConfig{Link: &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, VxlanId: 100, FlowBased: true}},
			true),
		Entry("VXLAN VniFilter false-to-true",
			&netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, VxlanId: 0, FlowBased: true, VniFilter: false},
			&DeviceConfig{Link: &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, VxlanId: 0, FlowBased: true, VniFilter: true}},
			true),
		Entry("VXLAN matching external with vnifilter",
			&netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, VxlanId: 0, FlowBased: true, VniFilter: true, SrcAddr: net.ParseIP("10.0.0.1"), Port: 4789},
			&DeviceConfig{Link: &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, VxlanId: 0, FlowBased: true, VniFilter: true, SrcAddr: net.ParseIP("10.0.0.1"), Port: 4789}},
			false),
		// VLAN
		Entry("VLAN ID mismatch",
			&netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "vlan100"}, VlanId: 100},
			&DeviceConfig{Link: &netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "vlan100"}, VlanId: 200}},
			true),
		Entry("VLAN protocol mismatch",
			&netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "br0.10"}, VlanId: 10, VlanProtocol: netlink.VLAN_PROTOCOL_8021Q},
			&DeviceConfig{Link: &netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "br0.10"}, VlanId: 10, VlanProtocol: netlink.VLAN_PROTOCOL_8021AD}},
			true),
		Entry("VLAN parent index mismatch",
			&netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "br0.10", ParentIndex: 5}, VlanId: 10},
			&DeviceConfig{Link: &netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "br0.10", ParentIndex: 6}, VlanId: 10}},
			true),
		Entry("VLAN matching configuration",
			&netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "br0.10", ParentIndex: 5}, VlanId: 10, VlanProtocol: netlink.VLAN_PROTOCOL_8021Q},
			&DeviceConfig{Link: &netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "br0.10", ParentIndex: 5}, VlanId: 10, VlanProtocol: netlink.VLAN_PROTOCOL_8021Q}},
			false),
		Entry("VLAN HardwareAddr mismatch",
			&netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "br0.10", ParentIndex: 5,
				HardwareAddr: net.HardwareAddr{0x00, 0x11, 0x22, 0x33, 0x44, 0x55}}, VlanId: 10},
			&DeviceConfig{Link: &netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "br0.10", ParentIndex: 5,
				HardwareAddr: net.HardwareAddr{0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF}}, VlanId: 10}},
			true),
		Entry("VLAN nil HardwareAddr in desired (not critical)",
			&netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "br0.10", ParentIndex: 5,
				HardwareAddr: net.HardwareAddr{0x00, 0x11, 0x22, 0x33, 0x44, 0x55}}, VlanId: 10},
			&DeviceConfig{Link: &netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "br0.10", ParentIndex: 5}, VlanId: 10}},
			false),
		Entry("VLAN matching HardwareAddr",
			&netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "br0.10", ParentIndex: 5,
				HardwareAddr: net.HardwareAddr{0x00, 0x11, 0x22, 0x33, 0x44, 0x55}}, VlanId: 10},
			&DeviceConfig{Link: &netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "br0.10", ParentIndex: 5,
				HardwareAddr: net.HardwareAddr{0x00, 0x11, 0x22, 0x33, 0x44, 0x55}}, VlanId: 10}},
			false),
		// Bridge (SVD)
		Entry("bridge VlanDefaultPVID mismatch",
			&netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}, VlanDefaultPVID: ptr.To[uint16](1)},
			&DeviceConfig{Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}, VlanDefaultPVID: ptr.To[uint16](0)}},
			false),
		Entry("bridge nil VlanDefaultPVID in desired",
			&netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}, VlanDefaultPVID: ptr.To[uint16](1)},
			&DeviceConfig{Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}, VlanDefaultPVID: nil}},
			false),
		Entry("bridge matching VlanDefaultPVID",
			&netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}, VlanDefaultPVID: ptr.To[uint16](0)},
			&DeviceConfig{Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}, VlanDefaultPVID: ptr.To[uint16](0)}},
			false),
		Entry("bridge VlanFiltering mismatch",
			&netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}, VlanFiltering: ptr.To(false)},
			&DeviceConfig{Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}, VlanFiltering: ptr.To(true)}},
			false),
		Entry("bridge nil VlanFiltering in desired (not critical)",
			&netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}, VlanFiltering: ptr.To(true)},
			&DeviceConfig{Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}, VlanFiltering: nil}},
			false),
		Entry("bridge matching VlanFiltering",
			&netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}, VlanFiltering: ptr.To(true)},
			&DeviceConfig{Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}, VlanFiltering: ptr.To(true)}},
			false),
		// Generic
		Entry("type mismatch",
			&netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "dev0"}},
			&DeviceConfig{Link: &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "dev0"}}},
			true),
	)

	DescribeTable("linkMutableFieldsEqual",
		func(current netlink.Link, cfg *DeviceConfig, expected bool) {
			managed, err := newManagedDeviceConfig(*cfg)
			Expect(err).NotTo(HaveOccurred())
			normalizedState := normalizeLinkState(managed.Link, current)
			Expect(linkMutableFieldsEqual(normalizedState, managed.Link)).To(Equal(expected))
		},
		Entry("all attributes match",
			&netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0", Index: 1, Alias: "ovn-k8s-ndm:bridge:br0", MTU: 1500}},
			&DeviceConfig{Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0", MTU: 1500}}},
			true),
		Entry("VXLAN attributes match",
			&netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0", Index: 1, Alias: "ovn-k8s-ndm:vxlan:vxlan0"}, Learning: false},
			&DeviceConfig{Link: &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, Learning: false}},
			true),
		Entry("alias differs",
			&netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0", Index: 1, Alias: ""}},
			&DeviceConfig{Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}}},
			false),
		Entry("MTU differs",
			&netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0", Index: 1, Alias: "ovn-k8s-ndm:bridge:br0", MTU: 1500}},
			&DeviceConfig{Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0", MTU: 9000}}},
			false),
		Entry("HardwareAddr differs",
			&netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0", Index: 1, Alias: "ovn-k8s-ndm:bridge:br0",
				HardwareAddr: net.HardwareAddr{0x00, 0x11, 0x22, 0x33, 0x44, 0x55}}},
			&DeviceConfig{Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0",
				HardwareAddr: net.HardwareAddr{0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF}}}},
			false),
		Entry("VXLAN Learning differs",
			&netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0", Index: 1, Alias: "ovn-k8s-ndm:vxlan:vxlan0"}, Learning: true},
			&DeviceConfig{Link: &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, Learning: false}},
			false),
		Entry("Bridge VlanFiltering differs",
			&netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0", Index: 1, Alias: "ovn-k8s-ndm:bridge:br0"}, VlanFiltering: ptr.To(false)},
			&DeviceConfig{Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}, VlanFiltering: ptr.To(true)}},
			false),
	)

	DescribeTable("addressesEqual",
		func(a, b []netlink.Addr, expected bool) {
			Expect(addressesEqual(a, b)).To(Equal(expected))
		},
		Entry("both nil", nil, nil, true),
		Entry("nil vs empty", nil, []netlink.Addr{}, false),
		Entry("empty vs nil", []netlink.Addr{}, nil, false),
		Entry("both empty", []netlink.Addr{}, []netlink.Addr{}, true),
		Entry("same addresses",
			[]netlink.Addr{{IPNet: mustParseIPNet("10.0.0.1/32")}},
			[]netlink.Addr{{IPNet: mustParseIPNet("10.0.0.1/32")}},
			true),
		Entry("different addresses",
			[]netlink.Addr{{IPNet: mustParseIPNet("10.0.0.1/32")}},
			[]netlink.Addr{{IPNet: mustParseIPNet("10.0.0.2/32")}},
			false),
		Entry("different lengths",
			[]netlink.Addr{{IPNet: mustParseIPNet("10.0.0.1/32")}, {IPNet: mustParseIPNet("10.0.0.2/32")}},
			[]netlink.Addr{{IPNet: mustParseIPNet("10.0.0.1/32")}},
			false),
		Entry("ignores order",
			[]netlink.Addr{{IPNet: mustParseIPNet("10.0.0.1/32")}, {IPNet: mustParseIPNet("10.0.0.2/32")}},
			[]netlink.Addr{{IPNet: mustParseIPNet("10.0.0.2/32")}, {IPNet: mustParseIPNet("10.0.0.1/32")}},
			true),
		Entry("same IP different prefix length",
			[]netlink.Addr{{IPNet: mustParseIPNet("10.0.0.1/32")}},
			[]netlink.Addr{{IPNet: mustParseIPNet("10.0.0.1/24")}},
			false),
		Entry("compares by IPNet string, ignoring other fields",
			[]netlink.Addr{{IPNet: mustParseIPNet("10.0.0.1/32"), Flags: 0}},
			[]netlink.Addr{{IPNet: mustParseIPNet("10.0.0.1/32"), Flags: 128}},
			true),
	)

	DescribeTable("addrUpdateRequiresReconcile",
		func(desired []netlink.Addr, update *netlink.AddrUpdate, expected bool) {
			Expect(addrUpdateRequiresReconcile(desired, update)).To(Equal(expected))
		},
		Entry("nil desired — no address management",
			nil, &netlink.AddrUpdate{LinkAddress: *mustParseIPNet("10.0.0.1/32"), NewAddr: true}, false),
		Entry("nil desired — removal also skipped",
			nil, &netlink.AddrUpdate{LinkAddress: *mustParseIPNet("10.0.0.1/32"), NewAddr: false}, false),
		Entry("desired addr added — converged, skip",
			[]netlink.Addr{{IPNet: mustParseIPNet("10.0.0.1/32")}},
			&netlink.AddrUpdate{LinkAddress: *mustParseIPNet("10.0.0.1/32"), NewAddr: true}, false),
		Entry("desired addr removed — diverged, reconcile",
			[]netlink.Addr{{IPNet: mustParseIPNet("10.0.0.1/32")}},
			&netlink.AddrUpdate{LinkAddress: *mustParseIPNet("10.0.0.1/32"), NewAddr: false}, true),
		Entry("unexpected addr added — diverged, reconcile",
			[]netlink.Addr{{IPNet: mustParseIPNet("10.0.0.1/32")}},
			&netlink.AddrUpdate{LinkAddress: *mustParseIPNet("10.0.0.99/32"), NewAddr: true}, true),
		Entry("unexpected addr removed — converging, skip",
			[]netlink.Addr{{IPNet: mustParseIPNet("10.0.0.1/32")}},
			&netlink.AddrUpdate{LinkAddress: *mustParseIPNet("10.0.0.99/32"), NewAddr: false}, false),
		Entry("empty desired — any add triggers reconcile",
			[]netlink.Addr{},
			&netlink.AddrUpdate{LinkAddress: *mustParseIPNet("10.0.0.1/32"), NewAddr: true}, true),
		Entry("empty desired — any removal is a no-op",
			[]netlink.Addr{},
			&netlink.AddrUpdate{LinkAddress: *mustParseIPNet("10.0.0.1/32"), NewAddr: false}, false),
	)

	// Managed field coverage invariants
	//
	// These tests enforce that cloneLink, normalizeLinkState,
	// linkMutableFieldsEqual, and linkImmutableFieldsEqual
	// cover every managed field. DeviceConfig.cloneNormalize centralizes
	// normalization and alias computation.
	// Adding a field to one without the others causes a test failure.

	Describe("managed field exhaustiveness", func() {
		type managedField struct {
			name    string
			current netlink.Link
			desired *DeviceConfig
		}

		allFields := []managedField{
			// Common mutable (LinkAttrs) — tested via Bridge
			{name: "Alias",
				current: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0", Alias: "old-alias"}},
				desired: &DeviceConfig{Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0", Alias: "ovn-k8s-ndm:bridge:br0"}}}},
			{name: "MTU",
				current: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0", MTU: 1500}},
				desired: &DeviceConfig{Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0", MTU: 9000}}}},
			{name: "HardwareAddr",
				current: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0",
					HardwareAddr: net.HardwareAddr{0x00, 0x11, 0x22, 0x33, 0x44, 0x55}}},
				desired: &DeviceConfig{Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0",
					HardwareAddr: net.HardwareAddr{0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF}}}}},

			// VXLAN
			{name: "VXLAN/Learning",
				current: &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, Learning: true},
				desired: &DeviceConfig{Link: &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, Learning: false}}},
			{name: "VXLAN/VxlanId",
				current: &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, VxlanId: 100},
				desired: &DeviceConfig{Link: &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, VxlanId: 200}}},
			{name: "VXLAN/SrcAddr",
				current: &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, SrcAddr: net.ParseIP("10.0.0.1")},
				desired: &DeviceConfig{Link: &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, SrcAddr: net.ParseIP("10.0.0.2")}}},
			{name: "VXLAN/Port",
				current: &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, Port: 4789},
				desired: &DeviceConfig{Link: &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, Port: 4790}}},
			{name: "VXLAN/FlowBased",
				current: &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, FlowBased: true},
				desired: &DeviceConfig{Link: &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, FlowBased: false}}},
			{name: "VXLAN/VniFilter",
				current: &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, VniFilter: true},
				desired: &DeviceConfig{Link: &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, VniFilter: false}}},

			// Bridge
			{name: "Bridge/VlanFiltering",
				current: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}, VlanFiltering: ptr.To(false)},
				desired: &DeviceConfig{Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}, VlanFiltering: ptr.To(true)}}},
			{name: "Bridge/VlanDefaultPVID",
				current: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}, VlanDefaultPVID: ptr.To[uint16](1)},
				desired: &DeviceConfig{Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}, VlanDefaultPVID: ptr.To[uint16](0)}}},

			// VRF
			{name: "VRF/Table",
				current: &netlink.Vrf{LinkAttrs: netlink.LinkAttrs{Name: "vrf0"}, Table: 100},
				desired: &DeviceConfig{Link: &netlink.Vrf{LinkAttrs: netlink.LinkAttrs{Name: "vrf0"}, Table: 200}}},

			// VLAN
			{name: "VLAN/VlanId",
				current: &netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "vlan0"}, VlanId: 100},
				desired: &DeviceConfig{Link: &netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "vlan0"}, VlanId: 200}}},
			{name: "VLAN/VlanProtocol",
				current: &netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "vlan0"}, VlanId: 10, VlanProtocol: netlink.VLAN_PROTOCOL_8021Q},
				desired: &DeviceConfig{Link: &netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "vlan0"}, VlanId: 10, VlanProtocol: netlink.VLAN_PROTOCOL_8021AD}}},
			{name: "VLAN/HardwareAddr",
				current: &netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "vlan0", ParentIndex: 5,
					HardwareAddr: net.HardwareAddr{0x00, 0x11, 0x22, 0x33, 0x44, 0x55}}, VlanId: 10},
				desired: &DeviceConfig{Link: &netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "vlan0", ParentIndex: 5,
					HardwareAddr: net.HardwareAddr{0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF}}, VlanId: 10}}},
		}

		It("each field is covered by comparison functions and cloneLink", func() {
			for _, f := range allFields {
				m := &managedDeviceConfig{DeviceConfig: *f.desired}
				normalizedState := normalizeLinkState(m.Link, f.current)
				mutableEqual := linkMutableFieldsEqual(normalizedState, m.Link)
				immutableEqual := linkImmutableFieldsEqual(normalizedState, m.Link)

				Expect(!mutableEqual || !immutableEqual).To(BeTrue(),
					"field %s is detected by neither mutable nor immutable", f.name)

				// prepareLinkForModify delegates to cloneLink
				result := prepareLinkForModify(f.current, m)
				normalizedResult := normalizeLinkState(m.Link, result)
				Expect(linkMutableFieldsEqual(normalizedResult, m.Link)).To(BeTrue(),
					"prepareLinkForModify does not carry field %s", f.name)

				// cloneLink must carry every field that comparison functions check
				managed := cloneLink(m.Link)
				Expect(linkMutableFieldsEqual(managed, m.Link)).To(BeTrue(),
					"cloneLink does not carry mutable field %s", f.name)
				Expect(linkImmutableFieldsEqual(managed, m.Link)).To(BeTrue(),
					"cloneLink does not carry immutable field %s", f.name)
			}
		})
	})

	Describe("upstream field audit", func() {
		// These tests use reflection to detect new fields added to vendored netlink
		// types. When a new field appears, the test fails and the developer must
		// classify it as managed (mutable/immutable) or explicitly ignored.

		vxlanManaged := map[string]bool{
			"VxlanId": true, "SrcAddr": true, "Port": true,
			"FlowBased": true, "VniFilter": true, "Learning": true,
		}
		vxlanIgnored := map[string]string{
			"VtepDevIndex":   "resolved by kernel at creation time",
			"Group":          "multicast group, not used by NDM",
			"TTL":            "not managed by NDM",
			"TOS":            "not managed by NDM",
			"Proxy":          "not managed by NDM",
			"RSC":            "not managed by NDM",
			"L2miss":         "not managed by NDM",
			"L3miss":         "not managed by NDM",
			"UDPCSum":        "not managed by NDM",
			"UDP6ZeroCSumTx": "not managed by NDM",
			"UDP6ZeroCSumRx": "not managed by NDM",
			"NoAge":          "not managed by NDM",
			"GBP":            "not managed by NDM",
			"Age":            "runtime state, not a config knob",
			"Limit":          "not managed by NDM",
			"PortLow":        "not managed by NDM",
			"PortHigh":       "not managed by NDM",
		}

		bridgeManaged := map[string]bool{
			"VlanFiltering": true, "VlanDefaultPVID": true,
		}
		bridgeIgnored := map[string]string{
			"MulticastSnooping": "not managed by NDM",
			"AgeingTime":        "not managed by NDM",
			"HelloTime":         "not managed by NDM",
			"GroupFwdMask":      "not managed by NDM",
		}

		vlanManaged := map[string]bool{
			"VlanId": true, "VlanProtocol": true,
		}
		vlanIgnored := map[string]string{
			"IngressQosMap": "not managed by NDM",
			"EgressQosMap":  "not managed by NDM",
			"ReorderHdr":    "not managed by NDM",
			"Gvrp":          "not managed by NDM",
			"LooseBinding":  "not managed by NDM",
			"Mvrp":          "not managed by NDM",
			"BridgeBinding": "not managed by NDM",
		}

		vrfManaged := map[string]bool{
			"Table": true,
		}

		auditLinkType := func(name string, typ reflect.Type, managed map[string]bool, ignored map[string]string) {
			for i := 0; i < typ.NumField(); i++ {
				field := typ.Field(i)
				if field.Name == "LinkAttrs" {
					continue
				}
				_, isManaged := managed[field.Name]
				_, isIgnored := ignored[field.Name]
				Expect(isManaged || isIgnored).To(BeTrue(),
					"%s field %q is not accounted for — add to managed or ignored with justification", name, field.Name)
			}
		}

		It("all Vxlan fields are accounted for", func() {
			auditLinkType("Vxlan", reflect.TypeFor[netlink.Vxlan](), vxlanManaged, vxlanIgnored)
		})

		It("all Bridge fields are accounted for", func() {
			auditLinkType("Bridge", reflect.TypeFor[netlink.Bridge](), bridgeManaged, bridgeIgnored)
		})

		It("all Vlan fields are accounted for", func() {
			auditLinkType("Vlan", reflect.TypeFor[netlink.Vlan](), vlanManaged, vlanIgnored)
		})

		It("all Vrf fields are accounted for", func() {
			auditLinkType("Vrf", reflect.TypeFor[netlink.Vrf](), vrfManaged, map[string]string{})
		})

		// setFieldNonZero sets a struct field to a non-zero value using reflection.
		// Supports int, bool, net.IP, *bool, *uint32, *uint16, map[uint32]uint32.
		setFieldNonZero := func(link netlink.Link, fieldName string) {
			v := reflect.ValueOf(link).Elem()
			f := v.FieldByName(fieldName)
			ExpectWithOffset(1, f.IsValid()).To(BeTrue(), "field %s not found on %T", fieldName, link)
			switch f.Kind() {
			case reflect.Int:
				f.SetInt(42)
			case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
				f.SetUint(42)
			case reflect.Bool:
				f.SetBool(true)
			case reflect.Slice:
				if f.Type() == reflect.TypeOf(net.IP{}) {
					f.Set(reflect.ValueOf(net.ParseIP("1.2.3.4")))
				} else {
					f.Set(reflect.MakeSlice(f.Type(), 1, 1))
				}
			case reflect.Pointer:
				f.Set(reflect.New(f.Type().Elem()))
			case reflect.Map:
				f.Set(reflect.MakeMap(f.Type()))
			default:
				Fail(fmt.Sprintf("setFieldNonZero: unsupported kind %s for field %s", f.Kind(), fieldName))
			}
		}

		It("managed maps match cloneLink", func() {
			allManaged := map[reflect.Type]map[string]bool{
				reflect.TypeFor[netlink.Vxlan]():  vxlanManaged,
				reflect.TypeFor[netlink.Bridge](): bridgeManaged,
				reflect.TypeFor[netlink.Vlan]():   vlanManaged,
				reflect.TypeFor[netlink.Vrf]():    vrfManaged,
				reflect.TypeFor[netlink.Dummy]():  {},
			}
			for typ, managed := range allManaged {
				link := reflect.New(typ).Interface().(netlink.Link)
				link.Attrs().Name = "test0"

				v := reflect.ValueOf(link).Elem()
				for i := 0; i < v.NumField(); i++ {
					f := v.Type().Field(i)
					if !f.IsExported() || f.Name == "LinkAttrs" {
						continue
					}
					setFieldNonZero(link, f.Name)
				}

				result := cloneLink(link)
				rv := reflect.ValueOf(result).Elem()

				for i := 0; i < rv.NumField(); i++ {
					f := rv.Type().Field(i)
					if !f.IsExported() || f.Name == "LinkAttrs" {
						continue
					}
					if managed[f.Name] {
						Expect(rv.Field(i).IsZero()).To(BeFalse(),
							"managed map lists %s.%s but cloneLink zeroes it", typ.Name(), f.Name)
					} else {
						Expect(rv.Field(i).IsZero()).To(BeTrue(),
							"managed map does not list %s.%s but cloneLink preserves it", typ.Name(), f.Name)
					}
				}
			}
		})

		// Verify validateSupportedFields rejects exactly the ignored fields.

		It("validateSupportedFields rejects each ignored Vxlan field", func() {
			for fieldName := range vxlanIgnored {
				link := &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}}
				setFieldNonZero(link, fieldName)
				err := validateSupportedFields(link)
				Expect(err).To(HaveOccurred(), "field %s should be rejected", fieldName)
				Expect(err.Error()).To(ContainSubstring(fieldName))
			}
		})

		It("validateSupportedFields rejects each ignored Bridge field", func() {
			for fieldName := range bridgeIgnored {
				link := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}}
				setFieldNonZero(link, fieldName)
				err := validateSupportedFields(link)
				Expect(err).To(HaveOccurred(), "field %s should be rejected", fieldName)
				Expect(err.Error()).To(ContainSubstring(fieldName))
			}
		})

		It("validateSupportedFields rejects each ignored Vlan field", func() {
			for fieldName := range vlanIgnored {
				link := &netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "vlan0"}}
				setFieldNonZero(link, fieldName)
				err := validateSupportedFields(link)
				Expect(err).To(HaveOccurred(), "field %s should be rejected", fieldName)
				Expect(err.Error()).To(ContainSubstring(fieldName))
			}
		})

		It("validateSupportedFields accepts managed-only configs", func() {
			Expect(validateSupportedFields(&netlink.Vxlan{
				LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"},
				VxlanId:   100, SrcAddr: net.ParseIP("10.0.0.1"), Port: 4789,
				FlowBased: true, VniFilter: true, Learning: false,
			})).To(Succeed())
			Expect(validateSupportedFields(&netlink.Bridge{
				LinkAttrs:       netlink.LinkAttrs{Name: "br0"},
				VlanFiltering:   ptr.To(true),
				VlanDefaultPVID: ptr.To[uint16](0),
			})).To(Succeed())
			Expect(validateSupportedFields(&netlink.Vlan{
				LinkAttrs: netlink.LinkAttrs{Name: "vlan0"},
				VlanId:    100, VlanProtocol: netlink.VLAN_PROTOCOL_8021Q,
			})).To(Succeed())
			Expect(validateSupportedFields(&netlink.Vrf{
				LinkAttrs: netlink.LinkAttrs{Name: "vrf0"},
				Table:     100,
			})).To(Succeed())
			Expect(validateSupportedFields(&netlink.Dummy{
				LinkAttrs: netlink.LinkAttrs{Name: "dummy0"},
			})).To(Succeed())
		})
	})

	DescribeTable("isLinkLocalAddress",
		func(ip net.IP, expected bool) {
			Expect(isLinkLocalAddress(ip)).To(Equal(expected))
		},
		Entry("IPv6 link-local", net.ParseIP("fe80::1"), true),
		Entry("IPv4 link-local (169.254.x.x)", net.ParseIP("169.254.1.1"), true),
		Entry("regular IPv6", net.ParseIP("2001:db8::1"), false),
		Entry("regular IPv4", net.ParseIP("10.0.0.1"), false),
		Entry("nil", nil, false),
	)

	DescribeTable("validateInterfaceName",
		func(name string, valid bool) {
			err := validateInterfaceName(name, "device")
			if valid {
				Expect(err).NotTo(HaveOccurred())
			} else {
				Expect(err).To(HaveOccurred())
			}
		},
		Entry("empty name", "", false),
		Entry("regular name", "br0", true),
		Entry("name at the length limit (IFNAMSIZ-1)", strings.Repeat("a", types.MaxInterfaceNameLength), true),
		Entry("name one character over the length limit", strings.Repeat("a", types.MaxInterfaceNameLength+1), false),
		Entry("reserved name", ".", false),
		Entry("reserved name (dot dot)", "..", false),
		Entry("name with a slash", "br/0", false),
		Entry("name with an embedded NUL", "br\x000", false),
		Entry("name with whitespace", "br 0", false),
		Entry("name with a tab", "br\t0", false),
		Entry("name with a newline", "br\n0", false),
	)

})
