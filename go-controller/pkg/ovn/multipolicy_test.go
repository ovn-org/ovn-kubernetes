// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"context"
	"fmt"
	"net"
	"strings"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	mnpapi "github.com/k8snetworkplumbingwg/multi-networkpolicy/pkg/apis/k8s.cni.cncf.io/v1beta1"
	nettypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/onsi/gomega/format"
	"github.com/urfave/cli/v2"

	corev1 "k8s.io/api/core/v1"
	knet "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	ovncnitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func convertNetPolicyToMultiNetPolicy(policy *knet.NetworkPolicy) *mnpapi.MultiNetworkPolicy {
	var mpolicy mnpapi.MultiNetworkPolicy
	var ipb *mnpapi.IPBlock

	mpolicy.Name = policy.Name
	mpolicy.Namespace = policy.Namespace
	mpolicy.Spec.PodSelector = policy.Spec.PodSelector
	mpolicy.Annotations = policy.Annotations
	mpolicy.Spec.Ingress = make([]mnpapi.MultiNetworkPolicyIngressRule, len(policy.Spec.Ingress))
	for i, ingress := range policy.Spec.Ingress {
		var mingress mnpapi.MultiNetworkPolicyIngressRule
		mingress.Ports = make([]mnpapi.MultiNetworkPolicyPort, len(ingress.Ports))
		for j, port := range ingress.Ports {
			mingress.Ports[j] = mnpapi.MultiNetworkPolicyPort{
				Protocol: port.Protocol,
				Port:     port.Port,
			}
		}
		mingress.From = make([]mnpapi.MultiNetworkPolicyPeer, len(ingress.From))
		for j, from := range ingress.From {
			ipb = nil
			if from.IPBlock != nil {
				ipb = &mnpapi.IPBlock{CIDR: from.IPBlock.CIDR, Except: from.IPBlock.Except}
			}
			mingress.From[j] = mnpapi.MultiNetworkPolicyPeer{
				PodSelector:       from.PodSelector,
				NamespaceSelector: from.NamespaceSelector,
				IPBlock:           ipb,
			}
		}
		mpolicy.Spec.Ingress[i] = mingress
	}
	mpolicy.Spec.Egress = make([]mnpapi.MultiNetworkPolicyEgressRule, len(policy.Spec.Egress))
	for i, egress := range policy.Spec.Egress {
		var megress mnpapi.MultiNetworkPolicyEgressRule
		megress.Ports = make([]mnpapi.MultiNetworkPolicyPort, len(egress.Ports))
		for j, port := range egress.Ports {
			megress.Ports[j] = mnpapi.MultiNetworkPolicyPort{
				Protocol: port.Protocol,
				Port:     port.Port,
			}
		}
		megress.To = make([]mnpapi.MultiNetworkPolicyPeer, len(egress.To))
		for j, to := range egress.To {
			ipb = nil
			if to.IPBlock != nil {
				ipb = &mnpapi.IPBlock{CIDR: to.IPBlock.CIDR, Except: to.IPBlock.Except}
			}
			megress.To[j] = mnpapi.MultiNetworkPolicyPeer{
				PodSelector:       to.PodSelector,
				NamespaceSelector: to.NamespaceSelector,
				IPBlock:           ipb,
			}
		}
		mpolicy.Spec.Egress[i] = megress
	}
	mpolicy.Spec.PolicyTypes = make([]mnpapi.MultiPolicyType, len(policy.Spec.PolicyTypes))
	for i, policytype := range policy.Spec.PolicyTypes {
		mpolicy.Spec.PolicyTypes[i] = mnpapi.MultiPolicyType(policytype)
	}
	return &mpolicy
}

func addPodNetwork(pod *corev1.Pod, udnPodInfos map[string]*udnPodInfo) {
	nadNames := []string{}
	for _, podInfo := range udnPodInfos {
		for nadName := range podInfo.allportInfo {
			nadNames = append(nadNames, nadName)
		}
	}
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[nettypes.NetworkAttachmentAnnot] = strings.Join(nadNames, ",")
}

func (p testPod) populateUserDefinedNetworkLogicalSwitchCache(ocInfo userDefinedNetworkControllerInfo) {
	var err error
	switch ocInfo.bnc.TopologyType() {
	case ovntypes.Layer3Topology:
		podInfo := p.udnPodInfos[ocInfo.bnc.GetNetworkName()]
		err = ocInfo.bnc.lsManager.AddOrUpdateSwitch(ocInfo.bnc.GetNetworkScopedName(p.nodeName), []*net.IPNet{ovntest.MustParseIPNet(podInfo.nodeSubnet)}, nil, nil)
	case ovntypes.Layer2Topology:
		subnet := ocInfo.bnc.Subnets()[0]
		err = ocInfo.bnc.lsManager.AddOrUpdateSwitch(ocInfo.bnc.GetNetworkScopedName(ovntypes.OVNLayer2Switch), []*net.IPNet{subnet.CIDR}, nil, nil)
	case ovntypes.LocalnetTopology:
		subnet := ocInfo.bnc.Subnets()[0]
		err = ocInfo.bnc.lsManager.AddOrUpdateSwitch(ocInfo.bnc.GetNetworkScopedName(ovntypes.OVNLocalnetSwitch), []*net.IPNet{subnet.CIDR}, nil, nil)
	}
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
}

func getExpectedDataPodsAndSwitchesForUserDefinedNetwork(fakeOvn *FakeOVN, pods []testPod, netInfo util.NetInfo) []libovsdb.TestData {
	data := []libovsdb.TestData{}
	for _, ocInfo := range fakeOvn.userDefinedNetworkControllers {
		nodeslsps := make(map[string][]string)
		var switchName string
		for _, pod := range pods {
			podInfo, ok := pod.udnPodInfos[ocInfo.bnc.GetNetworkName()]
			if !ok {
				continue
			}
			for nad, portInfo := range podInfo.allportInfo {
				portName := portInfo.portName
				var lspUUID string
				if len(portInfo.portUUID) == 0 {
					lspUUID = portName + "-UUID"
				} else {
					lspUUID = portInfo.portUUID
				}
				podAddr := fmt.Sprintf("%s %s", portInfo.podMAC, portInfo.podIP)
				lsp := &nbdb.LogicalSwitchPort{
					UUID:      lspUUID,
					Name:      portName,
					Addresses: []string{podAddr},
					ExternalIDs: map[string]string{
						"pod":                       "true",
						"namespace":                 pod.namespace,
						ovntypes.NetworkExternalID:  ocInfo.bnc.GetNetworkName(),
						ovntypes.NADExternalID:      nad,
						ovntypes.TopologyExternalID: ocInfo.bnc.TopologyType(),
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
				if ocInfo.bnc.isLayer2WithInterconnectTransport() {
					lsp.Options[libovsdbops.RequestedTnlKey] = "1"
				}
				data = append(data, lsp)
				switch ocInfo.bnc.TopologyType() {
				case ovntypes.Layer3Topology:
					switchName = ocInfo.bnc.GetNetworkScopedName(pod.nodeName)
				case ovntypes.Layer2Topology:
					switchName = ocInfo.bnc.GetNetworkScopedName(ovntypes.OVNLayer2Switch)
				case ovntypes.LocalnetTopology:
					switchName = ocInfo.bnc.GetNetworkScopedName(ovntypes.OVNLocalnetSwitch)
				}
				nodeslsps[switchName] = append(nodeslsps[switchName], lspUUID)
			}
			data = append(data, &nbdb.LogicalSwitch{
				UUID:  switchName + "-UUID",
				Name:  switchName,
				Ports: nodeslsps[switchName],
				ExternalIDs: map[string]string{
					ovntypes.NetworkExternalID:     ocInfo.bnc.GetNetworkName(),
					ovntypes.NetworkRoleExternalID: getNetworkRole(netInfo),
				},
			})
		}
	}
	return data
}

var _ = ginkgo.Describe("OVN MultiNetworkPolicy Operations", func() {
	const (
		namespaceName1                = "namespace1"
		namespaceName2                = "namespace2"
		netPolicyName1                = "networkpolicy1"
		nodeName                      = "node1"
		userDefinedNetworkName        = "network1"
		nadName                       = "nad1"
		labelName              string = "pod-name"
		labelVal               string = "server"
		portNum                int32  = 81
	)
	var (
		app       *cli.App
		fakeOvn   *FakeOVN
		initialDB libovsdb.TestSetup

		gomegaFormatMaxLength int
		nadNamespacedName     string
		nad, nad2             *nettypes.NetworkAttachmentDefinition
		netInfo               util.NetInfo
	)

	ginkgo.BeforeEach(func() {
		// Restore global default values before each testcase
		gomega.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		config.OVNKubernetesFeature.EnableMultiNetworkPolicy = true

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

		fakeOvn = NewFakeOVN(false, nodeName)

		gomegaFormatMaxLength = format.MaxLength
		format.MaxLength = 0
		logicalSwitch := &nbdb.LogicalSwitch{
			Name: nodeName,
			UUID: nodeName + "_UUID",
		}
		initialData := getHairpinningACLsV4AndPortGroup()
		initialData = append(initialData, logicalSwitch)
		initialDB = libovsdb.TestSetup{
			NBData: initialData,
		}
	})

	ginkgo.AfterEach(func() {
		fakeOvn.shutdown()
		format.MaxLength = gomegaFormatMaxLength
	})

	// setUserDefinedNetworkTestData sets relevant test data (NAD, NetInfo & NB DB
	// initial data) assuming a user-defined network of the given topology and
	// subnet
	setUserDefinedNetworkTestData := func(topology, subnets string) {
		nadNamespacedName = util.GetNADName(namespaceName1, nadName)
		netconf := ovncnitypes.NetConf{
			NetConf: cnitypes.NetConf{
				Name: userDefinedNetworkName,
				Type: "ovn-k8s-cni-overlay",
			},
			Topology: topology,
			NADName:  nadNamespacedName,
			Subnets:  subnets,
		}

		var err error
		nad, err = newNetworkAttachmentDefinition(
			namespaceName1,
			nadName,
			netconf,
		)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		netconf.NADName = util.GetNADName(namespaceName2, nadName)
		nad2, err = newNetworkAttachmentDefinition(
			namespaceName2,
			nadName,
			netconf,
		)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		netInfo, err = util.NewNetInfo(&netconf)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		switch topology {
		case ovntypes.Layer2Topology:
			initialDB.NBData = append(initialDB.NBData, &nbdb.LogicalSwitch{
				Name: netInfo.GetNetworkScopedName(ovntypes.OVNLayer2Switch),
				UUID: netInfo.GetNetworkScopedName(ovntypes.OVNLayer2Switch) + "_UUID",
				ExternalIDs: map[string]string{
					ovntypes.NetworkExternalID:     userDefinedNetworkName,
					ovntypes.NetworkRoleExternalID: getNetworkRole(netInfo),
				},
			})
		case ovntypes.LocalnetTopology:
			initialDB.NBData = append(initialDB.NBData, &nbdb.LogicalSwitch{
				Name: netInfo.GetNetworkScopedName(ovntypes.OVNLocalnetSwitch),
				UUID: netInfo.GetNetworkScopedName(ovntypes.OVNLocalnetSwitch) + "_UUID",
				ExternalIDs: map[string]string{
					ovntypes.NetworkExternalID:     userDefinedNetworkName,
					ovntypes.NetworkRoleExternalID: getNetworkRole(netInfo),
				},
			})
		}
	}

	startOvn := func(dbSetup libovsdb.TestSetup, watchNodes bool, nodes []corev1.Node, namespaces []corev1.Namespace, networkPolicies []knet.NetworkPolicy,
		multinetworkPolicies []mnpapi.MultiNetworkPolicy, nads []nettypes.NetworkAttachmentDefinition,
		pods []testPod, podLabels map[string]string) {
		var podsList []corev1.Pod
		for _, testPod := range pods {
			knetPod := ovntest.NewPod(testPod.namespace, testPod.podName, testPod.nodeName, testPod.podIP)
			if len(podLabels) > 0 {
				knetPod.Labels = podLabels
			}
			addPodNetwork(knetPod, testPod.udnPodInfos)
			setPodAnnotations(knetPod, testPod)
			podsList = append(podsList, *knetPod)
		}
		fakeOvn.startWithDBSetup(dbSetup,
			&corev1.NamespaceList{
				Items: namespaces,
			},
			&corev1.PodList{
				Items: podsList,
			},
			&corev1.NodeList{
				Items: nodes,
			},
			&knet.NetworkPolicyList{
				Items: networkPolicies,
			},
			&mnpapi.MultiNetworkPolicyList{
				Items: multinetworkPolicies,
			},
			&nettypes.NetworkAttachmentDefinitionList{
				Items: nads,
			},
		)
		var err error
		if watchNodes {
			// add the transit switch port bindings on behalf of ovn-controller
			// before WatchNodes so it does not synchrounously wait for them
			for _, node := range nodes {
				transistSwitchPortName := ovntypes.TransitSwitchToRouterPrefix + node.Name
				err := libovsdb.CreateTransitSwitchPortBindings(fakeOvn.sbClient, ovntypes.TransitSwitch, transistSwitchPortName)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			}
			startDefaultNodeController(fakeOvn.controller)
		}

		if namespaces != nil {
			err = fakeOvn.controller.WatchNamespaces()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}

		for _, testPod := range pods {
			testPod.populateLogicalSwitchCache(fakeOvn)
		}
		if pods != nil {
			err = fakeOvn.controller.WatchPods()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}
		err = fakeOvn.controller.WatchNetworkPolicy()
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		for _, ocInfo := range fakeOvn.userDefinedNetworkControllers {
			if watchNodes {
				if ocInfo.bnc.TopologyType() == ovntypes.Layer3Topology {
					// add the transit switch port bindings on behalf of ovn-controller
					// before registering the node handler so it does not synchronously wait for them
					for _, node := range nodes {
						transistSwitchPortName := ocInfo.bnc.GetNetworkScopedName(ovntypes.TransitSwitchToRouterPrefix + node.Name)
						err = libovsdb.CreateTransitSwitchPortBindings(fakeOvn.sbClient, ovntypes.TransitSwitch, transistSwitchPortName)
						gomega.Expect(err).NotTo(gomega.HaveOccurred())
					}
				}
				gomega.Expect(fakeOvn.registerUDNNodeHandler(ocInfo.bnc.GetNetworkName())).To(gomega.Succeed())
			}

			if namespaces != nil {
				err = ocInfo.bnc.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			}

			for _, testPod := range pods {
				testPod.populateUserDefinedNetworkLogicalSwitchCache(ocInfo)
			}
			if pods != nil {
				err = ocInfo.bnc.WatchPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			}

			err = ocInfo.bnc.WatchMultiNetworkPolicy()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}
	}

	getUpdatedInitialDB := func(tPods []testPod) []libovsdb.TestData {
		updatedSwitchAndPods := getDefaultNetExpectedPodsAndSwitches(tPods, []string{nodeName})
		udnSwitchesAndPods := getExpectedDataPodsAndSwitchesForUserDefinedNetwork(fakeOvn, tPods, netInfo)
		if len(udnSwitchesAndPods) != 0 {
			updatedSwitchAndPods = append(updatedSwitchAndPods, udnSwitchesAndPods...)
		}
		return append(getHairpinningACLsV4AndPortGroup(), updatedSwitchAndPods...)
	}

	ginkgo.Context("during execution", func() {
		ginkgo.It("correctly creating an multinetworkPolicy with a peer namespace label", func() {
			app.Action = func(*cli.Context) error {
				var err error

				topology := ovntypes.Layer2Topology
				subnets := "10.1.0.0/24"
				setUserDefinedNetworkTestData(topology, subnets)

				namespace1 := *ovntest.NewNamespace(namespaceName1)
				namespace2 := *ovntest.NewNamespace(namespaceName2)
				policy := ovntest.NewMatchLabelsNetworkPolicy(netPolicyName1, namespace1.Name,
					namespace2.Name, "", true, true)
				policy.Annotations = map[string]string{PolicyForAnnotation: nadNamespacedName}
				mpolicy := convertNetPolicyToMultiNetPolicy(policy)

				watchNodes := false
				node := *newNode(nodeName, "192.168.126.202/24")

				startOvn(initialDB, watchNodes, []corev1.Node{node}, []corev1.Namespace{namespace1, namespace2}, nil, nil,
					[]nettypes.NetworkAttachmentDefinition{*nad, *nad2}, nil, nil)

				_, err = fakeOvn.fakeClient.MultiNetworkPolicyClient.K8sCniCncfIoV1beta1().MultiNetworkPolicies(mpolicy.Namespace).
					Create(context.TODO(), mpolicy, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				_, err = fakeOvn.fakeClient.MultiNetworkPolicyClient.K8sCniCncfIoV1beta1().MultiNetworkPolicies(mpolicy.Namespace).
					Get(context.TODO(), mpolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectedData := getNamespaceWithSinglePolicyExpectedData(
					newNetpolDataParams(policy).
						withNetInfo(netInfo),
					initialDB.NBData)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.DescribeTable("correctly creates and deletes network policy and multi network policy with the same policy",
			func(allowICMPNetworkPolicy bool) {
				app.Action = func(*cli.Context) error {
					var err error

					config.OVNKubernetesFeature.AllowICMPNetworkPolicy = allowICMPNetworkPolicy
					topology := ovntypes.Layer2Topology
					subnets := "10.1.0.0/24"
					setUserDefinedNetworkTestData(topology, subnets)

					namespace1 := *ovntest.NewNamespace(namespaceName1)
					nPodTest := getTestPod(namespace1.Name, nodeName)
					nPodTest.addNetwork(userDefinedNetworkName, nadNamespacedName, "", "", "", "10.1.0.1", "0a:58:0a:01:00:01", "secondary", 1, nil)
					networkPolicy := getPortNetworkPolicy(netPolicyName1, namespace1.Name, labelName, labelVal, portNum)

					watchNodes := false
					node := *newNode(nodeName, "192.168.126.202/24")

					startOvn(initialDB, watchNodes, []corev1.Node{node}, []corev1.Namespace{namespace1}, nil, nil,
						[]nettypes.NetworkAttachmentDefinition{*nad}, []testPod{nPodTest}, map[string]string{labelName: labelVal})

					ginkgo.By("Creating networkPolicy applied to the pod")
					_, err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
						Create(context.TODO(), networkPolicy, metav1.CreateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					_, err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
						Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					dataParams := newNetpolDataParams(networkPolicy).
						withLocalPortUUIDs(nPodTest.portUUID).
						withTCPPeerPorts(portNum)
					gressPolicyExpectedData1 := getPolicyData(dataParams)
					defaultDenyExpectedData1 := getDefaultDenyData(dataParams)
					initData := getUpdatedInitialDB([]testPod{nPodTest})
					expectedData1 := append(initData, gressPolicyExpectedData1...)
					expectedData1 = append(expectedData1, defaultDenyExpectedData1...)
					gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData1...))

					ginkgo.By("Creating multi-networkPolicy applied to the pod")
					mpolicy := convertNetPolicyToMultiNetPolicy(networkPolicy)
					mpolicy.Annotations = map[string]string{PolicyForAnnotation: nadNamespacedName}

					_, err = fakeOvn.fakeClient.MultiNetworkPolicyClient.K8sCniCncfIoV1beta1().MultiNetworkPolicies(mpolicy.Namespace).
						Create(context.TODO(), mpolicy, metav1.CreateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					_, err = fakeOvn.fakeClient.MultiNetworkPolicyClient.K8sCniCncfIoV1beta1().MultiNetworkPolicies(mpolicy.Namespace).
						Get(context.TODO(), mpolicy.Name, metav1.GetOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					portInfo := nPodTest.getNetworkPortInfo(userDefinedNetworkName, nadNamespacedName)
					gomega.Expect(portInfo).NotTo(gomega.BeNil())

					dataParams2 := newNetpolDataParams(networkPolicy).
						withLocalPortUUIDs(portInfo.portUUID).
						withTCPPeerPorts(portNum).
						withNetInfo(netInfo)
					gressPolicyExpectedData2 := getPolicyData(dataParams2)
					defaultDenyExpectedData2 := getDefaultDenyData(dataParams2)
					expectedData2 := append(expectedData1, gressPolicyExpectedData2...)
					expectedData2 = append(expectedData2, defaultDenyExpectedData2...)
					gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData2...))

					// Delete the multi network policy
					ginkgo.By("Deleting the multi network policy")
					err = fakeOvn.fakeClient.MultiNetworkPolicyClient.K8sCniCncfIoV1beta1().MultiNetworkPolicies(mpolicy.Namespace).
						Delete(context.TODO(), mpolicy.Name, metav1.DeleteOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData1))

					ginkgo.By("Deleting the network policy")
					err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
						Delete(context.TODO(), networkPolicy.Name, metav1.DeleteOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(initData))
					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			},
			ginkgo.Entry("with allow ICMP network policy disabled", false),
			ginkgo.Entry("with allow ICMP network policy enabled", true),
		)

		ginkgo.It("correctly creates, updates and deletes multi network policies", func() {
			app.Action = func(*cli.Context) error {
				config.OVNKubernetesFeature.EnableStatelessNetPol = true
				var err error

				topology := ovntypes.Layer2Topology
				subnets := "10.1.0.0/24"
				setUserDefinedNetworkTestData(topology, subnets)

				namespace1 := *ovntest.NewNamespace(namespaceName1)
				namespace2 := *ovntest.NewNamespace(namespaceName2)
				nPodTest := getTestPod(namespace1.Name, nodeName)
				nPodTest.addNetwork(userDefinedNetworkName, nadNamespacedName, "", "", "", "10.1.0.1", "0a:58:0a:01:00:01", "secondary", 1, nil)
				networkPolicy := getPortNetworkPolicy(netPolicyName1, namespace1.Name, labelName, labelVal, portNum)

				watchNodes := false
				node := *newNode(nodeName, "192.168.126.202/24")

				startOvn(initialDB, watchNodes, []corev1.Node{node}, []corev1.Namespace{namespace1, namespace2}, nil, nil,
					[]nettypes.NetworkAttachmentDefinition{*nad, *nad2}, []testPod{nPodTest}, map[string]string{labelName: labelVal})

				ginkgo.By("Creating multi-networkPolicy applied to the pod")
				mpolicy := convertNetPolicyToMultiNetPolicy(networkPolicy)
				mpolicy.Annotations = map[string]string{PolicyForAnnotation: nadNamespacedName}

				_, err = fakeOvn.fakeClient.MultiNetworkPolicyClient.K8sCniCncfIoV1beta1().MultiNetworkPolicies(mpolicy.Namespace).
					Create(context.TODO(), mpolicy, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				_, err = fakeOvn.fakeClient.MultiNetworkPolicyClient.K8sCniCncfIoV1beta1().MultiNetworkPolicies(mpolicy.Namespace).
					Get(context.TODO(), mpolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				portInfo := nPodTest.getNetworkPortInfo(userDefinedNetworkName, nadNamespacedName)
				gomega.Expect(portInfo).NotTo(gomega.BeNil())

				dataParams2 := newNetpolDataParams(networkPolicy).
					withLocalPortUUIDs(portInfo.portUUID).
					withTCPPeerPorts(portNum).
					withNetInfo(netInfo).
					withPeerIPs(portInfo.podIP)
				gressPolicyExpectedData2 := getPolicyData(dataParams2)
				defaultDenyExpectedData2 := getDefaultDenyData(dataParams2)
				initData := getUpdatedInitialDB([]testPod{nPodTest})
				expectedData2 := append(initData, gressPolicyExpectedData2...)
				expectedData2 = append(expectedData2, defaultDenyExpectedData2...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData2))

				ginkgo.By("Updating the multi network policy ingress and egress rules")
				intstrPort := intstr.FromInt(int(portNum + 1))
				mpolicy.Spec.Ingress = []mnpapi.MultiNetworkPolicyIngressRule{{
					Ports: []mnpapi.MultiNetworkPolicyPort{{
						Port: &intstrPort,
					}},
				}}
				mpolicy.Spec.Egress = []mnpapi.MultiNetworkPolicyEgressRule{{
					Ports: []mnpapi.MultiNetworkPolicyPort{{
						Port: &intstrPort,
					}},
				}}

				_, err = fakeOvn.fakeClient.MultiNetworkPolicyClient.K8sCniCncfIoV1beta1().MultiNetworkPolicies(mpolicy.Namespace).
					Update(context.TODO(), mpolicy, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				updatedNetworkPolicy, err := convertMultiNetPolicyToNetPolicy(mpolicy, true)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				dataParams3 := newNetpolDataParams(updatedNetworkPolicy).
					withLocalPortUUIDs(portInfo.portUUID).
					withTCPPeerPorts(portNum + 1).
					withNetInfo(netInfo)
				gressPolicyExpectedData3 := getPolicyData(dataParams3)
				defaultDenyExpectedData3 := getDefaultDenyData(dataParams3)
				expectedData3 := append(initData, gressPolicyExpectedData3...)
				expectedData3 = append(expectedData3, defaultDenyExpectedData3...)

				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData3))
				gomega.Consistently(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData3))

				ginkgo.By("Updating the multi network policy stateless OVN ACLs annotation to true")
				mpolicy.Annotations[ovnStatelessNetPolAnnotationName] = "true"
				_, err = fakeOvn.fakeClient.MultiNetworkPolicyClient.K8sCniCncfIoV1beta1().MultiNetworkPolicies(mpolicy.Namespace).
					Update(context.TODO(), mpolicy, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				updatedNetworkPolicy, err = convertMultiNetPolicyToNetPolicy(mpolicy, true)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				dataParams4 := newNetpolDataParams(updatedNetworkPolicy).
					withLocalPortUUIDs(portInfo.portUUID).
					withTCPPeerPorts(portNum + 1).
					withStateless(true).
					withNetInfo(netInfo)
				gressPolicyExpectedData4 := getPolicyData(dataParams4)
				defaultDenyExpectedData4 := getDefaultDenyData(dataParams4)
				expectedData4 := append(initData, gressPolicyExpectedData4...)
				expectedData4 = append(expectedData4, defaultDenyExpectedData4...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData4))

				ginkgo.By("Updating the multi network policy stateless OVN ACLs annotation to false")
				mpolicy.Annotations[ovnStatelessNetPolAnnotationName] = "false"
				_, err = fakeOvn.fakeClient.MultiNetworkPolicyClient.K8sCniCncfIoV1beta1().MultiNetworkPolicies(mpolicy.Namespace).
					Update(context.TODO(), mpolicy, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				updatedNetworkPolicy, err = convertMultiNetPolicyToNetPolicy(mpolicy, true)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				dataParams5 := newNetpolDataParams(updatedNetworkPolicy).
					withLocalPortUUIDs(portInfo.portUUID).
					withTCPPeerPorts(portNum + 1).
					withStateless(false).
					withNetInfo(netInfo)
				gressPolicyExpectedData5 := getPolicyData(dataParams5)
				defaultDenyExpectedData5 := getDefaultDenyData(dataParams5)
				expectedData5 := append(initData, gressPolicyExpectedData5...)
				expectedData5 = append(expectedData5, defaultDenyExpectedData5...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData5))

				ginkgo.By("Updating the multi network policy labels")
				mpolicy.Labels = map[string]string{labelName: labelVal}
				_, err = fakeOvn.fakeClient.MultiNetworkPolicyClient.K8sCniCncfIoV1beta1().MultiNetworkPolicies(mpolicy.Namespace).
					Update(context.TODO(), mpolicy, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Consistently(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData5))

				ginkgo.By("Updating the multi network policy unrelated annotation")
				mpolicy.Annotations["test-annotation"] = "test-value"
				_, err = fakeOvn.fakeClient.MultiNetworkPolicyClient.K8sCniCncfIoV1beta1().MultiNetworkPolicies(mpolicy.Namespace).
					Update(context.TODO(), mpolicy, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Consistently(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData5))

				ginkgo.By("Updating the multi network policy policy-for annotation to the other namespace")
				mpolicy.Annotations[PolicyForAnnotation] = util.GetNADName(namespace2.Name, nadName)
				_, err = fakeOvn.fakeClient.MultiNetworkPolicyClient.K8sCniCncfIoV1beta1().MultiNetworkPolicies(mpolicy.Namespace).
					Update(context.TODO(), mpolicy, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData5))

				ginkgo.By("Updating the multi network policy policy-for annotation to the original namespace")
				mpolicy.Annotations[PolicyForAnnotation] = nadNamespacedName
				_, err = fakeOvn.fakeClient.MultiNetworkPolicyClient.K8sCniCncfIoV1beta1().MultiNetworkPolicies(mpolicy.Namespace).
					Update(context.TODO(), mpolicy, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData5))

				// Delete the multi network policy
				ginkgo.By("Deleting the multi network policy")
				err = fakeOvn.fakeClient.MultiNetworkPolicyClient.K8sCniCncfIoV1beta1().MultiNetworkPolicies(mpolicy.Namespace).
					Delete(context.TODO(), mpolicy.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(initData))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})
})
