// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package routeadvertisements

import (
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"unicode"

	frrtypes "github.com/metallb/frr-k8s/api/v1beta1"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
)

// errUnsafeFRRInput marks a configuration error raised because a value that
// would be written into the generated FRR configuration failed validation.
var errUnsafeFRRInput = fmt.Errorf("%w: unsafe FRR input", errConfig)

const (
	// maxRouteTargetLength equals the MaxLength marker on RouteTargetString
	// in pkg/crd/userdefinednetwork/v1/evpn.go.
	maxRouteTargetLength = 21
	// minVNI and maxVNI equal the Minimum and Maximum markers on
	// VRFConfig.VNI in pkg/crd/userdefinednetwork/v1/evpn.go.
	minVNI = 1
	maxVNI = 1<<24 - 1
	// maxQuotedValueLength is the longest prefix, in bytes, of an invalid
	// value that is quoted in an error message.
	maxQuotedValueLength = 64
)

// newVRFConfig returns the MAC-VRF or IP-VRF EVPN configuration of a network
// after validating its VNI and route target; kind names the VRF type in
// errors. An empty route target means FRR defaults.
func newVRFConfig(kind, networkName string, vni int32, routeTarget string) (*vrfConfig, error) {
	if err := validateVNI(vni); err != nil {
		return nil, fmt.Errorf("invalid %s VNI %d for network %q: %w", kind, vni, networkName, err)
	}
	if routeTarget != "" {
		if err := validateRouteTarget(routeTarget); err != nil {
			return nil, fmt.Errorf("invalid %s route target %s for network %q: %w", kind, quoteForError(routeTarget), networkName, err)
		}
	}
	return &vrfConfig{networkName: networkName, vni: vni, routeTarget: routeTarget}, nil
}

// newIPVRFConfig returns the IP-VRF EVPN configuration of a network after
// validating its VNI, route target and VRF name.
func newIPVRFConfig(networkName, vrfName string, vni int32, routeTarget string, hasIPv4, hasIPv6 bool) (*ipVRFConfig, error) {
	cfg, err := newVRFConfig("IP-VRF", networkName, vni, routeTarget)
	if err != nil {
		return nil, err
	}
	if err := validateNetworkVRFName(networkName, vrfName); err != nil {
		return nil, err
	}
	return &ipVRFConfig{vrfConfig: *cfg, vrfName: vrfName, hasIPv4: hasIPv4, hasIPv6: hasIPv6}, nil
}

// validateNetworkVRFName checks that the VRF name derived for a network is a
// valid Linux interface name.
func validateNetworkVRFName(networkName, vrf string) error {
	if err := validateInterfaceName(vrf); err != nil {
		return fmt.Errorf("invalid VRF name %s for network %q: %w", quoteForError(vrf), networkName, err)
	}
	return nil
}

// quoteForError returns the value quoted for an error message, cut to
// maxQuotedValueLength bytes followed by "..." when longer.
func quoteForError(value string) string {
	if len(value) > maxQuotedValueLength {
		return fmt.Sprintf("%q...", value[:maxQuotedValueLength])
	}
	return fmt.Sprintf("%q", value)
}

// validateVNI checks that the VNI is within minVNI and maxVNI.
func validateVNI(vni int32) error {
	if vni < minVNI || vni > maxVNI {
		return fmt.Errorf("must be between %d and %d", minVNI, maxVNI)
	}
	return nil
}

// validateToken checks that the value is a single non-empty FRR token: no
// whitespace and no control or other non-printable characters.
func validateToken(value string) error {
	if value == "" {
		return errors.New("empty value")
	}
	for _, r := range value {
		if unicode.IsSpace(r) || !unicode.IsPrint(r) {
			return fmt.Errorf("contains whitespace or non-printable character %q", r)
		}
	}
	return nil
}

// frrNeighbor is a neighbor identifier written to the raw FRR config: the
// session address or, for an unnumbered neighbor, the interface name. It is
// only built through newFRRNeighbor, which validates it.
type frrNeighbor struct {
	text string
	// addr is the parsed address, zero for an interface name
	addr netip.Addr
}

// newFRRNeighbor returns the validated identifier of an FRRConfiguration
// neighbor, with the same precedence as neighborKey: the address when set,
// which must be an IP address without zone, otherwise the interface, which
// must be a Linux interface name.
func newFRRNeighbor(neighbor frrtypes.Neighbor) (frrNeighbor, error) {
	if neighbor.Address != "" {
		addr, err := netip.ParseAddr(neighbor.Address)
		if err != nil {
			return frrNeighbor{}, fmt.Errorf("address %s is not an IP address", quoteForError(neighbor.Address))
		}
		if addr.Zone() != "" {
			return frrNeighbor{}, fmt.Errorf("address %s must not have a zone", quoteForError(neighbor.Address))
		}
		// An IPv4-mapped IPv6 address classifies as IPv4, as it does in the
		// prefix filtering based on net.IP.To4.
		return frrNeighbor{text: neighbor.Address, addr: addr.Unmap()}, nil
	}
	if err := validateInterfaceName(neighbor.Interface); err != nil {
		return frrNeighbor{}, fmt.Errorf("interface %s is not an interface name: %w", quoteForError(neighbor.Interface), err)
	}
	return frrNeighbor{text: neighbor.Interface}, nil
}

// is returns whether the neighbor is identified by the given address.
func (n frrNeighbor) is(addr netip.Addr) bool {
	return n.addr.IsValid() && n.addr == addr
}

// isIPv4 returns whether the neighbor is identified by an IPv4 address.
func (n frrNeighbor) isIPv4() bool {
	return n.addr.Is4()
}

// isIPv6 returns whether the neighbor is identified by an IPv6 address.
func (n frrNeighbor) isIPv6() bool {
	return n.addr.Is6()
}

// validateInterfaceName checks that the value is a valid Linux network
// interface name: a single token of at most types.MaxInterfaceNameLength
// bytes, not "." or "..", and without '/' or ':'.
func validateInterfaceName(name string) error {
	if len(name) > types.MaxInterfaceNameLength {
		return fmt.Errorf("longer than %d bytes", types.MaxInterfaceNameLength)
	}
	if err := validateToken(name); err != nil {
		return err
	}
	if name == "." || name == ".." {
		return fmt.Errorf("must not be %q", name)
	}
	if strings.ContainsAny(name, "/:") {
		return errors.New("must not contain '/' or ':'")
	}
	return nil
}

// validateRouteTarget checks that the value is an FRR EVPN route target of
// the form A.B.C.D:MN, EF:OPQR, GHJK:MN, *:OPQR or *:MN. It rejects
// everything the MaxLength and XValidation markers on RouteTargetString in
// pkg/crd/userdefinednetwork/v1/evpn.go reject (a 2-byte ASN or wildcard
// global administrator allows a 4-byte local administrator, a 4-byte ASN or
// IPv4 global administrator allows a 2-byte local administrator) and
// additionally rejects whitespace and control characters and a numeric
// global administrator above 4294967295.
func validateRouteTarget(rt string) error {
	if len(rt) > maxRouteTargetLength {
		return fmt.Errorf("longer than %d bytes", maxRouteTargetLength)
	}
	if err := validateToken(rt); err != nil {
		return err
	}
	global, local, found := strings.Cut(rt, ":")
	if !found || strings.Contains(local, ":") {
		return errors.New("must contain exactly one colon")
	}
	if !isDecimal(local) {
		return errors.New("local administrator must be a number")
	}
	localAdmin, err := strconv.ParseUint(local, 10, 32)
	if err != nil {
		return errors.New("local administrator must be at most 4294967295")
	}
	switch {
	case global == "*":
		return nil
	case isIPv4Address(global):
		if localAdmin > 65535 {
			return errors.New("local administrator must be at most 65535 with an IPv4 global administrator")
		}
		return nil
	case isDecimal(global):
		globalAdmin, err := strconv.ParseUint(global, 10, 32)
		if err != nil {
			return errors.New("global administrator must be at most 4294967295")
		}
		if globalAdmin > 65535 && localAdmin > 65535 {
			return errors.New("local administrator must be at most 65535 with a 4-byte ASN global administrator")
		}
		return nil
	default:
		return errors.New("global administrator must be either '*', an IPv4 address, or a number")
	}
}

// isIPv4Address returns whether the value parses as an IPv4 address.
func isIPv4Address(value string) bool {
	addr, err := netip.ParseAddr(value)
	return err == nil && addr.Is4()
}

// isDecimal returns whether the value is a non-empty string of ASCII digits.
func isDecimal(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
