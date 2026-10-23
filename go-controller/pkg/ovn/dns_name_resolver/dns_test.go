// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package dnsnameresolver

import (
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/stretchr/testify/assert"
	mock "github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	utilnet "k8s.io/utils/net"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	mocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/address_set/mocks"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	util_mocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/mocks"
)

const DefaultNetworkControllerName = "default-network-controller"

func TestNewEgressDNS(t *testing.T) {
	testCh := make(chan struct{})
	dbSetup := libovsdbtest.TestSetup{}

	libovsdbOvnNBClient, _, libovsdbCleanup, err := libovsdbtest.NewNBSBTestHarness(dbSetup)
	require.NoError(t, err)
	t.Cleanup(libovsdbCleanup.Cleanup)

	testOvnAddFtry := addressset.NewOvnAddressSetFactory(libovsdbOvnNBClient, config.IPv4Mode, config.IPv6Mode)
	mockDnsOps := new(util_mocks.DNSOps)
	util.SetDNSLibOpsMockInst(mockDnsOps)
	tests := []struct {
		desc             string
		errExp           bool
		dnsOpsMockHelper []ovntest.TestifyMockHelper
	}{
		{
			desc:   "fails to read the /etc/resolv.conf file",
			errExp: true,
			dnsOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "ClientConfigFromFile", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{nil, fmt.Errorf("mock error")}, CallTimes: 1},
			},
		},
		{
			desc: "positive tests case",
			dnsOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "ClientConfigFromFile", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{&dns.ClientConfig{}, nil}, CallTimes: 1},
			},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			for _, item := range tc.dnsOpsMockHelper {
				call := mockDnsOps.On(item.OnCallMethodName)
				for _, arg := range item.OnCallMethodArgType {
					call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range item.RetArgList {
					call.ReturnArguments = append(call.ReturnArguments, ret)
				}
				call.Once()
			}
			_, err := NewEgressDNS(testOvnAddFtry, DefaultNetworkControllerName, testCh, 0)
			//t.Log(res, err)
			if tc.errExp {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			mockDnsOps.AssertExpectations(t)
		})
	}
}

func generateRR(dnsName, ip, nextQueryTime string) dns.RR {
	var rr dns.RR
	if utilnet.IsIPv6(net.ParseIP(ip)) {
		rr, _ = dns.NewRR(dnsName + ".        " + nextQueryTime + "     IN      AAAA       " + ip)
	} else {
		rr, _ = dns.NewRR(dnsName + ".        " + nextQueryTime + "     IN      A       " + ip)
	}
	return rr
}

// TestUpdateAddressSetRecreatesMissingAddressSet verifies replacement of a stale address set handle.
func TestUpdateAddressSetRecreatesMissingAddressSet(t *testing.T) {
	const dnsName = "www.test.com"
	addresses := []string{"192.0.2.10"}
	staleAddressSet := new(mocks.AddressSet)
	recreatedAddressSet := new(mocks.AddressSet)
	factory := new(mocks.AddressSetFactory)
	expectedDBIDs := GetEgressFirewallDNSAddrSetDbIDs(dnsName, DefaultNetworkControllerName)

	staleAddressSet.On("SetAddresses", addresses).
		Return(fmt.Errorf("address set was removed: %w", libovsdbclient.ErrNotFound)).Once()
	factory.On("EnsureAddressSet", mock.MatchedBy(func(dbIDs *libovsdbops.DbObjectIDs) bool {
		return dbIDs.String() == expectedDBIDs.String()
	})).Return(recreatedAddressSet, nil).Once()
	recreatedAddressSet.On("SetAddresses", addresses).Return(nil).Once()

	resolver := &EgressDNS{
		dnsEntries: map[string]*dnsEntry{
			dnsName: {
				dnsAddressSet: staleAddressSet,
			},
		},
		addressSetFactory: factory,
		controllerName:    DefaultNetworkControllerName,
	}

	require.NoError(t, resolver.updateAddressSet(dnsName, addresses),
		"failed to recreate address set for DNS name %s", dnsName)
	assert.Same(t, recreatedAddressSet, resolver.dnsEntries[dnsName].dnsAddressSet,
		"recreated address set was not stored for DNS name %s", dnsName)

	staleAddressSet.AssertExpectations(t)
	recreatedAddressSet.AssertExpectations(t)
	factory.AssertExpectations(t)
}

// TestUpdateAddressSetDoesNotRecreateOnUnexpectedError verifies that only a
// missing address set triggers recreation.
func TestUpdateAddressSetDoesNotRecreateOnUnexpectedError(t *testing.T) {
	const dnsName = "www.test.com"
	addresses := []string{"192.0.2.10"}
	staleAddressSet := new(mocks.AddressSet)
	factory := new(mocks.AddressSetFactory)

	staleAddressSet.On("SetAddresses", addresses).Return(fmt.Errorf("update failed")).Once()

	resolver := &EgressDNS{
		dnsEntries: map[string]*dnsEntry{
			dnsName: {
				dnsAddressSet: staleAddressSet,
			},
		},
		addressSetFactory: factory,
		controllerName:    DefaultNetworkControllerName,
	}

	require.Error(t, resolver.updateAddressSet(dnsName, addresses),
		"unexpected address-set update errors must be returned")
	factory.AssertNotCalled(t, "EnsureAddressSet", mock.Anything)
	staleAddressSet.AssertExpectations(t)
}

// TestACLMatchReferencesAddressSet verifies address-set references are matched
// as complete ACL tokens rather than arbitrary substrings.
func TestACLMatchReferencesAddressSet(t *testing.T) {
	tests := []struct {
		name          string
		match         string
		addressSet    string
		expectedMatch bool
	}{
		{
			name:          "exact reference",
			match:         "ip4.dst == $a123",
			addressSet:    "a123",
			expectedMatch: true,
		},
		{
			name:          "reference in set",
			match:         "ip4.dst == {$a123, $a456}",
			addressSet:    "a123",
			expectedMatch: true,
		},
		{
			name:          "hash prefix is not a reference",
			match:         "ip4.dst == $a1234",
			addressSet:    "a123",
			expectedMatch: false,
		},
		{
			name:          "hash embedded in another token is not a reference",
			match:         "ip4.dst == $xa123",
			addressSet:    "a123",
			expectedMatch: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expectedMatch, aclMatchReferencesAddressSet(tc.match, tc.addressSet),
				"ACL match %q must identify address set %q correctly", tc.match, tc.addressSet)
		})
	}
}

// TestUpdateEntryForNameRecreatesMissingAddressSet verifies the recovery path against a real
// libovsdb client for IPv4-only, IPv6-only, and dual-stack address sets.
func TestUpdateEntryForNameRecreatesMissingAddressSet(t *testing.T) {
	const dnsName = "www.test.com"
	tests := []struct {
		name              string
		ipv4Mode          bool
		ipv6Mode          bool
		ipv4Address       string
		ipv6Address       string
		deleteIPv6Address bool
	}{
		{
			name:        "IPv4-only address set",
			ipv4Mode:    true,
			ipv4Address: "192.0.2.10",
		},
		{
			name:              "IPv6-only address set",
			ipv6Mode:          true,
			ipv6Address:       "2001:db8::10",
			deleteIPv6Address: true,
		},
		{
			name:              "dual-stack address set with missing IPv6 row",
			ipv4Mode:          true,
			ipv6Mode:          true,
			ipv4Address:       "192.0.2.10",
			ipv6Address:       "2001:db8::10",
			deleteIPv6Address: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			oldDNSOps := util.GetDNSLibOps()
			require.NoError(t, config.PrepareTestConfig(), "failed to prepare test configuration for %s", tc.name)
			config.IPv4Mode = tc.ipv4Mode
			config.IPv6Mode = tc.ipv6Mode
			dnsServer := "192.0.2.53"
			dnsServerAddress := "192.0.2.53:53"
			if tc.ipv6Mode && !tc.ipv4Mode {
				dnsServer = "2001:db8::53"
				dnsServerAddress = "[2001:db8::53]:53"
			}
			t.Cleanup(func() {
				util.SetDNSLibOpsMockInst(oldDNSOps)
				if err := config.PrepareTestConfig(); err != nil {
					t.Errorf("failed to restore test config: %v", err)
				}
			})

			mockDnsOps := new(util_mocks.DNSOps)
			util.SetDNSLibOpsMockInst(mockDnsOps)
			mockDnsOps.On("ClientConfigFromFile", mock.AnythingOfType("string")).
				Return(&dns.ClientConfig{Servers: []string{dnsServer}, Port: "53"}, nil).Once()
			if tc.ipv4Address != "" {
				mockDnsOps.On("Fqdn", dnsName).Return(dnsName + ".").Once()
				mockDnsOps.On("SetQuestion", mock.AnythingOfType("*dns.Msg"), dnsName+".", uint16(dns.TypeA)).
					Return(&dns.Msg{}).Once()
				mockDnsOps.On("Exchange", mock.AnythingOfType("*dns.Client"), mock.AnythingOfType("*dns.Msg"), dnsServerAddress).
					Return(&dns.Msg{Answer: []dns.RR{generateRR(dnsName, tc.ipv4Address, "30")}}, time.Second, nil).Once()
			}
			if tc.ipv6Address != "" {
				mockDnsOps.On("Fqdn", dnsName).Return(dnsName + ".").Once()
				mockDnsOps.On("SetQuestion", mock.AnythingOfType("*dns.Msg"), dnsName+".", uint16(dns.TypeAAAA)).
					Return(&dns.Msg{}).Once()
				mockDnsOps.On("Exchange", mock.AnythingOfType("*dns.Client"), mock.AnythingOfType("*dns.Msg"), dnsServerAddress).
					Return(&dns.Msg{Answer: []dns.RR{generateRR(dnsName, tc.ipv6Address, "30")}}, time.Second, nil).Once()
			}

			dnsInfo, err := util.NewDNS("/etc/resolv.conf")
			require.NoError(t, err, "failed to create DNS resolver for %s", tc.name)
			require.NoError(t, dnsInfo.Add(dnsName), "failed to resolve DNS name for %s", tc.name)
			mockDnsOps.AssertExpectations(t)

			nbClient, _, cleanup, err := libovsdbtest.NewNBSBTestHarness(libovsdbtest.TestSetup{})
			require.NoError(t, err, "failed to create libovsdb test harness for %s", tc.name)
			t.Cleanup(cleanup.Cleanup)

			factory := addressset.NewOvnAddressSetFactory(nbClient, tc.ipv4Mode, tc.ipv6Mode)
			dbIDs := GetEgressFirewallDNSAddrSetDbIDs(dnsName, DefaultNetworkControllerName)
			initialAddresses := []string{}
			if tc.ipv4Address != "" {
				initialAddresses = append(initialAddresses, tc.ipv4Address)
			}
			if tc.ipv6Address != "" {
				initialAddresses = append(initialAddresses, tc.ipv6Address)
			}
			staleAddressSet, err := factory.NewAddressSet(dbIDs, initialAddresses)
			require.NoError(t, err, "failed to create initial address set for %s", tc.name)

			if tc.deleteIPv6Address {
				_, ipv6HashName := addressset.GetHashNamesForAS(dbIDs)
				require.NoError(t, libovsdbops.DeleteAddressSets(nbClient, &nbdb.AddressSet{Name: ipv6HashName}),
					"failed to delete IPv6 address set for %s", tc.name)
			} else {
				require.NoError(t, staleAddressSet.Destroy(), "failed to delete address set for %s", tc.name)
			}

			resolver := &EgressDNS{
				dns:               dnsInfo,
				dnsEntries:        map[string]*dnsEntry{dnsName: {dnsAddressSet: staleAddressSet}},
				addressSetFactory: factory,
				controllerName:    DefaultNetworkControllerName,
			}
			require.NoError(t, resolver.updateEntryForName(dnsName),
				"failed to update DNS entry for %s", tc.name)

			recreatedAddressSet, err := factory.GetAddressSet(dbIDs)
			require.NoError(t, err, "failed to retrieve recovered address set for %s", tc.name)
			addresses, ipv6Addresses := recreatedAddressSet.GetAddresses()
			expectedIPv4 := []string{}
			if tc.ipv4Address != "" {
				expectedIPv4 = append(expectedIPv4, tc.ipv4Address)
			}
			expectedIPv6 := []string{}
			if tc.ipv6Address != "" {
				expectedIPv6 = append(expectedIPv6, net.ParseIP(tc.ipv6Address).String())
			}
			assert.ElementsMatch(t, expectedIPv4, addresses,
				"recovered IPv4 addresses for %s", tc.name)
			assert.ElementsMatch(t, expectedIPv6, ipv6Addresses,
				"recovered IPv6 addresses for %s", tc.name)
		})
	}
}

// TestDeleteStaleAddrSetsKeepsReferencedMissingEntries verifies missing rows
// remain recoverable while unreferenced resolver entries are forgotten.
func TestDeleteStaleAddrSetsKeepsReferencedMissingEntries(t *testing.T) {
	staleDNSName := "stale.test.com"
	activeDNSName := "active.test.com"
	staleDBIDs := GetEgressFirewallDNSAddrSetDbIDs(staleDNSName, DefaultNetworkControllerName)
	activeDBIDs := GetEgressFirewallDNSAddrSetDbIDs(activeDNSName, DefaultNetworkControllerName)
	staleDBAddressSet, _ := addressset.GetTestDbAddrSets(staleDBIDs, nil)
	activeDBAddressSet, _ := addressset.GetTestDbAddrSets(activeDBIDs, nil)
	activeV4HashName := activeDBAddressSet.Name
	aclDBIDs := libovsdbops.NewDbObjectIDs(libovsdbops.ACLEgressFirewall, DefaultNetworkControllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: "test",
			libovsdbops.RuleIndex:     "0",
		})
	acl := libovsdbutil.BuildACLWithDefaultTier(aclDBIDs, 1000, activeV4HashName, nbdb.ACLActionAllow,
		nil, libovsdbutil.LportIngress)
	acl.UUID = "uactive-acl"
	portGroup := &nbdb.PortGroup{
		UUID: "uactive-port-group",
		ACLs: []string{acl.UUID},
	}
	nbClient, _, cleanup, err := libovsdbtest.NewNBSBTestHarness(libovsdbtest.TestSetup{
		NBData: []libovsdbtest.TestData{staleDBAddressSet, activeDBAddressSet, acl, portGroup},
	})
	require.NoError(t, err, "failed to create libovsdb test harness for stale-entry cleanup")
	t.Cleanup(cleanup.Cleanup)

	factory := addressset.NewOvnAddressSetFactory(nbClient, true, false)
	staleAddressSet, err := factory.GetAddressSet(staleDBIDs)
	require.NoError(t, err, "failed to retrieve stale DNS address set")
	activeAddressSet, err := factory.GetAddressSet(activeDBIDs)
	require.NoError(t, err, "failed to retrieve active DNS address set")
	require.NoError(t, activeAddressSet.Destroy(), "failed to simulate the missing active address-set row")

	resolver := &EgressDNS{
		dns: &util.DNS{},
		dnsEntries: map[string]*dnsEntry{
			staleDNSName: {
				namespaces:    map[string]struct{}{"namespace": {}},
				dnsAddressSet: staleAddressSet,
			},
			activeDNSName: {
				namespaces:    map[string]struct{}{"namespace": {}},
				dnsAddressSet: activeAddressSet,
			},
		},
		addressSetFactory: factory,
		controllerName:    DefaultNetworkControllerName,
		deleted:           make(chan string, 1),
	}

	require.NoError(t, resolver.DeleteStaleAddrSets(nbClient), "failed to remove stale EgressFirewall DNS address sets")
	_, staleEntryExists := resolver.dnsEntries[staleDNSName]
	_, activeEntryExists := resolver.dnsEntries[activeDNSName]
	assert.False(t, staleEntryExists, "unreferenced DNS entry should be removed")
	assert.True(t, activeEntryExists, "referenced missing DNS entry must be retained for recovery")

	predicateIDs := libovsdbops.NewDbObjectIDs(libovsdbops.AddressSetEgressFirewallDNS, DefaultNetworkControllerName, nil)
	addressSets, err := libovsdbops.FindAddressSetsWithPredicate(nbClient, libovsdbops.GetPredicate[*nbdb.AddressSet](predicateIDs, nil))
	require.NoError(t, err, "failed to list EgressFirewall DNS address sets after cleanup")
	assert.Empty(t, addressSets, "unreferenced address set should be deleted")
}

// TestDeleteStaleAddrSetsOnlyTracksEgressFirewallACLReferences verifies that
// other ACL types keep their referenced address-set rows but do not keep stale
// EgressDNS tracker entries alive.
func TestDeleteStaleAddrSetsOnlyTracksEgressFirewallACLReferences(t *testing.T) {
	const dnsName = "other-acl.test.com"
	dbIDs := GetEgressFirewallDNSAddrSetDbIDs(dnsName, DefaultNetworkControllerName)
	dbAddressSet, _ := addressset.GetTestDbAddrSets(dbIDs, nil)
	otherACL := &nbdb.ACL{
		UUID:  "uother-acl",
		Match: "ip4.dst == $" + dbAddressSet.Name,
		ExternalIDs: map[string]string{
			libovsdbops.OwnerControllerKey.String(): DefaultNetworkControllerName,
			libovsdbops.OwnerTypeKey.String():       libovsdbops.NetworkPolicyOwnerType,
		},
	}
	portGroup := &nbdb.PortGroup{
		UUID: "uother-port-group",
		ACLs: []string{otherACL.UUID},
	}
	nbClient, _, cleanup, err := libovsdbtest.NewNBSBTestHarness(libovsdbtest.TestSetup{
		NBData: []libovsdbtest.TestData{dbAddressSet, otherACL, portGroup},
	})
	require.NoError(t, err, "failed to create libovsdb test harness for non-EgressFirewall ACL reference")
	t.Cleanup(cleanup.Cleanup)

	factory := addressset.NewOvnAddressSetFactory(nbClient, true, false)
	dnsAddressSet, err := factory.GetAddressSet(dbIDs)
	require.NoError(t, err, "failed to retrieve DNS address set")
	resolver := &EgressDNS{
		dnsEntries: map[string]*dnsEntry{
			dnsName: {
				namespaces:    map[string]struct{}{"namespace": {}},
				dnsAddressSet: dnsAddressSet,
			},
		},
		addressSetFactory: factory,
		controllerName:    DefaultNetworkControllerName,
	}

	require.NoError(t, resolver.DeleteStaleAddrSets(nbClient), "failed to remove stale EgressFirewall DNS resolver entry")
	assert.NotContains(t, resolver.dnsEntries, dnsName,
		"a non-EgressFirewall ACL reference should not keep the DNS resolver entry alive")

	addressSets, err := libovsdbops.FindAddressSetsWithPredicate(nbClient,
		libovsdbops.GetPredicate[*nbdb.AddressSet](dbIDs, nil))
	require.NoError(t, err, "failed to query DNS address set after cleanup")
	assert.Len(t, addressSets, 1,
		"the generic GC should preserve an address set referenced by any ACL")
}

// TestDeleteStaleAddrSetsDefersDuringACLAttachment verifies GC waits for ACL
// attachment during both initial rule creation and a later resync.
func TestDeleteStaleAddrSetsDefersDuringACLAttachment(t *testing.T) {
	const dnsName = "pending.test.com"
	const namespace = "test-namespace"
	dbIDs := GetEgressFirewallDNSAddrSetDbIDs(dnsName, DefaultNetworkControllerName)
	dbAddressSet, _ := addressset.GetTestDbAddrSets(dbIDs, nil)
	nbClient, _, cleanup, err := libovsdbtest.NewNBSBTestHarness(libovsdbtest.TestSetup{
		NBData: []libovsdbtest.TestData{dbAddressSet},
	})
	require.NoError(t, err, "failed to create libovsdb test harness for pending ACL attachment")
	t.Cleanup(cleanup.Cleanup)

	factory := addressset.NewOvnAddressSetFactory(nbClient, true, false)
	dnsAddressSet, err := factory.GetAddressSet(dbIDs)
	require.NoError(t, err, "failed to retrieve the DNS address set before attachment")
	resolver := &EgressDNS{
		dnsEntries: map[string]*dnsEntry{
			dnsName: {
				namespaces:    map[string]struct{}{namespace: {}},
				dnsAddressSet: dnsAddressSet,
			},
		},
		addressSetFactory: factory,
		controllerName:    DefaultNetworkControllerName,
	}

	// The row exists before its first ACL reference is attached. Add marks this
	// handoff as pending so GC cannot remove the row and forget the resolver entry.
	_, err = resolver.Add(namespace, dnsName)
	require.NoError(t, err, "first Add should mark the initial ACL attachment as pending")
	_, err = resolver.Add(namespace, dnsName)
	require.NoError(t, err, "second Add should increment the pending ACL attachment count")
	assert.Equal(t, 2, resolver.dnsEntries[dnsName].pendingACLAttachments[namespace],
		"Add should mark the address set as pending until its ACL transaction completes")
	require.NoError(t, resolver.DeleteStaleAddrSets(nbClient), "GC should defer while the initial ACL attachment is pending")
	predicateIDs := libovsdbops.NewDbObjectIDs(libovsdbops.AddressSetEgressFirewallDNS,
		DefaultNetworkControllerName, nil)
	addressSets, err := libovsdbops.FindAddressSetsWithPredicate(nbClient,
		libovsdbops.GetPredicate[*nbdb.AddressSet](predicateIDs, nil))
	require.NoError(t, err, "failed to find DNS address sets after the initial GC pass")
	assert.Len(t, addressSets, 1, "pending address set should not be deleted")
	assert.Contains(t, resolver.dnsEntries, dnsName, "pending resolver entry should not be forgotten")

	// Simulate the ACL transaction completing after the first GC pass.
	ipv4HashName, _ := dnsAddressSet.GetASHashNames()
	aclIDs := libovsdbops.NewDbObjectIDs(libovsdbops.ACLEgressFirewall, DefaultNetworkControllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: "test",
			libovsdbops.RuleIndex:     "0",
		})
	aclMatch := "ip4.dst == $" + ipv4HashName
	acl := libovsdbutil.BuildACLWithDefaultTier(aclIDs, 1000, aclMatch, nbdb.ACLActionAllow,
		nil, libovsdbutil.LportIngress)
	// ACL rows are garbage-collected by OVN when no port group references them,
	// so create the ACL and its port-group reference in one transaction.
	ops, err := libovsdbops.CreateOrUpdateACLsOps(nbClient, nil, nil, acl)
	require.NoError(t, err, "failed to create the test ACL operations")
	require.NotEmpty(t, acl.UUID, "creating the ACL should assign a named UUID for its port-group reference")
	portGroup := &nbdb.PortGroup{Name: "pending-acl-test-port-group", ACLs: []string{acl.UUID}}
	ops, err = libovsdbops.CreateOrUpdatePortGroupsOps(nbClient, ops, portGroup)
	require.NoError(t, err, "failed to create the test port-group operations")
	_, err = libovsdbops.TransactAndCheck(nbClient, ops)
	require.NoError(t, err, "failed to commit the test ACL and port-group reference")
	require.Eventually(t, func() bool {
		acls, err := libovsdbops.FindACLsWithPredicate(nbClient, func(*nbdb.ACL) bool { return true })
		return err == nil && len(acls) == 1
	}, 5*time.Second, 10*time.Millisecond, "the test ACL should appear in the NBDB cache")
	acls, err := libovsdbops.FindACLsWithPredicate(nbClient, func(*nbdb.ACL) bool { return true })
	require.NoError(t, err, "failed to read the committed test ACL")
	require.Equal(t, aclMatch, acls[0].Match, "the test ACL should contain the address-set hash")
	resolver.CompleteAdd(namespace, dnsName)
	require.Equal(t, 1, resolver.dnsEntries[dnsName].pendingACLAttachments[namespace],
		"completing one of two Add calls should leave the other handoff pending")
	resolver.CompleteAdd(namespace, dnsName)
	require.NotContains(t, resolver.dnsEntries[dnsName].pendingACLAttachments, namespace,
		"completed Add should release its pending ACL attachment")

	require.NoError(t, resolver.DeleteStaleAddrSets(nbClient), "GC should retain the ACL-referenced DNS address set")
	addressSets, err = libovsdbops.FindAddressSetsWithPredicate(nbClient,
		libovsdbops.GetPredicate[*nbdb.AddressSet](predicateIDs, nil))
	require.NoError(t, err, "failed to find the retained DNS address set after ACL attachment")
	require.Len(t, addressSets, 1, "ACL-referenced address set should be retained")
	require.Contains(t, resolver.dnsEntries, dnsName, "ACL-referenced resolver entry should be retained")

	// A later reconcile temporarily removes the old ACL before attaching its
	// replacement. The existing set is unreferenced again while Add is pending.
	acl.Match = "ip4.src == 192.0.2.10"
	require.NoError(t, libovsdbops.CreateOrUpdateACLs(nbClient, nil, acl), "failed to remove the old ACL reference for resync")
	_, err = resolver.Add(namespace, dnsName)
	require.NoError(t, err, "resync Add should mark the replacement ACL attachment as pending")
	require.NoError(t, resolver.DeleteStaleAddrSets(nbClient), "GC should defer while the resync ACL attachment is pending")
	addressSets, err = libovsdbops.FindAddressSetsWithPredicate(nbClient,
		libovsdbops.GetPredicate[*nbdb.AddressSet](predicateIDs, nil))
	require.NoError(t, err, "failed to find the pending address set during resync")
	require.Len(t, addressSets, 1, "pending resync address set should not be deleted")
	require.Contains(t, resolver.dnsEntries, dnsName, "pending resync resolver entry should be retained")

	// Once the replacement ACL is committed, completing Add allows normal GC.
	acl.Match = aclMatch
	require.NoError(t, libovsdbops.CreateOrUpdateACLs(nbClient, nil, acl), "failed to commit the replacement ACL reference")
	require.Eventually(t, func() bool {
		acls, err := libovsdbops.FindACLsWithPredicate(nbClient, func(*nbdb.ACL) bool { return true })
		return err == nil && len(acls) == 1 && acls[0].Match == aclMatch
	}, 5*time.Second, 10*time.Millisecond, "the replacement ACL should reference the address set in the NBDB cache")
	resolver.CompleteAdd(namespace, dnsName)
	require.NoError(t, resolver.DeleteStaleAddrSets(nbClient), "GC should retain the resynced ACL-referenced DNS address set")
	addressSets, err = libovsdbops.FindAddressSetsWithPredicate(nbClient,
		libovsdbops.GetPredicate[*nbdb.AddressSet](predicateIDs, nil))
	require.NoError(t, err, "failed to find the retained address set after resync")
	require.Len(t, addressSets, 1, "resynced ACL-referenced address set should be retained")
}

// TestOrphanRecoversAfterFailedACLCommitAndGCSweep verifies that an address set
// left without an ACL after a failed reconcile is forgotten by GC and recreated
// by the next Add.
func TestOrphanRecoversAfterFailedACLCommitAndGCSweep(t *testing.T) {
	const (
		namespace = "failed-commit-namespace"
		dnsName   = "failed-commit.test.com."
	)

	require.NoError(t, config.PrepareTestConfig(), "failed to prepare test configuration for orphan recovery")
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
	mockDNSOps.On("Fqdn", dnsName).Return(dnsName).Twice()
	mockDNSOps.On("SetQuestion", mock.AnythingOfType("*dns.Msg"), dnsName, uint16(dns.TypeA)).
		Return(&dns.Msg{}).Twice()
	dnsAnswer := &dns.Msg{Answer: []dns.RR{generateRR("failed-commit.test.com", "192.0.2.10", "30")}}
	mockDNSOps.On("Exchange", mock.AnythingOfType("*dns.Client"), mock.AnythingOfType("*dns.Msg"), "192.0.2.53:53").
		Return(dnsAnswer, time.Second, nil).Twice()
	t.Cleanup(func() { util.SetDNSLibOpsMockInst(previousDNSOps) })

	nbClient, _, cleanup, err := libovsdbtest.NewNBSBTestHarness(libovsdbtest.TestSetup{})
	require.NoError(t, err, "failed to create libovsdb test harness for orphan recovery")
	t.Cleanup(cleanup.Cleanup)

	factory := addressset.NewOvnAddressSetFactory(nbClient, true, false)
	egressDNS, err := NewEgressDNS(factory, DefaultNetworkControllerName, make(chan struct{}), 0)
	require.NoError(t, err, "failed to create EgressDNS for orphan recovery")

	_, err = egressDNS.Add(namespace, dnsName)
	require.NoError(t, err, "failed to add DNS name before simulating the failed ACL commit")
	// Add populates DNS asynchronously. Wait for the refresh before simulating
	// the failed ACL transaction and subsequent GC pass.
	select {
	case <-egressDNS.added:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the initial DNS refresh")
	}

	asIDs := GetEgressFirewallDNSAddrSetDbIDs(dnsName, DefaultNetworkControllerName)
	asPredicate := libovsdbops.GetPredicate[*nbdb.AddressSet](asIDs, nil)
	firstRows, err := libovsdbops.FindAddressSetsWithPredicate(nbClient, asPredicate)
	require.NoError(t, err, "failed to find the initial DNS address set")
	require.Len(t, firstRows, 1, "Add should create one AddressSet")
	firstUUID := firstRows[0].UUID

	egressDNS.lock.Lock()
	firstEntry, exists := egressDNS.dnsEntries[dnsName]
	pendingAttachments := 0
	if exists {
		pendingAttachments = firstEntry.pendingACLAttachments[namespace]
	}
	egressDNS.lock.Unlock()
	require.True(t, exists, "Add should register the DNS entry")
	require.Equal(t, 1, pendingAttachments, "Add should mark its ACL handoff pending")

	// Hold the old entry's asynchronous DNS cleanup while the same name is
	// retried. Add must wait for cleanup before installing a new DNS tracker
	// entry, so the old deletion cannot remove the new entry.
	firstEntry.dnsOperationLock.Lock()
	entryLockHeld := true
	t.Cleanup(func() {
		if entryLockHeld {
			firstEntry.dnsOperationLock.Unlock()
		}
	})

	// No ACL is created: this models the final ACL transaction failing after Add
	// has created the AddressSet. addEgressFirewallRules' deferred callback still
	// runs when that transaction fails.
	egressDNS.CompleteAdd(namespace, dnsName)
	require.NoError(t, egressDNS.DeleteStaleAddrSets(nbClient), "failed to garbage-collect the orphaned DNS address set")

	deletedRows, err := libovsdbops.FindAddressSetsWithPredicate(nbClient, asPredicate)
	require.NoError(t, err, "failed to verify the orphaned address-set row was deleted")
	require.Empty(t, deletedRows, "GC should delete the unreferenced AddressSet row")

	egressDNS.lock.Lock()
	_, entryExists := egressDNS.dnsEntries[dnsName]
	egressDNS.lock.Unlock()
	require.False(t, entryExists, "GC should forget the resolver entry with no ACL reference")

	egressDNS.lock.Lock()
	deletingEntry, isDeleting := egressDNS.pendingDNSDeletes[dnsName]
	egressDNS.lock.Unlock()
	require.True(t, isDeleting, "GC should keep a deletion marker until DNS cleanup finishes")
	require.Same(t, firstEntry, deletingEntry, "GC should associate pending cleanup with the removed entry")

	retryResult := make(chan error, 1)
	retryStarted := make(chan struct{})
	go func() {
		close(retryStarted)
		_, retryErr := egressDNS.Add(namespace, dnsName)
		retryResult <- retryErr
	}()
	<-retryStarted
	select {
	case retryErr := <-retryResult:
		t.Fatalf("Add should wait for the previous DNS cleanup, returned early with error %v", retryErr)
	case <-time.After(50 * time.Millisecond):
	}

	firstEntry.dnsOperationLock.Unlock()
	entryLockHeld = false
	select {
	case deletedName := <-egressDNS.deleted:
		require.Equal(t, dnsName, deletedName)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for delayed GC cleanup to finish")
	}
	select {
	case err = <-retryResult:
		require.NoError(t, err, "retry should create a new entry after DNS cleanup finishes")
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for Add to resume after DNS cleanup")
	}
	select {
	case <-egressDNS.added:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the retried DNS refresh")
	}
	require.Equal(t, 1, egressDNS.dns.Size(), "late cleanup must preserve the re-added DNS tracker entry")
	mockDNSOps.AssertExpectations(t)

	newRows, err := libovsdbops.FindAddressSetsWithPredicate(nbClient, asPredicate)
	require.NoError(t, err, "failed to find the recreated DNS address set")
	require.Len(t, newRows, 1, "retry should create one fresh AddressSet")
	require.NotEqual(t, firstUUID, newRows[0].UUID, "retry should not reuse the deleted AddressSet row")
	require.NoError(t, egressDNS.updateEntryForName(dnsName), "the recreated resolver entry should refresh successfully")
}

func TestAdd(t *testing.T) {
	mockAddressSetFactoryOps := new(mocks.AddressSetFactory)
	mockAddressSetOps := new(mocks.AddressSet)
	mockDnsOps := new(util_mocks.DNSOps)
	util.SetDNSLibOpsMockInst(mockDnsOps)
	test1DNSName := "www.test.com"
	test1IPv4 := "2.2.2.2"
	test1IPv4Update := "3.3.3.3"
	test1IPv6 := "2001:0db8:85a3:0000:0000:8a2e:0370:7334"
	clusterSubnetStr := "10.128.0.0/14"
	_, clusterSubnet, _ := net.ParseCIDR(clusterSubnetStr)
	clusterSubnetIP := "10.128.0.1"
	tests := []struct {
		desc                       string
		errExp                     bool
		dnsName                    string
		configIPv4                 bool
		configIPv6                 bool
		testingUpdateOnQueryTime   bool
		syncTime                   time.Duration
		waitForSyncLoop            bool
		dnsOpsMockHelper           []ovntest.TestifyMockHelper
		addressSetFactoryOpsHelper []ovntest.TestifyMockHelper
		addressSetOpsHelper        []ovntest.TestifyMockHelper
	}{
		{
			desc:     "NewAddressSet returns error",
			errExp:   true,
			syncTime: 5 * time.Minute,
			dnsOpsMockHelper: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName:    "ClientConfigFromFile",
					OnCallMethodArgType: []string{"string"},
					RetArgList:          []interface{}{&dns.ClientConfig{Servers: []string{"1.1.1.1"}, Port: "1234"}, nil},
					CallTimes:           1,
				},
			},
			addressSetFactoryOpsHelper: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName:    "NewAddressSet",
					OnCallMethodArgType: []string{"*ops.DbObjectIDs", "[]string"},
					RetArgList:          []interface{}{nil, fmt.Errorf("mock error")},
					CallTimes:           1,
				},
			},
		},
		{
			desc:       "EgressFirewall Add(dnsName) succeeds IPv4 only",
			errExp:     false,
			syncTime:   5 * time.Minute,
			dnsName:    test1DNSName,
			configIPv4: true,
			configIPv6: false,

			dnsOpsMockHelper: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName: "ClientConfigFromFile", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{&dns.ClientConfig{
						Servers: []string{"1.1.1.1"},
						Port:    "1234"}, nil}, CallTimes: 1,
				},
				{OnCallMethodName: "Fqdn", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{test1DNSName}, CallTimes: 1},
				{OnCallMethodName: "SetQuestion", OnCallMethodArgType: []string{"*dns.Msg", "string", "uint16"}, RetArgList: []interface{}{&dns.Msg{}}, CallTimes: 1},
				{
					OnCallMethodName:    "Exchange",
					OnCallMethodArgType: []string{"*dns.Client", "*dns.Msg", "string"},
					RetArgList:          []interface{}{&dns.Msg{Answer: []dns.RR{generateRR(test1DNSName, test1IPv4, "300")}}, 500 * time.Second, nil},
					CallTimes:           1,
				},
			},
			addressSetFactoryOpsHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "NewAddressSet", OnCallMethodArgType: []string{"*ops.DbObjectIDs", "[]string"}, OnCallMethodArgs: []interface{}{}, RetArgList: []interface{}{mockAddressSetOps, nil}, OnCallMethodsArgsStrTypeAppendCount: 0, CallTimes: 1},
			},
			addressSetOpsHelper: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName: "SetAddresses",
					OnCallMethodArgs: []interface{}{[]string{test1IPv4}},
					RetArgList:       []interface{}{nil},
				},
			},
		},
		{
			desc:       "EgressFirewall Add(dnsName) ignores ips from clusterSubnet",
			errExp:     false,
			syncTime:   5 * time.Minute,
			dnsName:    test1DNSName,
			configIPv4: true,
			configIPv6: false,

			dnsOpsMockHelper: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName:    "ClientConfigFromFile",
					OnCallMethodArgType: []string{"string"},
					RetArgList:          []interface{}{&dns.ClientConfig{Servers: []string{"1.1.1.1"}, Port: "1234"}, nil},
					CallTimes:           1,
				},
				{OnCallMethodName: "Fqdn", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{test1DNSName}, CallTimes: 1},
				{OnCallMethodName: "SetQuestion", OnCallMethodArgType: []string{"*dns.Msg", "string", "uint16"}, RetArgList: []interface{}{&dns.Msg{}}, CallTimes: 1},
				{
					OnCallMethodName:    "Exchange",
					OnCallMethodArgType: []string{"*dns.Client", "*dns.Msg", "string"},
					RetArgList:          []interface{}{&dns.Msg{Answer: []dns.RR{generateRR(test1DNSName, clusterSubnetIP, "300")}}, 500 * time.Second, nil},
					CallTimes:           1,
				},
			},
			addressSetFactoryOpsHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "NewAddressSet", OnCallMethodArgType: []string{"*ops.DbObjectIDs", "[]string"}, OnCallMethodArgs: []interface{}{}, RetArgList: []interface{}{mockAddressSetOps, nil}, OnCallMethodsArgsStrTypeAppendCount: 0, CallTimes: 1},
			},
			addressSetOpsHelper: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName: "SetAddresses",
					OnCallMethodArgs: []interface{}{[]string{}},
					RetArgList:       []interface{}{nil},
				},
			},
		},
		{
			desc:       "EgressFirewall Add(dnsName) ignores ips from clusterSubnet leaving other ips",
			errExp:     false,
			syncTime:   5 * time.Minute,
			dnsName:    test1DNSName,
			configIPv4: true,
			configIPv6: false,

			dnsOpsMockHelper: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName:    "ClientConfigFromFile",
					OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{&dns.ClientConfig{Servers: []string{"1.1.1.1"}, Port: "1234"}, nil},
					CallTimes: 1,
				},
				{OnCallMethodName: "Fqdn", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{test1DNSName}, CallTimes: 1},
				{OnCallMethodName: "SetQuestion", OnCallMethodArgType: []string{"*dns.Msg", "string", "uint16"}, RetArgList: []interface{}{&dns.Msg{}}, CallTimes: 1},
				{
					OnCallMethodName:    "Exchange",
					OnCallMethodArgType: []string{"*dns.Client", "*dns.Msg", "string"},
					RetArgList:          []interface{}{&dns.Msg{Answer: []dns.RR{generateRR(test1DNSName, test1IPv4, "300"), generateRR(test1DNSName, clusterSubnetIP, "300")}}, 500 * time.Second, nil},
					CallTimes:           1,
				},
			},
			addressSetFactoryOpsHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "NewAddressSet", OnCallMethodArgType: []string{"*ops.DbObjectIDs", "[]string"}, RetArgList: []interface{}{mockAddressSetOps, nil}, CallTimes: 1},
			},
			addressSetOpsHelper: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName: "SetAddresses",
					OnCallMethodArgs: []interface{}{[]string{test1IPv4}},
					RetArgList:       []interface{}{nil},
				},
			},
		},
		{
			desc:                     "EgressFirewall Add(dnsName) succeeds dual stack",
			errExp:                   false,
			syncTime:                 5 * time.Minute,
			dnsName:                  test1DNSName,
			testingUpdateOnQueryTime: false,
			configIPv4:               true,
			configIPv6:               true,

			dnsOpsMockHelper: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName:    "ClientConfigFromFile",
					OnCallMethodArgType: []string{"string"},
					RetArgList:          []interface{}{&dns.ClientConfig{Servers: []string{"1.1.1.1"}, Port: "1234"}, nil},
					CallTimes:           1,
				},
				{OnCallMethodName: "Fqdn", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{test1DNSName}, CallTimes: 1},
				{OnCallMethodName: "Fqdn", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{test1DNSName}, CallTimes: 1},
				{OnCallMethodName: "SetQuestion", OnCallMethodArgType: []string{"*dns.Msg", "string", "uint16"}, RetArgList: []interface{}{&dns.Msg{}}, CallTimes: 1},
				{OnCallMethodName: "SetQuestion", OnCallMethodArgType: []string{"*dns.Msg", "string", "uint16"}, RetArgList: []interface{}{&dns.Msg{}}, CallTimes: 1},
				{
					OnCallMethodName:    "Exchange",
					OnCallMethodArgType: []string{"*dns.Client", "*dns.Msg", "string"},
					RetArgList:          []interface{}{&dns.Msg{Answer: []dns.RR{generateRR(test1DNSName, test1IPv4, "300")}}, 500 * time.Second, nil},
					CallTimes:           1,
				},
				{
					OnCallMethodName:    "Exchange",
					OnCallMethodArgType: []string{"*dns.Client", "*dns.Msg", "string"},
					RetArgList:          []interface{}{&dns.Msg{Answer: []dns.RR{generateRR(test1DNSName, test1IPv6, "300")}}, 500 * time.Second, nil},
					CallTimes:           1,
				},
			},
			addressSetFactoryOpsHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "NewAddressSet", OnCallMethodArgType: []string{"*ops.DbObjectIDs", "[]string"}, OnCallMethodArgs: []interface{}{}, RetArgList: []interface{}{mockAddressSetOps, nil}, OnCallMethodsArgsStrTypeAppendCount: 0, CallTimes: 1},
			},
			addressSetOpsHelper: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName: "SetAddresses",
					OnCallMethodArgs: []interface{}{[]string{test1IPv4, net.ParseIP(test1IPv6).String()}},
					RetArgList:       []interface{}{nil},
				},
			},
		},
		{
			desc:                     "EgressFirewall DNS Run Runs update after the ttl returned from the DNS server expires",
			errExp:                   false,
			dnsName:                  test1DNSName,
			testingUpdateOnQueryTime: true,
			syncTime:                 5 * time.Minute,
			configIPv4:               true,
			configIPv6:               false,

			dnsOpsMockHelper: []ovntest.TestifyMockHelper{

				{OnCallMethodName: "ClientConfigFromFile",
					OnCallMethodArgType: []string{"string"},
					RetArgList:          []interface{}{&dns.ClientConfig{Servers: []string{"1.1.1.1"}, Port: "1234"}, nil},
					CallTimes:           1,
				},
				{OnCallMethodName: "Fqdn", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{test1DNSName}, CallTimes: 1},

				{OnCallMethodName: "SetQuestion", OnCallMethodArgType: []string{"*dns.Msg", "string", "uint16"}, RetArgList: []interface{}{&dns.Msg{}}, CallTimes: 1},
				// return a very low ttl so that the update based on ttl timeout occurs
				{
					OnCallMethodName:    "Exchange",
					OnCallMethodArgType: []string{"*dns.Client", "*dns.Msg", "string"},
					RetArgList:          []interface{}{&dns.Msg{Answer: []dns.RR{generateRR(test1DNSName, test1IPv4, "4")}}, 1 * time.Second, nil},
					CallTimes:           1,
				},
				{OnCallMethodName: "Fqdn", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{test1DNSName}, CallTimes: 1},
				{OnCallMethodName: "SetQuestion", OnCallMethodArgType: []string{"*dns.Msg", "string", "uint16"}, RetArgList: []interface{}{&dns.Msg{}}, CallTimes: 1},
				{
					OnCallMethodName:    "Exchange",
					OnCallMethodArgType: []string{"*dns.Client", "*dns.Msg", "string"},
					RetArgList:          []interface{}{&dns.Msg{Answer: []dns.RR{generateRR(test1DNSName, test1IPv4Update, "300")}}, 1 * time.Second, nil},
					CallTimes:           1,
				},
			},
			addressSetFactoryOpsHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "NewAddressSet", OnCallMethodArgType: []string{"*ops.DbObjectIDs", "[]string"}, OnCallMethodArgs: []interface{}{}, RetArgList: []interface{}{mockAddressSetOps, nil}, OnCallMethodsArgsStrTypeAppendCount: 0, CallTimes: 1},
			},
			addressSetOpsHelper: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName: "SetAddresses",
					OnCallMethodArgs: []interface{}{[]string{test1IPv4}},
					RetArgList:       []interface{}{nil},
				},
				{
					OnCallMethodName: "SetAddresses",
					OnCallMethodArgs: []interface{}{[]string{test1IPv4Update}},
					RetArgList:       []interface{}{nil},
				},
			},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			testCh := make(chan struct{})
			config.IPv4Mode = tc.configIPv4
			config.IPv6Mode = tc.configIPv6
			config.Default.ClusterSubnets = []config.CIDRNetworkEntry{{CIDR: clusterSubnet}}

			for _, item := range tc.dnsOpsMockHelper {
				call := mockDnsOps.On(item.OnCallMethodName)
				for _, arg := range item.OnCallMethodArgType {
					call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range item.RetArgList {
					call.ReturnArguments = append(call.ReturnArguments, ret)
				}
				call.Once()
			}
			for _, item := range tc.addressSetFactoryOpsHelper {
				call := mockAddressSetFactoryOps.On(item.OnCallMethodName)
				for _, arg := range item.OnCallMethodArgType {
					call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range item.RetArgList {
					call.ReturnArguments = append(call.ReturnArguments, ret)
				}
				call.Once()
			}
			for _, item := range tc.addressSetOpsHelper {
				call := mockAddressSetOps.On(item.OnCallMethodName)
				// use exact arguments for AddressSet call to match ips
				call.Arguments = item.OnCallMethodArgs
				for _, ret := range item.RetArgList {
					call.ReturnArguments = append(call.ReturnArguments, ret)
				}
				call.Once()
			}
			res, err := NewEgressDNS(mockAddressSetFactoryOps, DefaultNetworkControllerName, testCh, tc.syncTime)
			require.NoError(t, err)

			err = res.Run()
			require.NoError(t, err)

			_, err = res.Add("addNamespace", test1DNSName)
			if tc.errExp {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				for stay, timeout := true, time.After(10*time.Second); stay; {
					_, dnsResolves, _ := res.getDNSEntry(tc.dnsName)
					if dnsResolves != nil {
						break
					}
					select {
					case <-timeout:
						stay = false
						t.Errorf("timeout: it is taking too long for the goroutine to complete")
					default:
					}

				}
			}

			if tc.testingUpdateOnQueryTime {
				for stay, timeout := true, time.After(15*time.Second); stay; {
					_, dnsResolves, _ := res.getDNSEntry(tc.dnsName)
					if dnsResolves != nil {
						if len(dnsResolves) == 1 && dnsResolves[0].String() == test1IPv4Update {
							break
						}
					}
					select {
					case <-timeout:
						stay = false
						t.Errorf("timeout waiting for update based on ttl to fire or process")
					default:
					}

				}
			}

			close(testCh)
			mockDnsOps.AssertExpectations(t)
			mockAddressSetFactoryOps.AssertExpectations(t)
			mockAddressSetOps.AssertExpectations(t)

			mockDnsOps.ExpectedCalls = nil
			mockAddressSetFactoryOps.ExpectedCalls = nil
			mockAddressSetOps.ExpectedCalls = nil
		})
	}
}

func TestDelete(t *testing.T) {
	mockAddressSetFactoryOps := new(mocks.AddressSetFactory)
	mockAddressSetOps := new(mocks.AddressSet)
	mockDnsOps := new(util_mocks.DNSOps)
	util.SetDNSLibOpsMockInst(mockDnsOps)
	test1DNSName := "www.test.com"
	test1IPv4 := "2.2.2.2"
	test1IPv6 := "2001:0db8:85a3:0000:0000:8a2e:0370:7334"
	tests := []struct {
		desc                       string
		errExp                     bool
		dnsName                    string
		configIPv4                 bool
		configIPv6                 bool
		testingUpdateOnQueryTime   bool
		syncTime                   time.Duration
		waitForSyncLoop            bool
		dnsOpsMockHelper           []ovntest.TestifyMockHelper
		addressSetFactoryOpsHelper []ovntest.TestifyMockHelper
		addressSetOpsHelper        []ovntest.TestifyMockHelper
	}{
		{
			desc:                     "EgressFirewall Delete functions",
			errExp:                   false,
			syncTime:                 5 * time.Minute,
			dnsName:                  test1DNSName,
			testingUpdateOnQueryTime: false,
			configIPv4:               true,
			configIPv6:               true,

			dnsOpsMockHelper: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName:    "ClientConfigFromFile",
					OnCallMethodArgType: []string{"string"},
					RetArgList:          []interface{}{&dns.ClientConfig{Servers: []string{"1.1.1.1"}, Port: "1234"}, nil},
					CallTimes:           1,
				},
				{OnCallMethodName: "Fqdn", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{test1DNSName}, CallTimes: 1},
				{OnCallMethodName: "Fqdn", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{test1DNSName}, CallTimes: 1},
				{OnCallMethodName: "SetQuestion", OnCallMethodArgType: []string{"*dns.Msg", "string", "uint16"}, RetArgList: []interface{}{&dns.Msg{}}, CallTimes: 1},
				{OnCallMethodName: "SetQuestion", OnCallMethodArgType: []string{"*dns.Msg", "string", "uint16"}, RetArgList: []interface{}{&dns.Msg{}}, CallTimes: 1},
				{
					OnCallMethodName:    "Exchange",
					OnCallMethodArgType: []string{"*dns.Client", "*dns.Msg", "string"},
					RetArgList:          []interface{}{&dns.Msg{Answer: []dns.RR{generateRR(test1DNSName, test1IPv4, "300")}}, 500 * time.Second, nil},
					CallTimes:           1,
				},
				{
					OnCallMethodName:    "Exchange",
					OnCallMethodArgType: []string{"*dns.Client", "*dns.Msg", "string"},
					RetArgList:          []interface{}{&dns.Msg{Answer: []dns.RR{generateRR(test1DNSName, test1IPv6, "300")}}, 500 * time.Second, nil},
					CallTimes:           1,
				},
			},
			addressSetFactoryOpsHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "NewAddressSet", OnCallMethodArgType: []string{"*ops.DbObjectIDs", "[]string"}, OnCallMethodArgs: []interface{}{}, RetArgList: []interface{}{mockAddressSetOps, nil}, OnCallMethodsArgsStrTypeAppendCount: 0, CallTimes: 1},
			},
			addressSetOpsHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "SetAddresses", OnCallMethodArgType: []string{"[]string"}, OnCallMethodArgs: []interface{}{}, RetArgList: []interface{}{nil}, OnCallMethodsArgsStrTypeAppendCount: 0, CallTimes: 1},
				{OnCallMethodName: "Destroy", OnCallMethodArgType: []string{}, OnCallMethodArgs: []interface{}{}, RetArgList: []interface{}{nil}, OnCallMethodsArgsStrTypeAppendCount: 0, CallTimes: 1},
			},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			testCh := make(chan struct{})
			config.IPv4Mode = tc.configIPv4
			config.IPv6Mode = tc.configIPv6

			for _, item := range tc.dnsOpsMockHelper {
				call := mockDnsOps.On(item.OnCallMethodName)
				for _, arg := range item.OnCallMethodArgType {
					call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range item.RetArgList {
					call.ReturnArguments = append(call.ReturnArguments, ret)
				}
				call.Once()
			}
			for _, item := range tc.addressSetFactoryOpsHelper {
				call := mockAddressSetFactoryOps.On(item.OnCallMethodName)
				for _, arg := range item.OnCallMethodArgType {
					call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range item.RetArgList {
					call.ReturnArguments = append(call.ReturnArguments, ret)
				}
				call.Once()
			}
			for _, item := range tc.addressSetOpsHelper {
				call := mockAddressSetOps.On(item.OnCallMethodName)
				for _, arg := range item.OnCallMethodArgType {
					call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range item.RetArgList {
					call.ReturnArguments = append(call.ReturnArguments, ret)
				}
				call.Once()
			}
			res, err := NewEgressDNS(mockAddressSetFactoryOps, DefaultNetworkControllerName, testCh, tc.syncTime)
			require.NoError(t, err)

			err = res.Run()
			require.NoError(t, err)

			_, err = res.Add("addNamespace", test1DNSName)
			if tc.errExp {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				for stay, timeout := true, time.After(10*time.Second); stay; {
					_, dnsResolves, _ := res.getDNSEntry(tc.dnsName)
					if dnsResolves != nil {
						break
					}
					select {
					case <-timeout:
						stay = false
						t.Errorf("timeout: it is taking too long for the goroutine to complete")
					default:
					}

				}
			}
			_, dnsResolves, _ := res.getDNSEntry(tc.dnsName)
			err = res.Delete("addNamespace")
			require.NoError(t, err)
			for stay, timeout := true, time.After(10*time.Second); stay; {
				_, dnsResolves, _ = res.getDNSEntry(tc.dnsName)
				if dnsResolves == nil {
					break
				}
				select {
				case <-timeout:
					stay = false
					t.Errorf("timeout: dns is taking to long for the goroutine to update the dns object")
				default:
				}
			}

			assert.Nil(t, dnsResolves)

			close(testCh)
			mockDnsOps.AssertExpectations(t)
			mockAddressSetFactoryOps.AssertExpectations(t)
			mockAddressSetOps.AssertExpectations(t)

			mockDnsOps.ExpectedCalls = nil
			mockAddressSetFactoryOps.ExpectedCalls = nil
			mockAddressSetOps.ExpectedCalls = nil
		})
	}
}

func (e *EgressDNS) getDNSEntry(dnsName string) (map[string]struct{}, []net.IP, addressset.AddressSet) {
	e.lock.Lock()
	defer e.lock.Unlock()
	if dnsEntry, exists := e.dnsEntries[dnsName]; exists {
		return dnsEntry.namespaces, dnsEntry.dnsResolves, dnsEntry.dnsAddressSet
	}

	return nil, nil, nil
}
