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

type networkPolicy struct{}

func newNetworkPolicyMeta(name, namespace string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		UID:       types.UID(namespace),
		Name:      name,
		Namespace: namespace,
		Labels: map[string]string{
			"name": name,
		},
	}
}

func newNetworkPolicy(name, namespace string, podSelector metav1.LabelSelector, ingress []knet.NetworkPolicyIngressRule, egress []knet.NetworkPolicyEgressRule) *knet.NetworkPolicy {
	return &knet.NetworkPolicy{
		ObjectMeta: newServiceMeta(name, namespace),
		Spec: knet.NetworkPolicySpec{
			PodSelector: podSelector,
			Ingress:     ingress,
			Egress:      egress,
		},
	}
}

func (s networkPolicy) baseCmds(fexec *ovntest.FakeExec, networkPolicy knet.NetworkPolicy) {
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=external_ids find address_set",
		Output: "",
	})
}
func (s networkPolicy) addNamespaceSelectorCmdsForGress(fexec *ovntest.FakeExec, networkPolicy knet.NetworkPolicy, gress string, i int) {
	hashedOVNName := hashedAddressSet(fmt.Sprintf("%s.%s.%s.%d", networkPolicy.Namespace, networkPolicy.Name, gress, i))
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    fmt.Sprintf("ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find address_set name=%s", hashedOVNName),
		Output: "",
	})
	fexec.AddFakeCmdsNoOutputNoError([]string{
		fmt.Sprintf("ovn-nbctl --timeout=15 create address_set name=%s external-ids:name=%s", hashedOVNName, fmt.Sprintf("%s.%s.%s.%v", networkPolicy.Namespace, networkPolicy.Name, gress, i)),
	})
}

func (s networkPolicy) addNamespaceSelectorCmds(fexec *ovntest.FakeExec, networkPolicy knet.NetworkPolicy) {
	s.baseCmds(fexec, networkPolicy)
	for i := range networkPolicy.Spec.Ingress {
		s.addNamespaceSelectorCmdsForGress(fexec, networkPolicy, "ingress", i)
	}
	for i := range networkPolicy.Spec.Egress {
		s.addNamespaceSelectorCmdsForGress(fexec, networkPolicy, "egress", i)
	}
}

func (s networkPolicy) addPodSelectorCmdsForGress(fexec *ovntest.FakeExec, pod pod, networkPolicy knet.NetworkPolicy, i int, gress string, hashName string) {
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

func (s networkPolicy) addLocalPodSelectorCmdsForGress(fexec *ovntest.FakeExec, pod pod, networkPolicy knet.NetworkPolicy, i int, gress string, hashName string) {
	fexec.AddFakeCmdsNoOutputNoError([]string{
		fmt.Sprintf("ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find ACL external-ids:l4Match=\"None\" external-ids:ipblock_cidr=false external-ids:namespace=%s external-ids:policy=%s external-ids:%s_num=%v external-ids:policy_type=%s external-ids:logical_switch=%s external-ids:logical_port=%s", pod.namespace, networkPolicy.Name, gress, i, gress, pod.nodeName, pod.portName),
	})
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

func (s networkPolicy) addLocalPodSelectorCmds(fexec *ovntest.FakeExec, pod pod, networkPolicy knet.NetworkPolicy) {
	s.baseCmds(fexec, networkPolicy)
	for i := range networkPolicy.Spec.Ingress {
		s.addNamespaceSelectorCmdsForGress(fexec, networkPolicy, "ingress", i)
		hashedOVNName := hashedAddressSet(fmt.Sprintf("%s.%s.%s.%d", networkPolicy.Namespace, networkPolicy.Name, "ingress", i))
		fexec.AddFakeCmdsNoOutputNoError([]string{
			fmt.Sprintf("ovn-nbctl --timeout=15 set address_set %s addresses=\"%s\"", hashedOVNName, pod.podIP),
		})
	}
	for i := range networkPolicy.Spec.Egress {
		s.addNamespaceSelectorCmdsForGress(fexec, networkPolicy, "egress", i)
		hashedOVNName := hashedAddressSet(fmt.Sprintf("%s.%s.%s.%d", networkPolicy.Namespace, networkPolicy.Name, "egress", i))
		fexec.AddFakeCmdsNoOutputNoError([]string{
			fmt.Sprintf("ovn-nbctl --timeout=15 set address_set %s addresses=\"%s\"", hashedOVNName, pod.podIP),
		})
	}
	for i := range networkPolicy.Spec.Ingress {
		hashedOVNName := hashedAddressSet(fmt.Sprintf("%s.%s.%s.%d", networkPolicy.Namespace, networkPolicy.Name, "ingress", i))
		s.addPodSelectorCmdsForGress(fexec, pod, networkPolicy, i, "Ingress", hashedOVNName)
	}
	for i := range networkPolicy.Spec.Egress {
		hashedOVNName := hashedAddressSet(fmt.Sprintf("%s.%s.%s.%d", networkPolicy.Namespace, networkPolicy.Name, "egress", i))
		s.addPodSelectorCmdsForGress(fexec, pod, networkPolicy, i, "Egress", hashedOVNName)
	}

	for i := range networkPolicy.Spec.Ingress {
		hashedOVNName := hashedAddressSet(fmt.Sprintf("%s.%s.%s.%d", networkPolicy.Namespace, networkPolicy.Name, "ingress", i))
		s.addLocalPodSelectorCmdsForGress(fexec, pod, networkPolicy, i, "Ingress", hashedOVNName)
	}
	for i := range networkPolicy.Spec.Egress {
		hashedOVNName := hashedAddressSet(fmt.Sprintf("%s.%s.%s.%d", networkPolicy.Namespace, networkPolicy.Name, "egress", i))
		s.addLocalPodSelectorCmdsForGress(fexec, pod, networkPolicy, i, "Egress", hashedOVNName)
	}
}

func (s networkPolicy) addPodAndNamespaceSelectorCmds(fexec *ovntest.FakeExec, pod pod, namespace v1.Namespace, networkPolicy knet.NetworkPolicy) {
	s.baseCmds(fexec, networkPolicy)
	for i := range networkPolicy.Spec.Ingress {
		hashedOVNName := hashedAddressSet(fmt.Sprintf("%s.%s.%s.%d", networkPolicy.Namespace, networkPolicy.Name, "ingress", i))
		s.addNamespaceSelectorCmdsForGress(fexec, networkPolicy, "ingress", i)
		fexec.AddFakeCmdsNoOutputNoError([]string{
			fmt.Sprintf("ovn-nbctl --timeout=15 set address_set %s addresses=\"%s\"", hashedOVNName, pod.podIP),
		})
	}
	for i := range networkPolicy.Spec.Egress {
		hashedOVNName := hashedAddressSet(fmt.Sprintf("%s.%s.%s.%d", networkPolicy.Namespace, networkPolicy.Name, "egress", i))
		s.addNamespaceSelectorCmdsForGress(fexec, networkPolicy, "egress", i)
		fexec.AddFakeCmdsNoOutputNoError([]string{
			fmt.Sprintf("ovn-nbctl --timeout=15 set address_set %s addresses=\"%s\"", hashedOVNName, pod.podIP),
		})
	}
}

func (s networkPolicy) delCmds(fexec *ovntest.FakeExec, networkPolicy knet.NetworkPolicy) {
}

var _ = Describe("OVN NetworkPolicy Operations", func() {
	var app *cli.App

	BeforeEach(func() {
		// Restore global default values before each testcase
		config.RestoreDefaultConfig()

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags
	})

	Context("on startup", func() {

		It("reconciles an existing ingress networkPolicy with a namespace selector", func() {
			app.Action = func(ctx *cli.Context) error {

				npTest := networkPolicy{}
				nTest := namespace{}

				namespace1 := *newNamespace("namespace1")
				namespace2 := *newNamespace("namespace2")
				networkPolicy := *newNetworkPolicy("networkpolicy1", namespace1.Name,
					metav1.LabelSelector{},
					[]knet.NetworkPolicyIngressRule{
						knet.NetworkPolicyIngressRule{
							From: []knet.NetworkPolicyPeer{
								knet.NetworkPolicyPeer{
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
						knet.NetworkPolicyEgressRule{
							To: []knet.NetworkPolicyPeer{
								knet.NetworkPolicyPeer{
									NamespaceSelector: &metav1.LabelSelector{
										MatchLabels: map[string]string{
											"name": namespace2.Name,
										},
									},
								},
							},
						},
					})

				fExec := ovntest.NewFakeExec(false)
				nTest.baseCmds(fExec, namespace1, namespace2)
				nTest.addCmds(fExec, namespace1, namespace2)
				npTest.addNamespaceSelectorCmds(fExec, networkPolicy)

				fakeOvn := FakeOVN{}
				fakeOvn.start(ctx, fExec,
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
				fakeOvn.controller.portGroupSupport = false
				fakeOvn.controller.WatchNamespaces()
				fakeOvn.controller.WatchNetworkPolicy()

				_, err := fakeOvn.fakeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).Get(networkPolicy.Name, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				Expect(fExec.CalledMatchesExpected()).To(BeTrue())

				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("reconciles an existing ingress networkPolicy with a pod selector in its own namespace", func() {
			app.Action = func(ctx *cli.Context) error {

				npTest := networkPolicy{}
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
						knet.NetworkPolicyIngressRule{
							From: []knet.NetworkPolicyPeer{
								knet.NetworkPolicyPeer{
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
						knet.NetworkPolicyEgressRule{
							To: []knet.NetworkPolicyPeer{
								knet.NetworkPolicyPeer{
									PodSelector: &metav1.LabelSelector{
										MatchLabels: map[string]string{
											"name": nPodTest.podName,
										},
									},
								},
							},
						},
					})

				fExec := ovntest.NewFakeExec(false)
				nPodTest.baseCmds(fExec)
				nPodTest.addNodeSetupCmds(fExec)
				nPodTest.addCmdsForNonExistingPod(fExec)
				nTest.baseCmds(fExec, namespace1)
				nTest.addCmdsWithPods(fExec, nPodTest, namespace1)
				npTest.addLocalPodSelectorCmds(fExec, nPodTest, networkPolicy)

				fakeOvn := FakeOVN{}
				fakeOvn.start(ctx, fExec,
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
				fakeOvn.controller.portGroupSupport = false
				fakeOvn.controller.WatchPods()
				fakeOvn.controller.WatchNamespaces()
				fakeOvn.controller.WatchNetworkPolicy()

				_, err := fakeOvn.fakeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).Get(networkPolicy.Name, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				Expect(fExec.CalledMatchesExpected()).To(BeTrue())

				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("reconciles an existing gress networkPolicy with a pod and namespace selector in another namespace", func() {
			app.Action = func(ctx *cli.Context) error {

				npTest := networkPolicy{}
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
						knet.NetworkPolicyIngressRule{
							From: []knet.NetworkPolicyPeer{
								knet.NetworkPolicyPeer{
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
						knet.NetworkPolicyEgressRule{
							To: []knet.NetworkPolicyPeer{
								knet.NetworkPolicyPeer{
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

				fExec := ovntest.NewFakeExec(false)
				nPodTest.baseCmds(fExec)
				nPodTest.addNodeSetupCmds(fExec)
				nPodTest.addCmdsForNonExistingPod(fExec)
				nTest.baseCmds(fExec, namespace1, namespace2)
				nTest.addCmds(fExec, namespace1)
				nTest.addCmdsWithPods(fExec, nPodTest, namespace2)
				npTest.addPodAndNamespaceSelectorCmds(fExec, nPodTest, namespace2, networkPolicy)

				fakeOvn := FakeOVN{}
				fakeOvn.start(ctx, fExec,
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
				fakeOvn.controller.portGroupSupport = false
				fakeOvn.controller.WatchPods()
				fakeOvn.controller.WatchNamespaces()
				fakeOvn.controller.WatchNetworkPolicy()

				_, err := fakeOvn.fakeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).Get(networkPolicy.Name, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				Expect(fExec.CalledMatchesExpected()).To(BeTrue())

				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("during execution", func() {

		It("reconciles a deleted namespace referenced by a networkpolicy", func() {
			app.Action = func(ctx *cli.Context) error {

				npTest := networkPolicy{}
				nTest := namespace{}

				namespace1 := *newNamespace("namespace1")
				namespace2 := *newNamespace("namespace2")
				networkPolicy := *newNetworkPolicy("networkpolicy1", namespace1.Name,
					metav1.LabelSelector{},
					[]knet.NetworkPolicyIngressRule{
						knet.NetworkPolicyIngressRule{
							From: []knet.NetworkPolicyPeer{
								knet.NetworkPolicyPeer{
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
						knet.NetworkPolicyEgressRule{
							To: []knet.NetworkPolicyPeer{
								knet.NetworkPolicyPeer{
									NamespaceSelector: &metav1.LabelSelector{
										MatchLabels: map[string]string{
											"name": namespace2.Name,
										},
									},
								},
							},
						},
					})

				fExec := ovntest.NewFakeExec(true)
				nTest.baseCmds(fExec, namespace1, namespace2)
				nTest.addCmds(fExec, namespace1, namespace2)
				npTest.addNamespaceSelectorCmds(fExec, networkPolicy)

				fakeOvn := FakeOVN{}
				fakeOvn.start(ctx, fExec,
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
				fakeOvn.controller.portGroupSupport = false
				fakeOvn.controller.WatchNamespaces()
				fakeOvn.controller.WatchNetworkPolicy()

				_, err := fakeOvn.fakeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).Get(networkPolicy.Name, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				Expect(fExec.CalledMatchesExpected()).To(BeTrue())

				// NOTE to self: should we not delete any networkpolicy created ovn flows?
				nTest.delCmds(fExec, namespace2)

				err = fakeOvn.fakeClient.CoreV1().Namespaces().Delete(namespace2.Name, metav1.NewDeleteOptions(0))
				Expect(err).NotTo(HaveOccurred())
				Eventually(fExec.CalledMatchesExpected).Should(BeTrue())

				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
