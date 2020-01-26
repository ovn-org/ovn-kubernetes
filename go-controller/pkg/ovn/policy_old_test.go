package ovn

import (
	"fmt"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/urfave/cli"
	v1 "k8s.io/api/core/v1"
	knet "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

type networkPolicyOld struct{}

var fakeUUID = "fake_uuid"
var fakeLogicalSwitch = "fake_ls"

func newNetworkPolicyOldMeta(name, namespace string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		UID:       types.UID(namespace),
		Name:      name,
		Namespace: namespace,
		Labels: map[string]string{
			"name": name,
		},
	}
}

func newNetworkPolicyOld(name, namespace string, podSelector metav1.LabelSelector, ingress []knet.NetworkPolicyIngressRule, egress []knet.NetworkPolicyEgressRule) *knet.NetworkPolicy {
	return &knet.NetworkPolicy{
		ObjectMeta: newNetworkPolicyOldMeta(name, namespace),
		Spec: knet.NetworkPolicySpec{
			PodSelector: podSelector,
			Ingress:     ingress,
			Egress:      egress,
		},
	}
}

func (n networkPolicyOld) baseCmds(fexec *ovntest.FakeExec, networkPolicy knet.NetworkPolicy) {
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovn-nbctl --timeout=15 --data=bare --no-heading --columns=external_ids find address_set",
	})
}

func (n networkPolicyOld) addNamespaceSelectorCmdsForGress(fexec *ovntest.FakeExec, networkPolicy knet.NetworkPolicy, gress string, i int) {
	hashedOVNName := hashedAddressSet(fmt.Sprintf("%s.%s.%s.%d", networkPolicy.Namespace, networkPolicy.Name, gress, i))
	fexec.AddFakeCmdsNoOutputNoError([]string{
		fmt.Sprintf("ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find address_set name=%s", hashedOVNName),
		fmt.Sprintf("ovn-nbctl --timeout=15 create address_set name=%s external-ids:name=%s", hashedOVNName, fmt.Sprintf("%s.%s.%s.%v", networkPolicy.Namespace, networkPolicy.Name, gress, i)),
	})
}

func (n networkPolicyOld) addNamespaceSelectorCmds(fexec *ovntest.FakeExec, networkPolicy knet.NetworkPolicy) {
	n.baseCmds(fexec, networkPolicy)
	for i := range networkPolicy.Spec.Ingress {
		n.addNamespaceSelectorCmdsForGress(fexec, networkPolicy, "ingress", i)
	}
	for i := range networkPolicy.Spec.Egress {
		n.addNamespaceSelectorCmdsForGress(fexec, networkPolicy, "egress", i)
	}
}

func (n networkPolicyOld) addPodSelectorCmdsForGress(fexec *ovntest.FakeExec, pod pod, networkPolicy knet.NetworkPolicy, i int, gress string, hashName string) {
	if gress == "Ingress" {
		fexec.AddFakeCmdsNoOutputNoError([]string{
			fmt.Sprintf("ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find ACL %s action=drop external-ids:default-deny-policy-type=%s external-ids:namespace=%s external-ids:logical_switch=%s external-ids:logical_port=%s", fmt.Sprintf("match=\"outport == \\\"%s\\\"\"", pod.portName), gress, pod.namespace, pod.nodeName, pod.portName),
		})
		fexec.AddFakeCmdsNoOutputNoError([]string{
			fmt.Sprintf("ovn-nbctl --timeout=15 --id=@acl create acl priority=1000 direction=to-lport %s action=drop external-ids:default-deny-policy-type=%s external-ids:namespace=%s external-ids:logical_switch=%s external-ids:logical_port=%s -- add logical_switch %s acls @acl", fmt.Sprintf("match=\"outport == \\\"%s\\\"\"", pod.portName), gress, pod.namespace, pod.nodeName, pod.portName, pod.nodeName),
		})
	} else {
		fexec.AddFakeCmdsNoOutputNoError([]string{
			fmt.Sprintf("ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find ACL %s action=drop external-ids:default-deny-policy-type=%s external-ids:namespace=%s external-ids:logical_switch=%s external-ids:logical_port=%s", fmt.Sprintf("match=\"inport == \\\"%s\\\"\"", pod.portName), gress, pod.namespace, pod.nodeName, pod.portName),
		})
		fexec.AddFakeCmdsNoOutputNoError([]string{
			fmt.Sprintf("ovn-nbctl --timeout=15 --id=@acl create acl priority=1000 direction=to-lport %s action=drop external-ids:default-deny-policy-type=%s external-ids:namespace=%s external-ids:logical_switch=%s external-ids:logical_port=%s -- add logical_switch %s acls @acl", fmt.Sprintf("match=\"inport == \\\"%s\\\"\"", pod.portName), gress, pod.namespace, pod.nodeName, pod.portName, pod.nodeName),
		})
	}
}

func (n networkPolicyOld) baseLocalCmdsForGress(fexec *ovntest.FakeExec, pod pod, networkPolicy knet.NetworkPolicy, i int, gress string, hashName string) {
	fexec.AddFakeCmdsNoOutputNoError([]string{
		fmt.Sprintf("ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find ACL external-ids:l4Match=\"None\" external-ids:ipblock_cidr=false external-ids:namespace=%s external-ids:policy=%s external-ids:%s_num=%v external-ids:policy_type=%s external-ids:logical_switch=%s external-ids:logical_port=%s", pod.namespace, networkPolicy.Name, gress, i, gress, pod.nodeName, pod.portName),
	})
}

func (n networkPolicyOld) addLocalPodWithNamespaceSelectorCmdsForGress(fexec *ovntest.FakeExec, pod pod, networkPolicy knet.NetworkPolicy, i int, gress, hashName, hashedNamespaceName string) {
	if gress == "Ingress" {
		fexec.AddFakeCmdsNoOutputNoError([]string{
			fmt.Sprintf("ovn-nbctl --timeout=15 --id=@acl create acl priority=1001 direction=to-lport %s action=allow-related external-ids:l4Match=\"None\" external-ids:ipblock_cidr=false external-ids:namespace=%s external-ids:policy=%s external-ids:%s_num=%v external-ids:policy_type=%s external-ids:logical_switch=%s external-ids:logical_port=%s -- add logical_switch %s acls @acl", fmt.Sprintf("match=\"ip4.src == {$%s, $%s} && outport == \\\"%s\\\"\"", hashName, hashedNamespaceName, pod.portName), pod.namespace, networkPolicy.Name, gress, i, gress, pod.nodeName, pod.portName, pod.nodeName),
		})
	} else {
		fexec.AddFakeCmdsNoOutputNoError([]string{
			fmt.Sprintf("ovn-nbctl --timeout=15 --id=@acl create acl priority=1001 direction=to-lport %s action=allow external-ids:l4Match=\"None\" external-ids:ipblock_cidr=false external-ids:namespace=%s external-ids:policy=%s external-ids:%s_num=%v external-ids:policy_type=%s external-ids:logical_switch=%s external-ids:logical_port=%s -- add logical_switch %s acls @acl", fmt.Sprintf("match=\"ip4.dst == {$%s, $%s} && inport == \\\"%s\\\"\"", hashedNamespaceName, hashName, pod.portName), pod.namespace, networkPolicy.Name, gress, i, gress, pod.nodeName, pod.portName, pod.nodeName),
		})
	}
}

func (n networkPolicyOld) addLocalPodSelectorCmdsForGress(fexec *ovntest.FakeExec, pod pod, networkPolicy knet.NetworkPolicy, i int, gress string, hashName string) {
	if gress == "Ingress" {
		fexec.AddFakeCmdsNoOutputNoError([]string{
			fmt.Sprintf("ovn-nbctl --timeout=15 --id=@acl create acl priority=1001 direction=to-lport %s action=allow-related external-ids:l4Match=\"None\" external-ids:ipblock_cidr=false external-ids:namespace=%s external-ids:policy=%s external-ids:%s_num=%v external-ids:policy_type=%s external-ids:logical_switch=%s external-ids:logical_port=%s -- add logical_switch %s acls @acl", fmt.Sprintf("match=\"ip4.src == {$%s} && outport == \\\"%s\\\"\"", hashName, pod.portName), pod.namespace, networkPolicy.Name, gress, i, gress, pod.nodeName, pod.portName, pod.nodeName),
		})
	} else {
		fexec.AddFakeCmdsNoOutputNoError([]string{
			fmt.Sprintf("ovn-nbctl --timeout=15 --id=@acl create acl priority=1001 direction=to-lport %s action=allow external-ids:l4Match=\"None\" external-ids:ipblock_cidr=false external-ids:namespace=%s external-ids:policy=%s external-ids:%s_num=%v external-ids:policy_type=%s external-ids:logical_switch=%s external-ids:logical_port=%s -- add logical_switch %s acls @acl", fmt.Sprintf("match=\"ip4.dst == {$%s} && inport == \\\"%s\\\"\"", hashName, pod.portName), pod.namespace, networkPolicy.Name, gress, i, gress, pod.nodeName, pod.portName, pod.nodeName),
		})
	}
}
func (n networkPolicyOld) addLocalPodAndNamespaceSelectorCmds(fexec *ovntest.FakeExec, pod pod, networkPolicy knet.NetworkPolicy, namespace v1.Namespace) {
	n.baseCmds(fexec, networkPolicy)
	for i := range networkPolicy.Spec.Ingress {
		n.addNamespaceSelectorCmdsForGress(fexec, networkPolicy, "ingress", i)
	}
	for i := range networkPolicy.Spec.Egress {
		n.addNamespaceSelectorCmdsForGress(fexec, networkPolicy, "egress", i)
	}
	for i := range networkPolicy.Spec.Ingress {
		hashedOVNName := hashedAddressSet(fmt.Sprintf("%s.%s.%s.%d", networkPolicy.Namespace, networkPolicy.Name, "ingress", i))
		n.addPodSelectorCmdsForGress(fexec, pod, networkPolicy, i, "Ingress", hashedOVNName)
	}
	for i := range networkPolicy.Spec.Egress {
		hashedOVNName := hashedAddressSet(fmt.Sprintf("%s.%s.%s.%d", networkPolicy.Namespace, networkPolicy.Name, "egress", i))
		n.addPodSelectorCmdsForGress(fexec, pod, networkPolicy, i, "Egress", hashedOVNName)
	}

	for i := range networkPolicy.Spec.Ingress {
		hashedOVNName := hashedAddressSet(fmt.Sprintf("%s.%s.%s.%d", networkPolicy.Namespace, networkPolicy.Name, "ingress", i))
		n.baseLocalCmdsForGress(fexec, pod, networkPolicy, i, "Ingress", hashedOVNName)
		hashedNamespaceName := hashedAddressSet(namespace.Name)
		n.addLocalPodWithNamespaceSelectorCmdsForGress(fexec, pod, networkPolicy, i, "Ingress", hashedOVNName, hashedNamespaceName)
	}
	for i := range networkPolicy.Spec.Egress {
		hashedOVNName := hashedAddressSet(fmt.Sprintf("%s.%s.%s.%d", networkPolicy.Namespace, networkPolicy.Name, "egress", i))
		n.baseLocalCmdsForGress(fexec, pod, networkPolicy, i, "Egress", hashedOVNName)
		hashedNamespaceName := hashedAddressSet(namespace.Name)
		n.addLocalPodWithNamespaceSelectorCmdsForGress(fexec, pod, networkPolicy, i, "Egress", hashedOVNName, hashedNamespaceName)
	}
}

func (n networkPolicyOld) addLocalPodSelectorCmds(fexec *ovntest.FakeExec, pod pod, networkPolicy knet.NetworkPolicy) {
	n.baseCmds(fexec, networkPolicy)
	for i := range networkPolicy.Spec.Ingress {
		n.addNamespaceSelectorCmdsForGress(fexec, networkPolicy, "ingress", i)
		hashedOVNName := hashedAddressSet(fmt.Sprintf("%s.%s.%s.%d", networkPolicy.Namespace, networkPolicy.Name, "ingress", i))
		fexec.AddFakeCmdsNoOutputNoError([]string{
			fmt.Sprintf("ovn-nbctl --timeout=15 set address_set %s addresses=\"%s\"", hashedOVNName, pod.podIP),
		})
	}
	for i := range networkPolicy.Spec.Egress {
		n.addNamespaceSelectorCmdsForGress(fexec, networkPolicy, "egress", i)
		hashedOVNName := hashedAddressSet(fmt.Sprintf("%s.%s.%s.%d", networkPolicy.Namespace, networkPolicy.Name, "egress", i))
		fexec.AddFakeCmdsNoOutputNoError([]string{
			fmt.Sprintf("ovn-nbctl --timeout=15 set address_set %s addresses=\"%s\"", hashedOVNName, pod.podIP),
		})
	}
	for i := range networkPolicy.Spec.Ingress {
		hashedOVNName := hashedAddressSet(fmt.Sprintf("%s.%s.%s.%d", networkPolicy.Namespace, networkPolicy.Name, "ingress", i))
		n.addPodSelectorCmdsForGress(fexec, pod, networkPolicy, i, "Ingress", hashedOVNName)
	}
	for i := range networkPolicy.Spec.Egress {
		hashedOVNName := hashedAddressSet(fmt.Sprintf("%s.%s.%s.%d", networkPolicy.Namespace, networkPolicy.Name, "egress", i))
		n.addPodSelectorCmdsForGress(fexec, pod, networkPolicy, i, "Egress", hashedOVNName)
	}

	for i := range networkPolicy.Spec.Ingress {
		hashedOVNName := hashedAddressSet(fmt.Sprintf("%s.%s.%s.%d", networkPolicy.Namespace, networkPolicy.Name, "ingress", i))
		n.baseLocalCmdsForGress(fexec, pod, networkPolicy, i, "Ingress", hashedOVNName)
		n.addLocalPodSelectorCmdsForGress(fexec, pod, networkPolicy, i, "Ingress", hashedOVNName)
	}
	for i := range networkPolicy.Spec.Egress {
		hashedOVNName := hashedAddressSet(fmt.Sprintf("%s.%s.%s.%d", networkPolicy.Namespace, networkPolicy.Name, "egress", i))
		n.baseLocalCmdsForGress(fexec, pod, networkPolicy, i, "Egress", hashedOVNName)
		n.addLocalPodSelectorCmdsForGress(fexec, pod, networkPolicy, i, "Egress", hashedOVNName)
	}
}

func (n networkPolicyOld) addPodAndNamespaceSelectorCmds(fexec *ovntest.FakeExec, pod pod, namespace v1.Namespace, networkPolicy knet.NetworkPolicy) {
	n.baseCmds(fexec, networkPolicy)
	for i := range networkPolicy.Spec.Ingress {
		hashedOVNName := hashedAddressSet(fmt.Sprintf("%s.%s.%s.%d", networkPolicy.Namespace, networkPolicy.Name, "ingress", i))
		n.addNamespaceSelectorCmdsForGress(fexec, networkPolicy, "ingress", i)
		fexec.AddFakeCmdsNoOutputNoError([]string{
			fmt.Sprintf("ovn-nbctl --timeout=15 set address_set %s addresses=\"%s\"", hashedOVNName, pod.podIP),
		})
	}
	for i := range networkPolicy.Spec.Egress {
		hashedOVNName := hashedAddressSet(fmt.Sprintf("%s.%s.%s.%d", networkPolicy.Namespace, networkPolicy.Name, "egress", i))
		n.addNamespaceSelectorCmdsForGress(fexec, networkPolicy, "egress", i)
		fexec.AddFakeCmdsNoOutputNoError([]string{
			fmt.Sprintf("ovn-nbctl --timeout=15 set address_set %s addresses=\"%s\"", hashedOVNName, pod.podIP),
		})
	}
}

func (n networkPolicyOld) delCmds(fexec *ovntest.FakeExec, pod pod, networkPolicy knet.NetworkPolicy) {
	for i := range networkPolicy.Spec.Ingress {
		hashedOVNName := hashedAddressSet(fmt.Sprintf("%s.%s.%s.%d", networkPolicy.Namespace, networkPolicy.Name, "ingress", i))
		fexec.AddFakeCmdsNoOutputNoError([]string{
			fmt.Sprintf("ovn-nbctl --timeout=15 --if-exists destroy address_set %s", hashedOVNName),
		})
	}
	for i := range networkPolicy.Spec.Egress {
		hashedOVNName := hashedAddressSet(fmt.Sprintf("%s.%s.%s.%d", networkPolicy.Namespace, networkPolicy.Name, "egress", i))
		fexec.AddFakeCmdsNoOutputNoError([]string{
			fmt.Sprintf("ovn-nbctl --timeout=15 --if-exists destroy address_set %s", hashedOVNName),
		})
	}
	fexec.AddFakeCmdsNoOutputNoError([]string{
		fmt.Sprintf("ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find ACL match=\"outport == \\\"%s\\\"\" action=drop external-ids:default-deny-policy-type=Ingress external-ids:namespace=%s external-ids:logical_switch=%s external-ids:logical_port=%s", pod.portName, pod.namespace, pod.nodeName, pod.portName),
	})
	fexec.AddFakeCmdsNoOutputNoError([]string{
		fmt.Sprintf("ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find ACL match=\"inport == \\\"%s\\\"\" action=drop external-ids:default-deny-policy-type=Egress external-ids:namespace=%s external-ids:logical_switch=%s external-ids:logical_port=%s", pod.portName, pod.namespace, pod.nodeName, pod.portName),
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    fmt.Sprintf("ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find ACL external-ids:namespace=%s external-ids:policy=%s", networkPolicy.Namespace, networkPolicy.Name),
		Output: fakeUUID,
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    fmt.Sprintf("ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find logical_switch acls{>=}%s", fakeUUID),
		Output: fakeLogicalSwitch,
	})
	fexec.AddFakeCmdsNoOutputNoError([]string{
		fmt.Sprintf("ovn-nbctl --timeout=15 remove logical_switch %s acls %s", fakeLogicalSwitch, fakeUUID),
	})
}

func (n networkPolicyOld) delPodCmds(fexec *ovntest.FakeExec, pod pod, networkPolicy knet.NetworkPolicy) {
	for i := range networkPolicy.Spec.Ingress {
		hashedOVNName := hashedAddressSet(fmt.Sprintf("%s.%s.%s.%d", networkPolicy.Namespace, networkPolicy.Name, "ingress", i))
		fexec.AddFakeCmdsNoOutputNoError([]string{
			fmt.Sprintf("ovn-nbctl --timeout=15 clear address_set %s addresses", hashedOVNName),
		})
	}
	for i := range networkPolicy.Spec.Egress {
		hashedOVNName := hashedAddressSet(fmt.Sprintf("%s.%s.%s.%d", networkPolicy.Namespace, networkPolicy.Name, "egress", i))
		fexec.AddFakeCmdsNoOutputNoError([]string{
			fmt.Sprintf("ovn-nbctl --timeout=15 clear address_set %s addresses", hashedOVNName),
		})
	}
	hashedNamespace := hashedAddressSet(pod.namespace)
	fexec.AddFakeCmdsNoOutputNoError([]string{
		fmt.Sprintf("ovn-nbctl --timeout=15 clear address_set %s addresses", hashedNamespace),
	})
}

func (n networkPolicyOld) delLocalPodCmds(fexec *ovntest.FakeExec, pod pod, networkPolicy knet.NetworkPolicy) {
	fexec.AddFakeCmdsNoOutputNoError([]string{
		fmt.Sprintf("ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find ACL match=\"outport == \\\"%s\\\"\" action=drop external-ids:default-deny-policy-type=Ingress external-ids:namespace=%s external-ids:logical_switch=%s external-ids:logical_port=%s", pod.portName, pod.namespace, pod.nodeName, pod.portName),
	})
	fexec.AddFakeCmdsNoOutputNoError([]string{
		fmt.Sprintf("ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find ACL match=\"inport == \\\"%s\\\"\" action=drop external-ids:default-deny-policy-type=Egress external-ids:namespace=%s external-ids:logical_switch=%s external-ids:logical_port=%s", pod.portName, pod.namespace, pod.nodeName, pod.portName),
	})
	for i := range networkPolicy.Spec.Ingress {
		hashedOVNName := hashedAddressSet(fmt.Sprintf("%s.%s.%s.%d", networkPolicy.Namespace, networkPolicy.Name, "ingress", i))
		n.baseLocalCmdsForGress(fexec, pod, networkPolicy, i, "Ingress", hashedOVNName)
		fexec.AddFakeCmdsNoOutputNoError([]string{
			fmt.Sprintf("ovn-nbctl --timeout=15 clear address_set %s addresses", hashedOVNName),
		})
	}
	for i := range networkPolicy.Spec.Egress {
		hashedOVNName := hashedAddressSet(fmt.Sprintf("%s.%s.%s.%d", networkPolicy.Namespace, networkPolicy.Name, "egress", i))
		n.baseLocalCmdsForGress(fexec, pod, networkPolicy, i, "Egress", hashedOVNName)
		fexec.AddFakeCmdsNoOutputNoError([]string{
			fmt.Sprintf("ovn-nbctl --timeout=15 clear address_set %s addresses", hashedOVNName),
		})
	}
	hashedNamespace := hashedAddressSet(pod.namespace)
	fexec.AddFakeCmdsNoOutputNoError([]string{
		fmt.Sprintf("ovn-nbctl --timeout=15 clear address_set %s addresses", hashedNamespace),
	})
}

var _ = Describe("OVN NetworkPolicy Operations", func() {
	var (
		app     *cli.App
		fakeOvn *FakeOVN
		fExec   *ovntest.FakeExec
	)

	BeforeEach(func() {
		// Restore global default values before each testcase
		config.RestoreDefaultConfig()

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

		fExec = ovntest.NewLooseCompareFakeExec()
		fakeOvn = NewFakeOVN(fExec, false)
	})

	AfterEach(func() {
		fakeOvn.shutdown()
	})

	Context("on startup", func() {

		It("reconciles an existing ingress networkPolicy with a namespace selector", func() {
			app.Action = func(ctx *cli.Context) error {

				npTest := networkPolicyOld{}
				nTest := namespace{}

				namespace1 := *newNamespace("namespace1")
				namespace2 := *newNamespace("namespace2")
				networkPolicy := *newNetworkPolicy("networkpolicy1", namespace1.Name,
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

				nTest.baseCmds(fExec, namespace1, namespace2)
				nTest.addCmds(fExec, namespace1, namespace2)
				npTest.addNamespaceSelectorCmds(fExec, networkPolicy)

				fakeOvn.start(ctx,
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespace1,
							namespace2,
						},
					},
					&knet.NetworkPolicyList{
						Items: []knet.NetworkPolicy{
							networkPolicy,
						},
					},
				)
				fakeOvn.controller.WatchNamespaces()
				fakeOvn.controller.WatchNetworkPolicy()

				_, err := fakeOvn.fakeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).Get(networkPolicy.Name, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				Eventually(fExec.CalledMatchesExpected).Should(BeTrue(), fExec.ErrorDesc)

				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("reconciles an existing gress networkPolicy with a pod selector in its own namespace", func() {
			app.Action = func(ctx *cli.Context) error {

				npTest := networkPolicyOld{}
				nTest := namespace{}

				namespace1 := *newNamespace("namespace1")

				nPodTest := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.4",
					"11:22:33:44:55:66",
					namespace1.Name,
				)
				networkPolicy := *newNetworkPolicy("networkpolicy1", namespace1.Name,
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
				nPodTest.addCmdsForNonExistingPod(fExec)
				nTest.baseCmds(fExec, namespace1)
				nTest.addCmdsWithPods(fExec, nPodTest, namespace1)
				npTest.addLocalPodSelectorCmds(fExec, nPodTest, networkPolicy)

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
							networkPolicy,
						},
					},
				)
				nPodTest.populateLogicalSwitchCache(fakeOvn)
				fakeOvn.controller.WatchPods()
				fakeOvn.controller.WatchNamespaces()
				fakeOvn.controller.WatchNetworkPolicy()

				_, err := fakeOvn.fakeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).Get(networkPolicy.Name, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				Eventually(fExec.CalledMatchesExpected).Should(BeTrue(), fExec.ErrorDesc)

				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("reconciles an existing gress networkPolicy with a pod and namespace selector in another namespace", func() {
			app.Action = func(ctx *cli.Context) error {

				npTest := networkPolicyOld{}
				nTest := namespace{}

				namespace1 := *newNamespace("namespace1")
				namespace2 := *newNamespace("namespace2")

				nPodTest := newTPod(
					"node2",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.4",
					"11:22:33:44:55:66",
					namespace2.Name,
				)
				networkPolicy := *newNetworkPolicy("networkpolicy1", namespace1.Name,
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
				nPodTest.addCmdsForNonExistingPod(fExec)
				nTest.baseCmds(fExec, namespace1, namespace2)
				nTest.addCmds(fExec, namespace1)
				nTest.addCmdsWithPods(fExec, nPodTest, namespace2)
				npTest.addPodAndNamespaceSelectorCmds(fExec, nPodTest, namespace2, networkPolicy)

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
							networkPolicy,
						},
					},
				)
				nPodTest.populateLogicalSwitchCache(fakeOvn)
				fakeOvn.controller.WatchPods()
				fakeOvn.controller.WatchNamespaces()
				fakeOvn.controller.WatchNetworkPolicy()

				_, err := fakeOvn.fakeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).Get(networkPolicy.Name, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				Eventually(fExec.CalledMatchesExpected).Should(BeTrue(), fExec.ErrorDesc)

				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("during execution", func() {

		It("reconciles a deleted namespace referenced by a networkpolicy with a local running pod", func() {
			app.Action = func(ctx *cli.Context) error {

				npTest := networkPolicyOld{}
				nTest := namespace{}

				namespace1 := *newNamespace("namespace1")
				namespace2 := *newNamespace("namespace2")

				nPodTest := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.4",
					"11:22:33:44:55:66",
					namespace1.Name,
				)

				networkPolicy := *newNetworkPolicy("networkpolicy1", namespace1.Name,
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
				nPodTest.addCmdsForNonExistingPod(fExec)
				nTest.baseCmds(fExec, namespace1, namespace2)
				nTest.addCmds(fExec, namespace2)
				nTest.addCmdsWithPods(fExec, nPodTest, namespace1)
				npTest.addLocalPodAndNamespaceSelectorCmds(fExec, nPodTest, networkPolicy, namespace2)

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
							networkPolicy,
						},
					},
				)
				nPodTest.populateLogicalSwitchCache(fakeOvn)
				fakeOvn.controller.WatchPods()
				fakeOvn.controller.WatchNamespaces()
				fakeOvn.controller.WatchNetworkPolicy()

				_, err := fakeOvn.fakeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).Get(networkPolicy.Name, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				Eventually(fExec.CalledMatchesExpected).Should(BeTrue(), fExec.ErrorDesc)

				nTest.delCmds(fExec, namespace2)

				fExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    fmt.Sprintf("ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find ACL match=\"ip4.src == {$a10148211500778908391, $a6953373268003663638} && outport == \\\"%s\\\"\" external-ids:namespace=%s external-ids:policy=%s external-ids:Ingress_num=0 external-ids:policy_type=Ingress external-ids:logical_port=%s", nPodTest.portName, networkPolicy.Namespace, networkPolicy.Name, nPodTest.portName),
					Output: fakeUUID,
				})
				fExec.AddFakeCmdsNoOutputNoError([]string{
					fmt.Sprintf("ovn-nbctl --timeout=15 set acl fake_uuid match=\"ip4.src == {$a10148211500778908391} && outport == \\\"%s\\\"\"", nPodTest.portName),
				})
				fExec.AddFakeCmdsNoOutputNoError([]string{
					fmt.Sprintf("ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find ACL match=\"ip4.dst == {$a6953373268003663638, $a9824637386382239951} && inport == \\\"%s\\\"\" external-ids:namespace=%s external-ids:policy=%s external-ids:Egress_num=0 external-ids:policy_type=Egress external-ids:logical_port=%s", nPodTest.portName, networkPolicy.Namespace, networkPolicy.Name, nPodTest.portName),
				})

				err = fakeOvn.fakeClient.CoreV1().Namespaces().Delete(namespace2.Name, metav1.NewDeleteOptions(0))
				Expect(err).NotTo(HaveOccurred())
				Eventually(fExec.CalledMatchesExpected).Should(BeTrue(), fExec.ErrorDesc)

				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("reconciles a deleted namespace referenced by a networkpolicy", func() {
			app.Action = func(ctx *cli.Context) error {

				npTest := networkPolicyOld{}
				nTest := namespace{}

				namespace1 := *newNamespace("namespace1")
				namespace2 := *newNamespace("namespace2")
				networkPolicy := *newNetworkPolicy("networkpolicy1", namespace1.Name,
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

				nTest.baseCmds(fExec, namespace1, namespace2)
				nTest.addCmds(fExec, namespace1, namespace2)
				npTest.addNamespaceSelectorCmds(fExec, networkPolicy)

				fakeOvn.start(ctx,
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespace1,
							namespace2,
						},
					},
					&knet.NetworkPolicyList{
						Items: []knet.NetworkPolicy{
							networkPolicy,
						},
					},
				)
				fakeOvn.controller.WatchNamespaces()
				fakeOvn.controller.WatchNetworkPolicy()

				_, err := fakeOvn.fakeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).Get(networkPolicy.Name, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				Eventually(fExec.CalledMatchesExpected).Should(BeTrue(), fExec.ErrorDesc)

				nTest.delCmds(fExec, namespace2)

				err = fakeOvn.fakeClient.CoreV1().Namespaces().Delete(namespace2.Name, metav1.NewDeleteOptions(0))
				Expect(err).NotTo(HaveOccurred())
				Eventually(fExec.CalledMatchesExpected).Should(BeTrue(), fExec.ErrorDesc)

				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("reconciles a deleted pod referenced by a networkpolicy in its own namespace", func() {
			app.Action = func(ctx *cli.Context) error {

				npTest := networkPolicyOld{}
				nTest := namespace{}

				namespace1 := *newNamespace("namespace1")

				nPodTest := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.4",
					"11:22:33:44:55:66",
					namespace1.Name,
				)
				networkPolicy := *newNetworkPolicy("networkpolicy1", namespace1.Name,
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
				nPodTest.addCmdsForNonExistingPod(fExec)
				nTest.baseCmds(fExec, namespace1)
				nTest.addCmdsWithPods(fExec, nPodTest, namespace1)
				npTest.addLocalPodSelectorCmds(fExec, nPodTest, networkPolicy)

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
							networkPolicy,
						},
					},
				)
				nPodTest.populateLogicalSwitchCache(fakeOvn)
				fakeOvn.controller.WatchPods()
				fakeOvn.controller.WatchNamespaces()
				fakeOvn.controller.WatchNetworkPolicy()

				_, err := fakeOvn.fakeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).Get(networkPolicy.Name, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				Eventually(fExec.CalledMatchesExpected).Should(BeTrue(), fExec.ErrorDesc)

				nPodTest.delCmds(fExec)
				npTest.delLocalPodCmds(fExec, nPodTest, networkPolicy)

				err = fakeOvn.fakeClient.CoreV1().Pods(nPodTest.namespace).Delete(nPodTest.podName, metav1.NewDeleteOptions(0))
				Expect(err).NotTo(HaveOccurred())
				Eventually(fExec.CalledMatchesExpected).Should(BeTrue(), fExec.ErrorDesc)

				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("reconciles a deleted pod referenced by a networkpolicy in another namespace", func() {
			app.Action = func(ctx *cli.Context) error {

				npTest := networkPolicyOld{}
				nTest := namespace{}

				namespace1 := *newNamespace("namespace1")
				namespace2 := *newNamespace("namespace2")

				nPodTest := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.4",
					"11:22:33:44:55:66",
					namespace2.Name,
				)
				networkPolicy := *newNetworkPolicy("networkpolicy1", namespace1.Name,
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
				nPodTest.addCmdsForNonExistingPod(fExec)
				nTest.baseCmds(fExec, namespace1, namespace2)
				nTest.addCmds(fExec, namespace1)
				nTest.addCmdsWithPods(fExec, nPodTest, namespace2)
				npTest.addPodAndNamespaceSelectorCmds(fExec, nPodTest, namespace2, networkPolicy)

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
							networkPolicy,
						},
					},
				)
				nPodTest.populateLogicalSwitchCache(fakeOvn)
				fakeOvn.controller.WatchPods()
				fakeOvn.controller.WatchNamespaces()
				fakeOvn.controller.WatchNetworkPolicy()

				_, err := fakeOvn.fakeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).Get(networkPolicy.Name, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				Eventually(fExec.CalledMatchesExpected).Should(BeTrue(), fExec.ErrorDesc)

				nPodTest.delCmds(fExec)
				npTest.delPodCmds(fExec, nPodTest, networkPolicy)

				fExec.AddFakeCmdsNoOutputNoError([]string{
					fmt.Sprintf("ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find ACL match=\"outport == \\\"%s\\\"\" action=drop external-ids:default-deny-policy-type=Ingress external-ids:namespace=%s external-ids:logical_switch=%s external-ids:logical_port=%s", nPodTest.portName, nPodTest.namespace, nPodTest.nodeName, nPodTest.portName),
				})
				fExec.AddFakeCmdsNoOutputNoError([]string{
					fmt.Sprintf("ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find ACL match=\"inport == \\\"%s\\\"\" action=drop external-ids:default-deny-policy-type=Egress external-ids:namespace=%s external-ids:logical_switch=%s external-ids:logical_port=%s", nPodTest.portName, nPodTest.namespace, nPodTest.nodeName, nPodTest.portName),
				})

				err = fakeOvn.fakeClient.CoreV1().Pods(nPodTest.namespace).Delete(nPodTest.podName, metav1.NewDeleteOptions(0))
				Expect(err).NotTo(HaveOccurred())
				Eventually(fExec.CalledMatchesExpected).Should(BeTrue(), fExec.ErrorDesc)

				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("reconciles a deleted networkpolicy", func() {
			app.Action = func(ctx *cli.Context) error {

				npTest := networkPolicyOld{}
				nTest := namespace{}

				namespace1 := *newNamespace("namespace1")

				nPodTest := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.4",
					"11:22:33:44:55:66",
					namespace1.Name,
				)
				networkPolicy := *newNetworkPolicy("networkpolicy1", namespace1.Name,
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
				nPodTest.addCmdsForNonExistingPod(fExec)
				nTest.baseCmds(fExec, namespace1)
				nTest.addCmdsWithPods(fExec, nPodTest, namespace1)
				npTest.addLocalPodSelectorCmds(fExec, nPodTest, networkPolicy)

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
							networkPolicy,
						},
					},
				)
				nPodTest.populateLogicalSwitchCache(fakeOvn)
				fakeOvn.controller.WatchPods()
				fakeOvn.controller.WatchNamespaces()
				fakeOvn.controller.WatchNetworkPolicy()

				_, err := fakeOvn.fakeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).Get(networkPolicy.Name, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				Eventually(fExec.CalledMatchesExpected).Should(BeTrue(), fExec.ErrorDesc)

				npTest.delCmds(fExec, nPodTest, networkPolicy)

				err = fakeOvn.fakeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).Delete(networkPolicy.Name, metav1.NewDeleteOptions(0))
				Expect(err).NotTo(HaveOccurred())
				Eventually(fExec.CalledMatchesExpected).Should(BeTrue(), fExec.ErrorDesc)

				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
