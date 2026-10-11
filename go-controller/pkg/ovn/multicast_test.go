// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"context"
	"fmt"

	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	"github.com/onsi/gomega/format"
	"github.com/urfave/cli/v2"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/addresssetmanager"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
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

func getMulticastPolicyExpectedData(netInfo util.NetInfo, ns string, ports []string) []libovsdb.TestData {
	return getMulticastPolicyExpectedDataWithPodIPs(netInfo, ns, ports, nil)
}

func getMulticastPolicyExpectedDataWithPodIPs(netInfo util.NetInfo, ns string, ports, podIPs []string) []libovsdb.TestData {
	netControllerName := getNetworkControllerName(netInfo.GetNetworkName())
	fakeController := getFakeController(netControllerName)
	pg_hash := fakeController.getNamespacePortGroupName(ns)
	egressMatch := libovsdbutil.GetACLMatch(pg_hash, fakeController.getMulticastACLEgrMatch(), libovsdbutil.ACLEgress)

	peerIndex := addresssetmanager.GetPodSelectorAddrSetDbIDs(&metav1.LabelSelector{}, nil, nil,
		ns, netControllerName, true)
	nsASv4, nsASv6 := addressset.GetTestDbAddrSets(peerIndex, podIPs)
	ip4AddressSet, ip6AddressSet := addressset.GetHashNamesForAS(peerIndex)
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

	data := []libovsdb.TestData{}
	if config.IPv4Mode {
		data = append(data, nsASv4)
	}
	if config.IPv6Mode {
		data = append(data, nsASv6)
	}

	return append(data,
		egressACL,
		ingressACL,
		pg,
	)
}

func getNamespacePG(ns, controllerName string) *nbdb.PortGroup {
	pgDbIDs := getNamespacePortGroupDbIDs(ns, controllerName)
	pg := libovsdbutil.BuildPortGroup(pgDbIDs, nil, nil)
	pg.UUID = pg.Name + "-UUID"
	return pg
}

func getMulticastPolicyStaleData(netInfo util.NetInfo, ns string) []libovsdb.TestData {
	testData := getMulticastPolicyExpectedData(netInfo, ns, nil)
	// remove address sets
	result := testData[len(testData)-3:]
	// get ingress ACL and spoil the match (just to make sure it will be updated)
	result[1].(*nbdb.ACL).Match = "stale-match"
	return result
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

func newNodeWithNad(nad *nadapi.NetworkAttachmentDefinition, networkName, networkID string) *corev1.Node {
	n := newNode(nodeName, "192.168.126.202/24")
	if nad != nil {
		n.Annotations["k8s.ovn.org/node-subnets"] = fmt.Sprintf("{\"default\":\"192.168.126.202/24\", \"%s\":\"192.168.127.202/24\"}", networkName)
		n.Annotations["k8s.ovn.org/network-ids"] = fmt.Sprintf("{\"default\":\"0\",\"%s\":\"%s\"}", networkName, networkID)
		n.Annotations["k8s.ovn.org/node-mgmt-port-mac-addresses"] = fmt.Sprintf("{\"default\":\"96:8f:e8:25:a2:e5\",\"%s\":\"d6:bc:85:32:30:fb\"}", networkName)
		n.Annotations["k8s.ovn.org/node-chassis-id"] = chassisIDForNode(n.Name)
		n.Annotations["k8s.ovn.org/l3-gateway-config"] = "{\"default\":{\"mac-address\":\"52:54:00:e2:ed:d0\",\"ip-addresses\":[\"10.1.1.10/24\"],\"ip-address\":\"10.1.1.10/24\",\"next-hops\":[\"10.1.1.1\"],\"next-hop\":\"10.1.1.1\"}}"
		n.Annotations[util.OvnNodeID] = "4"
	}
	return n
}

func createTestPods(nodeName, namespace string, useIPv4, useIPv6 bool) (pods []corev1.Pod, tPods []testPod, tPodIPs []string) {
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
		pods = append(pods, *ovntest.NewPod(tPod.namespace, tPod.podName, tPod.nodeName, tPod.podIP))
	}
	return
}

func updateMulticast(fakeOvn *FakeOVN, ns *corev1.Namespace, enable bool) {
	if enable {
		ns.Annotations[util.NsMulticastAnnotation] = "true"
	} else {
		ns.Annotations[util.NsMulticastAnnotation] = "false"
	}
	_, err := fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Update(context.TODO(), ns, metav1.UpdateOptions{})
	Expect(err).NotTo(HaveOccurred())
}

func startBaseNetworkController(fakeOvn *FakeOVN, nad *nadapi.NetworkAttachmentDefinition) *BaseNetworkController {
	if nad != nil {
		netInfo, err := util.ParseNADInfo(nad)
		Expect(err).ToNot(HaveOccurred())
		Expect(fakeOvn.NewUserDefinedNetworkController(nad)).To(Succeed())
		controller, ok := fakeOvn.userDefinedNetworkControllers[netInfo.GetNetworkName()]
		Expect(ok).To(BeTrue())
		return &controller.bnc.BaseNetworkController
	} else {
		return &fakeOvn.controller.BaseNetworkController
	}
}

var _ = Describe("OVN Multicast with IP Address Family", func() {
	const (
		namespaceName1         = "namespace1"
		longnamespaceName1Name = "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijk" // create with 63 characters

	)

	var (
		app                   *cli.App
		fakeOvn               *FakeOVN
		gomegaFormatMaxLength int
		networkName           = "bluenet"
		nadName               = "rednad"
		networkID             = "50"

		nadFromIPMode = func(namespace string, useIPv4, useIPv6 bool) *nadapi.NetworkAttachmentDefinition {
			var nad *nadapi.NetworkAttachmentDefinition
			if useIPv4 && useIPv6 {
				nad = ovntest.GenerateNAD(networkName, nadName, namespace,
					types.Layer3Topology, "100.128.0.0/16,ae70::66/60", types.NetworkRolePrimary)
			} else if useIPv4 {
				nad = ovntest.GenerateNAD(networkName, nadName, namespace,
					types.Layer3Topology, "100.128.0.0/16", types.NetworkRolePrimary)
			} else {
				nad = ovntest.GenerateNAD(networkName, nadName, namespace,
					types.Layer3Topology, "ae70::66/60", types.NetworkRolePrimary)
			}
			nad.Annotations = map[string]string{types.OvnNetworkIDAnnotation: networkID}
			return nad
		}
	)

	BeforeEach(func() {
		// Restore global default values before each testcase
		Expect(config.PrepareTestConfig()).To(Succeed())
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

		fakeOvn = NewFakeOVN(false, nodeName)
		gomegaFormatMaxLength = format.MaxLength
		format.MaxLength = 0
	})

	AfterEach(func() {
		fakeOvn.shutdown()
		format.MaxLength = gomegaFormatMaxLength
	})

	Context("on startup", func() {
		DescribeTable("creates default Multicast ACLs", func(useIPv4, useIPv6 bool, nad *nadapi.NetworkAttachmentDefinition) {
			app.Action = func(*cli.Context) error {
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
				bnc := startBaseNetworkController(fakeOvn, nad)

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
			Entry("[Network Segmentation] IPv4", true, false, nadFromIPMode(namespaceName1, true, false)),
			Entry("[Network Segmentation] IPv6", false, true, nadFromIPMode(namespaceName1, false, true)),
		)

		DescribeTable("cleans up Multicast resources when multicast is disabled", func(useIPv4, useIPv6 bool, nad *nadapi.NetworkAttachmentDefinition) {
			app.Action = func(*cli.Context) error {
				config.IPv4Mode = useIPv4
				config.IPv6Mode = useIPv6

				netInfo := getNetInfoFromNAD(nad)
				clusterPortGroup := newNetworkClusterPortGroup(netInfo)
				clusterRtrPortGroup := newNetworkRouterPortGroup(netInfo)
				initialData := getMulticastExpectedData(netInfo, clusterPortGroup, clusterRtrPortGroup)

				nsData := getMulticastPolicyExpectedData(netInfo, namespaceName1, nil)
				// preserve address set because it will only be cleaned up on the next restart by the addresssetManager as unreferenced
				// we never use dualstack mode in tests, so it will always be 1 address set
				addrSet := nsData[0]
				initialData = append(initialData, nsData...)
				// namespace is still present, but multicast support is disabled
				namespace1 := *ovntest.NewNamespace(namespaceName1)
				fakeOvn.startWithDBSetup(libovsdb.TestSetup{NBData: initialData},
					&corev1.NamespaceList{
						Items: []corev1.Namespace{
							namespace1,
						},
					},
				)
				bnc := startBaseNetworkController(fakeOvn, nad)

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
					addrSet,
				}
				Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData))
				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		},
			Entry("IPv4", true, false, nil),
			Entry("IPv6", false, true, nil),
			Entry("[Network Segmentation] IPv4", true, false, nadFromIPMode(namespaceName1, true, false)),
			Entry("[Network Segmentation] IPv6", false, true, nadFromIPMode(namespaceName1, false, true)),
		)

		DescribeTable("creates namespace Multicast ACLs", func(useIPv4, useIPv6 bool, nad *nadapi.NetworkAttachmentDefinition) {
			app.Action = func(*cli.Context) error {
				config.IPv4Mode = useIPv4
				config.IPv6Mode = useIPv6

				netInfo := getNetInfoFromNAD(nad)
				clusterPortGroup := newNetworkClusterPortGroup(netInfo)
				clusterRtrPortGroup := newNetworkRouterPortGroup(netInfo)
				expectedData := getMulticastExpectedData(netInfo, clusterPortGroup, clusterRtrPortGroup)
				// namespace exists, but multicast acls do not
				namespace1 := *ovntest.NewNamespace(namespaceName1)
				namespace1.Annotations[util.NsMulticastAnnotation] = "true"

				objs := []runtime.Object{&corev1.NamespaceList{
					Items: []corev1.Namespace{
						namespace1,
					},
				}}
				if nad != nil {
					objs = append(objs, &nadapi.NetworkAttachmentDefinitionList{
						Items: []nadapi.NetworkAttachmentDefinition{*nad},
					})
				}
				fakeOvn.startWithDBSetup(libovsdb.TestSetup{NBData: expectedData}, objs...)
				if nad != nil {
					Expect(fakeOvn.networkManager.Start()).To(Succeed())
					defer fakeOvn.networkManager.Stop()
				}
				bnc := startBaseNetworkController(fakeOvn, nad)

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
			Entry("[Network Segmentation] IPv4", true, false, nadFromIPMode(namespaceName1, true, false)),
			Entry("[Network Segmentation] IPv6", false, true, nadFromIPMode(namespaceName1, false, true)),
		)

		DescribeTable("updates stale namespace Multicast ACLs", func(useIPv4, useIPv6 bool, nad *nadapi.NetworkAttachmentDefinition) {
			app.Action = func(*cli.Context) error {
				config.IPv4Mode = useIPv4
				config.IPv6Mode = useIPv6

				// start with stale ACLs for existing namespace
				netInfo := getNetInfoFromNAD(nad)
				clusterPortGroup := newNetworkClusterPortGroup(netInfo)
				clusterRtrPortGroup := newNetworkRouterPortGroup(netInfo)
				expectedData := getMulticastExpectedData(netInfo, clusterPortGroup, clusterRtrPortGroup)
				expectedData = append(expectedData, getMulticastPolicyStaleData(netInfo, namespaceName1)...)
				namespace1 := *ovntest.NewNamespace(namespaceName1)
				namespace1.Annotations[util.NsMulticastAnnotation] = "true"

				objs := []runtime.Object{&corev1.NamespaceList{
					Items: []corev1.Namespace{
						namespace1,
					},
				}}
				if nad != nil {
					objs = append(objs, &nadapi.NetworkAttachmentDefinitionList{
						Items: []nadapi.NetworkAttachmentDefinition{*nad},
					})
				}
				fakeOvn.startWithDBSetup(libovsdb.TestSetup{NBData: expectedData}, objs...)
				if nad != nil {
					Expect(fakeOvn.networkManager.Start()).To(Succeed())
					defer fakeOvn.networkManager.Stop()
				}
				bnc := startBaseNetworkController(fakeOvn, nad)

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
			Entry("[Network Segmentation] IPv4", true, false, nadFromIPMode(namespaceName1, true, false)),
			Entry("[Network Segmentation] IPv6", false, true, nadFromIPMode(namespaceName1, false, true)),
		)

		DescribeTable("cleans up namespace Multicast ACLs when multicast is disabled for namespace", func(useIPv4, useIPv6 bool, nad *nadapi.NetworkAttachmentDefinition) {
			app.Action = func(*cli.Context) error {
				config.IPv4Mode = useIPv4
				config.IPv6Mode = useIPv6

				// start with stale ACLs
				netInfo := getNetInfoFromNAD(nad)
				clusterPortGroup := newNetworkClusterPortGroup(netInfo)
				clusterRtrPortGroup := newNetworkRouterPortGroup(netInfo)
				defaultMulticastData := getMulticastExpectedData(netInfo, clusterPortGroup, clusterRtrPortGroup)
				namespaceMulticastData := getMulticastPolicyExpectedData(netInfo, namespaceName1, nil)
				// preserve address set because it will only be cleaned up on the next restart by the addresssetManager as unreferenced
				// we never use dualstack mode in tests, so it will always be 1 address set
				addrSet := namespaceMulticastData[0]
				namespace1 := *ovntest.NewNamespace(namespaceName1)

				objs := []runtime.Object{&corev1.NamespaceList{
					Items: []corev1.Namespace{
						namespace1,
					},
				}}
				if nad != nil {
					objs = append(objs, &nadapi.NetworkAttachmentDefinitionList{
						Items: []nadapi.NetworkAttachmentDefinition{*nad},
					})
				}
				fakeOvn.startWithDBSetup(libovsdb.TestSetup{NBData: append(defaultMulticastData, namespaceMulticastData...)}, objs...)
				if nad != nil {
					Expect(fakeOvn.networkManager.Start()).To(Succeed())
					defer fakeOvn.networkManager.Stop()
				}
				bnc := startBaseNetworkController(fakeOvn, nad)

				Expect(bnc.WatchNamespaces()).To(Succeed())
				// only namespaced acls should be dereferenced, default acls will stay
				namespacePortGroup := getNamespacePG(namespaceName1, getNetworkControllerName(netInfo.GetNetworkName()))
				expectedData := append(defaultMulticastData, namespacePortGroup, addrSet)
				Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData))
				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		},
			Entry("IPv4", true, false, nil),
			Entry("IPv6", false, true, nil),
			Entry("[Network Segmentation] IPv4", true, false, nadFromIPMode(namespaceName1, true, false)),
			Entry("[Network Segmentation] IPv6", false, true, nadFromIPMode(namespaceName1, false, true)),
		)
	})

	Context("during execution", func() {
		DescribeTable("tests enabling/disabling multicast in a namespace", func(useIPv4, useIPv6 bool, nad *nadapi.NetworkAttachmentDefinition) {
			app.Action = func(*cli.Context) error {
				config.IPv4Mode = useIPv4
				config.IPv6Mode = useIPv6

				netInfo := getNetInfoFromNAD(nad)
				namespace1 := *ovntest.NewNamespace(namespaceName1)

				objs := []runtime.Object{&corev1.NamespaceList{
					Items: []corev1.Namespace{
						namespace1,
					},
				}}
				if nad != nil {
					objs = append(objs, &nadapi.NetworkAttachmentDefinitionList{
						Items: []nadapi.NetworkAttachmentDefinition{*nad},
					})
				}

				fakeOvn.startWithDBSetup(libovsdb.TestSetup{}, objs...)

				if nad != nil {
					Expect(fakeOvn.networkManager.Start()).To(Succeed())
					defer fakeOvn.networkManager.Stop()
				}

				bnc := startBaseNetworkController(fakeOvn, nad)
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
			Entry("[Network Segmentation] IPv4", true, false, nadFromIPMode(namespaceName1, true, false)),
			Entry("[Network Segmentation] IPv6", false, true, nadFromIPMode(namespaceName1, false, true)),
		)

		DescribeTable("tests enabling multicast in a namespace with a pod", func(useIPv4, useIPv6 bool, nad *nadapi.NetworkAttachmentDefinition) {
			app.Action = func(*cli.Context) error {
				config.IPv4Mode = useIPv4
				config.IPv6Mode = useIPv6

				netInfo := getNetInfoFromNAD(nad)
				node := newNodeWithNad(nad, networkName, networkID)
				namespace1 := *ovntest.NewNamespace(namespaceName1)
				if nad != nil {
					namespace1 = *newUDNNamespace(namespaceName1)
				}
				pods, tPods, tPodIPs := createTestPods(nodeName, namespaceName1, useIPv4, useIPv6)

				objs := []runtime.Object{
					&corev1.NamespaceList{
						Items: []corev1.Namespace{
							namespace1,
						},
					},
					&corev1.NodeList{
						Items: []corev1.Node{
							*node,
						},
					},
					&corev1.PodList{
						Items: pods,
					},
				}
				if nad != nil {
					objs = append(objs, &nadapi.NetworkAttachmentDefinitionList{
						Items: []nadapi.NetworkAttachmentDefinition{*nad},
					})
				}

				fakeOvn.startWithDBSetup(libovsdb.TestSetup{NBData: getNodeData(netInfo, nodeName)}, objs...)
				bnc := startBaseNetworkController(fakeOvn, nad)

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
				expectedData := getMulticastPolicyExpectedDataWithPodIPs(netInfo, namespace1.Name, ports, tPodIPs)
				nadKey := ""
				if nad != nil {
					nadKey = util.GetNADName(nad.Namespace, nad.Name)
				}
				expectedData = append(expectedData, getExpectedPodsAndSwitches(bnc.GetNetInfo(), tPods, []string{nodeName}, nadKey)...)
				Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))
				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		},
			Entry("IPv4", true, false, nil),
			Entry("IPv6", false, true, nil),
			Entry("[Network Segmentation] IPv4", true, false, nadFromIPMode(namespaceName1, true, false)),
			Entry("[Network Segmentation] IPv6", false, true, nadFromIPMode(namespaceName1, false, true)),
		)

		DescribeTable("tests enabling multicast in multiple namespaces with a long name > 42 characters", func(useIPv4, useIPv6 bool, nad *nadapi.NetworkAttachmentDefinition) {
			app.Action = func(*cli.Context) error {
				config.IPv4Mode = useIPv4
				config.IPv6Mode = useIPv6

				netInfo := getNetInfoFromNAD(nad)
				namespace1 := *ovntest.NewNamespace(longnamespaceName1Name)
				longNameSpace2Name := "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijl" // create with 63 characters
				namespace2 := *ovntest.NewNamespace(longNameSpace2Name)
				node := newNodeWithNad(nad, networkName, networkID)

				objs := []runtime.Object{
					&corev1.NamespaceList{
						Items: []corev1.Namespace{
							namespace1,
							namespace2,
						},
					},
					&corev1.NodeList{
						Items: []corev1.Node{
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
				bnc := startBaseNetworkController(fakeOvn, nad)

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

				expectedData := getMulticastPolicyExpectedData(netInfo, longnamespaceName1Name, nil)
				acl := expectedData[1].(*nbdb.ACL)
				// Post ACL indexing work, multicast ACL's don't have names
				// We use externalIDs instead; so we can check if the expected IDs exist for the long namespace so that
				// isEquivalent logic will be correct
				Expect(acl.Name).To(BeNil())
				Expect(acl.ExternalIDs[libovsdbops.ObjectNameKey.String()]).To(Equal(longnamespaceName1Name))

				// the NAD is configured in longnamespaceName1Name so there shouldn't be any entries for
				// longNameSpace2Name
				if nad == nil {
					expectedData = append(expectedData, getMulticastPolicyExpectedData(netInfo, longNameSpace2Name, nil)...)
					acl = expectedData[5].(*nbdb.ACL)
					Expect(acl.Name).To(BeNil())
					Expect(acl.ExternalIDs[libovsdbops.ObjectNameKey.String()]).To(Equal(longNameSpace2Name))
				}
				nadKey := ""
				if nad != nil {
					nadKey = util.GetNADName(nad.Namespace, nad.Name)
				}
				expectedData = append(expectedData, getExpectedPodsAndSwitches(bnc.GetNetInfo(), []testPod{}, []string{node.Name}, nadKey)...)
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
			Entry("[Network Segmentation] IPv4", true, false, nadFromIPMode(longnamespaceName1Name, true, false)),
			Entry("[Network Segmentation] IPv6", false, true, nadFromIPMode(longnamespaceName1Name, false, true)),
		)

		DescribeTable("tests adding a pod to a multicast enabled namespace", func(useIPv4, useIPv6 bool, nad *nadapi.NetworkAttachmentDefinition) {
			app.Action = func(*cli.Context) error {
				config.IPv4Mode = useIPv4
				config.IPv6Mode = useIPv6

				netInfo := getNetInfoFromNAD(nad)
				namespace1 := *ovntest.NewNamespace(namespaceName1)
				if nad != nil {
					namespace1 = *newUDNNamespace(namespaceName1)
				}
				node := newNodeWithNad(nad, networkName, networkID)
				_, tPods, tPodIPs := createTestPods(nodeName, namespaceName1, useIPv4, useIPv6)

				ports := []string{}
				for _, pod := range tPods {
					ports = append(ports, pod.portUUID)
				}

				objs := []runtime.Object{
					&corev1.NamespaceList{
						Items: []corev1.Namespace{
							namespace1,
						},
					},
					&corev1.NodeList{
						Items: []corev1.Node{
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
				bnc := startBaseNetworkController(fakeOvn, nad)

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
					_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(tPod.namespace).Create(context.TODO(), ovntest.NewPod(
						tPod.namespace, tPod.podName, tPod.nodeName, tPod.podIP), metav1.CreateOptions{})
					Expect(err).NotTo(HaveOccurred())
				}

				// Check pods were added
				expectedDataWithPods := getMulticastPolicyExpectedDataWithPodIPs(netInfo, namespace1.Name, ports, tPodIPs)
				nadKey := ""
				if nad != nil {
					nadKey = util.GetNADName(nad.Namespace, nad.Name)
				}
				expectedDataWithPods = append(expectedDataWithPods, getExpectedPodsAndSwitches(bnc.GetNetInfo(), tPods, []string{nodeName}, nadKey)...)
				Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedDataWithPods...))

				// Delete the pod from the namespace.
				for _, tPod := range tPods {
					err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(tPod.namespace).Delete(context.TODO(),
						tPod.podName, *metav1.NewDeleteOptions(0))
					Expect(err).NotTo(HaveOccurred())
				}
				Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedDataWithoutPods))

				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		},
			Entry("IPv4", true, false, nil),
			Entry("IPv6", false, true, nil),
			Entry("[Network Segmentation] IPv4", true, false, nadFromIPMode(namespaceName1, true, false)),
			Entry("[Network Segmentation] IPv6", false, true, nadFromIPMode(namespaceName1, false, true)),
		)
	})

	Context("IGMP/MLD querier gating", func() {
		newMcastACLForController := func(ns, controller string) *nbdb.ACL {
			aclIDs := libovsdbops.NewDbObjectIDs(libovsdbops.ACLMulticastNamespace, controller, map[libovsdbops.ExternalIDKey]string{
				libovsdbops.ObjectNameKey:      ns,
				libovsdbops.PolicyDirectionKey: string(libovsdbutil.ACLEgress),
			})
			return libovsdbutil.BuildACLWithDefaultTier(aclIDs, types.DefaultMcastAllowPriority, "ip4.mcast", nbdb.ACLActionAllow, nil, libovsdbutil.LportEgress)
		}

		newMcastACL := func(ns string) *nbdb.ACL {
			return newMcastACLForController(ns, types.DefaultNetworkControllerName)
		}

		// createMcastACLs inserts ACLs and attaches them to a port group so they
		// persist in OVSDB (ACL is a non-root table; unreferenced rows are garbage-collected).
		createMcastACLs := func(client libovsdbclient.Client, pgName string, acls ...*nbdb.ACL) {
			ops, err := libovsdbops.CreateOrUpdateACLsOps(client, nil, nil, acls...)
			ExpectWithOffset(1, err).NotTo(HaveOccurred(), "failed to build ACL create ops")
			ops, err = libovsdbops.AddACLsToPortGroupOps(client, ops, pgName, acls...)
			ExpectWithOffset(1, err).NotTo(HaveOccurred(), "failed to build port group attachment ops")
			_, err = libovsdbops.TransactAndCheckAndSetUUIDs(client, acls, ops)
			ExpectWithOffset(1, err).NotTo(HaveOccurred(), "failed to transact ACL creation")
		}

		findSwitch := func(client libovsdbclient.Client) *nbdb.LogicalSwitch {
			switches, err := libovsdbops.FindLogicalSwitchesWithPredicate(client, func(item *nbdb.LogicalSwitch) bool {
				return item.Name == nodeName
			})
			ExpectWithOffset(1, err).NotTo(HaveOccurred(), "failed to find logical switches")
			ExpectWithOffset(1, switches).To(HaveLen(1), "expected one logical switch for node %s", nodeName)
			return switches[0]
		}

		testNode := func(useIPv4, useIPv6 bool) *corev1.Node {
			n := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        nodeName,
					Annotations: map[string]string{util.OvnNodeChassisID: chassisIDForNode(nodeName)},
					Labels:      map[string]string{"k8s.ovn.org/egress-assignable": ""},
				},
				Status: corev1.NodeStatus{
					Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
				},
			}
			switch {
			case useIPv4 && useIPv6:
				n.Annotations["k8s.ovn.org/node-primary-ifaddr"] = `{"ipv4":"192.168.126.202/24","ipv6":"ae70::66/64"}`
				n.Annotations["k8s.ovn.org/node-subnets"] = fmt.Sprintf(`{"default":["%s","%s"]}`, v4Node1Subnet, v6Node1Subnet)
				n.Annotations[util.OVNNodeHostCIDRs] = `["192.168.126.202/24","ae70::66/64"]`
			case useIPv6:
				n.Annotations["k8s.ovn.org/node-primary-ifaddr"] = `{"ipv4":"","ipv6":"ae70::66/64"}`
				n.Annotations["k8s.ovn.org/node-subnets"] = fmt.Sprintf(`{"default":"%s"}`, v6Node1Subnet)
				n.Annotations[util.OVNNodeHostCIDRs] = `["ae70::66/64"]`
			default:
				n.Annotations["k8s.ovn.org/node-primary-ifaddr"] = `{"ipv4":"192.168.126.202/24","ipv6":""}`
				n.Annotations["k8s.ovn.org/node-subnets"] = fmt.Sprintf(`{"default":"%s"}`, v4Node1Subnet)
				n.Annotations[util.OVNNodeHostCIDRs] = `["192.168.126.202/24"]`
			}
			return n
		}

		DescribeTable("sets mcast_querier during node switch creation",
			func(useIPv4, useIPv6 bool, hasACL bool) {
				app.Action = func(*cli.Context) error {
					config.IPv4Mode = useIPv4
					config.IPv6Mode = useIPv6

					node := testNode(useIPv4, useIPv6)
					hostSubnets, err := util.ParseNodeHostSubnetAnnotation(node, types.DefaultNetworkName)
					Expect(err).NotTo(HaveOccurred(), "failed to parse host subnet annotation")

					nbData := []libovsdb.TestData{
						&nbdb.LogicalSwitch{Name: nodeName},
						newNetworkRouterPortGroup(getNetInfoFromNAD(nil)),
					}

					fakeOvn.startWithDBSetup(libovsdb.TestSetup{NBData: nbData},
						&corev1.NodeList{Items: []corev1.Node{*node}})
					bnc := startBaseNetworkController(fakeOvn, nil)

					if hasACL {
						mcastACL := newMcastACL("namespace1")
						pgName := bnc.getClusterPortGroupName(types.ClusterRtrPortGroupNameBase)
						createMcastACLs(fakeOvn.nbClient, pgName, mcastACL)
						Eventually(fakeOvn.nbClient, 5).Should(
							libovsdb.HaveDataSubset(mcastACL),
							"multicast ACL did not appear in cache")
					}

					Expect(bnc.isAnyNamespaceMulticastEnabled()).To(Equal(hasACL),
						"multicast enabled state mismatch")

					err = bnc.createNodeLogicalSwitch(nodeName, hostSubnets, "", "")
					Expect(err).NotTo(HaveOccurred(), "failed to create node logical switch")

					sw := findSwitch(fakeOvn.nbClient)
					expected := "false"
					if hasACL {
						expected = "true"
					}
					Expect(sw.OtherConfig["mcast_querier"]).To(Equal(expected),
						"mcast_querier mismatch on node %s", nodeName)
					Expect(sw.OtherConfig["mcast_snoop"]).To(Equal("true"),
						"mcast_snoop should always be true")

					if useIPv4 {
						Expect(sw.OtherConfig["mcast_ip4_src"]).NotTo(BeEmpty(),
							"mcast_ip4_src should be set for IPv4")
					}
					if useIPv6 {
						Expect(sw.OtherConfig["mcast_ip6_src"]).NotTo(BeEmpty(),
							"mcast_ip6_src should be set for IPv6")
					}

					return nil
				}
				Expect(app.Run([]string{app.Name})).NotTo(HaveOccurred(),
					"failed to run querier gating test for node switch creation")
			},
			Entry("IPv4 without ACL", true, false, false),
			Entry("IPv4 with ACL", true, false, true),
			Entry("IPv6 without ACL", false, true, false),
			Entry("IPv6 with ACL", false, true, true),
			Entry("dual-stack without ACL", true, true, false),
			Entry("dual-stack with ACL", true, true, true),
		)

		It("ignores multicast ACLs owned by a different controller", func() {
			app.Action = func(*cli.Context) error {
				config.IPv4Mode = true
				node := testNode(true, false)
				hostSubnets, err := util.ParseNodeHostSubnetAnnotation(node, types.DefaultNetworkName)
				Expect(err).NotTo(HaveOccurred(), "failed to parse host subnet annotation for foreign-controller ACL test")

				nbData := []libovsdb.TestData{
					&nbdb.LogicalSwitch{Name: nodeName},
					newNetworkRouterPortGroup(getNetInfoFromNAD(nil)),
				}
				fakeOvn.startWithDBSetup(libovsdb.TestSetup{NBData: nbData},
					&corev1.NodeList{Items: []corev1.Node{*node}})
				bnc := startBaseNetworkController(fakeOvn, nil)

				foreignACL := newMcastACLForController("ns-other", "other-network-controller")
				pgName := bnc.getClusterPortGroupName(types.ClusterRtrPortGroupNameBase)
				createMcastACLs(fakeOvn.nbClient, pgName, foreignACL)
				Eventually(fakeOvn.nbClient, 5).Should(
					libovsdb.HaveDataSubset(foreignACL),
					"foreign-controller ACL did not appear in cache")

				Expect(bnc.isAnyNamespaceMulticastEnabled()).To(BeFalse(),
					"should not detect ACLs owned by a different controller")

				err = bnc.createNodeLogicalSwitch(nodeName, hostSubnets, "", "")
				Expect(err).NotTo(HaveOccurred(), "failed to create node logical switch with foreign-controller ACL")

				sw := findSwitch(fakeOvn.nbClient)
				Expect(sw.OtherConfig["mcast_querier"]).To(Equal("false"),
					"querier must stay off when only foreign-controller ACLs exist")

				return nil
			}
			Expect(app.Run([]string{app.Name})).NotTo(HaveOccurred(),
				"failed to run foreign-controller ACL isolation test")
		})

		It("toggles mcast_querier on and off via sync", func() {
			app.Action = func(*cli.Context) error {
				config.IPv4Mode = true
				node := newNode(nodeName, "192.168.126.202/24")

				sw := &nbdb.LogicalSwitch{
					Name: nodeName,
					OtherConfig: map[string]string{
						"mcast_snoop":   "true",
						"mcast_querier": "false",
						"mcast_eth_src": "0a:58:0a:80:00:01",
						"mcast_ip4_src": "10.128.0.1",
					},
				}
				rtrPG := newNetworkRouterPortGroup(getNetInfoFromNAD(nil))
				fakeOvn.startWithDBSetup(libovsdb.TestSetup{NBData: []libovsdb.TestData{sw, rtrPG}},
					&corev1.NodeList{Items: []corev1.Node{*node}})
				bnc := startBaseNetworkController(fakeOvn, nil)

				// Enable: add ACLs for two namespaces, wait for cache, then sync
				mcastACL1 := newMcastACL("ns1")
				mcastACL2 := newMcastACL("ns2")
				pgName := bnc.getClusterPortGroupName(types.ClusterRtrPortGroupNameBase)
				createMcastACLs(fakeOvn.nbClient, pgName, mcastACL1, mcastACL2)
				Eventually(fakeOvn.nbClient, 5).Should(
					libovsdb.HaveDataSubset(mcastACL1, mcastACL2),
					"multicast ACLs did not appear in cache")

				Expect(bnc.isAnyNamespaceMulticastEnabled()).To(BeTrue(),
					"multicast should be enabled after ACLs are added")
				Expect(bnc.syncNodeLogicalSwitchQueriers()).To(Succeed(), "failed to sync queriers on")
				Expect(findSwitch(fakeOvn.nbClient).OtherConfig["mcast_querier"]).To(Equal("true"),
					"querier should be on after ACL is added")

				// Partial removal: delete only ns1 ACL, querier must stay on
				Expect(libovsdbops.DeleteACLsFromPortGroups(fakeOvn.nbClient, []string{pgName}, mcastACL1)).
					To(Succeed(), "failed to delete ns1 ACL from port group")
				Eventually(fakeOvn.nbClient, 5).ShouldNot(
					libovsdb.HaveDataSubset(mcastACL1),
					"ns1 ACL should be removed from cache")

				Expect(bnc.isAnyNamespaceMulticastEnabled()).To(BeTrue(),
					"multicast should still be enabled with ns2 ACL remaining")
				Expect(bnc.syncNodeLogicalSwitchQueriers()).To(Succeed(), "failed to sync queriers after partial removal")
				Expect(findSwitch(fakeOvn.nbClient).OtherConfig["mcast_querier"]).To(Equal("true"),
					"querier should stay on when at least one namespace ACL remains")

				// Full removal: delete ns2 ACL, querier must turn off
				Expect(libovsdbops.DeleteACLsFromPortGroups(fakeOvn.nbClient, []string{pgName}, mcastACL2)).
					To(Succeed(), "failed to delete ns2 ACL from port group")
				Eventually(fakeOvn.nbClient, 5).ShouldNot(
					libovsdb.HaveDataSubset(mcastACL2),
					"ns2 ACL should be removed from cache")

				Expect(bnc.isAnyNamespaceMulticastEnabled()).To(BeFalse(),
					"multicast should be disabled after all ACLs are removed")
				Expect(bnc.syncNodeLogicalSwitchQueriers()).To(Succeed(), "failed to sync queriers off")
				Expect(findSwitch(fakeOvn.nbClient).OtherConfig["mcast_querier"]).To(Equal("false"),
					"querier should be off after all ACLs are removed")

				return nil
			}
			Expect(app.Run([]string{app.Name})).NotTo(HaveOccurred(),
				"failed to run querier sync toggle test")
		})

		It("does not modify switches owned by a different network during sync", func() {
			app.Action = func(*cli.Context) error {
				config.IPv4Mode = true
				node := newNode(nodeName, "192.168.126.202/24")

				sw := &nbdb.LogicalSwitch{
					Name: nodeName,
					OtherConfig: map[string]string{
						"mcast_snoop":   "true",
						"mcast_querier": "false",
						"mcast_eth_src": "0a:58:0a:80:00:01",
						"mcast_ip4_src": "10.128.0.1",
					},
				}
				foreignSw := &nbdb.LogicalSwitch{
					Name: "foreign-net-node",
					OtherConfig: map[string]string{
						"mcast_snoop":   "true",
						"mcast_querier": "false",
						"mcast_eth_src": "0a:58:0a:81:00:01",
						"mcast_ip4_src": "10.129.0.1",
					},
					ExternalIDs: map[string]string{
						types.NetworkExternalID: "other-network",
					},
				}
				rtrPG := newNetworkRouterPortGroup(getNetInfoFromNAD(nil))
				fakeOvn.startWithDBSetup(libovsdb.TestSetup{NBData: []libovsdb.TestData{sw, foreignSw, rtrPG}},
					&corev1.NodeList{Items: []corev1.Node{*node}})
				bnc := startBaseNetworkController(fakeOvn, nil)

				mcastACL := newMcastACL("ns1")
				pgName := bnc.getClusterPortGroupName(types.ClusterRtrPortGroupNameBase)
				createMcastACLs(fakeOvn.nbClient, pgName, mcastACL)
				Eventually(fakeOvn.nbClient, 5).Should(
					libovsdb.HaveDataSubset(mcastACL),
					"multicast ACL did not appear in cache")

				Expect(bnc.syncNodeLogicalSwitchQueriers()).To(Succeed(), "failed to sync queriers")

				foreignSwitches, err := libovsdbops.FindLogicalSwitchesWithPredicate(fakeOvn.nbClient, func(item *nbdb.LogicalSwitch) bool {
					return item.Name == "foreign-net-node"
				})
				Expect(err).NotTo(HaveOccurred(), "failed to find foreign-network switch")
				Expect(foreignSwitches).To(HaveLen(1), "expected exactly one foreign-network switch")
				Expect(foreignSwitches[0].OtherConfig["mcast_querier"]).To(Equal("false"),
					"foreign-network switch querier must remain unchanged after sync")

				return nil
			}
			Expect(app.Run([]string{app.Name})).NotTo(HaveOccurred(),
				"failed to run foreign-network isolation test")
		})
	})
})
