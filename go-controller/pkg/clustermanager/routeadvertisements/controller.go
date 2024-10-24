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
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	metaapply "k8s.io/client-go/applyconfigurations/meta/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

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
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

const (
	generateName = "ovnk-generated-"
	fieldManager = "clustermanager-routeadvertisements-controller"
)

var (
	errConfig = errors.New("configuration error")
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
}

// NewController builds a controller that reconciles RouteAdvertisements
func NewController(wf *factory.WatchFactory, ovnClient *util.OVNClusterManagerClientset) *Controller {
	c := &Controller{
		eipLister:  wf.EgressIPInformer().Lister(),
		frrLister:  wf.FRRConfigurationsInformer().Lister(),
		nadLister:  wf.NADInformer().Lister(),
		nodeLister: wf.NodeCoreInformer().Lister(),
		raLister:   wf.RouteAdvertisementsInformer().Lister(),
		frrClient:  ovnClient.FRRClient,
		nadClient:  ovnClient.NetworkAttchDefClient,
		raClient:   ovnClient.RouteAdvertisementsClient,
	}

	raConfig := &controllerutil.ControllerConfig[ratypes.RouteAdvertisements]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:      c.reconcile,
		Threadiness:    1,
		Informer:       wf.RouteAdvertisementsInformer().Informer(),
		Lister:         wf.RouteAdvertisementsInformer().Lister().List,
		ObjNeedsUpdate: raNeedsUpdate,
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
func (c *Controller) reconcile(raName string) error {
	startTime := time.Now()
	klog.V(5).Infof("Syncing routeadvertisements %q", raName)
	defer func() {
		klog.V(4).Infof("Finished syncing routeadvertisements %q, took %v", raName, time.Since(startTime))
	}()

	ra, err := c.raLister.Get(raName)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to get RouteAdvertisements %q: %w", raName, err)
	}

	// generate FRRConfigurations
	frrConfigs, nads, err := c.generateFRRConfigurations(ra)
	if errors.Is(err, errConfig) {
		klog.Warningf("Failed generating FRRConfigurations for RouteAdvertisements %q: %v", raName, err)
		err = c.updateRAStatus(ra, err)
		if err != nil {
			return fmt.Errorf("failed to update RouteAdvertisements status: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed generating FRRConfigurations for RouteAdvertisements %q: %w", raName, err)
	}

	// update them
	hadFRRConfigUpdates, err := c.updateFRRConfigurations(raName, frrConfigs)
	if err != nil {
		return fmt.Errorf("failed updating FRRConfigurations for RouteAdvertisements %q: %w", raName, err)
	}

	// annotate NADs
	hadNADUpdates, err := c.updateNADs(raName, nads)
	if err != nil {
		return fmt.Errorf("failed annotating NADs for RouteAdvertisements %q: %w", raName, err)
	}

	hadUpdates := hadFRRConfigUpdates || hadNADUpdates

	// update status
	if hadUpdates {
		err = c.updateRAStatus(ra, nil)
		if err != nil {
			return fmt.Errorf("failed to update RouteAdvertisements status: %w", err)
		}
	}

	return nil
}

// generateFRRConfigurations generates FRRConfigurations for the route
// advertisements. Also returns the selected network NADs.
func (c *Controller) generateFRRConfigurations(ra *ratypes.RouteAdvertisements) ([]*frrtypes.FRRConfiguration, []*nadtypes.NetworkAttachmentDefinition, error) {
	if ra == nil {
		return nil, nil, nil
	}

	// gather selected networks
	networkSet := sets.New[string]()
	var nads []*nadtypes.NetworkAttachmentDefinition
	switch {
	case ra.Spec.NetworkSelector == nil:
		// nil selector selects the default network for wich we create an
		// internal NAD
		nad, err := c.getOrCreateDefaultNetworkNAD()
		if err != nil {
			return nil, nil, fmt.Errorf("failed to get/create default network NAD: %w", err)
		}
		nads = []*nadtypes.NetworkAttachmentDefinition{nad}

	case ra.Spec.NetworkSelector != nil:
		nadSelector, err := meta.LabelSelectorAsSelector(ra.Spec.NetworkSelector)
		if err != nil {
			return nil, nil, err
		}
		nads, err = c.nadLister.List(nadSelector)
		if err != nil {
			return nil, nil, err
		}
	}

	for _, nad := range nads {
		netInfo, err := util.ParseNADInfo(nad)
		if err != nil {
			return nil, nil, err
		}
		if !netInfo.IsDefault() && !netInfo.IsPrimaryNetwork() {
			return nil, nil, fmt.Errorf("%w: selected network %q is not the default nor a primary network", errConfig, netInfo.GetNetworkName())
		}
		if netInfo.TopologyType() != types.Layer3Topology {
			// TODO don't really know what to do with layer 2 topologies yet
			return nil, nil, fmt.Errorf("%w: selected network %q has unsupported topology %q", errConfig, netInfo.GetNetworkName(), netInfo.TopologyType())
		}
		networkSet.Insert(netInfo.GetNetworkName())
	}
	networks := sets.List(networkSet) // ordered

	// gather selected nodes
	nodeSelector, err := meta.LabelSelectorAsSelector(&ra.Spec.NodeSelector)
	if err != nil {
		return nil, nil, err
	}
	nodes, err := c.nodeLister.List(nodeSelector)
	if err != nil {
		return nil, nil, err
	}
	nodeToFrrConfig := map[string][]*frrtypes.FRRConfiguration{}
	for _, node := range nodes {
		nodeToFrrConfig[node.Name] = nil
	}

	// gather selected FRRConfigurations, map them to nodes
	frrSelector, err := meta.LabelSelectorAsSelector(&ra.Spec.FrrConfigurationSelector)
	if err != nil {
		return nil, nil, err
	}
	frrConfigs, err := c.frrLister.List(frrSelector)
	if err != nil {
		return nil, nil, err
	}
	for _, frrConfig := range frrConfigs {
		if strings.HasPrefix(frrConfig.Name, generateName) {
			klog.V(4).Infof("Skipping FRRConfiguration %q selected by RouteAdvertisements %q as it was generated by ovn-kubernetes", frrConfig.Name, ra.Name)
			continue
		}
		nodeSelector, err := meta.LabelSelectorAsSelector(&frrConfig.Spec.NodeSelector)
		if err != nil {
			return nil, nil, err
		}
		nodes, err := c.nodeLister.List(nodeSelector)
		if err != nil {
			return nil, nil, err
		}
		for _, node := range nodes {
			if _, selected := nodeToFrrConfig[node.Name]; !selected {
				// this RouteAdvertisements does not select this node, skip
				continue
			}
			nodeToFrrConfig[node.Name] = append(nodeToFrrConfig[node.Name], frrConfig)
		}
	}

	// helper to gather host subnets
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

	// helper to gather egress ips
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
		if ra.Spec.Advertisements.PodNetwork {
			subnets, err = getHostSubnets(nodeName, network)
			if err != nil || len(subnets) == 0 {
				return nil, fmt.Errorf("%w: will wait for subnet annotation to be set for node %q and network %q: %w", errConfig, nodeName, network, err)
			}

		}
		// gather EgressIPs
		var eips []string
		if ra.Spec.Advertisements.EgressIP {
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
	for nodeName, frrConfigs := range nodeToFrrConfig {
		if frrConfigs == nil {
			return nil, nil, fmt.Errorf("%w: no FRRConfiguration selected for node %q", errConfig, nodeName)
		}
		prefixes := make(map[string][]string, len(networks))
		allPrefixes := make([]string, 0, len(networks))
		for _, network := range networks {
			prefixes[network], err = getPrefixes(nodeName, network)
			if err != nil {
				return nil, nil, err
			}
			// sort to have a predictable order that we can match when we
			// compare
			slices.Sort(prefixes[network])
			allPrefixes = append(allPrefixes, prefixes[network]...)
		}
		slices.Sort(allPrefixes)
		matchedNetworks := sets.New[string]()
		for _, frrConfig := range frrConfigs {
			// generate FRRConfiguration for each source FRRConfiguration/node combination
			new := c.generateFRRConfiguration(ra, frrConfig, nodeName, networks, prefixes, allPrefixes, matchedNetworks)
			if new == nil {
				// if we got nil, we didn't match any VRF
				return nil, nil, fmt.Errorf("%w: FRRConfiguration %q selected for node %q has no matching VRF %q", errConfig, frrConfig.Name, nodeName, ra.Spec.TargetVRF)
			}
			generated = append(generated, new)
		}
		// check that we matched all the selected networks on 'auto'
		if ra.Spec.TargetVRF == "auto" && !matchedNetworks.HasAll(networks...) {
			return nil, nil, fmt.Errorf("%w: selected FRRConfigurations for node %q don't match all selected networks with target VRF 'auto'", errConfig, nodeName)
		}
	}

	return generated, nads, nil
}

// generateFRRConfiguration generates a FRRConfiguration from a source for a
// specific node. Also fills matchedVRFs with the matched VRFs, or if targetVRF
// is 'auto', with the matched networks.
func (c *Controller) generateFRRConfiguration(
	ra *ratypes.RouteAdvertisements,
	source *frrtypes.FRRConfiguration,
	nodeName string,
	networks []string,
	networkPrefixes map[string][]string,
	allPrefixes []string,
	matchedVRFs sets.Set[string],
) *frrtypes.FRRConfiguration {
	routers := []frrtypes.Router{}
	for i, router := range source.Spec.BGP.Routers {
		// match the router VRF with the RA VRF and select the appropriate
		// prefixes accordingly
		targetNetwork := ra.Spec.TargetVRF
		if targetNetwork == "" {
			targetNetwork = "default"
		}
		var targetVRF, matchedVRF string
		var prefixes []string
		switch {
		case targetNetwork == "auto" && router.VRF == "":
			targetVRF = router.VRF
			// prefixes for default network
			matchedVRF = types.DefaultNetworkName
			prefixes = networkPrefixes[matchedVRF]
		case targetNetwork == "auto":
			targetVRF = router.VRF
			// prefixes for this VRF network
			matchedVRF = router.VRF
			prefixes = networkPrefixes[matchedVRF]
		case targetNetwork == "default":
			targetVRF = ""
			matchedVRF = types.DefaultNetworkName
			prefixes = allPrefixes
		default:
			targetVRF = ra.Spec.TargetVRF
			matchedVRF = ra.Spec.TargetVRF
			prefixes = allPrefixes
		}
		if targetVRF != router.VRF || len(prefixes) == 0 {
			// either we are not targeting this VRF or we don't have prefixes
			// for it (which might be due to this RA not selecting this network,
			// but not just)
			continue
		}
		matchedVRFs.Insert(matchedVRF)

		targetRouter := router
		targetRouter.Prefixes = prefixes
		targetRouter.Neighbors = make([]frrtypes.Neighbor, len(router.Neighbors))
		for j, neighbor := range source.Spec.BGP.Routers[i].Neighbors {
			// set ToReceive to the default api value otherwise it wont match
			// when we compare to what the api server has
			neighbor.ToReceive = frrtypes.Receive{
				Allowed: frrtypes.AllowedInPrefixes{
					Mode: frrtypes.AllowRestricted,
				},
			}
			if neighbor.ToAdvertise.Allowed.Mode != frrtypes.AllowAll {
				neighbor.ToAdvertise.Allowed.Mode = frrtypes.AllowRestricted
				neighbor.ToAdvertise.Allowed.Prefixes = prefixes
			}
			targetRouter.Neighbors[j] = neighbor
		}
		routers = append(routers, targetRouter)
		targetRouterIndex := len(routers) - 1

		// VRFs are isolated in "auto" so no need to handle imports
		if targetNetwork == "auto" {
			continue
		}

		// we won't do imports if the pod network is not advetised
		if !ra.Spec.Advertisements.PodNetwork {
			continue
		}

		// handle imports: when the target VRF is not "auto" we need to leak
		// between the target VRF and the selected networks, reciprocally
		// importing from each
		for _, network := range networks { // ordered
			if network == targetNetwork {
				// skip self
				continue
			}

			// note that to import default, the VRF is "default" which matches
			// our own terminology for the default network
			routers[targetRouterIndex].Imports = append(routers[targetRouterIndex].Imports, frrtypes.Import{VRF: network})

			// add an additional router to import the target VRF into selected
			// network
			importRouter := frrtypes.Router{
				ASN:     router.ASN,
				ID:      router.ID,
				Imports: []frrtypes.Import{{VRF: targetNetwork}},
			}
			if network != types.DefaultNetworkName {
				importRouter.VRF = network
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
	new.Labels = map[string]string{
		types.OvnRouteAdvertisementsKey: ra.Name,
	}
	new.Annotations = map[string]string{
		types.OvnRouteAdvertisementsKey: fmt.Sprintf("%s/%s/%s", ra.Name, source.Name, nodeName),
	}
	new.Spec = source.Spec
	new.Spec.BGP.Routers = routers
	new.Spec.NodeSelector = meta.LabelSelector{
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
	selector, err := meta.LabelSelectorAsSelector(&meta.LabelSelector{
		MatchLabels: map[string]string{types.OvnRouteAdvertisementsKey: ra},
	})
	if err != nil {
		return hadUpdates, err
	}
	frrConfigs, err := c.frrLister.List(selector)
	if err != nil {
		return hadUpdates, err
	}
	// map them by our internal key
	existing := make(map[string]*frrtypes.FRRConfiguration, len(frrConfigs))
	for _, frrConfig := range frrConfigs {
		key := frrConfig.Annotations[types.OvnRouteAdvertisementsKey]
		if key == "" {
			continue
		}
		existing[key] = frrConfig
	}

	// go through the FRRConfigurations that should exist for this
	// RouteAdvertisements
	for _, newFRRConfig := range frrConfigurations {
		key := newFRRConfig.Annotations[types.OvnRouteAdvertisementsKey]
		oldFRRConfig := existing[key]

		if oldFRRConfig == nil {
			// does not exist, create
			_, err := c.frrClient.ApiV1beta1().FRRConfigurations(newFRRConfig.Namespace).Create(
				context.Background(),
				newFRRConfig,
				meta.CreateOptions{
					FieldManager: fieldManager,
				},
			)
			if err != nil {
				return hadUpdates, err
			}
			hadUpdates = true
			continue
		}

		// exists and will pontentially be updated, remove from map which will
		// be deleted at the end
		delete(existing, key)

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
			meta.UpdateOptions{
				FieldManager: fieldManager,
			},
		)
		if err != nil {
			return hadUpdates, err
		}
		hadUpdates = true
	}

	// delete FRRConfigurations that should not exist
	for _, obsoleteFRRConfig := range existing {
		err := c.frrClient.ApiV1beta1().FRRConfigurations(obsoleteFRRConfig.Namespace).Delete(
			context.Background(),
			obsoleteFRRConfig.Name,
			meta.DeleteOptions{},
		)
		if err != nil && !apierrors.IsNotFound(err) {
			return hadUpdates, err
		}
		hadUpdates = true
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
func (c *Controller) updateRAStatus(ra *ratypes.RouteAdvertisements, err error) error {
	if ra == nil {
		return nil
	}

	status := "Accepted"
	cstatus := meta.ConditionTrue
	reason := "Accepted"
	msg := "ovn-kubernetes cluster-manager validated the resource and requested the necessary configuration changes"
	if err != nil {
		status = fmt.Sprintf("Not Accepted: %v", err)
		cstatus = meta.ConditionFalse
		reason = "ConfigurationError"
		msg = err.Error()
	}
	_, err = c.raClient.K8sV1().RouteAdvertisements().ApplyStatus(
		context.Background(),
		raapply.RouteAdvertisements(ra.Name).WithStatus(
			raapply.RouteAdvertisementsStatus().WithStatus(status).WithConditions(
				metaapply.Condition().
					WithType("Accepted").
					WithStatus(cstatus).
					WithLastTransitionTime(meta.NewTime(time.Now())).
					WithReason(reason).
					WithMessage(msg).
					WithObservedGeneration(ra.Generation),
			),
		),
		meta.ApplyOptions{
			FieldManager: fieldManager,
		},
	)
	return err
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
			ObjectMeta: meta.ObjectMeta{
				Name:      types.DefaultNetworkName,
				Namespace: config.Kubernetes.OVNConfigNamespace,
			},
			Spec: nadtypes.NetworkAttachmentDefinitionSpec{
				Config: fmt.Sprintf("{\"cniVersion\": \"0.4.0\", \"name\": \"ovn-kubernetes\", \"type\": \"%s\"}", config.CNI.Plugin),
			},
		},
		meta.CreateOptions{
			FieldManager: fieldManager,
		},
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
			eipsByNode[item.Node] = append(eipsByNode[item.Node], item.EgressIP)
		}
	}

	return eipsByNode, nil
}

// isOwnUpdate checks if an object was updated by us last, as indicated by its
// managed fields. Used to avoid reconciling an update that we made ourselves.
func isOwnUpdate(managedFields []meta.ManagedFieldsEntry) bool {
	var lastUpdateTheirs, lastUpdateOurs time.Time
	for _, managedFieldEntry := range managedFields {
		switch managedFieldEntry.Manager {
		case fieldManager:
			if managedFieldEntry.Time.Time.After(lastUpdateOurs) {
				lastUpdateOurs = managedFieldEntry.Time.Time
			}
		default:
			if managedFieldEntry.Time.Time.After(lastUpdateTheirs) {
				lastUpdateTheirs = managedFieldEntry.Time.Time
			}
		}
	}
	return lastUpdateOurs.After(lastUpdateTheirs)
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
		if ra.Spec.Advertisements.EgressIP {
			c.raController.Reconcile(ra.Name)
		}
	}

	return nil
}
