package egressfirewalldns

import (
	"net"
	"sync"
	"time"

	factory "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	dnsobject "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/dnsobject/v1"
	dnsobjectapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/dnsobject/v1"

	errors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/klog/v2"
)

type EgressDNS struct {
	// Protects pdMap/namespaces operations
	lock sync.Mutex
	// holds DNS entries globally
	dns *util.DNS
	// name of the node
	nodeName string
	// this map holds dnsNames to the dnsEntries
	dnsEntries map[string]*dnsEntry
	// allows to get object using the informer chace
	wf factory.NodeWatchFactory
	k  kube.Interface

	// Report change when Add operation is done
	added          chan dnsNamespace
	removed        chan string
	stopChan       chan struct{}
	controllerStop <-chan struct{}
}

type dnsEntry struct {
	// the current IP addresses the dnsName resolves to
	dnsResolves []net.IP
	namespaces  map[string]struct{} //the namespaces that this DNSName applies to
}

type dnsNamespace struct {
	dnsName   string
	namespace string
}

func NewEgressDNS(nodeName string, watchFactory factory.NodeWatchFactory, k kube.Interface, controllerStop <-chan struct{}) (*EgressDNS, error) {
	dnsInfo, err := util.NewDNS("/etc/resolv.conf")
	if err != nil {
		return nil, err
	}

	egressDNS := &EgressDNS{
		dns:        dnsInfo,
		nodeName:   nodeName,
		dnsEntries: make(map[string]*dnsEntry),
		wf:         watchFactory,
		k:          k,

		added:          make(chan dnsNamespace, 1),
		removed:        make(chan string, 1),
		stopChan:       make(chan struct{}),
		controllerStop: controllerStop,
	}

	return egressDNS, nil
}

func (e *EgressDNS) Add(dnsNames []string, namespace string) error {
	e.lock.Lock()
	defer e.lock.Unlock()
	for _, dnsName := range dnsNames {
		if _, exists := e.dnsEntries[dnsName]; !exists {
			e.dnsEntries[dnsName] = &dnsEntry{
				namespaces: map[string]struct{}{
					namespace: {},
				},
			}
			e.signalAdded(dnsNamespace{dnsName: dnsName, namespace: namespace})
		} else {
			// only need to add the namespace to the struct
			e.dnsEntries[dnsName].namespaces[namespace] = struct{}{}
		}
	}
	return nil

}

func (e *EgressDNS) updateEntryForName(dnsNamespace dnsNamespace) error {
	e.lock.Lock()
	defer e.lock.Unlock()
	ips := e.dns.GetIPs(dnsNamespace.dnsName)
	//TODO: if the old and new resolutions are the same should not update the dnsObject
	e.dnsEntries[dnsNamespace.dnsName].dnsResolves = ips

	//update the dnsObject
	dnsObject, err := e.wf.GetDNSObject(e.nodeName)
	existed := true
	if errors.IsNotFound(err) {
		// the object *MOST LIKELY* has not been created yet
		dnsObject = &dnsobjectapi.DNSObject{
			ObjectMeta: metav1.ObjectMeta{Name: e.nodeName},
			Spec:       dnsobjectapi.DNSObjectSpec{},
		}
		existed = false
	} else if err != nil {
		return err
	}
	var ipStrings []string
	for _, ip := range e.dnsEntries[dnsNamespace.dnsName].dnsResolves {
		ipStrings = append(ipStrings, ip.String())
	}

	if dnsObject.Spec.DNSObjectEntries == nil {
		dnsObject.Spec.DNSObjectEntries = make(map[string]dnsobject.DNSObjectEntry)
	}

	dnsObjectEntry := dnsObject.Spec.DNSObjectEntries[dnsNamespace.dnsName]
	dnsObjectEntry.IPAddresses = ipStrings
	dnsObject.Spec.DNSObjectEntries[dnsNamespace.dnsName] = dnsObjectEntry

	if existed {
		err = e.k.UpdateDNSObject(dnsObject)
	} else {
		_, err = e.k.CreateDNSObject(dnsObject)
	}
	if err != nil {
		return err
	}

	return nil
}

func (e *EgressDNS) Update(dns string) (bool, error) {
	return e.dns.Update(dns)
}

// Run spawns a goroutine that handles updates to the dns entries for dnsNames used in
// EgressFirewalls. The loop runs after receiving one of two signals
// 1. a new dnsName has been added and a signal is sent to add the new DNS name, if an
//    EgressFirewall uses a DNS name already added by another egressFirewall the previous
//    entry is used
// 2. If the defaultInterval has run (30 min) without updating the DNS server is manually queried
func (e *EgressDNS) Run(defaultInterval time.Duration) {
	var dnsNamespace dnsNamespace
	var ttl time.Time
	var timeSet bool
	// initially the next DNS Query happens at the default interval
	durationTillNextQuery := defaultInterval
	go func() {
		for {
			// Wait for the given duration or until something gets added
			select {
			case dnsNamespace := <-e.added:
				if err := e.dns.Add(dnsNamespace.dnsName); err != nil {
					utilruntime.HandleError(err)
				}
				if err := e.updateEntryForName(dnsNamespace); err != nil {
					utilruntime.HandleError(err)
				}
			case <-time.After(durationTillNextQuery):
				if len(dnsNamespace.dnsName) > 0 {
					if _, err := e.Update(dnsNamespace.dnsName); err != nil {
						utilruntime.HandleError(err)
					}
					if err := e.updateEntryForName(dnsNamespace); err != nil {
						utilruntime.HandleError(err)
					}
				}
			case dnsName := <-e.removed:
				// when there is a dnsName removed the query time should be updated
				e.dns.Delete(dnsName)
			case <-e.stopChan:
				return
			case <-e.controllerStop:
				return
			}

			// before waiting on the signals get the next time this thread needs to wake up
			ttl, dnsNamespace.dnsName, timeSet = e.dns.GetNextQueryTime()
			if time.Until(ttl) > defaultInterval || !timeSet {
				durationTillNextQuery = defaultInterval
			} else {
				durationTillNextQuery = time.Until(ttl)
			}
			dnsNamespace.namespace = ""
		}
	}()

}

func (e *EgressDNS) Remove(dnsNames []string, namespace string) error {
	e.lock.Lock()
	defer e.lock.Unlock()

	for _, dnsName := range dnsNames {
		delete(e.dnsEntries[dnsName].namespaces, namespace)
		if len(e.dnsEntries[dnsName].namespaces) == 0 {
			delete(e.dnsEntries, dnsName)
			e.signalRemoved(dnsName)
			dnsObject, err := e.wf.GetDNSObject(e.nodeName)
			if err != nil {
				return err
			}
			if _, exists := dnsObject.Spec.DNSObjectEntries[dnsName]; exists {
				delete(dnsObject.Spec.DNSObjectEntries, dnsName)
				if len(dnsObject.Spec.DNSObjectEntries) == 0 {
					klog.Infof("Deleteing DNSObject %s from the cluster", dnsObject.Name)
					err := e.k.DeleteDNSObject(dnsObject.Name)
					if err != nil {
						return err
					}

				} else {
					err := e.k.UpdateDNSObject(dnsObject)
					if err != nil {
						return err
					}
				}

			}

		}

	}
	return nil

}

func (e *EgressDNS) Shutdown() {
	close(e.stopChan)
}

func (e *EgressDNS) signalRemoved(dnsName string) {
	e.removed <- dnsName
}

func (e *EgressDNS) signalAdded(dnsNS dnsNamespace) {
	e.added <- dnsNS
}

// *** Functions below are used for testing purposes only ***

// returns the fields of a dnsEntry for a given name
func (e *EgressDNS) GetDNSEntry(dnsName string) ([]net.IP, []string, bool) {
	e.lock.Lock()
	defer e.lock.Unlock()
	var namespaces []string
	if dnsEntry, exists := e.dnsEntries[dnsName]; exists {
		for namespace := range dnsEntry.namespaces {
			namespaces = append(namespaces, namespace)
		}

		return dnsEntry.dnsResolves, namespaces, true
	}
	return nil, nil, false

}
