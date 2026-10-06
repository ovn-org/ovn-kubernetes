// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/google/uuid"
	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/urfave/cli/v2"
	kubevirtv1 "kubevirt.io/api/core/v1"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	utilnet "k8s.io/utils/net"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	ipallocator "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/allocator/ip"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kubevirt"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/retry"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

type podCleanupFailureClient struct {
	libovsdbclient.Client
	failDeleteTable string
}

// Transact fails deletes from the selected table without disrupting other cleanup transactions.
func (c *podCleanupFailureClient) Transact(ctx context.Context, ops ...ovsdb.Operation) ([]ovsdb.OperationResult, error) {
	for _, op := range ops {
		if op.Op == ovsdb.OperationDelete && op.Table == c.failDeleteTable {
			return nil, fmt.Errorf("injected %s cleanup failure", c.failDeleteTable)
		}
	}
	return c.Client.Transact(ctx, ops...)
}

func getPodAnnotations(fakeClient kubernetes.Interface, namespace, name string) string {
	pod, err := fakeClient.CoreV1().Pods(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	return pod.Annotations[ovntypes.OvnPodAnnotationName]
}

func newNode(nodeName, nodeIPv4CIDR string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: nodeName,
			Annotations: map[string]string{
				"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", nodeIPv4CIDR, ""),
				"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":\"%s\"}", v4Node1Subnet),
				util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", nodeIPv4CIDR),
				util.OvnNodeChassisID:             chassisIDForNode(nodeName),
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
}

func newNodeNotEgressableV6Only(nodeName, nodeIPv6 string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: nodeName,
			Annotations: map[string]string{
				"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", "", nodeIPv6),
				"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":\"%s\"}", v6Node1Subnet),
				util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", nodeIPv6),
				util.OvnNodeChassisID:             chassisIDForNode(nodeName),
			},
		},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{
					Type:   corev1.NodeReady,
					Status: corev1.ConditionTrue,
				},
			},
			Addresses: []corev1.NodeAddress{
				{
					Type:    corev1.NodeInternalIP,
					Address: nodeIPv6,
				},
			},
		},
	}
}

type testPod struct {
	portUUID      string
	nodeName      string
	nodeChassisID string
	nodeSubnet    string
	nodeMgtIP     string
	nodeGWIP      string
	podName       string
	podIP         string
	hostNetwork   bool
	podMAC        string
	namespace     string
	portName      string
	routes        []util.PodRoute
	noIfaceIdVer  bool
	networkRole   string

	udnPodInfos map[string]*udnPodInfo
}

type udnPodInfo struct {
	nodeSubnet  string
	nodeMgtIP   string
	nodeGWIP    string
	role        string
	routes      []util.PodRoute
	allportInfo map[string]portInfo
}

type portInfo struct {
	portUUID  string
	podIP     string
	podMAC    string
	portName  string
	tunnelID  int
	prefixLen int
}

func chassisIDForNode(nodeName string) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(nodeName)).String()
}

func requestedChassisForPod(pod testPod) string {
	if pod.nodeChassisID != "" {
		return pod.nodeChassisID
	}
	if pod.nodeName == "" {
		return ""
	}
	return chassisIDForNode(pod.nodeName)
}

func newTPod(nodeName, nodeSubnet, nodeMgtIP, nodeGWIP, podName, podIPs, podMAC, namespace string) testPod {
	portName := util.GetLogicalPortName(namespace, podName)
	nodeChassisID := ""
	if nodeName != "" {
		nodeChassisID = chassisIDForNode(nodeName)
	}
	to := testPod{
		portUUID:      portName + "-UUID",
		nodeSubnet:    nodeSubnet,
		nodeMgtIP:     nodeMgtIP,
		nodeGWIP:      nodeGWIP,
		podIP:         podIPs,
		podMAC:        podMAC,
		portName:      portName,
		nodeName:      nodeName,
		nodeChassisID: nodeChassisID,
		podName:       podName,
		namespace:     namespace,
		udnPodInfos:   map[string]*udnPodInfo{},
		networkRole:   ovntypes.NetworkRolePrimary, // all tests here run with network-segmentation disabled by default by default
	}

	var routeSources []*net.IPNet
	for _, podIP := range strings.Split(podIPs, " ") {
		if podIP == "" {
			continue
		}

		isIPv6 := ovntest.MustParseIP(podIP).To4() == nil

		for _, subnet := range config.Default.ClusterSubnets {
			if utilnet.IsIPv6CIDR(subnet.CIDR) == isIPv6 {
				routeSources = append(routeSources, subnet.CIDR)
			}
		}
		for _, sc := range config.Kubernetes.ServiceCIDRs {
			if utilnet.IsIPv6CIDR(sc) == isIPv6 {
				routeSources = append(routeSources, sc)
			}
		}
		hairpinMasqueradeIP := config.Gateway.MasqueradeIPs.V4OVNServiceHairpinMasqueradeIP.String()
		mask := 32
		if isIPv6 {
			hairpinMasqueradeIP = config.Gateway.MasqueradeIPs.V6OVNServiceHairpinMasqueradeIP.String()
			mask = 128
		}
		routeSources = append(routeSources, ovntest.MustParseIPNet(fmt.Sprintf("%s/%d", hairpinMasqueradeIP, mask)))
		joinNet := config.Gateway.V4JoinSubnet
		if isIPv6 {
			joinNet = config.Gateway.V6JoinSubnet
		}
		routeSources = append(routeSources, ovntest.MustParseIPNet(joinNet))
	}

	var gwIPs []string
	if nodeGWIP != "" {
		gwIPs = strings.Split(nodeGWIP, " ")
	}
	var gwIPv4, gwIPv6 *net.IP
	for _, gwIP := range gwIPs {
		gwNetIP := ovntest.MustParseIP(gwIP)
		isIPv6 := gwNetIP.To4() == nil
		if isIPv6 {
			gwIPv6 = &gwNetIP
		} else {
			gwIPv4 = &gwNetIP
		}
	}
	for _, rs := range routeSources {
		isIPv6 := ovntest.MustParseIPNet(rs.String()).IP.To4() == nil
		gwIP := gwIPv4
		if isIPv6 {
			gwIP = gwIPv6
		}
		if gwIP == nil {
			continue
		}
		to.routes = append(to.routes, util.PodRoute{Dest: rs, NextHop: *gwIP})
	}

	return to
}

func (p testPod) populateControllerLogicalSwitchCache(bnc *BaseNetworkController) {
	gomega.Expect(p.nodeName).NotTo(gomega.Equal(""))
	subnets := []*net.IPNet{}
	for _, subnet := range strings.Split(p.nodeSubnet, " ") {
		subnets = append(subnets, ovntest.MustParseIPNet(subnet))
	}
	err := bnc.lsManager.AddOrUpdateSwitch(bnc.GetNetworkScopedSwitchName(p.nodeName), subnets, nil, nil)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
}

func (p testPod) populateLogicalSwitchCache(fakeOvn *FakeOVN) {
	p.populateControllerLogicalSwitchCache(&fakeOvn.controller.BaseNetworkController)
}

func (p testPod) getAnnotationsJson() string {
	type podRoute struct {
		Dest    string `json:"dest"`
		NextHop string `json:"nextHop"`
	}
	type podAnnotation struct {
		MAC      string     `json:"mac_address"`
		IP       string     `json:"ip_address,omitempty"`
		IPs      []string   `json:"ip_addresses"`
		Gateway  string     `json:"gateway_ip,omitempty"`
		Gateways []string   `json:"gateway_ips,omitempty"`
		Routes   []podRoute `json:"routes,omitempty"`
		TunnelID int        `json:"tunnel_id,omitempty"`
		Role     string     `json:"role,omitempty"`
	}

	var address string
	addresses := []string{}
	for _, podIP := range strings.Split(p.podIP, " ") {
		if utilnet.IsIPv4String(podIP) {
			podIP += "/24"
		} else if utilnet.IsIPv6String(podIP) {
			podIP += "/64"
		}
		addresses = append(addresses, podIP)
	}
	if len(addresses) == 1 {
		address = addresses[0]
	}

	var nodeGWIPs []string
	if p.nodeGWIP != "" {
		nodeGWIPs = strings.Split(p.nodeGWIP, " ")
	}
	var nodeGWIP string
	if len(nodeGWIPs) == 1 {
		nodeGWIP = nodeGWIPs[0]
	}

	var routes []podRoute
	for _, route := range p.routes {
		routes = append(routes, podRoute{Dest: route.Dest.String(), NextHop: route.NextHop.String()})
	}

	podAnnotations := map[string]podAnnotation{
		"default": {
			MAC:      p.podMAC,
			IP:       address,
			IPs:      addresses,
			Gateway:  nodeGWIP,
			Gateways: nodeGWIPs,
			Routes:   routes,
			Role:     p.networkRole,
		},
	}

	for _, portInfos := range p.udnPodInfos {
		var udnIfaceRoutes []podRoute
		for _, route := range portInfos.routes {
			udnIfaceRoutes = append(
				udnIfaceRoutes,
				podRoute{Dest: route.Dest.String(), NextHop: route.NextHop.String()},
			)
		}
		for nad, portInfo := range portInfos.allportInfo {
			ipPrefix := 24
			if ovntest.MustParseIP(p.podIP).To4() == nil {
				ipPrefix = 64
			}
			if portInfo.prefixLen != 0 {
				ipPrefix = portInfo.prefixLen
			}
			ip := fmt.Sprintf("%s/%d", portInfo.podIP, ipPrefix)
			podAnnotation := podAnnotation{
				MAC:      portInfo.podMAC,
				IP:       ip,
				IPs:      []string{ip},
				TunnelID: portInfo.tunnelID,
				Role:     portInfos.role,
				Routes:   udnIfaceRoutes,
			}
			if portInfos.nodeGWIP != "" {
				podAnnotation.Gateway = portInfos.nodeGWIP
				podAnnotation.Gateways = []string{portInfos.nodeGWIP}
			}
			podAnnotations[nad] = podAnnotation
		}
	}

	bytes, err := json.Marshal(podAnnotations)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	return string(bytes)
}

func setPodAnnotations(podObj *corev1.Pod, testPod testPod) {
	if podObj.Annotations == nil {
		podObj.Annotations = map[string]string{}
	}
	podObj.Annotations[ovntypes.OvnPodAnnotationName] = testPod.getAnnotationsJson()
}

func getDefaultNetExpectedPodsAndSwitches(pods []testPod, nodes []string) []libovsdbtest.TestData {
	return getDefaultNetExpectedDataPodsSwitchesPortGroup(pods, nodes, "")
}

func getExpectedPodsAndSwitches(netInfo util.NetInfo, pods []testPod, nodes []string, nadKey string) []libovsdbtest.TestData {
	return getExpectedDataPodsSwitchesPortGroup(netInfo, pods, nodes, "", nadKey)
}

func getDefaultNetExpectedDataPodsSwitchesPortGroup(pods []testPod, nodes []string, namespacedPortGroup string) []libovsdbtest.TestData {
	return getExpectedDataPodsSwitchesPortGroup(&util.DefaultNetInfo{}, pods, nodes, namespacedPortGroup, "")
}

func getExpectedDataPodsSwitchesPortGroup(netInfo util.NetInfo, pods []testPod, nodes []string, namespacedPortGroup string, nadKey string) []libovsdbtest.TestData {
	if !netInfo.IsDefault() && nadKey == "" {
		panic("missing NAD key for non-default network")
	}
	nodeslsps := make(map[string][]string)
	var logicalSwitchPorts []*nbdb.LogicalSwitchPort
	for _, pod := range pods {
		var portName string
		if netInfo.IsDefault() {
			portName = util.GetLogicalPortName(pod.namespace, pod.podName)
		} else {
			portName = util.GetUserDefinedNetworkLogicalPortName(pod.namespace, pod.podName, nadKey)
		}
		var lspUUID string
		if len(pod.portUUID) == 0 {
			lspUUID = portName + "-UUID"
		} else {
			lspUUID = pod.portUUID
		}
		podAddr := fmt.Sprintf("%s %s", pod.podMAC, pod.podIP)
		lsp := &nbdb.LogicalSwitchPort{
			UUID:      lspUUID,
			Name:      portName,
			Addresses: []string{podAddr},
			ExternalIDs: map[string]string{
				"pod":       "true",
				"namespace": pod.namespace,
			},
			Options: map[string]string{
				libovsdbops.RequestedChassis: requestedChassisForPod(pod),
				"iface-id-ver":               pod.podName,
			},
			PortSecurity: []string{podAddr},
		}
		if pod.noIfaceIdVer {
			delete(lsp.Options, "iface-id-ver")
		}
		if !netInfo.IsDefault() {
			lsp.ExternalIDs["k8s.ovn.org/network"] = netInfo.GetNetworkName()
			lsp.ExternalIDs["k8s.ovn.org/nad"] = nadKey
			lsp.ExternalIDs["k8s.ovn.org/topology"] = netInfo.TopologyType()
		}
		logicalSwitchPorts = append(logicalSwitchPorts, lsp)
		nodeslsps[pod.nodeName] = append(nodeslsps[pod.nodeName], lspUUID)
	}
	var logicalSwitches []*nbdb.LogicalSwitch
	for _, node := range nodes {
		logicalSwitches = append(logicalSwitches, &nbdb.LogicalSwitch{
			UUID:  netInfo.GetNetworkScopedSwitchName(node) + "-UUID",
			Name:  netInfo.GetNetworkScopedSwitchName(node),
			Ports: nodeslsps[node],
		})
	}
	data := []libovsdbtest.TestData{}
	for _, lsp := range logicalSwitchPorts {
		data = append(data, lsp)
	}
	for _, ls := range logicalSwitches {
		data = append(data, ls)
	}
	if namespacedPortGroup != "" {
		// namespace port group is created
		pgIDs := getNamespacePortGroupDbIDs(namespacedPortGroup, ovntypes.DefaultNetworkControllerName)
		pg := libovsdbutil.BuildPortGroup(pgIDs, logicalSwitchPorts, nil)
		pg.UUID = pg.Name + "-UUID"
		data = append(data, pg)
	}

	return data
}

var _ = ginkgo.Describe("OVN Pod Operations", func() {
	var (
		app       *cli.App
		fakeOvn   *FakeOVN
		initialDB libovsdbtest.TestSetup
	)

	const (
		node1Name = "node1"
		node2Name = "node2"
	)

	ginkgo.BeforeEach(func() {
		// Restore global default values before each testcase
		gomega.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

		fakeOvn = NewFakeOVN(true, node1Name)
		initialDB = libovsdbtest.TestSetup{
			NBData: []libovsdbtest.TestData{
				&nbdb.LogicalSwitch{
					Name: "node1",
				},
			},
		}

	})

	ginkgo.AfterEach(func() {
		fakeOvn.shutdown()
	})

	ginkgo.Context("during execution", func() {

		ginkgo.DescribeTable("rolls back only newly reserved logical-port IPs", func(alreadyOwned bool) {
			pod := ovntest.NewPod("namespace1", "pod1", node1Name, "")
			ips := ovntest.MustParseIPNets("10.128.1.3/24")
			mac := util.IPAddrToHWAddr(ips[0].IP)
			lsp := &nbdb.LogicalSwitchPort{
				UUID: "pod-port-UUID", Name: util.GetLogicalPortName(pod.Namespace, pod.Name),
				Addresses: []string{mac.String() + " " + ips[0].IP.String()},
				Options:   map[string]string{"iface-id-ver": string(pod.UID)},
			}
			initialDB.NBData = []libovsdbtest.TestData{
				&nbdb.LogicalSwitch{Name: node1Name, Ports: []string{lsp.UUID}}, lsp,
			}
			fakeOvn.startWithDBSetup(initialDB, pod, newNode(node1Name, "192.0.2.10/24"))
			bnc := &fakeOvn.controller.BaseNetworkController
			gomega.Expect(bnc.lsManager.AddOrUpdateSwitch(node1Name, ovntest.MustParseIPNets("10.128.1.0/24"), nil, nil)).To(gomega.Succeed())
			if alreadyOwned {
				gomega.Expect(bnc.lsManager.AllocateIPs(node1Name, bnc.podIPOwner(pod, ovntypes.DefaultNetworkName), ips)).To(gomega.Succeed())
			}
			_, _, err := bnc.allocatePodAnnotation(pod, lsp, pod.Name, ovntypes.DefaultNetworkName,
				&nadapi.NetworkSelectionElement{MacRequest: "invalid-mac"}, ovntypes.NetworkRolePrimary)
			gomega.Expect(err).To(gomega.HaveOccurred())
			gomega.Expect(bnc.lsManager.OwnsIPs(node1Name, bnc.podIPOwner(pod, ovntypes.DefaultNetworkName), ips)).To(gomega.Equal(alreadyOwned))
			if !alreadyOwned {
				gomega.Expect(bnc.lsManager.AllocateIPs(node1Name, "test-owner", ips)).To(gomega.Succeed())
			}
		},
			ginkgo.Entry("keeps this owner's existing reservation on a later error", true),
			ginkgo.Entry("releases addresses newly reserved by the failed attempt", false),
		)

		ginkgo.DescribeTable("updates the MAC when replacing stale logical-port IPs", func(requestedMAC string) {
			config.IPv4Mode, config.IPv6Mode = true, true
			const namespace, podName = "namespace1", "pod1"
			pod := ovntest.NewPod(namespace, podName, node1Name, "")
			if requestedMAC != "" {
				pod.Annotations = map[string]string{
					util.DefNetworkAnnotation: fmt.Sprintf(`[{"name":"default","mac":%q}]`, requestedMAC),
				}
			}
			subnets := ovntest.MustParseIPNets("10.128.1.0/24", "fd00::/64")
			node := newNode(node1Name, "192.168.126.202/24")
			var err error
			node.Annotations, err = util.UpdateNodeHostSubnetAnnotation(node.Annotations, subnets, ovntypes.DefaultNetworkName)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			oldMAC := util.IPAddrToHWAddr(ovntest.MustParseIP("10.129.1.3"))
			lsp := &nbdb.LogicalSwitchPort{
				UUID: uuid.NewString(), Name: util.GetLogicalPortName(namespace, podName),
				Addresses: []string{oldMAC.String() + " 10.129.1.3 fd01::3"},
				Options:   map[string]string{"iface-id-ver": string(pod.UID)},
			}
			initialDB.NBData = []libovsdbtest.TestData{
				&nbdb.LogicalSwitch{Name: node1Name, Ports: []string{lsp.UUID}}, lsp,
			}
			fakeOvn.startWithDBSetup(initialDB, pod, ovntest.NewNamespace(namespace), node)
			gomega.Expect(fakeOvn.controller.lsManager.AddOrUpdateSwitch(node1Name, subnets, nil, nil)).To(gomega.Succeed())
			gomega.Expect(fakeOvn.controller.WatchNamespaces()).To(gomega.Succeed())

			ginkgo.By("reallocating IPs because the old logical port belongs to different subnets")
			gomega.Expect(fakeOvn.controller.addLogicalPort(pod)).To(gomega.Succeed())
			updatedPod, err := fakeOvn.fakeClient.KubeClient.CoreV1().Pods(namespace).Get(context.Background(), podName, metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			annotation, err := util.UnmarshalPodAnnotation(updatedPod.Annotations, ovntypes.DefaultNetworkName)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(annotation.IPs).To(gomega.HaveLen(2))
			for i, ip := range annotation.IPs {
				gomega.Expect(subnets[i].Contains(ip.IP)).To(gomega.BeTrue())
			}
			expectedMAC := util.IPAddrToHWAddr(annotation.IPs[0].IP)
			if requestedMAC != "" {
				expectedMAC = ovntest.MustParseMAC(requestedMAC)
			}
			gomega.Expect(annotation.MAC).To(gomega.Equal(expectedMAC))
			gomega.Expect(annotation.MAC).NotTo(gomega.Equal(oldMAC))
			addresses := expectedMAC.String() + " " + util.JoinIPNetIPs(annotation.IPs, " ")
			gomega.Eventually(func() (*nbdb.LogicalSwitchPort, error) {
				return libovsdbops.GetLogicalSwitchPort(fakeOvn.nbClient, &nbdb.LogicalSwitchPort{Name: lsp.Name})
			}).Should(gomega.SatisfyAll(
				gomega.HaveField("Addresses", gomega.ConsistOf(addresses)),
				gomega.HaveField("PortSecurity", gomega.ConsistOf(addresses)),
			))
		},
			ginkgo.Entry("derives a new MAC from the newly allocated IP", ""),
			ginkgo.Entry("preserves an explicit MAC request", "02:00:00:00:00:99"),
		)

		ginkgo.It("reconciles an existing pod", func() {
			app.Action = func(*cli.Context) error {
				// this flag will create namespaced port group
				config.OVNKubernetesFeature.EnableEgressFirewall = true
				namespaceT := *ovntest.NewNamespace("namespace1")
				// Setup an unassigned pod, perform an update later on which assigns it.
				t := newTPod(
					"",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespaceT.Name,
				)

				fakeOvn.startWithDBSetup(initialDB,
					&corev1.NamespaceList{
						Items: []corev1.Namespace{
							namespaceT,
						},
					},
					&corev1.NodeList{
						Items: []corev1.Node{
							*newNode(node1Name, "192.168.126.202/24"),
						},
					},
					&corev1.PodList{
						Items: []corev1.Pod{
							*ovntest.NewPod(t.namespace, t.podName, t.nodeName, t.podIP),
						},
					},
				)

				ctext, cancel := context.WithTimeout(context.Background(), config.Default.OVSDBTxnTimeout)
				defer cancel()
				lsl := []nbdb.LogicalSwitch{}
				err := fakeOvn.controller.nbClient.WhereCache(
					func(ls *nbdb.LogicalSwitch) bool {
						return ls.Name == "node1"
					}).List(ctext, &lsl)

				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(lsl).To(gomega.HaveLen(1))

				err = fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				pod, err := fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).Get(context.TODO(), t.podName, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				_, ok := pod.Annotations[ovntypes.OvnPodAnnotationName]
				gomega.Expect(ok).To(gomega.BeFalse())

				// Assign it and perform the update
				t.nodeName = "node1"
				t.portName = util.GetLogicalPortName(t.namespace, t.podName)
				t.populateLogicalSwitchCache(fakeOvn)

				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).Update(context.TODO(),
					ovntest.NewPod(t.namespace, t.podName, t.nodeName, t.podIP), metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(func() string {
					return getPodAnnotations(fakeOvn.fakeClient.KubeClient, t.namespace, t.podName)
				}, 2).Should(gomega.MatchJSON(t.getAnnotationsJson()))

				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(
					getDefaultNetExpectedDataPodsSwitchesPortGroup([]testPod{t}, []string{"node1"}, namespaceT.Name)))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("reconciles a new pod", func() {
			app.Action = func(*cli.Context) error {
				namespaceT := *ovntest.NewNamespace("namespace1")
				t := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespaceT.Name,
				)

				fakeOvn.startWithDBSetup(initialDB,
					&corev1.NamespaceList{
						Items: []corev1.Namespace{
							namespaceT,
						},
					},
					&corev1.NodeList{
						Items: []corev1.Node{
							*newNode(node1Name, "192.168.126.202/24"),
						},
					},
					&corev1.PodList{
						Items: []corev1.Pod{},
					},
				)

				t.populateLogicalSwitchCache(fakeOvn)
				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).Get(context.TODO(), t.podName, metav1.GetOptions{})
				gomega.Expect(err).To(gomega.MatchError(apierrors.IsNotFound, "IsNotFound"))

				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).Create(context.TODO(),
					ovntest.NewPod(t.namespace, t.podName, t.nodeName, t.podIP), metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(func() string {
					return getPodAnnotations(fakeOvn.fakeClient.KubeClient, t.namespace, t.podName)
				}, 2).Should(gomega.MatchJSON(t.getAnnotationsJson()))

				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(getDefaultNetExpectedPodsAndSwitches([]testPod{t}, []string{"node1"})))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("allows allocation after pods are completed", func() {
			app.Action = func(*cli.Context) error {
				namespaceT := *ovntest.NewNamespace("namespace1")
				t := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespaceT.Name,
				)

				fakeOvn.startWithDBSetup(initialDB,
					&corev1.NamespaceList{
						Items: []corev1.Namespace{
							namespaceT,
						},
					},
					&corev1.NodeList{
						Items: []corev1.Node{
							*newNode(node1Name, "192.168.126.202/24"),
						},
					},
					&corev1.PodList{
						Items: []corev1.Pod{},
					},
				)

				t.populateLogicalSwitchCache(fakeOvn)
				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).Get(context.TODO(), t.podName, metav1.GetOptions{})
				gomega.Expect(err).To(gomega.MatchError(apierrors.IsNotFound, "IsNotFound"))

				myPod := ovntest.NewPod(t.namespace, t.podName, t.nodeName, t.podIP)
				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).Create(context.TODO(),
					myPod, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(func() string {
					return getPodAnnotations(fakeOvn.fakeClient.KubeClient, t.namespace, t.podName)
				}, 2).Should(gomega.MatchJSON(t.getAnnotationsJson()))

				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(getDefaultNetExpectedPodsAndSwitches([]testPod{t}, []string{"node1"})))

				ginkgo.By("Allocating all of the rest of the node subnet")
				// allocate all the rest of the IPs in the subnet
				err = fakeOvn.controller.lsManager.AllocateUntilFull("node1")
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				ginkgo.By("Creating another pod which will fail due to allocation full")
				t2 := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod2",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespaceT.Name,
				)

				myPod2, err := fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).Create(context.TODO(),
					ovntest.NewPod(t2.namespace, t2.podName, t2.nodeName, ""), metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(func() string {
					return getPodAnnotations(fakeOvn.fakeClient.KubeClient, t2.namespace, t2.podName)
				}, 2).Should(gomega.BeEmpty())
				myPod2Key, err := retry.GetResourceKey(myPod2)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				retry.CheckRetryObjectEventually(myPod2Key, true, fakeOvn.controller.retryPods)

				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(getDefaultNetExpectedPodsAndSwitches([]testPod{t}, []string{"node1"})))
				ginkgo.By("Marking myPod as completed should free IP")
				myPod.Status.Phase = corev1.PodSucceeded

				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).UpdateStatus(context.TODO(),
					myPod, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// port should be gone or marked for removal in logical port cache
				gomega.Eventually(func() bool {
					info, err := fakeOvn.controller.logicalPortCache.get(myPod, ovntypes.DefaultNetworkName)
					return err != nil || !info.expires.IsZero()
				}, 2).Should(gomega.BeTrue())

				ginkgo.By("Freed IP should now allow mypod2 to come up")
				key, err := retry.GetResourceKey(myPod2)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// let the retry logic run until the IP from myPod is released and myPod2 is added
				gomega.Eventually(func(g gomega.Gomega) {
					retry.SetRetryObjWithNoBackoff(key, fakeOvn.controller.retryPods)
					fakeOvn.controller.retryPods.RequestRetryObjs()
					retry.CheckRetryObjectEventuallyWrapped(g, myPod2Key, false, fakeOvn.controller.retryPods)
				}, 60*time.Second, 500*time.Millisecond).Should(gomega.Succeed())

				gomega.Eventually(func() string {
					return getPodAnnotations(fakeOvn.fakeClient.KubeClient, t2.namespace, t2.podName)
				}, 2).Should(gomega.MatchJSON(t2.getAnnotationsJson()))
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(getDefaultNetExpectedPodsAndSwitches([]testPod{t2}, []string{"node1"})))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should not deallocate in-use and previously freed completed pods IP", func() {
			app.Action = func(*cli.Context) error {
				namespaceT := *ovntest.NewNamespace("namespace1")
				t := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespaceT.Name,
				)

				fakeOvn.startWithDBSetup(initialDB,
					&corev1.NamespaceList{
						Items: []corev1.Namespace{
							namespaceT,
						},
					},
					&corev1.NodeList{
						Items: []corev1.Node{
							*newNode("node1", "192.168.126.202/24"),
						},
					},
					&corev1.PodList{
						Items: []corev1.Pod{},
					},
				)

				t.populateLogicalSwitchCache(fakeOvn)
				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).Get(context.TODO(), t.podName, metav1.GetOptions{})
				gomega.Expect(err).To(gomega.MatchError(apierrors.IsNotFound, "IsNotFound"))

				myPod := ovntest.NewPod(t.namespace, t.podName, t.nodeName, t.podIP)
				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).Create(context.TODO(),
					myPod, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(func() string {
					return getPodAnnotations(fakeOvn.fakeClient.KubeClient, t.namespace, t.podName)
				}, 2).Should(gomega.MatchJSON(t.getAnnotationsJson()))

				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(getDefaultNetExpectedPodsAndSwitches([]testPod{t}, []string{"node1"})))

				ginkgo.By("Allocating all of the rest of the node subnet")
				// allocate all the rest of the IPs in the subnet
				err = fakeOvn.controller.lsManager.AllocateUntilFull("node1")
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				ginkgo.By("Creating another pod which will fail due to allocation full")
				t2 := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod2",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespaceT.Name,
				)

				myPod2, err := fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).Create(context.TODO(),
					ovntest.NewPod(t2.namespace, t2.podName, t2.nodeName, ""), metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(func() string {
					return getPodAnnotations(fakeOvn.fakeClient.KubeClient, t2.namespace, t2.podName)
				}, 2).Should(gomega.BeEmpty())

				myPod2Key, err := retry.GetResourceKey(myPod2)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				retry.CheckRetryObjectEventually(myPod2Key, true, fakeOvn.controller.retryPods)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(getDefaultNetExpectedPodsAndSwitches([]testPod{t}, []string{"node1"})))
				ginkgo.By("Marking myPod as completed should free IP")
				myPod.Status.Phase = corev1.PodSucceeded

				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).UpdateStatus(context.TODO(),
					myPod, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// port should be gone or marked for removal in logical port cache
				gomega.Eventually(func() bool {
					info, err := fakeOvn.controller.logicalPortCache.get(myPod, ovntypes.DefaultNetworkName)
					return err != nil || !info.expires.IsZero()
				}, 2).Should(gomega.BeTrue())

				ginkgo.By("Freed IP should now allow mypod2 to come up")
				key, err := retry.GetResourceKey(myPod2)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// let the retry logic run until the IP from myPod is released and myPod2 is added
				gomega.Eventually(func(g gomega.Gomega) {
					retry.SetRetryObjWithNoBackoff(key, fakeOvn.controller.retryPods)
					fakeOvn.controller.retryPods.RequestRetryObjs()
					// there should be no entry for this pod in the retry cache
					retry.CheckRetryObjectEventuallyWrapped(g, myPod2Key, false, fakeOvn.controller.retryPods)
				}, 60*time.Second, 500*time.Millisecond).Should(gomega.Succeed())

				gomega.Eventually(func() string {
					return getPodAnnotations(fakeOvn.fakeClient.KubeClient, t2.namespace, t2.podName)
				}, 2).Should(gomega.MatchJSON(t2.getAnnotationsJson()))

				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(getDefaultNetExpectedPodsAndSwitches([]testPod{t2}, []string{"node1"})))
				// 2nd pod should now have the IP
				myPod2, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).Get(context.TODO(),
					t2.podName, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				ginkgo.By("Updating the completed pod should not free the IP")
				patch := struct {
					Metadata map[string]interface{} `json:"metadata"`
				}{
					Metadata: map[string]interface{}{
						"annotations": map[string]string{"dummy": "data"},
					},
				}
				patchData, err := json.Marshal(&patch)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// trigger update event
				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).Patch(context.TODO(), myPod.Name, types.MergePatchType, patchData, metav1.PatchOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// sleep a small amount to ensure the event was processed
				time.Sleep(time.Second)
				// try to allocate the IP and it should not work
				annotation, err := util.UnmarshalPodAnnotation(myPod2.Annotations, ovntypes.DefaultNetworkName)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.lsManager.AllocateIPs(t.nodeName, "test-owner", annotation.IPs)
				gomega.Expect(ipallocator.IsErrAllocated(err)).To(gomega.BeTrue())
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(getDefaultNetExpectedPodsAndSwitches([]testPod{t2}, []string{"node1"})))

				ginkgo.By("Deleting the completed pod should not allow a third pod to take the IP")
				// now delete the completed pod
				err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).Delete(context.TODO(),
					t.podName, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// now create a 3rd pod
				t3 := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod3",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespaceT.Name,
				)

				myPod3, err := fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).Create(context.TODO(),
					ovntest.NewPod(t3.namespace, t3.podName, t3.nodeName, t3.podIP), metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(func() string {
					return getPodAnnotations(fakeOvn.fakeClient.KubeClient, t3.namespace, t3.podName)
				}, 2).Should(gomega.BeEmpty())

				// should be in retry because there are no more IPs left
				myPod3Key, err := retry.GetResourceKey(myPod3)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				retry.CheckRetryObjectEventually(myPod3Key, true, fakeOvn.controller.retryPods)
				// TODO validate that the pods also have correct GW SNATs and route policies
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(getDefaultNetExpectedPodsAndSwitches([]testPod{t2}, []string{"node1"})))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should not allocate a completed pod on start up", func() {
			app.Action = func(*cli.Context) error {
				namespaceT := *ovntest.NewNamespace("namespace1")
				t := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespaceT.Name,
				)
				myPod := ovntest.NewPod(t.namespace, t.podName, t.nodeName, t.podIP)
				myPod.Status.Phase = corev1.PodSucceeded

				fakeOvn.startWithDBSetup(initialDB,
					&corev1.NamespaceList{
						Items: []corev1.Namespace{
							namespaceT,
						},
					},
					&corev1.NodeList{
						Items: []corev1.Node{
							*newNode(node1Name, "192.168.126.202/24"),
						},
					},
					&corev1.PodList{
						Items: []corev1.Pod{*myPod},
					},
				)

				t.populateLogicalSwitchCache(fakeOvn)

				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectedData := []libovsdbtest.TestData{getDefaultNetExpectedPodsAndSwitches([]testPod{}, []string{"node1"})}
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData...))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("retryPod cache operations while adding a new pod", func() {
			app.Action = func(*cli.Context) error {
				config.Gateway.DisableSNATMultipleGWs = true
				namespaceT := *ovntest.NewNamespace("namespace1")
				t := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespaceT.Name,
				)

				fakeOvn.startWithDBSetup(initialDB,
					&corev1.NamespaceList{
						Items: []corev1.Namespace{
							namespaceT,
						},
					},
					&corev1.NodeList{
						Items: []corev1.Node{
							*newNode(node1Name, "192.168.126.202/24"),
						},
					},
					&corev1.PodList{
						Items: []corev1.Pod{},
					},
				)

				t.populateLogicalSwitchCache(fakeOvn)
				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).Get(context.TODO(), t.podName, metav1.GetOptions{})
				gomega.Expect(err).To(gomega.MatchError(apierrors.IsNotFound, "IsNotFound"))

				podObj := &corev1.Pod{
					Spec: corev1.PodSpec{NodeName: "node1"},
					ObjectMeta: metav1.ObjectMeta{
						Name:      t.podName,
						Namespace: namespaceT.Name,
					},
				}
				err = fakeOvn.controller.ensurePod(nil, podObj, true) // this fails since pod doesn't exist to set annotations
				gomega.Expect(err).To(gomega.HaveOccurred())

				key, err := retry.GetResourceKey(podObj)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Expect(retry.RetryObjsLen(fakeOvn.controller.retryPods)).To(gomega.Equal(0))
				retry.InitRetryObjWithAdd(podObj, key, fakeOvn.controller.retryPods)
				gomega.Expect(retry.RetryObjsLen(fakeOvn.controller.retryPods)).To(gomega.Equal(1))
				gomega.Expect(retry.CheckRetryObj(key, fakeOvn.controller.retryPods)).To(gomega.BeTrue())
				newObj := retry.GetNewObjFieldFromRetryObj(key, fakeOvn.controller.retryPods)
				storedPod, ok := newObj.(*corev1.Pod)
				gomega.Expect(ok).To(gomega.BeTrue())
				gomega.Expect(storedPod.UID).To(gomega.Equal(podObj.UID))

				retry.DeleteRetryObj(key, fakeOvn.controller.retryPods)
				gomega.Expect(retry.CheckRetryObj(key, fakeOvn.controller.retryPods)).To(gomega.BeFalse())

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("correctly retries a failure while adding a pod", func() {
			app.Action = func(*cli.Context) error {
				namespace1 := *ovntest.NewNamespace("namespace1")
				podTest := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespace1.Name,
				)
				pod := ovntest.NewPod(podTest.namespace, podTest.podName, podTest.nodeName, podTest.podIP)

				key, err := retry.GetResourceKey(pod)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				fakeOvn.startWithDBSetup(initialDB,
					&corev1.NamespaceList{
						Items: []corev1.Namespace{
							namespace1,
						},
					},
					&corev1.NodeList{
						Items: []corev1.Node{
							*newNode(node1Name, "192.168.126.202/24"),
						},
					},
					&corev1.PodList{
						Items: []corev1.Pod{*pod},
					},
				)

				podTest.populateLogicalSwitchCache(fakeOvn)
				err = fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(fakeOvn.controller.nbClient).Should(
					libovsdbtest.HaveData(getDefaultNetExpectedPodsAndSwitches([]testPod{}, []string{"node1"})...))

				// inject transient problem, nbdb is down
				fakeOvn.controller.nbClient.Close()
				gomega.Eventually(func() bool {
					return fakeOvn.controller.nbClient.Connected()
				}).Should(gomega.BeFalse())
				// trigger pod add which will fail with "context deadline exceeded: while awaiting reconnection"
				err = fakeOvn.controller.WatchPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// sleep long enough for TransactWithRetry to fail, causing pod add to fail
				time.Sleep(config.Default.OVSDBTxnTimeout + time.Second)

				// check to see if the pod retry cache has an entry for this policy
				retry.CheckRetryObjectEventually(key, true, fakeOvn.controller.retryPods)
				connCtx, cancel := context.WithTimeout(context.Background(), config.Default.OVSDBTxnTimeout)

				defer cancel()
				resetNBClient(connCtx, fakeOvn.controller.nbClient)
				// reset backoff for immediate retry
				retry.SetRetryObjWithNoBackoff(key, fakeOvn.controller.retryPods)
				fakeOvn.controller.retryPods.RequestRetryObjs() // retry the failed entry

				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(podTest.namespace).Get(context.TODO(), podTest.podName, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(fakeOvn.controller.nbClient).Should(
					libovsdbtest.HaveData(getDefaultNetExpectedPodsAndSwitches([]testPod{podTest}, []string{"node1"})...))
				// check the retry cache no longer has the entry
				retry.CheckRetryObjectEventually(key, false, fakeOvn.controller.retryPods)
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("correctly retries a failure while deleting a pod", func() {
			app.Action = func(*cli.Context) error {
				namespace1 := *ovntest.NewNamespace("namespace1")
				podTest := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespace1.Name,
				)
				pod := ovntest.NewPod(podTest.namespace, podTest.podName, podTest.nodeName, podTest.podIP)
				expectedData := []libovsdbtest.TestData{getDefaultNetExpectedPodsAndSwitches([]testPod{podTest}, []string{"node1"})}
				key, err := retry.GetResourceKey(pod)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.startWithDBSetup(initialDB,
					&corev1.NamespaceList{
						Items: []corev1.Namespace{
							namespace1,
						},
					},
					&corev1.NodeList{
						Items: []corev1.Node{
							*newNode(node1Name, "192.168.126.202/24"),
						},
					},
					&corev1.PodList{
						Items: []corev1.Pod{*pod},
					},
				)

				podTest.populateLogicalSwitchCache(fakeOvn)
				err = fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(podTest.namespace).Get(context.TODO(), podTest.podName, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData...))

				// inject transient problem, nbdb is down
				fakeOvn.controller.nbClient.Close()
				gomega.Eventually(func() bool {
					return fakeOvn.controller.nbClient.Connected()
				}).Should(gomega.BeFalse())
				// trigger pod delete which will fail with "context deadline exceeded: while awaiting reconnection"
				err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(pod.Namespace).Delete(context.TODO(), pod.Name, *metav1.NewDeleteOptions(0))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// sleep long enough for TransactWithRetry to fail, causing pod delete to fail
				time.Sleep(config.Default.OVSDBTxnTimeout + time.Second)

				// check to see if the pod retry cache has an entry for this pod
				retry.CheckRetryObjectEventually(key, true, fakeOvn.controller.retryPods)
				connCtx, cancel := context.WithTimeout(context.Background(), config.Default.OVSDBTxnTimeout)

				defer cancel()
				resetNBClient(connCtx, fakeOvn.controller.nbClient)
				retry.SetRetryObjWithNoBackoff(key, fakeOvn.controller.retryPods)
				fakeOvn.controller.retryPods.RequestRetryObjs() // retry the failed entry

				gomega.Eventually(fakeOvn.controller.nbClient).Should(
					libovsdbtest.HaveData(getDefaultNetExpectedPodsAndSwitches([]testPod{}, []string{"node1"})...))
				// check the retry cache no longer has the entry
				retry.CheckRetryObjectEventually(key, false, fakeOvn.controller.retryPods)
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.DescribeTable("retains pod IPs until KubeVirt cleanup succeeds", func(local bool, failDeleteTable string) {
			const namespace, podName, vmName = "namespace1", "vm-pod", "vm1"
			config.IPv4Mode, config.IPv6Mode = true, true
			podNode := node1Name
			if !local {
				podNode = node2Name
			}
			pod := ovntest.NewPod(namespace, podName, podNode, "10.128.1.3 fd00::3")
			pod.UID = "vm-pod-uid"
			pod.Status.Phase = corev1.PodSucceeded
			pod.Labels = map[string]string{
				kubevirtv1.AppLabel:                "virt-launcher",
				kubevirtv1.VirtualMachineNameLabel: vmName,
			}
			ips := ovntest.MustParseIPNets("10.128.1.3/24", "fd00::3/64")
			mac := ovntest.MustParseMAC("0a:58:0a:80:01:03")
			var err error
			pod.Annotations, err = util.MarshalPodAnnotation(map[string]string{
				kubevirtv1.AllowPodBridgeNetworkLiveMigrationAnnotation: "",
				kubevirtv1.DomainAnnotation:                             vmName,
			}, &util.PodAnnotation{IPs: ips, MAC: mac}, ovntypes.DefaultNetworkName)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			externalIDs := map[string]string{
				string(libovsdbops.ObjectNameKey): namespace + "/" + vmName,
				kubevirt.OvnZoneExternalIDKey:     kubevirt.OvnLocalZone,
			}
			dhcpv4 := &nbdb.DHCPOptions{UUID: uuid.NewString(), Cidr: "10.128.1.0/24", ExternalIDs: externalIDs}
			dhcpv6 := &nbdb.DHCPOptions{UUID: uuid.NewString(), Cidr: "fd00::/64", ExternalIDs: externalIDs}
			route := &nbdb.LogicalRouterStaticRoute{
				UUID: uuid.NewString(), IPPrefix: "10.128.1.3/32", Nexthop: "100.64.0.2", ExternalIDs: externalIDs,
			}
			policy := &nbdb.LogicalRouterPolicy{
				UUID: uuid.NewString(), Priority: ovntypes.EgressSVCReroutePriority,
				Match: "ip4.src == 10.128.1.3", Action: nbdb.LogicalRouterPolicyActionReroute,
				Nexthops: []string{"100.64.0.2"}, ExternalIDs: externalIDs,
			}
			router := &nbdb.LogicalRouter{
				UUID: uuid.NewString(), Name: ovntypes.OVNClusterRouter,
				StaticRoutes: []string{route.UUID}, Policies: []string{policy.UUID},
			}
			logicalSwitch := &nbdb.LogicalSwitch{UUID: uuid.NewString(), Name: node1Name}
			lsp := &nbdb.LogicalSwitchPort{
				UUID: uuid.NewString(), Name: util.GetLogicalPortName(namespace, podName),
				Addresses:     []string{"0a:58:0a:80:01:03 10.128.1.3 fd00::3"},
				Dhcpv4Options: &dhcpv4.UUID, Dhcpv6Options: &dhcpv6.UUID,
			}
			initialDB.NBData = []libovsdbtest.TestData{logicalSwitch, router, route, policy, dhcpv4, dhcpv6}
			if local {
				logicalSwitch.Ports = []string{lsp.UUID}
				initialDB.NBData = append(initialDB.NBData, lsp)
			}
			fakeOvn.startWithDBSetup(initialDB, pod)
			oc := fakeOvn.controller
			gomega.Expect(oc.lsManager.AddOrUpdateSwitch(node1Name,
				ovntest.MustParseIPNets("10.128.1.0/24", "fd00::/64"), nil, nil)).To(gomega.Succeed())
			gomega.Expect(oc.lsManager.AllocateIPs(node1Name, oc.podIPOwner(pod, ovntypes.DefaultNetworkName), ips)).To(gomega.Succeed())
			var portInfo *lpInfo
			if local {
				portInfo = oc.logicalPortCache.add(pod, node1Name, ovntypes.DefaultNetworkName, lsp.UUID, mac, ips)
			}

			ginkgo.By("failing VM cleanup without disrupting ordinary pod teardown transactions")
			oc.nbClient = &podCleanupFailureClient{Client: fakeOvn.nbClient, failDeleteTable: failDeleteTable}
			err = oc.removePod(pod, portInfo)
			gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("injected " + failDeleteTable + " cleanup failure")))
			for _, ip := range ips {
				gomega.Expect(ipallocator.IsErrAllocated(oc.lsManager.AllocateIPs(node1Name, "test-owner", []*net.IPNet{ip}))).To(
					gomega.BeTrue(), "VM cleanup failure must leave %s reserved", ip)
			}

			expectedRouter := *router
			expectedAfterFailure := []libovsdbtest.TestData{logicalSwitch, &expectedRouter, policy}
			expectedLSP := *lsp
			if failDeleteTable == nbdb.DHCPOptionsTable {
				expectedAfterFailure = append(expectedAfterFailure, dhcpv4, dhcpv6, route)
			} else {
				// DHCP and static-route cleanup committed before policy deletion failed.
				expectedRouter.StaticRoutes = nil
				expectedLSP.Dhcpv4Options, expectedLSP.Dhcpv6Options = nil, nil
			}
			if local {
				expectedAfterFailure = append(expectedAfterFailure, &expectedLSP)
			}
			gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedAfterFailure...))

			ginkgo.By("retrying partial VM cleanup before releasing the pod's IPs")
			oc.nbClient = fakeOvn.nbClient
			gomega.Expect(oc.removePod(pod, portInfo)).To(gomega.Succeed())
			gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(
				&nbdb.LogicalSwitch{UUID: logicalSwitch.UUID, Name: node1Name},
				&nbdb.LogicalRouter{UUID: router.UUID, Name: ovntypes.OVNClusterRouter},
			))
			gomega.Expect(oc.lsManager.AllocateIPs(node1Name, "test-owner", ips)).To(gomega.Succeed(), "successful cleanup must release both IPs")
		},
			ginkgo.Entry("local pod, DHCP cleanup failure", true, nbdb.DHCPOptionsTable),
			ginkgo.Entry("local pod, routing cleanup failure after DHCP deletion", true, nbdb.LogicalRouterPolicyTable),
			ginkgo.Entry("remote pod, DHCP cleanup failure", false, nbdb.DHCPOptionsTable),
			ginkgo.Entry("remote pod, routing cleanup failure after DHCP deletion", false, nbdb.LogicalRouterPolicyTable),
		)

		ginkgo.It("doesn't stop retrying adding a pod after failing n times", func() {
			app.Action = func(*cli.Context) error {
				namespace1 := *ovntest.NewNamespace("namespace1")
				podTest := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespace1.Name,
				)
				pod := ovntest.NewPod(podTest.namespace, podTest.podName,
					podTest.nodeName, podTest.podIP)

				key, err := retry.GetResourceKey(pod)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				fakeOvn.startWithDBSetup(initialDB,
					&corev1.NamespaceList{
						Items: []corev1.Namespace{
							namespace1,
						},
					},
					&corev1.NodeList{
						Items: []corev1.Node{
							*newNode(node1Name, "192.168.126.202/24"),
						},
					},
					&corev1.PodList{
						Items: []corev1.Pod{*pod},
					},
				)

				podTest.populateLogicalSwitchCache(fakeOvn)
				err = fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(fakeOvn.controller.nbClient).Should(
					libovsdbtest.HaveData(getDefaultNetExpectedPodsAndSwitches([]testPod{}, []string{"node1"})...))

				// inject transient problem, nbdb is down
				fakeOvn.controller.nbClient.Close()
				gomega.Eventually(func() bool {
					return fakeOvn.controller.nbClient.Connected()
				}).Should(gomega.BeFalse())

				// trigger pod add, which will fail with "context deadline exceeded: while awaiting reconnection"
				err = fakeOvn.controller.WatchPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// sleep long enough for TransactWithRetry to fail, causing pod add to fail
				time.Sleep(config.Default.OVSDBTxnTimeout + time.Second)

				// wait until retry entry appears

				// check that the retry entry is marked for creation
				retry.CheckRetryObjectMultipleFieldsEventually(
					key,
					fakeOvn.controller.retryPods,
					gomega.BeNil(),                // oldObj should be nil
					gomega.Not(gomega.BeNil()),    // newObj should not be nil
					nil,                           // skip config
					gomega.BeNumerically("==", 1), // failedAttempts should be 1
				)

				// set failedAttempts to retry.MaxFailedAttempts-1, trigger a retry
				// (which will fail due to nbdb being down)
				// and verify that failedAttempts is now equal to retry.MaxFailedAttempts
				retry.SetFailedAttemptsCounterForTestingOnly(key, retry.MaxFailedAttempts-1,
					fakeOvn.controller.retryPods)
				// reset backoff for immediate retry
				retry.SetRetryObjWithNoBackoff(key, fakeOvn.controller.retryPods)
				fakeOvn.controller.retryPods.RequestRetryObjs()

				retry.CheckRetryObjectMultipleFieldsEventually(
					key,
					fakeOvn.controller.retryPods,
					gomega.BeNil(),             // oldObj should nil
					gomega.Not(gomega.BeNil()), // newObj should not be nil
					nil,                        // skip config
					gomega.BeNumerically("==", retry.MaxFailedAttempts), // failedAttempts should reach the max
				)

				// restore nbdb, trigger a retry and verify that the pod is added
				connCtx, cancel := context.WithTimeout(context.Background(), config.Default.OVSDBTxnTimeout)
				defer cancel()
				resetNBClient(connCtx, fakeOvn.controller.nbClient)

				// reset backoff for immediate retry
				retry.SetRetryObjWithNoBackoff(key, fakeOvn.controller.retryPods)
				fakeOvn.controller.retryPods.RequestRetryObjs()
				// check that pod is in API server
				pod, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(podTest.namespace).Get(
					context.TODO(), podTest.podName, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(pod).NotTo(gomega.BeNil())

				// check that the retry cache no longer has the entry
				retry.CheckRetryObjectEventually(key, false, fakeOvn.controller.retryPods)

				// check that pod is configured in OVN
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(
					getDefaultNetExpectedPodsAndSwitches([]testPod{podTest}, []string{"node1"})...))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("doesn't stop retrying deleting a pod after failing n times", func() {
			app.Action = func(*cli.Context) error {
				namespace1 := *ovntest.NewNamespace("namespace1")
				podTest := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespace1.Name,
				)
				pod := ovntest.NewPod(podTest.namespace, podTest.podName, podTest.nodeName, podTest.podIP)
				expectedData := []libovsdbtest.TestData{getDefaultNetExpectedPodsAndSwitches(
					[]testPod{podTest},
					[]string{"node1"})}
				key, err := retry.GetResourceKey(pod)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.startWithDBSetup(initialDB,
					&corev1.NamespaceList{
						Items: []corev1.Namespace{
							namespace1,
						},
					},
					&corev1.NodeList{
						Items: []corev1.Node{
							*newNode(node1Name, "192.168.126.202/24"),
						},
					},
					&corev1.PodList{
						Items: []corev1.Pod{*pod},
					},
				)

				podTest.populateLogicalSwitchCache(fakeOvn)
				err = fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(podTest.namespace).Get(
					context.TODO(), podTest.podName, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData...))

				// inject transient problem, nbdb is down
				fakeOvn.controller.nbClient.Close()
				gomega.Eventually(func() bool {
					return fakeOvn.controller.nbClient.Connected()
				}).Should(gomega.BeFalse())

				// trigger pod delete, which will fail with "context deadline exceeded: while awaiting reconnection"
				err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(pod.Namespace).Delete(
					context.TODO(), pod.Name, *metav1.NewDeleteOptions(0))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// sleep long enough for TransactWithRetry to fail, causing pod delete to fail
				time.Sleep(config.Default.OVSDBTxnTimeout + time.Second)

				// wait until retry entry appears and check that it is marked for deletion
				retry.CheckRetryObjectMultipleFieldsEventually(
					key,
					fakeOvn.controller.retryPods,
					gomega.Not(gomega.BeNil()),    // oldObj should not be nil
					gomega.BeNil(),                // newObj should be nil
					nil,                           // skip config
					gomega.BeNumerically("==", 1), // failedAttempts should be 1
				)

				// set failedAttempts to retry.MaxFailedAttempts-1, trigger a retry (which will fail due to nbdb),
				// check that failedAttempts is now equal to retry.MaxFailedAttempts
				retry.SetFailedAttemptsCounterForTestingOnly(key, retry.MaxFailedAttempts-1,
					fakeOvn.controller.retryPods)
				fakeOvn.controller.retryPods.RequestRetryObjs()

				retry.CheckRetryObjectMultipleFieldsEventually(
					key,
					fakeOvn.controller.retryPods,
					gomega.Not(gomega.BeNil()), // oldObj should not be nil
					gomega.BeNil(),             // newObj should be nil
					nil,                        // config is skipped
					gomega.BeNumerically("==", retry.MaxFailedAttempts), // failedAttempts should be the max
				)

				// restore nbdb and verify that the pod is deleted
				connCtx, cancel := context.WithTimeout(context.Background(), config.Default.OVSDBTxnTimeout)
				defer cancel()
				resetNBClient(connCtx, fakeOvn.controller.nbClient)

				// reset backoff for immediate retry
				retry.SetRetryObjWithNoBackoff(key, fakeOvn.controller.retryPods)
				fakeOvn.controller.retryPods.RequestRetryObjs()

				// check that the pod is not in API server
				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(podTest.namespace).Get(
					context.TODO(), podTest.podName, metav1.GetOptions{})
				gomega.Expect(err).To(gomega.MatchError(apierrors.IsNotFound, "IsNotFound"))

				// check that the retry cache no longer has the entry
				retry.CheckRetryObjectEventually(key, false, fakeOvn.controller.retryPods)

				// check that the pod is deleted in OVN
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(
					getDefaultNetExpectedPodsAndSwitches([]testPod{}, []string{"node1"})...))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("correctly remove a LSP from a pod that has stale nodeName annotation", func() {
			app.Action = func(*cli.Context) error {
				namespace1 := *ovntest.NewNamespace("namespace1")
				podTest := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespace1.Name,
				)
				pod := ovntest.NewPod(podTest.namespace, podTest.podName, podTest.nodeName, podTest.podIP)
				expectedData := []libovsdbtest.TestData{getDefaultNetExpectedPodsAndSwitches([]testPod{podTest}, []string{"node1"})}
				key, err := retry.GetResourceKey(pod)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.startWithDBSetup(initialDB,
					&corev1.NamespaceList{
						Items: []corev1.Namespace{
							namespace1,
						},
					},
					&corev1.NodeList{
						Items: []corev1.Node{
							*newNode(node1Name, "192.168.126.202/24"),
						},
					},
					&corev1.PodList{
						Items: []corev1.Pod{*pod},
					},
				)

				podTest.populateLogicalSwitchCache(fakeOvn)
				err = fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData...))

				// Get pod from api with its metadata filled in
				pod, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(podTest.namespace).Get(context.TODO(), podTest.podName, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Fudge nodename from pod's spec, to ensure it is not used by deleteLogicalPort
				pod.Spec.NodeName = "this_is_the_wrong_nodeName"

				// Deleting port from a pod that has no portInfo and the wrong nodeName should still be okay!
				err = fakeOvn.controller.deleteLogicalPort(pod, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// OVN db should be empty now
				gomega.Eventually(fakeOvn.controller.nbClient).Should(
					libovsdbtest.HaveData(getDefaultNetExpectedPodsAndSwitches([]testPod{}, []string{"node1"})...))

				// Once again, get pod from api with its metadata filled in
				pod, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(podTest.namespace).Get(context.TODO(), podTest.podName, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Expect(fakeOvn.controller.deleteLogicalPort(pod, nil)).To(gomega.Succeed(), "Deleting port that no longer exists should be okay")

				err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(pod.Namespace).Delete(context.TODO(), pod.Name, *metav1.NewDeleteOptions(0))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// check the retry cache has no entry
				retry.CheckRetryObjectEventually(key, false, fakeOvn.controller.retryPods)

				// Remove Logical Switch created on behalf of node and make sure deleteLogicalPort will not fail
				err = libovsdbops.DeleteLogicalSwitch(fakeOvn.controller.nbClient, pod.Spec.NodeName)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(fakeOvn.controller.deleteLogicalPort(pod, nil)).To(gomega.Succeed(), "Deleting port from switch that no longer exists should be okay")

				// Delete cache from lsManager and make sure deleteLogicalPort will not fail
				fakeOvn.controller.lsManager.DeleteSwitch(pod.Spec.NodeName)
				gomega.Expect(fakeOvn.controller.deleteLogicalPort(pod, nil)).To(gomega.Succeed(), "Deleting port from node that no longer exists should be okay")

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("remove a LSP from a pod that has no OVN annotations", func() {
			app.Action = func(*cli.Context) error {
				namespaceT := *ovntest.NewNamespace("namespace1")
				t := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespaceT.Name,
				)
				pod := ovntest.NewPod(t.namespace, t.podName, t.nodeName, t.podIP)
				fakeOvn.startWithDBSetup(initialDB,
					&corev1.NamespaceList{
						Items: []corev1.Namespace{
							namespaceT,
						},
					},
					&corev1.NodeList{
						Items: []corev1.Node{
							*newNode(node1Name, "192.168.126.202/24"),
						},
					},
					&corev1.PodList{
						Items: []corev1.Pod{
							*pod,
						},
					},
				)
				annotations := getPodAnnotations(fakeOvn.fakeClient.KubeClient, t.namespace, t.podName)
				gomega.Expect(annotations).To(gomega.Equal(""))

				// Deleting port from a pod that has no annotations should be okay
				err := fakeOvn.controller.deleteLogicalPort(pod, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("reconciles a deleted pod", func() {
			app.Action = func(*cli.Context) error {

				namespaceT := *ovntest.NewNamespace("namespace1")
				// Setup an assigned pod
				t := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespaceT.Name,
				)

				fakeOvn.startWithDBSetup(initialDB,
					&corev1.NamespaceList{
						Items: []corev1.Namespace{
							namespaceT,
						},
					},
					&corev1.NodeList{
						Items: []corev1.Node{
							*newNode(node1Name, "192.168.126.202/24"),
						},
					},
					&corev1.PodList{
						Items: []corev1.Pod{
							*ovntest.NewPod(t.namespace, t.podName, t.nodeName, t.podIP),
						},
					},
				)
				t.populateLogicalSwitchCache(fakeOvn)

				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(func() string {
					return getPodAnnotations(fakeOvn.fakeClient.KubeClient, t.namespace, t.podName)
				}, 2).Should(gomega.MatchJSON(t.getAnnotationsJson()))

				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(getDefaultNetExpectedPodsAndSwitches([]testPod{t}, []string{"node1"})))

				err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).Delete(context.TODO(), t.podName, *metav1.NewDeleteOptions(0))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).Get(context.TODO(), t.podName, metav1.GetOptions{})
				gomega.Expect(err).To(gomega.MatchError(apierrors.IsNotFound, "IsNotFound"))

				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(getDefaultNetExpectedPodsAndSwitches([]testPod{}, []string{"node1"})))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("retries a failed pod Add on Update", func() {
			app.Action = func(*cli.Context) error {

				namespaceT := *ovntest.NewNamespace("namespace1")
				// Setup an unassigned pod, perform an update later on which assigns it.
				t := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespaceT.Name,
				)

				fakeOvn.startWithDBSetup(initialDB,
					&corev1.NamespaceList{
						Items: []corev1.Namespace{
							namespaceT,
						},
					},
					&corev1.NodeList{
						Items: []corev1.Node{
							*newNode(node1Name, "192.168.126.202/24"),
						},
					},
					&corev1.PodList{
						Items: []corev1.Pod{
							*ovntest.NewPod(t.namespace, t.podName, t.nodeName, t.podIP),
						},
					},
				)
				t.populateLogicalSwitchCache(fakeOvn)

				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// allow pod retry from update annotation to fail
				time.Sleep(2 * time.Second)

				patch := struct {
					Metadata map[string]interface{} `json:"metadata"`
				}{
					Metadata: map[string]interface{}{
						"annotations": map[string]string{"dummy": "data"},
					},
				}
				patchData, err := json.Marshal(&patch)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// trigger update event
				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).Patch(context.TODO(), t.podName, types.MergePatchType, patchData, metav1.PatchOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(func() string {
					return getPodAnnotations(fakeOvn.fakeClient.KubeClient, t.namespace, t.podName)
				}, 2).Should(gomega.MatchJSON(t.getAnnotationsJson()))
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(getDefaultNetExpectedPodsAndSwitches([]testPod{t}, []string{"node1"})))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("pod Add should succeed even when namespace doesn't yet exist", func() {
			app.Action = func(*cli.Context) error {

				namespaceT := ovntest.NewNamespace("namespace1")
				t := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespaceT.Name,
				)
				podJSON := t.getAnnotationsJson()

				fakeOvn.startWithDBSetup(initialDB,
					&corev1.NodeList{
						Items: []corev1.Node{
							*newNode(node1Name, "192.168.126.202/24"),
						},
					},
				)
				t.populateLogicalSwitchCache(fakeOvn)
				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Add pod before namespace; pod will be annotated
				// but namespace address set will not exist
				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).Create(context.TODO(), ovntest.NewPod(t.namespace, t.podName, t.nodeName, t.podIP), metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(func() string { return getPodAnnotations(fakeOvn.fakeClient.KubeClient, t.namespace, t.podName) }, 2).Should(gomega.MatchJSON(podJSON))

				// Add Pod logical port should succeed even without namespace
				gomega.Expect(getPodAnnotations(fakeOvn.fakeClient.KubeClient, t.namespace, t.podName)).Should(gomega.MatchJSON(podJSON))

				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(getDefaultNetExpectedPodsAndSwitches([]testPod{t}, []string{"node1"})))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("pod Add should retry until namespace handling creates the port group", func() {
			app.Action = func(*cli.Context) error {
				config.OVNKubernetesFeature.EnableEgressFirewall = true

				namespaceT := ovntest.NewNamespace("namespace1")
				t := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespaceT.Name,
				)

				fakeOvn.startWithDBSetup(initialDB,
					&corev1.NamespaceList{Items: []corev1.Namespace{*namespaceT}},
					&corev1.NodeList{
						Items: []corev1.Node{
							*newNode(node1Name, "192.168.126.202/24"),
						},
					},
				)
				t.populateLogicalSwitchCache(fakeOvn)
				// Delay the namespace handler, not namespace creation in Kubernetes.
				err := fakeOvn.controller.WatchPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).Create(context.TODO(), ovntest.NewPod(t.namespace, t.podName, t.nodeName, t.podIP), metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(func() string {
					return getPodAnnotations(fakeOvn.fakeClient.KubeClient, t.namespace, t.podName)
				}, 2).Should(gomega.MatchJSON(t.getAnnotationsJson()))
				key := t.namespace + "/" + t.podName
				retry.CheckRetryObjectEventually(key, true, fakeOvn.controller.retryPods)
				_, err = libovsdbops.GetPortGroup(fakeOvn.nbClient,
					&nbdb.PortGroup{Name: fakeOvn.controller.getNamespacePortGroupName(t.namespace)})
				gomega.Expect(err).To(gomega.HaveOccurred())

				gomega.Expect(fakeOvn.controller.WatchNamespaces()).To(gomega.Succeed())
				gomega.Eventually(func() error {
					_, err := libovsdbops.GetPortGroup(fakeOvn.nbClient,
						&nbdb.PortGroup{Name: fakeOvn.controller.getNamespacePortGroupName(t.namespace)})
					return err
				}).Should(gomega.Succeed())
				retry.SetRetryObjWithNoBackoff(key, fakeOvn.controller.retryPods)
				fakeOvn.controller.retryPods.RequestRetryObjs()
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(
					getDefaultNetExpectedDataPodsSwitchesPortGroup([]testPod{t}, []string{"node1"}, namespaceT.Name)))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("pod Delete should remove namespace port group membership", func() {
			app.Action = func(*cli.Context) error {
				config.OVNKubernetesFeature.EnableEgressFirewall = true

				namespaceT := *ovntest.NewNamespace("namespace1")
				t := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespaceT.Name,
				)

				fakeOvn.startWithDBSetup(initialDB,
					&corev1.NamespaceList{
						Items: []corev1.Namespace{
							namespaceT,
						},
					},
					&corev1.NodeList{
						Items: []corev1.Node{
							*newNode(node1Name, "192.168.126.202/24"),
						},
					},
				)
				t.populateLogicalSwitchCache(fakeOvn)
				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).Create(context.TODO(), ovntest.NewPod(t.namespace, t.podName, t.nodeName, t.podIP), metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(
					getDefaultNetExpectedDataPodsSwitchesPortGroup([]testPod{t}, []string{"node1"}, namespaceT.Name)))

				err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).Delete(context.TODO(), t.podName, *metav1.NewDeleteOptions(0))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(
					getDefaultNetExpectedDataPodsSwitchesPortGroup(nil, []string{"node1"}, namespaceT.Name)))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("pod Delete should tolerate a missing namespace port group", func() {
			app.Action = func(*cli.Context) error {
				config.OVNKubernetesFeature.EnableEgressFirewall = true
				fakeOvn.startWithDBSetup(initialDB)

				ops, err := fakeOvn.controller.deletePodFromNamespace("unmanaged-namespace", fakeUUID)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(ops).To(gomega.BeEmpty())
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("pod Add should create namespace port group membership when multicast needs namespace port groups", func() {
			app.Action = func(*cli.Context) error {
				config.EnableMulticast = true

				namespaceT := ovntest.NewNamespace("namespace1")
				t := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespaceT.Name,
				)

				fakeOvn.startWithDBSetup(initialDB,
					&corev1.NamespaceList{Items: []corev1.Namespace{*namespaceT}},
					&corev1.NodeList{
						Items: []corev1.Node{
							*newNode(node1Name, "192.168.126.202/24"),
						},
					},
				)
				t.populateLogicalSwitchCache(fakeOvn)
				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).Create(context.TODO(), ovntest.NewPod(t.namespace, t.podName, t.nodeName, t.podIP), metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(func() string {
					return getPodAnnotations(fakeOvn.fakeClient.KubeClient, t.namespace, t.podName)
				}, 2).Should(gomega.MatchJSON(t.getAnnotationsJson()))

				pgIDs := getNamespacePortGroupDbIDs(namespaceT.Name, ovntypes.DefaultNetworkControllerName)
				namespacePortGroup := libovsdbutil.BuildPortGroup(
					pgIDs,
					[]*nbdb.LogicalSwitchPort{{UUID: t.portUUID}},
					nil,
				)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveDataSubset(namespacePortGroup))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})

	ginkgo.Context("on startup", func() {

		ginkgo.DescribeTable("preserves the current owner's SNAT when cleaning up a completed pod after restart", func(completedOwner bool) {
			config.IPv4Mode, config.IPv6Mode = true, true
			config.Gateway.DisableSNATMultipleGWs = true
			const namespace = "namespace1"
			ips := ovntest.MustParseIPNets("10.128.1.3/24", "fd00::3/64")
			annotation := &util.PodAnnotation{
				IPs: ips, MAC: util.IPAddrToHWAddr(ips[0].IP), Role: ovntypes.NetworkRolePrimary,
			}
			stale := ovntest.NewPod(namespace, "old-pod", node1Name, ips[0].IP.String())
			stale.Status.Phase = corev1.PodSucceeded
			stale.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodInitialized, LastTransitionTime: metav1.NewTime(time.Unix(100, 0))}}
			var err error
			stale.Annotations, err = util.MarshalPodAnnotation(stale.Annotations, annotation, ovntypes.DefaultNetworkName)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			owner := ovntest.NewPod(namespace, "owner", node1Name, ips[0].IP.String())
			owner.Status.Phase = corev1.PodRunning
			if completedOwner {
				owner.Status.Phase = corev1.PodSucceeded
			}
			owner.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodInitialized, LastTransitionTime: metav1.NewTime(time.Unix(200, 0))}}
			owner.Annotations, err = util.MarshalPodAnnotation(owner.Annotations, annotation, ovntypes.DefaultNetworkName)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// The old pod's cleanup finished before restart; only the new owner's
			// port and SNATs remain, but both pods still carry the reused IPs.
			lsp := &nbdb.LogicalSwitchPort{
				UUID: uuid.NewString(), Name: util.GetLogicalPortName(namespace, owner.Name),
				Addresses:   []string{annotation.MAC.String() + " 10.128.1.3 fd00::3"},
				ExternalIDs: map[string]string{"pod": "true"},
			}
			logicalSwitch := &nbdb.LogicalSwitch{UUID: uuid.NewString(), Name: node1Name, Ports: []string{lsp.UUID}}
			nat4 := &nbdb.NAT{UUID: uuid.NewString(), Type: nbdb.NATTypeSNAT, LogicalIP: "10.128.1.3", ExternalIP: "192.0.2.10"}
			nat6 := &nbdb.NAT{UUID: uuid.NewString(), Type: nbdb.NATTypeSNAT, LogicalIP: "fd00::3", ExternalIP: "2001:db8::10"}
			router := &nbdb.LogicalRouter{
				UUID: uuid.NewString(), Name: ovntypes.GWRouterPrefix + node1Name, Nat: []string{nat4.UUID, nat6.UUID},
			}
			initialDB.NBData = []libovsdbtest.TestData{logicalSwitch, lsp, router, nat4, nat6}
			fakeOvn.startWithDBSetup(initialDB, stale, owner, ovntest.NewNamespace(namespace), newNode(node1Name, "192.0.2.10/24"))
			oc := fakeOvn.controller
			gomega.Expect(oc.lsManager.AddOrUpdateSwitch(node1Name,
				ovntest.MustParseIPNets("10.128.1.0/24", "fd00::/64"), nil, nil)).To(gomega.Succeed())

			ginkgo.By("restoring ownership from the startup snapshot")
			gomega.Expect(oc.syncPods([]interface{}{stale, owner})).To(gomega.Succeed())
			gomega.Expect(oc.lsManager.OwnsIPs(node1Name, oc.podIPOwner(owner, ovntypes.DefaultNetworkName), ips)).To(gomega.BeTrue())
			ginkgo.By("processing the old completed pod again without changing the new owner's networking")
			gomega.Expect(oc.removePod(stale, nil)).To(gomega.Succeed())
			gomega.Consistently(fakeOvn.nbClient, time.Second).Should(libovsdbtest.HaveData(initialDB.NBData...))
			gomega.Expect(oc.lsManager.OwnsIPs(node1Name, oc.podIPOwner(owner, ovntypes.DefaultNetworkName), ips)).To(gomega.BeTrue())

			ginkgo.By("removing the owner's SNATs and reservation when its own cleanup runs")
			gomega.Expect(oc.removePod(owner, nil)).To(gomega.Succeed())
			gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(
				&nbdb.LogicalSwitch{UUID: logicalSwitch.UUID, Name: node1Name},
				&nbdb.LogicalRouter{UUID: router.UUID, Name: router.Name},
			))
			gomega.Expect(oc.lsManager.AllocateIPs(node1Name, "next-owner", ips)).To(gomega.Succeed())
		},
			ginkgo.Entry("running owner", false),
			ginkgo.Entry("completed owner awaiting cleanup", true),
		)

		ginkgo.It("reconciles a new pod", func() {
			app.Action = func(*cli.Context) error {

				namespaceT := *ovntest.NewNamespace("namespace1")
				t := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespaceT.Name,
				)

				fakeOvn.startWithDBSetup(initialDB,
					&corev1.NamespaceList{
						Items: []corev1.Namespace{
							namespaceT,
						},
					},
					&corev1.NodeList{
						Items: []corev1.Node{
							*newNode(node1Name, "192.168.126.202/24"),
						},
					},
					&corev1.PodList{
						Items: []corev1.Pod{
							*ovntest.NewPod(t.namespace, t.podName, t.nodeName, t.podIP),
						},
					},
				)
				t.populateLogicalSwitchCache(fakeOvn)
				// pod exists, networks annotations don't
				pod, err := fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).Get(context.TODO(), t.podName, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				_, ok := pod.Annotations[ovntypes.OvnPodAnnotationName]
				gomega.Expect(ok).To(gomega.BeFalse())

				err = fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// check that after start networks annotations and nbdb will be updated
				gomega.Eventually(func() string {
					return getPodAnnotations(fakeOvn.fakeClient.KubeClient, t.namespace, t.podName)
				}, 2).Should(gomega.MatchJSON(t.getAnnotationsJson()))

				gomega.Eventually(fakeOvn.nbClient).Should(
					libovsdbtest.HaveData(getDefaultNetExpectedPodsAndSwitches([]testPod{t}, []string{"node1"})))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("reconciles an existing pod without an existing logical switch port", func() {
			app.Action = func(*cli.Context) error {

				namespaceT := *ovntest.NewNamespace("namespace1")
				t := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespaceT.Name,
				)
				pod := ovntest.NewPod(t.namespace, t.podName, t.nodeName, t.podIP)
				setPodAnnotations(pod, t)
				fakeOvn.startWithDBSetup(initialDB,
					&corev1.NamespaceList{
						Items: []corev1.Namespace{
							namespaceT,
						},
					},
					&corev1.NodeList{
						Items: []corev1.Node{
							*newNode(node1Name, "192.168.126.202/24"),
						},
					},
					&corev1.PodList{
						Items: []corev1.Pod{
							*pod,
						},
					},
				)
				t.populateLogicalSwitchCache(fakeOvn)
				// pod annotations exist, lsp doesn't
				annotations := getPodAnnotations(fakeOvn.fakeClient.KubeClient, t.namespace, t.podName)
				gomega.Expect(annotations).To(gomega.MatchJSON(t.getAnnotationsJson()))

				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// check nbdb data is added
				gomega.Eventually(fakeOvn.nbClient).Should(
					libovsdbtest.HaveData(getDefaultNetExpectedPodsAndSwitches([]testPod{t}, []string{"node1"})))
				// check that the pod annotations are preserved
				// makes sense only when handling is finished, therefore check after nbdb is updated
				annotations = getPodAnnotations(fakeOvn.fakeClient.KubeClient, t.namespace, t.podName)
				gomega.Expect(annotations).To(gomega.MatchJSON(t.getAnnotationsJson()))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("reconciles an existing logical switch port without an existing pod", func() {
			app.Action = func(*cli.Context) error {
				namespaceT := *ovntest.NewNamespace("namespace1")
				// create ovsdb with no pod
				initialDB = libovsdbtest.TestSetup{
					NBData: []libovsdbtest.TestData{
						&nbdb.LogicalSwitchPort{
							UUID:      "namespace1_non-existing-pod-UUID",
							Name:      "namespace1_non-existing-pod",
							Addresses: []string{"0a:58:0a:80:02:03", "10.128.2.3"},
							ExternalIDs: map[string]string{
								"pod": "true",
							},
						},
						&nbdb.LogicalSwitch{
							UUID:  "ls-uuid",
							Name:  "node1",
							Ports: []string{"namespace1_non-existing-pod-UUID"},
						},
					},
				}

				testNode := corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name:        "node1",
						Annotations: map[string]string{},
					},
				}

				fakeOvn.startWithDBSetup(initialDB,
					&corev1.NamespaceList{
						Items: []corev1.Namespace{
							namespaceT,
						},
					},
					&corev1.NodeList{
						Items: []corev1.Node{
							testNode,
						},
					},
					// no pods
					&corev1.PodList{
						Items: []corev1.Pod{},
					},
				)

				err := fakeOvn.controller.lsManager.AddOrUpdateSwitch(testNode.Name, []*net.IPNet{ovntest.MustParseIPNet(v4Node1Subnet)}, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// expect stale logical switch port removed and stale logical switch port removed from logical switch
				expectData := []libovsdbtest.TestData{
					&nbdb.LogicalSwitch{
						UUID:  "ls-uuid",
						Name:  "node1",
						Ports: []string{},
					},
				}

				gomega.Eventually(fakeOvn.nbClient).Should(
					libovsdbtest.HaveData(expectData))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

		})

		ginkgo.It("reconciles an existing pod with an existing logical switch port", func() {
			app.Action = func(*cli.Context) error {
				namespaceT := *ovntest.NewNamespace("namespace1")
				// use 3 pods for different test options
				t1 := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod1",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespaceT.Name,
				)
				t2 := newTPod(
					"node2",
					"10.128.2.0/24",
					"10.128.2.2",
					"10.128.2.1",
					"myPod2",
					"10.128.2.3",
					"0a:58:0a:80:02:03",
					namespaceT.Name,
				)
				t3 := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod3",
					"10.128.1.4",
					"0a:58:0a:80:01:03",
					namespaceT.Name,
				)
				// add an outdated hybrid route for pod 3 to the hybrid overlay IF addr on the node
				t3Route := util.PodRoute{}
				_, t3Route.Dest, _ = net.ParseCIDR("10.132.0.0/14")
				_, nodeSubnet, _ := net.ParseCIDR(t3.nodeSubnet)
				t3Route.NextHop = util.GetNodeHybridOverlayIfAddr(nodeSubnet).IP
				t3.routes = []util.PodRoute{t3Route}

				initialDB = libovsdbtest.TestSetup{
					NBData: []libovsdbtest.TestData{
						&nbdb.LogicalSwitchPort{
							UUID:      t1.portUUID,
							Name:      util.GetLogicalPortName(t1.namespace, t1.podName),
							Addresses: []string{t1.podMAC, t1.podIP},
							ExternalIDs: map[string]string{
								"pod":       "true",
								"namespace": t1.namespace,
							},
							Options: map[string]string{
								// check requested-chassis will be updated to correct t1.nodeName value
								libovsdbops.RequestedChassis: requestedChassisForPod(t2),
								// check old value for iface-id-ver will be updated to pod.UID
								"iface-id-ver": "wrong_value",
							},
							PortSecurity: []string{fmt.Sprintf("%s %s", t1.podMAC, t1.podIP)},
						},
						&nbdb.LogicalSwitchPort{
							UUID:      t2.portUUID,
							Name:      util.GetLogicalPortName(t2.namespace, t2.podName),
							Addresses: []string{t2.podMAC, t2.podIP},
							ExternalIDs: map[string]string{
								"pod":       "true",
								"namespace": t2.namespace,
							},
							Options: map[string]string{
								libovsdbops.RequestedChassis: requestedChassisForPod(t2),
								//"iface-id-ver": is empty to check that it won't be set on update
							},
							PortSecurity: []string{fmt.Sprintf("%s %s", t2.podMAC, t2.podIP)},
						},
						&nbdb.LogicalSwitchPort{
							UUID:      t3.portUUID,
							Name:      util.GetLogicalPortName(t3.namespace, t3.podName),
							Addresses: []string{t3.podMAC, t3.podIP},
							ExternalIDs: map[string]string{
								"pod":       "true",
								"namespace": t3.namespace,
							},
							Options: map[string]string{
								// check requested-chassis will be updated to correct t1.nodeName value
								libovsdbops.RequestedChassis: requestedChassisForPod(t3),
								// check old value for iface-id-ver will be updated to pod.UID
								"iface-id-ver": "wrong_value",
							},
							PortSecurity: []string{fmt.Sprintf("%s %s", t3.podMAC, t3.podIP)},
						},
						&nbdb.LogicalSwitch{
							Name:  "node1",
							Ports: []string{t1.portUUID, t3.portUUID},
						},
						&nbdb.LogicalSwitch{
							Name:  "node2",
							Ports: []string{t2.portUUID},
						},
					},
				}
				// update TestPod to check nbdb lsp later
				t2.noIfaceIdVer = true

				pod1 := ovntest.NewPod(t1.namespace, t1.podName, t1.nodeName, t1.podIP)
				setPodAnnotations(pod1, t1)
				pod2 := ovntest.NewPod(t2.namespace, t2.podName, t2.nodeName, t2.podIP)
				setPodAnnotations(pod2, t2)
				pod3 := ovntest.NewPod(t3.namespace, t3.podName, t3.nodeName, t3.podIP)
				setPodAnnotations(pod3, t3)
				fakeOvn.startWithDBSetup(initialDB,
					&corev1.NamespaceList{
						Items: []corev1.Namespace{
							namespaceT,
						},
					},
					&corev1.NodeList{
						Items: []corev1.Node{
							*newNode(node1Name, "192.168.126.202/24"),
							*newNode(node2Name, "192.168.126.51/24"),
						},
					},
					&corev1.PodList{
						Items: []corev1.Pod{
							*pod1,
							*pod2,
							*pod3,
						},
					},
				)
				localPortUUIDs := map[string]string{}
				for _, localPod := range []testPod{t1, t3} {
					portName := util.GetLogicalPortName(localPod.namespace, localPod.podName)
					lsp, err := libovsdbops.GetLogicalSwitchPort(fakeOvn.nbClient, &nbdb.LogicalSwitchPort{Name: portName})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					localPortUUIDs[portName] = lsp.UUID
				}
				t1.populateLogicalSwitchCache(fakeOvn)
				t2.populateLogicalSwitchCache(fakeOvn)
				t3.populateLogicalSwitchCache(fakeOvn)
				// pod annotations and lsp exist now

				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// check db values are updated to correlate with test pods settings
				expectedPodsAndSwitches := getDefaultNetExpectedPodsAndSwitches([]testPod{t1, t2, t3}, []string{"node1", "node2"})
				for _, item := range expectedPodsAndSwitches {
					if lsp, ok := item.(*nbdb.LogicalSwitchPort); ok && lsp.Name == util.GetLogicalPortName(t2.namespace, t2.podName) {
						// A remote-zone port keeps its MAC and IP as separate
						// address entries.
						lsp.Addresses = []string{t2.podMAC, t2.podIP}
					}
				}
				gomega.Eventually(fakeOvn.nbClient).Should(
					libovsdbtest.HaveDataIgnoringUUIDs(expectedPodsAndSwitches))
				// Existing ports for pods on the controller node are updated in
				// place. The remote pod port may be reconstructed during startup.
				for _, localPod := range []testPod{t1, t3} {
					portName := util.GetLogicalPortName(localPod.namespace, localPod.podName)
					gomega.Eventually(func() string {
						lsp, err := libovsdbops.GetLogicalSwitchPort(fakeOvn.nbClient, &nbdb.LogicalSwitchPort{
							Name: portName,
						})
						if err != nil {
							return ""
						}
						return lsp.UUID
					}).Should(gomega.Equal(localPortUUIDs[portName]))
				}
				// check annotations are preserved
				// makes sense only when handling is finished, therefore check after nbdb is updated
				annotations := getPodAnnotations(fakeOvn.fakeClient.KubeClient, t1.namespace, t1.podName)
				gomega.Expect(annotations).To(gomega.MatchJSON(t1.getAnnotationsJson()))
				annotations = getPodAnnotations(fakeOvn.fakeClient.KubeClient, t2.namespace, t2.podName)
				gomega.Expect(annotations).To(gomega.MatchJSON(t2.getAnnotationsJson()))
				annotations = getPodAnnotations(fakeOvn.fakeClient.KubeClient, t3.namespace, t3.podName)
				// remove the outdated route from pod t3
				t3.routes = []util.PodRoute{}
				gomega.Expect(annotations).To(gomega.MatchJSON(t3.getAnnotationsJson()))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("cleans stale LSPs and ignore cleanup of stale ports on nodes with no LS", func() {
			// may occur if sync nodes runs out of host subnets to assign therefore a logical switch for a node never gets created.
			// expect reconciliation not to fail and to continue reconciliation for existing pods.
			// this test proves reconciliation continues for existing pods by checking a pod that doesn't exist in kapi
			// is cleaned up in OVN.
			app.Action = func(*cli.Context) error {
				initialDB := libovsdbtest.TestSetup{
					NBData: []libovsdbtest.TestData{
						&nbdb.LogicalSwitchPort{
							UUID:      "namespace1_non-existing-pod-UUID",
							Name:      "namespace1_non-existing-pod",
							Addresses: []string{"0a:58:0a:80:02:03", "10.128.2.3"},
							ExternalIDs: map[string]string{
								"pod": "true",
							},
						},
						&nbdb.LogicalSwitch{
							UUID:  "ls-uuid",
							Name:  "node1",
							Ports: []string{"namespace1_non-existing-pod-UUID"},
						},
					},
				}
				testNodeWithLS := corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name:        "node1",
						Annotations: map[string]string{},
					},
				}
				testNodeWithoutLS := corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name:        "node2",
						Annotations: map[string]string{},
					},
				}
				fakeOvn.startWithDBSetup(initialDB,
					&corev1.NodeList{
						Items: []corev1.Node{
							testNodeWithLS,
							testNodeWithoutLS,
						},
					},
					// no pods - we want to test that cleanup of pod LSP is successful within OVN DB despite one node having
					// no logical switch
					&corev1.PodList{
						Items: []corev1.Pod{},
					},
				)
				err := fakeOvn.controller.lsManager.AddOrUpdateSwitch(testNodeWithLS.Name, []*net.IPNet{ovntest.MustParseIPNet(v4Node1Subnet)}, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// expect stale logical switch port removed if reconciliation is successful
				expectData := []libovsdbtest.TestData{
					&nbdb.LogicalSwitch{
						UUID:  "ls-uuid",
						Name:  testNodeWithLS.Name,
						Ports: []string{},
					},
				}
				gomega.Eventually(fakeOvn.nbClient).Should(
					libovsdbtest.HaveData(expectData))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("Negative test: fails to add existing pod with an existing logical switch port on wrong node", func() {
			app.Action = func(*cli.Context) error {
				namespaceT := *ovntest.NewNamespace("namespace1")
				// use 2 pods for different test options
				t1 := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod1",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespaceT.Name,
				)

				initialDB = libovsdbtest.TestSetup{
					NBData: []libovsdbtest.TestData{
						&nbdb.LogicalSwitchPort{
							UUID:      t1.portUUID,
							Name:      util.GetLogicalPortName(t1.namespace, t1.podName),
							Addresses: []string{t1.podMAC, t1.podIP},
							ExternalIDs: map[string]string{
								"pod":       "true",
								"namespace": t1.namespace,
							},
							Options: map[string]string{
								// check requested-chassis will be updated to correct t1.nodeName value
								libovsdbops.RequestedChassis: requestedChassisForPod(t1),
								// check old value for iface-id-ver will be updated to pod.UID
								"iface-id-ver": "wrong_value",
							},
							PortSecurity: []string{fmt.Sprintf("%s %s", t1.podMAC, t1.podIP)},
						},
						&nbdb.LogicalSwitch{
							Name:  "node1",
							Ports: []string{},
						},
						&nbdb.LogicalSwitch{
							Name:  "node2",
							Ports: []string{t1.portUUID},
						},
					},
				}

				pod1 := ovntest.NewPod(t1.namespace, t1.podName, t1.nodeName, t1.podIP)
				setPodAnnotations(pod1, t1)
				fakeOvn.startWithDBSetup(initialDB,
					&corev1.NamespaceList{
						Items: []corev1.Namespace{
							namespaceT,
						},
					},
					&corev1.NodeList{
						Items: []corev1.Node{
							*newNode(node1Name, "192.168.126.202/24"),
						},
					},
					&corev1.PodList{
						Items: []corev1.Pod{
							*pod1,
						},
					},
				)
				t1.populateLogicalSwitchCache(fakeOvn)
				// pod annotations and lsp exist now

				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// should fail to update a port on the wrong switch
				myPod1Key, err := retry.GetResourceKey(pod1)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				retry.CheckRetryObjectEventually(myPod1Key, true, fakeOvn.controller.retryPods)

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("reconciles a terminating pod with no node", func() {
			app.Action = func(*cli.Context) error {

				namespaceT := *ovntest.NewNamespace("namespace1")
				t := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespaceT.Name,
				)

				p := ovntest.NewPod(t.namespace, t.podName, t.nodeName, t.podIP)
				now := metav1.Now()
				p.SetDeletionTimestamp(&now)

				fakeOvn.startWithDBSetup(initialDB,
					&corev1.NamespaceList{
						Items: []corev1.Namespace{
							namespaceT,
						},
					},
					&corev1.NodeList{
						Items: []corev1.Node{
							*newNode(node1Name, "192.168.126.202/24"),
						},
					},
					&corev1.PodList{
						Items: []corev1.Pod{
							*p,
						},
					},
				)
				// pod exists, networks annotations don't
				pod, err := fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).Get(context.TODO(), t.podName, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				_, ok := pod.Annotations[ovntypes.OvnPodAnnotationName]
				gomega.Expect(ok).To(gomega.BeFalse())

				err = fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// port should not be in cache, because it should never have been added
				_, err = fakeOvn.controller.logicalPortCache.get(pod, ovntypes.DefaultNetworkName)
				gomega.Expect(err).To(gomega.HaveOccurred())
				myPod1Key, err := retry.GetResourceKey(pod)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				retry.CheckRetryObjectEventually(myPod1Key, true, fakeOvn.controller.retryPods)
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("deletes an outdated hybrid overlay subnet route in dual stack configuration", func() {
			app.Action = func(*cli.Context) error {

				namespaceT := *ovntest.NewNamespace("namespace1")
				t := newTPod(
					"node1",
					"10.128.1.0/24 fd11::/64",
					"",
					"10.128.1.1 fd11::1",
					"myPod",
					"10.128.1.30 fd11::30",
					"0a:58:0a:80:01:03",
					namespaceT.Name,
				)

				// annotations without HO route
				expectedAnnotations := t.getAnnotationsJson()

				// add HO route
				t.routes = append(
					t.routes,
					util.PodRoute{
						Dest:    ovntest.MustParseIPNet("10.128.10.0/24"),
						NextHop: ovntest.MustParseIP("10.128.1.3"),
					},
				)

				// set annotattions on pod with the oudated HO route
				pod := ovntest.NewPod(t.namespace, t.podName, t.nodeName, t.podIP)
				setPodAnnotations(pod, t)

				fakeOvn.startWithDBSetup(initialDB,
					&corev1.NamespaceList{
						Items: []corev1.Namespace{
							namespaceT,
						},
					},
					&corev1.NodeList{
						Items: []corev1.Node{
							*newNode(node1Name, "192.168.126.202/24"),
						},
					},
					&corev1.PodList{
						Items: []corev1.Pod{
							*pod,
						},
					},
				)
				t.populateLogicalSwitchCache(fakeOvn)

				// pod exists with the expected annotation including the oudated HO route
				gomega.Eventually(func() string {
					return getPodAnnotations(fakeOvn.fakeClient.KubeClient, t.namespace, t.podName)
				}, 2).Should(gomega.MatchJSON(t.getAnnotationsJson()))

				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// check that after start the HO route has been removed
				gomega.Eventually(func() string {
					return getPodAnnotations(fakeOvn.fakeClient.KubeClient, t.namespace, t.podName)
				}, 2).Should(gomega.MatchJSON(expectedAnnotations))

				gomega.Eventually(fakeOvn.nbClient).Should(
					libovsdbtest.HaveData(getDefaultNetExpectedPodsAndSwitches([]testPod{t}, []string{"node1"})))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("won't release a completed pod IP if a running pod has the same IP", func() {
			app.Action = func(*cli.Context) error {
				namespaceT := *ovntest.NewNamespace("namespace1")

				completedTPod := newTPod(
					"node1",
					"10.128.1.0/24",
					"",
					"10.128.1.1",
					"myCompletedPod",
					"10.128.1.30",
					"0a:58:0a:80:01:03",
					namespaceT.Name,
				)
				completedPod := ovntest.NewPod(completedTPod.namespace, completedTPod.podName, completedTPod.nodeName, completedTPod.podIP)
				setPodAnnotations(completedPod, completedTPod)
				completedPod.UID = types.UID(completedPod.ObjectMeta.Name)
				completedPod.Status.Phase = corev1.PodSucceeded

				runningTPod := newTPod(
					"node1",
					"10.128.1.0/24",
					"",
					"10.128.1.1 fd11::1",
					"myRunningPod",
					"10.128.1.30",
					"0a:58:0a:80:01:03",
					namespaceT.Name,
				)
				runningPod := ovntest.NewPod(runningTPod.namespace, runningTPod.podName, runningTPod.nodeName, runningTPod.podIP)
				setPodAnnotations(runningPod, runningTPod)
				runningPod.UID = types.UID(runningPod.ObjectMeta.Name)

				fakeOvn.startWithDBSetup(initialDB,
					&corev1.NamespaceList{
						Items: []corev1.Namespace{
							namespaceT,
						},
					},
					&corev1.NodeList{
						Items: []corev1.Node{
							*newNode(node1Name, "192.168.126.202/24"),
						},
					},
					&corev1.PodList{
						Items: []corev1.Pod{
							*runningPod,
							*completedPod,
						},
					},
				)
				runningTPod.populateLogicalSwitchCache(fakeOvn)

				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				podIP, podNet, _ := net.ParseCIDR("10.128.1.30/24")
				podNet.IP = podIP
				gomega.Expect(fakeOvn.controller.lsManager.OwnsIPs(
					fakeOvn.controller.GetNetworkScopedSwitchName(node1Name),
					fakeOvn.controller.podIPOwner(runningPod, ovntypes.DefaultNetworkName),
					[]*net.IPNet{podNet})).To(gomega.BeTrue())
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should handle a scheduled or failed remote pod with no IPs annotated", func() {
			app.Action = func(*cli.Context) error {
				namespaceT := *ovntest.NewNamespace("namespace1")
				t := newTPod(
					node2Name,
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespaceT.Name,
				)
				myPod := ovntest.NewPod(t.namespace, t.podName, t.nodeName, t.podIP)

				// testing how a scheduled non-annotated pod is handled is
				// tricky, let's settle with a failed pod which should be
				// handled in a similar way
				myPod.Status.Phase = corev1.PodFailed

				fakeOvn.startWithDBSetup(initialDB,
					&corev1.NamespaceList{
						Items: []corev1.Namespace{
							namespaceT,
						},
					},
					&corev1.NodeList{
						Items: []corev1.Node{
							*newNode(node1Name, "192.168.126.202/24"),
							*newNode(node2Name, "192.168.126.203/24"),
						},
					},
					&corev1.PodList{
						Items: []corev1.Pod{*myPod},
					},
				)

				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// check that the pod is not being retried, it should have been
				// handled synchronously and succesfully in WatchPods
				podKey, err := retry.GetResourceKey(myPod)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(retry.CheckRetryObj(podKey, fakeOvn.controller.retryPods)).To(gomega.BeFalse())
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("uses the controller node identity for pod locality", func() {
			app.Action = func(*cli.Context) error {
				localNode := newNode(node1Name, "192.168.126.202/24")
				remoteNode := newNode(node2Name, "192.168.126.203/24")

				fakeOvn.startWithDBSetup(initialDB,
					&corev1.NodeList{
						Items: []corev1.Node{
							*localNode,
							*remoteNode,
						},
					},
				)

				localPod := ovntest.NewPod("ns1", "local-pod", node1Name, "10.128.1.3")
				remotePod := ovntest.NewPod("ns1", "remote-pod", node2Name, "10.128.1.4")
				unscheduledPod := ovntest.NewPod("ns1", "unscheduled-pod", "", "")

				gomega.Expect(fakeOvn.controller.isPodScheduledOnLocalNode(localPod)).To(gomega.BeTrue())
				gomega.Expect(fakeOvn.controller.isPodScheduledOnLocalNode(remotePod)).To(gomega.BeFalse())
				gomega.Expect(fakeOvn.controller.isPodScheduledOnLocalNode(unscheduledPod)).To(gomega.BeFalse())

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
		ginkgo.It("should correctly handle a pod running on no node", func() {
			app.Action = func(*cli.Context) error {
				namespaceT := *ovntest.NewNamespace("namespace1")
				t := newTPod(
					node2Name,
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespaceT.Name,
				)
				myPod := ovntest.NewPod(t.namespace, t.podName, t.nodeName, t.podIP)
				myPod.Status.Phase = corev1.PodRunning
				setPodAnnotations(myPod, t)
				initialDB = libovsdbtest.TestSetup{
					NBData: []libovsdbtest.TestData{},
				}
				fakeOvn.startWithDBSetup(initialDB,
					&corev1.NamespaceList{
						Items: []corev1.Namespace{
							namespaceT,
						},
					},
					&corev1.NodeList{
						Items: []corev1.Node{
							*newNode(node1Name, "192.168.126.202/24"),
						},
					},
					&corev1.PodList{
						Items: []corev1.Pod{*myPod},
					},
				)

				err := fakeOvn.controller.WatchPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should correctly handle a pod running on a no host subnet node", func() {
			app.Action = func(*cli.Context) error {
				testNs := "namespace1"
				testPodIP := "10.128.1.3"
				namespaceT := *ovntest.NewNamespace(testNs)
				myPod := ovntest.NewPod(testNs, "myPod", node2Name, testPodIP)
				myPod.Status.Phase = corev1.PodRunning
				initialDB = libovsdbtest.TestSetup{
					NBData: []libovsdbtest.TestData{},
				}
				fakeOvn.startWithDBSetup(initialDB,
					&corev1.NamespaceList{
						Items: []corev1.Namespace{
							namespaceT,
						},
					},
					&corev1.NodeList{
						Items: []corev1.Node{
							*newNode(node1Name, "192.168.126.202/24"),
							// Add a hybrid overlay node
							{
								ObjectMeta: metav1.ObjectMeta{
									Name: node2Name,
								},
								Status: corev1.NodeStatus{
									Conditions: []corev1.NodeCondition{
										{
											Type:   corev1.NodeReady,
											Status: corev1.ConditionTrue,
										},
									},
								},
							},
						},
					},
					&corev1.PodList{
						Items: []corev1.Pod{*myPod},
					},
				)
				err := fakeOvn.controller.lsManager.AddOrUpdateSwitch(myPod.Spec.NodeName, nil, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})
})
