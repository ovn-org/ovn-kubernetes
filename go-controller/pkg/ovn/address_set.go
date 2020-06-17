package ovn

import (
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	"k8s.io/klog"
)

type AddressSetIterFunc func(hashedName, namespace, suffix string)
type AddressSetDoFunc func(as AddressSet) error

// AddressSetFactory is an interface for managing address set objects
type AddressSetFactory interface {
	// NewAddressSet returns a new object that implements AddressSet
	// and contains the given IPs, or an error
	NewAddressSet(name string, ips []net.IP) (AddressSet, error)
	// ForEachAddressSet calls the given function for each address set
	// known to the factory
	ForEachAddressSet(iteratorFn AddressSetIterFunc) error
	// DestroyAddressSetInBackingStore deletes the named address set from the
	// factory's backing store. SHOULD NOT BE CALLED for any address set
	// for which an AddressSet object has been created.
	DestroyAddressSetInBackingStore(name string) error
}

// AddressSet is an interface for address set objects
type AddressSet interface {
	// GetHashName returns the hashed name of the address set
	GetHashName() string
	// GetName returns the descriptive name of the address set
	GetName() string
	AddIP(ip net.IP) error
	DeleteIP(ip net.IP) error
	Destroy() error
}

type ovnAddressSetFactory struct{}

// NewOvnAddressSetFactory creates a new AddressSetFactory backed by
// address set objects that execute OVN commands
func NewOvnAddressSetFactory() AddressSetFactory {
	return &ovnAddressSetFactory{}
}

// ovnAddressSetFactory implements the AddressSetFactory interface
var _ AddressSetFactory = &ovnAddressSetFactory{}

// NewAddressSet returns a new address set object
func (asf *ovnAddressSetFactory) NewAddressSet(name string, ips []net.IP) (AddressSet, error) {
	return newOvnAddressSet(name, ips)
}

// ForEachAddressSet will pass the unhashed address set name, namespace name
// and the first suffix in the name to the 'iteratorFn' for every address_set in
// OVN. (Unhashed address set names are of the form namespaceName[.suffix1.suffix2. .suffixN])
func (asf *ovnAddressSetFactory) ForEachAddressSet(iteratorFn AddressSetIterFunc) error {
	output, stderr, err := util.RunOVNNbctl("--format=csv", "--data=bare", "--no-heading",
		"--columns=external_ids", "find", "address_set")
	if err != nil {
		return fmt.Errorf("error reading address sets: "+
			"stdout: %q, stderr: %q err: %v", output, stderr, err)
	}
	for _, line := range strings.Split(output, "\n") {
		parts := strings.Split(line, ",")
		if len(parts) != 2 {
			continue
		}
		for _, externalID := range strings.Fields(parts[1]) {
			if !strings.HasPrefix(externalID, "name=") {
				continue
			}
			addrSetName := externalID[5:]
			names := strings.Split(addrSetName, ".")
			addrSetNamespace := names[0]
			nameSuffix := ""
			if len(names) >= 2 {
				nameSuffix = names[1]
			}
			iteratorFn(addrSetName, addrSetNamespace, nameSuffix)
			break
		}
	}
	return nil
}

func (asf *ovnAddressSetFactory) DestroyAddressSetInBackingStore(name string) error {
	hashName := hashedAddressSet(name)
	_, stderr, err := util.RunOVNNbctl("--if-exists", "destroy", "address_set", hashName)
	if err != nil {
		return fmt.Errorf("failed to destroy address set %q, stderr: %q, (%v)",
			hashName, stderr, err)
	}
	return nil
}

type ovnAddressSet struct {
	sync.RWMutex
	name     string
	hashName string
	uuid     string
	ips      map[string]net.IP
}

// ovnAddressSet implements the AddressSet interface
var _ AddressSet = &ovnAddressSet{}

// hash the provided input to make it a valid ovnAddressSet name.
func hashedAddressSet(s string) string {
	return hashForOVN(s)
}

func asDetail(as *ovnAddressSet) string {
	return fmt.Sprintf("%s/%s/%s", as.uuid, as.name, as.hashName)
}

func newOvnAddressSet(name string, ips []net.IP) (*ovnAddressSet, error) {
	as := &ovnAddressSet{
		name:     name,
		hashName: hashedAddressSet(name),
		ips:      make(map[string]net.IP),
	}
	for _, ip := range ips {
		as.ips[ip.String()] = ip
	}

	uuid, stderr, err := util.RunOVNNbctl("--data=bare",
		"--no-heading", "--columns=_uuid", "find", "address_set",
		"name="+as.hashName)
	if err != nil {
		return nil, fmt.Errorf("find failed to get address set %q, stderr: %q (%v)",
			as.name, stderr, err)
	}
	as.uuid = uuid

	if uuid != "" {
		klog.V(5).Infof("New(%s) already exists; updating IPs", asDetail(as))
		// ovnAddressSet already exists in the database; just update IPs
		if err := as.setOrClear(); err != nil {
			return nil, err
		}
	} else {
		// ovnAddressSet has not been created yet. Create it.
		args := []string{
			"create",
			"address_set",
			"name=" + as.hashName,
			"external-ids:name=" + as.name,
		}
		joinedIPs := as.joinIPs()
		if len(joinedIPs) > 0 {
			args = append(args, "addresses="+joinedIPs)
		}
		as.uuid, stderr, err = util.RunOVNNbctl(args...)
		if err != nil {
			return nil, fmt.Errorf("failed to create address set %q, stderr: %q (%v)",
				asDetail(as), stderr, err)
		}
	}

	klog.V(5).Infof("New(%s) with %v", asDetail(as), ips)

	return as, nil
}

func (as *ovnAddressSet) GetHashName() string {
	return as.hashName
}

func (as *ovnAddressSet) GetName() string {
	return as.name
}

func (as *ovnAddressSet) joinIPs() string {
	list := make([]string, 0, len(as.ips))
	for ipStr := range as.ips {
		list = append(list, `"`+ipStr+`"`)
	}
	sort.Strings(list)
	return strings.Join(list, " ")
}

// setOrClear updates the OVN database with the address set's addresses or
// clears the address set if there are no addresses in the address set
func (as *ovnAddressSet) setOrClear() error {
	joinedIPs := as.joinIPs()
	if len(joinedIPs) > 0 {
		_, stderr, err := util.RunOVNNbctl("set", "address_set", as.uuid, "addresses="+joinedIPs)
		if err != nil {
			return fmt.Errorf("failed to set address set %q, stderr: %q (%v)",
				asDetail(as), stderr, err)
		}
	} else {
		_, stderr, err := util.RunOVNNbctl("clear", "address_set", as.uuid, "addresses")
		if err != nil {
			return fmt.Errorf("failed to clear address set %q, stderr: %q (%v)",
				asDetail(as), stderr, err)
		}
	}
	return nil
}

func (as *ovnAddressSet) AddIP(ip net.IP) error {
	as.Lock()
	defer as.Unlock()

	ipStr := ip.String()
	if _, ok := as.ips[ipStr]; ok {
		return nil
	}

	klog.V(5).Infof("(%s) adding IP %s to address set", asDetail(as), ipStr)

	_, stderr, err := util.RunOVNNbctl("add", "address_set", as.uuid, "addresses", `"`+ipStr+`"`)
	if err != nil {
		return fmt.Errorf("failed to add IP %q to address set %q, stderr: %q (%v)",
			ip, asDetail(as), stderr, err)
	}

	as.ips[ip.String()] = ip
	return nil
}

func (as *ovnAddressSet) DeleteIP(ip net.IP) error {
	as.Lock()
	defer as.Unlock()

	ipStr := ip.String()
	if _, ok := as.ips[ipStr]; !ok {
		return nil
	}

	klog.V(5).Infof("(%s) deleting IP %s from address set", asDetail(as), ipStr)

	_, stderr, err := util.RunOVNNbctl("remove", "address_set", as.uuid, "addresses", `"`+ipStr+`"`)
	if err != nil {
		return fmt.Errorf("failed to remove IP %q from address set %q, stderr: %q (%v)",
			ip, asDetail(as), stderr, err)
	}

	delete(as.ips, ipStr)
	return nil
}

func (as *ovnAddressSet) Destroy() error {
	as.Lock()
	defer as.Unlock()
	klog.V(5).Infof("Destroy(%s)", asDetail(as))
	_, stderr, err := util.RunOVNNbctl("--if-exists", "destroy", "address_set", as.uuid)
	if err != nil {
		return fmt.Errorf("failed to destroy address set %q, stderr: %q, (%v)",
			asDetail(as), stderr, err)
	}
	as.ips = nil
	return nil
}
