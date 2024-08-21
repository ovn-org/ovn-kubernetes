package dnsnameresolver

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"time"

	ocpnetworkapiv1alpha1 "github.com/openshift/api/network/v1alpha1"
	ocpnetworkclientset "github.com/openshift/client-go/network/clientset/versioned"
	ocpnetworklisterv1alpha1 "github.com/openshift/client-go/network/listers/network/v1alpha1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/controller"
	egressfirewall "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1"
	egressfirewalllister "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/listers/egressfirewall/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
)

// Controller holds the information of the DNS names and the corresponding
// DNSNameResolver objects. The controller maintains the DNSNameResolver
// objects in the cluster.
type Controller struct {
	lock             sync.Mutex
	ocpNetworkClient ocpnetworkclientset.Interface

	// controller for egress firewall
	efController controller.Controller
	// Lister for egress firewall
	efLister egressfirewalllister.EgressFirewallLister
	// controller for dns name resolver
	dnsController controller.Controller
	// Lister for dns name resolver
	dnsLister ocpnetworklisterv1alpha1.DNSNameResolverLister

	resInfo *resolverInfo
}

// NewController returns an instance of the Controller. The level-driven controller is also
// initialized before returning the Controller intance.
func NewController(ovnClient *util.OVNClusterManagerClientset, wf *factory.WatchFactory) *Controller {
	c := &Controller{
		ocpNetworkClient: ovnClient.OCPNetworkClient,
		resInfo:          newResolverInfo(ovnClient.OCPNetworkClient),
	}
	c.initControllers(wf)
	return c
}

// initControllers initializes the controllers for the different resource
// types related to DNSNameResolver.
func (c *Controller) initControllers(watchFactory *factory.WatchFactory) {
	efSharedIndexInformer := watchFactory.EgressFirewallInformer().Informer()
	c.efLister = watchFactory.EgressFirewallInformer().Lister()
	efConfig := &controller.ControllerConfig[egressfirewall.EgressFirewall]{
		RateLimiter:    workqueue.NewTypedItemFastSlowRateLimiter[string](time.Second, 5*time.Second, 5),
		Informer:       efSharedIndexInformer,
		Lister:         c.efLister.List,
		ObjNeedsUpdate: efNeedsUpdate,
		Reconcile:      c.reconcileEgressFirewall,
		Threadiness:    1,
	}
	c.efController = controller.NewController[egressfirewall.EgressFirewall]("cm-ef-controller", efConfig)

	dnsSharedIndexInformer := watchFactory.DNSNameResolverInformer().Informer()
	c.dnsLister = ocpnetworklisterv1alpha1.NewDNSNameResolverLister(dnsSharedIndexInformer.GetIndexer())
	dnsConfig := &controller.ControllerConfig[ocpnetworkapiv1alpha1.DNSNameResolver]{
		RateLimiter:    workqueue.NewTypedItemFastSlowRateLimiter[string](time.Second, 5*time.Second, 5),
		Informer:       dnsSharedIndexInformer,
		Lister:         c.dnsLister.List,
		ObjNeedsUpdate: dnsNeedsUpdate,
		Reconcile:      c.reconcileDNSNameResolver,
		Threadiness:    1,
	}
	c.dnsController = controller.NewController[ocpnetworkapiv1alpha1.DNSNameResolver]("cm-dns-controller", dnsConfig)
}

// efNeedsUpdate returns true if an egress firewall object is either added
// or deleted. If an egress firewall is updated, then efNeedsUpdate returns
// true if the .spec of the object is modified.
func efNeedsUpdate(oldObj, newObj *egressfirewall.EgressFirewall) bool {
	if oldObj == nil || newObj == nil {
		return true
	}
	return !reflect.DeepEqual(oldObj.Spec, newObj.Spec)
}

// dnsNeedsUpdate returns true if a dns name resolver object is either added
// or deleted. The spec of a dns name resolver object is immutable. If the
// status of a dns name resolver is updated, then dnsNeedsUpdate returns
// false.
func dnsNeedsUpdate(oldObj, newObj *ocpnetworkapiv1alpha1.DNSNameResolver) bool {
	if oldObj == nil && newObj != nil || oldObj != nil && newObj == nil {
		return true
	}
	return false
}

// Start initializes the handlers for EgressFirewall and DNSNameResolver
// by watching the corresponding resource types.
func (c *Controller) Start() error {
	if err := controller.StartWithInitialSync(c.syncDNSNames, c.efController, c.dnsController); err != nil {
		return fmt.Errorf("unable to start egress firewall and dns name resolver controllers %w", err)
	}
	return nil
}

// Stop gracefully stops the controller. The handlers for EgressFirewall
// and DNSNameResolver are removed.
func (c *Controller) Stop() {
	controller.Stop(c.efController, c.dnsController)
}

// syncDNSNames syncs the existing EgressFirewall and DNSNameResolver objects
// after a restart and updates the dnsNameObj and objDNSName maps. After the
// sync these two maps will only contain the details of the DNS names which
// are used in at least one namespace.
func (c *Controller) syncDNSNames() error {
	c.lock.Lock()
	defer c.lock.Unlock()

	// Fetch the existing DNSNameResolver objects.
	dnsNameResolvers, err := c.dnsLister.DNSNameResolvers(config.Kubernetes.OVNConfigNamespace).List(labels.Everything())
	if err != nil {
		return fmt.Errorf("syncDNSNames unable to get Egress Firewalls: %w", err)
	}

	dnsNameToResolver := make(map[string]string)
	for _, dnsNameResolver := range dnsNameResolvers {
		dnsNameToResolver[string(dnsNameResolver.Spec.Name)] = dnsNameResolver.Name
	}

	// Fetch the existing EgressFirewall objects.
	egressFirewalls, err := c.efLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("syncDNSNames unable to get Egress Firewalls: %w", err)
	}

	namespaceToDNSNames := make(map[string][]string)
	for _, egressFirewall := range egressFirewalls {
		namespaceToDNSNames[egressFirewall.Namespace] = util.GetDNSNames(egressFirewall)
	}

	c.resInfo.SyncResolverInfo(dnsNameToResolver, namespaceToDNSNames)

	return nil
}

// reconcileEgressFirewall reconciles an EgressFirewall object.
func (c *Controller) reconcileEgressFirewall(key string) error {
	c.lock.Lock()
	defer c.lock.Unlock()
	// Split the key in namespace and name of the corresponding object.
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		klog.Errorf("reconcileEgressFirewall failed to split meta namespace cache key %s for egress firewall: %v", key, err)
		return nil
	}
	// Fetch the egress firewall object using the name and namespace.
	ef, err := c.efLister.EgressFirewalls(namespace).Get(name)
	if err != nil {
		if kerrors.IsNotFound(err) {
			// EgressFirewall object was deleted. Delete all the DNSNameResolver
			// objects corresponding to the DNS names used in the EgressFirewall
			// object.
			return c.resInfo.DeleteDNSNamesForNamespace(namespace)
		}
		return fmt.Errorf("failed to fetch egress firewall %s in namespace %s", name, namespace)
	}

	// EgressFirewall object was added/updated. Check the DNS names which are
	// newly added and create the corresponding DNSNameResolver objects. Also
	// check the DNS names which are deleted and delete the corresponding
	// DNSNameResolver objects.
	return c.resInfo.ModifyDNSNamesForNamespace(util.GetDNSNames(ef), namespace)
}

// reconcileDNSNameResolver reconciles a DNSNameResolver object. If an object
// was deleted, but it was not supposed to, then it is recreated. If an object
// is created, but it was not supposed to, then it is deleted.
func (c *Controller) reconcileDNSNameResolver(key string) error {
	c.lock.Lock()
	defer c.lock.Unlock()
	// Split the key in namespace and name of the corresponding object.
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		klog.Errorf("reconcileDNSNameResolver failed to split meta namespace cache key %s for dns name resolver: %v", key, err)
		return nil
	}
	// Fetch the dns name resolver object using the name and namespace.
	resolverObj, err := c.dnsLister.DNSNameResolvers(namespace).Get(name)
	if err != nil {
		if kerrors.IsNotFound(err) {
			// DNSNameResolver object was deleted. If it is not supposed to be deleted,
			// recreate it.

			// Check if the DNSNameResolver object should exist. If so, then get the
			// corresponding DNS name.
			dnsName, found := c.resInfo.GetDNSNameForResolver(name)
			if !found {
				return nil
			}

			// Recreate the DNSNameResolver object.
			klog.Warningf("Recreating deleted dns name resolver object %s for dns name %s", name, dnsName)
			return createDNSNameResolver(c.ocpNetworkClient, name, dnsName)

		}
		return fmt.Errorf("reconcileDNSNameResolver failed to fetch dns name resolver %s in namespace %s", name, namespace)
	}

	// DNSNameResolver object was added/updated. If it is not supposed to exist,
	// delete the object.

	// Check if the DNSNameResolver object matches the DNS name.
	dnsName := string(resolverObj.Spec.Name)
	if !c.resInfo.IsDNSNameMatchingResolverName(dnsName, resolverObj.Name) {
		// Delete the DNSNameResolver object.
		klog.Warningf("Deleting additional dns name resolver object %s for dns name %s", resolverObj.Name, dnsName)
		return deleteDNSNameResolver(c.ocpNetworkClient, resolverObj.Name)
	}
	return nil
}

// createDNSNameResolver creates a DNSNameResolver object for the DNS name
// and adds the status, if any, to the object. The error, if any, encountered
// during the object creation is returned.
func createDNSNameResolver(ocpNetworkClient ocpnetworkclientset.Interface, objName, dnsName string) error {
	dnsNameResolverObj := &ocpnetworkapiv1alpha1.DNSNameResolver{
		ObjectMeta: metav1.ObjectMeta{
			Name:      objName,
			Namespace: config.Kubernetes.OVNConfigNamespace,
		},
		Spec: ocpnetworkapiv1alpha1.DNSNameResolverSpec{
			Name: ocpnetworkapiv1alpha1.DNSName(dnsName),
		},
	}
	_, err := ocpNetworkClient.NetworkV1alpha1().DNSNameResolvers(config.Kubernetes.OVNConfigNamespace).
		Create(context.TODO(), dnsNameResolverObj, metav1.CreateOptions{})

	return err

}

// deleteDNSNameResolver deletes a DNSNameResolver object and if an error
// is encountered, which is not IsNotFound, then it is returned.
func deleteDNSNameResolver(ocpNetworkClient ocpnetworkclientset.Interface, objName string) error {
	err := ocpNetworkClient.NetworkV1alpha1().DNSNameResolvers(config.Kubernetes.OVNConfigNamespace).
		Delete(context.TODO(), objName, metav1.DeleteOptions{})
	if err != nil && !kerrors.IsNotFound(err) {
		return err
	}
	return nil
}
