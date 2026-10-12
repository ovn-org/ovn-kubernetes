// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package clustermanager

import (
	"context"
	"fmt"
	"maps"
	"net"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	ocpcloudnetworkapi "github.com/openshift/api/cloudnetwork/v1"
	ocpconfigapi "github.com/openshift/api/config/v1"
	"github.com/urfave/cli/v2"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	utilnet "k8s.io/utils/net"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	egressipv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/healthcheck"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

type fakeEgressIPDialer struct{}

func (f fakeEgressIPDialer) dial(net.IP, time.Duration) bool {
	return true
}

type fakeEgressIPHealthClient struct {
	Connected        bool
	ProbeCount       int
	FakeProbeFailure bool
	mutex            sync.Mutex
}

func (fehc *fakeEgressIPHealthClient) IsConnected() bool {
	fehc.mutex.Lock()
	defer fehc.mutex.Unlock()
	return fehc.Connected
}

func (fehc *fakeEgressIPHealthClient) Connect(context.Context, []net.IP, int) bool {
	fehc.mutex.Lock()
	defer fehc.mutex.Unlock()
	if fehc.FakeProbeFailure {
		return false
	}
	fehc.Connected = true
	return true
}

func (fehc *fakeEgressIPHealthClient) Disconnect() {
	fehc.mutex.Lock()
	defer fehc.mutex.Unlock()
	fehc.Connected = false
	fehc.ProbeCount = 0
}

func (fehc *fakeEgressIPHealthClient) Probe(context.Context) bool {
	fehc.mutex.Lock()
	defer fehc.mutex.Unlock()
	if fehc.Connected && !fehc.FakeProbeFailure {
		fehc.ProbeCount++
		return true
	}
	return false
}

func (fehc *fakeEgressIPHealthClient) getProbeCount() int {
	fehc.mutex.Lock()
	defer fehc.mutex.Unlock()
	return fehc.ProbeCount
}

func (fehc *fakeEgressIPHealthClient) setFakeProbeFailure(probeFailure bool) {
	fehc.mutex.Lock()
	defer fehc.mutex.Unlock()
	fehc.FakeProbeFailure = probeFailure
}

const (
	v4NodeSubnet = "10.128.0.0/24"
	v6NodeSubnet = "ae70::66/64"
)

type fakeEgressIPHealthClientAllocator struct{}

func (f *fakeEgressIPHealthClientAllocator) allocate(string) healthcheck.EgressIPHealthClient {
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

func newNamespace(namespace string) *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: newNamespaceMeta(namespace, nil),
		Spec:       corev1.NamespaceSpec{},
		Status:     corev1.NamespaceStatus{},
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

func newEgressTestNode(name, ipv4, ipv6, hostCIDRs string, egressLabelled bool) corev1.Node {
	annotations := map[string]string{}
	labels := map[string]string{}
	if ipv4 != "" || ipv6 != "" {
		annotations["k8s.ovn.org/node-primary-ifaddr"] = fmt.Sprintf(`{"ipv4": "%s", "ipv6": "%s"}`, ipv4, ipv6)
		if ipv4 != "" {
			annotations["k8s.ovn.org/node-subnets"] = fmt.Sprintf(`{"default":"%s"}`, v4NodeSubnet)
		} else {
			annotations["k8s.ovn.org/node-subnets"] = fmt.Sprintf(`{"default":"%s"}`, v6NodeSubnet)
		}
	}
	if hostCIDRs != "" {
		annotations[util.OVNNodeHostCIDRs] = hostCIDRs
	}
	if egressLabelled {
		labels["k8s.ovn.org/egress-assignable"] = ""
	}
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Labels:      labels,
			Annotations: annotations,
		},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{
					Type:   corev1.NodeReady,
					Status: corev1.ConditionTrue,
				},
			},
		},
	}
}

func setupNode(nodeName string, ipNets []string, mockAllocationIPs map[string]string) egressNode {
	unlimited := util.UnlimitedNodeCapacity
	var config = &util.ParsedNodeEgressIPConfiguration{Capacity: util.Capacity{
		IP:   &unlimited,
		IPv4: &unlimited,
		IPv6: &unlimited,
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
	)

	dialer = fakeEgressIPDialer{}
	hccAllocator = &fakeEgressIPHealthClientAllocator{}

	getEgressIPAllocatorSizeSafely := func() int {
		fakeClusterManagerOVN.eIPC.nodeAllocator.Lock()
		defer fakeClusterManagerOVN.eIPC.nodeAllocator.Unlock()
		return len(fakeClusterManagerOVN.eIPC.nodeAllocator.cache)
	}

	getEgressIPAllocatorSafely := func(s string, v6 bool) util.ParsedIFAddr {
		fakeClusterManagerOVN.eIPC.nodeAllocator.Lock()
		defer fakeClusterManagerOVN.eIPC.nodeAllocator.Unlock()
		c, ok := fakeClusterManagerOVN.eIPC.nodeAllocator.cache[s]
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
		fakeClusterManagerOVN.eIPC.nodeAllocator.Lock()
		defer fakeClusterManagerOVN.eIPC.nodeAllocator.Unlock()
		c, ok := fakeClusterManagerOVN.eIPC.nodeAllocator.cache[s]
		if !ok {
			panic(fmt.Sprintf("failed to find key %s", s))
		}
		return c.isReachable
	}

	doesEgressIPAllocatorContainSafely := func(s string) bool {
		fakeClusterManagerOVN.eIPC.nodeAllocator.Lock()
		defer fakeClusterManagerOVN.eIPC.nodeAllocator.Unlock()
		_, ok := fakeClusterManagerOVN.eIPC.nodeAllocator.cache[s]
		return ok
	}

	getEgressIPAllocatorHealthCheckSafely := func(s string) *fakeEgressIPHealthClient {
		fakeClusterManagerOVN.eIPC.nodeAllocator.Lock()
		defer fakeClusterManagerOVN.eIPC.nodeAllocator.Unlock()
		c, ok := fakeClusterManagerOVN.eIPC.nodeAllocator.cache[s]
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

	getEgressIPAnnotationValue := func(egressIPName string) func() (string, error) {
		return func() (string, error) {
			tmp, err := fakeClusterManagerOVN.fakeClient.EgressIPClient.K8sV1().EgressIPs().Get(context.TODO(), egressIPName, metav1.GetOptions{})
			if err != nil {
				return "", err
			}
			return tmp.Annotations[util.EgressIPMarkAnnotation], nil
		}
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
			fakeClusterManagerOVN.eIPC.nodeAllocator.Lock()
			defer fakeClusterManagerOVN.eIPC.nodeAllocator.Unlock()
			if item, exists := fakeClusterManagerOVN.eIPC.nodeAllocator.cache[nodeName]; exists {
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

	egressIPsMatch := func(expectedEgressIPs []egressipv1.EgressIP) func() bool {
		return func() (result bool) {
			egressIPList, err := fakeClusterManagerOVN.fakeClient.EgressIPClient.K8sV1().EgressIPs().List(context.TODO(), metav1.ListOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gotEgressIPs := egressIPList.Items

			defer func() {
				if !result {
					ginkgo.GinkgoWriter.Printf("Mismatch in egressIP lists, got: %+v, expected: %+v\n", gotEgressIPs, expectedEgressIPs)
				}
			}()

			if len(expectedEgressIPs) != len(gotEgressIPs) {
				return false
			}

			numEquals := 0
			for _, expectedEgressIP := range expectedEgressIPs {
				for _, gotEgressIP := range gotEgressIPs {
					if expectedEgressIP.Name != gotEgressIP.Name {
						continue
					}

					specExpectedEIP := slices.Clone(expectedEgressIP.Spec.EgressIPs)
					specGotEgressIP := slices.Clone(gotEgressIP.Spec.EgressIPs)
					slices.Sort(specExpectedEIP)
					slices.Sort(specGotEgressIP)
					if !reflect.DeepEqual(specExpectedEIP, specGotEgressIP) {
						return false
					}

					statusExpectedEgressIP := slices.Clone(expectedEgressIP.Status.Items)
					statusGotEgressIP := slices.Clone(gotEgressIP.Status.Items)
					sortFunc := func(a, b egressipv1.EgressIPStatusItem) int {
						return strings.Compare(a.EgressIP, b.EgressIP)
					}
					slices.SortFunc(statusExpectedEgressIP, sortFunc)
					slices.SortFunc(statusGotEgressIP, sortFunc)
					if !reflect.DeepEqual(statusExpectedEgressIP, statusGotEgressIP) {
						return false
					}
					numEquals++
				}
			}
			return len(expectedEgressIPs) == numEquals
		}
	}

	readAllocations := func(nodeName string) map[string]string {
		fakeClusterManagerOVN.eIPC.nodeAllocator.Lock()
		defer fakeClusterManagerOVN.eIPC.nodeAllocator.Unlock()
		node, ok := fakeClusterManagerOVN.eIPC.nodeAllocator.cache[nodeName]
		gomega.Expect(ok).To(gomega.BeTrue(), fmt.Sprintf("node %s missing from allocator cache", nodeName))
		return maps.Clone(node.allocations)
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
		ginkgo.DescribeTable("should re-assign EgressIPs and perform proper egressIP allocation changes", func(egressIP string) {
			app.Action = func(*cli.Context) error {
				node1IPv4OVN := "192.168.126.202/24"
				node1IPv4SecondaryHost := "10.10.10.3/24"
				node1Ipv4SecondaryHost2 := "7.7.7.7/16"
				node2IPv4OVN := "192.168.126.51/24"
				node2IPv4SecondaryHost := "10.10.10.4/24"
				node2Ipv4SecondaryHost2 := "7.7.7.8/16"
				node2Ipv4SecondaryHost3 := "5.5.5.5/16"

				egressNamespace := newNamespace(namespace)
				node1 := corev1.Node{
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
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
							},
						},
					},
				}
				node2 := corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: node2Name,
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node2IPv4OVN, ""),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":\"%s\"}", v4NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\",\"%s\",\"%s\",\"%s\"]", node2IPv4OVN, node2IPv4SecondaryHost, node2Ipv4SecondaryHost2, node2Ipv4SecondaryHost3),
						},
					},
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
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
					&corev1.NodeList{
						Items: []corev1.Node{node1, node2},
					},
				)

				_, err := fakeClusterManagerOVN.eIPC.WatchEgressNodes()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				_, err = fakeClusterManagerOVN.eIPC.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(getEgressIPAllocatorSizeSafely).Should(gomega.Equal(2))
				gomega.Expect(fakeClusterManagerOVN.eIPC.nodeAllocator.cache).To(gomega.HaveKey(node1.Name))
				gomega.Expect(fakeClusterManagerOVN.eIPC.nodeAllocator.cache).To(gomega.HaveKey(node2.Name))
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
		}, ginkgo.Entry("OVN network", "192.168.126.101"),
			ginkgo.Entry("Secondary host network", "10.10.10.100"),
			ginkgo.Entry("Secondary host network", "7.7.7.100"),
			ginkgo.Entry("Secondary host network", "7.7.8.100"),
		)

		ginkgo.DescribeTable("should re-assign EgressIPs and perform proper egressIP allocation changes during node deletion", func(egressIP string) {
			app.Action = func(*cli.Context) error {
				node1IPv4OVN := "192.168.126.202/24"
				node1IPv4SecondaryHost := "10.10.10.3/24"
				node1Ipv4SecondaryHost2 := "7.7.7.7/16"
				node2IPv4OVN := "192.168.126.51/24"
				node2IPv4SecondaryHost := "10.10.10.4/24"
				node2Ipv4SecondaryHost2 := "7.7.7.8/16"
				node2Ipv4SecondaryHost3 := "5.5.5.5/16"
				egressNamespace := newNamespace(namespace)

				node1 := corev1.Node{
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
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
							},
						},
					},
				}
				node2 := corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: node2Name,
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node2IPv4OVN, ""),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":\"%s\"}", v4NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\",\"%s\",\"%s\",\"%s\"]", node2IPv4OVN, node2IPv4SecondaryHost, node2Ipv4SecondaryHost2, node2Ipv4SecondaryHost3),
						},
					},
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
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
					&corev1.NodeList{
						Items: []corev1.Node{node1, node2},
					},
				)

				_, err := fakeClusterManagerOVN.eIPC.WatchEgressNodes()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				_, err = fakeClusterManagerOVN.eIPC.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(getEgressIPAllocatorSizeSafely).Should(gomega.Equal(2))
				gomega.Expect(fakeClusterManagerOVN.eIPC.nodeAllocator.cache).To(gomega.HaveKey(node1.Name))
				gomega.Expect(fakeClusterManagerOVN.eIPC.nodeAllocator.cache).To(gomega.HaveKey(node2.Name))
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
		}, ginkgo.Entry("OVN network", "192.168.126.101"),
			ginkgo.Entry("Secondary host network", "10.10.10.100"),
			ginkgo.Entry("Secondary host network", "7.7.7.100"),
			ginkgo.Entry("Secondary host network", "7.7.8.100"),
		)
		ginkgo.It("should assign EgressIPs to a linux node when there are windows nodes in the cluster", func() {
			app.Action = func(*cli.Context) error {
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

				node1 := corev1.Node{
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
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
							},
						},
					},
				}
				hybridOverlayNode := corev1.Node{
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
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
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
					&corev1.NodeList{
						Items: []corev1.Node{node1, hybridOverlayNode},
					},
				)
				_, err = fakeClusterManagerOVN.eIPC.WatchEgressNodes()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				_, err = fakeClusterManagerOVN.eIPC.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(getEgressIPAllocatorSizeSafely).Should(gomega.Equal(1))
				gomega.Expect(fakeClusterManagerOVN.eIPC.nodeAllocator.cache).To(gomega.HaveKey(node1.Name))
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
			app.Action = func(*cli.Context) error {

				node1IPv4OVN := "192.168.128.202/24"
				node1IPv6OVN := "1521:b9cc:59b3:b9ee:95e9:f04c:4fec:33c4/64"
				node1IPv4SecondaryHost := "10.10.10.3/24"
				node1IPv4SecondaryHost2 := "7.7.7.7/16"
				node2IPv4OVN := "192.168.126.51/24"
				node2IPv4SecondaryHost := "10.10.10.20/24"
				node2IPv4SecondaryHost2 := "7.7.7.40/16"

				node1 := corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: "node1",
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4OVN, node1IPv6OVN),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":[\"%s\", \"%s\"]}", v4NodeSubnet, v6NodeSubnet),
							util.OVNNodeHostCIDRs: fmt.Sprintf("[\"%s\",\"%s\",\"%s\",\"%s\"]", node1IPv4OVN,
								node1IPv6OVN, node1IPv4SecondaryHost, node1IPv4SecondaryHost2),
						},
					},
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
							},
						},
					},
				}
				node2 := corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: "node2",
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node2IPv4OVN, ""),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":\"%s\"}", v4NodeSubnet),
							util.OVNNodeHostCIDRs: fmt.Sprintf("[\"%s\",\"%s\",\"%s\"]", node2IPv4OVN,
								node2IPv4SecondaryHost, node2IPv4SecondaryHost2),
						},
					},
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
							},
						},
					},
				}
				fakeClusterManagerOVN.start(
					&corev1.NodeList{
						Items: []corev1.Node{},
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
			app.Action = func(*cli.Context) error {
				nodeIPv4 := "192.168.126.51/24"
				nodeIPv6 := "9287:980a:2f3a:52e5:a579:df85:e82f:b7cd/64"
				node := corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: "node",
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", nodeIPv4, nodeIPv6),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":[\"%s\", \"%s\"]}", v4NodeSubnet, v6NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\",\"%s\"]", nodeIPv4, nodeIPv6),
						},
					},
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
							},
						},
					},
				}
				fakeClusterManagerOVN.start(
					&corev1.NodeList{
						Items: []corev1.Node{},
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
			app.Action = func(*cli.Context) error {

				config.Gateway.DisableSNATMultipleGWs = true

				egressIP1 := "192.168.126.25"
				egressIP2 := "192.168.126.30"
				egressIP3 := "192.168.126.35"
				node1IPv4 := "192.168.126.12/24"
				node2IPv4 := "192.168.126.13/24"

				egressNamespace := newNamespace(namespace)

				node1 := corev1.Node{
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
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
							},
						},
					},
				}

				node2 := corev1.Node{
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
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
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
					&corev1.NodeList{
						Items: []corev1.Node{node1, node2},
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
			app.Action = func(*cli.Context) error {
				nodeIPv4 := "192.168.126.510/24"
				nodeIPv6 := "1521:b9cc:59b3:b9ee:95e9:f04c:4fec:33c4/64"
				node := corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: node1Name,
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", nodeIPv4, nodeIPv6),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":[\"%s\", \"%s\"]}", v4NodeSubnet, v6NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\",\"%s\"]", nodeIPv4, nodeIPv6),
						},
					},
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
							},
						},
					},
				}
				fakeClusterManagerOVN.start(
					&corev1.NodeList{
						Items: []corev1.Node{node},
					},
				)

				allocatorItems := func() int {
					return len(fakeClusterManagerOVN.eIPC.nodeAllocator.cache)
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
			app.Action = func(*cli.Context) error {
				node1IPv6 := "1521:b9cc:59b3:b9ee:95e9:f04c:4fec:33c4/64"
				node2IPv4 := "192.168.126.51/24"

				node1 := corev1.Node{
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
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
							},
						},
					},
				}
				node2 := corev1.Node{
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
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
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
						tcPrepareFunc: func(hcc1, _ *fakeEgressIPHealthClient) {
							hcc1.Disconnect()
						},
					},
					{
						desc:            "connect node1, disconnect node2",
						node2FailProbes: true,
						tcPrepareFunc: func(_, hcc2 *fakeEgressIPHealthClient) {
							hcc2.Disconnect()
						},
					},
					{
						desc: "node1 and node2 connected and both counters bump",
						tcPrepareFunc: func(_, _ *fakeEgressIPHealthClient) {
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
						gomega.Expect(hcc.getProbeCount()).To(gomega.Equal(prevProbes), desc)
					} else if prevNodeIsConnected && currNodeIsConnected {
						if failProbes {
							// Still connected, but no probes should be successful
							gomega.Expect(prevProbes).To(gomega.Equal(hcc.getProbeCount()), desc)
						} else {
							// Still connected and probe counters should be going up
							gomega.Expect(prevProbes).To(gomega.BeNumerically("<", hcc.getProbeCount()), desc)
						}
					}
				}

				for _, tt := range tests {
					hcc1.setFakeProbeFailure(tt.node1FailProbes)
					hcc2.setFakeProbeFailure(tt.node2FailProbes)

					prevNode1IsConnected := hcc1.IsConnected()
					prevNode2IsConnected := hcc2.IsConnected()
					prevNode1Probes := hcc1.getProbeCount()
					prevNode2Probes := hcc2.getProbeCount()

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
			app.Action = func(*cli.Context) error {

				egressIP := "192.168.126.51"
				node1IPv4 := "192.168.128.202/24"
				node1IPv6 := "1521:b9cc:59b3:b9ee:95e9:f04c:4fec:33c4/64"
				node2IPv4 := "192.168.126.51/24"

				node1 := corev1.Node{
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
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
							},
						},
					},
				}
				node2 := corev1.Node{
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
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
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
					&corev1.NodeList{
						Items: []corev1.Node{node1, node2},
					})

				_, err := fakeClusterManagerOVN.eIPC.WatchEgressNodes()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				_, err = fakeClusterManagerOVN.eIPC.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(getEgressIPAllocatorSizeSafely).Should(gomega.Equal(2))
				gomega.Expect(fakeClusterManagerOVN.eIPC.nodeAllocator.cache).To(gomega.HaveKey(node1.Name))
				gomega.Expect(fakeClusterManagerOVN.eIPC.nodeAllocator.cache).To(gomega.HaveKey(node2.Name))

				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(0))
				// event1 triggered during node1 add for the existing EIP and event2 is triggered during node2 add for the existing EIP
				/*I0212 20:22:37.636573 1837759 egressip_controller.go:723] Egress node: node1 about to be initialized
				I0212 20:22:37.636670 1837759 obj_retry.go:512] Add event received for *factory.egressNode node2
				I0212 20:22:37.636757 1837759 egressip_controller.go:723] Egress node: node2 about to be initialized
				I0212 20:22:37.636855 1837759 egressip_controller.go:1173] Current assignments are: map[]
				I0212 20:22:37.636865 1837759 egressip_controller.go:1175] Will attempt assignment for egress IP: 192.168.126.51
				E0212 20:22:37.636909 1837759 egressip_controller.go:1190] Egress IP: 192.168.126.51 address is already assigned on an interface on node node2
				I0212 20:22:37.636919 1837759 obj_retry.go:551] Creating *factory.egressNode node2 took: 217.968µs
				I0212 20:22:37.638850 1837759 egressip_controller.go:1173] Current assignments are: map[]
				I0212 20:22:37.639675 1837759 egressip_controller.go:1175] Will attempt assignment for egress IP: 192.168.126.51
				E0212 20:22:37.639753 1837759 egressip_controller.go:1190] Egress IP: 192.168.126.51 address is already assigned on an interface on node node2
				I0212 20:22:37.639776 1837759 obj_retry.go:551] Creating *factory.egressNode node1 took: 3.316342ms*/

				// event3 trigged from syncEgressIPMarkAllocator called during WatchEgressIP starter syncFunc and
				// event4 is triggered on the actual EIP add reconcile
				/*I0212 20:22:37.639894 1837759 egressip_controller.go:1729] Patching status on EgressIP egressip: [{add /metadata/annotations map[k8s.ovn.org/egressip-mark:50000]}]
				I0212 20:22:37.641594 1837759 obj_retry.go:512] Add event received for *v1.EgressIP egressip
				I0212 20:22:37.641635 1837759 egressip_controller.go:1173] Current assignments are: map[]
				I0212 20:22:37.641641 1837759 egressip_controller.go:1175] Will attempt assignment for egress IP: 192.168.126.51
				E0212 20:22:37.641664 1837759 egressip_controller.go:1190] Egress IP: 192.168.126.51 address is already assigned on an interface on node node2
				I0212 20:22:37.641670 1837759 obj_retry.go:551] Creating *v1.EgressIP egressip took: 53.241µs
				I0212 20:22:37.643003 1837759 factory.go:1322] Added *v1.EgressIP event handler 2
				I0212 20:22:37.643123 1837759 obj_retry.go:568] Update event received for resource *v1.EgressIP, old object is equal to new: false
				I0212 20:22:37.643154 1837759 obj_retry.go:620] Update event received for *v1.EgressIP egressip
				I0212 20:22:37.643187 1837759 egressip_controller.go:1173] Current assignments are: map[]
				I0212 20:22:37.643205 1837759 egressip_controller.go:1175] Will attempt assignment for egress IP: 192.168.126.51
				E0212 20:22:37.643254 1837759 egressip_controller.go:1190] Egress IP: 192.168.126.51 address is already assigned on an interface on node node2*/
				gomega.Eventually(fakeClusterManagerOVN.fakeRecorder.Events).Should(gomega.HaveLen(8))
				conflictCount := 0
				noMatchingCount := 0
				for i := 0; i < 8; i++ {
					recordedEvent := <-fakeClusterManagerOVN.fakeRecorder.Events
					gomega.Expect(recordedEvent).To(gomega.SatisfyAny(
						gomega.ContainSubstring("EgressIPConflict Egress IP egressip with IP 192.168.126.51 is conflicting with a host (node2) IP address and will not be assigned"),
						gomega.ContainSubstring("NoMatchingNodeFound No matching nodes found, which can host any of the egress IPs: [192.168.126.51] for object EgressIP: egressip")))
					if strings.Contains(recordedEvent, "EgressIPConflict") {
						conflictCount++
					}
					if strings.Contains(recordedEvent, "NoMatchingNodeFound") {
						noMatchingCount++
					}
				}
				gomega.Expect(conflictCount).To(gomega.Equal(4))
				gomega.Expect(noMatchingCount).To(gomega.Equal(4))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should assign EgressIP when one node is missing host-cidrs annotation", func() {
			app.Action = func(*cli.Context) error {
				config.IPv4Mode = true
				config.IPv6Mode = true

				egressIPv4 := "192.168.126.101"
				egressIPv6 := "ae70::100"
				node1IPv4 := "192.168.126.12/24"
				node2IPv6 := "ae70::2/64"

				node1 := newEgressTestNode(node1Name, node1IPv4, "", fmt.Sprintf(`["%s"]`, node1IPv4), true)
				node2 := newEgressTestNode(node2Name, "", node2IPv6, fmt.Sprintf(`["%s"]`, node2IPv6), true)
				orphanNode := newEgressTestNode("orphan-node", "192.168.126.14/24", "", "", true)

				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{egressIPv4, egressIPv6},
					},
					Status: egressipv1.EgressIPStatus{
						Items: []egressipv1.EgressIPStatusItem{},
					},
				}

				fakeClusterManagerOVN.start(
					&egressipv1.EgressIPList{
						Items: []egressipv1.EgressIP{eIP},
					},
					&corev1.NodeList{
						Items: []corev1.Node{node1, node2, orphanNode},
					})

				_, err := fakeClusterManagerOVN.eIPC.WatchEgressNodes()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				_, err = fakeClusterManagerOVN.eIPC.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(func() []egressipv1.EgressIPStatusItem {
					eIP, err := fakeClusterManagerOVN.fakeClient.EgressIPClient.K8sV1().EgressIPs().Get(context.TODO(), egressIPName, metav1.GetOptions{})
					if err != nil {
						return nil
					}
					return eIP.Status.Items
				}).WithTimeout(2 * time.Second).Should(gomega.HaveLen(2))

				finalEIP, err := fakeClusterManagerOVN.fakeClient.EgressIPClient.K8sV1().EgressIPs().Get(context.TODO(), egressIPName, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				assignedNodes := make(map[string]string)
				for _, item := range finalEIP.Status.Items {
					assignedNodes[item.Node] = item.EgressIP
				}
				gomega.Expect(assignedNodes).To(gomega.HaveKey(node1Name))
				gomega.Expect(assignedNodes).To(gomega.HaveKey(node2Name))
				gomega.Expect(assignedNodes[node1Name]).To(gomega.Equal(egressIPv4))
				gomega.Expect(assignedNodes[node2Name]).To(gomega.Equal(egressIPv6))
				gomega.Expect(assignedNodes).NotTo(gomega.HaveKey("orphan-node"))

				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.DescribeTable("should not assign EgressIP to a node with unusable host-cidrs",
			func(badHostCIDRs string) {
				app.Action = func(*cli.Context) error {
					config.IPv4Mode = true

					node1IPv4 := "192.168.126.12/24"
					node1 := newEgressTestNode(node1Name, node1IPv4, "", fmt.Sprintf(`["%s"]`, node1IPv4), true)
					node2 := newEgressTestNode(node2Name, "192.168.126.13/24", "", badHostCIDRs, true)

					eIP := egressipv1.EgressIP{
						ObjectMeta: newEgressIPMeta(egressIPName),
						Spec: egressipv1.EgressIPSpec{
							EgressIPs: []string{"192.168.126.101"},
						},
						Status: egressipv1.EgressIPStatus{
							Items: []egressipv1.EgressIPStatusItem{},
						},
					}

					fakeClusterManagerOVN.start(
						&egressipv1.EgressIPList{
							Items: []egressipv1.EgressIP{eIP},
						},
						&corev1.NodeList{
							Items: []corev1.Node{node1, node2},
						})

					_, err := fakeClusterManagerOVN.eIPC.WatchEgressNodes()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					_, err = fakeClusterManagerOVN.eIPC.WatchEgressIP()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					getStatusItems := func() []egressipv1.EgressIPStatusItem {
						eIP, err := fakeClusterManagerOVN.fakeClient.EgressIPClient.K8sV1().EgressIPs().Get(context.TODO(), egressIPName, metav1.GetOptions{})
						if err != nil {
							return nil
						}
						return eIP.Status.Items
					}

					gomega.Eventually(getStatusItems).WithTimeout(2 * time.Second).Should(gomega.HaveLen(1))
					gomega.Expect(getStatusItems()[0].Node).To(gomega.Equal(node1Name))
					gomega.Consistently(getStatusItems).WithTimeout(500 * time.Millisecond).ShouldNot(
						gomega.ContainElement(gomega.HaveField("Node", node2Name)))

					return nil
				}
				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			},
			ginkgo.Entry("malformed annotation", "not-json"),
			ginkgo.Entry("empty annotation", "[]"),
		)

		ginkgo.It("should retry cleanup of unassignable node via inRetryCache when both old and new have broken annotation", func() {
			app.Action = func(*cli.Context) error {
				config.IPv4Mode = true

				node1IPv4 := "192.168.126.12/24"
				node2IPv4 := "192.168.126.13/24"
				node1 := newEgressTestNode(node1Name, node1IPv4, "", fmt.Sprintf(`["%s"]`, node1IPv4), true)
				node2 := newEgressTestNode(node2Name, node2IPv4, "", fmt.Sprintf(`["%s"]`, node2IPv4), true)

				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{"192.168.126.101"},
					},
					Status: egressipv1.EgressIPStatus{
						Items: []egressipv1.EgressIPStatusItem{},
					},
				}

				fakeClusterManagerOVN.start(
					&egressipv1.EgressIPList{
						Items: []egressipv1.EgressIP{eIP},
					},
					&corev1.NodeList{
						Items: []corev1.Node{node1, node2},
					})

				_, err := fakeClusterManagerOVN.eIPC.WatchEgressNodes()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				_, err = fakeClusterManagerOVN.eIPC.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				initialNodeName := ""
				gomega.Eventually(func() string {
					eIP, err := fakeClusterManagerOVN.fakeClient.EgressIPClient.K8sV1().EgressIPs().Get(context.TODO(), egressIPName, metav1.GetOptions{})
					if err != nil || len(eIP.Status.Items) == 0 {
						return ""
					}
					initialNodeName = eIP.Status.Items[0].Node
					return initialNodeName
				}).WithTimeout(2 * time.Second).Should(gomega.BeElementOf(node1Name, node2Name))

				brokenNode := node1.DeepCopy()
				failoverNodeName := node2Name
				if initialNodeName == node2Name {
					brokenNode = node2.DeepCopy()
					failoverNodeName = node1Name
				}
				delete(brokenNode.Annotations, util.OVNNodeHostCIDRs)
				_, err = fakeClusterManagerOVN.fakeClient.KubeClient.CoreV1().Nodes().Update(
					context.TODO(), brokenNode, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				handler := &egressIPClusterControllerEventHandler{
					objType: factory.EgressNodeType,
					eIPC:    fakeClusterManagerOVN.eIPC,
				}
				err = handler.UpdateResource(brokenNode, brokenNode, true)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(func() string {
					eIP, err := fakeClusterManagerOVN.fakeClient.EgressIPClient.K8sV1().EgressIPs().Get(context.TODO(), egressIPName, metav1.GetOptions{})
					if err != nil || len(eIP.Status.Items) == 0 {
						return ""
					}
					return eIP.Status.Items[0].Node
				}).WithTimeout(2 * time.Second).Should(gomega.Equal(failoverNodeName))

				gomega.Consistently(func() string {
					eIP, err := fakeClusterManagerOVN.fakeClient.EgressIPClient.K8sV1().EgressIPs().Get(context.TODO(), egressIPName, metav1.GetOptions{})
					if err != nil || len(eIP.Status.Items) == 0 {
						return ""
					}
					return eIP.Status.Items[0].Node
				}).WithTimeout(500 * time.Millisecond).Should(gomega.Equal(failoverNodeName))

				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should remove stale EgressIP setup when node label is removed while ovnkube-cluster-manager is not running and assign to newly labelled node", func() {
			app.Action = func(*cli.Context) error {

				egressIP1 := "192.168.126.25"
				node1IPv4 := "192.168.126.51/24"

				egressNamespace := newNamespace(namespace)

				node1 := corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: node1Name,
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\"}", node1IPv4),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":\"%s\"}", v4NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node1IPv4),
						},
					},
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
							},
						},
					},
				}
				node2 := corev1.Node{
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
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
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
					&corev1.NodeList{
						Items: []corev1.Node{node1, node2},
					},
				)

				_, err := fakeClusterManagerOVN.eIPC.WatchEgressNodes()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				_, err = fakeClusterManagerOVN.eIPC.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
				gomega.Eventually(getEgressIPReassignmentCount).Should(gomega.Equal(0))
				gomega.Expect(fakeClusterManagerOVN.eIPC.nodeAllocator.cache).To(gomega.HaveKey(node2.Name))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should only get assigned EgressIPs which matches their subnet when the node is tagged", func() {
			app.Action = func(*cli.Context) error {

				egressIP := "192.168.126.101"
				node1IPv4 := "192.168.128.202/24"
				node1IPv6 := "1521:b9cc:59b3:b9ee:95e9:f04c:4fec:33c4/64"
				node2IPv4 := "192.168.126.51/24"

				node1 := corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: node1Name,
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4, node1IPv6),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":\"%s\"}", v4NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\",\"%s\"]", node1IPv4, node1IPv6),
						},
					},
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
							},
						},
					},
				}
				node2 := corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: node2Name,
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node2IPv4, ""),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":\"%s\"}", v4NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node2IPv4),
						},
					},
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
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
					&corev1.NodeList{
						Items: []corev1.Node{node1, node2},
					})

				_, err := fakeClusterManagerOVN.eIPC.WatchEgressNodes()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				_, err = fakeClusterManagerOVN.eIPC.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				_, ip1V4Sub, err := net.ParseCIDR(node1IPv4)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				_, ip1V6Sub, err := net.ParseCIDR(node1IPv6)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				_, ip2V4Sub, err := net.ParseCIDR(node2IPv4)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(getEgressIPAllocatorSizeSafely).Should(gomega.Equal(2))
				gomega.Expect(fakeClusterManagerOVN.eIPC.nodeAllocator.cache).To(gomega.HaveKey(node1.Name))
				gomega.Expect(fakeClusterManagerOVN.eIPC.nodeAllocator.cache).To(gomega.HaveKey(node2.Name))
				gomega.Eventually(isEgressAssignableNode(node1.Name)).Should(gomega.BeFalse())
				gomega.Eventually(isEgressAssignableNode(node2.Name)).Should(gomega.BeFalse())
				gomega.Expect(getEgressIPAllocatorSafely(node1.Name, false).Net).To(gomega.Equal(ip1V4Sub))
				gomega.Expect(getEgressIPAllocatorSafely(node1.Name, true).Net).To(gomega.Equal(ip1V6Sub))
				gomega.Expect(getEgressIPAllocatorSafely(node2.Name, false).Net).To(gomega.Equal(ip2V4Sub))
				gomega.Eventually(eIP.Status.Items).Should(gomega.BeEmpty())

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
			app.Action = func(*cli.Context) error {
				egressIP := "192.168.126.101"
				node1IPv4 := "192.168.126.12/24"
				node1IPv6 := "1521:b9cc:59b3:b9ee:95e9:f04c:4fec:33c4/64"
				node2IPv4 := "192.168.126.51/24"

				node1 := corev1.Node{
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
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
							},
						},
					},
				}
				node2 := corev1.Node{
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
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
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
					&corev1.NodeList{
						Items: []corev1.Node{node1},
					})

				_, err := fakeClusterManagerOVN.eIPC.WatchEgressNodes()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				_, err = fakeClusterManagerOVN.eIPC.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(getEgressIPAllocatorSizeSafely).Should(gomega.Equal(1))
				gomega.Expect(fakeClusterManagerOVN.eIPC.nodeAllocator.cache).To(gomega.HaveKey(node1.Name))
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
				gomega.Expect(fakeClusterManagerOVN.eIPC.nodeAllocator.cache).To(gomega.HaveKey(node1.Name))
				gomega.Expect(fakeClusterManagerOVN.eIPC.nodeAllocator.cache).To(gomega.HaveKey(node2.Name))

				err = fakeClusterManagerOVN.fakeClient.KubeClient.CoreV1().Nodes().Delete(context.TODO(), node1.Name, *metav1.NewDeleteOptions(0))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(getEgressIPAllocatorSizeSafely).Should(gomega.Equal(1))
				gomega.Expect(fakeClusterManagerOVN.eIPC.nodeAllocator.cache).ToNot(gomega.HaveKey(node1.Name))
				gomega.Expect(fakeClusterManagerOVN.eIPC.nodeAllocator.cache).To(gomega.HaveKey(node2.Name))
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
			app.Action = func(*cli.Context) error {
				egressIP := "192.168.126.101"
				nodeIPv4 := "192.168.126.51/24"
				node := corev1.Node{
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
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
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
					&corev1.NodeList{
						Items: []corev1.Node{node},
					},
				)

				// Virtually disable background reachability check by using a huge interval
				fakeClusterManagerOVN.eIPC.reachabilityCheckInterval = time.Hour

				_, err := fakeClusterManagerOVN.eIPC.WatchEgressNodes()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(getEgressIPStatusLen(eIP1.Name)).Should(gomega.Equal(1))
				egressIPs, _ := getEgressIPStatus(eIP1.Name)
				gomega.Expect(egressIPs[0]).To(gomega.Equal(egressIP))
				hcClient := fakeClusterManagerOVN.eIPC.nodeAllocator.cache[node.Name].healthClient.(*fakeEgressIPHealthClient)
				hcClient.setFakeProbeFailure(true)
				// explicitly call check reachability, periodic checker is not active
				checkEgressNodesReachabilityIterate(fakeClusterManagerOVN.eIPC)
				gomega.Eventually(getEgressIPStatusLen(eIP1.Name)).Should(gomega.Equal(0))

				hcClient.setFakeProbeFailure(false)
				node.Annotations["test"] = "dummy"
				_, err = fakeClusterManagerOVN.fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), &node, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(hcClient.IsConnected).Should(gomega.BeTrue())
				// the node should not be marked as reachable in the update handler as it is not getting added
				gomega.Consistently(func() bool { return fakeClusterManagerOVN.eIPC.nodeAllocator.cache[node.Name].isReachable }).Should(gomega.BeFalse())

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

		ginkgo.It("cloud egress IP config annotation update should trigger egress IP re-evaluation", func() {
			// When cloud-network-config-controller adds or corrects the
			// cloud.network.openshift.io/egress-ipconfig annotation on a node (e.g.
			// adding an IPv6 subnet that was initially absent because the AWS subnet's
			// IPv6 CIDR block was not yet in "associated" state), the node update
			// handler must call addEgressNode so that previously-unassigned EgressIPs
			// can be reconsidered and assigned.
			//
			// The EgressIP has a single IPv6 address so that one node is sufficient —
			// the assignment algorithm disallows two IPs from the same EgressIP object
			// on the same node.
			app.Action = func(*cli.Context) error {
				config.Kubernetes.PlatformType = string(ocpconfigapi.AWSPlatformType)

				nodeIPv4 := "192.168.126.12/24"
				egressIPv6 := "fc00:f853:ccd:e793::101"

				// Node starts with IPv4-only cloud egress-ipconfig — simulates CNCC
				// not yet having observed the IPv6 CIDR association on the cloud subnet.
				node := corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: node1Name,
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf(`{"ipv4": "%s", "ipv6": ""}`, nodeIPv4),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf(`{"default":["%s"]}`, v4NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf(`["%s"]`, nodeIPv4),
							"cloud.network.openshift.io/egress-ipconfig": fmt.Sprintf(
								`[{"interface":"eth0","ifaddr":{"ipv4":"%s"},"capacity":{}}]`, nodeIPv4),
						},
						Labels: map[string]string{"k8s.ovn.org/egress-assignable": ""},
					},
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
						},
					},
				}
				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec:       egressipv1.EgressIPSpec{EgressIPs: []string{egressIPv6}},
				}

				fakeClusterManagerOVN.start(
					&egressipv1.EgressIPList{Items: []egressipv1.EgressIP{eIP}},
					&corev1.NodeList{Items: []corev1.Node{node}},
				)
				_, err := fakeClusterManagerOVN.eIPC.WatchEgressNodes()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				_, err = fakeClusterManagerOVN.eIPC.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				_, err = fakeClusterManagerOVN.eIPC.WatchCloudPrivateIPConfig()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Phase 1: wait for the node ADD event to be processed (node appears
				// in the allocator cache). The IPv6 EgressIP remains unassigned because
				// the node's cloud egress-ipconfig has no IPv6 subnet yet.
				gomega.Eventually(doesEgressIPAllocatorContainSafely).WithArguments(node1Name).Should(gomega.BeTrue())
				ipv6CPIPCName := ipStringToCloudPrivateIPConfigName(egressIPv6)
				gomega.Consistently(func() error {
					_, err := fakeClusterManagerOVN.fakeClient.CloudNetworkClient.CloudV1().
						CloudPrivateIPConfigs().Get(context.TODO(), ipv6CPIPCName, metav1.GetOptions{})
					return err
				}).ShouldNot(gomega.Succeed())

				// Phase 2: CNCC corrects the annotation to add the IPv6 subnet.
				node.Annotations["cloud.network.openshift.io/egress-ipconfig"] = fmt.Sprintf(
					`[{"interface":"eth0","ifaddr":{"ipv4":"%s","ipv6":"fc00:f853:ccd:e793::/64"},"capacity":{}}]`,
					nodeIPv4)
				_, err = fakeClusterManagerOVN.fakeClient.KubeClient.CoreV1().Nodes().Update(
					context.TODO(), &node, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// The annotation change must trigger addEgressNode → reconcileEgressIP
				// runs for the still-unassigned IPv6 EgressIP → CloudPrivateIPConfig
				// is created, proving the early-return guard was not hit.
				gomega.Eventually(func() error {
					_, err := fakeClusterManagerOVN.fakeClient.CloudNetworkClient.CloudV1().
						CloudPrivateIPConfigs().Get(context.TODO(), ipv6CPIPCName, metav1.GetOptions{})
					return err
				}).Should(gomega.Succeed())

				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("unrelated annotation update should not trigger egress IP re-evaluation on cloud platform", func() {
			// Counterpart to the test above: changing an annotation that is neither
			// cloud.network.openshift.io/egress-ipconfig, k8s.ovn.org/host-cidrs,
			// nor node readiness must NOT call addEgressNode (early-return guard).
			app.Action = func(*cli.Context) error {
				config.Kubernetes.PlatformType = string(ocpconfigapi.AWSPlatformType)

				nodeIPv4 := "192.168.126.12/24"
				egressIPv6 := "fc00:f853:ccd:e793::101"

				node := corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: node1Name,
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf(`{"ipv4": "%s", "ipv6": ""}`, nodeIPv4),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf(`{"default":["%s"]}`, v4NodeSubnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf(`["%s"]`, nodeIPv4),
							"cloud.network.openshift.io/egress-ipconfig": fmt.Sprintf(
								`[{"interface":"eth0","ifaddr":{"ipv4":"%s"},"capacity":{}}]`, nodeIPv4),
						},
						Labels: map[string]string{"k8s.ovn.org/egress-assignable": ""},
					},
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
						},
					},
				}
				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec:       egressipv1.EgressIPSpec{EgressIPs: []string{egressIPv6}},
				}

				fakeClusterManagerOVN.start(
					&egressipv1.EgressIPList{Items: []egressipv1.EgressIP{eIP}},
					&corev1.NodeList{Items: []corev1.Node{node}},
				)
				_, err := fakeClusterManagerOVN.eIPC.WatchEgressNodes()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				_, err = fakeClusterManagerOVN.eIPC.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				_, err = fakeClusterManagerOVN.eIPC.WatchCloudPrivateIPConfig()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Wait for the node ADD event to be fully processed before sending
				// the unrelated UPDATE.
				gomega.Eventually(doesEgressIPAllocatorContainSafely).WithArguments(node1Name).Should(gomega.BeTrue())

				// Touch an unrelated annotation — readiness, host-cidrs, and
				// cloud egress-ipconfig are all unchanged.
				node.Annotations["test"] = "dummy"
				_, err = fakeClusterManagerOVN.fakeClient.KubeClient.CoreV1().Nodes().Update(
					context.TODO(), &node, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// IPv6 CloudPrivateIPConfig must remain absent throughout — the
				// early-return guard should fire since no relevant annotation changed.
				ipv6CPIPCName := ipStringToCloudPrivateIPConfigName(egressIPv6)
				gomega.Consistently(func() error {
					_, err := fakeClusterManagerOVN.fakeClient.CloudNetworkClient.CloudV1().
						CloudPrivateIPConfigs().Get(context.TODO(), ipv6CPIPCName, metav1.GetOptions{})
					return err
				}).ShouldNot(gomega.Succeed())

				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})

	ginkgo.Context("IPv6 assignment", func() {

		ginkgo.It("should be able to allocate non-conflicting IP on node with lowest amount of allocations", func() {
			app.Action = func(*cli.Context) error {

				egressIP := "0:0:0:0:0:feff:c0a8:8e0f"
				node1IPv4 := ""
				node1IPv6 := "0:0:0:0:0:feff:c0a8:8e0c/64"
				node2IPv4 := ""
				node2IPv6 := "0:0:0:0:0:fedf:c0a8:8e0c/64"

				node1 := corev1.Node{
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
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
							},
						},
					},
				}
				node2 := corev1.Node{
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
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
							},
						},
					},
				}

				fakeClusterManagerOVN.start(&corev1.NodeList{
					Items: []corev1.Node{node1, node2},
				})

				egressNode1 := setupNode(node1Name, []string{"0:0:0:0:0:feff:c0a8:8e0c/64"}, map[string]string{"0:0:0:0:0:feff:c0a8:8e32": "bogus1", "0:0:0:0:0:feff:c0a8:8e1e": "bogus2"})
				egressNode2 := setupNode(node2Name, []string{"0:0:0:0:0:fedf:c0a8:8e0c/64"}, map[string]string{"0:0:0:0:0:feff:c0a8:8e23": "bogus3"})

				fakeClusterManagerOVN.eIPC.nodeAllocator.cache[egressNode1.name] = &egressNode1
				fakeClusterManagerOVN.eIPC.nodeAllocator.cache[egressNode2.name] = &egressNode2

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
			app.Action = func(*cli.Context) error {
				node1IPv4OVN := ""
				node1IPv6OVN := "0:0:0:0:0:feff:c0a8:8e0c/64"
				node1IPv6SecondaryHost := "0:0:1:0:0:feff:c0a8:8e0c/64"
				node2IPv4OVN := ""
				node2IPv6OVN := "0:0:0:0:0:fedf:c0a8:8e0c/64"
				node2IPv6SecondaryHost := "0:0:1:0:0:fedf:c0a8:8e0c/64"

				node1 := corev1.Node{
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
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
							},
						},
					},
				}
				node2 := corev1.Node{
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
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
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
					&corev1.NodeList{Items: []corev1.Node{node1, node2}},
					&egressipv1.EgressIPList{Items: []egressipv1.EgressIP{eIP}},
				)

				egressNode1 := setupNode(node1Name, []string{node1IPv6OVN}, map[string]string{"0:0:0:0:0:feff:c0a8:8e32": "bogus1", "0:0:0:0:0:feff:c0a8:8e1e": "bogus2"})
				egressNode2 := setupNode(node2Name, []string{node2IPv6OVN}, map[string]string{"0:0:0:0:0:feff:c0a8:8e23": "bogus3"})

				fakeClusterManagerOVN.eIPC.nodeAllocator.cache[egressNode1.name] = &egressNode1
				fakeClusterManagerOVN.eIPC.nodeAllocator.cache[egressNode2.name] = &egressNode2

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
			app.Action = func(*cli.Context) error {
				node1IPv4OVN := ""
				node1IPv6OVN := "0:0:0:0:0:feff:c0a8:8e0c/64"
				node1IPv6SecondaryHost := "0:0:1:0:0:feff:c0a8:8e0c/64"
				node2IPv4OVN := ""
				node2IPv6OVN := "0:0:0:0:0:fedf:c0a8:8e0c/64"
				node2IPv6SecondaryHost := "0:0:1:0:0:fedf:c0a8:8e0c/64"

				node1 := corev1.Node{
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
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
							},
						},
					},
				}
				node2 := corev1.Node{
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
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
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
					&corev1.NodeList{Items: []corev1.Node{node1, node2}},
					&egressipv1.EgressIPList{Items: []egressipv1.EgressIP{eIP}},
				)

				egressNode1 := setupNode(node1Name, []string{node1IPv4OVN}, map[string]string{"0:0:0:0:0:feff:c0a8:8e32": "bogus1", "0:0:0:0:0:feff:c0a8:8e1e": "bogus2"})
				egressNode2 := setupNode(node2Name, []string{node2IPv4OVN}, map[string]string{"0:0:0:0:0:feff:c0a8:8e23": "bogus3"})

				fakeClusterManagerOVN.eIPC.nodeAllocator.cache[egressNode1.name] = &egressNode1
				fakeClusterManagerOVN.eIPC.nodeAllocator.cache[egressNode2.name] = &egressNode2

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
			app.Action = func(*cli.Context) error {
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

				node1 := corev1.Node{
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
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
							},
						},
					},
				}
				node2 := corev1.Node{
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
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
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
					&corev1.NodeList{Items: []corev1.Node{node1, node2}},
					&egressipv1.EgressIPList{Items: []egressipv1.EgressIP{eIP}},
				)

				egressNode1 := setupNode(node1Name, []string{node1IPv4OVN}, map[string]string{"0:0:0:0:0:feff:c0a8:8e32": "bogus1", "0:0:0:0:0:feff:c0a8:8e1e": "bogus2"})
				egressNode2 := setupNode(node2Name, []string{node2IPv4OVN}, map[string]string{"0:0:0:0:0:feff:c0a8:8e23": "bogus3"})

				fakeClusterManagerOVN.eIPC.nodeAllocator.cache[egressNode1.name] = &egressNode1
				fakeClusterManagerOVN.eIPC.nodeAllocator.cache[egressNode2.name] = &egressNode2

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
			app.Action = func(*cli.Context) error {
				node1IPv4OVN := ""
				node1IPv6OVN := "0:0:0:0:0:feff:c0a8:8e0c/64"
				node1IPv6SecondaryHost := "0:0:1:0:0:feff:c0a8:8e0c/64"
				node2IPv4OVN := ""
				node2IPv6OVN := "0:0:0:0:0:feff:c0a8:8e0b/64"
				node2IPv6SecondaryHost := "0:0:1:0:0:feff:c0a8:8e0b/64"

				node1 := corev1.Node{
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
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
							},
						},
					},
				}
				node2 := corev1.Node{
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
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
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
					&corev1.NodeList{Items: []corev1.Node{node1, node2}},
					&egressipv1.EgressIPList{Items: []egressipv1.EgressIP{eIP}},
				)

				egressNode1 := setupNode(node1Name, []string{node1IPv4OVN}, map[string]string{"0:0:0:0:0:feff:c0a8:8e32": "bogus1", "0:0:0:0:0:feff:c0a8:8e1e": "bogus2"})
				egressNode2 := setupNode(node2Name, []string{node2IPv4OVN}, map[string]string{"0:0:0:0:0:feff:c0a8:8e23": "bogus3"})

				fakeClusterManagerOVN.eIPC.nodeAllocator.cache[egressNode1.name] = &egressNode1
				fakeClusterManagerOVN.eIPC.nodeAllocator.cache[egressNode2.name] = &egressNode2

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
			app.Action = func(*cli.Context) error {

				egressIP := "0:0:0:0:0:feff:c0a8:8e0c"
				node1IPv4 := ""
				node1IPv6 := egressIP + "/64"
				node2IPv4 := ""
				node2IPv6 := "0:0:0:0:0:fedf:c0a8:8e0c/64"

				node1 := corev1.Node{
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
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
							},
						},
					},
				}
				node2 := corev1.Node{
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
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
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
					&corev1.NodeList{Items: []corev1.Node{node1, node2}},
					&egressipv1.EgressIPList{Items: []egressipv1.EgressIP{eIP}},
				)

				egressNode1 := setupNode(node1Name, []string{egressIP + "/64"}, map[string]string{"0:0:0:0:0:feff:c0a8:8e32": "bogus1", "0:0:0:0:0:feff:c0a8:8e1e": "bogus2"})
				egressNode2 := setupNode(node2Name, []string{"0:0:0:0:0:fedf:c0a8:8e0c/64"}, map[string]string{"0:0:0:0:0:feff:c0a8:8e23": "bogus3"})

				fakeClusterManagerOVN.eIPC.nodeAllocator.cache[egressNode1.name] = &egressNode1
				fakeClusterManagerOVN.eIPC.nodeAllocator.cache[egressNode2.name] = &egressNode2

				assignedStatuses := fakeClusterManagerOVN.eIPC.assignEgressIPs(eIP.Name, eIP.Spec.EgressIPs)
				gomega.Expect(assignedStatuses).To(gomega.BeEmpty())

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should not be able to allocate EgressIP equal to an address in host-cidrs annotation", func() {
			app.Action = func(*cli.Context) error {

				egressIP := "0:0:0:0:0:feff:c0a8:8e0a"
				node1IPv4 := ""
				node1IPv6 := "0:0:0:0:0:fedf:c0a8:8e0b/64"
				node1IPv62 := "0:0:0:0:0:feff:c0a8:8e0a/64" // same addr as EIP
				node2IPv4 := ""
				node2IPv6 := "0:0:0:0:0:fedf:c0a8:8e0c/64"

				node1 := corev1.Node{
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
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
							},
						},
					},
				}
				node2 := corev1.Node{
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
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
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
					&corev1.NodeList{Items: []corev1.Node{node1, node2}},
					&egressipv1.EgressIPList{Items: []egressipv1.EgressIP{eIP}},
				)

				egressNode1 := setupNode(node1Name, []string{node1IPv6, node1IPv62}, map[string]string{"0:0:0:0:0:feff:c0a8:8e32": "bogus1", "0:0:0:0:0:feff:c0a8:8e1e": "bogus2"})
				egressNode2 := setupNode(node2Name, []string{node2IPv6}, map[string]string{"0:0:0:0:0:feff:c0a8:8e23": "bogus3"})

				fakeClusterManagerOVN.eIPC.nodeAllocator.cache[egressNode1.name] = &egressNode1
				fakeClusterManagerOVN.eIPC.nodeAllocator.cache[egressNode2.name] = &egressNode2

				gomega.Expect(fakeClusterManagerOVN.eIPC.initEgressIPAllocator(&node1)).To(gomega.Succeed())
				gomega.Expect(fakeClusterManagerOVN.eIPC.initEgressIPAllocator(&node2)).To(gomega.Succeed())
				assignedStatuses := fakeClusterManagerOVN.eIPC.assignEgressIPs(eIP.Name, eIP.Spec.EgressIPs)
				gomega.Expect(assignedStatuses).To(gomega.BeEmpty())

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should not be able to allocate conflicting compressed IP", func() {
			app.Action = func(*cli.Context) error {
				egressIP := "::feff:c0a8:8e32"
				node1IPv4 := ""
				node1IPv6 := "0:0:0:0:0:feff:c0a8:8e0c/64"
				node2IPv4 := ""
				node2IPv6 := "0:0:0:0:0:fedf:c0a8:8e0c/64"

				node1 := corev1.Node{
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
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
							},
						},
					},
				}
				node2 := corev1.Node{
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
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
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
					&corev1.NodeList{Items: []corev1.Node{node1, node2}},
					&egressipv1.EgressIPList{Items: []egressipv1.EgressIP{eIP}},
				)

				egressNode1 := setupNode(node1Name, []string{"0:0:0:0:0:feff:c0a8:8e0c/64"}, map[string]string{"0:0:0:0:0:feff:c0a8:8e32": "bogus1", "0:0:0:0:0:feff:c0a8:8e1e": "bogus2"})
				egressNode2 := setupNode(node2Name, []string{"0:0:0:0:0:fedf:c0a8:8e0c/64"}, map[string]string{"0:0:0:0:0:feff:c0a8:8e23": "bogus3"})

				fakeClusterManagerOVN.eIPC.nodeAllocator.cache[egressNode1.name] = &egressNode1
				fakeClusterManagerOVN.eIPC.nodeAllocator.cache[egressNode2.name] = &egressNode2

				assignedStatuses := fakeClusterManagerOVN.eIPC.assignEgressIPs(eIP.Name, eIP.Spec.EgressIPs)
				gomega.Expect(assignedStatuses).To(gomega.BeEmpty())
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should not be able to allocate IPv4 IP on nodes which can only host IPv6", func() {
			app.Action = func(*cli.Context) error {

				egressIP := "192.168.126.16"
				node1IPv4 := ""
				node1IPv6 := "0:0:0:0:0:feff:c0a8:8e0c/64"
				node2IPv4 := ""
				node2IPv6 := "0:0:0:0:0:fedf:c0a8:8e0c/64"

				node1 := corev1.Node{
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
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
							},
						},
					},
				}
				node2 := corev1.Node{
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
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
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
					&corev1.NodeList{Items: []corev1.Node{node1, node2}},
					&egressipv1.EgressIPList{Items: []egressipv1.EgressIP{eIP}},
				)

				egressNode1 := setupNode(node1Name, []string{"0:0:0:0:0:feff:c0a8:8e0c/64"}, map[string]string{"0:0:0:0:0:feff:c0a8:8e32": "bogus1", "0:0:0:0:0:feff:c0a8:8e1e": "bogus2"})
				egressNode2 := setupNode(node2Name, []string{"0:0:0:0:0:fedf:c0a8:8e0c/64"}, map[string]string{"0:0:0:0:0:feff:c0a8:8e23": "bogus3"})

				fakeClusterManagerOVN.eIPC.nodeAllocator.cache[egressNode1.name] = &egressNode1
				fakeClusterManagerOVN.eIPC.nodeAllocator.cache[egressNode2.name] = &egressNode2

				assignedStatuses := fakeClusterManagerOVN.eIPC.assignEgressIPs(eIP.Name, eIP.Spec.EgressIPs)
				gomega.Expect(assignedStatuses).To(gomega.BeEmpty())
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should be able to allocate non-conflicting compressed uppercase IP", func() {
			app.Action = func(*cli.Context) error {

				egressIP := "::FEFF:C0A8:8D32"
				node1IPv4 := ""
				node1IPv6 := "0:0:0:0:0:feff:c0a8:8e0c/64"
				node2IPv4 := ""
				node2IPv6 := "0:0:0:0:0:fedf:c0a8:8e0c/64"

				node1 := corev1.Node{
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
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
							},
						},
					},
				}
				node2 := corev1.Node{
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
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
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
					&corev1.NodeList{Items: []corev1.Node{node1, node2}},
					&egressipv1.EgressIPList{Items: []egressipv1.EgressIP{eIP}},
				)

				egressNode1 := setupNode(node1Name, []string{"0:0:0:0:0:feff:c0a8:8e0c/64"}, map[string]string{"0:0:0:0:0:feff:c0a8:8e32": "bogus1", "0:0:0:0:0:feff:c0a8:8e1e": "bogus2"})
				egressNode2 := setupNode(node2Name, []string{"0:0:0:0:0:fedf:c0a8:8e0c/64"}, map[string]string{"0:0:0:0:0:feff:c0a8:8e23": "bogus3"})

				fakeClusterManagerOVN.eIPC.nodeAllocator.cache[egressNode1.name] = &egressNode1
				fakeClusterManagerOVN.eIPC.nodeAllocator.cache[egressNode2.name] = &egressNode2

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
			app.Action = func(*cli.Context) error {

				egressIP := "::FEFF:C0A8:8E32"
				node1IPv4 := ""
				node1IPv6 := "0:0:0:0:0:feff:c0a8:8e0c/64"
				node2IPv4 := ""
				node2IPv6 := "0:0:0:0:0:fedf:c0a8:8e0c/64"

				node1 := corev1.Node{
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
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
							},
						},
					},
				}
				node2 := corev1.Node{
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
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
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
					&corev1.NodeList{Items: []corev1.Node{node1, node2}},
					&egressipv1.EgressIPList{Items: []egressipv1.EgressIP{eIP}},
				)

				egressNode1 := setupNode(node1Name, []string{"0:0:0:0:0:feff:c0a8:8e0c/64"}, map[string]string{"0:0:0:0:0:feff:c0a8:8e32": "bogus1", "0:0:0:0:0:feff:c0a8:8e1e": "bogus2"})
				egressNode2 := setupNode(node2Name, []string{"0:0:0:0:0:fedf:c0a8:8e0c/64"}, map[string]string{"0:0:0:0:0:feff:c0a8:8e23": "bogus3"})

				fakeClusterManagerOVN.eIPC.nodeAllocator.cache[egressNode1.name] = &egressNode1
				fakeClusterManagerOVN.eIPC.nodeAllocator.cache[egressNode2.name] = &egressNode2

				assignedStatuses := fakeClusterManagerOVN.eIPC.assignEgressIPs(eIP.Name, eIP.Spec.EgressIPs)
				gomega.Expect(assignedStatuses).To(gomega.BeEmpty())
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should not be able to allocate invalid IP", func() {
			app.Action = func(*cli.Context) error {

				egressIPs := []string{"0:0:0:0:0:feff:c0a8:8e32:5"}
				node1IPv4 := ""
				node1IPv6 := "0:0:0:0:0:feff:c0a8:8e0c/64"
				node2IPv4 := ""
				node2IPv6 := "0:0:0:0:0:fedf:c0a8:8e0c/64"

				node1 := corev1.Node{
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
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
							},
						},
					},
				}
				node2 := corev1.Node{
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
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
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
					&corev1.NodeList{Items: []corev1.Node{node1, node2}},
					&egressipv1.EgressIPList{Items: []egressipv1.EgressIP{eIP}},
				)

				assignedStatuses, err := fakeClusterManagerOVN.eIPC.validateEgressIPSpec(eIP.Name, eIP.Spec.EgressIPs)
				gomega.Expect(err).To(gomega.HaveOccurred())
				gomega.Expect(err.Error()).To(gomega.Equal(fmt.Sprintf("unable to parse provided EgressIP: %s, invalid", egressIPs[0])))
				gomega.Expect(assignedStatuses).To(gomega.BeEmpty())
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})

	ginkgo.Context("Dual-stack assignment", func() {

		ginkgo.It("should be able to allocate non-conflicting IPv4 on node which can host it, even if it happens to be the node with more assignments", func() {
			app.Action = func(*cli.Context) error {
				egressIP := "192.168.126.99"
				node1IPv4 := ""
				node1IPv6 := "0:0:0:0:0:feff:c0a8:8e0c/64"
				node2IPv4 := "192.168.126.51/24"
				node2IPv6 := ""

				node1 := corev1.Node{
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
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
							},
						},
					},
				}
				node2 := corev1.Node{
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
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
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
					&corev1.NodeList{Items: []corev1.Node{node1, node2}},
					&egressipv1.EgressIPList{Items: []egressipv1.EgressIP{eIP}},
				)

				egressNode1 := setupNode(node1Name, []string{"0:0:0:0:0:feff:c0a8:8e0c/64"}, map[string]string{"0:0:0:0:0:feff:c0a8:8e23": "bogus1"})
				egressNode2 := setupNode(node2Name, []string{"192.168.126.51/24"}, map[string]string{"192.168.126.68": "bogus1", "192.168.126.102": "bogus2"})

				fakeClusterManagerOVN.eIPC.nodeAllocator.cache[egressNode1.name] = &egressNode1
				fakeClusterManagerOVN.eIPC.nodeAllocator.cache[egressNode2.name] = &egressNode2

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
			app.Action = func(*cli.Context) error {

				egressIPs := []string{"192.168.126.99/32"}
				node1IPv4 := "192.168.126.12/24"
				node2IPv4 := "192.168.126.51/24"

				node1 := corev1.Node{
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
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
							},
						},
					},
				}
				node2 := corev1.Node{
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
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
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
					&corev1.NodeList{Items: []corev1.Node{node1, node2}},
					&egressipv1.EgressIPList{Items: []egressipv1.EgressIP{eIP}},
				)

				egressNode1 := setupNode(node1Name, []string{node1IPv4}, map[string]string{"192.168.126.102": "bogus1", "192.168.126.111": "bogus2"})
				egressNode2 := setupNode(node2Name, []string{node2IPv4}, map[string]string{"192.168.126.68": "bogus3"})

				fakeClusterManagerOVN.eIPC.nodeAllocator.cache[egressNode1.name] = &egressNode1
				fakeClusterManagerOVN.eIPC.nodeAllocator.cache[egressNode2.name] = &egressNode2

				validatedIPs, err := fakeClusterManagerOVN.eIPC.validateEgressIPSpec(eIP.Name, eIP.Spec.EgressIPs)
				gomega.Expect(err).To(gomega.HaveOccurred())
				gomega.Expect(err.Error()).To(gomega.Equal(fmt.Sprintf("unable to parse provided EgressIP: %s, invalid", egressIPs[0])))
				gomega.Expect(validatedIPs).To(gomega.BeEmpty())
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

	})

	ginkgo.Context("WatchEgressIP", func() {

		ginkgo.It("should update status correctly for single-stack IPv4", func() {
			app.Action = func(*cli.Context) error {
				egressIP := "192.168.126.10"
				node1IPv4 := "192.168.126.12/24"
				node2IPv4 := "192.168.126.51/24"

				node1 := corev1.Node{
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
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
							},
						},
					},
				}
				node2 := corev1.Node{
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
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
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
					&corev1.NodeList{Items: []corev1.Node{node1, node2}},
					&egressipv1.EgressIPList{Items: []egressipv1.EgressIP{eIP}},
				)

				egressNode1 := setupNode(node1Name, []string{node1IPv4}, map[string]string{"192.168.126.102": "bogus1", "192.168.126.111": "bogus2"})
				egressNode2 := setupNode(node2Name, []string{node2IPv4}, map[string]string{"192.168.126.68": "bogus3"})

				fakeClusterManagerOVN.eIPC.nodeAllocator.cache[egressNode1.name] = &egressNode1
				fakeClusterManagerOVN.eIPC.nodeAllocator.cache[egressNode2.name] = &egressNode2

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
			app.Action = func(*cli.Context) error {
				egressIP := "0:0:0:0:0:feff:c0a8:8e0d"
				node1IPv4 := ""
				node1IPv6 := "0:0:0:0:0:feff:c0a8:8e0c/64"
				node2IPv4 := ""
				node2IPv6 := "0:0:0:0:0:fedf:c0a8:8e0c/64"

				node1 := corev1.Node{
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
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
							},
						},
					},
				}
				node2 := corev1.Node{
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
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
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
					&corev1.NodeList{Items: []corev1.Node{node1, node2}},
					&egressipv1.EgressIPList{Items: []egressipv1.EgressIP{eIP}},
				)

				egressNode1 := setupNode(node1Name, []string{"0:0:0:0:0:feff:c0a8:8e0c/64"}, map[string]string{"0:0:0:0:0:feff:c0a8:8e32": "bogus1", "0:0:0:0:0:feff:c0a8:8e1e": "bogus2"})
				egressNode2 := setupNode(node2Name, []string{"0:0:0:0:0:fedf:c0a8:8e0c/64"}, map[string]string{"0:0:0:0:0:feff:c0a8:8e23": "bogus3"})

				fakeClusterManagerOVN.eIPC.nodeAllocator.cache[egressNode1.name] = &egressNode1
				fakeClusterManagerOVN.eIPC.nodeAllocator.cache[egressNode2.name] = &egressNode2

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
			app.Action = func(*cli.Context) error {
				egressIPv4 := "192.168.126.101"
				egressIPv6 := "::feff:c0a8:8e0d"
				node1IPv6 := "::feff:c0a8:8e0c/64"
				node2IPv4 := "192.168.126.51/24"

				node1 := corev1.Node{
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
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
							},
						},
					},
				}
				node2 := corev1.Node{
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
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
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
					&corev1.NodeList{Items: []corev1.Node{node1, node2}},
					&egressipv1.EgressIPList{Items: []egressipv1.EgressIP{eIP}},
				)

				egressNode1 := setupNode(node1Name, []string{node1IPv6}, map[string]string{"0:0:0:0:0:feff:c0a8:8e23": "bogus1"})
				egressNode2 := setupNode(node2Name, []string{node2IPv4}, map[string]string{"192.168.126.68": "bogus2", "192.168.126.102": "bogus3"})

				fakeClusterManagerOVN.eIPC.nodeAllocator.cache[egressNode1.name] = &egressNode1
				fakeClusterManagerOVN.eIPC.nodeAllocator.cache[egressNode2.name] = &egressNode2

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

		ginkgo.It("should manage mark cache for add/update/delete", func() {
			app.Action = func(*cli.Context) error {
				egressIP := "192.168.126.10"
				node1IPv4 := "192.168.126.12/24"
				node2IPv4 := "192.168.126.51/24"

				node1 := corev1.Node{
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
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
							},
						},
					},
				}
				node2 := corev1.Node{
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
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
							},
						},
					},
				}

				eIP := &egressipv1.EgressIP{
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
					&corev1.NodeList{Items: []corev1.Node{node1, node2}},
				)

				egressNode1 := setupNode(node1Name, []string{node1IPv4}, map[string]string{"192.168.126.102": "bogus1", "192.168.126.111": "bogus2"})
				egressNode2 := setupNode(node2Name, []string{node2IPv4}, map[string]string{"192.168.126.68": "bogus3"})

				fakeClusterManagerOVN.eIPC.nodeAllocator.cache[egressNode1.name] = &egressNode1
				fakeClusterManagerOVN.eIPC.nodeAllocator.cache[egressNode2.name] = &egressNode2

				_, err := fakeClusterManagerOVN.eIPC.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				eIP, err = fakeClusterManagerOVN.fakeClient.EgressIPClient.K8sV1().EgressIPs().Create(context.TODO(), eIP, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred(), "failed to create EgressIP")

				gomega.Eventually(getEgressIPAnnotationValue(eIP.Name)).ShouldNot(gomega.BeEmpty())
				assignedMarkStr, err := getEgressIPAnnotationValue(eIP.Name)()
				gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "failed to get egress IP mark from annotations")
				assignedMark, err := strconv.Atoi(assignedMarkStr)
				gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "failed to convert mark to string")

				ginkgo.By("confirm cache is set correctly")
				cachedMark, _, err := fakeClusterManagerOVN.eIPC.getOrAllocMark(eIP.Name)
				gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
				gomega.Expect(cachedMark).Should(gomega.Equal(assignedMark), "EIP annotation and cache mark integer must be the same")
				ginkgo.By("deleted EgressIP removes cache entry")
				err = fakeClusterManagerOVN.fakeClient.EgressIPClient.K8sV1().EgressIPs().Delete(context.TODO(), eIP.Name, metav1.DeleteOptions{})
				gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "Deletion of EIP must succeed")
				gomega.Eventually(func() int {
					return fakeClusterManagerOVN.eIPC.markAllocator.GetID(eIP.Name)
				}).Should(gomega.Equal(types.InvalidID))
				// The released mark must also be available to another EgressIP.
				gomega.Expect(fakeClusterManagerOVN.eIPC.reserveMark("another-egressip", assignedMark)).To(gomega.Succeed())
				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})

	ginkgo.Context("syncEgressIP for dual-stack", func() {

		// This test validates that if the allocator cache contains valid entries that match
		// the egress IP status items, no reassignment shall happen.
		// In order to do so, it does 2 comparisons:
		// a) take egressIP that's passed to the cluster manager and compare it to the list of EgressIPs after running
		//    WatchEgressIP.
		// b) take egressNode.allocations before running WatchEgressIP and make sure that they haven't changed.
		ginkgo.It("should not reallocate if cache matches EgressIP status", func() {
			app.Action = func(*cli.Context) error {
				config.IPv6Mode = true
				config.IPv4Mode = true
				egressIPv4 := "192.168.126.101"
				egressIPv6 := "0:0:0:0:0:feff:c0a8:8e0d"
				node1IPv6 := "0:0:0:0:0:feff:c0a8:8e0b/64"
				node2IPv6 := "0:0:0:0:0:feff:c0a8:8e0c/64"
				node1IPv4 := "192.168.126.50/16"
				node2IPv4 := "192.168.126.51/16"

				node1 := corev1.Node{
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
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
							},
						},
					},
				}
				node2 := corev1.Node{
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
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
							},
						},
					},
				}

				egressNode1 := setupNode(node1Name, []string{node1IPv4, node1IPv6}, map[string]string{
					net.ParseIP(egressIPv6).String(): egressIPName, // Cache matches eIP Status.
				})
				egressNode2 := setupNode(node2Name, []string{node2IPv4, node2IPv6}, map[string]string{
					"192.168.126.102": "bogus3",
					egressIPv4:        egressIPName, // Cache matches eIP Status.
				})
				egressNode1OriginalAllocations := maps.Clone(egressNode1.allocations)
				egressNode2OriginalAllocations := maps.Clone(egressNode2.allocations)

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
					&corev1.NodeList{Items: []corev1.Node{node1, node2}},
					&egressipv1.EgressIPList{Items: []egressipv1.EgressIP{eIP}},
				)

				fakeClusterManagerOVN.eIPC.nodeAllocator.cache[egressNode1.name] = &egressNode1
				fakeClusterManagerOVN.eIPC.nodeAllocator.cache[egressNode2.name] = &egressNode2

				_, err := fakeClusterManagerOVN.eIPC.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(egressIPsMatch([]egressipv1.EgressIP{eIP})).Should(gomega.BeTrue())
				gomega.Eventually(readAllocations).WithArguments(egressNode1.name).Should(
					gomega.Equal(egressNode1OriginalAllocations))
				gomega.Eventually(readAllocations).WithArguments(egressNode2.name).Should(
					gomega.Equal(egressNode2OriginalAllocations))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})

	ginkgo.Context("syncEgressIP for IPv4", func() {

		ginkgo.It("should update invalid assignments on duplicated node", func() {
			app.Action = func(*cli.Context) error {
				egressIP1 := "192.168.126.101"
				egressIP2 := "192.168.126.100"
				node1IPv4 := "192.168.126.12/24"
				node1IPv6 := ""
				node2IPv4 := "192.168.126.51/24"
				node2IPv6 := ""

				node1 := corev1.Node{
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
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
							},
						},
					},
				}
				node2 := corev1.Node{
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
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
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
					&corev1.NodeList{Items: []corev1.Node{node1, node2}},
					&egressipv1.EgressIPList{Items: []egressipv1.EgressIP{eIP}},
				)

				fakeClusterManagerOVN.eIPC.nodeAllocator.cache[egressNode1.name] = &egressNode1
				fakeClusterManagerOVN.eIPC.nodeAllocator.cache[egressNode2.name] = &egressNode2

				_, err := fakeClusterManagerOVN.eIPC.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(2))
				egressIPs, nodes := getEgressIPStatus(egressIPName)
				gomega.Expect(nodes).To(gomega.ConsistOf(egressNode1.name, egressNode2.name))
				gomega.Expect(egressIPs).To(gomega.ConsistOf(eIP.Status.Items[0].EgressIP, eIP.Status.Items[1].EgressIP))
				// give some time for event handler to be added and finish processing initial updates
				time.Sleep(3 * time.Second)
				realEIP, err := fakeClusterManagerOVN.fakeClient.EgressIPClient.K8sV1().EgressIPs().Get(context.TODO(), egressIPName, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				ginkgo.By("Simulate lagging informer and send an event with outdated status")
				eIP.Annotations = make(map[string]string)
				for k, v := range realEIP.Annotations {
					eIP.Annotations[k] = v
				}
				eIP.Status = egressipv1.EgressIPStatus{
					Items: []egressipv1.EgressIPStatusItem{
						{
							EgressIP: egressIP2,
							Node:     egressNode1.name,
						},
						{
							EgressIP: egressIP1,
							Node:     egressNode1.name,
						},
					},
				}
				_, err = fakeClusterManagerOVN.fakeClient.EgressIPClient.K8sV1().EgressIPs().Update(context.TODO(), &eIP, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// give sometime for events to be processed
				time.Sleep(3 * time.Second)
				ginkgo.By("Simulate lagging informer and send an event with real updated status")
				_, err = fakeClusterManagerOVN.fakeClient.EgressIPClient.K8sV1().EgressIPs().Update(context.TODO(), realEIP, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// give sometime for events to be processed
				time.Sleep(3 * time.Second)
				gomega.Eventually(func() error {
					defer ginkgo.GinkgoRecover()
					tmp, err := fakeClusterManagerOVN.fakeClient.EgressIPClient.K8sV1().EgressIPs().Get(context.TODO(), egressIPName, metav1.GetOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					var egressIPs, nodes []string
					for _, status := range tmp.Status.Items {
						egressIPs = append(egressIPs, status.EgressIP)
						nodes = append(nodes, status.Node)
					}
					gomega.Expect(nodes).To(gomega.ConsistOf(egressNode1.name, egressNode2.name))
					gomega.Expect(egressIPs).To(gomega.ConsistOf(eIP.Status.Items[0].EgressIP, eIP.Status.Items[1].EgressIP))
					return nil
				}).Should(gomega.Succeed())

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should update invalid assignments with incorrectly parsed IP", func() {
			app.Action = func(*cli.Context) error {
				egressIP1 := "192.168.126.101"
				egressIPIncorrect := "192.168.126.1000"
				node1IPv4 := "192.168.126.12/24"
				node2IPv4 := "192.168.126.51/24"

				node1 := corev1.Node{
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
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
							},
						},
					},
				}
				node2 := corev1.Node{
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
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
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
					&corev1.NodeList{Items: []corev1.Node{node1, node2}},
					&egressipv1.EgressIPList{Items: []egressipv1.EgressIP{eIP}},
				)

				fakeClusterManagerOVN.eIPC.nodeAllocator.cache[egressNode1.name] = &egressNode1
				fakeClusterManagerOVN.eIPC.nodeAllocator.cache[egressNode2.name] = &egressNode2

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
			app.Action = func(*cli.Context) error {
				egressIP1 := "192.168.126.101"
				egressIPIncorrect := "192.168.128.100"
				node1IPv4 := "192.168.126.12/24"
				node2IPv4 := "192.168.126.51/24"

				node1 := corev1.Node{
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
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
							},
						},
					},
				}
				node2 := corev1.Node{
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
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
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
					&corev1.NodeList{Items: []corev1.Node{node1, node2}},
					&egressipv1.EgressIPList{Items: []egressipv1.EgressIP{eIP}},
				)

				fakeClusterManagerOVN.eIPC.nodeAllocator.cache[egressNode1.name] = &egressNode1
				fakeClusterManagerOVN.eIPC.nodeAllocator.cache[egressNode2.name] = &egressNode2

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
			app.Action = func(*cli.Context) error {
				config.Kubernetes.PlatformType = string(ocpconfigapi.AWSPlatformType)
				node1IPv4 := "192.168.126.12/24"
				node2IPv4 := "192.168.126.51/24"
				egressIP1 := "192.168.126.101"
				egressIP2 := "192.168.126.100"
				node1 := corev1.Node{
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
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
							},
						},
					},
				}
				node2 := corev1.Node{
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
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
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
					&corev1.NodeList{
						Items: []corev1.Node{node1, node2},
					},
				)

				fakeClusterManagerOVN.eIPC.nodeAllocator.cache[egressNode1.name] = &egressNode1
				fakeClusterManagerOVN.eIPC.nodeAllocator.cache[egressNode2.name] = &egressNode2

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
			app.Action = func(*cli.Context) error {
				config.Kubernetes.PlatformType = string(ocpconfigapi.AWSPlatformType)

				egressIP := "192.168.126.101"
				node1IPv4 := "192.168.126.12/24"
				node2IPv4 := "192.168.126.51/24"

				node1 := corev1.Node{
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
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
							},
						},
					},
				}
				node2 := corev1.Node{
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
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
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
					&corev1.NodeList{
						Items: []corev1.Node{node1, node2},
					},
				)

				fakeClusterManagerOVN.eIPC.nodeAllocator.cache[egressNode1.name] = &egressNode1
				fakeClusterManagerOVN.eIPC.nodeAllocator.cache[egressNode2.name] = &egressNode2

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
			app.Action = func(*cli.Context) error {
				config.Kubernetes.PlatformType = string(ocpconfigapi.AWSPlatformType)
				node1IPv4 := "192.168.126.12/24"
				node2IPv4 := "192.168.126.51/24"
				egressIP1 := "192.168.126.101"
				egressIP2 := "192.168.126.100"

				node1 := corev1.Node{
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
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
							},
						},
					},
				}
				node2 := corev1.Node{
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
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
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
					&corev1.NodeList{
						Items: []corev1.Node{node1, node2},
					},
				)

				fakeClusterManagerOVN.eIPC.nodeAllocator.cache[egressNode1.name] = &egressNode1
				fakeClusterManagerOVN.eIPC.nodeAllocator.cache[egressNode2.name] = &egressNode2

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

		// This test validates that if the allocator cache contains valid entries that match
		// the egress IP status items, no reassignment shall happen.
		// In order to do so, it does 2 comparisons:
		// a) take egressIP that's passed to the cluster manager and compare it to the list of EgressIPs after running
		//    WatchEgressIP.
		// b) take egressNode.allocations before running WatchEgressIP and make sure that they haven't changed.
		ginkgo.It("should not reallocate if cache matches EgressIP status", func() {
			app.Action = func(*cli.Context) error {
				egressIP1 := "192.168.126.101"
				node1IPv4 := "192.168.126.12/24"
				node2IPv4 := "192.168.126.51/24"

				node1 := corev1.Node{
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
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
							},
						},
					},
				}
				node2 := corev1.Node{
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
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
							},
						},
					},
				}

				egressNode1 := setupNode(node1Name, []string{"192.168.126.12/24"}, map[string]string{
					"192.168.126.111": "bogus2",
					egressIP1:         egressIPName, // Cache matches eIP Status.
				})
				egressNode2 := setupNode(node2Name, []string{"192.168.126.51/24"}, map[string]string{
					"192.168.126.68": "bogus3",
				})
				egressNode1OriginalAllocations := maps.Clone(egressNode1.allocations)
				egressNode2OriginalAllocations := maps.Clone(egressNode2.allocations)

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
					&corev1.NodeList{Items: []corev1.Node{node1, node2}},
					&egressipv1.EgressIPList{Items: []egressipv1.EgressIP{eIP}},
				)

				fakeClusterManagerOVN.eIPC.nodeAllocator.cache[egressNode1.name] = &egressNode1
				fakeClusterManagerOVN.eIPC.nodeAllocator.cache[egressNode2.name] = &egressNode2

				_, err := fakeClusterManagerOVN.eIPC.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(egressIPsMatch([]egressipv1.EgressIP{eIP})).Should(gomega.BeTrue())
				gomega.Eventually(readAllocations).WithArguments(egressNode1.name).Should(
					gomega.Equal(egressNode1OriginalAllocations))
				gomega.Eventually(readAllocations).WithArguments(egressNode2.name).Should(
					gomega.Equal(egressNode2OriginalAllocations))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should assign new marks and add existing marks to mark cache", func() {
			app.Action = func(*cli.Context) error {
				eIPMark := util.EgressIPMarkBase + 100 // mark to be added to EIP obj before sync func is called
				eIPMarkStr := fmt.Sprintf("%d", eIPMark)
				objMeta := newEgressIPMeta(egressIPName)
				objMeta.Annotations = createAnnotWithMark(eIPMark)
				eIPWithMark := egressipv1.EgressIP{ObjectMeta: objMeta}
				eIP2WithoutMark := egressipv1.EgressIP{ObjectMeta: newEgressIPMeta(egressIPName2)}
				fakeClusterManagerOVN.start(&egressipv1.EgressIPList{Items: []egressipv1.EgressIP{eIPWithMark, eIP2WithoutMark}})
				_, err := fakeClusterManagerOVN.eIPC.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				ginkgo.By("ensure existing mark value isn't altered")
				gomega.Consistently(func() string {
					mark, err := getEgressIPAnnotationValue(eIPWithMark.Name)()
					gomega.Expect(err).Should(gomega.Succeed(), "failed to get mark from annotation")
					return mark
				}).Should(gomega.Equal(eIPMarkStr))
				ginkgo.By("ensure mark is written to EgressIP annotations")
				gomega.Consistently(func() string {
					mark, err := getEgressIPAnnotationValue(eIP2WithoutMark.Name)()
					gomega.Expect(err).Should(gomega.Succeed(), "failed to get mark from annotation")
					return mark
				}).ShouldNot(gomega.Equal(""))
				ginkgo.By("ensure mark cache contains correct values")
				eIP2MarkStr, err := getEgressIPAnnotationValue(eIP2WithoutMark.Name)()
				gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
				eIP2Mark, err := strconv.Atoi(eIP2MarkStr)
				gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
				// call allocate with the egress IP name and then ensure the returned mark is the same annotation on the EgressIP obj
				eIPMarkFromCache, _, err := fakeClusterManagerOVN.eIPC.getOrAllocMark(egressIPName)
				gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
				gomega.Expect(eIPMark).Should(gomega.Equal(eIPMarkFromCache))
				eIP2MarkFromCache, _, err := fakeClusterManagerOVN.eIPC.getOrAllocMark(egressIPName2)
				gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
				gomega.Expect(eIP2Mark).Should(gomega.Equal(eIP2MarkFromCache))
				gomega.Expect(eIPMarkFromCache).ShouldNot(gomega.Equal(eIP2MarkFromCache))
				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		// This test validates that when two EgressIP CRs have the same IP in their specs,
		// and one already has the IP assigned in status (from before restart), the sync
		// function properly pre-populates the allocator cache to prevent duplicate assignment.
		// This is a regression test for the bug where duplicate IPs were assigned during
		// control-plane pod restart because the allocator cache wasn't populated from
		// existing EgressIP statuses before processing individual ADD events.
		ginkgo.It("should not assign duplicate IP during restart when two EgressIPs have same IP in spec", func() {
			app.Action = func(*cli.Context) error {
				duplicateIP := "192.168.126.101"
				node1IPv4 := "192.168.126.12/24"
				node2IPv4 := "192.168.126.51/24"

				node1 := corev1.Node{
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
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
							},
						},
					},
				}
				node2 := corev1.Node{
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
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
							},
						},
					},
				}

				// eIP1 has the IP assigned in status (simulating state from before restart)
				eIP1 := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta("egressip-1"),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{duplicateIP},
					},
					Status: egressipv1.EgressIPStatus{
						Items: []egressipv1.EgressIPStatusItem{
							{
								EgressIP: duplicateIP,
								Node:     node1Name,
							},
						},
					},
				}

				// eIP2 has the same IP in spec but NOT in status (unassigned, but was created
				// with duplicate IP - which should have been rejected but wasn't due to a bug
				// or manual API manipulation)
				eIP2 := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta("egressip-2"),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{duplicateIP},
					},
					Status: egressipv1.EgressIPStatus{
						Items: []egressipv1.EgressIPStatusItem{},
					},
				}

				fakeClusterManagerOVN.start(
					&corev1.NodeList{Items: []corev1.Node{node1, node2}},
					// Both EgressIPs exist at startup - simulating restart scenario
					&egressipv1.EgressIPList{Items: []egressipv1.EgressIP{eIP1, eIP2}},
				)

				// Use WatchEgressNodes to properly initialize the allocator cache
				// (simulating real startup behavior rather than manually setting up cache)
				_, err := fakeClusterManagerOVN.eIPC.WatchEgressNodes()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				_, err = fakeClusterManagerOVN.eIPC.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// eIP1 should keep its assignment (the IP was already assigned)
				gomega.Eventually(getEgressIPStatusLen("egressip-1")).Should(gomega.Equal(1))
				egressIPs1, nodes1 := getEgressIPStatus("egressip-1")
				gomega.Expect(nodes1[0]).To(gomega.Equal(node1Name))
				gomega.Expect(egressIPs1[0]).To(gomega.Equal(duplicateIP))

				// eIP2 should NOT get the duplicate IP assigned (not even to node2) -
				// it should remain unassigned because initEgressNodeReachability pre-populated the
				// cache with eIP1's assignment
				gomega.Eventually(getEgressIPStatusLen("egressip-2")).Should(gomega.Equal(0))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})

	ginkgo.Context("AddEgressIP for IPv4", func() {

		ginkgo.It("should not create two EgressIPs with same egress IP value", func() {
			app.Action = func(*cli.Context) error {
				egressIP1 := "192.168.126.101"
				node1IPv4 := "192.168.126.12/24"
				node2IPv4 := "192.168.126.51/24"

				node1 := corev1.Node{
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
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
							},
						},
					},
				}
				node2 := corev1.Node{
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
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
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
					&corev1.NodeList{Items: []corev1.Node{node1, node2}},
					&egressipv1.EgressIPList{Items: []egressipv1.EgressIP{eIP1}},
				)

				fakeClusterManagerOVN.eIPC.nodeAllocator.cache[egressNode1.name] = &egressNode1
				fakeClusterManagerOVN.eIPC.nodeAllocator.cache[egressNode2.name] = &egressNode2

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
			app.Action = func(*cli.Context) error {
				egressIP := "192.168.126.101"
				updateEgressIP := "192.168.126.10"
				node1IPv4 := "192.168.126.12/24"
				node2IPv4 := "192.168.126.51/24"

				node1 := corev1.Node{
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
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
							},
						},
					},
				}
				node2 := corev1.Node{
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
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
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
					&corev1.NodeList{Items: []corev1.Node{node1, node2}},
					&egressipv1.EgressIPList{Items: []egressipv1.EgressIP{eIP1}},
				)

				fakeClusterManagerOVN.eIPC.nodeAllocator.cache[egressNode1.name] = &egressNode1
				fakeClusterManagerOVN.eIPC.nodeAllocator.cache[egressNode2.name] = &egressNode2
				_, err := fakeClusterManagerOVN.eIPC.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
				egressIPs, nodes := getEgressIPStatus(egressIPName)
				// store mark in-order to compare with mark after an update and ensure it has not been altered
				markBeforeUpdate, err := getEgressIPAnnotationValue(egressIPName)()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
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
				markAfterUpdate, err := getEgressIPAnnotationValue(egressIPName)()
				gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
				gomega.Expect(markBeforeUpdate).Should(gomega.Equal(markAfterUpdate), "The mark should be consistent")
				markInCache, _, err := fakeClusterManagerOVN.eIPC.getOrAllocMark(egressIPName)
				gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
				gomega.Expect(markAfterUpdate).Should(gomega.Equal(fmt.Sprintf("%d", markInCache)))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})

	ginkgo.Context("EgressIP Mark cache", func() {
		ginkgo.It("should round robin when mark range is exhausted", func() {
			nodeAlloc := getEgressIPMarkAllocator()
			ecc := &egressIPClusterController{markAllocator: nodeAlloc}
			eipMarkMin = 50000
			eipMarkMax = eipMarkMin + 50
			defer func() {
				eipMarkMax = util.EgressIPMarkMax
				eipMarkMin = util.EgressIPMarkBase
			}()
			for i := 0; i < 50; i++ {
				mark, _, err := ecc.getOrAllocMark(egressIPName)
				gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
				gomega.Expect(mark).Should(gomega.BeNumerically("<=", eipMarkMax), "mark should be less than allowable max")
				gomega.Expect(mark).Should(gomega.BeNumerically(">=", eipMarkMin), "mark should be greater or equal to allowable min")
				ecc.deallocMark(egressIPName)
			}
		})
	})
})
