// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"context"
	"fmt"
	"net"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	nadv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/urfave/cli/v2"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ovncnitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	egressipv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/generator/udn"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/controller/udnenabledsvc"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

var _ = ginkgo.DescribeTable("Layer2 EgressIP gateway next hop uses the GR transit IP regardless of topology annotation",
	func(isIPv6, annotated bool, expectedTransitIP, expectedJoinIP string) {
		gomega.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
		ginkgo.DeferCleanup(func() {
			gomega.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
		})
		config.IPv4Mode, config.IPv6Mode = true, true
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		config.Layer2UsesTransitRouter = true
		netInfo, err := util.NewNetInfo(&ovncnitypes.NetConf{
			NetConf:       cnitypes.NetConf{Name: "network1"},
			Role:          ovntypes.NetworkRolePrimary,
			Topology:      ovntypes.Layer2Topology,
			Subnets:       "10.128.0.0/16,fd10::/64",
			JoinSubnet:    "100.65.0.0/16,fd98::/64",
			TransitSubnet: "100.88.0.0/16,fd97::/64",
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		node := getNodeObj("node1", map[string]string{util.OvnNodeID: "1"}, nil)
		if annotated {
			node.Annotations[util.Layer2TopologyVersion] = util.TransitRouterTopoVersion
		}

		joinIPs, err := udn.GetGWRouterIPs(&node, netInfo)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		joinIP, err := util.MatchFirstIPNetFamily(isIPv6, joinIPs)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(joinIP.IP.String()).To(gomega.Equal(expectedJoinIP))

		controller := &EgressIPController{}
		nextHop, err := controller.getGatewayNextHop(netInfo, &node, isIPv6)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(nextHop.String()).To(gomega.Equal(expectedTransitIP))
		gomega.Expect(nextHop.Equal(joinIP.IP)).To(gomega.BeFalse(), "must not fall back to the legacy GR join IP")
	},
	ginkgo.Entry("IPv4 without annotation", false, false, "100.88.0.3", "100.65.0.1"),
	ginkgo.Entry("IPv6 without annotation", true, false, "fd97::3", "fd98::1"),
	ginkgo.Entry("IPv4 with annotation", false, true, "100.88.0.3", "100.65.0.1"),
	ginkgo.Entry("IPv6 with annotation", true, true, "fd97::3", "fd98::1"),
)

var _ = ginkgo.Describe("EgressIP Operations for user defined network with topology L2", func() {
	var (
		app     *cli.App
		fakeOvn *FakeOVN
	)

	const (
		nadName1                 = "nad1"
		networkName1             = "network1"
		networkName1_            = networkName1 + "_"
		node1Name                = "node1"
		v4Net1                   = "20.128.0.0/14"
		v4Node1Net1              = "20.128.0.0/16"
		v4Pod1IPNode1Net1        = "20.128.0.5"
		node1DefaultRtoJIP       = "100.64.0.1"
		node1DefaultRtoJIPCIDR   = node1DefaultRtoJIP + "/16"
		node1Network1JoinIP      = "100.65.0.1"
		node1Network1JoinCIDR    = node1Network1JoinIP + "/16"
		node1Network1TransitIP   = "100.88.0.3"
		node1Network1TransitCIDR = node1Network1TransitIP + "/31"
		podName3                 = "egress-pod3"
		v4Node1Tsp               = "100.88.0.2"
		node2Name                = "node2"
		v4Node2Net1              = "20.129.0.0/16"
		v4Node2Tsp               = "100.88.0.3"
		podName4                 = "egress-pod4"
		v4Pod2IPNode2Net1        = "20.129.0.3"
		node2Network1JoinIP      = "100.65.0.2"
		node2Network1TransitIP   = "100.88.0.5"
		eIP1Mark                 = 50000
		layer2SwitchName         = "ovn_layer2_switch"
		gwIP                     = "192.168.126.1"
		userDefinedNetworkID     = "2"
	)

	getEgressIPStatusLen := func(egressIPName string) func() int {
		return func() int {
			tmp, err := fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Get(context.TODO(), egressIPName, metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			return len(tmp.Status.Items)
		}
	}

	getIPNetWithIP := func(cidr string) *net.IPNet {
		ip, ipNet, err := net.ParseCIDR(cidr)
		if err != nil {
			panic(err.Error())
		}
		ipNet.IP = ip
		return ipNet
	}

	setPrimaryNetworkAnnot := func(pod *corev1.Pod, nadName, cidr string) {
		var err error
		hwAddr, _ := net.ParseMAC("00:00:5e:00:53:01")
		pod.Annotations, err = util.MarshalPodAnnotation(pod.Annotations,
			&util.PodAnnotation{
				IPs:  []*net.IPNet{getIPNetWithIP(cidr)},
				MAC:  hwAddr,
				Role: "primary",
			},
			nadName)
		if err != nil {
			panic(err.Error())
		}
	}

	ginkgo.BeforeEach(func() {
		// Restore global default values before each testcase
		gomega.Expect(config.PrepareTestConfig()).Should(gomega.Succeed())
		config.OVNKubernetesFeature.EnableEgressIP = true
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		config.Gateway.Mode = config.GatewayModeShared
		config.OVNKubernetesFeature.EgressIPNodeHealthCheckPort = 1234

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

		fakeOvn = NewFakeOVN(false, node1Name)
	})

	ginkgo.AfterEach(func() {
		fakeOvn.shutdown()
		// Restore global default values
		gomega.Expect(config.PrepareTestConfig()).Should(gomega.Succeed())
	})

	ginkgo.Context("sync", func() {
		ginkgo.It("should remove stale LRPs and configures missing LRPs", func() {
			app.Action = func(ctx *cli.Context) error {
				// Node 1 is local, Node 2 is remote
				egressIP1 := "192.168.126.101"
				egressIP2 := "192.168.126.102"
				node1IPv4 := "192.168.126.202"
				node1IPv4CIDR := node1IPv4 + "/24"
				node2IPv4 := "192.168.126.51"
				node2IPv4CIDR := node2IPv4 + "/24"
				_, node1CDNSubnet, _ := net.ParseCIDR(v4Node1Subnet)
				_, node1UDNSubnet, _ := net.ParseCIDR(v4Node1Net1)
				nadName := util.GetNADName(eipNamespace2, nadName1)
				egressCDNNamespace := testing.NewNamespaceWithLabels(eipNamespace, egressPodLabel)
				egressUDNNamespace := newUDNNamespaceWithLabels(eipNamespace2, egressPodLabel)
				egressPodCDNLocal := *testing.NewPodWithLabels(eipNamespace, podName, node1Name, podV4IP, egressPodLabel)
				egressPodUDNLocal := *testing.NewPodWithLabels(eipNamespace2, podName2, node1Name, v4Pod1IPNode1Net1, egressPodLabel)
				setPrimaryNetworkAnnot(&egressPodUDNLocal, nadName, fmt.Sprintf("%s%s", v4Pod1IPNode1Net1, util.GetIPFullMaskString(v4Pod1IPNode1Net1)))
				egressPodCDNRemote := *testing.NewPodWithLabels(eipNamespace, podName3, node2Name, podV4IP2, egressPodLabel)
				setPrimaryNetworkAnnot(&egressPodCDNRemote, ovntypes.DefaultNetworkName, fmt.Sprintf("%s%s", podV4IP2, util.GetIPFullMaskString(podV4IP2)))
				egressPodUDNRemote := *testing.NewPodWithLabels(eipNamespace2, podName4, node2Name, v4Pod2IPNode2Net1, egressPodLabel)
				setPrimaryNetworkAnnot(&egressPodUDNRemote, nadName, fmt.Sprintf("%s%s", v4Pod2IPNode2Net1, util.GetIPFullMaskString(v4Pod2IPNode2Net1)))
				netconf := ovncnitypes.NetConf{
					NetConf: cnitypes.NetConf{
						Name: networkName1,
						Type: "ovn-k8s-cni-overlay",
					},
					Role:          ovntypes.NetworkRolePrimary,
					Topology:      ovntypes.Layer2Topology,
					NADName:       nadName,
					Subnets:       v4Net1,
					TransitSubnet: config.ClusterManager.V4TransitSubnet,
				}
				nad, err := newNetworkAttachmentDefinition(
					eipNamespace2,
					nadName1,
					netconf,
				)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				netInfo, err := util.NewNetInfo(&netconf)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				node1Annotations := map[string]string{
					"k8s.ovn.org/node-primary-ifaddr":             fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4CIDR, ""),
					"k8s.ovn.org/node-subnets":                    fmt.Sprintf("{\"default\":\"%s\",\"%s\":\"%s\"}", v4Node1Subnet, networkName1, v4Node1Net1),
					"k8s.ovn.org/node-transit-switch-port-ifaddr": fmt.Sprintf("{\"ipv4\":\"%s/16\"}", v4Node1Tsp),
					"k8s.ovn.org/node-chassis-id":                 "473ca66d-d800-472f-b289-1ab81ae7f21c",
					util.OvnNodeID:                                "1",
					util.OVNNodeHostCIDRs:                         fmt.Sprintf("[\"%s\"]", node1IPv4CIDR),
					util.OvnNodeL3GatewayConfig: fmt.Sprintf(`{"%s":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"%s", "next-hop":"%s", "next-hops": ["%s"]},
"default":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"192.168.126.12/24", "next-hop": "192.168.126.1", "next-hops": ["192.168.126.1"]}}`, networkName1, v4Net1, gwIP, gwIP),
					util.Layer2TopologyVersion: util.TransitRouterTopoVersion,
				}
				labels := map[string]string{
					"k8s.ovn.org/egress-assignable": "",
				}
				node1 := getNodeObj(node1Name, node1Annotations, labels)
				node2Annotations := map[string]string{
					"k8s.ovn.org/node-primary-ifaddr":             fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node2IPv4CIDR, ""),
					"k8s.ovn.org/node-subnets":                    fmt.Sprintf("{\"default\":\"%s\",\"%s\":\"%s\"}", v4Node2Subnet, networkName1, v4Node2Net1),
					"k8s.ovn.org/node-transit-switch-port-ifaddr": fmt.Sprintf("{\"ipv4\":\"%s/16\"}", v4Node2Tsp),
					"k8s.ovn.org/node-chassis-id":                 "473ca66d-d800-472f-b289-1ab81ae7f21c",
					util.OvnNodeID:                                "2",
					util.OVNNodeHostCIDRs:                         fmt.Sprintf("[\"%s\"]", node2IPv4CIDR),
					util.OvnNodeL3GatewayConfig: fmt.Sprintf(`{"%s":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"%s", "next-hop":"%s", "next-hops": ["%s"]},
"default":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"192.168.126.12/24", "next-hop":"192.168.126.1", "next-hops": ["192.168.126.1"]}`, networkName1, v4Net1, gwIP, gwIP),
					util.Layer2TopologyVersion: util.TransitRouterTopoVersion,
				}
				node2 := getNodeObj(node2Name, node2Annotations, labels)
				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMetaWithMark(egressIPName, eIP1Mark),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{egressIP1, egressIP2},
						PodSelector: metav1.LabelSelector{
							MatchLabels: egressPodLabel,
						},
						NamespaceSelector: metav1.LabelSelector{
							MatchLabels: egressPodLabel,
						},
					},
					Status: egressipv1.EgressIPStatus{
						Items: []egressipv1.EgressIPStatusItem{
							{
								Node:     node1Name,
								EgressIP: egressIP1,
							},
							{
								Node:     node2Name,
								EgressIP: egressIP2,
							},
						},
					},
				}
				initialDB := []libovsdbtest.TestData{
					//CDN start
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name,
						Networks: []string{node1DefaultRtoJIPCIDR},
					},
					&nbdb.LogicalRouter{
						Name: ovntypes.OVNClusterRouter,
						UUID: ovntypes.OVNClusterRouter + "-UUID",
					},
					&nbdb.LogicalRouter{
						Name:  ovntypes.GWRouterPrefix + node1.Name,
						UUID:  ovntypes.GWRouterPrefix + node1.Name + "-UUID",
						Ports: []string{ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name + "-UUID"},
					},
					&nbdb.LogicalSwitchPort{
						UUID:      "k8s-" + node1Name + "-UUID",
						Name:      "k8s-" + node1Name,
						Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1CDNSubnet).IP.String()},
					},
					&nbdb.LogicalSwitch{
						UUID:  node1Name + "-UUID",
						Name:  node1Name,
						Ports: []string{"k8s-" + node1Name + "-UUID"},
					},
					// UDN start
					getReRoutePolicyForController(egressIPName, eipNamespace2, podName2, v4Pod1IPNode1Net1, eIP1Mark, IPFamilyValueV4, []string{node1Network1JoinCIDR, node2Network1JoinIP}, netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName), // stale gateway
					getReRoutePolicyForController(egressIPName, eipNamespace2, podName, v4Pod1IPNode1Net1, eIP1Mark, IPFamilyValueV4, []string{node1Network1JoinCIDR, node2Network1JoinIP}, netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName),  // stale pod
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.RouterToTransitRouterPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name + "-UUID",
						Name:     ovntypes.RouterToTransitRouterPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name,
						Networks: []string{node1Network1JoinCIDR, node1Network1TransitCIDR},
					},
					&nbdb.LogicalRouter{
						Name:        netInfo.GetNetworkScopedClusterRouterName(),
						UUID:        netInfo.GetNetworkScopedClusterRouterName() + "-UUID",
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: networkName1, ovntypes.TopologyExternalID: ovntypes.Layer2Topology},
						Policies: []string{getReRoutePolicyUUID(eipNamespace2, podName2, IPFamilyValueV4, netInfo.GetNetworkName()),
							getReRoutePolicyUUID(eipNamespace2, podName, IPFamilyValueV4, netInfo.GetNetworkName())}, // stale policies
					},
					&nbdb.LogicalRouter{
						UUID:        netInfo.GetNetworkScopedGWRouterName(node1.Name) + "-UUID",
						Name:        netInfo.GetNetworkScopedGWRouterName(node1.Name),
						Ports:       []string{ovntypes.RouterToTransitRouterPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name + "-UUID"},
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: networkName1, ovntypes.TopologyExternalID: ovntypes.Layer2Topology},
					},
					&nbdb.LogicalSwitchPort{
						UUID:      "k8s-" + networkName1_ + node1Name + "-UUID",
						Name:      "k8s-" + networkName1_ + node1Name,
						Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1UDNSubnet).IP.String()},
					},
					&nbdb.LogicalSwitch{
						UUID:        networkName1_ + layer2SwitchName + "-UUID",
						Name:        networkName1_ + layer2SwitchName,
						Ports:       []string{"k8s-" + networkName1_ + node1Name + "-UUID"},
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: networkName1, ovntypes.TopologyExternalID: ovntypes.Layer2Topology},
						QOSRules:    []string{},
					},
				}
				fakeOvn.startWithDBSetup(
					libovsdbtest.TestSetup{
						NBData: initialDB,
					},
					&corev1.NodeList{
						Items: []corev1.Node{node1, node2},
					},
					&corev1.NamespaceList{
						Items: []corev1.Namespace{*egressCDNNamespace, *egressUDNNamespace},
					},
					&corev1.PodList{
						Items: []corev1.Pod{egressPodCDNLocal, egressPodUDNLocal, egressPodCDNRemote, egressPodUDNRemote},
					},
					&nadv1.NetworkAttachmentDefinitionList{
						Items: []nadv1.NetworkAttachmentDefinition{*nad},
					},
					&egressipv1.EgressIPList{
						Items: []egressipv1.EgressIP{eIP},
					},
				)
				asf := addressset.NewOvnAddressSetFactory(fakeOvn.nbClient, true, false)
				// watch EgressIP depends on UDN enabled svcs address set being available
				c := udnenabledsvc.NewController(fakeOvn.nbClient, asf, fakeOvn.controller.watchFactory.ServiceCoreInformer(), []string{})
				go func() {
					gomega.Expect(c.Run(ctx.Done())).Should(gomega.Succeed())
				}()
				// Add pod IPs to CDN cache
				iCDN, nCDN, _ := net.ParseCIDR(podV4IP + "/23")
				nCDN.IP = iCDN
				fakeOvn.controller.logicalPortCache.add(&egressPodCDNLocal, "", ovntypes.DefaultNetworkName, "", nil, []*net.IPNet{nCDN})
				// Add pod IPs to UDN cache
				iUDN, nUDN, _ := net.ParseCIDR(v4Pod1IPNode1Net1 + "/23")
				nUDN.IP = iUDN
				fakeOvn.controller.logicalPortCache.add(&egressPodUDNLocal, "", util.GetNADName(nad.Namespace, nad.Name), "", nil, []*net.IPNet{nUDN})
				fakeOvn.controller.eIPC.nodeName = node1.Name
				fakeOvn.controller.nodeName = node1.Name
				gomega.Expect(fakeOvn.eIPController.SyncNodeCache()).To(gomega.Succeed())
				err = fakeOvn.eIPController.ensureRouterPoliciesForNetwork(netInfo, &node1)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.eIPController.ensureSwitchPoliciesForNode(netInfo, node1Name)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(fakeOvn.networkManager.Start()).Should(gomega.Succeed())
				// Mirror UDN controller Start: register network after pods are in the port cache.
				fakeOvn.eIPController.ensureNetworkPending(netInfo.GetNetworkName())
				gomega.Expect(fakeOvn.eIPController.SyncNetworkReady(netInfo.GetNetworkName())).To(gomega.Succeed())
				err = fakeOvn.controller.WatchEgressIPPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIPNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressNodes()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				egressIPServedPodsASCDNv4, _ := buildEgressIPServedPodsAddressSets([]string{podV4IP}, ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkControllerName)
				egressNodeIPsASv4, _ := buildEgressIPNodeAddressSets([]string{node1IPv4, node2IPv4})
				egressSVCServedPodsASv4, _ := buildEgressServiceAddressSets(nil)
				egressIPServedPodsASUDNv4, _ := buildEgressIPServedPodsAddressSetsForController([]string{v4Pod1IPNode1Net1}, netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName)
				gomega.Eventually(c.IsAddressSetAvailable).Should(gomega.BeTrue())
				dbIDs := udnenabledsvc.GetAddressSetDBIDs()
				udnEnabledSvcV4, _ := addressset.GetTestDbAddrSets(dbIDs, []string{})

				node1LRP := "k8s-node1"
				expectedDatabaseStateTwoEgressNodes := []libovsdbtest.TestData{
					// CDN
					getReRouteStaticRoute(v4ClusterSubnet, node1DefaultRtoJIP),
					getReRoutePolicy(podV4IP, "4", "reroute-UUID", []string{node1DefaultRtoJIP, v4Node2Tsp},
						getEgressIPLRPReRouteDbIDs(eIP.Name, egressPodCDNLocal.Namespace, egressPodCDNLocal.Name, IPFamilyValueV4, ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkControllerName).GetExternalIDs()),
					&nbdb.LogicalRouterPolicy{
						Priority:    ovntypes.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4ClusterSubnet, v4ClusterSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-no-reroute-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    ovntypes.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4ClusterSubnet, config.Gateway.V4JoinSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-service-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouter{
						Name:  ovntypes.GWRouterPrefix + node1.Name,
						UUID:  ovntypes.GWRouterPrefix + node1.Name + "-UUID",
						Ports: []string{ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name + "-UUID"},
						Nat:   []string{"egressip-nat-UUID", "egressip-nat2-UUID"},
					},
					&nbdb.LogicalRouter{
						Name: ovntypes.OVNClusterRouter,
						UUID: ovntypes.OVNClusterRouter + "-UUID",
						Policies: []string{"default-no-reroute-UUID", "no-reroute-service-UUID",
							"default-no-reroute-node-UUID", "default-no-reroute-reply-traffic", "reroute-UUID"},
						StaticRoutes: []string{"reroute-static-route-UUID"},
					},
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name,
						Networks: []string{node1DefaultRtoJIPCIDR},
					},
					&nbdb.LogicalRouterPolicy{
						Priority: ovntypes.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
							egressIPServedPodsASCDNv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": ovntypes.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalSwitchPort{
						UUID:      "k8s-" + node1Name + "-UUID",
						Name:      "k8s-" + node1Name,
						Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1CDNSubnet).IP.String()},
					},
					&nbdb.LogicalSwitch{
						UUID:     node1Name + "-UUID",
						Name:     node1Name,
						Ports:    []string{"k8s-" + node1Name + "-UUID"},
						QOSRules: []string{"default-QoS-UUID"},
					},
					&nbdb.NAT{
						UUID:        "egressip-nat-UUID",
						LogicalIP:   podV4IP2,
						ExternalIP:  egressIP1,
						ExternalIDs: getEgressIPNATDbIDs(egressIPName, egressPodCDNRemote.Namespace, egressPodCDNRemote.Name, IPFamilyValueV4, ovntypes.DefaultNetworkControllerName).GetExternalIDs(),
						Type:        nbdb.NATTypeSNAT,
						LogicalPort: &node1LRP,
						Options: map[string]string{
							"stateless": "false",
						},
					},
					&nbdb.NAT{
						UUID:        "egressip-nat2-UUID",
						LogicalIP:   podV4IP,
						ExternalIP:  egressIP1,
						ExternalIDs: getEgressIPNATDbIDs(egressIPName, egressPodCDNLocal.Namespace, egressPodCDNLocal.Name, IPFamilyValueV4, ovntypes.DefaultNetworkControllerName).GetExternalIDs(),
						Type:        nbdb.NATTypeSNAT,
						LogicalPort: &node1LRP,
						Options: map[string]string{
							"stateless": "false",
						},
					},
					getNoReRouteReplyTrafficPolicy(ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkControllerName),
					getDefaultQoSRule(false, ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkControllerName),
					egressSVCServedPodsASv4,
					egressIPServedPodsASCDNv4,
					egressNodeIPsASv4,

					// UDN
					getReRoutePolicyForController(egressIPName, eipNamespace2, podName2, v4Pod1IPNode1Net1, eIP1Mark, IPFamilyValueV4, []string{node1Network1TransitIP, node2Network1TransitIP}, netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName),
					getReRoutePolicyForController(egressIPName, eipNamespace2, podName4, v4Pod2IPNode2Net1, eIP1Mark, IPFamilyValueV4, []string{node1Network1TransitIP}, netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName),
					getGWPktMarkLRPForController(eIP1Mark, egressIPName, eipNamespace2, podName2, v4Pod1IPNode1Net1, IPFamilyValueV4,
						netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName),
					getGWPktMarkLRPForController(eIP1Mark, egressIPName, eipNamespace2, podName4, v4Pod2IPNode2Net1, IPFamilyValueV4,
						netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName),
					getNoReRoutePolicyForUDNEnabledSvc(false, netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName, egressIPServedPodsASUDNv4.Name, egressSVCServedPodsASv4.Name, udnEnabledSvcV4.Name),
					&nbdb.LogicalRouterPolicy{
						Priority:    ovntypes.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4Net1, v4Net1),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "udn-default-no-reroute-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    ovntypes.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4Net1, config.Gateway.V4JoinSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "udn-no-reroute-service-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority: ovntypes.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
							egressIPServedPodsASUDNv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "udn-default-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": ovntypes.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.RouterToTransitRouterPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name + "-UUID",
						Name:     ovntypes.RouterToTransitRouterPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name,
						Networks: []string{node1Network1JoinCIDR, node1Network1TransitCIDR},
					},
					&nbdb.LogicalRouter{
						Name:        netInfo.GetNetworkScopedClusterRouterName(),
						UUID:        netInfo.GetNetworkScopedClusterRouterName() + "-UUID",
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: netInfo.GetNetworkName(), ovntypes.TopologyExternalID: ovntypes.Layer2Topology},
						Policies: []string{
							"udn-default-no-reroute-node-UUID", "udn-default-no-reroute-UUID", "udn-no-reroute-service-UUID", "udn-enabled-svc-no-reroute-UUID",
							fmt.Sprintf("%s-no-reroute-reply-traffic", netInfo.GetNetworkName()),
							getReRoutePolicyUUID(eipNamespace2, podName2, IPFamilyValueV4, netInfo.GetNetworkName()),
							getReRoutePolicyUUID(eipNamespace2, podName4, IPFamilyValueV4, netInfo.GetNetworkName())},
					},
					&nbdb.LogicalRouter{
						UUID:        netInfo.GetNetworkScopedGWRouterName(node1.Name) + "-UUID",
						Name:        netInfo.GetNetworkScopedGWRouterName(node1.Name),
						Ports:       []string{ovntypes.RouterToTransitRouterPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name + "-UUID"},
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: netInfo.GetNetworkName(), ovntypes.TopologyExternalID: ovntypes.Layer2Topology},
						Policies: []string{getGWPktMarkLRPUUID(eipNamespace2, podName2, IPFamilyValueV4, netInfo.GetNetworkName()),
							getGWPktMarkLRPUUID(eipNamespace2, podName4, IPFamilyValueV4, netInfo.GetNetworkName())},
					},
					&nbdb.LogicalSwitchPort{
						UUID:      "k8s-" + networkName1_ + node1Name + "-UUID",
						Name:      "k8s-" + networkName1_ + node1Name,
						Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1UDNSubnet).IP.String()},
					},
					&nbdb.LogicalSwitch{
						UUID:        networkName1_ + layer2SwitchName + "-UUID",
						Name:        networkName1_ + layer2SwitchName,
						Ports:       []string{"k8s-" + networkName1_ + node1Name + "-UUID"},
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: netInfo.GetNetworkName(), ovntypes.TopologyExternalID: ovntypes.Layer2Topology},
						QOSRules:    []string{fmt.Sprintf("%s-QoS-UUID", netInfo.GetNetworkName())},
					},
					getNoReRouteReplyTrafficPolicyForController(netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName),
					getDefaultQoSRule(false, netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName),
					egressIPServedPodsASUDNv4,
					udnEnabledSvcV4,
				}
				ginkgo.By("ensure expected equals actual")
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseStateTwoEgressNodes))
				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})

	ginkgo.Context("EgressIP update", func() {
		ginkgo.It("should update UDN and CDN config", func() {
			// Test steps:
			// update an EIP selecting a pod on an UDN and another pod on a CDN
			// EIP egresses locally and remote
			// EIP egresses remote
			// EIP egresses locally and remote
			app.Action = func(ctx *cli.Context) error {
				// Node 1 is local, Node 2 is remote
				egressIP1 := "192.168.126.101"
				egressIP2 := "192.168.126.102"
				node1IPv4 := "192.168.126.202"
				node1IPv4CIDR := node1IPv4 + "/24"
				node2IPv4 := "192.168.126.51"
				node2IPv4CIDR := node2IPv4 + "/24"
				_, node1CDNSubnet, _ := net.ParseCIDR(v4Node1Subnet)
				_, node1UDNSubnet, _ := net.ParseCIDR(v4Node1Net1)
				nadName := util.GetNADName(eipNamespace2, nadName1)
				egressCDNNamespace := testing.NewNamespaceWithLabels(eipNamespace, egressPodLabel)
				egressUDNNamespace := newUDNNamespaceWithLabels(eipNamespace2, egressPodLabel)
				egressPodCDNLocal := *testing.NewPodWithLabels(eipNamespace, podName, node1Name, podV4IP, egressPodLabel)
				egressPodUDNLocal := *testing.NewPodWithLabels(eipNamespace2, podName2, node1Name, v4Pod1IPNode1Net1, egressPodLabel)
				setPrimaryNetworkAnnot(&egressPodUDNLocal, nadName, fmt.Sprintf("%s%s", v4Pod1IPNode1Net1, util.GetIPFullMaskString(v4Pod1IPNode1Net1)))
				egressPodCDNRemote := *testing.NewPodWithLabels(eipNamespace, podName3, node2Name, podV4IP2, egressPodLabel)
				setPrimaryNetworkAnnot(&egressPodCDNRemote, ovntypes.DefaultNetworkName, fmt.Sprintf("%s%s", podV4IP2, util.GetIPFullMaskString(podV4IP2)))
				egressPodUDNRemote := *testing.NewPodWithLabels(eipNamespace2, podName4, node2Name, v4Pod2IPNode2Net1, egressPodLabel)
				setPrimaryNetworkAnnot(&egressPodUDNRemote, nadName, fmt.Sprintf("%s%s", v4Pod2IPNode2Net1, util.GetIPFullMaskString(v4Pod2IPNode2Net1)))

				netconf := ovncnitypes.NetConf{
					NetConf: cnitypes.NetConf{
						Name: networkName1,
						Type: "ovn-k8s-cni-overlay",
					},
					Role:          ovntypes.NetworkRolePrimary,
					Topology:      ovntypes.Layer2Topology,
					NADName:       nadName,
					Subnets:       v4Net1,
					TransitSubnet: config.ClusterManager.V4TransitSubnet,
				}
				nad, err := newNetworkAttachmentDefinition(
					eipNamespace2,
					nadName1,
					netconf,
				)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				netInfo, err := util.NewNetInfo(&netconf)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				node1Annotations := map[string]string{
					"k8s.ovn.org/node-primary-ifaddr":             fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4CIDR, ""),
					"k8s.ovn.org/node-subnets":                    fmt.Sprintf("{\"default\":\"%s\",\"%s\":\"%s\"}", v4Node1Subnet, networkName1, v4Node1Net1),
					"k8s.ovn.org/node-transit-switch-port-ifaddr": fmt.Sprintf("{\"ipv4\":\"%s/16\"}", v4Node1Tsp),
					"k8s.ovn.org/node-chassis-id":                 "473ca66d-d800-472f-b289-1ab81ae7f21c",
					util.OvnNodeID:                                "1",
					util.OVNNodeHostCIDRs:                         fmt.Sprintf("[\"%s\"]", node1IPv4CIDR),
					util.OvnNodeL3GatewayConfig: fmt.Sprintf(`{"%s":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"%s", "next-hop":"%s", "next-hops": ["%s"]},
"default":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"192.168.126.12/24", "next-hop": "192.168.126.1", "next-hops": ["192.168.126.1"]}}`, networkName1, v4Net1, gwIP, gwIP),
					util.Layer2TopologyVersion: util.TransitRouterTopoVersion,
				}
				labels := map[string]string{
					"k8s.ovn.org/egress-assignable": "",
				}
				node1 := getNodeObj(node1Name, node1Annotations, labels)
				node2Annotations := map[string]string{
					"k8s.ovn.org/node-primary-ifaddr":             fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node2IPv4CIDR, ""),
					"k8s.ovn.org/node-subnets":                    fmt.Sprintf("{\"default\":\"%s\",\"%s\":\"%s\"}", v4Node2Subnet, networkName1, v4Node2Net1),
					"k8s.ovn.org/node-transit-switch-port-ifaddr": fmt.Sprintf("{\"ipv4\":\"%s/16\"}", v4Node2Tsp),
					"k8s.ovn.org/node-chassis-id":                 "473ca66d-d800-472f-b289-1ab81ae7f21c",
					util.OvnNodeID:                                "2",
					util.OVNNodeHostCIDRs:                         fmt.Sprintf("[\"%s\"]", node2IPv4CIDR),
					util.OvnNodeL3GatewayConfig: fmt.Sprintf(`{"%s":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"%s", "next-hop":"%s", "next-hops": ["%s"]},
"default":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"192.168.126.12/24", "next-hop": "192.168.126.1", "next-hops": ["192.168.126.1"]}}`, networkName1, v4Net1, gwIP, gwIP),
					util.Layer2TopologyVersion: util.TransitRouterTopoVersion,
				}
				node2 := getNodeObj(node2Name, node2Annotations, labels)
				twoNodeStatus := []egressipv1.EgressIPStatusItem{
					{
						Node:     node1Name,
						EgressIP: egressIP1,
					},
					{
						Node:     node2Name,
						EgressIP: egressIP2,
					},
				}
				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMetaWithMark(egressIPName, eIP1Mark),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{egressIP1, egressIP2},
						PodSelector: metav1.LabelSelector{
							MatchLabels: egressPodLabel,
						},
						NamespaceSelector: metav1.LabelSelector{
							MatchLabels: egressPodLabel,
						},
					},
					Status: egressipv1.EgressIPStatus{
						Items: twoNodeStatus,
					},
				}
				ginkgo.By("create EgressIP that selects pods in a CDN and UDN")
				initialDB := []libovsdbtest.TestData{
					//CDN start
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name,
						Networks: []string{node1DefaultRtoJIPCIDR},
					},
					&nbdb.LogicalRouter{
						Name: ovntypes.OVNClusterRouter,
						UUID: ovntypes.OVNClusterRouter + "-UUID",
					},
					&nbdb.LogicalRouter{
						Name:  ovntypes.GWRouterPrefix + node1.Name,
						UUID:  ovntypes.GWRouterPrefix + node1.Name + "-UUID",
						Ports: []string{ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name + "-UUID"},
					},
					&nbdb.LogicalSwitchPort{
						UUID:      "k8s-" + node1Name + "-UUID",
						Name:      "k8s-" + node1Name,
						Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1CDNSubnet).IP.String()},
					},
					&nbdb.LogicalSwitch{
						UUID:  node1Name + "-UUID",
						Name:  node1Name,
						Ports: []string{"k8s-" + node1Name + "-UUID"},
					},
					// UDN start
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.RouterToTransitRouterPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name + "-UUID",
						Name:     ovntypes.RouterToTransitRouterPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name,
						Networks: []string{node1Network1JoinCIDR, node1Network1TransitCIDR},
					},
					&nbdb.LogicalRouter{
						Name:        netInfo.GetNetworkScopedClusterRouterName(),
						UUID:        netInfo.GetNetworkScopedClusterRouterName() + "-UUID",
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: networkName1, ovntypes.TopologyExternalID: ovntypes.Layer2Topology},
					},
					&nbdb.LogicalRouter{
						UUID:        netInfo.GetNetworkScopedGWRouterName(node1.Name) + "-UUID",
						Name:        netInfo.GetNetworkScopedGWRouterName(node1.Name),
						Ports:       []string{ovntypes.RouterToTransitRouterPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name + "-UUID"},
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: networkName1, ovntypes.TopologyExternalID: ovntypes.Layer2Topology},
					},
					&nbdb.LogicalSwitchPort{
						UUID:      "k8s-" + networkName1_ + node1Name + "-UUID",
						Name:      "k8s-" + networkName1_ + node1Name,
						Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1UDNSubnet).IP.String()},
					},
					&nbdb.LogicalSwitch{
						UUID:        networkName1_ + layer2SwitchName + "-UUID",
						Name:        networkName1_ + layer2SwitchName,
						Ports:       []string{"k8s-" + networkName1_ + node1Name + "-UUID"},
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: networkName1, ovntypes.TopologyExternalID: ovntypes.Layer2Topology},
					},
				}
				fakeOvn.startWithDBSetup(
					libovsdbtest.TestSetup{
						NBData: initialDB,
					},
					&corev1.NodeList{
						Items: []corev1.Node{node1, node2},
					},
					&corev1.NamespaceList{
						Items: []corev1.Namespace{*egressCDNNamespace, *egressUDNNamespace},
					},
					&corev1.PodList{
						Items: []corev1.Pod{egressPodCDNLocal, egressPodUDNLocal, egressPodCDNRemote, egressPodUDNRemote},
					},
					&nadv1.NetworkAttachmentDefinitionList{
						Items: []nadv1.NetworkAttachmentDefinition{*nad},
					},
				)
				asf := addressset.NewOvnAddressSetFactory(fakeOvn.nbClient, true, false)
				// watch EgressIP depends on UDN enabled svcs address set being available
				c := udnenabledsvc.NewController(fakeOvn.nbClient, asf, fakeOvn.controller.watchFactory.ServiceCoreInformer(), []string{})
				go func() {
					gomega.Expect(c.Run(ctx.Done())).Should(gomega.Succeed())
				}()
				// Add pod IPs to CDN cache
				iCDN, nCDN, _ := net.ParseCIDR(podV4IP + "/23")
				nCDN.IP = iCDN
				fakeOvn.controller.nodeName = node1.Name
				fakeOvn.eIPController.nodeName = node1.Name
				fakeOvn.controller.logicalPortCache.add(&egressPodCDNLocal, "", ovntypes.DefaultNetworkName, "", nil, []*net.IPNet{nCDN})
				secConInfo, ok := fakeOvn.userDefinedNetworkControllers[networkName1]
				gomega.Expect(ok).To(gomega.BeTrue())
				gomega.Expect(fakeOvn.eIPController.SyncNodeCache()).To(gomega.Succeed())
				err = fakeOvn.networkManager.Start()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				defer fakeOvn.networkManager.Stop()
				// simulate Start() of UDN controller
				err = fakeOvn.eIPController.ensureRouterPoliciesForNetwork(secConInfo.bnc.GetNetInfo(), &node1)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.eIPController.ensureSwitchPoliciesForNode(secConInfo.bnc.GetNetInfo(), node1Name)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.eIPController.ensureNetworkPending(networkName1)
				gomega.Expect(fakeOvn.eIPController.SyncNetworkReady(networkName1)).To(gomega.Succeed())
				err = fakeOvn.controller.WatchEgressIPPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIPNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressNodes()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// Add pod IPs to UDN cache
				iUDN, nUDN, _ := net.ParseCIDR(v4Pod1IPNode1Net1 + "/23")
				nUDN.IP = iUDN
				secConInfo.bnc.logicalPortCache.add(&egressPodUDNLocal, "", util.GetNADName(nad.Namespace, nad.Name), "", nil, []*net.IPNet{nUDN})
				_, err = fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Create(context.TODO(), &eIP, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				egressSVCServedPodsASv4, _ := buildEgressServiceAddressSets(nil)
				egressIPServedPodsASCDNv4, _ := buildEgressIPServedPodsAddressSets([]string{podV4IP}, ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkControllerName)
				egressNodeIPsASv4, _ := buildEgressIPNodeAddressSets([]string{node1IPv4, node2IPv4})
				egressIPServedPodsASUDNv4, _ := buildEgressIPServedPodsAddressSetsForController([]string{v4Pod1IPNode1Net1}, netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName)
				gomega.Eventually(c.IsAddressSetAvailable).Should(gomega.BeTrue())
				dbIDs := udnenabledsvc.GetAddressSetDBIDs()
				udnEnabledSvcV4, _ := addressset.GetTestDbAddrSets(dbIDs, []string{})

				node1LRP := "k8s-node1"
				expectedDatabaseStateTwoEgressNodes := []libovsdbtest.TestData{
					// CDN
					getReRouteStaticRoute(v4ClusterSubnet, node1DefaultRtoJIP),
					getReRoutePolicy(podV4IP, "4", "reroute-UUID", []string{node1DefaultRtoJIP, v4Node2Tsp},
						getEgressIPLRPReRouteDbIDs(eIP.Name, egressPodCDNLocal.Namespace, egressPodCDNLocal.Name, IPFamilyValueV4, ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkControllerName).GetExternalIDs()),
					&nbdb.LogicalRouterPolicy{
						Priority:    ovntypes.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4ClusterSubnet, v4ClusterSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-no-reroute-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    ovntypes.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4ClusterSubnet, config.Gateway.V4JoinSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-service-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouter{
						Name:  ovntypes.GWRouterPrefix + node1.Name,
						UUID:  ovntypes.GWRouterPrefix + node1.Name + "-UUID",
						Ports: []string{ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name + "-UUID"},
						Nat:   []string{"egressip-nat-UUID", "egressip-nat2-UUID"},
					},
					&nbdb.LogicalRouter{
						Name: ovntypes.OVNClusterRouter,
						UUID: ovntypes.OVNClusterRouter + "-UUID",
						Policies: []string{"default-no-reroute-UUID", "no-reroute-service-UUID",
							"default-no-reroute-node-UUID", "default-no-reroute-reply-traffic", "reroute-UUID"},
						StaticRoutes: []string{"reroute-static-route-UUID"},
					},
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name,
						Networks: []string{node1DefaultRtoJIPCIDR},
					},
					&nbdb.LogicalRouterPolicy{
						Priority: ovntypes.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
							egressIPServedPodsASCDNv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": ovntypes.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalSwitchPort{
						UUID:      "k8s-" + node1Name + "-UUID",
						Name:      "k8s-" + node1Name,
						Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1CDNSubnet).IP.String()},
					},
					&nbdb.LogicalSwitch{
						UUID:     node1Name + "-UUID",
						Name:     node1Name,
						Ports:    []string{"k8s-" + node1Name + "-UUID"},
						QOSRules: []string{"default-QoS-UUID"},
					},
					&nbdb.NAT{
						UUID:        "egressip-nat-UUID",
						LogicalIP:   podV4IP2,
						ExternalIP:  egressIP1,
						ExternalIDs: getEgressIPNATDbIDs(egressIPName, egressPodCDNRemote.Namespace, egressPodCDNRemote.Name, IPFamilyValueV4, ovntypes.DefaultNetworkControllerName).GetExternalIDs(),
						Type:        nbdb.NATTypeSNAT,
						LogicalPort: &node1LRP,
						Options: map[string]string{
							"stateless": "false",
						},
					},
					&nbdb.NAT{
						UUID:        "egressip-nat2-UUID",
						LogicalIP:   podV4IP,
						ExternalIP:  egressIP1,
						ExternalIDs: getEgressIPNATDbIDs(egressIPName, egressPodCDNLocal.Namespace, egressPodCDNLocal.Name, IPFamilyValueV4, ovntypes.DefaultNetworkControllerName).GetExternalIDs(),
						Type:        nbdb.NATTypeSNAT,
						LogicalPort: &node1LRP,
						Options: map[string]string{
							"stateless": "false",
						},
					},
					getNoReRouteReplyTrafficPolicy(ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkControllerName),
					getDefaultQoSRule(false, ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkControllerName),
					egressSVCServedPodsASv4,
					egressIPServedPodsASCDNv4,
					egressNodeIPsASv4,

					// UDN
					getReRoutePolicyForController(egressIPName, eipNamespace2, podName2, v4Pod1IPNode1Net1, eIP1Mark, IPFamilyValueV4, []string{node1Network1TransitIP, node2Network1TransitIP}, netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName),
					getReRoutePolicyForController(egressIPName, eipNamespace2, podName4, v4Pod2IPNode2Net1, eIP1Mark, IPFamilyValueV4, []string{node1Network1TransitIP}, netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName),
					getGWPktMarkLRPForController(eIP1Mark, egressIPName, eipNamespace2, podName2, v4Pod1IPNode1Net1, IPFamilyValueV4,
						netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName),
					getGWPktMarkLRPForController(eIP1Mark, egressIPName, eipNamespace2, podName4, v4Pod2IPNode2Net1, IPFamilyValueV4,
						netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName),
					getNoReRoutePolicyForUDNEnabledSvc(false, netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName,
						egressIPServedPodsASUDNv4.Name, egressSVCServedPodsASv4.Name, udnEnabledSvcV4.Name),
					&nbdb.LogicalRouterPolicy{
						Priority:    ovntypes.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4Net1, v4Net1),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "udn-default-no-reroute-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    ovntypes.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4Net1, config.Gateway.V4JoinSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "udn-no-reroute-service-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority: ovntypes.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
							egressIPServedPodsASUDNv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "udn-default-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": ovntypes.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.RouterToTransitRouterPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name + "-UUID",
						Name:     ovntypes.RouterToTransitRouterPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name,
						Networks: []string{node1Network1JoinCIDR, node1Network1TransitCIDR},
					},
					&nbdb.LogicalRouter{
						Name:        netInfo.GetNetworkScopedClusterRouterName(),
						UUID:        netInfo.GetNetworkScopedClusterRouterName() + "-UUID",
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: netInfo.GetNetworkName(), ovntypes.TopologyExternalID: ovntypes.Layer2Topology},
						Policies: []string{
							"udn-default-no-reroute-node-UUID", "udn-default-no-reroute-UUID", "udn-no-reroute-service-UUID", "udn-enabled-svc-no-reroute-UUID",
							fmt.Sprintf("%s-no-reroute-reply-traffic", netInfo.GetNetworkName()),
							getReRoutePolicyUUID(eipNamespace2, podName2, IPFamilyValueV4, netInfo.GetNetworkName()),
							getReRoutePolicyUUID(eipNamespace2, podName4, IPFamilyValueV4, netInfo.GetNetworkName())},
					},
					&nbdb.LogicalRouter{
						UUID:        netInfo.GetNetworkScopedGWRouterName(node1.Name) + "-UUID",
						Name:        netInfo.GetNetworkScopedGWRouterName(node1.Name),
						Ports:       []string{ovntypes.RouterToTransitRouterPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name + "-UUID"},
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: netInfo.GetNetworkName(), ovntypes.TopologyExternalID: ovntypes.Layer2Topology},
						Policies: []string{getGWPktMarkLRPUUID(eipNamespace2, podName2, IPFamilyValueV4, netInfo.GetNetworkName()),
							getGWPktMarkLRPUUID(eipNamespace2, podName4, IPFamilyValueV4, netInfo.GetNetworkName())},
					},
					&nbdb.LogicalSwitchPort{
						UUID:      "k8s-" + networkName1_ + node1Name + "-UUID",
						Name:      "k8s-" + networkName1_ + node1Name,
						Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1UDNSubnet).IP.String()},
					},
					&nbdb.LogicalSwitch{
						UUID:        networkName1_ + layer2SwitchName + "-UUID",
						Name:        networkName1_ + layer2SwitchName,
						Ports:       []string{"k8s-" + networkName1_ + node1Name + "-UUID"},
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: netInfo.GetNetworkName(), ovntypes.TopologyExternalID: ovntypes.Layer2Topology},
						QOSRules:    []string{fmt.Sprintf("%s-QoS-UUID", netInfo.GetNetworkName())},
					},
					getNoReRouteReplyTrafficPolicyForController(netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName),
					getDefaultQoSRule(false, netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName),
					egressIPServedPodsASUDNv4,
					udnEnabledSvcV4,
				}
				ginkgo.By("ensure expected equals actual")
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseStateTwoEgressNodes))
				ginkgo.By("patch EgressIP status to ensure remote node is egressable only")
				oneNodeStatus := []egressipv1.EgressIPStatusItem{
					{
						Node:     node2Name,
						EgressIP: egressIP2,
					},
				}
				err = patchEgressIP(fakeOvn.controller.kube.PatchEgressIP, eIP.Name, generateEgressIPPatches(eIP1Mark, oneNodeStatus)...)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(getEgressIPStatusLen(eIP.Name)).Should(gomega.Equal(1))
				expectedDatabaseStateOneEgressNode := []libovsdbtest.TestData{
					// CDN
					getReRouteStaticRoute(v4ClusterSubnet, node1DefaultRtoJIP),
					getReRoutePolicy(podV4IP, "4", "reroute-UUID", []string{v4Node2Tsp},
						getEgressIPLRPReRouteDbIDs(eIP.Name, egressPodCDNLocal.Namespace, egressPodCDNLocal.Name, IPFamilyValueV4, ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkControllerName).GetExternalIDs()),
					&nbdb.LogicalRouterPolicy{
						Priority:    ovntypes.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4ClusterSubnet, v4ClusterSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-no-reroute-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    ovntypes.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4ClusterSubnet, config.Gateway.V4JoinSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-service-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouter{
						Name:  ovntypes.GWRouterPrefix + node1.Name,
						UUID:  ovntypes.GWRouterPrefix + node1.Name + "-UUID",
						Ports: []string{ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name + "-UUID"},
					},
					&nbdb.LogicalRouter{
						Name: ovntypes.OVNClusterRouter,
						UUID: ovntypes.OVNClusterRouter + "-UUID",
						Policies: []string{"default-no-reroute-UUID", "no-reroute-service-UUID",
							"default-no-reroute-node-UUID", "default-no-reroute-reply-traffic", "reroute-UUID"},
						StaticRoutes: []string{"reroute-static-route-UUID"},
					},
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name,
						Networks: []string{node1DefaultRtoJIPCIDR},
					},
					&nbdb.LogicalRouterPolicy{
						Priority: ovntypes.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
							egressIPServedPodsASCDNv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": ovntypes.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalSwitchPort{
						UUID:      "k8s-" + node1Name + "-UUID",
						Name:      "k8s-" + node1Name,
						Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1CDNSubnet).IP.String()},
					},
					&nbdb.LogicalSwitch{
						UUID:     node1Name + "-UUID",
						Name:     node1Name,
						Ports:    []string{"k8s-" + node1Name + "-UUID"},
						QOSRules: []string{"default-QoS-UUID"},
					},
					getNoReRouteReplyTrafficPolicy(ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkControllerName),
					getDefaultQoSRule(false, ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkControllerName),
					egressSVCServedPodsASv4,
					egressIPServedPodsASCDNv4,
					egressNodeIPsASv4,

					// UDN
					getReRoutePolicyForController(egressIPName, eipNamespace2, podName2, v4Pod1IPNode1Net1, eIP1Mark, IPFamilyValueV4, []string{node2Network1TransitIP}, netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName),
					getNoReRoutePolicyForUDNEnabledSvc(false, netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName, egressIPServedPodsASUDNv4.Name, egressSVCServedPodsASv4.Name, udnEnabledSvcV4.Name),
					&nbdb.LogicalRouterPolicy{
						Priority:    ovntypes.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4Net1, v4Net1),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "udn-default-no-reroute-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    ovntypes.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4Net1, config.Gateway.V4JoinSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "udn-no-reroute-service-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority: ovntypes.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
							egressIPServedPodsASUDNv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "udn-default-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": ovntypes.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.RouterToTransitRouterPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name + "-UUID",
						Name:     ovntypes.RouterToTransitRouterPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name,
						Networks: []string{node1Network1JoinCIDR, node1Network1TransitCIDR},
					},
					&nbdb.LogicalRouter{
						Name:        netInfo.GetNetworkScopedClusterRouterName(),
						UUID:        netInfo.GetNetworkScopedClusterRouterName() + "-UUID",
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: netInfo.GetNetworkName(), ovntypes.TopologyExternalID: ovntypes.Layer2Topology},
						Policies: []string{
							"udn-default-no-reroute-node-UUID", "udn-default-no-reroute-UUID", "udn-no-reroute-service-UUID", "udn-enabled-svc-no-reroute-UUID",
							fmt.Sprintf("%s-no-reroute-reply-traffic", netInfo.GetNetworkName()),
							getReRoutePolicyUUID(eipNamespace2, podName2, IPFamilyValueV4, netInfo.GetNetworkName())},
					},
					&nbdb.LogicalRouter{
						UUID:        netInfo.GetNetworkScopedGWRouterName(node1.Name) + "-UUID",
						Name:        netInfo.GetNetworkScopedGWRouterName(node1.Name),
						Ports:       []string{ovntypes.RouterToTransitRouterPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name + "-UUID"},
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: netInfo.GetNetworkName(), ovntypes.TopologyExternalID: ovntypes.Layer2Topology},
					},
					&nbdb.LogicalSwitchPort{
						UUID:      "k8s-" + networkName1_ + node1Name + "-UUID",
						Name:      "k8s-" + networkName1_ + node1Name,
						Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1UDNSubnet).IP.String()},
					},
					&nbdb.LogicalSwitch{
						UUID:        networkName1_ + layer2SwitchName + "-UUID",
						Name:        networkName1_ + layer2SwitchName,
						Ports:       []string{"k8s-" + networkName1_ + node1Name + "-UUID"},
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: netInfo.GetNetworkName(), ovntypes.TopologyExternalID: ovntypes.Layer2Topology},
						QOSRules:    []string{fmt.Sprintf("%s-QoS-UUID", netInfo.GetNetworkName())},
					},
					getNoReRouteReplyTrafficPolicyForController(netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName),
					getDefaultQoSRule(false, netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName),
					egressIPServedPodsASUDNv4,
					udnEnabledSvcV4,
				}
				ginkgo.By("ensure expected equals actual")
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseStateOneEgressNode))

				ginkgo.By("restore both nodes as egressable")
				err = patchEgressIP(fakeOvn.controller.kube.PatchEgressIP, eIP.Name, generateEgressIPPatches(eIP1Mark, twoNodeStatus)...)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(getEgressIPStatusLen(eIP.Name)).Should(gomega.Equal(2))
				ginkgo.By("ensure expected equals actual")
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseStateTwoEgressNodes))
				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})

	ginkgo.Context("EgressIP delete", func() {
		ginkgo.It("should del UDN and CDN config", func() {
			// Test steps:
			// One EIP selecting a pod on an UDN and another pod on a CDN
			// EIP egresses locally and remote
			// Delete EIP
			app.Action = func(ctx *cli.Context) error {
				// Node 1 is local, Node 2 is remote
				egressIP1 := "192.168.126.101"
				egressIP2 := "192.168.126.102"
				node1IPv4 := "192.168.126.202"
				node1IPv4CIDR := node1IPv4 + "/24"
				node2IPv4 := "192.168.126.51"
				node2IPv4CIDR := node2IPv4 + "/24"
				_, node1CDNSubnet, _ := net.ParseCIDR(v4Node1Subnet)
				_, node1UDNSubnet, _ := net.ParseCIDR(v4Node1Net1)
				nadName := util.GetNADName(eipNamespace2, nadName1)
				egressCDNNamespace := testing.NewNamespaceWithLabels(eipNamespace, egressPodLabel)
				egressUDNNamespace := newUDNNamespaceWithLabels(eipNamespace2, egressPodLabel)
				egressPodCDNLocal := *testing.NewPodWithLabels(eipNamespace, podName, node1Name, podV4IP, egressPodLabel)
				egressPodUDNLocal := *testing.NewPodWithLabels(eipNamespace2, podName2, node1Name, v4Pod1IPNode1Net1, egressPodLabel)
				setPrimaryNetworkAnnot(&egressPodUDNLocal, nadName, fmt.Sprintf("%s%s", v4Pod1IPNode1Net1, util.GetIPFullMaskString(v4Pod1IPNode1Net1)))
				egressPodCDNRemote := *testing.NewPodWithLabels(eipNamespace, podName3, node2Name, podV4IP2, egressPodLabel)
				setPrimaryNetworkAnnot(&egressPodCDNRemote, ovntypes.DefaultNetworkName, fmt.Sprintf("%s%s", podV4IP2, util.GetIPFullMaskString(podV4IP2)))
				egressPodUDNRemote := *testing.NewPodWithLabels(eipNamespace2, podName4, node2Name, v4Pod2IPNode2Net1, egressPodLabel)
				setPrimaryNetworkAnnot(&egressPodUDNRemote, nadName, fmt.Sprintf("%s%s", v4Pod2IPNode2Net1, util.GetIPFullMaskString(v4Pod2IPNode2Net1)))
				netconf := ovncnitypes.NetConf{
					NetConf: cnitypes.NetConf{
						Name: networkName1,
						Type: "ovn-k8s-cni-overlay",
					},
					Role:          ovntypes.NetworkRolePrimary,
					Topology:      ovntypes.Layer2Topology,
					NADName:       nadName,
					Subnets:       v4Net1,
					TransitSubnet: config.ClusterManager.V4TransitSubnet,
				}
				nad, err := newNetworkAttachmentDefinition(
					eipNamespace2,
					nadName1,
					netconf,
				)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				netInfo, err := util.NewNetInfo(&netconf)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				node1Annotations := map[string]string{
					"k8s.ovn.org/node-primary-ifaddr":             fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4CIDR, ""),
					"k8s.ovn.org/node-subnets":                    fmt.Sprintf("{\"default\":\"%s\",\"%s\":\"%s\"}", v4Node1Subnet, networkName1, v4Node1Net1),
					"k8s.ovn.org/node-transit-switch-port-ifaddr": fmt.Sprintf("{\"ipv4\":\"%s/16\"}", v4Node1Tsp),
					"k8s.ovn.org/node-chassis-id":                 "473ca66d-d800-472f-b289-1ab81ae7f21c",
					util.OvnNodeID:                                "1",
					util.OVNNodeHostCIDRs:                         fmt.Sprintf("[\"%s\"]", node1IPv4CIDR),
					util.OvnNodeL3GatewayConfig: fmt.Sprintf(`{"%s":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"%s", "next-hop":"%s", "next-hops": ["%s"]},
"default":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"192.168.126.12/24", "next-hop": "192.168.126.1", "next-hops": ["192.168.126.1"]}}`, networkName1, v4Net1, gwIP, gwIP),
					util.Layer2TopologyVersion: util.TransitRouterTopoVersion,
				}
				labels := map[string]string{
					"k8s.ovn.org/egress-assignable": "",
				}
				node1 := getNodeObj(node1Name, node1Annotations, labels)
				node2Annotations := map[string]string{
					"k8s.ovn.org/node-primary-ifaddr":             fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node2IPv4CIDR, ""),
					"k8s.ovn.org/node-subnets":                    fmt.Sprintf("{\"default\":\"%s\",\"%s\":\"%s\"}", v4Node2Subnet, networkName1, v4Node2Net1),
					"k8s.ovn.org/node-transit-switch-port-ifaddr": fmt.Sprintf("{\"ipv4\":\"%s/16\"}", v4Node2Tsp),
					"k8s.ovn.org/node-chassis-id":                 "473ca66d-d800-472f-b289-1ab81ae7f21c",
					util.OvnNodeID:                                "2",
					util.OVNNodeHostCIDRs:                         fmt.Sprintf("[\"%s\"]", node2IPv4CIDR),
					util.OvnNodeL3GatewayConfig: fmt.Sprintf(`{"%s":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"%s", "next-hop":"%s", "next-hops": ["%s"]},
"default":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"192.168.126.12/24", "next-hop": "192.168.126.1", "next-hops": ["192.168.126.1"]}}`, networkName1, v4Net1, gwIP, gwIP),
					util.Layer2TopologyVersion: util.TransitRouterTopoVersion,
				}
				node2 := getNodeObj(node2Name, node2Annotations, labels)
				twoNodeStatus := []egressipv1.EgressIPStatusItem{
					{
						Node:     node1Name,
						EgressIP: egressIP1,
					},
					{
						Node:     node2Name,
						EgressIP: egressIP2,
					},
				}
				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMetaWithMark(egressIPName, eIP1Mark),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{egressIP1, egressIP2},
						PodSelector: metav1.LabelSelector{
							MatchLabels: egressPodLabel,
						},
						NamespaceSelector: metav1.LabelSelector{
							MatchLabels: egressPodLabel,
						},
					},
					Status: egressipv1.EgressIPStatus{
						Items: twoNodeStatus,
					},
				}
				ginkgo.By("create EgressIP that selects pods in a CDN and UDN")
				initialDB := []libovsdbtest.TestData{
					//CDN start
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name,
						Networks: []string{node1DefaultRtoJIPCIDR},
					},
					&nbdb.LogicalRouter{
						Name: ovntypes.OVNClusterRouter,
						UUID: ovntypes.OVNClusterRouter + "-UUID",
					},
					&nbdb.LogicalRouter{
						Name:  ovntypes.GWRouterPrefix + node1.Name,
						UUID:  ovntypes.GWRouterPrefix + node1.Name + "-UUID",
						Ports: []string{ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name + "-UUID"},
					},
					&nbdb.LogicalSwitchPort{
						UUID:      "k8s-" + node1Name + "-UUID",
						Name:      "k8s-" + node1Name,
						Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1CDNSubnet).IP.String()},
					},
					&nbdb.LogicalSwitch{
						UUID:  node1Name + "-UUID",
						Name:  node1Name,
						Ports: []string{"k8s-" + node1Name + "-UUID"},
					},
					// UDN start
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.RouterToTransitRouterPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name + "-UUID",
						Name:     ovntypes.RouterToTransitRouterPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name,
						Networks: []string{node1Network1JoinCIDR, node1Network1TransitCIDR},
					},
					&nbdb.LogicalRouter{
						Name:        netInfo.GetNetworkScopedClusterRouterName(),
						UUID:        netInfo.GetNetworkScopedClusterRouterName() + "-UUID",
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: networkName1, ovntypes.TopologyExternalID: ovntypes.Layer2Topology},
					},
					&nbdb.LogicalRouter{
						UUID:        netInfo.GetNetworkScopedGWRouterName(node1.Name) + "-UUID",
						Name:        netInfo.GetNetworkScopedGWRouterName(node1.Name),
						Ports:       []string{ovntypes.RouterToTransitRouterPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name + "-UUID"},
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: networkName1, ovntypes.TopologyExternalID: ovntypes.Layer2Topology},
					},
					&nbdb.LogicalSwitchPort{
						UUID:      "k8s-" + networkName1_ + node1Name + "-UUID",
						Name:      "k8s-" + networkName1_ + node1Name,
						Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1UDNSubnet).IP.String()},
					},
					&nbdb.LogicalSwitch{
						UUID:        networkName1_ + layer2SwitchName + "-UUID",
						Name:        networkName1_ + layer2SwitchName,
						Ports:       []string{"k8s-" + networkName1_ + node1Name + "-UUID"},
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: networkName1, ovntypes.TopologyExternalID: ovntypes.Layer2Topology},
					},
				}
				fakeOvn.startWithDBSetup(
					libovsdbtest.TestSetup{
						NBData: initialDB,
					},
					&corev1.NodeList{
						Items: []corev1.Node{node1, node2},
					},
					&corev1.NamespaceList{
						Items: []corev1.Namespace{*egressCDNNamespace, *egressUDNNamespace},
					},
					&corev1.PodList{
						Items: []corev1.Pod{egressPodCDNLocal, egressPodUDNLocal, egressPodCDNRemote, egressPodUDNRemote},
					},
					&nadv1.NetworkAttachmentDefinitionList{
						Items: []nadv1.NetworkAttachmentDefinition{*nad},
					},
				)
				asf := addressset.NewOvnAddressSetFactory(fakeOvn.nbClient, true, false)
				// watch EgressIP depends on UDN enabled svcs address set being available
				c := udnenabledsvc.NewController(fakeOvn.nbClient, asf, fakeOvn.controller.watchFactory.ServiceCoreInformer(), []string{})
				go func() {
					gomega.Expect(c.Run(ctx.Done())).Should(gomega.Succeed())
				}()
				// Add pod IPs to CDN cache
				iCDN, nCDN, _ := net.ParseCIDR(podV4IP + "/23")
				nCDN.IP = iCDN
				fakeOvn.controller.logicalPortCache.add(&egressPodCDNLocal, "", ovntypes.DefaultNetworkName, "", nil, []*net.IPNet{nCDN})
				fakeOvn.controller.nodeName = node1.Name
				fakeOvn.eIPController.nodeName = node1.Name
				err = fakeOvn.eIPController.SyncNodeCache()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.networkManager.Start()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				defer fakeOvn.networkManager.Stop()
				fakeOvn.eIPController.ensureNetworkPending(networkName1)
				gomega.Expect(fakeOvn.eIPController.SyncNetworkReady(networkName1)).To(gomega.Succeed())
				err = fakeOvn.controller.WatchEgressIPPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIPNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressNodes()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				secConInfo, ok := fakeOvn.userDefinedNetworkControllers[networkName1]
				gomega.Expect(ok).To(gomega.BeTrue())
				// Add pod IPs to UDN cache
				iUDN, nUDN, _ := net.ParseCIDR(v4Pod1IPNode1Net1 + "/23")
				nUDN.IP = iUDN
				secConInfo.bnc.logicalPortCache.add(&egressPodUDNLocal, "", util.GetNADName(nad.Namespace, nad.Name), "", nil, []*net.IPNet{nUDN})
				_, err = fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Create(context.TODO(), &eIP, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				egressSVCServedPodsASv4, _ := buildEgressServiceAddressSets(nil)
				egressIPServedPodsASCDNv4, _ := buildEgressIPServedPodsAddressSets([]string{podV4IP}, ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkControllerName)
				egressNodeIPsASv4, _ := buildEgressIPNodeAddressSets([]string{node1IPv4, node2IPv4})
				egressIPServedPodsASUDNv4, _ := buildEgressIPServedPodsAddressSetsForController([]string{v4Pod1IPNode1Net1}, netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName)
				gomega.Eventually(c.IsAddressSetAvailable).Should(gomega.BeTrue())
				dbIDs := udnenabledsvc.GetAddressSetDBIDs()
				udnEnabledSvcV4, _ := addressset.GetTestDbAddrSets(dbIDs, []string{})
				node1LRP := "k8s-node1"
				expectedDatabaseStateTwoEgressNodes := []libovsdbtest.TestData{
					// CDN
					getReRouteStaticRoute(v4ClusterSubnet, node1DefaultRtoJIP),
					getReRoutePolicy(podV4IP, "4", "reroute-UUID", []string{node1DefaultRtoJIP, v4Node2Tsp},
						getEgressIPLRPReRouteDbIDs(eIP.Name, egressPodCDNLocal.Namespace, egressPodCDNLocal.Name, IPFamilyValueV4, ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkControllerName).GetExternalIDs()),
					&nbdb.LogicalRouterPolicy{
						Priority:    ovntypes.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4ClusterSubnet, v4ClusterSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-no-reroute-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    ovntypes.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4ClusterSubnet, config.Gateway.V4JoinSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-service-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouter{
						Name:  ovntypes.GWRouterPrefix + node1.Name,
						UUID:  ovntypes.GWRouterPrefix + node1.Name + "-UUID",
						Ports: []string{ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name + "-UUID"},
						Nat:   []string{"egressip-nat-UUID", "egressip-nat2-UUID"},
					},
					&nbdb.LogicalRouter{
						Name: ovntypes.OVNClusterRouter,
						UUID: ovntypes.OVNClusterRouter + "-UUID",
						Policies: []string{"default-no-reroute-UUID", "no-reroute-service-UUID",
							"default-no-reroute-node-UUID", "default-no-reroute-reply-traffic", "reroute-UUID"},
						StaticRoutes: []string{"reroute-static-route-UUID"},
					},
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name,
						Networks: []string{node1DefaultRtoJIPCIDR},
					},
					&nbdb.LogicalRouterPolicy{
						Priority: ovntypes.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
							egressIPServedPodsASCDNv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": ovntypes.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalSwitchPort{
						UUID:      "k8s-" + node1Name + "-UUID",
						Name:      "k8s-" + node1Name,
						Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1CDNSubnet).IP.String()},
					},
					&nbdb.LogicalSwitch{
						UUID:     node1Name + "-UUID",
						Name:     node1Name,
						Ports:    []string{"k8s-" + node1Name + "-UUID"},
						QOSRules: []string{"default-QoS-UUID"},
					},
					&nbdb.NAT{
						UUID:        "egressip-nat-UUID",
						LogicalIP:   podV4IP2,
						ExternalIP:  egressIP1,
						ExternalIDs: getEgressIPNATDbIDs(egressIPName, egressPodCDNRemote.Namespace, egressPodCDNRemote.Name, IPFamilyValueV4, ovntypes.DefaultNetworkControllerName).GetExternalIDs(),
						Type:        nbdb.NATTypeSNAT,
						LogicalPort: &node1LRP,
						Options: map[string]string{
							"stateless": "false",
						},
					},
					&nbdb.NAT{
						UUID:        "egressip-nat2-UUID",
						LogicalIP:   podV4IP,
						ExternalIP:  egressIP1,
						ExternalIDs: getEgressIPNATDbIDs(egressIPName, egressPodCDNLocal.Namespace, egressPodCDNLocal.Name, IPFamilyValueV4, ovntypes.DefaultNetworkControllerName).GetExternalIDs(),
						Type:        nbdb.NATTypeSNAT,
						LogicalPort: &node1LRP,
						Options: map[string]string{
							"stateless": "false",
						},
					},
					getNoReRouteReplyTrafficPolicy(ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkControllerName),
					getDefaultQoSRule(false, ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkControllerName),
					egressSVCServedPodsASv4,
					egressIPServedPodsASCDNv4,
					egressNodeIPsASv4,

					// UDN
					getReRoutePolicyForController(egressIPName, eipNamespace2, podName2, v4Pod1IPNode1Net1, eIP1Mark, IPFamilyValueV4, []string{node1Network1TransitIP, node2Network1TransitIP}, netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName),
					getReRoutePolicyForController(egressIPName, eipNamespace2, podName4, v4Pod2IPNode2Net1, eIP1Mark, IPFamilyValueV4, []string{node1Network1TransitIP}, netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName),
					getGWPktMarkLRPForController(eIP1Mark, egressIPName, eipNamespace2, podName2, v4Pod1IPNode1Net1, IPFamilyValueV4,
						netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName),
					getGWPktMarkLRPForController(eIP1Mark, egressIPName, eipNamespace2, podName4, v4Pod2IPNode2Net1, IPFamilyValueV4,
						netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName),
					getNoReRoutePolicyForUDNEnabledSvc(false, netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName, egressIPServedPodsASUDNv4.Name, egressSVCServedPodsASv4.Name, udnEnabledSvcV4.Name),
					&nbdb.LogicalRouterPolicy{
						Priority:    ovntypes.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4Net1, v4Net1),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "udn-default-no-reroute-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    ovntypes.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4Net1, config.Gateway.V4JoinSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "udn-no-reroute-service-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority: ovntypes.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
							egressIPServedPodsASUDNv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "udn-default-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": ovntypes.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.RouterToTransitRouterPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name + "-UUID",
						Name:     ovntypes.RouterToTransitRouterPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name,
						Networks: []string{node1Network1JoinCIDR, node1Network1TransitCIDR},
					},
					&nbdb.LogicalRouter{
						Name:        netInfo.GetNetworkScopedClusterRouterName(),
						UUID:        netInfo.GetNetworkScopedClusterRouterName() + "-UUID",
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: netInfo.GetNetworkName(), ovntypes.TopologyExternalID: ovntypes.Layer2Topology},
						Policies: []string{
							"udn-default-no-reroute-node-UUID", "udn-default-no-reroute-UUID", "udn-no-reroute-service-UUID", "udn-enabled-svc-no-reroute-UUID",
							fmt.Sprintf("%s-no-reroute-reply-traffic", netInfo.GetNetworkName()),
							getReRoutePolicyUUID(eipNamespace2, podName2, IPFamilyValueV4, netInfo.GetNetworkName()),
							getReRoutePolicyUUID(eipNamespace2, podName4, IPFamilyValueV4, netInfo.GetNetworkName())},
					},
					&nbdb.LogicalRouter{
						UUID:        netInfo.GetNetworkScopedGWRouterName(node1.Name) + "-UUID",
						Name:        netInfo.GetNetworkScopedGWRouterName(node1.Name),
						Ports:       []string{ovntypes.RouterToTransitRouterPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name + "-UUID"},
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: netInfo.GetNetworkName(), ovntypes.TopologyExternalID: ovntypes.Layer2Topology},
						Policies: []string{getGWPktMarkLRPUUID(eipNamespace2, podName2, IPFamilyValueV4, netInfo.GetNetworkName()),
							getGWPktMarkLRPUUID(eipNamespace2, podName4, IPFamilyValueV4, netInfo.GetNetworkName())},
					},
					&nbdb.LogicalSwitchPort{
						UUID:      "k8s-" + networkName1_ + node1Name + "-UUID",
						Name:      "k8s-" + networkName1_ + node1Name,
						Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1UDNSubnet).IP.String()},
					},
					&nbdb.LogicalSwitch{
						UUID:        networkName1_ + layer2SwitchName + "-UUID",
						Name:        networkName1_ + layer2SwitchName,
						Ports:       []string{"k8s-" + networkName1_ + node1Name + "-UUID"},
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: netInfo.GetNetworkName(), ovntypes.TopologyExternalID: ovntypes.Layer2Topology},
						QOSRules:    []string{fmt.Sprintf("%s-QoS-UUID", netInfo.GetNetworkName())},
					},
					getNoReRouteReplyTrafficPolicyForController(netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName),
					getDefaultQoSRule(false, netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName),
					egressIPServedPodsASUDNv4,
					udnEnabledSvcV4,
				}
				ginkgo.By("ensure expected equals actual")
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseStateTwoEgressNodes))
				ginkgo.By("delete EgressIP")
				err = fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Delete(context.TODO(), eIP.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				egressIPServedPodsASCDNv4.Addresses = nil
				egressIPServedPodsASUDNv4.Addresses = nil
				expectedDatabaseState := []libovsdbtest.TestData{
					// CDN
					getReRouteStaticRoute(v4ClusterSubnet, node1DefaultRtoJIP),
					&nbdb.LogicalRouterPolicy{
						Priority:    ovntypes.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4ClusterSubnet, v4ClusterSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-no-reroute-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    ovntypes.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4ClusterSubnet, config.Gateway.V4JoinSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-service-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouter{
						Name:  ovntypes.GWRouterPrefix + node1.Name,
						UUID:  ovntypes.GWRouterPrefix + node1.Name + "-UUID",
						Ports: []string{ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name + "-UUID"},
					},
					&nbdb.LogicalRouter{
						Name: ovntypes.OVNClusterRouter,
						UUID: ovntypes.OVNClusterRouter + "-UUID",
						Policies: []string{"default-no-reroute-UUID", "no-reroute-service-UUID",
							"default-no-reroute-node-UUID", "default-no-reroute-reply-traffic"},
						StaticRoutes: []string{"reroute-static-route-UUID"},
					},
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name,
						Networks: []string{node1DefaultRtoJIPCIDR},
					},
					&nbdb.LogicalRouterPolicy{
						Priority: ovntypes.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
							egressIPServedPodsASCDNv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": ovntypes.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalSwitchPort{
						UUID:      "k8s-" + node1Name + "-UUID",
						Name:      "k8s-" + node1Name,
						Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1CDNSubnet).IP.String()},
					},
					&nbdb.LogicalSwitch{
						UUID:     node1Name + "-UUID",
						Name:     node1Name,
						Ports:    []string{"k8s-" + node1Name + "-UUID"},
						QOSRules: []string{"default-QoS-UUID"},
					},
					getNoReRouteReplyTrafficPolicy(ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkControllerName),
					getDefaultQoSRule(false, ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkControllerName),
					egressSVCServedPodsASv4,
					egressIPServedPodsASCDNv4,
					egressNodeIPsASv4,

					// UDN
					getNoReRoutePolicyForUDNEnabledSvc(false, netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName, egressIPServedPodsASUDNv4.Name, egressSVCServedPodsASv4.Name, udnEnabledSvcV4.Name),
					&nbdb.LogicalRouterPolicy{
						Priority:    ovntypes.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4Net1, v4Net1),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "udn-default-no-reroute-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    ovntypes.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4Net1, config.Gateway.V4JoinSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "udn-no-reroute-service-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority: ovntypes.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
							egressIPServedPodsASUDNv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "udn-default-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": ovntypes.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.RouterToTransitRouterPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name + "-UUID",
						Name:     ovntypes.RouterToTransitRouterPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name,
						Networks: []string{node1Network1JoinCIDR, node1Network1TransitCIDR},
					},
					&nbdb.LogicalRouter{
						Name:        netInfo.GetNetworkScopedClusterRouterName(),
						UUID:        netInfo.GetNetworkScopedClusterRouterName() + "-UUID",
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: netInfo.GetNetworkName(), ovntypes.TopologyExternalID: ovntypes.Layer2Topology},
						Policies: []string{
							"udn-default-no-reroute-node-UUID", "udn-default-no-reroute-UUID", "udn-no-reroute-service-UUID", "udn-enabled-svc-no-reroute-UUID",
							fmt.Sprintf("%s-no-reroute-reply-traffic", netInfo.GetNetworkName())},
					},
					&nbdb.LogicalRouter{
						UUID:        netInfo.GetNetworkScopedGWRouterName(node1.Name) + "-UUID",
						Name:        netInfo.GetNetworkScopedGWRouterName(node1.Name),
						Ports:       []string{ovntypes.RouterToTransitRouterPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name + "-UUID"},
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: netInfo.GetNetworkName(), ovntypes.TopologyExternalID: ovntypes.Layer2Topology},
					},
					&nbdb.LogicalSwitchPort{
						UUID:      "k8s-" + networkName1_ + node1Name + "-UUID",
						Name:      "k8s-" + networkName1_ + node1Name,
						Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1UDNSubnet).IP.String()},
					},
					&nbdb.LogicalSwitch{
						UUID:        networkName1_ + layer2SwitchName + "-UUID",
						Name:        networkName1_ + layer2SwitchName,
						Ports:       []string{"k8s-" + networkName1_ + node1Name + "-UUID"},
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: netInfo.GetNetworkName(), ovntypes.TopologyExternalID: ovntypes.Layer2Topology},
						QOSRules:    []string{fmt.Sprintf("%s-QoS-UUID", netInfo.GetNetworkName())},
					},
					getNoReRouteReplyTrafficPolicyForController(netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName),
					getDefaultQoSRule(false, netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName),
					egressIPServedPodsASUDNv4,
					udnEnabledSvcV4,
				}
				ginkgo.By("ensure expected equals actual")
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})

	ginkgo.Context("Namespace update", func() {
		ginkgo.It("should update UDN and CDN config", func() {
			// Test steps:
			// create an EIP not selecting a pod on an UDN and another pod on a CDN because namespace labels aren't selected
			// EIP egresses locally and remote
			// Update namespace to match EIP selectors
			app.Action = func(ctx *cli.Context) error {
				// Node 1 is local, Node 2 is remote
				egressIP1 := "192.168.126.101"
				egressIP2 := "192.168.126.102"
				node1IPv4 := "192.168.126.202"
				node1IPv4CIDR := node1IPv4 + "/24"
				node2IPv4 := "192.168.126.51"
				node2IPv4CIDR := node2IPv4 + "/24"
				_, node1CDNSubnet, _ := net.ParseCIDR(v4Node1Subnet)
				_, node1UDNSubnet, _ := net.ParseCIDR(v4Node1Net1)
				egressCDNNamespace := testing.NewNamespaceWithLabels(eipNamespace, nil)
				egressUDNNamespace := newUDNNamespaceWithLabels(eipNamespace2, nil)
				egressPodCDN := *testing.NewPodWithLabels(eipNamespace, podName, node1Name, podV4IP, egressPodLabel)
				egressPodUDN := *testing.NewPodWithLabels(eipNamespace2, podName2, node1Name, podV4IP2, egressPodLabel)
				setPrimaryNetworkAnnot(&egressPodUDN, util.GetNADName(eipNamespace2, nadName1), fmt.Sprintf("%s%s", podV4IP2, util.GetIPFullMaskString(podV4IP2)))

				nadNsName := util.GetNADName(eipNamespace2, nadName1)
				netconf := ovncnitypes.NetConf{
					NetConf: cnitypes.NetConf{
						Name: networkName1,
						Type: "ovn-k8s-cni-overlay",
					},
					Role:          ovntypes.NetworkRolePrimary,
					Topology:      ovntypes.Layer2Topology,
					NADName:       nadNsName,
					Subnets:       v4Net1,
					TransitSubnet: config.ClusterManager.V4TransitSubnet,
				}
				nad, err := newNetworkAttachmentDefinition(
					eipNamespace2,
					nadName1,
					netconf,
				)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				netInfo, err := util.NewNetInfo(&netconf)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				node1Annotations := map[string]string{
					"k8s.ovn.org/node-primary-ifaddr":             fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4CIDR, ""),
					"k8s.ovn.org/node-subnets":                    fmt.Sprintf("{\"default\":\"%s\",\"%s\":\"%s\"}", v4Node1Subnet, networkName1, v4Node1Net1),
					"k8s.ovn.org/node-transit-switch-port-ifaddr": fmt.Sprintf("{\"ipv4\":\"%s/16\"}", v4Node1Tsp),
					"k8s.ovn.org/node-chassis-id":                 "473ca66d-d800-472f-b289-1ab81ae7f21c",
					util.OvnNodeID:                                "1",
					util.OVNNodeHostCIDRs:                         fmt.Sprintf("[\"%s\"]", node1IPv4CIDR),
					util.OvnNodeL3GatewayConfig: fmt.Sprintf(`{"%s":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"%s", "next-hop":"%s", "next-hops": ["%s"]},
"default":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"192.168.126.12/24", "next-hop": "192.168.126.1", "next-hops": ["192.168.126.1"]}}`, networkName1, v4Net1, gwIP, gwIP),
					util.Layer2TopologyVersion: util.TransitRouterTopoVersion,
				}
				labels := map[string]string{
					"k8s.ovn.org/egress-assignable": "",
				}
				node1 := getNodeObj(node1Name, node1Annotations, labels)
				node2Annotations := map[string]string{
					"k8s.ovn.org/node-primary-ifaddr":             fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node2IPv4CIDR, ""),
					"k8s.ovn.org/node-subnets":                    fmt.Sprintf("{\"default\":\"%s\",\"%s\":\"%s\"}", v4Node2Subnet, networkName1, v4Node2Net1),
					"k8s.ovn.org/node-transit-switch-port-ifaddr": fmt.Sprintf("{\"ipv4\":\"%s/16\"}", v4Node2Tsp),
					"k8s.ovn.org/node-chassis-id":                 "473ca66d-d800-472f-b289-1ab81ae7f21c",
					util.OvnNodeID:                                "2",
					util.OVNNodeHostCIDRs:                         fmt.Sprintf("[\"%s\"]", node2IPv4CIDR),
					util.OvnNodeL3GatewayConfig: fmt.Sprintf(`{"%s":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"%s", "next-hop":"%s", "next-hops": ["%s"]},
"default":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"192.168.126.12/24", "next-hop": "192.168.126.1", "next-hops": ["192.168.126.1"]}}`, networkName1, v4Net1, gwIP, gwIP),
					util.Layer2TopologyVersion: util.TransitRouterTopoVersion,
				}
				node2 := getNodeObj(node2Name, node2Annotations, labels)
				twoNodeStatus := []egressipv1.EgressIPStatusItem{
					{
						Node:     node1Name,
						EgressIP: egressIP1,
					},
					{
						Node:     node2Name,
						EgressIP: egressIP2,
					},
				}
				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMetaWithMark(egressIPName, eIP1Mark),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{egressIP1, egressIP2},
						PodSelector: metav1.LabelSelector{
							MatchLabels: egressPodLabel,
						},
						NamespaceSelector: metav1.LabelSelector{
							MatchLabels: egressPodLabel,
						},
					},
					Status: egressipv1.EgressIPStatus{
						Items: twoNodeStatus,
					},
				}
				ginkgo.By("create EgressIP that doesnt select pods in a CDN and UDN")
				initialDB := []libovsdbtest.TestData{
					//CDN start
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name,
						Networks: []string{node1DefaultRtoJIPCIDR},
					},
					&nbdb.LogicalRouter{
						Name: ovntypes.OVNClusterRouter,
						UUID: ovntypes.OVNClusterRouter + "-UUID",
					},
					&nbdb.LogicalRouter{
						Name:  ovntypes.GWRouterPrefix + node1.Name,
						UUID:  ovntypes.GWRouterPrefix + node1.Name + "-UUID",
						Ports: []string{ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name + "-UUID"},
					},
					&nbdb.LogicalSwitchPort{
						UUID:      "k8s-" + node1Name + "-UUID",
						Name:      "k8s-" + node1Name,
						Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1CDNSubnet).IP.String()},
					},
					&nbdb.LogicalSwitch{
						UUID:  node1Name + "-UUID",
						Name:  node1Name,
						Ports: []string{"k8s-" + node1Name + "-UUID"},
					},
					// UDN start
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.RouterToTransitRouterPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name + "-UUID",
						Name:     ovntypes.RouterToTransitRouterPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name,
						Networks: []string{node1Network1JoinCIDR, node1Network1TransitCIDR},
					},
					&nbdb.LogicalRouter{
						Name:        netInfo.GetNetworkScopedClusterRouterName(),
						UUID:        netInfo.GetNetworkScopedClusterRouterName() + "-UUID",
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: networkName1, ovntypes.TopologyExternalID: ovntypes.Layer2Topology},
					},
					&nbdb.LogicalRouter{
						UUID:        netInfo.GetNetworkScopedGWRouterName(node1.Name) + "-UUID",
						Name:        netInfo.GetNetworkScopedGWRouterName(node1.Name),
						Ports:       []string{ovntypes.RouterToTransitRouterPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name + "-UUID"},
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: networkName1, ovntypes.TopologyExternalID: ovntypes.Layer2Topology},
					},
					&nbdb.LogicalSwitchPort{
						UUID:      "k8s-" + networkName1_ + node1Name + "-UUID",
						Name:      "k8s-" + networkName1_ + node1Name,
						Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1UDNSubnet).IP.String()},
					},
					&nbdb.LogicalSwitch{
						UUID:        networkName1_ + layer2SwitchName + "-UUID",
						Name:        networkName1_ + layer2SwitchName,
						Ports:       []string{"k8s-" + networkName1_ + node1Name + "-UUID"},
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: networkName1, ovntypes.TopologyExternalID: ovntypes.Layer2Topology},
					},
				}
				fakeOvn.startWithDBSetup(
					libovsdbtest.TestSetup{
						NBData: initialDB,
					},
					&corev1.NodeList{
						Items: []corev1.Node{node1, node2},
					},
					&corev1.NamespaceList{
						Items: []corev1.Namespace{*egressCDNNamespace, *egressUDNNamespace},
					},
					&corev1.PodList{
						Items: []corev1.Pod{egressPodCDN, egressPodUDN},
					},
					&nadv1.NetworkAttachmentDefinitionList{
						Items: []nadv1.NetworkAttachmentDefinition{*nad},
					},
					&egressipv1.EgressIPList{
						Items: []egressipv1.EgressIP{eIP},
					},
				)
				asf := addressset.NewOvnAddressSetFactory(fakeOvn.nbClient, true, false)
				// watch EgressIP depends on UDN enabled svcs address set being available
				c := udnenabledsvc.NewController(fakeOvn.nbClient, asf, fakeOvn.controller.watchFactory.ServiceCoreInformer(), []string{})
				go func() {
					gomega.Expect(c.Run(ctx.Done())).Should(gomega.Succeed())
				}()
				// Add pod IPs to CDN cache
				iCDN, nCDN, _ := net.ParseCIDR(podV4IP + "/23")
				nCDN.IP = iCDN
				fakeOvn.controller.logicalPortCache.add(&egressPodCDN, "", ovntypes.DefaultNetworkName, "", nil, []*net.IPNet{nCDN})
				fakeOvn.controller.nodeName = node1Name
				fakeOvn.controller.eIPC.nodeName = node1Name
				err = fakeOvn.eIPController.SyncNodeCache()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.networkManager.Start()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				defer fakeOvn.networkManager.Stop()
				err = fakeOvn.eIPController.ensureRouterPoliciesForNetwork(netInfo, &node1)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.eIPController.ensureSwitchPoliciesForNode(netInfo, node1Name)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.eIPController.ensureNetworkPending(networkName1)
				gomega.Expect(fakeOvn.eIPController.SyncNetworkReady(networkName1)).To(gomega.Succeed())
				err = fakeOvn.controller.WatchEgressIPPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIPNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressNodes()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				secConInfo, ok := fakeOvn.userDefinedNetworkControllers[networkName1]
				gomega.Expect(ok).To(gomega.BeTrue())
				// Add pod IPs to UDN cache
				iUDN, nUDN, _ := net.ParseCIDR(v4Pod1IPNode1Net1 + "/23")
				nUDN.IP = iUDN
				secConInfo.bnc.logicalPortCache.add(&egressPodUDN, "", util.GetNADName(nad.Namespace, nad.Name), "", nil, []*net.IPNet{nUDN})
				ginkgo.By("update namespaces with label so its now selected by EgressIP")
				egressCDNNamespace.Labels = egressPodLabel
				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Update(context.Background(), egressCDNNamespace, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				egressUDNNamespace.Labels = map[string]string{}
				for k, v := range egressPodLabel {
					egressUDNNamespace.Labels[k] = v
				}
				egressUDNNamespace.Labels[ovntypes.RequiredUDNNamespaceLabel] = ""
				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Update(context.Background(), egressUDNNamespace, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				egressSVCServedPodsASv4, _ := buildEgressServiceAddressSets(nil)
				egressIPServedPodsASCDNv4, _ := buildEgressIPServedPodsAddressSets([]string{podV4IP}, ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkControllerName)
				egressNodeIPsASv4, _ := buildEgressIPNodeAddressSets([]string{node1IPv4, node2IPv4})
				egressIPServedPodsASUDNv4, _ := buildEgressIPServedPodsAddressSetsForController([]string{v4Pod1IPNode1Net1}, netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName)
				gomega.Eventually(c.IsAddressSetAvailable).Should(gomega.BeTrue())
				dbIDs := udnenabledsvc.GetAddressSetDBIDs()
				udnEnabledSvcV4, _ := addressset.GetTestDbAddrSets(dbIDs, []string{})
				node1LRP := "k8s-node1"
				expectedDatabaseStateTwoEgressNodes := []libovsdbtest.TestData{
					// CDN
					getReRouteStaticRoute(v4ClusterSubnet, node1DefaultRtoJIP),
					getReRoutePolicy(podV4IP, "4", "reroute-UUID", []string{node1DefaultRtoJIP, v4Node2Tsp},
						getEgressIPLRPReRouteDbIDs(eIP.Name, egressPodCDN.Namespace, egressPodCDN.Name, IPFamilyValueV4, ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkControllerName).GetExternalIDs()),
					&nbdb.NAT{
						UUID:        "egressip-nat-UUID",
						LogicalIP:   podV4IP,
						ExternalIP:  egressIP1,
						ExternalIDs: getEgressIPNATDbIDs(egressIPName, egressPodCDN.Namespace, egressPodCDN.Name, IPFamilyValueV4, ovntypes.DefaultNetworkControllerName).GetExternalIDs(),
						Type:        nbdb.NATTypeSNAT,
						LogicalPort: &node1LRP,
						Options: map[string]string{
							"stateless": "false",
						},
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    ovntypes.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4ClusterSubnet, v4ClusterSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-no-reroute-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    ovntypes.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4ClusterSubnet, config.Gateway.V4JoinSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-service-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouter{
						Name:  ovntypes.GWRouterPrefix + node1.Name,
						UUID:  ovntypes.GWRouterPrefix + node1.Name + "-UUID",
						Ports: []string{ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name + "-UUID"},
						Nat:   []string{"egressip-nat-UUID"},
					},
					&nbdb.LogicalRouter{
						Name: ovntypes.OVNClusterRouter,
						UUID: ovntypes.OVNClusterRouter + "-UUID",
						Policies: []string{"default-no-reroute-UUID", "no-reroute-service-UUID",
							"default-no-reroute-node-UUID", "default-no-reroute-reply-traffic", "reroute-UUID"},
						StaticRoutes: []string{"reroute-static-route-UUID"},
					},
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name,
						Networks: []string{node1DefaultRtoJIPCIDR},
					},
					&nbdb.LogicalRouterPolicy{
						Priority: ovntypes.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
							egressIPServedPodsASCDNv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": ovntypes.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalSwitchPort{
						UUID:      "k8s-" + node1Name + "-UUID",
						Name:      "k8s-" + node1Name,
						Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1CDNSubnet).IP.String()},
					},
					&nbdb.LogicalSwitch{
						UUID:     node1Name + "-UUID",
						Name:     node1Name,
						Ports:    []string{"k8s-" + node1Name + "-UUID"},
						QOSRules: []string{"default-QoS-UUID"},
					},
					getNoReRouteReplyTrafficPolicy(ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkControllerName),
					getDefaultQoSRule(false, ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkControllerName),
					egressSVCServedPodsASv4,
					egressIPServedPodsASCDNv4,
					egressNodeIPsASv4,

					// UDN
					getReRoutePolicyForController(egressIPName, eipNamespace2, podName2, v4Pod1IPNode1Net1, eIP1Mark, IPFamilyValueV4, []string{node1Network1TransitIP, node2Network1TransitIP}, netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName),
					getGWPktMarkLRPForController(eIP1Mark, egressIPName, eipNamespace2, podName2, v4Pod1IPNode1Net1, IPFamilyValueV4,
						netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName),
					getNoReRoutePolicyForUDNEnabledSvc(false, netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName, egressIPServedPodsASUDNv4.Name, egressSVCServedPodsASv4.Name, udnEnabledSvcV4.Name),
					&nbdb.LogicalRouterPolicy{
						Priority:    ovntypes.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4Net1, v4Net1),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "udn-default-no-reroute-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    ovntypes.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4Net1, config.Gateway.V4JoinSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "udn-no-reroute-service-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority: ovntypes.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
							egressIPServedPodsASUDNv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "udn-default-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": ovntypes.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.RouterToTransitRouterPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name + "-UUID",
						Name:     ovntypes.RouterToTransitRouterPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name,
						Networks: []string{node1Network1JoinCIDR, node1Network1TransitCIDR},
					},
					&nbdb.LogicalRouter{
						Name:        netInfo.GetNetworkScopedClusterRouterName(),
						UUID:        netInfo.GetNetworkScopedClusterRouterName() + "-UUID",
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: netInfo.GetNetworkName(), ovntypes.TopologyExternalID: ovntypes.Layer2Topology},
						Policies: []string{
							"udn-default-no-reroute-node-UUID", "udn-default-no-reroute-UUID", "udn-no-reroute-service-UUID", "udn-enabled-svc-no-reroute-UUID",
							fmt.Sprintf("%s-no-reroute-reply-traffic", netInfo.GetNetworkName()),
							getReRoutePolicyUUID(eipNamespace2, podName2, IPFamilyValueV4, netInfo.GetNetworkName())},
					},
					&nbdb.LogicalRouter{
						UUID:        netInfo.GetNetworkScopedGWRouterName(node1.Name) + "-UUID",
						Name:        netInfo.GetNetworkScopedGWRouterName(node1.Name),
						Ports:       []string{ovntypes.RouterToTransitRouterPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name + "-UUID"},
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: netInfo.GetNetworkName(), ovntypes.TopologyExternalID: ovntypes.Layer2Topology},
						Policies:    []string{getGWPktMarkLRPUUID(eipNamespace2, podName2, IPFamilyValueV4, netInfo.GetNetworkName())},
					},
					&nbdb.LogicalSwitchPort{
						UUID:      "k8s-" + networkName1_ + node1Name + "-UUID",
						Name:      "k8s-" + networkName1_ + node1Name,
						Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1UDNSubnet).IP.String()},
					},
					&nbdb.LogicalSwitch{
						UUID:        networkName1_ + layer2SwitchName + "-UUID",
						Name:        networkName1_ + layer2SwitchName,
						Ports:       []string{"k8s-" + networkName1_ + node1Name + "-UUID"},
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: netInfo.GetNetworkName(), ovntypes.TopologyExternalID: ovntypes.Layer2Topology},
						QOSRules:    []string{fmt.Sprintf("%s-QoS-UUID", netInfo.GetNetworkName())},
					},
					getNoReRouteReplyTrafficPolicyForController(netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName),
					getDefaultQoSRule(false, netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName),
					egressIPServedPodsASUDNv4,
					udnEnabledSvcV4,
				}
				ginkgo.By("ensure expected equals actual")
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseStateTwoEgressNodes))
				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})

	ginkgo.Context("Namespace delete", func() {
		ginkgo.It("should delete UDN and CDN config", func() {
			// Test steps:
			// create an EIP selecting a pod on an UDN and another pod on a CDN
			// EIP egresses locally and remote
			// Delete namespace
			app.Action = func(ctx *cli.Context) error {
				// Node 1 is local, Node 2 is remote
				egressIP1 := "192.168.126.101"
				egressIP2 := "192.168.126.102"
				node1IPv4 := "192.168.126.202"
				node1IPv4CIDR := node1IPv4 + "/24"
				node2IPv4 := "192.168.126.51"
				node2IPv4CIDR := node2IPv4 + "/24"
				_, node1CDNSubnet, _ := net.ParseCIDR(v4Node1Subnet)
				_, node1UDNSubnet, _ := net.ParseCIDR(v4Node1Net1)
				egressCDNNamespace := testing.NewNamespaceWithLabels(eipNamespace, egressPodLabel)
				egressUDNNamespace := newUDNNamespaceWithLabels(eipNamespace2, egressPodLabel)
				egressPodCDN := *testing.NewPodWithLabels(eipNamespace, podName, node1Name, podV4IP, egressPodLabel)
				egressPodUDN := *testing.NewPodWithLabels(eipNamespace2, podName2, node1Name, podV4IP2, egressPodLabel)
				setPrimaryNetworkAnnot(&egressPodUDN, util.GetNADName(eipNamespace2, nadName1), fmt.Sprintf("%s%s", podV4IP2, util.GetIPFullMaskString(podV4IP2)))

				nadNsName := util.GetNADName(eipNamespace2, nadName1)
				netconf := ovncnitypes.NetConf{
					NetConf: cnitypes.NetConf{
						Name: networkName1,
						Type: "ovn-k8s-cni-overlay",
					},
					Role:          ovntypes.NetworkRolePrimary,
					Topology:      ovntypes.Layer2Topology,
					NADName:       nadNsName,
					Subnets:       v4Net1,
					TransitSubnet: config.ClusterManager.V4TransitSubnet,
				}
				nad, err := newNetworkAttachmentDefinition(
					eipNamespace2,
					nadName1,
					netconf,
				)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				netInfo, err := util.NewNetInfo(&netconf)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				node1Annotations := map[string]string{
					"k8s.ovn.org/node-primary-ifaddr":             fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4CIDR, ""),
					"k8s.ovn.org/node-subnets":                    fmt.Sprintf("{\"default\":\"%s\",\"%s\":\"%s\"}", v4Node1Subnet, networkName1, v4Node1Net1),
					"k8s.ovn.org/node-transit-switch-port-ifaddr": fmt.Sprintf("{\"ipv4\":\"%s/16\"}", v4Node1Tsp),
					"k8s.ovn.org/node-chassis-id":                 "473ca66d-d800-472f-b289-1ab81ae7f21c",
					util.OvnNodeID:                                "1",
					util.OVNNodeHostCIDRs:                         fmt.Sprintf("[\"%s\"]", node1IPv4CIDR),
					util.OvnNodeL3GatewayConfig: fmt.Sprintf(`{"%s":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"%s", "next-hop":"%s", "next-hops": ["%s"]},
"default":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"192.168.126.12/24", "next-hop": "192.168.126.1", "next-hops": ["192.168.126.1"]}}`, networkName1, v4Net1, gwIP, gwIP),
					util.Layer2TopologyVersion: util.TransitRouterTopoVersion,
				}
				labels := map[string]string{
					"k8s.ovn.org/egress-assignable": "",
				}
				node1 := getNodeObj(node1Name, node1Annotations, labels)
				node2Annotations := map[string]string{
					"k8s.ovn.org/node-primary-ifaddr":             fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node2IPv4CIDR, ""),
					"k8s.ovn.org/node-subnets":                    fmt.Sprintf("{\"default\":\"%s\",\"%s\":\"%s\"}", v4Node2Subnet, networkName1, v4Node2Net1),
					"k8s.ovn.org/node-transit-switch-port-ifaddr": fmt.Sprintf("{\"ipv4\":\"%s/16\"}", v4Node2Tsp),
					"k8s.ovn.org/node-chassis-id":                 "473ca66d-d800-472f-b289-1ab81ae7f21c",
					util.OvnNodeID:                                "2",
					util.OVNNodeHostCIDRs:                         fmt.Sprintf("[\"%s\"]", node2IPv4CIDR),
					util.OvnNodeL3GatewayConfig: fmt.Sprintf(`{"%s":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"%s", "next-hop":"%s", "next-hops": ["%s"]},
"default":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"192.168.126.12/24", "next-hop": "192.168.126.1", "next-hops": ["192.168.126.1"]}}`, networkName1, v4Net1, gwIP, gwIP),
					util.Layer2TopologyVersion: util.TransitRouterTopoVersion,
				}
				node2 := getNodeObj(node2Name, node2Annotations, labels)
				twoNodeStatus := []egressipv1.EgressIPStatusItem{
					{
						Node:     node1Name,
						EgressIP: egressIP1,
					},
					{
						Node:     node2Name,
						EgressIP: egressIP2,
					},
				}
				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMetaWithMark(egressIPName, eIP1Mark),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{egressIP1, egressIP2},
						PodSelector: metav1.LabelSelector{
							MatchLabels: egressPodLabel,
						},
						NamespaceSelector: metav1.LabelSelector{
							MatchLabels: egressPodLabel,
						},
					},
					Status: egressipv1.EgressIPStatus{
						Items: twoNodeStatus,
					},
				}
				ginkgo.By("create EgressIP that selects pods in a CDN and UDN")
				initialDB := []libovsdbtest.TestData{
					//CDN start
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name,
						Networks: []string{node1DefaultRtoJIPCIDR},
					},
					&nbdb.LogicalRouter{
						Name: ovntypes.OVNClusterRouter,
						UUID: ovntypes.OVNClusterRouter + "-UUID",
					},
					&nbdb.LogicalRouter{
						Name:  ovntypes.GWRouterPrefix + node1.Name,
						UUID:  ovntypes.GWRouterPrefix + node1.Name + "-UUID",
						Ports: []string{ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name + "-UUID"},
					},
					&nbdb.LogicalSwitchPort{
						UUID:      "k8s-" + node1Name + "-UUID",
						Name:      "k8s-" + node1Name,
						Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1CDNSubnet).IP.String()},
					},
					&nbdb.LogicalSwitch{
						UUID:  node1Name + "-UUID",
						Name:  node1Name,
						Ports: []string{"k8s-" + node1Name + "-UUID"},
					},
					// UDN start
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.RouterToTransitRouterPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name + "-UUID",
						Name:     ovntypes.RouterToTransitRouterPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name,
						Networks: []string{node1Network1JoinCIDR, node1Network1TransitCIDR},
					},
					&nbdb.LogicalRouter{
						Name:        netInfo.GetNetworkScopedClusterRouterName(),
						UUID:        netInfo.GetNetworkScopedClusterRouterName() + "-UUID",
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: networkName1, ovntypes.TopologyExternalID: ovntypes.Layer2Topology},
					},
					&nbdb.LogicalRouter{
						UUID:        netInfo.GetNetworkScopedGWRouterName(node1.Name) + "-UUID",
						Name:        netInfo.GetNetworkScopedGWRouterName(node1.Name),
						Ports:       []string{ovntypes.RouterToTransitRouterPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name + "-UUID"},
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: networkName1, ovntypes.TopologyExternalID: ovntypes.Layer2Topology},
					},
					&nbdb.LogicalSwitchPort{
						UUID:      "k8s-" + networkName1_ + node1Name + "-UUID",
						Name:      "k8s-" + networkName1_ + node1Name,
						Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1UDNSubnet).IP.String()},
					},
					&nbdb.LogicalSwitch{
						UUID:        networkName1_ + layer2SwitchName + "-UUID",
						Name:        networkName1_ + layer2SwitchName,
						Ports:       []string{"k8s-" + networkName1_ + node1Name + "-UUID"},
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: networkName1, ovntypes.TopologyExternalID: ovntypes.Layer2Topology},
					},
				}
				fakeOvn.startWithDBSetup(
					libovsdbtest.TestSetup{
						NBData: initialDB,
					},
					&corev1.NodeList{
						Items: []corev1.Node{node1, node2},
					},
					&corev1.NamespaceList{
						Items: []corev1.Namespace{*egressCDNNamespace, *egressUDNNamespace},
					},
					&corev1.PodList{
						Items: []corev1.Pod{egressPodCDN, egressPodUDN},
					},
					&nadv1.NetworkAttachmentDefinitionList{
						Items: []nadv1.NetworkAttachmentDefinition{*nad},
					},
					&egressipv1.EgressIPList{
						Items: []egressipv1.EgressIP{eIP},
					},
				)
				asf := addressset.NewOvnAddressSetFactory(fakeOvn.nbClient, true, false)
				// watch EgressIP depends on UDN enabled svcs address set being available
				c := udnenabledsvc.NewController(fakeOvn.nbClient, asf, fakeOvn.controller.watchFactory.ServiceCoreInformer(), []string{})
				go func() {
					gomega.Expect(c.Run(ctx.Done())).Should(gomega.Succeed())
				}()
				// Add pod IPs to CDN cache
				iCDN, nCDN, _ := net.ParseCIDR(podV4IP + "/23")
				nCDN.IP = iCDN
				fakeOvn.controller.logicalPortCache.add(&egressPodCDN, "", ovntypes.DefaultNetworkName, "", nil, []*net.IPNet{nCDN})
				fakeOvn.controller.nodeName = node1Name
				fakeOvn.eIPController.nodeName = node1Name
				gomega.Expect(fakeOvn.eIPController.SyncNodeCache()).To(gomega.Succeed())
				// Add pod IPs to UDN cache
				iUDN, nUDN, _ := net.ParseCIDR(v4Pod1IPNode1Net1 + "/23")
				nUDN.IP = iUDN
				fakeOvn.controller.logicalPortCache.add(&egressPodUDN, "", util.GetNADName(nad.Namespace, nad.Name), "", nil, []*net.IPNet{nUDN})
				err = fakeOvn.networkManager.Start()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				defer fakeOvn.networkManager.Stop()
				err = fakeOvn.eIPController.ensureRouterPoliciesForNetwork(netInfo, &node1)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.eIPController.ensureSwitchPoliciesForNode(netInfo, node1Name)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.eIPController.ensureNetworkPending(networkName1)
				gomega.Expect(fakeOvn.eIPController.SyncNetworkReady(networkName1)).To(gomega.Succeed())
				err = fakeOvn.controller.WatchEgressIPPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIPNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressNodes()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				egressSVCServedPodsASv4, _ := buildEgressServiceAddressSets(nil)
				egressIPServedPodsASCDNv4, _ := buildEgressIPServedPodsAddressSets([]string{podV4IP}, ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkControllerName)
				egressNodeIPsASv4, _ := buildEgressIPNodeAddressSets([]string{node1IPv4, node2IPv4})
				egressIPServedPodsASUDNv4, _ := buildEgressIPServedPodsAddressSetsForController([]string{v4Pod1IPNode1Net1}, netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName)
				gomega.Eventually(c.IsAddressSetAvailable).Should(gomega.BeTrue())
				dbIDs := udnenabledsvc.GetAddressSetDBIDs()
				udnEnabledSvcV4, _ := addressset.GetTestDbAddrSets(dbIDs, []string{})
				node1LRP := "k8s-node1"
				expectedDatabaseStateTwoEgressNodes := []libovsdbtest.TestData{
					// CDN
					getReRouteStaticRoute(v4ClusterSubnet, node1DefaultRtoJIP),
					getReRoutePolicy(podV4IP, "4", "reroute-UUID", []string{node1DefaultRtoJIP, v4Node2Tsp},
						getEgressIPLRPReRouteDbIDs(eIP.Name, egressPodCDN.Namespace, egressPodCDN.Name, IPFamilyValueV4, ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkControllerName).GetExternalIDs()),
					&nbdb.NAT{
						UUID:        "egressip-nat-UUID",
						LogicalIP:   podV4IP,
						ExternalIP:  egressIP1,
						ExternalIDs: getEgressIPNATDbIDs(egressIPName, egressPodCDN.Namespace, egressPodCDN.Name, IPFamilyValueV4, ovntypes.DefaultNetworkControllerName).GetExternalIDs(),
						Type:        nbdb.NATTypeSNAT,
						LogicalPort: &node1LRP,
						Options: map[string]string{
							"stateless": "false",
						},
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    ovntypes.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4ClusterSubnet, v4ClusterSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-no-reroute-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    ovntypes.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4ClusterSubnet, config.Gateway.V4JoinSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-service-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouter{
						Name:  ovntypes.GWRouterPrefix + node1.Name,
						UUID:  ovntypes.GWRouterPrefix + node1.Name + "-UUID",
						Ports: []string{ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name + "-UUID"},
						Nat:   []string{"egressip-nat-UUID"},
					},
					&nbdb.LogicalRouter{
						Name: ovntypes.OVNClusterRouter,
						UUID: ovntypes.OVNClusterRouter + "-UUID",
						Policies: []string{"default-no-reroute-UUID", "no-reroute-service-UUID",
							"default-no-reroute-node-UUID", "default-no-reroute-reply-traffic", "reroute-UUID"},
						StaticRoutes: []string{"reroute-static-route-UUID"},
					},
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name,
						Networks: []string{node1DefaultRtoJIPCIDR},
					},
					&nbdb.LogicalRouterPolicy{
						Priority: ovntypes.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
							egressIPServedPodsASCDNv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": ovntypes.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalSwitchPort{
						UUID:      "k8s-" + node1Name + "-UUID",
						Name:      "k8s-" + node1Name,
						Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1CDNSubnet).IP.String()},
					},
					&nbdb.LogicalSwitch{
						UUID:     node1Name + "-UUID",
						Name:     node1Name,
						Ports:    []string{"k8s-" + node1Name + "-UUID"},
						QOSRules: []string{"default-QoS-UUID"},
					},
					getNoReRouteReplyTrafficPolicy(ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkControllerName),
					getDefaultQoSRule(false, ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkControllerName),
					egressSVCServedPodsASv4,
					egressIPServedPodsASCDNv4,
					egressNodeIPsASv4,

					// UDN
					getReRoutePolicyForController(egressIPName, eipNamespace2, podName2, v4Pod1IPNode1Net1, eIP1Mark, IPFamilyValueV4, []string{node1Network1TransitIP, node2Network1TransitIP}, netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName),
					getGWPktMarkLRPForController(eIP1Mark, egressIPName, eipNamespace2, podName2, v4Pod1IPNode1Net1, IPFamilyValueV4,
						netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName),
					getNoReRoutePolicyForUDNEnabledSvc(false, netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName, egressIPServedPodsASUDNv4.Name, egressSVCServedPodsASv4.Name, udnEnabledSvcV4.Name),
					&nbdb.LogicalRouterPolicy{
						Priority:    ovntypes.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4Net1, v4Net1),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "udn-default-no-reroute-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    ovntypes.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4Net1, config.Gateway.V4JoinSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "udn-no-reroute-service-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority: ovntypes.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
							egressIPServedPodsASUDNv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "udn-default-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": ovntypes.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.RouterToTransitRouterPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name + "-UUID",
						Name:     ovntypes.RouterToTransitRouterPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name,
						Networks: []string{node1Network1JoinCIDR, node1Network1TransitCIDR},
					},
					&nbdb.LogicalRouter{
						Name:        netInfo.GetNetworkScopedClusterRouterName(),
						UUID:        netInfo.GetNetworkScopedClusterRouterName() + "-UUID",
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: netInfo.GetNetworkName(), ovntypes.TopologyExternalID: ovntypes.Layer2Topology},
						Policies: []string{
							"udn-default-no-reroute-node-UUID", "udn-default-no-reroute-UUID", "udn-no-reroute-service-UUID", "udn-enabled-svc-no-reroute-UUID",
							fmt.Sprintf("%s-no-reroute-reply-traffic", netInfo.GetNetworkName()),
							getReRoutePolicyUUID(eipNamespace2, podName2, IPFamilyValueV4, netInfo.GetNetworkName())},
					},
					&nbdb.LogicalRouter{
						UUID:        netInfo.GetNetworkScopedGWRouterName(node1.Name) + "-UUID",
						Name:        netInfo.GetNetworkScopedGWRouterName(node1.Name),
						Ports:       []string{ovntypes.RouterToTransitRouterPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name + "-UUID"},
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: netInfo.GetNetworkName(), ovntypes.TopologyExternalID: ovntypes.Layer2Topology},
						Policies:    []string{getGWPktMarkLRPUUID(eipNamespace2, podName2, IPFamilyValueV4, netInfo.GetNetworkName())},
					},
					&nbdb.LogicalSwitchPort{
						UUID:      "k8s-" + networkName1_ + node1Name + "-UUID",
						Name:      "k8s-" + networkName1_ + node1Name,
						Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1UDNSubnet).IP.String()},
					},
					&nbdb.LogicalSwitch{
						UUID:        networkName1_ + layer2SwitchName + "-UUID",
						Name:        networkName1_ + layer2SwitchName,
						Ports:       []string{"k8s-" + networkName1_ + node1Name + "-UUID"},
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: netInfo.GetNetworkName(), ovntypes.TopologyExternalID: ovntypes.Layer2Topology},
						QOSRules:    []string{fmt.Sprintf("%s-QoS-UUID", netInfo.GetNetworkName())},
					},
					getNoReRouteReplyTrafficPolicyForController(netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName),
					getDefaultQoSRule(false, netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName),
					egressIPServedPodsASUDNv4,
					udnEnabledSvcV4,
				}
				ginkgo.By("ensure expected equals actual")
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseStateTwoEgressNodes))
				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})

	ginkgo.Context("Pod update", func() {
		ginkgo.It("should update UDN and CDN config", func() {
			// Test steps:
			// create an EIP no pods
			// Create multiple pods, some selected by EIP selectors and some not
			// EIP egresses locally and remote
			app.Action = func(ctx *cli.Context) error {
				// Node 1 is local, Node 2 is remote
				egressIP1 := "192.168.126.101"
				egressIP2 := "192.168.126.102"
				node1IPv4 := "192.168.126.202"
				node1IPv4CIDR := node1IPv4 + "/24"
				node2IPv4 := "192.168.126.51"
				node2IPv4CIDR := node2IPv4 + "/24"
				_, node1CDNSubnet, _ := net.ParseCIDR(v4Node1Subnet)
				_, node1UDNSubnet, _ := net.ParseCIDR(v4Node1Net1)
				nadName := util.GetNADName(eipNamespace2, nadName1)
				egressCDNNamespace := testing.NewNamespaceWithLabels(eipNamespace, egressPodLabel)
				egressUDNNamespace := newUDNNamespaceWithLabels(eipNamespace2, egressPodLabel)
				egressPodCDNLocal := *testing.NewPodWithLabels(eipNamespace, podName, node1Name, podV4IP, nil)
				egressPodUDNLocal := *testing.NewPodWithLabels(eipNamespace2, podName2, node1Name, v4Pod1IPNode1Net1, nil)
				setPrimaryNetworkAnnot(&egressPodUDNLocal, nadName, fmt.Sprintf("%s%s", v4Pod1IPNode1Net1, util.GetIPFullMaskString(v4Pod1IPNode1Net1)))
				egressPodCDNRemote := *testing.NewPodWithLabels(eipNamespace, podName3, node2Name, podV4IP2, egressPodLabel)
				setPrimaryNetworkAnnot(&egressPodCDNRemote, ovntypes.DefaultNetworkName, fmt.Sprintf("%s%s", podV4IP2, util.GetIPFullMaskString(podV4IP2)))
				egressPodUDNRemote := *testing.NewPodWithLabels(eipNamespace2, podName4, node2Name, v4Pod2IPNode2Net1, egressPodLabel)
				setPrimaryNetworkAnnot(&egressPodUDNRemote, nadName, fmt.Sprintf("%s%s", v4Pod2IPNode2Net1, util.GetIPFullMaskString(v4Pod2IPNode2Net1)))
				netconf := ovncnitypes.NetConf{
					NetConf: cnitypes.NetConf{
						Name: networkName1,
						Type: "ovn-k8s-cni-overlay",
					},
					Role:          ovntypes.NetworkRolePrimary,
					Topology:      ovntypes.Layer2Topology,
					NADName:       nadName,
					Subnets:       v4Net1,
					TransitSubnet: config.ClusterManager.V4TransitSubnet,
				}
				nad, err := newNetworkAttachmentDefinition(
					eipNamespace2,
					nadName1,
					netconf,
				)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				netInfo, err := util.NewNetInfo(&netconf)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				node1Annotations := map[string]string{
					"k8s.ovn.org/node-primary-ifaddr":             fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4CIDR, ""),
					"k8s.ovn.org/node-subnets":                    fmt.Sprintf("{\"default\":\"%s\",\"%s\":\"%s\"}", v4Node1Subnet, networkName1, v4Node1Net1),
					"k8s.ovn.org/node-transit-switch-port-ifaddr": fmt.Sprintf("{\"ipv4\":\"%s/16\"}", v4Node1Tsp),
					"k8s.ovn.org/node-chassis-id":                 "473ca66d-d800-472f-b289-1ab81ae7f21c",
					util.OvnNodeID:                                "1",
					util.OVNNodeHostCIDRs:                         fmt.Sprintf("[\"%s\"]", node1IPv4CIDR),
					util.OvnNodeL3GatewayConfig: fmt.Sprintf(`{"%s":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"%s", "next-hop":"%s", "next-hops": ["%s"]},
"default":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"192.168.126.12/24", "next-hop": "192.168.126.1", "next-hops": ["192.168.126.1"]}}`, networkName1, v4Net1, gwIP, gwIP),
					util.Layer2TopologyVersion: util.TransitRouterTopoVersion,
				}
				labels := map[string]string{
					"k8s.ovn.org/egress-assignable": "",
				}
				node1 := getNodeObj(node1Name, node1Annotations, labels)
				node2Annotations := map[string]string{
					"k8s.ovn.org/node-primary-ifaddr":             fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node2IPv4CIDR, ""),
					"k8s.ovn.org/node-subnets":                    fmt.Sprintf("{\"default\":\"%s\",\"%s\":\"%s\"}", v4Node2Subnet, networkName1, v4Node2Net1),
					"k8s.ovn.org/node-transit-switch-port-ifaddr": fmt.Sprintf("{\"ipv4\":\"%s/16\"}", v4Node2Tsp),
					"k8s.ovn.org/node-chassis-id":                 "473ca66d-d800-472f-b289-1ab81ae7f21c",
					util.OvnNodeID:                                "2",
					util.OVNNodeHostCIDRs:                         fmt.Sprintf("[\"%s\"]", node2IPv4CIDR),
					util.OvnNodeL3GatewayConfig: fmt.Sprintf(`{"%s":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"%s", "next-hop":"%s", "next-hops": ["%s"]},
"default":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"192.168.126.12/24", "next-hop": "192.168.126.1", "next-hops": ["192.168.126.1"]}}`, networkName1, v4Net1, gwIP, gwIP),
					util.Layer2TopologyVersion: util.TransitRouterTopoVersion,
				}
				node2 := getNodeObj(node2Name, node2Annotations, labels)
				twoNodeStatus := []egressipv1.EgressIPStatusItem{
					{
						Node:     node1Name,
						EgressIP: egressIP1,
					},
					{
						Node:     node2Name,
						EgressIP: egressIP2,
					},
				}
				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMetaWithMark(egressIPName, eIP1Mark),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{egressIP1, egressIP2},
						PodSelector: metav1.LabelSelector{
							MatchLabels: egressPodLabel,
						},
						NamespaceSelector: metav1.LabelSelector{
							MatchLabels: egressPodLabel,
						},
					},
					Status: egressipv1.EgressIPStatus{
						Items: twoNodeStatus,
					},
				}
				ginkgo.By("create EgressIP that doesnt select pods in a CDN and UDN")
				initialDB := []libovsdbtest.TestData{
					//CDN start
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name,
						Networks: []string{node1DefaultRtoJIPCIDR},
					},
					&nbdb.LogicalRouter{
						Name: ovntypes.OVNClusterRouter,
						UUID: ovntypes.OVNClusterRouter + "-UUID",
					},
					&nbdb.LogicalRouter{
						Name:  ovntypes.GWRouterPrefix + node1.Name,
						UUID:  ovntypes.GWRouterPrefix + node1.Name + "-UUID",
						Ports: []string{ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name + "-UUID"},
					},
					&nbdb.LogicalSwitchPort{
						UUID:      "k8s-" + node1Name + "-UUID",
						Name:      "k8s-" + node1Name,
						Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1CDNSubnet).IP.String()},
					},
					&nbdb.LogicalSwitch{
						UUID:  node1Name + "-UUID",
						Name:  node1Name,
						Ports: []string{"k8s-" + node1Name + "-UUID"},
					},
					// UDN start
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.RouterToTransitRouterPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name + "-UUID",
						Name:     ovntypes.RouterToTransitRouterPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name,
						Networks: []string{node1Network1JoinCIDR, node1Network1TransitCIDR},
					},
					&nbdb.LogicalRouter{
						Name:        netInfo.GetNetworkScopedClusterRouterName(),
						UUID:        netInfo.GetNetworkScopedClusterRouterName() + "-UUID",
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: networkName1, ovntypes.TopologyExternalID: ovntypes.Layer2Topology},
					},
					&nbdb.LogicalRouter{
						UUID:        netInfo.GetNetworkScopedGWRouterName(node1.Name) + "-UUID",
						Name:        netInfo.GetNetworkScopedGWRouterName(node1.Name),
						Ports:       []string{ovntypes.RouterToTransitRouterPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name + "-UUID"},
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: networkName1, ovntypes.TopologyExternalID: ovntypes.Layer2Topology},
					},
					&nbdb.LogicalSwitchPort{
						UUID:      "k8s-" + networkName1_ + node1Name + "-UUID",
						Name:      "k8s-" + networkName1_ + node1Name,
						Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1UDNSubnet).IP.String()},
					},
					&nbdb.LogicalSwitch{
						UUID:        netInfo.GetNetworkScopedSwitchName(node1.Name) + "-UUID",
						Name:        netInfo.GetNetworkScopedSwitchName(node1.Name),
						Ports:       []string{"k8s-" + networkName1_ + node1Name + "-UUID"},
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: networkName1, ovntypes.TopologyExternalID: ovntypes.Layer2Topology},
					},
				}
				fakeOvn.startWithDBSetup(
					libovsdbtest.TestSetup{
						NBData: initialDB,
					},
					&corev1.NodeList{
						Items: []corev1.Node{node1, node2},
					},
					&corev1.NamespaceList{
						Items: []corev1.Namespace{*egressCDNNamespace, *egressUDNNamespace},
					},
					&corev1.PodList{
						Items: []corev1.Pod{egressPodCDNLocal, egressPodUDNLocal, egressPodCDNRemote, egressPodUDNRemote},
					},
					&nadv1.NetworkAttachmentDefinitionList{
						Items: []nadv1.NetworkAttachmentDefinition{*nad},
					},
					&egressipv1.EgressIPList{
						Items: []egressipv1.EgressIP{eIP},
					},
				)
				asf := addressset.NewOvnAddressSetFactory(fakeOvn.nbClient, true, false)
				// watch EgressIP depends on UDN enabled svcs address set being available
				c := udnenabledsvc.NewController(fakeOvn.nbClient, asf, fakeOvn.controller.watchFactory.ServiceCoreInformer(), []string{})
				go func() {
					gomega.Expect(c.Run(ctx.Done())).Should(gomega.Succeed())
				}()
				// Add pod IPs to CDN cache
				iCDN, nCDN, _ := net.ParseCIDR(podV4IP + "/23")
				nCDN.IP = iCDN
				fakeOvn.controller.logicalPortCache.add(&egressPodCDNLocal, "", ovntypes.DefaultNetworkName, "", nil, []*net.IPNet{nCDN})
				fakeOvn.controller.nodeName = node1Name
				fakeOvn.eIPController.nodeName = node1Name
				err = fakeOvn.eIPController.SyncNodeCache()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.networkManager.Start()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				defer fakeOvn.networkManager.Stop()
				fakeOvn.eIPController.ensureNetworkPending(networkName1)
				gomega.Expect(fakeOvn.eIPController.SyncNetworkReady(networkName1)).To(gomega.Succeed())
				err = fakeOvn.controller.WatchEgressIPPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIPNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressNodes()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				secConInfo, ok := fakeOvn.userDefinedNetworkControllers[networkName1]
				gomega.Expect(ok).To(gomega.BeTrue())
				// Add pod IPs to UDN cache
				iUDN, nUDN, _ := net.ParseCIDR(v4Pod1IPNode1Net1 + "/23")
				nUDN.IP = iUDN
				secConInfo.bnc.logicalPortCache.add(&egressPodUDNLocal, "", util.GetNADName(nad.Namespace, nad.Name), "", nil, []*net.IPNet{nUDN})
				ginkgo.By("update pod with label so its now selected by EgressIP")
				egressPodCDNLocal.Labels = egressPodLabel
				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(eipNamespace).Update(context.Background(), &egressPodCDNLocal, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				egressPodUDNLocal.Labels = egressPodLabel
				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(eipNamespace2).Update(context.Background(), &egressPodUDNLocal, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				egressSVCServedPodsASv4, _ := buildEgressServiceAddressSets(nil)
				egressIPServedPodsASCDNv4, _ := buildEgressIPServedPodsAddressSets([]string{podV4IP}, ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkControllerName)
				egressNodeIPsASv4, _ := buildEgressIPNodeAddressSets([]string{node1IPv4, node2IPv4})
				egressIPServedPodsASUDNv4, _ := buildEgressIPServedPodsAddressSetsForController([]string{v4Pod1IPNode1Net1}, netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName)
				gomega.Eventually(c.IsAddressSetAvailable).Should(gomega.BeTrue())
				dbIDs := udnenabledsvc.GetAddressSetDBIDs()
				udnEnabledSvcV4, _ := addressset.GetTestDbAddrSets(dbIDs, []string{})
				node1LRP := "k8s-node1"
				expectedDatabaseStateTwoEgressNodes := []libovsdbtest.TestData{
					// CDN
					getReRouteStaticRoute(v4ClusterSubnet, node1DefaultRtoJIP),
					getReRoutePolicy(podV4IP, "4", "reroute-UUID", []string{node1DefaultRtoJIP, v4Node2Tsp},
						getEgressIPLRPReRouteDbIDs(eIP.Name, egressPodCDNLocal.Namespace, egressPodCDNLocal.Name, IPFamilyValueV4, ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkControllerName).GetExternalIDs()),
					&nbdb.LogicalRouterPolicy{
						Priority:    ovntypes.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4ClusterSubnet, v4ClusterSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-no-reroute-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    ovntypes.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4ClusterSubnet, config.Gateway.V4JoinSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-service-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouter{
						Name:  ovntypes.GWRouterPrefix + node1.Name,
						UUID:  ovntypes.GWRouterPrefix + node1.Name + "-UUID",
						Ports: []string{ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name + "-UUID"},
						Nat:   []string{"egressip-nat-UUID", "egressip-nat2-UUID"},
					},
					&nbdb.LogicalRouter{
						Name: ovntypes.OVNClusterRouter,
						UUID: ovntypes.OVNClusterRouter + "-UUID",
						Policies: []string{"default-no-reroute-UUID", "no-reroute-service-UUID",
							"default-no-reroute-node-UUID", "default-no-reroute-reply-traffic", "reroute-UUID"},
						StaticRoutes: []string{"reroute-static-route-UUID"},
					},
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name,
						Networks: []string{node1DefaultRtoJIPCIDR},
					},
					&nbdb.LogicalRouterPolicy{
						Priority: ovntypes.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
							egressIPServedPodsASCDNv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": ovntypes.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalSwitchPort{
						UUID:      "k8s-" + node1Name + "-UUID",
						Name:      "k8s-" + node1Name,
						Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1CDNSubnet).IP.String()},
					},
					&nbdb.LogicalSwitch{
						UUID:     node1Name + "-UUID",
						Name:     node1Name,
						Ports:    []string{"k8s-" + node1Name + "-UUID"},
						QOSRules: []string{"default-QoS-UUID"},
					},
					&nbdb.NAT{
						UUID:        "egressip-nat-UUID",
						LogicalIP:   podV4IP2,
						ExternalIP:  egressIP1,
						ExternalIDs: getEgressIPNATDbIDs(egressIPName, egressPodCDNRemote.Namespace, egressPodCDNRemote.Name, IPFamilyValueV4, ovntypes.DefaultNetworkControllerName).GetExternalIDs(),
						Type:        nbdb.NATTypeSNAT,
						LogicalPort: &node1LRP,
						Options: map[string]string{
							"stateless": "false",
						},
					},
					&nbdb.NAT{
						UUID:        "egressip-nat2-UUID",
						LogicalIP:   podV4IP,
						ExternalIP:  egressIP1,
						ExternalIDs: getEgressIPNATDbIDs(egressIPName, egressPodCDNLocal.Namespace, egressPodCDNLocal.Name, IPFamilyValueV4, ovntypes.DefaultNetworkControllerName).GetExternalIDs(),
						Type:        nbdb.NATTypeSNAT,
						LogicalPort: &node1LRP,
						Options: map[string]string{
							"stateless": "false",
						},
					},
					getNoReRouteReplyTrafficPolicy(ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkControllerName),
					getDefaultQoSRule(false, ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkControllerName),
					egressSVCServedPodsASv4,
					egressIPServedPodsASCDNv4,
					egressNodeIPsASv4,

					// UDN
					getReRoutePolicyForController(egressIPName, eipNamespace2, podName2, v4Pod1IPNode1Net1, eIP1Mark, IPFamilyValueV4, []string{node1Network1TransitIP, node2Network1TransitIP}, netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName),
					getReRoutePolicyForController(egressIPName, eipNamespace2, podName4, v4Pod2IPNode2Net1, eIP1Mark, IPFamilyValueV4, []string{node1Network1TransitIP}, netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName),
					getGWPktMarkLRPForController(eIP1Mark, egressIPName, eipNamespace2, podName2, v4Pod1IPNode1Net1, IPFamilyValueV4,
						netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName),
					getGWPktMarkLRPForController(eIP1Mark, egressIPName, eipNamespace2, podName4, v4Pod2IPNode2Net1, IPFamilyValueV4,
						netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName),
					getNoReRoutePolicyForUDNEnabledSvc(false, netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName, egressIPServedPodsASUDNv4.Name, egressSVCServedPodsASv4.Name, udnEnabledSvcV4.Name),
					&nbdb.LogicalRouterPolicy{
						Priority:    ovntypes.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4Net1, v4Net1),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "udn-default-no-reroute-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    ovntypes.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4Net1, config.Gateway.V4JoinSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "udn-no-reroute-service-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority: ovntypes.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
							egressIPServedPodsASUDNv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "udn-default-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": ovntypes.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.RouterToTransitRouterPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name + "-UUID",
						Name:     ovntypes.RouterToTransitRouterPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name,
						Networks: []string{node1Network1JoinCIDR, node1Network1TransitCIDR},
					},
					&nbdb.LogicalRouter{
						Name:        netInfo.GetNetworkScopedClusterRouterName(),
						UUID:        netInfo.GetNetworkScopedClusterRouterName() + "-UUID",
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: netInfo.GetNetworkName(), ovntypes.TopologyExternalID: ovntypes.Layer2Topology},
						Policies: []string{
							"udn-default-no-reroute-node-UUID", "udn-default-no-reroute-UUID", "udn-no-reroute-service-UUID", "udn-enabled-svc-no-reroute-UUID",
							fmt.Sprintf("%s-no-reroute-reply-traffic", netInfo.GetNetworkName()),
							getReRoutePolicyUUID(eipNamespace2, podName2, IPFamilyValueV4, netInfo.GetNetworkName()),
							getReRoutePolicyUUID(eipNamespace2, podName4, IPFamilyValueV4, netInfo.GetNetworkName())},
					},
					&nbdb.LogicalRouter{
						UUID:        netInfo.GetNetworkScopedGWRouterName(node1.Name) + "-UUID",
						Name:        netInfo.GetNetworkScopedGWRouterName(node1.Name),
						Ports:       []string{ovntypes.RouterToTransitRouterPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name + "-UUID"},
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: netInfo.GetNetworkName(), ovntypes.TopologyExternalID: ovntypes.Layer2Topology},
						Policies: []string{getGWPktMarkLRPUUID(eipNamespace2, podName2, IPFamilyValueV4, netInfo.GetNetworkName()),
							getGWPktMarkLRPUUID(eipNamespace2, podName4, IPFamilyValueV4, netInfo.GetNetworkName())},
					},
					&nbdb.LogicalSwitchPort{
						UUID:      "k8s-" + networkName1_ + node1Name + "-UUID",
						Name:      "k8s-" + networkName1_ + node1Name,
						Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1UDNSubnet).IP.String()},
					},
					&nbdb.LogicalSwitch{
						UUID:        networkName1_ + layer2SwitchName + "-UUID",
						Name:        networkName1_ + layer2SwitchName,
						Ports:       []string{"k8s-" + networkName1_ + node1Name + "-UUID"},
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: netInfo.GetNetworkName(), ovntypes.TopologyExternalID: ovntypes.Layer2Topology},
						QOSRules:    []string{fmt.Sprintf("%s-QoS-UUID", netInfo.GetNetworkName())},
					},
					getNoReRouteReplyTrafficPolicyForController(netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName),
					getDefaultQoSRule(false, netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName),
					egressIPServedPodsASUDNv4,
					udnEnabledSvcV4,
				}
				ginkgo.By("ensure expected equals actual")
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseStateTwoEgressNodes))
				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})

	ginkgo.Context("Pod delete", func() {
		ginkgo.It("should delete UDN and CDN config", func() {
			// create a single EIP IP selecting multiple pods both local and remote.
			// Delete pods and ensure OVN DB is as expected
			app.Action = func(ctx *cli.Context) error {
				// Node 1 is local, Node 2 is remote
				egressIP := "192.168.126.101"
				node1IPv4 := "192.168.126.202"
				node1IPv4CIDR := node1IPv4 + "/24"
				node2IPv4 := "192.168.126.51"
				node2IPv4CIDR := node2IPv4 + "/24"
				_, node1CDNSubnet, _ := net.ParseCIDR(v4Node1Subnet)
				_, node1UDNSubnet, _ := net.ParseCIDR(v4Node1Net1)
				nadName := util.GetNADName(eipNamespace2, nadName1)
				egressCDNNamespace := testing.NewNamespaceWithLabels(eipNamespace, egressPodLabel)
				egressUDNNamespace := newUDNNamespaceWithLabels(eipNamespace2, egressPodLabel)
				egressPodCDNLocal := *testing.NewPodWithLabels(eipNamespace, podName, node1Name, podV4IP, egressPodLabel)
				egressPodUDNLocal := *testing.NewPodWithLabels(eipNamespace2, podName2, node1Name, v4Pod1IPNode1Net1, egressPodLabel)
				setPrimaryNetworkAnnot(&egressPodUDNLocal, nadName, fmt.Sprintf("%s%s", v4Pod1IPNode1Net1, util.GetIPFullMaskString(v4Pod1IPNode1Net1)))
				egressPodCDNRemote := *testing.NewPodWithLabels(eipNamespace, podName3, node2Name, podV4IP2, egressPodLabel)
				setPrimaryNetworkAnnot(&egressPodCDNRemote, ovntypes.DefaultNetworkName, fmt.Sprintf("%s%s", podV4IP2, util.GetIPFullMaskString(podV4IP2)))
				egressPodUDNRemote := *testing.NewPodWithLabels(eipNamespace2, podName4, node2Name, v4Pod2IPNode2Net1, egressPodLabel)
				setPrimaryNetworkAnnot(&egressPodUDNRemote, nadName, fmt.Sprintf("%s%s", v4Pod2IPNode2Net1, util.GetIPFullMaskString(v4Pod2IPNode2Net1)))
				netconf := ovncnitypes.NetConf{
					NetConf: cnitypes.NetConf{
						Name: networkName1,
						Type: "ovn-k8s-cni-overlay",
					},
					Role:          ovntypes.NetworkRolePrimary,
					Topology:      ovntypes.Layer2Topology,
					NADName:       nadName,
					Subnets:       v4Net1,
					TransitSubnet: config.ClusterManager.V4TransitSubnet,
				}
				nad, err := newNetworkAttachmentDefinition(
					eipNamespace2,
					nadName1,
					netconf,
				)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				netInfo, err := util.NewNetInfo(&netconf)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				node1Annotations := map[string]string{
					"k8s.ovn.org/node-primary-ifaddr":             fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node1IPv4CIDR, ""),
					"k8s.ovn.org/node-subnets":                    fmt.Sprintf("{\"default\":\"%s\",\"%s\":\"%s\"}", v4Node1Subnet, networkName1, v4Node1Net1),
					"k8s.ovn.org/node-transit-switch-port-ifaddr": fmt.Sprintf("{\"ipv4\":\"%s/16\"}", v4Node1Tsp),
					"k8s.ovn.org/node-chassis-id":                 "473ca66d-d800-472f-b289-1ab81ae7f21c",
					util.OvnNodeID:                                "1",
					util.OVNNodeHostCIDRs:                         fmt.Sprintf("[\"%s\"]", node1IPv4CIDR),
					util.OvnNodeL3GatewayConfig: fmt.Sprintf(`{"%s":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"%s", "next-hop":"%s", "next-hops": ["%s"]},
"default":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"192.168.126.12/24", "next-hop": "192.168.126.1", "next-hops": ["192.168.126.1"]}}`, networkName1, v4Net1, gwIP, gwIP),
					util.Layer2TopologyVersion: util.TransitRouterTopoVersion,
				}
				labels := map[string]string{
					"k8s.ovn.org/egress-assignable": "",
				}
				node1 := getNodeObj(node1Name, node1Annotations, labels)
				node2Annotations := map[string]string{
					"k8s.ovn.org/node-primary-ifaddr":             fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node2IPv4CIDR, ""),
					"k8s.ovn.org/node-subnets":                    fmt.Sprintf("{\"default\":\"%s\",\"%s\":\"%s\"}", v4Node2Subnet, networkName1, v4Node2Net1),
					"k8s.ovn.org/node-transit-switch-port-ifaddr": fmt.Sprintf("{\"ipv4\":\"%s/16\"}", v4Node2Tsp),
					"k8s.ovn.org/node-chassis-id":                 "473ca66d-d800-472f-b289-1ab81ae7f21c",
					util.OvnNodeID:                                "2",
					util.OVNNodeHostCIDRs:                         fmt.Sprintf("[\"%s\"]", node2IPv4CIDR),
					util.OvnNodeL3GatewayConfig: fmt.Sprintf(`{"%s":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"%s", "next-hop":"%s", "next-hops": ["%s"]},
"default":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"192.168.126.12/24", "next-hop": "192.168.126.1", "next-hops": ["192.168.126.1"]}}`, networkName1, v4Net1, gwIP, gwIP),
					util.Layer2TopologyVersion: util.TransitRouterTopoVersion,
				}
				node2 := getNodeObj(node2Name, node2Annotations, nil)
				oneNodeStatus := []egressipv1.EgressIPStatusItem{
					{
						Node:     node1Name,
						EgressIP: egressIP,
					},
				}
				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMetaWithMark(egressIPName, eIP1Mark),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{egressIP},
						PodSelector: metav1.LabelSelector{
							MatchLabels: egressPodLabel,
						},
						NamespaceSelector: metav1.LabelSelector{
							MatchLabels: egressPodLabel,
						},
					},
					Status: egressipv1.EgressIPStatus{
						Items: oneNodeStatus,
					},
				}

				initialDB := []libovsdbtest.TestData{
					//CDN start
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name,
						Networks: []string{node1DefaultRtoJIPCIDR},
					},
					&nbdb.LogicalRouter{
						Name: ovntypes.OVNClusterRouter,
						UUID: ovntypes.OVNClusterRouter + "-UUID",
					},
					&nbdb.LogicalRouter{
						Name:  ovntypes.GWRouterPrefix + node1.Name,
						UUID:  ovntypes.GWRouterPrefix + node1.Name + "-UUID",
						Ports: []string{ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name + "-UUID"},
					},
					&nbdb.LogicalSwitchPort{
						UUID:      "k8s-" + node1Name + "-UUID",
						Name:      "k8s-" + node1Name,
						Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1CDNSubnet).IP.String()},
					},
					&nbdb.LogicalSwitch{
						UUID:  node1Name + "-UUID",
						Name:  node1Name,
						Ports: []string{"k8s-" + node1Name + "-UUID"},
					},
					// UDN start
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.RouterToTransitRouterPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name + "-UUID",
						Name:     ovntypes.RouterToTransitRouterPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name,
						Networks: []string{node1Network1JoinCIDR, node1Network1TransitCIDR},
					},
					&nbdb.LogicalRouter{
						Name:        netInfo.GetNetworkScopedClusterRouterName(),
						UUID:        netInfo.GetNetworkScopedClusterRouterName() + "-UUID",
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: networkName1, ovntypes.TopologyExternalID: ovntypes.Layer2Topology},
					},
					&nbdb.LogicalRouter{
						UUID:        netInfo.GetNetworkScopedGWRouterName(node1.Name) + "-UUID",
						Name:        netInfo.GetNetworkScopedGWRouterName(node1.Name),
						Ports:       []string{ovntypes.RouterToTransitRouterPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name + "-UUID"},
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: networkName1, ovntypes.TopologyExternalID: ovntypes.Layer2Topology},
					},
					&nbdb.LogicalSwitchPort{
						UUID:      "k8s-" + networkName1_ + node1Name + "-UUID",
						Name:      "k8s-" + networkName1_ + node1Name,
						Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1UDNSubnet).IP.String()},
					},
					&nbdb.LogicalSwitch{
						UUID:        netInfo.GetNetworkScopedSwitchName(node1.Name) + "-UUID",
						Name:        netInfo.GetNetworkScopedSwitchName(node1.Name),
						Ports:       []string{"k8s-" + networkName1_ + node1Name + "-UUID"},
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: networkName1, ovntypes.TopologyExternalID: ovntypes.Layer2Topology},
					},
				}
				fakeOvn.startWithDBSetup(
					libovsdbtest.TestSetup{
						NBData: initialDB,
					},
					&corev1.NodeList{
						Items: []corev1.Node{node1, node2},
					},
					&corev1.NamespaceList{
						Items: []corev1.Namespace{*egressCDNNamespace, *egressUDNNamespace},
					},
					&corev1.PodList{
						Items: []corev1.Pod{egressPodCDNLocal, egressPodUDNLocal, egressPodCDNRemote, egressPodUDNRemote},
					},
					&nadv1.NetworkAttachmentDefinitionList{
						Items: []nadv1.NetworkAttachmentDefinition{*nad},
					},
					&egressipv1.EgressIPList{
						Items: []egressipv1.EgressIP{},
					},
				)
				asf := addressset.NewOvnAddressSetFactory(fakeOvn.nbClient, true, false)
				// watch EgressIP depends on UDN enabled svcs address set being available
				c := udnenabledsvc.NewController(fakeOvn.nbClient, asf, fakeOvn.controller.watchFactory.ServiceCoreInformer(), []string{})
				go func() {
					gomega.Expect(c.Run(ctx.Done())).Should(gomega.Succeed())
				}()
				// Add CDN pod IPs to CDN cache
				iCDN, nCDN, _ := net.ParseCIDR(podV4IP + "/23")
				nCDN.IP = iCDN
				fakeOvn.controller.logicalPortCache.add(&egressPodCDNLocal, "", ovntypes.DefaultNetworkName, "", nil, []*net.IPNet{nCDN})
				fakeOvn.controller.nodeName = node1Name
				fakeOvn.eIPController.nodeName = node1Name
				secConInfo, ok := fakeOvn.userDefinedNetworkControllers[networkName1]
				gomega.Expect(ok).To(gomega.BeTrue())
				err = fakeOvn.eIPController.SyncNodeCache()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.networkManager.Start()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				defer fakeOvn.networkManager.Stop()
				fakeOvn.eIPController.ensureNetworkPending(networkName1)
				gomega.Expect(fakeOvn.eIPController.SyncNetworkReady(networkName1)).To(gomega.Succeed())
				err = fakeOvn.controller.WatchEgressIPPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIPNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressNodes()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// Add pod IPs to UDN cache
				iUDN, nUDN, _ := net.ParseCIDR(v4Pod1IPNode1Net1 + "/23")
				nUDN.IP = iUDN
				secConInfo.bnc.logicalPortCache.add(&egressPodUDNLocal, "", util.GetNADName(nad.Namespace, nad.Name), "", nil, []*net.IPNet{nUDN})
				_, err = fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Create(context.TODO(), &eIP, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				egressSVCServedPodsASv4, _ := buildEgressServiceAddressSets(nil)
				egressIPServedPodsASCDNv4, _ := buildEgressIPServedPodsAddressSets([]string{podV4IP}, ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkControllerName)
				egressNodeIPsASv4, _ := buildEgressIPNodeAddressSets([]string{node1IPv4, node2IPv4})
				egressIPServedPodsASUDNv4, _ := buildEgressIPServedPodsAddressSetsForController([]string{v4Pod1IPNode1Net1}, netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName)
				gomega.Eventually(c.IsAddressSetAvailable).Should(gomega.BeTrue())
				dbIDs := udnenabledsvc.GetAddressSetDBIDs()
				udnEnabledSvcV4, _ := addressset.GetTestDbAddrSets(dbIDs, []string{})
				node1LRP := "k8s-node1"
				expectedDatabaseState := []libovsdbtest.TestData{
					// CDN
					getReRouteStaticRoute(v4ClusterSubnet, node1DefaultRtoJIP),
					getReRoutePolicy(podV4IP, "4", "reroute-UUID", []string{node1DefaultRtoJIP},
						getEgressIPLRPReRouteDbIDs(eIP.Name, egressPodCDNLocal.Namespace, egressPodCDNLocal.Name, IPFamilyValueV4, ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkControllerName).GetExternalIDs()),
					&nbdb.LogicalRouterPolicy{
						Priority:    ovntypes.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4ClusterSubnet, v4ClusterSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-no-reroute-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    ovntypes.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4ClusterSubnet, config.Gateway.V4JoinSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-service-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouter{
						Name:  ovntypes.GWRouterPrefix + node1.Name,
						UUID:  ovntypes.GWRouterPrefix + node1.Name + "-UUID",
						Ports: []string{ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name + "-UUID"},
						Nat:   []string{"egressip-nat-UUID", "egressip-nat2-UUID"},
					},
					&nbdb.LogicalRouter{
						Name: ovntypes.OVNClusterRouter,
						UUID: ovntypes.OVNClusterRouter + "-UUID",
						Policies: []string{"default-no-reroute-UUID", "no-reroute-service-UUID",
							"default-no-reroute-node-UUID", "default-no-reroute-reply-traffic", "reroute-UUID"},
						StaticRoutes: []string{"reroute-static-route-UUID"},
					},
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name,
						Networks: []string{node1DefaultRtoJIPCIDR},
					},
					&nbdb.LogicalRouterPolicy{
						Priority: ovntypes.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
							egressIPServedPodsASCDNv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": ovntypes.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalSwitchPort{
						UUID:      "k8s-" + node1Name + "-UUID",
						Name:      "k8s-" + node1Name,
						Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1CDNSubnet).IP.String()},
					},
					&nbdb.LogicalSwitch{
						UUID:     node1Name + "-UUID",
						Name:     node1Name,
						Ports:    []string{"k8s-" + node1Name + "-UUID"},
						QOSRules: []string{"default-QoS-UUID"},
					},
					&nbdb.NAT{
						UUID:        "egressip-nat-UUID",
						LogicalIP:   podV4IP2,
						ExternalIP:  egressIP,
						ExternalIDs: getEgressIPNATDbIDs(egressIPName, egressPodCDNRemote.Namespace, egressPodCDNRemote.Name, IPFamilyValueV4, ovntypes.DefaultNetworkControllerName).GetExternalIDs(),
						Type:        nbdb.NATTypeSNAT,
						LogicalPort: &node1LRP,
						Options: map[string]string{
							"stateless": "false",
						},
					},
					&nbdb.NAT{
						UUID:        "egressip-nat2-UUID",
						LogicalIP:   podV4IP,
						ExternalIP:  egressIP,
						ExternalIDs: getEgressIPNATDbIDs(egressIPName, egressPodCDNLocal.Namespace, egressPodCDNLocal.Name, IPFamilyValueV4, ovntypes.DefaultNetworkControllerName).GetExternalIDs(),
						Type:        nbdb.NATTypeSNAT,
						LogicalPort: &node1LRP,
						Options: map[string]string{
							"stateless": "false",
						},
					},
					getNoReRouteReplyTrafficPolicy(ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkControllerName),
					getDefaultQoSRule(false, ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkControllerName),
					egressSVCServedPodsASv4,
					egressIPServedPodsASCDNv4,
					egressNodeIPsASv4,

					// UDN
					getReRoutePolicyForController(egressIPName, eipNamespace2, podName2, v4Pod1IPNode1Net1, eIP1Mark, IPFamilyValueV4, []string{node1Network1TransitIP}, netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName),
					getReRoutePolicyForController(egressIPName, eipNamespace2, podName4, v4Pod2IPNode2Net1, eIP1Mark, IPFamilyValueV4, []string{node1Network1TransitIP}, netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName),
					getGWPktMarkLRPForController(eIP1Mark, egressIPName, eipNamespace2, podName2, v4Pod1IPNode1Net1, IPFamilyValueV4,
						netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName),
					getGWPktMarkLRPForController(eIP1Mark, egressIPName, eipNamespace2, podName4, v4Pod2IPNode2Net1, IPFamilyValueV4,
						netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName),
					getNoReRoutePolicyForUDNEnabledSvc(false, netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName, egressIPServedPodsASUDNv4.Name, egressSVCServedPodsASv4.Name, udnEnabledSvcV4.Name),
					&nbdb.LogicalRouterPolicy{
						Priority:    ovntypes.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4Net1, v4Net1),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "udn-default-no-reroute-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    ovntypes.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4Net1, config.Gateway.V4JoinSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "udn-no-reroute-service-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority: ovntypes.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
							egressIPServedPodsASUDNv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "udn-default-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": ovntypes.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.RouterToTransitRouterPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name + "-UUID",
						Name:     ovntypes.RouterToTransitRouterPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name,
						Networks: []string{node1Network1JoinCIDR, node1Network1TransitCIDR},
					},
					&nbdb.LogicalRouter{
						Name:        netInfo.GetNetworkScopedClusterRouterName(),
						UUID:        netInfo.GetNetworkScopedClusterRouterName() + "-UUID",
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: netInfo.GetNetworkName(), ovntypes.TopologyExternalID: ovntypes.Layer2Topology},
						Policies: []string{
							"udn-default-no-reroute-node-UUID", "udn-default-no-reroute-UUID", "udn-no-reroute-service-UUID", "udn-enabled-svc-no-reroute-UUID",
							fmt.Sprintf("%s-no-reroute-reply-traffic", netInfo.GetNetworkName()),
							getReRoutePolicyUUID(eipNamespace2, podName2, IPFamilyValueV4, netInfo.GetNetworkName()),
							getReRoutePolicyUUID(eipNamespace2, podName4, IPFamilyValueV4, netInfo.GetNetworkName())},
					},
					&nbdb.LogicalRouter{
						UUID:        netInfo.GetNetworkScopedGWRouterName(node1.Name) + "-UUID",
						Name:        netInfo.GetNetworkScopedGWRouterName(node1.Name),
						Ports:       []string{ovntypes.RouterToTransitRouterPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name + "-UUID"},
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: netInfo.GetNetworkName(), ovntypes.TopologyExternalID: ovntypes.Layer2Topology},
						Policies: []string{getGWPktMarkLRPUUID(eipNamespace2, podName2, IPFamilyValueV4, netInfo.GetNetworkName()),
							getGWPktMarkLRPUUID(eipNamespace2, podName4, IPFamilyValueV4, netInfo.GetNetworkName())},
					},
					&nbdb.LogicalSwitchPort{
						UUID:      "k8s-" + networkName1_ + node1Name + "-UUID",
						Name:      "k8s-" + networkName1_ + node1Name,
						Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1UDNSubnet).IP.String()},
					},
					&nbdb.LogicalSwitch{
						UUID:        networkName1_ + layer2SwitchName + "-UUID",
						Name:        networkName1_ + layer2SwitchName,
						Ports:       []string{"k8s-" + networkName1_ + node1Name + "-UUID"},
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: netInfo.GetNetworkName(), ovntypes.TopologyExternalID: ovntypes.Layer2Topology},
						QOSRules:    []string{fmt.Sprintf("%s-QoS-UUID", netInfo.GetNetworkName())},
					},
					getNoReRouteReplyTrafficPolicyForController(netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName),
					getDefaultQoSRule(false, netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName),
					egressIPServedPodsASUDNv4,
					udnEnabledSvcV4,
				}
				ginkgo.By("ensure expected equals actual")
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
				ginkgo.By("deleting all EgressIP seelected pods")
				deletePod(egressPodCDNLocal.Namespace, egressPodCDNLocal.Name, fakeOvn.fakeClient.KubeClient)
				deletePod(egressPodCDNRemote.Namespace, egressPodCDNRemote.Name, fakeOvn.fakeClient.KubeClient)
				deletePod(egressPodUDNLocal.Namespace, egressPodUDNLocal.Name, fakeOvn.fakeClient.KubeClient)
				deletePod(egressPodUDNRemote.Namespace, egressPodUDNRemote.Name, fakeOvn.fakeClient.KubeClient)

				ginkgo.By("ensure OVN config is removed for the deleted pods")
				egressIPServedPodsASCDNv4, _ = buildEgressIPServedPodsAddressSets([]string{}, ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkControllerName)
				egressIPServedPodsASUDNv4, _ = buildEgressIPServedPodsAddressSetsForController([]string{}, netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName)
				expectedDatabaseState = []libovsdbtest.TestData{
					// CDN
					getReRouteStaticRoute(v4ClusterSubnet, node1DefaultRtoJIP),
					&nbdb.LogicalRouterPolicy{
						Priority:    ovntypes.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4ClusterSubnet, v4ClusterSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-no-reroute-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    ovntypes.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4ClusterSubnet, config.Gateway.V4JoinSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-service-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouter{
						Name:  ovntypes.GWRouterPrefix + node1.Name,
						UUID:  ovntypes.GWRouterPrefix + node1.Name + "-UUID",
						Ports: []string{ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name + "-UUID"},
					},
					&nbdb.LogicalRouter{
						Name: ovntypes.OVNClusterRouter,
						UUID: ovntypes.OVNClusterRouter + "-UUID",
						Policies: []string{"default-no-reroute-UUID", "no-reroute-service-UUID",
							"default-no-reroute-node-UUID", "default-no-reroute-reply-traffic"},
						StaticRoutes: []string{"reroute-static-route-UUID"},
					},
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name,
						Networks: []string{node1DefaultRtoJIPCIDR},
					},
					&nbdb.LogicalRouterPolicy{
						Priority: ovntypes.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
							egressIPServedPodsASCDNv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": ovntypes.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalSwitchPort{
						UUID:      "k8s-" + node1Name + "-UUID",
						Name:      "k8s-" + node1Name,
						Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1CDNSubnet).IP.String()},
					},
					&nbdb.LogicalSwitch{
						UUID:     node1Name + "-UUID",
						Name:     node1Name,
						Ports:    []string{"k8s-" + node1Name + "-UUID"},
						QOSRules: []string{"default-QoS-UUID"},
					},
					getNoReRouteReplyTrafficPolicy(ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkControllerName),
					getDefaultQoSRule(false, ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkControllerName),
					egressSVCServedPodsASv4,
					egressIPServedPodsASCDNv4,
					egressNodeIPsASv4,

					// UDN
					getNoReRoutePolicyForUDNEnabledSvc(false, netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName, egressIPServedPodsASUDNv4.Name, egressSVCServedPodsASv4.Name, udnEnabledSvcV4.Name),
					&nbdb.LogicalRouterPolicy{
						Priority:    ovntypes.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4Net1, v4Net1),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "udn-default-no-reroute-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    ovntypes.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4Net1, config.Gateway.V4JoinSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "udn-no-reroute-service-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority: ovntypes.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
							egressIPServedPodsASUDNv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "udn-default-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": ovntypes.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.RouterToTransitRouterPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name + "-UUID",
						Name:     ovntypes.RouterToTransitRouterPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name,
						Networks: []string{node1Network1JoinCIDR, node1Network1TransitCIDR},
					},
					&nbdb.LogicalRouter{
						Name:        netInfo.GetNetworkScopedClusterRouterName(),
						UUID:        netInfo.GetNetworkScopedClusterRouterName() + "-UUID",
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: netInfo.GetNetworkName(), ovntypes.TopologyExternalID: ovntypes.Layer2Topology},
						Policies: []string{
							"udn-default-no-reroute-node-UUID", "udn-default-no-reroute-UUID", "udn-no-reroute-service-UUID", "udn-enabled-svc-no-reroute-UUID",
							fmt.Sprintf("%s-no-reroute-reply-traffic", netInfo.GetNetworkName())},
					},
					&nbdb.LogicalRouter{
						UUID:        netInfo.GetNetworkScopedGWRouterName(node1.Name) + "-UUID",
						Name:        netInfo.GetNetworkScopedGWRouterName(node1.Name),
						Ports:       []string{ovntypes.RouterToTransitRouterPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name + "-UUID"},
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: netInfo.GetNetworkName(), ovntypes.TopologyExternalID: ovntypes.Layer2Topology},
					},
					&nbdb.LogicalSwitchPort{
						UUID:      "k8s-" + networkName1_ + node1Name + "-UUID",
						Name:      "k8s-" + networkName1_ + node1Name,
						Addresses: []string{"fe:1a:b2:3f:0e:fb " + util.GetNodeManagementIfAddr(node1UDNSubnet).IP.String()},
					},
					&nbdb.LogicalSwitch{
						UUID:        networkName1_ + layer2SwitchName + "-UUID",
						Name:        networkName1_ + layer2SwitchName,
						Ports:       []string{"k8s-" + networkName1_ + node1Name + "-UUID"},
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: netInfo.GetNetworkName(), ovntypes.TopologyExternalID: ovntypes.Layer2Topology},
						QOSRules:    []string{fmt.Sprintf("%s-QoS-UUID", netInfo.GetNetworkName())},
					},
					getNoReRouteReplyTrafficPolicyForController(netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName),
					getDefaultQoSRule(false, netInfo.GetNetworkName(), ovntypes.DefaultNetworkControllerName),
					egressIPServedPodsASUDNv4,
					udnEnabledSvcV4,
				}
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})
})
