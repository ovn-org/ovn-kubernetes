package dnsnameresolver

import (
	"fmt"
	"reflect"
	"sync"
	"time"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	ocpnetworkapiv1alpha1 "github.com/openshift/api/network/v1alpha1"
	ocpnetworklisterv1alpha1 "github.com/openshift/client-go/network/listers/network/v1alpha1"
	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/controller"
	egressfirewalllister "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/listers/egressfirewall/v1"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
)

// ExternalEgressDNS keeps track of DNS names and the corresponding IP addresses.
// For each DNS name, an address set is allocated and the address set is
// kept updated with the corresponding IP addresses. Whenever a DNS name
// is removed the corresponding addresset is destroyed.
type ExternalEgressDNS struct {
	dnsTracker dnsTracker
	// dnsControllerLock protects the operations performed on the maps related to
	// dnsNameResolverController.
	dnsControllerLock sync.Mutex
	// Controller used to handle dns name resolver resoures
	controller controller.Controller
	// Lister for dns name resolver
	dnsLister ocpnetworklisterv1alpha1.DNSNameResolverLister
	// Lister for egress firewall
	efLister egressfirewalllister.EgressFirewallLister
	// maps to hold the details of the DNS names and the corresponding
	// DNSNameResolver object names
	dnsNameToResolver map[string]string
	resolverToDNSName map[string]string
}

var _ DNSNameResolver = &ExternalEgressDNS{}

// NewExternalEgressDNS initializes and returns a new ExternalEgressDNS instance.
func NewExternalEgressDNS(
	addressSetFactory addressset.AddressSetFactory,
	controllerName string,
	ignoreClusterSubnet bool,
	dnsSharedIndexInformer cache.SharedIndexInformer,
	efLister egressfirewalllister.EgressFirewallLister,
) (*ExternalEgressDNS, error) {
	if addressSetFactory == nil {
		return nil, fmt.Errorf("error creating ExternalEgressDNS, addressSetFactory is nil")
	}
	extEgDNS := &ExternalEgressDNS{
		dnsTracker:        newDNSTracker(addressSetFactory, controllerName, ignoreClusterSubnet),
		dnsNameToResolver: make(map[string]string),
		resolverToDNSName: make(map[string]string),
		efLister:          efLister,
	}

	extEgDNS.dnsLister = ocpnetworklisterv1alpha1.NewDNSNameResolverLister(dnsSharedIndexInformer.GetIndexer())
	dnsConfig := &controller.ControllerConfig[ocpnetworkapiv1alpha1.DNSNameResolver]{
		RateLimiter:    workqueue.NewTypedItemFastSlowRateLimiter[string](time.Second, 5*time.Second, 5),
		Informer:       dnsSharedIndexInformer,
		Lister:         extEgDNS.dnsLister.List,
		ObjNeedsUpdate: dnsNeedsUpdate,
		Reconcile:      extEgDNS.reconcileDNSNameResolver,
		Threadiness:    1,
	}

	extEgDNS.controller = controller.NewController[ocpnetworkapiv1alpha1.DNSNameResolver]("dns-controller", dnsConfig)

	return extEgDNS, nil
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

// reconcileDNSNameResolver reconciles a DNSNameResolver object. If an object was deleted,
// then egressFirewallExternalDNS.Delete is called for the corresponding DNS name. If an
// object is added/updated, then egressFirewallExternalDNS.Add is called for the
// corresponding DNS name along with the associated addresses.
func (extEgDNS *ExternalEgressDNS) reconcileDNSNameResolver(key string) error {
	extEgDNS.dnsControllerLock.Lock()
	defer extEgDNS.dnsControllerLock.Unlock()

	// Split the key in namespace and name of the corresponding object.
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		klog.Errorf("reconcileDNSNameResolver failed to split meta namespace cache key %s for dns name resolver: %v", key, err)
		return nil
	}
	// Fetch the dns name resolver object using the name and namespace.
	obj, err := extEgDNS.dnsLister.DNSNameResolvers(namespace).Get(name)
	if err != nil {
		if kerrors.IsNotFound(err) {
			// DNSNameResolver object was deleted.

			// Get the corresponding DNS name, if avialable.
			dnsName, found := extEgDNS.resolverToDNSName[name]
			// If the DNS name was not found then return.
			if !found {
				return nil
			}

			// Delete the address set associated with the DNS name.
			err := extEgDNS.dnsTracker.deleteDNSName(dnsName)
			if err != nil {
				return err
			}

			// Delete the corresponding entries from the objDNSName
			// and the dnsNameObj maps.
			delete(extEgDNS.resolverToDNSName, name)
			delete(extEgDNS.dnsNameToResolver, dnsName)

			return nil
		}
		return fmt.Errorf("reconcileDNSNameResolver failed to fetch dns name resolver %s in namespace %s", name, namespace)
	}

	// DNSNameResolver object was added/updated.

	// Check if the object name corresponding to the DNS name exists
	// or not. If it exists, whether the object name matches with the
	// existing entry. Existing object name not matching the current
	// object name can happen if a user creates the DNSNameResolver
	// object for the same DNS name. The cluster-manager will ensure
	// that such objects are removed from the cluster, however, the
	// node will still receive the events related to the unwarranted
	// object. Any event related to such objects should be ignored.
	dnsName := string(obj.Spec.Name)
	objName, exists := extEgDNS.dnsNameToResolver[dnsName]
	if exists && objName != name {
		return nil
	}

	// Add the detials of the object name and the DNS name to the
	// objDNSName and the dnsNameObj maps.
	extEgDNS.resolverToDNSName[name] = dnsName
	extEgDNS.dnsNameToResolver[dnsName] = name

	// Get the addresses corresponding to the DNS name and add them to
	// the address set corresponding to the DNS name.
	addresses := []string{}
	for _, resolvedName := range obj.Status.ResolvedNames {
		for _, resolvedAddress := range resolvedName.ResolvedAddresses {
			addresses = append(addresses, resolvedAddress.IP)
		}
	}
	err = extEgDNS.dnsTracker.addDNSName(dnsName, addresses)
	return err
}

// Add adds the namespace to the set of namespaces where the DNS name is used in the
// EgressFirewall rules. It also returns the address set corresponding to the DNS name.
// The address set may be empty at this point if the corresponding DNSNameResolver
// object's status is still not updated with the associated IP addresses.
func (extEgDNS *ExternalEgressDNS) Add(namespace, dnsName string) (addressset.AddressSet, error) {
	return extEgDNS.dnsTracker.addNamespace(namespace, dnsName)
}

// Delete removes the namespace from the set of namespaces where the DNS name is used in
// the EgressFirewall rules.
func (extEgDNS *ExternalEgressDNS) Delete(namespace string) error {
	return extEgDNS.dnsTracker.deleteNamespace(namespace)
}

// Run starts the DNSNameResolver controller.
func (extEgDNS *ExternalEgressDNS) Run() error {
	return controller.Start(extEgDNS.controller)
}

// Shutdown stops the DNSNameResolver controller.
func (extEgDNS *ExternalEgressDNS) Shutdown() {
	controller.Stop(extEgDNS.controller)

}

// DeleteStaleAddrSets deletes all the address sets related to EgressFirewall DNS rules which are not
// referenced by any acl.
func (extEgDNS *ExternalEgressDNS) DeleteStaleAddrSets(nbClient libovsdbclient.Client) error {
	return extEgDNS.dnsTracker.deleteStaleAddressSets(nbClient)
}
