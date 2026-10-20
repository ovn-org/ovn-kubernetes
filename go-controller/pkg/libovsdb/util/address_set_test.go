// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package util

import (
	"testing"

	"github.com/onsi/gomega"

	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
)

func TestDeleteAddrSetsWithoutMatchRefAnyControllerKeepsQoSReferences(t *testing.T) {
	g := gomega.NewWithT(t)
	podSelectorAddressSet := func(name string) *nbdb.AddressSet {
		dbIDs := libovsdbops.NewDbObjectIDs(libovsdbops.AddressSetPodSelector, "controller", map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: name,
			libovsdbops.IPFamilyKey:   "ipv4",
		})
		return &nbdb.AddressSet{UUID: name + "-uuid", Name: name, ExternalIDs: dbIDs.GetExternalIDs()}
	}
	unreferenced, referencedFromSource, referencedFromDestination := podSelectorAddressSet("a1"), podSelectorAddressSet("a2"),
		podSelectorAddressSet("a3")
	nbClient, cleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{NBData: []libovsdbtest.TestData{
		&nbdb.LogicalSwitch{UUID: "switch-uuid", Name: "switch", QOSRules: []string{"qos-uuid"}},
		&nbdb.QoS{
			UUID:      "qos-uuid",
			Direction: nbdb.QoSDirectionToLport,
			Priority:  10000,
			Match:     "ip4.src == {$" + referencedFromSource.Name + "} && ip4.dst == {$" + referencedFromDestination.Name + "}",
		},
		unreferenced,
		referencedFromSource,
		referencedFromDestination,
	}}, nil)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	t.Cleanup(cleanup.Cleanup)

	g.Expect(DeleteAddrSetsWithoutMatchRefAnyController(libovsdbops.AddressSetPodSelector, nbClient)).To(gomega.Succeed())
	addrSets, err := libovsdbops.FindAddressSetsWithPredicate(nbClient, func(*nbdb.AddressSet) bool { return true })
	g.Expect(err).NotTo(gomega.HaveOccurred())
	names := []string{}
	for _, addrSet := range addrSets {
		names = append(names, addrSet.Name)
	}
	g.Expect(names).To(gomega.ConsistOf(referencedFromSource.Name, referencedFromDestination.Name))
}
