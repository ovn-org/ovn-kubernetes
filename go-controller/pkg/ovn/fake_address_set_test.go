package ovn

import (
	"net"
	"strings"
	"sync"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"

	"k8s.io/apimachinery/pkg/util/sets"
	utilnet "k8s.io/utils/net"

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
	set, err := newFakeAddressSets(name, ips, f.removeAddressSet)
	if err != nil {
		return nil, err
	}
	if set.ipv4 != nil {
		f.sets[getIPv4ASName(name)] = set.ipv4
	}
	if set.ipv6 != nil {
		f.sets[getIPv6ASName(name)] = set.ipv6
	}
	return set, nil
}

func (f *fakeAddressSetFactory) ForEachAddressSet(iteratorFn AddressSetIterFunc) error {
	asNames := sets.String{}
	for _, set := range f.sets {
		asName := truncateSuffixFromAddressSet(set.getName())
		if asNames.Has(asName) {
			continue
		}
		asNames.Insert(asName)
		parts := strings.Split(asName, ".")
		addrSetNamespace := parts[0]
		nameSuffix := ""
		if len(parts) >= 2 {
			nameSuffix = parts[1]
		}
		iteratorFn(asName, addrSetNamespace, nameSuffix)
	}
	return nil
}

func (f *fakeAddressSetFactory) DestroyAddressSetInBackingStore(name string) error {
	if _, ok := f.sets[name]; ok {
		f.removeAddressSet(name)
		return nil
	}
	if config.IPv4Mode {
		f.removeAddressSet(getIPv4ASName(name))
	}
	if config.IPv6Mode {
		f.removeAddressSet(getIPv6ASName(name))
	}
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

// ExpectNoAddressSet ensures the named address set does not exist
func (f *fakeAddressSetFactory) ExpectNoAddressSet(name string) {
	_, ok := f.sets[name]
	Expect(ok).To(BeFalse())
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

// fakeAddressSets implements the AddressSet interface
var _ AddressSet = &fakeAddressSets{}

type fakeAddressSets struct {
	sync.Mutex
	name string
	ipv4 *fakeAddressSet
	ipv6 *fakeAddressSet
}

func newFakeAddressSets(name string, ips []net.IP, removeFn removeFunc) (*fakeAddressSets, error) {
	var v4set, v6set *fakeAddressSet
	v4Ips := make([]net.IP, 0)
	v6Ips := make([]net.IP, 0)
	for _, ip := range ips {
		if utilnet.IsIPv6(ip) {
			v6Ips = append(v6Ips, ip)
		} else {
			v4Ips = append(v4Ips, ip)
		}
	}
	if config.IPv4Mode {
		v4set = newFakeAddressSet(getIPv4ASName(name), v4Ips, removeFn)
	}
	if config.IPv6Mode {
		v6set = newFakeAddressSet(getIPv6ASName(name), v6Ips, removeFn)
	}
	return &fakeAddressSets{name: name, ipv4: v4set, ipv6: v6set}, nil
}

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

func (as *fakeAddressSets) GetIPv4HashName() string {
	if as.ipv4 != nil {
		return as.ipv4.getHashName()
	}
	return ""
}

func (as *fakeAddressSets) GetIPv6HashName() string {
	if as.ipv6 != nil {
		return as.ipv6.getHashName()
	}
	return ""
}

func (as *fakeAddressSets) GetName() string {
	return as.name
}

func (as *fakeAddressSets) AddIPs(ips []net.IP) error {
	var err error
	as.Lock()
	defer as.Unlock()

	for _, ip := range ips {
		if utilnet.IsIPv6(ip) {
			err = as.ipv6.addIP(ip)
		} else {
			err = as.ipv4.addIP(ip)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (as *fakeAddressSets) DeleteIPs(ips []net.IP) error {
	var err error
	as.Lock()
	defer as.Unlock()

	for _, ip := range ips {
		if utilnet.IsIPv6(ip) {
			err = as.ipv6.deleteIP(ip)
		} else {
			err = as.ipv4.deleteIP(ip)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (as *fakeAddressSets) Destroy() error {
	as.Lock()
	defer as.Unlock()

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
	Expect(as.destroyed).To(BeFalse())
	return as.hashName
}

func (as *fakeAddressSet) getName() string {
	Expect(as.destroyed).To(BeFalse())
	return as.name
}

func (as *fakeAddressSet) addIP(ip net.IP) error {
	Expect(as.destroyed).To(BeFalse())
	ipStr := ip.String()
	if _, ok := as.ips[ipStr]; !ok {
		as.ips[ip.String()] = ip
	}
	return nil
}

func (as *fakeAddressSet) deleteIP(ip net.IP) error {
	Expect(as.destroyed).To(BeFalse())
	delete(as.ips, ip.String())
	return nil
}

func (as *fakeAddressSet) destroyInternal() {
	Expect(as.destroyed).To(BeFalse())
	as.destroyed = true
	as.removeFn(as.name)
}

func (as *fakeAddressSet) destroy() error {
	Expect(as.destroyed).To(BeFalse())
	as.destroyInternal()
	return nil
}
