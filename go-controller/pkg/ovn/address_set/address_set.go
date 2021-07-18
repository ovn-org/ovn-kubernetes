package addressset

import (
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"

	"github.com/pkg/errors"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/libovsdb/model"
	"github.com/ovn-org/libovsdb/ovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
)

const (
	ipv4AddressSetSuffix = "_v4"
	ipv6AddressSetSuffix = "_v6"
)

type AddressSetIterFunc func(hashedName, namespace, suffix string)
type AddressSetDoFunc func(as AddressSet) error

// AddressSetFactory is an interface for managing address set objects
type AddressSetFactory interface {
	// NewAddressSet returns a new object that implements AddressSet
	// and contains the given IPs, or an error. Internally it creates
	// an address set for IPv4 and IPv6 each.
	NewAddressSet(name string, ips []net.IP) (AddressSet, error)
	// EnsureAddressSet makes sure that an address set object exists in ovn
	// with the given name
	EnsureAddressSet(name string) error
	// ProcessEachAddressSet calls the given function for each address set
	// known to the factory
	ProcessEachAddressSet(iteratorFn AddressSetIterFunc) error
	// DestroyAddressSetInBackingStore deletes the named address set from the
	// factory's backing store. SHOULD NOT BE CALLED for any address set
	// for which an AddressSet object has been created.
	DestroyAddressSetInBackingStore(name string) error
}

// AddressSet is an interface for address set objects
type AddressSet interface {
	// GetASHashName returns the hashed name for ipv6 and ipv4 addressSets
	GetASHashNames() (string, string)
	// GetName returns the descriptive name of the address set
	GetName() string
	// AddIPs adds the array of IPs to the address set
	AddIPs(ip []net.IP) error
	// SetIPs sets the address set to the given array of addresses
	SetIPs(ip []net.IP) error
	DeleteIPs(ip []net.IP) error
	Destroy() error
}

type ovnAddressSetFactory struct {
	nbClient libovsdbclient.Client
}

// NewOvnAddressSetFactory creates a new AddressSetFactory backed by
// address set objects that execute OVN commands
func NewOvnAddressSetFactory(nbClient libovsdbclient.Client) AddressSetFactory {
	return &ovnAddressSetFactory{
		nbClient: nbClient,
	}
}

// ovnAddressSetFactory implements the AddressSetFactory interface
var _ AddressSetFactory = &ovnAddressSetFactory{}

// NewAddressSet returns a new address set object
func (asf *ovnAddressSetFactory) NewAddressSet(name string, ips []net.IP) (AddressSet, error) {
	res, err := newOvnAddressSets(asf.nbClient, name, ips)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// EnsureAddressSet ensures the address_set with the given name exists and if it does not creates an empty addressSet
func (asf *ovnAddressSetFactory) EnsureAddressSet(name string) error {
	hashedAddressSetNames := []string{}
	ip4ASName, ip6ASName := MakeAddressSetName(name)
	if config.IPv4Mode {
		hashedAddressSetNames = append(hashedAddressSetNames, ip4ASName)
	}
	if config.IPv6Mode {
		hashedAddressSetNames = append(hashedAddressSetNames, ip6ASName)
	}
	for _, hashedAddressSetName := range hashedAddressSetNames {
		addrset := &nbdb.AddressSet{ExternalIDs: map[string]string{"name": name}}
		err := asf.nbClient.Get(addrset)
		if err != nil {
			return fmt.Errorf("Ensuring address set %s failed: %+v", name, err)
		}
		if len(addrset.UUID) == 0 {
			//create the address_set with no IPs
			ops, err := asf.nbClient.Create(&nbdb.AddressSet{
				Name:        hashedAddressSetName,
				ExternalIDs: map[string]string{"name": name},
			})
			if err != nil {
				return fmt.Errorf("failed to ensure address set %s (%v)",
					name, err)
			}
			_, err = asf.nbClient.Transact(ops...)
			if err != nil {
				return fmt.Errorf("failed to ensure address set %s (%v)",
					name, err)
			}
		} else {
			//should never happen, the name ExternalID should be unique
			return fmt.Errorf("Ensure addressSet failed, too many address set with name %s", name)
		}
	}

	return nil
}

func forEachAddressSet(nbClient libovsdbclient.Client, do func(string)) error {
	addrSetList := &[]nbdb.AddressSet{}
	err := nbClient.WhereCache(
		func(addrSet *nbdb.AddressSet) bool {
			_, exists := addrSet.ExternalIDs["name"]
			return exists
		}).List(addrSetList)
	if err != nil {
		return fmt.Errorf("error reading address sets: %+v", err)
	}

	for _, addrSet := range *addrSetList {
		do(addrSet.ExternalIDs["name"])
	}
	return nil
}

// ProcessEachAddressSet will pass the unhashed address set name, namespace name
// and the first suffix in the name to the 'iteratorFn' for every address_set in
// OVN. (Unhashed address set names are of the form namespaceName[.suffix1.suffix2. .suffixN])
func (asf *ovnAddressSetFactory) ProcessEachAddressSet(iteratorFn AddressSetIterFunc) error {
	processedAddressSets := sets.String{}
	err := forEachAddressSet(asf.nbClient, func(name string) {
		// Remove the suffix from the address set name and normalize
		addrSetName := truncateSuffixFromAddressSet(name)
		if processedAddressSets.Has(addrSetName) {
			// We have already processed the address set. In case of dual stack we will have _v4 and _v6
			// suffixes for address sets. Since we are normalizing these two address sets through this API
			// we will process only one normalized address set name.
			return
		}
		processedAddressSets.Insert(addrSetName)
		names := strings.Split(addrSetName, ".")
		addrSetNamespace := names[0]
		nameSuffix := ""
		if len(names) >= 2 {
			nameSuffix = names[1]
		}
		iteratorFn(addrSetName, addrSetNamespace, nameSuffix)
	})

	return err
}

func truncateSuffixFromAddressSet(asName string) string {
	// Legacy address set names will not have v4 or v6 suffixes.
	// truncate them for the new ones
	if strings.HasSuffix(asName, ipv4AddressSetSuffix) {
		return strings.TrimSuffix(asName, ipv4AddressSetSuffix)
	}
	if strings.HasSuffix(asName, ipv6AddressSetSuffix) {
		return strings.TrimSuffix(asName, ipv6AddressSetSuffix)
	}
	return asName
}

// DestroyAddressSetInBackingStore ensures an address set is deleted
func (asf *ovnAddressSetFactory) DestroyAddressSetInBackingStore(name string) error {
	// We need to handle both legacy and new address sets in this method. Legacy names
	// will not have v4 and v6 suffix as they were same as namespace name. Hence we will always try to destroy
	// the address set with raw name(namespace name), v4 name and v6 name.  The method destroyAddressSet uses
	// --if-exists parameter which will take care of deleting the address set only if it exists.
	err := destroyAddressSet(asf.nbClient, name)
	if err != nil {
		return err
	}
	ip4ASName, ip6ASName := MakeAddressSetName(name)
	err = destroyAddressSet(asf.nbClient, ip4ASName)
	if err != nil {
		return err
	}
	err = destroyAddressSet(asf.nbClient, ip6ASName)
	if err != nil {
		return err
	}
	return nil
}

func destroyAddressSet(nbClient libovsdbclient.Client, name string) error {
	addrset := &nbdb.AddressSet{
		Name:        hashedAddressSet(name),
		ExternalIDs: map[string]string{"name": name},
	}
	ops, err := nbClient.Where(addrset).Delete()
	if err != nil {
		return fmt.Errorf("failed to delete address set %s (%v)",
			name, err)
	}
	_, err = nbClient.Transact(ops...)
	if err != nil {
		return fmt.Errorf("failed to delete address set %s (%v)",
			name, err)
	}
	return nil
}

type ovnAddressSet struct {
	nbClient libovsdbclient.Client
	name     string
	hashName string
	uuid     string
	ips      map[string]net.IP
}

type ovnAddressSets struct {
	sync.RWMutex
	name string
	ipv4 *ovnAddressSet
	ipv6 *ovnAddressSet
}

// ovnAddressSets implements the AddressSet interface
var _ AddressSet = &ovnAddressSets{}

// hash the provided input to make it a valid ovnAddressSet name.
func hashedAddressSet(s string) string {
	return util.HashForOVN(s)
}

func asDetail(as *ovnAddressSet) string {
	return fmt.Sprintf("%s/%s/%s", as.uuid, as.name, as.hashName)
}

func newOvnAddressSets(nbClient libovsdbclient.Client, name string, ips []net.IP) (*ovnAddressSets, error) {
	var (
		v4set, v6set *ovnAddressSet
		err          error
	)
	v4IPs, v6IPs := splitIPsByFamily(ips)

	ip4ASName, ip6ASName := MakeAddressSetName(name)
	if config.IPv4Mode {
		v4set, err = newOvnAddressSet(nbClient, ip4ASName, v4IPs)
		if err != nil {
			return nil, err
		}
	}
	if config.IPv6Mode {
		v6set, err = newOvnAddressSet(nbClient, ip6ASName, v6IPs)
		if err != nil {
			return nil, err
		}
	}
	return &ovnAddressSets{name: name, ipv4: v4set, ipv6: v6set}, nil
}

func newOvnAddressSet(nbClient libovsdbclient.Client, name string, ips []net.IP) (*ovnAddressSet, error) {
	as := &ovnAddressSet{
		nbClient: nbClient,
		name:     name,
		hashName: hashedAddressSet(name),
		ips:      make(map[string]net.IP),
	}
	for _, ip := range ips {
		as.ips[ip.String()] = ip
	}

	addrSetList := &[]nbdb.AddressSet{}
	err := as.nbClient.WhereCache(
		func(addrSet *nbdb.AddressSet) bool {
			return addrSet.Name == as.hashName
		}).List(addrSetList)
	if err != nil {
		return nil, err
	}

	if len(*addrSetList) > 0 {
		// if there is already an addressSet, reuse the addressSet and set the IPs to the slice provided
		as.uuid = (*addrSetList)[0].UUID
		klog.V(5).Infof("New(%s) already exists; updating IPs", asDetail(as))

		allOps := []ovsdb.Operation{}
		addrset := &nbdb.AddressSet{UUID: as.uuid}
		ops, err := as.nbClient.Where(addrset).Mutate(addrset, model.Mutation{
			Field:   &addrset.Addresses,
			Mutator: ovsdb.MutateOperationDelete,
			Value:   (*addrSetList)[0].Addresses,
		})
		if err != nil {
			return nil, err
		}

		allOps = append(allOps, ops...)
		var ipStrings []string
		for _, ip := range ips {
			ipStrings = append(ipStrings, ip.String())
		}
		ops, err = as.nbClient.Where(addrset).Mutate(addrset, model.Mutation{
			Field:   &addrset.Addresses,
			Mutator: ovsdb.MutateOperationInsert,
			Value:   ipStrings,
		})
		if err != nil {
			return nil, err
		}
		allOps = append(allOps, ops...)

		_, err = as.nbClient.Transact(allOps...)
		if err != nil {
			return nil, err
		}
	} else {
		//create a new addressSet
		ops, err := nbClient.Create(&nbdb.AddressSet{
			Name:        as.hashName,
			Addresses:   as.allIPs(),
			ExternalIDs: map[string]string{"name": as.name},
		})
		if err != nil {
			return nil, fmt.Errorf("failed to create address set %s (%v)",
				name, err)
		}
		res, err := as.nbClient.Transact(ops...)
		if err != nil {
			return nil, fmt.Errorf("failed to ensure creat address set %s (%v)",
				name, err)
		}
		if len(res) == 1 {
			as.uuid = res[0].UUID.GoUUID
		} else {
			//should never happen
			return nil, fmt.Errorf("Returned too many results from addressSet creation")
		}
	}

	klog.V(5).Infof("New(%s) with %v", asDetail(as), ips)

	return as, nil
}

func (as *ovnAddressSets) GetASHashNames() (string, string) {
	var ipv4AS string
	var ipv6AS string
	if as.ipv4 != nil {
		ipv4AS = as.ipv4.hashName
	}
	if as.ipv6 != nil {
		ipv6AS = as.ipv6.hashName
	}
	return ipv4AS, ipv6AS
}

func (as *ovnAddressSets) GetName() string {
	return as.name
}

func (as *ovnAddressSets) SetIPs(ips []net.IP) error {
	var err error
	as.Lock()
	defer as.Unlock()

	v4ips, v6ips := splitIPsByFamily(ips)

	if as.ipv6 != nil {
		err = as.ipv6.setIPs(v6ips)
	}
	if as.ipv4 != nil {
		err = errors.Wrapf(err, "%v", as.ipv4.setIPs(v4ips))
	}

	return err
}

func (as *ovnAddressSets) AddIPs(ips []net.IP) error {
	if len(ips) == 0 {
		return nil
	}

	as.Lock()
	defer as.Unlock()

	v4ips, v6ips := splitIPsByFamily(ips)
	if as.ipv6 != nil {
		if err := as.ipv6.addIPs(v6ips); err != nil {
			return fmt.Errorf("failed to AddIPs to the v6 set: %w", err)
		}
	}
	if as.ipv4 != nil {
		if err := as.ipv4.addIPs(v4ips); err != nil {
			return fmt.Errorf("failed to AddIPs to the v4 set: %w", err)
		}
	}

	return nil
}

func (as *ovnAddressSets) DeleteIPs(ips []net.IP) error {
	if len(ips) == 0 {
		return nil
	}

	as.Lock()
	defer as.Unlock()

	v4ips, v6ips := splitIPsByFamily(ips)
	if as.ipv6 != nil {
		if err := as.ipv6.deleteIPs(v6ips); err != nil {
			return fmt.Errorf("failed to DeleteIPs to the v6 set: %w", err)
		}
	}
	if as.ipv4 != nil {
		if err := as.ipv4.deleteIPs(v4ips); err != nil {
			return fmt.Errorf("failed to DeleteIPs to the v4 set: %w", err)
		}
	}
	return nil
}

func (as *ovnAddressSets) Destroy() error {
	as.Lock()
	defer as.Unlock()

	if as.ipv4 != nil {
		err := as.ipv4.destroy()
		if err != nil {
			return err
		}
		as.ipv4 = nil
	}
	if as.ipv6 != nil {
		err := as.ipv6.destroy()
		if err != nil {
			return err
		}
		as.ipv6 = nil
	}
	return nil
}

// updateAddressSet is temporary. libovsdb Update() is non functional in order to update you need to use two mutates first to clear the address field
// and the second to set the new addrs
func (as *ovnAddressSet) updateAddressSet(addrsToAdd []net.IP) ([]ovsdb.Operation, error) {
	addrSet := &nbdb.AddressSet{
		UUID: as.uuid,
	}
	allOps := []ovsdb.Operation{}
	addrsToClear := make([]string, 0, len(as.ips))
	for ip := range as.ips {
		addrsToClear = append(addrsToClear, ip)
	}
	ops1, err := as.nbClient.Where(addrSet).Mutate(addrSet, model.Mutation{
		Field:   &addrSet.Addresses,
		Mutator: ovsdb.MutateOperationDelete,
		Value:   addrsToClear,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to set address set %q (%v)",
			asDetail(as), err)
	}
	allOps = append(allOps, ops1...)

	stringAddrsToAdd := make([]string, 0, len(addrsToAdd))
	for _, ip := range addrsToAdd {
		stringAddrsToAdd = append(stringAddrsToAdd, ip.String())
	}
	ops2, err := as.nbClient.Where(addrSet).Mutate(addrSet, model.Mutation{
		Field:   &addrSet.Addresses,
		Mutator: ovsdb.MutateOperationInsert,
		Value:   stringAddrsToAdd,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to set address set %q (%v)",
			asDetail(as), err)
	}
	allOps = append(allOps, ops2...)
	return allOps, nil

}

// setIP updates the given address set in OVN to be only the given IPs, disregarding
// existing state.
func (as *ovnAddressSet) setIPs(ips []net.IP) error {
	var err error
	ops, err := as.updateAddressSet(ips)
	if err != nil {
		return err
	}

	_, err = as.nbClient.Transact(ops...)
	if err != nil {
		return fmt.Errorf("failed to set IPs for  address set %q (%v)",
			asDetail(as), err)
	}

	as.ips = make(map[string]net.IP, len(ips))
	for _, ip := range ips {
		as.ips[ip.String()] = ip
	}
	return nil
}

// addIPs appends the set of IPs to the existing address_set.
func (as *ovnAddressSet) addIPs(ips []net.IP) error {
	// dedup
	uniqIPs := make([]string, 0, len(ips))
	for _, ip := range ips {
		if _, ok := as.ips[ip.String()]; ok {
			continue
		}
		uniqIPs = append(uniqIPs, ip.String())
	}

	if len(uniqIPs) == 0 {
		return nil
	}

	klog.V(5).Infof("(%s) adding IPs (%s) to address set", asDetail(as), uniqIPs)
	addrset := &nbdb.AddressSet{UUID: as.uuid}
	ops, err := as.nbClient.Where(addrset).Mutate(addrset, model.Mutation{
		Field:   &addrset.Addresses,
		Mutator: ovsdb.MutateOperationInsert,
		Value:   uniqIPs,
	})
	if err != nil {
		return fmt.Errorf("failed to add IPs toaddress set %q (%v)",
			asDetail(as), err)
	}
	_, err = as.nbClient.Transact(ops...)
	if err != nil {
		return fmt.Errorf("failed to add IPs to address set %q (%v)",
			asDetail(as), err)
	}

	for _, ip := range ips {
		as.ips[ip.String()] = ip
	}
	return nil
}

// deleteIPs removes selected IPs from the existing address_set
func (as *ovnAddressSet) deleteIPs(ips []net.IP) error {
	// dedup
	uniqIPs := make([]string, 0, len(ips))
	for _, ip := range ips {
		if _, ok := as.ips[ip.String()]; !ok {
			continue
		}
		uniqIPs = append(uniqIPs, ip.String())
	}

	if len(uniqIPs) == 0 {
		return nil
	}

	klog.V(5).Infof("(%s) deleting IP %s from address set", asDetail(as), uniqIPs)

	addrset := &nbdb.AddressSet{UUID: as.uuid}
	ops, err := as.nbClient.Where(addrset).Mutate(addrset, model.Mutation{
		Field:   &addrset.Addresses,
		Mutator: ovsdb.MutateOperationDelete,
		Value:   uniqIPs,
	})
	if err != nil {
		return fmt.Errorf("failed to delete IPs from address set %q (%v)",
			asDetail(as), err)
	}
	_, err = as.nbClient.Transact(ops...)
	if err != nil {
		return fmt.Errorf("failed to delete IPs from address set %q (%v)",
			asDetail(as), err)
	}

	for _, ip := range uniqIPs {
		delete(as.ips, ip)
	}
	return nil
}

func (as *ovnAddressSet) destroy() error {
	klog.V(5).Infof("destroy(%s)", asDetail(as))
	addrset := &nbdb.AddressSet{UUID: as.uuid}
	ops, err := as.nbClient.Where(addrset).Delete()
	if err != nil {
		return fmt.Errorf("failed to destroy address set %q (%v)",
			asDetail(as), err)
	}
	_, err = as.nbClient.Transact(ops...)
	if err != nil {
		return fmt.Errorf("failed to destroy address set %q (%v)",
			asDetail(as), err)
	}

	as.ips = nil
	return nil
}

func MakeAddressSetName(name string) (string, string) {
	return name + ipv4AddressSetSuffix, name + ipv6AddressSetSuffix
}

func MakeAddressSetHashNames(name string) (string, string) {
	ipv4AddressSetName, ipv6AddressSetName := MakeAddressSetName(name)
	return hashedAddressSet(ipv4AddressSetName), hashedAddressSet(ipv6AddressSetName)
}

// splitIPsByFamily takes a slice of IPs and returns two slices, with
// v4 and v6 addresses collated accordingly.
func splitIPsByFamily(ips []net.IP) (v4 []net.IP, v6 []net.IP) {
	for _, ip := range ips {
		if utilnet.IsIPv6(ip) {
			v6 = append(v6, ip)
		} else {
			v4 = append(v4, ip)
		}
	}
	return
}

func (as *ovnAddressSet) allIPs() []string {
	// my kingdom for a ".values()" function
	out := make([]string, 0, len(as.ips))
	for _, ip := range as.ips {
		out = append(out, ip.String())
	}
	// so the tests are predictable
	sort.Strings(out)
	return out
}
