package ovn

import (
	"fmt"
	"net"
	"sync"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"k8s.io/klog/v2"
)

// Resolver keeps track of DNS names and the corresponding IP addresses.
// For each DNS name, an address set is allocated and the address set is
// kept updated with the corresponding IP addresses. Whenever a DNS name
// is removed the corresponding addresset is destroyed.
type Resolver struct {
	// dnsLock protects the operations performed on the maps and address sets.
	dnsLock sync.Mutex
	// dnsNames maps the dnsNames to the dnsResolvedNames.
	dnsNames map[string]dnsResolvedName
	// addressSetFactory helps in the creation of the address sets.
	addressSetFactory addressset.AddressSetFactory
	// controllerName gives the name of the controller which created this
	// instance of the Resolver.
	controllerName string
}

// dnsResolvedName contains details about a DNS resolved name.
type dnsResolvedName struct {
	// ips gives the current IPs associated with the DNS resolved name.
	// NOTE: Used for tests only.
	ips []net.IP
	// dnsAddressSet gives the address set that contains the current IPs.
	dnsAddressSet addressset.AddressSet
}

// NewResolver initializes and returns a new resolver instance.
func NewResolver(addressSetFactory addressset.AddressSetFactory, controllerName string) *Resolver {
	return &Resolver{
		dnsNames:          make(map[string]dnsResolvedName),
		addressSetFactory: addressSetFactory,
		controllerName:    controllerName,
	}
}

// Add is called whenever a DNS name is needed to be added or updated.
func (res *Resolver) Add(dnsName string, addresses []string, updateAddressSet bool) (addressset.AddressSet, error) {
	res.dnsLock.Lock()
	defer res.dnsLock.Unlock()

	// Get the details of the resolved name corresponding to the DNS name.
	resolvedName, exists := res.dnsNames[dnsName]

	// If the details of the resolved name is not available, then create
	// the address set for the DNS name.
	if !exists {
		if res.addressSetFactory == nil {
			return nil, fmt.Errorf("error adding DNS name %s: addressSetFactory is nil", dnsName)
		}
		asIndex := getEgressFirewallDNSAddrSetDbIDs(dnsName, res.controllerName)
		dnsAddressSet, err := res.addressSetFactory.NewAddressSet(asIndex, nil)
		if err != nil {
			return nil, fmt.Errorf("cannot create AddressSet for DNS name %s: %v", dnsName, err)
		}
		resolvedName.dnsAddressSet = dnsAddressSet
	}

	// If updateAddressSet is set to true then update the address set of
	// the DNS name with the IPs. The IPs which don't match the ip mode
	// and those which match the cluster subnet are ignored.
	if updateAddressSet {
		var ips []net.IP
		for _, addr := range addresses {
			ip := net.ParseIP(addr)
			if ip != nil {
				ips = append(ips, ip)
			}
		}
		resolvedName.ips = ips

		// Ignore ips which don't match the configured ip mode.
		// Ignore ips from clusterSubnet, since this subnet shouldn't be affected by egress firewall.
		filteredIPs := []net.IP{}
		for _, ip := range ips {
			ignoreIP := false

			if config.IPv4Mode && !config.IPv6Mode && ip.To4() == nil {
				ignoreIP = true
			} else if !config.IPv4Mode && config.IPv6Mode && ip.To4() != nil {
				ignoreIP = true
			}

			if !ignoreIP {
				for _, clusterSubnet := range config.Default.ClusterSubnets {
					if clusterSubnet.CIDR.Contains(ip) {
						ignoreIP = true
						break
					}
				}
			}

			if !ignoreIP {
				filteredIPs = append(filteredIPs, ip)
			}
		}
		if err := resolvedName.dnsAddressSet.SetIPs(filteredIPs); err != nil {
			return nil, fmt.Errorf("cannot add IPs to AddressSet for DNS name %s: %v", dnsName, err)
		}
	}

	// Add the updated resolved name to the dnsNames map corresponding to the DNS name.
	res.dnsNames[dnsName] = resolvedName

	return resolvedName.dnsAddressSet, nil
}

// Delete is called whenever a DNS name is needed to be deleted.
func (res *Resolver) Delete(dnsName string) error {
	res.dnsLock.Lock()
	defer res.dnsLock.Unlock()

	// Get the resolved name details corresponding to the DNS name.
	resolvedName, exists := res.dnsNames[dnsName]
	// Return if the corresponding resolved name details is not found.
	if !exists {
		klog.Errorf("Details missing for DNS name %s. Skipping address set destroy operation.", dnsName)
		return nil
	}

	// Delete the address set associated with the DNS name.
	err := resolvedName.dnsAddressSet.Destroy()
	if err != nil {
		return fmt.Errorf("error deleting AddressSet for DNS name %s %v", dnsName, err)
	}
	delete(res.dnsNames, dnsName)

	return nil
}
