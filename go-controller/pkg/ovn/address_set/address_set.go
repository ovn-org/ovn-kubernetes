package addressset

import (
	"context"
	"fmt"
	"net"
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

	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
)

const (
	ipv4AddressSetSuffix = "_v4"
	ipv6AddressSetSuffix = "_v6"
)

type AddressSetIterFunc func(hashedName, namespace, suffix string) error
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
	// AddIPsReturnOps returns the ops needed to add the array of IPs to the address set
	AddIPsReturnOps(ip []net.IP) ([]ovsdb.Operation, error)
	// GetIPs gets the list of v4 & v6 IPs from the address set
	GetIPs() ([]string, []string)
	// SetIPs sets the address set to the given array of addresses
	SetIPs(ip []net.IP) error
	DeleteIPs(ip []net.IP) error
	// DeleteIPsReturnOps returns the ops needed to delete the array of IPs from the address set
	DeleteIPsReturnOps(ip []net.IP) ([]ovsdb.Operation, error)
	Destroy() error
}

type ovnAddressSetFactory struct {
	util.NetNameInfo
	nbClient libovsdbclient.Client
}

// NewOvnAddressSetFactory creates a new AddressSetFactory backed by
// address set objects that execute OVN commands
func NewOvnAddressSetFactory(netNameInfo util.NetNameInfo, nbClient libovsdbclient.Client) AddressSetFactory {
	return &ovnAddressSetFactory{
		NetNameInfo: netNameInfo,
		nbClient:    nbClient,
	}
}

// ovnAddressSetFactory implements the AddressSetFactory interface
var _ AddressSetFactory = &ovnAddressSetFactory{}

// NewAddressSet returns a new address set object
func (asf *ovnAddressSetFactory) NewAddressSet(name string, ips []net.IP) (AddressSet, error) {
	res, err := newOvnAddressSets(asf, name, ips)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// ensureAddressSet ensures the address_set with the given name exists and if it does not creates an empty addressSet
func ensureOvnAddressSet(asf *ovnAddressSetFactory, name string) (*ovnAddressSet, error) {
	as := &ovnAddressSet{
		nbClient: asf.nbClient,
		name:     name,
		hashName: asf.Prefix + hashedAddressSet(name),
	}

	addrSet := &nbdb.AddressSet{Name: as.hashName, ExternalIDs: map[string]string{"name": name}}
	if asf.IsSecondary {
		addrSet.ExternalIDs["network_name"] = asf.NetName
	}
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	err := as.nbClient.Get(ctx, addrSet)
	if err != nil && err != libovsdbclient.ErrNotFound {
		return nil, fmt.Errorf("ensuring address set %s failed: %+v", name, err)
	}

	if len(addrSet.UUID) == 0 {
		ops := make([]ovsdb.Operation, 0, 2)
		timeout := types.OVSDBWaitTimeout
		ops = append(ops, ovsdb.Operation{
			Op:      ovsdb.OperationWait,
			Timeout: &timeout,
			Table:   "Address_Set",
			Where:   []ovsdb.Condition{{Column: "name", Function: ovsdb.ConditionEqual, Value: addrSet.Name}},
			Columns: []string{"name"},
			Until:   "!=",
			Rows:    []ovsdb.Row{{"name": addrSet.Name}},
		})
		// hack used to make TransactAndCheckAndSetUUIDs track the model correctly
		addrSet.UUID = libovsdbops.BuildNamedUUID()
		// create the address_set with no IPs
		op, err := as.nbClient.Create(addrSet)
		if err != nil {
			return nil, fmt.Errorf("failed to create address set %s (%v)",
				name, err)
		}
		ops = append(ops, op...)
		results, err := libovsdbops.TransactAndCheckAndSetUUIDs(as.nbClient, addrSet, ops)
		if err != nil {
			return nil, fmt.Errorf("failed to create address set %s (%v)",
				name, err)
		}
		as.uuid = addrSet.UUID

		if len(as.uuid) == 0 {
			//should never happen
			return nil, fmt.Errorf("ensureOvnAddressSet: empty UUID in address set %q, model: %#v, results: %#v",
				name, *addrSet, results)
		}
	} else {
		// if there is already an addressSet, reuse the addressSet and return it
		as.uuid = addrSet.UUID
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
		v4set, err = ensureOvnAddressSet(asf, ip4ASName)
		if err != nil {
			return nil, err
		}
	}
	if config.IPv6Mode {
		v6set, err = ensureOvnAddressSet(asf, ip6ASName)
		if err != nil {
			return nil, err
		}
	}

	return &ovnAddressSets{nbClient: asf.nbClient, name: name, ipv4: v4set, ipv6: v6set}, nil
}

func forEachAddressSet(netNameInfo util.NetNameInfo, nbClient libovsdbclient.Client, do func(string) error) error {
	addrSetList := &[]nbdb.AddressSet{}
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	err := nbClient.WhereCache(
		func(addrSet *nbdb.AddressSet) bool {
			netName, exists := addrSet.ExternalIDs["network_name"]
			if netNameInfo.IsSecondary {
				if !exists || netName != netNameInfo.NetName {
					return false
				}
			} else if exists {
				return false
			}
			_, exists = addrSet.ExternalIDs["name"]
			return exists
		}).List(ctx, addrSetList)
	if err != nil {
		return fmt.Errorf("error reading address sets: %+v", err)
	}

	var errors []error
	for _, addrSet := range *addrSetList {
		if err := do(addrSet.ExternalIDs["name"]); err != nil {
			errors = append(errors, err)
		}
	}

	if len(errors) > 0 {
		return fmt.Errorf("failed to iterate address sets: %v", utilerrors.NewAggregate(errors))
	}

	return nil
}

// ProcessEachAddressSet will pass the unhashed address set name, namespace name
// and the first suffix in the name to the 'iteratorFn' for every address_set in
// OVN. (Unhashed address set names are of the form namespaceName[.suffix1.suffix2. .suffixN])
func (asf *ovnAddressSetFactory) ProcessEachAddressSet(iteratorFn AddressSetIterFunc) error {
	processedAddressSets := sets.String{}
	return forEachAddressSet(asf.NetNameInfo, asf.nbClient, func(name string) error {
		// Remove the suffix from the address set name and normalize
		addrSetName := truncateSuffixFromAddressSet(name)
		if processedAddressSets.Has(addrSetName) {
			// We have already processed the address set. In case of dual stack we will have _v4 and _v6
			// suffixes for address sets. Since we are normalizing these two address sets through this API
			// we will process only one normalized address set name.
			return nil
		}
		processedAddressSets.Insert(addrSetName)
		names := strings.Split(addrSetName, ".")
		addrSetNamespace := names[0]
		nameSuffix := ""
		if len(names) >= 2 {
			nameSuffix = names[1]
		}
		return iteratorFn(addrSetName, addrSetNamespace, nameSuffix)
	})
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
	err := destroyAddressSet(asf.NetNameInfo, asf.nbClient, name)
	if err != nil {
		return err
	}
	ip4ASName, ip6ASName := MakeAddressSetName(name)
	err = destroyAddressSet(asf.NetNameInfo, asf.nbClient, ip4ASName)
	if err != nil {
		return err
	}
	err = destroyAddressSet(asf.NetNameInfo, asf.nbClient, ip6ASName)
	if err != nil {
		return err
	}
	return nil
}

func destroyAddressSet(netNameInfo util.NetNameInfo, nbClient libovsdbclient.Client, name string) error {
	addrset := &nbdb.AddressSet{
		Name:        netNameInfo.Prefix + hashedAddressSet(name),
		ExternalIDs: map[string]string{"name": name},
	}
	if netNameInfo.IsSecondary {
		addrset.ExternalIDs["network_name"] = netNameInfo.NetName
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
	nbClient libovsdbclient.Client
	name     string
	ipv4     *ovnAddressSet
	ipv6     *ovnAddressSet
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

func newOvnAddressSets(asf *ovnAddressSetFactory, name string, ips []net.IP) (*ovnAddressSets, error) {
	var (
		v4set, v6set *ovnAddressSet
		err          error
	)
	v4IPs, v6IPs := splitIPsByFamily(ips)

	ip4ASName, ip6ASName := MakeAddressSetName(name)
	if config.IPv4Mode {
		v4set, err = newOvnAddressSet(asf, ip4ASName, v4IPs)
		if err != nil {
			return nil, err
		}
	}
	if config.IPv6Mode {
		v6set, err = newOvnAddressSet(asf, ip6ASName, v6IPs)
		if err != nil {
			return nil, err
		}
	}
	return &ovnAddressSets{nbClient: asf.nbClient, name: name, ipv4: v4set, ipv6: v6set}, nil
}

func newOvnAddressSet(asf *ovnAddressSetFactory, name string, ips []net.IP) (*ovnAddressSet, error) {
	as := &ovnAddressSet{
		nbClient: asf.nbClient,
		name:     name,
		hashName: asf.Prefix + hashedAddressSet(name),
	}
	uniqIPs := ipsToStringUnique(ips)
	addrSet := &nbdb.AddressSet{Name: as.hashName}
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	err := as.nbClient.Get(ctx, addrSet)
	if err != nil && err != libovsdbclient.ErrNotFound {
		return nil, err
	}

	// Update address set IPs and external ID
	addrSet.Addresses = uniqIPs
	addrSet.ExternalIDs = map[string]string{"name": as.name}
	if asf.IsSecondary {
		addrSet.ExternalIDs["network_name"] = asf.NetName
	}

	if len(addrSet.UUID) > 0 {
		// if there is already an addressSet, reuse the addressSet and set the IPs to the slice provided
		as.uuid = addrSet.UUID
		klog.V(5).Infof("New(%s) already exists; updating IPs", asDetail(as))

		ops, err := as.nbClient.Where(addrSet).Update(addrSet, &addrSet.Addresses)
		if err != nil {
			return nil, err
		}
		_, err = libovsdbops.TransactAndCheck(asf.nbClient, ops)
		if err != nil {
			return nil, fmt.Errorf("failed to update address set %s (%v)",
				name, err)
		}
	} else {
		ops := make([]ovsdb.Operation, 0, 2)
		timeout := types.OVSDBWaitTimeout
		ops = append(ops, ovsdb.Operation{
			Op:      ovsdb.OperationWait,
			Timeout: &timeout,
			Table:   "Address_Set",
			Where:   []ovsdb.Condition{{Column: "name", Function: ovsdb.ConditionEqual, Value: name}},
			Columns: []string{"name"},
			Until:   "!=",
			Rows:    []ovsdb.Row{{"name": name}},
		})
		if addrSet.UUID == "" {
			// hack used to make TransactAndCheckAndSetUUIDs track the model correctly
			addrSet.UUID = libovsdbops.BuildNamedUUID()
		}
		// create a new addressSet
		op, err := asf.nbClient.Create(addrSet)
		if err != nil {
			return nil, fmt.Errorf("failed to create address set %s (%v)",
				name, err)
		}
		ops = append(ops, op...)
		results, err := libovsdbops.TransactAndCheckAndSetUUIDs(asf.nbClient, addrSet, ops)
		if err != nil {
			return nil, fmt.Errorf("failed to create new address set %s (%v)",
				name, err)
		}
		as.uuid = addrSet.UUID
		if len(as.uuid) == 0 {
			// should never happen
			return nil, fmt.Errorf("newOVNAddressSet: empty UUID in address set %q, model: %#v,"+
				" results: %#v", name, *addrSet, results)
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
	var ops []ovsdb.Operation
	var err error
	if ops, err = as.AddIPsReturnOps(ips); err != nil {
		return err
	}
	_, err = libovsdbops.TransactAndCheck(as.nbClient, ops)
	if err != nil {
		return fmt.Errorf("failed add ips to address set %s (%v)",
			as.name, err)
	}
	return nil
}

func (as *ovnAddressSets) AddIPsReturnOps(ips []net.IP) ([]ovsdb.Operation, error) {
	var ops []ovsdb.Operation
	var err error
	if len(ips) == 0 {
		return ops, nil
	}

	v4ips, v6ips := splitIPsByFamily(ips)
	var op []ovsdb.Operation
	if as.ipv6 != nil {
		if op, err = as.ipv6.addIPs(v6ips); err != nil {
			return nil, fmt.Errorf("failed to AddIPs to the v6 set: %w", err)
		}
		ops = append(ops, op...)
	}
	if as.ipv4 != nil {
		if op, err = as.ipv4.addIPs(v4ips); err != nil {
			return nil, fmt.Errorf("failed to AddIPs to the v4 set: %w", err)
		}
		ops = append(ops, op...)
	}

	return ops, nil
}

func (as *ovnAddressSets) DeleteIPs(ips []net.IP) error {
	if len(ips) == 0 {
		return nil
	}
	var ops []ovsdb.Operation
	var err error
	if ops, err = as.DeleteIPsReturnOps(ips); err != nil {
		return err
	}

	_, err = libovsdbops.TransactAndCheck(as.nbClient, ops)
	if err != nil {
		return fmt.Errorf("failed to delete ips from address set %s (%v)",
			as.name, err)
	}
	return nil
}

func (as *ovnAddressSets) DeleteIPsReturnOps(ips []net.IP) ([]ovsdb.Operation, error) {
	var ops []ovsdb.Operation
	var err error
	if len(ips) == 0 {
		return ops, nil
	}

	v4ips, v6ips := splitIPsByFamily(ips)
	var op []ovsdb.Operation
	if as.ipv6 != nil {
		if op, err = as.ipv6.deleteIPs(v6ips); err != nil {
			return nil, fmt.Errorf("failed to DeleteIPs from the v6 set: %w", err)
		}
		ops = append(ops, op...)
	}
	if as.ipv4 != nil {
		if op, err = as.ipv4.deleteIPs(v4ips); err != nil {
			return nil, fmt.Errorf("failed to DeleteIPs from the v4 set: %w", err)
		}
		ops = append(ops, op...)
	}
	return ops, nil
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
	uniqIPs := ipsToStringUnique(ips)
	addrset := &nbdb.AddressSet{UUID: as.uuid, Addresses: uniqIPs}
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
func (as *ovnAddressSet) addIPs(ips []net.IP) ([]ovsdb.Operation, error) {
	var ops []ovsdb.Operation
	var err error

	if len(ips) == 0 {
		return ops, nil
	}
	uniqIPs := ipsToStringUnique(ips)

	klog.V(5).Infof("(%s) adding IPs (%s) to address set", asDetail(as), uniqIPs)
	addrset := &nbdb.AddressSet{UUID: as.uuid}
	ops, err = as.nbClient.Where(addrset).Mutate(addrset, model.Mutation{
		Field:   &addrset.Addresses,
		Mutator: ovsdb.MutateOperationInsert,
		Value:   uniqIPs,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to add IPs to address set %q (%v)",
			asDetail(as), err)
	}

	return ops, nil
}

// deleteIPs removes selected IPs from the existing address_set
func (as *ovnAddressSet) deleteIPs(ips []net.IP) ([]ovsdb.Operation, error) {
	var ops []ovsdb.Operation
	var err error

	if len(ips) == 0 {
		return ops, nil
	}
	uniqIPs := ipsToStringUnique(ips)

	klog.V(5).Infof("(%s) deleting IP %s from address set", asDetail(as), uniqIPs)

	addrset := &nbdb.AddressSet{UUID: as.uuid}
	ops, err = as.nbClient.Where(addrset).Mutate(addrset, model.Mutation{
		Field:   &addrset.Addresses,
		Mutator: ovsdb.MutateOperationDelete,
		Value:   uniqIPs,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to delete IPs from address set %q (%v)",
			asDetail(as), err)
	}

	return ops, nil
}

func (as *ovnAddressSet) destroy() error {
	klog.V(5).Infof("Destroy(%s)", asDetail(as))
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

// Takes a slice of IPs and returns a slice with unique IPs
func ipsToStringUnique(ips []net.IP) []string {
	s := sets.NewString()
	for _, ip := range ips {
		s.Insert(ip.String())
	}
	return s.UnsortedList()
}
