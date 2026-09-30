// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package node

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"reflect"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	coreinformers "k8s.io/client-go/informers/core/v1"
	listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/knftables"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	controllerutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	uplinkv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1"
	uplinkinformers "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/informers/externalversions/uplink/v1alpha1"
	uplinklisters "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/listers/uplink/v1alpha1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/generator/udn"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/bridgeconfig"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/iprulemanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/managementport"
	nodenft "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/nftables"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/vrfmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	uplinkutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/uplink"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	utilerrors "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/errors"
)

const (
	// ctMarkUDNBase is the conntrack mark base value for user defined networks to use
	// Each network gets its own mark == base + network-id
	ctMarkUDNBase = 3
	// pktMarkBase is the base value for packet mark assigned to user defined networks
	// Each network has a packet mark equal to base + network-id
	pktMarkBase = 4096
	// waitForPatchPortTimeout is the maximum time we wait for a UDN's patch
	// port to be created by OVN.
	waitForPatchPortTimeout = 30 * time.Second
	// UDNMasqueradeIPRulePriority the priority of the ip routing rules created for masquerade IP address
	// allocated for every user defined network.
	UDNMasqueradeIPRulePriority = 2000
	// dpuUDNVRFRouteTableIDStart is a distinct table range for UDN VRFs
	// created on DPUs. DPU mode has no host management interface to derive a
	// table from, so this avoids colliding with link-index based tables.
	dpuUDNVRFRouteTableIDStart = 100000
)

// UserDefinedNetworkGateway contains information
// required to program a UDN at each node's
// gateway.
// NOTE: Currently invoked only for primary networks.
type UserDefinedNetworkGateway struct {
	// network information
	util.NetInfo
	// node that its programming things on
	node          *corev1.Node
	nodeInformer  coreinformers.NodeInformer
	nodeLister    listers.NodeLister
	kubeInterface kube.Interface
	// vrf manager that creates and manages vrfs for all UDNs
	// used with a lock since its shared between all network controllers
	vrfManager *vrfmanager.Controller
	// masqCTMark holds the mark value for this network
	// which is used for egress traffic in shared gateway mode
	masqCTMark uint
	// pktMark hold the packets mark value for this network
	// which is used for directing traffic towards the UDN
	pktMark uint
	// v4MasqIPs holds the IPv4 masquerade IPs for this network
	v4MasqIPs *udn.MasqueradeIPs
	// v6MasqIPs holds the IPv6 masquerade IPs for this network
	v6MasqIPs *udn.MasqueradeIPs
	// stores the pointer to default network's gateway so that
	// we can leverage it from here to program UDN flows on breth0
	// Currently we use the openflowmanager and nodeIPManager from
	// gateway, but maybe we could invoke our own instance of these
	// for UDNs in the future. For now default network and UDNs will
	// use the same gateway struct instance
	*gateway
	// iprules manager that creates and manages iprules for
	// all UDNs. Must be accessed with a lock
	ruleManager iprulemanager.Interface

	// management port controller
	mgmtPortController *managementport.UDNManagementPortController

	ovsClient         libovsdbclient.Client
	uplinkStateLister uplinklisters.UplinkStateLister
	// uplinkStateController is this CUDN's level-driven Uplink reconciler. It
	// retries transient failures and owns its informer handler lifecycle.
	uplinkStateController              controllerutil.Controller
	uplinkStateGatewayStatusController *UplinkStateGatewayStatusController
	// openflowBridgeName selects the openflow manager target for this gateway.
	// defaultOpenFlowBridgeSetName means the default bridge plus external gateway bridge.
	openflowBridgeName string
	// nextHops holds the default route next hops for this UDN gateway.
	nextHops []net.IP

	// gatewayReconciler is fed advertised-state changes by the network manager.
	// It coalesces events and retries until the latest configuration converges.
	gatewayReconciler controllerutil.Reconciler

	// operationMutex serializes initial setup, Uplink transitions, ordinary
	// UDN reconciliation, and terminal cleanup for this network. Shared
	// managers provide their own narrower synchronization across networks.
	operationMutex sync.Mutex

	// vrfTableId holds the route table ID corresponding to management port interface of the network
	vrfTableId int

	// gwInterfaceName holds the gateway interface name for this network.
	gwInterfaceName string
	// gwInterfaceIndex holds the link index of the gateway interface for this network.
	gwInterfaceIndex int
	// uplinkGatewayCleanupRequired records that this gateway may own complete
	// or partial dataplane programming that delNetwork must remove before this
	// reusable gateway object can consume another resolved Uplink lifecycle.
	uplinkGatewayCleanupRequired bool
	// uplinkStateUID and uplinkFingerprint identify the UplinkState object and
	// inputs associated with fully programmed dataplane state. They are recorded
	// only after setup succeeds so retries never mistake partial programming for
	// an unchanged, ready gateway, and kept across a failed teardown so that
	// its failure is reported against the UplinkState this gateway handled.
	uplinkStateUID    k8stypes.UID
	uplinkFingerprint uplinkGatewayFingerprint
	// uplinkGatewayPartial records that the dataplane no longer matches
	// uplinkFingerprint, after a failed teardown or in-place update: the next
	// reconcile rebuilds the gateway instead of matching the fingerprint.
	uplinkGatewayPartial bool

	// nodeSubnetBridge is the shared gateway bridge (br-ex) whose subnet(s) the
	// node-connected subnet routes are derived from. Its IPs are read fresh on
	// every (re)configuration so that runtime gateway address changes are
	// reflected in the VRF routes. It is nil for Uplink-backed networks, which
	// do not get these routes: the Uplink's own connected prefix route is
	// already enslaved into their VRF by the kernel (see addNetworkWithResolvedUplink).
	nodeSubnetBridge *bridgeconfig.BridgeConfiguration
	// nodeSubnetRoutes tracks the node-connected subnet routes last programmed
	// into this network's VRF, so that a subsequent address change can replace
	// stale routes rather than leaking them.
	nodeSubnetRoutes []netlink.Route

	// save BGP state at the start of reconciliation loop run to handle it consistently throughout the run
	isNetworkAdvertisedToDefaultVRF bool
	isNetworkAdvertised             bool
}

func NewUserDefinedNetworkGateway(netInfo util.NetInfo, node *corev1.Node, nodeInformer coreinformers.NodeInformer,
	kubeInterface kube.Interface, vrfManager *vrfmanager.Controller, ruleManager iprulemanager.Interface,
	defaultNetworkGateway Gateway, ovsClient libovsdbclient.Client, uplinkStateInformer uplinkinformers.UplinkStateInformer,
	uplinkStateGatewayStatusController *UplinkStateGatewayStatusController) (*UserDefinedNetworkGateway, error) {
	if nodeInformer == nil {
		return nil, fmt.Errorf("node informer has not been provided for network %s", netInfo.GetNetworkName())
	}
	if netInfo.Uplink() != "" {
		if uplinkStateInformer == nil {
			return nil, fmt.Errorf("UplinkState informer has not been provided for network %s", netInfo.GetNetworkName())
		}
		if uplinkStateGatewayStatusController == nil {
			return nil, fmt.Errorf("UplinkState gateway status controller has not been provided for network %s", netInfo.GetNetworkName())
		}
	}

	// Generate a per network conntrack mark and masquerade IPs to be used for egress traffic.
	var (
		v4MasqIPs *udn.MasqueradeIPs
		v6MasqIPs *udn.MasqueradeIPs
		err       error
	)
	networkID := netInfo.GetNetworkID()
	masqCTMark := ctMarkUDNBase + uint(networkID)
	pktMark := pktMarkBase + uint(networkID)
	if config.IPv4Mode {
		v4MasqIPs, err = udn.AllocateV4MasqueradeIPs(networkID)
		if err != nil {
			return nil, fmt.Errorf("failed to get v4 masquerade IP, network %s (%d): %v", netInfo.GetNetworkName(), networkID, err)
		}
	}
	if config.IPv6Mode {
		v6MasqIPs, err = udn.AllocateV6MasqueradeIPs(networkID)
		if err != nil {
			return nil, fmt.Errorf("failed to get v6 masquerade IP, network %s (%d): %v", netInfo.GetNetworkName(), networkID, err)
		}
	}
	gw, ok := defaultNetworkGateway.(*gateway)
	if !ok {
		return nil, fmt.Errorf("unable to dereference default node network controller gateway object")
	}

	if (config.IsModeDPU() || config.IsModeFull()) && gw.openflowManager == nil {
		return nil, fmt.Errorf("openflow manager has not been provided for network: %s", netInfo.GetNetworkName())
	}

	intfName := gw.GetGatewayIface()
	if intfName == "" {
		return nil, fmt.Errorf("failed to get gateway interface for network: %s", netInfo.GetNetworkName())
	}
	link, err := util.GetNetLinkOps().LinkByName(intfName)
	if err != nil {
		return nil, fmt.Errorf("unable to get link for gateway interface %s, error: %v", intfName, err)
	}
	gwInterfaceName := intfName
	gwInterfaceIndex := link.Attrs().Index
	if netInfo.Uplink() != "" {
		gwInterfaceName = ""
		gwInterfaceIndex = 0
	}

	var uplinkStateLister uplinklisters.UplinkStateLister
	if uplinkStateInformer != nil {
		uplinkStateLister = uplinkStateInformer.Lister()
	}
	udng := &UserDefinedNetworkGateway{
		NetInfo:                            netInfo,
		node:                               node,
		nodeInformer:                       nodeInformer,
		nodeLister:                         nodeInformer.Lister(),
		kubeInterface:                      kubeInterface,
		vrfManager:                         vrfManager,
		masqCTMark:                         masqCTMark,
		pktMark:                            pktMark,
		v4MasqIPs:                          v4MasqIPs,
		v6MasqIPs:                          v6MasqIPs,
		gateway:                            gw,
		ruleManager:                        ruleManager,
		ovsClient:                          ovsClient,
		uplinkStateLister:                  uplinkStateLister,
		uplinkStateGatewayStatusController: uplinkStateGatewayStatusController,
		openflowBridgeName:                 defaultOpenFlowBridgeSetName,
		nextHops:                           gw.nextHops,
		gwInterfaceName:                    gwInterfaceName,
		gwInterfaceIndex:                   gwInterfaceIndex,
	}
	udng.gatewayReconciler = controllerutil.NewReconciler(
		fmt.Sprintf("udn-%s-gateway-reconciler", udng.GetNetworkName()),
		&controllerutil.ReconcilerConfig{
			MaxAttempts: controllerutil.InfiniteAttempts,
			Reconcile:   udng.reconcileGateway,
			Threadiness: 1,
		},
	)
	if netInfo.Uplink() != "" {
		uplinkStateConfig := &controllerutil.ControllerConfig[uplinkv1alpha1.UplinkState]{
			MaxAttempts:    controllerutil.InfiniteAttempts,
			Informer:       uplinkStateInformer.Informer(),
			Reconcile:      udng.reconcileUplinkState,
			ObjNeedsUpdate: udng.uplinkStateNeedsUpdate,
			Threadiness:    1,
		}
		udng.uplinkStateController = controllerutil.NewController(
			fmt.Sprintf("udn-%s-uplink-state-controller", udng.GetNetworkName()),
			uplinkStateConfig,
		)
	}
	return udng, nil
}

type resolvedUplinkGateway struct {
	hostInterfaceName string
	bridgeName        string
	macAddress        net.HardwareAddr
	ipAddresses       []*net.IPNet
	defaultGateways   []net.IP
	// hostFunction identifies the host PCI function backing the host
	// interface; nil when the DPU-host could not resolve it.
	hostFunction *uplinkv1alpha1.HostFunction
}

// uplinkGatewayFingerprint is comparable and identifies every UplinkState
// configuration input consumed while programming a UDN gateway.
type uplinkGatewayFingerprint struct {
	hostInterfaceName string
	bridgeName        string
	macAddress        string
	ipAddresses       string
	defaultGateways   string
	hasHostFunction   bool
	hostPFID          int32
	hasHostVF         bool
	hostVFID          int32
}

// uplinkGatewayFingerprintFromState can identify invalid or unresolved input,
// allowing a failed operation to report its observed configuration without
// recording that configuration as successfully programmed.
func uplinkGatewayFingerprintFromState(
	state *uplinkv1alpha1.UplinkState,
) uplinkGatewayFingerprint {
	ipAddresses := make([]string, 0, len(state.Status.IPAddresses))
	for _, ipAddress := range state.Status.IPAddresses {
		ipAddresses = append(ipAddresses, string(ipAddress))
	}
	defaultGateways := make([]string, 0, len(state.Status.DefaultGateways))
	for _, defaultGateway := range state.Status.DefaultGateways {
		defaultGateways = append(defaultGateways, string(defaultGateway))
	}
	sort.Strings(ipAddresses)
	sort.Strings(defaultGateways)
	bridgeName := ""
	if state.Status.OVSBridge != nil {
		bridgeName = state.Status.OVSBridge.Name
	}
	fingerprint := uplinkGatewayFingerprint{
		hostInterfaceName: string(state.Status.HostInterfaceName),
		bridgeName:        bridgeName,
		macAddress:        string(state.Status.MACAddress),
		ipAddresses:       strings.Join(ipAddresses, ","),
		defaultGateways:   strings.Join(defaultGateways, ","),
	}
	if state.Status.HostFunction != nil {
		fingerprint.hasHostFunction = true
		fingerprint.hostPFID = state.Status.HostFunction.PFID
		if state.Status.HostFunction.VFID != nil {
			fingerprint.hasHostVF = true
			fingerprint.hostVFID = *state.Status.HostFunction.VFID
		}
	}
	if config.IsModeDPUHost() {
		// The bridge and host function are discovered and consumed by the DPU.
		fingerprint.bridgeName = ""
		fingerprint.hasHostFunction = false
		fingerprint.hostPFID = 0
		fingerprint.hasHostVF = false
		fingerprint.hostVFID = 0
	}
	return fingerprint
}

func (udng *UserDefinedNetworkGateway) uplinkStateNeedsUpdate(
	oldState, newState *uplinkv1alpha1.UplinkState,
) bool {
	if newState == nil || newState.Name != uplinkutil.StateName(udng.Uplink(), udng.node.Name) {
		return false
	}
	return oldState == nil || !uplinkGatewayInputsEqual(oldState, newState)
}

// uplinkGatewayInputsEqual ignores the readiness conditions written by the
// gateway status controller, preventing its status update from feeding back
// into this UDN's UplinkState reconciler. All discovery-owned status remains
// part of the comparison. This is intentionally broader than
// uplinkGatewayFingerprint: discovery condition changes must trigger
// level-driven reconciliation even when concrete gateway configuration is
// unchanged.
func uplinkGatewayInputsEqual(oldState, newState *uplinkv1alpha1.UplinkState) bool {
	oldStatus := oldState.Status.DeepCopy()
	newStatus := newState.Status.DeepCopy()
	oldStatus.Conditions = gatewayInputConditions(oldStatus.Conditions)
	newStatus.Conditions = gatewayInputConditions(newStatus.Conditions)
	return reflect.DeepEqual(oldStatus, newStatus)
}

func gatewayInputConditions(conditions []metav1.Condition) []metav1.Condition {
	inputs := make([]metav1.Condition, 0, len(conditions))
	for _, condition := range conditions {
		if condition.Type == uplinkv1alpha1.UplinkStateConditionGatewayReady ||
			condition.Type == uplinkv1alpha1.UplinkStateConditionHostGatewayReady {
			continue
		}
		inputs = append(inputs, condition)
	}
	return inputs
}

func (udng *UserDefinedNetworkGateway) configureResolvedUplinkGateway(
	resolved *resolvedUplinkGateway,
) (*bridgeconfig.BridgeConfiguration, error) {
	udng.nextHops = resolved.defaultGateways

	gwInterfaceName := uplinkGatewayInterfaceName(resolved)
	if gwInterfaceName == "" {
		return nil, fmt.Errorf("uplink gateway interface is unset for network %s", udng.GetNetworkName())
	}
	link, err := util.GetNetLinkOps().LinkByName(gwInterfaceName)
	if err != nil {
		return nil, fmt.Errorf("unable to get link for Uplink gateway interface %s: %w",
			gwInterfaceName, err)
	}
	if link.Attrs().Index == 0 {
		return nil, fmt.Errorf("uplink gateway interface %s has invalid link index for network %s",
			gwInterfaceName, udng.GetNetworkName())
	}
	udng.gwInterfaceName = gwInterfaceName
	udng.gwInterfaceIndex = link.Attrs().Index
	ipv4Mode, _ := udng.IPMode()
	if err := configureUplinkGatewayRPFilter(resolved, ipv4Mode); err != nil {
		return nil, fmt.Errorf("failed to configure reverse path filtering for Uplink %s on network %s: %w",
			udng.Uplink(), udng.GetNetworkName(), err)
	}

	if config.IsModeDPUHost() {
		return nil, nil
	}
	if resolved.bridgeName == "" {
		return nil, fmt.Errorf("uplink state for uplink %s on node %s has no OVS bridge",
			udng.Uplink(), udng.node.Name)
	}

	bridge, err := bridgeconfig.NewUnmanagedBridgeConfiguration(
		udng.ovsClient,
		resolved.bridgeName,
		resolved.hostInterfaceName,
		udng.node.Name,
		physicalNetworkName(udng.NetInfo),
		resolved.ipAddresses,
		resolved.macAddress,
		resolved.hostFunction,
	)
	if err != nil {
		return nil, newUplinkGatewayError(
			uplinkv1alpha1.UplinkStateReasonBridgeMappingFailed,
			err,
		)
	}
	if err := bridge.ConfigureBridgePorts(); err != nil {
		return nil, newUplinkGatewayError(
			uplinkv1alpha1.UplinkStateReasonBridgeMappingFailed,
			err,
		)
	}
	if err := configureUplinkStaticFDBEntry(bridge); err != nil {
		return nil, newUplinkGatewayError(
			uplinkv1alpha1.UplinkStateReasonBridgeMappingFailed,
			err,
		)
	}
	return bridge, nil
}

func configureUplinkStaticFDBEntry(bridge *bridgeconfig.BridgeConfiguration) error {
	if config.IsModeDPUHost() {
		return nil
	}
	return util.SetStaticFDBEntry(
		bridge.GetBridgeName(),
		bridge.GetStaticFDBPort(),
		bridge.GetMAC(),
		config.Gateway.VLANID)
}

func (udng *UserDefinedNetworkGateway) getUplinkState() (*uplinkv1alpha1.UplinkState, error) {
	stateName := uplinkutil.StateName(udng.Uplink(), udng.node.Name)
	state, err := uplinkutil.GetState(udng.uplinkStateLister, udng.Uplink(), udng.node.Name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("waiting for UplinkState %s for uplink %q on node %q",
				stateName, udng.Uplink(), udng.node.Name)
		}
		return nil, fmt.Errorf("failed to get UplinkState %s: %w", stateName, err)
	}
	return state, nil
}

func configureUplinkGatewayRPFilter(resolved *resolvedUplinkGateway, ipv4Mode bool) error {
	if !ipv4Mode {
		return nil
	}

	interfaceName := uplinkGatewayInterfaceName(resolved)
	if interfaceName == "" {
		return nil
	}

	// Default-VRF Uplink networks keep the Uplink interface out of the UDN VRF,
	// while host service traffic can be marked and routed through the UDN table
	// to that interface. Replies return on the Uplink interface, but strict
	// rp_filter may validate them against a reverse route that does not use the
	// same interface. Loose mode matches the existing management port behavior.
	return util.SetRPFilterLooseModeForInterface(interfaceName)
}

// uplinkGatewayInterfaceName returns the Linux device used for host-side Uplink
// gateway configuration. In Full/DPU mode that device is the resolved OVS bridge;
// resolveUplinkGateway requires it to be ready before this helper is called. In
// DPU-host and non-OVS programming paths, the host interface is the local device.
// An empty return means no interface was resolved, so callers skip optional
// per-interface setup.
func uplinkGatewayInterfaceName(resolved *resolvedUplinkGateway) string {
	if config.IsModeDPU() || config.IsModeFull() {
		return resolved.bridgeName
	}
	return resolved.hostInterfaceName
}

func resolvedUplinkGatewayFromState(
	state *uplinkv1alpha1.UplinkState,
	uplinkName, nodeName string,
	requireResolvedBridge bool,
) (*resolvedUplinkGateway, error) {
	if err := uplinkutil.ValidateOVSBridgeState(state, uplinkName, nodeName, requireResolvedBridge); err != nil {
		return nil, err
	}
	return parseResolvedUplinkGateway(state)
}

func parseResolvedUplinkGateway(state *uplinkv1alpha1.UplinkState) (*resolvedUplinkGateway, error) {
	macAddress, err := net.ParseMAC(string(state.Status.MACAddress))
	if err != nil {
		return nil, fmt.Errorf("failed to parse UplinkState MAC address %q: %w",
			state.Status.MACAddress, err)
	}

	ipAddresses := make([]*net.IPNet, 0, len(state.Status.IPAddresses))
	for _, ipAddress := range state.Status.IPAddresses {
		ip, cidr, err := net.ParseCIDR(string(ipAddress))
		if err != nil {
			return nil, fmt.Errorf("failed to parse UplinkState IP address %q: %w",
				ipAddress, err)
		}
		cidr.IP = ip
		ipAddresses = append(ipAddresses, cidr)
	}
	if len(ipAddresses) == 0 {
		return nil, fmt.Errorf("uplink state has no gateway IP addresses")
	}

	defaultGateways := make([]net.IP, 0, len(state.Status.DefaultGateways))
	for _, defaultGateway := range state.Status.DefaultGateways {
		ip := net.ParseIP(string(defaultGateway))
		if ip == nil {
			return nil, fmt.Errorf("failed to parse UplinkState default gateway %q",
				defaultGateway)
		}
		defaultGateways = append(defaultGateways, ip)
	}

	bridgeName := ""
	if state.Status.OVSBridge != nil {
		bridgeName = state.Status.OVSBridge.Name
	}
	return &resolvedUplinkGateway{
		hostInterfaceName: string(state.Status.HostInterfaceName),
		bridgeName:        bridgeName,
		macAddress:        macAddress,
		ipAddresses:       ipAddresses,
		defaultGateways:   defaultGateways,
		hostFunction:      state.Status.HostFunction.DeepCopy(),
	}, nil
}

func physicalNetworkName(netInfo util.NetInfo) string {
	if netInfo.IsDefault() || (netInfo.IsPrimaryNetwork() && netInfo.Uplink() == "") {
		return types.PhysicalNetworkName
	}
	return netInfo.GetNetworkName()
}

// GetUDNMarkChain returns the UDN mark chain name
func GetUDNMarkChain(pktMark string) string {
	return "udn-mark-" + pktMark
}

// delMarkChain removes the UDN packet mark nftables chain
func (udng *UserDefinedNetworkGateway) delMarkChain() error {
	nft, err := nodenft.GetNFTablesHelper()
	if err != nil {
		return err
	}
	tx := nft.NewTransaction()
	chain := &knftables.Chain{
		Name: GetUDNMarkChain(fmt.Sprintf("0x%x", udng.pktMark)),
	}
	// Delete would return an error if we tried to delete a chain that didn't exist, so
	// we do an Add first (which is a no-op if the chain already exists) and then Delete.
	tx.Add(chain)
	tx.Delete(chain)
	return nft.Run(context.TODO(), tx)
}

// addMarkChain adds the UDN nftables chain containing a rule that marks packets
// with the network specific value
func (udng *UserDefinedNetworkGateway) addMarkChain() error {
	counterIfDebug := ""
	if config.Logging.Level > 4 {
		counterIfDebug = "counter"
	}

	nft, err := nodenft.GetNFTablesHelper()
	if err != nil {
		return err
	}
	tx := nft.NewTransaction()
	chain := &knftables.Chain{
		Name:    GetUDNMarkChain(fmt.Sprintf("0x%x", udng.pktMark)),
		Comment: ptr.To(fmt.Sprintf("%s: UDN packet marking", udng.GetNetworkName())),
	}
	tx.Add(chain)
	tx.Flush(chain)

	tx.Add(&knftables.Rule{
		Chain: chain.Name,
		Rule:  knftables.Concat("meta mark set", fmt.Sprintf("0x%x", udng.pktMark), counterIfDebug),
	})

	return nft.Run(context.TODO(), tx)
}

// Start creates the gateway plumbing required by this UDN and starts its
// reconcilers.
func (udng *UserDefinedNetworkGateway) Start() error {
	if udng.Uplink() == "" {
		udng.operationMutex.Lock()
		err := udng.addNetworkWithResolvedUplink(nil)
		udng.operationMutex.Unlock()
		if err != nil {
			return err
		}
		// React to runtime gateway bridge address changes to refresh the node
		// connected subnet routes. Uplink-backed networks instead react to their
		// UplinkState, which rebuilds this programming when its addresses change.
		udng.registerUDNAddressChangeReconciler(udng.GetNetworkName(), udng.Reconcile)
		return controllerutil.Start(udng.gatewayReconciler)
	}

	err := controllerutil.StartWithInitialSync(
		func() error {
			return udng.reconcileUplinkState(
				uplinkutil.StateName(udng.Uplink(), udng.node.Name),
			)
		},
		udng.uplinkStateController,
		udng.gatewayReconciler,
	)
	if err != nil {
		udng.operationMutex.Lock()
		cleanupErr := udng.unconfigureUplinkGateway()
		udng.operationMutex.Unlock()
		if cleanupErr != nil {
			cleanupErr = fmt.Errorf("failed to clean up gateway after initial programming failure: %w", cleanupErr)
		}
		return utilerrors.Join(err, cleanupErr)
	}

	return nil
}

func (udng *UserDefinedNetworkGateway) addNetworkWithResolvedUplink(
	resolved *resolvedUplinkGateway,
) error {
	if (config.IsModeDPU() || config.IsModeFull()) && udng.openflowManager == nil {
		return fmt.Errorf("openflow manager has not been provided for network: %s", udng.NetInfo.GetNetworkName())
	}
	udng.updateAdvertisementStatus()

	nodeSubnets, err := udng.getLocalSubnets()
	if err != nil {
		return fmt.Errorf("could not create management port for network %s, cannot determine subnets: %v",
			udng.GetNetworkName(), err)
	}

	// TBD-merge udng.node.Name, needs lower case?
	udng.mgmtPortController, err = managementport.NewUDNManagementPortController(udng.nodeInformer, udng.node.Name, nodeSubnets, udng.NetInfo)
	if err != nil {
		return fmt.Errorf("could not create management port for network %s, UDN management port controller init failure: %v",
			udng.GetNetworkName(), err)
	}

	err = udng.mgmtPortController.Create()
	if err != nil {
		klog.Errorf("Create management port for network %s failed: %v", udng.GetNetworkName(), err)
		return fmt.Errorf("could not create management port for network %s, management port creation failure: %v",
			udng.GetNetworkName(), err)
	}

	var uplinkBridge *bridgeconfig.BridgeConfiguration
	if resolved != nil {
		uplinkBridge, err = udng.configureResolvedUplinkGateway(resolved)
		if err != nil {
			return fmt.Errorf("failed to configure Uplink gateway for network %s: %w",
				udng.GetNetworkName(), err)
		}
	}

	// Select the shared gateway bridge whose subnet the node-connected subnet
	// routes are derived from; it is read fresh whenever the routes are
	// (re)programmed. Only ordinary (non-Uplink) networks get these routes: the
	// egress-to-same-subnet-node fix they implement is specific to local gateway
	// mode, whereas Uplink-backed networks are shared gateway only and already
	// have the Uplink's connected prefix route enslaved into their VRF by the
	// kernel. Programming our own SCOPE_LINK route for the same subnet would
	// collide with that kernel-owned route, so leave nodeSubnetBridge nil for
	// them (reconcileNodeSubnetRoutes then cleans up any previously programmed
	// routes and is otherwise a no-op).
	if uplinkBridge == nil && udng.openflowManager != nil {
		udng.nodeSubnetBridge = udng.openflowManager.defaultBridge.BridgeConfiguration
	} else {
		udng.nodeSubnetBridge = nil
	}

	if config.IsModeDPU() {
		vrfDeviceName := util.GetNetworkVRFName(udng.NetInfo)
		if err = udng.ensureDPUVRF(); err != nil {
			return err
		}
		if udng.Uplink() != "" {
			if err = udng.reconcileUplinkGatewayVRFSlave(vrfDeviceName); err != nil {
				return err
			}
		}
	} else if config.IsModeDPUHost() || config.IsModeFull() {
		mgmtPortName := util.GetNetworkScopedK8sMgmtHostIntfName(uint(udng.GetNetworkID()))
		mplink, err := util.LinkByName(mgmtPortName)
		if err != nil {
			return err
		}

		vrfTableId := util.CalculateRouteTableID(mplink.Attrs().Index)
		udng.vrfTableId = vrfTableId

		vrfDeviceName := util.GetNetworkVRFName(udng.NetInfo)
		routes, err := udng.computeRoutesForUDN(mplink)
		if err != nil {
			return fmt.Errorf("failed to compute routes for network %s, err: %v", udng.GetNetworkName(), err)
		}
		if err = udng.vrfManager.AddVRF(vrfDeviceName, mplink.Attrs().Name, uint32(udng.vrfTableId), nil); err != nil {
			return fmt.Errorf("could not add VRF %d for network %s, err: %v", udng.vrfTableId, udng.GetNetworkName(), err)
		}
		if err = udng.reconcileUplinkGatewayVRFSlave(vrfDeviceName); err != nil {
			return err
		}
		if udng.Uplink() != "" && udng.gwInterfaceName != "" && udng.gwInterfaceName != mplink.Attrs().Name {
			if err = setNodeMasqueradeIPOnExtBridge(udng.gwInterfaceName); err != nil {
				return fmt.Errorf("failed to configure service route source addresses on Uplink gateway interface %s for network %s: %w",
					udng.gwInterfaceName, udng.GetNetworkName(), err)
			}
			if err = addHostMACBindings(udng.gwInterfaceName); err != nil {
				return fmt.Errorf("failed to configure service next-hop neighbors on Uplink gateway interface %s for network %s: %w",
					udng.gwInterfaceName, udng.GetNetworkName(), err)
			}
		}
		if err = udng.addUDNManagementPortIPs(mplink); err != nil {
			return fmt.Errorf("unable to add management port IP(s) for link %s, for network %s: %w", mplink.Attrs().Name, udng.GetNetworkName(), err)
		}
		if err = udng.vrfManager.AddVRFRoutes(vrfDeviceName, routes); err != nil {
			return fmt.Errorf("could not add VRF %s routes for network %s, err: %v", vrfDeviceName, udng.GetNetworkName(), err)
		}
	}

	if config.IsModeDPUHost() || config.IsModeFull() {
		if err = udng.applyAdvertisedRoutingState(); err != nil {
			return err
		}
		if err = udng.updateAdvertisedUDNIsolationRules(); err != nil {
			return fmt.Errorf("failed to update isolation rules for network %s: %w", udng.GetNetworkName(), err)
		}
	}

	// Add static kernel neighbor entries for per-CUDN masquerade IPs on
	// the gateway bridge so that the kernel never sends ARP/NS for these
	// addresses. Without this, IPv6 NDP resolution for masquerade IPs
	// fails because NS packets are not forwarded to CUDN patch ports
	// (blocked by NO_FLOOD).
	if config.IsModeFull() {
		bridgeName := udng.openflowManager.getDefaultBridgeName()
		if uplinkBridge != nil {
			bridgeName = uplinkBridge.GetGatewayIface()
		}
		if err = addUDNMasqIPNeighbors(bridgeName, udng.v4MasqIPs, udng.v6MasqIPs); err != nil {
			return fmt.Errorf("failed to add masquerade IP neighbor entries on %s for network %s: %w",
				bridgeName, udng.GetNetworkName(), err)
		}
	}

	if config.IsModeDPU() || config.IsModeFull() {
		var mgmtIPs []*net.IPNet
		for _, subnet := range nodeSubnets {
			mgmtIPs = append(mgmtIPs, udng.GetNodeManagementIP(subnet))
		}
		udng.openflowBridgeName = defaultOpenFlowBridgeSetName
		if uplinkBridge != nil {
			udng.openflowBridgeName = uplinkBridge.GetBridgeName()
		}
		err = udng.openflowManager.addNetwork(udng.openflowBridgeName, uplinkBridge, udng.NetInfo, nodeSubnets, mgmtIPs,
			udng.masqCTMark, udng.pktMark, udng.v6MasqIPs, udng.v4MasqIPs)
		if err != nil {
			return fmt.Errorf("could not add network %s: %v", udng.GetNetworkName(), err)
		}

		waiter := newStartupWaiterWithTimeout(waitForPatchPortTimeout)
		readyFunc := func() (bool, error) {
			if err := udng.openflowManager.setNetworkOfPatchPort(udng.openflowBridgeName, udng.GetNetworkName()); err != nil {
				klog.V(3).Infof("Failed to set network %s's openflow ports for bridge target %s; error: %v",
					udng.GetNetworkName(), openflowBridgeTargetDisplayName(udng.openflowBridgeName), err)
				return false, nil
			}
			return true, nil
		}
		postFunc := func() error {
			if err := udng.gateway.ReconcileNetwork(udng.NetInfo); err != nil {
				return fmt.Errorf("failed to reconcile flows on bridge for network %s; error: %v", udng.GetNetworkName(), err)
			}
			if udng.Uplink() != "" {
				if err := udng.syncUplinkBridgeFlows(); err != nil {
					return err
				}
			}
			return nil
		}
		waiter.AddWait(readyFunc, postFunc)
		if err := waiter.Wait(); err != nil {
			return newUplinkGatewayError(uplinkv1alpha1.UplinkStateReasonBridgeMappingFailed, err)
		}
	} else {
		if err := udng.gateway.ReconcileNetwork(udng.NetInfo); err != nil {
			return fmt.Errorf("failed to reconcile flows on bridge for network %s; error: %v", udng.GetNetworkName(), err)
		}
	}

	if config.IsModeDPUHost() || config.IsModeFull() {
		if err := udng.addMarkChain(); err != nil {
			return fmt.Errorf("failed to add the service masquerade chain: %w", err)
		}
	}

	return nil
}

func (udng *UserDefinedNetworkGateway) GetNetworkRuleMetadata() string {
	return fmt.Sprintf("%s-%d", udng.GetNetworkName(), udng.GetNetworkID())
}

// Cleanup removes all gateway plumbing used by this UDN. Stop must be called
// before Cleanup so no reconciliation can race with terminal cleanup. It is
// invalid to use this instance after Cleanup returns successfully.
func (udng *UserDefinedNetworkGateway) Cleanup() error {
	udng.operationMutex.Lock()
	defer udng.operationMutex.Unlock()

	if udng.Uplink() == "" {
		return udng.delNetwork()
	}

	cleanupErr := udng.unconfigureUplinkGateway()
	udng.uplinkStateGatewayStatusController.ReportNetworkDeleted(
		udng.NetInfo, udng.uplinkStateUID, udng.uplinkFingerprint, cleanupErr)
	return cleanupErr
}

// unconfigureUplinkGateway tears down the currently programmed UDN gateway but
// keeps this object reusable by its reconcilers. This is the non-terminal
// counterpart of Cleanup used while an Uplink temporarily no longer selects
// the node or is being reconfigured.
func (udng *UserDefinedNetworkGateway) unconfigureUplinkGateway() error {
	if !udng.uplinkGatewayCleanupRequired {
		return nil
	}
	udng.uplinkGatewayPartial = true
	if err := udng.delNetwork(); err != nil {
		return fmt.Errorf("failed to unconfigure Uplink gateway for network %s: %w", udng.GetNetworkName(), err)
	}
	udng.uplinkGatewayCleanupRequired = false
	udng.uplinkGatewayPartial = false
	udng.uplinkStateUID = ""
	udng.uplinkFingerprint = uplinkGatewayFingerprint{}
	return nil
}

// replaceUplinkGateway always unconfigures existing programming before applying
// the resolved configuration. Call it only for initial programming, after the
// resolved fingerprint changes, or to retry a partial replacement. Mark the
// gateway as programmed before addNetwork starts so a partial add is cleaned up
// on the next retry.
func (udng *UserDefinedNetworkGateway) replaceUplinkGateway(
	resolved *resolvedUplinkGateway,
	stateUID k8stypes.UID,
	fingerprint uplinkGatewayFingerprint,
) error {
	if err := udng.unconfigureUplinkGateway(); err != nil {
		return err
	}
	udng.uplinkGatewayCleanupRequired = true
	if err := udng.addNetworkWithResolvedUplink(resolved); err != nil {
		return fmt.Errorf("failed to program Uplink gateway for network %s: %w", udng.GetNetworkName(), err)
	}
	udng.uplinkStateUID = stateUID
	udng.uplinkFingerprint = fingerprint
	return nil
}

// reconcileUplinkConfiguration reads the newest informer state and makes this
// UDN's dataplane match it. The caller serializes it with all other operations
// on this gateway.
func (udng *UserDefinedNetworkGateway) reconcileUplinkConfiguration() (
	k8stypes.UID,
	uplinkGatewayFingerprint,
	error,
) {
	state, err := udng.getUplinkState()
	if err != nil {
		unconfigureErr := udng.unconfigureUplinkGateway()
		if unconfigureErr != nil {
			return "", uplinkGatewayFingerprint{}, newUplinkGatewayError(
				uplinkv1alpha1.UplinkStateReasonGatewayProgrammingFailed,
				utilerrors.Join(err, unconfigureErr),
			)
		}
		return "", uplinkGatewayFingerprint{}, newUplinkGatewayError(
			uplinkv1alpha1.UplinkStateReasonGatewayProgrammingFailed,
			err,
		)
	}
	observedFingerprint := uplinkGatewayFingerprintFromState(state)
	resolved, err := resolvedUplinkGatewayFromState(
		state, udng.Uplink(), udng.node.Name,
		config.IsModeDPU() || config.IsModeFull())
	if err != nil {
		unconfigureErr := udng.unconfigureUplinkGateway()
		if unconfigureErr != nil {
			return state.UID, observedFingerprint, newUplinkGatewayError(
				uplinkv1alpha1.UplinkStateReasonGatewayProgrammingFailed,
				utilerrors.Join(err, unconfigureErr),
			)
		}
		return state.UID, observedFingerprint, newUplinkGatewayError(
			uplinkv1alpha1.UplinkStateReasonGatewayProgrammingFailed,
			err,
		)
	}

	programmed := udng.uplinkGatewayCleanupRequired && !udng.uplinkGatewayPartial
	if programmed && udng.uplinkFingerprint == observedFingerprint {
		// Readiness-only status changes must not disturb working dataplane.
		udng.uplinkStateUID = state.UID
		return state.UID, observedFingerprint, nil
	}
	if programmed &&
		fingerprintsDifferOnlyInDefaultGateways(udng.uplinkFingerprint, observedFingerprint) {
		return state.UID, observedFingerprint,
			udng.updateUplinkDefaultGateways(resolved, state.UID, observedFingerprint)
	}
	return state.UID, observedFingerprint,
		udng.replaceUplinkGateway(resolved, state.UID, observedFingerprint)
}

func fingerprintsDifferOnlyInDefaultGateways(a, b uplinkGatewayFingerprint) bool {
	a.defaultGateways = b.defaultGateways
	return a == b
}

// updateUplinkDefaultGateways follows a change of the published default
// gateways without replacing the gateway: replacing it releases the gateway
// interface from the network VRF, which drops a BGP session running over it.
// The gateway router is programmed from UplinkState by the OVN side; on the
// host only the managed default routes of the network VRF depend on the next
// hops.
func (udng *UserDefinedNetworkGateway) updateUplinkDefaultGateways(
	resolved *resolvedUplinkGateway,
	stateUID k8stypes.UID,
	fingerprint uplinkGatewayFingerprint,
) error {
	udng.nextHops = resolved.defaultGateways
	if config.IsModeDPUHost() || config.IsModeFull() {
		// A family left without any gateway needs its managed default
		// removed; updateUDNVRFIPRoute replaces the managed default of a
		// family that still has one in place, or removes every managed
		// default under VRF-Lite, including those a failed advertisement
		// reconcile left behind. A failure leaves the routes half-changed,
		// so the next reconcile rebuilds the gateway.
		v4Gateways, v6Gateways := util.SplitIPsByIPFamily(resolved.defaultGateways)
		hasV4Subnet, hasV6Subnet := udng.IPMode()
		var orphaned []netlink.Route
		if hasV4Subnet && len(v4Gateways) == 0 {
			orphaned = append(orphaned, udng.managedDefaultRouteKey(false))
		}
		if hasV6Subnet && len(v6Gateways) == 0 {
			orphaned = append(orphaned, udng.managedDefaultRouteKey(true))
		}
		var err error
		if len(orphaned) > 0 {
			err = udng.vrfManager.DeleteVRFRoutes(util.GetNetworkVRFName(udng.NetInfo), orphaned)
		}
		if err == nil {
			err = udng.updateUDNVRFIPRoute()
		}
		if err != nil {
			udng.uplinkGatewayPartial = true
			return newUplinkGatewayError(uplinkv1alpha1.UplinkStateReasonGatewayProgrammingFailed, err)
		}
	}
	udng.uplinkStateUID = stateUID
	udng.uplinkFingerprint = fingerprint
	return nil
}

// managedDefaultRouteKey returns the managed default route of the family as
// the VRF manager tracks it, without a next hop: deleting it removes and
// untracks the route via whichever gateway it was installed through, also
// when the kernel no longer has it. The protocol keeps an unmanaged default
// migrated into the VRF table by the enslavement out of the match.
func (udng *UserDefinedNetworkGateway) managedDefaultRouteKey(isV6 bool) netlink.Route {
	_, anyCIDR, _ := net.ParseCIDR("0.0.0.0/0")
	if isV6 {
		_, anyCIDR, _ = net.ParseCIDR("::/0")
	}
	return netlink.Route{
		LinkIndex: udng.gwInterfaceIndex,
		Dst:       anyCIDR,
		Table:     udng.vrfTableId,
		Protocol:  netlink.RouteProtocol(types.OVNKProtocol),
	}
}

// reconcileUplinkState makes this CUDN's gateway match the newest informer
// state. The controller retries errors indefinitely; operationMutex serializes
// each attempt with initial setup, advertised-state reconciliation, and cleanup.
func (udng *UserDefinedNetworkGateway) reconcileUplinkState(key string) error {
	if key != uplinkutil.StateName(udng.Uplink(), udng.node.Name) {
		return nil
	}

	udng.operationMutex.Lock()
	defer udng.operationMutex.Unlock()
	observedUID, observedFingerprint, reconcileErr :=
		udng.reconcileUplinkConfiguration()
	udng.uplinkStateGatewayStatusController.ReportNetworkResult(
		udng.NetInfo, observedUID, observedFingerprint, reconcileErr)
	return reconcileErr
}

func (udng *UserDefinedNetworkGateway) delNetwork() error {
	var errs []error
	vrfDeviceName := util.GetNetworkVRFName(udng.NetInfo)
	if config.IsModeDPUHost() || config.IsModeFull() {
		// delete the iprules for this network
		if err := udng.ruleManager.DeleteWithMetadata(udng.GetNetworkRuleMetadata()); err != nil {
			errs = append(errs, fmt.Errorf("unable to delete iprules for network %s, err: %v", udng.GetNetworkName(), err))
		}
	}
	// delete the VRF device for this network
	if err := udng.vrfManager.DeleteVRF(vrfDeviceName); err != nil {
		errs = append(errs, fmt.Errorf("unable to delete VRF device %s for network %s, err: %v", vrfDeviceName, udng.GetNetworkName(), err))
	}
	// The VRF and all its routes are gone; drop the tracked node subnet routes so
	// that a reused (Uplink) gateway object reprograms them from scratch.
	udng.nodeSubnetRoutes = nil
	if config.IsModeDPU() || config.IsModeFull() {
		// delete the openflows for this network
		if udng.openflowManager != nil {
			if err := udng.openflowManager.delNetwork(udng.NetInfo, udng.openflowBridgeName); err != nil {
				errs = append(errs, fmt.Errorf("unable to delete openflows for network %s from bridge target %s, err: %w",
					udng.GetNetworkName(), openflowBridgeTargetDisplayName(udng.openflowBridgeName), err))
			}
		}
	}
	if config.IsModeFull() && udng.openflowManager != nil {
		bridgeName := udng.openflowManager.getDefaultBridgeName()
		if udng.Uplink() != "" && udng.gwInterfaceName != "" {
			bridgeName = udng.gwInterfaceName
		}
		if err := delUDNMasqIPNeighbors(bridgeName, udng.v4MasqIPs, udng.v6MasqIPs); err != nil {
			errs = append(errs, fmt.Errorf("failed to delete masquerade IP neighbor entries on %s for network %s: %w",
				bridgeName, udng.GetNetworkName(), err))
		}
	}
	if udng.openflowManager != nil || config.IsModeDPUHost() {
		// Network config already removed: regenerate the shared bridge flows without it.
		if err := udng.gateway.ReconcileNetworkRemoval(); err != nil {
			errs = append(errs, fmt.Errorf("failed to reconcile default gateway for network %s, err: %v", udng.GetNetworkName(), err))
		}
	}
	if udng.Uplink() != "" && (config.IsModeDPU() || config.IsModeFull()) {
		if _, err := udng.openflowManager.syncUplinkBridgeFlows(udng.openflowBridgeName); err != nil {
			errs = append(errs, newUplinkGatewayError(uplinkv1alpha1.UplinkStateReasonGatewayProgrammingFailed, err))
		}
	}

	if config.IsModeDPUHost() || config.IsModeFull() {
		err := udng.deleteAdvertisedUDNIsolationRules()
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to remove advertised UDN isolation rules for network %s: %w", udng.GetNetworkName(), err))
		}

		if err := udng.delMarkChain(); err != nil {
			errs = append(errs, fmt.Errorf("failed to remove mark chain for network %s, err: %v", udng.GetNetworkName(), err))
		}
	}
	// delete the management port interface for this network
	if udng.mgmtPortController != nil {
		err := udng.mgmtPortController.Delete()
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to delete UDN management port for network %s, err: %w", udng.GetNetworkName(), err))
		}
	}
	if len(errs) > 0 {
		return utilerrors.Join(errs...)
	}

	return nil
}

// addUDNMasqIPNeighbors adds permanent kernel neighbor entries for a
// user-defined network's gateway router masquerade IPs on the given bridge
// interface. These IPs are internal to the node and handled entirely by OVS
// flows, so they will never respond to ARP/NS on the wire. Static entries
// prevent the kernel from sending ARP/NS requests that would otherwise fail
// (IPv6 NS is blocked by NO_FLOOD on CUDN patch ports).
func addUDNMasqIPNeighbors(bridgeName string, v4MasqIPs, v6MasqIPs *udn.MasqueradeIPs) error {
	link, err := util.LinkSetUp(bridgeName)
	if err != nil {
		return fmt.Errorf("unable to get link for %s: %v", bridgeName, err)
	}
	for _, ip := range udnMasqGatewayRouterIPs(v4MasqIPs, v6MasqIPs) {
		mac := util.IPAddrToHWAddr(ip)
		klog.Infof("Ensuring UDN masquerade IP neighbor entry: %s -> %s on %s", ip, mac, bridgeName)
		if exists, err := util.LinkNeighExists(link, ip, mac); err == nil && !exists {
			if err = util.LinkNeighDel(link, ip); err != nil {
				klog.Warningf("Failed to remove stale neighbor entry for %s on %s: %v",
					ip, bridgeName, err)
			}
			if err = util.LinkNeighSet(link, ip, mac); err != nil {
				return fmt.Errorf("failed to add neighbor %s -> %s on %s: %v",
					ip, mac, bridgeName, err)
			}
		} else if err != nil {
			return fmt.Errorf("failed to check neighbor %s on %s: %v", ip, bridgeName, err)
		}
	}
	return nil
}

// delUDNMasqIPNeighbors removes the kernel neighbor entries previously added
// by addUDNMasqIPNeighbors.
func delUDNMasqIPNeighbors(bridgeName string, v4MasqIPs, v6MasqIPs *udn.MasqueradeIPs) error {
	link, err := util.LinkSetUp(bridgeName)
	if err != nil {
		return fmt.Errorf("unable to get link for %s: %v", bridgeName, err)
	}
	var errs []error
	for _, ip := range udnMasqGatewayRouterIPs(v4MasqIPs, v6MasqIPs) {
		if err := util.LinkNeighDel(link, ip); err != nil && !errors.Is(err, syscall.ENOENT) {
			errs = append(errs, fmt.Errorf("failed to delete neighbor %s on %s: %v", ip, bridgeName, err))
		}
	}
	return errors.Join(errs...)
}

// udnMasqGatewayRouterIPs returns the gateway router masquerade IPs for a UDN.
func udnMasqGatewayRouterIPs(v4MasqIPs, v6MasqIPs *udn.MasqueradeIPs) []net.IP {
	var ips []net.IP
	if v4MasqIPs != nil && v4MasqIPs.GatewayRouter != nil {
		ips = append(ips, v4MasqIPs.GatewayRouter.IP)
	}
	if v6MasqIPs != nil && v6MasqIPs.GatewayRouter != nil {
		ips = append(ips, v6MasqIPs.GatewayRouter.IP)
	}
	return ips
}

// getLocalSubnets returns pod subnets used by the current node.
// For L3 networks it parses the ovnNodeSubnets annotation, for L2 networks it returns the network subnets.
func (udng *UserDefinedNetworkGateway) getLocalSubnets() ([]*net.IPNet, error) {
	var networkLocalSubnets []*net.IPNet
	var err error

	// fetch subnets which we will use to get management port IP(s)
	if udng.TopologyType() == types.Layer3Topology {
		networkLocalSubnets, err = util.ParseNodeHostSubnetAnnotation(udng.node, udng.GetNetworkName())
		if err != nil {
			return nil, fmt.Errorf("waiting for node %s to start, no annotation found on node for network %s: %w",
				udng.node.Name, udng.GetNetworkName(), err)
		}
	} else if udng.TopologyType() == types.Layer2Topology {
		// NOTE: We don't support L2 networks without subnets as primary UDNs
		globalFlatL2Networks := udng.Subnets()
		for _, globalFlatL2Network := range globalFlatL2Networks {
			networkLocalSubnets = append(networkLocalSubnets, globalFlatL2Network.CIDR)
		}
	}
	return networkLocalSubnets, nil
}

func (udng *UserDefinedNetworkGateway) addUDNManagementPortIPs(mpLink netlink.Link) error {
	networkLocalSubnets, err := udng.getLocalSubnets()
	if err != nil {
		return err
	}

	klog.V(5).Infof("Add management port IPs on interface %s for network %s subnets %+v",
		mpLink.Attrs().Name, udng.GetNetworkName(), networkLocalSubnets)

	// extract management port IP from subnets and add it to link
	for _, subnet := range networkLocalSubnets {
		if config.IPv6Mode && utilnet.IsIPv6CIDR(subnet) || config.IPv4Mode && utilnet.IsIPv4CIDR(subnet) {
			ip := udng.GetNodeManagementIP(subnet)
			addrFlags := 0
			if udng.useNoPrefixRouteManagementPortIPs() {
				// In local gateway no-overlay mode, ingress to local UDN pods
				// must go through OVN, so prevent Linux from creating a
				// connected route that bypasses the UDN cluster router.
				addrFlags = unix.IFA_F_NOPREFIXROUTE
			}
			existingIP, err := util.LinkAddrGetIPNet(mpLink, ip.IP)
			if err != nil {
				return fmt.Errorf("failed to find management port IP from subnet %s on netdevice %s for network %s, err: %v",
					subnet, mpLink.Attrs().Name, udng.GetNetworkName(), err)
			}
			if existingIP != nil && existingIP.String() != ip.String() {
				err = util.LinkAddrDel(mpLink, existingIP)
				if err != nil {
					return fmt.Errorf("failed to delete stale management port IP %s from netdevice %s for network %s, err: %v",
						existingIP, mpLink.Attrs().Name, udng.GetNetworkName(), err)
				}
				existingIP = nil
			}
			if existingIP == nil {
				err = util.LinkAddrAdd(mpLink, ip, addrFlags, 0, 0)
			}
			if err != nil {
				return fmt.Errorf("failed to add management port IP from subnet %s to netdevice %s for network %s, err: %v",
					subnet, mpLink.Attrs().Name, udng.GetNetworkName(), err)
			}
		}
	}
	return nil
}

func (udng *UserDefinedNetworkGateway) useNoPrefixRouteManagementPortIPs() bool {
	return config.Gateway.Mode == config.GatewayModeLocal &&
		udng.TopologyType() == types.Layer3Topology &&
		udng.Transport() == types.NetworkTransportNoOverlay
}

// computeRoutesForUDN returns a list of routes programmed into a given UDN's VRF
// when adding new routes please leave a sample comment on how that route looks like
func (udng *UserDefinedNetworkGateway) computeRoutesForUDN(mpLink netlink.Link) ([]netlink.Route, error) {
	networkMTU := udng.NetInfo.MTU()
	if networkMTU == 0 {
		networkMTU = config.Default.MTU
	}
	var retVal []netlink.Route
	serviceRouteLinkIndex := udng.gwInterfaceIndex
	serviceRouteUsesUplink := udng.Uplink() != ""
	// Route1: Add serviceCIDR route: 10.96.0.0/16 via 169.254.169.4 dev breth0 mtu 1400
	// or via the selected Uplink gateway interface when one is configured.
	// This is necessary for UDN CNI and host-networked pods to talk to services.
	addedUplinkV6DummyNextHopRoute := false
	for _, serviceSubnet := range config.Kubernetes.ServiceCIDRs {
		serviceSubnet := serviceSubnet
		isV6 := utilnet.IsIPv6CIDR(serviceSubnet)
		gwIP := config.Gateway.MasqueradeIPs.V4DummyNextHopMasqueradeIP
		if isV6 {
			gwIP = config.Gateway.MasqueradeIPs.V6DummyNextHopMasqueradeIP
		}
		if serviceRouteUsesUplink && isV6 && !addedUplinkV6DummyNextHopRoute {
			// Uplink bridges do not carry the shared gateway bridge's
			// masquerade address. Add a per-VRF connected route so Linux
			// accepts the IPv6 dummy next hop on the selected Uplink link.
			retVal = append(retVal, netlink.Route{
				LinkIndex: serviceRouteLinkIndex,
				Dst:       util.GetIPNetFullMaskFromIP(gwIP),
				Scope:     netlink.SCOPE_LINK,
				Table:     udng.vrfTableId,
			})
			addedUplinkV6DummyNextHopRoute = true
		}
		serviceRoute := netlink.Route{
			LinkIndex: serviceRouteLinkIndex,
			Dst:       serviceSubnet,
			MTU:       networkMTU,
			Gw:        gwIP,
			Src:       config.Gateway.MasqueradeIPs.V4HostMasqueradeIP,
			Table:     udng.vrfTableId,
		}
		if isV6 {
			serviceRoute.Src = config.Gateway.MasqueradeIPs.V6HostMasqueradeIP
		}
		if serviceRouteUsesUplink {
			serviceRoute.Flags = unix.RTNH_F_ONLINK
		}
		retVal = append(retVal, serviceRoute)
	}

	// Route2: Add default route: default via 172.18.0.1 dev breth0 mtu 1400
	// necessary for UDN CNI and host-networked pods default traffic to go to node's gatewayIP
	defaultRoute, err := udng.getDefaultRouteExceptIfVRFLite()
	if err != nil {
		return nil, fmt.Errorf("unable to add default route for network %s, err: %v", udng.GetNetworkName(), err)
	}
	retVal = append(retVal, defaultRoute...)

	// Note: the node network connected subnet route (e.g.
	// 192.168.x.0/24 dev br-ex scope link src 192.168.x.5) is programmed
	// separately via reconcileNodeSubnetRoutes so that it can be refreshed on
	// gateway bridge address changes without recomputing the full route set.

	// Route3: Add MasqueradeRoute for reply traffic route: 169.254.169.12 dev ovn-k8s-mpX mtu 1400
	// necessary for reply traffic towards UDN CNI pods to go into OVN
	masqIPv4, err := udng.getV4MasqueradeIP()
	if err != nil {
		return nil, fmt.Errorf("unable to fetch masqueradeV4 IP for network %s, err: %v", udng.GetNetworkName(), err)
	}
	if masqIPv4 != nil {
		retVal = append(retVal, netlink.Route{
			LinkIndex: mpLink.Attrs().Index,
			Dst:       masqIPv4,
			MTU:       networkMTU,
			Table:     udng.vrfTableId,
		})
	}

	masqIPv6, err := udng.getV6MasqueradeIP()
	if err != nil {
		return nil, fmt.Errorf("unable to fetch masqueradeV6 IP for network %s, err: %v", udng.GetNetworkName(), err)
	}
	if masqIPv6 != nil {
		retVal = append(retVal, netlink.Route{
			LinkIndex: mpLink.Attrs().Index,
			Dst:       masqIPv6,
			MTU:       networkMTU,
			Table:     udng.vrfTableId,
		})
	}

	// Add routes for V[4|6]HostETPLocalMasqueradeIP:
	//   169.254.0.3 via 100.100.1.1 dev ovn-k8s-mp1
	// For Layer3 networks add the cluster subnet route
	//   100.100.0.0/16 via 100.100.1.1 dev ovn-k8s-mp1
	// Local-gateway no-overlay Layer3 uses the node-local subnet instead:
	//   100.100.1.0/24 via 100.100.1.1 dev ovn-k8s-mp1
	networkLocalSubnets, err := udng.getLocalSubnets()
	if err != nil {
		return nil, err
	}
	for _, localSubnet := range networkLocalSubnets {
		gwIP := udng.GetNodeGatewayIP(localSubnet)
		if gwIP == nil {
			return nil, fmt.Errorf("unable to find gateway IP for network %s, subnet: %s", udng.GetNetworkName(), localSubnet)
		}
		if udng.useNoPrefixRouteManagementPortIPs() {
			// The management port address uses noprefixroute in local gateway
			// no-overlay mode. Add a host route to the UDN gateway before
			// installing routes through it.
			retVal = append(retVal, netlink.Route{
				LinkIndex: mpLink.Attrs().Index,
				Dst:       util.GetIPNetFullMaskFromIP(gwIP.IP),
				Scope:     netlink.SCOPE_LINK,
				Table:     udng.vrfTableId,
			})
		}
		etpLocalMasqueradeIP := config.Gateway.MasqueradeIPs.V4HostETPLocalMasqueradeIP
		if utilnet.IsIPv6CIDR(localSubnet) {
			etpLocalMasqueradeIP = config.Gateway.MasqueradeIPs.V6HostETPLocalMasqueradeIP
		}
		retVal = append(retVal, netlink.Route{
			LinkIndex: mpLink.Attrs().Index,
			Dst: &net.IPNet{
				IP:   etpLocalMasqueradeIP,
				Mask: util.GetIPFullMask(etpLocalMasqueradeIP),
			},
			Gw:    gwIP.IP,
			Table: udng.vrfTableId,
		})
		if udng.NetInfo.TopologyType() == types.Layer3Topology {
			if udng.useNoPrefixRouteManagementPortIPs() {
				retVal = append(retVal, netlink.Route{
					LinkIndex: mpLink.Attrs().Index,
					Dst:       localSubnet,
					Gw:        gwIP.IP,
					Table:     udng.vrfTableId,
				})
				continue
			}
			// Route every cluster subnet of the same IP family via this
			// node's gateway IP into OVN. A multi-subnet Layer3 UDN has more
			// than one cluster subnet; routing only the subnet that contains
			// the local gateway IP left host-networked traffic destined to
			// pods on the network's other cluster subnets without a route
			// into OVN, so it fell through to the VRF default route and left
			// the node via the external gateway.
			for _, clusterSubnet := range udng.Subnets() {
				if utilnet.IsIPv6CIDR(clusterSubnet.CIDR) != utilnet.IsIPv6(gwIP.IP) {
					continue
				}
				retVal = append(retVal, netlink.Route{
					LinkIndex: mpLink.Attrs().Index,
					Dst:       clusterSubnet.CIDR,
					Gw:        gwIP.IP,
					Table:     udng.vrfTableId,
				})
			}
		}
	}
	// Add unreachable route to enure that kernel always finds a match to the VRF table rather than
	// referring to default VRF table and send traffic via unwanted interfaces and to unwanted gateway.
	// non 0 link index for an unreachable or blackhole IPv4 route returns 'invalid argument'
	hasV4Subnet, hasV6Subnet := udng.IPMode()
	if hasV4Subnet {
		_, v4AnyCIDR, _ := net.ParseCIDR("0.0.0.0/0")
		retVal = append(retVal, netlink.Route{
			Dst:      v4AnyCIDR,
			Table:    udng.vrfTableId,
			Priority: 4278198272,
			Type:     unix.RTN_UNREACHABLE,
		})
	}
	// link index for an unreachable IPv6 route always get set to 1. Link index 1 refers to default loopback
	// device at all time. Reference: https://docs.kernel.org/networking/vrf.html#using-iproute2-for-vrfs
	if hasV6Subnet {
		_, v6AnyCIDR, _ := net.ParseCIDR("::/0")
		retVal = append(retVal, netlink.Route{
			LinkIndex: types.LoopbackInterfaceIndex,
			Dst:       v6AnyCIDR,
			Table:     udng.vrfTableId,
			Priority:  4278198272,
			Type:      unix.RTN_UNREACHABLE,
		})
	}
	return retVal, nil
}

// computeNodeSubnetRoutes returns the node-connected subnet routes for this
// network's VRF, e.g. 192.168.x.0/24 dev br-ex scope link src 192.168.x.5.
// These let UDN egress reach nodes on the shared gateway bridge's L2 subnet
// directly (via ARP/ND) instead of falling through to the VRF default route. The
// IPs are read fresh from the gateway bridge so that runtime address changes are
// reflected. It returns nil for Uplink-backed networks (nodeSubnetBridge is nil
// for them) since their connected route is managed by the kernel via VRF
// enslavement.
func (udng *UserDefinedNetworkGateway) computeNodeSubnetRoutes() []netlink.Route {
	if udng.nodeSubnetBridge == nil {
		return nil
	}
	var routes []netlink.Route
	for _, nodeNetAddr := range udng.nodeSubnetBridge.GetIPs() {
		if nodeNetAddr == nil || nodeNetAddr.IP == nil || nodeNetAddr.Mask == nil {
			continue
		}
		// only add routes for configured IP families
		isV6 := utilnet.IsIPv6(nodeNetAddr.IP)
		if (!isV6 && !config.IPv4Mode) || (isV6 && !config.IPv6Mode) {
			continue
		}
		nodeSubnet := nodeNetAddr.IP.Mask(nodeNetAddr.Mask)
		routes = append(routes, netlink.Route{
			LinkIndex: udng.gwInterfaceIndex,
			Dst:       &net.IPNet{IP: nodeSubnet, Mask: nodeNetAddr.Mask},
			Src:       nodeNetAddr.IP,
			Table:     udng.vrfTableId,
			Scope:     netlink.SCOPE_LINK,
		})
	}
	return routes
}

// reconcileNodeSubnetRoutes programs the node-connected subnet routes into this
// network's VRF and removes any previously programmed ones that no longer apply,
// e.g. after a gateway bridge address change. It is a no-op when the desired set
// already matches what was last programmed, so ordinary reconciliations do not
// churn the routes. Callers must hold operationMutex.
func (udng *UserDefinedNetworkGateway) reconcileNodeSubnetRoutes() error {
	desired := udng.computeNodeSubnetRoutes()
	if nodeSubnetRoutesEqual(udng.nodeSubnetRoutes, desired) {
		return nil
	}
	vrfName := util.GetNetworkVRFName(udng.NetInfo)
	if len(udng.nodeSubnetRoutes) > 0 {
		if err := udng.vrfManager.DeleteVRFRoutes(vrfName, udng.nodeSubnetRoutes); err != nil {
			return fmt.Errorf("failed to delete stale node subnet routes from VRF %s for network %s: %w",
				vrfName, udng.GetNetworkName(), err)
		}
		// The stale routes are gone; forget them so a failure adding the desired
		// set below leaves the tracking consistent with the kernel state and the
		// next reconcile does not attempt to re-delete them.
		udng.nodeSubnetRoutes = nil
	}
	if len(desired) > 0 {
		if err := udng.vrfManager.AddVRFRoutes(vrfName, desired); err != nil {
			// AddVRFRoutes can install some routes before a later one fails,
			// leaving partial state in the VRF. Roll back only the routes this
			// reconcile attempted so a subsequent address removal (which would
			// compute an empty set and skip cleanup) cannot leak a stale route.
			// If the rollback itself fails, keep tracking the desired set so a
			// later address change retries the cleanup.
			if cleanupErr := udng.vrfManager.DeleteVRFRoutes(vrfName, desired); cleanupErr != nil {
				udng.nodeSubnetRoutes = desired
				return fmt.Errorf("failed to add node subnet routes to VRF %s for network %s: %w; "+
					"failed to clean up partially added routes: %v", vrfName, udng.GetNetworkName(), err, cleanupErr)
			}
			return fmt.Errorf("failed to add node subnet routes to VRF %s for network %s: %w",
				vrfName, udng.GetNetworkName(), err)
		}
	}
	udng.nodeSubnetRoutes = desired
	return nil
}

// nodeSubnetRoutesEqual reports whether two node-connected subnet route sets are
// equivalent, ignoring order.
func nodeSubnetRoutesEqual(a, b []netlink.Route) bool {
	if len(a) != len(b) {
		return false
	}
	keys := make(map[string]struct{}, len(a))
	for i := range a {
		keys[nodeSubnetRouteKey(a[i])] = struct{}{}
	}
	for i := range b {
		if _, ok := keys[nodeSubnetRouteKey(b[i])]; !ok {
			return false
		}
	}
	return true
}

func nodeSubnetRouteKey(r netlink.Route) string {
	dst := ""
	if r.Dst != nil {
		dst = r.Dst.String()
	}
	return fmt.Sprintf("%d|%s|%s|%d|%d", r.LinkIndex, dst, r.Src.String(), r.Table, r.Scope)
}

func (udng *UserDefinedNetworkGateway) getDefaultRoute() ([]netlink.Route, error) {
	networkMTU := udng.NetInfo.MTU()
	if networkMTU == 0 {
		networkMTU = config.Default.MTU
	}

	var retVal []netlink.Route
	var defaultAnyCIDR *net.IPNet
	hasV4Subnet, hasV6Subnet := udng.IPMode()
	if udng.Uplink() != "" {
		if len(udng.nextHops) == 0 {
			return nil, nil
		}
	}
	for _, nextHop := range udng.nextHops {
		isV6 := utilnet.IsIPv6(nextHop)
		if (isV6 && !hasV6Subnet) || (!isV6 && !hasV4Subnet) {
			continue
		}
		_, defaultAnyCIDR, _ = net.ParseCIDR("0.0.0.0/0")
		if isV6 {
			_, defaultAnyCIDR, _ = net.ParseCIDR("::/0")
		}
		retVal = append(retVal, netlink.Route{
			LinkIndex: udng.gwInterfaceIndex,
			Dst:       defaultAnyCIDR,
			MTU:       networkMTU,
			Gw:        nextHop,
			Table:     udng.vrfTableId,
		})
	}
	return retVal, nil
}

func (udng *UserDefinedNetworkGateway) getDefaultRouteExceptIfVRFLite() ([]netlink.Route, error) {
	// If the network is advertised on a non default VRF then we should only consider routes received from external BGP
	// device and not send any traffic based on default route similar to one present in default VRF. This is more important
	// for VRF-Lite usecase where we need traffic to leave from vlan device instead of default gateway interface.
	if udng.isNetworkAdvertised && !udng.isNetworkAdvertisedToDefaultVRF {
		return nil, nil
	}
	return udng.getDefaultRoute()
}

// getV4MasqueradeIP returns the V4 management port masqueradeIP for this network
func (udng *UserDefinedNetworkGateway) getV4MasqueradeIP() (*net.IPNet, error) {
	if !config.IPv4Mode {
		return nil, nil
	}
	masqIPs, err := udn.AllocateV4MasqueradeIPs(udng.GetNetworkID())
	if err != nil {
		return nil, fmt.Errorf("failed to allocate masquerade IPs for v4 stack for network %s: %w", udng.GetNetworkName(), err)
	}
	return util.GetIPNetFullMaskFromIP(masqIPs.ManagementPort.IP), nil
}

// getV6MasqueradeIP returns the V6 management port masqueradeIP for this network
func (udng *UserDefinedNetworkGateway) getV6MasqueradeIP() (*net.IPNet, error) {
	if !config.IPv6Mode {
		return nil, nil
	}
	masqIPs, err := udn.AllocateV6MasqueradeIPs(udng.GetNetworkID())
	if err != nil {
		return nil, fmt.Errorf("failed to allocate masquerade IPs for v6 stack for network %s: %w", udng.GetNetworkName(), err)
	}
	return util.GetIPNetFullMaskFromIP(masqIPs.ManagementPort.IP), nil
}

// constructUDNVRFIPRules constructs rules that redirect matching packets
// into the corresponding UDN VRF routing table.
//
// When a network is not advertised on the default VRF, an example of the rules
// we set for it is:
// 2000:	from all fwmark 0x1001 lookup 1007
// 2000:	from all to 169.254.0.12 lookup 1007
//
// When a network is advertised on the default VRF, an example of the rules
// we set for it is:
// 2000:	from all fwmark 0x1001 lookup 1007
// 2000:	from all to 10.132.0.0/14 lookup 1007
// 2000:	from all to 169.254.0.12 lookup 1007
func (udng *UserDefinedNetworkGateway) constructUDNVRFIPRules() ([]iprulemanager.IPRule, []iprulemanager.IPRule, error) {
	var addIPRules []iprulemanager.IPRule
	var delIPRules []iprulemanager.IPRule
	var masqIPRules []iprulemanager.IPRule
	var subnetIPRules []iprulemanager.IPRule
	masqIPv4, err := udng.getV4MasqueradeIP()
	if err != nil {
		return nil, nil, err
	}
	masqIPv6, err := udng.getV6MasqueradeIP()
	if err != nil {
		return nil, nil, err
	}

	if masqIPv4 != nil {
		addIPRules = append(addIPRules, generateIPRuleForPacketMark(udng.pktMark, false, uint(udng.vrfTableId)))
		masqIPRules = append(masqIPRules, generateIPRuleForMasqIP(masqIPv4.IP, false, uint(udng.vrfTableId)))
		for _, subnet := range udng.Subnets() {
			if utilnet.IsIPv4CIDR(subnet.CIDR) {
				subnetIPRules = append(subnetIPRules, generateIPRuleForUDNSubnet(subnet.CIDR, false, uint(udng.vrfTableId)))
			}
		}
	}
	if masqIPv6 != nil {
		addIPRules = append(addIPRules, generateIPRuleForPacketMark(udng.pktMark, true, uint(udng.vrfTableId)))
		masqIPRules = append(masqIPRules, generateIPRuleForMasqIP(masqIPv6.IP, true, uint(udng.vrfTableId)))
		for _, subnet := range udng.Subnets() {
			if utilnet.IsIPv6CIDR(subnet.CIDR) {
				subnetIPRules = append(subnetIPRules, generateIPRuleForUDNSubnet(subnet.CIDR, true, uint(udng.vrfTableId)))
			}
		}
	}
	switch {
	case udng.isNetworkAdvertisedToDefaultVRF:
		// the network is advertised to the default VRF
		addIPRules = append(addIPRules, masqIPRules...)
		addIPRules = append(addIPRules, subnetIPRules...)
	default:
		// network is not advertised on the default VRF
		addIPRules = append(addIPRules, masqIPRules...)
		delIPRules = append(delIPRules, subnetIPRules...)
	}
	return addIPRules, delIPRules, nil
}

func generateIPRuleForPacketMark(mark uint, isIPv6 bool, vrfTableId uint) iprulemanager.IPRule {
	family := netlink.FAMILY_V4
	if isIPv6 {
		family = netlink.FAMILY_V6
	}
	return iprulemanager.IPRule{
		Table:    int(vrfTableId),
		Priority: UDNMasqueradeIPRulePriority,
		Family:   family,
		Mark:     uint32(mark),
	}
}

func generateIPRuleForMasqIP(masqIP net.IP, isIPv6 bool, vrfTableId uint) iprulemanager.IPRule {
	family := netlink.FAMILY_V4
	if isIPv6 {
		family = netlink.FAMILY_V6
	}
	addr, _ := netip.AddrFromSlice(masqIP)
	addr = addr.Unmap()
	return iprulemanager.IPRule{
		Table:    int(vrfTableId),
		Priority: UDNMasqueradeIPRulePriority,
		Family:   family,
		Dst:      netip.PrefixFrom(addr, addr.BitLen()),
	}
}

func generateIPRuleForUDNSubnet(udnIP *net.IPNet, isIPv6 bool, vrfTableId uint) iprulemanager.IPRule {
	family := netlink.FAMILY_V4
	if isIPv6 {
		family = netlink.FAMILY_V6
	}
	addr, _ := netip.AddrFromSlice(udnIP.IP)
	ones, _ := udnIP.Mask.Size()
	return iprulemanager.IPRule{
		Table:    int(vrfTableId),
		Priority: UDNMasqueradeIPRulePriority,
		Family:   family,
		Dst:      netip.PrefixFrom(addr.Unmap(), ones),
	}
}

// Stop prevents queued Uplink or network events from racing with terminal
// cleanup and waits until both per-UDN reconcilers have exited.
func (udng *UserDefinedNetworkGateway) Stop() {
	udng.unregisterUDNAddressChangeReconciler(udng.GetNetworkName())
	controllers := make([]controllerutil.Reconciler, 0, 2)
	if udng.gatewayReconciler != nil {
		controllers = append(controllers, udng.gatewayReconciler)
	}
	if udng.uplinkStateController != nil {
		controllers = append(controllers, udng.uplinkStateController)
	}
	controllerutil.Stop(controllers...)

	// Taken only once the reconcilers above have drained, because they hold
	// this mutex while they run and they are what publishes the controller.
	udng.operationMutex.Lock()
	defer udng.operationMutex.Unlock()
	udng.mgmtPortController.Stop()
}

// Reconcile signals reconcileAdvertisedState for advertised-state updates
// (VRF, isolation, DEFAULT OpenFlow). It does not implement Gateway.Reconcile:
// the signature has no error, and it must not be confused with the embedded
// *gateway.Reconcile which resyncs services.
func (udng *UserDefinedNetworkGateway) Reconcile() {
	udng.gatewayReconciler.Reconcile(udng.GetNetworkName())
}

func (udng *UserDefinedNetworkGateway) reconcileGateway(_ string) error {
	udng.operationMutex.Lock()
	defer udng.operationMutex.Unlock()
	if udng.Uplink() == "" {
		return udng.reconcileAdvertisedState()
	}
	// Reconcile and report against the UplinkState configuration last handled by
	// this gateway, not a newer informer object that has not been handled yet.
	reconcileErr := udng.reconcileAdvertisedState()
	udng.uplinkStateGatewayStatusController.ReportNetworkResult(
		udng.NetInfo, udng.uplinkStateUID, udng.uplinkFingerprint, reconcileErr)
	return reconcileErr
}

// reconcileAdvertisedState reapplies the mutable gateway state affected by
// RouteAdvertisements. Initial ownership and cache registration remain in
// addNetworkWithResolvedUplink.
func (udng *UserDefinedNetworkGateway) reconcileAdvertisedState() error {
	klog.Infof("Reconciling gateway with updates for UDN %s", udng.GetNetworkName())

	if config.IsModeDPU() || config.IsModeFull() {
		// shouldn't happen
		if udng.openflowManager == nil || udng.openflowManager.defaultBridge == nil {
			return fmt.Errorf("openflow manager with default bridge configuration has not been provided for network %s", udng.GetNetworkName())
		}
	}

	udng.updateAdvertisementStatus()

	if config.IsModeDPU() || config.IsModeFull() {
		// update bridge configuration
		netConfig := udng.openflowManager.getNetworkConfig(udng.NetInfo)
		if netConfig == nil {
			return fmt.Errorf("missing bridge configuration for network %s", udng.GetNetworkName())
		}
		netConfig.Advertised.Store(udng.isNetworkAdvertised)
	}

	if config.IsModeDPU() && udng.Uplink() != "" {
		vrfDeviceName := util.GetNetworkVRFName(udng.NetInfo)
		if err := udng.reconcileUplinkGatewayVRFSlave(vrfDeviceName); err != nil {
			return err
		}
	}

	if config.IsModeDPUHost() || config.IsModeFull() {
		vrfDeviceName := util.GetNetworkVRFName(udng.NetInfo)
		if err := udng.reconcileUplinkGatewayVRFSlave(vrfDeviceName); err != nil {
			return err
		}
		if err := udng.applyAdvertisedRoutingState(); err != nil {
			return err
		}
	}

	if config.IsModeDPU() || config.IsModeFull() {
		// add below OpenFlows based on the gateway mode and whether the network is advertised or not:
		// table=1, n_packets=0, n_bytes=0, priority=16,ip,nw_dst=128.192.0.2 actions=LOCAL (Both gateway modes)
		// table=1, n_packets=0, n_bytes=0, priority=15,ip,nw_dst=128.192.0.0/14 actions=output:3 (shared gateway mode)
		// necessary service isolation flows based on whether network is advertised or not
		if err := udng.openflowManager.updateBridgeFlowCache(udng.nodeIPManager.ListAddresses()); err != nil {
			return fmt.Errorf("error while updating logical flow for UDN %s: %s", udng.GetNetworkName(), err)
		}
		// let's sync these flows immediately
		udng.openflowManager.requestFlowSync()
	}

	if config.IsModeDPUHost() || config.IsModeFull() {
		if err := udng.updateAdvertisedUDNIsolationRules(); err != nil {
			return fmt.Errorf("error while updating advertised UDN isolation rules for network %s: %w", udng.GetNetworkName(), err)
		}
	}
	if udng.Uplink() != "" && (config.IsModeDPU() || config.IsModeFull()) {
		if err := udng.syncUplinkBridgeFlows(); err != nil {
			return err
		}
	}
	return nil
}

// applyAdvertisedRoutingState updates the host routing state that depends on
// RouteAdvertisements. The caller must refresh the advertisement flags and
// ensure the network VRF exists before calling it.
func (udng *UserDefinedNetworkGateway) applyAdvertisedRoutingState() error {
	if err := udng.updateUDNVRFIPRules(); err != nil {
		return fmt.Errorf("failed to update IP rules for network %s: %w", udng.GetNetworkName(), err)
	}
	if err := udng.updateUDNVRFIPRoute(); err != nil {
		return fmt.Errorf("failed to update IP routes for network %s: %w", udng.GetNetworkName(), err)
	}
	if err := udng.reconcileNodeSubnetRoutes(); err != nil {
		return fmt.Errorf("failed to update node subnet routes for network %s: %w", udng.GetNetworkName(), err)
	}
	return nil
}

func (udng *UserDefinedNetworkGateway) syncUplinkBridgeFlows() error {
	found, err := udng.openflowManager.syncUplinkBridgeFlows(udng.openflowBridgeName)
	if err != nil {
		return newUplinkGatewayError(uplinkv1alpha1.UplinkStateReasonGatewayProgrammingFailed, err)
	}
	if !found {
		return newUplinkGatewayError(uplinkv1alpha1.UplinkStateReasonGatewayProgrammingFailed,
			fmt.Errorf("uplink bridge %s not found", udng.openflowBridgeName))
	}
	return nil
}

func dpuUDNVRFRouteTableID(networkID int) int {
	return dpuUDNVRFRouteTableIDStart + networkID
}

func (udng *UserDefinedNetworkGateway) ensureDPUVRF() error {
	vrfDeviceName := util.GetNetworkVRFName(udng.NetInfo)
	vrfTableID := dpuUDNVRFRouteTableID(udng.GetNetworkID())
	udng.vrfTableId = vrfTableID
	if err := udng.vrfManager.AddVRF(vrfDeviceName, "", uint32(vrfTableID), nil); err != nil {
		return fmt.Errorf("could not add DPU VRF %d for network %s, err: %v",
			vrfTableID, udng.GetNetworkName(), err)
	}
	return nil
}

func (udng *UserDefinedNetworkGateway) shouldEnslaveUplinkGatewayToVRF() bool {
	return udng.Uplink() != "" &&
		udng.isNetworkAdvertised &&
		!udng.isNetworkAdvertisedToDefaultVRF
}

func (udng *UserDefinedNetworkGateway) reconcileUplinkGatewayVRFSlave(vrfDeviceName string) error {
	if udng.Uplink() == "" || udng.gwInterfaceName == "" {
		return nil
	}

	if udng.shouldEnslaveUplinkGatewayToVRF() {
		// Remove the managed default routes before enslaving the Uplink
		// gateway interface: enslavement preserves the interface's
		// pre-existing routes into the VRF table, and a still-managed
		// default route with the same key would otherwise clobber the
		// preserved default route, or absorb its addition only to be
		// removed right after. Managed default routes only exist on the
		// host side, so this does not apply to DPU mode.
		if config.IsModeDPUHost() || config.IsModeFull() {
			if err := udng.removeManagedDefaultRoutesFromVRF(); err != nil {
				return newUplinkGatewayError(
					uplinkv1alpha1.UplinkStateReasonVRFAttachmentFailed,
					fmt.Errorf("could not remove managed default routes from VRF %s for network %s: %w",
						vrfDeviceName, udng.GetNetworkName(), err),
				)
			}
		}
		// Whenever the kernel supports IPv6, regardless of the cluster IP
		// families: enslavement captures and restores routes of both
		// families, and without this sysctl the kernel flushes the
		// interface's IPv6 addresses on the master change, making any
		// captured IPv6 route fail its restore.
		if util.IsIPv6SysctlSupported() {
			if err := util.SetIPv6KeepAddrOnDownForInterface(udng.gwInterfaceName); err != nil {
				return newUplinkGatewayError(
					uplinkv1alpha1.UplinkStateReasonVRFAttachmentFailed,
					fmt.Errorf("could not preserve IPv6 addresses on Uplink gateway interface %s for network %s: %w",
						udng.gwInterfaceName, udng.GetNetworkName(), err),
				)
			}
		}
		if err := udng.vrfManager.AddVRFSlave(vrfDeviceName, udng.gwInterfaceName); err != nil {
			return newUplinkGatewayError(
				uplinkVRFAttachmentFailureReason(err),
				fmt.Errorf("could not add Uplink gateway interface %s to VRF %s for network %s: %w",
					udng.gwInterfaceName, vrfDeviceName, udng.GetNetworkName(), err),
			)
		}
		return nil
	}

	if err := udng.vrfManager.DeleteVRFSlave(vrfDeviceName, udng.gwInterfaceName); err != nil {
		return newUplinkGatewayError(
			uplinkv1alpha1.UplinkStateReasonVRFAttachmentFailed,
			fmt.Errorf("could not remove Uplink gateway interface %s from VRF %s for network %s: %w",
				udng.gwInterfaceName, vrfDeviceName, udng.GetNetworkName(), err),
		)
	}
	return nil
}

func uplinkVRFAttachmentFailureReason(err error) string {
	var conflict *vrfmanager.VRFSlaveConflictError
	if errors.As(err, &conflict) {
		return uplinkv1alpha1.UplinkStateReasonConfigurationConflict
	}
	return uplinkv1alpha1.UplinkStateReasonVRFAttachmentFailed
}

// updateUDNVRFIPRules updates IP rules for a network depending on whether the
// network is advertised to the default VRF or not
func (udng *UserDefinedNetworkGateway) updateUDNVRFIPRules() error {
	addIPRules, deleteIPRules, err := udng.constructUDNVRFIPRules()
	if err != nil {
		return fmt.Errorf("unable to get iprules for network %s, err: %v", udng.GetNetworkName(), err)
	}

	for _, rule := range addIPRules {
		if err = udng.ruleManager.AddWithMetadata(rule, udng.GetNetworkRuleMetadata()); err != nil {
			return fmt.Errorf("unable to create iprule %v for network %s, err: %v", rule, udng.GetNetworkName(), err)
		}
	}
	for _, rule := range deleteIPRules {
		if err = udng.ruleManager.Delete(rule); err != nil {
			return fmt.Errorf("unable to delete iprule for network %s, err: %v", udng.GetNetworkName(), err)
		}
	}
	return nil
}

// Add or remove default route from a vrf device based on the network is
// advertised on its own network or the default network
func (udng *UserDefinedNetworkGateway) updateUDNVRFIPRoute() error {
	vrfName := util.GetNetworkVRFName(udng.NetInfo)

	switch {
	case udng.isNetworkAdvertised && !udng.isNetworkAdvertisedToDefaultVRF:
		// Remove default route for networks advertised to non-default VRF
		if err := udng.removeManagedDefaultRoutesFromVRF(); err != nil {
			return fmt.Errorf("failed to remove default route from VRF %s for network %s: %v",
				vrfName, udng.GetNetworkName(), err)
		}

	default:
		// Add default route for networks that are either:
		// - not advertised
		// - advertised to default VRF
		defaultRoute, err := udng.getDefaultRouteExceptIfVRFLite()
		if err != nil {
			return fmt.Errorf("failed to get default route for network %s: %v",
				udng.GetNetworkName(), err)
		}

		if err = udng.vrfManager.AddVRFRoutes(vrfName, defaultRoute); err != nil {
			return fmt.Errorf("failed to add default route to VRF %s for network %s: %v",
				vrfName, udng.GetNetworkName(), err)
		}
	}

	return nil
}

// removeManagedDefaultRoutesFromVRF removes the OVN-Kubernetes-managed
// default routes from the network's VRF table. A managed default route only
// belongs in the VRF while the network follows the default routing domain:
//
//   - The host's own default route (typically DHCP-provided, not ours) is the
//     next hop OVN uses to leave via the gateway bridge.
//   - Networks that follow the default routing domain get a managed *copy* of
//     that route in their VRF table, so their traffic leaks into it.
//   - Networks advertised to their own VRF with an Uplink instead have the
//     Uplink interface's own default route *migrated* into the VRF table by
//     the enslavement, unmanaged; the managed copy must be removed first so
//     that it cannot clobber the migrated route.
//
// The routes to remove are matched in the kernel by the OVN-Kubernetes
// protocol rather than recomputed from the current gateway interface, so that
// a route installed for a previous gateway interface is removed as well.
func (udng *UserDefinedNetworkGateway) removeManagedDefaultRoutesFromVRF() error {
	vrfDeviceName := util.GetNetworkVRFName(udng.NetInfo)
	filter := &netlink.Route{Table: udng.vrfTableId}
	routes, err := util.GetNetLinkOps().RouteListFiltered(netlink.FAMILY_ALL, filter, netlink.RT_FILTER_TABLE)
	if err != nil {
		return fmt.Errorf("unable to list routes of VRF table %d for network %s, err: %v",
			udng.vrfTableId, udng.GetNetworkName(), err)
	}
	var managedDefaultRoutes []netlink.Route
	for _, route := range routes {
		if int(route.Protocol) != types.OVNKProtocol || len(route.Gw) == 0 || route.Dst != nil && route.Dst.IP != nil && !route.Dst.IP.IsUnspecified() {
			continue
		}
		// The kernel reports a default route with a nil destination; the
		// tracked routes carry the explicit any CIDR of their family.
		_, anyCIDR, _ := net.ParseCIDR("0.0.0.0/0")
		if utilnet.IsIPv6(route.Gw) {
			_, anyCIDR, _ = net.ParseCIDR("::/0")
		}
		route.Dst = anyCIDR
		managedDefaultRoutes = append(managedDefaultRoutes, route)
	}
	if len(managedDefaultRoutes) == 0 {
		return nil
	}
	if err = udng.vrfManager.DeleteVRFRoutes(vrfDeviceName, managedDefaultRoutes); err != nil {
		return fmt.Errorf("unable to delete routes for network %s, err: %v", udng.GetNetworkName(), err)
	}
	return nil
}

// updateAdvertisedUDNIsolationRules adds the full UDN subnets to nftablesAdvertisedUDNsSetV[4|6] nft set that is used
// in the following chain/rules to drop locally generated traffic towards a UDN network:
//
//	chain udn-bgp-drop {
//	  comment "Drop traffic generated locally towards advertised UDN subnets"
//	  type filter hook output priority filter; policy accept;
//	  ip daddr @advertised-udn-subnets-v4 counter packets 0 bytes 0 drop
//	  ip6 daddr @advertised-udn-subnets-v6 counter packets 0 bytes 0 drop
//	}
//
// It blocks access to the full UDN subnet to handle a case in L3 when a node tries to access
// a host subnet available on a different node. Example set entries:
//
//	 set advertised-udn-subnets-v4 {
//	   type ipv4_addr
//	   flags interval
//	   comment "advertised UDNs V4 subnets"
//	   elements = { 10.10.0.0/16 comment "cluster_udn_l3network" }
//	}
func (udng *UserDefinedNetworkGateway) updateAdvertisedUDNIsolationRules() error {
	switch {
	case udng.isNetworkAdvertised:
		return udng.addAdvertisedUDNIsolationRules()
	default:
		return udng.deleteAdvertisedUDNIsolationRules()
	}
}

func (udng *UserDefinedNetworkGateway) addAdvertisedUDNIsolationRules() error {
	nft, err := nodenft.GetNFTablesHelper()
	if err != nil {
		return fmt.Errorf("failed to get nftables helper: %v", err)
	}
	tx := nft.NewTransaction()

	for _, udnNet := range udng.Subnets() {
		set := nftablesAdvertisedUDNsSetV4
		if utilnet.IsIPv6CIDR(udnNet.CIDR) {
			set = nftablesAdvertisedUDNsSetV6
		}
		tx.Add(&knftables.Element{
			Set:     set,
			Key:     []string{udnNet.CIDR.String()},
			Comment: knftables.PtrTo(udng.GetNetworkName()),
		})

	}

	if tx.NumOperations() == 0 {
		return nil
	}
	return nft.Run(context.TODO(), tx)
}

func (udng *UserDefinedNetworkGateway) deleteAdvertisedUDNIsolationRules() error {
	nft, err := nodenft.GetNFTablesHelper()
	if err != nil {
		return fmt.Errorf("failed to get nftables helper: %v", err)
	}
	tx := nft.NewTransaction()

	existingV4, err := nft.ListElements(context.TODO(), "set", nftablesAdvertisedUDNsSetV4)
	if err != nil {
		if !knftables.IsNotFound(err) {
			return fmt.Errorf("could not list existing items in %s set: %w", nftablesAdvertisedUDNsSetV4, err)
		}
	}
	existingV6, err := nft.ListElements(context.TODO(), "set", nftablesAdvertisedUDNsSetV6)
	if err != nil {
		if !knftables.IsNotFound(err) {
			return fmt.Errorf("could not list existing items in %s set: %w", nftablesAdvertisedUDNsSetV6, err)
		}
	}

	for _, elem := range append(existingV4, existingV6...) {
		if elem.Comment != nil && *elem.Comment == udng.GetNetworkName() {
			tx.Delete(elem)
		}
	}

	if tx.NumOperations() == 0 {
		return nil
	}
	return nft.Run(context.TODO(), tx)
}

func (udng *UserDefinedNetworkGateway) updateAdvertisementStatus() {
	udng.isNetworkAdvertised = util.IsPodNetworkAdvertisedAtNode(udng.NetInfo, udng.node.Name)
	udng.isNetworkAdvertisedToDefaultVRF = util.IsPodNetworkAdvertisedAtNodeDefaultVRF(udng.NetInfo, udng.node.Name)
}
