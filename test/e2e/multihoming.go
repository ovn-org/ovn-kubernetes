package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"time"

	. "github.com/onsi/ginkgo"
	"github.com/onsi/ginkgo/extensions/table"
	. "github.com/onsi/gomega"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/rand"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"

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
	})

	Context("A single pod with an OVN-K secondary network", func() {
		table.DescribeTable("is able to get to the Running phase", func(netConfig networkAttachmentConfig, podConfig podConfiguration) {
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
			table.Entry(
				"when attaching to an L3 - routed - network",
				networkAttachmentConfig{
					cidr:     netCIDR(secondaryNetworkCIDR, netPrefixLengthPerNode),
					name:     secondaryNetworkName,
					topology: "layer3",
				},
				podConfiguration{
					attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:        podName,
				},
			),
			table.Entry(
				"when attaching to an L3 - routed - network with IPv6 network",
				networkAttachmentConfig{
					cidr:     netCIDR(secondaryIPv6CIDR, netPrefixLengthIPv6PerNode),
					name:     secondaryNetworkName,
					topology: "layer3",
				},
				podConfiguration{
					attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:        podName,
				},
			),
			table.Entry(
				"when attaching to an L2 - switched - network",
				networkAttachmentConfig{
					cidr:     secondaryFlatL2NetworkCIDR,
					name:     secondaryNetworkName,
					topology: "layer2",
				},
				podConfiguration{
					attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:        podName,
				},
			),
			table.Entry(
				"when attaching to an L2 - switched - network featuring `excludeCIDR`s",
				networkAttachmentConfig{
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
			table.Entry(
				"when attaching to an L2 - switched - network without IPAM",
				networkAttachmentConfig{
					name:     secondaryNetworkName,
					topology: "layer2",
				},
				podConfiguration{
					attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:        podName,
				},
			),
			table.Entry(
				"when attaching to an L2 - switched - network with an IPv6 subnet",
				networkAttachmentConfig{
					cidr:     secondaryIPv6CIDR,
					name:     secondaryNetworkName,
					topology: "layer2",
				},
				podConfiguration{
					attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:        podName,
				},
			),
			table.Entry(
				"when attaching to an L2 - switched - network with a dual stack configuration",
				networkAttachmentConfig{
					cidr:     strings.Join([]string{secondaryFlatL2NetworkCIDR, secondaryIPv6CIDR}, ","),
					name:     secondaryNetworkName,
					topology: "layer2",
				},
				podConfiguration{
					attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:        podName,
				},
			),
			table.Entry(
				"when attaching to an localnet - switched - network",
				networkAttachmentConfig{
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
			table.Entry(
				"when attaching to an Localnet - switched - network featuring `excludeCIDR`s",
				networkAttachmentConfig{
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
			table.Entry(
				"when attaching to an localnet - switched - network without IPAM",
				networkAttachmentConfig{
					name:     secondaryNetworkName,
					topology: "localnet",
					vlanID:   localnetVLANID,
				},
				podConfiguration{
					attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:        podName,
				},
			),
			table.Entry(
				"when attaching to an localnet - switched - network with an IPv6 subnet",
				networkAttachmentConfig{
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
			table.Entry(
				"when attaching to an L2 - switched - network with a dual stack configuration",
				networkAttachmentConfig{
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

		table.DescribeTable(
			"can communicate over the secondary network",
			func(netConfig networkAttachmentConfig, clientPodConfig podConfiguration, serverPodConfig podConfiguration) {
				netConfig.namespace = f.Namespace.Name
				clientPodConfig.namespace = f.Namespace.Name
				serverPodConfig.namespace = f.Namespace.Name

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
						updatedPod, err := cs.CoreV1().Pods(f.Namespace.Name).Get(context.Background(), serverPod.GetName(), metav1.GetOptions{})
						if err != nil {
							return err
						}

						if updatedPod.Status.Phase == v1.PodRunning {
							return connectToServer(f.Namespace.Name, clientPodName, serverIP, port)
						}

						return fmt.Errorf("pod not running. /me is sad")
					}, 2*time.Minute, 6*time.Second).Should(Succeed())
				}
			},
			table.Entry(
				"can communicate over an L2 secondary network when the pods are scheduled in different nodes",
				networkAttachmentConfig{
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
			table.Entry(
				"can communicate over an L2 - switched - secondary network with `excludeCIDR`s",
				networkAttachmentConfig{
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
			table.Entry(
				"can communicate over an L3 - routed - secondary network",
				networkAttachmentConfig{
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
			table.Entry(
				"can communicate over an L3 - routed - secondary network with IPv6 subnet",
				networkAttachmentConfig{
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
			table.Entry(
				"can communicate over an L3 - routed - secondary network with a dual stack configuration",
				networkAttachmentConfig{
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
			table.Entry(
				"can communicate over an L2 - switched - secondary network without IPAM",
				networkAttachmentConfig{
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
			table.Entry(
				"can communicate over an L2 secondary network without IPAM, with static IPs configured via network selection elements",
				networkAttachmentConfig{
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
			table.Entry(
				"can communicate over an L2 secondary network with an IPv6 subnet when pods are scheduled in different nodes",
				networkAttachmentConfig{
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
			table.Entry(
				"can communicate over an L2 secondary network with a dual stack configuration",
				networkAttachmentConfig{
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
			table.Entry(
				"can communicate over an Localnet secondary network when the pods are scheduled on the same node",
				networkAttachmentConfig{
					name:     secondaryNetworkName,
					topology: "localnet",
					cidr:     secondaryLocalnetNetworkCIDR,
				},
				podConfiguration{
					attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:         clientPodName,
					nodeSelector: map[string]string{nodeHostnameKey: workerTwoNodeName},
				},
				podConfiguration{
					attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:         podName,
					containerCmd: httpServerContainerCmd(port),
					nodeSelector: map[string]string{nodeHostnameKey: workerTwoNodeName},
				},
			),
			table.Entry(
				"can communicate over an Localnet secondary network without IPAM when the pods are scheduled on the same node",
				networkAttachmentConfig{
					name:     secondaryNetworkName,
					topology: "localnet",
				},
				podConfiguration{
					attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:         clientPodName,
					nodeSelector: map[string]string{nodeHostnameKey: workerTwoNodeName},
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
			table.Entry(
				"can communicate over an localnet secondary network without IPAM when the pods are scheduled on the same node, with static IPs configured via network selection elements",
				networkAttachmentConfig{
					name:     secondaryNetworkName,
					topology: "localnet",
				},
				podConfiguration{
					attachments: []nadapi.NetworkSelectionElement{{
						Name:      secondaryNetworkName,
						IPRequest: []string{clientIP},
					}},
					name:         clientPodName,
					nodeSelector: map[string]string{nodeHostnameKey: workerTwoNodeName},
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
			table.Entry(
				"can communicate over an localnet secondary network with an IPv6 subnet when pods are scheduled on the same node",
				networkAttachmentConfig{
					name:     secondaryNetworkName,
					topology: "localnet",
					cidr:     secondaryIPv6CIDR,
				},
				podConfiguration{
					attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:         clientPodName,
					nodeSelector: map[string]string{nodeHostnameKey: workerTwoNodeName},
				},
				podConfiguration{
					attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:         podName,
					containerCmd: httpServerContainerCmd(port),
					nodeSelector: map[string]string{nodeHostnameKey: workerTwoNodeName},
				},
			),
			table.Entry(
				"can communicate over an localnet secondary network with a dual stack configuration when pods are scheduled on the same node",
				networkAttachmentConfig{
					name:     secondaryNetworkName,
					topology: "localnet",
					cidr:     strings.Join([]string{secondaryLocalnetNetworkCIDR, secondaryIPv6CIDR}, ","),
				},
				podConfiguration{
					attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:         clientPodName,
					nodeSelector: map[string]string{nodeHostnameKey: workerTwoNodeName},
				},
				podConfiguration{
					attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:         podName,
					containerCmd: httpServerContainerCmd(port),
					nodeSelector: map[string]string{nodeHostnameKey: workerTwoNodeName},
				},
			),
		)

		table.DescribeTable(
			"multi-network policies configure traffic allow lists",
			func(netConfig networkAttachmentConfig, allowedClientPodConfig podConfiguration, blockedClientPodConfig podConfiguration, serverPodConfig podConfiguration, policy *mnpapi.MultiNetworkPolicy) {
				netConfig.namespace = f.Namespace.Name
				blockedClientPodConfig.namespace = f.Namespace.Name
				allowedClientPodConfig.namespace = f.Namespace.Name
				serverPodConfig.namespace = f.Namespace.Name

				By("creating the attachment configuration")
				_, err := nadClient.NetworkAttachmentDefinitions(f.Namespace.Name).Create(
					context.Background(),
					generateNAD(netConfig),
					metav1.CreateOptions{},
				)
				Expect(err).NotTo(HaveOccurred())

				By("provisioning the multi-network policy")
				_, err = mnpClient.MultiNetworkPolicies(f.Namespace.Name).Create(
					context.Background(),
					policy,
					metav1.CreateOptions{},
				)
				Expect(err).To(Succeed())

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
					updatedPod, err := cs.CoreV1().Pods(serverPodConfig.namespace).Get(context.Background(), serverPod.GetName(), metav1.GetOptions{})
					if err != nil {
						return v1.PodFailed
					}
					return updatedPod.Status.Phase
				}, 2*time.Minute, 6*time.Second).Should(Equal(v1.PodRunning))

				By("instantiating the *allowed-client* pod")
				allowedClient, err := cs.CoreV1().Pods(allowedClientPodConfig.namespace).Create(
					context.Background(),
					generatePodSpec(allowedClientPodConfig),
					metav1.CreateOptions{},
				)
				Expect(err).NotTo(HaveOccurred())

				By("asserting the *allowed-client* pod reaches the `Ready` state")
				Eventually(func() v1.PodPhase {
					updatedPod, err := cs.CoreV1().Pods(allowedClientPodConfig.namespace).Get(context.Background(), allowedClient.GetName(), metav1.GetOptions{})
					if err != nil {
						return v1.PodFailed
					}
					return updatedPod.Status.Phase
				}, 2*time.Minute, 6*time.Second).Should(Equal(v1.PodRunning))

				By("instantiating the *blocked-client* pod")
				blockedClient, err := cs.CoreV1().Pods(blockedClientPodConfig.namespace).Create(
					context.Background(),
					generatePodSpec(blockedClientPodConfig),
					metav1.CreateOptions{},
				)
				Expect(err).NotTo(HaveOccurred())

				By("asserting the *blocked-client* pod reaches the `Ready` state")
				Eventually(func() v1.PodPhase {
					updatedPod, err := cs.CoreV1().Pods(blockedClientPodConfig.namespace).Get(context.Background(), blockedClient.GetName(), metav1.GetOptions{})
					if err != nil {
						return v1.PodFailed
					}
					return updatedPod.Status.Phase
				}, 2*time.Minute, 6*time.Second).Should(Equal(v1.PodRunning))

				By("asserting the server pod has an IP from the configured range")
				serverIP, err := podIPForAttachment(cs, serverPodConfig.namespace, serverPod.GetName(), netConfig.name, 0)
				Expect(err).NotTo(HaveOccurred())
				By(fmt.Sprintf("asserting the server pod IP %v is from the configured range %v/%v", serverIP, netConfig.cidr, netPrefixLengthPerNode))
				subnet, err := getNetCIDRSubnet(netConfig.cidr)
				Expect(err).NotTo(HaveOccurred())
				Expect(inRange(subnet, serverIP)).To(Succeed())

				By("asserting the *allowed-client* pod can contact the server pod exposed endpoint")
				Eventually(func() error {
					updatedPod, err := cs.CoreV1().Pods(serverPodConfig.namespace).Get(context.Background(), serverPod.GetName(), metav1.GetOptions{})
					if err != nil {
						return err
					}

					if updatedPod.Status.Phase == v1.PodRunning {
						return connectToServer(allowedClientPodConfig.namespace, allowedClientPodConfig.name, serverIP, port)
					}

					return fmt.Errorf("pod not running. /me is sad")
				}, 2*time.Minute, 6*time.Second).Should(Succeed())

				By("asserting the *blocked-client* pod **cannot** contact the server pod exposed endpoint")
				Expect(connectToServer(blockedClientPodConfig.namespace, blockedClientPodConfig.name, serverIP, port)).To(
					MatchError(
						MatchRegexp("Connection timeout after 200[0-1] ms")))
			},
			table.Entry(
				"for a pure L2 overlay",
				networkAttachmentConfig{
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
			table.Entry(
				"for a routed topology",
				networkAttachmentConfig{
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
		)
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

func netCIDR(netCIDR string, netPrefixLengthPerNode int) string {
	return fmt.Sprintf("%s/%d", netCIDR, netPrefixLengthPerNode)
}

func getNetCIDRSubnet(netCIDR string) (string, error) {
	subStrings := strings.Split(netCIDR, "/")
	if len(subStrings) == 3 {
		return subStrings[0] + "/" + subStrings[1], nil
	} else if len(subStrings) == 2 {
		return netCIDR, nil
	}
	return "", fmt.Errorf("invalid network cidr %s", netCIDR)
}

type networkAttachmentConfig struct {
	cidr         string
	excludeCIDRs []string
	namespace    string
	name         string
	topology     string
	networkName  string
	vlanID       int
}

func (nac networkAttachmentConfig) attachmentName() string {
	if nac.networkName != "" {
		return nac.networkName
	}
	return uniqueNadName(nac.name)
}

func uniqueNadName(originalNetName string) string {
	const randomStringLength = 5
	return fmt.Sprintf("%s_%s", rand.String(randomStringLength), originalNetName)
}

func generateNAD(config networkAttachmentConfig) *nadapi.NetworkAttachmentDefinition {
	nadSpec := fmt.Sprintf(
		`
{
        "cniVersion": "0.3.0",
        "name": %q,
        "type": "ovn-k8s-cni-overlay",
        "topology":%q,
        "subnets": %q,
        "excludeSubnets": %q,
        "mtu": 1300,
        "netAttachDefName": %q,
        "vlanID": %d
}
`,
		config.attachmentName(),
		config.topology,
		config.cidr,
		strings.Join(config.excludeCIDRs, ","),
		namespacedName(config.namespace, config.name),
		config.vlanID,
	)
	return generateNetAttachDef(config.namespace, config.name, nadSpec)
}

func generateNetAttachDef(namespace, nadName, nadSpec string) *nadapi.NetworkAttachmentDefinition {
	return &nadapi.NetworkAttachmentDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name:      nadName,
			Namespace: namespace,
		},
		Spec: nadapi.NetworkAttachmentDefinitionSpec{Config: nadSpec},
	}
}

type podConfiguration struct {
	attachments  []nadapi.NetworkSelectionElement
	containerCmd []string
	name         string
	namespace    string
	nodeSelector map[string]string
	isPrivileged bool
	labels       map[string]string
}

func generatePodSpec(config podConfiguration) *v1.Pod {
	podSpec := e2epod.NewAgnhostPod(config.namespace, config.name, nil, nil, nil, config.containerCmd...)
	podSpec.Annotations = networkSelectionElements(config.attachments...)
	podSpec.Spec.NodeSelector = config.nodeSelector
	podSpec.Labels = config.labels
	if config.isPrivileged {
		privileged := true
		podSpec.Spec.Containers[0].SecurityContext.Privileged = &privileged
	}
	return podSpec
}

func networkSelectionElements(elements ...nadapi.NetworkSelectionElement) map[string]string {
	marshalledElements, err := json.Marshal(elements)
	if err != nil {
		panic(fmt.Errorf("programmer error: you've provided wrong input to the test data: %v", err))
	}
	return map[string]string{
		nadapi.NetworkAttachmentAnnot: string(marshalledElements),
	}
}

func httpServerContainerCmd(port uint16) []string {
	return []string{"netexec", "--http-port", fmt.Sprintf("%d", port)}
}

func podNetworkStatus(pod *v1.Pod, predicates ...func(nadapi.NetworkStatus) bool) ([]nadapi.NetworkStatus, error) {
	podNetStatus, found := pod.Annotations[nadapi.NetworkStatusAnnot]
	if !found {
		return nil, fmt.Errorf("the pod must feature the `networks-status` annotation")
	}

	var netStatus []nadapi.NetworkStatus
	if err := json.Unmarshal([]byte(podNetStatus), &netStatus); err != nil {
		return nil, err
	}

	var netStatusMeetingPredicates []nadapi.NetworkStatus
	for i := range netStatus {
		for _, predicate := range predicates {
			if predicate(netStatus[i]) {
				netStatusMeetingPredicates = append(netStatusMeetingPredicates, netStatus[i])
				continue
			}
		}
	}
	return netStatusMeetingPredicates, nil
}

func namespacedName(namespace, name string) string {
	return fmt.Sprintf("%s/%s", namespace, name)
}

func inRange(cidr string, ip string) error {
	_, cidrRange, err := net.ParseCIDR(cidr)
	if err != nil {
		return err
	}

	if cidrRange.Contains(net.ParseIP(ip)) {
		return nil
	}

	return fmt.Errorf("ip [%s] is NOT in range %s", ip, cidr)
}

func connectToServer(clientPodNs string, clientPodName, serverIP string, port int) error {
	_, err := framework.RunKubectl(
		clientPodNs,
		"exec",
		clientPodName,
		"--",
		"curl",
		"--connect-timeout",
		"2",
		net.JoinHostPort(serverIP, fmt.Sprintf("%d", port)),
	)
	return err
}

func newAttachmentConfigWithOverriddenName(name, namespace, networkName, topology, cidr string) networkAttachmentConfig {
	return networkAttachmentConfig{
		cidr:        cidr,
		name:        name,
		namespace:   namespace,
		networkName: networkName,
		topology:    topology,
	}
}

func configurePodStaticIP(podNamespace string, podName string, staticIP string) error {
	_, err := framework.RunKubectl(
		podNamespace, "exec", podName, "--",
		"ip", "addr", "add", staticIP, "dev", "net1",
	)
	return err
}

func areStaticIPsConfiguredViaCNI(podConfig podConfiguration) bool {
	for _, attachment := range podConfig.attachments {
		if len(attachment.IPRequest) > 0 {
			return true
		}
	}
	return false
}

func podIPForAttachment(k8sClient clientset.Interface, podNamespace string, podName string, attachmentName string, ipIndex int) (string, error) {
	pod, err := k8sClient.CoreV1().Pods(podNamespace).Get(context.Background(), podName, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	netStatus, err := podNetworkStatus(pod, func(status nadapi.NetworkStatus) bool {
		return status.Name == namespacedName(podNamespace, attachmentName)
	})
	if err != nil {
		return "", err
	}
	if len(netStatus) != 1 {
		return "", fmt.Errorf("more than one status entry for attachment %s on pod %s", attachmentName, namespacedName(podNamespace, podName))
	}
	if len(netStatus[0].IPs) == 0 {
		return "", fmt.Errorf("no IPs for attachment %s on pod %s", attachmentName, namespacedName(podNamespace, podName))
	}
	return netStatus[0].IPs[ipIndex], nil
}

func allowedClient(podName string) string {
	return "allowed-" + podName
}

func blockedClient(podName string) string {
	return "blocked-" + podName
}

func multiNetIngressLimitingPolicy(policyFor string, appliesFor metav1.LabelSelector, allowForSelector metav1.LabelSelector, allowPorts ...int) *mnpapi.MultiNetworkPolicy {
	var (
		portAllowlist []mnpapi.MultiNetworkPolicyPort
	)
	tcp := v1.ProtocolTCP
	for _, port := range allowPorts {
		p := intstr.FromInt(port)
		portAllowlist = append(portAllowlist, mnpapi.MultiNetworkPolicyPort{
			Protocol: &tcp,
			Port:     &p,
		})
	}
	return &mnpapi.MultiNetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
			PolicyForAnnotation: policyFor,
		},
			Name: "allow-ports-via-pod-selector",
		},

		Spec: mnpapi.MultiNetworkPolicySpec{
			PodSelector: appliesFor,
			Ingress: []mnpapi.MultiNetworkPolicyIngressRule{
				{
					Ports: portAllowlist,
					From: []mnpapi.MultiNetworkPolicyPeer{
						{
							PodSelector: &allowForSelector,
						},
					},
				},
			},
			PolicyTypes: []mnpapi.MultiPolicyType{mnpapi.PolicyTypeIngress},
		},
	}
}
