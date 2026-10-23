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
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	ovncnitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	egressfirewallapi "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1"
	egressfirewallfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/clientset/versioned/fake"
	egressfirewalllisters "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/listers/egressfirewall/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
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

	// Deleting a primary NAD transiently leaves its namespace with the required
	// UDN label but no active primary network. This must clean up EF state without
	// returning an error to the infinite-retry EF controller. Once the primary
	// network is active again, the NAD reconciler requeues and restores the EF.
	networkManager.Lock()
	networkManager.PrimaryNetworks[namespace] = nil
	networkManager.Unlock()
	require.NoError(t, oc.sync(namespace+"/"+egressFirewallName))

	acls, err = libovsdbops.FindACLsWithPredicate(oc.nbClient, p)
	require.NoError(t, err)
	require.Empty(t, acls)
	_, ok := oc.cache.Load(namespace)
	require.False(t, ok)

	networkManager.Lock()
	networkManager.PrimaryNetworks[namespace] = netInfo2
	networkManager.Unlock()
	require.NoError(t, oc.sync(namespace+"/"+egressFirewallName))

	acls, err = libovsdbops.FindACLsWithPredicate(oc.nbClient, p)
	require.NoError(t, err)
	require.Len(t, acls, 1)
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

// TestEFControllerSync_InvalidRuleUpdateKeepsExistingACLs covers the sync() error path: when an
// update introduces an invalid rule, addEgressFirewall must fail before any stale-ACL cleanup runs,
// so the ACLs (and cache entry) from the last successfully applied EgressFirewall are left in place.
func TestEFControllerSync_InvalidRuleUpdateKeepsExistingACLs(t *testing.T) {
	require.NoError(t, config.PrepareTestConfig())

	const (
		namespace = "namespace1"
		zone      = "global"
	)

	// No UDN involved: this test only cares about sync()'s validation-failure path, which is
	// independent of which network backs the namespace, so the default network is enough.
	networkManager := &fakenetworkmanager.FakeNetworkManager{}

	ownerController := types.DefaultNetworkName + "-network-controller"
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
		kube:            &kube.KubeOVN{EgressFirewallClient: egressfirewallfake.NewSimpleClientset()},
		namespaceLister: namespaceLister,
		efLister:        efLister,
		networkManager:  networkManager,
		ruleCounter:     sync.Map{},
		dnsNameResolver: noopDNSNameResolver{},
	}

	// Pre-seed rule counter so status updates don't affect global metrics.
	oc.ruleCounter.Store(namespace+"/"+egressFirewallName, uint32(len(ef.Spec.Egress)))

	// First sync creates the ACL for the valid rule.
	require.NoError(t, oc.sync(namespace+"/"+egressFirewallName))

	p := libovsdbops.GetPredicate[*nbdb.ACL](oc.GetEgressFirewallACLDbIDs(namespace, 0), nil)
	acls, err := libovsdbops.FindACLsWithPredicate(oc.nbClient, p)
	require.NoError(t, err)
	require.Len(t, acls, 1)
	originalMatch := acls[0].Match

	entry, ok := oc.cache.Load(namespace)
	require.True(t, ok)
	require.Equal(t, "1", entry.efResourceVersion)

	// Update to an invalid rule: a CIDR selector without a mask fails validation in addEgressFirewall.
	// Mutate a copy rather than the object already stored in the indexer.
	invalidEF := ef.DeepCopy()
	invalidEF.Spec.Egress[0].To.CIDRSelector = "10.128.1.0"
	invalidEF.ResourceVersion = "2"
	require.NoError(t, efIndexer.Update(invalidEF))

	// The second sync must fail validation before attempting any OVSDB write at all (and therefore
	// before stale-ACL cleanup runs). Swap in a Transact-panicking client to prove that, rather than
	// only inferring it from the ACL/cache state afterward.
	oc.nbClient = &panicTransactClient{Client: nbClient}
	require.NotPanics(t, func() {
		err = oc.sync(namespace + "/" + egressFirewallName)
	})
	require.Error(t, err)

	acls, err = libovsdbops.FindACLsWithPredicate(nbClient, p)
	require.NoError(t, err)
	require.Len(t, acls, 1)
	require.Equal(t, originalMatch, acls[0].Match)

	entry, ok = oc.cache.Load(namespace)
	require.True(t, ok)
	require.Equal(t, "1", entry.efResourceVersion, "cache must still reflect the last successfully applied EgressFirewall")

	// Same check for an unmasked IPv6 selector: validation is address-family agnostic, but dual-stack
	// inputs should be exercised explicitly rather than assumed to follow the IPv4 case.
	invalidEFv6 := ef.DeepCopy()
	invalidEFv6.Spec.Egress[0].To.CIDRSelector = "2001:db8::1"
	invalidEFv6.ResourceVersion = "3"
	require.NoError(t, efIndexer.Update(invalidEFv6))

	require.NotPanics(t, func() {
		err = oc.sync(namespace + "/" + egressFirewallName)
	})
	require.Error(t, err)

	acls, err = libovsdbops.FindACLsWithPredicate(nbClient, p)
	require.NoError(t, err)
	require.Len(t, acls, 1)
	require.Equal(t, originalMatch, acls[0].Match)

	entry, ok = oc.cache.Load(namespace)
	require.True(t, ok)
	require.Equal(t, "1", entry.efResourceVersion, "cache must still reflect the last successfully applied EgressFirewall")
}
