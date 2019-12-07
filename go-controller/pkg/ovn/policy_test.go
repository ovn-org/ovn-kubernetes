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
		ObjectMeta: newNetworkPolicyMeta(name, namespace),
		Spec: knet.NetworkPolicySpec{
			PodSelector: podSelector,
			Ingress:     ingress,
			Egress:      egress,
		},
	}
}

func (n networkPolicy) baseCmds(fexec *ovntest.FakeExec, networkPolicy knet.NetworkPolicy) {
	readableGroupName := fmt.Sprintf("%s_%s", networkPolicy.Namespace, networkPolicy.Name)
	hashedGroupName := hashedPortGroup(readableGroupName)
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=external_ids find address_set",
		Output: "",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    fmt.Sprintf("ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find port_group name=%s", hashedGroupName),
		Output: "",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    fmt.Sprintf("ovn-nbctl --timeout=15 create port_group name=%s external-ids:name=%s", hashedGroupName, readableGroupName),
		Output: fakeUUID,
	})
}
func (n networkPolicy) addNamespaceSelectorCmdsForGress(fexec *ovntest.FakeExec, networkPolicy knet.NetworkPolicy, gress string, i int) {
	hashedOVNName := hashedAddressSet(fmt.Sprintf("%s.%s.%s.%d", networkPolicy.Namespace, networkPolicy.Name, gress, i))
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    fmt.Sprintf("ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find address_set name=%s", hashedOVNName),
		Output: "",
	})
	fexec.AddFakeCmdsNoOutputNoError([]string{
		fmt.Sprintf("ovn-nbctl --timeout=15 create address_set name=%s external-ids:name=%s", hashedOVNName, fmt.Sprintf("%s.%s.%s.%v", networkPolicy.Namespace, networkPolicy.Name, gress, i)),
	})
}

func (n networkPolicy) addLocalPodCmds(fexec *ovntest.FakeExec, pod pod) {
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    fmt.Sprintf("ovn-nbctl --timeout=15 --if-exists get logical_switch_port %s _uuid", pod.portName),
		Output: fakeUUID,
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find port_group name=ingressDefaultDeny",
		Output: fakeUUID,
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find ACL match=\"outport == @ingressDefaultDeny\" action=drop external-ids:default-deny-policy-type=Ingress",
		Output: fakeUUID,
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find port_group name=egressDefaultDeny",
		Output: fakeUUID,
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find ACL match=\"inport == @egressDefaultDeny\" action=drop external-ids:default-deny-policy-type=Egress",
		Output: fakeUUID,
	})
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovn-nbctl --timeout=15 --if-exists remove port_group fake_uuid ports fake_uuid -- add port_group fake_uuid ports fake_uuid",
	})
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovn-nbctl --timeout=15 --if-exists remove port_group fake_uuid ports fake_uuid -- add port_group fake_uuid ports fake_uuid",
	})
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovn-nbctl --timeout=15 --if-exists remove port_group fake_uuid ports fake_uuid -- add port_group fake_uuid ports fake_uuid",
	})
}

func (n networkPolicy) addPodSelectorCmds(fexec *ovntest.FakeExec, pod pod, networkPolicy knet.NetworkPolicy, hasLocalPods bool, findAgain bool) {
	n.addNamespaceSelectorCmds(fexec, networkPolicy, findAgain)
	for i := range networkPolicy.Spec.Ingress {
		hashedOVNName := hashedAddressSet(fmt.Sprintf("%s.%s.%s.%d", networkPolicy.Namespace, networkPolicy.Name, "ingress", i))
		fexec.AddFakeCmdsNoOutputNoError([]string{
			fmt.Sprintf("ovn-nbctl --timeout=15 set address_set %s addresses=\"%s\"", hashedOVNName, pod.podIP),
		})
	}
	for i := range networkPolicy.Spec.Egress {
		hashedOVNName := hashedAddressSet(fmt.Sprintf("%s.%s.%s.%d", networkPolicy.Namespace, networkPolicy.Name, "egress", i))
		fexec.AddFakeCmdsNoOutputNoError([]string{
			fmt.Sprintf("ovn-nbctl --timeout=15 set address_set %s addresses=\"%s\"", hashedOVNName, pod.podIP),
		})
	}
	if hasLocalPods {
		n.addLocalPodCmds(fexec, pod)
	}
}

func (n networkPolicy) addNamespaceSelectorCmds(fexec *ovntest.FakeExec, networkPolicy knet.NetworkPolicy, findAgain bool) {
	n.baseCmds(fexec, networkPolicy)
	for i := range networkPolicy.Spec.Ingress {
		n.addNamespaceSelectorCmdsForGress(fexec, networkPolicy, "ingress", i)
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    fmt.Sprintf("ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find ACL external-ids:l4Match=\"None\" external-ids:ipblock_cidr=false external-ids:namespace=%s external-ids:policy=%s external-ids:Ingress_num=%v external-ids:policy_type=Ingress", networkPolicy.Namespace, networkPolicy.Name, i),
			Output: "",
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovn-nbctl --timeout=15 --id=@acl create acl priority=1001 direction=to-lport match=\"ip4.src == {$a10148211500778908391} && outport == @a14195333570786048679\" action=allow-related external-ids:l4Match=\"None\" external-ids:ipblock_cidr=false external-ids:namespace=namespace1 external-ids:policy=networkpolicy1 external-ids:Ingress_num=0 external-ids:policy_type=Ingress -- add port_group fake_uuid acls @acl",
			Output: "",
		})
		if findAgain {
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find ACL match=\"ip4.src == {$a10148211500778908391} && outport == @a14195333570786048679\" external-ids:namespace=namespace1 external-ids:policy=networkpolicy1 external-ids:Ingress_num=0 external-ids:policy_type=Ingress",
				Output: "",
			})
		}
	}
	for i := range networkPolicy.Spec.Egress {
		n.addNamespaceSelectorCmdsForGress(fexec, networkPolicy, "egress", i)
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    fmt.Sprintf("ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find ACL external-ids:l4Match=\"None\" external-ids:ipblock_cidr=false external-ids:namespace=%s external-ids:policy=%s external-ids:Egress_num=%v external-ids:policy_type=Egress", networkPolicy.Namespace, networkPolicy.Name, i),
			Output: "",
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovn-nbctl --timeout=15 --id=@acl create acl priority=1001 direction=to-lport match=\"ip4.dst == {$a9824637386382239951} && inport == @a14195333570786048679\" action=allow external-ids:l4Match=\"None\" external-ids:ipblock_cidr=false external-ids:namespace=namespace1 external-ids:policy=networkpolicy1 external-ids:Egress_num=0 external-ids:policy_type=Egress -- add port_group fake_uuid acls @acl",
			Output: "",
		})
		if findAgain {
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find ACL match=\"ip4.dst == {$a9824637386382239951} && inport == @a14195333570786048679\" external-ids:namespace=namespace1 external-ids:policy=networkpolicy1 external-ids:Egress_num=0 external-ids:policy_type=Egress",
				Output: "",
			})
		}
	}
}

func (n networkPolicy) delCmds(fexec *ovntest.FakeExec, pod pod, networkPolicy knet.NetworkPolicy, withLocal bool) {
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
	if withLocal {
		fexec.AddFakeCmdsNoOutputNoError([]string{
			"ovn-nbctl --timeout=15 --if-exists remove port_group fake_uuid ports fake_uuid",
		})
		fexec.AddFakeCmdsNoOutputNoError([]string{
			"ovn-nbctl --timeout=15 --if-exists remove port_group fake_uuid ports fake_uuid",
		})
	}
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find port_group name=a14195333570786048679",
		Output: fakeUUID,
	})
	fexec.AddFakeCmdsNoOutputNoError([]string{
		fmt.Sprintf("ovn-nbctl --timeout=15 --if-exists destroy port_group %s", fakeUUID),
	})
}

func (n networkPolicy) delPodCmds(fexec *ovntest.FakeExec, networkPolicy knet.NetworkPolicy, withLocal bool) {
	for i := range networkPolicy.Spec.Ingress {
		localPeerPods := fmt.Sprintf("%s.%s.%s.%d", networkPolicy.Namespace, networkPolicy.Name, "ingress", i)
		hashedLocalAddressSet := hashedAddressSet(localPeerPods)
		fexec.AddFakeCmdsNoOutputNoError([]string{
			fmt.Sprintf("ovn-nbctl --timeout=15 clear address_set %s addresses", hashedLocalAddressSet),
		})
	}
	for i := range networkPolicy.Spec.Egress {
		localPeerPods := fmt.Sprintf("%s.%s.%s.%d", networkPolicy.Namespace, networkPolicy.Name, "egress", i)
		hashedLocalAddressSet := hashedAddressSet(localPeerPods)
		fexec.AddFakeCmdsNoOutputNoError([]string{
			fmt.Sprintf("ovn-nbctl --timeout=15 clear address_set %s addresses", hashedLocalAddressSet),
		})
	}
	if withLocal {
		fexec.AddFakeCmdsNoOutputNoError([]string{
			"ovn-nbctl --timeout=15 --if-exists remove port_group fake_uuid ports fake_uuid",
		})
		fexec.AddFakeCmdsNoOutputNoError([]string{
			"ovn-nbctl --timeout=15 --if-exists remove port_group fake_uuid ports fake_uuid",
		})
		fexec.AddFakeCmdsNoOutputNoError([]string{
			"ovn-nbctl --timeout=15 --if-exists remove port_group fake_uuid ports fake_uuid",
		})
	}
}

var _ = Describe("OVN NetworkPolicy Operations", func() {
	var (
		app     *cli.App
		fakeOvn *FakeOVN
	)

	BeforeEach(func() {
		// Restore global default values before each testcase
		config.RestoreDefaultConfig()

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

		fakeOvn = &FakeOVN{}
	})

	AfterEach(func() {
		fakeOvn.shutdown()
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

				fExec := ovntest.NewLooseCompareFakeExec()
				nTest.baseCmds(fExec, namespace1, namespace2)
				nTest.addCmds(fExec, namespace1, namespace2)
				npTest.addNamespaceSelectorCmds(fExec, networkPolicy, true)

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

				fExec := ovntest.NewLooseCompareFakeExec()
				nPodTest.baseCmds(fExec)
				nPodTest.addNodeSetupCmds(fExec)
				nPodTest.addCmdsForNonExistingPod(fExec)
				nTest.baseCmds(fExec, namespace1)
				nTest.addCmdsWithPods(fExec, nPodTest, namespace1)
				npTest.addPodSelectorCmds(fExec, nPodTest, networkPolicy, true, false)

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

				fExec := ovntest.NewLooseCompareFakeExec()
				nPodTest.baseCmds(fExec)
				nPodTest.addNodeSetupCmds(fExec)
				nPodTest.addCmdsForNonExistingPod(fExec)
				nTest.baseCmds(fExec, namespace1, namespace2)
				nTest.addCmds(fExec, namespace1)
				nTest.addCmdsWithPods(fExec, nPodTest, namespace2)
				npTest.addPodSelectorCmds(fExec, nPodTest, networkPolicy, false, false)

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

				npTest := networkPolicy{}
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

				fExec := ovntest.NewLooseCompareFakeExec()
				nPodTest.baseCmds(fExec)
				nPodTest.addNodeSetupCmds(fExec)
				nPodTest.addCmdsForNonExistingPod(fExec)
				nTest.baseCmds(fExec, namespace1, namespace2)
				nTest.addCmds(fExec, namespace2)
				nTest.addCmdsWithPods(fExec, nPodTest, namespace1)
				npTest.addNamespaceSelectorCmds(fExec, networkPolicy, true)
				npTest.addLocalPodCmds(fExec, nPodTest)

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

				fakeOvn.controller.WatchPods()
				fakeOvn.controller.WatchNamespaces()
				fakeOvn.controller.WatchNetworkPolicy()

				_, err := fakeOvn.fakeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).Get(networkPolicy.Name, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				Eventually(fExec.CalledMatchesExpected).Should(BeTrue(), fExec.ErrorDesc)

				nTest.delCmds(fExec, namespace2)

				fExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find ACL match=\"ip4.src == {$a10148211500778908391, $a6953373268003663638} && outport == @a14195333570786048679\" external-ids:namespace=namespace1 external-ids:policy=networkpolicy1 external-ids:Ingress_num=0 external-ids:policy_type=Ingress",
					Output: fakeUUID,
				})
				fExec.AddFakeCmdsNoOutputNoError([]string{
					"ovn-nbctl --timeout=15 set acl fake_uuid match=\"ip4.src == {$a10148211500778908391} && outport == @a14195333570786048679\"",
				})
				fExec.AddFakeCmdsNoOutputNoError([]string{
					"ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find ACL match=\"ip4.dst == {$a6953373268003663638, $a9824637386382239951} && inport == @a14195333570786048679\" external-ids:namespace=namespace1 external-ids:policy=networkpolicy1 external-ids:Egress_num=0 external-ids:policy_type=Egress",
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

				npTest := networkPolicy{}
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

				fExec := ovntest.NewLooseCompareFakeExec()
				nTest.baseCmds(fExec, namespace1, namespace2)
				nTest.addCmds(fExec, namespace1, namespace2)
				npTest.addNamespaceSelectorCmds(fExec, networkPolicy, true)

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

				fakeOvn.controller.WatchNamespaces()
				fakeOvn.controller.WatchNetworkPolicy()

				_, err := fakeOvn.fakeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).Get(networkPolicy.Name, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				Eventually(fExec.CalledMatchesExpected).Should(BeTrue(), fExec.ErrorDesc)

				nTest.delCmds(fExec, namespace2)
				fExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find ACL match=\"ip4.src == {$a10148211500778908391, $a6953373268003663638} && outport == @a14195333570786048679\" external-ids:namespace=namespace1 external-ids:policy=networkpolicy1 external-ids:Ingress_num=0 external-ids:policy_type=Ingress",
					Output: fakeUUID,
				})
				fExec.AddFakeCmdsNoOutputNoError([]string{
					"ovn-nbctl --timeout=15 set acl fake_uuid match=\"ip4.src == {$a10148211500778908391} && outport == @a14195333570786048679\"",
				})
				fExec.AddFakeCmdsNoOutputNoError([]string{
					"ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find ACL match=\"ip4.dst == {$a6953373268003663638, $a9824637386382239951} && inport == @a14195333570786048679\" external-ids:namespace=namespace1 external-ids:policy=networkpolicy1 external-ids:Egress_num=0 external-ids:policy_type=Egress",
				})

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

				fExec := ovntest.NewLooseCompareFakeExec()
				nPodTest.baseCmds(fExec)
				nPodTest.addNodeSetupCmds(fExec)
				nPodTest.addCmdsForNonExistingPod(fExec)
				nTest.baseCmds(fExec, namespace1)
				nTest.addCmdsWithPods(fExec, nPodTest, namespace1)
				npTest.addPodSelectorCmds(fExec, nPodTest, networkPolicy, true, false)

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

				fakeOvn.controller.WatchPods()
				fakeOvn.controller.WatchNamespaces()
				fakeOvn.controller.WatchNetworkPolicy()

				_, err := fakeOvn.fakeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).Get(networkPolicy.Name, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				Eventually(fExec.CalledMatchesExpected).Should(BeTrue(), fExec.ErrorDesc)

				nPodTest.delCmds(fExec)
				nPodTest.delFromNamespaceCmds(fExec, nPodTest, true)
				npTest.delPodCmds(fExec, networkPolicy, true)

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

				npTest := networkPolicy{}
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

				fExec := ovntest.NewLooseCompareFakeExec()
				nPodTest.baseCmds(fExec)
				nPodTest.addNodeSetupCmds(fExec)
				nPodTest.addCmdsForNonExistingPod(fExec)
				nTest.baseCmds(fExec, namespace1, namespace2)
				nTest.addCmds(fExec, namespace1)
				nTest.addCmdsWithPods(fExec, nPodTest, namespace2)
				npTest.addPodSelectorCmds(fExec, nPodTest, networkPolicy, false, false)

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

				fakeOvn.controller.WatchPods()
				fakeOvn.controller.WatchNamespaces()
				fakeOvn.controller.WatchNetworkPolicy()

				_, err := fakeOvn.fakeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).Get(networkPolicy.Name, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				Eventually(fExec.CalledMatchesExpected).Should(BeTrue(), fExec.ErrorDesc)

				nPodTest.delCmds(fExec)
				nPodTest.delFromNamespaceCmds(fExec, nPodTest, true)
				npTest.delPodCmds(fExec, networkPolicy, false)

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

				fExec := ovntest.NewLooseCompareFakeExec()
				nPodTest.baseCmds(fExec)
				nPodTest.addNodeSetupCmds(fExec)
				nPodTest.addCmdsForNonExistingPod(fExec)
				nTest.baseCmds(fExec, namespace1)
				nTest.addCmdsWithPods(fExec, nPodTest, namespace1)
				npTest.addPodSelectorCmds(fExec, nPodTest, networkPolicy, true, false)

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

				fakeOvn.controller.WatchPods()
				fakeOvn.controller.WatchNamespaces()
				fakeOvn.controller.WatchNetworkPolicy()

				_, err := fakeOvn.fakeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).Get(networkPolicy.Name, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				Eventually(fExec.CalledMatchesExpected).Should(BeTrue(), fExec.ErrorDesc)

				npTest.delCmds(fExec, nPodTest, networkPolicy, true)

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
