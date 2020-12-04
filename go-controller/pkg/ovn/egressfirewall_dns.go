package ovn

import (
	"fmt"
	"net"
	"sync"
	"time"

	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/klog"
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
	added          chan string
	stopChan       chan struct{}
	controllerStop <-chan struct{}
}

type dnsEntry struct {
	// this map holds all the namespaces that a dnsName appears in
	namespaces map[string]struct{}
	// the current IP addresses the dnsName resolves to
	// NOTE: used for testing
	dnsResolves []net.IP
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

		added:          make(chan string, 1),
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
		e.signalAdded(dnsName)
	}
	e.dnsEntries[dnsName].namespaces[namespace] = struct{}{}
	return e.dnsEntries[dnsName].dnsAddressSet, nil

}

func (e *EgressDNS) Delete(namespace string) bool {
	e.lock.Lock()
	var dnsNamesToDelete []string

	// go through all dnsNames for namespaces
	for dnsName, dnsEntry := range e.dnsEntries {
		// delete the dnsEntry
		delete(dnsEntry.namespaces, namespace)
		if len(dnsEntry.namespaces) == 0 {
			// the dnsEntry appears in no other namespace so delete the address_set
			err := dnsEntry.dnsAddressSet.Destroy()
			if err != nil {
				klog.Errorf("Error deleteing EgressFirewall AddressSet for dnsName: %s %v", dnsName, err)
			}
			// the dnsEntry is no longer needed because nothing references it delete it
			delete(e.dnsEntries, dnsName)
			dnsNamesToDelete = append(dnsNamesToDelete, dnsName)
		}
	}
	e.lock.Unlock()
	for _, name := range dnsNamesToDelete {
		e.dns.Delete(name)
	}
	return len(e.dnsEntries) == 0
}

func (e *EgressDNS) Update(dns string) (bool, error) {
	return e.dns.Update(dns)
}

func (e *EgressDNS) updateEntryForName(dnsName string) error {
	e.lock.Lock()
	defer e.lock.Unlock()
	ips := e.dns.GetIPs(dnsName)
	e.dnsEntries[dnsName].dnsResolves = ips

	if err := e.dnsEntries[dnsName].dnsAddressSet.SetIPs(ips); err != nil {
		return fmt.Errorf("cannot add IPs from EgressFirewall AddressSet %s: %v", dnsName, err)
	}
	return nil
}

// Run spawns a goroutine that handles updates to the dns entries for dnsNames used in
// EgressFirewalls. The loop runs after receiving one of two signals
// 1. a new dnsName has been added and a signal is sent to add the new DNS name, if an
//    EgressFirewall uses a DNS name already added by another egressFirewall the previous
//    entry is used
// 2. If the defaultInterval has run (30 min) without updating the DNS server is manually queried
func (e *EgressDNS) Run(defaultInterval time.Duration) {
	var dnsName string
	var ttl time.Time
	var timeSet bool
	// initially the next DNS Query happens at the default interval
	durationTillNextQuery := defaultInterval
	go func() {
		for {
			// Wait for the given duration or until something gets added
			select {
			case dnsName := <-e.added:
				if err := e.dns.Add(dnsName); err != nil {
					utilruntime.HandleError(err)
				}
				if err := e.updateEntryForName(dnsName); err != nil {
					utilruntime.HandleError(err)
				}
			case <-time.After(durationTillNextQuery):
				if len(dnsName) > 0 {
					if _, err := e.Update(dnsName); err != nil {
						utilruntime.HandleError(err)
					}
					if err := e.updateEntryForName(dnsName); err != nil {
						utilruntime.HandleError(err)
					}
				}
			case <-e.stopChan:
				return
			case <-e.controllerStop:
				return
			}

			// before waiting on the signals get the next time this thread needs to wake up
			ttl, dnsName, timeSet = e.dns.GetNextQueryTime()
			if time.Until(ttl) > defaultInterval || !timeSet {
				durationTillNextQuery = defaultInterval
			} else {
				durationTillNextQuery = time.Until(ttl)
			}
		}
	}()

}

func (e *EgressDNS) Shutdown() {
	close(e.stopChan)
}

func (e *EgressDNS) signalAdded(dnsName string) {
	e.added <- dnsName
}
