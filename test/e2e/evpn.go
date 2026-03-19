// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	udnv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	vtepv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1"
	vtepclientset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1/apis/clientset/versioned"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/allocators"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider"
	infraapi "github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/api"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	utilnet "k8s.io/utils/net"
)

// =============================================================================
// EVPN E2E Test Infrastructure
// =============================================================================
//
// This file contains utilities and tests for EVPN (Ethernet VPN) functionality
// in OVN-Kubernetes.
//
// =============================================================================
// Test Infrastructure Overview (PS: the values are just for illustration purposes)
// =============================================================================
//
// EVPN Test Infrastructure:
//
// +---------------------------------------+                      +------------------+
// | KIND Cluster                          |    KIND Primary      | External FRR     |
// |                                       |    Network           | (reused from BGP)|
// | +-------------+ +-------------+       |    (BGP/EVPN)        |                  |
// | | Node 1      | | Node 2      |       |    172.18.0.0/16     | eth0 (primary)   |
// | | - FRR-K8s   | | - FRR-K8s   |       | <----------------->  | br0 (EVPN)       |
// | | - OVN VTEP  | | - OVN VTEP  |       |                      | vxlan0 (VTEP)    |
// | +-------------+ +-------------+       |                      +------------------+
// |                                       |                             |
// | +-------------+ +----------------+    |         +------------------+--------------------+
// | | Node 3      | | Pod (on CUDN)  |    |         |   CUDN-network   | agnhost-ipvrf-net  |
// | | - FRR-K8s   | | 10.100.0.0/16  |    |         |   10.100.0.0/16  | 172.27.102.0/24    |
// | | - OVN VTEP  | +----------------+    |         +--------|---------+---------|---------+
// | +-------------+                       |                  |                   |
// +---------------------------------------+                  v                   v
//                                                   +------------------+--------------------+
//                                                   | agnhost-macvrf   | agnhost-ipvrf      |
//                                                   | 10.100.0.250     | 172.27.102.2       |
//                                                   | VNI: 10100       | VNI: 20102         |
//                                                   +------------------+--------------------+
//
// EVPN tests depend on the RouteAdvertisements/BGP infrastructure being set up
// during KIND cluster installation. EVPN tests REUSE the existing FRR container
// created at install time rather than creating a new one.
// We extend its configuration to support EVPN by adding:
//   - a Linux bridge with vlan_filtering for EVPN (unique per test, e.g. brevpn<suffix>)
//   - a VXLAN device (VTEP) with vnifilter for SVD mode (unique per test, e.g. vxevpn<suffix>)
//   - VRF and SVI configuration for IP-VRF tests
//   - Access ports for MAC-VRF tests
//   - BGP EVPN address-family configuration
//
// See OKEP: https://github.com/ovn-org/ovn-kubernetes/blob/master/docs/okeps/okep-5088-evpn.md for more details.

// =============================================================================
// EVPN Utilities
// =============================================================================

const (
	agnhostHTTPPort = 8080
	// sharedNodeIPsVTEPName is the name of the shared VTEP CR used across
	// EVPN tests that reference node IP CIDRs. Created idempotently by the
	// first test that needs it; never deleted by per-test cleanup.
	sharedNodeIPsVTEPName = "e2e-evpn-shared-vtep-node-cidr-range-do-not-use-this-name-anywhere-else"
)

// setupEVPNBridgeOnExternalFRR creates a Linux bridge and VXLAN device on the external FRR
// container. This is the foundation for both MAC-VRF and IP-VRF tests.
//
// Creates:
//   - bridgeName (e.g. "brevpn7a3f"): Linux bridge with vlan_filtering enabled, vlan_default_pvid 0
//   - vxlanName  (e.g. "vxevpn7a3f"): VXLAN device in SVD (Single VXLAN Device) mode with vnifilter
//
// The vxlanName device is configured with:
//   - dstport 4789: Standard VXLAN port
//   - local <frrVTEPIPAddress>: Local VTEP IP (FRR's IP on KIND network) - in future when VTEP CR is implemented, this will be the VTEP IP
//   - nolearning: Disable MAC learning (controlled by BGP EVPN)
//   - external: Allow external FDB programming
//   - vnifilter: Enable per-VLAN VNI mapping (SVD mode)
//
// Cleanup is automatically registered via ictx.AddCleanUpFn().
func setupEVPNBridgeOnExternalFRR(ictx infraapi.Context, frrContainerName, frrVTEPIPAddress, bridgeName, vxlanName string) error {
	frr := infraapi.ExternalContainer{Name: frrContainerName}

	// Idempotent: if the bridge already exists, skip creation.
	if _, err := infraprovider.Get().ExecExternalContainerCommand(frr, []string{"ip", "link", "show", bridgeName}); err == nil {
		framework.Logf("EVPN bridge %s already exists on %s, reusing", bridgeName, frrContainerName)
		return nil
	}

	// Create bridge with VLAN filtering
	commands := [][]string{
		{"ip", "link", "add", bridgeName, "type", "bridge", "vlan_filtering", "1", "vlan_default_pvid", "0"},
		{"ip", "link", "set", bridgeName, "addrgenmode", "none"},
	}

	for _, cmd := range commands {
		_, err := infraprovider.Get().ExecExternalContainerCommand(frr, cmd)
		if err != nil {
			return fmt.Errorf("failed to execute %v: %w", cmd, err)
		}
	}

	// Create VXLAN device in SVD mode
	vxlanCmd := []string{
		"ip", "link", "add", vxlanName, "type", "vxlan",
		"dstport", "4789",
		"local", frrVTEPIPAddress,
		"nolearning",
		"external",
		"vnifilter",
	}
	_, err := infraprovider.Get().ExecExternalContainerCommand(frr, vxlanCmd)
	if err != nil {
		return fmt.Errorf("failed to create %s: %w", vxlanName, err)
	}

	// Configure VXLAN device
	commands = [][]string{
		{"ip", "link", "set", vxlanName, "addrgenmode", "none"},
		{"ip", "link", "set", vxlanName, "master", bridgeName},
	}

	for _, cmd := range commands {
		_, err := infraprovider.Get().ExecExternalContainerCommand(frr, cmd)
		if err != nil {
			return fmt.Errorf("failed to execute %v: %w", cmd, err)
		}
	}

	// Bring up interfaces
	commands = [][]string{
		{"ip", "link", "set", bridgeName, "up"},
		{"ip", "link", "set", vxlanName, "up"},
	}

	for _, cmd := range commands {
		_, err := infraprovider.Get().ExecExternalContainerCommand(frr, cmd)
		if err != nil {
			return fmt.Errorf("failed to execute %v: %w", cmd, err)
		}
	}

	// Configure VXLAN bridge options for EVPN
	bridgeCmd := []string{
		"bridge", "link", "set", "dev", vxlanName,
		"vlan_tunnel", "on",
		"neigh_suppress", "on",
		"learning", "off",
	}
	_, err = infraprovider.Get().ExecExternalContainerCommand(frr, bridgeCmd)
	if err != nil {
		return fmt.Errorf("failed to configure %s bridge options: %w", vxlanName, err)
	}

	// Register cleanup to remove bridge and VXLAN device.
	// Idempotent: checks existence before deleting so multiple cleanups are safe
	// (e.g. when shared bridge is cleaned up by first test, second finds it gone).
	ictx.AddCleanUpFn(func() error {
		frr := infraapi.ExternalContainer{Name: frrContainerName}

		if _, err := infraprovider.Get().ExecExternalContainerCommand(frr, []string{"ip", "link", "show", vxlanName}); err == nil {
			if _, err := infraprovider.Get().ExecExternalContainerCommand(frr, []string{"ip", "link", "del", vxlanName}); err != nil {
				return fmt.Errorf("failed to delete %s: %w", vxlanName, err)
			}
		}

		if _, err := infraprovider.Get().ExecExternalContainerCommand(frr, []string{"ip", "link", "show", bridgeName}); err == nil {
			if _, err := infraprovider.Get().ExecExternalContainerCommand(frr, []string{"ip", "link", "del", bridgeName}); err != nil {
				return fmt.Errorf("failed to delete %s: %w", bridgeName, err)
			}
		}

		framework.Logf("EVPN bridge cleanup complete on %s", frrContainerName)
		return nil
	})

	framework.Logf("EVPN bridge setup complete on %s (%s + %s with local IP %s)", frrContainerName, bridgeName, vxlanName, frrVTEPIPAddress)
	return nil
}

// setupVNIVIDMappingsOnExternalFRR sets up VLAN/VNI mappings for the given
// bridge and vxlan interfaces
func setupVNIVIDMappingsOnExternalFRR(frrContainerName string, vni, vid int, bridgeName, vxlanName string) error {
	frr := infraapi.ExternalContainer{Name: frrContainerName}
	vidStr := fmt.Sprintf("%d", vid)
	vniStr := fmt.Sprintf("%d", vni)
	commands := [][]string{
		// Add VLAN to bridge
		{"bridge", "vlan", "add", "dev", bridgeName, "vid", vidStr, "self"},
		// Add VLAN to vxlan device
		{"bridge", "vlan", "add", "dev", vxlanName, "vid", vidStr},
		// Add VNI to vxlan device
		{"bridge", "vni", "add", "dev", vxlanName, "vni", vniStr},
		// Map VLAN to VNI (tunnel_info)
		{"bridge", "vlan", "add", "dev", vxlanName, "vid", vidStr, "tunnel_info", "id", vniStr},
	}
	for _, cmd := range commands {
		if _, err := infraprovider.Get().ExecExternalContainerCommand(frr, cmd); err != nil {
			return fmt.Errorf("failed to setup VLAN/VNI mappings (VNI %d, VID %d): %w", vni, vid, err)
		}
	}

	framework.Logf("VLAN/VNI mappings setup complete on %s (VNI %d, VID %d)", frrContainerName, vni, vid)
	return nil
}

// setupSVIOnExternalFRR sets up a SVI on the provided VLAN and VLAN aware
// bridge and optionally attaches it to a VRF
func setupSVIOnExternalFRR(ictx infraapi.Context, frrContainerName string, vid int, bridgeName, vrfName string) error {
	frr := infraapi.ExternalContainer{Name: frrContainerName}
	vidStr := fmt.Sprintf("%d", vid)
	sviName := fmt.Sprintf("%s.%d", bridgeName, vid)

	// Create SVI
	_, err := infraprovider.Get().ExecExternalContainerCommand(frr,
		[]string{"ip", "link", "add", sviName, "link", bridgeName, "type", "vlan", "id", vidStr})
	if err != nil {
		return fmt.Errorf("failed to create SVI %s: %w", sviName, err)
	}
	ictx.AddCleanUpFn(func() error {
		// Delete SVI
		_, err := infraprovider.Get().ExecExternalContainerCommand(frr,
			[]string{"ip", "link", "del", sviName})
		if err != nil {
			return fmt.Errorf("failed to delete SVI %s: %v", sviName, err)
		}

		framework.Logf("SVI %s cleanup complete on %s (VID %d, VRF: %q)", sviName, frrContainerName, vid, vrfName)
		return nil
	})

	// No addresses for SVI
	_, err = infraprovider.Get().ExecExternalContainerCommand(frr,
		[]string{"ip", "link", "set", sviName, "addrgenmode", "none"})
	if err != nil {
		return fmt.Errorf("failed to disable addrgen on SVI %s: %w", sviName, err)
	}

	if vrfName != "" {
		// Bind SVI to VRF
		_, err = infraprovider.Get().ExecExternalContainerCommand(frr,
			[]string{"ip", "link", "set", sviName, "master", vrfName})
		if err != nil {
			return fmt.Errorf("failed to bind SVI %s to VRF %s: %w", sviName, vrfName, err)
		}
	}

	// Bring up SVI
	_, err = infraprovider.Get().ExecExternalContainerCommand(frr,
		[]string{"ip", "link", "set", sviName, "up"})
	if err != nil {
		return fmt.Errorf("failed to bring up SVI %s: %w", sviName, err)
	}

	framework.Logf("SVI %s setup complete on %s (VID %d, VRF: %q)", sviName, frrContainerName, vid, vrfName)
	return nil
}

// setupMACVRFOnExternalFRR configures MAC-VRF (Layer 2 EVPN) on the external FRR container.
// This adds the VLAN/VNI mapping to extend the L2 domain via EVPN Type-2/Type-3 routes.
//
// Requires: setupEVPNBridgeOnExternalFRR must be called first to create bridgeName and vxlanName.
//
// Cleanup is automatically registered via ictx.AddCleanUpFn().
//
// Parameters:
//   - vni: VXLAN Network Identifier (e.g., 10100)
//   - vid: VLAN ID for local bridging (e.g., 100)
//   - bridgeName: name of the bridge device (e.g., "brevpn7a3f")
//   - vxlanName: name of the VXLAN device (e.g., "vxevpn7a3f")
func setupMACVRFOnExternalFRR(ictx infraapi.Context, frrContainerName string, vni, vid int, bridgeName, vxlanName string) error {
	err := setupVNIVIDMappingsOnExternalFRR(frrContainerName, vni, vid, bridgeName, vxlanName)
	if err != nil {
		return fmt.Errorf("failed to configure VLAN/VNI mapping on bridge %s: %w", bridgeName, err)
	}

	err = setupSVIOnExternalFRR(ictx, frrContainerName, vid, bridgeName, "")
	if err != nil {
		return fmt.Errorf("failed to configure SVI for VID %d: %w", vid, err)
	}

	framework.Logf("MAC-VRF setup complete on %s (VNI %d)", frrContainerName, vni)
	return nil
}

// setupIPVRFOnExternalFRR configures IP-VRF (Layer 3 EVPN) on the external FRR container.
// This creates a Linux VRF with SVI for L3 routing via EVPN Type-5 routes.
//
// Requires: setupEVPNBridgeOnExternalFRR must be called first to create bridgeName and vxlanName.
//
// Parameters:
//   - vrfName: Name of the Linux VRF (e.g., "vrf202")
//   - vni: VXLAN Network Identifier (e.g., 20102)
//   - vid: VLAN ID for the SVI (e.g., 202)
//   - bridgeName: name of the bridge device (e.g., "brevpn7a3f")
//   - vxlanName: name of the VXLAN device (e.g., "vxevpn7a3f")
//
// Cleanup is automatically registered via ictx.AddCleanUpFn().
// Note: VLAN/VNI mappings are cleaned up when bridgeName/vxlanName are deleted.
func setupIPVRFOnExternalFRR(ictx infraapi.Context, frrContainerName, vrfName string, vni, vid int, bridgeName, vxlanName string) error {
	frr := infraapi.ExternalContainer{Name: frrContainerName}
	vniStr := fmt.Sprintf("%d", vni)

	// Create Linux VRF with routing table = VNI
	_, err := infraprovider.Get().ExecExternalContainerCommand(frr,
		[]string{"ip", "link", "add", vrfName, "type", "vrf", "table", vniStr})
	if err != nil {
		return fmt.Errorf("failed to create VRF %s: %w", vrfName, err)
	}

	// Bring up VRF
	_, err = infraprovider.Get().ExecExternalContainerCommand(frr,
		[]string{"ip", "link", "set", vrfName, "up"})
	if err != nil {
		return fmt.Errorf("failed to bring up VRF %s: %w", vrfName, err)
	}

	// Register cleanup to remove SVI, Linux VRF, and FRR VRF definition
	ictx.AddCleanUpFn(func() error {
		// Delete Linux VRF
		_, err = infraprovider.Get().ExecExternalContainerCommand(frr,
			[]string{"ip", "link", "del", vrfName})
		if err != nil {
			return fmt.Errorf("failed to delete Linux VRF %s: %v", vrfName, err)
		}

		// Delete FRR VRF definition (now that Linux VRF is gone, FRR should allow this)
		_, err = infraprovider.Get().ExecExternalContainerCommand(frr, vtyshCommand(
			"configure terminal", fmt.Sprintf("no vrf %s", vrfName), "end",
		))
		if err != nil {
			return fmt.Errorf("failed to delete FRR VRF definition %s: %v", vrfName, err)
		}

		framework.Logf("IP-VRF cleanup complete on %s (VNI %d)", frrContainerName, vni)
		return nil
	})

	err = setupVNIVIDMappingsOnExternalFRR(frrContainerName, vni, vid, bridgeName, vxlanName)
	if err != nil {
		return fmt.Errorf("failed to configure VLAN/VNI mapping on bridge %s: %w", bridgeName, err)
	}

	err = setupSVIOnExternalFRR(ictx, frrContainerName, vid, bridgeName, vrfName)
	if err != nil {
		return fmt.Errorf("failed to configure SVI for VID %d: %w", vid, err)
	}

	framework.Logf("IP-VRF setup complete on %s (VNI %d)", frrContainerName, vni)
	return nil
}

// vtyshCommand builds a shell command that invokes vtysh with single-quoted -c arguments.
// Using sh -c with single-quoted args ensures correct argument parsing regardless of
// how the infra provider executes the command (docker exec, podman exec, SSH, etc.).
func vtyshCommand(args ...string) []string {
	var parts []string
	for _, arg := range args {
		parts = append(parts, fmt.Sprintf("-c '%s'", arg))
	}
	return []string{"sh", "-c", "vtysh " + strings.Join(parts, " ")}
}

// setupEVPNBGPOnExternalFRR ensures the global BGP EVPN settings are present on the external FRR container.
// This is a redundancy check — the l2vpn evpn address-family, advertise-all-vni, and neighbor
// activations are configured at KIND cluster install time (deploy_frr_external_container in kind-common.sh
// when ENABLE_EVPN=true). Calling this here is idempotent and guards against clusters not set up
// with ENABLE_EVPN.
//
// Parameters:
//   - frrContainerName: Name of the external FRR container (e.g., "frr" or "frr-ebgp")
//   - asn: BGP Autonomous System Number of the external FRR (e.g., 64512 for iBGP, 64513 for eBGP)
//   - neighborIPs: List of cluster node IPs to peer with
//   - isIBGP: Whether this is an iBGP session (determines if route-reflector-client is configured)
//
// No cleanup is registered: these are shared cluster-level settings that must persist
// across all parallel EVPN tests.
func setupEVPNBGPOnExternalFRR(ictx infraapi.Context, frrContainerName string, asn int, neighborIPs []string, isIBGP bool) error {
	frr := infraapi.ExternalContainer{Name: frrContainerName}

	args := []string{"configure terminal", fmt.Sprintf("router bgp %d", asn), "address-family l2vpn evpn", "advertise-all-vni"}
	for _, ip := range neighborIPs {
		args = append(args, fmt.Sprintf("neighbor %s activate", ip))
		if isIBGP {
			args = append(args, fmt.Sprintf("neighbor %s route-reflector-client", ip))
		}
	}
	args = append(args, "exit-address-family", "end")

	_, err := infraprovider.Get().ExecExternalContainerCommand(frr, vtyshCommand(args...))
	if err != nil {
		return fmt.Errorf("failed to configure EVPN BGP: %w", err)
	}

	// No per-test BGP cleanup needed on the external FRR:
	// - advertise-all-vni: global setting, shared across all parallel tests
	// - neighbor <nodeIP> activate: all tests peer with the same cluster nodes
	// Both are baseline infrastructure set up by the KIND cluster install (kind.sh -rae)
	// and must persist for the duration of the test suite.
	// IP-VRF per-VRF BGP config is cleaned up by setupIPVRFBGPOnExternalFRR.
	ictx.AddCleanUpFn(func() error {
		framework.Logf("EVPN BGP cleanup complete on %s (no-op: global BGP settings are shared infrastructure)", frrContainerName)
		return nil
	})

	framework.Logf("EVPN BGP setup complete on %s (ASN %d, neighbors: %v, iBGP: %v)", frrContainerName, asn, neighborIPs, isIBGP)
	return nil
}

// setupIPVRFBGPOnExternalFRR configures BGP for an IP-VRF on the external FRR container.
// This binds the VRF to a VNI and configures route-targets for Type-5 route exchange.
//
// Requires: setupEVPNBGPOnExternalFRR must be called first for global EVPN BGP config.
//
// Parameters:
//   - vrfName: Name of the Linux VRF (must match the VRF created by setupIPVRFOnExternalFRR)
//   - asn: BGP ASN of this external FRR instance (e.g., 64512 for iBGP, 64513 for eBGP).
//     Used for the VRF BGP instance (`router bgp <asn> vrf <vrfName>`).
//   - rtASN: ASN component of Route Distinguisher and Route Target (RT = rtASN:VNI).
//     The cluster-side setup script always uses the cluster's own ASN for RTs,
//     so rtASN must always be the cluster's ASN to keep both sides in sync.
//   - vni: VXLAN Network Identifier for route-target (e.g., 20102)
//   - ipFamilies: IP families to configure (e.g., sets.New(utilnet.IPv4) or sets.New(utilnet.IPv4, utilnet.IPv6))
//   - subnets: Subnets to advertise via BGP (e.g., []string{"172.27.102.0/24"} or {"172.27.102.0/24", "fd00:102::/64"})
//
// Cleanup is automatically registered via ictx.AddCleanUpFn().
func setupIPVRFBGPOnExternalFRR(ictx infraapi.Context, frrContainerName, vrfName string, asn, rtASN, vni int, ipFamilies sets.Set[utilnet.IPFamily], subnets []string) error {
	frr := infraapi.ExternalContainer{Name: frrContainerName}
	rt := fmt.Sprintf("%d:%d", rtASN, vni)

	// Build vtysh args
	args := []string{
		"configure terminal",
		fmt.Sprintf("vrf %s", vrfName), fmt.Sprintf("vni %d", vni), "exit-vrf",
		fmt.Sprintf("router bgp %d vrf %s", asn, vrfName),
	}

	// Configure address-families with explicit network statements
	// (Using explicit 'network' statements instead of 'redistribute connected' to align
	// with how OVN-K advertises routes via RouteAdvertisements/FRRConfiguration Prefixes)
	if ipFamilies.Has(utilnet.IPv4) {
		args = append(args, "address-family ipv4 unicast")
		for _, subnet := range subnets {
			if !utilnet.IsIPv6CIDRString(subnet) {
				args = append(args, fmt.Sprintf("network %s", subnet))
			}
		}
		args = append(args, "exit-address-family")
	}
	if ipFamilies.Has(utilnet.IPv6) {
		args = append(args, "address-family ipv6 unicast")
		for _, subnet := range subnets {
			if utilnet.IsIPv6CIDRString(subnet) {
				args = append(args, fmt.Sprintf("network %s", subnet))
			}
		}
		args = append(args, "exit-address-family")
	}

	// l2vpn evpn - configure RD, RT, and advertise unicast routes
	args = append(args, "address-family l2vpn evpn", fmt.Sprintf("rd %s", rt), fmt.Sprintf("route-target import %s", rt), fmt.Sprintf("route-target export %s", rt))
	if ipFamilies.Has(utilnet.IPv4) {
		args = append(args, "advertise ipv4 unicast")
	}
	if ipFamilies.Has(utilnet.IPv6) {
		args = append(args, "advertise ipv6 unicast")
	}
	args = append(args, "exit-address-family", "end")

	_, err := infraprovider.Get().ExecExternalContainerCommand(frr, vtyshCommand(args...))
	if err != nil {
		return fmt.Errorf("failed to configure IP-VRF BGP for %s: %w", vrfName, err)
	}

	// Register cleanup to remove BGP VRF instance and VRF-VNI binding
	// NOTE: This cleanup may run after setupIPVRFOnExternalFRR cleanup has already deleted
	// the Linux VRF device, which triggers FRR to auto-cleanup the VRF config. In that case,
	// these commands may fail - that's OK, we just log warnings and won't retry that.
	ictx.AddCleanUpFn(func() error {
		frr := infraapi.ExternalContainer{Name: frrContainerName}

		_, err := infraprovider.Get().ExecExternalContainerCommand(frr, vtyshCommand(
			"configure terminal", fmt.Sprintf("vrf %s", vrfName), fmt.Sprintf("no vni %d", vni), "exit-vrf", "end",
		))
		if err != nil {
			framework.Logf("Warning: failed to remove VNI binding (may already be cleaned up): %v", err)
		}

		_, err = infraprovider.Get().ExecExternalContainerCommand(frr, vtyshCommand(
			"configure terminal", fmt.Sprintf("no router bgp %d vrf %s", asn, vrfName), "end",
		))
		if err != nil {
			framework.Logf("Warning: failed to remove BGP VRF (may already be cleaned up): %v", err)
		}

		// NOTE: We intentionally do NOT run "no vrf" here.
		// FRR's VRF definition is tied to the Linux VRF and will auto-cleanup
		// when the Linux VRF is deleted by setupIPVRFOnExternalFRR's cleanup.
		// Trying to delete the FRR VRF while the Linux VRF still has interfaces
		// attached causes "Only inactive VRFs can be deleted" errors.

		framework.Logf("IP-VRF BGP cleanup complete on %s (VRF %s)", frrContainerName, vrfName)
		return nil
	})

	framework.Logf("IP-VRF BGP setup complete on %s (VRF %s, ASN %d, VNI %d, RT %s, families %v)", frrContainerName, vrfName, asn, vni, rt, ipFamilies.UnsortedList())
	return nil
}

// =============================================================================
// EVPN Agnhost Utilities
// =============================================================================

// evpnContainerInfo holds the discovered network information for an EVPN external container.
type evpnContainerInfo struct {
	MAC                string
	containerIPs       []string
	containerInterface string
	frrIPs             []string
	frrInterface       string
}

// createEVPNExternalContainer creates a Docker network with the given subnets, creates
// the caller-provided container on it, attaches FRR to it, and discovers the assigned
// IPs and interface names.
//
// This is the shared foundation for both MAC-VRF and IP-VRF container setups.
// The caller is responsible for configuring FRR's interface (e.g., adding it to bridgeName
// as an access port for MAC-VRF, or putting it in a VRF for IP-VRF).
//
// The container's Image and CmdArgs are used as-is, allowing callers to provide any
// container image (e.g., agnhost for HTTP testing, netshoot for iperf3 testing).
//
// Parameters:
//   - container: ExternalContainer spec (Name, Image, CmdArgs, and optionally IPv4/IPv6 for static IPs)
//   - networkName: Name for the Docker network (e.g., "macvrf-net-100", "ipvrf-net-202")
//   - ipFamilies: Cluster IP family support, used to filter discovered IPs
//   - frrContainerName: name of the external FRR container to attach to the Docker network
//   - subnets: Subnets for the Docker network (e.g., "10.100.0.0/16" for IPv4, or both for dual-stack)
func createEVPNExternalContainer(ictx infraapi.Context, frrContainerName string, container infraapi.ExternalContainer, networkName string, ipFamilies sets.Set[utilnet.IPFamily], subnets []string) (*evpnContainerInfo, error) {
	// Step 1: Create Docker network with specific subnet(s)
	network, err := ictx.CreateNetwork(networkName, subnets...)
	if err != nil {
		return nil, fmt.Errorf("failed to create network %s: %w", networkName, err)
	}

	// Step 2: Create container on that network
	container.Network = network
	container.RuntimeArgs = append(container.RuntimeArgs, "--cap-add=NET_ADMIN")
	_, err = ictx.CreateExternalContainer(container)
	if err != nil {
		return nil, fmt.Errorf("failed to create container %s: %w", container.Name, err)
	}

	// Step 3: Connect FRR to the network
	_, err = ictx.AttachNetwork(network, frrContainerName)
	if err != nil {
		return nil, fmt.Errorf("failed to connect FRR to network %s: %w", networkName, err)
	}

	// Step 4: Discover assigned IPs and interface names
	containerNetInf, err := infraprovider.Get().GetExternalContainerNetworkInterface(
		infraapi.ExternalContainer{Name: container.Name}, network)
	if err != nil {
		return nil, fmt.Errorf("failed to get container network interface: %w", err)
	}

	frrNetInf, err := infraprovider.Get().GetExternalContainerNetworkInterface(
		infraapi.ExternalContainer{Name: frrContainerName}, network)
	if err != nil {
		return nil, fmt.Errorf("failed to get FRR network interface: %w", err)
	}

	frrInterface := frrNetInf.InfName
	if frrInterface == "" {
		return nil, fmt.Errorf("FRR interface name not found for network %s", networkName)
	}

	containerInterface := containerNetInf.InfName
	if containerInterface == "" {
		return nil, fmt.Errorf("container interface name not found for network %s", networkName)
	}

	// Collect IPs only for cluster-supported address families.
	// Docker may assign IPs for families we didn't request (e.g., default IPv4 on IPv6-only networks),
	// so we filter based on what the cluster actually supports.
	var containerIPs, frrIPs []string
	if ipFamilies.Has(utilnet.IPv4) {
		if containerNetInf.IPv4 != "" {
			containerIPs = append(containerIPs, containerNetInf.IPv4)
		}
		if frrNetInf.IPv4 != "" {
			frrIPs = append(frrIPs, frrNetInf.IPv4)
		}
	}
	if ipFamilies.Has(utilnet.IPv6) {
		if containerNetInf.IPv6 != "" {
			containerIPs = append(containerIPs, containerNetInf.IPv6)
		}
		if frrNetInf.IPv6 != "" {
			frrIPs = append(frrIPs, frrNetInf.IPv6)
		}
	}

	framework.Logf("EVPN external container created: %s (IPs: %v, FRR IPs: %v, interface: %s, FRR interface: %s)", container.Name, containerIPs, frrIPs, containerInterface, frrInterface)
	return &evpnContainerInfo{
		MAC:                containerNetInf.MAC,
		containerIPs:       containerIPs,
		containerInterface: containerInterface,
		frrIPs:             frrIPs,
		frrInterface:       frrInterface,
	}, nil
}

// =============================================================================
// MAC-VRF External Container Utilities
// =============================================================================

// getMACVRFAgnhostIPsFromSubnets derives MAC-VRF agnhost IPs from CUDN subnets.
// For each subnet, it returns an IP with host portion set to the high end address.
// Example: "10.100.0.0/16" -> "10.100.0.253/16", "fd00:100::/64" -> "fd00:100::ffff:ffff:ffff:fffe"
func getMACVRFAgnhostIPsFromSubnets(subnets []string) ([]string, error) {
	var ips []string
	for _, subnet := range subnets {
		_, ipNet, err := net.ParseCIDR(subnet)
		if err != nil {
			return nil, err
		}
		ips = append(ips, secondToLastIP(ipNet).String())
	}
	return ips, nil
}

// setupMACVRFExternalContainer creates a container connected to the EVPN bridge
// for MAC-VRF (Layer 2) connectivity testing.
//
// This function:
//  1. Creates a Docker network with the CUDN subnet and the provided container on it,
//     requesting the second-to-last IP of each subnet to avoid collisions
//     with OVN IPAM and Docker IPAM (both allocate from the low end)
//  2. Connects FRR to the network (Docker creates a veth pair automatically)
//  3. Moves FRR's interface to bridgeName as an access port for the MAC-VRF VLAN
//
// Requires: setupEVPNBridgeOnExternalFRR and setupMACVRFOnExternalFRR must be called first.
//
// The container will be on the same L2 segment as pods on the CUDN, allowing
// direct Layer 2 communication via EVPN Type-2/Type-3 routes.
//
// Parameters:
//   - container: ExternalContainer spec (Name, Image, CmdArgs) for the container to create
//   - networkName: name of the Docker network
//   - bridgeName: name of the EVPN bridge on the external FRR
//   - vid: VLAN ID for the access port on the bridge (e.g., 100)
//   - ipFamilies: Cluster IP family support (e.g., sets.New(utilnet.IPv4, utilnet.IPv6))
//   - subnets: Subnets for the Docker network matching the CUDN (e.g., "10.100.0.0/16")
//   - useProxyDockerNetwork: When true, the Docker network is created without explicit subnets
//     (Docker picks a random unused range) and the container's IPs are reassigned
//     afterward to match the CUDN subnets. This is needed when two MAC-VRF agnhosts
//     share the same CUDN subnet because Docker rejects overlapping subnets. The Docker
//     bridge still forwards L2 frames regardless of IP addressing.
func setupMACVRFExternalContainer(ictx infraapi.Context, frrContainerName string, container infraapi.ExternalContainer, networkName, bridgeName string, vid int, ipFamilies sets.Set[utilnet.IPFamily], subnets []string, useProxyDockerNetwork bool) (*evpnContainerInfo, error) {
	// Derive container IPs from CUDN subnets
	containerIPs, err := getMACVRFAgnhostIPsFromSubnets(subnets)
	if err != nil {
		return nil, fmt.Errorf("failed to derive MAC-VRF container IPs from subnets: %w", err)
	}

	if !useProxyDockerNetwork {
		ips4, ips6 := splitIPStringsByIPFamily(containerIPs)
		if len(ips4) > 0 {
			container.IPv4 = ips4[0]
		}
		if len(ips6) > 0 {
			container.IPv6 = ips6[0]
		}
	}

	var info *evpnContainerInfo
	dockerSubnets := subnets
	if useProxyDockerNetwork {
		dockerSubnets = nil
	}
	info, err = createEVPNExternalContainer(ictx, frrContainerName, container, networkName, ipFamilies, dockerSubnets)
	if err != nil {
		return nil, err
	}

	// When useProxyDockerNetwork is set, the network was created without explicit
	// subnets, this lets Docker pick a random unused range, avoiding Docker rejecting a
	// second network with the same CUDN CIDR. Then reassign the container's IPs
	// to match the actual CUDN subnets.
	if useProxyDockerNetwork {
		shellCmds := []string{
			fmt.Sprintf("ip addr flush dev %s", info.containerInterface),
		}
		// CreateNetwork infers IP family from the subnets passed to it; with nil
		// subnets it creates an IPv4-only network, which disables IPv6 on the
		// interface. Enable it so we can add IPv6 CUDN addresses.
		if ipFamilies.Has(utilnet.IPv6) {
			shellCmds = append(shellCmds,
				fmt.Sprintf("sysctl -w net.ipv6.conf.%s.disable_ipv6=0", info.containerInterface))
		}
		for i, subnet := range subnets {
			_, ipNet, parseErr := net.ParseCIDR(subnet)
			if parseErr != nil {
				return nil, fmt.Errorf("failed to parse CUDN subnet %s: %w", subnet, parseErr)
			}
			prefixLen, _ := ipNet.Mask.Size()
			addr := fmt.Sprintf("%s/%d", containerIPs[i], prefixLen)
			shellCmds = append(shellCmds,
				fmt.Sprintf("ip addr add %s dev %s", addr, info.containerInterface))
		}
		if _, err := infraprovider.Get().ExecExternalContainerCommand(container,
			[]string{"sh", "-c", strings.Join(shellCmds, " && ")}); err != nil {
			return nil, fmt.Errorf("failed to reassign IPs on container %s (network %s): %w", container.Name, networkName, err)
		}
		info.containerIPs = containerIPs
		framework.Logf("Reassigned container %s (network %s) IPs to CUDN IPs: %v", container.Name, networkName, containerIPs)
	}

	// Move FRR's interface to bridgeName and configure as access port
	frr := infraapi.ExternalContainer{Name: frrContainerName}
	vidStr := fmt.Sprintf("%d", vid)
	sviName := fmt.Sprintf("%s.%d", bridgeName, vid)
	frrCmds := [][]string{
		{"ip", "link", "set", info.frrInterface, "master", bridgeName},
		{"bridge", "vlan", "add", "dev", info.frrInterface, "vid", vidStr, "pvid", "untagged"},
	}
	// create static FDB and NEIGH entries to get BUM suppression working from the get go
	frrCmds = append(frrCmds,
		[]string{"bridge", "fdb", "replace", info.MAC, "dev", info.frrInterface, "vlan", vidStr, "master", "static"},
	)
	for _, ip := range info.containerIPs {
		frrCmds = append(frrCmds,
			[]string{"ip", "neigh", "replace", ip, "lladdr", info.MAC, "dev", sviName, "nud", "permanent"},
		)
	}
	for _, cmd := range frrCmds {
		if _, err = infraprovider.Get().ExecExternalContainerCommand(frr, cmd); err != nil {
			return nil, fmt.Errorf("failed to configure %s as %s access port for VID %s: %w", info.frrInterface, bridgeName, vidStr, err)
		}
	}

	framework.Logf("MAC-VRF external container setup complete: %s (IPs: %v, VID: %d, FRR interface: %s)", container.Name, containerIPs, vid, info.frrInterface)
	return info, nil
}

// =============================================================================
// IP-VRF External Container Utilities
// =============================================================================

// setupIPVRFExternalContainer creates a container connected to the external FRR's VRF
// for IP-VRF (Layer 3) connectivity testing.
//
// This function:
//  1. Creates a Docker network with the specified subnet and the provided container on it
//  2. Connects FRR to the network
//  3. Discovers assigned IPs (Docker assigns them from subnet)
//  4. Puts FRR's interface for that network into the VRF
//  5. Sets the container's default route via FRR
//
// Requires: setupIPVRFOnExternalFRR must be called first to create the VRF.
//
// The container will be on a separate routed subnet, reachable via EVPN Type-5 routes.
//
// Parameters:
//   - container: ExternalContainer spec (Name, Image, CmdArgs) for the container to create
//   - networkName: name of the Docker network
//   - vrfName: Name of the VRF to put FRR's interface in (must match setupIPVRFOnExternalFRR)
//   - ipFamilies: Cluster IP family support (e.g., sets.New(utilnet.IPv4, utilnet.IPv6))
//   - subnets: Subnets for the Docker network (e.g., "172.27.102.0/24" for IPv4, or both for dual-stack)
func setupIPVRFExternalContainer(ictx infraapi.Context, frrContainerName string, container infraapi.ExternalContainer, networkName, vrfName string, vid int, ipFamilies sets.Set[utilnet.IPFamily], subnets ...string) (*evpnContainerInfo, error) {
	info, err := createEVPNExternalContainer(ictx, frrContainerName, container, networkName, ipFamilies, subnets)
	if err != nil {
		return nil, err
	}

	// Put FRR's interface in the VRF
	// NOTE: keep_addr_on_down=1 is set at FRR container startup (in deploy_frr_external_container)
	// to preserve IPv6 addresses during VRF assignment. See https://github.com/FRRouting/frr/issues/1666
	frr := infraapi.ExternalContainer{Name: frrContainerName}

	frrCmds := [][]string{
		{"ip", "link", "set", info.frrInterface, "master", vrfName},
		{"ip", "link", "set", info.frrInterface, "up"},
	}
	for _, cmd := range frrCmds {
		if _, err = infraprovider.Get().ExecExternalContainerCommand(frr, cmd); err != nil {
			return nil, fmt.Errorf("failed to assign %s to VRF %s: %w", info.frrInterface, vrfName, err)
		}
	}

	// Set container's default routes via FRR (for each address family)
	c := infraapi.ExternalContainer{Name: container.Name}

	var routeCmds [][]string
	for _, gwIP := range info.frrIPs {
		cmd := []string{"ip"}
		if utilnet.IsIPv6String(gwIP) {
			cmd = append(cmd, "-6")
		}
		cmd = append(cmd, "route", "replace", "default", "via", gwIP, "dev", info.containerInterface)
		routeCmds = append(routeCmds, cmd)
	}
	for _, cmd := range routeCmds {
		if _, err = infraprovider.Get().ExecExternalContainerCommand(c, cmd); err != nil {
			return nil, fmt.Errorf("failed to set default routes on container: %w", err)
		}
	}

	framework.Logf("IP-VRF external container setup complete: %s (IPs: %v, network: %s, VRF: %s)", container.Name, info.containerIPs, networkName, vrfName)
	return info, nil
}

// =============================================================================
// VTEP Utilities
// =============================================================================

// createVTEP creates a VTEP (VXLAN Tunnel Endpoint) custom resource for EVPN.
// The VTEP CR defines the IP range from which VTEP IPs are allocated for EVPN VXLAN tunnels.
//
// Parameters:
//   - f: Test framework (used to get client config)
//   - ictx: Infrastructure context for cleanup registration
//   - name: Name of the VTEP CR
//   - cidrs: CIDR ranges for VTEP IP allocation (supports dual-stack with 2 CIDRs)
//   - mode: VTEP mode - "Managed" (OVN-K allocates IPs) or "Unmanaged" (external provider)
func createVTEP(f *framework.Framework, ictx infraapi.Context, name string, cidrs []string, mode vtepv1.VTEPMode) error {
	client, err := vtepclientset.NewForConfig(f.ClientConfig())
	if err != nil {
		return fmt.Errorf("failed to create VTEP client: %w", err)
	}

	// Convert string CIDRs to vtepv1.CIDR type
	vtepCIDRs := make([]vtepv1.CIDR, len(cidrs))
	for i, cidr := range cidrs {
		vtepCIDRs[i] = vtepv1.CIDR(cidr)
	}

	vtep := &vtepv1.VTEP{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: vtepv1.VTEPSpec{
			CIDRs: vtepCIDRs,
			Mode:  mode,
		},
	}

	_, err = client.K8sV1().VTEPs().Create(context.Background(), vtep, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		framework.Logf("VTEP %s already exists, reusing", name)
	} else if err != nil {
		return fmt.Errorf("failed to create VTEP %s: %w", name, err)
	} else {
		framework.Logf("VTEP created: %s (CIDRs: %v, Mode: %s)", name, cidrs, mode)
	}

	// Skip cleanup for the shared VTEP — it is reused across tests and
	// never deleted (the cluster is ephemeral).
	if name != sharedNodeIPsVTEPName {
		ictx.AddCleanUpFn(func() error {
			err := client.K8sV1().VTEPs().Delete(context.Background(), name, metav1.DeleteOptions{})
			if err != nil && !apierrors.IsNotFound(err) {
				return err
			}
			return wait.PollUntilContextTimeout(context.Background(), 1*time.Second, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
				_, err := client.K8sV1().VTEPs().Get(ctx, name, metav1.GetOptions{})
				if apierrors.IsNotFound(err) {
					return true, nil
				}
				return false, err
			})
		})
	}
	return nil
}

// ensureVTEPLoopbackIPs seeds each node with a VTEP-reachable IP when the
// VTEP CIDRs are custom subnets that don't overlap with the node's existing
// InternalIPs. It allocates one IP per CIDR per node, adds it to the loopback
// interface, and waits for it to appear in host-cidrs. Once the VTEP CR is
// created the ovnkube-node EVPN controller picks these IPs up through the
// address manager and writes the k8s.ovn.org/vteps node annotation
// automatically; the caller should use waitForVTEPAccepted to confirm all
// nodes have been annotated.
//
// When node IPs already fall within the VTEP CIDRs (e.g. VTEP CIDRs match the
// node IP subnets) this is a no-op.
func ensureVTEPLoopbackIPs(
	f *framework.Framework,
	ictx infraapi.Context,
	vtepCIDRs []string,
) error {
	nodeList, err := f.ClientSet.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list nodes: %w", err)
	}

	if nodeIPsOverlapCIDRs(nodeList, vtepCIDRs) {
		return nil
	}

	var parsedCIDRs []*net.IPNet
	for _, cidr := range vtepCIDRs {
		_, ipNet, err := net.ParseCIDR(cidr)
		if err != nil {
			return fmt.Errorf("failed to parse VTEP CIDR %q: %w", cidr, err)
		}
		parsedCIDRs = append(parsedCIDRs, ipNet)
	}

	for i, node := range nodeList.Items {
		for _, ipNet := range parsedCIDRs {
			ip := incrementIP(ipNet.IP, i+1)
			if !ipNet.Contains(ip) {
				return fmt.Errorf("ran out of IPs in CIDR %s for node %s", ipNet, node.Name)
			}
			_, err := infraprovider.Get().ExecK8NodeCommand(node.Name, []string{"ip", "addr", "add", ip.String() + "/32", "dev", "lo"})
			if err != nil {
				return fmt.Errorf("failed to add VTEP IP %s to loopback on node %s: %w", ip, node.Name, err)
			}
			framework.Logf("Added VTEP IP %s/32 to loopback on node %s", ip, node.Name)
		}
		nodeName := node.Name
		allocatedIPs := make([]string, 0, len(parsedCIDRs))
		for _, ipNet := range parsedCIDRs {
			allocatedIPs = append(allocatedIPs, incrementIP(ipNet.IP, i+1).String())
		}
		err := wait.PollUntilContextTimeout(context.Background(), 1*time.Second, 30*time.Second, true, func(ctx context.Context) (bool, error) {
			n, err := f.ClientSet.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
			if err != nil {
				return false, err
			}
			hostCIDRs, err := util.ParseNodeHostCIDRs(n)
			if err != nil {
				return false, nil
			}
			for _, ip := range allocatedIPs {
				if !hostCIDRs.Has(ip + "/32") {
					return false, nil
				}
			}
			return true, nil
		})
		if err != nil {
			return fmt.Errorf("timed out waiting for VTEP IPs %v to appear in host-cidrs on node %s: %w", allocatedIPs, nodeName, err)
		}
		framework.Logf("VTEP IPs %v confirmed in host-cidrs on node %s", allocatedIPs, nodeName)
	}

	ictx.AddCleanUpFn(func() error {
		nodeList, err := f.ClientSet.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
		if err != nil {
			return err
		}
		for i, node := range nodeList.Items {
			for _, ipNet := range parsedCIDRs {
				ip := incrementIP(ipNet.IP, i+1)
				_, err = infraprovider.Get().ExecK8NodeCommand(node.Name, []string{"ip", "addr", "del", ip.String() + "/32", "dev", "lo"})
				if err != nil {
					return fmt.Errorf("failed to delete VTEP IP %s to loopback on node %s: %w", ip, node.Name, err)
				}
			}
		}
		return nil
	})

	return nil
}

// incrementIP returns a copy of ip with offset added. Works for both IPv4 and IPv6.
func incrementIP(baseIP net.IP, offset int) net.IP {
	ip := make(net.IP, len(baseIP))
	copy(ip, baseIP)
	for i := len(ip) - 1; i >= 0 && offset > 0; i-- {
		sum := int(ip[i]) + offset
		ip[i] = byte(sum % 256)
		offset = sum / 256
	}
	return ip
}

// nodeIPsOverlapCIDRs returns true if at least one node's InternalIP falls
// within one of the provided CIDRs.
func nodeIPsOverlapCIDRs(nodeList *corev1.NodeList, cidrStrings []string) bool {
	var cidrs []*net.IPNet
	for _, s := range cidrStrings {
		_, ipNet, err := net.ParseCIDR(s)
		if err != nil {
			continue
		}
		cidrs = append(cidrs, ipNet)
	}
	for _, node := range nodeList.Items {
		for _, addr := range node.Status.Addresses {
			if addr.Type != corev1.NodeInternalIP {
				continue
			}
			ip := net.ParseIP(addr.Address)
			if ip == nil {
				continue
			}
			for _, cidr := range cidrs {
				if cidr.Contains(ip) {
					return true
				}
			}
		}
	}
	return false
}

// waitForVTEPAccepted polls the VTEP status until the Accepted condition is True.
func waitForVTEPAccepted(f *framework.Framework, vtepName string) error {
	client, err := vtepclientset.NewForConfig(f.ClientConfig())
	if err != nil {
		return fmt.Errorf("failed to create VTEP client: %w", err)
	}

	return wait.PollUntilContextTimeout(context.Background(), 1*time.Second, 60*time.Second, true, func(ctx context.Context) (bool, error) {
		vtep, err := client.K8sV1().VTEPs().Get(ctx, vtepName, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		condition := meta.FindStatusCondition(vtep.Status.Conditions, "Accepted")
		if condition == nil {
			return false, nil
		}
		if condition.Status == metav1.ConditionTrue {
			framework.Logf("VTEP %s is healthy (Accepted=True)", vtepName)
			return true, nil
		}
		framework.Logf("VTEP %s Accepted=%s reason=%s: %s", vtepName, condition.Status, condition.Reason, condition.Message)
		return false, nil
	})
}

// subnetOffsetIP returns an IP at the given offset from the base of the subnet.
// For example, subnetOffsetIP("10.199.128.0/20", 101) returns "10.199.128.101".
func subnetOffsetIP(cidr string, offset int) string {
	ip, _, err := net.ParseCIDR(cidr)
	if err != nil {
		panic(fmt.Sprintf("subnetOffsetIP: invalid CIDR %q: %v", cidr, err))
	}
	ip = ip.To16()
	for i := len(ip) - 1; i >= 0 && offset > 0; i-- {
		sum := int(ip[i]) + offset%256
		ip[i] = byte(sum % 256)
		offset = offset/256 + sum/256
	}
	if ip.To4() != nil {
		return ip.To4().String()
	}
	return ip.String()
}

// =============================================================================
// FRRConfiguration Utilities
// =============================================================================

func getExternalFRRIP(frrContainerName string, ipFamilySet sets.Set[utilnet.IPFamily]) (string, error) {
	kindNetwork, err := infraprovider.Get().PrimaryNetwork()
	if err != nil {
		return "", err
	}
	frrNetIf, err := infraprovider.Get().GetExternalContainerNetworkInterface(infraapi.ExternalContainer{Name: frrContainerName}, kindNetwork)
	if err != nil {
		return "", err
	}

	var externalFRRIP string
	switch {
	case ipFamilySet.Has(utilnet.IPv4) && frrNetIf.IPv4 != "":
		externalFRRIP = frrNetIf.IPv4
	case ipFamilySet.Has(utilnet.IPv6) && frrNetIf.IPv6 != "":
		externalFRRIP = frrNetIf.IPv6
	default:
		return "", fmt.Errorf("can't find external FRR IP on kind network")
	}
	return externalFRRIP, nil
}

// createFRRConfiguration creates an FRRConfiguration CR for BGP peering with the external FRR.
// This is used by RouteAdvertisements to determine which neighbors to advertise routes to.
//
// For EVPN L3 IP-VRF, we don't need toReceive because:
// - External routes come via EVPN Type-5 and are imported via route-target matching in the VRF
// - The FRRConfiguration just provides the BGP neighbor definition and label for RA selector
//
// Parameters:
//   - ictx: Infrastructure context for cleanup registration
//   - name: Name of the FRRConfiguration CR
//   - namespace: Namespace for the FRRConfiguration (typically frr-k8s-system)
//   - localASN: BGP ASN for cluster-side routers in this CR
//   - neighborASN: BGP ASN of the external FRR neighbor
//   - neighborIP: IP address of the external FRR to peer with
//   - labels: Labels to apply to the FRRConfiguration (used by RouteAdvertisements selector)
func createFRRConfiguration(ictx infraapi.Context,
	name, namespace string,
	localASN, neighborASN int,
	neighborIP string,
	labels map[string]string) error {

	// Build labels string for YAML
	labelsYAML := ""
	for k, v := range labels {
		labelsYAML += fmt.Sprintf("    %s: %s\n", k, v)
	}

	// Generate FRRConfiguration YAML
	// No toReceive needed - EVPN routes come via l2vpn evpn address-family
	// and are imported via route-target matching in the VRF
	isEBGP := localASN != neighborASN
	rawConfigSection := ""
	if isEBGP {
		// For eBGP EVPN, inter-node routes transit through the external FRR peer and
		// arrive with the local ASN in the AS_PATH, which BGP rejects as a loop.
		// allowas-in must be configured via rawConfig because the FRRConfiguration CRD
		// does not expose this field, and direct vtysh injection gets overwritten when
		// frr-k8s reconciles.
		rawConfigSection = fmt.Sprintf(`  raw:
    priority: 10
    rawConfig: |
      router bgp %d
       address-family l2vpn evpn
        neighbor %s allowas-in
       exit-address-family
      !
`, localASN, neighborIP)
	}
	yaml := fmt.Sprintf(`apiVersion: frrk8s.metallb.io/v1beta1
kind: FRRConfiguration
metadata:
  name: %s
  namespace: %s
  labels:
%sspec:
  bgp:
    routers:
    - asn: %d
      neighbors:
      - address: %s
        asn: %d
        disableMP: true
%s`, name, namespace, labelsYAML, localASN, neighborIP, neighborASN, rawConfigSection)

	// Write to temp file
	tmpFile, err := os.CreateTemp("", "frrconfig-*.yaml")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.WriteString(yaml); err != nil {
		return fmt.Errorf("failed to write FRRConfiguration YAML: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("failed to close temp file: %w", err)
	}

	// Apply via kubectl
	_, err = e2ekubectl.RunKubectl(namespace, "create", "-f", tmpFile.Name())
	if err != nil {
		return fmt.Errorf("failed to create FRRConfiguration: %w", err)
	}

	// Register cleanup
	ictx.AddCleanUpFn(func() error {
		_, err := e2ekubectl.RunKubectl(namespace, "delete", "frrconfiguration", name, "--ignore-not-found")
		if err != nil {
			return fmt.Errorf("failed to delete FRRConfiguration %s: %w", name, err)
		}
		framework.Logf("FRRConfiguration deleted: %s", name)
		return nil
	})

	framework.Logf("FRRConfiguration created: %s (neighbor: %s)", name, neighborIP)
	return nil
}

// =============================================================================
// EVPN Test Helpers
// =============================================================================

// populateExternalContainerIPs sets IPv4/IPv6 fields on the container from agnhost info.
func populateExternalContainerIPs(c *infraapi.ExternalContainer, info *evpnContainerInfo) {
	ips4, ips6 := splitIPStringsByIPFamily(info.containerIPs)
	if len(ips4) > 0 {
		c.IPv4 = ips4[0]
	}
	if len(ips6) > 0 {
		c.IPv6 = ips6[0]
	}
}

// runEVPNNetworkAndServers sets up the full EVPN test infrastructure: bridge,
// MAC-VRF and/or IP-VRF on the external FRR, BGP peering, VTEP CR, and
// FRRConfiguration. It creates external containers using the caller-provided
// ExternalContainer specs (image, command, etc.).
//
// The macVRFContainer and ipVRFContainer pointers are mutated: after creation
// their IPv4/IPv6 fields are populated with the discovered container IPs.
//
// When macVRFUseProxyDockerNetwork is true, the MAC-VRF Docker network is created
// without explicit subnets (Docker picks a random range) and the container's
// IPs are reassigned afterward to match the CUDN subnets. This is needed when
// two MAC-VRF containers share the same CUDN subnet because Docker rejects
// overlapping subnets.
func runEVPNNetworkAndServers(
	f *framework.Framework,
	ictx infraapi.Context,
	testName string,
	ipFamilySet sets.Set[utilnet.IPFamily],
	networkSpec *udnv1.NetworkSpec,
	bgpAlloc allocators.BGPAllocation,
	externalASN int,
	clusterASN int,
	frrContainerName string,
	bridgeName string,
	vxlanName string,
	vtepName string,
	macVRFContainer *infraapi.ExternalContainer,
	macVRFNetworkName string,
	ipVRFContainer *infraapi.ExternalContainer,
	ipVRFNetworkName string,
	macVRFUseProxyDockerNetwork bool,
) error {
	// Derive what to setup from networkSpec
	hasMACVRF := networkSpec.EVPN != nil && networkSpec.EVPN.MACVRF != nil
	hasIPVRF := networkSpec.EVPN != nil && networkSpec.EVPN.IPVRF != nil
	isIBGP := externalASN == clusterASN

	ipVRFAgnhostSubnets := matchCIDRStringsByIPFamilySet([]string{bgpAlloc.IPVRFSubnet, bgpAlloc.IPVRFSubnet6}, ipFamilySet)
	vtepSubnets := matchCIDRStringsByIPFamilySet([]string{bgpAlloc.VTEPSubnet, bgpAlloc.VTEPSubnet6}, ipFamilySet)

	cudnSubnetsFromSpec := getNetworkSubnetsFromSpec(networkSpec)

	externalFRRIP, err := getExternalFRRIP(frrContainerName, ipFamilySet)
	if err != nil {
		return err
	}

	nodeList, err := f.ClientSet.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list nodes: %w", err)
	}
	nodeIPs := e2enode.CollectAddresses(nodeList, corev1.NodeInternalIP)

	framework.Logf("Setting up EVPN bridge on external FRR (%s)", frrContainerName)
	err = setupEVPNBridgeOnExternalFRR(ictx, frrContainerName, externalFRRIP, bridgeName, vxlanName)
	if err != nil {
		return err
	}

	var macVRFVID int
	if hasMACVRF {
		macVRFVID = bgpAlloc.MACVRFVID
		framework.Logf("Allocated VIDs for external FRR: MAC-VRF VID=%d", macVRFVID)
		framework.Logf("Setting up MAC-VRF on external FRR")
		err = setupMACVRFOnExternalFRR(ictx, frrContainerName, int(networkSpec.EVPN.MACVRF.VNI), macVRFVID, bridgeName, vxlanName)
		if err != nil {
			return err
		}

		framework.Logf("Creating MAC-VRF external container")
		macVRFInfo, err := setupMACVRFExternalContainer(ictx, frrContainerName, *macVRFContainer, macVRFNetworkName, bridgeName, macVRFVID, ipFamilySet, cudnSubnetsFromSpec, macVRFUseProxyDockerNetwork)
		if err != nil {
			return err
		}
		populateExternalContainerIPs(macVRFContainer, macVRFInfo)
	}

	framework.Logf("Setting up EVPN BGP on external FRR (%s)", frrContainerName)
	err = setupEVPNBGPOnExternalFRR(ictx, frrContainerName, externalASN, nodeIPs, isIBGP)
	if err != nil {
		return err
	}

	if hasMACVRF && !isIBGP {
		macVRFVNI := int(networkSpec.EVPN.MACVRF.VNI)
		rt := fmt.Sprintf("%d:%d", clusterASN, macVRFVNI)
		framework.Logf("Overriding MAC-VRF VNI %d RT on external FRR to %s (eBGP RT alignment)", macVRFVNI, rt)
		frr := infraapi.ExternalContainer{Name: frrContainerName}
		_, err = infraprovider.Get().ExecExternalContainerCommand(frr, vtyshCommand(
			"configure terminal",
			fmt.Sprintf("router bgp %d", externalASN),
			"address-family l2vpn evpn",
			fmt.Sprintf("vni %d", macVRFVNI),
			fmt.Sprintf("rd %s", rt),
			fmt.Sprintf("route-target import %s", rt),
			fmt.Sprintf("route-target export %s", rt),
			"exit-vni",
			"exit-address-family",
			"end",
		))
		if err != nil {
			return fmt.Errorf("failed to set explicit MAC-VRF RT for eBGP: %w", err)
		}
	}

	if hasIPVRF {
		ipVRFName := fmt.Sprintf("vrf%d", networkSpec.EVPN.IPVRF.VNI)
		ipVRFVID := bgpAlloc.IPVRFVID
		framework.Logf("Allocated VIDs for external FRR: IP-VRF VID=%d", ipVRFVID)
		framework.Logf("Setting up IP-VRF on external FRR")
		err = setupIPVRFOnExternalFRR(ictx, frrContainerName, ipVRFName, int(networkSpec.EVPN.IPVRF.VNI), ipVRFVID, bridgeName, vxlanName)
		if err != nil {
			return err
		}

		framework.Logf("Creating IP-VRF external container")
		ipVRFInfo, err := setupIPVRFExternalContainer(ictx, frrContainerName, *ipVRFContainer, ipVRFNetworkName, ipVRFName, ipVRFVID, ipFamilySet, ipVRFAgnhostSubnets...)
		if err != nil {
			return err
		}
		populateExternalContainerIPs(ipVRFContainer, ipVRFInfo)

		framework.Logf("Setting up IP-VRF BGP on external FRR")
		err = setupIPVRFBGPOnExternalFRR(ictx, frrContainerName, ipVRFName, externalASN, clusterASN, int(networkSpec.EVPN.IPVRF.VNI), ipFamilySet, ipVRFAgnhostSubnets)
		if err != nil {
			return err
		}
	}

	framework.Logf("Ensuring VTEP loopback IPs on nodes")
	err = ensureVTEPLoopbackIPs(f, ictx, vtepSubnets)
	if err != nil {
		return err
	}

	framework.Logf("Creating VTEP CR %s with subnets %v", vtepName, vtepSubnets)
	err = createVTEP(f, ictx, vtepName, vtepSubnets, vtepv1.VTEPModeUnmanaged)
	if err != nil {
		return err
	}

	framework.Logf("Waiting for VTEP %s to be accepted", vtepName)
	err = waitForVTEPAccepted(f, vtepName)
	if err != nil {
		return fmt.Errorf("VTEP %s did not become healthy: %w", vtepName, err)
	}

	networkSpec.EVPN.VTEP = vtepName

	framework.Logf("Creating FRRConfiguration for EVPN")
	frrConfigLabels := map[string]string{"network": testName}
	err = createFRRConfiguration(ictx, testName, deploymentconfig.Get().FRRK8sNamespace(), clusterASN, externalASN, externalFRRIP, frrConfigLabels)
	if err != nil {
		return err
	}

	return nil
}


// evpnType2MACIPNLRI is the FRR/BGP print form of a Type-2 (MAC/IP) EVPN route NLRI.
func evpnType2MACIPNLRI(mac, hostIP string, fam utilnet.IPFamily) string {
	if fam == utilnet.IPv4 {
		return fmt.Sprintf("[2]:[0]:[48]:[%s]:[32]:[%s]", mac, hostIP)
	}
	return fmt.Sprintf("[2]:[0]:[48]:[%s]:[128]:[%s]", mac, hostIP)
}

// evpnType3MulticastNLRI is the FRR/BGP print form of a Type-3 (Inclusive Multicast) EVPN route NLRI.
func evpnType3MulticastNLRI(vtepIP string) string {
	if utilnet.IsIPv6String(vtepIP) {
		return fmt.Sprintf("[3]:[0]:[128]:[%s]", vtepIP)
	}
	return fmt.Sprintf("[3]:[0]:[32]:[%s]", vtepIP)
}
