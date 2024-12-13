package networkAttachDefController

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	coreinformers "k8s.io/client-go/informers/core/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	nettypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	nadinformers "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/informers/externalversions/k8s.cni.cncf.io/v1"
	nadlisters "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/listers/k8s.cni.cncf.io/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/controller"
	userdefinednetworkinformer "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/informers/externalversions/userdefinednetwork/v1"
	userdefinednetworklister "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/listers/userdefinednetwork/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	utiludn "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/udn"
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
	UserDefinedNetworkInformer() userdefinednetworkinformer.UserDefinedNetworkInformer
	ClusterUserDefinedNetworkInformer() userdefinednetworkinformer.ClusterUserDefinedNetworkInformer
	NamespaceInformer() coreinformers.NamespaceInformer
}

type NADController interface {
	Start() error
	Stop()
	GetActiveNetworkForNamespace(namespace string) (util.NetInfo, error)
	GetNetwork(networkName string) (util.NetInfo, error)
	// DoWithLock takes care of locking and unlocking while iterating over all role primary user defined networks.
	DoWithLock(f func(network util.NetInfo) error) error
	GetActiveNetworkNamespaces(networkName string) ([]string, error)
}

// NetAttachDefinitionController handles namespaced scoped NAD events and
// manages cluster scoped networks defined in those NADs. NADs are mostly
// referenced from pods to give them access to the network. Different NADs can
// define the same network as long as those definitions are actually equal.
// Unexpected situations are handled on best effort basis but improper NAD
// administration can lead to undefined behavior if referenced by running pods.
type NetAttachDefinitionController struct {
	sync.RWMutex
	name               string
	netAttachDefLister nadlisters.NetworkAttachmentDefinitionLister
	udnLister          userdefinednetworklister.UserDefinedNetworkLister
	cudnLister         userdefinednetworklister.ClusterUserDefinedNetworkLister
	namespaceLister    corev1listers.NamespaceLister
	controller         controller.Controller
	recorder           record.EventRecorder
	// networkManager is used to manage the network controllers
	networkManager networkManager

	// nads to network mapping
	nads map[string]string

	// primaryNADs holds a mapping of namespace to primary NAD names
	primaryNADs map[string]string
}

func NewNetAttachDefinitionController(
	name string,
	ncm NetworkControllerManager,
	wf watchFactory,
	recorder record.EventRecorder,
) (*NetAttachDefinitionController, error) {
	nadController := &NetAttachDefinitionController{
		name:           fmt.Sprintf("[%s NAD controller]", name),
		recorder:       recorder,
		nads:           map[string]string{},
		primaryNADs:    map[string]string{},
		networkManager: newNetworkManager(name, ncm),
	}

	config := &controller.ControllerConfig[nettypes.NetworkAttachmentDefinition]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:      nadController.sync,
		ObjNeedsUpdate: nadNeedsUpdate,
		Threadiness:    1,
	}

	nadInformer := wf.NADInformer()
	if nadInformer != nil {
		nadController.netAttachDefLister = nadInformer.Lister()
		config.Informer = nadInformer.Informer()
		config.Lister = nadController.netAttachDefLister.List
	}
	if util.IsNetworkSegmentationSupportEnabled() {
		if udnInformer := wf.UserDefinedNetworkInformer(); udnInformer != nil {
			nadController.udnLister = udnInformer.Lister()
		}
		if cudnInformer := wf.ClusterUserDefinedNetworkInformer(); cudnInformer != nil {
			nadController.cudnLister = cudnInformer.Lister()
		}
		if nsInformer := wf.NamespaceInformer(); nsInformer != nil {
			nadController.namespaceLister = nsInformer.Lister()
		}
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

	namespace, _, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return fmt.Errorf("%s: failed splitting key %s: %v", nadController.name, key, err)
	}

	if nad != nil {
		nadNetwork, err = util.ParseNADInfo(nad)
		if err != nil {
			if nadController.recorder != nil {
				nadController.recorder.Eventf(&corev1.ObjectReference{Kind: nad.Kind, Namespace: nad.Namespace, Name: nad.Name}, corev1.EventTypeWarning,
					"InvalidConfig", "Failed to parse network config: %v", err.Error())
			}
			klog.Errorf("%s: failed parsing NAD %s: %v", nadController.name, key, err)
			return nil
		}
		nadNetworkName = nadNetwork.GetNetworkName()
	}

	nadController.Lock()
	defer nadController.Unlock()
	// We can only have one primary NAD per namespace
	primaryNAD := nadController.primaryNADs[namespace]
	if nadNetwork != nil && nadNetwork.IsPrimaryNetwork() && primaryNAD != "" && primaryNAD != key {
		return fmt.Errorf("%s: NAD %s is primary for the namespace, NAD %s can't be primary", nadController.name, primaryNAD, key)
	}

	// As multiple NADs may define networks with the same name, these networks
	// should also have the same config to be considered compatible. If an
	// incompatible network change happens on NAD update, we can:
	// - Re-create network with the same name but different config, if it is not
	//   referred to by any other NAD
	// - Return an error AND clean up NAD from the old network

	// the NAD refers to a different network than before
	if nadNetworkName != nadController.nads[key] {
		oldNetwork = nadController.networkManager.getNetwork(nadController.nads[key])
	}
	currentNetwork := nadController.networkManager.getNetwork(nadNetworkName)

	switch {
	case currentNetwork == nil:
		// the NAD refers to a new network, ensure it
		ensureNetwork = nadNetwork
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
		} else {
			nadController.networkManager.EnsureNetwork(oldNetwork)
		}
	}

	// this was a nad delete
	if ensureNetwork == nil {
		delete(nadController.nads, key)
		if nadController.primaryNADs[namespace] == key {
			delete(nadController.primaryNADs, namespace)
		}
		return err
	}

	if ensureNetwork.IsDefault() {
		klog.V(5).Infof("%s: NAD add for default network (key %s), skip it", nadController.name, key)
		return nil
	}

	// ensure the network is associated with the NAD
	ensureNetwork.AddNADs(key)
	nadController.nads[key] = ensureNetwork.GetNetworkName()
	// track primary NAD
	switch {
	case ensureNetwork.IsPrimaryNetwork():
		nadController.primaryNADs[namespace] = key
	default:
		if nadController.primaryNADs[namespace] == key {
			delete(nadController.primaryNADs, namespace)
		}
	}

	// reconcile the network
	nadController.networkManager.EnsureNetwork(ensureNetwork)
	return nil
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

func (nadController *NetAttachDefinitionController) GetActiveNetworkForNamespace(namespace string) (util.NetInfo, error) {
	if !util.IsNetworkSegmentationSupportEnabled() {
		return &util.DefaultNetInfo{}, nil
	}
	nadController.RLock()
	defer nadController.RUnlock()
	primaryNAD := nadController.primaryNADs[namespace]
	if primaryNAD != "" {
		// we have a primary NAD, get the network
		netName := nadController.nads[primaryNAD]
		if netName == "" {
			// this should never happen where we have a nad keyed in the primaryNADs
			// map, but it doesn't exist in the nads map
			panic("NAD Controller broken consistency between primary NADs and cached NADs")
		}
		network := nadController.networkManager.getNetwork(netName)
		n := util.CopyNetInfo(network)
		// update the returned netInfo copy to only have the primary NAD for this namespace
		n.SetNADs(primaryNAD)
		return n, nil
	}

	// no primary network found, make sure we just haven't processed it yet and no UDN / CUDN exists
	udns, err := nadController.udnLister.UserDefinedNetworks(namespace).List(labels.Everything())
	if err != nil {
		return nil, fmt.Errorf("error getting user defined networks: %w", err)
	}
	for _, udn := range udns {
		if utiludn.IsPrimaryNetwork(&udn.Spec) {
			return nil, util.NewUnprocessedActiveNetworkError(namespace, udn.Name)
		}
	}
	cudns, err := nadController.cudnLister.List(labels.Everything())
	if err != nil {
		return nil, fmt.Errorf("failed to list CUDNs: %w", err)
	}
	for _, cudn := range cudns {
		if !utiludn.IsPrimaryNetwork(&cudn.Spec.Network) {
			continue
		}
		// check the subject namespace referred by the specified namespace-selector
		cudnNamespaceSelector, err := metav1.LabelSelectorAsSelector(&cudn.Spec.NamespaceSelector)
		if err != nil {
			return nil, fmt.Errorf("failed to convert CUDN %q namespaceSelector: %w", cudn.Name, err)
		}
		selectedNamespaces, err := nadController.namespaceLister.List(cudnNamespaceSelector)
		if err != nil {
			return nil, fmt.Errorf("failed to list namespaces using selector %q: %w", cudnNamespaceSelector, err)
		}
		for _, ns := range selectedNamespaces {
			if ns.Name == namespace {
				return nil, util.NewUnprocessedActiveNetworkError(namespace, cudn.Name)
			}
		}
	}

	return &util.DefaultNetInfo{}, nil
}

func (nadController *NetAttachDefinitionController) GetNetwork(networkName string) (util.NetInfo, error) {
	if !util.IsNetworkSegmentationSupportEnabled() {
		return &util.DefaultNetInfo{}, nil
	}
	nadController.RLock()
	defer nadController.RUnlock()
	if networkName == "" {
		return nil, fmt.Errorf("network must not be empty")
	}
	if networkName == "default" {
		return &util.DefaultNetInfo{}, nil
	}
	network := nadController.networkManager.getNetwork(networkName)
	if network == nil {
		return nil, fmt.Errorf("failed to find network %q", networkName)
	}
	return util.CopyNetInfo(network), nil
}

func (nadController *NetAttachDefinitionController) GetActiveNetworkNamespaces(networkName string) ([]string, error) {
	if !util.IsNetworkSegmentationSupportEnabled() {
		return []string{"default"}, nil
	}
	namespaces := make([]string, 0)
	nadController.RLock()
	defer nadController.RUnlock()
	for namespaceName, primaryNAD := range nadController.primaryNADs {
		nadNetworkName := nadController.nads[primaryNAD]
		if nadNetworkName != networkName {
			continue
		}
		namespaces = append(namespaces, namespaceName)
	}
	return namespaces, nil
}

// DoWithLock iterates over all role primary user defined networks and executes the given fn with each network as input.
// An error will not block execution and instead all errors will be aggregated and returned when all networks are processed.
func (nadController *NetAttachDefinitionController) DoWithLock(f func(network util.NetInfo) error) error {
	if !util.IsNetworkSegmentationSupportEnabled() {
		defaultNetwork := &util.DefaultNetInfo{}
		return f(defaultNetwork)
	}
	nadController.RLock()
	defer nadController.RUnlock()

	var errs []error
	for _, primaryNAD := range nadController.primaryNADs {
		if primaryNAD == "" {
			continue
		}
		netName := nadController.nads[primaryNAD]
		if netName == "" {
			// this should never happen where we have a nad keyed in the primaryNADs
			// map, but it doesn't exist in the nads map
			panic("NAD Controller broken consistency between primary NADs and cached NADs")
		}
		network := nadController.networkManager.getNetwork(netName)
		n := util.CopyNetInfo(network)
		// update the returned netInfo copy to only have the primary NAD for this namespace
		n.SetNADs(primaryNAD)
		if err := f(n); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}
