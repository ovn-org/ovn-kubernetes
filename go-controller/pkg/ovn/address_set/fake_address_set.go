package addressset

import (
	"errors"
	"fmt"
	"net"
	"runtime/debug"
	"sync"

	"github.com/onsi/ginkgo"
	"github.com/onsi/gomega"

	"github.com/ovn-org/libovsdb/ovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
)

func NewFakeAddressSetFactory(controllerName string) *FakeAddressSetFactory {
	return &FakeAddressSetFactory{
		ControllerName: controllerName,
		asf:            &ovnAddressSetFactory{},
		sets:           make(map[string]*libovsdbops.DbObjectIDs),
		ips:            map[string]sets.Set[string]{},
		ipgenerations:  map[string]map[string]int{},
		ipowner:        map[string]map[string]string{},
	}
}

type FakeAddressSetFactory struct {
	// ControllerName is stored here for convenience, it is used to build dbIDs for fake-only methods like
	// AddressSetExists, EventuallyExpectAddressSet, etc.
	ControllerName string
	asf            *ovnAddressSetFactory
	sync.Mutex

	errOnNextNewAddrSet bool

	// map of address set names (without ip family) to IDs
	sets map[string]*libovsdbops.DbObjectIDs

	// map of address sets names (with ip family) to IPs
	ips map[string]sets.Set[string]

	// keep track of of a operation/generation counter per IP and address set to
	// detect concurrency issues
	// when an IP is added or removed, compare the generation the user is
	// tracking vs the generation the factory is tracking
	// if it does not match it potentially means that we are removing or adding
	// an IP that was added or removed by someone else concurrently.
	// increase the counter every time an IP is operated with
	// map of address set name (with ip family) to ip to generation
	ipgenerations map[string]map[string]int

	// keep track of the stack trace per IP and address set for the last time it
	// was operated with it, as the current `owner` of the IP
	// map of address set name (with ip family) to ip to stack trace
	ipowner map[string]map[string]string
}

// fakeFactory implements the AddressSetFactory interface
var _ AddressSetFactory = &FakeAddressSetFactory{}

const FakeASFError = "fake asf error"

// ErrOnNextNewASCall will make FakeAddressSetFactory return FakeASFError on the next NewAddressSet call
func (f *FakeAddressSetFactory) ErrOnNextNewASCall() {
	f.errOnNextNewAddrSet = true
}

// NewAddressSet returns a new address set object
func (f *FakeAddressSetFactory) NewAddressSet(dbIDs *libovsdbops.DbObjectIDs, ips []net.IP) (AddressSet, error) {
	f.Lock()
	defer f.Unlock()

	if f.errOnNextNewAddrSet {
		f.errOnNextNewAddrSet = false
		return nil, fmt.Errorf(FakeASFError)
	}
	if err := f.asf.validateDbIDs(dbIDs); err != nil {
		return nil, fmt.Errorf("failed to create address set: %w", err)
	}

	name := getOvnAddressSetsName(dbIDs)
	if _, exists := f.sets[name]; exists {
		return nil, errors.New("fake address set already exists")
	}

	create := true
	set, err := f.newFakeAddressSets(ips, dbIDs, create)
	if err != nil {
		return nil, err
	}
	return set, nil
}

// EnsureAddressSet returns set object
func (f *FakeAddressSetFactory) EnsureAddressSet(dbIDs *libovsdbops.DbObjectIDs) (AddressSet, error) {
	f.Lock()
	defer f.Unlock()

	if err := f.asf.validateDbIDs(dbIDs); err != nil {
		return nil, fmt.Errorf("failed to ensure address set: %w", err)
	}

	create := true
	set, err := f.newFakeAddressSets(nil, dbIDs, create)
	if err != nil {
		return nil, err
	}

	return set, nil
}

// GetAddressSet returns set object
func (f *FakeAddressSetFactory) GetAddressSet(dbIDs *libovsdbops.DbObjectIDs) (AddressSet, error) {
	f.Lock()
	defer f.Unlock()

	if err := f.asf.validateDbIDs(dbIDs); err != nil {
		return nil, fmt.Errorf("failed to get address set: %w", err)
	}

	name := getOvnAddressSetsName(dbIDs)
	if _, exists := f.sets[name]; !exists {
		return nil, errors.New("fake address set does not exist")
	}

	create := false
	set, err := f.newFakeAddressSets(nil, dbIDs, create)
	if err != nil {
		return nil, err
	}

	return set, nil
}

func (f *FakeAddressSetFactory) ProcessEachAddressSet(ownerController string, indexT *libovsdbops.ObjectIDsType, iteratorFn AddressSetIterFunc) error {
	f.Lock()
	asNames := map[string]*libovsdbops.DbObjectIDs{}
	for name, dbIDs := range f.sets {
		if !dbIDs.HasSameOwner(ownerController, indexT) {
			continue
		}
		asNames[name] = dbIDs
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
	f.Lock()
	defer f.Unlock()

	if err := f.asf.validateDbIDs(dbIDs); err != nil {
		return fmt.Errorf("failed to destroy address set: %w", err)
	}

	name := getOvnAddressSetsName(dbIDs)
	if _, exists := f.sets[name]; !exists {
		return nil
	}

	create := false
	as, err := f.newFakeAddressSets(nil, dbIDs, create)
	if err != nil {
		return err
	}

	as.destroy()
	return nil
}

// ExpectAddressSetWithIPs ensures the named address set exists with the given set of IPs
func (f *FakeAddressSetFactory) expectAddressSetWithIPs(g gomega.Gomega, dbIDs *libovsdbops.DbObjectIDs, ips []string) {
	f.Lock()
	defer f.Unlock()

	name4 := getDbIDsWithIPFamily(dbIDs, ipv4InternalID).String()
	name6 := getDbIDsWithIPFamily(dbIDs, ipv6InternalID).String()

	ipv4s := f.ips[name4]
	ipv6s := f.ips[name6]

	lenAddressSet := len(ipv4s) + len(ipv6s)

	for _, ip := range ips {
		netIP := net.ParseIP(ip)
		if utilnet.IsIPv6(netIP) {
			g.Expect(ipv6s).To(gomega.HaveKey(ip))
		} else {
			g.Expect(ipv4s).To(gomega.HaveKey(ip))
		}
	}
	if lenAddressSet != len(ips) {
		var addrs []string
		addrs = append(addrs, ipv4s.UnsortedList()...)
		addrs = append(addrs, ipv6s.UnsortedList()...)
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
	f.Lock()
	defer f.Unlock()
	gomega.Expect(len(f.sets)).To(gomega.Equal(n))
}

type fakeAddressSet struct {
	factory       *FakeAddressSetFactory
	name          string
	hashName      string
	ipgenerations map[string]int
}

// fakeAddressSets implements the AddressSet interface
var _ AddressSet = &fakeAddressSets{}

type fakeAddressSets struct {
	factory *FakeAddressSetFactory

	// name without ip family
	name string

	ipv4  *fakeAddressSet
	ipv6  *fakeAddressSet
	dbIDs *libovsdbops.DbObjectIDs
}

func (f *FakeAddressSetFactory) newFakeAddressSets(ips []net.IP, dbIDs *libovsdbops.DbObjectIDs, create bool) (*fakeAddressSets, error) {
	var v4set, v6set *fakeAddressSet
	var err error
	if config.IPv4Mode {
		ipv4s, _ := util.MatchIPFamily(false, ips)
		v4set, err = f.newFakeAddressSet(ipv4s, dbIDs, ipv4InternalID, create)
		if err != nil {
			return nil, err
		}
	}
	if config.IPv6Mode {
		ipv6s, _ := util.MatchIPFamily(true, ips)
		v6set, err = f.newFakeAddressSet(ipv6s, dbIDs, ipv6InternalID, create)
		if err != nil {
			return nil, err
		}
	}
	name := getOvnAddressSetsName(dbIDs)
	as := &fakeAddressSets{factory: f, name: name, ipv4: v4set, ipv6: v6set, dbIDs: dbIDs}
	if create {
		f.sets[name] = dbIDs
	}
	return as, nil
}

func (f *FakeAddressSetFactory) newFakeAddressSet(netIPs []net.IP, dbIDs *libovsdbops.DbObjectIDs, ipFamily string, create bool) (*fakeAddressSet, error) {
	asName := getDbIDsWithIPFamily(dbIDs, ipFamily).String()

	as := &fakeAddressSet{
		factory:       f,
		name:          asName,
		hashName:      hashedAddressSet(asName),
		ipgenerations: map[string]int{},
	}

	_, exists := as.factory.sets[as.name]
	if create && !exists {
		f.ips[asName] = sets.New[string]()
		f.ipgenerations[asName] = map[string]int{}
		f.ipowner[asName] = map[string]string{}
	}

	// copy the current IPs generation from the factory to the AS
	for ip, generation := range f.ipgenerations[as.name] {
		as.ipgenerations[ip] = generation
	}

	if len(netIPs) > 0 {
		defer ginkgo.GinkgoRecover()
		// this might be ok in certain situations but for now check against it
		// just remove this expect if need be
		gomega.Expect(f.ips[asName]).To(gomega.BeEmpty(), "can't create a new address set overwriting existing IPs")
		ips := util.StringSlice(netIPs)
		f.ips[asName] = sets.New(ips...)
		// increase the IPs generation if setting new IPs
		f.incrementIPsGeneration(as, f.ips[asName].UnsortedList()...)
	}

	return as, nil
}

func (f *FakeAddressSetFactory) incrementIPsGeneration(as *fakeAddressSet, ips ...string) {
	for _, ip := range ips {
		f.ipgenerations[as.name][ip]++
		as.ipgenerations[ip]++
		// save the stack if generation was increased
		f.ipowner[as.name][ip] = string(debug.Stack())
	}
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

func (as *fakeAddressSets) AddIPs(ips []net.IP) error {
	_, err := as.AddIPsReturnOps(ips)
	return err
}

func (as *fakeAddressSets) AddIPsReturnOps(ips []net.IP) ([]ovsdb.Operation, error) {
	as.factory.Lock()
	defer as.factory.Unlock()

	if _, exists := as.factory.sets[as.name]; !exists {
		return nil, fmt.Errorf("fake address %s set does not exist", as.name)
	}

	if as.ipv4 != nil {
		ipv4s, _ := util.MatchIPFamily(false, ips)
		as.ipv4.addIPs(util.StringSlice(ipv4s)...)
	}

	if as.ipv6 != nil {
		ipv6s, _ := util.MatchIPFamily(true, ips)
		as.ipv6.addIPs(util.StringSlice(ipv6s)...)
	}

	return nil, nil
}

func (as *fakeAddressSets) GetIPs() ([]string, []string) {
	as.factory.Lock()
	defer as.factory.Unlock()

	var v4ips []string
	var v6ips []string

	if as.ipv6 != nil {
		v6ips = as.ipv6.getIPs()
	}
	if as.ipv4 != nil {
		v4ips = as.ipv4.getIPs()
	}

	return v4ips, v6ips
}

func (as *fakeAddressSets) SetIPs(ips []net.IP) error {
	as.factory.Lock()
	defer as.factory.Unlock()

	if _, exists := as.factory.sets[as.name]; !exists {
		return fmt.Errorf("fake address %s set does not exist", as.name)
	}

	if as.ipv4 != nil {
		ipv4s, _ := util.MatchIPFamily(false, ips)
		as.ipv4.setIPs(util.StringSlice(ipv4s)...)
	}

	if as.ipv6 != nil {
		ipv6s, _ := util.MatchIPFamily(true, ips)
		as.ipv6.setIPs(util.StringSlice(ipv6s)...)
	}

	return nil
}

func (as *fakeAddressSets) DeleteIPs(ips []net.IP) error {
	_, err := as.DeleteIPsReturnOps(ips)
	return err
}

func (as *fakeAddressSets) DeleteIPsReturnOps(ips []net.IP) ([]ovsdb.Operation, error) {
	as.factory.Lock()
	defer as.factory.Unlock()

	if as.ipv4 != nil {
		ipv4s, _ := util.MatchIPFamily(false, ips)
		as.ipv4.deleteIPs(util.StringSlice(ipv4s)...)
	}

	if as.ipv6 != nil {
		ipv6s, _ := util.MatchIPFamily(true, ips)
		as.ipv6.deleteIPs(util.StringSlice(ipv6s)...)
	}

	return nil, nil
}

func (as *fakeAddressSets) Destroy() error {
	as.factory.Lock()
	defer as.factory.Unlock()
	as.destroy()
	return nil
}

func (as *fakeAddressSets) destroy() {
	if as.ipv4 != nil {
		as.ipv4.destroy()
	}
	if as.ipv6 != nil {
		as.ipv6.destroy()
	}

	delete(as.factory.sets, as.name)
}

func (as *fakeAddressSet) getHashName() string {
	return as.hashName
}

func (as *fakeAddressSet) addIPs(ips ...string) {
	set := sets.New(ips...)
	added := set.Difference(as.factory.ips[as.name])
	defer ginkgo.GinkgoRecover()
	for ip := range added {
		gomega.Expect(as.factory.ipgenerations[as.name][ip]).To(gomega.Equal(as.ipgenerations[ip]),
			"Trying to add IP %s to AS %s which was concurrently removed by someone else:\nCurrent stack:\n%s\nOther stack:\n%s",
			ip,
			as.name,
			string(debug.Stack()),
			as.factory.ipowner[as.name][ip],
		)
	}
	as.factory.incrementIPsGeneration(as, ips...)
	as.factory.ips[as.name].Insert(ips...)
}

func (as *fakeAddressSet) setIPs(ips ...string) {
	removed := as.factory.ips[as.name].Difference(sets.New(ips...))
	as.deleteIPs(removed.UnsortedList()...)
	as.addIPs(ips...)
}

func (as *fakeAddressSet) getIPs() []string {
	return as.factory.ips[as.name].UnsortedList()
}

func (as *fakeAddressSet) deleteIPs(ips ...string) {
	removed := as.factory.ips[as.name].Intersection(sets.New(ips...))
	defer ginkgo.GinkgoRecover()
	for ip := range removed {
		gomega.Expect(as.factory.ipgenerations[as.name][ip]).To(gomega.Equal(as.ipgenerations[ip]),
			"Trying to delete IP %s from AS %s which was concurrently added by someone else:\nCurrent stack:\n%s\nOther stack:\n%s",
			ip,
			as.name,
			string(debug.Stack()),
			as.factory.ipowner[as.name][ip],
		)
	}
	as.factory.incrementIPsGeneration(as, ips...)
	as.factory.ips[as.name].Delete(ips...)
}

func (as *fakeAddressSet) destroy() {
	as.deleteIPs(as.factory.ips[as.name].UnsortedList()...)
	delete(as.factory.ips, as.name)
	delete(as.factory.ipgenerations, as.name)
	delete(as.factory.ipowner, as.name)
}
