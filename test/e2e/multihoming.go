package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/docker/docker/client"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"

	mnpapi "github.com/k8snetworkplumbingwg/multi-networkpolicy/pkg/apis/k8s.cni.cncf.io/v1beta1"
	mnpclient "github.com/k8snetworkplumbingwg/multi-networkpolicy/pkg/client/clientset/versioned/typed/k8s.cni.cncf.io/v1beta1"
	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	nadclient "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/typed/k8s.cni.cncf.io/v1"
)

const PolicyForAnnotation = "k8s.v1.cni.cncf.io/policy-for"

var _ = Describe("Multi Homing", func() {
	const (
		podName                      = "tinypod"
		secondaryNetworkCIDR         = "10.128.0.0/16"
		secondaryNetworkName         = "tenant-blue"
		secondaryFlatL2IgnoreCIDR    = "10.128.0.0/29"
		secondaryFlatL2NetworkCIDR   = "10.128.0.0/24"
		secondaryLocalnetIgnoreCIDR  = "60.128.0.0/29"
		secondaryLocalnetNetworkCIDR = "60.128.0.0/24"
		netPrefixLengthPerNode       = 24
		localnetVLANID               = 10
		secondaryIPv6CIDR            = "2010:100:200::0/60"
		netPrefixLengthIPv6PerNode   = 64
	)
	f := wrappedTestFramework("multi-homing")

	var (
		cs        clientset.Interface
		nadClient nadclient.K8sCniCncfIoV1Interface
		mnpClient mnpclient.K8sCniCncfIoV1beta1Interface
	)

	BeforeEach(func() {
		cs = f.ClientSet

		var err error
		nadClient, err = nadclient.NewForConfig(f.ClientConfig())
		Expect(err).NotTo(HaveOccurred())
		mnpClient, err = mnpclient.NewForConfig(f.ClientConfig())
		Expect(err).NotTo(HaveOccurred())
	})

	Context("A single pod with an OVN-K secondary network", func() {
		ginkgo.DescribeTable("is able to get to the Running phase", func(netConfigParams networkAttachmentConfigParams, podConfig podConfiguration) {
			netConfig := newNetworkAttachmentConfig(netConfigParams)

			netConfig.namespace = f.Namespace.Name
			podConfig.namespace = f.Namespace.Name

			By("creating the attachment configuration")
			_, err := nadClient.NetworkAttachmentDefinitions(netConfig.namespace).Create(
				context.Background(),
				generateNAD(netConfig),
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
			ginkgo.Entry(
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
			ginkgo.Entry(
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
			ginkgo.Entry(
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
			ginkgo.Entry(
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
			ginkgo.Entry(
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
			ginkgo.Entry(
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
			ginkgo.Entry(
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
			ginkgo.Entry(
				"when attaching to an localnet - switched - network",
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
			ginkgo.Entry(
				"when attaching to an Localnet - switched - network featuring `excludeCIDR`s",
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
			ginkgo.Entry(
				"when attaching to an localnet - switched - network without IPAM",
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
			ginkgo.Entry(
				"when attaching to an localnet - switched - network with an IPv6 subnet",
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
			ginkgo.Entry(
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
	})

	Context("multiple pods connected to the same OVN-K secondary network", func() {
		const (
			clientPodName     = "client-pod"
			nodeHostnameKey   = "kubernetes.io/hostname"
			port              = 9000
			workerOneNodeName = "ovn-worker"
			workerTwoNodeName = "ovn-worker2"
			clientIP          = "192.168.200.10/24"
			staticServerIP    = "192.168.200.20/24"
		)

		ginkgo.DescribeTable(
			"can communicate over the secondary network",
			func(netConfigParams networkAttachmentConfigParams, clientPodConfig podConfiguration, serverPodConfig podConfiguration) {
				netConfig := newNetworkAttachmentConfig(netConfigParams)

				netConfig.namespace = f.Namespace.Name
				clientPodConfig.namespace = f.Namespace.Name
				serverPodConfig.namespace = f.Namespace.Name

				if netConfig.topology == "localnet" {
					By("setting up the localnet underlay")
					nodes := ovsPods(cs)
					Expect(nodes).NotTo(BeEmpty())
					defer func() {
						By("tearing down the localnet underlay")
						Expect(teardownUnderlay(nodes)).To(Succeed())
					}()

					const secondaryInterfaceName = "eth1"
					Expect(setupUnderlay(nodes, secondaryInterfaceName, netConfig)).To(Succeed())
				}

				By("creating the attachment configuration")
				_, err := nadClient.NetworkAttachmentDefinitions(f.Namespace.Name).Create(
					context.Background(),
					generateNAD(netConfig),
					metav1.CreateOptions{},
				)
				Expect(err).NotTo(HaveOccurred())

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
						return reachToServerPodFromClient(cs, serverPodConfig, clientPodConfig, serverIP, port)
					}, 2*time.Minute, 6*time.Second).Should(Succeed())
				}
			},
			ginkgo.Entry(
				"can communicate over an L2 secondary network when the pods are scheduled in different nodes",
				networkAttachmentConfigParams{
					name:     secondaryNetworkName,
					topology: "layer2",
					cidr:     secondaryNetworkCIDR,
				},
				podConfiguration{
					attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:         clientPodName,
					nodeSelector: map[string]string{nodeHostnameKey: workerOneNodeName},
				},
				podConfiguration{
					attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:         podName,
					containerCmd: httpServerContainerCmd(port),
					nodeSelector: map[string]string{nodeHostnameKey: workerTwoNodeName},
				},
			),
			ginkgo.Entry(
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
			ginkgo.Entry(
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
			ginkgo.Entry(
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
			ginkgo.Entry(
				"can communicate over an L3 - routed - secondary network with a dual stack configuration",
				networkAttachmentConfigParams{
					name:     secondaryNetworkName,
					topology: "layer3",
					cidr:     strings.Join([]string{netCIDR(secondaryNetworkCIDR, netPrefixLengthPerNode), netCIDR(secondaryIPv6CIDR, netPrefixLengthIPv6PerNode)}, ","),
				},
				podConfiguration{
					attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:         clientPodName,
					nodeSelector: map[string]string{nodeHostnameKey: workerOneNodeName},
				},
				podConfiguration{
					attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:         podName,
					containerCmd: httpServerContainerCmd(port),
					nodeSelector: map[string]string{nodeHostnameKey: workerTwoNodeName},
				},
			),
			ginkgo.Entry(
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
			ginkgo.Entry(
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
			ginkgo.Entry(
				"can communicate over an L2 secondary network with an IPv6 subnet when pods are scheduled in different nodes",
				networkAttachmentConfigParams{
					name:     secondaryNetworkName,
					topology: "layer2",
					cidr:     secondaryIPv6CIDR,
				},
				podConfiguration{
					attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:         clientPodName,
					nodeSelector: map[string]string{nodeHostnameKey: workerOneNodeName},
				},
				podConfiguration{
					attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:         podName,
					containerCmd: httpServerContainerCmd(port),
					nodeSelector: map[string]string{nodeHostnameKey: workerTwoNodeName},
				},
			),
			ginkgo.Entry(
				"can communicate over an L2 secondary network with a dual stack configuration",
				networkAttachmentConfigParams{
					name:     secondaryNetworkName,
					topology: "layer2",
					cidr:     strings.Join([]string{secondaryFlatL2NetworkCIDR, secondaryIPv6CIDR}, ","),
				},
				podConfiguration{
					attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:         clientPodName,
					nodeSelector: map[string]string{nodeHostnameKey: workerOneNodeName},
				},
				podConfiguration{
					attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:         podName,
					containerCmd: httpServerContainerCmd(port),
					nodeSelector: map[string]string{nodeHostnameKey: workerTwoNodeName},
				},
			),
			ginkgo.Entry(
				"can communicate over an localnet secondary network when the pods are scheduled on different nodes",
				networkAttachmentConfigParams{
					name:     secondaryNetworkName,
					topology: "localnet",
					cidr:     secondaryLocalnetNetworkCIDR,
					vlanID:   localnetVLANID,
				},
				podConfiguration{
					attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:         clientPodName,
					nodeSelector: map[string]string{nodeHostnameKey: workerOneNodeName},
				},
				podConfiguration{
					attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:         podName,
					containerCmd: httpServerContainerCmd(port),
					nodeSelector: map[string]string{nodeHostnameKey: workerTwoNodeName},
				},
			),
			ginkgo.Entry(
				"can communicate over an localnet secondary network without IPAM when the pods are scheduled on different nodes",
				networkAttachmentConfigParams{
					name:     secondaryNetworkName,
					topology: "localnet",
					vlanID:   localnetVLANID,
				},
				podConfiguration{
					attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:         clientPodName,
					nodeSelector: map[string]string{nodeHostnameKey: workerOneNodeName},
					isPrivileged: true,
				},
				podConfiguration{
					attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:         podName,
					containerCmd: httpServerContainerCmd(port),
					nodeSelector: map[string]string{nodeHostnameKey: workerTwoNodeName},
					isPrivileged: true,
				},
			),
			ginkgo.Entry(
				"can communicate over an localnet secondary network without IPAM when the pods are scheduled on different nodes, with static IPs configured via network selection elements",
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
					name:         clientPodName,
					nodeSelector: map[string]string{nodeHostnameKey: workerOneNodeName},
				},
				podConfiguration{
					attachments: []nadapi.NetworkSelectionElement{{
						Name:      secondaryNetworkName,
						IPRequest: []string{staticServerIP},
					}},
					name:         podName,
					containerCmd: httpServerContainerCmd(port),
					nodeSelector: map[string]string{nodeHostnameKey: workerTwoNodeName},
				},
			),
			ginkgo.Entry(
				"can communicate over an localnet secondary network with an IPv6 subnet when pods are scheduled on different nodes",
				networkAttachmentConfigParams{
					name:     secondaryNetworkName,
					topology: "localnet",
					cidr:     secondaryIPv6CIDR,
					vlanID:   localnetVLANID,
				},
				podConfiguration{
					attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:         clientPodName,
					nodeSelector: map[string]string{nodeHostnameKey: workerOneNodeName},
				},
				podConfiguration{
					attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:         podName,
					containerCmd: httpServerContainerCmd(port),
					nodeSelector: map[string]string{nodeHostnameKey: workerTwoNodeName},
				},
			),
			ginkgo.Entry(
				"can communicate over an localnet secondary network with a dual stack configuration when pods are scheduled on different nodes",
				networkAttachmentConfigParams{
					name:     secondaryNetworkName,
					topology: "localnet",
					cidr:     strings.Join([]string{secondaryLocalnetNetworkCIDR, secondaryIPv6CIDR}, ","),
					vlanID:   localnetVLANID,
				},
				podConfiguration{
					attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:         clientPodName,
					nodeSelector: map[string]string{nodeHostnameKey: workerOneNodeName},
				},
				podConfiguration{
					attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:         podName,
					containerCmd: httpServerContainerCmd(port),
					nodeSelector: map[string]string{nodeHostnameKey: workerTwoNodeName},
				},
			),
		)

		Context("localnet OVN-K secondary network", func() {
			const (
				clientPodName          = "client-pod"
				nodeHostnameKey        = "kubernetes.io/hostname"
				servicePort            = 9000
				dockerNetworkName      = "underlay"
				underlayServiceIP      = "60.128.0.1"
				secondaryInterfaceName = "eth1"
			)

			var netConfig networkAttachmentConfig
			var nodes []v1.Pod
			var underlayBridgeName string
			var cmdWebServer *exec.Cmd

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
						})

					By("setting up the localnet underlay")
					nodes = ovsPods(cs)
					Expect(nodes).NotTo(BeEmpty())
					Expect(setupUnderlay(nodes, secondaryInterfaceName, netConfig)).To(Succeed())
				})

				BeforeEach(func() {
					By("adding IP to the underlay docker bridge")
					cli, err := client.NewClientWithOpts(client.FromEnv)
					Expect(err).NotTo(HaveOccurred())

					gatewayIP, err := getNetworkGateway(cli, dockerNetworkName)
					Expect(err).NotTo(HaveOccurred())

					underlayBridgeName, err = findInterfaceByIP(gatewayIP)
					Expect(err).NotTo(HaveOccurred())

					cmd := exec.Command("sudo", "ip", "addr", "add", underlayIP, "dev", underlayBridgeName)
					cmd.Stderr = os.Stderr
					err = cmd.Run()
					Expect(err).NotTo(HaveOccurred())
				})

				BeforeEach(func() {
					By("starting a service, connected to the underlay")
					cmdWebServer = exec.Command("python3", "-m", "http.server", "--bind", underlayServiceIP, strconv.Itoa(servicePort))
					cmdWebServer.Stderr = os.Stderr
					Expect(cmdWebServer.Start()).NotTo(HaveOccurred(), "failed to create web server, port might be busy")
				})

				BeforeEach(func() {
					By("creating the attachment configuration")
					_, err := nadClient.NetworkAttachmentDefinitions(netConfig.namespace).Create(
						context.Background(),
						generateNAD(netConfig),
						metav1.CreateOptions{},
					)
					Expect(err).NotTo(HaveOccurred())
				})

				AfterEach(func() {
					err := cmdWebServer.Process.Kill()
					Expect(err).NotTo(HaveOccurred())
				})

				AfterEach(func() {
					cmd := exec.Command("sudo", "ip", "addr", "del", underlayIP, "dev", underlayBridgeName)
					cmd.Stderr = os.Stderr
					err := cmd.Run()
					Expect(err).NotTo(HaveOccurred())
				})

				AfterEach(func() {
					By("tearing down the localnet underlay")
					Expect(teardownUnderlay(nodes)).To(Succeed())
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
						Expect(connectToServer(clientPodConfig, underlayServiceIP, servicePort)).To(Succeed())
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
						Expect(connectToServer(clientPodConfig, underlayServiceIP, servicePort)).To(MatchError(ContainSubstring("exit code 28")))
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
							Expect(connectToServer(clientPodConfig, underlayServiceIP, servicePort)).To(Succeed())
						} else {
							Expect(connectToServer(clientPodConfig, underlayServiceIP, servicePort)).To(Not(Succeed()))
						}

						By("deleting the multi-network policy")
						Expect(mnpClient.MultiNetworkPolicies(clientPodConfig.namespace).Delete(
							context.Background(),
							mnp.Name,
							metav1.DeleteOptions{},
						)).To(Succeed())

						By("asserting the *client* pod can contact the underlay service after deleting the policy")
						Expect(connectToServer(clientPodConfig, underlayServiceIP, servicePort)).To(Succeed())
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
							Label("BUG", "OCPBUGS-25928"),
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
					nodes = ovsPods(cs)
					Expect(nodes).NotTo(BeEmpty())

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
					Expect(setupUnderlay(nodes, secondaryInterfaceName, netConfig)).To(Succeed(), "configuring the OVS bridge")

					By(fmt.Sprintf("creating a VLAN interface on top of the bridge connecting the cluster nodes with IP: %s", underlayIP))
					cli, err := client.NewClientWithOpts(client.FromEnv)
					Expect(err).NotTo(HaveOccurred())

					gatewayIP, err := getNetworkGateway(cli, dockerNetworkName)
					Expect(err).NotTo(HaveOccurred())

					underlayBridgeName, err = findInterfaceByIP(gatewayIP)
					Expect(err).NotTo(HaveOccurred())
					Expect(createVLANInterface(underlayBridgeName, strconv.Itoa(vlanID), &underlayIP)).To(
						Succeed(),
						"create a VLAN interface on the bridge interconnecting the cluster nodes",
					)

					By("starting a service, connected to the underlay")
					cmdWebServer = exec.Command("python3", "-m", "http.server", "--bind", underlayServiceIP, strconv.Itoa(port))
					cmdWebServer.Stderr = os.Stderr
					Expect(cmdWebServer.Start()).NotTo(HaveOccurred(), "failed to create web server, port might be busy")
				})

				AfterEach(func() {
					Expect(cmdWebServer.Process.Kill()).NotTo(HaveOccurred(), "kill the python webserver")
					Expect(deleteVLANInterface(underlayBridgeName, strconv.Itoa(vlanID))).NotTo(HaveOccurred(), "remove the underlay physical configuration")
					Expect(teardownUnderlay(nodes)).To(Succeed(), "tear down the localnet underlay")
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
						generateNAD(vlan20NetConfig),
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

		Context("multi-network policies", func() {
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
					return podCatchErr == nil && errors.IsNotFound(err) && len(nsPods.Items) == 0
				}, 2*time.Minute, 5*time.Second).Should(BeTrue())
			})

			ginkgo.DescribeTable(
				"multi-network policies configure traffic allow lists",
				func(netConfigParams networkAttachmentConfigParams, allowedClientPodConfig podConfiguration, blockedClientPodConfig podConfiguration, serverPodConfig podConfiguration, policy *mnpapi.MultiNetworkPolicy) {
					netConfig := newNetworkAttachmentConfig(netConfigParams)

					if netConfig.topology == "localnet" {
						By("setting up the localnet underlay")
						nodes := ovsPods(cs)
						Expect(nodes).NotTo(BeEmpty())
						defer func() {
							By("tearing down the localnet underlay")
							Expect(teardownUnderlay(nodes)).To(Succeed())
						}()

						const secondaryInterfaceName = "eth1"
						Expect(setupUnderlay(nodes, secondaryInterfaceName, netConfig)).To(Succeed())
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
						return reachToServerPodFromClient(cs, serverPodConfig, allowedClientPodConfig, serverIP, port)
					}, 2*time.Minute, 6*time.Second).Should(Succeed())

					By("asserting the *blocked-client* pod **cannot** contact the server pod exposed endpoint")
					Expect(connectToServer(blockedClientPodConfig, serverIP, port)).To(MatchError(ContainSubstring("exit code 28")))
				},
				ginkgo.Entry(
					"for a pure L2 overlay when the multi-net policy describes the allow-list using pod selectors",
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
						port,
					),
				),
				ginkgo.Entry(
					"for a routed topology when the multi-net policy describes the allow-list using pod selectors",
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
						port,
					),
				),
				ginkgo.Entry(
					"for a localnet topology when the multi-net policy describes the allow-list using pod selectors",
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
						port,
					),
				),
				ginkgo.Entry(
					"for a pure L2 overlay when the multi-net policy describes the allow-list using IPBlock",
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
				ginkgo.Entry(
					"for a routed topology when the multi-net policy describes the allow-list using IPBlock",
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
				ginkgo.Entry(
					"for a localnet topology when the multi-net policy describes the allow-list using IPBlock",
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
				ginkgo.Entry(
					"for a pure L2 overlay when the multi-net policy describes the allow-list via namespace selectors",
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
				ginkgo.Entry(
					"for a routed topology when the multi-net policy describes the allow-list via namespace selectors",
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
				ginkgo.Entry(
					"for a localnet topology when the multi-net policy describes the allow-list via namespace selectors",
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

				ginkgo.Entry(
					"for an IPAMless pure L2 overlay when the multi-net policy describes the allow-list using IPBlock",
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

			ginkgo.DescribeTable(
				"multi-network ingress allow all",
				func(netConfigParams networkAttachmentConfigParams, clientPodConfig podConfiguration, serverPodConfig podConfiguration, policy *mnpapi.MultiNetworkPolicy) {
					netConfig := newNetworkAttachmentConfig(netConfigParams)

					By("setting up the localnet underlay")
					nodes := ovsPods(cs)
					Expect(nodes).NotTo(BeEmpty())
					defer func() {
						By("tearing down the localnet underlay")
						Expect(teardownUnderlay(nodes)).To(Succeed())
					}()
					const secondaryInterfaceName = "eth1"
					Expect(setupUnderlay(nodes, secondaryInterfaceName, netConfig)).To(Succeed())

					Expect(createNads(f, nadClient, extraNamespace, netConfig)).NotTo(HaveOccurred())

					kickstartPodInNamespace(cs, &clientPodConfig, f.Namespace.Name, extraNamespace.Name)
					kickstartPodInNamespace(cs, &serverPodConfig, f.Namespace.Name, extraNamespace.Name)

					serverIP, err := podIPForAttachment(cs, serverPodConfig.namespace, serverPodConfig.name, netConfig.name, 0)
					Expect(err).NotTo(HaveOccurred())
					assertServerPodIPInRange(netConfig.cidr, serverIP, netPrefixLengthPerNode)

					Expect(createMultiNetworkPolicy(mnpClient, f.Namespace.Name, policy)).To(Succeed())

					By("asserting the *client* pod can contact the server pod exposed endpoint")
					Eventually(func() error {
						return reachToServerPodFromClient(cs, serverPodConfig, clientPodConfig, serverIP, port)
					}, 2*time.Minute, 6*time.Second).Should(Succeed())
				},
				ginkgo.Entry(
					"for a localnet topology when the multi-net policy is ingress allow-all",
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
				ginkgo.XEntry(
					"for a localnet topology when the multi-net policy is egress deny-all, should not affect ingress",
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
					Label("BUG", "OCPBUGS-25928"),
				),
				ginkgo.Entry(
					"for a localnet topology when the multi-net policy is egress deny-all, ingress allow-all",
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

			ginkgo.DescribeTable(
				"multi-network ingress deny all policies",
				func(netConfigParams networkAttachmentConfigParams, clientPodConfig podConfiguration, serverPodConfig podConfiguration, policy *mnpapi.MultiNetworkPolicy) {
					netConfig := newNetworkAttachmentConfig(netConfigParams)

					By("setting up the localnet underlay")
					nodes := ovsPods(cs)
					Expect(nodes).NotTo(BeEmpty())
					defer func() {
						By("tearing down the localnet underlay")
						Expect(teardownUnderlay(nodes)).To(Succeed())
					}()
					const secondaryInterfaceName = "eth1"
					Expect(setupUnderlay(nodes, secondaryInterfaceName, netConfig)).To(Succeed())

					Expect(createNads(f, nadClient, extraNamespace, netConfig)).NotTo(HaveOccurred())

					kickstartPodInNamespace(cs, &clientPodConfig, f.Namespace.Name, extraNamespace.Name)
					kickstartPodInNamespace(cs, &serverPodConfig, f.Namespace.Name, extraNamespace.Name)

					serverIP, err := podIPForAttachment(cs, serverPodConfig.namespace, serverPodConfig.name, netConfig.name, 0)
					Expect(err).NotTo(HaveOccurred())
					assertServerPodIPInRange(netConfig.cidr, serverIP, netPrefixLengthPerNode)

					Expect(createMultiNetworkPolicy(mnpClient, f.Namespace.Name, policy)).To(Succeed())

					By("asserting the *client* pod can't contact the server pod exposed endpoint when using ingress deny-all")
					Eventually(func() error {
						return reachToServerPodFromClient(cs, serverPodConfig, clientPodConfig, serverIP, port)
					}, 2*time.Minute, 6*time.Second).Should(Not(Succeed()))
				},
				ginkgo.Entry(
					"for a localnet topology when the multi-net policy is ingress deny-all",
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
					generateNAD(netConfig),
					metav1.CreateOptions{},
				)
				Expect(err).NotTo(HaveOccurred())
			}

			By("sitting on our hands for a couple secs we give the controller time to sync all NADs before provisioning policies and pods")
			// TODO: this is temporary. We hope to eventually sync pods & multi-net policies on NAD C/U/D ops
			time.Sleep(3 * time.Second)

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
})

func kickstartPod(cs clientset.Interface, configuration podConfiguration) *v1.Pod {
	By(fmt.Sprintf("instantiating the %q pod", fmt.Sprintf("%s/%s", configuration.namespace, configuration.name)))
	createdPod, err := cs.CoreV1().Pods(configuration.namespace).Create(
		context.Background(),
		generatePodSpec(configuration),
		metav1.CreateOptions{},
	)
	Expect(err).WithOffset(1).NotTo(HaveOccurred())

	By("asserting the pod reaches the `Ready` state")
	EventuallyWithOffset(1, func() v1.PodPhase {
		updatedPod, err := cs.CoreV1().Pods(configuration.namespace).Get(context.Background(), configuration.name, metav1.GetOptions{})
		if err != nil {
			return v1.PodFailed
		}
		return updatedPod.Status.Phase
	}, 2*time.Minute, 6*time.Second).Should(Equal(v1.PodRunning))
	return createdPod
}

func createNads(f *framework.Framework, nadClient nadclient.K8sCniCncfIoV1Interface, extraNamespace *v1.Namespace, netConfig networkAttachmentConfig) error {
	for _, ns := range []*v1.Namespace{f.Namespace, extraNamespace} {
		By(fmt.Sprintf("creating the nad for namespace %q", ns.Name))
		netConfig.namespace = ns.Name
		_, err := nadClient.NetworkAttachmentDefinitions(ns.Name).Create(
			context.Background(),
			generateNAD(netConfig),
			metav1.CreateOptions{},
		)
		if err != nil {
			return err
		}
	}

	By("sitting on our hands for a couple secs we give the controller time to sync all NADs before provisioning policies and pods")
	// TODO: this is temporary. We hope to eventually sync pods & multi-net policies on NAD C/U/D ops
	time.Sleep(3 * time.Second)

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
