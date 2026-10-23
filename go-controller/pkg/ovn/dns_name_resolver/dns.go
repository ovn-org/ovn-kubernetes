// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package dnsnameresolver

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
	"unicode"

	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/klog/v2"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// EgressDNS tracks DNS names used by legacy EgressFirewall rules and manages
// their OVN address sets.
type EgressDNS struct {
	// Protects pdMap/namespaces operations
	lock sync.Mutex
	// holds DNS entries globally
	dns *util.DNS
	// this map holds dnsNames to the dnsEntries
	dnsEntries map[string]*dnsEntry
	// entries removed from dnsEntries whose asynchronous DNS cleanup is pending
	pendingDNSDeletes map[string]*dnsEntry
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
	// Serialize asynchronous DNS add/delete operations for this entry. Add waits
	// for this entry's cleanup before installing a replacement for the same name.
	dnsOperationLock sync.Mutex
	// this map holds all the namespaces that a dnsName appears in
	namespaces map[string]struct{}
	// number of Add-to-ACL handoffs still in flight, keyed by namespace
	pendingACLAttachments map[string]int
	// closed after this entry's DNS tracker value has been removed
	dnsDeleteDone chan struct{}
	// the current IP addresses the dnsName resolves to
	// NOTE: used for testing
	dnsResolves []net.IP
	// the addressSet that contains the current IPs
	dnsAddressSet addressset.AddressSet
}

// GetEgressFirewallDNSAddrSetDbIDs returns deterministic database IDs for the
// address set shared by EgressFirewall rules that use dnsName.
func GetEgressFirewallDNSAddrSetDbIDs(dnsName, controller string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.AddressSetEgressFirewallDNS, controller,
		map[libovsdbops.ExternalIDKey]string{
			// dns address sets are cluster-wide objects, they have unique names
			libovsdbops.ObjectNameKey: dnsName,
		})
}

// NewEgressDNS creates a resolver that uses the node's DNS configuration and
// refreshes tracked EgressFirewall names at defaultInterval or when a TTL expires.
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

// Add returns the address set used by an EgressFirewall rule for dnsName.
// The returned set is protected from stale-set cleanup until the caller
// completes the corresponding ACL transaction with CompleteAdd.
func (e *EgressDNS) Add(namespace, dnsName string) (addressset.AddressSet, error) {
	for {
		e.lock.Lock()
		if deletingEntry, deleting := e.pendingDNSDeletes[dnsName]; deleting {
			done := deletingEntry.dnsDeleteDone
			e.lock.Unlock()
			<-done
			continue
		}

		entry, exists := e.dnsEntries[dnsName]
		if !exists {
			entry = &dnsEntry{
				namespaces: make(map[string]struct{}),
			}
			if e.addressSetFactory == nil {
				e.lock.Unlock()
				return nil, fmt.Errorf("error adding EgressFirewall DNS rule for host %s, in namespace %s: addressSetFactory is nil", dnsName, namespace)
			}
			asIndex := GetEgressFirewallDNSAddrSetDbIDs(dnsName, e.controllerName)
			var err error
			entry.dnsAddressSet, err = e.addressSetFactory.NewAddressSet(asIndex, nil)
			if err != nil {
				e.lock.Unlock()
				return nil, fmt.Errorf("cannot create addressSet for %s: %w", dnsName, err)
			}
			e.dnsEntries[dnsName] = entry
			go e.addToDNS(dnsName, entry)
		}
		entry.namespaces[namespace] = struct{}{}
		if entry.pendingACLAttachments == nil {
			entry.pendingACLAttachments = make(map[string]int)
		}
		entry.pendingACLAttachments[namespace]++
		e.lock.Unlock()
		return entry.dnsAddressSet, nil
	}
}

// CompleteAdd releases one pending Add-to-ACL handoff. Call it once for each
// successful Add after the ACL transaction has completed, whether that
// transaction succeeds or fails.
func (e *EgressDNS) CompleteAdd(namespace, dnsName string) {
	e.lock.Lock()
	defer e.lock.Unlock()

	dnsEntry, exists := e.dnsEntries[dnsName]
	if !exists {
		return
	}
	if pending := dnsEntry.pendingACLAttachments[namespace]; pending > 1 {
		dnsEntry.pendingACLAttachments[namespace] = pending - 1
	} else {
		delete(dnsEntry.pendingACLAttachments, namespace)
	}
}

// delete removes namespace ownership and returns entries whose DNS tracker
// cleanup must happen after releasing e.lock.
func (e *EgressDNS) delete(namespace string) (map[string]*dnsEntry, error) {
	e.lock.Lock()
	defer e.lock.Unlock()

	dnsEntriesToDelete := make(map[string]*dnsEntry)

	// go through all dnsNames for namespaces
	for dnsName, dnsEntry := range e.dnsEntries {
		// delete the dnsEntry
		delete(dnsEntry.namespaces, namespace)
		if len(dnsEntry.namespaces) == 0 {
			// the dnsEntry appears in no other namespace, so delete the address_set
			err := dnsEntry.dnsAddressSet.Destroy()
			if err != nil {
				return dnsEntriesToDelete, fmt.Errorf("error deleting EgressFirewall AddressSet for dnsName: %s %w", dnsName, err)
			}
			// the dnsEntry is no longer needed because nothing references it, so delete it
			delete(e.dnsEntries, dnsName)
			if e.dns != nil {
				e.markDNSDeletePendingLocked(dnsName, dnsEntry)
				dnsEntriesToDelete[dnsName] = dnsEntry
			}
		}
	}
	return dnsEntriesToDelete, nil
}

// Delete removes a namespace from DNS tracking and destroys address sets that
// are no longer used by any namespace.
func (e *EgressDNS) Delete(namespace string) error {
	dnsEntriesToDelete, err := e.delete(namespace)
	for name, entry := range dnsEntriesToDelete {
		go e.deleteFromDNS(name, entry)
	}
	return err
}

// markDNSDeletePendingLocked prevents Add from creating a replacement entry
// until this entry's asynchronous cleanup has removed its DNS tracker value.
// The caller must hold e.lock.
func (e *EgressDNS) markDNSDeletePendingLocked(dnsName string, entry *dnsEntry) {
	if e.pendingDNSDeletes == nil {
		e.pendingDNSDeletes = make(map[string]*dnsEntry)
	}
	entry.dnsDeleteDone = make(chan struct{})
	e.pendingDNSDeletes[dnsName] = entry
}

// Update refreshes the cached DNS answer for dnsName and reports whether it changed.
func (e *EgressDNS) Update(dnsName string) (bool, error) {
	return e.dns.Update(dnsName)
}

// updateEntryForName refreshes the address set for a DNS entry using its latest resolved IPs.
func (e *EgressDNS) updateEntryForName(dnsName string) error {
	return e.updateEntryForNameIfCurrent(dnsName, nil)
}

// updateEntryForNameIfCurrent refreshes dnsName only if expectedEntry remains
// the active resolver entry. An asynchronous Add for an entry removed while
// its DNS lookup was in flight must not update a replacement entry.
func (e *EgressDNS) updateEntryForNameIfCurrent(dnsName string, expectedEntry *dnsEntry) error {
	e.lock.Lock()
	defer e.lock.Unlock()
	dnsEntry, ok := e.dnsEntries[dnsName]
	if expectedEntry != nil && (!ok || dnsEntry != expectedEntry) {
		return nil
	}
	if !ok {
		return fmt.Errorf("cannot update DNS record for %s: no entry found. "+
			"Was the EgressFirewall deleted?", dnsName)
	}
	ips := e.dns.GetIPs(dnsName)
	dnsEntry.dnsResolves = ips

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
	if err := e.updateAddressSet(dnsName, util.StringSlice(ipsNoClusterSubnet)); err != nil {
		return fmt.Errorf("cannot add IPs from EgressFirewall AddressSet %s: %w", dnsName, err)
	}
	return nil
}

// updateAddressSet updates the cached OVN address set and repairs it if OVN removed it.
func (e *EgressDNS) updateAddressSet(dnsName string, addresses []string) error {
	dnsEntry := e.dnsEntries[dnsName]
	if err := dnsEntry.dnsAddressSet.SetAddresses(addresses); err == nil {
		return nil
	} else if !errors.Is(err, libovsdbclient.ErrNotFound) {
		return fmt.Errorf("failed to update existing address set for DNS name %s: %w", dnsName, err)
	}

	asIndex := GetEgressFirewallDNSAddrSetDbIDs(dnsName, e.controllerName)
	dnsAddressSet, err := e.addressSetFactory.EnsureAddressSet(asIndex)
	if err != nil {
		return fmt.Errorf("failed to recreate address set for DNS name %s: %w", dnsName, err)
	}

	dnsEntry.dnsAddressSet = dnsAddressSet
	if err := dnsEntry.dnsAddressSet.SetAddresses(addresses); err != nil {
		return fmt.Errorf("failed to update recreated address set for DNS name %s: %w", dnsName, err)
	}
	return nil
}

// addToDNS takes the dnsName adds it to the underlying dns resolver and
// performs the first update. After completing that signals the
// thread performing periodic updates that a new DNS name has been added and
// so that it can updates GetNextQueryTime() if needed
func (e *EgressDNS) addToDNS(dnsName string, entry *dnsEntry) {
	entry.dnsOperationLock.Lock()
	defer entry.dnsOperationLock.Unlock()

	// An entry can be removed after Add schedules this goroutine but before it
	// starts. Do not reintroduce a DNS tracker value for an obsolete entry.
	e.lock.Lock()
	if e.dnsEntries[dnsName] != entry {
		e.lock.Unlock()
		return
	}
	e.lock.Unlock()

	if err := e.dns.Add(dnsName); err != nil {
		utilruntime.HandleError(err)
	}
	if err := e.updateEntryForNameIfCurrent(dnsName, entry); err != nil {
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

// deleteFromDNS serializes tracker removal with pending registration for the
// same entry, then wakes Add calls waiting to recreate the DNS name.
func (e *EgressDNS) deleteFromDNS(dnsName string, entry *dnsEntry) {
	entry.dnsOperationLock.Lock()
	e.dns.Delete(dnsName)
	entry.dnsOperationLock.Unlock()

	e.lock.Lock()
	if e.pendingDNSDeletes[dnsName] == entry {
		delete(e.pendingDNSDeletes, dnsName)
		close(entry.dnsDeleteDone)
	}
	e.lock.Unlock()

	// Recalculate the refresh schedule after this entry was deleted or
	// superseded by a new entry for the same name.
	e.deleted <- dnsName
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

// Shutdown stops the background DNS refresh loop.
func (e *EgressDNS) Shutdown() {
	close(e.stopChan)
}

// aclMatchAddressSetTokens returns the identifiers in an OVN ACL match.
func aclMatchAddressSetTokens(match string) []string {
	return strings.FieldsFunc(match, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// aclMatchReferencesAddressSet reports whether an ACL match contains the
// complete OVN address-set identifier as a token.
func aclMatchReferencesAddressSet(match, addressSetName string) bool {
	for _, token := range aclMatchAddressSetTokens(match) {
		if token == addressSetName {
			return true
		}
	}
	return false
}

// DeleteStaleAddrSets deletes unreferenced EgressFirewall DNS address sets and
// forgets resolver entries that no longer have an ACL reference. It defers the
// whole cleanup pass while any Add-to-ACL handoff is in flight, because a new
// set is temporarily unreferenced between Add and the ACL transaction.
func (e *EgressDNS) DeleteStaleAddrSets(nbClient libovsdbclient.Client) error {
	e.lock.Lock()
	defer e.lock.Unlock()

	// Add returns the AddressSet before its caller commits the ACL that
	// references it. Defer this GC pass while that handoff is in flight so a
	// concurrent reconcile cannot delete the new AddressSet and discard the
	// resolver entry before the ACL transaction completes.
	for _, dnsEntry := range e.dnsEntries {
		for _, pending := range dnsEntry.pendingACLAttachments {
			if pending > 0 {
				return nil
			}
		}
	}

	predicateIDs := libovsdbops.NewDbObjectIDs(libovsdbops.AddressSetEgressFirewallDNS, e.controllerName, nil)
	if err := libovsdbutil.DeleteAddrSetsWithoutACLRef(predicateIDs, nbClient); err != nil {
		return fmt.Errorf("failed to delete stale EgressFirewall DNS address sets: %w", err)
	}

	// Keep entries whose address-set hashes are still referenced by an ACL,
	// even when their database rows are missing so the refresh path can repair
	// them. Drop unreferenced entries so a removed policy cannot be resurrected
	// by a later DNS refresh.
	addressSetNameToDNSName := make(map[string]string, len(e.dnsEntries)*2)
	for dnsName, dnsEntry := range e.dnsEntries {
		v4HashName, v6HashName := dnsEntry.dnsAddressSet.GetASHashNames()
		if v4HashName != "" {
			addressSetNameToDNSName[v4HashName] = dnsName
		}
		if v6HashName != "" {
			addressSetNameToDNSName[v6HashName] = dnsName
		}
	}
	referencedDNSNames := make(map[string]struct{})
	aclPredicateIDs := libovsdbops.NewDbObjectIDs(libovsdbops.ACLEgressFirewall, e.controllerName, nil)
	acls, err := libovsdbops.FindACLsWithPredicate(nbClient,
		libovsdbops.GetPredicate[*nbdb.ACL](aclPredicateIDs, nil))
	if err != nil {
		return fmt.Errorf("failed to find EgressFirewall ACLs referencing DNS address sets: %w", err)
	}
	for _, acl := range acls {
		for _, token := range aclMatchAddressSetTokens(acl.Match) {
			if dnsName, ok := addressSetNameToDNSName[token]; ok {
				referencedDNSNames[dnsName] = struct{}{}
			}
		}
	}

	for dnsName, dnsEntry := range e.dnsEntries {
		if _, ok := referencedDNSNames[dnsName]; ok {
			continue
		}
		delete(e.dnsEntries, dnsName)
		if e.dns != nil {
			e.markDNSDeletePendingLocked(dnsName, dnsEntry)
			go e.deleteFromDNS(dnsName, dnsEntry)
		}
	}
	return nil
}
