package ovn

import (
	"context"
	"fmt"
	"reflect"
	"time"

	ocpnetworkapiv1alpha1 "github.com/openshift/api/network/v1alpha1"
	dnsnameresolver "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/dns-name-resolver"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

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
func (oc *DefaultNetworkController) reconcileDNSNameResolver(key string) error {
	// Split the key in namespace and name of the corresponding object.
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		klog.Errorf("reconcileDNSNameResolver failed to split meta namespace cache key %s for dns name resolver: %v", key, err)
		return nil
	}
	// Fetch the dns name resolver object using the name and namespace.
	obj, err := oc.dnsLister.DNSNameResolvers(namespace).Get(name)
	if err != nil {
		if kerrors.IsNotFound(err) {
			// DNSNameResolver object was deleted.

			// Get the corresponding DNS name, if avialable.
			dnsName, found := oc.resolverToDNSName[name]
			// If the DNS name was not found then return.
			if !found {
				return nil
			}

			// Delete the address set associated with the DNS name.
			var err error
			waitErr := wait.PollUntilContextTimeout(context.Background(), 100*time.Millisecond, 1*time.Second, true, func(ctx context.Context) (bool, error) {
				deleteResp := oc.dnsNameResolver.Delete(dnsnameresolver.DeleteRequest{DNSName: dnsName})
				err = deleteResp.Err
				if err != nil {
					return false, nil
				}
				return true, nil
			})
			if waitErr != nil {
				errs := []error{waitErr}
				if err != nil {
					errs = append(errs, err)
				}
				return fmt.Errorf("error with reconcileDNSNameResolver - %v", errors.NewAggregate(errs))
			}

			// Delete the corresponding entries from the objDNSName
			// and the dnsNameObj maps.
			delete(oc.resolverToDNSName, name)
			delete(oc.dnsNameToResolver, dnsName)

			return nil
		}
		return fmt.Errorf("reconcileDNSNameResolver failed to fetch dns name resolver %s in namespace %s", name, namespace)
	}

	// DNSNameResolver object was added/updated.

	// Check if the object name corresponding to the DNS name exists
	// or not. If it exists, whether the object name matches with the
	// exisiting extry.
	dnsName := string(obj.Spec.Name)
	objName, exists := oc.dnsNameToResolver[dnsName]
	if exists && objName != name {
		return nil
	}

	// Add the detials of the object name and the DNS name to the
	// objDNSName and the dnsNameObj maps.
	oc.resolverToDNSName[name] = dnsName
	oc.dnsNameToResolver[dnsName] = name

	// Get the addresses corresponding to the DNS name and add them to
	// the address set corresponding to the DNS name.
	addresses := []string{}
	for _, resolvedName := range obj.Status.ResolvedNames {
		for _, resolvedAddress := range resolvedName.ResolvedAddresses {
			addresses = append(addresses, resolvedAddress.IP)
		}
	}
	addResp := oc.dnsNameResolver.Add(dnsnameresolver.AddRequest{DNSName: dnsName, Addresses: addresses})
	return addResp.Err

}
