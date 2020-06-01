package ovn

import (
	"net"
	"strings"
	"sync"

	. "github.com/onsi/gomega"
)

func newFakeAddressSetFactory() *fakeAddressSetFactory {
	return &fakeAddressSetFactory{
		sets: make(map[string]*fakeAddressSet),
	}
}

type fakeAddressSetFactory struct {
	sync.RWMutex
	// maps address set name to object
	sets map[string]*fakeAddressSet
}

// fakeFactory implements the AddressSetFactory interface
var _ AddressSetFactory = &fakeAddressSetFactory{}

// NewAddressSet returns a new address set object
func (f *fakeAddressSetFactory) NewAddressSet(name string, ips []net.IP) (AddressSet, error) {
	f.Lock()
	defer f.Unlock()
	_, ok := f.sets[name]
	Expect(ok).To(BeFalse())
	set := newFakeAddressSet(name, ips, f.removeAddressSet)
	f.sets[name] = set
	return set, nil
}

func (f *fakeAddressSetFactory) ForEachAddressSet(iteratorFn AddressSetIterFunc) error {
	for name, set := range f.sets {
		parts := strings.Split(set.GetName(), ".")
		addrSetNamespace := parts[0]
		nameSuffix := ""
		if len(parts) >= 2 {
			nameSuffix = parts[1]
		}
		iteratorFn(name, addrSetNamespace, nameSuffix)
	}
	return nil
}

func (f *fakeAddressSetFactory) DestroyAddressSetInBackingStore(name string) error {
	f.removeAddressSet(name)
	return nil
}

func (f *fakeAddressSetFactory) getAddressSet(name string) *fakeAddressSet {
	f.RLock()
	defer f.RUnlock()
	if as, ok := f.sets[name]; ok {
		as.Lock()
		return as
	}
	return nil
}

// removeAddressSet removes the address set from the factory
func (f *fakeAddressSetFactory) removeAddressSet(name string) {
	f.Lock()
	defer f.Unlock()
	delete(f.sets, name)
}

// ExpectAddressSetWithIPs ensures the named address set exists with the given set of IPs
func (f *fakeAddressSetFactory) ExpectAddressSetWithIPs(name string, ips []string) {
	as := f.getAddressSet(name)
	Expect(as).NotTo(BeNil())
	defer as.Unlock()
	for _, expectedIP := range ips {
		Expect(as.ips).To(HaveKey(expectedIP))
	}
	Expect(as.ips).To(HaveLen(len(ips)))
}

// ExpectEmptyAddressSet ensures the named address set exists with no IPs
func (f *fakeAddressSetFactory) ExpectEmptyAddressSet(name string) {
	f.ExpectAddressSetWithIPs(name, nil)
}

// EventuallyExpectEmptyAddressSet ensures the named address set eventually exists with no IPs
func (f *fakeAddressSetFactory) EventuallyExpectEmptyAddressSet(name string) {
	Eventually(func() bool {
		as := f.getAddressSet(name)
		Expect(as).NotTo(BeNil())
		defer as.Unlock()
		return len(as.ips) == 0
	}).Should(BeTrue())
}

// EventuallyExpectNoAddressSet ensures the named address set eventually does not exist
func (f *fakeAddressSetFactory) EventuallyExpectNoAddressSet(name string) {
	Eventually(func() bool {
		f.RLock()
		defer f.RUnlock()
		_, ok := f.sets[name]
		return ok
	}).Should(BeFalse())
}

type removeFunc func(string)

type fakeAddressSet struct {
	sync.Mutex
	name      string
	hashName  string
	ips       map[string]net.IP
	destroyed bool
	removeFn  removeFunc
}

// fakeAddressSet implements the AddressSet interface
var _ AddressSet = &fakeAddressSet{}

func newFakeAddressSet(name string, ips []net.IP, removeFn removeFunc) *fakeAddressSet {
	as := &fakeAddressSet{
		name:     name,
		hashName: hashedAddressSet(name),
		ips:      make(map[string]net.IP),
		removeFn: removeFn,
	}
	for _, ip := range ips {
		as.ips[ip.String()] = ip
	}
	return as
}

func (as *fakeAddressSet) GetHashName() string {
	Expect(as.destroyed).To(BeFalse())
	return as.hashName
}

func (as *fakeAddressSet) GetName() string {
	Expect(as.destroyed).To(BeFalse())
	return as.name
}

func (as *fakeAddressSet) AddIP(ip net.IP) error {
	Expect(as.destroyed).To(BeFalse())
	as.Lock()
	defer as.Unlock()
	ipStr := ip.String()
	if _, ok := as.ips[ipStr]; !ok {
		as.ips[ip.String()] = ip
	}
	return nil
}

func (as *fakeAddressSet) DeleteIP(ip net.IP) error {
	Expect(as.destroyed).To(BeFalse())
	as.Lock()
	defer as.Unlock()
	delete(as.ips, ip.String())
	return nil
}

func (as *fakeAddressSet) destroyInternal() {
	Expect(as.destroyed).To(BeFalse())
	as.destroyed = true
	as.removeFn(as.name)
}

func (as *fakeAddressSet) Destroy() error {
	Expect(as.destroyed).To(BeFalse())
	as.Lock()
	defer as.Unlock()
	as.destroyInternal()
	return nil
}
