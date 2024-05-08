package dnsnameresolver

import (
	"fmt"
	"net"
	"sync"
	"time"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/klog/v2"
)

type EgressDNS struct {
	// Protects pdMap/namespaces operations
	lock sync.Mutex
	// holds DNS entries globally
	dns *util.DNS
	// this map holds dnsNames to the dnsEntries
	dnsEntries map[string]*dnsEntry
	// allows for the creation of addresssets
	addressSetFactory addressset.AddressSetFactory
	controllerName    string
	// default interval of time to send DNS lookup
	// requests.
	defaultInterval time.Duration

	// Report change when Add operation is done
	added          chan struct{}
	deleted        chan string
	stopChan       chan struct{}
	controllerStop <-chan struct{}
}

var _ DNSNameResolver = &EgressDNS{}

type dnsEntry struct {
	// this map holds all the namespaces that a dnsName appears in
	namespaces map[string]struct{}
	// the current IP addresses the dnsName resolves to
	// NOTE: used for testing
	dnsResolves []net.IP
	// the addressSet that contains the current IPs
	dnsAddressSet addressset.AddressSet
}

func GetEgressFirewallDNSAddrSetDbIDs(dnsName, controller string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.AddressSetEgressFirewallDNS, controller,
		map[libovsdbops.ExternalIDKey]string{
			// dns address sets are cluster-wide objects, they have unique names
			libovsdbops.ObjectNameKey: dnsName,
		})
}

func NewEgressDNS(addressSetFactory addressset.AddressSetFactory, controllerName string,
	controllerStop <-chan struct{}, defaultInterval time.Duration) (*EgressDNS, error) {
	dnsInfo, err := util.NewDNS("/etc/resolv.conf")
	if err != nil {
		return nil, err
	}

	egressDNS := &EgressDNS{
		dns:               dnsInfo,
		dnsEntries:        make(map[string]*dnsEntry),
		addressSetFactory: addressSetFactory,
		controllerName:    controllerName,
		defaultInterval:   defaultInterval,

		added:          make(chan struct{}, 1),
		deleted:        make(chan string, 1),
		stopChan:       make(chan struct{}),
		controllerStop: controllerStop,
	}

	return egressDNS, nil
}

func (e *EgressDNS) Add(namespace, dnsName string) (addressset.AddressSet, error) {
	e.lock.Lock()
	defer e.lock.Unlock()

	if _, exists := e.dnsEntries[dnsName]; !exists {
		var err error
		dnsEntry := dnsEntry{
			namespaces: make(map[string]struct{}),
		}
		if e.addressSetFactory == nil {
			return nil, fmt.Errorf("error adding EgressFirewall DNS rule for host %s, in namespace %s: addressSetFactory is nil", dnsName, namespace)
		}
		asIndex := GetEgressFirewallDNSAddrSetDbIDs(dnsName, e.controllerName)
		dnsEntry.dnsAddressSet, err = e.addressSetFactory.NewAddressSet(asIndex, nil)
		if err != nil {
			return nil, fmt.Errorf("cannot create addressSet for %s: %v", dnsName, err)
		}
		e.dnsEntries[dnsName] = &dnsEntry
		go e.addToDNS(dnsName)
	}
	e.dnsEntries[dnsName].namespaces[namespace] = struct{}{}
	return e.dnsEntries[dnsName].dnsAddressSet, nil
}

func (e *EgressDNS) Delete(namespace string) error {
	e.lock.Lock()
	var dnsNamesToDelete []string

	// go through all dnsNames for namespaces
	for dnsName, dnsEntry := range e.dnsEntries {
		// delete the dnsEntry
		delete(dnsEntry.namespaces, namespace)
		if len(dnsEntry.namespaces) == 0 {
			// the dnsEntry appears in no other namespace, so delete the address_set
			err := dnsEntry.dnsAddressSet.Destroy()
			if err != nil {
				return fmt.Errorf("error deleting EgressFirewall AddressSet for dnsName: %s %v", dnsName, err)
			}
			// the dnsEntry is no longer needed because nothing references it, so delete it
			delete(e.dnsEntries, dnsName)
			dnsNamesToDelete = append(dnsNamesToDelete, dnsName)
		}
	}
	e.lock.Unlock()
	for _, name := range dnsNamesToDelete {
		e.dns.Delete(name)
		// send a message to the "deleted" buffered channel so that Run() stops using
		// the deleted domain name. (channel is buffered so that sending values to it
		// blocks only if Run() is busy updating its internal values)
		e.deleted <- name
	}
	return nil
}

func (e *EgressDNS) Update(dnsName string) (bool, error) {
	return e.dns.Update(dnsName)
}

func (e *EgressDNS) updateEntryForName(dnsName string) error {
	e.lock.Lock()
	defer e.lock.Unlock()
	ips := e.dns.GetIPs(dnsName)
	if _, ok := e.dnsEntries[dnsName]; !ok {
		return fmt.Errorf("cannot update DNS record for %s: no entry found. "+
			"Was the EgressFirewall deleted?", dnsName)
	}
	e.dnsEntries[dnsName].dnsResolves = ips

	// ignore ips from clusterSubnet, since this subnet shouldn't be affected by egress firewall
	ipsNoClusterSubnet := []net.IP{}
	for _, ip := range ips {
		fromClusterSubnet := false
		for _, clusterSubnet := range config.Default.ClusterSubnets {
			if clusterSubnet.CIDR.Contains(ip) {
				fromClusterSubnet = true
				break
			}
		}
		if !fromClusterSubnet {
			// no intersection, add ip
			ipsNoClusterSubnet = append(ipsNoClusterSubnet, ip)
		}
	}
	if err := e.dnsEntries[dnsName].dnsAddressSet.SetAddresses(util.StringSlice(ipsNoClusterSubnet)); err != nil {
		return fmt.Errorf("cannot add IPs from EgressFirewall AddressSet %s: %v", dnsName, err)
	}
	return nil
}

// addToDNS takes the dnsName adds it to the underlying dns resolver and
// performs the first update. After completing that signals the
// thread performing periodic updates that a new DNS name has been added and
// so that it can updates GetNextQueryTime() if needed
func (e *EgressDNS) addToDNS(dnsName string) {
	if err := e.dns.Add(dnsName); err != nil {
		utilruntime.HandleError(err)
	}
	if err := e.updateEntryForName(dnsName); err != nil {
		utilruntime.HandleError(err)
	}
	// No need to block waiting to signal the add.
	select {
	case e.added <- struct{}{}:
		klog.V(5).Infof("Recalculation of next query time requested")
	default:
		klog.V(5).Infof("Recalculation of next query time already requested")
	}
}

// Run spawns a goroutine that handles updates to the dns entries for domain names used in
// EgressFirewalls. The loop runs after receiving one of three signals:
//  1. time.NewTicker(durationTillNextQuery) times out and the dnsName with the lowest ttl is checked
//     and the durationTillNextQuery is updated
//  2. e.added is received and durationTillNextQuery is recomputed
//  3. e.deleted is received and coincides with dnsName
func (e *EgressDNS) Run() error {
	var domainNameExpiringNext, domainNameDeleted string
	var ttl time.Time
	var timeSet bool
	// initially the next DNS Query happens at the default interval
	durationTillNextQuery := e.defaultInterval
	go func() {
		timer := time.NewTicker(durationTillNextQuery)
		defer timer.Stop()
		for {
			// perform periodic updates on dnsNames as each ttl runs out, checking for updates at
			// least every defaultInterval. Update durationTillNextQuery everytime a new DNS name gets
			// added
			select {
			case <-e.added:
				//on update need to check if the GetNextQueryTime has changed
			case <-timer.C:
				if len(domainNameExpiringNext) > 0 {
					if _, err := e.Update(domainNameExpiringNext); err != nil {
						utilruntime.HandleError(err)
					}
					if err := e.updateEntryForName(domainNameExpiringNext); err != nil {
						utilruntime.HandleError(err)
					}
				}
			case domainNameDeleted = <-e.deleted:
				// If domainNameExpiringNext we are waiting to update was deleted,
				// recalculate durationTillNextQuery and domainNameExpiringNext.
				// Otherwise, ignore this event
				if domainNameExpiringNext != domainNameDeleted {
					continue
				}
			case <-e.stopChan:
				return
			case <-e.controllerStop:
				return
			}
			// find the domain name whose DNS entry will expire first and calculate when it will expire,
			// set timer to what's sooner: default update interval or next expiration time
			ttl, domainNameExpiringNext, timeSet = e.dns.GetNextQueryTime()
			ttlDuration := time.Until(ttl)
			if ttlDuration > e.defaultInterval || !timeSet {
				durationTillNextQuery = e.defaultInterval
			} else if ttlDuration.Seconds() > 0 {
				durationTillNextQuery = ttlDuration
			} else {
				// DNS entry is already expired, so trigger tick as soon as possible.
				durationTillNextQuery = 1 * time.Millisecond
			}
			timer.Reset(durationTillNextQuery)
		}
	}()

	return nil
}

func (e *EgressDNS) Shutdown() {
	close(e.stopChan)
}

// DeleteStaleAddrSets deletes all the address sets related to EgressFirewall DNS rules which are not
// referenced by any acl.
func (e *EgressDNS) DeleteStaleAddrSets(nbClient libovsdbclient.Client) error {
	e.lock.Lock()
	defer e.lock.Unlock()

	predicateIDs := libovsdbops.NewDbObjectIDs(libovsdbops.AddressSetEgressFirewallDNS, e.controllerName, nil)
	return libovsdbutil.DeleteAddrSetsWithoutACLRef(predicateIDs, nbClient)
}
