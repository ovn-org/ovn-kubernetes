package addressset

import (
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/onsi/gomega"

	"github.com/ovn-org/libovsdb/ovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"

	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
)

func NewFakeAddressSetFactory(controllerName string) *FakeAddressSetFactory {
	return &FakeAddressSetFactory{
		ControllerName: controllerName,
		asf:            &ovnAddressSetFactory{},
		sets:           make(map[string]*fakeAddressSets),
	}
}

type FakeAddressSetFactory struct {
	// ControllerName is stored here for convenience, it is used to build dbIDs for fake-only methods like
	// AddressSetExists, EventuallyExpectAddressSet, etc.
	ControllerName string
	asf            *ovnAddressSetFactory
	sync.Mutex
	// maps address set name to object
	sets                map[string]*fakeAddressSets
	errOnNextNewAddrSet bool
}

// fakeFactory implements the AddressSetFactory interface
var _ AddressSetFactory = &FakeAddressSetFactory{}

const FakeASFError = "fake asf error"

// ErrOnNextNewASCall will make FakeAddressSetFactory return FakeASFError on the next NewAddressSet call
func (f *FakeAddressSetFactory) ErrOnNextNewASCall() {
	f.errOnNextNewAddrSet = true
}

// NewAddressSet returns a new address set object
func (f *FakeAddressSetFactory) NewAddressSet(dbIDs *libovsdbops.DbObjectIDs, addresses []string) (AddressSet, error) {
	if f.errOnNextNewAddrSet {
		f.errOnNextNewAddrSet = false
		return nil, fmt.Errorf(FakeASFError)
	}
	if err := f.asf.validateDbIDs(dbIDs); err != nil {
		return nil, fmt.Errorf("failed to create address set: %w", err)
	}
	f.Lock()
	defer f.Unlock()
	name := getOvnAddressSetsName(dbIDs)

	_, ok := f.sets[name]
	gomega.Expect(ok).To(gomega.BeFalse(), fmt.Sprintf("new address set %s already exists", name))
	set, err := f.newFakeAddressSets(addresses, dbIDs, f.removeAddressSet)
	if err != nil {
		return nil, err
	}
	f.sets[name] = set
	return set, nil
}

// NewAddressSetOps returns a new address set object
func (f *FakeAddressSetFactory) NewAddressSetOps(dbIDs *libovsdbops.DbObjectIDs, addresses []string) (AddressSet, []ovsdb.Operation, error) {
	if f.errOnNextNewAddrSet {
		f.errOnNextNewAddrSet = false
		return nil, nil, fmt.Errorf(FakeASFError)
	}
	if err := f.asf.validateDbIDs(dbIDs); err != nil {
		return nil, nil, fmt.Errorf("failed to create address set: %w", err)
	}
	f.Lock()
	defer f.Unlock()
	name := getOvnAddressSetsName(dbIDs)

	_, ok := f.sets[name]
	gomega.Expect(ok).To(gomega.BeFalse(), fmt.Sprintf("new address set %s already exists", name))
	set, err := f.newFakeAddressSets(addresses, dbIDs, f.removeAddressSet)
	if err != nil {
		return nil, nil, err
	}
	f.sets[name] = set
	return set, nil, nil
}

// EnsureAddressSet returns set object
func (f *FakeAddressSetFactory) EnsureAddressSet(dbIDs *libovsdbops.DbObjectIDs) (AddressSet, error) {
	if err := f.asf.validateDbIDs(dbIDs); err != nil {
		return nil, fmt.Errorf("failed to ensure address set: %w", err)
	}
	f.Lock()
	defer f.Unlock()
	name := getOvnAddressSetsName(dbIDs)
	set, ok := f.sets[name]
	if ok {
		return set, nil
	}
	set, err := f.newFakeAddressSets([]string{}, dbIDs, f.removeAddressSet)
	if err != nil {
		return nil, err
	}
	f.sets[name] = set
	return set, nil
}

// GetAddressSet returns set object
func (f *FakeAddressSetFactory) GetAddressSet(dbIDs *libovsdbops.DbObjectIDs) (AddressSet, error) {
	if err := f.asf.validateDbIDs(dbIDs); err != nil {
		return nil, fmt.Errorf("failed to get address set: %w", err)
	}
	f.Lock()
	defer f.Unlock()
	name := getOvnAddressSetsName(dbIDs)
	set, ok := f.sets[name]
	if ok {
		return set, nil
	}
	return nil, fmt.Errorf("error fetching address set")
}

func (f *FakeAddressSetFactory) ProcessEachAddressSet(ownerController string, indexT *libovsdbops.ObjectIDsType, iteratorFn AddressSetIterFunc) error {
	f.Lock()
	asNames := map[string]*libovsdbops.DbObjectIDs{}
	for _, set := range f.sets {
		if !set.dbIDs.HasSameOwner(ownerController, indexT) {
			continue
		}
		// set.dbIDs doesn't have ip family
		addrSetName := getOvnAddressSetsName(set.dbIDs)
		if _, ok := asNames[addrSetName]; ok {
			continue
		}
		asNames[addrSetName] = set.dbIDs
	}
	f.Unlock()
	for _, dbIDs := range asNames {
		if err := iteratorFn(dbIDs); err != nil {
			return err
		}
	}
	return nil
}

func (f *FakeAddressSetFactory) DestroyAddressSet(dbIDs *libovsdbops.DbObjectIDs) error {
	if err := f.asf.validateDbIDs(dbIDs); err != nil {
		return fmt.Errorf("failed to destroy address set: %w", err)
	}
	name := getOvnAddressSetsName(dbIDs)
	if _, ok := f.sets[name]; ok {
		f.removeAddressSet(name)
		return nil
	}
	return nil
}

func (f *FakeAddressSetFactory) getAddressSet(dbIDs *libovsdbops.DbObjectIDs) *fakeAddressSets {
	f.Lock()
	defer f.Unlock()
	name := getOvnAddressSetsName(dbIDs)
	if as, ok := f.sets[name]; ok {
		as.Lock()
		return as
	}
	return nil
}

// removeAddressSet removes the address set from the factory
func (f *FakeAddressSetFactory) removeAddressSet(name string) {
	f.Lock()
	defer f.Unlock()
	delete(f.sets, name)
}

// expectAddressSetWithAddresses ensures the named address set exists with the given set of addresses
func (f *FakeAddressSetFactory) expectAddressSetWithAddresses(g gomega.Gomega, dbIDs *libovsdbops.DbObjectIDs, addresses []string) {
	var lenAddressSet int
	as := f.getAddressSet(dbIDs)
	gomega.Expect(as).ToNot(gomega.BeNil(), fmt.Sprintf("expected address set %s to exist", dbIDs.String()))
	defer as.Unlock()
	as4 := as.ipv4
	if as4 != nil {
		lenAddressSet = lenAddressSet + len(as4.addresses)
	}
	as6 := as.ipv6
	if as6 != nil {
		lenAddressSet = lenAddressSet + len(as6.addresses)
	}

	for _, address := range addresses {
		if utilnet.IsIPv6String(address) || utilnet.IsIPv6CIDRString(address) {
			g.Expect(as6).NotTo(gomega.BeNil())
			g.Expect(as6.addresses).To(gomega.HaveKey(address))
		} else {
			g.Expect(as4).NotTo(gomega.BeNil())
			g.Expect(as4.addresses).To(gomega.HaveKey(address))
		}
	}
	if lenAddressSet != len(addresses) {
		var addrs []string
		if as4 != nil {
			for _, v := range as4.addresses {
				addrs = append(addrs, v)
			}
		}
		if as6 != nil {
			for _, v := range as6.addresses {
				addrs = append(addrs, v)
			}
		}

		klog.Errorf("IPv4 addresses mismatch in cache: %#v, expected: %#v", addrs, addresses)
	}

	g.Expect(lenAddressSet).To(gomega.Equal(len(addresses)))
}

func (f *FakeAddressSetFactory) getDbIDsFromNsNameOrDbIDs(dbIDsOrNsName any) *libovsdbops.DbObjectIDs {
	var dbIDs *libovsdbops.DbObjectIDs
	if nsName, ok := dbIDsOrNsName.(string); ok {
		dbIDs = libovsdbops.NewDbObjectIDs(libovsdbops.AddressSetNamespace, f.ControllerName, map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: nsName,
		})
	} else if dbIDs, ok = dbIDsOrNsName.(*libovsdbops.DbObjectIDs); !ok {
		panic("unexpected type of argument passed to ExpectAddressSetWithAddresses")
	}
	return dbIDs
}

// ExpectAddressSetWithAddresses ensure address set exists with the given set of ips.
// Address set is identified by dbIDsOrNsName, which may be a namespace name (string) or a *libovsdbops.DbObjectIDs.
func (f *FakeAddressSetFactory) ExpectAddressSetWithAddresses(dbIDsOrNsName any, addresses []string) {
	dbIDs := f.getDbIDsFromNsNameOrDbIDs(dbIDsOrNsName)
	g := gomega.Default
	f.expectAddressSetWithAddresses(g, dbIDs, addresses)
}

func (f *FakeAddressSetFactory) EventuallyExpectAddressSetWithAddresses(dbIDsOrNsName any, addresses []string) {
	dbIDs := f.getDbIDsFromNsNameOrDbIDs(dbIDsOrNsName)
	gomega.Eventually(func(g gomega.Gomega) {
		f.expectAddressSetWithAddresses(g, dbIDs, addresses)
	}).Should(gomega.Succeed())
}

// ExpectEmptyAddressSet ensures the address set owned by dbIDsOrNsName exists with no Addresses
func (f *FakeAddressSetFactory) ExpectEmptyAddressSet(dbIDsOrNsName any) {
	dbIDs := f.getDbIDsFromNsNameOrDbIDs(dbIDsOrNsName)
	f.ExpectAddressSetWithAddresses(dbIDs, nil)
}

// EventuallyExpectEmptyAddressSetExist ensures the named address set eventually exists with no Addresses
func (f *FakeAddressSetFactory) EventuallyExpectEmptyAddressSetExist(dbIDsOrNsName any) {
	dbIDs := f.getDbIDsFromNsNameOrDbIDs(dbIDsOrNsName)
	f.EventuallyExpectAddressSetWithAddresses(dbIDs, nil)
}

func (f *FakeAddressSetFactory) AddressSetExists(dbIDsOrNsName any) bool {
	dbIDs := f.getDbIDsFromNsNameOrDbIDs(dbIDsOrNsName)
	name := getOvnAddressSetsName(dbIDs)
	f.Lock()
	defer f.Unlock()
	_, ok := f.sets[name]
	return ok
}

// EventuallyExpectAddressSet ensures the named address set eventually exists
func (f *FakeAddressSetFactory) EventuallyExpectAddressSet(dbIDsOrNsName any) {
	dbIDs := f.getDbIDsFromNsNameOrDbIDs(dbIDsOrNsName)
	gomega.Eventually(func() bool {
		return f.AddressSetExists(dbIDs)
	}).Should(gomega.BeTrue())
}

// EventuallyExpectNoAddressSet ensures the named address set eventually does not exist
// For namespaces address set deletion is delayed by 20 seconds, it is only tested once in namespace_test
// to not slow down tests. Don't use for namespace-owned address sets
func (f *FakeAddressSetFactory) EventuallyExpectNoAddressSet(dbIDsOrNsName any) {
	dbIDs := f.getDbIDsFromNsNameOrDbIDs(dbIDsOrNsName)
	gomega.Eventually(func() bool {
		return f.AddressSetExists(dbIDs)
	}).Should(gomega.BeFalse())
}

// ExpectNumberOfAddressSets ensures the number of created address sets equals given number
func (f *FakeAddressSetFactory) ExpectNumberOfAddressSets(n int) {
	gomega.Expect(len(f.sets)).To(gomega.Equal(n))
}

type removeFunc func(string)

type fakeAddressSet struct {
	name      string
	hashName  string
	addresses map[string]string
	destroyed uint32
}

// fakeAddressSets implements the AddressSet interface
var _ AddressSet = &fakeAddressSets{}

type fakeAddressSets struct {
	sync.Mutex
	// name without ip family
	name     string
	ipv4     *fakeAddressSet
	ipv6     *fakeAddressSet
	dbIDs    *libovsdbops.DbObjectIDs
	removeFn removeFunc
}

func (f *FakeAddressSetFactory) newFakeAddressSets(addresses []string, dbIDs *libovsdbops.DbObjectIDs, removeFn removeFunc) (*fakeAddressSets, error) {
	var v4set, v6set *fakeAddressSet
	v4Addresses := make([]string, 0)
	v6Addresses := make([]string, 0)
	for _, ip := range addresses {
		if utilnet.IsIPv6String(ip) || utilnet.IsIPv6CIDRString(ip) {
			v6Addresses = append(v6Addresses, ip)
		} else {
			v4Addresses = append(v4Addresses, ip)
		}
	}
	if config.IPv4Mode {
		v4set = f.newFakeAddressSet(v4Addresses, dbIDs, ipv4InternalID)
	}
	if config.IPv6Mode {
		v6set = f.newFakeAddressSet(v6Addresses, dbIDs, ipv6InternalID)
	}
	name := getOvnAddressSetsName(dbIDs)
	return &fakeAddressSets{name: name, ipv4: v4set, ipv6: v6set, dbIDs: dbIDs, removeFn: removeFn}, nil
}

func (f *FakeAddressSetFactory) newFakeAddressSet(addresses []string, dbIDs *libovsdbops.DbObjectIDs, ipFamily string) *fakeAddressSet {
	name := getDbIDsWithIPFamily(dbIDs, ipFamily).String()

	as := &fakeAddressSet{
		name:      name,
		hashName:  hashedAddressSet(name),
		addresses: make(map[string]string),
	}
	for _, address := range addresses {
		as.addresses[address] = address
	}
	return as
}

func (as *fakeAddressSets) GetASHashNames() (string, string) {
	var ipv4AS string
	var ipv6AS string
	if as.ipv4 != nil {
		ipv4AS = as.ipv4.getHashName()
	}
	if as.ipv6 != nil {
		ipv6AS = as.ipv6.getHashName()
	}
	return ipv4AS, ipv6AS
}

func (as *fakeAddressSets) GetName() string {
	return as.name
}

func (as *fakeAddressSets) AddAddresses(addresses []string) error {
	_, err := as.AddAddressesReturnOps(addresses)
	return err
}

func (as *fakeAddressSets) AddAddressesReturnOps(addresses []string) ([]ovsdb.Operation, error) {
	var ops []ovsdb.Operation
	var err error
	as.Lock()
	defer as.Unlock()
	for _, address := range addresses {
		if as.ipv6 != nil && (utilnet.IsIPv6String(address) || utilnet.IsIPv6CIDRString(address)) {
			ops, err = as.ipv6.addAddress(address)
		} else if as.ipv4 != nil && (utilnet.IsIPv4String(address) || utilnet.IsIPv4CIDRString(address)) {
			ops, err = as.ipv4.addAddress(address)
		}
		if err != nil {
			return nil, err
		}
	}
	return ops, nil
}

func (as *fakeAddressSets) GetAddresses() ([]string, []string) {
	as.Lock()
	defer as.Unlock()

	var v4addresses []string
	var v6addresses []string

	if as.ipv6 != nil {
		v6addresses, _ = as.ipv6.getAddresses()
	}
	if as.ipv4 != nil {
		v4addresses, _ = as.ipv4.getAddresses()
	}

	return v4addresses, v6addresses
}

func (as *fakeAddressSets) SetAddresses(addresses []string) error {
	allAddresses := []string{}
	if as.ipv4 != nil {
		for _, ip := range as.ipv4.addresses {
			allAddresses = append(allAddresses, ip)
		}
	}

	if as.ipv6 != nil {
		for _, ip := range as.ipv6.addresses {
			allAddresses = append(allAddresses, ip)
		}
	}

	err := as.DeleteAddresses(allAddresses)
	if err != nil {
		return err
	}

	return as.AddAddresses(addresses)
}

func (as *fakeAddressSets) DeleteAddresses(addresses []string) error {
	_, err := as.DeleteAddressesReturnOps(addresses)
	return err
}

func (as *fakeAddressSets) DeleteAddressesReturnOps(addresses []string) ([]ovsdb.Operation, error) {
	var ops []ovsdb.Operation
	var err error
	as.Lock()
	defer as.Unlock()

	for _, ip := range addresses {
		if as.ipv6 != nil && (utilnet.IsIPv6String(ip) || utilnet.IsIPv6CIDRString(ip)) {
			ops, err = as.ipv6.deleteAddress(ip)
		} else if as.ipv4 != nil && (utilnet.IsIPv4String(ip) || utilnet.IsIPv4CIDRString(ip)) {
			ops, err = as.ipv4.deleteAddress(ip)
		}
		if err != nil {
			return nil, err
		}
	}
	return ops, nil
}

func (as *fakeAddressSets) Destroy() error {
	as.Lock()
	defer func() {
		as.Unlock()
		as.removeFn(as.name)
	}()

	if as.ipv4 != nil {
		err := as.ipv4.destroy()
		if err != nil {
			return err
		}
	}
	if as.ipv6 != nil {
		return as.ipv6.destroy()
	}
	return nil
}

func (as *fakeAddressSet) getHashName() string {
	gomega.Expect(atomic.LoadUint32(&as.destroyed)).To(gomega.Equal(uint32(0)))
	return as.hashName
}

func (as *fakeAddressSet) addAddress(address string) ([]ovsdb.Operation, error) {
	gomega.Expect(atomic.LoadUint32(&as.destroyed)).To(gomega.Equal(uint32(0)))
	if _, ok := as.addresses[address]; !ok {
		as.addresses[address] = address
	}
	return nil, nil
}

func (as *fakeAddressSet) getAddresses() ([]string, error) {
	gomega.Expect(atomic.LoadUint32(&as.destroyed)).To(gomega.Equal(uint32(0)))
	uniqAddresses := make([]string, 0, len(as.addresses))
	for _, ip := range as.addresses {
		uniqAddresses = append(uniqAddresses, ip)
	}
	return uniqAddresses, nil
}

func (as *fakeAddressSet) deleteAddress(address string) ([]ovsdb.Operation, error) {
	gomega.Expect(atomic.LoadUint32(&as.destroyed)).To(gomega.Equal(uint32(0)))
	delete(as.addresses, address)
	return nil, nil
}

func (as *fakeAddressSet) destroy() error {
	// Don't check here if the address set was already destroyed as it should be
	// a thread safe, idempotent operation anyway.
	atomic.StoreUint32(&as.destroyed, 1)
	return nil
}
