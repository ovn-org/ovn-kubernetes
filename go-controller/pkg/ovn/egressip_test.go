package ovn

import (
	"context"
	"fmt"
	"net"

	"github.com/onsi/ginkgo"
	"github.com/onsi/gomega"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	egressipv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/urfave/cli/v2"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	utilnet "k8s.io/utils/net"
)

type fakeEgressIPDialer struct{}

func (f fakeEgressIPDialer) dial(ip net.IP) bool {
	return true
}

var (
	reroutePolicyID           = "reroute_policy_id"
	natID                     = "nat_id"
	nodeLogicalRouterIPv6     = "fef0::56"
	nodeLogicalRouterIPv4     = "100.64.0.2"
	nodeLogicalRouterIfAddrV6 = nodeLogicalRouterIPv6 + "/125"
	nodeLogicalRouterIfAddrV4 = nodeLogicalRouterIPv4 + "/29"
)

const (
	namespace       = "egressip-namespace"
	nodeInternalIP  = "def0::56"
	podV4IP         = "10.128.0.15"
	podV6IP         = "ae70::66"
	v6ClusterSubnet = "ae70::66/64"
	v4ClusterSubnet = "10.128.0.0/14"
	podName         = "egress_pod"
	egressIPName    = "egressip"
)

func newEgressIPMeta(name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		UID:  k8stypes.UID(name),
		Name: name,
		Labels: map[string]string{
			"name": name,
		},
	}
}

var (
	egressPodLabel = map[string]string{"egress": "needed"}
	node1Name      = "node1"
	node2Name      = "node2"
)

func setupNode(nodeName string, ipNets []string, mockAllocationIPs []string) egressNode {
	var v4IP, v6IP net.IP
	var v4Subnet, v6Subnet *net.IPNet
	for _, ipNet := range ipNets {
		ip, net, _ := net.ParseCIDR(ipNet)
		if utilnet.IsIPv6CIDR(net) {
			v6Subnet = net
			v6IP = ip
		} else {
			v4Subnet = net
			v4IP = ip
		}
	}

	mockAllcations := map[string]bool{}
	for _, mockAllocationIP := range mockAllocationIPs {
		mockAllcations[net.ParseIP(mockAllocationIP).String()] = true
	}

	node := egressNode{
		v4IP:               v4IP,
		v6IP:               v6IP,
		v4Subnet:           v4Subnet,
		v6Subnet:           v6Subnet,
		allocations:        mockAllcations,
		name:               nodeName,
		isReady:            true,
		isReachable:        true,
		isEgressAssignable: true,
	}
	return node
}

var _ = ginkgo.Describe("OVN master EgressIP Operations", func() {
	var (
		app     *cli.App
		fakeOvn *FakeOVN
		tExec   *ovntest.FakeExec
	)

	dialer = fakeEgressIPDialer{}

	getEgressIPAllocatorSizeSafely := func() int {
		fakeOvn.controller.eIPC.allocatorMutex.Lock()
		defer fakeOvn.controller.eIPC.allocatorMutex.Unlock()
		return len(fakeOvn.controller.eIPC.allocator)
	}

	getEgressIPStatusLen := func(egressIPName string) func() int {
		return func() int {
			tmp, err := fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Get(context.TODO(), egressIPName, metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			return len(tmp.Status.Items)
		}
	}

	getEgressIPStatus := func(egressIPName string) []egressipv1.EgressIPStatusItem {
		tmp, err := fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Get(context.TODO(), egressIPName, metav1.GetOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		return tmp.Status.Items
	}

	getEgressIPReassignmentCount := func() int {
		fakeOvn.controller.eIPC.assignmentRetryMutex.Lock()
		defer fakeOvn.controller.eIPC.assignmentRetryMutex.Unlock()
		return len(fakeOvn.controller.eIPC.assignmentRetry)
	}

	isEgressAssignableNode := func(nodeName string) func() bool {
		return func() bool {
			fakeOvn.controller.eIPC.allocatorMutex.Lock()
			defer fakeOvn.controller.eIPC.allocatorMutex.Unlock()
			if item, exists := fakeOvn.controller.eIPC.allocator[nodeName]; exists {
				return item.isEgressAssignable
			}
			return false
		}
	}

	nodeSwitch := func() string {
		statuses := getEgressIPStatus(egressIPName)
		if len(statuses) != 1 {
			return ""
		}
		return statuses[0].Node
	}

	ginkgo.BeforeEach(func() {
		// Restore global default values before each testcase
		config.PrepareTestConfig()
		config.OVNKubernetesFeature.EnableEgressIP = true

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

		tExec = ovntest.NewLooseCompareFakeExec()
		fakeOvn = NewFakeOVN(tExec)

	})

	ginkgo.AfterEach(func() {
		fakeOvn.shutdown()
	})

	ginkgo.Context("On node UPDATE", func() {

		ginkgo.It("should re-assign EgressIPs and perform proper OVN transactions when pod is created after node egress label switch", func() {
			app.Action = func(ctx *cli.Context) error {

				egressIP := "192.168.126.101"
				node1IPv4 := "192.168.126.202/24"
				node2IPv4 := "192.168.126.51/24"

				egressPod := *newPodWithLabels(namespace, podName, node1Name, podV4IP, egressPodLabel)
				egressNamespace := newNamespace(namespace)

				node1 := v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: node1Name,
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4, ""),
						},
						Labels: map[string]string{
							"k8s.ovn.org/egress-assignable": "",
						},
					},
					Status: v1.NodeStatus{
						Conditions: []v1.NodeCondition{
							{
								Type:   v1.NodeReady,
								Status: v1.ConditionTrue,
							},
						},
					},
				}
				node2 := v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: node2Name,
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node2IPv4, ""),
						},
					},
					Status: v1.NodeStatus{
						Conditions: []v1.NodeCondition{
							{
								Type:   v1.NodeReady,
								Status: v1.ConditionTrue,
							},
						},
					},
				}

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
						fmt.Sprintf("ovn-nbctl --timeout=15 set logical_switch_port etor-GR_node1 options:nat-addresses=router"),
					},
				)

				fakeOvn.controller.WatchEgressNodes()
				gomega.Eventually(getEgressIPAllocatorSizeSafely).Should(gomega.Equal(2))
				gomega.Expect(fakeOvn.controller.eIPC.allocator).To(gomega.HaveKey(node1.Name))
				gomega.Expect(fakeOvn.controller.eIPC.allocator).To(gomega.HaveKey(node2.Name))
				gomega.Eventually(isEgressAssignableNode(node1.Name)).Should(gomega.BeTrue())
				gomega.Eventually(isEgressAssignableNode(node2.Name)).Should(gomega.BeFalse())

				fakeOvn.controller.WatchEgressIP()
				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
				statuses := getEgressIPStatus(egressIPName)
				gomega.Expect(statuses[0].Node).To(gomega.Equal(node1.Name))
				gomega.Expect(statuses[0].EgressIP).To(gomega.Equal(egressIP))

				node1.Labels = map[string]string{}
				node2.Labels = map[string]string{
					"k8s.ovn.org/egress-assignable": "",
				}

				fakeOvn.fakeExec.AddFakeCmdsNoOutputNoError(
					[]string{
						fmt.Sprintf("ovn-nbctl --timeout=15 remove logical_switch_port etor-GR_node1 options nat-addresses=router"),
						fmt.Sprintf("ovn-nbctl --timeout=15 set logical_switch_port etor-GR_node2 options:nat-addresses=router"),
					},
				)

				_, err := fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), &node1, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), &node2, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
				gomega.Eventually(nodeSwitch).Should(gomega.Equal(node2.Name))
				statuses = getEgressIPStatus(egressIPName)
				gomega.Expect(statuses[0].EgressIP).To(gomega.Equal(egressIP))

				fakeOvn.fakeExec.AddFakeCmd(
					&ovntest.ExpectedCmd{
						Cmd:    fmt.Sprintf("ovn-nbctl --timeout=15 --if-exist get logical_router_port rtoj-GR_%s networks", node2.Name),
						Output: nodeLogicalRouterIfAddrV4,
					},
				)
				fakeOvn.fakeExec.AddFakeCmdsNoOutputNoError(
					[]string{
						fmt.Sprintf("ovn-nbctl --timeout=15 --format=csv --data=bare --no-heading --columns=_uuid find logical_router_policy match=\"%s\" priority=%s external_ids:name=%s nexthop=%s", fmt.Sprintf("ip4.src == %s", egressPod.Status.PodIP), types.EgressIPReroutePriority, eIP.Name, nodeLogicalRouterIPv4),
						fmt.Sprintf("ovn-nbctl --timeout=15 --id=@lr-policy create logical_router_policy action=reroute match=\"%s\" priority=%s nexthop=%s external_ids:name=%s -- add logical_router %s policies @lr-policy", fmt.Sprintf("ip4.src == %s", egressPod.Status.PodIP), types.EgressIPReroutePriority, nodeLogicalRouterIPv4, eIP.Name, types.OVNClusterRouter),
						fmt.Sprintf("ovn-nbctl --timeout=15 --format=csv --data=bare --no-heading --columns=_uuid find nat external_ids:name=%s logical_ip=%s external_ip=%s", eIP.Name, egressPod.Status.PodIP, egressIP),
						fmt.Sprintf("ovn-nbctl --timeout=15 --id=@nat create nat type=snat %s %s %s %s -- add logical_router GR_%s nat @nat", fmt.Sprintf("logical_port=k8s-%s", node2.Name), fmt.Sprintf("external_ip=%s", egressIP), fmt.Sprintf("logical_ip=%s", egressPod.Status.PodIP), fmt.Sprintf("external_ids:name=%s", eIP.Name), node2.Name),
					},
				)
				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(egressPod.Namespace).Create(context.TODO(), &egressPod, metav1.CreateOptions{})

				gomega.Eventually(fakeOvn.fakeExec.CalledMatchesExpected).Should(gomega.BeTrue(), fakeOvn.fakeExec.ErrorDesc)

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should re-assign EgressIPs and perform proper OVN transactions when namespace and pod is created after node egress label switch", func() {
			app.Action = func(ctx *cli.Context) error {

				egressIP := "192.168.126.101"
				node1IPv4 := "192.168.126.202/24"
				node2IPv4 := "192.168.126.51/24"

				egressPod := *newPodWithLabels(namespace, podName, node1Name, podV4IP, egressPodLabel)
				egressNamespace := newNamespace(namespace)

				node1 := v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: node1Name,
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4, ""),
						},
						Labels: map[string]string{
							"k8s.ovn.org/egress-assignable": "",
						},
					},
					Status: v1.NodeStatus{
						Conditions: []v1.NodeCondition{
							{
								Type:   v1.NodeReady,
								Status: v1.ConditionTrue,
							},
						},
					},
				}
				node2 := v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: node2Name,
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node2IPv4, ""),
						},
					},
					Status: v1.NodeStatus{
						Conditions: []v1.NodeCondition{
							{
								Type:   v1.NodeReady,
								Status: v1.ConditionTrue,
							},
						},
					},
				}

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
						fmt.Sprintf("ovn-nbctl --timeout=15 set logical_switch_port etor-GR_node1 options:nat-addresses=router"),
					},
				)
				fakeOvn.controller.WatchEgressNodes()
				gomega.Eventually(getEgressIPAllocatorSizeSafely).Should(gomega.Equal(2))
				gomega.Expect(fakeOvn.controller.eIPC.allocator).To(gomega.HaveKey(node1.Name))
				gomega.Expect(fakeOvn.controller.eIPC.allocator).To(gomega.HaveKey(node2.Name))
				gomega.Eventually(isEgressAssignableNode(node1.Name)).Should(gomega.BeTrue())
				gomega.Eventually(isEgressAssignableNode(node2.Name)).Should(gomega.BeFalse())

				fakeOvn.controller.WatchEgressIP()
				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
				statuses := getEgressIPStatus(egressIPName)
				gomega.Expect(statuses[0].Node).To(gomega.Equal(node1.Name))
				gomega.Expect(statuses[0].EgressIP).To(gomega.Equal(egressIP))

				node1.Labels = map[string]string{}
				node2.Labels = map[string]string{
					"k8s.ovn.org/egress-assignable": "",
				}

				fakeOvn.fakeExec.AddFakeCmdsNoOutputNoError(
					[]string{
						fmt.Sprintf("ovn-nbctl --timeout=15 remove logical_switch_port etor-GR_node1 options nat-addresses=router"),
						fmt.Sprintf("ovn-nbctl --timeout=15 set logical_switch_port etor-GR_node2 options:nat-addresses=router"),
					},
				)

				_, err := fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), &node1, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), &node2, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
				gomega.Eventually(nodeSwitch).Should(gomega.Equal(node2.Name))
				statuses = getEgressIPStatus(egressIPName)
				gomega.Expect(statuses[0].EgressIP).To(gomega.Equal(egressIP))

				fakeOvn.fakeExec.AddFakeCmd(
					&ovntest.ExpectedCmd{
						Cmd:    fmt.Sprintf("ovn-nbctl --timeout=15 --if-exist get logical_router_port rtoj-GR_%s networks", node2.Name),
						Output: nodeLogicalRouterIfAddrV4,
					},
				)
				fakeOvn.fakeExec.AddFakeCmdsNoOutputNoError(
					[]string{
						fmt.Sprintf("ovn-nbctl --timeout=15 --format=csv --data=bare --no-heading --columns=_uuid find logical_router_policy match=\"%s\" priority=%s external_ids:name=%s nexthop=%s", fmt.Sprintf("ip4.src == %s", egressPod.Status.PodIP), types.EgressIPReroutePriority, eIP.Name, nodeLogicalRouterIPv4),
						fmt.Sprintf("ovn-nbctl --timeout=15 --id=@lr-policy create logical_router_policy action=reroute match=\"%s\" priority=%s nexthop=%s external_ids:name=%s -- add logical_router %s policies @lr-policy", fmt.Sprintf("ip4.src == %s", egressPod.Status.PodIP), types.EgressIPReroutePriority, nodeLogicalRouterIPv4, eIP.Name, types.OVNClusterRouter),
						fmt.Sprintf("ovn-nbctl --timeout=15 --format=csv --data=bare --no-heading --columns=_uuid find nat external_ids:name=%s logical_ip=%s external_ip=%s", eIP.Name, egressPod.Status.PodIP, egressIP),
						fmt.Sprintf("ovn-nbctl --timeout=15 --id=@nat create nat type=snat %s %s %s %s -- add logical_router GR_%s nat @nat", fmt.Sprintf("logical_port=k8s-%s", node2.Name), fmt.Sprintf("external_ip=%s", egressIP), fmt.Sprintf("logical_ip=%s", egressPod.Status.PodIP), fmt.Sprintf("external_ids:name=%s", eIP.Name), node2.Name),
					},
				)
				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Create(context.TODO(), egressNamespace, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(func() int {
					fakeOvn.controller.eIPC.podHandlerMutex.Lock()
					defer fakeOvn.controller.eIPC.podHandlerMutex.Unlock()
					return len(fakeOvn.controller.eIPC.podHandlerCache)
				}).Should(gomega.Equal(1))
				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(egressPod.Namespace).Create(context.TODO(), &egressPod, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(fakeOvn.fakeExec.CalledMatchesExpected).Should(gomega.BeTrue(), fakeOvn.fakeExec.ErrorDesc)
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})

	ginkgo.Context("IPv6 on pod UPDATE", func() {

		ginkgo.It("should remove OVN pod egress setup when EgressIP stops matching pod label", func() {
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

				fakeOvn.controller.eIPC.allocator[node1.name] = &node1
				fakeOvn.controller.eIPC.allocator[node2.name] = &node2

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
						Cmd:    fmt.Sprintf("ovn-nbctl --timeout=15 --if-exist get logical_router_port rtoj-GR_%s networks", node2.name),
						Output: nodeLogicalRouterIfAddrV6,
					},
				)
				fakeOvn.fakeExec.AddFakeCmdsNoOutputNoError(
					[]string{
						fmt.Sprintf("ovn-nbctl --timeout=15 --format=csv --data=bare --no-heading --columns=_uuid find logical_router_policy match=\"%s\" priority=%s external_ids:name=%s nexthop=%s", fmt.Sprintf("ip6.src == %s", egressPod.Status.PodIP), types.EgressIPReroutePriority, eIP.Name, nodeLogicalRouterIPv6),
						fmt.Sprintf("ovn-nbctl --timeout=15 --id=@lr-policy create logical_router_policy action=reroute match=\"%s\" priority=%s nexthop=%s external_ids:name=%s -- add logical_router %s policies @lr-policy", fmt.Sprintf("ip6.src == %s", egressPod.Status.PodIP), types.EgressIPReroutePriority, nodeLogicalRouterIPv6, eIP.Name, types.OVNClusterRouter),
						fmt.Sprintf("ovn-nbctl --timeout=15 --format=csv --data=bare --no-heading --columns=_uuid find nat external_ids:name=%s logical_ip=%s external_ip=%s", eIP.Name, egressPod.Status.PodIP, egressIP),
						fmt.Sprintf("ovn-nbctl --timeout=15 --id=@nat create nat type=snat %s %s %s %s -- add logical_router GR_%s nat @nat", fmt.Sprintf("logical_port=k8s-%s", node2.name), fmt.Sprintf("external_ip=%s", egressIP), fmt.Sprintf("logical_ip=%s", egressPod.Status.PodIP), fmt.Sprintf("external_ids:name=%s", eIP.Name), node2.name),
					},
				)

				fakeOvn.controller.WatchEgressIP()

				_, err := fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Create(context.TODO(), &eIP, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(getEgressIPStatusLen(eIP.Name)).Should(gomega.Equal(1))
				gomega.Eventually(fakeOvn.fakeExec.CalledMatchesExpected).Should(gomega.BeTrue(), fakeOvn.fakeExec.ErrorDesc)

				statuses := getEgressIPStatus(eIP.Name)
				gomega.Expect(statuses[0].Node).To(gomega.Equal(node2.name))
				gomega.Expect(statuses[0].EgressIP).To(gomega.Equal(egressIP.String()))

				podUpdate := newPod(namespace, podName, node1Name, podV6IP)

				fakeOvn.fakeExec.AddFakeCmd(
					&ovntest.ExpectedCmd{
						Cmd:    fmt.Sprintf("ovn-nbctl --timeout=15 --format=csv --data=bare --no-heading --columns=_uuid find logical_router_policy match=\"%s\" priority=%s external_ids:name=%s nexthop=%s", fmt.Sprintf("ip6.src == %s", egressPod.Status.PodIP), types.EgressIPReroutePriority, eIP.Name, nodeLogicalRouterIPv6),
						Output: reroutePolicyID,
					},
				)
				fakeOvn.fakeExec.AddFakeCmdsNoOutputNoError(
					[]string{
						fmt.Sprintf("ovn-nbctl --timeout=15 remove logical_router %s policies %s", types.OVNClusterRouter, reroutePolicyID),
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
				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(egressPod.Namespace).Update(context.TODO(), podUpdate, metav1.UpdateOptions{})
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				gomega.Eventually(getEgressIPStatusLen(eIP.Name)).Should(gomega.Equal(1))
				gomega.Eventually(fakeOvn.fakeExec.CalledMatchesExpected).Should(gomega.BeTrue(), fakeOvn.fakeExec.ErrorDesc)
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should not treat pod update if pod already had assigned IP when it got the ADD", func() {
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

				fakeOvn.controller.eIPC.allocator[node1.name] = &node1
				fakeOvn.controller.eIPC.allocator[node2.name] = &node2

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
						Cmd:    fmt.Sprintf("ovn-nbctl --timeout=15 --if-exist get logical_router_port rtoj-GR_%s networks", node2.name),
						Output: nodeLogicalRouterIfAddrV6,
					},
				)
				fakeOvn.fakeExec.AddFakeCmdsNoOutputNoError(
					[]string{
						fmt.Sprintf("ovn-nbctl --timeout=15 --format=csv --data=bare --no-heading --columns=_uuid find logical_router_policy match=\"%s\" priority=%s external_ids:name=%s nexthop=%s", fmt.Sprintf("ip6.src == %s", egressPod.Status.PodIP), types.EgressIPReroutePriority, eIP.Name, nodeLogicalRouterIPv6),
						fmt.Sprintf("ovn-nbctl --timeout=15 --id=@lr-policy create logical_router_policy action=reroute match=\"%s\" priority=%s nexthop=%s external_ids:name=%s -- add logical_router %s policies @lr-policy", fmt.Sprintf("ip6.src == %s", egressPod.Status.PodIP), types.EgressIPReroutePriority, nodeLogicalRouterIPv6, eIP.Name, types.OVNClusterRouter),
						fmt.Sprintf("ovn-nbctl --timeout=15 --format=csv --data=bare --no-heading --columns=_uuid find nat external_ids:name=%s logical_ip=%s external_ip=%s", eIP.Name, egressPod.Status.PodIP, egressIP),
						fmt.Sprintf("ovn-nbctl --timeout=15 --id=@nat create nat type=snat %s %s %s %s -- add logical_router GR_%s nat @nat", fmt.Sprintf("logical_port=k8s-%s", node2.name), fmt.Sprintf("external_ip=%s", egressIP), fmt.Sprintf("logical_ip=%s", egressPod.Status.PodIP), fmt.Sprintf("external_ids:name=%s", eIP.Name), node2.name),
					},
				)

				fakeOvn.controller.WatchEgressIP()

				_, err := fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Create(context.TODO(), &eIP, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(getEgressIPStatusLen(eIP.Name)).Should(gomega.Equal(1))
				gomega.Eventually(fakeOvn.fakeExec.CalledMatchesExpected).Should(gomega.BeTrue(), fakeOvn.fakeExec.ErrorDesc)

				statuses := getEgressIPStatus(eIP.Name)
				gomega.Expect(statuses[0].Node).To(gomega.Equal(node2.name))
				gomega.Expect(statuses[0].EgressIP).To(gomega.Equal(egressIP.String()))

				podUpdate := newPodWithLabels(namespace, podName, node1Name, podV6IP, map[string]string{
					"egress": "needed",
					"some":   "update",
				})

				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(egressPod.Namespace).Update(context.TODO(), podUpdate, metav1.UpdateOptions{})
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				gomega.Eventually(getEgressIPStatusLen(eIP.Name)).Should(gomega.Equal(1))
				gomega.Eventually(fakeOvn.fakeExec.CalledMatchesExpected).Should(gomega.BeTrue(), fakeOvn.fakeExec.ErrorDesc)
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should treat pod update if pod did not have an assigned IP when it got the ADD", func() {
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

				fakeOvn.controller.eIPC.allocator[node1.name] = &node1
				fakeOvn.controller.eIPC.allocator[node2.name] = &node2

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

				_, err := fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Create(context.TODO(), &eIP, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(getEgressIPStatusLen(eIP.Name)).Should(gomega.Equal(1))
				gomega.Eventually(fakeOvn.fakeExec.CalledMatchesExpected).Should(gomega.BeTrue(), fakeOvn.fakeExec.ErrorDesc)

				statuses := getEgressIPStatus(eIP.Name)
				gomega.Expect(statuses[0].Node).To(gomega.Equal(node2.name))
				gomega.Expect(statuses[0].EgressIP).To(gomega.Equal(egressIP.String()))

				podUpdate := newPodWithLabels(namespace, podName, node1Name, podV6IP, egressPodLabel)

				fakeOvn.fakeExec.AddFakeCmd(
					&ovntest.ExpectedCmd{
						Cmd:    fmt.Sprintf("ovn-nbctl --timeout=15 --if-exist get logical_router_port rtoj-GR_%s networks", node2.name),
						Output: nodeLogicalRouterIfAddrV6,
					},
				)
				fakeOvn.fakeExec.AddFakeCmdsNoOutputNoError(
					[]string{
						fmt.Sprintf("ovn-nbctl --timeout=15 --format=csv --data=bare --no-heading --columns=_uuid find logical_router_policy match=\"%s\" priority=%s external_ids:name=%s nexthop=%s", fmt.Sprintf("ip6.src == %s", podV6IP), types.EgressIPReroutePriority, eIP.Name, nodeLogicalRouterIPv6),
						fmt.Sprintf("ovn-nbctl --timeout=15 --id=@lr-policy create logical_router_policy action=reroute match=\"%s\" priority=%s nexthop=%s external_ids:name=%s -- add logical_router %s policies @lr-policy", fmt.Sprintf("ip6.src == %s", podV6IP), types.EgressIPReroutePriority, nodeLogicalRouterIPv6, eIP.Name, types.OVNClusterRouter),
						fmt.Sprintf("ovn-nbctl --timeout=15 --format=csv --data=bare --no-heading --columns=_uuid find nat external_ids:name=%s logical_ip=%s external_ip=%s", eIP.Name, podV6IP, egressIP),
						fmt.Sprintf("ovn-nbctl --timeout=15 --id=@nat create nat type=snat %s %s %s %s -- add logical_router GR_%s nat @nat", fmt.Sprintf("logical_port=k8s-%s", node2.name), fmt.Sprintf("external_ip=%s", egressIP), fmt.Sprintf("logical_ip=%s", podV6IP), fmt.Sprintf("external_ids:name=%s", eIP.Name), node2.name),
					},
				)

				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(egressPod.Namespace).Update(context.TODO(), podUpdate, metav1.UpdateOptions{})
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				gomega.Eventually(getEgressIPStatusLen(eIP.Name)).Should(gomega.Equal(1))
				gomega.Eventually(fakeOvn.fakeExec.CalledMatchesExpected).Should(gomega.BeTrue(), fakeOvn.fakeExec.ErrorDesc)
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should not treat pod DELETE if pod did not have an assigned IP when it got the ADD and we receive a DELETE before the IP UPDATE", func() {
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

				fakeOvn.controller.eIPC.allocator[node1.name] = &node1
				fakeOvn.controller.eIPC.allocator[node2.name] = &node2

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

				_, err := fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Create(context.TODO(), &eIP, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(getEgressIPStatusLen(eIP.Name)).Should(gomega.Equal(1))
				gomega.Eventually(fakeOvn.fakeExec.CalledMatchesExpected).Should(gomega.BeTrue(), fakeOvn.fakeExec.ErrorDesc)

				statuses := getEgressIPStatus(eIP.Name)
				gomega.Expect(statuses[0].Node).To(gomega.Equal(node2.name))
				gomega.Expect(statuses[0].EgressIP).To(gomega.Equal(egressIP.String()))

				err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(egressPod.Namespace).Delete(context.TODO(), egressPod.Name, *metav1.NewDeleteOptions(0))
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				gomega.Eventually(getEgressIPStatusLen(eIP.Name)).Should(gomega.Equal(1))
				gomega.Eventually(fakeOvn.fakeExec.CalledMatchesExpected).Should(gomega.BeTrue(), fakeOvn.fakeExec.ErrorDesc)
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})

	ginkgo.Context("IPv6 on namespace UPDATE", func() {

		ginkgo.It("should remove OVN pod egress setup when EgressIP stops matching", func() {
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

				fakeOvn.controller.eIPC.allocator[node1.name] = &node1
				fakeOvn.controller.eIPC.allocator[node2.name] = &node2

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
						Cmd:    fmt.Sprintf("ovn-nbctl --timeout=15 --if-exist get logical_router_port rtoj-GR_%s networks", node2.name),
						Output: nodeLogicalRouterIfAddrV6,
					},
				)
				fakeOvn.fakeExec.AddFakeCmdsNoOutputNoError(
					[]string{
						fmt.Sprintf("ovn-nbctl --timeout=15 --format=csv --data=bare --no-heading --columns=_uuid find logical_router_policy match=\"%s\" priority=%s external_ids:name=%s nexthop=%s", fmt.Sprintf("ip6.src == %s", egressPod.Status.PodIP), types.EgressIPReroutePriority, eIP.Name, nodeLogicalRouterIPv6),
						fmt.Sprintf("ovn-nbctl --timeout=15 --id=@lr-policy create logical_router_policy action=reroute match=\"%s\" priority=%s nexthop=%s external_ids:name=%s -- add logical_router %s policies @lr-policy", fmt.Sprintf("ip6.src == %s", egressPod.Status.PodIP), types.EgressIPReroutePriority, nodeLogicalRouterIPv6, eIP.Name, types.OVNClusterRouter),
						fmt.Sprintf("ovn-nbctl --timeout=15 --format=csv --data=bare --no-heading --columns=_uuid find nat external_ids:name=%s logical_ip=%s external_ip=%s", eIP.Name, egressPod.Status.PodIP, egressIP),
						fmt.Sprintf("ovn-nbctl --timeout=15 --id=@nat create nat type=snat %s %s %s %s -- add logical_router GR_%s nat @nat", fmt.Sprintf("logical_port=k8s-%s", node2.name), fmt.Sprintf("external_ip=%s", egressIP), fmt.Sprintf("logical_ip=%s", egressPod.Status.PodIP), fmt.Sprintf("external_ids:name=%s", eIP.Name), node2.name),
					},
				)

				fakeOvn.controller.WatchEgressIP()

				_, err := fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Create(context.TODO(), &eIP, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(getEgressIPStatusLen(eIP.Name)).Should(gomega.Equal(1))
				gomega.Eventually(fakeOvn.fakeExec.CalledMatchesExpected).Should(gomega.BeTrue(), fakeOvn.fakeExec.ErrorDesc)

				statuses := getEgressIPStatus(eIP.Name)
				gomega.Expect(statuses[0].Node).To(gomega.Equal(node2.name))
				gomega.Expect(statuses[0].EgressIP).To(gomega.Equal(egressIP.String()))

				namespaceUpdate := newNamespace(namespace)

				fakeOvn.fakeExec.AddFakeCmd(
					&ovntest.ExpectedCmd{
						Cmd:    fmt.Sprintf("ovn-nbctl --timeout=15 --format=csv --data=bare --no-heading --columns=_uuid find logical_router_policy match=\"%s\" priority=%s external_ids:name=%s nexthop=%s", fmt.Sprintf("ip6.src == %s", egressPod.Status.PodIP), types.EgressIPReroutePriority, eIP.Name, nodeLogicalRouterIPv6),
						Output: reroutePolicyID,
					},
				)
				fakeOvn.fakeExec.AddFakeCmdsNoOutputNoError(
					[]string{
						fmt.Sprintf("ovn-nbctl --timeout=15 remove logical_router %s policies %s", types.OVNClusterRouter, reroutePolicyID),
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

				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Update(context.TODO(), namespaceUpdate, metav1.UpdateOptions{})
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				gomega.Eventually(getEgressIPStatusLen(eIP.Name)).Should(gomega.Equal(1))
				gomega.Eventually(fakeOvn.fakeExec.CalledMatchesExpected).Should(gomega.BeTrue(), fakeOvn.fakeExec.ErrorDesc)
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should not remove OVN pod egress setup when EgressIP stops matching, but pod never had any IP to begin with", func() {
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

				fakeOvn.controller.eIPC.allocator[node1.name] = &node1
				fakeOvn.controller.eIPC.allocator[node2.name] = &node2

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

				_, err := fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Create(context.TODO(), &eIP, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(getEgressIPStatusLen(eIP.Name)).Should(gomega.Equal(1))
				gomega.Eventually(fakeOvn.fakeExec.CalledMatchesExpected).Should(gomega.BeTrue(), fakeOvn.fakeExec.ErrorDesc)

				statuses := getEgressIPStatus(eIP.Name)
				gomega.Expect(statuses[0].Node).To(gomega.Equal(node2.name))
				gomega.Expect(statuses[0].EgressIP).To(gomega.Equal(egressIP.String()))

				namespaceUpdate := newNamespace(namespace)

				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Update(context.TODO(), namespaceUpdate, metav1.UpdateOptions{})
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				gomega.Eventually(getEgressIPStatusLen(eIP.Name)).Should(gomega.Equal(1))
				gomega.Eventually(fakeOvn.fakeExec.CalledMatchesExpected).Should(gomega.BeTrue(), fakeOvn.fakeExec.ErrorDesc)
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

	})
	ginkgo.Context("IPv6 on EgressIP UPDATE", func() {

		ginkgo.It("should delete and re-create", func() {
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

				fakeOvn.controller.eIPC.allocator[node1.name] = &node1
				fakeOvn.controller.eIPC.allocator[node2.name] = &node2

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
						Cmd:    fmt.Sprintf("ovn-nbctl --timeout=15 --if-exist get logical_router_port rtoj-GR_%s networks", node2.name),
						Output: nodeLogicalRouterIfAddrV6,
					},
				)
				fakeOvn.fakeExec.AddFakeCmdsNoOutputNoError(
					[]string{
						fmt.Sprintf("ovn-nbctl --timeout=15 --format=csv --data=bare --no-heading --columns=_uuid find logical_router_policy match=\"%s\" priority=%s external_ids:name=%s nexthop=%s", fmt.Sprintf("ip6.src == %s", egressPod.Status.PodIP), types.EgressIPReroutePriority, eIP.Name, nodeLogicalRouterIPv6),
						fmt.Sprintf("ovn-nbctl --timeout=15 --id=@lr-policy create logical_router_policy action=reroute match=\"%s\" priority=%s nexthop=%s external_ids:name=%s -- add logical_router %s policies @lr-policy", fmt.Sprintf("ip6.src == %s", egressPod.Status.PodIP), types.EgressIPReroutePriority, nodeLogicalRouterIPv6, eIP.Name, types.OVNClusterRouter),
						fmt.Sprintf("ovn-nbctl --timeout=15 --format=csv --data=bare --no-heading --columns=_uuid find nat external_ids:name=%s logical_ip=%s external_ip=%s", eIP.Name, egressPod.Status.PodIP, egressIP),
						fmt.Sprintf("ovn-nbctl --timeout=15 --id=@nat create nat type=snat %s %s %s %s -- add logical_router GR_%s nat @nat", fmt.Sprintf("logical_port=k8s-%s", node2.name), fmt.Sprintf("external_ip=%s", egressIP), fmt.Sprintf("logical_ip=%s", egressPod.Status.PodIP), fmt.Sprintf("external_ids:name=%s", eIP.Name), node2.name),
					},
				)

				fakeOvn.controller.WatchEgressIP()

				_, err := fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Create(context.TODO(), &eIP, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(getEgressIPStatusLen(eIP.Name)).Should(gomega.Equal(1))
				gomega.Eventually(fakeOvn.fakeExec.CalledMatchesExpected).Should(gomega.BeTrue(), fakeOvn.fakeExec.ErrorDesc)

				statuses := getEgressIPStatus(eIP.Name)
				gomega.Expect(statuses[0].Node).To(gomega.Equal(node2.name))
				gomega.Expect(statuses[0].EgressIP).To(gomega.Equal(egressIP.String()))

				eIPUpdate, err := fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Get(context.TODO(), eIP.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

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
						Cmd:    fmt.Sprintf("ovn-nbctl --timeout=15 --format=csv --data=bare --no-heading --columns=_uuid find logical_router_policy match=\"%s\" priority=%s external_ids:name=%s nexthop=%s", fmt.Sprintf("ip6.src == %s", egressPod.Status.PodIP), types.EgressIPReroutePriority, eIP.Name, nodeLogicalRouterIPv6),
						Output: reroutePolicyID,
					},
				)
				fakeOvn.fakeExec.AddFakeCmdsNoOutputNoError(
					[]string{
						fmt.Sprintf("ovn-nbctl --timeout=15 remove logical_router %s policies %s", types.OVNClusterRouter, reroutePolicyID),
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
						fmt.Sprintf("ovn-nbctl --timeout=15 --format=csv --data=bare --no-heading --columns=_uuid find logical_router_policy match=\"%s\" priority=%s external_ids:name=%s nexthop=%s", fmt.Sprintf("ip6.src == %s", egressPod.Status.PodIP), types.EgressIPReroutePriority, eIP.Name, nodeLogicalRouterIPv6),
						fmt.Sprintf("ovn-nbctl --timeout=15 --id=@lr-policy create logical_router_policy action=reroute match=\"%s\" priority=%s nexthop=%s external_ids:name=%s -- add logical_router %s policies @lr-policy", fmt.Sprintf("ip6.src == %s", egressPod.Status.PodIP), types.EgressIPReroutePriority, nodeLogicalRouterIPv6, eIP.Name, types.OVNClusterRouter),
						fmt.Sprintf("ovn-nbctl --timeout=15 --format=csv --data=bare --no-heading --columns=_uuid find nat external_ids:name=%s logical_ip=%s external_ip=%s", eIP.Name, egressPod.Status.PodIP, updatedEgressIP.String()),
						fmt.Sprintf("ovn-nbctl --timeout=15 --id=@nat create nat type=snat %s %s %s %s -- add logical_router GR_%s nat @nat", fmt.Sprintf("logical_port=k8s-%s", node2.name), fmt.Sprintf("external_ip=%s", updatedEgressIP.String()), fmt.Sprintf("logical_ip=%s", egressPod.Status.PodIP), fmt.Sprintf("external_ids:name=%s", eIP.Name), node2.name),
					},
				)

				_, err = fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Update(context.TODO(), eIPUpdate, metav1.UpdateOptions{})
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				gomega.Eventually(fakeOvn.fakeExec.CalledMatchesExpected).Should(gomega.BeTrue(), fakeOvn.fakeExec.ErrorDesc)

				gomega.Eventually(func() string {
					statuses = getEgressIPStatus(eIP.Name)
					return statuses[0].EgressIP
				}).Should(gomega.Equal(updatedEgressIP.String()))
				gomega.Expect(statuses[0].Node).To(gomega.Equal(node2.name))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should not do anyting for user defined status updates", func() {
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

				fakeOvn.controller.eIPC.allocator[node1.name] = &node1
				fakeOvn.controller.eIPC.allocator[node2.name] = &node2

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
						Cmd:    fmt.Sprintf("ovn-nbctl --timeout=15 --if-exist get logical_router_port rtoj-GR_%s networks", node2.name),
						Output: nodeLogicalRouterIfAddrV6,
					},
				)
				fakeOvn.fakeExec.AddFakeCmdsNoOutputNoError(
					[]string{
						fmt.Sprintf("ovn-nbctl --timeout=15 --format=csv --data=bare --no-heading --columns=_uuid find logical_router_policy match=\"%s\" priority=%s external_ids:name=%s nexthop=%s", fmt.Sprintf("ip6.src == %s", egressPod.Status.PodIP), types.EgressIPReroutePriority, eIP.Name, nodeLogicalRouterIPv6),
						fmt.Sprintf("ovn-nbctl --timeout=15 --id=@lr-policy create logical_router_policy action=reroute match=\"%s\" priority=%s nexthop=%s external_ids:name=%s -- add logical_router %s policies @lr-policy", fmt.Sprintf("ip6.src == %s", egressPod.Status.PodIP), types.EgressIPReroutePriority, nodeLogicalRouterIPv6, eIP.Name, types.OVNClusterRouter),
						fmt.Sprintf("ovn-nbctl --timeout=15 --format=csv --data=bare --no-heading --columns=_uuid find nat external_ids:name=%s logical_ip=%s external_ip=%s", eIP.Name, egressPod.Status.PodIP, egressIP.String()),
						fmt.Sprintf("ovn-nbctl --timeout=15 --id=@nat create nat type=snat %s %s %s %s -- add logical_router GR_%s nat @nat", fmt.Sprintf("logical_port=k8s-%s", node2.name), fmt.Sprintf("external_ip=%s", egressIP.String()), fmt.Sprintf("logical_ip=%s", egressPod.Status.PodIP), fmt.Sprintf("external_ids:name=%s", eIP.Name), node2.name),
					},
				)

				fakeOvn.controller.WatchEgressIP()

				_, err := fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Create(context.TODO(), &eIP, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(getEgressIPStatusLen(eIP.Name)).Should(gomega.Equal(1))
				gomega.Eventually(fakeOvn.fakeExec.CalledMatchesExpected).Should(gomega.BeTrue(), fakeOvn.fakeExec.ErrorDesc)

				statuses := getEgressIPStatus(eIP.Name)
				gomega.Expect(statuses[0].Node).To(gomega.Equal(node2.name))
				gomega.Expect(statuses[0].EgressIP).To(gomega.Equal(egressIP.String()))

				bogusNode := "BOOOOGUUUUUS"
				bogusIP := "192.168.126.9"

				eIPUpdate, err := fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Get(context.TODO(), eIP.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				eIPUpdate.Status = egressipv1.EgressIPStatus{
					Items: []egressipv1.EgressIPStatusItem{
						{
							EgressIP: bogusIP,
							Node:     bogusNode,
						},
					},
				}

				_, err = fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Update(context.TODO(), eIPUpdate, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(fakeOvn.fakeExec.CalledMatchesExpected).Should(gomega.BeTrue(), fakeOvn.fakeExec.ErrorDesc)
				gomega.Eventually(getEgressIPStatusLen(eIP.Name)).Should(gomega.Equal(1))

				statuses = getEgressIPStatus(eIP.Name)
				gomega.Expect(statuses[0].Node).To(gomega.Equal(bogusNode))
				gomega.Expect(statuses[0].EgressIP).To(gomega.Equal(bogusIP))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})

	ginkgo.Context("WatchEgressNodes", func() {

		ginkgo.It("should populated egress node data as they are tagged `egress assignable` with variants of IPv4/IPv6", func() {
			app.Action = func(ctx *cli.Context) error {

				node1IPv4 := "192.168.128.202/24"
				node1IPv6 := "0:0:0:0:0:feff:c0a8:8e0c/64"
				node2IPv4 := "192.168.126.51/24"

				node1 := v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: "node1",
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4, node1IPv6),
						},
					},
					Status: v1.NodeStatus{
						Conditions: []v1.NodeCondition{
							{
								Type:   v1.NodeReady,
								Status: v1.ConditionTrue,
							},
						},
					},
				}
				node2 := v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: "node2",
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node2IPv4, ""),
						},
					},
					Status: v1.NodeStatus{
						Conditions: []v1.NodeCondition{
							{
								Type:   v1.NodeReady,
								Status: v1.ConditionTrue,
							},
						},
					},
				}
				fakeOvn.start(ctx)
				fakeOvn.fakeExec.AddFakeCmdsNoOutputNoError(
					[]string{
						fmt.Sprintf("ovn-nbctl --timeout=15 lr-policy-add ovn_cluster_router 101 ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14 allow"),
						fmt.Sprintf("ovn-nbctl --timeout=15 set logical_switch_port etor-GR_node1 options:nat-addresses=router"),
						fmt.Sprintf("ovn-nbctl --timeout=15 set logical_switch_port etor-GR_node2 options:nat-addresses=router"),
					},
				)
				fakeOvn.controller.WatchEgressNodes()
				gomega.Eventually(getEgressIPAllocatorSizeSafely).Should(gomega.Equal(0))
				node1.Labels = map[string]string{
					"k8s.ovn.org/egress-assignable": "",
				}

				_, ip1V4Sub, err := net.ParseCIDR(node1IPv4)
				_, ip1V6Sub, err := net.ParseCIDR(node1IPv6)
				_, ip2V4Sub, err := net.ParseCIDR(node2IPv4)

				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Create(context.TODO(), &node1, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(getEgressIPAllocatorSizeSafely).Should(gomega.Equal(1))
				gomega.Expect(fakeOvn.controller.eIPC.allocator).To(gomega.HaveKey(node1.Name))
				gomega.Expect(fakeOvn.controller.eIPC.allocator[node1.Name].v4Subnet).To(gomega.Equal(ip1V4Sub))
				gomega.Expect(fakeOvn.controller.eIPC.allocator[node1.Name].v6Subnet).To(gomega.Equal(ip1V6Sub))

				node2.Labels = map[string]string{
					"k8s.ovn.org/egress-assignable": "",
				}

				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Create(context.TODO(), &node2, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(getEgressIPAllocatorSizeSafely).Should(gomega.Equal(2))
				gomega.Expect(fakeOvn.controller.eIPC.allocator).To(gomega.HaveKey(node1.Name))
				gomega.Expect(fakeOvn.controller.eIPC.allocator).To(gomega.HaveKey(node2.Name))
				gomega.Expect(fakeOvn.controller.eIPC.allocator[node2.Name].v4Subnet).To(gomega.Equal(ip2V4Sub))
				gomega.Expect(fakeOvn.controller.eIPC.allocator[node1.Name].v4Subnet).To(gomega.Equal(ip1V4Sub))
				gomega.Expect(fakeOvn.controller.eIPC.allocator[node1.Name].v6Subnet).To(gomega.Equal(ip1V6Sub))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should skip populating egress node data for nodes that have incorrect IP address", func() {
			app.Action = func(ctx *cli.Context) error {

				nodeIPv4 := "192.168.126.510/24"
				nodeIPv6 := "0:0:0:0:0:feff:c0a8:8e0c/64"
				node := v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: node1Name,
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", nodeIPv4, nodeIPv6),
						},
					},
					Status: v1.NodeStatus{
						Conditions: []v1.NodeCondition{
							{
								Type:   v1.NodeReady,
								Status: v1.ConditionTrue,
							},
						},
					},
				}
				fakeOvn.start(ctx, &v1.NodeList{
					Items: []v1.Node{node},
				})
				fakeOvn.fakeExec.AddFakeCmdsNoOutputNoError(
					[]string{
						fmt.Sprintf("ovn-nbctl --timeout=15 lr-policy-add ovn_cluster_router 101 ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14 allow"),
						fmt.Sprintf("ovn-nbctl --timeout=15 set logical_switch_port etor-GR_node1 options:nat-addresses=router"),
					},
				)

				allocatorItems := func() int {
					return len(fakeOvn.controller.eIPC.allocator)
				}

				fakeOvn.controller.WatchEgressNodes()
				gomega.Eventually(allocatorItems).Should(gomega.Equal(0))

				node.Labels = map[string]string{
					"k8s.ovn.org/egress-assignable": "",
				}

				_, err := fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), &node, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(allocatorItems).Should(gomega.Equal(0))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

	})

	ginkgo.Context("WatchEgressNodes running with WatchEgressIP", func() {

		ginkgo.It("should treat un-assigned EgressIPs when it is tagged", func() {
			app.Action = func(ctx *cli.Context) error {

				egressIP := "192.168.126.101"
				nodeIPv4 := "192.168.126.51/24"
				nodeIPv6 := "0:0:0:0:0:feff:c0a8:8e0c/64"

				node := v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: node1Name,
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", nodeIPv4, nodeIPv6),
						},
					},
					Status: v1.NodeStatus{
						Conditions: []v1.NodeCondition{
							{
								Type:   v1.NodeReady,
								Status: v1.ConditionTrue,
							},
						},
					},
				}

				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{egressIP},
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
						Items: []v1.Node{node},
					})

				fakeOvn.fakeExec.AddFakeCmdsNoOutputNoError(
					[]string{
						fmt.Sprintf("ovn-nbctl --timeout=15 lr-policy-add ovn_cluster_router 101 ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14 allow"),
					},
				)
				fakeOvn.controller.WatchEgressNodes()
				fakeOvn.controller.WatchEgressIP()

				gomega.Eventually(getEgressIPAllocatorSizeSafely).Should(gomega.Equal(1))
				gomega.Eventually(isEgressAssignableNode(node.Name)).Should(gomega.BeFalse())
				gomega.Eventually(eIP.Status.Items).Should(gomega.HaveLen(0))

				node.Labels = map[string]string{
					"k8s.ovn.org/egress-assignable": "",
				}

				_, ipv4Sub, err := net.ParseCIDR(nodeIPv4)
				_, ipv6Sub, err := net.ParseCIDR(nodeIPv6)

				fakeOvn.fakeExec.AddFakeCmdsNoOutputNoError(
					[]string{
						fmt.Sprintf("ovn-nbctl --timeout=15 set logical_switch_port etor-GR_node1 options:nat-addresses=router"),
					},
				)

				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), &node, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
				gomega.Eventually(isEgressAssignableNode(node.Name)).Should(gomega.BeTrue())
				statuses := getEgressIPStatus(egressIPName)
				gomega.Expect(statuses[0].Node).To(gomega.Equal(node.Name))
				gomega.Expect(statuses[0].EgressIP).To(gomega.Equal(egressIP))

				gomega.Expect(fakeOvn.controller.eIPC.allocator).To(gomega.HaveLen(1))
				gomega.Expect(fakeOvn.controller.eIPC.allocator).To(gomega.HaveKey(node.Name))
				gomega.Expect(fakeOvn.controller.eIPC.allocator[node.Name].v4Subnet).To(gomega.Equal(ipv4Sub))
				gomega.Expect(fakeOvn.controller.eIPC.allocator[node.Name].v6Subnet).To(gomega.Equal(ipv6Sub))

				gomega.Eventually(getEgressIPReassignmentCount).Should(gomega.Equal(0))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should result in error and event if specified egress IP is a cluster node IP", func() {
			app.Action = func(ctx *cli.Context) error {

				egressIP := "192.168.126.51"
				node1IPv4 := "192.168.128.202/24"
				node1IPv6 := "0:0:0:0:0:feff:c0a8:8e0c/64"
				node2IPv4 := "192.168.126.51/24"

				node1 := v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: node1Name,
						Labels: map[string]string{
							"k8s.ovn.org/egress-assignable": "",
						},
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4, node1IPv6),
						},
					},
					Status: v1.NodeStatus{
						Conditions: []v1.NodeCondition{
							{
								Type:   v1.NodeReady,
								Status: v1.ConditionTrue,
							},
						},
					},
				}
				node2 := v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: node2Name,
						Labels: map[string]string{
							"k8s.ovn.org/egress-assignable": "",
						},
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node2IPv4, ""),
						},
					},
					Status: v1.NodeStatus{
						Conditions: []v1.NodeCondition{
							{
								Type:   v1.NodeReady,
								Status: v1.ConditionTrue,
							},
						},
					},
				}

				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{egressIP},
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
						fmt.Sprintf("ovn-nbctl --timeout=15 set logical_switch_port etor-GR_node1 options:nat-addresses=router"),
						fmt.Sprintf("ovn-nbctl --timeout=15 set logical_switch_port etor-GR_node2 options:nat-addresses=router"),
					},
				)
				fakeOvn.controller.WatchEgressNodes()
				fakeOvn.controller.WatchEgressIP()

				gomega.Eventually(getEgressIPAllocatorSizeSafely).Should(gomega.Equal(2))
				gomega.Expect(fakeOvn.controller.eIPC.allocator).To(gomega.HaveKey(node1.Name))
				gomega.Expect(fakeOvn.controller.eIPC.allocator).To(gomega.HaveKey(node2.Name))

				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(0))
				recordedEvent := <-fakeOvn.fakeRecorder.Events
				gomega.Expect(recordedEvent).To(gomega.ContainSubstring("Egress IP: %v for object EgressIP: %s is the IP address of node: %s, this is unsupported", egressIP, eIP.Name, node2.Name))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should re-assigned EgressIPs when more nodes get tagged if the first assignment attempt wasn't fully successful", func() {
			app.Action = func(ctx *cli.Context) error {

				egressIP1 := "192.168.126.25"
				egressIP2 := "192.168.126.30"
				node1IPv4 := "192.168.126.51/24"
				node2IPv4 := "192.168.126.101/24"

				node1 := v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: node1Name,
						Labels: map[string]string{
							"k8s.ovn.org/egress-assignable": "",
						},
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\"}", node1IPv4),
						},
					},
					Status: v1.NodeStatus{
						Conditions: []v1.NodeCondition{
							{
								Type:   v1.NodeReady,
								Status: v1.ConditionTrue,
							},
						},
					},
				}
				node2 := v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: node2Name,
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\"}", node2IPv4),
						},
					},
					Status: v1.NodeStatus{
						Conditions: []v1.NodeCondition{
							{
								Type:   v1.NodeReady,
								Status: v1.ConditionTrue,
							},
						},
					},
				}

				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{egressIP1, egressIP2},
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
						fmt.Sprintf("ovn-nbctl --timeout=15 set logical_switch_port etor-GR_node1 options:nat-addresses=router"),
					},
				)
				fakeOvn.controller.WatchEgressNodes()
				fakeOvn.controller.WatchEgressIP()

				gomega.Eventually(getEgressIPAllocatorSizeSafely).Should(gomega.Equal(2))
				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))

				gomega.Eventually(getEgressIPReassignmentCount).Should(gomega.Equal(1))

				recordedEvent := <-fakeOvn.fakeRecorder.Events
				gomega.Expect(recordedEvent).To(gomega.ContainSubstring("Not all egress IPs for EgressIP: %s could be assigned, please tag more nodes", eIP.Name))

				node2.Labels = map[string]string{
					"k8s.ovn.org/egress-assignable": "",
				}
				fakeOvn.fakeExec.AddFakeCmdsNoOutputNoError(
					[]string{
						fmt.Sprintf("ovn-nbctl --timeout=15 set logical_switch_port etor-GR_node2 options:nat-addresses=router"),
					},
				)
				_, err := fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), &node2, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(2))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should only get assigned EgressIPs which matches their subnet when the node is tagged", func() {
			app.Action = func(ctx *cli.Context) error {

				egressIP := "192.168.126.101"
				node1IPv4 := "192.168.128.202/24"
				node1IPv6 := "0:0:0:0:0:feff:c0a8:8e0c/64"
				node2IPv4 := "192.168.126.51/24"

				node1 := v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: node1Name,
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4, node1IPv6),
						},
					},
					Status: v1.NodeStatus{
						Conditions: []v1.NodeCondition{
							{
								Type:   v1.NodeReady,
								Status: v1.ConditionTrue,
							},
						},
					},
				}
				node2 := v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: node2Name,
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node2IPv4, ""),
						},
					},
					Status: v1.NodeStatus{
						Conditions: []v1.NodeCondition{
							{
								Type:   v1.NodeReady,
								Status: v1.ConditionTrue,
							},
						},
					},
				}

				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{egressIP},
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
				fakeOvn.controller.WatchEgressIP()

				_, ip1V4Sub, err := net.ParseCIDR(node1IPv4)
				_, ip1V6Sub, err := net.ParseCIDR(node1IPv6)
				_, ip2V4Sub, err := net.ParseCIDR(node2IPv4)

				gomega.Eventually(getEgressIPAllocatorSizeSafely).Should(gomega.Equal(2))
				gomega.Expect(fakeOvn.controller.eIPC.allocator).To(gomega.HaveKey(node1.Name))
				gomega.Expect(fakeOvn.controller.eIPC.allocator).To(gomega.HaveKey(node2.Name))
				gomega.Eventually(isEgressAssignableNode(node1.Name)).Should(gomega.BeFalse())
				gomega.Eventually(isEgressAssignableNode(node2.Name)).Should(gomega.BeFalse())
				gomega.Expect(fakeOvn.controller.eIPC.allocator[node1.Name].v4Subnet).To(gomega.Equal(ip1V4Sub))
				gomega.Expect(fakeOvn.controller.eIPC.allocator[node1.Name].v6Subnet).To(gomega.Equal(ip1V6Sub))
				gomega.Expect(fakeOvn.controller.eIPC.allocator[node2.Name].v4Subnet).To(gomega.Equal(ip2V4Sub))
				gomega.Eventually(eIP.Status.Items).Should(gomega.HaveLen(0))

				node1.Labels = map[string]string{
					"k8s.ovn.org/egress-assignable": "",
				}

				fakeOvn.fakeExec.AddFakeCmdsNoOutputNoError(
					[]string{
						fmt.Sprintf("ovn-nbctl --timeout=15 set logical_switch_port etor-GR_node1 options:nat-addresses=router"),
					},
				)

				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), &node1, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(0))
				gomega.Eventually(isEgressAssignableNode(node1.Name)).Should(gomega.BeTrue())

				gomega.Eventually(getEgressIPReassignmentCount).Should(gomega.Equal(1))

				node2.Labels = map[string]string{
					"k8s.ovn.org/egress-assignable": "",
				}

				fakeOvn.fakeExec.AddFakeCmdsNoOutputNoError(
					[]string{
						fmt.Sprintf("ovn-nbctl --timeout=15 set logical_switch_port etor-GR_node2 options:nat-addresses=router"),
					},
				)

				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), &node2, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))

				statuses := getEgressIPStatus(egressIPName)
				gomega.Expect(statuses[0].Node).To(gomega.Equal(node2.Name))
				gomega.Expect(statuses[0].EgressIP).To(gomega.Equal(egressIP))
				gomega.Eventually(getEgressIPReassignmentCount).Should(gomega.Equal(0))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should try re-assigning EgressIP until all defined egress IPs are assigned", func() {
			app.Action = func(ctx *cli.Context) error {

				egressIP1 := "192.168.126.101"
				egressIP2 := "192.168.126.102"
				node1IPv4 := "192.168.126.12/24"
				node2IPv4 := "192.168.126.51/24"

				node1 := v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: node1Name,
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\"}", node1IPv4),
						},
					},
					Status: v1.NodeStatus{
						Conditions: []v1.NodeCondition{
							{
								Type:   v1.NodeReady,
								Status: v1.ConditionTrue,
							},
						},
					},
				}
				node2 := v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: node2Name,
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\"}", node2IPv4),
						},
					},
					Status: v1.NodeStatus{
						Conditions: []v1.NodeCondition{
							{
								Type:   v1.NodeReady,
								Status: v1.ConditionTrue,
							},
						},
					},
				}

				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{egressIP1, egressIP2},
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
				fakeOvn.controller.WatchEgressIP()

				gomega.Eventually(getEgressIPAllocatorSizeSafely).Should(gomega.Equal(2))
				gomega.Expect(fakeOvn.controller.eIPC.allocator).To(gomega.HaveKey(node1.Name))
				gomega.Expect(fakeOvn.controller.eIPC.allocator).To(gomega.HaveKey(node2.Name))
				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(0))

				node1.Labels = map[string]string{
					"k8s.ovn.org/egress-assignable": "",
				}

				fakeOvn.fakeExec.AddFakeCmdsNoOutputNoError(
					[]string{
						fmt.Sprintf("ovn-nbctl --timeout=15 set logical_switch_port etor-GR_node1 options:nat-addresses=router"),
					},
				)

				_, err := fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), &node1, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
				statuses := getEgressIPStatus(egressIPName)
				gomega.Expect(statuses[0].Node).To(gomega.Equal(node1.Name))
				gomega.Expect(statuses[0].EgressIP).To(gomega.Equal(egressIP1))

				gomega.Eventually(getEgressIPReassignmentCount).Should(gomega.Equal(1))

				node2.Labels = map[string]string{
					"k8s.ovn.org/egress-assignable": "",
				}

				fakeOvn.fakeExec.AddFakeCmdsNoOutputNoError(
					[]string{
						fmt.Sprintf("ovn-nbctl --timeout=15 set logical_switch_port etor-GR_node2 options:nat-addresses=router"),
					},
				)

				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), &node2, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(2))
				gomega.Eventually(getEgressIPReassignmentCount).Should(gomega.Equal(0))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should re-balance EgressIPs when their node is removed", func() {
			app.Action = func(ctx *cli.Context) error {

				egressIP := "192.168.126.101"
				node1IPv4 := "192.168.126.12/24"
				node1IPv6 := "0:0:0:0:0:feff:c0a8:8e0c/64"
				node2IPv4 := "192.168.126.51/24"

				node1 := v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: node1Name,
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4, node1IPv6),
						},
						Labels: map[string]string{
							"k8s.ovn.org/egress-assignable": "",
						},
					},
					Status: v1.NodeStatus{
						Conditions: []v1.NodeCondition{
							{
								Type:   v1.NodeReady,
								Status: v1.ConditionTrue,
							},
						},
					},
				}
				node2 := v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: node2Name,
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node2IPv4, ""),
						},
						Labels: map[string]string{
							"k8s.ovn.org/egress-assignable": "",
						},
					},
					Status: v1.NodeStatus{
						Conditions: []v1.NodeCondition{
							{
								Type:   v1.NodeReady,
								Status: v1.ConditionTrue,
							},
						},
					},
				}

				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{egressIP},
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
						Items: []v1.Node{node1},
					})

				fakeOvn.fakeExec.AddFakeCmdsNoOutputNoError(
					[]string{
						fmt.Sprintf("ovn-nbctl --timeout=15 lr-policy-add ovn_cluster_router 101 ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14 allow"),
						fmt.Sprintf("ovn-nbctl --timeout=15 set logical_switch_port etor-GR_node1 options:nat-addresses=router"),
						fmt.Sprintf("ovn-nbctl --timeout=15 set logical_switch_port etor-GR_node2 options:nat-addresses=router"),
					},
				)

				fakeOvn.controller.WatchEgressNodes()
				fakeOvn.controller.WatchEgressIP()

				gomega.Eventually(getEgressIPAllocatorSizeSafely).Should(gomega.Equal(1))
				gomega.Expect(fakeOvn.controller.eIPC.allocator).To(gomega.HaveKey(node1.Name))
				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
				statuses := getEgressIPStatus(egressIPName)
				gomega.Expect(statuses[0].Node).To(gomega.Equal(node1.Name))
				gomega.Expect(statuses[0].EgressIP).To(gomega.Equal(egressIP))

				_, err := fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Create(context.TODO(), &node2, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
				statuses = getEgressIPStatus(egressIPName)
				gomega.Expect(statuses[0].Node).To(gomega.Equal(node1.Name))
				gomega.Expect(statuses[0].EgressIP).To(gomega.Equal(egressIP))
				gomega.Eventually(getEgressIPAllocatorSizeSafely).Should(gomega.Equal(2))
				gomega.Expect(fakeOvn.controller.eIPC.allocator).To(gomega.HaveKey(node1.Name))
				gomega.Expect(fakeOvn.controller.eIPC.allocator).To(gomega.HaveKey(node2.Name))

				fakeOvn.fakeExec.AddFakeCmdsNoOutputNoError(
					[]string{
						fmt.Sprintf("ovn-nbctl --timeout=15 remove logical_switch_port etor-GR_node1 options nat-addresses=router"),
					},
				)

				fakeOvn.fakeExec.AddFakeCmdsNoOutputNoError(
					[]string{
						fmt.Sprintf("ovn-nbctl --timeout=15 remove logical_switch_port etor-GR_node1 options nat-addresses=router"),
					},
				)

				err = fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Delete(context.TODO(), node1.Name, *metav1.NewDeleteOptions(0))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(getEgressIPAllocatorSizeSafely).Should(gomega.Equal(1))
				gomega.Expect(fakeOvn.controller.eIPC.allocator).ToNot(gomega.HaveKey(node1.Name))
				gomega.Expect(fakeOvn.controller.eIPC.allocator).To(gomega.HaveKey(node2.Name))
				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))

				getNewNode := func() string {
					statuses = getEgressIPStatus(egressIPName)
					return statuses[0].Node
				}

				gomega.Eventually(getNewNode).Should(gomega.Equal(node2.Name))
				statuses = getEgressIPStatus(egressIPName)
				gomega.Expect(statuses[0].EgressIP).To(gomega.Equal(egressIP))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

	})

	ginkgo.Context("Dual-stack assignment", func() {

		ginkgo.It("should be able to allocate non-conflicting IPv4 on node which can host it, even if it happens to be the node with more assignments", func() {
			app.Action = func(ctx *cli.Context) error {

				fakeOvn.start(ctx)
				egressIP := "192.168.126.99"

				node1 := setupNode(node1Name, []string{"0:0:0:0:0:feff:c0a8:8e0c/64"}, []string{"0:0:0:0:0:feff:c0a8:8e23"})
				node2 := setupNode(node2Name, []string{"192.168.126.51/24"}, []string{"192.168.126.68", "192.168.126.102"})

				fakeOvn.controller.eIPC.allocator[node1.name] = &node1
				fakeOvn.controller.eIPC.allocator[node2.name] = &node2

				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{egressIP},
					},
				}
				err := fakeOvn.controller.assignEgressIPs(&eIP)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(eIP.Status.Items).To(gomega.HaveLen(1))
				gomega.Expect(eIP.Status.Items[0].Node).To(gomega.Equal(node2.name))
				gomega.Expect(eIP.Status.Items[0].EgressIP).To(gomega.Equal(net.ParseIP(egressIP).String()))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

	})

	ginkgo.Context("IPv4 assignment", func() {

		ginkgo.It("Should not be able to assign egress IP defined in CIDR notation", func() {
			app.Action = func(ctx *cli.Context) error {

				fakeOvn.start(ctx)

				egressIPs := []string{"192.168.126.99/32"}

				node1 := setupNode(node1Name, []string{"192.168.126.12/24"}, []string{"192.168.126.102", "192.168.126.111"})
				node2 := setupNode(node2Name, []string{"192.168.126.51/24"}, []string{"192.168.126.68"})

				fakeOvn.controller.eIPC.allocator[node1.name] = &node1
				fakeOvn.controller.eIPC.allocator[node2.name] = &node2

				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: egressIPs,
					},
				}

				err := fakeOvn.controller.assignEgressIPs(&eIP)
				gomega.Expect(err).To(gomega.HaveOccurred())
				gomega.Expect(err.Error()).To(gomega.Equal(fmt.Sprintf("unable to parse provided EgressIP: %s, invalid", egressIPs[0])))
				gomega.Expect(eIP.Status.Items).To(gomega.HaveLen(0))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

	})

	ginkgo.Context("IPv6 assignment", func() {

		ginkgo.It("should be able to allocate non-conflicting IP on node with lowest amount of allocations", func() {
			app.Action = func(ctx *cli.Context) error {

				fakeOvn.start(ctx)

				egressIP := "0:0:0:0:0:feff:c0a8:8e0f"
				node1 := setupNode(node1Name, []string{"0:0:0:0:0:feff:c0a8:8e0c/64"}, []string{"0:0:0:0:0:feff:c0a8:8e32", "0:0:0:0:0:feff:c0a8:8e1e"})
				node2 := setupNode(node2Name, []string{"0:0:0:0:0:fedf:c0a8:8e0c/64"}, []string{"0:0:0:0:0:feff:c0a8:8e23"})

				fakeOvn.controller.eIPC.allocator[node1.name] = &node1
				fakeOvn.controller.eIPC.allocator[node2.name] = &node2

				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{egressIP},
					},
				}
				err := fakeOvn.controller.assignEgressIPs(&eIP)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(eIP.Status.Items).To(gomega.HaveLen(1))
				gomega.Expect(eIP.Status.Items[0].Node).To(gomega.Equal(node2.name))
				gomega.Expect(eIP.Status.Items[0].EgressIP).To(gomega.Equal(net.ParseIP(egressIP).String()))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should be able to allocate several EgressIPs and avoid the same node", func() {
			app.Action = func(ctx *cli.Context) error {

				fakeOvn.start(ctx)

				egressIP1 := "0:0:0:0:0:feff:c0a8:8e0d"
				egressIP2 := "0:0:0:0:0:feff:c0a8:8e0f"
				node1 := setupNode(node1Name, []string{"0:0:0:0:0:feff:c0a8:8e0c/64"}, []string{"0:0:0:0:0:feff:c0a8:8e32", "0:0:0:0:0:feff:c0a8:8e1e"})
				node2 := setupNode(node2Name, []string{"0:0:0:0:0:fedf:c0a8:8e0c/64"}, []string{"0:0:0:0:0:feff:c0a8:8e23"})

				fakeOvn.controller.eIPC.allocator[node1.name] = &node1
				fakeOvn.controller.eIPC.allocator[node2.name] = &node2

				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{egressIP1, egressIP2},
					},
				}
				err := fakeOvn.controller.assignEgressIPs(&eIP)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(eIP.Status.Items).To(gomega.HaveLen(2))
				gomega.Expect(eIP.Status.Items[0].Node).To(gomega.Equal(node2.name))
				gomega.Expect(eIP.Status.Items[0].EgressIP).To(gomega.Equal(net.ParseIP(egressIP1).String()))
				gomega.Expect(eIP.Status.Items[1].Node).To(gomega.Equal(node1.name))
				gomega.Expect(eIP.Status.Items[1].EgressIP).To(gomega.Equal(net.ParseIP(egressIP2).String()))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should be able to allocate several EgressIPs and avoid the same node and leave one un-assigned without error", func() {
			app.Action = func(ctx *cli.Context) error {

				fakeOvn.start(ctx)

				egressIP1 := "0:0:0:0:0:feff:c0a8:8e0d"
				egressIP2 := "0:0:0:0:0:feff:c0a8:8e0e"
				egressIP3 := "0:0:0:0:0:feff:c0a8:8e0f"

				node1 := setupNode(node1Name, []string{"0:0:0:0:0:feff:c0a8:8e0c/64"}, []string{"0:0:0:0:0:feff:c0a8:8e32", "0:0:0:0:0:feff:c0a8:8e1e"})
				node2 := setupNode(node2Name, []string{"0:0:0:0:0:fedf:c0a8:8e0c/64"}, []string{"0:0:0:0:0:feff:c0a8:8e23"})

				fakeOvn.controller.eIPC.allocator[node1.name] = &node1
				fakeOvn.controller.eIPC.allocator[node2.name] = &node2

				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{egressIP1, egressIP2, egressIP3},
					},
				}
				err := fakeOvn.controller.assignEgressIPs(&eIP)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(eIP.Status.Items).To(gomega.HaveLen(2))
				gomega.Expect(eIP.Status.Items[0].Node).To(gomega.Equal(node2.name))
				gomega.Expect(eIP.Status.Items[0].EgressIP).To(gomega.Equal(net.ParseIP(egressIP1).String()))
				gomega.Expect(eIP.Status.Items[1].Node).To(gomega.Equal(node1.name))
				gomega.Expect(eIP.Status.Items[1].EgressIP).To(gomega.Equal(net.ParseIP(egressIP2).String()))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should not be able to allocate already allocated IP", func() {
			app.Action = func(ctx *cli.Context) error {

				fakeOvn.start(ctx)

				egressIP := "0:0:0:0:0:feff:c0a8:8e32"

				node1 := setupNode(node1Name, []string{"0:0:0:0:0:feff:c0a8:8e0c/64"}, []string{egressIP, "0:0:0:0:0:feff:c0a8:8e1e"})
				node2 := setupNode(node2Name, []string{"0:0:0:0:0:fedf:c0a8:8e0c/64"}, []string{"0:0:0:0:0:feff:c0a8:8e23"})

				fakeOvn.controller.eIPC.allocator[node1.name] = &node1
				fakeOvn.controller.eIPC.allocator[node2.name] = &node2

				egressIPs := []string{egressIP}
				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: egressIPs,
					},
				}

				err := fakeOvn.controller.assignEgressIPs(&eIP)
				gomega.Expect(err).To(gomega.HaveOccurred())
				gomega.Expect(err.Error()).To(gomega.Equal("no matching host found"))
				gomega.Expect(eIP.Status.Items).To(gomega.HaveLen(0))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should not be able to allocate node IP", func() {
			app.Action = func(ctx *cli.Context) error {

				fakeOvn.start(ctx)

				egressIP := "0:0:0:0:0:feff:c0a8:8e0c"

				node1 := setupNode(node1Name, []string{egressIP + "/64"}, []string{"0:0:0:0:0:feff:c0a8:8e32", "0:0:0:0:0:feff:c0a8:8e1e"})
				node2 := setupNode(node2Name, []string{"0:0:0:0:0:fedf:c0a8:8e0c/64"}, []string{"0:0:0:0:0:feff:c0a8:8e23"})

				fakeOvn.controller.eIPC.allocator[node1.name] = &node1
				fakeOvn.controller.eIPC.allocator[node2.name] = &node2

				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{egressIP},
					},
				}
				err := fakeOvn.controller.assignEgressIPs(&eIP)
				gomega.Expect(err).To(gomega.HaveOccurred())
				gomega.Expect(eIP.Status.Items).To(gomega.HaveLen(0))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should not be able to allocate conflicting compressed IP", func() {
			app.Action = func(ctx *cli.Context) error {

				fakeOvn.start(ctx)

				egressIP := "::feff:c0a8:8e32"

				node1 := setupNode(node1Name, []string{"0:0:0:0:0:feff:c0a8:8e0c/64"}, []string{"0:0:0:0:0:feff:c0a8:8e32", "0:0:0:0:0:feff:c0a8:8e1e"})
				node2 := setupNode(node2Name, []string{"0:0:0:0:0:fedf:c0a8:8e0c/64"}, []string{"0:0:0:0:0:feff:c0a8:8e23"})

				fakeOvn.controller.eIPC.allocator[node1.name] = &node1
				fakeOvn.controller.eIPC.allocator[node2.name] = &node2

				egressIPs := []string{egressIP}

				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: egressIPs,
					},
				}

				err := fakeOvn.controller.assignEgressIPs(&eIP)
				gomega.Expect(err).To(gomega.HaveOccurred())
				gomega.Expect(err.Error()).To(gomega.Equal("no matching host found"))
				gomega.Expect(eIP.Status.Items).To(gomega.HaveLen(0))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should not be able to allocate IPv4 IP on nodes which can only host IPv6", func() {
			app.Action = func(ctx *cli.Context) error {

				fakeOvn.start(ctx)

				egressIP := "192.168.126.16"

				node1 := setupNode(node1Name, []string{"0:0:0:0:0:feff:c0a8:8e0c/64"}, []string{"0:0:0:0:0:feff:c0a8:8e32", "0:0:0:0:0:feff:c0a8:8e1e"})
				node2 := setupNode(node2Name, []string{"0:0:0:0:0:fedf:c0a8:8e0c/64"}, []string{"0:0:0:0:0:feff:c0a8:8e23"})

				fakeOvn.controller.eIPC.allocator[node1.name] = &node1
				fakeOvn.controller.eIPC.allocator[node2.name] = &node2

				eIPs := []string{egressIP}
				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: eIPs,
					},
				}

				err := fakeOvn.controller.assignEgressIPs(&eIP)
				gomega.Expect(err).To(gomega.HaveOccurred())
				gomega.Expect(err.Error()).To(gomega.Equal("no matching host found"))
				gomega.Expect(eIP.Status.Items).To(gomega.HaveLen(0))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should be able to allocate non-conflicting compressed uppercase IP", func() {
			app.Action = func(ctx *cli.Context) error {

				fakeOvn.start(ctx)

				egressIP := "::FEFF:C0A8:8D32"

				node1 := setupNode(node1Name, []string{"0:0:0:0:0:feff:c0a8:8e0c/64"}, []string{"0:0:0:0:0:feff:c0a8:8e32", "0:0:0:0:0:feff:c0a8:8e1e"})
				node2 := setupNode(node2Name, []string{"0:0:0:0:0:fedf:c0a8:8e0c/64"}, []string{"0:0:0:0:0:feff:c0a8:8e23"})

				fakeOvn.controller.eIPC.allocator[node1.name] = &node1
				fakeOvn.controller.eIPC.allocator[node2.name] = &node2

				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{egressIP},
					},
				}
				err := fakeOvn.controller.assignEgressIPs(&eIP)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(eIP.Status.Items).To(gomega.HaveLen(1))
				gomega.Expect(eIP.Status.Items[0].Node).To(gomega.Equal(node2.name))
				gomega.Expect(eIP.Status.Items[0].EgressIP).To(gomega.Equal(net.ParseIP(egressIP).String()))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should not be able to allocate conflicting compressed uppercase IP", func() {
			app.Action = func(ctx *cli.Context) error {

				fakeOvn.start(ctx)

				egressIP := "::FEFF:C0A8:8E32"

				node1 := setupNode(node1Name, []string{"0:0:0:0:0:feff:c0a8:8e0c/64"}, []string{"0:0:0:0:0:feff:c0a8:8e32", "0:0:0:0:0:feff:c0a8:8e1e"})
				node2 := setupNode(node2Name, []string{"0:0:0:0:0:fedf:c0a8:8e0c/64"}, []string{"0:0:0:0:0:feff:c0a8:8e23"})

				fakeOvn.controller.eIPC.allocator[node1.name] = &node1
				fakeOvn.controller.eIPC.allocator[node2.name] = &node2
				egressIPs := []string{egressIP}

				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: egressIPs,
					},
				}

				err := fakeOvn.controller.assignEgressIPs(&eIP)
				gomega.Expect(err).To(gomega.HaveOccurred())
				gomega.Expect(err.Error()).To(gomega.Equal("no matching host found"))
				gomega.Expect(eIP.Status.Items).To(gomega.HaveLen(0))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should not be able to allocate invalid IP", func() {
			app.Action = func(ctx *cli.Context) error {

				fakeOvn.start(ctx)

				egressIPs := []string{"0:0:0:0:0:feff:c0a8:8e32:5"}

				node1 := setupNode(node1Name, []string{"0:0:0:0:0:feff:c0a8:8e0c/64"}, []string{"0:0:0:0:0:feff:c0a8:8e32", "0:0:0:0:0:feff:c0a8:8e1e"})
				node2 := setupNode(node2Name, []string{"0:0:0:0:0:fedf:c0a8:8e0c/64"}, []string{"0:0:0:0:0:feff:c0a8:8e23"})

				fakeOvn.controller.eIPC.allocator[node1.name] = &node1
				fakeOvn.controller.eIPC.allocator[node2.name] = &node2

				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: egressIPs,
					},
				}

				err := fakeOvn.controller.assignEgressIPs(&eIP)
				gomega.Expect(err).To(gomega.HaveOccurred())
				gomega.Expect(err.Error()).To(gomega.Equal(fmt.Sprintf("unable to parse provided EgressIP: %s, invalid", egressIPs[0])))
				gomega.Expect(eIP.Status.Items).To(gomega.HaveLen(0))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})

	ginkgo.Context("WatchEgressIP", func() {

		ginkgo.It("should update status correctly for single-stack IPv4", func() {
			app.Action = func(ctx *cli.Context) error {
				fakeOvn.start(ctx)

				egressIP := "192.168.126.10"
				node1 := setupNode(node1Name, []string{"192.168.126.12/24"}, []string{"192.168.126.102", "192.168.126.111"})
				node2 := setupNode(node2Name, []string{"192.168.126.51/24"}, []string{"192.168.126.68"})

				fakeOvn.controller.eIPC.allocator[node1.name] = &node1
				fakeOvn.controller.eIPC.allocator[node2.name] = &node2

				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{egressIP},
						NamespaceSelector: metav1.LabelSelector{
							MatchLabels: map[string]string{
								"name": "does-not-exist",
							},
						},
					},
				}
				fakeOvn.fakeExec.AddFakeCmdsNoOutputNoError(
					[]string{
						fmt.Sprintf("ovn-nbctl --timeout=15 lr-policy-add ovn_cluster_router 101 ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14 allow"),
					},
				)
				fakeOvn.controller.WatchEgressIP()

				_, err := fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Create(context.TODO(), &eIP, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
				statuses := getEgressIPStatus(egressIPName)
				gomega.Expect(statuses[0].Node).To(gomega.Equal(node2.name))
				gomega.Expect(statuses[0].EgressIP).To(gomega.Equal(egressIP))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should update status correctly for single-stack IPv6", func() {
			app.Action = func(ctx *cli.Context) error {
				fakeOvn.start(ctx)

				egressIP := "0:0:0:0:0:feff:c0a8:8e0d"

				node1 := setupNode(node1Name, []string{"0:0:0:0:0:feff:c0a8:8e0c/64"}, []string{"0:0:0:0:0:feff:c0a8:8e32", "0:0:0:0:0:feff:c0a8:8e1e"})
				node2 := setupNode(node2Name, []string{"0:0:0:0:0:fedf:c0a8:8e0c/64"}, []string{"0:0:0:0:0:feff:c0a8:8e23"})

				fakeOvn.controller.eIPC.allocator[node1.name] = &node1
				fakeOvn.controller.eIPC.allocator[node2.name] = &node2

				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{egressIP},
					},
				}
				fakeOvn.fakeExec.AddFakeCmdsNoOutputNoError(
					[]string{
						fmt.Sprintf("ovn-nbctl --timeout=15 lr-policy-add ovn_cluster_router 101 ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14 allow"),
					},
				)
				fakeOvn.controller.WatchEgressIP()

				_, err := fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Create(context.TODO(), &eIP, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
				statuses := getEgressIPStatus(egressIPName)
				gomega.Expect(statuses[0].Node).To(gomega.Equal(node2.name))
				gomega.Expect(statuses[0].EgressIP).To(gomega.Equal(net.ParseIP(egressIP).String()))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should update status correctly for dual-stack", func() {
			app.Action = func(ctx *cli.Context) error {
				fakeOvn.start(ctx)

				egressIPv4 := "192.168.126.101"
				egressIPv6 := "0:0:0:0:0:feff:c0a8:8e0d"

				node1 := setupNode(node1Name, []string{"0:0:0:0:0:feff:c0a8:8e0c/64"}, []string{"0:0:0:0:0:feff:c0a8:8e23"})
				node2 := setupNode(node2Name, []string{"192.168.126.51/24"}, []string{"192.168.126.68", "192.168.126.102"})

				fakeOvn.controller.eIPC.allocator[node1.name] = &node1
				fakeOvn.controller.eIPC.allocator[node2.name] = &node2

				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{egressIPv4, egressIPv6},
					},
				}
				fakeOvn.fakeExec.AddFakeCmdsNoOutputNoError(
					[]string{
						fmt.Sprintf("ovn-nbctl --timeout=15 lr-policy-add ovn_cluster_router 101 ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14 allow"),
					},
				)
				fakeOvn.controller.WatchEgressIP()

				_, err := fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Create(context.TODO(), &eIP, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(2))
				statuses := getEgressIPStatus(egressIPName)
				gomega.Expect(statuses[0].Node).To(gomega.Equal(node2.name))
				gomega.Expect(statuses[0].EgressIP).To(gomega.Equal(net.ParseIP(egressIPv4).String()))
				gomega.Expect(statuses[1].Node).To(gomega.Equal(node1.name))
				gomega.Expect(statuses[1].EgressIP).To(gomega.Equal(net.ParseIP(egressIPv6).String()))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})

	ginkgo.Context("syncEgressIP for dual-stack", func() {

		ginkgo.It("should not update valid assignments", func() {
			app.Action = func(ctx *cli.Context) error {

				egressIPv4 := "192.168.126.101"
				egressIPv6 := "0:0:0:0:0:feff:c0a8:8e0d"

				node1 := setupNode(node1Name, []string{"0:0:0:0:0:feff:c0a8:8e0c/64"}, []string{"0:0:0:0:0:feff:c0a8:8e23"})
				node2 := setupNode(node2Name, []string{"192.168.126.51/24"}, []string{"192.168.126.68", "192.168.126.102"})

				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{egressIPv4, egressIPv6},
					},
					Status: egressipv1.EgressIPStatus{
						Items: []egressipv1.EgressIPStatusItem{
							{
								EgressIP: egressIPv4,
								Node:     node2.name,
							},
							{
								EgressIP: net.ParseIP(egressIPv6).String(),
								Node:     node1.name,
							},
						},
					},
				}

				fakeOvn.start(ctx,
					&egressipv1.EgressIPList{
						Items: []egressipv1.EgressIP{eIP},
					})

				fakeOvn.controller.eIPC.allocator[node1.name] = &node1
				fakeOvn.controller.eIPC.allocator[node2.name] = &node2

				fakeOvn.fakeExec.AddFakeCmdsNoOutputNoError(
					[]string{
						fmt.Sprintf("ovn-nbctl --timeout=15 lr-policy-add ovn_cluster_router 101 ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14 allow"),
					},
				)
				fakeOvn.controller.WatchEgressIP()

				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(2))
				statuses := getEgressIPStatus(egressIPName)
				gomega.Expect(statuses[0].Node).To(gomega.Equal(eIP.Status.Items[0].Node))
				gomega.Expect(statuses[0].EgressIP).To(gomega.Equal(eIP.Status.Items[0].EgressIP))
				gomega.Expect(statuses[1].Node).To(gomega.Equal(eIP.Status.Items[1].Node))
				gomega.Expect(statuses[1].EgressIP).To(gomega.Equal(eIP.Status.Items[1].EgressIP))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should update invalid assignments on UNKNOWN node", func() {
			app.Action = func(ctx *cli.Context) error {

				egressIPv4 := "192.168.126.101"
				egressIPv6 := "0:0:0:0:0:feff:c0a8:8e0d"

				node1 := setupNode(node1Name, []string{"0:0:0:0:0:feff:c0a8:8e0c/64"}, []string{"0:0:0:0:0:feff:c0a8:8e23"})
				node2 := setupNode(node2Name, []string{"192.168.126.51/24"}, []string{"192.168.126.68", "192.168.126.102"})

				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{egressIPv4, egressIPv6},
					},
					Status: egressipv1.EgressIPStatus{
						Items: []egressipv1.EgressIPStatusItem{
							{
								EgressIP: egressIPv4,
								Node:     "UNKNOWN",
							},
							{
								EgressIP: net.ParseIP(egressIPv6).String(),
								Node:     node1.name,
							},
						},
					},
				}

				fakeOvn.start(ctx,
					&egressipv1.EgressIPList{
						Items: []egressipv1.EgressIP{eIP},
					})

				fakeOvn.controller.eIPC.allocator[node1.name] = &node1
				fakeOvn.controller.eIPC.allocator[node2.name] = &node2
				fakeOvn.fakeExec.AddFakeCmdsNoOutputNoError(
					[]string{
						fmt.Sprintf("ovn-nbctl --timeout=15 lr-policy-add ovn_cluster_router 101 ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14 allow"),
					},
				)
				fakeOvn.controller.WatchEgressIP()

				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(2))
				statuses := getEgressIPStatus(egressIPName)
				gomega.Expect(statuses[0].Node).To(gomega.Equal(node2.name))
				gomega.Expect(statuses[0].EgressIP).To(gomega.Equal(eIP.Status.Items[0].EgressIP))
				gomega.Expect(statuses[1].Node).To(gomega.Equal(eIP.Status.Items[1].Node))
				gomega.Expect(statuses[1].EgressIP).To(gomega.Equal(eIP.Status.Items[1].EgressIP))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should update assignment on unsupported IP family node", func() {
			app.Action = func(ctx *cli.Context) error {

				egressIPv4 := "192.168.126.101"

				node1 := setupNode(node1Name, []string{"0:0:0:0:0:feff:c0a8:8e0c/64"}, []string{"0:0:0:0:0:feff:c0a8:8e23"})
				node2 := setupNode(node2Name, []string{"192.168.126.51/24"}, []string{"192.168.126.68", "192.168.126.102"})

				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{egressIPv4},
					},
					Status: egressipv1.EgressIPStatus{
						Items: []egressipv1.EgressIPStatusItem{
							{
								EgressIP: egressIPv4,
								Node:     node1.name,
							},
						},
					},
				}

				fakeOvn.start(ctx,
					&egressipv1.EgressIPList{
						Items: []egressipv1.EgressIP{eIP},
					})

				fakeOvn.controller.eIPC.allocator[node1.name] = &node1
				fakeOvn.controller.eIPC.allocator[node2.name] = &node2
				fakeOvn.fakeExec.AddFakeCmdsNoOutputNoError(
					[]string{
						fmt.Sprintf("ovn-nbctl --timeout=15 lr-policy-add ovn_cluster_router 101 ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14 allow"),
					},
				)
				fakeOvn.controller.WatchEgressIP()

				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
				statuses := getEgressIPStatus(egressIPName)
				gomega.Expect(statuses[0].Node).To(gomega.Equal(node2.name))
				gomega.Expect(statuses[0].EgressIP).To(gomega.Equal(eIP.Status.Items[0].EgressIP))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})

	ginkgo.Context("syncEgressIP for IPv4", func() {

		ginkgo.It("should update invalid assignments on duplicated node", func() {
			app.Action = func(ctx *cli.Context) error {

				egressIP1 := "192.168.126.101"
				egressIP2 := "192.168.126.100"

				node1 := setupNode(node1Name, []string{"192.168.126.12/24"}, []string{"192.168.126.102", "192.168.126.111"})
				node2 := setupNode(node2Name, []string{"192.168.126.51/24"}, []string{"192.168.126.68"})

				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{egressIP1, egressIP2},
					},
					Status: egressipv1.EgressIPStatus{
						Items: []egressipv1.EgressIPStatusItem{
							{
								EgressIP: egressIP1,
								Node:     node1.name,
							},
							{
								EgressIP: egressIP2,
								Node:     node1.name,
							},
						},
					},
				}

				fakeOvn.start(ctx,
					&egressipv1.EgressIPList{
						Items: []egressipv1.EgressIP{eIP},
					})

				fakeOvn.controller.eIPC.allocator[node1.name] = &node1
				fakeOvn.controller.eIPC.allocator[node2.name] = &node2
				fakeOvn.fakeExec.AddFakeCmdsNoOutputNoError(
					[]string{
						fmt.Sprintf("ovn-nbctl --timeout=15 lr-policy-add ovn_cluster_router 101 ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14 allow"),
					},
				)
				fakeOvn.controller.WatchEgressIP()

				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(2))
				statuses := getEgressIPStatus(egressIPName)
				gomega.Expect(statuses[0].Node).To(gomega.Equal(node2.name))
				gomega.Expect(statuses[0].EgressIP).To(gomega.Equal(eIP.Status.Items[0].EgressIP))
				gomega.Expect(statuses[1].Node).To(gomega.Equal(node1.name))
				gomega.Expect(statuses[1].EgressIP).To(gomega.Equal(eIP.Status.Items[1].EgressIP))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should update invalid assignments with incorrectly parsed IP", func() {
			app.Action = func(ctx *cli.Context) error {

				egressIP1 := "192.168.126.101"
				egressIPIncorrect := "192.168.126.1000"

				node1 := setupNode(node1Name, []string{"192.168.126.12/24"}, []string{"192.168.126.102", "192.168.126.111"})
				node2 := setupNode(node2Name, []string{"192.168.126.51/24"}, []string{"192.168.126.68"})

				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{egressIP1},
					},
					Status: egressipv1.EgressIPStatus{
						Items: []egressipv1.EgressIPStatusItem{
							{
								EgressIP: egressIPIncorrect,
								Node:     node1.name,
							},
						},
					},
				}

				fakeOvn.start(ctx,
					&egressipv1.EgressIPList{
						Items: []egressipv1.EgressIP{eIP},
					})

				fakeOvn.controller.eIPC.allocator[node1.name] = &node1
				fakeOvn.controller.eIPC.allocator[node2.name] = &node2
				fakeOvn.fakeExec.AddFakeCmdsNoOutputNoError(
					[]string{
						fmt.Sprintf("ovn-nbctl --timeout=15 lr-policy-add ovn_cluster_router 101 ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14 allow"),
					},
				)
				fakeOvn.controller.WatchEgressIP()

				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
				statuses := getEgressIPStatus(egressIPName)
				gomega.Expect(statuses[0].Node).To(gomega.Equal(node2.name))
				gomega.Expect(statuses[0].EgressIP).To(gomega.Equal(egressIP1))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should update invalid assignments with unhostable IP on a node", func() {
			app.Action = func(ctx *cli.Context) error {

				egressIP1 := "192.168.126.101"
				egressIPIncorrect := "192.168.128.100"

				node1 := setupNode(node1Name, []string{"192.168.126.12/24"}, []string{"192.168.126.102", "192.168.126.111"})
				node2 := setupNode(node2Name, []string{"192.168.126.51/24"}, []string{"192.168.126.68"})

				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{egressIP1},
					},
					Status: egressipv1.EgressIPStatus{
						Items: []egressipv1.EgressIPStatusItem{
							{
								EgressIP: egressIPIncorrect,
								Node:     node1.name,
							},
						},
					},
				}

				fakeOvn.start(ctx,
					&egressipv1.EgressIPList{
						Items: []egressipv1.EgressIP{eIP},
					})

				fakeOvn.controller.eIPC.allocator[node1.name] = &node1
				fakeOvn.controller.eIPC.allocator[node2.name] = &node2
				fakeOvn.fakeExec.AddFakeCmdsNoOutputNoError(
					[]string{
						fmt.Sprintf("ovn-nbctl --timeout=15 lr-policy-add ovn_cluster_router 101 ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14 allow"),
					},
				)
				fakeOvn.controller.WatchEgressIP()

				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
				statuses := getEgressIPStatus(egressIPName)
				gomega.Expect(statuses[0].Node).To(gomega.Equal(node2.name))
				gomega.Expect(statuses[0].EgressIP).To(gomega.Equal(egressIP1))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should not update valid assignment", func() {
			app.Action = func(ctx *cli.Context) error {

				egressIP1 := "192.168.126.101"

				node1 := setupNode(node1Name, []string{"192.168.126.12/24"}, []string{"192.168.126.102", "192.168.126.111"})
				node2 := setupNode(node2Name, []string{"192.168.126.51/24"}, []string{"192.168.126.68"})

				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{egressIP1},
					},
					Status: egressipv1.EgressIPStatus{
						Items: []egressipv1.EgressIPStatusItem{
							{
								EgressIP: egressIP1,
								Node:     node1.name,
							},
						},
					},
				}

				fakeOvn.start(ctx,
					&egressipv1.EgressIPList{
						Items: []egressipv1.EgressIP{eIP},
					})

				fakeOvn.controller.eIPC.allocator[node1.name] = &node1
				fakeOvn.controller.eIPC.allocator[node2.name] = &node2
				fakeOvn.fakeExec.AddFakeCmdsNoOutputNoError(
					[]string{
						fmt.Sprintf("ovn-nbctl --timeout=15 lr-policy-add ovn_cluster_router 101 ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14 allow"),
					},
				)
				fakeOvn.controller.WatchEgressIP()

				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
				statuses := getEgressIPStatus(egressIPName)
				gomega.Expect(statuses[0].Node).To(gomega.Equal(node1.name))
				gomega.Expect(statuses[0].EgressIP).To(gomega.Equal(egressIP1))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})

	ginkgo.Context("AddEgressIP for IPv4", func() {

		ginkgo.It("should not create two EgressIPs with same egress IP value", func() {
			app.Action = func(ctx *cli.Context) error {
				egressIP1 := "192.168.126.101"

				node1 := setupNode(node1Name, []string{"192.168.126.12/24"}, []string{"192.168.126.102", "192.168.126.111"})
				node2 := setupNode(node2Name, []string{"192.168.126.51/24"}, []string{"192.168.126.68"})

				eIP1 := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta("egressip"),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{egressIP1},
					},
				}
				eIP2 := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta("egressip2"),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{egressIP1},
					},
				}
				fakeOvn.start(ctx)

				fakeOvn.controller.eIPC.allocator[node1.name] = &node1
				fakeOvn.controller.eIPC.allocator[node2.name] = &node2
				fakeOvn.fakeExec.AddFakeCmdsNoOutputNoError(
					[]string{
						fmt.Sprintf("ovn-nbctl --timeout=15 lr-policy-add ovn_cluster_router 101 ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14 allow"),
					},
				)
				fakeOvn.controller.WatchEgressIP()

				_, err := fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Create(context.TODO(), &eIP1, metav1.CreateOptions{})
				gomega.Expect(err).ToNot(gomega.HaveOccurred())

				gomega.Eventually(getEgressIPStatusLen(eIP1.Name)).Should(gomega.Equal(1))
				statuses := getEgressIPStatus(eIP1.Name)
				gomega.Expect(statuses[0].Node).To(gomega.Equal(node2.name))
				gomega.Expect(statuses[0].EgressIP).To(gomega.Equal(egressIP1))

				_, err = fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Create(context.TODO(), &eIP2, metav1.CreateOptions{})
				gomega.Expect(err).ToNot(gomega.HaveOccurred())

				gomega.Eventually(getEgressIPStatusLen(eIP2.Name)).Should(gomega.Equal(0))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

	})

	ginkgo.Context("UpdateEgressIP for IPv4", func() {

		ginkgo.It("should perform re-assingment of EgressIPs", func() {
			app.Action = func(ctx *cli.Context) error {

				egressIP := "192.168.126.101"
				updateEgressIP := "192.168.126.10"

				node1 := setupNode(node1Name, []string{"192.168.126.41/24"}, []string{"192.168.126.102", "192.168.126.111"})
				node2 := setupNode(node2Name, []string{"192.168.126.51/24"}, []string{"192.168.126.68"})

				eIP1 := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{egressIP},
					},
				}
				fakeOvn.start(ctx)

				fakeOvn.controller.eIPC.allocator[node1.name] = &node1
				fakeOvn.controller.eIPC.allocator[node2.name] = &node2
				fakeOvn.fakeExec.AddFakeCmdsNoOutputNoError(
					[]string{
						fmt.Sprintf("ovn-nbctl --timeout=15 lr-policy-add ovn_cluster_router 101 ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14 allow"),
					},
				)
				fakeOvn.controller.WatchEgressIP()

				_, err := fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Create(context.TODO(), &eIP1, metav1.CreateOptions{})
				gomega.Expect(err).ToNot(gomega.HaveOccurred())

				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
				statuses := getEgressIPStatus(egressIPName)
				gomega.Expect(statuses[0].Node).To(gomega.Equal(node2.name))
				gomega.Expect(statuses[0].EgressIP).To(gomega.Equal(egressIP))

				eIPToUpdate, err := fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Get(context.TODO(), eIP1.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				eIPToUpdate.Spec.EgressIPs = []string{updateEgressIP}

				_, err = fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Update(context.TODO(), eIPToUpdate, metav1.UpdateOptions{})
				gomega.Expect(err).ToNot(gomega.HaveOccurred())

				getEgressIP := func() string {
					statuses = getEgressIPStatus(egressIPName)
					if len(statuses) == 0 {
						return "try again"
					}
					return statuses[0].EgressIP
				}

				gomega.Eventually(getEgressIP).Should(gomega.Equal(updateEgressIP))
				statuses = getEgressIPStatus(egressIPName)
				gomega.Expect(statuses[0].Node).To(gomega.Equal(node2.name))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})
})
