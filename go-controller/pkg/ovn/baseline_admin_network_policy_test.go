package ovn

import (
	"context"
	"fmt"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	libovsdbutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	anpovn "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/controller/admin_network_policy"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/urfave/cli/v2"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/klog/v2"
	utilpointer "k8s.io/utils/pointer"
	"k8s.io/utils/ptr"
	anpapi "sigs.k8s.io/network-policy-api/apis/v1alpha1"
	anpfake "sigs.k8s.io/network-policy-api/pkg/client/clientset/versioned/fake"
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

func getACLsForBANPRulesWithNamedPorts(banp *anpapi.BaselineAdminNetworkPolicy, namedIPorts, namedEPorts map[string][]libovsdbutil.NamedNetworkPolicyPort) []*nbdb.ACL {
	aclResults := []*nbdb.ACL{}
	for i, ingress := range banp.Spec.Ingress {
		acls := getANPGressACL(anpovn.GetACLActionForBANPRule(ingress.Action), banp.Name, string(libovsdbutil.ACLIngress),
			getBANPRulePriority(int32(i)), int32(i), ingress.Ports, namedIPorts, true)
		aclResults = append(aclResults, acls...)
	}
	for i, egress := range banp.Spec.Egress {
		acls := getANPGressACL(anpovn.GetACLActionForBANPRule(egress.Action), banp.Name, string(libovsdbutil.ACLEgress),
			getBANPRulePriority(int32(i)), int32(i), egress.Ports, namedEPorts, true)
		aclResults = append(aclResults, acls...)
	}
	return aclResults
}

func getACLsForBANPRules(banp *anpapi.BaselineAdminNetworkPolicy) []*nbdb.ACL {
	aclResults := []*nbdb.ACL{}
	for i, ingress := range banp.Spec.Ingress {
		acls := getANPGressACL(anpovn.GetACLActionForBANPRule(ingress.Action), banp.Name, string(libovsdbutil.ACLIngress),
			getBANPRulePriority(int32(i)), int32(i), ingress.Ports, nil, true)
		aclResults = append(aclResults, acls...)
	}
	for i, egress := range banp.Spec.Egress {
		acls := getANPGressACL(anpovn.GetACLActionForBANPRule(egress.Action), banp.Name, string(libovsdbutil.ACLEgress),
			getBANPRulePriority(int32(i)), int32(i), egress.Ports, nil, true)
		aclResults = append(aclResults, acls...)
	}
	return aclResults
}

func buildBANPAddressSets(banp *anpapi.BaselineAdminNetworkPolicy, index int32, ips []string, gressPrefix libovsdbutil.ACLDirection) (*nbdb.AddressSet, *nbdb.AddressSet) {
	asIndex := anpovn.GetANPPeerAddrSetDbIDs(banp.Name, string(gressPrefix),
		fmt.Sprintf("%d", index), DefaultNetworkControllerName, true)
	return addressset.GetTestDbAddrSets(asIndex, ips)
}

var _ = ginkgo.Describe("OVN BANP Operations", func() {
	var (
		app     *cli.App
		fakeOVN *FakeOVN
	)

	const (
		banpSubjectNamespaceName        = "banp-subject-namespace"
		banpPeerNamespaceName           = "banp-peer-namespace"
		banpSubjectPodName              = "banp-subject-pod"
		banpPodV4IP                     = "10.128.1.3"
		banpPodV6IP                     = "fe00:10:128:1::3"
		banpPodMAC                      = "0a:58:0a:80:01:03"
		banpPodV4IP2                    = "10.128.1.4"
		banpPodV6IP2                    = "fe00:10:128:1::4"
		banpPodMAC2                     = "0a:58:0a:80:01:04"
		banpPeerPodName                 = "banp-peer-pod"
		node1Name                string = "node1"
		node1IPv4                string = "100.100.100.0"
		node1IPv6                string = "fc00:f853:ccd:e793::1"
		node1IPv4Subnet          string = "10.128.1.0/24"
		node1IPv6Subnet          string = "fe00:10:128:1::/64"
		node1transitIPv4         string = "100.88.0.2"
		node1transitIPv6         string = "fd97::2"
		node2Name                string = "node2"
		node2IPv4                string = "200.200.200.0"
		node2IPv6                string = "fc00:f853:ccd:e793::2"
		node2IPv4Subnet          string = "10.128.2.0/24"
		node2IPv6Subnet          string = "fe00:10:128:2::/64"
		node2transitIPv4         string = "100.88.0.3"
		node2transitIPv6         string = "fd97::3"
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
				node1 := nodeFor(node1Name, "100.100.100.0", "fc00:f853:ccd:e793::1", "10.128.1.0/24", "fe00:10:128:1::/64", "", "")

				node1Switch := &nbdb.LogicalSwitch{
					Name: node1Name,
					UUID: node1Name + "-UUID",
				}
				subjectNSASIPv4, subjectNSASIPv6 := buildNamespaceAddressSets(banpSubjectNamespaceName, []string{})
				peerNSASIPv4, peerNSASIPv6 := buildNamespaceAddressSets(banpPeerNamespaceName, []string{})
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

				fakeOVN.controller.zone = node1Name
				err := fakeOVN.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOVN.controller.WatchPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOVN.InitAndRunANPController()
				fakeOVN.fakeClient.ANPClient.(*anpfake.Clientset).PrependReactor("update", "baselineadminnetworkpolicies", func(action clienttesting.Action) (handled bool, ret runtime.Object, err error) {
					update := action.(clienttesting.UpdateAction)
					// Since fake client (NewSimpleClientset) does not differentiate between
					// an update and updatestatus, updatestatus in tests updates the spec as
					// well causing race conditions. Thus adding a hack here to ensure update
					// status is caught and processed by the reactor while update spec is
					// delegated to the main code for handling
					if action.GetSubresource() == "status" {
						klog.Infof("Got an update status action for %v", update.GetObject())
						return true, update.GetObject(), nil
					}
					klog.Infof("Got an update spec action for %v", update.GetObject())
					return false, update.GetObject(), nil
				})
				ginkgo.By("1. creating a baseline admin network policy with 0 rules; check if port group is created for the subject")
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
				pg := getDefaultPGForANPSubject(banp.Name, nil, nil, true)
				expectedDatabaseState := []libovsdbtest.TestData{node1Switch, subjectNSASIPv4, subjectNSASIPv6, peerNSASIPv4, peerNSASIPv6, pg}
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				ginkgo.By("2. creating a pod that will act as subject of baseline admin network policy; check if lsp is added to port-group after retries")
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
				t.populateLogicalSwitchCache(fakeOVN)
				banpSubjectPod := *newPod(banpSubjectNamespaceName, banpSubjectPodName, node1Name, banpPodV4IP)
				_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Pods(banpSubjectPod.Namespace).Create(context.TODO(), &banpSubjectPod, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// The pod takes some time to get created so the first add pod event for ANP does nothing as it waits for LSP to be created - this triggers a retry.
				// Next when the pod is scheduled and IP is allocated and LSP is created we get an update event.
				// When ANP controller receives the pod update event, it will not trigger an update in unit testing since ResourceVersion and pod.status.Running
				// is not updated by the test, hence we need to trigger a manual update in the unit test framework that is accompanied by label update.
				// NOTE: This is not an issue in real deployments with actual kapi server; its just a potential flake in the UT framework that we need to protect against.
				time.Sleep(time.Second)
				banpSubjectPod.ResourceVersion = "500"
				// - this is to trigger an actual update technically speaking the pod already matches the namespace of the subject
				banpSubjectPod.Labels["rv"] = "resourceVersionUTHack"
				_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Pods(banpSubjectPod.Namespace).Update(context.TODO(), &banpSubjectPod, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				subjectNSASIPv4, subjectNSASIPv6 = buildNamespaceAddressSets(banpSubjectNamespaceName, []string{t.podIP})
				pg = getDefaultPGForANPSubject(banp.Name, []string{t.portUUID}, nil, true)
				expectedDatabaseState = []libovsdbtest.TestData{pg, subjectNSASIPv4, subjectNSASIPv6, peerNSASIPv4, peerNSASIPv6}
				expectedDatabaseState = append(expectedDatabaseState, getDefaultNetExpectedPodsAndSwitches([]testPod{t}, []string{node1Name})...)
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				ginkgo.By("3. update the BANP by adding one ingress rule; check if acl is created and added on the port-group")
				ingressRules := []anpapi.BaselineAdminNetworkPolicyIngressRule{
					{
						Name:   "gryffindor-don't-talk-to-slytherin",
						Action: anpapi.BaselineAdminNetworkPolicyRuleActionDeny,
						From: []anpapi.AdminNetworkPolicyIngressPeer{
							{
								Namespaces: &metav1.LabelSelector{
									MatchLabels: peerDenyLabel,
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
				pg = getDefaultPGForANPSubject(banp.Name, []string{t.portUUID}, acls, true)
				expectedDatabaseState[0] = pg // acl should be added to the port group
				for _, acl := range acls {
					acl := acl
					expectedDatabaseState = append(expectedDatabaseState, acl)
				}
				peerASIngressRule0v4, peerASIngressRule0v6 := buildBANPAddressSets(banp, 0, []string{}, libovsdbutil.ACLIngress)
				expectedDatabaseState = append(expectedDatabaseState, peerASIngressRule0v4)
				expectedDatabaseState = append(expectedDatabaseState, peerASIngressRule0v6)
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				ginkgo.By("4. creating a pod that will act as peer of baseline admin network policy; check if pod ip is added to address-set")
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
				t2.populateLogicalSwitchCache(fakeOVN)
				banpPeerPod := *newPod(banpPeerNamespaceName, banpPeerPodName, node1Name, banpPodV4IP2)
				_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Pods(banpPeerPod.Namespace).Create(context.TODO(), &banpPeerPod, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// The pod takes some time to get created so the first add pod event for ANP does nothing as it waits for LSP to be created - this triggers a retry.
				// Next when the pod is scheduled and IP is allocated and LSP is created we get an update event.
				// When ANP controller receives the pod update event, it will not trigger an update in unit testing since ResourceVersion and pod.status.Running
				// is not updated by the test, hence we need to trigger a manual update in the unit test framework that is accompanied by label update.
				// NOTE: This is not an issue in real deployments with actual kapi server; its just a potential flake in the UT framework that we need to protect against.
				banpPeerPod.ResourceVersion = "500"
				// - this is to trigger an actual update technically speaking the pod already matches the namespace of the subject
				banpPeerPod.Labels["rv"] = "resourceVersionUTHack"
				_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Pods(banpPeerPod.Namespace).Update(context.TODO(), &banpPeerPod, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				peerNSASIPv4, peerNSASIPv6 = buildNamespaceAddressSets(banpPeerNamespaceName, []string{t2.podIP})
				expectedDatabaseState = []libovsdbtest.TestData{pg, subjectNSASIPv4, subjectNSASIPv6, peerNSASIPv4, peerNSASIPv6}
				for _, acl := range acls {
					acl := acl
					expectedDatabaseState = append(expectedDatabaseState, acl)
				}
				expectedDatabaseState = append(expectedDatabaseState, getDefaultNetExpectedPodsAndSwitches([]testPod{t, t2}, []string{node1Name})...)
				peerASIngressRule0v4, peerASIngressRule0v6 = buildBANPAddressSets(banp, 0, []string{t2.podIP}, libovsdbutil.ACLIngress) // podIP should be added to v4 address-set
				expectedDatabaseState = append(expectedDatabaseState, peerASIngressRule0v4)
				expectedDatabaseState = append(expectedDatabaseState, peerASIngressRule0v6)
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				ginkgo.By("5. update the BANP by adding two more ingress rules with allow and deny actions; check if acls are created and added on the port-group")
				ingressRules = append(ingressRules,
					anpapi.BaselineAdminNetworkPolicyIngressRule{
						Name:   "gryffindor-talk-to-hufflepuff",
						Action: anpapi.BaselineAdminNetworkPolicyRuleActionAllow,
						From: []anpapi.AdminNetworkPolicyIngressPeer{
							{
								Pods: &anpapi.NamespacedPod{ // test different kind of peer expression
									NamespaceSelector: metav1.LabelSelector{
										MatchLabels: peerAllowLabel,
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
						From: []anpapi.AdminNetworkPolicyIngressPeer{
							{
								Namespaces: &metav1.LabelSelector{
									MatchLabels: peerPassLabel,
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
				pg = getDefaultPGForANPSubject(banp.Name, []string{t.portUUID}, acls, true)
				expectedDatabaseState = []libovsdbtest.TestData{pg, subjectNSASIPv4, subjectNSASIPv6, peerNSASIPv4, peerNSASIPv6}
				for _, acl := range acls {
					acl := acl
					expectedDatabaseState = append(expectedDatabaseState, acl)
				}
				expectedDatabaseState = append(expectedDatabaseState, getDefaultNetExpectedPodsAndSwitches([]testPod{t, t2}, []string{node1Name})...)
				// podIP should be added to v4 address-set
				peerASIngressRule0v4, peerASIngressRule0v6 = buildBANPAddressSets(banp, 0, []string{t2.podIP}, libovsdbutil.ACLIngress)
				expectedDatabaseState = append(expectedDatabaseState, peerASIngressRule0v4)
				expectedDatabaseState = append(expectedDatabaseState, peerASIngressRule0v6)
				// address-set will be empty since no pods match it yet
				peerASIngressRule1v4, peerASIngressRule1v6 := buildBANPAddressSets(banp, 1, []string{}, libovsdbutil.ACLIngress)
				expectedDatabaseState = append(expectedDatabaseState, peerASIngressRule1v4)
				expectedDatabaseState = append(expectedDatabaseState, peerASIngressRule1v6)
				// address-set will be empty since no pods match it yet
				peerASIngressRule2v4, peerASIngressRule2v6 := buildBANPAddressSets(banp, 2, []string{}, libovsdbutil.ACLIngress)
				expectedDatabaseState = append(expectedDatabaseState, peerASIngressRule2v4)
				expectedDatabaseState = append(expectedDatabaseState, peerASIngressRule2v6)
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				ginkgo.By("6. update the BANP by adding three egress rules with deny, allow and deny actions; check if acls are created and added on the port-group")
				egressRules := []anpapi.BaselineAdminNetworkPolicyEgressRule{
					{
						Name:   "slytherin-don't-talk-to-gryffindor",
						Action: anpapi.BaselineAdminNetworkPolicyRuleActionDeny,
						To: []anpapi.AdminNetworkPolicyEgressPeer{
							{
								Namespaces: &metav1.LabelSelector{
									MatchLabels: peerDenyLabel,
								},
							},
						},
					},
					{
						Name:   "hufflepuff-talk-to-gryffindor",
						Action: anpapi.BaselineAdminNetworkPolicyRuleActionAllow,
						To: []anpapi.AdminNetworkPolicyEgressPeer{
							{
								Pods: &anpapi.NamespacedPod{ // test different kind of peer expression
									NamespaceSelector: metav1.LabelSelector{
										MatchExpressions: []metav1.LabelSelectorRequirement{
											{
												Key:      "house",
												Operator: metav1.LabelSelectorOpIn,
												Values:   []string{"slytherin", "hufflepuff"},
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
						To: []anpapi.AdminNetworkPolicyEgressPeer{
							{
								Namespaces: &metav1.LabelSelector{
									MatchLabels: peerPassLabel,
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
				pg = getDefaultPGForANPSubject(banp.Name, []string{t.portUUID}, acls, true)
				expectedDatabaseState = []libovsdbtest.TestData{pg, subjectNSASIPv4, subjectNSASIPv6, peerNSASIPv4, peerNSASIPv6}
				for _, acl := range acls {
					acl := acl
					expectedDatabaseState = append(expectedDatabaseState, acl)
				}
				expectedDatabaseState = append(expectedDatabaseState, getDefaultNetExpectedPodsAndSwitches([]testPod{t, t2}, []string{node1Name})...)

				// ingressRule AddressSets - nothing has changed
				expectedDatabaseState = append(expectedDatabaseState, peerASIngressRule0v4)
				expectedDatabaseState = append(expectedDatabaseState, peerASIngressRule0v6)
				expectedDatabaseState = append(expectedDatabaseState, peerASIngressRule1v4)
				expectedDatabaseState = append(expectedDatabaseState, peerASIngressRule1v6)
				expectedDatabaseState = append(expectedDatabaseState, peerASIngressRule2v4)
				expectedDatabaseState = append(expectedDatabaseState, peerASIngressRule2v6)

				// egressRule AddressSets
				// address-set will be empty since no pods match it yet
				// podIP should be added to v4 address-set
				peerASEgressRule0v4, peerASEgressRule0v6 := buildBANPAddressSets(banp, 0, []string{t2.podIP}, libovsdbutil.ACLEgress)
				expectedDatabaseState = append(expectedDatabaseState, peerASEgressRule0v4)
				expectedDatabaseState = append(expectedDatabaseState, peerASEgressRule0v6)
				// address-set will be empty since no pods match it yet
				peerASEgressRule1v4, peerASEgressRule1v6 := buildBANPAddressSets(banp, 1, []string{}, libovsdbutil.ACLEgress)
				expectedDatabaseState = append(expectedDatabaseState, peerASEgressRule1v4)
				expectedDatabaseState = append(expectedDatabaseState, peerASEgressRule1v6)
				// address-set will be empty since no pods match it yet
				peerASEgressRule2v4, peerASEgressRule2v6 := buildBANPAddressSets(banp, 2, []string{}, libovsdbutil.ACLEgress)
				expectedDatabaseState = append(expectedDatabaseState, peerASEgressRule2v4)
				expectedDatabaseState = append(expectedDatabaseState, peerASEgressRule2v6)
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				ginkgo.By("7. update the labels of the pod that matches the peer selector; check if IPs are updated in address-sets")
				banpPeerPod.ResourceVersion = "2"
				banpPeerPod.Labels = peerAllowLabel // pod is now in namespace {house: slytherin} && pod {house: hufflepuff}
				_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Pods(banpPeerPod.Namespace).Update(context.TODO(), &banpPeerPod, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// ingressRule AddressSets - podIP should stay in deny rule's address-set since deny rule matcheS only on nsSelector which has not changed
				// egressRule AddressSets - podIP should now be present in both deny and allow rule's address-sets
				expectedDatabaseState[len(expectedDatabaseState)-4].(*nbdb.AddressSet).Addresses = []string{t2.podIP} // podIP should be added to v4 allow address-set
				expectedDatabaseState[len(expectedDatabaseState)-6].(*nbdb.AddressSet).Addresses = []string{t2.podIP} // podIP should be added to v4 deny address-set
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				ginkgo.By("8. update the labels of the namespace that matches the peer selector; check if IPs are updated in address-sets")
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

				ginkgo.By("9. update the subject of baseline admin network policy so that subject pod stops matching; check if port group is updated")
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
				pg = getDefaultPGForANPSubject(banp.Name, nil, acls, true) // no ports in PG
				expectedDatabaseState[0] = pg
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				ginkgo.By("10. update the resource version and labels of the pod that matches the new subject selector; check if port group is updated")
				banpSubjectPod.ResourceVersion = "2"
				banpSubjectPod.Labels = peerDenyLabel // pod is now in namespace {house: gryffindor} && pod {house: slytherin}
				_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Pods(banpSubjectPod.Namespace).Update(context.TODO(), &banpSubjectPod, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				pg = getDefaultPGForANPSubject(banp.Name, []string{t.portUUID}, acls, true) // port added back to PG
				expectedDatabaseState[0] = pg
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				ginkgo.By("11. update the labels of the namespace that matches the subject selector to stop matching; check if port group is updated")
				banpNamespaceSubject.ResourceVersion = "1"
				banpNamespaceSubject.Labels = peerPassLabel // pod is now in namespace {house: ravenclaw} && pod {house: slytherin}; stops matching
				_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Namespaces().Update(context.TODO(), &banpNamespaceSubject, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				pg = getDefaultPGForANPSubject(banp.Name, nil, acls, true) // no ports in PG
				expectedDatabaseState[0] = pg
				// Coincidentally the subject pod also ends up matching the peer label for Pass rule, so let's add that IP to the AS
				expectedDatabaseState[len(expectedDatabaseState)-8].(*nbdb.AddressSet).Addresses = []string{t.podIP, t2.podIP} // podIP should be added to v4 Pass address-set
				expectedDatabaseState[len(expectedDatabaseState)-2].(*nbdb.AddressSet).Addresses = []string{t.podIP, t2.podIP} // podIP should be added to v4 Pass address-set
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				ginkgo.By("12. update the labels of the namespace that matches the subject selector to start matching; check if port group is updated")
				banpNamespaceSubject.ResourceVersion = "2"
				banpNamespaceSubject.Labels = anpLabel // pod is now in namespace {house: gryffindor} && pod {house: slytherin}; starts matching
				_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Namespaces().Update(context.TODO(), &banpNamespaceSubject, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				pg = getDefaultPGForANPSubject(banp.Name, []string{t.portUUID}, acls, true) // no ports in PG
				expectedDatabaseState[0] = pg
				// Let us remove the IPs from the peer label AS as it stops matching
				expectedDatabaseState[len(expectedDatabaseState)-8].(*nbdb.AddressSet).Addresses = []string{t2.podIP} // podIP should be added to v4 Pass address-set
				expectedDatabaseState[len(expectedDatabaseState)-2].(*nbdb.AddressSet).Addresses = []string{t2.podIP} // podIP should be added to v4 Pass address-set
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				ginkgo.By("13. update the BANP by deleting 1 ingress rule & 1 egress rule; check if all objects are re-created correctly")
				banp.ResourceVersion = "6"
				banp.Spec.Ingress = []anpapi.BaselineAdminNetworkPolicyIngressRule{ingressRules[1], ingressRules[2]} // deny is gone
				banp.Spec.Egress = []anpapi.BaselineAdminNetworkPolicyEgressRule{egressRules[0], egressRules[2]}     // allow is gone
				banp, err = fakeOVN.fakeClient.ANPClient.PolicyV1alpha1().BaselineAdminNetworkPolicies().Update(context.TODO(), banp, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// TODO: test server does not garbage collect ACLs, so we just expect port groups to be updated for the old priority ACLs.
				// Both oldACLs and newACLs will be found in test server.
				newACLs := getACLsForBANPRules(banp)
				pg = getDefaultPGForANPSubject(banp.Name, []string{t.portUUID}, newACLs, true) // only newACLs are hosted
				expectedDatabaseState = []libovsdbtest.TestData{pg, subjectNSASIPv4, subjectNSASIPv6, peerNSASIPv4, peerNSASIPv6}
				for _, acl := range newACLs {
					acl := acl
					expectedDatabaseState = append(expectedDatabaseState, acl)
				}
				expectedDatabaseState = append(expectedDatabaseState, getDefaultNetExpectedPodsAndSwitches([]testPod{t, t2}, []string{node1Name})...)
				// ensure address-sets for the rules that were deleted are also gone (index 2 in the list)
				// ensure new address-sets have expected IPs
				// address-set will be empty since no pods match it yet
				peerASIngressRule0v4, peerASIngressRule0v6 = buildBANPAddressSets(banp, 0, []string{}, libovsdbutil.ACLIngress)
				expectedDatabaseState = append(expectedDatabaseState, peerASIngressRule0v4)
				expectedDatabaseState = append(expectedDatabaseState, peerASIngressRule0v6)
				// podIP should be added to v4 Pass address-set
				peerASIngressRule1v4, peerASIngressRule1v6 = buildBANPAddressSets(banp, 1, []string{t2.podIP}, libovsdbutil.ACLIngress)
				expectedDatabaseState = append(expectedDatabaseState, peerASIngressRule1v4)
				expectedDatabaseState = append(expectedDatabaseState, peerASIngressRule1v6)
				// address-set will be empty since no pods match it yet
				peerASEgressRule0v4, peerASEgressRule0v6 = buildBANPAddressSets(banp, 0, []string{}, libovsdbutil.ACLEgress)
				expectedDatabaseState = append(expectedDatabaseState, peerASEgressRule0v4)
				expectedDatabaseState = append(expectedDatabaseState, peerASEgressRule0v6)
				// podIP should be added to v4 Pass address-set
				peerASEgressRule1v4, peerASEgressRule1v6 = buildBANPAddressSets(banp, 1, []string{t2.podIP}, libovsdbutil.ACLEgress)
				expectedDatabaseState = append(expectedDatabaseState, peerASEgressRule1v4)
				expectedDatabaseState = append(expectedDatabaseState, peerASEgressRule1v6)
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				ginkgo.By("14. delete pod matching peer selector; check if IPs are updated in address-sets")
				banpPeerPod.ResourceVersion = "3"
				err = fakeOVN.fakeClient.KubeClient.CoreV1().Pods(banpPeerPod.Namespace).Delete(context.TODO(), banpPeerPod.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				pg = getDefaultPGForANPSubject(banp.Name, []string{t.portUUID}, newACLs, true)            // only newACLs are hosted
				peerNSASIPv4, peerNSASIPv6 = buildNamespaceAddressSets(banpPeerNamespaceName, []string{}) // pod is gone from peer namespace address-set
				expectedDatabaseState = []libovsdbtest.TestData{pg, subjectNSASIPv4, subjectNSASIPv6, peerNSASIPv4, peerNSASIPv6}
				for _, acl := range newACLs {
					acl := acl
					expectedDatabaseState = append(expectedDatabaseState, acl)
				}
				expectedDatabaseState = append(expectedDatabaseState, getDefaultNetExpectedPodsAndSwitches([]testPod{t}, []string{node1Name})...)
				// address-set will be empty since no pods match it yet
				peerASIngressRule1v4, peerASIngressRule1v6 = buildBANPAddressSets(banp, 1, []string{}, libovsdbutil.ACLIngress)
				// address-set will be empty since no pods match it yet
				peerASEgressRule1v4, peerASEgressRule1v6 = buildBANPAddressSets(banp, 1, []string{}, libovsdbutil.ACLEgress)
				expectedDatabaseState = append(expectedDatabaseState, []libovsdbtest.TestData{peerASIngressRule0v4, peerASIngressRule0v6,
					peerASIngressRule1v4, peerASIngressRule1v6, peerASEgressRule0v4, peerASEgressRule0v6, peerASEgressRule1v4, peerASEgressRule1v6}...)
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				ginkgo.By("15. update the subject pod to go into completed state; check if port group is updated")
				banpSubjectPod.ResourceVersion = "3"
				banpSubjectPod.Status.Phase = v1.PodSucceeded
				_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Pods(banpSubjectPod.Namespace).Update(context.TODO(), &banpSubjectPod, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				pg = getDefaultPGForANPSubject(banp.Name, nil, newACLs, true) // no ports in PG
				subjectNSASIPv4, subjectNSASIPv6 = buildNamespaceAddressSets(banpSubjectNamespaceName, []string{})
				expectedDatabaseState = []libovsdbtest.TestData{pg, subjectNSASIPv4, subjectNSASIPv6, peerNSASIPv4, peerNSASIPv6}
				for _, acl := range newACLs {
					acl := acl
					expectedDatabaseState = append(expectedDatabaseState, acl)
				}
				expectedDatabaseState = append(expectedDatabaseState, getDefaultNetExpectedPodsAndSwitches([]testPod{}, []string{node1Name})...)
				expectedDatabaseState = append(expectedDatabaseState, []libovsdbtest.TestData{peerASIngressRule0v4, peerASIngressRule0v6, peerASIngressRule1v4,
					peerASIngressRule1v6, peerASEgressRule0v4, peerASEgressRule0v6, peerASEgressRule1v4, peerASEgressRule1v6}...)
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
				err = fakeOVN.fakeClient.KubeClient.CoreV1().Pods(banpSubjectPod.Namespace).Delete(context.TODO(), banpSubjectPod.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				ginkgo.By("16. delete the subject and peer selected namespaces; check if port group and address-set's are updated")
				// create a new pod in subject and peer namespaces so that we can check namespace deletion properly
				banpSubjectPod = *newPodWithLabels(banpSubjectNamespaceName, banpSubjectPodName, node1Name, "10.128.1.5", peerDenyLabel)
				_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Pods(banpSubjectPod.Namespace).Create(context.TODO(), &banpSubjectPod, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// The pod takes some time to get created so the first add pod event for ANP does nothing as it waits for LSP to be created - this triggers a retry.
				// Next when the pod is scheduled and IP is allocated and LSP is created we get an update event.
				// When ANP controller receives the pod update event, it will not trigger an update in unit testing since ResourceVersion and pod.status.Running
				// is not updated by the test, hence we need to trigger a manual update in the unit test framework that is accompanied by label update.
				// NOTE: This is not an issue in real deployments with actual kapi server; its just a potential flake in the UT framework that we need to protect against.
				time.Sleep(time.Second)
				banpSubjectPod.ResourceVersion = "500"
				// - this is to trigger an actual update technically speaking the pod already matches the namespace of the subject
				banpSubjectPod.Labels["rv"] = "resourceVersionUTHack"
				_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Pods(banpSubjectPod.Namespace).Update(context.TODO(), &banpSubjectPod, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				t.podIP = "10.128.1.5"
				t.podMAC = "0a:58:0a:80:01:05"
				pg = getDefaultPGForANPSubject(banp.Name, []string{t.portUUID}, newACLs, true)
				subjectNSASIPv4, subjectNSASIPv6 = buildNamespaceAddressSets(banpSubjectNamespaceName, []string{t.podIP})
				expectedDatabaseState = []libovsdbtest.TestData{pg, subjectNSASIPv4, subjectNSASIPv6, peerNSASIPv4, peerNSASIPv6}
				for _, acl := range newACLs {
					acl := acl
					expectedDatabaseState = append(expectedDatabaseState, acl)
				}
				expectedDatabaseState = append(expectedDatabaseState, getDefaultNetExpectedPodsAndSwitches([]testPod{t}, []string{node1Name})...)
				expectedDatabaseState = append(expectedDatabaseState, []libovsdbtest.TestData{peerASIngressRule0v4, peerASIngressRule0v6, peerASIngressRule1v4,
					peerASIngressRule1v6, peerASEgressRule0v4, peerASEgressRule0v6, peerASEgressRule1v4, peerASEgressRule1v6}...)
				gomega.Eventually(fakeOVN.nbClient, "3s").Should(libovsdbtest.HaveData(expectedDatabaseState))
				banpPeerPod = *newPodWithLabels(banpPeerNamespaceName, banpPeerPodName, node1Name, "10.128.1.6", peerAllowLabel)
				_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Pods(banpPeerPod.Namespace).Create(context.TODO(), &banpPeerPod, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// The pod takes some time to get created so the first add pod event for ANP does nothing as it waits for LSP to be created - this triggers a retry.
				// Next when the pod is scheduled and IP is allocated and LSP is created we get an update event.
				// When ANP controller receives the pod update event, it will not trigger an update in unit testing since ResourceVersion and pod.status.Running
				// is not updated by the test, hence we need to trigger a manual update in the unit test framework that is accompanied by label update.
				// NOTE: This is not an issue in real deployments with actual kapi server; its just a potential flake in the UT framework that we need to protect against.
				banpPeerPod.ResourceVersion = "500"
				// - this is to trigger an actual update technically speaking the pod already matches the namespace of the subject
				banpPeerPod.Labels["rv"] = "resourceVersionUTHack"
				_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Pods(banpPeerPod.Namespace).Update(context.TODO(), &banpPeerPod, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				t2.podIP = "10.128.1.6"
				t2.podMAC = "0a:58:0a:80:01:06"
				peerNSASIPv4, peerNSASIPv6 = buildNamespaceAddressSets(banpPeerNamespaceName, []string{t2.podIP})
				expectedDatabaseState = []libovsdbtest.TestData{pg, subjectNSASIPv4, subjectNSASIPv6, peerNSASIPv4, peerNSASIPv6}
				for _, acl := range newACLs {
					acl := acl
					expectedDatabaseState = append(expectedDatabaseState, acl)
				}
				expectedDatabaseState = append(expectedDatabaseState, getDefaultNetExpectedPodsAndSwitches([]testPod{t, t2}, []string{node1Name})...)
				peerASIngressRule1v4, peerASIngressRule1v6 = buildBANPAddressSets(banp, 1, []string{t2.podIP}, libovsdbutil.ACLIngress)
				peerASEgressRule1v4, peerASEgressRule1v6 = buildBANPAddressSets(banp, 1, []string{t2.podIP}, libovsdbutil.ACLEgress)
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
				pg = getDefaultPGForANPSubject(banp.Name, nil, newACLs, true) // no ports in PG
				expectedDatabaseState = []libovsdbtest.TestData{pg}           // namespace address-sets are gone
				for _, acl := range newACLs {
					acl := acl
					expectedDatabaseState = append(expectedDatabaseState, acl)
				}
				// delete namespace in unit testing doesn't delete pods; we just need to check port groups and address-sets are updated
				expectedDatabaseState = append(expectedDatabaseState, getDefaultNetExpectedPodsAndSwitches([]testPod{t, t2}, []string{node1Name})...)
				peerASIngressRule1v4, peerASIngressRule1v6 = buildBANPAddressSets(banp, 1, []string{}, libovsdbutil.ACLIngress) // address-set will be empty since no pods match it yet
				peerASEgressRule1v4, peerASEgressRule1v6 = buildBANPAddressSets(banp, 1, []string{}, libovsdbutil.ACLEgress)    // address-set will be empty since no pods match it yet
				expectedDatabaseState = append(expectedDatabaseState, []libovsdbtest.TestData{peerASIngressRule0v4, peerASIngressRule0v6, peerASIngressRule1v4,
					peerASIngressRule1v6, peerASEgressRule0v4, peerASEgressRule0v6, peerASEgressRule1v4, peerASEgressRule1v6}...)
				// NOTE: Address set deletion is deferred for 20 seconds...
				gomega.Eventually(fakeOVN.nbClient, "25s").Should(libovsdbtest.HaveData(expectedDatabaseState))

				ginkgo.By("17. update the BANP by deleting all rules; check if all objects are re-created correctly")
				banp.Spec.Ingress = []anpapi.BaselineAdminNetworkPolicyIngressRule{}
				banp.Spec.Egress = []anpapi.BaselineAdminNetworkPolicyEgressRule{}
				banp.ResourceVersion = "7"
				banp, err = fakeOVN.fakeClient.ANPClient.PolicyV1alpha1().BaselineAdminNetworkPolicies().Update(context.TODO(), banp, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				pg = getDefaultPGForANPSubject(banp.Name, nil, nil, true) // no ports and acls in PG
				expectedDatabaseState = []libovsdbtest.TestData{pg}
				expectedDatabaseState = append(expectedDatabaseState, getDefaultNetExpectedPodsAndSwitches([]testPod{t, t2}, []string{node1Name})...)
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				ginkgo.By("18. delete the BANP; check if all objects are re-created correctly")
				banp.ResourceVersion = "8"
				err = fakeOVN.fakeClient.ANPClient.PolicyV1alpha1().BaselineAdminNetworkPolicies().Delete(context.TODO(), banp.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				expectedDatabaseState = []libovsdbtest.TestData{} // port group should be deleted
				expectedDatabaseState = append(expectedDatabaseState, getDefaultNetExpectedPodsAndSwitches([]testPod{t, t2}, []string{node1Name})...)
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				ginkgo.By("19. create new BANP with same name; check if all objects are re-created correctly; test's cache is cleared and then repopulated properly")
				banp2 := newBANPObject("harry-potter", banpSubject, []anpapi.BaselineAdminNetworkPolicyIngressRule{}, []anpapi.BaselineAdminNetworkPolicyEgressRule{})
				banp2.ResourceVersion = "1"
				banp2, err = fakeOVN.fakeClient.ANPClient.PolicyV1alpha1().BaselineAdminNetworkPolicies().Create(context.TODO(), banp, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// TODO: Check if clone methods did the right thing - do the test in the admin_network_policy_package
				pg = getDefaultPGForANPSubject(banp2.Name, nil, nil, true)
				expectedDatabaseState = []libovsdbtest.TestData{pg} // port-group should be recreated
				expectedDatabaseState = append(expectedDatabaseState, getDefaultNetExpectedPodsAndSwitches([]testPod{t, t2}, []string{node1Name})...)
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
		})
		ginkgo.It("ACL Logging for BANP", func() {
			app.Action = func(ctx *cli.Context) error {
				config.IPv4Mode = true
				config.IPv6Mode = true
				fakeOVN.start()
				fakeOVN.InitAndRunANPController()
				fakeOVN.fakeClient.ANPClient.(*anpfake.Clientset).PrependReactor("update", "baselineadminnetworkpolicies", func(action clienttesting.Action) (handled bool, ret runtime.Object, err error) {
					update := action.(clienttesting.UpdateAction)
					// Since fake client (NewSimpleClientset) does not differentiate between
					// an update and updatestatus, updatestatus in tests updates the spec as
					// well causing race conditions. Thus adding a hack here to ensure update
					// status is caught and processed by the reactor while update spec is
					// delegated to the main code for handling
					if action.GetSubresource() == "status" {
						klog.Infof("Got an update status action for %v", update.GetObject())
						return true, update.GetObject(), nil
					}
					klog.Infof("Got an update spec action for %v", update.GetObject())
					return false, update.GetObject(), nil
				})
				ginkgo.By("1. Create BANP with 1 ingress rule and 1 egress rule with the ACL logging annotation and ensure its honoured")
				anpSubject := newANPSubjectObject(
					&metav1.LabelSelector{
						MatchLabels: anpLabel,
					},
					nil,
				)
				banp := newBANPObject("default", anpSubject,
					[]anpapi.BaselineAdminNetworkPolicyIngressRule{
						{
							Name:   "deny-traffic-from-slytherin-to-gryffindor",
							Action: anpapi.BaselineAdminNetworkPolicyRuleActionDeny,
							From: []anpapi.AdminNetworkPolicyIngressPeer{
								{
									Namespaces: &metav1.LabelSelector{
										MatchLabels: peerDenyLabel,
									},
								},
							},
						},
					},
					[]anpapi.BaselineAdminNetworkPolicyEgressRule{
						{
							Name:   "allow-traffic-to-hufflepuff-from-gryffindor",
							Action: anpapi.BaselineAdminNetworkPolicyRuleActionAllow,
							To: []anpapi.AdminNetworkPolicyEgressPeer{
								{
									Namespaces: &metav1.LabelSelector{
										MatchLabels: peerAllowLabel,
									},
								},
							},
						},
					},
				)
				banp.ResourceVersion = "1"
				banp.Annotations = map[string]string{
					util.AclLoggingAnnotation: fmt.Sprintf(`{ "deny": "%s", "allow": "%s"}`, nbdb.ACLSeverityAlert, nbdb.ACLSeverityInfo),
				}
				banp, err := fakeOVN.fakeClient.ANPClient.PolicyV1alpha1().BaselineAdminNetworkPolicies().Create(context.TODO(), banp, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				acls := getACLsForBANPRules(banp)
				pg := getDefaultPGForANPSubject(banp.Name, []string{}, acls, true)
				expectedDatabaseState := []libovsdbtest.TestData{pg}
				for _, acl := range acls {
					acl := acl
					// update ACL logging information
					acl.Log = true
					if acl.Action == nbdb.ACLActionDrop {
						acl.Severity = utilpointer.String(nbdb.ACLSeverityAlert)
					} else if acl.Action == nbdb.ACLActionAllowRelated {
						acl.Severity = utilpointer.String(nbdb.ACLSeverityInfo)
					}
					expectedDatabaseState = append(expectedDatabaseState, acl)
				}
				peerASIngressRule0v4, peerASIngressRule0v6 := buildBANPAddressSets(banp, 0, []string{}, libovsdbutil.ACLIngress) // address-set will be empty since no pods match it yet
				expectedDatabaseState = append(expectedDatabaseState, peerASIngressRule0v4)
				expectedDatabaseState = append(expectedDatabaseState, peerASIngressRule0v6)
				peerASEgressRule0v4, peerASEgressRule0v6 := buildBANPAddressSets(banp, 0, []string{}, libovsdbutil.ACLEgress)
				expectedDatabaseState = append(expectedDatabaseState, peerASEgressRule0v4)
				expectedDatabaseState = append(expectedDatabaseState, peerASEgressRule0v6)
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))

				ginkgo.By("2. Update BANP by changing severity on the ACL logging annotation and ensure its honoured")
				banp.ResourceVersion = "2"
				banp.Annotations = map[string]string{
					util.AclLoggingAnnotation: fmt.Sprintf(`{"deny": "%s", "allow": "%s"}`, nbdb.ACLSeverityWarning, nbdb.ACLSeverityDebug),
				}
				banp, err = fakeOVN.fakeClient.ANPClient.PolicyV1alpha1().BaselineAdminNetworkPolicies().Update(context.TODO(), banp, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				for _, acl := range expectedDatabaseState[1:3] {
					acl := acl.(*nbdb.ACL)
					// update ACL logging information
					acl.Log = true
					if acl.Action == nbdb.ACLActionDrop {
						acl.Severity = utilpointer.String(nbdb.ACLSeverityWarning)
					} else if acl.Action == nbdb.ACLActionAllowRelated {
						acl.Severity = utilpointer.String(nbdb.ACLSeverityDebug)
					}
				}
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))

				ginkgo.By("3. Update BANP by deleting the ACL logging annotation and ensure its honoured")
				banp.ResourceVersion = "3"
				banp.Annotations = map[string]string{}
				_, err = fakeOVN.fakeClient.ANPClient.PolicyV1alpha1().BaselineAdminNetworkPolicies().Update(context.TODO(), banp, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				for _, acl := range expectedDatabaseState[1:3] {
					acl := acl.(*nbdb.ACL)
					// update ACL logging information
					acl.Log = false
					acl.Severity = nil
				}
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))
				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
		})
		ginkgo.It("egress node+network peers: should create/update/delete address-sets, acls, port-groups correctly", func() {
			app.Action = func(ctx *cli.Context) error {
				banpNamespaceSubject := *newNamespaceWithLabels(banpSubjectNamespaceName, anpLabel)
				banpNamespacePeer := *newNamespaceWithLabels(banpPeerNamespaceName, peerDenyLabel)
				config.IPv4Mode = true
				config.IPv6Mode = true
				node1 := nodeFor(node1Name, node1IPv4, node1IPv6, node1IPv4Subnet, node1IPv6Subnet, node1transitIPv4, node1transitIPv6)
				node1Switch := &nbdb.LogicalSwitch{
					Name: node1Name,
					UUID: node1Name + "-UUID",
				}
				t := newTPod(
					node1Name,
					node1IPv4Subnet+" "+node1IPv6Subnet,
					"10.128.1.2",
					"10.128.1.1"+" "+"fe00:10:128:1::1",
					banpSubjectPodName,
					banpPodV4IP+" "+banpPodV6IP,
					banpPodMAC,
					banpSubjectNamespaceName,
				)
				anpSubjectPod := *newPod(banpSubjectNamespaceName, banpSubjectPodName, node1Name, t.podIP)
				// pinning annotations because between subject and peer pods IPAM isunpredictable
				anpSubjectPod.Annotations = map[string]string{}
				anpSubjectPod.Annotations["k8s.ovn.org/pod-networks"] = `{"default":{"ip_addresses":["10.128.1.3/24","fe00:10:128:1::3/64"],` +
					`"mac_address":"0a:58:0a:80:01:03","gateway_ips":["10.128.1.1","fe00:10:128:1::1"],"routes":[{"dest":"10.128.1.0/24","nextHop":"10.128.1.1"}],` +
					`"ip_address":"10.128.1.3/24","gateway_ip":"10.128.1.1"}}`
				t2 := newTPod(
					node1Name,
					node1IPv4Subnet+" "+node1IPv6Subnet,
					"10.128.1.2",
					"10.128.1.1"+" "+"fe00:10:128:1::1",
					banpPeerPodName,
					banpPodV4IP2+" "+banpPodV6IP2,
					banpPodMAC2,
					banpPeerNamespaceName,
				)
				anpPeerPod := *newPod(banpPeerNamespaceName, banpPeerPodName, node1Name, t2.podIP)
				// pinning annotations because between subject and peer pods IPAM isunpredictable
				anpPeerPod.Annotations = map[string]string{}
				anpPeerPod.Annotations["k8s.ovn.org/pod-networks"] = `{"default":{"ip_addresses":["10.128.1.4/24","fe00:10:128:1::4/64"],` +
					`"mac_address":"0a:58:0a:80:01:04","gateway_ips":["10.128.1.1","fe00:10:128:1::1"],"routes":[{"dest":"10.128.1.0/24","nextHop":"10.128.1.1"}],` +
					`"ip_address":"10.128.1.4/24","gateway_ip":"10.128.1.1"}}`
				dbSetup := libovsdbtest.TestSetup{
					NBData: []libovsdbtest.TestData{
						node1Switch,
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
					&v1.PodList{
						Items: []v1.Pod{
							anpSubjectPod, // subject pod ("house": "gryffindor")
							anpPeerPod,    // peer pod ("house": "slytherin")
						},
					},
				)

				fakeOVN.controller.zone = node1Name // ensure we set the controller's zone as the node's zone
				t.portName = util.GetLogicalPortName(t.namespace, t.podName)
				t.populateLogicalSwitchCache(fakeOVN)
				t2.portName = util.GetLogicalPortName(t2.namespace, t2.podName)
				t2.populateLogicalSwitchCache(fakeOVN)
				err := fakeOVN.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOVN.controller.WatchPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOVN.InitAndRunANPController()

				fakeOVN.fakeClient.ANPClient.(*anpfake.Clientset).PrependReactor("update", "baselineadminnetworkpolicies", func(action clienttesting.Action) (handled bool, ret runtime.Object, err error) {
					update := action.(clienttesting.UpdateAction)
					// Since fake client (NewSimpleClientset) does not differentiate between
					// an update and updatestatus, updatestatus in tests updates the spec as
					// well causing race conditions. Thus adding a hack here to ensure update
					// status is caught and processed by the reactor while update spec is
					// delegated to the main code for handling
					if action.GetSubresource() == "status" {
						klog.Infof("Got an update status action for %v", update.GetObject())
						return true, update.GetObject(), nil
					}
					klog.Infof("Got an update spec action for %v", update.GetObject())
					return false, update.GetObject(), nil
				})

				ginkgo.By("1. creating a baseline admin network policy with 2 egress rules that have node+network peers")
				banpSubject := newANPSubjectObject(
					&metav1.LabelSelector{
						MatchLabels: anpLabel,
					},
					nil,
				)
				// NOTE: Logically speaking the rules don't make sense because we match on the same node, but goal here is to verify the match
				egressRules := []anpapi.BaselineAdminNetworkPolicyEgressRule{
					{
						Name:   "deny-traffic-to-slytherin-and-linux-nodes-from-gryffindor",
						Action: anpapi.BaselineAdminNetworkPolicyRuleActionDeny,
						To: []anpapi.AdminNetworkPolicyEgressPeer{
							{
								Namespaces: &metav1.LabelSelector{
									MatchLabels: peerDenyLabel,
								},
							},
							{
								Nodes: &metav1.LabelSelector{
									MatchLabels: map[string]string{"kubernetes.io/os": "linux"},
								},
							},
							{
								Networks: []anpapi.CIDR{"0.0.0.0/0", "::/0"},
							},
						},
					},
					{
						Name:   "allow-traffic-to-hufflepuff-and--all-nodes-from-gryffindor",
						Action: anpapi.BaselineAdminNetworkPolicyRuleActionAllow,
						To: []anpapi.AdminNetworkPolicyEgressPeer{
							{
								Pods: &anpapi.NamespacedPod{ // test different kind of peer expression
									NamespaceSelector: metav1.LabelSelector{
										MatchExpressions: []metav1.LabelSelectorRequirement{
											{
												Key:      "house",
												Operator: metav1.LabelSelectorOpIn,
												Values:   []string{"slytherin", "hufflepuff"},
											},
										},
									},
									PodSelector: metav1.LabelSelector{
										MatchLabels: peerAllowLabel,
									},
								},
							},
							{
								Nodes: &metav1.LabelSelector{
									MatchLabels: map[string]string{}, // empty selector, all nodes
								},
							},
							{
								Networks: []anpapi.CIDR{"135.10.0.5/32", "188.198.40.0/28", "2001:db8:abcd:1234:c000::/66"},
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
								PortRange: &anpapi.PortRange{
									Protocol: v1.ProtocolUDP,
									Start:    int32(12345),
									End:      int32(65000),
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
				banp := newBANPObject("harry-potter", banpSubject, []anpapi.BaselineAdminNetworkPolicyIngressRule{}, egressRules)
				banp.ResourceVersion = "1"
				banp, err = fakeOVN.fakeClient.ANPClient.PolicyV1alpha1().BaselineAdminNetworkPolicies().Create(context.TODO(), banp, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				acls := getACLsForBANPRules(banp)
				pg := getDefaultPGForANPSubject(banp.Name, []string{t.portUUID}, acls, true)
				subjectNSASIPv4, subjectNSASIPv6 := buildNamespaceAddressSets(banpSubjectNamespaceName, []string{banpPodV4IP, banpPodV6IP})
				peerNSASIPv4, peerNSASIPv6 := buildNamespaceAddressSets(banpPeerNamespaceName, []string{banpPodV4IP2, banpPodV6IP2})
				expectedDatabaseState := []libovsdbtest.TestData{pg, subjectNSASIPv4, subjectNSASIPv6, peerNSASIPv4, peerNSASIPv6}
				expectedDatabaseState = append(expectedDatabaseState, getDefaultNetExpectedPodsAndSwitches([]testPod{t, t2}, []string{node1Name})...)
				for _, acl := range acls {
					acl := acl
					expectedDatabaseState = append(expectedDatabaseState, acl)
				}
				// egressRule AddressSets
				peerASEgressRule0v4, peerASEgressRule0v6 := buildBANPAddressSets(banp,
					0, []string{banpPodV4IP2, banpPodV6IP2, "0.0.0.0/0", "::/0"}, libovsdbutil.ACLEgress) // address-set will contain matching peer nodes, kubernetes.io/hostname doesn't match node1 so no node peers here
				expectedDatabaseState = append(expectedDatabaseState, peerASEgressRule0v4)
				expectedDatabaseState = append(expectedDatabaseState, peerASEgressRule0v6)
				peerASEgressRule1v4, peerASEgressRule1v6 := buildBANPAddressSets(banp,
					1, []string{node1IPv4, node1IPv6, "135.10.0.5/32", "188.198.40.0/28", "2001:db8:abcd:1234:c000::/66"}, libovsdbutil.ACLEgress) // address-set will contain all nodeIPs (empty selector match)
				expectedDatabaseState = append(expectedDatabaseState, peerASEgressRule1v4)
				expectedDatabaseState = append(expectedDatabaseState, peerASEgressRule1v6)
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))

				ginkgo.By("2. creating another node2 that will act as peer of baseline admin network policy; check if IP is added to address-set")
				node2 := nodeFor(node2Name, node2IPv4, node2IPv6, node2IPv4Subnet, node2IPv6Subnet, node2transitIPv4, node2transitIPv6)
				_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Nodes().Create(context.TODO(), node2, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				expectedDatabaseState[len(expectedDatabaseState)-1].(*nbdb.AddressSet).Addresses = []string{node2IPv6, node1IPv6, "2001:db8:abcd:1234:c000::/66"}     // nodeIP should be added to v6 allow address-set
				expectedDatabaseState[len(expectedDatabaseState)-2].(*nbdb.AddressSet).Addresses = []string{node2IPv4, node1IPv4, "135.10.0.5/32", "188.198.40.0/28"} // nodeIP should be added to v4 allow address-set
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				ginkgo.By("3. update the labels of node2 such that it starts to match the DENY RULE peer selector; check if IPs are updated in address-sets")
				node2.ResourceVersion = "2"
				node2.Labels["kubernetes.io/os"] = "linux" // node should not match the 1st anp deny rule peer
				_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), node2, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				expectedDatabaseState[len(expectedDatabaseState)-3].(*nbdb.AddressSet).Addresses = []string{
					banpPodV6IP2, node2IPv6, "::/0"} // nodeIP should be added to v6 deny address-set
				expectedDatabaseState[len(expectedDatabaseState)-4].(*nbdb.AddressSet).Addresses = []string{
					banpPodV4IP2, node2IPv4, "0.0.0.0/0"} // nodeIP should be added to v4 deny address-set
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				ginkgo.By("4. update the peer selector of rule1 in baseline admin network policy so that v6 networks-peer is deleted; check if address-set is updated")
				egressRules[1].To[2].Networks = []anpapi.CIDR{"135.10.0.5/32", "188.198.40.0/28"} // delete v6 network CIDR
				banp.ResourceVersion = "3"
				banp.Spec.Egress = egressRules
				_, err = fakeOVN.fakeClient.ANPClient.PolicyV1alpha1().BaselineAdminNetworkPolicies().Update(context.TODO(), banp, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				expectedDatabaseState[len(expectedDatabaseState)-1].(*nbdb.AddressSet).Addresses = []string{node2IPv6, node1IPv6} // network should be removed from v6 allow address-set
				expectedDatabaseState[len(expectedDatabaseState)-2].(*nbdb.AddressSet).Addresses = []string{node2IPv4, node1IPv4, "135.10.0.5/32", "188.198.40.0/28"}
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				ginkgo.By("5. update the peer selector of rule2 in baseline admin network policy so that networks-peer is deleted; check if address-set is updated")
				egressRules[1].To = egressRules[1].To[:2]
				banp.ResourceVersion = "5"
				banp.Spec.Egress = egressRules
				_, err = fakeOVN.fakeClient.ANPClient.PolicyV1alpha1().BaselineAdminNetworkPolicies().Update(context.TODO(), banp, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				expectedDatabaseState[len(expectedDatabaseState)-1].(*nbdb.AddressSet).Addresses = []string{node2IPv6, node1IPv6} // network should be removed from v6 allow address-set
				expectedDatabaseState[len(expectedDatabaseState)-2].(*nbdb.AddressSet).Addresses = []string{node2IPv4, node1IPv4} // network should be removed from v4 allow address-set
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				ginkgo.By("6. update the hostCIDR annotation of node2; check if IPs are updated in address-sets")
				node2.ResourceVersion = "3"
				node2.Annotations[util.OVNNodeHostCIDRs] = fmt.Sprintf("[\"%s\",\"%s\"]", "200.100.100.0/24", "fc00:f853:ccd:e793::2/64")
				_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), node2, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				expectedDatabaseState[len(expectedDatabaseState)-2].(*nbdb.AddressSet).Addresses = []string{
					"200.100.100.0", node1IPv4} // nodeIP should be updated in v4 allow address-set
				expectedDatabaseState[len(expectedDatabaseState)-4].(*nbdb.AddressSet).Addresses = []string{
					banpPodV4IP2, "200.100.100.0", "0.0.0.0/0"} // nodeIP should be updated in v4 deny address-set
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				ginkgo.By("7. update the peer selector of rule1 in baseline admin network policy so that node1 stops matching; check if address-set is updated")
				egressRules[1].To[1].Nodes = &metav1.LabelSelector{
					MatchLabels: map[string]string{"kubernetes.io/hostname": "node2"},
				}
				banp.ResourceVersion = "10"
				banp.Spec.Egress = egressRules
				_, err = fakeOVN.fakeClient.ANPClient.PolicyV1alpha1().BaselineAdminNetworkPolicies().Update(context.TODO(), banp, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				expectedDatabaseState[len(expectedDatabaseState)-1].(*nbdb.AddressSet).Addresses = []string{node2IPv6}       // node1IP should be removed from v6 allow address-set
				expectedDatabaseState[len(expectedDatabaseState)-2].(*nbdb.AddressSet).Addresses = []string{"200.100.100.0"} // node1IP should be removed from v4 allow address-set
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				ginkgo.By("8. delete node matching peer selector; check if IPs are updated in address-sets")
				err = fakeOVN.fakeClient.KubeClient.CoreV1().Nodes().Delete(context.TODO(), node2Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				expectedDatabaseState[len(expectedDatabaseState)-3].(*nbdb.AddressSet).Addresses = []string{banpPodV6IP2, "::/0"}      // node2IP should be removed from v6 deny address-set
				expectedDatabaseState[len(expectedDatabaseState)-4].(*nbdb.AddressSet).Addresses = []string{banpPodV4IP2, "0.0.0.0/0"} // node2IP should be removed to v4 deny address-set
				expectedDatabaseState[len(expectedDatabaseState)-1].(*nbdb.AddressSet).Addresses = []string{}                          // node2IP should be removed from v6 allow address-set
				expectedDatabaseState[len(expectedDatabaseState)-2].(*nbdb.AddressSet).Addresses = []string{}                          // node2IP should be removed to v4 allow address-set
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				ginkgo.By("9. update the BANP by deleting all rules; check if all objects are deleted correctly")
				banp.Spec.Egress = []anpapi.BaselineAdminNetworkPolicyEgressRule{}
				banp.ResourceVersion = "15"
				banp, err = fakeOVN.fakeClient.ANPClient.PolicyV1alpha1().BaselineAdminNetworkPolicies().Update(context.TODO(), banp, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				pg = getDefaultPGForANPSubject(banp.Name, []string{t.portUUID}, nil, true)
				expectedDatabaseState = []libovsdbtest.TestData{pg, subjectNSASIPv4, subjectNSASIPv6, peerNSASIPv4, peerNSASIPv6}
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				expectedDatabaseState = append(expectedDatabaseState, getDefaultNetExpectedPodsAndSwitches([]testPod{t, t2}, []string{node1Name})...)
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				ginkgo.By("10. delete the BANP; check if all objects are deleted correctly")
				banp.ResourceVersion = "8"
				err = fakeOVN.fakeClient.ANPClient.PolicyV1alpha1().BaselineAdminNetworkPolicies().Delete(context.TODO(), banp.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				expectedDatabaseState = []libovsdbtest.TestData{subjectNSASIPv4, subjectNSASIPv6, peerNSASIPv4, peerNSASIPv6} // port group should be deleted
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				expectedDatabaseState = append(expectedDatabaseState, getDefaultNetExpectedPodsAndSwitches([]testPod{t, t2}, []string{node1Name})...)
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
		})
		ginkgo.It("NamedPort matching pods Rules for BANP", func() {
			app.Action = func(ctx *cli.Context) error {
				config.IPv4Mode = true
				config.IPv6Mode = true
				banpSubjectNamespace := *newNamespaceWithLabels(banpSubjectNamespaceName, anpLabel)
				banpNamespacePeer := *newNamespaceWithLabels(banpPeerNamespaceName, peerAllowLabel)
				banpNamespacePeer2 := *newNamespaceWithLabels(banpPeerNamespaceName+"2", peerDenyLabel)
				node1 := nodeFor(node1Name, node1IPv4, node1IPv6, node1IPv4Subnet, node1IPv6Subnet, node1transitIPv4, node1transitIPv6)
				node1Switch := &nbdb.LogicalSwitch{
					Name: node1Name,
					UUID: node1Name + "-UUID",
				}
				t1 := newTPod(
					node1Name,
					node1IPv4Subnet+" "+node1IPv6Subnet,
					"10.128.1.2",
					"10.128.1.1"+" "+"fe00:10:128:1::1",
					banpPeerPodName,
					banpPodV4IP2+" "+banpPodV6IP2,
					banpPodMAC2,
					banpPeerNamespaceName,
				)
				banpPeerPod := *newPod(banpPeerNamespaceName, banpPeerPodName, node1Name, t1.podIP)
				// pinning annotations because between subject and peer pods IPAM isunpredictable
				banpPeerPod.Annotations = map[string]string{}
				banpPeerPod.Annotations["k8s.ovn.org/pod-networks"] = `{"default":{"ip_addresses":["10.128.1.4/24","fe00:10:128:1::4/64"],` +
					`"mac_address":"0a:58:0a:80:01:04","gateway_ips":["10.128.1.1","fe00:10:128:1::1"],"routes":[{"dest":"10.128.1.0/24","nextHop":"10.128.1.1"}],` +
					`"ip_address":"10.128.1.4/24","gateway_ip":"10.128.1.1"}}`
				banpPeerPod.Spec.Containers = append(banpPeerPod.Spec.Containers,
					v1.Container{
						Name:  "containerName",
						Image: "containerImage",
						Ports: []v1.ContainerPort{
							{
								Name:          "web",
								ContainerPort: 3535,
								Protocol:      v1.ProtocolTCP,
							},
							{
								Name:          "web123",
								ContainerPort: 35356,
								Protocol:      v1.ProtocolSCTP,
							},
						},
					},
				)
				t2 := newTPod(
					node1Name,
					node1IPv4Subnet+" "+node1IPv6Subnet,
					"10.128.1.2",
					"10.128.1.1"+" "+"fe00:10:128:1::1",
					banpSubjectPodName,
					banpPodV4IP+" "+banpPodV6IP,
					banpPodMAC,
					banpSubjectNamespaceName,
				)
				banpSubjectPod := *newPod(banpSubjectNamespaceName, banpSubjectPodName, node1Name, t2.podIP)
				// pinning annotations because between subject and peer pods IPAM isunpredictable
				banpSubjectPod.Annotations = map[string]string{}
				banpSubjectPod.Annotations["k8s.ovn.org/pod-networks"] = `{"default":{"ip_addresses":["10.128.1.3/24","fe00:10:128:1::3/64"],` +
					`"mac_address":"0a:58:0a:80:01:03","gateway_ips":["10.128.1.1","fe00:10:128:1::1"],"routes":[{"dest":"10.128.1.0/24","nextHop":"10.128.1.1"}],` +
					`"ip_address":"10.128.1.3/24","gateway_ip":"10.128.1.1"}}`
				banpSubjectPod.Spec.Containers[0].Ports = []v1.ContainerPort{
					{
						Name:          "dns",
						ContainerPort: 5353,
						Protocol:      v1.ProtocolUDP,
					},
				}
				dbSetup := libovsdbtest.TestSetup{
					NBData: []libovsdbtest.TestData{
						node1Switch,
					},
				}
				fakeOVN.startWithDBSetup(dbSetup,
					&v1.NamespaceList{
						Items: []v1.Namespace{
							banpSubjectNamespace,
							banpNamespacePeer,
							banpNamespacePeer2,
						},
					},
					&v1.NodeList{
						Items: []v1.Node{
							*node1,
						},
					},
					&v1.PodList{
						Items: []v1.Pod{
							banpPeerPod, // peer pod ("house": "slytherin")
						},
					},
				)
				fakeOVN.controller.zone = node1Name // ensure we set the controller's zone as the node's zone
				t1.portName = util.GetLogicalPortName(t1.namespace, t1.podName)
				t1.populateLogicalSwitchCache(fakeOVN)
				t2.portName = util.GetLogicalPortName(t2.namespace, t2.podName)
				t2.populateLogicalSwitchCache(fakeOVN)
				err := fakeOVN.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOVN.controller.WatchPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// update namedPorts for constructing right test ACLs
				namedEPorts := make(map[string][]libovsdbutil.NamedNetworkPolicyPort)
				namedIPorts := make(map[string][]libovsdbutil.NamedNetworkPolicyPort)
				namedEPorts["web"] = []libovsdbutil.NamedNetworkPolicyPort{{L4Protocol: "tcp", L3PodIP: banpPodV4IP2, L3PodIPFamily: "ip4", L4PodPort: "3535"},
					{L4Protocol: "tcp", L3PodIP: banpPodV6IP2, L3PodIPFamily: "ip6", L4PodPort: "3535"}}
				namedEPorts["web123"] = []libovsdbutil.NamedNetworkPolicyPort{{L4Protocol: "sctp", L3PodIP: banpPodV4IP2, L3PodIPFamily: "ip4", L4PodPort: "35356"},
					{L4Protocol: "sctp", L3PodIP: banpPodV6IP2, L3PodIPFamily: "ip6", L4PodPort: "35356"}}
				fakeOVN.InitAndRunANPController()
				fakeOVN.fakeClient.ANPClient.(*anpfake.Clientset).PrependReactor("update", "baselineadminnetworkpolicies", func(action clienttesting.Action) (handled bool, ret runtime.Object, err error) {
					update := action.(clienttesting.UpdateAction)
					// Since fake client (NewSimpleClientset) does not differentiate between
					// an update and updatestatus, updatestatus in tests updates the spec as
					// well causing race conditions. Thus adding a hack here to ensure update
					// status is caught and processed by the reactor while update spec is
					// delegated to the main code for handling
					if action.GetSubresource() == "status" {
						klog.Infof("Got an update status action for %v", update.GetObject())
						return true, update.GetObject(), nil
					}
					klog.Infof("Got an update spec action for %v", update.GetObject())
					return false, update.GetObject(), nil
				})
				ginkgo.By("1. Create BANP with 1 ingress rule and 1 egress rule with multiple NamedPorts combinations")
				banpSubject := newANPSubjectObject(
					&metav1.LabelSelector{
						MatchLabels: anpLabel,
					},
					nil,
				)
				anpPortMangle := []anpapi.AdminNetworkPolicyPort{
					{
						PortNumber: &anpapi.Port{
							Port:     36363,
							Protocol: v1.ProtocolTCP,
						},
					},
					{
						NamedPort: ptr.To("web123"), // matched by anpPeerPod
					},
					{
						PortRange: &anpapi.PortRange{
							Start:    36330,
							End:      36550,
							Protocol: v1.ProtocolSCTP,
						},
					},
					{
						NamedPort: ptr.To("web"), // matched by anpPeerPod
					},
					{
						PortRange: &anpapi.PortRange{
							Start:    36330,
							End:      36550,
							Protocol: v1.ProtocolUDP,
						},
					},
					{
						NamedPort: ptr.To("dns"), // matched by anpSubjectPod (will be created in the future)
					},
				}
				banp := newBANPObject("harry-potter", banpSubject,
					[]anpapi.BaselineAdminNetworkPolicyIngressRule{
						{ // 3 ACLs + 1ACL for NamedPort
							Name:   "deny-traffic-from-slytherin-to-gryffindor",
							Action: anpapi.BaselineAdminNetworkPolicyRuleActionDeny,
							From: []anpapi.AdminNetworkPolicyIngressPeer{
								{
									Namespaces: &metav1.LabelSelector{
										MatchLabels: peerDenyLabel,
									},
								},
							},
							Ports: &anpPortMangle,
						},
					},
					[]anpapi.BaselineAdminNetworkPolicyEgressRule{
						{ // 3 ACLs
							Name:   "allow-traffic-to-hufflepuff-from-gryffindor",
							Action: anpapi.BaselineAdminNetworkPolicyRuleActionAllow,
							To: []anpapi.AdminNetworkPolicyEgressPeer{
								{
									Namespaces: &metav1.LabelSelector{
										MatchLabels: peerAllowLabel,
									},
								},
							},
							Ports: &anpPortMangle,
						},
					},
				)
				banp.ResourceVersion = "1"
				banp, err = fakeOVN.fakeClient.ANPClient.PolicyV1alpha1().BaselineAdminNetworkPolicies().Create(context.TODO(), banp, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				acls := getACLsForBANPRulesWithNamedPorts(banp, namedIPorts, namedEPorts)
				pg := getDefaultPGForANPSubject(banp.Name, []string{}, acls, true)
				subjectNSASIPv4, subjectNSASIPv6 := buildNamespaceAddressSets(banpSubjectNamespaceName, []string{})
				peerNSASIPv4, peerNSASIPv6 := buildNamespaceAddressSets(banpPeerNamespaceName, []string{banpPodV4IP2, banpPodV6IP2})
				peerNS2ASIPv4, peerNS2ASIPv6 := buildNamespaceAddressSets(banpPeerNamespaceName+"2", []string{})
				baseExpectedDatabaseState := []libovsdbtest.TestData{subjectNSASIPv4, subjectNSASIPv6, peerNSASIPv4, peerNSASIPv6, peerNS2ASIPv4, peerNS2ASIPv6}
				peerASIngressRule0v4, peerASIngressRule0v6 := buildBANPAddressSets(banp, 0, []string{}, libovsdbutil.ACLIngress)
				baseExpectedDatabaseState = append(baseExpectedDatabaseState, peerASIngressRule0v4)
				baseExpectedDatabaseState = append(baseExpectedDatabaseState, peerASIngressRule0v6)
				peerASEgressRule0v4, peerASEgressRule0v6 := buildBANPAddressSets(banp, 0, []string{banpPodV4IP2, banpPodV6IP2}, libovsdbutil.ACLEgress)
				baseExpectedDatabaseState = append(baseExpectedDatabaseState, peerASEgressRule0v4)
				baseExpectedDatabaseState = append(baseExpectedDatabaseState, peerASEgressRule0v6)
				expectedDatabaseState := append(baseExpectedDatabaseState, pg)
				expectedDatabaseState = append(expectedDatabaseState, getDefaultNetExpectedPodsAndSwitches([]testPod{t1}, []string{node1Name})...)
				for _, acl := range acls {
					acl := acl
					expectedDatabaseState = append(expectedDatabaseState, acl)
				}
				// 3 ingress protocol ACLs,
				// 3 egress[0] protocol ACLs and 2 egress[0] namedPort ACLs,
				// total 8ACLs
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))

				ginkgo.By("2. Create subject pod and ensure LSP and namedPorts ACLs are updated for the matching ingress rule")
				_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Pods(banpSubjectNamespaceName).Create(context.TODO(), &banpSubjectPod, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// The pod takes some time to get created so the first add pod event for ANP does nothing as it waits for LSP to be created - this triggers a retry.
				// Next when the pod is scheduled and IP is allocated and LSP is created we get an update event.
				// When ANP controller receives the pod update event, it will not trigger an update in unit testing since ResourceVersion and pod.status.Running
				// is not updated by the test, hence we need to trigger a manual update in the unit test framework that is accompanied by label update.
				// NOTE: This is not an issue in real deployments with actual kapi server; its just a potential flake in the UT framework that we need to protect against.
				time.Sleep(time.Second)
				banpSubjectPod.ResourceVersion = "5"
				// - this is to trigger an actual update technically speaking the pod already matches the namespace of the subject
				banpSubjectPod.Labels["rv"] = "resourceVersionUTHack"
				_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Pods(banpSubjectPod.Namespace).Update(context.TODO(), &banpSubjectPod, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				namedIPorts["dns"] = []libovsdbutil.NamedNetworkPolicyPort{
					{L4Protocol: "udp", L3PodIP: banpPodV4IP, L3PodIPFamily: "ip4", L4PodPort: "5353"},
					{L4Protocol: "udp", L3PodIP: banpPodV6IP, L3PodIPFamily: "ip6", L4PodPort: "5353"}}

				subjectNSASIPv4, subjectNSASIPv6 = buildNamespaceAddressSets(banpSubjectNamespaceName, []string{banpPodV4IP, banpPodV6IP})
				acls = getACLsForBANPRulesWithNamedPorts(banp, namedIPorts, namedEPorts)
				pg = getDefaultPGForANPSubject(banp.Name, []string{t2.portUUID}, acls, true)
				baseExpectedDatabaseState[0] = subjectNSASIPv4
				baseExpectedDatabaseState[1] = subjectNSASIPv6
				expectedDatabaseState = append(baseExpectedDatabaseState, pg)
				expectedDatabaseState = append(expectedDatabaseState, getDefaultNetExpectedPodsAndSwitches([]testPod{t1, t2}, []string{node1Name})...)
				for _, acl := range acls {
					acl := acl
					expectedDatabaseState = append(expectedDatabaseState, acl)
				}
				// 3 ingress protocol ACLs and 1 ingress namedPort ACL,
				// 3 egress[0] protocol ACLs and 2 egress[0] namedPort ACLs,
				// total 9ACLs
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))

				ginkgo.By("3. Update ANP ingress rule ports to remove DNS namedPort and ensure ACLs are updated correctly")
				banp.ResourceVersion = "4"
				newPort := anpPortMangle[0:5]
				banp.Spec.Ingress[0].Ports = &newPort
				banp, err = fakeOVN.fakeClient.ANPClient.PolicyV1alpha1().BaselineAdminNetworkPolicies().Update(context.TODO(), banp, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				delete(namedIPorts, "dns")
				acls = getACLsForBANPRulesWithNamedPorts(banp, namedIPorts, namedEPorts)
				pg = getDefaultPGForANPSubject(banp.Name, []string{t2.portUUID}, acls, true)
				expectedDatabaseState = append(baseExpectedDatabaseState, pg)
				expectedDatabaseState = append(expectedDatabaseState, getDefaultNetExpectedPodsAndSwitches([]testPod{t1, t2}, []string{node1Name})...)
				for _, acl := range acls {
					acl := acl
					expectedDatabaseState = append(expectedDatabaseState, acl)
				}
				// 3 ingress protocol ACLs
				// 3 egress[0] protocol ACLs and 2 egress[0] namedPort ACLs,
				// total 8ACLs
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))

				ginkgo.By("4. Delete matching peer pod and ensure ACLs are updated correctly")
				banpPeerPod.ResourceVersion = "3"
				err = fakeOVN.fakeClient.KubeClient.CoreV1().Pods(banpPeerPod.Namespace).Delete(context.TODO(), banpPeerPod.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				delete(namedEPorts, "web")
				delete(namedEPorts, "web123")
				acls = getACLsForBANPRulesWithNamedPorts(banp, namedIPorts, namedEPorts)
				pg = getDefaultPGForANPSubject(banp.Name, []string{t2.portUUID}, acls, true)
				peerNSASIPv4, peerNSASIPv6 = buildNamespaceAddressSets(banpPeerNamespaceName, []string{}) // pod is gone from peer namespace address-set
				baseExpectedDatabaseState[2] = peerNSASIPv4
				baseExpectedDatabaseState[3] = peerNSASIPv6
				peerASEgressRule0v4, peerASEgressRule0v6 = buildBANPAddressSets(banp, 0, []string{}, libovsdbutil.ACLEgress)
				baseExpectedDatabaseState[8] = peerASEgressRule0v4
				baseExpectedDatabaseState[9] = peerASEgressRule0v6
				expectedDatabaseState = append(baseExpectedDatabaseState, pg)
				expectedDatabaseState = append(expectedDatabaseState, getDefaultNetExpectedPodsAndSwitches([]testPod{t2}, []string{node1Name})...)
				for _, acl := range acls {
					acl := acl
					expectedDatabaseState = append(expectedDatabaseState, acl)
				}
				// 3 ingress protocol ACLs
				// 3 egress[0] protocol ACLs
				// totalACLs: 6
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))

				ginkgo.By("5. Re-Add matching peer pod and ensure ACLs are updated correctly")
				banpPeerPod.ResourceVersion = "5"
				_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Pods(banpPeerPod.Namespace).Create(context.TODO(), &banpPeerPod, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// update namedPorts for constructing right test ACLs
				namedEPorts["web"] = []libovsdbutil.NamedNetworkPolicyPort{
					{L4Protocol: "tcp", L3PodIP: banpPodV4IP2, L3PodIPFamily: "ip4", L4PodPort: "3535"},
					{L4Protocol: "tcp", L3PodIP: banpPodV6IP2, L3PodIPFamily: "ip6", L4PodPort: "3535"}}
				namedEPorts["web123"] = []libovsdbutil.NamedNetworkPolicyPort{
					{L4Protocol: "sctp", L3PodIP: banpPodV4IP2, L3PodIPFamily: "ip4", L4PodPort: "35356"},
					{L4Protocol: "sctp", L3PodIP: banpPodV6IP2, L3PodIPFamily: "ip6", L4PodPort: "35356"}}
				acls = getACLsForBANPRulesWithNamedPorts(banp, namedIPorts, namedEPorts)
				pg = getDefaultPGForANPSubject(banp.Name, []string{t2.portUUID}, acls, true)
				peerNSASIPv4, peerNSASIPv6 = buildNamespaceAddressSets(banpPeerNamespaceName, []string{"fe00:10:128:1::4", "10.128.1.4"}) // pod is gone from peer namespace address-set
				baseExpectedDatabaseState[2] = peerNSASIPv4
				baseExpectedDatabaseState[3] = peerNSASIPv6
				peerASEgressRule0v4, peerASEgressRule0v6 = buildBANPAddressSets(banp, 0, []string{"fe00:10:128:1::4", "10.128.1.4"}, libovsdbutil.ACLEgress)
				baseExpectedDatabaseState[8] = peerASEgressRule0v4
				baseExpectedDatabaseState[9] = peerASEgressRule0v6
				expectedDatabaseState = append(baseExpectedDatabaseState, pg)
				expectedDatabaseState = append(expectedDatabaseState, getDefaultNetExpectedPodsAndSwitches([]testPod{t1, t2}, []string{node1Name})...)
				for _, acl := range acls {
					acl := acl
					expectedDatabaseState = append(expectedDatabaseState, acl)
				}
				// 3 ingress protocol ACLs
				// 3 egress[0] protocol ACLs; 2 namedPort ACLs
				// totalACLs: 8
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))

				ginkgo.By("6. Update ANP egress rule port to rename web namedPort and ensure ACLs are updated correctly")
				banp.ResourceVersion = "7"
				newPort = anpPortMangle
				newPort[3].NamedPort = ptr.To("http")
				banp.Spec.Egress[0].Ports = &newPort
				banp, err = fakeOVN.fakeClient.ANPClient.PolicyV1alpha1().BaselineAdminNetworkPolicies().Update(context.TODO(), banp, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				delete(namedEPorts, "web")
				acls = getACLsForBANPRulesWithNamedPorts(banp, namedIPorts, namedEPorts)
				pg = getDefaultPGForANPSubject(banp.Name, []string{t2.portUUID}, acls, true)
				expectedDatabaseState = append(baseExpectedDatabaseState, pg)
				expectedDatabaseState = append(expectedDatabaseState, getDefaultNetExpectedPodsAndSwitches([]testPod{t1, t2}, []string{node1Name})...)
				for _, acl := range acls {
					acl := acl
					expectedDatabaseState = append(expectedDatabaseState, acl)
				}
				// 3 ingress protocol ACLs
				// 3 egress[0] protocol ACLs and 1 egress[0] namedPort ACLs,
				// totalACLs: 7
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))

				ginkgo.By("7. Delete 1st ANP egress rule and ensure ACLs are updated correctly")
				banp.ResourceVersion = "9"
				banp.Spec.Egress = []anpapi.BaselineAdminNetworkPolicyEgressRule{}
				_, err = fakeOVN.fakeClient.ANPClient.PolicyV1alpha1().BaselineAdminNetworkPolicies().Update(context.TODO(), banp, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				delete(namedEPorts, "web123")
				acls = getACLsForBANPRulesWithNamedPorts(banp, namedIPorts, namedEPorts)
				pg = getDefaultPGForANPSubject(banp.Name, []string{t2.portUUID}, acls, true)
				expectedDatabaseState = []libovsdbtest.TestData{pg, subjectNSASIPv4, subjectNSASIPv6, peerNSASIPv4, peerNSASIPv6, peerNS2ASIPv4, peerNS2ASIPv6}
				expectedDatabaseState = append(expectedDatabaseState, getDefaultNetExpectedPodsAndSwitches([]testPod{t1, t2}, []string{node1Name})...)
				expectedDatabaseState = append(expectedDatabaseState, peerASIngressRule0v4)
				expectedDatabaseState = append(expectedDatabaseState, peerASIngressRule0v6)
				for _, acl := range acls {
					acl := acl
					expectedDatabaseState = append(expectedDatabaseState, acl)
				}
				// 3 ingress protocol ACLs
				// totalACLs: 3
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))
				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
		})
	})
})
