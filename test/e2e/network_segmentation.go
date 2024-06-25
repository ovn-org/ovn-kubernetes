package e2e

import (
	"context"
	"fmt"
	"strings"
	"time"

	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	nadclient "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/typed/k8s.cni.cncf.io/v1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
)

var _ = Describe("Network Segmentation", func() {
	f := wrappedTestFramework("network-segmentation")
	Context("a user defined primary network", func() {
		const (
			externalServiceIPv4IP        = "10.128.0.1"
			nodeHostnameKey              = "kubernetes.io/hostname"
			port                         = 9000
			userDefinedNetworkIPv4Subnet = "10.128.0.0/16"
			userDefinedNetworkIPv6Subnet = "2014:100:200::0/60"
			userDefinedNetworkName       = "hogwarts"
			nadName                      = "gryffindor"
			workerOneNodeName            = "ovn-worker"
			workerTwoNodeName            = "ovn-worker2"
		)

		var (
			cs        clientset.Interface
			nadClient nadclient.K8sCniCncfIoV1Interface
		)

		BeforeEach(func() {
			cs = f.ClientSet

			var err error
			nadClient, err = nadclient.NewForConfig(f.ClientConfig())
			Expect(err).NotTo(HaveOccurred())
		})

		DescribeTable(
			"can perform east/west traffic between nodes",
			func(
				netConfigParams networkAttachmentConfigParams,
				clientPodConfig podConfiguration,
				serverPodConfig podConfiguration,
			) {
				netConfig := newNetworkAttachmentConfig(netConfigParams)

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

				var serverIP string
				for i, cidr := range strings.Split(netConfig.cidr, ",") {
					if cidr != "" {
						By("asserting the server pod has an IP from the configured range")
						serverIP, err = podIPsForUserDefinedPrimaryNetwork(
							cs,
							f.Namespace.Name,
							serverPod.GetName(),
							namespacedName(serverPod.Namespace, netConfig.name),
							i,
						)
						Expect(err).NotTo(HaveOccurred())
						const netPrefixLengthPerNode = 24
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
			Entry(
				"two pods connected over a L2 dualstack primary UDN",
				networkAttachmentConfigParams{
					name:           nadName,
					networkName:    userDefinedNetworkName,
					topology:       "layer2",
					cidr:           fmt.Sprintf("%s,%s", userDefinedNetworkIPv4Subnet, userDefinedNetworkIPv6Subnet),
					excludeCIDRs:   []string{externalServiceIPv4IP + "/32"},
					primaryNetwork: true,
				},
				*podConfig(
					"client-pod",
					nadName,
					withNodeSelector(map[string]string{nodeHostnameKey: workerOneNodeName}),
				),
				*podConfig(
					"server-pod",
					nadName,
					withCommand(func() []string {
						return httpServerContainerCmd(port)
					}),
					withNodeSelector(map[string]string{nodeHostnameKey: workerTwoNodeName}),
				),
			),
			Entry(
				"two pods connected over a L3 dualstack primary UDN",
				networkAttachmentConfigParams{
					name:           nadName,
					networkName:    userDefinedNetworkName,
					topology:       "layer3",
					cidr:           fmt.Sprintf("%s,%s", userDefinedNetworkIPv4Subnet, userDefinedNetworkIPv6Subnet),
					excludeCIDRs:   []string{externalServiceIPv4IP + "/32"},
					primaryNetwork: true,
				},
				*podConfig(
					"client-pod",
					nadName,
					withNodeSelector(map[string]string{nodeHostnameKey: workerOneNodeName}),
				),
				*podConfig(
					"server-pod",
					nadName,
					withCommand(func() []string {
						return httpServerContainerCmd(port)
					}),
					withNodeSelector(map[string]string{nodeHostnameKey: workerTwoNodeName}),
				),
			),
		)
	})
})

type podOption func(*podConfiguration)

func podConfig(podName, nadName string, opts ...podOption) *podConfiguration {
	pod := &podConfiguration{
		attachments: []nadapi.NetworkSelectionElement{{Name: nadName}},
		name:        podName,
	}
	for _, opt := range opts {
		opt(pod)
	}
	return pod
}

func withCommand(cmdGenerationFn func() []string) podOption {
	return func(pod *podConfiguration) {
		pod.containerCmd = cmdGenerationFn()
	}
}

func withNodeSelector(nodeSelector map[string]string) podOption {
	return func(pod *podConfiguration) {
		pod.nodeSelector = nodeSelector
	}
}

// podIPsForUserDefinedPrimaryNetwork returns the v4 or v6 IPs for a pod on the UDN
func podIPsForUserDefinedPrimaryNetwork(k8sClient clientset.Interface, podNamespace string, podName string, attachmentName string, index int) (string, error) {
	pod, err := k8sClient.CoreV1().Pods(podNamespace).Get(context.Background(), podName, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	netStatus, err := userDefinedNetworkStatus(pod, attachmentName)
	if err != nil {
		return "", err
	}

	if len(netStatus.IPs) == 0 {
		return "", fmt.Errorf("attachment for network %q without IPs", attachmentName)
	}
	if len(netStatus.IPs) > 2 {
		return "", fmt.Errorf("attachment for network %q with more than two IPs", attachmentName)
	}
	return netStatus.IPs[index].IP.String(), nil
}

func userDefinedNetworkStatus(pod *v1.Pod, networkName string) (PodAnnotation, error) {
	netStatus, err := unmarshalPodAnnotation(pod.Annotations, networkName)
	if err != nil {
		return PodAnnotation{}, fmt.Errorf("failed to unmarshall annotations for pod %q: %v", pod.Name, err)
	}

	return *netStatus, nil
}
