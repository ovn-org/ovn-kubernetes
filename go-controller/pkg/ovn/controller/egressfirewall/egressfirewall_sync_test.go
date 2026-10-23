// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package egressfirewall

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	"github.com/miekg/dns"
	mock "github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	ovncnitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	egressfirewallapi "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1"
	egressfirewalllisters "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/listers/egressfirewall/v1"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	fakenetworkmanager "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	addressset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	dnsnameresolver "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/dns_name_resolver"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/syncmap"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	util_mocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/mocks"
)

type noopDNSNameResolver struct{}

func (noopDNSNameResolver) Add(string, string) (addressset.AddressSet, error) { return nil, nil }
func (noopDNSNameResolver) Delete(string) error                               { return nil }
func (noopDNSNameResolver) Run() error                                        { return nil }
func (noopDNSNameResolver) Shutdown()                                         {}
func (noopDNSNameResolver) DeleteStaleAddrSets(libovsdbclient.Client) error   { return nil }

type panicTransactClient struct {
	libovsdbclient.Client
}

func (p *panicTransactClient) Transact(context.Context, ...ovsdb.Operation) ([]ovsdb.OperationResult, error) {
	panic("unexpected Transact call")
}

func mustNetInfo(t *testing.T, name, subnets string) util.NetInfo {
	t.Helper()
	ni, err := util.NewNetInfo(&ovncnitypes.NetConf{
		NetConf:  cnitypes.NetConf{Name: name},
		Topology: types.Layer3Topology,
		Subnets:  subnets,
		Role:     types.NetworkRolePrimary,
		MTU:      1400,
	})
	require.NoError(t, err)
	return ni
}

func TestEFControllerSync_UpdatesOnSubnetChangeAndSkipsWhenUnchanged(t *testing.T) {
	require.NoError(t, config.PrepareTestConfig())
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true

	const (
		namespace = "namespace1"
		udnName   = "udn-test"
		zone      = "node1"
	)

	netInfo1 := mustNetInfo(t, udnName, "10.128.0.0/14")
	// Keep the subnet intersecting the destination CIDR, but change it so we can verify the ACL match
	// is updated (rather than just removing the exclusion entirely).
	netInfo2 := mustNetInfo(t, udnName, "10.128.0.0/15")

	networkManager := &fakenetworkmanager.FakeNetworkManager{
		PrimaryNetworks: map[string]util.NetInfo{
			namespace: netInfo1,
		},
	}

	ownerController := udnName + "-network-controller"
	pgName := libovsdbutil.GetPortGroupName(getNamespacePortGroupDbIDs(namespace, ownerController))

	initialDB := libovsdbtest.TestSetup{
		NBData: []libovsdbtest.TestData{
			&nbdb.PortGroup{
				Name: pgName,
				ExternalIDs: map[string]string{
					libovsdbops.OwnerTypeKey.String():       libovsdbops.NamespaceOwnerType,
					libovsdbops.OwnerControllerKey.String(): ownerController,
					libovsdbops.ObjectNameKey.String():      namespace,
				},
			},
		},
	}
	nbClient, _, cleanup, err := libovsdbtest.NewNBSBTestHarness(initialDB)
	require.NoError(t, err)
	t.Cleanup(cleanup.Cleanup)

	nsIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	require.NoError(t, nsIndexer.Add(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}))
	namespaceLister := corelisters.NewNamespaceLister(nsIndexer)

	efIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	ef := &egressfirewallapi.EgressFirewall{
		ObjectMeta: metav1.ObjectMeta{
			Name:            egressFirewallName,
			Namespace:       namespace,
			ResourceVersion: "1",
		},
		Spec: egressfirewallapi.EgressFirewallSpec{
			Egress: []egressfirewallapi.EgressFirewallRule{
				{
					Type: egressfirewallapi.EgressFirewallRuleAllow,
					To: egressfirewallapi.EgressFirewallDestination{
						CIDRSelector: "10.128.1.0/24",
					},
				},
			},
		},
		Status: egressfirewallapi.EgressFirewallStatus{
			Messages: []string{types.GetZoneStatus(zone, EgressFirewallAppliedCorrectly)},
		},
	}
	require.NoError(t, efIndexer.Add(ef))
	efLister := egressfirewalllisters.NewEgressFirewallLister(efIndexer)

	oc := &EFController{
		name:            "test",
		zone:            zone,
		cache:           syncmap.NewSyncMap[*cacheEntry](),
		nbClient:        nbClient,
		kube:            nil, // status updates are no-op in this test due to pre-seeded status message
		namespaceLister: namespaceLister,
		efLister:        efLister,
		networkManager:  networkManager,
		ruleCounter:     sync.Map{},
		dnsNameResolver: noopDNSNameResolver{},
	}

	// Pre-seed rule counter so status updates don't affect global metrics.
	oc.ruleCounter.Store(namespace+"/"+egressFirewallName, uint32(len(ef.Spec.Egress)))

	// First sync creates ACLs and stores cache entry.
	err = oc.sync(namespace + "/" + egressFirewallName)
	require.NoError(t, err)

	p := libovsdbops.GetPredicate[*nbdb.ACL](oc.GetEgressFirewallACLDbIDs(namespace, 0), nil)
	acls, err := libovsdbops.FindACLsWithPredicate(oc.nbClient, p)
	require.NoError(t, err)
	require.Len(t, acls, 1)
	require.Contains(t, acls[0].Match, "ip4.dst != 10.128.0.0/14")

	// Update netInfo subnets (same network name => same PG name), then sync again.
	networkManager.Lock()
	networkManager.PrimaryNetworks[namespace] = netInfo2
	networkManager.Unlock()

	err = oc.sync(namespace + "/" + egressFirewallName)
	require.NoError(t, err)

	acls, err = libovsdbops.FindACLsWithPredicate(oc.nbClient, p)
	require.NoError(t, err)
	require.Len(t, acls, 1)
	require.NotContains(t, acls[0].Match, "ip4.dst != 10.128.0.0/14")
	require.Contains(t, acls[0].Match, "ip4.dst != 10.128.0.0/15")

	// Now that netInfo, EF, and PG are stable, ensure we skip OVN updates.
	oc.nbClient = &panicTransactClient{Client: nbClient}
	require.NotPanics(t, func() {
		err = oc.sync(namespace + "/" + egressFirewallName)
	})
	require.NoError(t, err)

	// No further changes; ensure match is still the updated one.
	acls, err = libovsdbops.FindACLsWithPredicate(nbClient, p)
	require.NoError(t, err)
	require.Len(t, acls, 1)
	require.NotContains(t, acls[0].Match, "ip4.dst != 10.128.0.0/14")
	require.Contains(t, acls[0].Match, "ip4.dst != 10.128.0.0/15")

	// Sanity: the controller cache is present and matches current pg/subnets.
	entry, ok := oc.cache.Load(namespace)
	require.True(t, ok)
	require.Equal(t, pgName, entry.pgName)
	require.True(t, util.IsIPNetsEqual(subnetsForNetInfo(netInfo2), entry.subnets))
}

func TestEFControllerSync_AddsCIDRExclusionWhenPrimaryNetworkAddsOverlappingSubnet(t *testing.T) {
	require.NoError(t, config.PrepareTestConfig())
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true

	const (
		namespace = "namespace1"
		udnName   = "udn-test"
		zone      = "node1"
	)

	netInfoBefore := mustNetInfo(t, udnName, "10.128.0.0/16")
	netInfoAfter := mustNetInfo(t, udnName, "10.128.0.0/16,10.129.0.0/16")

	networkManager := &fakenetworkmanager.FakeNetworkManager{
		PrimaryNetworks: map[string]util.NetInfo{
			namespace: netInfoBefore,
		},
	}

	ownerController := udnName + "-network-controller"
	pgName := libovsdbutil.GetPortGroupName(getNamespacePortGroupDbIDs(namespace, ownerController))

	initialDB := libovsdbtest.TestSetup{
		NBData: []libovsdbtest.TestData{
			&nbdb.PortGroup{
				Name: pgName,
				ExternalIDs: map[string]string{
					libovsdbops.OwnerTypeKey.String():       libovsdbops.NamespaceOwnerType,
					libovsdbops.OwnerControllerKey.String(): ownerController,
					libovsdbops.ObjectNameKey.String():      namespace,
				},
			},
		},
	}
	nbClient, _, cleanup, err := libovsdbtest.NewNBSBTestHarness(initialDB)
	require.NoError(t, err)
	t.Cleanup(cleanup.Cleanup)

	nsIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	require.NoError(t, nsIndexer.Add(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}))
	namespaceLister := corelisters.NewNamespaceLister(nsIndexer)

	efIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	ef := &egressfirewallapi.EgressFirewall{
		ObjectMeta: metav1.ObjectMeta{
			Name:            egressFirewallName,
			Namespace:       namespace,
			ResourceVersion: "1",
		},
		Spec: egressfirewallapi.EgressFirewallSpec{
			Egress: []egressfirewallapi.EgressFirewallRule{
				{
					Type: egressfirewallapi.EgressFirewallRuleAllow,
					To: egressfirewallapi.EgressFirewallDestination{
						CIDRSelector: "10.129.1.0/24",
					},
				},
			},
		},
		Status: egressfirewallapi.EgressFirewallStatus{
			Messages: []string{types.GetZoneStatus(zone, EgressFirewallAppliedCorrectly)},
		},
	}
	require.NoError(t, efIndexer.Add(ef))
	efLister := egressfirewalllisters.NewEgressFirewallLister(efIndexer)

	oc := &EFController{
		name:            "test",
		zone:            zone,
		cache:           syncmap.NewSyncMap[*cacheEntry](),
		nbClient:        nbClient,
		kube:            nil, // status updates are no-op in this test due to pre-seeded status message
		namespaceLister: namespaceLister,
		efLister:        efLister,
		networkManager:  networkManager,
		ruleCounter:     sync.Map{},
		dnsNameResolver: noopDNSNameResolver{},
	}

	// Pre-seed rule counter so status updates don't affect global metrics.
	oc.ruleCounter.Store(namespace+"/"+egressFirewallName, uint32(len(ef.Spec.Egress)))

	err = oc.sync(namespace + "/" + egressFirewallName)
	require.NoError(t, err)

	p := libovsdbops.GetPredicate[*nbdb.ACL](oc.GetEgressFirewallACLDbIDs(namespace, 0), nil)
	acls, err := libovsdbops.FindACLsWithPredicate(oc.nbClient, p)
	require.NoError(t, err)
	require.Len(t, acls, 1)
	require.Contains(t, acls[0].Match, "ip4.dst == 10.129.1.0/24")
	require.NotContains(t, acls[0].Match, "ip4.dst != 10.129.0.0/16")

	networkManager.Lock()
	networkManager.PrimaryNetworks[namespace] = netInfoAfter
	networkManager.Unlock()

	err = oc.sync(namespace + "/" + egressFirewallName)
	require.NoError(t, err)

	acls, err = libovsdbops.FindACLsWithPredicate(oc.nbClient, p)
	require.NoError(t, err)
	require.Len(t, acls, 1)
	require.Contains(t, acls[0].Match, "ip4.dst == 10.129.1.0/24")
	require.Contains(t, acls[0].Match, "ip4.dst != 10.129.0.0/16")

	entry, ok := oc.cache.Load(namespace)
	require.True(t, ok)
	require.Equal(t, pgName, entry.pgName)
	require.True(t, util.IsIPNetsEqual(subnetsForNetInfo(netInfoAfter), entry.subnets))
}

type failNextTransactClient struct {
	libovsdbclient.Client
	failNext  bool
	failure   error
	callCount int
}

// Transact injects one transaction failure, then delegates subsequent calls.
func (c *failNextTransactClient) Transact(ctx context.Context, ops ...ovsdb.Operation) ([]ovsdb.OperationResult, error) {
	c.callCount++
	if c.failNext {
		c.failNext = false
		return nil, c.failure
	}
	return c.Client.Transact(ctx, ops...)
}

// observedAddressSet reports completed address updates to its test.
type observedAddressSet struct {
	addressset.AddressSet
	updates chan<- error
}

// SetAddresses forwards an update and reports its result to the test.
func (as *observedAddressSet) SetAddresses(addresses []string) error {
	err := as.AddressSet.SetAddresses(addresses)
	as.updates <- err
	return err
}

// observedAddressSetFactory wraps new sets so asynchronous updates are observable.
type observedAddressSetFactory struct {
	addressset.AddressSetFactory
	updates chan<- error
}

// NewAddressSet wraps each new address set with the update observer.
func (f *observedAddressSetFactory) NewAddressSet(dbIDs *libovsdbops.DbObjectIDs, addresses []string) (addressset.AddressSet, error) {
	as, err := f.AddressSetFactory.NewAddressSet(dbIDs, addresses)
	if err != nil {
		return nil, err
	}
	return &observedAddressSet{AddressSet: as, updates: f.updates}, nil
}

// dnsAddCompletion records the handoff completed by the EgressFirewall controller.
type dnsAddCompletion struct {
	namespace string
	dnsName   string
}

// trackingDNSNameResolver records Add completions while delegating resolver behavior.
type trackingDNSNameResolver struct {
	dnsnameresolver.DNSNameResolver
	completer   dnsnameresolver.DNSNameResolverAddCompleter
	completions []dnsAddCompletion
}

// CompleteAdd records the completed handoff before releasing it in the resolver.
func (r *trackingDNSNameResolver) CompleteAdd(namespace, dnsName string) {
	r.completions = append(r.completions, dnsAddCompletion{namespace: namespace, dnsName: dnsName})
	r.completer.CompleteAdd(namespace, dnsName)
}

// waitForAddressSetUpdate waits for one asynchronous resolver update result.
func waitForAddressSetUpdate(t *testing.T, updates <-chan error) {
	t.Helper()
	select {
	case err := <-updates:
		require.NoError(t, err, "the asynchronous DNS refresh should update its address set")
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the asynchronous DNS address-set update")
	}
}

// TestEFControllerRecoversAfterFailedBatchCommit exercises the real
// addEgressFirewallRules defer and verifies that a DNS address set left
// unreferenced by a failed ACL transaction is forgotten by GC and recreated on
// the next reconcile.
func TestEFControllerRecoversAfterFailedBatchCommit(t *testing.T) {
	require.NoError(t, config.PrepareTestConfig(), "failed to prepare test configuration")
	config.IPv4Mode = true
	config.IPv6Mode = false
	t.Cleanup(func() {
		if err := config.PrepareTestConfig(); err != nil {
			t.Errorf("failed to restore test configuration: %v", err)
		}
	})

	previousDNSOps := util.GetDNSLibOps()
	mockDNSOps := new(util_mocks.DNSOps)
	util.SetDNSLibOpsMockInst(mockDNSOps)
	mockDNSOps.On("ClientConfigFromFile", mock.AnythingOfType("string")).
		Return(&dns.ClientConfig{Servers: []string{"192.0.2.53"}, Port: "53"}, nil).Once()
	mockDNSOps.On("Fqdn", "failed-batch.test.com.").Return("failed-batch.test.com.").Twice()
	mockDNSOps.On("SetQuestion", mock.AnythingOfType("*dns.Msg"), "failed-batch.test.com.", uint16(dns.TypeA)).
		Return(&dns.Msg{}).Twice()
	dnsAnswer := &dns.Msg{Answer: []dns.RR{dnsTestAnswer()}}
	mockDNSOps.On("Exchange", mock.AnythingOfType("*dns.Client"), mock.AnythingOfType("*dns.Msg"), "192.0.2.53:53").
		Return(dnsAnswer, time.Second, nil).Twice()
	t.Cleanup(func() { util.SetDNSLibOpsMockInst(previousDNSOps) })

	const (
		namespace = "failed-batch-namespace"
		dnsName   = "failed-batch.test.com."
	)

	pgName := libovsdbutil.GetPortGroupName(getNamespacePortGroupDbIDs(namespace, types.DefaultNetworkControllerName))
	nbClient, testCtx, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{
		NBData: []libovsdbtest.TestData{&nbdb.PortGroup{Name: pgName}},
	}, nil)
	require.NoError(t, err, "failed to create the NBDB test harness")
	t.Cleanup(testCtx.Cleanup)

	updates := make(chan error, 2)
	factory := &observedAddressSetFactory{
		AddressSetFactory: addressset.NewOvnAddressSetFactory(nbClient, true, false),
		updates:           updates,
	}
	egressDNS, err := dnsnameresolver.NewEgressDNS(factory, types.DefaultNetworkControllerName, make(chan struct{}), 0)
	require.NoError(t, err, "failed to create EgressDNS for the controller test")
	trackingResolver := &trackingDNSNameResolver{
		DNSNameResolver: egressDNS,
		completer:       egressDNS,
	}

	injectedErr := errors.New("injected NBDB transaction failure")
	wrappedClient := &failNextTransactClient{
		Client:   nbClient,
		failNext: true,
		failure:  injectedErr,
	}
	oc := &EFController{
		nbClient:        wrappedClient,
		dnsNameResolver: trackingResolver,
	}

	ef := &egressFirewall{
		name:      egressFirewallName,
		namespace: namespace,
		egressRules: []*egressFirewallRule{
			{
				id:     0,
				access: egressfirewallapi.EgressFirewallRuleAllow,
				to:     destination{dnsName: dnsName},
			},
			{
				id:     1,
				access: egressfirewallapi.EgressFirewallRuleAllow,
				// This empty destination causes the controller to check for an
				// old ACL. Since none exists, the injected error reaches the final
				// batched ACL transaction for rule 0.
				to: destination{},
			},
		},
	}

	err = oc.addEgressFirewallRules(ef, pgName, nil)
	require.ErrorContains(t, err, injectedErr.Error())
	require.Equal(t, 1, wrappedClient.callCount, "the failure should hit the final batched ACL transaction")
	require.Equal(t, []dnsAddCompletion{{namespace: namespace, dnsName: dnsName}}, trackingResolver.completions,
		"the deferred completion should fire once with the DNS name added before the failed commit")
	waitForAddressSetUpdate(t, updates)

	asIDs := dnsnameresolver.GetEgressFirewallDNSAddrSetDbIDs(dnsName, types.DefaultNetworkControllerName)
	asPredicate := libovsdbops.GetPredicate[*nbdb.AddressSet](asIDs, nil)
	firstAddressSets, err := libovsdbops.FindAddressSetsWithPredicate(nbClient, asPredicate)
	require.NoError(t, err, "failed to find the orphaned AddressSet after the failed ACL batch")
	require.Len(t, firstAddressSets, 1, "the DNS Add should have committed its address set before the ACL batch")
	firstUUID := firstAddressSets[0].UUID

	aclPredicate := libovsdbops.GetPredicate[*nbdb.ACL](oc.GetEgressFirewallACLDbIDs(namespace, 0), nil)
	acls, err := libovsdbops.FindACLsWithPredicate(nbClient, aclPredicate)
	require.NoError(t, err, "failed to confirm the failed batch created no ACL")
	require.Empty(t, acls, "the failed batch must leave the DNS address set without an ACL reference")

	// This is the real GC entry point used by sync after a successful reconcile.
	// Deleting the row proves the deferred CompleteAdd released the pending
	// attachment; the next Add below proves GC also forgot its resolver entry.
	require.NoError(t, oc.dnsNameResolver.DeleteStaleAddrSets(oc.nbClient), "failed to run stale DNS address-set cleanup")
	addressSetsAfterGC, err := libovsdbops.FindAddressSetsWithPredicate(nbClient, asPredicate)
	require.NoError(t, err, "failed to verify the orphaned AddressSet was removed")
	require.Empty(t, addressSetsAfterGC, "GC should delete the unreferenced address set")

	// The one-shot transaction error has been consumed, so this call models the
	// next reconcile succeeding after the original orphan was cleaned up.
	err = oc.addEgressFirewallRules(ef, pgName, nil)
	require.NoError(t, err, "failed to reconcile the EgressFirewall after orphan cleanup")
	waitForAddressSetUpdate(t, updates)

	newAddressSets, err := libovsdbops.FindAddressSetsWithPredicate(nbClient, asPredicate)
	require.NoError(t, err, "failed to find the recreated AddressSet")
	require.Len(t, newAddressSets, 1, "retry should create one fresh address set")
	require.NotEqual(t, firstUUID, newAddressSets[0].UUID, "retry must not reuse the deleted address-set row")

	acls, err = libovsdbops.FindACLsWithPredicate(nbClient, aclPredicate)
	require.NoError(t, err, "failed to find the ACL created by the successful retry")
	require.Len(t, acls, 1, "successful retry should create the dnsName ACL")
	require.Contains(t, acls[0].Match, "$"+newAddressSets[0].Name,
		"the retried ACL should reference the newly created address set")
	mockDNSOps.AssertExpectations(t)
}

// dnsTestAnswer returns the stable DNS response used by the controller test.
func dnsTestAnswer() dns.RR {
	return &dns.A{
		Hdr: dns.RR_Header{
			Name:   "failed-batch.test.com.",
			Rrtype: dns.TypeA,
			Class:  dns.ClassINET,
			Ttl:    30,
		},
		A: net.IPv4(192, 0, 2, 10).To4(),
	}
}
