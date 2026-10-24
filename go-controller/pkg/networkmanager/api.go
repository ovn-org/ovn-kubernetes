// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package networkmanager

import (
	"context"
	"errors"

	nadinformers "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/informers/externalversions/k8s.cni.cncf.io/v1"

	coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/tools/record"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/allocator/id"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	networkconnectinformer "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/clusternetworkconnect/v1/apis/informers/externalversions/clusternetworkconnect/v1"
	egressipinformer "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/informers/externalversions/egressip/v1"
	rainformers "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1/apis/informers/externalversions/routeadvertisements/v1"
	userdefinednetworkinformer "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/informers/externalversions/userdefinednetwork/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

var ErrNetworkControllerTopologyNotManaged = errors.New("no cluster network controller to manage topology")

// NADReconciler is a level-driven controller notified of NAD key changes.
type NADReconciler controller.Reconciler

// NetworkRefReconciler is notified when node network activity changes.
// The network is identified by name because activity can be observed before
// the NAD controller has fully resolved the network into a local netInfo/ID.
//
// These are edge-style notifications. Controllers that need eventual
// correctness after NAD/ID changes should also register an NADReconciler and
// treat NAD callbacks as a catch-up signal.
type NetworkRefReconciler interface {
	Reconcile(node string, networkName string)
}

type watchFactory interface {
	NADInformer() nadinformers.NetworkAttachmentDefinitionInformer
	ClusterNetworkConnectInformer() networkconnectinformer.ClusterNetworkConnectInformer
	UserDefinedNetworkInformer() userdefinednetworkinformer.UserDefinedNetworkInformer
	ClusterUserDefinedNetworkInformer() userdefinednetworkinformer.ClusterUserDefinedNetworkInformer
	NamespaceInformer() coreinformers.NamespaceInformer
	RouteAdvertisementsInformer() rainformers.RouteAdvertisementsInformer
	NodeCoreInformer() coreinformers.NodeInformer
	PodCoreInformer() coreinformers.PodInformer
	EgressIPInformer() egressipinformer.EgressIPInformer
}

// Interface is the main package entrypoint and provides network related
// information to the rest of the project.
type Interface interface {
	// GetActiveNetworkForNamespace returns a copy of the primary network for
	// the namespace if any or the default network otherwise.
	// If the network is non-existent for a legitimate reason (filtered by
	// Dynamic UDN) it returns nil NetInfo and no error.
	// Returns a NotFound error if the namespace is absent from the informer
	// cache (not necessarily definitively deleted).
	// If the network is non-existent, but should exist, return InvalidPrimaryNetworkError.
	// If unsure, use this one and not GetActiveNetworkForNamespaceFast.
	// Note this function is filtered by Dynamic UDN, so if your caller wants NAD/Network
	// information without D-UDN filtering, use GetPrimaryNADForNamespace.
	GetActiveNetworkForNamespace(namespace string) (util.NetInfo, error)

	// GetActiveNetworkForNamespaceFast returns the primary network for the
	// namespace if any or the default network otherwise. It is faster than
	// GetActiveNetworkForNamespace because it does not copy the network and it
	// does not verify against UDNs. However, it is recommended to be used only
	// by controllers capable of reconciling primary network changes. If unsure,
	// use GetActiveNetworkForNamespace.
	GetActiveNetworkForNamespaceFast(namespace string) util.NetInfo

	// GetPrimaryNADForNamespace returns the full namespaced key of the
	// primary NAD for the given namespace, if one exists.
	// Returns default network if namespace has no primary UDN.
	// Returns a NotFound error if the namespace is absent from the informer
	// cache (not necessarily definitively deleted).
	// This function is not filtered based on Dynamic UDN.
	GetPrimaryNADForNamespace(namespace string) (string, error)

	// GetNetwork returns the network of the given name or nil if unknown
	GetNetwork(name string) util.NetInfo

	// GetActiveNetwork returns the NetInfo currently held by the controller for the given network.
	// This may differ from the NetInfo returned by GetNetwork which reflects the API state.
	// Returns nil if there is no running controller for the provided network.
	GetActiveNetwork(network string) util.NetInfo

	// DoWithLock takes care of locking and unlocking while iterating over all role primary user defined networks.
	DoWithLock(f func(network util.NetInfo) error) error
	GetActiveNetworkNamespaces(networkName string) ([]string, error)

	// GetNetInfoForNADKey returns a copy of the  cached network info for the given NAD key, or nil if unknown.
	// This is a cheap lookup that does not parse the NAD object; it relies on NAD controller state.
	GetNetInfoForNADKey(nadKey string) util.NetInfo
	// GetNetworkNameForNADKey returns the network name mapped to the NAD key, or empty if unknown.
	// This uses NAD controller state and does not parse the NAD object.
	GetNetworkNameForNADKey(nadKey string) string
	// GetNADKeysForNetwork returns NAD keys mapped to the network name, or empty if unknown.
	// This uses NAD controller state and does not parse the NAD object.
	GetNADKeysForNetwork(networkName string) []string
	// RegisterNADReconciler registers a reconciler to be notified of NAD changes.
	RegisterNADReconciler(r NADReconciler) uint64
	// DeRegisterNADReconciler removes a previously registered reconciler.
	DeRegisterNADReconciler(id uint64)
	// RegisterNetworkRefReconciler registers a reconciler to be notified when
	// node network activity changes.
	// Pair this with RegisterNADReconciler when the controller needs both
	// immediate activity signals and eventual catch-up after NAD/ID changes.
	RegisterNetworkRefReconciler(r NetworkRefReconciler) uint64
	// DeRegisterNetworkRefReconciler removes a previously registered reconciler.
	DeRegisterNetworkRefReconciler(id uint64)

	// GetNetworkByID returns the network with the given ID or nil if not found.
	// This is an O(1) lookup using an internal index.
	GetNetworkByID(id int) util.NetInfo

	// NodeHasNetwork returns true if the given node has at least one pod/egress IP using any NAD
	// for the specified network with Dynamic UDN.
	// If Dynamic UDN is disabled, it always returns true.
	NodeHasNetwork(node, networkName string) bool
}

// Controller handles the runtime of the package
type Controller interface {
	Interface() Interface
	Start() error
	Stop()
}

// Default returns a default implementation that assumes the default network is
// the only ever existing network. Used when multi-network capabilities are not
// enabled or testing.
func Default() Controller {
	return def
}

// NewForCluster builds a controller for cluster manager
func NewForCluster(
	cm ControllerManager,
	wf watchFactory,
	ovnClient *util.OVNClusterManagerClientset,
	recorder record.EventRecorder,
	tunnelKeysAllocator *id.TunnelKeysAllocator,
) (Controller, error) {
	return new(
		"clustermanager-nad-controller",
		"",
		cm,
		wf,
		ovnClient,
		recorder,
		tunnelKeysAllocator,
		"",
		false,
	)
}

// NewForNode builds a controller for node manager
func NewForNode(
	node string,
	cm ControllerManager,
	wf watchFactory,
) (Controller, error) {
	return new(
		"node-nad-controller",
		node,
		cm,
		wf,
		nil,
		nil,
		nil,
		node,
		false,
	)
}

// NewForController builds a controller for ovnkube-controller. Existing networks
// are started asynchronously with bounded concurrency so bulk UDN start does not
// block controller readiness.
func NewForController(
	node string,
	cm ControllerManager,
	wf watchFactory,
) (Controller, error) {
	return new(
		"ovnkube-controller-nad-controller",
		node,
		cm,
		wf,
		nil,
		nil,
		nil,
		node,
		true,
	)
}

// New builds a new Controller. It's aware of networks configured in the system,
// gathers relevant information about them for the project and handles the
// lifecycle of their corresponding network controllers.
func new(
	name string,
	node string,
	cm ControllerManager,
	wf watchFactory,
	ovnClient *util.OVNClusterManagerClientset,
	recorder record.EventRecorder,
	tunnelKeysAllocator *id.TunnelKeysAllocator,
	filterNADsOnNode string,
	asyncNetworkStart bool,
) (Controller, error) {
	return newController(name, node, cm, wf, ovnClient, recorder, tunnelKeysAllocator, filterNADsOnNode, asyncNetworkStart)
}

// ControllerManager manages controllers. Needs to be provided in order to build
// new network controllers and to to be informed of potential stale networks in
// case it has clean-up of it's own to do.
type ControllerManager interface {
	NewNetworkController(netInfo util.NetInfo) (NetworkController, error)
	GetDefaultNetworkController() ReconcilableNetworkController
	CleanupStaleNetworks(validNetworks ...util.NetInfo) error

	// Reconcile informs the manager of network changes that other managed
	// network aware controllers might be interested in.
	Reconcile(name string, old, new util.NetInfo) error
}

// ReconcilableNetworkController is a network controller that can reconcile
// certain network configuration changes.
type ReconcilableNetworkController interface {
	util.NetInfo

	// Reconcile informs the controller of network configuration changes.
	// Implementations should not return any error at or after updating this
	// network information on their as there is nothing network manager can do
	// about it. In this case implementations should either carry their on
	// retries or log the error and give up.
	Reconcile(util.NetInfo) error
}

// BaseNetworkController is a ReconcilableNetworkController that can be started and
// stopped.
type BaseNetworkController interface {
	ReconcilableNetworkController
	Start(ctx context.Context) error
	Stop()
}

// NetworkController is a BaseNetworkController that can also clean up after
// itself.
type NetworkController interface {
	BaseNetworkController
	Cleanup() error
	// HandleNetworkRefChange is only used by nadControllers with Dynamic UDN
	// to inform the network controller that a relevant NAD has become active or inactive.
	HandleNetworkRefChange(node string, active bool)
}

// defaultNetworkManager assumes the default network is
// the only ever existing network. Used when multi-network capabilities are not
// enabled or testing.
type defaultNetworkManager struct{}

func (nm defaultNetworkManager) Interface() Interface {
	return &nm
}

func (nm defaultNetworkManager) Start() error {
	return nil
}

func (nm defaultNetworkManager) Stop() {}

func (nm defaultNetworkManager) GetActiveNetworkForNamespace(string) (util.NetInfo, error) {
	return &util.DefaultNetInfo{}, nil
}

func (nm defaultNetworkManager) GetPrimaryNADForNamespace(_ string) (string, error) {
	return types.DefaultNetworkName, nil
}

func (nm defaultNetworkManager) GetActiveNetworkForNamespaceFast(string) util.NetInfo {
	return &util.DefaultNetInfo{}
}

func (nm defaultNetworkManager) GetNetwork(name string) util.NetInfo {
	if name != types.DefaultNetworkName {
		return nil
	}
	return &util.DefaultNetInfo{}
}

func (nm defaultNetworkManager) DoWithLock(f func(network util.NetInfo) error) error {
	return f(&util.DefaultNetInfo{})
}

func (nm defaultNetworkManager) GetActiveNetworkNamespaces(_ string) ([]string, error) {
	return []string{"default"}, nil
}

func (nm defaultNetworkManager) GetActiveNetwork(network string) util.NetInfo {
	if network != types.DefaultNetworkName {
		return nil
	}
	return &util.DefaultNetInfo{}
}

func (nm defaultNetworkManager) GetNetInfoForNADKey(_ string) util.NetInfo { return nil }

func (nm defaultNetworkManager) GetNetworkNameForNADKey(_ string) string { return "" }

func (nm defaultNetworkManager) GetNADKeysForNetwork(_ string) []string { return nil }

func (nm defaultNetworkManager) RegisterNADReconciler(_ NADReconciler) uint64 {
	return 0
}

func (nm defaultNetworkManager) DeRegisterNADReconciler(_ uint64) {}

func (nm defaultNetworkManager) RegisterNetworkRefReconciler(_ NetworkRefReconciler) uint64 {
	return 0
}

func (nm defaultNetworkManager) DeRegisterNetworkRefReconciler(_ uint64) {}

func (nm defaultNetworkManager) GetNetworkByID(id int) util.NetInfo {
	if id != types.DefaultNetworkID {
		return nil
	}
	return &util.DefaultNetInfo{}
}

func (nm defaultNetworkManager) NodeHasNetwork(_ string, _ string) bool {
	// default network is never filtered
	return true
}

var def Controller = &defaultNetworkManager{}
