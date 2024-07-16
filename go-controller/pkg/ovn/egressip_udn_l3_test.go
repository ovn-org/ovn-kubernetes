package ovn

import (
	"context"
	"encoding/json"
	"fmt"
	"net"

	ovncnitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	egressipv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/controller/udnenabledsvc"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	k8stypes "k8s.io/apimachinery/pkg/types"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	nadv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/urfave/cli/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
)

var _ = ginkgo.Describe("EgressIP Operations for user defined network with topology L3", func() {
	var (
		app     *cli.App
		fakeOvn *FakeOVN
	)

	const (
		nadName1          = "nad1"
		networkName1      = "network1"
		networkName1_     = networkName1 + "_"
		node1Name         = "node1"
		v4Net1            = "20.128.0.0/14"
		v4Node1Net1       = "20.128.0.0/16"
		v4Pod1IPNode1Net1 = "20.128.0.5"
		podName3          = "egress-pod3"
		v4Pod2IPNode1Net1 = "20.128.0.6"
		v4Node1Tsp        = "100.88.0.2"
		node2Name         = "node2"
		v4Node2Net1       = "20.129.0.0/16"
		v4Node2Tsp        = "100.88.0.3"
		podName4          = "egress-pod4"
		v4Pod1IPNode2Net1 = "20.129.0.2"
		v4Pod2IPNode2Net1 = "20.129.0.3"
		eIP1Mark          = 50000
		eIP2Mark          = 50001
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
		config.OVNKubernetesFeature.EnableInterconnect = true
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		config.Gateway.Mode = config.GatewayModeShared
		config.OVNKubernetesFeature.EgressIPNodeHealthCheckPort = 1234

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

		fakeOvn = NewFakeOVN(false)
	})

	ginkgo.AfterEach(func() {
		fakeOvn.shutdown()
		// Restore global default values
		gomega.Expect(config.PrepareTestConfig()).Should(gomega.Succeed())
	})

	ginkgo.Context("sync", func() {
		ginkgo.It("should remove stale LRPs for marks and configures missing LRP marks", func() {
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
				egressCDNNamespace := newNamespaceWithLabels(eipNamespace, egressPodLabel)
				egressUDNNamespace := newNamespaceWithLabels(eipNamespace2, egressPodLabel)
				egressPodCDNLocal := *newPodWithLabels(eipNamespace, podName, node1Name, podV4IP, egressPodLabel)
				egressPodUDNLocal := *newPodWithLabels(eipNamespace2, podName2, node1Name, v4Pod1IPNode1Net1, egressPodLabel)
				egressPodCDNRemote := *newPodWithLabels(eipNamespace, podName3, node2Name, podV4IP2, egressPodLabel)
				setPrimaryNetworkAnnot(&egressPodCDNRemote, ovntypes.DefaultNetworkName, fmt.Sprintf("%s%s", podV4IP2, util.GetIPFullMaskString(podV4IP2)))
				egressPodUDNRemote := *newPodWithLabels(eipNamespace2, podName4, node2Name, v4Pod2IPNode2Net1, egressPodLabel)
				setPrimaryNetworkAnnot(&egressPodUDNRemote, nadName, fmt.Sprintf("%s%s", v4Pod2IPNode2Net1, util.GetIPFullMaskString(v4Pod2IPNode2Net1)))
				netconf := ovncnitypes.NetConf{
					NetConf: cnitypes.NetConf{
						Name: networkName1,
						Type: "ovn-k8s-cni-overlay",
					},
					Role:     ovntypes.NetworkRolePrimary,
					Topology: ovntypes.Layer3Topology,
					NADName:  nadName,
					Subnets:  v4Net1,
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
					"k8s.ovn.org/zone-name":                       node1Name,
					util.OVNNodeHostCIDRs:                         fmt.Sprintf("[\"%s\"]", node1IPv4CIDR),
				}
				labels := map[string]string{
					"k8s.ovn.org/egress-assignable": "",
				}
				node1 := getNodeObj(node1Name, node1Annotations, labels)
				node2Annotations := map[string]string{
					"k8s.ovn.org/node-primary-ifaddr":             fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node2IPv4CIDR, ""),
					"k8s.ovn.org/node-subnets":                    fmt.Sprintf("{\"default\":\"%s\",\"%s\":\"%s\"}", v4Node2Subnet, networkName1, v4Node2Net1),
					"k8s.ovn.org/node-transit-switch-port-ifaddr": fmt.Sprintf("{\"ipv4\":\"%s/16\"}", v4Node2Tsp),
					"k8s.ovn.org/zone-name":                       node2Name,
					util.OVNNodeHostCIDRs:                         fmt.Sprintf("[\"%s\"]", node2IPv4CIDR),
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
						Networks: []string{nodeLogicalRouterIfAddrV4},
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
					//getGWPktMarkLRPForController(eIP1Mark, egressIPName, eipNamespace2, podName3, v4Pod2IPNode1Net1, IPFamilyValueV4, networkName1, DefaultNetworkControllerName),
					//getGWPktMarkLRPForController(eIP2Mark, egressIPName, eipNamespace2, podName4, v4Pod1IPNode2Net1, IPFamilyValueV4, networkName1, DefaultNetworkControllerName), //stale EIP mark
					//getGWPktMarkLRPForController(eIP2Mark, egressIPName, eipNamespace2, podName2, v4Pod1IPNode1Net1, IPFamilyValueV4, networkName1, DefaultNetworkControllerName), //stale EIP mark
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name,
						Networks: []string{nodeLogicalRouterIfAddrV4},
					},
					&nbdb.LogicalRouter{
						Name:        netInfo.GetNetworkScopedClusterRouterName(),
						UUID:        netInfo.GetNetworkScopedClusterRouterName() + "-UUID",
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: networkName1, ovntypes.TopologyExternalID: ovntypes.Layer3Topology},
					},
					&nbdb.LogicalRouter{
						UUID:        netInfo.GetNetworkScopedGWRouterName(node1.Name) + "-UUID",
						Name:        netInfo.GetNetworkScopedGWRouterName(node1.Name),
						Ports:       []string{ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name + "-UUID"},
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: networkName1, ovntypes.TopologyExternalID: ovntypes.Layer3Topology},
						//Policies:    []string{getGWPktMarkLRPUUID(eipNamespace2, podName3, IPFamilyValueV4, networkName1)},
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
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: networkName1, ovntypes.TopologyExternalID: ovntypes.Layer3Topology},
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
				fakeOvn.controller.eIPC.nodeZoneState.Store(node1Name, true)
				fakeOvn.controller.eIPC.nodeZoneState.Store(node2Name, false)
				fakeOvn.controller.eIPC.zone = node1.Name
				fakeOvn.controller.zone = node1.Name
				err = fakeOvn.eIPController.ensureL3ClusterRouterPoliciesForNetwork(netInfo)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.eIPController.ensureL3SwitchPoliciesForNode(netInfo, node1Name)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(fakeOvn.networkManager.Start()).Should(gomega.Succeed())
				err = fakeOvn.controller.WatchEgressIPNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIPPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressNodes()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				egressIPServedPodsASCDNv4, _ := buildEgressIPServedPodsAddressSets([]string{podV4IP}, ovntypes.DefaultNetworkName, DefaultNetworkControllerName)
				egressNodeIPsASv4, _ := buildEgressIPNodeAddressSets([]string{node1IPv4, node2IPv4})
				egressSVCServedPodsASv4, _ := buildEgressServiceAddressSets(nil)
				egressIPServedPodsASUDNv4, _ := buildEgressIPServedPodsAddressSetsForController([]string{v4Pod1IPNode1Net1}, netInfo.GetNetworkName(), DefaultNetworkControllerName)
				gomega.Eventually(c.IsAddressSetAvailable).Should(gomega.BeTrue())
				dbIDs := udnenabledsvc.GetAddressSetDBIDs()
				udnEnabledSvcV4, _ := addressset.GetTestDbAddrSets(dbIDs, []string{})

				node1LRP := "k8s-node1"
				expectedDatabaseStateTwoEgressNodes := []libovsdbtest.TestData{
					// CDN
					getReRouteStaticRoute(v4ClusterSubnet, nodeLogicalRouterIPv4[0]),
					getReRoutePolicy(podV4IP, "4", "reroute-UUID", []string{nodeLogicalRouterIPv4[0], v4Node2Tsp},
						getEgressIPLRPReRouteDbIDs(eIP.Name, egressPodCDNLocal.Namespace, egressPodCDNLocal.Name, IPFamilyValueV4, ovntypes.DefaultNetworkName, DefaultNetworkControllerName).GetExternalIDs()),
					&nbdb.LogicalRouterPolicy{
						Priority:    ovntypes.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4ClusterSubnet, v4ClusterSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-no-reroute-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, ovntypes.DefaultNetworkName, DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    ovntypes.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4ClusterSubnet, config.Gateway.V4JoinSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-service-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, ovntypes.DefaultNetworkName, DefaultNetworkControllerName).GetExternalIDs(),
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
						Networks: []string{"100.64.0.2/29"},
					},
					&nbdb.LogicalRouterPolicy{
						Priority: ovntypes.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
							egressIPServedPodsASCDNv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": ovntypes.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, ovntypes.DefaultNetworkName, DefaultNetworkControllerName).GetExternalIDs(),
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
						ExternalIDs: getEgressIPNATDbIDs(egressIPName, egressPodCDNRemote.Namespace, egressPodCDNRemote.Name, IPFamilyValueV4, DefaultNetworkControllerName).GetExternalIDs(),
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
						ExternalIDs: getEgressIPNATDbIDs(egressIPName, egressPodCDNLocal.Namespace, egressPodCDNLocal.Name, IPFamilyValueV4, DefaultNetworkControllerName).GetExternalIDs(),
						Type:        nbdb.NATTypeSNAT,
						LogicalPort: &node1LRP,
						Options: map[string]string{
							"stateless": "false",
						},
					},
					getNoReRouteReplyTrafficPolicy(ovntypes.DefaultNetworkName, DefaultNetworkControllerName),
					getDefaultQoSRule(false, ovntypes.DefaultNetworkName, DefaultNetworkControllerName),
					egressSVCServedPodsASv4,
					egressIPServedPodsASCDNv4,
					egressNodeIPsASv4,

					// UDN
					getReRouteStaticRouteForController(v4Net1, nodeLogicalRouterIPv4[0], netInfo.GetNetworkName()),
					getReRoutePolicyForController(egressIPName, eipNamespace2, podName2, v4Pod1IPNode1Net1, eIP1Mark, IPFamilyValueV4, []string{nodeLogicalRouterIPv4[0], v4Node2Tsp}, netInfo.GetNetworkName(), DefaultNetworkControllerName),
					getGWPktMarkLRPForController(eIP1Mark, egressIPName, eipNamespace2, podName2, v4Pod1IPNode1Net1, IPFamilyValueV4, netInfo.GetNetworkName(), DefaultNetworkControllerName),
					getGWPktMarkLRPForController(eIP1Mark, egressIPName, eipNamespace2, podName4, v4Pod2IPNode2Net1, IPFamilyValueV4, netInfo.GetNetworkName(), DefaultNetworkControllerName),
					getNoReRoutePolicyForUDNEnabledSvc(false, netInfo.GetNetworkName(), DefaultNetworkControllerName, egressIPServedPodsASUDNv4.Name, egressSVCServedPodsASv4.Name, udnEnabledSvcV4.Name),
					&nbdb.LogicalRouterPolicy{
						Priority:    ovntypes.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4Net1, v4Net1),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "udn-default-no-reroute-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, netInfo.GetNetworkName(), DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    ovntypes.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4Net1, config.Gateway.V4JoinSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "udn-no-reroute-service-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, netInfo.GetNetworkName(), DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority: ovntypes.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
							egressIPServedPodsASUDNv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "udn-default-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": ovntypes.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, netInfo.GetNetworkName(), DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name,
						Networks: []string{nodeLogicalRouterIfAddrV4},
					},
					&nbdb.LogicalRouter{
						Name:        netInfo.GetNetworkScopedClusterRouterName(),
						UUID:        netInfo.GetNetworkScopedClusterRouterName() + "-UUID",
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: netInfo.GetNetworkName(), ovntypes.TopologyExternalID: ovntypes.Layer3Topology},
						Policies: []string{"udn-default-no-reroute-node-UUID", "udn-default-no-reroute-UUID", "udn-no-reroute-service-UUID",
							fmt.Sprintf("%s-no-reroute-reply-traffic", netInfo.GetNetworkName()), "udn-enabled-svc-no-reroute-UUID",
							getReRoutePolicyUUID(eipNamespace2, podName2, IPFamilyValueV4, netInfo.GetNetworkName())},
						StaticRoutes: []string{fmt.Sprintf("%s-reroute-static-route-UUID", netInfo.GetNetworkName())},
					},
					&nbdb.LogicalRouter{
						UUID:        netInfo.GetNetworkScopedGWRouterName(node1.Name) + "-UUID",
						Name:        netInfo.GetNetworkScopedGWRouterName(node1.Name),
						Ports:       []string{ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name + "-UUID"},
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: netInfo.GetNetworkName(), ovntypes.TopologyExternalID: ovntypes.Layer3Topology},
						Policies: []string{getGWPktMarkLRPUUID(eipNamespace2, podName2, IPFamilyValueV4, netInfo.GetNetworkName()),
							getGWPktMarkLRPUUID(eipNamespace2, podName4, IPFamilyValueV4, netInfo.GetNetworkName())},
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
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: netInfo.GetNetworkName(), ovntypes.TopologyExternalID: ovntypes.Layer3Topology},
						QOSRules:    []string{fmt.Sprintf("%s-QoS-UUID", netInfo.GetNetworkName())},
					},
					getNoReRouteReplyTrafficPolicyForController(netInfo.GetNetworkName(), DefaultNetworkControllerName),
					getDefaultQoSRule(false, netInfo.GetNetworkName(), DefaultNetworkControllerName),
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
				egressCDNNamespace := newNamespaceWithLabels(eipNamespace, egressPodLabel)
				egressUDNNamespace := newNamespaceWithLabels(eipNamespace2, egressPodLabel)
				egressPodCDNLocal := *newPodWithLabels(eipNamespace, podName, node1Name, podV4IP, egressPodLabel)
				egressPodUDNLocal := *newPodWithLabels(eipNamespace2, podName2, node1Name, v4Pod1IPNode1Net1, egressPodLabel)
				egressPodCDNRemote := *newPodWithLabels(eipNamespace, podName3, node2Name, podV4IP2, egressPodLabel)
				setPrimaryNetworkAnnot(&egressPodCDNRemote, ovntypes.DefaultNetworkName, fmt.Sprintf("%s%s", podV4IP2, util.GetIPFullMaskString(podV4IP2)))
				egressPodUDNRemote := *newPodWithLabels(eipNamespace2, podName4, node2Name, v4Pod2IPNode2Net1, egressPodLabel)
				setPrimaryNetworkAnnot(&egressPodUDNRemote, nadName, fmt.Sprintf("%s%s", v4Pod2IPNode2Net1, util.GetIPFullMaskString(v4Pod2IPNode2Net1)))

				netconf := ovncnitypes.NetConf{
					NetConf: cnitypes.NetConf{
						Name: networkName1,
						Type: "ovn-k8s-cni-overlay",
					},
					Role:     ovntypes.NetworkRolePrimary,
					Topology: ovntypes.Layer3Topology,
					NADName:  nadName,
					Subnets:  v4Net1,
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
					"k8s.ovn.org/zone-name":                       node1Name,
					"k8s.ovn.org/remote-zone-migrated":            node1Name,
					util.OVNNodeHostCIDRs:                         fmt.Sprintf("[\"%s\"]", node1IPv4CIDR),
				}
				labels := map[string]string{
					"k8s.ovn.org/egress-assignable": "",
				}
				node1 := getNodeObj(node1Name, node1Annotations, labels)
				node2Annotations := map[string]string{
					"k8s.ovn.org/node-primary-ifaddr":             fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node2IPv4CIDR, ""),
					"k8s.ovn.org/node-subnets":                    fmt.Sprintf("{\"default\":\"%s\",\"%s\":\"%s\"}", v4Node2Subnet, networkName1, v4Node2Net1),
					"k8s.ovn.org/node-transit-switch-port-ifaddr": fmt.Sprintf("{\"ipv4\":\"%s/16\"}", v4Node2Tsp),
					"k8s.ovn.org/zone-name":                       node2Name,
					"k8s.ovn.org/remote-zone-migrated":            node2Name,
					util.OVNNodeHostCIDRs:                         fmt.Sprintf("[\"%s\"]", node2IPv4CIDR),
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
						Networks: []string{nodeLogicalRouterIfAddrV4},
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
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name,
						Networks: []string{nodeLogicalRouterIfAddrV4},
					},
					&nbdb.LogicalRouter{
						Name:        netInfo.GetNetworkScopedClusterRouterName(),
						UUID:        netInfo.GetNetworkScopedClusterRouterName() + "-UUID",
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: networkName1, ovntypes.TopologyExternalID: ovntypes.Layer3Topology},
					},
					&nbdb.LogicalRouter{
						UUID:        netInfo.GetNetworkScopedGWRouterName(node1.Name) + "-UUID",
						Name:        netInfo.GetNetworkScopedGWRouterName(node1.Name),
						Ports:       []string{ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name + "-UUID"},
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: networkName1, ovntypes.TopologyExternalID: ovntypes.Layer3Topology},
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
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: networkName1, ovntypes.TopologyExternalID: ovntypes.Layer3Topology},
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
				fakeOvn.controller.zone = node1.Name
				fakeOvn.eIPController.zone = node1.Name
				fakeOvn.controller.logicalPortCache.add(&egressPodCDNLocal, "", ovntypes.DefaultNetworkName, "", nil, []*net.IPNet{nCDN})
				secConInfo, ok := fakeOvn.secondaryControllers[networkName1]
				gomega.Expect(ok).To(gomega.BeTrue())
				err = fakeOvn.networkManager.Start()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// simulate Start() of secondary network controller
				err = fakeOvn.eIPController.ensureL3ClusterRouterPoliciesForNetwork(secConInfo.bnc.GetNetInfo())
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.eIPController.ensureL3SwitchPoliciesForNode(secConInfo.bnc.GetNetInfo(), node1Name)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIPNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIPPods()
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
				egressIPServedPodsASCDNv4, _ := buildEgressIPServedPodsAddressSets([]string{podV4IP}, ovntypes.DefaultNetworkName, DefaultNetworkControllerName)
				egressNodeIPsASv4, _ := buildEgressIPNodeAddressSets([]string{node1IPv4, node2IPv4})
				egressIPServedPodsASUDNv4, _ := buildEgressIPServedPodsAddressSetsForController([]string{v4Pod1IPNode1Net1}, netInfo.GetNetworkName(), DefaultNetworkControllerName)
				gomega.Eventually(c.IsAddressSetAvailable).Should(gomega.BeTrue())
				dbIDs := udnenabledsvc.GetAddressSetDBIDs()
				udnEnabledSvcV4, _ := addressset.GetTestDbAddrSets(dbIDs, []string{})

				node1LRP := "k8s-node1"
				expectedDatabaseStateTwoEgressNodes := []libovsdbtest.TestData{
					// CDN
					getReRouteStaticRoute(v4ClusterSubnet, nodeLogicalRouterIPv4[0]),
					getReRoutePolicy(podV4IP, "4", "reroute-UUID", []string{nodeLogicalRouterIPv4[0], v4Node2Tsp},
						getEgressIPLRPReRouteDbIDs(eIP.Name, egressPodCDNLocal.Namespace, egressPodCDNLocal.Name, IPFamilyValueV4, ovntypes.DefaultNetworkName, DefaultNetworkControllerName).GetExternalIDs()),
					&nbdb.LogicalRouterPolicy{
						Priority:    ovntypes.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4ClusterSubnet, v4ClusterSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-no-reroute-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, ovntypes.DefaultNetworkName, DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    ovntypes.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4ClusterSubnet, config.Gateway.V4JoinSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-service-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, ovntypes.DefaultNetworkName, DefaultNetworkControllerName).GetExternalIDs(),
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
						Networks: []string{"100.64.0.2/29"},
					},
					&nbdb.LogicalRouterPolicy{
						Priority: ovntypes.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
							egressIPServedPodsASCDNv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": ovntypes.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, ovntypes.DefaultNetworkName, DefaultNetworkControllerName).GetExternalIDs(),
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
						ExternalIDs: getEgressIPNATDbIDs(egressIPName, egressPodCDNRemote.Namespace, egressPodCDNRemote.Name, IPFamilyValueV4, DefaultNetworkControllerName).GetExternalIDs(),
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
						ExternalIDs: getEgressIPNATDbIDs(egressIPName, egressPodCDNLocal.Namespace, egressPodCDNLocal.Name, IPFamilyValueV4, DefaultNetworkControllerName).GetExternalIDs(),
						Type:        nbdb.NATTypeSNAT,
						LogicalPort: &node1LRP,
						Options: map[string]string{
							"stateless": "false",
						},
					},
					getNoReRouteReplyTrafficPolicy(ovntypes.DefaultNetworkName, DefaultNetworkControllerName),
					getDefaultQoSRule(false, ovntypes.DefaultNetworkName, DefaultNetworkControllerName),
					egressSVCServedPodsASv4,
					egressIPServedPodsASCDNv4,
					egressNodeIPsASv4,

					// UDN
					getReRouteStaticRouteForController(v4Net1, nodeLogicalRouterIPv4[0], netInfo.GetNetworkName()),
					getReRoutePolicyForController(egressIPName, eipNamespace2, podName2, v4Pod1IPNode1Net1, eIP1Mark, IPFamilyValueV4, []string{nodeLogicalRouterIPv4[0], v4Node2Tsp}, netInfo.GetNetworkName(), DefaultNetworkControllerName),
					getGWPktMarkLRPForController(eIP1Mark, egressIPName, eipNamespace2, podName2, v4Pod1IPNode1Net1, IPFamilyValueV4, netInfo.GetNetworkName(), DefaultNetworkControllerName),
					getGWPktMarkLRPForController(eIP1Mark, egressIPName, eipNamespace2, podName4, v4Pod2IPNode2Net1, IPFamilyValueV4, netInfo.GetNetworkName(), DefaultNetworkControllerName),
					getNoReRoutePolicyForUDNEnabledSvc(false, netInfo.GetNetworkName(), DefaultNetworkControllerName, egressIPServedPodsASUDNv4.Name, egressSVCServedPodsASv4.Name, udnEnabledSvcV4.Name),
					&nbdb.LogicalRouterPolicy{
						Priority:    ovntypes.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4Net1, v4Net1),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "udn-default-no-reroute-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, netInfo.GetNetworkName(), DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    ovntypes.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4Net1, config.Gateway.V4JoinSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "udn-no-reroute-service-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, netInfo.GetNetworkName(), DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority: ovntypes.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
							egressIPServedPodsASUDNv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "udn-default-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": ovntypes.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, netInfo.GetNetworkName(), DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name,
						Networks: []string{nodeLogicalRouterIfAddrV4},
					},
					&nbdb.LogicalRouter{
						Name:        netInfo.GetNetworkScopedClusterRouterName(),
						UUID:        netInfo.GetNetworkScopedClusterRouterName() + "-UUID",
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: netInfo.GetNetworkName(), ovntypes.TopologyExternalID: ovntypes.Layer3Topology},
						Policies: []string{"udn-default-no-reroute-node-UUID", "udn-default-no-reroute-UUID", "udn-no-reroute-service-UUID", "udn-enabled-svc-no-reroute-UUID",
							fmt.Sprintf("%s-no-reroute-reply-traffic", netInfo.GetNetworkName()),
							getReRoutePolicyUUID(eipNamespace2, podName2, IPFamilyValueV4, netInfo.GetNetworkName())},
						StaticRoutes: []string{fmt.Sprintf("%s-reroute-static-route-UUID", netInfo.GetNetworkName())},
					},
					&nbdb.LogicalRouter{
						UUID:        netInfo.GetNetworkScopedGWRouterName(node1.Name) + "-UUID",
						Name:        netInfo.GetNetworkScopedGWRouterName(node1.Name),
						Ports:       []string{ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name + "-UUID"},
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: netInfo.GetNetworkName(), ovntypes.TopologyExternalID: ovntypes.Layer3Topology},
						Policies: []string{getGWPktMarkLRPUUID(eipNamespace2, podName2, IPFamilyValueV4, netInfo.GetNetworkName()),
							getGWPktMarkLRPUUID(eipNamespace2, podName4, IPFamilyValueV4, netInfo.GetNetworkName())},
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
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: netInfo.GetNetworkName(), ovntypes.TopologyExternalID: ovntypes.Layer3Topology},
						QOSRules:    []string{fmt.Sprintf("%s-QoS-UUID", netInfo.GetNetworkName())},
					},
					getNoReRouteReplyTrafficPolicyForController(netInfo.GetNetworkName(), DefaultNetworkControllerName),
					getDefaultQoSRule(false, netInfo.GetNetworkName(), DefaultNetworkControllerName),
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
					getReRouteStaticRoute(v4ClusterSubnet, nodeLogicalRouterIPv4[0]),
					getReRoutePolicy(podV4IP, "4", "reroute-UUID", []string{v4Node2Tsp},
						getEgressIPLRPReRouteDbIDs(eIP.Name, egressPodCDNLocal.Namespace, egressPodCDNLocal.Name, IPFamilyValueV4, ovntypes.DefaultNetworkName, DefaultNetworkControllerName).GetExternalIDs()),
					&nbdb.LogicalRouterPolicy{
						Priority:    ovntypes.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4ClusterSubnet, v4ClusterSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-no-reroute-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, ovntypes.DefaultNetworkName, DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    ovntypes.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4ClusterSubnet, config.Gateway.V4JoinSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-service-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, ovntypes.DefaultNetworkName, DefaultNetworkControllerName).GetExternalIDs(),
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
						Networks: []string{"100.64.0.2/29"},
					},
					&nbdb.LogicalRouterPolicy{
						Priority: ovntypes.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
							egressIPServedPodsASCDNv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": ovntypes.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, ovntypes.DefaultNetworkName, DefaultNetworkControllerName).GetExternalIDs(),
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
					getNoReRouteReplyTrafficPolicy(ovntypes.DefaultNetworkName, DefaultNetworkControllerName),
					getDefaultQoSRule(false, ovntypes.DefaultNetworkName, DefaultNetworkControllerName),
					egressSVCServedPodsASv4,
					egressIPServedPodsASCDNv4,
					egressNodeIPsASv4,

					// UDN
					getReRouteStaticRouteForController(v4Net1, nodeLogicalRouterIPv4[0], netInfo.GetNetworkName()),
					getReRoutePolicyForController(egressIPName, eipNamespace2, podName2, v4Pod1IPNode1Net1, eIP1Mark, IPFamilyValueV4, []string{v4Node2Tsp}, netInfo.GetNetworkName(), DefaultNetworkControllerName),
					getNoReRoutePolicyForUDNEnabledSvc(false, netInfo.GetNetworkName(), DefaultNetworkControllerName, egressIPServedPodsASUDNv4.Name, egressSVCServedPodsASv4.Name, udnEnabledSvcV4.Name),
					&nbdb.LogicalRouterPolicy{
						Priority:    ovntypes.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4Net1, v4Net1),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "udn-default-no-reroute-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, netInfo.GetNetworkName(), DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    ovntypes.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4Net1, config.Gateway.V4JoinSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "udn-no-reroute-service-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, netInfo.GetNetworkName(), DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority: ovntypes.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
							egressIPServedPodsASUDNv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "udn-default-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": ovntypes.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, netInfo.GetNetworkName(), DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name,
						Networks: []string{nodeLogicalRouterIfAddrV4},
					},
					&nbdb.LogicalRouter{
						Name:        netInfo.GetNetworkScopedClusterRouterName(),
						UUID:        netInfo.GetNetworkScopedClusterRouterName() + "-UUID",
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: netInfo.GetNetworkName(), ovntypes.TopologyExternalID: ovntypes.Layer3Topology},
						Policies: []string{"udn-default-no-reroute-node-UUID", "udn-default-no-reroute-UUID", "udn-no-reroute-service-UUID", "udn-enabled-svc-no-reroute-UUID",
							fmt.Sprintf("%s-no-reroute-reply-traffic", netInfo.GetNetworkName()),
							getReRoutePolicyUUID(eipNamespace2, podName2, IPFamilyValueV4, netInfo.GetNetworkName())},
						StaticRoutes: []string{fmt.Sprintf("%s-reroute-static-route-UUID", netInfo.GetNetworkName())},
					},
					&nbdb.LogicalRouter{
						UUID:        netInfo.GetNetworkScopedGWRouterName(node1.Name) + "-UUID",
						Name:        netInfo.GetNetworkScopedGWRouterName(node1.Name),
						Ports:       []string{ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name + "-UUID"},
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: netInfo.GetNetworkName(), ovntypes.TopologyExternalID: ovntypes.Layer3Topology},
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
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: netInfo.GetNetworkName(), ovntypes.TopologyExternalID: ovntypes.Layer3Topology},
						QOSRules:    []string{fmt.Sprintf("%s-QoS-UUID", netInfo.GetNetworkName())},
					},
					getNoReRouteReplyTrafficPolicyForController(netInfo.GetNetworkName(), DefaultNetworkControllerName),
					getDefaultQoSRule(false, netInfo.GetNetworkName(), DefaultNetworkControllerName),
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
				egressCDNNamespace := newNamespaceWithLabels(eipNamespace, egressPodLabel)
				egressUDNNamespace := newNamespaceWithLabels(eipNamespace2, egressPodLabel)
				egressPodCDNLocal := *newPodWithLabels(eipNamespace, podName, node1Name, podV4IP, egressPodLabel)
				egressPodUDNLocal := *newPodWithLabels(eipNamespace2, podName2, node1Name, v4Pod1IPNode1Net1, egressPodLabel)
				egressPodCDNRemote := *newPodWithLabels(eipNamespace, podName3, node2Name, podV4IP2, egressPodLabel)
				setPrimaryNetworkAnnot(&egressPodCDNRemote, ovntypes.DefaultNetworkName, fmt.Sprintf("%s%s", podV4IP2, util.GetIPFullMaskString(podV4IP2)))
				egressPodUDNRemote := *newPodWithLabels(eipNamespace2, podName4, node2Name, v4Pod2IPNode2Net1, egressPodLabel)
				setPrimaryNetworkAnnot(&egressPodUDNRemote, nadName, fmt.Sprintf("%s%s", v4Pod2IPNode2Net1, util.GetIPFullMaskString(v4Pod2IPNode2Net1)))
				netconf := ovncnitypes.NetConf{
					NetConf: cnitypes.NetConf{
						Name: networkName1,
						Type: "ovn-k8s-cni-overlay",
					},
					Role:     ovntypes.NetworkRolePrimary,
					Topology: ovntypes.Layer3Topology,
					NADName:  nadName,
					Subnets:  v4Net1,
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
					"k8s.ovn.org/zone-name":                       node1Name,
					"k8s.ovn.org/remote-zone-migrated":            node1Name,
					util.OVNNodeHostCIDRs:                         fmt.Sprintf("[\"%s\"]", node1IPv4CIDR),
				}
				labels := map[string]string{
					"k8s.ovn.org/egress-assignable": "",
				}
				node1 := getNodeObj(node1Name, node1Annotations, labels)
				node2Annotations := map[string]string{
					"k8s.ovn.org/node-primary-ifaddr":             fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node2IPv4CIDR, ""),
					"k8s.ovn.org/node-subnets":                    fmt.Sprintf("{\"default\":\"%s\",\"%s\":\"%s\"}", v4Node2Subnet, networkName1, v4Node2Net1),
					"k8s.ovn.org/node-transit-switch-port-ifaddr": fmt.Sprintf("{\"ipv4\":\"%s/16\"}", v4Node2Tsp),
					"k8s.ovn.org/zone-name":                       node2Name,
					"k8s.ovn.org/remote-zone-migrated":            node2Name,
					util.OVNNodeHostCIDRs:                         fmt.Sprintf("[\"%s\"]", node2IPv4CIDR),
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
						Networks: []string{nodeLogicalRouterIfAddrV4},
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
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name,
						Networks: []string{nodeLogicalRouterIfAddrV4},
					},
					&nbdb.LogicalRouter{
						Name:        netInfo.GetNetworkScopedClusterRouterName(),
						UUID:        netInfo.GetNetworkScopedClusterRouterName() + "-UUID",
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: networkName1, ovntypes.TopologyExternalID: ovntypes.Layer3Topology},
					},
					&nbdb.LogicalRouter{
						UUID:        netInfo.GetNetworkScopedGWRouterName(node1.Name) + "-UUID",
						Name:        netInfo.GetNetworkScopedGWRouterName(node1.Name),
						Ports:       []string{ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name + "-UUID"},
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: networkName1, ovntypes.TopologyExternalID: ovntypes.Layer3Topology},
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
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: networkName1, ovntypes.TopologyExternalID: ovntypes.Layer3Topology},
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
				fakeOvn.controller.zone = node1.Name
				fakeOvn.eIPController.zone = node1.Name
				err = fakeOvn.networkManager.Start()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIPNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIPPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressNodes()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				secConInfo, ok := fakeOvn.secondaryControllers[networkName1]
				gomega.Expect(ok).To(gomega.BeTrue())
				// Add pod IPs to UDN cache
				iUDN, nUDN, _ := net.ParseCIDR(v4Pod1IPNode1Net1 + "/23")
				nUDN.IP = iUDN
				secConInfo.bnc.logicalPortCache.add(&egressPodUDNLocal, "", util.GetNADName(nad.Namespace, nad.Name), "", nil, []*net.IPNet{nUDN})
				_, err = fakeOvn.fakeClient.EgressIPClient.K8sV1().EgressIPs().Create(context.TODO(), &eIP, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				egressSVCServedPodsASv4, _ := buildEgressServiceAddressSets(nil)
				egressIPServedPodsASCDNv4, _ := buildEgressIPServedPodsAddressSets([]string{podV4IP}, ovntypes.DefaultNetworkName, DefaultNetworkControllerName)
				egressNodeIPsASv4, _ := buildEgressIPNodeAddressSets([]string{node1IPv4, node2IPv4})
				egressIPServedPodsASUDNv4, _ := buildEgressIPServedPodsAddressSetsForController([]string{v4Pod1IPNode1Net1}, netInfo.GetNetworkName(), DefaultNetworkControllerName)
				gomega.Eventually(c.IsAddressSetAvailable).Should(gomega.BeTrue())
				dbIDs := udnenabledsvc.GetAddressSetDBIDs()
				udnEnabledSvcV4, _ := addressset.GetTestDbAddrSets(dbIDs, []string{})
				node1LRP := "k8s-node1"
				expectedDatabaseStateTwoEgressNodes := []libovsdbtest.TestData{
					// CDN
					getReRouteStaticRoute(v4ClusterSubnet, nodeLogicalRouterIPv4[0]),
					getReRoutePolicy(podV4IP, "4", "reroute-UUID", []string{nodeLogicalRouterIPv4[0], v4Node2Tsp},
						getEgressIPLRPReRouteDbIDs(eIP.Name, egressPodCDNLocal.Namespace, egressPodCDNLocal.Name, IPFamilyValueV4, ovntypes.DefaultNetworkName, DefaultNetworkControllerName).GetExternalIDs()),
					&nbdb.LogicalRouterPolicy{
						Priority:    ovntypes.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4ClusterSubnet, v4ClusterSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-no-reroute-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, ovntypes.DefaultNetworkName, DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    ovntypes.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4ClusterSubnet, config.Gateway.V4JoinSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-service-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, ovntypes.DefaultNetworkName, DefaultNetworkControllerName).GetExternalIDs(),
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
						Networks: []string{"100.64.0.2/29"},
					},
					&nbdb.LogicalRouterPolicy{
						Priority: ovntypes.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
							egressIPServedPodsASCDNv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": ovntypes.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, ovntypes.DefaultNetworkName, DefaultNetworkControllerName).GetExternalIDs(),
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
						ExternalIDs: getEgressIPNATDbIDs(egressIPName, egressPodCDNRemote.Namespace, egressPodCDNRemote.Name, IPFamilyValueV4, DefaultNetworkControllerName).GetExternalIDs(),
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
						ExternalIDs: getEgressIPNATDbIDs(egressIPName, egressPodCDNLocal.Namespace, egressPodCDNLocal.Name, IPFamilyValueV4, DefaultNetworkControllerName).GetExternalIDs(),
						Type:        nbdb.NATTypeSNAT,
						LogicalPort: &node1LRP,
						Options: map[string]string{
							"stateless": "false",
						},
					},
					getNoReRouteReplyTrafficPolicy(ovntypes.DefaultNetworkName, DefaultNetworkControllerName),
					getDefaultQoSRule(false, ovntypes.DefaultNetworkName, DefaultNetworkControllerName),
					egressSVCServedPodsASv4,
					egressIPServedPodsASCDNv4,
					egressNodeIPsASv4,

					// UDN
					getReRouteStaticRouteForController(v4Net1, nodeLogicalRouterIPv4[0], netInfo.GetNetworkName()),
					getReRoutePolicyForController(egressIPName, eipNamespace2, podName2, v4Pod1IPNode1Net1, eIP1Mark, IPFamilyValueV4, []string{nodeLogicalRouterIPv4[0], v4Node2Tsp}, netInfo.GetNetworkName(), DefaultNetworkControllerName),
					getGWPktMarkLRPForController(eIP1Mark, egressIPName, eipNamespace2, podName2, v4Pod1IPNode1Net1, IPFamilyValueV4, netInfo.GetNetworkName(), DefaultNetworkControllerName),
					getGWPktMarkLRPForController(eIP1Mark, egressIPName, eipNamespace2, podName4, v4Pod2IPNode2Net1, IPFamilyValueV4, netInfo.GetNetworkName(), DefaultNetworkControllerName),
					getNoReRoutePolicyForUDNEnabledSvc(false, netInfo.GetNetworkName(), DefaultNetworkControllerName, egressIPServedPodsASUDNv4.Name, egressSVCServedPodsASv4.Name, udnEnabledSvcV4.Name),
					&nbdb.LogicalRouterPolicy{
						Priority:    ovntypes.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4Net1, v4Net1),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "udn-default-no-reroute-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, netInfo.GetNetworkName(), DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    ovntypes.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4Net1, config.Gateway.V4JoinSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "udn-no-reroute-service-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, netInfo.GetNetworkName(), DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority: ovntypes.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
							egressIPServedPodsASUDNv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "udn-default-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": ovntypes.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, netInfo.GetNetworkName(), DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name,
						Networks: []string{nodeLogicalRouterIfAddrV4},
					},
					&nbdb.LogicalRouter{
						Name:        netInfo.GetNetworkScopedClusterRouterName(),
						UUID:        netInfo.GetNetworkScopedClusterRouterName() + "-UUID",
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: netInfo.GetNetworkName(), ovntypes.TopologyExternalID: ovntypes.Layer3Topology},
						Policies: []string{"udn-default-no-reroute-node-UUID", "udn-default-no-reroute-UUID", "udn-no-reroute-service-UUID", "udn-enabled-svc-no-reroute-UUID",
							fmt.Sprintf("%s-no-reroute-reply-traffic", netInfo.GetNetworkName()),
							getReRoutePolicyUUID(eipNamespace2, podName2, IPFamilyValueV4, netInfo.GetNetworkName())},
						StaticRoutes: []string{fmt.Sprintf("%s-reroute-static-route-UUID", netInfo.GetNetworkName())},
					},
					&nbdb.LogicalRouter{
						UUID:        netInfo.GetNetworkScopedGWRouterName(node1.Name) + "-UUID",
						Name:        netInfo.GetNetworkScopedGWRouterName(node1.Name),
						Ports:       []string{ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name + "-UUID"},
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: netInfo.GetNetworkName(), ovntypes.TopologyExternalID: ovntypes.Layer3Topology},
						Policies: []string{getGWPktMarkLRPUUID(eipNamespace2, podName2, IPFamilyValueV4, netInfo.GetNetworkName()),
							getGWPktMarkLRPUUID(eipNamespace2, podName4, IPFamilyValueV4, netInfo.GetNetworkName())},
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
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: netInfo.GetNetworkName(), ovntypes.TopologyExternalID: ovntypes.Layer3Topology},
						QOSRules:    []string{fmt.Sprintf("%s-QoS-UUID", netInfo.GetNetworkName())},
					},
					getNoReRouteReplyTrafficPolicyForController(netInfo.GetNetworkName(), DefaultNetworkControllerName),
					getDefaultQoSRule(false, netInfo.GetNetworkName(), DefaultNetworkControllerName),
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
					getReRouteStaticRoute(v4ClusterSubnet, nodeLogicalRouterIPv4[0]),
					&nbdb.LogicalRouterPolicy{
						Priority:    ovntypes.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4ClusterSubnet, v4ClusterSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-no-reroute-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, ovntypes.DefaultNetworkName, DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    ovntypes.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4ClusterSubnet, config.Gateway.V4JoinSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-service-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, ovntypes.DefaultNetworkName, DefaultNetworkControllerName).GetExternalIDs(),
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
						Networks: []string{"100.64.0.2/29"},
					},
					&nbdb.LogicalRouterPolicy{
						Priority: ovntypes.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
							egressIPServedPodsASCDNv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": ovntypes.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, ovntypes.DefaultNetworkName, DefaultNetworkControllerName).GetExternalIDs(),
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
					getNoReRouteReplyTrafficPolicy(ovntypes.DefaultNetworkName, DefaultNetworkControllerName),
					getDefaultQoSRule(false, ovntypes.DefaultNetworkName, DefaultNetworkControllerName),
					egressSVCServedPodsASv4,
					egressIPServedPodsASCDNv4,
					egressNodeIPsASv4,

					// UDN
					getReRouteStaticRouteForController(v4Net1, nodeLogicalRouterIPv4[0], netInfo.GetNetworkName()),
					getNoReRoutePolicyForUDNEnabledSvc(false, netInfo.GetNetworkName(), DefaultNetworkControllerName, egressIPServedPodsASUDNv4.Name, egressSVCServedPodsASv4.Name, udnEnabledSvcV4.Name),
					&nbdb.LogicalRouterPolicy{
						Priority:    ovntypes.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4Net1, v4Net1),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "udn-default-no-reroute-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, netInfo.GetNetworkName(), DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    ovntypes.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4Net1, config.Gateway.V4JoinSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "udn-no-reroute-service-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, netInfo.GetNetworkName(), DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority: ovntypes.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
							egressIPServedPodsASUDNv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "udn-default-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": ovntypes.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, netInfo.GetNetworkName(), DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name,
						Networks: []string{nodeLogicalRouterIfAddrV4},
					},
					&nbdb.LogicalRouter{
						Name:        netInfo.GetNetworkScopedClusterRouterName(),
						UUID:        netInfo.GetNetworkScopedClusterRouterName() + "-UUID",
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: secConInfo.bnc.GetNetworkName(), ovntypes.TopologyExternalID: ovntypes.Layer3Topology},
						Policies: []string{"udn-default-no-reroute-node-UUID", "udn-default-no-reroute-UUID", "udn-no-reroute-service-UUID", "udn-enabled-svc-no-reroute-UUID",
							fmt.Sprintf("%s-no-reroute-reply-traffic", netInfo.GetNetworkName()),
						},
						StaticRoutes: []string{fmt.Sprintf("%s-reroute-static-route-UUID", netInfo.GetNetworkName())},
					},
					&nbdb.LogicalRouter{
						UUID:        netInfo.GetNetworkScopedGWRouterName(node1.Name) + "-UUID",
						Name:        netInfo.GetNetworkScopedGWRouterName(node1.Name),
						Ports:       []string{ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name + "-UUID"},
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: secConInfo.bnc.GetNetworkName(), ovntypes.TopologyExternalID: ovntypes.Layer3Topology},
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
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: netInfo.GetNetworkName(), ovntypes.TopologyExternalID: ovntypes.Layer3Topology},
						QOSRules:    []string{fmt.Sprintf("%s-QoS-UUID", netInfo.GetNetworkName())},
					},
					getNoReRouteReplyTrafficPolicyForController(netInfo.GetNetworkName(), DefaultNetworkControllerName),
					getDefaultQoSRule(false, netInfo.GetNetworkName(), DefaultNetworkControllerName),
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
				egressCDNNamespace := newNamespaceWithLabels(eipNamespace, nil)
				egressUDNNamespace := newNamespaceWithLabels(eipNamespace2, nil)
				egressPodCDN := *newPodWithLabels(eipNamespace, podName, node1Name, podV4IP, egressPodLabel)
				egressPodUDN := *newPodWithLabels(eipNamespace2, podName2, node1Name, podV4IP2, egressPodLabel)

				nadNsName := util.GetNADName(eipNamespace2, nadName1)
				netconf := ovncnitypes.NetConf{
					NetConf: cnitypes.NetConf{
						Name: networkName1,
						Type: "ovn-k8s-cni-overlay",
					},
					Role:     ovntypes.NetworkRolePrimary,
					Topology: ovntypes.Layer3Topology,
					NADName:  nadNsName,
					Subnets:  v4Net1,
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
					"k8s.ovn.org/zone-name":                       node1Name,
					"k8s.ovn.org/remote-zone-migrated":            node1Name,
					util.OVNNodeHostCIDRs:                         fmt.Sprintf("[\"%s\"]", node1IPv4CIDR),
				}
				labels := map[string]string{
					"k8s.ovn.org/egress-assignable": "",
				}
				node1 := getNodeObj(node1Name, node1Annotations, labels)
				node2Annotations := map[string]string{
					"k8s.ovn.org/node-primary-ifaddr":             fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node2IPv4CIDR, ""),
					"k8s.ovn.org/node-subnets":                    fmt.Sprintf("{\"default\":\"%s\",\"%s\":\"%s\"}", v4Node2Subnet, networkName1, v4Node2Net1),
					"k8s.ovn.org/node-transit-switch-port-ifaddr": fmt.Sprintf("{\"ipv4\":\"%s/16\"}", v4Node2Tsp),
					"k8s.ovn.org/zone-name":                       node2Name,
					"k8s.ovn.org/remote-zone-migrated":            node2Name,
					util.OVNNodeHostCIDRs:                         fmt.Sprintf("[\"%s\"]", node2IPv4CIDR),
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
						Networks: []string{nodeLogicalRouterIfAddrV4},
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
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name,
						Networks: []string{nodeLogicalRouterIfAddrV4},
					},
					&nbdb.LogicalRouter{
						Name:        netInfo.GetNetworkScopedClusterRouterName(),
						UUID:        netInfo.GetNetworkScopedClusterRouterName() + "-UUID",
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: networkName1, ovntypes.TopologyExternalID: ovntypes.Layer3Topology},
					},
					&nbdb.LogicalRouter{
						UUID:        netInfo.GetNetworkScopedGWRouterName(node1.Name) + "-UUID",
						Name:        netInfo.GetNetworkScopedGWRouterName(node1.Name),
						Ports:       []string{ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name + "-UUID"},
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: networkName1, ovntypes.TopologyExternalID: ovntypes.Layer3Topology},
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
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: networkName1, ovntypes.TopologyExternalID: ovntypes.Layer3Topology},
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
				fakeOvn.controller.zone = node1Name
				fakeOvn.controller.eIPC.zone = node1Name
				err = fakeOvn.networkManager.Start()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.eIPController.ensureL3ClusterRouterPoliciesForNetwork(netInfo)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.eIPController.ensureL3SwitchPoliciesForNode(netInfo, node1Name)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIPNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIPPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressNodes()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				secConInfo, ok := fakeOvn.secondaryControllers[networkName1]
				gomega.Expect(ok).To(gomega.BeTrue())
				// Add pod IPs to UDN cache
				iUDN, nUDN, _ := net.ParseCIDR(v4Pod1IPNode1Net1 + "/23")
				nUDN.IP = iUDN
				secConInfo.bnc.logicalPortCache.add(&egressPodUDN, "", util.GetNADName(nad.Namespace, nad.Name), "", nil, []*net.IPNet{nUDN})
				ginkgo.By("update namespaces with label so its now selected by EgressIP")
				egressCDNNamespace.Labels = egressPodLabel
				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Update(context.Background(), egressCDNNamespace, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				egressUDNNamespace.Labels = egressPodLabel
				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Update(context.Background(), egressUDNNamespace, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				egressSVCServedPodsASv4, _ := buildEgressServiceAddressSets(nil)
				egressIPServedPodsASCDNv4, _ := buildEgressIPServedPodsAddressSets([]string{podV4IP}, ovntypes.DefaultNetworkName, DefaultNetworkControllerName)
				egressNodeIPsASv4, _ := buildEgressIPNodeAddressSets([]string{node1IPv4, node2IPv4})
				egressIPServedPodsASUDNv4, _ := buildEgressIPServedPodsAddressSetsForController([]string{v4Pod1IPNode1Net1}, netInfo.GetNetworkName(), DefaultNetworkControllerName)
				gomega.Eventually(c.IsAddressSetAvailable).Should(gomega.BeTrue())
				dbIDs := udnenabledsvc.GetAddressSetDBIDs()
				udnEnabledSvcV4, _ := addressset.GetTestDbAddrSets(dbIDs, []string{})
				node1LRP := "k8s-node1"
				expectedDatabaseStateTwoEgressNodes := []libovsdbtest.TestData{
					// CDN
					getReRouteStaticRoute(v4ClusterSubnet, nodeLogicalRouterIPv4[0]),
					getReRoutePolicy(podV4IP, "4", "reroute-UUID", []string{nodeLogicalRouterIPv4[0], v4Node2Tsp},
						getEgressIPLRPReRouteDbIDs(eIP.Name, egressPodCDN.Namespace, egressPodCDN.Name, IPFamilyValueV4, ovntypes.DefaultNetworkName, DefaultNetworkControllerName).GetExternalIDs()),
					&nbdb.NAT{
						UUID:        "egressip-nat-UUID",
						LogicalIP:   podV4IP,
						ExternalIP:  egressIP1,
						ExternalIDs: getEgressIPNATDbIDs(egressIPName, egressPodCDN.Namespace, egressPodCDN.Name, IPFamilyValueV4, DefaultNetworkControllerName).GetExternalIDs(),
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
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, ovntypes.DefaultNetworkName, DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    ovntypes.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4ClusterSubnet, config.Gateway.V4JoinSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-service-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, ovntypes.DefaultNetworkName, DefaultNetworkControllerName).GetExternalIDs(),
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
						Networks: []string{"100.64.0.2/29"},
					},
					&nbdb.LogicalRouterPolicy{
						Priority: ovntypes.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
							egressIPServedPodsASCDNv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": ovntypes.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, ovntypes.DefaultNetworkName, DefaultNetworkControllerName).GetExternalIDs(),
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
					getNoReRouteReplyTrafficPolicy(ovntypes.DefaultNetworkName, DefaultNetworkControllerName),
					getDefaultQoSRule(false, ovntypes.DefaultNetworkName, DefaultNetworkControllerName),
					egressSVCServedPodsASv4,
					egressIPServedPodsASCDNv4,
					egressNodeIPsASv4,

					// UDN
					getReRouteStaticRouteForController(v4Net1, nodeLogicalRouterIPv4[0], netInfo.GetNetworkName()),
					getReRoutePolicyForController(egressIPName, eipNamespace2, podName2, v4Pod1IPNode1Net1, eIP1Mark, IPFamilyValueV4, []string{nodeLogicalRouterIPv4[0], v4Node2Tsp}, netInfo.GetNetworkName(), DefaultNetworkControllerName),
					getGWPktMarkLRPForController(eIP1Mark, egressIPName, eipNamespace2, podName2, v4Pod1IPNode1Net1, IPFamilyValueV4, netInfo.GetNetworkName(), DefaultNetworkControllerName),
					getNoReRoutePolicyForUDNEnabledSvc(false, netInfo.GetNetworkName(), DefaultNetworkControllerName, egressIPServedPodsASUDNv4.Name, egressSVCServedPodsASv4.Name, udnEnabledSvcV4.Name),
					&nbdb.LogicalRouterPolicy{
						Priority:    ovntypes.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4Net1, v4Net1),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "udn-default-no-reroute-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, netInfo.GetNetworkName(), DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    ovntypes.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4Net1, config.Gateway.V4JoinSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "udn-no-reroute-service-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, netInfo.GetNetworkName(), DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority: ovntypes.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
							egressIPServedPodsASUDNv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "udn-default-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": ovntypes.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, netInfo.GetNetworkName(), DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name,
						Networks: []string{nodeLogicalRouterIfAddrV4},
					},
					&nbdb.LogicalRouter{
						Name:        netInfo.GetNetworkScopedClusterRouterName(),
						UUID:        netInfo.GetNetworkScopedClusterRouterName() + "-UUID",
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: netInfo.GetNetworkName(), ovntypes.TopologyExternalID: ovntypes.Layer3Topology},
						Policies: []string{"udn-default-no-reroute-node-UUID", "udn-default-no-reroute-UUID", "udn-no-reroute-service-UUID", "udn-enabled-svc-no-reroute-UUID",
							fmt.Sprintf("%s-no-reroute-reply-traffic", netInfo.GetNetworkName()),
							getReRoutePolicyUUID(eipNamespace2, podName2, IPFamilyValueV4, netInfo.GetNetworkName())},
						StaticRoutes: []string{fmt.Sprintf("%s-reroute-static-route-UUID", netInfo.GetNetworkName())},
					},
					&nbdb.LogicalRouter{
						UUID:        netInfo.GetNetworkScopedGWRouterName(node1.Name) + "-UUID",
						Name:        netInfo.GetNetworkScopedGWRouterName(node1.Name),
						Ports:       []string{ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name + "-UUID"},
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: netInfo.GetNetworkName(), ovntypes.TopologyExternalID: ovntypes.Layer3Topology},
						Policies:    []string{getGWPktMarkLRPUUID(eipNamespace2, podName2, IPFamilyValueV4, netInfo.GetNetworkName())},
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
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: netInfo.GetNetworkName(), ovntypes.TopologyExternalID: ovntypes.Layer3Topology},
						QOSRules:    []string{fmt.Sprintf("%s-QoS-UUID", netInfo.GetNetworkName())},
					},
					getNoReRouteReplyTrafficPolicyForController(netInfo.GetNetworkName(), DefaultNetworkControllerName),
					getDefaultQoSRule(false, netInfo.GetNetworkName(), DefaultNetworkControllerName),
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
				egressCDNNamespace := newNamespaceWithLabels(eipNamespace, egressPodLabel)
				egressUDNNamespace := newNamespaceWithLabels(eipNamespace2, egressPodLabel)
				egressPodCDN := *newPodWithLabels(eipNamespace, podName, node1Name, podV4IP, egressPodLabel)
				egressPodUDN := *newPodWithLabels(eipNamespace2, podName2, node1Name, podV4IP2, egressPodLabel)

				nadNsName := util.GetNADName(eipNamespace2, nadName1)
				netconf := ovncnitypes.NetConf{
					NetConf: cnitypes.NetConf{
						Name: networkName1,
						Type: "ovn-k8s-cni-overlay",
					},
					Role:     ovntypes.NetworkRolePrimary,
					Topology: ovntypes.Layer3Topology,
					NADName:  nadNsName,
					Subnets:  v4Net1,
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
					"k8s.ovn.org/zone-name":                       node1Name,
					"k8s.ovn.org/remote-zone-migrated":            node1Name,
					util.OVNNodeHostCIDRs:                         fmt.Sprintf("[\"%s\"]", node1IPv4CIDR),
				}
				labels := map[string]string{
					"k8s.ovn.org/egress-assignable": "",
				}
				node1 := getNodeObj(node1Name, node1Annotations, labels)
				node2Annotations := map[string]string{
					"k8s.ovn.org/node-primary-ifaddr":             fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node2IPv4CIDR, ""),
					"k8s.ovn.org/node-subnets":                    fmt.Sprintf("{\"default\":\"%s\",\"%s\":\"%s\"}", v4Node2Subnet, networkName1, v4Node2Net1),
					"k8s.ovn.org/node-transit-switch-port-ifaddr": fmt.Sprintf("{\"ipv4\":\"%s/16\"}", v4Node2Tsp),
					"k8s.ovn.org/zone-name":                       node2Name,
					"k8s.ovn.org/remote-zone-migrated":            node2Name,
					util.OVNNodeHostCIDRs:                         fmt.Sprintf("[\"%s\"]", node2IPv4CIDR),
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
						Networks: []string{nodeLogicalRouterIfAddrV4},
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
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name,
						Networks: []string{nodeLogicalRouterIfAddrV4},
					},
					&nbdb.LogicalRouter{
						Name:        netInfo.GetNetworkScopedClusterRouterName(),
						UUID:        netInfo.GetNetworkScopedClusterRouterName() + "-UUID",
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: networkName1, ovntypes.TopologyExternalID: ovntypes.Layer3Topology},
					},
					&nbdb.LogicalRouter{
						UUID:        netInfo.GetNetworkScopedGWRouterName(node1.Name) + "-UUID",
						Name:        netInfo.GetNetworkScopedGWRouterName(node1.Name),
						Ports:       []string{ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name + "-UUID"},
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: networkName1, ovntypes.TopologyExternalID: ovntypes.Layer3Topology},
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
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: networkName1, ovntypes.TopologyExternalID: ovntypes.Layer3Topology},
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
				fakeOvn.controller.zone = node1Name
				fakeOvn.eIPController.zone = node1Name
				fakeOvn.controller.eIPC.nodeZoneState.Store(node1Name, true)
				fakeOvn.controller.eIPC.nodeZoneState.Store(node2Name, false)
				// Add pod IPs to UDN cache
				iUDN, nUDN, _ := net.ParseCIDR(v4Pod1IPNode1Net1 + "/23")
				nUDN.IP = iUDN
				fakeOvn.controller.logicalPortCache.add(&egressPodUDN, "", util.GetNADName(nad.Namespace, nad.Name), "", nil, []*net.IPNet{nUDN})
				err = fakeOvn.networkManager.Start()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.eIPController.ensureL3ClusterRouterPoliciesForNetwork(netInfo)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.eIPController.ensureL3SwitchPoliciesForNode(netInfo, node1Name)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIPNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIPPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressNodes()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				egressSVCServedPodsASv4, _ := buildEgressServiceAddressSets(nil)
				egressIPServedPodsASCDNv4, _ := buildEgressIPServedPodsAddressSets([]string{podV4IP}, ovntypes.DefaultNetworkName, DefaultNetworkControllerName)
				egressNodeIPsASv4, _ := buildEgressIPNodeAddressSets([]string{node1IPv4, node2IPv4})
				egressIPServedPodsASUDNv4, _ := buildEgressIPServedPodsAddressSetsForController([]string{v4Pod1IPNode1Net1}, netInfo.GetNetworkName(), DefaultNetworkControllerName)
				gomega.Eventually(c.IsAddressSetAvailable).Should(gomega.BeTrue())
				dbIDs := udnenabledsvc.GetAddressSetDBIDs()
				udnEnabledSvcV4, _ := addressset.GetTestDbAddrSets(dbIDs, []string{})
				node1LRP := "k8s-node1"
				expectedDatabaseStateTwoEgressNodes := []libovsdbtest.TestData{
					// CDN
					getReRouteStaticRoute(v4ClusterSubnet, nodeLogicalRouterIPv4[0]),
					getReRoutePolicy(podV4IP, "4", "reroute-UUID", []string{nodeLogicalRouterIPv4[0], v4Node2Tsp},
						getEgressIPLRPReRouteDbIDs(eIP.Name, egressPodCDN.Namespace, egressPodCDN.Name, IPFamilyValueV4, ovntypes.DefaultNetworkName, DefaultNetworkControllerName).GetExternalIDs()),
					&nbdb.NAT{
						UUID:        "egressip-nat-UUID",
						LogicalIP:   podV4IP,
						ExternalIP:  egressIP1,
						ExternalIDs: getEgressIPNATDbIDs(egressIPName, egressPodCDN.Namespace, egressPodCDN.Name, IPFamilyValueV4, DefaultNetworkControllerName).GetExternalIDs(),
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
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, ovntypes.DefaultNetworkName, DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    ovntypes.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4ClusterSubnet, config.Gateway.V4JoinSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-service-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, ovntypes.DefaultNetworkName, DefaultNetworkControllerName).GetExternalIDs(),
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
						Networks: []string{"100.64.0.2/29"},
					},
					&nbdb.LogicalRouterPolicy{
						Priority: ovntypes.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
							egressIPServedPodsASCDNv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": ovntypes.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, ovntypes.DefaultNetworkName, DefaultNetworkControllerName).GetExternalIDs(),
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
					getNoReRouteReplyTrafficPolicy(ovntypes.DefaultNetworkName, DefaultNetworkControllerName),
					getDefaultQoSRule(false, ovntypes.DefaultNetworkName, DefaultNetworkControllerName),
					egressSVCServedPodsASv4,
					egressIPServedPodsASCDNv4,
					egressNodeIPsASv4,

					// UDN
					getReRouteStaticRouteForController(v4Net1, nodeLogicalRouterIPv4[0], netInfo.GetNetworkName()),
					getReRoutePolicyForController(egressIPName, eipNamespace2, podName2, v4Pod1IPNode1Net1, eIP1Mark, IPFamilyValueV4, []string{nodeLogicalRouterIPv4[0], v4Node2Tsp}, netInfo.GetNetworkName(), DefaultNetworkControllerName),
					getGWPktMarkLRPForController(eIP1Mark, egressIPName, eipNamespace2, podName2, v4Pod1IPNode1Net1, IPFamilyValueV4, netInfo.GetNetworkName(), DefaultNetworkControllerName),
					getNoReRoutePolicyForUDNEnabledSvc(false, netInfo.GetNetworkName(), DefaultNetworkControllerName, egressIPServedPodsASUDNv4.Name, egressSVCServedPodsASv4.Name, udnEnabledSvcV4.Name),
					&nbdb.LogicalRouterPolicy{
						Priority:    ovntypes.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4Net1, v4Net1),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "udn-default-no-reroute-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, netInfo.GetNetworkName(), DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    ovntypes.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4Net1, config.Gateway.V4JoinSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "udn-no-reroute-service-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, netInfo.GetNetworkName(), DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority: ovntypes.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
							egressIPServedPodsASUDNv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "udn-default-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": ovntypes.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, netInfo.GetNetworkName(), DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name,
						Networks: []string{nodeLogicalRouterIfAddrV4},
					},
					&nbdb.LogicalRouter{
						Name:        netInfo.GetNetworkScopedClusterRouterName(),
						UUID:        netInfo.GetNetworkScopedClusterRouterName() + "-UUID",
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: netInfo.GetNetworkName(), ovntypes.TopologyExternalID: ovntypes.Layer3Topology},
						Policies: []string{"udn-default-no-reroute-node-UUID", "udn-default-no-reroute-UUID", "udn-no-reroute-service-UUID", "udn-enabled-svc-no-reroute-UUID",
							fmt.Sprintf("%s-no-reroute-reply-traffic", netInfo.GetNetworkName()),
							getReRoutePolicyUUID(eipNamespace2, podName2, IPFamilyValueV4, netInfo.GetNetworkName())},
						StaticRoutes: []string{fmt.Sprintf("%s-reroute-static-route-UUID", netInfo.GetNetworkName())},
					},
					&nbdb.LogicalRouter{
						UUID:        netInfo.GetNetworkScopedGWRouterName(node1.Name) + "-UUID",
						Name:        netInfo.GetNetworkScopedGWRouterName(node1.Name),
						Ports:       []string{ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name + "-UUID"},
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: netInfo.GetNetworkName(), ovntypes.TopologyExternalID: ovntypes.Layer3Topology},
						Policies:    []string{getGWPktMarkLRPUUID(eipNamespace2, podName2, IPFamilyValueV4, netInfo.GetNetworkName())},
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
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: netInfo.GetNetworkName(), ovntypes.TopologyExternalID: ovntypes.Layer3Topology},
						QOSRules:    []string{fmt.Sprintf("%s-QoS-UUID", netInfo.GetNetworkName())},
					},
					getNoReRouteReplyTrafficPolicyForController(netInfo.GetNetworkName(), DefaultNetworkControllerName),
					getDefaultQoSRule(false, netInfo.GetNetworkName(), DefaultNetworkControllerName),
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
				egressCDNNamespace := newNamespaceWithLabels(eipNamespace, egressPodLabel)
				egressUDNNamespace := newNamespaceWithLabels(eipNamespace2, egressPodLabel)
				egressPodCDNLocal := *newPodWithLabels(eipNamespace, podName, node1Name, podV4IP, nil)
				egressPodUDNLocal := *newPodWithLabels(eipNamespace2, podName2, node1Name, v4Pod1IPNode1Net1, nil)
				egressPodCDNRemote := *newPodWithLabels(eipNamespace, podName3, node2Name, podV4IP2, egressPodLabel)
				setPrimaryNetworkAnnot(&egressPodCDNRemote, ovntypes.DefaultNetworkName, fmt.Sprintf("%s%s", podV4IP2, util.GetIPFullMaskString(podV4IP2)))
				egressPodUDNRemote := *newPodWithLabels(eipNamespace2, podName4, node2Name, v4Pod2IPNode2Net1, egressPodLabel)
				setPrimaryNetworkAnnot(&egressPodUDNRemote, nadName, fmt.Sprintf("%s%s", v4Pod2IPNode2Net1, util.GetIPFullMaskString(v4Pod2IPNode2Net1)))
				netconf := ovncnitypes.NetConf{
					NetConf: cnitypes.NetConf{
						Name: networkName1,
						Type: "ovn-k8s-cni-overlay",
					},
					Role:     ovntypes.NetworkRolePrimary,
					Topology: ovntypes.Layer3Topology,
					NADName:  nadName,
					Subnets:  v4Net1,
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
					"k8s.ovn.org/zone-name":                       node1Name,
					"k8s.ovn.org/remote-zone-migrated":            node1Name,
					util.OVNNodeHostCIDRs:                         fmt.Sprintf("[\"%s\"]", node1IPv4CIDR),
				}
				labels := map[string]string{
					"k8s.ovn.org/egress-assignable": "",
				}
				node1 := getNodeObj(node1Name, node1Annotations, labels)
				node2Annotations := map[string]string{
					"k8s.ovn.org/node-primary-ifaddr":             fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", node2IPv4CIDR, ""),
					"k8s.ovn.org/node-subnets":                    fmt.Sprintf("{\"default\":\"%s\",\"%s\":\"%s\"}", v4Node2Subnet, networkName1, v4Node2Net1),
					"k8s.ovn.org/node-transit-switch-port-ifaddr": fmt.Sprintf("{\"ipv4\":\"%s/16\"}", v4Node2Tsp),
					"k8s.ovn.org/zone-name":                       node2Name,
					"k8s.ovn.org/remote-zone-migrated":            node2Name,
					util.OVNNodeHostCIDRs:                         fmt.Sprintf("[\"%s\"]", node2IPv4CIDR),
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
						Networks: []string{nodeLogicalRouterIfAddrV4},
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
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name,
						Networks: []string{nodeLogicalRouterIfAddrV4},
					},
					&nbdb.LogicalRouter{
						Name:        netInfo.GetNetworkScopedClusterRouterName(),
						UUID:        netInfo.GetNetworkScopedClusterRouterName() + "-UUID",
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: networkName1, ovntypes.TopologyExternalID: ovntypes.Layer3Topology},
					},
					&nbdb.LogicalRouter{
						UUID:        netInfo.GetNetworkScopedGWRouterName(node1.Name) + "-UUID",
						Name:        netInfo.GetNetworkScopedGWRouterName(node1.Name),
						Ports:       []string{ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name + "-UUID"},
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: networkName1, ovntypes.TopologyExternalID: ovntypes.Layer3Topology},
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
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: networkName1, ovntypes.TopologyExternalID: ovntypes.Layer3Topology},
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
				err = fakeOvn.networkManager.Start()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.controller.zone = node1Name
				fakeOvn.eIPController.zone = node1Name
				err = fakeOvn.controller.WatchEgressIPNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIPPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressNodes()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchEgressIP()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				secConInfo, ok := fakeOvn.secondaryControllers[networkName1]
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
				egressIPServedPodsASCDNv4, _ := buildEgressIPServedPodsAddressSets([]string{podV4IP}, ovntypes.DefaultNetworkName, DefaultNetworkControllerName)
				egressNodeIPsASv4, _ := buildEgressIPNodeAddressSets([]string{node1IPv4, node2IPv4})
				egressIPServedPodsASUDNv4, _ := buildEgressIPServedPodsAddressSetsForController([]string{v4Pod1IPNode1Net1}, netInfo.GetNetworkName(), DefaultNetworkControllerName)
				gomega.Eventually(c.IsAddressSetAvailable).Should(gomega.BeTrue())
				dbIDs := udnenabledsvc.GetAddressSetDBIDs()
				udnEnabledSvcV4, _ := addressset.GetTestDbAddrSets(dbIDs, []string{})
				node1LRP := "k8s-node1"
				expectedDatabaseStateTwoEgressNodes := []libovsdbtest.TestData{
					// CDN
					getReRouteStaticRoute(v4ClusterSubnet, nodeLogicalRouterIPv4[0]),
					getReRoutePolicy(podV4IP, "4", "reroute-UUID", []string{nodeLogicalRouterIPv4[0], v4Node2Tsp},
						getEgressIPLRPReRouteDbIDs(eIP.Name, egressPodCDNLocal.Namespace, egressPodCDNLocal.Name, IPFamilyValueV4, ovntypes.DefaultNetworkName, DefaultNetworkControllerName).GetExternalIDs()),
					&nbdb.LogicalRouterPolicy{
						Priority:    ovntypes.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4ClusterSubnet, v4ClusterSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-no-reroute-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, ovntypes.DefaultNetworkName, DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    ovntypes.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4ClusterSubnet, config.Gateway.V4JoinSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "no-reroute-service-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, ovntypes.DefaultNetworkName, DefaultNetworkControllerName).GetExternalIDs(),
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
						Networks: []string{"100.64.0.2/29"},
					},
					&nbdb.LogicalRouterPolicy{
						Priority: ovntypes.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
							egressIPServedPodsASCDNv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "default-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": ovntypes.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, ovntypes.DefaultNetworkName, DefaultNetworkControllerName).GetExternalIDs(),
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
						ExternalIDs: getEgressIPNATDbIDs(egressIPName, egressPodCDNRemote.Namespace, egressPodCDNRemote.Name, IPFamilyValueV4, DefaultNetworkControllerName).GetExternalIDs(),
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
						ExternalIDs: getEgressIPNATDbIDs(egressIPName, egressPodCDNLocal.Namespace, egressPodCDNLocal.Name, IPFamilyValueV4, DefaultNetworkControllerName).GetExternalIDs(),
						Type:        nbdb.NATTypeSNAT,
						LogicalPort: &node1LRP,
						Options: map[string]string{
							"stateless": "false",
						},
					},
					getNoReRouteReplyTrafficPolicy(ovntypes.DefaultNetworkName, DefaultNetworkControllerName),
					getDefaultQoSRule(false, ovntypes.DefaultNetworkName, DefaultNetworkControllerName),
					egressSVCServedPodsASv4,
					egressIPServedPodsASCDNv4,
					egressNodeIPsASv4,

					// UDN
					getReRouteStaticRouteForController(v4Net1, nodeLogicalRouterIPv4[0], netInfo.GetNetworkName()),
					getReRoutePolicyForController(egressIPName, eipNamespace2, podName2, v4Pod1IPNode1Net1, eIP1Mark, IPFamilyValueV4, []string{nodeLogicalRouterIPv4[0], v4Node2Tsp}, netInfo.GetNetworkName(), DefaultNetworkControllerName),
					getGWPktMarkLRPForController(eIP1Mark, egressIPName, eipNamespace2, podName2, v4Pod1IPNode1Net1, IPFamilyValueV4, netInfo.GetNetworkName(), DefaultNetworkControllerName),
					getGWPktMarkLRPForController(eIP1Mark, egressIPName, eipNamespace2, podName4, v4Pod2IPNode2Net1, IPFamilyValueV4, netInfo.GetNetworkName(), DefaultNetworkControllerName),
					getNoReRoutePolicyForUDNEnabledSvc(false, netInfo.GetNetworkName(), DefaultNetworkControllerName, egressIPServedPodsASUDNv4.Name, egressSVCServedPodsASv4.Name, udnEnabledSvcV4.Name),
					&nbdb.LogicalRouterPolicy{
						Priority:    ovntypes.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4Net1, v4Net1),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "udn-default-no-reroute-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, netInfo.GetNetworkName(), DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority:    ovntypes.DefaultNoRereoutePriority,
						Match:       fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4Net1, config.Gateway.V4JoinSubnet),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "udn-no-reroute-service-UUID",
						ExternalIDs: getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, netInfo.GetNetworkName(), DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPolicy{
						Priority: ovntypes.DefaultNoRereoutePriority,
						Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
							egressIPServedPodsASUDNv4.Name, egressSVCServedPodsASv4.Name, egressNodeIPsASv4.Name),
						Action:      nbdb.LogicalRouterPolicyActionAllow,
						UUID:        "udn-default-no-reroute-node-UUID",
						Options:     map[string]string{"pkt_mark": ovntypes.EgressIPNodeConnectionMark},
						ExternalIDs: getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, netInfo.GetNetworkName(), DefaultNetworkControllerName).GetExternalIDs(),
					},
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name,
						Networks: []string{nodeLogicalRouterIfAddrV4},
					},
					&nbdb.LogicalRouter{
						Name:        netInfo.GetNetworkScopedClusterRouterName(),
						UUID:        netInfo.GetNetworkScopedClusterRouterName() + "-UUID",
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: netInfo.GetNetworkName(), ovntypes.TopologyExternalID: ovntypes.Layer3Topology},
						Policies: []string{"udn-default-no-reroute-node-UUID", "udn-default-no-reroute-UUID", "udn-no-reroute-service-UUID", "udn-enabled-svc-no-reroute-UUID",
							fmt.Sprintf("%s-no-reroute-reply-traffic", netInfo.GetNetworkName()),
							getReRoutePolicyUUID(eipNamespace2, podName2, IPFamilyValueV4, netInfo.GetNetworkName())},
						StaticRoutes: []string{fmt.Sprintf("%s-reroute-static-route-UUID", netInfo.GetNetworkName())},
					},
					&nbdb.LogicalRouter{
						UUID:        netInfo.GetNetworkScopedGWRouterName(node1.Name) + "-UUID",
						Name:        netInfo.GetNetworkScopedGWRouterName(node1.Name),
						Ports:       []string{ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + networkName1_ + node1.Name + "-UUID"},
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: netInfo.GetNetworkName(), ovntypes.TopologyExternalID: ovntypes.Layer3Topology},
						Policies: []string{getGWPktMarkLRPUUID(eipNamespace2, podName2, IPFamilyValueV4, netInfo.GetNetworkName()),
							getGWPktMarkLRPUUID(eipNamespace2, podName4, IPFamilyValueV4, netInfo.GetNetworkName())},
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
						ExternalIDs: map[string]string{ovntypes.NetworkExternalID: netInfo.GetNetworkName(), ovntypes.TopologyExternalID: ovntypes.Layer3Topology},
						QOSRules:    []string{fmt.Sprintf("%s-QoS-UUID", netInfo.GetNetworkName())},
					},
					getNoReRouteReplyTrafficPolicyForController(netInfo.GetNetworkName(), DefaultNetworkControllerName),
					getDefaultQoSRule(false, netInfo.GetNetworkName(), DefaultNetworkControllerName),
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
})

// returns the address set with externalID "k8s.ovn.org/name": "egressip-served-pods""
func buildEgressIPServedPodsAddressSetsForController(ips []string, network, controller string) (*nbdb.AddressSet, *nbdb.AddressSet) {
	dbIDs := getEgressIPAddrSetDbIDs(EgressIPServedPodsAddrSetName, network, controller)
	return addressset.GetTestDbAddrSets(dbIDs, ips)

}

// returns the address set with externalID "k8s.ovn.org/name": "node-ips"
func buildEgressIPNodeAddressSetsForController(ips []string) (*nbdb.AddressSet, *nbdb.AddressSet) {
	dbIDs := getEgressIPAddrSetDbIDs(NodeIPAddrSetName, ovntypes.DefaultNetworkName, DefaultNetworkControllerName)
	return addressset.GetTestDbAddrSets(dbIDs, ips)
}

// returns the LRP for marking reply traffic and not routing
func getNoReRouteReplyTrafficPolicyForController(network, controller string) *nbdb.LogicalRouterPolicy {
	return &nbdb.LogicalRouterPolicy{
		Priority:    ovntypes.DefaultNoRereoutePriority,
		Match:       fmt.Sprintf("pkt.mark == %d", ovntypes.EgressIPReplyTrafficConnectionMark),
		Action:      nbdb.LogicalRouterPolicyActionAllow,
		ExternalIDs: getEgressIPLRPNoReRouteDbIDs(ovntypes.DefaultNoRereoutePriority, ReplyTrafficNoReroute, IPFamilyValue, network, controller).GetExternalIDs(),
		UUID:        fmt.Sprintf("%s-no-reroute-reply-traffic", network),
	}
}

func getReRouteStaticRouteForController(clusterSubnet, nextHop, network string) *nbdb.LogicalRouterStaticRoute {
	return &nbdb.LogicalRouterStaticRoute{
		Nexthop:  nextHop,
		Policy:   &nbdb.LogicalRouterStaticRoutePolicySrcIP,
		IPPrefix: clusterSubnet,
		UUID:     fmt.Sprintf("%s-reroute-static-route-UUID", network),
	}
}

func getReRoutePolicyForController(eIPName, podNamespace, podName, podIP string, mark int, ipFamily egressIPFamilyValue, nextHops []string, network, controller string) *nbdb.LogicalRouterPolicy {
	return &nbdb.LogicalRouterPolicy{
		Priority:    ovntypes.EgressIPReroutePriority,
		Match:       fmt.Sprintf("%s.src == %s", ipFamily, podIP),
		Action:      nbdb.LogicalRouterPolicyActionReroute,
		Nexthops:    nextHops,
		ExternalIDs: getEgressIPLRPReRouteDbIDs(eIPName, podNamespace, podName, ipFamily, network, controller).GetExternalIDs(),
		Options:     getMarkOptions(mark),
		UUID:        getReRoutePolicyUUID(podNamespace, podName, ipFamily, network),
	}
}

func getNoReRoutePolicyForUDNEnabledSvc(v6 bool, network, controllerName, eipSrcASHash, eSvcSrcASHash, udnEnabledSvcASHash string) *nbdb.LogicalRouterPolicy {
	family := IPFamilyValueV4
	if v6 {
		family = IPFamilyValueV6
	}
	return &nbdb.LogicalRouterPolicy{
		Priority:    ovntypes.DefaultNoRereoutePriority,
		Match:       fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s", eipSrcASHash, eSvcSrcASHash, udnEnabledSvcASHash),
		Action:      nbdb.LogicalRouterPolicyActionAllow,
		UUID:        "udn-enabled-svc-no-reroute-UUID",
		ExternalIDs: getEgressIPLRPNoReRouteDbIDs(ovntypes.DefaultNoRereoutePriority, NoReRouteUDNPodToCDNSvc, family, network, controllerName).GetExternalIDs(),
	}
}

func getReRoutePolicyUUID(podNamespace, podName string, ipFamily egressIPFamilyValue, network string) string {
	return fmt.Sprintf("%s-reroute-%s-%s-%s", network, podNamespace, podName, ipFamily)
}

func getGWPktMarkLRPForController(mark int, eIPName, podNamespace, podName, podIP string, ipFamily egressIPFamilyValue, network, controller string) *nbdb.LogicalRouterPolicy {
	dbIDs := getEgressIPLRPSNATMarkDbIDs(eIPName, podNamespace, podName, ipFamily, network, controller)
	return &nbdb.LogicalRouterPolicy{
		UUID:        getGWPktMarkLRPUUID(podNamespace, podName, ipFamily, network),
		Priority:    ovntypes.EgressIPSNATMarkPriority,
		Action:      nbdb.LogicalRouterPolicyActionAllow,
		ExternalIDs: dbIDs.GetExternalIDs(),
		Options:     getMarkOptions(mark),
		Match:       fmt.Sprintf("%s.src == %s && pkt.mark == 0", ipFamily, podIP),
	}
}

func getGWPktMarkLRPUUID(podNamespace, podName string, ipFamily egressIPFamilyValue, network string) string {
	return fmt.Sprintf("%s-gw-pkt-mark-%s-%s-%s-UUID", network, podNamespace, podName, ipFamily)
}

func getMarkOptions(mark int) map[string]string {
	return map[string]string{"pkt_mark": fmt.Sprintf("%d", mark)}
}

// jsonPatchOperation contains all the info needed to perform a JSON path operation to a k8 object
type jsonPatchOperation struct {
	Operation string      `json:"op"`
	Path      string      `json:"path"`
	Value     interface{} `json:"value,omitempty"`
}

type patchFn func(name string, patchData []byte) error

func patchEgressIP(patchFn patchFn, name string, patches ...jsonPatchOperation) error {
	klog.Infof("Patching status on EgressIP %s: %v", name, patches)
	op, err := json.Marshal(patches)
	if err != nil {
		return fmt.Errorf("error serializing patch operation: %+v, err: %v", patches, err)
	}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		return patchFn(name, op)
	})
}

func generateEgressIPPatches(mark int, statusItems []egressipv1.EgressIPStatusItem) []jsonPatchOperation {
	patches := make([]jsonPatchOperation, 0, 1)
	patches = append(patches, generateMarkPatchOp(mark))
	return append(patches, generateStatusPatchOp(statusItems))
}

func generateMarkPatchOp(mark int) jsonPatchOperation {
	return jsonPatchOperation{
		Operation: "add",
		Path:      "/metadata/annotations",
		Value:     createAnnotWithMark(mark),
	}
}

func createAnnotWithMark(mark int) map[string]string {
	return map[string]string{util.EgressIPMarkAnnotation: fmt.Sprintf("%d", mark)}
}

func generateStatusPatchOp(statusItems []egressipv1.EgressIPStatusItem) jsonPatchOperation {
	return jsonPatchOperation{
		Operation: "replace",
		Path:      "/status",
		Value: egressipv1.EgressIPStatus{
			Items: statusItems,
		},
	}
}

func newEgressIPMetaWithMark(name string, mark int) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		UID:  k8stypes.UID(name),
		Name: name,
		Labels: map[string]string{
			"name": name,
		},
		Annotations: map[string]string{util.EgressIPMarkAnnotation: fmt.Sprintf("%d", mark)},
	}
}
