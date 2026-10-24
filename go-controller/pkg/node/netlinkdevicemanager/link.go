// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package netlinkdevicemanager

import (
	"bytes"
	"fmt"
	"net"
	"reflect"
	"slices"
	"strings"

	"github.com/vishvananda/netlink"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/ptr"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
)

// managedAliasPrefix is the prefix used in IFLA_IFALIAS to mark devices managed by this controller.
// This allows safe cleanup: only delete devices with this prefix.
// Format: "ovn-k8s-ndm:<type>:<name>" for debugging and collision avoidance.
const managedAliasPrefix = "ovn-k8s-ndm:"

const maxVNI = 1<<24 - 1 // 16777215
const maxVID = 4094

// isOurDevice returns true only if the device has our alias prefix.
// This is the single source of truth for ownership:
//   - Empty alias = unknown ownership, NOT ours (could be human-created or other automation)
//   - Foreign alias = definitely NOT ours
//   - Our prefix = ours, safe to modify/delete
func isOurDevice(link netlink.Link) bool {
	return strings.HasPrefix(link.Attrs().Alias, managedAliasPrefix)
}

// checkOwnership returns nil if the device is ours, or a notOwnedError explaining why not.
func checkOwnership(link netlink.Link) error {
	if isOurDevice(link) {
		return nil
	}
	return &notOwnedError{
		deviceName: link.Attrs().Name,
		reason:     fmt.Sprintf("alias does not match managed prefix (alias=%q)", link.Attrs().Alias),
	}
}

// cloneLink deep copies a Link containing only the fields NDM manages.
// This is the source of truth for which fields are managed per link type.
//
// FIELD CONTRACT: Every field included here must also be covered by
// normalizeLinkState, linkMutableFieldsEqual, and
// linkImmutableFieldsEqual.
// NOTE: Currently used also for cases where shallow copy is sufficient
// (e.g. when a netlink event is received), when this method becomes
// expensive consider reviewing those cases to use a shallow copy.
func cloneLink(link netlink.Link) netlink.Link {
	a := link.Attrs()
	base := netlink.LinkAttrs{
		Name:         a.Name,
		Alias:        a.Alias,
		MTU:          a.MTU,
		HardwareAddr: slices.Clone(a.HardwareAddr),
	}
	switch v := link.(type) {
	case *netlink.Vxlan:
		return &netlink.Vxlan{
			LinkAttrs: base,
			VxlanId:   v.VxlanId,
			SrcAddr:   slices.Clone(v.SrcAddr),
			Port:      v.Port,
			FlowBased: v.FlowBased,
			VniFilter: v.VniFilter,
			Learning:  v.Learning,
		}
	case *netlink.Bridge:
		b := &netlink.Bridge{LinkAttrs: base}
		if v.VlanFiltering != nil {
			b.VlanFiltering = ptr.To(*v.VlanFiltering)
		}
		if v.VlanDefaultPVID != nil {
			b.VlanDefaultPVID = ptr.To(*v.VlanDefaultPVID)
		}
		return b
	case *netlink.Vlan:
		vlanBase := base
		vlanBase.ParentIndex = a.ParentIndex
		return &netlink.Vlan{
			LinkAttrs:    vlanBase,
			VlanId:       v.VlanId,
			VlanProtocol: v.VlanProtocol,
		}
	case *netlink.Vrf:
		return &netlink.Vrf{
			LinkAttrs: base,
			Table:     v.Table,
		}
	case *netlink.Dummy:
		return &netlink.Dummy{LinkAttrs: base}
	default:
		panic(fmt.Sprintf("BUG: unsupported device type %T", link))
	}
}

// normalizeLinkState normalizes a kernel link state against the desired DeviceConfig's
// "care mask".
// The result is a new link with the managed fields only and with zeroed fields
// where the config has a "don't care" zero values. This makes the comparison between
// the kernel state and the desired DeviceConfig becomes a simple equality.
func normalizeLinkState(desired netlink.Link, state netlink.Link) netlink.Link {
	result := cloneLink(state)
	if desired.Type() != state.Type() {
		return result
	}
	desAttrs := desired.Attrs()
	resAttrs := result.Attrs()

	if desAttrs.Alias == "" {
		resAttrs.Alias = ""
	}
	if desAttrs.MTU == 0 {
		resAttrs.MTU = 0
	}
	if len(desAttrs.HardwareAddr) == 0 {
		resAttrs.HardwareAddr = nil
	}

	switch des := desired.(type) {
	case *netlink.Vxlan:
		r := result.(*netlink.Vxlan)
		if des.SrcAddr == nil {
			r.SrcAddr = nil
		}
		if des.Port == 0 {
			r.Port = 0
		}
	case *netlink.Bridge:
		r := result.(*netlink.Bridge)
		if des.VlanFiltering == nil {
			r.VlanFiltering = nil
		}
		if des.VlanDefaultPVID == nil {
			r.VlanDefaultPVID = nil
		}
	case *netlink.Vlan:
		r := result.(*netlink.Vlan)
		if des.Attrs().ParentIndex == 0 {
			r.LinkAttrs.ParentIndex = 0
		}
		if des.VlanProtocol == 0 {
			r.VlanProtocol = 0
		}
		if len(des.Attrs().HardwareAddr) == 0 {
			r.LinkAttrs.HardwareAddr = nil
		}
	case *netlink.Vrf:
		// No conditional fields — Table is always compared
	case *netlink.Dummy:
		// No type-specific fields
	}

	return result
}

// linkEqual compares all managed link fields (mutable + immutable).
// Both sides must be normalized before calling.
func linkEqual(l, r netlink.Link) bool {
	return linkMutableFieldsEqual(l, r) &&
		linkImmutableFieldsEqual(l, r)
}

// linkMutableFieldsEqual compares mutable managed link fields.
func linkMutableFieldsEqual(l, r netlink.Link) bool {
	if l.Type() != r.Type() {
		return false
	}
	la, ra := l.Attrs(), r.Attrs()
	if la.Alias != ra.Alias ||
		la.MTU != ra.MTU ||
		!bytes.Equal(la.HardwareAddr, ra.HardwareAddr) {
		return false
	}

	switch lv := l.(type) {
	case *netlink.Vxlan:
		if lv.Learning != r.(*netlink.Vxlan).Learning {
			return false
		}
	case *netlink.Bridge:
		rb := r.(*netlink.Bridge)
		if !ptr.Equal(lv.VlanFiltering, rb.VlanFiltering) || !ptr.Equal(lv.VlanDefaultPVID, rb.VlanDefaultPVID) {
			return false
		}
	case *netlink.Vrf:
		// No mutable type-specific fields
	case *netlink.Vlan:
		// No mutable type-specific fields
	case *netlink.Dummy:
		// No type-specific fields
	default:
		panic(fmt.Sprintf("BUG: unsupported device type %T", l))
	}
	return true
}

// linkImmutableFieldsEqual compares immutable managed link fields.
func linkImmutableFieldsEqual(l, r netlink.Link) bool {
	if l.Type() != r.Type() {
		return false
	}

	switch lv := l.(type) {
	case *netlink.Vxlan:
		rv := r.(*netlink.Vxlan)
		if lv.VxlanId != rv.VxlanId ||
			!lv.SrcAddr.Equal(rv.SrcAddr) ||
			lv.Port != rv.Port ||
			lv.FlowBased != rv.FlowBased ||
			lv.VniFilter != rv.VniFilter ||
			lv.Learning != rv.Learning {
			return false
		}
	case *netlink.Vrf:
		if lv.Table != r.(*netlink.Vrf).Table {
			return false
		}
	case *netlink.Vlan:
		rv := r.(*netlink.Vlan)
		if lv.VlanId != rv.VlanId ||
			lv.LinkAttrs.ParentIndex != rv.LinkAttrs.ParentIndex ||
			lv.VlanProtocol != rv.VlanProtocol ||
			!bytes.Equal(lv.Attrs().HardwareAddr, rv.Attrs().HardwareAddr) {
			return false
		}
	case *netlink.Bridge:
		// No immutable type-specific fields
	case *netlink.Dummy:
		// No type-specific fields
	default:
		panic(fmt.Sprintf("BUG: unsupported device type %T", l))
	}
	return true
}

// bridgePortSettingsMatch reports whether a kernel Protinfo matches the
// desired BridgePortSettings. This is the single source of truth for which
// Protinfo fields map to BridgePortSettings fields.
func bridgePortSettingsMatch(pi *netlink.Protinfo, desired *BridgePortSettings) bool {
	return pi.Learning == desired.Learning &&
		pi.NeighSuppress == desired.NeighSuppress &&
		pi.VlanTunnel == desired.VLANTunnel &&
		pi.Isolated == desired.Isolated
}

// addressesEqual compares two address slices for equality.
// Two slices are equal if they have the same addresses (by IPNet string).
// nil and empty slice are treated as different (nil = no management, empty = want no addresses).
func addressesEqual(a, b []netlink.Addr) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return sets.KeySet(addrListToMap(a)).Equal(sets.KeySet(addrListToMap(b)))
}

// addrListToMap converts a slice of addresses to a map keyed by IPNet string.
func addrListToMap(addrs []netlink.Addr) map[string]*netlink.Addr {
	result := make(map[string]*netlink.Addr, len(addrs))
	for i := range addrs {
		addr := &addrs[i]
		if addr.IPNet != nil {
			result[addr.IPNet.String()] = addr
		}
	}
	return result
}

// addrUpdateRequiresReconcile reports whether an address event indicates state
// divergence that requires reconciliation.
//
// The logic is an XOR of "is the address desired?" and "was it added?":
//   - Desired address added:     converged  → skip
//   - Desired address removed:   diverged   → reconcile
//   - Unexpected address added:  diverged   → reconcile
//   - Unexpected address removed: converging → skip
//
// When desired is nil (no address management), no address event needs
// reconciliation.
func addrUpdateRequiresReconcile(desired []netlink.Addr, update *netlink.AddrUpdate) bool {
	if desired == nil {
		return false
	}
	return isDesiredAddress(desired, update.LinkAddress) != update.NewAddr
}

func isDesiredAddress(desired []netlink.Addr, addr net.IPNet) bool {
	evalAddr := addr.String()
	for _, desiredAddr := range desired {
		if desiredAddr.IPNet != nil && desiredAddr.IPNet.String() == evalAddr {
			return true
		}
	}
	return false
}

// isLinkLocalAddress returns true for link-local addresses (IPv6 fe80::/10 or IPv4 169.254.0.0/16).
// These addresses are kernel-managed and should not be removed automatically.
func isLinkLocalAddress(ip net.IP) bool {
	return ip != nil && ip.IsLinkLocalUnicast()
}

// vidVNIMappingsEqual compares two VIDVNIMapping slices for equality (order-independent).
// nil and empty slice are treated as different (nil = no management, empty = want no mappings).
func vidVNIMappingsEqual(a, b []VIDVNIMapping) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return sets.New(a...).Equal(sets.New(b...))
}

// validateConfig checks that a DeviceConfig is well-formed.
func validateConfig(cfg *DeviceConfig) error {
	if err := validateLinkType(cfg.Link); err != nil {
		return err
	}
	name := cfg.Link.Attrs().Name
	if err := validateInterfaceName(name, "device"); err != nil {
		return err
	}
	if cfg.Master != "" {
		if err := validateInterfaceName(cfg.Master, "master"); err != nil {
			return err
		}
	}
	if cfg.Link.Attrs().MasterIndex != 0 {
		return fmt.Errorf("device %q: set DeviceConfig.Master instead of LinkAttrs.MasterIndex (resolved internally)", name)
	}
	if vlan, isVlan := cfg.Link.(*netlink.Vlan); isVlan {
		if cfg.VLANParent == "" {
			return fmt.Errorf("device %q: VLAN requires VLANParent", name)
		}
		if err := validateInterfaceName(cfg.VLANParent, "VLAN parent"); err != nil {
			return err
		}
		if vlan.ParentIndex != 0 {
			return fmt.Errorf("device %q: set DeviceConfig.VLANParent instead of LinkAttrs.ParentIndex (resolved internally)", name)
		}
	} else if cfg.VLANParent != "" {
		return fmt.Errorf("device %q: VLANParent set but Link is %T, not *netlink.Vlan", name, cfg.Link)
	}
	for _, addr := range cfg.Addresses {
		if addr.IPNet != nil && isLinkLocalAddress(addr.IPNet.IP) {
			return fmt.Errorf("device %q: link-local address %s cannot be managed (kernel-managed)", name, addr.IPNet)
		}
	}
	if cfg.VIDVNIMappings != nil {
		if _, ok := cfg.Link.(*netlink.Vxlan); !ok {
			return fmt.Errorf("device %q: VIDVNIMappings is only valid for VXLAN devices, got %T", name, cfg.Link)
		}
		if cfg.Master == "" {
			return fmt.Errorf("device %q: VIDVNIMappings requires a bridge master", name)
		}
		if err := validateMappings(cfg.VIDVNIMappings); err != nil {
			return fmt.Errorf("device %q: %w", name, err)
		}
	}
	if err := validateSupportedFields(cfg.Link); err != nil {
		return fmt.Errorf("device %q: %w", name, err)
	}
	return nil
}

// validateLinkType returns an error if the link type is not supported by the manager.
func validateLinkType(link netlink.Link) error {
	switch link.(type) {
	case *netlink.Bridge, *netlink.Vxlan, *netlink.Vlan, *netlink.Vrf, *netlink.Dummy:
		return nil
	default:
		return fmt.Errorf("unsupported link type %T; supported: Bridge, Vxlan, Vlan, Vrf, Dummy", link)
	}
}

// validateInterfaceName checks if an interface name is valid for Linux.
// Returns an error if the name is empty, exceeds IFNAMSIZ-1, contains
// characters rejected by the kernel (/, NUL, whitespace), or is reserved.
func validateInterfaceName(name, context string) error {
	if name == "" {
		return fmt.Errorf("%s name is empty", context)
	}
	if len(name) > types.MaxInterfaceNameLength {
		return fmt.Errorf("%s name %q exceeds maximum length of %d characters (got %d)",
			context, name, types.MaxInterfaceNameLength, len(name))
	}
	if name == "." || name == ".." {
		return fmt.Errorf("%s name %q is reserved", context, name)
	}
	if strings.ContainsAny(name, "/\x00") {
		return fmt.Errorf("%s name %q contains invalid characters", context, name)
	}
	if strings.ContainsAny(name, " \t\n") {
		return fmt.Errorf("%s name %q contains whitespace", context, name)
	}
	return nil
}

// validateSupportedFields checks that no unsupported fields are set to
// non-zero values by comparing the input link against its normalized form
// (which retains only managed fields). Any difference means unsupported
// fields were set.
//
// Uses reflect.DeepEqual because netlink structs embed LinkAttrs which
// contains slices making it not comparable with simple object equality
// (only field by field comparison would be possible).
// If reflection cost becomes a bottleneck we can reconsider this.
func validateSupportedFields(link netlink.Link) error {
	normalized := cloneLink(link)
	if !reflect.DeepEqual(link, normalized) {
		return fmt.Errorf("unsupported %T fields set to non-zero values; "+
			"these fields are not managed by NDM and will not be reconciled; "+
			"input: %#v, supported: %#v", link, link, normalized)
	}
	return nil
}

// validateMappings checks VID/VNI ranges and uniqueness constraints.
// Range: VID [1, 4094], VNI [1, 16777215].
// Uniqueness is required because:
//   - Two VIDs mapping to the same VNI would cause removeVIDVNIMapping to delete the VNI filter
//     entry still needed by the other VID
//   - Duplicate VIDs would be ambiguous
func validateMappings(mappings []VIDVNIMapping) error {
	seenVIDs := make(map[uint16]bool)
	seenVNIs := make(map[uint32]bool)

	for _, m := range mappings {
		if m.VID < 1 || m.VID > maxVID {
			return fmt.Errorf("VID %d out of valid range [1, %d]", m.VID, maxVID)
		}
		if m.VNI < 1 || m.VNI > maxVNI {
			return fmt.Errorf("VNI %d out of valid range [1, %d]", m.VNI, maxVNI)
		}
		if seenVIDs[m.VID] {
			return fmt.Errorf("duplicate VID %d", m.VID)
		}
		if seenVNIs[m.VNI] {
			return fmt.Errorf("duplicate VNI %d", m.VNI)
		}
		seenVIDs[m.VID] = true
		seenVNIs[m.VNI] = true
	}
	return nil
}
