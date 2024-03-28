package dnsnameresolver

import (
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
	ocpnetworkapiv1alpha1 "github.com/openshift/api/network/v1alpha1"
	ocpnetworklisterv1alpha1 "github.com/openshift/client-go/network/listers/network/v1alpha1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/controller"
	egressfirewall "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1"
	egressfirewalllister "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/listers/egressfirewall/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	hashutil "k8s.io/kubernetes/pkg/util/hash"
)

// Controller holds the information of the DNS names and the corresponding
// DNSNameResolver objects. The controller maintains the DNSNameResolver
// objects in the cluster.
type Controller struct {
	lock sync.Mutex
	kube *kube.KubeOVN

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
	kube := &kube.KubeOVN{
		OCPNetworkClient: ovnClient.OCPNetworkClient,
	}
	c := &Controller{
		kube:    kube,
		resInfo: newResolverInfo(kube),
	}
	c.initControllers(wf)
	return c
}

// initControllers initializes the controllers for the different resource
// types related to DNSNameResolver.
func (c *Controller) initControllers(watchFactory *factory.WatchFactory) {
	efSharedIndexInformer := watchFactory.EgressFirewallInformer().Informer()
	c.efLister = watchFactory.EgressFirewallInformer().Lister()
	efConfig := &controller.Config[egressfirewall.EgressFirewall]{
		RateLimiter:    workqueue.NewItemFastSlowRateLimiter(time.Second, 5*time.Second, 5),
		Informer:       efSharedIndexInformer,
		Lister:         c.efLister.List,
		ObjNeedsUpdate: efNeedsUpdate,
		Reconcile:      c.reconcileEgressFirewall,
	}
	c.efController = controller.NewController[egressfirewall.EgressFirewall]("cm-ef-controller", efConfig)

	dnsSharedIndexInformer := watchFactory.DNSNameResolverInformer().Informer()
	c.dnsLister = ocpnetworklisterv1alpha1.NewDNSNameResolverLister(dnsSharedIndexInformer.GetIndexer())
	dnsConfig := &controller.Config[ocpnetworkapiv1alpha1.DNSNameResolver]{
		RateLimiter:    workqueue.NewItemFastSlowRateLimiter(time.Second, 5*time.Second, 5),
		Informer:       dnsSharedIndexInformer,
		Lister:         c.dnsLister.List,
		ObjNeedsUpdate: dnsNeedsUpdate,
		Reconcile:      c.reconcileDNSNameResolver,
		InitialSync:    c.syncDNSNames,
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
// or deleted. If a dns name resolver is updated, then dnsNeedsUpdate returns
// true if the .status of the object is modified.
func dnsNeedsUpdate(oldObj, newObj *ocpnetworkapiv1alpha1.DNSNameResolver) bool {
	if oldObj == nil || newObj == nil {
		return true
	}
	return !reflect.DeepEqual(oldObj.Status, newObj.Status)
}

// Start initializes the handlers for EgressFirewall and DNSNameResolver
// by watching the corresponding resource types.
func (c *Controller) Start() error {
	if err := c.efController.Start(1); err != nil {
		return fmt.Errorf("unable to start egress firewall controller %w", err)
	}
	if err := c.dnsController.Start(1); err != nil {
		return fmt.Errorf("unable to start dns name resolver controller %w", err)
	}
	return nil
}

// Stop gracefully stops the controller. The handlers for EgressFirewall
// and DNSNameResolver are removed.
func (c *Controller) Stop() {
	c.efController.Stop()
	c.dnsController.Stop()
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
		namespaceToDNSNames[egressFirewall.Namespace] = getDNSNames(egressFirewall)
	}

	c.resInfo.syncResolverInfo(dnsNameToResolver, namespaceToDNSNames)

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
			return c.resInfo.deleteDNSNamesForNamespace(namespace)
		}
		return fmt.Errorf("failed to fetch egress firewall %s in namespace %s", name, namespace)
	}

	// EgressFirewall object was added/updated. Check the DNS names which are
	// newly added and create the corresponding DNSNameResolver objects. Also
	// check the DNS names which are deleted and delete the corresponding
	// DNSNameResolver objects.
	return c.resInfo.modifyDNSNamesForNamespace(getDNSNames(ef), namespace)
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
			dnsName, found := c.resInfo.getDNSNameForResolver(name)
			if !found {
				return nil
			}

			// Recreate the DNSNameResolver object.
			klog.Warningf("Recreating deleted dns name resolver object %s for dns name %s", name, dnsName)
			return createDNSNameResolver(c.kube, name, dnsName)

		}
		return fmt.Errorf("reconcileDNSNameResolver failed to fetch dns name resolver %s in namespace %s", name, namespace)
	}

	// DNSNameResolver object was added/updated. If it is not supposed to exist,
	// delete the object.

	// Check if the DNSNameResolver object matches the DNS name.
	dnsName := string(resolverObj.Spec.Name)
	if !c.resInfo.isDNSNameMatchingResolverName(dnsName, resolverObj.Name) {
		// Delete the DNSNameResolver object.
		klog.Warningf("Deleting additional dns name resolver object %s for dns name %s", resolverObj.Name, dnsName)
		return deleteDNSNameResolver(c.kube, resolverObj.Name)
	}
	return nil
}

// createDNSNameResolver creates a DNSNameResolver object for the DNS name
// and adds the status, if any, to the object. The error, if any, encountered
// during the object creation is returned.
func createDNSNameResolver(kube *kube.KubeOVN, objName, dnsName string) error {
	dnsNameResolverObj := &ocpnetworkapiv1alpha1.DNSNameResolver{
		ObjectMeta: metav1.ObjectMeta{
			Name:      objName,
			Namespace: config.Kubernetes.OVNConfigNamespace,
		},
		Spec: ocpnetworkapiv1alpha1.DNSNameResolverSpec{
			Name: ocpnetworkapiv1alpha1.DNSName(dnsName),
		},
	}
	_, err := kube.CreateDNSNameResolver(dnsNameResolverObj, config.Kubernetes.OVNConfigNamespace)

	return err

}

// deleteDNSNameResolver deletes a DNSNameResolver object and if an error
// is encountered, which is not IsNotFound, then it is returned.
func deleteDNSNameResolver(kube *kube.KubeOVN, objName string) error {
	err := kube.DeleteDNSNameResolver(objName, config.Kubernetes.OVNConfigNamespace)
	if err != nil && !kerrors.IsNotFound(err) {
		return err
	}
	return nil
}

// getDNSNames iterates through the egress firewall rules and returns the DNS
// names present in them after validating the rules.
func getDNSNames(ef *egressfirewall.EgressFirewall) []string {
	var dnsNameSlice []string
	for i, egressFirewallRule := range ef.Spec.Egress {
		if i > types.EgressFirewallStartPriority-types.MinimumReservedEgressFirewallPriority {
			klog.Warningf("egressFirewall for namespace %s has too many rules, the rest will be ignored", ef.Namespace)
			break
		}

		// Validate egress firewall rule destination and get the DNS name
		// if used in the rule.
		_, dnsName, _, _, err := util.ValidateAndGetEgressFirewallDestination(egressFirewallRule.To)
		if err != nil {
			return []string{}
		}

		if dnsName != "" {
			dnsNameSlice = append(dnsNameSlice, strings.ToLower(dns.Fqdn(dnsName)))
		}
	}

	return dnsNameSlice
}

// computeHash returns a hash value calculated from dns name and a collisionCount
// to avoid hash collision. The hash will be safe encoded to avoid bad words.
func computeHash(dnsName string, collisionCount int) string {
	dnsNameHasher := fnv.New32a()
	hashutil.DeepHashObject(dnsNameHasher, dnsName)

	// Add collisionCount in the hash if it is not zero.
	if collisionCount != 0 {
		collisionCountBytes := make([]byte, 8)
		binary.LittleEndian.PutUint32(collisionCountBytes, uint32(collisionCount))
		dnsNameHasher.Write(collisionCountBytes)
	}
	return rand.SafeEncodeString(fmt.Sprint(dnsNameHasher.Sum32()))
}
