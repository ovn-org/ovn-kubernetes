// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package node

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/containernetworking/plugins/pkg/testutils"
	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	nadfake "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/fake"
	"github.com/spf13/afero"
	"github.com/stretchr/testify/mock"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	informerfactory "k8s.io/client-go/informers"
	coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/knftables"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/model"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	rafakeclient "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1/apis/clientset/versioned/fake"
	uplinkv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1"
	uplinkfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/clientset/versioned/fake"
	uplinkinformerfactory "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/informers/externalversions"
	uplinkinformers "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/informers/externalversions/uplink/v1alpha1"
	uplinklisters "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/listers/uplink/v1alpha1"
	udnfakeclient "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/clientset/versioned/fake"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	factoryMocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory/mocks"
	udn "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/generator/udn"
	kubemocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube/mocks"
	ovsops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/bridgeconfig"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/iprulemanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/managementport"
	nodenft "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/nftables"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/routemanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/vrfmanager"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	coreinformermocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/mocks/k8s.io/client-go/informers/core/v1"
	v1mocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/mocks/k8s.io/client-go/listers/core/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	uplinkutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/uplink"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	utilmocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/mocks"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// emptyNodeInformer satisfies the gateway dependency for tests that do not
// look nodes up through it.
func emptyNodeInformer() coreinformers.NodeInformer {
	return informerfactory.NewSharedInformerFactory(fake.NewSimpleClientset(), 0).Core().V1().Nodes()
}

func getCreationFakeCommands(fexec *ovntest.FakeExec, mgtPort, mgtPortMAC, netName, nodeName string, mtu int) {
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovs-vsctl --timeout=15" +
			" -- --may-exist add-port br-int " + mgtPort +
			" -- set interface " + mgtPort +
			fmt.Sprintf(" mac=\"%s\"", mgtPortMAC) +
			" type=internal mtu_request=" + fmt.Sprintf("%d", mtu) +
			" external-ids:iface-id=" + types.K8sPrefix + netName + "_" + nodeName +
			fmt.Sprintf(" external-ids:%s=%s", types.NetworkExternalID, netName),
	})

	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "sysctl -w net.ipv4.conf." + mgtPort + ".forwarding = 1",
		Output: "net.ipv4.conf." + mgtPort + ".forwarding = 1",
	})
}

func getRPFilterLooseModeFakeCommand(fexec *ovntest.FakeExec, ifName string) {
	setVal := fmt.Sprintf("net.ipv4.conf.%s.rp_filter = 2", strings.ReplaceAll(ifName, ".", "/"))
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    fmt.Sprintf("sysctl -w %s", setVal),
		Output: setVal,
	})
}

func getIPv6KeepAddrOnDownFakeCommand(fexec *ovntest.FakeExec, ifName string) {
	setVal := fmt.Sprintf("net.ipv6.conf.%s.keep_addr_on_down = 1", strings.ReplaceAll(ifName, ".", "/"))
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    fmt.Sprintf("sysctl -w %s", setVal),
		Output: setVal,
	})
}

func getRPFilterLooseModeFakeCommands(fexec *ovntest.FakeExec) {
	getRPFilterLooseModeFakeCommand(fexec, "ovn-k8s-mp3")
}

func TestConfigureUplinkGatewayRPFilter(t *testing.T) {
	tests := []struct {
		name            string
		mode            string
		networkIPv4Mode bool
		resolved        *resolvedUplinkGateway
		wantSysctl      string
	}{
		{
			name:            "full mode uses bridge local interface",
			mode:            types.NodeModeFull,
			networkIPv4Mode: true,
			resolved: &resolvedUplinkGateway{
				hostInterfaceName: "eth1",
				bridgeName:        "ovsbr1",
			},
			wantSysctl: "ovsbr1",
		},
		{
			name:            "DPU-host mode uses host interface",
			mode:            types.NodeModeDPUHost,
			networkIPv4Mode: true,
			resolved: &resolvedUplinkGateway{
				hostInterfaceName: "pfhpf0",
				bridgeName:        "ovsbr1",
			},
			wantSysctl: "pfhpf0",
		},
		{
			name: "IPv6-only UDN skips IPv4 reverse path filtering",
			mode: types.NodeModeFull,
			resolved: &resolvedUplinkGateway{
				hostInterfaceName: "eth1",
				bridgeName:        "ovsbr1",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := config.PrepareTestConfig(); err != nil {
				t.Fatalf("failed to prepare test config: %v", err)
			}
			t.Cleanup(func() {
				_ = config.PrepareTestConfig()
				util.ResetRunner()
			})
			config.IPv4Mode = true
			config.OvnKubeNode.Mode = test.mode

			fexec := ovntest.NewFakeExec()
			if test.wantSysctl != "" {
				getRPFilterLooseModeFakeCommand(fexec, test.wantSysctl)
			}
			if err := util.SetExec(fexec); err != nil {
				t.Fatalf("failed to set fake exec: %v", err)
			}

			if err := configureUplinkGatewayRPFilter(test.resolved, test.networkIPv4Mode); err != nil {
				t.Fatalf("failed to configure Uplink rp_filter: %v", err)
			}
			if !fexec.CalledMatchesExpected() {
				t.Fatalf("%s", fexec.ErrorDesc())
			}
		})
	}
}

func TestConfigureResolvedUplinkGatewayRequiresValidInterface(t *testing.T) {
	if err := config.PrepareTestConfig(); err != nil {
		t.Fatalf("failed to prepare test config: %v", err)
	}
	t.Cleanup(func() {
		_ = config.PrepareTestConfig()
	})
	config.OvnKubeNode.Mode = types.NodeModeDPUHost
	config.IPv4Mode = false
	config.IPv6Mode = true
	config.Gateway.Mode = config.GatewayModeShared
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true
	config.OVNKubernetesFeature.EnableUplink = true

	nad := generateUplinkNAD("red", "rednad", "greenamespace",
		types.Layer3Topology, "ae70::/60/64", types.NetworkRolePrimary, "uplink1")
	netInfo, err := util.ParseNADInfo(nad)
	if err != nil {
		t.Fatalf("failed to parse NAD: %v", err)
	}

	tests := []struct {
		name           string
		interfaceName  string
		interfaceIndex int
		wantErr        string
	}{
		{
			name:    "interface is unresolved",
			wantErr: "uplink gateway interface is unset for network red",
		},
		{
			name:           "interface index is invalid",
			interfaceName:  "eth1",
			interfaceIndex: 0,
			wantErr:        "uplink gateway interface eth1 has invalid link index for network red",
		},
		{
			name:           "interface is valid",
			interfaceName:  "eth1",
			interfaceIndex: 7,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			netlinkOps := new(utilmocks.NetLinkOps)
			originalNetlinkOps := util.GetNetLinkOps()
			util.SetNetLinkOpMockInst(netlinkOps)
			t.Cleanup(func() {
				util.SetNetLinkOpMockInst(originalNetlinkOps)
			})
			if tt.interfaceName != "" {
				link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{
					Name:  tt.interfaceName,
					Index: tt.interfaceIndex,
				}}
				netlinkOps.On("LinkByName", tt.interfaceName).Return(link, nil).Once()
			}

			udng := &UserDefinedNetworkGateway{
				NetInfo: netInfo,
			}

			_, err := udng.configureResolvedUplinkGateway(&resolvedUplinkGateway{
				hostInterfaceName: tt.interfaceName,
			})
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("expected no error, got %v", err)
				}
				if udng.gwInterfaceIndex != tt.interfaceIndex {
					t.Fatalf("unexpected gateway interface index %d", udng.gwInterfaceIndex)
				}
			} else if err == nil || err.Error() != tt.wantErr {
				t.Fatalf("expected error %q, got %v", tt.wantErr, err)
			}
			netlinkOps.AssertExpectations(t)
		})
	}
}

func TestUplinkGatewayInterfaceName(t *testing.T) {
	tests := []struct {
		name     string
		mode     string
		resolved *resolvedUplinkGateway
		want     string
	}{
		{
			name: "full mode uses bridge local interface",
			mode: types.NodeModeFull,
			resolved: &resolvedUplinkGateway{
				hostInterfaceName: "eth1",
				bridgeName:        "ovsbr1",
			},
			want: "ovsbr1",
		},
		{
			name: "DPU mode uses bridge local interface",
			mode: types.NodeModeDPU,
			resolved: &resolvedUplinkGateway{
				hostInterfaceName: "pfhpf0",
				bridgeName:        "ovsbr1",
			},
			want: "ovsbr1",
		},
		{
			name: "DPU-host mode uses host interface",
			mode: types.NodeModeDPUHost,
			resolved: &resolvedUplinkGateway{
				hostInterfaceName: "pfhpf0",
				bridgeName:        "ovsbr1",
			},
			want: "pfhpf0",
		},
		{
			name: "full mode with empty bridge returns no interface",
			mode: types.NodeModeFull,
			resolved: &resolvedUplinkGateway{
				hostInterfaceName: "eth1",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := config.PrepareTestConfig(); err != nil {
				t.Fatalf("failed to prepare test config: %v", err)
			}
			t.Cleanup(func() {
				_ = config.PrepareTestConfig()
			})
			config.OvnKubeNode.Mode = test.mode

			if got := uplinkGatewayInterfaceName(test.resolved); got != test.want {
				t.Fatalf("expected %q, got %q", test.want, got)
			}
		})
	}
}

func TestConfigureUplinkStaticFDBEntry(t *testing.T) {
	tests := []struct {
		name        string
		mode        string
		bridge      *bridgeconfig.BridgeConfiguration
		expectedCmd string
	}{
		{
			name:   "full mode programs bridge local port",
			mode:   types.NodeModeFull,
			bridge: bridgeconfig.TestBridgeConfig("ovsbr1"),
			expectedCmd: "ovs-appctl -t /var/run/openvswitch/ovs-vswitchd.1234.ctl " +
				"fdb/add ovsbr1 ovsbr1 0 00:11:22:33:44:55",
		},
		{
			name:   "DPU mode programs gateway representor port",
			mode:   types.NodeModeDPU,
			bridge: bridgeconfig.TestBridgeConfigWithGatewayRepresentor("ovsbr1", "pfhpf0"),
			expectedCmd: "ovs-appctl -t /var/run/openvswitch/ovs-vswitchd.1234.ctl " +
				"fdb/add ovsbr1 pfhpf0 0 00:11:22:33:44:55",
		},
		{
			name:   "DPU-host mode skips DPU bridge programming",
			mode:   types.NodeModeDPUHost,
			bridge: bridgeconfig.TestBridgeConfigWithGatewayRepresentor("ovsbr1", "pfhpf0"),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := config.PrepareTestConfig(); err != nil {
				t.Fatalf("failed to prepare test config: %v", err)
			}
			t.Cleanup(func() {
				_ = config.PrepareTestConfig()
				util.ResetRunner()
			})
			config.OvnKubeNode.Mode = test.mode
			test.bridge.SetMAC(ovntest.MustParseMAC("00:11:22:33:44:55"))

			fexec := ovntest.NewFakeExec()
			if test.expectedCmd != "" {
				origAppFs := util.AppFs
				util.AppFs = afero.NewMemMapFs()
				t.Cleanup(func() {
					util.AppFs = origAppFs
				})
				fexec.AddFakeCmdsNoOutputNoError([]string{test.expectedCmd})
				if err := util.SetupMockOVSPidFile(); err != nil {
					t.Fatalf("failed to setup fake OVS pid file: %v", err)
				}
			}
			if err := util.SetExec(fexec); err != nil {
				t.Fatalf("failed to set fake exec: %v", err)
			}

			if err := configureUplinkStaticFDBEntry(test.bridge); err != nil {
				t.Fatalf("failed to configure Uplink static FDB entry: %v", err)
			}
			if !fexec.CalledMatchesExpected() {
				t.Fatalf("%s", fexec.ErrorDesc())
			}
		})
	}
}

func TestUplinkVRFAttachmentFailureReason(t *testing.T) {
	conflictErr := fmt.Errorf("failed to attach: %w", &vrfmanager.VRFSlaveConflictError{
		Interface:    "breth1",
		RequestedVRF: "blue",
		ExistingVRF:  "red",
	})
	if got := uplinkVRFAttachmentFailureReason(conflictErr); got != uplinkv1alpha1.UplinkStateReasonConfigurationConflict {
		t.Fatalf("expected configuration conflict reason, got %q", got)
	}

	got := uplinkVRFAttachmentFailureReason(fmt.Errorf("netlink failed"))
	if got != uplinkv1alpha1.UplinkStateReasonVRFAttachmentFailed {
		t.Fatalf("expected VRF attachment failure reason, got %q", got)
	}
}

func resolvedGatewayUplinkState(uplinkName, nodeName string) *uplinkv1alpha1.UplinkState {
	return &uplinkv1alpha1.UplinkState{
		ObjectMeta: metav1.ObjectMeta{Name: uplinkutil.StateName(uplinkName, nodeName)},
		Spec: uplinkv1alpha1.UplinkStateSpec{
			UplinkName: uplinkName,
			NodeName:   nodeName,
		},
		Status: uplinkv1alpha1.UplinkStateStatus{
			Type:              uplinkv1alpha1.UplinkTypeOVSBridge,
			HostInterfaceName: "eth0",
			OVSBridge:         &uplinkv1alpha1.OVSBridgeStatus{Name: "breth0"},
			MACAddress:        "02:42:ac:12:00:02",
			IPAddresses:       []uplinkv1alpha1.IPAddressCIDR{"192.0.2.10/24"},
			DefaultGateways:   []uplinkv1alpha1.IPAddress{"192.0.2.1"},
			Conditions: []metav1.Condition{{
				Type:   uplinkv1alpha1.UplinkStateConditionResolved,
				Status: metav1.ConditionTrue,
			}},
		},
	}
}

func newUplinkGatewayReconcileHarness(
	t *testing.T,
	state *uplinkv1alpha1.UplinkState,
) (*UserDefinedNetworkGateway, *utilmocks.NetLinkOps, *v1mocks.NodeLister) {
	t.Helper()
	const (
		uplinkName = "uplink1"
		nodeName   = "node-a"
		networkID  = "3"
	)

	nad := generateUplinkNAD("red", "red-nad", "test", types.Layer3Topology,
		"100.128.0.0/16/24", types.NetworkRolePrimary, uplinkName)
	ovntest.AnnotateNADWithNetworkID(networkID, nad)
	netInfo, err := util.ParseNADInfo(nad)
	if err != nil {
		t.Fatalf("failed to parse Uplink network: %v", err)
	}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: nodeName,
			Annotations: map[string]string{
				types.NodeSubnetsAnnotation: `{"red":["100.128.0.0/24"]}`,
			},
		},
	}

	netlinkOps := utilmocks.NewNetLinkOps(t)
	originalNetlinkOps := util.GetNetLinkOps()
	util.SetNetLinkOpMockInst(netlinkOps)
	t.Cleanup(func() { util.SetNetLinkOpMockInst(originalNetlinkOps) })
	netlinkOps.On("LinkByName", "breth0").Return(
		&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "breth0", Index: 1}}, nil,
	).Once()

	uplinkClient := uplinkfake.NewSimpleClientset(state)
	informerFactory := uplinkinformerfactory.NewSharedInformerFactory(uplinkClient, 0)
	uplinkStateInformer := informerFactory.K8s().V1alpha1().UplinkStates()
	_ = uplinkStateInformer.Informer()
	statusController := NewUplinkStateGatewayStatusController(
		nodeName,
		uplinkClient,
		uplinkStateInformer,
	)
	t.Cleanup(statusController.Stop)
	nodeLister := v1mocks.NewNodeLister(t)
	nodeInformer := coreinformermocks.NewNodeInformer(t)
	nodeInformer.On("Lister").Return(nodeLister)
	defaultGateway := &gateway{openflowManager: getDummyOpenflowManager()}
	udnGateway, err := NewUserDefinedNetworkGateway(
		netInfo,
		node,
		nodeInformer,
		nil,
		vrfmanager.NewController(nil),
		nil,
		defaultGateway,
		nil,
		uplinkStateInformer,
		statusController,
	)
	if err != nil {
		t.Fatalf("failed to create Uplink gateway: %v", err)
	}

	informerStop := make(chan struct{})
	informerFactory.Start(informerStop)
	if !cache.WaitForCacheSync(informerStop, uplinkStateInformer.Informer().HasSynced) {
		close(informerStop)
		t.Fatal("failed to sync UplinkState informer")
	}
	t.Cleanup(func() {
		udnGateway.Stop()
		close(informerStop)
	})
	return udnGateway, netlinkOps, nodeLister
}

func expectMissingGatewayVRF(
	netlinkOps *utilmocks.NetLinkOps,
	udnGateway *UserDefinedNetworkGateway,
) {
	notFoundErr := errors.New("link not found")
	netlinkOps.On("LinkByName", util.GetNetworkVRFName(udnGateway.NetInfo)).
		Return(nil, notFoundErr).Once()
	netlinkOps.On("IsLinkNotFoundError", notFoundErr).Return(true).Once()
}

func TestUserDefinedNetworkGatewayReconcilesUplinkConfiguration(t *testing.T) {
	if err := config.PrepareTestConfig(); err != nil {
		t.Fatalf("failed to prepare test config: %v", err)
	}
	t.Cleanup(func() { _ = config.PrepareTestConfig() })
	config.OvnKubeNode.Mode = types.NodeModeDPU
	config.Gateway.Mode = config.GatewayModeShared
	config.Gateway.V4MasqueradeSubnet = "169.254.0.0/17"
	config.IPv4Mode = true
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true
	config.OVNKubernetesFeature.EnableUplink = true

	t.Run("unresolved state unconfigures existing programming", func(t *testing.T) {
		state := resolvedGatewayUplinkState("uplink1", "node-a")
		state.Status.Conditions[0].Status = metav1.ConditionFalse
		udnGateway, netlinkOps, _ := newUplinkGatewayReconcileHarness(t, state)
		udnGateway.uplinkGatewayCleanupRequired = true
		udnGateway.uplinkStateUID = "old-uid"
		udnGateway.uplinkFingerprint = uplinkGatewayFingerprint{bridgeName: "old-bridge"}
		expectMissingGatewayVRF(netlinkOps, udnGateway)

		_, observedFingerprint, err :=
			udnGateway.reconcileUplinkConfiguration()
		if err == nil || uplinkGatewayFailureReason(err) !=
			uplinkv1alpha1.UplinkStateReasonGatewayProgrammingFailed {
			t.Fatalf("unexpected unresolved result: %v", err)
		}
		if udnGateway.uplinkGatewayCleanupRequired {
			t.Fatal("unresolved UplinkState retained old gateway programming")
		}
		if udnGateway.uplinkFingerprint != (uplinkGatewayFingerprint{}) {
			t.Fatalf("unresolved UplinkState retained fingerprint: %#v",
				udnGateway.uplinkFingerprint)
		}
		if udnGateway.uplinkStateUID != "" {
			t.Fatalf("unresolved UplinkState retained UID: %s",
				udnGateway.uplinkStateUID)
		}
		if observedFingerprint != uplinkGatewayFingerprintFromState(state) {
			t.Fatal("unresolved UplinkState did not produce a status fingerprint")
		}
	})

	t.Run("unconfiguration failure retains cleanup ownership", func(t *testing.T) {
		state := resolvedGatewayUplinkState("uplink1", "node-a")
		state.Status.Conditions[0].Status = metav1.ConditionFalse
		udnGateway, netlinkOps, _ := newUplinkGatewayReconcileHarness(t, state)
		current := uplinkGatewayFingerprint{bridgeName: "old-bridge"}
		udnGateway.uplinkGatewayCleanupRequired = true
		udnGateway.uplinkStateUID = "old-uid"
		udnGateway.uplinkFingerprint = current
		withdrawErr := errors.New("failed to remove old programming")
		netlinkOps.On("LinkByName", util.GetNetworkVRFName(udnGateway.NetInfo)).
			Return(nil, withdrawErr).Once()
		netlinkOps.On("IsLinkNotFoundError", withdrawErr).Return(false).Once()

		_, _, err := udnGateway.reconcileUplinkConfiguration()
		if err == nil || !strings.Contains(err.Error(), withdrawErr.Error()) ||
			uplinkGatewayFailureReason(err) !=
				uplinkv1alpha1.UplinkStateReasonGatewayProgrammingFailed {
			t.Fatalf("unexpected unconfiguration failure: %v", err)
		}
		if !udnGateway.uplinkGatewayCleanupRequired {
			t.Fatal("failed withdrawal released ownership of old programming")
		}
		// The handled identity is kept for status reporting; the partial
		// marker keeps the next reconcile from matching it.
		if !udnGateway.uplinkGatewayPartial {
			t.Fatal("failed withdrawal did not mark the programming partial")
		}
		if udnGateway.uplinkFingerprint != current {
			t.Fatalf("failed withdrawal changed the handled fingerprint to %#v", udnGateway.uplinkFingerprint)
		}
		if udnGateway.uplinkStateUID != "old-uid" {
			t.Fatal("failed withdrawal released the old UplinkState identity")
		}
	})

	t.Run("partial programming is rebuilt even when its fingerprint matches", func(t *testing.T) {
		state := resolvedGatewayUplinkState("uplink1", "node-a")
		udnGateway, netlinkOps, nodeLister := newUplinkGatewayReconcileHarness(t, state)
		udnGateway.uplinkGatewayCleanupRequired = true
		udnGateway.uplinkGatewayPartial = true
		udnGateway.uplinkStateUID = state.UID
		udnGateway.uplinkFingerprint = uplinkGatewayFingerprintFromState(state)
		expectMissingGatewayVRF(netlinkOps, udnGateway)
		addErr := errors.New("management port lookup failed")
		nodeLister.On("Get", "node-a").Return(nil, addErr).Once()

		_, _, err := udnGateway.reconcileUplinkConfiguration()
		if err == nil || !strings.Contains(err.Error(), addErr.Error()) {
			t.Fatalf("partial programming was not rebuilt: %v", err)
		}
	})

	t.Run("cleanup failure is reported against the handled UplinkState", func(t *testing.T) {
		state := resolvedGatewayUplinkState("uplink1", "node-a")
		state.UID = "handled-uid"
		udnGateway, netlinkOps, _ := newUplinkGatewayReconcileHarness(t, state)
		udnGateway.uplinkGatewayCleanupRequired = true
		udnGateway.uplinkStateUID = state.UID
		udnGateway.uplinkFingerprint = uplinkGatewayFingerprintFromState(state)
		cleanupErr := errors.New("failed to remove old programming")
		netlinkOps.On("LinkByName", util.GetNetworkVRFName(udnGateway.NetInfo)).
			Return(nil, cleanupErr).Once()
		netlinkOps.On("IsLinkNotFoundError", cleanupErr).Return(false).Once()

		if err := udnGateway.Cleanup(); err == nil || !strings.Contains(err.Error(), cleanupErr.Error()) {
			t.Fatalf("unexpected cleanup result: %v", err)
		}

		statusController := udnGateway.uplinkStateGatewayStatusController
		statusController.mutex.Lock()
		defer statusController.mutex.Unlock()
		uplinkState := statusController.uplinks["uplink1"]
		if uplinkState == nil || uplinkState.networks[udnGateway.GetNetworkName()] == nil {
			t.Fatal("cleanup failure was not reported")
		}
		if phase := uplinkState.networks[udnGateway.GetNetworkName()].phase; phase != uplinkGatewayNetworkFailed {
			t.Fatalf("cleanup failure reported phase %q", phase)
		}
	})

	t.Run("changed default gateways update routes without replacing the gateway", func(t *testing.T) {
		state := resolvedGatewayUplinkState("uplink1", "node-a")
		state.UID = "new-uid"
		udnGateway, _, _ := newUplinkGatewayReconcileHarness(t, state)
		current := uplinkGatewayFingerprintFromState(state)
		current.defaultGateways = "192.0.2.254"
		udnGateway.uplinkGatewayCleanupRequired = true
		udnGateway.uplinkStateUID = "old-uid"
		udnGateway.uplinkFingerprint = current

		// No netlink expectation is set: any teardown or setup call fails.
		if _, _, err := udnGateway.reconcileUplinkConfiguration(); err != nil {
			t.Fatalf("changed default gateways failed: %v", err)
		}
		if !udnGateway.uplinkGatewayCleanupRequired {
			t.Fatal("changed default gateways released gateway programming")
		}
		if udnGateway.uplinkFingerprint != uplinkGatewayFingerprintFromState(state) {
			t.Fatalf("changed default gateways left fingerprint %#v", udnGateway.uplinkFingerprint)
		}
		if udnGateway.uplinkStateUID != state.UID {
			t.Fatal("changed default gateways did not adopt the handled UplinkState identity")
		}
		var nextHops []string
		for _, nextHop := range udnGateway.nextHops {
			nextHops = append(nextHops, nextHop.String())
		}
		var published []string
		for _, gateway := range state.Status.DefaultGateways {
			published = append(published, string(gateway))
		}
		if strings.Join(nextHops, ",") != strings.Join(published, ",") {
			t.Fatalf("next hops %v do not follow the published gateways %v", nextHops, published)
		}
	})

	t.Run("failed in-place default gateway update marks the programming partial", func(t *testing.T) {
		// Only the host modes manage the default routes of the network VRF.
		config.OvnKubeNode.Mode = types.NodeModeFull
		t.Cleanup(func() { config.OvnKubeNode.Mode = types.NodeModeDPU })
		state := resolvedGatewayUplinkState("uplink1", "node-a")
		state.UID = "new-uid"
		udnGateway, netlinkOps, _ := newUplinkGatewayReconcileHarness(t, state)
		current := uplinkGatewayFingerprintFromState(state)
		current.defaultGateways = "192.0.2.254"
		udnGateway.uplinkGatewayCleanupRequired = true
		udnGateway.uplinkStateUID = "old-uid"
		udnGateway.uplinkFingerprint = current
		// The IPv4 default is replaced through the VRF manager; the network
		// has no IPv6 subnet, so no family is left without a gateway.
		updateErr := errors.New("vrf lookup failed")
		netlinkOps.On("LinkByName", util.GetNetworkVRFName(udnGateway.NetInfo)).
			Return(nil, updateErr).Once()

		_, _, err := udnGateway.reconcileUplinkConfiguration()
		if err == nil || !strings.Contains(err.Error(), updateErr.Error()) ||
			uplinkGatewayFailureReason(err) !=
				uplinkv1alpha1.UplinkStateReasonGatewayProgrammingFailed {
			t.Fatalf("unexpected in-place update failure: %v", err)
		}
		if !udnGateway.uplinkGatewayPartial {
			t.Fatal("failed in-place update did not mark the programming partial")
		}
		if udnGateway.uplinkFingerprint != current || udnGateway.uplinkStateUID != "old-uid" {
			t.Fatalf("failed in-place update changed the handled identity to %s %#v",
				udnGateway.uplinkStateUID, udnGateway.uplinkFingerprint)
		}
	})

	t.Run("unchanged configuration preserves programming", func(t *testing.T) {
		state := resolvedGatewayUplinkState("uplink1", "node-a")
		state.UID = "new-uid"
		udnGateway, _, _ := newUplinkGatewayReconcileHarness(t, state)
		current := uplinkGatewayFingerprintFromState(state)
		udnGateway.uplinkGatewayCleanupRequired = true
		udnGateway.uplinkStateUID = "old-uid"
		udnGateway.uplinkFingerprint = current

		if _, _, err := udnGateway.reconcileUplinkConfiguration(); err != nil {
			t.Fatalf("unchanged configuration failed: %v", err)
		}
		if !udnGateway.uplinkGatewayCleanupRequired || udnGateway.uplinkFingerprint != current {
			t.Fatal("unchanged configuration disturbed gateway programming")
		}
		if udnGateway.uplinkStateUID != state.UID {
			t.Fatal("unchanged configuration did not adopt the handled UplinkState identity")
		}
	})

	t.Run("advertisement reconciliation reports handled UplinkState identity", func(t *testing.T) {
		state := resolvedGatewayUplinkState("uplink1", "node-a")
		state.UID = "new-uid"
		udnGateway, _, _ := newUplinkGatewayReconcileHarness(t, state)
		udnGateway.uplinkGatewayCleanupRequired = true
		udnGateway.uplinkStateUID = "old-uid"
		udnGateway.uplinkFingerprint = uplinkGatewayFingerprintFromState(state)

		if err := udnGateway.reconcileGateway(udnGateway.GetNetworkName()); err == nil {
			t.Fatal("expected advertisement reconciliation to fail without bridge configuration")
		}

		statusController := udnGateway.uplinkStateGatewayStatusController
		statusController.mutex.Lock()
		defer statusController.mutex.Unlock()
		uplinkState := statusController.uplinks["uplink1"]
		if uplinkState != nil &&
			uplinkState.networks[udnGateway.GetNetworkName()] != nil {
			t.Fatal("advertisement reconciliation reported against an unhandled UplinkState")
		}
	})

	t.Run("changed configuration unconfigures before replacement", func(t *testing.T) {
		state := resolvedGatewayUplinkState("uplink1", "node-a")
		oldState := state.DeepCopy()
		oldState.Status.IPAddresses = []uplinkv1alpha1.IPAddressCIDR{"192.0.2.9/24"}
		udnGateway, netlinkOps, nodeLister := newUplinkGatewayReconcileHarness(t, state)
		udnGateway.uplinkGatewayCleanupRequired = true
		udnGateway.uplinkStateUID = "old-uid"
		udnGateway.uplinkFingerprint = uplinkGatewayFingerprintFromState(oldState)
		expectMissingGatewayVRF(netlinkOps, udnGateway)
		addErr := errors.New("management port lookup failed")
		nodeLister.On("Get", "node-a").Return(nil, addErr).Once()

		_, _, err := udnGateway.reconcileUplinkConfiguration()
		if err == nil || !strings.Contains(err.Error(), addErr.Error()) {
			t.Fatalf("replacement did not reach gateway programming: %v", err)
		}
		if !udnGateway.uplinkGatewayCleanupRequired {
			t.Fatal("partial replacement was not retained for retry cleanup")
		}
		if udnGateway.uplinkFingerprint != (uplinkGatewayFingerprint{}) {
			t.Fatalf("failed replacement recorded a new fingerprint: %#v",
				udnGateway.uplinkFingerprint)
		}
		if udnGateway.uplinkStateUID != "" {
			t.Fatalf("failed replacement retained UID: %s",
				udnGateway.uplinkStateUID)
		}
	})

	t.Run("resolved state starts initial programming", func(t *testing.T) {
		state := resolvedGatewayUplinkState("uplink1", "node-a")
		udnGateway, _, nodeLister := newUplinkGatewayReconcileHarness(t, state)
		addErr := errors.New("management port lookup failed")
		nodeLister.On("Get", "node-a").Return(nil, addErr).Once()

		_, observedFingerprint, err :=
			udnGateway.reconcileUplinkConfiguration()
		if err == nil || !strings.Contains(err.Error(), addErr.Error()) {
			t.Fatalf("initial reconciliation did not reach gateway programming: %v", err)
		}
		if !udnGateway.uplinkGatewayCleanupRequired {
			t.Fatal("partial initial programming was not retained for retry cleanup")
		}
		if udnGateway.uplinkFingerprint != (uplinkGatewayFingerprint{}) {
			t.Fatalf("failed programming stored fingerprint: %#v",
				udnGateway.uplinkFingerprint)
		}
		if observedFingerprint != uplinkGatewayFingerprintFromState(state) {
			t.Fatal("failed programming did not produce a status fingerprint")
		}
	})
}

func TestNewUserDefinedNetworkGatewayRequiresDependencies(t *testing.T) {
	if err := config.PrepareTestConfig(); err != nil {
		t.Fatalf("failed to prepare test config: %v", err)
	}
	t.Cleanup(func() {
		_ = config.PrepareTestConfig()
	})
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true
	config.OVNKubernetesFeature.EnableUplink = true
	config.Gateway.Mode = config.GatewayModeShared

	nad := generateUplinkNAD("red", "rednad", "greenamespace",
		types.Layer3Topology, "100.128.0.0/16/24", types.NetworkRolePrimary, "uplink1")
	netInfo, err := util.ParseNADInfo(nad)
	if err != nil {
		t.Fatalf("failed to parse NAD: %v", err)
	}

	_, err = NewUserDefinedNetworkGateway(
		netInfo, nil, nil, nil, nil, nil, nil, nil, nil, nil,
	)
	if err == nil || !strings.Contains(err.Error(), "node informer has not been provided") {
		t.Fatalf("expected missing node informer error, got %v", err)
	}

	_, err = NewUserDefinedNetworkGateway(
		netInfo, nil, emptyNodeInformer(), nil, nil, nil, nil, nil, nil, nil,
	)
	if err == nil || !strings.Contains(err.Error(), "UplinkState informer has not been provided") {
		t.Fatalf("expected missing UplinkState informer error, got %v", err)
	}

	uplinkStateInformer, statusController := newGatewayUplinkDependencies("node-a")
	t.Cleanup(statusController.Stop)
	_, err = NewUserDefinedNetworkGateway(
		netInfo, nil, emptyNodeInformer(), nil, nil, nil, nil, nil, uplinkStateInformer, nil,
	)
	if err == nil || !strings.Contains(err.Error(), "UplinkState gateway status controller has not been provided") {
		t.Fatalf("expected missing UplinkState gateway status controller error, got %v", err)
	}
}

func TestGetDefaultRouteDoesNotFallBackForUplinkWithoutNextHops(t *testing.T) {
	if err := config.PrepareTestConfig(); err != nil {
		t.Fatalf("failed to prepare test config: %v", err)
	}
	t.Cleanup(func() {
		_ = config.PrepareTestConfig()
	})
	config.IPv4Mode = true
	config.Default.MTU = 1400
	config.Gateway.Mode = config.GatewayModeShared
	nad := generateUplinkNAD("red", "rednad", "greenamespace",
		types.Layer3Topology, "100.128.0.0/16/24", types.NetworkRolePrimary, "uplink1")
	netInfo, err := util.ParseNADInfo(nad)
	if err != nil {
		t.Fatalf("failed to parse NAD: %v", err)
	}

	udng := &UserDefinedNetworkGateway{
		NetInfo:          netInfo,
		gwInterfaceIndex: 77,
	}
	routes, err := udng.getDefaultRoute()
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if len(routes) != 0 {
		t.Fatalf("expected no default routes, got %v", routes)
	}
}

func getDeletionFakeOVSCommands(fexec *ovntest.FakeExec, mgtPort string) {
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovs-vsctl --timeout=15 --if-exists del-port br-int " + mgtPort,
	})
}

func setManagementPortFakeCommands(fexec *ovntest.FakeExec, nodeName string) {
	// management port commands
	mpPortName := types.K8sMgmtIntfName
	mpPortRepName := types.K8sMgmtIntfName + "_0"
	mpPortLegacyName := types.K8sPrefix + nodeName
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovs-vsctl --timeout=15 --no-headings --data bare --format csv --columns type,name find Interface name=" + mpPortName,
		Output: "internal," + mpPortName,
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovs-vsctl --timeout=15 --no-headings --data bare --format csv --columns type,name find Interface name=" + mpPortRepName,
		Output: "internal," + mpPortRepName,
	})
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovs-vsctl --timeout=15 -- --if-exists del-port br-int " + mpPortLegacyName + " -- --may-exist add-port br-int " + mpPortName + " -- set interface " + mpPortName + " mac=\"0a:58:64:80:00:02\" type=internal mtu_request=1400 external-ids:iface-id=" + mpPortLegacyName,
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "sysctl -w net.ipv4.conf.ovn-k8s-mp0.forwarding = 1",
		Output: "net.ipv4.conf.ovn-k8s-mp0.forwarding = 1",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ip route replace table 7 172.16.1.0/24 via 100.128.0.1 dev ovn-k8s-mp0",
		Output: "0",
	})
	if config.IPv6Mode {
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ip route replace table 7 fd02::/112 via ae70::1 dev ovn-k8s-mp0",
			Output: "0",
		})
	}
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ip -4 rule",
		Output: "0",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ip -4 rule add fwmark 0x1745ec lookup 7 prio 30",
		Output: "0",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ip -6 rule",
		Output: "0",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ip -6 rule add fwmark 0x1745ec lookup 7 prio 30",
		Output: "0",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "sysctl -w net.ipv4.conf.ovn-k8s-mp0.rp_filter = 2",
		Output: "net.ipv4.conf.ovn-k8s-mp0.rp_filter = 2",
	})
}

func setUpGatewayFakeOVSCommands(fexec *ovntest.FakeExec) {
	// GetNicName lookups (list-ports / get Port Interfaces / get Interface Type)
	// are now served by the libovsdb harness seeded in BeforeEach.
	// getGatewayNextHops rewrites eth0 → breth0; GetPortBridge hides the
	// bridge local port, so NewBridgeConfiguration takes the "is a bridge"
	// path and getIntfName verifies eth0's ofport first.
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovs-vsctl --timeout=15 get interface eth0 ofport",
		Output: "7",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovs-vsctl --timeout=15 --if-exists get interface breth0 mac_in_use",
		Output: "00:00:00:55:66:99",
	})
	if config.IPv4Mode {
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "sysctl -w net.ipv4.conf.breth0.forwarding = 1",
			Output: "net.ipv4.conf.breth0.forwarding = 1",
		})
	}
	// ovn-bridge-mappings get/set are now handled via libovsdb in
	// bridgeconfig.bridgedGatewayNodeSetup; no fexec entries needed.
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovs-vsctl --timeout=15 --if-exists get Open_vSwitch . external_ids:system-id",
		Output: "cb9ec8fa-b409-4ef3-9f42-d9283c47aac6",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovs-appctl -t /var/run/openvswitch/ovs-vswitchd.1234.ctl dpif/show-dp-features breth0",
		Output: "Check pkt length action: Yes",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovs-vsctl --timeout=15 --if-exists get Open_vSwitch . other_config:hw-offload",
		Output: "false",
	})
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovs-appctl -t /var/run/openvswitch/ovs-vswitchd.1234.ctl fdb/add breth0 breth0 0 00:00:00:55:66:99",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovs-vsctl --timeout=15 get Interface patch-breth0_worker1-to-br-int ofport",
		Output: "5",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovs-vsctl --timeout=15 get interface eth0 ofport",
		Output: "7",
	})
	// newNodePortWatcher() looks up the physical interface's ofport via exec;
	// this is unrelated to checkPorts(), which now reads it from libovsdb.
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovs-vsctl --timeout=15 --if-exists get interface eth0 ofport",
		Output: "7",
	})
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovs-ofctl -O OpenFlow13 --bundle replace-flows breth0 -",
	})
	// SyncNoFlood calls GetNoFloodPorts (dump-ports-desc) during syncFlows.
	// Before any UDN is added, no ports have NO_FLOOD.
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovs-ofctl dump-ports-desc breth0",
		Output: " 5(patch-breth0_w): addr:00:00:00:00:00:00\n     config:     0\n     state:      LIVE\n",
	})
}

func setUpUDNOpenflowManagerFakeOVSCommands(fexec *ovntest.FakeExec) {
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovs-vsctl --timeout=15 get Interface patch-breth0_bluenet_worker1-to-br-int ofport",
		Output: "15",
	})
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovs-ofctl mod-port breth0 15 no-flood",
	})
}

// addOVSPatchPortInterface creates an OVS Port+Interface with the given ofport
// and attaches it to bridgeName, so checkPorts() can find it via libovsdb.
func addOVSPatchPortInterface(ovsClient libovsdbclient.Client, bridgeName, portName string, ofport int) {
	GinkgoHelper()
	iface := &vswitchd.Interface{UUID: "iface-" + portName, Name: portName, Type: "patch", Ofport: &ofport}
	ifaceOps, err := ovsClient.Create(iface)
	Expect(err).NotTo(HaveOccurred())

	port := &vswitchd.Port{UUID: "port-" + portName, Name: portName, Interfaces: []string{iface.UUID}}
	portOps, err := ovsClient.Create(port)
	Expect(err).NotTo(HaveOccurred())

	bridge := &vswitchd.Bridge{Name: bridgeName}
	Expect(ovsClient.Get(context.Background(), bridge)).To(Succeed())
	mutateOps, err := ovsClient.Where(bridge).Mutate(bridge, model.Mutation{
		Field:   &bridge.Ports,
		Mutator: ovsdb.MutateOperationInsert,
		Value:   []string{port.UUID},
	})
	Expect(err).NotTo(HaveOccurred())

	ops := append(ifaceOps, portOps...)
	ops = append(ops, mutateOps...)
	_, err = ovsops.TransactAndCheck(ovsClient, ops)
	Expect(err).NotTo(HaveOccurred())
}

// removeOVSPatchPortInterface deletes the OVS Port+Interface previously added
// with addOVSPatchPortInterface, simulating ovn-controller tearing down a
// stale patch port.
func removeOVSPatchPortInterface(ovsClient libovsdbclient.Client, bridgeName, portName string) {
	GinkgoHelper()
	Expect(ovsops.DeletePortWithInterfaces(ovsClient, bridgeName, portName)).To(Succeed())
}

func deleteStaleManagementPortFakeCommands(fexec *ovntest.FakeExec, mgtPort string) {
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd: fmt.Sprintf("ovs-vsctl --timeout=15 --no-headings --data bare --format csv --columns name find Interface external-ids:%s=%s", types.OvnManagementPortNameExternalID, mgtPort),
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd: fmt.Sprintf("ovs-vsctl --timeout=15 --no-headings --data bare --format csv --columns name find Interface type=internal name=%s", mgtPort),
	})
}

func openflowManagerCheckPorts(ofMgr *openflowManager) {
	GinkgoHelper()
	netConfigs, uplink, ofPortPhys := ofMgr.getDefaultBridgePortConfigurations()
	sort.SliceStable(netConfigs, func(i, j int) bool {
		return netConfigs[i].PatchPort < netConfigs[j].PatchPort
	})
	Expect(checkPorts(ofMgr.ovsClient, netConfigs, uplink, ofPortPhys)).To(Succeed())
}

// getDummyOpenflowManager creates a test openflow manager. Optional bridgeIPs
// can be passed to configure gateway bridge IPs for node network route tests.
func getDummyOpenflowManager(bridgeIPs ...*net.IPNet) *openflowManager {
	gwBridge := bridgeconfig.TestBridgeConfig("breth0")
	if len(bridgeIPs) > 0 {
		gwBridge.SetIPs(bridgeIPs)
	}
	ofm := &openflowManager{
		defaultBridge: newOpenflowBridge(gwBridge),
		uplinkBridges: map[string]*openflowBridge{},
	}
	return ofm
}

func generateNoOverlayNAD(networkName, name, namespace, topology, cidr, role, outboundSNAT string) *nadapi.NetworkAttachmentDefinition {
	return ovntest.GenerateNADWithConfig(name, namespace, fmt.Sprintf(
		`
{
        "cniVersion": "1.1.0",
        "name": %q,
        "type": "ovn-k8s-cni-overlay",
        "topology": %q,
        "subnets": %q,
        "mtu": 1300,
        "netAttachDefName": %q,
        "role": %q,
        "transport": %q,
        "outboundSNAT": %q
}
`,
		networkName,
		topology,
		cidr,
		fmt.Sprintf("%s/%s", namespace, name),
		role,
		types.NetworkTransportNoOverlay,
		outboundSNAT,
	))
}

func generateUplinkNAD(networkName, name, namespace, topology, cidr, role, uplink string) *nadapi.NetworkAttachmentDefinition {
	return ovntest.GenerateNADWithConfig(name, namespace, fmt.Sprintf(
		`
{
        "cniVersion": "1.1.0",
        "name": %q,
        "type": "ovn-k8s-cni-overlay",
        "topology": %q,
        "subnets": %q,
        "mtu": 1300,
        "netAttachDefName": %q,
        "role": %q,
        "uplink": %q
}
`,
		networkName,
		topology,
		cidr,
		fmt.Sprintf("%s/%s", namespace, name),
		role,
		uplink,
	))
}

func newGatewayUplinkStateAndLister(
	uplinkName, nodeName string,
) (*uplinkv1alpha1.UplinkState, uplinklisters.UplinkStateLister) {
	state := &uplinkv1alpha1.UplinkState{
		ObjectMeta: metav1.ObjectMeta{
			Name: uplinkutil.StateName(uplinkName, nodeName),
		},
		Spec: uplinkv1alpha1.UplinkStateSpec{
			UplinkName: uplinkName,
			NodeName:   nodeName,
		},
		Status: uplinkv1alpha1.UplinkStateStatus{
			Type:              uplinkv1alpha1.UplinkTypeOVSBridge,
			HostInterfaceName: "ovsbr1",
			OVSBridge: &uplinkv1alpha1.OVSBridgeStatus{
				Name: "ovsbr1",
			},
			Conditions: []metav1.Condition{
				{
					Type:   uplinkv1alpha1.UplinkStateConditionResolved,
					Status: metav1.ConditionTrue,
				},
			},
		},
	}
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	Expect(indexer.Add(state)).To(Succeed())
	return state, uplinklisters.NewUplinkStateLister(indexer)
}

func newGatewayUplinkDependencies(
	nodeName string,
) (uplinkinformers.UplinkStateInformer, *UplinkStateGatewayStatusController) {
	uplinkClient := uplinkfake.NewSimpleClientset()
	informerFactory := uplinkinformerfactory.NewSharedInformerFactory(uplinkClient, 0)
	uplinkStateInformer := informerFactory.K8s().V1alpha1().UplinkStates()
	return uplinkStateInformer, NewUplinkStateGatewayStatusController(
		nodeName,
		uplinkClient,
		uplinkStateInformer,
	)
}

func noOverlayLayer3NetInfo(t *testing.T) util.NetInfo {
	t.Helper()
	nad := generateNoOverlayNAD("bluenet", "rednad", "greenamespace",
		types.Layer3Topology, "100.128.0.0/16/24,ae70::/60/64", types.NetworkRolePrimary, types.NoOverlaySNATDisabled)
	ovntest.AnnotateNADWithNetworkID("3", nad)
	netInfo, err := util.ParseNADInfo(nad)
	if err != nil {
		t.Fatalf("failed to parse NAD: %v", err)
	}
	return netInfo
}

func TestShouldAddUDNServiceMarkRules(t *testing.T) {
	if err := config.PrepareTestConfig(); err != nil {
		t.Fatalf("failed to prepare test config: %v", err)
	}
	config.IPv4Mode = true
	config.IPv6Mode = true
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true

	nodeName := "worker1"
	nad := ovntest.GenerateNAD("bluenet", "rednad", "greenamespace",
		types.Layer3Topology, "100.128.0.0/16/24,ae70::/60/64", types.NetworkRolePrimary)
	ovntest.AnnotateNADWithNetworkID("3", nad)
	netInfo, err := util.ParseNADInfo(nad)
	if err != nil {
		t.Fatalf("failed to parse NAD: %v", err)
	}

	if !shouldAddUDNServiceMarkRules(netInfo, nodeName) {
		t.Fatalf("expected service mark rules for an unadvertised primary network")
	}

	defaultVRFNetInfo := util.NewMutableNetInfo(netInfo)
	defaultVRFNetInfo.SetPodNetworkAdvertisedVRFs(map[string][]string{
		nodeName: {types.DefaultNetworkName},
	})
	if !shouldAddUDNServiceMarkRules(defaultVRFNetInfo, nodeName) {
		t.Fatalf("expected service mark rules for a network advertised to the default VRF")
	}

	nonDefaultVRFNetInfo := util.NewMutableNetInfo(netInfo)
	nonDefaultVRFNetInfo.SetPodNetworkAdvertisedVRFs(map[string][]string{
		nodeName: {netInfo.GetNetworkName()},
	})
	if shouldAddUDNServiceMarkRules(nonDefaultVRFNetInfo, nodeName) {
		t.Fatalf("expected no service mark rules for a network advertised to a non-default VRF")
	}

	noOverlayNetInfo := noOverlayLayer3NetInfo(t)
	if !shouldAddUDNServiceMarkRules(noOverlayNetInfo, nodeName) {
		t.Fatalf("expected service mark rules for no-overlay network without explicit advertised VRF")
	}

	noOverlayDefaultVRFNetInfo := util.NewMutableNetInfo(noOverlayNetInfo)
	noOverlayDefaultVRFNetInfo.SetPodNetworkAdvertisedVRFs(map[string][]string{
		nodeName: {types.DefaultNetworkName},
	})
	if !shouldAddUDNServiceMarkRules(noOverlayDefaultVRFNetInfo, nodeName) {
		t.Fatalf("expected service mark rules for no-overlay network advertised to the default VRF")
	}

	noOverlayNonDefaultVRFNetInfo := util.NewMutableNetInfo(noOverlayNetInfo)
	noOverlayNonDefaultVRFNetInfo.SetPodNetworkAdvertisedVRFs(map[string][]string{
		nodeName: {noOverlayNetInfo.GetNetworkName()},
	})
	if shouldAddUDNServiceMarkRules(noOverlayNonDefaultVRFNetInfo, nodeName) {
		t.Fatalf("expected no service mark rules for no-overlay network advertised to a non-default VRF")
	}
}

var _ = Describe("UserDefinedNetworkGateway", func() {
	var (
		netName               = "bluenet"
		netID                 = "3"
		nodeName       string = "worker1"
		mgtPortMAC     string = "00:00:00:55:66:77" // dummy MAC used for fake commands
		fexec          *ovntest.FakeExec
		testNS         ns.NetNS
		factoryMock    factoryMocks.NodeWatchFactory
		kubeMock       kubemocks.Interface
		nodeLister     v1mocks.NodeLister
		vrf            *vrfmanager.Controller
		rm             *routemanager.Controller
		ipRulesManager *iprulemanager.Controller
		wg             sync.WaitGroup
		stopCh         chan struct{}
		v4NodeSubnet   = "100.128.0.0/24"
		v6NodeSubnet   = "ae70::/64"
		mgtPort        = fmt.Sprintf("%s%s", types.K8sMgmtIntfNamePrefix, netID)
		v4NodeIP       = "192.168.1.10/24"
		v6NodeIP       = "fc00:f853:ccd:e793::3/64"
		ovsClient      libovsdbclient.Client
		ovsCleanup     *libovsdbtest.Context
	)
	BeforeEach(func() {
		// Restore global default values before each testcase
		err := config.PrepareTestConfig()
		Expect(err).NotTo(HaveOccurred())
		// Skip the encap-update path inside addressManager.sync() — these tests
		// don't fake ovn-appctl and aren't exercising encap reconciliation.
		config.Default.EncapIP = "test-encap-ip"
		var ovsErr error
		ovsClient, ovsCleanup, ovsErr = libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{
			OVSData: []libovsdbtest.TestData{
				&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{"breth0-uuid"}},
				&vswitchd.Bridge{
					UUID:  "breth0-uuid",
					Name:  "breth0",
					Ports: []string{"breth0-port-uuid", "eth0-port-uuid"},
				},
				// Local port of the bridge (hidden by GetPortBridge / port-to-br).
				&vswitchd.Port{UUID: "breth0-port-uuid", Name: "breth0", Interfaces: []string{"breth0-iface-uuid"}},
				&vswitchd.Interface{UUID: "breth0-iface-uuid", Name: "breth0", Type: "internal", Ofport: ptr.To(65534)},
				// Physical uplink: system-typed so GetNicName resolves eth0.
				&vswitchd.Port{UUID: "eth0-port-uuid", Name: "eth0", Interfaces: []string{"eth0-iface-uuid"}},
				&vswitchd.Interface{UUID: "eth0-iface-uuid", Name: "eth0", Type: "system", Ofport: ptr.To(7)},
			},
		})
		Expect(ovsErr).NotTo(HaveOccurred())
		// Ensure gateway tests never rely on host iptables binaries.
		util.SetFakeIPTablesHelpers()

		// Set dual-stack service CIDRs directly after PrepareTestConfig
		config.Kubernetes.ServiceCIDRs = ovntest.MustParseIPNets("172.16.1.0/24", "fd02::/112")

		config.OVNKubernetesFeature.EnableMultiNetwork = true
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		config.OVNKubernetesFeature.EnableUplink = true
		config.OvnKubeNode.MgmtPortDPResourceName = ""
		// Use a larger masq subnet to allow OF manager to allocate IPs for UDNs.
		config.Gateway.V6MasqueradeSubnet = "fd69::/112"
		config.Gateway.V4MasqueradeSubnet = "169.254.0.0/17"
		// Set up a fake vsctl command mock interface
		fexec = ovntest.NewFakeExec()
		// Setup mock filesystem for ovs-vswitchd.pid file needed by ovs-appctl commands
		Expect(util.SetupMockOVSPidFile()).To(Succeed())
		Expect(util.SetExec(fexec)).To(Succeed())
		// Set up a fake k8sMgmt interface
		testNS, err = testutils.NewNS()
		Expect(err).NotTo(HaveOccurred())
		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()
			// given the netdevice is created using add-port command in OVS
			// we need to mock create a dummy link for things to work in unit tests
			ovntest.AddLink(mgtPort)
			link := ovntest.AddLink("breth0")
			addr, _ := netlink.ParseAddr(v4NodeIP)
			err = netlink.AddrAdd(link, addr)
			if err != nil {
				return err
			}
			addr, _ = netlink.ParseAddr(v6NodeIP)
			return netlink.AddrAdd(link, addr)
		})
		Expect(err).NotTo(HaveOccurred())
		factoryMock = factoryMocks.NodeWatchFactory{}
		nodeInformer := coreinformermocks.NodeInformer{}
		factoryMock.On("NodeCoreInformer").Return(&nodeInformer)
		nodeLister = v1mocks.NodeLister{}
		nodeInformer.On("Lister").Return(&nodeLister)
		kubeMock = kubemocks.Interface{}
		wg = sync.WaitGroup{}
		stopCh = make(chan struct{})
		rm = routemanager.NewController()
		vrf = vrfmanager.NewController(rm)
		wg.Add(1)
		go func() {
			defer GinkgoRecover()
			defer wg.Done()
			err := testNS.Do(func(ns.NetNS) error {
				return vrf.Run(stopCh, &wg)
			})
			Expect(err).NotTo(HaveOccurred())
		}()
		ipRulesManager = iprulemanager.NewController(true, true)
		wg.Add(1)
		go func() {
			defer GinkgoRecover()
			defer wg.Done()
			err := testNS.Do(func(ns.NetNS) error {
				ipRulesManager.Run(stopCh, 4*time.Minute)
				return nil
			})
			Expect(err).NotTo(HaveOccurred())
		}()
	})
	AfterEach(func() {
		close(stopCh)
		wg.Wait()
		ovsCleanup.Cleanup()
		Expect(testNS.Close()).To(Succeed())
		Expect(testutils.UnmountNS(testNS)).To(Succeed())
	})
	ovntest.OnSupportedPlatformsIt("should create management port for a L3 user defined network", func() {
		config.IPv6Mode = true
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
				Annotations: map[string]string{
					"k8s.ovn.org/network-ids":  fmt.Sprintf("{\"%s\": \"%s\"}", netName, netID),
					"k8s.ovn.org/node-subnets": fmt.Sprintf("{\"%s\":[\"%s\", \"%s\"]}", netName, v4NodeSubnet, v6NodeSubnet),
				},
			},
		}
		nad := ovntest.GenerateNAD(netName, "rednad", "greenamespace",
			types.Layer3Topology, "100.128.0.0/16/24,ae70::/60/64", types.NetworkRolePrimary)
		ovntest.AnnotateNADWithNetworkID(netID, nad)
		deleteStaleManagementPortFakeCommands(fexec, mgtPort)
		netInfo, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())
		_, ipNet, err := net.ParseCIDR(v4NodeSubnet)
		Expect(err).NotTo(HaveOccurred())
		mgtPortMAC = util.IPAddrToHWAddr(util.GetNodeManagementIfAddr(ipNet).IP).String()
		getCreationFakeCommands(fexec, mgtPort, mgtPortMAC, netName, nodeName, netInfo.MTU())
		getRPFilterLooseModeFakeCommands(fexec)
		nodeLister.On("Get", mock.AnythingOfType("string")).Return(node, nil)
		factoryMock.On("GetNodeForWindows", "worker1").Return(node, nil)

		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()
			ofm := getDummyOpenflowManager()
			udnGateway, err := NewUserDefinedNetworkGateway(netInfo, node, factoryMock.NodeCoreInformer(),
				&kubeMock, vrf, nil, &gateway{openflowManager: ofm}, nil, nil, nil)
			Expect(err).NotTo(HaveOccurred())
			localSubnets, err := udnGateway.getLocalSubnets()
			Expect(err).NotTo(HaveOccurred())
			udnGateway.mgmtPortController, err = managementport.NewUDNManagementPortController(udnGateway.nodeInformer, udnGateway.node.Name, localSubnets, udnGateway.NetInfo)
			Expect(err).NotTo(HaveOccurred())
			err = udnGateway.mgmtPortController.Create()
			Expect(err).NotTo(HaveOccurred())
			mpLink, err := util.LinkByName(util.GetNetworkScopedK8sMgmtHostIntfName(uint(udnGateway.GetNetworkID())))
			Expect(err).NotTo(HaveOccurred())
			Expect(mpLink).NotTo(BeNil())
			Expect(udnGateway.addUDNManagementPortIPs(mpLink)).Should(Succeed())
			exists, err := util.LinkAddrExist(mpLink, ovntest.MustParseIPNet("100.128.0.2/24"))
			Expect(err).NotTo(HaveOccurred())
			Expect(exists).To(BeTrue())
			exists, err = util.LinkAddrExist(mpLink, ovntest.MustParseIPNet("ae70::2/64"))
			Expect(err).NotTo(HaveOccurred())
			Expect(exists).To(BeTrue())
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
	})
	ovntest.OnSupportedPlatformsIt("should delete management port for a L3 user defined network", func() {
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
				Annotations: map[string]string{
					"k8s.ovn.org/network-ids":  fmt.Sprintf("{\"%s\": \"%s\"}", netName, netID),
					"k8s.ovn.org/node-subnets": fmt.Sprintf("{\"%s\":[\"%s\", \"%s\"]}", netName, v4NodeSubnet, v6NodeSubnet),
				},
			},
		}
		nad := ovntest.GenerateNAD(netName, "rednad", "greenamespace",
			types.Layer3Topology, "100.128.0.0/16/24,ae70::/60/64", types.NetworkRolePrimary)
		// must be defined so that the primary user defined network can match the ip families of the underlying cluster
		config.IPv4Mode = true
		config.IPv6Mode = true
		ovntest.AnnotateNADWithNetworkID(netID, nad)
		deleteStaleManagementPortFakeCommands(fexec, mgtPort)
		netInfo, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())
		getDeletionFakeOVSCommands(fexec, mgtPort)
		nodeLister.On("Get", mock.AnythingOfType("string")).Return(node, nil)
		factoryMock.On("GetNodeForWindows", "worker1").Return(node, nil)
		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()
			ofm := getDummyOpenflowManager()
			udnGateway, err := NewUserDefinedNetworkGateway(netInfo, node, factoryMock.NodeCoreInformer(),
				&kubeMock, vrf, nil, &gateway{openflowManager: ofm}, nil, nil, nil)
			Expect(err).NotTo(HaveOccurred())
			localSubnets, err := udnGateway.getLocalSubnets()
			Expect(err).NotTo(HaveOccurred())
			udnGateway.mgmtPortController, err = managementport.NewUDNManagementPortController(udnGateway.nodeInformer, udnGateway.node.Name, localSubnets, udnGateway.NetInfo)
			Expect(err).NotTo(HaveOccurred())
			Expect(udnGateway.mgmtPortController.Delete()).To(Succeed())
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
	})
	ovntest.OnSupportedPlatformsIt("should create management port for a L2 user defined network", func() {
		config.IPv6Mode = true
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
				Annotations: map[string]string{
					"k8s.ovn.org/network-ids":  fmt.Sprintf("{\"%s\": \"%s\"}", netName, netID),
					"k8s.ovn.org/node-subnets": fmt.Sprintf("{\"%s\":[\"%s\", \"%s\"]}", netName, v4NodeSubnet, v6NodeSubnet),
				},
			},
		}
		nad := ovntest.GenerateNAD(netName, "rednad", "greenamespace",
			types.Layer2Topology, "100.128.0.0/16,ae70::/60", types.NetworkRolePrimary)
		ovntest.AnnotateNADWithNetworkID(netID, nad)
		deleteStaleManagementPortFakeCommands(fexec, mgtPort)
		netInfo, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())
		_, ipNet, err := net.ParseCIDR(v4NodeSubnet)
		Expect(err).NotTo(HaveOccurred())
		mgtPortMAC = util.IPAddrToHWAddr(util.GetNodeManagementIfAddr(ipNet).IP).String()
		getCreationFakeCommands(fexec, mgtPort, mgtPortMAC, netName, nodeName, netInfo.MTU())
		getRPFilterLooseModeFakeCommands(fexec)
		nodeLister.On("Get", mock.AnythingOfType("string")).Return(node, nil)
		factoryMock.On("GetNodeForWindows", "worker1").Return(node, nil)
		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()
			ofm := getDummyOpenflowManager()
			udnGateway, err := NewUserDefinedNetworkGateway(netInfo, node, factoryMock.NodeCoreInformer(),
				&kubeMock, vrf, nil, &gateway{openflowManager: ofm}, nil, nil, nil)
			Expect(err).NotTo(HaveOccurred())
			localSubnets, err := udnGateway.getLocalSubnets()
			Expect(err).NotTo(HaveOccurred())
			udnGateway.mgmtPortController, err = managementport.NewUDNManagementPortController(udnGateway.nodeInformer, udnGateway.node.Name, localSubnets, udnGateway.NetInfo)
			Expect(err).NotTo(HaveOccurred())
			err = udnGateway.mgmtPortController.Create()
			Expect(err).NotTo(HaveOccurred())
			mpLink, err := util.LinkByName(util.GetNetworkScopedK8sMgmtHostIntfName(uint(udnGateway.GetNetworkID())))
			Expect(err).NotTo(HaveOccurred())
			Expect(mpLink).NotTo(BeNil())
			Expect(udnGateway.addUDNManagementPortIPs(mpLink)).Should(Succeed())
			exists, err := util.LinkAddrExist(mpLink, ovntest.MustParseIPNet("100.128.0.2/16"))
			Expect(err).NotTo(HaveOccurred())
			Expect(exists).To(BeTrue())
			exists, err = util.LinkAddrExist(mpLink, ovntest.MustParseIPNet("ae70::2/60"))
			Expect(err).NotTo(HaveOccurred())
			Expect(exists).To(BeTrue())
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
	})
	ovntest.OnSupportedPlatformsIt("should delete management port for a L2 user defined network", func() {
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
				Annotations: map[string]string{
					"k8s.ovn.org/network-ids":  fmt.Sprintf("{\"%s\": \"%s\"}", netName, netID),
					"k8s.ovn.org/node-subnets": fmt.Sprintf("{\"%s\":[\"%s\", \"%s\"]}", netName, v4NodeSubnet, v6NodeSubnet),
				},
			},
		}
		nad := ovntest.GenerateNAD(netName, "rednad", "greenamespace",
			types.Layer2Topology, "100.128.0.0/16,ae70::/60", types.NetworkRolePrimary)
		// must be defined so that the primary user defined network can match the ip families of the underlying cluster
		config.IPv4Mode = true
		config.IPv6Mode = true
		ovntest.AnnotateNADWithNetworkID(netID, nad)
		deleteStaleManagementPortFakeCommands(fexec, mgtPort)
		netInfo, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())
		getDeletionFakeOVSCommands(fexec, mgtPort)
		nodeLister.On("Get", mock.AnythingOfType("string")).Return(node, nil)
		factoryMock.On("GetNodeForWindows", "worker1").Return(node, nil)
		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()
			ofm := getDummyOpenflowManager()
			udnGateway, err := NewUserDefinedNetworkGateway(netInfo, node, factoryMock.NodeCoreInformer(),
				&kubeMock, vrf, nil, &gateway{openflowManager: ofm}, nil, nil, nil)
			Expect(err).NotTo(HaveOccurred())
			localSubnets, err := udnGateway.getLocalSubnets()
			Expect(err).NotTo(HaveOccurred())
			udnGateway.mgmtPortController, err = managementport.NewUDNManagementPortController(udnGateway.nodeInformer, udnGateway.node.Name, localSubnets, udnGateway.NetInfo)
			Expect(err).NotTo(HaveOccurred())
			err = udnGateway.mgmtPortController.Delete()
			Expect(err).NotTo(HaveOccurred())
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
	})
	ovntest.OnSupportedPlatformsIt("should create and delete correct openflows on breth0 for a L3 user defined network", func() {
		config.IPv4Mode = true
		config.IPv6Mode = true
		config.Gateway.Interface = "eth0"
		config.Gateway.NodeportEnable = true
		ifAddrs := ovntest.MustParseIPNets(v4NodeIP, v6NodeIP)
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
				Annotations: map[string]string{
					"k8s.ovn.org/network-ids":       fmt.Sprintf("{\"%s\": \"%s\"}", netName, netID),
					"k8s.ovn.org/node-subnets":      fmt.Sprintf("{\"default\":[\"%s\"],\"%s\":[\"%s\", \"%s\"]}", v4NodeSubnet, netName, v4NodeSubnet, v6NodeSubnet),
					"k8s.ovn.org/host-cidrs":        fmt.Sprintf("[\"%s\", \"%s\"]", v4NodeIP, v6NodeIP),
					"k8s.ovn.org/l3-gateway-config": "{\"default\": {}}",
				},
			},
			Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: strings.Split(v4NodeIP, "/")[0]},
				{Type: corev1.NodeInternalIP, Address: strings.Split(v6NodeIP, "/")[0]}},
			},
		}
		nad := ovntest.GenerateNAD(netName, "rednad", "greenamespace",
			types.Layer3Topology, "100.128.0.0/16/24,ae70::/60/64", types.NetworkRolePrimary)
		ovntest.AnnotateNADWithNetworkID(netID, nad)
		netInfo, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())

		setManagementPortFakeCommands(fexec, nodeName)
		setUpGatewayFakeOVSCommands(fexec)
		deleteStaleManagementPortFakeCommands(fexec, mgtPort)
		_, ipNet, err := net.ParseCIDR(v4NodeSubnet)
		Expect(err).NotTo(HaveOccurred())
		mgtPortMAC = util.IPAddrToHWAddr(util.GetNodeManagementIfAddr(ipNet).IP).String()
		getCreationFakeCommands(fexec, mgtPort, mgtPortMAC, netName, nodeName, netInfo.MTU())
		getRPFilterLooseModeFakeCommands(fexec)
		setUpUDNOpenflowManagerFakeOVSCommands(fexec)
		getDeletionFakeOVSCommands(fexec, mgtPort)
		nodeLister.On("Get", mock.AnythingOfType("string")).Return(node, nil)
		kubeFakeClient := fake.NewSimpleClientset(
			&corev1.NodeList{
				Items: []corev1.Node{*node},
			},
		)
		fakeClient := &util.OVNNodeClientset{
			KubeClient:               kubeFakeClient,
			NetworkAttchDefClient:    nadfake.NewSimpleClientset(),
			UplinkClient:             uplinkfake.NewSimpleClientset(),
			UserDefinedNetworkClient: udnfakeclient.NewSimpleClientset(),
		}

		stop := make(chan struct{})
		wf, err := factory.NewNodeWatchFactory(fakeClient, nodeName)
		Expect(err).NotTo(HaveOccurred())
		defer func() {
			close(stop)
			wf.Shutdown()
		}()
		err = wf.Start()
		Expect(err).NotTo(HaveOccurred())

		// Make Management port
		nodeSubnets := ovntest.MustParseIPNets(v4NodeSubnet, v6NodeSubnet)
		mp, err := managementport.NewManagementPortController(ovsClient, node, nodeSubnets, "", "", rm, netInfo)
		Expect(err).NotTo(HaveOccurred())

		nodeAnnotatorMock := &kubemocks.Annotator{}
		nodeAnnotatorMock.On("Delete", mock.Anything).Return(nil)
		nodeAnnotatorMock.On("Set", mock.Anything, map[string]*util.L3GatewayConfig{
			types.DefaultNetworkName: {
				ChassisID:   "cb9ec8fa-b409-4ef3-9f42-d9283c47aac6",
				BridgeID:    "breth0",
				InterfaceID: "breth0_worker1",
				MACAddress:  ovntest.MustParseMAC("00:00:00:55:66:99"),
				IPAddresses: ifAddrs,
				VLANID:      ptr.To(uint(0)),
			}}).Return(nil)
		nodeAnnotatorMock.On("Set", mock.Anything, mock.Anything).Return(nil)
		nodeAnnotatorMock.On("Run").Return(nil)
		kubeMock.On("SetAnnotationsOnNode", node.Name, map[string]interface{}{
			"k8s.ovn.org/node-masquerade-subnet": "{\"ipv4\":\"169.254.0.0/17\",\"ipv6\":\"fd69::/112\"}",
		}).Return(nil)
		kubeMock.On("SetAnnotationsOnNode", node.Name, map[string]interface{}{
			"k8s.ovn.org/host-cidrs":          "[\"192.168.1.10/24\",\"fc00:f853:ccd:e793::3/64\"]",
			"k8s.ovn.org/l3-gateway-config":   "{\"default\":{\"mode\":\"\"}}",
			"k8s.ovn.org/node-primary-ifaddr": "{\"ipv4\":\"192.168.1.10/24\",\"ipv6\":\"fc00:f853:ccd:e793::3/64\"}",
		}).Return(nil)

		wg.Add(1)
		go func() {
			defer GinkgoRecover()
			defer wg.Done()
			err := testNS.Do(func(ns.NetNS) error {
				rm.Run(stop, 10*time.Second)
				return nil
			})
			Expect(err).NotTo(HaveOccurred())
		}()
		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()
			gatewayNextHops, gatewayIntf, err := getGatewayNextHops(ovsClient)
			Expect(err).NotTo(HaveOccurred())

			// create dummy management interface
			err = netlink.LinkAdd(&netlink.Dummy{
				LinkAttrs: netlink.LinkAttrs{
					Name: types.K8sMgmtIntfName,
				},
			})
			Expect(err).NotTo(HaveOccurred())
			err = mp.Start(stop)
			Expect(err).NotTo(HaveOccurred())

			// make preparations for creating openflow manager in DNCC which can be used for SNCC
			localGw, err := newGateway(
				nodeName,
				nodeSubnets,
				gatewayNextHops,
				gatewayIntf,
				"",
				ifAddrs,
				nodeAnnotatorMock,
				mp,
				&kubeMock,
				wf,
				rm,
				nil,
				networkmanager.Default().Interface(),
				config.GatewayModeLocal,
				ovsClient,
			)
			Expect(err).NotTo(HaveOccurred())
			ovs, err := ovsops.GetOpenvSwitch(ovsClient)
			Expect(err).NotTo(HaveOccurred())
			Expect(ovs.ExternalIDs).To(HaveKeyWithValue("ovn-bridge-mappings", types.PhysicalNetworkName+":breth0"))
			stop := make(chan struct{})
			wg := &sync.WaitGroup{}
			err = localGw.initFunc()
			Expect(err).NotTo(HaveOccurred())
			Expect(localGw.Init(stop, wg)).To(Succeed())
			// we cannot start the shared gw directly because it will spawn a goroutine that may not be bound to the test netns
			// Start does two things, starts nodeIPManager which spawns a go routine and also starts openflow manager by spawning a go routine
			//sharedGw.Start()
			localGw.nodeIPManager.sync()
			// we cannot start openflow manager directly because it spawns a go routine
			// FIXME: extract openflow manager func from the spawning of a go routine so it can be called directly below.
			err = localGw.openflowManager.updateBridgeFlowCache(localGw.nodeIPManager.ListAddresses())
			Expect(err).NotTo(HaveOccurred())
			localGw.openflowManager.syncFlows()

			udnGateway, err := NewUserDefinedNetworkGateway(netInfo, node, wf.NodeCoreInformer(),
				&kubeMock, vrf, ipRulesManager, localGw, nil, nil, nil)
			Expect(err).NotTo(HaveOccurred())
			flowMap := udnGateway.gateway.openflowManager.defaultBridge.flowCache
			baseFlowCount := 52
			Expect(flowMap["DEFAULT"]).To(HaveLen(baseFlowCount))

			Expect(udnGateway.masqCTMark).To(Equal(udnGateway.masqCTMark))
			var udnFlows int
			for _, flows := range flowMap {
				for _, flow := range flows {
					mark := fmt.Sprintf("0x%x", udnGateway.masqCTMark)
					if strings.Contains(flow, mark) {
						// UDN Flow
						udnFlows++
					}
				}
			}
			Expect(udnFlows).To(Equal(0))
			Expect(udnGateway.openflowManager.defaultBridge.GetNetConfigLen()).To(Equal(1)) // only default network

			addOVSPatchPortInterface(ovsClient, "breth0", "patch-breth0_bluenet_worker1-to-br-int", 15)
			Expect(udnGateway.Start()).To(Succeed())

			flowMap = udnGateway.gateway.openflowManager.defaultBridge.flowCache
			udnDefaultFlows := 22
			Expect(flowMap["DEFAULT"]).To(HaveLen(baseFlowCount + udnDefaultFlows))
			Expect(udnGateway.openflowManager.defaultBridge.GetNetConfigLen()).To(Equal(2)) // default network + UDN network
			defaultUdnConfig := udnGateway.openflowManager.defaultBridge.GetNetworkConfig("default")
			bridgeUdnConfig := udnGateway.openflowManager.defaultBridge.GetNetworkConfig("bluenet")
			bridgeMAC := udnGateway.openflowManager.defaultBridge.GetMAC().String()
			ofPortHost := udnGateway.openflowManager.defaultBridge.GetOfPortHost()
			for _, flows := range flowMap {
				for _, flow := range flows {
					if strings.Contains(flow, fmt.Sprintf("0x%x", udnGateway.masqCTMark)) {
						// UDN Flow
						udnFlows++
					} else if strings.Contains(flow, fmt.Sprintf("in_port=%s", bridgeUdnConfig.OfPortPatch)) {
						udnFlows++
					}
				}
			}
			Expect(udnFlows).To(Equal(16))
			addOVSPatchPortInterface(ovsClient, "breth0", "patch-breth0_worker1-to-br-int", 5)
			openflowManagerCheckPorts(udnGateway.openflowManager)

			for _, svcCIDR := range config.Kubernetes.ServiceCIDRs {
				// Check flows for default network service CIDR.
				bridgeconfig.CheckDefaultSvcIsolationOVSFlows(flowMap["DEFAULT"], defaultUdnConfig, ofPortHost, bridgeMAC, svcCIDR)

				// Expect exactly one flow per UDN for table 2 for service isolation.
				bridgeconfig.CheckUDNSvcIsolationOVSFlows(flowMap["DEFAULT"], bridgeUdnConfig, "bluenet", svcCIDR, 1)
			}

			// The second call to checkPorts() will return no ofPort for the UDN - simulating a deletion that already was
			// processed by ovn-northd/ovn-controller.  We should not be panicking on that.
			removeOVSPatchPortInterface(ovsClient, "breth0", "patch-breth0_bluenet_worker1-to-br-int")
			openflowManagerCheckPorts(udnGateway.openflowManager)

			udnGateway.Stop()
			Expect(udnGateway.Cleanup()).To(Succeed())
			flowMap = udnGateway.gateway.openflowManager.defaultBridge.flowCache
			Expect(flowMap["DEFAULT"]).To(HaveLen(baseFlowCount))
			Expect(udnGateway.openflowManager.defaultBridge.GetNetConfigLen()).To(Equal(1)) // default network only
			udnFlows = 0
			for _, flows := range flowMap {
				for _, flow := range flows {
					if strings.Contains(flow, fmt.Sprintf("0x%x", udnGateway.masqCTMark)) {
						// UDN Flow
						udnFlows++
					}
				}
			}
			Expect(udnFlows).To(Equal(0))

			for _, svcCIDR := range config.Kubernetes.ServiceCIDRs {
				// Check flows for default network service CIDR.
				bridgeconfig.CheckDefaultSvcIsolationOVSFlows(flowMap["DEFAULT"], defaultUdnConfig, ofPortHost, bridgeMAC, svcCIDR)

				// Expect no more flows per UDN for table 2 for service isolation.
				bridgeconfig.CheckUDNSvcIsolationOVSFlows(flowMap["DEFAULT"], bridgeUdnConfig, "bluenet", svcCIDR, 0)
			}
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
	})
	ovntest.OnSupportedPlatformsIt("should handle gateway delete idempotently for a L3 user defined network", func() {
		config.IPv4Mode = true
		config.IPv6Mode = true
		config.Gateway.Interface = "eth0"
		config.Gateway.NodeportEnable = true
		ifAddrs := ovntest.MustParseIPNets(v4NodeIP, v6NodeIP)
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
				Annotations: map[string]string{
					"k8s.ovn.org/network-ids":       fmt.Sprintf("{\"%s\": \"%s\"}", netName, netID),
					"k8s.ovn.org/node-subnets":      fmt.Sprintf("{\"default\":[\"%s\"],\"%s\":[\"%s\", \"%s\"]}", v4NodeSubnet, netName, v4NodeSubnet, v6NodeSubnet),
					"k8s.ovn.org/host-cidrs":        fmt.Sprintf("[\"%s\", \"%s\"]", v4NodeIP, v6NodeIP),
					"k8s.ovn.org/l3-gateway-config": "{\"default\": {}}",
				},
			},
			Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: strings.Split(v4NodeIP, "/")[0]},
				{Type: corev1.NodeInternalIP, Address: strings.Split(v6NodeIP, "/")[0]}},
			},
		}
		nad := ovntest.GenerateNAD(netName, "rednad", "greenamespace",
			types.Layer3Topology, "100.128.0.0/16/24,ae70::/60/64", types.NetworkRolePrimary)
		ovntest.AnnotateNADWithNetworkID(netID, nad)
		netInfo, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())

		setManagementPortFakeCommands(fexec, nodeName)
		setUpGatewayFakeOVSCommands(fexec)
		deleteStaleManagementPortFakeCommands(fexec, mgtPort)
		_, ipNet, err := net.ParseCIDR(v4NodeSubnet)
		Expect(err).NotTo(HaveOccurred())
		mgtPortMAC = util.IPAddrToHWAddr(util.GetNodeManagementIfAddr(ipNet).IP).String()
		getDeletionFakeOVSCommands(fexec, mgtPort)
		nodeLister.On("Get", mock.AnythingOfType("string")).Return(node, nil)
		kubeFakeClient := fake.NewSimpleClientset(
			&corev1.NodeList{
				Items: []corev1.Node{*node},
			},
		)
		fakeClient := &util.OVNNodeClientset{
			KubeClient:               kubeFakeClient,
			NetworkAttchDefClient:    nadfake.NewSimpleClientset(),
			UplinkClient:             uplinkfake.NewSimpleClientset(),
			UserDefinedNetworkClient: udnfakeclient.NewSimpleClientset(),
		}

		stop := make(chan struct{})
		wf, err := factory.NewNodeWatchFactory(fakeClient, nodeName)
		Expect(err).NotTo(HaveOccurred())
		defer func() {
			close(stop)
			wf.Shutdown()
		}()
		err = wf.Start()
		Expect(err).NotTo(HaveOccurred())

		// Make Management port
		nodeSubnets := ovntest.MustParseIPNets(v4NodeSubnet, v6NodeSubnet)
		mp, err := managementport.NewManagementPortController(ovsClient, node, nodeSubnets, "", "", rm, netInfo)
		Expect(err).NotTo(HaveOccurred())

		nodeAnnotatorMock := &kubemocks.Annotator{}
		nodeAnnotatorMock.On("Delete", mock.Anything).Return(nil)
		nodeAnnotatorMock.On("Set", mock.Anything, map[string]*util.L3GatewayConfig{
			types.DefaultNetworkName: {
				ChassisID:   "cb9ec8fa-b409-4ef3-9f42-d9283c47aac6",
				BridgeID:    "breth0",
				InterfaceID: "breth0_worker1",
				MACAddress:  ovntest.MustParseMAC("00:00:00:55:66:99"),
				IPAddresses: ifAddrs,
				VLANID:      ptr.To(uint(0)),
			}}).Return(nil)
		nodeAnnotatorMock.On("Set", mock.Anything, mock.Anything).Return(nil)
		nodeAnnotatorMock.On("Run").Return(nil)
		kubeMock.On("SetAnnotationsOnNode", node.Name, map[string]interface{}{
			"k8s.ovn.org/node-masquerade-subnet": "{\"ipv4\":\"169.254.0.0/17\",\"ipv6\":\"fd69::/112\"}",
		}).Return(nil)
		kubeMock.On("SetAnnotationsOnNode", node.Name, map[string]interface{}{
			"k8s.ovn.org/host-cidrs":          "[\"192.168.1.10/24\",\"fc00:f853:ccd:e793::3/64\"]",
			"k8s.ovn.org/l3-gateway-config":   "{\"default\":{\"mode\":\"\"}}",
			"k8s.ovn.org/node-primary-ifaddr": "{\"ipv4\":\"192.168.1.10/24\",\"ipv6\":\"fc00:f853:ccd:e793::3/64\"}",
		}).Return(nil)

		wg.Add(1)
		go func() {
			defer GinkgoRecover()
			defer wg.Done()
			err := testNS.Do(func(ns.NetNS) error {
				rm.Run(stop, 10*time.Second)
				return nil
			})
			Expect(err).NotTo(HaveOccurred())
		}()
		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()
			gatewayNextHops, gatewayIntf, err := getGatewayNextHops(ovsClient)
			Expect(err).NotTo(HaveOccurred())

			// create dummy management interface
			err = netlink.LinkAdd(&netlink.Dummy{
				LinkAttrs: netlink.LinkAttrs{
					Name: types.K8sMgmtIntfName,
				},
			})
			Expect(err).NotTo(HaveOccurred())
			err = mp.Start(stop)
			Expect(err).NotTo(HaveOccurred())

			// make preparations for creating openflow manager in DNCC which can be used for SNCC
			localGw, err := newGateway(
				nodeName,
				nodeSubnets,
				gatewayNextHops,
				gatewayIntf,
				"",
				ifAddrs,
				nodeAnnotatorMock,
				mp,
				&kubeMock,
				wf,
				rm,
				nil,
				networkmanager.Default().Interface(),
				config.GatewayModeLocal,
				ovsClient,
			)
			Expect(err).NotTo(HaveOccurred())
			ovs, err := ovsops.GetOpenvSwitch(ovsClient)
			Expect(err).NotTo(HaveOccurred())
			Expect(ovs.ExternalIDs).To(HaveKeyWithValue("ovn-bridge-mappings", types.PhysicalNetworkName+":breth0"))
			stop := make(chan struct{})
			wg := &sync.WaitGroup{}
			err = localGw.initFunc()
			Expect(err).NotTo(HaveOccurred())
			Expect(localGw.Init(stop, wg)).To(Succeed())
			// we cannot start the shared gw directly because it will spawn a goroutine that may not be bound to the test netns
			// Start does two things, starts nodeIPManager which spawns a go routine and also starts openflow manager by spawning a go routine
			//sharedGw.Start()
			localGw.nodeIPManager.sync()
			// we cannot start openflow manager directly because it spawns a go routine
			// FIXME: extract openflow manager func from the spawning of a go routine so it can be called directly below.
			err = localGw.openflowManager.updateBridgeFlowCache(localGw.nodeIPManager.ListAddresses())
			Expect(err).NotTo(HaveOccurred())
			localGw.openflowManager.syncFlows()

			By("injecting error into ipRulesManager to ensure everything else still cleans up")
			udnGateway, err := NewUserDefinedNetworkGateway(netInfo, node, wf.NodeCoreInformer(),
				&kubeMock, vrf, &iprulemanager.FakeControllerWithError{}, localGw, nil, nil, nil)
			Expect(err).NotTo(HaveOccurred())
			localSubnets, err := udnGateway.getLocalSubnets()
			Expect(err).NotTo(HaveOccurred())
			udnGateway.mgmtPortController, err = managementport.NewUDNManagementPortController(udnGateway.nodeInformer, udnGateway.node.Name, localSubnets, udnGateway.NetInfo)
			Expect(err).NotTo(HaveOccurred())
			flowMap := udnGateway.gateway.openflowManager.defaultBridge.flowCache
			baseFlowCount := 52
			Expect(flowMap["DEFAULT"]).To(HaveLen(baseFlowCount))

			Expect(udnGateway.masqCTMark).To(Equal(udnGateway.masqCTMark))
			var udnFlows int
			for _, flows := range flowMap {
				for _, flow := range flows {
					mark := fmt.Sprintf("0x%x", udnGateway.masqCTMark)
					if strings.Contains(flow, mark) {
						// UDN Flow
						udnFlows++
					}
				}
			}
			Expect(udnFlows).To(Equal(0))
			Expect(udnGateway.openflowManager.defaultBridge.GetNetConfigLen()).To(Equal(1)) // only default network
			By("Deleting the gateway network with injected error")
			udnGateway.Stop()
			err = udnGateway.Cleanup()
			Expect(err).To(MatchError(ContainSubstring("fake delete metadata error")))
			By("Ensuring everything else was still cleaned up correctly")
			flowMap = udnGateway.gateway.openflowManager.defaultBridge.flowCache
			Expect(flowMap["DEFAULT"]).To(HaveLen(baseFlowCount))
			Expect(udnGateway.openflowManager.defaultBridge.GetNetConfigLen()).To(Equal(1)) // default network only
			udnFlows = 0
			for _, flows := range flowMap {
				for _, flow := range flows {
					if strings.Contains(flow, fmt.Sprintf("0x%x", udnGateway.masqCTMark)) {
						// UDN Flow
						udnFlows++
					}
				}
			}
			Expect(udnFlows).To(Equal(0))
			By("Ensure UDN management port was removed")
			_, err = netlink.LinkByName(mgtPort)
			Expect(err).To(HaveOccurred())
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
	})
	ovntest.OnSupportedPlatformsIt("should create and delete correct openflows on breth0 for a L2 user defined network", func() {
		config.IPv4Mode = true
		config.IPv6Mode = true
		config.Gateway.Interface = "eth0"
		config.Gateway.NodeportEnable = true
		ifAddrs := ovntest.MustParseIPNets(v4NodeIP, v6NodeIP)
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
				Annotations: map[string]string{
					"k8s.ovn.org/network-ids":  fmt.Sprintf("{\"%s\": \"%s\"}", netName, netID),
					"k8s.ovn.org/node-subnets": fmt.Sprintf("{\"default\":[\"%s\"],\"%s\":[\"%s\", \"%s\"]}", v4NodeSubnet, netName, v4NodeSubnet, v6NodeSubnet),
					"k8s.ovn.org/host-cidrs":   fmt.Sprintf("[\"%s\", \"%s\"]", v4NodeIP, v6NodeIP),
				},
			},
			Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: strings.Split(v4NodeIP, "/")[0]},
				{Type: corev1.NodeInternalIP, Address: strings.Split(v6NodeIP, "/")[0]}},
			},
		}
		nad := ovntest.GenerateNAD(netName, "rednad", "greenamespace",
			types.Layer2Topology, "100.128.0.0/16,ae70::/64", types.NetworkRolePrimary)
		ovntest.AnnotateNADWithNetworkID(netID, nad)
		netInfo, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())
		_, ipNet, err := net.ParseCIDR(v4NodeSubnet)
		Expect(err).NotTo(HaveOccurred())
		mgtPortMAC = util.IPAddrToHWAddr(util.GetNodeManagementIfAddr(ipNet).IP).String()

		setManagementPortFakeCommands(fexec, nodeName)
		setUpGatewayFakeOVSCommands(fexec)
		deleteStaleManagementPortFakeCommands(fexec, mgtPort)
		getCreationFakeCommands(fexec, mgtPort, mgtPortMAC, netName, nodeName, netInfo.MTU())
		getRPFilterLooseModeFakeCommands(fexec)
		setUpUDNOpenflowManagerFakeOVSCommands(fexec)
		getDeletionFakeOVSCommands(fexec, mgtPort)
		nodeLister.On("Get", mock.AnythingOfType("string")).Return(node, nil)
		kubeFakeClient := fake.NewSimpleClientset(
			&corev1.NodeList{
				Items: []corev1.Node{*node},
			},
		)
		fakeClient := &util.OVNNodeClientset{
			KubeClient:               kubeFakeClient,
			NetworkAttchDefClient:    nadfake.NewSimpleClientset(),
			UplinkClient:             uplinkfake.NewSimpleClientset(),
			UserDefinedNetworkClient: udnfakeclient.NewSimpleClientset(),
		}

		stop := make(chan struct{})
		wf, err := factory.NewNodeWatchFactory(fakeClient, nodeName)
		Expect(err).NotTo(HaveOccurred())
		wg := &sync.WaitGroup{}
		defer func() {
			close(stop)
			wf.Shutdown()
			wg.Wait()
		}()
		err = wf.Start()
		Expect(err).NotTo(HaveOccurred())

		// Make Management port
		nodeSubnets := ovntest.MustParseIPNets(v4NodeSubnet, v6NodeSubnet)
		mp, err := managementport.NewManagementPortController(ovsClient, node, nodeSubnets, "", "", rm, netInfo)
		Expect(err).NotTo(HaveOccurred())

		nodeAnnotatorMock := &kubemocks.Annotator{}
		nodeAnnotatorMock.On("Delete", mock.Anything).Return(nil)
		nodeAnnotatorMock.On("Set", mock.Anything, map[string]*util.L3GatewayConfig{
			types.DefaultNetworkName: {
				ChassisID:   "cb9ec8fa-b409-4ef3-9f42-d9283c47aac6",
				BridgeID:    "breth0",
				InterfaceID: "breth0_worker1",
				MACAddress:  ovntest.MustParseMAC("00:00:00:55:66:99"),
				IPAddresses: ifAddrs,
				VLANID:      ptr.To(uint(0)),
			}}).Return(nil)
		nodeAnnotatorMock.On("Set", mock.Anything, mock.Anything).Return(nil)
		nodeAnnotatorMock.On("Run").Return(nil)
		kubeMock.On("SetAnnotationsOnNode", node.Name, map[string]interface{}{
			"k8s.ovn.org/node-masquerade-subnet": "{\"ipv4\":\"169.254.0.0/17\",\"ipv6\":\"fd69::/112\"}",
		}).Return(nil)

		wg.Add(1)
		go func() {
			defer GinkgoRecover()
			defer wg.Done()
			err := testNS.Do(func(ns.NetNS) error {
				rm.Run(stop, 10*time.Second)
				return nil
			})
			Expect(err).NotTo(HaveOccurred())
		}()
		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()
			gatewayNextHops, gatewayIntf, err := getGatewayNextHops(ovsClient)
			Expect(err).NotTo(HaveOccurred())

			// create dummy management interface
			err = netlink.LinkAdd(&netlink.Dummy{
				LinkAttrs: netlink.LinkAttrs{
					Name: types.K8sMgmtIntfName,
				},
			})
			Expect(err).NotTo(HaveOccurred())
			err = mp.Start(stop)
			Expect(err).NotTo(HaveOccurred())

			// make preparations for creating openflow manager in DNCC which can be used for SNCC
			localGw, err := newGateway(
				nodeName,
				ovntest.MustParseIPNets(v4NodeSubnet, v6NodeSubnet),
				gatewayNextHops,
				gatewayIntf,
				"",
				ifAddrs,
				nodeAnnotatorMock,
				mp,
				&kubeMock,
				wf,
				rm,
				nil,
				networkmanager.Default().Interface(),
				config.GatewayModeLocal,
				ovsClient,
			)
			Expect(err).NotTo(HaveOccurred())
			ovs, err := ovsops.GetOpenvSwitch(ovsClient)
			Expect(err).NotTo(HaveOccurred())
			Expect(ovs.ExternalIDs).To(HaveKeyWithValue("ovn-bridge-mappings", types.PhysicalNetworkName+":breth0"))
			stop := make(chan struct{})
			wg := &sync.WaitGroup{}
			Expect(localGw.initFunc()).To(Succeed())
			Expect(localGw.Init(stop, wg)).To(Succeed())
			// we cannot start the shared gw directly because it will spawn a goroutine that may not be bound to the test netns
			// Start does two things, starts nodeIPManager which spawns a go routine and also starts openflow manager by spawning a go routine
			//sharedGw.Start()
			localGw.nodeIPManager.sync()
			// we cannot start openflow manager directly because it spawns a go routine
			// FIXME: extract openflow manager func from the spawning of a go routine so it can be called directly below.
			err = localGw.openflowManager.updateBridgeFlowCache(localGw.nodeIPManager.ListAddresses())
			Expect(err).NotTo(HaveOccurred())
			localGw.openflowManager.syncFlows()

			udnGateway, err := NewUserDefinedNetworkGateway(netInfo, node, wf.NodeCoreInformer(),
				&kubeMock, vrf, ipRulesManager, localGw, nil, nil, nil)
			Expect(err).NotTo(HaveOccurred())
			flowMap := udnGateway.gateway.openflowManager.defaultBridge.flowCache
			baseFlowCount := 52
			Expect(flowMap["DEFAULT"]).To(HaveLen(baseFlowCount))
			Expect(udnGateway.masqCTMark).To(Equal(udnGateway.masqCTMark))
			var udnFlows int
			for _, flows := range flowMap {
				for _, flow := range flows {
					mark := fmt.Sprintf("0x%x", udnGateway.masqCTMark)
					if strings.Contains(flow, mark) {
						// UDN Flow
						udnFlows++
					}
				}
			}
			Expect(udnFlows).To(Equal(0))
			Expect(udnGateway.openflowManager.defaultBridge.GetNetConfigLen()).To(Equal(1)) // only default network

			addOVSPatchPortInterface(ovsClient, "breth0", "patch-breth0_bluenet_worker1-to-br-int", 15)
			Expect(udnGateway.Start()).To(Succeed())

			flowMap = udnGateway.gateway.openflowManager.defaultBridge.flowCache
			udnDefaultFlows := 22
			Expect(flowMap["DEFAULT"]).To(HaveLen(baseFlowCount + udnDefaultFlows))
			Expect(udnGateway.openflowManager.defaultBridge.GetNetConfigLen()).To(Equal(2)) // default network + UDN network
			defaultUdnConfig := udnGateway.openflowManager.defaultBridge.GetNetworkConfig("default")
			bridgeUdnConfig := udnGateway.openflowManager.defaultBridge.GetNetworkConfig("bluenet")
			bridgeMAC := udnGateway.openflowManager.defaultBridge.GetMAC().String()
			ofPortHost := udnGateway.openflowManager.defaultBridge.GetOfPortHost()
			for _, flows := range flowMap {
				for _, flow := range flows {
					if strings.Contains(flow, fmt.Sprintf("0x%x", udnGateway.masqCTMark)) {
						// UDN Flow
						udnFlows++
					} else if strings.Contains(flow, fmt.Sprintf("in_port=%s", bridgeUdnConfig.OfPortPatch)) {
						udnFlows++
					}
				}
			}
			Expect(udnFlows).To(Equal(16))
			addOVSPatchPortInterface(ovsClient, "breth0", "patch-breth0_worker1-to-br-int", 5)
			openflowManagerCheckPorts(udnGateway.openflowManager)

			for _, svcCIDR := range config.Kubernetes.ServiceCIDRs {
				// Check flows for default network service CIDR.
				bridgeconfig.CheckDefaultSvcIsolationOVSFlows(flowMap["DEFAULT"], defaultUdnConfig, ofPortHost, bridgeMAC, svcCIDR)

				// Expect exactly one flow per UDN for tables 0 and 2 for service isolation.
				bridgeconfig.CheckUDNSvcIsolationOVSFlows(flowMap["DEFAULT"], bridgeUdnConfig, "bluenet", svcCIDR, 1)
			}

			// The second call to checkPorts() will return no ofPort for the UDN - simulating a deletion that already was
			// processed by ovn-northd/ovn-controller.  We should not be panicking on that.
			removeOVSPatchPortInterface(ovsClient, "breth0", "patch-breth0_bluenet_worker1-to-br-int")
			openflowManagerCheckPorts(udnGateway.openflowManager)

			udnGateway.Stop()
			Expect(udnGateway.Cleanup()).To(Succeed())
			flowMap = udnGateway.gateway.openflowManager.defaultBridge.flowCache
			Expect(flowMap["DEFAULT"]).To(HaveLen(baseFlowCount))
			Expect(udnGateway.openflowManager.defaultBridge.GetNetConfigLen()).To(Equal(1)) // default network only
			udnFlows = 0
			for _, flows := range flowMap {
				for _, flow := range flows {
					if strings.Contains(flow, fmt.Sprintf("0x%x", udnGateway.masqCTMark)) {
						// UDN Flow
						udnFlows++
					}
				}
			}
			Expect(udnFlows).To(Equal(0))

			for _, svcCIDR := range config.Kubernetes.ServiceCIDRs {
				// Check flows for default network service CIDR.
				bridgeconfig.CheckDefaultSvcIsolationOVSFlows(flowMap["DEFAULT"], defaultUdnConfig, ofPortHost, bridgeMAC, svcCIDR)

				// Expect no more flows per UDN for tables 0 and 2 for service isolation.
				bridgeconfig.CheckUDNSvcIsolationOVSFlows(flowMap["DEFAULT"], bridgeUdnConfig, "bluenet", svcCIDR, 0)
			}
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
	})
	// TODO: There is opportunity to fold some of these tests into describetables to cut down code duplication and test plumbing
	ovntest.OnSupportedPlatformsIt("should create and delete correct openflows on breth0 for an advertised L3 user defined network", func() {
		config.IPv4Mode = true
		config.IPv6Mode = true
		config.Gateway.Interface = "eth0"
		config.Gateway.NodeportEnable = true
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		config.OVNKubernetesFeature.EnableRouteAdvertisements = true
		Expect(configureAdvertisedUDNIsolationNFTables()).To(Succeed())
		ifAddrs := ovntest.MustParseIPNets(v4NodeIP, v6NodeIP)
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
				Annotations: map[string]string{
					"k8s.ovn.org/network-ids":       fmt.Sprintf("{\"%s\": \"%s\"}", netName, netID),
					"k8s.ovn.org/node-subnets":      fmt.Sprintf("{\"default\":[\"%s\"],\"%s\":[\"%s\", \"%s\"]}", v4NodeSubnet, netName, v4NodeSubnet, v6NodeSubnet),
					"k8s.ovn.org/host-cidrs":        fmt.Sprintf("[\"%s\", \"%s\"]", v4NodeIP, v6NodeIP),
					"k8s.ovn.org/l3-gateway-config": "{\"default\": {}}",
				},
			},
			Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: strings.Split(v4NodeIP, "/")[0]},
				{Type: corev1.NodeInternalIP, Address: strings.Split(v6NodeIP, "/")[0]}},
			},
		}
		nad := ovntest.GenerateNAD(netName, "rednad", "greenamespace",
			types.Layer3Topology, "100.128.0.0/16/24,ae70::/60/64", types.NetworkRolePrimary)
		ovntest.AnnotateNADWithNetworkID(netID, nad)
		netInfo, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())
		mutableNetInfo := util.NewMutableNetInfo(netInfo)
		mutableNetInfo.SetPodNetworkAdvertisedVRFs(map[string][]string{node.Name: {netName}})
		setManagementPortFakeCommands(fexec, nodeName)
		setUpGatewayFakeOVSCommands(fexec)
		deleteStaleManagementPortFakeCommands(fexec, mgtPort)
		_, ipNet, err := net.ParseCIDR(v4NodeSubnet)
		Expect(err).NotTo(HaveOccurred())
		mgtPortMAC = util.IPAddrToHWAddr(util.GetNodeManagementIfAddr(ipNet).IP).String()
		getCreationFakeCommands(fexec, mgtPort, mgtPortMAC, netName, nodeName, mutableNetInfo.MTU())
		getRPFilterLooseModeFakeCommands(fexec)
		setUpUDNOpenflowManagerFakeOVSCommands(fexec)
		getDeletionFakeOVSCommands(fexec, mgtPort)
		nodeLister.On("Get", mock.AnythingOfType("string")).Return(node, nil)
		kubeFakeClient := fake.NewSimpleClientset(
			&corev1.NodeList{
				Items: []corev1.Node{*node},
			},
		)
		fakeClient := &util.OVNNodeClientset{
			KubeClient:                kubeFakeClient,
			NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
			UplinkClient:              uplinkfake.NewSimpleClientset(),
			UserDefinedNetworkClient:  udnfakeclient.NewSimpleClientset(),
			RouteAdvertisementsClient: rafakeclient.NewSimpleClientset(),
		}

		stop := make(chan struct{})
		wf, err := factory.NewNodeWatchFactory(fakeClient, nodeName)
		Expect(err).NotTo(HaveOccurred())
		defer func() {
			close(stop)
			wf.Shutdown()
		}()
		err = wf.Start()
		Expect(err).NotTo(HaveOccurred())

		// Make Management port
		nodeSubnets := ovntest.MustParseIPNets(v4NodeSubnet, v6NodeSubnet)
		mp, err := managementport.NewManagementPortController(ovsClient, node, nodeSubnets, "", "", rm, mutableNetInfo)
		Expect(err).NotTo(HaveOccurred())

		nodeAnnotatorMock := &kubemocks.Annotator{}
		nodeAnnotatorMock.On("Delete", mock.Anything).Return(nil)
		nodeAnnotatorMock.On("Set", mock.Anything, map[string]*util.L3GatewayConfig{
			types.DefaultNetworkName: {
				ChassisID:   "cb9ec8fa-b409-4ef3-9f42-d9283c47aac6",
				BridgeID:    "breth0",
				InterfaceID: "breth0_worker1",
				MACAddress:  ovntest.MustParseMAC("00:00:00:55:66:99"),
				IPAddresses: ifAddrs,
				VLANID:      ptr.To(uint(0)),
			}}).Return(nil)
		nodeAnnotatorMock.On("Set", mock.Anything, mock.Anything).Return(nil)
		nodeAnnotatorMock.On("Run").Return(nil)
		kubeMock.On("SetAnnotationsOnNode", node.Name, map[string]interface{}{
			"k8s.ovn.org/node-masquerade-subnet": "{\"ipv4\":\"169.254.0.0/17\",\"ipv6\":\"fd69::/112\"}",
		}).Return(nil)
		kubeMock.On("SetAnnotationsOnNode", node.Name, map[string]interface{}{
			"k8s.ovn.org/host-cidrs":          "[\"192.168.1.10/24\",\"fc00:f853:ccd:e793::3/64\"]",
			"k8s.ovn.org/l3-gateway-config":   "{\"default\":{\"mode\":\"\"}}",
			"k8s.ovn.org/node-primary-ifaddr": "{\"ipv4\":\"192.168.1.10/24\",\"ipv6\":\"fc00:f853:ccd:e793::3/64\"}",
		}).Return(nil)

		wg.Add(1)
		go func() {
			defer GinkgoRecover()
			defer wg.Done()
			err := testNS.Do(func(ns.NetNS) error {
				rm.Run(stop, 10*time.Second)
				return nil
			})
			Expect(err).NotTo(HaveOccurred())
		}()
		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()
			gatewayNextHops, gatewayIntf, err := getGatewayNextHops(ovsClient)
			Expect(err).NotTo(HaveOccurred())

			// create dummy management interface
			err = netlink.LinkAdd(&netlink.Dummy{
				LinkAttrs: netlink.LinkAttrs{
					Name: types.K8sMgmtIntfName,
				},
			})
			Expect(err).NotTo(HaveOccurred())
			err = mp.Start(stop)
			Expect(err).NotTo(HaveOccurred())

			// make preparations for creating openflow manager in DNCC which can be used for SNCC
			localGw, err := newGateway(
				nodeName,
				nodeSubnets,
				gatewayNextHops,
				gatewayIntf,
				"",
				ifAddrs,
				nodeAnnotatorMock,
				mp,
				&kubeMock,
				wf,
				rm,
				nil,
				networkmanager.Default().Interface(),
				config.GatewayModeLocal,
				ovsClient,
			)
			Expect(err).NotTo(HaveOccurred())
			ovs, err := ovsops.GetOpenvSwitch(ovsClient)
			Expect(err).NotTo(HaveOccurred())
			Expect(ovs.ExternalIDs).To(HaveKeyWithValue("ovn-bridge-mappings", types.PhysicalNetworkName+":breth0"))
			stop := make(chan struct{})
			wg := &sync.WaitGroup{}
			err = localGw.initFunc()
			Expect(err).NotTo(HaveOccurred())
			Expect(localGw.Init(stop, wg)).To(Succeed())
			// we cannot start the shared gw directly because it will spawn a goroutine that may not be bound to the test netns
			// Start does two things, starts nodeIPManager which spawns a go routine and also starts openflow manager by spawning a go routine
			//sharedGw.Start()
			localGw.nodeIPManager.sync()
			// we cannot start openflow manager directly because it spawns a go routine
			// FIXME: extract openflow manager func from the spawning of a go routine so it can be called directly below.
			err = localGw.openflowManager.updateBridgeFlowCache(localGw.nodeIPManager.ListAddresses())
			Expect(err).NotTo(HaveOccurred())
			localGw.openflowManager.syncFlows()

			udnGateway, err := NewUserDefinedNetworkGateway(mutableNetInfo, node, wf.NodeCoreInformer(),
				&kubeMock, vrf, ipRulesManager, localGw, nil, nil, nil)
			Expect(err).NotTo(HaveOccurred())
			flowMap := udnGateway.gateway.openflowManager.defaultBridge.flowCache
			baseFlowCount := 52
			Expect(flowMap["DEFAULT"]).To(HaveLen(baseFlowCount))

			Expect(udnGateway.masqCTMark).To(Equal(udnGateway.masqCTMark))
			var udnFlows int
			for _, flows := range flowMap {
				for _, flow := range flows {
					mark := fmt.Sprintf("0x%x", udnGateway.masqCTMark)
					if strings.Contains(flow, mark) {
						// UDN Flow
						udnFlows++
					}
				}
			}
			Expect(udnFlows).To(Equal(0))
			Expect(udnGateway.openflowManager.defaultBridge.GetNetConfigLen()).To(Equal(1)) // only default network

			addOVSPatchPortInterface(ovsClient, "breth0", "patch-breth0_bluenet_worker1-to-br-int", 15)
			Expect(udnGateway.Start()).To(Succeed())

			flowMap = udnGateway.gateway.openflowManager.defaultBridge.flowCache
			udnDefaultFlows := 22
			advertisedFlows := 3
			packetMarkFlows := 5
			Expect(flowMap["DEFAULT"]).To(HaveLen(baseFlowCount + udnDefaultFlows + advertisedFlows + packetMarkFlows))
			Expect(udnGateway.openflowManager.defaultBridge.GetNetConfigLen()).To(Equal(2)) // default network + UDN network
			defaultUdnConfig := udnGateway.openflowManager.defaultBridge.GetNetworkConfig("default")
			bridgeUdnConfig := udnGateway.openflowManager.defaultBridge.GetNetworkConfig("bluenet")
			bridgeMAC := udnGateway.openflowManager.defaultBridge.GetMAC().String()
			ofPortHost := udnGateway.openflowManager.defaultBridge.GetOfPortHost()
			for _, flows := range flowMap {
				for _, flow := range flows {
					if strings.Contains(flow, fmt.Sprintf("0x%x", udnGateway.masqCTMark)) {
						// UDN Flow
						udnFlows++
					} else if strings.Contains(flow, fmt.Sprintf("in_port=%s", bridgeUdnConfig.OfPortPatch)) {
						udnFlows++
					}
				}
			}
			Expect(udnFlows).To(Equal(18))
			addOVSPatchPortInterface(ovsClient, "breth0", "patch-breth0_worker1-to-br-int", 5)
			openflowManagerCheckPorts(udnGateway.openflowManager)

			for _, svcCIDR := range config.Kubernetes.ServiceCIDRs {
				// Check flows for default network service CIDR.
				bridgeconfig.CheckDefaultSvcIsolationOVSFlows(flowMap["DEFAULT"], defaultUdnConfig, ofPortHost, bridgeMAC, svcCIDR)

				// Expect exactly two flow per advertised UDN for table 2 and table 0 for service isolation.
				// but one of the flows used by advertised UDNs is already tracked and used by default UDNs hence not
				// counted here but in the check above for default svc isolation flows.
				bridgeconfig.CheckAdvertisedUDNSvcIsolationOVSFlows(flowMap["DEFAULT"], bridgeUdnConfig, "bluenet", svcCIDR, 2)
			}

			// The second call to checkPorts() will return no ofPort for the UDN - simulating a deletion that already was
			// processed by ovn-northd/ovn-controller.  We should not be panicking on that.
			removeOVSPatchPortInterface(ovsClient, "breth0", "patch-breth0_bluenet_worker1-to-br-int")
			openflowManagerCheckPorts(udnGateway.openflowManager)

			udnGateway.Stop()
			Expect(udnGateway.Cleanup()).To(Succeed())
			flowMap = udnGateway.gateway.openflowManager.defaultBridge.flowCache
			Expect(flowMap["DEFAULT"]).To(HaveLen(baseFlowCount))
			Expect(udnGateway.openflowManager.defaultBridge.GetNetConfigLen()).To(Equal(1)) // default network only
			udnFlows = 0
			for _, flows := range flowMap {
				for _, flow := range flows {
					if strings.Contains(flow, fmt.Sprintf("0x%x", udnGateway.masqCTMark)) {
						// UDN Flow
						udnFlows++
					}
				}
			}
			Expect(udnFlows).To(Equal(0))

			for _, svcCIDR := range config.Kubernetes.ServiceCIDRs {
				// Check flows for default network service CIDR.
				bridgeconfig.CheckDefaultSvcIsolationOVSFlows(flowMap["DEFAULT"], defaultUdnConfig, ofPortHost, bridgeMAC, svcCIDR)

				// Expect no more flows per UDN for table 2 and table0 for service isolation.
				bridgeconfig.CheckAdvertisedUDNSvcIsolationOVSFlows(flowMap["DEFAULT"], bridgeUdnConfig, "bluenet", svcCIDR, 0)
			}
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
	})
	ovntest.OnSupportedPlatformsIt("should compute correct masquerade reply traffic routes for a user defined network", func() {
		config.Gateway.Interface = "eth0"
		config.IPv4Mode = true
		config.IPv6Mode = true
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
				Annotations: map[string]string{
					"k8s.ovn.org/node-subnets": fmt.Sprintf("{\"%s\":[\"%s\", \"%s\"]}", netName, v4NodeSubnet, v6NodeSubnet),
				},
			},
		}
		nad := ovntest.GenerateNAD(netName, "rednad", "greenamespace",
			types.Layer3Topology, "100.128.0.0/16/24,ae70::/60/64", types.NetworkRolePrimary)
		ovntest.AnnotateNADWithNetworkID(netID, nad)
		netInfo, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())
		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()
			ofm := getDummyOpenflowManager()
			udnGateway, err := NewUserDefinedNetworkGateway(netInfo, node, emptyNodeInformer(), nil, vrf, nil, &gateway{openflowManager: ofm}, nil, nil, nil)
			Expect(err).NotTo(HaveOccurred())
			mplink, err := netlink.LinkByName(mgtPort)
			Expect(err).NotTo(HaveOccurred())
			bridgelink, err := netlink.LinkByName("breth0")
			Expect(err).NotTo(HaveOccurred())
			vrfTableId := util.CalculateRouteTableID(mplink.Attrs().Index)
			udnGateway.vrfTableId = vrfTableId

			routes, err := udnGateway.computeRoutesForUDN(mplink)
			Expect(err).NotTo(HaveOccurred())
			Expect(routes).To(HaveLen(10))

			Expect(*routes[0].Dst).To(Equal(*ovntest.MustParseIPNet("172.16.1.0/24"))) // default service subnet
			Expect(routes[0].LinkIndex).To(Equal(bridgelink.Attrs().Index))
			Expect(routes[0].Gw).To(Equal(config.Gateway.MasqueradeIPs.V4DummyNextHopMasqueradeIP))

			Expect(*routes[1].Dst).To(Equal(*ovntest.MustParseIPNet("fd02::/112"))) // default service subnet
			Expect(routes[1].LinkIndex).To(Equal(bridgelink.Attrs().Index))
			Expect(routes[1].Gw).To(Equal(config.Gateway.MasqueradeIPs.V6DummyNextHopMasqueradeIP))

			cidr, err := util.GetIPNetFullMask("169.254.0.16")
			Expect(err).NotTo(HaveOccurred())
			Expect(*routes[2].Dst).To(Equal(*cidr))
			Expect(routes[2].LinkIndex).To(Equal(mplink.Attrs().Index))
			cidr, err = util.GetIPNetFullMask("fd69::10")
			Expect(err).NotTo(HaveOccurred())
			Expect(*routes[3].Dst).To(Equal(*cidr))
			Expect(routes[3].LinkIndex).To(Equal(mplink.Attrs().Index))

			// IPv4 ETP=Local service masquerade IP route
			Expect(*routes[4].Dst).To(Equal(*ovntest.MustParseIPNet("169.254.169.3/32"))) // ETP=Local svc masq IP
			Expect(routes[4].LinkIndex).To(Equal(mplink.Attrs().Index))
			Expect(routes[4].Gw.Equal(ovntest.MustParseIP("100.128.0.1"))).To(BeTrue())

			// IPv4 cluster subnet route
			Expect(*routes[5].Dst).To(Equal(*ovntest.MustParseIPNet("100.128.0.0/16"))) // cluster subnet route
			Expect(routes[5].LinkIndex).To(Equal(mplink.Attrs().Index))
			Expect(routes[5].Gw.Equal(ovntest.MustParseIP("100.128.0.1"))).To(BeTrue())

			// IPv6 ETP=Local service masquerade IP route
			Expect(*routes[6].Dst).To(Equal(*ovntest.MustParseIPNet("fd69::3/128"))) // ETP=Local svc masq IP
			Expect(routes[6].LinkIndex).To(Equal(mplink.Attrs().Index))
			Expect(routes[6].Gw.Equal(ovntest.MustParseIP("ae70::1"))).To(BeTrue())

			// IPv6 cluster subnet route
			Expect(*routes[7].Dst).To(Equal(*ovntest.MustParseIPNet("ae70::/60"))) // cluster subnet route
			Expect(routes[7].LinkIndex).To(Equal(mplink.Attrs().Index))
			Expect(routes[7].Gw.Equal(ovntest.MustParseIP("ae70::1"))).To(BeTrue())
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
	})
	ovntest.OnSupportedPlatformsIt("should compute correct routes for a user defined network", func() {
		config.Gateway.Interface = "eth0"
		config.IPv4Mode = true
		config.IPv6Mode = true
		config.Kubernetes.ServiceCIDRs = ovntest.MustParseIPNets("10.96.0.0/16", "fd00:10:96::/112")
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
				Annotations: map[string]string{
					"k8s.ovn.org/node-subnets": fmt.Sprintf("{\"%s\":[\"%s\", \"%s\"]}", netName, v4NodeSubnet, v6NodeSubnet),
				},
			},
		}
		nad := ovntest.GenerateNAD(netName, "rednad", "greenamespace",
			types.Layer3Topology, "100.128.0.0/16/24,ae70::/60/64", types.NetworkRolePrimary)
		ovntest.AnnotateNADWithNetworkID(netID, nad)
		netInfo, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())
		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()
			ofm := getDummyOpenflowManager()
			udnGateway, err := NewUserDefinedNetworkGateway(netInfo, node, emptyNodeInformer(), nil, vrf, nil, &gateway{openflowManager: ofm}, nil, nil, nil)
			Expect(err).NotTo(HaveOccurred())
			link, err := netlink.LinkByName("breth0")
			Expect(err).NotTo(HaveOccurred())

			mplink, err := netlink.LinkByName(mgtPort)
			Expect(err).NotTo(HaveOccurred())
			vrfTableId := util.CalculateRouteTableID(mplink.Attrs().Index)
			udnGateway.vrfTableId = vrfTableId

			routes, err := udnGateway.computeRoutesForUDN(mplink)
			Expect(err).NotTo(HaveOccurred())
			Expect(routes).To(HaveLen(10))
			Expect(err).NotTo(HaveOccurred())
			// 1st and 2nd routes are the service routes from user-provided config value
			Expect(*routes[0].Dst).To(Equal(*config.Kubernetes.ServiceCIDRs[0]))
			Expect(routes[0].LinkIndex).To(Equal(link.Attrs().Index))
			Expect(*routes[1].Dst).To(Equal(*config.Kubernetes.ServiceCIDRs[1]))
			Expect(routes[1].LinkIndex).To(Equal(link.Attrs().Index))
			cidr, err := util.GetIPNetFullMask("169.254.0.16")
			Expect(err).NotTo(HaveOccurred())
			Expect(*routes[2].Dst).To(Equal(*cidr))
			Expect(routes[2].LinkIndex).To(Equal(mplink.Attrs().Index))
			cidr, err = util.GetIPNetFullMask("fd69::10")
			Expect(err).NotTo(HaveOccurred())
			Expect(*routes[3].Dst).To(Equal(*cidr))
			Expect(routes[3].LinkIndex).To(Equal(mplink.Attrs().Index))

			// IPv4 ETP=Local service masquerade IP route
			Expect(*routes[4].Dst).To(Equal(*ovntest.MustParseIPNet("169.254.169.3/32"))) // ETP=Local svc masq IP
			Expect(routes[4].LinkIndex).To(Equal(mplink.Attrs().Index))
			Expect(routes[4].Gw.Equal(ovntest.MustParseIP("100.128.0.1"))).To(BeTrue())

			// IPv4 cluster subnet route
			Expect(*routes[5].Dst).To(Equal(*ovntest.MustParseIPNet("100.128.0.0/16"))) // cluster subnet route
			Expect(routes[5].LinkIndex).To(Equal(mplink.Attrs().Index))
			Expect(routes[5].Gw.Equal(ovntest.MustParseIP("100.128.0.1"))).To(BeTrue())

			// IPv6 ETP=Local service masquerade IP route
			Expect(*routes[6].Dst).To(Equal(*ovntest.MustParseIPNet("fd69::3/128"))) // ETP=Local svc masq IP
			Expect(routes[6].LinkIndex).To(Equal(mplink.Attrs().Index))
			Expect(routes[6].Gw.Equal(ovntest.MustParseIP("ae70::1"))).To(BeTrue())

			// IPv6 cluster subnet route
			Expect(*routes[7].Dst).To(Equal(*ovntest.MustParseIPNet("ae70::/60"))) // cluster subnet route
			Expect(routes[7].LinkIndex).To(Equal(mplink.Attrs().Index))
			Expect(routes[7].Gw.Equal(ovntest.MustParseIP("ae70::1"))).To(BeTrue())

			// IPv4 default unreachable route
			Expect(*routes[8].Dst).To(Equal(*ovntest.MustParseIPNet("0.0.0.0/0"))) // cluster subnet route
			Expect(routes[8].Priority).To(Equal(4278198272))
			Expect(routes[8].Type).To(Equal(unix.RTN_UNREACHABLE))

			// IPv6 default unreachable route
			Expect(*routes[9].Dst).To(Equal(*ovntest.MustParseIPNet("::/0"))) // cluster subnet route
			Expect(routes[9].Priority).To(Equal(4278198272))
			Expect(routes[9].Type).To(Equal(unix.RTN_UNREACHABLE))
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
	})

	ovntest.OnSupportedPlatformsIt("should compute routes for all cluster subnets of a multi-subnet user defined network", func() {
		config.Gateway.Interface = "eth0"
		config.IPv4Mode = true
		config.IPv6Mode = true
		config.Kubernetes.ServiceCIDRs = ovntest.MustParseIPNets("10.96.0.0/16", "fd00:10:96::/112")
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
				Annotations: map[string]string{
					"k8s.ovn.org/node-subnets": fmt.Sprintf("{\"%s\":[\"%s\", \"%s\"]}", netName, v4NodeSubnet, v6NodeSubnet),
				},
			},
		}
		// A multi-subnet Layer3 UDN with two IPv4 and two IPv6 cluster subnets.
		// The node only owns a host subnet out of the first subnet of each family.
		nad := ovntest.GenerateNAD(netName, "rednad", "greenamespace",
			types.Layer3Topology, "100.128.0.0/16/24,100.129.0.0/16/24,ae70::/60/64,ae71::/60/64", types.NetworkRolePrimary)
		ovntest.AnnotateNADWithNetworkID(netID, nad)
		netInfo, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())
		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()
			ofm := getDummyOpenflowManager()
			udnGateway, err := NewUserDefinedNetworkGateway(netInfo, node, emptyNodeInformer(), nil, vrf, nil,
				&gateway{openflowManager: ofm}, nil, nil, nil)
			Expect(err).NotTo(HaveOccurred())
			mplink, err := netlink.LinkByName(mgtPort)
			Expect(err).NotTo(HaveOccurred())
			vrfTableId := util.CalculateRouteTableID(mplink.Attrs().Index)
			udnGateway.vrfTableId = vrfTableId

			routes, err := udnGateway.computeRoutesForUDN(mplink)
			Expect(err).NotTo(HaveOccurred())
			// Compared to a single-subnet UDN (10 routes) there is one extra
			// cluster subnet route per IP family, so 12 routes total.
			Expect(routes).To(HaveLen(12))

			// IPv4 ETP=Local service masquerade IP route
			Expect(*routes[4].Dst).To(Equal(*ovntest.MustParseIPNet("169.254.169.3/32")))
			Expect(routes[4].LinkIndex).To(Equal(mplink.Attrs().Index))
			Expect(routes[4].Gw.Equal(ovntest.MustParseIP("100.128.0.1"))).To(BeTrue())

			// Both IPv4 cluster subnets are routed via the node's IPv4 gateway IP.
			Expect(*routes[5].Dst).To(Equal(*ovntest.MustParseIPNet("100.128.0.0/16")))
			Expect(routes[5].LinkIndex).To(Equal(mplink.Attrs().Index))
			Expect(routes[5].Gw.Equal(ovntest.MustParseIP("100.128.0.1"))).To(BeTrue())
			Expect(*routes[6].Dst).To(Equal(*ovntest.MustParseIPNet("100.129.0.0/16")))
			Expect(routes[6].LinkIndex).To(Equal(mplink.Attrs().Index))
			Expect(routes[6].Gw.Equal(ovntest.MustParseIP("100.128.0.1"))).To(BeTrue())

			// IPv6 ETP=Local service masquerade IP route
			Expect(*routes[7].Dst).To(Equal(*ovntest.MustParseIPNet("fd69::3/128")))
			Expect(routes[7].LinkIndex).To(Equal(mplink.Attrs().Index))
			Expect(routes[7].Gw.Equal(ovntest.MustParseIP("ae70::1"))).To(BeTrue())

			// Both IPv6 cluster subnets are routed via the node's IPv6 gateway IP.
			Expect(*routes[8].Dst).To(Equal(*ovntest.MustParseIPNet("ae70::/60")))
			Expect(routes[8].LinkIndex).To(Equal(mplink.Attrs().Index))
			Expect(routes[8].Gw.Equal(ovntest.MustParseIP("ae70::1"))).To(BeTrue())
			Expect(*routes[9].Dst).To(Equal(*ovntest.MustParseIPNet("ae71::/60")))
			Expect(routes[9].LinkIndex).To(Equal(mplink.Attrs().Index))
			Expect(routes[9].Gw.Equal(ovntest.MustParseIP("ae70::1"))).To(BeTrue())

			// IPv4 and IPv6 default unreachable routes.
			Expect(*routes[10].Dst).To(Equal(*ovntest.MustParseIPNet("0.0.0.0/0")))
			Expect(routes[10].Type).To(Equal(unix.RTN_UNREACHABLE))
			Expect(*routes[11].Dst).To(Equal(*ovntest.MustParseIPNet("::/0")))
			Expect(routes[11].Type).To(Equal(unix.RTN_UNREACHABLE))
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
	})

	ovntest.OnSupportedPlatformsIt("should create a route import VRF in DPU mode", func() {
		config.OvnKubeNode.Mode = types.NodeModeDPU
		config.Gateway.Mode = config.GatewayModeShared
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
			},
		}
		nad := ovntest.GenerateNADWithConfig("rednad", "greenamespace", fmt.Sprintf(
			`
{
        "cniVersion": "1.1.0",
        "name": %q,
        "type": "ovn-k8s-cni-overlay",
        "topology": %q,
        "subnets": "100.128.0.0/16/24",
        "mtu": 1300,
        "netAttachDefName": "greenamespace/rednad",
        "role": %q,
        "uplink": "uplink1"
}
`,
			netName,
			types.Layer3Topology,
			types.NetworkRolePrimary,
		))
		ovntest.AnnotateNADWithNetworkID(netID, nad)
		netInfo, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())

		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()
			ofm := getDummyOpenflowManager()
			uplinkStateInformer, statusController := newGatewayUplinkDependencies(node.Name)
			DeferCleanup(statusController.Stop)
			udnGateway, err := NewUserDefinedNetworkGateway(netInfo, node, emptyNodeInformer(), nil, vrf, nil,
				&gateway{openflowManager: ofm}, nil, uplinkStateInformer, statusController)
			Expect(err).NotTo(HaveOccurred())
			Expect(udnGateway.ensureDPUVRF()).To(Succeed())

			vrfLink, err := util.GetNetLinkOps().LinkByName(util.GetNetworkVRFName(netInfo))
			Expect(err).NotTo(HaveOccurred())
			vrfDevice, ok := vrfLink.(*netlink.Vrf)
			Expect(ok).To(BeTrue())
			Expect(vrfDevice.Table).To(Equal(uint32(dpuUDNVRFRouteTableID(netInfo.GetNetworkID()))))
			Expect(vrfLink.Attrs().MasterIndex).To(Equal(0))
			Expect(udnGateway.vrfTableId).To(Equal(dpuUDNVRFRouteTableID(netInfo.GetNetworkID())))
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
	})

	ovntest.OnSupportedPlatformsIt("should have default route when network is advertised on default VRF", func() {
		config.Gateway.Interface = "eth0"
		config.IPv4Mode = true
		config.IPv6Mode = true
		config.Gateway.NextHop = "10.0.0.11"
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
				Annotations: map[string]string{
					"k8s.ovn.org/node-subnets": fmt.Sprintf("{\"%s\":[\"%s\", \"%s\"]}", netName, v4NodeSubnet, v6NodeSubnet),
				},
			},
		}
		nad := ovntest.GenerateNAD(netName, "rednad", "greenamespace",
			types.Layer3Topology, "100.128.0.0/16/24,ae70::/60/64", types.NetworkRolePrimary)
		ovntest.AnnotateNADWithNetworkID(netID, nad)
		netInfo, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())
		mutableNetInfo := util.NewMutableNetInfo(netInfo)
		mutableNetInfo.SetPodNetworkAdvertisedVRFs(map[string][]string{node.Name: {types.DefaultNetworkName}})
		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()
			ofm := getDummyOpenflowManager()
			udnGateway, err := NewUserDefinedNetworkGateway(mutableNetInfo, node, emptyNodeInformer(), nil, vrf, nil,
				&gateway{openflowManager: ofm, nextHops: ovntest.MustParseIPs(config.Gateway.NextHop)},
				nil, nil, nil)
			Expect(err).NotTo(HaveOccurred())
			mplink, err := netlink.LinkByName(mgtPort)
			Expect(err).NotTo(HaveOccurred())
			bridgelink, err := netlink.LinkByName("breth0")
			Expect(err).NotTo(HaveOccurred())
			vrfTableId := util.CalculateRouteTableID(mplink.Attrs().Index)
			udnGateway.vrfTableId = vrfTableId

			routes, err := udnGateway.computeRoutesForUDN(mplink)
			Expect(err).NotTo(HaveOccurred())
			Expect(routes).To(HaveLen(11))
			Expect(err).NotTo(HaveOccurred())
			Expect(*routes[2].Dst).To(Equal(*ovntest.MustParseIPNet("0.0.0.0/0")))
			Expect(routes[2].LinkIndex).To(Equal(bridgelink.Attrs().Index))
			Expect(routes[2].Gw.Equal(ovntest.MustParseIP(config.Gateway.NextHop))).To(BeTrue())
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
	})
	ovntest.OnSupportedPlatformsIt("should use Uplink for service and default routes", func() {
		config.Gateway.Interface = "eth0"
		config.Gateway.Mode = config.GatewayModeShared
		config.IPv4Mode = true
		config.IPv6Mode = true
		config.Kubernetes.ServiceCIDRs = ovntest.MustParseIPNets("10.96.0.0/16", "fd00:10:96::/112")
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
				Annotations: map[string]string{
					"k8s.ovn.org/node-subnets": fmt.Sprintf("{\"%s\":[\"%s\", \"%s\"]}", netName, v4NodeSubnet, v6NodeSubnet),
				},
			},
		}
		nad := generateUplinkNAD(netName, "rednad", "greenamespace",
			types.Layer3Topology, "100.128.0.0/16/24,ae70::/60/64", types.NetworkRolePrimary, "uplink1")
		ovntest.AnnotateNADWithNetworkID(netID, nad)
		netInfo, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())
		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()
			ofm := getDummyOpenflowManager()
			uplinkStateInformer, statusController := newGatewayUplinkDependencies(node.Name)
			DeferCleanup(statusController.Stop)
			udnGateway, err := NewUserDefinedNetworkGateway(netInfo, node, emptyNodeInformer(), nil, vrf, nil,
				&gateway{openflowManager: ofm}, nil, uplinkStateInformer, statusController)
			Expect(err).NotTo(HaveOccurred())
			mplink, err := netlink.LinkByName(mgtPort)
			Expect(err).NotTo(HaveOccurred())
			udnGateway.vrfTableId = util.CalculateRouteTableID(mplink.Attrs().Index)
			udnGateway.gwInterfaceIndex = 77
			udnGateway.nextHops = ovntest.MustParseIPs("192.0.2.1", "2001:db8::1")

			routes, err := udnGateway.computeRoutesForUDN(mplink)
			Expect(err).NotTo(HaveOccurred())
			Expect(routes).To(HaveLen(13))

			Expect(*routes[0].Dst).To(Equal(*config.Kubernetes.ServiceCIDRs[0]))
			Expect(routes[0].LinkIndex).To(Equal(udnGateway.gwInterfaceIndex))
			Expect(routes[0].Flags).To(Equal(unix.RTNH_F_ONLINK))
			Expect(routes[0].Src.Equal(config.Gateway.MasqueradeIPs.V4HostMasqueradeIP)).To(BeTrue())

			Expect(*routes[1].Dst).To(Equal(*util.GetIPNetFullMaskFromIP(config.Gateway.MasqueradeIPs.V6DummyNextHopMasqueradeIP)))
			Expect(routes[1].LinkIndex).To(Equal(udnGateway.gwInterfaceIndex))
			Expect(routes[1].Scope).To(Equal(netlink.SCOPE_LINK))
			Expect(routes[1].Gw).To(BeNil())

			Expect(*routes[2].Dst).To(Equal(*config.Kubernetes.ServiceCIDRs[1]))
			Expect(routes[2].LinkIndex).To(Equal(udnGateway.gwInterfaceIndex))
			Expect(routes[2].Flags).To(Equal(unix.RTNH_F_ONLINK))
			Expect(routes[2].Src.Equal(config.Gateway.MasqueradeIPs.V6HostMasqueradeIP)).To(BeTrue())

			Expect(*routes[3].Dst).To(Equal(*ovntest.MustParseIPNet("0.0.0.0/0")))
			Expect(routes[3].LinkIndex).To(Equal(udnGateway.gwInterfaceIndex))
			Expect(routes[3].Gw.Equal(ovntest.MustParseIP("192.0.2.1"))).To(BeTrue())
			Expect(*routes[4].Dst).To(Equal(*ovntest.MustParseIPNet("::/0")))
			Expect(routes[4].LinkIndex).To(Equal(udnGateway.gwInterfaceIndex))
			Expect(routes[4].Gw.Equal(ovntest.MustParseIP("2001:db8::1"))).To(BeTrue())
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
	})
	ovntest.OnSupportedPlatformsIt("should filter Uplink default routes by network IP family", func() {
		config.Gateway.Interface = "eth0"
		config.Gateway.Mode = config.GatewayModeShared
		config.IPv4Mode = false
		config.IPv6Mode = true
		config.Kubernetes.ServiceCIDRs = ovntest.MustParseIPNets("fd00:10:96::/112")
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
				Annotations: map[string]string{
					"k8s.ovn.org/node-subnets": fmt.Sprintf("{\"%s\":[\"%s\"]}", netName, v6NodeSubnet),
				},
			},
		}
		nad := generateUplinkNAD(netName, "rednad", "greenamespace",
			types.Layer3Topology, "ae70::/60/64", types.NetworkRolePrimary, "uplink1")
		ovntest.AnnotateNADWithNetworkID(netID, nad)
		netInfo, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())
		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()
			ofm := getDummyOpenflowManager()
			uplinkStateInformer, statusController := newGatewayUplinkDependencies(node.Name)
			DeferCleanup(statusController.Stop)
			udnGateway, err := NewUserDefinedNetworkGateway(netInfo, node, emptyNodeInformer(), nil, vrf, nil,
				&gateway{openflowManager: ofm}, nil, uplinkStateInformer, statusController)
			Expect(err).NotTo(HaveOccurred())
			mplink, err := netlink.LinkByName(mgtPort)
			Expect(err).NotTo(HaveOccurred())
			udnGateway.vrfTableId = util.CalculateRouteTableID(mplink.Attrs().Index)
			udnGateway.gwInterfaceIndex = 77
			udnGateway.nextHops = ovntest.MustParseIPs("192.0.2.1", "2001:db8::1")

			routes, err := udnGateway.computeRoutesForUDN(mplink)
			Expect(err).NotTo(HaveOccurred())
			Expect(routes).To(HaveLen(7))

			Expect(*routes[0].Dst).To(Equal(*util.GetIPNetFullMaskFromIP(config.Gateway.MasqueradeIPs.V6DummyNextHopMasqueradeIP)))
			Expect(routes[0].LinkIndex).To(Equal(udnGateway.gwInterfaceIndex))
			Expect(routes[0].Scope).To(Equal(netlink.SCOPE_LINK))

			Expect(*routes[1].Dst).To(Equal(*config.Kubernetes.ServiceCIDRs[0]))
			Expect(routes[1].LinkIndex).To(Equal(udnGateway.gwInterfaceIndex))
			Expect(routes[1].Gw.Equal(config.Gateway.MasqueradeIPs.V6DummyNextHopMasqueradeIP)).To(BeTrue())
			Expect(routes[1].Src.Equal(config.Gateway.MasqueradeIPs.V6HostMasqueradeIP)).To(BeTrue())
			Expect(routes[1].Flags).To(Equal(unix.RTNH_F_ONLINK))

			Expect(*routes[2].Dst).To(Equal(*ovntest.MustParseIPNet("::/0")))
			Expect(routes[2].LinkIndex).To(Equal(udnGateway.gwInterfaceIndex))
			Expect(routes[2].Gw.Equal(ovntest.MustParseIP("2001:db8::1"))).To(BeTrue())
			for _, route := range routes {
				Expect(route.Dst.String()).NotTo(Equal("0.0.0.0/0"))
			}
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
	})

	ovntest.OnSupportedPlatformsIt("should enslave Uplink gateway interface based on advertisement VRF", func() {
		config.Gateway.Interface = "eth0"
		config.Gateway.Mode = config.GatewayModeShared
		config.IPv4Mode = true
		config.IPv6Mode = true
		getIPv6KeepAddrOnDownFakeCommand(fexec, "ovsbr1")
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
				Annotations: map[string]string{
					"k8s.ovn.org/node-subnets": fmt.Sprintf("{\"%s\":[\"%s\"]}", netName, v4NodeSubnet),
				},
			},
		}
		nad := ovntest.GenerateNADWithConfig("rednad", "greenamespace", fmt.Sprintf(
			`
{
        "cniVersion": "1.1.0",
        "name": %q,
        "type": "ovn-k8s-cni-overlay",
        "topology": %q,
        "subnets": "100.128.0.0/16/24",
        "mtu": 1300,
        "netAttachDefName": "greenamespace/rednad",
        "role": %q,
        "uplink": "uplink1"
}
`,
			netName,
			types.Layer3Topology,
			types.NetworkRolePrimary,
		))
		ovntest.AnnotateNADWithNetworkID(netID, nad)
		netInfo, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())
		mutableNetInfo := util.NewMutableNetInfo(netInfo)
		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()
			ofm := getDummyOpenflowManager()
			_, uplinkStateLister := newGatewayUplinkStateAndLister("uplink1", node.Name)
			uplinkStateInformer, statusController := newGatewayUplinkDependencies(node.Name)
			DeferCleanup(statusController.Stop)
			udnGateway, err := NewUserDefinedNetworkGateway(mutableNetInfo, node, emptyNodeInformer(), nil, vrf, nil,
				&gateway{openflowManager: ofm}, nil, uplinkStateInformer, statusController)
			Expect(err).NotTo(HaveOccurred())
			udnGateway.uplinkStateLister = uplinkStateLister
			uplinkLink := ovntest.AddLink("ovsbr1")
			udnGateway.gwInterfaceName = uplinkLink.Attrs().Name
			mplink, err := netlink.LinkByName(mgtPort)
			Expect(err).NotTo(HaveOccurred())
			vrfTableId := util.CalculateRouteTableID(mplink.Attrs().Index)
			vrfDeviceName := util.GetNetworkVRFName(udnGateway.NetInfo)
			Expect(vrf.AddVRF(vrfDeviceName, mplink.Attrs().Name, uint32(vrfTableId), nil)).To(Succeed())

			udnGateway.isNetworkAdvertised = true
			udnGateway.isNetworkAdvertisedToDefaultVRF = false
			Expect(udnGateway.reconcileUplinkGatewayVRFSlave(vrfDeviceName)).To(Succeed())
			vrfLink, err := netlink.LinkByName(vrfDeviceName)
			Expect(err).NotTo(HaveOccurred())
			uplinkLink, err = netlink.LinkByName("ovsbr1")
			Expect(err).NotTo(HaveOccurred())
			Expect(uplinkLink.Attrs().MasterIndex).To(Equal(vrfLink.Attrs().Index))

			udnGateway.isNetworkAdvertisedToDefaultVRF = true
			Expect(udnGateway.reconcileUplinkGatewayVRFSlave(vrfDeviceName)).To(Succeed())
			uplinkLink, err = netlink.LinkByName("ovsbr1")
			Expect(err).NotTo(HaveOccurred())
			Expect(uplinkLink.Attrs().MasterIndex).To(Equal(0))
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
	})

	ovntest.OnSupportedPlatformsIt("should omit default route when network is advertised on any other vrf than default", func() {
		config.Gateway.Interface = "eth0"
		config.IPv4Mode = true
		config.IPv6Mode = true
		config.Gateway.NextHop = "10.0.0.11"
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
				Annotations: map[string]string{
					"k8s.ovn.org/node-subnets": fmt.Sprintf("{\"%s\":[\"%s\", \"%s\"]}", netName, v4NodeSubnet, v6NodeSubnet),
				},
			},
		}
		nad := ovntest.GenerateNAD(netName, "rednad", "greenamespace",
			types.Layer3Topology, "100.128.0.0/16/24,ae70::/60/64", types.NetworkRolePrimary)
		ovntest.AnnotateNADWithNetworkID(netID, nad)
		netInfo, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())
		mutableNetInfo := util.NewMutableNetInfo(netInfo)
		mutableNetInfo.SetPodNetworkAdvertisedVRFs(map[string][]string{node.Name: {netName}})
		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()
			ofm := getDummyOpenflowManager()
			udnGateway, err := NewUserDefinedNetworkGateway(mutableNetInfo, node, emptyNodeInformer(), nil, vrf, nil, &gateway{openflowManager: ofm}, nil, nil, nil)
			Expect(err).NotTo(HaveOccurred())
			mplink, err := netlink.LinkByName(mgtPort)
			Expect(err).NotTo(HaveOccurred())
			vrfTableId := util.CalculateRouteTableID(mplink.Attrs().Index)
			udnGateway.vrfTableId = vrfTableId

			routes, err := udnGateway.computeRoutesForUDN(mplink)
			Expect(err).NotTo(HaveOccurred())
			Expect(routes).To(HaveLen(10))
			Expect(err).NotTo(HaveOccurred())
			Expect(*routes[1].Dst).To(Not(Equal(*ovntest.MustParseIPNet("0.0.0.0/0"))))
			Expect(routes[1].Gw.Equal(ovntest.MustParseIP(config.Gateway.NextHop))).To(BeFalse())
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
	})

	// Verifies that node network connected subnet routes (e.g. 192.168.1.0/24 dev br-ex scope link src 192.168.1.10)
	// are programmed in the VRF table when gateway bridge IPs are configured. These routes enable direct L2 forwarding
	// to other nodes on the same subnet without routing through the default gateway.
	ovntest.OnSupportedPlatformsIt("should add node network connected subnet routes to VRF table", func() {
		config.Gateway.Interface = "eth0"
		config.IPv4Mode = true
		config.IPv6Mode = true
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
				Annotations: map[string]string{
					"k8s.ovn.org/node-subnets": fmt.Sprintf("{\"%s\":[\"%s\", \"%s\"]}", netName, v4NodeSubnet, v6NodeSubnet),
				},
			},
		}
		nad := ovntest.GenerateNAD(netName, "rednad", "greenamespace",
			types.Layer3Topology, "100.128.0.0/16/24,ae70::/60/64", types.NetworkRolePrimary)
		ovntest.AnnotateNADWithNetworkID(netID, nad)
		netInfo, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())
		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()
			ofm := getDummyOpenflowManager(ovntest.MustParseIPNets(v4NodeIP, v6NodeIP)...)
			udnGateway, err := NewUserDefinedNetworkGateway(netInfo, node, emptyNodeInformer(), nil, vrf, nil, &gateway{openflowManager: ofm}, nil, nil, nil)
			Expect(err).NotTo(HaveOccurred())
			mplink, err := netlink.LinkByName(mgtPort)
			Expect(err).NotTo(HaveOccurred())
			bridgelink, err := netlink.LinkByName("breth0")
			Expect(err).NotTo(HaveOccurred())
			vrfTableId := util.CalculateRouteTableID(mplink.Attrs().Index)
			udnGateway.vrfTableId = vrfTableId
			// For non-uplink networks the node subnet routes are derived from the
			// shared gateway bridge; addNetworkWithResolvedUplink wires this up at
			// runtime, so set it explicitly here.
			udnGateway.nodeSubnetBridge = ofm.defaultBridge.BridgeConfiguration

			routes := udnGateway.computeNodeSubnetRoutes()

			// Verify node network connected subnet routes are present for both IPv4 and IPv6.
			// Match by scope and destination to stay resilient to route ordering changes.
			var v4Found, v6Found bool
			for _, route := range routes {
				if route.Scope == netlink.SCOPE_LINK && route.Dst != nil {
					if route.Dst.String() == "192.168.1.0/24" {
						Expect(route.LinkIndex).To(Equal(bridgelink.Attrs().Index))
						Expect(route.Src.Equal(ovntest.MustParseIP("192.168.1.10"))).To(BeTrue())
						Expect(route.Table).To(Equal(vrfTableId))
						v4Found = true
					}
					if route.Dst.String() == "fc00:f853:ccd:e793::/64" {
						Expect(route.LinkIndex).To(Equal(bridgelink.Attrs().Index))
						Expect(route.Src.Equal(ovntest.MustParseIP("fc00:f853:ccd:e793::3"))).To(BeTrue())
						Expect(route.Table).To(Equal(vrfTableId))
						v6Found = true
					}
				}
			}
			Expect(v4Found).To(BeTrue(), "IPv4 node network connected subnet route not found")
			Expect(v6Found).To(BeTrue(), "IPv6 node network connected subnet route not found")
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
	})

	ovntest.OnSupportedPlatformsIt("should filter node subnet routes by IP family and tolerate nil bridge IPs", func() {
		config.Gateway.Interface = "eth0"
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName, Annotations: map[string]string{
			"k8s.ovn.org/node-subnets": fmt.Sprintf("{\"%s\":[\"%s\", \"%s\"]}", netName, v4NodeSubnet, v6NodeSubnet),
		}}}
		for _, tc := range []struct {
			ipv4, ipv6     bool
			cidr           string
			wantV4, wantV6 bool
			injectNil      bool
			desc           string
		}{
			{true, false, "100.128.0.0/16/24", true, false, false, "IPv4-only"},
			{false, true, "ae70::/60/64", false, true, false, "IPv6-only"},
			{true, true, "100.128.0.0/16/24,ae70::/60/64", true, true, true, "nil bridge IP tolerance"},
		} {
			config.IPv4Mode = tc.ipv4
			config.IPv6Mode = tc.ipv6
			nad := ovntest.GenerateNAD(netName, "rednad", "greenamespace",
				types.Layer3Topology, tc.cidr, types.NetworkRolePrimary)
			ovntest.AnnotateNADWithNetworkID(netID, nad)
			netInfo, err := util.ParseNADInfo(nad)
			Expect(err).NotTo(HaveOccurred())
			err = testNS.Do(func(ns.NetNS) error {
				defer GinkgoRecover()
				ips := ovntest.MustParseIPNets(v4NodeIP, v6NodeIP)
				if tc.injectNil {
					ips = append(ips, nil)
				}
				ofm := getDummyOpenflowManager(ips...)
				gw, err := NewUserDefinedNetworkGateway(netInfo, node, emptyNodeInformer(), nil, vrf, nil, &gateway{openflowManager: ofm}, nil, nil, nil)
				Expect(err).NotTo(HaveOccurred())
				mp, err := netlink.LinkByName(mgtPort)
				Expect(err).NotTo(HaveOccurred())
				gw.vrfTableId = util.CalculateRouteTableID(mp.Attrs().Index)
				gw.nodeSubnetBridge = ofm.defaultBridge.BridgeConfiguration
				routes := gw.computeNodeSubnetRoutes()
				var v4, v6 bool
				for _, r := range routes {
					if r.Scope == netlink.SCOPE_LINK && r.Dst != nil {
						switch r.Dst.String() {
						case "192.168.1.0/24":
							v4 = true
						case "fc00:f853:ccd:e793::/64":
							v6 = true
						}
					}
				}
				Expect(v4).To(Equal(tc.wantV4), tc.desc+": IPv4 scope-link route")
				Expect(v6).To(Equal(tc.wantV6), tc.desc+": IPv6 scope-link route")
				return nil
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
		}
	})

	// Verifies node subnet routes are recomputed from the gateway bridge's
	// current IPs rather than a construction-time snapshot, so a runtime
	// address change on the bridge is reflected in the routes.
	ovntest.OnSupportedPlatformsIt("should recompute node subnet routes when gateway bridge IPs change", func() {
		config.Gateway.Interface = "eth0"
		config.IPv4Mode = true
		config.IPv6Mode = false
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName, Annotations: map[string]string{
			"k8s.ovn.org/node-subnets": fmt.Sprintf("{\"%s\":[\"%s\"]}", netName, v4NodeSubnet),
		}}}
		nad := ovntest.GenerateNAD(netName, "rednad", "greenamespace",
			types.Layer3Topology, "100.128.0.0/16/24", types.NetworkRolePrimary)
		ovntest.AnnotateNADWithNetworkID(netID, nad)
		netInfo, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())
		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()
			ofm := getDummyOpenflowManager(ovntest.MustParseIPNets(v4NodeIP)...)
			udnGateway, err := NewUserDefinedNetworkGateway(netInfo, node, emptyNodeInformer(), nil, vrf, nil, &gateway{openflowManager: ofm}, nil, nil, nil)
			Expect(err).NotTo(HaveOccurred())
			mplink, err := netlink.LinkByName(mgtPort)
			Expect(err).NotTo(HaveOccurred())
			udnGateway.vrfTableId = util.CalculateRouteTableID(mplink.Attrs().Index)
			udnGateway.nodeSubnetBridge = ofm.defaultBridge.BridgeConfiguration

			routes := udnGateway.computeNodeSubnetRoutes()
			Expect(routes).To(HaveLen(1))
			Expect(routes[0].Dst.String()).To(Equal("192.168.1.0/24"))
			Expect(routes[0].Src.Equal(ovntest.MustParseIP("192.168.1.10"))).To(BeTrue())

			// Simulate a runtime address change on the gateway bridge, as the
			// addressManager would apply via UpdateInterfaceIPAddresses.
			ofm.defaultBridge.SetIPs(ovntest.MustParseIPNets("192.168.5.20/24"))

			routes = udnGateway.computeNodeSubnetRoutes()
			Expect(routes).To(HaveLen(1))
			Expect(routes[0].Dst.String()).To(Equal("192.168.5.0/24"))
			Expect(routes[0].Src.Equal(ovntest.MustParseIP("192.168.5.20"))).To(BeTrue())
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
	})

	// Verifies runtime route replacement end to end through
	// reconcileNodeSubnetRoutes: the initial route is programmed into the VRF
	// table, and after a gateway bridge address change the stale route is
	// removed from the table and the new one is installed.
	ovntest.OnSupportedPlatformsIt("should replace stale node subnet routes in the VRF when gateway bridge IPs change", func() {
		config.Gateway.Interface = "eth0"
		config.IPv4Mode = true
		config.IPv6Mode = false
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName, Annotations: map[string]string{
			"k8s.ovn.org/node-subnets": fmt.Sprintf("{\"%s\":[\"%s\"]}", netName, v4NodeSubnet),
		}}}
		nad := ovntest.GenerateNAD(netName, "rednad", "greenamespace",
			types.Layer3Topology, "100.128.0.0/16/24", types.NetworkRolePrimary)
		ovntest.AnnotateNADWithNetworkID(netID, nad)
		netInfo, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())
		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()
			ofm := getDummyOpenflowManager(ovntest.MustParseIPNets(v4NodeIP)...)
			udnGateway, err := NewUserDefinedNetworkGateway(netInfo, node, emptyNodeInformer(), nil, vrf, nil, &gateway{openflowManager: ofm}, nil, nil, nil)
			Expect(err).NotTo(HaveOccurred())
			mplink, err := netlink.LinkByName(mgtPort)
			Expect(err).NotTo(HaveOccurred())
			bridgelink, err := netlink.LinkByName("breth0")
			Expect(err).NotTo(HaveOccurred())
			vrfTableId := util.CalculateRouteTableID(mplink.Attrs().Index)
			udnGateway.vrfTableId = vrfTableId
			udnGateway.gwInterfaceIndex = bridgelink.Attrs().Index
			udnGateway.nodeSubnetBridge = ofm.defaultBridge.BridgeConfiguration

			vrfDeviceName := util.GetNetworkVRFName(udnGateway.NetInfo)
			Expect(vrf.AddVRF(vrfDeviceName, mplink.Attrs().Name, uint32(vrfTableId), nil)).To(Succeed())

			// vrfRouteDsts returns the destinations currently programmed in the VRF table.
			vrfRouteDsts := func() []string {
				kernelRoutes, lerr := netlink.RouteListFiltered(netlink.FAMILY_ALL,
					&netlink.Route{Table: vrfTableId}, netlink.RT_FILTER_TABLE)
				Expect(lerr).NotTo(HaveOccurred())
				var dsts []string
				for _, r := range kernelRoutes {
					if r.Dst != nil {
						dsts = append(dsts, r.Dst.String())
					}
				}
				return dsts
			}

			// Program the initial route and confirm it lands in the VRF table.
			Expect(udnGateway.reconcileNodeSubnetRoutes()).To(Succeed())
			Expect(vrfRouteDsts()).To(ContainElement("192.168.1.0/24"))

			// Simulate a runtime address change: assign the new address to the
			// bridge link so the route's preferred source is valid, and update
			// the cached bridge IPs as the addressManager would.
			newAddr, err := netlink.ParseAddr("192.168.5.20/24")
			Expect(err).NotTo(HaveOccurred())
			Expect(netlink.AddrAdd(bridgelink, newAddr)).To(Succeed())
			ofm.defaultBridge.SetIPs(ovntest.MustParseIPNets("192.168.5.20/24"))

			// Reconcile again: the stale route must be removed and the new one added.
			Expect(udnGateway.reconcileNodeSubnetRoutes()).To(Succeed())
			dsts := vrfRouteDsts()
			Expect(dsts).To(ContainElement("192.168.5.0/24"))
			Expect(dsts).NotTo(ContainElement("192.168.1.0/24"))

			// Tracking must reflect only the new route.
			Expect(udnGateway.nodeSubnetRoutes).To(HaveLen(1))
			Expect(udnGateway.nodeSubnetRoutes[0].Dst.String()).To(Equal("192.168.5.0/24"))
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
	})

	// Verifies that a network bound to an Uplink does NOT get node subnet routes:
	// its VRF already has the Uplink's connected prefix route enslaved by the
	// kernel, so addNetworkWithResolvedUplink leaves nodeSubnetBridge nil. This
	// asserts reconcileNodeSubnetRoutes is then a no-op that does not touch a
	// pre-existing (kernel-owned) route already present in the VRF table.
	ovntest.OnSupportedPlatformsIt("should not program node subnet routes for uplink-backed networks", func() {
		config.Gateway.Interface = "eth0"
		config.IPv4Mode = true
		config.IPv6Mode = false
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName, Annotations: map[string]string{
			"k8s.ovn.org/node-subnets": fmt.Sprintf("{\"%s\":[\"%s\"]}", netName, v4NodeSubnet),
		}}}
		nad := ovntest.GenerateNAD(netName, "rednad", "greenamespace",
			types.Layer3Topology, "100.128.0.0/16/24", types.NetworkRolePrimary)
		ovntest.AnnotateNADWithNetworkID(netID, nad)
		netInfo, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())
		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()
			ofm := getDummyOpenflowManager(ovntest.MustParseIPNets(v4NodeIP)...)
			udnGateway, err := NewUserDefinedNetworkGateway(netInfo, node, emptyNodeInformer(), nil, vrf, nil, &gateway{openflowManager: ofm}, nil, nil, nil)
			Expect(err).NotTo(HaveOccurred())
			mplink, err := netlink.LinkByName(mgtPort)
			Expect(err).NotTo(HaveOccurred())
			vrfTableId := util.CalculateRouteTableID(mplink.Attrs().Index)
			udnGateway.vrfTableId = vrfTableId
			// Uplink-backed networks leave nodeSubnetBridge nil (set by
			// addNetworkWithResolvedUplink); computeNodeSubnetRoutes yields nothing.
			udnGateway.nodeSubnetBridge = nil
			Expect(udnGateway.computeNodeSubnetRoutes()).To(BeEmpty())

			vrfDeviceName := util.GetNetworkVRFName(udnGateway.NetInfo)
			Expect(vrf.AddVRF(vrfDeviceName, mplink.Attrs().Name, uint32(vrfTableId), nil)).To(Succeed())

			// Seed a kernel-owned connected route in the VRF, as the Uplink feature
			// does via bridge enslavement; reconcile must leave it untouched.
			preExisting := &netlink.Route{
				LinkIndex: mplink.Attrs().Index,
				Dst:       ovntest.MustParseIPNet("10.10.0.0/24"),
				Table:     vrfTableId,
				Scope:     netlink.SCOPE_LINK,
				Protocol:  unix.RTPROT_KERNEL,
			}
			Expect(netlink.RouteAdd(preExisting)).To(Succeed())

			Expect(udnGateway.reconcileNodeSubnetRoutes()).To(Succeed())
			Expect(udnGateway.nodeSubnetRoutes).To(BeEmpty())

			kernelRoutes, lerr := netlink.RouteListFiltered(netlink.FAMILY_ALL,
				&netlink.Route{Table: vrfTableId}, netlink.RT_FILTER_TABLE)
			Expect(lerr).NotTo(HaveOccurred())
			var found bool
			for _, r := range kernelRoutes {
				if r.Dst != nil && r.Dst.String() == "10.10.0.0/24" {
					found = true
					Expect(r.Protocol).To(Equal(netlink.RouteProtocol(unix.RTPROT_KERNEL)))
				}
			}
			Expect(found).To(BeTrue(), "pre-existing kernel route must survive reconcile")
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
	})

	ovntest.OnSupportedPlatformsIt("should set rp filter to loose mode for management port interface", func() {
		getRPFilterLooseModeFakeCommands(fexec)
		err := testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()
			err := util.SetRPFilterLooseModeForInterface(mgtPort)
			Expect(err).NotTo(HaveOccurred())
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
	})

	ovntest.OnSupportedPlatformsIt("should sync node port watcher successfully if a namespaces network is invalid", func() {
		// create new gateway, add ns with primary UDN, pod, expose pod via Node port service, delete pod, delete udn, ensure sync should succeeds
		namespace := util.NewNamespace("udn")
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		namespace.Labels[types.RequiredUDNNamespaceLabel] = ""
		service := newService("udn-svc", namespace.Name, "10.96.0.10", []corev1.ServicePort{{NodePort: int32(30300),
			Protocol: corev1.ProtocolTCP, Port: int32(8080)}}, corev1.ServiceTypeNodePort, []string{}, corev1.ServiceStatus{},
			true, false)
		fakeClient := util.GetOVNClientset(service, namespace)
		wf, err := factory.NewNodeWatchFactory(fakeClient.GetNodeClientset(), "node")
		Expect(err).ToNot(HaveOccurred(), "must get new node watch factory")
		Expect(wf.Start()).NotTo(HaveOccurred(), "must start Node watch factory")
		defer func() {
			wf.Shutdown()
		}()
		fNPW := initFakeNodePortWatcher()
		fNPW.watchFactory = wf
		// in-order to simulate a namespace with an Invalid UDN (when GetActiveNamespace is called), we add an entry
		// to the fake network manager but no specified network. GetActiveNetwork will return the appropriate error of Invalid Network for namespace.
		// network manager may have a different implementation that fake network manager but both will return the same error.
		fNPW.networkManager = &networkmanager.FakeNetworkManager{PrimaryNetworks: map[string]util.NetInfo{namespace.Name: nil}}
		services := append([]interface{}{}, service)
		Expect(fNPW.SyncServices(services)).NotTo(HaveOccurred(), "must sync services")
	})
})

func prepareNoOverlayLocalGatewayTestConfig(t *testing.T) {
	t.Helper()
	if err := config.PrepareTestConfig(); err != nil {
		t.Fatalf("failed to prepare test config: %v", err)
	}
	t.Cleanup(func() {
		_ = config.PrepareTestConfig()
	})
	config.Gateway.Mode = config.GatewayModeLocal
	config.IPv4Mode = true
	config.IPv6Mode = true
	config.Kubernetes.ServiceCIDRs = ovntest.MustParseIPNets("172.16.1.0/24", "fd02::/112")
	config.Gateway.V6MasqueradeSubnet = "fd69::/112"
	config.Gateway.V4MasqueradeSubnet = "169.254.0.0/17"
}

func TestNoOverlayLocalGatewayUsesNoPrefixRouteManagementPortIPs(t *testing.T) {
	prepareNoOverlayLocalGatewayTestConfig(t)
	g := NewWithT(t)

	udnGateway := &UserDefinedNetworkGateway{
		NetInfo: noOverlayLayer3NetInfo(t),
	}

	g.Expect(udnGateway.useNoPrefixRouteManagementPortIPs()).To(BeTrue())

	config.Gateway.Mode = config.GatewayModeShared
	g.Expect(udnGateway.useNoPrefixRouteManagementPortIPs()).To(BeFalse())
}

func TestNoOverlayLocalGatewayUDNGatewayRoutesUseLocalSubnet(t *testing.T) {
	prepareNoOverlayLocalGatewayTestConfig(t)
	g := NewWithT(t)

	udnGateway := &UserDefinedNetworkGateway{
		NetInfo: noOverlayLayer3NetInfo(t),
		node: &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "worker1",
				Annotations: map[string]string{
					"k8s.ovn.org/node-subnets": `{"bluenet":["100.128.0.0/24", "ae70::/64"]}`,
				},
			},
		},
		gateway:          &gateway{},
		vrfTableId:       1007,
		gwInterfaceIndex: 11,
	}
	mpLink := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "ovn-k8s-mp3", Index: 23}}

	routes, err := udnGateway.computeRoutesForUDN(mpLink)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(routes).To(HaveLen(12))
	// routes[0]: IPv4 service CIDR route.
	g.Expect(routes[0].Flags).To(Equal(0))

	// routes[4]: IPv4 gateway host route.
	g.Expect(*routes[4].Dst).To(Equal(*ovntest.MustParseIPNet("100.128.0.1/32")))
	g.Expect(routes[4].Gw).To(BeNil())
	g.Expect(routes[4].Scope).To(Equal(netlink.SCOPE_LINK))
	g.Expect(routes[4].Flags).To(Equal(0))

	// routes[5]: IPv4 ETP/local service masquerade IP route.
	g.Expect(*routes[5].Dst).To(Equal(*ovntest.MustParseIPNet("169.254.169.3/32")))
	g.Expect(routes[5].Gw.Equal(ovntest.MustParseIP("100.128.0.1"))).To(BeTrue())
	g.Expect(routes[5].Flags).To(Equal(0))

	// routes[6]: IPv4 node pod CIDR route.
	g.Expect(*routes[6].Dst).To(Equal(*ovntest.MustParseIPNet("100.128.0.0/24")))
	g.Expect(routes[6].Gw.Equal(ovntest.MustParseIP("100.128.0.1"))).To(BeTrue())
	g.Expect(routes[6].Flags).To(Equal(0))

	// routes[7]: IPv6 gateway host route.
	g.Expect(*routes[7].Dst).To(Equal(*ovntest.MustParseIPNet("ae70::1/128")))
	g.Expect(routes[7].Gw).To(BeNil())
	g.Expect(routes[7].Scope).To(Equal(netlink.SCOPE_LINK))
	g.Expect(routes[7].Flags).To(Equal(0))

	// routes[8]: IPv6 ETP/local service masquerade IP route.
	g.Expect(*routes[8].Dst).To(Equal(*ovntest.MustParseIPNet("fd69::3/128")))
	g.Expect(routes[8].Gw.Equal(ovntest.MustParseIP("ae70::1"))).To(BeTrue())
	g.Expect(routes[8].Flags).To(Equal(0))

	// routes[9]: IPv6 node pod CIDR route.
	g.Expect(*routes[9].Dst).To(Equal(*ovntest.MustParseIPNet("ae70::/64")))
	g.Expect(routes[9].Gw.Equal(ovntest.MustParseIP("ae70::1"))).To(BeTrue())
	g.Expect(routes[9].Flags).To(Equal(0))

	config.Gateway.Mode = config.GatewayModeShared
	routes, err = udnGateway.computeRoutesForUDN(mpLink)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(routes).To(HaveLen(10))
	// routes[4]: IPv4 ETP/local service masquerade IP route.
	g.Expect(*routes[4].Dst).To(Equal(*ovntest.MustParseIPNet("169.254.169.3/32")))
	g.Expect(routes[4].Gw.Equal(ovntest.MustParseIP("100.128.0.1"))).To(BeTrue())
	// routes[5]: IPv4 global pod CIDR route.
	g.Expect(*routes[5].Dst).To(Equal(*ovntest.MustParseIPNet("100.128.0.0/16")))
	g.Expect(routes[5].Gw.Equal(ovntest.MustParseIP("100.128.0.1"))).To(BeTrue())
	// routes[6]: IPv6 ETP/local service masquerade IP route.
	g.Expect(*routes[6].Dst).To(Equal(*ovntest.MustParseIPNet("fd69::3/128")))
	g.Expect(routes[6].Gw.Equal(ovntest.MustParseIP("ae70::1"))).To(BeTrue())
	// routes[7]: IPv6 global pod CIDR route.
	g.Expect(*routes[7].Dst).To(Equal(*ovntest.MustParseIPNet("ae70::/60")))
	g.Expect(routes[7].Gw.Equal(ovntest.MustParseIP("ae70::1"))).To(BeTrue())
}

func TestAdvertisedNoOverlayLocalGatewayRoutesDoNotUseConnectedSubnetPrefix(t *testing.T) {
	prepareNoOverlayLocalGatewayTestConfig(t)
	g := NewWithT(t)

	udnGateway := &UserDefinedNetworkGateway{
		NetInfo: noOverlayLayer3NetInfo(t),
		node: &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "worker1",
				Annotations: map[string]string{
					"k8s.ovn.org/node-subnets": `{"bluenet":["100.128.0.0/24", "ae70::/64"]}`,
				},
			},
		},
		gateway:             &gateway{},
		vrfTableId:          1007,
		gwInterfaceIndex:    11,
		isNetworkAdvertised: true,
	}

	mpLink := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "ovn-k8s-mp3", Index: 23}}
	routes, err := udnGateway.computeRoutesForUDN(mpLink)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(routes).To(HaveLen(12))
	g.Expect(*routes[4].Dst).To(Equal(*ovntest.MustParseIPNet("100.128.0.1/32")))
	g.Expect(routes[4].Gw).To(BeNil())
	g.Expect(routes[4].Scope).To(Equal(netlink.SCOPE_LINK))
	g.Expect(*routes[6].Dst).To(Equal(*ovntest.MustParseIPNet("100.128.0.0/24")))
	g.Expect(routes[6].Gw.Equal(ovntest.MustParseIP("100.128.0.1"))).To(BeTrue())
	g.Expect(*routes[7].Dst).To(Equal(*ovntest.MustParseIPNet("ae70::1/128")))
	g.Expect(routes[7].Gw).To(BeNil())
	g.Expect(routes[7].Scope).To(Equal(netlink.SCOPE_LINK))
	g.Expect(*routes[9].Dst).To(Equal(*ovntest.MustParseIPNet("ae70::/64")))
	g.Expect(routes[9].Gw.Equal(ovntest.MustParseIP("ae70::1"))).To(BeTrue())
}

func TestConstructUDNVRFIPRules(t *testing.T) {
	if ovntest.NoRoot() {
		t.Skip("Test requires root privileges")
	}
	type testRule struct {
		priority int
		family   int
		table    int
		mark     uint32
		dst      netip.Prefix
	}
	type testConfig struct {
		desc          string
		vrftableID    int
		v4mode        bool
		v6mode        bool
		expectedRules []testRule
		deleteRules   []testRule
	}

	tests := []testConfig{
		{
			desc:          "empty rules test",
			vrftableID:    1007,
			expectedRules: nil,
		},
		{
			desc:       "v4 rule test",
			vrftableID: 1007,
			expectedRules: []testRule{
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V4,
					table:    1007,
					mark:     0x1003,
				},
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V4,
					table:    1007,
					dst:      netip.MustParsePrefix("169.254.0.16/32"),
				},
			},
			deleteRules: []testRule{
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V4,
					table:    1007,
					dst:      netip.MustParsePrefix("100.128.0.0/16"),
				},
			},
			v4mode: true,
		},
		{
			desc:       "v6 rule test",
			vrftableID: 1009,
			expectedRules: []testRule{
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V6,
					table:    1009,
					mark:     0x1003,
				},
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V6,
					table:    1009,
					dst:      netip.MustParsePrefix("fd69::10/128"),
				},
			},
			deleteRules: []testRule{
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V6,
					table:    1009,
					dst:      netip.MustParsePrefix("ae70::/60"),
				},
			},
			v6mode: true,
		},
		{
			desc:       "dualstack rule test",
			vrftableID: 1010,
			expectedRules: []testRule{
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V4,
					table:    1010,
					mark:     0x1003,
				},
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V6,
					table:    1010,
					mark:     0x1003,
				},
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V4,
					table:    1010,
					dst:      netip.MustParsePrefix("169.254.0.16/32"),
				},
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V6,
					table:    1010,
					dst:      netip.MustParsePrefix("fd69::10/128"),
				},
			},
			deleteRules: []testRule{
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V4,
					table:    1010,
					dst:      netip.MustParsePrefix("100.128.0.0/16"),
				},
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V6,
					table:    1010,
					dst:      netip.MustParsePrefix("ae70::/60"),
				},
			},
			v4mode: true,
			v6mode: true,
		},
	}
	config.Gateway.V6MasqueradeSubnet = "fd69::/112"
	config.Gateway.V4MasqueradeSubnet = "169.254.0.0/16"
	for _, test := range tests {
		t.Run(test.desc, func(t *testing.T) {
			g := NewWithT(t)
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: nodeName,
				},
			}
			config.IPv4Mode = test.v4mode
			config.IPv6Mode = test.v6mode
			cidr := ""
			if config.IPv4Mode {
				cidr = "100.128.0.0/16/24"
			}
			if config.IPv4Mode && config.IPv6Mode {
				cidr += ",ae70::/60/64"
			} else if config.IPv6Mode {
				cidr = "ae70::/60/64"
			}
			nad := ovntest.GenerateNAD("bluenet", "rednad", "greenamespace",
				types.Layer3Topology, cidr, types.NetworkRolePrimary)
			ovntest.AnnotateNADWithNetworkID("3", nad)
			netInfo, err := util.ParseNADInfo(nad)
			g.Expect(err).NotTo(HaveOccurred())
			ofm := getDummyOpenflowManager()
			// create dummy gateway interface(Need to run this test as root)
			err = netlink.LinkAdd(&netlink.Dummy{
				LinkAttrs: netlink.LinkAttrs{
					Name: "breth0",
				},
			})
			g.Expect(err).NotTo(HaveOccurred())
			udnGateway, err := NewUserDefinedNetworkGateway(netInfo, node, emptyNodeInformer(), nil, nil, nil, &gateway{openflowManager: ofm}, nil, nil, nil)
			g.Expect(err).NotTo(HaveOccurred())
			// delete dummy gateway interface after creating UDN gateway(Need to run this test as root)
			err = netlink.LinkDel(&netlink.Dummy{
				LinkAttrs: netlink.LinkAttrs{
					Name: "breth0",
				},
			})
			g.Expect(err).NotTo(HaveOccurred())
			udnGateway.vrfTableId = test.vrftableID
			rules, delRules, err := udnGateway.constructUDNVRFIPRules()
			g.Expect(err).ToNot(HaveOccurred())
			for i, rule := range rules {
				g.Expect(rule.Priority).To(Equal(test.expectedRules[i].priority))
				g.Expect(rule.Table).To(Equal(test.expectedRules[i].table))
				g.Expect(rule.Family).To(Equal(test.expectedRules[i].family))
				if rule.Dst.IsValid() {
					g.Expect(rule.Dst).To(Equal(test.expectedRules[i].dst))
				} else {
					g.Expect(rule.Mark).To(Equal(test.expectedRules[i].mark))
				}
			}
			for i, rule := range delRules {
				g.Expect(rule.Priority).To(Equal(test.deleteRules[i].priority))
				g.Expect(rule.Table).To(Equal(test.deleteRules[i].table))
				g.Expect(rule.Family).To(Equal(test.deleteRules[i].family))
				g.Expect(rule.Dst).To(Equal(test.deleteRules[i].dst))
			}
		})
	}
}

func TestConstructUDNVRFIPRulesPodNetworkAdvertisedToDefaultVRF(t *testing.T) {
	if ovntest.NoRoot() {
		t.Skip("Test requires root privileges")
	}
	type testRule struct {
		priority int
		family   int
		table    int
		mark     uint32
		dst      netip.Prefix
	}
	type testConfig struct {
		desc          string
		vrftableID    int
		v4mode        bool
		v6mode        bool
		expectedRules []testRule
		deleteRules   []testRule
	}

	tests := []testConfig{
		{
			desc:       "v4 rule test",
			vrftableID: 1007,
			expectedRules: []testRule{
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V4,
					table:    1007,
					mark:     0x1003,
				},
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V4,
					table:    1007,
					dst:      netip.MustParsePrefix("169.254.0.16/32"),
				},
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V4,
					table:    1007,
					dst:      netip.MustParsePrefix("100.128.0.0/16"),
				},
			},
			v4mode: true,
		},
		{
			desc:       "v6 rule test",
			vrftableID: 1009,
			expectedRules: []testRule{
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V6,
					table:    1009,
					mark:     0x1003,
				},
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V6,
					table:    1009,
					dst:      netip.MustParsePrefix("fd69::10/128"),
				},
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V6,
					table:    1009,
					dst:      netip.MustParsePrefix("ae70::/60"),
				},
			},
			v6mode: true,
		},
		{
			desc:       "dualstack rule test",
			vrftableID: 1010,
			expectedRules: []testRule{
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V4,
					table:    1010,
					mark:     0x1003,
				},
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V6,
					table:    1010,
					mark:     0x1003,
				},
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V4,
					table:    1010,
					dst:      netip.MustParsePrefix("169.254.0.16/32"),
				},
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V6,
					table:    1010,
					dst:      netip.MustParsePrefix("fd69::10/128"),
				},
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V4,
					table:    1010,
					dst:      netip.MustParsePrefix("100.128.0.0/16"),
				},
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V6,
					table:    1010,
					dst:      netip.MustParsePrefix("ae70::/60"),
				},
			},
			v4mode: true,
			v6mode: true,
		},
	}
	config.Gateway.V6MasqueradeSubnet = "fd69::/112"
	config.Gateway.V4MasqueradeSubnet = "169.254.0.0/16"
	for _, test := range tests {
		t.Run(test.desc, func(t *testing.T) {
			g := NewWithT(t)
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: nodeName,
				},
			}
			config.IPv4Mode = test.v4mode
			config.IPv6Mode = test.v6mode
			cidr := ""
			if config.IPv4Mode {
				cidr = "100.128.0.0/16/24"
			}
			if config.IPv4Mode && config.IPv6Mode {
				cidr += ",ae70::/60/64"
			} else if config.IPv6Mode {
				cidr = "ae70::/60/64"
			}
			nad := ovntest.GenerateNAD("bluenet", "rednad", "greenamespace",
				types.Layer3Topology, cidr, types.NetworkRolePrimary)
			ovntest.AnnotateNADWithNetworkID("3", nad)
			netInfo, err := util.ParseNADInfo(nad)
			g.Expect(err).ToNot(HaveOccurred())
			mutableNetInfo := util.NewMutableNetInfo(netInfo)
			mutableNetInfo.SetPodNetworkAdvertisedVRFs(map[string][]string{node.Name: {"bluenet"}})
			ofm := getDummyOpenflowManager()
			// create dummy gateway interface(Need to run this test as root)
			err = netlink.LinkAdd(&netlink.Dummy{
				LinkAttrs: netlink.LinkAttrs{
					Name: "breth0",
				},
			})
			g.Expect(err).NotTo(HaveOccurred())
			udnGateway, err := NewUserDefinedNetworkGateway(mutableNetInfo, node, emptyNodeInformer(), nil, nil, nil, &gateway{openflowManager: ofm}, nil, nil, nil)
			g.Expect(err).NotTo(HaveOccurred())
			// delete dummy gateway interface after creating UDN gateway(Need to run this test as root)
			err = netlink.LinkDel(&netlink.Dummy{
				LinkAttrs: netlink.LinkAttrs{
					Name: "breth0",
				},
			})
			g.Expect(err).NotTo(HaveOccurred())
			udnGateway.vrfTableId = test.vrftableID
			udnGateway.isNetworkAdvertised = true
			udnGateway.isNetworkAdvertisedToDefaultVRF = true
			rules, delRules, err := udnGateway.constructUDNVRFIPRules()
			g.Expect(err).ToNot(HaveOccurred())
			for i, rule := range rules {
				g.Expect(rule.Priority).To(Equal(test.expectedRules[i].priority))
				g.Expect(rule.Table).To(Equal(test.expectedRules[i].table))
				g.Expect(rule.Family).To(Equal(test.expectedRules[i].family))
				if rule.Dst.IsValid() {
					g.Expect(rule.Dst).To(Equal(test.expectedRules[i].dst))
				} else {
					g.Expect(rule.Mark).To(Equal(test.expectedRules[i].mark))
				}
			}
			for i, rule := range delRules {
				g.Expect(rule.Priority).To(Equal(test.deleteRules[i].priority))
				g.Expect(rule.Table).To(Equal(test.deleteRules[i].table))
				g.Expect(rule.Family).To(Equal(test.deleteRules[i].family))
				g.Expect(rule.Dst).To(Equal(test.deleteRules[i].dst))
			}
		})
	}
}

func TestConstructUDNVRFIPRulesPodNetworkAdvertisedToNonDefaultVRF(t *testing.T) {
	if ovntest.NoRoot() {
		t.Skip("Test requires root privileges")
	}
	type testRule struct {
		priority int
		family   int
		table    int
		mark     uint32
		dst      netip.Prefix
	}
	type testConfig struct {
		desc          string
		vrftableID    int
		v4mode        bool
		v6mode        bool
		expectedRules []testRule
		deleteRules   []testRule
	}

	tests := []testConfig{
		{
			desc:       "v4 rule test",
			vrftableID: 1007,
			expectedRules: []testRule{
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V4,
					table:    1007,
					mark:     0x1003,
				},
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V4,
					table:    1007,
					dst:      netip.MustParsePrefix("169.254.0.16/32"),
				},
			},
			deleteRules: []testRule{
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V4,
					table:    1007,
					dst:      netip.MustParsePrefix("100.128.0.0/16"),
				},
			},
			v4mode: true,
		},
		{
			desc:       "v6 rule test",
			vrftableID: 1009,
			expectedRules: []testRule{
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V6,
					table:    1009,
					mark:     0x1003,
				},
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V6,
					table:    1009,
					dst:      netip.MustParsePrefix("fd69::10/128"),
				},
			},
			deleteRules: []testRule{
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V6,
					table:    1009,
					dst:      netip.MustParsePrefix("ae70::/60"),
				},
			},
			v6mode: true,
		},
		{
			desc:       "dualstack rule test",
			vrftableID: 1010,
			expectedRules: []testRule{
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V4,
					table:    1010,
					mark:     0x1003,
				},
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V6,
					table:    1010,
					mark:     0x1003,
				},
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V4,
					table:    1010,
					dst:      netip.MustParsePrefix("169.254.0.16/32"),
				},
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V6,
					table:    1010,
					dst:      netip.MustParsePrefix("fd69::10/128"),
				},
			},
			deleteRules: []testRule{
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V4,
					table:    1010,
					dst:      netip.MustParsePrefix("100.128.0.0/16"),
				},
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V6,
					table:    1010,
					dst:      netip.MustParsePrefix("ae70::/60"),
				},
			},
			v4mode: true,
			v6mode: true,
		},
	}
	config.Gateway.V6MasqueradeSubnet = "fd69::/112"
	config.Gateway.V4MasqueradeSubnet = "169.254.0.0/16"
	for _, test := range tests {
		t.Run(test.desc, func(t *testing.T) {
			g := NewWithT(t)
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: nodeName,
				},
			}
			config.IPv4Mode = test.v4mode
			config.IPv6Mode = test.v6mode
			cidr := ""
			if config.IPv4Mode {
				cidr = "100.128.0.0/16/24"
			}
			if config.IPv4Mode && config.IPv6Mode {
				cidr += ",ae70::/60"
			} else if config.IPv6Mode {
				cidr = "ae70::/60"
			}
			nad := ovntest.GenerateNAD("bluenet", "rednad", "greenamespace",
				types.Layer3Topology, cidr, types.NetworkRolePrimary)
			ovntest.AnnotateNADWithNetworkID("3", nad)
			netInfo, err := util.ParseNADInfo(nad)
			g.Expect(err).ToNot(HaveOccurred())
			mutableNetInfo := util.NewMutableNetInfo(netInfo)
			mutableNetInfo.SetPodNetworkAdvertisedVRFs(map[string][]string{node.Name: {"bluenet"}})
			ofm := getDummyOpenflowManager()
			// create dummy gateway interface(Need to run this test as root)
			err = netlink.LinkAdd(&netlink.Dummy{
				LinkAttrs: netlink.LinkAttrs{
					Name: "breth0",
				},
			})
			g.Expect(err).NotTo(HaveOccurred())
			udnGateway, err := NewUserDefinedNetworkGateway(mutableNetInfo, node, emptyNodeInformer(), nil, nil, nil, &gateway{openflowManager: ofm}, nil, nil, nil)
			g.Expect(err).NotTo(HaveOccurred())
			// delete dummy gateway interface after creating UDN gateway(Need to run this test as root)
			err = netlink.LinkDel(&netlink.Dummy{
				LinkAttrs: netlink.LinkAttrs{
					Name: "breth0",
				},
			})
			g.Expect(err).NotTo(HaveOccurred())
			udnGateway.vrfTableId = test.vrftableID
			udnGateway.isNetworkAdvertised = true
			udnGateway.isNetworkAdvertisedToDefaultVRF = false
			rules, delRules, err := udnGateway.constructUDNVRFIPRules()
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(rules).To(HaveLen(len(test.expectedRules)))
			g.Expect(delRules).To(HaveLen(len(test.deleteRules)))
			for i, rule := range rules {
				g.Expect(rule.Priority).To(Equal(test.expectedRules[i].priority))
				g.Expect(rule.Table).To(Equal(test.expectedRules[i].table))
				g.Expect(rule.Family).To(Equal(test.expectedRules[i].family))
				if rule.Dst.IsValid() {
					g.Expect(rule.Dst).To(Equal(test.expectedRules[i].dst))
				} else {
					g.Expect(rule.Mark).To(Equal(test.expectedRules[i].mark))
				}
			}
			for i, rule := range delRules {
				g.Expect(rule.Priority).To(Equal(test.deleteRules[i].priority))
				g.Expect(rule.Table).To(Equal(test.deleteRules[i].table))
				g.Expect(rule.Family).To(Equal(test.deleteRules[i].family))
				g.Expect(rule.Dst).To(Equal(test.deleteRules[i].dst))
			}
		})
	}
}

func TestUserDefinedNetworkGateway_updateAdvertisedUDNIsolationRules(t *testing.T) {
	tests := []struct {
		name                string
		nad                 *nadapi.NetworkAttachmentDefinition
		isNetworkAdvertised bool
		initialElements     []*knftables.Element
		expectedV4Elements  []*knftables.Element
		expectedV6Elements  []*knftables.Element
	}{
		{
			name: "Should add V4 and V6 entries to the set for advertised L3 network",
			nad: ovntest.GenerateNAD("test", "rednad", "greenamespace",
				types.Layer3Topology, "100.128.0.0/16/24,ae70::/60/64", types.NetworkRolePrimary),
			isNetworkAdvertised: true,
			expectedV4Elements: []*knftables.Element{{
				Set:     nftablesAdvertisedUDNsSetV4,
				Key:     []string{"100.128.0.0/16"},
				Comment: knftables.PtrTo[string]("test"),
			}},
			expectedV6Elements: []*knftables.Element{{
				Set:     nftablesAdvertisedUDNsSetV6,
				Key:     []string{"ae70::/60"},
				Comment: knftables.PtrTo[string]("test"),
			}},
		},
		{
			name: "Should add V4 and V6 entries to the set for advertised L2 network",
			nad: ovntest.GenerateNAD("test", "rednad", "greenamespace",
				types.Layer2Topology, "100.128.0.0/16,ae70::/60", types.NetworkRolePrimary),
			isNetworkAdvertised: true,
			expectedV4Elements: []*knftables.Element{{
				Set:     nftablesAdvertisedUDNsSetV4,
				Key:     []string{"100.128.0.0/16"},
				Comment: knftables.PtrTo[string]("test"),
			}},
			expectedV6Elements: []*knftables.Element{{
				Set:     nftablesAdvertisedUDNsSetV6,
				Key:     []string{"ae70::/60"},
				Comment: knftables.PtrTo[string]("test"),
			}},
		},
		{
			name: "Should not add duplicate V4 and V6 entries to the for advertised network",
			nad: ovntest.GenerateNAD("test", "rednad", "greenamespace",
				types.Layer3Topology, "100.128.0.0/16/24,ae70::/60/64", types.NetworkRolePrimary),
			isNetworkAdvertised: true,
			initialElements: []*knftables.Element{
				{
					Set:     nftablesAdvertisedUDNsSetV4,
					Key:     []string{"100.128.0.0/16"},
					Comment: knftables.PtrTo[string]("test"),
				}, {
					Set:     nftablesAdvertisedUDNsSetV6,
					Key:     []string{"ae70::/60"},
					Comment: knftables.PtrTo[string]("test"),
				},
			},
			expectedV4Elements: []*knftables.Element{{
				Set:     nftablesAdvertisedUDNsSetV4,
				Key:     []string{"100.128.0.0/16"},
				Comment: knftables.PtrTo[string]("test"),
			}},
			expectedV6Elements: []*knftables.Element{{
				Set:     nftablesAdvertisedUDNsSetV6,
				Key:     []string{"ae70::/60"},
				Comment: knftables.PtrTo[string]("test"),
			}},
		},
		{
			name: "Should remove V4 and V6 entries from the set when network for not advertised network",
			nad: ovntest.GenerateNAD("test", "rednad", "greenamespace",
				types.Layer3Topology, "100.128.0.0/16/24,ae70::/60/64", types.NetworkRolePrimary),
			isNetworkAdvertised: false,
			initialElements: []*knftables.Element{
				{
					Set:     nftablesAdvertisedUDNsSetV4,
					Key:     []string{"100.128.0.0/16"},
					Comment: knftables.PtrTo[string]("test"),
				}, {
					Set:     nftablesAdvertisedUDNsSetV6,
					Key:     []string{"ae70::/60"},
					Comment: knftables.PtrTo[string]("test"),
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			nft := nodenft.SetFakeNFTablesHelper()
			config.IPv4Mode = true
			config.IPv6Mode = true

			netInfo, err := util.ParseNADInfo(tt.nad)
			g.Expect(err).NotTo(HaveOccurred())

			err = configureAdvertisedUDNIsolationNFTables()
			g.Expect(err).ToNot(HaveOccurred())
			tx := nft.NewTransaction()
			for _, element := range tt.initialElements {
				tx.Add(element)
			}
			if tx.NumOperations() > 0 {
				err = nft.Run(context.TODO(), tx)
				g.Expect(err).NotTo(HaveOccurred())
			}
			udng := &UserDefinedNetworkGateway{
				NetInfo: netInfo,
			}
			udng.isNetworkAdvertised = tt.isNetworkAdvertised
			err = udng.updateAdvertisedUDNIsolationRules()
			g.Expect(err).NotTo(HaveOccurred())

			v4Elems, err := nft.ListElements(context.TODO(), "set", nftablesAdvertisedUDNsSetV4)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(v4Elems).To(HaveLen(len(tt.expectedV4Elements)))

			v6Elems, err := nft.ListElements(context.TODO(), "set", nftablesAdvertisedUDNsSetV6)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(v6Elems).To(HaveLen(len(tt.expectedV6Elements)))

			for i, element := range tt.expectedV4Elements {
				g.Expect(element.Key).To(HaveLen(len(v4Elems[i].Key)))
				g.Expect(element.Key[0]).To(BeEquivalentTo(v4Elems[i].Key[0]))
				g.Expect(element.Comment).To(BeEquivalentTo(v4Elems[i].Comment))
			}
			for i, element := range tt.expectedV6Elements {
				g.Expect(element.Key).To(HaveLen(len(v6Elems[i].Key)))
				g.Expect(element.Key[0]).To(BeEquivalentTo(v6Elems[i].Key[0]))
				g.Expect(element.Comment).To(BeEquivalentTo(v6Elems[i].Comment))
			}
		})
	}
}

func TestAddUDNMasqIPNeighbors(t *testing.T) {
	bridgeName := "breth0"
	linkIndex := 7
	link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: bridgeName, Index: linkIndex}}

	v4Masq := &udn.MasqueradeIPs{GatewayRouter: ovntest.MustParseIPNet("169.254.0.11/16")}
	v6Masq := &udn.MasqueradeIPs{GatewayRouter: ovntest.MustParseIPNet("fd69::b/112")}
	v4IP := net.ParseIP("169.254.0.11")
	v6IP := net.ParseIP("fd69::b")
	v4MAC := util.IPAddrToHWAddr(v4IP)
	v6MAC := util.IPAddrToHWAddr(v6IP)

	tests := []struct {
		name    string
		v4      *udn.MasqueradeIPs
		v6      *udn.MasqueradeIPs
		setup   func(m *utilmocks.NetLinkOps)
		wantErr string
	}{
		{
			name: "link lookup fails",
			v4:   v4Masq, v6: v6Masq,
			setup: func(m *utilmocks.NetLinkOps) {
				m.On("LinkByName", bridgeName).Return(nil, fmt.Errorf("no such device"))
			},
			wantErr: "unable to get link for breth0",
		},
		{
			name: "dual-stack adds both entries when none exist",
			v4:   v4Masq, v6: v6Masq,
			setup: func(m *utilmocks.NetLinkOps) {
				m.On("LinkByName", bridgeName).Return(link, nil)
				m.On("LinkSetUp", link).Return(nil)
				m.On("NeighList", linkIndex, netlink.FAMILY_V4).Return([]netlink.Neigh{}, nil)
				m.On("NeighDel", mock.MatchedBy(func(n *netlink.Neigh) bool {
					return n.IP.Equal(v4IP)
				})).Return(nil).Once()
				m.On("NeighSet", mock.MatchedBy(func(n *netlink.Neigh) bool {
					return n.IP.Equal(v4IP) && n.LinkIndex == linkIndex &&
						n.HardwareAddr.String() == v4MAC.String() &&
						n.State == netlink.NUD_PERMANENT
				})).Return(nil).Once()
				m.On("NeighList", linkIndex, netlink.FAMILY_V6).Return([]netlink.Neigh{}, nil)
				m.On("NeighDel", mock.MatchedBy(func(n *netlink.Neigh) bool {
					return n.IP.Equal(v6IP)
				})).Return(nil).Once()
				m.On("NeighSet", mock.MatchedBy(func(n *netlink.Neigh) bool {
					return n.IP.Equal(v6IP) && n.LinkIndex == linkIndex &&
						n.HardwareAddr.String() == v6MAC.String() &&
						n.State == netlink.NUD_PERMANENT
				})).Return(nil).Once()
			},
		},
		{
			name: "IPv4-only single-stack adds only v4 entry",
			v4:   v4Masq, v6: nil,
			setup: func(m *utilmocks.NetLinkOps) {
				m.On("LinkByName", bridgeName).Return(link, nil)
				m.On("LinkSetUp", link).Return(nil)
				m.On("NeighList", linkIndex, netlink.FAMILY_V4).Return([]netlink.Neigh{}, nil)
				m.On("NeighDel", mock.MatchedBy(func(n *netlink.Neigh) bool {
					return n.IP.Equal(v4IP)
				})).Return(nil).Once()
				m.On("NeighSet", mock.MatchedBy(func(n *netlink.Neigh) bool {
					return n.IP.Equal(v4IP) && n.State == netlink.NUD_PERMANENT
				})).Return(nil).Once()
			},
		},
		{
			name: "IPv6-only single-stack adds only v6 entry",
			v4:   nil, v6: v6Masq,
			setup: func(m *utilmocks.NetLinkOps) {
				m.On("LinkByName", bridgeName).Return(link, nil)
				m.On("LinkSetUp", link).Return(nil)
				m.On("NeighList", linkIndex, netlink.FAMILY_V6).Return([]netlink.Neigh{}, nil)
				m.On("NeighDel", mock.MatchedBy(func(n *netlink.Neigh) bool {
					return n.IP.Equal(v6IP)
				})).Return(nil).Once()
				m.On("NeighSet", mock.MatchedBy(func(n *netlink.Neigh) bool {
					return n.IP.Equal(v6IP) && n.State == netlink.NUD_PERMANENT
				})).Return(nil).Once()
			},
		},
		{
			name: "skips when correct entries already exist",
			v4:   v4Masq, v6: v6Masq,
			setup: func(m *utilmocks.NetLinkOps) {
				m.On("LinkByName", bridgeName).Return(link, nil)
				m.On("LinkSetUp", link).Return(nil)
				// v4: correct entry exists
				m.On("NeighList", linkIndex, netlink.FAMILY_V4).Return([]netlink.Neigh{
					{IP: v4IP, HardwareAddr: v4MAC, State: netlink.NUD_PERMANENT, LinkIndex: linkIndex},
				}, nil)
				// v6: correct entry exists
				m.On("NeighList", linkIndex, netlink.FAMILY_V6).Return([]netlink.Neigh{
					{IP: v6IP, HardwareAddr: v6MAC, State: netlink.NUD_PERMANENT, LinkIndex: linkIndex},
				}, nil)
			},
		},
		{
			name: "replaces stale entry with wrong MAC",
			v4:   v4Masq, v6: v6Masq,
			setup: func(m *utilmocks.NetLinkOps) {
				m.On("LinkByName", bridgeName).Return(link, nil)
				m.On("LinkSetUp", link).Return(nil)
				staleMAC, _ := net.ParseMAC("aa:bb:cc:dd:ee:ff")
				// v4: stale entry with wrong MAC
				m.On("NeighList", linkIndex, netlink.FAMILY_V4).Return([]netlink.Neigh{
					{IP: v4IP, HardwareAddr: staleMAC, State: netlink.NUD_REACHABLE, LinkIndex: linkIndex},
				}, nil)
				m.On("NeighDel", mock.MatchedBy(func(n *netlink.Neigh) bool {
					return n.IP.Equal(v4IP)
				})).Return(nil).Once()
				m.On("NeighSet", mock.MatchedBy(func(n *netlink.Neigh) bool {
					return n.IP.Equal(v4IP) && n.HardwareAddr.String() == v4MAC.String()
				})).Return(nil).Once()
				// v6: no entry
				m.On("NeighList", linkIndex, netlink.FAMILY_V6).Return([]netlink.Neigh{}, nil)
				m.On("NeighDel", mock.MatchedBy(func(n *netlink.Neigh) bool {
					return n.IP.Equal(v6IP)
				})).Return(nil).Once()
				m.On("NeighSet", mock.MatchedBy(func(n *netlink.Neigh) bool {
					return n.IP.Equal(v6IP)
				})).Return(nil).Once()
			},
		},
		{
			name: "returns error when NeighSet fails",
			v4:   v4Masq, v6: v6Masq,
			setup: func(m *utilmocks.NetLinkOps) {
				m.On("LinkByName", bridgeName).Return(link, nil)
				m.On("LinkSetUp", link).Return(nil)
				m.On("NeighList", linkIndex, netlink.FAMILY_V4).Return([]netlink.Neigh{}, nil)
				m.On("NeighDel", mock.Anything).Return(nil)
				m.On("NeighSet", mock.Anything).Return(fmt.Errorf("permission denied"))
			},
			wantErr: "failed to add neighbor",
		},
		{
			name: "returns error when NeighList fails",
			v4:   v4Masq, v6: v6Masq,
			setup: func(m *utilmocks.NetLinkOps) {
				m.On("LinkByName", bridgeName).Return(link, nil)
				m.On("LinkSetUp", link).Return(nil)
				m.On("NeighList", linkIndex, netlink.FAMILY_V4).Return(nil, fmt.Errorf("netlink error"))
			},
			wantErr: "failed to check neighbor",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nlMock := new(utilmocks.NetLinkOps)
			origOps := util.GetNetLinkOps()
			util.SetNetLinkOpMockInst(nlMock)
			t.Cleanup(func() { util.SetNetLinkOpMockInst(origOps) })

			tt.setup(nlMock)

			err := addUDNMasqIPNeighbors(bridgeName, tt.v4, tt.v6)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("expected error containing %q, got %v", tt.wantErr, err)
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			nlMock.AssertExpectations(t)
		})
	}
}

func TestDelUDNMasqIPNeighbors(t *testing.T) {
	bridgeName := "breth0"
	linkIndex := 7
	link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: bridgeName, Index: linkIndex}}

	v4Masq := &udn.MasqueradeIPs{GatewayRouter: ovntest.MustParseIPNet("169.254.0.11/16")}
	v6Masq := &udn.MasqueradeIPs{GatewayRouter: ovntest.MustParseIPNet("fd69::b/112")}
	v4IP := net.ParseIP("169.254.0.11")
	v6IP := net.ParseIP("fd69::b")

	tests := []struct {
		name    string
		setup   func(m *utilmocks.NetLinkOps)
		wantErr string
	}{
		{
			name: "deletes both entries",
			setup: func(m *utilmocks.NetLinkOps) {
				m.On("LinkByName", bridgeName).Return(link, nil)
				m.On("LinkSetUp", link).Return(nil)
				m.On("NeighDel", mock.MatchedBy(func(n *netlink.Neigh) bool {
					return n.IP.Equal(v4IP) && n.LinkIndex == linkIndex
				})).Return(nil).Once()
				m.On("NeighDel", mock.MatchedBy(func(n *netlink.Neigh) bool {
					return n.IP.Equal(v6IP) && n.LinkIndex == linkIndex
				})).Return(nil).Once()
			},
		},
		{
			name: "returns error on link lookup failure",
			setup: func(m *utilmocks.NetLinkOps) {
				m.On("LinkByName", bridgeName).Return(nil, fmt.Errorf("no such device"))
			},
			wantErr: "unable to get link for breth0",
		},
		{
			name: "ignores ENOENT (already deleted)",
			setup: func(m *utilmocks.NetLinkOps) {
				m.On("LinkByName", bridgeName).Return(link, nil)
				m.On("LinkSetUp", link).Return(nil)
				m.On("NeighDel", mock.MatchedBy(func(n *netlink.Neigh) bool {
					return n.IP.Equal(v4IP)
				})).Return(fmt.Errorf("failed: %w", syscall.ENOENT)).Once()
				m.On("NeighDel", mock.MatchedBy(func(n *netlink.Neigh) bool {
					return n.IP.Equal(v6IP)
				})).Return(fmt.Errorf("failed: %w", syscall.ENOENT)).Once()
			},
		},
		{
			name: "returns error on non-ENOENT NeighDel failure",
			setup: func(m *utilmocks.NetLinkOps) {
				m.On("LinkByName", bridgeName).Return(link, nil)
				m.On("LinkSetUp", link).Return(nil)
				m.On("NeighDel", mock.MatchedBy(func(n *netlink.Neigh) bool {
					return n.IP.Equal(v4IP)
				})).Return(fmt.Errorf("permission denied")).Once()
				m.On("NeighDel", mock.MatchedBy(func(n *netlink.Neigh) bool {
					return n.IP.Equal(v6IP)
				})).Return(nil).Once()
			},
			wantErr: "failed to delete neighbor",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nlMock := new(utilmocks.NetLinkOps)
			origOps := util.GetNetLinkOps()
			util.SetNetLinkOpMockInst(nlMock)
			t.Cleanup(func() { util.SetNetLinkOpMockInst(origOps) })

			tt.setup(nlMock)

			err := delUDNMasqIPNeighbors(bridgeName, v4Masq, v6Masq)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("expected error containing %q, got %v", tt.wantErr, err)
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			nlMock.AssertExpectations(t)
		})
	}
}
