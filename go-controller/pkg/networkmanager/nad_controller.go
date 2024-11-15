package networkmanager

import (
	"fmt"
	"reflect"
	"strconv"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	coreinformers "k8s.io/client-go/informers/core/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	nettypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	nadclientset "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned"
	nadinformers "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/informers/externalversions/k8s.cni.cncf.io/v1"
	nadlisters "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/listers/k8s.cni.cncf.io/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/allocator/id"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/controller"
	rainformers "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1/apis/informers/externalversions/routeadvertisements/v1"
	userdefinednetworkinformer "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/informers/externalversions/userdefinednetwork/v1"
	userdefinednetworklister "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/listers/userdefinednetwork/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	utiludn "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/udn"
)

type watchFactory interface {
	NADInformer() nadinformers.NetworkAttachmentDefinitionInformer
	UserDefinedNetworkInformer() userdefinednetworkinformer.UserDefinedNetworkInformer
	ClusterUserDefinedNetworkInformer() userdefinednetworkinformer.ClusterUserDefinedNetworkInformer
	NamespaceInformer() coreinformers.NamespaceInformer
	RouteAdvertisementsInformer() rainformers.RouteAdvertisementsInformer
	NodeCoreInformer() coreinformers.NodeInformer
}

// nadController handles namespaced scoped NAD events and
// manages cluster scoped networks defined in those NADs. NADs are mostly
// referenced from pods to give them access to the network. Different NADs can
// define the same network as long as those definitions are actually equal.
// Unexpected situations are handled on best effort basis but improper NAD
// administration can lead to undefined behavior if referenced by running pods.
type nadController struct {
	sync.RWMutex

	name            string
	nadLister       nadlisters.NetworkAttachmentDefinitionLister
	udnLister       userdefinednetworklister.UserDefinedNetworkLister
	cudnLister      userdefinednetworklister.ClusterUserDefinedNetworkLister
	namespaceLister corelisters.NamespaceLister
	nodeLister      corelisters.NodeLister

	controller controller.Controller
	recorder   record.EventRecorder

	// networkController reconciles network specific controllers
	networkController *networkController

	// nads to network mapping
	nads map[string]string

	// primaryNADs holds a mapping of namespace to primary NAD names
	primaryNADs map[string]string

	networkIDAllocator id.Allocator
	nadClient          nadclientset.Interface
}

func newController(
	name string,
	zone string,
	node string,
	cm ControllerManager,
	wf watchFactory,
	ovnClient *util.OVNClusterManagerClientset,
	recorder record.EventRecorder,
) (*nadController, error) {
	c := &nadController{
		name:              fmt.Sprintf("[%s NAD controller]", name),
		recorder:          recorder,
		nadLister:         wf.NADInformer().Lister(),
		nodeLister:        wf.NodeCoreInformer().Lister(),
		networkController: newNetworkController(name, zone, node, cm, wf),
		nads:              map[string]string{},
		primaryNADs:       map[string]string{},
	}

	if ovnClient != nil {
		c.nadClient = ovnClient.NetworkAttchDefClient
	}

	// prepare the network ID allocator if this is cluster manager
	var err error
	if zone == "" && node == "" {
		c.networkIDAllocator, err = id.NewIDAllocator("NetworkIDs", MaxNetworks)
		if err != nil {
			return nil, fmt.Errorf("failed to create network ID allocator: %w", err)
		}

		// Reserve the ID of the default network
		if err := c.networkIDAllocator.ReserveID(types.DefaultNetworkName, DefaultNetworkID); err != nil {
			return nil, fmt.Errorf("failed to allocate default network ID: %w", err)
		}
	}

	config := &controller.ControllerConfig[nettypes.NetworkAttachmentDefinition]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Informer:       wf.NADInformer().Informer(),
		Lister:         c.nadLister.List,
		Reconcile:      c.sync,
		ObjNeedsUpdate: c.nadNeedsUpdate,
		Threadiness:    1,
	}

	if util.IsNetworkSegmentationSupportEnabled() {
		if udnInformer := wf.UserDefinedNetworkInformer(); udnInformer != nil {
			c.udnLister = udnInformer.Lister()
		}
		if cudnInformer := wf.ClusterUserDefinedNetworkInformer(); cudnInformer != nil {
			c.cudnLister = cudnInformer.Lister()
		}
		if nsInformer := wf.NamespaceInformer(); nsInformer != nil {
			c.namespaceLister = nsInformer.Lister()
		}
	}
	c.controller = controller.NewController(
		c.name,
		config,
	)

	return c, nil
}

func (c *nadController) Interface() Interface {
	return c
}

func (c *nadController) Start() error {
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

	err = c.networkController.Start()
	if err != nil {
		return err
	}

	klog.Infof("%s: started", c.name)
	return nil
}

func (c *nadController) Stop() {
	klog.Infof("%s: shutting down", c.name)
	controller.Stop(c.controller)
	c.networkController.Stop()
}

func (c *nadController) syncAll() (err error) {
	existingNADs, err := c.nadLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("%s: failed to list NADs: %w", c.name, err)
	}

	syncNAD := func(nad *nettypes.NetworkAttachmentDefinition) error {
		key, err := cache.MetaNamespaceKeyFunc(nad)
		if err != nil {
			klog.Errorf("%s: failed to sync %v: %v", c.name, nad, err)
		}
		err = c.syncNAD(key, nad)
		if err != nil {
			return fmt.Errorf("%s: failed to sync %s: %v", c.name, key, err)
		}
		return nil
	}

	// create all networks with their updated list of NADs and only then start
	// the corresponding controllers so as to avoid to the extent possible the
	// errors and retries that would result if the controller attempted to
	// process pods attached with NADs we wouldn't otherwise know about yet
	nadsWithoutID := []*nettypes.NetworkAttachmentDefinition{}
	for _, nad := range existingNADs {
		// skip NADs that are not annotated with an ID
		if c.networkIDAllocator != nil && nad.Annotations[types.OvnNetworkIDAnnotation] == "" {
			nadsWithoutID = append(nadsWithoutID, nad)
			continue
		}
		err := syncNAD(nad)
		if err != nil {
			return err
		}
	}

	if len(nadsWithoutID) == 0 {
		return nil
	}

	// If we are missing IDs, get them from the nodes which is where we
	// originally had them
	klog.V(5).Infof("%s: %d NADs are missing the network ID annotation, fetching from nodes", c.name, len(nadsWithoutID))
	nodes, err := c.nodeLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("error listing nodes: %w", err)
	}
	for _, n := range nodes {
		networkIdsMap, err := util.GetNodeNetworkIDsAnnotationNetworkIDs(n)
		if err == nil {
			for networkName, id := range networkIdsMap {
				// Reserve the id for the network name. We can safely
				// ignore any errors if there are duplicate ids or if
				// two networks have the same id. We will resync the node
				// annotations correctly when the network controller
				// is created.
				_ = c.networkIDAllocator.ReserveID(networkName, id)
			}
		}
	}

	// finally process the pending NADs
	for _, nad := range existingNADs {
		err := syncNAD(nad)
		if err != nil {
			return err
		}
	}

	return nil
}

func (c *nadController) sync(key string) error {
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

	nad, err := c.nadLister.NetworkAttachmentDefinitions(namespace).Get(name)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}

	return c.syncNAD(key, nad)
}

func (c *nadController) syncNAD(key string, nad *nettypes.NetworkAttachmentDefinition) error {
	var nadNetworkName string
	var nadNetwork util.NetInfo
	var oldNetwork, ensureNetwork util.MutableNetInfo
	var err error

	namespace, _, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return fmt.Errorf("%s: failed splitting key %s: %v", c.name, key, err)
	}

	if nad != nil {
		nadNetwork, err = util.ParseNADInfo(nad)
		if err != nil {
			if c.recorder != nil {
				c.recorder.Eventf(&corev1.ObjectReference{Kind: nad.Kind, Namespace: nad.Namespace, Name: nad.Name}, corev1.EventTypeWarning,
					"InvalidConfig", "Failed to parse network config: %v", err.Error())
			}
			klog.Errorf("%s: failed parsing NAD %s: %v", c.name, key, err)
			return nil
		}
		nadNetworkName = nadNetwork.GetNetworkName()
	}

	c.Lock()
	defer c.Unlock()
	// We can only have one primary NAD per namespace
	primaryNAD := c.primaryNADs[namespace]
	if nadNetwork != nil && nadNetwork.IsPrimaryNetwork() && primaryNAD != "" && primaryNAD != key {
		return fmt.Errorf("%s: NAD %s is primary for the namespace, NAD %s can't be primary", c.name, primaryNAD, key)
	}

	// As multiple NADs may define networks with the same name, these networks
	// should also have the same config to be considered compatible. If an
	// incompatible network change happens on NAD update, we can:
	// - Re-create network with the same name but different config, if it is not
	//   referred to by any other NAD
	// - Return an error AND clean up NAD from the old network

	// the NAD refers to a different network than before
	if nadNetworkName != c.nads[key] {
		oldNetwork = c.networkController.getNetwork(c.nads[key])
	}

	currentNetwork := c.networkController.getNetwork(nadNetworkName)

	switch {
	case currentNetwork == nil:
		// the NAD refers to a new network, ensure it
		ensureNetwork = util.NewMutableNetInfo(nadNetwork)
	case util.AreNetworksCompatible(currentNetwork, nadNetwork):
		// the NAD refers to an existing compatible network, ensure that
		// existing network holds a reference to this NAD
		ensureNetwork = currentNetwork
	case sets.New(key).HasAll(currentNetwork.GetNADs()...):
		// the NAD is the only NAD referring to an existing incompatible
		// network, remove the reference from the old network and ensure that
		// existing network holds a reference to this NAD
		oldNetwork = currentNetwork
		ensureNetwork = util.NewMutableNetInfo(nadNetwork)
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
	var releaseNetwork util.NetInfo
	if oldNetwork != nil {
		oldNetworkName := oldNetwork.GetNetworkName()
		oldNetwork.DeleteNADs(key)
		if len(oldNetwork.GetNADs()) == 0 {
			c.networkController.DeleteNetwork(oldNetworkName)
			releaseNetwork = oldNetwork
		} else {
			c.networkController.EnsureNetwork(oldNetwork)
			// we have nothing else to do with oldNetwork so reset to nil
			oldNetwork = nil
		}
	}

	if err := c.handleNetworkID(releaseNetwork, ensureNetwork, nad); err != nil {
		return err
	}

	// this was a nad delete
	if ensureNetwork == nil {
		delete(c.nads, key)
		if c.primaryNADs[namespace] == key {
			delete(c.primaryNADs, namespace)
		}
		return err
	}

	if ensureNetwork.GetNetworkID() == DefaultNetworkID && nadNetworkName != types.DefaultNetworkName {
		// need to wait until cluster manager nad controller allocates an ID for
		// the network
		klog.V(4).Infof("%s: will wait for cluster manager to allocate an ID before ensuring network %s", c.name, nadNetworkName)
		return nil
	}

	// ensure the network is associated with the NAD
	ensureNetwork.AddNADs(key)
	c.nads[key] = ensureNetwork.GetNetworkName()
	// track primary NAD
	switch {
	case ensureNetwork.IsPrimaryNetwork():
		c.primaryNADs[namespace] = key
	default:
		if c.primaryNADs[namespace] == key {
			delete(c.primaryNADs, namespace)
		}
	}

	// reconcile the network
	c.networkController.EnsureNetwork(ensureNetwork)
	return nil
}

// isOwnUpdate checks if an object was updated by us last, as indicated by its
// managed fields. Used to avoid reconciling an update that we made ourselves.
func isOwnUpdate(manager string, managedFields []metav1.ManagedFieldsEntry) bool {
	return util.IsLastUpdatedByManager(manager, managedFields)
}

func (c *nadController) nadNeedsUpdate(oldNAD, newNAD *nettypes.NetworkAttachmentDefinition) bool {
	if oldNAD == nil || newNAD == nil {
		return true
	}

	// don't process resync or objects that are marked for deletion
	if oldNAD.ResourceVersion == newNAD.ResourceVersion ||
		!newNAD.GetDeletionTimestamp().IsZero() {
		return false
	}

	if isOwnUpdate(c.name, newNAD.ManagedFields) {
		return false
	}

	// also reconcile the network in case its route advertisements changed
	return !reflect.DeepEqual(oldNAD.Spec, newNAD.Spec) ||
		oldNAD.Annotations[types.OvnRouteAdvertisementsKey] != newNAD.Annotations[types.OvnRouteAdvertisementsKey]
}

func (c *nadController) GetActiveNetworkForNamespace(namespace string) (util.NetInfo, error) {
	nad, network := c.getActiveNetworkForNamespace(namespace)
	if nad == "" {
		// default network, just return
		return network, nil
	}
	if network != nil {
		// primary network
		copy := util.NewMutableNetInfo(network)
		copy.SetNADs(nad)
		return copy, nil
	}

	// no primary network found, make sure we just haven't processed it yet and no UDN / CUDN exists
	udns, err := c.udnLister.UserDefinedNetworks(namespace).List(labels.Everything())
	if err != nil {
		return nil, fmt.Errorf("error getting user defined networks: %w", err)
	}
	for _, udn := range udns {
		if utiludn.IsPrimaryNetwork(&udn.Spec) {
			return nil, util.NewUnprocessedActiveNetworkError(namespace, udn.Name)
		}
	}
	cudns, err := c.cudnLister.List(labels.Everything())
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
		selectedNamespaces, err := c.namespaceLister.List(cudnNamespaceSelector)
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

func (c *nadController) GetActiveNetworkForNamespaceFast(namespace string) util.NetInfo {
	_, network := c.getActiveNetworkForNamespace(namespace)
	return network
}

func (c *nadController) getActiveNetworkForNamespace(namespace string) (string, util.NetInfo) {
	c.RLock()
	defer c.RUnlock()

	var network util.NetInfo
	primaryNAD := c.primaryNADs[namespace]
	switch primaryNAD {
	case "":
		// default network
		network = c.networkController.getNetwork(types.DefaultNetworkName)
		if network == nil {
			network = &util.DefaultNetInfo{}
		}
	default:
		// we have a primary netwrok
		netName := c.nads[primaryNAD]
		if netName == "" {
			// this should never happen where we have a nad keyed in the primaryNADs
			// map, but it doesn't exist in the nads map
			panic("NAD Controller broken consistency between primary NADs and cached NADs")
		}
		network = c.networkController.getNetwork(netName)
	}

	return primaryNAD, network
}

func (c *nadController) GetNetwork(name string) util.NetInfo {
	return c.networkController.getNetwork(name)
}

// handleNetworkID releases, allocates and annotates network IDs. If both old
// and new networks are provided, it is assumed they are a different network
// with the same name and will be allocated the same ID.
func (c *nadController) handleNetworkID(old util.NetInfo, new util.MutableNetInfo, nad *nettypes.NetworkAttachmentDefinition) error {
	if new != nil && new.GetNetworkName() == types.DefaultNetworkName {
		return nil
	}

	var err error
	var id int

	// check what ID is currently annotated
	if nad != nil && nad.Annotations[types.OvnNetworkIDAnnotation] != "" {
		annotated := nad.Annotations[types.OvnNetworkIDAnnotation]
		id, err = strconv.Atoi(annotated)
		if err != nil {
			return fmt.Errorf("failed to parse annotated network ID: %w", err)
		}
	}

	// this is not the cluster manager nad controller and we are not allocating
	// so just return what we got from the annotation
	if c.networkIDAllocator == nil {
		if new != nil {
			new.SetNetworkID(id)
		}
		return nil
	}

	// release old ID
	if old != nil && new == nil {
		c.networkIDAllocator.ReleaseID(old.GetNetworkName())
		return nil
	}

	// nothing to allocate
	if new == nil {
		return nil
	}
	name := new.GetNetworkName()

	// an ID was annotated, check if it is free to use or stale
	if id != 0 {
		err = c.networkIDAllocator.ReserveID(name, id)
		if err != nil {
			// already reserved for a different network, allocate a new id
			id = 0
		}
	}

	// we don't have an ID, allocate a new one
	if id == 0 {
		id, err = c.networkIDAllocator.AllocateID(new.GetNetworkName())
		if err != nil {
			return fmt.Errorf("failed to allocate network ID: %w", err)
		}
	}

	// set and annotate the network ID
	new.SetNetworkID(id)
	annotations := map[string]string{
		types.OvnNetworkNameAnnotation: name,
		types.OvnNetworkIDAnnotation:   strconv.Itoa(id),
	}
	if nad.Annotations[types.OvnNetworkNameAnnotation] == annotations[types.OvnNetworkNameAnnotation] {
		delete(annotations, types.OvnNetworkNameAnnotation)
	}
	if nad.Annotations[types.OvnNetworkIDAnnotation] == annotations[types.OvnNetworkIDAnnotation] {
		delete(annotations, types.OvnNetworkIDAnnotation)
	}
	if len(annotations) == 0 {
		return nil
	}

	k := kube.KubeOVN{
		NADClient: c.nadClient,
	}

	err = k.SetAnnotationsOnNAD(
		nad.Namespace,
		nad.Name,
		annotations,
		c.name,
	)
	if err != nil {
		return fmt.Errorf("failed to annotate network ID on NAD: %w", err)
	}

	return nil
}
