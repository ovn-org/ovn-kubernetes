package node

import (
	"log"
	"reflect"
	"strings"
	"testing"

	"github.com/coreos/go-iptables/iptables"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	nodeiptables "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/iptables"
	linkMock "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/mocks/github.com/vishvananda/netlink"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	utilMock "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/mocks"
	"github.com/vishvananda/netlink"
)

func TestSyncPhysicalInterfaceDropForwardingRule(t *testing.T) {
	// With the following list of links ...
	links := []struct {
		linkName   string
		linkType   string
		isPhysical bool
	}{
		{
			linkName:   "ens1f0",
			linkType:   "device",
			isPhysical: true,
		},
		{
			linkName: "lo",
			linkType: "device",
		},
		{
			linkName:   "ens1f0.100",
			linkType:   "vlan",
			isPhysical: true,
		},
		{
			linkName: "veth0",
			linkType: "veth",
		},
	}

	// ... we expect to see the following IPTables rules.
	expectedResult := map[iptables.Protocol]map[string][]string{
		iptables.ProtocolIPv4: {
			"FORWARD": {"-A FORWARD -j OVN-KUBE-FORWARD-DROP"},
			"OVN-KUBE-FORWARD-DROP": {
				"-A OVN-KUBE-FORWARD-DROP -i ens1f0 -j DROP",
				"-A OVN-KUBE-FORWARD-DROP -o ens1f0 -j DROP",
				"-A OVN-KUBE-FORWARD-DROP -i ens1f0.100 -j DROP",
				"-A OVN-KUBE-FORWARD-DROP -o ens1f0.100 -j DROP",
			},
		},
		iptables.ProtocolIPv6: {
			"FORWARD": {"-A FORWARD -j OVN-KUBE-FORWARD-DROP"},
			"OVN-KUBE-FORWARD-DROP": {
				"-A OVN-KUBE-FORWARD-DROP -i ens1f0 -j DROP",
				"-A OVN-KUBE-FORWARD-DROP -o ens1f0 -j DROP",
				"-A OVN-KUBE-FORWARD-DROP -i ens1f0.100 -j DROP",
				"-A OVN-KUBE-FORWARD-DROP -o ens1f0.100 -j DROP",
			},
		},
	}

	// Create mock links.
	var netlinkMock utilMock.NetLinkOps
	util.SetNetLinkOpMockInst(&netlinkMock)
	defer util.ResetNetLinkOpMockInst()
	var mockLinks []netlink.Link
	for _, l := range links {
		mockLink := &linkMock.Link{}
		lnkAttr := &netlink.LinkAttrs{
			Name: l.linkName,
		}
		mockLink.On("Attrs").Return(lnkAttr)
		mockLink.On("Type").Return(l.linkType)
		mockLinks = append(mockLinks, mockLink)
	}
	netlinkMock.On("LinkList").Return(mockLinks, nil)

	// Mock iptables.
	iptV4, iptV6 := util.SetFakeIPTablesHelpers()

	// Run the sync for both IP AFs.
	config.IPv4Mode = true
	config.IPv6Mode = true
	if err := syncPhysicalInterfaceDropForwardingRules(); err != nil {
		t.Fatalf("creation of drop forwarding rules failed, err: %q", err)
	}

	// Verify that we got the correct iptables rules.
	for _, chain := range []string{"FORWARD", "OVN-KUBE-FORWARD-DROP"} {
		out, err := iptV4.List("filter", chain)
		if err != nil {
			log.Fatalf("Could not list IPv4 mock iptables rules for %q chain, err: %q", chain, err)
		}
		if !reflect.DeepEqual(out, expectedResult[iptables.ProtocolIPv4][chain]) {
			log.Fatalf("Rules in AF IPv4, table filter, chain %q do not match expected rules; got: %v; expected: %v",
				chain, out, expectedResult[iptables.ProtocolIPv4][chain])
		}
		out, err = iptV6.List("filter", chain)
		if err != nil {
			log.Fatalf("Could not list IPv6 mock iptables rules for %q chain, err: %q", chain, err)
		}
		if !reflect.DeepEqual(out, expectedResult[iptables.ProtocolIPv6][chain]) {
			log.Fatalf("Rules in AF IPv6, table filter, chain %q do not match expected rules; got: %v; expected: %v",
				chain, out, expectedResult[iptables.ProtocolIPv6][chain])
		}
	}
}

func TestSyncCleanupPhysicalInterfaceDropForwardingRules(t *testing.T) {
	var err error
	iptV4, iptV6 := util.SetFakeIPTablesHelpers()

	// Populate chains and rules.
	inputRules := []nodeiptables.Rule{
		{
			Table:    "filter",
			Chain:    "FORWARD",
			Args:     []string{"-i", "breth0", "-j", "DROP"},
			Protocol: iptables.ProtocolIPv4,
		},
		{
			Table:    "filter",
			Chain:    "FORWARD",
			Args:     []string{"-o", "breth0", "-j", "DROP"},
			Protocol: iptables.ProtocolIPv4,
		},
		{
			Table:    "filter",
			Chain:    "FORWARD",
			Args:     []string{"-j", "OVN-KUBE-FORWARD-DROP"},
			Protocol: iptables.ProtocolIPv4,
		},
		{
			Table:    "filter",
			Chain:    "OVN-KUBE-FORWARD-DROP",
			Args:     []string{"-i", "eth1", "-j", "DROP"},
			Protocol: iptables.ProtocolIPv4,
		},
		{
			Table:    "filter",
			Chain:    "OVN-KUBE-FORWARD-DROP",
			Args:     []string{"-o", "eth1", "-j", "DROP"},
			Protocol: iptables.ProtocolIPv4,
		},
		{
			Table:    "filter",
			Chain:    "FORWARD",
			Args:     []string{"-i", "breth0", "-j", "DROP"},
			Protocol: iptables.ProtocolIPv6,
		},
		{
			Table:    "filter",
			Chain:    "FORWARD",
			Args:     []string{"-o", "breth0", "-j", "DROP"},
			Protocol: iptables.ProtocolIPv6,
		},
		{
			Table:    "filter",
			Chain:    "FORWARD",
			Args:     []string{"-j", "OVN-KUBE-FORWARD-DROP"},
			Protocol: iptables.ProtocolIPv6,
		},
		{
			Table:    "filter",
			Chain:    "OVN-KUBE-FORWARD-DROP",
			Args:     []string{"-i", "eth1", "-j", "DROP"},
			Protocol: iptables.ProtocolIPv6,
		},
		{
			Table:    "filter",
			Chain:    "OVN-KUBE-FORWARD-DROP",
			Args:     []string{"-o", "eth1", "-j", "DROP"},
			Protocol: iptables.ProtocolIPv6,
		},
	}

	chains := [][]string{
		{"filter", "FORWARD"},
		{"filter", "OVN-KUBE-FORWARD-DROP"},
	}

	expectedResult := map[iptables.Protocol][]string{
		iptables.ProtocolIPv4: {"-A FORWARD -i breth0 -j DROP", "-A FORWARD -o breth0 -j DROP"},
		iptables.ProtocolIPv6: {"-A FORWARD -i breth0 -j DROP", "-A FORWARD -o breth0 -j DROP"},
	}

	// Create chains for both IPv4 and IPv6.
	for _, chain := range chains {
		if err := iptV4.NewChain(chain[0], chain[1]); err != nil {
			t.Fatalf("Error creating new chain for IPv4, err: %q", err)
		}
		if err := iptV6.NewChain(chain[0], chain[1]); err != nil {
			t.Fatalf("Error creating new chain for IPv6, err: %q", err)
		}
	}

	// Append all rules.
	for _, i := range inputRules {
		if i.Protocol == iptables.ProtocolIPv4 {
			if err := iptV4.Append(i.Table, i.Chain, i.Args...); err != nil {
				t.Fatalf("Error appending rule for IPv4, err: %q", err)
			}
		}
		if i.Protocol == iptables.ProtocolIPv6 {
			if err := iptV6.Append(i.Table, i.Chain, i.Args...); err != nil {
				t.Fatalf("Error appending rule for IPv6, err: %q", err)
			}
		}
	}

	// Run cleanup.
	config.IPv4Mode = true
	config.IPv6Mode = true
	if err := syncCleanupPhysicalInterfaceDropForwardingRules(); err != nil {
		t.Fatalf("cleanup of drop forwarding rules failed, err: %q", err)
	}

	out, err := iptV4.List("filter", "FORWARD")
	if err != nil {
		log.Fatalf("Could not list IPv4 mock iptables rules, err: %q", err)
	}
	if !reflect.DeepEqual(out, expectedResult[iptables.ProtocolIPv4]) {
		log.Fatalf("Rules in AF IPv4, table filter, chain FORWARD do not match expected rules; got: %v; expected: %v",
			out, expectedResult[iptables.ProtocolIPv4])
	}

	out, err = iptV6.List("filter", "FORWARD")
	if err != nil {
		log.Fatalf("Could not list IPv6 mock iptables rules, err: %q", err)
	}
	if !reflect.DeepEqual(out, expectedResult[iptables.ProtocolIPv6]) {
		log.Fatalf("Rules in AF IPv6, table filter, chain FORWARD do not match expected rules; got: %v; expected: %v",
			out, expectedResult[iptables.ProtocolIPv6])
	}

	_, err = iptV4.List("filter", "OVN-KUBE-FORWARD-DROP")
	if err == nil || !strings.Contains(err.Error(), "chain OVN-KUBE-FORWARD-DROP does not exist") {
		log.Fatalf("Expected to get a does not exist error for IPv4 OVN-KUBE-FORWARD-DROP, instead got: %q", err)
	}

	_, err = iptV6.List("filter", "OVN-KUBE-FORWARD-DROP")
	if err == nil || !strings.Contains(err.Error(), "chain OVN-KUBE-FORWARD-DROP does not exist") {
		log.Fatalf("Expected to get a does not exist error for IPv6 OVN-KUBE-FORWARD-DROP, instead got: %q", err)
	}
}
