// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package uplink

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"golang.org/x/time/rate"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
	"k8s.io/utils/ptr"

	libovsdbcache "github.com/ovn-kubernetes/libovsdb/cache"
	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	libovsdbmodel "github.com/ovn-kubernetes/libovsdb/model"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	controllerutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	uplinkv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1"
	uplinkapply "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/applyconfiguration/uplink/v1alpha1"
	uplinkclientset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/clientset/versioned"
	uplinklisters "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/listers/uplink/v1alpha1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	ovsops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops/ovs"
	nodeutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/util"
	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	uplinkutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/uplink"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
)

const (
	fieldManager         = "ovnkube-node-uplink-controller"
	dpuFieldManager      = "ovnkube-node-uplink-controller-dpu"
	dpuHostFieldManager  = "ovnkube-node-uplink-controller-dpu-host"
	ovsIntegrationBridge = "br-int"

	// missingDefaultGatewayRepollInterval paces the rediscovery of an
	// UplinkState whose host interface is missing a default gateway route
	// for one of its address families (see repollMissingDefaultGateways).
	missingDefaultGatewayRepollInterval = 30 * time.Second

	// maxDefaultGateways matches the status.defaultGateways CRD bound: room
	// for 128 next hops per family. Overflow fails discovery instead of
	// publishing an arbitrary subset. See the Uplink feature documentation.
	maxDefaultGateways = 256
)

type hostInterfaceState struct {
	macAddress      net.HardwareAddr
	ipAddresses     []*net.IPNet
	defaultGateways []net.IP
	// hostFunction identifies the SR-IOV function backing the host
	// interface (a PF, or a VF on that PF); nil when the interface is
	// neither or its indices cannot be resolved.
	hostFunction *uplinkv1alpha1.HostFunction
}

type hostInterfaceDiscoverer interface {
	Discover(hostInterfaceName string) (*hostInterfaceState, error)
}

type ovsBridgeResolver interface {
	// Resolve finds the OVS bridge for a host interface in Full mode.
	Resolve(hostInterfaceName string) (string, error)
	// ResolveByHostFunction finds the OVS bridge holding the representor of
	// the host PF or VF identified by its host function in DPU mode.
	// details must be non-nil. The resolved representor must peer with
	// hostMAC: the published MAC is the uplink's identity, so indices that
	// resolve a representor of some other host function are an error, not
	// a match.
	ResolveByHostFunction(details *uplinkv1alpha1.HostFunction, hostMAC net.HardwareAddr, nodeName string) (string, error)
	// ResolveByHostMAC finds the OVS bridge by its host-side MAC in DPU mode.
	ResolveByHostMAC(hostMAC net.HardwareAddr, nodeName string) (string, error)
	// BridgeUplink returns the physical uplink port attached to an OVS bridge.
	BridgeUplink(bridgeName string) (string, error)
}

type discoveryError struct {
	reason string
	err    error
}

func (e *discoveryError) Error() string {
	return e.err.Error()
}

func newDiscoveryError(reason string, err error) error {
	return &discoveryError{reason: reason, err: err}
}

// Controller publishes UplinkState discovery status for this node.
type Controller struct {
	nodeName string

	uplinkClient      uplinkclientset.Interface
	uplinkLister      uplinklisters.UplinkLister
	uplinkStateLister uplinklisters.UplinkStateLister
	nodeLister        corelisters.NodeLister

	hostDiscoverer hostInterfaceDiscoverer
	bridgeResolver ovsBridgeResolver
	// ovsClient is nil where the node has no OVS (DPU host).
	ovsClient libovsdbclient.Client
	// ovsEventsActive gates the OVSDB callback, which cannot be unregistered:
	// events arriving after Stop must not touch the stopped controllers.
	ovsEventsActive atomic.Bool

	uplinkController      controllerutil.Controller
	uplinkStateController controllerutil.Controller
	nodeController        controllerutil.Controller
}

// discoveryRateLimiter is the default controller rate limiter with its
// exponential backoff capped at 30s instead of 1000s: retries re-poll
// admin-owned host and OVS state, so the cap bounds how long discovery
// takes to notice an admin fix.
func discoveryRateLimiter() workqueue.TypedRateLimiter[string] {
	return workqueue.NewTypedMaxOfRateLimiter(
		workqueue.NewTypedItemExponentialFailureRateLimiter[string](5*time.Millisecond, 30*time.Second),
		&workqueue.TypedBucketRateLimiter[string]{Limiter: rate.NewLimiter(rate.Limit(10), 100)},
	)
}

// NewController creates the ovnkube-node Uplink discovery controller.
func NewController(nodeName string, wf factory.NodeWatchFactory, ovnClient *util.OVNNodeClientset, ovsClient libovsdbclient.Client) *Controller {
	c := &Controller{
		nodeName:          nodeName,
		uplinkClient:      ovnClient.UplinkClient,
		uplinkLister:      wf.UplinkInformer().Lister(),
		uplinkStateLister: wf.UplinkStateInformer().Lister(),
		nodeLister:        wf.NodeCoreInformer().Lister(),
		hostDiscoverer:    netlinkHostInterfaceDiscoverer{},
		bridgeResolver:    defaultOVSBridgeResolver{ovsClient: ovsClient},
		ovsClient:         ovsClient,
	}

	uplinkCfg := &controllerutil.ControllerConfig[uplinkv1alpha1.Uplink]{
		RateLimiter:    discoveryRateLimiter(),
		MaxAttempts:    controllerutil.InfiniteAttempts,
		Informer:       wf.UplinkInformer().Informer(),
		Lister:         c.uplinkLister.List,
		Reconcile:      c.reconcileUplink,
		ObjNeedsUpdate: uplinkNeedsUpdate,
		Threadiness:    1,
	}
	c.uplinkController = controllerutil.NewController(
		"ovnkube-node-uplink-controller",
		uplinkCfg,
	)

	uplinkStateCfg := &controllerutil.ControllerConfig[uplinkv1alpha1.UplinkState]{
		RateLimiter:    discoveryRateLimiter(),
		MaxAttempts:    controllerutil.InfiniteAttempts,
		Informer:       wf.UplinkStateInformer().Informer(),
		Lister:         c.uplinkStateLister.List,
		Reconcile:      c.reconcileUplinkState,
		ObjNeedsUpdate: uplinkStateNeedsUpdate,
		Threadiness:    1,
	}
	c.uplinkStateController = controllerutil.NewController(
		"ovnkube-node-uplink-state-controller",
		uplinkStateCfg,
	)

	nodeCfg := &controllerutil.ControllerConfig[corev1.Node]{
		RateLimiter:    discoveryRateLimiter(),
		MaxAttempts:    controllerutil.InfiniteAttempts,
		Informer:       wf.NodeCoreInformer().Informer(),
		Lister:         c.nodeLister.List,
		Reconcile:      c.reconcileNode,
		ObjNeedsUpdate: c.nodeNeedsUpdate,
		Threadiness:    1,
	}
	c.nodeController = controllerutil.NewController(
		"ovnkube-node-uplink-node-controller",
		nodeCfg,
	)

	return c
}

func (c *Controller) Start() error {
	err := controllerutil.Start(
		c.uplinkController,
		c.uplinkStateController,
		c.nodeController,
	)
	if err != nil {
		return err
	}
	if c.ovsClient != nil {
		c.ovsEventsActive.Store(true)
		c.registerOVSBridgeEventHandler()
	}
	klog.Infof("OVN-Kubernetes node Uplink controller started")
	return nil
}

// registerOVSBridgeEventHandler re-runs discovery when an OVS bridge is added
// or deleted: discovery has no other notification of OVSDB changes, and an
// admin may delete or recreate an Uplink bridge at any time.
func (c *Controller) registerOVSBridgeEventHandler() {
	c.ovsClient.Cache().AddEventHandler(&libovsdbcache.EventHandlerFuncs{
		AddFunc: func(table string, row libovsdbmodel.Model) {
			c.handleOVSBridgeEvent(table, row)
		},
		DeleteFunc: func(table string, row libovsdbmodel.Model) {
			c.handleOVSBridgeEvent(table, row)
		},
	})
}

// handleOVSBridgeEvent re-runs discovery for every UplinkState of this node
// (the informer is filtered on spec.nodeName). Bridge additions and deletions
// are rare admin actions, and a reconnecting OVSDB client that repopulates
// its cache reports surviving bridges as additions only, so a bridge deleted
// meanwhile is found by rediscovering everything rather than by matching names.
func (c *Controller) handleOVSBridgeEvent(table string, row libovsdbmodel.Model) {
	if table != vswitchd.BridgeTable || !c.ovsEventsActive.Load() {
		return
	}
	if _, ok := row.(*vswitchd.Bridge); !ok {
		return
	}
	c.uplinkStateController.ReconcileAll()
}

func (c *Controller) Stop() {
	c.ovsEventsActive.Store(false)
	controllerutil.Stop(
		c.uplinkController,
		c.uplinkStateController,
		c.nodeController,
	)
}

func (c *Controller) reconcileUplink(key string) error {
	uplink, err := c.uplinkLister.Get(key)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return c.deleteUplinkState(uplinkutil.StateName(key, c.nodeName))
		}
		return fmt.Errorf("failed to get Uplink %s: %w", key, err)
	}

	node, err := c.nodeLister.Get(c.nodeName)
	if err != nil {
		return fmt.Errorf("failed to get local node %s: %w", c.nodeName, err)
	}

	nodeConfig, nodeConfigErr := selectedNodeConfigForNode(uplink, node)
	if nodeConfigErr == nil && nodeConfig == nil {
		return c.deleteUplinkState(uplinkutil.StateName(uplink.Name, c.nodeName))
	}

	state, err := c.ensureUplinkState(uplink, nodeConfig, nodeConfigErr)
	if err != nil {
		return err
	}
	if state == nil {
		return nil
	}
	if nodeConfigErr != nil {
		return errors.Join(nodeConfigErr, c.updateUplinkStateStatus(
			state,
			string(state.Status.HostInterfaceName),
			nil,
			"",
			metav1.ConditionFalse,
			discoveryReason(nodeConfigErr),
			nodeConfigErr.Error(),
		))
	}
	// Creation also produces an UplinkState watch event, but existing states
	// need explicit rediscovery when the selected Uplink config changes.
	c.uplinkStateController.Reconcile(state.Name)
	return nil
}

func (c *Controller) reconcileNode(key string) error {
	// Delete events bypass nodeNeedsUpdate, so filter remote node deletes here.
	if key != c.nodeName {
		return nil
	}
	c.uplinkController.ReconcileAll()
	return nil
}

func (c *Controller) reconcileUplinkState(key string) error {
	state, err := c.uplinkStateLister.Get(key)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return c.reconcileOwnerOfDeletedUplinkState(key)
		}
		return fmt.Errorf("failed to get UplinkState %s: %w", key, err)
	}
	uplinkName, _ := uplinkutil.StateIdentity(state)

	uplink, err := c.uplinkLister.Get(uplinkName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to get Uplink %s: %w", uplinkName, err)
	}

	node, err := c.nodeLister.Get(c.nodeName)
	if err != nil {
		return fmt.Errorf("failed to get local node %s: %w", c.nodeName, err)
	}
	nodeConfig, err := selectedNodeConfigForNode(uplink, node)
	if err != nil {
		return errors.Join(err, c.updateUplinkStateStatus(
			state,
			string(state.Status.HostInterfaceName),
			nil,
			"",
			metav1.ConditionFalse,
			discoveryReason(err),
			err.Error(),
		))
	}
	if nodeConfig == nil {
		return c.deleteUplinkState(state.Name)
	}

	hostInterfaceName := string(nodeConfig.HostInterfaceName)
	var hostState *hostInterfaceState
	if config.OvnKubeNode.Mode == ovntypes.NodeModeDPU {
		hostState, err = hostInterfaceStateFromStatus(state, hostInterfaceName)
		if err != nil {
			return errors.Join(err, c.updateUplinkStateStatus(
				state,
				hostInterfaceName,
				nil,
				"",
				metav1.ConditionFalse,
				uplinkv1alpha1.UplinkStateReasonWaitingForDPUHost,
				err.Error(),
			))
		}
	} else {
		hostState, err = c.hostDiscoverer.Discover(hostInterfaceName)
		if err != nil {
			return errors.Join(err, c.updateUplinkStateStatus(
				state,
				hostInterfaceName,
				nil,
				"",
				metav1.ConditionFalse,
				discoveryReason(err),
				err.Error(),
			))
		}
	}

	if config.OvnKubeNode.Mode == ovntypes.NodeModeDPUHost {
		// Host interface discovery succeeded, which is everything the
		// DPU-host reports: publish HostDataReady=True. Bridge resolution,
		// validation and the Resolved condition belong to the DPU side.
		c.repollMissingDefaultGateways(state.Name, hostInterfaceName, hostState)
		return c.updateUplinkStateStatus(
			state,
			hostInterfaceName,
			hostState,
			"",
			metav1.ConditionTrue,
			uplinkv1alpha1.UplinkStateReasonHostDataDiscovered,
			"Uplink host interface data discovered",
		)
	}

	defaultBridgeName, err := defaultGatewayBridgeName(node)
	if err != nil {
		return errors.Join(err, c.updateUplinkStateStatus(
			state,
			hostInterfaceName,
			hostState,
			"",
			metav1.ConditionFalse,
			discoveryReason(err),
			err.Error(),
		))
	}

	if config.OvnKubeNode.Mode == ovntypes.NodeModeDPU {
		// The published host function is a hint that resolves the
		// representor and its bridge directly; a resolved representor is
		// verified against the published host MAC, the uplink's identity.
		// The host MAC scan over every bridge is the authoritative path:
		// it serves DPU-hosts that publish no host function (non-SR-IOV
		// host interface, or an older DPU-host) and is the fallback when
		// the hint does not resolve, so a published host function can
		// never break an uplink the MAC scan would resolve.
		var bridgeName string
		// The Resolved message names the resolution method so misbehavior of
		// one method is observable from the UplinkState alone: the methods
		// resolve the same bridge, so nothing else distinguishes them.
		resolvedVia := "host MAC"
		if hostState.hostFunction != nil {
			bridgeName, err = c.bridgeResolver.ResolveByHostFunction(
				hostState.hostFunction, hostState.macAddress, c.nodeName)
			if err != nil {
				klog.Warningf("UplinkState %s: host function %s did not resolve an OVS bridge, "+
					"falling back to the host MAC scan: %v",
					state.Name, util.HostFunctionName(hostState.hostFunction.PFID, hostState.hostFunction.VFID), err)
				bridgeName, err = c.bridgeResolver.ResolveByHostMAC(hostState.macAddress, c.nodeName)
			} else {
				resolvedVia = "host function"
			}
		} else {
			bridgeName, err = c.bridgeResolver.ResolveByHostMAC(hostState.macAddress, c.nodeName)
		}
		if err != nil {
			return errors.Join(err, c.updateUplinkStateStatus(
				state,
				hostInterfaceName,
				hostState,
				"",
				metav1.ConditionFalse,
				discoveryReason(err),
				err.Error(),
			))
		}
		if err := c.validateBridgeUplink(bridgeName, hostInterfaceName, defaultBridgeName, false); err != nil {
			return errors.Join(err, c.updateUplinkStateStatus(
				state,
				hostInterfaceName,
				hostState,
				bridgeName,
				metav1.ConditionFalse,
				discoveryReason(err),
				err.Error(),
			))
		}
		return c.updateResolvedUplinkStateStatus(
			state,
			hostInterfaceName,
			hostState,
			bridgeName,
			fmt.Sprintf("Uplink DPU bridge discovery succeeded via %s", resolvedVia),
		)
	}

	bridgeName, err := c.bridgeResolver.Resolve(hostInterfaceName)
	if err != nil {
		return errors.Join(err, c.updateUplinkStateStatus(
			state,
			hostInterfaceName,
			hostState,
			"",
			metav1.ConditionFalse,
			discoveryReason(err),
			err.Error(),
		))
	}
	if err := c.validateBridgeUplink(bridgeName, hostInterfaceName, defaultBridgeName, true); err != nil {
		return errors.Join(err, c.updateUplinkStateStatus(
			state,
			hostInterfaceName,
			hostState,
			bridgeName,
			metav1.ConditionFalse,
			discoveryReason(err),
			err.Error(),
		))
	}

	c.repollMissingDefaultGateways(state.Name, hostInterfaceName, hostState)
	return c.updateResolvedUplinkStateStatus(
		state,
		hostInterfaceName,
		hostState,
		bridgeName,
		"Uplink discovery succeeded",
	)
}

// repollMissingDefaultGateways schedules a delayed rediscovery while an IP
// family the host interface has an address for is missing its default
// gateway: netlink route events do not currently enqueue Uplink discovery.
// Quiet by design —
// an uplink without a default gateway is valid, so this is not an error and
// no condition degrades; the empty status.defaultGateways field is the
// signal. Only the netlink-discovering side re-polls: the DPU reads host
// data from the UplinkState and is reconciled by its status updates.
func (c *Controller) repollMissingDefaultGateways(
	stateName, hostInterfaceName string,
	hostState *hostInterfaceState,
) {
	missing := missingDefaultGatewayFamilies(hostState)
	if missing == "" {
		return
	}
	klog.V(5).Infof("UplinkState %s: no %s default gateway route found for host interface %s, re-polling",
		stateName, missing, hostInterfaceName)
	c.uplinkStateController.ReconcileAfter(stateName, missingDefaultGatewayRepollInterval)
}

// missingDefaultGatewayFamilies names the IP families ("IPv4", "IPv6" or
// "IPv4/IPv6") the host interface has an address but no default gateway for,
// or "" if none.
func missingDefaultGatewayFamilies(hostState *hostInterfaceState) string {
	wantV4, wantV6 := false, false
	for _, ipAddress := range hostState.ipAddresses {
		if utilnet.IsIPv6CIDR(ipAddress) {
			wantV6 = true
		} else {
			wantV4 = true
		}
	}
	for _, gateway := range hostState.defaultGateways {
		if utilnet.IsIPv6(gateway) {
			wantV6 = false
		} else {
			wantV4 = false
		}
	}
	switch {
	case wantV4 && wantV6:
		return "IPv4/IPv6"
	case wantV4:
		return "IPv4"
	case wantV6:
		return "IPv6"
	}
	return ""
}

// reconcileOwnerOfDeletedUplinkState reacts to an UplinkState deletion: the deletion
// generates no event on the Uplink that owns it, so reconcile that Uplink
// here to recreate the UplinkState.
func (c *Controller) reconcileOwnerOfDeletedUplinkState(key string) error {
	uplinks, err := c.uplinkLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list Uplinks: %w", err)
	}
	uplink, found := uplinkutil.UplinkForState(uplinks, []string{c.nodeName}, key)
	if !found {
		return nil
	}
	// The cluster-manager finalizer flow deletes the UplinkStates of a
	// terminating Uplink before releasing it; don't recreate them.
	if !uplink.DeletionTimestamp.IsZero() {
		return nil
	}
	klog.Infof("UplinkState %s was deleted, reconciling Uplink %s to recreate it", key, uplink.Name)
	// Rate-limited so recreation backs off while the informer cache still
	// misses a just-created UplinkState. Successful recreations reset the
	// per-key backoff, so a persistent deleter is only throttled to the
	// rate limiter's sustained token-bucket rate.
	c.uplinkController.ReconcileRateLimited(uplink.Name)
	return nil
}

func selectedNodeConfigForNode(
	uplink *uplinkv1alpha1.Uplink,
	node *corev1.Node,
) (*uplinkv1alpha1.UplinkNodeConfig, error) {
	selected, overlapping, err := uplinkutil.SelectNodeConfig(uplink, node)
	if err != nil {
		return nil, err
	}
	if overlapping {
		return nil, newDiscoveryError(
			uplinkv1alpha1.UplinkStateReasonNodeSelectorOverlap,
			fmt.Errorf("multiple Uplink %q nodeConfigs select node %q", uplink.Name, node.Name),
		)
	}
	return selected, nil
}

func defaultGatewayBridgeName(node *corev1.Node) (string, error) {
	gatewayConfig, err := util.ParseNodeL3GatewayAnnotation(node)
	if err != nil {
		return "", newDiscoveryError(
			uplinkv1alpha1.UplinkStateReasonGatewayInfoUnavailable,
			fmt.Errorf("failed to determine the default shared gateway bridge for node %q: %w",
				node.Name, err),
		)
	}
	if gatewayConfig.BridgeID == "" {
		return "", newDiscoveryError(
			uplinkv1alpha1.UplinkStateReasonGatewayInfoUnavailable,
			fmt.Errorf("default shared gateway bridge is not available for node %q", node.Name),
		)
	}
	return gatewayConfig.BridgeID, nil
}

func (c *Controller) validateBridgeUplink(
	bridgeName string,
	hostInterfaceName string,
	defaultGatewayBridgeName string,
	rejectHostInterfaceAsUplink bool,
) error {
	if bridgeName == ovsIntegrationBridge {
		return newDiscoveryError(
			uplinkv1alpha1.UplinkStateReasonBridgeInvalid,
			fmt.Errorf("OVN integration bridge %s cannot be used as an Uplink bridge",
				bridgeName),
		)
	}
	if err := validateDefaultGatewayBridge(bridgeName, defaultGatewayBridgeName); err != nil {
		return err
	}
	uplinkName, err := c.bridgeResolver.BridgeUplink(bridgeName)
	if err != nil {
		return err
	}
	if rejectHostInterfaceAsUplink && uplinkName == hostInterfaceName {
		return newDiscoveryError(
			uplinkv1alpha1.UplinkStateReasonInvalidHostInterface,
			fmt.Errorf("host interface %s is the physical uplink port for OVS bridge %s",
				hostInterfaceName, bridgeName),
		)
	}
	return nil
}

func validateDefaultGatewayBridge(bridgeName, defaultGatewayBridgeName string) error {
	if bridgeName != defaultGatewayBridgeName {
		return nil
	}
	return newDiscoveryError(
		uplinkv1alpha1.UplinkStateReasonDefaultGatewayBridgeUnsupported,
		fmt.Errorf("default shared gateway bridge %s cannot be used as an Uplink bridge", bridgeName),
	)
}

func (c *Controller) updateResolvedUplinkStateStatus(
	state *uplinkv1alpha1.UplinkState,
	hostInterfaceName string,
	hostState *hostInterfaceState,
	bridgeName string,
	message string,
) error {
	return c.updateUplinkStateStatus(
		state,
		hostInterfaceName,
		hostState,
		bridgeName,
		metav1.ConditionTrue,
		uplinkv1alpha1.UplinkStateReasonResolved,
		message,
	)
}

// updateUplinkStateStatus applies the status fields this reconciler writes
// together with the condition it owns: the DPU-host reports HostDataReady,
// while the DPU and full mode report Resolved (see statusConditionType).
// Full mode writes every status field; the split DPU modes each write their
// own subset and cannot write the peer's condition. Callers pass the
// condition status, reason and message, and on failure return their
// underlying error joined with any write error: discovery inputs live in
// netlink and OVSDB, which generate no Kubernetes events, so the
// controller's rate-limited retries are what re-polls them.
//
// TODO: subscribe to netlink and OVSDB events and reconcile on relevant
// changes instead of polling through retries.
func (c *Controller) updateUplinkStateStatus(
	state *uplinkv1alpha1.UplinkState,
	hostInterfaceName string,
	hostState *hostInterfaceState,
	bridgeName string,
	status metav1.ConditionStatus,
	reason string,
	message string,
) error {
	condition := statusCondition(state, status, reason, message)
	desiredStatus := desiredUplinkStateStatus(state, hostInterfaceName, hostState, bridgeName, condition)
	if reflect.DeepEqual(state.Status, desiredStatus) {
		return nil
	}

	statusApply := uplinkapply.UplinkStateStatus().
		WithType(uplinkv1alpha1.UplinkTypeOVSBridge).
		WithConditions(util.ConditionToApply(condition))

	// Only the DPU-host applies hostInterfaceName: it confirms which
	// interface the host-owned MAC/IP data belongs to, so the DPU must not
	// bump it ahead of fresh host data on a spec change.
	if config.OvnKubeNode.Mode != ovntypes.NodeModeDPU && hostInterfaceName != "" {
		statusApply = statusApply.WithHostInterfaceName(
			uplinkv1alpha1.InterfaceName(hostInterfaceName),
		)
	}
	if config.OvnKubeNode.Mode != ovntypes.NodeModeDPU && hostState != nil {
		if hostState.macAddress != nil {
			statusApply = statusApply.WithMACAddress(
				uplinkv1alpha1.MACAddress(hostState.macAddress.String()),
			)
		}
		statusApply = statusApply.WithIPAddresses(ipAddressCIDRs(hostState.ipAddresses)...)
		statusApply = statusApply.WithDefaultGateways(ipAddresses(hostState.defaultGateways)...)
		if hostState.hostFunction != nil {
			hostFunctionApply := uplinkapply.HostFunction().
				WithPFID(hostState.hostFunction.PFID)
			if hostState.hostFunction.VFID != nil {
				hostFunctionApply = hostFunctionApply.WithVFID(*hostState.hostFunction.VFID)
			}
			statusApply = statusApply.WithHostFunction(hostFunctionApply)
		}
	}
	if config.OvnKubeNode.Mode != ovntypes.NodeModeDPUHost && bridgeName != "" {
		statusApply = statusApply.WithOVSBridge(
			uplinkapply.OVSBridgeStatus().WithName(bridgeName),
		)
	}

	_, err := c.uplinkClient.K8sV1alpha1().UplinkStates().Apply(
		context.Background(),
		uplinkapply.UplinkState(state.Name).WithStatus(
			statusApply,
		),
		metav1.ApplyOptions{
			FieldManager: StatusFieldManager(),
			Force:        true,
		},
	)
	if err != nil {
		return fmt.Errorf("failed to update UplinkState %s status: %w",
			state.Name, err)
	}
	return nil
}

func desiredUplinkStateStatus(
	state *uplinkv1alpha1.UplinkState,
	hostInterfaceName string,
	hostState *hostInterfaceState,
	bridgeName string,
	condition metav1.Condition,
) uplinkv1alpha1.UplinkStateStatus {
	desired := uplinkv1alpha1.UplinkStateStatus{}
	// Split DPU modes preserve fields owned by the peer field manager while
	// comparing the status owned by this side.
	if config.OvnKubeNode.Mode == ovntypes.NodeModeDPU || config.OvnKubeNode.Mode == ovntypes.NodeModeDPUHost {
		desired = state.DeepCopy().Status
	}

	desired.Type = uplinkv1alpha1.UplinkTypeOVSBridge
	// The DPU side does not apply hostInterfaceName, so its desired status
	// keeps whatever the DPU-host side published.
	if config.OvnKubeNode.Mode != ovntypes.NodeModeDPU {
		desired.HostInterfaceName = uplinkv1alpha1.InterfaceName(hostInterfaceName)
	}
	desired.Conditions = append([]metav1.Condition(nil), state.Status.Conditions...)
	meta.SetStatusCondition(&desired.Conditions, condition)

	switch config.OvnKubeNode.Mode {
	case ovntypes.NodeModeDPU:
		setUplinkStateBridgeStatus(&desired, bridgeName)
	case ovntypes.NodeModeDPUHost:
		setUplinkStateHostStatus(&desired, hostState)
	default:
		setUplinkStateHostStatus(&desired, hostState)
		setUplinkStateBridgeStatus(&desired, bridgeName)
	}
	return desired
}

func setUplinkStateHostStatus(status *uplinkv1alpha1.UplinkStateStatus, hostState *hostInterfaceState) {
	status.MACAddress = ""
	status.IPAddresses = nil
	status.DefaultGateways = nil
	status.HostFunction = nil
	if hostState == nil {
		return
	}
	if hostState.macAddress != nil {
		status.MACAddress = uplinkv1alpha1.MACAddress(hostState.macAddress.String())
	}
	status.HostFunction = hostState.hostFunction.DeepCopy()
	if ipAddresses := ipAddressCIDRs(hostState.ipAddresses); len(ipAddresses) > 0 {
		status.IPAddresses = ipAddresses
	}
	if defaultGateways := ipAddresses(hostState.defaultGateways); len(defaultGateways) > 0 {
		status.DefaultGateways = defaultGateways
	}
}

func setUplinkStateBridgeStatus(status *uplinkv1alpha1.UplinkStateStatus, bridgeName string) {
	status.OVSBridge = nil
	if bridgeName != "" {
		status.OVSBridge = &uplinkv1alpha1.OVSBridgeStatus{Name: bridgeName}
	}
}

// StatusFieldManager returns the field manager used by ovnkube-node when it
// applies UplinkState status.
func StatusFieldManager() string {
	switch config.OvnKubeNode.Mode {
	case ovntypes.NodeModeDPU:
		return dpuFieldManager
	case ovntypes.NodeModeDPUHost:
		return dpuHostFieldManager
	default:
		return fieldManager
	}
}

// statusConditionType returns the condition this side of the discovery
// reconcile owns: each UplinkState condition has a single writer, so the
// DPU-host reports on HostDataReady while the DPU (and full mode) reports on
// Resolved.
func statusConditionType() string {
	if config.OvnKubeNode.Mode == ovntypes.NodeModeDPUHost {
		return uplinkv1alpha1.UplinkStateConditionHostDataReady
	}
	return uplinkv1alpha1.UplinkStateConditionResolved
}

func statusCondition(
	state *uplinkv1alpha1.UplinkState,
	status metav1.ConditionStatus,
	reason string,
	message string,
) metav1.Condition {
	// The stored condition of this type, which may predate a
	// hostInterfaceName change, is read only to carry its LastTransitionTime
	// over into the new condition; its reason and message are not reused.
	condition, _ := util.MergeStatusCondition(state.Status.Conditions, metav1.Condition{
		Type:    statusConditionType(),
		Status:  status,
		Reason:  reason,
		Message: message,
	})
	return condition
}

func (c *Controller) ensureUplinkState(
	uplink *uplinkv1alpha1.Uplink,
	nodeConfig *uplinkv1alpha1.UplinkNodeConfig,
	nodeConfigErr error,
) (*uplinkv1alpha1.UplinkState, error) {
	name := uplinkutil.StateName(uplink.Name, c.nodeName)
	state, err := c.uplinkStateLister.Get(name)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("failed to get UplinkState %s: %w", name, err)
		}
		state = desiredUplinkState(uplink, c.nodeName, name, nodeConfig, nodeConfigErr)
		created, err := c.uplinkClient.K8sV1alpha1().UplinkStates().Create(
			context.Background(),
			state,
			metav1.CreateOptions{},
		)
		if err == nil {
			return created, nil
		}
		if !apierrors.IsAlreadyExists(err) {
			return nil, fmt.Errorf("failed to create UplinkState %s: %w", name, err)
		}
		state, err = c.uplinkClient.K8sV1alpha1().UplinkStates().Get(
			context.Background(),
			name,
			metav1.GetOptions{},
		)
		if err != nil {
			return nil, fmt.Errorf("failed to get existing UplinkState %s: %w", name, err)
		}
	}

	if err := uplinkutil.ValidateStateIdentity(state, name, uplink.Name, c.nodeName); err != nil {
		klog.Warningf("Ignoring Uplink %q on node %q: %v", uplink.Name, c.nodeName, err)
		return nil, nil
	}

	desired := desiredUplinkState(uplink, c.nodeName, name, nodeConfig, nodeConfigErr)
	if !reflect.DeepEqual(state.Labels, desired.Labels) ||
		!reflect.DeepEqual(state.Annotations, desired.Annotations) ||
		!reflect.DeepEqual(state.OwnerReferences, desired.OwnerReferences) {
		copy := state.DeepCopy()
		copy.Labels = desired.Labels
		copy.Annotations = desired.Annotations
		copy.OwnerReferences = desired.OwnerReferences
		if _, err := c.uplinkClient.K8sV1alpha1().UplinkStates().Update(
			context.Background(),
			copy,
			metav1.UpdateOptions{},
		); err != nil {
			return nil, fmt.Errorf("failed to update UplinkState %s metadata: %w",
				name, err)
		}
		state = copy
	}
	return state, nil
}

func desiredUplinkState(
	uplink *uplinkv1alpha1.Uplink,
	nodeName, name string,
	nodeConfig *uplinkv1alpha1.UplinkNodeConfig,
	nodeConfigErr error,
) *uplinkv1alpha1.UplinkState {
	state := &uplinkv1alpha1.UplinkState{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(
					uplink,
					uplinkv1alpha1.SchemeGroupVersion.WithKind("Uplink"),
				),
			},
		},
		Spec: uplinkv1alpha1.UplinkStateSpec{
			UplinkName: uplink.Name,
			NodeName:   nodeName,
		},
	}
	if nodeConfig != nil {
		state.Status.Type = nodeConfig.Type
		// The DPU side does not seed hostInterfaceName on creation either:
		// only the DPU-host publishes it, as confirmation of which interface
		// the host-owned MAC/IP data belongs to.
		if config.OvnKubeNode.Mode != ovntypes.NodeModeDPU {
			state.Status.HostInterfaceName = nodeConfig.HostInterfaceName
		}
	}
	if nodeConfigErr != nil {
		state.Status.Conditions = []metav1.Condition{
			{
				Type:               statusConditionType(),
				Status:             metav1.ConditionFalse,
				Reason:             discoveryReason(nodeConfigErr),
				Message:            nodeConfigErr.Error(),
				LastTransitionTime: metav1.Now(),
			},
		}
	}
	return state
}

func (c *Controller) deleteUplinkState(name string) error {
	err := c.uplinkClient.K8sV1alpha1().UplinkStates().Delete(
		context.Background(),
		name,
		metav1.DeleteOptions{},
	)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to delete UplinkState %s: %w", name, err)
	}
	return nil
}

func hostInterfaceStateFromStatus(
	state *uplinkv1alpha1.UplinkState,
	hostInterfaceName string,
) (*hostInterfaceState, error) {
	if string(state.Status.HostInterfaceName) != hostInterfaceName {
		return nil, fmt.Errorf("waiting for DPU-host state for interface %s",
			hostInterfaceName)
	}
	macAddress, err := net.ParseMAC(string(state.Status.MACAddress))
	if err != nil {
		return nil, fmt.Errorf("waiting for DPU-host MAC address for interface %s: %w",
			hostInterfaceName, err)
	}
	ipAddresses := make([]*net.IPNet, 0, len(state.Status.IPAddresses))
	for _, ipAddress := range state.Status.IPAddresses {
		ip, cidr, err := net.ParseCIDR(string(ipAddress))
		if err != nil {
			return nil, fmt.Errorf("failed to parse DPU-host IP address %q: %w",
				ipAddress, err)
		}
		cidr.IP = ip
		ipAddresses = append(ipAddresses, cidr)
	}
	if len(ipAddresses) == 0 {
		return nil, fmt.Errorf("waiting for DPU-host IP addresses for interface %s",
			hostInterfaceName)
	}
	defaultGateways := make([]net.IP, 0, len(state.Status.DefaultGateways))
	for _, defaultGateway := range state.Status.DefaultGateways {
		ip := net.ParseIP(string(defaultGateway))
		if ip == nil {
			return nil, fmt.Errorf("failed to parse DPU-host default gateway %q",
				defaultGateway)
		}
		defaultGateways = append(defaultGateways, ip)
	}
	return &hostInterfaceState{
		macAddress:      macAddress,
		ipAddresses:     ipAddresses,
		defaultGateways: defaultGateways,
		hostFunction:    state.Status.HostFunction.DeepCopy(),
	}, nil
}

func discoveryReason(err error) string {
	var discoveryErr *discoveryError
	if errors.As(err, &discoveryErr) {
		return discoveryErr.reason
	}
	return uplinkv1alpha1.UplinkStateReasonGatewayInfoUnavailable
}

func ipAddressCIDRs(ipNets []*net.IPNet) []uplinkv1alpha1.IPAddressCIDR {
	out := make([]uplinkv1alpha1.IPAddressCIDR, 0, len(ipNets))
	for _, ipNet := range ipNets {
		out = append(out, uplinkv1alpha1.IPAddressCIDR(ipNet.String()))
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func ipAddresses(ips []net.IP) []uplinkv1alpha1.IPAddress {
	out := make([]uplinkv1alpha1.IPAddress, 0, len(ips))
	for _, ip := range ips {
		out = append(out, uplinkv1alpha1.IPAddress(ip.String()))
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func uplinkNeedsUpdate(oldObj, newObj *uplinkv1alpha1.Uplink) bool {
	if oldObj == nil {
		return true
	}
	return !reflect.DeepEqual(oldObj.Spec, newObj.Spec) ||
		oldObj.DeletionTimestamp.IsZero() != newObj.DeletionTimestamp.IsZero()
}

func uplinkStateNeedsUpdate(oldObj, newObj *uplinkv1alpha1.UplinkState) bool {
	if oldObj == nil {
		return true
	}
	return !reflect.DeepEqual(oldObj.Spec, newObj.Spec) ||
		!reflect.DeepEqual(oldObj.Status, newObj.Status) ||
		!reflect.DeepEqual(oldObj.Labels, newObj.Labels) ||
		!reflect.DeepEqual(oldObj.Annotations, newObj.Annotations) ||
		!reflect.DeepEqual(oldObj.OwnerReferences, newObj.OwnerReferences)
}

func (c *Controller) nodeNeedsUpdate(oldObj, newObj *corev1.Node) bool {
	if newObj.Name != c.nodeName {
		return false
	}
	if oldObj == nil {
		return true
	}
	// ParseNodeL3GatewayAnnotation requires a chassis ID for enabled gateways.
	// Watch its initial population so reconciliation recovers from a missing
	// annotation even though Uplink discovery does not use the chassis ID itself.
	return !reflect.DeepEqual(oldObj.Labels, newObj.Labels) ||
		util.NodeL3GatewayAnnotationChanged(oldObj, newObj) ||
		util.NodeChassisIDAnnotationChanged(oldObj, newObj) ||
		oldObj.DeletionTimestamp.IsZero() != newObj.DeletionTimestamp.IsZero()
}

type netlinkHostInterfaceDiscoverer struct{}

func (d netlinkHostInterfaceDiscoverer) Discover(hostInterfaceName string) (*hostInterfaceState, error) {
	link, err := util.GetNetLinkOps().LinkByName(hostInterfaceName)
	if err != nil {
		return nil, newDiscoveryError(
			uplinkv1alpha1.UplinkStateReasonHostInterfaceNotFound,
			fmt.Errorf("host interface %s was not found: %w", hostInterfaceName, err),
		)
	}
	// The MAC becomes the OVN gateway interface MAC: reject unusable ones
	// here rather than far away in gateway programming.
	macAddress := link.Attrs().HardwareAddr
	if !util.IsUsableEthernetMAC(macAddress) {
		return nil, newDiscoveryError(
			uplinkv1alpha1.UplinkStateReasonInvalidHostInterface,
			fmt.Errorf("host interface %s has no usable MAC address", hostInterfaceName),
		)
	}
	addrs, err := nodeutil.GetNetworkInterfaceIPAddresses(hostInterfaceName)
	if err != nil {
		return nil, newDiscoveryError(
			uplinkv1alpha1.UplinkStateReasonGatewayInfoUnavailable,
			fmt.Errorf("failed to read IP addresses from %s: %w",
				hostInterfaceName, err),
		)
	}
	routes, err := hostInterfaceRoutes(link)
	if err != nil {
		return nil, newDiscoveryError(
			uplinkv1alpha1.UplinkStateReasonGatewayInfoUnavailable,
			fmt.Errorf("failed to list routes for %s: %w", hostInterfaceName, err),
		)
	}

	defaultGateways := defaultGatewaysForLink(routes, link.Attrs().Index)
	if len(defaultGateways) > maxDefaultGateways {
		return nil, newDiscoveryError(
			uplinkv1alpha1.UplinkStateReasonGatewayInfoUnavailable,
			fmt.Errorf("host interface %s has %d distinct default gateway next hops, exceeding the UplinkState limit of %d",
				hostInterfaceName, len(defaultGateways), maxDefaultGateways),
		)
	}
	return &hostInterfaceState{
		macAddress:      macAddress,
		ipAddresses:     addrs,
		defaultGateways: defaultGateways,
		hostFunction:    discoverHostFunction(hostInterfaceName),
	}, nil
}

// discoverHostFunction resolves the PF (and, for a VF, the VF) index of the
// host interface for the DPU side to resolve its representor and OVS bridge
// directly. It is best-effort: only the DPU-host publishes host function,
// and only an SR-IOV function has them, so any resolution failure means the
// DPU falls back to resolving the bridge by host MAC.
func discoverHostFunction(hostInterfaceName string) *uplinkv1alpha1.HostFunction {
	if config.OvnKubeNode.Mode != ovntypes.NodeModeDPUHost {
		return nil
	}
	vfDetails, vfErr := util.GetDPUOps().ResolveDeviceDetails(hostInterfaceName)
	if vfErr == nil {
		return &uplinkv1alpha1.HostFunction{
			PFID: int32(vfDetails.PfId),
			VFID: ptr.To(int32(vfDetails.FuncId)),
		}
	}
	pfIndex, pfErr := util.GetDPUOps().ResolvePFIndex(hostInterfaceName)
	if pfErr == nil {
		return &uplinkv1alpha1.HostFunction{PFID: int32(pfIndex)}
	}
	klog.V(5).Infof("No host function for host interface %s: not a VF (%v), not a PF (%v)",
		hostInterfaceName, vfErr, pfErr)
	return nil
}

// hostInterfaceRoutes lists the routes of the table that holds the host
// interface's routes: the VRF's routing table when the interface is enslaved
// to a VRF, since it keeps its routes there, the default route included, and
// the main table otherwise. The dump filters by table only and leaves
// matching the interface to the caller: a multipath route carries an output
// interface and gateway per nexthop and none at the route level, so an OIF
// filter drops it entirely.
func hostInterfaceRoutes(link netlink.Link) ([]netlink.Route, error) {
	table := unix.RT_TABLE_MAIN
	if masterIndex := link.Attrs().MasterIndex; masterIndex != 0 {
		master, err := util.GetNetLinkOps().LinkByIndex(masterIndex)
		if err != nil {
			return nil, fmt.Errorf("failed to get master device of %s: %w",
				link.Attrs().Name, err)
		}
		if vrf, ok := master.(*netlink.Vrf); ok {
			table = int(vrf.Table)
		}
	}
	return util.GetNetLinkOps().RouteListFiltered(
		netlink.FAMILY_ALL,
		&netlink.Route{Table: table},
		netlink.RT_FILTER_TABLE,
	)
}

// defaultGatewaysForLink returns the distinct gateways of the default routes
// that leave through the link, per IP family: only the lowest-metric routes
// count, and among their next hops only the ones with the highest weight.
// UplinkState and the programmed routes carry no weights, so the lighter next
// hops of an unequal-weight multipath route are dropped rather than promoted
// to equal-cost paths.
func defaultGatewaysForLink(routes []netlink.Route, linkIndex int) []net.IP {
	type gatewayCandidates struct {
		metric   int
		nextHops []*netlink.NexthopInfo
	}
	defaults := make(map[bool]gatewayCandidates)
	addNextHop := func(nextHop *netlink.NexthopInfo, metric int) {
		if nextHop.LinkIndex != linkIndex || nextHop.Gw == nil {
			return
		}
		isV6 := nextHop.Gw.To4() == nil
		current, found := defaults[isV6]
		if !found || metric < current.metric {
			current = gatewayCandidates{metric: metric}
		}
		if metric == current.metric {
			current.nextHops = append(current.nextHops, nextHop)
			defaults[isV6] = current
		}
	}
	for _, route := range routes {
		if !isDefaultRoute(route) {
			continue
		}
		if route.LinkIndex == linkIndex && route.Gw != nil {
			addNextHop(&netlink.NexthopInfo{LinkIndex: route.LinkIndex, Gw: route.Gw}, route.Priority)
			continue
		}
		// TODO: Resolve next-hop IDs (nhid) when routes omit gateway IPs.
		for _, nexthop := range route.MultiPath {
			addNextHop(nexthop, route.Priority)
		}
	}

	gateways := make([]net.IP, 0, len(routes))
	seen := make(map[string]bool)
	for _, isV6 := range []bool{false, true} {
		nextHops := defaults[isV6].nextHops
		// netlink reports a next hop's weight minus one as Hops.
		maxHops := 0
		for _, nextHop := range nextHops {
			maxHops = max(maxHops, nextHop.Hops)
		}
		for _, nextHop := range nextHops {
			if nextHop.Hops != maxHops || seen[nextHop.Gw.String()] {
				continue
			}
			seen[nextHop.Gw.String()] = true
			gateways = append(gateways, nextHop.Gw)
		}
	}
	sort.Slice(gateways, func(i, j int) bool { return gateways[i].String() < gateways[j].String() })
	return gateways
}

func isDefaultRoute(route netlink.Route) bool {
	if route.Dst == nil {
		return true
	}
	ones, bits := route.Dst.Mask.Size()
	return bits != 0 && ones == 0
}

// defaultOVSBridgeResolver implements ovsBridgeResolver using OVS and device discovery.
type defaultOVSBridgeResolver struct {
	ovsClient libovsdbclient.Client
}

func (r defaultOVSBridgeResolver) Resolve(hostInterfaceName string) (string, error) {
	bridge, err := ovsops.GetBridge(r.ovsClient, hostInterfaceName)
	if err == nil {
		return bridge.Name, nil
	}
	if !errors.Is(err, libovsdbclient.ErrNotFound) {
		return "", newDiscoveryError(
			uplinkv1alpha1.UplinkStateReasonBridgeNotFound,
			fmt.Errorf("failed to look up OVS bridge %s: %w", hostInterfaceName, err),
		)
	}

	bridgeName, resolveErr := r.bridgeForPortOrInterface(hostInterfaceName)
	if resolveErr == nil {
		return bridgeName, nil
	}

	rep, repErr := util.GetNetdeviceRepresentorName(hostInterfaceName)
	if repErr != nil {
		return "", newDiscoveryError(
			uplinkv1alpha1.UplinkStateReasonBridgeNotFound,
			fmt.Errorf("failed to resolve OVS bridge for %s: %w", hostInterfaceName, resolveErr),
		)
	}

	bridgeName, err = r.bridgeForPortOrInterface(rep)
	if err != nil {
		return "", newDiscoveryError(
			uplinkv1alpha1.UplinkStateReasonBridgeNotFound,
			fmt.Errorf("failed to resolve OVS bridge for %s representor %s: %w", hostInterfaceName, rep, err),
		)
	}
	return bridgeName, nil
}

func (r defaultOVSBridgeResolver) BridgeUplink(bridgeName string) (string, error) {
	bridge, err := ovsops.GetBridge(r.ovsClient, bridgeName)
	if err != nil {
		return "", newDiscoveryError(
			uplinkv1alpha1.UplinkStateReasonBridgeUplinkNotFound,
			fmt.Errorf("failed to get OVS bridge %s: %w", bridgeName, err),
		)
	}

	ctx, cancel := context.WithTimeout(context.Background(), config.Default.OVSDBTxnTimeout)
	ports := []*vswitchd.Port{}
	err = r.ovsClient.WhereCacheByUUIDs(func(*vswitchd.Port) bool { return true }, bridge.Ports...).List(ctx, &ports)
	cancel()
	if err != nil {
		return "", newDiscoveryError(
			uplinkv1alpha1.UplinkStateReasonBridgeUplinkNotFound,
			fmt.Errorf("failed to list ports for OVS bridge %s: %w", bridgeName, err),
		)
	}

	interfaceIDs := []string{}
	for _, port := range ports {
		interfaceIDs = append(interfaceIDs, port.Interfaces...)
	}
	interfaces := []*vswitchd.Interface{}
	ctx, cancel = context.WithTimeout(context.Background(), config.Default.OVSDBTxnTimeout)
	err = r.ovsClient.WhereCacheByUUIDs(func(*vswitchd.Interface) bool { return true }, interfaceIDs...).List(ctx, &interfaces)
	cancel()
	if err != nil {
		return "", newDiscoveryError(
			uplinkv1alpha1.UplinkStateReasonBridgeUplinkNotFound,
			fmt.Errorf("failed to list interfaces for OVS bridge %s: %w", bridgeName, err),
		)
	}
	interfacesByID := make(map[string]*vswitchd.Interface, len(interfaces))
	for _, iface := range interfaces {
		interfacesByID[iface.UUID] = iface
	}

	systemPorts := []string{}
	for _, port := range ports {
		for _, interfaceID := range port.Interfaces {
			iface, ok := interfacesByID[interfaceID]
			if !ok || iface.Type != "system" {
				continue
			}
			// On a DPU, host function representors are system-type ports too,
			// but they can never be the bridge's physical uplink; don't let
			// them make the uplink ambiguous.
			if config.IsModeDPU() && util.GetDPUOps().IsHostFacingRepresentor(iface.Name) {
				klog.V(5).Infof("Bridge %s: ignoring host representor %s while deriving the physical uplink",
					bridgeName, iface.Name)
				continue
			}
			systemPorts = append(systemPorts, port.Name)
			// A port qualifies at most once, even when it carries several
			// system interfaces (e.g. a bond).
			break
		}
	}

	var uplinkName string
	if len(systemPorts) == 1 {
		// Assume port name == interface name; only bonds break this (checked below).
		uplinkName = systemPorts[0]
		if _, err := libovsdbops.GetOVSInterface(r.ovsClient, uplinkName); err != nil {
			if !errors.Is(err, libovsdbclient.ErrNotFound) {
				return "", newDiscoveryError(
					uplinkv1alpha1.UplinkStateReasonBridgeUplinkNotFound,
					fmt.Errorf("failed to get interface for bridge %s candidate uplink %s: %w", bridgeName, uplinkName, err),
				)
			}
			// An OVS-level bond is a Port (e.g. bond0) whose Interfaces have
			// different names (e.g. eth0, eth1), so the lookup above finds no
			// Interface for it; it cannot be the uplink itself, so let the
			// fallbacks decide.
			uplinkName = ""
		}
	}
	if uplinkName == "" {
		if len(systemPorts) > 1 {
			klog.Infof("Found more than one system Type ports on the OVS bridge %s, so skipping "+
				"this method of determining the uplink port", bridgeName)
		}
		// NicToBridge sets bridge-uplink on bridges OVN-Kubernetes creates
		// itself; pre-provisioned Uplink bridges carry it only if the
		// platform set it, so this fallback might not work for them.
		uplinkName = bridge.ExternalIDs["bridge-uplink"]
		if uplinkName == "" && strings.HasPrefix(bridgeName, "br") {
			uplinkName = strings.TrimPrefix(bridgeName, "br")
		}
	}
	if uplinkName == "" {
		return "", newDiscoveryError(
			uplinkv1alpha1.UplinkStateReasonBridgeUplinkNotFound,
			fmt.Errorf("failed to resolve physical uplink for OVS bridge %s", bridgeName),
		)
	}

	// TODO: OVS-level bonds are still unsupported past this point: the
	// resolved name is later used to read the uplink's ofport for flow
	// programming and to apply per-netdev settings, which assume a single
	// interface. Kernel bonds attached as a single interface work today.
	uplinkInterfaces, err := ovsops.FindInterfacesWithPredicate(r.ovsClient, func(iface *vswitchd.Interface) bool {
		return iface.Name == uplinkName
	})
	if err != nil {
		return "", newDiscoveryError(
			uplinkv1alpha1.UplinkStateReasonBridgeUplinkNotFound,
			fmt.Errorf("failed to get interface for bridge %s physical uplink %s: %w", bridgeName, uplinkName, err),
		)
	}
	if len(uplinkInterfaces) == 0 {
		return "", newDiscoveryError(
			uplinkv1alpha1.UplinkStateReasonBridgeUplinkNotFound,
			fmt.Errorf("failed to find interface for bridge %s physical uplink %s", bridgeName, uplinkName),
		)
	}
	return uplinkName, nil
}

// ResolveByHostFunction turns published host function data into the OVS
// bridge holding the uplink representor. The DPU-host publishes, best
// effort, two ways to identify the backing function, and the DPU tries
// the corresponding lookups in order:
//
//	published input             lookup on the DPU
//
//	1. hostFunction.pfID/vfID   phys_port_name match on "c1pf<P>vf<V>"
//	   (indices)                or legacy "pf<P>vf<V>" (pfID 0/1 only)
//
//	2. macAddress               scan every bridge for the representor
//	   (identity)               whose host peer MAC is macAddress
//
// A representor from 1 must peer with macAddress when that peer MAC is
// readable, and its bridge comes from the OVS port-to-bridge lookup.
// Method 2 needs no verification (the match is the identity) and yields
// the bridge directly; the reconcile loop runs it as the fallback when 1
// fails (see ResolveByHostMAC).
func (r defaultOVSBridgeResolver) ResolveByHostFunction(
	details *uplinkv1alpha1.HostFunction,
	hostMAC net.HardwareAddr,
	nodeName string,
) (string, error) {
	function := util.HostFunctionName(details.PFID, details.VFID)
	rep, err := util.FindHostRepresentorByFunction(details.PFID, details.VFID, hostMAC, nodeName)
	if err != nil {
		return "", newDiscoveryError(uplinkv1alpha1.UplinkStateReasonBridgeNotFound, err)
	}
	bridgeName, err := r.bridgeForPortOrInterface(rep)
	if err != nil {
		return "", newDiscoveryError(
			uplinkv1alpha1.UplinkStateReasonBridgeNotFound,
			fmt.Errorf("failed to resolve OVS bridge for host function %s representor %s: %w",
				function, rep, err),
		)
	}
	klog.Infof("Resolved Uplink host function %s to OVS bridge %s via DPU representor %s",
		function, bridgeName, rep)
	return bridgeName, nil
}

func (r defaultOVSBridgeResolver) ResolveByHostMAC(hostMAC net.HardwareAddr, nodeName string) (string, error) {
	bridges, err := ovsops.ListBridges(r.ovsClient)
	if err != nil {
		return "", newDiscoveryError(
			uplinkv1alpha1.UplinkStateReasonBridgeNotFound,
			fmt.Errorf("failed to list OVS bridges: %w", err),
		)
	}
	sort.Slice(bridges, func(i, j int) bool {
		return bridges[i].Name < bridges[j].Name
	})
	var lookupErrors []error
	for _, bridge := range bridges {
		if bridge.Name == ovsIntegrationBridge {
			continue
		}
		rep, err := util.GetDPUOps().FindHostRepresentorByPeerMAC(r.ovsClient, bridge, hostMAC, nodeName)
		if err != nil {
			if !errors.Is(err, util.ErrHostRepresentorNotFound) {
				// Keep searching the remaining bridges, but remember why this
				// one could not be inspected so a total miss can report it.
				lookupErrors = append(lookupErrors, err)
			}
			klog.V(5).Infof("Bridge %s does not back host MAC %s: %v", bridge.Name, hostMAC, err)
			continue
		}
		klog.Infof("Resolved Uplink host MAC %s to OVS bridge %s via DPU representor %s",
			hostMAC, bridge.Name, rep)
		return bridge.Name, nil
	}

	err = fmt.Errorf("failed to find DPU bridge for host MAC %s", hostMAC)
	if len(lookupErrors) > 0 {
		err = fmt.Errorf("%w: %v", err, kerrors.NewAggregate(lookupErrors))
	}
	return "", newDiscoveryError(uplinkv1alpha1.UplinkStateReasonBridgeNotFound, err)
}

func (r defaultOVSBridgeResolver) bridgeForPortOrInterface(name string) (string, error) {
	ports, err := libovsdbops.FindPortsForPortOrInterface(r.ovsClient, name)
	if err != nil {
		return "", err
	}
	if len(ports) == 0 {
		return "", fmt.Errorf("OVS port or interface %s not found: %w", name, libovsdbclient.ErrNotFound)
	}
	portIDs := make(map[string]struct{}, len(ports))
	for _, port := range ports {
		portIDs[port.UUID] = struct{}{}
	}

	bridges, err := ovsops.ListBridges(r.ovsClient)
	if err != nil {
		return "", fmt.Errorf("failed to list OVS bridges for %s: %w", name, err)
	}
	for _, bridge := range bridges {
		for _, portID := range bridge.Ports {
			if _, ok := portIDs[portID]; ok {
				return bridge.Name, nil
			}
		}
	}
	return "", fmt.Errorf("OVS port or interface %s is not attached to a bridge: %w", name, libovsdbclient.ErrNotFound)
}
