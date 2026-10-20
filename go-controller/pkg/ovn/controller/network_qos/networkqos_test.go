// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package networkqos

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	metaapplyv1 "k8s.io/client-go/applyconfigurations/meta/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/ptr"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	nqosapi "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1alpha1"
	nqosapiapply "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1alpha1/apis/applyconfiguration/networkqos/v1alpha1"
	nqosfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1alpha1/apis/clientset/versioned/fake"
	networkqoslister "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1alpha1/apis/listers/networkqos/v1alpha1"
	crdtypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/types"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/addresssetmanager"
	ovnk8stesting "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("NetworkQoS rules", func() {
	It("applies the rules of a localnet network as packets leave the switch", func() {
		nad := ovnk8stesting.GenerateNAD(streamNetwork, streamNetwork, "default", types.LocalnetTopology,
			"10.128.0.0/16", types.NetworkRoleSecondary)
		nad.Labels = map[string]string{"name": streamNetwork}
		info, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())
		netInfo := util.NewMutableNetInfo(info)
		netInfo.AddNADs(streamNADKey)
		nctx := &networkContext{netInfo: netInfo, controllerName: streamControllerName}

		remote := testPod()
		remote.Name = "remote-client"
		remote.Spec.NodeName = "node2"
		remote.Annotations[types.OvnPodAnnotationName] = fmt.Sprintf(
			`{%q:{"ip_addresses":["10.128.3.3/16"],"mac_address":"0a:58:0a:80:03:03"}}`, streamNADKey)
		db := initialDB()
		db.NBData = append(db.NBData, &nbdb.LogicalSwitch{Name: nctx.logicalSwitchName(testNode)})
		nm := startController(db, nad, testNamespaceObj(), testPod(), remote)
		nm.Lock()
		nm.NADNetworks[streamNADKey] = netInfo
		nm.Unlock()

		nqos := newNetworkQoS("qos-direction", crdtypes.NetworkSelectors{nadSelector(nad.Labels)})
		_, err = fakeNQoSClient.K8sV1alpha1().NetworkQoSes(testNamespace).Create(context.Background(), nqos, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		qos := eventuallyQoS(streamControllerName, testNamespace, nqos.Name, 0)
		eventuallySwitchHasQoS(nctx.logicalSwitchName(testNode), qos)
		Expect(qos.Direction).To(Equal(nbdb.QoSDirectionToLport))
		// Only the packets of local pods are classified here.
		Eventually(func() []string { return sourceAddresses(streamControllerName) }).Should(ConsistOf(testPodStreamIP))
		sets := findSourceAddressSets(streamControllerName)
		Expect(sets).To(HaveLen(1))
		Expect(qos.Match).To(Equal("ip4.src == {$" + sets[0].Name + "}"))
	})

	// Packets to a Service enter a layer2 switch twice on their source node: addressed to a
	// router, and once load balanced, from that router.
	It("skips the packets addressed to the routers of a layer2 switch for a destination on the network", func() {
		nad := ovnk8stesting.GenerateNAD(streamNetwork, streamNetwork, "default", types.Layer2Topology,
			"10.128.0.0/16", types.NetworkRolePrimary)
		nad.Labels = map[string]string{"name": streamNetwork}
		info, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())
		netInfo := util.NewMutableNetInfo(info)
		netInfo.AddNADs(streamNADKey)
		layer2Switch := &nbdb.LogicalSwitch{Name: (&networkContext{netInfo: netInfo}).logicalSwitchName(testNode)}
		router := &nbdb.LogicalRouter{Name: "transit-router"}
		db := initialDB()
		db.NBData = append(db.NBData,
			&nbdb.LogicalRouterPort{UUID: "trtos-uuid", Name: "trtos", MAC: "0a:58:0a:80:00:01", Networks: []string{"10.128.0.1/16"}},
			&nbdb.LogicalRouter{Name: router.Name, Ports: []string{"trtos-uuid"}},
			&nbdb.LogicalSwitchPort{UUID: "stotr-uuid", Name: "stotr", Type: "router",
				Options: map[string]string{libovsdbops.RouterPort: "trtos"}},
			&nbdb.LogicalSwitch{UUID: "layer2-switch-uuid", Name: layer2Switch.Name, Ports: []string{"stotr-uuid"}},
		)
		nm := startController(db, nad, testNamespaceObj(), testPod())
		nm.Lock()
		nm.NADNetworks[streamNADKey] = netInfo
		nm.Unlock()
		nqos := newNetworkQoS("qos-router-reentry", crdtypes.NetworkSelectors{nadSelector(nad.Labels)})
		// A rule without a bandwidth limit skips the same pass, so that rules of lower priority
		// cannot override the DSCP on the other pass.
		nqos.Spec.Egress = append(nqos.Spec.Egress, nqosapi.Rule{DSCP: 20})
		_, err = fakeNQoSClient.K8sV1alpha1().NetworkQoSes(testNamespace).Create(context.Background(), nqos, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		ruleMatch := func(index int) func() string {
			return func() string {
				if qos := findQoS(streamControllerName, testNamespace, nqos.Name, index); qos != nil {
					return qos.Match
				}
				return ""
			}
		}
		match := ruleMatch(0)
		Eventually(match).Should(HaveSuffix(" && !(eth.dst == 0a:58:0a:80:00:01 && ip4.dst == 10.128.0.0/16)"))
		sets := findSourceAddressSets(streamControllerName)
		Expect(sets).To(HaveLen(1))
		Expect(match()).To(HavePrefix("ip4.src == {$" + sets[0].Name + "} && "))
		Expect(findQoS(streamControllerName, testNamespace, nqos.Name, 0).Direction).To(Equal(nbdb.QoSDirectionFromLport))
		Eventually(ruleMatch(1)).Should(Equal("ip4.src == {$" + sets[0].Name + "} && !(eth.dst == 0a:58:0a:80:00:01 && ip4.dst == 10.128.0.0/16)"))
		// The rules apply on the last pass, so the skipped one needs no row.
		Expect(findReturnPassQoS(streamControllerName, testNamespace, nqos.Name, 0)).To(BeNil())

		By("adding the router port of a topology upgrade")
		Expect(libovsdbops.CreateOrUpdateLogicalRouterPort(nbClient, router,
			&nbdb.LogicalRouterPort{Name: "trtos-upgrade", MAC: "0a:58:64:41:00:02", Networks: []string{"100.65.0.2/16"}}, nil)).To(Succeed())
		Expect(libovsdbops.CreateOrUpdateLogicalSwitchPortsOnSwitch(nbClient, layer2Switch, &nbdb.LogicalSwitchPort{
			Name: "stotr-upgrade", Type: "router", Options: map[string]string{libovsdbops.RouterPort: "trtos-upgrade"}})).To(Succeed())
		Eventually(match).Should(HaveSuffix(" && !(eth.dst == {0a:58:0a:80:00:01, 0a:58:64:41:00:02} && ip4.dst == 10.128.0.0/16)"))

		By("removing it once the upgrade is over")
		Expect(libovsdbops.DeleteLogicalSwitchPorts(nbClient, layer2Switch, &nbdb.LogicalSwitchPort{Name: "stotr-upgrade"})).To(Succeed())
		Eventually(match).Should(HaveSuffix(" && !(eth.dst == 0a:58:0a:80:00:01 && ip4.dst == 10.128.0.0/16)"))

		By("adding a router port whose peer is created later")
		Expect(libovsdbops.CreateOrUpdateLogicalSwitchPortsOnSwitch(nbClient, layer2Switch, &nbdb.LogicalSwitchPort{
			Name: "stotr-late", Type: "router", Options: map[string]string{libovsdbops.RouterPort: "trtos-late"}})).To(Succeed())
		Consistently(match).WithTimeout(time.Second).Should(HaveSuffix(" && !(eth.dst == 0a:58:0a:80:00:01 && ip4.dst == 10.128.0.0/16)"))
		late := &nbdb.LogicalRouterPort{Name: "trtos-late", MAC: "0a:58:64:41:00:03", Networks: []string{"100.65.0.3/16"}}
		Expect(libovsdbops.CreateOrUpdateLogicalRouterPort(nbClient, router, late, nil)).To(Succeed())
		Eventually(match).Should(HaveSuffix(" && !(eth.dst == {0a:58:0a:80:00:01, 0a:58:64:41:00:03} && ip4.dst == 10.128.0.0/16)"))

		By("following a change of its MAC")
		late.MAC = "0a:58:64:41:00:04"
		Expect(libovsdbops.CreateOrUpdateLogicalRouterPort(nbClient, router, late, nil, &late.MAC)).To(Succeed())
		Eventually(match).Should(HaveSuffix(" && !(eth.dst == {0a:58:0a:80:00:01, 0a:58:64:41:00:04} && ip4.dst == 10.128.0.0/16)"))
		Eventually(ruleMatch(1)).Should(HaveSuffix(" && !(eth.dst == {0a:58:0a:80:00:01, 0a:58:64:41:00:04} && ip4.dst == 10.128.0.0/16)"))
	})

	// A packet to a Service whose selected pod is on the same node leaves the node switch
	// towards the router, then towards the pod.
	It("sets only the DSCP of the packets routers send into the switch where rules apply as packets leave it", func() {
		router := &nbdb.LogicalRouter{Name: "cluster-router"}
		nodeSwitch := &nbdb.LogicalSwitch{Name: testNode}
		db := &libovsdbtest.TestSetup{NBData: []libovsdbtest.TestData{
			&nbdb.LogicalRouterPort{UUID: "rtos-uuid", Name: "rtos-" + testNode, MAC: "0a:58:0a:c0:b1:01", Networks: []string{"10.192.177.1/26"}},
			&nbdb.LogicalRouter{Name: router.Name, Ports: []string{"rtos-uuid"}},
			&nbdb.LogicalSwitchPort{UUID: "stor-uuid", Name: "stor-" + testNode, Type: "router",
				Options: map[string]string{libovsdbops.RouterPort: "rtos-" + testNode}},
			&nbdb.LogicalSwitch{UUID: "node-switch-uuid", Name: testNode, Ports: []string{"stor-uuid"}},
		}}
		nqos := newNetworkQoS("qos-node-switch-router", nil)
		// A rule without a bandwidth limit skips the same pass, so that rules of lower priority
		// cannot override the DSCP on the other pass.
		nqos.Spec.Egress = append(nqos.Spec.Egress, nqosapi.Rule{DSCP: 20})
		startController(db, testNamespaceObj(), testPod(), nqos)
		metered := eventuallyQoS(defaultControllerNam, testNamespace, nqos.Name, 0)
		eventuallySwitchHasQoS(testNode, metered)
		Expect(metered.Direction).To(Equal(nbdb.QoSDirectionToLport))
		Expect(metered.Match).To(HaveSuffix(" && !(eth.src == 0a:58:0a:c0:b1:01)"))
		ruleMatch := func(index int) func() string {
			return func() string {
				if qos := findQoS(defaultControllerNam, testNamespace, nqos.Name, index); qos != nil {
					return qos.Match
				}
				return ""
			}
		}
		Eventually(ruleMatch(1)).Should(MatchRegexp(`^ip4\.src == \{\$\w+\} && !\(eth\.src == 0a:58:0a:c0:b1:01\)$`))
		returnPassMatch := func(index int) func() string {
			return func() string {
				if qos := findReturnPassQoS(defaultControllerNam, testNamespace, nqos.Name, index); qos != nil {
					return qos.Match
				}
				return ""
			}
		}
		Eventually(returnPassMatch(0)).Should(MatchRegexp(`^ip4\.src == \{\$\w+\} && eth\.src == 0a:58:0a:c0:b1:01$`))
		Eventually(returnPassMatch(1)).Should(MatchRegexp(`^ip4\.src == \{\$\w+\} && eth\.src == 0a:58:0a:c0:b1:01$`))

		By("following another router port")
		Expect(libovsdbops.CreateOrUpdateLogicalRouterPort(nbClient, router,
			&nbdb.LogicalRouterPort{Name: "rtos-other", MAC: "0a:58:64:40:00:02", Networks: []string{"100.64.0.2/16"}}, nil)).To(Succeed())
		Expect(libovsdbops.CreateOrUpdateLogicalSwitchPortsOnSwitch(nbClient, nodeSwitch, &nbdb.LogicalSwitchPort{
			Name: "stor-other", Type: "router", Options: map[string]string{libovsdbops.RouterPort: "rtos-other"}})).To(Succeed())
		Eventually(ruleMatch(0)).Should(HaveSuffix(" && !(eth.src == {0a:58:0a:c0:b1:01, 0a:58:64:40:00:02})"))
		Eventually(ruleMatch(1)).Should(HaveSuffix(" && !(eth.src == {0a:58:0a:c0:b1:01, 0a:58:64:40:00:02})"))
		Eventually(returnPassMatch(0)).Should(HaveSuffix(" && eth.src == {0a:58:0a:c0:b1:01, 0a:58:64:40:00:02}"))
		Eventually(returnPassMatch(1)).Should(HaveSuffix(" && eth.src == {0a:58:0a:c0:b1:01, 0a:58:64:40:00:02}"))

		By("removing it")
		Expect(libovsdbops.DeleteLogicalSwitchPorts(nbClient, nodeSwitch, &nbdb.LogicalSwitchPort{Name: "stor-other"})).To(Succeed())
		Eventually(ruleMatch(0)).Should(HaveSuffix(" && !(eth.src == 0a:58:0a:c0:b1:01)"))
		Eventually(ruleMatch(1)).Should(HaveSuffix(" && !(eth.src == 0a:58:0a:c0:b1:01)"))
		Eventually(returnPassMatch(0)).Should(HaveSuffix(" && eth.src == 0a:58:0a:c0:b1:01"))
		Eventually(returnPassMatch(1)).Should(HaveSuffix(" && eth.src == 0a:58:0a:c0:b1:01"))
	})

	// On the default network, EgressQoS rules apply as packets leave the switch too, with lower
	// priorities, and match the packets routers send into it.
	It("keeps EgressQoS from overriding the DSCP of a rule on the packets routers send into the switch", func() {
		egressQoS := &nbdb.QoS{
			UUID:      "egressqos-uuid",
			Direction: nbdb.QoSDirectionToLport,
			Priority:  1000,
			Match:     "(ip4.dst == 0.0.0.0/0 || ip6.dst == ::/0) && ip4.src == $a5678",
			Action:    map[string]int{nbdb.QoSActionDSCP: 10},
			ExternalIDs: libovsdbops.NewDbObjectIDs(libovsdbops.QoSEgressQoS, defaultControllerNam,
				map[libovsdbops.ExternalIDKey]string{
					libovsdbops.ObjectNameKey: testNamespace,
					libovsdbops.PriorityKey:   "1000",
				}).GetExternalIDs(),
		}
		db := &libovsdbtest.TestSetup{NBData: []libovsdbtest.TestData{
			&nbdb.LogicalRouterPort{UUID: "rtos-uuid", Name: "rtos-" + testNode, MAC: "0a:58:0a:c0:b1:01", Networks: []string{"10.192.177.1/26"}},
			&nbdb.LogicalRouterPort{UUID: "stream-rtos-uuid", Name: "stream-rtos-" + testNode, MAC: "0a:58:0a:80:02:01", Networks: []string{"10.128.2.1/24"}},
			&nbdb.LogicalRouter{Name: "cluster-router", Ports: []string{"rtos-uuid"}},
			&nbdb.LogicalRouter{Name: "stream-cluster-router", Ports: []string{"stream-rtos-uuid"}},
			&nbdb.LogicalSwitchPort{UUID: "stor-uuid", Name: "stor-" + testNode, Type: "router",
				Options: map[string]string{libovsdbops.RouterPort: "rtos-" + testNode}},
			&nbdb.LogicalSwitchPort{UUID: "stream-stor-uuid", Name: "stream-stor-" + testNode, Type: "router",
				Options: map[string]string{libovsdbops.RouterPort: "stream-rtos-" + testNode}},
			&nbdb.LogicalSwitch{UUID: "node-switch-uuid", Name: testNode, Ports: []string{"stor-uuid"},
				QOSRules: []string{egressQoS.UUID}},
			&nbdb.LogicalSwitch{UUID: "stream-switch-uuid", Name: streamNetwork + "_" + testNode, Ports: []string{"stream-stor-uuid"}},
			egressQoS,
		}}
		selected := map[string]string{"qos": "enabled"}
		nqos := newNetworkQoS("qos-over-egressqos", []crdtypes.NetworkSelector{nadSelector(selected)})
		nqos.Spec.Egress = append(nqos.Spec.Egress, nqosapi.Rule{DSCP: 20})
		startController(db, defaultNetworkNAD(selected), streamNAD(selected), testNamespaceObj(), testPod(), nqos)

		By("setting only the DSCP of each rule on that pass, with a higher priority than EgressQoS")
		metered := eventuallyQoS(defaultControllerNam, testNamespace, nqos.Name, 0)
		Expect(metered.Match).To(HaveSuffix(" && !(eth.src == 0a:58:0a:c0:b1:01)"))
		// Both rows of a rule are written in one transaction.
		returnPass := findReturnPassQoS(defaultControllerNam, testNamespace, nqos.Name, 0)
		Expect(returnPass).NotTo(BeNil())
		eventuallySwitchHasQoS(testNode, returnPass)
		Expect(returnPass.ExternalIDs).To(HaveKeyWithValue(libovsdbops.RuleIndex.String(), "0-return"))
		Expect(returnPass.Direction).To(Equal(nbdb.QoSDirectionToLport))
		Expect(returnPass.Match).To(Equal(strings.TrimSuffix(metered.Match, "!(eth.src == 0a:58:0a:c0:b1:01)") +
			"eth.src == 0a:58:0a:c0:b1:01"))
		Expect(returnPass.Action).To(Equal(map[string]int{nbdb.QoSActionDSCP: 50}))
		Expect(returnPass.Bandwidth).To(BeEmpty())
		Expect(returnPass.Priority).To(Equal(metered.Priority))
		Expect(returnPass.Priority).To(BeNumerically(">", egressQoS.Priority))
		Expect(findReturnPassQoS(defaultControllerNam, testNamespace, nqos.Name, 1)).To(
			HaveField("Action", Equal(map[string]int{nbdb.QoSActionDSCP: 20})))

		By("adding no such row on other networks, where no other rule sets the DSCP")
		streamQoS := eventuallyQoS(streamControllerName, testNamespace, nqos.Name, 0)
		Expect(streamQoS.Match).To(HaveSuffix(" && !(eth.src == 0a:58:0a:80:02:01)"))
		Expect(findReturnPassQoS(streamControllerName, testNamespace, nqos.Name, 0)).To(BeNil())

		By("removing the row of a removed rule")
		updated, err := fakeNQoSClient.K8sV1alpha1().NetworkQoSes(testNamespace).Get(context.Background(), nqos.Name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		updated.Spec.Egress = updated.Spec.Egress[:1]
		updated.Generation++
		_, err = fakeNQoSClient.K8sV1alpha1().NetworkQoSes(testNamespace).Update(context.Background(), updated, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
		eventuallyNoQoS(defaultControllerNam, testNamespace, nqos.Name, 1)
		Expect(findReturnPassQoS(defaultControllerNam, testNamespace, nqos.Name, 1)).To(BeNil())
		Expect(findReturnPassQoS(defaultControllerNam, testNamespace, nqos.Name, 0)).To(HaveField("UUID", returnPass.UUID))

		By("removing every row with the NetworkQoS and keeping the EgressQoS rule")
		Expect(fakeNQoSClient.K8sV1alpha1().NetworkQoSes(testNamespace).Delete(context.Background(), nqos.Name,
			metav1.DeleteOptions{})).To(Succeed())
		eventuallyNoQoS(defaultControllerNam, testNamespace, nqos.Name, 0)
		Expect(findReturnPassQoS(defaultControllerNam, testNamespace, nqos.Name, 0)).To(BeNil())
		egressQoSes, err := libovsdbops.FindQoSesWithPredicate(nbClient, func(qos *nbdb.QoS) bool {
			return qos.ExternalIDs[libovsdbops.OwnerTypeKey.String()] == string(libovsdbops.EgressQoSOwnerType)
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(egressQoSes).To(HaveLen(1))
		eventuallySwitchHasQoS(testNode, egressQoSes[0])
	})

	It("canonicalizes CIDRs in OVN matches without modifying API objects", func() {
		input := networkingv1.IPBlock{CIDR: "10.0.0.7/24", Except: []string{"10.0.0.129/25", "10.0.0.19/32"}}
		nqos := newNetworkQoS("qos-canonical-cidr", nil)
		nqos.Spec.Egress[0].Classifier.To = []nqosapi.Destination{{IPBlock: input.DeepCopy()}}
		startController(initialDB(), testNamespaceObj(), testPod(), nqos)
		qos := eventuallyQoS(defaultControllerNam, testNamespace, nqos.Name, 0)
		eventuallySwitchHasQoS(testNode, qos)
		Expect(qos.Match).To(HaveSuffix(" && ip4.dst == 10.0.0.0/24 && ip4.dst != {10.0.0.128/25,10.0.0.19/32}"))

		By("keeping both the API object and the informer cache unchanged")
		stored, err := fakeNQoSClient.K8sV1alpha1().NetworkQoSes(testNamespace).Get(context.Background(), nqos.Name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(stored.Spec.Egress[0].Classifier.To[0].IPBlock).To(Equal(&input))
		cached, err := nqosController.nqosLister.NetworkQoSes(testNamespace).Get(nqos.Name)
		Expect(err).NotTo(HaveOccurred())
		Expect(cached.Spec.Egress[0].Classifier.To[0].IPBlock).To(Equal(&input))

		By("keeping the match stable when equivalent canonical input is submitted")
		stored.Spec.Egress[0].Classifier.To[0].IPBlock = &networkingv1.IPBlock{
			CIDR: "10.0.0.0/24", Except: []string{"10.0.0.128/25", "10.0.0.19/32"}}
		stored.Spec.Egress[0].DSCP = 7
		stored.Generation++
		_, err = fakeNQoSClient.K8sV1alpha1().NetworkQoSes(testNamespace).Update(context.Background(), stored, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() *nbdb.QoS { return findQoS(defaultControllerNam, testNamespace, nqos.Name, 0) }).Should(SatisfyAll(
			HaveField("UUID", qos.UUID),
			HaveField("Match", qos.Match),
			HaveField("Action", Equal(map[string]int{nbdb.QoSActionDSCP: 7})),
		))
	})

	It("does not write to the northbound database when nothing changed", func() {
		startController(initialDB(), testNamespaceObj(), testPod())
		nqos := newNetworkQoS("qos-unchanged", nil)
		nqos.Spec.Egress[0].Classifier.To = []nqosapi.Destination{{PodSelector: &metav1.LabelSelector{}}}
		client := &countingClient{Client: nbClient}
		c := newDirectController(client)
		nctx := c.newNetworkContext(&util.DefaultNetInfo{})
		state, err := c.ensureNetworkQoSForNetwork(nqos, nctx, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(client.transactions.Load()).NotTo(BeZero())

		client.transactions.Store(0)
		_, err = c.ensureNetworkQoSForNetwork(nqos, nctx, state)
		Expect(err).NotTo(HaveOccurred())
		Expect(client.transactions.Load()).To(BeZero())
	})
})

var _ = Describe("NetworkQoS network selection", func() {
	It("programs the default network and a secondary network from a single instance", func() {
		selected := map[string]string{"qos": "enabled"}
		nqos := newNetworkQoS("qos-both", []crdtypes.NetworkSelector{nadSelector(selected)})
		startController(initialDB(), defaultNetworkNAD(selected), streamNAD(selected), testNamespaceObj(), testPod(), nqos)

		By("binding a QoS rule to the default network node switch")
		defaultQoS := eventuallyQoS(defaultControllerNam, testNamespace, nqos.Name, 0)
		Expect(defaultQoS.ExternalIDs).NotTo(HaveKey(types.NetworkExternalID))
		eventuallySwitchHasQoS(testNode, defaultQoS)
		eventuallyAddressSetHas(defaultControllerNam, testPodDefaultIP)

		By("binding a QoS rule to the secondary network node switch")
		streamQoS := eventuallyQoS(streamControllerName, testNamespace, nqos.Name, 0)
		Expect(streamQoS.ExternalIDs).To(HaveKeyWithValue(types.NetworkExternalID, streamNetwork))
		eventuallySwitchHasQoS(streamNetwork+"_"+testNode, streamQoS)
		eventuallyAddressSetHas(streamControllerName, testPodStreamIP)

		By("clearing every network when the NetworkQoS is deleted")
		Expect(fakeNQoSClient.K8sV1alpha1().NetworkQoSes(testNamespace).Delete(context.Background(), nqos.Name,
			metav1.DeleteOptions{})).To(Succeed())
		eventuallyNoQoS(defaultControllerNam, testNamespace, nqos.Name, 0)
		eventuallyNoQoS(streamControllerName, testNamespace, nqos.Name, 0)
		eventuallyNoAddressSet(defaultControllerNam)
		eventuallyNoAddressSet(streamControllerName)
	})

	It("stops programming a network whose NAD no longer matches the network selector", func() {
		nad := streamNAD(map[string]string{"name": streamNetwork})
		nqos := newNetworkQoS("qos-stream", []crdtypes.NetworkSelector{
			nadSelector(map[string]string{"name": streamNetwork}),
		})
		networkManagerMock := startController(initialDB(), nad, testNamespaceObj(), testPod(), nqos)

		streamQoS := eventuallyQoS(streamControllerName, testNamespace, nqos.Name, 0)
		eventuallySwitchHasQoS(streamNetwork+"_"+testNode, streamQoS)

		By("relabeling the NAD so that it stops being selected")
		nad.Labels = map[string]string{"name": "something-else"}
		_, err := fakeNADClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(nad.Namespace).
			Update(context.Background(), nad, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() []string {
			nads, err := nqosController.nadLister.List(labels.SelectorFromSet(labels.Set{"name": streamNetwork}))
			Expect(err).NotTo(HaveOccurred())
			names := make([]string, 0, len(nads))
			for _, nad := range nads {
				names = append(names, nad.Name)
			}
			return names
		}).WithTimeout(10*time.Second).Should(BeEmpty(), "the NAD lister should see the new labels")

		By("notifying the controller through the network manager NAD reconciler")
		networkManagerMock.TriggerHandlers(streamNADKey, nil, false)
		eventuallyNoQoS(streamControllerName, testNamespace, nqos.Name, 0)
		eventuallyNoAddressSet(streamControllerName)
	})

	It("updates network selection when only namespace labels change", func() {
		nad := streamNAD(map[string]string{"name": streamNetwork})
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default", Labels: map[string]string{"selected": "no"}}}
		nqos := newNetworkQoS("qos-namespace-labels", crdtypes.NetworkSelectors{{
			NetworkSelectionType: crdtypes.NetworkAttachmentDefinitions,
			NetworkAttachmentDefinitionSelector: &crdtypes.NetworkAttachmentDefinitionSelector{
				NetworkSelector:   metav1.LabelSelector{MatchLabels: nad.Labels},
				NamespaceSelector: metav1.LabelSelector{MatchLabels: map[string]string{"selected": "yes"}},
			},
		}})
		startController(initialDB(), ns, nad, testNamespaceObj(), testPod(), nqos)
		Expect(findQoS(streamControllerName, testNamespace, nqos.Name, 0)).To(BeNil())
		setSelected := func(value, resourceVersion string) {
			ns, err := fakeKubeClient.CoreV1().Namespaces().Get(context.Background(), "default", metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			ns.Labels["selected"] = value
			ns.ResourceVersion = resourceVersion
			_, err = fakeKubeClient.CoreV1().Namespaces().Update(context.Background(), ns, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}
		By("selecting a previously unprogrammed network")
		setSelected("yes", "2")
		qos := eventuallyQoS(streamControllerName, testNamespace, nqos.Name, 0)
		eventuallySwitchHasQoS(streamNetwork+"_"+testNode, qos)
		eventuallyAddressSetHas(streamControllerName, testPodStreamIP)

		By("deselecting and selecting it again")
		setSelected("no", "3")
		eventuallyNoQoS(streamControllerName, testNamespace, nqos.Name, 0)
		eventuallyNoAddressSet(streamControllerName)
		setSelected("yes", "4")
		eventuallyQoS(streamControllerName, testNamespace, nqos.Name, 0)
		eventuallyAddressSetHas(streamControllerName, testPodStreamIP)

		By("removing the namespace before its NAD delete event arrives")
		Expect(fakeKubeClient.CoreV1().Namespaces().Delete(context.Background(), "default", metav1.DeleteOptions{})).To(Succeed())
		eventuallyNoQoS(streamControllerName, testNamespace, nqos.Name, 0)
		eventuallyNoAddressSet(streamControllerName)
	})

	It("selects every cluster user defined network with an empty network selector", func() {
		cudnNAD := streamNAD(map[string]string{"any": "label"})
		cudnNAD.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: cudnKind.GroupVersion().String(),
			Kind:       cudnKind.Kind,
			Name:       streamNetwork,
			UID:        "cudn-uid",
			Controller: ptr.To(true),
		}}
		nqos := newNetworkQoS("qos-all-cudns", crdtypes.NetworkSelectors{{
			NetworkSelectionType:              crdtypes.ClusterUserDefinedNetworks,
			ClusterUserDefinedNetworkSelector: &crdtypes.ClusterUserDefinedNetworkSelector{},
		}})
		// a NAD that no ClusterUserDefinedNetwork owns is not selected
		startController(initialDB(), cudnNAD, defaultNetworkNAD(nil), testNamespaceObj(), testPod(), nqos)
		qos := eventuallyQoS(streamControllerName, testNamespace, nqos.Name, 0)
		eventuallySwitchHasQoS(streamNetwork+"_"+testNode, qos)
		Consistently(func() *nbdb.QoS { return findQoS(defaultControllerNam, testNamespace, nqos.Name, 0) }).
			WithTimeout(time.Second).Should(BeNil())
		Expect(conditions(nqos.Name)).To(BeEmpty())
	})

	It("ignores a selected network without IP addresses", func() {
		selected := map[string]string{"qos": "enabled"}
		iplessNAD := ovnk8stesting.GenerateNAD("ipless", "ipless", "default", types.LocalnetTopology, "", types.NetworkRoleSecondary)
		iplessNAD.Labels = selected
		nqos := newNetworkQoS("qos-ipless", crdtypes.NetworkSelectors{nadSelector(selected)})
		nm := startController(initialDB(), iplessNAD, streamNAD(selected), testNamespaceObj(), testPod())
		iplessInfo, err := util.ParseNADInfo(iplessNAD)
		Expect(err).NotTo(HaveOccurred())
		ipv4Mode, ipv6Mode := iplessInfo.IPMode()
		Expect(ipv4Mode || ipv6Mode).To(BeFalse(), "the test network must have no IP family")
		nm.Lock()
		nm.NADNetworks["default/ipless"] = iplessInfo
		nm.Unlock()
		_, err = fakeNQoSClient.K8sV1alpha1().NetworkQoSes(testNamespace).Create(context.Background(), nqos, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		qos := eventuallyQoS(streamControllerName, testNamespace, nqos.Name, 0)
		eventuallySwitchHasQoS(streamNetwork+"_"+testNode, qos)
		Consistently(func() []metav1.Condition { return conditions(nqos.Name) }).WithTimeout(time.Second).Should(BeEmpty())
		Expect(findQoS("ipless-network-controller", testNamespace, nqos.Name, 0)).To(BeNil())
	})

	It("restores QoS after network recreation removes an earlier successful reconcile", func() {
		nad := streamNAD(map[string]string{"name": streamNetwork})
		nqos := newNetworkQoS("qos-recreated-network", []crdtypes.NetworkSelector{nadSelector(nad.Labels)})
		nm := startController(initialDB(), nad, testNamespaceObj(), testPod(), nqos)
		qos := eventuallyQoS(streamControllerName, testNamespace, nqos.Name, 0)
		eventuallySwitchHasQoS(streamNetwork+"_"+testNode, qos)
		eventuallyAddressSetHas(streamControllerName, testPodStreamIP)
		key := nqosKey(testNamespace, nqos.Name)
		state := func() *nqosCacheEntry {
			var state *nqosCacheEntry
			Expect(nqosController.cache.DoWithLock(key, func(key string) error {
				state, _ = nqosController.cache.Load(key)
				return nil
			})).To(Succeed())
			return state
		}
		Eventually(state).ShouldNot(BeNil())
		previous := state()

		By("processing the NAD update before the old network has been cleaned up")
		var conf map[string]interface{}
		Expect(json.Unmarshal([]byte(nad.Spec.Config), &conf)).To(Succeed())
		conf["mtu"] = 1400
		raw, err := json.Marshal(conf)
		Expect(err).NotTo(HaveOccurred())
		nad.Spec.Config = string(raw)
		newInfo, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())
		mutable := util.NewMutableNetInfo(newInfo)
		mutable.AddNADs(streamNADKey)
		nm.Lock()
		nm.NADNetworks[streamNADKey] = &secondaryNetInfoWrapper{NetInfo: mutable}
		nm.Unlock()
		nm.TriggerHandlers(streamNADKey, newInfo, false)
		Eventually(state).ShouldNot(BeIdenticalTo(previous))

		By("cleaning the old network and creating its replacement switch")
		ops, err := libovsdbops.DeleteLogicalSwitchOps(nbClient, nil, streamNetwork+"_"+testNode)
		Expect(err).NotTo(HaveOccurred())
		ops, err = libovsdbops.DeleteQoSesWithPredicateOps(nbClient, ops,
			func(qos *nbdb.QoS) bool {
				return isNetworkQoSObject(qos.ExternalIDs, streamControllerName, ovnObjectName(testNamespace, nqos.Name))
			})
		Expect(err).NotTo(HaveOccurred())
		Expect(addrSetManager.CleanupForController(streamControllerName)).To(Succeed())
		_, err = libovsdbops.TransactAndCheck(nbClient, ops)
		Expect(err).NotTo(HaveOccurred())
		eventuallyNoQoS(streamControllerName, testNamespace, nqos.Name, 0)
		Expect(libovsdbops.CreateOrUpdateLogicalSwitch(nbClient, &nbdb.LogicalSwitch{Name: streamNetwork + "_" + testNode})).To(Succeed())

		By("restoring rules and address sets once the new network creates its switch")
		qos = eventuallyQoS(streamControllerName, testNamespace, nqos.Name, 0)
		eventuallySwitchHasQoS(streamNetwork+"_"+testNode, qos)
		eventuallyAddressSetHas(streamControllerName, testPodStreamIP)
	})
})

var _ = Describe("NetworkQoS logical switches", func() {
	It("programs a NetworkQoS once its delayed logical switch is created, without reporting a failure", func() {
		nqos := newNetworkQoS("qos-delayed-switch", nil)
		rateLimiter := workqueue.NewTypedItemFastSlowRateLimiter[string](10*time.Millisecond, 10*time.Millisecond, 1)
		startControllerWithRateLimiter(&libovsdbtest.TestSetup{}, rateLimiter, testNamespaceObj(), testPod(), nqos)
		key := nqosKey(testNamespace, nqos.Name)
		Eventually(func() bool { return nqosController.isProgrammed(key) }).Should(BeTrue())

		By("waiting for the switch rather than failing and retrying")
		Consistently(func() int { return rateLimiter.NumRequeues(key) }).WithTimeout(time.Second).Should(BeZero())
		Expect(conditions(nqos.Name)).To(BeEmpty())
		eventuallyAddressSetHas(defaultControllerNam, testPodDefaultIP)

		By("creating the switch without another Kubernetes event")
		Expect(libovsdbops.CreateOrUpdateLogicalSwitch(nbClient, &nbdb.LogicalSwitch{Name: testNode})).To(Succeed())
		qos := eventuallyQoS(defaultControllerNam, testNamespace, nqos.Name, 0)
		eventuallySwitchHasQoS(testNode, qos)
		Consistently(func() []metav1.Condition { return conditions(nqos.Name) }).WithTimeout(time.Second).Should(BeEmpty())
	})

	It("requeues a NetworkQoS whose first reconciliation waits for a switch created meanwhile", func() {
		nqos := newNetworkQoS("qos-in-flight", nil)
		startController(initialDB(), testNamespaceObj(), nqos)
		c := newDirectController(nbClient)
		c.nqosLister = watchFactory.NetworkQoSInformer().Lister()
		fake := &controller.FakeController{}
		c.nqosController = fake
		key := nqosKey(testNamespace, nqos.Name)
		Eventually(func() error {
			_, err := c.nqosLister.NetworkQoSes(testNamespace).Get(nqos.Name)
			return err
		}).Should(Succeed())

		By("creating the switch while the first reconciliation, which found none, is in flight")
		c.cache.LockKey(key)
		done := make(chan error)
		go func() { done <- c.reconcileLogicalSwitch("new-switch") }()
		Consistently(done).WithTimeout(200 * time.Millisecond).ShouldNot(Receive())
		c.cache.Store(key, &nqosCacheEntry{networks: map[string]*networkQoSState{"blue": {switchName: "new-switch"}}})
		c.cache.UnlockKey(key)
		Eventually(done).Should(Receive(BeNil()))
		Expect(fake.Reconciles).To(ConsistOf("Reconcile:" + key))
	})

	It("binds the QoS rules before any pod matches the source selector", func() {
		nqos := newNetworkQoS("qos-no-pods", nil)
		startController(initialDB(), testNamespaceObj(), nqos)

		By("binding to the node switch with an empty source address set")
		defaultQoS := eventuallyQoS(defaultControllerNam, testNamespace, nqos.Name, 0)
		eventuallySwitchHasQoS(testNode, defaultQoS)
		Expect(sourceAddresses(defaultControllerNam)).To(BeEmpty())

		By("holding the address of a pod created afterwards")
		_, err := fakeKubeClient.CoreV1().Pods(testNamespace).Create(context.Background(), testPod(), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		eventuallyAddressSetHas(defaultControllerNam, testPodDefaultIP)
	})
})

var _ = Describe("NetworkQoS address sets", func() {
	It("keeps an empty destination pod selector within the policy namespace", func() {
		nqos := newNetworkQoS("qos-empty-selectors", nil)
		nqos.Spec.Egress[0].Classifier.To = []nqosapi.Destination{{PodSelector: &metav1.LabelSelector{}}}
		local := defaultNetworkPod("local-peer", testNamespace, testNode, "10.192.177.5", "server")
		remote := defaultNetworkPod("remote-peer", "remote", "node2", "10.192.177.6", "server")
		host := defaultNetworkPod("host-peer", testNamespace, testNode, "10.192.177.7", "server")
		host.Spec.HostNetwork = true
		completed := defaultNetworkPod("completed-peer", testNamespace, testNode, "10.192.177.8", "server")
		completed.Status.Phase = corev1.PodSucceeded
		startController(initialDB(), testNamespaceObj(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "remote"}},
			testPod(), local, remote, host, completed, nqos)
		qos := eventuallyQoS(defaultControllerNam, testNamespace, nqos.Name, 0)
		eventuallySwitchHasQoS(testNode, qos)

		ids := addresssetmanager.GetPodSelectorAddrSetDbIDs(&metav1.LabelSelector{}, nil, nil, testNamespace,
			defaultControllerNam, false, addresssetmanager.ExcludeHostNetworkPods())
		Eventually(func() []*nbdb.AddressSet { return findAddressSets(ids) }).Should(HaveLen(1))
		factory := addressset.NewOvnAddressSetFactory(nbClient, true, false)
		as, err := factory.GetAddressSet(ids)
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() []string { v4, _ := as.GetAddresses(); return v4 }).Should(ConsistOf(testPodDefaultIP, "10.192.177.5"))
		hash, _ := as.GetASHashNames()
		Expect(qos.Match).To(ContainSubstring("ip4.dst == {$" + hash + "}"))
	})

	It("keeps shared destinations for identically named policies in different namespaces", func() {
		otherNamespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "other-qos-namespace"}}
		first := newNetworkQoS("same-name", nil)
		selector := &metav1.LabelSelector{MatchLabels: map[string]string{"app": "server"}}
		first.Spec.Egress[0].Classifier.To = []nqosapi.Destination{{PodSelector: selector, NamespaceSelector: &metav1.LabelSelector{}}}
		second := first.DeepCopy()
		second.Namespace = otherNamespace.Name
		startController(initialDB(), testNamespaceObj(), otherNamespace, testPod(), first, second,
			defaultNetworkPod("other-client", otherNamespace.Name, testNode, "10.192.177.8", "client"),
			defaultNetworkPod("server", otherNamespace.Name, "node2", "10.192.177.9", "server"))
		eventuallyQoS(defaultControllerNam, first.Namespace, first.Name, 0)
		qos := eventuallyQoS(defaultControllerNam, second.Namespace, second.Name, 0)
		ids := addresssetmanager.GetPodSelectorAddrSetDbIDs(selector, &metav1.LabelSelector{}, nil, "",
			defaultControllerNam, false, addresssetmanager.ExcludeHostNetworkPods())
		Eventually(func() []*nbdb.AddressSet { return findAddressSets(ids) }).Should(ConsistOf(
			HaveField("Addresses", ConsistOf("10.192.177.9"))))
		hash := findAddressSets(ids)[0].Name
		Expect(qos.Match).To(ContainSubstring("ip4.dst == {$" + hash + "}"))

		Expect(fakeNQoSClient.K8sV1alpha1().NetworkQoSes(first.Namespace).Delete(context.Background(), first.Name, metav1.DeleteOptions{})).To(Succeed())
		eventuallyNoQoS(defaultControllerNam, first.Namespace, first.Name, 0)
		Eventually(func() bool {
			var exists bool
			Expect(nqosController.cache.DoWithLock(nqosKey(first.Namespace, first.Name), func(key string) error {
				_, exists = nqosController.cache.Load(key)
				return nil
			})).To(Succeed())
			return exists
		}).Should(BeFalse())
		Eventually(func() []*nbdb.AddressSet { return findSourceAddressSets(defaultControllerNam) }).Should(BeEmpty())
		Expect(findAddressSets(ids)).To(HaveLen(1))
		Expect(findQoS(defaultControllerNam, second.Namespace, second.Name, 0).UUID).To(Equal(qos.UUID))
		eventuallySwitchHasQoS(testNode, qos)

		Expect(fakeNQoSClient.K8sV1alpha1().NetworkQoSes(second.Namespace).Delete(context.Background(), second.Name, metav1.DeleteOptions{})).To(Succeed())
		eventuallyNoQoS(defaultControllerNam, second.Namespace, second.Name, 0)
		Eventually(func() []*nbdb.AddressSet { return findAddressSets(ids) }).Should(BeEmpty())
	})

	It("shrinks rules and releases replaced destinations only after their last user leaves", func() {
		selectorA := &metav1.LabelSelector{MatchLabels: map[string]string{"app": "a"}}
		selectorB := &metav1.LabelSelector{MatchLabels: map[string]string{"app": "b"}}
		first := newNetworkQoS("qos-changing-rules", nil)
		first.Spec.Egress[0].Classifier.To = []nqosapi.Destination{{PodSelector: selectorA}}
		second := first.DeepCopy()
		second.Name = "qos-retaining-destination"
		ruleB := *first.Spec.Egress[0].DeepCopy()
		ruleB.Classifier.To = []nqosapi.Destination{{PodSelector: selectorB}}
		first.Spec.Egress = append(first.Spec.Egress, ruleB)
		startController(initialDB(), testNamespaceObj(), testPod(), first, second,
			defaultNetworkPod("peer-a", testNamespace, "node2", "10.192.177.5", "a"),
			defaultNetworkPod("peer-b", testNamespace, "node2", "10.192.177.6", "b"))
		original := eventuallyQoS(defaultControllerNam, testNamespace, first.Name, 0)
		eventuallyQoS(defaultControllerNam, testNamespace, first.Name, 1)
		eventuallyQoS(defaultControllerNam, testNamespace, second.Name, 0)
		idsA := addresssetmanager.GetPodSelectorAddrSetDbIDs(selectorA, nil, nil, testNamespace,
			defaultControllerNam, false, addresssetmanager.ExcludeHostNetworkPods())
		idsB := addresssetmanager.GetPodSelectorAddrSetDbIDs(selectorB, nil, nil, testNamespace,
			defaultControllerNam, false, addresssetmanager.ExcludeHostNetworkPods())
		Eventually(func() []*nbdb.AddressSet { return findAddressSets(idsA) }).Should(HaveLen(1))
		Eventually(func() []*nbdb.AddressSet { return findAddressSets(idsB) }).Should(HaveLen(1))
		hashB := findAddressSets(idsB)[0].Name

		By("keeping one rule, changing its destination, and removing its bandwidth limit")
		updated, err := fakeNQoSClient.K8sV1alpha1().NetworkQoSes(testNamespace).Get(context.Background(), first.Name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		ruleB.DSCP = 0
		ruleB.Bandwidth = nqosapi.Bandwidth{}
		updated.Spec.Egress = []nqosapi.Rule{ruleB}
		updated.Generation++
		_, err = fakeNQoSClient.K8sV1alpha1().NetworkQoSes(testNamespace).Update(context.Background(), updated, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
		eventuallyNoQoS(defaultControllerNam, testNamespace, first.Name, 1)
		Eventually(func() *nbdb.QoS { return findQoS(defaultControllerNam, testNamespace, first.Name, 0) }).Should(SatisfyAll(
			HaveField("UUID", original.UUID),
			HaveField("Action", Equal(map[string]int{nbdb.QoSActionDSCP: 0})),
			HaveField("Bandwidth", BeEmpty()),
			HaveField("Match", ContainSubstring("ip4.dst == {$"+hashB+"}")),
		))
		Expect(findAddressSets(idsA)).To(HaveLen(1))
		Expect(fakeNQoSClient.K8sV1alpha1().NetworkQoSes(testNamespace).Delete(context.Background(), second.Name, metav1.DeleteOptions{})).To(Succeed())
		Eventually(func() []*nbdb.AddressSet { return findAddressSets(idsA) }).Should(BeEmpty())
		Expect(findAddressSets(idsB)).To(HaveLen(1))
		eventuallyAddressSetHas(defaultControllerNam, testPodDefaultIP)

		By("releasing all remaining sets and rules after the last policy is deleted")
		Expect(fakeNQoSClient.K8sV1alpha1().NetworkQoSes(testNamespace).Delete(context.Background(), first.Name, metav1.DeleteOptions{})).To(Succeed())
		eventuallyNoQoS(defaultControllerNam, testNamespace, first.Name, 0)
		Eventually(func() []*nbdb.AddressSet { return findAddressSets(idsB) }).Should(BeEmpty())
		eventuallyNoAddressSet(defaultControllerNam)
	})

	DescribeTable("retains address sets while QoS removal fails",
		func(deleteObject bool) {
			startController(initialDB(), testNamespaceObj(), testPod())
			nqos := newNetworkQoS("qos-delete-retry", nil)
			c := newDirectController(nbClient)
			state, err := c.ensureNetworkQoSForNetwork(nqos, c.newNetworkContext(&util.DefaultNetInfo{}), nil)
			Expect(err).NotTo(HaveOccurred())
			qos := eventuallyQoS(defaultControllerNam, testNamespace, nqos.Name, 0)
			eventuallySwitchHasQoS(testNode, qos)
			eventuallyAddressSetHas(defaultControllerNam, testPodDefaultIP)
			key := nqosKey(testNamespace, nqos.Name)
			c.cache.Store(key, &nqosCacheEntry{networks: map[string]*networkQoSState{types.DefaultNetworkName: state}})
			c.nbClient = &failQoSUnbindClient{Client: nbClient}
			clear := func() error {
				if deleteObject {
					return c.clearNetworkQoS(key, testNamespace, nqos.Name)
				}
				return c.clearNetworkQoSOnNetwork(state)
			}
			Expect(clear()).To(MatchError(ContainSubstring("temporary QoS unbind failure")))
			eventuallySwitchHasQoS(testNode, qos)
			Expect(findSourceAddressSets(defaultControllerNam)).To(HaveLen(1), "a bound QoS must retain its referenced address set")
			Expect(state.addrSetKeys).NotTo(BeEmpty())
			Expect(clear()).To(Succeed())
			eventuallyNoQoS(defaultControllerNam, testNamespace, nqos.Name, 0)
			eventuallyNoAddressSet(defaultControllerNam)
			Expect(state.addrSetKeys).To(BeEmpty())
		},
		Entry("when deleting the policy", true),
		Entry("when deselecting a network", false),
	)

	It("cleans both networks if the policy is deleted before one of them finishes provisioning", func() {
		selected := map[string]string{"qos": "enabled"}
		nqos := newNetworkQoS("qos-partial-network", crdtypes.NetworkSelectors{nadSelector(selected)})
		db := &libovsdbtest.TestSetup{NBData: []libovsdbtest.TestData{&nbdb.LogicalSwitch{Name: testNode}}}
		limiter := workqueue.NewTypedItemFastSlowRateLimiter[string](10*time.Millisecond, 10*time.Millisecond, 1)
		startControllerWithRateLimiter(db, limiter, defaultNetworkNAD(selected), streamNAD(selected), testNamespaceObj(), testPod(), nqos)
		qos := eventuallyQoS(defaultControllerNam, testNamespace, nqos.Name, 0)
		eventuallySwitchHasQoS(testNode, qos)
		eventuallyAddressSetHas(defaultControllerNam, testPodDefaultIP)
		eventuallyAddressSetHas(streamControllerName, testPodStreamIP)
		By("waiting for the switch of the network being set up without reporting a failure")
		Consistently(func() []metav1.Condition { return conditions(nqos.Name) }).WithTimeout(time.Second).Should(BeEmpty())
		Expect(limiter.NumRequeues(nqosKey(testNamespace, nqos.Name))).To(BeZero())

		By("deleting the policy while the network being set up holds address-set references")
		Expect(fakeNQoSClient.K8sV1alpha1().NetworkQoSes(testNamespace).Delete(context.Background(), nqos.Name, metav1.DeleteOptions{})).To(Succeed())
		eventuallyNoQoS(defaultControllerNam, testNamespace, nqos.Name, 0)
		eventuallyNoAddressSet(defaultControllerNam)
		eventuallyNoAddressSet(streamControllerName)
	})
})

var _ = Describe("NetworkQoS status", func() {
	It("reports a spec no network can accept without retrying it", func() {
		nqos := newNetworkQoS("qos-invalid", nil)
		nqos.Spec.Egress[0].Classifier.To = []nqosapi.Destination{{
			IPBlock:     &networkingv1.IPBlock{CIDR: "10.10.0.0/16"},
			PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "server"}},
		}}
		startController(initialDB(), testNamespaceObj(), testPod(), nqos)

		By("reporting the failure of this zone")
		Eventually(func() []metav1.Condition {
			return conditions(nqos.Name)
		}).WithTimeout(10 * time.Second).WithPolling(500 * time.Millisecond).Should(ConsistOf(
			SatisfyAll(
				HaveField("Type", conditionTypeReady+testNode),
				HaveField("Status", metav1.ConditionFalse),
				HaveField("Message", ContainSubstring(types.NetworkQoSErrorMsg)),
			)))

		By("not programming anything for it")
		Expect(findQoS(defaultControllerNam, testNamespace, nqos.Name, 0)).To(BeNil())

		By("retracting the condition once the spec is fixed")
		nqos, err := fakeNQoSClient.K8sV1alpha1().NetworkQoSes(testNamespace).Get(context.Background(), nqos.Name,
			metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		nqos.Spec.Egress[0].Classifier.To[0].PodSelector = nil
		// Bump generation ourselves. The fake client does not do it, and unchanged
		// generations are ignored as resyncs.
		nqos.Generation++
		_, err = fakeNQoSClient.K8sV1alpha1().NetworkQoSes(testNamespace).Update(context.Background(), nqos,
			metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())

		eventuallyQoS(defaultControllerNam, testNamespace, nqos.Name, 0)
		Eventually(func() []metav1.Condition {
			return conditions(nqos.Name)
		}).WithTimeout(10 * time.Second).WithPolling(500 * time.Millisecond).Should(BeEmpty())
	})

	It("removes what a previous generation programmed once the spec becomes invalid", func() {
		nqos := newNetworkQoS("qos-turned-invalid", nil)
		startController(initialDB(), testNamespaceObj(), testPod(), nqos)
		qos := eventuallyQoS(defaultControllerNam, testNamespace, nqos.Name, 0)
		eventuallySwitchHasQoS(testNode, qos)
		eventuallyAddressSetHas(defaultControllerNam, testPodDefaultIP)

		By("updating it to a spec no network accepts")
		updated, err := fakeNQoSClient.K8sV1alpha1().NetworkQoSes(testNamespace).Get(context.Background(), nqos.Name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		updated.Spec.Egress[0].Classifier.To = []nqosapi.Destination{{IPBlock: &networkingv1.IPBlock{CIDR: "10.0.0.0/8", Except: []string{"fd00::/8"}}}}
		updated.Generation++
		_, err = fakeNQoSClient.K8sV1alpha1().NetworkQoSes(testNamespace).Update(context.Background(), updated, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())

		Eventually(func() []metav1.Condition { return conditions(nqos.Name) }).Should(ConsistOf(
			HaveField("Message", ContainSubstring("invalid spec"))))
		eventuallyNoQoS(defaultControllerNam, testNamespace, nqos.Name, 0)
		eventuallyNoAddressSet(defaultControllerNam)
	})

	It("leaves an invalid spec to the zone that reports it, and reports it once that zone no longer does", func() {
		nqos := newNetworkQoS("qos-invalid-everywhere", nil)
		nqos.Generation = 1
		nqos.Spec.Egress[0].Classifier.To = []nqosapi.Destination{{IPBlock: &networkingv1.IPBlock{CIDR: "10.0.0.0/33"}}}
		validationErr := validateAndNormalizeSpec(nqos.DeepCopy())
		Expect(validationErr).To(HaveOccurred())
		otherZone := metav1.Condition{
			Type:               conditionTypeReady + "node-a",
			Status:             metav1.ConditionFalse,
			Reason:             reasonQoSSetupFailed,
			Message:            truncateMessage(specFailureMessage(validationErr)),
			ObservedGeneration: 1,
			LastTransitionTime: metav1.Now(),
		}
		nqos.Status.Conditions = []metav1.Condition{otherZone}
		startController(initialDB(), testNamespaceObj(), testPod(), nqos)
		Consistently(func() []metav1.Condition { return conditions(nqos.Name) }).WithTimeout(time.Second).
			Should(ConsistOf(HaveField("Type", otherZone.Type)))

		By("taking over once the zone reporting it no longer does")
		current, err := fakeNQoSClient.K8sV1alpha1().NetworkQoSes(testNamespace).Get(context.Background(), nqos.Name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		current.Status.Conditions = nil
		_, err = fakeNQoSClient.K8sV1alpha1().NetworkQoSes(testNamespace).UpdateStatus(context.Background(), current, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() []metav1.Condition { return conditions(nqos.Name) }).Should(ConsistOf(SatisfyAll(
			HaveField("Type", conditionTypeReady+testNode),
			HaveField("Message", otherZone.Message),
		)))
	})

	It("reports the pods whose addresses cannot be resolved, and retracts it once the address set manager resolves them", func() {
		pod := testPod()
		resolvable := pod.Annotations[types.OvnPodAnnotationName]
		pod.Annotations[types.OvnPodAnnotationName] = "malformed"
		nad := streamNAD(map[string]string{"name": streamNetwork})
		nqos := newNetworkQoS("qos-unresolved", crdtypes.NetworkSelectors{nadSelector(nad.Labels)})
		// Delay retries for an hour so recovery depends on the address set notification.
		rateLimiter := workqueue.NewTypedItemFastSlowRateLimiter[string](time.Hour, time.Hour, 0)
		startControllerWithRateLimiter(initialDB(), rateLimiter, nad, testNamespaceObj(), pod, nqos)

		By("programming the rules and reporting the failure")
		qos := eventuallyQoS(streamControllerName, testNamespace, nqos.Name, 0)
		eventuallySwitchHasQoS(streamNetwork+"_"+testNode, qos)
		Eventually(func() []metav1.Condition { return conditions(nqos.Name) }).WithTimeout(10 * time.Second).Should(ConsistOf(SatisfyAll(
			HaveField("Type", conditionTypeReady+testNode),
			HaveField("Status", metav1.ConditionFalse),
			HaveField("Message", types.NetworkQoSErrorMsg+
				": failed to resolve the addresses of the pods it selects on network(s) "+streamNetwork),
		)))
		Expect(sourceAddresses(streamControllerName)).To(BeEmpty())

		By("retracting the failure once the addresses are resolved")
		pod.Annotations[types.OvnPodAnnotationName] = resolvable
		pod.ResourceVersion = "2"
		_, err := fakeKubeClient.CoreV1().Pods(testNamespace).Update(context.Background(), pod, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
		eventuallyAddressSetHas(streamControllerName, testPodStreamIP)
		Eventually(func() []metav1.Condition { return conditions(nqos.Name) }).WithTimeout(10 * time.Second).Should(BeEmpty())
	})

	It("keeps the success a previous version reported for this zone until a cluster manager of this version reports no error", func() {
		startController(initialDB(), testNamespaceObj(), testPod())
		nqos := newNetworkQoS("qos-legacy-success", nil)
		nqos.Generation = 1
		_, err := fakeNQoSClient.K8sV1alpha1().NetworkQoSes(testNamespace).Create(context.Background(), nqos, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		eventuallyQoS(defaultControllerNam, testNamespace, nqos.Name, 0)

		By("keeping the success the per-network controller of this zone reported")
		legacySuccess := nqosapiapply.NetworkQoS(nqos.Name, testNamespace).WithStatus(nqosapiapply.Status().WithConditions(
			metaapplyv1.Condition().
				WithType(conditionTypeReady + testNode).
				WithStatus(metav1.ConditionTrue).
				WithReason("Success").
				WithMessage("NetworkQoS was applied successfully").
				WithObservedGeneration(1).
				WithLastTransitionTime(metav1.Now())))
		_, err = fakeNQoSClient.K8sV1alpha1().NetworkQoSes(testNamespace).ApplyStatus(context.Background(), legacySuccess,
			metav1.ApplyOptions{FieldManager: testNode, Force: true})
		Expect(err).NotTo(HaveOccurred())
		current, err := fakeNQoSClient.K8sV1alpha1().NetworkQoSes(testNamespace).Get(context.Background(), nqos.Name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		current.Spec.Egress[0].DSCP = 51
		// Bump generation ourselves. The fake client does not do it, and unchanged
		// generations are ignored as resyncs.
		current.Generation++
		_, err = fakeNQoSClient.K8sV1alpha1().NetworkQoSes(testNamespace).Update(context.Background(), current, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() int {
			if qos := findQoS(defaultControllerNam, testNamespace, nqos.Name, 0); qos != nil {
				return qos.Action[nbdb.QoSActionDSCP]
			}
			return 0
		}).WithTimeout(10 * time.Second).Should(Equal(51))
		Consistently(func() []metav1.Condition { return conditions(nqos.Name) }).WithTimeout(time.Second).Should(ConsistOf(
			HaveField("Status", metav1.ConditionTrue)))

		By("retracting it once a cluster manager of this version reports no error")
		noErrors := nqosapiapply.NetworkQoS(nqos.Name, testNamespace).WithStatus(
			nqosapiapply.Status().WithStatus(types.NetworkQoSNoErrorsMsg))
		_, err = fakeNQoSClient.K8sV1alpha1().NetworkQoSes(testNamespace).ApplyStatus(context.Background(), noErrors,
			metav1.ApplyOptions{FieldManager: "cluster-manager", Force: true})
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() []metav1.Condition { return conditions(nqos.Name) }).WithTimeout(10 * time.Second).Should(BeEmpty())
	})

	It("retracts a failure it reported before the informer sees it", func() {
		nqos := newNetworkQoS("qos-informer-lag", nil)
		fakeNQoSClient = nqosfake.NewClientset(nqos.DeepCopy())
		// The informer never sees the status writes.
		indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
		Expect(indexer.Add(nqos)).To(Succeed())
		c := newDirectController(nil)
		c.nqosClientSet = fakeNQoSClient
		c.nqosLister = networkqoslister.NewNetworkQoSLister(indexer)

		Expect(c.reportFailure(testNamespace, nqos.Name, "failed to configure network(s) default")).To(Succeed())
		Expect(conditions(nqos.Name)).To(ConsistOf(HaveField("Type", conditionTypeReady+testNode)))

		Expect(c.clearStaleZoneStatus(testNamespace, nqos.Name)).To(Succeed())
		Expect(conditions(nqos.Name)).To(BeEmpty())
		Expect(c.failureReported(nqosKey(testNamespace, nqos.Name))).To(BeFalse())
	})
})

var _ = Describe("NetworkQoS restart and upgrade", func() {
	It("turns the layer2 rules of previous versions around in place", func() {
		startController(initialDB(), testNamespaceObj(), testPod())
		nad := ovnk8stesting.GenerateNAD(streamNetwork, streamNetwork, "default", types.Layer2Topology,
			"10.128.0.0/16", types.NetworkRoleSecondary)
		info, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())
		netInfo := util.NewMutableNetInfo(info)
		netInfo.AddNADs(streamNADKey)
		c := newDirectController(nbClient)
		nctx := c.newNetworkContext(netInfo)
		Expect(libovsdbops.CreateOrUpdateLogicalSwitch(nbClient, &nbdb.LogicalSwitch{Name: nctx.logicalSwitchName(testNode)})).To(Succeed())
		nqos := newNetworkQoS("qos-previous-direction", nil)
		_, err = c.ensureNetworkQoSForNetwork(nqos, nctx, nil)
		Expect(err).NotTo(HaveOccurred())

		By("applying the rule as packets leave the switch, as previous versions did")
		previous := eventuallyQoS(streamControllerName, testNamespace, nqos.Name, 0)
		Expect(previous.Direction).To(Equal(nbdb.QoSDirectionFromLport))
		previous.Direction = nbdb.QoSDirectionToLport
		ops, err := libovsdbops.UpdateQoSesOps(nbClient, nil, previous)
		Expect(err).NotTo(HaveOccurred())
		_, err = libovsdbops.TransactAndCheck(nbClient, ops)
		Expect(err).NotTo(HaveOccurred())

		By("reconciling it in a new process")
		c = newDirectController(nbClient)
		_, err = c.ensureNetworkQoSForNetwork(nqos, c.newNetworkContext(netInfo), nil)
		Expect(err).NotTo(HaveOccurred())
		qos := findQoS(streamControllerName, testNamespace, nqos.Name, 0)
		Expect(qos.UUID).To(Equal(previous.UUID))
		Expect(qos.Direction).To(Equal(nbdb.QoSDirectionFromLport))
		Expect(qos.Match).To(Equal(previous.Match))
		eventuallySwitchHasQoS(nctx.logicalSwitchName(testNode), qos)
	})

	DescribeTable("cleans networks deselected while the controller was stopped",
		func(selectors crdtypes.NetworkSelectors, multiNetwork, keepDefault bool) {
			if !multiNetwork {
				config.OVNKubernetesFeature.EnableMultiNetwork = false
				DeferCleanup(func() { config.OVNKubernetesFeature.EnableMultiNetwork = true })
			}
			nqos := newNetworkQoS("qos-restarted", selectors)
			db := initialDB()
			for index, owner := range []string{defaultControllerNam, streamControllerName} {
				qosIDs := libovsdbops.NewDbObjectIDs(libovsdbops.NetworkQoS, owner, map[libovsdbops.ExternalIDKey]string{
					libovsdbops.ObjectNameKey: ovnObjectName(testNamespace, nqos.Name),
					libovsdbops.RuleIndex:     "0",
				})
				legacyIDs := libovsdbops.NewDbObjectIDs(libovsdbops.AddressSetNetworkQoS, owner, map[libovsdbops.ExternalIDKey]string{
					libovsdbops.ObjectNameKey:   ovnObjectName(testNamespace, nqos.Name),
					libovsdbops.RuleIndex:       "src",
					libovsdbops.IpBlockIndexKey: "0",
				})
				legacyAS, _ := addressset.GetTestDbAddrSets(legacyIDs, []string{testPodDefaultIP})
				qos := &nbdb.QoS{
					UUID:        fmt.Sprintf("previous-qos-%d", index),
					Direction:   nbdb.QoSDirectionToLport,
					Priority:    11000,
					Match:       "ip4.src == $" + legacyAS.Name,
					Action:      map[string]int{nbdb.QoSActionDSCP: 50},
					ExternalIDs: qosIDs.GetExternalIDs(),
				}
				if owner == streamControllerName {
					qos.ExternalIDs[types.NetworkExternalID] = streamNetwork
				}
				db.NBData[index].(*nbdb.LogicalSwitch).QOSRules = []string{qos.UUID}
				db.NBData = append(db.NBData, qos, legacyAS)
			}
			startController(db, streamNAD(map[string]string{"name": "no-longer-selected"}), testNamespaceObj(), testPod(), nqos)
			eventuallyNoQoS(streamControllerName, testNamespace, nqos.Name, 0)
			if keepDefault {
				qos := eventuallyQoS(defaultControllerNam, testNamespace, nqos.Name, 0)
				eventuallySwitchHasQoS(testNode, qos)
				eventuallyAddressSetHas(defaultControllerNam, testPodDefaultIP)
			} else {
				eventuallyNoQoS(defaultControllerNam, testNamespace, nqos.Name, 0)
			}
			Eventually(func() []*nbdb.AddressSet {
				addrSets, err := libovsdbops.FindAddressSetsWithPredicate(nbClient,
					func(addrSet *nbdb.AddressSet) bool {
						return isNetworkQoSObject(addrSet.ExternalIDs, "", ovnObjectName(testNamespace, nqos.Name))
					})
				Expect(err).NotTo(HaveOccurred())
				return addrSets
			}).Should(BeEmpty())
		},
		Entry("preserves the selected default network", nil, true, true),
		Entry("removes every network when NAD labels no longer match", crdtypes.NetworkSelectors{nadSelector(map[string]string{"name": streamNetwork})}, true, false),
		// Network selectors select NADs only, so they select no network, not the default one.
		Entry("removes every network when multiple networks are disabled", crdtypes.NetworkSelectors{nadSelector(nil)}, false, false),
	)

	It("repairs stale NetworkQoS objects without touching other features", func() {
		staleExternalIDs := libovsdbops.NewDbObjectIDs(libovsdbops.NetworkQoS, defaultControllerNam,
			map[libovsdbops.ExternalIDKey]string{
				libovsdbops.ObjectNameKey: "some-namespace:deleted-qos",
				libovsdbops.RuleIndex:     "0",
			}).GetExternalIDs()
		egressQoSExternalIDs := libovsdbops.NewDbObjectIDs(libovsdbops.QoSEgressQoS, defaultControllerNam,
			map[libovsdbops.ExternalIDKey]string{
				libovsdbops.ObjectNameKey: "some-namespace",
				libovsdbops.PriorityKey:   "1000",
			}).GetExternalIDs()
		db := &libovsdbtest.TestSetup{
			NBData: []libovsdbtest.TestData{
				&nbdb.LogicalSwitch{
					Name:     testNode,
					QOSRules: []string{"stale-nqos-uuid", "egressqos-uuid"},
				},
				&nbdb.QoS{
					UUID:        "stale-nqos-uuid",
					Direction:   nbdb.QoSDirectionToLport,
					Priority:    11000,
					Match:       "ip4.src == {$a1234}",
					Action:      map[string]int{nbdb.QoSActionDSCP: 10},
					ExternalIDs: staleExternalIDs,
				},
				&nbdb.QoS{
					UUID:        "egressqos-uuid",
					Direction:   nbdb.QoSDirectionToLport,
					Priority:    1000,
					Match:       "ip4.src == {$a5678}",
					Action:      map[string]int{nbdb.QoSActionDSCP: 20},
					ExternalIDs: egressQoSExternalIDs,
				},
			},
		}
		startController(db, testNamespaceObj())

		By("deleting the QoS rule of a NetworkQoS that no longer exists")
		Eventually(func() []*nbdb.QoS {
			qoses, err := libovsdbops.FindQoSesWithPredicate(nbClient, func(qos *nbdb.QoS) bool {
				return qos.ExternalIDs[libovsdbops.ObjectNameKey.String()] == "some-namespace:deleted-qos"
			})
			Expect(err).NotTo(HaveOccurred())
			return qoses
		}).WithTimeout(10 * time.Second).Should(BeEmpty())

		By("keeping the QoS rule owned by EgressQoS on the same controller")
		Consistently(func() int {
			qoses, err := libovsdbops.FindQoSesWithPredicate(nbClient, func(qos *nbdb.QoS) bool {
				return qos.ExternalIDs[libovsdbops.OwnerTypeKey.String()] == string(libovsdbops.EgressQoSOwnerType)
			})
			Expect(err).NotTo(HaveOccurred())
			return len(qoses)
		}).WithTimeout(2 * time.Second).WithPolling(500 * time.Millisecond).Should(Equal(1))
	})
})
