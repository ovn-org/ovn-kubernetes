package ovn

import (
	"context"
	"fmt"

	"github.com/urfave/cli/v2"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/onsi/gomega/format"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func getMulticastExpectedData(netInfo util.NetInfo, clusterPortGroup, clusterRtrPortGroup *nbdb.PortGroup) []libovsdb.TestData {
	netControllerName := getNetworkControllerName(netInfo.GetNetworkName())
	match := getMulticastACLMatch()
	aclIDs := getDefaultMcastACLDbIDs(mcastDefaultDenyID, libovsdbutil.ACLEgress, netControllerName)
	aclName := libovsdbutil.GetACLName(aclIDs)
	defaultDenyEgressACL := libovsdbops.BuildACL(
		aclName,
		nbdb.ACLDirectionFromLport,
		types.DefaultMcastDenyPriority,
		match,
		nbdb.ACLActionDrop,
		types.OvnACLLoggingMeter,
		"",
		false,
		aclIDs.GetExternalIDs(),
		map[string]string{
			"apply-after-lb": "true",
		},
		types.DefaultACLTier,
	)
	defaultDenyEgressACL.UUID = "defaultDenyEgressACL_UUID"

	aclIDs = getDefaultMcastACLDbIDs(mcastDefaultDenyID, libovsdbutil.ACLIngress, netControllerName)
	aclName = libovsdbutil.GetACLName(aclIDs)
	defaultDenyIngressACL := libovsdbops.BuildACL(
		aclName,
		nbdb.ACLDirectionToLport,
		types.DefaultMcastDenyPriority,
		match,
		nbdb.ACLActionDrop,
		types.OvnACLLoggingMeter,
		"",
		false,
		aclIDs.GetExternalIDs(),
		nil,
		types.DefaultACLTier,
	)
	defaultDenyIngressACL.UUID = "defaultDenyIngressACL_UUID"
	clusterPortGroup.ACLs = []string{defaultDenyEgressACL.UUID, defaultDenyIngressACL.UUID}

	aclIDs = getDefaultMcastACLDbIDs(mcastAllowInterNodeID, libovsdbutil.ACLEgress, netControllerName)
	aclName = libovsdbutil.GetACLName(aclIDs)
	egressMatch := libovsdbutil.GetACLMatch(clusterRtrPortGroup.Name, match, libovsdbutil.ACLEgress)
	defaultAllowEgressACL := libovsdbops.BuildACL(
		aclName,
		nbdb.ACLDirectionFromLport,
		types.DefaultMcastAllowPriority,
		egressMatch,
		nbdb.ACLActionAllow,
		types.OvnACLLoggingMeter,
		"",
		false,
		aclIDs.GetExternalIDs(),
		map[string]string{
			"apply-after-lb": "true",
		},
		types.DefaultACLTier,
	)
	defaultAllowEgressACL.UUID = "defaultAllowEgressACL_UUID"

	aclIDs = getDefaultMcastACLDbIDs(mcastAllowInterNodeID, libovsdbutil.ACLIngress, netControllerName)
	aclName = libovsdbutil.GetACLName(aclIDs)
	ingressMatch := libovsdbutil.GetACLMatch(clusterRtrPortGroup.Name, match, libovsdbutil.ACLIngress)
	defaultAllowIngressACL := libovsdbops.BuildACL(
		aclName,
		nbdb.ACLDirectionToLport,
		types.DefaultMcastAllowPriority,
		ingressMatch,
		nbdb.ACLActionAllow,
		types.OvnACLLoggingMeter,
		"",
		false,
		aclIDs.GetExternalIDs(),
		nil,
		types.DefaultACLTier,
	)
	defaultAllowIngressACL.UUID = "defaultAllowIngressACL_UUID"
	clusterRtrPortGroup.ACLs = []string{defaultAllowEgressACL.UUID, defaultAllowIngressACL.UUID}
	return []libovsdb.TestData{
		defaultDenyIngressACL,
		defaultDenyEgressACL,
		defaultAllowEgressACL,
		defaultAllowIngressACL,
		clusterPortGroup,
		clusterRtrPortGroup,
	}
}

func getMulticastStaleData(netInfo util.NetInfo, clusterPortGroup, clusterRtrPortGroup *nbdb.PortGroup) []libovsdb.TestData {
	testData := getMulticastExpectedData(netInfo, clusterPortGroup, clusterRtrPortGroup)
	defaultDenyIngressACL := testData[0].(*nbdb.ACL)
	newName := libovsdbutil.JoinACLName(types.ClusterPortGroupNameBase, "DefaultDenyMulticastIngress")
	defaultDenyIngressACL.Name = &newName
	defaultDenyIngressACL.Options = nil

	defaultDenyEgressACL := testData[1].(*nbdb.ACL)
	newName1 := libovsdbutil.JoinACLName(types.ClusterPortGroupNameBase, "DefaultDenyMulticastEgress")
	defaultDenyEgressACL.Name = &newName1
	defaultDenyEgressACL.Options = nil

	defaultAllowEgressACL := testData[2].(*nbdb.ACL)
	newName2 := libovsdbutil.JoinACLName(types.ClusterRtrPortGroupNameBase, "DefaultAllowMulticastEgress")
	defaultAllowEgressACL.Name = &newName2
	defaultAllowEgressACL.Options = nil

	defaultAllowIngressACL := testData[3].(*nbdb.ACL)
	newName3 := libovsdbutil.JoinACLName(types.ClusterRtrPortGroupNameBase, "DefaultAllowMulticastIngress")
	defaultAllowIngressACL.Name = &newName3
	defaultAllowIngressACL.Options = nil

	return []libovsdb.TestData{
		defaultDenyIngressACL,
		defaultDenyEgressACL,
		defaultAllowEgressACL,
		defaultAllowIngressACL,
		testData[4],
		testData[5],
	}
}

func getMulticastPolicyExpectedData(netInfo util.NetInfo, ns string, ports []string) []libovsdb.TestData {
	netControllerName := getNetworkControllerName(netInfo.GetNetworkName())
	fakeController := getFakeController(netControllerName)
	pg_hash := fakeController.getNamespacePortGroupName(ns)
	egressMatch := libovsdbutil.GetACLMatch(pg_hash, fakeController.getMulticastACLEgrMatch(), libovsdbutil.ACLEgress)

	ip4AddressSet, ip6AddressSet := getNsAddrSetHashNames(netControllerName, ns)
	mcastMatch := getACLMatchAF(getMulticastACLIgrMatchV4(ip4AddressSet), getMulticastACLIgrMatchV6(ip6AddressSet), config.IPv4Mode, config.IPv6Mode)
	ingressMatch := libovsdbutil.GetACLMatch(pg_hash, mcastMatch, libovsdbutil.ACLIngress)

	aclIDs := getNamespaceMcastACLDbIDs(ns, libovsdbutil.ACLEgress, netControllerName)
	aclName := libovsdbutil.GetACLName(aclIDs)
	egressACL := libovsdbops.BuildACL(
		aclName,
		nbdb.ACLDirectionFromLport,
		types.DefaultMcastAllowPriority,
		egressMatch,
		nbdb.ACLActionAllow,
		types.OvnACLLoggingMeter,
		"",
		false,
		aclIDs.GetExternalIDs(),
		map[string]string{
			"apply-after-lb": "true",
		},
		types.DefaultACLTier,
	)
	egressACL.UUID = ns + "mc-egress-UUID"

	aclIDs = getNamespaceMcastACLDbIDs(ns, libovsdbutil.ACLIngress, netControllerName)
	aclName = libovsdbutil.GetACLName(aclIDs)
	ingressACL := libovsdbops.BuildACL(
		aclName,
		nbdb.ACLDirectionToLport,
		types.DefaultMcastAllowPriority,
		ingressMatch,
		nbdb.ACLActionAllow,
		types.OvnACLLoggingMeter,
		"",
		false,
		aclIDs.GetExternalIDs(),
		nil,
		types.DefaultACLTier,
	)
	ingressACL.UUID = ns + "mc-ingress-UUID"

	lsps := []*nbdb.LogicalSwitchPort{}
	for _, uuid := range ports {
		lsps = append(lsps, &nbdb.LogicalSwitchPort{UUID: uuid})
	}

	pgDbIDs := getNamespacePortGroupDbIDs(ns, fakeController.controllerName)
	pg := libovsdbutil.BuildPortGroup(
		pgDbIDs,
		lsps,
		[]*nbdb.ACL{egressACL, ingressACL},
	)
	pg.UUID = pg.Name + "-UUID"

	return []libovsdb.TestData{
		egressACL,
		ingressACL,
		pg,
	}
}

func getNamespacePG(ns, controllerName string) *nbdb.PortGroup {
	pgDbIDs := getNamespacePortGroupDbIDs(ns, controllerName)
	pg := libovsdbutil.BuildPortGroup(pgDbIDs, nil, nil)
	pg.UUID = pg.Name + "-UUID"
	return pg
}

func getMulticastPolicyStaleData(netInfo util.NetInfo, ns string, ports []string) []libovsdb.TestData {
	testData := getMulticastPolicyExpectedData(netInfo, ns, ports)

	egressACL := testData[0].(*nbdb.ACL)
	newName := libovsdbutil.JoinACLName(ns, "MulticastAllowEgress")
	egressACL.Name = &newName
	egressACL.Options = nil

	ingressACL := testData[1].(*nbdb.ACL)
	newName1 := libovsdbutil.JoinACLName(ns, "MulticastAllowIngress")
	ingressACL.Name = &newName1
	ingressACL.Options = nil

	return []libovsdb.TestData{
		egressACL,
		ingressACL,
		testData[2],
	}
}

func getNetInfoFromNAD(nad *nadapi.NetworkAttachmentDefinition) util.NetInfo {
	if nad == nil {
		return &util.DefaultNetInfo{}
	}
	netInfo, err := util.ParseNADInfo(nad)
	Expect(err).NotTo(HaveOccurred())
	return netInfo
}

func getNodeData(netInfo util.NetInfo, nodeName string) []libovsdb.TestData {
	switchName := netInfo.GetNetworkScopedSwitchName(nodeName)
	return []libovsdb.TestData{
		&nbdb.LogicalSwitch{
			UUID: switchName + "_UUID",
			Name: switchName,
		},
	}
}

func newNodeWithNad(nad *nadapi.NetworkAttachmentDefinition, networkName, networkID string) *v1.Node {
	n := newNode(nodeName, "192.168.126.202/24")
	if nad != nil {
		n.Annotations["k8s.ovn.org/node-subnets"] = fmt.Sprintf("{\"default\":\"192.168.126.202/24\", \"%s\":\"192.168.127.202/24\"}", networkName)
		n.Annotations["k8s.ovn.org/network-ids"] = fmt.Sprintf("{\"default\":\"0\",\"%s\":\"%s\"}", networkName, networkID)
		n.Annotations["k8s.ovn.org/node-mgmt-port-mac-addresses"] = fmt.Sprintf("{\"default\":\"96:8f:e8:25:a2:e5\",\"%s\":\"d6:bc:85:32:30:fb\"}", networkName)
		n.Annotations["k8s.ovn.org/node-chassis-id"] = "abdcef"
		n.Annotations["k8s.ovn.org/l3-gateway-config"] = "{\"default\":{\"mac-address\":\"52:54:00:e2:ed:d0\",\"ip-addresses\":[\"10.1.1.10/24\"],\"ip-address\":\"10.1.1.10/24\",\"next-hops\":[\"10.1.1.1\"],\"next-hop\":\"10.1.1.1\"}}"
		n.Annotations["k8s.ovn.org/node-gateway-router-lrp-ifaddrs"] = fmt.Sprintf("{\"default\":{\"ipv4\":\"100.64.0.4/16\"},\"%s\":{\"ipv4\":\"100.65.0.4/16\"}}", networkName)
	}
	return n
}

func createTestPods(nodeName, namespace string, useIPv4, useIPv6 bool) (pods []v1.Pod, tPods []testPod, tPodIPs []string) {
	nPodTestV4 := newTPod(
		nodeName,
		"10.128.1.0/24",
		"10.128.1.2",
		"10.128.1.1",
		"myPod1",
		"10.128.1.3",
		"0a:58:0a:80:01:03",
		namespace,
	)
	nPodTestV6 := newTPod(
		nodeName,
		"fd00:10:244::/64",
		"fd00:10:244::2",
		"fd00:10:244::1",
		"myPod2",
		"fd00:10:244::3",
		"0a:58:dd:33:05:d8",
		namespace,
	)
	if useIPv4 {
		tPods = append(tPods, nPodTestV4)
		tPodIPs = append(tPodIPs, nPodTestV4.podIP)
	}
	if useIPv6 {
		tPods = append(tPods, nPodTestV6)
		tPodIPs = append(tPodIPs, nPodTestV6.podIP)
	}
	for _, tPod := range tPods {
		pods = append(pods, *newPod(tPod.namespace, tPod.podName, tPod.nodeName, tPod.podIP))
	}
	return
}

func updateMulticast(fakeOvn *FakeOVN, ns *v1.Namespace, enable bool) {
	if enable {
		ns.Annotations[util.NsMulticastAnnotation] = "true"
	} else {
		ns.Annotations[util.NsMulticastAnnotation] = "false"
	}
	_, err := fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Update(context.TODO(), ns, metav1.UpdateOptions{})
	Expect(err).NotTo(HaveOccurred())
}

func startBaseNetworkController(fakeOvn *FakeOVN, nad *nadapi.NetworkAttachmentDefinition) (*BaseNetworkController, *addressset.FakeAddressSetFactory) {
	if nad != nil {
		netInfo, err := util.ParseNADInfo(nad)
		Expect(err).ToNot(HaveOccurred())
		Expect(fakeOvn.NewSecondaryNetworkController(nad)).To(Succeed())
		controller, ok := fakeOvn.secondaryControllers[netInfo.GetNetworkName()]
		Expect(ok).To(BeTrue())
		return &controller.bnc.BaseNetworkController, controller.asf
	} else {
		return &fakeOvn.controller.BaseNetworkController, fakeOvn.asf
	}
}

var _ = Describe("OVN Multicast with IP Address Family", func() {
	const (
		namespaceName1 = "namespace1"
	)

	var (
		app                   *cli.App
		fakeOvn               *FakeOVN
		gomegaFormatMaxLength int
		networkName           = "bluenet"
		nadName               = "rednad"
		networkID             = "50"

		nadFromIPMode = func(useIPv4, useIPv6 bool) *nadapi.NetworkAttachmentDefinition {
			var nad *nadapi.NetworkAttachmentDefinition
			if useIPv4 && useIPv6 {
				nad = ovntest.GenerateNAD(networkName, nadName, namespaceName1,
					types.Layer3Topology, "100.128.0.0/16,ae70::66/60", types.NetworkRolePrimary)
			} else if useIPv4 {
				nad = ovntest.GenerateNAD(networkName, nadName, namespaceName1,
					types.Layer3Topology, "100.128.0.0/16", types.NetworkRolePrimary)
			} else {
				nad = ovntest.GenerateNAD(networkName, nadName, namespaceName1,
					types.Layer3Topology, "ae70::66/60", types.NetworkRolePrimary)
			}
			nad.Annotations = map[string]string{types.OvnNetworkIDAnnotation: networkID}
			return nad
		}
	)

	BeforeEach(func() {
		// Restore global default values before each testcase
		config.PrepareTestConfig()
		config.EnableMulticast = true
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		config.OVNKubernetesFeature.EnableMultiNetworkPolicy = true
		config.Gateway.V6MasqueradeSubnet = "fd69::/112"
		config.Gateway.V4MasqueradeSubnet = "169.254.0.0/17"

		app = cli.NewApp()
		app.Name = "test"
		// flags are written to config.EnableMulticast
		// if there is no --enable-multicast flag, it will set to false.
		// alternative approach is to give this flag to app.Run, but that require more changes.
		//app.Flags = config.Flags

		fakeOvn = NewFakeOVN(true)
		gomegaFormatMaxLength = format.MaxLength
		format.MaxLength = 0
	})

	AfterEach(func() {
		fakeOvn.shutdown()
		format.MaxLength = gomegaFormatMaxLength
	})

	Context("on startup", func() {
		DescribeTable("creates default Multicast ACLs", func(useIPv4, useIPv6 bool, nad *nadapi.NetworkAttachmentDefinition) {
			app.Action = func(ctx *cli.Context) error {
				config.IPv4Mode = useIPv4
				config.IPv6Mode = useIPv6

				netInfo := getNetInfoFromNAD(nad)
				clusterPortGroup := newNetworkClusterPortGroup(netInfo)
				clusterRtrPortGroup := newNetworkRouterPortGroup(netInfo)
				fakeOvn.startWithDBSetup(libovsdb.TestSetup{
					NBData: []libovsdb.TestData{
						clusterPortGroup,
						clusterRtrPortGroup,
					},
				})
				bnc, _ := startBaseNetworkController(fakeOvn, nad)

				Expect(bnc.createDefaultDenyMulticastPolicy()).To(Succeed())
				Expect(bnc.createDefaultAllowMulticastPolicy()).To(Succeed())

				Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(
					getMulticastExpectedData(netInfo, clusterPortGroup, clusterRtrPortGroup)))
				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		},
			Entry("IPv4", true, false, nil),
			Entry("IPv6", false, true, nil),
			Entry("[Network Segmentation] IPv4", true, false, nadFromIPMode(true, false)),
			Entry("[Network Segmentation] IPv6", false, true, nadFromIPMode(false, true)),
		)

		DescribeTable("updates stale default Multicast ACLs", func(useIPv4, useIPv6 bool, nad *nadapi.NetworkAttachmentDefinition) {
			app.Action = func(ctx *cli.Context) error {
				config.IPv4Mode = useIPv4
				config.IPv6Mode = useIPv6

				// start with stale ACLs
				netInfo := getNetInfoFromNAD(nad)
				clusterPortGroup := newNetworkClusterPortGroup(netInfo)
				clusterRtrPortGroup := newNetworkRouterPortGroup(netInfo)
				fakeOvn.startWithDBSetup(libovsdb.TestSetup{
					NBData: getMulticastStaleData(netInfo, clusterPortGroup, clusterRtrPortGroup),
				})
				bnc, _ := startBaseNetworkController(fakeOvn, nad)

				Expect(bnc.createDefaultDenyMulticastPolicy()).To(Succeed())
				Expect(bnc.createDefaultAllowMulticastPolicy()).To(Succeed())

				// check acls are updated
				Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(
					getMulticastExpectedData(netInfo, clusterPortGroup, clusterRtrPortGroup)))
				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		},
			Entry("IPv4", true, false, nil),
			Entry("IPv6", false, true, nil),
			Entry("[Network Segmentation] IPv4", true, false, nadFromIPMode(true, false)),
			Entry("[Network Segmentation] IPv6", false, true, nadFromIPMode(false, true)),
		)

		DescribeTable("cleans up Multicast resources when multicast is disabled", func(useIPv4, useIPv6 bool, nad *nadapi.NetworkAttachmentDefinition) {
			app.Action = func(ctx *cli.Context) error {
				config.IPv4Mode = useIPv4
				config.IPv6Mode = useIPv6

				netInfo := getNetInfoFromNAD(nad)
				clusterPortGroup := newNetworkClusterPortGroup(netInfo)
				clusterRtrPortGroup := newNetworkRouterPortGroup(netInfo)
				initialData := getMulticastExpectedData(netInfo, clusterPortGroup, clusterRtrPortGroup)

				nsData := getMulticastPolicyExpectedData(netInfo, namespaceName1, nil)
				initialData = append(initialData, nsData...)
				// namespace is still present, but multicast support is disabled
				namespace1 := *newNamespace(namespaceName1)
				fakeOvn.startWithDBSetup(libovsdb.TestSetup{NBData: initialData},
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespace1,
						},
					},
				)
				bnc, _ := startBaseNetworkController(fakeOvn, nad)

				// this "if !oc.multicastSupport" part of SetupMaster
				Expect(bnc.disableMulticast()).To(Succeed())
				// check acls are deleted when multicast is disabled
				clusterPortGroup = newNetworkClusterPortGroup(netInfo)
				clusterRtrPortGroup = newNetworkRouterPortGroup(netInfo)
				namespacePortGroup := getNamespacePG(namespaceName1, getNetworkControllerName(netInfo.GetNetworkName()))
				expectedData := []libovsdb.TestData{
					clusterPortGroup,
					clusterRtrPortGroup,
					namespacePortGroup,
				}
				Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData))
				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		},
			Entry("IPv4", true, false, nil),
			Entry("IPv6", false, true, nil),
			Entry("[Network Segmentation] IPv4", true, false, nadFromIPMode(true, false)),
			Entry("[Network Segmentation] IPv6", false, true, nadFromIPMode(false, true)),
		)

		DescribeTable("creates namespace Multicast ACLs", func(useIPv4, useIPv6 bool, nad *nadapi.NetworkAttachmentDefinition) {
			app.Action = func(ctx *cli.Context) error {
				config.IPv4Mode = useIPv4
				config.IPv6Mode = useIPv6

				netInfo := getNetInfoFromNAD(nad)
				clusterPortGroup := newNetworkClusterPortGroup(netInfo)
				clusterRtrPortGroup := newNetworkRouterPortGroup(netInfo)
				expectedData := getMulticastExpectedData(netInfo, clusterPortGroup, clusterRtrPortGroup)
				// namespace exists, but multicast acls do not
				namespace1 := *newNamespace(namespaceName1)
				namespace1.Annotations[util.NsMulticastAnnotation] = "true"
				fakeOvn.startWithDBSetup(libovsdb.TestSetup{NBData: expectedData},
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespace1,
						},
					},
				)
				bnc, _ := startBaseNetworkController(fakeOvn, nad)

				Expect(bnc.WatchNamespaces()).To(Succeed())
				expectedData = append(expectedData, getMulticastPolicyExpectedData(netInfo, namespaceName1, nil)...)
				Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData))
				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		},
			Entry("IPv4", true, false, nil),
			Entry("IPv6", false, true, nil),
			Entry("[Network Segmentation] IPv4", true, false, nadFromIPMode(true, false)),
			Entry("[Network Segmentation] IPv6", false, true, nadFromIPMode(false, true)),
		)

		DescribeTable("updates stale namespace Multicast ACLs", func(useIPv4, useIPv6 bool, nad *nadapi.NetworkAttachmentDefinition) {
			app.Action = func(ctx *cli.Context) error {
				config.IPv4Mode = useIPv4
				config.IPv6Mode = useIPv6

				// start with stale ACLs for existing namespace
				netInfo := getNetInfoFromNAD(nad)
				clusterPortGroup := newNetworkClusterPortGroup(netInfo)
				clusterRtrPortGroup := newNetworkRouterPortGroup(netInfo)
				expectedData := getMulticastExpectedData(netInfo, clusterPortGroup, clusterRtrPortGroup)
				expectedData = append(expectedData, getMulticastPolicyStaleData(netInfo, namespaceName1, nil)...)
				namespace1 := *newNamespace(namespaceName1)
				namespace1.Annotations[util.NsMulticastAnnotation] = "true"
				fakeOvn.startWithDBSetup(libovsdb.TestSetup{NBData: expectedData},
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespace1,
						},
					},
				)
				bnc, _ := startBaseNetworkController(fakeOvn, nad)

				Expect(bnc.WatchNamespaces()).To(Succeed())
				expectedData = getMulticastExpectedData(netInfo, clusterPortGroup, clusterRtrPortGroup)
				expectedData = append(expectedData, getMulticastPolicyExpectedData(netInfo, namespaceName1, nil)...)
				Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData))
				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		},
			Entry("IPv4", true, false, nil),
			Entry("IPv6", false, true, nil),
			Entry("[Network Segmentation] IPv4", true, false, nadFromIPMode(true, false)),
			Entry("[Network Segmentation] IPv6", false, true, nadFromIPMode(false, true)),
		)

		DescribeTable("cleans up namespace Multicast ACLs when multicast is disabled for namespace", func(useIPv4, useIPv6 bool, nad *nadapi.NetworkAttachmentDefinition) {
			app.Action = func(ctx *cli.Context) error {
				config.IPv4Mode = useIPv4
				config.IPv6Mode = useIPv6

				// start with stale ACLs
				netInfo := getNetInfoFromNAD(nad)
				clusterPortGroup := newNetworkClusterPortGroup(netInfo)
				clusterRtrPortGroup := newNetworkRouterPortGroup(netInfo)
				defaultMulticastData := getMulticastExpectedData(netInfo, clusterPortGroup, clusterRtrPortGroup)
				namespaceMulticastData := getMulticastPolicyExpectedData(netInfo, namespaceName1, nil)
				namespace1 := *newNamespace(namespaceName1)

				fakeOvn.startWithDBSetup(libovsdb.TestSetup{NBData: append(defaultMulticastData, namespaceMulticastData...)},
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespace1,
						},
					},
				)
				bnc, _ := startBaseNetworkController(fakeOvn, nad)

				Expect(bnc.WatchNamespaces()).To(Succeed())
				// only namespaced acls should be dereferenced, default acls will stay
				namespacePortGroup := getNamespacePG(namespaceName1, getNetworkControllerName(netInfo.GetNetworkName()))
				expectedData := append(defaultMulticastData, namespacePortGroup)
				Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData))
				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		},
			Entry("IPv4", true, false, nil),
			Entry("IPv6", false, true, nil),
			Entry("[Network Segmentation] IPv4", true, false, nadFromIPMode(true, false)),
			Entry("[Network Segmentation] IPv6", false, true, nadFromIPMode(false, true)),
		)
	})

	Context("during execution", func() {
		DescribeTable("tests enabling/disabling multicast in a namespace", func(useIPv4, useIPv6 bool, nad *nadapi.NetworkAttachmentDefinition) {
			app.Action = func(ctx *cli.Context) error {
				config.IPv4Mode = useIPv4
				config.IPv6Mode = useIPv6

				netInfo := getNetInfoFromNAD(nad)
				namespace1 := *newNamespace(namespaceName1)
				fakeOvn.startWithDBSetup(libovsdb.TestSetup{},
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespace1,
						},
					},
				)

				bnc, _ := startBaseNetworkController(fakeOvn, nad)
				Expect(bnc.WatchNamespaces()).To(Succeed())

				ns, err := fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Get(context.TODO(), namespace1.Name, metav1.GetOptions{})
				Expect(err).To(Succeed())
				Expect(ns).NotTo(BeNil())

				// Multicast is denied by default.
				_, ok := ns.Annotations[util.NsMulticastAnnotation]
				Expect(ok).To(BeFalse())

				// Enable multicast in the namespace.
				updateMulticast(fakeOvn, ns, true)
				expectedData := getMulticastPolicyExpectedData(netInfo, namespace1.Name, nil)
				Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

				// Disable multicast in the namespace.
				updateMulticast(fakeOvn, ns, false)

				namespacePortGroup := getNamespacePG(namespaceName1, bnc.controllerName)
				Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(namespacePortGroup))

				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		},
			Entry("IPv4", true, false, nil),
			Entry("IPv6", false, true, nil),
			Entry("[Network Segmentation] IPv4", true, false, nadFromIPMode(true, false)),
			Entry("[Network Segmentation] IPv6", false, true, nadFromIPMode(false, true)),
		)

		DescribeTable("tests enabling multicast in a namespace with a pod", func(useIPv4, useIPv6 bool, nad *nadapi.NetworkAttachmentDefinition) {
			app.Action = func(ctx *cli.Context) error {
				config.IPv4Mode = useIPv4
				config.IPv6Mode = useIPv6

				netInfo := getNetInfoFromNAD(nad)
				node := newNodeWithNad(nad, networkName, networkID)
				namespace1 := *newNamespace(namespaceName1)
				pods, tPods, tPodIPs := createTestPods(nodeName, namespaceName1, useIPv4, useIPv6)

				objs := []runtime.Object{
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespace1,
						},
					},
					&v1.NodeList{
						Items: []v1.Node{
							*node,
						},
					},
					&v1.PodList{
						Items: pods,
					},
				}
				if nad != nil {
					objs = append(objs, &nadapi.NetworkAttachmentDefinitionList{
						Items: []nadapi.NetworkAttachmentDefinition{*nad},
					})
				}

				fakeOvn.startWithDBSetup(libovsdb.TestSetup{NBData: getNodeData(netInfo, nodeName)}, objs...)
				bnc, asf := startBaseNetworkController(fakeOvn, nad)

				for _, tPod := range tPods {
					tPod.populateControllerLogicalSwitchCache(bnc)
				}
				if nad != nil {
					Expect(fakeOvn.networkManager.Start()).To(Succeed())
					defer fakeOvn.networkManager.Stop()
				}

				Expect(bnc.WatchNamespaces()).To(Succeed())
				Expect(bnc.WatchPods()).To(Succeed())
				ns, err := fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Get(context.TODO(), namespace1.Name, metav1.GetOptions{})
				Expect(err).To(Succeed())
				Expect(ns).NotTo(BeNil())

				// Enable multicast in the namespace
				updateMulticast(fakeOvn, ns, true)
				// calculate expected data
				ports := []string{}
				for _, tPod := range tPods {
					ports = append(ports, tPod.portUUID)
				}
				expectedData := getMulticastPolicyExpectedData(netInfo, namespace1.Name, ports)
				expectedData = append(expectedData, getExpectedPodsAndSwitches(bnc.GetNetInfo(), tPods, []string{nodeName})...)
				Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))
				asf.ExpectAddressSetWithAddresses(namespace1.Name, tPodIPs)
				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		},
			Entry("IPv4", true, false, nil),
			Entry("IPv6", false, true, nil),
			Entry("[Network Segmentation] IPv4", true, false, nadFromIPMode(true, false)),
			Entry("[Network Segmentation] IPv6", false, true, nadFromIPMode(false, true)),
		)

		DescribeTable("tests enabling multicast in multiple namespaces with a long name > 42 characters", func(useIPv4, useIPv6 bool, nad *nadapi.NetworkAttachmentDefinition) {
			app.Action = func(ctx *cli.Context) error {
				config.IPv4Mode = useIPv4
				config.IPv6Mode = useIPv6

				netInfo := getNetInfoFromNAD(nad)
				longNameSpace1Name := "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijk" // create with 63 characters
				namespace1 := *newNamespace(longNameSpace1Name)
				longNameSpace2Name := "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijl" // create with 63 characters
				namespace2 := *newNamespace(longNameSpace2Name)
				node := newNodeWithNad(nad, networkName, networkID)

				objs := []runtime.Object{
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespace1,
							namespace2,
						},
					},
					&v1.NodeList{
						Items: []v1.Node{
							*node,
						},
					},
				}
				if nad != nil {
					objs = append(objs, &nadapi.NetworkAttachmentDefinitionList{
						Items: []nadapi.NetworkAttachmentDefinition{*nad},
					})
				}

				fakeOvn.startWithDBSetup(libovsdb.TestSetup{NBData: getNodeData(netInfo, nodeName)}, objs...)
				bnc, _ := startBaseNetworkController(fakeOvn, nad)

				if nad != nil {
					Expect(fakeOvn.networkManager.Start()).To(Succeed())
					defer fakeOvn.networkManager.Stop()
				}

				Expect(bnc.WatchNamespaces()).To(Succeed())
				Expect(bnc.WatchPods()).To(Succeed())
				ns1, err := fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Get(context.TODO(), namespace1.Name, metav1.GetOptions{})
				Expect(err).To(Succeed())
				Expect(ns1).NotTo(BeNil())
				ns2, err := fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Get(context.TODO(), namespace2.Name, metav1.GetOptions{})
				Expect(err).To(Succeed())
				Expect(ns2).NotTo(BeNil())

				portsns1 := []string{}
				expectedData := getMulticastPolicyExpectedData(netInfo, longNameSpace1Name, portsns1)
				acl := expectedData[0].(*nbdb.ACL)
				// Post ACL indexing work, multicast ACL's don't have names
				// We use externalIDs instead; so we can check if the expected IDs exist for the long namespace so that
				// isEquivalent logic will be correct
				Expect(acl.Name).To(BeNil())
				Expect(acl.ExternalIDs[libovsdbops.ObjectNameKey.String()]).To(Equal(longNameSpace1Name))
				expectedData = append(expectedData, getMulticastPolicyExpectedData(netInfo, longNameSpace2Name, nil)...)
				acl = expectedData[3].(*nbdb.ACL)
				Expect(acl.Name).To(BeNil())
				Expect(acl.ExternalIDs[libovsdbops.ObjectNameKey.String()]).To(Equal(longNameSpace2Name))
				expectedData = append(expectedData, getExpectedPodsAndSwitches(bnc.GetNetInfo(), []testPod{}, []string{node.Name})...)
				// Enable multicast in the namespace.
				updateMulticast(fakeOvn, ns1, true)
				updateMulticast(fakeOvn, ns2, true)
				Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		},
			Entry("IPv4", true, false, nil),
			Entry("IPv6", false, true, nil),
			Entry("[Network Segmentation] IPv4", true, false, nadFromIPMode(true, false)),
			Entry("[Network Segmentation] IPv6", false, true, nadFromIPMode(false, true)),
		)

		DescribeTable("tests adding a pod to a multicast enabled namespace", func(useIPv4, useIPv6 bool, nad *nadapi.NetworkAttachmentDefinition) {
			app.Action = func(ctx *cli.Context) error {
				config.IPv4Mode = useIPv4
				config.IPv6Mode = useIPv6

				netInfo := getNetInfoFromNAD(nad)
				namespace1 := *newNamespace(namespaceName1)
				node := newNodeWithNad(nad, networkName, networkID)
				_, tPods, tPodIPs := createTestPods(nodeName, namespaceName1, useIPv4, useIPv6)

				ports := []string{}
				for _, pod := range tPods {
					ports = append(ports, pod.portUUID)
				}

				objs := []runtime.Object{
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespace1,
						},
					},
					&v1.NodeList{
						Items: []v1.Node{
							*node,
						},
					},
				}
				if nad != nil {
					objs = append(objs, &nadapi.NetworkAttachmentDefinitionList{
						Items: []nadapi.NetworkAttachmentDefinition{*nad},
					})
				}

				fakeOvn.startWithDBSetup(libovsdb.TestSetup{NBData: getNodeData(netInfo, nodeName)}, objs...)
				bnc, asf := startBaseNetworkController(fakeOvn, nad)

				for _, tPod := range tPods {
					tPod.populateControllerLogicalSwitchCache(bnc)
				}
				if nad != nil {
					Expect(fakeOvn.networkManager.Start()).To(Succeed())
					defer fakeOvn.networkManager.Stop()
				}

				Expect(bnc.WatchNamespaces()).To(Succeed())
				Expect(bnc.WatchPods()).To(Succeed())
				ns, err := fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Get(context.TODO(), namespace1.Name, metav1.GetOptions{})
				Expect(err).To(Succeed())
				Expect(ns).NotTo(BeNil())

				// Enable multicast in the namespace.
				updateMulticast(fakeOvn, ns, true)
				// Check expected data without pods
				expectedDataWithoutPods := getMulticastPolicyExpectedData(netInfo, namespace1.Name, nil)
				expectedDataWithoutPods = append(expectedDataWithoutPods, getNodeData(netInfo, nodeName)...)
				Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedDataWithoutPods))

				// Create pods
				for _, tPod := range tPods {
					tPod.populateControllerLogicalSwitchCache(bnc)
					_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(tPod.namespace).Create(context.TODO(), newPod(
						tPod.namespace, tPod.podName, tPod.nodeName, tPod.podIP), metav1.CreateOptions{})
					Expect(err).NotTo(HaveOccurred())
				}

				// Check pods were added
				asf.EventuallyExpectAddressSetWithAddresses(namespace1.Name, tPodIPs)
				expectedDataWithPods := getMulticastPolicyExpectedData(netInfo, namespace1.Name, ports)
				expectedDataWithPods = append(expectedDataWithPods, getExpectedPodsAndSwitches(bnc, tPods, []string{nodeName})...)
				Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedDataWithPods...))

				// Delete the pod from the namespace.
				for _, tPod := range tPods {
					err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(tPod.namespace).Delete(context.TODO(),
						tPod.podName, *metav1.NewDeleteOptions(0))
					Expect(err).NotTo(HaveOccurred())
				}
				asf.EventuallyExpectEmptyAddressSetExist(namespace1.Name)
				Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedDataWithoutPods))

				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		},
			Entry("IPv4", true, false, nil),
			Entry("IPv6", false, true, nil),
			Entry("[Network Segmentation] IPv4", true, false, nadFromIPMode(true, false)),
			Entry("[Network Segmentation] IPv6", false, true, nadFromIPMode(false, true)),
		)
	})
})
