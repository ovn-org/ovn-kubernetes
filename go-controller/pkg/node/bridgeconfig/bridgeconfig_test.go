// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package bridgeconfig

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/k8snetworkplumbingwg/sriovnet"
	"github.com/onsi/gomega"

	"k8s.io/utils/ptr"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	uplinkv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1"
	ovsops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	utilmocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/mocks"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
)

type blockingListOVSClient struct {
	libovsdbclient.Client
	listEntered  chan int32
	releaseFirst chan struct{}
	listCalls    atomic.Int32
}

func (c *blockingListOVSClient) List(ctx context.Context, result interface{}) error {
	call := c.listCalls.Add(1)
	c.listEntered <- call
	if call == 1 {
		<-c.releaseFirst
	}
	return c.Client.List(ctx, result)
}

func TestBridgedGatewayNodeSetupSerializesBridgeMappingUpdates(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	t.Cleanup(func() { _ = config.PrepareTestConfig() })
	config.IPv4Mode = false
	config.IPv6Mode = false

	ovsClient, cleanup, err := libovsdbtest.NewOVSTestHarness(
		libovsdbtest.TestSetup{OVSData: []libovsdbtest.TestData{
			&vswitchd.OpenvSwitch{UUID: "root-ovs"},
		}},
	)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	t.Cleanup(cleanup.Cleanup)

	client := &blockingListOVSClient{
		Client:       ovsClient,
		listEntered:  make(chan int32, 2),
		releaseFirst: make(chan struct{}),
	}
	firstDone := make(chan error, 1)
	go func() {
		_, err := bridgedGatewayNodeSetup(client, "node-a", "br-a", "phys-a")
		firstDone <- err
	}()
	if call := <-client.listEntered; call != 1 {
		t.Fatalf("expected first bridge-mapping read, got call %d", call)
	}

	secondStarted := make(chan struct{})
	secondDone := make(chan error, 1)
	go func() {
		close(secondStarted)
		_, err := bridgedGatewayNodeSetup(client, "node-a", "br-b", "phys-b")
		secondDone <- err
	}()
	<-secondStarted
	enteredConcurrently := false
	select {
	case <-client.listEntered:
		enteredConcurrently = true
	case <-time.After(100 * time.Millisecond):
	}
	close(client.releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatalf("first bridge-mapping update failed: %v", err)
	}
	if !enteredConcurrently {
		select {
		case call := <-client.listEntered:
			if call != 2 {
				t.Fatalf("expected second bridge-mapping read, got call %d", call)
			}
		case <-time.After(time.Second):
			t.Fatal("second bridge-mapping update did not run")
		}
	}
	if err := <-secondDone; err != nil {
		t.Fatalf("second bridge-mapping update failed: %v", err)
	}
	if enteredConcurrently {
		t.Fatal("bridge-mapping read-modify-write operations ran concurrently")
	}

	ovs, err := ovsops.GetOpenvSwitch(ovsClient)
	if err != nil {
		t.Fatalf("failed to read bridge mappings: %v", err)
	}
	mappings := map[string]string{}
	for _, mapping := range strings.Split(ovs.ExternalIDs["ovn-bridge-mappings"], ",") {
		parts := strings.SplitN(mapping, ":", 2)
		if len(parts) == 2 {
			mappings[parts[0]] = parts[1]
		}
	}
	if mappings["phys-a"] != "br-a" || mappings["phys-b"] != "br-b" {
		t.Fatalf("concurrent updates lost a bridge mapping: %#v", mappings)
	}
}

func TestGetStaticFDBPort(t *testing.T) {
	tests := []struct {
		name     string
		bridge   *BridgeConfiguration
		expected string
	}{
		{
			name: "uses bridge when representor is absent",
			bridge: &BridgeConfiguration{
				bridgeName: "br-ex",
			},
			expected: "br-ex",
		},
		{
			name: "uses representor when present",
			bridge: &BridgeConfiguration{
				bridgeName: "ovsbr1",
				gwIfaceRep: "pf0hpf",
			},
			expected: "pf0hpf",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.bridge.GetStaticFDBPort(); got != tc.expected {
				t.Fatalf("expected static FDB port %q, got %q", tc.expected, got)
			}
		})
	}
}

func TestGatewayHostOVSInterfaceResolvesSmartNICRepresentor(t *testing.T) {
	g := gomega.NewWithT(t)
	fexec := ovntest.NewFakeExec()
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovs-vsctl --timeout=15 port-to-br pf0vf1",
		Stderr: "no bridge for pf0vf1",
		Err:    fmt.Errorf("not an OVS port"),
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovs-vsctl --timeout=15 port-to-br pf0vf1_rep",
		Output: "ovsbr1",
	})
	g.Expect(util.SetExec(fexec)).To(gomega.Succeed())
	t.Cleanup(util.ResetRunner)

	fsOps := utilmocks.NewFileSystemOps(t)
	origFSOps := util.GetFileSystemOps()
	util.SetFileSystemOps(fsOps)
	t.Cleanup(func() {
		util.SetFileSystemOps(origFSOps)
	})
	fsOps.On("Readlink", "/sys/class/net/pf0vf1/device").
		Return("../../0000:00:00.1", nil)

	sriovOps := utilmocks.NewSriovnetOps(t)
	origSriovOps := util.GetSriovnetOps()
	util.SetSriovnetOpsInst(sriovOps)
	t.Cleanup(func() {
		util.SetSriovnetOpsInst(origSriovOps)
	})
	sriovOps.On("GetUplinkRepresentor", "0000:00:00.1").Return("pf0", nil)
	sriovOps.On("GetVfIndexByPciAddress", "0000:00:00.1").Return(1, nil)
	sriovOps.On("GetVfRepresentor", "pf0", 1).Return("pf0vf1_rep", nil)

	rep, err := gatewayHostOVSInterface("ovsbr1", "pf0vf1")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(rep).To(gomega.Equal("pf0vf1_rep"))
	g.Expect(fexec.CalledMatchesExpected()).To(gomega.BeTrue(), fexec.ErrorDesc())
}

// newDPUUnmanagedBridgeHarness prepares the scaffolding shared by the
// unmanaged bridge configuration tests: a DPU-mode node config restored on
// cleanup, an OVSDB with one bridge holding the eth1 uplink and one
// (Port, Interface) pair per representor name, a fake exec, and a fresh sriovnet mock installed for the test.
func newDPUUnmanagedBridgeHarness(t *testing.T, bridgeName string, bridgeExternalIDs map[string]string,
	repNames ...string) (libovsdbclient.Client, *ovntest.FakeExec, *utilmocks.SriovnetOps) {
	t.Helper()
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	t.Cleanup(func() {
		_ = config.PrepareTestConfig()
		util.ResetRunner()
	})
	config.IPv4Mode = false
	config.OvnKubeNode.Mode = ovntypes.NodeModeDPU

	bridgeUUID := bridgeName + "-uuid"
	portUUIDs := []string{"eth1-port-uuid"}
	ovsData := []libovsdbtest.TestData{
		&vswitchd.Port{UUID: "eth1-port-uuid", Name: "eth1", Interfaces: []string{"eth1-interface-uuid"}},
		&vswitchd.Interface{UUID: "eth1-interface-uuid", Name: "eth1", Type: "system", Ofport: ptr.To(7)},
	}
	for i, repName := range repNames {
		portUUID := repName + "-port-uuid"
		interfaceUUID := repName + "-interface-uuid"
		portUUIDs = append(portUUIDs, portUUID)
		ovsData = append(ovsData,
			&vswitchd.Port{UUID: portUUID, Name: repName, Interfaces: []string{interfaceUUID}},
			&vswitchd.Interface{UUID: interfaceUUID, Name: repName, Type: "system", Ofport: ptr.To(8 + i)},
		)
	}
	ovsData = append(ovsData,
		&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{bridgeUUID}},
		&vswitchd.Bridge{
			UUID:        bridgeUUID,
			Name:        bridgeName,
			Ports:       portUUIDs,
			ExternalIDs: bridgeExternalIDs,
		},
	)
	ovsClient, ovsCleanup, err := libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{OVSData: ovsData})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	t.Cleanup(ovsCleanup.Cleanup)

	sriovOps := utilmocks.NewSriovnetOps(t)
	origSriovOps := util.GetSriovnetOps()
	util.SetSriovnetOpsInst(sriovOps)
	t.Cleanup(func() {
		util.SetSriovnetOpsInst(origSriovOps)
	})
	fexec := ovntest.NewFakeExec()
	g.Expect(util.SetExec(fexec)).To(gomega.Succeed())
	return ovsClient, fexec, sriovOps
}

func TestNewUnmanagedBridgeConfigurationResolvesDPUHostRepresentor(t *testing.T) {
	g := gomega.NewWithT(t)
	ovsClient, fexec, sriovOps := newDPUUnmanagedBridgeHarness(t, "ovsbr1",
		map[string]string{"bridge-uplink": "eth1"}, "pfhpf0")
	// FindHostRepresentorByPeerMAC walks the bridge ports in map order and
	// returns on the first match, so expectations for the other ports may
	// legitimately go unused.
	sriovOps.On("GetRepresentorPortFlavour", "eth1").
		Return(sriovnet.PortFlavour(sriovnet.PORT_FLAVOUR_UNKNOWN), fmt.Errorf("not a representor")).
		Maybe()
	sriovOps.On("GetRepresentorPortFlavour", "pfhpf0").
		Return(sriovnet.PortFlavour(sriovnet.PORT_FLAVOUR_PCI_PF), nil)
	sriovOps.On("GetDevlinkPortFunctionMacAddress", "pfhpf0").
		Return(ovntest.MustParseMAC("00:11:22:33:44:55"), nil)

	bridge, err := NewUnmanagedBridgeConfiguration(
		ovsClient,
		"ovsbr1",
		"pf0",
		"node-a",
		"physnet-blue",
		ovntest.MustParseIPNets("172.28.0.2/24"),
		ovntest.MustParseMAC("00:11:22:33:44:55"),
		nil,
	)
	g.Expect(err).NotTo(gomega.HaveOccurred(), "the PF-backed unmanaged bridge configuration must succeed")
	g.Expect(bridge.GetGatewayIfaceRep()).To(gomega.Equal("pfhpf0"),
		"the PF representor must be selected as the gateway representor")
	g.Expect(bridge.GetStaticFDBPort()).To(gomega.Equal("pfhpf0"),
		"the static FDB entry must be pinned to the PF representor")
	g.Expect(bridge.ConfigureBridgePorts()).To(gomega.Succeed())
	g.Expect(bridge.ofPortPhys).To(gomega.Equal("7"))
	g.Expect(bridge.ofPortHost).To(gomega.Equal("8"))
	g.Expect(fexec.CalledMatchesExpected()).To(gomega.BeTrue(), fexec.ErrorDesc())
}

// TestNewUnmanagedBridgeConfigurationResolvesDPUHostFunctionRepresentor covers
// the common host VF layout: the host assigns its own VF MAC, so the DPU
// eswitch reports no function MAC for the VF representor and only the published
// host function indices can identify it.
func TestNewUnmanagedBridgeConfigurationResolvesDPUHostFunctionRepresentor(t *testing.T) {
	g := gomega.NewWithT(t)
	ovsClient, fexec, sriovOps := newDPUUnmanagedBridgeHarness(t, "br-hostvf0", nil, "pf0vf0")
	sriovOps.On("GetVfRepresentorDPU", "0", "0").Return("pf0vf0", nil)
	sriovOps.On("GetRepresentorPortFlavour", "eth1").
		Return(sriovnet.PortFlavour(sriovnet.PORT_FLAVOUR_UNKNOWN), fmt.Errorf("not a representor"))
	sriovOps.On("GetRepresentorPortFlavour", "pf0vf0").
		Return(sriovnet.PortFlavour(sriovnet.PORT_FLAVOUR_PCI_VF), nil)
	// The eswitch holds no function MAC for a host assigned VF MAC, so the
	// representor offers no identity to verify against.
	sriovOps.On("GetDevlinkPortFunctionMacAddress", "pf0vf0").
		Return(nil, fmt.Errorf("devlink port function for netdev pf0vf0 does not report a usable hardware address"))

	bridge, err := NewUnmanagedBridgeConfiguration(
		ovsClient,
		"br-hostvf0",
		"enp4s0f0v0",
		"node-a",
		"physnet-blue",
		ovntest.MustParseIPNets("172.28.0.2/24"),
		ovntest.MustParseMAC("4e:68:cd:ae:2b:f5"),
		&uplinkv1alpha1.HostFunction{PFID: 0, VFID: ptr.To(int32(0))},
	)
	g.Expect(err).NotTo(gomega.HaveOccurred(),
		"an unreadable eswitch function MAC must not block gateway representor resolution")
	g.Expect(bridge.GetGatewayIfaceRep()).To(gomega.Equal("pf0vf0"),
		"the representor of the published host function must be selected as the gateway representor")
	g.Expect(bridge.GetStaticFDBPort()).To(gomega.Equal("pf0vf0"),
		"the static FDB entry must be pinned to the host function representor")
	g.Expect(fexec.CalledMatchesExpected()).To(gomega.BeTrue(), fexec.ErrorDesc())
}

// TestDPUGatewayRepresentorFallback covers the non-happy branches of
// dpuGatewayRepresentor: the MAC scan fallback when the host function lookup
// fails or resolves a representor that is not on the bridge, and the combined
// error when both resolution paths fail.
func TestDPUGatewayRepresentorFallback(t *testing.T) {
	hostMAC := ovntest.MustParseMAC("4e:68:cd:ae:2b:f5")
	hostFunction := &uplinkv1alpha1.HostFunction{PFID: 0, VFID: ptr.To(int32(0))}

	// The MAC scan walks the bridge ports in map order, so the eth1 probe may
	// legitimately go unused when pf0vf0 matches first.
	mockUplinkNotARepresentor := func(sriovOps *utilmocks.SriovnetOps) {
		sriovOps.On("GetRepresentorPortFlavour", "eth1").
			Return(sriovnet.PortFlavour(sriovnet.PORT_FLAVOUR_UNKNOWN), fmt.Errorf("not a representor")).
			Maybe()
	}

	t.Run("falls back to the MAC scan when the host function lookup fails", func(t *testing.T) {
		g := gomega.NewWithT(t)
		ovsClient, _, sriovOps := newDPUUnmanagedBridgeHarness(t, "br-hostvf0", nil, "pf0vf0")
		sriovOps.On("GetVfRepresentorDPU", "0", "0").
			Return("", fmt.Errorf("failed to get VF representor"))
		mockUplinkNotARepresentor(sriovOps)
		sriovOps.On("GetRepresentorPortFlavour", "pf0vf0").
			Return(sriovnet.PortFlavour(sriovnet.PORT_FLAVOUR_PCI_VF), nil)
		sriovOps.On("GetDevlinkPortFunctionMacAddress", "pf0vf0").
			Return(hostMAC, nil)

		bridge, err := ovsops.GetBridge(ovsClient, "br-hostvf0")
		g.Expect(err).NotTo(gomega.HaveOccurred())
		rep, err := dpuGatewayRepresentor(ovsClient, bridge, hostFunction, hostMAC, "node-a")
		g.Expect(err).NotTo(gomega.HaveOccurred(),
			"a failed host function lookup must fall back to the MAC scan")
		g.Expect(rep).To(gomega.Equal("pf0vf0"))
	})

	t.Run("falls back to the MAC scan when the resolved representor is not on the bridge", func(t *testing.T) {
		g := gomega.NewWithT(t)
		ovsClient, _, sriovOps := newDPUUnmanagedBridgeHarness(t, "br-hostvf0", nil, "pf0vf0")
		// The indices resolve a representor that is not attached to the
		// uplink bridge, with no eswitch function MAC to veto it early.
		sriovOps.On("GetVfRepresentorDPU", "0", "0").Return("pf9vf9", nil)
		sriovOps.On("GetRepresentorPortFlavour", "pf9vf9").
			Return(sriovnet.PortFlavour(sriovnet.PORT_FLAVOUR_PCI_VF), nil)
		sriovOps.On("GetDevlinkPortFunctionMacAddress", "pf9vf9").
			Return(nil, fmt.Errorf("no usable hardware address"))
		mockUplinkNotARepresentor(sriovOps)
		sriovOps.On("GetRepresentorPortFlavour", "pf0vf0").
			Return(sriovnet.PortFlavour(sriovnet.PORT_FLAVOUR_PCI_VF), nil)
		sriovOps.On("GetDevlinkPortFunctionMacAddress", "pf0vf0").
			Return(hostMAC, nil)

		bridge, err := ovsops.GetBridge(ovsClient, "br-hostvf0")
		g.Expect(err).NotTo(gomega.HaveOccurred())
		rep, err := dpuGatewayRepresentor(ovsClient, bridge, hostFunction, hostMAC, "node-a")
		g.Expect(err).NotTo(gomega.HaveOccurred(),
			"a representor on another bridge must be rejected and fall back to the MAC scan")
		g.Expect(rep).To(gomega.Equal("pf0vf0"))
	})

	t.Run("combines the errors when both resolution paths fail", func(t *testing.T) {
		g := gomega.NewWithT(t)
		ovsClient, _, sriovOps := newDPUUnmanagedBridgeHarness(t, "br-hostvf0", nil, "pf0vf0")
		sriovOps.On("GetVfRepresentorDPU", "0", "0").
			Return("", fmt.Errorf("failed to get VF representor"))
		mockUplinkNotARepresentor(sriovOps)
		// The only representor on the bridge peers with some other host MAC.
		sriovOps.On("GetRepresentorPortFlavour", "pf0vf0").
			Return(sriovnet.PortFlavour(sriovnet.PORT_FLAVOUR_PCI_VF), nil)
		sriovOps.On("GetDevlinkPortFunctionMacAddress", "pf0vf0").
			Return(ovntest.MustParseMAC("00:11:22:33:44:66"), nil)

		bridge, err := ovsops.GetBridge(ovsClient, "br-hostvf0")
		g.Expect(err).NotTo(gomega.HaveOccurred())
		_, err = dpuGatewayRepresentor(ovsClient, bridge, hostFunction, hostMAC, "node-a")
		g.Expect(err).To(gomega.MatchError(util.ErrHostRepresentorNotFound),
			"the MAC scan miss must stay inspectable in the combined error")
		g.Expect(err.Error()).To(gomega.ContainSubstring("host function pf0vf0 did not resolve one either"),
			"the combined error must carry the host function failure as well")
	})
}

func TestNewUnmanagedBridgeConfigurationResolvesDPUHostVFRepresentor(t *testing.T) {
	g := gomega.NewWithT(t)
	// No bridge-uplink external-id: the physical uplink must be derived by
	// skipping the VF representors.
	ovsClient, fexec, sriovOps := newDPUUnmanagedBridgeHarness(t, "br-hostvf0", nil, "pf0vf0", "pf0vf7")
	// The walk returns on the first match (pf0vf7), so the probes of the
	// other ports may legitimately go unused depending on map order.
	sriovOps.On("GetRepresentorPortFlavour", "eth1").
		Return(sriovnet.PortFlavour(sriovnet.PORT_FLAVOUR_UNKNOWN), fmt.Errorf("not a representor")).
		Maybe()
	sriovOps.On("GetRepresentorPortFlavour", "pf0vf0").
		Return(sriovnet.PortFlavour(sriovnet.PORT_FLAVOUR_PCI_VF), nil).
		Maybe()
	sriovOps.On("GetDevlinkPortFunctionMacAddress", "pf0vf0").
		Return(ovntest.MustParseMAC("00:11:22:33:44:66"), nil).
		Maybe()
	sriovOps.On("GetRepresentorPortFlavour", "pf0vf7").
		Return(sriovnet.PortFlavour(sriovnet.PORT_FLAVOUR_PCI_VF), nil)
	sriovOps.On("GetDevlinkPortFunctionMacAddress", "pf0vf7").
		Return(ovntest.MustParseMAC("00:11:22:33:44:55"), nil)

	bridge, err := NewUnmanagedBridgeConfiguration(
		ovsClient,
		"br-hostvf0",
		"enp4s0f0v0",
		"node-a",
		"physnet-blue",
		ovntest.MustParseIPNets("172.28.0.2/24"),
		ovntest.MustParseMAC("00:11:22:33:44:55"),
		nil,
	)
	g.Expect(err).NotTo(gomega.HaveOccurred(), "the VF-backed unmanaged bridge configuration must succeed")
	g.Expect(bridge.GetGatewayIfaceRep()).To(gomega.Equal("pf0vf7"),
		"the VF representor whose peer MAC matches must be selected as the gateway representor")
	g.Expect(bridge.GetStaticFDBPort()).To(gomega.Equal("pf0vf7"),
		"the static FDB entry must be pinned to the matching VF representor")
	g.Expect(fexec.CalledMatchesExpected()).To(gomega.BeTrue(), fexec.ErrorDesc())
}
