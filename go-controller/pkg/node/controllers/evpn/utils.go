// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package evpn

// EVPN device naming conventions.
//
// All names are capped at 15 characters. Names use a 4-character
// prefix followed by a separator and identifier:
//
//   "-" separator = human-readable name   (VTEP name or CUDN name)
//   "." separator = computed fallback     (sha256 hash or network ID)
//
// Since "." is not valid in Kubernetes names and names cannot start with "-",
// the two paths can never collide.
//
//   Prefix  Device          Name example  Fallback example
//   evbr    EVPN bridge     evbr-myvtep   evbr.a3f2b1c9
//   evx4    VXLAN IPv4      evx4-myvtep   evx4.a3f2b1c9
//   evx6    VXLAN IPv6      evx6-myvtep   evx6.a3f2b1c9
//   svl3    L3/IP-VRF SVI   svl3-blue     svl3.42
//   svl2    L2/MAC-VRF SVI  svl2-blue     svl2.42
//   ovl2    OVS L2 port     ovl2-blue     ovl2.42
import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	utilnet "k8s.io/utils/net"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

const (
	bridgePrefix  = "evbr"
	vxlan4Prefix  = "evx4"
	vxlan6Prefix  = "evx6"
	l3SVIPrefix   = "svl3"
	l2SVIPrefix   = "svl2"
	ovsPortPrefix = "ovl2"
)

// GetEVPNBridgeName returns the EVPN bridge name for a VTEP.
// Uses VTEP name if it fits within the interface name limit, otherwise uses a hash.
func GetEVPNBridgeName(vtepName string) string {
	return getEVPNVTEPDeviceName(vtepName, bridgePrefix)
}

// GetEVPNVXLANName returns the VXLAN name for a VTEP for the given IP family.
// Uses VTEP name if it fits within the interface name limit, otherwise uses a hash.
func GetEVPNVXLANName(vtepName string, family utilnet.IPFamily) string {
	if family == utilnet.IPv6 {
		return getEVPNVTEPDeviceName(vtepName, vxlan6Prefix)
	}
	return getEVPNVTEPDeviceName(vtepName, vxlan4Prefix)
}

// getEVPNVTEPDeviceName generates a device name from the VTEP name.
// Name-based uses "-" separator (e.g. evbr-abc), hash-based uses "." separator
// (e.g. evbr.a3f2b1c9). Since VTEP names cannot start with ".", and hash output
// is always hex, the two paths can never collide.
func getEVPNVTEPDeviceName(vtepName, prefix string) string {
	candidate := prefix + "-" + vtepName
	if len(candidate) <= types.MaxInterfaceNameLength {
		return candidate
	}
	h := sha256.Sum256([]byte(vtepName))
	return prefix + "." + hex.EncodeToString(h[:])[:8]
}

// GetEVPNL3SVIName returns the L3 (IP-VRF) SVI name for an EVPN network (e.g. svl3-mynet or svl3.42).
func GetEVPNL3SVIName(netInfo util.NetInfo) string {
	return getEVPNNetworkDeviceName(netInfo, l3SVIPrefix)
}

// GetEVPNL2SVIName returns the L2 (MAC-VRF) SVI name for an EVPN network (e.g. svl2-mynet or svl2.42).
func GetEVPNL2SVIName(netInfo util.NetInfo) string {
	return getEVPNNetworkDeviceName(netInfo, l2SVIPrefix)
}

// GetEVPNOVSPortName returns the OVS port name for an EVPN network (e.g. ovl2-mynet or ovl2.42).
func GetEVPNOVSPortName(netInfo util.NetInfo) string {
	return getEVPNNetworkDeviceName(netInfo, ovsPortPrefix)
}

// getEVPNNetworkDeviceName generates device names with a given prefix.
// It uses the CUDN name if available and fits, otherwise falls back to NetworkID.
// Name-based uses "-" separator (e.g. svl3-mynet), ID-based uses "." separator
// (e.g. svl3.42). Since "." is not valid in Kubernetes names, and Kubernetes names
// cannot start with "-", the two paths can never collide.
func getEVPNNetworkDeviceName(netInfo util.NetInfo, prefix string) string {
	udnNamespace, udnName := util.ParseNetworkName(netInfo.GetNetworkName())
	if udnName != "" && udnNamespace == "" {
		candidate := prefix + "-" + udnName
		if len(candidate) <= types.MaxInterfaceNameLength {
			return candidate
		}
	}
	return fmt.Sprintf("%s.%d", prefix, netInfo.GetNetworkID())
}
