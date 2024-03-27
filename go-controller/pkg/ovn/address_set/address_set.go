package addressset

import (
	"fmt"

	"github.com/pkg/errors"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/libovsdb/ovsdb"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
)

const (
	ipv4InternalID = "v4"
	ipv6InternalID = "v6"
)

type AddressSetIterFunc func(dbIDs *libovsdbops.DbObjectIDs) error

// AddressSetFactory is an interface for managing address set objects.
type AddressSetFactory interface {
	// NewAddressSet returns a new object that implements AddressSet
	// and contains the given IPs, or an error. Internally it creates
	// an address set for IPv4 and IPv6 each.
	NewAddressSet(dbIDs *libovsdbops.DbObjectIDs, ips sets.Set[string]) (AddressSet, error)
	// NewAddressSetOps returns a new object that implements AddressSet
	// and contains the given IPs, or an error. Internally it creates
	// ops to create an address set for IPv4 and IPv6 each.
	NewAddressSetOps(dbIDs *libovsdbops.DbObjectIDs, ips sets.Set[string]) (AddressSet, []ovsdb.Operation, error)
	// EnsureAddressSet makes sure that an address set object exists in ovn
	// with the given dbIDs.
	EnsureAddressSet(dbIDs *libovsdbops.DbObjectIDs) (AddressSet, error)
	// ProcessEachAddressSet calls the given function for each address set of type dbIDsType owned by given ownerController.
	ProcessEachAddressSet(ownerController string, dbIDsType *libovsdbops.ObjectIDsType, iteratorFn AddressSetIterFunc) error
	// DestroyAddressSet deletes the address sets with given dbIDs.
	DestroyAddressSet(dbIDs *libovsdbops.DbObjectIDs) error
	// GetAddressSet returns the address-set that matches the given dbIDs
	GetAddressSet(dbIDs *libovsdbops.DbObjectIDs) (AddressSet, error)
}

// AddressSet is an interface for address set objects
type AddressSet interface {
	// GetASHashNames returns the hashed name for ipv6 and ipv4 addressSets
	GetASHashNames() (string, string)
	// GetName returns the descriptive name of the address set
	GetName() string
	// AddIPs adds the array of IPs to the address set
	AddIPs(ips sets.Set[string]) error
	// AddIPsReturnOps returns the ops needed to add the array of IPs to the address set
	AddIPsReturnOps(ips sets.Set[string]) ([]ovsdb.Operation, error)
	// GetIPs gets the list of v4 & v6 IPs from the address set
	GetIPs() ([]string, []string)
	// SetIPs sets the address set to the given array of addresses
	SetIPs(ips sets.Set[string]) error
	DeleteIPs(ips sets.Set[string]) error
	// DeleteIPsReturnOps returns the ops needed to delete the array of IPs from the address set
	DeleteIPsReturnOps(ips sets.Set[string]) ([]ovsdb.Operation, error)
	Destroy() error
}

type ovnAddressSetFactory struct {
	nbClient libovsdbclient.Client
	ipv4Mode bool
	ipv6Mode bool
}

// NewOvnAddressSetFactory creates a new AddressSetFactory backed by
// address set objects that execute OVN commands
func NewOvnAddressSetFactory(nbClient libovsdbclient.Client, ipv4Mode, ipv6Mode bool) AddressSetFactory {
	return &ovnAddressSetFactory{
		nbClient: nbClient,
		ipv4Mode: ipv4Mode,
		ipv6Mode: ipv6Mode,
	}
}

// ovnAddressSetFactory implements the AddressSetFactory interface
var _ AddressSetFactory = &ovnAddressSetFactory{}

// NewAddressSet returns a new object that implements AddressSet
// and contains the given IPs, or an error. Internally it creates
// an address set for IPv4 and IPv6 each.
func (asf *ovnAddressSetFactory) NewAddressSet(dbIDs *libovsdbops.DbObjectIDs, ips sets.Set[string]) (AddressSet, error) {
	as, ops, err := asf.NewAddressSetOps(dbIDs, ips)
	if err != nil {
		return nil, err
	}
	_, err = libovsdbops.TransactAndCheck(asf.nbClient, ops)
	if err != nil {
		return nil, err
	}
	return as, nil
}

// NewAddressSetOps returns a new object that implements AddressSet
// and contains the given IPs, or an error. Internally it creates
// address set ops for IPv4 and IPv6 each.
func (asf *ovnAddressSetFactory) NewAddressSetOps(dbIDs *libovsdbops.DbObjectIDs, ips sets.Set[string]) (AddressSet, []ovsdb.Operation, error) {
	if err := asf.validateDbIDs(dbIDs); err != nil {
		return nil, nil, fmt.Errorf("failed to create address set ops: %w", err)
	}
	return asf.ensureOvnAddressSetsOps(ips, dbIDs, true)
}

// EnsureAddressSet makes sure that an address set object exists in ovn
// with the given dbIDs.
func (asf *ovnAddressSetFactory) EnsureAddressSet(dbIDs *libovsdbops.DbObjectIDs) (AddressSet, error) {
	if err := asf.validateDbIDs(dbIDs); err != nil {
		return nil, fmt.Errorf("failed to ensure address set: %w", err)
	}
	as, ops, err := asf.ensureOvnAddressSetsOps(nil, dbIDs, false)
	if err != nil {
		return nil, err
	}
	_, err = libovsdbops.TransactAndCheck(asf.nbClient, ops)
	if err != nil {
		return nil, err
	}
	return as, nil
}

func getDbIDsWithIPFamily(dbIDs *libovsdbops.DbObjectIDs, ipFamily string) *libovsdbops.DbObjectIDs {
	return dbIDs.AddIDs(map[libovsdbops.ExternalIDKey]string{libovsdbops.AddressSetIPFamilyKey: ipFamily})
}

// GetAddressSet returns the address-set that matches the given dbIDs
func (asf *ovnAddressSetFactory) GetAddressSet(dbIDs *libovsdbops.DbObjectIDs) (AddressSet, error) {
	if err := asf.validateDbIDs(dbIDs); err != nil {
		return nil, fmt.Errorf("failed to get address set: %w", err)
	}
	var (
		v4set, v6set *ovnAddressSet
	)
	p := libovsdbops.GetPredicate[*nbdb.AddressSet](dbIDs, nil)
	addrSetList, err := libovsdbops.FindAddressSetsWithPredicate(asf.nbClient, p)
	if err != nil {
		return nil, fmt.Errorf("error getting address sets: %w", err)
	}
	for i := range addrSetList {
		addrSet := addrSetList[i]
		if addrSet.ExternalIDs[libovsdbops.AddressSetIPFamilyKey.String()] == ipv4InternalID {
			v4set = asf.newOvnAddressSet(addrSet)
		}
		if addrSet.ExternalIDs[libovsdbops.AddressSetIPFamilyKey.String()] == ipv6InternalID {
			v6set = asf.newOvnAddressSet(addrSet)
		}
	}
	return asf.newOvnAddressSets(v4set, v6set, dbIDs), nil
}

// forEachAddressSet executes a do function on each address set owned by ovnAddressSetFactory.ControllerName
func (asf *ovnAddressSetFactory) forEachAddressSet(ownerController string, dbIDsType *libovsdbops.ObjectIDsType,
	do func(*nbdb.AddressSet) error) error {
	predIDs := libovsdbops.NewDbObjectIDs(dbIDsType, ownerController, nil)
	p := libovsdbops.GetPredicate[*nbdb.AddressSet](predIDs, nil)
	addrSetList, err := libovsdbops.FindAddressSetsWithPredicate(asf.nbClient, p)
	if err != nil {
		return fmt.Errorf("error reading address sets: %+v", err)
	}

	var errs []error
	for _, addrSet := range addrSetList {
		if err = do(addrSet); err != nil {
			errs = append(errs, err)
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("failed to iterate address sets: %v", utilerrors.NewAggregate(errs))
	}

	return nil
}

// ProcessEachAddressSet calls the given function for each address set of type dbIDsType owned by given ownerController.
func (asf *ovnAddressSetFactory) ProcessEachAddressSet(ownerController string, dbIDsType *libovsdbops.ObjectIDsType,
	iteratorFn AddressSetIterFunc) error {
	processedAddressSets := sets.Set[string]{}
	return asf.forEachAddressSet(ownerController, dbIDsType, func(as *nbdb.AddressSet) error {
		dbIDs, err := libovsdbops.NewDbObjectIDsFromExternalIDs(dbIDsType, as.ExternalIDs)
		if err != nil {
			return fmt.Errorf("failed to get objectIDs for %+v address set from ExternalIDs: %w", as, err)
		}
		// remove ipFamily to process address set only once
		dbIDsWithoutIPFam := dbIDs.RemoveIDs(libovsdbops.AddressSetIPFamilyKey)
		nameWithoutIPFam := getOvnAddressSetsName(dbIDsWithoutIPFam)
		if processedAddressSets.Has(nameWithoutIPFam) {
			// We have already processed the address set. In case of dual stack we will have _v4 and _v6
			// suffixes for address sets. Since we are normalizing these two address sets through this API
			// we will process only one normalized address set name.
			return nil
		}
		processedAddressSets.Insert(nameWithoutIPFam)
		return iteratorFn(dbIDsWithoutIPFam)
	})
}

// DestroyAddressSet deletes the address sets with given dbIDs.
func (asf *ovnAddressSetFactory) DestroyAddressSet(dbIDs *libovsdbops.DbObjectIDs) error {
	if err := asf.validateDbIDs(dbIDs); err != nil {
		return fmt.Errorf("failed to destroy address set: %w", err)
	}
	asv4 := &nbdb.AddressSet{
		Name: buildAddressSet(dbIDs, ipv4InternalID).Name,
	}
	asv6 := &nbdb.AddressSet{
		Name: buildAddressSet(dbIDs, ipv6InternalID).Name,
	}
	err := libovsdbops.DeleteAddressSets(asf.nbClient, asv4, asv6)
	if err != nil {
		return fmt.Errorf("failed to delete address sets %s: %v", getOvnAddressSetsName(dbIDs), err)
	}
	return nil
}

// if updateAS is false, ips will be ignored, only empty address sets will be created or existing address sets will
// be returned
func (asf *ovnAddressSetFactory) ensureOvnAddressSetsOps(ips sets.Set[string], dbIDs *libovsdbops.DbObjectIDs,
	updateAS bool) (*ovnAddressSets, []ovsdb.Operation, error) {
	var (
		v4set, v6set *ovnAddressSet
		v4IPs, v6IPs sets.Set[string]
		err          error
	)
	if ips != nil {
		v4IPs, v6IPs = splitIPsByFamily(ips)
	}
	var ops []ovsdb.Operation
	if asf.ipv4Mode {
		v4set, ops, err = asf.ensureOvnAddressSetOps(v4IPs, dbIDs, ipv4InternalID, updateAS, ops)
		if err != nil {
			return nil, nil, err
		}
	}
	if asf.ipv6Mode {
		v6set, ops, err = asf.ensureOvnAddressSetOps(v6IPs, dbIDs, ipv6InternalID, updateAS, ops)
		if err != nil {
			return nil, nil, err
		}
	}
	return asf.newOvnAddressSets(v4set, v6set, dbIDs), ops, nil
}

func buildAddressSet(dbIDs *libovsdbops.DbObjectIDs, ipFamily string) *nbdb.AddressSet {
	dbIDsWithIPFam := getDbIDsWithIPFamily(dbIDs, ipFamily)
	externalIDs := dbIDsWithIPFam.GetExternalIDs()
	name := externalIDs[libovsdbops.PrimaryIDKey.String()]
	as := &nbdb.AddressSet{
		Name:        hashedAddressSet(name),
		ExternalIDs: externalIDs,
	}
	return as
}

func (asf *ovnAddressSetFactory) newOvnAddressSet(addrSet *nbdb.AddressSet) *ovnAddressSet {
	return &ovnAddressSet{
		nbClient: asf.nbClient,
		name:     addrSet.ExternalIDs[libovsdbops.PrimaryIDKey.String()],
		hashName: addrSet.Name,
		uuid:     addrSet.UUID,
	}
}

func (asf *ovnAddressSetFactory) ensureOvnAddressSetOps(ips sets.Set[string], dbIDs *libovsdbops.DbObjectIDs,
	ipFamily string, updateAS bool, ops []ovsdb.Operation) (*ovnAddressSet, []ovsdb.Operation, error) {
	addrSet := buildAddressSet(dbIDs, ipFamily)
	var err error
	if updateAS {
		// overwrite ips, EnsureAddressSet doesn't do that
		addrSet.Addresses = ips.UnsortedList()
		ops, err = libovsdbops.CreateOrUpdateAddressSetsOps(asf.nbClient, ops, addrSet)
	} else {
		ops, err = libovsdbops.CreateAddressSetsOps(asf.nbClient, ops, addrSet)
	}

	// UUID should always be set if no error, check anyway
	if err != nil {
		// NOTE: While ovsdb transactions get serialized by libovsdb, the decision to create vs. update
		// the address set takes place before that serialization is done. Because of that, it is feasible
		// that one of the go threads attempting to call this routine at the same time will fail.
		// This is described in https://bugzilla.redhat.com/show_bug.cgi?id=2108026 . While we could
		// handle that failure here by retrying, a higher level retry (see retry_obj.go) is already
		// present in the codepath, so no additional handling for that condition has been added here.
		return nil, nil, fmt.Errorf("failed to create or update address set ops %+v: %w", addrSet, err)
	}
	as := asf.newOvnAddressSet(addrSet)
	klog.V(5).Infof("New(%s) with %v", asDetail(as), ips)
	return as, ops, nil
}

func (asf *ovnAddressSetFactory) validateDbIDs(dbIDs *libovsdbops.DbObjectIDs) error {
	unsetKeys := dbIDs.GetUnsetKeys()
	if len(unsetKeys) == 1 && unsetKeys[0] == libovsdbops.AddressSetIPFamilyKey {
		return nil
	}
	return fmt.Errorf("wrong set of keys is unset %v", unsetKeys)
}

// GetHashNamesForAS returns hashed address set names for given dbIDs for both ip families.
// Can be used to cleanup e.g. address set references if the address set was deleted.
func GetHashNamesForAS(dbIDs *libovsdbops.DbObjectIDs) (string, string) {
	return buildAddressSet(dbIDs, ipv4InternalID).Name,
		buildAddressSet(dbIDs, ipv6InternalID).Name
}

// GetTestDbAddrSets returns nbdb.AddressSet objects both for ipv4 and ipv6, regardless of current config.
// May only be used for testing.
func GetTestDbAddrSets(dbIDs *libovsdbops.DbObjectIDs, ips sets.Set[string]) (*nbdb.AddressSet, *nbdb.AddressSet) {
	var v4set, v6set *nbdb.AddressSet
	v4IPs, v6IPs := splitIPsByFamily(ips)
	// v4 address set
	v4set = buildAddressSet(dbIDs, ipv4InternalID)
	v4set.Addresses = v4IPs.UnsortedList()
	v4set.UUID = v4set.Name + "-UUID"
	// v6 address set
	v6set = buildAddressSet(dbIDs, ipv6InternalID)
	v6set.Addresses = v6IPs.UnsortedList()
	v6set.UUID = v6set.Name + "-UUID"
	return v4set, v6set
}

// ovnAddressSet is ipFamily-specific address set
type ovnAddressSet struct {
	nbClient libovsdbclient.Client
	// name is based on dbIDs and ipFamily
	name string
	// hashName = hashedAddressSet(name) and is set to AddressSet.Name
	hashName string
	uuid     string
}

// getOvnAddressSetsName returns the name for ovnAddressSets, that contains both ipv4 and ipv6 address sets,
// therefore the name should not include ipFamily information. DbObjectIDs without ipFamily is what is used by
// the AddressSetFactory functions.
func getOvnAddressSetsName(dbIDs *libovsdbops.DbObjectIDs) string {
	if dbIDs.GetObjectID(libovsdbops.AddressSetIPFamilyKey) != "" {
		dbIDs = dbIDs.RemoveIDs(libovsdbops.AddressSetIPFamilyKey)
	}
	return dbIDs.String()
}

// ovnAddressSets is an abstraction for ipv4 and ipv6 address sets
type ovnAddressSets struct {
	nbClient libovsdbclient.Client
	// name is based on dbIDs without ipFamily
	name string
	ipv4 *ovnAddressSet
	ipv6 *ovnAddressSet
}

// ovnAddressSets implements the AddressSet interface
var _ AddressSet = &ovnAddressSets{}

func (asf *ovnAddressSetFactory) newOvnAddressSets(v4set, v6set *ovnAddressSet, dbIDs *libovsdbops.DbObjectIDs) *ovnAddressSets {
	return &ovnAddressSets{nbClient: asf.nbClient, name: getOvnAddressSetsName(dbIDs), ipv4: v4set, ipv6: v6set}
}

// hash the provided input to make it a valid ovnAddressSet name.
func hashedAddressSet(s string) string {
	return util.HashForOVN(s)
}

func asDetail(as *ovnAddressSet) string {
	return fmt.Sprintf("%s/%s/%s", as.uuid, as.name, as.hashName)
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
func (as *ovnAddressSets) SetIPs(ips sets.Set[string]) error {
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

func (as *ovnAddressSets) AddIPs(ips sets.Set[string]) error {
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

func (as *ovnAddressSets) AddIPsReturnOps(ips sets.Set[string]) ([]ovsdb.Operation, error) {
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

func (as *ovnAddressSets) DeleteIPs(ips sets.Set[string]) error {
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

func (as *ovnAddressSets) DeleteIPsReturnOps(ips sets.Set[string]) ([]ovsdb.Operation, error) {
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
func (as *ovnAddressSet) setIPs(ips sets.Set[string]) error {
	addrset := nbdb.AddressSet{
		UUID:      as.uuid,
		Name:      as.hashName,
		Addresses: ips.UnsortedList(),
	}
	err := libovsdbops.UpdateAddressSetsIPs(as.nbClient, &addrset)
	if err != nil {
		return fmt.Errorf("failed to update address set IPs %+v: %v", addrset, err)
	}

	return nil
}

// getIPs gets the IPs of a given address set in OVN from libovsdb cache
func (as *ovnAddressSet) getIPs() ([]string, error) {
	addrset := &nbdb.AddressSet{
		UUID: as.uuid,
		Name: as.hashName,
	}
	addrset, err := libovsdbops.GetAddressSet(as.nbClient, addrset)
	if err != nil {
		return nil, err
	}

	return addrset.Addresses, nil
}

// addIPs appends the set of IPs to the existing address_set.
func (as *ovnAddressSet) addIPs(ips sets.Set[string]) ([]ovsdb.Operation, error) {
	if len(ips) == 0 {
		return nil, nil
	}

	if as.hasIPs(ips.UnsortedList()...) {
		return nil, nil
	}

	klog.V(5).Infof("(%s) adding IPs (%v) to address set", asDetail(as), ips.UnsortedList())

	addrset := nbdb.AddressSet{
		UUID: as.uuid,
		Name: as.hashName,
	}
	ops, err := libovsdbops.AddIPsToAddressSetOps(as.nbClient, nil, &addrset, ips.UnsortedList()...)
	if err != nil {
		return nil, fmt.Errorf("failed to add IPs %v to address set %+v: %v", ips.UnsortedList(), addrset, err)
	}

	return ops, nil
}

// hasIPs returns true if an address set contains all given IPs
func (as *ovnAddressSet) hasIPs(ips ...string) bool {
	existingIPs, err := as.getIPs()
	if err != nil {
		return false
	}

	if len(existingIPs) == 0 {
		return false
	}

	return sets.NewString(existingIPs...).HasAll(ips...)
}

// deleteIPs removes selected IPs from the existing address_set
func (as *ovnAddressSet) deleteIPs(ips sets.Set[string]) ([]ovsdb.Operation, error) {
	if len(ips) == 0 {
		return nil, nil
	}
	klog.V(5).Infof("(%s) deleting IP %+v from address set", asDetail(as), ips.UnsortedList())

	addrset := nbdb.AddressSet{
		UUID: as.uuid,
		Name: as.hashName,
	}
	ops, err := libovsdbops.DeleteIPsFromAddressSetOps(as.nbClient, nil, &addrset, ips.UnsortedList()...)
	if err != nil {
		return nil, fmt.Errorf("failed to delete IPs %+v to address set %+v: %v", ips.UnsortedList(), addrset, err)
	}

	return ops, nil
}

func (as *ovnAddressSet) destroy() error {
	klog.V(5).Infof("Destroy(%s)", asDetail(as))
	addrset := nbdb.AddressSet{
		UUID: as.uuid,
		Name: as.hashName,
	}
	err := libovsdbops.DeleteAddressSets(as.nbClient, &addrset)
	if err != nil {
		return fmt.Errorf("failed to delete address set %+v: %v", addrset, err)
	}

	return nil
}

// splitIPsByFamily takes a slice of IPs or CIDR strings and returns two slices, with
// v4 and v6 addresses collated accordingly.
func splitIPsByFamily(ips sets.Set[string]) (v4, v6 sets.Set[string]) {
	v4 = sets.New[string]()
	v6 = sets.New[string]()
	for _, ip := range ips.UnsortedList() {
		if utilnet.IsIPv6String(ip) || utilnet.IsIPv6CIDRString(ip) {
			v6.Insert(ip)
		} else {
			v4.Insert(ip)
		}
	}
	return
}
