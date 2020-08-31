package ovn

import (
	"context"
	"fmt"
	"net"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	egressipv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/urfave/cli/v2"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var (
	policyID = "policy_id"
	natID    = "nat_id"
)

var _ = Describe("Shared gateway mode EgressIP Operations with", func() {
	var (
		app     *cli.App
		fakeOvn *FakeOVN
		tExec   *ovntest.FakeExec
	)

	getEgressIPAllocatorSizeSafely := func() int {
		fakeOvn.controller.eIPAllocatorMutex.Lock()
		defer fakeOvn.controller.eIPAllocatorMutex.Unlock()
		return len(fakeOvn.controller.eIPAllocator)
	}

	getEgressIPStatusLen := func(egressIPName string) func() int {
		return func() int {
			tmp, err := fakeOvn.fakeEgressIPClient.K8sV1().EgressIPs().Get(context.TODO(), egressIPName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			return len(tmp.Status.Items)
		}
	}

	getEgressIPStatus := func(egressIPName string) []egressipv1.EgressIPStatusItem {
		tmp, err := fakeOvn.fakeEgressIPClient.K8sV1().EgressIPs().Get(context.TODO(), egressIPName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		return tmp.Status.Items
	}

	BeforeEach(func() {
		// Restore global default values before each testcase
		config.PrepareTestConfig()

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

		tExec = ovntest.NewLooseCompareFakeExec()
		fakeOvn = NewFakeOVN(tExec)

		config.Gateway.Mode = config.GatewayModeShared
	})

	AfterEach(func() {
		fakeOvn.shutdown()
	})

	Context("On node UPDATE", func() {

		It("should re-assign EgressIPs and perform proper OVN transactions when pod is created after node egress label switch", func() {
			app.Action = func(ctx *cli.Context) error {

				egressIP := "192.168.126.101"
				node1IPv4 := "192.168.126.202/24"
				node2IPv4 := "192.168.126.51/24"

				egressPod := *newPodWithLabels(namespace, podName, node1Name, podV4IP, egressPodLabel)
				egressNamespace := newNamespace(namespace)

				node1 := v1.Node{ObjectMeta: metav1.ObjectMeta{
					Name: node1Name,
					Annotations: map[string]string{
						"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4, ""),
					},
					Labels: map[string]string{
						"k8s.ovn.org/egress-assignable": "",
					},
				}}
				node2 := v1.Node{ObjectMeta: metav1.ObjectMeta{
					Name: node2Name,
					Annotations: map[string]string{
						"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node2IPv4, ""),
					},
				}}

				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{egressIP},
						PodSelector: metav1.LabelSelector{
							MatchLabels: egressPodLabel,
						},
						NamespaceSelector: metav1.LabelSelector{
							MatchLabels: map[string]string{
								"name": egressNamespace.Name,
							},
						},
					},
					Status: egressipv1.EgressIPStatus{
						Items: []egressipv1.EgressIPStatusItem{},
					},
				}

				fakeOvn.start(ctx,
					&egressipv1.EgressIPList{
						Items: []egressipv1.EgressIP{eIP},
					},
					&v1.NodeList{
						Items: []v1.Node{node1, node2},
					},
					&v1.NamespaceList{
						Items: []v1.Namespace{*egressNamespace},
					})

				fakeOvn.fakeExec.AddFakeCmdsNoOutputNoError(
					[]string{
						fmt.Sprintf("ovn-nbctl --timeout=15 lr-policy-add ovn_cluster_router 101 ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14 allow"),
					},
				)

				fakeOvn.controller.WatchEgressNodes()
				Eventually(getEgressIPAllocatorSizeSafely).Should(Equal(1))
				Expect(fakeOvn.controller.eIPAllocator).To(HaveKey(node1.Name))

				fakeOvn.controller.WatchEgressIP()
				Eventually(getEgressIPStatusLen(egressIPName)).Should(Equal(1))
				statuses := getEgressIPStatus(egressIPName)
				Expect(statuses[0].Node).To(Equal(node1.Name))
				Expect(statuses[0].EgressIP).To(Equal(egressIP))

				node1.Labels = map[string]string{}
				node2.Labels = map[string]string{
					"k8s.ovn.org/egress-assignable": "",
				}

				_, err := fakeOvn.fakeClient.CoreV1().Nodes().Update(context.TODO(), &node1, metav1.UpdateOptions{})
				Expect(err).NotTo(HaveOccurred())
				_, err = fakeOvn.fakeClient.CoreV1().Nodes().Update(context.TODO(), &node2, metav1.UpdateOptions{})
				Expect(err).NotTo(HaveOccurred())

				nodeSwitch := func() string {
					statuses = getEgressIPStatus(egressIPName)
					return statuses[0].Node
				}

				Eventually(getEgressIPStatusLen(egressIPName)).Should(Equal(1))
				Eventually(nodeSwitch).Should(Equal(node2.Name))
				statuses = getEgressIPStatus(egressIPName)
				Expect(statuses[0].EgressIP).To(Equal(egressIP))

				fakeOvn.fakeExec.AddFakeCmd(
					&ovntest.ExpectedCmd{
						Cmd:    fmt.Sprintf("ovn-nbctl --timeout=15 --format=table --no-heading --columns=options find logical_router name=GR_%s options:lb_force_snat_ip!=-", node2.Name),
						Output: fmt.Sprintf(`{ lb_force_snat_ip="%s" }`, nodeLogicalRouterIPv4),
					},
				)
				fakeOvn.fakeExec.AddFakeCmdsNoOutputNoError(
					[]string{
						fmt.Sprintf("ovn-nbctl --timeout=15 --format=csv --data=bare --no-heading --columns=_uuid find logical_router_policy match=\"%s\" priority=%s external_ids:name=%s", fmt.Sprintf("ip4.src == %s", egressPod.Status.PodIP), egressIPReroutePriority, eIP.Name),
						fmt.Sprintf("ovn-nbctl --timeout=15 --id=@lr-policy create logical_router_policy action=reroute match=\"%s\" priority=%s nexthop=%s external_ids:name=%s -- add logical_router %s policies @lr-policy", fmt.Sprintf("ip4.src == %s", egressPod.Status.PodIP), egressIPReroutePriority, nodeLogicalRouterIPv4, eIP.Name, ovnClusterRouter),
						fmt.Sprintf("ovn-nbctl --timeout=15 --format=csv --data=bare --no-heading --columns=_uuid find nat external_ids:name=%s logical_ip=%s external_ip=%s", eIP.Name, egressPod.Status.PodIP, egressIP),
						fmt.Sprintf("ovn-nbctl --timeout=15 --id=@nat create nat type=snat %s %s %s %s -- add logical_router GR_%s nat @nat", fmt.Sprintf("logical_port=k8s-%s", node2.Name), fmt.Sprintf("external_ip=%s", egressIP), fmt.Sprintf("logical_ip=%s", egressPod.Status.PodIP), fmt.Sprintf("external_ids:name=%s", eIP.Name), node2.Name),
					},
				)
				_, err = fakeOvn.fakeClient.CoreV1().Pods(egressPod.Namespace).Create(context.TODO(), &egressPod, metav1.CreateOptions{})

				Eventually(fakeOvn.fakeExec.CalledMatchesExpected).Should(BeTrue(), fakeOvn.fakeExec.ErrorDesc)

				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("should re-assign EgressIPs and perform proper OVN transactions when namespace and pod is created after node egress label switch", func() {
			app.Action = func(ctx *cli.Context) error {

				egressIP := "192.168.126.101"
				node1IPv4 := "192.168.126.202/24"
				node2IPv4 := "192.168.126.51/24"

				egressPod := *newPodWithLabels(namespace, podName, node1Name, podV4IP, egressPodLabel)
				egressNamespace := newNamespace(namespace)

				node1 := v1.Node{ObjectMeta: metav1.ObjectMeta{
					Name: node1Name,
					Annotations: map[string]string{
						"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4, ""),
					},
					Labels: map[string]string{
						"k8s.ovn.org/egress-assignable": "",
					},
				}}
				node2 := v1.Node{ObjectMeta: metav1.ObjectMeta{
					Name: node2Name,
					Annotations: map[string]string{
						"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node2IPv4, ""),
					},
				}}

				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{egressIP},
						PodSelector: metav1.LabelSelector{
							MatchLabels: egressPodLabel,
						},
						NamespaceSelector: metav1.LabelSelector{
							MatchLabels: map[string]string{
								"name": egressNamespace.Name,
							},
						},
					},
					Status: egressipv1.EgressIPStatus{
						Items: []egressipv1.EgressIPStatusItem{},
					},
				}

				fakeOvn.start(ctx,
					&egressipv1.EgressIPList{
						Items: []egressipv1.EgressIP{eIP},
					},
					&v1.NodeList{
						Items: []v1.Node{node1, node2},
					})

				fakeOvn.fakeExec.AddFakeCmdsNoOutputNoError(
					[]string{
						fmt.Sprintf("ovn-nbctl --timeout=15 lr-policy-add ovn_cluster_router 101 ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14 allow"),
					},
				)
				fakeOvn.controller.WatchEgressNodes()
				Eventually(getEgressIPAllocatorSizeSafely).Should(Equal(1))
				Expect(fakeOvn.controller.eIPAllocator).To(HaveKey(node1.Name))

				fakeOvn.controller.WatchEgressIP()
				Eventually(getEgressIPStatusLen(egressIPName)).Should(Equal(1))
				statuses := getEgressIPStatus(egressIPName)
				Expect(statuses[0].Node).To(Equal(node1.Name))
				Expect(statuses[0].EgressIP).To(Equal(egressIP))

				node1.Labels = map[string]string{}
				node2.Labels = map[string]string{
					"k8s.ovn.org/egress-assignable": "",
				}

				_, err := fakeOvn.fakeClient.CoreV1().Nodes().Update(context.TODO(), &node1, metav1.UpdateOptions{})
				Expect(err).NotTo(HaveOccurred())
				_, err = fakeOvn.fakeClient.CoreV1().Nodes().Update(context.TODO(), &node2, metav1.UpdateOptions{})
				Expect(err).NotTo(HaveOccurred())

				nodeSwitch := func() string {
					statuses = getEgressIPStatus(egressIPName)
					return statuses[0].Node
				}

				Eventually(getEgressIPStatusLen(egressIPName)).Should(Equal(1))
				Eventually(nodeSwitch).Should(Equal(node2.Name))
				statuses = getEgressIPStatus(egressIPName)
				Expect(statuses[0].EgressIP).To(Equal(egressIP))

				fakeOvn.fakeExec.AddFakeCmd(
					&ovntest.ExpectedCmd{
						Cmd:    fmt.Sprintf("ovn-nbctl --timeout=15 --format=table --no-heading --columns=options find logical_router name=GR_%s options:lb_force_snat_ip!=-", node2.Name),
						Output: fmt.Sprintf(`{ lb_force_snat_ip="%s" }`, nodeLogicalRouterIPv4),
					},
				)
				fakeOvn.fakeExec.AddFakeCmdsNoOutputNoError(
					[]string{
						fmt.Sprintf("ovn-nbctl --timeout=15 --format=csv --data=bare --no-heading --columns=_uuid find logical_router_policy match=\"%s\" priority=%s external_ids:name=%s", fmt.Sprintf("ip4.src == %s", egressPod.Status.PodIP), egressIPReroutePriority, eIP.Name),
						fmt.Sprintf("ovn-nbctl --timeout=15 --id=@lr-policy create logical_router_policy action=reroute match=\"%s\" priority=%s nexthop=%s external_ids:name=%s -- add logical_router %s policies @lr-policy", fmt.Sprintf("ip4.src == %s", egressPod.Status.PodIP), egressIPReroutePriority, nodeLogicalRouterIPv4, eIP.Name, ovnClusterRouter),
						fmt.Sprintf("ovn-nbctl --timeout=15 --format=csv --data=bare --no-heading --columns=_uuid find nat external_ids:name=%s logical_ip=%s external_ip=%s", eIP.Name, egressPod.Status.PodIP, egressIP),
						fmt.Sprintf("ovn-nbctl --timeout=15 --id=@nat create nat type=snat %s %s %s %s -- add logical_router GR_%s nat @nat", fmt.Sprintf("logical_port=k8s-%s", node2.Name), fmt.Sprintf("external_ip=%s", egressIP), fmt.Sprintf("logical_ip=%s", egressPod.Status.PodIP), fmt.Sprintf("external_ids:name=%s", eIP.Name), node2.Name),
					},
				)
				_, err = fakeOvn.fakeClient.CoreV1().Namespaces().Create(context.TODO(), egressNamespace, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())
				Eventually(func() int {
					fakeOvn.controller.egressIPPodHandlerMutex.Lock()
					defer fakeOvn.controller.egressIPPodHandlerMutex.Unlock()
					return len(fakeOvn.controller.egressIPPodHandlerCache)
				}).Should(Equal(1))
				_, err = fakeOvn.fakeClient.CoreV1().Pods(egressPod.Namespace).Create(context.TODO(), &egressPod, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())
				Eventually(fakeOvn.fakeExec.CalledMatchesExpected).Should(BeTrue(), fakeOvn.fakeExec.ErrorDesc)
				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("IPv6 on pod UPDATE", func() {

		It("should remove OVN pod egress setup when EgressIP stops matching pod label", func() {
			app.Action = func(ctx *cli.Context) error {

				egressIP := net.ParseIP("0:0:0:0:0:feff:c0a8:8e0d")

				egressPod := *newPodWithLabels(namespace, podName, node1Name, podV6IP, egressPodLabel)
				egressNamespace := newNamespace(namespace)
				fakeOvn.start(ctx,
					&v1.NamespaceList{
						Items: []v1.Namespace{*egressNamespace},
					},
					&v1.PodList{
						Items: []v1.Pod{egressPod},
					},
				)
				node1 := setupNode(node1Name, []string{"0:0:0:0:0:feff:c0a8:8e0c/64"}, []string{"0:0:0:0:0:feff:c0a8:8e32", "0:0:0:0:0:feff:c0a8:8e1e"})
				node2 := setupNode(node2Name, []string{"0:0:0:0:0:fedf:c0a8:8e0c/64"}, []string{"0:0:0:0:0:feff:c0a8:8e23"})

				fakeOvn.controller.eIPAllocator[node1.name] = &node1
				fakeOvn.controller.eIPAllocator[node2.name] = &node2

				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{
							egressIP.String(),
						},
						PodSelector: metav1.LabelSelector{
							MatchLabels: egressPodLabel,
						},
						NamespaceSelector: metav1.LabelSelector{
							MatchLabels: map[string]string{
								"name": egressNamespace.Name,
							},
						},
					},
				}

				fakeOvn.fakeExec.AddFakeCmd(
					&ovntest.ExpectedCmd{
						Cmd:    fmt.Sprintf("ovn-nbctl --timeout=15 --format=table --no-heading --columns=options find logical_router name=GR_%s options:lb_force_snat_ip!=-", node2.name),
						Output: fmt.Sprintf(`{ lb_force_snat_ip="%s" }`, nodeLogicalRouterIPv6),
					},
				)
				fakeOvn.fakeExec.AddFakeCmdsNoOutputNoError(
					[]string{
						fmt.Sprintf("ovn-nbctl --timeout=15 --format=csv --data=bare --no-heading --columns=_uuid find logical_router_policy match=\"%s\" priority=%s external_ids:name=%s", fmt.Sprintf("ip6.src == %s", egressPod.Status.PodIP), egressIPReroutePriority, eIP.Name),
						fmt.Sprintf("ovn-nbctl --timeout=15 --id=@lr-policy create logical_router_policy action=reroute match=\"%s\" priority=%s nexthop=%s external_ids:name=%s -- add logical_router %s policies @lr-policy", fmt.Sprintf("ip6.src == %s", egressPod.Status.PodIP), egressIPReroutePriority, nodeLogicalRouterIPv6, eIP.Name, ovnClusterRouter),
						fmt.Sprintf("ovn-nbctl --timeout=15 --format=csv --data=bare --no-heading --columns=_uuid find nat external_ids:name=%s logical_ip=%s external_ip=%s", eIP.Name, egressPod.Status.PodIP, egressIP),
						fmt.Sprintf("ovn-nbctl --timeout=15 --id=@nat create nat type=snat %s %s %s %s -- add logical_router GR_%s nat @nat", fmt.Sprintf("logical_port=k8s-%s", node2.name), fmt.Sprintf("external_ip=%s", egressIP), fmt.Sprintf("logical_ip=%s", egressPod.Status.PodIP), fmt.Sprintf("external_ids:name=%s", eIP.Name), node2.name),
					},
				)

				fakeOvn.controller.WatchEgressIP()

				_, err := fakeOvn.fakeEgressIPClient.K8sV1().EgressIPs().Create(context.TODO(), &eIP, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())

				Eventually(getEgressIPStatusLen(eIP.Name)).Should(Equal(1))
				Eventually(fakeOvn.fakeExec.CalledMatchesExpected).Should(BeTrue(), fakeOvn.fakeExec.ErrorDesc)

				statuses := getEgressIPStatus(eIP.Name)
				Expect(statuses[0].Node).To(Equal(node2.name))
				Expect(statuses[0].EgressIP).To(Equal(egressIP.String()))

				podUpdate := newPod(namespace, podName, node1Name, podV6IP)

				fakeOvn.fakeExec.AddFakeCmd(
					&ovntest.ExpectedCmd{
						Cmd:    fmt.Sprintf("ovn-nbctl --timeout=15 --format=csv --data=bare --no-heading --columns=_uuid find logical_router_policy match=\"%s\" priority=%s external_ids:name=%s", fmt.Sprintf("ip6.src == %s", egressPod.Status.PodIP), egressIPReroutePriority, eIP.Name),
						Output: policyID,
					},
				)
				fakeOvn.fakeExec.AddFakeCmdsNoOutputNoError(
					[]string{
						fmt.Sprintf("ovn-nbctl --timeout=15 remove logical_router %s policies %s", ovnClusterRouter, policyID),
					},
				)
				fakeOvn.fakeExec.AddFakeCmd(
					&ovntest.ExpectedCmd{
						Cmd:    fmt.Sprintf("ovn-nbctl --timeout=15 --format=csv --data=bare --no-heading --columns=_uuid find nat %s %s %s", fmt.Sprintf("external_ids:name=%s", egressIPName), fmt.Sprintf("logical_ip=%s", egressPod.Status.PodIP), fmt.Sprintf("external_ip=%s", egressIP.String())),
						Output: natID,
					},
				)
				fakeOvn.fakeExec.AddFakeCmdsNoOutputNoError(
					[]string{
						fmt.Sprintf("ovn-nbctl --timeout=15 remove logical_router GR_%s nat %s", node2.name, natID),
					},
				)
				_, err = fakeOvn.fakeClient.CoreV1().Pods(egressPod.Namespace).Update(context.TODO(), podUpdate, metav1.UpdateOptions{})
				Expect(err).ToNot(HaveOccurred())
				Eventually(getEgressIPStatusLen(eIP.Name)).Should(Equal(1))
				Eventually(fakeOvn.fakeExec.CalledMatchesExpected).Should(BeTrue(), fakeOvn.fakeExec.ErrorDesc)
				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("should not treat pod update if pod already had assigned IP when it got the ADD", func() {
			app.Action = func(ctx *cli.Context) error {

				egressIP := net.ParseIP("0:0:0:0:0:feff:c0a8:8e0d")

				egressPod := *newPodWithLabels(namespace, podName, node1Name, podV6IP, egressPodLabel)
				egressNamespace := newNamespace(namespace)
				fakeOvn.start(ctx,
					&v1.NamespaceList{
						Items: []v1.Namespace{*egressNamespace},
					},
					&v1.PodList{
						Items: []v1.Pod{egressPod},
					},
				)

				node1 := setupNode(node1Name, []string{"0:0:0:0:0:feff:c0a8:8e0c/64"}, []string{"0:0:0:0:0:feff:c0a8:8e32", "0:0:0:0:0:feff:c0a8:8e1e"})
				node2 := setupNode(node2Name, []string{"0:0:0:0:0:fedf:c0a8:8e0c/64"}, []string{"0:0:0:0:0:feff:c0a8:8e23"})

				fakeOvn.controller.eIPAllocator[node1.name] = &node1
				fakeOvn.controller.eIPAllocator[node2.name] = &node2

				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{
							egressIP.String(),
						},
						PodSelector: metav1.LabelSelector{
							MatchLabels: egressPodLabel,
						},
						NamespaceSelector: metav1.LabelSelector{
							MatchLabels: map[string]string{
								"name": egressNamespace.Name,
							},
						},
					},
				}

				fakeOvn.fakeExec.AddFakeCmd(
					&ovntest.ExpectedCmd{
						Cmd:    fmt.Sprintf("ovn-nbctl --timeout=15 --format=table --no-heading --columns=options find logical_router name=GR_%s options:lb_force_snat_ip!=-", node2.name),
						Output: fmt.Sprintf(`{ lb_force_snat_ip="%s" }`, nodeLogicalRouterIPv6),
					},
				)
				fakeOvn.fakeExec.AddFakeCmdsNoOutputNoError(
					[]string{
						fmt.Sprintf("ovn-nbctl --timeout=15 --format=csv --data=bare --no-heading --columns=_uuid find logical_router_policy match=\"%s\" priority=%s external_ids:name=%s", fmt.Sprintf("ip6.src == %s", egressPod.Status.PodIP), egressIPReroutePriority, eIP.Name),
						fmt.Sprintf("ovn-nbctl --timeout=15 --id=@lr-policy create logical_router_policy action=reroute match=\"%s\" priority=%s nexthop=%s external_ids:name=%s -- add logical_router %s policies @lr-policy", fmt.Sprintf("ip6.src == %s", egressPod.Status.PodIP), egressIPReroutePriority, nodeLogicalRouterIPv6, eIP.Name, ovnClusterRouter),
						fmt.Sprintf("ovn-nbctl --timeout=15 --format=csv --data=bare --no-heading --columns=_uuid find nat external_ids:name=%s logical_ip=%s external_ip=%s", eIP.Name, egressPod.Status.PodIP, egressIP),
						fmt.Sprintf("ovn-nbctl --timeout=15 --id=@nat create nat type=snat %s %s %s %s -- add logical_router GR_%s nat @nat", fmt.Sprintf("logical_port=k8s-%s", node2.name), fmt.Sprintf("external_ip=%s", egressIP), fmt.Sprintf("logical_ip=%s", egressPod.Status.PodIP), fmt.Sprintf("external_ids:name=%s", eIP.Name), node2.name),
					},
				)

				fakeOvn.controller.WatchEgressIP()

				_, err := fakeOvn.fakeEgressIPClient.K8sV1().EgressIPs().Create(context.TODO(), &eIP, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())

				Eventually(getEgressIPStatusLen(eIP.Name)).Should(Equal(1))
				Eventually(fakeOvn.fakeExec.CalledMatchesExpected).Should(BeTrue(), fakeOvn.fakeExec.ErrorDesc)

				statuses := getEgressIPStatus(eIP.Name)
				Expect(statuses[0].Node).To(Equal(node2.name))
				Expect(statuses[0].EgressIP).To(Equal(egressIP.String()))

				podUpdate := newPodWithLabels(namespace, podName, node1Name, podV6IP, map[string]string{
					"egress": "needed",
					"some":   "update",
				})

				_, err = fakeOvn.fakeClient.CoreV1().Pods(egressPod.Namespace).Update(context.TODO(), podUpdate, metav1.UpdateOptions{})
				Expect(err).ToNot(HaveOccurred())
				Eventually(getEgressIPStatusLen(eIP.Name)).Should(Equal(1))
				Eventually(fakeOvn.fakeExec.CalledMatchesExpected).Should(BeTrue(), fakeOvn.fakeExec.ErrorDesc)
				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("should treat pod update if pod did not have an assigned IP when it got the ADD", func() {
			app.Action = func(ctx *cli.Context) error {

				egressIP := net.ParseIP("0:0:0:0:0:feff:c0a8:8e0d")

				egressPod := *newPodWithLabels(namespace, podName, node1Name, "", egressPodLabel)
				egressNamespace := newNamespace(namespace)
				fakeOvn.start(ctx,
					&v1.NamespaceList{
						Items: []v1.Namespace{*egressNamespace},
					},
					&v1.PodList{
						Items: []v1.Pod{egressPod},
					},
				)

				node1 := setupNode(node1Name, []string{"0:0:0:0:0:feff:c0a8:8e0c/64"}, []string{"0:0:0:0:0:feff:c0a8:8e32", "0:0:0:0:0:feff:c0a8:8e1e"})
				node2 := setupNode(node2Name, []string{"0:0:0:0:0:fedf:c0a8:8e0c/64"}, []string{"0:0:0:0:0:feff:c0a8:8e23"})

				fakeOvn.controller.eIPAllocator[node1.name] = &node1
				fakeOvn.controller.eIPAllocator[node2.name] = &node2

				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{
							egressIP.String(),
						},
						PodSelector: metav1.LabelSelector{
							MatchLabels: egressPodLabel,
						},
						NamespaceSelector: metav1.LabelSelector{
							MatchLabels: map[string]string{
								"name": egressNamespace.Name,
							},
						},
					},
				}

				fakeOvn.controller.WatchEgressIP()

				_, err := fakeOvn.fakeEgressIPClient.K8sV1().EgressIPs().Create(context.TODO(), &eIP, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())

				Eventually(getEgressIPStatusLen(eIP.Name)).Should(Equal(1))
				Eventually(fakeOvn.fakeExec.CalledMatchesExpected).Should(BeTrue(), fakeOvn.fakeExec.ErrorDesc)

				statuses := getEgressIPStatus(eIP.Name)
				Expect(statuses[0].Node).To(Equal(node2.name))
				Expect(statuses[0].EgressIP).To(Equal(egressIP.String()))

				podUpdate := newPodWithLabels(namespace, podName, node1Name, podV6IP, egressPodLabel)

				fakeOvn.fakeExec.AddFakeCmd(
					&ovntest.ExpectedCmd{
						Cmd:    fmt.Sprintf("ovn-nbctl --timeout=15 --format=table --no-heading --columns=options find logical_router name=GR_%s options:lb_force_snat_ip!=-", node2.name),
						Output: fmt.Sprintf(`{ lb_force_snat_ip="%s" }`, nodeLogicalRouterIPv6),
					},
				)
				fakeOvn.fakeExec.AddFakeCmdsNoOutputNoError(
					[]string{
						fmt.Sprintf("ovn-nbctl --timeout=15 --format=csv --data=bare --no-heading --columns=_uuid find logical_router_policy match=\"%s\" priority=%s external_ids:name=%s", fmt.Sprintf("ip6.src == %s", podV6IP), egressIPReroutePriority, eIP.Name),
						fmt.Sprintf("ovn-nbctl --timeout=15 --id=@lr-policy create logical_router_policy action=reroute match=\"%s\" priority=%s nexthop=%s external_ids:name=%s -- add logical_router %s policies @lr-policy", fmt.Sprintf("ip6.src == %s", podV6IP), egressIPReroutePriority, nodeLogicalRouterIPv6, eIP.Name, ovnClusterRouter),
						fmt.Sprintf("ovn-nbctl --timeout=15 --format=csv --data=bare --no-heading --columns=_uuid find nat external_ids:name=%s logical_ip=%s external_ip=%s", eIP.Name, podV6IP, egressIP),
						fmt.Sprintf("ovn-nbctl --timeout=15 --id=@nat create nat type=snat %s %s %s %s -- add logical_router GR_%s nat @nat", fmt.Sprintf("logical_port=k8s-%s", node2.name), fmt.Sprintf("external_ip=%s", egressIP), fmt.Sprintf("logical_ip=%s", podV6IP), fmt.Sprintf("external_ids:name=%s", eIP.Name), node2.name),
					},
				)

				_, err = fakeOvn.fakeClient.CoreV1().Pods(egressPod.Namespace).Update(context.TODO(), podUpdate, metav1.UpdateOptions{})
				Expect(err).ToNot(HaveOccurred())
				Eventually(getEgressIPStatusLen(eIP.Name)).Should(Equal(1))
				Eventually(fakeOvn.fakeExec.CalledMatchesExpected).Should(BeTrue(), fakeOvn.fakeExec.ErrorDesc)
				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("should not treat pod DELETE if pod did not have an assigned IP when it got the ADD and we receive a DELETE before the IP UPDATE", func() {
			app.Action = func(ctx *cli.Context) error {

				egressIP := net.ParseIP("0:0:0:0:0:feff:c0a8:8e0d")

				egressPod := *newPodWithLabels(namespace, podName, node1Name, "", egressPodLabel)
				egressNamespace := newNamespace(namespace)
				fakeOvn.start(ctx,
					&v1.NamespaceList{
						Items: []v1.Namespace{*egressNamespace},
					},
					&v1.PodList{
						Items: []v1.Pod{egressPod},
					},
				)
				node1 := setupNode(node1Name, []string{"0:0:0:0:0:feff:c0a8:8e0c/64"}, []string{"0:0:0:0:0:feff:c0a8:8e32", "0:0:0:0:0:feff:c0a8:8e1e"})
				node2 := setupNode(node2Name, []string{"0:0:0:0:0:fedf:c0a8:8e0c/64"}, []string{"0:0:0:0:0:feff:c0a8:8e23"})

				fakeOvn.controller.eIPAllocator[node1.name] = &node1
				fakeOvn.controller.eIPAllocator[node2.name] = &node2

				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{
							egressIP.String(),
						},
						PodSelector: metav1.LabelSelector{
							MatchLabels: egressPodLabel,
						},
						NamespaceSelector: metav1.LabelSelector{
							MatchLabels: map[string]string{
								"name": egressNamespace.Name,
							},
						},
					},
				}

				fakeOvn.controller.WatchEgressIP()

				_, err := fakeOvn.fakeEgressIPClient.K8sV1().EgressIPs().Create(context.TODO(), &eIP, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())

				Eventually(getEgressIPStatusLen(eIP.Name)).Should(Equal(1))
				Eventually(fakeOvn.fakeExec.CalledMatchesExpected).Should(BeTrue(), fakeOvn.fakeExec.ErrorDesc)

				statuses := getEgressIPStatus(eIP.Name)
				Expect(statuses[0].Node).To(Equal(node2.name))
				Expect(statuses[0].EgressIP).To(Equal(egressIP.String()))

				err = fakeOvn.fakeClient.CoreV1().Pods(egressPod.Namespace).Delete(context.TODO(), egressPod.Name, *metav1.NewDeleteOptions(0))
				Expect(err).ToNot(HaveOccurred())
				Eventually(getEgressIPStatusLen(eIP.Name)).Should(Equal(1))
				Eventually(fakeOvn.fakeExec.CalledMatchesExpected).Should(BeTrue(), fakeOvn.fakeExec.ErrorDesc)
				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("IPv6 on namespace UPDATE", func() {

		It("should remove OVN pod egress setup when EgressIP stops matching", func() {
			app.Action = func(ctx *cli.Context) error {

				egressIP := net.ParseIP("0:0:0:0:0:feff:c0a8:8e0d")

				egressPod := *newPodWithLabels(namespace, podName, node1Name, podV6IP, egressPodLabel)
				egressNamespace := newNamespaceWithLabels(namespace, egressPodLabel)
				fakeOvn.start(ctx,
					&v1.NamespaceList{
						Items: []v1.Namespace{*egressNamespace},
					},
					&v1.PodList{
						Items: []v1.Pod{egressPod},
					},
				)
				node1 := setupNode(node1Name, []string{"0:0:0:0:0:feff:c0a8:8e0c/64"}, []string{"0:0:0:0:0:feff:c0a8:8e32", "0:0:0:0:0:feff:c0a8:8e1e"})
				node2 := setupNode(node2Name, []string{"0:0:0:0:0:fedf:c0a8:8e0c/64"}, []string{"0:0:0:0:0:feff:c0a8:8e23"})

				fakeOvn.controller.eIPAllocator[node1.name] = &node1
				fakeOvn.controller.eIPAllocator[node2.name] = &node2

				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{
							egressIP.String(),
						},
						PodSelector: metav1.LabelSelector{
							MatchLabels: egressPodLabel,
						},
						NamespaceSelector: metav1.LabelSelector{
							MatchLabels: egressPodLabel,
						},
					},
				}

				fakeOvn.fakeExec.AddFakeCmd(
					&ovntest.ExpectedCmd{
						Cmd:    fmt.Sprintf("ovn-nbctl --timeout=15 --format=table --no-heading --columns=options find logical_router name=GR_%s options:lb_force_snat_ip!=-", node2.name),
						Output: fmt.Sprintf(`{ lb_force_snat_ip="%s" }`, nodeLogicalRouterIPv6),
					},
				)
				fakeOvn.fakeExec.AddFakeCmdsNoOutputNoError(
					[]string{
						fmt.Sprintf("ovn-nbctl --timeout=15 --format=csv --data=bare --no-heading --columns=_uuid find logical_router_policy match=\"%s\" priority=%s external_ids:name=%s", fmt.Sprintf("ip6.src == %s", egressPod.Status.PodIP), egressIPReroutePriority, eIP.Name),
						fmt.Sprintf("ovn-nbctl --timeout=15 --id=@lr-policy create logical_router_policy action=reroute match=\"%s\" priority=%s nexthop=%s external_ids:name=%s -- add logical_router %s policies @lr-policy", fmt.Sprintf("ip6.src == %s", egressPod.Status.PodIP), egressIPReroutePriority, nodeLogicalRouterIPv6, eIP.Name, ovnClusterRouter),
						fmt.Sprintf("ovn-nbctl --timeout=15 --format=csv --data=bare --no-heading --columns=_uuid find nat external_ids:name=%s logical_ip=%s external_ip=%s", eIP.Name, egressPod.Status.PodIP, egressIP),
						fmt.Sprintf("ovn-nbctl --timeout=15 --id=@nat create nat type=snat %s %s %s %s -- add logical_router GR_%s nat @nat", fmt.Sprintf("logical_port=k8s-%s", node2.name), fmt.Sprintf("external_ip=%s", egressIP), fmt.Sprintf("logical_ip=%s", egressPod.Status.PodIP), fmt.Sprintf("external_ids:name=%s", eIP.Name), node2.name),
					},
				)

				fakeOvn.controller.WatchEgressIP()

				_, err := fakeOvn.fakeEgressIPClient.K8sV1().EgressIPs().Create(context.TODO(), &eIP, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())

				Eventually(getEgressIPStatusLen(eIP.Name)).Should(Equal(1))
				Eventually(fakeOvn.fakeExec.CalledMatchesExpected).Should(BeTrue(), fakeOvn.fakeExec.ErrorDesc)

				statuses := getEgressIPStatus(eIP.Name)
				Expect(statuses[0].Node).To(Equal(node2.name))
				Expect(statuses[0].EgressIP).To(Equal(egressIP.String()))

				namespaceUpdate := newNamespace(namespace)

				fakeOvn.fakeExec.AddFakeCmd(
					&ovntest.ExpectedCmd{
						Cmd:    fmt.Sprintf("ovn-nbctl --timeout=15 --format=csv --data=bare --no-heading --columns=_uuid find logical_router_policy match=\"%s\" priority=%s external_ids:name=%s", fmt.Sprintf("ip6.src == %s", egressPod.Status.PodIP), egressIPReroutePriority, eIP.Name),
						Output: policyID,
					},
				)
				fakeOvn.fakeExec.AddFakeCmdsNoOutputNoError(
					[]string{
						fmt.Sprintf("ovn-nbctl --timeout=15 remove logical_router %s policies %s", ovnClusterRouter, policyID),
					},
				)
				fakeOvn.fakeExec.AddFakeCmd(
					&ovntest.ExpectedCmd{
						Cmd:    fmt.Sprintf("ovn-nbctl --timeout=15 --format=csv --data=bare --no-heading --columns=_uuid find nat %s %s %s", fmt.Sprintf("external_ids:name=%s", egressIPName), fmt.Sprintf("logical_ip=%s", egressPod.Status.PodIP), fmt.Sprintf("external_ip=%s", egressIP.String())),
						Output: natID,
					},
				)
				fakeOvn.fakeExec.AddFakeCmdsNoOutputNoError(
					[]string{
						fmt.Sprintf("ovn-nbctl --timeout=15 remove logical_router GR_%s nat %s", node2.name, natID),
					},
				)

				_, err = fakeOvn.fakeClient.CoreV1().Namespaces().Update(context.TODO(), namespaceUpdate, metav1.UpdateOptions{})
				Expect(err).ToNot(HaveOccurred())
				Eventually(getEgressIPStatusLen(eIP.Name)).Should(Equal(1))
				Eventually(fakeOvn.fakeExec.CalledMatchesExpected).Should(BeTrue(), fakeOvn.fakeExec.ErrorDesc)
				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("should not remove OVN pod egress setup when EgressIP stops matching, but pod never had any IP to begin with", func() {
			app.Action = func(ctx *cli.Context) error {

				egressIP := net.ParseIP("0:0:0:0:0:feff:c0a8:8e0d")

				egressPod := *newPodWithLabels(namespace, podName, node1Name, "", egressPodLabel)
				egressNamespace := newNamespaceWithLabels(namespace, egressPodLabel)
				fakeOvn.start(ctx,
					&v1.NamespaceList{
						Items: []v1.Namespace{*egressNamespace},
					},
					&v1.PodList{
						Items: []v1.Pod{egressPod},
					},
				)

				node1 := setupNode(node1Name, []string{"0:0:0:0:0:feff:c0a8:8e0c/64"}, []string{"0:0:0:0:0:feff:c0a8:8e32", "0:0:0:0:0:feff:c0a8:8e1e"})
				node2 := setupNode(node2Name, []string{"0:0:0:0:0:fedf:c0a8:8e0c/64"}, []string{"0:0:0:0:0:feff:c0a8:8e23"})

				fakeOvn.controller.eIPAllocator[node1.name] = &node1
				fakeOvn.controller.eIPAllocator[node2.name] = &node2

				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta("egressip"),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{
							egressIP.String(),
						},
						PodSelector: metav1.LabelSelector{
							MatchLabels: egressPodLabel,
						},
						NamespaceSelector: metav1.LabelSelector{
							MatchLabels: egressPodLabel,
						},
					},
				}

				fakeOvn.controller.WatchEgressIP()

				_, err := fakeOvn.fakeEgressIPClient.K8sV1().EgressIPs().Create(context.TODO(), &eIP, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())

				Eventually(getEgressIPStatusLen(eIP.Name)).Should(Equal(1))
				Eventually(fakeOvn.fakeExec.CalledMatchesExpected).Should(BeTrue(), fakeOvn.fakeExec.ErrorDesc)

				statuses := getEgressIPStatus(eIP.Name)
				Expect(statuses[0].Node).To(Equal(node2.name))
				Expect(statuses[0].EgressIP).To(Equal(egressIP.String()))

				namespaceUpdate := newNamespace(namespace)

				_, err = fakeOvn.fakeClient.CoreV1().Namespaces().Update(context.TODO(), namespaceUpdate, metav1.UpdateOptions{})
				Expect(err).ToNot(HaveOccurred())
				Eventually(getEgressIPStatusLen(eIP.Name)).Should(Equal(1))
				Eventually(fakeOvn.fakeExec.CalledMatchesExpected).Should(BeTrue(), fakeOvn.fakeExec.ErrorDesc)
				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

	})
	Context("IPv6 on EgressIP UPDATE", func() {

		It("should delete and re-create", func() {
			app.Action = func(ctx *cli.Context) error {

				egressIP := net.ParseIP("0:0:0:0:0:feff:c0a8:8e0d")
				updatedEgressIP := net.ParseIP("0:0:0:0:0:feff:c0a8:8ffd")

				egressPod := *newPodWithLabels(namespace, podName, node1Name, podV6IP, egressPodLabel)
				egressNamespace := newNamespaceWithLabels(namespace, egressPodLabel)
				fakeOvn.start(ctx,
					&v1.NamespaceList{
						Items: []v1.Namespace{*egressNamespace},
					},
					&v1.PodList{
						Items: []v1.Pod{egressPod},
					},
				)
				node1 := setupNode(node1Name, []string{"0:0:0:0:0:feff:c0a8:8e0c/64"}, []string{"0:0:0:0:0:feff:c0a8:8e32", "0:0:0:0:0:feff:c0a8:8e1e"})
				node2 := setupNode(node2Name, []string{"0:0:0:0:0:fedf:c0a8:8e0c/64"}, []string{"0:0:0:0:0:feff:c0a8:8e23"})

				fakeOvn.controller.eIPAllocator[node1.name] = &node1
				fakeOvn.controller.eIPAllocator[node2.name] = &node2

				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{
							egressIP.String(),
						},
						PodSelector: metav1.LabelSelector{
							MatchLabels: egressPodLabel,
						},
						NamespaceSelector: metav1.LabelSelector{
							MatchLabels: egressPodLabel,
						},
					},
				}

				fakeOvn.fakeExec.AddFakeCmd(
					&ovntest.ExpectedCmd{
						Cmd:    fmt.Sprintf("ovn-nbctl --timeout=15 --format=table --no-heading --columns=options find logical_router name=GR_%s options:lb_force_snat_ip!=-", node2.name),
						Output: fmt.Sprintf(`{ lb_force_snat_ip="%s" }`, nodeLogicalRouterIPv6),
					},
				)
				fakeOvn.fakeExec.AddFakeCmdsNoOutputNoError(
					[]string{
						fmt.Sprintf("ovn-nbctl --timeout=15 --format=csv --data=bare --no-heading --columns=_uuid find logical_router_policy match=\"%s\" priority=%s external_ids:name=%s", fmt.Sprintf("ip6.src == %s", egressPod.Status.PodIP), egressIPReroutePriority, eIP.Name),
						fmt.Sprintf("ovn-nbctl --timeout=15 --id=@lr-policy create logical_router_policy action=reroute match=\"%s\" priority=%s nexthop=%s external_ids:name=%s -- add logical_router %s policies @lr-policy", fmt.Sprintf("ip6.src == %s", egressPod.Status.PodIP), egressIPReroutePriority, nodeLogicalRouterIPv6, eIP.Name, ovnClusterRouter),
						fmt.Sprintf("ovn-nbctl --timeout=15 --format=csv --data=bare --no-heading --columns=_uuid find nat external_ids:name=%s logical_ip=%s external_ip=%s", eIP.Name, egressPod.Status.PodIP, egressIP),
						fmt.Sprintf("ovn-nbctl --timeout=15 --id=@nat create nat type=snat %s %s %s %s -- add logical_router GR_%s nat @nat", fmt.Sprintf("logical_port=k8s-%s", node2.name), fmt.Sprintf("external_ip=%s", egressIP), fmt.Sprintf("logical_ip=%s", egressPod.Status.PodIP), fmt.Sprintf("external_ids:name=%s", eIP.Name), node2.name),
					},
				)

				fakeOvn.controller.WatchEgressIP()

				_, err := fakeOvn.fakeEgressIPClient.K8sV1().EgressIPs().Create(context.TODO(), &eIP, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())

				Eventually(getEgressIPStatusLen(eIP.Name)).Should(Equal(1))
				Eventually(fakeOvn.fakeExec.CalledMatchesExpected).Should(BeTrue(), fakeOvn.fakeExec.ErrorDesc)

				statuses := getEgressIPStatus(eIP.Name)
				Expect(statuses[0].Node).To(Equal(node2.name))
				Expect(statuses[0].EgressIP).To(Equal(egressIP.String()))

				eIPUpdate, err := fakeOvn.fakeEgressIPClient.K8sV1().EgressIPs().Get(context.TODO(), eIP.Name, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())

				eIPUpdate.Spec = egressipv1.EgressIPSpec{
					EgressIPs: []string{
						updatedEgressIP.String(),
					},
					PodSelector: metav1.LabelSelector{
						MatchLabels: egressPodLabel,
					},
					NamespaceSelector: metav1.LabelSelector{
						MatchLabels: egressPodLabel,
					},
				}
				fakeOvn.fakeExec.AddFakeCmd(
					&ovntest.ExpectedCmd{
						Cmd:    fmt.Sprintf("ovn-nbctl --timeout=15 --format=csv --data=bare --no-heading --columns=_uuid find logical_router_policy match=\"%s\" priority=%s external_ids:name=%s", fmt.Sprintf("ip6.src == %s", egressPod.Status.PodIP), egressIPReroutePriority, eIP.Name),
						Output: policyID,
					},
				)
				fakeOvn.fakeExec.AddFakeCmdsNoOutputNoError(
					[]string{
						fmt.Sprintf("ovn-nbctl --timeout=15 remove logical_router %s policies %s", ovnClusterRouter, policyID),
					},
				)
				fakeOvn.fakeExec.AddFakeCmd(
					&ovntest.ExpectedCmd{
						Cmd:    fmt.Sprintf("ovn-nbctl --timeout=15 --format=csv --data=bare --no-heading --columns=_uuid find nat %s %s %s", fmt.Sprintf("external_ids:name=%s", egressIPName), fmt.Sprintf("logical_ip=%s", egressPod.Status.PodIP), fmt.Sprintf("external_ip=%s", egressIP.String())),
						Output: natID,
					},
				)
				fakeOvn.fakeExec.AddFakeCmdsNoOutputNoError(
					[]string{
						fmt.Sprintf("ovn-nbctl --timeout=15 remove logical_router GR_%s nat %s", node2.name, natID),
					},
				)
				fakeOvn.fakeExec.AddFakeCmdsNoOutputNoError(
					[]string{
						fmt.Sprintf("ovn-nbctl --timeout=15 --format=csv --data=bare --no-heading --columns=_uuid find logical_router_policy match=\"%s\" priority=%s external_ids:name=%s", fmt.Sprintf("ip6.src == %s", egressPod.Status.PodIP), egressIPReroutePriority, eIP.Name),
						fmt.Sprintf("ovn-nbctl --timeout=15 --id=@lr-policy create logical_router_policy action=reroute match=\"%s\" priority=%s nexthop=%s external_ids:name=%s -- add logical_router %s policies @lr-policy", fmt.Sprintf("ip6.src == %s", egressPod.Status.PodIP), egressIPReroutePriority, nodeLogicalRouterIPv6, eIP.Name, ovnClusterRouter),
						fmt.Sprintf("ovn-nbctl --timeout=15 --format=csv --data=bare --no-heading --columns=_uuid find nat external_ids:name=%s logical_ip=%s external_ip=%s", eIP.Name, egressPod.Status.PodIP, updatedEgressIP.String()),
						fmt.Sprintf("ovn-nbctl --timeout=15 --id=@nat create nat type=snat %s %s %s %s -- add logical_router GR_%s nat @nat", fmt.Sprintf("logical_port=k8s-%s", node2.name), fmt.Sprintf("external_ip=%s", updatedEgressIP.String()), fmt.Sprintf("logical_ip=%s", egressPod.Status.PodIP), fmt.Sprintf("external_ids:name=%s", eIP.Name), node2.name),
					},
				)

				_, err = fakeOvn.fakeEgressIPClient.K8sV1().EgressIPs().Update(context.TODO(), eIPUpdate, metav1.UpdateOptions{})
				Expect(err).ToNot(HaveOccurred())
				Eventually(fakeOvn.fakeExec.CalledMatchesExpected).Should(BeTrue(), fakeOvn.fakeExec.ErrorDesc)
				Eventually(getEgressIPStatusLen(eIP.Name)).Should(Equal(1))

				statuses = getEgressIPStatus(eIP.Name)
				Expect(statuses[0].Node).To(Equal(node2.name))
				Expect(statuses[0].EgressIP).To(Equal(updatedEgressIP.String()))
				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("should not do anyting for user defined status updates", func() {
			app.Action = func(ctx *cli.Context) error {

				egressIP := net.ParseIP("0:0:0:0:0:feff:c0a8:8e0d")

				egressPod := *newPodWithLabels(namespace, podName, node1Name, podV6IP, egressPodLabel)
				egressNamespace := newNamespaceWithLabels(namespace, egressPodLabel)
				fakeOvn.start(ctx,
					&v1.NamespaceList{
						Items: []v1.Namespace{*egressNamespace},
					},
					&v1.PodList{
						Items: []v1.Pod{egressPod},
					},
				)
				node1 := setupNode(node1Name, []string{"0:0:0:0:0:feff:c0a8:8e0c/64"}, []string{"0:0:0:0:0:feff:c0a8:8e32", "0:0:0:0:0:feff:c0a8:8e1e"})
				node2 := setupNode(node2Name, []string{"0:0:0:0:0:fedf:c0a8:8e0c/64"}, []string{"0:0:0:0:0:feff:c0a8:8e23"})

				fakeOvn.controller.eIPAllocator[node1.name] = &node1
				fakeOvn.controller.eIPAllocator[node2.name] = &node2

				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{
							egressIP.String(),
						},
						PodSelector: metav1.LabelSelector{
							MatchLabels: egressPodLabel,
						},
						NamespaceSelector: metav1.LabelSelector{
							MatchLabels: egressPodLabel,
						},
					},
				}

				fakeOvn.fakeExec.AddFakeCmd(
					&ovntest.ExpectedCmd{
						Cmd:    fmt.Sprintf("ovn-nbctl --timeout=15 --format=table --no-heading --columns=options find logical_router name=GR_%s options:lb_force_snat_ip!=-", node2.name),
						Output: fmt.Sprintf(`{ lb_force_snat_ip="%s" }`, nodeLogicalRouterIPv6),
					},
				)
				fakeOvn.fakeExec.AddFakeCmdsNoOutputNoError(
					[]string{
						fmt.Sprintf("ovn-nbctl --timeout=15 --format=csv --data=bare --no-heading --columns=_uuid find logical_router_policy match=\"%s\" priority=%s external_ids:name=%s", fmt.Sprintf("ip6.src == %s", egressPod.Status.PodIP), egressIPReroutePriority, eIP.Name),
						fmt.Sprintf("ovn-nbctl --timeout=15 --id=@lr-policy create logical_router_policy action=reroute match=\"%s\" priority=%s nexthop=%s external_ids:name=%s -- add logical_router %s policies @lr-policy", fmt.Sprintf("ip6.src == %s", egressPod.Status.PodIP), egressIPReroutePriority, nodeLogicalRouterIPv6, eIP.Name, ovnClusterRouter),
						fmt.Sprintf("ovn-nbctl --timeout=15 --format=csv --data=bare --no-heading --columns=_uuid find nat external_ids:name=%s logical_ip=%s external_ip=%s", eIP.Name, egressPod.Status.PodIP, egressIP.String()),
						fmt.Sprintf("ovn-nbctl --timeout=15 --id=@nat create nat type=snat %s %s %s %s -- add logical_router GR_%s nat @nat", fmt.Sprintf("logical_port=k8s-%s", node2.name), fmt.Sprintf("external_ip=%s", egressIP.String()), fmt.Sprintf("logical_ip=%s", egressPod.Status.PodIP), fmt.Sprintf("external_ids:name=%s", eIP.Name), node2.name),
					},
				)

				fakeOvn.controller.WatchEgressIP()

				_, err := fakeOvn.fakeEgressIPClient.K8sV1().EgressIPs().Create(context.TODO(), &eIP, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())

				Eventually(getEgressIPStatusLen(eIP.Name)).Should(Equal(1))
				Eventually(fakeOvn.fakeExec.CalledMatchesExpected).Should(BeTrue(), fakeOvn.fakeExec.ErrorDesc)

				statuses := getEgressIPStatus(eIP.Name)
				Expect(statuses[0].Node).To(Equal(node2.name))
				Expect(statuses[0].EgressIP).To(Equal(egressIP.String()))

				bogusNode := "BOOOOGUUUUUS"
				bogusIP := "192.168.126.9"

				eIPUpdate, err := fakeOvn.fakeEgressIPClient.K8sV1().EgressIPs().Get(context.TODO(), eIP.Name, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())

				eIPUpdate.Status = egressipv1.EgressIPStatus{
					Items: []egressipv1.EgressIPStatusItem{
						{
							EgressIP: bogusIP,
							Node:     bogusNode,
						},
					},
				}

				_, err = fakeOvn.fakeEgressIPClient.K8sV1().EgressIPs().Update(context.TODO(), eIPUpdate, metav1.UpdateOptions{})
				Expect(err).NotTo(HaveOccurred())
				Eventually(fakeOvn.fakeExec.CalledMatchesExpected).Should(BeTrue(), fakeOvn.fakeExec.ErrorDesc)
				Eventually(getEgressIPStatusLen(eIP.Name)).Should(Equal(1))

				statuses = getEgressIPStatus(eIP.Name)
				Expect(statuses[0].Node).To(Equal(bogusNode))
				Expect(statuses[0].EgressIP).To(Equal(bogusIP))

				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
	})

})
