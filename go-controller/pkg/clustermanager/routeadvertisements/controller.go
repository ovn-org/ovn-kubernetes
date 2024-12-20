package routeadvertisements

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	nadtypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	nadclientset "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned"
	nadlisters "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/listers/k8s.cni.cncf.io/v1"
	frrtypes "github.com/metallb/frr-k8s/api/v1beta1"
	frrclientset "github.com/metallb/frr-k8s/pkg/client/clientset/versioned"
	frrlisters "github.com/metallb/frr-k8s/pkg/client/listers/api/v1beta1"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	metaapply "k8s.io/client-go/applyconfigurations/meta/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	controllerutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/controller"
	eiptypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	egressiplisters "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/listers/egressip/v1"
	ratypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1"
	raapply "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1/apis/applyconfiguration/routeadvertisements/v1"
	raclientset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1/apis/clientset/versioned"
	ralisters "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1/apis/listers/routeadvertisements/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

const (
	generateName = "ovnk-generated-"
	fieldManager = "clustermanager-routeadvertisements-controller"
)

var (
	errConfig  = errors.New("configuration error")
	errPending = errors.New("configuration pending")
)

// Controller reconciles RouteAdvertisements
type Controller struct {
	eipLister  egressiplisters.EgressIPLister
	frrLister  frrlisters.FRRConfigurationLister
	nadLister  nadlisters.NetworkAttachmentDefinitionLister
	nodeLister corelisters.NodeLister
	raLister   ralisters.RouteAdvertisementsLister

	frrClient frrclientset.Interface
	nadClient nadclientset.Interface
	raClient  raclientset.Interface

	eipController  controllerutil.Controller
	frrController  controllerutil.Controller
	nadController  controllerutil.Controller
	nodeController controllerutil.Controller
	raController   controllerutil.Controller

	nm networkmanager.Interface
}

// NewController builds a controller that reconciles RouteAdvertisements
func NewController(
	nm networkmanager.Interface,
	wf *factory.WatchFactory,
	ovnClient *util.OVNClusterManagerClientset,
) *Controller {
	c := &Controller{
		eipLister:  wf.EgressIPInformer().Lister(),
		frrLister:  wf.FRRConfigurationsInformer().Lister(),
		nadLister:  wf.NADInformer().Lister(),
		nodeLister: wf.NodeCoreInformer().Lister(),
		raLister:   wf.RouteAdvertisementsInformer().Lister(),
		frrClient:  ovnClient.FRRClient,
		nadClient:  ovnClient.NetworkAttchDefClient,
		raClient:   ovnClient.RouteAdvertisementsClient,
		nm:         nm,
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

		return c.updateRAStatus(ra, false, err)
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

	nodeConfig := &controllerutil.ControllerConfig[core.Node]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:      func(key string) error { c.raController.ReconcileAll(); return nil },
		Threadiness:    1,
		Informer:       wf.NodeCoreInformer().Informer(),
		Lister:         wf.NodeCoreInformer().Lister().List,
		ObjNeedsUpdate: nodeNeedsUpdate,
	}
	c.nodeController = controllerutil.NewController("clustermanager routeadvertisements node controller", nodeConfig)

	eipConfig := &controllerutil.ControllerConfig[eiptypes.EgressIP]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:      c.reconcileEgressIP,
		Threadiness:    1,
		Informer:       wf.EgressIPInformer().Informer(),
		Lister:         wf.EgressIPInformer().Lister().List,
		ObjNeedsUpdate: egressIPNeedsUpdate,
	}
	c.eipController = controllerutil.NewController("clustermanager routeadvertisements egressip controller", eipConfig)

	return c
}

func (c *Controller) Start() error {
	defer klog.Infof("Cluster manager routeadvertisements started")
	return controllerutil.Start(
		c.eipController,
		c.frrController,
		c.nadController,
		c.nodeController,
		c.raController,
	)
}

func (c *Controller) Stop() {
	controllerutil.Stop(
		c.eipController,
		c.frrController,
		c.nadController,
		c.nodeController,
		c.raController,
	)
	klog.Infof("Cluster manager routeadvertisements stoppedu")
}

func (c *Controller) ReconcileNetwork(name string, old, new util.NetInfo) {
	// This controller already listens on NAD events however we skip NADs
	// pointing to networks that network manager is still not aware of; so we
	// only need to signal the reconciliation of new networks. Reconcile one of
	// the NADs of the network to do s
	if new == nil || old != nil {
		return
	}
	c.nadController.Reconcile(new.GetNADs()[0])
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
// VRFs.
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
// FRRConfigurations, Nodes, EgressIPs and NADs.
func (c *Controller) reconcile(name string) error {
	startTime := time.Now()
	klog.V(5).Infof("Syncing routeadvertisements %q", name)
	defer func() {
		klog.V(4).Infof("Finished syncing routeadvertisements %q, took %v", name, time.Since(startTime))
	}()

	ra, err := c.raLister.Get(name)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to get RouteAdvertisements %q: %w", name, err)
	}

	hadUpdates, err := c.reconcileRouteAdvertisements(name, ra)
	if err != nil && !errors.Is(err, errConfig) && !errors.Is(err, errPending) {
		return fmt.Errorf("failed to reconcile RouteAdvertisements %q: %w", name, err)
	}

	return c.updateRAStatus(ra, hadUpdates, err)
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
	// hostNetworkSubnets is a map of selected network names to their ordered network subnets specific for a node
	hostNetworkSubnets map[string][]string
	// prefixLength is a map of selected network to their prefix length
	prefixLength map[string]uint32
}

// generateFRRConfigurations generates FRRConfigurations for the route
// advertisements. Also returns the selected network NADs.
func (c *Controller) generateFRRConfigurations(ra *ratypes.RouteAdvertisements) ([]*frrtypes.FRRConfiguration, []*nadtypes.NetworkAttachmentDefinition, error) {
	if ra == nil {
		return nil, nil, nil
	}

	// if we are matching on the well known default network label, create an
	// internal nad for it if it doesn't exist
	if matchesDefaultNetworkLabel(ra.Spec.NetworkSelector) {
		_, err := c.getOrCreateDefaultNetworkNAD()
		if err != nil {
			return nil, nil, fmt.Errorf("failed to get/create default network NAD: %w", err)
		}
	}

	// gather selected networks
	var nads []*nadtypes.NetworkAttachmentDefinition
	nadSelector, err := metav1.LabelSelectorAsSelector(&ra.Spec.NetworkSelector)
	if err != nil {
		return nil, nil, err
	}
	nads, err = c.nadLister.List(nadSelector)
	if err != nil {
		return nil, nil, err
	}
	if len(nads) == 0 {
		return nil, nil, fmt.Errorf("%w: no networks selected", errPending)
	}

	// validate and gather information about the networks
	networkSet := sets.New[string]()
	selectedNetworks := &selectedNetworks{
		networkVRFs:    map[string]string{},
		networkSubnets: map[string][]string{},
		prefixLength:   map[string]uint32{},
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
		if network.TopologyType() != types.Layer3Topology {
			// TODO don't really know what to do with layer 2 topologies yet
			return nil, nil, fmt.Errorf("%w: selected network %q has unsupported topology %q", errConfig, networkName, network.TopologyType())
		}
		vrf := util.GetNetworkVRFName(network)
		if vfrNet, hasVFR := selectedNetworks.networkVRFs[vrf]; hasVFR && vfrNet != networkName {
			return nil, nil, fmt.Errorf("%w: vrf %q found to be mapped to multiple networks %v", errConfig, vrf, []string{vfrNet, networkName})
		}
		networkSet.Insert(networkName)
		selectedNetworks.vrfs = append(selectedNetworks.vrfs, vrf)
		selectedNetworks.networkVRFs[vrf] = networkName
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
	selectedNetworks.networks = sets.List(networkSet)

	// gather selected nodes
	nodeSelector, err := metav1.LabelSelectorAsSelector(&ra.Spec.NodeSelector)
	if err != nil {
		return nil, nil, err
	}
	advertisements := sets.New(ra.Spec.Advertisements...)
	if !nodeSelector.Empty() && advertisements.Has(ratypes.PodNetwork) {
		return nil, nil, fmt.Errorf("%w: node selector cannot be specified if pod network is advertised", errConfig)
	}
	nodes, err := c.nodeLister.List(nodeSelector)
	if err != nil {
		return nil, nil, err
	}
	if len(nodes) == 0 {
		return nil, nil, fmt.Errorf("%w: no nodes selected", errPending)
	}
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
	}

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
	var nodeEgressIPs map[string][]string
	getEgressIPs := func(nodeName string) ([]string, error) {
		if nodeEgressIPs == nil {
			nodeEgressIPs, err = c.getEgressIPsByNode()
			if err != nil {
				return nil, err
			}
		}
		return nodeEgressIPs[nodeName], nil
	}

	// helper to gather host subnets and egress ips as prefixes
	getPrefixes := func(nodeName string, network string) ([]string, error) {
		// gather host subnets
		var subnets []string
		if advertisements.Has(ratypes.PodNetwork) {
			subnets, err = getHostSubnets(nodeName, network)
			if err != nil || len(subnets) == 0 {
				return nil, fmt.Errorf("%w: will wait for subnet annotation to be set for node %q and network %q: %w", errConfig, nodeName, network, err)
			}

		}
		// gather EgressIPs
		var eips []string
		if advertisements.Has(ratypes.EgressIP) {
			if network != types.DefaultNetworkName {
				return nil, fmt.Errorf("%w: can't advertise EgressIP in selected non default network %q: %w", errConfig, network, err)
			}
			eips, err = getEgressIPs(nodeName)
			if err != nil {
				return nil, err
			}
		}

		prefixes := make([]string, 0, len(subnets)+len(eips))
		prefixes = append(prefixes, subnets...)
		prefixes = append(prefixes, eips...)
		return prefixes, nil
	}

	generated := []*frrtypes.FRRConfiguration{}
	for nodeName, frrConfigs := range nodeToFRRConfig {
		// reset node specific information
		selectedNetworks.hostNetworkSubnets = map[string][]string{}
		selectedNetworks.hostSubnets = []string{}

		// gather node specific information
		for _, network := range selectedNetworks.networks {
			selectedNetworks.hostNetworkSubnets[network], err = getPrefixes(nodeName, network)
			if err != nil {
				return nil, nil, err
			}
			selectedNetworks.hostSubnets = append(selectedNetworks.hostSubnets, selectedNetworks.hostNetworkSubnets[network]...)
			// ordered
			slices.Sort(selectedNetworks.hostNetworkSubnets[network])
		}
		// ordered
		slices.Sort(selectedNetworks.hostSubnets)

		matchedNetworks := sets.New[string]()
		for _, frrConfig := range frrConfigs {
			// generate FRRConfiguration for each source FRRConfiguration/node combination
			new := c.generateFRRConfiguration(
				ra,
				frrConfig,
				nodeName,
				selectedNetworks,
				matchedNetworks,
			)
			if new == nil {
				// if we got nil, we didn't match any VRF
				return nil, nil, fmt.Errorf("%w: FRRConfiguration %q selected for node %q has no VRF matching the RouteAdvertisements target VRF or any selected network",
					errConfig, frrConfig.Name, nodeName)
			}
			generated = append(generated, new)
		}
		// check that we matched all the selected networks on 'auto'
		if ra.Spec.TargetVRF == "auto" && !matchedNetworks.HasAll(selectedNetworks.networks...) {
			return nil, nil, fmt.Errorf("%w: selected FRRConfigurations for node %q don't match all selected networks with target VRF 'auto'", errConfig, nodeName)
		}
	}

	return generated, nads, nil
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
) *frrtypes.FRRConfiguration {
	routers := []frrtypes.Router{}
	advertisements := sets.New(ra.Spec.Advertisements...)

	// go over the source routers
	for i, router := range source.Spec.BGP.Routers {

		targetVRF := ra.Spec.TargetVRF
		var matchedVRF, matchedNetwork string
		var receivePrefixes, advertisePrefixes []string

		// We will use the router if:
		// - the router VRF matches the target VRF
		// - if the target VRF is 'auto', the router VRF is that of a selected network
		// Prepare each scenario with a switch statement and check after that
		switch {
		case targetVRF == "auto" && router.VRF == "":
			// match on default network/VRF, advertise node prefixes and receive
			// any prefix of default network.
			matchedVRF = ""
			matchedNetwork = types.DefaultNetworkName
			advertisePrefixes = selectedNetworks.hostNetworkSubnets[matchedNetwork]
			receivePrefixes = selectedNetworks.networkSubnets[matchedNetwork]
		case targetVRF == "auto":
			// match router.VRF to network.VRF, advertise node prefixes and
			// receive any prefix of the matched network
			matchedVRF = router.VRF
			matchedNetwork = selectedNetworks.networkVRFs[matchedVRF]
			advertisePrefixes = selectedNetworks.hostNetworkSubnets[matchedNetwork]
			receivePrefixes = selectedNetworks.networkSubnets[matchedNetwork]
		case targetVRF == "":
			// match on default network/VRF, advertise node prefixes and
			// receive any prefix of selected networks
			matchedVRF = ""
			matchedNetwork = types.DefaultNetworkName
			advertisePrefixes = selectedNetworks.hostSubnets
			receivePrefixes = selectedNetworks.subnets
		default:
			// match router.VRF to network.VRF, advertise node prefixes and
			// receive any prefix of selected networks
			matchedVRF = targetVRF
			matchedNetwork = selectedNetworks.networkVRFs[matchedVRF]
			advertisePrefixes = selectedNetworks.hostSubnets
			receivePrefixes = selectedNetworks.subnets
		}
		if matchedVRF != router.VRF || len(advertisePrefixes) == 0 {
			// either this router VRF does not match the target VRF or we don't
			// have prefixes for it (which might be due to this RA not selecting
			// this network, but not just)
			continue
		}
		matchedNetworks.Insert(matchedNetwork)

		// if this router's VRF matches the target VRF, copy it and set the
		// prefixes as appropriate
		targetRouter := router
		targetRouter.Prefixes = advertisePrefixes
		targetRouter.Neighbors = make([]frrtypes.Neighbor, 0, len(source.Spec.BGP.Routers[i].Neighbors))
		for _, neighbor := range source.Spec.BGP.Routers[i].Neighbors {
			advertisePrefixes := advertisePrefixes
			receivePrefixes := receivePrefixes
			if neighbor.DisableMP {
				isIPV6 := utilnet.IsIPv6String(neighbor.Address)
				advertisePrefixes = util.MatchAllIPNetsStringFamily(isIPV6, advertisePrefixes)
				receivePrefixes = util.MatchAllIPNetsStringFamily(isIPV6, receivePrefixes)
			}
			if len(advertisePrefixes) == 0 {
				continue
			}
			neighbor.ToAdvertise = frrtypes.Advertise{
				Allowed: frrtypes.AllowedOutPrefixes{
					Mode:     frrtypes.AllowRestricted,
					Prefixes: advertisePrefixes,
				},
			}
			neighbor.ToReceive = frrtypes.Receive{
				Allowed: frrtypes.AllowedInPrefixes{
					Mode: frrtypes.AllowRestricted,
				},
			}
			if advertisements.Has(ratypes.PodNetwork) {
				for _, prefix := range receivePrefixes {
					neighbor.ToReceive.Allowed.Prefixes = append(neighbor.ToReceive.Allowed.Prefixes,
						frrtypes.PrefixSelector{
							Prefix: prefix,
							LE:     selectedNetworks.prefixLength[prefix],
							GE:     selectedNetworks.prefixLength[prefix],
						},
					)
				}
			}
			targetRouter.Neighbors = append(targetRouter.Neighbors, neighbor)
		}
		if len(targetRouter.Neighbors) == 0 {
			// we ended up with no neighbor
			continue
		}

		// append this router to the list of routers we will include in the
		// generated FRR config and track its index as we might need to add
		// imports to it
		routers = append(routers, targetRouter)
		targetRouterIndex := len(routers) - 1

		// VRFs are isolated in "auto" so no need to handle imports
		if targetVRF == "auto" {
			continue
		}

		// we won't do imports if the pod network is not advertised
		if !advertisements.Has(ratypes.PodNetwork) {
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
	if len(routers) == 0 {
		// we ended up with no routers, bail out
		return nil
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

	return new
}

// updateFRRConfigurations updates the FRRConfigurations that apply for a
// RouteAdvertisements. It fetches existing FRRConfigurations by label and
// indexes them by the annotated key. Then compares this state with desired
// state and creates, updates or deletes the FRRConfigurations accordingly.
func (c *Controller) updateFRRConfigurations(ra string, frrConfigurations []*frrtypes.FRRConfiguration) (bool, error) {
	var hadUpdates bool

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

	condition := meta.FindStatusCondition(ra.Status.Conditions, "Accepted")
	updateStatus := hadUpdates || condition == nil || condition.ObservedGeneration != ra.Generation
	updateStatus = updateStatus || err != nil

	if !updateStatus {
		return nil
	}

	status := "Accepted"
	cstatus := metav1.ConditionTrue
	reason := "Accepted"
	msg := "ovn-kubernetes cluster-manager validated the resource and requested the necessary configuration changes"
	if err != nil {
		status = fmt.Sprintf("Not Accepted: %v", err)
		cstatus = metav1.ConditionFalse
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

	_, err = c.raClient.K8sV1().RouteAdvertisements().ApplyStatus(
		context.Background(),
		raapply.RouteAdvertisements(ra.Name).WithStatus(
			raapply.RouteAdvertisementsStatus().WithStatus(status).WithConditions(
				metaapply.Condition().
					WithType("Accepted").
					WithStatus(cstatus).
					WithLastTransitionTime(metav1.NewTime(time.Now())).
					WithReason(reason).
					WithMessage(msg).
					WithObservedGeneration(ra.Generation),
			),
		),
		metav1.ApplyOptions{
			FieldManager: fieldManager,
		},
	)
	if err != nil {
		return fmt.Errorf("failed to apply status for RouteAdvertisements %q: %w", ra.Name, err)
	}

	return nil
}

// getOrCreateDefaultNetworkNAD ensure that a well-known NAD exists for the
// default network in ovn-k namespace.
func (c *Controller) getOrCreateDefaultNetworkNAD() (*nadtypes.NetworkAttachmentDefinition, error) {
	nad, err := c.nadLister.NetworkAttachmentDefinitions(config.Kubernetes.OVNConfigNamespace).Get(types.DefaultNetworkName)
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, err
	}
	if nad != nil {
		return nad, nil
	}
	return c.nadClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(config.Kubernetes.OVNConfigNamespace).Create(
		context.Background(),
		&nadtypes.NetworkAttachmentDefinition{
			ObjectMeta: metav1.ObjectMeta{
				Name:      types.DefaultNetworkName,
				Namespace: config.Kubernetes.OVNConfigNamespace,
				Labels:    map[string]string{types.DefaultNetworkLabelSelector: ""},
			},
			Spec: nadtypes.NetworkAttachmentDefinitionSpec{
				Config: fmt.Sprintf("{\"cniVersion\": \"0.4.0\", \"name\": \"ovn-kubernetes\", \"type\": \"%s\"}", config.CNI.Plugin),
			},
		},
		// note we don't set ourselves as field manager for this create as we
		// want to process the resulting event that would otherwise be filtered
		// out in nadNeedsUpdate
		metav1.CreateOptions{},
	)
}

// getEgressIPsByNode iterates all existing egress IPs and returns them indexed
// by node
func (c *Controller) getEgressIPsByNode() (map[string][]string, error) {
	eips, err := c.eipLister.List(labels.Everything())
	if err != nil {
		return nil, err
	}

	eipsByNode := map[string][]string{}
	for _, eip := range eips {
		for _, item := range eip.Status.Items {
			if item.EgressIP == "" {
				continue
			}
			ip := item.EgressIP + util.GetIPFullMaskString(item.EgressIP)
			eipsByNode[item.Node] = append(eipsByNode[item.Node], ip)
		}
	}

	return eipsByNode, nil
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
		return network.IsDefault() || (network.IsPrimaryNetwork() && network.TopologyType() == types.Layer3Topology)
	}
	// ignore if we don't support this NAD
	if !nadSupported(oldObj) && !nadSupported(newObj) {
		return false
	}

	return oldObj == nil || newObj == nil ||
		!reflect.DeepEqual(oldObj.Labels, newObj.Labels) ||
		oldObj.Annotations[types.OvnRouteAdvertisementsKey] != newObj.Annotations[types.OvnRouteAdvertisementsKey]
}

func nodeNeedsUpdate(oldObj, newObj *core.Node) bool {
	return oldObj == nil || newObj == nil ||
		!reflect.DeepEqual(oldObj.Labels, newObj.Labels) ||
		util.NodeSubnetAnnotationChanged(oldObj, newObj)
}

func egressIPNeedsUpdate(oldObj, newObj *eiptypes.EgressIP) bool {
	if oldObj != nil && newObj != nil && reflect.DeepEqual(oldObj.Status, newObj.Status) {
		return false
	}
	if oldObj != nil && len(oldObj.Status.Items) > 0 {
		return true
	}
	if newObj != nil && len(newObj.Status.Items) > 0 {
		return true
	}
	return false
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

func (c *Controller) reconcileEgressIP(eipName string) error {
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

func matchesDefaultNetworkLabel(selector metav1.LabelSelector) bool {
	_, matchesLabel := selector.MatchLabels[types.DefaultNetworkLabelSelector]
	if matchesLabel {
		return true
	}
	for _, expr := range selector.MatchExpressions {
		if expr.Key == types.DefaultNetworkLabelSelector {
			return true
		}
	}
	return false
}
