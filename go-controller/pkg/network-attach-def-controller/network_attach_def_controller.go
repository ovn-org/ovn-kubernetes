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
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	nettypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	nadinformers "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/informers/externalversions/k8s.cni.cncf.io/v1"
	nadlisters "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/listers/k8s.cni.cncf.io/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/controller"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

var ErrNetworkControllerTopologyNotManaged = errors.New("no cluster network controller to manage topology")

type BaseNetworkController interface {
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
}

type watchFactory interface {
	NADInformer() nadinformers.NetworkAttachmentDefinitionInformer
}

// NetAttachDefinitionController handles namespaced scoped NAD events and
// manages cluster scoped networks defined in those NADs. NADs are mostly
// referred from pods to give them access to the network. Different NADs can
// define the same network as long as those definitions are actually equal.
// Unexpected situations are handled on best effort basis but improper NAD
// adminstration can lead to undefined behavior in referred from running pods.
type NetAttachDefinitionController struct {
	name               string
	netAttachDefLister nadlisters.NetworkAttachmentDefinitionLister
	controller         controller.Controller

	// networkManager is used to manage the network controllers
	networkManager networkManager

	networks map[string]util.NetInfo

	// nads to network mapping
	nads map[string]string
}

func NewNetAttachDefinitionController(
	name string,
	ncm NetworkControllerManager,
	wf watchFactory,
) (*NetAttachDefinitionController, error) {
	nadController := &NetAttachDefinitionController{
		name:           fmt.Sprintf("[%s NAD controller]", name),
		networkManager: newNetworkManager(name, ncm),
		networks:       map[string]util.NetInfo{},
		nads:           map[string]string{},
	}

	config := &controller.ControllerConfig[nettypes.NetworkAttachmentDefinition]{
		RateLimiter:    workqueue.DefaultControllerRateLimiter(),
		Reconcile:      nadController.sync,
		ObjNeedsUpdate: nadNeedsUpdate,
		// this controller is not thread safe
		Threadiness: 1,
	}

	nadInformer := wf.NADInformer()
	if nadInformer != nil {
		nadController.netAttachDefLister = nadInformer.Lister()
		config.Informer = nadInformer.Informer()
		config.Lister = nadController.netAttachDefLister.List
	}

	nadController.controller = controller.NewController(
		nadController.name,
		config,
	)

	return nadController, nil
}

func (nadController *NetAttachDefinitionController) Start() error {
	// initial sync here will ensure networks in network manager
	// network manager will use this initial set of ensured networks to consider
	// any other network stale on its own sync
	err := controller.StartWithInitialSync(
		nadController.syncAll,
		nadController.controller,
	)
	if err != nil {
		return err
	}

	err = nadController.networkManager.Start()
	if err != nil {
		return err
	}

	klog.Infof("%s: started", nadController.name)
	return nil
}

func (nadController *NetAttachDefinitionController) Stop() {
	klog.Infof("%s: shutting down", nadController.name)
	controller.Stop(nadController.controller)
	nadController.networkManager.Stop()
}

func (nadController *NetAttachDefinitionController) syncAll() (err error) {
	existingNADs, err := nadController.netAttachDefLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("%s: failed to list NADs: %w", nadController.name, err)
	}

	// create all networks with their updated list of NADs and only then start
	// the corresponding controllers so as to avoid to the extent possible the
	// errors and retries that would result if the controller attempted to
	// process pods attached with NADs we wouldn't otherwise know about yet
	for _, nad := range existingNADs {
		key, err := cache.MetaNamespaceKeyFunc(nad)
		if err != nil {
			klog.Errorf("%s: failed to sync %v: %v", nadController.name, nad, err)
			continue
		}
		err = nadController.syncNAD(key, nad)
		if err != nil {
			return fmt.Errorf("%s: failed to sync %s: %v", nadController.name, key, err)
		}
	}

	return nil
}

func (nadController *NetAttachDefinitionController) sync(key string) error {
	startTime := time.Now()
	klog.V(5).Infof("%s: sync NAD %s", nadController.name, key)
	defer func() {
		klog.V(4).Infof("%s: finished syncing NAD %s, took %v", nadController.name, key, time.Since(startTime))
	}()

	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		klog.Errorf("%s: failed splitting key %s: %v", nadController.name, key, err)
		return nil
	}

	nad, err := nadController.netAttachDefLister.NetworkAttachmentDefinitions(namespace).Get(name)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}

	return nadController.syncNAD(key, nad)
}

func (nadController *NetAttachDefinitionController) syncNAD(key string, nad *nettypes.NetworkAttachmentDefinition) error {
	var nadNetworkName string
	var nadNetwork, oldNetwork, ensureNetwork util.NetInfo
	var err error

	if nad != nil {
		nadNetwork, err = util.ParseNADInfo(nad)
		if err != nil {
			klog.Errorf("%s: failed parsing NAD %s: %v", nadController.name, key, err)
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
	if nadNetworkName != nadController.nads[key] {
		oldNetwork = nadController.networks[nadController.nads[key]]
	}

	currentNetwork := nadController.networks[nadNetworkName]

	switch {
	case currentNetwork == nil:
		// the NAD refers to a new network, ensure it
		ensureNetwork = nadNetwork
		nadController.networks[nadNetworkName] = ensureNetwork
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
		err = fmt.Errorf("%s: NAD %s CNI config does not match that of network %s", nadController.name, key, nadNetworkName)
	}

	// remove the NAD reference from the old network and delete the network if
	// it is no longer referenced
	if oldNetwork != nil {
		oldNetworkName := oldNetwork.GetNetworkName()
		oldNetwork.DeleteNADs(key)
		if len(oldNetwork.GetNADs()) == 0 {
			nadController.networkManager.DeleteNetwork(oldNetworkName)
			delete(nadController.networks, oldNetworkName)
		} else {
			nadController.networkManager.EnsureNetwork(oldNetwork)
		}
	}

	// this was a nad delete
	if ensureNetwork == nil {
		delete(nadController.nads, key)
		return err
	}

	if ensureNetwork.IsDefault() {
		klog.V(5).Infof("%s: NAD add for default network (key %s), skip it", nadController.name, key)
		return nil
	}

	// ensure the network associated with the NAD
	ensureNetwork.AddNADs(key)
	nadController.nads[key] = ensureNetwork.GetNetworkName()
	nadController.networkManager.EnsureNetwork(ensureNetwork)
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

	return !reflect.DeepEqual(oldNAD.Spec, newNAD.Spec)
}
