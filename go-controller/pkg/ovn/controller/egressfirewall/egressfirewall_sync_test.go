// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package egressfirewall

import (
	"context"
	"sync"
	"testing"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	"github.com/stretchr/testify/require"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	ovncnitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	egressfirewallapi "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1"
	egressfirewalllisters "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/listers/egressfirewall/v1"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	fakenetworkmanager "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	addressset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/syncmap"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
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

// TestEntriesEqual_DetectsSamplingChange verifies that a change in the resolved sampling
// collectors alone is enough to make entriesEqual report inequality, so sync re-applies
// Sample.Collectors after an ObservabilityConfig change without invalidating the entry.
func TestEntriesEqual_DetectsSamplingChange(t *testing.T) {
	base := &cacheEntry{pgName: "pg", efResourceVersion: "1"}
	require.True(t, entriesEqual(base, &cacheEntry{pgName: "pg", efResourceVersion: "1"}))

	withCollectors := &cacheEntry{pgName: "pg", efResourceVersion: "1", sampledCollectors: []string{"1"}}
	require.False(t, entriesEqual(base, withCollectors), "adding collectors must be detected")
	require.True(t, entriesEqual(withCollectors, &cacheEntry{pgName: "pg", efResourceVersion: "1", sampledCollectors: []string{"1"}}))
	require.False(t, entriesEqual(withCollectors, &cacheEntry{pgName: "pg", efResourceVersion: "1", sampledCollectors: []string{"2"}}),
		"a different collector set must be detected")
}

// TestEFControllerResyncSampling_PreservesCacheEntryForDeletePath guards the regression where
// ResyncSampling deleted the namespace cache entry to force a re-apply. If the EgressFirewall was
// then deleted before the queued key was processed (the workqueue coalesces both into one key),
// sync saw existingEntry == nil and newEntry == nil, short-circuited on entriesEqual, and left the
// ACLs on the port group. The entry must survive a sampling resync so the delete path can clean up.
func TestEFControllerResyncSampling_PreservesCacheEntryForDeletePath(t *testing.T) {
	require.NoError(t, config.PrepareTestConfig())
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true

	const (
		namespace = "namespace1"
		udnName   = "udn-test"
		zone      = "node1"
	)

	netInfo := mustNetInfo(t, udnName, "10.128.0.0/14")
	networkManager := &fakenetworkmanager.FakeNetworkManager{
		PrimaryNetworks: map[string]util.NetInfo{namespace: netInfo},
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
					To:   egressfirewallapi.EgressFirewallDestination{CIDRSelector: "10.128.1.0/24"},
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
	// ResyncSampling re-enqueues through the controller; a queue-only controller (never started)
	// is enough here since sync is driven manually.
	oc.controller = controller.NewController[egressfirewallapi.EgressFirewall](
		"test", &controller.ControllerConfig[egressfirewallapi.EgressFirewall]{})

	key := namespace + "/" + egressFirewallName
	oc.ruleCounter.Store(key, uint32(len(ef.Spec.Egress)))

	// First sync creates the ACLs and stores the cache entry.
	require.NoError(t, oc.sync(key))
	p := libovsdbops.GetPredicate[*nbdb.ACL](oc.GetEgressFirewallACLDbIDs(namespace, 0), nil)
	acls, err := libovsdbops.FindACLsWithPredicate(oc.nbClient, p)
	require.NoError(t, err)
	require.Len(t, acls, 1)

	// A sampling resync must NOT invalidate the cache entry: the delete path depends on it.
	require.NoError(t, oc.ResyncSampling(libovsdbops.EgressFirewallSample, sets.New(namespace)))
	_, ok := oc.cache.Load(namespace)
	require.True(t, ok, "ResyncSampling must not delete the cache entry")

	// Coalesced-events race: the EgressFirewall is deleted before the resync key is processed. With
	// the entry preserved, sync still has existingEntry.pgName to clean up the ACLs.
	require.NoError(t, efIndexer.Delete(ef))
	require.NoError(t, oc.sync(key))

	acls, err = libovsdbops.FindACLsWithPredicate(oc.nbClient, p)
	require.NoError(t, err)
	require.Empty(t, acls, "EgressFirewall ACLs must be removed after the EF is deleted")

	_, ok = oc.cache.Load(namespace)
	require.False(t, ok, "cache entry must be removed after the EF is deleted")
}
