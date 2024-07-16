package networkAttachDefController

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	nettypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	nadinformers "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/informers/externalversions/k8s.cni.cncf.io/v1"
	nadlisters "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/listers/k8s.cni.cncf.io/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/controller"
	rainformers "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1/apis/informers/externalversions/routeadvertisements/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

var ErrNetworkControllerTopologyNotManaged = errors.New("no cluster network controller to manage topology")

type ReconcilableNetworkController interface {
	Reconcile(util.ReconcilableNetInfo) error
}

type BaseNetworkController interface {
	ReconcilableNetworkController
	Start(ctx context.Context) error
	Stop()
}

type NetworkController interface {
	BaseNetworkController
	util.NetInfo
	// Cleanup cleans up the NetworkController-owned resources, it could be called to clean up network controllers that are deleted when
	// ovn-k8s is down; so it's receiver could be a dummy network controller, it just needs to know its network name.
	Cleanup() error
}

// NetworkControllerManager manages all network controllers
type NetworkControllerManager interface {
	NewNetworkController(netInfo util.NetInfo) (NetworkController, error)
	CleanupDeletedNetworks(validNetworks ...util.BasicNetInfo) error
	GetDefaultNetworkController() ReconcilableNetworkController
}

type watchFactory interface {
	NADInformer() nadinformers.NetworkAttachmentDefinitionInformer
	RouteAdvertisementsInformer() rainformers.RouteAdvertisementsInformer
	NodeCoreInformer() coreinformers.NodeInformer
}

// NADController handles namespaced scoped NAD events and
// manages cluster scoped networks defined in those NADs. NADs are mostly
// referred from pods to give them access to the network. Different NADs can
// define the same network as long as those definitions are actually equal.
// Unexpected situations are handled on best effort basis but improper NAD
// adminstration can lead to undefined behavior in referred from running pods.
type NADController struct {
	name               string
	netAttachDefLister nadlisters.NetworkAttachmentDefinitionLister
	controller         controller.Controller

	// networkManager is used to manage the network controllers
	networkManager networkManager

	networks map[string]util.NetInfo

	// nads to network mapping
	nads map[string]string
}

// NewClusterNADController builds a NAD controller for cluster manager
func NewClusterNADController(
	name string,
	ncm NetworkControllerManager,
	wf watchFactory,
) (*NADController, error) {
	return newNADController(
		name,
		ncm,
		wf,
		"",
		"",
	)
}

// NewZoneNADController builds a NAD controller for OVN network manager
func NewZoneNADController(
	name string,
	ncm NetworkControllerManager,
	wf watchFactory,
	zone string,
) (*NADController, error) {
	return newNADController(
		name,
		ncm,
		wf,
		zone,
		"",
	)
}

// NewNodeNADController builds a NAD controller for node network manager
func NewNodeNADController(
	name string,
	ncm NetworkControllerManager,
	wf watchFactory,
	node string,
) (*NADController, error) {
	return newNADController(
		name,
		ncm,
		wf,
		"",
		node,
	)
}

func newNADController(
	name string,
	ncm NetworkControllerManager,
	wf watchFactory,
	zone string,
	node string,
) (*NADController, error) {
	c := &NADController{
		name:               fmt.Sprintf("[%s NAD controller]", name),
		netAttachDefLister: wf.NADInformer().Lister(),
		networkManager:     newNetworkManager(name, zone, node, ncm, wf),
		networks:           map[string]util.NetInfo{},
		nads:               map[string]string{},
	}
	config := &controller.ControllerConfig[nettypes.NetworkAttachmentDefinition]{
		RateLimiter:    workqueue.DefaultControllerRateLimiter(),
		Informer:       wf.NADInformer().Informer(),
		Lister:         c.netAttachDefLister.List,
		Reconcile:      c.sync,
		ObjNeedsUpdate: nadNeedsUpdate,
		// this controller is not thread safe
		Threadiness: 1,
	}
	c.controller = controller.NewController(
		c.name,
		config,
	)

	return c, nil
}

func (c *NADController) Start() error {
	// initial sync here will ensure networks in network manager
	// network manager will use this initial set of ensured networks to consider
	// any other network stale on its own sync
	err := controller.StartWithInitialSync(
		c.syncAll,
		c.controller,
	)
	if err != nil {
		return err
	}

	err = c.networkManager.Start()
	if err != nil {
		return err
	}

	klog.Infof("%s: started", c.name)
	return nil
}

func (c *NADController) Stop() {
	klog.Infof("%s: shutting down", c.name)
	controller.Stop(c.controller)
	c.networkManager.Stop()
}

func (c *NADController) syncAll() (err error) {
	existingNADs, err := c.netAttachDefLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("%s: failed to list NADs: %w", c.name, err)
	}

	// create all networks with their updated list of NADs and only then start
	// the corresponding controllers so as to avoid to the extent possible the
	// errors and retries that would result if the controller attempted to
	// process pods attached with NADs we wouldn't otherwise know about yet
	for _, nad := range existingNADs {
		key, err := cache.MetaNamespaceKeyFunc(nad)
		if err != nil {
			klog.Errorf("%s: failed to sync %v: %v", c.name, nad, err)
			continue
		}
		err = c.syncNAD(key, nad)
		if err != nil {
			return fmt.Errorf("%s: failed to sync %s: %v", c.name, key, err)
		}
	}

	return nil
}

func (c *NADController) sync(key string) error {
	startTime := time.Now()
	klog.V(5).Infof("%s: sync NAD %s", c.name, key)
	defer func() {
		klog.V(4).Infof("%s: finished syncing NAD %s, took %v", c.name, key, time.Since(startTime))
	}()

	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		klog.Errorf("%s: failed splitting key %s: %v", c.name, key, err)
		return nil
	}

	nad, err := c.netAttachDefLister.NetworkAttachmentDefinitions(namespace).Get(name)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}

	return c.syncNAD(key, nad)
}

func (c *NADController) syncNAD(key string, nad *nettypes.NetworkAttachmentDefinition) error {
	var nadNetworkName string
	var nadNetwork, oldNetwork, ensureNetwork util.NetInfo
	var err error

	if nad != nil {
		nadNetwork, err = util.ParseNADInfo(nad)
		if err != nil {
			klog.Errorf("%s: failed parsing NAD %s: %v", c.name, key, err)
			return nil
		}
		nadNetworkName = nadNetwork.GetNetworkName()
	}

	// As multiple NADs may define networks with the same name, these networks
	// should also have the same config to be considered compatible. If an
	// incompatible network change happens on NAD update, we can:
	// - Re-create network with the same name but different config, if it is not
	//   referred to by any other NAD
	// - Return an error AND clean up NAD from the old network

	// the NAD refers to a different network than before
	if nadNetworkName != c.nads[key] {
		oldNetwork = c.networks[c.nads[key]]
	}

	currentNetwork := c.networks[nadNetworkName]

	switch {
	case currentNetwork == nil:
		// the NAD refers to a new network, ensure it
		ensureNetwork = nadNetwork
		c.networks[nadNetworkName] = ensureNetwork
	case currentNetwork.Equals(nadNetwork):
		// the NAD refers to an existing compatible network, ensure that
		// existing network holds a reference to this NAD
		ensureNetwork = currentNetwork
	case sets.New(key).HasAll(currentNetwork.GetNADs()...):
		// the NAD is the only NAD referring to an existing incompatible
		// network, remove the reference from the old network and ensure that
		// existing network holds a reference to this NAD
		oldNetwork = currentNetwork
		ensureNetwork = nadNetwork
	// the NAD refers to an existing incompatible network referred by other
	// NADs, return error
	case oldNetwork == nil:
		// the NAD refers to the same network as before but with different
		// incompatible configuration, remove the NAD reference from the network
		oldNetwork = currentNetwork
		fallthrough
	default:
		err = fmt.Errorf("%s: NAD %s CNI config does not match that of network %s", c.name, key, nadNetworkName)
	}

	// remove the NAD reference from the old network and delete the network if
	// it is no longer referenced
	if oldNetwork != nil {
		oldNetworkName := oldNetwork.GetNetworkName()
		oldNetwork.DeleteNADs(key)
		if len(oldNetwork.GetNADs()) == 0 {
			c.networkManager.DeleteNetwork(oldNetworkName)
			delete(c.networks, oldNetworkName)
		} else {
			c.networkManager.EnsureNetwork(oldNetwork)
		}
	}

	// this was a nad delete
	if ensureNetwork == nil {
		delete(c.nads, key)
		return err
	}

	// ensure the network associated with the NAD we also reconcile the network
	// in case route advertisements changed
	ensureNetwork.AddNADs(key)
	c.nads[key] = ensureNetwork.GetNetworkName()
	c.networkManager.EnsureNetwork(ensureNetwork)
	return err
}

func nadNeedsUpdate(oldNAD, newNAD *nettypes.NetworkAttachmentDefinition) bool {
	if oldNAD == nil || newNAD == nil {
		return true
	}

	// don't process resync or objects that are marked for deletion
	if oldNAD.ResourceVersion == newNAD.ResourceVersion ||
		!newNAD.GetDeletionTimestamp().IsZero() {
		return false
	}

	// also reconcile the network in case its route advertisements changed
	return !reflect.DeepEqual(oldNAD.Spec, newNAD.Spec) ||
		oldNAD.Annotations[util.OvnRouteAdvertisements] != newNAD.Annotations[util.OvnRouteAdvertisements]
}
