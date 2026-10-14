// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package routeadvertisements

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	nadtypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	nadclientset "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned"
	nadlisters "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/listers/k8s.cni.cncf.io/v1"
	frrtypes "github.com/metallb/frr-k8s/api/v1beta1"
	frrclientset "github.com/metallb/frr-k8s/pkg/client/clientset/versioned"
	frrlisters "github.com/metallb/frr-k8s/pkg/client/listers/api/v1beta1"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	controllerutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	eiptypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	egressiplisters "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/listers/egressip/v1"
	ratypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1"
	raapply "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1/apis/applyconfiguration/routeadvertisements/v1"
	raclientset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1/apis/clientset/versioned"
	ralisters "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1/apis/listers/routeadvertisements/v1"
	apitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/types"
	uplinkv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1"
	uplinklisters "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/listers/uplink/v1alpha1"
	userdefinednetworkv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	vteplisters "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1/apis/listers/vtep/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	uplinkutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/uplink"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

const (
	generateName          = "ovnk-generated-"
	fieldManager          = "clustermanager-routeadvertisements-controller"
	conditionTypeAccepted = "Accepted"
	// rawConfigPriority is set to an arbitrary value that still allows users to
	// override if needed.
	rawConfigPriority = 10

	// reconcile outcomes, used as the "result" label of the reconcile duration
	// metric
	reconcileResultSuccess     = "success"
	reconcileResultError       = "error"
	reconcileResultConfigError = "config_error"
	reconcileResultPending     = "pending"

	// generated FRRConfiguration operations, used as the "op" label of the
	// write counter
	frrConfigOpCreate    = "create"
	frrConfigOpUpdate    = "update"
	frrConfigOpDelete    = "delete"
	frrConfigOpUnchanged = "unchanged"
)

var (
	errConfig      = errors.New("configuration error")
	errPending     = errors.New("configuration pending")
	cudnController = userdefinednetworkv1.SchemeGroupVersion.WithKind("ClusterUserDefinedNetwork")
)

// Controller reconciles RouteAdvertisements
type Controller struct {
	wf *factory.WatchFactory

	eipLister         egressiplisters.EgressIPLister
	frrLister         frrlisters.FRRConfigurationLister
	nadLister         nadlisters.NetworkAttachmentDefinitionLister
	nodeLister        corelisters.NodeLister
	raLister          ralisters.RouteAdvertisementsLister
	namespaceLister   corelisters.NamespaceLister
	uplinkStateLister uplinklisters.UplinkStateLister
	vtepLister        vteplisters.VTEPLister

	frrClient frrclientset.Interface
	nadClient nadclientset.Interface
	raClient  raclientset.Interface

	eipController         controllerutil.Controller
	frrController         controllerutil.Controller
	nadController         controllerutil.Controller
	nodeController        controllerutil.Controller
	raController          controllerutil.Controller
	nsController          controllerutil.Controller
	uplinkStateController controllerutil.Controller

	nm networkmanager.Interface
	// networkRefReconcilerID identifies our registration with the network
	// manager for network activity change notifications
	networkRefReconcilerID uint64

	// raNetworks caches the networks each RouteAdvertisements selects, so
	// that a network activity change only reconciles the RouteAdvertisements
	// selecting that network
	raNetworksLock sync.RWMutex
	raNetworks     map[string]sets.Set[string]
}

// networkRefReconcilerFunc adapts a function to the
// networkmanager.NetworkRefReconciler interface.
type networkRefReconcilerFunc func(node, networkName string)

func (f networkRefReconcilerFunc) Reconcile(node, networkName string) { f(node, networkName) }

// setRANetworks caches the networks selected by a RouteAdvertisements.
func (c *Controller) setRANetworks(ra string, networks []string) {
	c.raNetworksLock.Lock()
	defer c.raNetworksLock.Unlock()
	c.raNetworks[ra] = sets.New(networks...)
}

// deleteRANetworks removes a RouteAdvertisements from the selected networks
// cache.
func (c *Controller) deleteRANetworks(ra string) {
	c.raNetworksLock.Lock()
	defer c.raNetworksLock.Unlock()
	delete(c.raNetworks, ra)
}

// getRAsForNetwork returns the RouteAdvertisements known to select the
// network. A RouteAdvertisements not in the cache has not been reconciled
// yet: its pending reconcile will read the current activity state of its
// networks, so it doesn't need this notification.
func (c *Controller) getRAsForNetwork(network string) []string {
	c.raNetworksLock.RLock()
	defer c.raNetworksLock.RUnlock()
	var ras []string
	for ra, networks := range c.raNetworks {
		if networks.Has(network) {
			ras = append(ras, ra)
		}
	}
	return ras
}

// NewController builds a controller that reconciles RouteAdvertisements
func NewController(
	nm networkmanager.Interface,
	wf *factory.WatchFactory,
	ovnClient *util.OVNClusterManagerClientset,
) *Controller {
	c := &Controller{
		wf:              wf,
		frrLister:       wf.FRRConfigurationsInformer().Lister(),
		nadLister:       wf.NADInformer().Lister(),
		nodeLister:      wf.NodeCoreInformer().Lister(),
		raLister:        wf.RouteAdvertisementsInformer().Lister(),
		namespaceLister: wf.NamespaceInformer().Lister(),
		frrClient:       ovnClient.FRRClient,
		nadClient:       ovnClient.NetworkAttchDefClient,
		raClient:        ovnClient.RouteAdvertisementsClient,
		nm:              nm,
		raNetworks:      map[string]sets.Set[string]{},
	}
	if util.IsUplinkEnabled() {
		c.uplinkStateLister = wf.UplinkStateInformer().Lister()
	}

	handleError := func(key string, errorstatus error) error {
		ra, err := c.raLister.Get(key)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("cannot get RouteAdvertisements %q to report error %v in status: %v",
				key,
				errorstatus,
				err,
			)
		}

		return c.updateRAStatus(ra, false, errorstatus)
	}

	raConfig := &controllerutil.ControllerConfig[ratypes.RouteAdvertisements]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:      c.reconcile,
		Threadiness:    1,
		Informer:       wf.RouteAdvertisementsInformer().Informer(),
		Lister:         wf.RouteAdvertisementsInformer().Lister().List,
		ObjNeedsUpdate: raNeedsUpdate,
		HandleError:    handleError,
	}
	c.raController = controllerutil.NewController("clustermanager routeadvertisements controller", raConfig)

	frrConfig := &controllerutil.ControllerConfig[frrtypes.FRRConfiguration]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:      c.reconcileFRRConfiguration,
		Threadiness:    1,
		Informer:       wf.FRRConfigurationsInformer().Informer(),
		Lister:         wf.FRRConfigurationsInformer().Lister().List,
		ObjNeedsUpdate: frrConfigurationNeedsUpdate,
	}
	c.frrController = controllerutil.NewController("clustermanager routeadvertisements frrconfiguration controller", frrConfig)

	nadConfig := &controllerutil.ControllerConfig[nadtypes.NetworkAttachmentDefinition]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:      c.reconcileNAD,
		Threadiness:    1,
		Informer:       wf.NADInformer().Informer(),
		Lister:         wf.NADInformer().Lister().List,
		ObjNeedsUpdate: nadNeedsUpdate,
	}
	c.nadController = controllerutil.NewController("clustermanager routeadvertisements nad controller", nadConfig)

	nodeConfig := &controllerutil.ControllerConfig[corev1.Node]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:      func(_ string) error { c.raController.ReconcileAll(); return nil },
		Threadiness:    1,
		Informer:       wf.NodeCoreInformer().Informer(),
		Lister:         wf.NodeCoreInformer().Lister().List,
		ObjNeedsUpdate: nodeNeedsUpdate,
	}
	c.nodeController = controllerutil.NewController("clustermanager routeadvertisements node controller", nodeConfig)

	if util.IsUplinkEnabled() {
		uplinkStateConfig := &controllerutil.ControllerConfig[uplinkv1alpha1.UplinkState]{
			RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
			Reconcile:      func(_ string) error { c.raController.ReconcileAll(); return nil },
			Threadiness:    1,
			Informer:       wf.UplinkStateInformer().Informer(),
			Lister:         c.uplinkStateLister.List,
			ObjNeedsUpdate: uplinkStateNeedsUpdate,
		}
		c.uplinkStateController = controllerutil.NewController(
			"clustermanager routeadvertisements uplinkstate controller",
			uplinkStateConfig,
		)
	}

	if config.OVNKubernetesFeature.EnableEgressIP {
		c.eipLister = wf.EgressIPInformer().Lister()

		eipConfig := &controllerutil.ControllerConfig[eiptypes.EgressIP]{
			RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
			Reconcile:      c.reconcileEgressIPs,
			Threadiness:    1,
			Informer:       wf.EgressIPInformer().Informer(),
			Lister:         wf.EgressIPInformer().Lister().List,
			ObjNeedsUpdate: egressIPNeedsUpdate,
		}
		c.eipController = controllerutil.NewController("clustermanager routeadvertisements egressip controller", eipConfig)

		nsConfig := &controllerutil.ControllerConfig[corev1.Namespace]{
			RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
			Reconcile:      c.reconcileEgressIPs,
			Threadiness:    1,
			Informer:       wf.NamespaceInformer().Informer(),
			Lister:         wf.NamespaceInformer().Lister().List,
			ObjNeedsUpdate: nsNeedsUpdate,
		}
		c.nsController = controllerutil.NewController("clustermanager routeadvertisements namespace controller", nsConfig)
	}

	if util.IsEVPNEnabled() {
		c.vtepLister = wf.VTEPInformer().Lister()
	}

	return c
}

func (c *Controller) Start() error {
	defer klog.Infof("Cluster manager routeadvertisements started")
	// reconcile when a network goes active or inactive on a node: some of
	// these changes, like a network going active again during the deletion
	// grace period, update no object we watch
	c.networkRefReconcilerID = c.nm.RegisterNetworkRefReconciler(networkRefReconcilerFunc(func(_, networkName string) {
		ras := c.getRAsForNetwork(networkName)
		if len(ras) == 0 {
			klog.V(5).Infof("No RouteAdvertisements select network %s, ignoring its activity change", networkName)
			return
		}
		for _, ra := range ras {
			c.raController.Reconcile(ra)
		}
	}))
	controllers := []controllerutil.Reconciler{
		c.frrController,
		c.nadController,
		c.nodeController,
		c.raController,
	}
	if util.IsUplinkEnabled() {
		controllers = append(controllers, c.uplinkStateController)
	}
	if config.OVNKubernetesFeature.EnableEgressIP {
		controllers = append(controllers, c.eipController, c.nsController)
	}
	return controllerutil.Start(controllers...)
}

func (c *Controller) Stop() {
	c.nm.DeRegisterNetworkRefReconciler(c.networkRefReconcilerID)
	controllers := []controllerutil.Reconciler{
		c.frrController,
		c.nadController,
		c.nodeController,
		c.raController,
	}
	if util.IsUplinkEnabled() {
		controllers = append(controllers, c.uplinkStateController)
	}
	if config.OVNKubernetesFeature.EnableEgressIP {
		controllers = append(controllers, c.eipController, c.nsController)
	}
	controllerutil.Stop(controllers...)
	klog.Infof("Cluster manager routeadvertisements stopped")
}

func (c *Controller) ReconcileNetwork(_ string, old, new util.NetInfo) {
	// This controller already listens on NAD events but there is two additional
	// scenarios we need to cover for:
	// - for newly created networks, we need to wait until network manager is
	// aware of them.
	// - if the namespaces served by a network change.
	oldNamespaces, newNamespaces := sets.New[string](), sets.New[string]()
	if old != nil {
		oldNamespaces.Insert(old.GetNADNamespaces()...)
	}
	if new != nil {
		newNamespaces.Insert(new.GetNADNamespaces()...)
	}
	if new != nil && !newNamespaces.Equal(oldNamespaces) {
		// we use one of the NADs of the network to reconcile it
		nads := c.nm.GetNADKeysForNetwork(new.GetNetworkName())
		if len(nads) > 0 {
			c.nadController.Reconcile(nads[0])
		}
		// if the namespaces served by a network changed, it is possible that
		// those namespaces are served or no longer served by the default
		// network, so reconcile it as well
		c.nadController.Reconcile(config.Default.ClusterDefaultNADName)
	}
}

// Reconcile RouteAdvertisements. For each selected FRRConfiguration and node,
// another FRRConfiguration might be generated:
//
// - If pod network advertisements are enabled, the generated FRRConfiguration
// will announce from the node the selected network prefixes for that node on
// the matching target VRFs.
//
// - If EgressIP advertisements are enabled, the generated FRRConfiguration will
// announce from the node the EgressIPs allocated to it on the matching target
// VRFs. Selected EgressIP are those that serve the same namespaces as the
// selected networks. Target VRF `auto` is not supported for EgressIPs.
//
// - If pod network advertisements are enabled, the generated FRRConfiguration
// will import the target VRFs on the selected networks as required.
//
// - The generated FRRConfiguration will be labeled with the RouteAdvertisements
// name and annotated with an internal key to facilitate updating it when
// needed.
//
// The controller will also annotate the NADs of the selected networks with the
// RouteAdvertisements that select them to facilitate processing for downstream
// zone/node controllers.
//
// Finally, it will update the status of the RouteAdvertisements.
//
// The controller processes selected events of RouteAdvertisements,
// FRRConfigurations, Nodes, EgressIPs, NADs and namespaces.
func (c *Controller) reconcile(name string) error {
	startTime := time.Now()
	result := reconcileResultSuccess
	deleted := false
	klog.V(5).Infof("Syncing routeadvertisements %q", name)
	defer func() {
		if deleted {
			// drop the per-resource timeseries last: the cleanup reconcile
			// below still records against them
			metrics.DeleteRouteAdvertisementMetrics(name)
		} else {
			metrics.RecordRouteAdvertisementReconcile(name, result, time.Since(startTime))
		}
		klog.V(4).Infof("Finished syncing routeadvertisements %q, took %v", name, time.Since(startTime))
	}()

	ra, err := c.raLister.Get(name)
	if err != nil && !apierrors.IsNotFound(err) {
		result = reconcileResultError
		return fmt.Errorf("failed to get RouteAdvertisements %q: %w", name, err)
	}

	if ra == nil {
		deleted = true
		c.deleteRANetworks(name)
	}

	hadUpdates, err := c.reconcileRouteAdvertisements(name, ra)
	switch {
	case err == nil:
	case errors.Is(err, errConfig):
		result = reconcileResultConfigError
	case errors.Is(err, errPending):
		result = reconcileResultPending
	default:
		result = reconcileResultError
		return fmt.Errorf("failed to reconcile RouteAdvertisements %q: %w", name, err)
	}

	if statusErr := c.updateRAStatus(ra, hadUpdates, err); statusErr != nil {
		result = reconcileResultError
		return statusErr
	}

	return nil
}

func (c *Controller) reconcileRouteAdvertisements(name string, ra *ratypes.RouteAdvertisements) (bool, error) {
	// generate FRRConfigurations
	frrConfigs, nads, cfgErr := c.generateFRRConfigurations(ra)
	if cfgErr != nil && !errors.Is(cfgErr, errPending) {
		return false, cfgErr
	}

	// update them
	hadFRRConfigUpdates, err := c.updateFRRConfigurations(name, frrConfigs)
	if err != nil {
		return false, fmt.Errorf("failed updating FRRConfigurations for RouteAdvertisements %q: %w", name, err)
	}

	// annotate NADs
	hadNADUpdates, err := c.updateNADs(name, nads)
	if err != nil {
		return false, fmt.Errorf("failed annotating NADs for RouteAdvertisements %q: %w", name, err)
	}

	return hadFRRConfigUpdates || hadNADUpdates, cfgErr
}

// selectedNetworks is a helper struct that stores information about networks
// that have been selected by a RouteAdvertisements. It is important that prefix
// lists are ordered to generate consistent FRRConfigurations.
type selectedNetworks struct {
	// networks is an ordered list of selected network names
	networks []string
	// vrfs is an ordered list of selected networks VRF's
	vrfs []string
	// networkVRFs is a mapping of VRF to corresponding network
	networkVRFs map[string]string
	// subnets is an ordered list of all selected network subnets
	subnets []string
	// hostSubnets is an ordered list of all selected network subnets specific to a node
	hostSubnets []string
	// networkSubnets is a map of selected network names to their ordered network subnets
	networkSubnets map[string][]string
	// vtepIPsByNode maps node name to an ordered list of VTEP IP prefixes (/32 or /128)
	// to advertise in the default-VRF router for EVPN underlay reachability.
	// The value should mostly be a single /32 for IPv4 and /128 for IPv6, but its a string of IPs if we
	// need to support dual-stack tunnels in the future which seems really uncommon and
	// only has a rare migration use case OR we have 1 RA selecting more than one CUDN with each
	// CUDN having a different VTEPs which is also rare.
	vtepIPsByNode map[string][]string
	// vtepCIDRs is the deduplicated, ordered list of VTEP CIDRs from the
	// referenced VTEP resources.  Used as the ToReceive prefix filter so that
	// each node accepts routes for all VTEP IPs within these ranges.
	vtepCIDRs []string
	// hostNetworkSubnets is a map of selected network names to their ordered network subnets specific for a node
	hostNetworkSubnets map[string][]string
	// prefixLength is a map of selected network to their prefix length
	prefixLength map[string]uint32
	// networkType is a map of selected network to their topology
	networkTopology map[string]string
	// macVRFConfigs is an ordered list of MAC-VRF EVPN configurations for selected networks
	macVRFConfigs []*vrfConfig
	// ipVRFConfigs is an ordered list of IP-VRF EVPN configurations for selected networks
	ipVRFConfigs []*ipVRFConfig
	// networkTransport is a map of selected network to their transport mode
	networkTransport map[string]string
	// networkUplinks is a map of selected network to its Uplink name.
	networkUplinks map[string]string
}

// vrfConfig holds base VRF EVPN configuration for a network
type vrfConfig struct {
	// NetworkName is the name of the network this config belongs to
	NetworkName string
	// VNI is the VXLAN Network Identifier
	VNI int32
	// RouteTarget is the BGP route target, empty means use FRR defaults
	RouteTarget string
}

// ipVRFConfig holds IP-VRF EVPN configuration for a network
type ipVRFConfig struct {
	vrfConfig
	// VRFName is the Linux VRF name
	VRFName string
	// HasIPv4 indicates if the network has IPv4 subnets
	HasIPv4 bool
	// HasIPv6 indicates if the network has IPv6 subnets
	HasIPv6 bool
}

// generateFRRConfigurations generates FRRConfigurations for the route
// advertisements. Also returns the selected network NADs.
func (c *Controller) generateFRRConfigurations(ra *ratypes.RouteAdvertisements) ([]*frrtypes.FRRConfiguration, []*nadtypes.NetworkAttachmentDefinition, error) {
	if ra == nil {
		return nil, nil, nil
	}

	advertisements := sets.New(ra.Spec.Advertisements...)
	if advertisements.Has(ratypes.EgressIP) && !config.OVNKubernetesFeature.EnableEgressIP {
		return nil, nil, fmt.Errorf("%w: advertising EgressIP requires EgressIP feature to be enabled", errConfig)
	}
	if advertisements.Has(ratypes.EgressIP) && ra.Spec.TargetVRF == "auto" {
		return nil, nil, fmt.Errorf("%w: advertising EgressIP not supported with TargetVRF set to 'auto'", errConfig)
	}

	// if we are matching on the well known default network label, create an
	// internal nad for it if it doesn't exist
	nads, err := c.getSelectedNADs(ra.Spec.NetworkSelectors)
	if err != nil {
		return nil, nil, err
	}
	if len(nads) == 0 {
		return nil, nil, fmt.Errorf("%w: no networks selected", errPending)
	}
	// ordered, so that processing and error messages are deterministic across
	// reconciles regardless of lister iteration order
	slices.SortFunc(nads, func(a, b *nadtypes.NetworkAttachmentDefinition) int {
		return strings.Compare(a.Namespace+"/"+a.Name, b.Namespace+"/"+b.Name)
	})

	// validate and gather information about the networks
	networkSet := sets.New[string]()
	vtepNames := sets.New[string]()
	selectedNetworks := &selectedNetworks{
		networkVRFs:      map[string]string{},
		networkSubnets:   map[string][]string{},
		prefixLength:     map[string]uint32{},
		networkTopology:  map[string]string{},
		networkTransport: map[string]string{},
		networkUplinks:   map[string]string{},
	}
	for _, nad := range nads {
		networkName := util.GetAnnotatedNetworkName(nad)
		network := c.nm.GetNetwork(networkName)
		if network == nil {
			// network not yet known by network manager, skip
			continue
		}
		if networkSet.Has(networkName) {
			continue
		}
		if !network.IsDefault() && !network.IsPrimaryNetwork() {
			return nil, nil, fmt.Errorf("%w: selected network %q is not the default nor a primary network", errConfig, networkName)
		}
		if network.TopologyType() != types.Layer3Topology && network.TopologyType() != types.Layer2Topology {
			return nil, nil, fmt.Errorf("%w: selected network %q has unsupported topology %q", errConfig, networkName, network.TopologyType())
		}

		if advertisements.Has(ratypes.EgressIP) && network.TopologyType() == types.Layer2Topology {
			return nil, nil, fmt.Errorf("%w: EgressIP advertisement is currently not supported for Layer2 networks, network: %s", errConfig, network.GetNetworkName())
		}

		vrf := util.GetNetworkVRFName(network)
		if vfrNet, hasVFR := selectedNetworks.networkVRFs[vrf]; hasVFR && vfrNet != networkName {
			return nil, nil, fmt.Errorf("%w: vrf %q found to be mapped to multiple networks %v", errConfig, vrf, []string{vfrNet, networkName})
		}
		networkSet.Insert(networkName)
		selectedNetworks.vrfs = append(selectedNetworks.vrfs, vrf)
		selectedNetworks.networkVRFs[vrf] = networkName
		selectedNetworks.networkTopology[networkName] = network.TopologyType()
		selectedNetworks.networkTransport[networkName] = network.Transport()
		selectedNetworks.networkUplinks[networkName] = network.Uplink()

		// MAC-VRF configuration
		if macVNI := network.EVPNMACVRFVNI(); macVNI > 0 {
			selectedNetworks.macVRFConfigs = append(selectedNetworks.macVRFConfigs, &vrfConfig{
				NetworkName: networkName,
				VNI:         macVNI,
				RouteTarget: network.EVPNMACVRFRouteTarget(),
			})
		}

		// IP-VRF configuration
		if ipVNI := network.EVPNIPVRFVNI(); ipVNI > 0 {
			// Compute IP families from network subnets
			hasIPv4, hasIPv6 := false, false
			for _, subnet := range network.Subnets() {
				if subnet.CIDR.IP.To4() == nil {
					hasIPv6 = true
				} else {
					hasIPv4 = true
				}
			}
			selectedNetworks.ipVRFConfigs = append(selectedNetworks.ipVRFConfigs, &ipVRFConfig{
				vrfConfig: vrfConfig{
					NetworkName: networkName,
					VNI:         ipVNI,
					RouteTarget: network.EVPNIPVRFRouteTarget(),
				},
				VRFName: vrf,
				HasIPv4: hasIPv4,
				HasIPv6: hasIPv6,
			})
		}
		hasEVPNConfig := network.EVPNMACVRFVNI() > 0 || network.EVPNIPVRFVNI() > 0
		if vtepName := network.EVPNVTEPName(); hasEVPNConfig && vtepName != "" {
			vtepNames.Insert(vtepName)
		}
		if hasEVPNConfig && ra.Spec.TargetVRF != "auto" && ra.Spec.TargetVRF != vrf {
			return nil, nil, fmt.Errorf("%w: EVPN network %q with VRF %q requires TargetVRF to be 'auto' or %q, got %q",
				errConfig, networkName, vrf, vrf, ra.Spec.TargetVRF)
		}
		// TODO check overlaps?
		for _, cidr := range network.Subnets() {
			subnet := cidr.CIDR.String()
			len := uint32(cidr.HostSubnetLength)
			selectedNetworks.networkSubnets[networkName] = append(selectedNetworks.networkSubnets[networkName], subnet)
			selectedNetworks.subnets = append(selectedNetworks.subnets, subnet)
			selectedNetworks.prefixLength[subnet] = len
		}
		// ordered
		slices.Sort(selectedNetworks.networkSubnets[networkName])
	}
	// ordered
	slices.Sort(selectedNetworks.vrfs)
	slices.Sort(selectedNetworks.subnets)
	slices.SortFunc(selectedNetworks.macVRFConfigs, func(a, b *vrfConfig) int { return int(a.VNI - b.VNI) })
	slices.SortFunc(selectedNetworks.ipVRFConfigs, func(a, b *ipVRFConfig) int { return int(a.VNI - b.VNI) })
	selectedNetworks.networks = sets.List(networkSet)
	// cache the selected networks so that network activity changes only
	// reconcile the RouteAdvertisements selecting them. Returning early above
	// keeps the previous entry, which matches the FRRConfigurations still
	// deployed from the last successful reconcile; the errors returned above
	// are only resolved by RA/NAD/node updates, which we watch and which
	// refresh this cache.
	c.setRANetworks(ra.Name, selectedNetworks.networks)

	// gather selected nodes
	nodeSelector, err := metav1.LabelSelectorAsSelector(&ra.Spec.NodeSelector)
	if err != nil {
		return nil, nil, err
	}
	if !nodeSelector.Empty() && advertisements.Has(ratypes.PodNetwork) {
		return nil, nil, fmt.Errorf("%w: node selector has to select all nodes if pod network is advertised", errConfig)
	}
	nodes, err := c.nodeLister.List(nodeSelector)
	if err != nil {
		return nil, nil, err
	}
	if len(nodes) == 0 {
		return nil, nil, fmt.Errorf("%w: no nodes selected", errPending)
	}
	// ordered, so that processing and error messages are deterministic across
	// reconciles regardless of lister iteration order
	slices.SortFunc(nodes, func(a, b *corev1.Node) int { return strings.Compare(a.Name, b.Name) })
	// prepare a map of selected nodes to the FRRConfigurations that apply to
	// them
	nodeToFRRConfig := map[string][]*frrtypes.FRRConfiguration{}
	for _, node := range nodes {
		nodeToFRRConfig[node.Name] = nil
	}

	// gather selected FRRConfigurations, map them to the selected nodes
	frrSelector, err := metav1.LabelSelectorAsSelector(&ra.Spec.FRRConfigurationSelector)
	if err != nil {
		return nil, nil, err
	}
	frrConfigs, err := c.frrLister.List(frrSelector)
	if err != nil {
		return nil, nil, err
	}
	if len(frrConfigs) == 0 {
		return nil, nil, fmt.Errorf("%w: no FRRConfigurations selected", errPending)
	}
	// ordered, so that processing and error messages are deterministic across
	// reconciles regardless of lister iteration order
	slices.SortFunc(frrConfigs, func(a, b *frrtypes.FRRConfiguration) int {
		return strings.Compare(a.Namespace+"/"+a.Name, b.Namespace+"/"+b.Name)
	})

	frrRouterVRFs := sets.New[string]()
	for _, frrConfig := range frrConfigs {
		if strings.HasPrefix(frrConfig.Name, generateName) {
			klog.V(4).Infof("Skipping FRRConfiguration %q selected by RouteAdvertisements %q as it was generated by ovn-kubernetes", frrConfig.Name, ra.Name)
			continue
		}
		nodeSelector, err := metav1.LabelSelectorAsSelector(&frrConfig.Spec.NodeSelector)
		if err != nil {
			return nil, nil, err
		}
		nodes, err := c.nodeLister.List(nodeSelector)
		if err != nil {
			return nil, nil, err
		}
		for _, node := range nodes {
			if _, selected := nodeToFRRConfig[node.Name]; !selected {
				// this RouteAdvertisements does not select this node, skip
				continue
			}
			nodeToFRRConfig[node.Name] = append(nodeToFRRConfig[node.Name], frrConfig)
		}
		for _, router := range frrConfig.Spec.BGP.Routers {
			frrRouterVRFs.Insert(router.VRF)
		}
	}

	// Validate EVPN configuration requirements
	hasEVPNConfig := len(selectedNetworks.macVRFConfigs) > 0 || len(selectedNetworks.ipVRFConfigs) > 0
	if hasEVPNConfig && !config.OVNKubernetesFeature.EnableEVPN {
		return nil, nil, fmt.Errorf("%w: EVPN networks selected but EVPN feature is not enabled", errConfig)
	}
	if hasEVPNConfig && config.Gateway.Mode != config.GatewayModeLocal {
		return nil, nil, fmt.Errorf("%w: EVPN networks selected but EVPN feature is only supported in local gateway mode", errConfig)
	}
	// Require a router with default VRF for any EVPN configuration, since the
	// global EVPN section with advertise-all-vni is required for EVPN to work properly.
	if hasEVPNConfig && !frrRouterVRFs.Has("") {
		return nil, nil, fmt.Errorf("%w: EVPN requires a router with default VRF but none were found in selected FRRConfigurations", errConfig)
	}
	// Validate IP-VRF networks: each needs either an existing VRF router or
	// the default VRF router to create one from.
	for _, cfg := range selectedNetworks.ipVRFConfigs {
		if !frrRouterVRFs.Has(cfg.VRFName) && !frrRouterVRFs.Has("") {
			return nil, nil, fmt.Errorf("%w: IP-VRF EVPN network %q requires a router with VRF %q or a router with default VRF, but none were found in selected FRRConfigurations", errConfig, cfg.NetworkName, cfg.VRFName)
		}
	}

	// Read per-node VTEP IPs from the k8s.ovn.org/vteps annotation written by
	// ovnkube-node. This runs after EVPN validation so c.vtepLister (only
	// initialized when EVPN is enabled) is safe to dereference.
	vtepCIDRSet := sets.New[string]()
	for _, vtepName := range sets.List(vtepNames) {
		vtep, err := c.vtepLister.Get(vtepName)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: VTEP %q referenced by EVPN network not found: %w", errConfig, vtepName, err)
		}
		for _, cidr := range vtep.Spec.CIDRs {
			vtepCIDRSet.Insert(string(cidr))
		}
	}
	vtepIPsByNode := map[string]sets.Set[string]{}
	for _, node := range nodes {
		vteps, err := util.ParseNodeVTEPs(node)
		if err != nil {
			if util.IsAnnotationNotSetError(err) {
				// Annotation not yet written; skip for now and rely on the
				// node update event (nodeNeedsUpdate) to re-trigger reconciliation
				// once the annotation is set.
				continue
			}
			// A malformed annotation must not be silently skipped: the
			// VTEP IPs for this node would not be advertised, breaking
			// VXLAN underlay reachability and all EVPN traffic to/from it.
			return nil, nil, fmt.Errorf("%w: failed to parse VTEP annotation for VTEPs %v on node %s: %w", errConfig, sets.List(vtepNames), node.Name, err)
		}
		for _, vtepName := range sets.List(vtepNames) {
			entry, ok := vteps[vtepName]
			if !ok || len(entry.IPs) == 0 {
				continue
			}
			if vtepIPsByNode[node.Name] == nil {
				vtepIPsByNode[node.Name] = sets.New[string]()
			}
			for _, ip := range entry.IPs {
				if utilnet.IsIPv6String(ip) {
					vtepIPsByNode[node.Name].Insert(ip + "/128")
				} else {
					vtepIPsByNode[node.Name].Insert(ip + "/32")
				}
			}
		}
	}
	selectedNetworks.vtepIPsByNode = make(map[string][]string, len(vtepIPsByNode))
	for node, ips := range vtepIPsByNode {
		selectedNetworks.vtepIPsByNode[node] = sets.List(ips)
	}
	selectedNetworks.vtepCIDRs = sets.List(vtepCIDRSet)

	// helper to gather host subnets and cache during reconcile
	// TODO perhaps cache across reconciles as well
	hostSubnets := map[string]map[string][]string{}
	getHostSubnets := func(nodeName string, network string) ([]string, error) {
		if _, parsed := hostSubnets[nodeName]; !parsed {
			node, err := c.nodeLister.Get(nodeName)
			if err != nil {
				return nil, err
			}
			subnets, err := util.ParseNodeHostSubnetsAnnotation(node)
			if err != nil {
				return nil, fmt.Errorf("%w: waiting for subnet annotation to be set for node %q: %w", errConfig, nodeName, err)
			}
			hostSubnets[nodeName] = make(map[string][]string, len(subnets))
			for network, subnet := range subnets {
				hostSubnets[nodeName][network] = util.StringSlice(subnet)
			}
		}
		return hostSubnets[nodeName][network], nil
	}

	// helper to gather egress ips and cache during reconcile
	// TODO perhaps cache across reconciles as well
	var eipsByNodesByNetworks map[string]map[string]sets.Set[string]
	getEgressIPsByNode := func(nodeName string) (map[string]sets.Set[string], error) {
		if eipsByNodesByNetworks == nil {
			eipsByNodesByNetworks, err = c.getEgressIPsByNodesByNetworks(networkSet)
			if err != nil {
				return nil, err
			}
		}
		return eipsByNodesByNetworks[nodeName], nil
	}

	// helper to gather the following prefixes:
	//  - EgressIPs
	//  - host subnets for networks with networkTopology layer3
	//  - network subnets for networks with networkTopology layer2
	getPrefixes := func(nodeName, network, networkTopology string, networkSubnets []string) ([]string, error) {
		// gather host subnets
		var subnets []string
		if advertisements.Has(ratypes.PodNetwork) {
			if networkTopology == types.Layer2Topology {
				subnets = networkSubnets
				if len(subnets) == 0 {
					return nil, fmt.Errorf("%w: no layer2 subnets found", errConfig)
				}
			} else {
				subnets, err = getHostSubnets(nodeName, network)
				if err != nil {
					return nil, fmt.Errorf("%w: will wait for subnet annotation to be set for node %q and network %q: %w", errConfig, nodeName, network, err)
				}
				if len(subnets) == 0 {
					return nil, fmt.Errorf("%w: will wait for subnet annotation to be set for node %q and network %q", errConfig, nodeName, network)
				}
			}

		}
		// gather EgressIPs
		var eips []string
		if advertisements.Has(ratypes.EgressIP) {
			eipsByNode, err := getEgressIPsByNode(nodeName)
			if err != nil {
				return nil, err
			}
			eips = eipsByNode[network].UnsortedList()
		}

		prefixes := make([]string, 0, len(subnets)+len(eips))
		prefixes = append(prefixes, subnets...)
		prefixes = append(prefixes, eips...)
		return prefixes, nil
	}

	generated := []*frrtypes.FRRConfiguration{}
	// iterate nodes in their sorted order rather than ranging over the map, so
	// that processing and error messages are deterministic across reconciles
	for _, node := range nodes {
		nodeName := node.Name
		frrConfigs := nodeToFRRConfig[nodeName]
		// reset node specific information
		selectedNetworks.hostNetworkSubnets = map[string][]string{}
		selectedNetworks.hostSubnets = []string{}

		// gather node specific information
		nodeNetworks := make([]string, 0, len(selectedNetworks.networks))
		for _, network := range selectedNetworks.networks {
			if !c.nm.NodeHasNetwork(nodeName, network) {
				// only advertise a network on nodes where it is active, that
				// is, with pods or EgressIPs attached to it; NodeHasNetwork
				// returns false only with dynamic UDN allocation enabled.
				// The network manager notifies us when a network goes active
				// or inactive on a node.
				continue
			}
			if config.OVNKubernetesFeature.EnableDynamicUDNAllocation &&
				!config.Layer2UsesTransitRouter &&
				selectedNetworks.networkTopology[network] == types.Layer2Topology &&
				!c.nodeHasLayer2Allocation(nodeName, network) {
				// Legacy layer2 topology uses the tunnel ID allocation as a
				// signal that the network is rendered on the node. The allocation
				// is stored as a node annotation whose update triggers this
				// reconcile. Transit-router topology intentionally has no such
				// allocation, so NodeHasNetwork above is the available signal there.
				// TODO: replace with a per-node network status once
				// available, to know when the network is actually rendered.
				continue
			}
			nodeNetworks = append(nodeNetworks, network)
			selectedNetworks.hostNetworkSubnets[network], err = getPrefixes(nodeName, network,
				selectedNetworks.networkTopology[network], selectedNetworks.networkSubnets[network])
			if err != nil {
				return nil, nil, err
			}
			selectedNetworks.hostSubnets = append(selectedNetworks.hostSubnets, selectedNetworks.hostNetworkSubnets[network]...)
			// ordered
			slices.Sort(selectedNetworks.hostNetworkSubnets[network])
		}
		// order, dedup
		selectedNetworks.hostSubnets = sets.List(sets.New(selectedNetworks.hostSubnets...))

		// if there is no prefixes to advertise for this node, skip it
		if len(selectedNetworks.hostSubnets) == 0 {
			continue
		}

		matchedNetworks := sets.New[string]()
		for _, frrConfig := range frrConfigs {
			// generate FRRConfiguration for each source FRRConfiguration/node combination
			new, err := c.generateFRRConfiguration(
				ra,
				frrConfig,
				nodeName,
				selectedNetworks,
				matchedNetworks,
				frrRouterVRFs,
			)
			if err != nil {
				return nil, nil, err
			}
			if new == nil {
				// if we got nil, we didn't match any VRF
				if ra.Spec.TargetVRF == "auto" && frrConfigOnlyMatchesInactiveNetworks(frrConfig, selectedNetworks, nodeNetworks) {
					// this FRRConfiguration only carries routers for VRFs of
					// selected networks that are not active on this node:
					// skip it for this node instead of failing, mirroring the
					// per-network skip above.
					continue
				}
				return nil, nil, fmt.Errorf("%w: FRRConfiguration %q selected for node %q has no VRF matching the RouteAdvertisements target VRF or any selected network",
					errConfig, frrConfig.Name, nodeName)
			}
			generated = append(generated, new)
		}
		// check that we matched all the networks selected for this node on 'auto'
		if ra.Spec.TargetVRF == "auto" && !matchedNetworks.HasAll(nodeNetworks...) {
			return nil, nil, fmt.Errorf("%w: selected FRRConfigurations for node %q don't match all selected networks with target VRF 'auto'", errConfig, nodeName)
		}
	}

	recordAdvertisedPrefixes(ra.Name, generated)

	return generated, nads, nil
}

// recordAdvertisedPrefixes reports the number of distinct prefixes a
// RouteAdvertisements advertises, split by IP family.
//
// Derived from the generated FRRConfigurations rather than from the per-node
// candidate prefixes: a selected node whose networks matched no source
// FRRConfiguration produces no advertisement, and generation adds prefixes of
// its own, such as the EVPN VTEP host routes.
func recordAdvertisedPrefixes(ra string, generated []*frrtypes.FRRConfiguration) {
	v4, v6 := countAdvertisedPrefixes(generated)
	metrics.RecordRouteAdvertisementAdvertisedPrefixes(ra, "ipv4", v4)
	metrics.RecordRouteAdvertisementAdvertisedPrefixes(ra, "ipv6", v6)
}

// countAdvertisedPrefixes counts the distinct prefixes across the generated
// FRRConfigurations, by IP family. The same prefix advertised from several
// nodes, or by several routers, counts once.
func countAdvertisedPrefixes(generated []*frrtypes.FRRConfiguration) (v4, v6 int) {
	prefixes := sets.New[string]()
	for _, frrConfig := range generated {
		for _, router := range frrConfig.Spec.BGP.Routers {
			prefixes.Insert(router.Prefixes...)
		}
	}
	for prefix := range prefixes {
		if utilnet.IsIPv6CIDRString(prefix) {
			v6++
			continue
		}
		v4++
	}
	return v4, v6
}

// nodeHasLayer2Allocation reports whether the node has a gateway router LRP
// tunnel ID allocated for the layer2 network.
func (c *Controller) nodeHasLayer2Allocation(nodeName, network string) bool {
	node, err := c.nodeLister.Get(nodeName)
	if err != nil {
		return false
	}
	return util.HasUDNLayer2NodeGRLRPTunnelID(node, network)
}

// frrConfigOnlyMatchesInactiveNetworks helps decide whether an
// FRRConfiguration that matched no router for a node is a configuration
// error. It is not an error if the FRRConfiguration is dedicated to selected
// networks that are simply not active on the node: that is, its router
// VRFs reference at least one selected network, and none of the referenced
// networks are among the ones advertised on the node (nodeNetworks). Used
// with target VRF 'auto' to skip such FRRConfigurations for the node.
func frrConfigOnlyMatchesInactiveNetworks(frrConfig *frrtypes.FRRConfiguration, selected *selectedNetworks, nodeNetworks []string) bool {
	advertised := sets.New(nodeNetworks...)
	referencesSelected := false
	for _, router := range frrConfig.Spec.BGP.Routers {
		network := selected.networkVRFs[router.VRF]
		if router.VRF == "" && slices.Contains(selected.networks, types.DefaultNetworkName) {
			network = types.DefaultNetworkName
		}
		if network == "" {
			continue
		}
		referencesSelected = true
		if advertised.Has(network) {
			return false
		}
	}
	return referencesSelected
}

// generateFRRConfiguration generates a FRRConfiguration from a source for a
// specific node. Also fills matchedNetworks with the networks that have a VRF
// that matched any router VRF of the FRRConfiguration.
func (c *Controller) generateFRRConfiguration(
	ra *ratypes.RouteAdvertisements,
	source *frrtypes.FRRConfiguration,
	nodeName string,
	selectedNetworks *selectedNetworks,
	matchedNetworks sets.Set[string],
	frrRouterVRFs sets.Set[string],
) (*frrtypes.FRRConfiguration, error) {
	var routers []frrtypes.Router

	// track neighbors and ASNs to generate raw config later on
	vrfNeighbors := map[string][]string{}
	vrfASNs := map[string]uint32{}

	// go over the source routers
	for i, router := range source.Spec.BGP.Routers {

		targetVRF := ra.Spec.TargetVRF
		var matchedVRF, matchedNetwork string
		var advertisePrefixes []string

		// We will use the router if:
		// - the router VRF matches the target VRF
		// - if the target VRF is 'auto', the router VRF is that of a selected network
		// Prepare each scenario with a switch statement and check after that
		switch {
		case targetVRF == "auto" && router.VRF == "":
			// match on default network/VRF, advertise node prefixes
			matchedVRF = ""
			matchedNetwork = types.DefaultNetworkName
			advertisePrefixes = selectedNetworks.hostNetworkSubnets[matchedNetwork]
		case targetVRF == "auto":
			// match router.VRF to network.VRF, advertise node prefixes
			matchedVRF = router.VRF
			matchedNetwork = selectedNetworks.networkVRFs[matchedVRF]
			advertisePrefixes = selectedNetworks.hostNetworkSubnets[matchedNetwork]
		case targetVRF == "":
			// match on default network/VRF, advertise node prefixes
			matchedVRF = ""
			matchedNetwork = types.DefaultNetworkName
			advertisePrefixes = selectedNetworks.hostSubnets
		default:
			// match router.VRF to network.VRF, advertise node prefixes
			matchedVRF = targetVRF
			matchedNetwork = selectedNetworks.networkVRFs[matchedVRF]
			advertisePrefixes = selectedNetworks.hostSubnets
		}
		if matchedVRF != router.VRF || len(advertisePrefixes) == 0 {
			// either this router VRF does not match the target VRF or we don't
			// have prefixes for it (which might be due to this RA not selecting
			// this network, but not just)
			continue
		}
		matchedNetworks.Insert(matchedNetwork)

		// Collect pod subnets from all selected no-overlay networks
		var allNoOverlayPodSubnets []string
		for _, networkName := range selectedNetworks.networks {
			if selectedNetworks.networkTransport[networkName] == types.NetworkTransportNoOverlay {
				// Get the pod subnets for this network (the network subnets, not host subnets)
				if podSubnets := selectedNetworks.networkSubnets[networkName]; len(podSubnets) > 0 {
					allNoOverlayPodSubnets = append(allNoOverlayPodSubnets, podSubnets...)
				}
			}
		}

		// if this router's VRF matches the target VRF, copy it and set the
		// prefixes as appropriate
		targetRouter := router
		targetRouter.Prefixes = advertisePrefixes

		// For managed FRRConfigurations, the base config lists all nodes as
		// neighbors. When generating the per-node FRRConfiguration, we need
		// to exclude the node itself from its own neighbor list to avoid
		// self-peering. Look up the node's primary interface addresses so
		// we can filter them out below.
		node, err := c.nodeLister.Get(nodeName)
		if err != nil {
			return nil, fmt.Errorf("failed to get node %s: %w", nodeName, err)
		}
		nodeIfAddr, err := util.GetNodeIfAddrAnnotation(node)
		if err != nil {
			return nil, fmt.Errorf("failed to get node %s primary interface address annotation: %w", nodeName, err)
		}
		// Strip CIDR mask to get bare IP strings for neighbor comparison.
		nodeV4, _, _ := strings.Cut(nodeIfAddr.IPv4, "/")
		nodeV6, _, _ := strings.Cut(nodeIfAddr.IPv6, "/")

		dpuHostGatewayNextHops, err := c.getDPUHostGatewayNextHops(node, selectedNetworks, matchedNetwork)
		if err != nil {
			return nil, err
		}

		targetRouter.Neighbors = make([]frrtypes.Neighbor, 0, len(source.Spec.BGP.Routers[i].Neighbors))
		for _, neighbor := range source.Spec.BGP.Routers[i].Neighbors {
			// Validate the neighbor identifier upfront, before any filtering
			// might skip the neighbor, so that an invalid neighbor is
			// consistently reported as a configuration error
			key := neighborKey(neighbor)
			if key == "" {
				return nil, fmt.Errorf("%w: neighbor with neither address nor interface on FRRConfiguration %s/%s",
					errConfig,
					source.Namespace,
					source.Name,
				)
			}

			// Skip neighbors that are the node itself
			if (nodeV4 != "" && neighbor.Address == nodeV4) || (nodeV6 != "" && neighbor.Address == nodeV6) {
				continue
			}

			// If the dual-stack address family is enabled then a BGP session
			// carries prefixes of both IPv4 and IPv6 families. Our problem is
			// that with an IPv4 session, FRR can incorrectly pick the
			// masquerade IPv6 address (instead of the real address) as next
			// hop for IPv6 prefixes and that won't work. Note that with a
			// dedicated IPv6 session that can't happen since FRR will use the
			// same address that was used to establish the session. Enforce
			// address-family-specific sessions for now.
			if neighbor.DualStackAddressFamily {
				return nil, fmt.Errorf("%w: DualStackAddressFamily==true not supported, seen on FRRConfiguration %s/%s neighbor %s",
					errConfig,
					source.Namespace,
					source.Name,
					neighbor.Address,
				)
			}

			// FRR establishes an unnumbered session over an IPv4 address
			// derived from a /30 or /31 on the interface or, failing that,
			// over the peer's IPv6 link-local address. In either case the
			// session can carry prefixes of both families (RFC 8950), so
			// don't filter by family.
			isUnnumbered := isUnnumberedNeighbor(neighbor)
			isIPV6 := utilnet.IsIPv6String(neighbor.Address)
			advertisePrefixesForNeighbor := advertisePrefixes
			if !isUnnumbered {
				advertisePrefixesForNeighbor = util.MatchAllIPNetsStringFamily(isIPV6, advertisePrefixes)
			}
			if len(advertisePrefixesForNeighbor) == 0 {
				continue
			}

			neighbor.ToAdvertise = frrtypes.Advertise{
				Allowed: frrtypes.AllowedOutPrefixes{
					Mode:     frrtypes.AllowRestricted,
					Prefixes: advertisePrefixesForNeighbor,
				},
			}
			if isUnnumbered {
				neighbor.ToAdvertise.NextHop.IPv4 = dpuHostGatewayNextHops[false]
				neighbor.ToAdvertise.NextHop.IPv6 = dpuHostGatewayNextHops[true]
			} else if nextHop := dpuHostGatewayNextHops[isIPV6]; nextHop != "" {
				if isIPV6 {
					neighbor.ToAdvertise.NextHop.IPv6 = nextHop
				} else {
					neighbor.ToAdvertise.NextHop.IPv4 = nextHop
				}
			}

			// For no-overlay networks, add routes to pod subnets to the accepted routes list
			// frr-k8s will merge the prefixes from both the generated and the base FRRConfiguration
			if len(allNoOverlayPodSubnets) > 0 {
				// Filter pod subnets by IP family to match the neighbor;
				// unnumbered neighbors carry both families
				filteredPodSubnets := allNoOverlayPodSubnets
				if !isUnnumbered {
					filteredPodSubnets = util.MatchAllIPNetsStringFamily(isIPV6, allNoOverlayPodSubnets)
				}
				if len(filteredPodSubnets) > 0 {
					neighbor.ToReceive = frrtypes.Receive{
						Allowed: frrtypes.AllowedInPrefixes{
							Mode: frrtypes.AllowRestricted,
						},
					}
					for _, subnet := range filteredPodSubnets {
						neighbor.ToReceive.Allowed.Prefixes = append(neighbor.ToReceive.Allowed.Prefixes, frrtypes.PrefixSelector{
							Prefix: subnet,
							LE:     selectedNetworks.prefixLength[subnet],
							GE:     selectedNetworks.prefixLength[subnet],
						})
					}
				}
			}

			vrfNeighbors[matchedVRF] = append(vrfNeighbors[matchedVRF], key)
			targetRouter.Neighbors = append(targetRouter.Neighbors, neighbor)
		}
		if len(targetRouter.Neighbors) == 0 {
			// we ended up with no neighbor
			continue
		}

		// append this router to the list of routers we will include in the
		// generated FRR config and track its index as we might need to add
		// imports to it
		vrfASNs[matchedVRF] = router.ASN
		routers = append(routers, targetRouter)
		targetRouterIndex := len(routers) - 1

		// VRFs are isolated in "auto" so no need to handle imports
		if targetVRF == "auto" {
			continue
		}

		// before handling imports, lets normalize the VRF for the default
		// network: when doing imports, the default VRF is is referred to as
		// "default" instead of ""
		if matchedVRF == "" {
			matchedVRF = types.DefaultNetworkName
		}

		// handle imports: when the target VRF is not "auto" we need to leak
		// between the target VRF and the selected networks, reciprocally
		// importing from each
		for _, vrf := range selectedNetworks.vrfs { // ordered
			// skip self
			if vrf == matchedVRF {
				continue
			}

			// import all other selected networks into this router's network.
			routers[targetRouterIndex].Imports = append(routers[targetRouterIndex].Imports, frrtypes.Import{VRF: vrf})

			// add an additional router to import the target VRF into selected
			// network
			importRouter := frrtypes.Router{
				ASN:     router.ASN,
				ID:      router.ID,
				Imports: []frrtypes.Import{{VRF: matchedVRF}},
			}
			if vrf != types.DefaultNetworkName {
				importRouter.VRF = vrf
			}
			routers = append(routers, importRouter)
		}
	}

	hasEVPN := len(selectedNetworks.macVRFConfigs) > 0 || len(selectedNetworks.ipVRFConfigs) > 0
	if hasEVPN && vrfASNs[""] == 0 {
		// Look for global router in the source FRRConfiguration, not in the filtered routers
		for _, router := range source.Spec.BGP.Routers {
			if router.VRF == "" { // default VRF
				vrfASNs[""] = router.ASN
				vrfNeighbors[""] = make([]string, 0, len(router.Neighbors))
				for _, neighbor := range router.Neighbors {
					key := neighborKey(neighbor)
					if key == "" {
						return nil, fmt.Errorf("%w: neighbor with neither address nor interface on FRRConfiguration %s/%s",
							errConfig,
							source.Namespace,
							source.Name,
						)
					}
					vrfNeighbors[""] = append(vrfNeighbors[""], key)
				}
				break
			}
		}
	}

	// For IP-VRF: Find or create routers for each EVPN network's VRF.
	// IP-VRF routers don't need neighbors for EVPN (they use the global router's neighbors).
	ipVRFNetworks := sets.New[string]()
	for _, cfg := range selectedNetworks.ipVRFConfigs {
		ipVRFNetworks.Insert(cfg.NetworkName)
		if frrRouterVRFs.Has(cfg.VRFName) {
			// VRF router exists somewhere - check if it's in the current source
			for _, router := range source.Spec.BGP.Routers {
				if router.VRF == cfg.VRFName {
					vrfASNs[cfg.VRFName] = router.ASN
					if !slices.ContainsFunc(routers, func(r frrtypes.Router) bool { return r.VRF == cfg.VRFName }) {
						routers = append(routers, frrtypes.Router{
							ASN:      router.ASN,
							VRF:      cfg.VRFName,
							Prefixes: selectedNetworks.hostNetworkSubnets[cfg.NetworkName],
						})
					}
					break
				}
			}
			// If not in current source, another source will handle it
		} else if vrfASNs[""] > 0 {
			// VRF router doesn't exist anywhere - create with global ASN
			klog.Infof("Creating router for EVPN network %q VRF %q with ASN=%d, prefixes=%v",
				cfg.NetworkName, cfg.VRFName, vrfASNs[""], selectedNetworks.hostNetworkSubnets[cfg.NetworkName])
			matchedNetworks.Insert(cfg.NetworkName)
			vrfASNs[cfg.VRFName] = vrfASNs[""]
			routers = append(routers, frrtypes.Router{
				ASN:      vrfASNs[""],
				VRF:      cfg.VRFName,
				Prefixes: selectedNetworks.hostNetworkSubnets[cfg.NetworkName],
			})
		}
	}

	// MAC-VRF only EVPN networks with targetVRF == "auto" are handled
	// by the global router's EVPN raw config (advertise-all-vni) rather
	// than by a VRF-specific router. Mark them as matched when a global
	// router with neighbors is present.
	if ra.Spec.TargetVRF == "auto" && vrfASNs[""] > 0 && len(vrfNeighbors[""]) > 0 {
		for _, cfg := range selectedNetworks.macVRFConfigs {
			if !ipVRFNetworks.Has(cfg.NetworkName) {
				matchedNetworks.Insert(cfg.NetworkName)
			}
		}
	}

	// For EVPN underlay reachability, advertise VTEP IPs as /32 (or /128) in
	// the default-VRF router's address-family ipv4/ipv6 unicast.
	//
	// When targetVRF is "auto" or "", the main router matching loop above
	// already includes the default-VRF router in the generated routers, so we
	// append the VTEP IPs to it. When targetVRF is a specific VRF name (e.g.
	// "red"), only that VRF's router is in the generated set — the default-VRF
	// router is not included even though it exists in the source (confirmed by globalRouterASN > 0 above).
	// In that case we create a new default-VRF router from the source
	// to carry the VTEP IPs.
	if vtepIPs := selectedNetworks.vtepIPsByNode[nodeName]; len(vtepIPs) > 0 && vrfASNs[""] > 0 {
		// Build ToReceive prefix selectors from the VTEP CIDRs so each
		// node accepts routes for all VTEP IPs within these ranges.
		vtepReceiveSelectors := vtepCIDRPrefixSelectors(selectedNetworks.vtepCIDRs)

		defaultIdx := slices.IndexFunc(routers, func(r frrtypes.Router) bool { return r.VRF == "" })
		if defaultIdx >= 0 {
			// dedup, ordered
			// router level - injects the prefix into BGP
			routers[defaultIdx].Prefixes = sets.List(sets.New(routers[defaultIdx].Prefixes...).Insert(vtepIPs...))
			// neighbor level - advertise this node's VTEP IPs and accept VTEP
			// CIDRs; unnumbered neighbors carry both families
			for i := range routers[defaultIdx].Neighbors {
				allPrefixes := routers[defaultIdx].Prefixes
				isUnnumbered := isUnnumberedNeighbor(routers[defaultIdx].Neighbors[i])
				isIPV6 := utilnet.IsIPv6String(routers[defaultIdx].Neighbors[i].Address)
				routers[defaultIdx].Neighbors[i].ToAdvertise.Allowed.Prefixes = allPrefixes
				if !isUnnumbered {
					routers[defaultIdx].Neighbors[i].ToAdvertise.Allowed.Prefixes =
						util.MatchAllIPNetsStringFamily(isIPV6, allPrefixes)
				}
				for _, ps := range vtepReceiveSelectors {
					if isUnnumbered || utilnet.IsIPv6CIDRString(ps.Prefix) == isIPV6 {
						routers[defaultIdx].Neighbors[i].ToReceive.Allowed.Prefixes = append(
							routers[defaultIdx].Neighbors[i].ToReceive.Allowed.Prefixes, ps)
					}
				}
				if len(routers[defaultIdx].Neighbors[i].ToReceive.Allowed.Prefixes) > 0 {
					routers[defaultIdx].Neighbors[i].ToReceive.Allowed.Mode = frrtypes.AllowRestricted
				}
			}
		} else {
			// No default-VRF router in the generated set; clone the source's
			// default-VRF router to carry VTEP IPs, preserving all router-level
			// settings (ID, Imports, etc.) from the source.
			for _, router := range source.Spec.BGP.Routers {
				if router.VRF != "" {
					continue
				}
				vtepRouter := router
				// Only VTEP IPs go into Prefixes — the source's original
				// default-VRF prefixes are not this RA's responsibility
				// (they'd be advertised by a separate RA with targetVRF=""
				// or "auto" that includes the default-VRF router).
				vtepRouter.Prefixes = vtepIPs
				vtepRouter.Neighbors = nil // will rebuild below
				for _, neighbor := range router.Neighbors {
					if neighbor.DualStackAddressFamily {
						return nil, fmt.Errorf("%w: DualStackAddressFamily==true not supported, seen on FRRConfiguration %s/%s neighbor %s",
							errConfig,
							source.Namespace,
							source.Name,
							neighbor.Address,
						)
					}
					// unnumbered neighbors carry both families
					isUnnumbered := isUnnumberedNeighbor(neighbor)
					isIPV6 := utilnet.IsIPv6String(neighbor.Address)
					filteredVTEPIPs := vtepIPs
					if !isUnnumbered {
						filteredVTEPIPs = util.MatchAllIPNetsStringFamily(isIPV6, vtepIPs)
					}
					if len(filteredVTEPIPs) == 0 {
						continue
					}
					n := neighbor
					n.ToAdvertise = frrtypes.Advertise{
						Allowed: frrtypes.AllowedOutPrefixes{
							Mode:     frrtypes.AllowRestricted,
							Prefixes: filteredVTEPIPs,
						},
					}
					for _, ps := range vtepReceiveSelectors {
						if isUnnumbered || utilnet.IsIPv6CIDRString(ps.Prefix) == isIPV6 {
							n.ToReceive.Allowed.Prefixes = append(n.ToReceive.Allowed.Prefixes, ps)
						}
					}
					if len(n.ToReceive.Allowed.Prefixes) > 0 {
						n.ToReceive.Allowed.Mode = frrtypes.AllowRestricted
					}
					vtepRouter.Neighbors = append(vtepRouter.Neighbors, n)
				}
				if len(vtepRouter.Neighbors) > 0 {
					routers = append(routers, vtepRouter)
				}
				break
			}
		}
	}

	// Generate raw config, if any.
	// TODO: once frr-k8s provides a typed API for this config, we can use that instead of raw config
	rawConfig := generateRawConfig(selectedNetworks, vrfNeighbors, vrfASNs)
	if len(routers) == 0 && rawConfig == "" {
		// we ended up with no routers and no raw config to generate, bail out
		return nil, nil
	}

	new := &frrtypes.FRRConfiguration{}
	new.GenerateName = generateName
	new.Namespace = source.Namespace
	// label the FRRConfigurations with the RA name, we use this to find the
	// existing set of FRRConfigurations that need to be reconciled for a given
	// RA
	new.Labels = map[string]string{
		types.OvnRouteAdvertisementsKey: ra.Name,
	}
	// annotate each generated FRRConfiguration with a unique key
	// (ra/source/node) which is used in the reconciliation to know whether an
	// existing FRRConfiguration should be deleted or not.
	new.Annotations = map[string]string{
		types.OvnRouteAdvertisementsKey: fmt.Sprintf("%s/%s/%s", ra.Name, source.Name, nodeName),
	}
	new.Spec = source.Spec
	new.Spec.BGP.Routers = routers
	new.Spec.NodeSelector = metav1.LabelSelector{
		MatchLabels: map[string]string{
			"kubernetes.io/hostname": nodeName,
		},
	}
	if rawConfig != "" {
		new.Spec.Raw = frrtypes.RawConfig{
			Priority: rawConfigPriority,
			Config:   rawConfig,
		}
	}

	return new, nil
}

func getDPUHostGatewayNextHops(node *corev1.Node) (map[bool]string, error) {
	// ParseNodeL3GatewayAnnotation also requires the chassis ID for enabled
	// gateways, but reports a missing chassis ID as a config error. Check it
	// first so a DPU host that is still initializing leaves the RA pending.
	if _, err := util.ParseNodeChassisIDAnnotation(node); err != nil {
		if util.IsAnnotationNotSetError(err) {
			return nil, fmt.Errorf("%w: waiting for chassis ID annotation to be set for DPU host node %q: %w",
				errPending, node.Name, err)
		}
		return nil, fmt.Errorf("%w: failed to parse chassis ID annotation for DPU host node %q: %w",
			errConfig, node.Name, err)
	}

	gatewayConfig, err := util.ParseNodeL3GatewayAnnotation(node)
	if err != nil {
		if util.IsAnnotationNotSetError(err) {
			return nil, fmt.Errorf("%w: waiting for L3 gateway annotation to be set for DPU host node %q: %w",
				errPending, node.Name, err)
		}
		return nil, fmt.Errorf("%w: failed to parse L3 gateway annotation for DPU host node %q: %w",
			errConfig, node.Name, err)
	}
	nextHops := map[bool]string{}
	for _, ipNet := range gatewayConfig.IPAddresses {
		if ipNet == nil || ipNet.IP == nil {
			continue
		}
		nextHops[ipNet.IP.To4() == nil] = ipNet.IP.String()
	}
	if len(nextHops) == 0 {
		return nil, fmt.Errorf("%w: no shared gateway IP addresses found for DPU host node %q", errConfig, node.Name)
	}
	return nextHops, nil
}

func (c *Controller) getDPUHostGatewayNextHops(
	node *corev1.Node,
	selectedNetworks *selectedNetworks,
	networkName string,
) (map[bool]string, error) {
	if config.Gateway.Mode != config.GatewayModeShared {
		return nil, nil
	}
	if _, err := util.GetNodePrimaryDPUHostAddrAnnotation(node); err != nil {
		if util.IsAnnotationNotSetError(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("%w: failed to parse primary DPU host address annotation for node %q: %w",
			errConfig, node.Name, err)
	}

	uplinkName := selectedNetworks.networkUplinks[networkName]
	if uplinkName == "" || c.uplinkStateLister == nil {
		return getDPUHostGatewayNextHops(node)
	}
	stateName := uplinkutil.StateName(uplinkName, node.Name)
	state, err := uplinkutil.GetState(c.uplinkStateLister, uplinkName, node.Name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("%w: waiting for UplinkState %q to be set for DPU host node %q",
				errPending, stateName, node.Name)
		}
		return nil, fmt.Errorf("%w: failed to get UplinkState %q for DPU host node %q: %w",
			errConfig, stateName, node.Name, err)
	}
	if !meta.IsStatusConditionTrue(state.Status.Conditions, uplinkv1alpha1.UplinkStateConditionResolved) {
		return nil, fmt.Errorf("%w: waiting for UplinkState %q to become resolved for DPU host node %q",
			errPending, stateName, node.Name)
	}

	nextHops := map[bool]string{}
	for _, ipCIDR := range state.Status.IPAddresses {
		ip, _, err := net.ParseCIDR(string(ipCIDR))
		if err != nil {
			return nil, fmt.Errorf("%w: failed to parse UplinkState %q IP address %q: %w",
				errConfig, stateName, ipCIDR, err)
		}
		nextHops[ip.To4() == nil] = ip.String()
	}
	if len(nextHops) == 0 {
		return nil, fmt.Errorf("%w: no IP addresses found in UplinkState %q for DPU host node %q",
			errConfig, stateName, node.Name)
	}
	return nextHops, nil
}

// vtepCIDRPrefixSelectors converts VTEP CIDRs into FRR PrefixSelectors that
// accept host routes (/32 for IPv4, /128 for IPv6) within each CIDR range.
func vtepCIDRPrefixSelectors(cidrs []string) []frrtypes.PrefixSelector {
	selectors := make([]frrtypes.PrefixSelector, 0, len(cidrs))
	for _, cidr := range cidrs {
		var le uint32
		if utilnet.IsIPv6CIDRString(cidr) {
			le = 128
		} else {
			le = 32
		}
		selectors = append(selectors, frrtypes.PrefixSelector{Prefix: cidr, LE: le, GE: le})
	}
	return selectors
}

// updateFRRConfigurations updates the FRRConfigurations that apply for a
// RouteAdvertisements. It fetches existing FRRConfigurations by label and
// indexes them by the annotated key. Then compares this state with desired
// state and creates, updates or deletes the FRRConfigurations accordingly.
func (c *Controller) updateFRRConfigurations(ra string, frrConfigurations []*frrtypes.FRRConfiguration) (bool, error) {
	var hadUpdates bool

	metrics.RecordRouteAdvertisementFRRConfigurations(ra, len(frrConfigurations))

	// fetch the currently existing FRRConfigurations for this
	// RouteAdvertisements
	selector, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{
		MatchLabels: map[string]string{types.OvnRouteAdvertisementsKey: ra},
	})
	if err != nil {
		return hadUpdates, err
	}
	frrConfigs, err := c.frrLister.List(selector)
	if err != nil {
		return hadUpdates, err
	}
	// map them by our internal unique key
	existing := make(map[string][]*frrtypes.FRRConfiguration, len(frrConfigs))
	for _, frrConfig := range frrConfigs {
		key := frrConfig.Annotations[types.OvnRouteAdvertisementsKey]
		if key == "" {
			continue
		}
		existing[key] = append(existing[key], frrConfig)
	}

	// go through the FRRConfigurations that should exist for this
	// RouteAdvertisements
	for _, newFRRConfig := range frrConfigurations {
		key := newFRRConfig.Annotations[types.OvnRouteAdvertisementsKey]
		oldFRRConfigs := existing[key]

		if len(oldFRRConfigs) == 0 {
			// does not exist, create
			_, err := c.frrClient.ApiV1beta1().FRRConfigurations(newFRRConfig.Namespace).Create(
				context.Background(),
				newFRRConfig,
				metav1.CreateOptions{
					FieldManager: fieldManager,
				},
			)
			if err != nil {
				return hadUpdates, err
			}
			metrics.RecordRouteAdvertisementFRRConfigurationWrite(ra, frrConfigOpCreate)
			hadUpdates = true
			continue
		}

		// If it already exists, update. Unexpected user actions can lead us to
		// have multiple FRRConfigurations with the same key, in that case we
		// pick one to update and delete the rest.
		oldFRRConfig := oldFRRConfigs[len(oldFRRConfigs)-1]
		existing[key] = oldFRRConfigs[:len(oldFRRConfigs)-1]

		// no changes needed so skip
		if reflect.DeepEqual(newFRRConfig.Spec, oldFRRConfig.Spec) {
			metrics.RecordRouteAdvertisementFRRConfigurationWrite(ra, frrConfigOpUnchanged)
			continue
		}

		// otherwise update
		newFRRConfig.Name = oldFRRConfig.Name
		newFRRConfig.ResourceVersion = oldFRRConfig.ResourceVersion
		_, err := c.frrClient.ApiV1beta1().FRRConfigurations(newFRRConfig.Namespace).Update(
			context.Background(),
			newFRRConfig,
			metav1.UpdateOptions{
				FieldManager: fieldManager,
			},
		)
		if err != nil {
			return hadUpdates, err
		}
		metrics.RecordRouteAdvertisementFRRConfigurationWrite(ra, frrConfigOpUpdate)
		hadUpdates = true
	}

	// delete FRRConfigurations that should not exist
	for _, obsoleteFRRConfigs := range existing {
		for _, obsoleteFRRConfig := range obsoleteFRRConfigs {
			err := c.frrClient.ApiV1beta1().FRRConfigurations(obsoleteFRRConfig.Namespace).Delete(
				context.Background(),
				obsoleteFRRConfig.Name,
				metav1.DeleteOptions{},
			)
			if err != nil && !apierrors.IsNotFound(err) {
				return hadUpdates, err
			}
			metrics.RecordRouteAdvertisementFRRConfigurationWrite(ra, frrConfigOpDelete)
			hadUpdates = true
		}
	}

	return hadUpdates, nil
}

// updateNADs updates the annotation of the NADs that apply for a
// RouteAdvertisements. It iterates all the existing NADs updating the
// annotation accordingly, adding or removing the RouteAdvertisements reference
// as needed.
func (c *Controller) updateNADs(ra string, nads []*nadtypes.NetworkAttachmentDefinition) (bool, error) {
	var hadUpdates bool
	selected := sets.New[string]()
	for _, nad := range nads {
		selected.Insert(nad.Namespace + "/" + nad.Name)
	}

	nads, err := c.nadLister.List(labels.Everything())
	if err != nil {
		return hadUpdates, err
	}
	metrics.RecordRouteAdvertisementNADsListed(len(nads))

	k := kube.KubeOVN{
		NADClient: c.nadClient,
	}

	// go through all the NADs and update the annotation adding or removing the
	// reference to this RouteAdvertisements as required
	for _, nad := range nads {
		var ras []string

		if nad.Annotations[types.OvnRouteAdvertisementsKey] != "" {
			err := json.Unmarshal([]byte(nad.Annotations[types.OvnRouteAdvertisementsKey]), &ras)
			if err != nil {
				return hadUpdates, err
			}
		}

		raSet := sets.New(ras...)
		nadName := nad.Namespace + "/" + nad.Name
		if selected.Has(nadName) {
			raSet.Insert(ra)
			selected.Delete(nadName)
		} else {
			raSet.Delete(ra)
		}

		if len(ras) == raSet.Len() {
			continue
		}

		nadRAjson, err := json.Marshal(raSet.UnsortedList())
		if err != nil {
			return hadUpdates, err
		}

		err = k.SetAnnotationsOnNAD(
			nad.Namespace,
			nad.Name,
			map[string]string{
				types.OvnRouteAdvertisementsKey: string(nadRAjson),
			},
			fieldManager,
		)
		if err != nil {
			return hadUpdates, fmt.Errorf("failed to annotate NAD %q: %w", nad.Name, err)
		}
		metrics.RecordRouteAdvertisementNADWrite(ra)

		hadUpdates = true
	}
	if selected.Len() != 0 {
		return hadUpdates, fmt.Errorf("failed to annotate NADs that were not found %v", selected.UnsortedList())
	}

	return hadUpdates, nil
}

// updateRAStatus update the RouteAdvertisements 'Accepted' status according to
// the error provided
func (c *Controller) updateRAStatus(ra *ratypes.RouteAdvertisements, hadUpdates bool, err error) error {
	if ra == nil {
		return nil
	}

	cstatus := metav1.ConditionTrue
	if err != nil {
		cstatus = metav1.ConditionFalse
	}

	status := "Accepted"
	reason := "Accepted"
	msg := "ovn-kubernetes cluster-manager validated the resource and requested the necessary configuration changes"
	if err != nil {
		status = fmt.Sprintf("Not Accepted: %v", err)
		msg = err.Error()
		switch {
		case errors.Is(err, errConfig):
			reason = "ConfigurationError"
		case errors.Is(err, errPending):
			reason = "ConfigurationPending"
		default:
			reason = "InternalError"
		}
	}

	// MergeStatusCondition reports whether the condition differs from the
	// existing one (in status, reason, message or observed generation),
	// preserves the last transition time when the condition status is
	// unchanged and trims the message to the maximum length allowed for
	// conditions
	condition, changed := util.MergeStatusCondition(ra.Status.Conditions, metav1.Condition{
		Type:               conditionTypeAccepted,
		Status:             cstatus,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: ra.Generation,
	})
	updateStatus := changed || hadUpdates
	if !updateStatus {
		// Record the metric from the existing API-confirmed condition so it is
		// populated after controller restarts, where the informer fires synthetic
		// creates for all RAs but the condition hasn't changed.
		metrics.RecordRouteAdvertisementCondition(ra.Name, conditionTypeAccepted, cstatus)
		return nil
	}

	if changed && err != nil {
		// errConfig/errPending are not retried, so make them visible in the
		// logs and not just in the status; log only when the condition changes
		// to avoid repeating the same warning on every reconcile of a steady
		// error state, which can otherwise still update the status through
		// hadUpdates
		klog.Warningf("Reconciling RouteAdvertisements %q failed: %v", ra.Name, err)
	}

	_, err = c.raClient.K8sV1().RouteAdvertisements().ApplyStatus(
		context.Background(),
		raapply.RouteAdvertisements(ra.Name).WithStatus(
			raapply.RouteAdvertisementsStatus().WithStatus(status).WithConditions(
				util.ConditionToApply(condition),
			),
		),
		metav1.ApplyOptions{
			FieldManager: fieldManager,
		},
	)
	if err != nil {
		return fmt.Errorf("failed to apply status for RouteAdvertisements %q: %w", ra.Name, err)
	}
	metrics.RecordRouteAdvertisementCondition(ra.Name, conditionTypeAccepted, cstatus)

	return nil
}

func (c *Controller) getSelectedNADs(networkSelectors apitypes.NetworkSelectors) ([]*nadtypes.NetworkAttachmentDefinition, error) {
	var selected []*nadtypes.NetworkAttachmentDefinition
	for _, networkSelector := range networkSelectors {
		switch networkSelector.NetworkSelectionType {
		case apitypes.DefaultNetwork:
			// if we are selecting the default networkdefault network label,
			// make sure a NAD exists for it
			nad, err := util.EnsureDefaultNetworkNAD(c.nadLister, c.nadClient)
			if err != nil {
				return nil, fmt.Errorf("failed to ensure default network NAD: %w", err)
			}
			selected = append(selected, nad)
		case apitypes.ClusterUserDefinedNetworks:
			nadSelector, err := metav1.LabelSelectorAsSelector(&networkSelector.ClusterUserDefinedNetworkSelector.NetworkSelector)
			if err != nil {
				return nil, err
			}
			nads, err := c.nadLister.List(nadSelector)
			if err != nil {
				return nil, err
			}
			for _, nad := range nads {
				// check this NAD is controlled by a CUDN
				controller := metav1.GetControllerOfNoCopy(nad)
				isCUDN := controller != nil && controller.Kind == cudnController.Kind && controller.APIVersion == cudnController.GroupVersion().String()
				if !isCUDN {
					continue
				}
				selected = append(selected, nad)
			}
		default:
			return nil, fmt.Errorf("%w: unsupported network selection type %s", errConfig, networkSelector.NetworkSelectionType)
		}
	}

	return selected, nil
}

// getEgressIPsByNodesByNetworks iterates all existing egress IPs that apply to
// any of the provided networks and returns a "node -> network -> eips"
// map.
func (c *Controller) getEgressIPsByNodesByNetworks(networks sets.Set[string]) (map[string]map[string]sets.Set[string], error) {
	eipsByNodesByNetworks := map[string]map[string]sets.Set[string]{}
	addEgressIPsByNodesByNetwork := func(eipsByNodes map[string]string, network string) {
		for node, eip := range eipsByNodes {
			if eipsByNodesByNetworks[node] == nil {
				eipsByNodesByNetworks[node] = map[string]sets.Set[string]{}
			}
			if eipsByNodesByNetworks[node][network] == nil {
				eipsByNodesByNetworks[node][network] = sets.New[string]()
			}
			eipsByNodesByNetworks[node][network].Insert(eip)
		}
	}

	addEgressIPsByNodesByNetworkSelector := func(eipsByNodes map[string]string, namespaceSelector *metav1.LabelSelector) error {
		nsSelector, err := metav1.LabelSelectorAsSelector(namespaceSelector)
		if err != nil {
			return err
		}
		selected, err := c.namespaceLister.List(nsSelector)
		if err != nil {
			return err
		}
		for _, namespace := range selected {
			namespaceNetwork := c.nm.GetActiveNetworkForNamespaceFast(namespace.Name)
			networkName := namespaceNetwork.GetNetworkName()
			if networks.Has(networkName) {
				addEgressIPsByNodesByNetwork(eipsByNodes, networkName)
			}
		}
		return nil
	}

	eips, err := c.eipLister.List(labels.Everything())
	if err != nil {
		return nil, err
	}

	for _, eip := range eips {
		eipsByNodes := make(map[string]string, len(eip.Status.Items))
		for _, item := range eip.Status.Items {
			// skip unassigned EIPs
			if item.EgressIP == "" || item.Node == "" {
				continue
			}

			ip := item.EgressIP + util.GetIPFullMaskString(item.EgressIP)
			eipsByNodes[item.Node] = ip
		}
		if len(eipsByNodes) == 0 {
			continue
		}

		err = addEgressIPsByNodesByNetworkSelector(eipsByNodes, &eip.Spec.NamespaceSelector)
		if err != nil {
			return nil, err
		}
	}

	return eipsByNodesByNetworks, nil
}

// isOwnUpdate checks if an object was updated by us last, as indicated by its
// managed fields. Used to avoid reconciling an update that we made ourselves.
func isOwnUpdate(managedFields []metav1.ManagedFieldsEntry) bool {
	return util.IsLastUpdatedByManager(fieldManager, managedFields)
}

func raNeedsUpdate(oldObj, newObj *ratypes.RouteAdvertisements) bool {
	return oldObj == nil || newObj == nil || oldObj.Generation != newObj.Generation
}

func frrConfigurationNeedsUpdate(oldObj, newObj *frrtypes.FRRConfiguration) bool {
	// ignore if it was created or updated by ourselves
	if newObj != nil && isOwnUpdate(newObj.ManagedFields) {
		return false
	}
	return oldObj == nil || newObj == nil || oldObj.Generation != newObj.Generation ||
		!reflect.DeepEqual(oldObj.Labels, newObj.Labels) ||
		oldObj.Annotations[types.OvnRouteAdvertisementsKey] != newObj.Annotations[types.OvnRouteAdvertisementsKey]
}

func nadNeedsUpdate(oldObj, newObj *nadtypes.NetworkAttachmentDefinition) bool {
	// ignore if it updated by ourselves
	if newObj != nil && isOwnUpdate(newObj.ManagedFields) {
		return false
	}
	nadSupported := func(nad *nadtypes.NetworkAttachmentDefinition) bool {
		if nad == nil {
			return false
		}
		network, err := util.ParseNADInfo(newObj)
		if err != nil {
			return true
		}
		return network.IsDefault() ||
			(network.IsPrimaryNetwork() && (network.TopologyType() == types.Layer3Topology || network.TopologyType() == types.Layer2Topology))
	}
	// ignore if we don't support this NAD
	if !nadSupported(oldObj) && !nadSupported(newObj) {
		return false
	}

	return oldObj == nil || newObj == nil ||
		!reflect.DeepEqual(oldObj.Labels, newObj.Labels) ||
		oldObj.Annotations[types.OvnRouteAdvertisementsKey] != newObj.Annotations[types.OvnRouteAdvertisementsKey]
}

func nodeNeedsUpdate(oldObj, newObj *corev1.Node) bool {
	return oldObj == nil || newObj == nil ||
		!reflect.DeepEqual(oldObj.Labels, newObj.Labels) ||
		util.NodeSubnetAnnotationChanged(oldObj, newObj) ||
		// With dynamic UDN allocation in legacy layer2 topology, the tunnel
		// ID allocation determines which nodes advertise the network.
		oldObj.Annotations[types.UDNLayer2NodeGRLRPTunnelIDAnnotation] != newObj.Annotations[types.UDNLayer2NodeGRLRPTunnelIDAnnotation] ||
		oldObj.Annotations[util.OvnNodeIfAddr] != newObj.Annotations[util.OvnNodeIfAddr] ||
		util.NodePrimaryDPUHostAddrAnnotationChanged(oldObj, newObj) ||
		util.NodeL3GatewayAnnotationChanged(oldObj, newObj) ||
		util.NodeChassisIDAnnotationChanged(oldObj, newObj) ||
		util.NodeVTEPsAnnotationChanged(oldObj, newObj)
}

func uplinkStateNeedsUpdate(oldObj, newObj *uplinkv1alpha1.UplinkState) bool {
	return oldObj == nil || newObj == nil || !reflect.DeepEqual(oldObj.Status, newObj.Status)
}

func egressIPNeedsUpdate(oldObj, newObj *eiptypes.EgressIP) bool {
	if oldObj != nil && newObj != nil {
		return !reflect.DeepEqual(oldObj.Status, newObj.Status) || !reflect.DeepEqual(oldObj.Spec.NamespaceSelector, newObj.Spec.NamespaceSelector)
	}
	if oldObj != nil && len(oldObj.Status.Items) > 0 {
		return true
	}
	if newObj != nil && len(newObj.Status.Items) > 0 {
		return true
	}
	return false
}

func nsNeedsUpdate(oldObj, newObj *corev1.Namespace) bool {
	// we only care about label changes, added/deleted namespaces served by a
	// UDN will already be reflected in a network update
	return oldObj != nil && newObj != nil && !reflect.DeepEqual(oldObj.Labels, newObj.Labels)
}

func (c *Controller) reconcileFRRConfiguration(key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		klog.Errorf("Failed spliting FRFConfiguration reconcile key %q: %v", key, err)
		return nil
	}

	frrConfig, err := c.frrLister.FRRConfigurations(namespace).Get(name)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}

	// safest approach is to reconcile all existing RouteAdvertisements (could
	// be potentially avoided with additional caching but let's hold it until we
	// know we need it)
	c.raController.ReconcileAll()

	// on startup, we might be syncing a FRRConfiguration generated by us for a
	// RouteAdvertisements that does not exist, so make sure to reconcile it so
	// that the FRRConfiguration is deleted if needed
	if frrConfig != nil && frrConfig.Labels[types.OvnRouteAdvertisementsKey] != "" {
		c.raController.Reconcile(frrConfig.Labels[types.OvnRouteAdvertisementsKey])
	}

	return nil
}

func (c *Controller) reconcileNAD(key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		klog.Errorf("Failed spliting NAD reconcile key %q: %v", key, err)
		return nil
	}

	nad, err := c.nadLister.NetworkAttachmentDefinitions(namespace).Get(name)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}

	// safest approach is to reconcile all existing RouteAdvertisements (could be potentially
	// avoided with additional caching but let's hold it until we know we need
	// it)
	c.raController.ReconcileAll()

	// on startup, we might be syncing a NAD annotated by us with a
	// RouteAdvertisements that does not longer exist, so make sure to reconcile
	// annotated RouteAdvertisements so that the annotation is updated
	// accordingly
	if nad != nil && nad.Annotations[types.OvnRouteAdvertisementsKey] != "" {
		var ras []string
		err := json.Unmarshal([]byte(nad.Annotations[types.OvnRouteAdvertisementsKey]), &ras)
		if err != nil {
			return err
		}
		for _, ra := range ras {
			c.raController.Reconcile(ra)
		}
	}

	return nil
}

// neighborKey returns the identifier of a neighbor as used in raw FRR config:
// the session address or, for unnumbered neighbors, the interface name.
func neighborKey(neighbor frrtypes.Neighbor) string {
	if neighbor.Address != "" {
		return neighbor.Address
	}
	return neighbor.Interface
}

// isUnnumberedNeighbor returns whether the neighbor is unnumbered, that is
// defined by interface rather than by address. Coherent with neighborKey, a
// neighbor that specifies both is considered defined by address.
func isUnnumberedNeighbor(neighbor frrtypes.Neighbor) bool {
	return neighbor.Address == "" && neighbor.Interface != ""
}

func (c *Controller) reconcileEgressIPs(string) error {
	// reconcile RAs that advertise EIPs
	ras, err := c.raLister.List(labels.Everything())
	if err != nil {
		return err
	}

	for _, ra := range ras {
		if sets.New(ra.Spec.Advertisements...).Has(ratypes.EgressIP) {
			c.raController.Reconcile(ra.Name)
		}
	}

	return nil
}
