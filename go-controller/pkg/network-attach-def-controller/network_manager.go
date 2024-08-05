package networkAttachDefController

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
	"time"

	nadlisters "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/listers/k8s.cni.cncf.io/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/controller"
	ratypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1"
	ralisters "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1/apis/listers/routeadvertisements/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

// networkManager is a level driven controller that handles creating, starting,
// stopping and cleaning up network controllers
type networkManager interface {
	// EnsureNetwork will add the network controller for the provided network
	// configuration. If a controller already exists for the same network with a
	// different configuration, the existing controller is stopped and cleaned
	// up before creating the new one. If a controller already exists for the
	// same network configuration, it synchronizes the list of NADs to the
	// controller.
	EnsureNetwork(util.NetInfo)

	// DeleteNetwork removes the network controller for the provided network
	DeleteNetwork(string)

	// Start the controller
	Start() error

	// Stop the controller
	Stop()
}

func newNetworkManager(name, zone, node string, ncm NetworkControllerManager, wf watchFactory) networkManager {
	nc := &networkManagerImpl{
		name:               fmt.Sprintf("[%s network manager]", name),
		node:               node,
		zone:               zone,
		ncm:                ncm,
		networks:           map[string]util.NetInfo{},
		networkControllers: map[string]*networkControllerState{},
	}

	// this controller does not feed from an informer, networks are manually
	// added to the queue for processing
	networkConfig := &controller.ReconcilerConfig{
		RateLimiter: workqueue.DefaultControllerRateLimiter(),
		Reconcile:   nc.syncNetwork,
		Threadiness: 1,
	}
	nc.networkReconciler = controller.NewReconciler(
		nc.name,
		networkConfig,
	)

	// we don't care about route advertisements in cluster manager
	if nc.hasRouteAdvertisements() {
		nc.nadLister = wf.NADInformer().Lister()
		nc.raLister = wf.RouteAdvertisementsInformer().Lister()
		nc.nodeLister = wf.NodeCoreInformer().Lister()

		// ra controller
		raConfig := &controller.ControllerConfig[ratypes.RouteAdvertisements]{
			RateLimiter:    workqueue.DefaultControllerRateLimiter(),
			Informer:       wf.RouteAdvertisementsInformer().Informer(),
			Lister:         nc.raLister.List,
			Reconcile:      func(string) error { return nc.syncRunningNetworks() },
			ObjNeedsUpdate: raNeedsUpdate,
			Threadiness:    1,
		}
		nc.raController = controller.NewController(
			nc.name,
			raConfig,
		)

		// node controller
		nodeConfig := &controller.ControllerConfig[corev1.Node]{
			RateLimiter:    workqueue.DefaultControllerRateLimiter(),
			Informer:       wf.NodeCoreInformer().Informer(),
			Lister:         nc.nodeLister.List,
			Reconcile:      func(string) error { return nc.syncRunningNetworks() },
			ObjNeedsUpdate: nodeNeedsUpdate,
			Threadiness:    1,
		}
		nc.nodeController = controller.NewController(
			nc.name,
			nodeConfig,
		)
	}

	return nc
}

type networkControllerState struct {
	controller         NetworkController
	stoppedAndDeleting bool
}

type networkManagerImpl struct {
	sync.Mutex

	name string
	zone string
	node string

	nadLister  nadlisters.NetworkAttachmentDefinitionLister
	raLister   ralisters.RouteAdvertisementsLister
	nodeLister corelisters.NodeLister

	networkReconciler controller.Reconciler
	raController      controller.Controller
	nodeController    controller.Controller

	ncm                NetworkControllerManager
	networks           map[string]util.NetInfo
	networkControllers map[string]*networkControllerState
}

// Start will cleanup stale networks that have not been ensured via
// EnsuredNetwork before this call
func (nm *networkManagerImpl) Start() error {
	controllers := []controller.Reconciler{nm.networkReconciler}
	if nm.raController != nil {
		controllers = append(controllers, nm.raController)
	}
	if nm.nodeController != nil {
		controllers = append(controllers, nm.nodeController)
	}
	return controller.StartWithInitialSync(
		nm.syncStaleNetworks,
		controllers...,
	)
}

func (nm *networkManagerImpl) Stop() {
	controllers := []controller.Reconciler{nm.networkReconciler}
	if nm.raController != nil {
		controllers = append(controllers, nm.raController)
	}
	if nm.nodeController != nil {
		controllers = append(controllers, nm.nodeController)
	}
	controller.Stop(controllers...)

	for _, networkControllerState := range nm.networkControllers {
		networkControllerState.controller.Stop()
	}
}

func (nm *networkManagerImpl) EnsureNetwork(network util.NetInfo) {
	nm.Lock()
	defer nm.Unlock()
	nm.networks[network.GetNetworkName()] = network
	nm.networkReconciler.Reconcile(network.GetNetworkName())
}

func (nm *networkManagerImpl) DeleteNetwork(network string) {
	nm.Lock()
	defer nm.Unlock()
	delete(nm.networks, network)
	if network == types.DefaultNetworkName {
		// for the default network however ensure it runs with the default
		// config
		nm.networks[types.DefaultNetworkName] = &util.DefaultNetInfo{}
	}
	nm.networkReconciler.Reconcile(network)
}

func (nm *networkManagerImpl) syncStaleNetworks() error {
	// as we sync upon start, consider networks that have not been ensured as
	// stale and clean them up
	validNetworks := make([]util.BasicNetInfo, 0, len(nm.networks))
	for _, network := range nm.networks {
		validNetworks = append(validNetworks, network)
	}
	return nm.ncm.CleanupDeletedNetworks(validNetworks...)
}

func (nm *networkManagerImpl) syncRunningNetworks() error {
	nm.Lock()
	defer nm.Unlock()

	for network := range nm.networkControllers {
		nm.networkReconciler.Reconcile(network)
	}

	return nil
}

func (nm *networkManagerImpl) syncNetwork(network string) error {
	nm.Lock()
	defer nm.Unlock()

	startTime := time.Now()
	klog.V(5).Infof("%s: sync network %s", nm.name, network)
	defer func() {
		klog.V(4).Infof("%s: finished syncing network %s, took %v", nm.name, network, time.Since(startTime))
	}()

	want := nm.networks[network]
	have := nm.networkControllers[network]

	// we will dispose of the old network if deletion is in progress or if
	// non-reconcilable configuration changed
	dispose := have != nil && (have.stoppedAndDeleting || !have.controller.Equals(want))
	if dispose {
		err := nm.deleteNetwork(network)
		if err != nil {
			return err
		}
	}

	// no network needed so nothing to do
	if want == nil {
		return nil
	}

	// ensure the network
	err := nm.ensureNetwork(want)
	if err != nil {
		return fmt.Errorf("%s: failed to ensure network %s: %w", nm.name, network, err)
	}

	return nil
}

func (nm *networkManagerImpl) ensureNetwork(network util.NetInfo) error {
	networkName := network.GetNetworkName()

	err := nm.setVRFs(network)
	if err != nil {
		return fmt.Errorf("failed to set VRFs for network %s: %w", networkName, err)
	}

	var reconcilable ReconcilableNetworkController
	switch network.IsDefault() {
	case true:
		reconcilable = nm.ncm.GetDefaultNetworkController()
		if reconcilable == nil {
			// no default network controller to act on
			return nil
		}
	default:
		state := nm.networkControllers[network.GetNetworkName()]
		if state != nil {
			reconcilable = state.controller
		}
	}

	// this might just be an update of reconcilable network configuration
	if reconcilable != nil {
		err := reconcilable.Reconcile(network)
		if err != nil {
			return fmt.Errorf("failed to reconcile network %s: %w", networkName, err)
		}
		return nil
	}

	// otherwise setup & start the new network controller
	nc, err := nm.ncm.NewNetworkController(util.CopyNetInfo(network))
	if err != nil {
		return fmt.Errorf("failed to create network %s: %w", networkName, err)
	}

	err = nc.Start(context.Background())
	if err != nil {
		return fmt.Errorf("failed to start network %s: %w", networkName, err)
	}
	nm.networkControllers[network.GetNetworkName()] = &networkControllerState{controller: nc}

	return nil
}

func (nm *networkManagerImpl) deleteNetwork(network string) error {
	have := nm.networkControllers[network]
	if have == nil {
		return nil
	}

	if !have.stoppedAndDeleting {
		have.controller.Stop()
	}
	have.stoppedAndDeleting = true

	err := have.controller.Cleanup()
	if err != nil {
		return fmt.Errorf("%s: failed to cleanup network %s: %w", nm.name, network, err)
	}

	delete(nm.networkControllers, network)
	return nil
}

func (nm *networkManagerImpl) setVRFs(network util.NetInfo) error {
	if network.IsSecondary() {
		return nil
	}
	if !nm.hasRouteAdvertisements() {
		// we won't look after VRFs in cluster manager
		return nil
	}

	raNames := sets.New[string]()
	for _, nadNamespacedName := range network.GetNADs() {
		namespace, name, err := cache.SplitMetaNamespaceKey(nadNamespacedName)
		if err != nil {
			return err
		}

		nad, err := nm.nadLister.NetworkAttachmentDefinitions(namespace).Get(name)
		if err != nil {
			return err
		}

		var nadRANames []string
		err = json.Unmarshal([]byte(nad.Annotations[util.OvnRouteAdvertisements]), &nadRANames)
		if err != nil {
			return err
		}

		raNames.Insert(nadRANames...)
	}

	vrfs := map[string][]string{}
	for raName := range raNames {
		ra, err := nm.raLister.Get(raName)
		if err != nil {
			return err
		}

		if !ra.Spec.Advertisements.PodNetwork {
			continue
		}

		// TODO check RA status

		nodeSelector, err := metav1.LabelSelectorAsSelector(&ra.Spec.NodeSelector)
		if err != nil {
			return err
		}

		nodes, err := nm.nodeLister.List(nodeSelector)
		if err != nil {
			return err
		}

		for _, node := range nodes {
			if node.Name == nm.node || util.GetNodeZone(node) == nm.zone {
				vrfs[node.Name] = append(vrfs[node.Name], ra.Spec.TargetVRF)
			}
		}
	}

	network.SetVRFs(vrfs)
	return nil
}

func (nm *networkManagerImpl) hasRouteAdvertisements() bool {
	isClusterManager := nm.zone == "" && nm.node == ""
	return config.OVNKubernetesFeature.EnableRouteAdvertisements && !isClusterManager
}

func raNeedsUpdate(oldRA, newRA *ratypes.RouteAdvertisements) bool {
	if oldRA == nil || newRA == nil {
		// handle RA add/delete through the NAD annotation update
		return false
	}

	// don't process resync or objects that are marked for deletion
	if oldRA.ResourceVersion == newRA.ResourceVersion ||
		!newRA.GetDeletionTimestamp().IsZero() {
		return false
	}

	return oldRA.Spec.TargetVRF != newRA.Spec.TargetVRF || !reflect.DeepEqual(oldRA.Spec.NodeSelector, newRA.Spec.NodeSelector)
}

func nodeNeedsUpdate(oldNode, newNode *corev1.Node) bool {
	if oldNode == nil || newNode == nil {
		return true
	}

	// don't process resync or objects that are marked for deletion
	if oldNode.ResourceVersion == newNode.ResourceVersion ||
		!newNode.GetDeletionTimestamp().IsZero() {
		return false
	}

	return !reflect.DeepEqual(oldNode.Labels, newNode.Labels) || oldNode.Annotations[util.OvnNodeZoneName] != newNode.Annotations[util.OvnNodeZoneName]
}
