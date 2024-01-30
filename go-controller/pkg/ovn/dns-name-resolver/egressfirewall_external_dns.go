package dnsnameresolver

import (
	"fmt"
	"net"
	"sync"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
)

// ExternalEgressDNS keeps track of DNS names and the corresponding IP addresses.
// For each DNS name, an address set is allocated and the address set is
// kept updated with the corresponding IP addresses. Whenever a DNS name
// is removed the corresponding addresset is destroyed.
type ExternalEgressDNS struct {
	// dnsLock protects the operations performed on the maps and address sets.
	dnsLock sync.Mutex
	// dnsNames maps the dnsNames to the dnsResolvedNames.
	dnsNames map[string]dnsResolvedName
	// namespaceDNSNames maps the DNS names used in the EgressFirewall rule of
	// a namespace to the namespace
	namespaceDNSNames map[string]sets.Set[string]
	// addressSetFactory helps in the creation of the address sets.
	addressSetFactory addressset.AddressSetFactory
	// controllerName gives the name of the controller which created this
	// instance of the ExternalEgressDNS.
	controllerName string
}

var _ DNSNameResolver = &ExternalEgressDNS{}

// dnsResolvedName contains details about a DNS resolved name.
type dnsResolvedName struct {
	// dnsAddressSet gives the address set that contains the current IPs.
	dnsAddressSet addressset.AddressSet
	// namespaces gives those namespaces in which the DNS name is used
	// in the EgressFirewall rules.
	namespaces sets.Set[string]
}

// NewExternalEgressDNS initializes and returns a new ExternalEgressDNS instance.
func NewExternalEgressDNS(addressSetFactory addressset.AddressSetFactory, controllerName string) *ExternalEgressDNS {
	return &ExternalEgressDNS{
		dnsNames:          make(map[string]dnsResolvedName),
		namespaceDNSNames: make(map[string]sets.Set[string]),
		addressSetFactory: addressSetFactory,
		controllerName:    controllerName,
	}
}

// Add is called whenever a DNS name is needed to be added or updated.
func (extEgDNS *ExternalEgressDNS) Add(request AddRequest) AddResponse {
	extEgDNS.dnsLock.Lock()
	defer extEgDNS.dnsLock.Unlock()

	// Get the details of the resolved name corresponding to the DNS name.
	resolvedName, exists := extEgDNS.dnsNames[request.DNSName]

	// If the details of the resolved name is not available, then create
	// the address set for the DNS name.
	if !exists {
		if extEgDNS.addressSetFactory == nil {
			return AddResponse{
				Err: fmt.Errorf("error adding DNS name %s: addressSetFactory is nil", request.DNSName),
			}
		}
		asIndex := GetEgressFirewallDNSAddrSetDbIDs(request.DNSName, extEgDNS.controllerName)
		dnsAddressSet, err := extEgDNS.addressSetFactory.NewAddressSet(asIndex, nil)
		if err != nil {
			return AddResponse{
				Err: fmt.Errorf("cannot create AddressSet for DNS name %s: %v", request.DNSName, err),
			}
		}
		resolvedName.dnsAddressSet = dnsAddressSet
		resolvedName.namespaces = sets.New[string]()
	}

	// Update the address set of the DNS name with the IPs. The IPs which don't match the ip mode
	// and those which match the cluster subnet are ignored.
	var ips []net.IP
	for _, addr := range request.Addresses {
		ip := net.ParseIP(addr)
		if ip != nil {
			ips = append(ips, ip)
		}
	}

	// Ignore ips from clusterSubnet, since this subnet shouldn't be affected by egress firewall.
	filteredIPs := []net.IP{}
	for _, ip := range ips {
		ignoreIP := false

		for _, clusterSubnet := range config.Default.ClusterSubnets {
			if clusterSubnet.CIDR.Contains(ip) {
				ignoreIP = true
				break
			}
		}

		if !ignoreIP {
			filteredIPs = append(filteredIPs, ip)
		}
	}
	if err := resolvedName.dnsAddressSet.SetIPs(filteredIPs); err != nil {
		return AddResponse{
			Err: fmt.Errorf("cannot add IPs to AddressSet for DNS name %s: %v", request.DNSName, err),
		}
	}

	// Add the updated resolved name to the dnsNames map corresponding to the DNS name.
	extEgDNS.dnsNames[request.DNSName] = resolvedName

	return AddResponse{
		Err: nil,
	}
}

// Delete is called whenever a DNS name is needed to be deleted.
func (res *ExternalEgressDNS) Delete(request DeleteRequest) DeleteResponse {
	res.dnsLock.Lock()
	defer res.dnsLock.Unlock()

	// Get the resolved name details corresponding to the DNS name.
	resolvedName, exists := res.dnsNames[request.DNSName]
	// Return if the corresponding resolved name details is not found.
	if !exists {
		klog.Errorf("Details missing for DNS name %s. Skipping address set destroy operation.", request.DNSName)
		return DeleteResponse{
			Err: nil,
		}
	}

	// Check if the DNS name is still in use in any namespace.
	if resolvedName.namespaces.Len() != 0 {
		return DeleteResponse{
			Err: fmt.Errorf("the DNS name, %s, is still in use in the following namespaces: %s", request.DNSName, resolvedName.namespaces.UnsortedList()),
		}
	}

	// Delete the address set associated with the DNS name.
	err := resolvedName.dnsAddressSet.Destroy()
	if err != nil {
		return DeleteResponse{
			Err: fmt.Errorf("error deleting AddressSet for DNS name %s %v", request.DNSName, err),
		}
	}
	delete(res.dnsNames, request.DNSName)

	return DeleteResponse{
		Err: nil,
	}
}

// AddNamespace adds the namespace to the set of namespaces where the DNS name is used in the
// EgressFirewall rules.
func (res *ExternalEgressDNS) AddNamespace(request AddNamespaceRequest) AddNamespaceResponse {
	res.dnsLock.Lock()
	defer res.dnsLock.Unlock()

	// Get the details of the resolved name corresponding to the DNS name.
	resolvedName, exists := res.dnsNames[request.DNSName]
	if !exists {
		return AddNamespaceResponse{
			Err: fmt.Errorf("no information found for DNS name: %s", request.DNSName),
		}
	}

	// Insert the namespace to the set of namespaces.
	resolvedName.namespaces.Insert(request.Namespace)

	dnsNames, found := res.namespaceDNSNames[request.Namespace]
	if !found {
		dnsNames = sets.New[string]()
	}
	dnsNames.Insert(request.DNSName)
	res.namespaceDNSNames[request.Namespace] = dnsNames

	return AddNamespaceResponse{
		Err: nil,
	}
}

// RemoveNamespace removes the namespace from the set of namespaces where the DNS name is used in
// the EgressFirewall rules.
func (res *ExternalEgressDNS) RemoveNamespace(request RemoveNamespaceRequest) RemoveNamespaceResponse {
	res.dnsLock.Lock()
	defer res.dnsLock.Unlock()

	dnsNames, found := res.namespaceDNSNames[request.Namespace]
	if !found {
		return RemoveNamespaceResponse{
			Err: nil,
		}
	}

	// Get the details of the resolved name corresponding to the DNS name.
	for dnsName := range dnsNames {
		// Get the resolved name details corresponding to the DNS name.
		resolvedName, exists := res.dnsNames[dnsName]
		// Return if the corresponding resolved name details is not found.
		if !exists {
			klog.Errorf("Details missing for DNS name %s. Skipping address set destroy operation.", dnsName)
			return RemoveNamespaceResponse{
				Err: nil,
			}
		}
		// Delete the namespace from the set of namespaces.
		resolvedName.namespaces.Delete(request.Namespace)
	}

	delete(res.namespaceDNSNames, request.Namespace)

	return RemoveNamespaceResponse{
		Err: nil,
	}
}

// GetAddressSet returns the address set corresponding to the DNS name, if it exists.
func (res *ExternalEgressDNS) GetAddressSet(request GetAddressSetRequest) GetAddressSetResponse {
	res.dnsLock.Lock()
	defer res.dnsLock.Unlock()

	// Get the details of the resolved name corresponding to the DNS name.
	resolvedName, exists := res.dnsNames[request.DNSName]
	if !exists {
		return GetAddressSetResponse{
			DNSAddressSet: nil,
			Err:           fmt.Errorf("address set not yet created for DNS name: %s", request.DNSName),
		}
	}

	return GetAddressSetResponse{
		DNSAddressSet: resolvedName.dnsAddressSet,
		Err:           nil,
	}
}

func (res *ExternalEgressDNS) Update(UpdateRequest) UpdateResponse {
	return UpdateResponse{}
}

func (res *ExternalEgressDNS) Run(RunRequest) {}

func (res *ExternalEgressDNS) Shutdown() {}
