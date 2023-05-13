package ovn

import (
	"context"
	"fmt"
	"math/rand"
	"net"
	"strings"

	"github.com/onsi/ginkgo"
	"github.com/onsi/gomega"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	anpovn "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/controller/admin_network_policy"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/urfave/cli/v2"
	kapi "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilpointer "k8s.io/utils/pointer"
	anpapi "sigs.k8s.io/network-policy-api/apis/v1alpha1"
)

var banpLabel = map[string]string{"house": "gryffindor"}

func newBANPObject(name string, subject anpapi.AdminNetworkPolicySubject,
	ingressRules []anpapi.BaselineAdminNetworkPolicyIngressRule, egressRules []anpapi.BaselineAdminNetworkPolicyEgressRule) *anpapi.BaselineAdminNetworkPolicy {
	return &anpapi.BaselineAdminNetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: anpapi.BaselineAdminNetworkPolicySpec{
			Subject: subject,
			Ingress: ingressRules,
			Egress:  egressRules,
		},
	}
}

func getBANPRulePriority(index int32) int32 {
	return (anpovn.BANPFlowPriority - index)
}

func getDefaultPGForBANPSubject(banpName string, portUUIDs []string, acls []*nbdb.ACL) *nbdb.PortGroup {
	lsps := []*nbdb.LogicalSwitchPort{}
	for _, uuid := range portUUIDs {
		lsps = append(lsps, &nbdb.LogicalSwitchPort{UUID: uuid})
	}
	pgExternalIDs := map[string]string{"name": fmt.Sprintf("BaselineAdminNetworkPolicy_%s_%d", banpName, anpovn.BANPFlowPriority)}
	pg := libovsdbops.BuildPortGroup(
		util.HashForOVN(fmt.Sprintf("BaselineAdminNetworkPolicy_%s_%d", banpName, anpovn.BANPFlowPriority)),
		lsps,
		acls,
		pgExternalIDs,
	)
	pg.UUID = pg.Name + "-UUID"
	return pg
}

func getBANPGressACL(action anpapi.BaselineAdminNetworkPolicyRuleAction, banpName, direction string, banpPriority, rulePriority int32, ports *[]anpapi.AdminNetworkPolicyPort) []*nbdb.ACL {
	// we are not using BuildACL and instead manually building it on purpose so that the code path for BuildACL is also tested
	acl := nbdb.ACL{}
	if action == anpapi.BaselineAdminNetworkPolicyRuleActionAllow {
		acl.Action = nbdb.ACLActionAllowRelated
	} else if action == anpapi.BaselineAdminNetworkPolicyRuleActionDeny {
		acl.Action = nbdb.ACLActionDrop
	}
	acl.Severity = &nbdb.ACLSeverityDebug // TODO: FIX THIS LATER
	acl.Log = false                       // TODO: FIX THIS LATER
	acl.Priority = int(rulePriority)
	acl.Tier = types.DefaultBANPACLTier
	acl.Meter = utilpointer.String(types.OvnACLLoggingMeter) // TODO: FIX THIS LATER
	acl.ExternalIDs = map[string]string{
		libovsdbops.OwnerControllerKey.String(): "network-policy-controller",
		libovsdbops.ObjectNameKey.String():      banpName,
		libovsdbops.PriorityKey.String():        fmt.Sprintf("%d", rulePriority),
		libovsdbops.PolicyDirectionKey.String(): direction,
		libovsdbops.OwnerTypeKey.String():       "BaselineAdminNetworkPolicy",
		libovsdbops.PrimaryIDKey.String():       fmt.Sprintf("network-policy-controller:BaselineAdminNetworkPolicy:%s:%s:%d", banpName, direction, rulePriority),
	}
	acl.Name = utilpointer.String(fmt.Sprintf("%s_%s_%d", banpName, direction, rulePriority))
	acl.UUID = fmt.Sprintf("%s_%s_%d-%f-UUID", banpName, direction, rulePriority, rand.Float64())
	// determine ACL match
	pgHashName := util.HashForOVN(fmt.Sprintf("BaselineAdminNetworkPolicy_%s_%d", banpName, banpPriority))
	var l3PortMatch, matchDirection string
	if direction == anpovn.BANPIngressPrefix {
		acl.Direction = nbdb.ACLDirectionToLport
		l3PortMatch = fmt.Sprintf("(outport == @%s)", pgHashName)
		matchDirection = "src"
	} else {
		acl.Direction = nbdb.ACLDirectionFromLport
		acl.Options = map[string]string{
			"apply-after-lb": "true",
		}
		l3PortMatch = fmt.Sprintf("(inport == @%s)", pgHashName)
		matchDirection = "dst"
	}
	asIndex := anpovn.GetANPPeerAddrSetDbIDs(banpName, direction, fmt.Sprintf("%d", rulePriority), "network-policy-controller", libovsdbops.AddressSetBaselineAdminNetworkPolicy)
	asv4, asv6 := addressset.GetHashNamesForAS(asIndex)
	if config.IPv4Mode && config.IPv6Mode {
		acl.Match = fmt.Sprintf("((ip4.%s == $%s || ip6.%s == $%s)) && %s", matchDirection, asv4, matchDirection, asv6, l3PortMatch)
	} else if config.IPv4Mode {
		acl.Match = fmt.Sprintf("(ip4.%s == $%s) && %s", matchDirection, asv4, l3PortMatch)
	} else {
		acl.Match = fmt.Sprintf("(ip6.%s == $%s) && %s", matchDirection, asv6, l3PortMatch)
	}
	if ports != nil && len(*ports) != 0 {
		portACLs := []*nbdb.ACL{}
		for i, port := range *ports {
			aclCopy := acl
			aclCopy.ExternalIDs = make(map[string]string)
			for k, v := range acl.ExternalIDs {
				aclCopy.ExternalIDs[k] = v
			}
			var l4PortMatch string
			if port.PortNumber != nil && port.PortNumber.Port != 0 {
				protocol := strings.ToLower(string(port.PortNumber.Protocol))
				l4PortMatch = fmt.Sprintf("(%s && %s.dst==%d)", protocol, protocol, port.PortNumber.Port)
			} else if port.PortRange != nil && port.PortRange.End != 0 && port.PortRange.Start != port.PortRange.End {
				protocol := strings.ToLower(string(port.PortRange.Protocol))
				l4PortMatch = fmt.Sprintf("(%s && %d<=%s.dst<=%d)", protocol, port.PortRange.Start, protocol, port.PortRange.Start)
			}
			aclCopy.Match = fmt.Sprintf("%s && %s", acl.Match, l4PortMatch)
			aclCopy.Name = utilpointer.String(fmt.Sprintf("%s_%s_%d.%d", banpName, direction, rulePriority, i))
			aclCopy.UUID = fmt.Sprintf("%s_%s_%d.%d-%f-UUID", banpName, direction, rulePriority, i, rand.Float64())
			aclCopy.ExternalIDs[libovsdbops.PriorityKey.String()] = fmt.Sprintf("%d.%d", rulePriority, i)
			aclCopy.ExternalIDs[libovsdbops.PrimaryIDKey.String()] = fmt.Sprintf("network-policy-controller:BaselineAdminNetworkPolicy:%s:%s:%d.%d", banpName, direction, rulePriority, i)
			portACLs = append(portACLs, &aclCopy)
		}
		return portACLs
	}
	return []*nbdb.ACL{&acl}
}

func getACLsForBANPRules(banp *anpapi.BaselineAdminNetworkPolicy) []*nbdb.ACL {
	aclResults := []*nbdb.ACL{}
	for i, ingress := range banp.Spec.Ingress {
		acls := getBANPGressACL(ingress.Action, banp.Name, anpovn.BANPIngressPrefix, anpovn.BANPFlowPriority,
			getBANPRulePriority(int32(i)), ingress.Ports)
		aclResults = append(aclResults, acls...)
	}
	for i, egress := range banp.Spec.Egress {
		acls := getBANPGressACL(egress.Action, banp.Name, anpovn.BANPEgressPrefix, anpovn.BANPFlowPriority,
			getBANPRulePriority(int32(i)), egress.Ports)
		aclResults = append(aclResults, acls...)
	}
	return aclResults
}

var _ = ginkgo.Describe("OVN BANP Operations", func() {
	var (
		app     *cli.App
		fakeOVN *FakeOVN
	)

	const (
		banpSubjectNamespaceName = "banp-subject-namespace"
		banpPeerNamespaceName    = "banp-peer-namespace"
		banpSubjectPodName       = "banp-subject-pod"
		banpPodV4IP              = "10.128.1.3"
		banpPodMAC               = "0a:58:0a:80:01:03"
		banpPodV4IP2             = "10.128.1.4"
		banpPodMAC2              = "0a:58:0a:80:01:04"
		banpPeerPodName          = "anp-peer-pod"
		node1Name                = "node1"
	)

	ginkgo.BeforeEach(func() {
		// Restore global default values before each testcase
		config.PrepareTestConfig()
		config.OVNKubernetesFeature.EnableAdminNetworkPolicy = true

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

		fakeOVN = NewFakeOVN(false)
	})

	ginkgo.AfterEach(func() {
		fakeOVN.shutdown()
	})

	ginkgo.Context("on baseline admin network policy changes", func() {
		ginkgo.It("should create/update/delete address-sets, acls, port-groups correctly", func() {
			app.Action = func(ctx *cli.Context) error {
				banpNamespaceSubject := *newNamespaceWithLabels(banpSubjectNamespaceName, anpLabel)
				banpNamespacePeer := *newNamespaceWithLabels(banpPeerNamespaceName, peerDenyLabel)
				config.IPv4Mode = true
				config.IPv6Mode = true
				node1 := nodeFor(node1Name, "100.100.100.0", "fc00:f853:ccd:e793::1", "10.128.1.0/24", "fe00:10:128:1::/64")

				node1Switch := &nbdb.LogicalSwitch{
					Name: node1Name,
					UUID: node1Name + "-UUID",
				}
				subjectNSASIPv4, subjectNSASIPv6 := buildNamespaceAddressSets(banpSubjectNamespaceName, []net.IP{})
				peerNSASIPv4, peerNSASIPv6 := buildNamespaceAddressSets(banpPeerNamespaceName, []net.IP{})
				dbSetup := libovsdbtest.TestSetup{
					NBData: []libovsdbtest.TestData{
						node1Switch,
						subjectNSASIPv4,
						subjectNSASIPv6,
						peerNSASIPv4,
						peerNSASIPv6,
					},
				}
				fakeOVN.startWithDBSetup(dbSetup,
					&v1.NamespaceList{
						Items: []v1.Namespace{
							banpNamespaceSubject,
							banpNamespacePeer,
						},
					},
					&v1.NodeList{
						Items: []v1.Node{
							*node1,
						},
					},
				)

				err := fakeOVN.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOVN.controller.WatchPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOVN.InitAndRunANPController()
				ginkgo.By("0. creating a baseline admin network policy with 0 rules; check if port group is created for the subject")
				banpSubject := newANPSubjectObject(
					&metav1.LabelSelector{
						MatchLabels: banpLabel,
					},
					nil,
				)
				banp := newBANPObject("harry-potter", banpSubject, []anpapi.BaselineAdminNetworkPolicyIngressRule{}, []anpapi.BaselineAdminNetworkPolicyEgressRule{})
				banp.ResourceVersion = "1"
				banp, err = fakeOVN.fakeClient.ANPClient.PolicyV1alpha1().BaselineAdminNetworkPolicies().Create(context.TODO(), banp, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// TODO: Check if clone methods did the right thing - do the test in the admin_network_policy_package
				pg := getDefaultPGForBANPSubject(banp.Name, nil, nil)
				expectedDatabaseState := []libovsdbtest.TestData{node1Switch, subjectNSASIPv4, subjectNSASIPv6, peerNSASIPv4, peerNSASIPv6, pg}
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				ginkgo.By("1. creating a pod that will act as subject of baseline admin network policy; check if lsp is created")
				t := newTPod(
					node1Name,
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					banpSubjectPodName,
					banpPodV4IP,
					banpPodMAC,
					banpSubjectNamespaceName,
				)
				t.portName = util.GetLogicalPortName(t.namespace, t.podName)
				t.populateLogicalSwitchCache(fakeOVN, getLogicalSwitchUUID(fakeOVN.controller.nbClient, node1Name))
				banpSubjectPod := *newPod(banpSubjectNamespaceName, banpSubjectPodName, node1Name, banpPodV4IP)
				_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Pods(banpSubjectPod.Namespace).Create(context.TODO(), &banpSubjectPod, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				subjectNSASIPv4, subjectNSASIPv6 = buildNamespaceAddressSets(banpSubjectNamespaceName, []net.IP{testing.MustParseIP(t.podIP)})
				expectedDatabaseState = []libovsdbtest.TestData{pg, subjectNSASIPv4, subjectNSASIPv6, peerNSASIPv4, peerNSASIPv6}
				expectedDatabaseState = append(expectedDatabaseState, getExpectedDataPodsAndSwitches([]testPod{t}, []string{node1Name})...)
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				ginkgo.By("2. update the resource version and labels of the pod that matches the subject selector; check if lsp is added to port-group")
				// The pod takes some time to get created so the first add pod event for ANP does nothing as it waits for LSP to be created.
				// When ANP controller receives the pod update event, it will not trigger an update in unit testing since ResourceVersion
				// is not updated by the test, hence we need to trigger a manual update in the unit test framework that is accompanied by label update.
				// NOTE: This is not an issue in real deployments with actual kapi server.
				banpSubjectPod.ResourceVersion = "1"
				banpSubjectPod.Labels = anpLabel // pod is now in namespace {house: gryffindor} && pod {house: gryffindor}
				_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Pods(banpSubjectPod.Namespace).Update(context.TODO(), &banpSubjectPod, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				pg = getDefaultPGForBANPSubject(banp.Name, []string{t.portUUID}, nil)
				expectedDatabaseState[0] = pg // port should be added to the port group
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				ginkgo.By("3. update the BANP by adding one ingress rule; check if acl is created and added on the port-group")
				ingressRules := []anpapi.BaselineAdminNetworkPolicyIngressRule{
					{
						Name:   "gryffindor-don't-talk-to-slytherin",
						Action: anpapi.BaselineAdminNetworkPolicyRuleActionDeny,
						From: []anpapi.AdminNetworkPolicyPeer{
							{
								Namespaces: &anpapi.NamespacedPeer{
									NamespaceSelector: &metav1.LabelSelector{
										MatchLabels: peerDenyLabel,
									},
								},
							},
						},
					},
				}
				banp = newBANPObject("harry-potter", banpSubject, ingressRules, []anpapi.BaselineAdminNetworkPolicyEgressRule{})
				banp.ResourceVersion = "2"
				banp, err = fakeOVN.fakeClient.ANPClient.PolicyV1alpha1().BaselineAdminNetworkPolicies().Update(context.TODO(), banp, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				acls := getACLsForBANPRules(banp)
				pg = getDefaultPGForBANPSubject(banp.Name, []string{t.portUUID}, acls)
				expectedDatabaseState[0] = pg // acl should be added to the port group
				for _, acl := range acls {
					acl := acl
					expectedDatabaseState = append(expectedDatabaseState, acl)
				}
				asIndex := anpovn.GetANPPeerAddrSetDbIDs(banp.Name, anpovn.BANPIngressPrefix,
					fmt.Sprintf("%d", getBANPRulePriority(0)), "network-policy-controller", libovsdbops.AddressSetBaselineAdminNetworkPolicy)
				peerASIngressRule0v4, peerASIngressRule0v6 := addressset.GetDbObjsForAS(asIndex, []net.IP{}) // address-set will be empty since no pods match it yet
				expectedDatabaseState = append(expectedDatabaseState, peerASIngressRule0v4)
				expectedDatabaseState = append(expectedDatabaseState, peerASIngressRule0v6)
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				ginkgo.By("4. creating a pod that will act as peer of baseline admin network policy; check if lsp is created")
				t2 := newTPod(
					node1Name,
					"10.128.1.1/24",
					"10.128.1.2",
					"10.128.1.1",
					banpPeerPodName,
					banpPodV4IP2,
					banpPodMAC2,
					banpPeerNamespaceName,
				)
				t2.portName = util.GetLogicalPortName(t2.namespace, t2.podName)
				t2.populateLogicalSwitchCache(fakeOVN, getLogicalSwitchUUID(fakeOVN.controller.nbClient, node1Name))
				banpPeerPod := *newPod(banpPeerNamespaceName, banpPeerPodName, node1Name, banpPodV4IP2)
				_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Pods(banpPeerPod.Namespace).Create(context.TODO(), &banpPeerPod, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				peerNSASIPv4, peerNSASIPv6 = buildNamespaceAddressSets(banpPeerNamespaceName, []net.IP{testing.MustParseIP(t2.podIP)})
				expectedDatabaseState = []libovsdbtest.TestData{pg, subjectNSASIPv4, subjectNSASIPv6, peerNSASIPv4, peerNSASIPv6}
				for _, acl := range acls {
					acl := acl
					expectedDatabaseState = append(expectedDatabaseState, acl)
				}
				expectedDatabaseState = append(expectedDatabaseState, getExpectedDataPodsAndSwitches([]testPod{t, t2}, []string{node1Name})...)
				peerASIngressRule0v4, peerASIngressRule0v6 = addressset.GetDbObjsForAS(asIndex, []net.IP{}) // address-set will be empty since no pods match it
				expectedDatabaseState = append(expectedDatabaseState, peerASIngressRule0v4)
				expectedDatabaseState = append(expectedDatabaseState, peerASIngressRule0v6)
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				ginkgo.By("5. update the resource version and labels of the pod that matches the peer selector; check if IP is added to address-set")
				// The pod takes some time to get created so the first add pod event for ANP does nothing as it waits for LSP to be created.
				// When ANP controller receives the pod update event, it will not trigger an update in unit testing since ResourceVersion
				// is not updated by the test, hence we need to trigger a manual update in the unit test framework that is accompanied by label update.
				// NOTE: This is not an issue in real deployments with actual kapi server.
				banpPeerPod.ResourceVersion = "1"
				banpPeerPod.Labels = peerDenyLabel
				_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Pods(banpPeerPod.Namespace).Update(context.TODO(), &banpPeerPod, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				expectedDatabaseState[len(expectedDatabaseState)-2].(*nbdb.AddressSet).Addresses = []string{t2.podIP} // podIP should be added to v4 address-set
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				ginkgo.By("6. update the BANP by adding two more ingress rules with allow and deny actions; check if acls are created and added on the port-group")
				ingressRules = append(ingressRules,
					anpapi.BaselineAdminNetworkPolicyIngressRule{
						Name:   "gryffindor-talk-to-hufflepuff",
						Action: anpapi.BaselineAdminNetworkPolicyRuleActionAllow,
						From: []anpapi.AdminNetworkPolicyPeer{
							{
								Pods: &anpapi.NamespacedPodPeer{ // test different kind of peer expression
									Namespaces: anpapi.NamespacedPeer{
										NamespaceSelector: &metav1.LabelSelector{
											MatchLabels: peerAllowLabel,
										},
									},
									PodSelector: metav1.LabelSelector{
										MatchLabels: peerAllowLabel,
									},
								},
							},
						},
					},
					anpapi.BaselineAdminNetworkPolicyIngressRule{
						Name:   "gryffindor-deny-to-ravenclaw",
						Action: anpapi.BaselineAdminNetworkPolicyRuleActionDeny,
						From: []anpapi.AdminNetworkPolicyPeer{
							{
								Namespaces: &anpapi.NamespacedPeer{
									NamespaceSelector: &metav1.LabelSelector{
										MatchLabels: peerPassLabel,
									},
								},
							},
						},
						Ports: &[]anpapi.AdminNetworkPolicyPort{ // test different ports combination
							{
								PortNumber: &anpapi.Port{
									Protocol: v1.ProtocolTCP,
									Port:     int32(12345),
								},
							},
							{
								PortNumber: &anpapi.Port{
									Protocol: v1.ProtocolUDP,
									Port:     int32(12345),
								},
							},
							{
								PortNumber: &anpapi.Port{
									Protocol: v1.ProtocolSCTP,
									Port:     int32(12345),
								},
							},
						},
					},
				)
				banp = newBANPObject("harry-potter", banpSubject, ingressRules, []anpapi.BaselineAdminNetworkPolicyEgressRule{})
				banp.ResourceVersion = "3"
				banp, err = fakeOVN.fakeClient.ANPClient.PolicyV1alpha1().BaselineAdminNetworkPolicies().Update(context.TODO(), banp, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				acls = getACLsForBANPRules(banp)
				pg = getDefaultPGForBANPSubject(banp.Name, []string{t.portUUID}, acls)
				expectedDatabaseState = []libovsdbtest.TestData{pg, subjectNSASIPv4, subjectNSASIPv6, peerNSASIPv4, peerNSASIPv6}
				for _, acl := range acls {
					acl := acl
					expectedDatabaseState = append(expectedDatabaseState, acl)
				}
				expectedDatabaseState = append(expectedDatabaseState, getExpectedDataPodsAndSwitches([]testPod{t, t2}, []string{node1Name})...)
				peerASIngressRule0v4, peerASIngressRule0v6 = addressset.GetDbObjsForAS(asIndex, []net.IP{net.ParseIP(t2.podIP)}) // podIP should be added to v4 address-set
				expectedDatabaseState = append(expectedDatabaseState, peerASIngressRule0v4)
				expectedDatabaseState = append(expectedDatabaseState, peerASIngressRule0v6)
				asIndex = anpovn.GetANPPeerAddrSetDbIDs(banp.Name, anpovn.BANPIngressPrefix,
					fmt.Sprintf("%d", getBANPRulePriority(1)), "network-policy-controller", libovsdbops.AddressSetBaselineAdminNetworkPolicy)
				peerASIngressRule1v4, peerASIngressRule1v6 := addressset.GetDbObjsForAS(asIndex, []net.IP{}) // address-set will be empty since no pods match it yet
				expectedDatabaseState = append(expectedDatabaseState, peerASIngressRule1v4)
				expectedDatabaseState = append(expectedDatabaseState, peerASIngressRule1v6)
				asIndex = anpovn.GetANPPeerAddrSetDbIDs(banp.Name, anpovn.BANPIngressPrefix,
					fmt.Sprintf("%d", getBANPRulePriority(2)), "network-policy-controller", libovsdbops.AddressSetBaselineAdminNetworkPolicy)
				peerASIngressRule2v4, peerASIngressRule2v6 := addressset.GetDbObjsForAS(asIndex, []net.IP{}) // address-set will be empty since no pods match it yet
				expectedDatabaseState = append(expectedDatabaseState, peerASIngressRule2v4)
				expectedDatabaseState = append(expectedDatabaseState, peerASIngressRule2v6)
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				ginkgo.By("7. update the BANP by adding three egress rules with deny, allow and deny actions; check if acls are created and added on the port-group")
				egressRules := []anpapi.BaselineAdminNetworkPolicyEgressRule{
					{
						Name:   "slytherin-don't-talk-to-gryffindor",
						Action: anpapi.BaselineAdminNetworkPolicyRuleActionDeny,
						To: []anpapi.AdminNetworkPolicyPeer{
							{
								Namespaces: &anpapi.NamespacedPeer{
									NamespaceSelector: &metav1.LabelSelector{
										MatchLabels: peerDenyLabel,
									},
								},
							},
						},
					},
					{
						Name:   "hufflepuff-talk-to-gryffindor",
						Action: anpapi.BaselineAdminNetworkPolicyRuleActionAllow,
						To: []anpapi.AdminNetworkPolicyPeer{
							{
								Pods: &anpapi.NamespacedPodPeer{ // test different kind of peer expression
									Namespaces: anpapi.NamespacedPeer{
										NamespaceSelector: &metav1.LabelSelector{
											MatchExpressions: []metav1.LabelSelectorRequirement{
												{
													Key:      "house",
													Operator: metav1.LabelSelectorOpIn,
													Values:   []string{"slytherin", "hufflepuff"},
												},
											},
										},
									},
									PodSelector: metav1.LabelSelector{
										MatchLabels: peerAllowLabel,
									},
								},
							},
						},
					},
					{
						Name:   "ravenclaw-deny-to-gryffindor",
						Action: anpapi.BaselineAdminNetworkPolicyRuleActionDeny,
						To: []anpapi.AdminNetworkPolicyPeer{
							{
								Namespaces: &anpapi.NamespacedPeer{
									NamespaceSelector: &metav1.LabelSelector{
										MatchLabels: peerPassLabel,
									},
								},
							},
						},
						Ports: &[]anpapi.AdminNetworkPolicyPort{ // test different ports combination
							{
								PortNumber: &anpapi.Port{
									Protocol: v1.ProtocolTCP,
									Port:     int32(12345),
								},
							},
							{
								PortNumber: &anpapi.Port{
									Protocol: v1.ProtocolUDP,
									Port:     int32(12345),
								},
							},
							{
								PortNumber: &anpapi.Port{
									Protocol: v1.ProtocolSCTP,
									Port:     int32(12345),
								},
							},
						},
					},
				}
				banp = newBANPObject("harry-potter", banpSubject, ingressRules, egressRules)
				banp.ResourceVersion = "4"
				banp, err = fakeOVN.fakeClient.ANPClient.PolicyV1alpha1().BaselineAdminNetworkPolicies().Update(context.TODO(), banp, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				acls = getACLsForBANPRules(banp)
				pg = getDefaultPGForBANPSubject(banp.Name, []string{t.portUUID}, acls)
				expectedDatabaseState = []libovsdbtest.TestData{pg, subjectNSASIPv4, subjectNSASIPv6, peerNSASIPv4, peerNSASIPv6}
				for _, acl := range acls {
					acl := acl
					expectedDatabaseState = append(expectedDatabaseState, acl)
				}
				expectedDatabaseState = append(expectedDatabaseState, getExpectedDataPodsAndSwitches([]testPod{t, t2}, []string{node1Name})...)

				// ingressRule AddressSets - nothing has changed
				expectedDatabaseState = append(expectedDatabaseState, peerASIngressRule0v4)
				expectedDatabaseState = append(expectedDatabaseState, peerASIngressRule0v6)
				expectedDatabaseState = append(expectedDatabaseState, peerASIngressRule1v4)
				expectedDatabaseState = append(expectedDatabaseState, peerASIngressRule1v6)
				expectedDatabaseState = append(expectedDatabaseState, peerASIngressRule2v4)
				expectedDatabaseState = append(expectedDatabaseState, peerASIngressRule2v6)

				// egressRule AddressSets
				asIndex = anpovn.GetANPPeerAddrSetDbIDs(banp.Name, anpovn.BANPEgressPrefix,
					fmt.Sprintf("%d", getBANPRulePriority(0)), "network-policy-controller", libovsdbops.AddressSetBaselineAdminNetworkPolicy)
				peerASEgressRule0v4, peerASEgressRule0v6 := addressset.GetDbObjsForAS(asIndex, []net.IP{net.ParseIP(t2.podIP)}) // podIP should be added to v4 address-set
				expectedDatabaseState = append(expectedDatabaseState, peerASEgressRule0v4)
				expectedDatabaseState = append(expectedDatabaseState, peerASEgressRule0v6)
				asIndex = anpovn.GetANPPeerAddrSetDbIDs(banp.Name, anpovn.BANPEgressPrefix,
					fmt.Sprintf("%d", getBANPRulePriority(1)), "network-policy-controller", libovsdbops.AddressSetBaselineAdminNetworkPolicy)
				peerASEgressRule1v4, peerASEgressRule1v6 := addressset.GetDbObjsForAS(asIndex, []net.IP{}) // address-set will be empty since no pods match it yet
				expectedDatabaseState = append(expectedDatabaseState, peerASEgressRule1v4)
				expectedDatabaseState = append(expectedDatabaseState, peerASEgressRule1v6)
				asIndex = anpovn.GetANPPeerAddrSetDbIDs(banp.Name, anpovn.BANPEgressPrefix,
					fmt.Sprintf("%d", getBANPRulePriority(2)), "network-policy-controller", libovsdbops.AddressSetBaselineAdminNetworkPolicy)
				peerASEgressRule2v4, peerASEgressRule2v6 := addressset.GetDbObjsForAS(asIndex, []net.IP{}) // address-set will be empty since no pods match it yet
				expectedDatabaseState = append(expectedDatabaseState, peerASEgressRule2v4)
				expectedDatabaseState = append(expectedDatabaseState, peerASEgressRule2v6)
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				ginkgo.By("8. update the labels of the pod that matches the peer selector; check if IPs are updated in address-sets")
				banpPeerPod.ResourceVersion = "2"
				banpPeerPod.Labels = peerAllowLabel // pod is now in namespace {house: slytherin} && pod {house: hufflepuff}
				_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Pods(banpPeerPod.Namespace).Update(context.TODO(), &banpPeerPod, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
				// ingressRule AddressSets - podIP should stay in deny rule's address-set since deny rule matcheS only on nsSelector which has not changed
				// egressRule AddressSets - podIP should now be present in both deny and allow rule's address-sets
				expectedDatabaseState[len(expectedDatabaseState)-4].(*nbdb.AddressSet).Addresses = []string{t2.podIP} // podIP should be added to v4 allow address-set
				expectedDatabaseState[len(expectedDatabaseState)-6].(*nbdb.AddressSet).Addresses = []string{t2.podIP} // podIP should be added to v4 deny address-set
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				ginkgo.By("9. update the labels of the namespace that matches the peer selector; check if IPs are updated in address-sets")
				banpNamespacePeer.ResourceVersion = "2"
				banpNamespacePeer.Labels = peerPassLabel // pod is now in namespace {house: ravenclaw} && pod {house: hufflepuff}
				_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Namespaces().Update(context.TODO(), &banpNamespacePeer, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// ingressRule AddressSets - podIP should stay in only pass rule's address-set
				expectedDatabaseState[len(expectedDatabaseState)-8].(*nbdb.AddressSet).Addresses = []string{t2.podIP} // podIP should be added to v4 Pass address-set
				expectedDatabaseState[len(expectedDatabaseState)-10].(*nbdb.AddressSet).Addresses = []string{}        // podIP should be removed from allow rule AS
				expectedDatabaseState[len(expectedDatabaseState)-12].(*nbdb.AddressSet).Addresses = []string{}        // podIP should be removed from deny rule AS

				// egressRule AddressSets - podIP should stay in only pass rule's address-set
				expectedDatabaseState[len(expectedDatabaseState)-2].(*nbdb.AddressSet).Addresses = []string{t2.podIP} // podIP should be added to v4 Pass address-set
				expectedDatabaseState[len(expectedDatabaseState)-4].(*nbdb.AddressSet).Addresses = []string{}         // podIP should be removed from allow rule AS
				expectedDatabaseState[len(expectedDatabaseState)-6].(*nbdb.AddressSet).Addresses = []string{}         // podIP should be removed from deny rule AS

				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				ginkgo.By("10. update the subject of baseline admin network policy so that subject pod stops matching; check if port group is updated")
				banpSubject = newANPSubjectObject(
					&metav1.LabelSelector{ // test different match expression
						MatchLabels: anpLabel,
					},
					&metav1.LabelSelector{
						MatchLabels: peerDenyLabel, // subject pod won't match any more
					},
				)
				banp = newBANPObject("harry-potter", banpSubject, ingressRules, egressRules)
				banp.ResourceVersion = "5"
				banp, err = fakeOVN.fakeClient.ANPClient.PolicyV1alpha1().BaselineAdminNetworkPolicies().Update(context.TODO(), banp, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				pg = getDefaultPGForBANPSubject(banp.Name, nil, acls) // no ports in PG
				expectedDatabaseState[0] = pg
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				ginkgo.By("11. update the resource version and labels of the pod that matches the new subject selector; check if port group is updated")
				banpSubjectPod.ResourceVersion = "2"
				banpSubjectPod.Labels = peerDenyLabel // pod is now in namespace {house: gryffindor} && pod {house: slytherin}
				_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Pods(banpSubjectPod.Namespace).Update(context.TODO(), &banpSubjectPod, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				pg = getDefaultPGForBANPSubject(banp.Name, []string{t.portUUID}, acls) // port added back to PG
				expectedDatabaseState[0] = pg
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				ginkgo.By("12. update the labels of the namespace that matches the subject selector to stop matching; check if port group is updated")
				banpNamespaceSubject.ResourceVersion = "1"
				banpNamespaceSubject.Labels = peerPassLabel // pod is now in namespace {house: ravenclaw} && pod {house: slytherin}; stops matching
				_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Namespaces().Update(context.TODO(), &banpNamespaceSubject, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				pg = getDefaultPGForBANPSubject(banp.Name, nil, acls) // no ports in PG
				expectedDatabaseState[0] = pg
				// Coincidentally the subject pod also ends up matching the peer label for Pass rule, so let's add that IP to the AS
				expectedDatabaseState[len(expectedDatabaseState)-8].(*nbdb.AddressSet).Addresses = []string{t.podIP, t2.podIP} // podIP should be added to v4 Pass address-set
				expectedDatabaseState[len(expectedDatabaseState)-2].(*nbdb.AddressSet).Addresses = []string{t.podIP, t2.podIP} // podIP should be added to v4 Pass address-set
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				ginkgo.By("13. update the labels of the namespace that matches the subject selector to start matching; check if port group is updated")
				banpNamespaceSubject.ResourceVersion = "2"
				banpNamespaceSubject.Labels = anpLabel // pod is now in namespace {house: gryffindor} && pod {house: slytherin}; starts matching
				_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Namespaces().Update(context.TODO(), &banpNamespaceSubject, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				pg = getDefaultPGForBANPSubject(banp.Name, []string{t.portUUID}, acls) // no ports in PG
				expectedDatabaseState[0] = pg
				// Let us remove the IPs from the peer label AS as it stops matching
				expectedDatabaseState[len(expectedDatabaseState)-8].(*nbdb.AddressSet).Addresses = []string{t2.podIP} // podIP should be added to v4 Pass address-set
				expectedDatabaseState[len(expectedDatabaseState)-2].(*nbdb.AddressSet).Addresses = []string{t2.podIP} // podIP should be added to v4 Pass address-set
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				ginkgo.By("14. update the BANP by deleting 1 ingress rule & 1 egress rule; check if all objects are re-created correctly")
				banp.ResourceVersion = "6"
				banp.Spec.Ingress = []anpapi.BaselineAdminNetworkPolicyIngressRule{ingressRules[1], ingressRules[2]} // deny is gone
				banp.Spec.Egress = []anpapi.BaselineAdminNetworkPolicyEgressRule{egressRules[0], egressRules[2]}     // allow is gone
				banp, err = fakeOVN.fakeClient.ANPClient.PolicyV1alpha1().BaselineAdminNetworkPolicies().Update(context.TODO(), banp, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// TODO: test server does not garbage collect ACLs, so we just expect port groups to be updated for the old priority ACLs.
				// Both oldACLs and newACLs will be found in test server.
				newACLs := getACLsForBANPRules(banp)
				pg = getDefaultPGForBANPSubject(banp.Name, []string{t.portUUID}, newACLs) // only newACLs are hosted
				expectedDatabaseState = []libovsdbtest.TestData{pg, subjectNSASIPv4, subjectNSASIPv6, peerNSASIPv4, peerNSASIPv6}
				garbageCollectACLs := []*nbdb.ACL{acls[1], acls[2], acls[3], acls[4], acls[6], acls[7], acls[8], acls[9]} // ones that are stuck due to test server not being able to do garbage collection
				acls = append(garbageCollectACLs, newACLs...)
				for _, acl := range acls {
					acl := acl
					expectedDatabaseState = append(expectedDatabaseState, acl)
				}
				expectedDatabaseState = append(expectedDatabaseState, getExpectedDataPodsAndSwitches([]testPod{t, t2}, []string{node1Name})...)
				// ensure address-sets for the rules that were deleted are also gone (index 2 in the list)
				// ensure new address-sets have expected IPs
				asIndex = anpovn.GetANPPeerAddrSetDbIDs(banp.Name, anpovn.BANPIngressPrefix,
					fmt.Sprintf("%d", getBANPRulePriority(0)), "network-policy-controller", libovsdbops.AddressSetBaselineAdminNetworkPolicy)
				peerASIngressRule0v4, peerASIngressRule0v6 = addressset.GetDbObjsForAS(asIndex, []net.IP{}) // address-set will be empty since no pods match it yet
				expectedDatabaseState = append(expectedDatabaseState, peerASIngressRule0v4)
				expectedDatabaseState = append(expectedDatabaseState, peerASIngressRule0v6)
				asIndex = anpovn.GetANPPeerAddrSetDbIDs(banp.Name, anpovn.BANPIngressPrefix,
					fmt.Sprintf("%d", getBANPRulePriority(1)), "network-policy-controller", libovsdbops.AddressSetBaselineAdminNetworkPolicy)
				peerASIngressRule1v4, peerASIngressRule1v6 = addressset.GetDbObjsForAS(asIndex, []net.IP{net.ParseIP(t2.podIP)}) // podIP should be added to v4 Pass address-set
				expectedDatabaseState = append(expectedDatabaseState, peerASIngressRule1v4)
				expectedDatabaseState = append(expectedDatabaseState, peerASIngressRule1v6)
				asIndex = anpovn.GetANPPeerAddrSetDbIDs(banp.Name, anpovn.BANPEgressPrefix,
					fmt.Sprintf("%d", getBANPRulePriority(0)), "network-policy-controller", libovsdbops.AddressSetBaselineAdminNetworkPolicy)
				peerASEgressRule0v4, peerASEgressRule0v6 = addressset.GetDbObjsForAS(asIndex, []net.IP{}) // address-set will be empty since no pods match it yet
				expectedDatabaseState = append(expectedDatabaseState, peerASEgressRule0v4)
				expectedDatabaseState = append(expectedDatabaseState, peerASEgressRule0v6)
				asIndex = anpovn.GetANPPeerAddrSetDbIDs(banp.Name, anpovn.BANPEgressPrefix,
					fmt.Sprintf("%d", getBANPRulePriority(1)), "network-policy-controller", libovsdbops.AddressSetBaselineAdminNetworkPolicy)
				peerASEgressRule1v4, peerASEgressRule1v6 = addressset.GetDbObjsForAS(asIndex, []net.IP{net.ParseIP(t2.podIP)}) // podIP should be added to v4 Pass address-set
				expectedDatabaseState = append(expectedDatabaseState, peerASEgressRule1v4)
				expectedDatabaseState = append(expectedDatabaseState, peerASEgressRule1v6)
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				ginkgo.By("15. delete pod matching peer selector; check if IPs are updated in address-sets")
				banpPeerPod.ResourceVersion = "3"
				err = fakeOVN.fakeClient.KubeClient.CoreV1().Pods(banpPeerPod.Namespace).Delete(context.TODO(), banpPeerPod.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				pg = getDefaultPGForBANPSubject(banp.Name, []string{t.portUUID}, newACLs)                 // only newACLs are hosted
				peerNSASIPv4, peerNSASIPv6 = buildNamespaceAddressSets(banpPeerNamespaceName, []net.IP{}) // pod is gone from peer namespace address-set
				expectedDatabaseState = []libovsdbtest.TestData{pg, subjectNSASIPv4, subjectNSASIPv6, peerNSASIPv4, peerNSASIPv6}
				for _, acl := range acls {
					acl := acl
					expectedDatabaseState = append(expectedDatabaseState, acl)
				}
				expectedDatabaseState = append(expectedDatabaseState, getExpectedDataPodsAndSwitches([]testPod{t}, []string{node1Name})...)
				asIndex = anpovn.GetANPPeerAddrSetDbIDs(banp.Name, anpovn.BANPIngressPrefix,
					fmt.Sprintf("%d", getBANPRulePriority(1)), "network-policy-controller", libovsdbops.AddressSetBaselineAdminNetworkPolicy)
				peerASIngressRule1v4, peerASIngressRule1v6 = addressset.GetDbObjsForAS(asIndex, []net.IP{}) // address-set will be empty since no pods match it yet
				asIndex = anpovn.GetANPPeerAddrSetDbIDs(banp.Name, anpovn.BANPEgressPrefix,
					fmt.Sprintf("%d", getBANPRulePriority(1)), "network-policy-controller", libovsdbops.AddressSetBaselineAdminNetworkPolicy)
				peerASEgressRule1v4, peerASEgressRule1v6 = addressset.GetDbObjsForAS(asIndex, []net.IP{}) // address-set will be empty since no pods match it yet
				expectedDatabaseState = append(expectedDatabaseState, []libovsdbtest.TestData{peerASIngressRule0v4, peerASIngressRule0v6,
					peerASIngressRule1v4, peerASIngressRule1v6, peerASEgressRule0v4, peerASEgressRule0v6, peerASEgressRule1v4, peerASEgressRule1v6}...)
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				ginkgo.By("16. update the subject pod to go into completed state; check if port group is updated")
				banpSubjectPod.ResourceVersion = "3"
				banpSubjectPod.Status.Phase = kapi.PodSucceeded
				_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Pods(banpSubjectPod.Namespace).Update(context.TODO(), &banpSubjectPod, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				pg = getDefaultPGForBANPSubject(banp.Name, nil, newACLs) // no ports in PG
				subjectNSASIPv4, subjectNSASIPv6 = buildNamespaceAddressSets(banpSubjectNamespaceName, []net.IP{})
				expectedDatabaseState = []libovsdbtest.TestData{pg, subjectNSASIPv4, subjectNSASIPv6, peerNSASIPv4, peerNSASIPv6}
				for _, acl := range acls {
					acl := acl
					expectedDatabaseState = append(expectedDatabaseState, acl)
				}
				expectedDatabaseState = append(expectedDatabaseState, getExpectedDataPodsAndSwitches([]testPod{}, []string{node1Name})...)
				expectedDatabaseState = append(expectedDatabaseState, []libovsdbtest.TestData{peerASIngressRule0v4, peerASIngressRule0v6, peerASIngressRule1v4,
					peerASIngressRule1v6, peerASEgressRule0v4, peerASEgressRule0v6, peerASEgressRule1v4, peerASEgressRule1v6}...)
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
				err = fakeOVN.fakeClient.KubeClient.CoreV1().Pods(banpSubjectPod.Namespace).Delete(context.TODO(), banpSubjectPod.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				ginkgo.By("17. delete the subject and peer selected namespaces; check if port group and address-set's are updated")
				// create a new pod in subject and peer namespaces so that we can check namespace deletion properly
				banpSubjectPod = *newPodWithLabels(banpSubjectNamespaceName, banpSubjectPodName, node1Name, "10.128.1.5", peerDenyLabel)
				_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Pods(banpSubjectPod.Namespace).Create(context.TODO(), &banpSubjectPod, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				t.podIP = "10.128.1.5"
				t.podMAC = "0a:58:0a:80:01:05"
				pg = getDefaultPGForBANPSubject(banp.Name, []string{t.portUUID}, newACLs)
				subjectNSASIPv4, subjectNSASIPv6 = buildNamespaceAddressSets(banpSubjectNamespaceName, []net.IP{testing.MustParseIP(t.podIP)})
				expectedDatabaseState = []libovsdbtest.TestData{pg, subjectNSASIPv4, subjectNSASIPv6, peerNSASIPv4, peerNSASIPv6}
				for _, acl := range acls {
					acl := acl
					expectedDatabaseState = append(expectedDatabaseState, acl)
				}
				expectedDatabaseState = append(expectedDatabaseState, getExpectedDataPodsAndSwitches([]testPod{t}, []string{node1Name})...)
				expectedDatabaseState = append(expectedDatabaseState, []libovsdbtest.TestData{peerASIngressRule0v4, peerASIngressRule0v6, peerASIngressRule1v4,
					peerASIngressRule1v6, peerASEgressRule0v4, peerASEgressRule0v6, peerASEgressRule1v4, peerASEgressRule1v6}...)
				gomega.Eventually(fakeOVN.nbClient, "3s").Should(libovsdbtest.HaveData(expectedDatabaseState))
				banpPeerPod = *newPodWithLabels(banpPeerNamespaceName, banpPeerPodName, node1Name, "10.128.1.6", peerAllowLabel)
				_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Pods(banpPeerPod.Namespace).Create(context.TODO(), &banpPeerPod, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				t2.podIP = "10.128.1.6"
				t2.podMAC = "0a:58:0a:80:01:06"
				peerNSASIPv4, peerNSASIPv6 = buildNamespaceAddressSets(banpPeerNamespaceName, []net.IP{testing.MustParseIP(t2.podIP)})
				expectedDatabaseState = []libovsdbtest.TestData{pg, subjectNSASIPv4, subjectNSASIPv6, peerNSASIPv4, peerNSASIPv6}
				for _, acl := range acls {
					acl := acl
					expectedDatabaseState = append(expectedDatabaseState, acl)
				}
				expectedDatabaseState = append(expectedDatabaseState, getExpectedDataPodsAndSwitches([]testPod{t, t2}, []string{node1Name})...)
				asIndex = anpovn.GetANPPeerAddrSetDbIDs(banp.Name, anpovn.BANPIngressPrefix,
					fmt.Sprintf("%d", getBANPRulePriority(1)), "network-policy-controller", libovsdbops.AddressSetBaselineAdminNetworkPolicy)
				peerASIngressRule1v4, peerASIngressRule1v6 = addressset.GetDbObjsForAS(asIndex, []net.IP{net.ParseIP(t2.podIP)})
				asIndex = anpovn.GetANPPeerAddrSetDbIDs(banp.Name, anpovn.BANPEgressPrefix,
					fmt.Sprintf("%d", getBANPRulePriority(1)), "network-policy-controller", libovsdbops.AddressSetBaselineAdminNetworkPolicy)
				peerASEgressRule1v4, peerASEgressRule1v6 = addressset.GetDbObjsForAS(asIndex, []net.IP{net.ParseIP(t2.podIP)})
				expectedDatabaseState = append(expectedDatabaseState, []libovsdbtest.TestData{peerASIngressRule0v4, peerASIngressRule0v6, peerASIngressRule1v4,
					peerASIngressRule1v6, peerASEgressRule0v4, peerASEgressRule0v6, peerASEgressRule1v4, peerASEgressRule1v6}...)
				gomega.Eventually(fakeOVN.nbClient, "3s").Should(libovsdbtest.HaveData(expectedDatabaseState))

				// now delete the namespaces
				banpNamespaceSubject.ResourceVersion = "3"
				err = fakeOVN.fakeClient.KubeClient.CoreV1().Namespaces().Delete(context.TODO(), banpNamespaceSubject.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				banpNamespacePeer.ResourceVersion = "3"
				err = fakeOVN.fakeClient.KubeClient.CoreV1().Namespaces().Delete(context.TODO(), banpNamespacePeer.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				pg = getDefaultPGForBANPSubject(banp.Name, nil, newACLs) // no ports in PG
				expectedDatabaseState = []libovsdbtest.TestData{pg}      // namespace address-sets are gone
				for _, acl := range acls {
					acl := acl
					expectedDatabaseState = append(expectedDatabaseState, acl)
				}
				// delete namespace in unit testing doesn't delete pods; we just need to check port groups and address-sets are updated
				expectedDatabaseState = append(expectedDatabaseState, getExpectedDataPodsAndSwitches([]testPod{t, t2}, []string{node1Name})...)
				asIndex = anpovn.GetANPPeerAddrSetDbIDs(banp.Name, anpovn.BANPIngressPrefix,
					fmt.Sprintf("%d", getBANPRulePriority(1)), "network-policy-controller", libovsdbops.AddressSetBaselineAdminNetworkPolicy)
				peerASIngressRule1v4, peerASIngressRule1v6 = addressset.GetDbObjsForAS(asIndex, []net.IP{}) // address-set will be empty since no pods match it yet
				asIndex = anpovn.GetANPPeerAddrSetDbIDs(banp.Name, anpovn.BANPEgressPrefix,
					fmt.Sprintf("%d", getBANPRulePriority(1)), "network-policy-controller", libovsdbops.AddressSetBaselineAdminNetworkPolicy)
				peerASEgressRule1v4, peerASEgressRule1v6 = addressset.GetDbObjsForAS(asIndex, []net.IP{})
				expectedDatabaseState = append(expectedDatabaseState, []libovsdbtest.TestData{peerASIngressRule0v4, peerASIngressRule0v6, peerASIngressRule1v4,
					peerASIngressRule1v6, peerASEgressRule0v4, peerASEgressRule0v6, peerASEgressRule1v4, peerASEgressRule1v6}...)
				// NOTE: Address set deletion is deferred for 20 seconds...
				gomega.Eventually(fakeOVN.nbClient, "25s").Should(libovsdbtest.HaveData(expectedDatabaseState))

				ginkgo.By("18. update the BANP by deleting all rules; check if all objects are re-created correctly")
				banp.Spec.Ingress = []anpapi.BaselineAdminNetworkPolicyIngressRule{}
				banp.Spec.Egress = []anpapi.BaselineAdminNetworkPolicyEgressRule{}
				banp.ResourceVersion = "7"
				banp, err = fakeOVN.fakeClient.ANPClient.PolicyV1alpha1().BaselineAdminNetworkPolicies().Update(context.TODO(), banp, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				pg = getDefaultPGForBANPSubject(banp.Name, nil, nil) // no ports and acls in PG
				expectedDatabaseState = []libovsdbtest.TestData{pg}
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// at this point all acls are for garbage collection
				for _, acl := range acls {
					acl := acl
					expectedDatabaseState = append(expectedDatabaseState, acl)
				}
				expectedDatabaseState = append(expectedDatabaseState, getExpectedDataPodsAndSwitches([]testPod{t, t2}, []string{node1Name})...)
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				ginkgo.By("19. delete the BANP; check if all objects are re-created correctly")
				banp.ResourceVersion = "8"
				err = fakeOVN.fakeClient.ANPClient.PolicyV1alpha1().BaselineAdminNetworkPolicies().Delete(context.TODO(), banp.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				expectedDatabaseState = []libovsdbtest.TestData{} // port group should be deleted
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// at this point all acls are for garbage collection
				for _, acl := range acls {
					acl := acl
					expectedDatabaseState = append(expectedDatabaseState, acl)
				}
				expectedDatabaseState = append(expectedDatabaseState, getExpectedDataPodsAndSwitches([]testPod{t, t2}, []string{node1Name})...)
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
		})
	})
})
