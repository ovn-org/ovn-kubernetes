package udnenabledsvc

import (
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	ktypes "k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
)

const (
	addressSetName = "udn-enabled-svc-cluster-ips"
	controllerName = "udn-enabled-svc"
)

// Controller watches services as defined with binary argument flag --udn-allowed-default-services. It gathers all the clusterIPs
// from the services defined and updates an address_set with the total set. This address set maybe consumed. It will never be deleted even
// if UDN is disabled because consumers may still reference it for a period of time.
type Controller struct {
	// libovsdb northbound client interface
	nbClient libovsdbclient.Client
	// An address set factory that creates address sets
	addressSetFactory addressset.AddressSetFactory
	addressSetMu      *sync.Mutex
	addressSet        addressset.AddressSet // stores selected services cluster IPs
	serviceInformer   coreinformers.ServiceInformer
	queue             workqueue.TypedRateLimitingInterface[string]
	// Exposes UDN enabled service VIPs. Key = namespaced name of service and value represents a string list of clusterIPs.
	cache map[string][]string
	// services represents the desired list of namespaced name services that the controller will watch and consume all clusterIPs.
	// If the service isn't found or doesn't contain at least one clusterIP, then its not added to the cache.
	// services is set once and then should only be read only for the lifetime of the controller.
	services sets.Set[string]
}

// NewController creates a new controller to sync UDN enabled services clusterIPs with an OVN address set. Address set is
// never deleted and maybe read-only referenced by consumers. Single worker only for processing events.
func NewController(nbClient libovsdbclient.Client, addressSetFactory addressset.AddressSetFactory,
	serviceInformer coreinformers.ServiceInformer, services []string) *Controller {

	serviceSet := sets.New[string]()
	serviceSet.Insert(services...)

	return &Controller{
		nbClient:          nbClient,
		addressSetFactory: addressSetFactory,
		addressSetMu:      &sync.Mutex{},
		serviceInformer:   serviceInformer,
		queue: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.NewTypedItemFastSlowRateLimiter[string](time.Second, 5*time.Second, 5),
			workqueue.TypedRateLimitingQueueConfig[string]{Name: "udnenabledservice"},
		),
		cache:    make(map[string][]string),
		services: serviceSet,
	}
}

// Run adds event handlers and starts a single worker to copy all UDN enabled services VIPs to an OVN address set and expose that
// address set to consumers. It will block until stop channel is closed.
func (c *Controller) Run(stopCh <-chan struct{}) error {
	defer c.queue.ShutDown()
	// ensure service informer is sync'd
	klog.Info("Waiting for service informer to sync")
	if ok := cache.WaitForCacheSync(stopCh, c.serviceInformer.Informer().HasSynced); !ok {
		return nil
	}
	handler, err := c.serviceInformer.Informer().AddEventHandler(factory.WithUpdateHandlingForObjReplace(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.onServiceAdd,
		UpdateFunc: c.onServiceUpdate,
		DeleteFunc: c.onServiceDelete,
	}))
	if err != nil {
		return fmt.Errorf("failed to add event handler: %v", err)
	}
	if err = c.ensureAddressSet(); err != nil {
		return fmt.Errorf("failed to ensure UDN enabled services address set: %v", err)
	}
	klog.Info("Performing full resync")
	if err = c.fullResync(); err != nil {
		return fmt.Errorf("failed to run UDN enabled services controller because repairing failed: %v", err)
	}
	klog.Info("Waiting for handler to sync")
	if ok := cache.WaitForCacheSync(stopCh, handler.HasSynced); !ok {
		return nil
	}
	defer klog.Info("UDN enabled services controller ended")
	klog.Info("Starting worker")
	go wait.Until(c.worker, time.Second, stopCh)
	<-stopCh
	return nil
}

// GetAddressSetDBIDs returns the DB IDs of the address set managed by this controller - an address set for each IP family enabled.
func GetAddressSetDBIDs() *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.AddressSetUDNEnabledService, controllerName, map[libovsdbops.ExternalIDKey]string{
		libovsdbops.ObjectNameKey: addressSetName,
	})
}

func (c *Controller) IsAddressSetAvailable() bool {
	c.addressSetMu.Lock()
	defer c.addressSetMu.Unlock()
	return c.addressSet != nil
}

// getAddressSetHashNames returns the address set hash name. Address set will persist if created and will never be removed
func (c *Controller) getAddressSetHashNames() (string, string) {
	c.addressSetMu.Lock()
	defer c.addressSetMu.Unlock()
	if c.addressSet == nil {
		panic("Run() must be called before attempting to get address set hash names")
	}
	return c.addressSet.GetASHashNames()
}

func (c *Controller) ensureAddressSet() error {
	var err error
	c.addressSetMu.Lock()
	c.addressSet, err = c.addressSetFactory.EnsureAddressSet(GetAddressSetDBIDs())
	c.addressSetMu.Unlock()
	return err
}

func (c *Controller) addToAddressSet(addresses ...string) error {
	c.addressSetMu.Lock()
	defer c.addressSetMu.Unlock()
	return c.addressSet.AddAddresses(addresses)
}

func (c *Controller) deleteFromAddressSet(addresses ...string) error {
	c.addressSetMu.Lock()
	defer c.addressSetMu.Unlock()
	return c.addressSet.DeleteAddresses(addresses)
}

func (c *Controller) fullResync() error {
	services, err := c.serviceInformer.Lister().List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list all services: %v", err)
	}
	var allClusterIPs []string
	for _, service := range services {
		namespacedName := ktypes.NamespacedName{Namespace: service.Namespace, Name: service.Name}.String()
		if !util.IsUDNEnabledService(namespacedName) {
			continue
		}
		if len(service.Spec.ClusterIPs) == 0 {
			continue
		}
		allClusterIPs = append(allClusterIPs, service.Spec.ClusterIPs...)
		clusterIPs := make([]string, 0, len(service.Spec.ClusterIPs))
		copy(clusterIPs, service.Spec.ClusterIPs)
		c.cache[namespacedName] = clusterIPs
	}
	return c.addressSet.SetAddresses(allClusterIPs)
}

func (c *Controller) worker() {
	for c.processNextWorkItem() {
	}
}

func (c *Controller) processNextWorkItem() bool {
	key, done := c.queue.Get()
	if done {
		return false
	}
	defer c.queue.Done(key)
	err := c.syncService(key)
	c.handleErr(err, key)
	return true
}

func (c *Controller) handleErr(err error, key interface{}) {
	keyStr := key.(string)
	if err == nil {
		c.queue.Forget(keyStr)
		return
	}
	if c.queue.NumRequeues(keyStr) < 15 {
		klog.V(2).InfoS("Error syncing UDN enabled service %s, retrying: %v", key, err)
		c.queue.AddRateLimited(keyStr)
		return
	}
	klog.Warningf("Dropping UDN enabled service %s out of the queue: %v", key, err)
	c.queue.Forget(keyStr)
	utilruntime.HandleError(err)
}

func (c *Controller) syncService(key string) error {
	// we don't support cleanup of address set if configuration for UDN enabled services changes during run time. Sync will handle
	// full (re)sync during startup.
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return fmt.Errorf("failed to split meta data for service from key %q: %v", key, err)
	}
	service, err := c.serviceInformer.Lister().Services(namespace).Get(name)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to list service %s/%s: %v", namespace, name, err)
	}
	cachedClusterIPs, ok := c.cache[key]
	if !ok {
		cachedClusterIPs = make([]string, 0, len(service.Spec.ClusterIPs))
	}
	// service delete case
	if service == nil {
		if !ok {
			return nil
		}
		if err = c.deleteFromAddressSet(cachedClusterIPs...); err != nil {
			return fmt.Errorf("failed to delete clusterIP(s) from address set for service %s: %v", key, err)
		}
		delete(c.cache, key)
		return nil
	}
	// service add or update case
	// cached IPs should be in the same order as what is specified on the .spec.clusterIPs
	if slices.Equal(service.Spec.ClusterIPs, cachedClusterIPs) {
		return nil
	}
	// delete IPs which are present in cache but not in service spec clusterIPs
	ipsToDel := make([]string, 0)
	for _, cachedIP := range cachedClusterIPs {
		if slices.Contains(service.Spec.ClusterIPs, cachedIP) {
			continue
		}
		ipsToDel = append(ipsToDel, cachedIP)
	}
	if err = c.deleteFromAddressSet(ipsToDel...); err != nil {
		return fmt.Errorf("failed to add/update address set clusterIP(s) for service %s: %v", key, err)
	}
	// ensure any new IPs are added
	ipsToAdd := make([]string, 0)
	for _, wantedIP := range service.Spec.ClusterIPs {
		if slices.Contains(cachedClusterIPs, wantedIP) {
			continue
		}
		ipsToAdd = append(ipsToAdd, wantedIP)
	}
	if err = c.addToAddressSet(ipsToAdd...); err != nil {
		return fmt.Errorf("failed to add/update address set clusterIP(s) for service %s: %v", key, err)
	}
	cachedClusterIPs = make([]string, 0, len(service.Spec.ClusterIPs))
	cachedClusterIPs = append(cachedClusterIPs, service.Spec.ClusterIPs...)
	c.cache[key] = cachedClusterIPs
	return nil
}

func (c *Controller) onServiceAdd(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("service add event: couldn't get key for object %+v: %v", obj, err))
		return
	}
	if !c.services.Has(key) {
		return
	}
	c.queue.AddRateLimited(key)
}

func (c *Controller) onServiceUpdate(_, newObj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(newObj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("service update event: couldn't get key for object %+v: %v", newObj, err))
		return
	}
	if !c.services.Has(key) {
		return
	}
	c.queue.AddRateLimited(key)
}

func (c *Controller) onServiceDelete(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("service delete event: couldn't get key for object %+v: %v", obj, err))
		return
	}
	if !c.services.Has(key) {
		return
	}
	c.queue.AddRateLimited(key)
}
