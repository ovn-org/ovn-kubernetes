package ovn

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	anpovn "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/controller/admin_network_policy"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
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

var anpLabel = map[string]string{"house": "gryffindor"}
var peerDenyLabel = map[string]string{"house": "slytherin"}
var peerAllowLabel = map[string]string{"house": "hufflepuff"}
var peerPassLabel = map[string]string{"house": "ravenclaw"}

// newANPSubjectObject returns a new ANP subject; assumes nsSelect is not nil -> mandatory when calling from tests
func newANPSubjectObject(nsSelect *metav1.LabelSelector, podSelect *metav1.LabelSelector) anpapi.AdminNetworkPolicySubject {
	subject := anpapi.AdminNetworkPolicySubject{}
	if podSelect == nil {
		subject.Namespaces = nsSelect
	} else {
		subject.Pods = &anpapi.NamespacedPod{
			NamespaceSelector: *nsSelect,
			PodSelector:       *podSelect,
		}
	}
	return subject
}

func newANPObject(name string, priority int32, subject anpapi.AdminNetworkPolicySubject,
	ingressRules []anpapi.AdminNetworkPolicyIngressRule, egressRules []anpapi.AdminNetworkPolicyEgressRule) *anpapi.AdminNetworkPolicy {
	return &anpapi.AdminNetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: anpapi.AdminNetworkPolicySpec{
			Subject:  subject,
			Priority: priority,
			Ingress:  ingressRules,
			Egress:   egressRules,
		},
	}
}

func getBaseRulePriority(anpPriority int32) int32 {
	ovnANPPriority := anpovn.ANPFlowStartPriority - anpPriority*anpovn.ANPMaxRulesPerObject // 32000 - x*100
	return ovnANPPriority
}

func getANPRulePriority(basePriority, index int32) int32 {
	return (basePriority - index)
}

func getDefaultPGForANPSubject(anpName string, portUUIDs []string, acls []*nbdb.ACL, banp bool) *nbdb.PortGroup {
	lsps := []*nbdb.LogicalSwitchPort{}
	for _, uuid := range portUUIDs {
		lsps = append(lsps, &nbdb.LogicalSwitchPort{UUID: uuid})
	}
	pgDbIDs := anpovn.GetANPPortGroupDbIDs(anpName, banp, DefaultNetworkControllerName)

	pg := libovsdbutil.BuildPortGroup(
		pgDbIDs,
		lsps,
		acls,
	)
	pg.UUID = pg.Name + "-UUID"
	return pg
}

func getANPGressACL(action, anpName, direction string, rulePriority int32,
	ruleIndex int32, ports *[]anpapi.AdminNetworkPolicyPort,
	namedPorts map[string][]libovsdbutil.NamedNetworkPolicyPort, banp bool) []*nbdb.ACL {
	retACLs := []*nbdb.ACL{}
	// we are not using BuildACL and instead manually building it on purpose so that the code path for BuildACL is also tested
	acl := nbdb.ACL{}
	acl.Action = action
	acl.Severity = nil
	acl.Log = false
	acl.Meter = utilpointer.String(types.OvnACLLoggingMeter)
	acl.Priority = int(rulePriority)
	acl.Tier = types.DefaultANPACLTier
	if banp {
		acl.Tier = types.DefaultBANPACLTier
	}
	acl.ExternalIDs = map[string]string{
		libovsdbops.OwnerControllerKey.String():    DefaultNetworkControllerName,
		libovsdbops.ObjectNameKey.String():         anpName,
		libovsdbops.GressIdxKey.String():           fmt.Sprintf("%d", ruleIndex),
		libovsdbops.PolicyDirectionKey.String():    direction,
		libovsdbops.PortPolicyProtocolKey.String(): "None",
		libovsdbops.OwnerTypeKey.String():          "AdminNetworkPolicy",
		libovsdbops.PrimaryIDKey.String():          fmt.Sprintf("%s:AdminNetworkPolicy:%s:%s:%d:None", DefaultNetworkControllerName, anpName, direction, ruleIndex),
	}
	acl.Name = utilpointer.String(fmt.Sprintf("ANP:%s:%s:%d", anpName, direction, ruleIndex)) // tests logic for GetACLName
	if banp {
		acl.ExternalIDs[libovsdbops.OwnerTypeKey.String()] = "BaselineAdminNetworkPolicy"
		acl.ExternalIDs[libovsdbops.PrimaryIDKey.String()] = fmt.Sprintf("%s:BaselineAdminNetworkPolicy:%s:%s:%d:None",
			DefaultNetworkControllerName, anpName, direction, ruleIndex)
		acl.Name = utilpointer.String(fmt.Sprintf("BANP:%s:%s:%d", anpName, direction, ruleIndex)) // tests logic for GetACLName
	}
	acl.UUID = fmt.Sprintf("%s_%s_%d-%f-UUID", anpName, direction, ruleIndex, rand.Float64())
	// determine ACL match
	pgName := libovsdbutil.GetPortGroupName(anpovn.GetANPPortGroupDbIDs(anpName, banp, DefaultNetworkControllerName))
	var lPortMatch, l3Match, matchDirection, match string
	if direction == string(libovsdbutil.ACLIngress) {
		acl.Direction = nbdb.ACLDirectionToLport
		lPortMatch = fmt.Sprintf("outport == @%s", pgName)
		matchDirection = "src"
	} else {
		acl.Direction = nbdb.ACLDirectionFromLport
		acl.Options = map[string]string{
			"apply-after-lb": "true",
		}
		lPortMatch = fmt.Sprintf("inport == @%s", pgName)
		matchDirection = "dst"
	}
	asIndex := anpovn.GetANPPeerAddrSetDbIDs(anpName, direction, fmt.Sprintf("%d", ruleIndex),
		DefaultNetworkControllerName, banp)
	asv4, asv6 := addressset.GetHashNamesForAS(asIndex)
	if config.IPv4Mode && config.IPv6Mode {
		l3Match = fmt.Sprintf("((ip4.%s == $%s || ip6.%s == $%s))", matchDirection, asv4, matchDirection, asv6)
	} else if config.IPv4Mode {
		l3Match = fmt.Sprintf("(ip4.%s == $%s)", matchDirection, asv4)
	} else {
		l3Match = fmt.Sprintf("(ip6.%s == $%s)", matchDirection, asv6)
	}
	// determine L4 Port match
	npps := []*libovsdbutil.NetworkPolicyPort{}
	nnpps := make(map[string][]libovsdbutil.NamedNetworkPolicyPort, 0)
	if ports != nil && len(*ports) != 0 {
		for _, port := range *ports {
			if port.NamedPort != nil {
				// hardcoding the logic in the tests for enabling us to construct the matches;
				// dns matches ingress and web/web123 matches egress@0th index
				val, ok := namedPorts[*port.NamedPort]
				if ok {
					nnpps[*port.NamedPort] = val
				}
			} else if port.PortNumber != nil {
				npps = append(npps, libovsdbutil.GetNetworkPolicyPort(port.PortNumber.Protocol, port.PortNumber.Port, 0))
			} else {
				npps = append(npps, libovsdbutil.GetNetworkPolicyPort(port.PortRange.Protocol, port.PortRange.Start, port.PortRange.End))
			}
		}
	}
	for protocol, l4Match := range libovsdbutil.GetL4MatchesFromNetworkPolicyPorts(npps) {
		aclCopy := acl
		aclCopy.ExternalIDs = make(map[string]string)
		for k, v := range acl.ExternalIDs {
			aclCopy.ExternalIDs[k] = v
		}
		if l4Match == libovsdbutil.UnspecifiedL4Match {
			// we don't need the lportMatch here but we are keeping it for safety reasons
			match = fmt.Sprintf("%s && %s", lPortMatch, l3Match)
		} else {
			match = fmt.Sprintf("%s && %s && %s", lPortMatch, l3Match, l4Match)
		}
		aclCopy.ExternalIDs[libovsdbops.PortPolicyProtocolKey.String()] = protocol
		aclCopy.Match = match
		aclCopy.ExternalIDs[libovsdbops.PrimaryIDKey.String()] = fmt.Sprintf("%s:AdminNetworkPolicy:%s:%s:%d:%s",
			DefaultNetworkControllerName, anpName, direction, ruleIndex, protocol)
		if banp {
			aclCopy.ExternalIDs[libovsdbops.PrimaryIDKey.String()] = fmt.Sprintf("%s:BaselineAdminNetworkPolicy:%s:%s:%d:%s",
				DefaultNetworkControllerName, anpName, direction, ruleIndex, protocol)
		}
		aclCopy.UUID = fmt.Sprintf("%s_%s_%d.%s-%f-UUID", anpName, direction, ruleIndex, protocol, rand.Float64())
		retACLs = append(retACLs, &aclCopy)
	}
	// Process match for NamedPorts if any
	for protocol, l3l4Match := range libovsdbutil.GetL3L4MatchesFromNamedPorts(nnpps) {
		aclCopy := acl
		aclCopy.ExternalIDs = make(map[string]string)
		for k, v := range acl.ExternalIDs {
			aclCopy.ExternalIDs[k] = v
		}
		if direction == string(libovsdbutil.ACLIngress) {
			match = fmt.Sprintf("%s && %s", l3Match, l3l4Match)
		} else {
			match = fmt.Sprintf("%s && %s", lPortMatch, l3l4Match)
		}
		aclCopy.ExternalIDs[libovsdbops.PortPolicyProtocolKey.String()] = protocol + "-namedPort"
		aclCopy.Match = match
		aclCopy.ExternalIDs[libovsdbops.PrimaryIDKey.String()] = fmt.Sprintf("%s:AdminNetworkPolicy:%s:%s:%d:%s",
			DefaultNetworkControllerName, anpName, direction, ruleIndex, protocol+"-namedPort")
		if banp {
			aclCopy.ExternalIDs[libovsdbops.PrimaryIDKey.String()] = fmt.Sprintf("%s:BaselineAdminNetworkPolicy:%s:%s:%d:%s",
				DefaultNetworkControllerName, anpName, direction, ruleIndex, protocol+"-namedPort")
		}
		retACLs = append(retACLs, &aclCopy)
	}
	return retACLs
}

func getACLsForANPRulesWithNamedPorts(anp *anpapi.AdminNetworkPolicy, namedIPorts, namedEPorts map[string][]libovsdbutil.NamedNetworkPolicyPort) []*nbdb.ACL {
	aclResults := []*nbdb.ACL{}
	ovnBaseANPPriority := getBaseRulePriority(anp.Spec.Priority)
	for i, ingress := range anp.Spec.Ingress {
		acls := getANPGressACL(anpovn.GetACLActionForANPRule(ingress.Action), anp.Name, string(libovsdbutil.ACLIngress),
			getANPRulePriority(ovnBaseANPPriority, int32(i)), int32(i), ingress.Ports, namedIPorts, false)
		aclResults = append(aclResults, acls...)
	}
	for i, egress := range anp.Spec.Egress {
		// web/web123 matches egressRule@0th index; egressRule@1st index doesn't have matching pods
		if i != 0 {
			namedEPorts = nil
		}
		acls := getANPGressACL(anpovn.GetACLActionForANPRule(egress.Action), anp.Name, string(libovsdbutil.ACLEgress),
			getANPRulePriority(ovnBaseANPPriority, int32(i)), int32(i), egress.Ports, namedEPorts, false)
		aclResults = append(aclResults, acls...)
	}
	return aclResults
}

func getACLsForANPRules(anp *anpapi.AdminNetworkPolicy) []*nbdb.ACL {
	aclResults := []*nbdb.ACL{}
	ovnBaseANPPriority := getBaseRulePriority(anp.Spec.Priority)
	for i, ingress := range anp.Spec.Ingress {
		acls := getANPGressACL(anpovn.GetACLActionForANPRule(ingress.Action), anp.Name, string(libovsdbutil.ACLIngress),
			getANPRulePriority(ovnBaseANPPriority, int32(i)), int32(i), ingress.Ports, nil, false)
		aclResults = append(aclResults, acls...)
	}
	for i, egress := range anp.Spec.Egress {
		acls := getANPGressACL(anpovn.GetACLActionForANPRule(egress.Action), anp.Name, string(libovsdbutil.ACLEgress),
			getANPRulePriority(ovnBaseANPPriority, int32(i)), int32(i), egress.Ports, nil, false)
		aclResults = append(aclResults, acls...)
	}
	return aclResults
}

func buildANPAddressSets(anp *anpapi.AdminNetworkPolicy, index int32, ips []string, gressPrefix libovsdbutil.ACLDirection) (*nbdb.AddressSet, *nbdb.AddressSet) {
	asIndex := anpovn.GetANPPeerAddrSetDbIDs(anp.Name, string(gressPrefix),
		fmt.Sprintf("%d", index), DefaultNetworkControllerName, false)
	return addressset.GetTestDbAddrSets(asIndex, ips)
}

var _ = ginkgo.Describe("OVN ANP Operations", func() {
	var (
		app     *cli.App
		fakeOVN *FakeOVN
	)

	const (
		anpSubjectNamespaceName        = "anp-subject-namespace"
		anpPeerNamespaceName           = "anp-peer-namespace"
		anpSubjectPodName              = "anp-subject-pod"
		anpPodV4IP                     = "10.128.1.3"
		anpPodV6IP                     = "fe00:10:128:1::3"
		anpPodMAC                      = "0a:58:0a:80:01:03"
		anpPodV4IP2                    = "10.128.1.4"
		anpPodV6IP2                    = "fe00:10:128:1::4"
		anpPodMAC2                     = "0a:58:0a:80:01:04"
		anpPeerPodName                 = "anp-peer-pod"
		node1Name               string = "node1"
		node1IPv4               string = "100.100.100.0"
		node1IPv6               string = "fc00:f853:ccd:e793::1"
		node1IPv4Subnet         string = "10.128.1.0/24"
		node1IPv6Subnet         string = "fe00:10:128:1::/64"
		node1transitIPv4        string = "100.88.0.2"
		node1transitIPv6        string = "fd97::2"
		node2Name               string = "node2"
		node2IPv4               string = "200.200.200.0"
		node2IPv6               string = "fc00:f853:ccd:e793::2"
		node2IPv4Subnet         string = "10.128.2.0/24"
		node2IPv6Subnet         string = "fe00:10:128:2::/64"
		node2transitIPv4        string = "100.88.0.3"
		node2transitIPv6        string = "fd97::3"
	)

	ginkgo.BeforeEach(func() {
		// Restore global default values before each testcase
		config.PrepareTestConfig()
		config.OVNKubernetesFeature.EnableAdminNetworkPolicy = true
		// IC true or false does not really effect this feature
		config.OVNKubernetesFeature.EnableInterconnect = true

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

		fakeOVN = NewFakeOVN(false)
	})

	ginkgo.AfterEach(func() {
		fakeOVN.shutdown()
	})

	ginkgo.Context("On admin network policy changes", func() {
		ginkgo.It("should create/update/delete address-sets, acls, port-groups correctly", func() {
			app.Action = func(ctx *cli.Context) error {
				anpNamespaceSubject := *newNamespaceWithLabels(anpSubjectNamespaceName, anpLabel)
				anpNamespacePeer := *newNamespaceWithLabels(anpPeerNamespaceName, peerDenyLabel)
				config.IPv4Mode = true
				config.IPv6Mode = true
				node1 := nodeFor(node1Name, "100.100.100.0", "fc00:f853:ccd:e793::1", "10.128.1.0/24", "fe00:10:128:1::/64", "", "")

				node1Switch := &nbdb.LogicalSwitch{
					Name: node1Name,
					UUID: node1Name + "-UUID",
				}
				subjectNSASIPv4, subjectNSASIPv6 := buildNamespaceAddressSets(anpSubjectNamespaceName, []string{})
				peerNSASIPv4, peerNSASIPv6 := buildNamespaceAddressSets(anpPeerNamespaceName, []string{})
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
							anpNamespaceSubject,
							anpNamespacePeer,
						},
					},
					&v1.NodeList{
						Items: []v1.Node{
							*node1,
						},
					},
				)

				fakeOVN.controller.zone = node1Name // ensure we set the controller's zone as the node's zone
				err := fakeOVN.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOVN.controller.WatchPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOVN.InitAndRunANPController()
				fakeOVN.fakeClient.ANPClient.(*anpfake.Clientset).PrependReactor("update", "adminnetworkpolicies", func(action clienttesting.Action) (handled bool, ret runtime.Object, err error) {
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

				ginkgo.By("1. creating an admin network policy with 0 rules; check if port group is created for the subject")
				anpSubject := newANPSubjectObject(
					&metav1.LabelSelector{
						MatchLabels: anpLabel,
					},
					nil,
				)
				anp := newANPObject("harry-potter", 5, anpSubject, []anpapi.AdminNetworkPolicyIngressRule{}, []anpapi.AdminNetworkPolicyEgressRule{})
				anp.ResourceVersion = "1"
				anp, err = fakeOVN.fakeClient.ANPClient.PolicyV1alpha1().AdminNetworkPolicies().Create(context.TODO(), anp, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				pg := getDefaultPGForANPSubject(anp.Name, nil, nil, false)
				expectedDatabaseState := []libovsdbtest.TestData{node1Switch, subjectNSASIPv4, subjectNSASIPv6, peerNSASIPv4, peerNSASIPv6, pg}
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				ginkgo.By("2. creating a pod that will act as subject of admin network policy; check if lsp is added to port-group after retries")
				t := newTPod(
					node1Name,
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					anpSubjectPodName,
					anpPodV4IP,
					anpPodMAC,
					anpSubjectNamespaceName,
				)
				t.portName = util.GetLogicalPortName(t.namespace, t.podName)
				t.populateLogicalSwitchCache(fakeOVN)
				anpSubjectPod := *newPod(anpSubjectNamespaceName, anpSubjectPodName, node1Name, anpPodV4IP)
				_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Pods(anpSubjectPod.Namespace).Create(context.TODO(), &anpSubjectPod, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// The pod takes some time to get created so the first add pod event for ANP does nothing as it waits for LSP to be created - this triggers a retry.
				// Next when the pod is scheduled and IP is allocated and LSP is created we get an update event.
				// When ANP controller receives the pod update event, it will not trigger an update in unit testing since ResourceVersion and pod.status.Running
				// is not updated by the test, hence we need to trigger a manual update in the unit test framework that is accompanied by label update.
				// NOTE: This is not an issue in real deployments with actual kapi server; its just a potential flake in the UT framework that we need to protect against.
				time.Sleep(time.Second)
				anpSubjectPod.ResourceVersion = "5"
				// - this is to trigger an actual update technically speaking the pod already matches the namespace of the subject
				anpSubjectPod.Labels["rv"] = "resourceVersionUTHack"
				_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Pods(anpSubjectPod.Namespace).Update(context.TODO(), &anpSubjectPod, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				subjectNSASIPv4, subjectNSASIPv6 = buildNamespaceAddressSets(anpSubjectNamespaceName, []string{t.podIP})
				pg = getDefaultPGForANPSubject(anp.Name, []string{t.portUUID}, nil, false)
				expectedDatabaseState = []libovsdbtest.TestData{pg, subjectNSASIPv4, subjectNSASIPv6, peerNSASIPv4, peerNSASIPv6}
				expectedDatabaseState = append(expectedDatabaseState, getDefaultNetExpectedPodsAndSwitches([]testPod{t}, []string{node1Name})...)
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				ginkgo.By("3. update the ANP by adding one ingress rule; check if acl is created and added on the port-group")
				ingressRules := []anpapi.AdminNetworkPolicyIngressRule{
					{
						Name:   "deny-traffic-from-slytherin-to-gryffindor",
						Action: anpapi.AdminNetworkPolicyRuleActionDeny,
						From: []anpapi.AdminNetworkPolicyIngressPeer{
							{
								Namespaces: &metav1.LabelSelector{
									MatchLabels: peerDenyLabel,
								},
							},
						},
					},
				}
				anp = newANPObject("harry-potter", 5, anpSubject, ingressRules, []anpapi.AdminNetworkPolicyEgressRule{})
				anp.ResourceVersion = "2"
				anp, err = fakeOVN.fakeClient.ANPClient.PolicyV1alpha1().AdminNetworkPolicies().Update(context.TODO(), anp, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				acls := getACLsForANPRules(anp)
				pg = getDefaultPGForANPSubject(anp.Name, []string{t.portUUID}, acls, false)
				expectedDatabaseState[0] = pg // acl should be added to the port group
				for _, acl := range acls {
					acl := acl
					expectedDatabaseState = append(expectedDatabaseState, acl)
				}
				peerASIngressRule0v4, peerASIngressRule0v6 := buildANPAddressSets(anp, 0, []string{}, libovsdbutil.ACLIngress) // address-set will be empty since no pods match it yet
				expectedDatabaseState = append(expectedDatabaseState, peerASIngressRule0v4)
				expectedDatabaseState = append(expectedDatabaseState, peerASIngressRule0v6)
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				ginkgo.By("4. creating a pod that will act as peer of admin network policy; check if IP is added to address-set after retries")
				t2 := newTPod(
					node1Name,
					"10.128.1.1/24",
					"10.128.1.2",
					"10.128.1.1",
					anpPeerPodName,
					anpPodV4IP2,
					anpPodMAC2,
					anpPeerNamespaceName,
				)
				t2.portName = util.GetLogicalPortName(t2.namespace, t2.podName)
				t2.populateLogicalSwitchCache(fakeOVN)
				anpPeerPod := *newPod(anpPeerNamespaceName, anpPeerPodName, node1Name, anpPodV4IP2)
				_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Pods(anpPeerPod.Namespace).Create(context.TODO(), &anpPeerPod, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// The pod takes some time to get created so the first add pod event for ANP does nothing as it waits for LSP to be created - this triggers a retry.
				// Next when the pod is scheduled and IP is allocated and LSP is created we get an update event.
				// When ANP controller receives the pod update event, it will not trigger an update in unit testing since ResourceVersion and pod.status.Running
				// is not updated by the test, hence we need to trigger a manual update in the unit test framework that is accompanied by label update.
				// NOTE: This is not an issue in real deployments with actual kapi server; its just a potential flake in the UT framework that we need to protect against.
				time.Sleep(time.Second)
				anpPeerPod.ResourceVersion = "5"
				// - this is to trigger an actual update technically speaking the pod already matches the namespace of the subject
				anpPeerPod.Labels["rv"] = "resourceVersionUTHack"
				_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Pods(anpPeerPod.Namespace).Update(context.TODO(), &anpPeerPod, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				peerNSASIPv4, peerNSASIPv6 = buildNamespaceAddressSets(anpPeerNamespaceName, []string{t2.podIP})
				expectedDatabaseState = []libovsdbtest.TestData{pg, subjectNSASIPv4, subjectNSASIPv6, peerNSASIPv4, peerNSASIPv6}
				for _, acl := range acls {
					acl := acl
					expectedDatabaseState = append(expectedDatabaseState, acl)
				}
				expectedDatabaseState = append(expectedDatabaseState, getDefaultNetExpectedPodsAndSwitches([]testPod{t, t2}, []string{node1Name})...)
				peerASIngressRule0v4, peerASIngressRule0v6 = buildANPAddressSets(anp, 0, []string{t2.podIP}, libovsdbutil.ACLIngress) // podIP should be added to v4 address-set
				expectedDatabaseState = append(expectedDatabaseState, peerASIngressRule0v4)
				expectedDatabaseState = append(expectedDatabaseState, peerASIngressRule0v6)
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				ginkgo.By("5. update the ANP by adding two more ingress rules with allow and pass actions; check if AS-es are created + acls are created and added on the port-group")
				ingressRules = append(ingressRules,
					anpapi.AdminNetworkPolicyIngressRule{
						Name:   "allow-traffic-from-hufflepuff-to-gryffindor",
						Action: anpapi.AdminNetworkPolicyRuleActionAllow,
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
					anpapi.AdminNetworkPolicyIngressRule{
						Name:   "pass-traffic-from-ravenclaw-to-gryffindor",
						Action: anpapi.AdminNetworkPolicyRuleActionPass,
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
									Port:     int32(1235),
								},
							},
							{
								PortNumber: &anpapi.Port{
									Protocol: v1.ProtocolSCTP,
									Port:     int32(1345),
								},
							},
						},
					},
				)
				anp = newANPObject("harry-potter", 5, anpSubject, ingressRules, []anpapi.AdminNetworkPolicyEgressRule{})
				anp.ResourceVersion = "3"
				anp, err = fakeOVN.fakeClient.ANPClient.PolicyV1alpha1().AdminNetworkPolicies().Update(context.TODO(), anp, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				acls = getACLsForANPRules(anp)
				pg = getDefaultPGForANPSubject(anp.Name, []string{t.portUUID}, acls, false)
				expectedDatabaseState = []libovsdbtest.TestData{pg, subjectNSASIPv4, subjectNSASIPv6, peerNSASIPv4, peerNSASIPv6}
				for _, acl := range acls {
					acl := acl
					expectedDatabaseState = append(expectedDatabaseState, acl)
				}
				expectedDatabaseState = append(expectedDatabaseState, getDefaultNetExpectedPodsAndSwitches([]testPod{t, t2}, []string{node1Name})...)
				peerASIngressRule0v4, peerASIngressRule0v6 = buildANPAddressSets(anp,
					0, []string{t2.podIP}, libovsdbutil.ACLIngress) // podIP should be added to v4
				expectedDatabaseState = append(expectedDatabaseState, peerASIngressRule0v4)
				expectedDatabaseState = append(expectedDatabaseState, peerASIngressRule0v6)
				peerASIngressRule1v4, peerASIngressRule1v6 := buildANPAddressSets(anp,
					1, []string{}, libovsdbutil.ACLIngress) // address-set will be empty since no pods match it yet
				expectedDatabaseState = append(expectedDatabaseState, peerASIngressRule1v4)
				expectedDatabaseState = append(expectedDatabaseState, peerASIngressRule1v6)
				peerASIngressRule2v4, peerASIngressRule2v6 := buildANPAddressSets(anp,
					2, []string{}, libovsdbutil.ACLIngress) // address-set will be empty since no pods match it yet

				expectedDatabaseState = append(expectedDatabaseState, peerASIngressRule2v4)
				expectedDatabaseState = append(expectedDatabaseState, peerASIngressRule2v6)
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				ginkgo.By("6. update the ANP by adding three egress rules with deny, allow and pass actions;" +
					"check if acls are created and added on the port-group")
				egressRules := []anpapi.AdminNetworkPolicyEgressRule{
					{
						Name:   "deny-traffic-to-slytherin-from-gryffindor",
						Action: anpapi.AdminNetworkPolicyRuleActionDeny,
						To: []anpapi.AdminNetworkPolicyEgressPeer{
							{
								Namespaces: &metav1.LabelSelector{
									MatchLabels: peerDenyLabel,
								},
							},
						},
					},
					{
						Name:   "allow-traffic-to-hufflepuff-from-gryffindor",
						Action: anpapi.AdminNetworkPolicyRuleActionAllow,
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
						Name:   "pass-traffic-to-ravenclaw-from-gryffindor",
						Action: anpapi.AdminNetworkPolicyRuleActionPass,
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
				anp = newANPObject("harry-potter", 5, anpSubject, ingressRules, egressRules)
				anp.ResourceVersion = "4"
				anp, err = fakeOVN.fakeClient.ANPClient.PolicyV1alpha1().AdminNetworkPolicies().Update(context.TODO(), anp, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				acls = getACLsForANPRules(anp)
				pg = getDefaultPGForANPSubject(anp.Name, []string{t.portUUID}, acls, false)
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
				peerASEgressRule0v4, peerASEgressRule0v6 := buildANPAddressSets(anp,
					0, []string{t2.podIP}, libovsdbutil.ACLEgress) // podIP should be added to v4
				expectedDatabaseState = append(expectedDatabaseState, peerASEgressRule0v4)
				expectedDatabaseState = append(expectedDatabaseState, peerASEgressRule0v6)
				peerASEgressRule1v4, peerASEgressRule1v6 := buildANPAddressSets(anp,
					1, []string{}, libovsdbutil.ACLEgress) // address-set will be empty since no pods match it yet
				expectedDatabaseState = append(expectedDatabaseState, peerASEgressRule1v4)
				expectedDatabaseState = append(expectedDatabaseState, peerASEgressRule1v6)
				peerASEgressRule2v4, peerASEgressRule2v6 := buildANPAddressSets(anp,
					2, []string{}, libovsdbutil.ACLEgress) // address-set will be empty since no pods match it yet
				expectedDatabaseState = append(expectedDatabaseState, peerASEgressRule2v4)
				expectedDatabaseState = append(expectedDatabaseState, peerASEgressRule2v6)
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				ginkgo.By("7. update the labels of the pod that matches the peer selector; check if IPs are updated in address-sets")
				anpPeerPod.ResourceVersion = "2"
				anpPeerPod.Labels = peerAllowLabel // pod is now in namespace {house: slytherin} && pod {house: hufflepuff}
				_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Pods(anpPeerPod.Namespace).Update(context.TODO(), &anpPeerPod, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// ingressRule AddressSets - podIP should stay in deny rule's address-set since deny rule matcheS only on nsSelector which has not changed
				// egressRule AddressSets - podIP should now be present in both deny and allow rule's address-sets
				expectedDatabaseState[len(expectedDatabaseState)-4].(*nbdb.AddressSet).Addresses = []string{t2.podIP} // podIP should be added to v4 allow address-set
				expectedDatabaseState[len(expectedDatabaseState)-6].(*nbdb.AddressSet).Addresses = []string{t2.podIP} // podIP should be added to v4 deny address-set
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				ginkgo.By("8. update the labels of the namespace that matches the peer selector; check if IPs are updated in address-sets")
				anpNamespacePeer.ResourceVersion = "2"
				anpNamespacePeer.Labels = peerPassLabel // pod is now in namespace {house: ravenclaw} && pod {house: hufflepuff}
				_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Namespaces().Update(context.TODO(), &anpNamespacePeer, metav1.UpdateOptions{})
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

				ginkgo.By("9. update the subject of admin network policy so that subject pod stops matching; check if port group is updated")
				anpSubject = newANPSubjectObject(
					&metav1.LabelSelector{ // test different match expression
						MatchLabels: anpLabel,
					},
					&metav1.LabelSelector{
						MatchLabels: peerDenyLabel, // subject pod won't match any more
					},
				)
				anp = newANPObject("harry-potter", 5, anpSubject, ingressRules, egressRules)
				anp.ResourceVersion = "5"
				anp, err = fakeOVN.fakeClient.ANPClient.PolicyV1alpha1().AdminNetworkPolicies().Update(context.TODO(), anp, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				pg = getDefaultPGForANPSubject(anp.Name, nil, acls, false) // no ports in PG
				expectedDatabaseState[0] = pg
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				ginkgo.By("10. update the resource version and labels of the pod that matches the new subject selector; check if port group is updated")
				anpSubjectPod.ResourceVersion = "2"
				anpSubjectPod.Labels = peerDenyLabel // pod is now in namespace {house: gryffindor} && pod {house: slytherin}
				_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Pods(anpSubjectPod.Namespace).Update(context.TODO(), &anpSubjectPod, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				pg = getDefaultPGForANPSubject(anp.Name, []string{t.portUUID}, acls, false) // port added back to PG
				expectedDatabaseState[0] = pg
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				ginkgo.By("11. update the labels of the namespace that matches the subject selector to stop matching; check if port group is updated")
				anpNamespaceSubject.ResourceVersion = "1"
				anpNamespaceSubject.Labels = peerPassLabel // pod is now in namespace {house: ravenclaw} && pod {house: slytherin}; stops matching
				_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Namespaces().Update(context.TODO(), &anpNamespaceSubject, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				pg = getDefaultPGForANPSubject(anp.Name, nil, acls, false) // no ports in PG
				expectedDatabaseState[0] = pg
				// Coincidentally the subject pod also ends up matching the peer label for Pass rule, so let's add that IP to the AS
				expectedDatabaseState[len(expectedDatabaseState)-8].(*nbdb.AddressSet).Addresses = []string{t.podIP, t2.podIP} // podIP should be added to v4 Pass address-set
				expectedDatabaseState[len(expectedDatabaseState)-2].(*nbdb.AddressSet).Addresses = []string{t.podIP, t2.podIP} // podIP should be added to v4 Pass address-set
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				ginkgo.By("12. update the labels of the namespace that matches the subject selector to start matching; check if port group is updated")
				anpNamespaceSubject.ResourceVersion = "2"
				anpNamespaceSubject.Labels = anpLabel // pod is now in namespace {house: gryffindor} && pod {house: slytherin}; starts matching
				_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Namespaces().Update(context.TODO(), &anpNamespaceSubject, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				pg = getDefaultPGForANPSubject(anp.Name, []string{t.portUUID}, acls, false) // no ports in PG
				expectedDatabaseState[0] = pg
				// Let us remove the IPs from the peer label AS as it stops matching
				expectedDatabaseState[len(expectedDatabaseState)-8].(*nbdb.AddressSet).Addresses = []string{t2.podIP} // podIP should be added to v4 Pass address-set
				expectedDatabaseState[len(expectedDatabaseState)-2].(*nbdb.AddressSet).Addresses = []string{t2.podIP} // podIP should be added to v4 Pass address-set
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				ginkgo.By("13. update the ANP by changing its priority and deleting 1 ingress rule & 1 egress rule; check if all objects are re-created correctly")
				anp.ResourceVersion = "6"
				anp.Spec.Priority = 6
				anp.Spec.Ingress = []anpapi.AdminNetworkPolicyIngressRule{ingressRules[1], ingressRules[2]} // deny is gone
				anp.Spec.Egress = []anpapi.AdminNetworkPolicyEgressRule{egressRules[0], egressRules[2]}     // allow is gone
				anp, err = fakeOVN.fakeClient.ANPClient.PolicyV1alpha1().AdminNetworkPolicies().Update(context.TODO(), anp, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				newACLs := getACLsForANPRules(anp)
				pg = getDefaultPGForANPSubject(anp.Name, []string{t.portUUID}, newACLs, false) // only newACLs are hosted
				expectedDatabaseState = []libovsdbtest.TestData{pg, subjectNSASIPv4, subjectNSASIPv6, peerNSASIPv4, peerNSASIPv6}
				for _, acl := range newACLs {
					acl := acl
					expectedDatabaseState = append(expectedDatabaseState, acl)
				}
				expectedDatabaseState = append(expectedDatabaseState, getDefaultNetExpectedPodsAndSwitches([]testPod{t, t2}, []string{node1Name})...)
				// ensure address-sets for the rules that were deleted are also gone (index 2 in the list)
				// ensure new address-sets have expected IPs
				peerASIngressRule0v4, peerASIngressRule0v6 = buildANPAddressSets(anp,
					0, []string{}, libovsdbutil.ACLIngress) // address-set will be empty since no pods match it yet
				expectedDatabaseState = append(expectedDatabaseState, peerASIngressRule0v4)
				expectedDatabaseState = append(expectedDatabaseState, peerASIngressRule0v6)
				peerASIngressRule1v4, peerASIngressRule1v6 = buildANPAddressSets(anp,
					1, []string{t2.podIP}, libovsdbutil.ACLIngress) // podIP should be added to v4 Pass address-set
				expectedDatabaseState = append(expectedDatabaseState, peerASIngressRule1v4)
				expectedDatabaseState = append(expectedDatabaseState, peerASIngressRule1v6)
				peerASEgressRule0v4, peerASEgressRule0v6 = buildANPAddressSets(anp,
					0, []string{}, libovsdbutil.ACLEgress) // address-set will be empty since no pods match it yet
				expectedDatabaseState = append(expectedDatabaseState, peerASEgressRule0v4)
				expectedDatabaseState = append(expectedDatabaseState, peerASEgressRule0v6)
				peerASEgressRule1v4, peerASEgressRule1v6 = buildANPAddressSets(anp,
					1, []string{t2.podIP}, libovsdbutil.ACLEgress) // podIP should be added to v4 Pass address-set
				expectedDatabaseState = append(expectedDatabaseState, peerASEgressRule1v4)
				expectedDatabaseState = append(expectedDatabaseState, peerASEgressRule1v6)
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				ginkgo.By("14. delete pod matching peer selector; check if IPs are updated in address-sets")
				anpPeerPod.ResourceVersion = "3"
				err = fakeOVN.fakeClient.KubeClient.CoreV1().Pods(anpPeerPod.Namespace).Delete(context.TODO(), anpPeerPod.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				pg = getDefaultPGForANPSubject(anp.Name, []string{t.portUUID}, newACLs, false)           // only newACLs are hosted
				peerNSASIPv4, peerNSASIPv6 = buildNamespaceAddressSets(anpPeerNamespaceName, []string{}) // pod is gone from peer namespace address-set
				expectedDatabaseState = []libovsdbtest.TestData{pg, subjectNSASIPv4, subjectNSASIPv6, peerNSASIPv4, peerNSASIPv6}
				for _, acl := range newACLs {
					acl := acl
					expectedDatabaseState = append(expectedDatabaseState, acl)
				}
				expectedDatabaseState = append(expectedDatabaseState, getDefaultNetExpectedPodsAndSwitches([]testPod{t}, []string{node1Name})...)
				peerASIngressRule0v4, peerASIngressRule0v6 = buildANPAddressSets(anp,
					0, []string{}, libovsdbutil.ACLIngress) // address-set will be empty since no pods match it yet
				expectedDatabaseState = append(expectedDatabaseState, peerASIngressRule0v4)
				expectedDatabaseState = append(expectedDatabaseState, peerASIngressRule0v6)
				peerASIngressRule1v4, peerASIngressRule1v6 = buildANPAddressSets(anp,
					1, []string{}, libovsdbutil.ACLIngress) // address-set will be empty since no pods match it yet
				expectedDatabaseState = append(expectedDatabaseState, peerASIngressRule1v4)
				expectedDatabaseState = append(expectedDatabaseState, peerASIngressRule1v6)
				peerASEgressRule0v4, peerASEgressRule0v6 = buildANPAddressSets(anp,
					0, []string{}, libovsdbutil.ACLEgress) // address-set will be empty since no pods match it yet
				expectedDatabaseState = append(expectedDatabaseState, peerASEgressRule0v4)
				expectedDatabaseState = append(expectedDatabaseState, peerASEgressRule0v6)
				peerASEgressRule1v4, peerASEgressRule1v6 = buildANPAddressSets(anp,
					1, []string{}, libovsdbutil.ACLEgress) // address-set will be empty since no pods match it yet
				expectedDatabaseState = append(expectedDatabaseState, peerASEgressRule1v4)
				expectedDatabaseState = append(expectedDatabaseState, peerASEgressRule1v6)
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				ginkgo.By("15. update the subject pod to go into completed state; check if port group and address-set is updated")
				anpSubjectPod.ResourceVersion = "3"
				anpSubjectPod.Status.Phase = v1.PodSucceeded
				_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Pods(anpSubjectPod.Namespace).Update(context.TODO(), &anpSubjectPod, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				pg = getDefaultPGForANPSubject(anp.Name, nil, newACLs, false) // no ports in PG
				subjectNSASIPv4, subjectNSASIPv6 = buildNamespaceAddressSets(anpSubjectNamespaceName, []string{})
				expectedDatabaseState = []libovsdbtest.TestData{pg, subjectNSASIPv4, subjectNSASIPv6, peerNSASIPv4, peerNSASIPv6}
				for _, acl := range newACLs {
					acl := acl
					expectedDatabaseState = append(expectedDatabaseState, acl)
				}
				expectedDatabaseState = append(expectedDatabaseState, getDefaultNetExpectedPodsAndSwitches([]testPod{}, []string{node1Name})...)
				expectedDatabaseState = append(expectedDatabaseState, []libovsdbtest.TestData{peerASIngressRule0v4, peerASIngressRule0v6, peerASIngressRule1v4,
					peerASIngressRule1v6, peerASEgressRule0v4, peerASEgressRule0v6, peerASEgressRule1v4, peerASEgressRule1v6}...)
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
				err = fakeOVN.fakeClient.KubeClient.CoreV1().Pods(anpSubjectPod.Namespace).Delete(context.TODO(), anpSubjectPod.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				ginkgo.By("16. delete the subject and peer selected namespaces; check if port group and address-set's are updated")
				// create a new pod in subject and peer namespaces so that we can check namespace deletion properly
				anpSubjectPod = *newPodWithLabels(anpSubjectNamespaceName, anpSubjectPodName, node1Name, "10.128.1.5", peerDenyLabel)
				_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Pods(anpSubjectPod.Namespace).Create(context.TODO(), &anpSubjectPod, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// The pod takes some time to get created so the first add pod event for ANP does nothing as it waits for LSP to be created - this triggers a retry.
				// Next when the pod is scheduled and IP is allocated and LSP is created we get an update event.
				// When ANP controller receives the pod update event, it will not trigger an update in unit testing since ResourceVersion and pod.status.Running
				// is not updated by the test, hence we need to trigger a manual update in the unit test framework that is accompanied by label update.
				// NOTE: This is not an issue in real deployments with actual kapi server; its just a potential flake in the UT framework that we need to protect against.
				time.Sleep(time.Second)
				anpSubjectPod.ResourceVersion = "500"
				// - this is to trigger an actual update technically speaking the pod already matches the namespace of the subject
				anpSubjectPod.Labels["rv"] = "resourceVersionUTHack"
				_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Pods(anpSubjectPod.Namespace).Update(context.TODO(), &anpSubjectPod, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				t.podIP = "10.128.1.5"
				t.podMAC = "0a:58:0a:80:01:05"
				pg = getDefaultPGForANPSubject(anp.Name, []string{t.portUUID}, newACLs, false)
				subjectNSASIPv4, subjectNSASIPv6 = buildNamespaceAddressSets(anpSubjectNamespaceName, []string{t.podIP})
				expectedDatabaseState = []libovsdbtest.TestData{pg, subjectNSASIPv4, subjectNSASIPv6, peerNSASIPv4, peerNSASIPv6}
				for _, acl := range newACLs {
					acl := acl
					expectedDatabaseState = append(expectedDatabaseState, acl)
				}
				expectedDatabaseState = append(expectedDatabaseState, getDefaultNetExpectedPodsAndSwitches([]testPod{t}, []string{node1Name})...)
				expectedDatabaseState = append(expectedDatabaseState, []libovsdbtest.TestData{peerASIngressRule0v4, peerASIngressRule0v6, peerASIngressRule1v4,
					peerASIngressRule1v6, peerASEgressRule0v4, peerASEgressRule0v6, peerASEgressRule1v4, peerASEgressRule1v6}...)
				gomega.Eventually(fakeOVN.nbClient, "3s").Should(libovsdbtest.HaveData(expectedDatabaseState))
				anpPeerPod = *newPodWithLabels(anpPeerNamespaceName, anpPeerPodName, node1Name, "10.128.1.6", peerAllowLabel)
				_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Pods(anpPeerPod.Namespace).Create(context.TODO(), &anpPeerPod, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// The pod takes some time to get created so the first add pod event for ANP does nothing as it waits for LSP to be created - this triggers a retry.
				// Next when the pod is scheduled and IP is allocated and LSP is created we get an update event.
				// When ANP controller receives the pod update event, it will not trigger an update in unit testing since ResourceVersion and pod.status.Running
				// is not updated by the test, hence we need to trigger a manual update in the unit test framework that is accompanied by label update.
				// NOTE: This is not an issue in real deployments with actual kapi server; its just a potential flake in the UT framework that we need to protect against.
				time.Sleep(time.Second)
				anpPeerPod.ResourceVersion = "500"
				// - this is to trigger an actual update technically speaking the pod already matches the namespace of the subject
				anpPeerPod.Labels["rv"] = "resourceVersionUTHack"
				_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Pods(anpPeerPod.Namespace).Update(context.TODO(), &anpPeerPod, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				t2.podIP = "10.128.1.6"
				t2.podMAC = "0a:58:0a:80:01:06"
				peerNSASIPv4, peerNSASIPv6 = buildNamespaceAddressSets(anpPeerNamespaceName, []string{t2.podIP})
				expectedDatabaseState = []libovsdbtest.TestData{pg, subjectNSASIPv4, subjectNSASIPv6, peerNSASIPv4, peerNSASIPv6}
				for _, acl := range newACLs {
					acl := acl
					expectedDatabaseState = append(expectedDatabaseState, acl)
				}
				expectedDatabaseState = append(expectedDatabaseState, getDefaultNetExpectedPodsAndSwitches([]testPod{t, t2}, []string{node1Name})...)
				peerASIngressRule1v4, peerASIngressRule1v6 = buildANPAddressSets(anp,
					1, []string{t2.podIP}, libovsdbutil.ACLIngress)
				peerASEgressRule1v4, peerASEgressRule1v6 = buildANPAddressSets(anp,
					1, []string{t2.podIP}, libovsdbutil.ACLEgress)
				expectedDatabaseState = append(expectedDatabaseState, []libovsdbtest.TestData{peerASIngressRule0v4, peerASIngressRule0v6, peerASIngressRule1v4,
					peerASIngressRule1v6, peerASEgressRule0v4, peerASEgressRule0v6, peerASEgressRule1v4, peerASEgressRule1v6}...)
				gomega.Eventually(fakeOVN.nbClient, "3s").Should(libovsdbtest.HaveData(expectedDatabaseState))

				// now delete the namespaces
				anpNamespaceSubject.ResourceVersion = "3"
				err = fakeOVN.fakeClient.KubeClient.CoreV1().Namespaces().Delete(context.TODO(), anpNamespaceSubject.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				anpNamespacePeer.ResourceVersion = "3"
				err = fakeOVN.fakeClient.KubeClient.CoreV1().Namespaces().Delete(context.TODO(), anpNamespacePeer.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				pg = getDefaultPGForANPSubject(anp.Name, nil, newACLs, false) // no ports in PG
				expectedDatabaseState = []libovsdbtest.TestData{pg}           // namespace address-sets are gone
				for _, acl := range newACLs {
					acl := acl
					expectedDatabaseState = append(expectedDatabaseState, acl)
				}
				// delete namespace in unit testing doesn't delete pods; we just need to check port groups and address-sets are updated
				expectedDatabaseState = append(expectedDatabaseState, getDefaultNetExpectedPodsAndSwitches([]testPod{t, t2}, []string{node1Name})...)
				peerASIngressRule1v4, peerASIngressRule1v6 = buildANPAddressSets(anp,
					1, []string{}, libovsdbutil.ACLIngress)
				peerASEgressRule1v4, peerASEgressRule1v6 = buildANPAddressSets(anp,
					1, []string{}, libovsdbutil.ACLEgress)
				expectedDatabaseState = append(expectedDatabaseState, []libovsdbtest.TestData{peerASIngressRule0v4, peerASIngressRule0v6, peerASIngressRule1v4,
					peerASIngressRule1v6, peerASEgressRule0v4, peerASEgressRule0v6, peerASEgressRule1v4, peerASEgressRule1v6}...)
				// NOTE: Address set deletion is deferred for 20 seconds...
				gomega.Eventually(fakeOVN.nbClient, "25s").Should(libovsdbtest.HaveData(expectedDatabaseState))

				ginkgo.By("17. update the ANP by deleting all rules; check if all objects are deleted correctly")
				anp.Spec.Ingress = []anpapi.AdminNetworkPolicyIngressRule{}
				anp.Spec.Egress = []anpapi.AdminNetworkPolicyEgressRule{}
				anp.ResourceVersion = "7"
				anp, err = fakeOVN.fakeClient.ANPClient.PolicyV1alpha1().AdminNetworkPolicies().Update(context.TODO(), anp, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				pg = getDefaultPGForANPSubject(anp.Name, nil, nil, false) // no ports and acls in PG
				expectedDatabaseState = []libovsdbtest.TestData{pg}
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				expectedDatabaseState = append(expectedDatabaseState, getDefaultNetExpectedPodsAndSwitches([]testPod{t, t2}, []string{node1Name})...)
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				ginkgo.By("18. delete the ANP; check if all objects are deleted correctly")
				anp.ResourceVersion = "8"
				err = fakeOVN.fakeClient.ANPClient.PolicyV1alpha1().AdminNetworkPolicies().Delete(context.TODO(), anp.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				expectedDatabaseState = []libovsdbtest.TestData{} // port group should be deleted
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				expectedDatabaseState = append(expectedDatabaseState, getDefaultNetExpectedPodsAndSwitches([]testPod{t, t2}, []string{node1Name})...)
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
		})
		ginkgo.It("PortNumber, PortRange and NamedPort (no matching pods) Rules for ANP", func() {
			app.Action = func(ctx *cli.Context) error {
				config.IPv4Mode = true
				config.IPv6Mode = true
				fakeOVN.start()
				fakeOVN.InitAndRunANPController()
				fakeOVN.fakeClient.ANPClient.(*anpfake.Clientset).PrependReactor("update", "adminnetworkpolicies", func(action clienttesting.Action) (handled bool, ret runtime.Object, err error) {
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
				ginkgo.By("1. Create ANP with 1 ingress rule and 2 egress rule with multiple Ports combinations")
				anpSubject := newANPSubjectObject(
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
						PortNumber: &anpapi.Port{
							Port:     36364,
							Protocol: v1.ProtocolTCP,
						},
					},
					{
						PortRange: &anpapi.PortRange{
							Start:    36330,
							End:      36550,
							Protocol: v1.ProtocolTCP,
						},
					},
					{
						NamedPort: ptr.To("http"), // unmatched port; no pod has it; ensure its ignored
					},
					{
						PortRange: &anpapi.PortRange{
							Start:    36330,
							End:      36550,
							Protocol: v1.ProtocolSCTP,
						},
					},
					{
						PortRange: &anpapi.PortRange{
							Start:    36330,
							End:      36550,
							Protocol: v1.ProtocolUDP,
						},
					},
				}
				anp := newANPObject("harry-potter", 75, anpSubject,
					[]anpapi.AdminNetworkPolicyIngressRule{
						{
							Name:   "deny-traffic-from-slytherin-to-gryffindor",
							Action: anpapi.AdminNetworkPolicyRuleActionDeny,
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
					[]anpapi.AdminNetworkPolicyEgressRule{
						{
							Name:   "pass-traffic-to-slytherin-from-gryffindor",
							Action: anpapi.AdminNetworkPolicyRuleActionPass,
							To: []anpapi.AdminNetworkPolicyEgressPeer{
								{
									Namespaces: &metav1.LabelSelector{
										MatchLabels: peerPassLabel,
									},
								},
							},
							Ports: &anpPortMangle,
						},
						{
							Name:   "allow-traffic-to-hufflepuff-from-gryffindor",
							Action: anpapi.AdminNetworkPolicyRuleActionAllow,
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
				anp.ResourceVersion = "1"
				anp, err := fakeOVN.fakeClient.ANPClient.PolicyV1alpha1().AdminNetworkPolicies().Create(context.TODO(), anp, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				acls := getACLsForANPRules(anp)
				pg := getDefaultPGForANPSubject(anp.Name, []string{}, acls, false)
				expectedDatabaseState := []libovsdbtest.TestData{pg}
				for _, acl := range acls {
					acl := acl
					expectedDatabaseState = append(expectedDatabaseState, acl)
				}
				peerASIngressRule0v4, peerASIngressRule0v6 := buildANPAddressSets(anp, 0, []string{}, libovsdbutil.ACLIngress) // address-set will be empty since no pods match it yet
				expectedDatabaseState = append(expectedDatabaseState, peerASIngressRule0v4)
				expectedDatabaseState = append(expectedDatabaseState, peerASIngressRule0v6)
				peerASEgressRule0v4, peerASEgressRule0v6 := buildANPAddressSets(anp, 0, []string{}, libovsdbutil.ACLEgress)
				expectedDatabaseState = append(expectedDatabaseState, peerASEgressRule0v4)
				expectedDatabaseState = append(expectedDatabaseState, peerASEgressRule0v6)
				peerASEgressRule1v4, peerASEgressRule1v6 := buildANPAddressSets(anp, 1, []string{}, libovsdbutil.ACLEgress)
				expectedDatabaseState = append(expectedDatabaseState, peerASEgressRule1v4)
				expectedDatabaseState = append(expectedDatabaseState, peerASEgressRule1v6)
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))

				ginkgo.By("2. Update ANP by changing port range in ingress rule and ensure its honoured")
				anp.ResourceVersion = "2"
				anpPortMangle[4].PortRange.End = 60000
				anp.Spec.Ingress[0].Ports = &anpPortMangle
				_, err = fakeOVN.fakeClient.ANPClient.PolicyV1alpha1().AdminNetworkPolicies().Update(context.TODO(), anp, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				ingressACL := expectedDatabaseState[2] // ingress SCTP ACL
				acl := ingressACL.(*nbdb.ACL)
				_ = strings.Replace(acl.Match, "36550", "60000", 1)
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))
				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
		})
		ginkgo.It("NamedPort matching pods Rules for ANP", func() {
			app.Action = func(ctx *cli.Context) error {
				config.IPv4Mode = true
				config.IPv6Mode = true
				anpSubjectNamespace := *newNamespaceWithLabels(anpSubjectNamespaceName, anpLabel)
				anpNamespacePeer := *newNamespaceWithLabels(anpPeerNamespaceName, peerPassLabel)
				anpNamespacePeer2 := *newNamespaceWithLabels(anpPeerNamespaceName+"2", peerDenyLabel)
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
					anpPeerPodName,
					anpPodV4IP2+" "+anpPodV6IP2,
					anpPodMAC2,
					anpPeerNamespaceName,
				)
				anpPeerPod := *newPod(anpPeerNamespaceName, anpPeerPodName, node1Name, t1.podIP)
				// pinning annotations because between subject and peer pods IPAM isunpredictable
				anpPeerPod.Annotations = map[string]string{}
				anpPeerPod.Annotations["k8s.ovn.org/pod-networks"] = `{"default":{"ip_addresses":["10.128.1.4/24","fe00:10:128:1::4/64"],` +
					`"mac_address":"0a:58:0a:80:01:04","gateway_ips":["10.128.1.1","fe00:10:128:1::1"],"routes":[{"dest":"10.128.1.0/24","nextHop":"10.128.1.1"}],` +
					`"ip_address":"10.128.1.4/24","gateway_ip":"10.128.1.1"}}`
				anpPeerPod.Spec.Containers = append(anpPeerPod.Spec.Containers,
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
					anpSubjectPodName,
					anpPodV4IP+" "+anpPodV6IP,
					anpPodMAC,
					anpSubjectNamespaceName,
				)
				anpSubjectPod := *newPod(anpSubjectNamespaceName, anpSubjectPodName, node1Name, t2.podIP)
				// pinning annotations because between subject and peer pods IPAM isunpredictable
				anpSubjectPod.Annotations = map[string]string{}
				anpSubjectPod.Annotations["k8s.ovn.org/pod-networks"] = `{"default":{"ip_addresses":["10.128.1.3/24","fe00:10:128:1::3/64"],` +
					`"mac_address":"0a:58:0a:80:01:03","gateway_ips":["10.128.1.1","fe00:10:128:1::1"],"routes":[{"dest":"10.128.1.0/24","nextHop":"10.128.1.1"}],` +
					`"ip_address":"10.128.1.3/24","gateway_ip":"10.128.1.1"}}`
				anpSubjectPod.Spec.Containers[0].Ports = []v1.ContainerPort{
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
							anpSubjectNamespace,
							anpNamespacePeer,
							anpNamespacePeer2,
						},
					},
					&v1.NodeList{
						Items: []v1.Node{
							*node1,
						},
					},
					&v1.PodList{
						Items: []v1.Pod{
							anpPeerPod, // peer pod ("house": "slytherin")
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
				namedEPorts["web"] = []libovsdbutil.NamedNetworkPolicyPort{
					{L4Protocol: "tcp", L3PodIP: anpPodV4IP2, L3PodIPFamily: "ip4", L4PodPort: "3535"},
					{L4Protocol: "tcp", L3PodIP: anpPodV6IP2, L3PodIPFamily: "ip6", L4PodPort: "3535"},
				}
				namedEPorts["web123"] = []libovsdbutil.NamedNetworkPolicyPort{
					{L4Protocol: "sctp", L3PodIP: anpPodV4IP2, L3PodIPFamily: "ip4", L4PodPort: "35356"},
					{L4Protocol: "sctp", L3PodIP: anpPodV6IP2, L3PodIPFamily: "ip6", L4PodPort: "35356"},
				}
				namedIPorts["dns"] = []libovsdbutil.NamedNetworkPolicyPort{}
				fakeOVN.InitAndRunANPController()
				fakeOVN.fakeClient.ANPClient.(*anpfake.Clientset).PrependReactor("update", "adminnetworkpolicies", func(action clienttesting.Action) (handled bool, ret runtime.Object, err error) {
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
				ginkgo.By("1. Create ANP with 1 ingress rule and 2 egress rule with multiple NamedPorts combinations")
				anpSubject := newANPSubjectObject(
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
				anp := newANPObject("harry-potter", 65, anpSubject,
					[]anpapi.AdminNetworkPolicyIngressRule{
						{ // 3 ACLs + 1ACL for NamedPort
							Name:   "deny-traffic-from-slytherin-to-gryffindor",
							Action: anpapi.AdminNetworkPolicyRuleActionDeny,
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
					[]anpapi.AdminNetworkPolicyEgressRule{
						{ // 3 ACLs + 2ACLs for NamedPort
							Name:   "pass-traffic-to-slytherin-from-gryffindor",
							Action: anpapi.AdminNetworkPolicyRuleActionPass,
							To: []anpapi.AdminNetworkPolicyEgressPeer{
								{
									Namespaces: &metav1.LabelSelector{
										MatchLabels: peerPassLabel,
									},
								},
							},
							Ports: &anpPortMangle,
						},
						{ // 3 ACLs
							Name:   "allow-traffic-to-hufflepuff-from-gryffindor",
							Action: anpapi.AdminNetworkPolicyRuleActionAllow,
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
				anp.ResourceVersion = "1"
				anp, err = fakeOVN.fakeClient.ANPClient.PolicyV1alpha1().AdminNetworkPolicies().Create(context.TODO(), anp, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				acls := getACLsForANPRulesWithNamedPorts(anp, namedIPorts, namedEPorts)
				pg := getDefaultPGForANPSubject(anp.Name, []string{}, acls, false)
				subjectNSASIPv4, subjectNSASIPv6 := buildNamespaceAddressSets(anpSubjectNamespaceName, []string{})
				peerNSASIPv4, peerNSASIPv6 := buildNamespaceAddressSets(anpPeerNamespaceName, []string{anpPodV4IP2, anpPodV6IP2})
				peerNS2ASIPv4, peerNS2ASIPv6 := buildNamespaceAddressSets(anpPeerNamespaceName+"2", []string{})
				baseExpectedDatabaseState := []libovsdbtest.TestData{subjectNSASIPv4, subjectNSASIPv6, peerNSASIPv4, peerNSASIPv6, peerNS2ASIPv4, peerNS2ASIPv6}
				peerASIngressRule0v4, peerASIngressRule0v6 := buildANPAddressSets(anp, 0, []string{}, libovsdbutil.ACLIngress)
				baseExpectedDatabaseState = append(baseExpectedDatabaseState, peerASIngressRule0v4)
				baseExpectedDatabaseState = append(baseExpectedDatabaseState, peerASIngressRule0v6)
				peerASEgressRule0v4, peerASEgressRule0v6 := buildANPAddressSets(anp, 0, []string{"fe00:10:128:1::4", "10.128.1.4"}, libovsdbutil.ACLEgress)
				baseExpectedDatabaseState = append(baseExpectedDatabaseState, peerASEgressRule0v4)
				baseExpectedDatabaseState = append(baseExpectedDatabaseState, peerASEgressRule0v6)
				peerASEgressRule1v4, peerASEgressRule1v6 := buildANPAddressSets(anp, 1, []string{}, libovsdbutil.ACLEgress)
				baseExpectedDatabaseState = append(baseExpectedDatabaseState, peerASEgressRule1v4)
				baseExpectedDatabaseState = append(baseExpectedDatabaseState, peerASEgressRule1v6)
				expectedDatabaseState := append(baseExpectedDatabaseState, pg)
				expectedDatabaseState = append(expectedDatabaseState, getDefaultNetExpectedPodsAndSwitches([]testPod{t1}, []string{node1Name})...)
				for _, acl := range acls {
					acl := acl
					expectedDatabaseState = append(expectedDatabaseState, acl)
				}
				// 3 ingress protocol ACLs,
				// 3 egress[0] protocol ACLs and 2 egress[0] namedPort ACLs,
				// 3 egress[1] protocol ACLs
				// total 11ACLs
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))

				ginkgo.By("2. Create subject pod and ensure LSP and namedPorts ACLs are updated for the matching ingress rule")
				_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Pods(anpSubjectNamespaceName).Create(context.TODO(), &anpSubjectPod, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// The pod takes some time to get created so the first add pod event for ANP does nothing as it waits for LSP to be created - this triggers a retry.
				// Next when the pod is scheduled and IP is allocated and LSP is created we get an update event.
				// When ANP controller receives the pod update event, it will not trigger an update in unit testing since ResourceVersion and pod.status.Running
				// is not updated by the test, hence we need to trigger a manual update in the unit test framework that is accompanied by label update.
				// NOTE: This is not an issue in real deployments with actual kapi server; its just a potential flake in the UT framework that we need to protect against.
				time.Sleep(time.Second)
				anpSubjectPod.ResourceVersion = "5"
				// - this is to trigger an actual update technically speaking the pod already matches the namespace of the subject
				anpSubjectPod.Labels["rv"] = "resourceVersionUTHack"
				_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Pods(anpSubjectPod.Namespace).Update(context.TODO(), &anpSubjectPod, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				namedIPorts["dns"] = []libovsdbutil.NamedNetworkPolicyPort{
					{L4Protocol: "udp", L3PodIP: anpPodV4IP, L3PodIPFamily: "ip4", L4PodPort: "5353"},
					{L4Protocol: "udp", L3PodIP: anpPodV6IP, L3PodIPFamily: "ip6", L4PodPort: "5353"},
				}

				subjectNSASIPv4, subjectNSASIPv6 = buildNamespaceAddressSets(anpSubjectNamespaceName, []string{anpPodV4IP, anpPodV6IP})
				baseExpectedDatabaseState[0] = subjectNSASIPv4
				baseExpectedDatabaseState[1] = subjectNSASIPv6
				acls = getACLsForANPRulesWithNamedPorts(anp, namedIPorts, namedEPorts)
				pg = getDefaultPGForANPSubject(anp.Name, []string{t2.portUUID}, acls, false)
				expectedDatabaseState = append(baseExpectedDatabaseState, pg)
				expectedDatabaseState = append(expectedDatabaseState, getDefaultNetExpectedPodsAndSwitches([]testPod{t1, t2}, []string{node1Name})...)
				for _, acl := range acls {
					acl := acl
					expectedDatabaseState = append(expectedDatabaseState, acl)
				}
				// 3 ingress protocol ACLs and 1 ingress namedPort ACL,
				// 3 egress[0] protocol ACLs and 2 egress[0] namedPort ACLs,
				// 3 egress[1] protocol ACLs
				// total 12ACLs
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))

				ginkgo.By("3. Update ANP ingress rule ports to remove DNS namedPort and ensure ACLs are updated correctly")
				anp.ResourceVersion = "4"
				newPort := anpPortMangle[0:5]
				anp.Spec.Ingress[0].Ports = &newPort
				anp, err = fakeOVN.fakeClient.ANPClient.PolicyV1alpha1().AdminNetworkPolicies().Update(context.TODO(), anp, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				delete(namedIPorts, "dns")
				acls = getACLsForANPRulesWithNamedPorts(anp, namedIPorts, namedEPorts)
				pg = getDefaultPGForANPSubject(anp.Name, []string{t2.portUUID}, acls, false)
				expectedDatabaseState = append(baseExpectedDatabaseState, pg)
				expectedDatabaseState = append(expectedDatabaseState, getDefaultNetExpectedPodsAndSwitches([]testPod{t1, t2}, []string{node1Name})...)
				for _, acl := range acls {
					acl := acl
					expectedDatabaseState = append(expectedDatabaseState, acl)
				}
				// 3 ingress protocol ACLs
				// 3 egress[0] protocol ACLs and 2 egress[0] namedPort ACLs,
				// 3 egress[1] protocol ACLs
				// total 11ACLs
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))

				ginkgo.By("4. Delete matching peer pod and ensure ACLs are updated correctly")
				anpPeerPod.ResourceVersion = "3"
				err = fakeOVN.fakeClient.KubeClient.CoreV1().Pods(anpPeerPod.Namespace).Delete(context.TODO(), anpPeerPod.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				delete(namedEPorts, "web")
				delete(namedEPorts, "web123")
				acls = getACLsForANPRulesWithNamedPorts(anp, namedIPorts, namedEPorts)
				pg = getDefaultPGForANPSubject(anp.Name, []string{t2.portUUID}, acls, false)
				peerNSASIPv4, peerNSASIPv6 = buildNamespaceAddressSets(anpPeerNamespaceName, []string{}) // pod is gone from peer namespace address-set
				baseExpectedDatabaseState[2] = peerNSASIPv4
				baseExpectedDatabaseState[3] = peerNSASIPv6
				peerASEgressRule0v4, peerASEgressRule0v6 = buildANPAddressSets(anp, 0, []string{}, libovsdbutil.ACLEgress)
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
				// 3 egress[1] protocol ACLs
				// totalACLs: 9
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))

				ginkgo.By("5. Re-Add matching peer pod and ensure ACLs are updated correctly")
				anpPeerPod.ResourceVersion = "5"
				_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Pods(anpPeerPod.Namespace).Create(context.TODO(), &anpPeerPod, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// update namedPorts for constructing right test ACLs
				namedEPorts["web"] = []libovsdbutil.NamedNetworkPolicyPort{
					{L4Protocol: "tcp", L3PodIP: anpPodV4IP2, L3PodIPFamily: "ip4", L4PodPort: "3535"},
					{L4Protocol: "tcp", L3PodIP: anpPodV6IP2, L3PodIPFamily: "ip6", L4PodPort: "3535"},
				}
				namedEPorts["web123"] = []libovsdbutil.NamedNetworkPolicyPort{
					{L4Protocol: "sctp", L3PodIP: anpPodV4IP2, L3PodIPFamily: "ip4", L4PodPort: "35356"},
					{L4Protocol: "sctp", L3PodIP: anpPodV6IP2, L3PodIPFamily: "ip6", L4PodPort: "35356"},
				}
				acls = getACLsForANPRulesWithNamedPorts(anp, namedIPorts, namedEPorts)
				pg = getDefaultPGForANPSubject(anp.Name, []string{t2.portUUID}, acls, false)
				peerNSASIPv4, peerNSASIPv6 = buildNamespaceAddressSets(anpPeerNamespaceName, []string{"fe00:10:128:1::4", "10.128.1.4"}) // pod is re-added to peer namespace address-set
				baseExpectedDatabaseState[2] = peerNSASIPv4
				baseExpectedDatabaseState[3] = peerNSASIPv6
				peerASEgressRule0v4, peerASEgressRule0v6 = buildANPAddressSets(anp, 0, []string{"fe00:10:128:1::4", "10.128.1.4"}, libovsdbutil.ACLEgress)
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
				// 3 egress[1] protocol ACLs
				// totalACLs: 11
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))

				ginkgo.By("6. Update ANP egress rule port to rename web namedPort and ensure ACLs are updated correctly")
				anp.ResourceVersion = "7"
				newPort = anpPortMangle
				newPort[3].NamedPort = ptr.To("http")
				anp.Spec.Egress[0].Ports = &newPort
				anp, err = fakeOVN.fakeClient.ANPClient.PolicyV1alpha1().AdminNetworkPolicies().Update(context.TODO(), anp, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				delete(namedEPorts, "web")
				acls = getACLsForANPRulesWithNamedPorts(anp, namedIPorts, namedEPorts)
				pg = getDefaultPGForANPSubject(anp.Name, []string{t2.portUUID}, acls, false)
				expectedDatabaseState = []libovsdbtest.TestData{pg, subjectNSASIPv4, subjectNSASIPv6, peerNSASIPv4, peerNSASIPv6, peerNS2ASIPv4, peerNS2ASIPv6}
				expectedDatabaseState = append(expectedDatabaseState, getDefaultNetExpectedPodsAndSwitches([]testPod{t1, t2}, []string{node1Name})...)
				expectedDatabaseState = append(expectedDatabaseState, peerASIngressRule0v4)
				expectedDatabaseState = append(expectedDatabaseState, peerASIngressRule0v6)
				expectedDatabaseState = append(expectedDatabaseState, peerASEgressRule0v4)
				expectedDatabaseState = append(expectedDatabaseState, peerASEgressRule0v6)
				expectedDatabaseState = append(expectedDatabaseState, peerASEgressRule1v4)
				expectedDatabaseState = append(expectedDatabaseState, peerASEgressRule1v6)
				for _, acl := range acls {
					acl := acl
					expectedDatabaseState = append(expectedDatabaseState, acl)
				}
				// 3 ingress protocol ACLs
				// 3 egress[0] protocol ACLs and 1 egress[0] namedPort ACLs,
				// 3 egress[1] protocol ACLs
				// totalACLs: 10
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))

				ginkgo.By("7. Delete 1st ANP egress rule and ensure ACLs are updated correctly")
				anp.ResourceVersion = "9"
				anp.Spec.Egress = anp.Spec.Egress[1:]
				_, err = fakeOVN.fakeClient.ANPClient.PolicyV1alpha1().AdminNetworkPolicies().Update(context.TODO(), anp, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				delete(namedEPorts, "web123")
				acls = getACLsForANPRulesWithNamedPorts(anp, namedIPorts, namedEPorts)
				pg = getDefaultPGForANPSubject(anp.Name, []string{t2.portUUID}, acls, false)
				expectedDatabaseState = []libovsdbtest.TestData{pg, subjectNSASIPv4, subjectNSASIPv6, peerNSASIPv4, peerNSASIPv6, peerNS2ASIPv4, peerNS2ASIPv6}
				expectedDatabaseState = append(expectedDatabaseState, getDefaultNetExpectedPodsAndSwitches([]testPod{t1, t2}, []string{node1Name})...)
				expectedDatabaseState = append(expectedDatabaseState, peerASIngressRule0v4)
				expectedDatabaseState = append(expectedDatabaseState, peerASIngressRule0v6)
				peerASEgressRule0v4, peerASEgressRule0v6 = buildANPAddressSets(anp, 0, []string{}, libovsdbutil.ACLEgress)
				expectedDatabaseState = append(expectedDatabaseState, peerASEgressRule0v4)
				expectedDatabaseState = append(expectedDatabaseState, peerASEgressRule0v6)
				for _, acl := range acls {
					acl := acl
					expectedDatabaseState = append(expectedDatabaseState, acl)
				}
				// 3 ingress protocol ACLs
				// 3 egress[0] protocol ACLs
				// totalACLs: 6
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))
				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
		})
		ginkgo.It("ACL Logging for ANP", func() {
			app.Action = func(ctx *cli.Context) error {
				config.IPv4Mode = true
				config.IPv6Mode = true
				fakeOVN.start()
				fakeOVN.InitAndRunANPController()
				fakeOVN.fakeClient.ANPClient.(*anpfake.Clientset).PrependReactor("update", "adminnetworkpolicies", func(action clienttesting.Action) (handled bool, ret runtime.Object, err error) {
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
				ginkgo.By("1. Create ANP with 1 ingress rule and 2 egress rules with the ACL logging annotation and ensure its honoured")
				anpSubject := newANPSubjectObject(
					&metav1.LabelSelector{
						MatchLabels: anpLabel,
					},
					nil,
				)
				anp := newANPObject("harry-potter", 75, anpSubject,
					[]anpapi.AdminNetworkPolicyIngressRule{
						{
							Name:   "deny-traffic-from-slytherin-to-gryffindor",
							Action: anpapi.AdminNetworkPolicyRuleActionDeny,
							From: []anpapi.AdminNetworkPolicyIngressPeer{
								{
									Namespaces: &metav1.LabelSelector{
										MatchLabels: peerDenyLabel,
									},
								},
							},
						},
					},
					[]anpapi.AdminNetworkPolicyEgressRule{
						{
							Name:   "pass-traffic-to-slytherin-from-gryffindor",
							Action: anpapi.AdminNetworkPolicyRuleActionPass,
							To: []anpapi.AdminNetworkPolicyEgressPeer{
								{
									Namespaces: &metav1.LabelSelector{
										MatchLabels: peerPassLabel,
									},
								},
							},
						},
						{
							Name:   "allow-traffic-to-hufflepuff-from-gryffindor",
							Action: anpapi.AdminNetworkPolicyRuleActionAllow,
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
				anp.ResourceVersion = "1"
				anp.Annotations = map[string]string{
					util.AclLoggingAnnotation: fmt.Sprintf(`{ "deny": "%s", "allow": "%s", "pass": "%s" }`, nbdb.ACLSeverityAlert, nbdb.ACLSeverityInfo, nbdb.ACLSeverityNotice),
				}
				anp, err := fakeOVN.fakeClient.ANPClient.PolicyV1alpha1().AdminNetworkPolicies().Create(context.TODO(), anp, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				acls := getACLsForANPRules(anp)
				pg := getDefaultPGForANPSubject(anp.Name, []string{}, acls, false)
				expectedDatabaseState := []libovsdbtest.TestData{pg}
				for _, acl := range acls {
					acl := acl
					// update ACL logging information
					acl.Log = true
					if acl.Action == nbdb.ACLActionPass {
						acl.Severity = ptr.To(nbdb.ACLSeverityNotice)
					} else if acl.Action == nbdb.ACLActionDrop {
						acl.Severity = ptr.To(nbdb.ACLSeverityAlert)
					} else if acl.Action == nbdb.ACLActionAllowRelated {
						acl.Severity = ptr.To(nbdb.ACLSeverityInfo)
					}
					expectedDatabaseState = append(expectedDatabaseState, acl)
				}
				peerASIngressRule0v4, peerASIngressRule0v6 := buildANPAddressSets(anp, 0, []string{}, libovsdbutil.ACLIngress) // address-set will be empty since no pods match it yet
				expectedDatabaseState = append(expectedDatabaseState, peerASIngressRule0v4)
				expectedDatabaseState = append(expectedDatabaseState, peerASIngressRule0v6)
				peerASEgressRule0v4, peerASEgressRule0v6 := buildANPAddressSets(anp, 0, []string{}, libovsdbutil.ACLEgress)
				expectedDatabaseState = append(expectedDatabaseState, peerASEgressRule0v4)
				expectedDatabaseState = append(expectedDatabaseState, peerASEgressRule0v6)
				peerASEgressRule1v4, peerASEgressRule1v6 := buildANPAddressSets(anp, 1, []string{}, libovsdbutil.ACLEgress)
				expectedDatabaseState = append(expectedDatabaseState, peerASEgressRule1v4)
				expectedDatabaseState = append(expectedDatabaseState, peerASEgressRule1v6)
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))

				ginkgo.By("2. Update ANP by changing severity on the ACL logging annotation and ensure its honoured")
				anp.ResourceVersion = "2"
				anp.Annotations = map[string]string{
					util.AclLoggingAnnotation: fmt.Sprintf(`{ "deny": "%s", "allow": "%s", "pass": "%s" }`, nbdb.ACLSeverityWarning, nbdb.ACLSeverityDebug, nbdb.ACLSeverityInfo),
				}
				anp, err = fakeOVN.fakeClient.ANPClient.PolicyV1alpha1().AdminNetworkPolicies().Update(context.TODO(), anp, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				for _, acl := range expectedDatabaseState[1:4] {
					acl := acl.(*nbdb.ACL)
					// update ACL logging information
					acl.Log = true
					if acl.Action == nbdb.ACLActionPass {
						acl.Severity = ptr.To(nbdb.ACLSeverityInfo)
					} else if acl.Action == nbdb.ACLActionDrop {
						acl.Severity = ptr.To(nbdb.ACLSeverityWarning)
					} else if acl.Action == nbdb.ACLActionAllowRelated {
						acl.Severity = ptr.To(nbdb.ACLSeverityDebug)
					}
				}
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))

				ginkgo.By("3. Update ANP by deleting the ACL logging annotation and ensure its honoured")
				anp.ResourceVersion = "3"
				anp.Annotations = map[string]string{}
				_, err = fakeOVN.fakeClient.ANPClient.PolicyV1alpha1().AdminNetworkPolicies().Update(context.TODO(), anp, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				for _, acl := range expectedDatabaseState[1:4] {
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
				anpNamespaceSubject := *newNamespaceWithLabels(anpSubjectNamespaceName, anpLabel)
				anpNamespacePeer := *newNamespaceWithLabels(anpPeerNamespaceName, peerDenyLabel)
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
					anpSubjectPodName,
					anpPodV4IP+" "+anpPodV6IP,
					anpPodMAC,
					anpSubjectNamespaceName,
				)
				anpSubjectPod := *newPod(anpSubjectNamespaceName, anpSubjectPodName, node1Name, t.podIP)
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
					anpPeerPodName,
					anpPodV4IP2+" "+anpPodV6IP2,
					anpPodMAC2,
					anpPeerNamespaceName,
				)
				anpPeerPod := *newPod(anpPeerNamespaceName, anpPeerPodName, node1Name, t2.podIP)
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
							anpNamespaceSubject,
							anpNamespacePeer,
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

				fakeOVN.fakeClient.ANPClient.(*anpfake.Clientset).PrependReactor("update", "adminnetworkpolicies", func(action clienttesting.Action) (handled bool, ret runtime.Object, err error) {
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

				ginkgo.By("1. creating an admin network policy with 3 egress rules that have node+network peers")
				anpSubject := newANPSubjectObject(
					&metav1.LabelSelector{
						MatchLabels: anpLabel,
					},
					nil,
				)
				// NOTE: Logically speaking the rules don't make sense because we match on the same node, but goal here is to verify the match
				egressRules := []anpapi.AdminNetworkPolicyEgressRule{
					{
						Name:   "deny-traffic-to-slytherin-and-linux-nodes-from-gryffindor",
						Action: anpapi.AdminNetworkPolicyRuleActionDeny,
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
						Action: anpapi.AdminNetworkPolicyRuleActionAllow,
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
					},
					{
						Name:   "pass-traffic-to-ravenclaw-and-node1-from-gryffindor",
						Action: anpapi.AdminNetworkPolicyRuleActionPass,
						To: []anpapi.AdminNetworkPolicyEgressPeer{
							{
								Namespaces: &metav1.LabelSelector{
									MatchLabels: peerPassLabel,
								},
							},
							{
								Nodes: &metav1.LabelSelector{
									MatchLabels: map[string]string{"kubernetes.io/hostname": "node1"},
								},
							},
							{
								Networks: []anpapi.CIDR{"135.10.0.0/24", "10.244.40.0/23", "2001:0db8::/32"},
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
				anp := newANPObject("harry-potter", 5, anpSubject, []anpapi.AdminNetworkPolicyIngressRule{}, egressRules)
				anp.ResourceVersion = "1"
				anp, err = fakeOVN.fakeClient.ANPClient.PolicyV1alpha1().AdminNetworkPolicies().Create(context.TODO(), anp, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				acls := getACLsForANPRules(anp)
				pg := getDefaultPGForANPSubject(anp.Name, []string{t.portUUID}, acls, false)
				subjectNSASIPv4, subjectNSASIPv6 := buildNamespaceAddressSets(anpSubjectNamespaceName, []string{anpPodV4IP, anpPodV6IP})
				peerNSASIPv4, peerNSASIPv6 := buildNamespaceAddressSets(anpPeerNamespaceName, []string{anpPodV4IP2, anpPodV6IP2})
				expectedDatabaseState := []libovsdbtest.TestData{pg, subjectNSASIPv4, subjectNSASIPv6, peerNSASIPv4, peerNSASIPv6}
				expectedDatabaseState = append(expectedDatabaseState, getDefaultNetExpectedPodsAndSwitches([]testPod{t, t2}, []string{node1Name})...)
				for _, acl := range acls {
					acl := acl
					expectedDatabaseState = append(expectedDatabaseState, acl)
				}
				// egressRule AddressSets
				peerASEgressRule0v4, peerASEgressRule0v6 := buildANPAddressSets(anp,
					0, []string{anpPodV4IP2, anpPodV6IP2, "0.0.0.0/0", "::/0"}, libovsdbutil.ACLEgress) // address-set will contain matching peer nodes, kubernetes.io/hostname doesn't match node1 so no node peers here + networks
				expectedDatabaseState = append(expectedDatabaseState, peerASEgressRule0v4)
				expectedDatabaseState = append(expectedDatabaseState, peerASEgressRule0v6)
				peerASEgressRule1v4, peerASEgressRule1v6 := buildANPAddressSets(anp,
					1, []string{node1IPv4, node1IPv6, "135.10.0.5/32", "188.198.40.0/28", "2001:db8:abcd:1234:c000::/66"}, libovsdbutil.ACLEgress) // address-set will contain all nodeIPs (empty selector match) + networks
				expectedDatabaseState = append(expectedDatabaseState, peerASEgressRule1v4)
				expectedDatabaseState = append(expectedDatabaseState, peerASEgressRule1v6)
				peerASEgressRule2v4, peerASEgressRule2v6 := buildANPAddressSets(anp,
					2, []string{node1IPv4, node1IPv6, "135.10.0.0/24", "10.244.40.0/23", "2001:db8::/32"}, libovsdbutil.ACLEgress) // address-set will contain the matching node peer + networks
				expectedDatabaseState = append(expectedDatabaseState, peerASEgressRule2v4)
				expectedDatabaseState = append(expectedDatabaseState, peerASEgressRule2v6)
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))

				ginkgo.By("2. creating another node2 that will act as peer of admin network policy; check if IP is added to address-set")
				node2 := nodeFor(node2Name, node2IPv4, node2IPv6, node2IPv4Subnet, node2IPv6Subnet, node2transitIPv4, node2transitIPv6)
				_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Nodes().Create(context.TODO(), node2, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				expectedDatabaseState[len(expectedDatabaseState)-3].(*nbdb.AddressSet).Addresses = []string{node2IPv6, node1IPv6, "2001:db8:abcd:1234:c000::/66"}     // nodeIP should be added to v6 allow address-set
				expectedDatabaseState[len(expectedDatabaseState)-4].(*nbdb.AddressSet).Addresses = []string{node2IPv4, node1IPv4, "135.10.0.5/32", "188.198.40.0/28"} // nodeIP should be added to v4 allow address-set
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				ginkgo.By("3. update the labels of node2 such that it starts to match the DENY RULE peer selector; check if IPs are updated in address-sets")
				node2.ResourceVersion = "2"
				node2.Labels["kubernetes.io/os"] = "linux" // node should not match the 1st anp deny rule peer
				_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), node2, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				expectedDatabaseState[len(expectedDatabaseState)-5].(*nbdb.AddressSet).Addresses = []string{
					anpPodV6IP2, node2IPv6, "::/0"} // nodeIP should be added to v6 deny address-set
				expectedDatabaseState[len(expectedDatabaseState)-6].(*nbdb.AddressSet).Addresses = []string{
					anpPodV4IP2, node2IPv4, "0.0.0.0/0"} // nodeIP should be added to v4 deny address-set
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				ginkgo.By("4. update the peer selector of rule2 in admin network policy so that v6 networks-peer is deleted; check if address-set is updated")
				egressRules[1].To[2].Networks = []anpapi.CIDR{"135.10.0.5/32", "188.198.40.0/28"} // delete v6 network CIDR
				anp.ResourceVersion = "3"
				anp.Spec.Egress = egressRules
				_, err = fakeOVN.fakeClient.ANPClient.PolicyV1alpha1().AdminNetworkPolicies().Update(context.TODO(), anp, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				expectedDatabaseState[len(expectedDatabaseState)-3].(*nbdb.AddressSet).Addresses = []string{node2IPv6, node1IPv6} // network should be removed from v6 allow address-set
				expectedDatabaseState[len(expectedDatabaseState)-4].(*nbdb.AddressSet).Addresses = []string{node2IPv4, node1IPv4, "135.10.0.5/32", "188.198.40.0/28"}
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				ginkgo.By("5. update the peer selector of rule2 in admin network policy so that networks-peer is deleted; check if address-set is updated")
				egressRules[1].To = egressRules[1].To[:2]
				anp.ResourceVersion = "5"
				anp.Spec.Egress = egressRules
				_, err = fakeOVN.fakeClient.ANPClient.PolicyV1alpha1().AdminNetworkPolicies().Update(context.TODO(), anp, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				expectedDatabaseState[len(expectedDatabaseState)-3].(*nbdb.AddressSet).Addresses = []string{node2IPv6, node1IPv6} // network should be removed from v6 allow address-set
				expectedDatabaseState[len(expectedDatabaseState)-4].(*nbdb.AddressSet).Addresses = []string{node2IPv4, node1IPv4} // network should be removed from v4 allow address-set
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				ginkgo.By("6. update the hostCIDR annotation of node2; check if IPs are updated in address-sets")
				node2.ResourceVersion = "3"
				node2.Annotations[util.OVNNodeHostCIDRs] = fmt.Sprintf("[\"%s\",\"%s\"]", "200.100.100.0/24", "fc00:f853:ccd:e793::2/64")
				_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), node2, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				expectedDatabaseState[len(expectedDatabaseState)-4].(*nbdb.AddressSet).Addresses = []string{
					"200.100.100.0", node1IPv4} // nodeIP should be updated in v4 allow address-set
				expectedDatabaseState[len(expectedDatabaseState)-6].(*nbdb.AddressSet).Addresses = []string{
					anpPodV4IP2, "200.100.100.0", "0.0.0.0/0"} // nodeIP should be updated in v4 deny address-set
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				ginkgo.By("7. update the peer selector of rule3 in admin network policy so that node1 stops matching; check if address-set is updated")
				egressRules[2].To[1].Nodes = &metav1.LabelSelector{
					MatchLabels: map[string]string{"kubernetes.io/hostname": "node100"},
				}
				anp.ResourceVersion = "10"
				anp.Spec.Egress = egressRules
				_, err = fakeOVN.fakeClient.ANPClient.PolicyV1alpha1().AdminNetworkPolicies().Update(context.TODO(), anp, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				expectedDatabaseState[len(expectedDatabaseState)-1].(*nbdb.AddressSet).Addresses = []string{"2001:db8::/32"}                   // node1IP should be removed from v6 pass address-set
				expectedDatabaseState[len(expectedDatabaseState)-2].(*nbdb.AddressSet).Addresses = []string{"135.10.0.0/24", "10.244.40.0/23"} // node1IP should be removed from v4 pass address-set
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				ginkgo.By("8. delete node matching peer selector; check if IPs are updated in address-sets")
				err = fakeOVN.fakeClient.KubeClient.CoreV1().Nodes().Delete(context.TODO(), node2Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				expectedDatabaseState[len(expectedDatabaseState)-5].(*nbdb.AddressSet).Addresses = []string{anpPodV6IP2, "::/0"}      // node2IP should be removed from v6 deny address-set
				expectedDatabaseState[len(expectedDatabaseState)-6].(*nbdb.AddressSet).Addresses = []string{anpPodV4IP2, "0.0.0.0/0"} // node2IP should be removed to v4 deny address-set
				expectedDatabaseState[len(expectedDatabaseState)-3].(*nbdb.AddressSet).Addresses = []string{node1IPv6}                // node2IP should be removed from v6 allow address-set
				expectedDatabaseState[len(expectedDatabaseState)-4].(*nbdb.AddressSet).Addresses = []string{node1IPv4}                // node2IP should be removed to v4 allow address-set
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				ginkgo.By("9. update the ANP by deleting all rules; check if all objects are deleted correctly")
				anp.Spec.Egress = []anpapi.AdminNetworkPolicyEgressRule{}
				anp.ResourceVersion = "15"
				anp, err = fakeOVN.fakeClient.ANPClient.PolicyV1alpha1().AdminNetworkPolicies().Update(context.TODO(), anp, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				pg = getDefaultPGForANPSubject(anp.Name, []string{t.portUUID}, nil, false)
				expectedDatabaseState = []libovsdbtest.TestData{pg, subjectNSASIPv4, subjectNSASIPv6, peerNSASIPv4, peerNSASIPv6}
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				expectedDatabaseState = append(expectedDatabaseState, getDefaultNetExpectedPodsAndSwitches([]testPod{t, t2}, []string{node1Name})...)
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				ginkgo.By("10. delete the ANP; check if all objects are deleted correctly")
				anp.ResourceVersion = "20"
				err = fakeOVN.fakeClient.ANPClient.PolicyV1alpha1().AdminNetworkPolicies().Delete(context.TODO(), anp.Name, metav1.DeleteOptions{})
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
	})
	ginkgo.Context("Multiple ANPs at the same priority", func() {
		anpSubject := newANPSubjectObject(
			&metav1.LabelSelector{
				MatchLabels: anpLabel,
			},
			nil,
		)
		ginkgo.BeforeEach(func() {
			fakeOVN.start()
			fakeOVN.InitAndRunANPController()
			ginkgo.By("1. creating an admin network policy harry-potter at priority 5")
			anp := newANPObject("harry-potter", 5, anpSubject, []anpapi.AdminNetworkPolicyIngressRule{}, []anpapi.AdminNetworkPolicyEgressRule{})
			anp.ResourceVersion = "1"
			anp, err := fakeOVN.fakeClient.ANPClient.PolicyV1alpha1().AdminNetworkPolicies().Create(context.TODO(), anp, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Eventually(func() int {
				anp, err = fakeOVN.fakeClient.ANPClient.PolicyV1alpha1().AdminNetworkPolicies().Get(context.TODO(), "harry-potter", metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				return len(anp.Status.Conditions)
			}).Should(gomega.Equal(1))
			gomega.Expect(anp.Status.Conditions[0].Type).To(gomega.Equal("Ready-In-Zone-global"))
			gomega.Expect(anp.Status.Conditions[0].Message).To(gomega.Equal("Setting up OVN DB plumbing was successful"))
			gomega.Expect(anp.Status.Conditions[0].Reason).To(gomega.Equal("SetupSucceeded"))
			gomega.Expect(anp.Status.Conditions[0].Status).To(gomega.Equal(metav1.ConditionTrue))
		})
		ginkgo.It("should be able to create two admin network policies at the same priority; ensure duplicate priority event is emitted", func() {
			app.Action = func(ctx *cli.Context) error {
				// now try to create a second admin network policy with same priority and ensure its created and triggers event
				ginkgo.By("2. creating an admin network policy draco-malfoy also at priority 5") // STEP1 in beforeEach
				dupANP := newANPObject("draco-malfoy", 5, anpSubject, []anpapi.AdminNetworkPolicyIngressRule{}, []anpapi.AdminNetworkPolicyEgressRule{})
				dupANP.ResourceVersion = "1"
				dupANP, err := fakeOVN.fakeClient.ANPClient.PolicyV1alpha1().AdminNetworkPolicies().Create(context.TODO(), dupANP, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(func() int {
					dupANP, err = fakeOVN.fakeClient.ANPClient.PolicyV1alpha1().AdminNetworkPolicies().Get(context.TODO(), "draco-malfoy", metav1.GetOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					return len(dupANP.Status.Conditions)
				}).Should(gomega.Equal(1))
				gomega.Expect(dupANP.Status.Conditions[0].Type).To(gomega.Equal("Ready-In-Zone-global"))
				gomega.Expect(dupANP.Status.Conditions[0].Message).To(gomega.Equal("Setting up OVN DB plumbing was successful"))
				gomega.Expect(dupANP.Status.Conditions[0].Reason).To(gomega.Equal("SetupSucceeded"))
				gomega.Expect(dupANP.Status.Conditions[0].Status).To(gomega.Equal(metav1.ConditionTrue))
				gomega.Expect(len(fakeOVN.fakeRecorder.Events)).To(gomega.Equal(1))
				recordedEvent := <-fakeOVN.fakeRecorder.Events // FIFO dequeued
				gomega.Expect(len(fakeOVN.fakeRecorder.Events)).To(gomega.Equal(0))
				gomega.Expect(recordedEvent).To(gomega.ContainSubstring(anpovn.ANPWithDuplicatePriorityEvent))
				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
		})
		ginkgo.It("deletion of existing ANP should be able to create a new ANP at the same priority without any event being emitted", func() {
			app.Action = func(ctx *cli.Context) error {
				// now try to delete the first existing ANP at that priority
				ginkgo.By("2. deleting harry-potter at priority 5") // STEP1 in beforeEach
				err := fakeOVN.fakeClient.ANPClient.PolicyV1alpha1().AdminNetworkPolicies().Delete(context.TODO(), "harry-potter", metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// now try to create a second admin network policy with same priority and ensure its created and does NOT trigger event
				ginkgo.By("3. creating an admin network policy draco-malfoy also at priority 5")
				dupANP := newANPObject("draco-malfoy", 5, anpSubject, []anpapi.AdminNetworkPolicyIngressRule{}, []anpapi.AdminNetworkPolicyEgressRule{})
				dupANP.ResourceVersion = "1"
				dupANP, err = fakeOVN.fakeClient.ANPClient.PolicyV1alpha1().AdminNetworkPolicies().Create(context.TODO(), dupANP, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(func() int {
					dupANP, err = fakeOVN.fakeClient.ANPClient.PolicyV1alpha1().AdminNetworkPolicies().Get(context.TODO(), "draco-malfoy", metav1.GetOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					return len(dupANP.Status.Conditions)
				}).Should(gomega.Equal(1))
				gomega.Expect(dupANP.Status.Conditions[0].Type).To(gomega.Equal("Ready-In-Zone-global"))
				gomega.Expect(dupANP.Status.Conditions[0].Message).To(gomega.Equal("Setting up OVN DB plumbing was successful"))
				gomega.Expect(dupANP.Status.Conditions[0].Reason).To(gomega.Equal("SetupSucceeded"))
				gomega.Expect(dupANP.Status.Conditions[0].Status).To(gomega.Equal(metav1.ConditionTrue))
				gomega.Expect(len(fakeOVN.fakeRecorder.Events)).To(gomega.Equal(0))
				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
		})
		ginkgo.It("should be able to create two admin network policies at different priorities; ensure duplicate priority event is NOT emitted", func() {
			app.Action = func(ctx *cli.Context) error {
				// check if "draco-malfoy" now get's created with no events
				ginkgo.By("2. creating an admin network policy draco-malfoy at priority 6") // STEP1 in beforeEach
				dupANP := newANPObject("draco-malfoy", 6, anpSubject, []anpapi.AdminNetworkPolicyIngressRule{}, []anpapi.AdminNetworkPolicyEgressRule{})
				dupANP.ResourceVersion = "1"
				dupANP, err := fakeOVN.fakeClient.ANPClient.PolicyV1alpha1().AdminNetworkPolicies().Create(context.TODO(), dupANP, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(func() int {
					dupANP, err = fakeOVN.fakeClient.ANPClient.PolicyV1alpha1().AdminNetworkPolicies().Get(context.TODO(), "draco-malfoy", metav1.GetOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					return len(dupANP.Status.Conditions)
				}).Should(gomega.Equal(1))
				gomega.Expect(dupANP.Status.Conditions[0].Type).To(gomega.Equal("Ready-In-Zone-global"))
				gomega.Expect(dupANP.Status.Conditions[0].Message).To(gomega.Equal("Setting up OVN DB plumbing was successful"))
				gomega.Expect(dupANP.Status.Conditions[0].Reason).To(gomega.Equal("SetupSucceeded"))
				gomega.Expect(dupANP.Status.Conditions[0].Status).To(gomega.Equal(metav1.ConditionTrue))
				gomega.Expect(len(fakeOVN.fakeRecorder.Events)).To(gomega.Equal(0)) // no new event
				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
		})
		ginkgo.It("updating an existing ANP's priority will cause priority conflict; ensure duplicate priority event is emitted", func() {
			app.Action = func(ctx *cli.Context) error {
				ginkgo.By("2. creating an admin network policy draco-malfoy at priority 6") // STEP1 in beforeEach
				dupANP := newANPObject("draco-malfoy", 6, anpSubject, []anpapi.AdminNetworkPolicyIngressRule{}, []anpapi.AdminNetworkPolicyEgressRule{})
				dupANP.ResourceVersion = "1"
				dupANP, err := fakeOVN.fakeClient.ANPClient.PolicyV1alpha1().AdminNetworkPolicies().Create(context.TODO(), dupANP, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(func() int {
					dupANP, err = fakeOVN.fakeClient.ANPClient.PolicyV1alpha1().AdminNetworkPolicies().Get(context.TODO(), "draco-malfoy", metav1.GetOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					return len(dupANP.Status.Conditions)
				}).Should(gomega.Equal(1))
				gomega.Expect(dupANP.Status.Conditions[0].Type).To(gomega.Equal("Ready-In-Zone-global"))
				gomega.Expect(dupANP.Status.Conditions[0].Message).To(gomega.Equal("Setting up OVN DB plumbing was successful"))
				gomega.Expect(dupANP.Status.Conditions[0].Reason).To(gomega.Equal("SetupSucceeded"))
				gomega.Expect(dupANP.Status.Conditions[0].Status).To(gomega.Equal(metav1.ConditionTrue))
				gomega.Expect(len(fakeOVN.fakeRecorder.Events)).To(gomega.Equal(0)) // no new event

				// now update the priority of harry-potter to 6 and ensure event is generated
				ginkgo.By("3. update existing harry-potter ANP's priority to 6")
				anp, err := fakeOVN.fakeClient.ANPClient.PolicyV1alpha1().AdminNetworkPolicies().Get(context.TODO(), "harry-potter", metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				anp.ResourceVersion = "2"
				anp.Spec.Priority = 6
				_, err = fakeOVN.fakeClient.ANPClient.PolicyV1alpha1().AdminNetworkPolicies().Update(context.TODO(), anp, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(func() string {
					anp, err = fakeOVN.fakeClient.ANPClient.PolicyV1alpha1().AdminNetworkPolicies().Get(context.TODO(), "harry-potter", metav1.GetOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					return anp.Status.Conditions[0].Message
				}).Should(gomega.Equal("Setting up OVN DB plumbing was successful"))
				gomega.Expect(anp.Status.Conditions[0].Type).To(gomega.Equal("Ready-In-Zone-global"))
				gomega.Expect(anp.Status.Conditions[0].Reason).To(gomega.Equal("SetupSucceeded"))
				gomega.Expect(anp.Status.Conditions[0].Status).To(gomega.Equal(metav1.ConditionTrue))
				gomega.Eventually(func() int {
					return len(fakeOVN.fakeRecorder.Events)
				}, "2s").Should(gomega.Equal(1))
				recordedEvent := <-fakeOVN.fakeRecorder.Events // FIFO dequeued
				gomega.Expect(recordedEvent).To(gomega.ContainSubstring(anpovn.ANPWithDuplicatePriorityEvent))
				gomega.Expect(len(fakeOVN.fakeRecorder.Events)).To(gomega.Equal(0))
				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
		})
	})
	ginkgo.Context("Priority upper bound limits", func() {
		anpSubject := newANPSubjectObject(
			&metav1.LabelSelector{
				MatchLabels: anpLabel,
			},
			nil,
		)
		ginkgo.BeforeEach(func() {
			fakeOVN.start()
			fakeOVN.InitAndRunANPController()
		})
		ginkgo.It("should not be able to create admin network policy with priority > 99", func() {
			app.Action = func(ctx *cli.Context) error {
				anp := newANPObject("harry-potter", 100, anpSubject, []anpapi.AdminNetworkPolicyIngressRule{}, []anpapi.AdminNetworkPolicyEgressRule{})
				anp.ResourceVersion = "1"
				anp, err := fakeOVN.fakeClient.ANPClient.PolicyV1alpha1().AdminNetworkPolicies().Create(context.TODO(), anp, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(func() int {
					anp, err = fakeOVN.fakeClient.ANPClient.PolicyV1alpha1().AdminNetworkPolicies().Get(context.TODO(), "harry-potter", metav1.GetOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					return len(anp.Status.Conditions)
				}).Should(gomega.Equal(1))
				gomega.Expect(anp.Status.Conditions[0].Type).To(gomega.Equal("Ready-In-Zone-global"))
				gomega.Expect(anp.Status.Conditions[0].Message).To(gomega.Equal("error attempting to add ANP harry-potter with " +
					"priority 100 because, OVNK only supports priority ranges 0-99"))
				gomega.Expect(anp.Status.Conditions[0].Reason).To(gomega.Equal("SetupFailed"))
				gomega.Expect(anp.Status.Conditions[0].Status).To(gomega.Equal(metav1.ConditionFalse))
				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
		})
		ginkgo.It("should not be able to update admin network policy to a priority > 99", func() {
			app.Action = func(ctx *cli.Context) error {
				ginkgo.By("create ANP at priority 5")
				anp := newANPObject("harry-potter", 5, anpSubject, []anpapi.AdminNetworkPolicyIngressRule{}, []anpapi.AdminNetworkPolicyEgressRule{})
				anp.ResourceVersion = "1"
				anp, err := fakeOVN.fakeClient.ANPClient.PolicyV1alpha1().AdminNetworkPolicies().Create(context.TODO(), anp, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(func() int {
					anp, err = fakeOVN.fakeClient.ANPClient.PolicyV1alpha1().AdminNetworkPolicies().Get(context.TODO(), "harry-potter", metav1.GetOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					return len(anp.Status.Conditions)
				}).Should(gomega.Equal(1))
				gomega.Expect(anp.Status.Conditions[0].Type).To(gomega.Equal("Ready-In-Zone-global"))
				gomega.Expect(anp.Status.Conditions[0].Message).To(gomega.Equal("Setting up OVN DB plumbing was successful"))
				gomega.Expect(anp.Status.Conditions[0].Reason).To(gomega.Equal("SetupSucceeded"))
				gomega.Expect(anp.Status.Conditions[0].Status).To(gomega.Equal(metav1.ConditionTrue))

				ginkgo.By("update ANP priority to 500 and fail updation; event must be generated")
				anp, err = fakeOVN.fakeClient.ANPClient.PolicyV1alpha1().AdminNetworkPolicies().Get(context.TODO(), "harry-potter", metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				anp.ResourceVersion = "2"
				anp.Spec.Priority = 500
				_, err = fakeOVN.fakeClient.ANPClient.PolicyV1alpha1().AdminNetworkPolicies().Update(context.TODO(), anp, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(func() string {
					anp, err = fakeOVN.fakeClient.ANPClient.PolicyV1alpha1().AdminNetworkPolicies().Get(context.TODO(), "harry-potter", metav1.GetOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					return anp.Status.Conditions[0].Message
				}).Should(gomega.Equal("error attempting to add ANP harry-potter with " +
					"priority 500 because, OVNK only supports priority ranges 0-99"))
				gomega.Expect(anp.Status.Conditions[0].Type).To(gomega.Equal("Ready-In-Zone-global"))
				gomega.Expect(anp.Status.Conditions[0].Reason).To(gomega.Equal("SetupFailed"))
				gomega.Expect(anp.Status.Conditions[0].Status).To(gomega.Equal(metav1.ConditionFalse))
				gomega.Eventually(func() int {
					return len(fakeOVN.fakeRecorder.Events)
				}, "2s").Should(gomega.Equal(1))
				recordedEvent := <-fakeOVN.fakeRecorder.Events // FIFO dequeued
				gomega.Expect(recordedEvent).To(gomega.ContainSubstring(anpovn.ANPWithUnsupportedPriorityEvent))
				gomega.Expect(len(fakeOVN.fakeRecorder.Events)).To(gomega.Equal(0))
				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
		})
	})
})
