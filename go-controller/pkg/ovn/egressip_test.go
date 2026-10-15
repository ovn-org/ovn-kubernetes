// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/urfave/cli/v2"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	utilnet "k8s.io/utils/net"
	"k8s.io/utils/ptr"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	egressipv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/addresssetmanager"
	egresssvc "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/controller/egressservice"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/retry"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/syncmap"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

var (
	nodeLogicalRouterIPv6      = []string{"fd98::2"}
	node2LogicalRouterIPv6     = []string{"fd98::3"}
	nodeLogicalRouterIPv4      = []string{"100.64.0.2"}
	node2LogicalRouterIPv4     = []string{"100.64.0.3"}
	node3LogicalRouterIPv4     = []string{"100.64.0.4"}
	nodeLogicalRouterIfAddrV6  = nodeLogicalRouterIPv6[0] + "/125"
	nodeLogicalRouterIfAddrV4  = nodeLogicalRouterIPv4[0] + "/29"
	node2LogicalRouterIfAddrV4 = node2LogicalRouterIPv4[0] + "/29"
	node2LogicalRouterIfAddrV6 = node2LogicalRouterIPv6[0] + "/125"
	node3LogicalRouterIfAddrV4 = node3LogicalRouterIPv4[0] + "/29"
)

const (
	eipNamespace    = "egressip-namespace"
	eipNamespace2   = "egressip-namespace2"
	podV4IP         = "10.128.0.15"
	podV4IP2        = "10.128.0.16"
	podV4IP3        = "10.128.1.3"
	podV4IP4        = "10.128.1.4"
	podV6IP         = "ae70::66"
	podV6IP2        = "be70::66"
	podV6IP3        = "be70::67"
	v6GatewayIP     = "ae70::1"
	v6Node1Subnet   = "ae70::66/64"
	v6Node2Subnet   = "be70::66/64"
	v6Node3Subnet   = "ce70::66/64"
	v4ClusterSubnet = "10.128.0.0/14"
	v4Node1Subnet   = "10.128.0.0/16"
	v4Node2Subnet   = "10.90.0.0/16"
	v4Node3Subnet   = "10.80.0.0/16"
	podName         = "egress-pod"
	podName2        = "egress-pod2"
	egressIPName    = "egressip"
	egressIP2Name   = "egressip-2"
	inspectTimeout  = 4 * time.Second // arbitrary, to avoid failures on github CI
)

func newEgressIPMeta(name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		UID:  k8stypes.UID(name),
		Name: name,
		Labels: map[string]string{
			"name": name,
		},
		Annotations: map[string]string{util.EgressIPMarkAnnotation: "50001"},
	}
}

type nodeInfo struct {
	addresses     []string
	transitPortIP string
}

var egressPodLabel = map[string]string{"egress": "needed"}

var _ = ginkgo.Describe("EgressIP node locality", func() {
	ginkgo.It("derives locality from controller identity only for observed nodes", func() {
		controller := &EgressIPController{
			nodeName:  "node1",
			nodeState: syncmap.NewSyncMap[struct{}](),
		}

		isLocal, loaded := controller.getNodeLocality("node1")
		gomega.Expect(isLocal).To(gomega.BeFalse())
		gomega.Expect(loaded).To(gomega.BeFalse())

		controller.nodeState.Store("node2", struct{}{})
		isLocal, loaded = controller.getNodeLocality("node2")
		gomega.Expect(isLocal).To(gomega.BeFalse())
		gomega.Expect(loaded).To(gomega.BeTrue())

		controller.nodeState.Store("node1", struct{}{})
		isLocal, loaded = controller.getNodeLocality("node1")
		gomega.Expect(isLocal).To(gomega.BeTrue())
		gomega.Expect(loaded).To(gomega.BeTrue())

		controller.nodeState.Delete("node1")
		isLocal, loaded = controller.getNodeLocality("node1")
		gomega.Expect(isLocal).To(gomega.BeFalse())
		gomega.Expect(loaded).To(gomega.BeFalse())
	})
})

var _ = ginkgo.Describe("Deprecated cluster node IP address set cleanup", func() {
	var fakeOvn *FakeOVN

	ginkgo.BeforeEach(func() {
		gomega.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	})

	ginkgo.AfterEach(func() {
		if fakeOvn != nil {
			fakeOvn.shutdown()
		}
	})

	ginkgo.It("cleans up unreferenced deprecated cluster node IP address sets", func() {
		oldDefaultV4AS, oldDefaultV6AS := addressset.GetTestDbAddrSets(
			getDeprecatedEgressIPClusterNodeIPsAddrSetDbIDsForNetwork(types.DefaultNetworkName),
			[]string{"10.0.0.1", "fd00::1"},
		)
		oldBlueV4AS, oldBlueV6AS := addressset.GetTestDbAddrSets(
			getDeprecatedEgressIPClusterNodeIPsAddrSetDbIDsForNetwork("blue"),
			[]string{"10.0.0.2", "fd00::2"},
		)
		fakeOvn = NewFakeOVN(false, "node1")
		fakeOvn.startWithDBSetup(libovsdbtest.TestSetup{
			NBData: []libovsdbtest.TestData{oldDefaultV4AS, oldDefaultV6AS, oldBlueV4AS, oldBlueV6AS},
		})

		gomega.Expect(cleanupDeprecatedClusterNodeIPsAddressSet(fakeOvn.nbClient)).To(gomega.Succeed())

		gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveEmptyData())
	})

	ginkgo.It("keeps deprecated cluster node IP address sets while referenced by matches", func() {
		oldDefaultV4AS, oldDefaultV6AS := addressset.GetTestDbAddrSets(
			getDeprecatedEgressIPClusterNodeIPsAddrSetDbIDsForNetwork(types.DefaultNetworkName),
			[]string{"10.0.0.1", "fd00::1"},
		)
		oldBlueV4AS, oldBlueV6AS := addressset.GetTestDbAddrSets(
			getDeprecatedEgressIPClusterNodeIPsAddrSetDbIDsForNetwork("blue"),
			[]string{"10.0.0.2", "fd00::2"},
		)
		nat := &nbdb.NAT{
			UUID:       "nat-UUID",
			Type:       nbdb.NATTypeSNAT,
			LogicalIP:  "10.128.0.0/14",
			ExternalIP: "172.18.0.2",
			Match:      fmt.Sprintf("ip4.dst == $%s", oldBlueV4AS.Name),
		}
		lrp := &nbdb.LogicalRouterPolicy{
			UUID:     "lrp-UUID",
			Priority: types.DefaultNoRereoutePriority,
			Action:   nbdb.LogicalRouterPolicyActionAllow,
			Match:    fmt.Sprintf("ip6.dst == $%s", oldDefaultV6AS.Name),
		}
		acl := libovsdbops.BuildACL(
			"acl",
			nbdb.ACLDirectionFromLport,
			types.DefaultAllowPriority,
			fmt.Sprintf("ip4.src == $%s || ip6.src == $%s", oldDefaultV4AS.Name, oldBlueV6AS.Name),
			nbdb.ACLActionAllow,
			types.OvnACLLoggingMeter,
			"",
			false,
			nil,
			nil,
			types.DefaultACLTier,
		)
		acl.UUID = "acl-UUID"
		router := &nbdb.LogicalRouter{
			UUID:     "router-UUID",
			Name:     "router",
			Nat:      []string{nat.UUID},
			Policies: []string{lrp.UUID},
		}
		logicalSwitch := &nbdb.LogicalSwitch{
			UUID: "switch-UUID",
			ACLs: []string{acl.UUID},
		}
		expectedDB := []libovsdbtest.TestData{
			oldDefaultV4AS,
			oldDefaultV6AS,
			oldBlueV4AS,
			oldBlueV6AS,
			nat,
			lrp,
			acl,
			router,
			logicalSwitch,
		}
		fakeOvn = NewFakeOVN(false, "node1")
		fakeOvn.startWithDBSetup(libovsdbtest.TestSetup{NBData: expectedDB})

		gomega.Expect(cleanupDeprecatedClusterNodeIPsAddressSet(fakeOvn.nbClient)).To(gomega.Succeed())

		gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDB))
	})
})

var _ = ginkgo.Describe("OVN EgressIP Operations cluster default network", func() {
	var (
		app     *cli.App
		fakeOvn *FakeOVN
	)
	const (
		node1Name = "node1"
		node2Name = "node2"
		node3Name = "node3"
	)

	clusterRouterDbSetup := libovsdbtest.TestSetup{
		NBData: []libovsdbtest.TestData{
			&nbdb.LogicalRouter{
				Name: types.OVNClusterRouter,
				UUID: types.OVNClusterRouter + "-UUID",
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

	getIPv4Nodes := func(nodeInfos []nodeInfo) []corev1.Node {
		// first address in each nodeAddress address is assumed to be OVN network
		nodeSuffix := 1
		nodeSubnets := []string{v4Node1Subnet, v4Node2Subnet, v4Node3Subnet}
		if len(nodeInfos) > len(nodeSubnets) {
			panic("not enough node subnets for the amount of nodes that needs to be created")
		}
		nodes := make([]corev1.Node, 0)
		var hostCIDRs string
		for i, ni := range nodeInfos {
			hostCIDRs = "["
			for _, nodeAddress := range ni.addresses {
				hostCIDRs = fmt.Sprintf("%s\"%s\",", hostCIDRs, nodeAddress)
			}
			hostCIDRs = strings.TrimSuffix(hostCIDRs, ",")
			hostCIDRs = hostCIDRs + "]"
			annotations := map[string]string{
				"k8s.ovn.org/node-primary-ifaddr":             fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", ni.addresses[0], ""),
				"k8s.ovn.org/node-subnets":                    fmt.Sprintf("{\"default\":\"%s\"}", nodeSubnets[i]),
				util.OVNNodeHostCIDRs:                         hostCIDRs,
				"k8s.ovn.org/node-transit-switch-port-ifaddr": fmt.Sprintf("{\"ipv4\":\"%s\"}", ni.transitPortIP), // used only for ic=true test
			}
			nodes = append(nodes, getNodeObj(fmt.Sprintf("node%d", nodeSuffix), annotations, map[string]string{}))
			nodeSuffix = nodeSuffix + 1
		}
		return nodes
	}

	getIPv6Nodes := func(nodeInfos []nodeInfo) []corev1.Node {
		// first address in each nodeAddress address is assumed to be OVN network
		nodeSuffix := 1
		nodeSubnets := []string{v6Node1Subnet, v6Node2Subnet}
		if len(nodeInfos) > len(nodeSubnets) {
			panic("not enough node subnets for the amount of nodes that needs to be created")
		}
		nodes := make([]corev1.Node, 0)
		var hostCIDRs string
		for i, ni := range nodeInfos {
			hostCIDRs = "["
			for _, nodeAddress := range ni.addresses {
				hostCIDRs = fmt.Sprintf("%s\"%s\",", hostCIDRs, nodeAddress)
			}
			hostCIDRs = strings.TrimSuffix(hostCIDRs, ",")
			hostCIDRs = hostCIDRs + "]"
			annotations := map[string]string{
				"k8s.ovn.org/node-primary-ifaddr":             fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", "", ni.addresses[0]),
				"k8s.ovn.org/node-subnets":                    fmt.Sprintf("{\"default\":\"%s\"}", nodeSubnets[i]),
				util.OVNNodeHostCIDRs:                         hostCIDRs,
				"k8s.ovn.org/node-transit-switch-port-ifaddr": fmt.Sprintf("{\"ipv6\":\"%s\"}", ni.transitPortIP), // used only for ic=true test
			}
			nodes = append(nodes, getNodeObj(fmt.Sprintf("node%d", nodeSuffix), annotations, map[string]string{}))
			nodeSuffix = nodeSuffix + 1
		}
		return nodes
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

		fakeOvn = NewFakeOVN(false, node1Name)
	})

	ginkgo.AfterEach(func() {
		fakeOvn.shutdown()
	})

	getPodAssignmentState := func(pod *corev1.Pod) *podAssignmentState {
		podKey := getPodKey(pod)
		fakeOvn.controller.eIPC.podAssignment.LockKey(podKey)
		defer fakeOvn.controller.eIPC.podAssignment.UnlockKey(podKey)
		if pas, exists := fakeOvn.controller.eIPC.podAssignment.Load(podKey); exists && pas != nil {
			return pas.Clone()
		}
		return nil
	}

	ginkgo.Context("On node UPDATE", func() {
		ginkgo.It("OVN network does not depend on EgressIP status for assignment", func() {
			egressIP := "192.168.126.101"
			node1IPv4OVN := "192.168.126.202/24"
			node1IPv4TranSwitchIP := "100.88.0.2/16"
			node1IPv4SecondaryHost1 := "10.10.10.4/24"
			node1IPv4SecondaryHost2 := "5.5.5.10/24"
			node1IPv4Addresses := []string{node1IPv4OVN, node1IPv4SecondaryHost1, node1IPv4SecondaryHost2}

			egressPod := *ovntest.NewPodWithLabels(eipNamespace, podName, node1Name, podV4IP, egressPodLabel)
			egressNamespace := ovntest.NewNamespace(eipNamespace)
			nodes := getIPv4Nodes([]nodeInfo{{node1IPv4Addresses, node1IPv4TranSwitchIP}})
			node1 := nodes[0]
			node1.Labels = map[string]string{
				"k8s.ovn.org/egress-assignable": "",
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
					Items: []egressipv1.EgressIPStatusItem{
						{
							Node:     node1Name,
							EgressIP: egressIP,
						},
					},
				},
			}
			node1Switch := &nbdb.LogicalSwitch{
				UUID: node1.Name + "-UUID",
				Name: node1.Name,
			}
			dynamicNeighRouters := "false"

			logicalRouterOptions := map[string]string{
				"dynamic_neigh_routers": dynamicNeighRouters,
			}

			fakeOvn.startWithDBSetup(
				libovsdbtest.TestSetup{
					NBData: []libovsdbtest.TestData{
						&nbdb.LogicalRouterPort{
							UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID",
							Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name,
							Networks: []string{nodeLogicalRouterIfAddrV4},
						},
						&nbdb.LogicalRouter{
							Name: types.OVNClusterRouter,
							UUID: types.OVNClusterRouter + "-UUID",
						},
						&nbdb.LogicalRouter{
							Name:    types.GWRouterPrefix + node1.Name,
							UUID:    types.GWRouterPrefix + node1.Name + "-UUID",
							Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID"},
							Options: logicalRouterOptions,
						},
						&nbdb.LogicalSwitchPort{
							UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID",
							Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
							Type: "router",
							Options: map[string]string{
								libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
								"nat-addresses":             "router",
								"exclude-lb-vips-from-garp": "true",
							},
						},
						&nbdb.LogicalSwitch{
							UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
							Name:  types.ExternalSwitchPrefix + node1Name,
							Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
						},
						node1Switch,
					},
				},
				&egressipv1.EgressIPList{
					Items: []egressipv1.EgressIP{eIP},
				},
				&corev1.NodeList{
					Items: []corev1.Node{node1},
				},
				&corev1.NamespaceList{
					Items: []corev1.Namespace{*egressNamespace},
				})

			err := fakeOvn.controller.WatchEgressIPPods()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = fakeOvn.controller.WatchEgressIPNamespaces()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = fakeOvn.controller.WatchEgressNodes()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = fakeOvn.controller.WatchEgressIP()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			i, n, _ := net.ParseCIDR(podV4IP + "/23")
			n.IP = i
			fakeOvn.controller.logicalPortCache.add(&egressPod, "", types.DefaultNetworkName, "", nil, []*net.IPNet{n})
			_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(egressPod.Namespace).Create(context.TODO(), &egressPod, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			node1Switch.QOSRules = []string{"default-QoS-UUID"}
			expectedNatLogicalPort := "k8s-node1"

			egressSVCServedPodsASv4, _ := buildEgressServiceAddressSets(nil)
			egressIPServedPodsASv4, _ := buildEgressIPServedPodsAddressSets([]string{podV4IP}, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName)
			ipNets, _ := util.ParseIPNets(node1IPv4Addresses)
			egressNodeIPs := []string{}
			for _, ipNet := range ipNets {
				egressNodeIPs = append(egressNodeIPs, ipNet.IP.String())
			}
			egressNodeIPsASv4, _ := buildEgressIPNodeAddressSets(egressNodeIPs)
			node1Switch.QOSRules = []string{"default-QoS-UUID"}
			expectedDatabaseState := []libovsdbtest.TestData{
				&nbdb.LogicalRouterPolicy{
					Priority:    types.DefaultNoRereoutePriority,
					Match:       "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
					Action:      nbdb.LogicalRouterPolicyActionAllow,
					UUID:        "default-no-reroute-UUID",
					ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
				},
				getNoReRouteReplyTrafficPolicy(types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
				&nbdb.LogicalRouterPolicy{
					Priority:    types.DefaultNoRereoutePriority,
					Match:       fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
					Action:      nbdb.LogicalRouterPolicyActionAllow,
					UUID:        "no-reroute-service-UUID",
					ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
				},
				&nbdb.LogicalRouterPolicy{
					Priority:    types.EgressIPReroutePriority,
					Match:       fmt.Sprintf("ip4.src == %s", egressPod.Status.PodIP),
					Action:      nbdb.LogicalRouterPolicyActionReroute,
					Nexthops:    nodeLogicalRouterIPv4,
					ExternalIDs: getEgressIPLRPReRouteDbIDs(eIP.Name, egressPod.Namespace, egressPod.Name, IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					UUID:        "reroute-UUID",
				},
				&nbdb.LogicalRouterPolicy{
					Priority:    types.DefaultNoRereoutePriority,
					Match:       "(ip4.src == $a8519615025667110816 || ip4.src == $a13607449821398607916) && ip4.dst == $a1042611113178530741",
					Action:      nbdb.LogicalRouterPolicyActionAllow,
					Options:     map[string]string{"pkt_mark": types.EgressIPNodeConnectionMark},
					UUID:        "no-reroute-node-UUID",
					ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
				},
				&nbdb.NAT{
					UUID:        "egressip-nat-UUID",
					LogicalIP:   podV4IP,
					ExternalIP:  egressIP,
					ExternalIDs: getEgressIPNATDbIDs(egressIPName, egressPod.Namespace, egressPod.Name, IPFamilyValueV4, fakeOvn.controller.controllerName).GetExternalIDs(),
					Type:        nbdb.NATTypeSNAT,
					LogicalPort: &expectedNatLogicalPort,
					Options: map[string]string{
						"stateless": "false",
					},
				},
				&nbdb.LogicalRouter{
					Name:    types.GWRouterPrefix + node1.Name,
					UUID:    types.GWRouterPrefix + node1.Name + "-UUID",
					Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID"},
					Nat:     []string{"egressip-nat-UUID"},
					Options: logicalRouterOptions,
				},
				&nbdb.LogicalRouter{
					Name:     types.OVNClusterRouter,
					UUID:     types.OVNClusterRouter + "-UUID",
					Policies: []string{"reroute-UUID", "default-no-reroute-UUID", "no-reroute-service-UUID", "no-reroute-node-UUID", "default-no-reroute-reply-traffic"},
				},
				&nbdb.LogicalRouterPort{
					UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID",
					Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name,
					Networks: []string{nodeLogicalRouterIfAddrV4},
				},
				&nbdb.LogicalSwitchPort{
					UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID",
					Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
					Type: "router",
					Options: map[string]string{
						libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
						"nat-addresses":             "router",
						"exclude-lb-vips-from-garp": "true",
					},
				},
				&nbdb.LogicalSwitch{
					UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
					Name:  types.ExternalSwitchPrefix + node1Name,
					Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
				},
				node1Switch,
				getDefaultQoSRule(false, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
				egressSVCServedPodsASv4,
				egressIPServedPodsASv4,
				egressNodeIPsASv4,
			}
			gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
		})

		ginkgo.It("Secondary host network does not depend on EgressIP status for assignment", func() {
			egressIP := "10.10.10.10"
			node1IPv4OVN := "192.168.126.202/24"
			node1IPv4TranSwitchIP := "100.88.0.2/16"
			node1IPv4SecondaryHost1 := "10.10.10.4/24"
			node1IPv4SecondaryHost2 := "5.5.5.10/24"
			_, node1Subnet, _ := net.ParseCIDR(v4Node1Subnet)
			node1IPv4Addresses := []string{node1IPv4OVN, node1IPv4SecondaryHost1, node1IPv4SecondaryHost2}

			egressPod := *ovntest.NewPodWithLabels(eipNamespace, podName, node1Name, podV4IP, egressPodLabel)
			egressNamespace := ovntest.NewNamespace(eipNamespace)
			nodes := getIPv4Nodes([]nodeInfo{{node1IPv4Addresses, node1IPv4TranSwitchIP}})
			node1 := nodes[0]
			node1.Labels = map[string]string{
				"k8s.ovn.org/egress-assignable": "",
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
					Items: []egressipv1.EgressIPStatusItem{
						{
							Node:     node1Name,
							EgressIP: egressIP,
						},
					},
				},
			}
			node1Switch := &nbdb.LogicalSwitch{
				UUID:  node1.Name + "-UUID",
				Name:  node1.Name,
				Ports: []string{"k8s-" + node1.Name + "-UUID"},
			}
			dynamicNeighRouters := "false"

			logicalRouterOptions := map[string]string{
				"dynamic_neigh_routers": dynamicNeighRouters,
			}
			fakeOvn.startWithDBSetup(
				libovsdbtest.TestSetup{
					NBData: []libovsdbtest.TestData{
						&nbdb.LogicalRouterPort{
							UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID",
							Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name,
							Networks: []string{nodeLogicalRouterIfAddrV4},
						},
						&nbdb.LogicalRouter{
							Name: types.OVNClusterRouter,
							UUID: types.OVNClusterRouter + "-UUID",
						},
						&nbdb.LogicalRouter{
							Name:    types.GWRouterPrefix + node1.Name,
							UUID:    types.GWRouterPrefix + node1.Name + "-UUID",
							Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID"},
							Options: logicalRouterOptions,
						},
						&nbdb.LogicalSwitchPort{
							UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID",
							Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
							Type: "router",
							Options: map[string]string{
								libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
								"nat-addresses":             "router",
								"exclude-lb-vips-from-garp": "true",
							},
						},
						&nbdb.LogicalSwitchPort{
							UUID:      "k8s-" + node1.Name + "-UUID",
							Name:      "k8s-" + node1.Name,
							Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1Subnet).IP.String()},
						},
						&nbdb.LogicalSwitch{
							UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
							Name:  types.ExternalSwitchPrefix + node1Name,
							Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
						},
						node1Switch,
					},
				},
				&egressipv1.EgressIPList{
					Items: []egressipv1.EgressIP{eIP},
				},
				&corev1.NodeList{
					Items: []corev1.Node{node1},
				},
				&corev1.NamespaceList{
					Items: []corev1.Namespace{*egressNamespace},
				})

			err := fakeOvn.controller.WatchEgressIPPods()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = fakeOvn.controller.WatchEgressIPNamespaces()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = fakeOvn.controller.WatchEgressNodes()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = fakeOvn.controller.WatchEgressIP()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			i, n, _ := net.ParseCIDR(podV4IP + "/23")
			n.IP = i
			fakeOvn.controller.logicalPortCache.add(&egressPod, "", types.DefaultNetworkName, "", nil, []*net.IPNet{n})
			_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(egressPod.Namespace).Create(context.TODO(), &egressPod, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			node1MgntIP, err := getSwitchManagementPortIP(&node1)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())

			node1Switch.QOSRules = []string{"default-QoS-UUID"}
			egressSVCServedPodsASv4, _ := buildEgressServiceAddressSets(nil)
			egressIPServedPodsASv4, _ := buildEgressIPServedPodsAddressSets([]string{podV4IP}, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName)
			ipNets, _ := util.ParseIPNets(node1IPv4Addresses)

			egressNodeIPs := []string{}
			for _, ipNet := range ipNets {
				egressNodeIPs = append(egressNodeIPs, ipNet.IP.String())
			}
			egressNodeIPsASv4, _ := buildEgressIPNodeAddressSets(egressNodeIPs)
			expectedDatabaseState := []libovsdbtest.TestData{
				&nbdb.LogicalRouterPolicy{
					Priority:    types.DefaultNoRereoutePriority,
					Match:       "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
					Action:      nbdb.LogicalRouterPolicyActionAllow,
					UUID:        "default-no-reroute-UUID",
					ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
				},
				getNoReRouteReplyTrafficPolicy(types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
				&nbdb.LogicalRouterPolicy{
					Priority:    types.DefaultNoRereoutePriority,
					Match:       fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
					Action:      nbdb.LogicalRouterPolicyActionAllow,
					UUID:        "no-reroute-service-UUID",
					ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
				},
				&nbdb.LogicalRouterPolicy{
					Priority:    types.EgressIPReroutePriority,
					Match:       fmt.Sprintf("ip4.src == %s", egressPod.Status.PodIP),
					Action:      nbdb.LogicalRouterPolicyActionReroute,
					Nexthops:    []string{node1MgntIP.To4().String()},
					Options:     map[string]string{"pkt_mark": types.EgressIPSecondaryInterfaceMark},
					ExternalIDs: getEgressIPLRPReRouteDbIDs(eIP.Name, egressPod.Namespace, egressPod.Name, IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					UUID:        "reroute-UUID",
				},
				&nbdb.LogicalRouterPolicy{
					Priority:    types.DefaultNoRereoutePriority,
					Match:       "(ip4.src == $a8519615025667110816 || ip4.src == $a13607449821398607916) && ip4.dst == $a1042611113178530741",
					Action:      nbdb.LogicalRouterPolicyActionAllow,
					Options:     map[string]string{"pkt_mark": types.EgressIPNodeConnectionMark},
					UUID:        "no-reroute-node-UUID",
					ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
				},
				&nbdb.LogicalRouter{
					Name:    types.GWRouterPrefix + node1.Name,
					UUID:    types.GWRouterPrefix + node1.Name + "-UUID",
					Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID"},
					Options: logicalRouterOptions,
				},
				&nbdb.LogicalRouter{
					Name:     types.OVNClusterRouter,
					UUID:     types.OVNClusterRouter + "-UUID",
					Policies: []string{"reroute-UUID", "default-no-reroute-UUID", "no-reroute-service-UUID", "no-reroute-node-UUID", "default-no-reroute-reply-traffic"},
				},
				&nbdb.LogicalRouterPort{
					UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID",
					Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name,
					Networks: []string{nodeLogicalRouterIfAddrV4},
				},
				&nbdb.LogicalSwitchPort{
					UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID",
					Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
					Type: "router",
					Options: map[string]string{
						libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
						"nat-addresses":             "router",
						"exclude-lb-vips-from-garp": "true",
					},
				},
				&nbdb.LogicalSwitchPort{
					UUID:      "k8s-" + node1.Name + "-UUID",
					Name:      "k8s-" + node1.Name,
					Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1Subnet).IP.String()},
				},
				&nbdb.LogicalSwitch{
					UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
					Name:  types.ExternalSwitchPrefix + node1Name,
					Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
				},
				node1Switch,
				getDefaultQoSRule(false, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
				egressSVCServedPodsASv4,
				egressIPServedPodsASv4,
				egressNodeIPsASv4,
			}
			gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
		})

		ginkgo.It("[OVN network] should perform proper OVN transactions when pod is created after node egress label switch",
			func() {
				app.Action = func(*cli.Context) error {
					egressIP := "192.168.126.101"

					node1IPv4OVN := "192.168.126.202/24"
					node1IPv4TranSwitchIP := "100.88.0.2/16"
					node2IPv4OVN := "192.168.126.51/24"
					node2IPv4TranSwitchIP := "100.88.0.3/16"
					node1IPv4SecondaryHost1 := "10.10.10.4/24"
					node1IPv4SecondaryHost2 := "5.5.5.10/24"
					node2IPv4SecondaryHost1 := "10.10.10.5/24"
					node2IPv4SecondaryHost2 := "7.7.7.9/16"
					node1IPv4Addresses := []string{node1IPv4OVN, node1IPv4SecondaryHost1, node1IPv4SecondaryHost2}
					node2IPv4Addresses := []string{node2IPv4OVN, node2IPv4SecondaryHost1, node2IPv4SecondaryHost2}

					egressPod := *ovntest.NewPodWithLabels(eipNamespace, podName, node1Name, podV4IP, egressPodLabel)
					egressNamespace := ovntest.NewNamespace(eipNamespace)
					nodes := getIPv4Nodes([]nodeInfo{{node1IPv4Addresses, node1IPv4TranSwitchIP},
						{node2IPv4Addresses, node2IPv4TranSwitchIP}})
					node1 := nodes[0]
					node1.Labels = map[string]string{
						"k8s.ovn.org/egress-assignable": "",
					}
					node2 := nodes[1]

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
							Items: []egressipv1.EgressIPStatusItem{
								{
									Node:     node1Name,
									EgressIP: egressIP,
								},
							},
						},
					}
					node1Switch := &nbdb.LogicalSwitch{
						UUID: node1.Name + "-UUID",
						Name: node1.Name,
					}
					node2Switch := &nbdb.LogicalSwitch{
						UUID: node2.Name + "-UUID",
						Name: node2.Name,
					}
					dynamicNeighRouters := "false"

					logicalRouterOptions := map[string]string{
						"dynamic_neigh_routers": dynamicNeighRouters,
					}
					fakeOvn.startWithDBSetup(
						libovsdbtest.TestSetup{
							NBData: []libovsdbtest.TestData{
								&nbdb.LogicalRouterPort{
									UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID",
									Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name,
									Networks: []string{node2LogicalRouterIfAddrV4},
								},
								&nbdb.LogicalRouterPort{
									UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID",
									Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name,
									Networks: []string{nodeLogicalRouterIfAddrV4},
								},
								&nbdb.LogicalRouter{
									Name: types.OVNClusterRouter,
									UUID: types.OVNClusterRouter + "-UUID",
								},
								&nbdb.LogicalRouter{
									Name:    types.GWRouterPrefix + node1.Name,
									UUID:    types.GWRouterPrefix + node1.Name + "-UUID",
									Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID"},
									Options: logicalRouterOptions,
								},
								&nbdb.LogicalRouter{
									Name:    types.GWRouterPrefix + node2.Name,
									UUID:    types.GWRouterPrefix + node2.Name + "-UUID",
									Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID"},
									Options: logicalRouterOptions,
								},
								&nbdb.LogicalSwitchPort{
									UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID",
									Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
									Type: "router",
									Options: map[string]string{
										libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
										"nat-addresses":             "router",
										"exclude-lb-vips-from-garp": "true",
									},
								},
								&nbdb.LogicalSwitchPort{
									UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID",
									Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
									Type: "router",
									Options: map[string]string{
										libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
										"nat-addresses":             "router",
										"exclude-lb-vips-from-garp": "true",
									},
								},
								&nbdb.LogicalSwitch{
									UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
									Name:  types.ExternalSwitchPrefix + node1Name,
									Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
								},
								&nbdb.LogicalSwitch{
									UUID:  types.ExternalSwitchPrefix + node2Name + "-UUID",
									Name:  types.ExternalSwitchPrefix + node2Name,
									Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
								},
								node1Switch,
								node2Switch,
							},
						},
						&egressipv1.EgressIPList{
							Items: []egressipv1.EgressIP{eIP},
						},
						&corev1.NodeList{
							Items: []corev1.Node{node2, node1},
						},
						&corev1.NamespaceList{
							Items: []corev1.Namespace{*egressNamespace},
						})

					err := fakeOvn.controller.WatchEgressIPPods()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchEgressIPNamespaces()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchEgressNodes()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchEgressIP()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					fakeOvn.patchEgressIPObj(node1Name, egressIPName, egressIP)
					gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
					egressIPs, eipNodes := getEgressIPStatus(egressIPName)
					gomega.Expect(eipNodes[0]).To(gomega.Equal(node1.Name))
					gomega.Expect(egressIPs[0]).To(gomega.Equal(egressIP))

					node1.Labels = map[string]string{}
					node2.Labels = map[string]string{
						"k8s.ovn.org/egress-assignable": "",
					}

					_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), &node1, metav1.UpdateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), &node2, metav1.UpdateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					fakeOvn.patchEgressIPObj(node2Name, egressIPName, egressIP)
					gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
					gomega.Eventually(nodeSwitch).Should(gomega.Equal(node2.Name))
					egressIPs, _ = getEgressIPStatus(egressIPName)
					gomega.Expect(egressIPs[0]).To(gomega.Equal(egressIP))

					i, n, _ := net.ParseCIDR(podV4IP + "/23")
					n.IP = i
					fakeOvn.controller.logicalPortCache.add(&egressPod, "", types.DefaultNetworkName, "", nil, []*net.IPNet{n})
					_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(egressPod.Namespace).Create(context.TODO(), &egressPod, metav1.CreateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					node1Switch.QOSRules = []string{"default-QoS-UUID"}
					egressSVCServedPodsASv4, _ := buildEgressServiceAddressSets(nil)
					egressIPServedPodsASv4, _ := buildEgressIPServedPodsAddressSets([]string{podV4IP}, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName)
					ipNets, _ := util.ParseIPNets(append(node1IPv4Addresses, node2IPv4Addresses...))

					egressNodeIPs := []string{}
					for _, ipNet := range ipNets {
						egressNodeIPs = append(egressNodeIPs, ipNet.IP.String())
					}
					egressNodeIPsASv4, _ := buildEgressIPNodeAddressSets(egressNodeIPs)
					expectedDatabaseState := []libovsdbtest.TestData{
						&nbdb.LogicalRouterPolicy{
							Priority:    types.DefaultNoRereoutePriority,
							Match:       "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
							Action:      nbdb.LogicalRouterPolicyActionAllow,
							UUID:        "default-no-reroute-UUID",
							ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
						},
						getNoReRouteReplyTrafficPolicy(types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
						&nbdb.LogicalRouterPolicy{
							Priority:    types.DefaultNoRereoutePriority,
							Match:       fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
							Action:      nbdb.LogicalRouterPolicyActionAllow,
							UUID:        "no-reroute-service-UUID",
							ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
						},
						&nbdb.LogicalRouterPolicy{
							Priority:    types.EgressIPReroutePriority,
							Match:       fmt.Sprintf("ip4.src == %s", egressPod.Status.PodIP),
							Action:      nbdb.LogicalRouterPolicyActionReroute,
							Nexthops:    []string{"100.88.0.3"},
							ExternalIDs: getEgressIPLRPReRouteDbIDs(eIP.Name, egressPod.Namespace, egressPod.Name, IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
							UUID:        "reroute-UUID",
						},
						&nbdb.LogicalRouterPolicy{
							Priority:    types.DefaultNoRereoutePriority,
							Match:       "(ip4.src == $a8519615025667110816 || ip4.src == $a13607449821398607916) && ip4.dst == $a1042611113178530741",
							Action:      nbdb.LogicalRouterPolicyActionAllow,
							Options:     map[string]string{"pkt_mark": types.EgressIPNodeConnectionMark},
							UUID:        "no-reroute-node-UUID",
							ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
						},
						&nbdb.LogicalRouter{
							Name:    types.GWRouterPrefix + node1.Name,
							UUID:    types.GWRouterPrefix + node1.Name + "-UUID",
							Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID"},
							Options: logicalRouterOptions,
						},
						&nbdb.LogicalRouter{
							Name:    types.GWRouterPrefix + node2.Name,
							UUID:    types.GWRouterPrefix + node2.Name + "-UUID",
							Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID"},
							Options: logicalRouterOptions,
						},
						&nbdb.LogicalRouter{
							Name:     types.OVNClusterRouter,
							UUID:     types.OVNClusterRouter + "-UUID",
							Policies: []string{"reroute-UUID", "default-no-reroute-UUID", "no-reroute-service-UUID", "no-reroute-node-UUID", "default-no-reroute-reply-traffic"},
						},
						&nbdb.LogicalRouterPort{
							UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID",
							Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name,
							Networks: []string{node2LogicalRouterIfAddrV4},
						},
						&nbdb.LogicalRouterPort{
							UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID",
							Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name,
							Networks: []string{nodeLogicalRouterIfAddrV4},
						},
						&nbdb.LogicalSwitchPort{
							UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID",
							Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
							Type: "router",
							Options: map[string]string{
								libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
								"nat-addresses":             "router",
								"exclude-lb-vips-from-garp": "true",
							},
						},
						&nbdb.LogicalSwitchPort{
							UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID",
							Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
							Type: "router",
							Options: map[string]string{
								libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
								"nat-addresses":             "router",
								"exclude-lb-vips-from-garp": "true",
							},
						},
						&nbdb.LogicalSwitch{
							UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
							Name:  types.ExternalSwitchPrefix + node1Name,
							Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
						},
						&nbdb.LogicalSwitch{
							UUID:  types.ExternalSwitchPrefix + node2Name + "-UUID",
							Name:  types.ExternalSwitchPrefix + node2Name,
							Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
						},
						node1Switch,
						node2Switch,
						getDefaultQoSRule(false, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
						egressSVCServedPodsASv4,
						egressIPServedPodsASv4,
						egressNodeIPsASv4,
					}

					gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			},
		)

		ginkgo.It("[OVN network] using EgressNode retry should perform proper OVN transactions when pod is created after node egress label switch",
			func() {
				app.Action = func(*cli.Context) error {
					egressIP := "192.168.126.101"
					node1IPv4OVN := "192.168.126.202/24"
					node1IPv4SecondaryHost1 := "10.10.10.4/24"
					node1IPv4SecondaryHost2 := "5.5.5.10/24"
					node1IPv4TranSwitchIP := "100.88.0.2/16"
					node2IPv4OVN := "192.168.126.51/24"
					node2IPv4SecondaryHost1 := "10.10.10.5/24"
					node2IPv4SecondaryHost2 := "7.7.7.9/16"
					node2IPv4TranSwitchIP := "100.88.0.3/16"
					node3IPv4OVN := "192.168.126.5/24"
					node3IPv4SecondaryHost1 := "10.10.10.6/24"
					node3IPv4TranSwitchIP := "100.88.0.4/16"
					node1IPv4Addresses := []string{node1IPv4OVN, node1IPv4SecondaryHost1, node1IPv4SecondaryHost2}
					node2IPv4Addresses := []string{node2IPv4OVN, node2IPv4SecondaryHost1, node2IPv4SecondaryHost2}
					node3IPv4Addresses := []string{node3IPv4OVN, node3IPv4SecondaryHost1}
					nodes := getIPv4Nodes([]nodeInfo{{node1IPv4Addresses, node1IPv4TranSwitchIP},
						{node2IPv4Addresses, node2IPv4TranSwitchIP}, {node3IPv4Addresses, node3IPv4TranSwitchIP}})
					node1 := nodes[0]
					node1.Labels = map[string]string{
						"k8s.ovn.org/egress-assignable": "",
					}
					node2 := nodes[1]
					node3 := nodes[2]
					config.IPv4Mode = true
					fakeOvn.nodeName = node3Name
					egressPod := *ovntest.NewPodWithLabels(eipNamespace, podName, node3Name, podV4IP, egressPodLabel)
					egressNamespace := ovntest.NewNamespace(eipNamespace)

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
					node2Switch := &nbdb.LogicalSwitch{
						UUID: node2.Name + "-UUID",
						Name: node2.Name,
					}
					node3Switch := &nbdb.LogicalSwitch{
						UUID: node3.Name + "-UUID",
						Name: node3.Name,
					}
					dynamicNeighRouters := "false"

					logicalRouterOptions := map[string]string{
						"dynamic_neigh_routers": dynamicNeighRouters,
					}
					initialDB := libovsdbtest.TestSetup{
						NBData: []libovsdbtest.TestData{
							&nbdb.LogicalRouterPort{
								UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID",
								Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name,
								Networks: []string{nodeLogicalRouterIfAddrV4},
							},
							&nbdb.LogicalRouterPort{
								UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID",
								Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name,
								Networks: []string{node2LogicalRouterIfAddrV4},
							},
							&nbdb.LogicalRouterPort{
								UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node3.Name + "-UUID",
								Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node3.Name,
								Networks: []string{node3LogicalRouterIfAddrV4},
							},
							&nbdb.LogicalRouter{
								Name: types.OVNClusterRouter,
								UUID: types.OVNClusterRouter + "-UUID",
							},
							&nbdb.LogicalRouter{
								Name:    types.GWRouterPrefix + node1.Name,
								UUID:    types.GWRouterPrefix + node1.Name + "-UUID",
								Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID"},
								Options: logicalRouterOptions,
							},
							&nbdb.LogicalRouter{
								Name:    types.GWRouterPrefix + node2.Name,
								UUID:    types.GWRouterPrefix + node2.Name + "-UUID",
								Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID"},
								Options: logicalRouterOptions,
							},
							&nbdb.LogicalRouter{
								Name:    types.GWRouterPrefix + node3.Name,
								UUID:    types.GWRouterPrefix + node3.Name + "-UUID",
								Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node3.Name + "-UUID"},
								Options: logicalRouterOptions,
							},
							&nbdb.LogicalSwitchPort{
								UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID",
								Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
								Type: "router",
								Options: map[string]string{
									libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
									"nat-addresses":             "router",
									"exclude-lb-vips-from-garp": "true",
								},
							},
							&nbdb.LogicalSwitchPort{
								UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID",
								Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
								Type: "router",
								Options: map[string]string{
									libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
									"nat-addresses":             "router",
									"exclude-lb-vips-from-garp": "true",
								},
							},
							&nbdb.LogicalSwitchPort{
								UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node3Name + "-UUID",
								Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node3Name,
								Type: "router",
								Options: map[string]string{
									libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node3Name,
									"nat-addresses":             "router",
									"exclude-lb-vips-from-garp": "true",
								},
							},
							&nbdb.LogicalSwitch{
								UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
								Name:  types.ExternalSwitchPrefix + node1Name,
								Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
							},
							&nbdb.LogicalSwitch{
								UUID:  types.ExternalSwitchPrefix + node2Name + "-UUID",
								Name:  types.ExternalSwitchPrefix + node2Name,
								Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
							},
							&nbdb.LogicalSwitch{
								UUID:  types.ExternalSwitchPrefix + node3Name + "-UUID",
								Name:  types.ExternalSwitchPrefix + node3Name,
								Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node3Name + "-UUID"},
							},
							node1Switch,
							node2Switch,
							node3Switch,
						},
					}
					fakeOvn.startWithDBSetup(
						initialDB,
						&egressipv1.EgressIPList{
							Items: []egressipv1.EgressIP{eIP},
						},
						&corev1.NodeList{
							Items: []corev1.Node{node1, node2, node3},
						},
						&corev1.NamespaceList{
							Items: []corev1.Namespace{*egressNamespace},
						})

					err := fakeOvn.controller.WatchEgressIPPods()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchEgressIPNamespaces()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchEgressNodes()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchEgressIP()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					fakeOvn.patchEgressIPObj(node1Name, egressIPName, egressIP)

					gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
					egressIPs, eIPNodes := getEgressIPStatus(egressIPName)
					gomega.Expect(eIPNodes[0]).To(gomega.Equal(node1.Name))
					gomega.Expect(egressIPs[0]).To(gomega.Equal(egressIP))

					node1.Labels = map[string]string{}
					node2.Labels = map[string]string{
						"k8s.ovn.org/egress-assignable": "",
					}
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
					fakeOvn.patchEgressIPObj(node2Name, egressIPName, egressIP)

					// sleep long enough for TransactWithRetry to fail, causing egressnode operations to fail
					// there is a chance that both egressnode events(node1 removal and node2 update) will end up in the same event queue
					// sleep for double the time to allow for two consecutive TransactWithRetry timeouts
					time.Sleep(2 * (config.Default.OVSDBTxnTimeout + time.Second))
					// check to see if the retry cache has an entry
					key1 := node1.Name
					ginkgo.By("retry entry: old obj should not be nil")
					retry.CheckRetryObjectMultipleFieldsEventually(
						key1,
						fakeOvn.controller.retryEgressNodes,
						gomega.Not(gomega.BeNil()), // oldObj should not be nil
						nil,
					)
					key2 := node2.Name
					ginkgo.By("retry entry: old obj should be nil, new obj should not be nil, config should not be nil")
					// (mk) FIXME: unknown why this isn't working
					//retry.CheckRetryObjectMultipleFieldsEventually(
					//	key2,
					//	fakeOvn.controller.retryEgressNodes,
					//	gomega.BeNil(),             // oldObj should be nil
					//	gomega.Not(gomega.BeNil()), // newObj should not be nil
					//	gomega.Not(gomega.BeNil()), // config should not be nil
					//)
					fakeOvn.patchEgressIPObj(node2Name, egressIPName, egressIP)
					connCtx, cancel := context.WithTimeout(context.Background(), config.Default.OVSDBTxnTimeout)
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
					node3Switch.QOSRules = []string{"default-QoS-UUID"}
					egressSVCServedPodsASv4, _ := buildEgressServiceAddressSets(nil)
					egressIPServedPodsASv4, _ := buildEgressIPServedPodsAddressSets([]string{podV4IP}, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName)
					ipNets, _ := util.ParseIPNets(append(node2IPv4Addresses, node3IPv4Addresses...))
					egressNodeIPs := []string{}
					for _, ipNet := range ipNets {
						egressNodeIPs = append(egressNodeIPs, ipNet.IP.String())
					}
					egressNodeIPsASv4, _ := buildEgressIPNodeAddressSets(egressNodeIPs)
					expectedDatabaseState := []libovsdbtest.TestData{
						&nbdb.LogicalRouterPolicy{
							Priority:    types.EgressIPReroutePriority,
							Match:       fmt.Sprintf("ip4.src == %s", egressPod.Status.PodIP),
							Action:      nbdb.LogicalRouterPolicyActionReroute,
							Nexthops:    []string{"100.88.0.3"},
							ExternalIDs: getEgressIPLRPReRouteDbIDs(eIP.Name, egressPod.Namespace, egressPod.Name, IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
							UUID:        "reroute-UUID",
						},
						&nbdb.LogicalRouterPolicy{
							Priority:    types.DefaultNoRereoutePriority,
							Match:       "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
							Action:      nbdb.LogicalRouterPolicyActionAllow,
							UUID:        "default-no-reroute-UUID",
							ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
						},
						&nbdb.LogicalRouterPolicy{
							Priority:    types.DefaultNoRereoutePriority,
							Match:       fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
							Action:      nbdb.LogicalRouterPolicyActionAllow,
							UUID:        "no-reroute-service-UUID",
							ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
						},
						&nbdb.LogicalRouterPolicy{
							Priority:    types.DefaultNoRereoutePriority,
							Match:       "(ip4.src == $a8519615025667110816 || ip4.src == $a13607449821398607916) && ip4.dst == $a1042611113178530741",
							Action:      nbdb.LogicalRouterPolicyActionAllow,
							Options:     map[string]string{"pkt_mark": types.EgressIPNodeConnectionMark},
							UUID:        "no-reroute-node-UUID",
							ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
						},
						&nbdb.LogicalRouter{
							Name:    types.GWRouterPrefix + node1.Name,
							UUID:    types.GWRouterPrefix + node1.Name + "-UUID",
							Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID"},
							Options: logicalRouterOptions,
						},
						&nbdb.LogicalRouter{
							Name:    types.GWRouterPrefix + node2.Name,
							UUID:    types.GWRouterPrefix + node2.Name + "-UUID",
							Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID"},
							Options: logicalRouterOptions,
						},
						&nbdb.LogicalRouter{
							Name:    types.GWRouterPrefix + node3.Name,
							UUID:    types.GWRouterPrefix + node3.Name + "-UUID",
							Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node3.Name + "-UUID"},
							Options: logicalRouterOptions,
						},
						&nbdb.LogicalRouter{
							Name: types.OVNClusterRouter,
							UUID: types.OVNClusterRouter + "-UUID",
							Policies: []string{"reroute-UUID", "default-no-reroute-UUID", "no-reroute-service-UUID",
								"no-reroute-node-UUID", "default-no-reroute-reply-traffic"},
						},
						&nbdb.LogicalRouterPort{
							UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID",
							Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name,
							Networks: []string{node2LogicalRouterIfAddrV4},
						},
						&nbdb.LogicalRouterPort{
							UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID",
							Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name,
							Networks: []string{nodeLogicalRouterIfAddrV4},
						},
						&nbdb.LogicalRouterPort{
							UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node3.Name + "-UUID",
							Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node3.Name,
							Networks: []string{node3LogicalRouterIfAddrV4},
						},
						&nbdb.LogicalSwitchPort{
							UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID",
							Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
							Type: "router",
							Options: map[string]string{
								libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
								"nat-addresses":             "router",
								"exclude-lb-vips-from-garp": "true",
							},
						},
						&nbdb.LogicalSwitchPort{
							UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID",
							Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
							Type: "router",
							Options: map[string]string{
								libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
								"nat-addresses":             "router",
								"exclude-lb-vips-from-garp": "true",
							},
						},
						&nbdb.LogicalSwitchPort{
							UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node3Name + "-UUID",
							Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node3Name,
							Type: "router",
							Options: map[string]string{
								libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node3Name,
								"nat-addresses":             "router",
								"exclude-lb-vips-from-garp": "true",
							},
						},
						&nbdb.LogicalSwitch{
							UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
							Name:  types.ExternalSwitchPrefix + node1Name,
							Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
						},
						&nbdb.LogicalSwitch{
							UUID:  types.ExternalSwitchPrefix + node2Name + "-UUID",
							Name:  types.ExternalSwitchPrefix + node2Name,
							Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
						},
						&nbdb.LogicalSwitch{
							UUID:  types.ExternalSwitchPrefix + node3Name + "-UUID",
							Name:  types.ExternalSwitchPrefix + node3Name,
							Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node3Name + "-UUID"},
						},
						getNoReRouteReplyTrafficPolicy(types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
						node1Switch,
						node2Switch,
						node3Switch,
						getDefaultQoSRule(false, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
						egressSVCServedPodsASv4,
						egressIPServedPodsASv4,
						egressNodeIPsASv4,
					}

					gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			},
		)

		ginkgo.It("[secondary host network] using EgressNode retry should perform proper OVN transactions when pod is created after node egress label switch",
			func() {
				app.Action = func(*cli.Context) error {
					egressIP := "10.10.10.7"
					node1IPv4OVN := "192.168.126.202/24"
					node1IPv4SecondaryHost1 := "10.10.10.4/24"
					node1IPv4SecondaryHost2 := "5.5.5.10/24"
					node1IPv4TranSwitchIP := "100.88.0.2/16"
					node2IPv4OVN := "192.168.126.51/24"
					node2IPv4SecondaryHost1 := "10.10.10.5/24"
					node2IPv4SecondaryHost2 := "7.7.7.9/16"
					node2IPv4TranSwitchIP := "100.88.0.3/16"
					node3IPv4OVN := "192.168.126.5/24"
					node3IPv4SecondaryHost1 := "12.10.10.6/24"
					node3IPv4TranSwitchIP := "100.88.0.4/16"
					_, node1Subnet, _ := net.ParseCIDR(v4Node1Subnet)
					_, node2Subnet, _ := net.ParseCIDR(v4Node2Subnet)
					_, node3Subnet, _ := net.ParseCIDR(v4Node3Subnet)
					node1IPv4Addresses := []string{node1IPv4OVN, node1IPv4SecondaryHost1, node1IPv4SecondaryHost2}
					node2IPv4Addresses := []string{node2IPv4OVN, node2IPv4SecondaryHost1, node2IPv4SecondaryHost2}
					node3IPv4Addresses := []string{node3IPv4OVN, node3IPv4SecondaryHost1}
					nodes := getIPv4Nodes([]nodeInfo{{node1IPv4Addresses, node1IPv4TranSwitchIP},
						{node2IPv4Addresses, node2IPv4TranSwitchIP}, {node3IPv4Addresses, node3IPv4TranSwitchIP}})
					node1 := nodes[0]
					node1.Labels = map[string]string{
						"k8s.ovn.org/egress-assignable": "",
					}
					node2 := nodes[1]
					node3 := nodes[2]
					config.IPv4Mode = true
					fakeOvn.nodeName = node3Name
					egressPod := *ovntest.NewPodWithLabels(eipNamespace, podName, node3Name, podV4IP, egressPodLabel)
					egressNamespace := ovntest.NewNamespace(eipNamespace)

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
						UUID:  node1.Name + "-UUID",
						Name:  node1.Name,
						Ports: []string{"k8s-" + node1.Name + "-UUID"},
					}
					node2Switch := &nbdb.LogicalSwitch{
						UUID:  node2.Name + "-UUID",
						Name:  node2.Name,
						Ports: []string{"k8s-" + node2.Name + "-UUID"},
					}
					node3Switch := &nbdb.LogicalSwitch{
						UUID:  node3.Name + "-UUID",
						Name:  node3.Name,
						Ports: []string{"k8s-" + node3.Name + "-UUID"},
					}
					dynamicNeighRouters := "false"

					logicalRouterOptions := map[string]string{
						"dynamic_neigh_routers": dynamicNeighRouters,
					}
					initialDB := libovsdbtest.TestSetup{
						NBData: []libovsdbtest.TestData{
							&nbdb.LogicalRouterPort{
								UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID",
								Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name,
								Networks: []string{node2LogicalRouterIfAddrV4},
							},
							&nbdb.LogicalRouterPort{
								UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID",
								Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name,
								Networks: []string{nodeLogicalRouterIfAddrV4},
							},
							&nbdb.LogicalRouterPort{
								UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node3.Name + "-UUID",
								Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node3.Name,
								Networks: []string{node3LogicalRouterIfAddrV4},
							},
							&nbdb.LogicalRouter{
								Name: types.OVNClusterRouter,
								UUID: types.OVNClusterRouter + "-UUID",
							},
							&nbdb.LogicalRouter{
								Name:    types.GWRouterPrefix + node1.Name,
								UUID:    types.GWRouterPrefix + node1.Name + "-UUID",
								Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID"},
								Options: logicalRouterOptions,
							},
							&nbdb.LogicalRouter{
								Name:    types.GWRouterPrefix + node2.Name,
								UUID:    types.GWRouterPrefix + node2.Name + "-UUID",
								Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID"},
								Options: logicalRouterOptions,
							},
							&nbdb.LogicalRouter{
								Name:    types.GWRouterPrefix + node3.Name,
								UUID:    types.GWRouterPrefix + node3.Name + "-UUID",
								Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node3.Name + "-UUID"},
								Options: logicalRouterOptions,
							},
							&nbdb.LogicalSwitchPort{
								UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID",
								Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
								Type: "router",
								Options: map[string]string{
									libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
									"nat-addresses":             "router",
									"exclude-lb-vips-from-garp": "true",
								},
							},
							&nbdb.LogicalSwitchPort{
								UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID",
								Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
								Type: "router",
								Options: map[string]string{
									libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
									"nat-addresses":             "router",
									"exclude-lb-vips-from-garp": "true",
								},
							},
							&nbdb.LogicalSwitchPort{
								UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node3Name + "-UUID",
								Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node3Name,
								Type: "router",
								Options: map[string]string{
									libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node3Name,
									"nat-addresses":             "router",
									"exclude-lb-vips-from-garp": "true",
								},
							},
							&nbdb.LogicalSwitchPort{
								UUID:      "k8s-" + node1.Name + "-UUID",
								Name:      "k8s-" + node1.Name,
								Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1Subnet).IP.String()},
							},
							&nbdb.LogicalSwitchPort{
								UUID:      "k8s-" + node2.Name + "-UUID",
								Name:      "k8s-" + node2.Name,
								Addresses: []string{"fe:1a:c2:3f:0e:fb " + util.GetNodeManagementIfAddr(node2Subnet).IP.String()},
							},
							&nbdb.LogicalSwitchPort{
								UUID:      "k8s-" + node3.Name + "-UUID",
								Name:      "k8s-" + node3.Name,
								Addresses: []string{"fe:1c:f2:3f:0e:fb " + util.GetNodeManagementIfAddr(node3Subnet).IP.String()},
							},
							&nbdb.LogicalSwitch{
								UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
								Name:  types.ExternalSwitchPrefix + node1Name,
								Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
							},
							&nbdb.LogicalSwitch{
								UUID:  types.ExternalSwitchPrefix + node2Name + "-UUID",
								Name:  types.ExternalSwitchPrefix + node2Name,
								Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
							},
							&nbdb.LogicalSwitch{
								UUID:  types.ExternalSwitchPrefix + node3Name + "-UUID",
								Name:  types.ExternalSwitchPrefix + node3Name,
								Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node3Name + "-UUID"},
							},
							node1Switch,
							node2Switch,
							node3Switch,
						},
					}
					fakeOvn.startWithDBSetup(
						initialDB,
						&egressipv1.EgressIPList{
							Items: []egressipv1.EgressIP{eIP},
						},
						&corev1.NodeList{
							Items: []corev1.Node{node1, node2, node3},
						},
						&corev1.NamespaceList{
							Items: []corev1.Namespace{*egressNamespace},
						})

					err := fakeOvn.controller.WatchEgressIPPods()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchEgressIPNamespaces()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchEgressNodes()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchEgressIP()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					fakeOvn.patchEgressIPObj(node1Name, egressIPName, egressIP)

					gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
					egressIPs, eIPNodes := getEgressIPStatus(egressIPName)
					gomega.Expect(eIPNodes[0]).To(gomega.Equal(node1.Name))
					gomega.Expect(egressIPs[0]).To(gomega.Equal(egressIP))

					node1.Labels = map[string]string{}
					node2.Labels = map[string]string{
						"k8s.ovn.org/egress-assignable": "",
					}
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
					fakeOvn.patchEgressIPObj(node2Name, egressIPName, egressIP)

					// sleep long enough for TransactWithRetry to fail, causing egressnode operations to fail
					// there is a chance that both egressnode events(node1 removal and node2 update) will end up in the same event queue
					// sleep for double the time to allow for two consecutive TransactWithRetry timeouts
					time.Sleep(2 * (config.Default.OVSDBTxnTimeout + time.Second))
					// check to see if the retry cache has an entry
					key1 := node1.Name
					ginkgo.By("retry entry: old obj should not be nil")
					retry.CheckRetryObjectMultipleFieldsEventually(
						key1,
						fakeOvn.controller.retryEgressNodes,
						gomega.Not(gomega.BeNil()), // oldObj should not be nil
						nil,
					)
					key2 := node2.Name
					ginkgo.By("retry entry: old obj should be nil, new obj should not be nil, config should not be nil")
					// FIXME: (mk): unknown why this is not working
					//retry.CheckRetryObjectMultipleFieldsEventually(
					//	key2,
					//	fakeOvn.controller.retryEgressNodes,
					//	gomega.BeNil(),             // oldObj should be nil
					//	gomega.Not(gomega.BeNil()), // newObj should not be nil
					//	gomega.Not(gomega.BeNil()), // config should not be nil
					//)
					connCtx, cancel := context.WithTimeout(context.Background(), config.Default.OVSDBTxnTimeout)
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
					gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
					node3Switch.QOSRules = []string{"default-QoS-UUID"}
					egressSVCServedPodsASv4, _ := buildEgressServiceAddressSets(nil)
					egressIPServedPodsASv4, _ := buildEgressIPServedPodsAddressSets([]string{podV4IP}, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName)
					ipNets, _ := util.ParseIPNets(append(node2IPv4Addresses, node3IPv4Addresses...))
					egressNodeIPs := []string{}
					for _, ipNet := range ipNets {
						egressNodeIPs = append(egressNodeIPs, ipNet.IP.String())
					}
					egressNodeIPsASv4, _ := buildEgressIPNodeAddressSets(egressNodeIPs)
					expectedDatabaseState := []libovsdbtest.TestData{
						&nbdb.LogicalRouterPolicy{
							Priority:    types.EgressIPReroutePriority,
							Match:       fmt.Sprintf("ip4.src == %s", egressPod.Status.PodIP),
							Action:      nbdb.LogicalRouterPolicyActionReroute,
							Nexthops:    []string{"100.88.0.3"},
							ExternalIDs: getEgressIPLRPReRouteDbIDs(eIP.Name, egressPod.Namespace, egressPod.Name, IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
							UUID:        "reroute-UUID",
						},
						&nbdb.LogicalRouterPolicy{
							Priority:    types.DefaultNoRereoutePriority,
							Match:       "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
							Action:      nbdb.LogicalRouterPolicyActionAllow,
							UUID:        "default-no-reroute-UUID",
							ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
						},
						getNoReRouteReplyTrafficPolicy(types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
						&nbdb.LogicalRouterPolicy{
							Priority:    types.DefaultNoRereoutePriority,
							Match:       fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
							Action:      nbdb.LogicalRouterPolicyActionAllow,
							UUID:        "no-reroute-service-UUID",
							ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
						},
						&nbdb.LogicalRouterPolicy{
							Priority:    types.DefaultNoRereoutePriority,
							Match:       "(ip4.src == $a8519615025667110816 || ip4.src == $a13607449821398607916) && ip4.dst == $a1042611113178530741",
							Action:      nbdb.LogicalRouterPolicyActionAllow,
							Options:     map[string]string{"pkt_mark": types.EgressIPNodeConnectionMark},
							UUID:        "no-reroute-node-UUID",
							ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
						},
						&nbdb.LogicalRouter{
							Name:    types.GWRouterPrefix + node1.Name,
							UUID:    types.GWRouterPrefix + node1.Name + "-UUID",
							Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID"},
							Options: logicalRouterOptions,
						},
						&nbdb.LogicalRouter{
							Name:    types.GWRouterPrefix + node2.Name,
							UUID:    types.GWRouterPrefix + node2.Name + "-UUID",
							Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID"},
							Nat:     []string{},
							Options: logicalRouterOptions,
						},
						&nbdb.LogicalRouter{
							Name:    types.GWRouterPrefix + node3.Name,
							UUID:    types.GWRouterPrefix + node3.Name + "-UUID",
							Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node3.Name + "-UUID"},
							Options: logicalRouterOptions,
						},
						&nbdb.LogicalRouter{
							Name:     types.OVNClusterRouter,
							UUID:     types.OVNClusterRouter + "-UUID",
							Policies: []string{"reroute-UUID", "default-no-reroute-UUID", "no-reroute-service-UUID", "no-reroute-node-UUID", "default-no-reroute-reply-traffic"},
						},
						&nbdb.LogicalRouterPort{
							UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID",
							Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name,
							Networks: []string{node2LogicalRouterIfAddrV4},
						},
						&nbdb.LogicalRouterPort{
							UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID",
							Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name,
							Networks: []string{nodeLogicalRouterIfAddrV4},
						},
						&nbdb.LogicalRouterPort{
							UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node3.Name + "-UUID",
							Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node3.Name,
							Networks: []string{node3LogicalRouterIfAddrV4},
						},
						&nbdb.LogicalSwitchPort{
							UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID",
							Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
							Type: "router",
							Options: map[string]string{
								libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
								"nat-addresses":             "router",
								"exclude-lb-vips-from-garp": "true",
							},
						},
						&nbdb.LogicalSwitchPort{
							UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID",
							Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
							Type: "router",
							Options: map[string]string{
								libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
								"nat-addresses":             "router",
								"exclude-lb-vips-from-garp": "true",
							},
						},
						&nbdb.LogicalSwitchPort{
							UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node3Name + "-UUID",
							Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node3Name,
							Type: "router",
							Options: map[string]string{
								libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node3Name,
								"nat-addresses":             "router",
								"exclude-lb-vips-from-garp": "true",
							},
						},
						&nbdb.LogicalSwitchPort{
							UUID:      "k8s-" + node1.Name + "-UUID",
							Name:      "k8s-" + node1.Name,
							Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1Subnet).IP.String()},
						},
						&nbdb.LogicalSwitchPort{
							UUID:      "k8s-" + node2.Name + "-UUID",
							Name:      "k8s-" + node2.Name,
							Addresses: []string{"fe:1a:c2:3f:0e:fb " + util.GetNodeManagementIfAddr(node2Subnet).IP.String()},
						},
						&nbdb.LogicalSwitchPort{
							UUID:      "k8s-" + node3.Name + "-UUID",
							Name:      "k8s-" + node3.Name,
							Addresses: []string{"fe:1c:f2:3f:0e:fb " + util.GetNodeManagementIfAddr(node3Subnet).IP.String()},
						},
						&nbdb.LogicalSwitch{
							UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
							Name:  types.ExternalSwitchPrefix + node1Name,
							Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
						},
						&nbdb.LogicalSwitch{
							UUID:  types.ExternalSwitchPrefix + node2Name + "-UUID",
							Name:  types.ExternalSwitchPrefix + node2Name,
							Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
						},
						&nbdb.LogicalSwitch{
							UUID:  types.ExternalSwitchPrefix + node3Name + "-UUID",
							Name:  types.ExternalSwitchPrefix + node3Name,
							Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node3Name + "-UUID"},
						},
						node1Switch,
						node2Switch,
						node3Switch,
						getDefaultQoSRule(false, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
						egressSVCServedPodsASv4,
						egressIPServedPodsASv4,
						egressNodeIPsASv4,
					}

					gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			},
		)

		ginkgo.DescribeTable("[secondary host network] should perform proper OVN transactions when namespace and pod is created after node egress label switch",
			func(interconnect bool, controllerZone string) {
				app.Action = func(*cli.Context) error {
					egressIP := "10.10.10.20"
					fakeOvn.nodeName = controllerZone
					node1Zone := node1Name
					node2Zone := node2Name
					node1Local := controllerZone == node1Zone
					node2Local := controllerZone == node2Zone

					node1IPv4OVN := "192.168.126.202/24"
					node1IPv4SecondaryHost1 := "10.10.10.4/24"
					node1IPv4SecondaryHost2 := "5.5.5.10/24"
					node1IPv4TranSwitchIP := "100.88.0.2/16"
					node2IPv4OVN := "192.168.126.51/24"
					node2IPv4SecondaryHost1 := "10.10.10.5/24"
					node2IPv4SecondaryHost2 := "7.7.7.9/16"
					node2IPv4TranSwitchIP := "100.88.0.3/16"
					_, node1Subnet, _ := net.ParseCIDR(v4Node1Subnet)
					_, node2Subnet, _ := net.ParseCIDR(v4Node2Subnet)
					node1IPv4Addresses := []string{node1IPv4OVN, node1IPv4SecondaryHost1, node1IPv4SecondaryHost2}
					node2IPv4Addresses := []string{node2IPv4OVN, node2IPv4SecondaryHost1, node2IPv4SecondaryHost2}

					nodes := getIPv4Nodes([]nodeInfo{{node1IPv4Addresses, node1IPv4TranSwitchIP},
						{node2IPv4Addresses, node2IPv4TranSwitchIP}})

					node1 := nodes[0]
					node1.Labels = map[string]string{
						"k8s.ovn.org/egress-assignable": "",
					}
					node2 := nodes[1]

					egressPod := *ovntest.NewPodWithLabels(eipNamespace, podName, node1Name, podV4IP, egressPodLabel)
					egressNamespace := ovntest.NewNamespace(eipNamespace)

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
						UUID:  node1.Name + "-UUID",
						Name:  node1.Name,
						Ports: []string{"k8s-" + node1.Name + "-UUID"},
					}
					node2Switch := &nbdb.LogicalSwitch{
						UUID:  node2.Name + "-UUID",
						Name:  node2.Name,
						Ports: []string{"k8s-" + node2.Name + "-UUID"},
					}
					dynamicNeighRouters := "false"

					logicalRouterOptions := map[string]string{
						"dynamic_neigh_routers": dynamicNeighRouters,
					}

					fakeOvn.startWithDBSetup(
						libovsdbtest.TestSetup{
							NBData: []libovsdbtest.TestData{
								&nbdb.LogicalRouterPort{
									UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID",
									Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name,
									Networks: []string{nodeLogicalRouterIfAddrV4},
								},
								&nbdb.LogicalRouterPort{
									UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID",
									Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name,
									Networks: []string{node2LogicalRouterIfAddrV4},
								},
								&nbdb.LogicalRouter{
									Name: types.OVNClusterRouter,
									UUID: types.OVNClusterRouter + "-UUID",
								},
								&nbdb.LogicalRouter{
									Name:    types.GWRouterPrefix + node1.Name,
									UUID:    types.GWRouterPrefix + node1.Name + "-UUID",
									Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID"},
									Options: logicalRouterOptions,
								},
								&nbdb.LogicalRouter{
									Name:    types.GWRouterPrefix + node2.Name,
									UUID:    types.GWRouterPrefix + node2.Name + "-UUID",
									Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID"},
									Nat:     nil,
									Options: logicalRouterOptions,
								},
								&nbdb.LogicalSwitchPort{
									UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID",
									Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
									Type: "router",
									Options: map[string]string{
										libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
										"nat-addresses":             "router",
										"exclude-lb-vips-from-garp": "true",
									},
								},
								&nbdb.LogicalSwitchPort{
									UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID",
									Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
									Type: "router",
									Options: map[string]string{
										libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
										"nat-addresses":             "router",
										"exclude-lb-vips-from-garp": "true",
									},
								},
								&nbdb.LogicalSwitchPort{
									UUID:      "k8s-" + node1.Name + "-UUID",
									Name:      "k8s-" + node1.Name,
									Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1Subnet).IP.String()},
								},
								&nbdb.LogicalSwitchPort{
									UUID:      "k8s-" + node2.Name + "-UUID",
									Name:      "k8s-" + node2.Name,
									Addresses: []string{"fe:1a:c2:3f:0e:fb " + util.GetNodeManagementIfAddr(node2Subnet).IP.String()},
								},
								&nbdb.LogicalSwitch{
									UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
									Name:  types.ExternalSwitchPrefix + node1Name,
									Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
								},
								&nbdb.LogicalSwitch{
									UUID:  types.ExternalSwitchPrefix + node2Name + "-UUID",
									Name:  types.ExternalSwitchPrefix + node2Name,
									Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
								},
								node1Switch,
								node2Switch,
							},
						},
						&egressipv1.EgressIPList{
							Items: []egressipv1.EgressIP{eIP},
						},
						&corev1.NodeList{
							Items: []corev1.Node{node1, node2},
						})

					i, n, _ := net.ParseCIDR(podV4IP + "/23")
					n.IP = i
					fakeOvn.controller.logicalPortCache.add(&egressPod, "", types.DefaultNetworkName, "", nil, []*net.IPNet{n})

					err := fakeOvn.controller.WatchEgressIPPods()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchEgressIPNamespaces()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchEgressNodes()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					fakeOvn.patchEgressIPObj(node1Name, egressIPName, egressIP)

					err = fakeOvn.controller.WatchEgressIP()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
					egressIPs, eIPNodes := getEgressIPStatus(egressIPName)
					gomega.Expect(eIPNodes[0]).To(gomega.Equal(node1.Name))
					gomega.Expect(egressIPs[0]).To(gomega.Equal(egressIP))

					node1.Labels = map[string]string{}
					node2.Labels = map[string]string{
						"k8s.ovn.org/egress-assignable": "",
					}

					_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), &node1, metav1.UpdateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), &node2, metav1.UpdateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					fakeOvn.patchEgressIPObj(node2Name, egressIPName, egressIP)
					gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
					gomega.Eventually(nodeSwitch).Should(gomega.Equal(node2.Name))
					egressIPs, _ = getEgressIPStatus(egressIPName)
					gomega.Expect(egressIPs[0]).To(gomega.Equal(egressIP))

					_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Create(context.TODO(), egressNamespace, metav1.CreateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(egressPod.Namespace).Create(context.TODO(), &egressPod, metav1.CreateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					node2MgntIP, err := getSwitchManagementPortIP(&node2)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					reroutePolicyNextHop := []string{node2MgntIP.To4().String()}
					if interconnect && node1Zone != node2Zone && !node2Local {

						//todo fix me
						reroutePolicyNextHop = []string{"100.88.0.3"} // node2's transit switch portIP
					}

					egressSVCServedPodsASv4, _ := buildEgressServiceAddressSets(nil)
					servedPodIPs := []string{podV4IP}
					// pod is located on node1, therefore if remote, we dont expect to see its IPs in the served pods address set
					if !node1Local {
						servedPodIPs = nil
					}
					egressIPServedPodsASv4, _ := buildEgressIPServedPodsAddressSets(servedPodIPs, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName)
					var reroutePolicy *nbdb.LogicalRouterPolicy
					if !node2Local {
						reroutePolicy = getReRoutePolicy(egressPod.Status.PodIP, "4", "reroute-UUID", reroutePolicyNextHop,
							getEgressIPLRPReRouteDbIDs(eIP.Name, egressPod.Namespace, egressPod.Name, IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs())
					} else {
						reroutePolicy = getReRoutePolicySecondaryHost(egressPod.Status.PodIP, "4", "reroute-UUID", reroutePolicyNextHop,
							getEgressIPLRPReRouteDbIDs(eIP.Name, egressPod.Namespace, egressPod.Name, IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs())
					}
					expectedDatabaseState := []libovsdbtest.TestData{
						reroutePolicy,
						&nbdb.LogicalRouterPolicy{
							Priority:    types.DefaultNoRereoutePriority,
							Match:       "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
							Action:      nbdb.LogicalRouterPolicyActionAllow,
							UUID:        "default-no-reroute-UUID",
							ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
						},
						&nbdb.LogicalRouterPolicy{
							Priority:    types.DefaultNoRereoutePriority,
							Match:       fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
							Action:      nbdb.LogicalRouterPolicyActionAllow,
							UUID:        "no-reroute-service-UUID",
							ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
						},
						&nbdb.LogicalRouterPolicy{
							Priority:    types.DefaultNoRereoutePriority,
							Match:       "(ip4.src == $a8519615025667110816 || ip4.src == $a13607449821398607916) && ip4.dst == $a1042611113178530741",
							Action:      nbdb.LogicalRouterPolicyActionAllow,
							Options:     map[string]string{"pkt_mark": types.EgressIPNodeConnectionMark},
							UUID:        "no-reroute-node-UUID",
							ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
						},
						&nbdb.LogicalRouter{
							Name:    types.GWRouterPrefix + node1.Name,
							UUID:    types.GWRouterPrefix + node1.Name + "-UUID",
							Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID"},
							Options: logicalRouterOptions,
						},
						&nbdb.LogicalRouter{
							Name:    types.GWRouterPrefix + node2.Name,
							UUID:    types.GWRouterPrefix + node2.Name + "-UUID",
							Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID"},
							Options: logicalRouterOptions,
						},
						&nbdb.LogicalRouter{
							Name:     types.OVNClusterRouter,
							UUID:     types.OVNClusterRouter + "-UUID",
							Policies: []string{"reroute-UUID", "no-reroute-node-UUID", "default-no-reroute-UUID", "no-reroute-service-UUID", "default-no-reroute-reply-traffic"},
						},
						&nbdb.LogicalRouterPort{
							UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID",
							Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name,
							Networks: []string{nodeLogicalRouterIfAddrV4},
						},
						&nbdb.LogicalRouterPort{
							UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID",
							Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name,
							Networks: []string{node2LogicalRouterIfAddrV4},
						},
						&nbdb.LogicalSwitchPort{
							UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID",
							Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
							Type: "router",
							Options: map[string]string{
								libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
								"nat-addresses":             "router",
								"exclude-lb-vips-from-garp": "true",
							},
						},
						&nbdb.LogicalSwitchPort{
							UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID",
							Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
							Type: "router",
							Options: map[string]string{
								libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
								"nat-addresses":             "router",
								"exclude-lb-vips-from-garp": "true",
							},
						},
						&nbdb.LogicalSwitchPort{
							UUID:      "k8s-" + node1.Name + "-UUID",
							Name:      "k8s-" + node1.Name,
							Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1Subnet).IP.String()},
						},
						&nbdb.LogicalSwitchPort{
							UUID:      "k8s-" + node2.Name + "-UUID",
							Name:      "k8s-" + node2.Name,
							Addresses: []string{"fe:1a:c2:3f:0e:fb " + util.GetNodeManagementIfAddr(node2Subnet).IP.String()},
						},
						&nbdb.LogicalSwitch{
							UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
							Name:  types.ExternalSwitchPrefix + node1Name,
							Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
						},
						&nbdb.LogicalSwitch{
							UUID:  types.ExternalSwitchPrefix + node2Name + "-UUID",
							Name:  types.ExternalSwitchPrefix + node2Name,
							Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
						},
						getNoReRouteReplyTrafficPolicy(types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
						node1Switch,
						node2Switch,
						getDefaultQoSRule(false, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
						egressSVCServedPodsASv4,
						egressIPServedPodsASv4,
					}
					if node2Local {
						// add qosrules only if node is in local zone
						node2Switch.QOSRules = []string{"default-QoS-UUID"}
					}
					if node1Local {
						// QoS Rule is configured only for nodes in local zones; the controller in the remote zone will do it for the remote nodes
						node1Switch.QOSRules = []string{"default-QoS-UUID"}
						ipNets, _ := util.ParseIPNets(append(node1IPv4Addresses, node2IPv4Addresses...))
						egressNodeIPs := []string{}
						for _, ipNet := range ipNets {
							egressNodeIPs = append(egressNodeIPs, ipNet.IP.String())
						}
						egressNodeIPsASv4, _ := buildEgressIPNodeAddressSets(egressNodeIPs)
						expectedDatabaseState = append(expectedDatabaseState, egressNodeIPsASv4)
					}
					if !node1Local {
						expectedDatabaseState = append(expectedDatabaseState, getReRoutePolicySecondaryHost(egressPod.Status.PodIP, "4", "remote-reroute-UUID", reroutePolicyNextHop,
							getEgressIPLRPReRouteDbIDs(eIP.Name, egressPod.Namespace, egressPod.Name, IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs()))
						expectedDatabaseState[6].(*nbdb.LogicalRouter).Policies = expectedDatabaseState[6].(*nbdb.LogicalRouter).Policies[1:]                            // remove LRP ref
						expectedDatabaseState[6].(*nbdb.LogicalRouter).Policies = append(expectedDatabaseState[6].(*nbdb.LogicalRouter).Policies, "remote-reroute-UUID") // remove LRP ref
						expectedDatabaseState = expectedDatabaseState[1:]                                                                                                // remove LRP
						ipNets, _ := util.ParseIPNets(append(node1IPv4Addresses, node2IPv4Addresses...))
						egressNodeIPs := []string{}
						for _, ipNet := range ipNets {
							egressNodeIPs = append(egressNodeIPs, ipNet.IP.String())
						}
						egressNodeIPsASv4, _ := buildEgressIPNodeAddressSets(egressNodeIPs)
						expectedDatabaseState = append(expectedDatabaseState, egressNodeIPsASv4)
					}

					gomega.Eventually(fakeOvn.nbClient, inspectTimeout).Should(libovsdbtest.HaveData(expectedDatabaseState))

					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			},
			// will showcase localzone setup - the controller for the pod's zone is local, where pod's reroute policy towards egressNode will be done.
			// NOTE: SNAT won't be visible because its in remote zone
			ginkgo.Entry("interconnect enabled; node1 in local and node2 in remote zones", true, node1Name),
			// will showcase localzone setup - the controller for the egress node's zone is local, where pod's SNAT policy and static route will be done.
			// NOTE: reroute policy won't be visible because its in remote zone (pod is in remote zone)
			ginkgo.Entry("interconnect enabled; node1 in remote and node2 in local zones", true, node2Name),
		)

		ginkgo.DescribeTable("[mixed networks] should perform proper OVN transactions when namespace and pod is created after node egress label switch",
			func(interconnect bool, controllerZone string) {
				app.Action = func(*cli.Context) error {
					egressIPOVN := "192.168.126.190"
					egressIPSecondaryHost := "10.10.10.20"
					fakeOvn.nodeName = controllerZone
					node1Zone := node1Name
					node2Zone := node2Name
					node1Local := controllerZone == node1Zone
					node2Local := controllerZone == node2Zone
					node1IPv4 := "192.168.126.202"
					node1IPv4OVN := node1IPv4 + "/24"
					node1IPv4SecondaryHost1 := "10.10.10.4/24"
					node1IPv4SecondaryHost2 := "5.5.5.10/24"
					node1IPv4TranSwitchIP := "100.88.0.2/16"
					node2IPv4OVN := "192.168.126.51/24"
					node2IPv4SecondaryHost1 := "10.10.10.5/24"
					node2IPv4SecondaryHost2 := "7.7.7.9/16"
					node2IPv4TranSwitchIP := "100.88.0.3/16"
					_, node1Subnet, _ := net.ParseCIDR(v4Node1Subnet)
					_, node2Subnet, _ := net.ParseCIDR(v4Node2Subnet)

					node1IPv4Addresses := []string{node1IPv4OVN, node1IPv4SecondaryHost1, node1IPv4SecondaryHost2}
					node2IPv4Addresses := []string{node2IPv4OVN, node2IPv4SecondaryHost1, node2IPv4SecondaryHost2}

					nodes := getIPv4Nodes([]nodeInfo{{node1IPv4Addresses, node1IPv4TranSwitchIP},
						{node2IPv4Addresses, node2IPv4TranSwitchIP}})

					node1 := nodes[0]
					node1.Labels = map[string]string{
						"k8s.ovn.org/egress-assignable": "",
					}
					node2 := nodes[1]
					egressNamespace := newUDNNamespace(eipNamespace)
					egressNamespace2 := newUDNNamespace(eipNamespace2)
					egressPod1Node1 := *ovntest.NewPodWithLabels(eipNamespace, podName, node1Name, podV4IP, egressPodLabel)
					egressPod2Node1 := *ovntest.NewPodWithLabels(eipNamespace2, podName, node1Name, podV4IP2, egressPodLabel)
					egressPod3Node2 := *ovntest.NewPodWithLabels(eipNamespace, podName2, node2Name, podV4IP3, egressPodLabel)
					egressPod4Node2 := *ovntest.NewPodWithLabels(eipNamespace2, podName2, node2Name, podV4IP4, egressPodLabel)

					eIPOVN := egressipv1.EgressIP{
						ObjectMeta: newEgressIPMeta(egressIPName),
						Spec: egressipv1.EgressIPSpec{
							EgressIPs: []string{egressIPOVN},
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

					eIPSecondaryHost := egressipv1.EgressIP{
						ObjectMeta: newEgressIPMeta(egressIP2Name),
						Spec: egressipv1.EgressIPSpec{
							EgressIPs: []string{egressIPSecondaryHost},
							PodSelector: metav1.LabelSelector{
								MatchLabels: egressPodLabel,
							},
							NamespaceSelector: metav1.LabelSelector{
								MatchLabels: map[string]string{
									"name": egressNamespace2.Name,
								},
							},
						},
						Status: egressipv1.EgressIPStatus{
							Items: []egressipv1.EgressIPStatusItem{},
						},
					}
					node1Switch := &nbdb.LogicalSwitch{
						UUID:  node1.Name + "-UUID",
						Name:  node1.Name,
						Ports: []string{"k8s-" + node1.Name + "-UUID"},
					}
					node2Switch := &nbdb.LogicalSwitch{
						UUID:  node2.Name + "-UUID",
						Name:  node2.Name,
						Ports: []string{"k8s-" + node2.Name + "-UUID"},
					}
					dynamicNeighRouters := "false"

					logicalRouterOptions := map[string]string{
						"dynamic_neigh_routers": dynamicNeighRouters,
					}
					fakeOvn.startWithDBSetup(
						libovsdbtest.TestSetup{
							NBData: []libovsdbtest.TestData{
								&nbdb.LogicalRouterPort{
									UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID",
									Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name,
									Networks: []string{nodeLogicalRouterIfAddrV4},
								},
								&nbdb.LogicalRouterPort{
									UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID",
									Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name,
									Networks: []string{node2LogicalRouterIfAddrV4},
								},
								&nbdb.LogicalRouter{
									Name: types.OVNClusterRouter,
									UUID: types.OVNClusterRouter + "-UUID",
								},
								&nbdb.LogicalRouter{
									Name:    types.GWRouterPrefix + node1.Name,
									UUID:    types.GWRouterPrefix + node1.Name + "-UUID",
									Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID"},
									Options: logicalRouterOptions,
								},
								&nbdb.LogicalRouter{
									Name:    types.GWRouterPrefix + node2.Name,
									UUID:    types.GWRouterPrefix + node2.Name + "-UUID",
									Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID"},
									Nat:     nil,
									Options: logicalRouterOptions,
								},
								&nbdb.LogicalSwitchPort{
									UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID",
									Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
									Type: "router",
									Options: map[string]string{
										libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
										"nat-addresses":             "router",
										"exclude-lb-vips-from-garp": "true",
									},
								},
								&nbdb.LogicalSwitchPort{
									UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID",
									Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
									Type: "router",
									Options: map[string]string{
										libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
										"nat-addresses":             "router",
										"exclude-lb-vips-from-garp": "true",
									},
								},
								&nbdb.LogicalSwitchPort{
									UUID:      "k8s-" + node1.Name + "-UUID",
									Name:      "k8s-" + node1.Name,
									Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1Subnet).IP.String()},
								},
								&nbdb.LogicalSwitchPort{
									UUID:      "k8s-" + node2.Name + "-UUID",
									Name:      "k8s-" + node2.Name,
									Addresses: []string{"fe:1a:c2:3f:0e:fb " + util.GetNodeManagementIfAddr(node2Subnet).IP.String()},
								},
								&nbdb.LogicalSwitch{
									UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
									Name:  types.ExternalSwitchPrefix + node1Name,
									Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
								},
								&nbdb.LogicalSwitch{
									UUID:  types.ExternalSwitchPrefix + node2Name + "-UUID",
									Name:  types.ExternalSwitchPrefix + node2Name,
									Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
								},
								node1Switch,
								node2Switch,
							},
						},
						&egressipv1.EgressIPList{
							Items: []egressipv1.EgressIP{eIPOVN, eIPSecondaryHost},
						},
						&corev1.NodeList{
							Items: []corev1.Node{node1, node2},
						},
						&corev1.NamespaceList{
							Items: []corev1.Namespace{*egressNamespace, *egressNamespace2},
						},
						&corev1.PodList{
							Items: []corev1.Pod{egressPod1Node1, egressPod2Node1, egressPod3Node2, egressPod4Node2},
						})

					for _, p := range []struct {
						corev1.Pod
						podIP string
					}{
						{
							egressPod1Node1,
							podV4IP,
						},
						{
							egressPod2Node1,
							podV4IP2,
						},
						{
							egressPod3Node2,
							podV4IP3,
						},
						{
							egressPod4Node2,
							podV4IP4,
						},
					} {
						i, n, err := net.ParseCIDR(p.podIP + "/23")
						gomega.Expect(err).NotTo(gomega.HaveOccurred())
						n.IP = i
						fakeOvn.controller.logicalPortCache.add(&p.Pod, "", types.DefaultNetworkName, "", nil, []*net.IPNet{n})
					}
					err := fakeOvn.controller.WatchEgressIPPods()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchEgressIPNamespaces()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchEgressNodes()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					fakeOvn.patchEgressIPObj(node1Name, egressIPName, egressIPOVN)

					err = fakeOvn.controller.WatchEgressIP()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
					egressIPs, eIPNodes := getEgressIPStatus(egressIPName)
					gomega.Expect(eIPNodes[0]).To(gomega.Equal(node1.Name))
					gomega.Expect(egressIPs[0]).To(gomega.Equal(egressIPOVN))
					time.Sleep(20 * time.Millisecond)
					fakeOvn.patchEgressIPObj(node1Name, egressIP2Name, egressIPSecondaryHost)
					gomega.Eventually(getEgressIPStatusLen(egressIP2Name)).Should(gomega.Equal(1))
					gomega.Eventually(nodeSwitch).Should(gomega.Equal(node1.Name))
					egressIPs, _ = getEgressIPStatus(egressIP2Name)
					gomega.Expect(egressIPs[0]).To(gomega.Equal(egressIPSecondaryHost))
					time.Sleep(20 * time.Millisecond)
					nodeMgntIP, err := getSwitchManagementPortIP(&node1)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					egressPod1Node1Reroute := nodeLogicalRouterIPv4
					egressPod2Node1Reroute := []string{nodeMgntIP.To4().String()}
					egressPod3Node2Reroute := nodeLogicalRouterIPv4
					egressPod4Node2Reroute := []string{nodeMgntIP.To4().String()}

					if interconnect && node1Zone != node2Zone && !node1Local {
						//todo fix me
						egressPod3Node2Reroute = []string{"100.88.0.2"} // node1's transit switch portIP
						egressPod4Node2Reroute = []string{"100.88.0.2"} // node1's transit switch portIP
					}
					lrps := make([]*nbdb.LogicalRouterPolicy, 0)

					if !interconnect {
						lrps = append(lrps, getReRoutePolicy(egressPod1Node1.Status.PodIP, "4", "reroute-UUID", egressPod1Node1Reroute,
							getEgressIPLRPReRouteDbIDs(eIPOVN.Name, egressPod1Node1.Namespace, egressPod1Node1.Name, IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs()),
							getReRoutePolicySecondaryHost(egressPod2Node1.Status.PodIP, "4", "reroute-UUID2", egressPod2Node1Reroute,
								getEgressIPLRPReRouteDbIDs(eIPSecondaryHost.Name, egressPod2Node1.Namespace, egressPod2Node1.Name, IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs()),
							getReRoutePolicy(egressPod3Node2.Status.PodIP, "4", "reroute-UUID3", egressPod3Node2Reroute,
								getEgressIPLRPReRouteDbIDs(eIPOVN.Name, egressPod3Node2.Namespace, egressPod3Node2.Name, IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs()),
							getReRoutePolicySecondaryHost(egressPod4Node2.Status.PodIP, "4", "reroute-UUID4", egressPod4Node2Reroute,
								getEgressIPLRPReRouteDbIDs(eIPSecondaryHost.Name, egressPod4Node2.Namespace, egressPod4Node2.Name, IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs()))
					}

					if interconnect && node1Local && !node2Local {
						lrps = append(lrps, getReRoutePolicy(egressPod1Node1.Status.PodIP, "4", "reroute-UUID", egressPod1Node1Reroute,
							getEgressIPLRPReRouteDbIDs(eIPOVN.Name, egressPod1Node1.Namespace, egressPod1Node1.Name, IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs()),
							getReRoutePolicySecondaryHost(egressPod2Node1.Status.PodIP, "4", "reroute-UUID2", egressPod2Node1Reroute,
								getEgressIPLRPReRouteDbIDs(eIPSecondaryHost.Name, egressPod2Node1.Namespace, egressPod2Node1.Name, IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs()),
							getReRoutePolicySecondaryHost(podV4IP4, "4", "egressip-pod4node2", egressPod4Node2Reroute,
								getEgressIPLRPReRouteDbIDs(eIPSecondaryHost.Name, egressPod4Node2.Namespace, egressPod4Node2.Name, IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs()))
					}

					if interconnect && !node1Local && node2Local {
						lrps = append(lrps,
							getReRoutePolicy(egressPod3Node2.Status.PodIP, "4", "reroute-UUID", egressPod3Node2Reroute,
								getEgressIPLRPReRouteDbIDs(eIPOVN.Name, egressPod3Node2.Namespace, egressPod3Node2.Name, IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs()),
							getReRoutePolicy(egressPod4Node2.Status.PodIP, "4", "reroute-UUID2", egressPod4Node2Reroute,
								getEgressIPLRPReRouteDbIDs(eIPSecondaryHost.Name, egressPod4Node2.Namespace, egressPod4Node2.Name, IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs()))
					}
					ovnCRPolicies := []string{"no-reroute-node-UUID", "default-no-reroute-UUID", "no-reroute-service-UUID", "default-no-reroute-reply-traffic"}
					for _, lrp := range lrps {
						ovnCRPolicies = append(ovnCRPolicies, lrp.UUID)
					}
					nodeName := "k8s-node1"
					egressSVCServedPodsASv4, _ := buildEgressServiceAddressSets(nil)
					servedPodIPs := []string{}
					if node1Local {
						servedPodIPs = append(servedPodIPs, podV4IP, podV4IP2)
					}
					if node2Local {
						servedPodIPs = append(servedPodIPs, podV4IP3, podV4IP4)
					}
					egressIPServedPodsASv4, _ := buildEgressIPServedPodsAddressSets(servedPodIPs, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName)

					ipNets, _ := util.ParseIPNets(append(node1IPv4Addresses, node2IPv4Addresses...))
					egressNodeIPs := []string{}
					for _, ipNet := range ipNets {
						egressNodeIPs = append(egressNodeIPs, ipNet.IP.String())
					}
					egressNodeIPsASv4, _ := buildEgressIPNodeAddressSets(egressNodeIPs)
					expectedDatabaseState := []libovsdbtest.TestData{
						&nbdb.LogicalRouterPolicy{
							Priority:    types.DefaultNoRereoutePriority,
							Match:       "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
							Action:      nbdb.LogicalRouterPolicyActionAllow,
							UUID:        "default-no-reroute-UUID",
							ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
						},
						&nbdb.LogicalRouterPolicy{
							Priority:    types.DefaultNoRereoutePriority,
							Match:       fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
							Action:      nbdb.LogicalRouterPolicyActionAllow,
							UUID:        "no-reroute-service-UUID",
							ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
						},
						&nbdb.LogicalRouterPolicy{
							Priority:    types.DefaultNoRereoutePriority,
							Match:       "(ip4.src == $a8519615025667110816 || ip4.src == $a13607449821398607916) && ip4.dst == $a1042611113178530741",
							Action:      nbdb.LogicalRouterPolicyActionAllow,
							Options:     map[string]string{"pkt_mark": types.EgressIPNodeConnectionMark},
							UUID:        "no-reroute-node-UUID",
							ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
						},
						&nbdb.LogicalRouter{
							Name:    types.GWRouterPrefix + node1.Name,
							UUID:    types.GWRouterPrefix + node1.Name + "-UUID",
							Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID"},
							Nat:     []string{},
							Options: logicalRouterOptions,
						},
						&nbdb.LogicalRouter{
							Name:    types.GWRouterPrefix + node2.Name,
							UUID:    types.GWRouterPrefix + node2.Name + "-UUID",
							Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID"},
							Options: logicalRouterOptions,
						},
						&nbdb.LogicalRouter{
							Name:         types.OVNClusterRouter,
							UUID:         types.OVNClusterRouter + "-UUID",
							Policies:     ovnCRPolicies,
							StaticRoutes: []string{}, // if needed to be populated, will be done further down
						},
						&nbdb.LogicalRouterPort{
							UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID",
							Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name,
							Networks: []string{nodeLogicalRouterIfAddrV4},
						},
						&nbdb.LogicalRouterPort{
							UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID",
							Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name,
							Networks: []string{node2LogicalRouterIfAddrV4},
						},
						&nbdb.LogicalSwitchPort{
							UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID",
							Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
							Type: "router",
							Options: map[string]string{
								libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
								"nat-addresses":             "router",
								"exclude-lb-vips-from-garp": "true",
							},
						},
						&nbdb.LogicalSwitchPort{
							UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID",
							Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
							Type: "router",
							Options: map[string]string{
								libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
								"nat-addresses":             "router",
								"exclude-lb-vips-from-garp": "true",
							},
						},
						&nbdb.LogicalSwitchPort{
							UUID:      "k8s-" + node1.Name + "-UUID",
							Name:      "k8s-" + node1.Name,
							Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1Subnet).IP.String()},
						},
						&nbdb.LogicalSwitchPort{
							UUID:      "k8s-" + node2.Name + "-UUID",
							Name:      "k8s-" + node2.Name,
							Addresses: []string{"fe:1a:c2:3f:0e:fb " + util.GetNodeManagementIfAddr(node2Subnet).IP.String()},
						},
						&nbdb.LogicalSwitch{
							UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
							Name:  types.ExternalSwitchPrefix + node1Name,
							Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
						},
						&nbdb.LogicalSwitch{
							UUID:  types.ExternalSwitchPrefix + node2Name + "-UUID",
							Name:  types.ExternalSwitchPrefix + node2Name,
							Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
						},
						getNoReRouteReplyTrafficPolicy(types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
						node1Switch,
						node2Switch,
						getDefaultQoSRule(false, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
						egressSVCServedPodsASv4,
						egressIPServedPodsASv4,
						egressNodeIPsASv4,
					}
					if node1Local {
						// QoS rules is configured only for nodes in local zones; the controller in the remote zone will do it for the remote nodes
						node1Switch.QOSRules = []string{"default-QoS-UUID"}
						expectedDatabaseState[3].(*nbdb.LogicalRouter).Nat = append(expectedDatabaseState[3].(*nbdb.LogicalRouter).Nat, "egressip-nat-UUID", "egressip2-nat-UUID")
						expectedDatabaseState = append(expectedDatabaseState, &nbdb.NAT{
							UUID:        "egressip-nat-UUID",
							LogicalIP:   podV4IP,
							ExternalIP:  egressIPOVN,
							ExternalIDs: getEgressIPNATDbIDs(egressIPName, egressPod1Node1.Namespace, egressPod1Node1.Name, IPFamilyValueV4, fakeOvn.controller.controllerName).GetExternalIDs(),
							Type:        nbdb.NATTypeSNAT,
							LogicalPort: &nodeName,
							Options: map[string]string{
								"stateless": "false",
							},
						}, &nbdb.NAT{
							UUID:        "egressip2-nat-UUID",
							LogicalIP:   podV4IP3,
							ExternalIP:  egressIPOVN,
							ExternalIDs: getEgressIPNATDbIDs(egressIPName, egressPod3Node2.Namespace, egressPod3Node2.Name, IPFamilyValueV4, fakeOvn.controller.controllerName).GetExternalIDs(),
							Type:        nbdb.NATTypeSNAT,
							LogicalPort: &nodeName,
							Options: map[string]string{
								"stateless": "false",
							},
						})
					}

					if node2Local {
						// add QoS rules config only if node is in local zone
						node2Switch.QOSRules = []string{"default-QoS-UUID"}
					}

					for _, lrp := range lrps {
						expectedDatabaseState = append(expectedDatabaseState, lrp)
					}

					gomega.Eventually(fakeOvn.nbClient, inspectTimeout).Should(libovsdbtest.HaveData(expectedDatabaseState))

					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			},
			// will showcase localzone setup - the controller for the pod's zone is local, where pod's reroute policy towards egressNode will be done.
			// NOTE: SNAT won't be visible because its in remote zone
			ginkgo.Entry("interconnect enabled; node1 in local and node2 in remote zones", true, node1Name),
			// will showcase localzone setup - the controller for the egress node's zone is local, where pod's SNAT policy and static route will be done.
			// NOTE: reroute policy won't be visible because its in remote zone (pod is in remote zone)
			ginkgo.Entry("interconnect enabled; node1 in remote and node2 in local zones", true, node2Name),
		)

	})

	ginkgo.Context("On node DELETE", func() {

		ginkgo.DescribeTable("should perform proper OVN transactions when node's gateway objects are already deleted",
			func(controllerZone string) {
				app.Action = func(*cli.Context) error {

					egressIP := "192.168.126.101"
					fakeOvn.nodeName = controllerZone
					node1Zone := node1Name
					node2Zone := node2Name
					node1Local := controllerZone == node1Zone
					node2Local := controllerZone == node2Zone
					node1IPv4 := "192.168.126.202/24"
					node2IPv4 := "192.168.126.51/24"
					_, node1Subnet, _ := net.ParseCIDR(v4Node1Subnet)
					_, node2Subnet, _ := net.ParseCIDR(v4Node2Subnet)

					egressPod := *ovntest.NewPodWithLabels(eipNamespace, podName, node1Name, podV4IP, egressPodLabel)
					egressNamespace := ovntest.NewNamespace(eipNamespace)
					annotations := map[string]string{
						"k8s.ovn.org/node-primary-ifaddr":             fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4, ""),
						"k8s.ovn.org/node-subnets":                    fmt.Sprintf("{\"default\":\"%s\"}", v4Node1Subnet),
						"k8s.ovn.org/node-transit-switch-port-ifaddr": "{\"ipv4\":\"100.88.0.2/16\"}", // used only for ic=true test
						util.OVNNodeHostCIDRs:                         fmt.Sprintf("[\"%s\"]", node1IPv4),
					}
					labels := map[string]string{
						"k8s.ovn.org/egress-assignable": "",
					}
					node1 := getNodeObj(node1Name, annotations, labels)
					annotations = map[string]string{
						"k8s.ovn.org/node-primary-ifaddr":             fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node2IPv4, ""),
						"k8s.ovn.org/node-subnets":                    fmt.Sprintf("{\"default\":\"%s\"}", v4Node1Subnet),
						"k8s.ovn.org/node-transit-switch-port-ifaddr": "{\"ipv4\":\"100.88.0.3/16\"}", // used only for ic=true test
						util.OVNNodeHostCIDRs:                         fmt.Sprintf("[\"%s\"]", node2IPv4),
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
					node1Switch := &nbdb.LogicalSwitch{
						UUID:  node1.Name + "-UUID",
						Name:  node1.Name,
						Ports: []string{"k8s-" + node1.Name + "-UUID"},
					}
					node2Switch := &nbdb.LogicalSwitch{
						UUID:  node2.Name + "-UUID",
						Name:  node2.Name,
						Ports: []string{"k8s-" + node2.Name + "-UUID"},
					}
					dynamicNeighRouters := "false"

					logicalRouterOptions := map[string]string{
						"dynamic_neigh_routers": dynamicNeighRouters,
					}

					fakeOvn.startWithDBSetup(
						libovsdbtest.TestSetup{
							NBData: []libovsdbtest.TestData{
								&nbdb.LogicalRouterPort{
									UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID",
									Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name,
									Networks: []string{nodeLogicalRouterIfAddrV4},
								},
								&nbdb.LogicalRouterPort{
									UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID",
									Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name,
									Networks: []string{node2LogicalRouterIfAddrV4},
								},
								&nbdb.LogicalRouter{
									Name: types.OVNClusterRouter,
									UUID: types.OVNClusterRouter + "-UUID",
								},
								&nbdb.LogicalRouter{
									Name:    types.GWRouterPrefix + node1.Name,
									UUID:    types.GWRouterPrefix + node1.Name + "-UUID",
									Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID"},
									Options: logicalRouterOptions,
								},
								&nbdb.LogicalSwitch{
									UUID:  types.ExternalSwitchPrefix + node1.Name + "-UUID",
									Name:  types.ExternalSwitchPrefix + node1.Name,
									Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
								},
								&nbdb.LogicalRouter{
									Name:    types.GWRouterPrefix + node2.Name,
									UUID:    types.GWRouterPrefix + node2.Name + "-UUID",
									Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID"},
									Options: logicalRouterOptions,
								},
								&nbdb.LogicalSwitch{
									UUID:  types.ExternalSwitchPrefix + node2.Name + "-UUID",
									Name:  types.ExternalSwitchPrefix + node2.Name,
									Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
								},
								&nbdb.LogicalSwitchPort{
									UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID",
									Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
									Type: "router",
									Options: map[string]string{
										libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
										"nat-addresses":             "router",
										"exclude-lb-vips-from-garp": "true",
									},
								},
								&nbdb.LogicalSwitchPort{
									UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID",
									Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
									Type: "router",
									Options: map[string]string{
										libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
										"nat-addresses":             "router",
										"exclude-lb-vips-from-garp": "true",
									},
								},
								&nbdb.LogicalSwitch{
									UUID: types.OVNJoinSwitch + "-UUID",
									Name: types.OVNJoinSwitch,
								},
								&nbdb.LogicalSwitchPort{
									UUID:      "k8s-" + node1.Name + "-UUID",
									Name:      "k8s-" + node1.Name,
									Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1Subnet).IP.String()},
								},
								&nbdb.LogicalSwitchPort{
									UUID:      "k8s-" + node2.Name + "-UUID",
									Name:      "k8s-" + node2.Name,
									Addresses: []string{"fe:1a:c2:3f:0e:fb " + util.GetNodeManagementIfAddr(node2Subnet).IP.String()},
								},
								node1Switch,
								node2Switch,
							},
						},
						&egressipv1.EgressIPList{
							Items: []egressipv1.EgressIP{eIP},
						},
						&corev1.NodeList{
							Items: []corev1.Node{node1, node2},
						},
						&corev1.NamespaceList{
							Items: []corev1.Namespace{*egressNamespace},
						})

					err := fakeOvn.controller.WatchEgressIPPods()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchEgressIPNamespaces()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchEgressNodes()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchEgressIP()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					fakeOvn.patchEgressIPObj(node1Name, egressIPName, egressIP)

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
					primarySNAT := getEIPSNAT(podV4IP, egressPod.Namespace, egressPod.Name, egressIP, expectedNatLogicalPort, types.DefaultNetworkControllerName)
					primarySNAT.UUID = "egressip-nat1-UUID"
					egressSVCServedPodsASv4, _ := buildEgressServiceAddressSets(nil)
					servedPodIPs := []string{podV4IP}
					// pod is located on node1, therefore if remote, we don't expect to see its IPs in the served pods address set
					if !node1Local {
						servedPodIPs = nil
					}
					egressIPServedPodsASv4, _ := buildEgressIPServedPodsAddressSets(servedPodIPs, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName)
					ipNets, _ := util.ParseIPNets([]string{node1IPv4, node2IPv4})

					egressNodeIPs := []string{}
					for _, ipNet := range ipNets {
						egressNodeIPs = append(egressNodeIPs, ipNet.IP.String())
					}
					egressNodeIPsASv4, _ := buildEgressIPNodeAddressSets(egressNodeIPs)
					expectedDatabaseState := []libovsdbtest.TestData{
						primarySNAT,
						&nbdb.LogicalRouterPolicy{
							Priority:    types.DefaultNoRereoutePriority,
							Match:       "(ip4.src == $a8519615025667110816 || ip4.src == $a13607449821398607916) && ip4.dst == $a1042611113178530741",
							Action:      nbdb.LogicalRouterPolicyActionAllow,
							Options:     map[string]string{"pkt_mark": types.EgressIPNodeConnectionMark},
							UUID:        "no-reroute-node-UUID",
							ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
						},
						&nbdb.LogicalRouterPolicy{
							Priority:    types.DefaultNoRereoutePriority,
							Match:       "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
							Action:      nbdb.LogicalRouterPolicyActionAllow,
							UUID:        "default-no-reroute-UUID",
							ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
						},
						&nbdb.LogicalRouterPolicy{
							Priority:    types.DefaultNoRereoutePriority,
							Match:       fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
							Action:      nbdb.LogicalRouterPolicyActionAllow,
							UUID:        "no-reroute-service-UUID",
							ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
						},
						&nbdb.LogicalRouter{
							Name:    types.GWRouterPrefix + node1.Name,
							UUID:    types.GWRouterPrefix + node1.Name + "-UUID",
							Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID"},
							Nat:     []string{"egressip-nat1-UUID"},
							Options: logicalRouterOptions,
						},
						&nbdb.LogicalRouter{
							Name:    types.GWRouterPrefix + node2.Name,
							UUID:    types.GWRouterPrefix + node2.Name + "-UUID",
							Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID"},
							Nat:     []string{},
							Options: logicalRouterOptions,
						},
						&nbdb.LogicalRouter{
							Name:     types.OVNClusterRouter,
							UUID:     types.OVNClusterRouter + "-UUID",
							Policies: []string{"default-no-reroute-UUID", "no-reroute-service-UUID", "no-reroute-node-UUID", "default-no-reroute-reply-traffic"},
						},
						&nbdb.LogicalRouterPort{
							UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID",
							Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name,
							Networks: []string{nodeLogicalRouterIfAddrV4},
						},
						&nbdb.LogicalRouterPort{
							UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID",
							Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name,
							Networks: []string{node2LogicalRouterIfAddrV4},
						},
						&nbdb.LogicalSwitchPort{
							UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID",
							Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
							Type: "router",
							Options: map[string]string{
								libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
								"nat-addresses":             "router",
								"exclude-lb-vips-from-garp": "true",
							},
						},
						&nbdb.LogicalSwitchPort{
							UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID",
							Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
							Type: "router",
							Options: map[string]string{
								libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
								"nat-addresses":             "router",
								"exclude-lb-vips-from-garp": "true",
							},
						},
						&nbdb.LogicalSwitch{
							UUID: types.OVNJoinSwitch + "-UUID",
							Name: types.OVNJoinSwitch,
						},
						&nbdb.LogicalSwitch{
							UUID:  types.ExternalSwitchPrefix + node1.Name + "-UUID",
							Name:  types.ExternalSwitchPrefix + node1.Name,
							Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
						},
						&nbdb.LogicalSwitch{
							UUID:  types.ExternalSwitchPrefix + node2.Name + "-UUID",
							Name:  types.ExternalSwitchPrefix + node2.Name,
							Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
						},
						&nbdb.LogicalSwitchPort{
							UUID:      "k8s-" + node1.Name + "-UUID",
							Name:      "k8s-" + node1.Name,
							Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1Subnet).IP.String()},
						},
						&nbdb.LogicalSwitchPort{
							UUID:      "k8s-" + node2.Name + "-UUID",
							Name:      "k8s-" + node2.Name,
							Addresses: []string{"fe:1a:c2:3f:0e:fb " + util.GetNodeManagementIfAddr(node2Subnet).IP.String()},
						},
						getNoReRouteReplyTrafficPolicy(types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
						node1Switch,
						node2Switch,
						getDefaultQoSRule(false, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
						egressSVCServedPodsASv4,
						egressIPServedPodsASv4,
						egressNodeIPsASv4,
					}
					if node2Local {
						// add QoS rules only if node is in local zone
						node2Switch.QOSRules = []string{"default-QoS-UUID"}
					}
					if node1Local {
						expectedDatabaseState = append(expectedDatabaseState, getReRoutePolicy(egressPod.Status.PodIP, "4", "reroute-UUID",
							nodeLogicalRouterIPv4, getEgressIPLRPReRouteDbIDs(eIP.Name, egressPod.Namespace, egressPod.Name, IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs()))
						expectedDatabaseState[6].(*nbdb.LogicalRouter).Policies = append(expectedDatabaseState[6].(*nbdb.LogicalRouter).Policies, "reroute-UUID")
						node1Switch.QOSRules = []string{"default-QoS-UUID"}
					} else {
						// if node1 where the pod lives is remote we can't see the EIP setup done since the local controller is in the local zone
						expectedDatabaseState[4].(*nbdb.LogicalRouter).Nat = []string{}
						expectedDatabaseState[6].(*nbdb.LogicalRouter).Policies = []string{"no-reroute-node-UUID", "default-no-reroute-UUID",
							"no-reroute-service-UUID", "default-no-reroute-reply-traffic"}
						expectedDatabaseState = expectedDatabaseState[1:]
					}
					gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

					err = fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Delete(context.TODO(), node1Name, metav1.DeleteOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					err = newGatewayManager(fakeOvn, node1Name).Cleanup() // simulate an already deleted node
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					// NOTE: This test checks if plumbing is removed when node is gone but pod on the node is still present (unusual scenario)
					// Thus we need to check the cache state to verify things in unit tests to avoid races - we don't control the order of
					// node deletion removing the entry from nodeState versus the add happening for the pod.
					// (in real env this won't be a problem since eventually things will reconcile as pod will also be gone if node is gone)
					gomega.Eventually(func() bool {
						_, ok := fakeOvn.controller.eIPC.nodeState.Load(egressPod.Spec.NodeName)
						return ok
					}).Should(gomega.BeFalse())

					// W0608 12:53:33.728205 1161455 base_network_controller_egressip.go:2030] Unable to retrieve gateway IP for node: node1, protocol is IPv6: false, err: attempt at finding node gateway router network information failed, err: unable to find router port rtoj-GR_node1: object not found
					// 2023-04-25T11:01:13.2804834Z W0425 11:01:13.280407   21055 base_network_controller_egressip.go:2036] Unable to fetch transit switch IP for node: node1: err: failed to get node node1: node "node1" not found
					fakeOvn.patchEgressIPObj(node2Name, egressIPName, egressIP)
					gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
					gomega.Eventually(nodeSwitch).Should(gomega.Equal(node2.Name)) // egressIP successfully reassigned to node2
					egressIPs, _ = getEgressIPStatus(egressIPName)
					gomega.Expect(egressIPs[0]).To(gomega.Equal(egressIP))

					expectedNatLogicalPort = "k8s-node2"
					eipSNAT := getEIPSNAT(podV4IP, egressPod.Namespace, egressPod.Name, egressIP, expectedNatLogicalPort, types.DefaultNetworkControllerName)
					egressSVCServedPodsASv4, _ = buildEgressServiceAddressSets(nil)
					egressIPServedPodsASv4, _ = buildEgressIPServedPodsAddressSets(servedPodIPs, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName)
					ip, _, _ := net.ParseCIDR(node2IPv4)

					egressNodeIPsASv4, _ = buildEgressIPNodeAddressSets([]string{ip.String()})
					expectedDatabaseState = []libovsdbtest.TestData{
						getReRoutePolicy(egressPod.Status.PodIP, "4", "reroute-UUID", node2LogicalRouterIPv4,
							getEgressIPLRPReRouteDbIDs(eIP.Name, egressPod.Namespace, egressPod.Name, IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs()),
						&nbdb.LogicalRouterPolicy{
							Priority:    types.DefaultNoRereoutePriority,
							Match:       "(ip4.src == $a8519615025667110816 || ip4.src == $a13607449821398607916) && ip4.dst == $a1042611113178530741",
							Action:      nbdb.LogicalRouterPolicyActionAllow,
							Options:     map[string]string{"pkt_mark": types.EgressIPNodeConnectionMark},
							UUID:        "no-reroute-node-UUID",
							ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
						},
						&nbdb.LogicalRouterPolicy{
							Priority:    types.DefaultNoRereoutePriority,
							Match:       "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
							Action:      nbdb.LogicalRouterPolicyActionAllow,
							UUID:        "default-no-reroute-UUID",
							ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
						},
						&nbdb.LogicalRouterPolicy{
							Priority:    types.DefaultNoRereoutePriority,
							Match:       fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
							Action:      nbdb.LogicalRouterPolicyActionAllow,
							UUID:        "no-reroute-service-UUID",
							ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
						},
						&nbdb.LogicalRouter{
							Name:    types.GWRouterPrefix + node2.Name,
							UUID:    types.GWRouterPrefix + node2.Name + "-UUID",
							Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID"},
							Options: logicalRouterOptions,
						},
						&nbdb.LogicalRouter{
							Name:     types.OVNClusterRouter,
							UUID:     types.OVNClusterRouter + "-UUID",
							Policies: []string{"reroute-UUID", "default-no-reroute-UUID", "no-reroute-service-UUID", "no-reroute-node-UUID"},
						},
						&nbdb.LogicalRouterPort{
							UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID",
							Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name,
							Networks: []string{node2LogicalRouterIfAddrV4},
						},
						&nbdb.LogicalSwitchPort{
							UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID",
							Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
							Type: "router",
							Options: map[string]string{
								libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
								"nat-addresses":             "router",
								"exclude-lb-vips-from-garp": "true",
							},
						},
						&nbdb.LogicalSwitch{
							UUID: types.OVNJoinSwitch + "-UUID",
							Name: types.OVNJoinSwitch,
						},
						&nbdb.LogicalSwitch{
							UUID:  types.ExternalSwitchPrefix + node2.Name + "-UUID",
							Name:  types.ExternalSwitchPrefix + node2.Name,
							Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
						},
						&nbdb.LogicalSwitchPort{
							UUID:      "k8s-" + node1.Name + "-UUID",
							Name:      "k8s-" + node1.Name,
							Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1Subnet).IP.String()},
						},
						&nbdb.LogicalSwitchPort{
							UUID:      "k8s-" + node2.Name + "-UUID",
							Name:      "k8s-" + node2.Name,
							Addresses: []string{"fe:1a:c2:3f:0e:fb " + util.GetNodeManagementIfAddr(node2Subnet).IP.String()},
						},
						getNoReRouteReplyTrafficPolicy(types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
						node1Switch,
						node2Switch,
						getDefaultQoSRule(false, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
						egressSVCServedPodsASv4,
						egressIPServedPodsASv4,
						egressNodeIPsASv4,
					}
					if node2Local {
						// either not interconnect or egressNode is in localZone
						expectedDatabaseState = append(expectedDatabaseState, eipSNAT)
						expectedDatabaseState[4].(*nbdb.LogicalRouter).Nat = []string{"egressip-nat-UUID"} // 4th item is node2's GR
					}
					// all cases: reroute logical router policy is gone and won't be recreated since node1 is deleted - that is where the pod lives
					// NOTE: This test is not really a real scenario, it depicts a transient state.
					expectedDatabaseState[5].(*nbdb.LogicalRouter).Policies = []string{"default-no-reroute-UUID", "no-reroute-service-UUID",
						"no-reroute-node-UUID", "default-no-reroute-reply-traffic"}
					expectedDatabaseState = expectedDatabaseState[1:]

					gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))
					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			},
			// will showcase localzone setup - the controller for the pod's zone is local, where pod's reroute policy towards egressNode will be done.
			// NOTE: SNAT won't be visible because its in remote zone
			ginkgo.Entry("node1 in local and node2 in remote zones", node1Name),
			// will showcase localzone setup - the controller for the egress node's zone is local, where pod's SNAT policy and static route will* be done.
			// * the static route won't be visible because the pod's node node1 is getting deleted in this test
			// NOTE: reroute policy won't be visible because its in remote zone (pod is in remote zone)
			ginkgo.Entry("node1 in remote and node2 in local zones", node2Name),
		)

		ginkgo.DescribeTable("[secondary host network] should perform proper OVN transactions when namespace and pod is created after node egress label switch",
			func(interconnect bool, controllerZone string) {
				app.Action = func(*cli.Context) error {
					egressIP := "10.10.10.10"
					fakeOvn.nodeName = controllerZone
					node1Zone := node1Name
					node2Zone := node2Name
					node1Local := controllerZone == node1Zone
					node2Local := controllerZone == node2Zone
					node1IPv4OVN := "192.168.126.202/24"
					node1IPv4SecondaryHost := "10.10.10.5/16"
					node1IPv4TranSwitchIP := "100.88.0.2/16"
					node2IPv4OVN := "192.168.126.51/24"
					node2IPv4TranSwitchIP := "100.88.0.3/16"
					_, node1Subnet, _ := net.ParseCIDR(v4Node1Subnet)
					_, node2Subnet, _ := net.ParseCIDR(v4Node2Subnet)

					egressPod := *ovntest.NewPodWithLabels(eipNamespace, podName, node1Name, podV4IP, egressPodLabel)
					egressNamespace := ovntest.NewNamespace(eipNamespace)

					node1IPv4Addresses := []string{node1IPv4OVN, node1IPv4SecondaryHost}
					node2IPv4Addresses := []string{node2IPv4OVN}

					nodes := getIPv4Nodes([]nodeInfo{{node1IPv4Addresses, node1IPv4TranSwitchIP},
						{node2IPv4Addresses, node2IPv4TranSwitchIP}})
					node1 := nodes[0]
					node1.Labels = map[string]string{
						"k8s.ovn.org/egress-assignable": "",
					}
					node2 := nodes[1]

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
						UUID:  node1.Name + "-UUID",
						Name:  node1.Name,
						Ports: []string{"k8s-" + node1.Name + "-UUID"},
					}
					node2Switch := &nbdb.LogicalSwitch{
						UUID:  node2.Name + "-UUID",
						Name:  node2.Name,
						Ports: []string{"k8s-" + node2.Name + "-UUID"},
					}
					dynamicNeighRouters := "false"

					logicalRouterOptions := map[string]string{
						"dynamic_neigh_routers": dynamicNeighRouters,
					}
					fakeOvn.startWithDBSetup(
						libovsdbtest.TestSetup{
							NBData: []libovsdbtest.TestData{
								&nbdb.LogicalRouterPort{
									UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID",
									Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name,
									Networks: []string{nodeLogicalRouterIfAddrV4},
								},
								&nbdb.LogicalRouterPort{
									UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID",
									Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name,
									Networks: []string{node2LogicalRouterIfAddrV4},
								},
								&nbdb.LogicalRouter{
									Name: types.OVNClusterRouter,
									UUID: types.OVNClusterRouter + "-UUID",
								},
								&nbdb.LogicalRouter{
									Name:    types.GWRouterPrefix + node1.Name,
									UUID:    types.GWRouterPrefix + node1.Name + "-UUID",
									Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID"},
									Options: logicalRouterOptions,
								},
								&nbdb.LogicalRouter{
									Name:    types.GWRouterPrefix + node2.Name,
									UUID:    types.GWRouterPrefix + node2.Name + "-UUID",
									Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID"},
									Nat:     nil,
									Options: logicalRouterOptions,
								},
								&nbdb.LogicalSwitchPort{
									UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID",
									Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
									Type: "router",
									Options: map[string]string{
										libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
										"nat-addresses":             "router",
										"exclude-lb-vips-from-garp": "true",
									},
								},
								&nbdb.LogicalSwitchPort{
									UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID",
									Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
									Type: "router",
									Options: map[string]string{
										libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
										"nat-addresses":             "router",
										"exclude-lb-vips-from-garp": "true",
									},
								},
								&nbdb.LogicalSwitchPort{
									UUID:      "k8s-" + node1.Name + "-UUID",
									Name:      "k8s-" + node1.Name,
									Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1Subnet).IP.String()},
								},
								&nbdb.LogicalSwitchPort{
									UUID:      "k8s-" + node2.Name + "-UUID",
									Name:      "k8s-" + node2.Name,
									Addresses: []string{"fe:1a:c2:3f:0e:fb " + util.GetNodeManagementIfAddr(node2Subnet).IP.String()},
								},
								&nbdb.LogicalSwitch{
									UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
									Name:  types.ExternalSwitchPrefix + node1Name,
									Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
								},
								&nbdb.LogicalSwitch{
									UUID:  types.ExternalSwitchPrefix + node2Name + "-UUID",
									Name:  types.ExternalSwitchPrefix + node2Name,
									Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
								},
								node1Switch,
								node2Switch,
							},
						},
						&egressipv1.EgressIPList{
							Items: []egressipv1.EgressIP{eIP},
						},
						&corev1.NodeList{
							Items: []corev1.Node{node1, node2},
						})

					i, n, _ := net.ParseCIDR(podV4IP + "/23")
					n.IP = i
					fakeOvn.controller.logicalPortCache.add(&egressPod, "", types.DefaultNetworkName, "", nil, []*net.IPNet{n})

					err := fakeOvn.controller.WatchEgressIPPods()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchEgressIPNamespaces()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchEgressNodes()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					fakeOvn.patchEgressIPObj(node1Name, egressIPName, egressIP)

					err = fakeOvn.controller.WatchEgressIP()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
					egressIPs, eIPNodes := getEgressIPStatus(egressIPName)
					gomega.Expect(eIPNodes[0]).To(gomega.Equal(node1.Name))
					gomega.Expect(egressIPs[0]).To(gomega.Equal(egressIP))

					node1.Labels = map[string]string{}
					node2.Labels = map[string]string{
						"k8s.ovn.org/egress-assignable": "",
					}

					_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), &node1, metav1.UpdateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), &node2, metav1.UpdateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					fakeOvn.patchEgressIPObj(node2Name, egressIPName, egressIP)
					gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
					gomega.Eventually(nodeSwitch).Should(gomega.Equal(node2.Name))
					egressIPs, _ = getEgressIPStatus(egressIPName)
					gomega.Expect(egressIPs[0]).To(gomega.Equal(egressIP))

					_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Create(context.TODO(), egressNamespace, metav1.CreateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(egressPod.Namespace).Create(context.TODO(), &egressPod, metav1.CreateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					node2MgntIP, err := getSwitchManagementPortIP(&node2)
					gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
					//expectedNatLogicalPort := "k8s-node2"
					reroutePolicyNextHop := []string{node2MgntIP.To4().String()}
					if interconnect && node1Zone != node2Zone && !node2Local {
						reroutePolicyNextHop = []string{"100.88.0.3"} // node2's transit switch portIP
					}
					egressSVCServedPodsASv4, _ := buildEgressServiceAddressSets(nil)
					servedPodIPs := []string{podV4IP}
					// pod is located on node1, therefore if remote, we dont expect to see its IPs in the served pods address set
					if !node1Local {
						servedPodIPs = nil
					}
					egressIPServedPodsASv4, _ := buildEgressIPServedPodsAddressSets(servedPodIPs, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName)
					ipNets, _ := util.ParseIPNets(append(node1IPv4Addresses, node2IPv4OVN))
					egressNodeIPs := []string{}
					for _, ipNet := range ipNets {
						egressNodeIPs = append(egressNodeIPs, ipNet.IP.String())
					}
					egressNodeIPsASv4, _ := buildEgressIPNodeAddressSets(egressNodeIPs)
					var reroutePolicy *nbdb.LogicalRouterPolicy
					if !node2Local {
						reroutePolicy = getReRoutePolicy(egressPod.Status.PodIP, "4", "reroute-UUID", reroutePolicyNextHop,
							getEgressIPLRPReRouteDbIDs(eIP.Name, egressPod.Namespace, egressPod.Name, IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs())
					} else {
						reroutePolicy = getReRoutePolicySecondaryHost(egressPod.Status.PodIP, "4", "reroute-UUID", reroutePolicyNextHop,
							getEgressIPLRPReRouteDbIDs(eIP.Name, egressPod.Namespace, egressPod.Name, IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs())
					}
					expectedDatabaseState := []libovsdbtest.TestData{
						reroutePolicy,
						&nbdb.LogicalRouterPolicy{
							Priority:    types.DefaultNoRereoutePriority,
							Match:       "(ip4.src == $a8519615025667110816 || ip4.src == $a13607449821398607916) && ip4.dst == $a1042611113178530741",
							Action:      nbdb.LogicalRouterPolicyActionAllow,
							Options:     map[string]string{"pkt_mark": types.EgressIPNodeConnectionMark},
							UUID:        "no-reroute-node-UUID",
							ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
						},
						&nbdb.LogicalRouterPolicy{
							Priority:    types.DefaultNoRereoutePriority,
							Match:       "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
							Action:      nbdb.LogicalRouterPolicyActionAllow,
							UUID:        "default-no-reroute-UUID",
							ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
						},
						&nbdb.LogicalRouterPolicy{
							Priority:    types.DefaultNoRereoutePriority,
							Match:       fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
							Action:      nbdb.LogicalRouterPolicyActionAllow,
							UUID:        "no-reroute-service-UUID",
							ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
						},
						&nbdb.LogicalRouter{
							Name:    types.GWRouterPrefix + node1.Name,
							UUID:    types.GWRouterPrefix + node1.Name + "-UUID",
							Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID"},
							Options: logicalRouterOptions,
						},
						&nbdb.LogicalRouter{
							Name:    types.GWRouterPrefix + node2.Name,
							UUID:    types.GWRouterPrefix + node2.Name + "-UUID",
							Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID"},
							Options: logicalRouterOptions,
						},
						&nbdb.LogicalRouter{
							Name: types.OVNClusterRouter,
							UUID: types.OVNClusterRouter + "-UUID",
							Policies: []string{"reroute-UUID", "default-no-reroute-UUID", "no-reroute-service-UUID",
								"no-reroute-node-UUID", "default-no-reroute-reply-traffic"},
						},
						&nbdb.LogicalRouterPort{
							UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID",
							Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name,
							Networks: []string{nodeLogicalRouterIfAddrV4},
						},
						&nbdb.LogicalRouterPort{
							UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID",
							Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name,
							Networks: []string{node2LogicalRouterIfAddrV4},
						},
						&nbdb.LogicalSwitchPort{
							UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID",
							Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
							Type: "router",
							Options: map[string]string{
								libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
								"nat-addresses":             "router",
								"exclude-lb-vips-from-garp": "true",
							},
						},
						&nbdb.LogicalSwitchPort{
							UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID",
							Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
							Type: "router",
							Options: map[string]string{
								libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
								"nat-addresses":             "router",
								"exclude-lb-vips-from-garp": "true",
							},
						},
						&nbdb.LogicalSwitchPort{
							UUID:      "k8s-" + node1.Name + "-UUID",
							Name:      "k8s-" + node1.Name,
							Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1Subnet).IP.String()},
						},
						&nbdb.LogicalSwitchPort{
							UUID:      "k8s-" + node2.Name + "-UUID",
							Name:      "k8s-" + node2.Name,
							Addresses: []string{"fe:1a:c2:3f:0e:fb " + util.GetNodeManagementIfAddr(node2Subnet).IP.String()},
						},
						&nbdb.LogicalSwitch{
							UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
							Name:  types.ExternalSwitchPrefix + node1Name,
							Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
						},
						&nbdb.LogicalSwitch{
							UUID:  types.ExternalSwitchPrefix + node2Name + "-UUID",
							Name:  types.ExternalSwitchPrefix + node2Name,
							Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
						},
						getNoReRouteReplyTrafficPolicy(types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
						node1Switch,
						node2Switch,
						getDefaultQoSRule(false, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
						egressSVCServedPodsASv4,
						egressIPServedPodsASv4,
						egressNodeIPsASv4,
					}

					if node1Local {
						// QoS Rule is configured only for nodes in local zones; the controller in the remote zone will do it for the remote nodes
						node1Switch.QOSRules = []string{"default-QoS-UUID"}
					}
					if node2Local {
						// add QoS config only if node is in local zone
						node2Switch.QOSRules = []string{"default-QoS-UUID"}
					}
					if !node1Local {
						podPolicy := getReRoutePolicySecondaryHost(podV4IP, "4", "static-reroute-UUID", []string{node2MgntIP.To4().String()},
							getEgressIPLRPReRouteDbIDs(eIP.Name, egressPod.Namespace, egressPod.Name, IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs())
						expectedDatabaseState = append(expectedDatabaseState, podPolicy)
						expectedDatabaseState[6].(*nbdb.LogicalRouter).Policies = append(expectedDatabaseState[6].(*nbdb.LogicalRouter).Policies, "static-reroute-UUID")
						expectedDatabaseState[6].(*nbdb.LogicalRouter).Policies = expectedDatabaseState[6].(*nbdb.LogicalRouter).Policies[1:] // remove ref to LRP since static route is routing the pod
						expectedDatabaseState = expectedDatabaseState[1:]                                                                     //remove needless LRP since static route is incharge of routing when pod is remote
					}

					gomega.Eventually(fakeOvn.nbClient, inspectTimeout).Should(libovsdbtest.HaveData(expectedDatabaseState))

					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			},
			// will showcase localzone setup - the controller for the pod's zone is local, where pod's reroute policy towards egressNode will be done.
			// NOTE: SNAT won't be visible because its in remote zone
			ginkgo.Entry("interconnect enabled; node1 in local and node2 in remote zones", true, node1Name),
			// will showcase localzone setup - the controller for the egress node's zone is local, where pod's SNAT policy and static route will be done.
			// NOTE: reroute policy won't be visible because its in remote zone (pod is in remote zone)
			ginkgo.Entry("interconnect enabled; node1 in remote and node2 in local zones", true, node2Name),
		)
	})

	ginkgo.Context("IPv4 on pod UPDATE", func() {
		ginkgo.It("does not reconfigure or remove existing pod config if no change", func() {
			app.Action = func(*cli.Context) error {
				egressPod := *ovntest.NewPodWithLabels(eipNamespace, podName, node1Name, podV4IP, egressPodLabel)
				egressNamespace := ovntest.NewNamespace(eipNamespace)
				nodeIPv4 := "192.168.126.210/24"
				egressIP := net.ParseIP("192.168.126.211")
				_, nodeSubnetV4, _ := net.ParseCIDR(v4Node1Subnet)
				_, nodeSubnetV6, _ := net.ParseCIDR(v6Node1Subnet)

				annotations := map[string]string{
					"k8s.ovn.org/node-primary-ifaddr":             fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", nodeIPv4, ""),
					"k8s.ovn.org/node-subnets":                    fmt.Sprintf("{\"default\":\"%s\",\"%s\"}", v4Node1Subnet, v6Node1Subnet),
					"k8s.ovn.org/node-transit-switch-port-ifaddr": "{\"ipv4\":\"100.88.0.2/16\", \"ipv6\": \"fd97::2/64\"}",
					util.OVNNodeHostCIDRs:                         fmt.Sprintf("[\"%s\"]", nodeIPv4),
				}
				node := getNodeObj(node1Name, annotations, map[string]string{}) // add node to avoid errori-ing out on transit switch IP fetch
				fakeOvn.startWithDBSetup(
					libovsdbtest.TestSetup{
						NBData: []libovsdbtest.TestData{
							&nbdb.LogicalRouterPort{
								UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1Name + "-UUID",
								Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1Name,
								Networks: []string{nodeLogicalRouterIfAddrV6, nodeLogicalRouterIfAddrV4},
							},
							&nbdb.LogicalRouter{
								Name: types.OVNClusterRouter,
								UUID: types.OVNClusterRouter + "-UUID",
							},
							&nbdb.LogicalRouter{
								Name:    types.GWRouterPrefix + node1Name,
								UUID:    types.GWRouterPrefix + node1Name + "-UUID",
								Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
								Options: map[string]string{"dynamic_neigh_routers": "false"},
							},
							&nbdb.LogicalSwitchPort{
								UUID: "k8s-" + node.Name + "-UUID",
								Name: "k8s-" + node.Name,
								Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(nodeSubnetV4).IP.String(),
									"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(nodeSubnetV6).IP.String()},
							},
							&nbdb.LogicalSwitch{
								UUID:  node.Name + "-UUID",
								Name:  node.Name,
								Ports: []string{"k8s-" + node.Name + "-UUID"},
							},
						},
					},
					&corev1.NamespaceList{
						Items: []corev1.Namespace{*egressNamespace},
					},
					&corev1.PodList{
						Items: []corev1.Pod{egressPod},
					},
					&corev1.NodeList{
						Items: []corev1.Node{node},
					},
				)

				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{
							egressIP.String(),
						},
						NamespaceSelector: metav1.LabelSelector{
							MatchLabels: map[string]string{
								"name": egressNamespace.Name,
							},
						},
						PodSelector: metav1.LabelSelector{
							MatchLabels: egressPodLabel,
						},
					},
				}
				i, n, _ := net.ParseCIDR(podV4IP + "/23")
				n.IP = i
				fakeOvn.controller.logicalPortCache.add(&egressPod, "", types.DefaultNetworkName, "", nil, []*net.IPNet{n})
				err := fakeOvn.controller.WatchEgressIPPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIPNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				_, err = fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Create(context.TODO(), &eIP, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.patchEgressIPObj(node1Name, egressIPName, egressIP.String())
				gomega.Eventually(getEgressIPStatusLen(eIP.Name)).Should(gomega.Equal(1))

				expectedDatabaseState := []libovsdbtest.TestData{
					getReRoutePolicy(egressPod.Status.PodIP, "4", "reroute-UUID", nodeLogicalRouterIPv4,
						getEgressIPLRPReRouteDbIDs(eIP.Name, egressPod.Namespace, egressPod.Name, IPFamilyValueV4,
							types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs()),
					getEIPSNAT(podV4IP, egressPod.Namespace, egressPod.Name, egressIP.String(), "k8s-node1", types.DefaultNetworkControllerName),
					&nbdb.LogicalRouter{
						Name:     types.OVNClusterRouter,
						UUID:     types.OVNClusterRouter + "-UUID",
						Policies: []string{"reroute-UUID"},
					},
					&nbdb.LogicalRouterPort{
						UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1Name + "-UUID",
						Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1Name,
						Networks: []string{nodeLogicalRouterIfAddrV6, nodeLogicalRouterIfAddrV4},
					},
					&nbdb.LogicalRouter{
						Name:    types.GWRouterPrefix + node1Name,
						UUID:    types.GWRouterPrefix + node1Name + "-UUID",
						Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
						Nat:     []string{"egressip-nat-UUID"},
						Options: map[string]string{"dynamic_neigh_routers": "false"},
					},
					&nbdb.LogicalSwitchPort{
						UUID: "k8s-" + node.Name + "-UUID",
						Name: "k8s-" + node.Name,
						Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(nodeSubnetV4).IP.String(),
							"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(nodeSubnetV6).IP.String()},
					},
					&nbdb.LogicalSwitch{
						UUID:  node.Name + "-UUID",
						Name:  node.Name,
						Ports: []string{"k8s-" + node.Name + "-UUID"},
					},
				}
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
				// async, create no-op updates that trigger reconcile for the selected pod async but update should continue to select the pod and not alter pod config
				// meanwhile we watch the ovn dbs and ensure they do not alter for the given pods eip config
				// therefore spawn a go routine to update the k8 constructs that will trigger reconcile of the pods, and, we want to ensure nothing is reconfigured.
				errCh := make(chan error, 2)
				go func() {
					ns, err := fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Get(context.TODO(), egressNamespace.Name, metav1.GetOptions{})
					if err != nil {
						errCh <- err
						return
					}
					// add new namespace label. Does not affect pod selection for EIP
					ns = ns.DeepCopy()
					ns.Labels["newlabel"] = "noop"
					_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Update(context.TODO(), ns, metav1.UpdateOptions{})
					if err != nil {
						errCh <- err
						return
					}
					// add new pod label. Does not affect pod selection for EIP
					pod, err := fakeOvn.fakeClient.KubeClient.CoreV1().Pods(egressNamespace.Name).Get(context.TODO(), egressPod.Name, metav1.GetOptions{})
					if err != nil {
						errCh <- err
						return
					}
					pod = pod.DeepCopy()
					pod.Labels["newlabel"] = "noop"
					_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(egressNamespace.Name).Update(context.TODO(), pod, metav1.UpdateOptions{})
					if err != nil {
						errCh <- err
					}
					close(errCh)
				}()
				ginkgo.By("ensure OVN DB config for EIP remains consistent")
				// ensure the DBs are unaltered
				gomega.Consistently(fakeOvn.nbClient, 500*time.Millisecond, 1*time.Millisecond).WithTimeout(5 * time.Second).Should(libovsdbtest.HaveData(expectedDatabaseState))
				ginkgo.By("check for errors from goroutine updating namespace and pods")
				select {
				case err := <-errCh:
					if err != nil {
						gomega.Expect(err).NotTo(gomega.HaveOccurred(), "must successfully update namespace and pods")
					}
				case <-time.After(100 * time.Millisecond):
					// Updates completed successfully
				}
				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})

	ginkgo.Context("IPv6 on pod UPDATE", func() {

		ginkgo.DescribeTable("should remove OVN pod egress setup when EgressIP stops matching pod label",
			func(isnode1Local, isnode2Local bool) {
				if isnode2Local {
					fakeOvn.nodeName = node2Name
				}
				config.IPv6Mode = true
				app.Action = func(*cli.Context) error {

					egressIP := net.ParseIP("0:0:0:0:0:feff:c0a8:8e0d")

					egressPod := *ovntest.NewPodWithLabels(eipNamespace, podName, node1Name, podV6IP, egressPodLabel)
					egressNamespace := ovntest.NewNamespace(eipNamespace)
					node1IPv4 := "192.168.126.210/24"
					_, node1SubnetV4, _ := net.ParseCIDR(v4Node1Subnet)
					_, node1SubnetV6, _ := net.ParseCIDR(v6Node1Subnet)

					annotations := map[string]string{
						"k8s.ovn.org/node-primary-ifaddr":             fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4, ""),
						"k8s.ovn.org/node-subnets":                    fmt.Sprintf("{\"default\":\"%s\",\"%s\"}", v4Node1Subnet, v6Node1Subnet),
						"k8s.ovn.org/node-transit-switch-port-ifaddr": "{\"ipv4\":\"100.88.0.2/16\", \"ipv6\": \"fd97::2/64\"}", // used only for ic=true test
						util.OVNNodeHostCIDRs:                         fmt.Sprintf("[\"%s\"]", node1IPv4),
					}
					node1 := getNodeObj(node1Name, annotations, map[string]string{}) // add node to avoid errori-ing out on transit switch IP fetch

					node2IPv4 := "192.168.126.202/24"
					_, node2SubnetV4, _ := net.ParseCIDR(v4Node2Subnet)
					node2IPv6 := "::feff:c0a8:8e0c/64"
					_, node2SubnetV6, _ := net.ParseCIDR(v6Node2Subnet)
					annotations = map[string]string{
						"k8s.ovn.org/node-primary-ifaddr":             fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node2IPv4, node2IPv6),
						"k8s.ovn.org/node-subnets":                    fmt.Sprintf("{\"default\":\"%s\",\"%s\"}", v4Node2Subnet, v6Node2Subnet),
						"k8s.ovn.org/node-transit-switch-port-ifaddr": "{\"ipv4\":\"100.88.0.3/16\", \"ipv6\": \"fd97::3/64\"}", // used only for ic=true test
						util.OVNNodeHostCIDRs:                         fmt.Sprintf("[\"%s\",\"%s\"]", node2IPv4, node2IPv6),
					}

					node2 := getNodeObj(node2Name, annotations, map[string]string{}) // add node to avoid errori-ing out on transit switch IP fetch
					dynamicNeighRouters := "false"

					logicalRouterOptions := map[string]string{
						"dynamic_neigh_routers": dynamicNeighRouters,
					}
					fakeOvn.startWithDBSetup(
						libovsdbtest.TestSetup{
							NBData: []libovsdbtest.TestData{
								&nbdb.LogicalRouterPort{
									UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1Name + "-UUID",
									Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1Name,
									Networks: []string{nodeLogicalRouterIfAddrV6, nodeLogicalRouterIfAddrV4},
								},
								&nbdb.LogicalRouterPort{
									UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2Name + "-UUID",
									Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2Name,
									Networks: []string{node2LogicalRouterIfAddrV6, node2LogicalRouterIfAddrV6},
								},
								&nbdb.LogicalRouter{
									Name: types.OVNClusterRouter,
									UUID: types.OVNClusterRouter + "-UUID",
								},
								&nbdb.LogicalRouter{
									Name:    types.GWRouterPrefix + node1Name,
									UUID:    types.GWRouterPrefix + node1Name + "-UUID",
									Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
									Options: logicalRouterOptions,
								},
								&nbdb.LogicalRouter{
									Name:    types.GWRouterPrefix + node2Name,
									UUID:    types.GWRouterPrefix + node2Name + "-UUID",
									Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
									Nat:     nil,
									Options: logicalRouterOptions,
								},
								&nbdb.LogicalSwitchPort{
									UUID: "k8s-" + node1.Name + "-UUID",
									Name: "k8s-" + node1.Name,
									Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1SubnetV4).IP.String(),
										"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1SubnetV6).IP.String()},
								},
								&nbdb.LogicalSwitchPort{
									UUID: "k8s-" + node2.Name + "-UUID",
									Name: "k8s-" + node2.Name,
									Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node2SubnetV4).IP.String(),
										"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node2SubnetV6).IP.String()},
								},
								&nbdb.LogicalSwitch{
									UUID:  node1.Name + "-UUID",
									Name:  node1.Name,
									Ports: []string{"k8s-" + node1.Name + "-UUID"},
								},
								&nbdb.LogicalSwitch{
									UUID:  node2.Name + "-UUID",
									Name:  node2.Name,
									Ports: []string{"k8s-" + node2.Name + "-UUID"},
								},
							},
						},
						&corev1.NamespaceList{
							Items: []corev1.Namespace{*egressNamespace},
						},
						&corev1.PodList{
							Items: []corev1.Pod{egressPod},
						},
						&corev1.NodeList{
							Items: []corev1.Node{node1, node2},
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

					err := fakeOvn.controller.WatchEgressIPPods()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchEgressIPNamespaces()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchEgressIP()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					_, err = fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Create(context.TODO(), &eIP, metav1.CreateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					fakeOvn.patchEgressIPObj(node2Name, egressIPName, egressIP.String())

					gomega.Eventually(getEgressIPStatusLen(eIP.Name)).Should(gomega.Equal(1))

					expectedNatLogicalPort := "k8s-node2"
					expectedDatabaseState := []libovsdbtest.TestData{
						getReRoutePolicy(egressPod.Status.PodIP, "6", "reroute-UUID", node2LogicalRouterIPv6,
							getEgressIPLRPReRouteDbIDs(eIP.Name, egressPod.Namespace, egressPod.Name, IPFamilyValueV6,
								types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs()),
						getEIPSNAT(podV6IP, egressPod.Namespace, egressPod.Name, egressIP.String(), expectedNatLogicalPort, types.DefaultNetworkControllerName),
						&nbdb.LogicalRouter{
							Name:     types.OVNClusterRouter,
							UUID:     types.OVNClusterRouter + "-UUID",
							Policies: []string{"reroute-UUID"},
						},
						&nbdb.LogicalRouterPort{
							UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1Name + "-UUID",
							Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1Name,
							Networks: []string{nodeLogicalRouterIfAddrV6, nodeLogicalRouterIfAddrV4},
						},
						&nbdb.LogicalRouterPort{
							UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2Name + "-UUID",
							Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2Name,
							Networks: []string{node2LogicalRouterIfAddrV6, node2LogicalRouterIfAddrV6},
						},
						&nbdb.LogicalRouter{
							Name:    types.GWRouterPrefix + node1Name,
							UUID:    types.GWRouterPrefix + node1Name + "-UUID",
							Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
							Options: logicalRouterOptions,
						},
						&nbdb.LogicalRouter{
							Name:    types.GWRouterPrefix + node2Name,
							UUID:    types.GWRouterPrefix + node2Name + "-UUID",
							Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
							Nat:     []string{"egressip-nat-UUID"},
							Options: logicalRouterOptions,
						},
						&nbdb.LogicalSwitchPort{
							UUID: "k8s-" + node1.Name + "-UUID",
							Name: "k8s-" + node1.Name,
							Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1SubnetV4).IP.String(),
								"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1SubnetV6).IP.String()},
						},
						&nbdb.LogicalSwitchPort{
							UUID: "k8s-" + node2.Name + "-UUID",
							Name: "k8s-" + node2.Name,
							Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node2SubnetV4).IP.String(),
								"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node2SubnetV6).IP.String()},
						},
						&nbdb.LogicalSwitch{
							UUID:  node1.Name + "-UUID",
							Name:  node1.Name,
							Ports: []string{"k8s-" + node1.Name + "-UUID"},
						},
						&nbdb.LogicalSwitch{
							UUID:  node2.Name + "-UUID",
							Name:  node2.Name,
							Ports: []string{"k8s-" + node2.Name + "-UUID"},
						},
					}

					if !isnode2Local {
						// case3: pod's SNAT is not visible because egress node is remote
						expectedDatabaseState[6].(*nbdb.LogicalRouter).Nat = []string{}
						expectedDatabaseState = expectedDatabaseState[2:]
						// add policy with nextHop towards egressNode's transit switchIP
						expectedDatabaseState = append(expectedDatabaseState, getReRoutePolicy(egressPod.Status.PodIP,
							"6", "reroute-UUID", []string{"fd97::3"},
							getEgressIPLRPReRouteDbIDs(eIP.Name, egressPod.Namespace, egressPod.Name, IPFamilyValueV6, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs()))
					}
					if !isnode1Local {
						expectedDatabaseState[2].(*nbdb.LogicalRouter).Policies = []string{}
						expectedDatabaseState = expectedDatabaseState[1:]
					}
					gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

					egressIPs, nodes := getEgressIPStatus(eIP.Name)
					gomega.Expect(nodes[0]).To(gomega.Equal(node2Name))
					gomega.Expect(egressIPs[0]).To(gomega.Equal(egressIP.String()))

					podUpdate := ovntest.NewPod(eipNamespace, podName, node1Name, podV6IP)

					_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(egressPod.Namespace).Update(context.TODO(), podUpdate, metav1.UpdateOptions{})
					gomega.Expect(err).ToNot(gomega.HaveOccurred())
					gomega.Eventually(getEgressIPStatusLen(eIP.Name)).Should(gomega.Equal(1))
					expectedDatabaseState = []libovsdbtest.TestData{
						&nbdb.LogicalRouter{
							Name:     types.OVNClusterRouter,
							UUID:     types.OVNClusterRouter + "-UUID",
							Policies: []string{},
						},
						&nbdb.LogicalRouterPort{
							UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1Name + "-UUID",
							Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1Name,
							Networks: []string{nodeLogicalRouterIfAddrV6, nodeLogicalRouterIfAddrV4},
						},
						&nbdb.LogicalRouterPort{
							UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2Name + "-UUID",
							Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2Name,
							Networks: []string{node2LogicalRouterIfAddrV6, node2LogicalRouterIfAddrV6},
						},
						&nbdb.LogicalRouter{
							Name:    types.GWRouterPrefix + node1Name,
							UUID:    types.GWRouterPrefix + node1Name + "-UUID",
							Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
							Options: logicalRouterOptions,
						},
						&nbdb.LogicalRouter{
							Name:    types.GWRouterPrefix + node2Name,
							UUID:    types.GWRouterPrefix + node2Name + "-UUID",
							Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
							Nat:     nil,
							Options: logicalRouterOptions,
						},
						&nbdb.LogicalSwitchPort{
							UUID: "k8s-" + node1.Name + "-UUID",
							Name: "k8s-" + node1.Name,
							Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1SubnetV4).IP.String(),
								"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1SubnetV6).IP.String()},
						},
						&nbdb.LogicalSwitchPort{
							UUID: "k8s-" + node2.Name + "-UUID",
							Name: "k8s-" + node2.Name,
							Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node2SubnetV4).IP.String(),
								"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node2SubnetV6).IP.String()},
						},
						&nbdb.LogicalSwitch{
							UUID:  node1.Name + "-UUID",
							Name:  node1.Name,
							Ports: []string{"k8s-" + node1.Name + "-UUID"},
						},
						&nbdb.LogicalSwitch{
							UUID:  node2.Name + "-UUID",
							Name:  node2.Name,
							Ports: []string{"k8s-" + node2.Name + "-UUID"},
						},
					}

					gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			},
			ginkgo.Entry("pod is in local zone and egressnode is in remote zone", true, false), // snat won't be visible
			ginkgo.Entry("pod is in remote zone and egressnode is in local zone", false, true),
		)

		ginkgo.DescribeTable("egressIP pod retry should remove OVN pod egress setup when EgressIP stops matching pod label",
			func(controllerNodeName string) {
				fakeOvn.nodeName = controllerNodeName
				app.Action = func(*cli.Context) error {

					egressIP := net.ParseIP("0:0:0:0:0:feff:c0a8:8e0f")

					egressPod := *ovntest.NewPodWithLabels(eipNamespace, podName, node1Name, podV6IP, egressPodLabel)
					egressNamespace := ovntest.NewNamespace(eipNamespace)
					nodes := getIPv6Nodes([]nodeInfo{
						{[]string{"0:0:0:0:0:feff:c0a8:8e0c/64"}, "fd97::2/64"},
						{[]string{"0:0:0:0:0:fedf:c0a8:8e0c/64"}, "fd97::3/64"},
					})
					node1 := nodes[0]
					_, node1Subnet, _ := net.ParseCIDR(v6Node1Subnet)
					node2 := nodes[1]
					_, node2Subnet, _ := net.ParseCIDR(v6Node2Subnet)
					dynamicNeighRouters := "false"

					logicalRouterOptions := map[string]string{
						"dynamic_neigh_routers": dynamicNeighRouters,
					}
					fakeOvn.startWithDBSetup(
						libovsdbtest.TestSetup{
							NBData: []libovsdbtest.TestData{
								&nbdb.LogicalRouterPort{
									UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1Name + "-UUID",
									Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1Name,
									Networks: []string{nodeLogicalRouterIfAddrV6},
								},
								&nbdb.LogicalRouterPort{
									UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2Name + "-UUID",
									Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2Name,
									Networks: []string{node2LogicalRouterIfAddrV6},
								},
								&nbdb.LogicalRouter{
									Name: types.OVNClusterRouter,
									UUID: types.OVNClusterRouter + "-UUID",
								},
								&nbdb.LogicalRouter{
									Name:    types.GWRouterPrefix + node1Name,
									UUID:    types.GWRouterPrefix + node1Name + "-UUID",
									Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
									Options: logicalRouterOptions,
								},
								&nbdb.LogicalRouter{
									Name:    types.GWRouterPrefix + node2Name,
									UUID:    types.GWRouterPrefix + node2Name + "-UUID",
									Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
									Nat:     nil,
									Options: logicalRouterOptions,
								},
								&nbdb.LogicalSwitchPort{
									UUID:      "k8s-" + node1.Name + "-UUID",
									Name:      "k8s-" + node1.Name,
									Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1Subnet).IP.String()},
								},
								&nbdb.LogicalSwitchPort{
									UUID:      "k8s-" + node2.Name + "-UUID",
									Name:      "k8s-" + node2.Name,
									Addresses: []string{"fe:1a:c2:3f:0e:fb " + util.GetNodeManagementIfAddr(node2Subnet).IP.String()},
								},
								&nbdb.LogicalSwitch{
									UUID:  node1.Name + "-UUID",
									Name:  node1.Name,
									Ports: []string{"k8s-" + node1.Name + "-UUID"},
								},
								&nbdb.LogicalSwitch{
									UUID:  node2.Name + "-UUID",
									Name:  node2.Name,
									Ports: []string{"k8s-" + node2.Name + "-UUID"},
								},
							},
						},
						&corev1.NamespaceList{
							Items: []corev1.Namespace{*egressNamespace},
						},
						&corev1.PodList{
							Items: []corev1.Pod{egressPod},
						},
						&corev1.NodeList{Items: []corev1.Node{node1, node2}},
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

					err := fakeOvn.controller.WatchEgressIPPods()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchEgressIPNamespaces()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchEgressIP()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					_, err = fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Create(context.TODO(), &eIP, metav1.CreateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					fakeOvn.patchEgressIPObj(node2Name, egressIPName, egressIP.String())

					gomega.Eventually(getEgressIPStatusLen(eIP.Name)).Should(gomega.Equal(1))

					expectedNatLogicalPort := "k8s-node2"
					expectedDatabaseState := []libovsdbtest.TestData{
						getReRoutePolicy(egressPod.Status.PodIP, "6", "reroute-UUID", node2LogicalRouterIPv6,
							getEgressIPLRPReRouteDbIDs(eIP.Name, egressPod.Namespace, egressPod.Name, IPFamilyValueV6, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs()),
						getEIPSNAT(podV6IP, egressPod.Namespace, egressPod.Name, egressIP.String(), expectedNatLogicalPort, types.DefaultNetworkControllerName),
						&nbdb.LogicalRouter{
							Name:     types.OVNClusterRouter,
							UUID:     types.OVNClusterRouter + "-UUID",
							Policies: []string{"reroute-UUID"},
						},
						&nbdb.LogicalRouterPort{
							UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1Name + "-UUID",
							Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1Name,
							Networks: []string{nodeLogicalRouterIfAddrV6},
						},
						&nbdb.LogicalRouterPort{
							UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2Name + "-UUID",
							Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2Name,
							Networks: []string{node2LogicalRouterIfAddrV6},
						},
						&nbdb.LogicalRouter{
							Name:    types.GWRouterPrefix + node1Name,
							UUID:    types.GWRouterPrefix + node1Name + "-UUID",
							Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
							Options: logicalRouterOptions,
						},
						&nbdb.LogicalRouter{
							Name:    types.GWRouterPrefix + node2Name,
							UUID:    types.GWRouterPrefix + node2Name + "-UUID",
							Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
							Nat:     []string{"egressip-nat-UUID"},
							Options: logicalRouterOptions,
						},
						&nbdb.LogicalSwitchPort{
							UUID:      "k8s-" + node1.Name + "-UUID",
							Name:      "k8s-" + node1.Name,
							Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1Subnet).IP.String()},
						},
						&nbdb.LogicalSwitchPort{
							UUID:      "k8s-" + node2.Name + "-UUID",
							Name:      "k8s-" + node2.Name,
							Addresses: []string{"fe:1a:c2:3f:0e:fb " + util.GetNodeManagementIfAddr(node2Subnet).IP.String()},
						},
						&nbdb.LogicalSwitch{
							UUID:  node1.Name + "-UUID",
							Name:  node1.Name,
							Ports: []string{"k8s-" + node1.Name + "-UUID"},
						},
						&nbdb.LogicalSwitch{
							UUID:  node2.Name + "-UUID",
							Name:  node2.Name,
							Ports: []string{"k8s-" + node2.Name + "-UUID"},
						},
					}
					if controllerNodeName == node1Name {
						expectedDatabaseState[0] = getReRoutePolicy(egressPod.Status.PodIP, "6", "reroute-UUID", []string{"fd97::3"},
							getEgressIPLRPReRouteDbIDs(eIP.Name, egressPod.Namespace, egressPod.Name, IPFamilyValueV6, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs())
						expectedDatabaseState[6].(*nbdb.LogicalRouter).Nat = nil
						expectedDatabaseState = append(expectedDatabaseState[:1], expectedDatabaseState[2:]...)
					} else {
						expectedDatabaseState[2].(*nbdb.LogicalRouter).Policies = []string{}
						expectedDatabaseState = expectedDatabaseState[1:]
					}

					gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

					egressIPs, assignedNodes := getEgressIPStatus(eIP.Name)
					gomega.Expect(assignedNodes[0]).To(gomega.Equal(node2Name))
					gomega.Expect(egressIPs[0]).To(gomega.Equal(egressIP.String()))

					podUpdate := ovntest.NewPod(eipNamespace, podName, node1Name, podV6IP)
					ginkgo.By("Bringing down NBDB")
					// inject transient problem, nbdb is down
					fakeOvn.controller.nbClient.Close()
					gomega.Eventually(func() bool {
						return fakeOvn.controller.nbClient.Connected()
					}).Should(gomega.BeFalse())
					_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(egressPod.Namespace).Update(context.TODO(), podUpdate, metav1.UpdateOptions{})
					gomega.Expect(err).ToNot(gomega.HaveOccurred())
					time.Sleep(config.Default.OVSDBTxnTimeout + time.Second)
					// check to see if the retry cache has an entry
					var key string
					key, err = retry.GetResourceKey(podUpdate)
					gomega.Expect(err).ToNot(gomega.HaveOccurred())
					// no retry is expected if the pod is remote and the node isn't an egress node
					if controllerNodeName == node1Name {
						ginkgo.By("retry entry: new obj should not be nil, config should not be nil")
						retry.CheckRetryObjectMultipleFieldsEventually(
							key,
							fakeOvn.controller.retryEgressIPPods,
							gomega.BeNil(),             // oldObj should be nil
							gomega.Not(gomega.BeNil()), // newObj should not be nil
							gomega.Not(gomega.BeNil()), // config should not be nil
						)
					}

					connCtx, cancel := context.WithTimeout(context.Background(), config.Default.OVSDBTxnTimeout)
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
			ginkgo.Entry("controller manages the pod node", node1Name),
			ginkgo.Entry("controller manages the egress node", node2Name),
		)

		ginkgo.It("should not treat pod update if pod already had assigned IP when it got the ADD", func() {
			app.Action = func(*cli.Context) error {

				egressIP := net.ParseIP("0:0:0:0:0:feff:c0a8:8e0d")

				egressPod := *ovntest.NewPodWithLabels(eipNamespace, podName, node1Name, podV6IP, egressPodLabel)
				egressNamespace := ovntest.NewNamespace(eipNamespace)
				_, node1Subnet, _ := net.ParseCIDR(v6Node1Subnet)
				_, node2Subnet, _ := net.ParseCIDR(v6Node2Subnet)
				node1 := newNodeNotEgressableV6Only(node1Name, "0:0:0:0:0:fedf:c0a8:8e0c/64")
				node2 := newNodeNotEgressableV6Only(node2Name, "0:0:0:0:0:feff:c0a8:8e0c/64")
				node2.Annotations["k8s.ovn.org/node-transit-switch-port-ifaddr"] = `{"ipv6":"fd97::3/64"}`
				dynamicNeighRouters := "false"

				logicalRouterOptions := map[string]string{
					"dynamic_neigh_routers": dynamicNeighRouters,
				}
				fakeOvn.startWithDBSetup(
					libovsdbtest.TestSetup{
						NBData: []libovsdbtest.TestData{
							&nbdb.LogicalRouterPort{
								UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2Name + "-UUID",
								Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2Name,
								Networks: []string{nodeLogicalRouterIfAddrV6},
							},
							&nbdb.LogicalRouter{
								Name: types.OVNClusterRouter,
								UUID: types.OVNClusterRouter + "-UUID",
							},
							&nbdb.LogicalRouter{
								Name:    types.GWRouterPrefix + node1Name,
								UUID:    types.GWRouterPrefix + node1Name + "-UUID",
								Options: logicalRouterOptions,
							},
							&nbdb.LogicalRouter{
								Name:    types.GWRouterPrefix + node2Name,
								UUID:    types.GWRouterPrefix + node2Name + "-UUID",
								Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
								Options: logicalRouterOptions,
							},
							&nbdb.LogicalSwitchPort{
								UUID:      "k8s-" + node1Name + "-UUID",
								Name:      "k8s-" + node1Name,
								Addresses: []string{"fe:1a:c2:3f:0e:fc " + util.GetNodeManagementIfAddr(node1Subnet).IP.String()},
							},
							&nbdb.LogicalSwitchPort{
								UUID:      "k8s-" + node2Name + "-UUID",
								Name:      "k8s-" + node2Name,
								Addresses: []string{"fe:1a:c2:3f:0e:fb " + util.GetNodeManagementIfAddr(node2Subnet).IP.String()},
							},
							&nbdb.LogicalSwitch{
								UUID:  node1Name + "-UUID",
								Name:  node1Name,
								Ports: []string{"k8s-" + node1Name + "-UUID"},
							},
							&nbdb.LogicalSwitch{
								UUID:  node2Name + "-UUID",
								Name:  node2Name,
								Ports: []string{"k8s-" + node2Name + "-UUID"},
							},
						},
					},
					&corev1.NamespaceList{
						Items: []corev1.Namespace{*egressNamespace},
					},
					&corev1.PodList{
						Items: []corev1.Pod{egressPod},
					},
					&corev1.NodeList{Items: []corev1.Node{*node2, *node1}},
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

				err := fakeOvn.controller.WatchEgressIPPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIPNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				_, err = fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Create(context.TODO(), &eIP, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				fakeOvn.patchEgressIPObj(node2Name, egressIPName, egressIP.String())

				gomega.Eventually(getEgressIPStatusLen(eIP.Name)).Should(gomega.Equal(1))

				expectedDatabaseState := []libovsdbtest.TestData{
					&nbdb.LogicalRouterPolicy{
						Priority:    types.EgressIPReroutePriority,
						Match:       fmt.Sprintf("ip6.src == %s", egressPod.Status.PodIP),
						Action:      nbdb.LogicalRouterPolicyActionReroute,
						Nexthops:    []string{"fd97::3"},
						ExternalIDs: getEgressIPLRPReRouteDbIDs(eIP.Name, egressPod.Namespace, egressPod.Name, IPFamilyValueV6, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
						UUID:        "reroute-UUID",
					},
					&nbdb.LogicalRouter{
						Name:     types.OVNClusterRouter,
						UUID:     types.OVNClusterRouter + "-UUID",
						Policies: []string{"reroute-UUID"},
					},
					&nbdb.LogicalRouterPort{
						UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2Name + "-UUID",
						Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2Name,
						Networks: []string{nodeLogicalRouterIfAddrV6},
					},
					&nbdb.LogicalRouter{
						Name:    types.GWRouterPrefix + node1Name,
						UUID:    types.GWRouterPrefix + node1Name + "-UUID",
						Options: logicalRouterOptions,
					},
					&nbdb.LogicalRouter{
						Name:    types.GWRouterPrefix + node2Name,
						UUID:    types.GWRouterPrefix + node2Name + "-UUID",
						Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
						Options: logicalRouterOptions,
					},
					&nbdb.LogicalSwitchPort{
						UUID:      "k8s-" + node1Name + "-UUID",
						Name:      "k8s-" + node1Name,
						Addresses: []string{"fe:1a:c2:3f:0e:fc " + util.GetNodeManagementIfAddr(node1Subnet).IP.String()},
					},
					&nbdb.LogicalSwitchPort{
						UUID:      "k8s-" + node2Name + "-UUID",
						Name:      "k8s-" + node2Name,
						Addresses: []string{"fe:1a:c2:3f:0e:fb " + util.GetNodeManagementIfAddr(node2Subnet).IP.String()},
					},
					&nbdb.LogicalSwitch{
						UUID:  node1Name + "-UUID",
						Name:  node1Name,
						Ports: []string{"k8s-" + node1Name + "-UUID"},
					},
					&nbdb.LogicalSwitch{
						UUID:  node2Name + "-UUID",
						Name:  node2Name,
						Ports: []string{"k8s-" + node2Name + "-UUID"},
					},
				}
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				egressIPs, nodes := getEgressIPStatus(eIP.Name)
				gomega.Expect(nodes[0]).To(gomega.Equal(node2Name))
				gomega.Expect(egressIPs[0]).To(gomega.Equal(egressIP.String()))

				podUpdate := ovntest.NewPodWithLabels(eipNamespace, podName, node1Name, podV6IP, map[string]string{
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
			app.Action = func(*cli.Context) error {

				config.IPv6Mode = true
				node1IPv4 := "192.168.126.202"
				node1IPv4CIDR := node1IPv4 + "/24"
				node1IPv6 := "fc00:f853:ccd:e793::1"
				node1IPv6CIDR := node1IPv6 + "/64"
				node2IPv4 := "192.168.126.51"
				node2IPv4CIDR := node2IPv4 + "/24"
				node2IPv6 := "fc00:f853:ccd:e793::2"
				node2IPv6CIDR := node2IPv6 + "/64"
				vipIPv4 := "192.168.126.10"
				vipIPv4CIDR := vipIPv4 + "/24"
				vipIPv6 := "fc00:f853:ccd:e793::10"
				vipIPv6CIDR := vipIPv6 + "/64"
				_, node2Subnet, _ := net.ParseCIDR(v6Node2Subnet)

				node1 := corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: node1Name,
						Annotations: map[string]string{
							util.OVNNodeHostCIDRs: fmt.Sprintf("[\"%s\", \"%s\", \"%s\", \"%s\"]", node1IPv4CIDR, node1IPv6CIDR, vipIPv4CIDR, vipIPv6CIDR),
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
							util.OVNNodeHostCIDRs: fmt.Sprintf("[\"%s\", \"%s\"]", node2IPv4CIDR, node2IPv6CIDR),
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
				node1Switch := &nbdb.LogicalSwitch{
					UUID: node1Name + "-UUID",
					Name: node1Name,
				}
				node2Switch := &nbdb.LogicalSwitch{
					UUID:  node2Name + "-UUID",
					Name:  node2Name,
					Ports: []string{"k8s-" + node2Name + "-UUID"},
				}

				fakeOvn.startWithDBSetup(
					libovsdbtest.TestSetup{
						NBData: []libovsdbtest.TestData{
							&nbdb.LogicalRouter{
								Name: types.OVNClusterRouter,
								UUID: types.OVNClusterRouter + "-UUID",
							},
							&nbdb.LogicalSwitchPort{
								UUID:      "k8s-" + node2Name + "-UUID",
								Name:      "k8s-" + node2Name,
								Addresses: []string{"fe:1a:c2:3f:0e:fb " + util.GetNodeManagementIfAddr(node2Subnet).IP.String()},
							},
							node1Switch,
							node2Switch,
						},
					},
					&corev1.NodeList{
						Items: []corev1.Node{node1, node2},
					},
				)

				err := fakeOvn.controller.WatchEgressNodes()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				egressSVCServedPodsASv4, egressSVCServedPodsASv6 := buildEgressServiceAddressSets(nil)
				egressIPServedPodsASv4, egressIPServedPodsASv6 := buildEgressIPServedPodsAddressSets(nil, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName)
				egressNodeIPsASv4, egressNodeIPsASv6 := buildEgressIPNodeAddressSets([]string{node1IPv4, vipIPv4, node2IPv4, node1IPv6, vipIPv6, node2IPv6})
				node1Switch.QOSRules = []string{"default-QoS-UUID", "default-QoSv6-UUID"}

				expectedDatabaseState := []libovsdbtest.TestData{
					&nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-no-reroute-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					getNoReRouteReplyTrafficPolicy(types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
					&nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-service-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
							egressIPServedPodsASv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": types.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip6.src == $%s || ip6.src == $%s) && ip6.dst == $%s",
							egressIPServedPodsASv6.Name, egressSVCServedPodsASv6.Name, egressNodeIPsASv6.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-v6-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": types.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV6, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouter{
						Name: types.OVNClusterRouter,
						UUID: types.OVNClusterRouter + "-UUID",
						Policies: []string{
							"default-no-reroute-UUID",
							"no-reroute-service-UUID",
							"default-no-reroute-node-UUID",
							"default-v6-no-reroute-node-UUID",
							"default-no-reroute-reply-traffic",
						},
					},
					&nbdb.LogicalSwitchPort{
						UUID:      "k8s-" + node2Name + "-UUID",
						Name:      "k8s-" + node2Name,
						Addresses: []string{"fe:1a:c2:3f:0e:fb " + util.GetNodeManagementIfAddr(node2Subnet).IP.String()},
					},
					node1Switch,
					node2Switch,
					egressSVCServedPodsASv4, egressSVCServedPodsASv6, egressIPServedPodsASv4, egressIPServedPodsASv6, egressNodeIPsASv4, egressNodeIPsASv6,
					getDefaultQoSRule(false, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
					getDefaultQoSRule(true, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
				}

				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				node1.ObjectMeta.Annotations[util.OVNNodeHostCIDRs] = fmt.Sprintf("[\"%s\", \"%s\"]", node1IPv4CIDR, node1IPv6CIDR)
				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), &node1, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				err = fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Delete(context.TODO(), node1.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				egressNodeIPsASv4.Addresses = []string{node2IPv4}
				egressNodeIPsASv6.Addresses = []string{node2IPv6}
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})

	ginkgo.Context("On node DELETE", func() {

		ginkgo.DescribeTable("should treat pod update if pod did not have an assigned IP when it got the ADD",
			func(controllerNodeName string) {
				fakeOvn.nodeName = controllerNodeName
				app.Action = func(*cli.Context) error {

					egressIP := net.ParseIP("0:0:0:0:0:feff:c0a8:8e0d")

					egressPod := *ovntest.NewPodWithLabels(eipNamespace, podName, node1Name, "", egressPodLabel)
					egressNamespace := ovntest.NewNamespace(eipNamespace)
					_, node1Subnet, _ := net.ParseCIDR(v6Node1Subnet)
					_, node2Subnet, _ := net.ParseCIDR(v6Node2Subnet)
					node1 := newNodeNotEgressableV6Only(node1Name, "0:0:0:0:0:fedf:c0a8:8e0c/64")
					node2 := newNodeNotEgressableV6Only(node2Name, "0:0:0:0:0:feff:c0a8:8e0c/64")
					node2.Annotations["k8s.ovn.org/node-transit-switch-port-ifaddr"] = `{"ipv6":"fd97::3/64"}`
					dynamicNeighRouters := "false"

					logicalRouterOptions := map[string]string{
						"dynamic_neigh_routers": dynamicNeighRouters,
					}
					fakeOvn.startWithDBSetup(
						libovsdbtest.TestSetup{
							NBData: []libovsdbtest.TestData{
								&nbdb.LogicalRouterPort{
									UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2Name + "-UUID",
									Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2Name,
									Networks: []string{node2LogicalRouterIfAddrV6},
								},
								&nbdb.LogicalRouter{
									Name: types.OVNClusterRouter,
									UUID: types.OVNClusterRouter + "-UUID",
								},
								&nbdb.LogicalRouter{
									Name:    types.GWRouterPrefix + node1Name,
									UUID:    types.GWRouterPrefix + node1Name + "-UUID",
									Options: logicalRouterOptions,
								},
								&nbdb.LogicalRouter{
									Name:    types.GWRouterPrefix + node2Name,
									UUID:    types.GWRouterPrefix + node2Name + "-UUID",
									Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
									Nat:     nil,
									Options: logicalRouterOptions,
								},
								&nbdb.LogicalSwitchPort{
									UUID:      "k8s-" + node1Name + "-UUID",
									Name:      "k8s-" + node1Name,
									Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1Subnet).IP.String()},
								},
								&nbdb.LogicalSwitchPort{
									UUID:      "k8s-" + node2Name + "-UUID",
									Name:      "k8s-" + node2Name,
									Addresses: []string{"fe:1a:c2:3f:0e:fb " + util.GetNodeManagementIfAddr(node2Subnet).IP.String()},
								},
								&nbdb.LogicalSwitch{
									UUID:  node1Name + "-UUID",
									Name:  node1Name,
									Ports: []string{"k8s-" + node1Name + "-UUID"},
								},
								&nbdb.LogicalSwitch{
									UUID:  node2Name + "-UUID",
									Name:  node2Name,
									Ports: []string{"k8s-" + node2Name + "-UUID"},
								},
							},
						},
						&corev1.NamespaceList{
							Items: []corev1.Namespace{*egressNamespace},
						},
						&corev1.PodList{
							Items: []corev1.Pod{egressPod},
						},
						&corev1.NodeList{Items: []corev1.Node{*node2, *node1}},
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

					err := fakeOvn.controller.WatchEgressIPPods()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchEgressIPNamespaces()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchEgressIP()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					_, err = fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Create(context.TODO(), &eIP, metav1.CreateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					fakeOvn.patchEgressIPObj(node2Name, egressIPName, egressIP.String())

					gomega.Eventually(getEgressIPStatusLen(eIP.Name)).Should(gomega.Equal(1))

					egressIPs, nodes := getEgressIPStatus(eIP.Name)
					gomega.Expect(nodes[0]).To(gomega.Equal(node2Name))
					gomega.Expect(egressIPs[0]).To(gomega.Equal(egressIP.String()))

					podUpdate := ovntest.NewPodWithLabels(eipNamespace, podName, node1Name, podV6IP, egressPodLabel)
					podUpdate.Annotations = map[string]string{
						"k8s.ovn.org/pod-networks": fmt.Sprintf("{\"default\":{\"ip_addresses\":[\"%s/23\"],\"mac_address\":\"0a:58:0a:83:00:0f\",\"gateway_ips\":[\"%s\"],\"ip_address\":\"%s/23\",\"gateway_ip\":\"%s\"}}", podV6IP, v6GatewayIP, podV6IP, v6GatewayIP),
					}
					i, n, _ := net.ParseCIDR(podV6IP + "/23")
					n.IP = i
					fakeOvn.controller.logicalPortCache.add(&egressPod, "", types.DefaultNetworkName, "", nil, []*net.IPNet{n})

					_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(egressPod.Namespace).Update(context.TODO(), podUpdate, metav1.UpdateOptions{})
					gomega.Expect(err).ToNot(gomega.HaveOccurred())
					gomega.Eventually(getEgressIPStatusLen(eIP.Name)).Should(gomega.Equal(1))

					expectedNatLogicalPort := "k8s-node2"
					expectedDatabaseState := []libovsdbtest.TestData{
						getReRoutePolicy(podV6IP, "6", "reroute-UUID", node2LogicalRouterIPv6,
							getEgressIPLRPReRouteDbIDs(eIP.Name, egressPod.Namespace, egressPod.Name, IPFamilyValueV6, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs()),
						getEIPSNAT(podV6IP, egressPod.Namespace, egressPod.Name, egressIP.String(), expectedNatLogicalPort, types.DefaultNetworkControllerName),
						&nbdb.LogicalRouter{
							Name:     types.OVNClusterRouter,
							UUID:     types.OVNClusterRouter + "-UUID",
							Policies: []string{"reroute-UUID"},
						},
						&nbdb.LogicalRouterPort{
							UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2Name + "-UUID",
							Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2Name,
							Networks: []string{node2LogicalRouterIfAddrV6},
						},
						&nbdb.LogicalRouter{
							Name:    types.GWRouterPrefix + node1Name,
							UUID:    types.GWRouterPrefix + node1Name + "-UUID",
							Options: logicalRouterOptions,
						},
						&nbdb.LogicalRouter{
							Name:    types.GWRouterPrefix + node2Name,
							UUID:    types.GWRouterPrefix + node2Name + "-UUID",
							Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
							Nat:     []string{"egressip-nat-UUID"},
							Options: logicalRouterOptions,
						},
						&nbdb.LogicalSwitchPort{
							UUID:      "k8s-" + node1Name + "-UUID",
							Name:      "k8s-" + node1Name,
							Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1Subnet).IP.String()},
						},
						&nbdb.LogicalSwitchPort{
							UUID:      "k8s-" + node2Name + "-UUID",
							Name:      "k8s-" + node2Name,
							Addresses: []string{"fe:1a:c2:3f:0e:fb " + util.GetNodeManagementIfAddr(node2Subnet).IP.String()},
						},
						&nbdb.LogicalSwitch{
							UUID:  node1Name + "-UUID",
							Name:  node1Name,
							Ports: []string{"k8s-" + node1Name + "-UUID"},
						},
						&nbdb.LogicalSwitch{
							UUID:  node2Name + "-UUID",
							Name:  node2Name,
							Ports: []string{"k8s-" + node2Name + "-UUID"},
						},
					}
					if controllerNodeName == node1Name {
						expectedDatabaseState[0] = getReRoutePolicy(podV6IP, "6", "reroute-UUID", []string{"fd97::3"},
							getEgressIPLRPReRouteDbIDs(eIP.Name, egressPod.Namespace, egressPod.Name, IPFamilyValueV6, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs())
						expectedDatabaseState[5].(*nbdb.LogicalRouter).Nat = nil
						expectedDatabaseState = append(expectedDatabaseState[:1], expectedDatabaseState[2:]...)
					} else {
						expectedDatabaseState[2].(*nbdb.LogicalRouter).Policies = []string{}
						expectedDatabaseState = expectedDatabaseState[1:]
					}
					gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			},
			ginkgo.Entry("controller manages the pod node", node1Name),
			ginkgo.Entry("controller manages the egress node", node2Name),
		)

		ginkgo.It("should not treat pod DELETE if pod did not have an assigned IP when it got the ADD and we receive a DELETE before the IP UPDATE", func() {
			app.Action = func(*cli.Context) error {

				egressIP := net.ParseIP("0:0:0:0:0:feff:c0a8:8e0d")

				egressPod := *ovntest.NewPodWithLabels(eipNamespace, podName, node1Name, "", egressPodLabel)
				egressNamespace := ovntest.NewNamespace(eipNamespace)
				fakeOvn.startWithDBSetup(clusterRouterDbSetup,
					&corev1.NamespaceList{
						Items: []corev1.Namespace{*egressNamespace},
					},
					&corev1.PodList{
						Items: []corev1.Pod{egressPod},
					},
					&corev1.NodeList{Items: []corev1.Node{*newNodeNotEgressableV6Only(node2Name, "0:0:0:0:0:feff:c0a8:8e0c/64"),
						*newNodeNotEgressableV6Only(node1Name, "0:0:0:0:0:fedf:c0a8:8e0c/64")}},
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

				err := fakeOvn.controller.WatchEgressIPPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIPNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				_, err = fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Create(context.TODO(), &eIP, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				fakeOvn.patchEgressIPObj(node2Name, egressIPName, egressIP.String())

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

		ginkgo.DescribeTable("should remove OVN pod egress setup when EgressIP is deleted",
			func(controllerNodeName string) {
				fakeOvn.nodeName = controllerNodeName
				app.Action = func(*cli.Context) error {

					egressIP := net.ParseIP("0:0:0:0:0:feff:c0a8:8e0d")

					egressPod := *ovntest.NewPodWithLabels(eipNamespace, podName, node1Name, podV6IP, egressPodLabel)
					egressNamespace := ovntest.NewNamespaceWithLabels(eipNamespace, egressPodLabel)
					node1 := newNodeNotEgressableV6Only(node1Name, "0:0:0:0:0:feff:c0a8:8e0c/64")
					node2 := newNodeNotEgressableV6Only(node2Name, "0:0:0:0:0:fedf:c0a8:8e0c/64")
					node2.Annotations["k8s.ovn.org/node-transit-switch-port-ifaddr"] = `{"ipv6":"fd97::3/64"}`
					_, node1Subnet, _ := net.ParseCIDR(v6Node1Subnet)
					_, node2Subnet, _ := net.ParseCIDR(v6Node2Subnet)
					dynamicNeighRouters := "false"

					logicalRouterOptions := map[string]string{
						"dynamic_neigh_routers": dynamicNeighRouters,
					}
					fakeOvn.startWithDBSetup(
						libovsdbtest.TestSetup{
							NBData: []libovsdbtest.TestData{
								&nbdb.LogicalRouterPort{
									UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2Name + "-UUID",
									Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2Name,
									Networks: []string{node2LogicalRouterIfAddrV6},
								},
								&nbdb.LogicalRouter{
									Name: types.OVNClusterRouter,
									UUID: types.OVNClusterRouter + "-UUID",
								},
								&nbdb.LogicalRouter{
									Name:    types.GWRouterPrefix + node1Name,
									UUID:    types.GWRouterPrefix + node1Name + "-UUID",
									Options: logicalRouterOptions,
								},
								&nbdb.LogicalRouter{
									Name:    types.GWRouterPrefix + node2Name,
									UUID:    types.GWRouterPrefix + node2Name + "-UUID",
									Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
									Nat:     nil,
									Options: logicalRouterOptions,
								},
								&nbdb.LogicalSwitchPort{
									UUID:      "k8s-" + node1Name + "-UUID",
									Name:      "k8s-" + node1Name,
									Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1Subnet).IP.String()},
								},
								&nbdb.LogicalSwitchPort{
									UUID:      "k8s-" + node2Name + "-UUID",
									Name:      "k8s-" + node2Name,
									Addresses: []string{"fe:1a:c2:3f:0e:fb " + util.GetNodeManagementIfAddr(node2Subnet).IP.String()},
								},
								&nbdb.LogicalSwitch{
									UUID:  node1Name + "-UUID",
									Name:  node1Name,
									Ports: []string{"k8s-" + node1Name + "-UUID"},
								},
								&nbdb.LogicalSwitch{
									UUID:  node2Name + "-UUID",
									Name:  node2Name,
									Ports: []string{"k8s-" + node2Name + "-UUID"},
								},
							},
						},
						&corev1.NamespaceList{
							Items: []corev1.Namespace{*egressNamespace},
						},
						&corev1.PodList{
							Items: []corev1.Pod{egressPod},
						},
						&corev1.NodeList{Items: []corev1.Node{*node1, *node2}},
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

					err := fakeOvn.controller.WatchEgressIPPods()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchEgressIPNamespaces()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchEgressIP()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					_, err = fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Create(context.TODO(), &eIP, metav1.CreateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					fakeOvn.patchEgressIPObj(node2Name, egressIPName, egressIP.String())

					gomega.Eventually(getEgressIPStatusLen(eIP.Name)).Should(gomega.Equal(1))

					expectedNatLogicalPort := "k8s-node2"
					expectedDatabaseState := []libovsdbtest.TestData{
						getReRoutePolicy(egressPod.Status.PodIP, "6", "reroute-UUID", node2LogicalRouterIPv6,
							getEgressIPLRPReRouteDbIDs(eIP.Name, egressPod.Namespace, egressPod.Name, IPFamilyValueV6, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs()),
						getEIPSNAT(podV6IP, egressPod.Namespace, egressPod.Name, egressIP.String(), expectedNatLogicalPort, types.DefaultNetworkControllerName),
						&nbdb.LogicalRouter{
							Name:     types.OVNClusterRouter,
							UUID:     types.OVNClusterRouter + "-UUID",
							Policies: []string{"reroute-UUID"},
						},
						&nbdb.LogicalRouterPort{
							UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2Name + "-UUID",
							Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2Name,
							Networks: []string{node2LogicalRouterIfAddrV6},
						},
						&nbdb.LogicalRouter{
							Name:    types.GWRouterPrefix + node1Name,
							UUID:    types.GWRouterPrefix + node1Name + "-UUID",
							Options: logicalRouterOptions,
						},
						&nbdb.LogicalRouter{
							Name:    types.GWRouterPrefix + node2Name,
							UUID:    types.GWRouterPrefix + node2Name + "-UUID",
							Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
							Nat:     []string{"egressip-nat-UUID"},
							Options: logicalRouterOptions,
						},
						&nbdb.LogicalSwitchPort{
							UUID:      "k8s-" + node1Name + "-UUID",
							Name:      "k8s-" + node1Name,
							Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1Subnet).IP.String()},
						},
						&nbdb.LogicalSwitchPort{
							UUID:      "k8s-" + node2Name + "-UUID",
							Name:      "k8s-" + node2Name,
							Addresses: []string{"fe:1a:c2:3f:0e:fb " + util.GetNodeManagementIfAddr(node2Subnet).IP.String()},
						},
						&nbdb.LogicalSwitch{
							UUID:  node1Name + "-UUID",
							Name:  node1Name,
							Ports: []string{"k8s-" + node1Name + "-UUID"},
						},
						&nbdb.LogicalSwitch{
							UUID:  node2Name + "-UUID",
							Name:  node2Name,
							Ports: []string{"k8s-" + node2Name + "-UUID"},
						},
					}
					if controllerNodeName == node1Name {
						expectedDatabaseState[0] = getReRoutePolicy(egressPod.Status.PodIP, "6", "reroute-UUID", []string{"fd97::3"},
							getEgressIPLRPReRouteDbIDs(eIP.Name, egressPod.Namespace, egressPod.Name, IPFamilyValueV6, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs())
						expectedDatabaseState[5].(*nbdb.LogicalRouter).Nat = nil
						expectedDatabaseState = append(expectedDatabaseState[:1], expectedDatabaseState[2:]...)
					} else {
						expectedDatabaseState[2].(*nbdb.LogicalRouter).Policies = []string{}
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
							Name: types.OVNClusterRouter,
							UUID: types.OVNClusterRouter + "-UUID",
						},
						&nbdb.LogicalRouterPort{
							UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2Name + "-UUID",
							Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2Name,
							Networks: []string{node2LogicalRouterIfAddrV6},
						},
						&nbdb.LogicalRouter{
							Name:    types.GWRouterPrefix + node1Name,
							UUID:    types.GWRouterPrefix + node1Name + "-UUID",
							Options: logicalRouterOptions,
						},
						&nbdb.LogicalRouter{
							Name:    types.GWRouterPrefix + node2Name,
							UUID:    types.GWRouterPrefix + node2Name + "-UUID",
							Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
							Nat:     nil,
							Options: logicalRouterOptions,
						},
						&nbdb.LogicalSwitchPort{
							UUID:      "k8s-" + node1Name + "-UUID",
							Name:      "k8s-" + node1Name,
							Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1Subnet).IP.String()},
						},
						&nbdb.LogicalSwitchPort{
							UUID:      "k8s-" + node2Name + "-UUID",
							Name:      "k8s-" + node2Name,
							Addresses: []string{"fe:1a:c2:3f:0e:fb " + util.GetNodeManagementIfAddr(node2Subnet).IP.String()},
						},
						&nbdb.LogicalSwitch{
							UUID:  node1Name + "-UUID",
							Name:  node1Name,
							Ports: []string{"k8s-" + node1Name + "-UUID"},
						},
						&nbdb.LogicalSwitch{
							UUID:  node2Name + "-UUID",
							Name:  node2Name,
							Ports: []string{"k8s-" + node2Name + "-UUID"},
						},
					}
					gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			},
			ginkgo.Entry("controller manages the pod node", node1Name),
			ginkgo.Entry("controller manages the egress node", node2Name),
		)

		ginkgo.DescribeTable("egressIP retry should remove OVN pod egress setup when EgressIP is deleted",
			func(controllerNodeName string) {
				fakeOvn.nodeName = controllerNodeName
				app.Action = func(*cli.Context) error {

					egressIP := net.ParseIP("0:0:0:0:0:feff:c0a8:8e0d")

					egressPod := *ovntest.NewPodWithLabels(eipNamespace, podName, node1Name, podV6IP, egressPodLabel)
					egressNamespace := ovntest.NewNamespaceWithLabels(eipNamespace, egressPodLabel)
					node1 := newNodeNotEgressableV6Only(node1Name, "0:0:0:0:0:fedf:c0a8:8e0c/64")
					node2 := newNodeNotEgressableV6Only(node2Name, "0:0:0:0:0:feff:c0a8:8e0c/64")
					node2.Annotations["k8s.ovn.org/node-transit-switch-port-ifaddr"] = `{"ipv6":"fd97::3/64"}`
					_, node1Subnet, _ := net.ParseCIDR(v6Node1Subnet)
					_, node2Subnet, _ := net.ParseCIDR(v6Node2Subnet)
					dynamicNeighRouters := "false"

					logicalRouterOptions := map[string]string{
						"dynamic_neigh_routers": dynamicNeighRouters,
					}
					fakeOvn.startWithDBSetup(
						libovsdbtest.TestSetup{
							NBData: []libovsdbtest.TestData{
								&nbdb.LogicalRouterPort{
									UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2Name + "-UUID",
									Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2Name,
									Networks: []string{nodeLogicalRouterIfAddrV6},
								},
								&nbdb.LogicalRouter{
									Name: types.OVNClusterRouter,
									UUID: types.OVNClusterRouter + "-UUID",
								},
								&nbdb.LogicalRouter{
									Name:    types.GWRouterPrefix + node1Name,
									UUID:    types.GWRouterPrefix + node1Name + "-UUID",
									Options: logicalRouterOptions,
								},
								&nbdb.LogicalRouter{
									Name:    types.GWRouterPrefix + node2Name,
									UUID:    types.GWRouterPrefix + node2Name + "-UUID",
									Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
									Nat:     nil,
									Options: logicalRouterOptions,
								},
								&nbdb.LogicalSwitchPort{
									UUID:      "k8s-" + node1Name + "-UUID",
									Name:      "k8s-" + node1Name,
									Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1Subnet).IP.String()},
								},
								&nbdb.LogicalSwitchPort{
									UUID:      "k8s-" + node2Name + "-UUID",
									Name:      "k8s-" + node2Name,
									Addresses: []string{"fe:1a:c2:3f:0e:fb " + util.GetNodeManagementIfAddr(node2Subnet).IP.String()},
								},
								&nbdb.LogicalSwitch{
									UUID:  node1Name + "-UUID",
									Name:  node1Name,
									Ports: []string{"k8s-" + node1Name + "-UUID"},
								},
								&nbdb.LogicalSwitch{
									UUID:  node2Name + "-UUID",
									Name:  node2Name,
									Ports: []string{"k8s-" + node2Name + "-UUID"},
								},
							},
						},
						&corev1.NamespaceList{
							Items: []corev1.Namespace{*egressNamespace},
						},
						&corev1.PodList{
							Items: []corev1.Pod{egressPod},
						},
						&corev1.NodeList{Items: []corev1.Node{*node1, *node2}},
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

					err := fakeOvn.controller.WatchEgressIPPods()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchEgressIPNamespaces()
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

					fakeOvn.patchEgressIPObj(node2Name, egressIPName, egressIP.String())

					// sleep long enough for TransactWithRetry to fail, causing egressnode operations to fail
					time.Sleep(config.Default.OVSDBTxnTimeout + time.Second)
					// check to see if the retry cache has an entry
					key, err := retry.GetResourceKey(&eIP)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					retry.CheckRetryObjectEventually(key, true, fakeOvn.controller.retryEgressIPs)

					connCtx, cancel := context.WithTimeout(context.Background(), config.Default.OVSDBTxnTimeout)
					defer cancel()
					resetNBClient(connCtx, fakeOvn.controller.nbClient)
					retry.SetRetryObjWithNoBackoff(key, fakeOvn.controller.retryEgressIPs)
					fakeOvn.controller.retryEgressIPs.RequestRetryObjs()
					// check the cache no longer has the entry
					retry.CheckRetryObjectEventually(key, false, fakeOvn.controller.retryEgressIPs)

					gomega.Eventually(getEgressIPStatusLen(eIP.Name)).Should(gomega.Equal(1))

					expectedNatLogicalPort := "k8s-node2"
					expectedDatabaseState := []libovsdbtest.TestData{
						getReRoutePolicy(egressPod.Status.PodIP, "6", "reroute-UUID", nodeLogicalRouterIPv6,
							getEgressIPLRPReRouteDbIDs(eIP.Name, egressPod.Namespace, egressPod.Name, IPFamilyValueV6, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs()),
						getEIPSNAT(podV6IP, egressPod.Namespace, egressPod.Name, egressIP.String(), expectedNatLogicalPort, types.DefaultNetworkControllerName),
						&nbdb.LogicalRouter{
							Name:     types.OVNClusterRouter,
							UUID:     types.OVNClusterRouter + "-UUID",
							Policies: []string{"reroute-UUID"},
						},
						&nbdb.LogicalRouterPort{
							UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2Name + "-UUID",
							Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2Name,
							Networks: []string{nodeLogicalRouterIfAddrV6},
						},
						&nbdb.LogicalRouter{
							Name:    types.GWRouterPrefix + node1Name,
							UUID:    types.GWRouterPrefix + node1Name + "-UUID",
							Options: logicalRouterOptions,
						},
						&nbdb.LogicalRouter{
							Name:    types.GWRouterPrefix + node2Name,
							UUID:    types.GWRouterPrefix + node2Name + "-UUID",
							Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
							Nat:     []string{"egressip-nat-UUID"},
							Options: logicalRouterOptions,
						},
						&nbdb.LogicalSwitchPort{
							UUID:      "k8s-" + node1Name + "-UUID",
							Name:      "k8s-" + node1Name,
							Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1Subnet).IP.String()},
						},
						&nbdb.LogicalSwitchPort{
							UUID:      "k8s-" + node2Name + "-UUID",
							Name:      "k8s-" + node2Name,
							Addresses: []string{"fe:1a:c2:3f:0e:fb " + util.GetNodeManagementIfAddr(node2Subnet).IP.String()},
						},
						&nbdb.LogicalSwitch{
							UUID:  node1Name + "-UUID",
							Name:  node1Name,
							Ports: []string{"k8s-" + node1Name + "-UUID"},
						},
						&nbdb.LogicalSwitch{
							UUID:  node2Name + "-UUID",
							Name:  node2Name,
							Ports: []string{"k8s-" + node2Name + "-UUID"},
						},
					}
					if controllerNodeName == node1Name {
						expectedDatabaseState[0] = getReRoutePolicy(podV6IP, "6", "reroute-UUID", []string{"fd97::3"},
							getEgressIPLRPReRouteDbIDs(eIP.Name, egressPod.Namespace, egressPod.Name, IPFamilyValueV6, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs())
						expectedDatabaseState[5].(*nbdb.LogicalRouter).Nat = nil
						expectedDatabaseState = append(expectedDatabaseState[:1], expectedDatabaseState[2:]...)
					} else {
						expectedDatabaseState[2].(*nbdb.LogicalRouter).Policies = []string{}
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
							Name: types.OVNClusterRouter,
							UUID: types.OVNClusterRouter + "-UUID",
						},
						&nbdb.LogicalRouterPort{
							UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2Name + "-UUID",
							Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2Name,
							Networks: []string{nodeLogicalRouterIfAddrV6},
						},
						&nbdb.LogicalRouter{
							Name:    types.GWRouterPrefix + node1Name,
							UUID:    types.GWRouterPrefix + node1Name + "-UUID",
							Options: logicalRouterOptions,
						},
						&nbdb.LogicalRouter{
							Name:    types.GWRouterPrefix + node2Name,
							UUID:    types.GWRouterPrefix + node2Name + "-UUID",
							Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
							Nat:     nil,
							Options: logicalRouterOptions,
						},
						&nbdb.LogicalSwitchPort{
							UUID:      "k8s-" + node1Name + "-UUID",
							Name:      "k8s-" + node1Name,
							Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1Subnet).IP.String()},
						},
						&nbdb.LogicalSwitchPort{
							UUID:      "k8s-" + node2Name + "-UUID",
							Name:      "k8s-" + node2Name,
							Addresses: []string{"fe:1a:c2:3f:0e:fb " + util.GetNodeManagementIfAddr(node2Subnet).IP.String()},
						},
						&nbdb.LogicalSwitch{
							UUID:  node1Name + "-UUID",
							Name:  node1Name,
							Ports: []string{"k8s-" + node1Name + "-UUID"},
						},
						&nbdb.LogicalSwitch{
							UUID:  node2Name + "-UUID",
							Name:  node2Name,
							Ports: []string{"k8s-" + node2Name + "-UUID"},
						},
					}
					gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			},
			ginkgo.Entry("controller manages the pod node", node1Name),
			ginkgo.Entry("controller manages the egress node", node2Name),
		)

		ginkgo.DescribeTable("should remove OVN pod egress setup when EgressIP stops matching",
			func(controllerNodeName string) {
				fakeOvn.nodeName = controllerNodeName
				app.Action = func(*cli.Context) error {

					egressIP := net.ParseIP("0:0:0:0:0:feff:c0a8:8e0d")

					egressPod := *ovntest.NewPodWithLabels(eipNamespace, podName, node1Name, podV6IP, egressPodLabel)
					egressNamespace := ovntest.NewNamespaceWithLabels(eipNamespace, egressPodLabel)
					node1 := newNodeNotEgressableV6Only(node1Name, "0:0:0:0:0:feff:c0a8:8e0c/64")
					node2 := newNodeNotEgressableV6Only(node2Name, "0:0:0:0:0:fedf:c0a8:8e0c/64")
					node2.Annotations["k8s.ovn.org/node-transit-switch-port-ifaddr"] = `{"ipv6":"fd97::3/64"}`
					_, node1Subnet, _ := net.ParseCIDR(v6Node1Subnet)
					_, node2Subnet, _ := net.ParseCIDR(v6Node2Subnet)
					egressIPServedPodsASv4, _ := buildEgressIPServedPodsAddressSets(nil, types.DefaultNetworkName, types.DefaultNetworkControllerName)
					dynamicNeighRouters := "false"

					logicalRouterOptions := map[string]string{
						"dynamic_neigh_routers": dynamicNeighRouters,
					}
					fakeOvn.startWithDBSetup(
						libovsdbtest.TestSetup{
							NBData: []libovsdbtest.TestData{
								&nbdb.LogicalRouterPort{
									UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2Name + "-UUID",
									Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2Name,
									Networks: []string{node2LogicalRouterIfAddrV6},
								},
								&nbdb.LogicalRouter{
									Name: types.OVNClusterRouter,
									UUID: types.OVNClusterRouter + "-UUID",
								},
								&nbdb.LogicalRouter{
									Name:    types.GWRouterPrefix + node1Name,
									UUID:    types.GWRouterPrefix + node1Name + "-UUID",
									Options: logicalRouterOptions,
								},
								&nbdb.LogicalRouter{
									Name:    types.GWRouterPrefix + node2Name,
									UUID:    types.GWRouterPrefix + node2Name + "-UUID",
									Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
									Nat:     nil,
									Options: logicalRouterOptions,
								},
								&nbdb.LogicalSwitchPort{
									UUID:      "k8s-" + node1Name + "-UUID",
									Name:      "k8s-" + node1Name,
									Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1Subnet).IP.String()},
								},
								&nbdb.LogicalSwitchPort{
									UUID:      "k8s-" + node2Name + "-UUID",
									Name:      "k8s-" + node2Name,
									Addresses: []string{"fe:1a:c2:3f:0e:fb " + util.GetNodeManagementIfAddr(node2Subnet).IP.String()},
								},
								&nbdb.LogicalSwitch{
									UUID:  node1Name + "-UUID",
									Name:  node1Name,
									Ports: []string{"k8s-" + node1Name + "-UUID"},
								},
								&nbdb.LogicalSwitch{
									UUID:  node2Name + "-UUID",
									Name:  node2Name,
									Ports: []string{"k8s-" + node2Name + "-UUID"},
								},
								egressIPServedPodsASv4,
							},
						},
						&corev1.NamespaceList{
							Items: []corev1.Namespace{*egressNamespace},
						},
						&corev1.PodList{
							Items: []corev1.Pod{egressPod},
						},
						&corev1.NodeList{Items: []corev1.Node{*node1, *node2}},
					)

					i, n, _ := net.ParseCIDR(podV6IP + "/23")
					n.IP = i
					fakeOvn.controller.logicalPortCache.add(&egressPod, "", types.DefaultNetworkName, "", nil, []*net.IPNet{n})

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

					err := fakeOvn.controller.WatchEgressIPPods()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchEgressIPNamespaces()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchEgressIP()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					_, err = fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Create(context.TODO(), &eIP, metav1.CreateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					fakeOvn.patchEgressIPObj(node2Name, egressIPName, egressIP.String())

					gomega.Eventually(getEgressIPStatusLen(eIP.Name)).Should(gomega.Equal(1))

					expectedNatLogicalPort := "k8s-node2"
					expectedDatabaseState := []libovsdbtest.TestData{
						getReRoutePolicy(egressPod.Status.PodIP, "6", "reroute-UUID", node2LogicalRouterIPv6,
							getEgressIPLRPReRouteDbIDs(eIP.Name, egressPod.Namespace, egressPod.Name, IPFamilyValueV6, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs()),
						&nbdb.LogicalRouter{
							Name:     types.OVNClusterRouter,
							UUID:     types.OVNClusterRouter + "-UUID",
							Policies: []string{"reroute-UUID"},
						},
						&nbdb.LogicalRouterPort{
							UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2Name + "-UUID",
							Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2Name,
							Networks: []string{node2LogicalRouterIfAddrV6},
						},
						&nbdb.NAT{
							UUID:        "egressip-nat-UUID",
							LogicalIP:   podV6IP,
							ExternalIP:  egressIP.String(),
							ExternalIDs: getEgressIPNATDbIDs(egressIPName, egressPod.Namespace, egressPod.Name, IPFamilyValueV6, fakeOvn.controller.controllerName).GetExternalIDs(),
							Type:        nbdb.NATTypeSNAT,
							LogicalPort: &expectedNatLogicalPort,
							Options: map[string]string{
								"stateless": "false",
							},
						},
						&nbdb.LogicalRouter{
							Name:    types.GWRouterPrefix + node1Name,
							UUID:    types.GWRouterPrefix + node1Name + "-UUID",
							Options: logicalRouterOptions,
						},
						&nbdb.LogicalRouter{
							Name:    types.GWRouterPrefix + node2Name,
							UUID:    types.GWRouterPrefix + node2Name + "-UUID",
							Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
							Nat:     []string{"egressip-nat-UUID"},
							Options: logicalRouterOptions,
						},
						&nbdb.LogicalSwitchPort{
							UUID:      "k8s-" + node1Name + "-UUID",
							Name:      "k8s-" + node1Name,
							Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1Subnet).IP.String()},
						},
						&nbdb.LogicalSwitchPort{
							UUID:      "k8s-" + node2Name + "-UUID",
							Name:      "k8s-" + node2Name,
							Addresses: []string{"fe:1a:c2:3f:0e:fb " + util.GetNodeManagementIfAddr(node2Subnet).IP.String()},
						},
						&nbdb.LogicalSwitch{
							UUID:  node1Name + "-UUID",
							Name:  node1Name,
							Ports: []string{"k8s-" + node1Name + "-UUID"},
						},
						&nbdb.LogicalSwitch{
							UUID:  node2Name + "-UUID",
							Name:  node2Name,
							Ports: []string{"k8s-" + node2Name + "-UUID"},
						},
						egressIPServedPodsASv4,
					}
					if controllerNodeName == node1Name {
						expectedDatabaseState[0] = getReRoutePolicy(egressPod.Status.PodIP, "6", "reroute-UUID", []string{"fd97::3"},
							getEgressIPLRPReRouteDbIDs(eIP.Name, egressPod.Namespace, egressPod.Name, IPFamilyValueV6, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs())
						expectedDatabaseState[5].(*nbdb.LogicalRouter).Nat = nil
						expectedDatabaseState = append(expectedDatabaseState[:3], expectedDatabaseState[4:]...)
					} else {
						expectedDatabaseState[1].(*nbdb.LogicalRouter).Policies = []string{}
						expectedDatabaseState = expectedDatabaseState[1:]
					}
					gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

					egressIPs, nodes := getEgressIPStatus(eIP.Name)
					gomega.Expect(nodes[0]).To(gomega.Equal(node2Name))
					gomega.Expect(egressIPs[0]).To(gomega.Equal(egressIP.String()))

					namespaceUpdate := ovntest.NewNamespace(eipNamespace)

					_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Update(context.TODO(), namespaceUpdate, metav1.UpdateOptions{})
					gomega.Expect(err).ToNot(gomega.HaveOccurred())
					gomega.Eventually(getEgressIPStatusLen(eIP.Name)).Should(gomega.Equal(1))

					expectedDatabaseState = []libovsdbtest.TestData{
						&nbdb.LogicalRouter{
							Name: types.OVNClusterRouter,
							UUID: types.OVNClusterRouter + "-UUID",
						},
						&nbdb.LogicalRouterPort{
							UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2Name + "-UUID",
							Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2Name,
							Networks: []string{node2LogicalRouterIfAddrV6},
						},
						&nbdb.LogicalRouter{
							Name:    types.GWRouterPrefix + node1Name,
							UUID:    types.GWRouterPrefix + node1Name + "-UUID",
							Options: logicalRouterOptions,
						},
						&nbdb.LogicalRouter{
							Name:    types.GWRouterPrefix + node2Name,
							UUID:    types.GWRouterPrefix + node2Name + "-UUID",
							Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
							Nat:     nil,
							Options: logicalRouterOptions,
						},
						&nbdb.LogicalSwitchPort{
							UUID:      "k8s-" + node1Name + "-UUID",
							Name:      "k8s-" + node1Name,
							Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1Subnet).IP.String()},
						},
						&nbdb.LogicalSwitchPort{
							UUID:      "k8s-" + node2Name + "-UUID",
							Name:      "k8s-" + node2Name,
							Addresses: []string{"fe:1a:c2:3f:0e:fb " + util.GetNodeManagementIfAddr(node2Subnet).IP.String()},
						},
						&nbdb.LogicalSwitch{
							UUID:  node1Name + "-UUID",
							Name:  node1Name,
							Ports: []string{"k8s-" + node1Name + "-UUID"},
						},
						&nbdb.LogicalSwitch{
							UUID:  node2Name + "-UUID",
							Name:  node2Name,
							Ports: []string{"k8s-" + node2Name + "-UUID"},
						},
						egressIPServedPodsASv4,
					}
					gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			},
			ginkgo.Entry("controller manages the pod node", node1Name),
			ginkgo.Entry("controller manages the egress node", node2Name),
		)

		ginkgo.It("should not remove OVN pod egress setup when EgressIP stops matching, but pod never had any IP to begin with", func() {
			app.Action = func(*cli.Context) error {

				egressIP := net.ParseIP("0:0:0:0:0:feff:c0a8:8e0d")

				egressPod := *ovntest.NewPodWithLabels(eipNamespace, podName, node1Name, "", egressPodLabel)
				egressNamespace := ovntest.NewNamespaceWithLabels(eipNamespace, egressPodLabel)
				fakeOvn.startWithDBSetup(clusterRouterDbSetup,
					&corev1.NamespaceList{
						Items: []corev1.Namespace{*egressNamespace},
					},
					&corev1.PodList{
						Items: []corev1.Pod{egressPod},
					},
					&corev1.NodeList{Items: []corev1.Node{*newNodeNotEgressableV6Only(node2Name, "0:0:0:0:0:feff:c0a8:8e0c/64"),
						*newNodeNotEgressableV6Only(node1Name, "0:0:0:0:0:fedf:c0a8:8e0c/64")}},
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

				err := fakeOvn.controller.WatchEgressIPPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIPNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				_, err = fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Create(context.TODO(), &eIP, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				fakeOvn.patchEgressIPObj(node2Name, egressIPName, egressIP.String())

				gomega.Eventually(getEgressIPStatusLen(eIP.Name)).Should(gomega.Equal(1))

				egressIPs, nodes := getEgressIPStatus(eIP.Name)
				gomega.Expect(nodes[0]).To(gomega.Equal(node2Name))
				gomega.Expect(egressIPs[0]).To(gomega.Equal(egressIP.String()))

				namespaceUpdate := ovntest.NewNamespace(eipNamespace)

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

		ginkgo.DescribeTable("should update OVN on EgressIP .spec.egressips change",
			func(controllerNodeName string) {
				app.Action = func(*cli.Context) error {

					egressIP1 := "192.168.126.101"
					egressIP2 := "192.168.126.102"
					egressIP3 := "192.168.126.103"
					fakeOvn.nodeName = controllerNodeName
					node1Local := controllerNodeName == node1Name
					node2Local := controllerNodeName == node2Name
					node1IPv4 := "192.168.126.202"
					node1IPv4CIDR := node1IPv4 + "/24"
					node2IPv4 := "192.168.126.51"
					node2IPv4CIDR := node2IPv4 + "/24"
					_, node1Subnet, _ := net.ParseCIDR(v4Node1Subnet)
					egressPod := *ovntest.NewPodWithLabels(eipNamespace, podName, node1Name, podV4IP, egressPodLabel)
					egressNamespace := ovntest.NewNamespace(eipNamespace)
					annotations := map[string]string{
						"k8s.ovn.org/node-primary-ifaddr":             fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4CIDR, ""),
						"k8s.ovn.org/node-subnets":                    fmt.Sprintf("{\"default\":\"%s\"}", v4Node1Subnet),
						"k8s.ovn.org/node-transit-switch-port-ifaddr": "{\"ipv4\":\"100.88.0.2/16\"}", // used only for ic=true test
						util.OVNNodeHostCIDRs:                         fmt.Sprintf("[\"%s\"]", node1IPv4CIDR),
						util.OvnNodeID:                                "2",
					}
					labels := map[string]string{
						"k8s.ovn.org/egress-assignable": "",
					}
					node1 := getNodeObj(node1Name, annotations, labels)
					annotations = map[string]string{
						"k8s.ovn.org/node-primary-ifaddr":             fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node2IPv4CIDR, ""),
						"k8s.ovn.org/node-subnets":                    fmt.Sprintf("{\"default\":\"%s\"}", v4Node2Subnet),
						"k8s.ovn.org/node-transit-switch-port-ifaddr": "{\"ipv4\":\"100.88.0.3/16\"}", // used only for ic=true test
						util.OVNNodeHostCIDRs:                         fmt.Sprintf("[\"%s\"]", node2IPv4CIDR),
						util.OvnNodeID:                                "3",
					}
					node2 := getNodeObj(node2Name, annotations, labels)
					_, node2Subnet, _ := net.ParseCIDR(v4Node2Subnet)

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
					node1Switch := &nbdb.LogicalSwitch{
						UUID:  node1Name + "-UUID",
						Name:  node1Name,
						Ports: []string{"k8s-" + node1Name + "-UUID"},
					}
					node2Switch := &nbdb.LogicalSwitch{
						UUID:  node2Name + "-UUID",
						Name:  node2Name,
						Ports: []string{"k8s-" + node2Name + "-UUID"},
					}
					dynamicNeighRouters := "false"

					logicalRouterOptions := map[string]string{
						"dynamic_neigh_routers": dynamicNeighRouters,
					}
					fakeOvn.startWithDBSetup(
						libovsdbtest.TestSetup{
							NBData: []libovsdbtest.TestData{
								&nbdb.LogicalRouterPort{
									UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID",
									Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name,
									Networks: []string{node2LogicalRouterIfAddrV4},
								},
								&nbdb.LogicalRouterPort{
									UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID",
									Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name,
									Networks: []string{nodeLogicalRouterIfAddrV4},
								},
								&nbdb.LogicalRouter{
									Name: types.OVNClusterRouter,
									UUID: types.OVNClusterRouter + "-UUID",
								},
								&nbdb.LogicalRouter{
									Name:    types.GWRouterPrefix + node1.Name,
									UUID:    types.GWRouterPrefix + node1.Name + "-UUID",
									Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID"},
									Options: logicalRouterOptions,
								},
								&nbdb.LogicalRouter{
									Name:    types.GWRouterPrefix + node2.Name,
									UUID:    types.GWRouterPrefix + node2.Name + "-UUID",
									Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID"},
									Options: logicalRouterOptions,
								},
								&nbdb.LogicalSwitchPort{
									UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID",
									Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
									Type: "router",
									Options: map[string]string{
										libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
										"nat-addresses":             "router",
										"exclude-lb-vips-from-garp": "true",
									},
								},
								&nbdb.LogicalSwitchPort{
									UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID",
									Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
									Type: "router",
									Options: map[string]string{
										libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
										"nat-addresses":             "router",
										"exclude-lb-vips-from-garp": "true",
									},
								},
								&nbdb.LogicalSwitchPort{
									UUID:      "k8s-" + node1Name + "-UUID",
									Name:      "k8s-" + node1Name,
									Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1Subnet).IP.String()},
								},
								&nbdb.LogicalSwitchPort{
									UUID:      "k8s-" + node2Name + "-UUID",
									Name:      "k8s-" + node2Name,
									Addresses: []string{"fe:1a:c2:3f:0e:fb " + util.GetNodeManagementIfAddr(node2Subnet).IP.String()},
								},
								&nbdb.LogicalSwitch{
									UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
									Name:  types.ExternalSwitchPrefix + node1Name,
									Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
								},
								&nbdb.LogicalSwitch{
									UUID:  types.ExternalSwitchPrefix + node2Name + "-UUID",
									Name:  types.ExternalSwitchPrefix + node2Name,
									Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
								},
								node1Switch,
								node2Switch,
							},
						},
						&corev1.NodeList{
							Items: []corev1.Node{node1, node2},
						},
						&corev1.NamespaceList{
							Items: []corev1.Namespace{*egressNamespace},
						},
						&corev1.PodList{
							Items: []corev1.Pod{egressPod},
						})

					i, n, _ := net.ParseCIDR(podV4IP + "/23")
					n.IP = i
					fakeOvn.controller.logicalPortCache.add(&egressPod, "", types.DefaultNetworkName, "", nil, []*net.IPNet{n})
					err := fakeOvn.controller.WatchEgressIPPods()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchEgressIPNamespaces()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchEgressNodes()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchEgressIP()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					_, err = fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Create(context.TODO(), &eIP, metav1.CreateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					// NOTE: Cluster manager is the one who patches the egressIP object.
					// For the sake of unit testing the EgressIP controller we need to patch the EgressIP object manually.
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
					err = fakeOvn.controller.eIPC.patchReplaceEgressIPStatus(eIP.Name, status)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					gomega.Eventually(getEgressIPStatusLen(eIP.Name)).Should(gomega.Equal(2))
					egressIPs, nodes := getEgressIPStatus(eIP.Name)
					assignmentNode1, assignmentNode2 := nodes[0], nodes[1]
					assignedEgressIP1, assignedEgressIP2 := egressIPs[0], egressIPs[1]

					expectedNatLogicalPort1 := fmt.Sprintf("k8s-%s", assignmentNode1)
					expectedNatLogicalPort2 := fmt.Sprintf("k8s-%s", assignmentNode2)
					natEIP1 := &nbdb.NAT{
						UUID:        "egressip-nat-1-UUID",
						LogicalIP:   podV4IP,
						ExternalIP:  assignedEgressIP1,
						ExternalIDs: getEgressIPNATDbIDs(egressIPName, egressPod.Namespace, egressPod.Name, IPFamilyValueV4, fakeOvn.controller.controllerName).GetExternalIDs(),
						Type:        nbdb.NATTypeSNAT,
						LogicalPort: &expectedNatLogicalPort1,
						Options: map[string]string{
							"stateless": "false",
						},
					}
					natEIP2 := &nbdb.NAT{
						UUID:        "egressip-nat-2-UUID",
						LogicalIP:   podV4IP,
						ExternalIP:  assignedEgressIP2,
						ExternalIDs: getEgressIPNATDbIDs(egressIPName, egressPod.Namespace, egressPod.Name, IPFamilyValueV4, fakeOvn.controller.controllerName).GetExternalIDs(),
						Type:        nbdb.NATTypeSNAT,
						LogicalPort: &expectedNatLogicalPort2,
						Options: map[string]string{
							"stateless": "false",
						},
					}

					egressSVCServedPodsASv4, _ := buildEgressServiceAddressSets(nil)
					servedPodsIPs := []string{podV4IP}
					if !node1Local {
						servedPodsIPs = nil
					}
					egressIPServedPodsASv4, _ := buildEgressIPServedPodsAddressSets(servedPodsIPs, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName)
					egressNodeIPsASv4, _ := buildEgressIPNodeAddressSets([]string{node1IPv4, node2IPv4})

					expectedDatabaseState := []libovsdbtest.TestData{
						getReRoutePolicy(egressPod.Status.PodIP, "4", "reroute-UUID", []string{"100.64.0.2", "100.64.0.3"},
							getEgressIPLRPReRouteDbIDs(eIP.Name, egressPod.Namespace, egressPod.Name, IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs()),
						&nbdb.LogicalRouterPolicy{
							Priority:    types.DefaultNoRereoutePriority,
							Match:       "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
							Action:      nbdb.LogicalRouterPolicyActionAllow,
							UUID:        "default-no-reroute-UUID",
							ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
						},
						&nbdb.LogicalRouterPolicy{
							Priority:    types.DefaultNoRereoutePriority,
							Match:       fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
							Action:      nbdb.LogicalRouterPolicyActionAllow,
							UUID:        "no-reroute-service-UUID",
							ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
						},
						&nbdb.LogicalRouter{
							Name:    types.GWRouterPrefix + assignmentNode1,
							UUID:    types.GWRouterPrefix + assignmentNode1 + "-UUID",
							Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID"},
							Options: logicalRouterOptions,
						},
						&nbdb.LogicalRouter{
							Name:    types.GWRouterPrefix + assignmentNode2,
							UUID:    types.GWRouterPrefix + assignmentNode2 + "-UUID",
							Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID"},
							Options: logicalRouterOptions,
						},
						&nbdb.LogicalRouter{
							Name: types.OVNClusterRouter,
							UUID: types.OVNClusterRouter + "-UUID",
							Policies: []string{"reroute-UUID", "default-no-reroute-UUID", "no-reroute-service-UUID",
								"default-no-reroute-node-UUID", "default-no-reroute-reply-traffic"},
						},
						&nbdb.LogicalRouterPort{
							UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID",
							Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name,
							Networks: []string{"100.64.0.3/29"},
						},
						&nbdb.LogicalRouterPort{
							UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID",
							Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name,
							Networks: []string{"100.64.0.2/29"},
						},
						&nbdb.LogicalSwitchPort{
							UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID",
							Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
							Type: "router",
							Options: map[string]string{
								libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
								"nat-addresses":             "router",
								"exclude-lb-vips-from-garp": "true",
							},
						},
						&nbdb.LogicalSwitchPort{
							UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID",
							Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
							Type: "router",
							Options: map[string]string{
								libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
								"nat-addresses":             "router",
								"exclude-lb-vips-from-garp": "true",
							},
						},
						&nbdb.LogicalRouterPolicy{
							Priority: types.DefaultNoRereoutePriority,
							Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
								egressIPServedPodsASv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
							Action:      nbdb.LogicalRouterPolicyActionAllow,
							UUID:        "default-no-reroute-node-UUID",
							Options:     map[string]string{"pkt_mark": types.EgressIPNodeConnectionMark},
							ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
						},
						&nbdb.LogicalSwitchPort{
							UUID:      "k8s-" + node1Name + "-UUID",
							Name:      "k8s-" + node1Name,
							Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1Subnet).IP.String()},
						},
						&nbdb.LogicalSwitchPort{
							UUID:      "k8s-" + node2Name + "-UUID",
							Name:      "k8s-" + node2Name,
							Addresses: []string{"fe:1a:c2:3f:0e:fb " + util.GetNodeManagementIfAddr(node2Subnet).IP.String()},
						},
						&nbdb.LogicalSwitch{
							UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
							Name:  types.ExternalSwitchPrefix + node1Name,
							Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
						},
						&nbdb.LogicalSwitch{
							UUID:  types.ExternalSwitchPrefix + node2Name + "-UUID",
							Name:  types.ExternalSwitchPrefix + node2Name,
							Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
						},
						getNoReRouteReplyTrafficPolicy(types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
						node1Switch,
						node2Switch,
						getDefaultQoSRule(false, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
						egressSVCServedPodsASv4,
						egressIPServedPodsASv4,
						egressNodeIPsASv4,
					}
					if node1Local {
						expectedDatabaseState[3].(*nbdb.LogicalRouter).Nat = []string{"egressip-nat-1-UUID"}
						expectedDatabaseState = append(expectedDatabaseState, natEIP1)
						node1Switch.QOSRules = []string{"default-QoS-UUID"}
					}
					if node1Local {
						expectedDatabaseState = append(expectedDatabaseState, getReRouteStaticRoute(v4ClusterSubnet, nodeLogicalRouterIPv4[0]))
						expectedDatabaseState[5].(*nbdb.LogicalRouter).StaticRoutes = []string{"reroute-static-route-UUID"}
					}
					if node2Local {
						expectedDatabaseState[4].(*nbdb.LogicalRouter).Nat = []string{"egressip-nat-2-UUID"}
						expectedDatabaseState = append(expectedDatabaseState, natEIP2)
						node2Switch.QOSRules = []string{"default-QoS-UUID"}
					}
					if node2Local {
						expectedDatabaseState = append(expectedDatabaseState, getReRouteStaticRoute(v4ClusterSubnet, node2LogicalRouterIPv4[0]))
						expectedDatabaseState[5].(*nbdb.LogicalRouter).StaticRoutes = []string{"reroute-static-route-UUID"}
					}
					if !node2Local {
						// the policy reroute will have its second nexthop as transit switchIP
						// so the one with join switchIP is where podNode == egressNode and one with transitIP is where podNode != egressNode
						expectedDatabaseState[0].(*nbdb.LogicalRouterPolicy).Nexthops = []string{"100.64.0.2", "100.88.0.3"}
					}
					if !node1Local {
						expectedDatabaseState[5].(*nbdb.LogicalRouter).Policies = []string{"default-no-reroute-UUID", "no-reroute-service-UUID",
							"default-no-reroute-node-UUID", "default-no-reroute-reply-traffic"}
						expectedDatabaseState = expectedDatabaseState[1:] // policy is not visible since podNode is remote
					}
					gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

					latest, err := fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Get(context.TODO(), eIP.Name, metav1.GetOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					latest.Spec.EgressIPs = []string{egressIP3, egressIP2}
					_, err = fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Update(context.TODO(), latest, metav1.UpdateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					// NOTE: Cluster manager is the one who patches the egressIP object.
					// For the sake of unit testing the EgressIP controller we need to patch the EgressIP object manually.
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
					err = fakeOvn.controller.eIPC.patchReplaceEgressIPStatus(eIP.Name, status)
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
						getReRoutePolicy(egressPod.Status.PodIP, "4", "reroute-UUID", []string{"100.64.0.2", "100.64.0.3"},
							getEgressIPLRPReRouteDbIDs(eIP.Name, egressPod.Namespace, egressPod.Name, IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs()),
						&nbdb.LogicalRouterPolicy{
							Priority:    types.DefaultNoRereoutePriority,
							Match:       "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
							Action:      nbdb.LogicalRouterPolicyActionAllow,
							UUID:        "default-no-reroute-UUID",
							ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
						},
						&nbdb.LogicalRouterPolicy{
							Priority:    types.DefaultNoRereoutePriority,
							Match:       fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
							Action:      nbdb.LogicalRouterPolicyActionAllow,
							UUID:        "no-reroute-service-UUID",
							ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
						},
						&nbdb.LogicalRouter{
							Name:    types.GWRouterPrefix + assignmentNode1,
							UUID:    types.GWRouterPrefix + assignmentNode1 + "-UUID",
							Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID"},
							Options: logicalRouterOptions,
						},
						&nbdb.LogicalRouter{
							Name:    types.GWRouterPrefix + assignmentNode2,
							UUID:    types.GWRouterPrefix + assignmentNode2 + "-UUID",
							Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID"},
							Options: logicalRouterOptions,
						},
						&nbdb.LogicalRouter{
							Name: types.OVNClusterRouter,
							UUID: types.OVNClusterRouter + "-UUID",
							Policies: []string{"reroute-UUID", "default-no-reroute-UUID", "no-reroute-service-UUID",
								"default-no-reroute-node-UUID", "default-no-reroute-reply-traffic"},
						},
						&nbdb.LogicalRouterPort{
							UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID",
							Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name,
							Networks: []string{"100.64.0.3/29"},
						},
						&nbdb.LogicalRouterPort{
							UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID",
							Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name,
							Networks: []string{"100.64.0.2/29"},
						},
						&nbdb.LogicalSwitchPort{
							UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID",
							Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
							Type: "router",
							Options: map[string]string{
								libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
								"nat-addresses":             "router",
								"exclude-lb-vips-from-garp": "true",
							},
						},
						&nbdb.LogicalSwitchPort{
							UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID",
							Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
							Type: "router",
							Options: map[string]string{
								libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
								"nat-addresses":             "router",
								"exclude-lb-vips-from-garp": "true",
							},
						},
						&nbdb.LogicalRouterPolicy{
							Priority: types.DefaultNoRereoutePriority,
							Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
								egressIPServedPodsASv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
							Action:      nbdb.LogicalRouterPolicyActionAllow,
							UUID:        "default-no-reroute-node-UUID",
							Options:     map[string]string{"pkt_mark": types.EgressIPNodeConnectionMark},
							ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
						},
						&nbdb.LogicalSwitchPort{
							UUID:      "k8s-" + node1Name + "-UUID",
							Name:      "k8s-" + node1Name,
							Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1Subnet).IP.String()},
						},
						&nbdb.LogicalSwitchPort{
							UUID:      "k8s-" + node2Name + "-UUID",
							Name:      "k8s-" + node2Name,
							Addresses: []string{"fe:1a:c2:3f:0e:fb " + util.GetNodeManagementIfAddr(node2Subnet).IP.String()},
						},
						&nbdb.LogicalSwitch{
							UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
							Name:  types.ExternalSwitchPrefix + node1Name,
							Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
						},
						&nbdb.LogicalSwitch{
							UUID:  types.ExternalSwitchPrefix + node2Name + "-UUID",
							Name:  types.ExternalSwitchPrefix + node2Name,
							Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
						},
						getNoReRouteReplyTrafficPolicy(types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
						node1Switch,
						node2Switch,
						getDefaultQoSRule(false, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
						egressSVCServedPodsASv4,
						egressIPServedPodsASv4,
						egressNodeIPsASv4,
					}
					if node1Local {
						expectedDatabaseState[3].(*nbdb.LogicalRouter).Nat = []string{"egressip-nat-1-UUID"}
						natEIP1.ExternalIP = assignedEgressIP1
						expectedDatabaseState = append(expectedDatabaseState, natEIP1)
						node1Switch.QOSRules = []string{"default-QoS-UUID"}
					}
					if node1Local {
						expectedDatabaseState = append(expectedDatabaseState, getReRouteStaticRoute(v4ClusterSubnet, nodeLogicalRouterIPv4[0]))
						expectedDatabaseState[5].(*nbdb.LogicalRouter).StaticRoutes = []string{"reroute-static-route-UUID"}
					}
					if node2Local {
						expectedDatabaseState[4].(*nbdb.LogicalRouter).Nat = []string{"egressip-nat-2-UUID"}
						natEIP2.ExternalIP = assignedEgressIP2
						expectedDatabaseState = append(expectedDatabaseState, natEIP2)
						node2Switch.QOSRules = []string{"default-QoS-UUID"}
					}
					if node2Local {
						expectedDatabaseState = append(expectedDatabaseState, getReRouteStaticRoute(v4ClusterSubnet, node2LogicalRouterIPv4[0]))
						expectedDatabaseState[5].(*nbdb.LogicalRouter).StaticRoutes = []string{"reroute-static-route-UUID"}
					}
					if !node2Local {
						// the policy reroute will have its second nexthop as transit switchIP
						// so the one with join switchIP is where podNode == egressNode and one with transitIP is where podNode != egressNode
						expectedDatabaseState[0].(*nbdb.LogicalRouterPolicy).Nexthops = []string{"100.64.0.2", "100.88.0.3"}
					}
					if !node1Local {
						expectedDatabaseState[5].(*nbdb.LogicalRouter).Policies = []string{"default-no-reroute-UUID", "no-reroute-service-UUID",
							"default-no-reroute-node-UUID", "default-no-reroute-reply-traffic"}
						expectedDatabaseState = expectedDatabaseState[1:] // policy is not visible since podNode is remote
					}
					gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			},
			ginkgo.Entry("controller manages the pod node", node1Name),
			ginkgo.Entry("controller manages the egress node", node2Name),
		)

		ginkgo.DescribeTable("should delete and re-create and delete", func(deleteNamespaceFirst bool) {
			fakeOvn.nodeName = node2Name
			app.Action = func(*cli.Context) error {

				egressIP := net.ParseIP("0:0:0:0:0:feff:c0a8:8e0d")
				updatedEgressIP := net.ParseIP("0:0:0:0:0:feff:c0a8:8ffd")

				egressPod := *ovntest.NewPodWithLabels(eipNamespace, podName, node1Name, podV6IP, egressPodLabel)
				egressNamespace := ovntest.NewNamespaceWithLabels(eipNamespace, egressPodLabel)
				_, node1Subnet, _ := net.ParseCIDR(v6Node1Subnet)
				_, node2Subnet, _ := net.ParseCIDR(v6Node2Subnet)
				egressIPServedPodsASv4, _ := buildEgressIPServedPodsAddressSets(nil, types.DefaultNetworkName, types.DefaultNetworkControllerName)
				dynamicNeighRouters := "false"

				logicalRouterOptions := map[string]string{
					"dynamic_neigh_routers": dynamicNeighRouters,
				}
				fakeOvn.startWithDBSetup(
					libovsdbtest.TestSetup{
						NBData: []libovsdbtest.TestData{
							&nbdb.LogicalRouterPort{
								UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2Name + "-UUID",
								Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2Name,
								Networks: []string{node2LogicalRouterIfAddrV6},
							},
							&nbdb.LogicalRouter{
								Name: types.OVNClusterRouter,
								UUID: types.OVNClusterRouter + "-UUID",
							},
							&nbdb.LogicalRouter{
								Name:    types.GWRouterPrefix + node1Name,
								UUID:    types.GWRouterPrefix + node1Name + "-UUID",
								Options: logicalRouterOptions,
							},
							&nbdb.LogicalRouter{
								Name:    types.GWRouterPrefix + node2Name,
								UUID:    types.GWRouterPrefix + node2Name + "-UUID",
								Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
								Nat:     nil,
								Options: logicalRouterOptions,
							},
							&nbdb.LogicalSwitchPort{
								UUID:      "k8s-" + node1Name + "-UUID",
								Name:      "k8s-" + node1Name,
								Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1Subnet).IP.String()},
							},
							&nbdb.LogicalSwitchPort{
								UUID:      "k8s-" + node2Name + "-UUID",
								Name:      "k8s-" + node2Name,
								Addresses: []string{"fe:1a:c2:3f:0e:fb " + util.GetNodeManagementIfAddr(node2Subnet).IP.String()},
							},
							&nbdb.LogicalSwitch{
								UUID:  node1Name + "-UUID",
								Name:  node1Name,
								Ports: []string{"k8s-" + node1Name + "-UUID"},
							},
							&nbdb.LogicalSwitch{
								UUID:  node2Name + "-UUID",
								Name:  node2Name,
								Ports: []string{"k8s-" + node2Name + "-UUID"},
							},
							egressIPServedPodsASv4,
						},
					},
					&corev1.NamespaceList{
						Items: []corev1.Namespace{*egressNamespace},
					},
					&corev1.PodList{
						Items: []corev1.Pod{egressPod},
					},
					&corev1.NodeList{Items: []corev1.Node{*newNodeNotEgressableV6Only(node2Name, "0:0:0:0:0:feff:c0a8:8e0c/64"),
						*newNodeNotEgressableV6Only(node1Name, "0:0:0:0:0:fedf:c0a8:8e0c/64")}},
				)

				i, n, _ := net.ParseCIDR(podV6IP + "/23")
				n.IP = i
				fakeOvn.controller.logicalPortCache.add(&egressPod, "", types.DefaultNetworkName, "", nil, []*net.IPNet{n})
				fakeOvn.controller.logicalPortCache.add(&egressPod, "", types.DefaultNetworkName, "", nil, []*net.IPNet{n})

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

				err := fakeOvn.controller.WatchEgressIPPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIPNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				_, err = fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Create(context.TODO(), &eIP, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				fakeOvn.patchEgressIPObj(node2Name, egressIPName, egressIP.String())

				gomega.Eventually(getEgressIPStatusLen(eIP.Name)).Should(gomega.Equal(1))

				expectedNatLogicalPort := "k8s-node2"
				expectedNAT := &nbdb.NAT{
					UUID:        "egressip-nat-UUID",
					LogicalIP:   podV6IP,
					ExternalIP:  egressIP.String(),
					ExternalIDs: getEgressIPNATDbIDs(egressIPName, egressPod.Namespace, egressPod.Name, IPFamilyValueV6, fakeOvn.controller.controllerName).GetExternalIDs(),
					Type:        nbdb.NATTypeSNAT,
					LogicalPort: &expectedNatLogicalPort,
					Options: map[string]string{
						"stateless": "false",
					},
				}

				expectedDatabaseState := []libovsdbtest.TestData{
					&nbdb.LogicalRouter{
						Name: types.OVNClusterRouter,
						UUID: types.OVNClusterRouter + "-UUID",
					},
					&nbdb.LogicalRouterPort{
						UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2Name + "-UUID",
						Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2Name,
						Networks: []string{node2LogicalRouterIfAddrV6},
					},
					expectedNAT,
					&nbdb.LogicalRouter{
						Name:    types.GWRouterPrefix + node1Name,
						UUID:    types.GWRouterPrefix + node1Name + "-UUID",
						Options: logicalRouterOptions,
					},
					&nbdb.LogicalRouter{
						Name:    types.GWRouterPrefix + node2Name,
						UUID:    types.GWRouterPrefix + node2Name + "-UUID",
						Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
						Nat:     []string{"egressip-nat-UUID"},
						Options: logicalRouterOptions,
					},
					&nbdb.LogicalSwitchPort{
						UUID:      "k8s-" + node1Name + "-UUID",
						Name:      "k8s-" + node1Name,
						Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1Subnet).IP.String()},
					},
					&nbdb.LogicalSwitchPort{
						UUID:      "k8s-" + node2Name + "-UUID",
						Name:      "k8s-" + node2Name,
						Addresses: []string{"fe:1a:c2:3f:0e:fb " + util.GetNodeManagementIfAddr(node2Subnet).IP.String()},
					},
					&nbdb.LogicalSwitch{
						UUID:  node1Name + "-UUID",
						Name:  node1Name,
						Ports: []string{"k8s-" + node1Name + "-UUID"},
					},
					&nbdb.LogicalSwitch{
						UUID:  node2Name + "-UUID",
						Name:  node2Name,
						Ports: []string{"k8s-" + node2Name + "-UUID"},
					},
					egressIPServedPodsASv4,
				}
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				egressIPs, nodes := getEgressIPStatus(eIP.Name)
				gomega.Expect(nodes[0]).To(gomega.Equal(node2Name))
				gomega.Expect(egressIPs[0]).To(gomega.Equal(egressIP.String()))

				eIPUpdate, err := fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Get(context.TODO(), eIP.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// For pod selector, let's test a use case where the selector only looks for keys that are not
				// present in the pod. That should be properly handled in deletion cases, where the match is
				// true, but the "new" pod object is nil.
				eIPUpdate.Spec = egressipv1.EgressIPSpec{
					EgressIPs: []string{
						updatedEgressIP.String(),
					},
					PodSelector: metav1.LabelSelector{
						MatchExpressions: []metav1.LabelSelectorRequirement{
							{
								Key:      "someNonExistingKey",
								Operator: metav1.LabelSelectorOpDoesNotExist,
							},
							{
								Key:      "egress",
								Operator: metav1.LabelSelectorOpNotIn,
								Values:   []string{"not_needed", "needle", "haystack"},
							},
						},
					},
					NamespaceSelector: metav1.LabelSelector{
						MatchLabels: egressPodLabel,
					},
				}

				_, err = fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Update(context.TODO(), eIPUpdate, metav1.UpdateOptions{})
				gomega.Expect(err).ToNot(gomega.HaveOccurred())

				fakeOvn.patchEgressIPObj(node2Name, egressIPName, updatedEgressIP.String())
				expectedNAT.ExternalIP = updatedEgressIP.String()
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				gomega.Eventually(func() []string {
					egressIPs, _ = getEgressIPStatus(eIP.Name)
					return egressIPs
				}).Should(gomega.ContainElement(updatedEgressIP.String()))

				gomega.Expect(nodes[0]).To(gomega.Equal(node2Name))

				gomega.Eventually(func() *podAssignmentState {
					return getPodAssignmentState(&egressPod)
				}).ShouldNot(gomega.BeNil())
				if deleteNamespaceFirst {
					// Simulate the namespace informer observing deletion before the pod informer.
					// The namespace handler cannot clean this pod after it disappears from the pod
					// informer, so the pod delete handler must use its cached assignment.
					err = fakeOvn.controller.watchFactory.NamespaceInformer().Informer().GetStore().Delete(egressNamespace)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					_, err = fakeOvn.controller.watchFactory.GetNamespace(egressNamespace.Name)
					gomega.Expect(apierrors.IsNotFound(err)).To(gomega.BeTrue())
					ginkgo.By("Deleting the pod after its namespace should update EIPs SNATs")
				} else {
					ginkgo.By("Deleting the pod should update EIPs SNATs")
				}

				// Remove pod and ensure LRP, SNAT, and cached assignment are removed as well.
				err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(eipNamespace).Delete(context.TODO(), podName, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectedDatabaseState2 := []libovsdbtest.TestData{
					&nbdb.LogicalRouter{
						Name:     types.OVNClusterRouter,
						UUID:     types.OVNClusterRouter + "-UUID",
						Policies: []string{},
					},
					&nbdb.LogicalRouterPort{
						UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2Name + "-UUID",
						Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2Name,
						Networks: []string{node2LogicalRouterIfAddrV6},
					},
					&nbdb.LogicalRouter{
						Name:    types.GWRouterPrefix + node1Name,
						UUID:    types.GWRouterPrefix + node1Name + "-UUID",
						Options: logicalRouterOptions,
					},
					&nbdb.LogicalRouter{
						Name:    types.GWRouterPrefix + node2Name,
						UUID:    types.GWRouterPrefix + node2Name + "-UUID",
						Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
						Nat:     []string{},
						Options: logicalRouterOptions,
					},
					&nbdb.LogicalSwitchPort{
						UUID:      "k8s-" + node1Name + "-UUID",
						Name:      "k8s-" + node1Name,
						Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1Subnet).IP.String()},
					},
					&nbdb.LogicalSwitchPort{
						UUID:      "k8s-" + node2Name + "-UUID",
						Name:      "k8s-" + node2Name,
						Addresses: []string{"fe:1a:c2:3f:0e:fb " + util.GetNodeManagementIfAddr(node2Subnet).IP.String()},
					},
					&nbdb.LogicalSwitch{
						UUID:  node1Name + "-UUID",
						Name:  node1Name,
						Ports: []string{"k8s-" + node1Name + "-UUID"},
					},
					&nbdb.LogicalSwitch{
						UUID:  node2Name + "-UUID",
						Name:  node2Name,
						Ports: []string{"k8s-" + node2Name + "-UUID"},
					},
					egressIPServedPodsASv4,
				}
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState2))
				gomega.Eventually(func() *podAssignmentState {
					return getPodAssignmentState(&egressPod)
				}).Should(gomega.BeNil())

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		},
			ginkgo.Entry("when the namespace remains in the informer", false),
			ginkgo.Entry("when the namespace leaves the informer first", true),
		)

	})
	ginkgo.Context("on invalid EgressIP selectors", func() {
		ginkgo.It("reconcileEgressIP should return an error", func() {
			app.Action = func(*cli.Context) error {
				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{"egressIP1"},
						PodSelector: metav1.LabelSelector{
							MatchLabels: map[string]string{"": ""},
						},
						NamespaceSelector: metav1.LabelSelector{
							MatchLabels: map[string]string{"": ""},
						},
					},
				}
				fakeOvn.startWithDBSetup(libovsdbtest.TestSetup{})
				err := fakeOvn.controller.eIPC.reconcileEgressIP(&eIP, &eIP)
				gomega.Expect(err).To(gomega.HaveOccurred())
				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})
	ginkgo.Context("WatchEgressNodes", func() {
		ginkgo.DescribeTable("retries egress node setup after reroute setup failed", func(update bool) {
			app.Action = func(*cli.Context) error {
				node := getNodeObj(node1Name, map[string]string{util.OvnNodeID: "1"}, nil)
				fakeOvn.startWithDBSetup(libovsdbtest.TestSetup{
					NBData: []libovsdbtest.TestData{
						&nbdb.LogicalRouter{
							Name: types.OVNClusterRouter,
							UUID: types.OVNClusterRouter + "-UUID",
						},
						&nbdb.LogicalSwitch{
							Name: node1Name,
							UUID: node1Name + "-UUID",
						},
					},
				}, &corev1.NodeList{Items: []corev1.Node{node}})
				gomega.Expect(fakeOvn.controller.eIPC.initClusterEgressPolicies(nil)).To(gomega.Succeed())

				fakeOvn.controller.syncEIPNodeRerouteFailed.Store(node.Name, true)
				handler := &defaultNetworkControllerEventHandler{
					objType: factory.EgressNodeType,
					oc:      fakeOvn.controller,
				}
				if update {
					gomega.Expect(handler.UpdateResource(&node, &node, true)).To(gomega.Succeed())
				} else {
					gomega.Expect(handler.AddResource(&node, true)).To(gomega.Succeed())
				}

				var routes []nbdb.LogicalRouterStaticRoute
				err := fakeOvn.nbClient.WhereCache(func(route *nbdb.LogicalRouterStaticRoute) bool {
					return route.IPPrefix == v4ClusterSubnet &&
						route.Policy != nil && *route.Policy == nbdb.LogicalRouterStaticRoutePolicySrcIP
				}).List(context.Background(), &routes)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(routes).To(gomega.HaveLen(1))
				return nil
			}

			gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
		},
			ginkgo.Entry("during an add retry", false),
			ginkgo.Entry("during an update", true),
		)
		ginkgo.It("should populated egress node data as they are tagged `egress assignable` with variants of IPv4/IPv6", func() {
			app.Action = func(*cli.Context) error {
				config.IPv6Mode = true
				node1IPv4 := "192.168.128.202"
				node1IPv4CIDR := node1IPv4 + "/24"
				node1IPv6 := "::feff:c0a8:8e0c"
				node1IPv6CIDR := node1IPv6 + "/64"
				node2IPv4 := "192.168.126.51"
				node2IPv4CIDR := node2IPv4 + "/24"
				_, node1Subnet, _ := net.ParseCIDR(v4Node1Subnet)
				annotations := map[string]string{
					"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4CIDR, node1IPv6CIDR),
					"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":[\"%s\", \"%s\"]}", v4Node1Subnet, v6Node1Subnet),
					util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\",\"%s\"]", node1IPv4CIDR, node1IPv6CIDR),
				}
				node1 := getNodeObj(node1Name, annotations, map[string]string{})
				_, node2Subnet, _ := net.ParseCIDR(v4Node2Subnet)
				annotations = map[string]string{
					"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node2IPv4CIDR, ""),
					"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":\"%s\"}", v4Node1Subnet),
					util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node2IPv4CIDR),
				}
				node2 := getNodeObj(node2Name, annotations, map[string]string{})
				node1Switch := &nbdb.LogicalSwitch{
					UUID:  node1Name + "-UUID",
					Name:  node1Name,
					Ports: []string{"k8s-" + node1Name + "-UUID"},
				}
				node2Switch := &nbdb.LogicalSwitch{
					UUID:  node2Name + "-UUID",
					Name:  node2Name,
					Ports: []string{"k8s-" + node2Name + "-UUID"},
				}
				dynamicNeighRouters := "false"

				logicalRouterOptions := map[string]string{
					"dynamic_neigh_routers": dynamicNeighRouters,
				}
				fakeOvn.startWithDBSetup(libovsdbtest.TestSetup{
					NBData: []libovsdbtest.TestData{
						&nbdb.LogicalRouter{
							Name: types.OVNClusterRouter,
							UUID: types.OVNClusterRouter + "-UUID",
						},
						&nbdb.LogicalRouter{
							Name:    types.GWRouterPrefix + node1.Name,
							UUID:    types.GWRouterPrefix + node1.Name + "-UUID",
							Options: logicalRouterOptions,
						},
						&nbdb.LogicalRouter{
							Name:    types.GWRouterPrefix + node2.Name,
							UUID:    types.GWRouterPrefix + node2.Name + "-UUID",
							Options: logicalRouterOptions,
						},
						&nbdb.LogicalSwitchPort{
							UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID",
							Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
							Type: "router",
							Options: map[string]string{
								libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
								"nat-addresses":             "router",
								"exclude-lb-vips-from-garp": "true",
							},
						},
						&nbdb.LogicalSwitchPort{
							UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID",
							Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
							Type: "router",
							Options: map[string]string{
								libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
								"nat-addresses":             "router",
								"exclude-lb-vips-from-garp": "true",
							},
						},
						&nbdb.LogicalSwitchPort{
							UUID:      "k8s-" + node1Name + "-UUID",
							Name:      "k8s-" + node1Name,
							Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1Subnet).IP.String()},
						},
						&nbdb.LogicalSwitchPort{
							UUID:      "k8s-" + node2Name + "-UUID",
							Name:      "k8s-" + node2Name,
							Addresses: []string{"fe:1a:c2:3f:0e:fb " + util.GetNodeManagementIfAddr(node2Subnet).IP.String()},
						},
						&nbdb.LogicalSwitch{
							UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
							Name:  types.ExternalSwitchPrefix + node1Name,
							Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
						},
						&nbdb.LogicalSwitch{
							UUID:  types.ExternalSwitchPrefix + node2Name + "-UUID",
							Name:  types.ExternalSwitchPrefix + node2Name,
							Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
						},
						node1Switch,
						node2Switch,
					},
				})
				err := fakeOvn.controller.WatchEgressNodes()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				node1.Labels = map[string]string{
					"k8s.ovn.org/egress-assignable": "",
				}
				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Create(context.TODO(), &node1, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// ensure the QoS rules are created correctly for node1 to avoid race between node1 and node2 add's both trying to
				// add the QoS rule at the same time thus causing UUID conflicts in UT env. This is unlikely in real scenario because nodes are
				// added first before the watch EIP namespaces/nodes etc are started so chances are the list of nodes are already available
				// and at sync time we already create the QoS rules for the nodes and add nodes don't need to do any new rule adds.
				// Also in real env the UUIDs won't conflict instead we will end up with duplicate entries (not a problem in IC with single node per node)
				time.Sleep(20 * time.Millisecond)

				node2.Labels = map[string]string{
					"k8s.ovn.org/egress-assignable": "",
				}
				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Create(context.TODO(), &node2, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				node1Switch.QOSRules = []string{"default-QoS-UUID", "default-QoSv6-UUID"}

				egressSVCServedPodsASv4, egressSVCServedPodsASv6 := buildEgressServiceAddressSets(nil)
				egressIPServedPodsASv4, egressIPServedPodsASv6 := buildEgressIPServedPodsAddressSets(nil, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName)
				egressNodeIPsASv4, egressNodeIPsASv6 := buildEgressIPNodeAddressSets([]string{node1IPv4, node1IPv6, node2IPv4})

				expectedDatabaseState := []libovsdbtest.TestData{
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
							egressIPServedPodsASv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": types.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					getNoReRouteReplyTrafficPolicy(types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip6.src == $%s || ip6.src == $%s) && ip6.dst == $%s",
							egressIPServedPodsASv6.Name, egressSVCServedPodsASv6.Name, egressNodeIPsASv6.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-v6-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": types.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV6, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouter{
						Name: types.OVNClusterRouter,
						UUID: types.OVNClusterRouter + "-UUID",
						Policies: []string{"reroute-UUID", "no-reroute-service-UUID", "default-no-reroute-node-UUID",
							"default-v6-no-reroute-node-UUID", "default-no-reroute-reply-traffic"},
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "reroute-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-service-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouter{
						Name:    types.GWRouterPrefix + node1.Name,
						UUID:    types.GWRouterPrefix + node1.Name + "-UUID",
						Options: logicalRouterOptions,
					},
					&nbdb.LogicalRouter{
						Name:    types.GWRouterPrefix + node2.Name,
						UUID:    types.GWRouterPrefix + node2.Name + "-UUID",
						Options: logicalRouterOptions,
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
						Type: "router",
						Options: map[string]string{
							libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
						Type: "router",
						Options: map[string]string{
							libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalSwitchPort{
						UUID:      "k8s-" + node1Name + "-UUID",
						Name:      "k8s-" + node1Name,
						Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1Subnet).IP.String()},
					},
					&nbdb.LogicalSwitchPort{
						UUID:      "k8s-" + node2Name + "-UUID",
						Name:      "k8s-" + node2Name,
						Addresses: []string{"fe:1a:c2:3f:0e:fb " + util.GetNodeManagementIfAddr(node2Subnet).IP.String()},
					},
					&nbdb.LogicalSwitch{
						UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
						Name:  types.ExternalSwitchPrefix + node1Name,
						Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
					},
					&nbdb.LogicalSwitch{
						UUID:  types.ExternalSwitchPrefix + node2Name + "-UUID",
						Name:  types.ExternalSwitchPrefix + node2Name,
						Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
					},
					node1Switch,
					node2Switch,
					getDefaultQoSRule(false, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
					getDefaultQoSRule(true, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
					egressSVCServedPodsASv4, egressSVCServedPodsASv6,
					egressIPServedPodsASv4, egressIPServedPodsASv6,
					egressNodeIPsASv4, egressNodeIPsASv6,
				}
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
		// FIXME(mk): This works locally but not in GH VM.
		ginkgo.XIt("using retry to create egress node with forced error followed by an update", func() {
			app.Action = func(*cli.Context) error {
				nodeIPv4 := "192.168.126.51/24"
				nodeIPv6 := "0:0:0:0:0:feff:c0a8:8e0c/64"
				_, node1SubnetV4, _ := net.ParseCIDR(v4Node1Subnet)
				_, node1SubnetV6, _ := net.ParseCIDR(v6Node1Subnet)
				annotations := map[string]string{
					"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", nodeIPv4, nodeIPv6),
					"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":[\"%s\", \"%s\"]}", v4Node1Subnet, v6Node1Subnet),
					util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", nodeIPv4),
				}
				node := getNodeObj("node", annotations, map[string]string{})
				dynamicNeighRouters := "false"

				logicalRouterOptions := map[string]string{
					"dynamic_neigh_routers": dynamicNeighRouters,
				}
				fakeOvn.startWithDBSetup(libovsdbtest.TestSetup{
					NBData: []libovsdbtest.TestData{
						&nbdb.LogicalRouter{
							Name: types.OVNClusterRouter,
							UUID: types.OVNClusterRouter + "-UUID",
						},
						&nbdb.LogicalRouter{
							Name:    types.GWRouterPrefix + node.Name,
							UUID:    types.GWRouterPrefix + node.Name + "-UUID",
							Options: logicalRouterOptions,
						},
						&nbdb.LogicalSwitchPort{
							UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + nodeName + "UUID",
							Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + nodeName,
							Type: "router",
							Options: map[string]string{
								libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + nodeName,
								"nat-addresses":             "router",
								"exclude-lb-vips-from-garp": "true",
							},
						},
						&nbdb.LogicalSwitchPort{
							UUID: "k8s-" + node.Name + "-UUID",
							Name: "k8s-" + node.Name,
							Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1SubnetV4).IP.String(),
								"fe:1a:b2:3f:0e:fa " + util.GetNodeManagementIfAddr(node1SubnetV6).IP.String()},
						},
						&nbdb.LogicalSwitch{
							UUID:  types.ExternalSwitchPrefix + nodeName + "-UUID",
							Name:  types.ExternalSwitchPrefix + nodeName,
							Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + nodeName + "-UUID"},
						},
						&nbdb.LogicalSwitch{
							UUID:  node.Name + "-UUID",
							Name:  node.Name,
							Ports: []string{"k8s-" + node.Name + "-UUID"},
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
				time.Sleep(2 * (config.Default.OVSDBTxnTimeout + time.Second))
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
				connCtx, cancel := context.WithTimeout(context.Background(), config.Default.OVSDBTxnTimeout)
				defer cancel()
				resetNBClient(connCtx, fakeOvn.controller.nbClient)
				retry.SetRetryObjWithNoBackoff(key, fakeOvn.controller.retryEgressNodes)
				fakeOvn.controller.retryEgressNodes.RequestRetryObjs()
				// check the cache no longer has the entry
				retry.CheckRetryObjectEventually(key, false, fakeOvn.controller.retryEgressNodes)

				expectedDatabaseState := []libovsdbtest.TestData{
					&nbdb.LogicalRouter{
						Name:     types.OVNClusterRouter,
						UUID:     types.OVNClusterRouter + "-UUID",
						Policies: []string{"reroute-UUID", "no-reroute-service-UUID"},
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "reroute-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-service-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouter{
						Name:    types.GWRouterPrefix + node.Name,
						UUID:    types.GWRouterPrefix + node.Name + "-UUID",
						Options: logicalRouterOptions,
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + nodeName + "UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + nodeName,
						Type: "router",
						Options: map[string]string{
							libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + nodeName,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalSwitchPort{
						UUID: "k8s-" + node.Name + "-UUID",
						Name: "k8s-" + node.Name,
						Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1SubnetV4).IP.String(),
							"fe:1a:b2:3f:0e:fa " + util.GetNodeManagementIfAddr(node1SubnetV6).IP.String()},
					},
					&nbdb.LogicalSwitch{
						UUID:  types.ExternalSwitchPrefix + nodeName + "-UUID",
						Name:  types.ExternalSwitchPrefix + nodeName,
						Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + nodeName + "-UUID"},
					},
					&nbdb.LogicalSwitch{
						UUID:  node.Name + "-UUID",
						Name:  node.Name,
						Ports: []string{"k8s-" + node.Name + "-UUID"},
					},
				}
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("egressIP pod recreate with same name (stateful-sets) shouldn't use stale logicalPortCache entries", func() {
			app.Action = func(*cli.Context) error {

				config.Gateway.DisableSNATMultipleGWs = true

				egressIP1 := "192.168.126.101"
				node1IPv4 := "192.168.126.12"
				node1IPv4CIDR := node1IPv4 + "/24"
				_, node1Subnet, _ := net.ParseCIDR(v4Node1Subnet)
				egressPod1 := *ovntest.NewPodWithLabels(eipNamespace, podName, node1Name, "", egressPodLabel)
				egressNamespace := ovntest.NewNamespace(eipNamespace)
				annotations := map[string]string{
					"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\"}", node1IPv4CIDR),
					"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":\"%s\"}", v4Node1Subnet),
					"k8s.ovn.org/l3-gateway-config":   `{"default":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"192.168.126.12/24", "next-hop":"192.168.126.1"}}`,
					"k8s.ovn.org/node-chassis-id":     "79fdcfc4-6fe6-4cd3-8242-c0f85a4668ec",
					util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node1IPv4CIDR),
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
					UUID:  node1.Name + "-UUID",
					Name:  node1.Name,
					Ports: []string{"k8s-" + node1.Name + "-UUID"},
				}
				dynamicNeighRouters := "false"

				logicalRouterOptions := map[string]string{
					"dynamic_neigh_routers": dynamicNeighRouters,
				}
				fakeOvn.startWithDBSetup(
					libovsdbtest.TestSetup{
						NBData: []libovsdbtest.TestData{
							&nbdb.LogicalRouter{
								Name: types.OVNClusterRouter,
								UUID: types.OVNClusterRouter + "-UUID",
							},
							&nbdb.LogicalRouter{
								Name:    types.GWRouterPrefix + node1.Name,
								UUID:    types.GWRouterPrefix + node1.Name + "-UUID",
								Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID"},
								Options: logicalRouterOptions,
							},
							&nbdb.LogicalRouterPort{
								UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID",
								Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name,
								Networks: []string{nodeLogicalRouterIfAddrV4},
							},
							&nbdb.LogicalSwitchPort{
								UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID",
								Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
								Type: "router",
								Options: map[string]string{
									libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
									"nat-addresses":             "router",
									"exclude-lb-vips-from-garp": "true",
								},
							},
							nodeSwitch,
							&nbdb.LogicalSwitchPort{
								UUID:      "k8s-" + node1.Name + "-UUID",
								Name:      "k8s-" + node1.Name,
								Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1Subnet).IP.String()},
							},
							&nbdb.LogicalSwitch{
								UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
								Name:  types.ExternalSwitchPrefix + node1Name,
								Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
							},
						},
					},
					&egressipv1.EgressIPList{
						Items: []egressipv1.EgressIP{eIP},
					},
					&corev1.NodeList{
						Items: []corev1.Node{node1},
					},
					&corev1.NamespaceList{
						Items: []corev1.Namespace{*egressNamespace},
					},
					&corev1.PodList{
						Items: []corev1.Pod{egressPod1},
					},
				)
				err := fakeOvn.controller.lsManager.AddOrUpdateSwitch(node1.Name, []*net.IPNet{ovntest.MustParseIPNet(v4Node1Subnet)}, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIPPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIPNamespaces()
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

				nodeSwitch.QOSRules = []string{"default-QoS-UUID"}

				egressSVCServedPodsASv4, _ := buildEgressServiceAddressSets(nil)
				egressIPServedPodsASv4, _ := buildEgressIPServedPodsAddressSets([]string{egressPodIP.String()}, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName)
				egressNodeIPsASv4, _ := buildEgressIPNodeAddressSets([]string{node1IPv4})

				expectedDatabaseStatewithPod := []libovsdbtest.TestData{
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
							egressIPServedPodsASv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": types.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					getNoReRouteReplyTrafficPolicy(types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
					&nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-service-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    types.EgressIPReroutePriority,
						Match:       fmt.Sprintf("ip4.src == %s", egressPodIP),
						Action:      nbdb.LogicalRouterPolicyActionReroute,
						Nexthops:    nodeLogicalRouterIPv4,
						ExternalIDs: getEgressIPLRPReRouteDbIDs(eIP.Name, egressPod1.Namespace, egressPod1.Name, IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
						UUID:        "reroute-UUID1",
					},
					&nbdb.LogicalRouter{
						Name: types.OVNClusterRouter,
						UUID: types.OVNClusterRouter + "-UUID",
						Policies: []string{"no-reroute-UUID", "no-reroute-service-UUID", "reroute-UUID1",
							"default-no-reroute-node-UUID", "default-no-reroute-reply-traffic"},
					},
					&nbdb.LogicalRouter{
						Name:    types.GWRouterPrefix + node1.Name,
						UUID:    types.GWRouterPrefix + node1.Name + "-UUID",
						Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID"},
						Nat:     []string{"egressip-nat-UUID1"},
						Options: logicalRouterOptions,
					},
					&nbdb.NAT{
						UUID:        "egressip-nat-UUID1",
						LogicalIP:   egressPodIP.String(),
						ExternalIP:  egressIP1,
						ExternalIDs: getEgressIPNATDbIDs(egressIPName, egressPod1.Namespace, egressPod1.Name, IPFamilyValueV4, fakeOvn.controller.controllerName).GetExternalIDs(),
						Type:        nbdb.NATTypeSNAT,
						LogicalPort: &expectedNatLogicalPort1,
						Options: map[string]string{
							"stateless": "false",
						},
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
						Type: "router",
						Options: map[string]string{
							libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalRouterPort{
						UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID",
						Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name,
						Networks: []string{"100.64.0.2/29"},
					},
					nodeSwitch,
					&nbdb.LogicalSwitchPort{
						UUID:      "k8s-" + node1.Name + "-UUID",
						Name:      "k8s-" + node1.Name,
						Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1Subnet).IP.String()},
					},
					&nbdb.LogicalSwitch{
						UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
						Name:  types.ExternalSwitchPrefix + node1Name,
						Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
					},
					getDefaultQoSRule(false, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
					egressSVCServedPodsASv4,
					egressIPServedPodsASv4,
					egressNodeIPsASv4,
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
						libovsdbops.RequestedChassis: node1.Annotations[util.OvnNodeChassisID],
						"iface-id-ver":               egressPod1.Name,
					},
					PortSecurity: []string{podAddr},
				}
				nodeSwitch.Ports = append(nodeSwitch.Ports, podLSP.UUID)
				finalDatabaseStatewithPod := append(expectedDatabaseStatewithPod, podLSP)
				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
				_, nodes := getEgressIPStatus(egressIPName)
				gomega.Expect(nodes[0]).To(gomega.Equal(node1.Name))

				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(finalDatabaseStatewithPod))

				// delete the pod
				err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(egressPod1.Namespace).Delete(context.TODO(),
					egressPod1.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				nodeSwitch.Ports = []string{"k8s-" + node1.Name + "-UUID"} // remove the pod port
				egressIPServedPodsASv4.Addresses = nil
				expectedDatabaseStateWithoutPod := []libovsdbtest.TestData{
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
							egressIPServedPodsASv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": types.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					getNoReRouteReplyTrafficPolicy(types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
					&nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-service-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouter{
						Name:     types.OVNClusterRouter,
						UUID:     types.OVNClusterRouter + "-UUID",
						Policies: []string{"no-reroute-UUID", "no-reroute-service-UUID", "default-no-reroute-node-UUID", "default-no-reroute-reply-traffic"},
					},
					&nbdb.LogicalRouter{
						Name:    types.GWRouterPrefix + node1.Name,
						UUID:    types.GWRouterPrefix + node1.Name + "-UUID",
						Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID"},
						Nat:     []string{},
						Options: logicalRouterOptions,
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
						Type: "router",
						Options: map[string]string{
							libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalRouterPort{
						UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID",
						Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name,
						Networks: []string{"100.64.0.2/29"},
					},
					nodeSwitch,
					&nbdb.LogicalSwitchPort{
						UUID:      "k8s-" + node1.Name + "-UUID",
						Name:      "k8s-" + node1.Name,
						Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1Subnet).IP.String()},
					},
					&nbdb.LogicalSwitch{
						UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
						Name:  types.ExternalSwitchPrefix + node1Name,
						Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
					},
					getDefaultQoSRule(false, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
					egressSVCServedPodsASv4,
					egressIPServedPodsASv4,
					egressNodeIPsASv4,
				}
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseStateWithoutPod))
				// recreate pod with same name immediately; simulating handler race (pods v/s egressip) condition,
				// so instead of proper pod create, we try out egressIP pod setup which will be a no-op since pod doesn't exist
				ginkgo.By("should not add egress IP setup for a deleted pod whose entry exists in logicalPortCache")
				err = fakeOvn.controller.eIPC.addPodEgressIPAssignments(fakeOvn.controller, egressIPName, eIP.Status.Items, util.EgressIPMark{}, &egressPod1)
				gomega.Expect(err).To(gomega.HaveOccurred())
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
			app.Action = func(*cli.Context) error {

				config.Gateway.DisableSNATMultipleGWs = true

				egressIP1 := "192.168.126.101"
				node1IPv4 := "192.168.126.12"
				node1IPv4CIDR := node1IPv4 + "/24"
				_, node1Subnet, _ := net.ParseCIDR(v4Node1Subnet)
				oldEgressPodIP := "10.128.0.50"
				egressPod1 := ovntest.NewPodWithLabels(eipNamespace, podName, node1Name, "", egressPodLabel)
				oldAnnotation := map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["10.128.0.50/24"],"mac_address":"0a:58:0a:80:00:05","gateway_ips":["10.128.0.1"],"routes":[{"dest":"10.128.0.0/24","nextHop":"10.128.0.1"}],"ip_address":"10.128.0.50/24","gateway_ip":"10.128.0.1"}}`}
				egressPod1.Annotations = oldAnnotation
				egressNamespace := ovntest.NewNamespace(eipNamespace)

				annotations := map[string]string{
					"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\"}", node1IPv4CIDR),
					"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":\"%s\"}", v4Node1Subnet),
					"k8s.ovn.org/l3-gateway-config":   `{"default":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"192.168.126.12/24", "next-hop":"192.168.126.1"}}`,
					"k8s.ovn.org/node-chassis-id":     "79fdcfc4-6fe6-4cd3-8242-c0f85a4668ec",
					util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node1IPv4CIDR),
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
					UUID:  node1.Name + "-UUID",
					Name:  node1.Name,
					Ports: []string{"k8s-" + node1.Name + "-UUID"},
				}
				dynamicNeighRouters := "false"

				logicalRouterOptions := map[string]string{
					"dynamic_neigh_routers": dynamicNeighRouters,
				}
				fakeOvn.startWithDBSetup(
					libovsdbtest.TestSetup{
						NBData: []libovsdbtest.TestData{
							&nbdb.LogicalRouter{
								Name: types.OVNClusterRouter,
								UUID: types.OVNClusterRouter + "-UUID",
							},
							&nbdb.LogicalRouter{
								Name:    types.GWRouterPrefix + node1.Name,
								UUID:    types.GWRouterPrefix + node1.Name + "-UUID",
								Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID"},
								Options: logicalRouterOptions,
							},
							&nbdb.LogicalRouterPort{
								UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID",
								Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name,
								Networks: []string{nodeLogicalRouterIfAddrV4},
							},
							&nbdb.LogicalSwitchPort{
								UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID",
								Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
								Type: "router",
								Options: map[string]string{
									libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
									"nat-addresses":             "router",
									"exclude-lb-vips-from-garp": "true",
								},
							},
							nodeSwitch,
							&nbdb.LogicalSwitchPort{
								UUID:      "k8s-" + node1.Name + "-UUID",
								Name:      "k8s-" + node1.Name,
								Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1Subnet).IP.String()},
							},
							&nbdb.LogicalSwitch{
								UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
								Name:  types.ExternalSwitchPrefix + node1Name,
								Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
							},
						},
					},
					&egressipv1.EgressIPList{
						Items: []egressipv1.EgressIP{eIP},
					},
					&corev1.NodeList{
						Items: []corev1.Node{node1},
					},
					&corev1.NamespaceList{
						Items: []corev1.Namespace{*egressNamespace},
					},
					&corev1.PodList{
						Items: []corev1.Pod{*egressPod1},
					},
				)
				err := fakeOvn.controller.lsManager.AddOrUpdateSwitch(node1.Name, []*net.IPNet{ovntest.MustParseIPNet(v4Node1Subnet)}, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(fakeOvn.controller.WatchPods()).To(gomega.Succeed())
				gomega.Expect(fakeOvn.controller.WatchEgressIPPods()).To(gomega.Succeed())
				gomega.Expect(fakeOvn.controller.WatchEgressIPNamespaces()).To(gomega.Succeed())
				gomega.Expect(fakeOvn.controller.WatchEgressNodes()).To(gomega.Succeed())
				gomega.Expect(fakeOvn.controller.WatchEgressIP()).To(gomega.Succeed())

				oldEgressPodPortInfo, err := fakeOvn.controller.logicalPortCache.get(egressPod1, types.DefaultNetworkName)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				egressPodIP, _, err := net.ParseCIDR(oldEgressPodPortInfo.ips[0].String())
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(egressPodIP.String()).To(gomega.Equal(oldEgressPodIP))
				gomega.Expect(oldEgressPodPortInfo.expires.IsZero()).To(gomega.BeTrue())
				podAddr := fmt.Sprintf("%s %s", oldEgressPodPortInfo.mac.String(), egressPodIP)

				expectedNatLogicalPort1 := "k8s-node1"
				podEIPSNAT := &nbdb.NAT{
					UUID:        "egressip-nat-UUID1",
					LogicalIP:   egressPodIP.String(),
					ExternalIP:  egressIP1,
					ExternalIDs: getEgressIPNATDbIDs(egressIPName, egressPod1.Namespace, egressPod1.Name, IPFamilyValueV4, fakeOvn.controller.controllerName).GetExternalIDs(),
					Type:        nbdb.NATTypeSNAT,
					LogicalPort: &expectedNatLogicalPort1,
					Options: map[string]string{
						"stateless": "false",
					},
				}
				podReRoutePolicy := &nbdb.LogicalRouterPolicy{
					Priority:    types.EgressIPReroutePriority,
					Match:       fmt.Sprintf("ip4.src == %s", oldEgressPodIP),
					Action:      nbdb.LogicalRouterPolicyActionReroute,
					Nexthops:    nodeLogicalRouterIPv4,
					ExternalIDs: getEgressIPLRPReRouteDbIDs(eIP.Name, egressPod1.Namespace, egressPod1.Name, IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					UUID:        "reroute-UUID1",
				}
				node1GR := &nbdb.LogicalRouter{
					Name:    types.GWRouterPrefix + node1.Name,
					UUID:    types.GWRouterPrefix + node1.Name + "-UUID",
					Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID"},
					Nat:     []string{"egressip-nat-UUID1"},
					Options: logicalRouterOptions,
				}
				nodeSwitch.QOSRules = []string{"default-QoS-UUID"}

				egressSVCServedPodsASv4, _ := buildEgressServiceAddressSets(nil)
				egressIPServedPodsASv4, _ := buildEgressIPServedPodsAddressSets([]string{oldEgressPodIP}, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName)
				egressNodeIPsASv4, _ := buildEgressIPNodeAddressSets([]string{node1IPv4})

				expectedDatabaseStatewithPod := []libovsdbtest.TestData{
					podEIPSNAT,
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
							egressIPServedPodsASv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": types.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					getNoReRouteReplyTrafficPolicy(types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
					&nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-service-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					podReRoutePolicy,
					&nbdb.LogicalRouter{
						Name: types.OVNClusterRouter,
						UUID: types.OVNClusterRouter + "-UUID",
						Policies: []string{"no-reroute-UUID", "no-reroute-service-UUID", "reroute-UUID1",
							"default-no-reroute-node-UUID", "default-no-reroute-reply-traffic"},
					},
					node1GR,
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
						Type: "router",
						Options: map[string]string{
							libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalRouterPort{
						UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID",
						Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name,
						Networks: []string{"100.64.0.2/29"},
					},
					nodeSwitch,
					&nbdb.LogicalSwitchPort{
						UUID:      "k8s-" + node1.Name + "-UUID",
						Name:      "k8s-" + node1.Name,
						Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1Subnet).IP.String()},
					},
					&nbdb.LogicalSwitch{
						UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
						Name:  types.ExternalSwitchPrefix + node1Name,
						Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
					},
					getDefaultQoSRule(false, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
					egressSVCServedPodsASv4,
					egressNodeIPsASv4,
					egressIPServedPodsASv4,
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
						libovsdbops.RequestedChassis: node1.Annotations[util.OvnNodeChassisID],
						"iface-id-ver":               egressPod1.Name,
					},
					PortSecurity: []string{podAddr},
				}
				nodeSwitch.Ports = append(nodeSwitch.Ports, podLSP.UUID)
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

				_, err = libovsdbops.GetLogicalSwitchPort(fakeOvn.controller.nbClient, &nbdb.LogicalSwitchPort{Name: podLSP.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Delete the pod to trigger the cleanup failure
				err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(egressPod1.Namespace).Delete(context.TODO(),
					egressPod1.Name, metav1.DeleteOptions{})
				// recreate pod with same name immediately;
				ginkgo.By("should add egress IP setup for the NEW pod which exists in logicalPortCache")
				newEgressPodIP := "10.128.0.60"
				egressPod1 = ovntest.NewPodWithLabels(eipNamespace, podName, node1Name, newEgressPodIP, egressPodLabel)
				egressPod1.Annotations = map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["10.128.0.60/24"],"mac_address":"0a:58:0a:80:00:06","gateway_ips":["10.128.0.1"],"routes":[{"dest":"10.128.0.0/24","nextHop":"10.128.0.1"}],"ip_address":"10.128.0.60/24","gateway_ip":"10.128.0.1"}}`}
				gomega.Eventually(func() error {
					_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(egressPod1.Namespace).Create(context.TODO(), egressPod1, metav1.CreateOptions{})
					return err
				}, "5s", "1s").ShouldNot(gomega.HaveOccurred())
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
				// ensure that egressIP setup is being done with the new pod's information from logicalPortCache
				podReRoutePolicy.Match = fmt.Sprintf("ip4.src == %s", newEgressPodIP)
				podEIPSNAT.LogicalIP = newEgressPodIP
				node1GR.Nat = []string{podEIPSNAT.UUID}
				egressIPServedPodsASv4.Addresses = []string{"10.128.0.60"}

				podPortInfo, err := fakeOvn.controller.logicalPortCache.get(egressPod1, types.DefaultNetworkName)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				egressPodIP, _, err = net.ParseCIDR(podPortInfo.ips[0].String())
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(podPortInfo.expires.IsZero()).To(gomega.BeTrue())
				podAddr = fmt.Sprintf("%s %s", podPortInfo.mac.String(), egressPodIP)
				podLSP.Addresses = []string{podAddr}
				podLSP.PortSecurity = []string{podAddr}
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(finalDatabaseStatewithPod))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.DescribeTable("egressIP pod managed by multiple objects, verify standby works wells, verify syncPodAssignmentCache on restarts",
			func(controllerNodeName string) {
				fakeOvn.nodeName = controllerNodeName
				app.Action = func(*cli.Context) error {

					config.Gateway.DisableSNATMultipleGWs = true

					egressIP1 := "192.168.126.25"
					egressIP2 := "192.168.126.30"
					egressIP3 := "192.168.126.35"
					isNode1Local := controllerNodeName == node1Name
					isNode2Local := controllerNodeName == node2Name
					node1IPv4 := "192.168.126.12"
					node1IPv4CIDR := node1IPv4 + "/24"
					node2IPv4 := "192.168.126.13"
					node2IPv4CIDR := node2IPv4 + "/24"

					egressPod1 := *ovntest.NewPodWithLabels(eipNamespace, podName, node1Name, "", egressPodLabel)
					egressNamespace := ovntest.NewNamespace(eipNamespace)
					annotations := map[string]string{
						"k8s.ovn.org/node-primary-ifaddr":             fmt.Sprintf("{\"ipv4\": \"%s\"}", node1IPv4CIDR),
						"k8s.ovn.org/node-subnets":                    fmt.Sprintf("{\"default\":\"%s\"}", v4Node1Subnet),
						"k8s.ovn.org/l3-gateway-config":               `{"default":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"192.168.126.12/24", "next-hop":"192.168.126.1"}}`,
						"k8s.ovn.org/node-chassis-id":                 "79fdcfc4-6fe6-4cd3-8242-c0f85a4668ec",
						"k8s.ovn.org/node-transit-switch-port-ifaddr": "{\"ipv4\":\"100.88.0.2/16\"}", // used only for ic=true test
						util.OVNNodeHostCIDRs:                         fmt.Sprintf("[\"%s\"]", node1IPv4CIDR),
					}
					labels := map[string]string{
						"k8s.ovn.org/egress-assignable": "",
					}
					node1 := getNodeObj(node1Name, annotations, labels)
					annotations = map[string]string{
						"k8s.ovn.org/node-primary-ifaddr":             fmt.Sprintf("{\"ipv4\": \"%s\"}", node2IPv4CIDR),
						"k8s.ovn.org/node-subnets":                    fmt.Sprintf("{\"default\":\"%s\"}", v4Node1Subnet),
						"k8s.ovn.org/l3-gateway-config":               `{"default":{"mode":"local","mac-address":"7e:57:f8:f0:3c:50", "ip-address":"192.168.126.13/24", "next-hop":"192.168.126.1"}}`,
						"k8s.ovn.org/node-chassis-id":                 "79fdcfc4-6fe6-4cd3-8242-c0f85a4668ec",
						"k8s.ovn.org/node-transit-switch-port-ifaddr": "{\"ipv4\":\"100.88.0.3/16\"}", // used only for ic=true test
						util.OVNNodeHostCIDRs:                         fmt.Sprintf("[\"%s\"]", node2IPv4CIDR),
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
						ObjectMeta: newEgressIPMeta(egressIP2Name),
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
					node2Switch := &nbdb.LogicalSwitch{
						UUID: node2.Name + "-UUID",
						Name: node2.Name,
					}
					node1GR := &nbdb.LogicalRouter{
						Name:  types.GWRouterPrefix + node1.Name,
						UUID:  types.GWRouterPrefix + node1.Name + "-UUID",
						Ports: []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID"},
					}
					node2GR := &nbdb.LogicalRouter{
						Name:  types.GWRouterPrefix + node2.Name,
						UUID:  types.GWRouterPrefix + node2.Name + "-UUID",
						Ports: []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID"},
					}
					node1LSP := &nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
						Type: "router",
						Options: map[string]string{
							libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					}
					node2LSP := &nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
						Type: "router",
						Options: map[string]string{
							libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					}

					fakeOvn.startWithDBSetup(
						libovsdbtest.TestSetup{
							NBData: []libovsdbtest.TestData{
								&nbdb.LogicalRouter{
									Name: types.OVNClusterRouter,
									UUID: types.OVNClusterRouter + "-UUID",
								},
								node1GR, node2GR,
								node1LSP, node2LSP,
								&nbdb.LogicalRouterPort{
									UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID",
									Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name,
									Networks: []string{"100.64.0.3/29"},
								},
								&nbdb.LogicalRouterPort{
									UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID",
									Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name,
									Networks: []string{"100.64.0.2/29"},
								},
								node1Switch,
								node2Switch,
								&nbdb.LogicalSwitch{
									UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
									Name:  types.ExternalSwitchPrefix + node1Name,
									Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
								},
								&nbdb.LogicalSwitch{
									UUID:  types.ExternalSwitchPrefix + node2Name + "-UUID",
									Name:  types.ExternalSwitchPrefix + node2Name,
									Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
								},
							},
						},
						&egressipv1.EgressIPList{
							Items: []egressipv1.EgressIP{eIP1, eIP2},
						},
						&corev1.NodeList{
							Items: []corev1.Node{node1, node2},
						},
						&corev1.NamespaceList{
							Items: []corev1.Namespace{*egressNamespace},
						},
						&corev1.PodList{
							Items: []corev1.Pod{egressPod1},
						},
					)
					err := fakeOvn.controller.lsManager.AddOrUpdateSwitch(node1.Name, []*net.IPNet{ovntest.MustParseIPNet(v4Node1Subnet)}, nil, nil)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.lsManager.AddOrUpdateSwitch(node2.Name, []*net.IPNet{ovntest.MustParseIPNet(v4Node2Subnet)}, nil, nil)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchPods()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchEgressIPPods()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchEgressIPNamespaces()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchEgressNodes()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchEgressIP()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					fakeOvn.patchEgressIPObj(node1Name, egressIPName, egressIP1)

					// NOTE: Cluster manager is the one who patches the egressIP object.
					// For the sake of unit testing the EgressIP controller we need to patch the EgressIP object manually.
					// There are tests in cluster-manager package covering the patch logic.
					status := []egressipv1.EgressIPStatusItem{
						{
							Node:     node1Name,
							EgressIP: egressIP3,
						},
					}
					err = fakeOvn.controller.eIPC.patchReplaceEgressIPStatus(egressIP2Name, status)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					// Ensure first egressIP object is assigned, since only node1 is an egressNode, only 1IP will be assigned, other will be pending
					gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
					gomega.Eventually(getEgressIPReassignmentCount).Should(gomega.Equal(1))
					egressIPs1, nodes1 := getEgressIPStatus(egressIPName)
					gomega.Expect(nodes1[0]).To(gomega.Equal(node1.Name))
					gomega.Expect(egressIPs1[0]).To(gomega.Equal(egressIP1))

					// Ensure second egressIP object is also assigned to node1, but no OVN config will be done for this
					gomega.Eventually(getEgressIPStatusLen(egressIP2Name)).Should(gomega.Equal(1))
					egressIPs2, nodes2 := getEgressIPStatus(egressIP2Name)
					gomega.Expect(nodes2[0]).To(gomega.Equal(node1.Name))
					gomega.Expect(egressIPs2[0]).To(gomega.Equal(egressIP3))
					// egress node is node 1 and pod is located on node 1. If node 2 is local, there is not config
					if !isNode1Local && isNode2Local {
						return nil
					}

					egressPodPortInfo, err := fakeOvn.controller.logicalPortCache.get(&egressPod1, types.DefaultNetworkName)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					ePod, err := fakeOvn.fakeClient.KubeClient.CoreV1().Pods(egressPod1.Namespace).Get(context.TODO(), egressPod1.Name, metav1.GetOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					egressPodIP, err := util.GetPodIPsOfNetwork(ePod, &util.DefaultNetInfo{}, nil)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					egressNetPodIP, _, err := net.ParseCIDR(egressPodPortInfo.ips[0].String())
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					gomega.Expect(egressNetPodIP.String()).To(gomega.Equal(egressPodIP[0].String()))
					gomega.Expect(egressPodPortInfo.expires.IsZero()).To(gomega.BeTrue())
					podAddr := fmt.Sprintf("%s %s", egressPodPortInfo.mac.String(), egressPodIP[0].String())

					gomega.Eventually(fakeOvn.fakeRecorder.Events).Should(gomega.Receive(
						gomega.ContainSubstring("EgressIP object egressip-2 will not be configured for pod egressip-namespace_egress-pod since another egressIP object egressip is serving it, this is undefined"),
					))

					assignedEIP := egressIPs1[0]
					var pas *podAssignmentState
					gomega.Eventually(func(g gomega.Gomega) {
						pas = getPodAssignmentState(&egressPod1)
						g.Expect(pas).NotTo(gomega.BeNil())
						g.Expect(pas.egressIPName).To(gomega.Equal(egressIPName))
						eip1Obj, err := fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Get(context.TODO(), eIP1.Name, metav1.GetOptions{})
						g.Expect(err).NotTo(gomega.HaveOccurred())
						g.Expect(pas.egressStatuses.statusMap[eip1Obj.Status.Items[0]]).To(gomega.Equal(""))
						g.Expect(pas.standbyEgressIPNames.Has(egressIP2Name)).To(gomega.BeTrue())
					}).Should(gomega.Succeed())
					podEIPSNAT := &nbdb.NAT{
						UUID:        "egressip-nat-UUID1",
						LogicalIP:   egressPodIP[0].String(),
						ExternalIP:  assignedEIP,
						ExternalIDs: getEgressIPNATDbIDs(egressIPName, eipNamespace, podName, IPFamilyValueV4, fakeOvn.controller.controllerName).GetExternalIDs(),
						Type:        nbdb.NATTypeSNAT,
						LogicalPort: ptr.To("k8s-node1"),
						Options: map[string]string{
							"stateless": "false",
						},
					}
					podReRoutePolicy := &nbdb.LogicalRouterPolicy{
						Priority:    types.EgressIPReroutePriority,
						Match:       fmt.Sprintf("ip4.src == %s", egressPodIP[0].String()),
						Action:      nbdb.LogicalRouterPolicyActionReroute,
						Nexthops:    nodeLogicalRouterIPv4,
						ExternalIDs: getEgressIPLRPReRouteDbIDs(pas.egressIPName, egressPod1.Namespace, egressPod1.Name, IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
						UUID:        "reroute-UUID1",
					}
					node1GR.Nat = []string{"egressip-nat-UUID1"}

					egressSVCServedPodsASv4, _ := buildEgressServiceAddressSets(nil)
					egressIPServedPodsASv4, _ := buildEgressIPServedPodsAddressSets([]string{egressPodIP[0].String()}, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName)
					egressNodeIPsASv4, _ := buildEgressIPNodeAddressSets([]string{node1IPv4, node2IPv4})

					expectedDatabaseStatewithPod := []libovsdbtest.TestData{
						podEIPSNAT,
						podReRoutePolicy,
						&nbdb.LogicalRouterPolicy{
							Priority:    types.DefaultNoRereoutePriority,
							Match:       "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
							Action:      nbdb.LogicalRouterPolicyActionAllow,
							UUID:        "no-reroute-UUID",
							ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
						},
						&nbdb.LogicalRouterPolicy{
							Priority:    types.DefaultNoRereoutePriority,
							Match:       fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
							Action:      nbdb.LogicalRouterPolicyActionAllow,
							UUID:        "no-reroute-service-UUID",
							ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
						},
						&nbdb.LogicalRouter{
							Name: types.OVNClusterRouter,
							UUID: types.OVNClusterRouter + "-UUID",
							Policies: []string{"no-reroute-UUID", "no-reroute-service-UUID", "reroute-UUID1",
								"default-no-reroute-node-UUID", "default-no-reroute-reply-traffic"},
						},
						node1GR, node2GR,
						node1LSP, node2LSP,
						&nbdb.LogicalRouterPort{
							UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID",
							Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name,
							Networks: []string{"100.64.0.2/29"},
						},
						&nbdb.LogicalRouterPort{
							UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID",
							Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name,
							Networks: []string{"100.64.0.3/29"},
						},
						node1Switch,
						node2Switch,
						&nbdb.LogicalRouterPolicy{
							Priority: types.DefaultNoRereoutePriority,
							Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
								egressIPServedPodsASv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
							Action:      nbdb.LogicalRouterPolicyActionAllow,
							UUID:        "default-no-reroute-node-UUID",
							Options:     map[string]string{"pkt_mark": types.EgressIPNodeConnectionMark},
							ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
						},
						&nbdb.LogicalSwitch{
							UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
							Name:  types.ExternalSwitchPrefix + node1Name,
							Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
						},
						&nbdb.LogicalSwitch{
							UUID:  types.ExternalSwitchPrefix + node2Name + "-UUID",
							Name:  types.ExternalSwitchPrefix + node2Name,
							Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
						},
						getNoReRouteReplyTrafficPolicy(types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
						getDefaultQoSRule(false, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
						egressSVCServedPodsASv4,
						egressIPServedPodsASv4,
						egressNodeIPsASv4,
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
							libovsdbops.RequestedChassis: node1.Annotations[util.OvnNodeChassisID],
							"iface-id-ver":               egressPod1.Name,
						},
						PortSecurity: []string{podAddr},
					}
					node1Switch.Ports = []string{podLSP.UUID}
					node1Switch.QOSRules = []string{"default-QoS-UUID"}
					node2Switch.QOSRules = []string{"default-QoS-UUID"}
					finalDatabaseStatewithPod := append(expectedDatabaseStatewithPod, podLSP)
					if !isNode1Local {
						// policy is not visible since podNode is in remote zone
						finalDatabaseStatewithPod[4].(*nbdb.LogicalRouter).Policies = []string{"no-reroute-UUID", "no-reroute-service-UUID",
							"default-no-reroute-node-UUID", "default-no-reroute-reply-traffic"}
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
						node1Switch.QOSRules = nil // if node is remote then we don't add qos rule
					}
					if !isNode2Local {
						node2Switch.QOSRules = nil // if node is remote then we don't add qos rule
					}

					gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(finalDatabaseStatewithPod))

					// Make second node egressIP assignable
					node2.Labels = map[string]string{
						"k8s.ovn.org/egress-assignable": "",
					}
					_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), &node2, metav1.UpdateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					// NOTE: Cluster manager is the one who patches the egressIP object.
					// For the sake of unit testing the EgressIP controller we need to patch the EgressIP object manually.
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
					err = fakeOvn.controller.eIPC.patchReplaceEgressIPStatus(egressIPName, status)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					// ensure secondIP from first object gets assigned to node2
					gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(2))
					egressIPs1, nodes1 = getEgressIPStatus(egressIPName)
					gomega.Expect(nodes1[1]).To(gomega.Equal(node2.Name))
					gomega.Expect(egressIPs1[1]).To(gomega.Equal(egressIP2))

					podEIPSNAT2 := &nbdb.NAT{
						UUID:        "egressip-nat-UUID2",
						LogicalIP:   egressPodIP[0].String(),
						ExternalIP:  egressIPs1[1],
						ExternalIDs: getEgressIPNATDbIDs(pas.egressIPName, egressPod1.Namespace, egressPod1.Name, IPFamilyValueV4, fakeOvn.controller.controllerName).GetExternalIDs(),
						Type:        nbdb.NATTypeSNAT,
						LogicalPort: ptr.To("k8s-node2"),
						Options: map[string]string{
							"stateless": "false",
						},
					}
					podReRoutePolicy.Nexthops = []string{nodeLogicalRouterIPv4[0], node2LogicalRouterIPv4[0]}
					if !isNode2Local {
						// the policy reroute will have its second nexthop as transit switchIP
						// so the one with join switchIP is where podNode == egressNode and one with transitIP is where podNode != egressNode
						podReRoutePolicy.Nexthops = []string{"100.64.0.2", "100.88.0.3"}
					}
					if isNode2Local {
						node2GR.Nat = []string{"egressip-nat-UUID2"}
						finalDatabaseStatewithPod = append(finalDatabaseStatewithPod, podEIPSNAT2)
					}

					gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(finalDatabaseStatewithPod))

					// check the state of the cache for podKey
					var eip1Obj *egressipv1.EgressIP
					gomega.Eventually(func(g gomega.Gomega) {
						pas = getPodAssignmentState(&egressPod1)
						g.Expect(pas).NotTo(gomega.BeNil())
						g.Expect(pas.egressIPName).To(gomega.Equal(egressIPName))
						eip1Obj, err = fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Get(context.TODO(), eIP1.Name, metav1.GetOptions{})
						g.Expect(err).NotTo(gomega.HaveOccurred())
						g.Expect(pas.egressStatuses.statusMap).To(gomega.HaveLen(2))
						g.Expect(pas.egressStatuses.statusMap[eip1Obj.Status.Items[0]]).To(gomega.Equal(""))
						g.Expect(pas.egressStatuses.statusMap[eip1Obj.Status.Items[1]]).To(gomega.Equal(""))
						g.Expect(pas.standbyEgressIPNames.Has(egressIP2Name)).To(gomega.BeTrue())
					}).Should(gomega.Succeed())

					// let's test syncPodAssignmentCache works as expected! Nuke the podAssignment cache first
					// only key in the map, lets lock it out of precaution to wipe the cache
					egressPod1Key := getPodKey(&egressPod1)
					oldPodAssignment := fakeOvn.controller.eIPC.podAssignment
					oldPodAssignment.LockKey(egressPod1Key)
					fakeOvn.controller.eIPC.podAssignment = syncmap.NewSyncMap[*podAssignmentState]()
					oldPodAssignment.UnlockKey(egressPod1Key)
					// replicates controller startup state
					egressIPCache, err := fakeOvn.controller.eIPC.generateCacheForEgressIP()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.eIPC.syncPodAssignmentCache(egressIPCache)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					pas = getPodAssignmentState(&egressPod1)
					gomega.Expect(pas).NotTo(gomega.BeNil())
					gomega.Expect(pas.egressIPName).To(gomega.Equal(egressIPName))
					gomega.Expect(pas.egressStatuses.statusMap).To(gomega.HaveLen(2))
					gomega.Expect(pas.standbyEgressIPNames.Has(egressIP2Name)).To(gomega.BeTrue())

					// delete the standby egressIP object to make sure the cache is updated
					err = fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Delete(context.TODO(), egressIP2Name, metav1.DeleteOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					gomega.Eventually(func(g gomega.Gomega) {
						pas = getPodAssignmentState(&egressPod1)
						g.Expect(pas).NotTo(gomega.BeNil())
						g.Expect(pas.egressIPName).To(gomega.Equal(egressIPName))
						eip1Obj, err = fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Get(context.TODO(), eIP1.Name, metav1.GetOptions{})
						g.Expect(err).NotTo(gomega.HaveOccurred())
						g.Expect(pas.egressStatuses.statusMap).To(gomega.HaveLen(2))
						g.Expect(pas.egressStatuses.statusMap[eip1Obj.Status.Items[0]]).To(gomega.Equal(egressStatusStatePending))
						g.Expect(pas.egressStatuses.statusMap[eip1Obj.Status.Items[1]]).To(gomega.Equal(egressStatusStatePending))
						g.Expect(pas.standbyEgressIPNames.Has(egressIP2Name)).To(gomega.BeFalse())
					}).Should(gomega.Succeed())
					// add back the standby egressIP object
					_, err = fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Create(context.TODO(), &eIP2, metav1.CreateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					// NOTE: Cluster manager is the one who patches the egressIP object.
					// For the sake of unit testing the EgressIP controller we need to patch the EgressIP object manually.
					// There are tests in cluster-manager package covering the patch logic.
					status = []egressipv1.EgressIPStatusItem{
						{
							Node:     node1Name,
							EgressIP: egressIP3,
						},
					}
					err = fakeOvn.controller.eIPC.patchReplaceEgressIPStatus(egressIP2Name, status)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					gomega.Eventually(func(g gomega.Gomega) {
						pas = getPodAssignmentState(&egressPod1)
						g.Expect(pas).NotTo(gomega.BeNil())
						g.Expect(pas.egressIPName).To(gomega.Equal(egressIPName))
						eip1Obj, err = fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Get(context.TODO(), eIP1.Name, metav1.GetOptions{})
						g.Expect(err).NotTo(gomega.HaveOccurred())
						g.Expect(pas.egressStatuses.statusMap).To(gomega.HaveLen(2))
						g.Expect(pas.egressStatuses.statusMap[eip1Obj.Status.Items[0]]).To(gomega.Equal(egressStatusStatePending))
						g.Expect(pas.egressStatuses.statusMap[eip1Obj.Status.Items[1]]).To(gomega.Equal(egressStatusStatePending))
						g.Expect(pas.standbyEgressIPNames.Has(egressIP2Name)).To(gomega.BeTrue())
					}).Should(gomega.Succeed())
					gomega.Eventually(fakeOvn.fakeRecorder.Events).Should(gomega.Receive(
						gomega.ContainSubstring("EgressIP object egressip-2 will not be configured for pod egressip-namespace_egress-pod since another egressIP object egressip is serving it, this is undefined"),
					))

					gomega.Eventually(getEgressIPStatusLen(egressIP2Name)).Should(gomega.Equal(1))
					egressIPs2, nodes2 = getEgressIPStatus(egressIP2Name)
					gomega.Expect(egressIPs2[0]).To(gomega.Equal(egressIP3))
					assginedNodeForEIPObj2 := nodes2[0]

					// Delete the IP from object1 that was on node1 and ensure standby is not taking over
					eIPUpdate, err := fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Get(context.TODO(), eIP1.Name, metav1.GetOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					ipOnNode1 := assignedEIP
					var ipOnNode2 string
					if ipOnNode1 == egressIP1 {
						ipOnNode2 = egressIP2
					} else {
						ipOnNode2 = egressIP1
					}
					eIPUpdate.Spec.EgressIPs = []string{ipOnNode2}
					_, err = fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Update(context.TODO(), eIPUpdate, metav1.UpdateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					fakeOvn.patchEgressIPObj(node2Name, egressIPName, ipOnNode2)

					gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
					egressIPs1, nodes1 = getEgressIPStatus(egressIPName)
					gomega.Expect(nodes1[0]).To(gomega.Equal(node2.Name))
					gomega.Expect(egressIPs1[0]).To(gomega.Equal(ipOnNode2))

					// check if the setup for firstIP from object1 is deleted properly
					podReRoutePolicy.Nexthops = node2LogicalRouterIPv4
					if !isNode2Local {
						// the policy reroute will have its second nexthop as transit switchIP
						// so the one with join switchIP is where podNode == egressNode and one with transitIP is where podNode != egressNode
						podReRoutePolicy.Nexthops = []string{"100.88.0.3"}
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
					if isNode1Local {
						node1GR.Nat = []string{podNodeSNAT.UUID}
						finalDatabaseStatewithPod = append(finalDatabaseStatewithPod, podNodeSNAT)
						gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(finalDatabaseStatewithPod[1:]))
					} else {
						gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(finalDatabaseStatewithPod))
					}

					gomega.Eventually(func(g gomega.Gomega) {
						pas = getPodAssignmentState(&egressPod1)
						g.Expect(pas).NotTo(gomega.BeNil())
						g.Expect(pas.egressIPName).To(gomega.Equal(egressIPName))
						eip1Obj, err = fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Get(context.TODO(), eIP1.Name, metav1.GetOptions{})
						g.Expect(err).NotTo(gomega.HaveOccurred())
						g.Expect(pas.egressStatuses.statusMap).To(gomega.HaveLen(1))
						g.Expect(pas.egressStatuses.statusMap[eip1Obj.Status.Items[0]]).To(gomega.Equal(egressStatusStatePending))
						g.Expect(pas.standbyEgressIPNames.Has(egressIP2Name)).To(gomega.BeTrue())
					}).Should(gomega.Succeed())
					// delete the first egressIP object and make sure the cache is updated
					err = fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Delete(context.TODO(), egressIPName, metav1.DeleteOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					// ensure standby takes over and we do the setup for it in OVN DB
					gomega.Eventually(func(g gomega.Gomega) {
						pas = getPodAssignmentState(&egressPod1)
						g.Expect(pas).NotTo(gomega.BeNil())
						g.Expect(pas.egressIPName).To(gomega.Equal(egressIP2Name))
						eip1Obj, err = fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Get(context.TODO(), eIP2.Name, metav1.GetOptions{})
						g.Expect(err).NotTo(gomega.HaveOccurred())
						g.Expect(pas.egressStatuses.statusMap).To(gomega.HaveLen(1))
						g.Expect(pas.egressStatuses.statusMap[eip1Obj.Status.Items[0]]).To(gomega.Equal(""))
						g.Expect(pas.standbyEgressIPNames).To(gomega.BeEmpty())
					}).Should(gomega.Succeed())
					finalDatabaseStatewithPod = expectedDatabaseStatewithPod
					finalDatabaseStatewithPod = append(expectedDatabaseStatewithPod, podLSP)
					podEIPSNAT.ExternalIP = egressIP3
					podEIPSNAT.ExternalIDs = getEgressIPNATDbIDs(egressIP2Name, egressPod1.Namespace, egressPod1.Name, IPFamilyValueV4, types.DefaultNetworkControllerName).GetExternalIDs()
					podReRoutePolicy.ExternalIDs = getEgressIPLRPReRouteDbIDs(egressIP2Name, egressPod1.Namespace, egressPod1.Name, IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs()
					if assginedNodeForEIPObj2 == node2.Name {
						podEIPSNAT.LogicalPort = ptr.To("k8s-node2")
						finalDatabaseStatewithPod = append(finalDatabaseStatewithPod, podNodeSNAT)
						node1GR.Nat = []string{podNodeSNAT.UUID}
						node2GR.Nat = []string{podEIPSNAT.UUID}
					}
					if assginedNodeForEIPObj2 == node1.Name {
						podReRoutePolicy.Nexthops = nodeLogicalRouterIPv4
						node1GR.Nat = []string{podEIPSNAT.UUID}
						node2GR.Nat = []string{}
					}
					if !isNode1Local {
						// policy is not visible since podNode is in remote zone
						finalDatabaseStatewithPod[4].(*nbdb.LogicalRouter).Policies = []string{"no-reroute-UUID", "no-reroute-service-UUID",
							"default-no-reroute-node-UUID", "default-no-reroute-reply-traffic"}
						finalDatabaseStatewithPod = finalDatabaseStatewithPod[2:]
						podEIPSNAT.ExternalIP = "192.168.126.12" // EIP SNAT is not visible since podNode is remote, SNAT towards nodeIP is visible.
						podEIPSNAT.LogicalPort = nil
						finalDatabaseStatewithPod = append(finalDatabaseStatewithPod, podNodeSNAT)
						node1GR.Nat = []string{"node-nat-UUID1"}
						finalDatabaseStatewithPod[2].(*nbdb.LogicalRouter).StaticRoutes = []string{}
					}
					gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(finalDatabaseStatewithPod))

					// delete the second egressIP object to make sure the cache is updated podKey should be gone since nothing is managing it anymore
					err = fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Delete(context.TODO(), egressIP2Name, metav1.DeleteOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					gomega.Eventually(func() bool {
						return getPodAssignmentState(&egressPod1) != nil
					}).Should(gomega.BeFalse())

					// let's test syncPodAssignmentCache works as expected! Nuke the podAssignment cache first
					oldPodAssignment = fakeOvn.controller.eIPC.podAssignment
					oldPodAssignment.LockKey(egressPod1Key)
					fakeOvn.controller.eIPC.podAssignment = syncmap.NewSyncMap[*podAssignmentState]()
					oldPodAssignment.UnlockKey(egressPod1Key)

					egressIPCache, err = fakeOvn.controller.eIPC.generateCacheForEgressIP()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.eIPC.syncPodAssignmentCache(egressIPCache)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					// we don't have any egressIPs, so cache is nil
					gomega.Expect(getPodAssignmentState(&egressPod1)).To(gomega.BeNil())

					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			},
			ginkgo.Entry("controller manages the pod and egress node", node1Name),
			ginkgo.Entry("controller manages neither the pod nor egress node", node2Name),
		)

	})

	ginkgo.Context("WatchEgressNodes running with WatchEgressIP", func() {

		ginkgo.It("should treat un-assigned EgressIPs when it is tagged", func() {
			app.Action = func(*cli.Context) error {
				config.IPv6Mode = true
				egressIP := "192.168.126.101"
				nodeIPv4 := "192.168.126.51"
				nodeIPv4CIDR := nodeIPv4 + "/24"
				nodeIPv6 := "::feff:c0a8:8e0c"
				nodeIPv6CIDR := nodeIPv6 + "/64"

				node := corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: node1Name,
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", nodeIPv4CIDR, nodeIPv6CIDR),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":[\"%s\", \"%s\"]}", v4Node1Subnet, v6Node1Subnet),
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\",\"%s\"]", nodeIPv4CIDR, nodeIPv6CIDR),
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
				node1Switch := &nbdb.LogicalSwitch{
					UUID: node1Name + "-UUID",
					Name: node1Name,
				}
				fakeOvn.startWithDBSetup(
					libovsdbtest.TestSetup{
						NBData: []libovsdbtest.TestData{
							&nbdb.LogicalRouter{
								Name: types.OVNClusterRouter,
								UUID: types.OVNClusterRouter + "-UUID",
							},
							&nbdb.LogicalRouter{
								Name: types.GWRouterPrefix + node.Name,
								UUID: types.GWRouterPrefix + node.Name + "-UUID",
							},
							&nbdb.LogicalSwitchPort{
								UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node.Name + "UUID",
								Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node.Name,
								Type: "router",
								Options: map[string]string{
									libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node.Name,
									"nat-addresses":             "router",
									"exclude-lb-vips-from-garp": "true",
								},
							},
							&nbdb.LogicalSwitch{
								UUID:  types.ExternalSwitchPrefix + node.Name + "-UUID",
								Name:  types.ExternalSwitchPrefix + node.Name,
								Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node.Name + "UUID"},
							},
							node1Switch,
						},
					},
					&egressipv1.EgressIPList{
						Items: []egressipv1.EgressIP{eIP},
					},
					&corev1.NodeList{
						Items: []corev1.Node{node},
					})

				err := fakeOvn.controller.WatchEgressIPPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIPNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressNodes()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				node1Switch.QOSRules = []string{"default-QoS-UUID", "default-QoSv6-UUID"}

				egressSVCServedPodsASv4, egressSVCServedPodsASv6 := buildEgressServiceAddressSets(nil)
				egressIPServedPodsASv4, egressIPServedPodsASv6 := buildEgressIPServedPodsAddressSets(nil, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName)
				egressNodeIPsASv4, egressNodeIPsASv6 := buildEgressIPNodeAddressSets([]string{nodeIPv4, nodeIPv6})

				expectedDatabaseState := []libovsdbtest.TestData{
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
							egressIPServedPodsASv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": types.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					getNoReRouteReplyTrafficPolicy(types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip6.src == $%s || ip6.src == $%s) && ip6.dst == $%s",
							egressIPServedPodsASv6.Name, egressSVCServedPodsASv6.Name, egressNodeIPsASv6.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-v6-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": types.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV6, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-service-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouter{
						Name: types.OVNClusterRouter,
						UUID: types.OVNClusterRouter + "-UUID",
						Policies: []string{"no-reroute-UUID", "no-reroute-service-UUID", "default-no-reroute-node-UUID",
							"default-v6-no-reroute-node-UUID", "default-no-reroute-reply-traffic"},
					},
					&nbdb.LogicalRouter{
						Name: types.GWRouterPrefix + node.Name,
						UUID: types.GWRouterPrefix + node.Name + "-UUID",
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node.Name + "UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node.Name,
						Type: "router",
						Options: map[string]string{
							libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node.Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalSwitch{
						UUID:  types.ExternalSwitchPrefix + node.Name + "-UUID",
						Name:  types.ExternalSwitchPrefix + node.Name,
						Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node.Name + "UUID"},
					},
					node1Switch,
					getDefaultQoSRule(false, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
					getDefaultQoSRule(true, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
					egressSVCServedPodsASv4, egressSVCServedPodsASv6,
					egressIPServedPodsASv4, egressIPServedPodsASv6,
					egressNodeIPsASv4, egressNodeIPsASv6,
				}
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				gomega.Eventually(eIP.Status.Items).Should(gomega.BeEmpty())

				node.Labels = map[string]string{
					"k8s.ovn.org/egress-assignable": "",
				}

				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), &node, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				fakeOvn.patchEgressIPObj(node1Name, egressIPName, egressIP)

				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
				egressIPs, nodes := getEgressIPStatus(egressIPName)
				gomega.Expect(nodes[0]).To(gomega.Equal(node.Name))
				gomega.Expect(egressIPs[0]).To(gomega.Equal(egressIP))

				gomega.Eventually(getEgressIPReassignmentCount).Should(gomega.Equal(0))
				expectedDatabaseState = []libovsdbtest.TestData{
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
							egressIPServedPodsASv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": types.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					getNoReRouteReplyTrafficPolicy(types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip6.src == $%s || ip6.src == $%s) && ip6.dst == $%s",
							egressIPServedPodsASv6.Name, egressSVCServedPodsASv6.Name, egressNodeIPsASv6.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-v6-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": types.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV6, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-service-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouter{
						Name: types.OVNClusterRouter,
						UUID: types.OVNClusterRouter + "-UUID",
						Policies: []string{"no-reroute-UUID", "no-reroute-service-UUID", "default-no-reroute-node-UUID",
							"default-v6-no-reroute-node-UUID", "default-no-reroute-reply-traffic"},
					},
					&nbdb.LogicalRouter{
						Name: types.GWRouterPrefix + node.Name,
						UUID: types.GWRouterPrefix + node.Name + "-UUID",
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node.Name + "UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node.Name,
						Type: "router",
						Options: map[string]string{
							libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node.Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalSwitch{
						UUID:  types.ExternalSwitchPrefix + node.Name + "-UUID",
						Name:  types.ExternalSwitchPrefix + node.Name,
						Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node.Name + "UUID"},
					},
					node1Switch,
					getDefaultQoSRule(false, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
					getDefaultQoSRule(true, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
					egressSVCServedPodsASv4, egressSVCServedPodsASv6,
					egressIPServedPodsASv4, egressIPServedPodsASv6,
					egressNodeIPsASv4, egressNodeIPsASv6,
				}
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should result in error and event if specified egress IP is a cluster node IP", func() {
			app.Action = func(*cli.Context) error {

				egressIP := "192.168.126.51"
				node1IPv4 := "192.168.128.202"
				node1IPv4CIDR := node1IPv4 + "/24"
				node1IPv6CIDR := "::feff:c0a8:8e0c/64"
				node2IPv4 := "192.168.126.51"
				node2IPv4CIDR := node2IPv4 + "/24"
				annotations := map[string]string{
					"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4CIDR, node1IPv6CIDR),
					"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":[\"%s\", \"%s\"]}", v4Node1Subnet, v6Node1Subnet),
					util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node1IPv4CIDR),
				}
				labels := map[string]string{
					"k8s.ovn.org/egress-assignable": "",
				}
				node1 := getNodeObj(node1Name, annotations, labels)
				annotations = map[string]string{
					"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node2IPv4CIDR, ""),
					"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":\"%s\"}", v4Node1Subnet),
					util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node2IPv4CIDR),
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
				node1Switch := &nbdb.LogicalSwitch{
					UUID: node1.Name + "-UUID",
					Name: node1.Name,
				}
				node2Switch := &nbdb.LogicalSwitch{
					UUID: node2.Name + "-UUID",
					Name: node2.Name,
				}
				fakeOvn.startWithDBSetup(
					libovsdbtest.TestSetup{
						NBData: []libovsdbtest.TestData{
							&nbdb.LogicalRouter{
								Name: types.OVNClusterRouter,
								UUID: types.OVNClusterRouter + "-UUID",
							},
							&nbdb.LogicalRouter{
								Name: types.GWRouterPrefix + node1.Name,
								UUID: types.GWRouterPrefix + node1.Name + "-UUID",
							},
							&nbdb.LogicalRouter{
								Name: types.GWRouterPrefix + node2.Name,
								UUID: types.GWRouterPrefix + node2.Name + "-UUID",
							},
							&nbdb.LogicalSwitchPort{
								UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID",
								Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
								Type: "router",
								Options: map[string]string{
									libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
									"nat-addresses":             "router",
									"exclude-lb-vips-from-garp": "true",
								},
							},
							&nbdb.LogicalSwitchPort{
								UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID",
								Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
								Type: "router",
								Options: map[string]string{
									libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
									"nat-addresses":             "router",
									"exclude-lb-vips-from-garp": "true",
								},
							},
							&nbdb.LogicalSwitch{
								UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
								Name:  types.ExternalSwitchPrefix + node1Name,
								Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
							},
							&nbdb.LogicalSwitch{
								UUID:  types.ExternalSwitchPrefix + node2Name + "-UUID",
								Name:  types.ExternalSwitchPrefix + node2Name,
								Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
							},
							node1Switch,
							node2Switch,
						},
					},
					&egressipv1.EgressIPList{
						Items: []egressipv1.EgressIP{eIP},
					},
					&corev1.NodeList{
						Items: []corev1.Node{node1, node2},
					})

				err := fakeOvn.controller.WatchEgressIPPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIPNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressNodes()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				node1Switch.QOSRules = []string{"default-QoS-UUID"}

				egressSVCServedPodsASv4, _ := buildEgressServiceAddressSets(nil)
				egressIPServedPodsASv4, _ := buildEgressIPServedPodsAddressSets(nil, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName)
				egressNodeIPsASv4, _ := buildEgressIPNodeAddressSets([]string{node1IPv4, node2IPv4})

				expectedDatabaseState := []libovsdbtest.TestData{
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
							egressIPServedPodsASv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": types.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					getNoReRouteReplyTrafficPolicy(types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
					&nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-service-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouter{
						Name:     types.OVNClusterRouter,
						UUID:     types.OVNClusterRouter + "-UUID",
						Policies: []string{"no-reroute-UUID", "no-reroute-service-UUID", "default-no-reroute-node-UUID", "default-no-reroute-reply-traffic"},
					},
					&nbdb.LogicalRouter{
						Name: types.GWRouterPrefix + node1.Name,
						UUID: types.GWRouterPrefix + node1.Name + "-UUID",
					},
					&nbdb.LogicalRouter{
						Name: types.GWRouterPrefix + node2.Name,
						UUID: types.GWRouterPrefix + node2.Name + "-UUID",
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
						Type: "router",
						Options: map[string]string{
							libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
						Type: "router",
						Options: map[string]string{
							libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalSwitch{
						UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
						Name:  types.ExternalSwitchPrefix + node1Name,
						Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
					},
					&nbdb.LogicalSwitch{
						UUID:  types.ExternalSwitchPrefix + node2Name + "-UUID",
						Name:  types.ExternalSwitchPrefix + node2Name,
						Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
					},
					node1Switch,
					node2Switch,
					getDefaultQoSRule(false, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
					egressSVCServedPodsASv4,
					egressIPServedPodsASv4,
					egressNodeIPsASv4,
				}
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should re-assigned EgressIPs when more nodes get tagged if the first assignment attempt wasn't fully successful", func() {
			app.Action = func(*cli.Context) error {

				egressIP1 := "192.168.126.25"
				egressIP2 := "192.168.126.30"
				node1IPv4 := "192.168.126.51"
				node1IPv4CIDR := node1IPv4 + "/24"
				node2IPv4 := "192.168.126.101"
				node2IPv4CIDR := node2IPv4 + "/24"

				annotations := map[string]string{
					"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4CIDR, ""),
					"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":\"%s\"}", v4Node1Subnet),
					util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node1IPv4CIDR),
				}
				labels := map[string]string{
					"k8s.ovn.org/egress-assignable": "",
				}
				node1 := getNodeObj(node1Name, annotations, labels)
				annotations = map[string]string{
					"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node2IPv4CIDR, ""),
					"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":\"%s\"}", v4Node2Subnet),
					util.OvnTransitSwitchPortAddr:     "{\"ipv4\":\"100.88.0.3/16\"}",
					util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node2IPv4CIDR),
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
				node1Switch := &nbdb.LogicalSwitch{
					UUID: node1.Name + "-UUID",
					Name: node1.Name,
				}
				node2Switch := &nbdb.LogicalSwitch{
					UUID: node2.Name + "-UUID",
					Name: node2.Name,
				}
				fakeOvn.startWithDBSetup(
					libovsdbtest.TestSetup{
						NBData: []libovsdbtest.TestData{
							&nbdb.LogicalRouter{
								Name: types.OVNClusterRouter,
								UUID: types.OVNClusterRouter + "-UUID",
							},
							&nbdb.LogicalRouter{
								Name: types.GWRouterPrefix + node1.Name,
								UUID: types.GWRouterPrefix + node1.Name + "-UUID",
							},
							&nbdb.LogicalRouter{
								Name: types.GWRouterPrefix + node2.Name,
								UUID: types.GWRouterPrefix + node2.Name + "-UUID",
							},
							&nbdb.LogicalSwitchPort{
								UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID",
								Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
								Type: "router",
								Options: map[string]string{
									libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
									"nat-addresses":             "router",
									"exclude-lb-vips-from-garp": "true",
								},
							},
							&nbdb.LogicalSwitchPort{
								UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID",
								Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
								Type: "router",
								Options: map[string]string{
									libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
									"nat-addresses":             "router",
									"exclude-lb-vips-from-garp": "true",
								},
							},
							&nbdb.LogicalSwitch{
								UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
								Name:  types.ExternalSwitchPrefix + node1Name,
								Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
							},
							&nbdb.LogicalSwitch{
								UUID:  types.ExternalSwitchPrefix + node2Name + "-UUID",
								Name:  types.ExternalSwitchPrefix + node2Name,
								Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
							},
							node1Switch,
							node2Switch,
						},
					},
					&egressipv1.EgressIPList{
						Items: []egressipv1.EgressIP{eIP},
					},
					&corev1.NodeList{
						Items: []corev1.Node{node1, node2},
					})

				err := fakeOvn.controller.WatchEgressIPPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIPNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressNodes()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				node1Switch.QOSRules = []string{"default-QoS-UUID"}

				egressSVCServedPodsASv4, _ := buildEgressServiceAddressSets(nil)
				egressIPServedPodsASv4, _ := buildEgressIPServedPodsAddressSets(nil, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName)
				egressNodeIPsASv4, _ := buildEgressIPNodeAddressSets([]string{node1IPv4, node2IPv4})

				expectedDatabaseState := []libovsdbtest.TestData{
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
							egressIPServedPodsASv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": types.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					getNoReRouteReplyTrafficPolicy(types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
					&nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-service-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouter{
						Name:     types.OVNClusterRouter,
						UUID:     types.OVNClusterRouter + "-UUID",
						Policies: []string{"no-reroute-UUID", "no-reroute-service-UUID", "default-no-reroute-node-UUID", "default-no-reroute-reply-traffic"},
					},
					&nbdb.LogicalRouter{
						Name: types.GWRouterPrefix + node1.Name,
						UUID: types.GWRouterPrefix + node1.Name + "-UUID",
					},
					&nbdb.LogicalRouter{
						Name: types.GWRouterPrefix + node2.Name,
						UUID: types.GWRouterPrefix + node2.Name + "-UUID",
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
						Type: "router",
						Options: map[string]string{
							libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
						Type: "router",
						Options: map[string]string{
							libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalSwitch{
						UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
						Name:  types.ExternalSwitchPrefix + node1Name,
						Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
					},
					&nbdb.LogicalSwitch{
						UUID:  types.ExternalSwitchPrefix + node2Name + "-UUID",
						Name:  types.ExternalSwitchPrefix + node2Name,
						Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
					},
					node1Switch,
					node2Switch,
					getDefaultQoSRule(false, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
					egressSVCServedPodsASv4,
					egressIPServedPodsASv4,
					egressNodeIPsASv4,
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
						Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
							egressIPServedPodsASv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": types.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					getNoReRouteReplyTrafficPolicy(types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
					&nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-service-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouter{
						Name: types.OVNClusterRouter,
						UUID: types.OVNClusterRouter + "-UUID",
						Policies: []string{"no-reroute-UUID", "no-reroute-service-UUID", "default-no-reroute-node-UUID",
							"default-no-reroute-reply-traffic"},
					},
					&nbdb.LogicalRouter{
						Name: types.GWRouterPrefix + node1.Name,
						UUID: types.GWRouterPrefix + node1.Name + "-UUID",
					},
					&nbdb.LogicalRouter{
						Name: types.GWRouterPrefix + node2.Name,
						UUID: types.GWRouterPrefix + node2.Name + "-UUID",
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
						Type: "router",
						Options: map[string]string{
							libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
						Type: "router",
						Options: map[string]string{
							libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalSwitch{
						UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
						Name:  types.ExternalSwitchPrefix + node1Name,
						Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
					},
					&nbdb.LogicalSwitch{
						UUID:  types.ExternalSwitchPrefix + node2Name + "-UUID",
						Name:  types.ExternalSwitchPrefix + node2Name,
						Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
					},
					node1Switch,
					node2Switch,
					getDefaultQoSRule(false, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
					egressIPServedPodsASv4,
					egressSVCServedPodsASv4,
					egressNodeIPsASv4,
				}
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should remove stale EgressIP setup when node label is removed while ovnkube-controller is not running and assign to newly labelled node", func() {
			app.Action = func(*cli.Context) error {

				egressIP1 := "192.168.126.25"
				node1IPv4 := "192.168.126.51"
				node1IPv4CIDR := node1IPv4 + "/24"
				node2IPv4 := "192.168.126.5"
				node2IPv4CIDR := node2IPv4 + "/24"

				egressPod := *ovntest.NewPodWithLabels(eipNamespace, podName, node1Name, podV4IP, egressPodLabel)
				egressNamespace := ovntest.NewNamespace(eipNamespace)
				annotations := map[string]string{
					"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4CIDR, ""),
					"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":\"%s\"}", v4Node1Subnet),
					util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node1IPv4CIDR),
				}
				labels := map[string]string{}
				node1 := getNodeObj(node1Name, annotations, labels)
				annotations = map[string]string{
					"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node2IPv4CIDR, ""),
					"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":\"%s\"}", v4Node2Subnet),
					util.OvnTransitSwitchPortAddr:     "{\"ipv4\":\"100.88.0.3/16\"}",
					util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node2IPv4CIDR),
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
				node1Switch := &nbdb.LogicalSwitch{
					UUID: node1.Name + "-UUID",
					Name: node1.Name,
				}
				node2Switch := &nbdb.LogicalSwitch{
					UUID: node2.Name + "-UUID",
					Name: node2.Name,
				}
				fakeOvn.startWithDBSetup(
					libovsdbtest.TestSetup{
						NBData: []libovsdbtest.TestData{
							&nbdb.LogicalRouter{
								Name: types.OVNClusterRouter,
							},
							&nbdb.LogicalRouter{
								Name:  types.GWRouterPrefix + node1.Name,
								UUID:  types.GWRouterPrefix + node1.Name + "-UUID",
								Ports: []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID"},
							},
							&nbdb.LogicalRouter{
								Name:  types.GWRouterPrefix + node2.Name,
								UUID:  types.GWRouterPrefix + node2.Name + "-UUID",
								Ports: []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID"},
							},
							&nbdb.LogicalRouterPort{
								UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID",
								Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name,
								Networks: []string{nodeLogicalRouterIfAddrV4},
							},
							&nbdb.LogicalRouterPort{
								UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID",
								Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name,
								Networks: []string{node2LogicalRouterIfAddrV4},
							},
							&nbdb.LogicalSwitchPort{
								UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID",
								Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
								Type: "router",
								Options: map[string]string{
									libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
									"nat-addresses":             "router",
									"exclude-lb-vips-from-garp": "true",
								},
							},
							&nbdb.LogicalSwitchPort{
								UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID",
								Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
								Type: "router",
								Options: map[string]string{
									libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
									"nat-addresses":             "router",
									"exclude-lb-vips-from-garp": "true",
								},
							},
							&nbdb.LogicalSwitch{
								UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
								Name:  types.ExternalSwitchPrefix + node1Name,
								Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
							},
							&nbdb.LogicalSwitch{
								UUID:  types.ExternalSwitchPrefix + node2Name + "-UUID",
								Name:  types.ExternalSwitchPrefix + node2Name,
								Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
							},
							node1Switch,
							node2Switch,
						},
					},
					&egressipv1.EgressIPList{
						Items: []egressipv1.EgressIP{eIP},
					},
					&corev1.NodeList{
						Items: []corev1.Node{node1, node2},
					},
					&corev1.NamespaceList{
						Items: []corev1.Namespace{*egressNamespace},
					},
					&corev1.PodList{
						Items: []corev1.Pod{egressPod},
					},
				)

				i, n, _ := net.ParseCIDR(podV4IP + "/23")
				n.IP = i
				fakeOvn.controller.logicalPortCache.add(&egressPod, "", types.DefaultNetworkName, "", nil, []*net.IPNet{n})

				err := fakeOvn.controller.WatchEgressIPPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIPNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressNodes()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				fakeOvn.patchEgressIPObj(node2Name, egressIPName, egressIP1)

				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
				gomega.Eventually(getEgressIPReassignmentCount).Should(gomega.Equal(0))
				node1Switch.QOSRules = []string{"default-QoS-UUID"}

				egressSVCServedPodsASv4, _ := buildEgressServiceAddressSets(nil)
				egressIPServedPodsASv4, _ := buildEgressIPServedPodsAddressSets([]string{podV4IP}, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName)
				egressNodeIPsASv4, _ := buildEgressIPNodeAddressSets([]string{node1IPv4, node2IPv4})

				expectedDatabaseState := []libovsdbtest.TestData{
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
							egressIPServedPodsASv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": types.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-no-reroute-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					getNoReRouteReplyTrafficPolicy(types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
					&nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-service-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    types.EgressIPReroutePriority,
						Match:       fmt.Sprintf("ip4.src == %s", egressPod.Status.PodIP),
						Action:      nbdb.LogicalRouterPolicyActionReroute,
						Nexthops:    []string{"100.88.0.3"},
						ExternalIDs: getEgressIPLRPReRouteDbIDs(eIP.Name, egressPod.Namespace, egressPod.Name, IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
						UUID:        "reroute-UUID",
					},
					&nbdb.LogicalRouter{
						Name: types.OVNClusterRouter,
						UUID: types.OVNClusterRouter + "-UUID",
						Policies: []string{"reroute-UUID", "default-no-reroute-UUID", "no-reroute-service-UUID",
							"default-no-reroute-node-UUID", "default-no-reroute-reply-traffic"},
					},
					&nbdb.LogicalRouter{
						Name:  types.GWRouterPrefix + node1.Name,
						UUID:  types.GWRouterPrefix + node1.Name + "-UUID",
						Ports: []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID"},
					},
					&nbdb.LogicalRouter{
						Name:  types.GWRouterPrefix + node2.Name,
						UUID:  types.GWRouterPrefix + node2.Name + "-UUID",
						Ports: []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID"},
					},
					&nbdb.LogicalRouterPort{
						UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID",
						Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name,
						Networks: []string{node2LogicalRouterIfAddrV4},
					},
					&nbdb.LogicalRouterPort{
						UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID",
						Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name,
						Networks: []string{nodeLogicalRouterIfAddrV4},
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
						Type: "router",
						Options: map[string]string{
							libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
						Type: "router",
						Options: map[string]string{
							libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalSwitch{
						UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
						Name:  types.ExternalSwitchPrefix + node1Name,
						Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
					},
					&nbdb.LogicalSwitch{
						UUID:  types.ExternalSwitchPrefix + node2Name + "-UUID",
						Name:  types.ExternalSwitchPrefix + node2Name,
						Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
					},
					node1Switch,
					node2Switch,
					getDefaultQoSRule(false, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
					egressSVCServedPodsASv4,
					egressIPServedPodsASv4,
					egressNodeIPsASv4,
				}

				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should remove stale EgressIP setup when pod is deleted while ovnkube-controller is not running", func() {
			app.Action = func(*cli.Context) error {

				egressIP1 := "192.168.126.25"
				node1IPv4 := "192.168.126.51"
				node1IPv4CIDR := node1IPv4 + "/24"

				egressNamespace := ovntest.NewNamespace(eipNamespace)

				annotations := map[string]string{
					"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4CIDR, ""),
					"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":\"%s\"}", v4Node1Subnet),
					util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node1IPv4CIDR),
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
								UUID:        "keep-me-UUID",
								Match:       "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
								Priority:    types.DefaultNoRereoutePriority,
								Action:      nbdb.LogicalRouterPolicyActionAllow,
								ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, types.DefaultNetworkName, types.DefaultNetworkControllerName).GetExternalIDs(),
							},
							&nbdb.LogicalRouterPolicy{
								UUID:        "remove-me-UUID",
								ExternalIDs: getEgressIPLRPReRouteDbIDs(eIP.Name, egressNamespace.Name, "doesnt-exist-pod", IPFamilyValueV4, types.DefaultNetworkName, types.DefaultNetworkControllerName).GetExternalIDs(),
								Match:       "ip.src == 10.128.3.8",
								Priority:    types.EgressIPReroutePriority,
								Action:      nbdb.LogicalRouterPolicyActionReroute,
							},
							&nbdb.LogicalRouter{
								Name:     types.OVNClusterRouter,
								UUID:     types.OVNClusterRouter + "-UUID",
								Policies: []string{"remove-me-UUID", "keep-me-UUID"},
							},
							&nbdb.LogicalRouter{
								Name: types.GWRouterPrefix + node1.Name,
								UUID: types.GWRouterPrefix + node1.Name + "-UUID",
								Nat:  []string{"egressip-nat-UUID"},
							},
							&nbdb.NAT{
								UUID:        "egressip-nat-UUID",
								LogicalIP:   podV4IP,
								ExternalIP:  egressIP1,
								ExternalIDs: getEgressIPNATDbIDs(egressIPName, eipNamespace, podName, IPFamilyValueV4, types.DefaultNetworkControllerName).GetExternalIDs(),
								Type:        nbdb.NATTypeSNAT,
								LogicalPort: &expectedNatLogicalPort,
								Options: map[string]string{
									"stateless": "false",
								},
							},
							&nbdb.LogicalSwitchPort{
								UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID",
								Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
								Type: "router",
								Options: map[string]string{
									libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
									"nat-addresses":             "router",
									"exclude-lb-vips-from-garp": "true",
								},
							},
							&nbdb.LogicalSwitch{
								UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
								Name:  types.ExternalSwitchPrefix + node1Name,
								Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
							},
							&nbdb.LogicalSwitch{
								UUID:     node1Name + "-UUID",
								Name:     node1Name,
								QOSRules: []string{"default-QoS-UUID"},
							},
							getDefaultQoSRule(false, types.DefaultNetworkName, types.DefaultNetworkControllerName),
						},
					},
					&egressipv1.EgressIPList{
						Items: []egressipv1.EgressIP{eIP},
					},
					&corev1.NodeList{
						Items: []corev1.Node{node1},
					},
					&corev1.NamespaceList{
						Items: []corev1.Namespace{*egressNamespace},
					},
				)

				err := fakeOvn.controller.WatchEgressIPPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIPNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressNodes()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
				gomega.Eventually(getEgressIPReassignmentCount).Should(gomega.Equal(0))

				egressSVCServedPodsASv4, _ := buildEgressServiceAddressSets(nil)
				egressIPServedPodsASv4, _ := buildEgressIPServedPodsAddressSets(nil, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName)
				egressNodeIPsASv4, _ := buildEgressIPNodeAddressSets([]string{node1IPv4})

				expectedDatabaseState := []libovsdbtest.TestData{
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
							egressIPServedPodsASv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": types.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					getNoReRouteReplyTrafficPolicy(types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
					&nbdb.LogicalRouterPolicy{
						UUID:        "keep-me-UUID",
						Match:       "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
						Priority:    types.DefaultNoRereoutePriority,
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-service-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouter{
						Name: types.OVNClusterRouter,
						UUID: types.OVNClusterRouter + "-UUID",
						Policies: []string{"keep-me-UUID", "no-reroute-service-UUID", "default-no-reroute-node-UUID",
							"default-no-reroute-reply-traffic"},
					},
					&nbdb.LogicalRouter{
						Name: types.GWRouterPrefix + node1.Name,
						UUID: types.GWRouterPrefix + node1.Name + "-UUID",
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
						Type: "router",
						Options: map[string]string{
							libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalSwitch{
						UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
						Name:  types.ExternalSwitchPrefix + node1Name,
						Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
					},
					&nbdb.LogicalSwitch{
						UUID:     node1Name + "-UUID",
						Name:     node1Name,
						QOSRules: []string{"default-QoS-UUID"},
					},
					getDefaultQoSRule(false, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
					egressSVCServedPodsASv4,
					egressIPServedPodsASv4,
					egressNodeIPsASv4,
				}
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should remove stale pod SNAT referring to wrong logical port after ovnkube-controller is started", func() {
			app.Action = func(*cli.Context) error {
				config.Gateway.DisableSNATMultipleGWs = true
				egressIP := "192.168.126.25"
				node1IPv4 := "192.168.126.12"
				node1IPv4CIDR := node1IPv4 + "/24"

				egressPod := *ovntest.NewPodWithLabels(eipNamespace, podName, node1Name, podV4IP, egressPodLabel)
				egressNamespace := ovntest.NewNamespace(eipNamespace)

				annotations := map[string]string{
					"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\"}", node1IPv4CIDR),
					"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":\"%s\"}", v4Node1Subnet),
					"k8s.ovn.org/l3-gateway-config":   `{"default":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"192.168.126.12/24", "next-hop":"192.168.126.1"}}`,
					"k8s.ovn.org/node-chassis-id":     "79fdcfc4-6fe6-4cd3-8242-c0f85a4668ec",
					util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node1IPv4CIDR),
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
					Name:  types.GWRouterPrefix + node1.Name,
					UUID:  types.GWRouterPrefix + node1.Name + "-UUID",
					Ports: []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID"},
				}
				node1LSP := &nbdb.LogicalSwitchPort{
					UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID",
					Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
					Type: "router",
					Options: map[string]string{
						libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
						"nat-addresses":             "router",
						"exclude-lb-vips-from-garp": "true",
					},
				}
				fakeOvn.startWithDBSetup(
					libovsdbtest.TestSetup{
						NBData: []libovsdbtest.TestData{
							&nbdb.LogicalRouter{
								Name: types.OVNClusterRouter,
								UUID: types.OVNClusterRouter + "-UUID",
							},
							node1GR,
							node1LSP,
							&nbdb.LogicalRouterPort{
								UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID",
								Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name,
								Networks: []string{nodeLogicalRouterIfAddrV4},
							},
							node1Switch,
							// This is unexpected snat entry where its logical port refers to an unavailable node
							// and ensure this entry is removed as soon as ovnkube-controller is up and running.
							&nbdb.NAT{
								UUID:        "egressip-nat-UUID2",
								LogicalIP:   podV4IP,
								ExternalIP:  egressIP,
								ExternalIDs: getEgressIPNATDbIDs(egressIPName, eipNamespace, podName, IPFamilyValueV4, types.DefaultNetworkControllerName).GetExternalIDs(),
								Type:        nbdb.NATTypeSNAT,
								LogicalPort: ptr.To("k8s-node2"),
								Options: map[string]string{
									"stateless": "false",
								},
							},
							&nbdb.LogicalSwitch{
								UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
								Name:  types.ExternalSwitchPrefix + node1Name,
								Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
							},
						},
					},
					&egressipv1.EgressIPList{
						Items: []egressipv1.EgressIP{eIP},
					},
					&corev1.NodeList{
						Items: []corev1.Node{node1},
					},
					&corev1.NamespaceList{
						Items: []corev1.Namespace{*egressNamespace},
					},
					&corev1.PodList{
						Items: []corev1.Pod{egressPod},
					},
				)

				i, n, _ := net.ParseCIDR(podV4IP + "/23")
				n.IP = i
				fakeOvn.controller.logicalPortCache.add(&egressPod, "", types.DefaultNetworkName, "", nil, []*net.IPNet{n})

				err := fakeOvn.controller.WatchPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIPPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIPNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressNodes()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				egressPodPortInfo, err := fakeOvn.controller.logicalPortCache.get(&egressPod, types.DefaultNetworkName)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				ePod, err := fakeOvn.fakeClient.KubeClient.CoreV1().Pods(egressPod.Namespace).Get(context.TODO(), egressPod.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				egressPodIP, err := util.GetPodIPsOfNetwork(ePod, &util.DefaultNetInfo{}, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				egressNetPodIP, _, err := net.ParseCIDR(egressPodPortInfo.ips[0].String())
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(egressNetPodIP.String()).To(gomega.Equal(egressPodIP[0].String()))
				gomega.Expect(egressPodPortInfo.expires.IsZero()).To(gomega.BeTrue())

				fakeOvn.patchEgressIPObj(node1Name, egressIPName, egressIP)
				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
				gomega.Eventually(getEgressIPReassignmentCount).Should(gomega.Equal(0))
				egressIPs, nodes := getEgressIPStatus(egressIPName)
				gomega.Expect(nodes[0]).To(gomega.Equal(node1.Name))
				gomega.Expect(egressIPs[0]).To(gomega.Equal(egressIP))

				podEIPSNAT := getEIPSNAT(podV4IP, egressPod.Namespace, egressPod.Name, egressIP, "k8s-node1", types.DefaultNetworkControllerName)
				podReRoutePolicy := getReRoutePolicy(egressPodIP[0].String(), "4", "reroute-UUID", nodeLogicalRouterIPv4,
					getEgressIPLRPReRouteDbIDs(eIP.Name, egressPod.Namespace, egressPod.Name, IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs())
				node1GR.Nat = []string{"egressip-nat-UUID"}

				node1Switch.QOSRules = []string{"default-QoS-UUID"}

				egressSVCServedPodsASv4, _ := buildEgressServiceAddressSets(nil)
				egressIPServedPodsASv4, _ := buildEgressIPServedPodsAddressSets([]string{podV4IP}, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName)
				egressNodeIPsASv4, _ := buildEgressIPNodeAddressSets([]string{node1IPv4})
				expectedDatabaseStatewithPod := []libovsdbtest.TestData{
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
							egressIPServedPodsASv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": types.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					getNoReRouteReplyTrafficPolicy(types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
					podEIPSNAT, &nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					}, &nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-service-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					}, podReRoutePolicy, &nbdb.LogicalRouter{
						Name: types.OVNClusterRouter,
						UUID: types.OVNClusterRouter + "-UUID",
						Policies: []string{"no-reroute-UUID", "no-reroute-service-UUID", "reroute-UUID", "default-no-reroute-node-UUID",
							"default-no-reroute-reply-traffic"},
					}, node1GR, node1LSP,
					&nbdb.LogicalRouterPort{
						UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID",
						Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name,
						Networks: []string{nodeLogicalRouterIfAddrV4},
					}, node1Switch,
					&nbdb.LogicalSwitch{
						UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
						Name:  types.ExternalSwitchPrefix + node1Name,
						Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
					},
					getDefaultQoSRule(false, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
					egressSVCServedPodsASv4,
					egressIPServedPodsASv4,
					egressNodeIPsASv4,
				}

				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseStatewithPod))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should only get assigned EgressIPs which matches their subnet when the node is tagged", func() {
			app.Action = func(*cli.Context) error {

				egressIP := "192.168.126.101"
				node1IPv4 := "192.168.128.202"
				node1IPv4CIDR := node1IPv4 + "/24"
				node1IPv6 := "::feff:c0a8:8e0c"
				node1IPv6CIDR := node1IPv6 + "/64"
				node2IPv4 := "192.168.126.51"
				node2IPv4CIDR := node2IPv4 + "/24"

				annotations := map[string]string{
					"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4CIDR, node1IPv6CIDR),
					"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":[\"%s\", \"%s\"]}", v4Node1Subnet, v6Node1Subnet),
					util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\",\"%s\"]", node1IPv4CIDR, node1IPv6CIDR),
				}
				node1 := getNodeObj(node1Name, annotations, map[string]string{})
				annotations = map[string]string{
					"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node2IPv4CIDR, ""),
					"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":\"%s\"}", v4Node2Subnet),
					util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node2IPv4CIDR),
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
				node1Switch := &nbdb.LogicalSwitch{
					UUID: node1Name + "-UUID",
					Name: node1Name,
				}
				node2Switch := &nbdb.LogicalSwitch{
					UUID: node2Name + "-UUID",
					Name: node2Name,
				}

				fakeOvn.startWithDBSetup(
					libovsdbtest.TestSetup{
						NBData: []libovsdbtest.TestData{
							&nbdb.LogicalRouter{
								Name: types.OVNClusterRouter,
								UUID: types.OVNClusterRouter + "-UUID",
							},
							&nbdb.LogicalRouter{
								Name: types.GWRouterPrefix + node1.Name,
								UUID: types.GWRouterPrefix + node1.Name + "-UUID",
							},
							&nbdb.LogicalRouter{
								Name: types.GWRouterPrefix + node2.Name,
								UUID: types.GWRouterPrefix + node2.Name + "-UUID",
							},
							&nbdb.LogicalSwitchPort{
								UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID",
								Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
								Type: "router",
								Options: map[string]string{
									libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
									"nat-addresses":             "router",
									"exclude-lb-vips-from-garp": "true",
								},
							},
							&nbdb.LogicalSwitchPort{
								UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID",
								Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
								Type: "router",
								Options: map[string]string{
									libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
									"nat-addresses":             "router",
									"exclude-lb-vips-from-garp": "true",
								},
							},
							&nbdb.LogicalSwitch{
								UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
								Name:  types.ExternalSwitchPrefix + node1Name,
								Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
							},
							&nbdb.LogicalSwitch{
								UUID:  types.ExternalSwitchPrefix + node2Name + "-UUID",
								Name:  types.ExternalSwitchPrefix + node2Name,
								Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
							},
							node1Switch,
							node2Switch,
						},
					},
					&egressipv1.EgressIPList{
						Items: []egressipv1.EgressIP{eIP},
					},
					&corev1.NodeList{
						Items: []corev1.Node{node1, node2},
					})

				err := fakeOvn.controller.WatchEgressIPPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIPNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressNodes()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				egressSVCServedPodsASv4, _ := buildEgressServiceAddressSets(nil)
				egressIPServedPodsASv4, _ := buildEgressIPServedPodsAddressSets(nil, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName)
				egressNodeIPsASv4, _ := buildEgressIPNodeAddressSets([]string{node1IPv4, node2IPv4})

				node1Switch.QOSRules = []string{"default-QoS-UUID"}
				expectedDatabaseState := []libovsdbtest.TestData{
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
							egressIPServedPodsASv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": types.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					getNoReRouteReplyTrafficPolicy(types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
					&nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-service-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouter{
						Name:     types.OVNClusterRouter,
						UUID:     types.OVNClusterRouter + "-UUID",
						Policies: []string{"no-reroute-UUID", "no-reroute-service-UUID", "default-no-reroute-node-UUID", "default-no-reroute-reply-traffic"},
					},
					&nbdb.LogicalRouter{
						Name: types.GWRouterPrefix + node1.Name,
						UUID: types.GWRouterPrefix + node1.Name + "-UUID",
					},
					&nbdb.LogicalRouter{
						Name: types.GWRouterPrefix + node2.Name,
						UUID: types.GWRouterPrefix + node2.Name + "-UUID",
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
						Type: "router",
						Options: map[string]string{
							libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
						Type: "router",
						Options: map[string]string{
							libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalSwitch{
						UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
						Name:  types.ExternalSwitchPrefix + node1Name,
						Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
					},
					&nbdb.LogicalSwitch{
						UUID:  types.ExternalSwitchPrefix + node2Name + "-UUID",
						Name:  types.ExternalSwitchPrefix + node2Name,
						Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
					},
					node1Switch,
					getDefaultQoSRule(false, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
					node2Switch,
					egressSVCServedPodsASv4,
					egressIPServedPodsASv4,
					egressNodeIPsASv4,
				}
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
				gomega.Eventually(eIP.Status.Items).Should(gomega.BeEmpty())

				node1.Labels = map[string]string{
					"k8s.ovn.org/egress-assignable": "",
				}

				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), &node1, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				expectedDatabaseState = []libovsdbtest.TestData{
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
							egressIPServedPodsASv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": types.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					getNoReRouteReplyTrafficPolicy(types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
					&nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-service-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouter{
						Name:     types.OVNClusterRouter,
						UUID:     types.OVNClusterRouter + "-UUID",
						Policies: []string{"no-reroute-UUID", "no-reroute-service-UUID", "default-no-reroute-node-UUID", "default-no-reroute-reply-traffic"},
					},
					&nbdb.LogicalRouter{
						Name: types.GWRouterPrefix + node1.Name,
						UUID: types.GWRouterPrefix + node1.Name + "-UUID",
					},
					&nbdb.LogicalRouter{
						Name: types.GWRouterPrefix + node2.Name,
						UUID: types.GWRouterPrefix + node2.Name + "-UUID",
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
						Type: "router",
						Options: map[string]string{
							libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
						Type: "router",
						Options: map[string]string{
							libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalSwitch{
						UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
						Name:  types.ExternalSwitchPrefix + node1Name,
						Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
					},
					&nbdb.LogicalSwitch{
						UUID:  types.ExternalSwitchPrefix + node2Name + "-UUID",
						Name:  types.ExternalSwitchPrefix + node2Name,
						Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
					},
					node1Switch,
					getDefaultQoSRule(false, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
					node2Switch,
					egressSVCServedPodsASv4,
					egressIPServedPodsASv4,
					egressNodeIPsASv4,
				}
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(0))
				gomega.Eventually(getEgressIPReassignmentCount).Should(gomega.Equal(1))

				node2.Labels = map[string]string{
					"k8s.ovn.org/egress-assignable": "",
				}

				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), &node2, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				fakeOvn.patchEgressIPObj(node2Name, egressIPName, egressIP)
				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))

				egressIPs, nodes := getEgressIPStatus(egressIPName)
				gomega.Expect(nodes[0]).To(gomega.Equal(node2.Name))
				gomega.Expect(egressIPs[0]).To(gomega.Equal(egressIP))
				gomega.Eventually(getEgressIPReassignmentCount).Should(gomega.Equal(0))
				expectedDatabaseState = []libovsdbtest.TestData{
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
							egressIPServedPodsASv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": types.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					getNoReRouteReplyTrafficPolicy(types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
					&nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-service-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouter{
						Name:     types.OVNClusterRouter,
						UUID:     types.OVNClusterRouter + "-UUID",
						Policies: []string{"no-reroute-UUID", "no-reroute-service-UUID", "default-no-reroute-node-UUID", "default-no-reroute-reply-traffic"},
					},
					&nbdb.LogicalRouter{
						Name: types.GWRouterPrefix + node1.Name,
						UUID: types.GWRouterPrefix + node1.Name + "-UUID",
					},
					&nbdb.LogicalRouter{
						Name: types.GWRouterPrefix + node2.Name,
						UUID: types.GWRouterPrefix + node2.Name + "-UUID",
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
						Type: "router",
						Options: map[string]string{
							libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
						Type: "router",
						Options: map[string]string{
							libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalSwitch{
						UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
						Name:  types.ExternalSwitchPrefix + node1Name,
						Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
					},
					&nbdb.LogicalSwitch{
						UUID:  types.ExternalSwitchPrefix + node2Name + "-UUID",
						Name:  types.ExternalSwitchPrefix + node2Name,
						Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
					},
					node1Switch,
					getDefaultQoSRule(false, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
					node2Switch,
					egressSVCServedPodsASv4,
					egressIPServedPodsASv4,
					egressNodeIPsASv4,
				}
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should try re-assigning EgressIP until all defined egress IPs are assigned", func() {
			app.Action = func(*cli.Context) error {

				egressIP1 := "192.168.126.101"
				egressIP2 := "192.168.126.102"
				node1IPv4 := "192.168.126.12"
				node1IPv4CIDR := node1IPv4 + "/24"
				node2IPv4 := "192.168.126.51"
				node2IPv4CIDR := node2IPv4 + "/24"

				annotations := map[string]string{
					"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4CIDR, ""),
					"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":\"%s\"}", v4Node1Subnet),
					util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node1IPv4CIDR),
				}
				node1 := getNodeObj(node1Name, annotations, map[string]string{})
				annotations = map[string]string{
					"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node2IPv4CIDR, ""),
					"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":\"%s\"}", v4Node2Subnet),
					util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node2IPv4CIDR),
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
				node1Switch := &nbdb.LogicalSwitch{
					UUID: node1Name + "-UUID",
					Name: node1Name,
				}
				node2Switch := &nbdb.LogicalSwitch{
					UUID: node2Name + "-UUID",
					Name: node2Name,
				}

				fakeOvn.startWithDBSetup(
					libovsdbtest.TestSetup{
						NBData: []libovsdbtest.TestData{
							&nbdb.LogicalRouter{
								Name: types.OVNClusterRouter,
								UUID: types.OVNClusterRouter + "-UUID",
							},
							&nbdb.LogicalRouter{
								Name: types.GWRouterPrefix + node1.Name,
								UUID: types.GWRouterPrefix + node1.Name + "-UUID",
							},
							&nbdb.LogicalRouter{
								Name: types.GWRouterPrefix + node2.Name,
								UUID: types.GWRouterPrefix + node2.Name + "-UUID",
							},
							&nbdb.LogicalSwitchPort{
								UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID",
								Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
								Type: "router",
								Options: map[string]string{
									libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
									"nat-addresses":             "router",
									"exclude-lb-vips-from-garp": "true",
								},
							},
							&nbdb.LogicalSwitchPort{
								UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID",
								Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
								Type: "router",
								Options: map[string]string{
									libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
									"nat-addresses":             "router",
									"exclude-lb-vips-from-garp": "true",
								},
							},
							&nbdb.LogicalSwitch{
								UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
								Name:  types.ExternalSwitchPrefix + node1Name,
								Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
							},
							&nbdb.LogicalSwitch{
								UUID:  types.ExternalSwitchPrefix + node2Name + "-UUID",
								Name:  types.ExternalSwitchPrefix + node2Name,
								Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
							},
							node1Switch,
							node2Switch,
						},
					},
					&egressipv1.EgressIPList{
						Items: []egressipv1.EgressIP{eIP},
					},
					&corev1.NodeList{
						Items: []corev1.Node{node1, node2},
					})

				err := fakeOvn.controller.WatchEgressIPPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIPNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressNodes()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				node1Switch.QOSRules = []string{"default-QoS-UUID"}

				egressSVCServedPodsASv4, _ := buildEgressServiceAddressSets(nil)
				egressIPServedPodsASv4, _ := buildEgressIPServedPodsAddressSets(nil, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName)
				egressNodeIPsASv4, _ := buildEgressIPNodeAddressSets([]string{node1IPv4, node2IPv4})

				expectedDatabaseState := []libovsdbtest.TestData{
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
							egressIPServedPodsASv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": types.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					getNoReRouteReplyTrafficPolicy(types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
					&nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-service-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouter{
						Name:     types.OVNClusterRouter,
						UUID:     types.OVNClusterRouter + "-UUID",
						Policies: []string{"no-reroute-UUID", "no-reroute-service-UUID", "default-no-reroute-node-UUID", "default-no-reroute-reply-traffic"},
					},
					&nbdb.LogicalRouter{
						Name: types.GWRouterPrefix + node1.Name,
						UUID: types.GWRouterPrefix + node1.Name + "-UUID",
					},
					&nbdb.LogicalRouter{
						Name: types.GWRouterPrefix + node2.Name,
						UUID: types.GWRouterPrefix + node2.Name + "-UUID",
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
						Type: "router",
						Options: map[string]string{
							libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
						Type: "router",
						Options: map[string]string{
							libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalSwitch{
						UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
						Name:  types.ExternalSwitchPrefix + node1Name,
						Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
					},
					&nbdb.LogicalSwitch{
						UUID:  types.ExternalSwitchPrefix + node2Name + "-UUID",
						Name:  types.ExternalSwitchPrefix + node2Name,
						Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
					},
					node1Switch,
					getDefaultQoSRule(false, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
					node2Switch,
					egressSVCServedPodsASv4,
					egressIPServedPodsASv4,
					egressNodeIPsASv4,
				}
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(0))

				node1.Labels = map[string]string{
					"k8s.ovn.org/egress-assignable": "",
				}

				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), &node1, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.patchEgressIPObj(node1Name, egressIPName, egressIP1)
				expectedDatabaseState = []libovsdbtest.TestData{
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
							egressIPServedPodsASv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": types.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					getNoReRouteReplyTrafficPolicy(types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
					&nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-service-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouter{
						Name:     types.OVNClusterRouter,
						UUID:     types.OVNClusterRouter + "-UUID",
						Policies: []string{"no-reroute-UUID", "no-reroute-service-UUID", "default-no-reroute-node-UUID", "default-no-reroute-reply-traffic"},
					},
					&nbdb.LogicalRouter{
						Name: types.GWRouterPrefix + node1.Name,
						UUID: types.GWRouterPrefix + node1.Name + "-UUID",
					},
					&nbdb.LogicalRouter{
						Name: types.GWRouterPrefix + node2.Name,
						UUID: types.GWRouterPrefix + node2.Name + "-UUID",
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
						Type: "router",
						Options: map[string]string{
							libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
						Type: "router",
						Options: map[string]string{
							libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalSwitch{
						UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
						Name:  types.ExternalSwitchPrefix + node1Name,
						Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
					},
					&nbdb.LogicalSwitch{
						UUID:  types.ExternalSwitchPrefix + node2Name + "-UUID",
						Name:  types.ExternalSwitchPrefix + node2Name,
						Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
					},
					node1Switch,
					getDefaultQoSRule(false, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
					node2Switch,
					egressSVCServedPodsASv4,
					egressIPServedPodsASv4,
					egressNodeIPsASv4,
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
				// For the sake of unit testing the EgressIP controller we need to patch the EgressIP object manually.
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
				err = fakeOvn.controller.eIPC.patchReplaceEgressIPStatus(egressIPName, status)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(2))
				gomega.Eventually(getEgressIPReassignmentCount).Should(gomega.Equal(0))

				expectedDatabaseState = []libovsdbtest.TestData{
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
							egressIPServedPodsASv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": types.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					getNoReRouteReplyTrafficPolicy(types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
					&nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-service-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouter{
						Name:     types.OVNClusterRouter,
						UUID:     types.OVNClusterRouter + "-UUID",
						Policies: []string{"no-reroute-UUID", "no-reroute-service-UUID", "default-no-reroute-node-UUID", "default-no-reroute-reply-traffic"},
					},
					&nbdb.LogicalRouter{
						Name: types.GWRouterPrefix + node1.Name,
						UUID: types.GWRouterPrefix + node1.Name + "-UUID",
					},
					&nbdb.LogicalRouter{
						Name: types.GWRouterPrefix + node2.Name,
						UUID: types.GWRouterPrefix + node2.Name + "-UUID",
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
						Type: "router",
						Options: map[string]string{
							libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
						Type: "router",
						Options: map[string]string{
							libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalSwitch{
						UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
						Name:  types.ExternalSwitchPrefix + node1Name,
						Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
					},
					&nbdb.LogicalSwitch{
						UUID:  types.ExternalSwitchPrefix + node2Name + "-UUID",
						Name:  types.ExternalSwitchPrefix + node2Name,
						Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
					},
					node1Switch,
					getDefaultQoSRule(false, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
					node2Switch,
					egressSVCServedPodsASv4,
					egressIPServedPodsASv4,
					egressNodeIPsASv4,
				}
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("ensure egress ip entries are not created when pod is already moved into completed state", func() {
			app.Action = func(*cli.Context) error {
				config.Gateway.DisableSNATMultipleGWs = true
				egressIP := "192.168.126.25"
				node1IPv4 := "192.168.126.12"
				node1IPv4CIDR := node1IPv4 + "/24"

				egressPod := *ovntest.NewPodWithLabels(eipNamespace, podName, node1Name, podV4IP, egressPodLabel)
				egressPod.Status.Phase = corev1.PodSucceeded
				egressNamespace := ovntest.NewNamespace(eipNamespace)

				node1 := corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: node1Name,
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\"}", node1IPv4CIDR),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":\"%s\"}", v4Node1Subnet),
							"k8s.ovn.org/l3-gateway-config":   `{"default":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"192.168.126.12/24", "next-hop":"192.168.126.1"}}`,
							"k8s.ovn.org/node-chassis-id":     "79fdcfc4-6fe6-4cd3-8242-c0f85a4668ec",
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node1IPv4CIDR),
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
					Name:  types.GWRouterPrefix + node1.Name,
					UUID:  types.GWRouterPrefix + node1.Name + "-UUID",
					Ports: []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID"},
				}
				node1LSP := &nbdb.LogicalSwitchPort{
					UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID",
					Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
					Type: "router",
					Options: map[string]string{
						libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
						"nat-addresses":             "router",
						"exclude-lb-vips-from-garp": "true",
					},
				}
				fakeOvn.startWithDBSetup(
					libovsdbtest.TestSetup{
						NBData: []libovsdbtest.TestData{
							&nbdb.LogicalRouter{
								Name: types.OVNClusterRouter,
								UUID: types.OVNClusterRouter + "-UUID",
							},
							node1GR,
							node1LSP,
							&nbdb.LogicalRouterPort{
								UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID",
								Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name,
								Networks: []string{nodeLogicalRouterIfAddrV4},
							},
							node1Switch,
							&nbdb.LogicalSwitch{
								UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
								Name:  types.ExternalSwitchPrefix + node1Name,
								Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
							},
						},
					},
					&egressipv1.EgressIPList{
						Items: []egressipv1.EgressIP{eIP},
					},
					&corev1.NodeList{
						Items: []corev1.Node{node1},
					},
					&corev1.NamespaceList{
						Items: []corev1.Namespace{*egressNamespace},
					},
					&corev1.PodList{
						Items: []corev1.Pod{egressPod},
					},
				)
				i, n, _ := net.ParseCIDR(podV4IP + "/23")
				n.IP = i
				fakeOvn.controller.logicalPortCache.add(&egressPod, "", types.DefaultNetworkName, "", nil, []*net.IPNet{n})

				err := fakeOvn.controller.WatchPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIPPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIPNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressNodes()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				fakeOvn.patchEgressIPObj(node1Name, egressIPName, egressIP)

				egressPodPortInfo, err := fakeOvn.controller.logicalPortCache.get(&egressPod, types.DefaultNetworkName)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				ePod, err := fakeOvn.fakeClient.KubeClient.CoreV1().Pods(egressPod.Namespace).Get(context.TODO(), egressPod.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				egressPodIP, err := util.GetPodIPsOfNetwork(ePod, &util.DefaultNetInfo{}, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				egressNetPodIP, _, err := net.ParseCIDR(egressPodPortInfo.ips[0].String())
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(egressNetPodIP.String()).To(gomega.Equal(egressPodIP[0].String()))

				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
				gomega.Eventually(getEgressIPReassignmentCount).Should(gomega.Equal(0))
				egressIPs, nodes := getEgressIPStatus(egressIPName)
				gomega.Expect(nodes[0]).To(gomega.Equal(node1.Name))
				gomega.Expect(egressIPs[0]).To(gomega.Equal(egressIP))

				egressSVCServedPodsASv4, _ := buildEgressServiceAddressSets(nil)
				egressIPServedPodsASv4, _ := buildEgressIPServedPodsAddressSets(nil, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName)
				egressNodeIPsASv4, _ := buildEgressIPNodeAddressSets([]string{node1IPv4})

				node1Switch.QOSRules = []string{"default-QoS-UUID"}
				expectedDatabaseStatewithPod := []libovsdbtest.TestData{
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
							egressIPServedPodsASv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": types.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					getNoReRouteReplyTrafficPolicy(types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
					&nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					}, &nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-service-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					}, &nbdb.LogicalRouter{
						Name:     types.OVNClusterRouter,
						UUID:     types.OVNClusterRouter + "-UUID",
						Policies: []string{"no-reroute-UUID", "no-reroute-service-UUID", "default-no-reroute-node-UUID", "default-no-reroute-reply-traffic"},
					}, node1GR, node1LSP,
					&nbdb.LogicalRouterPort{
						UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID",
						Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name,
						Networks: []string{"100.64.0.2/29"},
					}, node1Switch,
					&nbdb.LogicalSwitch{
						UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
						Name:  types.ExternalSwitchPrefix + node1Name,
						Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
					},
					getDefaultQoSRule(false, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
					egressSVCServedPodsASv4,
					egressIPServedPodsASv4,
					egressNodeIPsASv4,
				}

				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseStatewithPod))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("ensure external gw pod snat entry is not created back when pod is moved into completed state", func() {
			app.Action = func(*cli.Context) error {
				config.Gateway.DisableSNATMultipleGWs = true
				egressIP := "192.168.126.25"
				node1IPv4 := "192.168.126.12"
				node1IPv4CIDR := node1IPv4 + "/24"

				egressPod := *ovntest.NewPodWithLabels(eipNamespace, podName, node1Name, podV4IP, egressPodLabel)
				egressNamespace := ovntest.NewNamespace(eipNamespace)

				node1 := corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: node1Name,
						Annotations: map[string]string{
							"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\"}", node1IPv4CIDR),
							"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":\"%s\"}", v4Node1Subnet),
							"k8s.ovn.org/l3-gateway-config":   `{"default":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"192.168.126.12/24", "next-hop":"192.168.126.1"}}`,
							"k8s.ovn.org/node-chassis-id":     "79fdcfc4-6fe6-4cd3-8242-c0f85a4668ec",
							util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node1IPv4CIDR),
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
					Name:  types.GWRouterPrefix + node1.Name,
					UUID:  types.GWRouterPrefix + node1.Name + "-UUID",
					Ports: []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID"},
				}
				node1LSP := &nbdb.LogicalSwitchPort{
					UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID",
					Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
					Type: "router",
					Options: map[string]string{
						libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
						"nat-addresses":             "router",
						"exclude-lb-vips-from-garp": "true",
					},
				}
				fakeOvn.startWithDBSetup(
					libovsdbtest.TestSetup{
						NBData: []libovsdbtest.TestData{
							&nbdb.LogicalRouter{
								Name: types.OVNClusterRouter,
								UUID: types.OVNClusterRouter + "-UUID",
							},
							node1GR,
							node1LSP,
							&nbdb.LogicalRouterPort{
								UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID",
								Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name,
								Networks: []string{nodeLogicalRouterIfAddrV4},
							},
							node1Switch,
							&nbdb.LogicalSwitch{
								UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
								Name:  types.ExternalSwitchPrefix + node1Name,
								Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
							},
						},
					},
					&egressipv1.EgressIPList{
						Items: []egressipv1.EgressIP{eIP},
					},
					&corev1.NodeList{
						Items: []corev1.Node{node1},
					},
					&corev1.NamespaceList{
						Items: []corev1.Namespace{*egressNamespace},
					},
					&corev1.PodList{
						Items: []corev1.Pod{egressPod},
					},
				)

				i, n, _ := net.ParseCIDR(podV4IP + "/23")
				n.IP = i
				fakeOvn.controller.logicalPortCache.add(&egressPod, "", types.DefaultNetworkName, "", nil, []*net.IPNet{n})

				err := fakeOvn.controller.WatchPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIPPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIPNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressNodes()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				fakeOvn.patchEgressIPObj(node1Name, egressIPName, egressIP)

				egressPodPortInfo, err := fakeOvn.controller.logicalPortCache.get(&egressPod, types.DefaultNetworkName)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				ePod, err := fakeOvn.fakeClient.KubeClient.CoreV1().Pods(egressPod.Namespace).Get(context.TODO(), egressPod.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				egressPodIP, err := util.GetPodIPsOfNetwork(ePod, &util.DefaultNetInfo{}, nil)
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
					UUID:        "egressip-nat-UUID1",
					LogicalIP:   podV4IP,
					ExternalIP:  egressIP,
					ExternalIDs: getEgressIPNATDbIDs(egressIPName, egressPod.Namespace, egressPod.Name, IPFamilyValueV4, fakeOvn.controller.controllerName).GetExternalIDs(),
					Type:        nbdb.NATTypeSNAT,
					LogicalPort: ptr.To("k8s-node1"),
					Options: map[string]string{
						"stateless": "false",
					},
				}
				podReRoutePolicy := &nbdb.LogicalRouterPolicy{
					Priority:    types.EgressIPReroutePriority,
					Match:       fmt.Sprintf("ip4.src == %s", egressPodIP[0].String()),
					Action:      nbdb.LogicalRouterPolicyActionReroute,
					Nexthops:    nodeLogicalRouterIPv4,
					ExternalIDs: getEgressIPLRPReRouteDbIDs(eIP.Name, egressPod.Namespace, egressPod.Name, IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					UUID:        "reroute-UUID1",
				}
				node1GR.Nat = []string{"egressip-nat-UUID1"}
				egressSVCServedPodsASv4, _ := buildEgressServiceAddressSets(nil)
				egressIPServedPodsASv4, _ := buildEgressIPServedPodsAddressSets([]string{podV4IP}, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName)
				egressNodeIPsASv4, _ := buildEgressIPNodeAddressSets([]string{node1IPv4})

				node1Switch.QOSRules = []string{"default-QoS-UUID"}
				expectedDatabaseStatewithPod := []libovsdbtest.TestData{
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
							egressIPServedPodsASv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": types.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					getNoReRouteReplyTrafficPolicy(types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
					podEIPSNAT, &nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					}, &nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-service-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					}, podReRoutePolicy, &nbdb.LogicalRouter{
						Name: types.OVNClusterRouter,
						UUID: types.OVNClusterRouter + "-UUID",
						Policies: []string{"no-reroute-UUID", "no-reroute-service-UUID", "reroute-UUID1", "default-no-reroute-node-UUID",
							"default-no-reroute-reply-traffic"},
					}, node1GR, node1LSP,
					&nbdb.LogicalRouterPort{
						UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID",
						Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name,
						Networks: []string{nodeLogicalRouterIfAddrV4},
					}, node1Switch,
					&nbdb.LogicalSwitch{
						UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
						Name:  types.ExternalSwitchPrefix + node1Name,
						Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
					},
					getDefaultQoSRule(false, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
					egressSVCServedPodsASv4,
					egressIPServedPodsASv4,
					egressNodeIPsASv4,
				}

				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseStatewithPod))

				egressPod.Status.Phase = corev1.PodSucceeded
				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(egressPod.Namespace).Update(context.TODO(), &egressPod, metav1.UpdateOptions{})
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				// Wait for pod to get moved into succeeded state.
				gomega.Eventually(func() corev1.PodPhase {
					egressPod1, _ := fakeOvn.watcher.GetPod(egressPod.Namespace, egressPod.Name)
					return egressPod1.Status.Phase
				}, 5).Should(gomega.Equal(corev1.PodSucceeded))

				node1GR.Nat = []string{}
				egressIPServedPodsASv4, _ = buildEgressIPServedPodsAddressSets(nil, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName)

				expectedDatabaseStatewitCompletedPod := []libovsdbtest.TestData{
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
							egressIPServedPodsASv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": types.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					getNoReRouteReplyTrafficPolicy(types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
					&nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					}, &nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-service-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					}, &nbdb.LogicalRouter{
						Name:     types.OVNClusterRouter,
						UUID:     types.OVNClusterRouter + "-UUID",
						Policies: []string{"no-reroute-UUID", "no-reroute-service-UUID", "default-no-reroute-node-UUID", "default-no-reroute-reply-traffic"},
					}, node1GR, node1LSP,
					&nbdb.LogicalRouterPort{
						UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID",
						Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name,
						Networks: []string{nodeLogicalRouterIfAddrV4},
					}, node1Switch,
					&nbdb.LogicalSwitch{
						UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
						Name:  types.ExternalSwitchPrefix + node1Name,
						Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
					},
					getDefaultQoSRule(false, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
					egressSVCServedPodsASv4,
					egressIPServedPodsASv4,
					egressNodeIPsASv4,
				}

				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseStatewitCompletedPod))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.DescribeTable(
			"DualStack cluster with single stack egressIP removes the correct snat rule when DisableSNATMultipleGWs=true",
			func(
				egressIP net.IP,
			) {
				app.Action = func(ctx *cli.Context) error {
					gomega.Expect(ctx.Set("enable-egress-ip", "true")).To(gomega.Succeed())
					gomega.Expect(ctx.Set("egressip-node-healthcheck-port", "1234")).To(gomega.Succeed())

					gomega.Expect(ctx.Set("disable-snat-multiple-gws", "true")).To(gomega.Succeed())
					gomega.Expect(ctx.Set("cluster-subnets", fmt.Sprintf("%s/%d,%s/%d", "10.0.0.0/16", 24, "fd01::/48", 64))).To(gomega.Succeed())
					gomega.Expect(ctx.Set("k8s-service-cidrs", fmt.Sprintf("%s,%s", "172.30.0.0/16", "fd02::/112"))).To(gomega.Succeed())

					_, err := config.InitConfig(ctx, nil, nil)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					isEgressIPv6 := utilnet.IsIPv6(egressIP)
					node1IPv4 := "192.168.126.12"
					node1IPv4CIDR := node1IPv4 + "/24"
					expectedNatLogicalPort := "k8s-node1"

					egressPod := *ovntest.NewPodWithLabelsAllIPFamilies(eipNamespace, podName, node1Name, []string{podV4IP, podV6IP}, egressPodLabel)
					egressNamespace := ovntest.NewNamespace(eipNamespace)

					node1 := corev1.Node{
						ObjectMeta: metav1.ObjectMeta{
							Name: node1Name,
							Annotations: map[string]string{
								"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"fc00:f853:ccd:e793::13/64\"}", node1IPv4CIDR),
								"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":[\"%s\", \"%s\"]}", v4Node1Subnet, v6Node1Subnet),
								"k8s.ovn.org/l3-gateway-config":   "{\"default\":{\"mode\":\"shared\",\"bridge-id\":\"breth0\",\"interface-id\":\"breth0_ovn-worker\",\"mac-address\":\"3a:24:24:5d:85:aa\",\"ip-addresses\":[\"10.89.0.19/24\",\"fc00:f853:ccd:e793::13/64\"],\"next-hops\":[\"10.89.0.1\",\"fc00:f853:ccd:e793::1\"],\"node-port-enable\":\"true\",\"vlan-id\":\"0\"}}",
								"k8s.ovn.org/node-chassis-id":     "79fdcfc4-6fe6-4cd3-8242-c0f85a4668ec",
								util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\",\"%s\"]", node1IPv4CIDR, "fc00:f853:ccd:e793::13/64"),
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
							EgressIPs: []string{egressIP.String()},
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
						Name:  types.GWRouterPrefix + node1.Name,
						UUID:  types.GWRouterPrefix + node1.Name + "-UUID",
						Ports: []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID"},
						Nat:   []string{"egressip-nat-v4-UUID", "egressip-nat-v6-UUID"},
					}
					node1LSP := &nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
						Type: "router",
						Options: map[string]string{
							libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					}
					egressipv4NAT := &nbdb.NAT{
						UUID:        "egressip-nat-v4-UUID",
						LogicalIP:   podV4IP,
						ExternalIP:  "10.89.0.19", // nodes ip address
						Type:        nbdb.NATTypeSNAT,
						LogicalPort: &expectedNatLogicalPort,
						Options: map[string]string{
							"stateless": "false",
						},
					}
					egressipv6NAT := &nbdb.NAT{
						UUID:        "egressip-nat-v6-UUID",
						LogicalIP:   podV6IP,
						ExternalIP:  "fc00:f853:ccd:e793::13",
						Type:        nbdb.NATTypeSNAT,
						LogicalPort: &expectedNatLogicalPort,
						Options: map[string]string{
							"stateless": "false",
						},
					}
					// we only want to remove the SNAT of the podIP that matches the ip family of the EgressIP
					natToRemain := egressipv6NAT
					if isEgressIPv6 {
						natToRemain = egressipv4NAT
					}

					fakeOvn.startWithDBSetup(
						libovsdbtest.TestSetup{
							NBData: []libovsdbtest.TestData{
								&nbdb.LogicalRouter{
									Name: types.OVNClusterRouter,
									UUID: types.OVNClusterRouter + "-UUID",
								},
								node1GR,
								node1LSP,
								&nbdb.LogicalRouterPort{
									UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID",
									Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name,
									Networks: []string{nodeLogicalRouterIfAddrV4, nodeLogicalRouterIfAddrV6},
								},
								node1Switch,
								&nbdb.LogicalSwitch{
									UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
									Name:  types.ExternalSwitchPrefix + node1Name,
									Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
								},
								egressipv6NAT,
								egressipv4NAT,
							},
						},
						&egressipv1.EgressIPList{
							Items: []egressipv1.EgressIP{eIP},
						},
						&corev1.NodeList{
							Items: []corev1.Node{node1},
						},
						&corev1.NamespaceList{
							Items: []corev1.Namespace{*egressNamespace},
						},
						&corev1.PodList{
							Items: []corev1.Pod{egressPod},
						},
					)

					i, n, _ := net.ParseCIDR(podV4IP + "/23")
					n.IP = i
					ipv6, ipv6net, _ := net.ParseCIDR(podV6IP + "/23")
					ipv6net.IP = ipv6
					fakeOvn.controller.logicalPortCache.add(&egressPod, "", types.DefaultNetworkName, "", nil, []*net.IPNet{n, ipv6net})

					err = fakeOvn.controller.WatchPods()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchEgressIPPods()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchEgressIPNamespaces()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchEgressNodes()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchEgressIP()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					if !isEgressIPv6 {
						fakeOvn.patchEgressIPObj(node1Name, egressIPName, egressIP.String())
					} else {
						fakeOvn.patchEgressIPObj(node1Name, egressIPName, egressIP.String())
					}

					egressPodPortInfo, err := fakeOvn.controller.logicalPortCache.get(&egressPod, types.DefaultNetworkName)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					ePod, err := fakeOvn.fakeClient.KubeClient.CoreV1().Pods(egressPod.Namespace).Get(context.TODO(), egressPod.Name, metav1.GetOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					egressPodIP, err := util.GetPodIPsOfNetwork(ePod, &util.DefaultNetInfo{}, nil)
					index := 0 //ipv4 address at zero index
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					egressNetPodIP, _, err := net.ParseCIDR(egressPodPortInfo.ips[0].String())
					if isEgressIPv6 {
						egressNetPodIP, _, err = net.ParseCIDR(egressPodPortInfo.ips[1].String())
						index = 1
					}
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					gomega.Expect(egressNetPodIP.String()).To(gomega.Equal(egressPodIP[index].String()))
					gomega.Expect(egressPodPortInfo.expires.IsZero()).To(gomega.BeTrue())

					gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
					gomega.Eventually(getEgressIPReassignmentCount).Should(gomega.Equal(0))
					egressIPs, nodes := getEgressIPStatus(egressIPName)
					gomega.Expect(nodes[0]).To(gomega.Equal(node1.Name))
					gomega.Expect(egressIPs[0]).To(gomega.Equal(egressIP.String()))
					ipfamily := IPFamilyValueV4
					if isEgressIPv6 {
						ipfamily = IPFamilyValueV6
					}
					podEIPSNAT := &nbdb.NAT{
						UUID:        "egressip-nat-UUID1",
						ExternalIP:  egressIP.String(),
						ExternalIDs: getEgressIPNATDbIDs(egressIPName, egressPod.Namespace, egressPod.Name, ipfamily, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
						Type:        nbdb.NATTypeSNAT,
						LogicalPort: ptr.To("k8s-node1"),
						Options: map[string]string{
							"stateless": "false",
						},
					}
					if !isEgressIPv6 {
						podEIPSNAT.LogicalIP = podV4IP
					} else {
						podEIPSNAT.LogicalIP = podV6IP
					}
					podReRoutePolicy := &nbdb.LogicalRouterPolicy{
						Priority:    types.EgressIPReroutePriority,
						Match:       fmt.Sprintf("%s.src == %s", ipfamily, egressPodIP[index].String()),
						Action:      nbdb.LogicalRouterPolicyActionReroute,
						Nexthops:    nodeLogicalRouterIPv4,
						ExternalIDs: getEgressIPLRPReRouteDbIDs(eIP.Name, egressPod.Namespace, egressPod.Name, ipfamily, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
						UUID:        "reroute-UUID1",
					}
					if isEgressIPv6 {
						podReRoutePolicy.Nexthops = nodeLogicalRouterIPv6
					}

					node1GR.Nat = []string{"egressip-nat-UUID1", natToRemain.UUID}
					node1Switch.QOSRules = []string{"default-QoS-UUID", "default-QoSv6-UUID"}
					ovnClusterRouterPolicies := []string{"reroute-UUID1", "default-no-reroute-reply-traffic", "no-reroute-UUID", "no-reroute-v6-UUID",
						"no-reroute-service-UUID", "no-reroute-service-v6-UUID", "default-no-reroute-node-UUID", "default-no-reroute-node-v6-UUID"}
					egressSVCServedPodsASv4, egressSVCServedPodsASv6 := buildEgressServiceAddressSets(nil)
					egressIPServedPodsASv4, egressIPServedPodsASv6 := buildEgressIPServedPodsAddressSets([]string{podV4IP, podV6IP}, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName)
					egressNodeIPsASv4, egressNodeIPsASv6 := buildEgressIPNodeAddressSets([]string{node1IPv4, "fc00:f853:ccd:e793::13"})
					expectedDatabaseStatewithPod := []libovsdbtest.TestData{
						&nbdb.LogicalRouterPolicy{
							Priority: types.DefaultNoRereoutePriority,
							Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
								egressIPServedPodsASv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
							Action:      nbdb.LogicalRouterPolicyActionAllow,
							UUID:        "default-no-reroute-node-UUID",
							Options:     map[string]string{"pkt_mark": types.EgressIPNodeConnectionMark},
							ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
						},
						&nbdb.LogicalRouterPolicy{
							Priority: types.DefaultNoRereoutePriority,
							Match: fmt.Sprintf("(ip6.src == $%s || ip6.src == $%s) && ip6.dst == $%s",
								egressIPServedPodsASv6.Name, egressSVCServedPodsASv6.Name, egressNodeIPsASv6.Name),
							Action:      nbdb.LogicalRouterPolicyActionAllow,
							UUID:        "default-no-reroute-node-v6-UUID",
							Options:     map[string]string{"pkt_mark": types.EgressIPNodeConnectionMark},
							ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV6, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
						},
						&nbdb.LogicalRouterPolicy{
							Priority:    types.DefaultNoRereoutePriority,
							Match:       "ip4.src == 10.0.0.0/16 && ip4.dst == 10.0.0.0/16",
							Action:      nbdb.LogicalRouterPolicyActionAllow,
							UUID:        "no-reroute-UUID",
							ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
						},
						&nbdb.LogicalRouterPolicy{
							Priority:    types.DefaultNoRereoutePriority,
							Match:       "ip6.src == fd01::/48 && ip6.dst == fd01::/48",
							Action:      nbdb.LogicalRouterPolicyActionAllow,
							UUID:        "no-reroute-v6-UUID",
							ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV6, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
						},
						&nbdb.LogicalRouterPolicy{
							Priority:    types.DefaultNoRereoutePriority,
							Match:       fmt.Sprintf("ip4.src == 10.0.0.0/16 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
							Action:      nbdb.LogicalRouterPolicyActionAllow,
							UUID:        "no-reroute-service-UUID",
							ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
						},
						&nbdb.LogicalRouterPolicy{
							Priority:    types.DefaultNoRereoutePriority,
							Match:       fmt.Sprintf("ip6.src == fd01::/48 && ip6.dst == %s", config.Gateway.V6JoinSubnet),
							Action:      nbdb.LogicalRouterPolicyActionAllow,
							UUID:        "no-reroute-service-v6-UUID",
							ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV6, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
						},
						getNoReRouteReplyTrafficPolicy(types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
						podEIPSNAT,
						podReRoutePolicy, &nbdb.LogicalRouter{
							Name:     types.OVNClusterRouter,
							UUID:     types.OVNClusterRouter + "-UUID",
							Policies: ovnClusterRouterPolicies,
						}, node1GR, node1LSP, node1Switch,
						&nbdb.LogicalSwitch{
							UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
							Name:  types.ExternalSwitchPrefix + node1Name,
							Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
						},
						&nbdb.LogicalRouterPort{
							UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID",
							Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name,
							Networks: []string{nodeLogicalRouterIfAddrV4, nodeLogicalRouterIfAddrV6},
						},
						natToRemain,
						getDefaultQoSRule(false, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
						getDefaultQoSRule(true, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
						egressSVCServedPodsASv4,
						egressSVCServedPodsASv6,
						egressIPServedPodsASv4,
						egressIPServedPodsASv6,
						egressNodeIPsASv4,
						egressNodeIPsASv6,
					}
					gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseStatewithPod))

					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			},
			ginkgo.Entry(
				"When EgressIP is ipv4",
				net.ParseIP("192.168.126.101"),
			),
			ginkgo.Entry(
				"When EgressIP is ipv6",
				net.ParseIP("fc00:f853:0ccd:e793:ffff:ffff:ffff:0000"),
			),
		)

		ginkgo.It("should update SNAT and LRP nexthops during simultaneous EIP failover and ovnkube-controller restart", func() {
			app.Action = func(*cli.Context) error {
				config.Gateway.DisableSNATMultipleGWs = true

				egressIP := "192.168.126.101"
				node1IPv4 := "192.168.126.12"
				node1IPv4CIDR := node1IPv4 + "/24"
				node1IPv4TranSwitchIP := "100.88.0.2/16"
				node2IPv4 := "192.168.126.51"
				node2IPv4CIDR := node2IPv4 + "/24"
				node2IPv4TranSwitchIP := "100.88.0.3"
				node2IPv4TranSwitchIPCIDR := node2IPv4TranSwitchIP + "/16"
				node3IPv4 := "192.168.126.61"
				node3IPv4CIDR := node3IPv4 + "/24"
				node3IPv4TranSwitchIP := "100.88.0.4/16"

				egressPod := *ovntest.NewPodWithLabels(eipNamespace, podName, node1Name, podV4IP, egressPodLabel)
				egressPod2 := *ovntest.NewPodWithLabels(eipNamespace, podName2, node3Name, podV4IP2, egressPodLabel)
				egressNamespace := ovntest.NewNamespace(eipNamespace)

				nodes := getIPv4Nodes([]nodeInfo{{[]string{node1IPv4CIDR}, node1IPv4TranSwitchIP},
					{[]string{node2IPv4CIDR}, node2IPv4TranSwitchIPCIDR}, {[]string{node3IPv4CIDR}, node3IPv4TranSwitchIP}})
				node1 := nodes[0]
				node1.Labels = map[string]string{
					"k8s.ovn.org/egress-assignable": "",
				}
				node1.Annotations["k8s.ovn.org/l3-gateway-config"] = `{"default":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"192.168.126.12/24", "next-hop":"192.168.126.1"}}`
				node1.Annotations["k8s.ovn.org/node-chassis-id"] = "79fdcfc4-6fe6-4cd3-8242-c0f85a4668ec"
				node2 := nodes[1]
				node2.Labels = map[string]string{
					"k8s.ovn.org/egress-assignable": "",
				}
				node2.Annotations["k8s.ovn.org/l3-gateway-config"] = `{"default":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"192.168.126.51/24", "next-hop":"192.168.126.1"}}`
				node2.Annotations["k8s.ovn.org/node-chassis-id"] = "89fdcfc4-6fe6-4cd3-8242-c0f85a4668ec"
				node3 := nodes[2]
				node3.Annotations["k8s.ovn.org/l3-gateway-config"] = `{"default":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"192.168.126.75/24", "next-hop":"192.168.126.1"}}`
				node3.Annotations["k8s.ovn.org/node-chassis-id"] = "99fdcfc4-6fe6-4cd3-8242-c0f85a4668ec"

				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{egressIP},
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
								EgressIP: egressIP,
							},
						},
					},
				}
				node1Switch := &nbdb.LogicalSwitch{
					UUID: node1.Name + "-UUID",
					Name: node1.Name,
				}
				node2Switch := &nbdb.LogicalSwitch{
					UUID: node2.Name + "-UUID",
					Name: node2.Name,
				}
				node3Switch := &nbdb.LogicalSwitch{
					UUID: node3.Name + "-UUID",
					Name: node3.Name,
				}
				logicalRouterOptions := map[string]string{
					"dynamic_neigh_routers": "false",
				}
				fakeOvn.startWithDBSetup(
					libovsdbtest.TestSetup{
						NBData: []libovsdbtest.TestData{
							&nbdb.LogicalRouter{
								Name: types.OVNClusterRouter,
								UUID: types.OVNClusterRouter + "-UUID",
							},
							&nbdb.LogicalRouter{
								Name:    types.GWRouterPrefix + node1.Name,
								UUID:    types.GWRouterPrefix + node1.Name + "-UUID",
								Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID"},
								Options: logicalRouterOptions,
							},
							&nbdb.LogicalRouter{
								Name:    types.GWRouterPrefix + node2.Name,
								UUID:    types.GWRouterPrefix + node2.Name + "-UUID",
								Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID"},
								Options: logicalRouterOptions,
							},
							&nbdb.LogicalRouter{
								Name:    types.GWRouterPrefix + node3.Name,
								UUID:    types.GWRouterPrefix + node3.Name + "-UUID",
								Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node3.Name + "-UUID"},
								Options: logicalRouterOptions,
							},
							&nbdb.LogicalRouterPort{
								UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID",
								Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name,
								Networks: []string{nodeLogicalRouterIfAddrV4},
							},
							&nbdb.LogicalRouterPort{
								UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID",
								Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name,
								Networks: []string{node2LogicalRouterIfAddrV4},
							},
							&nbdb.LogicalRouterPort{
								UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node3.Name + "-UUID",
								Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node3.Name,
								Networks: []string{node3LogicalRouterIfAddrV4},
							},
							&nbdb.LogicalSwitchPort{
								UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID",
								Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
								Type: "router",
								Options: map[string]string{
									libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
									"nat-addresses":             "router",
									"exclude-lb-vips-from-garp": "true",
								},
							},
							&nbdb.LogicalSwitchPort{
								UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID",
								Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
								Type: "router",
								Options: map[string]string{
									libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
									"nat-addresses":             "router",
									"exclude-lb-vips-from-garp": "true",
								},
							},
							&nbdb.LogicalSwitchPort{
								UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node3Name + "-UUID",
								Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node3Name,
								Type: "router",
								Options: map[string]string{
									libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node3Name,
									"nat-addresses":             "router",
									"exclude-lb-vips-from-garp": "true",
								},
							},
							&nbdb.LogicalSwitch{
								UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
								Name:  types.ExternalSwitchPrefix + node1Name,
								Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
							},
							&nbdb.LogicalSwitch{
								UUID:  types.ExternalSwitchPrefix + node2Name + "-UUID",
								Name:  types.ExternalSwitchPrefix + node2Name,
								Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
							},
							&nbdb.LogicalSwitch{
								UUID:  types.ExternalSwitchPrefix + node3Name + "-UUID",
								Name:  types.ExternalSwitchPrefix + node3Name,
								Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node3Name + "-UUID"},
							},
							node1Switch,
							node2Switch,
							node3Switch,
						},
					},
					&egressipv1.EgressIPList{
						Items: []egressipv1.EgressIP{eIP},
					},
					&corev1.NodeList{
						Items: []corev1.Node{node1, node2, node3},
					},
					&corev1.NamespaceList{
						Items: []corev1.Namespace{*egressNamespace},
					},
					&corev1.PodList{
						Items: []corev1.Pod{egressPod, egressPod2},
					},
				)

				i, n, _ := net.ParseCIDR(podV4IP + "/23")
				n.IP = i
				fakeOvn.controller.logicalPortCache.add(&egressPod, "", types.DefaultNetworkName, "", nil, []*net.IPNet{n})
				i, n, _ = net.ParseCIDR(podV4IP2 + "/23")
				n.IP = i
				fakeOvn.controller.logicalPortCache.add(&egressPod2, "", types.DefaultNetworkName, "", nil, []*net.IPNet{n})

				err := fakeOvn.controller.WatchEgressIPPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIPNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressNodes()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// To simulate an ovnkube-controller restart, update the EIP object with the newly assigned node.
				// Then invoke reconcileEgressIP using only the updated EIP object to trigger the EgressIP add event.
				eIP.Status = egressipv1.EgressIPStatus{
					Items: []egressipv1.EgressIPStatusItem{
						{
							Node:     node2.Name,
							EgressIP: egressIP,
						},
					},
				}
				err = fakeOvn.controller.eIPC.reconcileEgressIP(nil, &eIP)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(getPodAssignmentState(&egressPod)).NotTo(gomega.BeNil())

				egressSVCServedPodsASv4, _ := buildEgressServiceAddressSets(nil)
				egressIPServedPodsASv4, _ := buildEgressIPServedPodsAddressSets([]string{podV4IP}, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName)
				egressNodeIPsASv4, _ := buildEgressIPNodeAddressSets([]string{node1IPv4, node2IPv4, node3IPv4})

				node1Switch.QOSRules = []string{"default-QoS-UUID"}
				expectedDatabaseState := []libovsdbtest.TestData{
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
							egressIPServedPodsASv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": types.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					getNoReRouteReplyTrafficPolicy(types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
					&nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-service-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    types.EgressIPReroutePriority,
						Match:       fmt.Sprintf("ip4.src == %s", egressPod.Status.PodIP),
						Action:      nbdb.LogicalRouterPolicyActionReroute,
						Nexthops:    []string{node2IPv4TranSwitchIP},
						ExternalIDs: getEgressIPLRPReRouteDbIDs(eIP.Name, egressPod.Namespace, egressPod.Name, IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
						UUID:        "reroute-UUID1",
					},
					&nbdb.LogicalRouter{
						Name:     types.OVNClusterRouter,
						UUID:     types.OVNClusterRouter + "-UUID",
						Policies: []string{"no-reroute-UUID", "no-reroute-service-UUID", "default-no-reroute-node-UUID", "default-no-reroute-reply-traffic", "reroute-UUID1"},
					},
					&nbdb.LogicalRouter{
						Name:    types.GWRouterPrefix + node1.Name,
						UUID:    types.GWRouterPrefix + node1.Name + "-UUID",
						Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID"},
						Nat:     []string{"pod-node-nat-UUID1"},
						Options: logicalRouterOptions,
					},
					&nbdb.LogicalRouter{
						Name:    types.GWRouterPrefix + node2.Name,
						UUID:    types.GWRouterPrefix + node2.Name + "-UUID",
						Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID"},
						Options: logicalRouterOptions,
					},
					&nbdb.LogicalRouter{
						Name:    types.GWRouterPrefix + node3.Name,
						UUID:    types.GWRouterPrefix + node3.Name + "-UUID",
						Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node3.Name + "-UUID"},
						Options: logicalRouterOptions,
					},
					&nbdb.NAT{
						UUID:        "pod-node-nat-UUID1",
						LogicalIP:   podV4IP,
						ExternalIP:  node1IPv4,
						ExternalIDs: nil,
						Type:        nbdb.NATTypeSNAT,
						Options: map[string]string{
							"stateless": "false",
						},
					},
					&nbdb.LogicalRouterPort{
						UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID",
						Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name,
						Networks: []string{node2LogicalRouterIfAddrV4},
					},
					&nbdb.LogicalRouterPort{
						UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID",
						Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name,
						Networks: []string{nodeLogicalRouterIfAddrV4},
					},
					&nbdb.LogicalRouterPort{
						UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node3.Name + "-UUID",
						Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node3.Name,
						Networks: []string{node3LogicalRouterIfAddrV4},
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
						Type: "router",
						Options: map[string]string{
							libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
						Type: "router",
						Options: map[string]string{
							libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node3Name + "-UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node3Name,
						Type: "router",
						Options: map[string]string{
							libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node3Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalSwitch{
						UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
						Name:  types.ExternalSwitchPrefix + node1Name,
						Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
					},
					&nbdb.LogicalSwitch{
						UUID:  types.ExternalSwitchPrefix + node2Name + "-UUID",
						Name:  types.ExternalSwitchPrefix + node2Name,
						Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
					},
					&nbdb.LogicalSwitch{
						UUID:  types.ExternalSwitchPrefix + node3Name + "-UUID",
						Name:  types.ExternalSwitchPrefix + node3Name,
						Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node3Name + "-UUID"},
					},
					node1Switch,
					node2Switch,
					node3Switch,
					getDefaultQoSRule(false, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
					egressSVCServedPodsASv4, egressIPServedPodsASv4, egressNodeIPsASv4,
				}
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
				// Since we called reconcileEgressIP manually with new eIP status in the above step, so update EIP object with
				// same status as well.
				_, err = fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Update(context.TODO(), &eIP, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				// Now delete local zone egressPod and check LRP is removed.
				err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(egressPod.Namespace).Delete(context.TODO(), egressPod.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				egressIPServedPodsASv4, _ = buildEgressIPServedPodsAddressSets([]string{}, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName)
				node1Switch.QOSRules = []string{"default-QoS-UUID"}
				expectedDatabaseState = []libovsdbtest.TestData{
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
							egressIPServedPodsASv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": types.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					getNoReRouteReplyTrafficPolicy(types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
					&nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-service-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouter{
						Name:     types.OVNClusterRouter,
						UUID:     types.OVNClusterRouter + "-UUID",
						Policies: []string{"no-reroute-UUID", "no-reroute-service-UUID", "default-no-reroute-node-UUID", "default-no-reroute-reply-traffic"},
					},
					&nbdb.LogicalRouter{
						Name:    types.GWRouterPrefix + node1.Name,
						UUID:    types.GWRouterPrefix + node1.Name + "-UUID",
						Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID"},
						Nat:     []string{"pod-node-nat-UUID1"},
						Options: logicalRouterOptions,
					},
					&nbdb.LogicalRouter{
						Name:    types.GWRouterPrefix + node2.Name,
						UUID:    types.GWRouterPrefix + node2.Name + "-UUID",
						Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID"},
						Options: logicalRouterOptions,
					},
					&nbdb.LogicalRouter{
						Name:    types.GWRouterPrefix + node3.Name,
						UUID:    types.GWRouterPrefix + node3.Name + "-UUID",
						Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node3.Name + "-UUID"},
						Options: logicalRouterOptions,
					},
					// we still expect the SNAT at the end because we are not watching pod events that would remove the SNAT on pod deletion
					&nbdb.NAT{
						UUID:        "pod-node-nat-UUID1",
						LogicalIP:   podV4IP,
						ExternalIP:  node1IPv4,
						ExternalIDs: nil,
						Type:        nbdb.NATTypeSNAT,
						Options: map[string]string{
							"stateless": "false",
						},
					},
					&nbdb.LogicalRouterPort{
						UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID",
						Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name,
						Networks: []string{node2LogicalRouterIfAddrV4},
					},
					&nbdb.LogicalRouterPort{
						UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID",
						Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name,
						Networks: []string{nodeLogicalRouterIfAddrV4},
					},
					&nbdb.LogicalRouterPort{
						UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node3.Name + "-UUID",
						Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node3.Name,
						Networks: []string{node3LogicalRouterIfAddrV4},
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
						Type: "router",
						Options: map[string]string{
							libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
						Type: "router",
						Options: map[string]string{
							libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node3Name + "-UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node3Name,
						Type: "router",
						Options: map[string]string{
							libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node3Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalSwitch{
						UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
						Name:  types.ExternalSwitchPrefix + node1Name,
						Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
					},
					&nbdb.LogicalSwitch{
						UUID:  types.ExternalSwitchPrefix + node2Name + "-UUID",
						Name:  types.ExternalSwitchPrefix + node2Name,
						Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
					},
					&nbdb.LogicalSwitch{
						UUID:  types.ExternalSwitchPrefix + node3Name + "-UUID",
						Name:  types.ExternalSwitchPrefix + node3Name,
						Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node3Name + "-UUID"},
					},
					node1Switch,
					node2Switch,
					node3Switch,
					getDefaultQoSRule(false, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
					egressSVCServedPodsASv4, egressIPServedPodsASv4, egressNodeIPsASv4,
				}
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.DescribeTable(
			"should ensure EgressIP skips host-network and pending pods from namespace",
			func(includeUnscheduled, includeScheduledButNoIP, includeHostNetwork bool, testPodNodeName string) {
				app.Action = func(*cli.Context) error {
					config.Gateway.DisableSNATMultipleGWs = true

					egressIP1 := "192.168.126.101"
					node1IPv4 := "192.168.126.12"
					node1IPv4CIDR := node1IPv4 + "/24"
					node2IPv4 := "192.168.126.51"
					node2IPv4CIDR := node2IPv4 + "/24"

					egressPod3 := *ovntest.NewPodWithLabels(eipNamespace, "egress-pod3", node1Name, podV4IP, egressPodLabel)
					egressPod4 := *ovntest.NewPodWithLabels(eipNamespace, "egress-pod4", node1Name, podV4IP2, egressPodLabel)
					egressNamespace := ovntest.NewNamespace(eipNamespace)
					annotations := map[string]string{
						"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\"}", node1IPv4CIDR),
						"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":\"%s\"}", v4Node1Subnet),
						"k8s.ovn.org/l3-gateway-config":   `{"default":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"192.168.126.12/24", "next-hop":"192.168.126.1"}}`,
						"k8s.ovn.org/node-chassis-id":     "79fdcfc4-6fe6-4cd3-8242-c0f85a4668ec",
						util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node1IPv4CIDR),
					}
					node1 := getNodeObj(node1Name, annotations, map[string]string{})
					annotations = map[string]string{
						"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\"}", node2IPv4CIDR),
						"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":\"%s\"}", v4Node2Subnet),
						"k8s.ovn.org/l3-gateway-config":   `{"default":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"192.168.126.51/24", "next-hop":"192.168.126.1"}}`,
						"k8s.ovn.org/node-chassis-id":     "89fdcfc4-6fe6-4cd3-8242-c0f85a4668ec",
						util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node2IPv4CIDR),
					}
					node2 := getNodeObj(node2Name, annotations, map[string]string{})

					eIP := egressipv1.EgressIP{
						ObjectMeta: newEgressIPMeta(egressIPName),
						Spec: egressipv1.EgressIPSpec{
							EgressIPs: []string{egressIP1},
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
					node2Switch := &nbdb.LogicalSwitch{
						UUID: node2.Name + "-UUID",
						Name: node2.Name,
					}

					pods := []corev1.Pod{egressPod3, egressPod4}
					if includeUnscheduled {
						unScheduledPod := corev1.Pod{
							ObjectMeta: ovntest.NewPodMeta(eipNamespace, "egress-pod", egressPodLabel),
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name:  "containerName",
										Image: "containerImage",
									},
								},
							},
							Status: corev1.PodStatus{
								Phase: corev1.PodPending,
							},
						}
						pods = append(pods, unScheduledPod)
					}
					var pendingScheduledPodWithNoIP *corev1.Pod
					if includeScheduledButNoIP {
						pendingScheduledPodWithNoIP = ovntest.NewPodWithLabels(eipNamespace, "egress-pod1", testPodNodeName, "", egressPodLabel)
						pendingScheduledPodWithNoIP.Status.Phase = corev1.PodPending
						pods = append(pods, *pendingScheduledPodWithNoIP)
					}
					if includeHostNetwork {
						hostNetworkIP := node1IPv4
						if testPodNodeName == node2Name {
							hostNetworkIP = node2IPv4
						}
						hnPod := *ovntest.NewPodWithLabels(eipNamespace, "egress-pod2", testPodNodeName, hostNetworkIP, egressPodLabel)
						hnPod.Spec.HostNetwork = true
						pods = append(pods, hnPod)
					}

					dynamicNeighRouters := "false"
					logicalRouterOptions := map[string]string{
						"dynamic_neigh_routers": dynamicNeighRouters,
					}
					expectedNatLogicalPort1 := "k8s-node1"
					fakeOvn.startWithDBSetup(
						libovsdbtest.TestSetup{
							NBData: []libovsdbtest.TestData{
								&nbdb.LogicalRouter{
									Name: types.OVNClusterRouter,
									UUID: types.OVNClusterRouter + "-UUID",
									// stale LRP reference to be removed by EIP controller sync function.
									Policies: []string{"hostnet-pod-reroute-UUID1"},
								},
								&nbdb.LogicalRouter{
									Name:  types.GWRouterPrefix + node1.Name,
									UUID:  types.GWRouterPrefix + node1.Name + "-UUID",
									Ports: []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID"},
									// stale NAT reference to be removed by EIP controller sync function.
									Nat:     []string{"hostnet-pod-egressip-nat-UUID1"},
									Options: logicalRouterOptions,
								},
								&nbdb.LogicalRouter{
									Name:    types.GWRouterPrefix + node2.Name,
									UUID:    types.GWRouterPrefix + node2.Name + "-UUID",
									Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID"},
									Options: logicalRouterOptions,
								},
								// stale LRP and SNAT entries (for a host networked pod) to be removed by EIP controller sync function.
								&nbdb.LogicalRouterPolicy{
									Priority:    types.EgressIPReroutePriority,
									Match:       fmt.Sprintf("ip4.src == %s", node2IPv4),
									Action:      nbdb.LogicalRouterPolicyActionReroute,
									Nexthops:    []string{"100.64.0.2"},
									ExternalIDs: getEgressIPLRPReRouteDbIDs(egressIPName, eipNamespace, "egress-pod2", IPFamilyValueV4, types.DefaultNetworkName, types.DefaultNetworkControllerName).GetExternalIDs(),
									UUID:        "hostnet-pod-reroute-UUID1",
								},
								&nbdb.NAT{
									UUID:        "hostnet-pod-egressip-nat-UUID1",
									LogicalIP:   node2IPv4,
									ExternalIP:  egressIP1,
									ExternalIDs: getEgressIPNATDbIDs(egressIPName, eipNamespace, "egress-pod2", IPFamilyValueV4, types.DefaultNetworkControllerName).GetExternalIDs(),
									Type:        nbdb.NATTypeSNAT,
									LogicalPort: &expectedNatLogicalPort1,
									Options: map[string]string{
										"stateless": "false",
									},
								},
								&nbdb.LogicalRouterPort{
									UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID",
									Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name,
									Networks: []string{nodeLogicalRouterIfAddrV4},
								},
								&nbdb.LogicalRouterPort{
									UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID",
									Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name,
									Networks: []string{node2LogicalRouterIfAddrV4},
								},
								&nbdb.LogicalSwitchPort{
									UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID",
									Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
									Type: "router",
									Options: map[string]string{
										libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
										"nat-addresses":             "router",
										"exclude-lb-vips-from-garp": "true",
									},
								},
								&nbdb.LogicalSwitchPort{
									UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID",
									Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
									Type: "router",
									Options: map[string]string{
										libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
										"nat-addresses":             "router",
										"exclude-lb-vips-from-garp": "true",
									},
								},
								&nbdb.LogicalSwitch{
									UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
									Name:  types.ExternalSwitchPrefix + node1Name,
									Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
								},
								&nbdb.LogicalSwitch{
									UUID:  types.ExternalSwitchPrefix + node2Name + "-UUID",
									Name:  types.ExternalSwitchPrefix + node2Name,
									Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
								},
								node1Switch,
								node2Switch,
							},
						},
						&egressipv1.EgressIPList{
							Items: []egressipv1.EgressIP{eIP},
						},
						&corev1.NodeList{
							Items: []corev1.Node{node1, node2},
						},
						&corev1.NamespaceList{
							Items: []corev1.Namespace{*egressNamespace},
						},
						&corev1.PodList{
							Items: pods,
						},
					)

					i, n, _ := net.ParseCIDR(podV4IP + "/23")
					n.IP = i
					fakeOvn.controller.logicalPortCache.add(&egressPod3, "", types.DefaultNetworkName, "", nil, []*net.IPNet{n})
					if pendingScheduledPodWithNoIP != nil {
						fakeOvn.controller.logicalPortCache.add(pendingScheduledPodWithNoIP, "", types.DefaultNetworkName, "", nil, []*net.IPNet{})
					}
					i, n, _ = net.ParseCIDR(podV4IP2 + "/23")
					n.IP = i
					fakeOvn.controller.logicalPortCache.add(&egressPod4, "", types.DefaultNetworkName, "", nil, []*net.IPNet{n})
					err := fakeOvn.controller.WatchEgressIPPods()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchEgressIPNamespaces()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchEgressNodes()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchEgressIP()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					egressSVCServedPodsASv4, _ := buildEgressServiceAddressSets(nil)
					egressIPServedPodsASv4, _ := buildEgressIPServedPodsAddressSets([]string{podV4IP, podV4IP2}, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName)
					egressNodeIPsASv4, _ := buildEgressIPNodeAddressSets([]string{node1IPv4, node2IPv4})

					node1Switch.QOSRules = []string{"default-QoS-UUID"}
					if node2.Name == fakeOvn.controller.nodeName {
						node2Switch.QOSRules = []string{"default-QoS-UUID"}
					}
					expectedDatabaseState := []libovsdbtest.TestData{
						&nbdb.LogicalRouterPolicy{
							Priority: types.DefaultNoRereoutePriority,
							Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
								egressIPServedPodsASv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
							Action:      nbdb.LogicalRouterPolicyActionAllow,
							UUID:        "default-no-reroute-node-UUID",
							Options:     map[string]string{"pkt_mark": types.EgressIPNodeConnectionMark},
							ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
						},
						getNoReRouteReplyTrafficPolicy(types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
						&nbdb.LogicalRouterPolicy{
							Priority:    types.DefaultNoRereoutePriority,
							Match:       "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
							Action:      nbdb.LogicalRouterPolicyActionAllow,
							UUID:        "no-reroute-UUID",
							ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
						},
						&nbdb.LogicalRouterPolicy{
							Priority:    types.DefaultNoRereoutePriority,
							Match:       fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
							Action:      nbdb.LogicalRouterPolicyActionAllow,
							UUID:        "no-reroute-service-UUID",
							ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
						},
						&nbdb.LogicalRouter{
							Name:     types.OVNClusterRouter,
							UUID:     types.OVNClusterRouter + "-UUID",
							Policies: []string{"no-reroute-UUID", "no-reroute-service-UUID", "default-no-reroute-node-UUID", "default-no-reroute-reply-traffic"},
						},
						&nbdb.LogicalRouter{
							Name:    types.GWRouterPrefix + node1.Name,
							UUID:    types.GWRouterPrefix + node1.Name + "-UUID",
							Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID"},
							Options: logicalRouterOptions,
						},
						&nbdb.LogicalRouter{
							Name:    types.GWRouterPrefix + node2.Name,
							UUID:    types.GWRouterPrefix + node2.Name + "-UUID",
							Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID"},
							Options: logicalRouterOptions,
						},
						&nbdb.LogicalRouterPort{
							UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID",
							Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name,
							Networks: []string{node2LogicalRouterIfAddrV4},
						},
						&nbdb.LogicalRouterPort{
							UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID",
							Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name,
							Networks: []string{nodeLogicalRouterIfAddrV4},
						},
						&nbdb.LogicalSwitchPort{
							UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID",
							Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
							Type: "router",
							Options: map[string]string{
								libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
								"nat-addresses":             "router",
								"exclude-lb-vips-from-garp": "true",
							},
						},
						&nbdb.LogicalSwitchPort{
							UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID",
							Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
							Type: "router",
							Options: map[string]string{
								libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
								"nat-addresses":             "router",
								"exclude-lb-vips-from-garp": "true",
							},
						},
						&nbdb.LogicalSwitch{
							UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
							Name:  types.ExternalSwitchPrefix + node1Name,
							Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
						},
						&nbdb.LogicalSwitch{
							UUID:  types.ExternalSwitchPrefix + node2Name + "-UUID",
							Name:  types.ExternalSwitchPrefix + node2Name,
							Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
						},
						node1Switch,
						node2Switch,
						getDefaultQoSRule(false, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
						egressSVCServedPodsASv4, egressIPServedPodsASv4, egressNodeIPsASv4,
					}
					gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

					for i := 0; i < 10; i++ {
						gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(0))
						node1.Labels = map[string]string{
							"k8s.ovn.org/egress-assignable": "",
						}
						_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), &node1, metav1.UpdateOptions{})
						gomega.Expect(err).NotTo(gomega.HaveOccurred())

						fakeOvn.patchEgressIPObj(node1Name, egressIPName, egressIP1)
						gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
						eips, nodes := getEgressIPStatus(egressIPName)
						gomega.Expect(nodes[0]).To(gomega.Equal(node1.Name))

						expectedDatabaseState = []libovsdbtest.TestData{
							&nbdb.LogicalRouterPolicy{
								Priority: types.DefaultNoRereoutePriority,
								Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
									egressIPServedPodsASv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
								Action:      nbdb.LogicalRouterPolicyActionAllow,
								UUID:        "default-no-reroute-node-UUID",
								Options:     map[string]string{"pkt_mark": types.EgressIPNodeConnectionMark},
								ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
							},
							getNoReRouteReplyTrafficPolicy(types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
							&nbdb.LogicalRouterPolicy{
								Priority:    types.DefaultNoRereoutePriority,
								Match:       "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
								Action:      nbdb.LogicalRouterPolicyActionAllow,
								UUID:        "no-reroute-UUID",
								ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
							},
							&nbdb.LogicalRouterPolicy{
								Priority:    types.DefaultNoRereoutePriority,
								Match:       fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
								Action:      nbdb.LogicalRouterPolicyActionAllow,
								UUID:        "no-reroute-service-UUID",
								ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
							},
							&nbdb.LogicalRouterPolicy{
								Priority:    types.EgressIPReroutePriority,
								Match:       fmt.Sprintf("ip4.src == %s", egressPod3.Status.PodIP),
								Action:      nbdb.LogicalRouterPolicyActionReroute,
								Nexthops:    []string{"100.64.0.2"},
								ExternalIDs: getEgressIPLRPReRouteDbIDs(eIP.Name, egressPod3.Namespace, egressPod3.Name, IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
								UUID:        "reroute-UUID1",
							},
							&nbdb.LogicalRouterPolicy{
								Priority:    types.EgressIPReroutePriority,
								Match:       fmt.Sprintf("ip4.src == %s", egressPod4.Status.PodIP),
								Action:      nbdb.LogicalRouterPolicyActionReroute,
								Nexthops:    []string{"100.64.0.2"},
								ExternalIDs: getEgressIPLRPReRouteDbIDs(eIP.Name, egressPod4.Namespace, egressPod4.Name, IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
								UUID:        "reroute-UUID2",
							},
							&nbdb.LogicalRouter{
								Name:     types.OVNClusterRouter,
								UUID:     types.OVNClusterRouter + "-UUID",
								Policies: []string{"no-reroute-UUID", "no-reroute-service-UUID", "default-no-reroute-node-UUID", "reroute-UUID1", "reroute-UUID2", "default-no-reroute-reply-traffic"},
							},
							&nbdb.LogicalRouter{
								Name:    types.GWRouterPrefix + node1.Name,
								UUID:    types.GWRouterPrefix + node1.Name + "-UUID",
								Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID"},
								Nat:     []string{"pod1-egressip-nat-UUID1", "pod4-egressip-nat-UUID1"},
								Options: logicalRouterOptions,
							},
							&nbdb.LogicalRouter{
								Name:    types.GWRouterPrefix + node2.Name,
								UUID:    types.GWRouterPrefix + node2.Name + "-UUID",
								Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID"},
								Options: logicalRouterOptions,
							},
							&nbdb.NAT{
								UUID:        "pod1-egressip-nat-UUID1",
								LogicalIP:   podV4IP,
								ExternalIP:  eips[0],
								ExternalIDs: getEgressIPNATDbIDs(egressIPName, egressPod3.Namespace, egressPod3.Name, IPFamilyValueV4, fakeOvn.controller.controllerName).GetExternalIDs(),
								Type:        nbdb.NATTypeSNAT,
								LogicalPort: &expectedNatLogicalPort1,
								Options: map[string]string{
									"stateless": "false",
								},
							},
							&nbdb.NAT{
								UUID:        "pod4-egressip-nat-UUID1",
								LogicalIP:   podV4IP2,
								ExternalIP:  eips[0],
								ExternalIDs: getEgressIPNATDbIDs(egressIPName, egressPod4.Namespace, egressPod4.Name, IPFamilyValueV4, fakeOvn.controller.controllerName).GetExternalIDs(),
								Type:        nbdb.NATTypeSNAT,
								LogicalPort: &expectedNatLogicalPort1,
								Options: map[string]string{
									"stateless": "false",
								},
							},
							&nbdb.LogicalSwitchPort{
								UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID",
								Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
								Type: "router",
								Options: map[string]string{
									libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
									"nat-addresses":             "router",
									"exclude-lb-vips-from-garp": "true",
								},
							},
							&nbdb.LogicalSwitchPort{
								UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID",
								Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
								Type: "router",
								Options: map[string]string{
									libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
									"nat-addresses":             "router",
									"exclude-lb-vips-from-garp": "true",
								},
							},
							&nbdb.LogicalRouterPort{
								UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID",
								Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name,
								Networks: []string{"100.64.0.3/29"},
							},
							&nbdb.LogicalRouterPort{
								UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID",
								Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name,
								Networks: []string{"100.64.0.2/29"},
							},
							&nbdb.LogicalSwitch{
								UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
								Name:  types.ExternalSwitchPrefix + node1Name,
								Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
							},
							&nbdb.LogicalSwitch{
								UUID:  types.ExternalSwitchPrefix + node2Name + "-UUID",
								Name:  types.ExternalSwitchPrefix + node2Name,
								Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
							},
							node1Switch,
							node2Switch,
							getDefaultQoSRule(false, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
							egressSVCServedPodsASv4, egressIPServedPodsASv4, egressNodeIPsASv4,
						}
						gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

						// Try out EgressIP failover scenarios for the next iteration.
						node2.Labels = map[string]string{
							"k8s.ovn.org/egress-assignable": "",
						}
						_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), &node2, metav1.UpdateOptions{})
						gomega.Expect(err).NotTo(gomega.HaveOccurred())
						fakeOvn.patchEgressIPObj(node2Name, egressIPName, egressIP1)
						gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
						_, nodes = getEgressIPStatus(egressIPName)
						gomega.Expect(nodes[0]).To(gomega.Equal(node2.Name))
						// remove label from node1 and node2.
						node1.Labels = map[string]string{}
						_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), &node1, metav1.UpdateOptions{})
						gomega.Expect(err).NotTo(gomega.HaveOccurred())
						node2.Labels = map[string]string{}
						_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), &node2, metav1.UpdateOptions{})
						gomega.Expect(err).NotTo(gomega.HaveOccurred())
						status := []egressipv1.EgressIPStatusItem{}
						err = fakeOvn.controller.eIPC.patchReplaceEgressIPStatus(egressIPName, status)
						gomega.Expect(err).NotTo(gomega.HaveOccurred())
					}
					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			},
			ginkgo.Entry("unscheduled pending pod only, local pod", true, false, false, node1Name),
			ginkgo.Entry("scheduled pending pod only, local pod", false, true, false, node1Name),
			ginkgo.Entry("host-networked pod only, local pod", false, false, true, node1Name),
			ginkgo.Entry("unscheduled pending pod only, remote pod", true, false, false, node2Name),
			ginkgo.Entry("scheduled pending pod only, remote pod", false, true, false, node2Name),
			ginkgo.Entry("host-networked pod only, remote pod", false, false, true, node2Name),
		)

		ginkgo.It("should not deadlock when two EgressIP pods are hosted and egress through opposite nodes", func() {
			// This test is designed to reproduce a deadlock issue.
			// To reproduce undo the corrspoinding fix introduced in this commit and
			// add a hard sleep of 10 seconds immediately before the code at line 1053
			// in egressip.go
			// (https://github.com/ovn-kubernetes/ovn-kubernetes/blob/fdb1f44bb04cc7f90ba06ca2782d494d5a8068fc/go-controller/pkg/ovn/egressip.go#L1053).
			// Note: Multiple test runs may be required to consistently reproduce this issue.
			app.Action = func(*cli.Context) error {
				egressIP1 := "192.168.126.101"
				egressIP2 := "192.168.126.102"
				node1IPv4OVN := "192.168.126.202/24"
				node1IPv4TranSwitchIP := "100.88.0.2/16"
				node1IPv4Addresses := []string{node1IPv4OVN}
				node2IPv4OVN := "192.168.126.203/24"
				node2IPv4TranSwitchIP := "100.88.0.3/16"
				node2IPv4Addresses := []string{node2IPv4OVN}

				egressPod := *ovntest.NewPodWithLabels(eipNamespace, podName, node1Name, podV4IP, egressPodLabel)
				egressNamespace := ovntest.NewNamespace(eipNamespace)

				egressPod2 := *ovntest.NewPodWithLabels(eipNamespace2, podName2, node2Name, podV4IP2, egressPodLabel)
				egressNamespace2 := ovntest.NewNamespace(eipNamespace2)

				nodes := getIPv4Nodes([]nodeInfo{{node1IPv4Addresses, node1IPv4TranSwitchIP}, {node2IPv4Addresses, node2IPv4TranSwitchIP}})
				gomega.Expect(nodes).To(gomega.HaveLen(2))
				node1 := nodes[0]
				node1.Labels = map[string]string{
					"k8s.ovn.org/egress-assignable": "",
				}
				node2 := nodes[1]
				node2.Labels = map[string]string{
					"k8s.ovn.org/egress-assignable": "",
				}

				// Assign EIPs on two different nodes and also ensure EIP is not on the node where its EIP pod is placed.
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
								Node:     node2.Name,
								EgressIP: egressIP1,
							},
						},
					},
				}

				eIP2 := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIP2Name),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{egressIP2},
						PodSelector: metav1.LabelSelector{
							MatchLabels: egressPodLabel,
						},
						NamespaceSelector: metav1.LabelSelector{
							MatchLabels: map[string]string{
								"name": egressNamespace2.Name,
							},
						},
					},
					Status: egressipv1.EgressIPStatus{
						Items: []egressipv1.EgressIPStatusItem{
							{
								Node:     node1.Name,
								EgressIP: egressIP2,
							},
						},
					},
				}
				node1Switch := &nbdb.LogicalSwitch{
					UUID: node1.Name + "-UUID",
					Name: node1.Name,
				}
				node2Switch := &nbdb.LogicalSwitch{
					UUID: node2.Name + "-UUID",
					Name: node2.Name,
				}
				dynamicNeighRouters := "false"

				logicalRouterOptions := map[string]string{
					"dynamic_neigh_routers": dynamicNeighRouters,
				}

				fakeOvn.startWithDBSetup(
					libovsdbtest.TestSetup{
						NBData: []libovsdbtest.TestData{
							&nbdb.LogicalRouterPort{
								UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID",
								Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name,
								Networks: []string{nodeLogicalRouterIfAddrV4},
							},
							&nbdb.LogicalRouterPort{
								UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID",
								Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name,
								Networks: []string{node2LogicalRouterIfAddrV4},
							},
							&nbdb.LogicalRouter{
								Name: types.OVNClusterRouter,
								UUID: types.OVNClusterRouter + "-UUID",
							},
							&nbdb.LogicalRouter{
								Name:    types.GWRouterPrefix + node1.Name,
								UUID:    types.GWRouterPrefix + node1.Name + "-UUID",
								Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID"},
								Options: logicalRouterOptions,
							},
							&nbdb.LogicalRouter{
								Name:    types.GWRouterPrefix + node2.Name,
								UUID:    types.GWRouterPrefix + node2.Name + "-UUID",
								Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID"},
								Options: logicalRouterOptions,
							},
							&nbdb.LogicalSwitchPort{
								UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID",
								Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
								Type: "router",
								Options: map[string]string{
									"router-port":               types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
									"nat-addresses":             "router",
									"exclude-lb-vips-from-garp": "true",
								},
							},
							&nbdb.LogicalSwitchPort{
								UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID",
								Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
								Type: "router",
								Options: map[string]string{
									"router-port":               types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
									"nat-addresses":             "router",
									"exclude-lb-vips-from-garp": "true",
								},
							},
							&nbdb.LogicalSwitch{
								UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
								Name:  types.ExternalSwitchPrefix + node1Name,
								Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
							},
							&nbdb.LogicalSwitch{
								UUID:  types.ExternalSwitchPrefix + node2Name + "-UUID",
								Name:  types.ExternalSwitchPrefix + node2Name,
								Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
							},
							node1Switch,
							node2Switch,
						},
					},
					&egressipv1.EgressIPList{
						Items: []egressipv1.EgressIP{eIP, eIP2},
					},
					&corev1.NodeList{
						Items: []corev1.Node{node1, node2},
					},
					&corev1.NamespaceList{
						Items: []corev1.Namespace{*egressNamespace, *egressNamespace2},
					})

				err := fakeOvn.controller.WatchEgressIPPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIPNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressNodes()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Run the test for 10 times.
				for range 10 {
					// Create both pods in parallel to trigger pod add events for both EIPs and check it's handled gracefully.
					var wg sync.WaitGroup
					wg.Add(2)
					go func() {
						defer ginkgo.GinkgoRecover()
						defer wg.Done()
						i, n, _ := net.ParseCIDR(podV4IP + "/23")
						n.IP = i
						fakeOvn.controller.logicalPortCache.add(&egressPod, "", types.DefaultNetworkName, "", nil, []*net.IPNet{n})
						_, err := fakeOvn.fakeClient.KubeClient.CoreV1().Pods(egressPod.Namespace).Create(context.TODO(), &egressPod, metav1.CreateOptions{})
						gomega.Expect(err).NotTo(gomega.HaveOccurred())
					}()
					go func() {
						defer ginkgo.GinkgoRecover()
						defer wg.Done()
						i, n, _ := net.ParseCIDR(podV4IP2 + "/23")
						n.IP = i
						fakeOvn.controller.logicalPortCache.add(&egressPod2, "", types.DefaultNetworkName, "", nil, []*net.IPNet{n})
						_, err := fakeOvn.fakeClient.KubeClient.CoreV1().Pods(egressPod2.Namespace).Create(context.TODO(), &egressPod2, metav1.CreateOptions{})
						gomega.Expect(err).NotTo(gomega.HaveOccurred())
					}()
					wg.Wait()

					expectedNode1NatLogicalPort := "k8s-node1"
					egressSVCServedPodsASv4, _ := buildEgressServiceAddressSets(nil)
					egressIPServedPodsASv4, _ := buildEgressIPServedPodsAddressSets([]string{podV4IP}, types.DefaultNetworkName,
						fakeOvn.controller.eIPC.controllerName)
					ipNets, _ := util.ParseIPNets(node1IPv4Addresses)
					egressNodeIPs := []string{}
					for _, ipNet := range ipNets {
						egressNodeIPs = append(egressNodeIPs, ipNet.IP.String())
					}
					ipNets, _ = util.ParseIPNets(node2IPv4Addresses)
					for _, ipNet := range ipNets {
						egressNodeIPs = append(egressNodeIPs, ipNet.IP.String())
					}

					egressNodeIPsASv4, _ := buildEgressIPNodeAddressSets(egressNodeIPs)
					node1Switch.QOSRules = []string{"default-QoS-UUID"}
					expectedDatabaseState := []libovsdbtest.TestData{
						&nbdb.LogicalRouterPolicy{
							Priority:    types.DefaultNoRereoutePriority,
							Match:       "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
							Action:      nbdb.LogicalRouterPolicyActionAllow,
							UUID:        "default-no-reroute-UUID",
							ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
						},
						getNoReRouteReplyTrafficPolicy(types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
						&nbdb.LogicalRouterPolicy{
							Priority:    types.DefaultNoRereoutePriority,
							Match:       fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
							Action:      nbdb.LogicalRouterPolicyActionAllow,
							UUID:        "no-reroute-service-UUID",
							ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
						},
						&nbdb.LogicalRouterPolicy{
							Priority:    types.EgressIPReroutePriority,
							Match:       fmt.Sprintf("ip4.src == %s", egressPod.Status.PodIP),
							Action:      nbdb.LogicalRouterPolicyActionReroute,
							Nexthops:    []string{"100.88.0.3"},
							ExternalIDs: getEgressIPLRPReRouteDbIDs(eIP.Name, egressPod.Namespace, egressPod.Name, IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
							UUID:        "reroute-UUID",
						},
						&nbdb.LogicalRouterPolicy{
							Priority:    types.DefaultNoRereoutePriority,
							Match:       "(ip4.src == $a8519615025667110816 || ip4.src == $a13607449821398607916) && ip4.dst == $a1042611113178530741",
							Action:      nbdb.LogicalRouterPolicyActionAllow,
							Options:     map[string]string{"pkt_mark": types.EgressIPNodeConnectionMark},
							UUID:        "no-reroute-node-UUID",
							ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
						},
						&nbdb.NAT{
							UUID:        "egressip2-nat-UUID",
							LogicalIP:   podV4IP2,
							ExternalIP:  egressIP2,
							ExternalIDs: getEgressIPNATDbIDs(egressIP2Name, egressPod2.Namespace, egressPod2.Name, IPFamilyValueV4, fakeOvn.controller.controllerName).GetExternalIDs(),
							Type:        nbdb.NATTypeSNAT,
							LogicalPort: &expectedNode1NatLogicalPort,
							Options: map[string]string{
								"stateless": "false",
							},
						},
						&nbdb.LogicalRouter{
							Name:    types.GWRouterPrefix + node1.Name,
							UUID:    types.GWRouterPrefix + node1.Name + "-UUID",
							Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID"},
							Nat:     []string{"egressip2-nat-UUID"},
							Options: logicalRouterOptions,
						},
						&nbdb.LogicalRouter{
							Name:    types.GWRouterPrefix + node2.Name,
							UUID:    types.GWRouterPrefix + node2.Name + "-UUID",
							Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID"},
							Options: logicalRouterOptions,
						},
						&nbdb.LogicalRouter{
							Name: types.OVNClusterRouter,
							UUID: types.OVNClusterRouter + "-UUID",
							Policies: []string{"reroute-UUID", "default-no-reroute-UUID", "no-reroute-service-UUID",
								"no-reroute-node-UUID", "default-no-reroute-reply-traffic"},
						},
						&nbdb.LogicalRouterPort{
							UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID",
							Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name,
							Networks: []string{nodeLogicalRouterIfAddrV4},
						},
						&nbdb.LogicalRouterPort{
							UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID",
							Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name,
							Networks: []string{node2LogicalRouterIfAddrV4},
						},
						&nbdb.LogicalSwitchPort{
							UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID",
							Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
							Type: "router",
							Options: map[string]string{
								"router-port":               types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
								"nat-addresses":             "router",
								"exclude-lb-vips-from-garp": "true",
							},
						},
						&nbdb.LogicalSwitchPort{
							UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID",
							Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
							Type: "router",
							Options: map[string]string{
								"router-port":               types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
								"nat-addresses":             "router",
								"exclude-lb-vips-from-garp": "true",
							},
						},
						&nbdb.LogicalSwitch{
							UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
							Name:  types.ExternalSwitchPrefix + node1Name,
							Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
						},
						&nbdb.LogicalSwitch{
							UUID:  types.ExternalSwitchPrefix + node2Name + "-UUID",
							Name:  types.ExternalSwitchPrefix + node2Name,
							Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
						},
						node1Switch,
						node2Switch,
						getDefaultQoSRule(false, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
						egressSVCServedPodsASv4,
						egressIPServedPodsASv4,
						egressNodeIPsASv4,
					}
					gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

					// Delete both pods in parallel to trigger pod delete events for both EIPs and check it's handled gracefully.
					var wg2 sync.WaitGroup
					wg2.Add(2)
					go func() {
						defer ginkgo.GinkgoRecover()
						defer wg2.Done()
						err := fakeOvn.fakeClient.KubeClient.CoreV1().Pods(egressPod.Namespace).Delete(context.TODO(), egressPod.Name, metav1.DeleteOptions{})
						gomega.Expect(err).NotTo(gomega.HaveOccurred())
					}()
					go func() {
						defer ginkgo.GinkgoRecover()
						defer wg2.Done()
						err := fakeOvn.fakeClient.KubeClient.CoreV1().Pods(egressPod2.Namespace).Delete(context.TODO(), egressPod2.Name, metav1.DeleteOptions{})
						gomega.Expect(err).NotTo(gomega.HaveOccurred())
					}()
					wg2.Wait()

					egressIPServedPodsASv4.Addresses = nil
					expectedDatabaseState = []libovsdbtest.TestData{
						&nbdb.LogicalRouterPolicy{
							Priority:    types.DefaultNoRereoutePriority,
							Match:       "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
							Action:      nbdb.LogicalRouterPolicyActionAllow,
							UUID:        "default-no-reroute-UUID",
							ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
						},
						getNoReRouteReplyTrafficPolicy(types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
						&nbdb.LogicalRouterPolicy{
							Priority:    types.DefaultNoRereoutePriority,
							Match:       fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
							Action:      nbdb.LogicalRouterPolicyActionAllow,
							UUID:        "no-reroute-service-UUID",
							ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
						},
						&nbdb.LogicalRouterPolicy{
							Priority:    types.DefaultNoRereoutePriority,
							Match:       "(ip4.src == $a8519615025667110816 || ip4.src == $a13607449821398607916) && ip4.dst == $a1042611113178530741",
							Action:      nbdb.LogicalRouterPolicyActionAllow,
							Options:     map[string]string{"pkt_mark": types.EgressIPNodeConnectionMark},
							UUID:        "no-reroute-node-UUID",
							ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
						},
						&nbdb.LogicalRouter{
							Name:    types.GWRouterPrefix + node1.Name,
							UUID:    types.GWRouterPrefix + node1.Name + "-UUID",
							Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID"},
							Options: logicalRouterOptions,
						},
						&nbdb.LogicalRouter{
							Name:    types.GWRouterPrefix + node2.Name,
							UUID:    types.GWRouterPrefix + node2.Name + "-UUID",
							Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID"},
							Options: logicalRouterOptions,
						},
						&nbdb.LogicalRouter{
							Name: types.OVNClusterRouter,
							UUID: types.OVNClusterRouter + "-UUID",
							Policies: []string{"default-no-reroute-UUID", "no-reroute-service-UUID",
								"no-reroute-node-UUID", "default-no-reroute-reply-traffic"},
						},
						&nbdb.LogicalRouterPort{
							UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID",
							Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name,
							Networks: []string{nodeLogicalRouterIfAddrV4},
						},
						&nbdb.LogicalRouterPort{
							UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID",
							Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name,
							Networks: []string{node2LogicalRouterIfAddrV4},
						},
						&nbdb.LogicalSwitchPort{
							UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID",
							Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
							Type: "router",
							Options: map[string]string{
								"router-port":               types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
								"nat-addresses":             "router",
								"exclude-lb-vips-from-garp": "true",
							},
						},
						&nbdb.LogicalSwitchPort{
							UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID",
							Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
							Type: "router",
							Options: map[string]string{
								"router-port":               types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
								"nat-addresses":             "router",
								"exclude-lb-vips-from-garp": "true",
							},
						},
						&nbdb.LogicalSwitch{
							UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
							Name:  types.ExternalSwitchPrefix + node1Name,
							Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
						},
						&nbdb.LogicalSwitch{
							UUID:  types.ExternalSwitchPrefix + node2Name + "-UUID",
							Name:  types.ExternalSwitchPrefix + node2Name,
							Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
						},
						node1Switch,
						node2Switch,
						getDefaultQoSRule(false, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
						egressSVCServedPodsASv4,
						egressIPServedPodsASv4,
						egressNodeIPsASv4,
					}
					gomega.Eventually(fakeOvn.nbClient).WithTimeout(30 * time.Second).Should(libovsdbtest.HaveData(expectedDatabaseState))
				}

				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should ensure SNATs towards egressIP and nodeIP are correctly configured during egressIP re-assignment", func() {
			app.Action = func(*cli.Context) error {
				config.Gateway.DisableSNATMultipleGWs = true

				egressIP1 := "192.168.126.101"
				egressIP2 := "192.168.126.102"
				node1IPv4 := "192.168.126.12"
				node1IPv4CIDR := node1IPv4 + "/24"
				node2IPv4 := "192.168.126.51"
				node2IPv4CIDR := node2IPv4 + "/24"

				egressPod1 := *ovntest.NewPodWithLabels(eipNamespace, podName, node1Name, podV4IP, egressPodLabel)
				egressPod2 := *ovntest.NewPodWithLabels(eipNamespace, "egress-pod2", node2Name, "10.128.0.16", egressPodLabel)
				egressNamespace := ovntest.NewNamespace(eipNamespace)
				annotations := map[string]string{
					"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\"}", node1IPv4CIDR),
					"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":\"%s\"}", v4Node1Subnet),
					"k8s.ovn.org/l3-gateway-config":   `{"default":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"192.168.126.12/24", "next-hop":"192.168.126.1"}}`,
					"k8s.ovn.org/node-chassis-id":     "79fdcfc4-6fe6-4cd3-8242-c0f85a4668ec",
					util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node1IPv4CIDR),
				}
				node1 := getNodeObj(node1Name, annotations, map[string]string{})
				annotations = map[string]string{
					"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\"}", node2IPv4CIDR),
					"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":\"%s\"}", v4Node2Subnet),
					"k8s.ovn.org/l3-gateway-config":   `{"default":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"192.168.126.51/24", "next-hop":"192.168.126.1"}}`,
					"k8s.ovn.org/node-chassis-id":     "89fdcfc4-6fe6-4cd3-8242-c0f85a4668ec",
					util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node2IPv4CIDR),
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
				node1Switch := &nbdb.LogicalSwitch{
					UUID: node1.Name + "-UUID",
					Name: node1.Name,
				}
				node2Switch := &nbdb.LogicalSwitch{
					UUID: node2.Name + "-UUID",
					Name: node2.Name,
				}
				fakeOvn.startWithDBSetup(
					libovsdbtest.TestSetup{
						NBData: []libovsdbtest.TestData{
							&nbdb.LogicalRouter{
								Name: types.OVNClusterRouter,
								UUID: types.OVNClusterRouter + "-UUID",
							},
							&nbdb.LogicalRouter{
								Name:  types.GWRouterPrefix + node1.Name,
								UUID:  types.GWRouterPrefix + node1.Name + "-UUID",
								Ports: []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID"},
							},
							&nbdb.LogicalRouter{
								Name:  types.GWRouterPrefix + node2.Name,
								UUID:  types.GWRouterPrefix + node2.Name + "-UUID",
								Ports: []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID"},
							},
							&nbdb.LogicalRouterPort{
								UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID",
								Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name,
								Networks: []string{nodeLogicalRouterIfAddrV4},
							},
							&nbdb.LogicalRouterPort{
								UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID",
								Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name,
								Networks: []string{node2LogicalRouterIfAddrV4},
							},
							&nbdb.LogicalSwitchPort{
								UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID",
								Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
								Type: "router",
								Options: map[string]string{
									libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
									"nat-addresses":             "router",
									"exclude-lb-vips-from-garp": "true",
								},
							},
							&nbdb.LogicalSwitchPort{
								UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID",
								Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
								Type: "router",
								Options: map[string]string{
									libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
									"nat-addresses":             "router",
									"exclude-lb-vips-from-garp": "true",
								},
							},
							&nbdb.LogicalSwitch{
								UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
								Name:  types.ExternalSwitchPrefix + node1Name,
								Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
							},
							&nbdb.LogicalSwitch{
								UUID:  types.ExternalSwitchPrefix + node2Name + "-UUID",
								Name:  types.ExternalSwitchPrefix + node2Name,
								Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
							},
							node1Switch,
							node2Switch,
						},
					},
					&egressipv1.EgressIPList{
						Items: []egressipv1.EgressIP{eIP},
					},
					&corev1.NodeList{
						Items: []corev1.Node{node1, node2},
					},
					&corev1.NamespaceList{
						Items: []corev1.Namespace{*egressNamespace},
					},
					&corev1.PodList{
						Items: []corev1.Pod{egressPod1, egressPod2},
					},
				)

				i, n, _ := net.ParseCIDR(podV4IP + "/23")
				n.IP = i
				fakeOvn.controller.logicalPortCache.add(&egressPod1, "", types.DefaultNetworkName, "", nil, []*net.IPNet{n})
				i, n, _ = net.ParseCIDR("10.128.0.16" + "/23")
				n.IP = i
				fakeOvn.controller.logicalPortCache.add(&egressPod2, "", types.DefaultNetworkName, "", nil, []*net.IPNet{n})

				err := fakeOvn.controller.WatchEgressIPPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIPNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressNodes()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				egressSVCServedPodsASv4, _ := buildEgressServiceAddressSets(nil)
				egressIPServedPodsASv4, _ := buildEgressIPServedPodsAddressSets([]string{podV4IP}, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName)
				egressNodeIPsASv4, _ := buildEgressIPNodeAddressSets([]string{node1IPv4, node2IPv4})

				node1Switch.QOSRules = []string{"default-QoS-UUID"}
				expectedDatabaseState := []libovsdbtest.TestData{
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
							egressIPServedPodsASv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": types.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					getNoReRouteReplyTrafficPolicy(types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
					&nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-service-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouter{
						Name:     types.OVNClusterRouter,
						UUID:     types.OVNClusterRouter + "-UUID",
						Policies: []string{"no-reroute-UUID", "no-reroute-service-UUID", "default-no-reroute-node-UUID", "default-no-reroute-reply-traffic"},
					},
					&nbdb.LogicalRouter{
						Name:  types.GWRouterPrefix + node1.Name,
						UUID:  types.GWRouterPrefix + node1.Name + "-UUID",
						Ports: []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID"},
					},
					&nbdb.LogicalRouter{
						Name:  types.GWRouterPrefix + node2.Name,
						UUID:  types.GWRouterPrefix + node2.Name + "-UUID",
						Ports: []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID"},
					},
					&nbdb.LogicalRouterPort{
						UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID",
						Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name,
						Networks: []string{node2LogicalRouterIfAddrV4},
					},
					&nbdb.LogicalRouterPort{
						UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID",
						Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name,
						Networks: []string{nodeLogicalRouterIfAddrV4},
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
						Type: "router",
						Options: map[string]string{
							libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
						Type: "router",
						Options: map[string]string{
							libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalSwitch{
						UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
						Name:  types.ExternalSwitchPrefix + node1Name,
						Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
					},
					&nbdb.LogicalSwitch{
						UUID:  types.ExternalSwitchPrefix + node2Name + "-UUID",
						Name:  types.ExternalSwitchPrefix + node2Name,
						Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
					},
					node1Switch,
					node2Switch,
					getDefaultQoSRule(false, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
					egressSVCServedPodsASv4, egressIPServedPodsASv4, egressNodeIPsASv4,
				}
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(0))
				node1.Labels = map[string]string{
					"k8s.ovn.org/egress-assignable": "",
				}

				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), &node1, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				fakeOvn.patchEgressIPObj(node1Name, egressIPName, egressIP1)
				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
				gomega.Eventually(getEgressIPReassignmentCount).Should(gomega.Equal(1))
				eips, nodes := getEgressIPStatus(egressIPName)
				gomega.Expect(nodes[0]).To(gomega.Equal(node1.Name))

				expectedNatLogicalPort1 := "k8s-node1"
				expectedDatabaseState = []libovsdbtest.TestData{
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
							egressIPServedPodsASv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": types.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					getNoReRouteReplyTrafficPolicy(types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
					&nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-service-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    types.EgressIPReroutePriority,
						Match:       fmt.Sprintf("ip4.src == %s", egressPod1.Status.PodIP),
						Action:      nbdb.LogicalRouterPolicyActionReroute,
						Nexthops:    []string{"100.64.0.2"},
						ExternalIDs: getEgressIPLRPReRouteDbIDs(eIP.Name, egressPod1.Namespace, egressPod1.Name, IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
						UUID:        "reroute-UUID1",
					},
					&nbdb.LogicalRouter{
						Name: types.OVNClusterRouter,
						UUID: types.OVNClusterRouter + "-UUID",
						Policies: []string{"no-reroute-UUID", "no-reroute-service-UUID", "default-no-reroute-node-UUID",
							"reroute-UUID1", "default-no-reroute-reply-traffic"},
					},
					&nbdb.LogicalRouter{
						Name:  types.GWRouterPrefix + node1.Name,
						UUID:  types.GWRouterPrefix + node1.Name + "-UUID",
						Ports: []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID"},
						Nat:   []string{"egressip-nat-UUID1", "egressip-nat-UUID2"},
					},
					&nbdb.LogicalRouter{
						Name:  types.GWRouterPrefix + node2.Name,
						UUID:  types.GWRouterPrefix + node2.Name + "-UUID",
						Ports: []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID"},
					},
					&nbdb.NAT{
						UUID:        "egressip-nat-UUID1",
						LogicalIP:   podV4IP,
						ExternalIP:  eips[0],
						ExternalIDs: getEgressIPNATDbIDs(egressIPName, egressPod1.Namespace, egressPod1.Name, IPFamilyValueV4, fakeOvn.controller.controllerName).GetExternalIDs(),
						Type:        nbdb.NATTypeSNAT,
						LogicalPort: &expectedNatLogicalPort1,
						Options: map[string]string{
							"stateless": "false",
						},
					},
					&nbdb.NAT{
						UUID:        "egressip-nat-UUID2",
						LogicalIP:   "10.128.0.16",
						ExternalIP:  eips[0],
						ExternalIDs: getEgressIPNATDbIDs(egressIPName, egressPod2.Namespace, egressPod2.Name, IPFamilyValueV4, fakeOvn.controller.controllerName).GetExternalIDs(),
						Type:        nbdb.NATTypeSNAT,
						LogicalPort: &expectedNatLogicalPort1,
						Options: map[string]string{
							"stateless": "false",
						},
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
						Type: "router",
						Options: map[string]string{
							libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
						Type: "router",
						Options: map[string]string{
							libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalRouterPort{
						UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID",
						Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name,
						Networks: []string{"100.64.0.3/29"},
					},
					&nbdb.LogicalRouterPort{
						UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID",
						Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name,
						Networks: []string{"100.64.0.2/29"},
					},
					&nbdb.LogicalSwitch{
						UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
						Name:  types.ExternalSwitchPrefix + node1Name,
						Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
					},
					&nbdb.LogicalSwitch{
						UUID:  types.ExternalSwitchPrefix + node2Name + "-UUID",
						Name:  types.ExternalSwitchPrefix + node2Name,
						Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
					},
					node1Switch,
					node2Switch,
					getDefaultQoSRule(false, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
					egressSVCServedPodsASv4, egressIPServedPodsASv4, egressNodeIPsASv4,
				}
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				node2.Labels = map[string]string{
					"k8s.ovn.org/egress-assignable": "",
				}

				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), &node2, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// NOTE: Cluster manager is the one who patches the egressIP object.
				// For the sake of unit testing the EgressIP controller we need to patch the EgressIP object manually.
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
				err = fakeOvn.controller.eIPC.patchReplaceEgressIPStatus(egressIPName, status)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(2))
				gomega.Eventually(getEgressIPReassignmentCount).Should(gomega.Equal(0))

				eips, nodes = getEgressIPStatus(egressIPName)
				gomega.Expect(nodes[0]).To(gomega.Equal(node1.Name))
				gomega.Expect(nodes[1]).To(gomega.Equal(node2.Name))

				expectedDatabaseState = []libovsdbtest.TestData{
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
							egressIPServedPodsASv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": types.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					getNoReRouteReplyTrafficPolicy(types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
					&nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-service-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    types.EgressIPReroutePriority,
						Match:       fmt.Sprintf("ip4.src == %s", egressPod1.Status.PodIP),
						Action:      nbdb.LogicalRouterPolicyActionReroute,
						Nexthops:    []string{"100.64.0.2"},
						ExternalIDs: getEgressIPLRPReRouteDbIDs(eIP.Name, egressPod1.Namespace, egressPod1.Name, IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
						UUID:        "reroute-UUID1",
					},
					&nbdb.NAT{
						UUID:        "egressip-nat-UUID1",
						LogicalIP:   podV4IP,
						ExternalIP:  eips[0],
						ExternalIDs: getEgressIPNATDbIDs(egressIPName, egressPod1.Namespace, egressPod1.Name, IPFamilyValueV4, fakeOvn.controller.controllerName).GetExternalIDs(),
						Type:        nbdb.NATTypeSNAT,
						LogicalPort: &expectedNatLogicalPort1,
						Options: map[string]string{
							"stateless": "false",
						},
					},
					&nbdb.NAT{
						UUID:        "egressip-nat-UUID2",
						LogicalIP:   "10.128.0.16",
						ExternalIP:  eips[0],
						ExternalIDs: getEgressIPNATDbIDs(egressIPName, egressPod2.Namespace, egressPod2.Name, IPFamilyValueV4, fakeOvn.controller.controllerName).GetExternalIDs(),
						Type:        nbdb.NATTypeSNAT,
						LogicalPort: &expectedNatLogicalPort1,
						Options: map[string]string{
							"stateless": "false",
						},
					},
					&nbdb.LogicalRouter{
						Name: types.OVNClusterRouter,
						UUID: types.OVNClusterRouter + "-UUID",
						Policies: []string{"no-reroute-UUID", "no-reroute-service-UUID", "default-no-reroute-node-UUID", "reroute-UUID1",
							"default-no-reroute-reply-traffic"},
					},
					&nbdb.LogicalRouter{
						Name:  types.GWRouterPrefix + node1.Name,
						UUID:  types.GWRouterPrefix + node1.Name + "-UUID",
						Ports: []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID"},
						Nat:   []string{"egressip-nat-UUID1", "egressip-nat-UUID2"},
					},
					&nbdb.LogicalRouter{
						Name:  types.GWRouterPrefix + node2.Name,
						UUID:  types.GWRouterPrefix + node2.Name + "-UUID",
						Ports: []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID"},
					},
					&nbdb.LogicalRouterPort{
						UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID",
						Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name,
						Networks: []string{"100.64.0.3/29"},
					},
					&nbdb.LogicalRouterPort{
						UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID",
						Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name,
						Networks: []string{"100.64.0.2/29"},
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
						Type: "router",
						Options: map[string]string{
							libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
						Type: "router",
						Options: map[string]string{
							libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalSwitch{
						UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
						Name:  types.ExternalSwitchPrefix + node1Name,
						Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
					},
					&nbdb.LogicalSwitch{
						UUID:  types.ExternalSwitchPrefix + node2Name + "-UUID",
						Name:  types.ExternalSwitchPrefix + node2Name,
						Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
					},
					node1Switch,
					node2Switch,
					getDefaultQoSRule(false, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
					egressSVCServedPodsASv4, egressIPServedPodsASv4, egressNodeIPsASv4,
				}
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				// remove label from node2
				node2.Labels = map[string]string{}

				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), &node2, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				fakeOvn.patchEgressIPObj(node1Name, egressIPName, egressIP1)

				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(1))
				gomega.Eventually(getEgressIPReassignmentCount).Should(gomega.Equal(1))

				expectedDatabaseState = []libovsdbtest.TestData{
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
							egressIPServedPodsASv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": types.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					getNoReRouteReplyTrafficPolicy(types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
					&nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-service-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    types.EgressIPReroutePriority,
						Match:       fmt.Sprintf("ip4.src == %s", egressPod1.Status.PodIP),
						Action:      nbdb.LogicalRouterPolicyActionReroute,
						Nexthops:    nodeLogicalRouterIPv4,
						ExternalIDs: getEgressIPLRPReRouteDbIDs(eIP.Name, egressPod1.Namespace, egressPod1.Name, IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
						UUID:        "reroute-UUID1",
					},
					&nbdb.NAT{
						UUID:        "egressip-nat-UUID1",
						LogicalIP:   podV4IP,
						ExternalIP:  eips[0],
						ExternalIDs: getEgressIPNATDbIDs(egressIPName, egressPod1.Namespace, egressPod1.Name, IPFamilyValueV4, fakeOvn.controller.controllerName).GetExternalIDs(),
						Type:        nbdb.NATTypeSNAT,
						LogicalPort: &expectedNatLogicalPort1,
						Options: map[string]string{
							"stateless": "false",
						},
					},
					&nbdb.NAT{
						UUID:        "egressip-nat-UUID2",
						LogicalIP:   "10.128.0.16",
						ExternalIP:  eips[0],
						ExternalIDs: getEgressIPNATDbIDs(egressIPName, egressPod2.Namespace, egressPod2.Name, IPFamilyValueV4, fakeOvn.controller.controllerName).GetExternalIDs(),
						Type:        nbdb.NATTypeSNAT,
						LogicalPort: &expectedNatLogicalPort1,
						Options: map[string]string{
							"stateless": "false",
						},
					},
					&nbdb.LogicalRouter{
						Name: types.OVNClusterRouter,
						UUID: types.OVNClusterRouter + "-UUID",
						Policies: []string{"no-reroute-UUID", "no-reroute-service-UUID", "default-no-reroute-node-UUID", "reroute-UUID1",
							"default-no-reroute-reply-traffic"},
					},
					&nbdb.LogicalRouter{
						Name:  types.GWRouterPrefix + node1.Name,
						UUID:  types.GWRouterPrefix + node1.Name + "-UUID",
						Ports: []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID"},
						Nat:   []string{"egressip-nat-UUID1", "egressip-nat-UUID2"},
					},
					&nbdb.LogicalRouter{
						Name:  types.GWRouterPrefix + node2.Name,
						UUID:  types.GWRouterPrefix + node2.Name + "-UUID",
						Ports: []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID"},
					},
					&nbdb.LogicalRouterPort{
						UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID",
						Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name,
						Networks: []string{node2LogicalRouterIfAddrV4},
					},
					&nbdb.LogicalRouterPort{
						UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID",
						Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name,
						Networks: []string{nodeLogicalRouterIfAddrV4},
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
						Type: "router",
						Options: map[string]string{
							libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
						Type: "router",
						Options: map[string]string{
							libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalSwitch{
						UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
						Name:  types.ExternalSwitchPrefix + node1Name,
						Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
					},
					&nbdb.LogicalSwitch{
						UUID:  types.ExternalSwitchPrefix + node2Name + "-UUID",
						Name:  types.ExternalSwitchPrefix + node2Name,
						Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
					},
					node1Switch,
					node2Switch,
					getDefaultQoSRule(false, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
					egressSVCServedPodsASv4, egressIPServedPodsASv4, egressNodeIPsASv4,
				}
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				// remove label from node1
				node1.Labels = map[string]string{}

				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), &node1, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// NOTE: Cluster manager is the one who patches the egressIP object.
				// For the sake of unit testing the EgressIP controller we need to patch the EgressIP object manually.
				// There are tests in cluster-manager package covering the patch logic.
				status = []egressipv1.EgressIPStatusItem{}
				err = fakeOvn.controller.eIPC.patchReplaceEgressIPStatus(egressIPName, status)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(0))
				gomega.Eventually(getEgressIPReassignmentCount).Should(gomega.Equal(1)) // though 2 egressIPs to be re-assigned its only 1 egressIP object

				egressIPServedPodsASv4, _ = buildEgressIPServedPodsAddressSets(nil, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName)
				expectedDatabaseState = []libovsdbtest.TestData{
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
							egressIPServedPodsASv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": types.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-service-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					getNoReRouteReplyTrafficPolicy(types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
					&nbdb.NAT{
						UUID:       "egressip-nat-UUID1",
						LogicalIP:  podV4IP,
						ExternalIP: "192.168.126.12", // adds back SNAT towards nodeIP
						Type:       nbdb.NATTypeSNAT,
						Options: map[string]string{
							"stateless": "false",
						},
					},
					&nbdb.LogicalRouter{
						Name: types.OVNClusterRouter,
						UUID: types.OVNClusterRouter + "-UUID",
						Policies: []string{"no-reroute-UUID", "no-reroute-service-UUID", "default-no-reroute-node-UUID",
							"default-no-reroute-reply-traffic"},
					},
					&nbdb.LogicalRouter{
						Name:  types.GWRouterPrefix + node1.Name,
						UUID:  types.GWRouterPrefix + node1.Name + "-UUID",
						Ports: []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID"},
						Nat:   []string{"egressip-nat-UUID1"},
					},
					&nbdb.LogicalRouter{
						Name:  types.GWRouterPrefix + node2.Name,
						UUID:  types.GWRouterPrefix + node2.Name + "-UUID",
						Ports: []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID"},
					},
					&nbdb.LogicalRouterPort{
						UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID",
						Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name,
						Networks: []string{node2LogicalRouterIfAddrV4},
					},
					&nbdb.LogicalRouterPort{
						UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID",
						Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name,
						Networks: []string{nodeLogicalRouterIfAddrV4},
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
						Type: "router",
						Options: map[string]string{
							libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
						Type: "router",
						Options: map[string]string{
							libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalSwitch{
						UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
						Name:  types.ExternalSwitchPrefix + node1Name,
						Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
					},
					&nbdb.LogicalSwitch{
						UUID:  types.ExternalSwitchPrefix + node2Name + "-UUID",
						Name:  types.ExternalSwitchPrefix + node2Name,
						Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
					},
					node1Switch,
					node2Switch,
					getDefaultQoSRule(false, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
					egressSVCServedPodsASv4, egressIPServedPodsASv4, egressNodeIPsASv4,
				}
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("creates pod to pod and pod to join LRPs for multiple default network subnets of the same IP family", func() {
			// test creates two IPv4 cluster subnets and two IPv6 cluster subnets and ensures the appropriate pod to pod and
			// pod to join LRPs are created.
			app.Action = func(*cli.Context) error {
				config.IPv4Mode = true
				config.IPv6Mode = true
				clusterSubnetIPv41 := ovntest.MustParseIPNet("10.128.0.0/14")
				clusterSubnetIPv42 := ovntest.MustParseIPNet("10.132.0.0/14")
				clusterSubnetIPv61 := ovntest.MustParseIPNet("fd69::1/64")
				clusterSubnetIPv62 := ovntest.MustParseIPNet("fd70::1/64")
				config.Default.ClusterSubnets = []config.CIDRNetworkEntry{
					{
						CIDR:             clusterSubnetIPv41,
						HostSubnetLength: 23,
					},
					{
						CIDR:             clusterSubnetIPv42,
						HostSubnetLength: 23,
					},
					{
						CIDR:             clusterSubnetIPv61,
						HostSubnetLength: 120,
					},
					{
						CIDR:             clusterSubnetIPv62,
						HostSubnetLength: 120,
					},
				}
				node1IPv4 := "192.168.126.12"
				node1IPv4CIDR := node1IPv4 + "/24"
				node1IPv6 := "::feff:c0a8:8e0c"
				node1IPv6CIDR := node1IPv6 + "/64"
				egressNamespace := ovntest.NewNamespace(eipNamespace)
				annotations := map[string]string{
					"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4CIDR, node1IPv6CIDR),
					"k8s.ovn.org/node-subnets":        "{\"default\":[\"10.128.0.0/23\", \"10.132.0.0/23\", \"fd69::1/120\"]}",
					util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\",\"%s\"]", node1IPv4CIDR, node1IPv6CIDR),
				}
				node1 := getNodeObj(node1Name, annotations, nil)

				node1Switch := &nbdb.LogicalSwitch{
					UUID: node1.Name + "-UUID",
					Name: node1.Name,
				}

				fakeOvn.startWithDBSetup(
					libovsdbtest.TestSetup{
						NBData: []libovsdbtest.TestData{
							&nbdb.LogicalRouterPort{
								UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID",
								Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name,
								Networks: []string{nodeLogicalRouterIfAddrV4, nodeLogicalRouterIfAddrV6},
							},
							&nbdb.LogicalRouter{
								Name: types.OVNClusterRouter,
								UUID: types.OVNClusterRouter + "-UUID",
							},
							&nbdb.LogicalRouter{
								Name:  types.GWRouterPrefix + node1.Name,
								UUID:  types.GWRouterPrefix + node1.Name + "-UUID",
								Ports: []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID"},
							},
							&nbdb.LogicalSwitchPort{
								UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID",
								Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
								Type: "router",
								Options: map[string]string{
									"nat-addresses":             "router",
									"exclude-lb-vips-from-garp": "true",
									libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
								},
							},
							&nbdb.LogicalSwitch{
								UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
								Name:  types.ExternalSwitchPrefix + node1Name,
								Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
							},
							node1Switch,
						},
					},
					&corev1.NodeList{
						Items: []corev1.Node{node1},
					},
					&corev1.NamespaceList{
						Items: []corev1.Namespace{*egressNamespace},
					},
				)
				err := fakeOvn.controller.WatchEgressIPPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIPNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressNodes()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				egressSVCServedPodsASv4, egressSVCServedPodsASv6 := buildEgressServiceAddressSets(nil)
				egressIPServedPodsASv4, egressIPServedPodsASv6 := buildEgressIPServedPodsAddressSets([]string{}, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName)
				egressNodeIPsASv4, egressNodeIPsASv6 := buildEgressIPNodeAddressSets([]string{node1IPv4, node1IPv6})

				node1Switch.QOSRules = []string{"default-QoS-UUID", "default-QoSv6-UUID"}
				expectedDatabaseState := []libovsdbtest.TestData{
					getNoReRouteReplyTrafficPolicy(types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
							egressIPServedPodsASv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-v4-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": types.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip6.src == $%s || ip6.src == $%s) && ip6.dst == $%s",
							egressIPServedPodsASv6.Name, egressSVCServedPodsASv6.Name, egressNodeIPsASv6.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-v6-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": types.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV6, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},

					&nbdb.LogicalRouterPort{
						UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID",
						Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name,
						Networks: []string{nodeLogicalRouterIfAddrV4, nodeLogicalRouterIfAddrV6},
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-ipv4-1-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       "ip4.src == 10.128.0.0/14 && ip4.dst == 10.132.0.0/14",
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-ipv4-2-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       "ip4.src == 10.132.0.0/14 && ip4.dst == 10.128.0.0/14",
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-ipv4-3-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       "ip4.src == 10.132.0.0/14 && ip4.dst == 10.132.0.0/14",
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-ipv4-4-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       "ip6.src == fd69::1/64 && ip6.dst == fd69::1/64",
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-ipv6-1-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV6, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       "ip6.src == fd69::1/64 && ip6.dst == fd70::1/64",
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-ipv6-2-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV6, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       "ip6.src == fd70::1/64 && ip6.dst == fd69::1/64",
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-ipv6-3-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV6, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       "ip6.src == fd70::1/64 && ip6.dst == fd70::1/64",
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-ipv6-4-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV6, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-service-ipv4-1-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == 10.132.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-service-ipv4-2-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip6.src == fd69::1/64 && ip6.dst == %s", config.Gateway.V6JoinSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-service-ipv6-1-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV6, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip6.src == fd70::1/64 && ip6.dst == %s", config.Gateway.V6JoinSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-service-ipv6-2-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV6, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouter{
						Name: types.OVNClusterRouter,
						UUID: types.OVNClusterRouter + "-UUID",
						Policies: []string{"no-reroute-ipv4-1-UUID", "no-reroute-ipv4-2-UUID", "no-reroute-ipv4-3-UUID", "no-reroute-ipv4-4-UUID",
							"no-reroute-service-ipv4-1-UUID", "no-reroute-service-ipv4-2-UUID",
							"no-reroute-ipv6-1-UUID", "no-reroute-ipv6-2-UUID", "no-reroute-ipv6-3-UUID", "no-reroute-ipv6-4-UUID",
							"no-reroute-service-ipv6-1-UUID", "no-reroute-service-ipv6-2-UUID",
							"default-v4-no-reroute-node-UUID", "default-v6-no-reroute-node-UUID", "default-no-reroute-reply-traffic"},
					},
					&nbdb.LogicalRouter{
						Name:  types.GWRouterPrefix + node1.Name,
						UUID:  types.GWRouterPrefix + node1.Name + "-UUID",
						Ports: []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID"},
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
						Type: "router",
						Options: map[string]string{
							libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalSwitch{
						UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
						Name:  types.ExternalSwitchPrefix + node1Name,
						Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
					},
					node1Switch,
					getDefaultQoSRule(false, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
					getDefaultQoSRule(true, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
					egressSVCServedPodsASv4, egressSVCServedPodsASv6, egressIPServedPodsASv4, egressIPServedPodsASv6, egressNodeIPsASv4, egressNodeIPsASv6,
				}
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should re-balance EgressIPs when their node is removed", func() {
			app.Action = func(*cli.Context) error {
				config.IPv4Mode = true
				config.IPv6Mode = true
				egressIP1 := "192.168.126.101"
				egressIP2 := "192.168.126.102"
				node1IPv4 := "192.168.126.12"
				node1IPv4CIDR := node1IPv4 + "/24"
				node1IPv6 := "::feff:c0a8:8e0c"
				node1IPv6CIDR := node1IPv6 + "/64"
				node2IPv4 := "192.168.126.51"
				node2IPv4CIDR := node2IPv4 + "/24"
				node3IPv4 := "192.168.126.61"
				node3IPv4CIDR := node3IPv4 + "/24"

				egressPod1 := *ovntest.NewPodWithLabels(eipNamespace, podName, node1Name, podV4IP, egressPodLabel)
				egressNamespace := ovntest.NewNamespace(eipNamespace)
				annotations := map[string]string{
					"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4CIDR, node1IPv6CIDR),
					"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":[\"%s\", \"%s\"]}", v4Node1Subnet, v6Node1Subnet),
					util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\",\"%s\"]", node1IPv4CIDR, node1IPv6CIDR),
					util.OvnNodeID:                    "2",
				}
				labels := map[string]string{
					"k8s.ovn.org/egress-assignable": "",
				}
				node1 := getNodeObj(node1Name, annotations, labels)
				annotations = map[string]string{
					"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node2IPv4CIDR, ""),
					"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":\"%s\"}", v4Node1Subnet),
					util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node2IPv4CIDR),
					util.OvnNodeID:                    "3",
					util.OvnTransitSwitchPortAddr:     "{\"ipv4\":\"100.88.0.3/16\"}",
				}
				node2 := getNodeObj(node2Name, annotations, labels)
				annotations = map[string]string{
					"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node3IPv4CIDR, ""),
					"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":\"%s\"}", v4Node1Subnet),
					util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", node3IPv4CIDR),
					util.OvnNodeID:                    "4",
					util.OvnTransitSwitchPortAddr:     "{\"ipv4\":\"100.88.0.4/16\"}",
				}
				node3 := getNodeObj(node3Name, annotations, labels)

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
				node1Switch := &nbdb.LogicalSwitch{
					UUID: node1.Name + "-UUID",
					Name: node1.Name,
				}
				node2Switch := &nbdb.LogicalSwitch{
					UUID: node2.Name + "-UUID",
					Name: node2.Name,
				}
				node3Switch := &nbdb.LogicalSwitch{
					UUID: node3.Name + "-UUID",
					Name: node3.Name,
				}
				defaultRoute := getReRouteStaticRoute(v4ClusterSubnet, nodeLogicalRouterIPv4[0])
				fakeOvn.startWithDBSetup(
					libovsdbtest.TestSetup{
						NBData: []libovsdbtest.TestData{
							&nbdb.LogicalRouterPort{
								UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID",
								Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name,
								Networks: []string{nodeLogicalRouterIfAddrV4, nodeLogicalRouterIfAddrV6},
							},
							&nbdb.LogicalRouterPort{
								UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID",
								Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name,
								Networks: []string{node2LogicalRouterIfAddrV4},
							},
							&nbdb.LogicalRouterPort{
								UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node3.Name + "-UUID",
								Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node3.Name,
								Networks: []string{node3LogicalRouterIfAddrV4},
							},
							&nbdb.LogicalRouter{
								Name: types.OVNClusterRouter,
								UUID: types.OVNClusterRouter + "-UUID",
							},
							&nbdb.LogicalRouter{
								Name:  types.GWRouterPrefix + node1.Name,
								UUID:  types.GWRouterPrefix + node1.Name + "-UUID",
								Ports: []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID"},
							},
							&nbdb.LogicalRouter{
								Name:  types.GWRouterPrefix + node2.Name,
								UUID:  types.GWRouterPrefix + node2.Name + "-UUID",
								Ports: []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID"},
							},
							&nbdb.LogicalRouter{
								Name:  types.GWRouterPrefix + node3.Name,
								UUID:  types.GWRouterPrefix + node3.Name + "-UUID",
								Ports: []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node3.Name + "-UUID"},
							},
							&nbdb.LogicalSwitchPort{
								UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID",
								Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
								Type: "router",
								Options: map[string]string{
									"nat-addresses":             "router",
									"exclude-lb-vips-from-garp": "true",
									libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
								},
							},
							&nbdb.LogicalSwitchPort{
								UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID",
								Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
								Type: "router",
								Options: map[string]string{
									libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
									"nat-addresses":             "router",
									"exclude-lb-vips-from-garp": "true",
								},
							},
							&nbdb.LogicalSwitchPort{
								UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node3Name + "-UUID",
								Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node3Name,
								Type: "router",
								Options: map[string]string{
									libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node3Name,
									"nat-addresses":             "router",
									"exclude-lb-vips-from-garp": "true",
								},
							},
							&nbdb.LogicalSwitch{
								UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
								Name:  types.ExternalSwitchPrefix + node1Name,
								Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
							},
							&nbdb.LogicalSwitch{
								UUID:  types.ExternalSwitchPrefix + node2Name + "-UUID",
								Name:  types.ExternalSwitchPrefix + node2Name,
								Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
							},
							&nbdb.LogicalSwitch{
								UUID:  types.ExternalSwitchPrefix + node3Name + "-UUID",
								Name:  types.ExternalSwitchPrefix + node3Name,
								Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node3Name + "-UUID"},
							},
							node1Switch,
							node2Switch,
							node3Switch,
						},
					},
					&egressipv1.EgressIPList{
						Items: []egressipv1.EgressIP{eIP},
					},
					&corev1.NodeList{
						Items: []corev1.Node{node1, node3},
					},
					&corev1.NamespaceList{
						Items: []corev1.Namespace{*egressNamespace},
					},
					&corev1.PodList{
						Items: []corev1.Pod{egressPod1},
					})

				i, n, _ := net.ParseCIDR(podV4IP + "/23")
				n.IP = i
				fakeOvn.controller.logicalPortCache.add(&egressPod1, "", types.DefaultNetworkName, "", nil, []*net.IPNet{n})

				err := fakeOvn.controller.WatchEgressIPPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIPNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressNodes()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				egressSVCServedPodsASv4, egressSVCServedPodsASv6 := buildEgressServiceAddressSets(nil)
				egressIPServedPodsASv4, egressIPServedPodsASv6 := buildEgressIPServedPodsAddressSets([]string{podV4IP}, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName)
				egressNodeIPsASv4, egressNodeIPsASv6 := buildEgressIPNodeAddressSets([]string{node1IPv4, node1IPv6, node3IPv4})

				node1Switch.QOSRules = []string{"default-QoS-UUID", "default-QoSv6-UUID"}
				expectedDatabaseState := []libovsdbtest.TestData{
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
							egressIPServedPodsASv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": types.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					getNoReRouteReplyTrafficPolicy(types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip6.src == $%s || ip6.src == $%s) && ip6.dst == $%s",
							egressIPServedPodsASv6.Name, egressSVCServedPodsASv6.Name, egressNodeIPsASv6.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-v6-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": types.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV6, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPort{
						UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID",
						Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name,
						Networks: []string{nodeLogicalRouterIfAddrV4, nodeLogicalRouterIfAddrV6},
					},
					&nbdb.LogicalRouterPort{
						UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID",
						Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name,
						Networks: []string{node2LogicalRouterIfAddrV4},
					},
					&nbdb.LogicalRouterPort{
						UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node3.Name + "-UUID",
						Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node3.Name,
						Networks: []string{node3LogicalRouterIfAddrV4},
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-service-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouter{
						Name: types.OVNClusterRouter,
						UUID: types.OVNClusterRouter + "-UUID",
						Policies: []string{"no-reroute-UUID", "no-reroute-service-UUID", "default-no-reroute-node-UUID",
							"default-v6-no-reroute-node-UUID", "default-no-reroute-reply-traffic"},
						StaticRoutes: []string{defaultRoute.UUID},
					},
					&nbdb.LogicalRouter{
						Name:  types.GWRouterPrefix + node1.Name,
						UUID:  types.GWRouterPrefix + node1.Name + "-UUID",
						Ports: []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID"},
					},
					&nbdb.LogicalRouter{
						Name:  types.GWRouterPrefix + node2.Name,
						UUID:  types.GWRouterPrefix + node2.Name + "-UUID",
						Ports: []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID"},
					},
					&nbdb.LogicalRouter{
						Name:  types.GWRouterPrefix + node3.Name,
						UUID:  types.GWRouterPrefix + node3.Name + "-UUID",
						Ports: []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node3.Name + "-UUID"},
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
						Type: "router",
						Options: map[string]string{
							libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
						Type: "router",
						Options: map[string]string{
							libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node3Name + "-UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node3Name,
						Type: "router",
						Options: map[string]string{
							libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node3Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalSwitch{
						UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
						Name:  types.ExternalSwitchPrefix + node1Name,
						Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
					},
					&nbdb.LogicalSwitch{
						UUID:  types.ExternalSwitchPrefix + node2Name + "-UUID",
						Name:  types.ExternalSwitchPrefix + node2Name,
						Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
					},
					&nbdb.LogicalSwitch{
						UUID:  types.ExternalSwitchPrefix + node3Name + "-UUID",
						Name:  types.ExternalSwitchPrefix + node3Name,
						Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node3Name + "-UUID"},
					},
					node1Switch,
					node2Switch,
					node3Switch,
					defaultRoute,
					getDefaultQoSRule(false, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
					getDefaultQoSRule(true, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
					egressSVCServedPodsASv4, egressSVCServedPodsASv6, egressIPServedPodsASv4, egressIPServedPodsASv6, egressNodeIPsASv4, egressNodeIPsASv6,
				}
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				status := []egressipv1.EgressIPStatusItem{
					{
						Node:     node1Name,
						EgressIP: egressIP1,
					},
					{
						Node:     node3Name,
						EgressIP: egressIP2,
					},
				}
				err = fakeOvn.controller.eIPC.patchReplaceEgressIPStatus(egressIPName, status)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(2))
				gomega.Eventually(getEgressIPReassignmentCount).Should(gomega.Equal(0))

				egressIPs, nodes := getEgressIPStatus(egressIPName)
				gomega.Expect(nodes[0]).To(gomega.Equal(node1.Name))
				gomega.Expect(nodes[1]).To(gomega.Equal(node3.Name))

				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Create(context.TODO(), &node2, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				egressNodeIPsASv4, egressNodeIPsASv6 = buildEgressIPNodeAddressSets([]string{node1IPv4, node1IPv6, node2IPv4, node3IPv4})
				expectedNatLogicalPort1 := "k8s-node1"
				expectedDatabaseState = []libovsdbtest.TestData{
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
							egressIPServedPodsASv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": types.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					getNoReRouteReplyTrafficPolicy(types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip6.src == $%s || ip6.src == $%s) && ip6.dst == $%s",
							egressIPServedPodsASv6.Name, egressSVCServedPodsASv6.Name, egressNodeIPsASv6.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-v6-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": types.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV6, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPort{
						UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID",
						Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name,
						Networks: []string{nodeLogicalRouterIfAddrV4, nodeLogicalRouterIfAddrV6},
					},
					&nbdb.LogicalRouterPort{
						UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID",
						Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name,
						Networks: []string{node2LogicalRouterIfAddrV4},
					},
					&nbdb.LogicalRouterPort{
						UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node3.Name + "-UUID",
						Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node3.Name,
						Networks: []string{node3LogicalRouterIfAddrV4},
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-service-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority: types.EgressIPReroutePriority,
						Match:    fmt.Sprintf("ip4.src == %s", egressPod1.Status.PodIP),
						Action:   nbdb.LogicalRouterPolicyActionReroute,
						Nexthops: []string{"100.64.0.2", "100.88.0.4"},
						ExternalIDs: getEgressIPLRPReRouteDbIDs(egressIPName, egressPod1.Namespace, egressPod1.Name,
							"ip4", types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
						UUID: "reroute-UUID1",
					},
					&nbdb.NAT{
						UUID:        "egressip-nat-UUID1",
						LogicalIP:   podV4IP,
						ExternalIP:  egressIPs[0],
						ExternalIDs: getEgressIPNATDbIDs(egressIPName, egressPod1.Namespace, egressPod1.Name, IPFamilyValueV4, fakeOvn.controller.controllerName).GetExternalIDs(),
						Type:        nbdb.NATTypeSNAT,
						LogicalPort: &expectedNatLogicalPort1,
						Options: map[string]string{
							"stateless": "false",
						},
					},
					&nbdb.LogicalRouter{
						Name: types.OVNClusterRouter,
						UUID: types.OVNClusterRouter + "-UUID",
						Policies: []string{"no-reroute-UUID", "no-reroute-service-UUID", "default-no-reroute-node-UUID",
							"default-v6-no-reroute-node-UUID", "default-no-reroute-reply-traffic", "reroute-UUID1"},
						StaticRoutes: []string{defaultRoute.UUID},
					},
					&nbdb.LogicalRouter{
						Name:  types.GWRouterPrefix + node1.Name,
						UUID:  types.GWRouterPrefix + node1.Name + "-UUID",
						Ports: []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID"},
						Nat:   []string{"egressip-nat-UUID1"},
					},
					&nbdb.LogicalRouter{
						Name:  types.GWRouterPrefix + node2.Name,
						UUID:  types.GWRouterPrefix + node2.Name + "-UUID",
						Ports: []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID"},
					},
					&nbdb.LogicalRouter{
						Name:  types.GWRouterPrefix + node3.Name,
						UUID:  types.GWRouterPrefix + node3.Name + "-UUID",
						Ports: []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node3.Name + "-UUID"},
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
						Type: "router",
						Options: map[string]string{
							libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
						Type: "router",
						Options: map[string]string{
							libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node3Name + "-UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node3Name,
						Type: "router",
						Options: map[string]string{
							libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node3Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalSwitch{
						UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
						Name:  types.ExternalSwitchPrefix + node1Name,
						Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
					},
					&nbdb.LogicalSwitch{
						UUID:  types.ExternalSwitchPrefix + node2Name + "-UUID",
						Name:  types.ExternalSwitchPrefix + node2Name,
						Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
					},
					&nbdb.LogicalSwitch{
						UUID:  types.ExternalSwitchPrefix + node3Name + "-UUID",
						Name:  types.ExternalSwitchPrefix + node3Name,
						Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node3Name + "-UUID"},
					},
					node1Switch,
					node2Switch,
					node3Switch,
					defaultRoute,
					getDefaultQoSRule(false, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
					getDefaultQoSRule(true, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
					egressSVCServedPodsASv4, egressSVCServedPodsASv6, egressIPServedPodsASv4, egressIPServedPodsASv6, egressNodeIPsASv4, egressNodeIPsASv6,
				}
				gomega.Eventually(fakeOvn.nbClient, 5).Should(libovsdbtest.HaveData(expectedDatabaseState))

				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(2))
				_, nodes = getEgressIPStatus(egressIPName)
				gomega.Expect(nodes[0]).To(gomega.Equal(node1.Name))
				gomega.Expect(nodes[1]).To(gomega.Equal(node3.Name))

				err = fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Delete(context.TODO(), node3.Name, *metav1.NewDeleteOptions(0))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

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
				err = fakeOvn.controller.eIPC.patchReplaceEgressIPStatus(egressIPName, status)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(getEgressIPStatusLen(egressIPName)).Should(gomega.Equal(2))
				gomega.Eventually(getEgressIPReassignmentCount).Should(gomega.Equal(0))

				egressIPs, nodes = getEgressIPStatus(egressIPName)
				gomega.Expect(nodes[0]).To(gomega.Equal(node1.Name))
				gomega.Expect(nodes[1]).To(gomega.Equal(node2.Name))

				egressNodeIPsASv4, egressNodeIPsASv6 = buildEgressIPNodeAddressSets([]string{node1IPv4, node1IPv6, node2IPv4})
				expectedDatabaseState = []libovsdbtest.TestData{
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
							egressIPServedPodsASv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": types.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					getNoReRouteReplyTrafficPolicy(types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
					&nbdb.LogicalRouterPolicy{
						Priority: types.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip6.src == $%s || ip6.src == $%s) && ip6.dst == $%s",
							egressIPServedPodsASv6.Name, egressSVCServedPodsASv6.Name, egressNodeIPsASv6.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-v6-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": types.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV6, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPort{
						UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID",
						Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name,
						Networks: []string{nodeLogicalRouterIfAddrV4, nodeLogicalRouterIfAddrV6},
					},
					&nbdb.LogicalRouterPort{
						UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID",
						Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name,
						Networks: []string{node2LogicalRouterIfAddrV4},
					},
					&nbdb.LogicalRouterPort{
						UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node3.Name + "-UUID",
						Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node3.Name,
						Networks: []string{node3LogicalRouterIfAddrV4},
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    types.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-service-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority: types.EgressIPReroutePriority,
						Match:    fmt.Sprintf("ip4.src == %s", egressPod1.Status.PodIP),
						Action:   nbdb.LogicalRouterPolicyActionReroute,
						Nexthops: []string{"100.64.0.2", "100.88.0.3"},
						ExternalIDs: getEgressIPLRPReRouteDbIDs(egressIPName, egressPod1.Namespace, egressPod1.Name,
							"ip4", types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
						UUID: "reroute-UUID1",
					},
					&nbdb.NAT{
						UUID:        "egressip-nat-UUID2",
						LogicalIP:   podV4IP,
						ExternalIP:  egressIPs[0],
						ExternalIDs: getEgressIPNATDbIDs(egressIPName, egressPod1.Namespace, egressPod1.Name, IPFamilyValueV4, fakeOvn.controller.controllerName).GetExternalIDs(),
						Type:        nbdb.NATTypeSNAT,
						LogicalPort: &expectedNatLogicalPort1,
						Options: map[string]string{
							"stateless": "false",
						},
					},
					&nbdb.LogicalRouter{
						Name: types.OVNClusterRouter,
						UUID: types.OVNClusterRouter + "-UUID",
						Policies: []string{"no-reroute-UUID", "no-reroute-service-UUID", "default-no-reroute-node-UUID",
							"default-v6-no-reroute-node-UUID", "default-no-reroute-reply-traffic", "reroute-UUID1"},
						StaticRoutes: []string{defaultRoute.UUID},
					},
					&nbdb.LogicalRouter{
						Name:  types.GWRouterPrefix + node1.Name,
						UUID:  types.GWRouterPrefix + node1.Name + "-UUID",
						Ports: []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID"},
						Nat:   []string{"egressip-nat-UUID2"},
					},
					&nbdb.LogicalRouter{
						Name:  types.GWRouterPrefix + node2.Name,
						UUID:  types.GWRouterPrefix + node2.Name + "-UUID",
						Ports: []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node2.Name + "-UUID"},
					},
					&nbdb.LogicalRouter{
						Name:  types.GWRouterPrefix + node3.Name,
						UUID:  types.GWRouterPrefix + node3.Name + "-UUID",
						Ports: []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node3.Name + "-UUID"},
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name,
						Type: "router",
						Options: map[string]string{
							libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node1Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name,
						Type: "router",
						Options: map[string]string{
							libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node2Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalSwitchPort{
						UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node3Name + "-UUID",
						Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node3Name,
						Type: "router",
						Options: map[string]string{
							libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node3Name,
							"nat-addresses":             "router",
							"exclude-lb-vips-from-garp": "true",
						},
					},
					&nbdb.LogicalSwitch{
						UUID:  types.ExternalSwitchPrefix + node1Name + "-UUID",
						Name:  types.ExternalSwitchPrefix + node1Name,
						Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
					},
					&nbdb.LogicalSwitch{
						UUID:  types.ExternalSwitchPrefix + node2Name + "-UUID",
						Name:  types.ExternalSwitchPrefix + node2Name,
						Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node2Name + "-UUID"},
					},
					&nbdb.LogicalSwitch{
						UUID:  types.ExternalSwitchPrefix + node3Name + "-UUID",
						Name:  types.ExternalSwitchPrefix + node3Name,
						Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node3Name + "-UUID"},
					},
					node1Switch,
					node2Switch,
					node3Switch,
					defaultRoute,
					getDefaultQoSRule(false, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
					getDefaultQoSRule(true, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
					egressSVCServedPodsASv4, egressSVCServedPodsASv6, egressIPServedPodsASv4, egressIPServedPodsASv6, egressNodeIPsASv4, egressNodeIPsASv6,
				}
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

	})

	ginkgo.Context("Sync", func() {
		ginkgo.DescribeTable("remove invalid next hop from LRP", func(isV6 bool) {
			// scenario is OVNDB is config'd with pod is local and EIP egress is local and remote.
			// The EgressIPs assigned Nodes is however different from the OVNDB config therefore we expect Sync to
			// reconcile and update the local pod LRP (attached to cluster router) next hops to reflect the current state.
			// removes invalid next hop from LRP. Controller may have missed the signal that the EIP moved to another node.
			app.Action = func(*cli.Context) error {
				if isV6 {
					config.IPv6Mode = true
				} else {
					config.IPv4Mode = true
				}
				getSupportedByIPFamily := func(inputForV4, inputForV6 string) string { // helper to return the current supported single stack IP family (IP/CIDR/IP family name)
					if isV6 {
						return inputForV6
					}
					return inputForV4
				}
				getSupportedIPFamilyForDBID := func() egressIPFamilyValue {
					if isV6 {
						return IPFamilyValueV6
					}
					return IPFamilyValueV4
				}
				getSupportedOVNIPFamily := func() string {
					if isV6 {
						return "6"
					}
					return "4"
				}
				type nodeInfo struct { // help construct a Node obj / ovn config for each test node
					nodeName     string
					primaryINFIP string
					podSubnet    string
					transitSWIP  string
				}
				// node 1 is index 0, node 2 is index 1, node 3 is index 2 in the slice
				nodes := []nodeInfo{{node1Name, getSupportedByIPFamily("192.168.126.12", "fc00:f853:ccd:e793::2"),
					getSupportedByIPFamily(v4Node1Subnet, v6Node1Subnet), getSupportedByIPFamily("100.88.0.2", "fd97::2")},
					{node2Name, getSupportedByIPFamily("192.168.126.13", "fc00:f853:ccd:e793::3"),
						getSupportedByIPFamily(v4Node2Subnet, v6Node2Subnet), getSupportedByIPFamily("100.88.0.3", "fd97::3")},
					{node3Name, getSupportedByIPFamily("192.168.126.14", "fc00:f853:ccd:e793::4"),
						getSupportedByIPFamily(v4Node3Subnet, v6Node3Subnet), getSupportedByIPFamily("100.88.0.4", "fd97::4")}}
				mask := "/24" // use same mask for all IP family's since it doesn't matter for this test

				generateNodeObj := func(n nodeInfo) corev1.Node {
					// used to populate annotations - supports only a single IP family
					addValueToIPFamilyKey := func(value string) string {
						key := "ipv4"
						if isV6 {
							key = "ipv6"
						}
						return fmt.Sprintf("\"%s\":\"%s\"", key, value)
					}
					nodeAnnotations := map[string]string{
						"k8s.ovn.org/l3-gateway-config":               fmt.Sprintf(`{"default":{"mode":"shared","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"%s", "next-hop":"192.168.126.1"}}`, n.primaryINFIP+mask),
						"k8s.ovn.org/node-primary-ifaddr":             fmt.Sprintf("{%s}", addValueToIPFamilyKey(n.primaryINFIP+mask)),
						"k8s.ovn.org/node-subnets":                    fmt.Sprintf("{\"default\":[\"%s\"]}", n.podSubnet),
						"k8s.ovn.org/node-transit-switch-port-ifaddr": fmt.Sprintf("{%s}", addValueToIPFamilyKey(n.transitSWIP+mask)),
						util.OVNNodeHostCIDRs:                         fmt.Sprintf("[\"%s\"]", n.primaryINFIP+mask),
					}
					return getNodeObj(n.nodeName, nodeAnnotations, map[string]string{})
				}
				node1, node2, node3 := generateNodeObj(nodes[0]), generateNodeObj(nodes[1]), generateNodeObj(nodes[2])
				egressNamespace := ovntest.NewNamespace(eipNamespace)
				podIP := getSupportedByIPFamily(podV4IP, podV6IP)
				egressPod := ovntest.NewPodWithLabels(eipNamespace, podName, node1.Name, podIP, egressPodLabel)
				eipIP1 := getSupportedByIPFamily("192.168.126.200", "fc00:f853:ccd:e793::200")
				eipIP2 := getSupportedByIPFamily("192.168.126.201", "fc00:f853:ccd:e793::201")
				ginkgo.By("creating EgressIP that is assigned to node 2 and node 3")
				eIPObj := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{
							eipIP1,
							eipIP2,
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
					Status: egressipv1.EgressIPStatus{
						Items: []egressipv1.EgressIPStatusItem{
							// node 1 was assigned eipIP1 however, it is no longer assigned to this node. The OVN DBs will start
							// with node 1 configured with IP eipIP1 and upon sync, the OVN DB must update to reflect
							// the state defined here in the EgressIPStatus
							{
								Node:     node2.Name, // remote
								EgressIP: eipIP1,
							},
							{
								Node:     node3.Name, // remote
								EgressIP: eipIP2,
							},
						},
					},
				}
				ginkgo.By("start OVN DBs with egress nodes set to node 1 & 2 (EIP status states node 2 & 3)")
				getNodeLogicalPortName := func(nodeName string) *string { n := "k8s-" + nodeName; return &n }
				// node 1 is a stale next hop. EIP IP moved to node 3.
				node1GWToJoinIP := getSupportedByIPFamily(nodeLogicalRouterIPv4[0], nodeLogicalRouterIPv6[0])
				staleHops := []string{node1GWToJoinIP, nodes[1].transitSWIP} // nodes[1] is node 2

				fakeOvn.startWithDBSetup(
					libovsdbtest.TestSetup{
						NBData: []libovsdbtest.TestData{
							&nbdb.NBGlobal{UUID: "nbglobal-UUID", Name: node1.Name},
							// LRPs to support EIP assigned to local and a remote node
							getReRoutePolicy(podIP, getSupportedOVNIPFamily(), "stale-reroute-UUID",
								staleHops, getEgressIPLRPReRouteDbIDs(eIPObj.Name, egressPod.Namespace, egressPod.Name,
									getSupportedIPFamilyForDBID(), types.DefaultNetworkName, types.DefaultNetworkControllerName).GetExternalIDs()),
							// stale NAT to support EIP that was previously assigned to the local node but OVN DB config persists
							&nbdb.NAT{
								UUID:       "stale-nat-UUID",
								LogicalIP:  podIP,
								ExternalIP: eipIP1,
								ExternalIDs: getEgressIPNATDbIDs(egressIPName, egressPod.Namespace, egressPod.Name, getSupportedIPFamilyForDBID(),
									types.DefaultNetworkControllerName).GetExternalIDs(),
								Type:        nbdb.NATTypeSNAT,
								LogicalPort: getNodeLogicalPortName(node1.Name),
								Options: map[string]string{
									"stateless": "false",
								},
							},
							&nbdb.LogicalRouterPort{
								UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID",
								Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name,
								Networks: []string{node1GWToJoinIP + mask},
							},
							&nbdb.LogicalRouter{
								Name:     types.OVNClusterRouter,
								UUID:     types.OVNClusterRouter + "-UUID",
								Policies: []string{"stale-reroute-UUID"},
							},
							&nbdb.LogicalRouter{
								Name:  types.GWRouterPrefix + node1.Name,
								UUID:  types.GWRouterPrefix + node1.Name + "-UUID",
								Ports: []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID"},
								Nat:   []string{"stale-nat-UUID"},
							},
							&nbdb.LogicalSwitchPort{
								UUID: "k8s-" + node1.Name + "-UUID",
								Name: "k8s-" + node1.Name,
								Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(
									ovntest.MustParseIPNet(nodes[0].podSubnet)).IP.String()},
							},
							&nbdb.LogicalSwitch{
								UUID:  node1.Name + "-UUID",
								Name:  node1.Name,
								Ports: []string{"k8s-" + node1.Name + "-UUID"},
							},
						},
					},
					&corev1.NamespaceList{
						Items: []corev1.Namespace{*egressNamespace},
					},
					&corev1.PodList{
						Items: []corev1.Pod{*egressPod},
					},
					&corev1.NodeList{
						Items: []corev1.Node{node1, node2, node3},
					},
					&egressipv1.EgressIPList{
						Items: []egressipv1.EgressIP{eIPObj},
					},
				)
				i, podIPv4Net, _ := net.ParseCIDR(podIP + mask)
				podIPv4Net.IP = i
				fakeOvn.controller.logicalPortCache.add(egressPod, "", types.DefaultNetworkName, "",
					nil, []*net.IPNet{podIPv4Net})

				err := fakeOvn.controller.WatchEgressIPPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIPNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				ginkgo.By("ensuring removal of invalid local external gateway next hop")
				egressIPServedPodsASv4, egressIPServedPodsASv6 := buildEgressIPServedPodsAddressSets([]string{podIP},
					types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName)
				egressIPServedPodsAS := egressIPServedPodsASv4
				if isV6 {
					egressIPServedPodsAS = egressIPServedPodsASv6
				}
				validNextHops := []string{nodes[1].transitSWIP, nodes[2].transitSWIP} // node 2 and 3
				expectedDatabaseState := []libovsdbtest.TestData{
					&nbdb.NBGlobal{UUID: "nbglobal-UUID", Name: node1.Name},
					// LRPs to support EIP assigned to a remote node
					// valid LRP for IPv4/IPv6. IPv4 Egress Node is local, IPv6 is remote
					getReRoutePolicy(podIP, getSupportedOVNIPFamily(), "valid-reroute-UUID",
						validNextHops, getEgressIPLRPReRouteDbIDs(eIPObj.Name, egressPod.Namespace, egressPod.Name,
							getSupportedIPFamilyForDBID(), types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs()),
					&nbdb.LogicalRouter{
						Name:     types.OVNClusterRouter,
						UUID:     types.OVNClusterRouter + "-UUID",
						Policies: []string{"valid-reroute-UUID"},
					},
					&nbdb.LogicalRouterPort{
						UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID",
						Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name,
						Networks: []string{node1GWToJoinIP + mask},
					},
					&nbdb.LogicalRouter{
						Name:  types.GWRouterPrefix + node1.Name,
						UUID:  types.GWRouterPrefix + node1.Name + "-UUID",
						Ports: []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1.Name + "-UUID"},
					},
					&nbdb.LogicalSwitchPort{
						UUID: "k8s-" + node1.Name + "-UUID",
						Name: "k8s-" + node1.Name,
						Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(
							ovntest.MustParseIPNet(nodes[0].podSubnet)).IP.String()},
					},
					&nbdb.LogicalSwitch{
						UUID:  node1.Name + "-UUID",
						Name:  node1.Name,
						Ports: []string{"k8s-" + node1.Name + "-UUID"},
					},
					egressIPServedPodsAS,
				}
				// if IPv6 enabled and IPv4 is disabled, we still create the IPv4 served pods AS
				if isV6 {
					expectedDatabaseState = append(expectedDatabaseState, egressIPServedPodsASv4)
				}
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
				ginkgo.By("ensure config is consistent")
				gomega.Consistently(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}, ginkgo.Entry("IPv4", false), ginkgo.Entry("IPv6", true))

		ginkgo.It("removes config for previously selected pods on a deleted Node", func() {
			// node 1 is local zone and egress Node.
			// pod was on node 2 but it is deleted. Node 2 previously was also an egress Node.
			app.Action = func(*cli.Context) error {
				// dual stack cluster
				config.IPv4Mode = true
				config.IPv6Mode = true
				egressNamespace := ovntest.NewNamespace(eipNamespace)
				egressPod := corev1.Pod{
					ObjectMeta: ovntest.NewPodMeta(eipNamespace, podName, egressPodLabel),
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "containerName",
								Image: "containerImage",
							},
						},
						NodeName: node1Name,
					},
					Status: corev1.PodStatus{
						Phase:  corev1.PodRunning,
						PodIP:  podV4IP,
						PodIPs: []corev1.PodIP{{IP: podV4IP}, {IP: podV6IP}},
					},
				}
				// node 1 (local zone)
				node1IPv4 := "192.168.126.210"
				Node1IPv4CIDR := node1IPv4 + "/24"
				node1IPv6 := "fc00:f853:ccd:e793::30"
				node1IPv6CIDR := node1IPv6 + "/64"
				node1TranSwitchIPv4CIDR := "100.88.0.2/16"
				node1TranSwitchIPv6CIDR := "fd97::2/64"
				_, node1IPV4Net, _ := net.ParseCIDR(v4Node1Subnet)
				_, node1IPV6Net, _ := net.ParseCIDR(v6Node1Subnet)
				nodeAnnotations := map[string]string{
					"k8s.ovn.org/l3-gateway-config":               `{"default":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"192.168.126.12/24", "next-hop":"192.168.126.1"}}`,
					util.OvnNodeID:                                "2",
					"k8s.ovn.org/node-primary-ifaddr":             fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", Node1IPv4CIDR, node1IPv6CIDR),
					"k8s.ovn.org/node-subnets":                    fmt.Sprintf("{\"default\":[\"%s\", \"%s\"]}", v4Node1Subnet, v6Node1Subnet),
					"k8s.ovn.org/node-transit-switch-port-ifaddr": fmt.Sprintf("{\"ipv4\":\"%s\", \"ipv6\": \"%s\"}", node1TranSwitchIPv4CIDR, node1TranSwitchIPv6CIDR),
					util.OVNNodeHostCIDRs:                         fmt.Sprintf("[\"%s\",\"%s\"]", Node1IPv4CIDR, node1IPv6CIDR),
				}
				node1 := getNodeObj(node1Name, nodeAnnotations, map[string]string{}) // add node to avoid error-ing out on transit switch IP fetch
				// node 2 - deleted (remote zone)
				node2TranSwitchIPv6 := "fd97::3"
				eipIPv4 := "192.168.126.200"
				eipIPv6 := "0:0:0:0:0:feff:c0a8:8e0d"
				deletedPodIPv4 := podV4IP2
				// dual IP family EIP selecting one pod in local zone
				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{
							eipIPv4,
							eipIPv6,
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
					Status: egressipv1.EgressIPStatus{
						Items: []egressipv1.EgressIPStatusItem{
							{
								Node:     node1Name,
								EgressIP: eipIPv4,
							},
							// Previous was set to node 2 and Node was deleted while local zone EIP controller was down.
							//{
							//	Node:     node2Name,
							//	EgressIP: eipIPv6,
							//},
						},
					},
				}
				ginkgo.By("start OVN DBs with valid and invalid (pod doesn't exist..) OVN config")
				node1NatLogicalPortName := "k8s-" + node1Name
				fakeOvn.nodeName = node1Name
				fakeOvn.startWithDBSetup(
					libovsdbtest.TestSetup{
						NBData: []libovsdbtest.TestData{
							// LRPs to support EIP assigned to a remote node node thats deleted while the controller was down
							// Valid LRP for IPv4 egress node. IPv4 egress Node is local. IPv6 egress node is remote and deleted but ovn config remains
							getReRoutePolicy(podV4IP, "4", "valid-reroute-ipv4-UUID",
								nodeLogicalRouterIPv4, getEgressIPLRPReRouteDbIDs(eIP.Name, egressPod.Namespace, egressPod.Name,
									IPFamilyValueV4, types.DefaultNetworkName, types.DefaultNetworkControllerName).GetExternalIDs()),
							// invalid LRP for IPv6 because remove node is deleted
							getReRoutePolicy(podV6IP, "6", "invalid-reroute-ipv6-UUID",
								[]string{node2TranSwitchIPv6}, getEgressIPLRPReRouteDbIDs(eIP.Name, egressPod.Namespace, egressPod.Name,
									IPFamilyValueV6, types.DefaultNetworkName, types.DefaultNetworkControllerName).GetExternalIDs()),
							// NATs to support EIP assigned to the local node
							// valid NAT
							&nbdb.NAT{
								UUID:        "valid-nat-ipv4-UUID",
								LogicalIP:   podV4IP,
								ExternalIP:  eipIPv4,
								ExternalIDs: getEgressIPNATDbIDs(egressIPName, egressPod.Namespace, egressPod.Name, IPFamilyValueV4, types.DefaultNetworkControllerName).GetExternalIDs(),
								Type:        nbdb.NATTypeSNAT,
								LogicalPort: &node1NatLogicalPortName,
								Options: map[string]string{
									"stateless": "false",
								},
							},
							// invalid NAT for a deleted pod on remote node
							&nbdb.NAT{
								UUID:        "invalid-nat-ipv4-UUID",
								LogicalIP:   deletedPodIPv4,
								ExternalIP:  eipIPv4,
								ExternalIDs: getEgressIPNATDbIDs(egressIPName, egressNamespace.Namespace, "deletedpod", IPFamilyValueV4, types.DefaultNetworkControllerName).GetExternalIDs(),
								Type:        nbdb.NATTypeSNAT,
								LogicalPort: &node1NatLogicalPortName,
								Options: map[string]string{
									"stateless": "false",
								},
							},
							&nbdb.LogicalRouterPort{
								UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1Name + "-UUID",
								Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1Name,
								Networks: []string{nodeLogicalRouterIfAddrV6, nodeLogicalRouterIfAddrV4},
							},
							&nbdb.LogicalRouter{
								Name:     types.OVNClusterRouter,
								UUID:     types.OVNClusterRouter + "-UUID",
								Policies: []string{"valid-reroute-ipv4-UUID", "invalid-reroute-ipv6-UUID"},
							},
							&nbdb.LogicalRouter{
								Name:    types.GWRouterPrefix + node1Name,
								UUID:    types.GWRouterPrefix + node1Name + "-UUID",
								Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
								Options: map[string]string{"dynamic_neigh_routers": "false"},
								Nat:     []string{"valid-nat-ipv4-UUID", "invalid-nat-ipv4-UUID"},
							},
							&nbdb.LogicalSwitchPort{
								UUID: "k8s-" + node1.Name + "-UUID",
								Name: "k8s-" + node1.Name,
								Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1IPV4Net).IP.String(),
									"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1IPV6Net).IP.String()},
							},
							&nbdb.LogicalSwitch{
								UUID:  node1.Name + "-UUID",
								Name:  node1.Name,
								Ports: []string{"k8s-" + node1.Name + "-UUID"},
							},
						},
					},
					&corev1.NamespaceList{
						Items: []corev1.Namespace{*egressNamespace},
					},
					&corev1.PodList{
						Items: []corev1.Pod{egressPod},
					},
					&corev1.NodeList{
						Items: []corev1.Node{node1},
					},
					&egressipv1.EgressIPList{
						Items: []egressipv1.EgressIP{eIP},
					},
				)
				i, podIPv4Net, _ := net.ParseCIDR(podV4IP + "/23")
				podIPv4Net.IP = i
				i, podIPv6Net, _ := net.ParseCIDR(podV6IP + "/23")
				podIPv6Net.IP = i
				fakeOvn.controller.logicalPortCache.add(&egressPod, "", types.DefaultNetworkName, "", nil, []*net.IPNet{podIPv4Net, podIPv6Net})

				err := fakeOvn.controller.WatchEgressIPPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIPNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				ginkgo.By("ensuring cleanup of invalid LRP and NAT")
				egressIPServedPodsASv4, egressIPServedPodsASv6 := buildEgressIPServedPodsAddressSets([]string{podV4IP, podV6IP}, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName)
				expectedDatabaseState := []libovsdbtest.TestData{
					getReRoutePolicy(podV4IP, "4", "valid-reroute-ipv4-UUID",
						nodeLogicalRouterIPv4, getEgressIPLRPReRouteDbIDs(eIP.Name, egressPod.Namespace, egressPod.Name,
							IPFamilyValueV4, types.DefaultNetworkName, types.DefaultNetworkControllerName).GetExternalIDs()),
					&nbdb.NAT{
						UUID:        "valid-egressip-nat-UUID",
						LogicalIP:   podV4IP,
						ExternalIP:  eipIPv4,
						ExternalIDs: getEgressIPNATDbIDs(egressIPName, egressPod.Namespace, egressPod.Name, IPFamilyValueV4, fakeOvn.controller.controllerName).GetExternalIDs(),
						Type:        nbdb.NATTypeSNAT,
						LogicalPort: &node1NatLogicalPortName,
						Options: map[string]string{
							"stateless": "false",
						},
					},
					&nbdb.LogicalRouter{
						Name:     types.OVNClusterRouter,
						UUID:     types.OVNClusterRouter + "-UUID",
						Policies: []string{"valid-reroute-ipv4-UUID"},
					},
					&nbdb.LogicalRouterPort{
						UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1Name + "-UUID",
						Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1Name,
						Networks: []string{nodeLogicalRouterIfAddrV6, nodeLogicalRouterIfAddrV4},
					},
					&nbdb.LogicalRouter{
						Name:    types.GWRouterPrefix + node1Name,
						UUID:    types.GWRouterPrefix + node1Name + "-UUID",
						Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
						Nat:     []string{"valid-egressip-nat-UUID"},
						Options: map[string]string{"dynamic_neigh_routers": "false"},
					},
					&nbdb.LogicalSwitchPort{
						UUID: "k8s-" + node1.Name + "-UUID",
						Name: "k8s-" + node1.Name,
						Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1IPV4Net).IP.String(),
							"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1IPV6Net).IP.String()},
					},
					&nbdb.LogicalSwitch{
						UUID:  node1.Name + "-UUID",
						Name:  node1.Name,
						Ports: []string{"k8s-" + node1.Name + "-UUID"},
					},
					egressIPServedPodsASv4,
					egressIPServedPodsASv6,
				}
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
				ginkgo.By("ensure config is consistent")
				gomega.Consistently(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("remove invalid OVN config for deleted pod", func() {
			// removes invalid SNAT/NAT for a pod that was selected by an EIP but was removed while controller was not running and therefore OVN config should be removed
			// does not modify valid SNAT/NAT
			// further references to local or remote are relative to the controller zone for IC.
			// one EIP object with two assigned IPs of different IP families (v4 and v6) which select one pod that's local
			app.Action = func(*cli.Context) error {
				// dual stack cluster
				config.IPv4Mode = true
				config.IPv6Mode = true
				egressPod := corev1.Pod{
					ObjectMeta: ovntest.NewPodMeta(eipNamespace, podName, egressPodLabel),
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "containerName",
								Image: "containerImage",
							},
						},
						NodeName: node1Name,
					},
					Status: corev1.PodStatus{
						Phase:  corev1.PodRunning,
						PodIP:  podV4IP,
						PodIPs: []corev1.PodIP{{IP: podV4IP}, {IP: podV6IP}},
					},
				}
				// deletedPodIP is a pod IP of a Pod that was deleted while eip controller was not running therefore config will exist in OVN DBs to support EIP
				deletedPodIPv4, deletedPod2IPv4, deletedPodIPv6, deletedPod2IPv6 := podV4IP2, "10.128.0.20", podV6IP2, podV6IP3
				egressNamespace := ovntest.NewNamespace(eipNamespace)
				// node 1 (local zone)
				node1IPv4 := "192.168.126.210"
				Node1IPv4CIDR := node1IPv4 + "/24"
				node1IPv6 := "fc00:f853:ccd:e793::30"
				node1IPv6CIDR := node1IPv6 + "/64"
				node1TranSwitchIPv4CIDR := "100.88.0.2/16"
				node1TranSwitchIPv6CIDR := "fd97::2/64"
				_, node1IPV4Net, _ := net.ParseCIDR(v4Node1Subnet)
				_, node1IPV6Net, _ := net.ParseCIDR(v6Node1Subnet)
				nodeAnnotations := map[string]string{
					"k8s.ovn.org/l3-gateway-config":               `{"default":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"192.168.126.12/24", "next-hop":"192.168.126.1"}}`,
					util.OvnNodeID:                                "2",
					"k8s.ovn.org/node-primary-ifaddr":             fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", Node1IPv4CIDR, node1IPv6CIDR),
					"k8s.ovn.org/node-subnets":                    fmt.Sprintf("{\"default\":[\"%s\", \"%s\"]}", v4Node1Subnet, v6Node1Subnet),
					"k8s.ovn.org/node-transit-switch-port-ifaddr": fmt.Sprintf("{\"ipv4\":\"%s\", \"ipv6\": \"%s\"}", node1TranSwitchIPv4CIDR, node1TranSwitchIPv6CIDR),
					util.OVNNodeHostCIDRs:                         fmt.Sprintf("[\"%s\",\"%s\"]", Node1IPv4CIDR, node1IPv6CIDR),
				}
				node1 := getNodeObj(node1Name, nodeAnnotations, map[string]string{}) // add node to avoid error-ing out on transit switch IP fetch
				// node 2 (remote zone)
				node2IPv4 := "192.168.126.202"
				node2IPv4CIDR := node2IPv4 + "/24"
				node2IPv6 := "fc00:f853:cce:e793::20"
				node2IPv6CIDR := node2IPv6 + "/64"
				node2TranSwitchIPv4 := "100.88.0.3"
				node2TranSwitchIPv4CIDR := node2TranSwitchIPv4 + "/16"
				node2TranSwitchIPv6 := "fd97::3"
				node2TranSwitchIPv6CIDR := node2TranSwitchIPv6 + "/64"
				_, node2IPV4Net, _ := net.ParseCIDR(v4Node2Subnet)
				_, node2IPV6Net, _ := net.ParseCIDR(v6Node2Subnet)
				nodeAnnotations = map[string]string{
					util.OvnNodeID:                                "3",
					"k8s.ovn.org/node-primary-ifaddr":             fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node2IPv4CIDR, node2IPv6CIDR),
					"k8s.ovn.org/node-subnets":                    fmt.Sprintf("{\"default\":[\"%s\", \"%s\"]}", v4Node2Subnet, v6Node2Subnet),
					"k8s.ovn.org/node-transit-switch-port-ifaddr": fmt.Sprintf("{\"ipv4\":\"%s\", \"ipv6\": \"%s\"}", node2TranSwitchIPv4CIDR, node2TranSwitchIPv6CIDR),
					util.OVNNodeHostCIDRs:                         fmt.Sprintf("[\"%s\",\"%s\"]", node2IPv4CIDR, node2IPv6CIDR),
				}
				node2 := getNodeObj(node2Name, nodeAnnotations, map[string]string{})
				eipIPv4 := "192.168.126.200"
				eipIPv6 := "0:0:0:0:0:feff:c0a8:8e0d"
				// dual IP family EIP selecting one pod in local zone
				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{
							eipIPv4,
							eipIPv6,
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
					Status: egressipv1.EgressIPStatus{
						Items: []egressipv1.EgressIPStatusItem{
							{
								Node:     node1Name,
								EgressIP: eipIPv4,
							},
							{
								Node:     node2Name,
								EgressIP: eipIPv6,
							},
						},
					},
				}
				ginkgo.By("start OVN DBs with valid and invalid ( 2 pods don't exist..) OVN config")
				node1NatLogicalPortName := "k8s-" + node1Name
				fakeOvn.nodeName = node1Name
				fakeOvn.startWithDBSetup(
					libovsdbtest.TestSetup{
						NBData: []libovsdbtest.TestData{
							// LRPs to support EIP assigned to a remote node
							// valid LRP for IPv4/IPv6. IPv4 Egress Node is local, IPv6 is remote
							getReRoutePolicy(podV4IP, "4", "valid-reroute-ipv4-UUID",
								nodeLogicalRouterIPv4, getEgressIPLRPReRouteDbIDs(eIP.Name, egressPod.Namespace, egressPod.Name,
									IPFamilyValueV4, types.DefaultNetworkName, types.DefaultNetworkControllerName).GetExternalIDs()),
							getReRoutePolicy(podV6IP, "6", "valid-reroute-ipv6-UUID",
								[]string{node2TranSwitchIPv6}, getEgressIPLRPReRouteDbIDs(eIP.Name, egressPod.Namespace, egressPod.Name,
									IPFamilyValueV6, types.DefaultNetworkName, types.DefaultNetworkControllerName).GetExternalIDs()),
							// invalid lrps to redirect to the remote egress node for deleted pods
							getReRoutePolicy(deletedPodIPv6, "6", "invalid-reroute-ipv6-UUID",
								[]string{node2TranSwitchIPv6}, getEgressIPLRPReRouteDbIDs(eIP.Name, "UNKNOWN", "UNKNOWN",
									IPFamilyValueV6, types.DefaultNetworkName, types.DefaultNetworkControllerName).GetExternalIDs()),
							getReRoutePolicy(deletedPod2IPv6, "6", "invalid-reroute2-ipv6-UUID",
								[]string{node2TranSwitchIPv6}, getEgressIPLRPReRouteDbIDs(eIP.Name, "UNKNOWN", "UNKNOWN",
									IPFamilyValueV6, types.DefaultNetworkName, types.DefaultNetworkControllerName).GetExternalIDs()),
							// NATs to support EIP assigned to the local node
							// valid NAT
							&nbdb.NAT{
								UUID:        "valid-nat-ipv4-UUID",
								LogicalIP:   podV4IP,
								ExternalIP:  eipIPv4,
								ExternalIDs: getEgressIPNATDbIDs(egressIPName, egressPod.Namespace, egressPod.Name, IPFamilyValueV4, types.DefaultNetworkControllerName).GetExternalIDs(),
								Type:        nbdb.NATTypeSNAT,
								LogicalPort: &node1NatLogicalPortName,
								Options: map[string]string{
									"stateless": "false",
								},
							},
							// invalid NATs
							&nbdb.NAT{
								UUID:        "invalid-nat-ipv4-UUID",
								LogicalIP:   deletedPodIPv4,
								ExternalIP:  eipIPv4,
								ExternalIDs: getEgressIPNATDbIDs(egressIPName, "UNKNOWN", "UNKNOWN", IPFamilyValueV4, types.DefaultNetworkControllerName).GetExternalIDs(),
								Type:        nbdb.NATTypeSNAT,
								LogicalPort: &node1NatLogicalPortName,
								Options: map[string]string{
									"stateless": "false",
								},
							},
							&nbdb.NAT{
								UUID:        "invalid-nat2-ipv4-UUID",
								LogicalIP:   deletedPod2IPv4,
								ExternalIP:  eipIPv4,
								ExternalIDs: getEgressIPNATDbIDs(egressIPName, "UNKNOWN", "UNKNOWN", IPFamilyValueV4, types.DefaultNetworkControllerName).GetExternalIDs(),
								Type:        nbdb.NATTypeSNAT,
								LogicalPort: &node1NatLogicalPortName,
								Options: map[string]string{
									"stateless": "false",
								},
							},
							&nbdb.LogicalRouterPort{
								UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1Name + "-UUID",
								Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1Name,
								Networks: []string{nodeLogicalRouterIfAddrV6, nodeLogicalRouterIfAddrV4},
							},
							&nbdb.LogicalRouter{
								Name:     types.OVNClusterRouter,
								UUID:     types.OVNClusterRouter + "-UUID",
								Policies: []string{"valid-reroute-ipv4-UUID", "valid-reroute-ipv6-UUID", "invalid-reroute-ipv6-UUID", "invalid-reroute2-ipv6-UUID"},
							},
							&nbdb.LogicalRouter{
								Name:    types.GWRouterPrefix + node1Name,
								UUID:    types.GWRouterPrefix + node1Name + "-UUID",
								Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
								Options: map[string]string{"dynamic_neigh_routers": "false"},
								Nat:     []string{"valid-nat-ipv4-UUID", "invalid-nat-ipv4-UUID"},
							},
							&nbdb.LogicalSwitchPort{
								UUID: "k8s-" + node1.Name + "-UUID",
								Name: "k8s-" + node1.Name,
								Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1IPV4Net).IP.String(),
									"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1IPV6Net).IP.String()},
							},
							&nbdb.LogicalSwitchPort{
								UUID: "k8s-" + node2.Name + "-UUID",
								Name: "k8s-" + node2.Name,
								Addresses: []string{"fe:1a:b2:3f:0e:fd " + util.GetNodeManagementIfAddr(node2IPV4Net).IP.String(),
									"fe:1a:b2:3f:0e:fd " + util.GetNodeManagementIfAddr(node2IPV6Net).IP.String()},
							},
							&nbdb.LogicalSwitch{
								UUID:  node1.Name + "-UUID",
								Name:  node1.Name,
								Ports: []string{"k8s-" + node1.Name + "-UUID"},
							},
							&nbdb.LogicalSwitch{
								UUID:  node2.Name + "-UUID",
								Name:  node2.Name,
								Ports: []string{"k8s-" + node2.Name + "-UUID"},
							},
						},
					},
					&corev1.NamespaceList{
						Items: []corev1.Namespace{*egressNamespace},
					},
					&corev1.PodList{
						Items: []corev1.Pod{egressPod},
					},
					&corev1.NodeList{
						Items: []corev1.Node{node1, node2},
					},
					&egressipv1.EgressIPList{
						Items: []egressipv1.EgressIP{eIP},
					},
				)
				i, podIPv4Net, _ := net.ParseCIDR(podV4IP + "/23")
				podIPv4Net.IP = i
				i, podIPv6Net, _ := net.ParseCIDR(podV6IP + "/23")
				podIPv6Net.IP = i
				fakeOvn.controller.logicalPortCache.add(&egressPod, "", types.DefaultNetworkName, "", nil, []*net.IPNet{podIPv4Net, podIPv6Net})

				err := fakeOvn.controller.WatchEgressIPPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIPNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				ginkgo.By("ensuring cleanup of invalid LRP and NAT")
				egressIPServedPodsASv4, egressIPServedPodsASv6 := buildEgressIPServedPodsAddressSets([]string{podV4IP, podV6IP}, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName)
				expectedDatabaseState := []libovsdbtest.TestData{
					// LRPs to support EIP assigned to a remote node
					// valid LRP for IPv4/IPv6. IPv4 Egress Node is local, IPv6 is remote
					getReRoutePolicy(podV4IP, "4", "valid-reroute-ipv4-UUID",
						nodeLogicalRouterIPv4, getEgressIPLRPReRouteDbIDs(eIP.Name, egressPod.Namespace, egressPod.Name,
							IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs()),
					getReRoutePolicy(podV6IP, "6", "valid-reroute-ipv6-UUID",
						[]string{node2TranSwitchIPv6}, getEgressIPLRPReRouteDbIDs(eIP.Name, egressPod.Namespace, egressPod.Name,
							IPFamilyValueV6, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs()),
					// valid NAT
					&nbdb.NAT{
						UUID:        "valid-egressip-nat-UUID",
						LogicalIP:   podV4IP,
						ExternalIP:  eipIPv4,
						ExternalIDs: getEgressIPNATDbIDs(egressIPName, egressPod.Namespace, egressPod.Name, IPFamilyValueV4, fakeOvn.controller.controllerName).GetExternalIDs(),
						Type:        nbdb.NATTypeSNAT,
						LogicalPort: &node1NatLogicalPortName,
						Options: map[string]string{
							"stateless": "false",
						},
					},
					&nbdb.LogicalRouter{
						Name:     types.OVNClusterRouter,
						UUID:     types.OVNClusterRouter + "-UUID",
						Policies: []string{"valid-reroute-ipv4-UUID", "valid-reroute-ipv6-UUID"},
					},
					&nbdb.LogicalRouterPort{
						UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1Name + "-UUID",
						Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1Name,
						Networks: []string{nodeLogicalRouterIfAddrV6, nodeLogicalRouterIfAddrV4},
					},
					&nbdb.LogicalRouter{
						Name:    types.GWRouterPrefix + node1Name,
						UUID:    types.GWRouterPrefix + node1Name + "-UUID",
						Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node1Name + "-UUID"},
						Nat:     []string{"valid-egressip-nat-UUID"},
						Options: map[string]string{"dynamic_neigh_routers": "false"},
					},
					&nbdb.LogicalSwitchPort{
						UUID: "k8s-" + node1.Name + "-UUID",
						Name: "k8s-" + node1.Name,
						Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1IPV4Net).IP.String(),
							"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1IPV6Net).IP.String()},
					},
					&nbdb.LogicalSwitchPort{
						UUID: "k8s-" + node2.Name + "-UUID",
						Name: "k8s-" + node2.Name,
						Addresses: []string{"fe:1a:b2:3f:0e:fd " + util.GetNodeManagementIfAddr(node2IPV4Net).IP.String(),
							"fe:1a:b2:3f:0e:fd " + util.GetNodeManagementIfAddr(node2IPV6Net).IP.String()},
					},
					&nbdb.LogicalSwitch{
						UUID:  node1.Name + "-UUID",
						Name:  node1.Name,
						Ports: []string{"k8s-" + node1.Name + "-UUID"},
					},
					&nbdb.LogicalSwitch{
						UUID:  node2.Name + "-UUID",
						Name:  node2.Name,
						Ports: []string{"k8s-" + node2.Name + "-UUID"},
					},
					egressIPServedPodsASv4,
					egressIPServedPodsASv6,
				}
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
				ginkgo.By("ensure config is consistent")
				gomega.Consistently(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})

	ginkgo.Context("on label updates", func() {
		buildEIPReassignmentState := func(egressPod *corev1.Pod, eipName, eIPIP, podIP, nodeIP, nodeName string, withAssignment bool) []libovsdbtest.TestData {
			ginkgo.GinkgoHelper()
			nodeSwitch := &nbdb.LogicalSwitch{
				UUID: nodeName + "-UUID",
				Name: nodeName,
			}
			logicalRouterOptions := map[string]string{
				"dynamic_neigh_routers": "false",
			}
			egressSVCServedPodsASv4, _ := buildEgressServiceAddressSets(nil)
			var egressIPServedpodIPs []string
			if podIP != "" {
				egressIPServedpodIPs = append(egressIPServedpodIPs, podIP)
			}
			egressIPServedPodsASv4, _ := buildEgressIPServedPodsAddressSets(egressIPServedpodIPs, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName)
			egressNodeIPsASv4, _ := buildEgressIPNodeAddressSets([]string{nodeIP})
			nodeSwitch.QOSRules = []string{"default-QoS-UUID"}
			logicalPort := fmt.Sprintf("k8s-%s", nodeName)
			policies := []string{"no-reroute-UUID", "no-reroute-service-UUID", "default-no-reroute-node-UUID", "default-no-reroute-reply-traffic"}
			data := []libovsdbtest.TestData{
				&nbdb.LogicalRouterPolicy{
					Priority: types.DefaultNoRereoutePriority,
					Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
						egressIPServedPodsASv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
					Action:      nbdb.LogicalRouterPolicyActionAllow,
					UUID:        "default-no-reroute-node-UUID",
					Options:     map[string]string{"pkt_mark": types.EgressIPNodeConnectionMark},
					ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
				},
				getNoReRouteReplyTrafficPolicy(types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
				&nbdb.LogicalRouterPolicy{
					Priority:    types.DefaultNoRereoutePriority,
					Match:       "ip4.src == 10.128.0.0/14 && ip4.dst == 10.128.0.0/14",
					Action:      nbdb.LogicalRouterPolicyActionAllow,
					UUID:        "no-reroute-UUID",
					ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
				},
				&nbdb.LogicalRouterPolicy{
					Priority:    types.DefaultNoRereoutePriority,
					Match:       fmt.Sprintf("ip4.src == 10.128.0.0/14 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
					Action:      nbdb.LogicalRouterPolicyActionAllow,
					UUID:        "no-reroute-service-UUID",
					ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
				},
				&nbdb.LogicalRouterPort{
					UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + nodeName + "-UUID",
					Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + nodeName,
					Networks: []string{nodeLogicalRouterIfAddrV4},
				},
				&nbdb.LogicalSwitchPort{
					UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + nodeName + "-UUID",
					Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + nodeName,
					Type: "router",
					Options: map[string]string{
						libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + nodeName,
						"nat-addresses":             "router",
						"exclude-lb-vips-from-garp": "true",
					},
				},
				&nbdb.LogicalSwitch{
					UUID:  types.ExternalSwitchPrefix + nodeName + "-UUID",
					Name:  types.ExternalSwitchPrefix + nodeName,
					Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + nodeName + "-UUID"},
				},
				nodeSwitch,
				getDefaultQoSRule(false, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName),
				egressSVCServedPodsASv4, egressIPServedPodsASv4, egressNodeIPsASv4,
			}
			var nats []string
			if withAssignment && egressPod != nil {
				reroutePolicyUUID := "reroute-UUID1"
				podNatUUID := "pod1-egressip-nat-UUID1"
				policies = append(policies, reroutePolicyUUID)
				nats = append(nats, podNatUUID)
				data = append(data, &nbdb.LogicalRouterPolicy{
					Priority:    types.EgressIPReroutePriority,
					Match:       fmt.Sprintf("ip4.src == %s", egressPod.Status.PodIP),
					Action:      nbdb.LogicalRouterPolicyActionReroute,
					Nexthops:    []string{"100.64.0.2"},
					ExternalIDs: getEgressIPLRPReRouteDbIDs(eipName, egressPod.Namespace, egressPod.Name, IPFamilyValueV4, types.DefaultNetworkName, fakeOvn.controller.eIPC.controllerName).GetExternalIDs(),
					UUID:        reroutePolicyUUID,
				})
				data = append(data, &nbdb.NAT{
					UUID:        podNatUUID,
					LogicalIP:   podIP,
					ExternalIP:  eIPIP,
					ExternalIDs: getEgressIPNATDbIDs(eipName, egressPod.Namespace, egressPod.Name, IPFamilyValueV4, fakeOvn.controller.controllerName).GetExternalIDs(),
					Type:        nbdb.NATTypeSNAT,
					LogicalPort: &logicalPort,
					Options: map[string]string{
						"stateless": "false",
					},
				})
			}
			data = append(data, &nbdb.LogicalRouter{
				Name:     types.OVNClusterRouter,
				UUID:     types.OVNClusterRouter + "-UUID",
				Policies: policies,
			})
			data = append(data, &nbdb.LogicalRouter{
				Name:    types.GWRouterPrefix + nodeName,
				UUID:    types.GWRouterPrefix + nodeName + "-UUID",
				Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + nodeName + "-UUID"},
				Nat:     nats,
				Options: logicalRouterOptions,
			})
			return data
		}

		startDBSetup := func(node corev1.Node, eIP1, eIP2 egressipv1.EgressIP, namespace corev1.Namespace, pod *corev1.Pod) {
			ginkgo.GinkgoHelper()
			node1Switch := &nbdb.LogicalSwitch{
				UUID: node.Name + "-UUID",
				Name: node.Name,
			}
			logicalRouterOptions := map[string]string{
				"dynamic_neigh_routers": "false",
			}
			fakeOvn.startWithDBSetup(
				libovsdbtest.TestSetup{
					NBData: []libovsdbtest.TestData{
						&nbdb.LogicalRouter{
							Name: types.OVNClusterRouter,
							UUID: types.OVNClusterRouter + "-UUID",
						},
						&nbdb.LogicalRouter{
							Name:    types.GWRouterPrefix + node.Name,
							UUID:    types.GWRouterPrefix + node.Name + "-UUID",
							Ports:   []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node.Name + "-UUID"},
							Options: logicalRouterOptions,
						},
						&nbdb.LogicalRouterPort{
							UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node.Name + "-UUID",
							Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node.Name,
							Networks: []string{nodeLogicalRouterIfAddrV4},
						},
						&nbdb.LogicalSwitchPort{
							UUID: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node.Name + "-UUID",
							Name: types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node.Name,
							Type: "router",
							Options: map[string]string{
								libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + "GR_" + node.Name,
								"nat-addresses":             "router",
								"exclude-lb-vips-from-garp": "true",
							},
						},
						&nbdb.LogicalSwitch{
							UUID:  types.ExternalSwitchPrefix + node.Name + "-UUID",
							Name:  types.ExternalSwitchPrefix + node.Name,
							Ports: []string{types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + node.Name + "-UUID"},
						},
						node1Switch,
					},
				},
				&egressipv1.EgressIPList{
					Items: []egressipv1.EgressIP{eIP1, eIP2},
				},
				&corev1.NodeList{
					Items: []corev1.Node{node},
				},
				&corev1.NamespaceList{
					Items: []corev1.Namespace{namespace},
				},
				&corev1.PodList{
					Items: []corev1.Pod{*pod},
				},
			)

			i, n, err := net.ParseCIDR(podV4IP + "/23")
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			n.IP = i
			fakeOvn.controller.logicalPortCache.add(pod, "", types.DefaultNetworkName, "", nil, []*net.IPNet{n})

			err = fakeOvn.controller.WatchEgressIPPods()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = fakeOvn.controller.WatchEgressIPNamespaces()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = fakeOvn.controller.WatchEgressNodes()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = fakeOvn.controller.WatchEgressIP()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}

		getEgressIP := func(name, IP, node string) egressipv1.EgressIP {
			return egressipv1.EgressIP{
				ObjectMeta: newEgressIPMeta(name),
				Spec: egressipv1.EgressIPSpec{
					EgressIPs: []string{IP},
					NamespaceSelector: metav1.LabelSelector{
						MatchLabels: map[string]string{
							"test": "dev1",
						},
					},
				},
				Status: egressipv1.EgressIPStatus{
					Items: []egressipv1.EgressIPStatusItem{
						{
							Node:     node,
							EgressIP: IP,
						},
					},
				},
			}
		}

		ginkgo.It("should handle EIP reassignment correctly on namespace label update", func() {
			app.Action = func(*cli.Context) error {
				config.Gateway.DisableSNATMultipleGWs = true

				egressIP := "192.168.126.101"
				egressIP2 := "192.168.126.102"
				node1IPv4 := "192.168.126.12"
				node1IPv4CIDR := node1IPv4 + "/24"
				node1IPv4TranSwitchIP := "100.88.0.2/16"

				egressPod := ovntest.NewPodWithLabels(eipNamespace, podName, node1Name, podV4IP, egressPodLabel)
				egressNamespace := ovntest.NewNamespace(eipNamespace)
				egressNamespace.Labels["test"] = "dev2"
				nodes := getIPv4Nodes([]nodeInfo{{[]string{node1IPv4CIDR}, node1IPv4TranSwitchIP}})
				node1 := nodes[0]
				node1.Labels = map[string]string{
					"k8s.ovn.org/egress-assignable": "",
				}
				node1.Annotations["k8s.ovn.org/l3-gateway-config"] = `{"default":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"192.168.126.12/24", "next-hop":"192.168.126.1"}}`
				node1.Annotations["k8s.ovn.org/node-chassis-id"] = "79fdcfc4-6fe6-4cd3-8242-c0f85a4668ec"
				eIP1 := getEgressIP(egressIPName, egressIP, node1.Name)
				eIP1.Spec.NamespaceSelector = metav1.LabelSelector{
					MatchLabels: map[string]string{
						"test": "dev1",
					},
				}
				eIP2 := getEgressIP(egressIP2Name, egressIP2, node1.Name)
				eIP2.Spec.NamespaceSelector = metav1.LabelSelector{
					MatchLabels: map[string]string{
						"test": "dev2",
					},
				}
				startDBSetup(node1, eIP1, eIP2, *egressNamespace, egressPod)

				// check egressPod is served by eIP2.
				expectedDatabaseState := buildEIPReassignmentState(egressPod, eIP2.Name, egressIP2, podV4IP, node1IPv4, node1.Name, true)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				ns, err := fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Get(context.TODO(), egressNamespace.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// update namespace label so that eIP1 takes over for serving egressPod.
				ns = ns.DeepCopy()
				ns.Labels["test"] = "dev1"
				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Update(context.TODO(), ns, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectedDatabaseState = buildEIPReassignmentState(egressPod, eIP1.Name, egressIP, podV4IP, node1IPv4, node1.Name, true)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				// Update eIP2 namesapce selector label and check it is configured as standby assignment for egressPod.
				eIP2Latest, err := fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Get(context.TODO(), egressIP2Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				eIP2Latest.Spec.NamespaceSelector.MatchLabels["test"] = "dev1"
				_, err = fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Update(context.TODO(), eIP2Latest, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(func(g gomega.Gomega) {
					pas := getPodAssignmentState(egressPod)
					g.Expect(pas).NotTo(gomega.BeNil())
					g.Expect(pas.egressIPName).To(gomega.Equal(egressIPName))
					g.Expect(pas.standbyEgressIPNames.Len()).To(gomega.Equal(1))
					g.Expect(pas.standbyEgressIPNames.Has(egressIP2Name)).To(gomega.BeTrue())
				}).Should(gomega.Succeed())

				// Delete egressPod now, check pod assignment cache and LRP/SNAT entries are cleaned up.
				err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(egressPod.Namespace).Delete(context.TODO(), egressPod.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(func(g gomega.Gomega) {
					pas := getPodAssignmentState(egressPod)
					g.Expect(pas).To(gomega.BeNil())
				}).Should(gomega.Succeed())

				expectedDatabaseState = buildEIPReassignmentState(nil, "", "", "", node1IPv4, node1.Name, false)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should handle EIP reassignment correctly on pod label update", func() {
			app.Action = func(*cli.Context) error {
				config.Gateway.DisableSNATMultipleGWs = true

				egressIP := "192.168.126.101"
				egressIP2 := "192.168.126.102"
				node1IPv4 := "192.168.126.12"
				node1IPv4CIDR := node1IPv4 + "/24"
				node1IPv4TranSwitchIP := "100.88.0.2/16"

				podLabel := map[string]string{"test": "dev2"}
				egressPod := ovntest.NewPodWithLabels(eipNamespace, podName, node1Name, podV4IP, podLabel)
				egressNamespace := ovntest.NewNamespace(eipNamespace)
				nodes := getIPv4Nodes([]nodeInfo{{[]string{node1IPv4CIDR}, node1IPv4TranSwitchIP}})
				node1 := nodes[0]
				node1.Labels = map[string]string{
					"k8s.ovn.org/egress-assignable": "",
				}
				node1.Annotations["k8s.ovn.org/l3-gateway-config"] = `{"default":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"192.168.126.12/24", "next-hop":"192.168.126.1"}}`
				node1.Annotations["k8s.ovn.org/node-chassis-id"] = "79fdcfc4-6fe6-4cd3-8242-c0f85a4668ec"
				eIP1 := getEgressIP(egressIPName, egressIP, node1.Name)
				eIP1.Spec.NamespaceSelector = metav1.LabelSelector{
					MatchLabels: map[string]string{
						"name": eipNamespace,
					},
				}
				eIP1.Spec.PodSelector = metav1.LabelSelector{
					MatchLabels: map[string]string{
						"test": "dev1",
					},
				}
				eIP2 := getEgressIP(egressIP2Name, egressIP2, node1.Name)
				eIP2.Spec.NamespaceSelector = metav1.LabelSelector{
					MatchLabels: map[string]string{
						"name": eipNamespace,
					},
				}
				eIP2.Spec.PodSelector = metav1.LabelSelector{
					MatchLabels: map[string]string{
						"test": "dev2",
					},
				}
				startDBSetup(node1, eIP1, eIP2, *egressNamespace, egressPod)

				// check egressPod is served by eIP2.
				expectedDatabaseState := buildEIPReassignmentState(egressPod, eIP2.Name, egressIP2, podV4IP, node1IPv4, node1.Name, true)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				pod, err := fakeOvn.fakeClient.KubeClient.CoreV1().Pods(egressNamespace.Name).Get(context.TODO(), egressPod.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// update egressPod label so that eIP1 takes over for serving egressPod.
				pod = pod.DeepCopy()
				pod.Labels["test"] = "dev1"
				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(egressNamespace.Name).Update(context.TODO(), pod, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectedDatabaseState = buildEIPReassignmentState(egressPod, eIP1.Name, egressIP, podV4IP, node1IPv4, node1.Name, true)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				// Update eIP2 namesapce selector label and check it is configured as standby assignment for egressPod.
				eIP2Latest, err := fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Get(context.TODO(), egressIP2Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				eIP2Latest.Spec.PodSelector.MatchLabels["test"] = "dev1"
				_, err = fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Update(context.TODO(), eIP2Latest, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(func(g gomega.Gomega) {
					pas := getPodAssignmentState(egressPod)
					g.Expect(pas).NotTo(gomega.BeNil())
					g.Expect(pas.egressIPName).To(gomega.Equal(egressIPName))
					g.Expect(pas.standbyEgressIPNames.Len()).To(gomega.Equal(1))
					g.Expect(pas.standbyEgressIPNames.Has(egressIP2Name)).To(gomega.BeTrue())
				}).Should(gomega.Succeed())

				// Delete egressPod now, check pod assignment cache and LRP/SNAT entries are cleaned up.
				err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(egressPod.Namespace).Delete(context.TODO(), egressPod.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(func(g gomega.Gomega) {
					pas := getPodAssignmentState(egressPod)
					g.Expect(pas).To(gomega.BeNil())
				}).Should(gomega.Succeed())

				expectedDatabaseState = buildEIPReassignmentState(nil, "", "", "", node1IPv4, node1.Name, false)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should handle EIP reassignment correctly on EgressIP label update", func() {
			app.Action = func(*cli.Context) error {
				config.Gateway.DisableSNATMultipleGWs = true

				egressIP := "192.168.126.101"
				egressIP2 := "192.168.126.102"
				node1IPv4 := "192.168.126.12"
				node1IPv4CIDR := node1IPv4 + "/24"
				node1IPv4TranSwitchIP := "100.88.0.2/16"

				egressPod := ovntest.NewPodWithLabels(eipNamespace, podName, node1Name, podV4IP, egressPodLabel)
				egressNamespace := ovntest.NewNamespace(eipNamespace)
				egressNamespace.Labels["test"] = "dev2"
				nodes := getIPv4Nodes([]nodeInfo{{[]string{node1IPv4CIDR}, node1IPv4TranSwitchIP}})
				node1 := nodes[0]
				node1.Labels = map[string]string{
					"k8s.ovn.org/egress-assignable": "",
				}
				node1.Annotations["k8s.ovn.org/l3-gateway-config"] = `{"default":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"192.168.126.12/24", "next-hop":"192.168.126.1"}}`
				node1.Annotations["k8s.ovn.org/node-chassis-id"] = "79fdcfc4-6fe6-4cd3-8242-c0f85a4668ec"
				eIP1 := getEgressIP(egressIPName, egressIP, node1.Name)
				eIP1.Spec.NamespaceSelector = metav1.LabelSelector{
					MatchLabels: map[string]string{
						"test": "dev1",
					},
				}
				eIP2 := getEgressIP(egressIP2Name, egressIP2, node1.Name)
				eIP2.Spec.NamespaceSelector = metav1.LabelSelector{
					MatchLabels: map[string]string{
						"test": "dev2",
					},
				}
				startDBSetup(node1, eIP1, eIP2, *egressNamespace, egressPod)

				// check egressPod is served by eIP2.
				expectedDatabaseState := buildEIPReassignmentState(egressPod, eIP2.Name, egressIP2, podV4IP, node1IPv4, node1.Name, true)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				// Update eIP and eIP2 namespace selector labels, This makes EIP assignment to move from eIP2 to eIP for egressPod.
				eIPLatest, err := fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Get(context.TODO(), egressIPName, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				eIPLatest.Spec.NamespaceSelector.MatchLabels["test"] = "dev2"
				_, err = fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Update(context.TODO(), eIPLatest, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				eIP2Latest, err := fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Get(context.TODO(), egressIP2Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				eIP2Latest.Spec.NamespaceSelector.MatchLabels["test"] = "dev1"
				_, err = fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Update(context.TODO(), eIP2Latest, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectedDatabaseState = buildEIPReassignmentState(egressPod, eIP1.Name, egressIP, podV4IP, node1IPv4, node1.Name, true)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				// Update eIP2 namesapce selector label and check it is configured as standby assignment for egressPod.
				eIP2Latest, err = fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Get(context.TODO(), egressIP2Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				eIP2Latest.Spec.NamespaceSelector.MatchLabels["test"] = "dev2"
				_, err = fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Update(context.TODO(), eIP2Latest, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(func(g gomega.Gomega) {
					pas := getPodAssignmentState(egressPod)
					g.Expect(pas).NotTo(gomega.BeNil())
					g.Expect(pas.egressIPName).To(gomega.Equal(egressIPName))
					g.Expect(pas.standbyEgressIPNames.Len()).To(gomega.Equal(1))
					g.Expect(pas.standbyEgressIPNames.Has(egressIP2Name)).To(gomega.BeTrue())
				}).Should(gomega.Succeed())

				// Delete egressPod now, check pod assignment cache and LRP/SNAT entries are cleaned up.
				err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(egressPod.Namespace).Delete(context.TODO(), egressPod.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(func(g gomega.Gomega) {
					pas := getPodAssignmentState(egressPod)
					g.Expect(pas).To(gomega.BeNil())
				}).Should(gomega.Succeed())

				expectedDatabaseState = buildEIPReassignmentState(nil, "", "", "", node1IPv4, node1.Name, false)
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

func getDefaultQoSRule(isv6 bool, network, controller string) *nbdb.QoS {
	egressipPodsV4, egressipPodsV6 := addressset.GetHashNamesForAS(getEgressIPAddrSetDbIDs(EgressIPServedPodsAddrSetName, network, controller))
	qos := &nbdb.QoS{
		Priority:    types.EgressIPRerouteQoSRulePriority,
		Action:      map[string]int{"mark": types.EgressIPReplyTrafficConnectionMark},
		ExternalIDs: getEgressIPQoSRuleDbIDs(IPFamilyValueV4, network, controller).GetExternalIDs(),
		Direction:   nbdb.QoSDirectionFromLport,
		UUID:        fmt.Sprintf("%s-QoS-UUID", network),
		Match:       fmt.Sprintf(`ip4.src == $%s && ct.trk && ct.rpl`, egressipPodsV4),
	}
	if isv6 {
		qos.UUID = fmt.Sprintf("%s-QoSv6-UUID", network)
		qos.Match = fmt.Sprintf(`ip6.src == $%s && ct.trk && ct.rpl`, egressipPodsV6)
		qos.ExternalIDs = getEgressIPQoSRuleDbIDs(IPFamilyValueV6, network, controller).GetExternalIDs()
	}
	return qos
}

func getEIPSNAT(podIP, podNamespace, podName, egressIP, expectedNatLogicalPort, controllerName string) *nbdb.NAT {
	ipFamily := IPFamilyValueV4
	if utilnet.IsIPv6String(podIP) {
		ipFamily = IPFamilyValueV6
	}
	return &nbdb.NAT{
		UUID:        "egressip-nat-UUID",
		LogicalIP:   podIP,
		ExternalIP:  egressIP,
		ExternalIDs: getEgressIPNATDbIDs(egressIPName, podNamespace, podName, ipFamily, controllerName).GetExternalIDs(),
		Type:        nbdb.NATTypeSNAT,
		LogicalPort: &expectedNatLogicalPort,
		Options: map[string]string{
			"stateless": "false",
		},
	}
}

func getNoReRouteReplyTrafficPolicy(network, controller string) *nbdb.LogicalRouterPolicy {
	return &nbdb.LogicalRouterPolicy{
		Priority:    types.DefaultNoRereoutePriority,
		Match:       fmt.Sprintf("pkt.mark == %d", types.EgressIPReplyTrafficConnectionMark),
		Action:      nbdb.LogicalRouterPolicyActionAllow,
		ExternalIDs: getEgressIPLRPNoReRouteDbIDs(types.DefaultNoRereoutePriority, ReplyTrafficNoReroute, IPFamilyValue, network, controller).GetExternalIDs(),
		UUID:        fmt.Sprintf("%s-no-reroute-reply-traffic", network),
	}
}

func getReRoutePolicy(podIP, ipFamily, uuid string, nextHops []string, externalID map[string]string) *nbdb.LogicalRouterPolicy {
	return &nbdb.LogicalRouterPolicy{
		Priority:    types.EgressIPReroutePriority,
		Match:       fmt.Sprintf("ip%s.src == %s", ipFamily, podIP),
		Action:      nbdb.LogicalRouterPolicyActionReroute,
		Nexthops:    nextHops,
		ExternalIDs: externalID,
		UUID:        uuid,
	}
}

func getReRoutePolicySecondaryHost(podIP, ipFamily, uuid string, nextHops []string, externalID map[string]string) *nbdb.LogicalRouterPolicy {
	return &nbdb.LogicalRouterPolicy{
		Priority:    types.EgressIPReroutePriority,
		Match:       fmt.Sprintf("ip%s.src == %s", ipFamily, podIP),
		Action:      nbdb.LogicalRouterPolicyActionReroute,
		Nexthops:    nextHops,
		Options:     map[string]string{"pkt_mark": types.EgressIPSecondaryInterfaceMark},
		ExternalIDs: externalID,
		UUID:        uuid,
	}
}

func getReRouteStaticRoute(clusterSubnet, nextHop string) *nbdb.LogicalRouterStaticRoute {
	return &nbdb.LogicalRouterStaticRoute{
		Nexthop:  nextHop,
		Policy:   &nbdb.LogicalRouterStaticRoutePolicySrcIP,
		IPPrefix: clusterSubnet,
		UUID:     "reroute-static-route-UUID",
	}
}

func getNodeObj(nodeName string, annotations, labels map[string]string) corev1.Node {
	nodeAnnotations := map[string]string{}
	if annotations != nil {
		nodeAnnotations = make(map[string]string, len(annotations)+1)
		for k, v := range annotations {
			nodeAnnotations[k] = v
		}
	}
	if _, ok := nodeAnnotations[util.OvnNodeChassisID]; !ok {
		nodeAnnotations[util.OvnNodeChassisID] = uuid.NewSHA1(uuid.NameSpaceOID, []byte(nodeName)).String()
	}
	node := corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        nodeName,
			Annotations: nodeAnnotations,
			Labels:      labels,
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
	if _, ok := nodeAnnotations[util.OVNNodeEncapIPs]; !ok {
		if primaryIfAddr, err := util.GetNodeIfAddrAnnotation(&node); err == nil {
			encapIPs := []string{}
			for _, cidr := range []string{primaryIfAddr.IPv4, primaryIfAddr.IPv6} {
				if cidr == "" {
					continue
				}
				ip, _, err := net.ParseCIDR(cidr)
				if err == nil {
					encapIPs = append(encapIPs, ip.String())
				}
			}
			if len(encapIPs) > 0 {
				encapAnnotation, err := json.Marshal(encapIPs)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				nodeAnnotations[util.OVNNodeEncapIPs] = string(encapAnnotation)
			}
		}
	}
	return node
}

func getSwitchManagementPortIP(node *corev1.Node) (net.IP, error) {
	// fetch node annotation of the egress node
	network := "default"
	ipNets, err := util.ParseNodeHostSubnetAnnotation(node, network)
	if err != nil {
		return nil, fmt.Errorf("failed to parse node (%s) subnets to get management port IP: %v", node.Name, err)
	}
	for _, ipNet := range ipNets {
		return util.GetNodeManagementIfAddr(ipNet).IP, nil
	}
	return nil, fmt.Errorf("failed to find management port IP for node %s", node.Name)
}

// returns the address set with externalID "k8s.ovn.org/name": "egresssvc-served-pods"
func buildEgressServiceAddressSets(ips []string) (*nbdb.AddressSet, *nbdb.AddressSet) {
	dbIDs := egresssvc.GetEgressServiceAddrSetDbIDs(types.DefaultNetworkControllerName)
	return addressset.GetTestDbAddrSets(dbIDs, ips)
}

// returns the address set with externalID "k8s.ovn.org/name": "egressip-served-pods""
func buildEgressIPServedPodsAddressSets(ips []string, network, controller string) (*nbdb.AddressSet, *nbdb.AddressSet) {
	dbIDs := getEgressIPAddrSetDbIDs(EgressIPServedPodsAddrSetName, network, controller)
	return addressset.GetTestDbAddrSets(dbIDs, ips)

}

// returns the address set with externalID "k8s.ovn.org/name": "node-ips"
func buildEgressIPNodeAddressSets(ips []string) (*nbdb.AddressSet, *nbdb.AddressSet) {
	return addressset.GetTestDbAddrSets(getClusterNodeIPsAddrSetDbIDsForTest(), ips)
}

func getDeprecatedEgressIPClusterNodeIPsAddrSetDbIDsForNetwork(network string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.AddressSetEgressIP, types.DefaultNetworkControllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: addresssetmanager.ClusterNodeIPsAddrSetName,
			libovsdbops.NetworkKey:    network,
		})
}

func getClusterNodeIPsAddrSetDbIDsForTest() *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.AddressSetClusterNodeIPs, types.DefaultNetworkControllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: addresssetmanager.ClusterNodeIPsAddrSetName,
		})
}
