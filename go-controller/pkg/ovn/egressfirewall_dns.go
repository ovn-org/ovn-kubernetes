package ovn

import (
	"fmt"
	"net"
	"sync"
	"time"

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

	// Report change when Add operation is done
	added          chan struct{}
	deleted        chan string
	stopChan       chan struct{}
	controllerStop <-chan struct{}
}

type dnsTimedEntry struct {
	ip net.IP
	ts time.Time
}

type dnsEntry struct {
	// this map holds all the namespaces that a dnsName appears in
	namespaces map[string]struct{}
	// the current IP addresses the dnsName resolves to
	// NOTE: used for testing
	dnsResolves map[string]dnsTimedEntry
	// the addressSet that contains the current IPs
	dnsAddressSet addressset.AddressSet
}

func NewEgressDNS(addressSetFactory addressset.AddressSetFactory, controllerStop <-chan struct{}) (*EgressDNS, error) {
	dnsInfo, err := util.NewDNS("/etc/resolv.conf")
	if err != nil {
		return nil, err
	}

	egressDNS := &EgressDNS{
		dns:               dnsInfo,
		dnsEntries:        make(map[string]*dnsEntry),
		addressSetFactory: addressSetFactory,

		added:          make(chan struct{}),
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
		dnsEntry.dnsAddressSet, err = e.addressSetFactory.NewAddressSet(dnsName, nil)
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

func (e *EgressDNS) Update(dns string) (bool, error) {
	return e.dns.Update(dns)
}

// serve stale entries for this duration
const dnsEntryTimeout = 7200 * time.Second

// maximum number of entries to keep for one DNS name
const maxDnsEntry = 4000

// merge the IPs the DNS server answered with the existing IPs we know
// about for a DNS entry
func (e *EgressDNS) merge(dnsName string, ips []net.IP) []net.IP {
	tsNow := time.Now()
	tsUntil := tsNow.Add(dnsEntryTimeout).Round(10 * time.Minute)

	if e.dnsEntries[dnsName].dnsResolves == nil {
		e.dnsEntries[dnsName].dnsResolves = make(map[string]dnsTimedEntry)
	}

	// merge new ips into existing entries
	for _, ip := range ips {
		e.dnsEntries[dnsName].dnsResolves[ip.String()] = dnsTimedEntry{ip, tsUntil}
	}

	actualIps := make([]net.IP, 0, len(ips))
	// clean-up expired entries
	for key, entry := range e.dnsEntries[dnsName].dnsResolves {
		if entry.ts.Before(tsNow) || len(actualIps) > maxDnsEntry {
			delete(e.dnsEntries[dnsName].dnsResolves, key)
		} else {
			actualIps = append(actualIps, entry.ip)
		}
	}
	return actualIps
}

func (e *EgressDNS) updateEntryForName(dnsName string) error {
	e.lock.Lock()
	defer e.lock.Unlock()
	ips := e.dns.GetIPs(dnsName)
	if _, ok := e.dnsEntries[dnsName]; !ok {
		return fmt.Errorf("cannot update DNS record for %s: no entry found. "+
			"Was the EgressFirewall deleted?", dnsName)
	}
	mergedIps := e.merge(dnsName, ips)

	if err := e.dnsEntries[dnsName].dnsAddressSet.SetIPs(mergedIps); err != nil {
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
func (e *EgressDNS) Run(defaultInterval time.Duration) {
	var domainNameExpiringNext, domainNameDeleted string
	var ttl time.Time
	var timeSet bool
	// initially the next DNS Query happens at the default interval
	durationTillNextQuery := defaultInterval
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
			if ttlDuration > defaultInterval || !timeSet {
				durationTillNextQuery = defaultInterval
			} else if ttlDuration.Seconds() > 0 {
				durationTillNextQuery = ttlDuration
			} else {
				// DNS entry is already expired, so trigger tick as soon as possible.
				durationTillNextQuery = 1 * time.Millisecond
			}
			timer.Reset(durationTillNextQuery)
		}
	}()

}

func (e *EgressDNS) Shutdown() {
	close(e.stopChan)
}
