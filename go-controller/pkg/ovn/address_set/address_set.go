package addressset

import (
	"fmt"
	goovn "github.com/ebay/go-ovn"
	"net"
	"sort"
	"strings"

	"github.com/pkg/errors"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
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
	PrepareAddIPsCmds(ip []net.IP) ([]*goovn.OvnCommand, error)
}

type ovnAddressSetFactory struct {
	nb goovn.Client
}

// NewOvnAddressSetFactory creates a new AddressSetFactory backed by
// address set objects that execute OVN commands
func NewOvnAddressSetFactory(nb goovn.Client) AddressSetFactory {
	return &ovnAddressSetFactory{nb: nb}
}

// ovnAddressSetFactory implements the AddressSetFactory interface
var _ AddressSetFactory = &ovnAddressSetFactory{}

// NewAddressSet returns a new address set object
func (asf *ovnAddressSetFactory) NewAddressSet(name string, ips []net.IP) (AddressSet, error) {
	res, err := newOvnAddressSets(asf.nb, name, ips)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// EnsureAddressSet ensures the address_set with the given name exists and if it does not creates an empty addressSet
func (asf *ovnAddressSetFactory) EnsureAddressSet(name string) error {
	hashedAddressSetNames := []string{}
	ip4ASName, ip6ASName := MakeAddressSetHashNames(name)
	if config.IPv4Mode {
		hashedAddressSetNames = append(hashedAddressSetNames, ip4ASName)
	}
	if config.IPv6Mode {
		hashedAddressSetNames = append(hashedAddressSetNames, ip6ASName)
	}
	for _, hashedAddressSetName := range hashedAddressSetNames {
		_, err := newOvnAddressSet(asf.nb, hashedAddressSetName, nil)
		if err != nil {
			return err
		}
	}

	return nil
}

func forEachAddressSet(nbClient goovn.Client, do func(string)) error {
	addrSets, err := nbClient.ASList()
	if err != nil && err != goovn.ErrorSchema {
		return fmt.Errorf("error reading address sets: %v", err)
	}

	for _, set := range addrSets {
		nameI, ok := set.ExternalID["name"]
		if !ok {
			continue
		}
		name, ok := nameI.(string)
		if !ok {
			continue
		}
		do(name)
	}
	return nil
}

// ProcessEachAddressSet will pass the unhashed address set name, namespace name
// and the first suffix in the name to the 'iteratorFn' for every address_set in
// OVN. (Unhashed address set names are of the form namespaceName[.suffix1.suffix2. .suffixN])
func (asf *ovnAddressSetFactory) ProcessEachAddressSet(iteratorFn AddressSetIterFunc) error {
	processedAddressSets := sets.String{}
	err := forEachAddressSet(asf.nb, func(name string) {
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
	err := destroyAddressSet(asf.nb, name)
	if err != nil {
		return err
	}
	ip4ASName, ip6ASName := MakeAddressSetName(name)
	err = destroyAddressSet(asf.nb, ip4ASName)
	if err != nil {
		return err
	}
	err = destroyAddressSet(asf.nb, ip6ASName)
	if err != nil {
		return err
	}
	return nil
}

func destroyAddressSet(nb goovn.Client, name string) error {
	hashName := hashedAddressSet(name)
	cmd, err := nb.ASDel(hashName)
	if err != nil {
		return fmt.Errorf("failed to create delete cmd for address set %q: %v", hashName, err)
	}
	if err := nb.Execute(cmd); err != nil && err != goovn.ErrorNotFound {
		return fmt.Errorf("failed to destroy address set %q: %v", hashName, err)
	}
	return nil
}

type ovnAddressSet struct {
	name     string
	hashName string
	nb       goovn.Client
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
	return fmt.Sprintf("%s/%s", as.name, as.hashName)
}

func newOvnAddressSets(nb goovn.Client, name string, ips []net.IP) (*ovnAddressSets, error) {
	var (
		v4set, v6set *ovnAddressSet
		err          error
	)
	v4IPs, v6IPs := splitIPsByFamily(ips)

	ip4ASName, ip6ASName := MakeAddressSetName(name)
	if config.IPv4Mode {
		v4set, err = newOvnAddressSet(nb, ip4ASName, v4IPs)
		if err != nil {
			return nil, err
		}
	}
	if config.IPv6Mode {
		v6set, err = newOvnAddressSet(nb, ip6ASName, v6IPs)
		if err != nil {
			return nil, err
		}
	}
	return &ovnAddressSets{name: name, ipv4: v4set, ipv6: v6set}, nil
}

func newOvnAddressSet(nb goovn.Client, name string, ips []net.IP) (*ovnAddressSet, error) {
	as := &ovnAddressSet{
		name:     name,
		hashName: hashedAddressSet(name),
		nb:       nb,
	}

	_, err := nb.ASGet(as.hashName)
	if err != nil {
		if err != goovn.ErrorNotFound && err != goovn.ErrorSchema {
			return nil, fmt.Errorf("failed to get address set %q: %v", name, err)
		}
		// ovnAddressSet has not been created yet. Create it.
		cmd, err := nb.ASAdd(as.hashName, ipsToStringArray(ips), map[string]string{"name": name})
		if err != nil {
			return nil, fmt.Errorf("failed to create address set cmd: %q: %v", name, err)
		}
		if err := nb.Execute(cmd); err != nil {
			return nil, fmt.Errorf("failed to create address set %q: %v", asDetail(as), err)
		}
	} else {
		klog.V(5).Infof("New(%s) already exists; updating IPs", asDetail(as))
		if err := as.setIPs(ips); err != nil {
			return nil, err
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

func (as *ovnAddressSets) PrepareAddIPsCmds(ips []net.IP) ([]*goovn.OvnCommand, error) {
	if len(ips) == 0 {
		return nil, nil
	}

	v4ips, v6ips := splitIPsByFamily(ips)
	var cmds []*goovn.OvnCommand
	if as.ipv6 != nil {
		if cmd, err := as.ipv6.addIPsCmd(v6ips); err != nil {
			return nil, fmt.Errorf("failed to AddIPs to the v6 set: %w", err)
		} else {
			cmds = append(cmds, cmd)
		}
	}
	if as.ipv4 != nil {
		if cmd, err := as.ipv4.addIPsCmd(v4ips); err != nil {
			return nil, fmt.Errorf("failed to AddIPs to the v4 set: %w", err)
		} else {
			cmds = append(cmds, cmd)
		}
	}

	return cmds, nil
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
	newIPs := ipsToStringArray(ips)
	cmd, err := as.nb.ASUpdate(as.hashName, newIPs, map[string]string{"name": as.name})
	if err != nil {
		return fmt.Errorf("failed to create update for address set %q: %v", asDetail(as), err)
	}
	if err := as.nb.Execute(cmd); err != nil {
		return fmt.Errorf("failed to execute update for address set %q: %v", asDetail(as), err)
	}

	return nil
}

// addIPs appends the set of IPs to the existing address_set.
func (as *ovnAddressSet) addIPs(ips []net.IP) error {
	cmd, err := as.addIPsCmd(ips)
	if err != nil {
		return err
	}
	if err := as.nb.Execute(cmd); err != nil {
		return fmt.Errorf("failed to execute add ips for address set %q: %v", asDetail(as), err)
	}

	return nil
}

// addIPsCmd appends the set of IPs to the existing address_set. Returns the goovn command
func (as *ovnAddressSet) addIPsCmd(ips []net.IP) (*goovn.OvnCommand, error) {
	if len(ips) == 0 {
		return nil, nil
	}
	uniqIPs := make([]string, 0, len(ips))
	for _, ip := range ips {
		uniqIPs = append(uniqIPs, ip.String())
	}

	cmd, err := as.nb.ASAddIPs(as.hashName, uniqIPs)
	if err != nil {
		return nil, fmt.Errorf("failed to create add ips cmd for address set %q: %v", asDetail(as), err)
	}

	return cmd, nil
}

// deleteIPs removes selected IPs from the existing address_set
func (as *ovnAddressSet) deleteIPs(ips []net.IP) error {
	cmd, err := as.deleteIPsCmd(ips)
	if err != nil {
		return err
	}
	if err := as.nb.Execute(cmd); err != nil {
		return fmt.Errorf("failed to execute delete ips for address set %q: %v", asDetail(as), err)
	}

	return nil
}

// deleteIPs removes selected IPs from the existing address_set
func (as *ovnAddressSet) deleteIPsCmd(ips []net.IP) (*goovn.OvnCommand, error) {
	if len(ips) == 0 {
		return nil, nil
	}
	uniqIPs := make([]string, 0, len(ips))
	for _, ip := range ips {
		uniqIPs = append(uniqIPs, ip.String())
	}
	ipStr := joinIPs(ips)

	cmd, err := as.nb.ASDelIPs(as.hashName, uniqIPs)
	if err != nil {
		return nil, fmt.Errorf("failed to create add ips cmd for address set %q, ips: %s: %v", asDetail(as), ipStr, err)
	}
	return cmd, nil
}

func (as *ovnAddressSet) destroy() error {
	klog.V(5).Infof("destroy(%s)", asDetail(as))
	cmd, err := as.nb.ASDel(as.hashName)
	if err != nil {
		return fmt.Errorf("failed to create delete for address set %q: %v", asDetail(as), err)
	}
	if err := as.nb.Execute(cmd); err != nil {
		return fmt.Errorf("failed to destroy address set %q: %v", asDetail(as), err)
	}
	return nil
}

func ipsToStringArray(ips []net.IP) []string {
	out := make([]string, 0, len(ips))
	for _, ip := range ips {
		out = append(out, ip.String())
	}
	return out
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

func joinIPs(ips []net.IP) string {
	list := make([]string, 0, len(ips))
	for _, ip := range ips {
		list = append(list, `"`+ip.String()+`"`)
	}
	// so tests are predictable
	sort.Strings(list)
	return strings.Join(list, " ")
}
