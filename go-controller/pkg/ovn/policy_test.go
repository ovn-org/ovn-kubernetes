package ovn

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/onsi/ginkgo"
	"github.com/onsi/ginkgo/extensions/table"
	"github.com/onsi/gomega"
	"github.com/onsi/gomega/format"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"

	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"

	"github.com/urfave/cli/v2"

	v1 "k8s.io/api/core/v1"
	knet "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apimachinerytypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	utilnet "k8s.io/utils/net"
)

type ipMode struct {
	IPv4Mode bool
	IPv6Mode bool
}

// FIXME DUAL-STACK: FakeOVN doesn't really support adding more than one
// pod to the namespace. All logical ports would share the same fakeUUID.
// When this is addressed we can add an entry for
// IPv4Mode = true, IPv6Mode = true.
func getIpModes() []ipMode {
	return []ipMode{
		{true, false},
		{false, true},
	}
}

func ipModeStr(m ipMode) string {
	return fmt.Sprintf("(IPv4 %t IPv6 %t)", m.IPv4Mode, m.IPv6Mode)
}

func setIpMode(m ipMode) {
	config.IPv4Mode = m.IPv4Mode
	config.IPv6Mode = m.IPv6Mode
}

type kNetworkPolicy struct{}

func newNetworkPolicyMeta(name, namespace string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		UID:       apimachinerytypes.UID(namespace),
		Name:      name,
		Namespace: namespace,
		Labels: map[string]string{
			"name": name,
		},
	}
}

func newNetworkPolicy(name, namespace string, podSelector metav1.LabelSelector, ingress []knet.NetworkPolicyIngressRule, egress []knet.NetworkPolicyEgressRule) *knet.NetworkPolicy {
	return &knet.NetworkPolicy{
		ObjectMeta: newNetworkPolicyMeta(name, namespace),
		Spec: knet.NetworkPolicySpec{
			PodSelector: podSelector,
			Ingress:     ingress,
			Egress:      egress,
		},
	}
}

const (
	ingressDenyPG string = "ingressDefaultDeny"
	egressDenyPG  string = "egressDefaultDeny"
)

func (n kNetworkPolicy) getDefaultDenyData(networkPolicy *knet.NetworkPolicy, ports []string, logSeverity nbdb.ACLSeverity, oldEgress bool) []libovsdb.TestData {
	pgHash := hashedPortGroup(networkPolicy.Namespace)
	shouldBeLogged := logSeverity != nbdb.ACLSeverityInfo
	egressDirection := nbdb.ACLDirectionFromLport
	egressOptions := map[string]string{
		"apply-after-lb": "true",
	}
	if oldEgress {
		egressDirection = nbdb.ACLDirectionToLport
		egressOptions = map[string]string{}
	}
	egressDenyACL := libovsdbops.BuildACL(
		networkPolicy.Namespace+"_"+egressDefaultDenySuffix,
		egressDirection,
		types.DefaultDenyPriority,
		"inport == @"+pgHash+"_"+egressDenyPG,
		nbdb.ACLActionDrop,
		types.OvnACLLoggingMeter,
		logSeverity,
		shouldBeLogged,
		map[string]string{
			defaultDenyPolicyTypeACLExtIdKey: string(knet.PolicyTypeEgress),
		},
		egressOptions,
	)
	egressDenyACL.UUID = *egressDenyACL.Name + "-egressDenyACL-UUID"

	egressAllowACL := libovsdbops.BuildACL(
		networkPolicy.Namespace+"_"+arpAllowPolicySuffix,
		egressDirection,
		types.DefaultAllowPriority,
		"inport == @"+pgHash+"_"+egressDenyPG+" && (arp || nd)",
		nbdb.ACLActionAllow,
		types.OvnACLLoggingMeter,
		nbdb.ACLSeverityInfo,
		false,
		map[string]string{
			defaultDenyPolicyTypeACLExtIdKey: string(knet.PolicyTypeEgress),
		},
		egressOptions,
	)
	egressAllowACL.UUID = *egressAllowACL.Name + "-egressAllowACL-UUID"

	ingressDenyACL := libovsdbops.BuildACL(
		networkPolicy.Namespace+"_"+ingressDefaultDenySuffix,
		nbdb.ACLDirectionToLport,
		types.DefaultDenyPriority,
		"outport == @"+pgHash+"_"+ingressDenyPG,
		nbdb.ACLActionDrop,
		types.OvnACLLoggingMeter,
		logSeverity,
		shouldBeLogged,
		map[string]string{
			defaultDenyPolicyTypeACLExtIdKey: string(knet.PolicyTypeIngress),
		},
		nil,
	)
	ingressDenyACL.UUID = *ingressDenyACL.Name + "-ingressDenyACL-UUID"

	ingressAllowACL := libovsdbops.BuildACL(
		networkPolicy.Namespace+"_"+arpAllowPolicySuffix,
		nbdb.ACLDirectionToLport,
		types.DefaultAllowPriority,
		"outport == @"+pgHash+"_"+ingressDenyPG+" && (arp || nd)",
		nbdb.ACLActionAllow,
		types.OvnACLLoggingMeter,
		nbdb.ACLSeverityInfo,
		false,
		map[string]string{
			defaultDenyPolicyTypeACLExtIdKey: string(knet.PolicyTypeIngress),
		},
		nil,
	)
	ingressAllowACL.UUID = *ingressAllowACL.Name + "-ingressAllowACL-UUID"

	lsps := []*nbdb.LogicalSwitchPort{}
	for _, uuid := range ports {
		lsps = append(lsps, &nbdb.LogicalSwitchPort{UUID: uuid})
	}

	egressDenyPG := libovsdbops.BuildPortGroup(
		pgHash+"_"+egressDenyPG,
		pgHash+"_"+egressDenyPG,
		lsps,
		[]*nbdb.ACL{egressDenyACL, egressAllowACL},
	)
	egressDenyPG.UUID = egressDenyPG.Name + "-UUID"

	ingressDenyPG := libovsdbops.BuildPortGroup(
		pgHash+"_"+ingressDenyPG,
		pgHash+"_"+ingressDenyPG,
		lsps,
		[]*nbdb.ACL{ingressDenyACL, ingressAllowACL},
	)
	ingressDenyPG.UUID = ingressDenyPG.Name + "-UUID"

	return []libovsdb.TestData{
		egressDenyACL,
		egressAllowACL,
		ingressDenyACL,
		ingressAllowACL,
		egressDenyPG,
		ingressDenyPG,
	}
}

func (n kNetworkPolicy) getPolicyData(networkPolicy *knet.NetworkPolicy, policyPorts []string, peerNamespaces []string, tcpPeerPorts []int32, logSeverity nbdb.ACLSeverity, oldEgress bool) []libovsdb.TestData {
	pgName := networkPolicy.Namespace + "_" + networkPolicy.Name
	pgHash := hashedPortGroup(pgName)
	acls := []*nbdb.ACL{}

	shouldBeLogged := logSeverity != nbdb.ACLSeverityInfo
	for i := range networkPolicy.Spec.Ingress {
		aclName := networkPolicy.Namespace + "_" + networkPolicy.Name + "_" + strconv.Itoa(i)
		policyType := string(knet.PolicyTypeIngress)
		if peerNamespaces != nil {
			ingressAsMatch := asMatch(append(peerNamespaces, getAddressSetName(networkPolicy.Namespace, networkPolicy.Name, knet.PolicyTypeIngress, i)))
			acl := libovsdbops.BuildACL(
				aclName,
				nbdb.ACLDirectionToLport,
				types.DefaultAllowPriority,
				fmt.Sprintf("ip4.src == {%s} && outport == @%s", ingressAsMatch, pgHash),
				nbdb.ACLActionAllowRelated,
				types.OvnACLLoggingMeter,
				logSeverity,
				shouldBeLogged,
				map[string]string{
					l4MatchACLExtIdKey:     "None",
					ipBlockCIDRACLExtIdKey: "false",
					namespaceACLExtIdKey:   networkPolicy.Namespace,
					policyACLExtIdKey:      networkPolicy.Name,
					policyTypeACLExtIdKey:  policyType,
					policyType + "_num":    strconv.Itoa(i),
				},
				nil,
			)
			acl.UUID = *acl.Name + "-ingressACL-UUID"
			acls = append(acls, acl)
		}
		for _, v := range tcpPeerPorts {
			acl := libovsdbops.BuildACL(
				aclName,
				nbdb.ACLDirectionToLport,
				types.DefaultAllowPriority,
				fmt.Sprintf("ip4 && tcp && tcp.dst==%d && outport == @%s", v, pgHash),
				nbdb.ACLActionAllowRelated,
				types.OvnACLLoggingMeter,
				logSeverity,
				shouldBeLogged,
				map[string]string{
					l4MatchACLExtIdKey:     fmt.Sprintf("tcp && tcp.dst==%d", v),
					ipBlockCIDRACLExtIdKey: "false",
					namespaceACLExtIdKey:   networkPolicy.Namespace,
					policyACLExtIdKey:      networkPolicy.Name,
					policyTypeACLExtIdKey:  policyType,
					policyType + "_num":    strconv.Itoa(i),
				},
				nil,
			)
			acl.UUID = fmt.Sprintf("%s-ingressACL-port_%d-UUID", *acl.Name, v)
			acls = append(acls, acl)
		}
	}

	for i := range networkPolicy.Spec.Egress {
		direction := nbdb.ACLDirectionFromLport
		options := map[string]string{
			"apply-after-lb": "true",
		}
		if oldEgress {
			direction = nbdb.ACLDirectionToLport
			options = map[string]string{}
		}
		aclName := networkPolicy.Namespace + "_" + networkPolicy.Name + "_" + strconv.Itoa(i)
		policyType := string(knet.PolicyTypeEgress)
		if peerNamespaces != nil {
			egressAsMatch := asMatch(append(peerNamespaces, getAddressSetName(networkPolicy.Namespace, networkPolicy.Name, knet.PolicyTypeEgress, i)))
			acl := libovsdbops.BuildACL(
				aclName,
				direction,
				types.DefaultAllowPriority,
				fmt.Sprintf("ip4.dst == {%s} && inport == @%s", egressAsMatch, pgHash),
				nbdb.ACLActionAllowRelated,
				types.OvnACLLoggingMeter,
				logSeverity,
				shouldBeLogged,
				map[string]string{
					l4MatchACLExtIdKey:     "None",
					ipBlockCIDRACLExtIdKey: "false",
					namespaceACLExtIdKey:   networkPolicy.Namespace,
					policyACLExtIdKey:      networkPolicy.Name,
					policyTypeACLExtIdKey:  policyType,
					policyType + "_num":    strconv.Itoa(i),
				},
				options,
			)
			acl.UUID = *acl.Name + "-egressACL-UUID"
			acls = append(acls, acl)
		}
		for _, v := range tcpPeerPorts {
			acl := libovsdbops.BuildACL(
				aclName,
				direction,
				types.DefaultAllowPriority,
				fmt.Sprintf("ip4 && tcp && tcp.dst==%d && inport == @%s", v, pgHash),
				nbdb.ACLActionAllowRelated,
				types.OvnACLLoggingMeter,
				logSeverity,
				shouldBeLogged,
				map[string]string{
					l4MatchACLExtIdKey:     fmt.Sprintf("tcp && tcp.dst==%d", v),
					ipBlockCIDRACLExtIdKey: "false",
					namespaceACLExtIdKey:   networkPolicy.Namespace,
					policyACLExtIdKey:      networkPolicy.Name,
					policyTypeACLExtIdKey:  policyType,
					policyType + "_num":    strconv.Itoa(i),
				},
				options,
			)
			acl.UUID = fmt.Sprintf("%s-egressACL-port_%d-UUID", *acl.Name, v)
			acls = append(acls, acl)
		}
	}

	lsps := []*nbdb.LogicalSwitchPort{}
	for _, uuid := range policyPorts {
		lsps = append(lsps, &nbdb.LogicalSwitchPort{UUID: uuid})
	}

	pg := libovsdbops.BuildPortGroup(
		pgHash,
		pgName,
		lsps,
		acls,
	)
	pg.UUID = pg.Name + "-UUID"

	data := []libovsdb.TestData{}
	for _, acl := range acls {
		data = append(data, acl)
	}
	data = append(data, pg)
	return data
}

func (n kNetworkPolicy) generateExpectedACLDataForFirstPolicyOnNamespace(policy knet.NetworkPolicy, loggingSeverity nbdb.ACLSeverity) []libovsdb.TestData {
	var expectedData []libovsdb.TestData
	expectedData = append(
		expectedData,
		n.getDefaultDenyData(&policy, nil, loggingSeverity, false)...)
	return append(
		expectedData,
		n.getPolicyData(&policy, nil, []string{}, nil, loggingSeverity, false)...)
}

func getAddressSetName(namespace, name string, policyType knet.PolicyType, idx int) string {
	direction := strings.ToLower(string(policyType))
	return fmt.Sprintf("%s.%s.%s.%d", namespace, name, direction, idx)
}

func eventuallyExpectNoAddressSets(fakeOvn *FakeOVN, networkPolicy *knet.NetworkPolicy) {
	for i := range networkPolicy.Spec.Ingress {
		asName := getAddressSetName(networkPolicy.Namespace, networkPolicy.Name, knet.PolicyTypeIngress, i)
		fakeOvn.asf.EventuallyExpectNoAddressSet(asName)
	}
	for i := range networkPolicy.Spec.Egress {
		asName := getAddressSetName(networkPolicy.Namespace, networkPolicy.Name, knet.PolicyTypeEgress, i)
		fakeOvn.asf.EventuallyExpectNoAddressSet(asName)
	}
}

func eventuallyExpectAddressSetsWithIP(fakeOvn *FakeOVN, networkPolicy *knet.NetworkPolicy, ip string) {
	for i := range networkPolicy.Spec.Ingress {
		asName := getAddressSetName(networkPolicy.Namespace, networkPolicy.Name, knet.PolicyTypeIngress, i)
		fakeOvn.asf.EventuallyExpectAddressSetWithIPs(asName, []string{ip})
	}
	for i := range networkPolicy.Spec.Egress {
		asName := getAddressSetName(networkPolicy.Namespace, networkPolicy.Name, knet.PolicyTypeEgress, i)
		fakeOvn.asf.EventuallyExpectAddressSetWithIPs(asName, []string{ip})
	}
}

func eventuallyExpectEmptyAddressSetsExist(fakeOvn *FakeOVN, networkPolicy *knet.NetworkPolicy) {
	for i := range networkPolicy.Spec.Ingress {
		asName := getAddressSetName(networkPolicy.Namespace, networkPolicy.Name, knet.PolicyTypeIngress, i)
		fakeOvn.asf.EventuallyExpectEmptyAddressSetExist(asName)
	}
	for i := range networkPolicy.Spec.Egress {
		asName := getAddressSetName(networkPolicy.Namespace, networkPolicy.Name, knet.PolicyTypeEgress, i)
		fakeOvn.asf.EventuallyExpectEmptyAddressSetExist(asName)
	}
}

type multicastPolicy struct{}

func (p multicastPolicy) getMulticastPolicyExpectedData(ns string, ports []string) []libovsdb.TestData {
	pg_hash := hashedPortGroup(ns)
	egressMatch := getACLMatch(pg_hash, getMulticastACLEgrMatch(), knet.PolicyTypeEgress)

	ip4AddressSet, ip6AddressSet := addressset.MakeAddressSetHashNames(ns)
	mcastMatch := getACLMatchAF(getMulticastACLIgrMatchV4(ip4AddressSet), getMulticastACLIgrMatchV6(ip6AddressSet))
	ingressMatch := getACLMatch(pg_hash, mcastMatch, knet.PolicyTypeIngress)

	egressACL := libovsdbops.BuildACL(
		ns+"_MulticastAllowEgress",
		nbdb.ACLDirectionFromLport,
		types.DefaultMcastAllowPriority,
		egressMatch,
		nbdb.ACLActionAllow,
		types.OvnACLLoggingMeter,
		nbdb.ACLSeverityInfo,
		false,
		map[string]string{
			defaultDenyPolicyTypeACLExtIdKey: string(knet.PolicyTypeEgress),
		},
		map[string]string{
			"apply-after-lb": "true",
		},
	)
	egressACL.UUID = *egressACL.Name + "-UUID"

	ingressACL := libovsdbops.BuildACL(
		ns+"_MulticastAllowIngress",
		nbdb.ACLDirectionToLport,
		types.DefaultMcastAllowPriority,
		ingressMatch,
		nbdb.ACLActionAllow,
		types.OvnACLLoggingMeter,
		nbdb.ACLSeverityInfo,
		false,
		map[string]string{
			defaultDenyPolicyTypeACLExtIdKey: string(knet.PolicyTypeIngress),
		},
		nil,
	)
	ingressACL.UUID = *ingressACL.Name + "-UUID"

	lsps := []*nbdb.LogicalSwitchPort{}
	for _, uuid := range ports {
		lsps = append(lsps, &nbdb.LogicalSwitchPort{UUID: uuid})
	}

	pg := libovsdbops.BuildPortGroup(
		hashedPortGroup(ns),
		ns,
		lsps,
		[]*nbdb.ACL{egressACL, ingressACL},
	)
	pg.UUID = pg.Name + "-UUID"

	return []libovsdb.TestData{
		egressACL,
		ingressACL,
		pg,
	}
}

var _ = ginkgo.Describe("OVN NetworkPolicy Operations with IP Address Family", func() {
	const (
		namespaceName1 = "namespace1"
		namespaceName2 = "namespace2"
	)
	var (
		app                   *cli.App
		fakeOvn               *FakeOVN
		initialDB             libovsdb.TestSetup
		gomegaFormatMaxLength int
	)

	ginkgo.BeforeEach(func() {
		// Restore global default values before each testcase
		config.PrepareTestConfig()
		config.IPv4Mode = true
		config.IPv6Mode = false

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

		fakeOvn = NewFakeOVN()
		gomegaFormatMaxLength = format.MaxLength
		format.MaxLength = 0
		initialDB = libovsdb.TestSetup{
			NBData: []libovsdb.TestData{
				&nbdb.LogicalSwitch{
					Name: "node1",
				},
			},
		}
	})

	ginkgo.AfterEach(func() {
		fakeOvn.shutdown()
		format.MaxLength = gomegaFormatMaxLength
	})

	ginkgo.Context("during execution", func() {
		for _, m := range getIpModes() {
			m := m
			ginkgo.It("tests enabling/disabling multicast in a namespace "+ipModeStr(m), func() {
				app.Action = func(ctx *cli.Context) error {
					namespace1 := *newNamespace(namespaceName1)

					fakeOvn.startWithDBSetup(libovsdb.TestSetup{},
						&v1.NamespaceList{
							Items: []v1.Namespace{
								namespace1,
							},
						},
					)
					setIpMode(m)

					err := fakeOvn.controller.WatchNamespaces()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					ns, err := fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Get(context.TODO(), namespace1.Name, metav1.GetOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					gomega.Expect(ns).NotTo(gomega.BeNil())

					// Multicast is denied by default.
					_, ok := ns.Annotations[nsMulticastAnnotation]
					gomega.Expect(ok).To(gomega.BeFalse())

					// Enable multicast in the namespace.
					mcastPolicy := multicastPolicy{}
					expectedData := mcastPolicy.getMulticastPolicyExpectedData(namespace1.Name, nil)
					ns.Annotations[nsMulticastAnnotation] = "true"
					_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Update(context.TODO(), ns, metav1.UpdateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

					// Disable multicast in the namespace.
					ns.Annotations[nsMulticastAnnotation] = "false"
					_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Update(context.TODO(), ns, metav1.UpdateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					acls := expectedData[:len(expectedData)-1]
					gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(acls))
					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			})

			ginkgo.It("tests enabling multicast in a namespace with a pod "+ipModeStr(m), func() {
				app.Action = func(ctx *cli.Context) error {
					namespace1 := *newNamespace(namespaceName1)
					nPodTestV4 := newTPod(
						"node1",
						"10.128.1.0/24",
						"10.128.1.2",
						"10.128.1.1",
						"myPod1",
						"10.128.1.3",
						"0a:58:0a:80:01:03",
						namespace1.Name,
					)
					nPodTestV6 := newTPod(
						"node1",
						"fd00:10:244::/64",
						"fd00:10:244::2",
						"fd00:10:244::1",
						"myPod2",
						"fd00:10:244::3",
						"0a:58:dd:33:05:d8",
						namespace1.Name,
					)
					var tPods []testPod
					var tPodIPs []string
					if m.IPv4Mode {
						tPods = append(tPods, nPodTestV4)
						tPodIPs = append(tPodIPs, nPodTestV4.podIP)
					}
					if m.IPv6Mode {
						tPods = append(tPods, nPodTestV6)
						tPodIPs = append(tPodIPs, nPodTestV6.podIP)
					}

					var pods []v1.Pod
					for _, tPod := range tPods {
						pods = append(pods, *newPod(tPod.namespace, tPod.podName, tPod.nodeName, tPod.podIP))
					}

					fakeOvn.startWithDBSetup(initialDB,
						&v1.NamespaceList{
							Items: []v1.Namespace{
								namespace1,
							},
						},
						&v1.PodList{
							Items: pods,
						},
					)
					setIpMode(m)

					for _, tPod := range tPods {
						tPod.populateLogicalSwitchCache(fakeOvn, getLogicalSwitchUUID(fakeOvn.controller.nbClient, "node1"))
					}

					err := fakeOvn.controller.WatchNamespaces()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					fakeOvn.controller.WatchPods()
					ns, err := fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Get(context.TODO(), namespace1.Name, metav1.GetOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					gomega.Expect(ns).NotTo(gomega.BeNil())

					// Enable multicast in the namespace
					mcastPolicy := multicastPolicy{}
					ports := []string{}
					for _, tPod := range tPods {
						ports = append(ports, tPod.portUUID)
					}

					expectedData := mcastPolicy.getMulticastPolicyExpectedData(namespace1.Name, ports)
					expectedData = append(expectedData, getExpectedDataPodsAndSwitches(tPods, []string{"node1"})...)
					ns.Annotations[nsMulticastAnnotation] = "true"
					_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Update(context.TODO(), ns, metav1.UpdateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))
					fakeOvn.asf.ExpectAddressSetWithIPs(namespace1.Name, tPodIPs)
					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			})

			ginkgo.It("tests adding a pod to a multicast enabled namespace "+ipModeStr(m), func() {
				app.Action = func(ctx *cli.Context) error {
					namespace1 := *newNamespace(namespaceName1)
					nPodTestV4 := newTPod(
						"node1",
						"10.128.1.0/24",
						"10.128.1.2",
						"10.128.1.1",
						"myPod1",
						"10.128.1.3",
						"0a:58:0a:80:01:03",
						namespace1.Name,
					)
					nPodTestV6 := newTPod(
						"node1",
						"fd00:10:244::/64",
						"fd00:10:244::2",
						"fd00:10:244::1",
						"myPod2",
						"fd00:10:244::3",
						"0a:58:dd:33:05:d8",
						namespace1.Name,
					)
					var tPods []testPod
					var tPodIPs []string
					ports := []string{}
					if m.IPv4Mode {
						tPods = append(tPods, nPodTestV4)
						tPodIPs = append(tPodIPs, nPodTestV4.podIP)
					}
					if m.IPv6Mode {
						tPods = append(tPods, nPodTestV6)
						tPodIPs = append(tPodIPs, nPodTestV6.podIP)
					}
					for _, pod := range tPods {
						ports = append(ports, pod.portUUID)
					}

					fakeOvn.startWithDBSetup(initialDB,
						&v1.NamespaceList{
							Items: []v1.Namespace{
								namespace1,
							},
						},
					)
					setIpMode(m)

					err := fakeOvn.controller.WatchNamespaces()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					fakeOvn.controller.WatchPods()
					ns, err := fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Get(context.TODO(), namespace1.Name, metav1.GetOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					gomega.Expect(ns).NotTo(gomega.BeNil())

					// Enable multicast in the namespace.
					mcastPolicy := multicastPolicy{}
					expectedData := mcastPolicy.getMulticastPolicyExpectedData(namespace1.Name, nil)
					ns.Annotations[nsMulticastAnnotation] = "true"
					_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Update(context.TODO(), ns, metav1.UpdateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(append(expectedData, &nbdb.LogicalSwitch{
						UUID: "node1-UUID",
						Name: "node1",
					})...))

					for _, tPod := range tPods {
						tPod.populateLogicalSwitchCache(fakeOvn, getLogicalSwitchUUID(fakeOvn.controller.nbClient, "node1"))
						_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(tPod.namespace).Create(context.TODO(), newPod(
							tPod.namespace, tPod.podName, tPod.nodeName, tPod.podIP), metav1.CreateOptions{})
						gomega.Expect(err).NotTo(gomega.HaveOccurred())
					}
					fakeOvn.asf.EventuallyExpectAddressSetWithIPs(namespace1.Name, tPodIPs)
					expectedDataWithPods := mcastPolicy.getMulticastPolicyExpectedData(namespace1.Name, ports)
					expectedDataWithPods = append(expectedDataWithPods, getExpectedDataPodsAndSwitches(tPods, []string{"node1"})...)
					gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedDataWithPods...))

					for _, tPod := range tPods {
						// Delete the pod from the namespace.
						err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(tPod.namespace).Delete(context.TODO(),
							tPod.podName, *metav1.NewDeleteOptions(0))
						gomega.Expect(err).NotTo(gomega.HaveOccurred())
					}
					fakeOvn.asf.EventuallyExpectEmptyAddressSetExist(namespace1.Name)
					gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(append(expectedData, &nbdb.LogicalSwitch{
						UUID: "node1-UUID",
						Name: "node1",
					})...))

					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			})
		}
	})
})

var _ = ginkgo.Describe("OVN NetworkPolicy Operations", func() {
	const (
		namespaceName1 = "namespace1"

		namespaceName2 = "namespace2"
	)
	var (
		app       *cli.App
		fakeOvn   *FakeOVN
		initialDB libovsdb.TestSetup

		gomegaFormatMaxLength int
	)

	ginkgo.BeforeEach(func() {
		// Restore global default values before each testcase
		config.PrepareTestConfig()

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

		fakeOvn = NewFakeOVN()

		gomegaFormatMaxLength = format.MaxLength
		format.MaxLength = 0
		initialDB = libovsdb.TestSetup{
			NBData: []libovsdb.TestData{
				&nbdb.LogicalSwitch{
					Name: "node1",
				},
			},
		}
	})

	ginkgo.AfterEach(func() {
		fakeOvn.shutdown()
		format.MaxLength = gomegaFormatMaxLength
	})

	ginkgo.Context("on startup", func() {

		ginkgo.It("reconciles an existing ingress networkPolicy with a namespace selector", func() {
			app.Action = func(ctx *cli.Context) error {

				namespace1 := *newNamespace(namespaceName1)
				namespace2 := *newNamespace(namespaceName2)
				networkPolicy := newNetworkPolicy("networkpolicy1", namespace1.Name,
					metav1.LabelSelector{},
					[]knet.NetworkPolicyIngressRule{
						{
							From: []knet.NetworkPolicyPeer{
								{
									NamespaceSelector: &metav1.LabelSelector{
										MatchLabels: map[string]string{
											"name": namespace2.Name,
										},
									},
								},
							},
						},
					},
					[]knet.NetworkPolicyEgressRule{
						{
							To: []knet.NetworkPolicyPeer{
								{
									NamespaceSelector: &metav1.LabelSelector{
										MatchLabels: map[string]string{
											"name": namespace2.Name,
										},
									},
								},
							},
						},
					})

				npTest := kNetworkPolicy{}
				gressPolicyExpectedData := npTest.getPolicyData(networkPolicy, nil, []string{namespace2.Name}, nil, nbdb.ACLSeverityInfo, false)
				defaultDenyExpectedData := npTest.getDefaultDenyData(networkPolicy, nil, nbdb.ACLSeverityInfo, false)
				expectedData := []libovsdb.TestData{}
				expectedData = append(expectedData, gressPolicyExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)

				fakeOvn.startWithDBSetup(initialDB,
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespace1,
							namespace2,
						},
					},
					&knet.NetworkPolicyList{
						Items: []knet.NetworkPolicy{
							*networkPolicy,
						},
					},
				)

				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.controller.WatchNetworkPolicy()

				fakeOvn.asf.ExpectEmptyAddressSet(namespaceName1)
				fakeOvn.asf.ExpectEmptyAddressSet(namespaceName2)

				eventuallyExpectEmptyAddressSetsExist(fakeOvn, networkPolicy)

				_, err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(append(expectedData, &nbdb.LogicalSwitch{
					UUID: "node1-UUID",
					Name: "node1",
				})...))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
		ginkgo.It("reconciles an egress networkPolicy updating existing ACLs", func() {
			app.Action = func(ctx *cli.Context) error {
				namespace1 := *newNamespace(namespaceName1)
				namespace2 := *newNamespace(namespaceName2)
				networkPolicy := newNetworkPolicy("networkpolicy1", namespace1.Name,
					metav1.LabelSelector{},
					[]knet.NetworkPolicyIngressRule{
						{
							From: []knet.NetworkPolicyPeer{
								{
									NamespaceSelector: &metav1.LabelSelector{
										MatchLabels: map[string]string{
											"name": namespace2.Name,
										},
									},
								},
							},
						},
					},
					[]knet.NetworkPolicyEgressRule{
						{
							To: []knet.NetworkPolicyPeer{
								{
									NamespaceSelector: &metav1.LabelSelector{
										MatchLabels: map[string]string{
											"name": namespace2.Name,
										},
									},
								},
							},
						},
					})
				npTest := kNetworkPolicy{}
				gressPolicyInitialData := npTest.getPolicyData(networkPolicy, nil, []string{namespace2.Name}, nil, nbdb.ACLSeverityInfo, true)
				defaultDenyInitialData := npTest.getDefaultDenyData(networkPolicy, nil, nbdb.ACLSeverityInfo, true)
				initialData := []libovsdb.TestData{}
				initialData = append(initialData, gressPolicyInitialData...)
				initialData = append(initialData, defaultDenyInitialData...)
				fakeOvn.startWithDBSetup(
					libovsdb.TestSetup{
						NBData: initialData,
					},
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespace1,
							namespace2,
						},
					},
					&knet.NetworkPolicyList{
						Items: []knet.NetworkPolicy{
							*networkPolicy,
						},
					},
				)

				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.controller.syncNetworkPolicies([]interface{}{networkPolicy})

				fakeOvn.asf.ExpectEmptyAddressSet(namespaceName1)
				fakeOvn.asf.ExpectEmptyAddressSet(namespaceName2)

				eventuallyExpectEmptyAddressSetsExist(fakeOvn, networkPolicy)

				_, err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectedData := npTest.getPolicyData(networkPolicy, nil, []string{namespace2.Name}, nil, nbdb.ACLSeverityInfo, false)
				defaultDenyExpectedData := npTest.getDefaultDenyData(networkPolicy, nil, nbdb.ACLSeverityInfo, false)
				expectedData = append(expectedData, defaultDenyExpectedData...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

		})

		ginkgo.It("reconciles an ingress networkPolicy updating an existing ACL", func() {
			app.Action = func(ctx *cli.Context) error {

				namespace1 := *newNamespace(namespaceName1)
				namespace2 := *newNamespace(namespaceName2)
				networkPolicy := newNetworkPolicy("networkpolicy1", namespace1.Name,
					metav1.LabelSelector{},
					[]knet.NetworkPolicyIngressRule{
						{
							From: []knet.NetworkPolicyPeer{
								{
									NamespaceSelector: &metav1.LabelSelector{
										MatchLabels: map[string]string{
											"name": namespace2.Name,
										},
									},
								},
							},
						},
					},
					[]knet.NetworkPolicyEgressRule{
						{
							To: []knet.NetworkPolicyPeer{
								{
									NamespaceSelector: &metav1.LabelSelector{
										MatchLabels: map[string]string{
											"name": namespace2.Name,
										},
									},
								},
							},
						},
					})

				npTest := kNetworkPolicy{}
				gressPolicyInitialData := npTest.getPolicyData(networkPolicy, nil, []string{}, nil, nbdb.ACLSeverityInfo, false)
				defaultDenyInitialData := npTest.getDefaultDenyData(networkPolicy, nil, nbdb.ACLSeverityInfo, false)
				initialData := []libovsdb.TestData{}
				initialData = append(initialData, gressPolicyInitialData...)
				initialData = append(initialData, defaultDenyInitialData...)

				fakeOvn.startWithDBSetup(
					libovsdb.TestSetup{
						NBData: initialData,
					},
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespace1,
							namespace2,
						},
					},
					&knet.NetworkPolicyList{
						Items: []knet.NetworkPolicy{
							*networkPolicy,
						},
					},
				)

				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.controller.WatchNetworkPolicy()

				fakeOvn.asf.ExpectEmptyAddressSet(namespaceName1)
				fakeOvn.asf.ExpectEmptyAddressSet(namespaceName2)

				eventuallyExpectEmptyAddressSetsExist(fakeOvn, networkPolicy)

				_, err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectedData := npTest.getPolicyData(networkPolicy, nil, []string{namespace2.Name}, nil, nbdb.ACLSeverityInfo, false)
				expectedData = append(expectedData, defaultDenyInitialData...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("reconciles an existing gress networkPolicy with a pod selector in its own namespace", func() {
			app.Action = func(ctx *cli.Context) error {

				namespace1 := *newNamespace(namespaceName1)

				nPodTest := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespace1.Name,
				)
				networkPolicy := newNetworkPolicy("networkpolicy1", namespace1.Name,
					metav1.LabelSelector{},
					[]knet.NetworkPolicyIngressRule{
						{
							From: []knet.NetworkPolicyPeer{
								{
									PodSelector: &metav1.LabelSelector{
										MatchLabels: map[string]string{
											"name": nPodTest.podName,
										},
									},
								},
							},
						},
					},
					[]knet.NetworkPolicyEgressRule{
						{
							To: []knet.NetworkPolicyPeer{
								{
									PodSelector: &metav1.LabelSelector{
										MatchLabels: map[string]string{
											"name": nPodTest.podName,
										},
									},
								},
							},
						},
					})

				npTest := kNetworkPolicy{}
				gressPolicyExpectedData := npTest.getPolicyData(networkPolicy, []string{nPodTest.portUUID}, []string{}, nil, nbdb.ACLSeverityInfo, false)
				defaultDenyExpectedData := npTest.getDefaultDenyData(networkPolicy, []string{nPodTest.portUUID}, nbdb.ACLSeverityInfo, false)
				expectedData := []libovsdb.TestData{}
				expectedData = append(expectedData, gressPolicyExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)
				expectedData = append(expectedData, getExpectedDataPodsAndSwitches([]testPod{nPodTest}, []string{"node1"})...)

				fakeOvn.startWithDBSetup(initialDB,
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespace1,
						},
					},
					&v1.PodList{
						Items: []v1.Pod{
							*newPod(nPodTest.namespace, nPodTest.podName, nPodTest.nodeName, nPodTest.podIP),
						},
					},
					&knet.NetworkPolicyList{
						Items: []knet.NetworkPolicy{
							*networkPolicy,
						},
					},
				)
				nPodTest.populateLogicalSwitchCache(fakeOvn, getLogicalSwitchUUID(fakeOvn.controller.nbClient, "node1"))
				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.controller.WatchPods()
				fakeOvn.controller.WatchNetworkPolicy()

				eventuallyExpectAddressSetsWithIP(fakeOvn, networkPolicy, nPodTest.podIP)
				fakeOvn.asf.ExpectAddressSetWithIPs(namespaceName1, []string{nPodTest.podIP})

				_, err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("reconciles an existing gress networkPolicy with a pod and namespace selector in another namespace", func() {
			app.Action = func(ctx *cli.Context) error {

				namespace1 := *newNamespace(namespaceName1)
				namespace2 := *newNamespace(namespaceName2)

				nPodTest := newTPod(
					"node2",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespace2.Name,
				)
				networkPolicy := newNetworkPolicy("networkpolicy1", namespace1.Name,
					metav1.LabelSelector{},
					[]knet.NetworkPolicyIngressRule{
						{
							From: []knet.NetworkPolicyPeer{
								{
									PodSelector: &metav1.LabelSelector{
										MatchLabels: map[string]string{
											"name": nPodTest.podName,
										},
									},
									NamespaceSelector: &metav1.LabelSelector{
										MatchLabels: map[string]string{
											"name": namespace2.Name,
										},
									},
								},
							},
						},
					},
					[]knet.NetworkPolicyEgressRule{
						{
							To: []knet.NetworkPolicyPeer{
								{
									PodSelector: &metav1.LabelSelector{
										MatchLabels: map[string]string{
											"name": nPodTest.podName,
										},
									},
									NamespaceSelector: &metav1.LabelSelector{
										MatchLabels: map[string]string{
											"name": namespace2.Name,
										},
									},
								},
							},
						},
					})

				npTest := kNetworkPolicy{}
				gressPolicyExpectedData := npTest.getPolicyData(networkPolicy, nil, []string{}, nil, nbdb.ACLSeverityInfo, false)
				defaultDenyExpectedData := npTest.getDefaultDenyData(networkPolicy, nil, nbdb.ACLSeverityInfo, false)
				expectedData := []libovsdb.TestData{}
				expectedData = append(expectedData, gressPolicyExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)

				fakeOvn.startWithDBSetup(initialDB,
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespace1,
							namespace2,
						},
					},
					&v1.PodList{
						Items: []v1.Pod{
							*newPod(nPodTest.namespace, nPodTest.podName, nPodTest.nodeName, nPodTest.podIP),
						},
					},
					&knet.NetworkPolicyList{
						Items: []knet.NetworkPolicy{
							*networkPolicy,
						},
					},
				)
				nPodTest.populateLogicalSwitchCache(fakeOvn, "")
				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.controller.WatchPods()
				fakeOvn.controller.WatchNetworkPolicy()

				fakeOvn.asf.ExpectEmptyAddressSet(namespaceName1)
				eventuallyExpectAddressSetsWithIP(fakeOvn, networkPolicy, nPodTest.podIP)
				fakeOvn.asf.ExpectAddressSetWithIPs(namespaceName2, []string{nPodTest.podIP})

				_, err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(append(expectedData, &nbdb.LogicalSwitch{
					UUID: "node1-UUID",
					Name: "node1",
				})...))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})

	ginkgo.Context("during execution", func() {

		ginkgo.It("correctly creates and deletes a networkpolicy allowing a port to a local pod", func() {
			app.Action = func(ctx *cli.Context) error {
				namespace1 := *newNamespace(namespaceName1)
				nPodTest := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespace1.Name,
				)
				nPod := newPod(nPodTest.namespace, nPodTest.podName, nPodTest.nodeName, nPodTest.podIP)

				const (
					labelName string = "pod-name"
					labelVal  string = "server"
					portNum   int32  = 81
				)
				nPod.Labels[labelName] = labelVal

				tcpProtocol := v1.Protocol(v1.ProtocolTCP)
				networkPolicy := newNetworkPolicy("networkpolicy1", namespace1.Name,
					metav1.LabelSelector{
						MatchLabels: map[string]string{
							labelName: labelVal,
						},
					},
					[]knet.NetworkPolicyIngressRule{{
						Ports: []knet.NetworkPolicyPort{{
							Port:     &intstr.IntOrString{IntVal: portNum},
							Protocol: &tcpProtocol,
						}},
					}},
					[]knet.NetworkPolicyEgressRule{{
						Ports: []knet.NetworkPolicyPort{{
							Port:     &intstr.IntOrString{IntVal: portNum},
							Protocol: &tcpProtocol,
						}},
					}},
				)

				// This is not yet going to be created
				networkPolicy2 := newNetworkPolicy("networkpolicy2", namespace1.Name,
					metav1.LabelSelector{
						MatchLabels: map[string]string{
							labelName: labelVal,
						},
					},
					[]knet.NetworkPolicyIngressRule{{
						Ports: []knet.NetworkPolicyPort{{
							Port:     &intstr.IntOrString{IntVal: portNum + 1},
							Protocol: &tcpProtocol,
						}},
					}},
					[]knet.NetworkPolicyEgressRule{{
						Ports: []knet.NetworkPolicyPort{{
							Port:     &intstr.IntOrString{IntVal: portNum + 1},
							Protocol: &tcpProtocol,
						}},
					}},
				)

				npTest := kNetworkPolicy{}
				gressPolicy1ExpectedData := npTest.getPolicyData(networkPolicy, []string{nPodTest.portUUID}, nil, []int32{portNum}, nbdb.ACLSeverityInfo, false)
				defaultDenyExpectedData := npTest.getDefaultDenyData(networkPolicy, []string{nPodTest.portUUID}, nbdb.ACLSeverityInfo, false)
				expectedData := []libovsdb.TestData{}
				expectedData = append(expectedData, gressPolicy1ExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)
				expectedData = append(expectedData, getExpectedDataPodsAndSwitches([]testPod{nPodTest}, []string{"node1"})...)

				fakeOvn.startWithDBSetup(initialDB,
					&v1.NamespaceList{
						Items: []v1.Namespace{namespace1},
					},
					&v1.PodList{
						Items: []v1.Pod{*nPod},
					},
					&knet.NetworkPolicyList{
						Items: []knet.NetworkPolicy{*networkPolicy},
					},
				)
				nPodTest.populateLogicalSwitchCache(fakeOvn, getLogicalSwitchUUID(fakeOvn.controller.nbClient, "node1"))
				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.controller.WatchPods()
				fakeOvn.controller.WatchNetworkPolicy()

				ginkgo.By("Creating a network policy that applies to a pod")

				_, err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.asf.ExpectAddressSetWithIPs(namespaceName1, []string{nPodTest.podIP})
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

				// Create a second NP
				ginkgo.By("Creating and deleting another policy that references that pod")

				_, err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).Create(context.TODO(), networkPolicy2, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gressPolicy2ExpectedData := npTest.getPolicyData(networkPolicy2, []string{nPodTest.portUUID}, nil, []int32{portNum + 1}, nbdb.ACLSeverityInfo, false)
				expectedDataWithPolicy2 := append(expectedData, gressPolicy2ExpectedData...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedDataWithPolicy2...))

				// Delete the second network policy
				err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy2.Namespace).Delete(context.TODO(), networkPolicy2.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// TODO: test server does not garbage collect ACLs, so we just expect policy2 portgroup to be removed
				expectedDataWithoutPolicy2 := append(expectedData, gressPolicy2ExpectedData[:len(gressPolicy2ExpectedData)-1]...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedDataWithoutPolicy2...))

				// Delete the first network policy
				ginkgo.By("Deleting that network policy")
				err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).Delete(context.TODO(), networkPolicy.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// TODO: test server does not garbage collect ACLs, so we just expect policy & deny portgroups to be removed
				acls := []libovsdb.TestData{}
				acls = append(acls, gressPolicy1ExpectedData[:len(gressPolicy1ExpectedData)-1]...)
				acls = append(acls, gressPolicy2ExpectedData[:len(gressPolicy2ExpectedData)-1]...)
				acls = append(acls, defaultDenyExpectedData[:len(defaultDenyExpectedData)-2]...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(append(acls, getExpectedDataPodsAndSwitches([]testPod{nPodTest}, []string{"node1"})...)))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("correctly retries creating a network policy allowing a port to a local pod", func() {
			app.Action = func(ctx *cli.Context) error {
				namespace1 := *newNamespace(namespaceName1)
				nPodTest := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespace1.Name,
				)
				nPod := newPod(nPodTest.namespace, nPodTest.podName, nPodTest.nodeName, nPodTest.podIP)

				const (
					labelName string = "pod-name"
					labelVal  string = "server"
					portNum   int32  = 81
				)
				nPod.Labels[labelName] = labelVal

				tcpProtocol := v1.Protocol(v1.ProtocolTCP)
				networkPolicy := newNetworkPolicy("networkpolicy1", namespace1.Name,
					metav1.LabelSelector{
						MatchLabels: map[string]string{
							labelName: labelVal,
						},
					},
					[]knet.NetworkPolicyIngressRule{{
						Ports: []knet.NetworkPolicyPort{{
							Port:     &intstr.IntOrString{IntVal: portNum},
							Protocol: &tcpProtocol,
						}},
					}},
					[]knet.NetworkPolicyEgressRule{{
						Ports: []knet.NetworkPolicyPort{{
							Port:     &intstr.IntOrString{IntVal: portNum},
							Protocol: &tcpProtocol,
						}},
					}},
				)

				// This is not yet going to be created
				networkPolicy2 := newNetworkPolicy("networkpolicy2", namespace1.Name,
					metav1.LabelSelector{
						MatchLabels: map[string]string{
							labelName: labelVal,
						},
					},
					[]knet.NetworkPolicyIngressRule{{
						Ports: []knet.NetworkPolicyPort{{
							Port:     &intstr.IntOrString{IntVal: portNum + 1},
							Protocol: &tcpProtocol,
						}},
					}},
					[]knet.NetworkPolicyEgressRule{{
						Ports: []knet.NetworkPolicyPort{{
							Port:     &intstr.IntOrString{IntVal: portNum + 1},
							Protocol: &tcpProtocol,
						}},
					}},
				)

				npTest := kNetworkPolicy{}
				gressPolicy1ExpectedData := npTest.getPolicyData(networkPolicy, []string{nPodTest.portUUID}, nil, []int32{portNum}, nbdb.ACLSeverityInfo, false)
				defaultDenyExpectedData := npTest.getDefaultDenyData(networkPolicy, []string{nPodTest.portUUID}, nbdb.ACLSeverityInfo, false)
				expectedData := []libovsdb.TestData{}
				expectedData = append(expectedData, gressPolicy1ExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)
				expectedData = append(expectedData, getExpectedDataPodsAndSwitches([]testPod{nPodTest}, []string{"node1"})...)

				fakeOvn.startWithDBSetup(initialDB,
					&v1.NamespaceList{
						Items: []v1.Namespace{namespace1},
					},
					&v1.PodList{
						Items: []v1.Pod{*nPod},
					},
					&knet.NetworkPolicyList{
						Items: []knet.NetworkPolicy{*networkPolicy},
					},
				)
				nPodTest.populateLogicalSwitchCache(fakeOvn, getLogicalSwitchUUID(fakeOvn.controller.nbClient, "node1"))
				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.controller.WatchPods()
				fakeOvn.controller.WatchNetworkPolicy()

				ginkgo.By("Creating a network policy that applies to a pod")

				_, err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.asf.ExpectAddressSetWithIPs(namespaceName1, []string{nPodTest.podIP})
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

				ginkgo.By("Bringing down NBDB")
				// inject transient problem, nbdb is down
				fakeOvn.controller.nbClient.Close()
				gomega.Eventually(func() bool {
					return fakeOvn.controller.nbClient.Connected()
				}).Should(gomega.BeFalse())

				// Create a second NP
				ginkgo.By("Creating and deleting another policy that references that pod")
				_, err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).Create(context.TODO(), networkPolicy2, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// sleep long enough for TransactWithRetry to fail, causing NP Add to fail
				time.Sleep(types.OVSDBTimeout + time.Second)
				// check to see if the retry cache has an entry for this policy
				key := getPolicyNamespacedName(networkPolicy2)
				fakeOvn.controller.retryNetworkPolicies.getObjRetryEntry(key)
				gomega.Eventually(func() *retryObjEntry {
					return fakeOvn.controller.retryNetworkPolicies.getObjRetryEntry(key)
				}).ShouldNot(gomega.BeNil())
				connCtx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
				defer cancel()
				resetNBClient(connCtx, fakeOvn.controller.nbClient)
				fakeOvn.controller.retryNetworkPolicies.requestRetryObjs()

				gressPolicy2ExpectedData := npTest.getPolicyData(networkPolicy2, []string{nPodTest.portUUID}, nil, []int32{portNum + 1}, nbdb.ACLSeverityInfo, false)
				expectedDataWithPolicy2 := append(expectedData, gressPolicy2ExpectedData...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedDataWithPolicy2...))
				// check the cache no longer has the entry
				gomega.Eventually(func() *retryObjEntry {
					return fakeOvn.controller.retryNetworkPolicies.getObjRetryEntry(key)
				}).Should(gomega.BeNil())

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("correctly retries recreating a network policy with the same name", func() {
			app.Action = func(ctx *cli.Context) error {
				namespace1 := *newNamespace(namespaceName1)
				nPodTest := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespace1.Name,
				)
				nPod := newPod(nPodTest.namespace, nPodTest.podName, nPodTest.nodeName, nPodTest.podIP)

				const (
					labelName string = "pod-name"
					labelVal  string = "server"
					portNum   int32  = 81
				)
				nPod.Labels[labelName] = labelVal

				tcpProtocol := v1.Protocol(v1.ProtocolTCP)
				networkPolicy := newNetworkPolicy("networkpolicy1", namespace1.Name,
					metav1.LabelSelector{
						MatchLabels: map[string]string{
							labelName: labelVal,
						},
					},
					[]knet.NetworkPolicyIngressRule{{
						Ports: []knet.NetworkPolicyPort{{
							Port:     &intstr.IntOrString{IntVal: portNum},
							Protocol: &tcpProtocol,
						}},
					}},
					[]knet.NetworkPolicyEgressRule{{
						Ports: []knet.NetworkPolicyPort{{
							Port:     &intstr.IntOrString{IntVal: portNum},
							Protocol: &tcpProtocol,
						}},
					}},
				)

				// This is not yet going to be created
				networkPolicy2 := newNetworkPolicy("networkpolicy1", namespace1.Name,
					metav1.LabelSelector{
						MatchLabels: map[string]string{
							labelName: labelVal,
						},
					},
					[]knet.NetworkPolicyIngressRule{{
						Ports: []knet.NetworkPolicyPort{{
							Port:     &intstr.IntOrString{IntVal: portNum + 1},
							Protocol: &tcpProtocol,
						}},
					}},
					[]knet.NetworkPolicyEgressRule{{
						Ports: []knet.NetworkPolicyPort{{
							Port:     &intstr.IntOrString{IntVal: portNum + 1},
							Protocol: &tcpProtocol,
						}},
					}},
				)

				npTest := kNetworkPolicy{}
				gressPolicy1ExpectedData := npTest.getPolicyData(networkPolicy, []string{nPodTest.portUUID}, nil, []int32{portNum}, nbdb.ACLSeverityInfo, false)
				defaultDenyExpectedData := npTest.getDefaultDenyData(networkPolicy, []string{nPodTest.portUUID}, nbdb.ACLSeverityInfo, false)
				expectedData := []libovsdb.TestData{}
				expectedData = append(expectedData, gressPolicy1ExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)
				expectedData = append(expectedData, getExpectedDataPodsAndSwitches([]testPod{nPodTest}, []string{"node1"})...)

				fakeOvn.startWithDBSetup(initialDB,
					&v1.NamespaceList{
						Items: []v1.Namespace{namespace1},
					},
					&v1.PodList{
						Items: []v1.Pod{*nPod},
					},
					&knet.NetworkPolicyList{
						Items: []knet.NetworkPolicy{*networkPolicy},
					},
				)
				nPodTest.populateLogicalSwitchCache(fakeOvn, getLogicalSwitchUUID(fakeOvn.controller.nbClient, "node1"))
				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.controller.WatchPods()
				fakeOvn.controller.WatchNetworkPolicy()

				ginkgo.By("Creating a network policy that applies to a pod")

				_, err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.asf.ExpectAddressSetWithIPs(namespaceName1, []string{nPodTest.podIP})
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

				ginkgo.By("Bringing down NBDB")
				// inject transient problem, nbdb is down
				fakeOvn.controller.nbClient.Close()
				gomega.Eventually(func() bool {
					return fakeOvn.controller.nbClient.Connected()
				}).Should(gomega.BeFalse())

				ginkgo.By("Delete the first network policy")
				err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).Delete(context.TODO(), networkPolicy.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// sleep long enough for TransactWithRetry to fail, causing NP Add to fail
				time.Sleep(types.OVSDBTimeout + time.Second)
				// check to see if the retry cache has an entry for this policy
				key := getPolicyNamespacedName(networkPolicy2)
				gomega.Eventually(func() *retryObjEntry {
					return fakeOvn.controller.retryNetworkPolicies.getObjRetryEntry(key)
				}).ShouldNot(gomega.BeNil())
				if retryEntry := fakeOvn.controller.retryNetworkPolicies.getObjRetryEntry(key); retryEntry != nil {
					ginkgo.By("retry entry new policy should be nil")
					gomega.Expect(retryEntry.newObj).To(gomega.BeNil())
					ginkgo.By("retry entry old policy should not be nil")
					gomega.Expect(retryEntry.oldObj).NotTo(gomega.BeNil())
				}
				connCtx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
				defer cancel()
				resetNBClient(connCtx, fakeOvn.controller.nbClient)

				ginkgo.By("Create a new network policy with same name")

				_, err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).Create(context.TODO(), networkPolicy2, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gressPolicy2ExpectedData := npTest.getPolicyData(networkPolicy2, []string{nPodTest.portUUID}, nil, []int32{portNum + 1}, nbdb.ACLSeverityInfo, false)
				defaultDenyExpectedData2 := npTest.getDefaultDenyData(networkPolicy2, []string{nPodTest.portUUID}, nbdb.ACLSeverityInfo, false)

				expectedData = []libovsdb.TestData{}
				// FIXME(trozet): libovsdb server doesn't remove referenced ACLs to PG when deleting the PG
				// https://github.com/ovn-org/libovsdb/issues/219
				expectedPolicy1ACLs := gressPolicy1ExpectedData[:len(gressPolicy1ExpectedData)-1]
				expectedData = append(expectedData, expectedPolicy1ACLs...)
				expectedData = append(expectedData, gressPolicy2ExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData2...)
				expectedData = append(expectedData, getExpectedDataPodsAndSwitches([]testPod{nPodTest}, []string{"node1"})...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))
				// check the cache no longer has the entry
				key = getPolicyNamespacedName(networkPolicy)
				gomega.Eventually(func() *retryObjEntry {
					return fakeOvn.controller.retryNetworkPolicies.getObjRetryEntry(key)
				}).Should(gomega.BeNil())

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("deleting a network policy that failed half-way through creation succeeds", func() {
			app.Action = func(ctx *cli.Context) error {
				namespace1 := *newNamespace(namespaceName1)
				nPodTest := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespace1.Name,
				)
				nPod := newPod(nPodTest.namespace, nPodTest.podName, nPodTest.nodeName, nPodTest.podIP)

				const (
					labelName string = "pod-name"
					labelVal  string = "server"
					portNum   int32  = 81
				)
				nPod.Labels[labelName] = labelVal

				tcpProtocol := v1.Protocol(v1.ProtocolTCP)
				networkPolicy := newNetworkPolicy("networkpolicy1", namespace1.Name,
					metav1.LabelSelector{
						MatchLabels: map[string]string{
							labelName: labelVal,
						},
					},
					[]knet.NetworkPolicyIngressRule{{
						Ports: []knet.NetworkPolicyPort{{
							Port:     &intstr.IntOrString{IntVal: portNum},
							Protocol: &tcpProtocol,
						}},
					}},
					[]knet.NetworkPolicyEgressRule{{
						Ports: []knet.NetworkPolicyPort{{
							Port:     &intstr.IntOrString{IntVal: portNum},
							Protocol: &tcpProtocol,
						}},
					}},
				)

				egressOptions := map[string]string{
					"apply-after-lb": "true",
				}
				pgHash := hashedPortGroup(networkPolicy.Namespace)
				leftOverACLFromUpgrade1 := libovsdbops.BuildACL(
					networkPolicy.Namespace+"_"+arpAllowPolicySuffix,
					nbdb.ACLDirectionFromLport,
					types.DefaultAllowPriority,
					"inport == @"+pgHash+"_"+egressDenyPG+" && (arp)", // invalid ACL match; won't be cleaned up
					nbdb.ACLActionAllow,
					types.OvnACLLoggingMeter,
					nbdb.ACLSeverityInfo,
					false,
					map[string]string{
						defaultDenyPolicyTypeACLExtIdKey: string(knet.PolicyTypeEgress),
					},
					egressOptions,
				)
				leftOverACLFromUpgrade1.UUID = *leftOverACLFromUpgrade1.Name + "-egressAllowACL-UUID1"

				leftOverACLFromUpgrade2 := libovsdbops.BuildACL(
					networkPolicy.Namespace+"_"+arpAllowPolicySuffix,
					nbdb.ACLDirectionFromLport,
					types.DefaultAllowPriority,
					"inport == @"+pgHash+"_"+egressDenyPG+" && (arp || nd)",
					nbdb.ACLActionAllow,
					types.OvnACLLoggingMeter,
					nbdb.ACLSeverityInfo,
					false,
					map[string]string{
						defaultDenyPolicyTypeACLExtIdKey: string(knet.PolicyTypeEgress),
					},
					egressOptions,
				)
				leftOverACLFromUpgrade2.UUID = *leftOverACLFromUpgrade2.Name + "-egressAllowACL-UUID2"

				initialDB1 := libovsdb.TestSetup{
					NBData: []libovsdb.TestData{
						&nbdb.LogicalSwitch{
							Name: "node1",
						},
						leftOverACLFromUpgrade1,
						leftOverACLFromUpgrade2,
					},
				}

				fakeOvn.startWithDBSetup(initialDB1,
					&v1.NamespaceList{
						Items: []v1.Namespace{namespace1},
					},
					&v1.PodList{
						Items: []v1.Pod{*nPod},
					},
				)
				nPodTest.populateLogicalSwitchCache(fakeOvn, getLogicalSwitchUUID(fakeOvn.controller.nbClient, "node1"))
				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.controller.WatchPods()
				fakeOvn.controller.WatchNetworkPolicy()

				ginkgo.By("Creating a network policy that applies to a pod and ensuring creation fails")

				err = fakeOvn.controller.addNetworkPolicy(networkPolicy)
				gomega.Expect(err).To(gomega.HaveOccurred())
				gomega.Expect(err.Error()).To(gomega.ContainSubstring("failed to create default port groups and acls for policy: namespace1/networkpolicy1, error: unexpectedly found multiple results for provided predicate"))

				//ensure the default PGs and ACLs were removed via rollback from add failure
				expectedData := []libovsdb.TestData{}
				expectedData = append(expectedData, getExpectedDataPodsAndSwitches([]testPod{nPodTest}, []string{"node1"})...)
				// note stale leftovers from previous upgrades won't be cleanedup
				expectedData = append(expectedData, leftOverACLFromUpgrade1, leftOverACLFromUpgrade2)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

				ginkgo.By("Deleting the network policy that failed to create and ensuring we don't panic")
				err = fakeOvn.controller.deleteNetworkPolicy(networkPolicy, nil)
				// I0623 policy.go:1285] Deleting network policy networkpolicy1 in namespace namespace1, np is nil: true
				// W0623 policy.go:1315] Unable to delete network policy: namespace1/networkpolicy1 since its not found in cache
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("older stale ACLs should be cleaned up or updated at startup via syncNetworkPolicies", func() {
			app.Action = func(ctx *cli.Context) error {
				namespace1 := *newNamespace(namespaceName1)
				nPodTest := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespace1.Name,
				)
				nPod := newPod(nPodTest.namespace, nPodTest.podName, nPodTest.nodeName, nPodTest.podIP)

				const (
					labelName string = "pod-name"
					labelVal  string = "server"
					portNum   int32  = 81
				)
				nPod.Labels[labelName] = labelVal

				tcpProtocol := v1.Protocol(v1.ProtocolTCP)
				networkPolicy1 := newNetworkPolicy("networkpolicy1", namespace1.Name,
					metav1.LabelSelector{
						MatchLabels: map[string]string{
							labelName: labelVal,
						},
					},
					[]knet.NetworkPolicyIngressRule{{
						Ports: []knet.NetworkPolicyPort{{
							Port:     &intstr.IntOrString{IntVal: portNum},
							Protocol: &tcpProtocol,
						}},
					}},
					[]knet.NetworkPolicyEgressRule{{
						Ports: []knet.NetworkPolicyPort{{
							Port:     &intstr.IntOrString{IntVal: portNum},
							Protocol: &tcpProtocol,
						}},
					}},
				)
				networkPolicy2 := newNetworkPolicy("networkpolicy2", namespace1.Name,
					metav1.LabelSelector{
						MatchLabels: map[string]string{
							labelName: labelVal,
						},
					},
					[]knet.NetworkPolicyIngressRule{{
						Ports: []knet.NetworkPolicyPort{{
							Port:     &intstr.IntOrString{IntVal: portNum + 1},
							Protocol: &tcpProtocol,
						}},
					}},
					[]knet.NetworkPolicyEgressRule{{
						Ports: []knet.NetworkPolicyPort{{
							Port:     &intstr.IntOrString{IntVal: portNum + 1},
							Protocol: &tcpProtocol,
						}},
					}},
				)

				egressOptions := map[string]string{
					// older versions of ACLs don't have, should be added by syncNetworkPolicies on startup
					//	"apply-after-lb": "true",
				}
				pgHash := hashedPortGroup("leftover1")
				// ACL1: leftover arp allow ACL egress with old match (arp)
				leftOverACL1FromUpgrade := libovsdbops.BuildACL(
					"leftover1"+"_"+arpAllowPolicySuffix,
					nbdb.ACLDirectionFromLport,
					types.DefaultAllowPriority,
					"inport == @"+pgHash+"_"+egressDenyPG+" && arp",
					nbdb.ACLActionAllow,
					types.OvnACLLoggingMeter,
					nbdb.ACLSeverityInfo,
					false,
					map[string]string{
						defaultDenyPolicyTypeACLExtIdKey: string(knet.PolicyTypeEgress),
					},
					egressOptions,
				)
				leftOverACL1FromUpgrade.UUID = *leftOverACL1FromUpgrade.Name + "-egressAllowACL-UUID"
				// ACL2: leftover arp allow ACL ingress with old match (arp)
				leftOverACL2FromUpgrade := libovsdbops.BuildACL(
					"leftover1"+"_"+arpAllowPolicySuffix,
					nbdb.ACLDirectionToLport,
					types.DefaultAllowPriority,
					"outport == @"+pgHash+"_"+ingressDenyPG+" && arp",
					nbdb.ACLActionAllow,
					types.OvnACLLoggingMeter,
					nbdb.ACLSeverityInfo,
					false,
					map[string]string{
						defaultDenyPolicyTypeACLExtIdKey: string(knet.PolicyTypeIngress),
					},
					nil,
				)
				leftOverACL2FromUpgrade.UUID = *leftOverACL2FromUpgrade.Name + "-ingressAllowACL-UUID"

				// ACL3: leftover default deny ACL egress with old name (namespace_policyname)
				leftOverACL3FromUpgrade := libovsdbops.BuildACL(
					"leftover1"+"_"+networkPolicy2.Name,
					nbdb.ACLDirectionFromLport,
					types.DefaultDenyPriority,
					"inport == @"+pgHash+"_"+egressDenyPG,
					nbdb.ACLActionDrop,
					types.OvnACLLoggingMeter,
					nbdb.ACLSeverityInfo,
					false,
					map[string]string{
						defaultDenyPolicyTypeACLExtIdKey: string(knet.PolicyTypeEgress),
					},
					egressOptions,
				)
				leftOverACL3FromUpgrade.UUID = *leftOverACL3FromUpgrade.Name + "-egressDenyACL-UUID"

				// ACL4: leftover default deny ACL ingress with old name (namespace_policyname)
				leftOverACL4FromUpgrade := libovsdbops.BuildACL(
					"leftover1"+"_"+networkPolicy2.Name,
					nbdb.ACLDirectionToLport,
					types.DefaultDenyPriority,
					"outport == @"+pgHash+"_"+ingressDenyPG,
					nbdb.ACLActionDrop,
					types.OvnACLLoggingMeter,
					nbdb.ACLSeverityInfo,
					false,
					map[string]string{
						defaultDenyPolicyTypeACLExtIdKey: string(knet.PolicyTypeIngress),
					},
					nil,
				)
				leftOverACL4FromUpgrade.UUID = *leftOverACL4FromUpgrade.Name + "-ingressDenyACL-UUID"

				initialDB1 := libovsdb.TestSetup{
					NBData: []libovsdb.TestData{
						&nbdb.LogicalSwitch{
							Name: "node1",
						},
						leftOverACL1FromUpgrade,
						leftOverACL2FromUpgrade,
						leftOverACL3FromUpgrade,
						leftOverACL4FromUpgrade,
					},
				}

				fakeOvn.startWithDBSetup(initialDB1,
					&v1.NamespaceList{
						Items: []v1.Namespace{namespace1},
					},
					&v1.PodList{
						Items: []v1.Pod{*nPod},
					},
					&knet.NetworkPolicyList{
						Items: []knet.NetworkPolicy{*networkPolicy1},
					},
				)
				nPodTest.populateLogicalSwitchCache(fakeOvn, getLogicalSwitchUUID(fakeOvn.controller.nbClient, "node1"))
				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.controller.WatchPods()
				fakeOvn.controller.WatchNetworkPolicy()

				_, err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy1.Namespace).Create(context.TODO(), networkPolicy2, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				npTest := kNetworkPolicy{}
				expectedData := []libovsdb.TestData{}
				expectedData = append(expectedData, getExpectedDataPodsAndSwitches([]testPod{nPodTest}, []string{"node1"})...)
				gressPolicy1ExpectedData := npTest.getPolicyData(networkPolicy1, []string{nPodTest.portUUID}, nil, []int32{portNum}, nbdb.ACLSeverityInfo, false)
				defaultDeny1ExpectedData := npTest.getDefaultDenyData(networkPolicy1, []string{nPodTest.portUUID}, nbdb.ACLSeverityInfo, false)
				expectedData = append(expectedData, gressPolicy1ExpectedData...)
				expectedData = append(expectedData, defaultDeny1ExpectedData...)
				gressPolicy2ExpectedData := npTest.getPolicyData(networkPolicy2, []string{nPodTest.portUUID}, nil, []int32{portNum + 1}, nbdb.ACLSeverityInfo, false)
				expectedData = append(expectedData, gressPolicy2ExpectedData...)
				egressOptions = map[string]string{
					"apply-after-lb": "true",
				}
				leftOverACL3FromUpgrade.Options = egressOptions
				newDefaultDenyEgressACLName := "leftover1_" + egressDefaultDenySuffix
				newDefaultDenyIngressACLName := "leftover1_" + ingressDefaultDenySuffix
				leftOverACL3FromUpgrade.Name = &newDefaultDenyEgressACLName
				leftOverACL4FromUpgrade.Name = &newDefaultDenyIngressACLName
				expectedData = append(expectedData, leftOverACL3FromUpgrade)
				expectedData = append(expectedData, leftOverACL4FromUpgrade)

				fakeOvn.asf.ExpectAddressSetWithIPs(namespaceName1, []string{nPodTest.podIP})
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("reconciles a deleted namespace referenced by a networkpolicy with a local running pod", func() {
			app.Action = func(ctx *cli.Context) error {
				namespace1 := *newNamespace(namespaceName1)
				namespace2 := *newNamespace(namespaceName2)

				nPodTest := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespace1.Name,
				)

				networkPolicy := newNetworkPolicy("networkpolicy1", namespace1.Name,
					metav1.LabelSelector{},
					[]knet.NetworkPolicyIngressRule{
						{
							From: []knet.NetworkPolicyPeer{
								{
									NamespaceSelector: &metav1.LabelSelector{
										MatchLabels: map[string]string{
											"name": namespace2.Name,
										},
									},
								},
							},
						},
					},
					[]knet.NetworkPolicyEgressRule{
						{
							To: []knet.NetworkPolicyPeer{
								{
									NamespaceSelector: &metav1.LabelSelector{
										MatchLabels: map[string]string{
											"name": namespace2.Name,
										},
									},
								},
							},
						},
					})

				npTest := kNetworkPolicy{}
				gressPolicyExpectedData := npTest.getPolicyData(networkPolicy, []string{nPodTest.portUUID}, []string{namespace2.Name}, nil, nbdb.ACLSeverityInfo, false)
				defaultDenyExpectedData := npTest.getDefaultDenyData(networkPolicy, []string{nPodTest.portUUID}, nbdb.ACLSeverityInfo, false)
				expectedData := []libovsdb.TestData{}
				expectedData = append(expectedData, gressPolicyExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)
				expectedData = append(expectedData, getExpectedDataPodsAndSwitches([]testPod{nPodTest}, []string{"node1"})...)

				fakeOvn.startWithDBSetup(initialDB,
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespace1,
							namespace2,
						},
					},
					&v1.PodList{
						Items: []v1.Pod{
							*newPod(nPodTest.namespace, nPodTest.podName, nPodTest.nodeName, nPodTest.podIP),
						},
					},
					&knet.NetworkPolicyList{
						Items: []knet.NetworkPolicy{
							*networkPolicy,
						},
					},
				)
				nPodTest.populateLogicalSwitchCache(fakeOvn, getLogicalSwitchUUID(fakeOvn.controller.nbClient, "node1"))
				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.controller.WatchPods()
				fakeOvn.controller.WatchNetworkPolicy()

				_, err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.asf.ExpectAddressSetWithIPs(namespaceName1, []string{nPodTest.podIP})
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

				expectedData = []libovsdb.TestData{}
				gressPolicyExpectedData = npTest.getPolicyData(networkPolicy, []string{nPodTest.portUUID}, []string{}, nil, nbdb.ACLSeverityInfo, false)
				expectedData = append(expectedData, gressPolicyExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)
				expectedData = append(expectedData, getExpectedDataPodsAndSwitches([]testPod{nPodTest}, []string{"node1"})...)

				err = fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Delete(context.TODO(), namespace2.Name, *metav1.NewDeleteOptions(0))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.asf.EventuallyExpectNoAddressSet(namespaceName2)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("reconciles a deleted namespace referenced by a networkpolicy", func() {
			app.Action = func(ctx *cli.Context) error {
				namespace1 := *newNamespace(namespaceName1)
				namespace2 := *newNamespace(namespaceName2)
				networkPolicy := newNetworkPolicy("networkpolicy1", namespace1.Name,
					metav1.LabelSelector{},
					[]knet.NetworkPolicyIngressRule{
						{
							From: []knet.NetworkPolicyPeer{
								{
									NamespaceSelector: &metav1.LabelSelector{
										MatchLabels: map[string]string{
											"name": namespace2.Name,
										},
									},
								},
							},
						},
					},
					[]knet.NetworkPolicyEgressRule{
						{
							To: []knet.NetworkPolicyPeer{
								{
									NamespaceSelector: &metav1.LabelSelector{
										MatchLabels: map[string]string{
											"name": namespace2.Name,
										},
									},
								},
							},
						},
					})

				npTest := kNetworkPolicy{}
				gressPolicyExpectedData := npTest.getPolicyData(networkPolicy, nil, []string{namespace2.Name}, nil, nbdb.ACLSeverityInfo, false)
				defaultDenyExpectedData := npTest.getDefaultDenyData(networkPolicy, nil, nbdb.ACLSeverityInfo, false)
				expectedData := []libovsdb.TestData{}
				expectedData = append(expectedData, gressPolicyExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)
				expectedData = append(expectedData, &nbdb.LogicalSwitch{
					UUID: "node1-UUID",
					Name: "node1",
				})

				fakeOvn.startWithDBSetup(initialDB,
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespace1,
							namespace2,
						},
					},
					&knet.NetworkPolicyList{
						Items: []knet.NetworkPolicy{
							*networkPolicy,
						},
					},
				)

				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.controller.WatchNetworkPolicy()

				_, err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

				expectedData = []libovsdb.TestData{}
				gressPolicyExpectedData = npTest.getPolicyData(networkPolicy, nil, []string{}, nil, nbdb.ACLSeverityInfo, false)
				expectedData = append(expectedData, gressPolicyExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)
				expectedData = append(expectedData, &nbdb.LogicalSwitch{
					UUID: "node1-UUID",
					Name: "node1",
				})

				err = fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Delete(context.TODO(), namespace2.Name, *metav1.NewDeleteOptions(0))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("reconciles a deleted pod referenced by a networkpolicy in its own namespace", func() {
			app.Action = func(ctx *cli.Context) error {
				namespace1 := *newNamespace(namespaceName1)

				nPodTest := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespace1.Name,
				)
				networkPolicy := newNetworkPolicy("networkpolicy1", namespace1.Name,
					metav1.LabelSelector{},
					[]knet.NetworkPolicyIngressRule{
						{
							From: []knet.NetworkPolicyPeer{
								{
									PodSelector: &metav1.LabelSelector{
										MatchLabels: map[string]string{
											"name": nPodTest.podName,
										},
									},
								},
							},
						},
					},
					[]knet.NetworkPolicyEgressRule{
						{
							To: []knet.NetworkPolicyPeer{
								{
									PodSelector: &metav1.LabelSelector{
										MatchLabels: map[string]string{
											"name": nPodTest.podName,
										},
									},
								},
							},
						},
					})

				npTest := kNetworkPolicy{}
				gressPolicyExpectedData := npTest.getPolicyData(networkPolicy, []string{nPodTest.portUUID}, []string{}, nil, nbdb.ACLSeverityInfo, false)
				defaultDenyExpectedData := npTest.getDefaultDenyData(networkPolicy, []string{nPodTest.portUUID}, nbdb.ACLSeverityInfo, false)
				expectedData := []libovsdb.TestData{}
				expectedData = append(expectedData, gressPolicyExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)
				expectedData = append(expectedData, getExpectedDataPodsAndSwitches([]testPod{nPodTest}, []string{"node1"})...)

				fakeOvn.startWithDBSetup(initialDB,
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespace1,
						},
					},
					&v1.PodList{
						Items: []v1.Pod{
							*newPod(nPodTest.namespace, nPodTest.podName, nPodTest.nodeName, nPodTest.podIP),
						},
					},
					&knet.NetworkPolicyList{
						Items: []knet.NetworkPolicy{
							*networkPolicy,
						},
					},
				)
				nPodTest.populateLogicalSwitchCache(fakeOvn, getLogicalSwitchUUID(fakeOvn.controller.nbClient, "node1"))
				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.controller.WatchPods()
				fakeOvn.controller.WatchNetworkPolicy()

				eventuallyExpectAddressSetsWithIP(fakeOvn, networkPolicy, nPodTest.podIP)
				fakeOvn.asf.ExpectAddressSetWithIPs(namespaceName1, []string{nPodTest.podIP})

				_, err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

				err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(nPodTest.namespace).Delete(context.TODO(), nPodTest.podName, *metav1.NewDeleteOptions(0))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				eventuallyExpectEmptyAddressSetsExist(fakeOvn, networkPolicy)
				fakeOvn.asf.EventuallyExpectEmptyAddressSetExist(namespaceName1)

				gressPolicyExpectedData = npTest.getPolicyData(networkPolicy, nil, []string{}, nil, nbdb.ACLSeverityInfo, false)
				defaultDenyExpectedData = npTest.getDefaultDenyData(networkPolicy, nil, nbdb.ACLSeverityInfo, false)
				expectedData = []libovsdb.TestData{}
				expectedData = append(expectedData, gressPolicyExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)
				expectedData = append(expectedData, &nbdb.LogicalSwitch{
					UUID: "node1-UUID",
					Name: "node1",
				})
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("reconciles a deleted pod referenced by a networkpolicy in another namespace", func() {
			app.Action = func(ctx *cli.Context) error {
				namespace1 := *newNamespace(namespaceName1)
				namespace2 := *newNamespace(namespaceName2)

				nPodTest := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespace2.Name,
				)
				networkPolicy := newNetworkPolicy("networkpolicy1", namespace1.Name,
					metav1.LabelSelector{},
					[]knet.NetworkPolicyIngressRule{
						{
							From: []knet.NetworkPolicyPeer{
								{
									PodSelector: &metav1.LabelSelector{
										MatchLabels: map[string]string{
											"name": nPodTest.podName,
										},
									},
									NamespaceSelector: &metav1.LabelSelector{
										MatchLabels: map[string]string{
											"name": nPodTest.namespace,
										},
									},
								},
							},
						},
					},
					[]knet.NetworkPolicyEgressRule{
						{
							To: []knet.NetworkPolicyPeer{
								{
									PodSelector: &metav1.LabelSelector{
										MatchLabels: map[string]string{
											"name": nPodTest.podName,
										},
									},
									NamespaceSelector: &metav1.LabelSelector{
										MatchLabels: map[string]string{
											"name": nPodTest.namespace,
										},
									},
								},
							},
						},
					})

				npTest := kNetworkPolicy{}
				gressPolicyExpectedData := npTest.getPolicyData(networkPolicy, nil, []string{}, nil, nbdb.ACLSeverityInfo, false)
				defaultDenyExpectedData := npTest.getDefaultDenyData(networkPolicy, nil, nbdb.ACLSeverityInfo, false)
				expectedData := []libovsdb.TestData{}
				expectedData = append(expectedData, gressPolicyExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)
				expectedData1 := append(expectedData, getExpectedDataPodsAndSwitches([]testPod{nPodTest}, []string{"node1"})...)

				fakeOvn.startWithDBSetup(initialDB,
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespace1,
							namespace2,
						},
					},
					&v1.PodList{
						Items: []v1.Pod{
							*newPod(nPodTest.namespace, nPodTest.podName, nPodTest.nodeName, nPodTest.podIP),
						},
					},
					&knet.NetworkPolicyList{
						Items: []knet.NetworkPolicy{
							*networkPolicy,
						},
					},
				)
				nPodTest.populateLogicalSwitchCache(fakeOvn, getLogicalSwitchUUID(fakeOvn.controller.nbClient, "node1"))
				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.controller.WatchPods()
				fakeOvn.controller.WatchNetworkPolicy()

				fakeOvn.asf.ExpectEmptyAddressSet(namespaceName1)
				eventuallyExpectAddressSetsWithIP(fakeOvn, networkPolicy, nPodTest.podIP)
				fakeOvn.asf.ExpectAddressSetWithIPs(namespaceName2, []string{nPodTest.podIP})

				_, err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData1...))

				err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(nPodTest.namespace).Delete(context.TODO(), nPodTest.podName, *metav1.NewDeleteOptions(0))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				expectedData2 := append(expectedData, getExpectedDataPodsAndSwitches([]testPod{}, []string{"node1"})...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData2...))

				// After deleting the pod all address sets should be empty
				eventuallyExpectEmptyAddressSetsExist(fakeOvn, networkPolicy)
				fakeOvn.asf.EventuallyExpectEmptyAddressSetExist(namespaceName1)

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
		ginkgo.It("reconciles an updated namespace label", func() {
			app.Action = func(ctx *cli.Context) error {
				namespace1 := *newNamespace(namespaceName1)
				namespace2 := *newNamespace(namespaceName2)

				nPodTest := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespace2.Name,
				)
				networkPolicy := newNetworkPolicy("networkpolicy1", namespace1.Name,
					metav1.LabelSelector{},
					[]knet.NetworkPolicyIngressRule{
						{
							From: []knet.NetworkPolicyPeer{
								{
									PodSelector: &metav1.LabelSelector{
										MatchLabels: map[string]string{
											"name": nPodTest.podName,
										},
									},
									NamespaceSelector: &metav1.LabelSelector{
										MatchLabels: map[string]string{
											"name": nPodTest.namespace,
										},
									},
								},
							},
						},
					},
					[]knet.NetworkPolicyEgressRule{
						{
							To: []knet.NetworkPolicyPeer{
								{
									PodSelector: &metav1.LabelSelector{
										MatchLabels: map[string]string{
											"name": nPodTest.podName,
										},
									},
									NamespaceSelector: &metav1.LabelSelector{
										MatchLabels: map[string]string{
											"name": nPodTest.namespace,
										},
									},
								},
							},
						},
					})

				npTest := kNetworkPolicy{}
				gressPolicyExpectedData := npTest.getPolicyData(networkPolicy, nil, []string{}, nil, nbdb.ACLSeverityInfo, false)
				defaultDenyExpectedData := npTest.getDefaultDenyData(networkPolicy, nil, nbdb.ACLSeverityInfo, false)
				expectedData := []libovsdb.TestData{}
				expectedData = append(expectedData, gressPolicyExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)
				expectedData = append(expectedData, getExpectedDataPodsAndSwitches([]testPod{nPodTest}, []string{"node1"})...)

				fakeOvn.startWithDBSetup(initialDB,
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespace1,
							namespace2,
						},
					},
					&v1.PodList{
						Items: []v1.Pod{
							*newPod(nPodTest.namespace, nPodTest.podName, nPodTest.nodeName, nPodTest.podIP),
						},
					},
					&knet.NetworkPolicyList{
						Items: []knet.NetworkPolicy{
							*networkPolicy,
						},
					},
				)
				nPodTest.populateLogicalSwitchCache(fakeOvn, getLogicalSwitchUUID(fakeOvn.controller.nbClient, "node1"))
				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.controller.WatchPods()
				fakeOvn.controller.WatchNetworkPolicy()

				fakeOvn.asf.ExpectEmptyAddressSet(namespaceName1)
				eventuallyExpectAddressSetsWithIP(fakeOvn, networkPolicy, nPodTest.podIP)
				fakeOvn.asf.ExpectAddressSetWithIPs(namespaceName2, []string{nPodTest.podIP})

				_, err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

				namespace2.ObjectMeta.Labels = map[string]string{"labels": "test"}
				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Update(context.TODO(), &namespace2, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

				// After updating the namespace all address sets should be empty
				eventuallyExpectEmptyAddressSetsExist(fakeOvn, networkPolicy)

				fakeOvn.asf.EventuallyExpectEmptyAddressSetExist(namespaceName1)

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("reconciles a deleted networkpolicy", func() {
			app.Action = func(ctx *cli.Context) error {
				namespace1 := *newNamespace(namespaceName1)

				nPodTest := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespace1.Name,
				)
				networkPolicy := newNetworkPolicy("networkpolicy1", namespace1.Name,
					metav1.LabelSelector{},
					[]knet.NetworkPolicyIngressRule{
						{
							From: []knet.NetworkPolicyPeer{
								{
									PodSelector: &metav1.LabelSelector{
										MatchLabels: map[string]string{
											"name": nPodTest.podName,
										},
									},
								},
							},
						},
					},
					[]knet.NetworkPolicyEgressRule{
						{
							To: []knet.NetworkPolicyPeer{
								{
									PodSelector: &metav1.LabelSelector{
										MatchLabels: map[string]string{
											"name": nPodTest.podName,
										},
									},
								},
							},
						},
					})

				npTest := kNetworkPolicy{}
				gressPolicyExpectedData := npTest.getPolicyData(networkPolicy, []string{nPodTest.portUUID}, []string{}, nil, nbdb.ACLSeverityInfo, false)
				defaultDenyExpectedData := npTest.getDefaultDenyData(networkPolicy, []string{nPodTest.portUUID}, nbdb.ACLSeverityInfo, false)
				expectedData := []libovsdb.TestData{}
				expectedData = append(expectedData, gressPolicyExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)
				expectedData = append(expectedData, getExpectedDataPodsAndSwitches([]testPod{nPodTest}, []string{"node1"})...)

				fakeOvn.startWithDBSetup(initialDB,
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespace1,
						},
					},
					&v1.PodList{
						Items: []v1.Pod{
							*newPod(nPodTest.namespace, nPodTest.podName, nPodTest.nodeName, nPodTest.podIP),
						},
					},
					&knet.NetworkPolicyList{
						Items: []knet.NetworkPolicy{
							*networkPolicy,
						},
					},
				)
				nPodTest.populateLogicalSwitchCache(fakeOvn, getLogicalSwitchUUID(fakeOvn.controller.nbClient, "node1"))
				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.controller.WatchPods()
				fakeOvn.controller.WatchNetworkPolicy()

				_, err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))
				fakeOvn.asf.ExpectAddressSetWithIPs(namespaceName1, []string{nPodTest.podIP})

				err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).Delete(context.TODO(), networkPolicy.Name, *metav1.NewDeleteOptions(0))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				eventuallyExpectNoAddressSets(fakeOvn, networkPolicy)

				acls := []libovsdb.TestData{}
				acls = append(acls, gressPolicyExpectedData[:len(gressPolicyExpectedData)-1]...)
				acls = append(acls, defaultDenyExpectedData[:len(defaultDenyExpectedData)-2]...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(append(acls, getExpectedDataPodsAndSwitches([]testPod{nPodTest}, []string{"node1"})...)))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("retries a deleted network policy", func() {
			app.Action = func(ctx *cli.Context) error {
				namespace1 := *newNamespace(namespaceName1)

				nPodTest := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespace1.Name,
				)
				networkPolicy := newNetworkPolicy("networkpolicy1", namespace1.Name,
					metav1.LabelSelector{},
					[]knet.NetworkPolicyIngressRule{
						{
							From: []knet.NetworkPolicyPeer{
								{
									PodSelector: &metav1.LabelSelector{
										MatchLabels: map[string]string{
											"name": nPodTest.podName,
										},
									},
								},
							},
						},
					},
					[]knet.NetworkPolicyEgressRule{
						{
							To: []knet.NetworkPolicyPeer{
								{
									PodSelector: &metav1.LabelSelector{
										MatchLabels: map[string]string{
											"name": nPodTest.podName,
										},
									},
								},
							},
						},
					})

				npTest := kNetworkPolicy{}
				gressPolicyExpectedData := npTest.getPolicyData(networkPolicy, []string{nPodTest.portUUID}, []string{}, nil, nbdb.ACLSeverityInfo, false)
				defaultDenyExpectedData := npTest.getDefaultDenyData(networkPolicy, []string{nPodTest.portUUID}, nbdb.ACLSeverityInfo, false)
				expectedData := []libovsdb.TestData{}
				expectedData = append(expectedData, gressPolicyExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)
				expectedData = append(expectedData, getExpectedDataPodsAndSwitches([]testPod{nPodTest}, []string{"node1"})...)

				fakeOvn.startWithDBSetup(initialDB,
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespace1,
						},
					},
					&v1.PodList{
						Items: []v1.Pod{
							*newPod(nPodTest.namespace, nPodTest.podName, nPodTest.nodeName, nPodTest.podIP),
						},
					},
					&knet.NetworkPolicyList{
						Items: []knet.NetworkPolicy{
							*networkPolicy,
						},
					},
				)
				gomega.Eventually(func() bool {
					return fakeOvn.controller.nbClient.Connected()
				}).Should(gomega.BeTrue())

				nPodTest.populateLogicalSwitchCache(fakeOvn, getLogicalSwitchUUID(fakeOvn.controller.nbClient, "node1"))
				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.controller.WatchPods()
				fakeOvn.controller.WatchNetworkPolicy()

				_, err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))
				fakeOvn.asf.ExpectAddressSetWithIPs(namespaceName1, []string{nPodTest.podIP})

				// inject transient problem, nbdb is down
				fakeOvn.controller.nbClient.Close()
				gomega.Eventually(func() bool {
					return fakeOvn.controller.nbClient.Connected()
				}).Should(gomega.BeFalse())
				err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).Delete(context.TODO(), networkPolicy.Name, *metav1.NewDeleteOptions(0))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// sleep long enough for TransactWithRetry to fail, causing NP Add to fail
				time.Sleep(types.OVSDBTimeout + time.Second)
				// check to see if the retry cache has an entry for this policy
				key := getPolicyNamespacedName(networkPolicy)
				gomega.Eventually(func() *retryObjEntry {
					return fakeOvn.controller.retryNetworkPolicies.getObjRetryEntry(key)
				}).ShouldNot(gomega.BeNil())
				connCtx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
				defer cancel()
				resetNBClient(connCtx, fakeOvn.controller.nbClient)
				fakeOvn.controller.retryNetworkPolicies.requestRetryObjs()

				eventuallyExpectNoAddressSets(fakeOvn, networkPolicy)

				acls := []libovsdb.TestData{}
				acls = append(acls, gressPolicyExpectedData[:len(gressPolicyExpectedData)-1]...)
				acls = append(acls, defaultDenyExpectedData[:len(defaultDenyExpectedData)-2]...)
				gomega.Eventually(fakeOvn.controller.nbClient).Should(libovsdb.HaveData(append(acls, getExpectedDataPodsAndSwitches([]testPod{nPodTest}, []string{"node1"})...)))

				// check the cache no longer has the entry
				gomega.Eventually(func() *retryObjEntry {
					return fakeOvn.controller.retryNetworkPolicies.getObjRetryEntry(key)
				}).Should(gomega.BeNil())
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.Context("ACL logging for network policies", func() {
			const (
				firstNetworkPolicyName  = "networkpolicy1"
				logSeverityAnnotation   = "k8s.ovn.org/acl-logging"
				namespaceName           = "namespace1"
				secondNetworkPolicyName = "networkpolicy2"
				targetNamespaceName     = "target-namespace"
				thirdNetworkPolicyName  = "networkpolicy3"
			)

			var originalNamespace v1.Namespace

			setInitialOVNState := func(namespace v1.Namespace, initialNetworkPolicies ...knet.NetworkPolicy) {
				fakeOvn.start(
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespace,
						},
					},
					&v1.PodList{},
					&knet.NetworkPolicyList{
						Items: initialNetworkPolicies,
					},
				)
				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.controller.WatchNetworkPolicy()
			}

			newAnnotatedNamespace := func(name string, annotations map[string]string) *v1.Namespace {
				createdNamespace := newNamespace(namespaceName)
				createdNamespace.Annotations = annotations
				return createdNamespace
			}

			generateIngressEmptyPolicy := func() knet.NetworkPolicy {
				return *newNetworkPolicy(
					"emptyPol",
					namespaceName,
					metav1.LabelSelector{},
					nil, nil)
			}

			generateIngressPolicyWithSingleRule := func() knet.NetworkPolicy {
				return *newNetworkPolicy(
					firstNetworkPolicyName,
					namespaceName,
					metav1.LabelSelector{},
					[]knet.NetworkPolicyIngressRule{
						{
							From: []knet.NetworkPolicyPeer{
								{
									NamespaceSelector: &metav1.LabelSelector{
										MatchLabels: map[string]string{
											"name": targetNamespaceName,
										},
									},
								},
							},
						},
					}, nil)
			}

			generateIngressPolicyWithMultipleRules := func() knet.NetworkPolicy {
				return *newNetworkPolicy(
					thirdNetworkPolicyName,
					namespaceName,
					metav1.LabelSelector{},
					[]knet.NetworkPolicyIngressRule{
						{
							From: []knet.NetworkPolicyPeer{
								{
									NamespaceSelector: &metav1.LabelSelector{
										MatchLabels: map[string]string{
											"name": targetNamespaceName,
										},
									},
								},
							},
						},
						{
							From: []knet.NetworkPolicyPeer{
								{
									PodSelector: &metav1.LabelSelector{
										MatchLabels: map[string]string{
											"name": "tiny-winy-pod",
										},
									},
								},
							},
						},
					}, nil)
			}

			generateEgressPolicyWithSingleRule := func() knet.NetworkPolicy {
				return *newNetworkPolicy(
					secondNetworkPolicyName,
					namespaceName,
					metav1.LabelSelector{},
					nil,
					[]knet.NetworkPolicyEgressRule{
						{
							To: []knet.NetworkPolicyPeer{
								{
									NamespaceSelector: &metav1.LabelSelector{
										MatchLabels: map[string]string{
											"name": targetNamespaceName,
										},
									},
								},
							},
						},
					})
			}

			updateNamespaceACLLogSeverity := func(namespaceToUpdate *v1.Namespace, desiredDenyLogLevel string, desiredAllowLogLevel string) error {
				ginkgo.By("updating the namespace's ACL logging severity")
				updatedLogSeverity := fmt.Sprintf(`{ "deny": "%s", "allow": "%s" }`, desiredDenyLogLevel, desiredAllowLogLevel)
				namespaceToUpdate.Annotations[logSeverityAnnotation] = updatedLogSeverity

				_, err := fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Update(context.TODO(), namespaceToUpdate, metav1.UpdateOptions{})
				return err
			}

			provisionNetworkPolicy := func(namespaceName string, policy knet.NetworkPolicy) error {
				ginkgo.By("Provisioning a new network policy")
				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(namespaceName).Create(context.TODO(), &policy, metav1.CreateOptions{})
				return err
			}

			ginkgo.BeforeEach(func() {
				originalACLLogSeverity := fmt.Sprintf(`{ "deny": "%s", "allow": "%s" }`, nbdb.ACLSeverityAlert, nbdb.ACLSeverityNotice)
				originalNamespace = *newAnnotatedNamespace(
					namespaceName,
					map[string]string{logSeverityAnnotation: originalACLLogSeverity})
			})

			table.DescribeTable("ACL logging for network policies reacts to severity updates", func(networkPolicies ...knet.NetworkPolicy) {
				npTest := kNetworkPolicy{}

				ginkgo.By("Provisioning the system with an initial empty policy, we know deterministically the names of the default deny ACLs")
				initialDenyAllPolicy := generateIngressEmptyPolicy()
				initialExpectedData := npTest.generateExpectedACLDataForFirstPolicyOnNamespace(initialDenyAllPolicy, nbdb.ACLSeverityAlert)

				app.Action = func(ctx *cli.Context) error {
					setInitialOVNState(originalNamespace, initialDenyAllPolicy)
					gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(initialExpectedData...))

					var provisionedPolicies []libovsdb.TestData
					for i := range networkPolicies {
						provisionedPolicies = append(
							provisionedPolicies,
							npTest.getPolicyData(&networkPolicies[i], nil, []string{}, nil, nbdb.ACLSeverityNotice, false)...)
						gomega.Expect(provisionNetworkPolicy(networkPolicies[i].GetNamespace(), networkPolicies[i])).To(gomega.Succeed())
					}
					gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(append(initialExpectedData, provisionedPolicies...)))

					desiredLogSeverity := nbdb.ACLSeverityDebug
					gomega.Expect(
						updateNamespaceACLLogSeverity(&originalNamespace, desiredLogSeverity, desiredLogSeverity)).To(gomega.Succeed(),
						"should have managed to update the ACL logging severity within the namespace")

					expectedDataAfterLoggingSeverityUpdate := npTest.generateExpectedACLDataForFirstPolicyOnNamespace(
						initialDenyAllPolicy, desiredLogSeverity)
					for i := range networkPolicies {
						expectedDataAfterLoggingSeverityUpdate = append(
							expectedDataAfterLoggingSeverityUpdate,
							npTest.getPolicyData(&networkPolicies[i], nil, []string{}, nil, desiredLogSeverity, false)...)
					}

					gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedDataAfterLoggingSeverityUpdate...))
					return nil
				}
				gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
			},
				table.Entry("when the namespace features a network policy with a single rule",
					generateIngressPolicyWithSingleRule()),
				table.Entry("when the namespace features *multiple* network policies with a single rule",
					generateIngressPolicyWithSingleRule(),
					generateEgressPolicyWithSingleRule()),
				table.Entry("when the namespace features a network policy with *multiple* rules",
					generateIngressPolicyWithMultipleRules()))

			ginkgo.It("policies created after namespace logging level updates inherit updated logging level", func() {
				app.Action = func(ctx *cli.Context) error {
					setInitialOVNState(originalNamespace)
					desiredLogSeverity := nbdb.ACLSeverityDebug
					gomega.Expect(
						updateNamespaceACLLogSeverity(&originalNamespace, desiredLogSeverity, desiredLogSeverity)).To(gomega.Succeed(),
						"should have managed to update the ACL logging severity within the namespace")

					newPolicy := generateIngressPolicyWithSingleRule()
					gomega.Expect(provisionNetworkPolicy(namespaceName, newPolicy)).To(gomega.Succeed(), "should have managed to create a new network policy")

					npTest := kNetworkPolicy{}
					var expectedData []libovsdb.TestData
					expectedData = append(expectedData, npTest.getPolicyData(&newPolicy, nil, []string{}, nil, desiredLogSeverity, false)...)
					expectedData = append(expectedData, npTest.getDefaultDenyData(&newPolicy, nil, desiredLogSeverity, false)...)

					gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))
					return nil
				}
				gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
			})
		})

	})
})

func asMatch(addressSets []string) string {
	hashedNames := make([]string, 0, len(addressSets))
	for _, as := range addressSets {
		v4HashedName, _ := addressset.MakeAddressSetHashNames(as)
		hashedNames = append(hashedNames, v4HashedName)
	}
	sort.Strings(hashedNames)
	var match string
	for i, n := range hashedNames {
		if i > 0 {
			match += ", "
		}
		match += fmt.Sprintf("$%s", n)
	}
	return match
}

func buildExpectedACL(gp *gressPolicy, pgName string, as []string) *nbdb.ACL {
	name := gp.policyNamespace + "_" + gp.policyName + "_" + strconv.Itoa(gp.idx)
	asMatch := asMatch(as)
	match := fmt.Sprintf("ip4.src == {%s} && outport == @%s", asMatch, pgName)
	gpDirection := string(knet.PolicyTypeIngress)
	externalIds := map[string]string{
		l4MatchACLExtIdKey:     "None",
		ipBlockCIDRACLExtIdKey: "false",
		namespaceACLExtIdKey:   gp.policyNamespace,
		policyACLExtIdKey:      gp.policyName,
		policyTypeACLExtIdKey:  gpDirection,
		gpDirection + "_num":   fmt.Sprintf("%d", gp.idx),
	}
	acl := libovsdbops.BuildACL(name, nbdb.ACLDirectionToLport, types.DefaultAllowPriority, match, nbdb.ACLActionAllowRelated, types.OvnACLLoggingMeter, nbdb.ACLSeverityInfo, true, externalIds, nil)
	return acl
}

var _ = ginkgo.Describe("OVN NetworkPolicy Low-Level Operations", func() {
	var (
		asFactory *addressset.FakeAddressSetFactory
		nbCleanup *libovsdbtest.Cleanup
	)

	const (
		nodeName   = "node1"
		ipv4MgmtIP = "192.168.10.10"
		ipv6MgmtIP = "fd01::1234"
	)

	ginkgo.BeforeEach(func() {
		nbCleanup = nil
	})

	ginkgo.AfterEach(func() {
		if nbCleanup != nil {
			nbCleanup.Cleanup()
		}
	})

	ginkgo.It("computes match strings from address sets correctly", func() {
		const (
			pgName string = "pg-name"
		)
		// Restore global default values before each testcase
		config.PrepareTestConfig()

		asFactory = addressset.NewFakeAddressSetFactory()
		config.IPv4Mode = true
		config.IPv6Mode = false

		policy := &knet.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{
				UID:       apimachinerytypes.UID("testing"),
				Name:      "policy",
				Namespace: "testing",
			},
		}

		gp := newGressPolicy(knet.PolicyTypeIngress, 0, policy.Namespace, policy.Name)
		err := gp.ensurePeerAddressSet(asFactory)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		asName := gp.peerAddressSet.GetName()

		one := "testing.policy.ingress.1"
		two := "testing.policy.ingress.2"
		three := "testing.policy.ingress.3"
		four := "testing.policy.ingress.4"
		five := "testing.policy.ingress.5"
		six := "testing.policy.ingress.6"

		gomega.Expect(gp.addNamespaceAddressSet(one)).To(gomega.BeTrue())
		expected := buildExpectedACL(gp, pgName, []string{asName, one})
		actual := gp.buildLocalPodACLs(pgName, defaultACLLoggingSeverity)
		gomega.Expect(actual).To(libovsdb.ConsistOfIgnoringUUIDs(expected))

		gomega.Expect(gp.addNamespaceAddressSet(two)).To(gomega.BeTrue())
		expected = buildExpectedACL(gp, pgName, []string{asName, one, two})
		actual = gp.buildLocalPodACLs(pgName, defaultACLLoggingSeverity)
		gomega.Expect(actual).To(libovsdb.ConsistOfIgnoringUUIDs(expected))

		// address sets should be alphabetized
		gomega.Expect(gp.addNamespaceAddressSet(three)).To(gomega.BeTrue())
		expected = buildExpectedACL(gp, pgName, []string{asName, one, two, three})
		actual = gp.buildLocalPodACLs(pgName, defaultACLLoggingSeverity)
		gomega.Expect(actual).To(libovsdb.ConsistOfIgnoringUUIDs(expected))

		// re-adding an existing set is a no-op
		gomega.Expect(gp.addNamespaceAddressSet(three)).To(gomega.BeFalse())

		gomega.Expect(gp.addNamespaceAddressSet(four)).To(gomega.BeTrue())
		expected = buildExpectedACL(gp, pgName, []string{asName, one, two, three, four})
		actual = gp.buildLocalPodACLs(pgName, defaultACLLoggingSeverity)
		gomega.Expect(actual).To(libovsdb.ConsistOfIgnoringUUIDs(expected))

		// now delete a set
		gomega.Expect(gp.delNamespaceAddressSet(one)).To(gomega.BeTrue())
		expected = buildExpectedACL(gp, pgName, []string{asName, two, three, four})
		actual = gp.buildLocalPodACLs(pgName, defaultACLLoggingSeverity)
		gomega.Expect(actual).To(libovsdb.ConsistOfIgnoringUUIDs(expected))

		// deleting again is a no-op
		gomega.Expect(gp.delNamespaceAddressSet(one)).To(gomega.BeFalse())

		// add and delete some more...
		gomega.Expect(gp.addNamespaceAddressSet(five)).To(gomega.BeTrue())
		expected = buildExpectedACL(gp, pgName, []string{asName, two, three, four, five})
		actual = gp.buildLocalPodACLs(pgName, defaultACLLoggingSeverity)
		gomega.Expect(actual).To(libovsdb.ConsistOfIgnoringUUIDs(expected))

		gomega.Expect(gp.delNamespaceAddressSet(three)).To(gomega.BeTrue())
		expected = buildExpectedACL(gp, pgName, []string{asName, two, four, five})
		actual = gp.buildLocalPodACLs(pgName, defaultACLLoggingSeverity)
		gomega.Expect(actual).To(libovsdb.ConsistOfIgnoringUUIDs(expected))

		// deleting again is no-op
		gomega.Expect(gp.delNamespaceAddressSet(one)).To(gomega.BeFalse())

		gomega.Expect(gp.addNamespaceAddressSet(six)).To(gomega.BeTrue())
		expected = buildExpectedACL(gp, pgName, []string{asName, two, four, five, six})
		actual = gp.buildLocalPodACLs(pgName, defaultACLLoggingSeverity)
		gomega.Expect(actual).To(libovsdb.ConsistOfIgnoringUUIDs(expected))

		gomega.Expect(gp.delNamespaceAddressSet(two)).To(gomega.BeTrue())
		expected = buildExpectedACL(gp, pgName, []string{asName, four, five, six})
		actual = gp.buildLocalPodACLs(pgName, defaultACLLoggingSeverity)
		gomega.Expect(actual).To(libovsdb.ConsistOfIgnoringUUIDs(expected))

		gomega.Expect(gp.delNamespaceAddressSet(five)).To(gomega.BeTrue())
		expected = buildExpectedACL(gp, pgName, []string{asName, four, six})
		actual = gp.buildLocalPodACLs(pgName, defaultACLLoggingSeverity)
		gomega.Expect(actual).To(libovsdb.ConsistOfIgnoringUUIDs(expected))

		gomega.Expect(gp.delNamespaceAddressSet(six)).To(gomega.BeTrue())
		expected = buildExpectedACL(gp, pgName, []string{asName, four})
		actual = gp.buildLocalPodACLs(pgName, defaultACLLoggingSeverity)
		gomega.Expect(actual).To(libovsdb.ConsistOfIgnoringUUIDs(expected))

		gomega.Expect(gp.delNamespaceAddressSet(four)).To(gomega.BeTrue())
		expected = buildExpectedACL(gp, pgName, []string{asName})
		actual = gp.buildLocalPodACLs(pgName, defaultACLLoggingSeverity)
		gomega.Expect(actual).To(libovsdb.ConsistOfIgnoringUUIDs(expected))

		// deleting again is no-op
		gomega.Expect(gp.delNamespaceAddressSet(four)).To(gomega.BeFalse())
	})

	ginkgo.It("Tests AddAllowACLFromNode", func() {
		ginkgo.By("adding an existing ACL to the node switch", func() {
			initialNbdb := libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					&nbdb.LogicalSwitch{
						Name: nodeName,
					},
					&nbdb.ACL{
						Match:     "ip4.src==" + ipv4MgmtIP,
						Priority:  types.DefaultAllowPriority,
						Action:    "allow-related",
						Direction: types.DirectionToLPort,
					},
				},
			}

			var err error
			var nbClient libovsdbclient.Client
			nbClient, nbCleanup, err = libovsdbtest.NewNBTestHarness(initialNbdb, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			err = addAllowACLFromNode(nodeName, ovntest.MustParseIP(ipv4MgmtIP), nbClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			testNode, nodeACL := generateAllowFromNodeData(nodeName, ipv4MgmtIP)
			expectedData := []libovsdb.TestData{
				testNode,
				nodeACL,
			}
			gomega.Expect(nbClient).Should(libovsdb.HaveData(expectedData...))
		})

		ginkgo.By("creating an ipv4 ACL and adding it to node switch", func() {
			initialNbdb := libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					&nbdb.LogicalSwitch{
						Name: nodeName,
					},
				},
			}

			var err error
			var nbClient libovsdbclient.Client
			nbClient, nbCleanup, err = libovsdbtest.NewNBTestHarness(initialNbdb, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			err = addAllowACLFromNode(nodeName, ovntest.MustParseIP(ipv4MgmtIP), nbClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			testNode, nodeACL := generateAllowFromNodeData(nodeName, ipv4MgmtIP)
			expectedData := []libovsdb.TestData{
				testNode,
				nodeACL,
			}
			gomega.Expect(nbClient).Should(libovsdb.HaveData(expectedData...))
		})
		ginkgo.By("creating an ipv6 ACL and adding it to node switch", func() {
			initialNbdb := libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					&nbdb.LogicalSwitch{
						Name: nodeName,
					},
				},
			}

			var err error
			var nbClient libovsdbclient.Client
			nbClient, nbCleanup, err = libovsdbtest.NewNBTestHarness(initialNbdb, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			err = addAllowACLFromNode(nodeName, ovntest.MustParseIP(ipv6MgmtIP), nbClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			testNode, nodeACL := generateAllowFromNodeData(nodeName, ipv6MgmtIP)
			expectedData := []libovsdb.TestData{
				testNode,
				nodeACL,
			}
			gomega.Expect(nbClient).Should(libovsdb.HaveData(expectedData...))
		})
	})
})

func generateAllowFromNodeData(nodeName, mgmtIP string) (nodeSwitch *nbdb.LogicalSwitch, acl *nbdb.ACL) {
	var ipFamily = "ip4"
	if utilnet.IsIPv6(ovntest.MustParseIP(mgmtIP)) {
		ipFamily = "ip6"
	}

	match := fmt.Sprintf("%s.src==%s", ipFamily, mgmtIP)

	nodeACL := libovsdbops.BuildACL("", types.DirectionToLPort, types.DefaultAllowPriority, match, "allow-related", "", "", false, nil, nil)
	nodeACL.UUID = "nodeACL-UUID"

	testNode := &nbdb.LogicalSwitch{
		UUID: nodeName + "-UUID",
		Name: nodeName,
		ACLs: []string{nodeACL.UUID},
	}

	return testNode, nodeACL
}
