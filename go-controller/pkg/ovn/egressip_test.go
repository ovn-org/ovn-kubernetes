package ovn

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/onsi/ginkgo"
	ginkgotable "github.com/onsi/ginkgo/extensions/table"
	"github.com/onsi/gomega"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	egressipv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	egresssvc "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/controller/egressservice"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/retry"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/urfave/cli/v2"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	utilpointer "k8s.io/utils/pointer"
)

var (
	nodeLogicalRouterIPv6      = []string{"fef0::56"}
	nodeLogicalRouterIPv4      = []string{"100.64.0.2"}
	node2LogicalRouterIPv4     = []string{"100.64.0.3"}
	nodeLogicalRouterIfAddrV6  = nodeLogicalRouterIPv6[0] + "/125"
	nodeLogicalRouterIfAddrV4  = nodeLogicalRouterIPv4[0] + "/29"
	node2LogicalRouterIfAddrV4 = node2LogicalRouterIPv4[0] + "/29"
)

const (
	namespace       = "egressip-namespace"
	nodeInternalIP  = "def0::56"
	v4GatewayIP     = "10.128.0.1"
	podV4IP         = "10.128.0.15"
	podV6IP         = "ae70::66"
	v6GatewayIP     = "ae70::1"
	v6ClusterSubnet = "ae70::66/64"
	v6NodeSubnet    = "ae70::66/64"
	v4ClusterSubnet = "10.128.0.0/14"
	v4NodeSubnet    = "10.128.0.0/24"
	podName         = "egress-pod"
	egressIPName    = "egressip"
	egressIPName2   = "egressip-2"
	inspectTimeout  = 4 * time.Second // arbitrary, to avoid failures on github CI
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

var egressPodLabel = map[string]string{"egress": "needed"}

var _ = ginkgo.Describe("OVN master EgressIP Operations", func() {
	var (
		app     *cli.App
		fakeOvn *FakeOVN
	)
	const (
		node1Name = "node1"
		node2Name = "node2"
	)

	clusterRouterDbSetup := libovsdbtest.TestSetup{
		NBData: []libovsdbtest.TestData{
			&nbdb.LogicalRouter{
				Name: ovntypes.OVNClusterRouter,
				UUID: ovntypes.OVNClusterRouter + "-UUID",
			},
		},
	}

	getEgressIPStatusLen := func(egressIPName string) func() int {
		return func() int {
			tmp, err := fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Get(context.TODO(), egressIPName, metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			return len(tmp.Status.Items)
		}
	}

	getEgressIPStatus := func(egressIPName string) ([]string, []string) {
		tmp, err := fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Get(context.TODO(), egressIPName, metav1.GetOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		var egressIPs, nodes []string
		for _, status := range tmp.Status.Items {
			egressIPs = append(egressIPs, status.EgressIP)
			nodes = append(nodes, status.Node)
		}
		return egressIPs, nodes
	}

	getEgressIPReassignmentCount := func() int {
		reAssignmentCount := 0
		egressIPs, err := fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().List(context.TODO(), metav1.ListOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		for _, egressIP := range egressIPs.Items {
			if len(egressIP.Spec.EgressIPs) != len(egressIP.Status.Items) {
				reAssignmentCount++
			}
		}
		return reAssignmentCount
	}

	nodeSwitch := func() string {
		_, nodes := getEgressIPStatus(egressIPName)
		if len(nodes) != 1 {
			return ""
		}
		return nodes[0]
	}

	ginkgo.BeforeEach(func() {
		// Restore global default values before each testcase
		config.PrepareTestConfig()
		config.OVNKubernetesFeature.EnableEgressIP = true
		config.OVNKubernetesFeature.EgressIPNodeHealthCheckPort = 1234

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

		fakeOvn = NewFakeOVN(true)
	})

	ginkgo.AfterEach(func() {
		fakeOvn.shutdown()
	})

	getPodAssignmentState := func(pod *v1.Pod) *podAssignmentState {
		fakeOvn.controller.eIPC.podAssignmentMutex.Lock()
		defer fakeOvn.controller.eIPC.podAssignmentMutex.Unlock()
		if pas := fakeOvn.controller.eIPC.podAssignment[getPodKey(pod)]; pas != nil {
			return pas.Clone()
		}
		return nil
	}

	ginkgo.Context("On node UPDATE", func() {

		ginkgotable.DescribeTable("should perform proper OVN transactions when pod is created after node egress label switch",
			func(interconnect bool) {
				app.Action = func(ctx *cli.Context) error {
					config.OVNKubernetesFeature.EnableInterconnect = interconnect
					egressIP := "192.168.126.101"
					node1IPv4 := "192.168.126.202/24"
					node2IPv4 := "192.168.126.51/24"

					egressPod := *newPodWithLabels(namespace, podName, node1Name, podV4IP, egressPodLabel)
					egressNamespace := newNamespace(namespace)
					annotations := map[string]string{
						"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4, ""),
						"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":\"%s\"}", v4NodeSubnet),
					}
					labels := map[string]string{
						"k8s.ovn.org/egress-assignable": "",
					}
					node1 := getNodeObj(node1Name, annotations, labels)
					annotations = map[string]string{
						"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node2IPv4, ""),
						"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":\"%s\"}", v4NodeSubnet),
					}
					labels = map[string]string{}
					node2 := getNodeObj(node2Name, annotations, labels)

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

					fakeOvn.startWithDBSetup(
						libovsdbtest.TestSetup{
							NBData: []libovsdbtest.TestData{
								&nbdb.LogicalRouterPort{
									UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2.Name + "-UUID",
									Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2.Name,
									Networks: []string{node2LogicalRouterIfAddrV4},
								},
								&nbdb.LogicalRouterPort{
									UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name + "-UUID",
									Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name,
									Networks: []string{nodeLogicalRouterIfAddrV4},
								},
								&nbdb.LogicalRouter{
									Name: ovntypes.OVNClusterRouter,
									UUID: ovntypes.OVNClusterRouter + "-UUID",
								},
								&nbdb.LogicalRouter{
									Name: ovntypes.GWRouterPrefix + node1.Name,
									UUID: ovntypes.GWRouterPrefix + node1.Name + "-UUID",
								},
								&nbdb.LogicalRouter{
									Name: ovntypes.GWRouterPrefix + node2.Name,
									UUID: ovntypes.GWRouterPrefix + node2.Name + "-UUID",
								},
								&nbdb.LogicalSwitchPort{
									UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "UUID",
									Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
									Type: "router",
									Options: map[string]string{
										"router-port": types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
									},
								},
								&nbdb.LogicalSwitchPort{
									UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "UUID",
									Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
									Type: "router",
									Options: map[string]string{
										"router-port": types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
									},
								},
							},
						},
						&egressipv1.EgressIPList{
							Items: []egressipv1.EgressIP{eIP},
						},
						&v1.NodeList{
							Items: []v1.Node{node1, node2},
						},
						&v1.NamespaceList{
							Items: []v1.Namespace{*egressNamespace},
						})

					err := fakeOvn.controller.WatchEgressIPNamespaces()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchEgressIPPods()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchEgressNodes()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchEgressIP()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					lsp := &nbdb.LogicalSwitchPort{Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name}
					fakeOvn.controller.nbClient.Get(context.Background(), lsp)
					gomega.Eventually(lsp.Options["nat-addresses"]).Should(gomega.Equal("router"))
					gomega.Eventually(lsp.Options["exclude-lb-vips-from-garp"]).Should(gomega.Equal("true"))

					fakeOvn.patchEgressIPObj(node1Name, egressIP)
					gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
					egressIPs, nodes := getEgressIPStatus(egressIPName)
					gomega.Expect(nodes[0]).To(gomega.Equal(node1.Name))
					gomega.Expect(egressIPs[0]).To(gomega.Equal(egressIP))

					node1.Labels = map[string]string{}
					node2.Labels = map[string]string{
						"k8s.ovn.org/egress-assignable": "",
					}

					_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), &node1, metav1.UpdateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), &node2, metav1.UpdateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					fakeOvn.patchEgressIPObj(node2Name, egressIP)
					gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
					gomega.Eventually(nodeSwitch).Should(gomega.Equal(node2.Name))
					egressIPs, _ = getEgressIPStatus(egressIPName)
					gomega.Expect(egressIPs[0]).To(gomega.Equal(egressIP))

					i, n, _ := net.ParseCIDR(podV4IP + "/23")
					n.IP = i
					fakeOvn.controller.logicalPortCache.add(&egressPod, "", types.DefaultNetworkName, "", nil, []*net.IPNet{n})
					_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(egressPod.Namespace).Create(context.TODO(), &egressPod, metav1.CreateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					expectedNatLogicalPort := "k8s-node2"
					expectedDatabaseState := []libovsdbtest.TestData{
						&nbdb.LogicalRouterPolicy{
							Priority: types.DefaultNoRereoutePriority,
							Match:    "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
							Action:   nbdb.LogicalRouterPolicyActionAllow,
							UUID:     "default-no-reroute-UUID",
						},
						&nbdb.LogicalRouterPolicy{
							Priority: types.DefaultNoRereoutePriority,
							Match:    fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
							Action:   nbdb.LogicalRouterPolicyActionAllow,
							UUID:     "no-reroute-service-UUID",
						},
						&nbdb.LogicalRouterPolicy{
							Priority: types.EgressIPReroutePriority,
							Match:    fmt.Sprintf("ip4.src == %s", egressPod.Status.PodIP),
							Action:   nbdb.LogicalRouterPolicyActionReroute,
							Nexthops: node2LogicalRouterIPv4,
							ExternalIDs: map[string]string{
								"name": eIP.Name,
							},
							UUID: "reroute-UUID",
						},
						&nbdb.NAT{
							UUID:       "egressip-nat-UUID",
							LogicalIP:  podV4IP,
							ExternalIP: egressIP,
							ExternalIDs: map[string]string{
								"name": egressIPName,
							},
							Type:        nbdb.NATTypeSNAT,
							LogicalPort: &expectedNatLogicalPort,
							Options: map[string]string{
								"stateless": "false",
							},
						},
						&nbdb.LogicalRouter{
							Name: ovntypes.GWRouterPrefix + node1.Name,
							UUID: ovntypes.GWRouterPrefix + node1.Name + "-UUID",
						},
						&nbdb.LogicalRouter{
							Name: ovntypes.GWRouterPrefix + node2.Name,
							UUID: ovntypes.GWRouterPrefix + node2.Name + "-UUID",
							Nat:  []string{"egressip-nat-UUID"},
						},
						&nbdb.LogicalRouter{
							Name:     ovntypes.OVNClusterRouter,
							UUID:     ovntypes.OVNClusterRouter + "-UUID",
							Policies: []string{"reroute-UUID", "default-no-reroute-UUID", "no-reroute-service-UUID"},
						},
						&nbdb.LogicalRouterPort{
							UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2.Name + "-UUID",
							Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2.Name,
							Networks: []string{node2LogicalRouterIfAddrV4},
						},
						&nbdb.LogicalRouterPort{
							UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name + "-UUID",
							Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name,
							Networks: []string{nodeLogicalRouterIfAddrV4},
						},
						&nbdb.LogicalSwitchPort{
							UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "UUID",
							Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
							Type: "router",
							Options: map[string]string{
								"router-port":               types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
								"nat-addresses":             "router",
								"exclude-lb-vips-from-garp": "true",
							},
						},
						&nbdb.LogicalSwitchPort{
							UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "UUID",
							Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
							Type: "router",
							Options: map[string]string{
								"router-port":               types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
								"nat-addresses":             "router",
								"exclude-lb-vips-from-garp": "true",
							},
						},
					}

					gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			},
			ginkgotable.Entry("interconnect disabled", false),
			ginkgotable.Entry("interconnect enabled", true),
		)

		ginkgotable.DescribeTable("using EgressNode retry should perform proper OVN transactions when pod is created after node egress label switch",
			func(interconnect bool) {
				config.OVNKubernetesFeature.EnableInterconnect = interconnect
				app.Action = func(ctx *cli.Context) error {

					egressIP := "192.168.126.101"
					node1IPv4 := "192.168.126.202/24"
					node2IPv4 := "192.168.126.51/24"
					node3IPv4 := "192.168.126.0/24"

					egressPod := *newPodWithLabels(namespace, podName, "node3", podV4IP, egressPodLabel)
					egressNamespace := newNamespace(namespace)

					annotations := map[string]string{
						"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4, ""),
						"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":\"%s\"}", v4NodeSubnet),
					}
					labels := map[string]string{
						"k8s.ovn.org/egress-assignable": "",
					}
					node1 := getNodeObj(node1Name, annotations, labels)
					annotations = map[string]string{
						"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node2IPv4, ""),
						"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":\"%s\"}", v4NodeSubnet),
					}
					labels = map[string]string{}
					node2 := getNodeObj(node2Name, annotations, labels)
					annotations = map[string]string{
						"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node3IPv4, ""),
						"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":\"%s\"}", v4NodeSubnet),
					}
					labels = map[string]string{}
					node3 := getNodeObj("node3", annotations, labels)

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
					initialDB := libovsdbtest.TestSetup{
						NBData: []libovsdbtest.TestData{
							&nbdb.LogicalRouterPort{
								UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2.Name + "-UUID",
								Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2.Name,
								Networks: []string{node2LogicalRouterIfAddrV4},
							},
							&nbdb.LogicalRouterPort{
								UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name + "-UUID",
								Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name,
								Networks: []string{nodeLogicalRouterIfAddrV4},
							},
							&nbdb.LogicalRouter{
								Name: ovntypes.OVNClusterRouter,
								UUID: ovntypes.OVNClusterRouter + "-UUID",
							},
							&nbdb.LogicalRouter{
								Name: ovntypes.GWRouterPrefix + node1.Name,
								UUID: ovntypes.GWRouterPrefix + node1.Name + "-UUID",
							},
							&nbdb.LogicalRouter{
								Name: ovntypes.GWRouterPrefix + node2.Name,
								UUID: ovntypes.GWRouterPrefix + node2.Name + "-UUID",
							},
							&nbdb.LogicalSwitchPort{
								UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "UUID",
								Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
								Type: "router",
								Options: map[string]string{
									"router-port": types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
								},
							},
							&nbdb.LogicalSwitchPort{
								UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "UUID",
								Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
								Type: "router",
								Options: map[string]string{
									"router-port": types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
								},
							},
						},
					}
					fakeOvn.startWithDBSetup(
						initialDB,
						&egressipv1.EgressIPList{
							Items: []egressipv1.EgressIP{eIP},
						},
						&v1.NodeList{
							Items: []v1.Node{node1, node2, node3},
						},
						&v1.NamespaceList{
							Items: []v1.Namespace{*egressNamespace},
						})

					err := fakeOvn.controller.WatchEgressIPNamespaces()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchEgressIPPods()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchEgressNodes()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchEgressIP()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					lsp := &nbdb.LogicalSwitchPort{Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name}
					err = fakeOvn.controller.nbClient.Get(context.Background(), lsp)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					gomega.Eventually(lsp.Options["nat-addresses"]).Should(gomega.Equal("router"))
					gomega.Eventually(lsp.Options["exclude-lb-vips-from-garp"]).Should(gomega.Equal("true"))

					lsp = &nbdb.LogicalSwitchPort{Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name}
					err = fakeOvn.controller.nbClient.Get(context.Background(), lsp)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					gomega.Eventually(lsp.Options["nat-addresses"]).Should(gomega.Equal("router"))
					gomega.Eventually(lsp.Options["exclude-lb-vips-from-garp"]).Should(gomega.Equal("true"))

					fakeOvn.patchEgressIPObj(node1Name, egressIP)

					gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
					egressIPs, nodes := getEgressIPStatus(egressIPName)
					gomega.Expect(nodes[0]).To(gomega.Equal(node1.Name))
					gomega.Expect(egressIPs[0]).To(gomega.Equal(egressIP))

					node1.Labels = map[string]string{}
					node2.Labels = map[string]string{
						"k8s.ovn.org/egress-assignable": "",
					}
					node2.Annotations["k8s.ovn.org/host-addresses"] = fmt.Sprintf("[\"%s\",\"%s\"]", "192.168.126.51", "")
					ginkgo.By("Bringing down NBDB")
					// inject transient problem, nbdb is down
					fakeOvn.controller.nbClient.Close()
					gomega.Eventually(func() bool {
						return fakeOvn.controller.nbClient.Connected()
					}).Should(gomega.BeFalse())
					err = fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Delete(context.TODO(), node1.Name, metav1.DeleteOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), &node2, metav1.UpdateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					// sleep long enough for TransactWithRetry to fail, causing egressnode operations to fail
					// there is a chance that both egressnode events(node1 removal and node2 update) will end up in the same event queue
					// sleep for double the time to allow for two consecutive TransactWithRetry timeouts
					time.Sleep(2 * (types.OVSDBTimeout + time.Second))
					// check to see if the retry cache has an entry
					key1 := node1.Name
					ginkgo.By("retry entry: old obj should not be nil, new obj should be nil")
					retry.CheckRetryObjectMultipleFieldsEventually(
						key1,
						fakeOvn.controller.retryEgressNodes,
						gomega.Not(gomega.BeNil()), // oldObj should not be nil
						gomega.BeNil(),             // newObj should be nil
					)

					key2 := node2.Name
					ginkgo.By("retry entry: old obj should be nil, new obj should not be nil, config should not be nil")
					retry.CheckRetryObjectMultipleFieldsEventually(
						key2,
						fakeOvn.controller.retryEgressNodes,
						gomega.BeNil(),             // oldObj should be nil
						gomega.Not(gomega.BeNil()), // newObj should not be nil
						gomega.Not(gomega.BeNil()), // config should not be nil
					)
					fakeOvn.patchEgressIPObj(node2Name, egressIP)
					connCtx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
					defer cancel()
					resetNBClient(connCtx, fakeOvn.controller.nbClient)
					retry.SetRetryObjWithNoBackoff(key1, fakeOvn.controller.retryEgressNodes)
					retry.SetRetryObjWithNoBackoff(key2, fakeOvn.controller.retryEgressNodes)
					fakeOvn.controller.retryEgressNodes.RequestRetryObjs()
					// check the cache no longer has the entry
					retry.CheckRetryObjectEventually(key1, false, fakeOvn.controller.retryEgressNodes)
					retry.CheckRetryObjectEventually(key2, false, fakeOvn.controller.retryEgressNodes)

					i, n, _ := net.ParseCIDR(podV4IP + "/23")
					n.IP = i
					fakeOvn.controller.logicalPortCache.add(&egressPod, "", types.DefaultNetworkName, "", nil, []*net.IPNet{n})
					_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(egressPod.Namespace).Create(context.TODO(), &egressPod, metav1.CreateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					expectedNatLogicalPort := "k8s-node2"
					expectedDatabaseState := []libovsdbtest.TestData{
						&nbdb.LogicalRouterPolicy{
							Priority: types.EgressIPReroutePriority,
							Match:    fmt.Sprintf("ip4.src == %s", egressPod.Status.PodIP),
							Action:   nbdb.LogicalRouterPolicyActionReroute,
							Nexthops: node2LogicalRouterIPv4,
							ExternalIDs: map[string]string{
								"name": eIP.Name,
							},
							UUID: "reroute-UUID",
						},
						&nbdb.LogicalRouterPolicy{
							Priority: types.DefaultNoRereoutePriority,
							Match:    "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
							Action:   nbdb.LogicalRouterPolicyActionAllow,
							UUID:     "default-no-reroute-UUID",
						},
						&nbdb.LogicalRouterPolicy{
							Priority: types.DefaultNoRereoutePriority,
							Match:    fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
							Action:   nbdb.LogicalRouterPolicyActionAllow,
							UUID:     "no-reroute-service-UUID",
						},
						&nbdb.LogicalRouterPolicy{
							Priority: types.DefaultNoRereoutePriority,
							Match:    "(ip4.src == $a4548040316634674295 || ip4.src == $a13607449821398607916) && ip4.dst == $a14918748166599097711",
							Action:   nbdb.LogicalRouterPolicyActionAllow,
							Options:  map[string]string{"pkt_mark": "1008"},
							UUID:     "no-reroute-node-UUID",
						},
						&nbdb.NAT{
							UUID:       "egressip-nat-UUID",
							LogicalIP:  podV4IP,
							ExternalIP: egressIP,
							ExternalIDs: map[string]string{
								"name": egressIPName,
							},
							Type:        nbdb.NATTypeSNAT,
							LogicalPort: &expectedNatLogicalPort,
							Options: map[string]string{
								"stateless": "false",
							},
						},
						&nbdb.LogicalRouter{
							Name: ovntypes.GWRouterPrefix + node1.Name,
							UUID: ovntypes.GWRouterPrefix + node1.Name + "-UUID",
						},
						&nbdb.LogicalRouter{
							Name: ovntypes.GWRouterPrefix + node2.Name,
							UUID: ovntypes.GWRouterPrefix + node2.Name + "-UUID",
							Nat:  []string{"egressip-nat-UUID"},
						},
						&nbdb.LogicalRouter{
							Name:     ovntypes.OVNClusterRouter,
							UUID:     ovntypes.OVNClusterRouter + "-UUID",
							Policies: []string{"reroute-UUID", "default-no-reroute-UUID", "no-reroute-service-UUID", "no-reroute-node-UUID"},
						},
						&nbdb.LogicalRouterPort{
							UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2.Name + "-UUID",
							Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2.Name,
							Networks: []string{node2LogicalRouterIfAddrV4},
						},
						&nbdb.LogicalRouterPort{
							UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name + "-UUID",
							Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name,
							Networks: []string{nodeLogicalRouterIfAddrV4},
						},
						&nbdb.LogicalSwitchPort{
							UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "UUID",
							Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
							Type: "router",
							Options: map[string]string{
								"router-port": types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
								/* GARP is deleted since node has been deleted */
								// "nat-addresses":             "router",
								// "exclude-lb-vips-from-garp": "true",
							},
						},
						&nbdb.LogicalSwitchPort{
							UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "UUID",
							Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
							Type: "router",
							Options: map[string]string{
								"router-port":               types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
								"nat-addresses":             "router",
								"exclude-lb-vips-from-garp": "true",
							},
						},
					}

					gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			},
			ginkgotable.Entry("interconnect disabled", false),
			ginkgotable.Entry("interconnect enabled", true), // all 3 nodes in same zone, so behaves like non-ic
		)

		ginkgotable.DescribeTable("should perform proper OVN transactions when namespace and pod is created after node egress label switch",
			func(interconnect bool, node1Zone, node2Zone string) {
				config.OVNKubernetesFeature.EnableInterconnect = interconnect
				app.Action = func(ctx *cli.Context) error {

					egressIP := "192.168.126.101"
					node1IPv4 := "192.168.126.202/24"
					node2IPv4 := "192.168.126.51/24"

					egressPod := *newPodWithLabels(namespace, podName, node1Name, podV4IP, egressPodLabel)
					egressNamespace := newNamespace(namespace)

					annotations := map[string]string{
						"k8s.ovn.org/node-primary-ifaddr":             fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4, ""),
						"k8s.ovn.org/node-subnets":                    fmt.Sprintf("{\"default\":\"%s\"}", v4NodeSubnet),
						"k8s.ovn.org/node-transit-switch-port-ifaddr": "{\"ipv4\":\"168.254.0.2/16\"}", // used only for ic=true test
						"k8s.ovn.org/zone-name":                       node1Zone,                       // used only for ic=true test
					}
					labels := map[string]string{
						"k8s.ovn.org/egress-assignable": "",
					}
					node1 := getNodeObj(node1Name, annotations, labels)
					annotations = map[string]string{
						"k8s.ovn.org/node-primary-ifaddr":             fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node2IPv4, ""),
						"k8s.ovn.org/node-subnets":                    fmt.Sprintf("{\"default\":\"%s\"}", v4NodeSubnet),
						"k8s.ovn.org/node-transit-switch-port-ifaddr": "{\"ipv4\":\"168.254.0.3/16\"}", // used only for ic=true test
						"k8s.ovn.org/zone-name":                       node2Zone,                       // used only for ic=true test
					}
					labels = map[string]string{}
					node2 := getNodeObj(node2Name, annotations, labels)

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

					fakeOvn.startWithDBSetup(
						libovsdbtest.TestSetup{
							NBData: []libovsdbtest.TestData{
								&nbdb.LogicalRouterPort{
									UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name + "-UUID",
									Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name,
									Networks: []string{nodeLogicalRouterIfAddrV4},
								},
								&nbdb.LogicalRouterPort{
									UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2.Name + "-UUID",
									Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2.Name,
									Networks: []string{node2LogicalRouterIfAddrV4},
								},
								&nbdb.LogicalRouter{
									Name: ovntypes.OVNClusterRouter,
									UUID: ovntypes.OVNClusterRouter + "-UUID",
								},
								&nbdb.LogicalRouter{
									Name: ovntypes.GWRouterPrefix + node1.Name,
									UUID: ovntypes.GWRouterPrefix + node1.Name + "-UUID",
								},
								&nbdb.LogicalRouter{
									Name: ovntypes.GWRouterPrefix + node2.Name,
									UUID: ovntypes.GWRouterPrefix + node2.Name + "-UUID",
									Nat:  nil,
								},
								&nbdb.LogicalSwitchPort{
									UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "UUID",
									Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
									Type: "router",
									Options: map[string]string{
										"router-port": types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
									},
								},
								&nbdb.LogicalSwitchPort{
									UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "UUID",
									Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
									Type: "router",
									Options: map[string]string{
										"router-port": types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
									},
								},
							},
						},
						&egressipv1.EgressIPList{
							Items: []egressipv1.EgressIP{eIP},
						},
						&v1.NodeList{
							Items: []v1.Node{node1, node2},
						})

					i, n, _ := net.ParseCIDR(podV4IP + "/23")
					n.IP = i
					fakeOvn.controller.logicalPortCache.add(&egressPod, "", types.DefaultNetworkName, "", nil, []*net.IPNet{n})

					err := fakeOvn.controller.WatchEgressIPNamespaces()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchEgressIPPods()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchEgressNodes()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					fakeOvn.patchEgressIPObj(node1Name, egressIP)

					lsp := &nbdb.LogicalSwitchPort{Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name}
					fakeOvn.controller.nbClient.Get(context.Background(), lsp)
					if node1Zone == "global" {
						// GARP is configured only for nodes in local zones, the master of the remote zone will do it for the remote nodes
						gomega.Eventually(lsp.Options["nat-addresses"]).Should(gomega.Equal("router"))
						gomega.Eventually(lsp.Options["exclude-lb-vips-from-garp"]).Should(gomega.Equal("true"))
					}

					err = fakeOvn.controller.WatchEgressIP()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
					egressIPs, nodes := getEgressIPStatus(egressIPName)
					gomega.Expect(nodes[0]).To(gomega.Equal(node1.Name))
					gomega.Expect(egressIPs[0]).To(gomega.Equal(egressIP))

					node1.Labels = map[string]string{}
					node2.Labels = map[string]string{
						"k8s.ovn.org/egress-assignable": "",
					}

					_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), &node1, metav1.UpdateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), &node2, metav1.UpdateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					fakeOvn.patchEgressIPObj(node2Name, egressIP)
					gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
					gomega.Eventually(nodeSwitch).Should(gomega.Equal(node2.Name))
					egressIPs, _ = getEgressIPStatus(egressIPName)
					gomega.Expect(egressIPs[0]).To(gomega.Equal(egressIP))

					_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Create(context.TODO(), egressNamespace, metav1.CreateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(egressPod.Namespace).Create(context.TODO(), &egressPod, metav1.CreateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					expectedNatLogicalPort := "k8s-node2"
					reroutePolicyNextHop := node2LogicalRouterIPv4
					if interconnect && node1Zone != node2Zone {
						reroutePolicyNextHop = []string{"168.254.0.3"} // node2's transit switch portIP
					}
					expectedDatabaseState := []libovsdbtest.TestData{
						getReRoutePolicy(egressPod.Status.PodIP, "4", reroutePolicyNextHop),
						&nbdb.LogicalRouterPolicy{
							Priority: types.DefaultNoRereoutePriority,
							Match:    "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
							Action:   nbdb.LogicalRouterPolicyActionAllow,
							UUID:     "default-no-reroute-UUID",
						},
						&nbdb.LogicalRouterPolicy{
							Priority: types.DefaultNoRereoutePriority,
							Match:    fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
							Action:   nbdb.LogicalRouterPolicyActionAllow,
							UUID:     "no-reroute-service-UUID",
						},
						&nbdb.LogicalRouter{
							Name: ovntypes.GWRouterPrefix + node1.Name,
							UUID: ovntypes.GWRouterPrefix + node1.Name + "-UUID",
						},
						&nbdb.LogicalRouter{
							Name: ovntypes.GWRouterPrefix + node2.Name,
							UUID: ovntypes.GWRouterPrefix + node2.Name + "-UUID",
						},
						&nbdb.LogicalRouter{
							Name:     ovntypes.OVNClusterRouter,
							UUID:     ovntypes.OVNClusterRouter + "-UUID",
							Policies: []string{"reroute-UUID", "default-no-reroute-UUID", "no-reroute-service-UUID"},
						},
						&nbdb.LogicalRouterPort{
							UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name + "-UUID",
							Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name,
							Networks: []string{nodeLogicalRouterIfAddrV4},
						},
						&nbdb.LogicalRouterPort{
							UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2.Name + "-UUID",
							Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2.Name,
							Networks: []string{node2LogicalRouterIfAddrV4},
						},
						&nbdb.LogicalSwitchPort{
							UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "UUID",
							Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
							Type: "router",
							Options: map[string]string{
								"router-port": types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
							},
						},
						&nbdb.LogicalSwitchPort{
							UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "UUID",
							Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
							Type: "router",
							Options: map[string]string{
								"router-port": types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
							},
						},
					}
					if node1Zone == "global" {
						// GARP is configured only for nodes in local zones, the master of the remote zone will do it for the remote nodes
						expectedDatabaseState[8].(*nbdb.LogicalSwitchPort).Options["nat-addresses"] = "router"
						expectedDatabaseState[8].(*nbdb.LogicalSwitchPort).Options["exclude-lb-vips-from-garp"] = "true"
					}
					if node2Zone != "remote" {
						// either not interconnect or egressNode is in localZone
						eipSNAT := getEIPSNAT(podV4IP, egressIP, expectedNatLogicalPort)
						expectedDatabaseState = append(expectedDatabaseState, eipSNAT)
						expectedDatabaseState[4].(*nbdb.LogicalRouter).Nat = []string{"egressip-nat-UUID"} // 4th item is node2's GR
						// add GARP config only if node is in local zone
						expectedDatabaseState[9].(*nbdb.LogicalSwitchPort).Options["nat-addresses"] = "router"
						expectedDatabaseState[9].(*nbdb.LogicalSwitchPort).Options["exclude-lb-vips-from-garp"] = "true"
					}
					if node2Zone != node1Zone && node2Zone == "global" {
						// node2 will have a static route added for the pod
						expectedDatabaseState = append(expectedDatabaseState, getReRouteStaticRoute(podV4IP, node2LogicalRouterIPv4[0]))
						expectedDatabaseState[5].(*nbdb.LogicalRouter).StaticRoutes = []string{"reroute-static-route-UUID"}
						expectedDatabaseState[5].(*nbdb.LogicalRouter).Policies = []string{"default-no-reroute-UUID", "no-reroute-service-UUID"}
						expectedDatabaseState = expectedDatabaseState[1:] // reroute logical router policy is in remote zone, hence not visible
					}

					gomega.Eventually(fakeOvn.nbClient, inspectTimeout).Should(libovsdbtest.HaveData(expectedDatabaseState))

					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			},
			ginkgotable.Entry("interconnect disabled; non-ic - single zone setup", false, "global", "global"),
			ginkgotable.Entry("interconnect enabled; node1 and node2 in global zones", true, "global", "global"),
			// will showcase localzone setup - master is in pod's zone where pod's reroute policy towards egressNode will be done.
			// NOTE: SNAT won't be visible because its in remote zone
			ginkgotable.Entry("interconnect enabled; node1 in global and node2 in remote zones", true, "global", "remote"),
			// will showcase localzone setup - master is in egress node's zone where pod's SNAT policy and static route will be done.
			// NOTE: reroute policy won't be visible because its in remote zone (pod is in remote zone)
			ginkgotable.Entry("interconnect enabled; node1 in remote and node2 in global zones", true, "remote", "global"),
		)
	})

	ginkgo.Context("On node DELETE", func() {

		ginkgotable.DescribeTable("should perform proper OVN transactions when node's gateway objects are already deleted",
			func(interconnect bool, node1Zone, node2Zone string) {
				config.OVNKubernetesFeature.EnableInterconnect = interconnect
				app.Action = func(ctx *cli.Context) error {

					egressIP := "192.168.126.101"
					node1IPv4 := "192.168.126.202/24"
					node2IPv4 := "192.168.126.51/24"

					egressPod := *newPodWithLabels(namespace, podName, node1Name, podV4IP, egressPodLabel)
					egressNamespace := newNamespace(namespace)
					annotations := map[string]string{
						"k8s.ovn.org/node-primary-ifaddr":             fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4, ""),
						"k8s.ovn.org/node-subnets":                    fmt.Sprintf("{\"default\":\"%s\"}", v4NodeSubnet),
						"k8s.ovn.org/node-transit-switch-port-ifaddr": "{\"ipv4\":\"168.254.0.2/16\"}", // used only for ic=true test
						"k8s.ovn.org/zone-name":                       node1Zone,
					}
					labels := map[string]string{
						"k8s.ovn.org/egress-assignable": "",
					}
					node1 := getNodeObj(node1Name, annotations, labels)
					annotations = map[string]string{
						"k8s.ovn.org/node-primary-ifaddr":             fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node2IPv4, ""),
						"k8s.ovn.org/node-subnets":                    fmt.Sprintf("{\"default\":\"%s\"}", v4NodeSubnet),
						"k8s.ovn.org/node-transit-switch-port-ifaddr": "{\"ipv4\":\"168.254.0.3/16\"}", // used only for ic=true test
						"k8s.ovn.org/zone-name":                       node2Zone,                       // used only for ic=true test
					}
					labels = map[string]string{}
					node2 := getNodeObj(node2Name, annotations, labels)

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

					fakeOvn.startWithDBSetup(
						libovsdbtest.TestSetup{
							NBData: []libovsdbtest.TestData{
								&nbdb.LogicalRouterPort{
									UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name + "-UUID",
									Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name,
									Networks: []string{nodeLogicalRouterIfAddrV4},
								},
								&nbdb.LogicalRouterPort{
									UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2.Name + "-UUID",
									Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2.Name,
									Networks: []string{node2LogicalRouterIfAddrV4},
								},
								&nbdb.LogicalRouter{
									Name: ovntypes.OVNClusterRouter,
									UUID: ovntypes.OVNClusterRouter + "-UUID",
								},
								&nbdb.LogicalRouter{
									Name: ovntypes.GWRouterPrefix + node1.Name,
									UUID: ovntypes.GWRouterPrefix + node1.Name + "-UUID",
								},
								&nbdb.LogicalRouter{
									Name: ovntypes.GWRouterPrefix + node2.Name,
									UUID: ovntypes.GWRouterPrefix + node2.Name + "-UUID",
								},
								&nbdb.LogicalSwitchPort{
									UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "UUID",
									Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
									Type: "router",
									Options: map[string]string{
										"router-port": types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
									},
								},
								&nbdb.LogicalSwitchPort{
									UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "UUID",
									Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
									Type: "router",
									Options: map[string]string{
										"router-port": types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
									},
								},
								&nbdb.LogicalSwitch{
									UUID: types.OVNJoinSwitch + "-UUID",
									Name: types.OVNJoinSwitch,
								},
							},
						},
						&egressipv1.EgressIPList{
							Items: []egressipv1.EgressIP{eIP},
						},
						&v1.NodeList{
							Items: []v1.Node{node1, node2},
						},
						&v1.NamespaceList{
							Items: []v1.Namespace{*egressNamespace},
						})

					err := fakeOvn.controller.WatchEgressIPNamespaces()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchEgressIPPods()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchEgressNodes()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchEgressIP()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					fakeOvn.patchEgressIPObj(node1Name, egressIP)

					lsp := &nbdb.LogicalSwitchPort{Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name}
					fakeOvn.controller.nbClient.Get(context.Background(), lsp)
					if node1Zone == "global" {
						// only if node is global we add this
						gomega.Eventually(lsp.Options["nat-addresses"]).Should(gomega.Equal("router"))
						gomega.Eventually(lsp.Options["exclude-lb-vips-from-garp"]).Should(gomega.Equal("true"))
					}

					gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
					egressIPs, nodes := getEgressIPStatus(egressIPName)
					gomega.Expect(nodes[0]).To(gomega.Equal(node1.Name))
					gomega.Expect(egressIPs[0]).To(gomega.Equal(egressIP))

					node2.Labels = map[string]string{
						"k8s.ovn.org/egress-assignable": "",
					}
					_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), &node2, metav1.UpdateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					i, n, _ := net.ParseCIDR(podV4IP + "/23")
					n.IP = i
					fakeOvn.controller.logicalPortCache.add(&egressPod, "", types.DefaultNetworkName, "", nil, []*net.IPNet{n})
					_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(egressPod.Namespace).Create(context.TODO(), &egressPod, metav1.CreateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					expectedNatLogicalPort := "k8s-node1"
					primarySNAT := getEIPSNAT(podV4IP, egressIP, expectedNatLogicalPort)
					primarySNAT.UUID = "egressip-nat1-UUID"
					expectedDatabaseState := []libovsdbtest.TestData{
						getReRoutePolicy(egressPod.Status.PodIP, "4", nodeLogicalRouterIPv4),
						primarySNAT,
						&nbdb.LogicalRouterPolicy{
							Priority: types.DefaultNoRereoutePriority,
							Match:    "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
							Action:   nbdb.LogicalRouterPolicyActionAllow,
							UUID:     "default-no-reroute-UUID",
						},
						&nbdb.LogicalRouterPolicy{
							Priority: types.DefaultNoRereoutePriority,
							Match:    fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
							Action:   nbdb.LogicalRouterPolicyActionAllow,
							UUID:     "no-reroute-service-UUID",
						},
						&nbdb.LogicalRouter{
							Name: ovntypes.GWRouterPrefix + node1.Name,
							UUID: ovntypes.GWRouterPrefix + node1.Name + "-UUID",
							Nat:  []string{"egressip-nat1-UUID"},
						},
						&nbdb.LogicalRouter{
							Name: ovntypes.GWRouterPrefix + node2.Name,
							UUID: ovntypes.GWRouterPrefix + node2.Name + "-UUID",
							Nat:  []string{},
						},
						&nbdb.LogicalRouter{
							Name:     ovntypes.OVNClusterRouter,
							UUID:     ovntypes.OVNClusterRouter + "-UUID",
							Policies: []string{"reroute-UUID", "default-no-reroute-UUID", "no-reroute-service-UUID"},
						},
						&nbdb.LogicalRouterPort{
							UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name + "-UUID",
							Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name,
							Networks: []string{nodeLogicalRouterIfAddrV4},
						},
						&nbdb.LogicalRouterPort{
							UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2.Name + "-UUID",
							Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2.Name,
							Networks: []string{node2LogicalRouterIfAddrV4},
						},
						&nbdb.LogicalSwitchPort{
							UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "UUID",
							Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
							Type: "router",
							Options: map[string]string{
								"router-port": types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
							},
						},
						&nbdb.LogicalSwitchPort{
							UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "UUID",
							Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
							Type: "router",
							Options: map[string]string{
								"router-port": types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
							},
						},
						&nbdb.LogicalSwitch{
							UUID: types.OVNJoinSwitch + "-UUID",
							Name: types.OVNJoinSwitch,
						},
					}
					if node2Zone != "remote" {
						// add GARP config only if node is in local zone
						expectedDatabaseState[10].(*nbdb.LogicalSwitchPort).Options["nat-addresses"] = "router"
						expectedDatabaseState[10].(*nbdb.LogicalSwitchPort).Options["exclude-lb-vips-from-garp"] = "true"
					}
					if node1Zone != "remote" {
						// add GARP config only if node is in local zone
						expectedDatabaseState[9].(*nbdb.LogicalSwitchPort).Options["nat-addresses"] = "router"
						expectedDatabaseState[9].(*nbdb.LogicalSwitchPort).Options["exclude-lb-vips-from-garp"] = "true"
					} else {
						// if node1 where the pod lives is remote we can't see the EIP setup done since master belongs to local zone
						expectedDatabaseState[4].(*nbdb.LogicalRouter).Nat = []string{}
						expectedDatabaseState[6].(*nbdb.LogicalRouter).Policies = []string{"default-no-reroute-UUID", "no-reroute-service-UUID"}
						expectedDatabaseState = expectedDatabaseState[2:]
					}
					gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

					err = fakeOvn.controller.gatewayCleanup(node1Name) // simulate an already deleted node
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					err = fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Delete(context.TODO(), node1Name, metav1.DeleteOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					// NOTE: This test checks if plumbing is removed when node is gone but pod on the node is still present (unusual scenario)
					// Thus we need to check the cache state to verify things in unit tests to avoid races - we don't control the order of
					// node's deletion removing the entry from localZonesCache versus the add happening for the pod.
					// (in real env this won't be a problem since eventually things will reconcile as pod will also be gone if node is gone)
					gomega.Eventually(func() bool {
						_, ok := fakeOvn.controller.eIPC.nodeZoneState.Load(egressPod.Spec.NodeName)
						return ok
					}).Should(gomega.BeFalse())

					// W0608 12:53:33.728205 1161455 egressip.go:2030] Unable to retrieve gateway IP for node: node1, protocol is IPv6: false, err: attempt at finding node gateway router network information failed, err: unable to find router port rtoj-GR_node1: object not found
					// 2023-04-25T11:01:13.2804834Z W0425 11:01:13.280407   21055 egressip.go:2036] Unable to fetch transit switch IP for node: node1: err: failed to get node node1: node "node1" not found
					fakeOvn.patchEgressIPObj(node2Name, egressIP)
					gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
					gomega.Eventually(nodeSwitch).Should(gomega.Equal(node2.Name)) // egressIP successfully reassigned to node2
					egressIPs, _ = getEgressIPStatus(egressIPName)
					gomega.Expect(egressIPs[0]).To(gomega.Equal(egressIP))

					expectedNatLogicalPort = "k8s-node2"
					eipSNAT := getEIPSNAT(podV4IP, egressIP, expectedNatLogicalPort)
					expectedDatabaseState = []libovsdbtest.TestData{
						getReRoutePolicy(egressPod.Status.PodIP, "4", node2LogicalRouterIPv4),
						&nbdb.LogicalRouterPolicy{
							Priority: types.DefaultNoRereoutePriority,
							Match:    "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
							Action:   nbdb.LogicalRouterPolicyActionAllow,
							UUID:     "default-no-reroute-UUID",
						},
						&nbdb.LogicalRouterPolicy{
							Priority: types.DefaultNoRereoutePriority,
							Match:    fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
							Action:   nbdb.LogicalRouterPolicyActionAllow,
							UUID:     "no-reroute-service-UUID",
						},
						&nbdb.LogicalRouter{
							Name: ovntypes.GWRouterPrefix + node2.Name,
							UUID: ovntypes.GWRouterPrefix + node2.Name + "-UUID",
						},
						&nbdb.LogicalRouter{
							Name:     ovntypes.OVNClusterRouter,
							UUID:     ovntypes.OVNClusterRouter + "-UUID",
							Policies: []string{"reroute-UUID", "default-no-reroute-UUID", "no-reroute-service-UUID"},
						},
						&nbdb.LogicalRouterPort{
							UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2.Name + "-UUID",
							Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2.Name,
							Networks: []string{node2LogicalRouterIfAddrV4},
						},
						&nbdb.LogicalSwitchPort{
							UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "UUID",
							Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
							Type: "router",
							Options: map[string]string{
								"router-port": types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
							},
						},
						&nbdb.LogicalSwitchPort{
							UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "UUID",
							Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
							Type: "router",
							Options: map[string]string{
								"router-port": types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
							},
						},
						&nbdb.LogicalSwitch{
							UUID: types.OVNJoinSwitch + "-UUID",
							Name: types.OVNJoinSwitch,
						},
					}
					if node2Zone != "remote" {
						// either not interconnect or egressNode is in localZone
						expectedDatabaseState = append(expectedDatabaseState, eipSNAT)
						expectedDatabaseState[3].(*nbdb.LogicalRouter).Nat = []string{"egressip-nat-UUID"} // 4th item is node2's GR
						// add GARP config only if node is in local zone
						expectedDatabaseState[7].(*nbdb.LogicalSwitchPort).Options["nat-addresses"] = "router"
						expectedDatabaseState[7].(*nbdb.LogicalSwitchPort).Options["exclude-lb-vips-from-garp"] = "true"
					}
					if node1Zone == "global" {
						// even if node1's GR is gone already, in the libovsdb test framework we need to explicitly remove the NATs from GR.
						// This won't be a problem in real env, See https://github.com/ovn-org/libovsdb/issues/338 for details.
						// Hence we will be left with a stale SNAT
						// in non-IC setup, this is not dependent on localZoneNodes cache; we nuke all SNATs for this pod on all nodes
						// hence we need to do this only when its IC and pod is in local zone
						expectedDatabaseState = append(expectedDatabaseState, primarySNAT)
					}
					// all cases: reroute logical router policy is gone and won't be recreated since node1 is deleted - that is where the pod lives
					// NOTE: This test is not really a real scenario, it depicts a transient state.
					expectedDatabaseState[4].(*nbdb.LogicalRouter).Policies = []string{"default-no-reroute-UUID", "no-reroute-service-UUID"}
					expectedDatabaseState = expectedDatabaseState[1:]

					gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			},
			ginkgotable.Entry("interconnect disabled; non-ic - single zone setup", false, "global", "global"),
			ginkgotable.Entry("interconnect enabled; node1 and node2 in global zones", true, "global", "global"),
			// will showcase localzone setup - master is in pod's zone where pod's reroute policy towards egressNode will be done.
			// NOTE: SNAT won't be visible because its in remote zone
			ginkgotable.Entry("interconnect enabled; node1 in global and node2 in remote zones", true, "global", "remote"),
			// will showcase localzone setup - master is in egress node's zone where pod's SNAT policy and static route will* be done.
			// * the static route won't be visible because the pod's node node1 is getting deleted in this test
			// NOTE: reroute policy won't be visible because its in remote zone (pod is in remote zone)
			ginkgotable.Entry("interconnect enabled; node1 in remote and node2 in global zones", true, "remote", "global"),
		)
	})

	ginkgo.Context("IPv6 on pod UPDATE", func() {

		ginkgotable.DescribeTable("should remove OVN pod egress setup when EgressIP stops matching pod label",
			func(interconnect, isnode1Local, isnode2Local bool) {
				config.OVNKubernetesFeature.EnableInterconnect = interconnect
				app.Action = func(ctx *cli.Context) error {

					egressIP := net.ParseIP("0:0:0:0:0:feff:c0a8:8e0d")

					egressPod := *newPodWithLabels(namespace, podName, node1Name, podV6IP, egressPodLabel)
					egressNamespace := newNamespace(namespace)
					node2IPv4 := "192.168.126.202/24"
					annotations := map[string]string{
						"k8s.ovn.org/node-primary-ifaddr":             fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node2IPv4, ""),
						"k8s.ovn.org/node-subnets":                    fmt.Sprintf("{\"default\":\"%s\"}", v6NodeSubnet),
						"k8s.ovn.org/node-transit-switch-port-ifaddr": "{\"ipv4\":\"168.254.0.2/16\", \"ipv6\": \"fd97::2/64\"}", // used only for ic=true test
					}
					node2 := getNodeObj(node2Name, annotations, map[string]string{}) // add node to avoid errori-ing out on transit switch IP fetch

					fakeOvn.startWithDBSetup(
						libovsdbtest.TestSetup{
							NBData: []libovsdbtest.TestData{
								&nbdb.LogicalRouterPort{
									UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2Name + "-UUID",
									Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2Name,
									Networks: []string{nodeLogicalRouterIfAddrV6},
								},
								&nbdb.LogicalRouter{
									Name: ovntypes.OVNClusterRouter,
									UUID: ovntypes.OVNClusterRouter + "-UUID",
								},
								&nbdb.LogicalRouter{
									Name: ovntypes.GWRouterPrefix + node1Name,
									UUID: ovntypes.GWRouterPrefix + node1Name + "-UUID",
								},
								&nbdb.LogicalRouter{
									Name: ovntypes.GWRouterPrefix + node2Name,
									UUID: ovntypes.GWRouterPrefix + node2Name + "-UUID",
									Nat:  nil,
								},
							},
						},
						&v1.NamespaceList{
							Items: []v1.Namespace{*egressNamespace},
						},
						&v1.PodList{
							Items: []v1.Pod{egressPod},
						},
						&v1.NodeList{
							Items: []v1.Node{node2},
						},
					)

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

					i, n, _ := net.ParseCIDR(podV6IP + "/23")
					n.IP = i
					fakeOvn.controller.logicalPortCache.add(&egressPod, "", types.DefaultNetworkName, "", nil, []*net.IPNet{n})

					err := fakeOvn.controller.WatchEgressIPNamespaces()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchEgressIPPods()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchEgressIP()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					// hack pod to be in the provided zone
					fakeOvn.controller.eIPC.nodeZoneState.Store(node1Name, isnode1Local)
					fakeOvn.controller.eIPC.nodeZoneState.Store(node2Name, isnode2Local)
					if isnode1Local {
						fakeOvn.controller.localZoneNodes.Store(node1Name, true)
					}
					if isnode2Local {
						fakeOvn.controller.localZoneNodes.Store(node2Name, true)
					}

					_, err = fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Create(context.TODO(), &eIP, metav1.CreateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					fakeOvn.patchEgressIPObj(node2Name, egressIP.String())

					gomega.Eventually(getEgressIPStatusLen(eIP.Name)).Should(gomega.Equal(1))

					expectedNatLogicalPort := "k8s-node2"
					expectedDatabaseState := []libovsdbtest.TestData{
						getReRoutePolicy(egressPod.Status.PodIP, "6", nodeLogicalRouterIPv6),
						getEIPSNAT(podV6IP, egressIP.String(), expectedNatLogicalPort),
						&nbdb.LogicalRouter{
							Name:     ovntypes.OVNClusterRouter,
							UUID:     ovntypes.OVNClusterRouter + "-UUID",
							Policies: []string{"reroute-UUID"},
						},
						&nbdb.LogicalRouterPort{
							UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2Name + "-UUID",
							Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2Name,
							Networks: []string{nodeLogicalRouterIfAddrV6},
						},
						&nbdb.LogicalRouter{
							Name: ovntypes.GWRouterPrefix + node1Name,
							UUID: ovntypes.GWRouterPrefix + node1Name + "-UUID",
						},
						&nbdb.LogicalRouter{
							Name: ovntypes.GWRouterPrefix + node2Name,
							UUID: ovntypes.GWRouterPrefix + node2Name + "-UUID",
							Nat:  []string{"egressip-nat-UUID"},
						},
					}
					if !isnode1Local {
						// case4: egressNode is in different zone than pod and egressNode is in local zone, so static reroute will be visible
						expectedDatabaseState = append(expectedDatabaseState, getReRouteStaticRoute(egressPod.Status.PodIP, nodeLogicalRouterIPv6[0]))
						expectedDatabaseState[2].(*nbdb.LogicalRouter).Policies = []string{}
						expectedDatabaseState[2].(*nbdb.LogicalRouter).StaticRoutes = []string{"reroute-static-route-UUID"}
						expectedDatabaseState = expectedDatabaseState[1:]
					}
					if !isnode2Local {
						// case3: pod's SNAT is not visible because egress node is remote
						expectedDatabaseState[5].(*nbdb.LogicalRouter).Nat = []string{}
						expectedDatabaseState = expectedDatabaseState[2:]
						// add policy with nextHop towards egressNode's transit switchIP
						expectedDatabaseState = append(expectedDatabaseState, getReRoutePolicy(egressPod.Status.PodIP, "6", []string{"fd97::2"}))
					}
					gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

					egressIPs, nodes := getEgressIPStatus(eIP.Name)
					gomega.Expect(nodes[0]).To(gomega.Equal(node2Name))
					gomega.Expect(egressIPs[0]).To(gomega.Equal(egressIP.String()))

					podUpdate := newPod(namespace, podName, node1Name, podV6IP)

					_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(egressPod.Namespace).Update(context.TODO(), podUpdate, metav1.UpdateOptions{})
					gomega.Expect(err).ToNot(gomega.HaveOccurred())
					gomega.Eventually(getEgressIPStatusLen(eIP.Name)).Should(gomega.Equal(1))

					expectedDatabaseState = []libovsdbtest.TestData{
						&nbdb.LogicalRouter{
							Name:     ovntypes.OVNClusterRouter,
							UUID:     ovntypes.OVNClusterRouter + "-UUID",
							Policies: []string{},
						},
						&nbdb.LogicalRouterPort{
							UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2Name + "-UUID",
							Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2Name,
							Networks: []string{nodeLogicalRouterIfAddrV6},
						},
						&nbdb.LogicalRouter{
							Name: ovntypes.GWRouterPrefix + node1Name,
							UUID: ovntypes.GWRouterPrefix + node1Name + "-UUID",
						},
						&nbdb.LogicalRouter{
							Name: ovntypes.GWRouterPrefix + node2Name,
							UUID: ovntypes.GWRouterPrefix + node2Name + "-UUID",
							Nat:  nil,
						},
					}

					gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			},
			ginkgotable.Entry("interconnect disabled; non-ic - single zone setup", false, true, true),
			ginkgotable.Entry("interconnect enabled; pod and egressnode are in local zone", true, true, true),
			ginkgotable.Entry("interconnect enabled; pod is in local zone and egressnode is in remote zone", true, true, false), // snat won't be visible
			ginkgotable.Entry("interconnect enabled; pod is in remote zone and egressnode is in local zone", true, false, true),
		)
		ginkgotable.DescribeTable("egressIP pod retry should remove OVN pod egress setup when EgressIP stops matching pod label",
			func(interconnect bool, podZone string) {
				config.OVNKubernetesFeature.EnableInterconnect = interconnect
				app.Action = func(ctx *cli.Context) error {

					egressIP := net.ParseIP("0:0:0:0:0:feff:c0a8:8e0d")

					egressPod := *newPodWithLabels(namespace, podName, node1Name, podV6IP, egressPodLabel)
					egressNamespace := newNamespace(namespace)

					fakeOvn.startWithDBSetup(
						libovsdbtest.TestSetup{
							NBData: []libovsdbtest.TestData{
								&nbdb.LogicalRouterPort{
									UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2Name + "-UUID",
									Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2Name,
									Networks: []string{nodeLogicalRouterIfAddrV6},
								},
								&nbdb.LogicalRouter{
									Name: ovntypes.OVNClusterRouter,
									UUID: ovntypes.OVNClusterRouter + "-UUID",
								},
								&nbdb.LogicalRouter{
									Name: ovntypes.GWRouterPrefix + node1Name,
									UUID: ovntypes.GWRouterPrefix + node1Name + "-UUID",
								},
								&nbdb.LogicalRouter{
									Name: ovntypes.GWRouterPrefix + node2Name,
									UUID: ovntypes.GWRouterPrefix + node2Name + "-UUID",
									Nat:  nil,
								},
							},
						},
						&v1.NamespaceList{
							Items: []v1.Namespace{*egressNamespace},
						},
						&v1.PodList{
							Items: []v1.Pod{egressPod},
						},
					)
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

					i, n, _ := net.ParseCIDR(podV6IP + "/23")
					n.IP = i
					fakeOvn.controller.logicalPortCache.add(&egressPod, "", types.DefaultNetworkName, "", nil, []*net.IPNet{n})
					// hack pod to be in the provided zone
					fakeOvn.controller.eIPC.nodeZoneState.Store(node1Name, true)
					fakeOvn.controller.eIPC.nodeZoneState.Store(node2Name, true)
					if podZone == "remote" {
						fakeOvn.controller.eIPC.nodeZoneState.Store(node1Name, false)
					}

					err := fakeOvn.controller.WatchEgressIPNamespaces()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchEgressIPPods()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchEgressIP()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					_, err = fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Create(context.TODO(), &eIP, metav1.CreateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					fakeOvn.patchEgressIPObj(node2Name, egressIP.String())

					gomega.Eventually(getEgressIPStatusLen(eIP.Name)).Should(gomega.Equal(1))

					expectedNatLogicalPort := "k8s-node2"
					expectedDatabaseState := []libovsdbtest.TestData{
						getReRoutePolicy(egressPod.Status.PodIP, "6", nodeLogicalRouterIPv6),
						getEIPSNAT(podV6IP, egressIP.String(), expectedNatLogicalPort),
						&nbdb.LogicalRouter{
							Name:     ovntypes.OVNClusterRouter,
							UUID:     ovntypes.OVNClusterRouter + "-UUID",
							Policies: []string{"reroute-UUID"},
						},
						&nbdb.LogicalRouterPort{
							UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2Name + "-UUID",
							Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2Name,
							Networks: []string{nodeLogicalRouterIfAddrV6},
						},
						&nbdb.LogicalRouter{
							Name: ovntypes.GWRouterPrefix + node1Name,
							UUID: ovntypes.GWRouterPrefix + node1Name + "-UUID",
						},
						&nbdb.LogicalRouter{
							Name: ovntypes.GWRouterPrefix + node2Name,
							UUID: ovntypes.GWRouterPrefix + node2Name + "-UUID",
							Nat:  []string{"egressip-nat-UUID"},
						},
					}
					if podZone == "remote" {
						// egressNode is in different zone than pod and egressNode is in local zone, so static reroute will be visible
						expectedDatabaseState = append(expectedDatabaseState, getReRouteStaticRoute(egressPod.Status.PodIP, nodeLogicalRouterIPv6[0]))
						expectedDatabaseState[2].(*nbdb.LogicalRouter).Policies = []string{}
						expectedDatabaseState[2].(*nbdb.LogicalRouter).StaticRoutes = []string{"reroute-static-route-UUID"}
						expectedDatabaseState = expectedDatabaseState[1:]
					}

					gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

					egressIPs, nodes := getEgressIPStatus(eIP.Name)
					gomega.Expect(nodes[0]).To(gomega.Equal(node2Name))
					gomega.Expect(egressIPs[0]).To(gomega.Equal(egressIP.String()))

					podUpdate := newPod(namespace, podName, node1Name, podV6IP)
					ginkgo.By("Bringing down NBDB")
					// inject transient problem, nbdb is down
					fakeOvn.controller.nbClient.Close()
					gomega.Eventually(func() bool {
						return fakeOvn.controller.nbClient.Connected()
					}).Should(gomega.BeFalse())
					_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(egressPod.Namespace).Update(context.TODO(), podUpdate, metav1.UpdateOptions{})
					gomega.Expect(err).ToNot(gomega.HaveOccurred())
					time.Sleep(types.OVSDBTimeout + time.Second)
					// check to see if the retry cache has an entry
					var key string
					key, err = retry.GetResourceKey(podUpdate)
					gomega.Expect(err).ToNot(gomega.HaveOccurred())

					ginkgo.By("retry entry: new obj should not be nil, config should not be nil")
					retry.CheckRetryObjectMultipleFieldsEventually(
						key,
						fakeOvn.controller.retryEgressIPPods,
						gomega.BeNil(),             // oldObj should be nil
						gomega.Not(gomega.BeNil()), // newObj should not be nil
						gomega.Not(gomega.BeNil()), // config should not be nil
					)

					connCtx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
					defer cancel()
					resetNBClient(connCtx, fakeOvn.controller.nbClient)

					retry.SetRetryObjWithNoBackoff(key, fakeOvn.controller.retryEgressIPPods)
					fakeOvn.controller.retryEgressIPPods.RequestRetryObjs()
					// check the cache no longer has the entry
					retry.CheckRetryObjectEventually(key, false, fakeOvn.controller.retryEgressIPPods)

					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			},
			ginkgotable.Entry("interconnect disabled; non-ic - single zone setup", false, "global"),
			ginkgotable.Entry("interconnect enabled; pod is in global zone", true, "global"),
			ginkgotable.Entry("interconnect enabled; pod is in remote zone", true, "remote"), // static re-route is visible but reroute policy won't be
		)

		ginkgo.It("should not treat pod update if pod already had assigned IP when it got the ADD", func() {
			app.Action = func(ctx *cli.Context) error {

				egressIP := net.ParseIP("0:0:0:0:0:feff:c0a8:8e0d")

				egressPod := *newPodWithLabels(namespace, podName, node1Name, podV6IP, egressPodLabel)
				egressNamespace := newNamespace(namespace)
				fakeOvn.startWithDBSetup(
					libovsdbtest.TestSetup{
						NBData: []libovsdbtest.TestData{
							&nbdb.LogicalRouterPort{
								UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2Name + "-UUID",
								Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2Name,
								Networks: []string{nodeLogicalRouterIfAddrV6},
							},
							&nbdb.LogicalRouter{
								Name: ovntypes.OVNClusterRouter,
								UUID: ovntypes.OVNClusterRouter + "-UUID",
							},
							&nbdb.LogicalRouter{
								Name: ovntypes.GWRouterPrefix + node1Name,
								UUID: ovntypes.GWRouterPrefix + node1Name + "-UUID",
							},
							&nbdb.LogicalRouter{
								Name: ovntypes.GWRouterPrefix + node2Name,
								UUID: ovntypes.GWRouterPrefix + node2Name + "-UUID",
							},
						},
					},
					&v1.NamespaceList{
						Items: []v1.Namespace{*egressNamespace},
					},
					&v1.PodList{
						Items: []v1.Pod{egressPod},
					},
				)

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

				i, n, _ := net.ParseCIDR(podV6IP + "/23")
				n.IP = i
				fakeOvn.controller.logicalPortCache.add(&egressPod, "", types.DefaultNetworkName, "", nil, []*net.IPNet{n})

				err := fakeOvn.controller.WatchEgressIPNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIPPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.controller.eIPC.nodeZoneState.Store(node1Name, true)
				fakeOvn.controller.eIPC.nodeZoneState.Store(node2Name, true)

				_, err = fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Create(context.TODO(), &eIP, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				fakeOvn.patchEgressIPObj(node2Name, egressIP.String())

				gomega.Eventually(getEgressIPStatusLen(eIP.Name)).Should(gomega.Equal(1))

				expectedNatLogicalPort := "k8s-node2"
				expectedDatabaseState := []libovsdbtest.TestData{
					&nbdb.LogicalRouterPolicy{
						Priority: types.EgressIPReroutePriority,
						Match:    fmt.Sprintf("ip6.src == %s", egressPod.Status.PodIP),
						Action:   nbdb.LogicalRouterPolicyActionReroute,
						Nexthops: nodeLogicalRouterIPv6,
						ExternalIDs: map[string]string{
							"name": eIP.Name,
						},
						UUID: "reroute-UUID",
					},
					&nbdb.LogicalRouter{
						Name:     ovntypes.OVNClusterRouter,
						UUID:     ovntypes.OVNClusterRouter + "-UUID",
						Policies: []string{"reroute-UUID"},
					},
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2Name + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2Name,
						Networks: []string{nodeLogicalRouterIfAddrV6},
					},
					&nbdb.NAT{
						UUID:       "egressip-nat-UUID",
						LogicalIP:  podV6IP,
						ExternalIP: egressIP.String(),
						ExternalIDs: map[string]string{
							"name": egressIPName,
						},
						Type:        nbdb.NATTypeSNAT,
						LogicalPort: &expectedNatLogicalPort,
						Options: map[string]string{
							"stateless": "false",
						},
					},
					&nbdb.LogicalRouter{
						Name: ovntypes.GWRouterPrefix + node1Name,
						UUID: ovntypes.GWRouterPrefix + node1Name + "-UUID",
					},
					&nbdb.LogicalRouter{
						Name: ovntypes.GWRouterPrefix + node2Name,
						UUID: ovntypes.GWRouterPrefix + node2Name + "-UUID",
						Nat:  []string{"egressip-nat-UUID"},
					},
				}
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				egressIPs, nodes := getEgressIPStatus(eIP.Name)
				gomega.Expect(nodes[0]).To(gomega.Equal(node2Name))
				gomega.Expect(egressIPs[0]).To(gomega.Equal(egressIP.String()))

				podUpdate := newPodWithLabels(namespace, podName, node1Name, podV6IP, map[string]string{
					"egress": "needed",
					"some":   "update",
				})

				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(egressPod.Namespace).Update(context.TODO(), podUpdate, metav1.UpdateOptions{})
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				gomega.Eventually(getEgressIPStatusLen(eIP.Name)).Should(gomega.Equal(1))

				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should update node no-reroute policy address set", func() {
			app.Action = func(ctx *cli.Context) error {

				config.IPv6Mode = true
				node1IPv4 := "192.168.126.202"
				node1IPv6 := "fc00:f853:ccd:e793::1"
				node2IPv4 := "192.168.126.51"
				node2IPv6 := "fc00:f853:ccd:e793::2"
				vipIPv4 := "192.168.126.10"
				vipIPv6 := "fc00:f853:ccd:e793::10"

				node1 := v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: node1Name,
						Annotations: map[string]string{
							"k8s.ovn.org/host-addresses": fmt.Sprintf("[\"%s\", \"%s\", \"%s\", \"%s\"]", node1IPv4, node1IPv6, vipIPv4, vipIPv6),
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
							"k8s.ovn.org/host-addresses": fmt.Sprintf("[\"%s\", \"%s\"]", node2IPv4, node2IPv6),
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

				fakeOvn.startWithDBSetup(
					libovsdbtest.TestSetup{
						NBData: []libovsdbtest.TestData{
							&nbdb.LogicalRouter{
								Name: ovntypes.OVNClusterRouter,
								UUID: ovntypes.OVNClusterRouter + "-UUID",
							},
						},
					},
					&v1.NodeList{
						Items: []v1.Node{node1, node2},
					},
				)

				err := fakeOvn.controller.WatchEgressNodes()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				nodeIPsASdbIDs := getEgressIPAddrSetDbIDs(NodeIPAddrSetName, DefaultNetworkControllerName)
				fakeOvn.asf.EventuallyExpectAddressSetWithIPs(nodeIPsASdbIDs, []string{node1IPv4, node2IPv4, node1IPv6, node2IPv6, vipIPv4, vipIPv6})

				egressSvcPodsV4, egressSvcPodsV6 := addressset.GetHashNamesForAS(egresssvc.GetEgressServiceAddrSetDbIDs(DefaultNetworkControllerName))
				egressipPodsV4, egressipPodsV6 := addressset.GetHashNamesForAS(getEgressIPAddrSetDbIDs(EgressIPServedPodsAddrSetName, DefaultNetworkControllerName))
				nodeIPsV4, nodeIPsV6 := addressset.GetHashNamesForAS(nodeIPsASdbIDs)
				expectedDatabaseState := []libovsdbtest.TestData{
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match:    "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
						Action:   nbdb.LogicalRouterPolicyActionAllow,
						UUID:     "default-no-reroute-UUID",
					},
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match:    fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
						Action:   nbdb.LogicalRouterPolicyActionAllow,
						UUID:     "no-reroute-service-UUID",
					},
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
							egressipPodsV4, egressSvcPodsV4, nodeIPsV4),
						Action:  nbdb.LogicalRouterPolicyActionAllow,
						UUID:    "default-no-reroute-node-UUID",
						Options: map[string]string{"pkt_mark": "1008"},
					},
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip6.src == $%s || ip6.src == $%s) && ip6.dst == $%s",
							egressipPodsV6, egressSvcPodsV6, nodeIPsV6),
						Action:  nbdb.LogicalRouterPolicyActionAllow,
						UUID:    "default-v6-no-reroute-node-UUID",
						Options: map[string]string{"pkt_mark": "1008"},
					},
					&nbdb.LogicalRouter{
						Name: ovntypes.OVNClusterRouter,
						UUID: ovntypes.OVNClusterRouter + "-UUID",
						Policies: []string{
							"default-no-reroute-UUID",
							"no-reroute-service-UUID",
							"default-no-reroute-node-UUID",
							"default-v6-no-reroute-node-UUID",
						},
					},
				}

				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				node1.ObjectMeta.Annotations["k8s.ovn.org/host-addresses"] = fmt.Sprintf("[\"%s\", \"%s\"]", node1IPv4, node1IPv6)
				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), &node1, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				fakeOvn.asf.EventuallyExpectAddressSetWithIPs(nodeIPsASdbIDs, []string{node1IPv4, node2IPv4, node1IPv6, node2IPv6})
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				node2.ObjectMeta.Annotations["k8s.ovn.org/host-addresses"] = fmt.Sprintf("[\"%s\", \"%s\", \"%s\", \"%s\"]", node2IPv4, node2IPv6, vipIPv4, vipIPv6)
				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), &node2, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				fakeOvn.asf.EventuallyExpectAddressSetWithIPs(nodeIPsASdbIDs, []string{node1IPv4, node2IPv4, node1IPv6, node2IPv6, vipIPv4, vipIPv6})
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				err = fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Delete(context.TODO(), node1.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				fakeOvn.asf.EventuallyExpectAddressSetWithIPs(nodeIPsASdbIDs, []string{node2IPv4, node2IPv6, vipIPv4, vipIPv6})
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})

	ginkgo.Context("On node DELETE", func() {

		ginkgotable.DescribeTable("should treat pod update if pod did not have an assigned IP when it got the ADD",
			func(interconnect bool, podZone string) {
				config.OVNKubernetesFeature.EnableInterconnect = interconnect
				app.Action = func(ctx *cli.Context) error {

					egressIP := net.ParseIP("0:0:0:0:0:feff:c0a8:8e0d")

					egressPod := *newPodWithLabels(namespace, podName, node1Name, "", egressPodLabel)
					egressNamespace := newNamespace(namespace)

					fakeOvn.startWithDBSetup(
						libovsdbtest.TestSetup{
							NBData: []libovsdbtest.TestData{
								&nbdb.LogicalRouterPort{
									UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2Name + "-UUID",
									Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2Name,
									Networks: []string{nodeLogicalRouterIfAddrV6},
								},
								&nbdb.LogicalRouter{
									Name: ovntypes.OVNClusterRouter,
									UUID: ovntypes.OVNClusterRouter + "-UUID",
								},
								&nbdb.LogicalRouter{
									Name: ovntypes.GWRouterPrefix + node1Name,
									UUID: ovntypes.GWRouterPrefix + node1Name + "-UUID",
								},
								&nbdb.LogicalRouter{
									Name: ovntypes.GWRouterPrefix + node2Name,
									UUID: ovntypes.GWRouterPrefix + node2Name + "-UUID",
									Nat:  nil,
								},
							},
						},
						&v1.NamespaceList{
							Items: []v1.Namespace{*egressNamespace},
						},
						&v1.PodList{
							Items: []v1.Pod{egressPod},
						},
					)

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

					err := fakeOvn.controller.WatchEgressIPNamespaces()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchEgressIPPods()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchEgressIP()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					_, err = fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Create(context.TODO(), &eIP, metav1.CreateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					fakeOvn.patchEgressIPObj(node2Name, egressIP.String())

					gomega.Eventually(getEgressIPStatusLen(eIP.Name)).Should(gomega.Equal(1))

					egressIPs, nodes := getEgressIPStatus(eIP.Name)
					gomega.Expect(nodes[0]).To(gomega.Equal(node2Name))
					gomega.Expect(egressIPs[0]).To(gomega.Equal(egressIP.String()))

					podUpdate := newPodWithLabels(namespace, podName, node1Name, podV6IP, egressPodLabel)
					podUpdate.Annotations = map[string]string{
						"k8s.ovn.org/pod-networks": fmt.Sprintf("{\"default\":{\"ip_addresses\":[\"%s/23\"],\"mac_address\":\"0a:58:0a:83:00:0f\",\"gateway_ips\":[\"%s\"],\"ip_address\":\"%s/23\",\"gateway_ip\":\"%s\"}}", podV6IP, v6GatewayIP, podV6IP, v6GatewayIP),
					}
					i, n, _ := net.ParseCIDR(podV6IP + "/23")
					n.IP = i
					fakeOvn.controller.logicalPortCache.add(&egressPod, "", types.DefaultNetworkName, "", nil, []*net.IPNet{n})
					// hack pod to be in the provided zone
					fakeOvn.controller.eIPC.nodeZoneState.Store(node1Name, true)
					fakeOvn.controller.eIPC.nodeZoneState.Store(node2Name, true)
					if podZone == "remote" {
						fakeOvn.controller.eIPC.nodeZoneState.Store(node1Name, false)
					}

					_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(egressPod.Namespace).Update(context.TODO(), podUpdate, metav1.UpdateOptions{})
					gomega.Expect(err).ToNot(gomega.HaveOccurred())
					gomega.Eventually(getEgressIPStatusLen(eIP.Name)).Should(gomega.Equal(1))

					expectedNatLogicalPort := "k8s-node2"
					expectedDatabaseState := []libovsdbtest.TestData{
						getReRoutePolicy(podV6IP, "6", nodeLogicalRouterIPv6),
						getEIPSNAT(podV6IP, egressIP.String(), expectedNatLogicalPort),
						&nbdb.LogicalRouter{
							Name:     ovntypes.OVNClusterRouter,
							UUID:     ovntypes.OVNClusterRouter + "-UUID",
							Policies: []string{"reroute-UUID"},
						},
						&nbdb.LogicalRouterPort{
							UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2Name + "-UUID",
							Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2Name,
							Networks: []string{nodeLogicalRouterIfAddrV6},
						},
						&nbdb.LogicalRouter{
							Name: ovntypes.GWRouterPrefix + node1Name,
							UUID: ovntypes.GWRouterPrefix + node1Name + "-UUID",
						},
						&nbdb.LogicalRouter{
							Name: ovntypes.GWRouterPrefix + node2Name,
							UUID: ovntypes.GWRouterPrefix + node2Name + "-UUID",
							Nat:  []string{"egressip-nat-UUID"},
						},
					}
					if podZone == "remote" {
						// egressNode is in different zone than pod and egressNode is in local zone, so static reroute will be visible
						expectedDatabaseState = append(expectedDatabaseState, getReRouteStaticRoute(podV6IP, nodeLogicalRouterIPv6[0]))
						expectedDatabaseState[2].(*nbdb.LogicalRouter).Policies = []string{}
						expectedDatabaseState[2].(*nbdb.LogicalRouter).StaticRoutes = []string{"reroute-static-route-UUID"}
						expectedDatabaseState = expectedDatabaseState[1:]
					}
					gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			},
			ginkgotable.Entry("interconnect disabled; non-ic - single zone setup", false, "global"),
			ginkgotable.Entry("interconnect enabled; pod is in global zone", true, "global"),
			ginkgotable.Entry("interconnect enabled; pod is in remote zone", true, "remote"), // static re-route is visible but reroute policy won't be
		)

		ginkgo.It("should not treat pod DELETE if pod did not have an assigned IP when it got the ADD and we receive a DELETE before the IP UPDATE", func() {
			app.Action = func(ctx *cli.Context) error {

				egressIP := net.ParseIP("0:0:0:0:0:feff:c0a8:8e0d")

				egressPod := *newPodWithLabels(namespace, podName, node1Name, "", egressPodLabel)
				egressNamespace := newNamespace(namespace)
				fakeOvn.startWithDBSetup(clusterRouterDbSetup,
					&v1.NamespaceList{
						Items: []v1.Namespace{*egressNamespace},
					},
					&v1.PodList{
						Items: []v1.Pod{egressPod},
					},
				)
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

				err := fakeOvn.controller.WatchEgressIPNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIPPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				_, err = fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Create(context.TODO(), &eIP, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				fakeOvn.patchEgressIPObj(node2Name, egressIP.String())

				gomega.Eventually(getEgressIPStatusLen(eIP.Name)).Should(gomega.Equal(1))

				egressIPs, nodes := getEgressIPStatus(eIP.Name)
				gomega.Expect(nodes[0]).To(gomega.Equal(node2Name))
				gomega.Expect(egressIPs[0]).To(gomega.Equal(egressIP.String()))

				err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(egressPod.Namespace).Delete(context.TODO(), egressPod.Name, *metav1.NewDeleteOptions(0))
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				gomega.Eventually(getEgressIPStatusLen(eIP.Name)).Should(gomega.Equal(1))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})

	ginkgo.Context("IPv6 on namespace UPDATE", func() {

		ginkgotable.DescribeTable("should remove OVN pod egress setup when EgressIP is deleted",
			func(interconnect bool, podZone string) {
				config.OVNKubernetesFeature.EnableInterconnect = interconnect
				app.Action = func(ctx *cli.Context) error {

					egressIP := net.ParseIP("0:0:0:0:0:feff:c0a8:8e0d")

					egressPod := *newPodWithLabels(namespace, podName, node1Name, podV6IP, egressPodLabel)
					egressNamespace := newNamespaceWithLabels(namespace, egressPodLabel)

					fakeOvn.startWithDBSetup(
						libovsdbtest.TestSetup{
							NBData: []libovsdbtest.TestData{
								&nbdb.LogicalRouterPort{
									UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2Name + "-UUID",
									Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2Name,
									Networks: []string{nodeLogicalRouterIfAddrV6},
								},
								&nbdb.LogicalRouter{
									Name: ovntypes.OVNClusterRouter,
									UUID: ovntypes.OVNClusterRouter + "-UUID",
								},
								&nbdb.LogicalRouter{
									Name: ovntypes.GWRouterPrefix + node1Name,
									UUID: ovntypes.GWRouterPrefix + node1Name + "-UUID",
								},
								&nbdb.LogicalRouter{
									Name: ovntypes.GWRouterPrefix + node2Name,
									UUID: ovntypes.GWRouterPrefix + node2Name + "-UUID",
									Nat:  nil,
								},
							},
						},
						&v1.NamespaceList{
							Items: []v1.Namespace{*egressNamespace},
						},
						&v1.PodList{
							Items: []v1.Pod{egressPod},
						},
					)

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

					i, n, _ := net.ParseCIDR(podV6IP + "/23")
					n.IP = i
					fakeOvn.controller.logicalPortCache.add(&egressPod, "", types.DefaultNetworkName, "", nil, []*net.IPNet{n})
					// hack pod to be in the provided zone
					fakeOvn.controller.eIPC.nodeZoneState.Store(node1Name, true)
					fakeOvn.controller.eIPC.nodeZoneState.Store(node2Name, true)
					if podZone == "remote" {
						fakeOvn.controller.eIPC.nodeZoneState.Store(node1Name, false)
					}

					err := fakeOvn.controller.WatchEgressIPNamespaces()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchEgressIPPods()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchEgressIP()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					_, err = fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Create(context.TODO(), &eIP, metav1.CreateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					fakeOvn.patchEgressIPObj(node2Name, egressIP.String())

					gomega.Eventually(getEgressIPStatusLen(eIP.Name)).Should(gomega.Equal(1))

					expectedNatLogicalPort := "k8s-node2"
					expectedDatabaseState := []libovsdbtest.TestData{
						getReRoutePolicy(egressPod.Status.PodIP, "6", nodeLogicalRouterIPv6),
						getEIPSNAT(podV6IP, egressIP.String(), expectedNatLogicalPort),
						&nbdb.LogicalRouter{
							Name:     ovntypes.OVNClusterRouter,
							UUID:     ovntypes.OVNClusterRouter + "-UUID",
							Policies: []string{"reroute-UUID"},
						},
						&nbdb.LogicalRouterPort{
							UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2Name + "-UUID",
							Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2Name,
							Networks: []string{nodeLogicalRouterIfAddrV6},
						},
						&nbdb.LogicalRouter{
							Name: ovntypes.GWRouterPrefix + node1Name,
							UUID: ovntypes.GWRouterPrefix + node1Name + "-UUID",
						},
						&nbdb.LogicalRouter{
							Name: ovntypes.GWRouterPrefix + node2Name,
							UUID: ovntypes.GWRouterPrefix + node2Name + "-UUID",
							Nat:  []string{"egressip-nat-UUID"},
						},
					}
					if podZone == "remote" {
						// egressNode is in different zone than pod and egressNode is in local zone, so static reroute will be visible
						expectedDatabaseState = append(expectedDatabaseState, getReRouteStaticRoute(egressPod.Status.PodIP, nodeLogicalRouterIPv6[0]))
						expectedDatabaseState[2].(*nbdb.LogicalRouter).Policies = []string{}
						expectedDatabaseState[2].(*nbdb.LogicalRouter).StaticRoutes = []string{"reroute-static-route-UUID"}
						expectedDatabaseState = expectedDatabaseState[1:]
					}

					gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

					egressIPs, nodes := getEgressIPStatus(eIP.Name)
					gomega.Expect(nodes[0]).To(gomega.Equal(node2Name))
					gomega.Expect(egressIPs[0]).To(gomega.Equal(egressIP.String()))

					err = fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Delete(context.TODO(), eIP.Name, metav1.DeleteOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					expectedDatabaseState = []libovsdbtest.TestData{
						&nbdb.LogicalRouter{
							Name: ovntypes.OVNClusterRouter,
							UUID: ovntypes.OVNClusterRouter + "-UUID",
						},
						&nbdb.LogicalRouterPort{
							UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2Name + "-UUID",
							Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2Name,
							Networks: []string{nodeLogicalRouterIfAddrV6},
						},
						&nbdb.LogicalRouter{
							Name: ovntypes.GWRouterPrefix + node1Name,
							UUID: ovntypes.GWRouterPrefix + node1Name + "-UUID",
						},
						&nbdb.LogicalRouter{
							Name: ovntypes.GWRouterPrefix + node2Name,
							UUID: ovntypes.GWRouterPrefix + node2Name + "-UUID",
							Nat:  nil,
						},
					}
					gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			},
			ginkgotable.Entry("interconnect disabled; non-ic - single zone setup", false, "global"),
			ginkgotable.Entry("interconnect enabled; pod is in global zone", true, "global"),
			ginkgotable.Entry("interconnect enabled; pod is in remote zone", true, "remote"),
		)

		ginkgotable.DescribeTable("egressIP retry should remove OVN pod egress setup when EgressIP is deleted",
			func(interconnect bool, podZone string) {
				config.OVNKubernetesFeature.EnableInterconnect = interconnect
				app.Action = func(ctx *cli.Context) error {

					egressIP := net.ParseIP("0:0:0:0:0:feff:c0a8:8e0d")

					egressPod := *newPodWithLabels(namespace, podName, node1Name, podV6IP, egressPodLabel)
					egressNamespace := newNamespaceWithLabels(namespace, egressPodLabel)

					fakeOvn.startWithDBSetup(
						libovsdbtest.TestSetup{
							NBData: []libovsdbtest.TestData{
								&nbdb.LogicalRouterPort{
									UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2Name + "-UUID",
									Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2Name,
									Networks: []string{nodeLogicalRouterIfAddrV6},
								},
								&nbdb.LogicalRouter{
									Name: ovntypes.OVNClusterRouter,
									UUID: ovntypes.OVNClusterRouter + "-UUID",
								},
								&nbdb.LogicalRouter{
									Name: ovntypes.GWRouterPrefix + node1Name,
									UUID: ovntypes.GWRouterPrefix + node1Name + "-UUID",
								},
								&nbdb.LogicalRouter{
									Name: ovntypes.GWRouterPrefix + node2Name,
									UUID: ovntypes.GWRouterPrefix + node2Name + "-UUID",
									Nat:  nil,
								},
							},
						},
						&v1.NamespaceList{
							Items: []v1.Namespace{*egressNamespace},
						},
						&v1.PodList{
							Items: []v1.Pod{egressPod},
						},
					)
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

					i, n, _ := net.ParseCIDR(podV6IP + "/23")
					n.IP = i
					fakeOvn.controller.logicalPortCache.add(&egressPod, "", types.DefaultNetworkName, "", nil, []*net.IPNet{n})

					// hack pod to be in the provided zone
					fakeOvn.controller.eIPC.nodeZoneState.Store(node1Name, true)
					fakeOvn.controller.eIPC.nodeZoneState.Store(node2Name, true)
					if podZone == "remote" {
						fakeOvn.controller.eIPC.nodeZoneState.Store(node1Name, false)
					}

					err := fakeOvn.controller.WatchEgressIPNamespaces()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchEgressIPPods()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchEgressIP()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					ginkgo.By("Bringing down NBDB")
					// inject transient problem, nbdb is down
					fakeOvn.controller.nbClient.Close()
					gomega.Eventually(func() bool {
						return fakeOvn.controller.nbClient.Connected()
					}).Should(gomega.BeFalse())

					_, err = fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Create(context.TODO(), &eIP, metav1.CreateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					fakeOvn.patchEgressIPObj(node2Name, egressIP.String())

					// sleep long enough for TransactWithRetry to fail, causing egressnode operations to fail
					time.Sleep(types.OVSDBTimeout + time.Second)
					// check to see if the retry cache has an entry
					key, err := retry.GetResourceKey(&eIP)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					retry.CheckRetryObjectEventually(key, true, fakeOvn.controller.retryEgressIPs)

					connCtx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
					defer cancel()
					resetNBClient(connCtx, fakeOvn.controller.nbClient)
					retry.SetRetryObjWithNoBackoff(key, fakeOvn.controller.retryEgressIPs)
					fakeOvn.controller.retryEgressIPs.RequestRetryObjs()
					// check the cache no longer has the entry
					retry.CheckRetryObjectEventually(key, false, fakeOvn.controller.retryEgressIPs)

					gomega.Eventually(getEgressIPStatusLen(eIP.Name)).Should(gomega.Equal(1))

					expectedNatLogicalPort := "k8s-node2"
					expectedDatabaseState := []libovsdbtest.TestData{
						getReRoutePolicy(egressPod.Status.PodIP, "6", nodeLogicalRouterIPv6),
						getEIPSNAT(podV6IP, egressIP.String(), expectedNatLogicalPort),
						&nbdb.LogicalRouter{
							Name:     ovntypes.OVNClusterRouter,
							UUID:     ovntypes.OVNClusterRouter + "-UUID",
							Policies: []string{"reroute-UUID"},
						},
						&nbdb.LogicalRouterPort{
							UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2Name + "-UUID",
							Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2Name,
							Networks: []string{nodeLogicalRouterIfAddrV6},
						},
						&nbdb.LogicalRouter{
							Name: ovntypes.GWRouterPrefix + node1Name,
							UUID: ovntypes.GWRouterPrefix + node1Name + "-UUID",
						},
						&nbdb.LogicalRouter{
							Name: ovntypes.GWRouterPrefix + node2Name,
							UUID: ovntypes.GWRouterPrefix + node2Name + "-UUID",
							Nat:  []string{"egressip-nat-UUID"},
						},
					}
					if podZone == "remote" {
						// egressNode is in different zone than pod and egressNode is in local zone, so static reroute will be visible
						expectedDatabaseState = append(expectedDatabaseState, getReRouteStaticRoute(egressPod.Status.PodIP, nodeLogicalRouterIPv6[0]))
						expectedDatabaseState[2].(*nbdb.LogicalRouter).Policies = []string{}
						expectedDatabaseState[2].(*nbdb.LogicalRouter).StaticRoutes = []string{"reroute-static-route-UUID"}
						expectedDatabaseState = expectedDatabaseState[1:]
					}
					gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

					egressIPs, nodes := getEgressIPStatus(eIP.Name)
					gomega.Expect(nodes[0]).To(gomega.Equal(node2Name))
					gomega.Expect(egressIPs[0]).To(gomega.Equal(egressIP.String()))

					err = fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Delete(context.TODO(), eIP.Name, metav1.DeleteOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					expectedDatabaseState = []libovsdbtest.TestData{
						&nbdb.LogicalRouter{
							Name: ovntypes.OVNClusterRouter,
							UUID: ovntypes.OVNClusterRouter + "-UUID",
						},
						&nbdb.LogicalRouterPort{
							UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2Name + "-UUID",
							Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2Name,
							Networks: []string{nodeLogicalRouterIfAddrV6},
						},
						&nbdb.LogicalRouter{
							Name: ovntypes.GWRouterPrefix + node1Name,
							UUID: ovntypes.GWRouterPrefix + node1Name + "-UUID",
						},
						&nbdb.LogicalRouter{
							Name: ovntypes.GWRouterPrefix + node2Name,
							UUID: ovntypes.GWRouterPrefix + node2Name + "-UUID",
							Nat:  nil,
						},
					}
					gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			},
			ginkgotable.Entry("interconnect disabled; non-ic - single zone setup", false, "global"),
			ginkgotable.Entry("interconnect enabled; pod is in global zone", true, "global"),
			ginkgotable.Entry("interconnect enabled; pod is in remote zone", true, "remote"),
		)

		ginkgotable.DescribeTable("should remove OVN pod egress setup when EgressIP stops matching",
			func(interconnect bool, podZone string) {
				config.OVNKubernetesFeature.EnableInterconnect = interconnect
				app.Action = func(ctx *cli.Context) error {

					egressIP := net.ParseIP("0:0:0:0:0:feff:c0a8:8e0d")

					egressPod := *newPodWithLabels(namespace, podName, node1Name, podV6IP, egressPodLabel)
					egressNamespace := newNamespaceWithLabels(namespace, egressPodLabel)

					fakeOvn.startWithDBSetup(
						libovsdbtest.TestSetup{
							NBData: []libovsdbtest.TestData{
								&nbdb.LogicalRouterPort{
									UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2Name + "-UUID",
									Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2Name,
									Networks: []string{nodeLogicalRouterIfAddrV6},
								},
								&nbdb.LogicalRouter{
									Name: ovntypes.OVNClusterRouter,
									UUID: ovntypes.OVNClusterRouter + "-UUID",
								},
								&nbdb.LogicalRouter{
									Name: ovntypes.GWRouterPrefix + node1Name,
									UUID: ovntypes.GWRouterPrefix + node1Name + "-UUID",
								},
								&nbdb.LogicalRouter{
									Name: ovntypes.GWRouterPrefix + node2Name,
									UUID: ovntypes.GWRouterPrefix + node2Name + "-UUID",
									Nat:  nil,
								},
							},
						},
						&v1.NamespaceList{
							Items: []v1.Namespace{*egressNamespace},
						},
						&v1.PodList{
							Items: []v1.Pod{egressPod},
						},
					)

					i, n, _ := net.ParseCIDR(podV6IP + "/23")
					n.IP = i
					fakeOvn.controller.logicalPortCache.add(&egressPod, "", types.DefaultNetworkName, "", nil, []*net.IPNet{n})
					// hack pod to be in the provided zone
					fakeOvn.controller.eIPC.nodeZoneState.Store(node1Name, true)
					fakeOvn.controller.eIPC.nodeZoneState.Store(node2Name, true)
					if podZone == "remote" {
						fakeOvn.controller.eIPC.nodeZoneState.Store(node1Name, false)
					}

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

					err := fakeOvn.controller.WatchEgressIPNamespaces()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchEgressIPPods()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchEgressIP()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					_, err = fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Create(context.TODO(), &eIP, metav1.CreateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					fakeOvn.patchEgressIPObj(node2Name, egressIP.String())

					gomega.Eventually(getEgressIPStatusLen(eIP.Name)).Should(gomega.Equal(1))

					expectedNatLogicalPort := "k8s-node2"
					expectedDatabaseState := []libovsdbtest.TestData{
						getReRoutePolicy(egressPod.Status.PodIP, "6", nodeLogicalRouterIPv6),
						&nbdb.LogicalRouter{
							Name:     ovntypes.OVNClusterRouter,
							UUID:     ovntypes.OVNClusterRouter + "-UUID",
							Policies: []string{"reroute-UUID"},
						},
						&nbdb.LogicalRouterPort{
							UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2Name + "-UUID",
							Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2Name,
							Networks: []string{nodeLogicalRouterIfAddrV6},
						},
						&nbdb.NAT{
							UUID:       "egressip-nat-UUID",
							LogicalIP:  podV6IP,
							ExternalIP: egressIP.String(),
							ExternalIDs: map[string]string{
								"name": egressIPName,
							},
							Type:        nbdb.NATTypeSNAT,
							LogicalPort: &expectedNatLogicalPort,
							Options: map[string]string{
								"stateless": "false",
							},
						},
						&nbdb.LogicalRouter{
							Name: ovntypes.GWRouterPrefix + node1Name,
							UUID: ovntypes.GWRouterPrefix + node1Name + "-UUID",
						},
						&nbdb.LogicalRouter{
							Name: ovntypes.GWRouterPrefix + node2Name,
							UUID: ovntypes.GWRouterPrefix + node2Name + "-UUID",
							Nat:  []string{"egressip-nat-UUID"},
						},
					}
					if podZone == "remote" {
						// pod is in remote zone, its LRP won't be visible
						expectedDatabaseState = append(expectedDatabaseState, getReRouteStaticRoute(egressPod.Status.PodIP, nodeLogicalRouterIPv6[0]))
						expectedDatabaseState[1].(*nbdb.LogicalRouter).StaticRoutes = []string{"reroute-static-route-UUID"}
						expectedDatabaseState[1].(*nbdb.LogicalRouter).Policies = []string{}
						expectedDatabaseState = expectedDatabaseState[1:]
					}
					gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

					egressIPs, nodes := getEgressIPStatus(eIP.Name)
					gomega.Expect(nodes[0]).To(gomega.Equal(node2Name))
					gomega.Expect(egressIPs[0]).To(gomega.Equal(egressIP.String()))

					namespaceUpdate := newNamespace(namespace)

					_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Update(context.TODO(), namespaceUpdate, metav1.UpdateOptions{})
					gomega.Expect(err).ToNot(gomega.HaveOccurred())
					gomega.Eventually(getEgressIPStatusLen(eIP.Name)).Should(gomega.Equal(1))

					expectedDatabaseState = []libovsdbtest.TestData{
						&nbdb.LogicalRouter{
							Name: ovntypes.OVNClusterRouter,
							UUID: ovntypes.OVNClusterRouter + "-UUID",
						},
						&nbdb.LogicalRouterPort{
							UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2Name + "-UUID",
							Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2Name,
							Networks: []string{nodeLogicalRouterIfAddrV6},
						},
						&nbdb.LogicalRouter{
							Name: ovntypes.GWRouterPrefix + node1Name,
							UUID: ovntypes.GWRouterPrefix + node1Name + "-UUID",
						},
						&nbdb.LogicalRouter{
							Name: ovntypes.GWRouterPrefix + node2Name,
							UUID: ovntypes.GWRouterPrefix + node2Name + "-UUID",
							Nat:  nil,
						},
					}
					gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			},
			ginkgotable.Entry("interconnect disabled; non-ic - single zone setup", false, "global"),
			ginkgotable.Entry("interconnect enabled; pod is in global zone", true, "global"),
			ginkgotable.Entry("interconnect enabled; pod is in remote zone", true, "remote"),
		)

		ginkgo.It("should not remove OVN pod egress setup when EgressIP stops matching, but pod never had any IP to begin with", func() {
			app.Action = func(ctx *cli.Context) error {

				egressIP := net.ParseIP("0:0:0:0:0:feff:c0a8:8e0d")

				egressPod := *newPodWithLabels(namespace, podName, node1Name, "", egressPodLabel)
				egressNamespace := newNamespaceWithLabels(namespace, egressPodLabel)
				fakeOvn.startWithDBSetup(clusterRouterDbSetup,
					&v1.NamespaceList{
						Items: []v1.Namespace{*egressNamespace},
					},
					&v1.PodList{
						Items: []v1.Pod{egressPod},
					},
				)

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

				err := fakeOvn.controller.WatchEgressIPNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIPPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				_, err = fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Create(context.TODO(), &eIP, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				fakeOvn.patchEgressIPObj(node2Name, egressIP.String())

				gomega.Eventually(getEgressIPStatusLen(eIP.Name)).Should(gomega.Equal(1))

				egressIPs, nodes := getEgressIPStatus(eIP.Name)
				gomega.Expect(nodes[0]).To(gomega.Equal(node2Name))
				gomega.Expect(egressIPs[0]).To(gomega.Equal(egressIP.String()))

				namespaceUpdate := newNamespace(namespace)

				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Update(context.TODO(), namespaceUpdate, metav1.UpdateOptions{})
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				gomega.Eventually(getEgressIPStatusLen(eIP.Name)).Should(gomega.Equal(1))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})
	ginkgo.Context("on EgressIP UPDATE", func() {

		ginkgotable.DescribeTable("should update OVN on EgressIP .spec.egressips change",
			func(interconnect bool, node1Zone, node2Zone string) {
				config.OVNKubernetesFeature.EnableInterconnect = interconnect
				app.Action = func(ctx *cli.Context) error {

					egressIP1 := "192.168.126.101"
					egressIP2 := "192.168.126.102"
					egressIP3 := "192.168.126.103"
					node1IPv4 := "192.168.126.202/24"
					node2IPv4 := "192.168.126.51/24"

					egressPod := *newPodWithLabels(namespace, podName, node1Name, podV4IP, egressPodLabel)
					egressNamespace := newNamespace(namespace)
					annotations := map[string]string{
						"k8s.ovn.org/node-primary-ifaddr":             fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4, ""),
						"k8s.ovn.org/node-subnets":                    fmt.Sprintf("{\"default\":\"%s\"}", v4NodeSubnet),
						"k8s.ovn.org/node-transit-switch-port-ifaddr": "{\"ipv4\":\"168.254.0.2/16\"}", // used only for ic=true test
						"k8s.ovn.org/zone-name":                       node1Zone,                       // used only for ic=true test
					}
					labels := map[string]string{
						"k8s.ovn.org/egress-assignable": "",
					}
					node1 := getNodeObj(node1Name, annotations, labels)
					annotations = map[string]string{
						"k8s.ovn.org/node-primary-ifaddr":             fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node2IPv4, ""),
						"k8s.ovn.org/node-subnets":                    fmt.Sprintf("{\"default\":\"%s\"}", v4NodeSubnet),
						"k8s.ovn.org/node-transit-switch-port-ifaddr": "{\"ipv4\":\"168.254.0.3/16\"}", // used only for ic=true test
						"k8s.ovn.org/zone-name":                       node2Zone,                       // used only for ic=true test
					}
					node2 := getNodeObj(node2Name, annotations, labels)

					eIP := egressipv1.EgressIP{
						ObjectMeta: newEgressIPMeta(egressIPName),
						Spec: egressipv1.EgressIPSpec{
							EgressIPs: []string{egressIP1, egressIP2},
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

					fakeOvn.startWithDBSetup(
						libovsdbtest.TestSetup{
							NBData: []libovsdbtest.TestData{
								&nbdb.LogicalRouterPort{
									UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2.Name + "-UUID",
									Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2.Name,
									Networks: []string{"100.64.0.3/29"},
								},
								&nbdb.LogicalRouterPort{
									UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name + "-UUID",
									Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name,
									Networks: []string{"100.64.0.2/29"},
								},
								&nbdb.LogicalRouter{
									Name: ovntypes.OVNClusterRouter,
									UUID: ovntypes.OVNClusterRouter + "-UUID",
								},
								&nbdb.LogicalRouter{
									Name: ovntypes.GWRouterPrefix + node1.Name,
									UUID: ovntypes.GWRouterPrefix + node1.Name + "-UUID",
								},
								&nbdb.LogicalRouter{
									Name: ovntypes.GWRouterPrefix + node2.Name,
									UUID: ovntypes.GWRouterPrefix + node2.Name + "-UUID",
								},
								&nbdb.LogicalSwitchPort{
									UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "UUID",
									Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
									Type: "router",
									Options: map[string]string{
										"router-port": types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
									},
								},
								&nbdb.LogicalSwitchPort{
									UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "UUID",
									Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
									Type: "router",
									Options: map[string]string{
										"router-port": types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
									},
								},
							},
						},
						&v1.NodeList{
							Items: []v1.Node{node1, node2},
						},
						&v1.NamespaceList{
							Items: []v1.Namespace{*egressNamespace},
						},
						&v1.PodList{
							Items: []v1.Pod{egressPod},
						})

					i, n, _ := net.ParseCIDR(podV4IP + "/23")
					n.IP = i
					fakeOvn.controller.logicalPortCache.add(&egressPod, "", types.DefaultNetworkName, "", nil, []*net.IPNet{n})

					err := fakeOvn.controller.WatchEgressIPNamespaces()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchEgressIPPods()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchEgressNodes()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchEgressIP()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					_, err = fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Create(context.TODO(), &eIP, metav1.CreateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					// NOTE: Cluster manager is the one who patches the egressIP object.
					// For the sake of unit testing egressip zone controller we need to patch egressIP object manually
					// There are tests in cluster-manager package covering the patch logic.
					status := []egressipv1.EgressIPStatusItem{
						{
							Node:     node1Name,
							EgressIP: egressIP1,
						},
						{
							Node:     node2Name,
							EgressIP: egressIP2,
						},
					}
					err = fakeOvn.controller.patchReplaceEgressIPStatus(eIP.Name, status)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					gomega.Eventually(getEgressIPStatusLen(eIP.Name)).Should(gomega.Equal(2))
					egressIPs, nodes := getEgressIPStatus(eIP.Name)
					assignmentNode1, assignmentNode2 := nodes[0], nodes[1]
					assignedEgressIP1, assignedEgressIP2 := egressIPs[0], egressIPs[1]

					expectedNatLogicalPort1 := fmt.Sprintf("k8s-%s", assignmentNode1)
					expectedNatLogicalPort2 := fmt.Sprintf("k8s-%s", assignmentNode2)
					natEIP1 := &nbdb.NAT{
						UUID:       "egressip-nat-1-UUID",
						LogicalIP:  podV4IP,
						ExternalIP: assignedEgressIP1,
						ExternalIDs: map[string]string{
							"name": egressIPName,
						},
						Type:        nbdb.NATTypeSNAT,
						LogicalPort: &expectedNatLogicalPort1,
						Options: map[string]string{
							"stateless": "false",
						},
					}
					natEIP2 := &nbdb.NAT{
						UUID:       "egressip-nat-2-UUID",
						LogicalIP:  podV4IP,
						ExternalIP: assignedEgressIP2,
						ExternalIDs: map[string]string{
							"name": egressIPName,
						},
						Type:        nbdb.NATTypeSNAT,
						LogicalPort: &expectedNatLogicalPort2,
						Options: map[string]string{
							"stateless": "false",
						},
					}
					expectedDatabaseState := []libovsdbtest.TestData{
						getReRoutePolicy(egressPod.Status.PodIP, "4", []string{"100.64.0.2", "100.64.0.3"}),
						&nbdb.LogicalRouterPolicy{
							Priority: types.DefaultNoRereoutePriority,
							Match:    "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
							Action:   nbdb.LogicalRouterPolicyActionAllow,
							UUID:     "default-no-reroute-UUID",
						},
						&nbdb.LogicalRouterPolicy{
							Priority: types.DefaultNoRereoutePriority,
							Match:    fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
							Action:   nbdb.LogicalRouterPolicyActionAllow,
							UUID:     "no-reroute-service-UUID",
						},
						&nbdb.LogicalRouter{
							Name: ovntypes.GWRouterPrefix + assignmentNode1,
							UUID: ovntypes.GWRouterPrefix + assignmentNode1 + "-UUID",
						},
						&nbdb.LogicalRouter{
							Name: ovntypes.GWRouterPrefix + assignmentNode2,
							UUID: ovntypes.GWRouterPrefix + assignmentNode2 + "-UUID",
						},
						&nbdb.LogicalRouter{
							Name:     ovntypes.OVNClusterRouter,
							UUID:     ovntypes.OVNClusterRouter + "-UUID",
							Policies: []string{"reroute-UUID", "default-no-reroute-UUID", "no-reroute-service-UUID"},
						},
						&nbdb.LogicalRouterPort{
							UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2.Name + "-UUID",
							Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2.Name,
							Networks: []string{"100.64.0.3/29"},
						},
						&nbdb.LogicalRouterPort{
							UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name + "-UUID",
							Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name,
							Networks: []string{"100.64.0.2/29"},
						},
						&nbdb.LogicalSwitchPort{
							UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "UUID",
							Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
							Type: "router",
							Options: map[string]string{
								"router-port": types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
							},
						},
						&nbdb.LogicalSwitchPort{
							UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "UUID",
							Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
							Type: "router",
							Options: map[string]string{
								"router-port": types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
							},
						},
					}
					if !interconnect || node1Zone == "global" {
						expectedDatabaseState[8].(*nbdb.LogicalSwitchPort).Options["nat-addresses"] = "router"
						expectedDatabaseState[8].(*nbdb.LogicalSwitchPort).Options["exclude-lb-vips-from-garp"] = "true"
						expectedDatabaseState[3].(*nbdb.LogicalRouter).Nat = []string{"egressip-nat-1-UUID"}
						expectedDatabaseState = append(expectedDatabaseState, natEIP1)
					}
					if !interconnect || node2Zone == "global" {
						expectedDatabaseState[9].(*nbdb.LogicalSwitchPort).Options["nat-addresses"] = "router"
						expectedDatabaseState[9].(*nbdb.LogicalSwitchPort).Options["exclude-lb-vips-from-garp"] = "true"
						expectedDatabaseState[4].(*nbdb.LogicalRouter).Nat = []string{"egressip-nat-2-UUID"}
						expectedDatabaseState = append(expectedDatabaseState, natEIP2)
					}
					if node2Zone != node1Zone && node2Zone == "remote" {
						// the policy reroute will have its second nexthop as transit switchIP
						// so the one with join switchIP is where podNode == egressNode and one with transitIP is where podNode != egressNode
						expectedDatabaseState[0].(*nbdb.LogicalRouterPolicy).Nexthops = []string{"100.64.0.2", "168.254.0.3"}
					}
					if node2Zone != node1Zone && node1Zone == "remote" {
						expectedDatabaseState = append(expectedDatabaseState, getReRouteStaticRoute(egressPod.Status.PodIP, "100.64.0.3"))
						expectedDatabaseState[5].(*nbdb.LogicalRouter).Policies = []string{"default-no-reroute-UUID", "no-reroute-service-UUID"}
						expectedDatabaseState[5].(*nbdb.LogicalRouter).StaticRoutes = []string{"reroute-static-route-UUID"}
						expectedDatabaseState = expectedDatabaseState[1:] // policy is not visible since podNode is remote
					}
					gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

					latest, err := fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Get(context.TODO(), eIP.Name, metav1.GetOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					latest.Spec.EgressIPs = []string{egressIP3, egressIP2}
					_, err = fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Update(context.TODO(), latest, metav1.UpdateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					// NOTE: Cluster manager is the one who patches the egressIP object.
					// For the sake of unit testing egressip zone controller we need to patch egressIP object manually
					// There are tests in cluster-manager package covering the patch logic.
					status = []egressipv1.EgressIPStatusItem{
						{
							Node:     node1Name,
							EgressIP: egressIP3,
						},
						{
							Node:     node2Name,
							EgressIP: egressIP2,
						},
					}
					err = fakeOvn.controller.patchReplaceEgressIPStatus(eIP.Name, status)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					gomega.Eventually(func() []string {
						egressIPs, _ = getEgressIPStatus(eIP.Name)
						return egressIPs
					}).Should(gomega.ConsistOf(egressIP3, egressIP2))

					egressIPs, nodes = getEgressIPStatus(eIP.Name)
					assignmentNode1, assignmentNode2 = nodes[0], nodes[1]
					assignedEgressIP1, assignedEgressIP2 = egressIPs[0], egressIPs[1]

					expectedNatLogicalPort1 = fmt.Sprintf("k8s-%s", assignmentNode1)
					expectedNatLogicalPort2 = fmt.Sprintf("k8s-%s", assignmentNode2)
					expectedDatabaseState = []libovsdbtest.TestData{
						getReRoutePolicy(egressPod.Status.PodIP, "4", []string{"100.64.0.2", "100.64.0.3"}),
						&nbdb.LogicalRouterPolicy{
							Priority: types.DefaultNoRereoutePriority,
							Match:    "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
							Action:   nbdb.LogicalRouterPolicyActionAllow,
							UUID:     "default-no-reroute-UUID",
						},
						&nbdb.LogicalRouterPolicy{
							Priority: types.DefaultNoRereoutePriority,
							Match:    fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
							Action:   nbdb.LogicalRouterPolicyActionAllow,
							UUID:     "no-reroute-service-UUID",
						},
						&nbdb.LogicalRouter{
							Name: ovntypes.GWRouterPrefix + assignmentNode1,
							UUID: ovntypes.GWRouterPrefix + assignmentNode1 + "-UUID",
						},
						&nbdb.LogicalRouter{
							Name: ovntypes.GWRouterPrefix + assignmentNode2,
							UUID: ovntypes.GWRouterPrefix + assignmentNode2 + "-UUID",
						},
						&nbdb.LogicalRouter{
							Name:     ovntypes.OVNClusterRouter,
							UUID:     ovntypes.OVNClusterRouter + "-UUID",
							Policies: []string{"reroute-UUID", "default-no-reroute-UUID", "no-reroute-service-UUID"},
						},
						&nbdb.LogicalRouterPort{
							UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2.Name + "-UUID",
							Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2.Name,
							Networks: []string{"100.64.0.3/29"},
						},
						&nbdb.LogicalRouterPort{
							UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name + "-UUID",
							Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name,
							Networks: []string{"100.64.0.2/29"},
						},
						&nbdb.LogicalSwitchPort{
							UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "UUID",
							Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
							Type: "router",
							Options: map[string]string{
								"router-port": types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
							},
						},
						&nbdb.LogicalSwitchPort{
							UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "UUID",
							Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
							Type: "router",
							Options: map[string]string{
								"router-port": types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
							},
						},
					}
					if !interconnect || node1Zone == "global" {
						expectedDatabaseState[8].(*nbdb.LogicalSwitchPort).Options["nat-addresses"] = "router"
						expectedDatabaseState[8].(*nbdb.LogicalSwitchPort).Options["exclude-lb-vips-from-garp"] = "true"
						expectedDatabaseState[3].(*nbdb.LogicalRouter).Nat = []string{"egressip-nat-1-UUID"}
						natEIP1.ExternalIP = assignedEgressIP1
						expectedDatabaseState = append(expectedDatabaseState, natEIP1)
					}
					if !interconnect || node2Zone == "global" {
						expectedDatabaseState[9].(*nbdb.LogicalSwitchPort).Options["nat-addresses"] = "router"
						expectedDatabaseState[9].(*nbdb.LogicalSwitchPort).Options["exclude-lb-vips-from-garp"] = "true"
						expectedDatabaseState[4].(*nbdb.LogicalRouter).Nat = []string{"egressip-nat-2-UUID"}
						natEIP2.ExternalIP = assignedEgressIP2
						expectedDatabaseState = append(expectedDatabaseState, natEIP2)
					}
					if node2Zone != node1Zone && node2Zone == "remote" {
						// the policy reroute will have its second nexthop as transit switchIP
						// so the one with join switchIP is where podNode == egressNode and one with transitIP is where podNode != egressNode
						expectedDatabaseState[0].(*nbdb.LogicalRouterPolicy).Nexthops = []string{"100.64.0.2", "168.254.0.3"}
					}
					if node2Zone != node1Zone && node1Zone == "remote" {
						expectedDatabaseState = append(expectedDatabaseState, getReRouteStaticRoute(egressPod.Status.PodIP, "100.64.0.3"))
						expectedDatabaseState[5].(*nbdb.LogicalRouter).Policies = []string{"default-no-reroute-UUID", "no-reroute-service-UUID"}
						expectedDatabaseState[5].(*nbdb.LogicalRouter).StaticRoutes = []string{"reroute-static-route-UUID"}
						expectedDatabaseState = expectedDatabaseState[1:] // policy is not visible since podNode is remote
					}
					gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			},
			ginkgotable.Entry("interconnect disabled; non-ic - single zone setup", false, "global", "global"),
			ginkgotable.Entry("interconnect enabled; node1 and node2 in global zones", true, "global", "global"),
			// will showcase localzone setup - master is in pod's zone where pod's reroute policy towards egressNode will be done.
			// NOTE: SNAT won't be visible because its in remote zone
			ginkgotable.Entry("interconnect enabled; node1 in global and node2 in remote zones", true, "global", "remote"),
			// will showcase localzone setup - master is in egress node's zone where pod's SNAT policy and static route will be done.
			// NOTE: reroute policy won't be visible because its in remote zone (pod is in remote zone)
			ginkgotable.Entry("interconnect enabled; node1 in remote and node2 in global zones", true, "remote", "global"),
		)

		ginkgo.It("should delete and re-create", func() {
			app.Action = func(ctx *cli.Context) error {

				egressIP := net.ParseIP("0:0:0:0:0:feff:c0a8:8e0d")
				updatedEgressIP := net.ParseIP("0:0:0:0:0:feff:c0a8:8ffd")

				egressPod := *newPodWithLabels(namespace, podName, node1Name, podV6IP, egressPodLabel)
				egressNamespace := newNamespaceWithLabels(namespace, egressPodLabel)

				fakeOvn.startWithDBSetup(
					libovsdbtest.TestSetup{
						NBData: []libovsdbtest.TestData{
							&nbdb.LogicalRouterPort{
								UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2Name + "-UUID",
								Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2Name,
								Networks: []string{nodeLogicalRouterIfAddrV6},
							},
							&nbdb.LogicalRouter{
								Name: ovntypes.OVNClusterRouter,
								UUID: ovntypes.OVNClusterRouter + "-UUID",
							},
							&nbdb.LogicalRouter{
								Name: ovntypes.GWRouterPrefix + node1Name,
								UUID: ovntypes.GWRouterPrefix + node1Name + "-UUID",
							},
							&nbdb.LogicalRouter{
								Name: ovntypes.GWRouterPrefix + node2Name,
								UUID: ovntypes.GWRouterPrefix + node2Name + "-UUID",
								Nat:  nil,
							},
						},
					},
					&v1.NamespaceList{
						Items: []v1.Namespace{*egressNamespace},
					},
					&v1.PodList{
						Items: []v1.Pod{egressPod},
					},
				)

				i, n, _ := net.ParseCIDR(podV6IP + "/23")
				n.IP = i
				fakeOvn.controller.logicalPortCache.add(&egressPod, "", types.DefaultNetworkName, "", nil, []*net.IPNet{n})
				fakeOvn.controller.logicalPortCache.add(&egressPod, "", types.DefaultNetworkName, "", nil, []*net.IPNet{n})
				// hack pod to be in the provided zone
				fakeOvn.controller.eIPC.nodeZoneState.Store(node1Name, true)
				fakeOvn.controller.eIPC.nodeZoneState.Store(node2Name, true)

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

				err := fakeOvn.controller.WatchEgressIPNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIPPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				_, err = fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Create(context.TODO(), &eIP, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				fakeOvn.patchEgressIPObj(node2Name, egressIP.String())

				gomega.Eventually(getEgressIPStatusLen(eIP.Name)).Should(gomega.Equal(1))

				expectedNatLogicalPort := "k8s-node2"
				expectedDatabaseState := []libovsdbtest.TestData{
					&nbdb.LogicalRouterPolicy{
						Priority: types.EgressIPReroutePriority,
						Match:    fmt.Sprintf("ip6.src == %s", egressPod.Status.PodIP),
						Action:   nbdb.LogicalRouterPolicyActionReroute,
						Nexthops: nodeLogicalRouterIPv6,
						ExternalIDs: map[string]string{
							"name": eIP.Name,
						},
						UUID: "reroute-UUID",
					},
					&nbdb.LogicalRouter{
						Name:     ovntypes.OVNClusterRouter,
						UUID:     ovntypes.OVNClusterRouter + "-UUID",
						Policies: []string{"reroute-UUID"},
					},
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2Name + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2Name,
						Networks: []string{nodeLogicalRouterIfAddrV6},
					},
					&nbdb.NAT{
						UUID:       "egressip-nat-UUID",
						LogicalIP:  podV6IP,
						ExternalIP: egressIP.String(),
						ExternalIDs: map[string]string{
							"name": egressIPName,
						},
						Type:        nbdb.NATTypeSNAT,
						LogicalPort: &expectedNatLogicalPort,
						Options: map[string]string{
							"stateless": "false",
						},
					},
					&nbdb.LogicalRouter{
						Name: ovntypes.GWRouterPrefix + node1Name,
						UUID: ovntypes.GWRouterPrefix + node1Name + "-UUID",
					},
					&nbdb.LogicalRouter{
						Name: ovntypes.GWRouterPrefix + node2Name,
						UUID: ovntypes.GWRouterPrefix + node2Name + "-UUID",
						Nat:  []string{"egressip-nat-UUID"},
					},
				}
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				egressIPs, nodes := getEgressIPStatus(eIP.Name)
				gomega.Expect(nodes[0]).To(gomega.Equal(node2Name))
				gomega.Expect(egressIPs[0]).To(gomega.Equal(egressIP.String()))

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

				_, err = fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Update(context.TODO(), eIPUpdate, metav1.UpdateOptions{})
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				fakeOvn.patchEgressIPObj(node2Name, updatedEgressIP.String())
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				gomega.Eventually(func() []string {
					egressIPs, _ = getEgressIPStatus(eIP.Name)
					return egressIPs
				}).Should(gomega.ContainElement(updatedEgressIP.String()))

				gomega.Expect(nodes[0]).To(gomega.Equal(node2Name))
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

				annotations := map[string]string{
					"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4, node1IPv6),
					"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":[\"%s\", \"%s\"]}", v4NodeSubnet, v6NodeSubnet),
				}
				node1 := getNodeObj(node1Name, annotations, map[string]string{})
				annotations = map[string]string{
					"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node2IPv4, ""),
					"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":\"%s\"}", v4NodeSubnet),
				}
				node2 := getNodeObj(node2Name, annotations, map[string]string{})
				fakeOvn.startWithDBSetup(libovsdbtest.TestSetup{
					NBData: []libovsdbtest.TestData{
						&nbdb.LogicalRouter{
							Name: ovntypes.OVNClusterRouter,
							UUID: ovntypes.OVNClusterRouter + "-UUID",
						},
						&nbdb.LogicalRouter{
							Name: ovntypes.GWRouterPrefix + node1.Name,
							UUID: ovntypes.GWRouterPrefix + node1.Name + "-UUID",
						},
						&nbdb.LogicalRouter{
							Name: ovntypes.GWRouterPrefix + node2.Name,
							UUID: ovntypes.GWRouterPrefix + node2.Name + "-UUID",
						},
						&nbdb.LogicalSwitchPort{
							UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "UUID",
							Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
							Type: "router",
							Options: map[string]string{
								"router-port": types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
							},
						},
						&nbdb.LogicalSwitchPort{
							UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "UUID",
							Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
							Type: "router",
							Options: map[string]string{
								"router-port": types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
							},
						},
					},
				})
				err := fakeOvn.controller.WatchEgressNodes()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				node1.Labels = map[string]string{
					"k8s.ovn.org/egress-assignable": "",
				}

				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Create(context.TODO(), &node1, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				node2.Labels = map[string]string{
					"k8s.ovn.org/egress-assignable": "",
				}

				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Create(context.TODO(), &node2, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectedDatabaseState := []libovsdbtest.TestData{
					&nbdb.LogicalRouter{
						Name:     ovntypes.OVNClusterRouter,
						UUID:     ovntypes.OVNClusterRouter + "-UUID",
						Policies: []string{"reroute-UUID", "no-reroute-service-UUID"},
					},
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match:    "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
						Action:   nbdb.LogicalRouterPolicyActionAllow,
						UUID:     "reroute-UUID",
					},
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match:    fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
						Action:   nbdb.LogicalRouterPolicyActionAllow,
						UUID:     "no-reroute-service-UUID",
					},
					&nbdb.LogicalRouter{
						Name: ovntypes.GWRouterPrefix + node1.Name,
						UUID: ovntypes.GWRouterPrefix + node1.Name + "-UUID",
					},
					&nbdb.LogicalRouter{
						Name: ovntypes.GWRouterPrefix + node2.Name,
						UUID: ovntypes.GWRouterPrefix + node2.Name + "-UUID",
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
						Type: "router",
						Options: map[string]string{
							"router-port":               types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
						Type: "router",
						Options: map[string]string{
							"router-port":               types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
				}
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("using retry to create egress node with forced error followed by an update", func() {
			app.Action = func(ctx *cli.Context) error {
				nodeIPv4 := "192.168.126.51/24"
				nodeIPv6 := "0:0:0:0:0:feff:c0a8:8e0c/64"
				annotations := map[string]string{
					"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", nodeIPv4, nodeIPv6),
					"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":[\"%s\", \"%s\"]}", v4NodeSubnet, v6NodeSubnet),
				}
				node := getNodeObj("node", annotations, map[string]string{})
				fakeOvn.startWithDBSetup(libovsdbtest.TestSetup{
					NBData: []libovsdbtest.TestData{
						&nbdb.LogicalRouter{
							Name: ovntypes.OVNClusterRouter,
							UUID: ovntypes.OVNClusterRouter + "-UUID",
						},
						&nbdb.LogicalRouter{
							Name: ovntypes.GWRouterPrefix + node.Name,
							UUID: ovntypes.GWRouterPrefix + node.Name + "-UUID",
						},
						&nbdb.LogicalSwitchPort{
							UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + nodeName + "UUID",
							Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + nodeName,
							Type: "router",
							Options: map[string]string{
								"router-port": types.GWRouterToExtSwitchPrefix + "GR_" + nodeName,
							},
						},
					},
				})
				err := fakeOvn.controller.WatchEgressNodes()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				node.Labels = map[string]string{
					"k8s.ovn.org/egress-assignable": "",
				}
				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Create(context.TODO(), &node, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				ginkgo.By("Bringing down NBDB")
				// inject transient problem, nbdb is down
				fakeOvn.controller.nbClient.Close()
				gomega.Eventually(func() bool {
					return fakeOvn.controller.nbClient.Connected()
				}).Should(gomega.BeFalse())

				// sleep long enough for TransactWithRetry to fail, causing egressnode operations to fail
				// there is a chance that both egressnode events(node1 removal and node2 update) will end up in the same event queue
				// sleep for double the time to allow for two consecutive TransactWithRetry timeouts
				time.Sleep(2 * (types.OVSDBTimeout + time.Second))
				// check to see if the retry cache has an entry
				key, err := retry.GetResourceKey(&node)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				retry.CheckRetryObjectEventually(key, true, fakeOvn.controller.retryEgressNodes)
				ginkgo.By("retry entry: old obj should be nil, new obj should not be nil")
				retry.CheckRetryObjectMultipleFieldsEventually(
					key,
					fakeOvn.controller.retryEgressNodes,
					gomega.BeNil(),             // oldObj should be nil
					gomega.Not(gomega.BeNil()), // newObj should not be nil
				)

				node.Labels = map[string]string{}
				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), &node, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				connCtx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
				defer cancel()
				resetNBClient(connCtx, fakeOvn.controller.nbClient)
				retry.SetRetryObjWithNoBackoff(key, fakeOvn.controller.retryEgressNodes)
				fakeOvn.controller.retryEgressNodes.RequestRetryObjs()
				// check the cache no longer has the entry
				retry.CheckRetryObjectEventually(key, false, fakeOvn.controller.retryEgressNodes)

				expectedDatabaseState := []libovsdbtest.TestData{
					&nbdb.LogicalRouter{
						Name:     ovntypes.OVNClusterRouter,
						UUID:     ovntypes.OVNClusterRouter + "-UUID",
						Policies: []string{"reroute-UUID", "no-reroute-service-UUID"},
					},
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match:    "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
						Action:   nbdb.LogicalRouterPolicyActionAllow,
						UUID:     "reroute-UUID",
					},
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match:    fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
						Action:   nbdb.LogicalRouterPolicyActionAllow,
						UUID:     "no-reroute-service-UUID",
					},
					&nbdb.LogicalRouter{
						Name: ovntypes.GWRouterPrefix + node.Name,
						UUID: ovntypes.GWRouterPrefix + node.Name + "-UUID",
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + nodeName + "UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + nodeName,
						Type: "router",
						Options: map[string]string{
							"router-port": types.GWRouterToExtSwitchPrefix + "GR_" + nodeName,
						},
					},
				}
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("egressIP pod recreate with same name (stateful-sets) shouldn't use stale logicalPortCache entries", func() {
			app.Action = func(ctx *cli.Context) error {

				config.Gateway.DisableSNATMultipleGWs = true

				egressIP1 := "192.168.126.101"
				node1IPv4 := "192.168.126.12/24"

				egressPod1 := *newPodWithLabels(namespace, podName, node1Name, "", egressPodLabel)
				egressNamespace := newNamespace(namespace)
				annotations := map[string]string{
					"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\"}", node1IPv4),
					"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":\"%s\"}", v4NodeSubnet),
					"k8s.ovn.org/l3-gateway-config":   `{"default":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"192.168.126.12/24", "next-hop":"192.168.126.1"}}`,
					"k8s.ovn.org/node-chassis-id":     "79fdcfc4-6fe6-4cd3-8242-c0f85a4668ec",
				}
				labels := map[string]string{
					"k8s.ovn.org/egress-assignable": "",
				}
				node1 := getNodeObj(node1Name, annotations, labels)

				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{egressIP1},
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
						Items: []egressipv1.EgressIPStatusItem{
							{
								Node:     node1.Name,
								EgressIP: egressIP1,
							},
						},
					},
				}
				nodeSwitch := &nbdb.LogicalSwitch{
					UUID: node1.Name + "-UUID",
					Name: node1.Name,
				}

				fakeOvn.startWithDBSetup(
					libovsdbtest.TestSetup{
						NBData: []libovsdbtest.TestData{
							&nbdb.LogicalRouter{
								Name: ovntypes.OVNClusterRouter,
								UUID: ovntypes.OVNClusterRouter + "-UUID",
							},
							&nbdb.LogicalRouter{
								Name: ovntypes.GWRouterPrefix + node1.Name,
								UUID: ovntypes.GWRouterPrefix + node1.Name + "-UUID",
							},
							&nbdb.LogicalRouterPort{
								UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name + "-UUID",
								Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name,
								Networks: []string{nodeLogicalRouterIfAddrV4},
							},
							&nbdb.LogicalSwitchPort{
								UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "UUID",
								Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
								Type: "router",
								Options: map[string]string{
									"router-port": types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
								},
							},
							nodeSwitch,
						},
					},
					&egressipv1.EgressIPList{
						Items: []egressipv1.EgressIP{eIP},
					},
					&v1.NodeList{
						Items: []v1.Node{node1},
					},
					&v1.NamespaceList{
						Items: []v1.Namespace{*egressNamespace},
					},
					&v1.PodList{
						Items: []v1.Pod{egressPod1},
					},
				)
				// we don't know the real switch UUID in the db, but it can be found by name
				swUUID := getLogicalSwitchUUID(fakeOvn.controller.nbClient, node1.Name)
				fakeOvn.controller.lsManager.AddSwitch(node1.Name, swUUID, []*net.IPNet{ovntest.MustParseIPNet(v4NodeSubnet)})

				err := fakeOvn.controller.WatchPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIPNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIPPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressNodes()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				egressPodPortInfo, err := fakeOvn.controller.logicalPortCache.get(&egressPod1, types.DefaultNetworkName)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				egressPodIP, _, err := net.ParseCIDR(egressPodPortInfo.ips[0].String())
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(egressPodPortInfo.expires.IsZero()).To(gomega.BeTrue())
				podAddr := fmt.Sprintf("%s %s", egressPodPortInfo.mac.String(), egressPodIP)

				expectedNatLogicalPort1 := "k8s-node1"
				expectedDatabaseStatewithPod := []libovsdbtest.TestData{
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match:    "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
						Action:   nbdb.LogicalRouterPolicyActionAllow,
						UUID:     "no-reroute-UUID",
					},
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match:    fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
						Action:   nbdb.LogicalRouterPolicyActionAllow,
						UUID:     "no-reroute-service-UUID",
					},
					&nbdb.LogicalRouterPolicy{
						Priority: types.EgressIPReroutePriority,
						Match:    fmt.Sprintf("ip4.src == %s", egressPodIP),
						Action:   nbdb.LogicalRouterPolicyActionReroute,
						Nexthops: nodeLogicalRouterIPv4,
						ExternalIDs: map[string]string{
							"name": eIP.Name,
						},
						UUID: "reroute-UUID1",
					},
					&nbdb.LogicalRouter{
						Name:     ovntypes.OVNClusterRouter,
						UUID:     ovntypes.OVNClusterRouter + "-UUID",
						Policies: []string{"no-reroute-UUID", "no-reroute-service-UUID", "reroute-UUID1"},
					},
					&nbdb.LogicalRouter{
						Name: ovntypes.GWRouterPrefix + node1.Name,
						UUID: ovntypes.GWRouterPrefix + node1.Name + "-UUID",
						Nat:  []string{"egressip-nat-UUID1"},
					},
					&nbdb.NAT{
						UUID:       "egressip-nat-UUID1",
						LogicalIP:  egressPodIP.String(),
						ExternalIP: egressIP1,
						ExternalIDs: map[string]string{
							"name": egressIPName,
						},
						Type:        nbdb.NATTypeSNAT,
						LogicalPort: &expectedNatLogicalPort1,
						Options: map[string]string{
							"stateless": "false",
						},
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
						Type: "router",
						Options: map[string]string{
							"router-port":               types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name,
						Networks: []string{"100.64.0.2/29"},
					},
					nodeSwitch,
				}
				podLSP := &nbdb.LogicalSwitchPort{
					UUID:      util.GetLogicalPortName(egressPod1.Namespace, egressPod1.Name) + "-UUID",
					Name:      util.GetLogicalPortName(egressPod1.Namespace, egressPod1.Name),
					Addresses: []string{podAddr},
					ExternalIDs: map[string]string{
						"pod":       "true",
						"namespace": egressPod1.Namespace,
					},
					Options: map[string]string{
						"requested-chassis": egressPod1.Spec.NodeName,
						"iface-id-ver":      egressPod1.Name,
					},
					PortSecurity: []string{podAddr},
				}
				nodeSwitch.Ports = []string{podLSP.UUID}
				finalDatabaseStatewithPod := append(expectedDatabaseStatewithPod, podLSP)
				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
				_, nodes := getEgressIPStatus(egressIPName)
				gomega.Expect(nodes[0]).To(gomega.Equal(node1.Name))

				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(finalDatabaseStatewithPod))

				// delete the pod
				err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(egressPod1.Namespace).Delete(context.TODO(),
					egressPod1.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				expectedDatabaseStateWithoutPod := []libovsdbtest.TestData{
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match:    "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
						Action:   nbdb.LogicalRouterPolicyActionAllow,
						UUID:     "no-reroute-UUID",
					},
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match:    fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
						Action:   nbdb.LogicalRouterPolicyActionAllow,
						UUID:     "no-reroute-service-UUID",
					},
					&nbdb.LogicalRouter{
						Name:     ovntypes.OVNClusterRouter,
						UUID:     ovntypes.OVNClusterRouter + "-UUID",
						Policies: []string{"no-reroute-UUID", "no-reroute-service-UUID"},
					},
					&nbdb.LogicalRouter{
						Name: ovntypes.GWRouterPrefix + node1.Name,
						UUID: ovntypes.GWRouterPrefix + node1.Name + "-UUID",
						Nat:  []string{},
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
						Type: "router",
						Options: map[string]string{
							"router-port":               types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name,
						Networks: []string{"100.64.0.2/29"},
					},
					&nbdb.LogicalSwitch{
						UUID:  node1.Name + "-UUID",
						Name:  node1.Name,
						Ports: []string{},
					},
				}
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseStateWithoutPod))
				// recreate pod with same name immediately; simulating handler race (pods v/s egressip) condition,
				// so instead of proper pod create, we try out egressIP pod setup which will be a no-op since pod doesn't exist
				ginkgo.By("should not add egress IP setup for a deleted pod whose entry exists in logicalPortCache")
				err = fakeOvn.controller.addPodEgressIPAssignments(egressIPName, eIP.Status.Items, &egressPod1)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// pod is gone but logicalPortCache holds the entry for 60seconds
				egressPodPortInfo, err = fakeOvn.controller.logicalPortCache.get(&egressPod1, types.DefaultNetworkName)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(egressPodPortInfo.expires.IsZero()).To(gomega.BeFalse())
				staleEgressPodIP, _, err := net.ParseCIDR(egressPodPortInfo.ips[0].String())
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(staleEgressPodIP).To(gomega.Equal(egressPodIP))
				// no-op
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseStateWithoutPod))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("egressIP pod recreate with same name (stateful-sets) shouldn't use stale logicalPortCache entries AND stale podAssignment cache entries", func() {
			app.Action = func(ctx *cli.Context) error {

				config.Gateway.DisableSNATMultipleGWs = true

				egressIP1 := "192.168.126.101"
				node1IPv4 := "192.168.126.12/24"

				oldEgressPodIP := "10.128.0.50"
				egressPod1 := newPodWithLabels(namespace, podName, node1Name, "", egressPodLabel)
				oldAnnotation := map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["10.128.0.50/24"],"mac_address":"0a:58:0a:80:00:05","gateway_ips":["10.128.0.1"],"routes":[{"dest":"10.128.0.0/24","nextHop":"10.128.0.1"}],"ip_address":"10.128.0.50/24","gateway_ip":"10.128.0.1"}}`}
				egressPod1.Annotations = oldAnnotation
				egressNamespace := newNamespace(namespace)

				annotations := map[string]string{
					"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\"}", node1IPv4),
					"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":\"%s\"}", v4NodeSubnet),
					"k8s.ovn.org/l3-gateway-config":   `{"default":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"192.168.126.12/24", "next-hop":"192.168.126.1"}}`,
					"k8s.ovn.org/node-chassis-id":     "79fdcfc4-6fe6-4cd3-8242-c0f85a4668ec",
				}
				labels := map[string]string{
					"k8s.ovn.org/egress-assignable": "",
				}
				node1 := getNodeObj(node1Name, annotations, labels)

				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{egressIP1},
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
						Items: []egressipv1.EgressIPStatusItem{
							{
								Node:     node1.Name,
								EgressIP: egressIP1,
							},
						},
					},
				}
				nodeSwitch := &nbdb.LogicalSwitch{
					UUID: node1.Name + "-UUID",
					Name: node1.Name,
				}

				fakeOvn.startWithDBSetup(
					libovsdbtest.TestSetup{
						NBData: []libovsdbtest.TestData{
							&nbdb.LogicalRouter{
								Name: ovntypes.OVNClusterRouter,
								UUID: ovntypes.OVNClusterRouter + "-UUID",
							},
							&nbdb.LogicalRouter{
								Name: ovntypes.GWRouterPrefix + node1.Name,
								UUID: ovntypes.GWRouterPrefix + node1.Name + "-UUID",
							},
							&nbdb.LogicalRouterPort{
								UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name + "-UUID",
								Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name,
								Networks: []string{nodeLogicalRouterIfAddrV4},
							},
							&nbdb.LogicalSwitchPort{
								UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "UUID",
								Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
								Type: "router",
								Options: map[string]string{
									"router-port": types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
								},
							},
							nodeSwitch,
						},
					},
					&egressipv1.EgressIPList{
						Items: []egressipv1.EgressIP{eIP},
					},
					&v1.NodeList{
						Items: []v1.Node{node1},
					},
					&v1.NamespaceList{
						Items: []v1.Namespace{*egressNamespace},
					},
					&v1.PodList{
						Items: []v1.Pod{*egressPod1},
					},
				)

				// we don't know the real switch UUID in the db, but it can be found by name
				swUUID := getLogicalSwitchUUID(fakeOvn.controller.nbClient, node1.Name)
				fakeOvn.controller.lsManager.AddSwitch(node1.Name, swUUID, []*net.IPNet{ovntest.MustParseIPNet(v4NodeSubnet)})
				fakeOvn.controller.WatchPods()
				fakeOvn.controller.WatchEgressIPNamespaces()
				fakeOvn.controller.WatchEgressIPPods()
				fakeOvn.controller.WatchEgressNodes()
				fakeOvn.controller.WatchEgressIP()

				oldEgressPodPortInfo, err := fakeOvn.controller.logicalPortCache.get(egressPod1, types.DefaultNetworkName)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				egressPodIP, _, err := net.ParseCIDR(oldEgressPodPortInfo.ips[0].String())
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(egressPodIP.String()).To(gomega.Equal(oldEgressPodIP))
				gomega.Expect(oldEgressPodPortInfo.expires.IsZero()).To(gomega.BeTrue())
				podAddr := fmt.Sprintf("%s %s", oldEgressPodPortInfo.mac.String(), egressPodIP)

				expectedNatLogicalPort1 := "k8s-node1"
				podEIPSNAT := &nbdb.NAT{
					UUID:       "egressip-nat-UUID1",
					LogicalIP:  egressPodIP.String(),
					ExternalIP: egressIP1,
					ExternalIDs: map[string]string{
						"name": egressIPName,
					},
					Type:        nbdb.NATTypeSNAT,
					LogicalPort: &expectedNatLogicalPort1,
					Options: map[string]string{
						"stateless": "false",
					},
				}
				podReRoutePolicy := &nbdb.LogicalRouterPolicy{
					Priority: types.EgressIPReroutePriority,
					Match:    fmt.Sprintf("ip4.src == %s", oldEgressPodIP),
					Action:   nbdb.LogicalRouterPolicyActionReroute,
					Nexthops: nodeLogicalRouterIPv4,
					ExternalIDs: map[string]string{
						"name": eIP.Name,
					},
					UUID: "reroute-UUID1",
				}
				node1GR := &nbdb.LogicalRouter{
					Name: ovntypes.GWRouterPrefix + node1.Name,
					UUID: ovntypes.GWRouterPrefix + node1.Name + "-UUID",
					Nat:  []string{"egressip-nat-UUID1"},
				}
				expectedDatabaseStatewithPod := []libovsdbtest.TestData{
					podEIPSNAT,
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match:    "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
						Action:   nbdb.LogicalRouterPolicyActionAllow,
						UUID:     "no-reroute-UUID",
					},
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match:    fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
						Action:   nbdb.LogicalRouterPolicyActionAllow,
						UUID:     "no-reroute-service-UUID",
					},
					podReRoutePolicy,
					&nbdb.LogicalRouter{
						Name:     ovntypes.OVNClusterRouter,
						UUID:     ovntypes.OVNClusterRouter + "-UUID",
						Policies: []string{"no-reroute-UUID", "no-reroute-service-UUID", "reroute-UUID1"},
					},
					node1GR,
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
						Type: "router",
						Options: map[string]string{
							"router-port":               types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name,
						Networks: []string{"100.64.0.2/29"},
					},
					nodeSwitch,
				}
				podLSP := &nbdb.LogicalSwitchPort{
					UUID:      util.GetLogicalPortName(egressPod1.Namespace, egressPod1.Name) + "-UUID",
					Name:      util.GetLogicalPortName(egressPod1.Namespace, egressPod1.Name),
					Addresses: []string{podAddr},
					ExternalIDs: map[string]string{
						"pod":       "true",
						"namespace": egressPod1.Namespace,
					},
					Options: map[string]string{
						"requested-chassis": egressPod1.Spec.NodeName,
						"iface-id-ver":      egressPod1.Name,
					},
					PortSecurity: []string{podAddr},
				}
				nodeSwitch.Ports = []string{podLSP.UUID}
				finalDatabaseStatewithPod := append(expectedDatabaseStatewithPod, podLSP)
				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
				_, nodes := getEgressIPStatus(egressIPName)
				gomega.Expect(nodes[0]).To(gomega.Equal(node1.Name))

				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(finalDatabaseStatewithPod))

				// delete the pod and simulate a cleanup failure:
				// 1) create a situation where pod is gone from kapi but egressIP setup wasn't cleanedup due to deletion error
				//    - we remove annotation from pod to mimic this situation
				// 2) leaves us with a stale podAssignment cache
				// 3) check to make sure the logicalPortCache is used always even if podAssignment already has the podKey
				ginkgo.By("delete the egress IP pod and force the deletion to fail")
				egressPod1.Annotations = map[string]string{}
				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(egressPod1.Namespace).Update(context.TODO(), egressPod1, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// Wait for the cleared annotations to show up client-side
				gomega.Eventually(func() int {
					egressPod1, _ = fakeOvn.watcher.GetPod(egressPod1.Namespace, egressPod1.Name)
					return len(egressPod1.Annotations)
				}, 5).Should(gomega.Equal(0))

				// Delete the pod to trigger the cleanup failure
				err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(egressPod1.Namespace).Delete(context.TODO(),
					egressPod1.Name, metav1.DeleteOptions{})
				// internally we have an error:
				// E1006 12:51:59.594899 2500972 obj_retry.go:1517] Failed to delete *factory.egressIPPod egressip-namespace/egress-pod, error: pod egressip-namespace/egress-pod: no pod IPs found
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// notice that pod objects aren't cleaned up yet since deletion failed!
				// even the LSP sticks around for 60 seconds
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(finalDatabaseStatewithPod))
				// egressIP cache is stale in the sense the podKey has not been deleted since deletion failed
				pas := getPodAssignmentState(egressPod1)
				gomega.Expect(pas).NotTo(gomega.BeNil())
				gomega.Expect(pas.egressStatuses).To(gomega.Equal(map[egressipv1.EgressIPStatusItem]string{
					{
						Node:     "node1",
						EgressIP: "192.168.126.101",
					}: "",
				}))
				// recreate pod with same name immediately;
				ginkgo.By("should add egress IP setup for the NEW pod which exists in logicalPortCache")
				newEgressPodIP := "10.128.0.60"
				egressPod1 = newPodWithLabels(namespace, podName, node1Name, newEgressPodIP, egressPodLabel)
				egressPod1.Annotations = map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["10.128.0.60/24"],"mac_address":"0a:58:0a:80:00:06","gateway_ips":["10.128.0.1"],"routes":[{"dest":"10.128.0.0/24","nextHop":"10.128.0.1"}],"ip_address":"10.128.0.60/24","gateway_ip":"10.128.0.1"}}`}
				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(egressPod1.Namespace).Create(context.TODO(), egressPod1, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// wait for the logical port cache to get updated with the new pod's IP
				var newEgressPodPortInfo *lpInfo
				getEgressPodIP := func() string {
					newEgressPodPortInfo, err = fakeOvn.controller.logicalPortCache.get(egressPod1, types.DefaultNetworkName)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					egressPodIP, _, err := net.ParseCIDR(newEgressPodPortInfo.ips[0].String())
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					return egressPodIP.String()
				}
				gomega.Eventually(func() string {
					return getEgressPodIP()
				}).Should(gomega.Equal(newEgressPodIP))
				gomega.Expect(newEgressPodPortInfo.expires.IsZero()).To(gomega.BeTrue())

				// deletion for the older EIP pod object is still being retried so we still have SNAT
				// towards nodeIP for new pod which is created by addLogicalPort.
				// Note that we while have the stale re-route policy for old pod, the snat for the old pod towards egressIP is gone
				// because deleteLogicalPort removes ALL snats for a given pod but doesn't remove the policies.
				ipv4Addr, _, _ := net.ParseCIDR(node1IPv4)
				podNodeSNAT := &nbdb.NAT{
					UUID:       "node-nat-UUID1",
					LogicalIP:  newEgressPodIP,
					ExternalIP: ipv4Addr.String(),
					Type:       nbdb.NATTypeSNAT,
					Options: map[string]string{
						"stateless": "false",
					},
				}
				finalDatabaseStatewithPod = append(finalDatabaseStatewithPod, podNodeSNAT)
				node1GR.Nat = []string{podNodeSNAT.UUID}
				podAddr = fmt.Sprintf("%s %s", newEgressPodPortInfo.mac.String(), newEgressPodIP)
				podLSP.PortSecurity = []string{podAddr}
				podLSP.Addresses = []string{podAddr}
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(finalDatabaseStatewithPod[1:]))

				ginkgo.By("trigger a forced retry and ensure deletion of oldPod and creation of newPod are successful")
				// let us add back the annotation to the oldPod which is being retried to make deletion a success
				podKey, err := retry.GetResourceKey(egressPod1)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				retry.CheckRetryObjectEventually(podKey, true, fakeOvn.controller.retryEgressIPPods)
				retryOldObj := retry.GetOldObjFromRetryObj(podKey, fakeOvn.controller.retryEgressIPPods)
				//fakeOvn.controller.retryEgressIPPods.retryEntries.LoadOrStore(podKey, &RetryObjEntry{backoffSec: 1})
				pod, _ := retryOldObj.(*v1.Pod)
				pod.Annotations = oldAnnotation
				fakeOvn.controller.retryEgressIPPods.RequestRetryObjs()
				// there should also be no entry for this pod in the retry cache
				gomega.Eventually(func() bool {
					return retry.CheckRetryObj(podKey, fakeOvn.controller.retryEgressIPPods)
				}, retry.RetryObjInterval+time.Second).Should(gomega.BeFalse())

				// ensure that egressIP setup is being done with the new pod's information from logicalPortCache
				podReRoutePolicy.Match = fmt.Sprintf("ip4.src == %s", newEgressPodIP)
				podEIPSNAT.LogicalIP = newEgressPodIP
				node1GR.Nat = []string{podEIPSNAT.UUID}
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(finalDatabaseStatewithPod[:len(finalDatabaseStatewithPod)-1]))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgotable.DescribeTable("egressIP pod managed by multiple objects, verify standby works wells, verify syncPodAssignmentCache on restarts",
			func(interconnect bool, node1Zone, node2Zone string) {
				config.OVNKubernetesFeature.EnableInterconnect = interconnect
				app.Action = func(ctx *cli.Context) error {

					config.Gateway.DisableSNATMultipleGWs = true

					egressIP1 := "192.168.126.25"
					egressIP2 := "192.168.126.30"
					egressIP3 := "192.168.126.35"
					node1IPv4 := "192.168.126.12/24"
					node2IPv4 := "192.168.126.13/24"

					egressPod1 := *newPodWithLabels(namespace, podName, node1Name, "", egressPodLabel)
					egressNamespace := newNamespace(namespace)
					annotations := map[string]string{
						"k8s.ovn.org/node-primary-ifaddr":             fmt.Sprintf("{\"ipv4\": \"%s\"}", node1IPv4),
						"k8s.ovn.org/node-subnets":                    fmt.Sprintf("{\"default\":\"%s\"}", v4NodeSubnet),
						"k8s.ovn.org/l3-gateway-config":               `{"default":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"192.168.126.12/24", "next-hop":"192.168.126.1"}}`,
						"k8s.ovn.org/node-chassis-id":                 "79fdcfc4-6fe6-4cd3-8242-c0f85a4668ec",
						"k8s.ovn.org/node-transit-switch-port-ifaddr": "{\"ipv4\":\"168.254.0.2/16\"}", // used only for ic=true test
						"k8s.ovn.org/zone-name":                       node1Zone,                       // used only for ic=true test
					}
					labels := map[string]string{
						"k8s.ovn.org/egress-assignable": "",
					}
					node1 := getNodeObj(node1Name, annotations, labels)
					annotations = map[string]string{
						"k8s.ovn.org/node-primary-ifaddr":             fmt.Sprintf("{\"ipv4\": \"%s\"}", node2IPv4),
						"k8s.ovn.org/node-subnets":                    fmt.Sprintf("{\"default\":\"%s\"}", v4NodeSubnet),
						"k8s.ovn.org/l3-gateway-config":               `{"default":{"mode":"local","mac-address":"7e:57:f8:f0:3c:50", "ip-address":"192.168.126.13/24", "next-hop":"192.168.126.1"}}`,
						"k8s.ovn.org/node-chassis-id":                 "79fdcfc4-6fe6-4cd3-8242-c0f85a4668ec",
						"k8s.ovn.org/node-transit-switch-port-ifaddr": "{\"ipv4\":\"168.254.0.3/16\"}", // used only for ic=true test
						"k8s.ovn.org/zone-name":                       node2Zone,                       // used only for ic=true test
					}
					node2 := getNodeObj(node2Name, annotations, map[string]string{})
					eIP1 := egressipv1.EgressIP{
						ObjectMeta: newEgressIPMeta(egressIPName),
						Spec: egressipv1.EgressIPSpec{
							EgressIPs: []string{egressIP1, egressIP2},
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

					eIP2 := egressipv1.EgressIP{
						ObjectMeta: newEgressIPMeta(egressIPName2),
						Spec: egressipv1.EgressIPSpec{
							EgressIPs: []string{egressIP3},
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

					node1Switch := &nbdb.LogicalSwitch{
						UUID: node1.Name + "-UUID",
						Name: node1.Name,
					}
					node1GR := &nbdb.LogicalRouter{
						Name: ovntypes.GWRouterPrefix + node1.Name,
						UUID: ovntypes.GWRouterPrefix + node1.Name + "-UUID",
					}
					node2GR := &nbdb.LogicalRouter{
						Name: ovntypes.GWRouterPrefix + node2.Name,
						UUID: ovntypes.GWRouterPrefix + node2.Name + "-UUID",
					}
					node1LSP := &nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
						Type: "router",
						Options: map[string]string{
							"router-port": types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
						},
					}
					node2LSP := &nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
						Type: "router",
						Options: map[string]string{
							"router-port": types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
						},
					}

					fakeOvn.startWithDBSetup(
						libovsdbtest.TestSetup{
							NBData: []libovsdbtest.TestData{
								&nbdb.LogicalRouter{
									Name: ovntypes.OVNClusterRouter,
									UUID: ovntypes.OVNClusterRouter + "-UUID",
								},
								node1GR, node2GR,
								node1LSP, node2LSP,
								&nbdb.LogicalRouterPort{
									UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2.Name + "-UUID",
									Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2.Name,
									Networks: []string{"100.64.0.3/29"},
								},
								&nbdb.LogicalRouterPort{
									UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name + "-UUID",
									Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name,
									Networks: []string{"100.64.0.2/29"},
								},
								node1Switch,
								&nbdb.LogicalSwitch{
									UUID: node2.Name + "-UUID",
									Name: node2.Name,
								},
							},
						},
						&egressipv1.EgressIPList{
							Items: []egressipv1.EgressIP{eIP1, eIP2},
						},
						&v1.NodeList{
							Items: []v1.Node{node1, node2},
						},
						&v1.NamespaceList{
							Items: []v1.Namespace{*egressNamespace},
						},
						&v1.PodList{
							Items: []v1.Pod{egressPod1},
						},
					)

					// we don't know the real switch UUID in the db, but it can be found by name
					sw1UUID := getLogicalSwitchUUID(fakeOvn.controller.nbClient, node1.Name)
					sw2UUID := getLogicalSwitchUUID(fakeOvn.controller.nbClient, node2.Name)
					fakeOvn.controller.lsManager.AddSwitch(node1.Name, sw1UUID, []*net.IPNet{ovntest.MustParseIPNet(v4NodeSubnet)})
					fakeOvn.controller.lsManager.AddSwitch(node2.Name, sw2UUID, []*net.IPNet{ovntest.MustParseIPNet(v4NodeSubnet)})
					err := fakeOvn.controller.WatchPods()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchEgressIPNamespaces()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchEgressIPPods()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchEgressNodes()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchEgressIP()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					fakeOvn.patchEgressIPObj(node1Name, egressIP1)

					// NOTE: Cluster manager is the one who patches the egressIP object.
					// For the sake of unit testing egressip zone controller we need to patch egressIP object manually
					// There are tests in cluster-manager package covering the patch logic.
					status := []egressipv1.EgressIPStatusItem{
						{
							Node:     node1Name,
							EgressIP: egressIP3,
						},
					}
					err = fakeOvn.controller.patchReplaceEgressIPStatus(egressIPName2, status)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					egressPodPortInfo, err := fakeOvn.controller.logicalPortCache.get(&egressPod1, types.DefaultNetworkName)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					ePod, err := fakeOvn.fakeClient.KubeClient.CoreV1().Pods(egressPod1.Namespace).Get(context.TODO(), egressPod1.Name, metav1.GetOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					egressPodIP, err := util.GetPodIPsOfNetwork(ePod, &util.DefaultNetInfo{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					egressNetPodIP, _, err := net.ParseCIDR(egressPodPortInfo.ips[0].String())
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					gomega.Expect(egressNetPodIP.String()).To(gomega.Equal(egressPodIP[0].String()))
					gomega.Expect(egressPodPortInfo.expires.IsZero()).To(gomega.BeTrue())
					podAddr := fmt.Sprintf("%s %s", egressPodPortInfo.mac.String(), egressPodIP[0].String())

					// Ensure first egressIP object is assigned, since only node1 is an egressNode, only 1IP will be assigned, other will be pending
					gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
					gomega.Eventually(getEgressIPReassignmentCount).Should(gomega.Equal(1))
					egressIPs1, nodes1 := getEgressIPStatus(egressIPName)
					gomega.Expect(nodes1[0]).To(gomega.Equal(node1.Name))
					gomega.Expect(egressIPs1[0]).To(gomega.Equal(egressIP1))

					// Ensure second egressIP object is also assigned to node1, but no OVN config will be done for this
					gomega.Eventually(getEgressIPStatusLen(egressIPName2)).Should(gomega.Equal(1))
					egressIPs2, nodes2 := getEgressIPStatus(egressIPName2)
					gomega.Expect(nodes2[0]).To(gomega.Equal(node1.Name))
					gomega.Expect(egressIPs2[0]).To(gomega.Equal(egressIP3))
					recordedEvent := <-fakeOvn.fakeRecorder.Events
					gomega.Expect(recordedEvent).To(gomega.ContainSubstring("EgressIP object egressip-2 will not be configured for pod egressip-namespace_egress-pod since another egressIP object egressip is serving it, this is undefined"))

					pas := getPodAssignmentState(&egressPod1)
					gomega.Expect(pas).NotTo(gomega.BeNil())

					assginedEIP := egressIPs1[0]
					gomega.Expect(pas.egressIPName).To(gomega.Equal(egressIPName))
					eip1Obj, err := fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Get(context.TODO(), eIP1.Name, metav1.GetOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					gomega.Expect(pas.egressStatuses[eip1Obj.Status.Items[0]]).To(gomega.Equal(""))
					gomega.Expect(pas.standbyEgressIPNames.Has(egressIPName2)).To(gomega.BeTrue())

					podEIPSNAT := &nbdb.NAT{
						UUID:       "egressip-nat-UUID1",
						LogicalIP:  egressPodIP[0].String(),
						ExternalIP: assginedEIP,
						ExternalIDs: map[string]string{
							"name": pas.egressIPName,
						},
						Type:        nbdb.NATTypeSNAT,
						LogicalPort: utilpointer.String("k8s-node1"),
						Options: map[string]string{
							"stateless": "false",
						},
					}
					podReRoutePolicy := &nbdb.LogicalRouterPolicy{
						Priority: types.EgressIPReroutePriority,
						Match:    fmt.Sprintf("ip4.src == %s", egressPodIP[0].String()),
						Action:   nbdb.LogicalRouterPolicyActionReroute,
						Nexthops: nodeLogicalRouterIPv4,
						ExternalIDs: map[string]string{
							"name": pas.egressIPName,
						},
						UUID: "reroute-UUID1",
					}
					node1GR.Nat = []string{"egressip-nat-UUID1"}
					node1LSP.Options = map[string]string{
						"router-port":               types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
						"nat-addresses":             "router",
						"exclude-lb-vips-from-garp": "true",
					}
					if node1Zone != node2Zone && node1Zone == "remote" {
						// GARP for remote zones are taken care of by remote controller
						node1LSP.Options = map[string]string{
							"router-port": types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
						}
					}
					if !interconnect || node2Zone == "global" {
						node2LSP.Options = map[string]string{
							"router-port":               types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						}
					}
					expectedDatabaseStatewithPod := []libovsdbtest.TestData{
						podEIPSNAT,
						podReRoutePolicy,
						&nbdb.LogicalRouterPolicy{
							Priority: types.DefaultNoRereoutePriority,
							Match:    "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
							Action:   nbdb.LogicalRouterPolicyActionAllow,
							UUID:     "no-reroute-UUID",
						},
						&nbdb.LogicalRouterPolicy{
							Priority: types.DefaultNoRereoutePriority,
							Match:    fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
							Action:   nbdb.LogicalRouterPolicyActionAllow,
							UUID:     "no-reroute-service-UUID",
						},
						&nbdb.LogicalRouter{
							Name:     ovntypes.OVNClusterRouter,
							UUID:     ovntypes.OVNClusterRouter + "-UUID",
							Policies: []string{"no-reroute-UUID", "no-reroute-service-UUID", "reroute-UUID1"},
						},
						node1GR, node2GR,
						node1LSP, node2LSP,
						&nbdb.LogicalRouterPort{
							UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name + "-UUID",
							Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name,
							Networks: []string{"100.64.0.2/29"},
						},
						&nbdb.LogicalRouterPort{
							UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2.Name + "-UUID",
							Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2.Name,
							Networks: []string{"100.64.0.3/29"},
						},
						node1Switch,
						&nbdb.LogicalSwitch{
							UUID: node2.Name + "-UUID",
							Name: node2.Name,
						},
					}
					podLSP := &nbdb.LogicalSwitchPort{
						UUID:      util.GetLogicalPortName(egressPod1.Namespace, egressPod1.Name) + "-UUID",
						Name:      util.GetLogicalPortName(egressPod1.Namespace, egressPod1.Name),
						Addresses: []string{podAddr},
						ExternalIDs: map[string]string{
							"pod":       "true",
							"namespace": egressPod1.Namespace,
						},
						Options: map[string]string{
							"requested-chassis": egressPod1.Spec.NodeName,
							"iface-id-ver":      egressPod1.Name,
						},
						PortSecurity: []string{podAddr},
					}
					node1Switch.Ports = []string{podLSP.UUID}
					finalDatabaseStatewithPod := append(expectedDatabaseStatewithPod, podLSP)
					if node1Zone == "remote" {
						// policy is not visible since podNode is in remote zone
						finalDatabaseStatewithPod[4].(*nbdb.LogicalRouter).Policies = []string{"no-reroute-UUID", "no-reroute-service-UUID"}
						finalDatabaseStatewithPod = finalDatabaseStatewithPod[2:]
						podEIPSNAT.ExternalIP = "192.168.126.12" // EIP SNAT is not visible since podNode is remote, SNAT towards nodeIP is visible.
						podEIPSNAT.LogicalPort = nil
						podNodeSNAT := &nbdb.NAT{
							UUID:       "node-nat-UUID1",
							LogicalIP:  egressPodIP[0].String(),
							ExternalIP: "192.168.126.12",
							Type:       nbdb.NATTypeSNAT,
							Options: map[string]string{
								"stateless": "false",
							},
						}
						finalDatabaseStatewithPod = append(finalDatabaseStatewithPod, podNodeSNAT)
						node1GR.Nat = []string{"node-nat-UUID1"}
					}

					gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(finalDatabaseStatewithPod))

					// Make second node egressIP assignable
					node2.Labels = map[string]string{
						"k8s.ovn.org/egress-assignable": "",
					}
					_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), &node2, metav1.UpdateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					// NOTE: Cluster manager is the one who patches the egressIP object.
					// For the sake of unit testing egressip zone controller we need to patch egressIP object manually
					// There are tests in cluster-manager package covering the patch logic.
					status = []egressipv1.EgressIPStatusItem{
						{
							Node:     node1Name,
							EgressIP: egressIP1,
						},
						{
							Node:     node2Name,
							EgressIP: egressIP2,
						},
					}
					err = fakeOvn.controller.patchReplaceEgressIPStatus(egressIPName, status)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					// ensure secondIP from first object gets assigned to node2
					gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(2))
					egressIPs1, nodes1 = getEgressIPStatus(egressIPName)
					gomega.Expect(nodes1[1]).To(gomega.Equal(node2.Name))
					gomega.Expect(egressIPs1[1]).To(gomega.Equal(egressIP2))

					podEIPSNAT2 := &nbdb.NAT{
						UUID:       "egressip-nat-UUID2",
						LogicalIP:  egressPodIP[0].String(),
						ExternalIP: egressIPs1[1],
						ExternalIDs: map[string]string{
							"name": pas.egressIPName,
						},
						Type:        nbdb.NATTypeSNAT,
						LogicalPort: utilpointer.String("k8s-node2"),
						Options: map[string]string{
							"stateless": "false",
						},
					}
					podReRoutePolicy.Nexthops = []string{nodeLogicalRouterIPv4[0], node2LogicalRouterIPv4[0]}
					if node2Zone == "remote" {
						// the policy reroute will have its second nexthop as transit switchIP
						// so the one with join switchIP is where podNode == egressNode and one with transitIP is where podNode != egressNode
						podReRoutePolicy.Nexthops = []string{"100.64.0.2", "168.254.0.3"}
					}
					if node1Zone == "remote" {
						finalDatabaseStatewithPod = append(finalDatabaseStatewithPod, getReRouteStaticRoute(egressPodIP[0].String(), node2LogicalRouterIPv4[0]))
						finalDatabaseStatewithPod[2].(*nbdb.LogicalRouter).StaticRoutes = []string{"reroute-static-route-UUID"}
					}
					if !interconnect || node2Zone == "global" {
						node2GR.Nat = []string{"egressip-nat-UUID2"}
						finalDatabaseStatewithPod = append(finalDatabaseStatewithPod, podEIPSNAT2)
					}

					gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(finalDatabaseStatewithPod))

					// check the state of the cache for podKey
					pas = getPodAssignmentState(&egressPod1)
					gomega.Expect(pas).NotTo(gomega.BeNil())

					gomega.Expect(pas.egressIPName).To(gomega.Equal(egressIPName))
					eip1Obj, err = fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Get(context.TODO(), eIP1.Name, metav1.GetOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					gomega.Expect(pas.egressStatuses[eip1Obj.Status.Items[0]]).To(gomega.Equal(""))
					gomega.Expect(pas.egressStatuses[eip1Obj.Status.Items[1]]).To(gomega.Equal(""))
					gomega.Expect(pas.standbyEgressIPNames.Has(egressIPName2)).To(gomega.BeTrue())

					// let's test syncPodAssignmentCache works as expected! Nuke the podAssignment cache first
					fakeOvn.controller.eIPC.podAssignmentMutex.Lock()
					fakeOvn.controller.eIPC.podAssignment = make(map[string]*podAssignmentState) // replicates controller startup state
					fakeOvn.controller.eIPC.podAssignmentMutex.Unlock()

					egressIPCache, err := fakeOvn.controller.generateCacheForEgressIP()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.syncPodAssignmentCache(egressIPCache)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					pas = getPodAssignmentState(&egressPod1)
					gomega.Expect(pas).NotTo(gomega.BeNil())
					gomega.Expect(pas.egressIPName).To(gomega.Equal(egressIPName))
					gomega.Expect(pas.egressStatuses).To(gomega.Equal(map[egressipv1.EgressIPStatusItem]string{}))
					gomega.Expect(pas.standbyEgressIPNames.Has(egressIPName2)).To(gomega.BeTrue())

					// reset egressStatuses for rest of the test to progress correctly
					fakeOvn.controller.eIPC.podAssignmentMutex.Lock()
					fakeOvn.controller.eIPC.podAssignment[getPodKey(&egressPod1)].egressStatuses[eip1Obj.Status.Items[0]] = ""
					fakeOvn.controller.eIPC.podAssignment[getPodKey(&egressPod1)].egressStatuses[eip1Obj.Status.Items[1]] = ""
					fakeOvn.controller.eIPC.podAssignmentMutex.Unlock()

					// delete the standby egressIP object to make sure the cache is updated
					err = fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Delete(context.TODO(), egressIPName2, metav1.DeleteOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					gomega.Eventually(func() bool {
						pas := getPodAssignmentState(&egressPod1)
						gomega.Expect(pas).NotTo(gomega.BeNil())
						return pas.standbyEgressIPNames.Has(egressIPName2)
					}).Should(gomega.BeFalse())
					gomega.Expect(getPodAssignmentState(&egressPod1).egressIPName).To(gomega.Equal(egressIPName))

					// add back the standby egressIP object
					_, err = fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Create(context.TODO(), &eIP2, metav1.CreateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					// NOTE: Cluster manager is the one who patches the egressIP object.
					// For the sake of unit testing egressip zone controller we need to patch egressIP object manually
					// There are tests in cluster-manager package covering the patch logic.
					status = []egressipv1.EgressIPStatusItem{
						{
							Node:     node1Name,
							EgressIP: egressIP3,
						},
					}
					err = fakeOvn.controller.patchReplaceEgressIPStatus(egressIPName2, status)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					gomega.Eventually(func() bool {
						pas := getPodAssignmentState(&egressPod1)
						gomega.Expect(pas).NotTo(gomega.BeNil())
						return pas.standbyEgressIPNames.Has(egressIPName2)
					}).Should(gomega.BeTrue())
					gomega.Expect(getPodAssignmentState(&egressPod1).egressIPName).To(gomega.Equal(egressIPName))
					gomega.Eventually(func() string {
						return <-fakeOvn.fakeRecorder.Events
					}).Should(gomega.ContainSubstring("EgressIP object egressip-2 will not be configured for pod egressip-namespace_egress-pod since another egressIP object egressip is serving it, this is undefined"))

					gomega.Eventually(getEgressIPStatusLen(egressIPName2)).Should(gomega.Equal(1))
					egressIPs2, nodes2 = getEgressIPStatus(egressIPName2)
					gomega.Expect(egressIPs2[0]).To(gomega.Equal(egressIP3))
					assginedNodeForEIPObj2 := nodes2[0]

					// Delete the IP from object1 that was on node1 and ensure standby is not taking over
					eIPUpdate, err := fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Get(context.TODO(), eIP1.Name, metav1.GetOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					ipOnNode1 := assginedEIP
					var ipOnNode2 string
					if ipOnNode1 == egressIP1 {
						ipOnNode2 = egressIP2
					} else {
						ipOnNode2 = egressIP1
					}
					eIPUpdate.Spec.EgressIPs = []string{ipOnNode2}
					_, err = fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Update(context.TODO(), eIPUpdate, metav1.UpdateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					fakeOvn.patchEgressIPObj(node2Name, ipOnNode2)

					gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
					egressIPs1, nodes1 = getEgressIPStatus(egressIPName)
					gomega.Expect(nodes1[0]).To(gomega.Equal(node2.Name))
					gomega.Expect(egressIPs1[0]).To(gomega.Equal(ipOnNode2))

					// check if the setup for firstIP from object1 is deleted properly
					podReRoutePolicy.Nexthops = node2LogicalRouterIPv4
					if node2Zone == "remote" {
						// the policy reroute will have its second nexthop as transit switchIP
						// so the one with join switchIP is where podNode == egressNode and one with transitIP is where podNode != egressNode
						podReRoutePolicy.Nexthops = []string{"168.254.0.3"}
					}
					podNodeSNAT := &nbdb.NAT{
						UUID:       "node-nat-UUID1",
						LogicalIP:  egressPodIP[0].String(),
						ExternalIP: "192.168.126.12", // adds back SNAT to nodeIP
						Type:       nbdb.NATTypeSNAT,
						Options: map[string]string{
							"stateless": "false",
						},
					}
					if node1Zone != "remote" {
						node1GR.Nat = []string{podNodeSNAT.UUID}
						finalDatabaseStatewithPod = append(finalDatabaseStatewithPod, podNodeSNAT)
						gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(finalDatabaseStatewithPod[1:]))
					} else {
						gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(finalDatabaseStatewithPod))
					}

					gomega.Eventually(func() bool {
						pas := getPodAssignmentState(&egressPod1)
						gomega.Expect(pas).NotTo(gomega.BeNil())
						return pas.standbyEgressIPNames.Has(egressIPName2)
					}).Should(gomega.BeTrue())
					gomega.Expect(getPodAssignmentState(&egressPod1).egressIPName).To(gomega.Equal(egressIPName))

					// delete the first egressIP object and make sure the cache is updated
					err = fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Delete(context.TODO(), egressIPName, metav1.DeleteOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					// ensure standby takes over and we do the setup for it in OVN DB
					gomega.Eventually(func() bool {
						pas := getPodAssignmentState(&egressPod1)
						gomega.Expect(pas).NotTo(gomega.BeNil())
						return pas.standbyEgressIPNames.Has(egressIPName2)
					}).Should(gomega.BeFalse())
					gomega.Expect(getPodAssignmentState(&egressPod1).egressIPName).To(gomega.Equal(egressIPName2))

					finalDatabaseStatewithPod = expectedDatabaseStatewithPod
					finalDatabaseStatewithPod = append(expectedDatabaseStatewithPod, podLSP)
					podEIPSNAT.ExternalIP = egressIP3
					podEIPSNAT.ExternalIDs = map[string]string{
						"name": egressIPName2,
					}
					podReRoutePolicy.ExternalIDs = map[string]string{
						"name": egressIPName2,
					}
					if assginedNodeForEIPObj2 == node2.Name {
						podEIPSNAT.LogicalPort = utilpointer.String("k8s-node2")
						finalDatabaseStatewithPod = append(finalDatabaseStatewithPod, podNodeSNAT)
						node1GR.Nat = []string{podNodeSNAT.UUID}
						node2GR.Nat = []string{podEIPSNAT.UUID}
					}
					if assginedNodeForEIPObj2 == node1.Name {
						podReRoutePolicy.Nexthops = nodeLogicalRouterIPv4
						node1GR.Nat = []string{podEIPSNAT.UUID}
						node2GR.Nat = []string{}
					}
					if node1Zone == "remote" {
						// policy is not visible since podNode is in remote zone
						finalDatabaseStatewithPod[4].(*nbdb.LogicalRouter).Policies = []string{"no-reroute-UUID", "no-reroute-service-UUID"}
						finalDatabaseStatewithPod = finalDatabaseStatewithPod[2:]
						podEIPSNAT.ExternalIP = "192.168.126.12" // EIP SNAT is not visible since podNode is remote, SNAT towards nodeIP is visible.
						podEIPSNAT.LogicalPort = nil
						finalDatabaseStatewithPod = append(finalDatabaseStatewithPod, podNodeSNAT)
						node1GR.Nat = []string{"node-nat-UUID1"}
						finalDatabaseStatewithPod[2].(*nbdb.LogicalRouter).StaticRoutes = []string{}
					}
					gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(finalDatabaseStatewithPod))

					// delete the second egressIP object to make sure the cache is updated podKey should be gone since nothing is managing it anymore
					err = fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Delete(context.TODO(), egressIPName2, metav1.DeleteOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					gomega.Eventually(func() bool {
						return getPodAssignmentState(&egressPod1) != nil
					}).Should(gomega.BeFalse())

					// let's test syncPodAssignmentCache works as expected! Nuke the podAssignment cache first
					fakeOvn.controller.eIPC.podAssignmentMutex.Lock()
					fakeOvn.controller.eIPC.podAssignment = make(map[string]*podAssignmentState) // replicates controller startup state
					fakeOvn.controller.eIPC.podAssignmentMutex.Unlock()

					egressIPCache, err = fakeOvn.controller.generateCacheForEgressIP()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.syncPodAssignmentCache(egressIPCache)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					// we don't have any egressIPs, so cache is nil
					gomega.Eventually(func() bool {
						return getPodAssignmentState(&egressPod1) != nil
					}).Should(gomega.BeFalse())

					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			},
			ginkgotable.Entry("interconnect disabled; non-ic - single zone setup", false, "global", "global"),
			ginkgotable.Entry("interconnect enabled; node1 and node2 in global zones", true, "global", "global"),
			// will showcase localzone setup - master is in pod's zone where pod's reroute policy towards egressNode will be done.
			// NOTE: SNAT won't be visible because its in remote zone
			ginkgotable.Entry("interconnect enabled; node1 in global and node2 in remote zones", true, "global", "remote"),
			// will showcase localzone setup - master is in egress node's zone where pod's SNAT policy and static route will be done.
			// NOTE: reroute policy won't be visible because its in remote zone (pod is in remote zone)
			ginkgotable.Entry("interconnect enabled; node1 in remote and node2 in global zones", true, "remote", "global"),
		)

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
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":[\"%s\", \"%s\"]}", v4NodeSubnet, v6NodeSubnet),
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

				fakeOvn.startWithDBSetup(
					libovsdbtest.TestSetup{
						NBData: []libovsdbtest.TestData{
							&nbdb.LogicalRouter{
								Name: ovntypes.OVNClusterRouter,
								UUID: ovntypes.OVNClusterRouter + "-UUID",
							},
							&nbdb.LogicalRouter{
								Name: ovntypes.GWRouterPrefix + node.Name,
								UUID: ovntypes.GWRouterPrefix + node.Name + "-UUID",
							},
							&nbdb.LogicalSwitchPort{
								UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node.Name + "UUID",
								Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node.Name,
								Type: "router",
								Options: map[string]string{
									"router-port": types.GWRouterToExtSwitchPrefix + "GR_" + node.Name,
								},
							},
						},
					},
					&egressipv1.EgressIPList{
						Items: []egressipv1.EgressIP{eIP},
					},
					&v1.NodeList{
						Items: []v1.Node{node},
					})

				err := fakeOvn.controller.WatchEgressIPNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIPPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressNodes()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectedDatabaseState := []libovsdbtest.TestData{
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match:    "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
						Action:   nbdb.LogicalRouterPolicyActionAllow,
						UUID:     "no-reroute-UUID",
					},
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match:    fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
						Action:   nbdb.LogicalRouterPolicyActionAllow,
						UUID:     "no-reroute-service-UUID",
					},
					&nbdb.LogicalRouter{
						Name:     ovntypes.OVNClusterRouter,
						UUID:     ovntypes.OVNClusterRouter + "-UUID",
						Policies: []string{"no-reroute-UUID", "no-reroute-service-UUID"},
					},
					&nbdb.LogicalRouter{
						Name: ovntypes.GWRouterPrefix + node.Name,
						UUID: ovntypes.GWRouterPrefix + node.Name + "-UUID",
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node.Name + "UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node.Name,
						Type: "router",
						Options: map[string]string{
							"router-port":               types.GWRouterToExtSwitchPrefix + "GR_" + node.Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
				}
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				gomega.Eventually(eIP.Status.Items).Should(gomega.HaveLen(0))

				node.Labels = map[string]string{
					"k8s.ovn.org/egress-assignable": "",
				}

				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), &node, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				fakeOvn.patchEgressIPObj(node1Name, egressIP)

				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
				egressIPs, nodes := getEgressIPStatus(egressIPName)
				gomega.Expect(nodes[0]).To(gomega.Equal(node.Name))
				gomega.Expect(egressIPs[0]).To(gomega.Equal(egressIP))

				gomega.Eventually(getEgressIPReassignmentCount).Should(gomega.Equal(0))
				expectedDatabaseState = []libovsdbtest.TestData{
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match:    "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
						Action:   nbdb.LogicalRouterPolicyActionAllow,
						UUID:     "no-reroute-UUID",
					},
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match:    fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
						Action:   nbdb.LogicalRouterPolicyActionAllow,
						UUID:     "no-reroute-service-UUID",
					},
					&nbdb.LogicalRouter{
						Name:     ovntypes.OVNClusterRouter,
						UUID:     ovntypes.OVNClusterRouter + "-UUID",
						Policies: []string{"no-reroute-UUID", "no-reroute-service-UUID"},
					},
					&nbdb.LogicalRouter{
						Name: ovntypes.GWRouterPrefix + node.Name,
						UUID: ovntypes.GWRouterPrefix + node.Name + "-UUID",
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node.Name + "UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node.Name,
						Type: "router",
						Options: map[string]string{
							"router-port":               types.GWRouterToExtSwitchPrefix + "GR_" + node.Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
				}
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
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
				annotations := map[string]string{
					"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4, node1IPv6),
					"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":[\"%s\", \"%s\"]}", v4NodeSubnet, v6NodeSubnet),
				}
				labels := map[string]string{
					"k8s.ovn.org/egress-assignable": "",
				}
				node1 := getNodeObj(node1Name, annotations, labels)
				annotations = map[string]string{
					"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node2IPv4, ""),
					"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":\"%s\"}", v4NodeSubnet),
				}
				node2 := getNodeObj(node2Name, annotations, labels)

				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{egressIP},
					},
					Status: egressipv1.EgressIPStatus{
						Items: []egressipv1.EgressIPStatusItem{},
					},
				}

				fakeOvn.startWithDBSetup(
					libovsdbtest.TestSetup{
						NBData: []libovsdbtest.TestData{
							&nbdb.LogicalRouter{
								Name: ovntypes.OVNClusterRouter,
								UUID: ovntypes.OVNClusterRouter + "-UUID",
							},
							&nbdb.LogicalRouter{
								Name: ovntypes.GWRouterPrefix + node1.Name,
								UUID: ovntypes.GWRouterPrefix + node1.Name + "-UUID",
							},
							&nbdb.LogicalRouter{
								Name: ovntypes.GWRouterPrefix + node2.Name,
								UUID: ovntypes.GWRouterPrefix + node2.Name + "-UUID",
							},
							&nbdb.LogicalSwitchPort{
								UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "UUID",
								Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
								Type: "router",
								Options: map[string]string{
									"router-port": types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
								},
							},
							&nbdb.LogicalSwitchPort{
								UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "UUID",
								Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
								Type: "router",
								Options: map[string]string{
									"router-port": types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
								},
							},
						},
					},
					&egressipv1.EgressIPList{
						Items: []egressipv1.EgressIP{eIP},
					},
					&v1.NodeList{
						Items: []v1.Node{node1, node2},
					})

				err := fakeOvn.controller.WatchEgressIPNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIPPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressNodes()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectedDatabaseState := []libovsdbtest.TestData{
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match:    "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
						Action:   nbdb.LogicalRouterPolicyActionAllow,
						UUID:     "no-reroute-UUID",
					},
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match:    fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
						Action:   nbdb.LogicalRouterPolicyActionAllow,
						UUID:     "no-reroute-service-UUID",
					},
					&nbdb.LogicalRouter{
						Name:     ovntypes.OVNClusterRouter,
						UUID:     ovntypes.OVNClusterRouter + "-UUID",
						Policies: []string{"no-reroute-UUID", "no-reroute-service-UUID"},
					},
					&nbdb.LogicalRouter{
						Name: ovntypes.GWRouterPrefix + node1.Name,
						UUID: ovntypes.GWRouterPrefix + node1.Name + "-UUID",
					},
					&nbdb.LogicalRouter{
						Name: ovntypes.GWRouterPrefix + node2.Name,
						UUID: ovntypes.GWRouterPrefix + node2.Name + "-UUID",
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
						Type: "router",
						Options: map[string]string{
							"router-port":               types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
						Type: "router",
						Options: map[string]string{
							"router-port":               types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
				}
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
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

				annotations := map[string]string{
					"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4, ""),
					"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":\"%s\"}", v4NodeSubnet),
				}
				labels := map[string]string{
					"k8s.ovn.org/egress-assignable": "",
				}
				node1 := getNodeObj(node1Name, annotations, labels)
				annotations = map[string]string{
					"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node2IPv4, ""),
					"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":\"%s\"}", v4NodeSubnet),
				}
				labels = map[string]string{}
				node2 := getNodeObj(node2Name, annotations, labels)

				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{egressIP1, egressIP2},
					},
					Status: egressipv1.EgressIPStatus{
						Items: []egressipv1.EgressIPStatusItem{},
					},
				}

				fakeOvn.startWithDBSetup(
					libovsdbtest.TestSetup{
						NBData: []libovsdbtest.TestData{
							&nbdb.LogicalRouter{
								Name: ovntypes.OVNClusterRouter,
								UUID: ovntypes.OVNClusterRouter + "-UUID",
							},
							&nbdb.LogicalRouter{
								Name: ovntypes.GWRouterPrefix + node1.Name,
								UUID: ovntypes.GWRouterPrefix + node1.Name + "-UUID",
							},
							&nbdb.LogicalRouter{
								Name: ovntypes.GWRouterPrefix + node2.Name,
								UUID: ovntypes.GWRouterPrefix + node2.Name + "-UUID",
							},
							&nbdb.LogicalSwitchPort{
								UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "UUID",
								Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
								Type: "router",
								Options: map[string]string{
									"router-port": types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
								},
							},
							&nbdb.LogicalSwitchPort{
								UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "UUID",
								Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
								Type: "router",
								Options: map[string]string{
									"router-port": types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
								},
							},
						},
					},
					&egressipv1.EgressIPList{
						Items: []egressipv1.EgressIP{eIP},
					},
					&v1.NodeList{
						Items: []v1.Node{node1, node2},
					})

				err := fakeOvn.controller.WatchEgressIPNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIPPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressNodes()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectedDatabaseState := []libovsdbtest.TestData{
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match:    "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
						Action:   nbdb.LogicalRouterPolicyActionAllow,
						UUID:     "no-reroute-UUID",
					},
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match:    fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
						Action:   nbdb.LogicalRouterPolicyActionAllow,
						UUID:     "no-reroute-service-UUID",
					},
					&nbdb.LogicalRouter{
						Name:     ovntypes.OVNClusterRouter,
						UUID:     ovntypes.OVNClusterRouter + "-UUID",
						Policies: []string{"no-reroute-UUID", "no-reroute-service-UUID"},
					},
					&nbdb.LogicalRouter{
						Name: ovntypes.GWRouterPrefix + node1.Name,
						UUID: ovntypes.GWRouterPrefix + node1.Name + "-UUID",
					},
					&nbdb.LogicalRouter{
						Name: ovntypes.GWRouterPrefix + node2.Name,
						UUID: ovntypes.GWRouterPrefix + node2.Name + "-UUID",
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
						Type: "router",
						Options: map[string]string{
							"router-port":               types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
						Type: "router",
						Options: map[string]string{
							"router-port":               types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
				}
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				node2.Labels = map[string]string{
					"k8s.ovn.org/egress-assignable": "",
				}
				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), &node2, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// note: since there are no egressIP pods created in this test, we didn't need to manually patch the status.
				expectedDatabaseState = []libovsdbtest.TestData{
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match:    "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
						Action:   nbdb.LogicalRouterPolicyActionAllow,
						UUID:     "no-reroute-UUID",
					},
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match:    fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
						Action:   nbdb.LogicalRouterPolicyActionAllow,
						UUID:     "no-reroute-service-UUID",
					},
					&nbdb.LogicalRouter{
						Name:     ovntypes.OVNClusterRouter,
						UUID:     ovntypes.OVNClusterRouter + "-UUID",
						Policies: []string{"no-reroute-UUID", "no-reroute-service-UUID"},
					},
					&nbdb.LogicalRouter{
						Name: ovntypes.GWRouterPrefix + node1.Name,
						UUID: ovntypes.GWRouterPrefix + node1.Name + "-UUID",
					},
					&nbdb.LogicalRouter{
						Name: ovntypes.GWRouterPrefix + node2.Name,
						UUID: ovntypes.GWRouterPrefix + node2.Name + "-UUID",
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
						Type: "router",
						Options: map[string]string{
							"router-port":               types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
						Type: "router",
						Options: map[string]string{
							"router-port":               types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
				}
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should remove stale EgressIP setup when node label is removed while ovnkube-master is not running and assign to newly labelled node", func() {
			app.Action = func(ctx *cli.Context) error {

				egressIP1 := "192.168.126.25"
				node1IPv4 := "192.168.126.51/24"

				egressPod := *newPodWithLabels(namespace, podName, node1Name, podV4IP, egressPodLabel)
				egressNamespace := newNamespace(namespace)
				annotations := map[string]string{
					"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4, ""),
					"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":\"%s\"}", v4NodeSubnet),
				}
				labels := map[string]string{}
				node1 := getNodeObj(node1Name, annotations, labels)
				annotations = map[string]string{
					"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4, ""),
					"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":\"%s\"}", v4NodeSubnet),
				}

				labels = map[string]string{
					"k8s.ovn.org/egress-assignable": "",
				}
				node2 := getNodeObj(node2Name, annotations, labels)

				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{egressIP1},
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
						Items: []egressipv1.EgressIPStatusItem{
							{
								Node:     node1.Name,
								EgressIP: egressIP1,
							},
						},
					},
				}

				fakeOvn.startWithDBSetup(
					libovsdbtest.TestSetup{
						NBData: []libovsdbtest.TestData{
							&nbdb.LogicalRouter{
								Name: types.OVNClusterRouter,
							},
							&nbdb.LogicalRouter{
								Name: ovntypes.GWRouterPrefix + node1.Name,
								UUID: ovntypes.GWRouterPrefix + node1.Name + "-UUID",
							},
							&nbdb.LogicalRouter{
								Name: ovntypes.GWRouterPrefix + node2.Name,
								UUID: ovntypes.GWRouterPrefix + node2.Name + "-UUID",
							},
							&nbdb.LogicalRouterPort{
								UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name + "-UUID",
								Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name,
								Networks: []string{nodeLogicalRouterIfAddrV4},
							},
							&nbdb.LogicalRouterPort{
								UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2.Name + "-UUID",
								Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2.Name,
								Networks: []string{nodeLogicalRouterIfAddrV4},
							},
							&nbdb.LogicalSwitchPort{
								UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "UUID",
								Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
								Type: "router",
								Options: map[string]string{
									"router-port": types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
								},
							},
							&nbdb.LogicalSwitchPort{
								UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "UUID",
								Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
								Type: "router",
								Options: map[string]string{
									"router-port": types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
								},
							},
						},
					},
					&egressipv1.EgressIPList{
						Items: []egressipv1.EgressIP{eIP},
					},
					&v1.NodeList{
						Items: []v1.Node{node1, node2},
					},
					&v1.NamespaceList{
						Items: []v1.Namespace{*egressNamespace},
					},
					&v1.PodList{
						Items: []v1.Pod{egressPod},
					},
				)

				i, n, _ := net.ParseCIDR(podV4IP + "/23")
				n.IP = i
				fakeOvn.controller.logicalPortCache.add(&egressPod, "", types.DefaultNetworkName, "", nil, []*net.IPNet{n})

				err := fakeOvn.controller.WatchEgressIPNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIPPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressNodes()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				fakeOvn.patchEgressIPObj(node2Name, egressIP1)

				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
				gomega.Eventually(getEgressIPReassignmentCount).Should(gomega.Equal(0))
				expectedNatLogicalPort := "k8s-node2"
				expectedDatabaseState := []libovsdbtest.TestData{
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match:    "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
						Action:   nbdb.LogicalRouterPolicyActionAllow,
						UUID:     "default-no-reroute-UUID",
					},
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match:    fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
						Action:   nbdb.LogicalRouterPolicyActionAllow,
						UUID:     "no-reroute-service-UUID",
					},
					&nbdb.LogicalRouterPolicy{
						Priority: types.EgressIPReroutePriority,
						Match:    fmt.Sprintf("ip4.src == %s", egressPod.Status.PodIP),
						Action:   nbdb.LogicalRouterPolicyActionReroute,
						Nexthops: nodeLogicalRouterIPv4,
						ExternalIDs: map[string]string{
							"name": eIP.Name,
						},
						UUID: "reroute-UUID",
					},
					&nbdb.NAT{
						UUID:       "egressip-nat-UUID",
						LogicalIP:  podV4IP,
						ExternalIP: egressIP1,
						ExternalIDs: map[string]string{
							"name": egressIPName,
						},
						Type:        nbdb.NATTypeSNAT,
						LogicalPort: &expectedNatLogicalPort,
						Options: map[string]string{
							"stateless": "false",
						},
					},
					&nbdb.LogicalRouter{
						Name:     ovntypes.OVNClusterRouter,
						UUID:     ovntypes.OVNClusterRouter + "-UUID",
						Policies: []string{"reroute-UUID", "default-no-reroute-UUID", "no-reroute-service-UUID"},
					},
					&nbdb.LogicalRouter{
						Name: ovntypes.GWRouterPrefix + node1.Name,
						UUID: ovntypes.GWRouterPrefix + node1.Name + "-UUID",
					},
					&nbdb.LogicalRouter{
						Name: ovntypes.GWRouterPrefix + node2.Name,
						UUID: ovntypes.GWRouterPrefix + node2.Name + "-UUID",
						Nat:  []string{"egressip-nat-UUID"},
					},
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2.Name + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2.Name,
						Networks: []string{nodeLogicalRouterIfAddrV4},
					},
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name,
						Networks: []string{nodeLogicalRouterIfAddrV4},
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
						Type: "router",
						Options: map[string]string{
							"router-port":               types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
						Type: "router",
						Options: map[string]string{
							"router-port":               types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
				}

				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should remove stale EgressIP setup when pod is deleted while ovnkube-master is not running", func() {
			app.Action = func(ctx *cli.Context) error {

				egressIP1 := "192.168.126.25"
				node1IPv4 := "192.168.126.51/24"

				egressNamespace := newNamespace(namespace)

				annotations := map[string]string{
					"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4, ""),
					"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":\"%s\"}", v4NodeSubnet),
				}
				labels := map[string]string{
					"k8s.ovn.org/egress-assignable": "",
				}
				node1 := getNodeObj(node1Name, annotations, labels)

				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{egressIP1},
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
						Items: []egressipv1.EgressIPStatusItem{
							{
								Node:     node1.Name,
								EgressIP: egressIP1,
							},
						},
					},
				}

				expectedNatLogicalPort := "k8s-node1"
				fakeOvn.startWithDBSetup(
					libovsdbtest.TestSetup{
						NBData: []libovsdbtest.TestData{
							&nbdb.LogicalRouterPolicy{
								UUID:     "keep-me-UUID",
								Match:    "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
								Priority: types.DefaultNoRereoutePriority,
								Action:   nbdb.LogicalRouterPolicyActionAllow,
							},
							&nbdb.LogicalRouterPolicy{
								UUID: "remove-me-UUID",
								ExternalIDs: map[string]string{
									"name": eIP.Name,
								},
								Match:    "ip.src == 10.128.3.8",
								Priority: types.EgressIPReroutePriority,
								Action:   nbdb.LogicalRouterPolicyActionReroute,
							},
							&nbdb.LogicalRouter{
								Name:     ovntypes.OVNClusterRouter,
								UUID:     ovntypes.OVNClusterRouter + "-UUID",
								Policies: []string{"remove-me-UUID", "keep-me-UUID"},
							},
							&nbdb.LogicalRouter{
								Name: ovntypes.GWRouterPrefix + node1.Name,
								UUID: ovntypes.GWRouterPrefix + node1.Name + "-UUID",
								Nat:  []string{"egressip-nat-UUID"},
							},
							&nbdb.NAT{
								UUID:       "egressip-nat-UUID",
								LogicalIP:  podV4IP,
								ExternalIP: egressIP1,
								ExternalIDs: map[string]string{
									"name": egressIPName,
								},
								Type:        nbdb.NATTypeSNAT,
								LogicalPort: &expectedNatLogicalPort,
								Options: map[string]string{
									"stateless": "false",
								},
							},
							&nbdb.LogicalSwitchPort{
								UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "UUID",
								Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
								Type: "router",
								Options: map[string]string{
									"router-port": types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
								},
							},
						},
					},
					&egressipv1.EgressIPList{
						Items: []egressipv1.EgressIP{eIP},
					},
					&v1.NodeList{
						Items: []v1.Node{node1},
					},
					&v1.NamespaceList{
						Items: []v1.Namespace{*egressNamespace},
					},
				)

				err := fakeOvn.controller.WatchEgressIPNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIPPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressNodes()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
				gomega.Eventually(getEgressIPReassignmentCount).Should(gomega.Equal(0))
				expectedDatabaseState := []libovsdbtest.TestData{
					&nbdb.LogicalRouterPolicy{
						UUID:     "keep-me-UUID",
						Match:    "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
						Priority: types.DefaultNoRereoutePriority,
						Action:   nbdb.LogicalRouterPolicyActionAllow,
					},
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match:    fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
						Action:   nbdb.LogicalRouterPolicyActionAllow,
						UUID:     "no-reroute-service-UUID",
					},
					&nbdb.LogicalRouter{
						Name:     ovntypes.OVNClusterRouter,
						UUID:     ovntypes.OVNClusterRouter + "-UUID",
						Policies: []string{"keep-me-UUID", "no-reroute-service-UUID"},
					},
					&nbdb.LogicalRouter{
						Name: ovntypes.GWRouterPrefix + node1.Name,
						UUID: ovntypes.GWRouterPrefix + node1.Name + "-UUID",
						Nat:  []string{"egressip-nat-UUID"},
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
						Type: "router",
						Options: map[string]string{
							"router-port":               types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
				}
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should remove stale pod SNAT referring to wrong logical port after ovnkube-master is started", func() {
			app.Action = func(ctx *cli.Context) error {
				config.Gateway.DisableSNATMultipleGWs = true
				egressIP := "192.168.126.25"
				node1IPv4 := "192.168.126.12/24"

				egressPod := *newPodWithLabels(namespace, podName, node1Name, podV4IP, egressPodLabel)
				egressNamespace := newNamespace(namespace)

				annotations := map[string]string{
					"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\"}", node1IPv4),
					"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":\"%s\"}", v4NodeSubnet),
					"k8s.ovn.org/l3-gateway-config":   `{"default":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"192.168.126.12/24", "next-hop":"192.168.126.1"}}`,
					"k8s.ovn.org/node-chassis-id":     "79fdcfc4-6fe6-4cd3-8242-c0f85a4668ec",
				}
				labels := map[string]string{
					"k8s.ovn.org/egress-assignable": "",
				}
				node1 := getNodeObj(node1Name, annotations, labels)

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

				node1Switch := &nbdb.LogicalSwitch{
					UUID: node1.Name + "-UUID",
					Name: node1.Name,
				}
				node1GR := &nbdb.LogicalRouter{
					Name: ovntypes.GWRouterPrefix + node1.Name,
					UUID: ovntypes.GWRouterPrefix + node1.Name + "-UUID",
				}
				node1LSP := &nbdb.LogicalSwitchPort{
					UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "UUID",
					Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
					Type: "router",
					Options: map[string]string{
						"router-port": types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
					},
				}
				fakeOvn.startWithDBSetup(
					libovsdbtest.TestSetup{
						NBData: []libovsdbtest.TestData{
							&nbdb.LogicalRouter{
								Name: ovntypes.OVNClusterRouter,
								UUID: ovntypes.OVNClusterRouter + "-UUID",
							},
							node1GR,
							node1LSP,
							&nbdb.LogicalRouterPort{
								UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name + "-UUID",
								Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name,
								Networks: []string{"100.64.0.2/29"},
							},
							node1Switch,
							// This is unexpected snat entry where its logical port refers to an unavailable node
							// and ensure this entry is removed as soon as ovnk master is up and running.
							&nbdb.NAT{
								UUID:       "egressip-nat-UUID2",
								LogicalIP:  podV4IP,
								ExternalIP: egressIP,
								ExternalIDs: map[string]string{
									"name": egressIPName,
								},
								Type:        nbdb.NATTypeSNAT,
								LogicalPort: utilpointer.String("k8s-node2"),
								Options: map[string]string{
									"stateless": "false",
								},
							},
						},
					},
					&egressipv1.EgressIPList{
						Items: []egressipv1.EgressIP{eIP},
					},
					&v1.NodeList{
						Items: []v1.Node{node1},
					},
					&v1.NamespaceList{
						Items: []v1.Namespace{*egressNamespace},
					},
					&v1.PodList{
						Items: []v1.Pod{egressPod},
					},
				)

				i, n, _ := net.ParseCIDR(podV4IP + "/23")
				n.IP = i
				fakeOvn.controller.logicalPortCache.add(&egressPod, "", types.DefaultNetworkName, "", nil, []*net.IPNet{n})

				err := fakeOvn.controller.WatchPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIPNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIPPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressNodes()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				egressPodPortInfo, err := fakeOvn.controller.logicalPortCache.get(&egressPod, types.DefaultNetworkName)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				ePod, err := fakeOvn.fakeClient.KubeClient.CoreV1().Pods(egressPod.Namespace).Get(context.TODO(), egressPod.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				egressPodIP, err := util.GetPodIPsOfNetwork(ePod, &util.DefaultNetInfo{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				egressNetPodIP, _, err := net.ParseCIDR(egressPodPortInfo.ips[0].String())
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(egressNetPodIP.String()).To(gomega.Equal(egressPodIP[0].String()))
				gomega.Expect(egressPodPortInfo.expires.IsZero()).To(gomega.BeTrue())

				fakeOvn.patchEgressIPObj(node1Name, egressIP)
				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
				gomega.Eventually(getEgressIPReassignmentCount).Should(gomega.Equal(0))
				egressIPs, nodes := getEgressIPStatus(egressIPName)
				gomega.Expect(nodes[0]).To(gomega.Equal(node1.Name))
				gomega.Expect(egressIPs[0]).To(gomega.Equal(egressIP))

				podEIPSNAT := getEIPSNAT(podV4IP, egressIP, "k8s-node1")
				podReRoutePolicy := getReRoutePolicy(egressPodIP[0].String(), "4", nodeLogicalRouterIPv4)
				node1GR.Nat = []string{"egressip-nat-UUID"}
				node1LSP.Options = map[string]string{
					"router-port":               types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
					"nat-addresses":             "router",
					"exclude-lb-vips-from-garp": "true",
				}
				expectedDatabaseStatewithPod := []libovsdbtest.TestData{
					podEIPSNAT, &nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match:    "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
						Action:   nbdb.LogicalRouterPolicyActionAllow,
						UUID:     "no-reroute-UUID",
					}, &nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match:    fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
						Action:   nbdb.LogicalRouterPolicyActionAllow,
						UUID:     "no-reroute-service-UUID",
					}, podReRoutePolicy, &nbdb.LogicalRouter{
						Name:     ovntypes.OVNClusterRouter,
						UUID:     ovntypes.OVNClusterRouter + "-UUID",
						Policies: []string{"no-reroute-UUID", "no-reroute-service-UUID", "reroute-UUID"},
					}, node1GR, node1LSP,
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name,
						Networks: []string{"100.64.0.2/29"},
					}, node1Switch}

				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseStatewithPod))
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

				annotations := map[string]string{
					"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4, node1IPv6),
					"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":[\"%s\", \"%s\"]}", v4NodeSubnet, v6NodeSubnet),
				}
				node1 := getNodeObj(node1Name, annotations, map[string]string{})
				annotations = map[string]string{
					"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node2IPv4, ""),
					"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":\"%s\"}", v4NodeSubnet),
				}
				node2 := getNodeObj(node2Name, annotations, map[string]string{})

				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{egressIP},
					},
					Status: egressipv1.EgressIPStatus{
						Items: []egressipv1.EgressIPStatusItem{},
					},
				}

				fakeOvn.startWithDBSetup(
					libovsdbtest.TestSetup{
						NBData: []libovsdbtest.TestData{
							&nbdb.LogicalRouter{
								Name: ovntypes.OVNClusterRouter,
								UUID: ovntypes.OVNClusterRouter + "-UUID",
							},
							&nbdb.LogicalRouter{
								Name: ovntypes.GWRouterPrefix + node1.Name,
								UUID: ovntypes.GWRouterPrefix + node1.Name + "-UUID",
							},
							&nbdb.LogicalRouter{
								Name: ovntypes.GWRouterPrefix + node2.Name,
								UUID: ovntypes.GWRouterPrefix + node2.Name + "-UUID",
							},
							&nbdb.LogicalSwitchPort{
								UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "UUID",
								Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
								Type: "router",
								Options: map[string]string{
									"router-port": types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
								},
							},
							&nbdb.LogicalSwitchPort{
								UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "UUID",
								Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
								Type: "router",
								Options: map[string]string{
									"router-port": types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
								},
							},
						},
					},
					&egressipv1.EgressIPList{
						Items: []egressipv1.EgressIP{eIP},
					},
					&v1.NodeList{
						Items: []v1.Node{node1, node2},
					})

				err := fakeOvn.controller.WatchEgressIPNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIPPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressNodes()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectedDatabaseState := []libovsdbtest.TestData{
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match:    "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
						Action:   nbdb.LogicalRouterPolicyActionAllow,
						UUID:     "no-reroute-UUID",
					},
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match:    fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
						Action:   nbdb.LogicalRouterPolicyActionAllow,
						UUID:     "no-reroute-service-UUID",
					},
					&nbdb.LogicalRouter{
						Name:     ovntypes.OVNClusterRouter,
						UUID:     ovntypes.OVNClusterRouter + "-UUID",
						Policies: []string{"no-reroute-UUID", "no-reroute-service-UUID"},
					},
					&nbdb.LogicalRouter{
						Name: ovntypes.GWRouterPrefix + node1.Name,
						UUID: ovntypes.GWRouterPrefix + node1.Name + "-UUID",
					},
					&nbdb.LogicalRouter{
						Name: ovntypes.GWRouterPrefix + node2.Name,
						UUID: ovntypes.GWRouterPrefix + node2.Name + "-UUID",
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
						Type: "router",
						Options: map[string]string{
							"router-port":               types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
						Type: "router",
						Options: map[string]string{
							"router-port":               types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
				}
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
				gomega.Eventually(eIP.Status.Items).Should(gomega.HaveLen(0))

				node1.Labels = map[string]string{
					"k8s.ovn.org/egress-assignable": "",
				}

				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), &node1, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				expectedDatabaseState = []libovsdbtest.TestData{
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match:    "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
						Action:   nbdb.LogicalRouterPolicyActionAllow,
						UUID:     "no-reroute-UUID",
					},
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match:    fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
						Action:   nbdb.LogicalRouterPolicyActionAllow,
						UUID:     "no-reroute-service-UUID",
					},
					&nbdb.LogicalRouter{
						Name:     ovntypes.OVNClusterRouter,
						UUID:     ovntypes.OVNClusterRouter + "-UUID",
						Policies: []string{"no-reroute-UUID", "no-reroute-service-UUID"},
					},
					&nbdb.LogicalRouter{
						Name: ovntypes.GWRouterPrefix + node1.Name,
						UUID: ovntypes.GWRouterPrefix + node1.Name + "-UUID",
					},
					&nbdb.LogicalRouter{
						Name: ovntypes.GWRouterPrefix + node2.Name,
						UUID: ovntypes.GWRouterPrefix + node2.Name + "-UUID",
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
						Type: "router",
						Options: map[string]string{
							"router-port":               types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
						Type: "router",
						Options: map[string]string{
							"router-port":               types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
				}
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(0))
				gomega.Eventually(getEgressIPReassignmentCount).Should(gomega.Equal(1))

				node2.Labels = map[string]string{
					"k8s.ovn.org/egress-assignable": "",
				}

				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), &node2, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				fakeOvn.patchEgressIPObj(node2Name, egressIP)
				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))

				egressIPs, nodes := getEgressIPStatus(egressIPName)
				gomega.Expect(nodes[0]).To(gomega.Equal(node2.Name))
				gomega.Expect(egressIPs[0]).To(gomega.Equal(egressIP))
				gomega.Eventually(getEgressIPReassignmentCount).Should(gomega.Equal(0))
				expectedDatabaseState = []libovsdbtest.TestData{
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match:    "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
						Action:   nbdb.LogicalRouterPolicyActionAllow,
						UUID:     "no-reroute-UUID",
					},
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match:    fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
						Action:   nbdb.LogicalRouterPolicyActionAllow,
						UUID:     "no-reroute-service-UUID",
					},
					&nbdb.LogicalRouter{
						Name:     ovntypes.OVNClusterRouter,
						UUID:     ovntypes.OVNClusterRouter + "-UUID",
						Policies: []string{"no-reroute-UUID", "no-reroute-service-UUID"},
					},
					&nbdb.LogicalRouter{
						Name: ovntypes.GWRouterPrefix + node1.Name,
						UUID: ovntypes.GWRouterPrefix + node1.Name + "-UUID",
					},
					&nbdb.LogicalRouter{
						Name: ovntypes.GWRouterPrefix + node2.Name,
						UUID: ovntypes.GWRouterPrefix + node2.Name + "-UUID",
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
						Type: "router",
						Options: map[string]string{
							"router-port":               types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
						Type: "router",
						Options: map[string]string{
							"router-port":               types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
				}
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

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

				annotations := map[string]string{
					"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4, ""),
					"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":\"%s\"}", v4NodeSubnet),
				}
				node1 := getNodeObj(node1Name, annotations, map[string]string{})
				annotations = map[string]string{
					"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node2IPv4, ""),
					"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":\"%s\"}", v4NodeSubnet),
				}
				node2 := getNodeObj(node2Name, annotations, map[string]string{})

				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{egressIP1, egressIP2},
					},
					Status: egressipv1.EgressIPStatus{
						Items: []egressipv1.EgressIPStatusItem{},
					},
				}

				fakeOvn.startWithDBSetup(
					libovsdbtest.TestSetup{
						NBData: []libovsdbtest.TestData{
							&nbdb.LogicalRouter{
								Name: ovntypes.OVNClusterRouter,
								UUID: ovntypes.OVNClusterRouter + "-UUID",
							},
							&nbdb.LogicalRouter{
								Name: ovntypes.GWRouterPrefix + node1.Name,
								UUID: ovntypes.GWRouterPrefix + node1.Name + "-UUID",
							},
							&nbdb.LogicalRouter{
								Name: ovntypes.GWRouterPrefix + node2.Name,
								UUID: ovntypes.GWRouterPrefix + node2.Name + "-UUID",
							},
							&nbdb.LogicalSwitchPort{
								UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "UUID",
								Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
								Type: "router",
								Options: map[string]string{
									"router-port": types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
								},
							},
							&nbdb.LogicalSwitchPort{
								UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "UUID",
								Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
								Type: "router",
								Options: map[string]string{
									"router-port": types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
								},
							},
						},
					},
					&egressipv1.EgressIPList{
						Items: []egressipv1.EgressIP{eIP},
					},
					&v1.NodeList{
						Items: []v1.Node{node1, node2},
					})

				err := fakeOvn.controller.WatchEgressIPNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIPPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressNodes()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectedDatabaseState := []libovsdbtest.TestData{
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match:    "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
						Action:   nbdb.LogicalRouterPolicyActionAllow,
						UUID:     "no-reroute-UUID",
					},
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match:    fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
						Action:   nbdb.LogicalRouterPolicyActionAllow,
						UUID:     "no-reroute-service-UUID",
					},
					&nbdb.LogicalRouter{
						Name:     ovntypes.OVNClusterRouter,
						UUID:     ovntypes.OVNClusterRouter + "-UUID",
						Policies: []string{"no-reroute-UUID", "no-reroute-service-UUID"},
					},
					&nbdb.LogicalRouter{
						Name: ovntypes.GWRouterPrefix + node1.Name,
						UUID: ovntypes.GWRouterPrefix + node1.Name + "-UUID",
					},
					&nbdb.LogicalRouter{
						Name: ovntypes.GWRouterPrefix + node2.Name,
						UUID: ovntypes.GWRouterPrefix + node2.Name + "-UUID",
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
						Type: "router",
						Options: map[string]string{
							"router-port":               types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
						Type: "router",
						Options: map[string]string{
							"router-port":               types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
				}
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(0))

				node1.Labels = map[string]string{
					"k8s.ovn.org/egress-assignable": "",
				}

				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), &node1, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.patchEgressIPObj(node1Name, egressIP1)
				expectedDatabaseState = []libovsdbtest.TestData{
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match:    "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
						Action:   nbdb.LogicalRouterPolicyActionAllow,
						UUID:     "no-reroute-UUID",
					},
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match:    fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
						Action:   nbdb.LogicalRouterPolicyActionAllow,
						UUID:     "no-reroute-service-UUID",
					},
					&nbdb.LogicalRouter{
						Name:     ovntypes.OVNClusterRouter,
						UUID:     ovntypes.OVNClusterRouter + "-UUID",
						Policies: []string{"no-reroute-UUID", "no-reroute-service-UUID"},
					},
					&nbdb.LogicalRouter{
						Name: ovntypes.GWRouterPrefix + node1.Name,
						UUID: ovntypes.GWRouterPrefix + node1.Name + "-UUID",
					},
					&nbdb.LogicalRouter{
						Name: ovntypes.GWRouterPrefix + node2.Name,
						UUID: ovntypes.GWRouterPrefix + node2.Name + "-UUID",
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
						Type: "router",
						Options: map[string]string{
							"router-port":               types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
						Type: "router",
						Options: map[string]string{
							"router-port":               types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
				}
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
				_, nodes := getEgressIPStatus(egressIPName)
				gomega.Expect(nodes[0]).To(gomega.Equal(node1.Name))

				gomega.Eventually(getEgressIPReassignmentCount).Should(gomega.Equal(1))

				node2.Labels = map[string]string{
					"k8s.ovn.org/egress-assignable": "",
				}

				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), &node2, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), &node1, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// NOTE: Cluster manager is the one who patches the egressIP object.
				// For the sake of unit testing egressip zone controller we need to patch egressIP object manually
				// There are tests in cluster-manager package covering the patch logic.
				status := []egressipv1.EgressIPStatusItem{
					{
						Node:     node1Name,
						EgressIP: egressIP1,
					},
					{
						Node:     node2Name,
						EgressIP: egressIP2,
					},
				}
				err = fakeOvn.controller.patchReplaceEgressIPStatus(egressIPName, status)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(2))
				gomega.Eventually(getEgressIPReassignmentCount).Should(gomega.Equal(0))

				expectedDatabaseState = []libovsdbtest.TestData{
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match:    "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
						Action:   nbdb.LogicalRouterPolicyActionAllow,
						UUID:     "no-reroute-UUID",
					},
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match:    fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
						Action:   nbdb.LogicalRouterPolicyActionAllow,
						UUID:     "no-reroute-service-UUID",
					},
					&nbdb.LogicalRouter{
						Name:     ovntypes.OVNClusterRouter,
						UUID:     ovntypes.OVNClusterRouter + "-UUID",
						Policies: []string{"no-reroute-UUID", "no-reroute-service-UUID"},
					},
					&nbdb.LogicalRouter{
						Name: ovntypes.GWRouterPrefix + node1.Name,
						UUID: ovntypes.GWRouterPrefix + node1.Name + "-UUID",
					},
					&nbdb.LogicalRouter{
						Name: ovntypes.GWRouterPrefix + node2.Name,
						UUID: ovntypes.GWRouterPrefix + node2.Name + "-UUID",
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
						Type: "router",
						Options: map[string]string{
							"router-port":               types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
						Type: "router",
						Options: map[string]string{
							"router-port":               types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
				}
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("ensure egress ip entries are not created when pod is already moved into completed state", func() {
			app.Action = func(ctx *cli.Context) error {
				config.Gateway.DisableSNATMultipleGWs = true
				egressIP := "192.168.126.25"
				node1IPv4 := "192.168.126.12/24"

				egressPod := *newPodWithLabels(namespace, podName, node1Name, podV4IP, egressPodLabel)
				egressPod.Status.Phase = v1.PodSucceeded
				egressNamespace := newNamespace(namespace)

				node1 := v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: node1Name,
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\"}", node1IPv4),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":\"%s\"}", v4NodeSubnet),
							"k8s.ovn.org/l3-gateway-config":   `{"default":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"192.168.126.12/24", "next-hop":"192.168.126.1"}}`,
							"k8s.ovn.org/node-chassis-id":     "79fdcfc4-6fe6-4cd3-8242-c0f85a4668ec",
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

				node1Switch := &nbdb.LogicalSwitch{
					UUID: node1.Name + "-UUID",
					Name: node1.Name,
				}
				node1GR := &nbdb.LogicalRouter{
					Name: ovntypes.GWRouterPrefix + node1.Name,
					UUID: ovntypes.GWRouterPrefix + node1.Name + "-UUID",
				}
				node1LSP := &nbdb.LogicalSwitchPort{
					UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "UUID",
					Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
					Type: "router",
					Options: map[string]string{
						"router-port": types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
					},
				}
				fakeOvn.startWithDBSetup(
					libovsdbtest.TestSetup{
						NBData: []libovsdbtest.TestData{
							&nbdb.LogicalRouter{
								Name: ovntypes.OVNClusterRouter,
								UUID: ovntypes.OVNClusterRouter + "-UUID",
							},
							node1GR,
							node1LSP,
							&nbdb.LogicalRouterPort{
								UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name + "-UUID",
								Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name,
								Networks: []string{"100.64.0.2/29"},
							},
							node1Switch,
						},
					},
					&egressipv1.EgressIPList{
						Items: []egressipv1.EgressIP{eIP},
					},
					&v1.NodeList{
						Items: []v1.Node{node1},
					},
					&v1.NamespaceList{
						Items: []v1.Namespace{*egressNamespace},
					},
					&v1.PodList{
						Items: []v1.Pod{egressPod},
					},
				)
				i, n, _ := net.ParseCIDR(podV4IP + "/23")
				n.IP = i
				fakeOvn.controller.logicalPortCache.add(&egressPod, "", types.DefaultNetworkName, "", nil, []*net.IPNet{n})

				err := fakeOvn.controller.WatchPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIPNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIPPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressNodes()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				fakeOvn.patchEgressIPObj(node1Name, egressIP)

				egressPodPortInfo, err := fakeOvn.controller.logicalPortCache.get(&egressPod, types.DefaultNetworkName)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				ePod, err := fakeOvn.fakeClient.KubeClient.CoreV1().Pods(egressPod.Namespace).Get(context.TODO(), egressPod.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				egressPodIP, err := util.GetPodIPsOfNetwork(ePod, &util.DefaultNetInfo{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				egressNetPodIP, _, err := net.ParseCIDR(egressPodPortInfo.ips[0].String())
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(egressNetPodIP.String()).To(gomega.Equal(egressPodIP[0].String()))

				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
				gomega.Eventually(getEgressIPReassignmentCount).Should(gomega.Equal(0))
				egressIPs, nodes := getEgressIPStatus(egressIPName)
				gomega.Expect(nodes[0]).To(gomega.Equal(node1.Name))
				gomega.Expect(egressIPs[0]).To(gomega.Equal(egressIP))

				node1LSP.Options = map[string]string{
					"router-port":               types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
					"nat-addresses":             "router",
					"exclude-lb-vips-from-garp": "true",
				}
				expectedDatabaseStatewithPod := []libovsdbtest.TestData{
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match:    "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
						Action:   nbdb.LogicalRouterPolicyActionAllow,
						UUID:     "no-reroute-UUID",
					}, &nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match:    fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
						Action:   nbdb.LogicalRouterPolicyActionAllow,
						UUID:     "no-reroute-service-UUID",
					}, &nbdb.LogicalRouter{
						Name:     ovntypes.OVNClusterRouter,
						UUID:     ovntypes.OVNClusterRouter + "-UUID",
						Policies: []string{"no-reroute-UUID", "no-reroute-service-UUID"},
					}, node1GR, node1LSP,
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name,
						Networks: []string{"100.64.0.2/29"},
					}, node1Switch}

				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseStatewithPod))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("ensure external gw pod snat entry is not created back when pod is moved into completed state", func() {
			app.Action = func(ctx *cli.Context) error {
				config.Gateway.DisableSNATMultipleGWs = true
				egressIP := "192.168.126.25"
				node1IPv4 := "192.168.126.12/24"

				egressPod := *newPodWithLabels(namespace, podName, node1Name, podV4IP, egressPodLabel)
				egressNamespace := newNamespace(namespace)

				node1 := v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: node1Name,
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\"}", node1IPv4),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":\"%s\"}", v4NodeSubnet),
							"k8s.ovn.org/l3-gateway-config":   `{"default":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"192.168.126.12/24", "next-hop":"192.168.126.1"}}`,
							"k8s.ovn.org/node-chassis-id":     "79fdcfc4-6fe6-4cd3-8242-c0f85a4668ec",
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

				node1Switch := &nbdb.LogicalSwitch{
					UUID: node1.Name + "-UUID",
					Name: node1.Name,
				}
				node1GR := &nbdb.LogicalRouter{
					Name: ovntypes.GWRouterPrefix + node1.Name,
					UUID: ovntypes.GWRouterPrefix + node1.Name + "-UUID",
				}
				node1LSP := &nbdb.LogicalSwitchPort{
					UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "UUID",
					Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
					Type: "router",
					Options: map[string]string{
						"router-port": types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
					},
				}
				fakeOvn.startWithDBSetup(
					libovsdbtest.TestSetup{
						NBData: []libovsdbtest.TestData{
							&nbdb.LogicalRouter{
								Name: ovntypes.OVNClusterRouter,
								UUID: ovntypes.OVNClusterRouter + "-UUID",
							},
							node1GR,
							node1LSP,
							&nbdb.LogicalRouterPort{
								UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name + "-UUID",
								Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name,
								Networks: []string{"100.64.0.2/29"},
							},
							node1Switch,
						},
					},
					&egressipv1.EgressIPList{
						Items: []egressipv1.EgressIP{eIP},
					},
					&v1.NodeList{
						Items: []v1.Node{node1},
					},
					&v1.NamespaceList{
						Items: []v1.Namespace{*egressNamespace},
					},
					&v1.PodList{
						Items: []v1.Pod{egressPod},
					},
				)

				i, n, _ := net.ParseCIDR(podV4IP + "/23")
				n.IP = i
				fakeOvn.controller.logicalPortCache.add(&egressPod, "", types.DefaultNetworkName, "", nil, []*net.IPNet{n})

				err := fakeOvn.controller.WatchPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIPNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIPPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressNodes()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				fakeOvn.patchEgressIPObj(node1Name, egressIP)

				egressPodPortInfo, err := fakeOvn.controller.logicalPortCache.get(&egressPod, types.DefaultNetworkName)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				ePod, err := fakeOvn.fakeClient.KubeClient.CoreV1().Pods(egressPod.Namespace).Get(context.TODO(), egressPod.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				egressPodIP, err := util.GetPodIPsOfNetwork(ePod, &util.DefaultNetInfo{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				egressNetPodIP, _, err := net.ParseCIDR(egressPodPortInfo.ips[0].String())
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(egressNetPodIP.String()).To(gomega.Equal(egressPodIP[0].String()))
				gomega.Expect(egressPodPortInfo.expires.IsZero()).To(gomega.BeTrue())

				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
				gomega.Eventually(getEgressIPReassignmentCount).Should(gomega.Equal(0))
				egressIPs, nodes := getEgressIPStatus(egressIPName)
				gomega.Expect(nodes[0]).To(gomega.Equal(node1.Name))
				gomega.Expect(egressIPs[0]).To(gomega.Equal(egressIP))

				podEIPSNAT := &nbdb.NAT{
					UUID:       "egressip-nat-UUID1",
					LogicalIP:  podV4IP,
					ExternalIP: egressIP,
					ExternalIDs: map[string]string{
						"name": egressIPName,
					},
					Type:        nbdb.NATTypeSNAT,
					LogicalPort: utilpointer.StringPtr("k8s-node1"),
					Options: map[string]string{
						"stateless": "false",
					},
				}
				podReRoutePolicy := &nbdb.LogicalRouterPolicy{
					Priority: types.EgressIPReroutePriority,
					Match:    fmt.Sprintf("ip4.src == %s", egressPodIP[0].String()),
					Action:   nbdb.LogicalRouterPolicyActionReroute,
					Nexthops: nodeLogicalRouterIPv4,
					ExternalIDs: map[string]string{
						"name": egressIPName,
					},
					UUID: "reroute-UUID1",
				}
				node1GR.Nat = []string{"egressip-nat-UUID1"}
				node1LSP.Options = map[string]string{
					"router-port":               types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
					"nat-addresses":             "router",
					"exclude-lb-vips-from-garp": "true",
				}
				expectedDatabaseStatewithPod := []libovsdbtest.TestData{
					podEIPSNAT, &nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match:    "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
						Action:   nbdb.LogicalRouterPolicyActionAllow,
						UUID:     "no-reroute-UUID",
					}, &nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match:    fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
						Action:   nbdb.LogicalRouterPolicyActionAllow,
						UUID:     "no-reroute-service-UUID",
					}, podReRoutePolicy, &nbdb.LogicalRouter{
						Name:     ovntypes.OVNClusterRouter,
						UUID:     ovntypes.OVNClusterRouter + "-UUID",
						Policies: []string{"no-reroute-UUID", "no-reroute-service-UUID", "reroute-UUID1"},
					}, node1GR, node1LSP,
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name,
						Networks: []string{"100.64.0.2/29"},
					}, node1Switch}

				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseStatewithPod))

				egressPod.Status.Phase = v1.PodSucceeded
				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(egressPod.Namespace).Update(context.TODO(), &egressPod, metav1.UpdateOptions{})
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				// Wait for pod to get moved into succeeded state.
				gomega.Eventually(func() v1.PodPhase {
					egressPod1, _ := fakeOvn.watcher.GetPod(egressPod.Namespace, egressPod.Name)
					return egressPod1.Status.Phase
				}, 5).Should(gomega.Equal(v1.PodSucceeded))

				node1GR.Nat = []string{}
				expectedDatabaseStatewitCompletedPod := []libovsdbtest.TestData{
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match:    "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
						Action:   nbdb.LogicalRouterPolicyActionAllow,
						UUID:     "no-reroute-UUID",
					}, &nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match:    fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
						Action:   nbdb.LogicalRouterPolicyActionAllow,
						UUID:     "no-reroute-service-UUID",
					}, &nbdb.LogicalRouter{
						Name:     ovntypes.OVNClusterRouter,
						UUID:     ovntypes.OVNClusterRouter + "-UUID",
						Policies: []string{"no-reroute-UUID", "no-reroute-service-UUID"},
					}, node1GR, node1LSP,
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name,
						Networks: []string{"100.64.0.2/29"},
					}, node1Switch}

				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseStatewitCompletedPod))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should ensure SNATs towards egressIP and nodeIP are correctly configured during egressIP re-assignment", func() {
			app.Action = func(ctx *cli.Context) error {
				config.Gateway.DisableSNATMultipleGWs = true

				egressIP1 := "192.168.126.101"
				egressIP2 := "192.168.126.102"
				node1IPv4 := "192.168.126.12/24"
				node2IPv4 := "192.168.126.51/24"

				egressPod1 := *newPodWithLabels(namespace, podName, node1Name, podV4IP, egressPodLabel)
				egressPod2 := *newPodWithLabels(namespace, "egress-pod2", node2Name, "10.128.0.16", egressPodLabel)
				egressNamespace := newNamespace(namespace)
				annotations := map[string]string{
					"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\"}", node1IPv4),
					"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":\"%s\"}", v4NodeSubnet),
					"k8s.ovn.org/l3-gateway-config":   `{"default":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"192.168.126.12/24", "next-hop":"192.168.126.1"}}`,
					"k8s.ovn.org/node-chassis-id":     "79fdcfc4-6fe6-4cd3-8242-c0f85a4668ec",
				}
				node1 := getNodeObj(node1Name, annotations, map[string]string{})
				annotations = map[string]string{
					"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\"}", node2IPv4),
					"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":\"%s\"}", v4NodeSubnet),
					"k8s.ovn.org/l3-gateway-config":   `{"default":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"192.168.126.51/24", "next-hop":"192.168.126.1"}}`,
					"k8s.ovn.org/node-chassis-id":     "89fdcfc4-6fe6-4cd3-8242-c0f85a4668ec",
				}
				node2 := getNodeObj(node2Name, annotations, map[string]string{})

				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{egressIP1, egressIP2},
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

				fakeOvn.startWithDBSetup(
					libovsdbtest.TestSetup{
						NBData: []libovsdbtest.TestData{
							&nbdb.LogicalRouter{
								Name: ovntypes.OVNClusterRouter,
								UUID: ovntypes.OVNClusterRouter + "-UUID",
							},
							&nbdb.LogicalRouter{
								Name: ovntypes.GWRouterPrefix + node1.Name,
								UUID: ovntypes.GWRouterPrefix + node1.Name + "-UUID",
							},
							&nbdb.LogicalRouter{
								Name: ovntypes.GWRouterPrefix + node2.Name,
								UUID: ovntypes.GWRouterPrefix + node2.Name + "-UUID",
							},
							&nbdb.LogicalRouterPort{
								UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name + "-UUID",
								Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name,
								Networks: []string{"100.64.0.2/29"},
							},
							&nbdb.LogicalRouterPort{
								UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2.Name + "-UUID",
								Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2.Name,
								Networks: []string{"100.64.0.3/29"},
							},
							&nbdb.LogicalSwitchPort{
								UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "UUID",
								Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
								Type: "router",
								Options: map[string]string{
									"router-port": types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
								},
							},
							&nbdb.LogicalSwitchPort{
								UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "UUID",
								Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
								Type: "router",
								Options: map[string]string{
									"router-port": types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
								},
							},
						},
					},
					&egressipv1.EgressIPList{
						Items: []egressipv1.EgressIP{eIP},
					},
					&v1.NodeList{
						Items: []v1.Node{node1, node2},
					},
					&v1.NamespaceList{
						Items: []v1.Namespace{*egressNamespace},
					},
					&v1.PodList{
						Items: []v1.Pod{egressPod1, egressPod2},
					},
				)

				i, n, _ := net.ParseCIDR(podV4IP + "/23")
				n.IP = i
				fakeOvn.controller.logicalPortCache.add(&egressPod1, "", types.DefaultNetworkName, "", nil, []*net.IPNet{n})
				i, n, _ = net.ParseCIDR("10.128.0.16" + "/23")
				n.IP = i
				fakeOvn.controller.logicalPortCache.add(&egressPod2, "", types.DefaultNetworkName, "", nil, []*net.IPNet{n})

				err := fakeOvn.controller.WatchEgressIPNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIPPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressNodes()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectedDatabaseState := []libovsdbtest.TestData{
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match:    "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
						Action:   nbdb.LogicalRouterPolicyActionAllow,
						UUID:     "no-reroute-UUID",
					},
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match:    fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
						Action:   nbdb.LogicalRouterPolicyActionAllow,
						UUID:     "no-reroute-service-UUID",
					},
					&nbdb.LogicalRouter{
						Name:     ovntypes.OVNClusterRouter,
						UUID:     ovntypes.OVNClusterRouter + "-UUID",
						Policies: []string{"no-reroute-UUID", "no-reroute-service-UUID"},
					},
					&nbdb.LogicalRouter{
						Name: ovntypes.GWRouterPrefix + node1.Name,
						UUID: ovntypes.GWRouterPrefix + node1.Name + "-UUID",
					},
					&nbdb.LogicalRouter{
						Name: ovntypes.GWRouterPrefix + node2.Name,
						UUID: ovntypes.GWRouterPrefix + node2.Name + "-UUID",
					},
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2.Name + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2.Name,
						Networks: []string{"100.64.0.3/29"},
					},
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name,
						Networks: []string{"100.64.0.2/29"},
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
						Type: "router",
						Options: map[string]string{
							"router-port":               types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
						Type: "router",
						Options: map[string]string{
							"router-port":               types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
				}
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(0))
				node1.Labels = map[string]string{
					"k8s.ovn.org/egress-assignable": "",
				}

				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), &node1, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				fakeOvn.patchEgressIPObj(node1Name, egressIP1)
				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
				gomega.Eventually(getEgressIPReassignmentCount).Should(gomega.Equal(1))
				eips, nodes := getEgressIPStatus(egressIPName)
				gomega.Expect(nodes[0]).To(gomega.Equal(node1.Name))

				expectedNatLogicalPort1 := "k8s-node1"
				expectedDatabaseState = []libovsdbtest.TestData{
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match:    "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
						Action:   nbdb.LogicalRouterPolicyActionAllow,
						UUID:     "no-reroute-UUID",
					},
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match:    fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
						Action:   nbdb.LogicalRouterPolicyActionAllow,
						UUID:     "no-reroute-service-UUID",
					},
					&nbdb.LogicalRouterPolicy{
						Priority: types.EgressIPReroutePriority,
						Match:    fmt.Sprintf("ip4.src == %s", egressPod1.Status.PodIP),
						Action:   nbdb.LogicalRouterPolicyActionReroute,
						Nexthops: []string{"100.64.0.2"},
						ExternalIDs: map[string]string{
							"name": eIP.Name,
						},
						UUID: "reroute-UUID1",
					},
					&nbdb.LogicalRouterPolicy{
						Priority: types.EgressIPReroutePriority,
						Match:    fmt.Sprintf("ip4.src == %s", egressPod2.Status.PodIP),
						Action:   nbdb.LogicalRouterPolicyActionReroute,
						Nexthops: []string{"100.64.0.2"},
						ExternalIDs: map[string]string{
							"name": eIP.Name,
						},
						UUID: "reroute-UUID2",
					},
					&nbdb.LogicalRouter{
						Name:     ovntypes.OVNClusterRouter,
						UUID:     ovntypes.OVNClusterRouter + "-UUID",
						Policies: []string{"no-reroute-UUID", "no-reroute-service-UUID", "reroute-UUID1", "reroute-UUID2"},
					},
					&nbdb.LogicalRouter{
						Name: ovntypes.GWRouterPrefix + node1.Name,
						UUID: ovntypes.GWRouterPrefix + node1.Name + "-UUID",
						Nat:  []string{"egressip-nat-UUID1", "egressip-nat-UUID2"},
					},
					&nbdb.LogicalRouter{
						Name: ovntypes.GWRouterPrefix + node2.Name,
						UUID: ovntypes.GWRouterPrefix + node2.Name + "-UUID",
					},
					&nbdb.NAT{
						UUID:       "egressip-nat-UUID1",
						LogicalIP:  podV4IP,
						ExternalIP: eips[0],
						ExternalIDs: map[string]string{
							"name": egressIPName,
						},
						Type:        nbdb.NATTypeSNAT,
						LogicalPort: &expectedNatLogicalPort1,
						Options: map[string]string{
							"stateless": "false",
						},
					},
					&nbdb.NAT{
						UUID:       "egressip-nat-UUID2",
						LogicalIP:  "10.128.0.16",
						ExternalIP: eips[0],
						ExternalIDs: map[string]string{
							"name": egressIPName,
						},
						Type:        nbdb.NATTypeSNAT,
						LogicalPort: &expectedNatLogicalPort1,
						Options: map[string]string{
							"stateless": "false",
						},
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
						Type: "router",
						Options: map[string]string{
							"router-port":               types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
						Type: "router",
						Options: map[string]string{
							"router-port":               types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2.Name + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2.Name,
						Networks: []string{"100.64.0.3/29"},
					},
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name,
						Networks: []string{"100.64.0.2/29"},
					},
				}
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				node2.Labels = map[string]string{
					"k8s.ovn.org/egress-assignable": "",
				}

				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), &node2, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// NOTE: Cluster manager is the one who patches the egressIP object.
				// For the sake of unit testing egressip zone controller we need to patch egressIP object manually
				// There are tests in cluster-manager package covering the patch logic.
				status := []egressipv1.EgressIPStatusItem{
					{
						Node:     node1Name,
						EgressIP: egressIP1,
					},
					{
						Node:     node2Name,
						EgressIP: egressIP2,
					},
				}
				err = fakeOvn.controller.patchReplaceEgressIPStatus(egressIPName, status)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(2))
				gomega.Eventually(getEgressIPReassignmentCount).Should(gomega.Equal(0))

				eips, nodes = getEgressIPStatus(egressIPName)
				gomega.Expect(nodes[0]).To(gomega.Equal(node1.Name))
				gomega.Expect(nodes[1]).To(gomega.Equal(node2.Name))

				expectedNatLogicalPort2 := "k8s-node2"
				expectedDatabaseState = []libovsdbtest.TestData{
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match:    "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
						Action:   nbdb.LogicalRouterPolicyActionAllow,
						UUID:     "no-reroute-UUID",
					},
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match:    fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
						Action:   nbdb.LogicalRouterPolicyActionAllow,
						UUID:     "no-reroute-service-UUID",
					},
					&nbdb.LogicalRouterPolicy{
						Priority: types.EgressIPReroutePriority,
						Match:    fmt.Sprintf("ip4.src == %s", egressPod1.Status.PodIP),
						Action:   nbdb.LogicalRouterPolicyActionReroute,
						Nexthops: []string{"100.64.0.2", "100.64.0.3"},
						ExternalIDs: map[string]string{
							"name": eIP.Name,
						},
						UUID: "reroute-UUID1",
					},
					&nbdb.LogicalRouterPolicy{
						Priority: types.EgressIPReroutePriority,
						Match:    fmt.Sprintf("ip4.src == %s", egressPod2.Status.PodIP),
						Action:   nbdb.LogicalRouterPolicyActionReroute,
						Nexthops: []string{"100.64.0.2", "100.64.0.3"},
						ExternalIDs: map[string]string{
							"name": eIP.Name,
						},
						UUID: "reroute-UUID2",
					},
					&nbdb.NAT{
						UUID:       "egressip-nat-UUID1",
						LogicalIP:  podV4IP,
						ExternalIP: eips[0],
						ExternalIDs: map[string]string{
							"name": egressIPName,
						},
						Type:        nbdb.NATTypeSNAT,
						LogicalPort: &expectedNatLogicalPort1,
						Options: map[string]string{
							"stateless": "false",
						},
					},
					&nbdb.NAT{
						UUID:       "egressip-nat-UUID2",
						LogicalIP:  "10.128.0.16",
						ExternalIP: eips[0],
						ExternalIDs: map[string]string{
							"name": egressIPName,
						},
						Type:        nbdb.NATTypeSNAT,
						LogicalPort: &expectedNatLogicalPort1,
						Options: map[string]string{
							"stateless": "false",
						},
					},
					&nbdb.NAT{
						UUID:       "egressip-nat-UUID3",
						LogicalIP:  podV4IP,
						ExternalIP: eips[1],
						ExternalIDs: map[string]string{
							"name": egressIPName,
						},
						Type:        nbdb.NATTypeSNAT,
						LogicalPort: &expectedNatLogicalPort2,
						Options: map[string]string{
							"stateless": "false",
						},
					},
					&nbdb.NAT{
						UUID:       "egressip-nat-UUID4",
						LogicalIP:  "10.128.0.16",
						ExternalIP: eips[1],
						ExternalIDs: map[string]string{
							"name": egressIPName,
						},
						Type:        nbdb.NATTypeSNAT,
						LogicalPort: &expectedNatLogicalPort2,
						Options: map[string]string{
							"stateless": "false",
						},
					},
					&nbdb.LogicalRouter{
						Name:     ovntypes.OVNClusterRouter,
						UUID:     ovntypes.OVNClusterRouter + "-UUID",
						Policies: []string{"no-reroute-UUID", "no-reroute-service-UUID", "reroute-UUID1", "reroute-UUID2"},
					},
					&nbdb.LogicalRouter{
						Name: ovntypes.GWRouterPrefix + node1.Name,
						UUID: ovntypes.GWRouterPrefix + node1.Name + "-UUID",
						Nat:  []string{"egressip-nat-UUID1", "egressip-nat-UUID2"},
					},
					&nbdb.LogicalRouter{
						Name: ovntypes.GWRouterPrefix + node2.Name,
						UUID: ovntypes.GWRouterPrefix + node2.Name + "-UUID",
						Nat:  []string{"egressip-nat-UUID3", "egressip-nat-UUID4"},
					},
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2.Name + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2.Name,
						Networks: []string{"100.64.0.3/29"},
					},
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name,
						Networks: []string{"100.64.0.2/29"},
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
						Type: "router",
						Options: map[string]string{
							"router-port":               types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
						Type: "router",
						Options: map[string]string{
							"router-port":               types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
				}
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				// remove label from node2
				node2.Labels = map[string]string{}

				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), &node2, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				fakeOvn.patchEgressIPObj(node1Name, egressIP1)

				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
				gomega.Eventually(getEgressIPReassignmentCount).Should(gomega.Equal(1))

				expectedDatabaseState = []libovsdbtest.TestData{
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match:    "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
						Action:   nbdb.LogicalRouterPolicyActionAllow,
						UUID:     "no-reroute-UUID",
					},
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match:    fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
						Action:   nbdb.LogicalRouterPolicyActionAllow,
						UUID:     "no-reroute-service-UUID",
					},
					&nbdb.LogicalRouterPolicy{
						Priority: types.EgressIPReroutePriority,
						Match:    fmt.Sprintf("ip4.src == %s", egressPod1.Status.PodIP),
						Action:   nbdb.LogicalRouterPolicyActionReroute,
						Nexthops: nodeLogicalRouterIPv4,
						ExternalIDs: map[string]string{
							"name": eIP.Name,
						},
						UUID: "reroute-UUID1",
					},
					&nbdb.LogicalRouterPolicy{
						Priority: types.EgressIPReroutePriority,
						Match:    fmt.Sprintf("ip4.src == %s", egressPod2.Status.PodIP),
						Action:   nbdb.LogicalRouterPolicyActionReroute,
						Nexthops: nodeLogicalRouterIPv4,
						ExternalIDs: map[string]string{
							"name": eIP.Name,
						},
						UUID: "reroute-UUID2",
					},
					&nbdb.NAT{
						UUID:       "egressip-nat-UUID1",
						LogicalIP:  podV4IP,
						ExternalIP: eips[0],
						ExternalIDs: map[string]string{
							"name": egressIPName,
						},
						Type:        nbdb.NATTypeSNAT,
						LogicalPort: &expectedNatLogicalPort1,
						Options: map[string]string{
							"stateless": "false",
						},
					},
					&nbdb.NAT{
						UUID:       "egressip-nat-UUID2",
						LogicalIP:  "10.128.0.16",
						ExternalIP: eips[0],
						ExternalIDs: map[string]string{
							"name": egressIPName,
						},
						Type:        nbdb.NATTypeSNAT,
						LogicalPort: &expectedNatLogicalPort1,
						Options: map[string]string{
							"stateless": "false",
						},
					},
					&nbdb.NAT{
						UUID:       "egressip-nat-UUID3",
						LogicalIP:  "10.128.0.16",
						ExternalIP: "192.168.126.51", // adds back SNAT towards nodeIP
						Type:       nbdb.NATTypeSNAT,
						Options: map[string]string{
							"stateless": "false",
						},
					},
					&nbdb.LogicalRouter{
						Name:     ovntypes.OVNClusterRouter,
						UUID:     ovntypes.OVNClusterRouter + "-UUID",
						Policies: []string{"no-reroute-UUID", "no-reroute-service-UUID", "reroute-UUID1", "reroute-UUID2"},
					},
					&nbdb.LogicalRouter{
						Name: ovntypes.GWRouterPrefix + node1.Name,
						UUID: ovntypes.GWRouterPrefix + node1.Name + "-UUID",
						Nat:  []string{"egressip-nat-UUID1", "egressip-nat-UUID2"},
					},
					&nbdb.LogicalRouter{
						Name: ovntypes.GWRouterPrefix + node2.Name,
						UUID: ovntypes.GWRouterPrefix + node2.Name + "-UUID",
						Nat:  []string{"egressip-nat-UUID3"},
					},
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2.Name + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2.Name,
						Networks: []string{"100.64.0.3/29"},
					},
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name,
						Networks: []string{"100.64.0.2/29"},
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
						Type: "router",
						Options: map[string]string{
							"router-port":               types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
						Type: "router",
						Options: map[string]string{
							"router-port":               types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
				}
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				// remove label from node1
				node1.Labels = map[string]string{}

				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), &node1, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// NOTE: Cluster manager is the one who patches the egressIP object.
				// For the sake of unit testing egressip zone controller we need to patch egressIP object manually
				// There are tests in cluster-manager package covering the patch logic.
				status = []egressipv1.EgressIPStatusItem{}
				err = fakeOvn.controller.patchReplaceEgressIPStatus(egressIPName, status)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(0))
				gomega.Eventually(getEgressIPReassignmentCount).Should(gomega.Equal(1)) // though 2 egressIPs to be re-assigned its only 1 egressIP object

				expectedDatabaseState = []libovsdbtest.TestData{
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match:    "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
						Action:   nbdb.LogicalRouterPolicyActionAllow,
						UUID:     "no-reroute-UUID",
					},
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match:    fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
						Action:   nbdb.LogicalRouterPolicyActionAllow,
						UUID:     "no-reroute-service-UUID",
					},
					&nbdb.NAT{
						UUID:       "egressip-nat-UUID1",
						LogicalIP:  podV4IP,
						ExternalIP: "192.168.126.12", // adds back SNAT towards nodeIP
						Type:       nbdb.NATTypeSNAT,
						Options: map[string]string{
							"stateless": "false",
						},
					},
					&nbdb.NAT{
						UUID:       "egressip-nat-UUID3",
						LogicalIP:  "10.128.0.16",
						ExternalIP: "192.168.126.51",
						Type:       nbdb.NATTypeSNAT,
						Options: map[string]string{
							"stateless": "false",
						},
					},
					&nbdb.LogicalRouter{
						Name:     ovntypes.OVNClusterRouter,
						UUID:     ovntypes.OVNClusterRouter + "-UUID",
						Policies: []string{"no-reroute-UUID", "no-reroute-service-UUID"},
					},
					&nbdb.LogicalRouter{
						Name: ovntypes.GWRouterPrefix + node1.Name,
						UUID: ovntypes.GWRouterPrefix + node1.Name + "-UUID",
						Nat:  []string{"egressip-nat-UUID1"},
					},
					&nbdb.LogicalRouter{
						Name: ovntypes.GWRouterPrefix + node2.Name,
						UUID: ovntypes.GWRouterPrefix + node2.Name + "-UUID",
						Nat:  []string{"egressip-nat-UUID3"},
					},
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2.Name + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2.Name,
						Networks: []string{"100.64.0.3/29"},
					},
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name,
						Networks: []string{"100.64.0.2/29"},
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
						Type: "router",
						Options: map[string]string{
							"router-port":               types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
						Type: "router",
						Options: map[string]string{
							"router-port":               types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
				}
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

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

				annotations := map[string]string{
					"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4, node1IPv6),
					"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":[\"%s\", \"%s\"]}", v4NodeSubnet, v6NodeSubnet),
				}
				labels := map[string]string{
					"k8s.ovn.org/egress-assignable": "",
				}
				node1 := getNodeObj(node1Name, annotations, labels)
				annotations = map[string]string{
					"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node2IPv4, ""),
					"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":\"%s\"}", v4NodeSubnet),
				}
				node2 := getNodeObj(node2Name, annotations, labels)

				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{egressIP},
					},
					Status: egressipv1.EgressIPStatus{
						Items: []egressipv1.EgressIPStatusItem{},
					},
				}

				fakeOvn.startWithDBSetup(
					libovsdbtest.TestSetup{
						NBData: []libovsdbtest.TestData{
							&nbdb.LogicalRouterPort{
								UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name + "-UUID",
								Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name,
								Networks: []string{nodeLogicalRouterIfAddrV4, nodeLogicalRouterIfAddrV6},
							},
							&nbdb.LogicalRouterPort{
								UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2.Name + "-UUID",
								Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2.Name,
								Networks: []string{nodeLogicalRouterIfAddrV4},
							},
							&nbdb.LogicalRouter{
								Name: ovntypes.OVNClusterRouter,
								UUID: ovntypes.OVNClusterRouter + "-UUID",
							},
							&nbdb.LogicalRouter{
								Name: ovntypes.GWRouterPrefix + node1.Name,
								UUID: ovntypes.GWRouterPrefix + node1.Name + "-UUID",
							},
							&nbdb.LogicalRouter{
								Name: ovntypes.GWRouterPrefix + node2.Name,
								UUID: ovntypes.GWRouterPrefix + node2.Name + "-UUID",
							},
							&nbdb.LogicalSwitchPort{
								UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "UUID",
								Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
								Type: "router",
								Options: map[string]string{
									"router-port": types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
								},
							},
							&nbdb.LogicalSwitchPort{
								UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "UUID",
								Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
								Type: "router",
								Options: map[string]string{
									"router-port": types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
								},
							},
						},
					},
					&egressipv1.EgressIPList{
						Items: []egressipv1.EgressIP{eIP},
					},
					&v1.NodeList{
						Items: []v1.Node{node1},
					})

				err := fakeOvn.controller.WatchEgressIPNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIPPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressNodes()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectedDatabaseState := []libovsdbtest.TestData{
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name,
						Networks: []string{nodeLogicalRouterIfAddrV4, nodeLogicalRouterIfAddrV6},
					},
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2.Name + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2.Name,
						Networks: []string{nodeLogicalRouterIfAddrV4},
					},
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match:    "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
						Action:   nbdb.LogicalRouterPolicyActionAllow,
						UUID:     "no-reroute-UUID",
					},
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match:    fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
						Action:   nbdb.LogicalRouterPolicyActionAllow,
						UUID:     "no-reroute-service-UUID",
					},
					&nbdb.LogicalRouter{
						Name:     ovntypes.OVNClusterRouter,
						UUID:     ovntypes.OVNClusterRouter + "-UUID",
						Policies: []string{"no-reroute-UUID", "no-reroute-service-UUID"},
					},
					&nbdb.LogicalRouter{
						Name: ovntypes.GWRouterPrefix + node1.Name,
						UUID: ovntypes.GWRouterPrefix + node1.Name + "-UUID",
					},
					&nbdb.LogicalRouter{
						Name: ovntypes.GWRouterPrefix + node2.Name,
						UUID: ovntypes.GWRouterPrefix + node2.Name + "-UUID",
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
						Type: "router",
						Options: map[string]string{
							"router-port":               types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
						Type: "router",
						Options: map[string]string{
							"router-port": types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
						},
					},
				}
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
				fakeOvn.patchEgressIPObj(node1Name, egressIP)
				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
				egressIPs, nodes := getEgressIPStatus(egressIPName)
				gomega.Expect(nodes[0]).To(gomega.Equal(node1.Name))
				gomega.Expect(egressIPs[0]).To(gomega.Equal(egressIP))

				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Create(context.TODO(), &node2, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				expectedDatabaseState = []libovsdbtest.TestData{
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name,
						Networks: []string{nodeLogicalRouterIfAddrV4, nodeLogicalRouterIfAddrV6},
					},
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2.Name + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2.Name,
						Networks: []string{nodeLogicalRouterIfAddrV4},
					},
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match:    "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
						Action:   nbdb.LogicalRouterPolicyActionAllow,
						UUID:     "no-reroute-UUID",
					},
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match:    fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
						Action:   nbdb.LogicalRouterPolicyActionAllow,
						UUID:     "no-reroute-service-UUID",
					},
					&nbdb.LogicalRouter{
						Name:     ovntypes.OVNClusterRouter,
						UUID:     ovntypes.OVNClusterRouter + "-UUID",
						Policies: []string{"no-reroute-UUID", "no-reroute-service-UUID"},
					},
					&nbdb.LogicalRouter{
						Name: ovntypes.GWRouterPrefix + node1.Name,
						UUID: ovntypes.GWRouterPrefix + node1.Name + "-UUID",
					},
					&nbdb.LogicalRouter{
						Name: ovntypes.GWRouterPrefix + node2.Name,
						UUID: ovntypes.GWRouterPrefix + node2.Name + "-UUID",
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
						Type: "router",
						Options: map[string]string{
							"router-port":               types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
						Type: "router",
						Options: map[string]string{
							"router-port":               types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
				}
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
				egressIPs, nodes = getEgressIPStatus(egressIPName)
				gomega.Expect(nodes[0]).To(gomega.Equal(node1.Name))
				gomega.Expect(egressIPs[0]).To(gomega.Equal(egressIP))

				err = fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Delete(context.TODO(), node1.Name, *metav1.NewDeleteOptions(0))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.patchEgressIPObj(node2Name, egressIP)
				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))

				getNewNode := func() string {
					_, nodes = getEgressIPStatus(egressIPName)
					if len(nodes) > 0 {
						return nodes[0]
					}
					return ""
				}

				gomega.Eventually(getNewNode).Should(gomega.Equal(node2.Name))
				egressIPs, _ = getEgressIPStatus(egressIPName)
				gomega.Expect(egressIPs[0]).To(gomega.Equal(egressIP))

				expectedDatabaseState = []libovsdbtest.TestData{
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name,
						Networks: []string{nodeLogicalRouterIfAddrV4, nodeLogicalRouterIfAddrV6},
					},
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2.Name + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node2.Name,
						Networks: []string{nodeLogicalRouterIfAddrV4},
					},
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match:    "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
						Action:   nbdb.LogicalRouterPolicyActionAllow,
						UUID:     "no-reroute-UUID",
					},
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match:    fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
						Action:   nbdb.LogicalRouterPolicyActionAllow,
						UUID:     "no-reroute-service-UUID",
					},
					&nbdb.LogicalRouter{
						Name:     ovntypes.OVNClusterRouter,
						UUID:     ovntypes.OVNClusterRouter + "-UUID",
						Policies: []string{"no-reroute-UUID", "no-reroute-service-UUID"},
					},
					&nbdb.LogicalRouter{
						Name: ovntypes.GWRouterPrefix + node1.Name,
						UUID: ovntypes.GWRouterPrefix + node1.Name + "-UUID",
					},
					&nbdb.LogicalRouter{
						Name: ovntypes.GWRouterPrefix + node2.Name,
						UUID: ovntypes.GWRouterPrefix + node2.Name + "-UUID",
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
						Type: "router",
						Options: map[string]string{
							"router-port": types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
						},
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
						Type: "router",
						Options: map[string]string{
							"router-port":               types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
				}
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})
})

// TEST UTILITY FUNCTIONS;
// reduces redundant code

func getEIPSNAT(podIP, egressIP, expectedNatLogicalPort string) *nbdb.NAT {
	return &nbdb.NAT{
		UUID:       "egressip-nat-UUID",
		LogicalIP:  podIP,
		ExternalIP: egressIP,
		ExternalIDs: map[string]string{
			"name": egressIPName,
		},
		Type:        nbdb.NATTypeSNAT,
		LogicalPort: &expectedNatLogicalPort,
		Options: map[string]string{
			"stateless": "false",
		},
	}
}

func getReRoutePolicy(podIP, ipFamily string, nodeLogicalRouterIPv4 []string) *nbdb.LogicalRouterPolicy {
	return &nbdb.LogicalRouterPolicy{
		Priority: types.EgressIPReroutePriority,
		Match:    fmt.Sprintf("ip%s.src == %s", ipFamily, podIP),
		Action:   nbdb.LogicalRouterPolicyActionReroute,
		Nexthops: nodeLogicalRouterIPv4,
		ExternalIDs: map[string]string{
			"name": egressIPName,
		},
		UUID: "reroute-UUID",
	}
}

func getReRouteStaticRoute(podIP, nextHop string) *nbdb.LogicalRouterStaticRoute {
	return &nbdb.LogicalRouterStaticRoute{
		ExternalIDs: map[string]string{"name": egressIPName},
		Nexthop:     nextHop,
		Policy:      &nbdb.LogicalRouterStaticRoutePolicySrcIP,
		IPPrefix:    podIP,
		UUID:        "reroute-static-route-UUID",
	}
}

func getNodeObj(nodeName string, annotations, labels map[string]string) v1.Node {
	return v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        nodeName,
			Annotations: annotations,
			Labels:      labels,
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
}
