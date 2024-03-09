package addressset

import (
	"fmt"
	"net"
	"sync"
	"sync/atomic"

	"github.com/onsi/gomega"

	"github.com/ovn-org/libovsdb/ovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"

	"k8s.io/apimachinery/pkg/util/sets"
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
func (f *FakeAddressSetFactory) NewAddressSet(dbIDs *libovsdbops.DbObjectIDs, ips sets.Set[string]) (AddressSet, error) {
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
	set, err := f.newFakeAddressSets(ips, dbIDs, f.removeAddressSet)
	if err != nil {
		return nil, err
	}
	f.sets[name] = set
	return set, nil
}

// NewAddressSetOps returns a new address set object
func (f *FakeAddressSetFactory) NewAddressSetOps(dbIDs *libovsdbops.DbObjectIDs, ips sets.Set[string]) (AddressSet, []ovsdb.Operation, error) {
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
	set, err := f.newFakeAddressSets(ips, dbIDs, f.removeAddressSet)
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
	set, err := f.newFakeAddressSets(sets.New[string](), dbIDs, f.removeAddressSet)
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

// ExpectAddressSetWithIPs ensures the named address set exists with the given set of IPs
func (f *FakeAddressSetFactory) expectAddressSetWithIPs(g gomega.Gomega, dbIDs *libovsdbops.DbObjectIDs, ips []string) {
	var lenAddressSet int
	as := f.getAddressSet(dbIDs)
	gomega.Expect(as).ToNot(gomega.BeNil(), fmt.Sprintf("expected address set %s to exist", dbIDs.String()))
	defer as.Unlock()
	as4 := as.ipv4
	if as4 != nil {
		lenAddressSet = lenAddressSet + len(as4.ips)
	}
	as6 := as.ipv6
	if as6 != nil {
		lenAddressSet = lenAddressSet + len(as6.ips)
	}

	for _, ip := range ips {
		if utilnet.IsIPv6(net.ParseIP(ip)) {
			g.Expect(as6).NotTo(gomega.BeNil())
			g.Expect(as6.ips).To(gomega.HaveKey(ip))
		} else {
			g.Expect(as4).NotTo(gomega.BeNil())
			g.Expect(as4.ips).To(gomega.HaveKey(ip))
		}
	}
	if lenAddressSet != len(ips) {
		var addrs []string
		if as4 != nil {
			addrs = append(addrs, as4.ips.UnsortedList()...)
		}
		if as6 != nil {
			addrs = append(addrs, as6.ips.UnsortedList()...)
		}

		klog.Errorf("IPv4 addresses mismatch in cache: %#v, expected: %#v", addrs, ips)
	}

	g.Expect(lenAddressSet).To(gomega.Equal(len(ips)))
}

func (f *FakeAddressSetFactory) getDbIDsFromNsNameOrDbIDs(dbIDsOrNsName any) *libovsdbops.DbObjectIDs {
	var dbIDs *libovsdbops.DbObjectIDs
	if nsName, ok := dbIDsOrNsName.(string); ok {
		dbIDs = libovsdbops.NewDbObjectIDs(libovsdbops.AddressSetNamespace, f.ControllerName, map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: nsName,
		})
	} else if dbIDs, ok = dbIDsOrNsName.(*libovsdbops.DbObjectIDs); !ok {
		panic("unexpected type of argument passed to ExpectAddressSetWithIPs")
	}
	return dbIDs
}

// ExpectAddressSetWithIPs ensure address set exists with the given set of ips.
// Address set is identified by dbIDsOrNsName, which may be a namespace name (string) or a *libovsdbops.DbObjectIDs.
func (f *FakeAddressSetFactory) ExpectAddressSetWithIPs(dbIDsOrNsName any, ips []string) {
	dbIDs := f.getDbIDsFromNsNameOrDbIDs(dbIDsOrNsName)
	g := gomega.Default
	f.expectAddressSetWithIPs(g, dbIDs, ips)
}

func (f *FakeAddressSetFactory) EventuallyExpectAddressSetWithIPs(dbIDsOrNsName any, ips []string) {
	dbIDs := f.getDbIDsFromNsNameOrDbIDs(dbIDsOrNsName)
	gomega.Eventually(func(g gomega.Gomega) {
		f.expectAddressSetWithIPs(g, dbIDs, ips)
	}).Should(gomega.Succeed())
}

// ExpectEmptyAddressSet ensures the address set owned by dbIDsOrNsName exists with no IPs
func (f *FakeAddressSetFactory) ExpectEmptyAddressSet(dbIDsOrNsName any) {
	dbIDs := f.getDbIDsFromNsNameOrDbIDs(dbIDsOrNsName)
	f.ExpectAddressSetWithIPs(dbIDs, nil)
}

// EventuallyExpectEmptyAddressSetExist ensures the named address set eventually exists with no IPs
func (f *FakeAddressSetFactory) EventuallyExpectEmptyAddressSetExist(dbIDsOrNsName any) {
	dbIDs := f.getDbIDsFromNsNameOrDbIDs(dbIDsOrNsName)
	f.EventuallyExpectAddressSetWithIPs(dbIDs, nil)
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
	ips       sets.Set[string]
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

func (f *FakeAddressSetFactory) newFakeAddressSets(ips sets.Set[string], dbIDs *libovsdbops.DbObjectIDs, removeFn removeFunc) (*fakeAddressSets, error) {
	var v4set, v6set *fakeAddressSet
	v4Ips := sets.New[string]()
	v6Ips := sets.New[string]()
	for _, ip := range ips.UnsortedList() {
		if utilnet.IsIPv6String(ip) || utilnet.IsIPv6CIDRString(ip) {
			v6Ips.Insert(ip)
		} else {
			v4Ips.Insert(ip)
		}
	}
	if config.IPv4Mode {
		v4set = f.newFakeAddressSet(v4Ips, dbIDs, ipv4InternalID)
	}
	if config.IPv6Mode {
		v6set = f.newFakeAddressSet(v6Ips, dbIDs, ipv6InternalID)
	}
	name := getOvnAddressSetsName(dbIDs)
	return &fakeAddressSets{name: name, ipv4: v4set, ipv6: v6set, dbIDs: dbIDs, removeFn: removeFn}, nil
}

func (f *FakeAddressSetFactory) newFakeAddressSet(ips sets.Set[string], dbIDs *libovsdbops.DbObjectIDs, ipFamily string) *fakeAddressSet {
	name := getDbIDsWithIPFamily(dbIDs, ipFamily).String()

	as := &fakeAddressSet{
		name:     name,
		hashName: hashedAddressSet(name),
		ips:      ips,
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

func (as *fakeAddressSets) AddIPs(ips sets.Set[string]) error {
	_, err := as.AddIPsReturnOps(ips)
	return err
}

func (as *fakeAddressSets) AddIPsReturnOps(ips sets.Set[string]) ([]ovsdb.Operation, error) {
	var ops []ovsdb.Operation
	var err error
	as.Lock()
	defer as.Unlock()
	for _, ip := range ips.UnsortedList() {
		if as.ipv6 != nil && (utilnet.IsIPv6String(ip) || utilnet.IsIPv6CIDRString(ip)) {
			ops, err = as.ipv6.addIP(ip)
		} else if as.ipv4 != nil && (utilnet.IsIPv4String(ip) || utilnet.IsIPv4CIDRString(ip)) {
			ops, err = as.ipv4.addIP(ip)
		}
		if err != nil {
			return nil, err
		}
	}
	return ops, nil
}

func (as *fakeAddressSets) GetIPs() ([]string, []string) {
	as.Lock()
	defer as.Unlock()

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

func (as *fakeAddressSets) SetIPs(ips sets.Set[string]) error {
	allIPs := sets.Set[string]{}
	if as.ipv4 != nil {
		for _, ip := range as.ipv4.ips.UnsortedList() {
			allIPs.Insert(ip)
		}
	}

	if as.ipv6 != nil {
		for _, ip := range as.ipv6.ips.UnsortedList() {
			allIPs.Insert(ip)
		}
	}

	err := as.DeleteIPs(allIPs)
	if err != nil {
		return err
	}

	return as.AddIPs(ips)
}

func (as *fakeAddressSets) DeleteIPs(ips sets.Set[string]) error {
	_, err := as.DeleteIPsReturnOps(ips)
	return err
}

func (as *fakeAddressSets) DeleteIPsReturnOps(ips sets.Set[string]) ([]ovsdb.Operation, error) {
	var ops []ovsdb.Operation
	var err error
	as.Lock()
	defer as.Unlock()

	for _, ip := range ips.UnsortedList() {
		if as.ipv6 != nil && (utilnet.IsIPv6String(ip) || utilnet.IsIPv6CIDRString(ip)) {
			ops, err = as.ipv6.deleteIP(ip)
		} else if as.ipv4 != nil && (utilnet.IsIPv4String(ip) || utilnet.IsIPv4CIDRString(ip)) {
			ops, err = as.ipv4.deleteIP(ip)
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

func (as *fakeAddressSet) addIP(ip string) ([]ovsdb.Operation, error) {
	gomega.Expect(atomic.LoadUint32(&as.destroyed)).To(gomega.Equal(uint32(0)))
	if ok := as.ips.Has(ip); !ok {
		as.ips.Insert(ip)
	}
	return nil, nil
}

func (as *fakeAddressSet) getIPs() ([]string, error) {
	gomega.Expect(atomic.LoadUint32(&as.destroyed)).To(gomega.Equal(uint32(0)))
	return as.ips.UnsortedList(), nil
}

func (as *fakeAddressSet) deleteIP(ip string) ([]ovsdb.Operation, error) {
	gomega.Expect(atomic.LoadUint32(&as.destroyed)).To(gomega.Equal(uint32(0)))
	delete(as.ips, ip)
	return nil, nil
}

func (as *fakeAddressSet) destroy() error {
	// Don't check here if the address set was already destroyed as it should be
	// a thread safe, idempotent operation anyway.
	atomic.StoreUint32(&as.destroyed, 1)
	return nil
}
