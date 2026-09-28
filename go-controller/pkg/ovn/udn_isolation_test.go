// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"context"
	"fmt"
	"strings"

	nettypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("UDN Isolation", func() {
	BeforeEach(func() {
		Expect(config.PrepareTestConfig()).To(Succeed())
	})

	It("ACLs should be updated to the Primary tier ", func() {
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		fakeController := getFakeController(types.DefaultNetworkControllerName)

		// build port group with one ACL that has default tier
		pgIDs := fakeController.getSecondaryPodsPortGroupDbIDs()
		pgName := libovsdbutil.GetPortGroupName(pgIDs)
		egressDenyIDs := fakeController.getUDNACLDbIDs(denyPrimaryUDNACL, libovsdbutil.ACLEgress)
		match := libovsdbutil.GetACLMatch(pgName, "", libovsdbutil.ACLEgress)
		// in the real code we use BuildACL here instead of BuildACLWithDefaultTier
		egressDenyACL := libovsdbutil.BuildACLWithDefaultTier(egressDenyIDs, types.PrimaryUDNDenyPriority, match, nbdb.ACLActionDrop,
			nil, libovsdbutil.LportEgress)
		// required to make sure port group correctly references the ACL
		egressDenyACL.UUID = egressDenyIDs.String() + "-UUID"
		pg := libovsdbutil.BuildPortGroup(pgIDs, nil, []*nbdb.ACL{egressDenyACL})

		nbClient, nbCleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{
			NBData: []libovsdbtest.TestData{egressDenyACL, pg},
		}, nil)
		Expect(err).NotTo(HaveOccurred())
		defer nbCleanup.Cleanup()
		fakeController.nbClient = nbClient

		// now run the setupUDNACLs function which should create all ACLs and update the existing ACLs to the Primary tier
		Expect(fakeController.setupUDNACLs(nil)).To(Succeed())

		// verify that the egressDenyACL is updated to the Primary 0
		acls, err := libovsdbops.FindACLs(nbClient, []*nbdb.ACL{egressDenyACL})
		Expect(err).NotTo(HaveOccurred())
		Expect(acls).To(HaveLen(1))
		Expect(acls[0].Tier).To(Equal(types.PrimaryACLTier))
	})

	It("Should handle syncing legacy DBIDs", func() {
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		fakeController := getFakeController(types.DefaultNetworkControllerName)

		By("initializing the database with all legacy ACLs")
		pgIDs := fakeController.getSecondaryPodsPortGroupDbIDs()
		pgName := libovsdbutil.GetPortGroupName(pgIDs)

		type legacyACLDef struct {
			oldName  string
			newName  string
			dir      libovsdbutil.ACLDirection
			priority int
			action   string
			applyDir libovsdbutil.ACLPipelineType
		}
		legacyACLDefs := []legacyACLDef{
			{denySecondaryACL, denyPrimaryUDNACL, libovsdbutil.ACLEgress, types.PrimaryUDNDenyPriority, nbdb.ACLActionDrop, libovsdbutil.LportEgress},
			{legacyAllowHostARPACL, allowHostARPACL, libovsdbutil.ACLEgress, types.PrimaryUDNAllowPriority, nbdb.ACLActionAllow, libovsdbutil.LportEgress},
			{denySecondaryACL, denyPrimaryUDNACL, libovsdbutil.ACLIngress, types.PrimaryUDNDenyPriority, nbdb.ACLActionDrop, libovsdbutil.LportIngress},
			{legacyAllowHostARPACL, allowHostARPACL, libovsdbutil.ACLIngress, types.PrimaryUDNAllowPriority, nbdb.ACLActionAllow, libovsdbutil.LportIngress},
			{allowHostSecondaryACL, allowHostPrimaryUDNACL, libovsdbutil.ACLIngress, types.PrimaryUDNAllowPriority, nbdb.ACLActionAllowRelated, libovsdbutil.LportIngress},
		}

		var legacyACLs []*nbdb.ACL
		nbData := []libovsdbtest.TestData{}
		for _, def := range legacyACLDefs {
			oldIDs := fakeController.getUDNACLDbIDs(def.oldName, def.dir)
			match := libovsdbutil.GetACLMatch(pgName, "", def.dir)
			acl := libovsdbutil.BuildACL(oldIDs, def.priority, match, def.action, nil, def.applyDir, isolationTier)
			acl.UUID = oldIDs.String() + "-UUID"
			legacyACLs = append(legacyACLs, acl)
			nbData = append(nbData, acl)
		}

		pg := libovsdbutil.BuildPortGroup(pgIDs, nil, legacyACLs)
		nbData = append(nbData, pg)

		nbClient, nbCleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{
			NBData: nbData,
		}, nil)
		Expect(err).NotTo(HaveOccurred())
		defer nbCleanup.Cleanup()
		fakeController.nbClient = nbClient
		By("running UDN Isolation sync to update ACLs")
		Expect(fakeController.syncUDNIsolation()).To(Succeed())
		By("expect updated port group with proper external_ids")
		pgs, err := libovsdbops.FindPortGroupsWithPredicate(nbClient, func(_ *nbdb.PortGroup) bool { return true })
		Expect(err).NotTo(HaveOccurred())
		Expect(pgs).To(HaveLen(1))
		Expect(pgs[0].ExternalIDs).To(Equal(fakeController.getSecondaryPodsPortGroupDbIDs().GetExternalIDs()))
		By("expect all ACLs updated with proper external_ids and names")
		acls, err := libovsdbops.FindACLsWithPredicate(nbClient, func(_ *nbdb.ACL) bool { return true })
		Expect(err).NotTo(HaveOccurred())
		Expect(acls).To(HaveLen(len(legacyACLDefs)))
		for _, def := range legacyACLDefs {
			newIDs := fakeController.getUDNACLDbIDs(def.newName, def.dir)
			expectedExtIDs := newIDs.GetExternalIDs()
			found := false
			for _, acl := range acls {
				if acl.ExternalIDs[libovsdbops.ObjectNameKey.String()] == expectedExtIDs[libovsdbops.ObjectNameKey.String()] &&
					acl.ExternalIDs[libovsdbops.PolicyDirectionKey.String()] == expectedExtIDs[libovsdbops.PolicyDirectionKey.String()] {
					Expect(acl.ExternalIDs).To(Equal(expectedExtIDs))
					Expect(*acl.Name).To(BeEmpty())
					found = true
					break
				}
			}
			Expect(found).To(BeTrue(), "expected ACL with name=%s dir=%s not found", def.newName, def.dir)
		}
	})

	Describe("ConfigureAdvertisedNetworkIsolation", func() {
		expectedDropACLMatch := func() string {
			v4HashName, v6HashName := addressset.GetHashNamesForAS(GetAdvertisedNetworkSubnetsAddressSetDBIDs())
			var matches []string
			if config.IPv4Mode {
				matches = append(matches, fmt.Sprintf("(ip4.src == $%s && ip4.dst == $%s)", v4HashName, v4HashName))
			}
			if config.IPv6Mode {
				matches = append(matches, fmt.Sprintf("(ip6.src == $%s && ip6.dst == $%s)", v6HashName, v6HashName))
			}
			return strings.Join(matches, " || ")
		}

		expectedAddrSets := func() []libovsdbtest.TestData {
			var data []libovsdbtest.TestData
			v4set, v6set := addressset.GetTestDbAddrSets(GetAdvertisedNetworkSubnetsAddressSetDBIDs(), nil)
			if config.IPv4Mode {
				data = append(data, v4set)
			}
			if config.IPv6Mode {
				data = append(data, v6set)
			}
			return data
		}

		It("creates the port group and drop ACL on fresh install", func() {
			nbClient, nbCleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{}, nil)
			Expect(err).NotTo(HaveOccurred())
			defer nbCleanup.Cleanup()

			Expect(ConfigureAdvertisedNetworkIsolation(nbClient)).To(Succeed())

			dropACL := libovsdbutil.BuildACL(GetAdvertisedNetworkSubnetsDropACLdbIDs(),
				types.AdvertisedNetworkDenyPriority, expectedDropACLMatch(),
				nbdb.ACLActionDrop, nil, libovsdbutil.LportEgressAfterLB, isolationTier)
			dropACL.UUID = "drop-acl-UUID"
			pg := libovsdbutil.BuildPortGroup(GetAdvertisedNetworkSubnetsDropPGdbIDs(), nil, []*nbdb.ACL{dropACL})
			pg.UUID = "drop-pg-UUID"
			expectedData := append([]libovsdbtest.TestData{dropACL, pg}, expectedAddrSets()...)
			Expect(nbClient).To(libovsdbtest.HaveData(expectedData))
		})

		It("migrates a single drop ACL from a switch to the port group", func() {
			dropACLdbIDs := GetAdvertisedNetworkSubnetsDropACLdbIDs()
			dropACL := libovsdbutil.BuildACL(dropACLdbIDs, types.AdvertisedNetworkDenyPriority,
				"(ip4.src == $fake && ip4.dst == $fake)", nbdb.ACLActionDrop, nil,
				libovsdbutil.LportEgressAfterLB, isolationTier)
			dropACL.UUID = "drop-acl-UUID"

			storLSP := &nbdb.LogicalSwitchPort{UUID: "stor-sw1-UUID", Name: types.SwitchToRouterPrefix + "sw1", Type: "router"}
			sw := &nbdb.LogicalSwitch{UUID: "sw1-UUID", Name: "sw1", Ports: []string{storLSP.UUID}, ACLs: []string{dropACL.UUID}}

			nbClient, nbCleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{dropACL, sw, storLSP},
			}, nil)
			Expect(err).NotTo(HaveOccurred())
			defer nbCleanup.Cleanup()

			Expect(ConfigureAdvertisedNetworkIsolation(nbClient)).To(Succeed())

			expectedDropACL := libovsdbutil.BuildACL(dropACLdbIDs,
				types.AdvertisedNetworkDenyPriority, expectedDropACLMatch(),
				nbdb.ACLActionDrop, nil, libovsdbutil.LportEgressAfterLB, isolationTier)
			expectedDropACL.UUID = "drop-acl-UUID"
			expectedPG := libovsdbutil.BuildPortGroup(GetAdvertisedNetworkSubnetsDropPGdbIDs(), nil, []*nbdb.ACL{expectedDropACL})
			expectedPG.UUID = "drop-pg-UUID"
			expectedPG.Ports = []string{storLSP.UUID}
			expectedData := append([]libovsdbtest.TestData{
				expectedDropACL, expectedPG,
				&nbdb.LogicalSwitch{UUID: "sw1-UUID", Name: "sw1", Ports: []string{storLSP.UUID}},
				storLSP,
			}, expectedAddrSets()...)
			Expect(nbClient).To(libovsdbtest.HaveData(expectedData))
		})

		It("migrates duplicate drop ACLs from switches to the port group", func() {
			dropACLdbIDs := GetAdvertisedNetworkSubnetsDropACLdbIDs()
			dropACL1 := libovsdbutil.BuildACL(dropACLdbIDs, types.AdvertisedNetworkDenyPriority,
				"(ip4.src == $fake && ip4.dst == $fake)", nbdb.ACLActionDrop, nil,
				libovsdbutil.LportEgressAfterLB, isolationTier)
			dropACL1.UUID = "drop-acl-1-UUID"
			dropACL2 := libovsdbutil.BuildACL(dropACLdbIDs, types.AdvertisedNetworkDenyPriority,
				"(ip4.src == $fake && ip4.dst == $fake)", nbdb.ACLActionDrop, nil,
				libovsdbutil.LportEgressAfterLB, isolationTier)
			dropACL2.UUID = "drop-acl-2-UUID"

			sw1LSP := &nbdb.LogicalSwitchPort{UUID: "stor-sw1-UUID", Name: types.SwitchToRouterPrefix + "sw1", Type: "router"}
			sw1 := &nbdb.LogicalSwitch{UUID: "sw1-UUID", Name: "sw1", Ports: []string{sw1LSP.UUID}, ACLs: []string{dropACL1.UUID}}
			// emulate Layer 2 network
			sw2Name := "sw2_" + types.OVNLayer2Switch
			sw2LSP := &nbdb.LogicalSwitchPort{UUID: "stor-sw2-UUID", Name: types.SwitchToTransitRouterPrefix + sw2Name, Type: "router"}
			sw2 := &nbdb.LogicalSwitch{UUID: "sw2-UUID", Name: sw2Name, Ports: []string{sw2LSP.UUID}, ACLs: []string{dropACL2.UUID}}

			nbClient, nbCleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{dropACL1, dropACL2, sw1, sw2, sw1LSP, sw2LSP},
			}, nil)
			Expect(err).NotTo(HaveOccurred())
			defer nbCleanup.Cleanup()

			Expect(ConfigureAdvertisedNetworkIsolation(nbClient)).To(Succeed())

			expectedDropACL := libovsdbutil.BuildACL(dropACLdbIDs,
				types.AdvertisedNetworkDenyPriority, expectedDropACLMatch(),
				nbdb.ACLActionDrop, nil, libovsdbutil.LportEgressAfterLB, isolationTier)
			expectedDropACL.UUID = "drop-acl-1-UUID"
			expectedPG := libovsdbutil.BuildPortGroup(GetAdvertisedNetworkSubnetsDropPGdbIDs(), nil, []*nbdb.ACL{expectedDropACL})
			expectedPG.UUID = "drop-pg-UUID"
			expectedPG.Ports = []string{sw1LSP.UUID, sw2LSP.UUID}
			sw1.ACLs = nil
			sw2.ACLs = nil
			expectedData := append([]libovsdbtest.TestData{
				expectedDropACL, expectedPG,
				sw1, sw2,
				sw1LSP, sw2LSP,
			}, expectedAddrSets()...)
			Expect(nbClient).To(libovsdbtest.HaveData(expectedData))
		})

		It("skips migration but self-heals the drop ACL when the port group already exists", func() {
			dropACLdbIDs := GetAdvertisedNetworkSubnetsDropACLdbIDs()
			dropACL := libovsdbutil.BuildACL(dropACLdbIDs, types.AdvertisedNetworkDenyPriority,
				"(ip4.src == $fake && ip4.dst == $fake)", nbdb.ACLActionDrop, nil,
				libovsdbutil.LportEgressAfterLB, isolationTier)
			dropACL.UUID = "drop-acl-UUID"
			storLSP := &nbdb.LogicalSwitchPort{UUID: "stor-sw1-UUID", Name: types.SwitchToRouterPrefix + "sw1", Type: "router"}
			sw := &nbdb.LogicalSwitch{UUID: "sw1-UUID", Name: "sw1", Ports: []string{storLSP.UUID}}
			pg := libovsdbutil.BuildPortGroup(GetAdvertisedNetworkSubnetsDropPGdbIDs(), nil, []*nbdb.ACL{dropACL})
			pg.UUID = "drop-pg-UUID"
			pg.Ports = []string{storLSP.UUID}

			nbClient, nbCleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{dropACL, pg, sw, storLSP},
			}, nil)
			Expect(err).NotTo(HaveOccurred())
			defer nbCleanup.Cleanup()

			Expect(ConfigureAdvertisedNetworkIsolation(nbClient)).To(Succeed())

			expectedDropACL := libovsdbutil.BuildACL(dropACLdbIDs,
				types.AdvertisedNetworkDenyPriority, expectedDropACLMatch(),
				nbdb.ACLActionDrop, nil, libovsdbutil.LportEgressAfterLB, isolationTier)
			expectedDropACL.UUID = "drop-acl-UUID"
			expectedPG := libovsdbutil.BuildPortGroup(GetAdvertisedNetworkSubnetsDropPGdbIDs(), nil, []*nbdb.ACL{expectedDropACL})
			expectedPG.UUID = "drop-pg-UUID"
			expectedPG.Ports = []string{storLSP.UUID}
			expectedData := append([]libovsdbtest.TestData{expectedDropACL, expectedPG, sw, storLSP}, expectedAddrSets()...)
			Expect(nbClient).To(libovsdbtest.HaveData(expectedData))
		})
	})

	It("queues advertised local nodes when a primary Layer3 UDN adds a subnet", func() {
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		config.OVNKubernetesFeature.EnableDynamicUDNAllocation = true
		config.OVNKubernetesFeature.AdvertisedUDNIsolationMode = config.AdvertisedUDNIsolationModeStrict
		config.IPv4Mode = true
		config.IPv6Mode = false

		fakeOVN := NewFakeOVN(true, nodeName)

		netInfoBefore := dummyPrimaryLayer3UserDefinedNetwork("192.168.0.0/16", "192.168.1.0/24")
		nadBefore, err := newNetworkAttachmentDefinition(ns, nadName, *netInfoBefore.netconf())
		Expect(err).NotTo(HaveOccurred())
		node, err := newNodeWithUserDefinedNetworks(nodeName, "192.168.126.202/24", netInfoBefore)
		Expect(err).NotTo(HaveOccurred())

		fakeOVN.startWithDBSetup(
			libovsdbtest.TestSetup{},
			node,
			&nettypes.NetworkAttachmentDefinitionList{Items: []nettypes.NetworkAttachmentDefinition{*nadBefore}},
		)
		defer fakeOVN.shutdown()

		l3Controller, ok := fakeOVN.fullL3UDNControllers[netInfoBefore.netName]
		Expect(ok).To(BeTrue())
		mutableNetInfo := util.NewMutableNetInfo(l3Controller.GetNetInfo())
		mutableNetInfo.SetNetworkID(2)
		mutableNetInfo.SetPodNetworkAdvertisedVRFs(map[string][]string{nodeName: {"vrf"}})
		Expect(util.ReconcileNetInfo(l3Controller.ReconcilableNetInfo, mutableNetInfo)).To(Succeed())
		l3Controller.lastNADKeys = sets.New[string](ns + "/" + nadName)

		netInfoAfter := dummyPrimaryLayer3UserDefinedNetwork("192.168.0.0/16,192.169.0.0/16", "192.168.1.0/24")
		nadAfter, err := newNetworkAttachmentDefinition(ns, nadName, *netInfoAfter.netconf())
		Expect(err).NotTo(HaveOccurred())
		parsedNetInfoAfter, err := util.ParseNADInfo(nadAfter)
		Expect(err).NotTo(HaveOccurred())
		mutableNetInfoAfter := util.NewMutableNetInfo(parsedNetInfoAfter)
		mutableNetInfoAfter.SetNetworkID(2)
		mutableNetInfoAfter.SetPodNetworkAdvertisedVRFs(map[string][]string{nodeName: {"vrf"}})

		reconciledNodes := sets.New[string]()
		Expect(l3Controller.reconcile(context.Background(), mutableNetInfoAfter, func(node string) {
			reconciledNodes.Insert(node)
		})).To(Succeed())
		Expect(reconciledNodes.Has(nodeName)).To(BeTrue())
	})

	It("updates advertised network isolation ACLs when a primary Layer3 UDN adds a subnet", func() {
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		config.OVNKubernetesFeature.EnableDynamicUDNAllocation = true
		config.OVNKubernetesFeature.AdvertisedUDNIsolationMode = config.AdvertisedUDNIsolationModeStrict
		config.IPv4Mode = true
		config.IPv6Mode = false

		fakeOVN := NewFakeOVN(false, nodeName)

		netInfoBefore := dummyPrimaryLayer3UserDefinedNetwork("192.168.0.0/16", "192.168.1.0/24")
		nadBefore, err := newNetworkAttachmentDefinition(ns, nadName, *netInfoBefore.netconf())
		Expect(err).NotTo(HaveOccurred())
		node, err := newNodeWithUserDefinedNetworks(nodeName, "192.168.126.202/24", netInfoBefore)
		Expect(err).NotTo(HaveOccurred())

		fakeOVN.startWithDBSetup(
			libovsdbtest.TestSetup{},
			node,
			&nettypes.NetworkAttachmentDefinitionList{Items: []nettypes.NetworkAttachmentDefinition{*nadBefore}},
		)
		defer fakeOVN.shutdown()

		l3Controller, ok := fakeOVN.fullL3UDNControllers[netInfoBefore.netName]
		Expect(ok).To(BeTrue())
		mutableNetInfo := util.NewMutableNetInfo(l3Controller.GetNetInfo())
		mutableNetInfo.SetNetworkID(2)
		mutableNetInfo.SetPodNetworkAdvertisedVRFs(map[string][]string{nodeName: {"vrf"}})
		Expect(util.ReconcileNetInfo(l3Controller.ReconcilableNetInfo, mutableNetInfo)).To(Succeed())

		Expect(ConfigureAdvertisedNetworkIsolation(fakeOVN.nbClient)).To(Succeed())
		switchName := l3Controller.GetNetworkScopedSwitchName(nodeName)
		Expect(libovsdbops.CreateOrUpdateLogicalSwitch(fakeOVN.nbClient, &nbdb.LogicalSwitch{
			Name: switchName,
		})).To(Succeed())
		Expect(libovsdbops.CreateOrUpdateLogicalSwitchPortsOnSwitch(fakeOVN.nbClient,
			&nbdb.LogicalSwitch{Name: switchName},
			&nbdb.LogicalSwitchPort{Name: types.SwitchToRouterPrefix + switchName, Type: "router"},
		)).To(Succeed())

		Expect(l3Controller.addAdvertisedNetworkIsolation(nodeName)).To(Succeed())
		getPassACL := func() *nbdb.ACL {
			acls, err := libovsdbops.FindACLsWithPredicate(fakeOVN.nbClient, func(acl *nbdb.ACL) bool {
				return acl.Action == nbdb.ACLActionPass &&
					acl.ExternalIDs[libovsdbops.ObjectNameKey.String()] == netInfoBefore.netName
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(acls).To(HaveLen(1))
			return acls[0]
		}
		Expect(getPassACL().Match).To(ContainSubstring("192.168.0.0/16"))
		Expect(getPassACL().Match).NotTo(ContainSubstring("192.169.0.0/16"))

		netInfoAfter := dummyPrimaryLayer3UserDefinedNetwork("192.168.0.0/16,192.169.0.0/16", "192.168.1.0/24")
		nadAfter, err := newNetworkAttachmentDefinition(ns, nadName, *netInfoAfter.netconf())
		Expect(err).NotTo(HaveOccurred())
		parsedNetInfoAfter, err := util.ParseNADInfo(nadAfter)
		Expect(err).NotTo(HaveOccurred())
		mutableNetInfoAfter := util.NewMutableNetInfo(parsedNetInfoAfter)
		mutableNetInfoAfter.SetNetworkID(2)
		mutableNetInfoAfter.SetPodNetworkAdvertisedVRFs(map[string][]string{nodeName: {"vrf"}})
		Expect(util.ReconcileNetInfo(l3Controller.ReconcilableNetInfo, mutableNetInfoAfter)).To(Succeed())

		Expect(l3Controller.addAdvertisedNetworkIsolation(nodeName)).To(Succeed())
		passACLMatch := getPassACL().Match
		Expect(passACLMatch).To(ContainSubstring("192.168.0.0/16"))
		Expect(passACLMatch).To(ContainSubstring("192.169.0.0/16"))
		Expect(strings.Count(passACLMatch, "ip4.src ==")).To(Equal(2))
		Expect(strings.Count(passACLMatch, "ip4.dst ==")).To(Equal(2))
	})
})
