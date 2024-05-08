package dnsnameresolver

import (
	"fmt"
	"net"
	"sync"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
)

type dnsTracker struct {
	// dnsLock protects the operations performed on the maps related to
	// the address sets.
	dnsLock sync.Mutex
	// dnsNames maps the dnsNames to the dnsResolvedNames.
	dnsNames map[string]*dnsResolvedName
	// namespaceDNSNames maps the DNS names used in the EgressFirewall rule of
	// a namespace to the namespace
	namespaceDNSNames map[string]sets.Set[string]
	// addressSetFactory helps in the creation of the address sets.
	addressSetFactory addressset.AddressSetFactory
	// controllerName gives the name of the controller which created this
	// instance of the ExternalEgressDNS.
	controllerName string
	// ignoreClusterSubnet indicates whether to ignore IP addresses matching
	// the cluster subnet.
	ignoreClusterSubnet bool
}

// dnsResolvedName contains details about a DNS resolved name.
type dnsResolvedName struct {
	// dnsAddressSet gives the address set that contains the current IPs.
	dnsAddressSet addressset.AddressSet
	// namespaces gives those namespaces in which the DNS name is used
	// in the EgressFirewall rules. If any entry exists in the namespaces
	// set, then the dnsAddressSet is still referenced by an acl. The
	// dnsAddressSet can only be deleted when the namespaces set is empty.
	namespaces sets.Set[string]
	// deleted indicates whether the DNSNameResolver object corresponding
	// to the DNS name was deleted or not.
	deleted bool
}

func newDNSTracker(addressSetFactory addressset.AddressSetFactory, controllerName string, ignoreClusterSubnet bool) dnsTracker {
	return dnsTracker{
		dnsNames:            make(map[string]*dnsResolvedName),
		namespaceDNSNames:   make(map[string]sets.Set[string]),
		addressSetFactory:   addressSetFactory,
		controllerName:      controllerName,
		ignoreClusterSubnet: ignoreClusterSubnet,
	}
}

// addDNSName is called whenever a DNS name is needed to be added or updated.
func (dnsTracker *dnsTracker) addDNSName(dnsName string, addresses []string) error {
	dnsTracker.dnsLock.Lock()
	defer dnsTracker.dnsLock.Unlock()

	// Get the details of the resolved name corresponding to the DNS name.
	resolvedName, err := dnsTracker.ensureResolvedName(dnsName)
	if err != nil {
		return err
	}

	// Update the address set of the DNS name with the IPs. The IPs which don't match the ip mode
	// and those which match the cluster subnet, if ignoreClusterSubnet is set to true, are ignored.
	if dnsTracker.ignoreClusterSubnet {
		// Ignore ips from clusterSubnet, since this subnet shouldn't be affected by egress firewall.
		filteredIPs := []string{}
		for _, addr := range addresses {
			ignoreIP := false

			for _, clusterSubnet := range config.Default.ClusterSubnets {
				if clusterSubnet.CIDR.Contains(net.ParseIP(addr)) {
					ignoreIP = true
					break
				}
			}

			if !ignoreIP {
				filteredIPs = append(filteredIPs, addr)
			}
		}
		addresses = filteredIPs
	}

	if err := resolvedName.dnsAddressSet.AddAddresses(addresses); err != nil {
		return fmt.Errorf("cannot add IPs to AddressSet for DNS name %s: %v", dnsName, err)
	}

	return nil
}

// deleteDNSName is called whenever a DNS name is needed to be deleted.
func (dnsTracker *dnsTracker) deleteDNSName(dnsName string) error {
	dnsTracker.dnsLock.Lock()
	defer dnsTracker.dnsLock.Unlock()

	// Get the resolved name details corresponding to the DNS name.
	resolvedName, exists := dnsTracker.dnsNames[dnsName]
	// Return if the corresponding resolved name details is not found.
	if !exists {
		klog.Errorf("Details missing for DNS name %s. Skipping address set destroy operation.", dnsName)
		return nil
	}

	// Check if the DNS name is still in use in any namespace.
	if resolvedName.namespaces.Len() != 0 {
		resolvedName.deleted = true
		return nil
	}

	// Delete the resolvedName corresponding to the DNS name as well as
	// the address set associated with the DNS name.
	return dnsTracker.deleteResolvedName(dnsName, resolvedName)
}

// addNamespace adds the namespace to the set of namespaces where the DNS name is used in the
// EgressFirewall rules. It also returns the address set corresponding to the DNS name.
// The address set may be empty at this point if the corresponding DNSNameResolver
// object's status is still not updated with the associated IP addresses.
func (dnsTracker *dnsTracker) addNamespace(namespace, dnsName string) (addressset.AddressSet, error) {
	dnsTracker.dnsLock.Lock()
	defer dnsTracker.dnsLock.Unlock()

	// Get the details of the resolved name corresponding to the DNS name.
	resolvedName, err := dnsTracker.ensureResolvedName(dnsName)
	if err != nil {
		return nil, err
	}

	// Insert the namespace to the set of namespaces.
	resolvedName.namespaces.Insert(namespace)

	dnsNames, found := dnsTracker.namespaceDNSNames[namespace]
	if !found {
		dnsNames = sets.New[string]()
		dnsTracker.namespaceDNSNames[namespace] = dnsNames
	}
	dnsNames.Insert(dnsName)

	return resolvedName.dnsAddressSet, nil
}

// deleteNamespace removes the namespace from the set of namespaces where the DNS name is used in
// the EgressFirewall rules.
func (dnsTracker *dnsTracker) deleteNamespace(namespace string) error {
	dnsTracker.dnsLock.Lock()
	defer dnsTracker.dnsLock.Unlock()

	dnsNames, found := dnsTracker.namespaceDNSNames[namespace]
	if !found {
		return nil
	}

	// Get the details of the resolved name corresponding to the DNS name.
	for dnsName := range dnsNames {
		// Get the resolved name details corresponding to the DNS name.
		resolvedName, exists := dnsTracker.dnsNames[dnsName]
		// Return if the corresponding resolved name details is not found.
		if !exists {
			klog.Errorf("Details missing for DNS name %s. Skipping address set destroy operation.", dnsName)
			return nil
		}
		// Delete the namespace from the set of namespaces.
		resolvedName.namespaces.Delete(namespace)

		if resolvedName.namespaces.Len() == 0 && resolvedName.deleted {
			err := dnsTracker.deleteResolvedName(dnsName, resolvedName)
			if err != nil {
				return err
			}
		}
	}

	delete(dnsTracker.namespaceDNSNames, namespace)

	return nil
}

// ensureResolvedName ensures that the resolved name corresponding to the DNS name exists
// and returns it.
func (dnsTracker *dnsTracker) ensureResolvedName(dnsName string) (*dnsResolvedName, error) {
	// Get the details of the resolved name corresponding to the DNS name.
	resolvedName, exists := dnsTracker.dnsNames[dnsName]

	// If the details of the resolved name is not available, then create
	// the address set for the DNS name.
	if !exists {
		asIndex := GetEgressFirewallDNSAddrSetDbIDs(dnsName, dnsTracker.controllerName)
		dnsAddressSet, err := dnsTracker.addressSetFactory.EnsureAddressSet(asIndex)
		if err != nil {
			return nil, fmt.Errorf("cannot create AddressSet for DNS name %s: %v", dnsName, err)
		}
		resolvedName = &dnsResolvedName{
			dnsAddressSet: dnsAddressSet,
			namespaces:    sets.New[string](),
		}
		dnsTracker.dnsNames[dnsName] = resolvedName
	}

	return resolvedName, nil
}

// deleteResolvedName deletes the resolvedName corresponding to the DNS name as well as
// the address set associated with the DNS name.
func (dnsTracker *dnsTracker) deleteResolvedName(dnsName string, resolvedName *dnsResolvedName) error {
	// Delete the address set associated with the DNS name.
	err := resolvedName.dnsAddressSet.Destroy()
	if err != nil {
		return fmt.Errorf("error deleting AddressSet for DNS name %s %v", dnsName, err)
	}
	delete(dnsTracker.dnsNames, dnsName)

	return nil
}

// deleteStaleAddressSets deletes all the address sets related to EgressFirewall DNS rules which are not
// referenced by any acl.
func (dnsTracker *dnsTracker) deleteStaleAddressSets(nbClient libovsdbclient.Client) error {
	dnsTracker.dnsLock.Lock()
	defer dnsTracker.dnsLock.Unlock()

	predicateIDs := libovsdbops.NewDbObjectIDs(libovsdbops.AddressSetEgressFirewallDNS, dnsTracker.controllerName, nil)
	return libovsdbutil.DeleteAddrSetsWithoutACLRef(predicateIDs, nbClient)
}
