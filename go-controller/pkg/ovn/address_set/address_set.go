package addressset

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strings"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"

	"github.com/pkg/errors"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/libovsdb/model"
	"github.com/ovn-org/libovsdb/ovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
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
	EnsureAddressSet(name string) (AddressSet, error)
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
	// GetIPs gets the list of v4 & v6 IPs from the address set
	GetIPs() ([]string, []string)
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

// ensureAddressSet ensures the address_set with the given name exists and if it does not creates an empty addressSet
func ensureOvnAddressSet(nbClient libovsdbclient.Client, name string) (*ovnAddressSet, error) {
	as := &ovnAddressSet{
		nbClient: nbClient,
		name:     name,
		hashName: hashedAddressSet(name),
	}

	addrset := &nbdb.AddressSet{Name: as.hashName, ExternalIDs: map[string]string{"name": name}}
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	err := as.nbClient.Get(ctx, addrset)
	if err != nil && err != libovsdbclient.ErrNotFound {
		return nil, fmt.Errorf("ensuring address set %s failed: %+v", name, err)
	}

	if len(addrset.UUID) == 0 {
		//create the address_set with no IPs
		ops, err := as.nbClient.Create(&nbdb.AddressSet{
			Name:        as.hashName,
			ExternalIDs: map[string]string{"name": name},
		})
		if err != nil {
			return nil, fmt.Errorf("failed to create address set %s (%v)",
				name, err)
		}
		results, err := libovsdbops.TransactAndCheck(as.nbClient, ops)
		if err != nil {
			return nil, fmt.Errorf("failed to create address set %s (%v)",
				name, err)
		}
		if len(results) == 1 {
			as.uuid = results[0].UUID.GoUUID
		} else {
			//should never happen
			return nil, fmt.Errorf("returned too many results from addressSet creation")
		}
	} else {
		// if there is already an addressSet, reuse the addressSet and return it
		as.uuid = addrset.UUID
	}

	return as, nil
}

// EnsureAddressSet ensures the address_set with the given name exists. If it exists it returns the set
// and if it does not exist, creates an empty addressSet and returns it.
func (asf *ovnAddressSetFactory) EnsureAddressSet(name string) (AddressSet, error) {
	var (
		v4set, v6set *ovnAddressSet
		err          error
	)
	ip4ASName, ip6ASName := MakeAddressSetName(name)
	if config.IPv4Mode {
		v4set, err = ensureOvnAddressSet(asf.nbClient, ip4ASName)
		if err != nil {
			return nil, err
		}
	}
	if config.IPv6Mode {
		v6set, err = ensureOvnAddressSet(asf.nbClient, ip6ASName)
		if err != nil {
			return nil, err
		}
	}

	return &ovnAddressSets{name: name, ipv4: v4set, ipv6: v6set}, nil
}

func forEachAddressSet(nbClient libovsdbclient.Client, do func(string)) error {
	addrSetList := &[]nbdb.AddressSet{}
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	err := nbClient.WhereCache(
		func(addrSet *nbdb.AddressSet) bool {
			_, exists := addrSet.ExternalIDs["name"]
			return exists
		}).List(ctx, addrSetList)
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
	_, err = libovsdbops.TransactAndCheck(nbClient, ops)
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
}

type ovnAddressSets struct {
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
	}
	uniqIPs := ipToStringSort(ips)
	addrSet := &nbdb.AddressSet{Name: hashedAddressSet(name)}
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	err := as.nbClient.Get(ctx, addrSet)
	if err != nil && err != libovsdbclient.ErrNotFound {
		return nil, err
	}

	if len(addrSet.UUID) > 0 {
		// if there is already an addressSet, reuse the addressSet and set the IPs to the slice provided
		as.uuid = addrSet.UUID
		klog.V(5).Infof("New(%s) already exists; updating IPs", asDetail(as))

		addrset := &nbdb.AddressSet{
			UUID:        as.uuid,
			Addresses:   uniqIPs,
			ExternalIDs: map[string]string{"name": as.name},
		}
		ops, err := as.nbClient.Where(addrset).Update(addrset, &addrset.Addresses)
		if err != nil {
			return nil, err
		}
		_, err = libovsdbops.TransactAndCheck(nbClient, ops)
		if err != nil {
			return nil, fmt.Errorf("failed to update address set %s (%v)",
				name, err)
		}
	} else {
		//create a new addressSet
		ops, err := nbClient.Create(&nbdb.AddressSet{
			Name:        as.hashName,
			Addresses:   uniqIPs,
			ExternalIDs: map[string]string{"name": as.name},
		})
		if err != nil {
			return nil, fmt.Errorf("failed to create address set %s (%v)",
				name, err)
		}
		results, err := libovsdbops.TransactAndCheck(nbClient, ops)
		if err != nil {
			return nil, fmt.Errorf("failed to create new address set %s (%v)",
				name, err)
		}
		if len(results) == 1 {
			as.uuid = results[0].UUID.GoUUID
		} else {
			//should never happen
			return nil, fmt.Errorf("returned too many results from addressSet creation")
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

// SetIPs replaces the address set's IP addresses with the given slice.
// NOTE: this function is not thread-safe when when run concurrently with other
// IP add/delete operations.
func (as *ovnAddressSets) SetIPs(ips []net.IP) error {
	var err error

	v4ips, v6ips := splitIPsByFamily(ips)

	if as.ipv6 != nil {
		err = as.ipv6.setIPs(v6ips)
	}
	if as.ipv4 != nil {
		err = errors.Wrapf(err, "%v", as.ipv4.setIPs(v4ips))
	}

	return err
}

func (as *ovnAddressSets) GetIPs() ([]string, []string) {
	var v4ips []string
	var v6ips []string

	if as.ipv6 != nil {
		v6ips, _ = as.ipv6.getIPs()
	}
	if as.ipv4 != nil {
		v4ips, _ = as.ipv4.getIPs()
	}

	return v4ips, v6ips
}

func (as *ovnAddressSets) AddIPs(ips []net.IP) error {
	if len(ips) == 0 {
		return nil
	}

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
	if as.ipv4 != nil {
		err := as.ipv4.destroy()
		if err != nil {
			return err
		}
	}
	if as.ipv6 != nil {
		err := as.ipv6.destroy()
		if err != nil {
			return err
		}
	}
	return nil
}

// setIP updates the given address set in OVN to be only the given IPs, disregarding
// existing state.
func (as *ovnAddressSet) setIPs(ips []net.IP) error {
	var err error
	ipStrings := make([]string, 0, len(ips))
	for _, ip := range ips {
		ipStrings = append(ipStrings, ip.String())
	}
	addrset := &nbdb.AddressSet{UUID: as.uuid, Addresses: ipStrings}
	ops, err := as.nbClient.Where(addrset).Update(addrset, &addrset.Addresses)
	if err != nil {
		return err
	}

	_, err = libovsdbops.TransactAndCheck(as.nbClient, ops)
	if err != nil {
		return fmt.Errorf("failed set ips for address set %s (%v)",
			as.name, err)
	}

	return nil
}

// getIPs gets the IPs of a given address set in OVN from libovsdb cache
func (as *ovnAddressSet) getIPs() ([]string, error) {
	addrset := &nbdb.AddressSet{Name: as.hashName}
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	err := as.nbClient.Get(ctx, addrset)
	if err != nil {
		return nil, err
	}

	return addrset.Addresses, nil
}

// addIPs appends the set of IPs to the existing address_set.
func (as *ovnAddressSet) addIPs(ips []net.IP) error {

	uniqIPs := make([]string, 0, len(ips))
	for _, ip := range ips {
		uniqIPs = append(uniqIPs, ip.String())
	}

	if len(ips) == 0 {
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
		return fmt.Errorf("failed to add IPs to address set %q (%v)",
			asDetail(as), err)
	}
	_, err = libovsdbops.TransactAndCheck(as.nbClient, ops)
	if err != nil {
		return fmt.Errorf("failed add ips to address set %s (%v)",
			as.name, err)
	}

	return nil
}

// deleteIPs removes selected IPs from the existing address_set
func (as *ovnAddressSet) deleteIPs(ips []net.IP) error {
	// dedup
	uniqIPs := make([]string, 0, len(ips))
	for _, ip := range ips {
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
	_, err = libovsdbops.TransactAndCheck(as.nbClient, ops)
	if err != nil {
		return fmt.Errorf("failed to delete ips from address set %s (%v)",
			as.name, err)
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
	_, err = libovsdbops.TransactAndCheck(as.nbClient, ops)
	if err != nil {
		return fmt.Errorf("failed to destroy address set %s (%v)",
			as.name, err)
	}

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

func ipToStringSort(ips []net.IP) []string {
	// my kingdom for a ".values()" function
	out := make([]string, 0, len(ips))
	for _, ip := range ips {
		out = append(out, ip.String())
	}
	// so the tests are predictable
	sort.Strings(out)
	return out
}
