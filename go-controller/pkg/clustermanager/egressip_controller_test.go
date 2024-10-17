package clustermanager

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	ocpcloudnetworkapi "github.com/openshift/api/cloudnetwork/v1"
	ocpconfigapi "github.com/openshift/api/config/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	egressipv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/healthcheck"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/urfave/cli/v2"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	utilnet "k8s.io/utils/net"
)

type fakeEgressIPDialer struct{}

func (f fakeEgressIPDialer) dial(ip net.IP, timeout time.Duration) bool {
	return true
}

type fakeEgressIPHealthClient struct {
	Connected        bool
	ProbeCount       int
	FakeProbeFailure bool
}

func (fehc *fakeEgressIPHealthClient) IsConnected() bool {
	return fehc.Connected
}

func (fehc *fakeEgressIPHealthClient) Connect(dialCtx context.Context, mgmtIPs []net.IP, healthCheckPort int) bool {
	if fehc.FakeProbeFailure {
		return false
	}
	fehc.Connected = true
	return true
}

func (fehc *fakeEgressIPHealthClient) Disconnect() {
	fehc.Connected = false
	fehc.ProbeCount = 0
}

func (fehc *fakeEgressIPHealthClient) Probe(dialCtx context.Context) bool {
	if fehc.Connected && !fehc.FakeProbeFailure {
		fehc.ProbeCount++
		return true
	}
	return false
}

type fakeEgressIPHealthClientAllocator struct{}

func (f *fakeEgressIPHealthClientAllocator) allocate(nodeName string) healthcheck.EgressIPHealthClient {
	return &fakeEgressIPHealthClient{}
}

func newNamespaceMeta(namespace string, additionalLabels map[string]string) metav1.ObjectMeta {
	labels := map[string]string{
		"name": namespace,
	}
	for k, v := range additionalLabels {
		labels[k] = v
	}
	return metav1.ObjectMeta{
		UID:         k8stypes.UID(namespace),
		Name:        namespace,
		Labels:      labels,
		Annotations: map[string]string{},
	}
}

func newNamespace(namespace string) *v1.Namespace {
	return &v1.Namespace{
		ObjectMeta: newNamespaceMeta(namespace, nil),
		Spec:       v1.NamespaceSpec{},
		Status:     v1.NamespaceStatus{},
	}
}

var egressPodLabel = map[string]string{"egress": "needed"}

func newEgressIPMeta(name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		UID:  k8stypes.UID(name),
		Name: name,
		Labels: map[string]string{
			"name": name,
		},
	}
}

func newCloudPrivateIPConfigMeta(egressIP string) metav1.ObjectMeta {
	name := ipStringToCloudPrivateIPConfigName(egressIP)
	return metav1.ObjectMeta{
		UID:  k8stypes.UID(name),
		Name: name,
		Labels: map[string]string{
			"name": name,
		},
	}
}

func setupNode(nodeName string, ipNets []string, mockAllocationIPs map[string]string) egressNode {
	var config = &util.ParsedNodeEgressIPConfiguration{Capacity: util.Capacity{
		IP:   util.UnlimitedNodeCapacity,
		IPv4: util.UnlimitedNodeCapacity,
		IPv6: util.UnlimitedNodeCapacity,
	}}
	for _, ipNetStr := range ipNets {
		ip, ipNet, _ := net.ParseCIDR(ipNetStr)
		if utilnet.IsIPv6CIDR(ipNet) {
			config.V6 = util.ParsedIFAddr{IP: ip, Net: ipNet}
		} else {
			config.V4 = util.ParsedIFAddr{IP: ip, Net: ipNet}
		}
	}

	mockAllcations := map[string]string{}
	for mockAllocationIP, egressIPName := range mockAllocationIPs {
		mockAllcations[net.ParseIP(mockAllocationIP).String()] = egressIPName
	}

	node := egressNode{
		egressIPConfig:     config,
		allocations:        mockAllcations,
		healthClient:       hccAllocator.allocate(nodeName), // using fakeEgressIPHealthClientAllocator
		name:               nodeName,
		isReady:            true,
		isReachable:        true,
		isEgressAssignable: true,
	}
	return node
}

var _ = ginkgo.Describe("OVN cluster-manager EgressIP Operations", func() {
	var (
		app                   *cli.App
		fakeClusterManagerOVN *FakeClusterManager
	)

	const (
		node1Name     = "node1"
		node2Name     = "node2"
		egressIPName  = "egressip"
		egressIPName2 = "egressip-2"
		namespace     = "egressip-namespace"
		v4NodeSubnet  = "10.128.0.0/24"
		v6NodeSubnet  = "ae70::66/64"
	)

	dialer = fakeEgressIPDialer{}
	hccAllocator = &fakeEgressIPHealthClientAllocator{}

	getEgressIPAllocatorSizeSafely := func() int {
		fakeClusterManagerOVN.eIPC.allocator.Lock()
		defer fakeClusterManagerOVN.eIPC.allocator.Unlock()
		return len(fakeClusterManagerOVN.eIPC.allocator.cache)
	}

	getEgressIPAllocatorSafely := func(s string, v6 bool) util.ParsedIFAddr {
		fakeClusterManagerOVN.eIPC.allocator.Lock()
		defer fakeClusterManagerOVN.eIPC.allocator.Unlock()
		c, ok := fakeClusterManagerOVN.eIPC.allocator.cache[s]
		if !ok {
			panic(fmt.Sprintf("failed to find key %s", s))
		}
		if v6 {
			return c.egressIPConfig.V6
		} else {
			return c.egressIPConfig.V4
		}
	}

	getEgressIPAllocatorReachableSafely := func(s string) bool {
		fakeClusterManagerOVN.eIPC.allocator.Lock()
		defer fakeClusterManagerOVN.eIPC.allocator.Unlock()
		c, ok := fakeClusterManagerOVN.eIPC.allocator.cache[s]
		if !ok {
			panic(fmt.Sprintf("failed to find key %s", s))
		}
		return c.isReachable
	}

	doesEgressIPAllocatorContainSafely := func(s string) bool {
		fakeClusterManagerOVN.eIPC.allocator.Lock()
		defer fakeClusterManagerOVN.eIPC.allocator.Unlock()
		_, ok := fakeClusterManagerOVN.eIPC.allocator.cache[s]
		return ok
	}

	getEgressIPAllocatorHealthCheckSafely := func(s string) *fakeEgressIPHealthClient {
		fakeClusterManagerOVN.eIPC.allocator.Lock()
		defer fakeClusterManagerOVN.eIPC.allocator.Unlock()
		c, ok := fakeClusterManagerOVN.eIPC.allocator.cache[s]
		if !ok {
			panic(fmt.Sprintf("failed to find node %s in allocator", s))
		}

		return c.healthClient.(*fakeEgressIPHealthClient)
	}

	getEgressIPStatusLen := func(egressIPName string) func() int {
		return func() int {
			tmp, err := fakeClusterManagerOVN.fakeClient.EgressIPClient.K8sV1().EgressIPs().Get(context.TODO(), egressIPName, metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			return len(tmp.Status.Items)
		}
	}

	getEgressIPStatus := func(egressIPName string) ([]string, []string) {
		tmp, err := fakeClusterManagerOVN.fakeClient.EgressIPClient.K8sV1().EgressIPs().Get(context.TODO(), egressIPName, metav1.GetOptions{})
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
		egressIPs, err := fakeClusterManagerOVN.fakeClient.EgressIPClient.K8sV1().EgressIPs().List(context.TODO(), metav1.ListOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		for _, egressIP := range egressIPs.Items {
			if len(egressIP.Spec.EgressIPs) != len(egressIP.Status.Items) {
				reAssignmentCount++
			}
		}
		return reAssignmentCount
	}

	isEgressAssignableNode := func(nodeName string) func() bool {
		return func() bool {
			fakeClusterManagerOVN.eIPC.allocator.Lock()
			defer fakeClusterManagerOVN.eIPC.allocator.Unlock()
			if item, exists := fakeClusterManagerOVN.eIPC.allocator.cache[nodeName]; exists {
				return item.isEgressAssignable
			}
			return false
		}
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
		gomega.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
		config.OVNKubernetesFeature.EnableEgressIP = true
		config.OVNKubernetesFeature.EgressIPNodeHealthCheckPort = 1234

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags
		fakeClusterManagerOVN = NewFakeClusterManagerOVN()
	})

	ginkgo.AfterEach(func() {
		fakeClusterManagerOVN.shutdown()
	})

	ginkgo.Context("On node ADD/UPDATE/DELETE", func() {
		ginkgo.DescribeTable("should re-assign EgressIPs and perform proper egressIP allocation changes", func(egressIP, expectedNetwork string) {
			app.Action = func(ctx *cli.Context) error {
				node1IPv4OVN := "192.168.126.202/24"
				node1IPv4SecondaryHost := "10.10.10.3/24"
				node1Ipv4SecondaryHost2 := "7.7.7.7/16"
				node2IPv4OVN := "192.168.126.51/24"
				node2IPv4SecondaryHost := "10.10.10.4/24"
				node2Ipv4SecondaryHost2 := "7.7.7.8/16"
				node2Ipv4SecondaryHost3 := "5.5.5.5/16"

				egressNamespace := newNamespace(namespace)
				node1 := v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: node1Name,
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4OVN, ""),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":\"%s\"}", v4NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\",\"%s\",\"%s\"]", node1IPv4OVN, node1IPv4SecondaryHost, node1Ipv4SecondaryHost2),
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
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node2IPv4OVN, ""),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":\"%s\"}", v4NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\",\"%s\",\"%s\",\"%s\"]", node2IPv4OVN, node2IPv4SecondaryHost, node2Ipv4SecondaryHost2, node2Ipv4SecondaryHost3),
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
				fakeClusterManagerOVN.start(
					&egressipv1.EgressIPList{
						Items: []egressipv1.EgressIP{eIP},
					},
					&v1.NodeList{
						Items: []v1.Node{node1, node2},
					},
				)

				_, err := fakeClusterManagerOVN.eIPC.WatchEgressNodes()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				_, err = fakeClusterManagerOVN.eIPC.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(getEgressIPAllocatorSizeSafely).Should(gomega.Equal(2))
				gomega.Expect(fakeClusterManagerOVN.eIPC.allocator.cache).To(gomega.HaveKey(node1.Name))
				gomega.Expect(fakeClusterManagerOVN.eIPC.allocator.cache).To(gomega.HaveKey(node2.Name))
				gomega.Eventually(isEgressAssignableNode(node1.Name)).Should(gomega.BeTrue())
				gomega.Eventually(isEgressAssignableNode(node2.Name)).Should(gomega.BeFalse())

				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
				egressIPs, nodes := getEgressIPStatus(egressIPName)
				gomega.Expect(nodes[0]).To(gomega.Equal(node1.Name))
				gomega.Expect(egressIPs[0]).To(gomega.Equal(egressIP))
				node1.Labels = map[string]string{}
				node2.Labels = map[string]string{
					"k8s.ovn.org/egress-assignable": "",
				}

				_, err = fakeClusterManagerOVN.fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), &node1, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				_, err = fakeClusterManagerOVN.fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), &node2, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
				gomega.Eventually(nodeSwitch).Should(gomega.Equal(node2.Name))
				egressIPs, _ = getEgressIPStatus(egressIPName)
				gomega.Expect(egressIPs[0]).To(gomega.Equal(egressIP))

				return nil
			}

			err := app.Run([]string{
				app.Name,
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}, ginkgo.Entry("OVN network", "192.168.126.101", "192.168.126.0/24"),
			ginkgo.Entry("Secondary host network", "10.10.10.100", "10.10.10.0/24"),
			ginkgo.Entry("Secondary host network", "7.7.7.100", "7.7.0.0/16"),
			ginkgo.Entry("Secondary host network", "7.7.8.100", "7.7.0.0/16"),
		)

		ginkgo.DescribeTable("should re-assign EgressIPs and perform proper egressIP allocation changes during node deletion", func(egressIP, expectedNetwork string) {
			app.Action = func(ctx *cli.Context) error {
				node1IPv4OVN := "192.168.126.202/24"
				node1IPv4SecondaryHost := "10.10.10.3/24"
				node1Ipv4SecondaryHost2 := "7.7.7.7/16"
				node2IPv4OVN := "192.168.126.51/24"
				node2IPv4SecondaryHost := "10.10.10.4/24"
				node2Ipv4SecondaryHost2 := "7.7.7.8/16"
				node2Ipv4SecondaryHost3 := "5.5.5.5/16"
				egressNamespace := newNamespace(namespace)

				node1 := v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: node1Name,
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4OVN, ""),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":\"%s\"}", v4NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\",\"%s\",\"%s\"]", node1IPv4OVN, node1IPv4SecondaryHost, node1Ipv4SecondaryHost2),
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
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node2IPv4OVN, ""),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":\"%s\"}", v4NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\",\"%s\",\"%s\",\"%s\"]", node2IPv4OVN, node2IPv4SecondaryHost, node2Ipv4SecondaryHost2, node2Ipv4SecondaryHost3),
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

				fakeClusterManagerOVN.start(
					&egressipv1.EgressIPList{
						Items: []egressipv1.EgressIP{eIP},
					},
					&v1.NodeList{
						Items: []v1.Node{node1, node2},
					},
				)

				_, err := fakeClusterManagerOVN.eIPC.WatchEgressNodes()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				_, err = fakeClusterManagerOVN.eIPC.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(getEgressIPAllocatorSizeSafely).Should(gomega.Equal(2))
				gomega.Expect(fakeClusterManagerOVN.eIPC.allocator.cache).To(gomega.HaveKey(node1.Name))
				gomega.Expect(fakeClusterManagerOVN.eIPC.allocator.cache).To(gomega.HaveKey(node2.Name))
				gomega.Eventually(isEgressAssignableNode(node1.Name)).Should(gomega.BeTrue())
				gomega.Eventually(isEgressAssignableNode(node2.Name)).Should(gomega.BeFalse())

				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
				egressIPs, nodes := getEgressIPStatus(egressIPName)
				gomega.Expect(nodes[0]).To(gomega.Equal(node1.Name))
				gomega.Expect(egressIPs[0]).To(gomega.Equal(egressIP))

				node2.Labels = map[string]string{
					"k8s.ovn.org/egress-assignable": "",
				}
				_, err = fakeClusterManagerOVN.fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), &node2, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				err = fakeClusterManagerOVN.fakeClient.KubeClient.CoreV1().Nodes().Delete(context.TODO(), node1Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
				gomega.Eventually(nodeSwitch).Should(gomega.Equal(node2.Name))
				egressIPs, _ = getEgressIPStatus(egressIPName)
				gomega.Expect(egressIPs[0]).To(gomega.Equal(egressIP))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}, ginkgo.Entry("OVN network", "192.168.126.101", "192.168.126.0/24"),
			ginkgo.Entry("Secondary host network", "10.10.10.100", "10.10.10.0/24"),
			ginkgo.Entry("Secondary host network", "7.7.7.100", "7.7.0.0/16"),
			ginkgo.Entry("Secondary host network", "7.7.8.100", "7.7.0.0/16"),
		)
		ginkgo.It("should assign EgressIPs to a linux node when there are windows nodes in the cluster", func() {
			app.Action = func(ctx *cli.Context) error {
				node1IPv4OVNManaged := "192.168.126.202/24"
				node1IPv4NonOVNManaged := "10.10.10.3/24"
				node1Ipv4NonOVNManaged2 := "7.7.7.7/16"
				egressIP := "192.168.126.101"
				egressNamespace := newNamespace(namespace)

				var err error
				config.Kubernetes.NoHostSubnetNodes, err = metav1.LabelSelectorAsSelector(&metav1.LabelSelector{
					MatchLabels: map[string]string{"kubernetes.io/os": "windows"},
				})
				gomega.Expect(err).ToNot(gomega.HaveOccurred())

				node1 := v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: node1Name,
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4OVNManaged, ""),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":\"%s\"}", v4NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\",\"%s\",\"%s\"]", node1IPv4OVNManaged, node1IPv4NonOVNManaged, node1Ipv4NonOVNManaged2),
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
				hybridOverlayNode := v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: "hybridOverlayNode",
						Annotations: map[string]string{
							"k8s.ovn.org/hybrid-overlay-distributed-router-gateway-mac": "00-15-5D-7B-E7-D2",
							"k8s.ovn.org/hybrid-overlay-node-subnet":                    "10.132.0.0/24",
						},
						Labels: map[string]string{
							"kubernetes.io/os": "windows",
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
				fakeClusterManagerOVN.start(
					&egressipv1.EgressIPList{
						Items: []egressipv1.EgressIP{eIP},
					},
					&v1.NodeList{
						Items: []v1.Node{node1, hybridOverlayNode},
					},
				)
				_, err = fakeClusterManagerOVN.eIPC.WatchEgressNodes()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				_, err = fakeClusterManagerOVN.eIPC.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(getEgressIPAllocatorSizeSafely).Should(gomega.Equal(1))
				gomega.Expect(fakeClusterManagerOVN.eIPC.allocator.cache).To(gomega.HaveKey(node1.Name))
				gomega.Eventually(isEgressAssignableNode(node1.Name)).Should(gomega.BeTrue())

				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
				egressIPs, nodes := getEgressIPStatus(egressIPName)
				gomega.Expect(nodes[0]).To(gomega.Equal(node1.Name))
				gomega.Expect(egressIPs[0]).To(gomega.Equal(egressIP))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})

	ginkgo.Context("WatchEgressNodes", func() {

		ginkgo.It("should populated egress node data as they are tagged `egress assignable` with variants of IPv4/IPv6", func() {
			app.Action = func(ctx *cli.Context) error {

				node1IPv4OVN := "192.168.128.202/24"
				node1IPv6OVN := "1521:b9cc:59b3:b9ee:95e9:f04c:4fec:33c4/64"
				node1IPv4SecondaryHost := "10.10.10.3/24"
				node1IPv4SecondaryHost2 := "7.7.7.7/16"
				node2IPv4OVN := "192.168.126.51/24"
				node2IPv4SecondaryHost := "10.10.10.20/24"
				node2IPv4SecondaryHost2 := "7.7.7.40/16"

				node1 := v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: "node1",
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4OVN, node1IPv6OVN),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":[\"%s\", \"%s\"]}", v4NodeSubnet, v6NodeSubnet),
							util.OVNNodeHostCIDRs: fmt.Sprintf("[\"%s\",\"%s\",\"%s\",\"%s\"]", node1IPv4OVN,
								node1IPv6OVN, node1IPv4SecondaryHost, node1IPv4SecondaryHost2),
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
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node2IPv4OVN, ""),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":\"%s\"}", v4NodeSubnet),
							util.OVNNodeHostCIDRs: fmt.Sprintf("[\"%s\",\"%s\",\"%s\"]", node2IPv4OVN,
								node2IPv4SecondaryHost, node2IPv4SecondaryHost2),
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
				fakeClusterManagerOVN.start(
					&v1.NodeList{
						Items: []v1.Node{},
					},
				)

				_, err := fakeClusterManagerOVN.eIPC.WatchEgressNodes()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(getEgressIPAllocatorSizeSafely).Should(gomega.Equal(0))

				node1.Labels = map[string]string{
					"k8s.ovn.org/egress-assignable": "",
				}

				_, node1ipV4OVNNet, err := net.ParseCIDR(node1IPv4OVN)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				_, err = fakeClusterManagerOVN.fakeClient.KubeClient.CoreV1().Nodes().Create(context.TODO(), &node1, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(getEgressIPAllocatorSizeSafely).Should(gomega.Equal(1))
				gomega.Expect(getEgressIPAllocatorSafely(node1.Name, false).Net).To(gomega.Equal(node1ipV4OVNNet))
				node2.Labels = map[string]string{
					"k8s.ovn.org/egress-assignable": "",
				}
				_, err = fakeClusterManagerOVN.fakeClient.KubeClient.CoreV1().Nodes().Create(context.TODO(), &node2, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(getEgressIPAllocatorSizeSafely).Should(gomega.Equal(2))
				gomega.Expect(getEgressIPAllocatorSafely(node1.Name, false).Net).To(gomega.Equal(node1ipV4OVNNet))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("using retry to create egress node with forced error followed by an update", func() {
			app.Action = func(ctx *cli.Context) error {
				nodeIPv4 := "192.168.126.51/24"
				nodeIPv6 := "9287:980a:2f3a:52e5:a579:df85:e82f:b7cd/64"
				node := v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: "node",
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", nodeIPv4, nodeIPv6),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":[\"%s\", \"%s\"]}", v4NodeSubnet, v6NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\",\"%s\"]", nodeIPv4, nodeIPv6),
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
				fakeClusterManagerOVN.start(
					&v1.NodeList{
						Items: []v1.Node{},
					},
				)
				_, err := fakeClusterManagerOVN.eIPC.WatchEgressNodes()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(getEgressIPAllocatorSizeSafely).Should(gomega.Equal(0))

				_, ipV4Sub, err := net.ParseCIDR(nodeIPv4)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				_, ipV6Sub, err := net.ParseCIDR(nodeIPv6)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				node.Labels = map[string]string{
					"k8s.ovn.org/egress-assignable": "",
				}
				_, err = fakeClusterManagerOVN.fakeClient.KubeClient.CoreV1().Nodes().Create(context.TODO(), &node, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				node.Labels = map[string]string{}
				_, err = fakeClusterManagerOVN.fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), &node, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(getEgressIPAllocatorSizeSafely).Should(gomega.Equal(1))
				gomega.Expect(doesEgressIPAllocatorContainSafely(node.Name)).To(gomega.BeTrue())
				gomega.Expect(getEgressIPAllocatorSafely(node.Name, false).Net).To(gomega.Equal(ipV4Sub))
				gomega.Expect(getEgressIPAllocatorSafely(node.Name, true).Net).To(gomega.Equal(ipV6Sub))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("ensure only one egressIP is assigned to the given node while rest of the IPs go into pending state", func() {
			app.Action = func(ctx *cli.Context) error {

				config.Gateway.DisableSNATMultipleGWs = true

				egressIP1 := "192.168.126.25"
				egressIP2 := "192.168.126.30"
				egressIP3 := "192.168.126.35"
				node1IPv4 := "192.168.126.12/24"
				node2IPv4 := "192.168.126.13/24"

				egressNamespace := newNamespace(namespace)

				node1 := v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: node1Name,
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\"}", node1IPv4),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":\"%s\"}", v4NodeSubnet),
							"k8s.ovn.org/l3-gateway-config":   `{"default":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"192.168.126.12/24", "next-hop":"192.168.126.1"}}`,
							"k8s.ovn.org/node-chassis-id":     "79fdcfc4-6fe6-4cd3-8242-c0f85a4668ec",
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node1IPv4),
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
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\"}", node2IPv4),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":\"%s\"}", v4NodeSubnet),
							"k8s.ovn.org/l3-gateway-config":   `{"default":{"mode":"local","mac-address":"7e:57:f8:f0:3c:50", "ip-address":"192.168.126.13/24", "next-hop":"192.168.126.1"}}`,
							"k8s.ovn.org/node-chassis-id":     "79fdcfc4-6fe6-4cd3-8242-c0f85a4668ec",
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node2IPv4),
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

				fakeClusterManagerOVN.start(
					&egressipv1.EgressIPList{
						Items: []egressipv1.EgressIP{eIP1, eIP2},
					},
					&v1.NodeList{
						Items: []v1.Node{node1, node2},
					},
				)

				_, err := fakeClusterManagerOVN.eIPC.WatchEgressNodes()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				_, err = fakeClusterManagerOVN.eIPC.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Ensure first egressIP object is assigned, since only node1 is an egressNode, only 1IP will be assigned, other will be pending
				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
				gomega.Eventually(getEgressIPReassignmentCount).Should(gomega.Equal(1))
				recordedEvent := <-fakeClusterManagerOVN.fakeRecorder.Events
				gomega.Expect(recordedEvent).To(gomega.ContainSubstring("Not all egress IPs for EgressIP: %s could be assigned, please tag more nodes", eIP1.Name))
				egressIPs1, nodes1 := getEgressIPStatus(egressIPName)
				gomega.Expect(nodes1[0]).To(gomega.Equal(node1.Name))
				possibleAssignments := sets.NewString(egressIP1, egressIP2)
				gomega.Expect(possibleAssignments.Has(egressIPs1[0])).To(gomega.BeTrue())

				// Ensure second egressIP object is also assigned to node1, but no OVN config will be done for this
				gomega.Eventually(getEgressIPStatusLen(egressIPName2)).Should(gomega.Equal(1))
				egressIPs2, nodes2 := getEgressIPStatus(egressIPName2)
				gomega.Expect(nodes2[0]).To(gomega.Equal(node1.Name))
				gomega.Expect(egressIPs2[0]).To(gomega.Equal(egressIP3))
				// Make second node egressIP assignable
				node2.Labels = map[string]string{
					"k8s.ovn.org/egress-assignable": "",
				}
				_, err = fakeClusterManagerOVN.fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), &node2, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// ensure secondIP from first object gets assigned to node2
				gomega.Eventually(isEgressAssignableNode(node2.Name)).Should(gomega.BeTrue())
				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(2))
				egressIPs1, nodes1 = getEgressIPStatus(egressIPName)
				gomega.Expect(nodes1[1]).To(gomega.Equal(node2.Name))
				gomega.Expect(possibleAssignments.Has(egressIPs1[1])).To(gomega.BeTrue())

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should skip populating egress node data for nodes that have incorrect IP address", func() {
			app.Action = func(ctx *cli.Context) error {
				config.OVNKubernetesFeature.EnableInterconnect = true // no impact on global eIPC functions
				nodeIPv4 := "192.168.126.510/24"
				nodeIPv6 := "1521:b9cc:59b3:b9ee:95e9:f04c:4fec:33c4/64"
				node := v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: node1Name,
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", nodeIPv4, nodeIPv6),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":[\"%s\", \"%s\"]}", v4NodeSubnet, v6NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\",\"%s\"]", nodeIPv4, nodeIPv6),
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
				fakeClusterManagerOVN.start(
					&v1.NodeList{
						Items: []v1.Node{node},
					},
				)

				allocatorItems := func() int {
					return len(fakeClusterManagerOVN.eIPC.allocator.cache)
				}

				_, err := fakeClusterManagerOVN.eIPC.WatchEgressNodes()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(allocatorItems).Should(gomega.Equal(0))

				node.Labels = map[string]string{
					"k8s.ovn.org/egress-assignable": "",
				}

				_, err = fakeClusterManagerOVN.fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), &node, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(allocatorItems).Should(gomega.Equal(0))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should probe nodes using grpc", func() {
			app.Action = func(ctx *cli.Context) error {
				config.OVNKubernetesFeature.EnableInterconnect = false // no impact on global eIPC functions
				node1IPv6 := "1521:b9cc:59b3:b9ee:95e9:f04c:4fec:33c4/64"
				node2IPv4 := "192.168.126.51/24"

				node1 := v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: "node1",
						Labels: map[string]string{
							"k8s.ovn.org/egress-assignable": "",
						},
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", "", node1IPv6),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":\"%s\"}", v6NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node1IPv6),
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
						Labels: map[string]string{
							"k8s.ovn.org/egress-assignable": "",
						},
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node2IPv4, ""),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":\"%s\"}", v4NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node2IPv4),
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
				fakeClusterManagerOVN.start()
				_, err := fakeClusterManagerOVN.eIPC.WatchEgressNodes()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(getEgressIPAllocatorSizeSafely).Should(gomega.Equal(0))

				_, ip1V6Sub, err := net.ParseCIDR(node1IPv6)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				_, ip2V4Sub, err := net.ParseCIDR(node2IPv4)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				_, err = fakeClusterManagerOVN.fakeClient.KubeClient.CoreV1().Nodes().Create(context.TODO(), &node1, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(getEgressIPAllocatorSizeSafely).Should(gomega.Equal(1))
				gomega.Expect(doesEgressIPAllocatorContainSafely(node1.Name)).To(gomega.BeTrue())
				gomega.Expect(getEgressIPAllocatorSafely(node1.Name, true).Net).To(gomega.Equal(ip1V6Sub))

				_, err = fakeClusterManagerOVN.fakeClient.KubeClient.CoreV1().Nodes().Create(context.TODO(), &node2, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(getEgressIPAllocatorSizeSafely).Should(gomega.Equal(2))
				gomega.Eventually(isEgressAssignableNode(node1.Name)).Should(gomega.BeTrue())
				gomega.Eventually(isEgressAssignableNode(node2.Name)).Should(gomega.BeTrue())
				gomega.Expect(doesEgressIPAllocatorContainSafely(node1.Name)).To(gomega.BeTrue())
				gomega.Expect(doesEgressIPAllocatorContainSafely(node2.Name)).To(gomega.BeTrue())

				gomega.Expect(getEgressIPAllocatorSafely(node1.Name, true).Net).To(gomega.Equal(ip1V6Sub))
				gomega.Expect(getEgressIPAllocatorSafely(node2.Name, false).Net).To(gomega.Equal(ip2V4Sub))
				// Explicitly call check reachibility so we need not to wait for slow periodic timer
				checkEgressNodesReachabilityIterate(fakeClusterManagerOVN.eIPC)
				gomega.Expect(getEgressIPAllocatorReachableSafely(node1.Name)).To(gomega.BeTrue())
				gomega.Expect(getEgressIPAllocatorReachableSafely(node2.Name)).To(gomega.BeTrue())

				// The test cases below will manipulate the fakeEgressIPHealthClient used for mocking
				// a gRPC session dedicated to monitoring each of the 2 nodes created. It does that
				// by setting the probe fail boolean which in turn causes the mocked probe call to
				// pretend that the periodic monitor succeeded or not.
				tests := []struct {
					desc            string
					node1FailProbes bool
					node2FailProbes bool
					// This function is an optional and generic function for the test case
					// to allow any special pre-conditioning needed before invoking of
					// checkEgressNodesReachabilityIterate in the test.
					tcPrepareFunc func(hcc1, hcc2 *fakeEgressIPHealthClient)
				}{
					{
						desc:            "disconnect nodes",
						node1FailProbes: true,
						node2FailProbes: true,
						tcPrepareFunc: func(hcc1, hcc2 *fakeEgressIPHealthClient) {
							hcc1.Disconnect()
							hcc2.Disconnect()
						},
					},
					{
						desc:            "connect node1",
						node2FailProbes: true,
					},
					{
						desc: "node1 connected, connect node2",
					},
					{
						desc:            "node1 and node2 connected, bump only node2 counters",
						node1FailProbes: true,
					},
					{
						desc:            "node2 connected, disconnect node1",
						node1FailProbes: true,
						node2FailProbes: true,
						tcPrepareFunc: func(hcc1, hcc2 *fakeEgressIPHealthClient) {
							hcc1.Disconnect()
						},
					},
					{
						desc:            "connect node1, disconnect node2",
						node2FailProbes: true,
						tcPrepareFunc: func(hcc1, hcc2 *fakeEgressIPHealthClient) {
							hcc2.Disconnect()
						},
					},
					{
						desc: "node1 and node2 connected and both counters bump",
						tcPrepareFunc: func(hcc1, hcc2 *fakeEgressIPHealthClient) {
							// Perform an additional iteration, to make probe counters to bump on second call
							checkEgressNodesReachabilityIterate(fakeClusterManagerOVN.eIPC)
						},
					},
				}

				// hcc1 and hcc2 are the mocked gRPC client to node1 and node2, respectively.
				// They are what we use to manipulate whether probes to the node should fail or
				// not, as well as a mechanism for explicitly disconnecting as part of the test.
				hcc1 := getEgressIPAllocatorHealthCheckSafely(node1.Name)
				hcc2 := getEgressIPAllocatorHealthCheckSafely(node2.Name)

				// ttIterCheck is the common function used by each test case. It will check whether
				// a client changed its connection state and if the number of probes to the node
				// changed as expected.
				ttIterCheck := func(hcc *fakeEgressIPHealthClient, prevNodeIsConnected bool, prevProbes int, failProbes bool, desc string) {
					currNodeIsConnected := hcc.IsConnected()
					gomega.Expect(currNodeIsConnected || failProbes).To(gomega.BeTrue(), desc)

					if !prevNodeIsConnected && !currNodeIsConnected {
						// Not connected (before and after): no probes should be successful
						gomega.Expect(hcc.ProbeCount).To(gomega.Equal(prevProbes), desc)
					} else if prevNodeIsConnected && currNodeIsConnected {
						if failProbes {
							// Still connected, but no probes should be successful
							gomega.Expect(prevProbes).To(gomega.Equal(hcc.ProbeCount), desc)
						} else {
							// Still connected and probe counters should be going up
							gomega.Expect(prevProbes < hcc.ProbeCount).To(gomega.BeTrue(), desc)
						}
					}
				}

				for _, tt := range tests {
					hcc1.FakeProbeFailure = tt.node1FailProbes
					hcc2.FakeProbeFailure = tt.node2FailProbes

					prevNode1IsConnected := hcc1.IsConnected()
					prevNode2IsConnected := hcc2.IsConnected()
					prevNode1Probes := hcc1.ProbeCount
					prevNode2Probes := hcc2.ProbeCount

					if tt.tcPrepareFunc != nil {
						tt.tcPrepareFunc(hcc1, hcc2)
					}

					// Perform connect or probing, depending on the state of the connections
					checkEgressNodesReachabilityIterate(fakeClusterManagerOVN.eIPC)

					ttIterCheck(hcc1, prevNode1IsConnected, prevNode1Probes, tt.node1FailProbes, tt.desc)
					ttIterCheck(hcc2, prevNode2IsConnected, prevNode2Probes, tt.node2FailProbes, tt.desc)
				}

				gomega.Expect(hcc1.IsConnected()).To(gomega.BeTrue())
				gomega.Expect(hcc2.IsConnected()).To(gomega.BeTrue())

				// Lastly, remove egress assignable from node 2 and make sure it disconnects
				node2.Labels = map[string]string{}
				_, err = fakeClusterManagerOVN.fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), &node2, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(isEgressAssignableNode(node1.Name)).Should(gomega.BeTrue())
				gomega.Eventually(isEgressAssignableNode(node2.Name)).Should(gomega.BeFalse())

				// Explicitly call check reachibility so we need not to wait for slow periodic timer
				checkEgressNodesReachabilityIterate(fakeClusterManagerOVN.eIPC)

				gomega.Expect(hcc1.IsConnected()).To(gomega.BeTrue())
				gomega.Expect(hcc2.IsConnected()).To(gomega.BeFalse())
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})

	ginkgo.Context("WatchEgressNodes running with WatchEgressIP", func() {
		ginkgo.It("should result in error and event if specified egress IP is a cluster node IP", func() {
			app.Action = func(ctx *cli.Context) error {

				egressIP := "192.168.126.51"
				node1IPv4 := "192.168.128.202/24"
				node1IPv6 := "1521:b9cc:59b3:b9ee:95e9:f04c:4fec:33c4/64"
				node2IPv4 := "192.168.126.51/24"

				node1 := v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: node1Name,
						Labels: map[string]string{
							"k8s.ovn.org/egress-assignable": "",
						},
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4, node1IPv6),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":[\"%s\", \"%s\"]}", v4NodeSubnet, v6NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\",\"%s\"]", node1IPv4, node1IPv6),
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
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":\"%s\"}", v4NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node2IPv4),
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

				fakeClusterManagerOVN.start(
					&egressipv1.EgressIPList{
						Items: []egressipv1.EgressIP{eIP},
					},
					&v1.NodeList{
						Items: []v1.Node{node1, node2},
					})

				_, err := fakeClusterManagerOVN.eIPC.WatchEgressNodes()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				_, err = fakeClusterManagerOVN.eIPC.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(getEgressIPAllocatorSizeSafely).Should(gomega.Equal(2))
				gomega.Expect(fakeClusterManagerOVN.eIPC.allocator.cache).To(gomega.HaveKey(node1.Name))
				gomega.Expect(fakeClusterManagerOVN.eIPC.allocator.cache).To(gomega.HaveKey(node2.Name))

				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(0))
				gomega.Eventually(fakeClusterManagerOVN.fakeRecorder.Events).Should(gomega.HaveLen(3))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should remove stale EgressIP setup when node label is removed while ovnkube-master is not running and assign to newly labelled node", func() {
			app.Action = func(ctx *cli.Context) error {

				egressIP1 := "192.168.126.25"
				node1IPv4 := "192.168.126.51/24"

				egressNamespace := newNamespace(namespace)

				node1 := v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: node1Name,
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\"}", node1IPv4),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":\"%s\"}", v4NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node1IPv4),
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
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\"}", node1IPv4),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":\"%s\"}", v4NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node1IPv4),
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

				fakeClusterManagerOVN.start(
					&egressipv1.EgressIPList{
						Items: []egressipv1.EgressIP{eIP},
					},
					&v1.NodeList{
						Items: []v1.Node{node1, node2},
					},
				)

				_, err := fakeClusterManagerOVN.eIPC.WatchEgressNodes()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				_, err = fakeClusterManagerOVN.eIPC.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
				gomega.Eventually(getEgressIPReassignmentCount).Should(gomega.Equal(0))
				gomega.Expect(fakeClusterManagerOVN.eIPC.allocator.cache).To(gomega.HaveKey(node2.Name))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should only get assigned EgressIPs which matches their subnet when the node is tagged", func() {
			app.Action = func(ctx *cli.Context) error {

				egressIP := "192.168.126.101"
				node1IPv4 := "192.168.128.202/24"
				node1IPv6 := "1521:b9cc:59b3:b9ee:95e9:f04c:4fec:33c4/64"
				node2IPv4 := "192.168.126.51/24"

				node1 := v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: node1Name,
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4, node1IPv6),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":\"%s\"}", v4NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\",\"%s\"]", node1IPv4, node1IPv6),
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
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":\"%s\"}", v4NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node2IPv4),
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

				fakeClusterManagerOVN.start(
					&egressipv1.EgressIPList{
						Items: []egressipv1.EgressIP{eIP},
					},
					&v1.NodeList{
						Items: []v1.Node{node1, node2},
					})

				_, err := fakeClusterManagerOVN.eIPC.WatchEgressNodes()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				_, err = fakeClusterManagerOVN.eIPC.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				_, ip1V4Sub, err := net.ParseCIDR(node1IPv4)
				_, ip1V6Sub, err := net.ParseCIDR(node1IPv6)
				_, ip2V4Sub, err := net.ParseCIDR(node2IPv4)

				gomega.Eventually(getEgressIPAllocatorSizeSafely).Should(gomega.Equal(2))
				gomega.Expect(fakeClusterManagerOVN.eIPC.allocator.cache).To(gomega.HaveKey(node1.Name))
				gomega.Expect(fakeClusterManagerOVN.eIPC.allocator.cache).To(gomega.HaveKey(node2.Name))
				gomega.Eventually(isEgressAssignableNode(node1.Name)).Should(gomega.BeFalse())
				gomega.Eventually(isEgressAssignableNode(node2.Name)).Should(gomega.BeFalse())
				gomega.Expect(getEgressIPAllocatorSafely(node1.Name, false).Net).To(gomega.Equal(ip1V4Sub))
				gomega.Expect(getEgressIPAllocatorSafely(node1.Name, true).Net).To(gomega.Equal(ip1V6Sub))
				gomega.Expect(getEgressIPAllocatorSafely(node2.Name, false).Net).To(gomega.Equal(ip2V4Sub))
				gomega.Eventually(eIP.Status.Items).Should(gomega.HaveLen(0))

				node1.Labels = map[string]string{
					"k8s.ovn.org/egress-assignable": "",
				}

				_, err = fakeClusterManagerOVN.fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), &node1, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(0))
				gomega.Eventually(isEgressAssignableNode(node1.Name)).Should(gomega.BeTrue())

				gomega.Eventually(getEgressIPReassignmentCount).Should(gomega.Equal(1))

				node2.Labels = map[string]string{
					"k8s.ovn.org/egress-assignable": "",
				}

				_, err = fakeClusterManagerOVN.fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), &node2, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))

				egressIPs, nodes := getEgressIPStatus(egressIPName)
				gomega.Expect(nodes[0]).To(gomega.Equal(node2.Name))
				gomega.Expect(egressIPs[0]).To(gomega.Equal(egressIP))
				gomega.Eventually(getEgressIPReassignmentCount).Should(gomega.Equal(0))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should re-balance EgressIPs when their node is removed", func() {
			app.Action = func(ctx *cli.Context) error {
				config.OVNKubernetesFeature.EnableInterconnect = true // no impact on global eIPC functions
				egressIP := "192.168.126.101"
				node1IPv4 := "192.168.126.12/24"
				node1IPv6 := "1521:b9cc:59b3:b9ee:95e9:f04c:4fec:33c4/64"
				node2IPv4 := "192.168.126.51/24"

				node1 := v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: node1Name,
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4, node1IPv6),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":[\"%s\", \"%s\"]}", v4NodeSubnet, v6NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\",\"%s\"]", node1IPv4, node1IPv6),
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
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":\"%s\"}", v4NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node2IPv4),
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

				fakeClusterManagerOVN.start(
					&egressipv1.EgressIPList{
						Items: []egressipv1.EgressIP{eIP},
					},
					&v1.NodeList{
						Items: []v1.Node{node1},
					})

				_, err := fakeClusterManagerOVN.eIPC.WatchEgressNodes()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				_, err = fakeClusterManagerOVN.eIPC.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(getEgressIPAllocatorSizeSafely).Should(gomega.Equal(1))
				gomega.Expect(fakeClusterManagerOVN.eIPC.allocator.cache).To(gomega.HaveKey(node1.Name))
				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
				egressIPs, nodes := getEgressIPStatus(egressIPName)
				gomega.Expect(nodes[0]).To(gomega.Equal(node1.Name))
				gomega.Expect(egressIPs[0]).To(gomega.Equal(egressIP))
				_, err = fakeClusterManagerOVN.fakeClient.KubeClient.CoreV1().Nodes().Create(context.TODO(), &node2, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
				egressIPs, nodes = getEgressIPStatus(egressIPName)
				gomega.Expect(nodes[0]).To(gomega.Equal(node1.Name))
				gomega.Expect(egressIPs[0]).To(gomega.Equal(egressIP))
				gomega.Eventually(getEgressIPAllocatorSizeSafely).Should(gomega.Equal(2))
				gomega.Expect(fakeClusterManagerOVN.eIPC.allocator.cache).To(gomega.HaveKey(node1.Name))
				gomega.Expect(fakeClusterManagerOVN.eIPC.allocator.cache).To(gomega.HaveKey(node2.Name))

				err = fakeClusterManagerOVN.fakeClient.KubeClient.CoreV1().Nodes().Delete(context.TODO(), node1.Name, *metav1.NewDeleteOptions(0))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(getEgressIPAllocatorSizeSafely).Should(gomega.Equal(1))
				gomega.Expect(fakeClusterManagerOVN.eIPC.allocator.cache).ToNot(gomega.HaveKey(node1.Name))
				gomega.Expect(fakeClusterManagerOVN.eIPC.allocator.cache).To(gomega.HaveKey(node2.Name))
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
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("egress node update should not mark the node as reachable if there was no label/readiness change", func() {
			// When an egress node becomes reachable during a node update event and there is no changes to node labels/readiness
			// unassigned egress IP should be eventually added by the periodic reachability check.
			// Test steps:
			//  - disable periodic check from running in background, so it can be called directly from the test
			//  - assign egress IP to an available node
			//  - make the node unreachable and verify that the egress IP was unassigned
			//  - make the node reachable and update a node
			//  - verify that the egress IP was assigned by calling the periodic reachability check
			app.Action = func(ctx *cli.Context) error {
				config.OVNKubernetesFeature.EnableInterconnect = true // no impact on global eIPC functions
				egressIP := "192.168.126.101"
				nodeIPv4 := "192.168.126.51/24"
				node := v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: node1Name,
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\"}", nodeIPv4),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":[\"%s\"]}", v4NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", nodeIPv4),
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
				eIP1 := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{egressIP},
					},
				}
				fakeClusterManagerOVN.start(
					&egressipv1.EgressIPList{
						Items: []egressipv1.EgressIP{eIP1},
					},
					&v1.NodeList{
						Items: []v1.Node{node},
					},
				)

				// Virtually disable background reachability check by using a huge interval
				fakeClusterManagerOVN.eIPC.reachabilityCheckInterval = time.Hour

				_, err := fakeClusterManagerOVN.eIPC.WatchEgressNodes()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(getEgressIPStatusLen(eIP1.Name)).Should(gomega.Equal(1))
				egressIPs, _ := getEgressIPStatus(eIP1.Name)
				gomega.Expect(egressIPs[0]).To(gomega.Equal(egressIP))
				hcClient := fakeClusterManagerOVN.eIPC.allocator.cache[node.Name].healthClient.(*fakeEgressIPHealthClient)
				hcClient.FakeProbeFailure = true
				// explicitly call check reachability, periodic checker is not active
				checkEgressNodesReachabilityIterate(fakeClusterManagerOVN.eIPC)
				gomega.Eventually(getEgressIPStatusLen(eIP1.Name)).Should(gomega.Equal(0))

				hcClient.FakeProbeFailure = false
				node.Annotations["test"] = "dummy"
				_, err = fakeClusterManagerOVN.fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), &node, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(hcClient.IsConnected()).Should(gomega.Equal(true))
				// the node should not be marked as reachable in the update handler as it is not getting added
				gomega.Consistently(func() bool { return fakeClusterManagerOVN.eIPC.allocator.cache[node.Name].isReachable }).Should(gomega.Equal(false))

				// egress IP should get assigned on the next checkEgressNodesReachabilityIterate call
				// explicitly call check reachability, periodic checker is not active
				checkEgressNodesReachabilityIterate(fakeClusterManagerOVN.eIPC)
				// this will trigger an immediate add retry for the node which we need to simulate for this test
				fakeClusterManagerOVN.eIPC.retryEgressNodes.RequestRetryObjs()
				gomega.Eventually(getEgressIPStatusLen(eIP1.Name)).Should(gomega.Equal(1))

				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})

	ginkgo.Context("IPv6 assignment", func() {

		ginkgo.It("should be able to allocate non-conflicting IP on node with lowest amount of allocations", func() {
			app.Action = func(ctx *cli.Context) error {

				egressIP := "0:0:0:0:0:feff:c0a8:8e0f"
				node1IPv4 := ""
				node1IPv6 := "0:0:0:0:0:feff:c0a8:8e0c/64"
				node2IPv4 := ""
				node2IPv6 := "0:0:0:0:0:fedf:c0a8:8e0c/64"

				node1 := v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: node1Name,
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4, node1IPv6),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":[\"%s\", \"%s\"]}", v4NodeSubnet, v6NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node1IPv6),
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
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node2IPv4, node2IPv6),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\": [\"%s\",\"%s\"]}", v4NodeSubnet, v6NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node2IPv6),
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

				fakeClusterManagerOVN.start(&v1.NodeList{
					Items: []v1.Node{node1, node2},
				})

				egressNode1 := setupNode(node1Name, []string{"0:0:0:0:0:feff:c0a8:8e0c/64"}, map[string]string{"0:0:0:0:0:feff:c0a8:8e32": "bogus1", "0:0:0:0:0:feff:c0a8:8e1e": "bogus2"})
				egressNode2 := setupNode(node2Name, []string{"0:0:0:0:0:fedf:c0a8:8e0c/64"}, map[string]string{"0:0:0:0:0:feff:c0a8:8e23": "bogus3"})

				fakeClusterManagerOVN.eIPC.allocator.cache[egressNode1.name] = &egressNode1
				fakeClusterManagerOVN.eIPC.allocator.cache[egressNode2.name] = &egressNode2

				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{egressIP},
					},
				}
				assignedStatuses := fakeClusterManagerOVN.eIPC.assignEgressIPs(eIP.Name, eIP.Spec.EgressIPs)
				gomega.Expect(assignedStatuses).To(gomega.HaveLen(1))
				gomega.Expect(assignedStatuses[0].Node).To(gomega.Equal(egressNode2.name))
				gomega.Expect(assignedStatuses[0].EgressIP).To(gomega.Equal(net.ParseIP(egressIP).String()))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.DescribeTable("should be able to allocate several EgressIPs and avoid the same node", func(egressIP1, egressIP2 string) {
			app.Action = func(ctx *cli.Context) error {
				node1IPv4OVN := ""
				node1IPv6OVN := "0:0:0:0:0:feff:c0a8:8e0c/64"
				node1IPv6SecondaryHost := "0:0:1:0:0:feff:c0a8:8e0c/64"
				node2IPv4OVN := ""
				node2IPv6OVN := "0:0:0:0:0:fedf:c0a8:8e0c/64"
				node2IPv6SecondaryHost := "0:0:1:0:0:fedf:c0a8:8e0c/64"

				node1 := v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: node1Name,
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4OVN, node1IPv6OVN),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":[\"%s\", \"%s\"]}", v4NodeSubnet, v6NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\",\"%s\"]", node1IPv6OVN, node1IPv6SecondaryHost),
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
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node2IPv4OVN, node2IPv6OVN),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\": [\"%s\",\"%s\"]}", v4NodeSubnet, v6NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\",\"%s\"]", node2IPv6OVN, node2IPv6SecondaryHost),
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
						EgressIPs: []string{egressIP1, egressIP2},
					},
				}

				fakeClusterManagerOVN.start(
					&v1.NodeList{Items: []v1.Node{node1, node2}},
					&egressipv1.EgressIPList{Items: []egressipv1.EgressIP{eIP}},
				)

				egressNode1 := setupNode(node1Name, []string{node1IPv6OVN}, map[string]string{"0:0:0:0:0:feff:c0a8:8e32": "bogus1", "0:0:0:0:0:feff:c0a8:8e1e": "bogus2"})
				egressNode2 := setupNode(node2Name, []string{node2IPv6OVN}, map[string]string{"0:0:0:0:0:feff:c0a8:8e23": "bogus3"})

				fakeClusterManagerOVN.eIPC.allocator.cache[egressNode1.name] = &egressNode1
				fakeClusterManagerOVN.eIPC.allocator.cache[egressNode2.name] = &egressNode2

				assignedStatuses := fakeClusterManagerOVN.eIPC.assignEgressIPs(eIP.Name, eIP.Spec.EgressIPs)
				gomega.Expect(assignedStatuses).To(gomega.HaveLen(2))
				gomega.Expect(assignedStatuses[0].Node).To(gomega.Equal(egressNode2.name))
				gomega.Expect(assignedStatuses[0].EgressIP).To(gomega.Equal(net.ParseIP(egressIP1).String()))
				gomega.Expect(assignedStatuses[1].Node).To(gomega.Equal(egressNode1.name))
				gomega.Expect(assignedStatuses[1].EgressIP).To(gomega.Equal(net.ParseIP(egressIP2).String()))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}, ginkgo.Entry("OVN egress IPs", "0:0:0:0:0:feff:c0a8:8e0d", "0:0:0:0:0:feff:c0a8:8e0f"),
			ginkgo.Entry("Secondary host egress IPs", "0:0:1:0:0:fecf:c0a8:8e0d", "0:0:1:0:0:febf:c0a8:8e0f"),
			ginkgo.Entry("OVN and secondary host egress IPs", "0:0:1:0:0:fecf:c0a8:8e0d", "0:0:0:0:0:feff:c0a8:8e0f"))

		ginkgo.DescribeTable("should be able to allocate several EgressIPs and avoid the same node and leave one un-assigned without error", func(egressIP1, egressIP2, egressIP3 string) {
			app.Action = func(ctx *cli.Context) error {
				node1IPv4OVN := ""
				node1IPv6OVN := "0:0:0:0:0:feff:c0a8:8e0c/64"
				node1IPv6SecondaryHost := "0:0:1:0:0:feff:c0a8:8e0c/64"
				node2IPv4OVN := ""
				node2IPv6OVN := "0:0:0:0:0:fedf:c0a8:8e0c/64"
				node2IPv6SecondaryHost := "0:0:1:0:0:fedf:c0a8:8e0c/64"

				node1 := v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: node1Name,
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4OVN, node1IPv6OVN),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":[\"%s\", \"%s\"]}", v4NodeSubnet, v6NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\",\"%s\"]", node1IPv6OVN, node1IPv6SecondaryHost),
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
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node2IPv4OVN, node2IPv6OVN),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\": [\"%s\",\"%s\"]}", v4NodeSubnet, v6NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\",\"%s\"]", node2IPv6OVN, node2IPv6SecondaryHost),
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
						EgressIPs: []string{egressIP1, egressIP2, egressIP3},
					},
				}

				fakeClusterManagerOVN.start(
					&v1.NodeList{Items: []v1.Node{node1, node2}},
					&egressipv1.EgressIPList{Items: []egressipv1.EgressIP{eIP}},
				)

				egressNode1 := setupNode(node1Name, []string{node1IPv4OVN}, map[string]string{"0:0:0:0:0:feff:c0a8:8e32": "bogus1", "0:0:0:0:0:feff:c0a8:8e1e": "bogus2"})
				egressNode2 := setupNode(node2Name, []string{node2IPv4OVN}, map[string]string{"0:0:0:0:0:feff:c0a8:8e23": "bogus3"})

				fakeClusterManagerOVN.eIPC.allocator.cache[egressNode1.name] = &egressNode1
				fakeClusterManagerOVN.eIPC.allocator.cache[egressNode2.name] = &egressNode2

				gomega.Expect(fakeClusterManagerOVN.eIPC.initEgressIPAllocator(&node1)).To(gomega.Succeed())
				gomega.Expect(fakeClusterManagerOVN.eIPC.initEgressIPAllocator(&node2)).To(gomega.Succeed())
				assignedStatuses := fakeClusterManagerOVN.eIPC.assignEgressIPs(eIP.Name, eIP.Spec.EgressIPs)
				gomega.Expect(assignedStatuses).To(gomega.HaveLen(2))
				gomega.Expect(assignedStatuses[0].Node).To(gomega.Equal(egressNode2.name))
				gomega.Expect(assignedStatuses[0].EgressIP).To(gomega.Equal(net.ParseIP(egressIP1).String()))
				gomega.Expect(assignedStatuses[1].Node).To(gomega.Equal(egressNode1.name))
				gomega.Expect(assignedStatuses[1].EgressIP).To(gomega.Equal(net.ParseIP(egressIP2).String()))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}, ginkgo.Entry("OVN network", "0:0:0:0:0:feff:c0a8:8e0d", "0:0:0:0:0:feff:c0a8:8e0e", "0:0:0:0:0:feff:c0a8:8e0f"),
			ginkgo.Entry("Secondary host network", "0:0:1:0:0:feff:c0a8:8e0d", "0:0:1:0:0:feff:c0a8:8e0e", "0:0:1:0:0:feff:c0a8:8e0f"),
		)

		ginkgo.It("should be able to allocate several EgressIPs for OVN and Secondary host networks and avoid the same node and leave multiple EgressIPs un-assigned without error", func() {
			app.Action = func(ctx *cli.Context) error {
				egressIP1SecondaryHost := "0:0:1:0:0:feff:c0a8:8e0d"
				egressIP2SecondaryHost := "0:0:1:0:0:feff:c0a8:8e0e"
				egressIP3SecondaryHost := "0:0:1:0:0:feff:c0a8:8e0f"
				egressIP4OVN := "0:0:0:0:0:feff:c0a8:8e1a"
				egressIP5OVN := "0:0:0:0:0:feff:c0a8:8e1b"

				node1IPv4OVN := ""
				node1IPv6OVN := "0:0:0:0:0:feff:c0a8:8e0c/64"
				node1IPv6SecondaryHost := "0:0:1:0:0:feff:c0a8:8e0c/64"
				node2IPv4OVN := ""
				node2IPv6OVN := "0:0:0:0:0:feff:c0a8:8e0b/64"
				node2IPv6SecondaryHost := "0:0:1:0:0:feff:c0a8:8e0b/64"

				node1 := v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: node1Name,
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4OVN, node1IPv6OVN),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":[\"%s\", \"%s\"]}", v4NodeSubnet, v6NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\",\"%s\"]", node1IPv6OVN, node1IPv6SecondaryHost),
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
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node2IPv4OVN, node2IPv6OVN),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\": [\"%s\",\"%s\"]}", v4NodeSubnet, v6NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\",\"%s\"]", node2IPv6OVN, node2IPv6SecondaryHost),
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
						EgressIPs: []string{egressIP1SecondaryHost, egressIP2SecondaryHost, egressIP3SecondaryHost, egressIP4OVN, egressIP5OVN},
					},
				}

				fakeClusterManagerOVN.start(
					&v1.NodeList{Items: []v1.Node{node1, node2}},
					&egressipv1.EgressIPList{Items: []egressipv1.EgressIP{eIP}},
				)

				egressNode1 := setupNode(node1Name, []string{node1IPv4OVN}, map[string]string{"0:0:0:0:0:feff:c0a8:8e32": "bogus1", "0:0:0:0:0:feff:c0a8:8e1e": "bogus2"})
				egressNode2 := setupNode(node2Name, []string{node2IPv4OVN}, map[string]string{"0:0:0:0:0:feff:c0a8:8e23": "bogus3"})

				fakeClusterManagerOVN.eIPC.allocator.cache[egressNode1.name] = &egressNode1
				fakeClusterManagerOVN.eIPC.allocator.cache[egressNode2.name] = &egressNode2

				gomega.Expect(fakeClusterManagerOVN.eIPC.initEgressIPAllocator(&node1)).To(gomega.Succeed())
				gomega.Expect(fakeClusterManagerOVN.eIPC.initEgressIPAllocator(&node2)).To(gomega.Succeed())
				assignedStatuses := fakeClusterManagerOVN.eIPC.assignEgressIPs(eIP.Name, eIP.Spec.EgressIPs)
				gomega.Expect(assignedStatuses).To(gomega.HaveLen(2))
				gomega.Expect(assignedStatuses[0].Node).To(gomega.Equal(egressNode2.name))
				gomega.Expect(assignedStatuses[0].EgressIP).To(gomega.Equal(net.ParseIP(egressIP1SecondaryHost).String()))
				gomega.Expect(assignedStatuses[1].Node).To(gomega.Equal(egressNode1.name))
				gomega.Expect(assignedStatuses[1].EgressIP).To(gomega.Equal(net.ParseIP(egressIP2SecondaryHost).String()))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.DescribeTable("should return the already allocated IP with the same node if it is allocated again", func(egressIP string) {
			app.Action = func(ctx *cli.Context) error {
				node1IPv4OVN := ""
				node1IPv6OVN := "0:0:0:0:0:feff:c0a8:8e0c/64"
				node1IPv6SecondaryHost := "0:0:1:0:0:feff:c0a8:8e0c/64"
				node2IPv4OVN := ""
				node2IPv6OVN := "0:0:0:0:0:feff:c0a8:8e0b/64"
				node2IPv6SecondaryHost := "0:0:1:0:0:feff:c0a8:8e0b/64"

				node1 := v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: node1Name,
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4OVN, node1IPv6OVN),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":[\"%s\", \"%s\"]}", v4NodeSubnet, v6NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\",\"%s\"]", node1IPv6OVN, node1IPv6SecondaryHost),
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
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node2IPv4OVN, node2IPv6OVN),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\": [\"%s\",\"%s\"]}", v4NodeSubnet, v6NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\",\"%s\"]", node2IPv6OVN, node2IPv6SecondaryHost),
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
				}

				fakeClusterManagerOVN.start(
					&v1.NodeList{Items: []v1.Node{node1, node2}},
					&egressipv1.EgressIPList{Items: []egressipv1.EgressIP{eIP}},
				)

				egressNode1 := setupNode(node1Name, []string{node1IPv4OVN}, map[string]string{"0:0:0:0:0:feff:c0a8:8e32": "bogus1", "0:0:0:0:0:feff:c0a8:8e1e": "bogus2"})
				egressNode2 := setupNode(node2Name, []string{node2IPv4OVN}, map[string]string{"0:0:0:0:0:feff:c0a8:8e23": "bogus3"})

				fakeClusterManagerOVN.eIPC.allocator.cache[egressNode1.name] = &egressNode1
				fakeClusterManagerOVN.eIPC.allocator.cache[egressNode2.name] = &egressNode2

				gomega.Expect(fakeClusterManagerOVN.eIPC.initEgressIPAllocator(&node1)).To(gomega.Succeed())
				gomega.Expect(fakeClusterManagerOVN.eIPC.initEgressIPAllocator(&node2)).To(gomega.Succeed())
				assignedStatuses := fakeClusterManagerOVN.eIPC.assignEgressIPs(eIP.Name, eIP.Spec.EgressIPs)
				gomega.Expect(assignedStatuses).To(gomega.HaveLen(1))
				gomega.Expect(assignedStatuses[0].Node).To(gomega.Equal(node2Name))
				assignedStatuses = fakeClusterManagerOVN.eIPC.assignEgressIPs(eIP.Name, eIP.Spec.EgressIPs)
				gomega.Expect(assignedStatuses).To(gomega.HaveLen(1))
				gomega.Expect(assignedStatuses[0].Node).To(gomega.Equal(node2Name))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}, ginkgo.Entry("OVN network", "0:0:0:0:0:feff:c0a8:8e1a"),
			ginkgo.Entry("Secondary host network", "0:0:1:0:0:feff:c0a8:8e0d"),
		)

		ginkgo.It("should not be able to allocate node IP", func() {
			app.Action = func(ctx *cli.Context) error {

				egressIP := "0:0:0:0:0:feff:c0a8:8e0c"
				node1IPv4 := ""
				node1IPv6 := egressIP + "/64"
				node2IPv4 := ""
				node2IPv6 := "0:0:0:0:0:fedf:c0a8:8e0c/64"

				node1 := v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: node1Name,
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4, node1IPv6),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":[\"%s\", \"%s\"]}", v4NodeSubnet, v6NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node1IPv6),
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
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node2IPv4, node2IPv6),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\": [\"%s\",\"%s\"]}", v4NodeSubnet, v6NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node2IPv6),
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
				}

				fakeClusterManagerOVN.start(
					&v1.NodeList{Items: []v1.Node{node1, node2}},
					&egressipv1.EgressIPList{Items: []egressipv1.EgressIP{eIP}},
				)

				egressNode1 := setupNode(node1Name, []string{egressIP + "/64"}, map[string]string{"0:0:0:0:0:feff:c0a8:8e32": "bogus1", "0:0:0:0:0:feff:c0a8:8e1e": "bogus2"})
				egressNode2 := setupNode(node2Name, []string{"0:0:0:0:0:fedf:c0a8:8e0c/64"}, map[string]string{"0:0:0:0:0:feff:c0a8:8e23": "bogus3"})

				fakeClusterManagerOVN.eIPC.allocator.cache[egressNode1.name] = &egressNode1
				fakeClusterManagerOVN.eIPC.allocator.cache[egressNode2.name] = &egressNode2

				assignedStatuses := fakeClusterManagerOVN.eIPC.assignEgressIPs(eIP.Name, eIP.Spec.EgressIPs)
				gomega.Expect(assignedStatuses).To(gomega.HaveLen(0))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should not be able to allocate EgressIP equal to an address in host-cidrs annotation", func() {
			app.Action = func(ctx *cli.Context) error {

				egressIP := "0:0:0:0:0:feff:c0a8:8e0a"
				node1IPv4 := ""
				node1IPv6 := "0:0:0:0:0:fedf:c0a8:8e0b/64"
				node1IPv62 := "0:0:0:0:0:feff:c0a8:8e0a/64" // same addr as EIP
				node2IPv4 := ""
				node2IPv6 := "0:0:0:0:0:fedf:c0a8:8e0c/64"

				node1 := v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: node1Name,
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4, node1IPv6),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":[\"%s\", \"%s\"]}", v4NodeSubnet, v6NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\",\"%s\"]", node1IPv6, node1IPv62),
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
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node2IPv4, node2IPv6),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\": [\"%s\",\"%s\"]}", v4NodeSubnet, v6NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node2IPv6),
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
				}

				fakeClusterManagerOVN.start(
					&v1.NodeList{Items: []v1.Node{node1, node2}},
					&egressipv1.EgressIPList{Items: []egressipv1.EgressIP{eIP}},
				)

				egressNode1 := setupNode(node1Name, []string{node1IPv6, node1IPv62}, map[string]string{"0:0:0:0:0:feff:c0a8:8e32": "bogus1", "0:0:0:0:0:feff:c0a8:8e1e": "bogus2"})
				egressNode2 := setupNode(node2Name, []string{node2IPv6}, map[string]string{"0:0:0:0:0:feff:c0a8:8e23": "bogus3"})

				fakeClusterManagerOVN.eIPC.allocator.cache[egressNode1.name] = &egressNode1
				fakeClusterManagerOVN.eIPC.allocator.cache[egressNode2.name] = &egressNode2

				gomega.Expect(fakeClusterManagerOVN.eIPC.initEgressIPAllocator(&node1)).To(gomega.Succeed())
				gomega.Expect(fakeClusterManagerOVN.eIPC.initEgressIPAllocator(&node2)).To(gomega.Succeed())
				assignedStatuses := fakeClusterManagerOVN.eIPC.assignEgressIPs(eIP.Name, eIP.Spec.EgressIPs)
				gomega.Expect(assignedStatuses).To(gomega.HaveLen(0))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should not be able to allocate conflicting compressed IP", func() {
			app.Action = func(ctx *cli.Context) error {
				egressIP := "::feff:c0a8:8e32"
				node1IPv4 := ""
				node1IPv6 := "0:0:0:0:0:feff:c0a8:8e0c/64"
				node2IPv4 := ""
				node2IPv6 := "0:0:0:0:0:fedf:c0a8:8e0c/64"

				node1 := v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: node1Name,
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4, node1IPv6),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":[\"%s\", \"%s\"]}", v4NodeSubnet, v6NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node1IPv6),
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
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node2IPv4, node2IPv6),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\": [\"%s\",\"%s\"]}", v4NodeSubnet, v6NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node2IPv6),
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
				}

				fakeClusterManagerOVN.start(
					&v1.NodeList{Items: []v1.Node{node1, node2}},
					&egressipv1.EgressIPList{Items: []egressipv1.EgressIP{eIP}},
				)

				egressNode1 := setupNode(node1Name, []string{"0:0:0:0:0:feff:c0a8:8e0c/64"}, map[string]string{"0:0:0:0:0:feff:c0a8:8e32": "bogus1", "0:0:0:0:0:feff:c0a8:8e1e": "bogus2"})
				egressNode2 := setupNode(node2Name, []string{"0:0:0:0:0:fedf:c0a8:8e0c/64"}, map[string]string{"0:0:0:0:0:feff:c0a8:8e23": "bogus3"})

				fakeClusterManagerOVN.eIPC.allocator.cache[egressNode1.name] = &egressNode1
				fakeClusterManagerOVN.eIPC.allocator.cache[egressNode2.name] = &egressNode2

				assignedStatuses := fakeClusterManagerOVN.eIPC.assignEgressIPs(eIP.Name, eIP.Spec.EgressIPs)
				gomega.Expect(assignedStatuses).To(gomega.HaveLen(0))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should not be able to allocate IPv4 IP on nodes which can only host IPv6", func() {
			app.Action = func(ctx *cli.Context) error {

				egressIP := "192.168.126.16"
				node1IPv4 := ""
				node1IPv6 := "0:0:0:0:0:feff:c0a8:8e0c/64"
				node2IPv4 := ""
				node2IPv6 := "0:0:0:0:0:fedf:c0a8:8e0c/64"

				node1 := v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: node1Name,
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4, node1IPv6),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":[\"%s\", \"%s\"]}", v4NodeSubnet, v6NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node1IPv6),
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
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node2IPv4, node2IPv6),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\": [\"%s\",\"%s\"]}", v4NodeSubnet, v6NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node2IPv6),
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
				}

				fakeClusterManagerOVN.start(
					&v1.NodeList{Items: []v1.Node{node1, node2}},
					&egressipv1.EgressIPList{Items: []egressipv1.EgressIP{eIP}},
				)

				egressNode1 := setupNode(node1Name, []string{"0:0:0:0:0:feff:c0a8:8e0c/64"}, map[string]string{"0:0:0:0:0:feff:c0a8:8e32": "bogus1", "0:0:0:0:0:feff:c0a8:8e1e": "bogus2"})
				egressNode2 := setupNode(node2Name, []string{"0:0:0:0:0:fedf:c0a8:8e0c/64"}, map[string]string{"0:0:0:0:0:feff:c0a8:8e23": "bogus3"})

				fakeClusterManagerOVN.eIPC.allocator.cache[egressNode1.name] = &egressNode1
				fakeClusterManagerOVN.eIPC.allocator.cache[egressNode2.name] = &egressNode2

				assignedStatuses := fakeClusterManagerOVN.eIPC.assignEgressIPs(eIP.Name, eIP.Spec.EgressIPs)
				gomega.Expect(assignedStatuses).To(gomega.HaveLen(0))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should be able to allocate non-conflicting compressed uppercase IP", func() {
			app.Action = func(ctx *cli.Context) error {

				egressIP := "::FEFF:C0A8:8D32"
				node1IPv4 := ""
				node1IPv6 := "0:0:0:0:0:feff:c0a8:8e0c/64"
				node2IPv4 := ""
				node2IPv6 := "0:0:0:0:0:fedf:c0a8:8e0c/64"

				node1 := v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: node1Name,
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4, node1IPv6),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":[\"%s\", \"%s\"]}", v4NodeSubnet, v6NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node1IPv6),
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
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node2IPv4, node2IPv6),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\": [\"%s\",\"%s\"]}", v4NodeSubnet, v6NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node2IPv6),
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
				}

				fakeClusterManagerOVN.start(
					&v1.NodeList{Items: []v1.Node{node1, node2}},
					&egressipv1.EgressIPList{Items: []egressipv1.EgressIP{eIP}},
				)

				egressNode1 := setupNode(node1Name, []string{"0:0:0:0:0:feff:c0a8:8e0c/64"}, map[string]string{"0:0:0:0:0:feff:c0a8:8e32": "bogus1", "0:0:0:0:0:feff:c0a8:8e1e": "bogus2"})
				egressNode2 := setupNode(node2Name, []string{"0:0:0:0:0:fedf:c0a8:8e0c/64"}, map[string]string{"0:0:0:0:0:feff:c0a8:8e23": "bogus3"})

				fakeClusterManagerOVN.eIPC.allocator.cache[egressNode1.name] = &egressNode1
				fakeClusterManagerOVN.eIPC.allocator.cache[egressNode2.name] = &egressNode2

				assignedStatuses := fakeClusterManagerOVN.eIPC.assignEgressIPs(eIP.Name, eIP.Spec.EgressIPs)
				gomega.Expect(assignedStatuses).To(gomega.HaveLen(1))
				gomega.Expect(assignedStatuses[0].Node).To(gomega.Equal(egressNode2.name))
				gomega.Expect(assignedStatuses[0].EgressIP).To(gomega.Equal(net.ParseIP(egressIP).String()))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should not be able to allocate conflicting compressed uppercase IP", func() {
			app.Action = func(ctx *cli.Context) error {

				egressIP := "::FEFF:C0A8:8E32"
				node1IPv4 := ""
				node1IPv6 := "0:0:0:0:0:feff:c0a8:8e0c/64"
				node2IPv4 := ""
				node2IPv6 := "0:0:0:0:0:fedf:c0a8:8e0c/64"

				node1 := v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: node1Name,
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4, node1IPv6),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":[\"%s\", \"%s\"]}", v4NodeSubnet, v6NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node1IPv6),
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
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node2IPv4, node2IPv6),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\": [\"%s\",\"%s\"]}", v4NodeSubnet, v6NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node2IPv6),
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
				}

				fakeClusterManagerOVN.start(
					&v1.NodeList{Items: []v1.Node{node1, node2}},
					&egressipv1.EgressIPList{Items: []egressipv1.EgressIP{eIP}},
				)

				egressNode1 := setupNode(node1Name, []string{"0:0:0:0:0:feff:c0a8:8e0c/64"}, map[string]string{"0:0:0:0:0:feff:c0a8:8e32": "bogus1", "0:0:0:0:0:feff:c0a8:8e1e": "bogus2"})
				egressNode2 := setupNode(node2Name, []string{"0:0:0:0:0:fedf:c0a8:8e0c/64"}, map[string]string{"0:0:0:0:0:feff:c0a8:8e23": "bogus3"})

				fakeClusterManagerOVN.eIPC.allocator.cache[egressNode1.name] = &egressNode1
				fakeClusterManagerOVN.eIPC.allocator.cache[egressNode2.name] = &egressNode2

				assignedStatuses := fakeClusterManagerOVN.eIPC.assignEgressIPs(eIP.Name, eIP.Spec.EgressIPs)
				gomega.Expect(assignedStatuses).To(gomega.HaveLen(0))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should not be able to allocate invalid IP", func() {
			app.Action = func(ctx *cli.Context) error {

				egressIPs := []string{"0:0:0:0:0:feff:c0a8:8e32:5"}
				node1IPv4 := ""
				node1IPv6 := "0:0:0:0:0:feff:c0a8:8e0c/64"
				node2IPv4 := ""
				node2IPv6 := "0:0:0:0:0:fedf:c0a8:8e0c/64"

				node1 := v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: node1Name,
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4, node1IPv6),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":[\"%s\", \"%s\"]}", v4NodeSubnet, v6NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node1IPv6),
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
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node2IPv4, node2IPv6),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\": [\"%s\",\"%s\"]}", v4NodeSubnet, v6NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node2IPv6),
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
						EgressIPs: egressIPs,
					},
				}

				fakeClusterManagerOVN.start(
					&v1.NodeList{Items: []v1.Node{node1, node2}},
					&egressipv1.EgressIPList{Items: []egressipv1.EgressIP{eIP}},
				)

				assignedStatuses, err := fakeClusterManagerOVN.eIPC.validateEgressIPSpec(eIP.Name, eIP.Spec.EgressIPs)
				gomega.Expect(err).To(gomega.HaveOccurred())
				gomega.Expect(err.Error()).To(gomega.Equal(fmt.Sprintf("unable to parse provided EgressIP: %s, invalid", egressIPs[0])))
				gomega.Expect(assignedStatuses).To(gomega.HaveLen(0))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})

	ginkgo.Context("Dual-stack assignment", func() {

		ginkgo.It("should be able to allocate non-conflicting IPv4 on node which can host it, even if it happens to be the node with more assignments", func() {
			app.Action = func(ctx *cli.Context) error {
				egressIP := "192.168.126.99"
				node1IPv4 := ""
				node1IPv6 := "0:0:0:0:0:feff:c0a8:8e0c/64"
				node2IPv4 := "192.168.126.51/24"
				node2IPv6 := ""

				node1 := v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: node1Name,
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4, node1IPv6),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":[\"%s\", \"%s\"]}", v4NodeSubnet, v6NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node1IPv6),
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
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node2IPv4, node2IPv6),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\": [\"%s\",\"%s\"]}", v4NodeSubnet, v6NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node2IPv4),
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
				}

				fakeClusterManagerOVN.start(
					&v1.NodeList{Items: []v1.Node{node1, node2}},
					&egressipv1.EgressIPList{Items: []egressipv1.EgressIP{eIP}},
				)

				egressNode1 := setupNode(node1Name, []string{"0:0:0:0:0:feff:c0a8:8e0c/64"}, map[string]string{"0:0:0:0:0:feff:c0a8:8e23": "bogus1"})
				egressNode2 := setupNode(node2Name, []string{"192.168.126.51/24"}, map[string]string{"192.168.126.68": "bogus1", "192.168.126.102": "bogus2"})

				fakeClusterManagerOVN.eIPC.allocator.cache[egressNode1.name] = &egressNode1
				fakeClusterManagerOVN.eIPC.allocator.cache[egressNode2.name] = &egressNode2

				assignedStatuses := fakeClusterManagerOVN.eIPC.assignEgressIPs(eIP.Name, eIP.Spec.EgressIPs)
				gomega.Expect(assignedStatuses).To(gomega.HaveLen(1))
				gomega.Expect(assignedStatuses[0].Node).To(gomega.Equal(egressNode2.name))
				gomega.Expect(assignedStatuses[0].EgressIP).To(gomega.Equal(net.ParseIP(egressIP).String()))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})

	ginkgo.Context("IPv4 assignment", func() {

		ginkgo.It("Should not be able to assign egress IP defined in CIDR notation", func() {
			app.Action = func(ctx *cli.Context) error {

				egressIPs := []string{"192.168.126.99/32"}
				node1IPv4 := "192.168.126.12/24"
				node2IPv4 := "192.168.126.51/24"

				node1 := v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: node1Name,
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4, ""),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":[\"%s\", \"%s\"]}", v4NodeSubnet, v6NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node1IPv4),
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
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\": [\"%s\",\"%s\"]}", v4NodeSubnet, v6NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node2IPv4),
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
						EgressIPs: egressIPs,
					},
				}

				fakeClusterManagerOVN.start(
					&v1.NodeList{Items: []v1.Node{node1, node2}},
					&egressipv1.EgressIPList{Items: []egressipv1.EgressIP{eIP}},
				)

				egressNode1 := setupNode(node1Name, []string{node1IPv4}, map[string]string{"192.168.126.102": "bogus1", "192.168.126.111": "bogus2"})
				egressNode2 := setupNode(node2Name, []string{node2IPv4}, map[string]string{"192.168.126.68": "bogus3"})

				fakeClusterManagerOVN.eIPC.allocator.cache[egressNode1.name] = &egressNode1
				fakeClusterManagerOVN.eIPC.allocator.cache[egressNode2.name] = &egressNode2

				validatedIPs, err := fakeClusterManagerOVN.eIPC.validateEgressIPSpec(eIP.Name, eIP.Spec.EgressIPs)
				gomega.Expect(err).To(gomega.HaveOccurred())
				gomega.Expect(err.Error()).To(gomega.Equal(fmt.Sprintf("unable to parse provided EgressIP: %s, invalid", egressIPs[0])))
				gomega.Expect(validatedIPs).To(gomega.HaveLen(0))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

	})

	ginkgo.Context("WatchEgressIP", func() {

		ginkgo.It("should update status correctly for single-stack IPv4", func() {
			app.Action = func(ctx *cli.Context) error {
				egressIP := "192.168.126.10"
				node1IPv4 := "192.168.126.12/24"
				node2IPv4 := "192.168.126.51/24"

				node1 := v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: node1Name,
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4, ""),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":[\"%s\", \"%s\"]}", v4NodeSubnet, v6NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node1IPv4),
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
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\": [\"%s\",\"%s\"]}", v4NodeSubnet, v6NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node2IPv4),
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
						NamespaceSelector: metav1.LabelSelector{
							MatchLabels: map[string]string{
								"name": "does-not-exist",
							},
						},
					},
				}

				fakeClusterManagerOVN.start(
					&v1.NodeList{Items: []v1.Node{node1, node2}},
					&egressipv1.EgressIPList{Items: []egressipv1.EgressIP{eIP}},
				)

				egressNode1 := setupNode(node1Name, []string{node1IPv4}, map[string]string{"192.168.126.102": "bogus1", "192.168.126.111": "bogus2"})
				egressNode2 := setupNode(node2Name, []string{node2IPv4}, map[string]string{"192.168.126.68": "bogus3"})

				fakeClusterManagerOVN.eIPC.allocator.cache[egressNode1.name] = &egressNode1
				fakeClusterManagerOVN.eIPC.allocator.cache[egressNode2.name] = &egressNode2

				_, err := fakeClusterManagerOVN.eIPC.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
				egressIPs, nodes := getEgressIPStatus(egressIPName)
				gomega.Expect(nodes[0]).To(gomega.Equal(egressNode2.name))
				gomega.Expect(egressIPs[0]).To(gomega.Equal(egressIP))
				gomega.Expect(egressIPs[0]).To(gomega.Equal(egressIP))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should update status correctly for single-stack IPv6", func() {
			app.Action = func(ctx *cli.Context) error {
				egressIP := "0:0:0:0:0:feff:c0a8:8e0d"
				node1IPv4 := ""
				node1IPv6 := "0:0:0:0:0:feff:c0a8:8e0c/64"
				node2IPv4 := ""
				node2IPv6 := "0:0:0:0:0:fedf:c0a8:8e0c/64"

				node1 := v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: node1Name,
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4, node1IPv6),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":[\"%s\", \"%s\"]}", v4NodeSubnet, v6NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node1IPv6),
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
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node2IPv4, node2IPv6),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\": [\"%s\",\"%s\"]}", v4NodeSubnet, v6NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node2IPv6),
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
				}

				fakeClusterManagerOVN.start(
					&v1.NodeList{Items: []v1.Node{node1, node2}},
					&egressipv1.EgressIPList{Items: []egressipv1.EgressIP{eIP}},
				)

				egressNode1 := setupNode(node1Name, []string{"0:0:0:0:0:feff:c0a8:8e0c/64"}, map[string]string{"0:0:0:0:0:feff:c0a8:8e32": "bogus1", "0:0:0:0:0:feff:c0a8:8e1e": "bogus2"})
				egressNode2 := setupNode(node2Name, []string{"0:0:0:0:0:fedf:c0a8:8e0c/64"}, map[string]string{"0:0:0:0:0:feff:c0a8:8e23": "bogus3"})

				fakeClusterManagerOVN.eIPC.allocator.cache[egressNode1.name] = &egressNode1
				fakeClusterManagerOVN.eIPC.allocator.cache[egressNode2.name] = &egressNode2

				_, err := fakeClusterManagerOVN.eIPC.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
				egressIPs, nodes := getEgressIPStatus(egressIPName)
				gomega.Expect(nodes[0]).To(gomega.Equal(egressNode2.name))
				gomega.Expect(egressIPs[0]).To(gomega.Equal(net.ParseIP(egressIP).String()))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should update status correctly for dual-stack", func() {
			app.Action = func(ctx *cli.Context) error {
				egressIPv4 := "192.168.126.101"
				egressIPv6 := "::feff:c0a8:8e0d"
				node1IPv6 := "::feff:c0a8:8e0c/64"
				node2IPv4 := "192.168.126.51/24"

				node1 := v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: node1Name,
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", "", node1IPv6),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":[\"%s\", \"%s\"]}", v4NodeSubnet, v6NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node1IPv6),
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
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\": [\"%s\",\"%s\"]}", v4NodeSubnet, v6NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node2IPv4),
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
						EgressIPs: []string{egressIPv4, egressIPv6},
					},
				}

				fakeClusterManagerOVN.start(
					&v1.NodeList{Items: []v1.Node{node1, node2}},
					&egressipv1.EgressIPList{Items: []egressipv1.EgressIP{eIP}},
				)

				egressNode1 := setupNode(node1Name, []string{node1IPv6}, map[string]string{"0:0:0:0:0:feff:c0a8:8e23": "bogus1"})
				egressNode2 := setupNode(node2Name, []string{node2IPv4}, map[string]string{"192.168.126.68": "bogus2", "192.168.126.102": "bogus3"})

				fakeClusterManagerOVN.eIPC.allocator.cache[egressNode1.name] = &egressNode1
				fakeClusterManagerOVN.eIPC.allocator.cache[egressNode2.name] = &egressNode2

				_, err := fakeClusterManagerOVN.eIPC.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(2))
				egressIPs, nodes := getEgressIPStatus(egressIPName)
				gomega.Expect(nodes).To(gomega.ConsistOf(egressNode2.name, egressNode1.name))
				gomega.Expect(egressIPs).To(gomega.ConsistOf(net.ParseIP(egressIPv6).String(), net.ParseIP(egressIPv4).String()))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})

	ginkgo.Context("syncEgressIP for dual-stack", func() {

		ginkgo.It("should not update valid assignments", func() {
			// FIXME(mk): this test doesn't ensure that the EIP status does not get patched during the test run
			// and therefore is an invalid test to test that the status is not patched
			app.Action = func(ctx *cli.Context) error {
				config.IPv6Mode = true
				config.IPv4Mode = true
				egressIPv4 := "192.168.126.101"
				egressIPv6 := "0:0:0:0:0:feff:c0a8:8e0d"
				node1IPv6 := "0:0:0:0:0:feff:c0a8:8e0c/64"
				node2IPv4 := "192.168.126.51/16"

				node1 := v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: node1Name,
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", "", node1IPv6),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":[\"%s\", \"%s\"]}", v4NodeSubnet, v6NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node1IPv6),
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
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\": [\"%s\",\"%s\"]}", v4NodeSubnet, v6NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node2IPv4),
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

				egressNode1 := setupNode(node1Name, []string{node1IPv6}, map[string]string{})
				egressNode2 := setupNode(node2Name, []string{node2IPv4}, map[string]string{"192.168.126.102": "bogus3"})

				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{egressIPv4, egressIPv6},
					},
					Status: egressipv1.EgressIPStatus{
						Items: []egressipv1.EgressIPStatusItem{
							{
								EgressIP: egressIPv4,
								Node:     egressNode2.name,
							},
							{
								EgressIP: net.ParseIP(egressIPv6).String(),
								Node:     egressNode1.name,
							},
						},
					},
				}

				fakeClusterManagerOVN.start(
					&v1.NodeList{Items: []v1.Node{node1, node2}},
					&egressipv1.EgressIPList{Items: []egressipv1.EgressIP{eIP}},
				)

				fakeClusterManagerOVN.eIPC.allocator.cache[egressNode1.name] = &egressNode1
				fakeClusterManagerOVN.eIPC.allocator.cache[egressNode2.name] = &egressNode2

				_, err := fakeClusterManagerOVN.eIPC.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(2))
				egressIPs, nodes := getEgressIPStatus(egressIPName)
				gomega.Expect(nodes).To(gomega.ConsistOf(eIP.Status.Items[0].Node, eIP.Status.Items[1].Node))
				gomega.Expect(egressIPs).To(gomega.ConsistOf(eIP.Status.Items[0].EgressIP, eIP.Status.Items[1].EgressIP))
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
				node1IPv4 := "192.168.126.12/24"
				node1IPv6 := ""
				node2IPv4 := "192.168.126.51/24"
				node2IPv6 := ""

				node1 := v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: node1Name,
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4, node1IPv6),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":[\"%s\", \"%s\"]}", v4NodeSubnet, v6NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node1IPv4),
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
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node2IPv4, node2IPv6),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\": [\"%s\",\"%s\"]}", v4NodeSubnet, v6NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node2IPv4),
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

				egressNode1 := setupNode(node1Name, []string{"192.168.126.12/24"}, map[string]string{egressIP1: egressIPName, egressIP2: egressIPName})
				egressNode2 := setupNode(node2Name, []string{"192.168.126.51/24"}, map[string]string{"192.168.126.68": "bogus3"})

				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{egressIP1, egressIP2},
					},
					Status: egressipv1.EgressIPStatus{
						Items: []egressipv1.EgressIPStatusItem{
							{
								EgressIP: egressIP1,
								Node:     egressNode1.name,
							},
							{
								EgressIP: egressIP2,
								Node:     egressNode1.name,
							},
						},
					},
				}

				fakeClusterManagerOVN.start(
					&v1.NodeList{Items: []v1.Node{node1, node2}},
					&egressipv1.EgressIPList{Items: []egressipv1.EgressIP{eIP}},
				)

				fakeClusterManagerOVN.eIPC.allocator.cache[egressNode1.name] = &egressNode1
				fakeClusterManagerOVN.eIPC.allocator.cache[egressNode2.name] = &egressNode2

				_, err := fakeClusterManagerOVN.eIPC.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(2))
				egressIPs, nodes := getEgressIPStatus(egressIPName)
				gomega.Expect(nodes).To(gomega.ConsistOf(egressNode1.name, egressNode2.name))
				gomega.Expect(egressIPs).To(gomega.ConsistOf(eIP.Status.Items[0].EgressIP, eIP.Status.Items[1].EgressIP))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should update invalid assignments with incorrectly parsed IP", func() {
			app.Action = func(ctx *cli.Context) error {
				egressIP1 := "192.168.126.101"
				egressIPIncorrect := "192.168.126.1000"
				node1IPv4 := "192.168.126.12/24"
				node2IPv4 := "192.168.126.51/24"

				node1 := v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: node1Name,
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4, ""),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":[\"%s\", \"%s\"]}", v4NodeSubnet, v6NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node1IPv4),
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
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\": [\"%s\",\"%s\"]}", v4NodeSubnet, v6NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node2IPv4),
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

				egressNode1 := setupNode(node1Name, []string{"192.168.126.12/24"}, map[string]string{"192.168.126.102": "bogus1", "192.168.126.111": "bogus2"})
				egressNode2 := setupNode(node2Name, []string{"192.168.126.51/24"}, map[string]string{"192.168.126.68": "bogus3"})

				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{egressIP1},
					},
					Status: egressipv1.EgressIPStatus{
						Items: []egressipv1.EgressIPStatusItem{
							{
								EgressIP: egressIPIncorrect,
								Node:     egressNode1.name,
							},
						},
					},
				}

				fakeClusterManagerOVN.start(
					&v1.NodeList{Items: []v1.Node{node1, node2}},
					&egressipv1.EgressIPList{Items: []egressipv1.EgressIP{eIP}},
				)

				fakeClusterManagerOVN.eIPC.allocator.cache[egressNode1.name] = &egressNode1
				fakeClusterManagerOVN.eIPC.allocator.cache[egressNode2.name] = &egressNode2

				_, err := fakeClusterManagerOVN.eIPC.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
				egressIPs, nodes := getEgressIPStatus(egressIPName)
				gomega.Expect(nodes[0]).To(gomega.Equal(egressNode2.name))
				gomega.Expect(egressIPs[0]).To(gomega.Equal(egressIP1))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should update invalid assignments with unhostable IP on a node", func() {
			app.Action = func(ctx *cli.Context) error {
				egressIP1 := "192.168.126.101"
				egressIPIncorrect := "192.168.128.100"
				node1IPv4 := "192.168.126.12/24"
				node2IPv4 := "192.168.126.51/24"

				node1 := v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: node1Name,
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4, ""),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":[\"%s\", \"%s\"]}", v4NodeSubnet, v6NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node1IPv4),
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
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\": [\"%s\",\"%s\"]}", v4NodeSubnet, v6NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node2IPv4),
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

				egressNode1 := setupNode(node1Name, []string{"192.168.126.12/24"}, map[string]string{"192.168.126.102": "bogus1", "192.168.126.111": "bogus2"})
				egressNode2 := setupNode(node2Name, []string{"192.168.126.51/24"}, map[string]string{"192.168.126.68": "bogus3"})

				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{egressIP1},
					},
					Status: egressipv1.EgressIPStatus{
						Items: []egressipv1.EgressIPStatusItem{
							{
								EgressIP: egressIPIncorrect,
								Node:     egressNode1.name,
							},
						},
					},
				}

				fakeClusterManagerOVN.start(
					&v1.NodeList{Items: []v1.Node{node1, node2}},
					&egressipv1.EgressIPList{Items: []egressipv1.EgressIP{eIP}},
				)

				fakeClusterManagerOVN.eIPC.allocator.cache[egressNode1.name] = &egressNode1
				fakeClusterManagerOVN.eIPC.allocator.cache[egressNode2.name] = &egressNode2

				_, err := fakeClusterManagerOVN.eIPC.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
				egressIPs, nodes := getEgressIPStatus(egressIPName)
				gomega.Expect(nodes[0]).To(gomega.Equal(egressNode2.name))
				gomega.Expect(egressIPs[0]).To(gomega.Equal(egressIP1))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("ensure egressIP status is in sync with cloud private ip config", func() {
			app.Action = func(ctx *cli.Context) error {
				config.OVNKubernetesFeature.EnableInterconnect = true
				config.Kubernetes.PlatformType = string(ocpconfigapi.AWSPlatformType)
				node1IPv4 := "192.168.126.12/24"
				node2IPv4 := "192.168.126.51/24"
				egressIP1 := "192.168.126.101"
				egressIP2 := "192.168.126.100"
				node1 := v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: node1Name,
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4, ""),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":[\"%s\", \"%s\"]}", v4NodeSubnet, v6NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node1IPv4),
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
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\": [\"%s\",\"%s\"]}", v4NodeSubnet, v6NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node2IPv4),
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

				egressNode1 := setupNode(node1Name, []string{"192.168.126.12/24"}, map[string]string{egressIP1: egressIPName})
				egressNode2 := setupNode(node2Name, []string{"192.168.126.51/24"}, map[string]string{egressIP2: egressIPName})

				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{egressIP1, egressIP2},
					},
					Status: egressipv1.EgressIPStatus{
						Items: []egressipv1.EgressIPStatusItem{
							{
								EgressIP: egressIP1,
								Node:     egressNode1.name,
							},
							{
								EgressIP: egressIP2,
								Node:     egressNode2.name,
							},
						},
					},
				}
				cloudPrivateIPConfig := ocpcloudnetworkapi.CloudPrivateIPConfig{
					ObjectMeta: newCloudPrivateIPConfigMeta(egressIP2),
					Spec:       ocpcloudnetworkapi.CloudPrivateIPConfigSpec{Node: node2Name},
					Status: ocpcloudnetworkapi.CloudPrivateIPConfigStatus{Node: node2Name,
						Conditions: []metav1.Condition{{Status: metav1.ConditionTrue, Type: string(ocpcloudnetworkapi.Assigned)}}},
				}
				fakeClusterManagerOVN.start(
					&egressipv1.EgressIPList{
						Items: []egressipv1.EgressIP{eIP},
					},
					&ocpcloudnetworkapi.CloudPrivateIPConfigList{
						Items: []ocpcloudnetworkapi.CloudPrivateIPConfig{cloudPrivateIPConfig},
					},
					&v1.NodeList{
						Items: []v1.Node{node1, node2},
					},
				)

				fakeClusterManagerOVN.eIPC.allocator.cache[egressNode1.name] = &egressNode1
				fakeClusterManagerOVN.eIPC.allocator.cache[egressNode2.name] = &egressNode2

				_, err := fakeClusterManagerOVN.eIPC.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				_, err = fakeClusterManagerOVN.eIPC.WatchCloudPrivateIPConfig()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// Since there is no assignment of cloud private ip for one of the egress ip.
				// syncCloudPrivateIPConfigs removes stale entry from egress ip object status
				// and check if that is done properly or not.
				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
				egressIPs, nodes := getEgressIPStatus(egressIPName)
				gomega.Expect(nodes[0]).To(gomega.Equal(egressNode2.name))
				gomega.Expect(egressIPs[0]).To(gomega.Equal(egressIP2))

				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("ensure failover egressIP status is updated properly while cloud private ip config update in progress", func() {
			app.Action = func(ctx *cli.Context) error {
				config.OVNKubernetesFeature.EnableInterconnect = true
				config.Kubernetes.PlatformType = string(ocpconfigapi.AWSPlatformType)

				egressIP := "192.168.126.101"
				node1IPv4 := "192.168.126.12/24"
				node2IPv4 := "192.168.126.51/24"

				node1 := v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: node1Name,
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4, ""),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":[\"%s\", \"%s\"]}", v4NodeSubnet, v6NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node1IPv4),
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
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\": [\"%s\",\"%s\"]}", v4NodeSubnet, v6NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node2IPv4),
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

				egressNode1 := setupNode(node1Name, []string{node1IPv4}, map[string]string{egressIP: egressIPName})
				egressNode2 := setupNode(node2Name, []string{node2IPv4}, map[string]string{})

				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{egressIP},
					},
					Status: egressipv1.EgressIPStatus{
						Items: []egressipv1.EgressIPStatusItem{
							{
								EgressIP: egressIP,
								Node:     egressNode1.name,
							},
						},
					},
				}
				cloudPrivateIPConfig := ocpcloudnetworkapi.CloudPrivateIPConfig{
					ObjectMeta: newCloudPrivateIPConfigMeta(egressIP),
					Spec:       ocpcloudnetworkapi.CloudPrivateIPConfigSpec{Node: node2Name},
					Status: ocpcloudnetworkapi.CloudPrivateIPConfigStatus{Node: node2Name,
						Conditions: []metav1.Condition{{Status: metav1.ConditionTrue, Type: string(ocpcloudnetworkapi.Assigned)}}},
				}
				fakeClusterManagerOVN.start(
					&egressipv1.EgressIPList{
						Items: []egressipv1.EgressIP{eIP},
					},
					&ocpcloudnetworkapi.CloudPrivateIPConfigList{
						Items: []ocpcloudnetworkapi.CloudPrivateIPConfig{cloudPrivateIPConfig},
					},
					&v1.NodeList{
						Items: []v1.Node{node1, node2},
					},
				)

				fakeClusterManagerOVN.eIPC.allocator.cache[egressNode1.name] = &egressNode1
				fakeClusterManagerOVN.eIPC.allocator.cache[egressNode2.name] = &egressNode2

				_, err := fakeClusterManagerOVN.eIPC.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				_, err = fakeClusterManagerOVN.eIPC.WatchCloudPrivateIPConfig()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// syncCloudPrivateIPConfigs might find different node assignments in the
				// egress ip status and the cloud private ip config status because of old
				// data in the informer cache. Check that the egress ip status is not
				// considered stale in this case and reconciliation happens without issues.
				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
				egressIPs, nodes := getEgressIPStatus(egressIPName)
				gomega.Expect(nodes[0]).To(gomega.Equal(egressNode1.name))
				gomega.Expect(egressIPs[0]).To(gomega.Equal(egressIP))

				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("ensure egressIP status is in sync with empty cloud private ip config", func() {
			app.Action = func(ctx *cli.Context) error {
				config.OVNKubernetesFeature.EnableInterconnect = true
				config.Kubernetes.PlatformType = string(ocpconfigapi.AWSPlatformType)
				node1IPv4 := "192.168.126.12/24"
				node2IPv4 := "192.168.126.51/24"
				egressIP1 := "192.168.126.101"
				egressIP2 := "192.168.126.100"

				node1 := v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: node1Name,
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4, ""),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":[\"%s\", \"%s\"]}", v4NodeSubnet, v6NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node1IPv4),
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
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\": [\"%s\",\"%s\"]}", v4NodeSubnet, v6NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node2IPv4),
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

				egressNode1 := setupNode(node1Name, []string{"192.168.126.12/24"}, map[string]string{egressIP1: egressIPName})
				egressNode2 := setupNode(node2Name, []string{"192.168.126.51/24"}, map[string]string{egressIP2: egressIPName})

				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{egressIP1, egressIP2},
					},
					Status: egressipv1.EgressIPStatus{
						Items: []egressipv1.EgressIPStatusItem{
							{
								EgressIP: egressIP1,
								Node:     egressNode1.name,
							},
							{
								EgressIP: egressIP2,
								Node:     egressNode2.name,
							},
						},
					},
				}
				fakeClusterManagerOVN.start(
					&egressipv1.EgressIPList{
						Items: []egressipv1.EgressIP{eIP},
					},
					&v1.NodeList{
						Items: []v1.Node{node1, node2},
					},
				)

				fakeClusterManagerOVN.eIPC.allocator.cache[egressNode1.name] = &egressNode1
				fakeClusterManagerOVN.eIPC.allocator.cache[egressNode2.name] = &egressNode2

				_, err := fakeClusterManagerOVN.eIPC.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				_, err = fakeClusterManagerOVN.eIPC.WatchCloudPrivateIPConfig()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// Since there is no assignment of cloud private ip. syncCloudPrivateIPConfigs removes
				// all entries from egress ip object status and check if that is done properly or not.
				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(0))
				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should not update valid assignment", func() {
			app.Action = func(ctx *cli.Context) error {
				egressIP1 := "192.168.126.101"
				node1IPv4 := "192.168.126.12/24"
				node2IPv4 := "192.168.126.51/24"

				node1 := v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: node1Name,
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4, ""),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":[\"%s\", \"%s\"]}", v4NodeSubnet, v6NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node1IPv4),
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
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\": [\"%s\",\"%s\"]}", v4NodeSubnet, v6NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node2IPv4),
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

				egressNode1 := setupNode(node1Name, []string{"192.168.126.12/24"}, map[string]string{"192.168.126.111": "bogus2"})
				egressNode2 := setupNode(node2Name, []string{"192.168.126.51/24"}, map[string]string{"192.168.126.68": "bogus3"})

				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{egressIP1},
					},
					Status: egressipv1.EgressIPStatus{
						Items: []egressipv1.EgressIPStatusItem{
							{
								EgressIP: egressIP1,
								Node:     egressNode1.name,
							},
						},
					},
				}

				fakeClusterManagerOVN.start(
					&v1.NodeList{Items: []v1.Node{node1, node2}},
					&egressipv1.EgressIPList{Items: []egressipv1.EgressIP{eIP}},
				)

				fakeClusterManagerOVN.eIPC.allocator.cache[egressNode1.name] = &egressNode1
				fakeClusterManagerOVN.eIPC.allocator.cache[egressNode2.name] = &egressNode2

				_, err := fakeClusterManagerOVN.eIPC.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
				egressIPs, nodes := getEgressIPStatus(egressIPName)
				gomega.Expect(nodes[0]).To(gomega.Equal(egressNode1.name))
				gomega.Expect(egressIPs[0]).To(gomega.Equal(egressIP1))
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
				node1IPv4 := "192.168.126.12/24"
				node2IPv4 := "192.168.126.51/24"

				node1 := v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: node1Name,
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4, ""),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":[\"%s\", \"%s\"]}", v4NodeSubnet, v6NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node1IPv4),
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
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\": [\"%s\",\"%s\"]}", v4NodeSubnet, v6NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node2IPv4),
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

				egressNode1 := setupNode(node1Name, []string{"192.168.126.12/24"}, map[string]string{"192.168.126.102": "bogus1", "192.168.126.111": "bogus2"})
				egressNode2 := setupNode(node2Name, []string{"192.168.126.51/24"}, map[string]string{"192.168.126.68": "bogus3"})

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

				fakeClusterManagerOVN.start(
					&v1.NodeList{Items: []v1.Node{node1, node2}},
					&egressipv1.EgressIPList{Items: []egressipv1.EgressIP{eIP1}},
				)

				fakeClusterManagerOVN.eIPC.allocator.cache[egressNode1.name] = &egressNode1
				fakeClusterManagerOVN.eIPC.allocator.cache[egressNode2.name] = &egressNode2

				_, err := fakeClusterManagerOVN.eIPC.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(getEgressIPStatusLen(eIP1.Name)).Should(gomega.Equal(1))
				egressIPs, nodes := getEgressIPStatus(eIP1.Name)
				gomega.Expect(nodes[0]).To(gomega.Equal(egressNode2.name))
				gomega.Expect(egressIPs[0]).To(gomega.Equal(egressIP1))
				_, err = fakeClusterManagerOVN.fakeClient.EgressIPClient.K8sV1().EgressIPs().Create(context.TODO(), &eIP2, metav1.CreateOptions{})
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
				node1IPv4 := "192.168.126.12/24"
				node2IPv4 := "192.168.126.51/24"

				node1 := v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: node1Name,
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4, ""),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":[\"%s\", \"%s\"]}", v4NodeSubnet, v6NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node1IPv4),
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
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\": [\"%s\",\"%s\"]}", v4NodeSubnet, v6NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node2IPv4),
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

				egressNode1 := setupNode(node1Name, []string{"192.168.126.41/24"}, map[string]string{"192.168.126.102": "bogus1", "192.168.126.111": "bogus2"})
				egressNode2 := setupNode(node2Name, []string{"192.168.126.51/24"}, map[string]string{"192.168.126.68": "bogus3"})

				eIP1 := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{egressIP},
					},
				}

				fakeClusterManagerOVN.start(
					&v1.NodeList{Items: []v1.Node{node1, node2}},
					&egressipv1.EgressIPList{Items: []egressipv1.EgressIP{eIP1}},
				)

				fakeClusterManagerOVN.eIPC.allocator.cache[egressNode1.name] = &egressNode1
				fakeClusterManagerOVN.eIPC.allocator.cache[egressNode2.name] = &egressNode2
				_, err := fakeClusterManagerOVN.eIPC.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
				egressIPs, nodes := getEgressIPStatus(egressIPName)
				gomega.Expect(nodes[0]).To(gomega.Equal(egressNode2.name))
				gomega.Expect(egressIPs[0]).To(gomega.Equal(egressIP))
				eIPToUpdate, err := fakeClusterManagerOVN.fakeClient.EgressIPClient.K8sV1().EgressIPs().Get(context.TODO(), eIP1.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				eIPToUpdate.Spec.EgressIPs = []string{updateEgressIP}

				_, err = fakeClusterManagerOVN.fakeClient.EgressIPClient.K8sV1().EgressIPs().Update(context.TODO(), eIPToUpdate, metav1.UpdateOptions{})
				gomega.Expect(err).ToNot(gomega.HaveOccurred())

				getEgressIP := func() string {
					egressIPs, _ = getEgressIPStatus(egressIPName)
					if len(egressIPs) == 0 {
						return "try again"
					}
					return egressIPs[0]
				}

				gomega.Eventually(getEgressIP).Should(gomega.Equal(updateEgressIP))
				_, nodes = getEgressIPStatus(egressIPName)
				gomega.Expect(nodes[0]).To(gomega.Equal(egressNode2.name))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})
})
