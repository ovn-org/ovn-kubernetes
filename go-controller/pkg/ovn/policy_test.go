package ovn

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/onsi/ginkgo"
	"github.com/onsi/gomega"
	"github.com/onsi/gomega/format"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/libovsdbops"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"

	//"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/urfave/cli/v2"

	v1 "k8s.io/api/core/v1"
	knet "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apimachinerytypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
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

func (n kNetworkPolicy) baseCmds(fexec *ovntest.FakeExec, networkPolicy *knet.NetworkPolicy) string {
	readableGroupName := fmt.Sprintf("%s_%s", networkPolicy.Namespace, networkPolicy.Name)
	return readableGroupName
}

const (
	ingressDenyPG string = "ingressDefaultDeny"
	egressDenyPG  string = "egressDefaultDeny"
)

func (n kNetworkPolicy) addDefaultDenyPGCmds(fexec *ovntest.FakeExec, networkPolicy *knet.NetworkPolicy) {
	pg_hash := hashedPortGroup(networkPolicy.Namespace)
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find ACL match=\"outport == @" + pg_hash + "_" + ingressDenyPG + "\" action=drop external-ids:default-deny-policy-type=Ingress",
		Output: fakeUUID,
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find ACL match=\"outport == @" + pg_hash + "_" + ingressDenyPG + " && arp\" action=allow external-ids:default-deny-policy-type=Ingress",
		Output: fakeUUID,
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find ACL match=\"inport == @" + pg_hash + "_" + egressDenyPG + "\" action=drop external-ids:default-deny-policy-type=Egress",
		Output: fakeUUID,
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find ACL match=\"inport == @" + pg_hash + "_" + egressDenyPG + " && arp\" action=allow external-ids:default-deny-policy-type=Egress",
		Output: fakeUUID,
	})
}

func (n kNetworkPolicy) getDefaultDenyData(networkPolicy *knet.NetworkPolicy, ports []string) []libovsdb.TestData {
	pgHash := hashedPortGroup(networkPolicy.Namespace)
	return []libovsdb.TestData{
		&nbdb.ACL{
			UUID:      "named_acl_" + networkPolicy.Namespace + "_" + networkPolicy.Name + "_Egress",
			Name:      []string{networkPolicy.Namespace + "_" + networkPolicy.Name},
			Priority:  1000, // types.DefaultDenyPriority
			Direction: nbdb.ACLDirectionToLport,
			Match:     "inport == @" + pgHash + "_" + egressDenyPG,
			Action:    nbdb.ACLActionDrop,
			Log:       false,
			Meter:     []string{"acl-logging"},
			Severity:  []string{nbdb.ACLSeverityInfo},
			ExternalIDs: map[string]string{
				"default-deny-policy-type": "Egress",
			},
		},
		&nbdb.ACL{
			UUID:      "named_acl_" + networkPolicy.Namespace + "_ARPallowPolicy" + "_Egress",
			Name:      []string{networkPolicy.Namespace + "_ARPallowPolicy"},
			Priority:  1001, // types.DefaultAllowPriority
			Direction: nbdb.ACLDirectionToLport,
			Match:     "inport == @" + pgHash + "_" + egressDenyPG + " && arp",
			Action:    nbdb.ACLActionAllow,
			Log:       false,
			Meter:     []string{"acl-logging"},
			Severity:  []string{nbdb.ACLSeverityInfo},
			ExternalIDs: map[string]string{
				"default-deny-policy-type": "Egress",
			},
		},
		&nbdb.ACL{
			UUID:      "named_acl_" + networkPolicy.Namespace + "_" + networkPolicy.Name + "_Ingress",
			Name:      []string{networkPolicy.Namespace + "_" + networkPolicy.Name},
			Priority:  1000, // types.DefaultDenyPriority
			Direction: nbdb.ACLDirectionToLport,
			Match:     "outport == @" + pgHash + "_" + ingressDenyPG,
			Action:    nbdb.ACLActionDrop,
			Log:       false,
			Meter:     []string{"acl-logging"},
			Severity:  []string{nbdb.ACLSeverityInfo},
			ExternalIDs: map[string]string{
				"default-deny-policy-type": "Ingress",
			},
		},
		&nbdb.ACL{
			UUID:      "named_acl_" + networkPolicy.Namespace + "_ARPallowPolicy" + "_Ingress",
			Name:      []string{networkPolicy.Namespace + "_ARPallowPolicy"},
			Priority:  1001, // types.DefaultAllowPriority
			Direction: nbdb.ACLDirectionToLport,
			Match:     "outport == @" + pgHash + "_" + ingressDenyPG + " && arp",
			Action:    nbdb.ACLActionAllow,
			Log:       false,
			Meter:     []string{"acl-logging"},
			Severity:  []string{nbdb.ACLSeverityInfo},
			ExternalIDs: map[string]string{
				"default-deny-policy-type": "Ingress",
			},
		},
		&nbdb.PortGroup{
			UUID: "named_pg_" + pgHash + "_" + ingressDenyPG,
			Name: pgHash + "_" + ingressDenyPG,
			ACLs: []string{
				"named_acl_" + networkPolicy.Namespace + "_" + networkPolicy.Name + "_Ingress",
				"named_acl_" + networkPolicy.Namespace + "_ARPallowPolicy" + "_Ingress",
			},
			ExternalIDs: map[string]string{
				"name": pgHash + "_" + ingressDenyPG,
			},
			Ports: ports,
		},
		&nbdb.PortGroup{
			UUID: "named_pg_" + pgHash + "_" + egressDenyPG,
			Name: pgHash + "_" + egressDenyPG,
			ACLs: []string{
				"named_acl_" + networkPolicy.Namespace + "_" + networkPolicy.Name + "_Egress",
				"named_acl_" + networkPolicy.Namespace + "_ARPallowPolicy" + "_Egress",
			},
			ExternalIDs: map[string]string{
				"name": pgHash + "_" + egressDenyPG,
			},
			Ports: ports,
		},
	}
}

func (n kNetworkPolicy) addLocalPodCmds(fexec *ovntest.FakeExec, networkPolicy *knet.NetworkPolicy) {
	n.addDefaultDenyPGCmds(fexec, networkPolicy)
}

func (n kNetworkPolicy) addNamespaceSelectorCmds(fexec *ovntest.FakeExec, networkPolicy *knet.NetworkPolicy, namespace string) {
	as := []string{}
	if namespace != "" {
		as = append(as, namespace)
	}

	for i := range networkPolicy.Spec.Ingress {
		ingressAsMatch := asMatch(append(as, getAddressSetName(networkPolicy.Namespace, networkPolicy.Name, knet.PolicyTypeIngress, i)))
		fexec.AddFakeCmdsNoOutputNoError([]string{
			fmt.Sprintf("ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find ACL external-ids:l4Match=\"None\" external-ids:ipblock_cidr=false external-ids:namespace=%s external-ids:policy=%s external-ids:Ingress_num=%v external-ids:policy_type=Ingress", networkPolicy.Namespace, networkPolicy.Name, i),
			"ovn-nbctl --timeout=15 --id=@acl create acl priority=" + types.DefaultAllowPriority + " direction=" + types.DirectionToLPort + " match=\"ip4.src == {" + ingressAsMatch + "} && outport == @a14195333570786048679\" action=allow-related log=false severity=info meter=acl-logging name=" + networkPolicy.Namespace + "_" + networkPolicy.Name + "_" + strconv.Itoa(i) + " external-ids:l4Match=\"None\" external-ids:ipblock_cidr=false external-ids:namespace=namespace1 external-ids:policy=networkpolicy1 external-ids:Ingress_num=0 external-ids:policy_type=Ingress -- add port_group " + fakePgUUID + " acls @acl",
		})
	}
	for i := range networkPolicy.Spec.Egress {
		egressAsMatch := asMatch(append(as, getAddressSetName(networkPolicy.Namespace, networkPolicy.Name, knet.PolicyTypeEgress, i)))
		fexec.AddFakeCmdsNoOutputNoError([]string{
			fmt.Sprintf("ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find ACL external-ids:l4Match=\"None\" external-ids:ipblock_cidr=false external-ids:namespace=%s external-ids:policy=%s external-ids:Egress_num=%v external-ids:policy_type=Egress", networkPolicy.Namespace, networkPolicy.Name, i),
			"ovn-nbctl --timeout=15 --id=@acl create acl priority=" + types.DefaultAllowPriority + " direction=" + types.DirectionToLPort + " match=\"ip4.dst == {" + egressAsMatch + "} && inport == @a14195333570786048679\" action=allow-related log=false severity=info meter=acl-logging name=" + networkPolicy.Namespace + "_" + networkPolicy.Name + "_" + strconv.Itoa(i) + " external-ids:l4Match=\"None\" external-ids:ipblock_cidr=false external-ids:namespace=namespace1 external-ids:policy=networkpolicy1 external-ids:Egress_num=0 external-ids:policy_type=Egress -- add port_group " + fakePgUUID + " acls @acl",
		})
	}
}

func (n kNetworkPolicy) getPolicyData(networkPolicy *knet.NetworkPolicy, policyPorts []string, peerNamespaces []string, tcpPeerPorts []int32) []libovsdb.TestData {
	pgHash := hashedPortGroup(networkPolicy.Namespace + "_" + networkPolicy.Name)
	data := []libovsdb.TestData{}
	aclNamedUUIDs := []string{}
	for i := range networkPolicy.Spec.Ingress {
		aclName := networkPolicy.Namespace + "_" + networkPolicy.Name + "_" + strconv.Itoa(i)
		if peerNamespaces != nil {
			ingressAsMatch := asMatch(append(peerNamespaces, getAddressSetName(networkPolicy.Namespace, networkPolicy.Name, knet.PolicyTypeIngress, i)))
			aclNamedUUID := "named_acl_" + aclName + "_Ingress"
			aclNamedUUIDs = append(aclNamedUUIDs, aclNamedUUID)
			data = append(data, &nbdb.ACL{
				UUID:      aclNamedUUID,
				Name:      []string{aclName},
				Priority:  1001, // types.DefaultAllowPriority
				Direction: nbdb.ACLDirectionToLport,
				Match:     fmt.Sprintf("ip4.src == {%s} && outport == @%s", ingressAsMatch, pgHash),
				Action:    nbdb.ACLActionAllowRelated,
				Log:       false,
				Meter:     []string{"acl-logging"},
				Severity:  []string{nbdb.ACLSeverityInfo},
				ExternalIDs: map[string]string{
					"l4Match":      "None",
					"ipblock_cidr": "false",
					"namespace":    networkPolicy.Namespace,
					"policy":       networkPolicy.Name,
					"Ingress_num":  strconv.Itoa(i),
					"policy_type":  string(knet.PolicyTypeIngress),
				},
			})
		}
		for _, v := range tcpPeerPorts {
			aclNamedUUID := fmt.Sprintf("named_acl_%s_Ingress_tcp_%d", aclName, v)
			aclNamedUUIDs = append(aclNamedUUIDs, aclNamedUUID)
			data = append(data, &nbdb.ACL{
				UUID:      aclNamedUUID,
				Name:      []string{aclName},
				Priority:  1001, // types.DefaultAllowPriority
				Direction: nbdb.ACLDirectionToLport,
				Match:     fmt.Sprintf("ip4 && tcp && tcp.dst==%d && outport == @%s", v, pgHash),
				Action:    nbdb.ACLActionAllowRelated,
				Log:       false,
				Meter:     []string{"acl-logging"},
				Severity:  []string{nbdb.ACLSeverityInfo},
				ExternalIDs: map[string]string{
					"l4Match":      fmt.Sprintf("tcp && tcp.dst==%d", v),
					"ipblock_cidr": "false",
					"namespace":    networkPolicy.Namespace,
					"policy":       networkPolicy.Name,
					"Ingress_num":  strconv.Itoa(i),
					"policy_type":  string(knet.PolicyTypeIngress),
				},
			})
		}
	}

	for i := range networkPolicy.Spec.Egress {
		aclName := networkPolicy.Namespace + "_" + networkPolicy.Name + "_" + strconv.Itoa(i)
		if peerNamespaces != nil {
			egressAsMatch := asMatch(append(peerNamespaces, getAddressSetName(networkPolicy.Namespace, networkPolicy.Name, knet.PolicyTypeEgress, i)))
			aclNamedUUID := "named_acl_" + aclName + "_Egress"
			aclNamedUUIDs = append(aclNamedUUIDs, aclNamedUUID)
			data = append(data, &nbdb.ACL{
				UUID:      aclNamedUUID,
				Name:      []string{aclName},
				Priority:  1001, // types.DefaultAllowPriority
				Direction: nbdb.ACLDirectionToLport,
				Match:     fmt.Sprintf("ip4.dst == {%s} && inport == @%s", egressAsMatch, pgHash),
				Action:    nbdb.ACLActionAllowRelated,
				Log:       false,
				Meter:     []string{"acl-logging"},
				Severity:  []string{nbdb.ACLSeverityInfo},
				ExternalIDs: map[string]string{
					"l4Match":      "None",
					"ipblock_cidr": "false",
					"namespace":    networkPolicy.Namespace,
					"policy":       networkPolicy.Name,
					"Egress_num":   strconv.Itoa(i),
					"policy_type":  string(knet.PolicyTypeEgress),
				},
			})
		}
		for _, v := range tcpPeerPorts {
			aclNamedUUID := fmt.Sprintf("named_acl_%s_Egress_tcp_%d", aclName, v)
			aclNamedUUIDs = append(aclNamedUUIDs, aclNamedUUID)
			data = append(data, &nbdb.ACL{
				UUID:      aclNamedUUID,
				Name:      []string{aclName},
				Priority:  1001, // types.DefaultAllowPriority
				Direction: nbdb.ACLDirectionToLport,
				Match:     fmt.Sprintf("ip4 && tcp && tcp.dst==%d && inport == @%s", v, pgHash),
				Action:    nbdb.ACLActionAllowRelated,
				Log:       false,
				Meter:     []string{"acl-logging"},
				Severity:  []string{nbdb.ACLSeverityInfo},
				ExternalIDs: map[string]string{
					"l4Match":      fmt.Sprintf("tcp && tcp.dst==%d", v),
					"ipblock_cidr": "false",
					"namespace":    networkPolicy.Namespace,
					"policy":       networkPolicy.Name,
					"Egress_num":   strconv.Itoa(i),
					"policy_type":  string(knet.PolicyTypeEgress),
				},
			})
		}
	}

	data = append(data,
		&nbdb.PortGroup{
			UUID: "named_pg_" + networkPolicy.Namespace + "_" + networkPolicy.Name,
			Name: hashedPortGroup(networkPolicy.Namespace + "_" + networkPolicy.Name),
			ACLs: aclNamedUUIDs,
			ExternalIDs: map[string]string{
				"name": networkPolicy.Namespace + "_" + networkPolicy.Name,
			},
			Ports: policyPorts,
		},
	)

	return data
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

func expectAddressSetsWithIP(fakeOvn *FakeOVN, networkPolicy *knet.NetworkPolicy, ip string) {
	for i := range networkPolicy.Spec.Ingress {
		asName := getAddressSetName(networkPolicy.Namespace, networkPolicy.Name, knet.PolicyTypeIngress, i)
		fakeOvn.asf.ExpectAddressSetWithIPs(asName, []string{ip})
	}
	for i := range networkPolicy.Spec.Egress {
		asName := getAddressSetName(networkPolicy.Namespace, networkPolicy.Name, knet.PolicyTypeEgress, i)
		fakeOvn.asf.ExpectAddressSetWithIPs(asName, []string{ip})
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

func (p multicastPolicy) getMulticastPolicyExpectedData(ns string) []libovsdb.TestData {
	pg_hash := hashedPortGroup(ns)
	egressMatch := getACLMatch(pg_hash, getMulticastACLEgrMatch(), knet.PolicyTypeEgress)

	ip4AddressSet, ip6AddressSet := addressset.MakeAddressSetHashNames(ns)
	mcastMatch := getACLMatchAF(getMulticastACLIgrMatchV4(ip4AddressSet), getMulticastACLIgrMatchV6(ip6AddressSet))
	ingressMatch := getACLMatch(pg_hash, mcastMatch, knet.PolicyTypeIngress)

	return []libovsdb.TestData{
		&nbdb.ACL{
			UUID:      ns + "_MulticastAllowEgress",
			Name:      []string{ns + "_MulticastAllowEgress"},
			Priority:  1012,
			Direction: nbdb.ACLDirectionFromLport,
			Match:     egressMatch,
			Action:    nbdb.ACLActionAllow,
			Log:       false,
			Meter:     []string{"acl-logging"},
			Severity:  []string{nbdb.ACLSeverityInfo},
			ExternalIDs: map[string]string{
				"default-deny-policy-type": "Egress",
			},
		},
		&nbdb.ACL{
			UUID:      ns + "_MulticastAllowIngress",
			Name:      []string{ns + "_MulticastAllowIngress"},
			Priority:  1012,
			Direction: nbdb.ACLDirectionToLport,
			Match:     ingressMatch,
			Action:    nbdb.ACLActionAllow,
			Log:       false,
			Meter:     []string{"acl-logging"},
			Severity:  []string{nbdb.ACLSeverityInfo},
			ExternalIDs: map[string]string{
				"default-deny-policy-type": "Ingress",
			},
		},
		&nbdb.PortGroup{
			UUID: ns,
			Name: hashedPortGroup(ns),
			ACLs: []string{
				ns + "_MulticastAllowEgress",
				ns + "_MulticastAllowIngress",
			},
			ExternalIDs: map[string]string{
				"name": ns,
			},
		},
	}
}

var _ = ginkgo.Describe("OVN NetworkPolicy Operations with IP Address Family", func() {
	const (
		namespaceName1 = "namespace1"
		namespaceName2 = "namespace2"
	)
	var (
		app     *cli.App
		fakeOvn *FakeOVN
		fExec   *ovntest.FakeExec
	)

	ginkgo.BeforeEach(func() {
		// Restore global default values before each testcase
		config.PrepareTestConfig()
		config.IPv4Mode = true
		config.IPv6Mode = false

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

		fExec = ovntest.NewLooseCompareFakeExec()
		fakeOvn = NewFakeOVN(fExec)
		ovntest.ResetNumMockExecutions()
	})

	ginkgo.AfterEach(func() {
		fakeOvn.shutdown()
	})

	ginkgo.Context("during execution", func() {
		for _, m := range getIpModes() {
			m := m
			ginkgo.It("tests enabling/disabling multicast in a namespace "+ipModeStr(m), func() {
				app.Action = func(ctx *cli.Context) error {
					namespace1 := *newNamespace(namespaceName1)

					fakeOvn.start(ctx,
						&v1.NamespaceList{
							Items: []v1.Namespace{
								namespace1,
							},
						},
					)
					setIpMode(m)

					fakeOvn.controller.WatchNamespaces()
					ns, err := fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Get(context.TODO(), namespace1.Name, metav1.GetOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					gomega.Expect(ns).NotTo(gomega.BeNil())

					// Multicast is denied by default.
					_, ok := ns.Annotations[nsMulticastAnnotation]
					gomega.Expect(ok).To(gomega.BeFalse())

					// Enable multicast in the namespace.
					mcastPolicy := multicastPolicy{}
					expectedData := mcastPolicy.getMulticastPolicyExpectedData(namespace1.Name)
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
						"0a:58:0a:80:02:03",
						namespace1.Name,
					)
					var tPods []pod
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

					fakeOvn.start(ctx,
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
						tPod.baseCmds(fExec)
						tPod.populateLogicalSwitchCache(fakeOvn)
					}

					fakeOvn.controller.WatchNamespaces()
					fakeOvn.controller.WatchPods()
					ns, err := fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Get(context.TODO(), namespace1.Name, metav1.GetOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					gomega.Expect(ns).NotTo(gomega.BeNil())

					// Enable multicast in the namespace.
					mcastPolicy := multicastPolicy{}
					expectedData := mcastPolicy.getMulticastPolicyExpectedData(namespace1.Name)
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
						"0a:58:0a:80:02:03",
						namespace1.Name,
					)
					var tPods []pod
					var tPodIPs []string
					if m.IPv4Mode {
						tPods = append(tPods, nPodTestV4)
						tPodIPs = append(tPodIPs, nPodTestV4.podIP)
					}
					if m.IPv6Mode {
						tPods = append(tPods, nPodTestV6)
						tPodIPs = append(tPodIPs, nPodTestV6.podIP)
					}

					fakeOvn.start(ctx,
						&v1.NamespaceList{
							Items: []v1.Namespace{
								namespace1,
							},
						},
					)
					setIpMode(m)

					nPodTestV4.baseCmds(fExec)

					fakeOvn.controller.WatchNamespaces()
					fakeOvn.controller.WatchPods()
					ns, err := fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Get(context.TODO(), namespace1.Name, metav1.GetOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					gomega.Expect(ns).NotTo(gomega.BeNil())

					// Enable multicast in the namespace.
					mcastPolicy := multicastPolicy{}
					expectedData := mcastPolicy.getMulticastPolicyExpectedData(namespace1.Name)
					ns.Annotations[nsMulticastAnnotation] = "true"
					_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Update(context.TODO(), ns, metav1.UpdateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

					for _, tPod := range tPods {
						tPod.populateLogicalSwitchCache(fakeOvn)
						_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(tPod.namespace).Create(context.TODO(), newPod(
							tPod.namespace, tPod.podName, tPod.nodeName, tPod.podIP), metav1.CreateOptions{})
						gomega.Expect(err).NotTo(gomega.HaveOccurred())
					}
					fakeOvn.asf.EventuallyExpectAddressSetWithIPs(namespace1.Name, tPodIPs)

					for _, tPod := range tPods {
						// Delete the pod from the namespace.
						err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(tPod.namespace).Delete(context.TODO(),
							tPod.podName, *metav1.NewDeleteOptions(0))
						gomega.Expect(err).NotTo(gomega.HaveOccurred())
					}
					fakeOvn.asf.EventuallyExpectEmptyAddressSetExist(namespace1.Name)

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
		app     *cli.App
		fakeOvn *FakeOVN
		fExec   *ovntest.FakeExec

		gomegaFormatMaxLength int
	)

	ginkgo.BeforeEach(func() {
		// Restore global default values before each testcase
		config.PrepareTestConfig()

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

		fExec = ovntest.NewLooseCompareFakeExec()
		fakeOvn = NewFakeOVN(fExec)

		gomegaFormatMaxLength = format.MaxLength
		format.MaxLength = 0
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
				gressPolicyExpectedData := npTest.getPolicyData(networkPolicy, nil, []string{namespace2.Name}, nil)
				defaultDenyExpectedData := npTest.getDefaultDenyData(networkPolicy, nil)
				expectedData := []libovsdb.TestData{}
				expectedData = append(expectedData, gressPolicyExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)

				fakeOvn.start(ctx,
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

				fakeOvn.controller.WatchNamespaces()
				fakeOvn.controller.WatchNetworkPolicy()

				fakeOvn.asf.ExpectEmptyAddressSet(namespaceName1)
				fakeOvn.asf.ExpectEmptyAddressSet(namespaceName2)

				eventuallyExpectEmptyAddressSetsExist(fakeOvn, networkPolicy)

				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
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
				gressPolicyInitialData := npTest.getPolicyData(networkPolicy, nil, []string{}, nil)
				defaultDenyInitialData := npTest.getDefaultDenyData(networkPolicy, nil)
				initialData := []libovsdb.TestData{}
				initialData = append(initialData, gressPolicyInitialData...)
				initialData = append(initialData, defaultDenyInitialData...)

				fakeOvn.startWithDBSetup(ctx,
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

				fakeOvn.controller.WatchNamespaces()
				fakeOvn.controller.WatchNetworkPolicy()

				fakeOvn.asf.ExpectEmptyAddressSet(namespaceName1)
				fakeOvn.asf.ExpectEmptyAddressSet(namespaceName2)

				eventuallyExpectEmptyAddressSetsExist(fakeOvn, networkPolicy)

				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectedData := npTest.getPolicyData(networkPolicy, nil, []string{namespace2.Name}, nil)
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

				nPodTest.baseCmds(fExec)

				npTest := kNetworkPolicy{}
				gressPolicyExpectedData := npTest.getPolicyData(networkPolicy, []string{fakeUUID}, []string{}, nil)
				defaultDenyExpectedData := npTest.getDefaultDenyData(networkPolicy, []string{fakeUUID})
				expectedData := []libovsdb.TestData{}
				expectedData = append(expectedData, gressPolicyExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)

				fakeOvn.start(ctx,
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
				nPodTest.populateLogicalSwitchCache(fakeOvn)
				fakeOvn.controller.WatchNamespaces()
				fakeOvn.controller.WatchPods()
				fakeOvn.controller.WatchNetworkPolicy()

				expectAddressSetsWithIP(fakeOvn, networkPolicy, nPodTest.podIP)
				fakeOvn.asf.ExpectAddressSetWithIPs(namespaceName1, []string{nPodTest.podIP})

				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(fExec.CalledMatchesExpected).Should(gomega.BeTrue(), fExec.ErrorDesc)
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

				nPodTest.baseCmds(fExec)

				npTest := kNetworkPolicy{}
				gressPolicyExpectedData := npTest.getPolicyData(networkPolicy, nil, []string{}, nil)
				defaultDenyExpectedData := npTest.getDefaultDenyData(networkPolicy, nil)
				expectedData := []libovsdb.TestData{}
				expectedData = append(expectedData, gressPolicyExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)

				fakeOvn.start(ctx,
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
				nPodTest.populateLogicalSwitchCache(fakeOvn)
				fakeOvn.controller.WatchNamespaces()
				fakeOvn.controller.WatchPods()
				fakeOvn.controller.WatchNetworkPolicy()

				fakeOvn.asf.ExpectEmptyAddressSet(namespaceName1)
				expectAddressSetsWithIP(fakeOvn, networkPolicy, nPodTest.podIP)
				fakeOvn.asf.ExpectAddressSetWithIPs(namespaceName2, []string{nPodTest.podIP})

				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(fExec.CalledMatchesExpected).Should(gomega.BeTrue(), fExec.ErrorDesc)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

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

				nPodTest.baseCmds(fExec)

				npTest := kNetworkPolicy{}
				gressPolicy1ExpectedData := npTest.getPolicyData(networkPolicy, []string{fakeUUID}, nil, []int32{portNum})
				defaultDenyExpectedData := npTest.getDefaultDenyData(networkPolicy, []string{fakeUUID})
				expectedData := []libovsdb.TestData{}
				expectedData = append(expectedData, gressPolicy1ExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)

				fakeOvn.start(ctx,
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
				nPodTest.populateLogicalSwitchCache(fakeOvn)
				fakeOvn.controller.WatchNamespaces()
				fakeOvn.controller.WatchPods()
				fakeOvn.controller.WatchNetworkPolicy()

				ginkgo.By("Creating a network policy that applies to a pod")

				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.asf.ExpectAddressSetWithIPs(namespaceName1, []string{nPodTest.podIP})
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

				// Create a second NP
				ginkgo.By("Creating and deleting another policy that references that pod")

				_, err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).Create(context.TODO(), networkPolicy2, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gressPolicy2ExpectedData := npTest.getPolicyData(networkPolicy2, []string{fakeUUID}, nil, []int32{portNum + 1})
				expectedDataWithPolicy2 := append(expectedData, gressPolicy2ExpectedData...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedDataWithPolicy2...))

				// Delete the second network policy
				err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy2.Namespace).Delete(context.TODO(), networkPolicy2.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

				// Delete the first network policy
				ginkgo.By("Deleting that network policy")
				err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).Delete(context.TODO(), networkPolicy.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				acls := []libovsdb.TestData{}
				acls = append(acls, gressPolicy1ExpectedData[:len(gressPolicy1ExpectedData)-1]...)
				acls = append(acls, gressPolicy2ExpectedData[:len(gressPolicy2ExpectedData)-1]...)
				acls = append(acls, defaultDenyExpectedData[:len(defaultDenyExpectedData)-2]...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(acls...))

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

				nPodTest.baseCmds(fExec)

				npTest := kNetworkPolicy{}
				gressPolicyExpectedData := npTest.getPolicyData(networkPolicy, []string{fakeUUID}, []string{namespace2.Name}, nil)
				defaultDenyExpectedData := npTest.getDefaultDenyData(networkPolicy, []string{fakeUUID})
				expectedData := []libovsdb.TestData{}
				expectedData = append(expectedData, gressPolicyExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)

				fakeOvn.start(ctx,
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
				nPodTest.populateLogicalSwitchCache(fakeOvn)
				fakeOvn.controller.WatchNamespaces()
				fakeOvn.controller.WatchPods()
				fakeOvn.controller.WatchNetworkPolicy()

				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.asf.ExpectAddressSetWithIPs(namespaceName1, []string{nPodTest.podIP})
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

				expectedData = []libovsdb.TestData{}
				gressPolicyExpectedData = npTest.getPolicyData(networkPolicy, []string{fakeUUID}, []string{}, nil)
				expectedData = append(expectedData, gressPolicyExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)

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
				gressPolicyExpectedData := npTest.getPolicyData(networkPolicy, nil, []string{namespace2.Name}, nil)
				defaultDenyExpectedData := npTest.getDefaultDenyData(networkPolicy, nil)
				expectedData := []libovsdb.TestData{}
				expectedData = append(expectedData, gressPolicyExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)

				fakeOvn.start(ctx,
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

				fakeOvn.controller.WatchNamespaces()
				fakeOvn.controller.WatchNetworkPolicy()

				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

				expectedData = []libovsdb.TestData{}
				gressPolicyExpectedData = npTest.getPolicyData(networkPolicy, nil, []string{}, nil)
				expectedData = append(expectedData, gressPolicyExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)

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

				nPodTest.baseCmds(fExec)

				npTest := kNetworkPolicy{}
				gressPolicyExpectedData := npTest.getPolicyData(networkPolicy, []string{fakeUUID}, []string{}, nil)
				defaultDenyExpectedData := npTest.getDefaultDenyData(networkPolicy, []string{fakeUUID})
				expectedData := []libovsdb.TestData{}
				expectedData = append(expectedData, gressPolicyExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)

				fakeOvn.start(ctx,
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
				nPodTest.populateLogicalSwitchCache(fakeOvn)
				fakeOvn.controller.WatchNamespaces()
				fakeOvn.controller.WatchPods()
				fakeOvn.controller.WatchNetworkPolicy()

				expectAddressSetsWithIP(fakeOvn, networkPolicy, nPodTest.podIP)
				fakeOvn.asf.ExpectAddressSetWithIPs(namespaceName1, []string{nPodTest.podIP})

				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

				err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(nPodTest.namespace).Delete(context.TODO(), nPodTest.podName, *metav1.NewDeleteOptions(0))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				eventuallyExpectEmptyAddressSetsExist(fakeOvn, networkPolicy)
				fakeOvn.asf.EventuallyExpectEmptyAddressSetExist(namespaceName1)

				gressPolicyExpectedData = npTest.getPolicyData(networkPolicy, nil, []string{}, nil)
				defaultDenyExpectedData = npTest.getDefaultDenyData(networkPolicy, nil)
				expectedData = []libovsdb.TestData{}
				expectedData = append(expectedData, gressPolicyExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)
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

				nPodTest.baseCmds(fExec)

				npTest := kNetworkPolicy{}
				gressPolicyExpectedData := npTest.getPolicyData(networkPolicy, nil, []string{}, nil)
				defaultDenyExpectedData := npTest.getDefaultDenyData(networkPolicy, nil)
				expectedData := []libovsdb.TestData{}
				expectedData = append(expectedData, gressPolicyExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)

				fakeOvn.start(ctx,
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
				nPodTest.populateLogicalSwitchCache(fakeOvn)
				fakeOvn.controller.WatchNamespaces()
				fakeOvn.controller.WatchPods()
				fakeOvn.controller.WatchNetworkPolicy()

				fakeOvn.asf.ExpectEmptyAddressSet(namespaceName1)
				expectAddressSetsWithIP(fakeOvn, networkPolicy, nPodTest.podIP)
				fakeOvn.asf.ExpectAddressSetWithIPs(namespaceName2, []string{nPodTest.podIP})

				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

				err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(nPodTest.namespace).Delete(context.TODO(), nPodTest.podName, *metav1.NewDeleteOptions(0))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

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

				nPodTest.baseCmds(fExec)

				npTest := kNetworkPolicy{}
				gressPolicyExpectedData := npTest.getPolicyData(networkPolicy, nil, []string{}, nil)
				defaultDenyExpectedData := npTest.getDefaultDenyData(networkPolicy, nil)
				expectedData := []libovsdb.TestData{}
				expectedData = append(expectedData, gressPolicyExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)

				fakeOvn.start(ctx,
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
				nPodTest.populateLogicalSwitchCache(fakeOvn)
				fakeOvn.controller.WatchNamespaces()
				fakeOvn.controller.WatchPods()
				fakeOvn.controller.WatchNetworkPolicy()

				fakeOvn.asf.ExpectEmptyAddressSet(namespaceName1)
				expectAddressSetsWithIP(fakeOvn, networkPolicy, nPodTest.podIP)
				fakeOvn.asf.ExpectAddressSetWithIPs(namespaceName2, []string{nPodTest.podIP})

				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
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

				nPodTest.baseCmds(fExec)

				npTest := kNetworkPolicy{}
				gressPolicyExpectedData := npTest.getPolicyData(networkPolicy, []string{fakeUUID}, []string{}, nil)
				defaultDenyExpectedData := npTest.getDefaultDenyData(networkPolicy, []string{fakeUUID})
				expectedData := []libovsdb.TestData{}
				expectedData = append(expectedData, gressPolicyExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)

				fakeOvn.start(ctx,
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
				nPodTest.populateLogicalSwitchCache(fakeOvn)
				fakeOvn.controller.WatchNamespaces()
				fakeOvn.controller.WatchPods()
				fakeOvn.controller.WatchNetworkPolicy()

				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))
				fakeOvn.asf.ExpectAddressSetWithIPs(namespaceName1, []string{nPodTest.podIP})

				err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).Delete(context.TODO(), networkPolicy.Name, *metav1.NewDeleteOptions(0))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				eventuallyExpectNoAddressSets(fakeOvn, networkPolicy)

				acls := []libovsdb.TestData{}
				acls = append(acls, gressPolicyExpectedData[:len(gressPolicyExpectedData)-1]...)
				acls = append(acls, defaultDenyExpectedData[:len(defaultDenyExpectedData)-2]...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(acls))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
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

func buildExpectedACLs(gp *gressPolicy, pgName string, as []string) []*nbdb.ACL {
	name := gp.policyNamespace + "_" + gp.policyName + "_" + strconv.Itoa(gp.idx)
	asMatch := asMatch(as)
	match := fmt.Sprintf("ip4.src == {%s} && outport == @%s", asMatch, pgName)
	gpDirection := string(knet.PolicyTypeIngress)
	externalIds := map[string]string{
		"l4Match":            "None",
		"ipblock_cidr":       "false",
		"namespace":          gp.policyNamespace,
		"policy":             gp.policyName,
		gpDirection + "_num": fmt.Sprintf("%d", gp.idx),
		"policy_type":        gpDirection,
	}
	acl := libovsdbops.BuildACL(name, nbdb.ACLDirectionToLport, match, nbdb.ACLActionAllowRelated, "acl-logging", nbdb.ACLSeverityInfo, 1001, true, externalIds)
	return []*nbdb.ACL{acl}
}

var _ = ginkgo.Describe("OVN NetworkPolicy Low-Level Operations", func() {
	var (
		asFactory *addressset.FakeAddressSetFactory
	)

	ginkgo.BeforeEach(func() {
		// Restore global default values before each testcase
		config.PrepareTestConfig()

		asFactory = addressset.NewFakeAddressSetFactory()
		config.IPv4Mode = true
		config.IPv6Mode = false
	})

	ginkgo.It("computes match strings from address sets correctly", func() {
		const (
			pgName string = "pg-name"
		)

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
		expected := buildExpectedACLs(gp, pgName, []string{asName, one})
		actual := gp.buildLocalPodACLs(pgName, defaultACLLoggingSeverity)
		gomega.Expect(actual).To(gomega.Equal(expected))

		gomega.Expect(gp.addNamespaceAddressSet(two)).To(gomega.BeTrue())
		expected = buildExpectedACLs(gp, pgName, []string{asName, one, two})
		actual = gp.buildLocalPodACLs(pgName, defaultACLLoggingSeverity)
		gomega.Expect(actual).To(gomega.Equal(expected))

		// address sets should be alphabetized
		gomega.Expect(gp.addNamespaceAddressSet(three)).To(gomega.BeTrue())
		expected = buildExpectedACLs(gp, pgName, []string{asName, one, two, three})
		actual = gp.buildLocalPodACLs(pgName, defaultACLLoggingSeverity)
		gomega.Expect(actual).To(gomega.Equal(expected))

		// re-adding an existing set is a no-op
		gomega.Expect(gp.addNamespaceAddressSet(three)).To(gomega.BeFalse())

		gomega.Expect(gp.addNamespaceAddressSet(four)).To(gomega.BeTrue())
		expected = buildExpectedACLs(gp, pgName, []string{asName, one, two, three, four})
		actual = gp.buildLocalPodACLs(pgName, defaultACLLoggingSeverity)
		gomega.Expect(actual).To(gomega.Equal(expected))

		// now delete a set
		gomega.Expect(gp.delNamespaceAddressSet(one)).To(gomega.BeTrue())
		expected = buildExpectedACLs(gp, pgName, []string{asName, two, three, four})
		actual = gp.buildLocalPodACLs(pgName, defaultACLLoggingSeverity)
		gomega.Expect(actual).To(gomega.Equal(expected))

		// deleting again is a no-op
		gomega.Expect(gp.delNamespaceAddressSet(one)).To(gomega.BeFalse())

		// add and delete some more...
		gomega.Expect(gp.addNamespaceAddressSet(five)).To(gomega.BeTrue())
		expected = buildExpectedACLs(gp, pgName, []string{asName, two, three, four, five})
		actual = gp.buildLocalPodACLs(pgName, defaultACLLoggingSeverity)
		gomega.Expect(actual).To(gomega.Equal(expected))

		gomega.Expect(gp.delNamespaceAddressSet(three)).To(gomega.BeTrue())
		expected = buildExpectedACLs(gp, pgName, []string{asName, two, four, five})
		actual = gp.buildLocalPodACLs(pgName, defaultACLLoggingSeverity)
		gomega.Expect(actual).To(gomega.Equal(expected))

		// deleting again is no-op
		gomega.Expect(gp.delNamespaceAddressSet(one)).To(gomega.BeFalse())

		gomega.Expect(gp.addNamespaceAddressSet(six)).To(gomega.BeTrue())
		expected = buildExpectedACLs(gp, pgName, []string{asName, two, four, five, six})
		actual = gp.buildLocalPodACLs(pgName, defaultACLLoggingSeverity)
		gomega.Expect(actual).To(gomega.Equal(expected))

		gomega.Expect(gp.delNamespaceAddressSet(two)).To(gomega.BeTrue())
		expected = buildExpectedACLs(gp, pgName, []string{asName, four, five, six})
		actual = gp.buildLocalPodACLs(pgName, defaultACLLoggingSeverity)
		gomega.Expect(actual).To(gomega.Equal(expected))

		gomega.Expect(gp.delNamespaceAddressSet(five)).To(gomega.BeTrue())
		expected = buildExpectedACLs(gp, pgName, []string{asName, four, six})
		actual = gp.buildLocalPodACLs(pgName, defaultACLLoggingSeverity)
		gomega.Expect(actual).To(gomega.Equal(expected))

		gomega.Expect(gp.delNamespaceAddressSet(six)).To(gomega.BeTrue())
		expected = buildExpectedACLs(gp, pgName, []string{asName, four})
		actual = gp.buildLocalPodACLs(pgName, defaultACLLoggingSeverity)
		gomega.Expect(actual).To(gomega.Equal(expected))

		gomega.Expect(gp.delNamespaceAddressSet(four)).To(gomega.BeTrue())
		expected = buildExpectedACLs(gp, pgName, []string{asName})
		actual = gp.buildLocalPodACLs(pgName, defaultACLLoggingSeverity)
		gomega.Expect(actual).To(gomega.Equal(expected))

		// deleting again is no-op
		gomega.Expect(gp.delNamespaceAddressSet(four)).To(gomega.BeFalse())
	})
})
