// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package cni

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/100"
	"github.com/containernetworking/cni/pkg/version"
	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/nclient4"
	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	ovncnitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni/types"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	netlink_mocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/mocks/github.com/vishvananda/netlink"
	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	util_mocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/mocks"
)

// TestDHCPClientID verifies the RFC 2132 Client-ID (option 61) construction:
// a 1-byte hardware type (0x01 = Ethernet) followed by the MAC. The one-shot
// DORA must present the same client identity the guest's DHCP client will use
// after boot, so the DHCP server hands the guest this very lease instead of
// allocating a different IP.
func TestDHCPClientID(t *testing.T) {
	mac := ovntest.MustParseMAC("02:00:00:00:20:0b")
	assert.Equal(t, append([]byte{0x01}, mac...), dhcpClientID(mac))
}

// TestClasslessRoutesToCNIRoutes covers the option-121 (RFC 3442) handling at
// our boundary: well-formed payloads are parsed by the dhcpv4 library and
// converted to CNI routes; malformed payloads — authored by the external,
// untrusted DHCP server — must be rejected wholesale (fail-closed), never
// half-parsed into bogus routes (a prefix length > 32 used to yield a
// nil-mask route that masqueraded as a default route).
func TestClasslessRoutesToCNIRoutes(t *testing.T) {
	// fromOption121 runs raw option-121 bytes through the same library path
	// acquireDHCPLease uses on the ACK: Options.Get → Routes.FromBytes.
	fromOption121 := func(payload []byte) []*cnitypes.Route {
		ack := &dhcpv4.DHCPv4{
			Options: dhcpv4.OptionsFromList(
				dhcpv4.OptGeneric(dhcpv4.OptionClasslessStaticRoute, payload),
			),
		}
		return classlessRoutesToCNIRoutes(ack.ClasslessStaticRoute())
	}

	tests := []struct {
		desc     string
		payload  []byte
		expected []*cnitypes.Route
	}{
		{
			desc:     "no option payload yields no routes",
			payload:  nil,
			expected: nil,
		},
		{
			desc:    "single /24 route",
			payload: []byte{24, 192, 168, 5, 10, 0, 0, 1},
			expected: []*cnitypes.Route{
				{
					Dst: net.IPNet{IP: net.IPv4(192, 168, 5, 0).To4(), Mask: net.CIDRMask(24, 32)},
					GW:  net.IPv4(10, 0, 0, 1).To4(),
				},
			},
		},
		{
			desc:    "default route (mask width 0)",
			payload: []byte{0, 10, 0, 0, 1},
			expected: []*cnitypes.Route{
				{
					Dst: net.IPNet{IP: net.IPv4(0, 0, 0, 0).To4(), Mask: net.CIDRMask(0, 32)},
					GW:  net.IPv4(10, 0, 0, 1).To4(),
				},
			},
		},
		{
			desc: "multiple routes",
			payload: []byte{
				24, 10, 220, 2, 10, 220, 2, 1, // 10.220.2.0/24 via 10.220.2.1
				16, 172, 16, 172, 16, 0, 1, // 172.16.0.0/16 via 172.16.0.1
			},
			expected: []*cnitypes.Route{
				{
					Dst: net.IPNet{IP: net.IPv4(10, 220, 2, 0).To4(), Mask: net.CIDRMask(24, 32)},
					GW:  net.IPv4(10, 220, 2, 1).To4(),
				},
				{
					Dst: net.IPNet{IP: net.IPv4(172, 16, 0, 0).To4(), Mask: net.CIDRMask(16, 32)},
					GW:  net.IPv4(172, 16, 0, 1).To4(),
				},
			},
		},
		{
			desc:     "truncated payload is rejected wholesale",
			payload:  []byte{24, 192, 168},
			expected: nil,
		},
		{
			desc: "valid route followed by a truncated one rejects the whole option",
			payload: []byte{
				8, 10, 10, 0, 0, 1, // 10.0.0.0/8 via 10.0.0.1
				24, 192, // truncated
			},
			expected: nil,
		},
		{
			desc:     "prefix length greater than 32 is rejected wholesale",
			payload:  []byte{33, 192, 168, 5, 0, 0, 10, 0, 0, 1},
			expected: nil,
		},
		{
			// an unspecified router means an on-link route (RFC 3442); it
			// must carry SCOPE_LINK, as the delegated dhcp plugin reports it
			desc:    "on-link route (unspecified router) is marked SCOPE_LINK",
			payload: []byte{24, 192, 168, 5, 0, 0, 0, 0},
			expected: []*cnitypes.Route{
				{
					Dst:   net.IPNet{IP: net.IPv4(192, 168, 5, 0).To4(), Mask: net.CIDRMask(24, 32)},
					GW:    net.IPv4zero.To4(),
					Scope: ptr.To(int(netlink.SCOPE_LINK)),
				},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			routes := fromOption121(tc.payload)
			require.Len(t, routes, len(tc.expected))
			for i, expected := range tc.expected {
				assert.True(t, expected.Dst.IP.Equal(routes[i].Dst.IP),
					"route %d dst IP: expected %s got %s", i, expected.Dst.IP, routes[i].Dst.IP)
				assert.Equal(t, expected.Dst.Mask.String(), routes[i].Dst.Mask.String(), "route %d dst mask", i)
				assert.True(t, expected.GW.Equal(routes[i].GW),
					"route %d gw: expected %s got %s", i, expected.GW, routes[i].GW)
				assert.Equal(t, expected.Scope, routes[i].Scope, "route %d scope", i)
			}
		})
	}
}

// TestDHCPACKToCNIResult covers the ACK→CNI-result conversion, which must
// mirror the delegated dhcp IPAM plugin (plugins/ipam/dhcp) so pods and VMs
// report identical results for the same lease: a mask-less ACK is an error
// (never a made-up prefix), the option-3 router is always the IPConfig
// gateway, and per RFC 3442 option 121 replaces the router-derived default
// route entirely.
func TestDHCPACKToCNIResult(t *testing.T) {
	const ifName = "net1"

	newACK := func(yiaddr net.IP, opts ...dhcpv4.Option) *dhcpv4.DHCPv4 {
		ack := &dhcpv4.DHCPv4{Options: dhcpv4.OptionsFromList(opts...)}
		if yiaddr != nil {
			ack.YourIPAddr = yiaddr
		}
		return ack
	}
	yiaddr := net.IPv4(10, 1, 192, 213)
	mask := dhcpv4.OptSubnetMask(net.CIDRMask(24, 32))
	router := dhcpv4.OptRouter(net.IPv4(10, 1, 192, 1))

	t.Run("full ACK yields IP, gateway and an explicit default route", func(t *testing.T) {
		result, err := dhcpACKToCNIResult(ifName, newACK(yiaddr, mask, router))
		require.NoError(t, err)
		require.Len(t, result.IPs, 1)
		assert.Equal(t, "10.1.192.213/24", result.IPs[0].Address.String())
		assert.Equal(t, "10.1.192.1", result.IPs[0].Gateway.String())
		// upstream appends the router as a default route when option 121 is absent
		require.Len(t, result.Routes, 1)
		assert.Equal(t, "0.0.0.0/0", result.Routes[0].Dst.String())
		assert.Equal(t, "10.1.192.1", result.Routes[0].GW.String())
	})

	t.Run("missing subnet mask is an error, like the delegated dhcp plugin", func(t *testing.T) {
		_, err := dhcpACKToCNIResult(ifName, newACK(yiaddr, router))
		require.ErrorContains(t, err, "Subnet Mask not found in DHCPACK")
	})

	t.Run("malformed subnet mask is an error", func(t *testing.T) {
		badMask := dhcpv4.OptGeneric(dhcpv4.OptionSubnetMask, []byte{255, 255})
		_, err := dhcpACKToCNIResult(ifName, newACK(yiaddr, badMask, router))
		require.ErrorContains(t, err, "Subnet Mask not found in DHCPACK")
	})

	t.Run("option 121 replaces the router-derived default route (RFC 3442)", func(t *testing.T) {
		opt121 := dhcpv4.OptGeneric(dhcpv4.OptionClasslessStaticRoute,
			[]byte{24, 192, 168, 5, 10, 0, 0, 1}) // 192.168.5.0/24 via 10.0.0.1
		result, err := dhcpACKToCNIResult(ifName, newACK(yiaddr, mask, router, opt121))
		require.NoError(t, err)
		// the router is still reported as the IPConfig gateway, as upstream does
		assert.Equal(t, "10.1.192.1", result.IPs[0].Gateway.String())
		require.Len(t, result.Routes, 1, "option 121 must be the entire route set")
		assert.Equal(t, "192.168.5.0/24", result.Routes[0].Dst.String())
	})

	t.Run("no router yields no gateway and no default route", func(t *testing.T) {
		result, err := dhcpACKToCNIResult(ifName, newACK(yiaddr, mask))
		require.NoError(t, err)
		assert.Nil(t, result.IPs[0].Gateway)
		assert.Empty(t, result.Routes)
	})

	t.Run("ACK without an IP is an error", func(t *testing.T) {
		_, err := dhcpACKToCNIResult(ifName, newACK(nil, mask, router))
		require.ErrorContains(t, err, "has no IP address")
	})
}
func TestBuildDHCPConf(t *testing.T) {
	pr := &PodRequest{
		CNIConf: &ovncnitypes.NetConf{
			NetConf: cnitypes.NetConf{
				CNIVersion: "1.0.0",
				Name:       "localnet-dhcp",
			},
		},
	}

	confBytes, err := pr.buildDHCPConf()
	require.NoError(t, err)

	conf := map[string]any{}
	require.NoError(t, json.Unmarshal(confBytes, &conf))
	assert.Equal(t, "1.0.0", conf["cniVersion"])
	assert.Equal(t, "localnet-dhcp", conf["name"])
	assert.Equal(t, "ovn-k8s-cni-overlay", conf["type"])
	ipam, ok := conf["ipam"].(map[string]any)
	require.True(t, ok, "ipam section must be an object")
	assert.Equal(t, "dhcp", ipam["type"])
}

func TestGetCNIPath(t *testing.T) {
	t.Setenv("CNI_PATH", "/custom/cni/bin")
	assert.Equal(t, "/custom/cni/bin", getCNIPath())

	t.Setenv("CNI_PATH", "")
	assert.Equal(t, "/opt/cni/bin", getCNIPath())
}

func TestMergeDHCPResultIntoCNIResult(t *testing.T) {
	newDHCPResult := func() *current.Result {
		return &current.Result{
			IPs: []*current.IPConfig{
				{
					Address: net.IPNet{IP: net.IPv4(10, 1, 192, 213), Mask: net.CIDRMask(24, 32)},
					Gateway: net.IPv4(10, 1, 192, 1),
				},
			},
			Routes: []*cnitypes.Route{
				{Dst: net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)}, GW: net.IPv4(10, 1, 192, 1)},
			},
		}
	}

	t.Run("assigns the sandbox interface index", func(t *testing.T) {
		result := &current.Result{
			Interfaces: []*current.Interface{
				{Name: "host_eth0"},
				{Name: "eth0", Sandbox: "/var/run/netns/foo"},
			},
		}
		mergeDHCPResultIntoCNIResult(newDHCPResult(), result)
		require.Len(t, result.IPs, 1)
		require.NotNil(t, result.IPs[0].Interface)
		assert.Equal(t, 1, *result.IPs[0].Interface)
		assert.Len(t, result.Routes, 1)
	})

	t.Run("leaves the interface index unset with an empty interfaces list", func(t *testing.T) {
		result := &current.Result{}
		mergeDHCPResultIntoCNIResult(newDHCPResult(), result)
		require.Len(t, result.IPs, 1)
		assert.Nil(t, result.IPs[0].Interface,
			"an index into an empty interfaces list would be invalid per the CNI spec")
	})

	t.Run("leaves the interface index unset with host-side interfaces only", func(t *testing.T) {
		// Defensive: no current production path produces this shape —
		// ConfigureInterface always reports a sandbox-bearing container
		// interface, including a synthetic one for VFIO — but if a future
		// path ever omits it, the DHCP IP must not be attributed to a
		// host-side interface.
		result := &current.Result{
			Interfaces: []*current.Interface{
				{Name: "host_rep0"},
			},
		}
		mergeDHCPResultIntoCNIResult(newDHCPResult(), result)
		require.Len(t, result.IPs, 1)
		assert.Nil(t, result.IPs[0].Interface,
			"the DHCP IP must not be attributed to a host-side interface")
	})

	t.Run("appends to existing IPs and routes", func(t *testing.T) {
		result := &current.Result{
			IPs: []*current.IPConfig{
				{Address: net.IPNet{IP: net.IPv4(100, 10, 10, 3), Mask: net.CIDRMask(24, 32)}},
			},
			Routes: []*cnitypes.Route{
				{Dst: net.IPNet{IP: net.IPv4(100, 10, 0, 0), Mask: net.CIDRMask(16, 32)}},
			},
		}
		mergeDHCPResultIntoCNIResult(newDHCPResult(), result)
		assert.Len(t, result.IPs, 2)
		assert.Len(t, result.Routes, 2)
	})
}

func TestShouldRunLocalnetVFIODriverHandoff(t *testing.T) {
	validPR := func() *PodRequest {
		return &PodRequest{
			IsVFIO:  true,
			netName: "localnet-net",
			nadName: "foo-ns/localnet-nad",
			CNIConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{},
				DeviceID: "0000:65:00.2",
				Topology: ovntypes.LocalnetTopology,
			},
		}
	}

	tests := []struct {
		desc     string
		mutate   func(*PodRequest) *PodRequest
		expected bool
	}{
		{
			desc:     "all conditions met",
			mutate:   func(pr *PodRequest) *PodRequest { return pr },
			expected: true,
		},
		{
			desc:     "nil pod request",
			mutate:   func(*PodRequest) *PodRequest { return nil },
			expected: false,
		},
		{
			desc:     "nil CNI conf",
			mutate:   func(pr *PodRequest) *PodRequest { pr.CNIConf = nil; return pr },
			expected: false,
		},
		{
			desc:     "no device ID",
			mutate:   func(pr *PodRequest) *PodRequest { pr.CNIConf.DeviceID = ""; return pr },
			expected: false,
		},
		{
			desc:     "not VFIO",
			mutate:   func(pr *PodRequest) *PodRequest { pr.IsVFIO = false; return pr },
			expected: false,
		},
		{
			desc:     "default network name",
			mutate:   func(pr *PodRequest) *PodRequest { pr.netName = ovntypes.DefaultNetworkName; return pr },
			expected: false,
		},
		{
			desc:     "default NAD name",
			mutate:   func(pr *PodRequest) *PodRequest { pr.nadName = ovntypes.DefaultNetworkName; return pr },
			expected: false,
		},
		{
			desc:     "layer2 topology",
			mutate:   func(pr *PodRequest) *PodRequest { pr.CNIConf.Topology = ovntypes.Layer2Topology; return pr },
			expected: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			assert.Equal(t, tc.expected, tc.mutate(validPR()).shouldRunLocalnetVFIODriverHandoff())
		})
	}
}

// fakeSysfsPCI builds a fake /sys/bus/pci tree with the given device bound to
// boundDriver (unbound if empty) and the listed drivers available, and points
// the package sysfs roots at it for the duration of the test.
func fakeSysfsPCI(t *testing.T, deviceID, boundDriver string, drivers ...string) (devicesRoot, driversRoot string) {
	t.Helper()
	root := t.TempDir()
	devicesRoot = filepath.Join(root, "devices")
	driversRoot = filepath.Join(root, "drivers")

	deviceDir := filepath.Join(devicesRoot, deviceID)
	require.NoError(t, os.MkdirAll(deviceDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(deviceDir, "driver_override"), []byte("\n"), 0o644))
	// default to an NVIDIA/Mellanox VF; tests exercising other vendors
	// overwrite this file
	require.NoError(t, os.WriteFile(filepath.Join(deviceDir, "vendor"), []byte(pciVendorMellanox+"\n"), 0o644))

	for _, driver := range drivers {
		driverDir := filepath.Join(driversRoot, driver)
		require.NoError(t, os.MkdirAll(driverDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(driverDir, "bind"), nil, 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(driverDir, "unbind"), nil, 0o644))
	}

	if boundDriver != "" {
		require.NoError(t, os.Symlink(filepath.Join(driversRoot, boundDriver), filepath.Join(deviceDir, "driver")))
	}

	oldDevices, oldDrivers := sysBusPCIDevices, sysBusPCIDrivers
	sysBusPCIDevices, sysBusPCIDrivers = devicesRoot, driversRoot
	t.Cleanup(func() {
		sysBusPCIDevices, sysBusPCIDrivers = oldDevices, oldDrivers
	})
	return devicesRoot, driversRoot
}

func TestGetPCIDeviceDriver(t *testing.T) {
	const deviceID = "0000:65:00.2"

	t.Run("returns the bound driver", func(t *testing.T) {
		fakeSysfsPCI(t, deviceID, vfioPCIDriver, vfioPCIDriver)
		driver, err := getPCIDeviceDriver(deviceID)
		require.NoError(t, err)
		assert.Equal(t, vfioPCIDriver, driver)
	})

	t.Run("returns empty for an unbound device", func(t *testing.T) {
		fakeSysfsPCI(t, deviceID, "")
		driver, err := getPCIDeviceDriver(deviceID)
		require.NoError(t, err)
		assert.Empty(t, driver)
	})
}

// TestVFKernelDriverForDevice covers the vendor→driver detection for the
// VFIO DHCP handoff: the kernel driver must match the VF hardware, and
// unsupported vendors must be rejected explicitly (OKEP-6224) instead of
// force-probing a driver that cannot serve the device.
func TestVFKernelDriverForDevice(t *testing.T) {
	const deviceID = "0000:65:00.2"

	setVendor := func(t *testing.T, devicesRoot, vendor string) {
		t.Helper()
		require.NoError(t, os.WriteFile(filepath.Join(devicesRoot, deviceID, "vendor"), []byte(vendor+"\n"), 0o644))
	}

	t.Run("maps an NVIDIA/Mellanox VF to mlx5_core", func(t *testing.T) {
		fakeSysfsPCI(t, deviceID, vfioPCIDriver, vfioPCIDriver)
		driver, err := getVfKernelDriverForDevice(deviceID)
		require.NoError(t, err)
		assert.Equal(t, mlx5CoreDriver, driver)
	})

	t.Run("rejects an unsupported vendor", func(t *testing.T) {
		devicesRoot, _ := fakeSysfsPCI(t, deviceID, vfioPCIDriver, vfioPCIDriver)
		setVendor(t, devicesRoot, "0x8086") // Intel
		_, err := getVfKernelDriverForDevice(deviceID)
		require.ErrorContains(t, err, "unsupported NIC vendor 0x8086")
	})

	t.Run("errors when the vendor attribute is unreadable", func(t *testing.T) {
		devicesRoot, _ := fakeSysfsPCI(t, deviceID, vfioPCIDriver, vfioPCIDriver)
		require.NoError(t, os.Remove(filepath.Join(devicesRoot, deviceID, "vendor")))
		_, err := getVfKernelDriverForDevice(deviceID)
		require.ErrorContains(t, err, "failed to read PCI vendor")
	})
}

func TestWritePCIDeviceDriverOverride(t *testing.T) {
	const deviceID = "0000:65:00.2"
	devicesRoot, _ := fakeSysfsPCI(t, deviceID, "")
	overridePath := filepath.Join(devicesRoot, deviceID, "driver_override")

	require.NoError(t, writePCIDeviceDriverOverride(deviceID, mlx5CoreDriver))
	content, err := os.ReadFile(overridePath)
	require.NoError(t, err)
	assert.Equal(t, mlx5CoreDriver, string(content))

	// Clearing the override writes the kernel convention of a single newline.
	require.NoError(t, writePCIDeviceDriverOverride(deviceID, ""))
	content, err = os.ReadFile(overridePath)
	require.NoError(t, err)
	assert.Equal(t, "\n", string(content))
}

func TestBindPCIDeviceDriver(t *testing.T) {
	const deviceID = "0000:65:00.2"

	t.Run("no-op when already bound to the requested driver", func(t *testing.T) {
		_, driversRoot := fakeSysfsPCI(t, deviceID, vfioPCIDriver, vfioPCIDriver, mlx5CoreDriver)
		require.NoError(t, bindPCIDeviceDriver(deviceID, vfioPCIDriver))
		content, err := os.ReadFile(filepath.Join(driversRoot, vfioPCIDriver, "bind"))
		require.NoError(t, err)
		assert.Empty(t, string(content), "bind should not have been written")
	})

	t.Run("unbinds from the current driver and binds to the new one", func(t *testing.T) {
		devicesRoot, driversRoot := fakeSysfsPCI(t, deviceID, vfioPCIDriver, vfioPCIDriver, mlx5CoreDriver)

		require.NoError(t, bindPCIDeviceDriver(deviceID, mlx5CoreDriver))

		unbind, err := os.ReadFile(filepath.Join(driversRoot, vfioPCIDriver, "unbind"))
		require.NoError(t, err)
		assert.Equal(t, deviceID, string(unbind))

		bind, err := os.ReadFile(filepath.Join(driversRoot, mlx5CoreDriver, "bind"))
		require.NoError(t, err)
		assert.Equal(t, deviceID, string(bind))

		override, err := os.ReadFile(filepath.Join(devicesRoot, deviceID, "driver_override"))
		require.NoError(t, err)
		assert.Equal(t, "\n", string(override), "override must be cleared after binding")
	})

	t.Run("binds an unbound device without unbinding", func(t *testing.T) {
		_, driversRoot := fakeSysfsPCI(t, deviceID, "", mlx5CoreDriver)
		require.NoError(t, bindPCIDeviceDriver(deviceID, mlx5CoreDriver))
		bind, err := os.ReadFile(filepath.Join(driversRoot, mlx5CoreDriver, "bind"))
		require.NoError(t, err)
		assert.Equal(t, deviceID, string(bind))
	})

	t.Run("restores the original driver when the target driver is missing", func(t *testing.T) {
		devicesRoot, driversRoot := fakeSysfsPCI(t, deviceID, vfioPCIDriver, vfioPCIDriver)

		err := bindPCIDeviceDriver(deviceID, "no_such_driver")
		require.Error(t, err)

		// the device was unbound from vfio-pci before the failed bind; it must
		// be handed back to vfio-pci instead of being left bound to nothing
		restored, readErr := os.ReadFile(filepath.Join(driversRoot, vfioPCIDriver, "bind"))
		require.NoError(t, readErr)
		assert.Equal(t, deviceID, string(restored), "device must be rebound to its original driver on failure")

		override, readErr := os.ReadFile(filepath.Join(devicesRoot, deviceID, "driver_override"))
		require.NoError(t, readErr)
		assert.Equal(t, "\n", string(override), "override must be cleared on failure")
	})
}

// TestRunLocalnetVFIODHCPAlwaysRebindsVFIO verifies the driver-handoff safety
// ordering: the rebind-to-vfio-pci cleanup must run on every failure path that
// may have touched the driver binding — including a failed swap to the kernel
// driver — so the VF is never left driverless (kubelet's retried CNI ADD checks
// for vfio-pci and could otherwise never self-heal). It must NOT run when the
// precondition check fails before any driver state was touched.
func TestRunLocalnetVFIODHCPAlwaysRebindsVFIO(t *testing.T) {
	const deviceID = "0000:65:00.2"

	newVFIOPodRequest := func(t *testing.T) *PodRequest {
		t.Helper()
		pr := newDHCPPodRequest(t)
		pr.IsVFIO = true
		pr.CNIConf.DeviceID = deviceID
		return pr
	}

	stubBind := func(t *testing.T, swapErr error) *bindStubDHCPOps {
		t.Helper()
		stub := &bindStubDHCPOps{swapErr: swapErr}
		dhcpOps = stub
		t.Cleanup(func() { dhcpOps = &defaultDHCPOps{} })
		oldDir := vfioHandoffJournalDir
		vfioHandoffJournalDir = t.TempDir()
		t.Cleanup(func() { vfioHandoffJournalDir = oldDir })
		return stub
	}

	readOverride := func(t *testing.T, devicesRoot string) string {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(devicesRoot, deviceID, "driver_override"))
		require.NoError(t, err)
		return string(raw)
	}

	journalExists := func(t *testing.T) bool {
		t.Helper()
		_, err := os.Stat(vfioHandoffJournalPath(deviceID))
		if err != nil {
			require.True(t, os.IsNotExist(err))
			return false
		}
		return true
	}

	t.Run("rebinds vfio-pci when the swap to the kernel driver fails", func(t *testing.T) {
		devicesRoot, _ := fakeSysfsPCI(t, deviceID, vfioPCIDriver, vfioPCIDriver)
		stub := stubBind(t, fmt.Errorf("mlx5_core module not loaded"))

		_, err := newVFIOPodRequest(t).runLocalnetVFIODHCP()
		require.ErrorContains(t, err, "failed to bind device")

		require.Equal(t, []string{mlx5CoreDriver, vfioPCIDriver}, stub.calls,
			"the rebind must run even though the swap failed")
		require.Equal(t, vfioPCIDriver, readOverride(t, devicesRoot),
			"the vfio pin must be in place after the rebind")
		require.False(t, journalExists(t),
			"the journal must be removed once the device is back on vfio-pci")
	})

	t.Run("rebinds vfio-pci when netdev discovery fails after the swap", func(t *testing.T) {
		devicesRoot, _ := fakeSysfsPCI(t, deviceID, vfioPCIDriver, vfioPCIDriver)
		stub := stubBind(t, nil)

		// the fake device has no netdev, so getNetdevIndex fails
		_, err := newVFIOPodRequest(t).runLocalnetVFIODHCP()
		require.ErrorContains(t, err, "failed to find netdev")

		require.Equal(t, []string{mlx5CoreDriver, vfioPCIDriver}, stub.calls,
			"the rebind must run when a later step fails")
		require.Equal(t, vfioPCIDriver, readOverride(t, devicesRoot),
			"the successful rebind must leave the vfio pin in place")
		require.False(t, journalExists(t),
			"the journal must be removed once the device is back on vfio-pci")
	})

	t.Run("keeps the journal when the final rebind fails", func(t *testing.T) {
		fakeSysfsPCI(t, deviceID, vfioPCIDriver, vfioPCIDriver)
		stub := stubBind(t, nil)
		stub.vfioErr = fmt.Errorf("vfio-pci refused the device")

		_, err := newVFIOPodRequest(t).runLocalnetVFIODHCP()
		require.ErrorContains(t, err, "rebind")

		require.True(t, journalExists(t),
			"the journal must survive a failed rebind so the retried ADD repairs the device")
	})

	t.Run("does not touch driver bindings when the device is not on vfio-pci", func(t *testing.T) {
		devicesRoot, _ := fakeSysfsPCI(t, deviceID, mlx5CoreDriver, mlx5CoreDriver)
		stub := stubBind(t, nil)

		_, err := newVFIOPodRequest(t).runLocalnetVFIODHCP()
		require.ErrorContains(t, err, "is not bound to vfio-pci")

		require.Empty(t, stub.calls, "no driver operation must happen when the precondition fails")
		require.Equal(t, "\n", readOverride(t, devicesRoot),
			"the override must stay untouched when the precondition fails")
		require.False(t, journalExists(t),
			"no journal must be written when the precondition fails")
	})

	t.Run("does not touch driver bindings for an unsupported NIC vendor", func(t *testing.T) {
		devicesRoot, _ := fakeSysfsPCI(t, deviceID, vfioPCIDriver, vfioPCIDriver)
		require.NoError(t, os.WriteFile(filepath.Join(devicesRoot, deviceID, "vendor"), []byte("0x8086\n"), 0o644))
		stub := stubBind(t, nil)

		_, err := newVFIOPodRequest(t).runLocalnetVFIODHCP()
		require.ErrorContains(t, err, "unsupported NIC vendor")

		require.Empty(t, stub.calls, "the VF must stay untouched on vfio-pci when the vendor is unsupported")
		require.Equal(t, "\n", readOverride(t, devicesRoot),
			"the override must stay untouched when the vendor is unsupported")
		require.False(t, journalExists(t),
			"no journal must be written when the vendor is unsupported")
	})

	t.Run("rollback release is skipped when no netdev exists", func(t *testing.T) {
		fakeSysfsPCI(t, deviceID, "")
		// The retained probe lease is released on the rebind-failure rollback
		// only when a netdev survived (or was recreated by) the failed rebind.
		// With no netdev anywhere — the double-failure state where the device
		// is bound to nothing — the RELEASE cannot be transmitted: the helper
		// must return promptly without attempting one (the retried ADD
		// re-acquires the same lease via the same MAC-keyed client-id).
		// A canceled context makes the netdev re-resolve fail immediately.
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		lease := &nclient4.Lease{ACK: &dhcpv4.DHCPv4{YourIPAddr: net.IPv4(172, 18, 0, 5)}}
		releaseVFIOProbeLease(ctx, deviceID, lease, ovntest.MustParseMAC("02:00:00:00:20:0b"))
	})
}

// TestHealInterruptedVFIOHandoff covers the ADD-time crash repair: a handoff
// journal with the device bound anywhere but vfio-pci can only be the
// leftover of a handoff interrupted by a process crash, and the device must
// be rebound before the IsVFIO routing decision reads the binding — whatever
// intermediate driver state the crash froze (kernel driver or none at all).
// Without a journal nothing is touched, whatever the binding looks like.
func TestHealInterruptedVFIOHandoff(t *testing.T) {
	const deviceID = "0000:65:00.2"

	stubOps := func(t *testing.T) *bindStubDHCPOps {
		t.Helper()
		stub := &bindStubDHCPOps{}
		dhcpOps = stub
		t.Cleanup(func() { dhcpOps = &defaultDHCPOps{} })
		return stub
	}

	fakeJournalDir := func(t *testing.T) {
		t.Helper()
		oldDir := vfioHandoffJournalDir
		vfioHandoffJournalDir = t.TempDir()
		t.Cleanup(func() { vfioHandoffJournalDir = oldDir })
	}

	writeJournal := func(t *testing.T) {
		t.Helper()
		require.NoError(t, os.WriteFile(vfioHandoffJournalPath(deviceID),
			[]byte(`{"intendedDriver":"vfio-pci"}`), 0o600))
	}

	journalExists := func(t *testing.T) bool {
		t.Helper()
		_, err := os.Stat(vfioHandoffJournalPath(deviceID))
		if err != nil {
			require.True(t, os.IsNotExist(err))
			return false
		}
		return true
	}

	readOverride := func(t *testing.T, devicesRoot string) string {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(devicesRoot, deviceID, "driver_override"))
		require.NoError(t, err)
		return string(raw)
	}

	t.Run("repairs a kernel-bound device with a journal", func(t *testing.T) {
		// a crash after the kernel bind: by sysfs alone the device is
		// indistinguishable from a plain SR-IOV netdev VF
		devicesRoot, _ := fakeSysfsPCI(t, deviceID, mlx5CoreDriver, mlx5CoreDriver, vfioPCIDriver)
		fakeJournalDir(t)
		writeJournal(t)
		stub := stubOps(t)

		require.NoError(t, healInterruptedVFIOHandoff(deviceID))

		require.Equal(t, []string{vfioPCIDriver}, stub.calls, "the device must be rebound to vfio-pci")
		require.Equal(t, vfioPCIDriver, readOverride(t, devicesRoot), "the pin must be restored after the repair")
		require.False(t, journalExists(t), "the journal must be removed once the handoff is concluded")
	})

	t.Run("repairs a driverless device with a journal", func(t *testing.T) {
		// a crash between the unbind and the bind leaves no driver at all:
		// IsVFIO would read false yet no netdev exists — without the repair
		// no retry ever converges
		fakeSysfsPCI(t, deviceID, "", mlx5CoreDriver, vfioPCIDriver)
		fakeJournalDir(t)
		writeJournal(t)
		stub := stubOps(t)

		require.NoError(t, healInterruptedVFIOHandoff(deviceID))

		require.Equal(t, []string{vfioPCIDriver}, stub.calls,
			"the repair must fire for a driverless journaled device")
		require.False(t, journalExists(t))
	})

	t.Run("does not touch a regular kernel-bound VF without a journal", func(t *testing.T) {
		fakeSysfsPCI(t, deviceID, mlx5CoreDriver, mlx5CoreDriver)
		fakeJournalDir(t)
		stub := stubOps(t)

		require.NoError(t, healInterruptedVFIOHandoff(deviceID))

		require.Empty(t, stub.calls, "a device without a journal is not an interrupted handoff")
	})

	t.Run("cleans up a leftover journal on a healthy vfio-bound device", func(t *testing.T) {
		// a crash after the successful rebind but before the journal removal
		fakeSysfsPCI(t, deviceID, vfioPCIDriver, vfioPCIDriver)
		fakeJournalDir(t)
		writeJournal(t)
		stub := stubOps(t)

		require.NoError(t, healInterruptedVFIOHandoff(deviceID))

		require.Empty(t, stub.calls, "a device already on vfio-pci needs no rebind")
		require.False(t, journalExists(t), "the stale journal must be cleaned up")
	})

	t.Run("fails and keeps the journal when the repair rebind fails", func(t *testing.T) {
		fakeSysfsPCI(t, deviceID, mlx5CoreDriver, mlx5CoreDriver, vfioPCIDriver)
		fakeJournalDir(t)
		writeJournal(t)
		stub := stubOps(t)
		stub.vfioErr = fmt.Errorf("vfio-pci module not loaded")

		require.ErrorContains(t, healInterruptedVFIOHandoff(deviceID), "failed to restore")
		require.True(t, journalExists(t),
			"the journal must survive a failed repair so the next attempt fires again")
	})

	t.Run("is a no-op when the device has no sysfs entry and no journal", func(t *testing.T) {
		// only a different device exists in the fake sysfs tree
		fakeSysfsPCI(t, "0000:99:00.9", mlx5CoreDriver, mlx5CoreDriver)
		fakeJournalDir(t)
		stub := stubOps(t)

		require.NoError(t, healInterruptedVFIOHandoff(deviceID))
		require.Empty(t, stub.calls)
	})
}

// fakeDHCPExec implements invoke.Exec, recording plugin invocations and
// returning a canned CNI result for ADD.
type fakeDHCPExec struct {
	addResultJSON []byte
	addErr        error
	findErr       error
	calls         []fakeDHCPExecCall
}

type fakeDHCPExecCall struct {
	pluginPath string
	stdin      []byte
	command    string
}

func (f *fakeDHCPExec) ExecPlugin(_ context.Context, pluginPath string, stdinData []byte, environ []string) ([]byte, error) {
	command := ""
	for _, env := range environ {
		if strings.HasPrefix(env, "CNI_COMMAND=") {
			command = strings.TrimPrefix(env, "CNI_COMMAND=")
		}
	}
	f.calls = append(f.calls, fakeDHCPExecCall{pluginPath: pluginPath, stdin: stdinData, command: command})
	if command == "ADD" {
		return f.addResultJSON, f.addErr
	}
	return []byte("{}"), nil
}

func (f *fakeDHCPExec) FindInPath(plugin string, _ []string) (string, error) {
	if f.findErr != nil {
		return "", f.findErr
	}
	return "/opt/cni/bin/" + plugin, nil
}

func (f *fakeDHCPExec) Decode(jsonBytes []byte) (version.PluginInfo, error) {
	return (&version.PluginDecoder{}).Decode(jsonBytes)
}

func setFakeDHCPExec(t *testing.T, fake *fakeDHCPExec, apply func(netnsPath, ifName string, dhcpResult *current.Result) error) {
	t.Helper()
	dhcpPluginExec = fake
	if apply != nil {
		dhcpOps = &applyStubDHCPOps{apply: apply}
	}
	t.Cleanup(func() {
		dhcpPluginExec = nil
		dhcpOps = &defaultDHCPOps{}
	})
}

// bindStubDHCPOps overrides only the driver-bind seam of DHCPOps; everything
// else falls through to the real implementation.
type bindStubDHCPOps struct {
	defaultDHCPOps
	calls   []string
	swapErr error
	vfioErr error
}

func (b *bindStubDHCPOps) BindPCIDeviceDriver(_ string, driver string) error {
	b.calls = append(b.calls, driver)
	if driver == mlx5CoreDriver {
		return b.swapErr
	}
	if driver == vfioPCIDriver {
		return b.vfioErr
	}
	return nil
}

// applyStubDHCPOps overrides only the apply-result seam of DHCPOps.
type applyStubDHCPOps struct {
	defaultDHCPOps
	apply func(netnsPath, ifName string, dhcpResult *current.Result) error
}

func (a *applyStubDHCPOps) ApplyResult(netnsPath, ifName string, dhcpResult *current.Result) error {
	return a.apply(netnsPath, ifName, dhcpResult)
}

func newDHCPPodRequest(t *testing.T) *PodRequest {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return &PodRequest{
		Command:      CNIAdd,
		PodNamespace: "foo-ns",
		PodName:      "bar-pod",
		SandboxID:    "824bceff24af3",
		Netns:        "/var/run/netns/foo",
		IfName:       "net1",
		netName:      "localnet-net",
		nadName:      "foo-ns/localnet-nad",
		ctx:          ctx,
		CNIConf: &ovncnitypes.NetConf{
			NetConf: cnitypes.NetConf{
				CNIVersion: "1.0.0",
				Name:       "localnet-net",
				Type:       "ovn-k8s-cni-overlay",
				IPAM:       cnitypes.IPAM{Type: "dhcp"},
			},
			Topology: ovntypes.LocalnetTopology,
		},
	}
}

const dhcpADDResultJSON = `{
	"cniVersion": "1.0.0",
	"ips": [{"address": "10.1.192.213/24", "gateway": "10.1.192.1"}],
	"routes": [
		{"dst": "0.0.0.0/0", "gw": "10.1.192.1"},
		{"dst": "192.168.5.0/24", "gw": "10.1.192.1"}
	]
}`

func TestExecDHCPAdd(t *testing.T) {
	t.Run("returns the DHCP result and applies it in the pod netns", func(t *testing.T) {
		fake := &fakeDHCPExec{addResultJSON: []byte(dhcpADDResultJSON)}
		var appliedNetns, appliedIfName string
		var appliedResult *current.Result
		setFakeDHCPExec(t, fake, func(netnsPath, ifName string, dhcpResult *current.Result) error {
			appliedNetns, appliedIfName, appliedResult = netnsPath, ifName, dhcpResult
			return nil
		})

		pr := newDHCPPodRequest(t)

		dhcpResult, err := pr.execDHCPAdd(false)
		require.NoError(t, err)

		require.Len(t, fake.calls, 1)
		assert.Equal(t, "ADD", fake.calls[0].command)
		assert.Contains(t, string(fake.calls[0].stdin), `"dhcp"`)

		assert.Equal(t, pr.Netns, appliedNetns)
		assert.Equal(t, pr.IfName, appliedIfName)
		require.NotNil(t, appliedResult)

		require.Len(t, dhcpResult.IPs, 1)
		assert.Equal(t, "10.1.192.213/24", dhcpResult.IPs[0].Address.String())
		// the default route is dropped (secondary attachment must not compete
		// with the pod's primary network), the subnet route is kept — in both
		// the applied and the returned result
		require.Len(t, dhcpResult.Routes, 1)
		assert.Equal(t, "192.168.5.0/24", dhcpResult.Routes[0].Dst.String())
		require.Len(t, appliedResult.Routes, 1)
		assert.Equal(t, "192.168.5.0/24", appliedResult.Routes[0].Dst.String())
	})

	t.Run("keeps the default route when the attachment requested it", func(t *testing.T) {
		fake := &fakeDHCPExec{addResultJSON: []byte(dhcpADDResultJSON)}
		var appliedResult *current.Result
		setFakeDHCPExec(t, fake, func(_, _ string, dhcpResult *current.Result) error {
			appliedResult = dhcpResult
			return nil
		})

		pr := newDHCPPodRequest(t)

		// the network selection element carried the default-route key, so
		// this attachment owns the pod's default route (the primary yields it)
		dhcpResult, err := pr.execDHCPAdd(true)
		require.NoError(t, err)

		require.Len(t, dhcpResult.Routes, 2)
		assert.Equal(t, "0.0.0.0/0", dhcpResult.Routes[0].Dst.String())
		require.Len(t, appliedResult.Routes, 2)
		assert.Equal(t, "0.0.0.0/0", appliedResult.Routes[0].Dst.String())
	})

	t.Run("fails when DHCP returns no IPs", func(t *testing.T) {
		fake := &fakeDHCPExec{addResultJSON: []byte(`{"cniVersion": "1.0.0", "ips": []}`)}
		setFakeDHCPExec(t, fake, func(string, string, *current.Result) error {
			t.Fatal("apply must not be called without IPs")
			return nil
		})

		_, err := newDHCPPodRequest(t).execDHCPAdd(false)
		require.ErrorContains(t, err, "no IP addresses")
	})

	t.Run("no release when applying the result fails", func(t *testing.T) {
		fake := &fakeDHCPExec{addResultJSON: []byte(dhcpADDResultJSON)}
		setFakeDHCPExec(t, fake, func(string, string, *current.Result) error {
			return fmt.Errorf("netns exploded")
		})

		_, err := newDHCPPodRequest(t).execDHCPAdd(false)
		require.ErrorContains(t, err, "failed to apply DHCP result")

		// the lease is not released here: the runtime-issued DEL for the
		// failed sandbox releases it
		require.Len(t, fake.calls, 1)
		assert.Equal(t, "ADD", fake.calls[0].command)
	})

	t.Run("fails when the plugin errors", func(t *testing.T) {
		fake := &fakeDHCPExec{addErr: fmt.Errorf("daemon unreachable")}
		setFakeDHCPExec(t, fake, nil)

		_, err := newDHCPPodRequest(t).execDHCPAdd(false)
		require.ErrorContains(t, err, "failed to execute DHCP plugin ADD")
	})
}

func TestExecDHCPDel(t *testing.T) {
	fake := &fakeDHCPExec{}
	setFakeDHCPExec(t, fake, nil)

	pr := newDHCPPodRequest(t)
	require.NoError(t, pr.execDHCPDel())

	require.Len(t, fake.calls, 1)
	assert.Equal(t, "DEL", fake.calls[0].command)
	assert.Contains(t, string(fake.calls[0].stdin), `"dhcp"`)
}

// TestFilterOutDefaultRoutes covers the secondary-attachment route policy:
// DHCP IPAM networks are Secondary by CRD rule, so a default route handed out
// by the DHCP server must never compete with the pod's primary network —
// only subnet-scoped (e.g. option-121) routes are kept.
func TestFilterOutDefaultRoutes(t *testing.T) {
	defaultRoute := func() *cnitypes.Route {
		return &cnitypes.Route{
			Dst: net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)},
			GW:  net.IPv4(10, 1, 192, 1),
		}
	}
	subnetRoute := func() *cnitypes.Route {
		return &cnitypes.Route{
			Dst: net.IPNet{IP: net.IPv4(192, 168, 5, 0).To4(), Mask: net.CIDRMask(24, 32)},
			GW:  net.IPv4(10, 1, 192, 1),
		}
	}

	t.Run("drops default routes and keeps subnet routes", func(t *testing.T) {
		result := &current.Result{Routes: []*cnitypes.Route{defaultRoute(), subnetRoute()}}
		dropped := filterOutDefaultRoutes(result)
		require.Len(t, result.Routes, 1)
		assert.Equal(t, "192.168.5.0/24", result.Routes[0].Dst.String())
		require.Len(t, dropped, 1)
		assert.Equal(t, "0.0.0.0/0", dropped[0].Dst.String())
	})

	t.Run("only default routes yields an empty route list", func(t *testing.T) {
		result := &current.Result{Routes: []*cnitypes.Route{defaultRoute()}}
		dropped := filterOutDefaultRoutes(result)
		assert.Empty(t, result.Routes)
		assert.Len(t, dropped, 1)
	})

	t.Run("no routes is a no-op", func(t *testing.T) {
		result := &current.Result{}
		dropped := filterOutDefaultRoutes(result)
		assert.Empty(t, result.Routes)
		assert.Empty(t, dropped)
	})
}

// TestApplyDHCPResultInNS covers the route-programming half of the DHCP apply
// path against upstream ipam.ConfigureIface's contract: scope/priority/table
// from the result are honored (an on-link option-121 route must be installed
// scope link, not scope universe with gateway 0.0.0.0), EEXIST is tolerated
// for repeat-ADD idempotency, and any other failure is fatal so a pod missing
// its DHCP-advertised routes never reports a successful ADD.
func TestApplyDHCPResultInNS(t *testing.T) {
	const ifName = "net1"

	newResult := func() *current.Result {
		return &current.Result{
			IPs: []*current.IPConfig{{
				Address: net.IPNet{IP: net.IPv4(10, 1, 192, 213), Mask: net.CIDRMask(24, 32)},
				Gateway: net.IPv4(10, 1, 192, 1),
			}},
			Routes: []*cnitypes.Route{
				{
					Dst:      net.IPNet{IP: net.IPv4(192, 168, 5, 0).To4(), Mask: net.CIDRMask(24, 32)},
					GW:       net.IPv4zero.To4(),
					Scope:    ptr.To(int(netlink.SCOPE_LINK)),
					Priority: 100,
					Table:    ptr.To(42),
				},
			},
		}
	}

	setup := func(t *testing.T) *util_mocks.NetLinkOps {
		t.Helper()
		mockNetLinkOps := new(util_mocks.NetLinkOps)
		mockLink := new(netlink_mocks.Link)
		util.SetNetLinkOpMockInst(mockNetLinkOps)
		mockLink.On("Attrs").Return(&netlink.LinkAttrs{Index: 7, Name: ifName})
		mockNetLinkOps.On("LinkByName", ifName).Return(mockLink, nil)
		return mockNetLinkOps
	}

	t.Run("honors scope, priority and table from the result", func(t *testing.T) {
		mockNetLinkOps := setup(t)
		mockNetLinkOps.On("AddrAdd", mock.Anything, mock.Anything).Return(nil)
		var captured *netlink.Route
		mockNetLinkOps.On("RouteAdd", mock.Anything).Run(func(args mock.Arguments) {
			captured = args.Get(0).(*netlink.Route)
		}).Return(nil)

		require.NoError(t, applyDHCPResultInNS(ifName, newResult()))
		require.NotNil(t, captured)
		assert.Equal(t, netlink.SCOPE_LINK, captured.Scope, "on-link route must be installed scope link")
		assert.Equal(t, 100, captured.Priority)
		assert.Equal(t, 42, captured.Table)
		assert.Equal(t, 7, captured.LinkIndex)
	})

	t.Run("route add failure is fatal", func(t *testing.T) {
		mockNetLinkOps := setup(t)
		mockNetLinkOps.On("AddrAdd", mock.Anything, mock.Anything).Return(nil)
		mockNetLinkOps.On("RouteAdd", mock.Anything).Return(fmt.Errorf("network is unreachable"))

		err := applyDHCPResultInNS(ifName, newResult())
		require.ErrorContains(t, err, "failed to add route")
	})

	t.Run("existing route is tolerated for idempotency", func(t *testing.T) {
		mockNetLinkOps := setup(t)
		mockNetLinkOps.On("AddrAdd", mock.Anything, mock.Anything).Return(nil)
		mockNetLinkOps.On("RouteAdd", mock.Anything).Return(os.ErrExist)

		require.NoError(t, applyDHCPResultInNS(ifName, newResult()))
	})

	t.Run("addr add failure is fatal", func(t *testing.T) {
		mockNetLinkOps := setup(t)
		mockNetLinkOps.On("AddrAdd", mock.Anything, mock.Anything).Return(fmt.Errorf("permission denied"))

		err := applyDHCPResultInNS(ifName, newResult())
		require.ErrorContains(t, err, "failed to add IP")
	})
}

// newDHCPAnnotationTestPod builds a pod with the given multus networks
// annotation (empty string for none) for the MAC-annotation tests.
func newDHCPAnnotationTestPod(pr *PodRequest, networksAnnotation string) *corev1.Pod {
	annotations := map[string]string{}
	if networksAnnotation != "" {
		annotations[nadapi.NetworkAttachmentAnnot] = networksAnnotation
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        pr.PodName,
			Namespace:   pr.PodNamespace,
			Annotations: annotations,
		},
	}
}

// TestAllocateDHCPMACAnnotation covers the CNI-side single-writer bootstrap of the
// pod-networks entry for DHCP attachments: reuse of an existing entry's MAC
// (identity stability across sandbox recreations and retried ADDs, with no
// write needed), MacRequest support, random-MAC creation, and the exact
// MAC-only entry shape handed to cmdAdd to write before plumbing (role
// secondary, ipam_mode dhcp, no IPs).
func TestAllocateDHCPMACAnnotation(t *testing.T) {
	newPR := func() *PodRequest {
		return &PodRequest{
			PodNamespace: "foo-ns",
			PodName:      "bar-pod",
			nadName:      "default/dhcpnet",
			nadKey:       "default/dhcpnet",
		}
	}

	t.Run("reuses the MAC of an existing MAC-only entry, no write needed", func(t *testing.T) {
		pr := newPR()
		pod := newDHCPAnnotationTestPod(pr, "")
		entry := &util.PodAnnotation{
			MAC:      ovntest.MustParseMAC("02:00:00:00:20:0b"),
			Role:     ovntypes.NetworkRoleSecondary,
			IPAMMode: ovntypes.IPAMTypeDHCP,
		}
		var err error
		pod.Annotations, err = util.MarshalPodAnnotation(pod.Annotations, entry, pr.nadKey)
		require.NoError(t, err)

		got, needsWrite, err := pr.allocateDHCPMACAnnotation(pod)
		require.NoError(t, err)
		assert.False(t, needsWrite)
		assert.Equal(t, entry.MAC.String(), got.MAC.String())
	})

	t.Run("clears the previous lease from an existing entry, keeping the MAC", func(t *testing.T) {
		pr := newPR()
		pod := newDHCPAnnotationTestPod(pr, "")
		entry := &util.PodAnnotation{
			IPs:      ovntest.MustParseIPNets("10.1.192.102/24"),
			MAC:      ovntest.MustParseMAC("02:00:00:00:20:0b"),
			Gateways: ovntest.MustParseIPs("10.1.192.1"),
			Role:     ovntypes.NetworkRoleSecondary,
			IPAMMode: ovntypes.IPAMTypeDHCP,
		}
		var err error
		pod.Annotations, err = util.MarshalPodAnnotation(pod.Annotations, entry, pr.nadKey)
		require.NoError(t, err)

		got, needsWrite, err := pr.allocateDHCPMACAnnotation(pod)
		require.NoError(t, err)
		// the stale lease must be cleared (and written, so the controller
		// relaxes port security for the new DHCP exchange), the MAC kept
		// (same client identity, same lease affinity)
		assert.True(t, needsWrite)
		assert.Equal(t, entry.MAC.String(), got.MAC.String())
		assert.Equal(t, ovntypes.IPAMTypeDHCP, got.IPAMMode)
		assert.Empty(t, got.IPs)
		assert.Empty(t, got.Gateways)
		assert.Empty(t, got.Routes)
	})

	newEntryCase := func(t *testing.T, networksAnnotation string) *util.PodAnnotation {
		t.Helper()
		pr := newPR()
		pod := newDHCPAnnotationTestPod(pr, networksAnnotation)

		got, needsWrite, err := pr.allocateDHCPMACAnnotation(pod)
		require.NoError(t, err)
		assert.True(t, needsWrite)
		assert.Equal(t, ovntypes.NetworkRoleSecondary, got.Role)
		assert.Equal(t, ovntypes.IPAMTypeDHCP, got.IPAMMode)
		assert.Empty(t, got.IPs, "the bootstrap entry must be MAC-only; IPs arrive after the DHCP exchange")
		return got
	}

	t.Run("creates a MAC-only entry with a generated MAC", func(t *testing.T) {
		got := newEntryCase(t, "")
		_, err := net.ParseMAC(got.MAC.String())
		require.NoError(t, err)
	})

	t.Run("honors an explicit MacRequest", func(t *testing.T) {
		got := newEntryCase(t, `[{"name":"dhcpnet","namespace":"default","mac":"02:03:04:05:06:07"}]`)
		assert.Equal(t, "02:03:04:05:06:07", got.MAC.String())
	})

	t.Run("rejects an unparseable MacRequest", func(t *testing.T) {
		pr := newPR()
		pod := newDHCPAnnotationTestPod(pr, `[{"name":"dhcpnet","namespace":"default","mac":"not-a-mac"}]`)
		_, _, err := pr.allocateDHCPMACAnnotation(pod)
		require.ErrorContains(t, err, "not-a-mac")
	})
}

// fakeSysfsNetdev lists netdev name with ifindex under the fake sysfs PCI
// device, as the kernel does once a driver binds the VF.
func fakeSysfsNetdev(t *testing.T, devicesRoot, deviceID, name string, index int) {
	t.Helper()
	dir := filepath.Join(devicesRoot, deviceID, "net", name)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ifindex"), []byte(fmt.Sprintf("%d\n", index)), 0o644))
}

func TestGetNetdevIndex(t *testing.T) {
	const deviceID = "0000:65:00.2"

	t.Run("returns the ifindex of the device's netdev", func(t *testing.T) {
		devicesRoot, _ := fakeSysfsPCI(t, deviceID, mlx5CoreDriver, mlx5CoreDriver)
		fakeSysfsNetdev(t, devicesRoot, deviceID, "eth1", 81)

		index, err := getNetdevIndex(context.Background(), deviceID)
		require.NoError(t, err)
		assert.Equal(t, 81, index)
	})

	t.Run("waits for the netdev and retries a rename that races the ifindex read", func(t *testing.T) {
		devicesRoot, _ := fakeSysfsPCI(t, deviceID, mlx5CoreDriver, mlx5CoreDriver)
		netDir := filepath.Join(devicesRoot, deviceID, "net")
		// The netdev is listed before its ifindex can be read: udev renames
		// it right after the listing.
		require.NoError(t, os.MkdirAll(filepath.Join(netDir, "eth1"), 0o755))
		renamed := make(chan error, 1)
		time.AfterFunc(2*netDevPollInterval, func() {
			err := os.Rename(filepath.Join(netDir, "eth1"), filepath.Join(netDir, "enp101s0f0v2"))
			if err == nil {
				err = os.WriteFile(filepath.Join(netDir, "enp101s0f0v2", "ifindex"), []byte("81\n"), 0o644)
			}
			renamed <- err
		})

		index, err := getNetdevIndex(context.Background(), deviceID)
		require.NoError(t, <-renamed)
		require.NoError(t, err)
		assert.Equal(t, 81, index)
	})

	t.Run("fails when the device has no netdev", func(t *testing.T) {
		fakeSysfsPCI(t, deviceID, mlx5CoreDriver, mlx5CoreDriver)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		_, err := getNetdevIndex(ctx, deviceID)
		require.ErrorContains(t, err, "failed to find netdev")
		require.ErrorIs(t, err, context.DeadlineExceeded)
	})

	t.Run("fails when the device lists more than one netdev", func(t *testing.T) {
		devicesRoot, _ := fakeSysfsPCI(t, deviceID, mlx5CoreDriver, mlx5CoreDriver)
		fakeSysfsNetdev(t, devicesRoot, deviceID, "eth1", 81)
		fakeSysfsNetdev(t, devicesRoot, deviceID, "eth2", 82)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		_, err := getNetdevIndex(ctx, deviceID)
		require.ErrorContains(t, err, "found 2 netdevs")
	})

	t.Run("names the ifindex file when its content is not a number", func(t *testing.T) {
		devicesRoot, _ := fakeSysfsPCI(t, deviceID, mlx5CoreDriver, mlx5CoreDriver)
		ifindexPath := filepath.Join(devicesRoot, deviceID, "net", "eth1", "ifindex")
		require.NoError(t, os.MkdirAll(filepath.Dir(ifindexPath), 0o755))
		require.NoError(t, os.WriteFile(ifindexPath, []byte("eth1\n"), 0o644))
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		_, err := getNetdevIndex(ctx, deviceID)
		require.ErrorContains(t, err, ifindexPath)
		require.ErrorIs(t, err, strconv.ErrSyntax)
	})
}

// TestNewDHCPClientBindsByIndex checks that the DHCP client's raw socket is
// opened on the link's ifindex: the link's name is stale after a udev rename,
// and no interface of that name exists here.
func TestNewDHCPClientBindsByIndex(t *testing.T) {
	oldOpen := openRawDHCPConn
	t.Cleanup(func() { openRawDHCPConn = oldOpen })
	var openedIndex int
	openRawDHCPConn = func(index int) (net.PacketConn, error) {
		openedIndex = index
		return net.ListenPacket("udp4", "127.0.0.1:0")
	}

	link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "ovnk-renamed0", Index: 81,
		HardwareAddr: ovntest.MustParseMAC("02:00:00:00:20:0b")}}
	client, err := newDHCPClient(link)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	assert.Equal(t, 81, openedIndex)
}

// leaseDHCPOps replaces only the DHCP exchange and release, which otherwise
// require a privileged raw socket.
type leaseDHCPOps struct {
	bindStubDHCPOps
	acquire func(context.Context, netlink.Link) (*current.Result, *nclient4.Lease, error)
	release func(netlink.Link, *nclient4.Lease, net.HardwareAddr) error
}

func (s *leaseDHCPOps) AcquireLease(ctx context.Context, link netlink.Link) (*current.Result, *nclient4.Lease, error) {
	return s.acquire(ctx, link)
}

func (s *leaseDHCPOps) ReleaseLease(link netlink.Link, lease *nclient4.Lease, hwAddr net.HardwareAddr) error {
	return s.release(link, lease, hwAddr)
}

// TestVFIODHCPUsesNetdevIndex fakes sysfs, netlink and the DHCP exchange: VF
// driver rebinds and udev renames need SR-IOV hardware, which the KIND-based
// E2E jobs do not have. The netlink mock has no LinkByName, so a lookup by
// name fails the test.
func TestVFIODHCPUsesNetdevIndex(t *testing.T) {
	const deviceID = "0000:65:00.2"
	mac := ovntest.MustParseMAC("02:00:00:00:20:0b")
	tests := []struct {
		name        string
		linkErr     error
		linkUpErr   error
		acquireErr  error
		wantErrText string
	}{
		{
			name: "acquires the lease on the netdev found by index although udev renamed it",
		},
		{
			name:        "a DHCP transaction failure is final",
			acquireErr:  fmt.Errorf("no matching response packet received"),
			wantErrText: "DHCP lease acquisition failed",
		},
		{
			name:        "a failed link lookup fails the ADD",
			linkErr:     fmt.Errorf("link not found"),
			wantErrText: "failed to look up netdev 81",
		},
		{
			name:        "a failed link-up fails the ADD",
			linkUpErr:   fmt.Errorf("device or resource busy"),
			wantErrText: "failed to bring up netdev eth1",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			devicesRoot, _ := fakeSysfsPCI(t, deviceID, vfioPCIDriver, vfioPCIDriver)
			fakeSysfsNetdev(t, devicesRoot, deviceID, "eth1", 81)
			oldJournal := vfioHandoffJournalDir
			vfioHandoffJournalDir = t.TempDir()
			oldDHCP := dhcpOps
			oldNetlink := util.GetNetLinkOps()
			t.Cleanup(func() {
				vfioHandoffJournalDir = oldJournal
				dhcpOps = oldDHCP
				util.SetNetLinkOpMockInst(oldNetlink)
			})

			link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "eth1", Index: 81, HardwareAddr: mac}}
			links := &util_mocks.NetLinkOps{}
			if tc.linkErr != nil {
				links.On("LinkByIndex", 81).Return(nil, tc.linkErr).Once()
			} else {
				links.On("LinkByIndex", 81).Return(link, nil).Once()
				links.On("LinkSetUp", link).Run(func(mock.Arguments) {
					// udev renames the netdev while the handoff runs
					netDir := filepath.Join(devicesRoot, deviceID, "net")
					require.NoError(t, os.Rename(filepath.Join(netDir, "eth1"), filepath.Join(netDir, "enp101s0f0v2")))
				}).Return(tc.linkUpErr).Once()
			}
			util.SetNetLinkOpMockInst(links)

			pr := newDHCPPodRequest(t)
			pr.IsVFIO = true
			pr.CNIConf.DeviceID = deviceID
			attempts := 0
			leaseResult := &current.Result{IPs: []*current.IPConfig{{Address: net.IPNet{IP: net.IPv4(192, 0, 2, 10), Mask: net.CIDRMask(24, 32)}}}}
			ops := &leaseDHCPOps{acquire: func(ctx context.Context, actual netlink.Link) (*current.Result, *nclient4.Lease, error) {
				attempts++
				assert.Same(t, pr.ctx, ctx, "DHCP must keep the request context and deadline")
				assert.Same(t, link, actual)
				if tc.acquireErr != nil {
					return nil, nil, tc.acquireErr
				}
				return leaseResult, &nclient4.Lease{}, nil
			}}
			dhcpOps = ops
			// Run these checks even when a fatal assertion stops the test early.
			t.Cleanup(func() {
				assert.Equal(t, []string{mlx5CoreDriver, vfioPCIDriver}, ops.calls)
				pin, readErr := os.ReadFile(filepath.Join(devicesRoot, deviceID, "driver_override"))
				require.NoError(t, readErr)
				assert.Equal(t, vfioPCIDriver, string(pin))
				_, statErr := os.Stat(vfioHandoffJournalPath(deviceID))
				assert.True(t, os.IsNotExist(statErr), "completed handoff must remove its journal")
				links.AssertExpectations(t)
			})

			result, err := pr.runLocalnetVFIODHCP()
			if tc.wantErrText != "" {
				require.ErrorContains(t, err, tc.wantErrText)
				assert.Nil(t, result)
			} else {
				require.NoError(t, err)
				assert.Same(t, leaseResult, result)
			}
			wantAttempts := 1
			if tc.linkErr != nil || tc.linkUpErr != nil {
				wantAttempts = 0
			}
			assert.Equal(t, wantAttempts, attempts, "the DHCP exchange runs at most once")
		})
	}
}

// TestReleaseVFIOProbeLease covers the release after a failed rebind to
// vfio-pci. bindPCIDeviceDriver then restores the kernel driver, whose new
// netdev starts down, so the release brings it up first.
func TestReleaseVFIOProbeLease(t *testing.T) {
	const deviceID = "0000:65:00.2"
	mac := ovntest.MustParseMAC("02:00:00:00:20:0b")
	lease := &nclient4.Lease{ACK: &dhcpv4.DHCPv4{YourIPAddr: net.IPv4(192, 0, 2, 10)}}
	for _, releaseErr := range []error{nil, fmt.Errorf("network is down")} {
		name := "releases on the netdev found by index, then brings it back down"
		if releaseErr != nil {
			name = "a failed release is logged, not retried, and the netdev goes back down"
		}
		t.Run(name, func(t *testing.T) {
			devicesRoot, _ := fakeSysfsPCI(t, deviceID, mlx5CoreDriver, mlx5CoreDriver)
			fakeSysfsNetdev(t, devicesRoot, deviceID, "eth1", 82)
			oldDHCP := dhcpOps
			oldNetlink := util.GetNetLinkOps()
			t.Cleanup(func() {
				dhcpOps = oldDHCP
				util.SetNetLinkOpMockInst(oldNetlink)
			})

			link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "eth1", Index: 82, HardwareAddr: mac}}
			releases := 0
			links := &util_mocks.NetLinkOps{}
			links.On("LinkByIndex", 82).Return(link, nil).Once()
			links.On("LinkSetUp", link).Return(nil).Once()
			// The restored kernel driver's netdev started down; the failed ADD
			// must not leave it up on the VM's network.
			links.On("LinkSetDown", link).Run(func(mock.Arguments) {
				assert.Equal(t, 1, releases, "the netdev must go down only after the release attempt")
			}).Return(nil).Once()
			util.SetNetLinkOpMockInst(links)
			dhcpOps = &leaseDHCPOps{release: func(actual netlink.Link, actualLease *nclient4.Lease, hwAddr net.HardwareAddr) error {
				releases++
				assert.Same(t, link, actual)
				assert.Same(t, lease, actualLease)
				assert.Equal(t, mac, hwAddr)
				return releaseErr
			}}

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			releaseVFIOProbeLease(ctx, deviceID, lease, mac)

			assert.Equal(t, 1, releases)
			links.AssertExpectations(t)
		})
	}
}
