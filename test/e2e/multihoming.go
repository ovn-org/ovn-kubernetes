// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/feature"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"

	mnpapi "github.com/k8snetworkplumbingwg/multi-networkpolicy/pkg/apis/k8s.cni.cncf.io/v1beta1"
	mnpclient "github.com/k8snetworkplumbingwg/multi-networkpolicy/pkg/client/clientset/versioned/typed/k8s.cni.cncf.io/v1beta1"
	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	nadclient "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/typed/k8s.cni.cncf.io/v1"

	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/images"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider"
	infraapi "github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/api"
)

const (
	PolicyForAnnotation = "k8s.v1.cni.cncf.io/policy-for"
	nodeHostnameKey     = "kubernetes.io/hostname"

	externalNetworkSubnetV4 = "172.20.0.0/16"
	externalNetworkSubnetV6 = "fd00:20::/64"

	fromHostSubnet      = "from-host-subnet"      // the test will generate an IP from the host subnet
	fromExternalNetwork = "from-external-network" // the test will generate an IP from a subnet that the cluster is not aware of
)

var _ = Describe("Multi Homing", feature.MultiHoming, func() {
	const (
		podName                      = "tinypod"
		secondaryNetworkCIDR         = "172.31.0.0/16" // last subnet in private range 172.16.0.0/12 (rfc1918)
		secondaryNetworkName         = "tenant-blue"
		secondaryFlatL2IgnoreCIDR    = "172.31.0.0/29"
		secondaryFlatL2NetworkCIDR   = "172.31.0.0/24"
		secondaryLocalnetIgnoreCIDR  = "60.128.0.0/29"
		secondaryLocalnetNetworkCIDR = "60.128.0.0/24"
		netPrefixLengthPerNode       = 24
		localnetVLANID               = 10
		secondaryIPv6CIDR            = "2010:100:200::0/60"
		netPrefixLengthIPv6PerNode   = 64
	)
	f := wrappedTestFramework("multi-homing")

	var (
		cs          clientset.Interface
		nadClient   nadclient.K8sCniCncfIoV1Interface
		mnpClient   mnpclient.K8sCniCncfIoV1beta1Interface
		providerCtx infraapi.Context
	)

	BeforeEach(func() {
		cs = f.ClientSet

		var err error
		nadClient, err = nadclient.NewForConfig(f.ClientConfig())
		Expect(err).NotTo(HaveOccurred())
		mnpClient, err = mnpclient.NewForConfig(f.ClientConfig())
		Expect(err).NotTo(HaveOccurred())
		providerCtx = infraprovider.Get().NewTestContext()
	})

	Context("A single pod with an OVN-K secondary network", func() {
		DescribeTable("is able to get to the Running phase", func(netConfigParams networkAttachmentConfigParams, podConfig podConfiguration) {
			netConfig := newNetworkAttachmentConfig(netConfigParams)

			netConfig.namespace = f.Namespace.Name
			podConfig.namespace = f.Namespace.Name

			if netConfig.topology == "localnet" {
				By("applying ovs bridge mapping")
				Expect(infraprovider.SetupUnderlay(providerCtx, f, infraapi.Underlay{
					LogicalNetworkName: netConfig.networkName,
					VlanID:             netConfig.vlanID,
				})).To(Succeed())
			}

			By("creating the attachment configuration")
			_, err := nadClient.NetworkAttachmentDefinitions(netConfig.namespace).Create(
				context.Background(),
				generateNetAttachDef(netConfig.namespace, netConfig.name, generateNADSpec(netConfig)),
				metav1.CreateOptions{},
			)
			Expect(err).NotTo(HaveOccurred())

			By("creating the pod using a secondary network")
			pod, err := cs.CoreV1().Pods(podConfig.namespace).Create(
				context.Background(),
				generatePodSpec(podConfig),
				metav1.CreateOptions{},
			)
			Expect(err).NotTo(HaveOccurred())

			By("asserting the pod gets to the `Ready` phase")
			Eventually(func() v1.PodPhase {
				updatedPod, err := cs.CoreV1().Pods(podConfig.namespace).Get(context.Background(), pod.GetName(), metav1.GetOptions{})
				if err != nil {
					return v1.PodFailed
				}
				return updatedPod.Status.Phase
			}, 2*time.Minute, 6*time.Second).Should(Equal(v1.PodRunning))

			if netConfig.excludeCIDRs != nil {
				podIP, err := podIPForAttachment(cs, pod.GetNamespace(), pod.GetName(), secondaryNetworkName, 0)
				Expect(err).NotTo(HaveOccurred())
				subnet, err := getNetCIDRSubnet(netConfig.cidr)
				Expect(err).NotTo(HaveOccurred())
				Expect(inRange(subnet, podIP)).To(Succeed())
				for _, excludedRange := range netConfig.excludeCIDRs {
					Expect(inRange(excludedRange, podIP)).To(
						MatchError(fmt.Errorf("ip [%s] is NOT in range %s", podIP, excludedRange)))
				}
			}
		},
			Entry(
				"when attaching to an L3 - routed - network",
				networkAttachmentConfigParams{
					cidr:     netCIDR(secondaryNetworkCIDR, netPrefixLengthPerNode),
					name:     secondaryNetworkName,
					topology: "layer3",
				},
				podConfiguration{
					attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:        podName,
				},
			),
			Entry(
				"when attaching to an L3 - routed - network with IPv6 network",
				networkAttachmentConfigParams{
					cidr:     netCIDR(secondaryIPv6CIDR, netPrefixLengthIPv6PerNode),
					name:     secondaryNetworkName,
					topology: "layer3",
				},
				podConfiguration{
					attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:        podName,
				},
			),
			Entry(
				"when attaching to an L2 - switched - network",
				networkAttachmentConfigParams{
					cidr:     secondaryFlatL2NetworkCIDR,
					name:     secondaryNetworkName,
					topology: "layer2",
				},
				podConfiguration{
					attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:        podName,
				},
			),
			Entry(
				"when attaching to an L2 - switched - network featuring `excludeCIDR`s",
				networkAttachmentConfigParams{
					cidr:         secondaryFlatL2NetworkCIDR,
					name:         secondaryNetworkName,
					topology:     "layer2",
					excludeCIDRs: []string{secondaryFlatL2IgnoreCIDR},
				},
				podConfiguration{
					attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:        podName,
				},
			),
			Entry(
				"when attaching to an L2 - switched - network without IPAM",
				networkAttachmentConfigParams{
					name:     secondaryNetworkName,
					topology: "layer2",
				},
				podConfiguration{
					attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:        podName,
				},
			),
			Entry(
				"when attaching to an L2 - switched - network with an IPv6 subnet",
				networkAttachmentConfigParams{
					cidr:     secondaryIPv6CIDR,
					name:     secondaryNetworkName,
					topology: "layer2",
				},
				podConfiguration{
					attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:        podName,
				},
			),
			Entry(
				"when attaching to an L2 - switched - network with a dual stack configuration",
				networkAttachmentConfigParams{
					cidr:     strings.Join([]string{secondaryFlatL2NetworkCIDR, secondaryIPv6CIDR}, ","),
					name:     secondaryNetworkName,
					topology: "layer2",
				},
				podConfiguration{
					attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:        podName,
				},
			),
			Entry(
				"when attaching to a localnet - switched - network",
				networkAttachmentConfigParams{
					cidr:     secondaryLocalnetNetworkCIDR,
					name:     secondaryNetworkName,
					topology: "localnet",
					vlanID:   localnetVLANID,
				},
				podConfiguration{
					attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:        podName,
				},
			),
			Entry(
				"when attaching to a localnet - switched - network featuring `excludeCIDR`s",
				networkAttachmentConfigParams{
					cidr:         secondaryLocalnetNetworkCIDR,
					name:         secondaryNetworkName,
					topology:     "localnet",
					excludeCIDRs: []string{secondaryLocalnetIgnoreCIDR},
					vlanID:       localnetVLANID,
				},
				podConfiguration{
					attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:        podName,
				},
			),
			Entry(
				"when attaching to a localnet - switched - network without IPAM",
				networkAttachmentConfigParams{
					name:     secondaryNetworkName,
					topology: "localnet",
					vlanID:   localnetVLANID,
				},
				podConfiguration{
					attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:        podName,
				},
			),
			Entry(
				"when attaching to a localnet - switched - network with an IPv6 subnet",
				networkAttachmentConfigParams{
					cidr:     secondaryIPv6CIDR,
					name:     secondaryNetworkName,
					topology: "localnet",
					vlanID:   localnetVLANID,
				},
				podConfiguration{
					attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:        podName,
				},
			),
			Entry(
				"when attaching to an L2 - switched - network with a dual stack configuration",
				networkAttachmentConfigParams{
					cidr:     strings.Join([]string{secondaryLocalnetNetworkCIDR, secondaryIPv6CIDR}, ","),
					name:     secondaryNetworkName,
					topology: "localnet",
					vlanID:   localnetVLANID,
				},
				podConfiguration{
					attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:        podName,
				},
			),
		)

		const (
			clientPodName          = "client-pod"
			clientIPOffset         = 100 // offset for IP generation from a given subnet for client pod
			serverIPOffset         = 102
			externalRouterIPOffset = 55
			port                   = 9000
		)

		DescribeTable("attached to a localnet network mapped to external primary interface bridge", //nolint:lll
			func(netConfigParams networkAttachmentConfigParams, clientPodConfig, serverPodConfig podConfiguration, isCollocatedPods bool) {
				By("Get two schedulable nodes and ensure client and server are located on distinct Nodes")
				nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.Background(), f.ClientSet, 2)
				framework.ExpectNoError(err, "2 schedulable nodes are required")
				Expect(len(nodes.Items)).To(BeNumerically(">", 1), "cluster should have at least 2 nodes")
				if isCollocatedPods {
					clientPodConfig.nodeSelector = map[string]string{nodeHostnameKey: nodes.Items[0].GetName()}
					serverPodConfig.nodeSelector = map[string]string{nodeHostnameKey: nodes.Items[0].GetName()}
				} else {
					clientPodConfig.nodeSelector = map[string]string{nodeHostnameKey: nodes.Items[0].GetName()}
					serverPodConfig.nodeSelector = map[string]string{nodeHostnameKey: nodes.Items[1].GetName()}
				}

				netConfigParams.namespace = f.Namespace.Name
				netConfig := newNetworkAttachmentConfig(netConfigParams)
				if clientPodConfig.namespace == "" {
					clientPodConfig.namespace = f.Namespace.Name
				}
				if serverPodConfig.namespace == "" {
					serverPodConfig.namespace = f.Namespace.Name
				}

				By("setting up the localnet underlay")
				Expect(infraprovider.SetupUnderlay(providerCtx, f, infraapi.Underlay{
					BridgeName:         deploymentconfig.Get().ExternalBridgeName(),
					LogicalNetworkName: netConfig.networkName,
				})).To(Succeed())

				nad := generateNAD(netConfig, f.ClientSet)
				By(fmt.Sprintf("creating the attachment configuration: %v\n", nad))
				_, err = nadClient.NetworkAttachmentDefinitions(f.Namespace.Name).Create(
					context.Background(),
					nad,
					metav1.CreateOptions{},
				)
				Expect(err).NotTo(HaveOccurred())

				if len(serverPodConfig.attachments) > 0 && serverPodConfig.ipRequestFromSubnet != "" {
					By("finalizing the server pod IP configuration")
					err = addIPRequestToPodConfig(cs, &serverPodConfig, serverIPOffset)
					Expect(err).NotTo(HaveOccurred())
				}

				if len(clientPodConfig.attachments) > 0 && clientPodConfig.ipRequestFromSubnet != "" {
					By("finalizing the client pod IP configuration")
					err = addIPRequestToPodConfig(cs, &clientPodConfig, clientIPOffset)
					Expect(err).NotTo(HaveOccurred())
				}

				By("instantiating the server pod")
				serverPod := kickstartPod(cs, serverPodConfig)

				By("instantiating the client pod")
				clientPod := kickstartPod(cs, clientPodConfig)

				serverInterface, err := getNetworkInterfaceName(serverPod, serverPodConfig, netConfig.name)
				Expect(err).NotTo(HaveOccurred(), "failed to extract server pod interface name")

				clientInterface, err := getNetworkInterfaceName(clientPod, clientPodConfig, netConfig.name)
				Expect(err).NotTo(HaveOccurred(), "failed to extract client pod interface name")

				// Add external container that will act as external router for the localnet
				if (clientPodConfig.usesExternalRouter && len(clientPodConfig.attachments) > 0) ||
					(serverPodConfig.usesExternalRouter && len(serverPodConfig.attachments) > 0) {
					By("instantiating the external container")
					externalRouterName, err := createExternalRouter(providerCtx, cs, f, netConfig.vlanID, externalRouterIPOffset)
					Expect(err).NotTo(HaveOccurred())

					By("injecting routes via the external container")
					err = injectStaticRoutesViaExternalContainer(f, cs, clientPodConfig, serverPodConfig,
						clientInterface, serverInterface, externalRouterName, netConfig.vlanID)
					Expect(err).NotTo(HaveOccurred())
				}

				// Check that the client pod can reach the server pod on the server localnet interface
				var serverIPs []string
				if len(serverPodConfig.attachments) > 0 {
					serverIPs, err = podIPsForAttachment(cs, serverPod.Namespace, serverPod.Name, netConfig.name)
				} else {
					serverIPs, err = podIPsFromStatus(cs, serverPodConfig.namespace, serverPodConfig.name)
				}
				Expect(err).NotTo(HaveOccurred())

				for _, serverIP := range serverIPs {
					curlArgs := []string{}
					pingArgs := []string{}
					if len(clientPodConfig.attachments) > 0 {
						// When the client is attached to a localnet, send probes from the localnet interface
						curlArgs = []string{"--interface", clientInterface}
						pingArgs = []string{"-I", clientInterface}
					}

					By(fmt.Sprintf("asserting the *client* can contact the server pod exposed endpoint: %q on port %d", serverIP, port))
					Eventually(func() error {
						return reachServerPodFromClient(cs, serverPodConfig, clientPodConfig, serverIP, port, curlArgs...)
					}, 2*time.Minute, 6*time.Second).Should(Succeed())

					By(fmt.Sprintf("asserting the *client* can ping the server pod exposed endpoint: %q", serverIP))
					Eventually(func() error {
						return pingServerPodFromClient(cs, serverPodConfig, clientPodConfig, serverIP, pingArgs...)
					}, 2*time.Minute, 6*time.Second).Should(Succeed())
				}
			},

			// The first setup we test is that of a localnet that uses IPs in the host subnet.
			// Pod A is a pod in the default network, podL is a pod in a localnet.
			//
			//                         +-----------------------+
			//                         |     Kubernetes Node   |
			//                         |       ovn-worker2     |
			//                         |                       |
			// podA (10.244.1.10/24)---+-------[ br-int ]------+--- podL (172.18.0.4/16, net1)
			// (default network)       |           |           |   (localnet)
			//                         |       [ br-ex ]       |
			//                         |        172.18.0.2     |
			//                         |           |           |
			//                         +-----------|-----------+
			//                                     |
			//                               host network
			//                               172.18.0.0/16
			//                                     |
			//              +------------------------------------------+
			//              |   other hosts / routers / services       |
			//              |   (directly reachable in 172.18.0.0/16)  |
			//              +------------------------------------------+
			//
			// We test podA when it sits on top of the overlay network, as depicted above, and
			// when it is host-networked.
			Entry(
				// default network -> localnet, different nodes
				"can be reached by a client pod in the default network on a different node, when the localnet uses an IP in the host subnet",
				networkAttachmentConfigParams{
					name:     secondaryNetworkName,
					topology: "localnet",
				},
				podConfiguration{ // client on default network
					name:         clientPodName,
					isPrivileged: true,
				},
				podConfiguration{ // server attached to localnet secondary network
					attachments: []nadapi.NetworkSelectionElement{{
						Name: secondaryNetworkName,
					}},
					name:                podName,
					containerCmd:        httpServerContainerCmd(port),
					ipRequestFromSubnet: fromHostSubnet, // override attachments with an IPRequest from host subnet
				},
				false, // scheduled on distinct Nodes
			),
			Entry(
				// default network -> localnet, same node
				"can be reached by a client pod in the default network on the same node, when the localnet uses an IP in the host subnet",
				networkAttachmentConfigParams{
					name:     secondaryNetworkName,
					topology: "localnet",
				},
				podConfiguration{ // client on default network
					name:         clientPodName,
					isPrivileged: true,
				},
				podConfiguration{ // server attached to localnet secondary network
					attachments: []nadapi.NetworkSelectionElement{{
						Name: secondaryNetworkName,
					}},
					name:                podName,
					containerCmd:        httpServerContainerCmd(port),
					ipRequestFromSubnet: fromHostSubnet,
				},
				true, // collocated on same Node
			),
			Entry(
				// localnet -> host network, different nodes
				"can reach a host-networked pod on a different node, when the localnet uses an IP in the host subnet",
				networkAttachmentConfigParams{
					name:     secondaryNetworkName,
					topology: "localnet",
				},
				podConfiguration{ // client on localnet
					attachments: []nadapi.NetworkSelectionElement{{
						Name: secondaryNetworkName,
					}},
					name:                clientPodName,
					isPrivileged:        true,
					ipRequestFromSubnet: fromHostSubnet,
				},
				podConfiguration{ // server on default network, pod is host-networked
					name:         podName,
					containerCmd: httpServerContainerCmd(port),
					hostNetwork:  true,
				},
				false, // not collocated on the same node
			),
			Entry(
				// localnet -> host network, same node
				"can reach a host-networked pod on the same node, when the localnet uses an IP in the host subnet",
				networkAttachmentConfigParams{
					name:     secondaryNetworkName,
					topology: "localnet",
				},
				podConfiguration{ // client on localnet
					attachments: []nadapi.NetworkSelectionElement{{
						Name: secondaryNetworkName,
					}},
					name:                clientPodName,
					isPrivileged:        true,
					ipRequestFromSubnet: fromHostSubnet,
				},
				podConfiguration{ // server on default network, pod is host-networked
					name:         podName,
					containerCmd: httpServerContainerCmd(port),
					hostNetwork:  true,
				},
				true, // collocated on the same node
			),
			Entry(
				// host network -> localnet, different nodes
				"can be reached by a host-networked pod on a different node, when the localnet uses an IP in the host subnet",
				networkAttachmentConfigParams{
					name:     secondaryNetworkName,
					topology: "localnet",
				},
				podConfiguration{ // client is host-networked
					name:         clientPodName,
					hostNetwork:  true,
					isPrivileged: true,
				},
				podConfiguration{ // server on localnet
					attachments: []nadapi.NetworkSelectionElement{{
						Name: secondaryNetworkName,
					}},
					containerCmd:        httpServerContainerCmd(port),
					name:                podName,
					ipRequestFromSubnet: fromHostSubnet,
				},
				false, // collocated on different nodes
			),
			Entry(
				// host network -> localnet, same node
				"can be reached by a host-networked pod on the same node, when the localnet uses an IP in the host subnet",
				networkAttachmentConfigParams{
					name:     secondaryNetworkName,
					topology: "localnet",
				},
				podConfiguration{ // client is host-networked
					name:         clientPodName,
					hostNetwork:  true,
					isPrivileged: true,
				},
				podConfiguration{ // server on localnet
					attachments: []nadapi.NetworkSelectionElement{{
						Name: secondaryNetworkName,
					}},
					containerCmd:        httpServerContainerCmd(port),
					name:                podName,
					ipRequestFromSubnet: fromHostSubnet,
				},
				true, // collocated on the same node
			),
			// The second setup we test configures: a localnet that uses a VLAN, an external router
			// that acts as gateway for the localnet pod for traffic destined to the host network.
			// We implement the external router as an external container, where we create a VLAN interface
			// on top of eth0 and assign to it an IP in the subnet in use by the localnet.
			// Pod A is a pod in the default network, podL is a pod in a localnet.
			//
			//                            +-----------------------+
			//                            |     Kubernetes Node   |
			//                            |       ovn-worker2     |
			//                            |                       |
			//    podA (10.244.1.10/24)---+-------[ br-int ]------+--- podL (172.20.0.4/16, net1)
			//    (default net)           |           |           |     (localnet, VLAN 10)
			//                            |       [ br-ex ]       |
			//                            |        172.18.0.2     |
			//                            +-----------|-----------+
			//                                        |
			//                                  host network
			//                                  172.18.0.0/16
			//                                        |
			//                           +------------------------+
			//                           |     external router    |
			//                           |                        |
			//                           |  eth0: 172.18.x.x      |
			//                           |  eth0.10: 172.20.0.55  |
			//                           +------------------------+
			//
			// Packet path (ping podA → podL):
			//   podA (10.244.1.10)
			//     → br-int
			//       → br-ex (172.18.0.2, SNAT to node IP)
			//         → eth0 (external router, 172.18.x.x)
			//           → eth0.10 (external router, 172.20.0.55)
			//             → eth0 (external router)
			//               → br-ex (172.18.0.2)
			//                 → br-int
			//                   → podL (172.20.0.4)
			//
			// Reply traffic follows the reverse path.

			Entry(
				// default network -> localnet, different nodes
				"can be reached by a client pod in the default network on a different node, when the localnet uses a VLAN and an external router",
				networkAttachmentConfigParams{
					name:     secondaryNetworkName,
					topology: "localnet",
					vlanID:   localnetVLANID,
				},
				podConfiguration{ // client on default network
					name:         clientPodName,
					isPrivileged: true,
				},
				podConfiguration{ // server attached to localnet secondary network
					attachments: []nadapi.NetworkSelectionElement{{
						Name: secondaryNetworkName,
					}},
					name:                podName,
					containerCmd:        httpServerContainerCmd(port),
					ipRequestFromSubnet: fromExternalNetwork,
					isPrivileged:        true,
					usesExternalRouter:  true,
				},
				false, // scheduled on distinct Nodes
			),
			Entry(
				// default network -> localnet, same node
				"can be reached by a client pod in the default network on the same node, when the localnet uses a VLAN and an external router",
				networkAttachmentConfigParams{
					name:     secondaryNetworkName,
					topology: "localnet",
					vlanID:   localnetVLANID,
				},
				podConfiguration{ // client on default network
					name:         clientPodName,
					isPrivileged: true,
				},
				podConfiguration{ // server attached to localnet secondary network
					attachments: []nadapi.NetworkSelectionElement{{
						Name: secondaryNetworkName,
					}},
					name:                podName,
					containerCmd:        httpServerContainerCmd(port),
					ipRequestFromSubnet: fromExternalNetwork,
					isPrivileged:        true,
					usesExternalRouter:  true,
				},
				true, // scheduled on the same node
			),
			Entry(
				// host network -> localnet, different nodes
				"can be reached by a host-networked pod on a different node, when the localnet uses a VLAN and an external router",
				networkAttachmentConfigParams{
					name:     secondaryNetworkName,
					topology: "localnet",
					vlanID:   localnetVLANID,
				},
				podConfiguration{ // client on host network
					name:         clientPodName,
					hostNetwork:  true,
					isPrivileged: true,
				},
				podConfiguration{ // server attached to localnet secondary network
					attachments: []nadapi.NetworkSelectionElement{{
						Name: secondaryNetworkName,
					}},
					name:                podName,
					containerCmd:        httpServerContainerCmd(port),
					ipRequestFromSubnet: fromExternalNetwork,
					isPrivileged:        true,
					usesExternalRouter:  true,
				},
				false, // scheduled on distinct Nodes
			),
			Entry(
				// host network -> localnet, same node
				"can be reached by a host-networked pod on the same node, when the localnet uses a VLAN and an external router",
				networkAttachmentConfigParams{
					name:     secondaryNetworkName,
					topology: "localnet",
					vlanID:   localnetVLANID,
				},
				podConfiguration{ // client on host network
					name:         clientPodName,
					hostNetwork:  true,
					isPrivileged: true,
				},
				podConfiguration{ // server attached to localnet secondary network
					attachments: []nadapi.NetworkSelectionElement{{
						Name: secondaryNetworkName,
					}},
					name:                podName,
					containerCmd:        httpServerContainerCmd(port),
					ipRequestFromSubnet: fromExternalNetwork,
					isPrivileged:        true,
					usesExternalRouter:  true,
				},
				true, // scheduled on the same node
			),
			Entry(
				// localnet -> host network, different nodes
				"can reach a host-network pod on a different node, when the localnet uses a VLAN and an external router",
				networkAttachmentConfigParams{
					name:     secondaryNetworkName,
					topology: "localnet",
					vlanID:   localnetVLANID,
				},
				podConfiguration{ // client attached to localnet secondary network
					attachments: []nadapi.NetworkSelectionElement{{
						Name: secondaryNetworkName,
					}},
					name:                clientPodName,
					ipRequestFromSubnet: fromExternalNetwork,
					isPrivileged:        true,
					usesExternalRouter:  true,
				},
				podConfiguration{ // server on host network
					name:         podName,
					containerCmd: httpServerContainerCmd(port),
					hostNetwork:  true,
					isPrivileged: true,
				},
				false, // scheduled on distinct Nodes
			),
			Entry(
				// localnet -> host network, same node
				"can reach a host-network pod on the same node, when the localnet uses a VLAN and an external router",
				networkAttachmentConfigParams{
					name:     secondaryNetworkName,
					topology: "localnet",
					vlanID:   localnetVLANID,
				},
				podConfiguration{ // client attached to localnet secondary network
					attachments: []nadapi.NetworkSelectionElement{{
						Name: secondaryNetworkName,
					}},
					name:                clientPodName,
					ipRequestFromSubnet: fromExternalNetwork,
					isPrivileged:        true,
					usesExternalRouter:  true,
				},
				podConfiguration{ // server on host network
					name:         podName,
					containerCmd: httpServerContainerCmd(port),
					hostNetwork:  true,
					isPrivileged: true,
				},
				true, // scheduled on the same node
			),
		)
	})

	Context("multiple pods connected to the same OVN-K secondary network", func() {
		const (
			clientPodName   = "client-pod"
			nodeHostnameKey = "kubernetes.io/hostname"
			port            = 9000
			clientIP        = "192.168.200.10/24"
			staticServerIP  = "192.168.200.20/24"
		)

		It("eventually configures pods that were added to an already existing network before the nad", func() {
			netConfig := newNetworkAttachmentConfig(networkAttachmentConfigParams{
				name:      secondaryNetworkName,
				namespace: f.Namespace.Name,
				topology:  "layer2",
				cidr:      secondaryNetworkCIDR,
			})

			By("creating the attachment configuration")
			_, err := nadClient.NetworkAttachmentDefinitions(f.Namespace.Name).Create(
				context.Background(),
				generateNAD(netConfig, f.ClientSet),
				metav1.CreateOptions{},
			)
			Expect(err).NotTo(HaveOccurred())

			By("creating a new namespace")

			createdNamespace, err := f.CreateNamespace(context.Background(), "multi-nad-namespace", nil)
			Expect(err).NotTo(HaveOccurred())

			By("creating the pod in the new namespace")
			pod, err := cs.CoreV1().Pods(createdNamespace.Name).Create(
				context.Background(),
				generatePodSpec(podConfiguration{
					attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:        clientPodName,
				}),
				metav1.CreateOptions{},
			)
			Expect(err).NotTo(HaveOccurred())
			Expect(pod).NotTo(BeNil())

			By("asserting the pod is trying to start")
			Eventually(func() bool {
				updatedPod, err := cs.CoreV1().Pods(createdNamespace.Name).Get(context.Background(), pod.GetName(), metav1.GetOptions{})
				if err != nil {
					return false
				}
				if updatedPod.Status.Phase == v1.PodPending {
					for _, containerStatus := range updatedPod.Status.ContainerStatuses {
						// ensure that the container is trying to start
						if containerStatus.State.Waiting != nil {
							return true
						}
					}
				}
				return false
			}, 2*time.Minute, 6*time.Second).Should(BeTrue())

			By("creating the attachment configuration in the new namespace")
			netConfig.namespace = createdNamespace.Name
			_, err = nadClient.NetworkAttachmentDefinitions(createdNamespace.Name).Create(
				context.Background(),
				generateNAD(netConfig, f.ClientSet),
				metav1.CreateOptions{},
			)
			Expect(err).NotTo(HaveOccurred())

			By("asserting the pod reaches the `Ready` state")
			Eventually(func() v1.PodPhase {
				updatedPod, err := cs.CoreV1().Pods(createdNamespace.Name).Get(context.Background(), pod.GetName(), metav1.GetOptions{})
				if err != nil {
					return v1.PodFailed
				}
				return updatedPod.Status.Phase
			}, 2*time.Minute, 6*time.Second).Should(Equal(v1.PodRunning))

		})
		DescribeTable(
			"can communicate over the secondary network",
			func(netConfigParams networkAttachmentConfigParams, clientPodConfig podConfiguration, serverPodConfig podConfiguration) {
				netConfig := newNetworkAttachmentConfig(netConfigParams)

				netConfig.namespace = f.Namespace.Name
				clientPodConfig.namespace = f.Namespace.Name
				serverPodConfig.namespace = f.Namespace.Name

				if netConfig.topology == "localnet" {
					Expect(infraprovider.SetupUnderlay(providerCtx, f, infraapi.Underlay{
						LogicalNetworkName: netConfig.networkName,
						VlanID:             netConfig.vlanID,
					})).To(Succeed())
				}

				By("creating the attachment configuration")
				_, err := nadClient.NetworkAttachmentDefinitions(f.Namespace.Name).Create(
					context.Background(),
					generateNetAttachDef(netConfig.namespace, netConfig.name, generateNADSpec(netConfig)),
					metav1.CreateOptions{},
				)
				Expect(err).NotTo(HaveOccurred())

				By("Get two schedulable nodes and schedule client and server to be on distinct Nodes")
				nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.Background(), f.ClientSet, 2)
				framework.ExpectNoError(err, "2 schedulable nodes are required")
				Expect(len(nodes.Items)).To(BeNumerically(">", 1), "cluster should have at least 2 nodes")
				clientPodConfig.nodeSelector = map[string]string{nodeHostnameKey: nodes.Items[0].GetName()}
				serverPodConfig.nodeSelector = map[string]string{nodeHostnameKey: nodes.Items[1].GetName()}

				By("instantiating the server pod")
				serverPod, err := cs.CoreV1().Pods(serverPodConfig.namespace).Create(
					context.Background(),
					generatePodSpec(serverPodConfig),
					metav1.CreateOptions{},
				)
				Expect(err).NotTo(HaveOccurred())
				Expect(serverPod).NotTo(BeNil())

				By("asserting the server pod reaches the `Ready` state")
				Eventually(func() v1.PodPhase {
					updatedPod, err := cs.CoreV1().Pods(f.Namespace.Name).Get(context.Background(), serverPod.GetName(), metav1.GetOptions{})
					if err != nil {
						return v1.PodFailed
					}
					return updatedPod.Status.Phase
				}, 2*time.Minute, 6*time.Second).Should(Equal(v1.PodRunning))

				By("instantiating the *client* pod")
				clientPod, err := cs.CoreV1().Pods(clientPodConfig.namespace).Create(
					context.Background(),
					generatePodSpec(clientPodConfig),
					metav1.CreateOptions{},
				)
				Expect(err).NotTo(HaveOccurred())

				By("asserting the client pod reaches the `Ready` state")
				Eventually(func() v1.PodPhase {
					updatedPod, err := cs.CoreV1().Pods(f.Namespace.Name).Get(context.Background(), clientPod.GetName(), metav1.GetOptions{})
					if err != nil {
						return v1.PodFailed
					}
					return updatedPod.Status.Phase
				}, 2*time.Minute, 6*time.Second).Should(Equal(v1.PodRunning))

				serverIP := ""
				if netConfig.cidr == "" {
					By("configuring static IP addresses in the pods")
					const (
						clientIP       = "192.168.200.10/24"
						staticServerIP = "192.168.200.20/24"
					)
					if !areStaticIPsConfiguredViaCNI(clientPodConfig) {
						Expect(configurePodStaticIP(clientPodConfig.namespace, clientPodName, clientIP)).To(Succeed())
					}
					if !areStaticIPsConfiguredViaCNI(serverPodConfig) {
						Expect(configurePodStaticIP(serverPodConfig.namespace, serverPod.GetName(), staticServerIP)).To(Succeed())
					}
					serverIP = strings.ReplaceAll(staticServerIP, "/24", "")
				}

				for i, cidr := range strings.Split(netConfig.cidr, ",") {
					if cidr != "" {
						By("asserting the server pod has an IP from the configured range")
						serverIP, err = podIPForAttachment(cs, f.Namespace.Name, serverPod.GetName(), netConfig.name, i)
						Expect(err).NotTo(HaveOccurred())
						By(fmt.Sprintf("asserting the server pod IP %v is from the configured range %v/%v", serverIP, cidr, netPrefixLengthPerNode))
						subnet, err := getNetCIDRSubnet(cidr)
						Expect(err).NotTo(HaveOccurred())
						Expect(inRange(subnet, serverIP)).To(Succeed())
					}

					By("asserting the *client* pod can contact the server pod exposed endpoint")
					Eventually(func() error {
						return reachServerPodFromClient(cs, serverPodConfig, clientPodConfig, serverIP, port)
					}, 2*time.Minute, 6*time.Second).Should(Succeed())
				}
			},
			Entry(
				"can communicate over an L2 secondary network when the pods are scheduled in different nodes",
				networkAttachmentConfigParams{
					name:     secondaryNetworkName,
					topology: "layer2",
					cidr:     secondaryNetworkCIDR,
				},
				podConfiguration{
					attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:        clientPodName,
				},
				podConfiguration{
					attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:         podName,
					containerCmd: httpServerContainerCmd(port),
				},
			),
			Entry(
				"can communicate over an L2 - switched - secondary network with `excludeCIDR`s",
				networkAttachmentConfigParams{
					name:         secondaryNetworkName,
					topology:     "layer2",
					cidr:         secondaryNetworkCIDR,
					excludeCIDRs: []string{secondaryFlatL2IgnoreCIDR},
				},
				podConfiguration{
					attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:        clientPodName,
				},
				podConfiguration{
					attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:         podName,
					containerCmd: httpServerContainerCmd(port),
				},
			),
			Entry(
				"can communicate over an L3 - routed - secondary network",
				networkAttachmentConfigParams{
					name:     secondaryNetworkName,
					topology: "layer3",
					cidr:     netCIDR(secondaryNetworkCIDR, netPrefixLengthPerNode),
				},
				podConfiguration{
					attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:        clientPodName,
				},
				podConfiguration{
					attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:         podName,
					containerCmd: httpServerContainerCmd(port),
				},
			),
			Entry(
				"can communicate over an L3 - routed - secondary network with IPv6 subnet",
				networkAttachmentConfigParams{
					name:     secondaryNetworkName,
					topology: "layer3",
					cidr:     netCIDR(secondaryIPv6CIDR, netPrefixLengthIPv6PerNode),
				},
				podConfiguration{
					attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:        clientPodName,
				},
				podConfiguration{
					attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:         podName,
					containerCmd: httpServerContainerCmd(port),
				},
			),
			Entry(
				"can communicate over an L3 - routed - secondary network with a dual stack configuration",
				networkAttachmentConfigParams{
					name:     secondaryNetworkName,
					topology: "layer3",
					cidr:     strings.Join([]string{netCIDR(secondaryNetworkCIDR, netPrefixLengthPerNode), netCIDR(secondaryIPv6CIDR, netPrefixLengthIPv6PerNode)}, ","),
				},
				podConfiguration{
					attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:        clientPodName,
				},
				podConfiguration{
					attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:         podName,
					containerCmd: httpServerContainerCmd(port),
				},
			),
			Entry(
				"can communicate over an L2 - switched - secondary network without IPAM",
				networkAttachmentConfigParams{
					name:     secondaryNetworkName,
					topology: "layer2",
				},
				podConfiguration{
					attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:         clientPodName,
					isPrivileged: true,
				},
				podConfiguration{
					attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:         podName,
					containerCmd: httpServerContainerCmd(port),
					isPrivileged: true,
				},
			),
			Entry(
				"can communicate over an L2 secondary network without IPAM, with static IPs configured via network selection elements",
				networkAttachmentConfigParams{
					name:     secondaryNetworkName,
					topology: "layer2",
				},
				podConfiguration{
					attachments: []nadapi.NetworkSelectionElement{{
						Name:      secondaryNetworkName,
						IPRequest: []string{clientIP},
					}},
					name: clientPodName,
				},
				podConfiguration{
					attachments: []nadapi.NetworkSelectionElement{{
						Name:      secondaryNetworkName,
						IPRequest: []string{staticServerIP},
					}},
					name:         podName,
					containerCmd: httpServerContainerCmd(port),
				},
			),
			Entry(
				"can communicate over an L2 secondary network with an IPv6 subnet when pods are scheduled in different nodes",
				networkAttachmentConfigParams{
					name:     secondaryNetworkName,
					topology: "layer2",
					cidr:     secondaryIPv6CIDR,
				},
				podConfiguration{
					attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:        clientPodName,
				},
				podConfiguration{
					attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:         podName,
					containerCmd: httpServerContainerCmd(port),
				},
			),
			Entry(
				"can communicate over an L2 secondary network with a dual stack configuration",
				networkAttachmentConfigParams{
					name:     secondaryNetworkName,
					topology: "layer2",
					cidr:     strings.Join([]string{secondaryFlatL2NetworkCIDR, secondaryIPv6CIDR}, ","),
				},
				podConfiguration{
					attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:        clientPodName,
				},
				podConfiguration{
					attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:         podName,
					containerCmd: httpServerContainerCmd(port),
				},
			),
			Entry(
				"can communicate over a localnet secondary network when the pods are scheduled on different nodes",
				networkAttachmentConfigParams{
					name:     secondaryNetworkName,
					topology: "localnet",
					cidr:     secondaryLocalnetNetworkCIDR,
					vlanID:   localnetVLANID,
				},
				podConfiguration{
					attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:        clientPodName,
				},
				podConfiguration{
					attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:         podName,
					containerCmd: httpServerContainerCmd(port),
				},
			),
			Entry(
				"can communicate over a localnet secondary network without IPAM when the pods are scheduled on different nodes",
				networkAttachmentConfigParams{
					name:     secondaryNetworkName,
					topology: "localnet",
					vlanID:   localnetVLANID,
				},
				podConfiguration{
					attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:         clientPodName,
					isPrivileged: true,
				},
				podConfiguration{
					attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:         podName,
					containerCmd: httpServerContainerCmd(port),
					isPrivileged: true,
				},
			),
			Entry(
				"can communicate over a localnet secondary network without IPAM when the pods are scheduled on different nodes, with static IPs configured via network selection elements",
				networkAttachmentConfigParams{
					name:     secondaryNetworkName,
					topology: "localnet",
					vlanID:   localnetVLANID,
				},
				podConfiguration{
					attachments: []nadapi.NetworkSelectionElement{{
						Name:      secondaryNetworkName,
						IPRequest: []string{clientIP},
					}},
					name: clientPodName,
				},
				podConfiguration{
					attachments: []nadapi.NetworkSelectionElement{{
						Name:      secondaryNetworkName,
						IPRequest: []string{staticServerIP},
					}},
					name:         podName,
					containerCmd: httpServerContainerCmd(port),
				},
			),
			Entry(
				"can communicate over a localnet secondary network with an IPv6 subnet when pods are scheduled on different nodes",
				networkAttachmentConfigParams{
					name:     secondaryNetworkName,
					topology: "localnet",
					cidr:     secondaryIPv6CIDR,
					vlanID:   localnetVLANID,
				},
				podConfiguration{
					attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:        clientPodName,
				},
				podConfiguration{
					attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:         podName,
					containerCmd: httpServerContainerCmd(port),
				},
			),
			Entry(
				"can communicate over a localnet secondary network with a dual stack configuration when pods are scheduled on different nodes",
				networkAttachmentConfigParams{
					name:     secondaryNetworkName,
					topology: "localnet",
					cidr:     strings.Join([]string{secondaryLocalnetNetworkCIDR, secondaryIPv6CIDR}, ","),
					vlanID:   localnetVLANID,
				},
				podConfiguration{
					attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:        clientPodName,
				},
				podConfiguration{
					attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:         podName,
					containerCmd: httpServerContainerCmd(port),
				},
			),
		)

		Context("localnet OVN-K secondary network", func() {
			const (
				clientPodName              = "client-pod"
				nodeHostnameKey            = "kubernetes.io/hostname"
				servicePort         uint16 = 9000
				dockerNetworkName          = "underlay"
				underlayServiceIP          = "60.128.0.1"
				expectedOriginalMTU        = 1200
			)

			var (
				netConfig networkAttachmentConfig
			)

			underlayIP := underlayServiceIP + "/24"
			Context("with a service running on the underlay", func() {
				BeforeEach(func() {
					netConfig = newNetworkAttachmentConfig(
						networkAttachmentConfigParams{
							name:         secondaryNetworkName,
							namespace:    f.Namespace.Name,
							vlanID:       localnetVLANID,
							topology:     "localnet",
							cidr:         secondaryLocalnetNetworkCIDR,
							excludeCIDRs: []string{underlayServiceIP + "/32"},
							mtu:          expectedOriginalMTU,
						})

					By("setting up the localnet underlay")
					Expect(infraprovider.SetupUnderlay(providerCtx, f, infraapi.Underlay{
						LogicalNetworkName: netConfig.networkName,
						VlanID:             netConfig.vlanID,
					})).To(Succeed())
				})

				BeforeEach(func() {
					By("starting a service, connected to the underlay")
					providerCtx = infraprovider.Get().NewTestContext()

					underlayNetwork, err := infraprovider.Get().GetNetwork(dockerNetworkName)
					Expect(err).NotTo(HaveOccurred(), "must get underlay network")
					externalContainerName := f.Namespace.Name + "-web-server"
					serviceContainerSpec := infraapi.ExternalContainer{
						Name:       externalContainerName,
						Image:      images.AgnHost(),
						Network:    underlayNetwork,
						Entrypoint: "bash",
						CmdArgs:    []string{"-c", fmt.Sprintf("ip a add %s/24 dev eth0 && ./agnhost netexec --http-port=%d", underlayServiceIP, servicePort)},
						ExtPort:    servicePort,
					}
					_, err = providerCtx.CreateExternalContainer(serviceContainerSpec)
					Expect(err).NotTo(HaveOccurred(), "must create external container 1")
				})

				BeforeEach(func() {
					By("creating the attachment configuration")
					_, err := nadClient.NetworkAttachmentDefinitions(netConfig.namespace).Create(
						context.Background(),
						generateNAD(netConfig, f.ClientSet),
						metav1.CreateOptions{},
					)
					Expect(err).NotTo(HaveOccurred())
				})

				It("correctly sets the MTU on the pod", func() {
					Eventually(func() error {
						clientPodConfig := podConfiguration{
							name:        clientPodName + randStr(10),
							namespace:   f.Namespace.Name,
							attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						}
						kickstartPod(cs, clientPodConfig)
						mtu, err := getSecondaryInterfaceMTU(clientPodConfig)
						if err != nil {
							return fmt.Errorf("failed to get MTU: %w", err)
						}

						if mtu != expectedOriginalMTU {
							return fmt.Errorf("pod MTU is %d, but expected %d", mtu, expectedOriginalMTU)
						}
						return nil
					}).Should(Succeed(), "pod MTU should be properly configured")
				})

				It("can communicate over a localnet secondary network from pod to the underlay service", func() {
					clientPodConfig := podConfiguration{
						name:        clientPodName,
						namespace:   f.Namespace.Name,
						attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					}
					kickstartPod(cs, clientPodConfig)

					By("asserting the *client* pod can contact the underlay service")
					Expect(connectToServer(clientPodConfig, underlayServiceIP, servicePort)).To(Succeed())

				})

				Context("and networkAttachmentDefinition is modified", func() {
					const (
						expectedChangedMTU        = 1600
						newDesiredRange           = "60.128.0.192/28" // Desired IPs from 60.128.0.192 to 60.128.0.207
						excludedSubnetLowerRange1 = "60.128.0.0/25"   // Excludes IPs from 60.128.0.0 to 60.128.0.127
						excludedSubnetLowerRange2 = "60.128.0.128/26" // Excludes IPs from 60.128.0.128 to 60.128.0.191
						excludedSubnetUpperRange1 = "60.128.0.208/28" // Excludes IPs from 60.128.0.208 to 60.128.0.223
						excludedSubnetUpperRange2 = "60.128.0.224/27" // Excludes IPs from 60.128.0.224 to 60.128.0.255
						newLocalnetVLANID         = 30
					)
					BeforeEach(func() {
						By("setting new MTU")
						netConfig.mtu = expectedChangedMTU
						By("setting new subnets to leave a smaller range")
						netConfig.excludeCIDRs = []string{excludedSubnetLowerRange1, excludedSubnetLowerRange2, excludedSubnetUpperRange1, excludedSubnetUpperRange2}
						By("setting new VLAN-ID")
						netConfig.vlanID = newLocalnetVLANID
						p := []byte(fmt.Sprintf(`[{"op":"replace","path":"/spec/config","value":%q}]`, generateNADSpec(netConfig)))
						Expect(patchNADSpec(nadClient, netConfig.name, netConfig.namespace, p)).To(Succeed())
					})

					It("sets the new MTU on the pod after NetworkAttachmentDefinition reconcile", func() {
						Eventually(func() error {
							clientPodConfig := podConfiguration{
								name:        clientPodName + randStr(10),
								namespace:   f.Namespace.Name,
								attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
							}
							kickstartPod(cs, clientPodConfig)
							mtu, err := getSecondaryInterfaceMTU(clientPodConfig)
							if err != nil {
								return fmt.Errorf("failed to get MTU: %w", err)
							}
							if mtu != expectedChangedMTU {
								err := fmt.Errorf("pod MTU is %d, but expected %d", mtu, expectedChangedMTU)
								if delErr := cs.CoreV1().Pods(clientPodConfig.namespace).Delete(context.Background(), clientPodConfig.name, metav1.DeleteOptions{}); delErr != nil {
									err = errors.Join(err, fmt.Errorf("pod delete failed: %w", delErr))
								}
								return err
							}
							return nil
						}).Should(Succeed(), "pod MTU should be properly configured")
					})

					It("allocates the pod's secondary interface IP in the new range after NetworkAttachmentDefinition reconcile", func() {
						By("asserting the pod's secondary interface IP is properly configured")
						Eventually(func() error {
							clientPodConfig := podConfiguration{
								name:        clientPodName + "-" + randStr(10),
								namespace:   f.Namespace.Name,
								attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
							}
							kickstartPod(cs, clientPodConfig)

							clientIP, err := podIPForAttachment(cs, clientPodConfig.namespace, clientPodConfig.name, netConfig.name, 0)
							if err != nil {
								return err
							}

							// In order to prevent the pod from interfering with the test, deleting it before retrying
							if err := inRange(newDesiredRange, clientIP); err != nil {
								if delErr := cs.CoreV1().Pods(clientPodConfig.namespace).Delete(context.Background(), clientPodConfig.name, metav1.DeleteOptions{}); delErr != nil {
									err = errors.Join(err, fmt.Errorf("pod delete failed: %w", delErr))
								}
								return err
							}
							return nil
						}).Should(Succeed(), "pod's secondary NIC is not allocated in the desired range")
					})

					It("can no longer communicate over a localnet secondary network from pod to the underlay service", func() {
						Eventually(func() error {
							clientPodConfig := podConfiguration{
								name:        clientPodName,
								namespace:   f.Namespace.Name,
								attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
							}
							kickstartPod(cs, clientPodConfig)

							By("asserting the *client* pod can no longer contact the underlay service")
							var err error
							if err = connectToServer(clientPodConfig, underlayServiceIP, servicePort); err != nil && strings.Contains(err.Error(), "exit code 28") {
								return nil
							}
							err = fmt.Errorf("expected exit code 28 from underlay service, got err %w", err)

							if delErr := cs.CoreV1().Pods(clientPodConfig.namespace).Delete(context.Background(), clientPodConfig.name, metav1.DeleteOptions{}); delErr != nil {
								err = errors.Join(err, fmt.Errorf("pod delete failed: %w", delErr))
							}
							return err
						}).Should(Succeed(), "pod should be disconnected from underlay")
					})

					Context("and the service connected to the underlay is reconfigured to connect to the new VLAN-ID", func() {
						BeforeEach(func() {
							Expect(infraprovider.SetupUnderlay(providerCtx, f, infraapi.Underlay{
								LogicalNetworkName: netConfig.networkName,
								VlanID:             newLocalnetVLANID,
							})).To(Succeed(), "configuring the OVS bridge with new localnet vlan id")
						})

						It("can now communicate over a localnet secondary network from pod to the underlay service", func() {
							Eventually(func() error {
								clientPodConfig := podConfiguration{
									name:        clientPodName,
									namespace:   f.Namespace.Name,
									attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
								}
								kickstartPod(cs, clientPodConfig)

								By("asserting the *client* pod can contact the underlay service")
								if err := connectToServer(clientPodConfig, underlayServiceIP, servicePort); err != nil {
									if delErr := cs.CoreV1().Pods(clientPodConfig.namespace).Delete(context.Background(), clientPodConfig.name, metav1.DeleteOptions{}); delErr != nil {
										err = errors.Join(err, fmt.Errorf("pod delete failed: %w", delErr))
									}
									return err
								}
								return nil
							}).Should(Succeed(), "pod should be connected to underlay")
						})
					})
				})

				Context("with multi network policy blocking the traffic", func() {
					var clientPodConfig podConfiguration
					labels := map[string]string{"name": "access-control"}

					const policyName = "allow-egress-ipblock"

					BeforeEach(func() {
						clientPodConfig = podConfiguration{
							name:        clientPodName,
							namespace:   f.Namespace.Name,
							attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
							labels:      labels,
						}
						kickstartPod(cs, clientPodConfig)

						By("asserting the *client* pod can contact the underlay service before creating the policy")
						Expect(connectToServer(clientPodConfig, underlayServiceIP, servicePort)).To(Succeed())
					})

					AfterEach(func() {
						By("deleting the multi-network policy")
						err := mnpClient.MultiNetworkPolicies(clientPodConfig.namespace).Delete(
							context.Background(),
							policyName,
							metav1.DeleteOptions{},
						)
						Expect(err).NotTo(HaveOccurred())

						By("asserting the *client* pod can contact the underlay service after deleting the policy")
						Eventually(func() error {
							return connectToServer(clientPodConfig, underlayServiceIP, servicePort)
						}, 30*time.Second, time.Second).Should(Succeed())
					})

					It("can not communicate over a localnet secondary network from pod to the underlay service", func() {
						By("creating the multi-network policy")
						_, err := mnpClient.MultiNetworkPolicies(clientPodConfig.namespace).Create(
							context.Background(),
							multiNetEgressLimitingIPBlockPolicy(
								policyName,
								secondaryNetworkName,
								metav1.LabelSelector{
									MatchLabels: labels,
								},
								mnpapi.IPBlock{
									CIDR:   secondaryLocalnetNetworkCIDR,
									Except: []string{underlayServiceIP},
								},
							),
							metav1.CreateOptions{},
						)
						Expect(err).NotTo(HaveOccurred())

						By("asserting the *client* pod cannot contact the underlay service after creating the policy")
						Eventually(func() error {
							return connectToServer(clientPodConfig, underlayServiceIP, servicePort)
						}, 30*time.Second, time.Second).Should(MatchError(ContainSubstring("exit code 28")))
					})
				})

				When("a policy is provisioned", func() {
					var clientPodConfig podConfiguration
					allPodsSelector := map[string]string{}

					const (
						denyAllIngress = "deny-all-ingress"
						allowAllEgress = "allow-all-egress"
						denyAllEgress  = "deny-all-egress"
					)

					BeforeEach(func() {
						clientPodConfig = podConfiguration{
							name:        clientPodName,
							namespace:   f.Namespace.Name,
							attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
							labels:      allPodsSelector,
						}
						kickstartPod(cs, clientPodConfig)
					})

					DescribeTable("can communicate over a localnet secondary network from pod to gw", func(mnp *mnpapi.MultiNetworkPolicy) {
						By("provisioning the multi-network policy")
						_, err := mnpClient.MultiNetworkPolicies(clientPodConfig.namespace).Create(
							context.Background(),
							mnp,
							metav1.CreateOptions{},
						)
						Expect(err).NotTo(HaveOccurred())

						if mnp.Name != denyAllEgress {
							Eventually(func() error {
								return connectToServer(clientPodConfig, underlayServiceIP, servicePort)
							}, 30*time.Second, time.Second).Should(Succeed())
						} else {
							Eventually(func() error {
								return connectToServer(clientPodConfig, underlayServiceIP, servicePort)
							}, 30*time.Second, time.Second).Should(MatchError(ContainSubstring("exit code 28")))
						}

						By("deleting the multi-network policy")
						Expect(mnpClient.MultiNetworkPolicies(clientPodConfig.namespace).Delete(
							context.Background(),
							mnp.Name,
							metav1.DeleteOptions{},
						)).To(Succeed())

						By("asserting the *client* pod can contact the underlay service after deleting the policy")
						Eventually(func() error {
							return connectToServer(clientPodConfig, underlayServiceIP, servicePort)
						}, 30*time.Second, time.Second).Should(Succeed())
					},
						XEntry(
							"ingress denyall, ingress policy should have no impact on egress",
							multiNetPolicy(
								denyAllIngress,
								secondaryNetworkName,
								metav1.LabelSelector{
									MatchLabels: allPodsSelector,
								},
								[]mnpapi.MultiPolicyType{mnpapi.PolicyTypeIngress},
								nil,
								nil,
							),
						),
						Entry(
							"ingress denyall, egress allow all, ingress policy should have no impact on egress",
							multiNetPolicy(
								denyAllIngress,
								secondaryNetworkName,
								metav1.LabelSelector{
									MatchLabels: allPodsSelector,
								},
								[]mnpapi.MultiPolicyType{mnpapi.PolicyTypeIngress, mnpapi.PolicyTypeEgress},
								nil,
								[]mnpapi.MultiNetworkPolicyEgressRule{
									{},
								},
							),
						),
						Entry(
							"egress allow all",
							multiNetPolicy(
								allowAllEgress,
								secondaryNetworkName,
								metav1.LabelSelector{
									MatchLabels: allPodsSelector,
								},
								[]mnpapi.MultiPolicyType{mnpapi.PolicyTypeEgress},
								nil,
								[]mnpapi.MultiNetworkPolicyEgressRule{
									{},
								},
							),
						),
						Entry(
							"egress deny all",
							multiNetPolicy(
								denyAllEgress,
								secondaryNetworkName,
								metav1.LabelSelector{
									MatchLabels: allPodsSelector,
								},
								[]mnpapi.MultiPolicyType{mnpapi.PolicyTypeEgress},
								nil,
								nil,
							),
						),
					)
				})
			})

			Context("with a trunked configuration", func() {
				const vlanID = 20
				BeforeEach(func() {
					// we are setting up the bridge in trunked mode by not
					// specifying a particular VLAN ID on the network conf
					netConfig = newNetworkAttachmentConfig(
						networkAttachmentConfigParams{
							name:         secondaryNetworkName,
							namespace:    f.Namespace.Name,
							topology:     "localnet",
							cidr:         secondaryLocalnetNetworkCIDR,
							excludeCIDRs: []string{underlayServiceIP + "/32"},
						})

					By("setting up the localnet underlay with a trunked configuration")
					Expect(infraprovider.SetupUnderlay(providerCtx, f, infraapi.Underlay{
						LogicalNetworkName: netConfig.networkName,
						VlanID:             netConfig.vlanID,
					})).To(Succeed(), "configuring the OVS bridge")

					By("starting a service, connected to the underlay over a VLAN")
					providerCtx = infraprovider.Get().NewTestContext()

					ifName := "eth0"
					vlanName := fmt.Sprintf("%s.%d", ifName, vlanID)
					underlayNetwork, err := infraprovider.Get().GetNetwork(dockerNetworkName)
					Expect(err).NotTo(HaveOccurred(), "must get underlay network")
					externalContainerName := f.Namespace.Name + "-web-server"
					serviceContainerSpec := infraapi.ExternalContainer{
						Name:       externalContainerName,
						Image:      images.AgnHost(),
						Network:    underlayNetwork,
						Entrypoint: "bash",
						ExtPort:    servicePort,
						CmdArgs: []string{"-c", fmt.Sprintf(`
ip link add link %[1]s name %[2]s type vlan id %[3]d
ip link set dev %[2]s up
ip a add %[4]s/24 dev %[2]s
./agnhost netexec --http-port=%[5]d
`, ifName, vlanName, vlanID, underlayServiceIP, servicePort)},
					}
					_, err = providerCtx.CreateExternalContainer(serviceContainerSpec)
					Expect(err).NotTo(HaveOccurred(), "must create external container 1")

				})

				It("the same bridge mapping can be shared by a separate VLAN by using the physical network name attribute", func() {
					const otherNetworkName = "different-network"
					vlan20NetConfig := newNetworkAttachmentConfig(
						networkAttachmentConfigParams{
							name:                otherNetworkName,
							physicalNetworkName: netConfig.networkName,
							namespace:           f.Namespace.Name,
							vlanID:              vlanID,
							topology:            "localnet",
							cidr:                secondaryLocalnetNetworkCIDR,
							excludeCIDRs:        []string{underlayServiceIP + "/32"},
						})

					By("creating the attachment configuration for a separate VLAN")
					_, err := nadClient.NetworkAttachmentDefinitions(f.Namespace.Name).Create(
						context.Background(),
						generateNAD(vlan20NetConfig, f.ClientSet),
						metav1.CreateOptions{},
					)
					Expect(err).NotTo(HaveOccurred())

					clientPodConfig := podConfiguration{
						name:        clientPodName,
						namespace:   f.Namespace.Name,
						attachments: []nadapi.NetworkSelectionElement{{Name: otherNetworkName}},
					}
					kickstartPod(cs, clientPodConfig)

					By(fmt.Sprintf("asserting the *client* pod can contact the underlay service with IP %q on the separate vlan", underlayIP))
					Expect(connectToServer(clientPodConfig, underlayServiceIP, servicePort)).To(Succeed())

				})
			})
		})

		Context("with multi-network policies that", func() {
			const (
				generatedNamespaceNamePrefix = "pepe"
				blockedServerStaticIP        = "192.168.200.30"
			)
			var extraNamespace *v1.Namespace

			BeforeEach(func() {
				createdNamespace, err := cs.CoreV1().Namespaces().Create(
					context.Background(),
					&v1.Namespace{
						ObjectMeta: metav1.ObjectMeta{
							Labels:       map[string]string{"role": "trusted"},
							GenerateName: generatedNamespaceNamePrefix,
						},
					},
					metav1.CreateOptions{},
				)
				Expect(err).NotTo(HaveOccurred())
				extraNamespace = createdNamespace
			})

			AfterEach(func() {
				blockUntilEverythingIsGone := metav1.DeletePropagationForeground
				Expect(cs.CoreV1().Namespaces().Delete(
					context.Background(),
					extraNamespace.Name,
					metav1.DeleteOptions{PropagationPolicy: &blockUntilEverythingIsGone},
				)).To(Succeed())
				Eventually(func() bool {
					_, err := cs.CoreV1().Namespaces().Get(context.Background(), extraNamespace.Name, metav1.GetOptions{})
					nsPods, podCatchErr := cs.CoreV1().Pods(extraNamespace.Name).List(context.Background(), metav1.ListOptions{})
					return podCatchErr == nil && apierrors.IsNotFound(err) && len(nsPods.Items) == 0
				}, 2*time.Minute, 5*time.Second).Should(BeTrue())
			})

			DescribeTable(
				"configure traffic allow lists",
				func(netConfigParams networkAttachmentConfigParams, allowedClientPodConfig podConfiguration, blockedClientPodConfig podConfiguration, serverPodConfig podConfiguration, policy *mnpapi.MultiNetworkPolicy) {
					netConfig := newNetworkAttachmentConfig(netConfigParams)

					if netConfig.topology == "localnet" {
						By("setting up the localnet underlay")
						Expect(infraprovider.SetupUnderlay(providerCtx, f, infraapi.Underlay{
							LogicalNetworkName: netConfig.networkName,
							VlanID:             netConfig.vlanID,
						})).To(Succeed())
					}

					Expect(createNads(f, nadClient, extraNamespace, netConfig)).NotTo(HaveOccurred())

					kickstartPodInNamespace(cs, &allowedClientPodConfig, f.Namespace.Name, extraNamespace.Name)
					kickstartPodInNamespace(cs, &blockedClientPodConfig, f.Namespace.Name, extraNamespace.Name)
					kickstartPodInNamespace(cs, &serverPodConfig, f.Namespace.Name, extraNamespace.Name)

					serverIP, err := podIPForAttachment(cs, serverPodConfig.namespace, serverPodConfig.name, netConfig.name, 0)
					Expect(err).NotTo(HaveOccurred())
					if netConfig.cidr != "" {
						assertServerPodIPInRange(netConfig.cidr, serverIP, netPrefixLengthPerNode)
					}

					if doesPolicyFeatAnIPBlock(policy) {
						blockedIP, err := podIPForAttachment(cs, f.Namespace.Name, blockedClientPodConfig.name, netConfig.name, 0)
						Expect(err).NotTo(HaveOccurred())
						setBlockedClientIPInPolicyIPBlockExcludedRanges(policy, blockedIP)
					}

					Expect(createMultiNetworkPolicy(mnpClient, f.Namespace.Name, policy)).To(Succeed())

					By("asserting the *allowed-client* pod can contact the server pod exposed endpoint")
					Eventually(func() error {
						return reachServerPodFromClient(cs, serverPodConfig, allowedClientPodConfig, serverIP, port)
					}, 2*time.Minute, 6*time.Second).Should(Succeed())

					By("asserting the *blocked-client* pod **cannot** contact the server pod exposed endpoint")
					Eventually(func() error {
						return connectToServer(blockedClientPodConfig, serverIP, port)
					}, 2*time.Minute, 6*time.Second).Should(MatchError(ContainSubstring("exit code 28")))
				},
				Entry(
					"using pod selectors for a pure L2 overlay",
					networkAttachmentConfigParams{
						name:     secondaryNetworkName,
						topology: "layer2",
						cidr:     secondaryFlatL2NetworkCIDR,
					},
					podConfiguration{
						attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:        allowedClient(clientPodName),
						labels: map[string]string{
							"app":  "client",
							"role": "trusted",
						},
					},
					podConfiguration{
						attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:        blockedClient(clientPodName),
						labels:      map[string]string{"app": "client"},
					},
					podConfiguration{
						attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:         podName,
						containerCmd: httpServerContainerCmd(port),
						labels:       map[string]string{"app": "stuff-doer"},
					},
					multiNetIngressLimitingPolicy(
						secondaryNetworkName,
						metav1.LabelSelector{
							MatchLabels: map[string]string{"app": "stuff-doer"},
						},
						metav1.LabelSelector{
							MatchLabels: map[string]string{"role": "trusted"},
						},
						multiNetPolicyPort(port),
					),
				),
				Entry(
					"using pod selectors and port range for a pure L2 overlay",
					networkAttachmentConfigParams{
						name:     secondaryNetworkName,
						topology: "layer2",
						cidr:     secondaryFlatL2NetworkCIDR,
					},
					podConfiguration{
						attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:        allowedClient(clientPodName),
						labels: map[string]string{
							"app":  "client",
							"role": "trusted",
						},
					},
					podConfiguration{
						attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:        blockedClient(clientPodName),
						labels:      map[string]string{"app": "client"},
					},
					podConfiguration{
						attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:         podName,
						containerCmd: httpServerContainerCmd(port),
						labels:       map[string]string{"app": "stuff-doer"},
					},
					multiNetIngressLimitingPolicy(
						secondaryNetworkName,
						metav1.LabelSelector{
							MatchLabels: map[string]string{"app": "stuff-doer"},
						},
						metav1.LabelSelector{
							MatchLabels: map[string]string{"role": "trusted"},
						},
						// build a random range around the port we are actually trying to allow without explicitly setting it
						multiNetPolicyPortRange(port-3, port+5),
					),
				),
				Entry(
					"using pod selectors for a routed topology",
					networkAttachmentConfigParams{
						name:     secondaryNetworkName,
						topology: "layer3",
						cidr:     netCIDR(secondaryNetworkCIDR, netPrefixLengthPerNode),
					},
					podConfiguration{
						attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:        allowedClient(clientPodName),
						labels: map[string]string{
							"app":  "client",
							"role": "trusted",
						},
					},
					podConfiguration{
						attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:        blockedClient(clientPodName),
						labels:      map[string]string{"app": "client"},
					},
					podConfiguration{
						attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:         podName,
						containerCmd: httpServerContainerCmd(port),
						labels:       map[string]string{"app": "stuff-doer"},
					},
					multiNetIngressLimitingPolicy(
						secondaryNetworkName,
						metav1.LabelSelector{
							MatchLabels: map[string]string{"app": "stuff-doer"},
						},
						metav1.LabelSelector{
							MatchLabels: map[string]string{"role": "trusted"},
						},
						multiNetPolicyPort(port),
					),
				),
				Entry(
					"using pod selectors for a localnet topology",
					networkAttachmentConfigParams{
						name:     secondaryNetworkName,
						topology: "localnet",
						cidr:     secondaryLocalnetNetworkCIDR,
					},
					podConfiguration{
						attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:        allowedClient(clientPodName),
						labels: map[string]string{
							"app":  "client",
							"role": "trusted",
						},
					},
					podConfiguration{
						attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:        blockedClient(clientPodName),
						labels:      map[string]string{"app": "client"},
					},
					podConfiguration{
						attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:         podName,
						containerCmd: httpServerContainerCmd(port),
						labels:       map[string]string{"app": "stuff-doer"},
					},
					multiNetIngressLimitingPolicy(
						secondaryNetworkName,
						metav1.LabelSelector{
							MatchLabels: map[string]string{"app": "stuff-doer"},
						},
						metav1.LabelSelector{
							MatchLabels: map[string]string{"role": "trusted"},
						},
						multiNetPolicyPort(port),
					),
				),
				Entry(
					"using IPBlock for a pure L2 overlay",
					networkAttachmentConfigParams{
						name:     secondaryNetworkName,
						topology: "layer2",
						cidr:     secondaryFlatL2NetworkCIDR,
					},
					podConfiguration{
						attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:        allowedClient(clientPodName),
					},
					podConfiguration{
						attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:        blockedClient(clientPodName),
					},
					podConfiguration{
						attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:         podName,
						containerCmd: httpServerContainerCmd(port),
						labels:       map[string]string{"app": "stuff-doer"},
					},
					multiNetIngressLimitingIPBlockPolicy(
						secondaryNetworkName,
						metav1.LabelSelector{
							MatchLabels: map[string]string{"app": "stuff-doer"},
						},
						mnpapi.IPBlock{ // the test will find out the IP address of the client and put it in the `exclude` list
							CIDR: secondaryFlatL2NetworkCIDR,
						},
						port,
					),
				),
				Entry(
					"using IPBlock for a routed topology",
					networkAttachmentConfigParams{
						name:     secondaryNetworkName,
						topology: "layer3",
						cidr:     netCIDR(secondaryNetworkCIDR, netPrefixLengthPerNode),
					},
					podConfiguration{
						attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:        allowedClient(clientPodName),
					},
					podConfiguration{
						attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:        blockedClient(clientPodName),
					},
					podConfiguration{
						attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:         podName,
						containerCmd: httpServerContainerCmd(port),
						labels:       map[string]string{"app": "stuff-doer"},
					},
					multiNetIngressLimitingIPBlockPolicy(
						secondaryNetworkName,
						metav1.LabelSelector{
							MatchLabels: map[string]string{"app": "stuff-doer"},
						},
						mnpapi.IPBlock{ // the test will find out the IP address of the client and put it in the `exclude` list
							CIDR: secondaryNetworkCIDR,
						},
						port,
					),
				),
				Entry(
					"using IPBlock for a localnet topology",
					networkAttachmentConfigParams{
						name:     secondaryNetworkName,
						topology: "localnet",
						cidr:     secondaryLocalnetNetworkCIDR,
					},
					podConfiguration{
						attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:        allowedClient(clientPodName),
					},
					podConfiguration{
						attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:        blockedClient(clientPodName),
					},
					podConfiguration{
						attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:         podName,
						containerCmd: httpServerContainerCmd(port),
						labels:       map[string]string{"app": "stuff-doer"},
					},
					multiNetIngressLimitingIPBlockPolicy(
						secondaryNetworkName,
						metav1.LabelSelector{
							MatchLabels: map[string]string{"app": "stuff-doer"},
						},
						mnpapi.IPBlock{ // the test will find out the IP address of the client and put it in the `exclude` list
							CIDR: secondaryLocalnetNetworkCIDR,
						},
						port,
					),
				),
				Entry(
					"using namespace selectors for a pure L2 overlay",
					networkAttachmentConfigParams{
						name:        secondaryNetworkName,
						topology:    "layer2",
						cidr:        secondaryFlatL2NetworkCIDR,
						networkName: uniqueNadName("spans-multiple-namespaces"),
					},
					podConfiguration{
						attachments:            []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:                   allowedClient(clientPodName),
						requiresExtraNamespace: true,
					},
					podConfiguration{
						attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:        blockedClient(clientPodName),
					},
					podConfiguration{
						attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:         podName,
						containerCmd: httpServerContainerCmd(port),
						labels:       map[string]string{"app": "stuff-doer"},
					},
					multiNetIngressLimitingPolicyAllowFromNamespace(
						secondaryNetworkName,
						metav1.LabelSelector{
							MatchLabels: map[string]string{"app": "stuff-doer"},
						},
						metav1.LabelSelector{
							MatchLabels: map[string]string{"role": "trusted"},
						},
						port,
					),
				),
				Entry(
					"using namespace selectors for a routed topology",
					networkAttachmentConfigParams{
						name:        secondaryNetworkName,
						topology:    "layer3",
						cidr:        netCIDR(secondaryNetworkCIDR, netPrefixLengthPerNode),
						networkName: uniqueNadName("spans-multiple-namespaces"),
					},
					podConfiguration{
						attachments:            []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:                   allowedClient(clientPodName),
						requiresExtraNamespace: true,
					},
					podConfiguration{
						attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:        blockedClient(clientPodName),
					},
					podConfiguration{
						attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:         podName,
						containerCmd: httpServerContainerCmd(port),
						labels:       map[string]string{"app": "stuff-doer"},
					},
					multiNetIngressLimitingPolicyAllowFromNamespace(
						secondaryNetworkName,
						metav1.LabelSelector{
							MatchLabels: map[string]string{"app": "stuff-doer"},
						},
						metav1.LabelSelector{
							MatchLabels: map[string]string{"role": "trusted"},
						},
						port,
					),
				),
				Entry(
					"using namespace selectors for a localnet topology",
					networkAttachmentConfigParams{
						name:        secondaryNetworkName,
						topology:    "localnet",
						cidr:        secondaryLocalnetNetworkCIDR,
						networkName: uniqueNadName("spans-multiple-namespaces"),
					},
					podConfiguration{
						attachments:            []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:                   allowedClient(clientPodName),
						requiresExtraNamespace: true,
					},
					podConfiguration{
						attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:        blockedClient(clientPodName),
					},
					podConfiguration{
						attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:         podName,
						containerCmd: httpServerContainerCmd(port),
						labels:       map[string]string{"app": "stuff-doer"},
					},
					multiNetIngressLimitingPolicyAllowFromNamespace(
						secondaryNetworkName,
						metav1.LabelSelector{
							MatchLabels: map[string]string{"app": "stuff-doer"},
						},
						metav1.LabelSelector{
							MatchLabels: map[string]string{"role": "trusted"},
						},
						port,
					),
				),

				Entry(
					"using IPBlock for an IPAMless pure L2 overlay",
					networkAttachmentConfigParams{
						name:     secondaryNetworkName,
						topology: "layer2",
					},
					podConfiguration{
						attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName, IPRequest: []string{clientIP}}},
						name:        allowedClient(clientPodName),
					},
					podConfiguration{
						attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName, IPRequest: []string{blockedServerStaticIP + "/24"}}},
						name:        blockedClient(clientPodName),
					},
					podConfiguration{
						attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName, IPRequest: []string{staticServerIP}}},
						name:         podName,
						containerCmd: httpServerContainerCmd(port),
						labels:       map[string]string{"app": "stuff-doer"},
					},
					multiNetIngressLimitingIPBlockPolicy(
						secondaryNetworkName,
						metav1.LabelSelector{
							MatchLabels: map[string]string{"app": "stuff-doer"},
						},
						mnpapi.IPBlock{
							CIDR:   "192.168.200.0/24",
							Except: []string{blockedServerStaticIP},
						},
						port,
					),
				),
			)

			DescribeTable(
				"allow all ingress",
				func(netConfigParams networkAttachmentConfigParams, clientPodConfig podConfiguration, serverPodConfig podConfiguration, policy *mnpapi.MultiNetworkPolicy) {
					netConfig := newNetworkAttachmentConfig(netConfigParams)

					By("setting up the localnet underlay")
					Expect(infraprovider.SetupUnderlay(providerCtx, f, infraapi.Underlay{
						LogicalNetworkName: netConfig.networkName,
						VlanID:             netConfig.vlanID,
					})).To(Succeed())

					Expect(createNads(f, nadClient, extraNamespace, netConfig)).NotTo(HaveOccurred())

					kickstartPodInNamespace(cs, &clientPodConfig, f.Namespace.Name, extraNamespace.Name)
					kickstartPodInNamespace(cs, &serverPodConfig, f.Namespace.Name, extraNamespace.Name)

					serverIP, err := podIPForAttachment(cs, serverPodConfig.namespace, serverPodConfig.name, netConfig.name, 0)
					Expect(err).NotTo(HaveOccurred())
					assertServerPodIPInRange(netConfig.cidr, serverIP, netPrefixLengthPerNode)

					Expect(createMultiNetworkPolicy(mnpClient, f.Namespace.Name, policy)).To(Succeed())

					By("asserting the *client* pod can contact the server pod exposed endpoint")
					Eventually(func() error {
						return reachServerPodFromClient(cs, serverPodConfig, clientPodConfig, serverIP, port)
					}, 2*time.Minute, 6*time.Second).Should(Succeed())
				},
				Entry(
					"using ingress allow-all for a localnet topology",
					networkAttachmentConfigParams{
						name:     secondaryNetworkName,
						topology: "localnet",
						cidr:     secondaryLocalnetNetworkCIDR,
					},
					podConfiguration{
						attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:        allowedClient(clientPodName),
						labels: map[string]string{
							"app":  "client",
							"role": "trusted",
						},
					},
					podConfiguration{
						attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:         podName,
						containerCmd: httpServerContainerCmd(port),
						labels:       map[string]string{"app": "stuff-doer"},
					},
					multiNetPolicy(
						"allow-all-ingress",
						secondaryNetworkName,
						metav1.LabelSelector{},
						[]mnpapi.MultiPolicyType{mnpapi.PolicyTypeIngress},
						[]mnpapi.MultiNetworkPolicyIngressRule{
							mnpapi.MultiNetworkPolicyIngressRule{},
						},
						nil,
					),
				),
				XEntry(
					"using egress deny-all for a localnet topology",
					networkAttachmentConfigParams{
						name:     secondaryNetworkName,
						topology: "localnet",
						cidr:     secondaryLocalnetNetworkCIDR,
					},
					podConfiguration{
						attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:        allowedClient(clientPodName),
						labels: map[string]string{
							"app":  "client",
							"role": "trusted",
						},
					},
					podConfiguration{
						attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:         podName,
						containerCmd: httpServerContainerCmd(port),
						labels:       map[string]string{"app": "stuff-doer"},
					},
					multiNetPolicy(
						"deny-all-egress",
						secondaryNetworkName,
						metav1.LabelSelector{
							MatchLabels: map[string]string{"app": "stuff-doer"},
						},
						[]mnpapi.MultiPolicyType{mnpapi.PolicyTypeEgress},
						nil,
						nil,
					),
				),
				Entry(
					"using egress deny-all, ingress allow-all for a localnet topology",
					networkAttachmentConfigParams{
						name:     secondaryNetworkName,
						topology: "localnet",
						cidr:     secondaryLocalnetNetworkCIDR,
					},
					podConfiguration{
						attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:        allowedClient(clientPodName),
						labels: map[string]string{
							"app":  "client",
							"role": "trusted",
						},
					},
					podConfiguration{
						attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:         podName,
						containerCmd: httpServerContainerCmd(port),
						labels:       map[string]string{"app": "stuff-doer"},
					},
					multiNetPolicy(
						"deny-all-egress-allow-all-ingress",
						secondaryNetworkName,
						metav1.LabelSelector{
							MatchLabels: map[string]string{"app": "stuff-doer"},
						},
						[]mnpapi.MultiPolicyType{mnpapi.PolicyTypeIngress, mnpapi.PolicyTypeEgress},
						[]mnpapi.MultiNetworkPolicyIngressRule{
							mnpapi.MultiNetworkPolicyIngressRule{},
						},
						nil,
					),
				),
			)

			DescribeTable(
				"deny traffic",
				func(netConfigParams networkAttachmentConfigParams, clientPodConfig podConfiguration, serverPodConfig podConfiguration, policy *mnpapi.MultiNetworkPolicy) {
					netConfig := newNetworkAttachmentConfig(netConfigParams)

					By("setting up the localnet underlay")
					Expect(infraprovider.SetupUnderlay(providerCtx, f, infraapi.Underlay{
						LogicalNetworkName: netConfig.networkName,
						VlanID:             netConfig.vlanID,
					})).To(Succeed())

					Expect(createNads(f, nadClient, extraNamespace, netConfig)).NotTo(HaveOccurred())

					kickstartPodInNamespace(cs, &clientPodConfig, f.Namespace.Name, extraNamespace.Name)
					kickstartPodInNamespace(cs, &serverPodConfig, f.Namespace.Name, extraNamespace.Name)

					serverIP, err := podIPForAttachment(cs, serverPodConfig.namespace, serverPodConfig.name, netConfig.name, 0)
					Expect(err).NotTo(HaveOccurred())
					assertServerPodIPInRange(netConfig.cidr, serverIP, netPrefixLengthPerNode)

					Expect(createMultiNetworkPolicy(mnpClient, f.Namespace.Name, policy)).To(Succeed())

					By("asserting the *client* pod can't contact the server pod exposed endpoint when using ingress deny-all")
					Eventually(func() error {
						return reachServerPodFromClient(cs, serverPodConfig, clientPodConfig, serverIP, port)
					}, 2*time.Minute, 6*time.Second).Should(Not(Succeed()))
				},
				Entry(
					"using ingress deny-all for a localnet topology",
					networkAttachmentConfigParams{
						name:     secondaryNetworkName,
						topology: "localnet",
						cidr:     secondaryLocalnetNetworkCIDR,
					},
					podConfiguration{
						attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:        allowedClient(clientPodName),
						labels: map[string]string{
							"app":  "client",
							"role": "trusted",
						},
					},
					podConfiguration{
						attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:         podName,
						containerCmd: httpServerContainerCmd(port),
						labels:       map[string]string{"app": "stuff-doer"},
					},
					multiNetPolicy(
						"deny-all-ingress",
						secondaryNetworkName,
						metav1.LabelSelector{
							MatchLabels: map[string]string{"app": "stuff-doer"},
						},
						[]mnpapi.MultiPolicyType{mnpapi.PolicyTypeIngress},
						nil,
						nil,
					),
				),
				Entry(
					"using pod selectors and wrong port range for a localnet topology",
					networkAttachmentConfigParams{
						name:     secondaryNetworkName,
						topology: "localnet",
						cidr:     secondaryLocalnetNetworkCIDR,
					},
					podConfiguration{
						attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:        allowedClient(clientPodName),
						labels: map[string]string{
							"app":  "client",
							"role": "trusted",
						},
					},
					podConfiguration{
						attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:         podName,
						containerCmd: httpServerContainerCmd(port),
						labels:       map[string]string{"app": "stuff-doer"},
					},
					multiNetIngressLimitingPolicy(
						secondaryNetworkName,
						metav1.LabelSelector{
							MatchLabels: map[string]string{"app": "stuff-doer"},
						},
						metav1.LabelSelector{
							MatchLabels: map[string]string{"role": "trusted"},
						},
						// build a port range that doesn't include server port
						multiNetPolicyPortRange(port-10, port-1),
					),
				),
			)
		})
	})

	Context("A pod with multiple attachments to the same OVN-K networks", func() {
		var pod *v1.Pod

		BeforeEach(func() {
			netAttachDefs := []networkAttachmentConfig{
				newAttachmentConfigWithOverriddenName(secondaryNetworkName, f.Namespace.Name, secondaryNetworkName, "layer2", secondaryFlatL2NetworkCIDR),
				newAttachmentConfigWithOverriddenName(secondaryNetworkName+"-alias", f.Namespace.Name, secondaryNetworkName, "layer2", secondaryFlatL2NetworkCIDR),
			}

			for i := range netAttachDefs {
				netConfig := netAttachDefs[i]
				By("creating the attachment configuration")
				_, err := nadClient.NetworkAttachmentDefinitions(netConfig.namespace).Create(
					context.Background(),
					generateNAD(netConfig, f.ClientSet),
					metav1.CreateOptions{},
				)
				Expect(err).NotTo(HaveOccurred())
			}

			podConfig := podConfiguration{
				attachments: []nadapi.NetworkSelectionElement{
					{Name: secondaryNetworkName},
					{Name: secondaryNetworkName + "-alias"},
				},
				name:      podName,
				namespace: f.Namespace.Name,
			}
			By("creating the pod using a secondary network")
			var err error
			pod, err = cs.CoreV1().Pods(podConfig.namespace).Create(
				context.Background(),
				generatePodSpec(podConfig),
				metav1.CreateOptions{},
			)
			Expect(err).NotTo(HaveOccurred())

			By("asserting the pod gets to the `Ready` phase")
			Eventually(func() v1.PodPhase {
				updatedPod, err := cs.CoreV1().Pods(podConfig.namespace).Get(context.Background(), pod.GetName(), metav1.GetOptions{})
				if err != nil {
					return v1.PodFailed
				}
				return updatedPod.Status.Phase
			}, 2*time.Minute, 6*time.Second).Should(Equal(v1.PodRunning))
		})

		It("features two different IPs from the same subnet", func() {
			var err error
			pod, err = cs.CoreV1().Pods(pod.GetNamespace()).Get(context.Background(), pod.GetName(), metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			netStatus, err := podNetworkStatus(pod, func(status nadapi.NetworkStatus) bool {
				return !status.Default
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(netStatus).To(HaveLen(2))

			Expect(netStatus[0].IPs).To(HaveLen(1))
			Expect(netStatus[1].IPs).To(HaveLen(1))
			Expect(netStatus[0].IPs[0]).NotTo(Equal(netStatus[1].IPs[0]))
			Expect(inRange(secondaryFlatL2NetworkCIDR, netStatus[0].IPs[0]))
			Expect(inRange(secondaryFlatL2NetworkCIDR, netStatus[1].IPs[0]))
		})
	})

	Context("A pod with multiple attachments to the same secondary NAD", func() {
		const (
			testNadName = "test-multi-secondary-nad"
			testPodName = "test-pod-multi-secondary-nad"
		)

		var pod *v1.Pod

		DescribeTable("features multiple different IPs and connectivity redundancy",
			func(netConfigParams networkAttachmentConfigParams) {
				netConfig := newNetworkAttachmentConfig(netConfigParams)
				netConfig.namespace = f.Namespace.Name
				netConfig.name = testNadName

				if netConfig.topology == "localnet" {
					By("setting up the localnet underlay")
					Expect(infraprovider.SetupUnderlay(providerCtx, f, infraapi.Underlay{
						LogicalNetworkName: netConfig.networkName,
						VlanID:             netConfig.vlanID,
					})).To(Succeed())
				}

				By("creating the secondary network attachment definition")
				_, err := nadClient.NetworkAttachmentDefinitions(netConfig.namespace).Create(
					context.Background(),
					generateNAD(netConfig, f.ClientSet),
					metav1.CreateOptions{},
				)
				Expect(err).NotTo(HaveOccurred())

				By("creating a pod with multiple attachments to the same secondary NAD")
				// Specify the same NAD name multiple times to test GetIndexedNADKey functionality
				// This will create indexed NAD keys like "ns/nad", "ns/nad/1"
				podConfig := podConfiguration{
					attachments: []nadapi.NetworkSelectionElement{
						{Name: testNadName, Namespace: netConfig.namespace}, // First attachment - will be indexed as "ns/nad"
						{Name: testNadName, Namespace: netConfig.namespace}, // Second attachment - will be indexed as "ns/nad/1"
					},
					name:         testPodName,
					namespace:    f.Namespace.Name,
					isPrivileged: true, // Required for ip link set commands to manipulate network interfaces
				}
				pod, err = cs.CoreV1().Pods(podConfig.namespace).Create(
					context.Background(),
					generatePodSpec(podConfig),
					metav1.CreateOptions{},
				)
				Expect(err).NotTo(HaveOccurred())

				By("asserting the pod gets to the `Running` phase")
				Eventually(func() v1.PodPhase {
					updatedPod, err := cs.CoreV1().Pods(podConfig.namespace).Get(context.Background(), pod.GetName(), metav1.GetOptions{})
					if err != nil {
						return v1.PodFailed
					}
					pod = updatedPod
					return updatedPod.Status.Phase
				}, 2*time.Minute, 6*time.Second).Should(Equal(v1.PodRunning))

				By("verifying the pod has two network status entries for the same secondary NAD")
				netStatus, err := podNetworkStatus(pod, func(status nadapi.NetworkStatus) bool {
					return !status.Default && strings.Contains(status.Name, testNadName)
				})
				Expect(err).NotTo(HaveOccurred())
				Expect(netStatus).To(HaveLen(2), "Pod should have two network status entries for the secondary NAD attachments")

				By("verifying both interfaces have IPs")
				for i := 0; i < 2; i++ {
					Expect(netStatus[i].IPs).NotTo(BeEmpty(), fmt.Sprintf("Interface %d should have at least one IP", i))
				}

				By("verifying both IPs are different")
				ips := make([]string, 2)
				for i := 0; i < 2; i++ {
					ips[i] = netStatus[i].IPs[0]
				}
				Expect(ips[0]).NotTo(Equal(ips[1]), "First and second interface IPs should be different")

				By("verifying all IPs are from the configured secondary NAD subnet")
				subnet, err := getNetCIDRSubnet(netConfig.cidr)
				Expect(err).NotTo(HaveOccurred())
				for i, ip := range ips {
					Expect(inRange(subnet, ip)).To(Succeed(), fmt.Sprintf("IP[%d] %s should be in subnet %s", i, ip, subnet))
					By(fmt.Sprintf("Verified IP[%d] %s is from subnet %s", i, ip, subnet))
				}

				By("verifying both interfaces have unique interface names")
				interfaceNames := make([]string, 2)
				for i := 0; i < 2; i++ {
					Expect(netStatus[i].Interface).NotTo(BeEmpty(), fmt.Sprintf("Interface %d should have a name", i))
					interfaceNames[i] = netStatus[i].Interface
				}
				Expect(interfaceNames[0]).NotTo(Equal(interfaceNames[1]), "First and second interface names should be different")

				By(fmt.Sprintf("Successfully validated two interfaces with IPs: %s, %s", ips[0], ips[1]))

				// For L3 secondary NADs, verify ECMP routes are added in the pod
				if netConfigParams.topology == "layer3" {
					By("verifying ECMP routes are added for L3 secondary NAD with multiple attachments")

					// Calculate the gateway IP from the pod's IP - gateway is the .1 address of the subnet
					// For example, if pod IP is 172.31.0.3/24, gateway is 172.31.0.1
					podIP := net.ParseIP(ips[0])
					Expect(podIP).NotTo(BeNil(), "Pod IP should be valid")
					podIPv4 := podIP.To4()
					Expect(podIPv4).NotTo(BeNil(), "Pod IP should be IPv4")
					expectedGateway := net.IPv4(podIPv4[0], podIPv4[1], podIPv4[2], 1).String()

					// Get routes for the secondary network CIDR
					routeOutput, err := e2ekubectl.RunKubectl(
						podConfig.namespace,
						"exec",
						pod.Name,
						"--",
						"ip",
						"route",
						"show",
						secondaryNetworkCIDR,
					)
					Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to get routes from pod for dest %s", secondaryNetworkCIDR))

					// ECMP routes should contain "nexthop" entries for both interfaces
					// Example format:
					// 172.31.0.0/16
					//     nexthop via 172.31.0.1 dev net1 weight 1
					//     nexthop via 172.31.0.1 dev net2 weight 1
					By(fmt.Sprintf("ECMP routes to %s", routeOutput))
					routes := strings.Split(routeOutput, "\n")
					// output should be at lease 3
					Expect(len(routes)).To(BeNumerically(">", 2))
					Expect(routes[1]).To(ContainSubstring(fmt.Sprintf("nexthop via %s dev %s weight 1", expectedGateway, interfaceNames[0])), fmt.Sprintf("ECMP routes should include interface %s", interfaceNames[0]))
					Expect(routes[2]).To(ContainSubstring(fmt.Sprintf("nexthop via %s dev %s weight 1", expectedGateway, interfaceNames[1])), fmt.Sprintf("ECMP routes should include interface %s", interfaceNames[1]))

					By(fmt.Sprintf("Successfully verified ECMP routes to %s via gateway %s with interfaces %s and %s",
						secondaryNetworkCIDR, expectedGateway, interfaceNames[0], interfaceNames[1]))
				}

				By("creating a second pod with a single attachment to the same secondary NAD")
				serverPodName := "test-pod-multi-secondary-nad-server"
				serverPodConfig := podConfiguration{
					attachments: []nadapi.NetworkSelectionElement{
						{Name: testNadName, Namespace: netConfig.namespace},
					},
					name:         serverPodName,
					namespace:    f.Namespace.Name,
					containerCmd: httpServerContainerCmd(8080),
				}
				serverPod, err := cs.CoreV1().Pods(serverPodConfig.namespace).Create(
					context.Background(),
					generatePodSpec(serverPodConfig),
					metav1.CreateOptions{},
				)
				Expect(err).NotTo(HaveOccurred())

				By("asserting the server pod gets to the `Running` phase")
				Eventually(func() v1.PodPhase {
					updatedPod, err := cs.CoreV1().Pods(serverPodConfig.namespace).Get(context.Background(), serverPod.GetName(), metav1.GetOptions{})
					if err != nil {
						return v1.PodFailed
					}
					serverPod = updatedPod
					return updatedPod.Status.Phase
				}, 2*time.Minute, 6*time.Second).Should(Equal(v1.PodRunning))

				By("getting the server pod's IP on the secondary NAD")
				serverNetStatus, err := podNetworkStatus(serverPod, func(status nadapi.NetworkStatus) bool {
					return !status.Default && strings.Contains(status.Name, testNadName)
				})
				Expect(err).NotTo(HaveOccurred())
				Expect(serverNetStatus).To(HaveLen(1), "Server pod should have one network status entry for the secondary NAD")
				Expect(serverNetStatus[0].IPs).NotTo(BeEmpty(), "Server pod should have at least one IP")
				serverIP := serverNetStatus[0].IPs[0]
				By(fmt.Sprintf("Server pod IP on secondary NAD: %s", serverIP))

				By("verifying connectivity from client pod to server pod with all interfaces up")
				Eventually(func() error {
					_, err := e2ekubectl.RunKubectl(
						podConfig.namespace,
						"exec",
						pod.Name,
						"--",
						"curl",
						"--connect-timeout",
						"2",
						fmt.Sprintf("http://%s:8080/hostname", serverIP),
					)
					return err
				}, 30*time.Second, 2*time.Second).Should(Succeed(), "Should be able to reach server pod from client pod")

				// Test connectivity explicitly using the first interface
				interfaceName := interfaceNames[0]
				By(fmt.Sprintf("verifying connectivity explicitly through interface %d (%s)", 0, interfaceName))

				Eventually(func() error {
					_, err := e2ekubectl.RunKubectl(
						podConfig.namespace,
						"exec",
						pod.Name,
						"--",
						"curl",
						"--connect-timeout",
						"2",
						"--interface",
						interfaceName,
						fmt.Sprintf("http://%s:8080/hostname", serverIP),
					)
					return err
				}, 30*time.Second, 2*time.Second).Should(Succeed(), fmt.Sprintf("Should be able to reach server through interface %s", interfaceName))

				By(fmt.Sprintf("Successfully verified connectivity through interface %d (%s)", 0, interfaceName))

				// Test redundancy: verify that when the first interface is down, we can still reach the server
				interfaceToDisable := interfaceNames[0]
				workingInterface := interfaceNames[1]

				By(fmt.Sprintf("bringing down interface 0 (%s) and verifying connectivity through interface 1 (%s)", interfaceToDisable, workingInterface))

				_, err = e2ekubectl.RunKubectl(
					podConfig.namespace,
					"exec",
					pod.Name,
					"--",
					"ip",
					"link",
					"set",
					interfaceToDisable,
					"down",
				)
				Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to bring down interface %s", interfaceToDisable))

				Eventually(func() error {
					_, err := e2ekubectl.RunKubectl(
						podConfig.namespace,
						"exec",
						pod.Name,
						"--",
						"curl",
						"--connect-timeout",
						"2",
						"--interface",
						workingInterface,
						fmt.Sprintf("http://%s:8080/hostname", serverIP),
					)
					return err
				}, 30*time.Second, 2*time.Second).Should(Succeed(), fmt.Sprintf("Should be able to reach server through working interface %s when %s is down", workingInterface, interfaceToDisable))

				_, err = e2ekubectl.RunKubectl(
					podConfig.namespace,
					"exec",
					pod.Name,
					"--",
					"ip",
					"link",
					"set",
					interfaceToDisable,
					"up",
				)
				Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to bring interface %s back up", interfaceToDisable))

				// Test the reverse
				interfaceToDisable = interfaceNames[1]
				workingInterface = interfaceNames[0]

				By(fmt.Sprintf("bringing down interface 1 (%s) and verifying connectivity through interface 0 (%s)", interfaceToDisable, workingInterface))

				_, err = e2ekubectl.RunKubectl(
					podConfig.namespace,
					"exec",
					pod.Name,
					"--",
					"ip",
					"link",
					"set",
					interfaceToDisable,
					"down",
				)
				Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to bring down interface %s", interfaceToDisable))

				Eventually(func() error {
					_, err := e2ekubectl.RunKubectl(
						podConfig.namespace,
						"exec",
						pod.Name,
						"--",
						"curl",
						"--connect-timeout",
						"2",
						"--interface",
						workingInterface,
						fmt.Sprintf("http://%s:8080/hostname", serverIP),
					)
					return err
				}, 30*time.Second, 2*time.Second).Should(Succeed(), fmt.Sprintf("Should be able to reach server through working interface %s when %s is down", workingInterface, interfaceToDisable))

				_, err = e2ekubectl.RunKubectl(
					podConfig.namespace,
					"exec",
					pod.Name,
					"--",
					"ip",
					"link",
					"set",
					interfaceToDisable,
					"up",
				)
				Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to bring interface %s back up", interfaceToDisable))

				By("Successfully verified that both interfaces can reach the same secondary NAD and provide redundancy")
			},
			Entry("L2 secondary NAD",
				networkAttachmentConfigParams{
					name:     testNadName,
					topology: "layer2",
					cidr:     secondaryFlatL2NetworkCIDR,
					role:     "secondary",
				},
			),
			Entry("L3 secondary NAD",
				networkAttachmentConfigParams{
					name:     testNadName,
					topology: "layer3",
					cidr:     netCIDR(secondaryNetworkCIDR, netPrefixLengthPerNode),
					role:     "secondary",
				},
			),
			Entry("Localnet secondary NAD",
				networkAttachmentConfigParams{
					name:     testNadName,
					topology: "localnet",
					cidr:     secondaryLocalnetNetworkCIDR,
					vlanID:   localnetVLANID,
					role:     "secondary",
				},
			),
		)
	})
})

func kickstartPod(cs clientset.Interface, configuration podConfiguration) *v1.Pod {
	podNamespacedName := fmt.Sprintf("%s/%s", configuration.namespace, configuration.name)
	var (
		pod *v1.Pod
		err error
	)
	By(fmt.Sprintf("instantiating pod %q", podNamespacedName))
	_, err = cs.CoreV1().Pods(configuration.namespace).Create(
		context.Background(),
		generatePodSpec(configuration),
		metav1.CreateOptions{},
	)
	Expect(err).WithOffset(1).NotTo(HaveOccurred())

	By(fmt.Sprintf("asserting that pod %q reaches the `Ready` state", podNamespacedName))
	EventuallyWithOffset(1, func() v1.PodPhase {
		p, err := cs.CoreV1().Pods(configuration.namespace).Get(context.Background(), configuration.name, metav1.GetOptions{})
		if err != nil {
			return v1.PodFailed
		}
		pod = p
		return p.Status.Phase

	}, 2*time.Minute, 6*time.Second).Should(Equal(v1.PodRunning))
	return pod // return the updated pod
}

func createNads(f *framework.Framework, nadClient nadclient.K8sCniCncfIoV1Interface, extraNamespace *v1.Namespace, netConfig networkAttachmentConfig) error {
	for _, ns := range []*v1.Namespace{f.Namespace, extraNamespace} {
		By(fmt.Sprintf("creating the nad for namespace %q", ns.Name))
		netConfig.namespace = ns.Name
		_, err := nadClient.NetworkAttachmentDefinitions(ns.Name).Create(
			context.Background(),
			generateNAD(netConfig, f.ClientSet),
			metav1.CreateOptions{},
		)
		if err != nil {
			return err
		}
	}

	return nil
}

func kickstartPodInNamespace(cs clientset.Interface, podConfig *podConfiguration, defaultNamespace string, extraNamespace string) *v1.Pod {
	if podConfig.requiresExtraNamespace {
		podConfig.namespace = extraNamespace
	} else {
		podConfig.namespace = defaultNamespace
	}

	return kickstartPod(cs, *podConfig)
}

func assertServerPodIPInRange(cidr string, serverIP string, netPrefixLengthPerNode int) {
	By(fmt.Sprintf("asserting the server pod IP %v is from the configured range %v/%v", serverIP, cidr, netPrefixLengthPerNode))
	subnet, err := getNetCIDRSubnet(cidr)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	ExpectWithOffset(1, inRange(subnet, serverIP)).To(Succeed())
}

func createMultiNetworkPolicy(mnpClient mnpclient.K8sCniCncfIoV1beta1Interface, namespace string, policy *mnpapi.MultiNetworkPolicy) error {
	By("provisioning the multi-network policy")
	_, err := mnpClient.MultiNetworkPolicies(namespace).Create(
		context.Background(),
		policy,
		metav1.CreateOptions{},
	)
	return err
}

// generateIPsFromNodePrimaryNetworkAddresses returns IPv4 and IPv6 addresses at the provided offset from the primary interface network addresses found on the node
func generateIPsFromNodePrimaryNetworkAddresses(cs clientset.Interface, nodeName string, offset int) ([]string, error) {
	hostSubnets, err := getHostSubnetsForNode(cs, nodeName)
	if err != nil {
		return nil, fmt.Errorf("failed to get host subnets for node %q: %w", nodeName, err)
	}
	return generateIPsFromSubnets(hostSubnets, offset)
}

func addIPRequestToPodConfig(cs clientset.Interface, podConfig *podConfiguration, offset int) error {
	nodeName, ok := podConfig.nodeSelector[nodeHostnameKey]
	if !ok {
		return fmt.Errorf("missing node selector %q in podConfig for pod %s/%s", nodeHostnameKey, podConfig.namespace, podConfig.name)
	}

	var (
		ipsToRequest []string
		err          error
	)

	switch podConfig.ipRequestFromSubnet {
	case fromHostSubnet:
		ipsToRequest, err = generateIPsFromNodePrimaryNetworkAddresses(cs, nodeName, offset)

	case fromExternalNetwork:
		subnets := filterCIDRs(cs, externalNetworkSubnetV4, externalNetworkSubnetV6)
		if len(subnets) == 0 {
			return fmt.Errorf("no external network subnets available for IP family support")
		}
		ipsToRequest, err = generateIPsFromSubnets(subnets, offset)

	default:
		return fmt.Errorf("unknown or unimplemented subnet source: %q", podConfig.ipRequestFromSubnet)
	}

	if err != nil {
		return err
	}
	for i := range podConfig.attachments {
		podConfig.attachments[i].IPRequest = ipsToRequest
	}
	return nil
}
