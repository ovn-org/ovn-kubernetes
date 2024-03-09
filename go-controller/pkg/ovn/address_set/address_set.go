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

// DISCLAIMER: OVN AddressSets support adding addresses of types
// IPs, CIDRs and MACs. This util supports adding types IPs and
// CIDRs currently. If you want to add MAC support the ipFamily
// split utils need to be updated to accommodate that.

const (
	ipv4InternalID = "v4"
	ipv6InternalID = "v6"
)

type AddressSetIterFunc func(dbIDs *libovsdbops.DbObjectIDs) error

// AddressSetFactory is an interface for managing address set objects.
type AddressSetFactory interface {
	// NewAddressSet returns a new object that implements AddressSet
	// and contains the given slice of addresses which today can be string
	// representations of net.IPs or net.IPNets, or an error. Internally it creates
	// an address set for v4 and v6 families each.
	NewAddressSet(dbIDs *libovsdbops.DbObjectIDs, addresses []string) (AddressSet, error)
	// NewAddressSetOps returns a new object that implements AddressSet
	// and contains the given slice of addresses which today can be string
	// representations of net.IPs or net.IPNets, or an error. Internally it creates
	// ops to create an address set for for v4 and v6 families each.
	NewAddressSetOps(dbIDs *libovsdbops.DbObjectIDs, addresses []string) (AddressSet, []ovsdb.Operation, error)
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
	// GetASHashNames returns the hashed name for v6 and v4 addressSets
	GetASHashNames() (string, string)
	// GetName returns the descriptive name of the address set
	GetName() string
	// AddAddresses adds the slice of addresses to the address set
	AddAddresses(addresses []string) error
	// AddAddressesReturnOps returns the ops needed to add the slice of addresses to the address set
	AddAddressesReturnOps(addresses []string) ([]ovsdb.Operation, error)
	// GetAddresses gets the list of v4 & v6 addresses from the address set
	GetAddresses() ([]string, []string)
	// SetAddresses sets the address set to the given slice of addresses
	SetAddresses(addresses []string) error
	// DeleteAddresses deletes the provided slice of addresses from the address set
	DeleteAddresses(addresses []string) error
	// DeleteAddressesReturnOps returns the ops needed to delete the slice of addresses from the address set
	DeleteAddressesReturnOps(addresses []string) ([]ovsdb.Operation, error)
	// Destroy deletes the entire address set
	Destroy() error
}

type ovnAddressSetFactory struct {
	nbClient libovsdbclient.Client
	v4Mode   bool
	v6Mode   bool
}

// NewOvnAddressSetFactory creates a new AddressSetFactory backed by
// address set objects that execute OVN commands
func NewOvnAddressSetFactory(nbClient libovsdbclient.Client, v4Mode, v6Mode bool) AddressSetFactory {
	return &ovnAddressSetFactory{
		nbClient: nbClient,
		v4Mode:   v4Mode,
		v6Mode:   v6Mode,
	}
}

// ovnAddressSetFactory implements the AddressSetFactory interface
var _ AddressSetFactory = &ovnAddressSetFactory{}

// NewAddressSet returns a new object that implements AddressSet
// and contains the given addresses, or an error. Internally it creates
// an address set for v4Addresses and v6Addresses each.
func (asf *ovnAddressSetFactory) NewAddressSet(dbIDs *libovsdbops.DbObjectIDs, addresses []string) (AddressSet, error) {
	as, ops, err := asf.NewAddressSetOps(dbIDs, addresses)
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
// and contains the given addresses, or an error. Internally it creates
// address set ops for v4Addresses and v6Addresses each.
func (asf *ovnAddressSetFactory) NewAddressSetOps(dbIDs *libovsdbops.DbObjectIDs, addresses []string) (AddressSet, []ovsdb.Operation, error) {
	if err := asf.validateDbIDs(dbIDs); err != nil {
		return nil, nil, fmt.Errorf("failed to create address set ops: %w", err)
	}
	return asf.ensureOvnAddressSetsOps(addresses, dbIDs, true)
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

// if updateAS is false, addresses will be ignored, only empty address sets will be created or existing address sets will
// be returned
func (asf *ovnAddressSetFactory) ensureOvnAddressSetsOps(addresses []string, dbIDs *libovsdbops.DbObjectIDs,
	updateAS bool) (*ovnAddressSets, []ovsdb.Operation, error) {
	var (
		v4set, v6set             *ovnAddressSet
		v4Addresses, v6Addresses []string
		err                      error
	)
	if addresses != nil {
		v4Addresses, v6Addresses = splitAddressesByFamily(addresses)
	}
	var ops []ovsdb.Operation
	if asf.v4Mode {
		v4set, ops, err = asf.ensureOvnAddressSetOps(v4Addresses, dbIDs, ipv4InternalID, updateAS, ops)
		if err != nil {
			return nil, nil, err
		}
	}
	if asf.v6Mode {
		v6set, ops, err = asf.ensureOvnAddressSetOps(v6Addresses, dbIDs, ipv6InternalID, updateAS, ops)
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

func (asf *ovnAddressSetFactory) ensureOvnAddressSetOps(addresses []string, dbIDs *libovsdbops.DbObjectIDs,
	ipFamily string, updateAS bool, ops []ovsdb.Operation) (*ovnAddressSet, []ovsdb.Operation, error) {
	addrSet := buildAddressSet(dbIDs, ipFamily)
	var err error
	if updateAS {
		// overwrite addresses, EnsureAddressSet doesn't do that
		uniqAddresses := getUniqueAddresses(addresses)
		addrSet.Addresses = uniqAddresses
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
	klog.V(5).Infof("New(%s) with %v", asDetail(as), addresses)
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
func GetTestDbAddrSets(dbIDs *libovsdbops.DbObjectIDs, addresses []string) (*nbdb.AddressSet, *nbdb.AddressSet) {
	var v4set, v6set *nbdb.AddressSet
	v4Addresses, v6Addresses := splitAddressesByFamily(addresses)
	// v4 address set
	v4set = buildAddressSet(dbIDs, ipv4InternalID)
	uniqAddresses := getUniqueAddresses(v4Addresses)
	v4set.Addresses = uniqAddresses
	v4set.UUID = v4set.Name + "-UUID"
	// v6 address set
	v6set = buildAddressSet(dbIDs, ipv6InternalID)
	uniqAddresses = getUniqueAddresses(v6Addresses)
	v6set.Addresses = uniqAddresses
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
	v4   *ovnAddressSet
	v6   *ovnAddressSet
}

// ovnAddressSets implements the AddressSet interface
var _ AddressSet = &ovnAddressSets{}

func (asf *ovnAddressSetFactory) newOvnAddressSets(v4set, v6set *ovnAddressSet, dbIDs *libovsdbops.DbObjectIDs) *ovnAddressSets {
	return &ovnAddressSets{nbClient: asf.nbClient, name: getOvnAddressSetsName(dbIDs), v4: v4set, v6: v6set}
}

// hash the provided input to make it a valid ovnAddressSet name.
func hashedAddressSet(s string) string {
	return util.HashForOVN(s)
}

func asDetail(as *ovnAddressSet) string {
	return fmt.Sprintf("%s/%s/%s", as.uuid, as.name, as.hashName)
}

func (as *ovnAddressSets) GetASHashNames() (string, string) {
	var v4AS string
	var v6AS string
	if as.v4 != nil {
		v4AS = as.v4.hashName
	}
	if as.v6 != nil {
		v6AS = as.v6.hashName
	}
	return v4AS, v6AS
}

func (as *ovnAddressSets) GetName() string {
	return as.name
}

// SetAddresses replaces the address set's addresses with the given slice.
// NOTE: this function is not thread-safe when when run concurrently with other
// IP add/delete operations.
func (as *ovnAddressSets) SetAddresses(addresses []string) error {
	var err error

	v4addresses, v6addresses := splitAddressesByFamily(addresses)

	if as.v6 != nil {
		err = as.v6.setAddresses(v6addresses)
	}
	if as.v4 != nil {
		err = errors.Wrapf(err, "%v", as.v4.setAddresses(v4addresses))
	}

	return err
}

func (as *ovnAddressSets) GetAddresses() ([]string, []string) {
	var v4addresses []string
	var v6addresses []string

	if as.v6 != nil {
		v6addresses, _ = as.v6.getAddresses()
	}
	if as.v4 != nil {
		v4addresses, _ = as.v4.getAddresses()
	}

	return v4addresses, v6addresses
}

func (as *ovnAddressSets) AddAddresses(addresses []string) error {
	if len(addresses) == 0 {
		return nil
	}
	var ops []ovsdb.Operation
	var err error
	if ops, err = as.AddAddressesReturnOps(addresses); err != nil {
		return err
	}
	_, err = libovsdbops.TransactAndCheck(as.nbClient, ops)
	if err != nil {
		return fmt.Errorf("failed add addresses to address set %s (%v)",
			as.name, err)
	}
	return nil
}

func (as *ovnAddressSets) AddAddressesReturnOps(addresses []string) ([]ovsdb.Operation, error) {
	var ops []ovsdb.Operation
	var err error
	if len(addresses) == 0 {
		return ops, nil
	}

	v4addresses, v6addresses := splitAddressesByFamily(addresses)
	var op []ovsdb.Operation
	if as.v6 != nil {
		if op, err = as.v6.addAddresses(v6addresses); err != nil {
			return nil, fmt.Errorf("failed to Add Addresses to the v6 set: %w", err)
		}
		ops = append(ops, op...)
	}
	if as.v4 != nil {
		if op, err = as.v4.addAddresses(v4addresses); err != nil {
			return nil, fmt.Errorf("failed to Add Addresses to the v4 set: %w", err)
		}
		ops = append(ops, op...)
	}

	return ops, nil
}

func (as *ovnAddressSets) DeleteAddresses(addresses []string) error {
	if len(addresses) == 0 {
		return nil
	}
	var ops []ovsdb.Operation
	var err error
	if ops, err = as.DeleteAddressesReturnOps(addresses); err != nil {
		return err
	}

	_, err = libovsdbops.TransactAndCheck(as.nbClient, ops)
	if err != nil {
		return fmt.Errorf("failed to delete addresses from address set %s (%v)",
			as.name, err)
	}
	return nil
}

func (as *ovnAddressSets) DeleteAddressesReturnOps(addresses []string) ([]ovsdb.Operation, error) {
	var ops []ovsdb.Operation
	var err error
	if len(addresses) == 0 {
		return ops, nil
	}

	v4addresses, v6addresses := splitAddressesByFamily(addresses)
	var op []ovsdb.Operation
	if as.v6 != nil {
		if op, err = as.v6.deleteAddresses(v6addresses); err != nil {
			return nil, fmt.Errorf("failed to Delete Addresses from the v6 set: %w", err)
		}
		ops = append(ops, op...)
	}
	if as.v4 != nil {
		if op, err = as.v4.deleteAddresses(v4addresses); err != nil {
			return nil, fmt.Errorf("failed to Delete Addresses from the v4 set: %w", err)
		}
		ops = append(ops, op...)
	}
	return ops, nil
}

func (as *ovnAddressSets) Destroy() error {
	if as.v4 != nil {
		err := as.v4.destroy()
		if err != nil {
			return err
		}
	}
	if as.v6 != nil {
		err := as.v6.destroy()
		if err != nil {
			return err
		}
	}
	return nil
}

// setAddresses updates the given address set in OVN to be only the
// given addresses, disregarding existing state.
func (as *ovnAddressSet) setAddresses(addresses []string) error {
	uniqAddresses := getUniqueAddresses(addresses)
	addrset := nbdb.AddressSet{
		UUID:      as.uuid,
		Name:      as.hashName,
		Addresses: uniqAddresses,
	}
	err := libovsdbops.UpdateAddressSetsAddresses(as.nbClient, &addrset)
	if err != nil {
		return fmt.Errorf("failed to update address set addresses %+v: %v", addrset, err)
	}

	return nil
}

// getAddresses gets the addresses of a given address set in OVN from libovsdb cache
func (as *ovnAddressSet) getAddresses() ([]string, error) {
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

// addAddresses appends the set of addresses to the existing address_set.
func (as *ovnAddressSet) addAddresses(addresses []string) ([]ovsdb.Operation, error) {
	if len(addresses) == 0 {
		return nil, nil
	}

	uniqAddresses := getUniqueAddresses(addresses)

	if as.hasAddresses(uniqAddresses...) {
		return nil, nil
	}

	klog.V(5).Infof("(%s) adding Addresses (%s) to address set", asDetail(as), uniqAddresses)

	addrset := nbdb.AddressSet{
		UUID: as.uuid,
		Name: as.hashName,
	}
	ops, err := libovsdbops.AddAddressesToAddressSetOps(as.nbClient, nil, &addrset, uniqAddresses...)
	if err != nil {
		return nil, fmt.Errorf("failed to add addresses %v to address set %+v: %v", uniqAddresses, addrset, err)
	}

	return ops, nil
}

// hasAddresses returns true if an address set contains all given Addresses
func (as *ovnAddressSet) hasAddresses(addresses ...string) bool {
	existingAddresses, err := as.getAddresses()
	if err != nil {
		return false
	}

	if len(existingAddresses) == 0 {
		return false
	}

	return sets.NewString(existingAddresses...).HasAll(addresses...)
}

// deleteAddresses removes selected addresses from the existing address_set
func (as *ovnAddressSet) deleteAddresses(addresses []string) ([]ovsdb.Operation, error) {
	if len(addresses) == 0 {
		return nil, nil
	}

	uniqAddresses := getUniqueAddresses(addresses)

	klog.V(5).Infof("(%s) deleting addresses %v from address set", asDetail(as), uniqAddresses)

	addrset := nbdb.AddressSet{
		UUID: as.uuid,
		Name: as.hashName,
	}
	ops, err := libovsdbops.DeleteAddressesFromAddressSetOps(as.nbClient, nil, &addrset, uniqAddresses...)
	if err != nil {
		return nil, fmt.Errorf("failed to delete addresses %v from address set %+v: %v", uniqAddresses, addrset, err)
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

// splitAddressesByFamily takes a slice of IPs or CIDR string representations and returns two slices, with
// v4 and v6 addresses collated accordingly.
// NOTE: We don't support Ethernet type addresses split today since we don't use them
// This might change in the future where we need to decide if they will go into v4 or v6
func splitAddressesByFamily(addresses []string) (v4, v6 []string) {
	for _, address := range addresses {
		if utilnet.IsIPv6String(address) || utilnet.IsIPv6CIDRString(address) {
			v6 = append(v6, address)
		} else {
			v4 = append(v4, address)
		}
	}
	return
}

// Takes a slice of addresses and returns a slice with unique address strings
func getUniqueAddresses(addresses []string) []string {
	s := sets.New[string]()
	for _, address := range addresses {
		s.Insert(address)
	}
	return s.UnsortedList()
}
