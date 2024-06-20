package e2e

import (
	"context"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"

	nadclient "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/typed/k8s.cni.cncf.io/v1"
)

var _ = Describe("Network Segmentation", func() {
	const (
		activeNetworkAnnotation = "k8s.ovn.org/active-network"
	)

	f := wrappedTestFramework("network-segmentation")

	type activeNetworkTest struct {
		nads                  []networkAttachmentConfigParams
		podsBeforeNADs        []podConfiguration
		podsAfterNADs         []podConfiguration
		expectedActiveNetwork string
		deleteNADs            []string
	}
	DescribeTable("should annotate namespace with proper active-network", func(td activeNetworkTest) {
		nadClient, err := nadclient.NewForConfig(f.ClientConfig())
		Expect(err).NotTo(HaveOccurred())

		By("Create pods before network attachment definition")
		podsBeforeNADs := []*corev1.Pod{}
		for _, pod := range td.podsBeforeNADs {
			pod, err := f.ClientSet.CoreV1().Pods(f.Namespace.Name).Create(
				context.Background(),
				generatePodSpec(pod),
				metav1.CreateOptions{},
			)
			Expect(err).NotTo(HaveOccurred())
			podsBeforeNADs = append(podsBeforeNADs, pod)
		}

		By("Create network attachment definitions")
		for _, nad := range td.nads {
			netConfig := newNetworkAttachmentConfig(nad)
			netConfig.namespace = f.Namespace.Name

			_, err = nadClient.NetworkAttachmentDefinitions(netConfig.namespace).Create(
				context.Background(),
				generateNAD(netConfig),
				metav1.CreateOptions{},
			)
			Expect(err).NotTo(HaveOccurred())
		}

		By("Create pods after network attachment definition")
		podsAfterNADs := []*corev1.Pod{}
		for _, pod := range td.podsAfterNADs {
			pod, err := f.ClientSet.CoreV1().Pods(f.Namespace.Name).Create(
				context.Background(),
				generatePodSpec(pod),
				metav1.CreateOptions{},
			)
			Expect(err).NotTo(HaveOccurred())
			podsAfterNADs = append(podsAfterNADs, pod)
		}
		for _, nadNameToDelete := range td.deleteNADs {
			Expect(nadClient.NetworkAttachmentDefinitions(f.Namespace.Name).Delete(
				context.Background(),
				nadNameToDelete,
				metav1.DeleteOptions{},
			)).To(Succeed())
		}

		By("Wait for expected active-network annotation")
		activeNetworkMatch := HaveKeyWithValue(activeNetworkAnnotation, td.expectedActiveNetwork)
		if td.expectedActiveNetwork == "" {
			activeNetworkMatch = Not(HaveKey(activeNetworkAnnotation))
		}

		Eventually(thisNamespace(f.ClientSet, f.Namespace)).
			WithPolling(time.Second / 2).
			WithTimeout(5 * time.Second).
			Should(WithTransform(getAnnotations, activeNetworkMatch))

	},
		Entry("without primary network nads to 'default'", activeNetworkTest{
			nads:                  []networkAttachmentConfigParams{},
			expectedActiveNetwork: "default",
		}),
		Entry("with one primaryNetwork nad on layer2 to network name", activeNetworkTest{
			nads: []networkAttachmentConfigParams{{
				name:           "tenant-blue-l2",
				networkName:    "net-l2",
				cidr:           "10.128.0.0/24",
				topology:       "layer2",
				primaryNetwork: true,
			}},
			expectedActiveNetwork: "net-l2",
		}),
		Entry("with one primaryNetwork nad on layer3 to network name", activeNetworkTest{
			nads: []networkAttachmentConfigParams{{
				name:           "tenant-blue-l3",
				networkName:    "net-l3",
				cidr:           "10.128.0.0/16/24",
				topology:       "layer3",
				primaryNetwork: true,
			}},
			expectedActiveNetwork: "net-l3",
		}),
		Entry("with two primaryNetwork nads on layer3 and same network with network name", activeNetworkTest{
			nads: []networkAttachmentConfigParams{
				{
					name:           "tenant-blue-l3",
					networkName:    "net-l3",
					cidr:           "10.128.0.0/16/24",
					topology:       "layer3",
					primaryNetwork: true,
				},
				{
					name:           "tenant-red-l3",
					networkName:    "net-l3",
					cidr:           "10.128.0.0/16/24",
					topology:       "layer3",
					primaryNetwork: true,
				},
			},
			expectedActiveNetwork: "net-l3",
		}),
		Entry("with two primaryNetwork nads on layer2 and same network with network name", activeNetworkTest{
			nads: []networkAttachmentConfigParams{
				{
					name:           "tenant-blue-l2",
					networkName:    "net-l2",
					cidr:           "10.128.0.0/24",
					topology:       "layer2",
					primaryNetwork: true,
				},
				{
					name:           "tenant-red-l2",
					networkName:    "net-l2",
					cidr:           "10.128.0.0/24",
					topology:       "layer2",
					primaryNetwork: true,
				},
			},
			expectedActiveNetwork: "net-l2",
		}),
		Entry("with two primaryNetwork nads and different network with 'unknown'", activeNetworkTest{
			nads: []networkAttachmentConfigParams{
				{
					name:           "tenant-blue-l3",
					networkName:    "net-l3",
					cidr:           "10.128.0.0/16/24",
					topology:       "layer3",
					primaryNetwork: true,
				},
				{
					name:           "tenant-blue-l2",
					networkName:    "net-l2",
					cidr:           "10.128.0.0/24",
					topology:       "layer2",
					primaryNetwork: true,
				},
			},
			expectedActiveNetwork: "unknown",
		}),
		Entry("with one primaryNetwork nad pods at the namespace with 'unknown'", activeNetworkTest{
			podsBeforeNADs: []podConfiguration{{
				name: "pod1",
			}},
			nads: []networkAttachmentConfigParams{{
				name:           "tenant-blue-l2",
				networkName:    "net-l2",
				cidr:           "10.128.0.0/24",
				topology:       "layer2",
				primaryNetwork: true,
			}},
			expectedActiveNetwork: "unknown",
		}),
		Entry("with two primaryNetwork nads and different network created and then one delete with networkName", activeNetworkTest{
			nads: []networkAttachmentConfigParams{
				{
					name:           "tenant-blue-l3",
					networkName:    "net-l3",
					cidr:           "10.128.0.0/16/24",
					topology:       "layer3",
					primaryNetwork: true,
				},
				{
					name:           "tenant-blue-l2",
					networkName:    "net-l2",
					cidr:           "10.128.0.0/24",
					topology:       "layer2",
					primaryNetwork: true,
				},
			},
			deleteNADs:            []string{"tenant-blue-l3"},
			expectedActiveNetwork: "net-l2",
		}),
		Entry("with one primaryNetwork nad deleted and no pods should annotate back to 'default'", activeNetworkTest{
			nads: []networkAttachmentConfigParams{
				{
					name:           "tenant-blue-l3",
					networkName:    "net-l3",
					cidr:           "10.128.0.0/16/24",
					topology:       "layer3",
					primaryNetwork: true,
				},
			},
			deleteNADs:            []string{"tenant-blue-l3"},
			expectedActiveNetwork: "default",
		}),
		Entry("with one primaryNetwork nad deleted and pods should annotate to 'unknown'", activeNetworkTest{
			nads: []networkAttachmentConfigParams{
				{
					name:           "tenant-blue-l3",
					networkName:    "net-l3",
					cidr:           "10.128.0.0/16/24",
					topology:       "layer3",
					primaryNetwork: true,
				},
			},
			podsAfterNADs:         []podConfiguration{{name: "pod1"}},
			deleteNADs:            []string{"tenant-blue-l3"},
			expectedActiveNetwork: "unknown",
		}),
	)

	Context("a user defined primary network", func() {
		const (
			externalServiceIPv4IP        = "10.128.0.1"
			nodeHostnameKey              = "kubernetes.io/hostname"
			port                         = 9000
			userDefinedNetworkIPv4Subnet = "10.128.0.0/16"
			userDefinedNetworkIPv6Subnet = "2014:100:200::0/60"
			userDefinedNetworkName       = "tenantblue"
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

				serverIP := ""
				for _, cidr := range strings.Split(netConfig.cidr, ",") {
					if cidr != "" {
						By("asserting the server pod has an IP from the configured range")
						serverIP, err = podIPForUserDefinedPrimaryNetwork(
							cs,
							f.Namespace.Name,
							serverPod.GetName(),
							namespacedName(serverPod.Namespace, netConfig.name),
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
				"two pods connected over an IPv4 network",
				networkAttachmentConfigParams{
					name:           userDefinedNetworkName,
					networkName:    userDefinedNetworkName,
					topology:       "layer2",
					cidr:           userDefinedNetworkIPv4Subnet,
					excludeCIDRs:   []string{externalServiceIPv4IP + "/32"},
					primaryNetwork: true,
				},
				*podConfig(
					"client-pod",
					withNodeSelector(map[string]string{nodeHostnameKey: workerOneNodeName}),
				),
				*podConfig(
					"server-pod",
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

func podConfig(podName string, opts ...podOption) *podConfiguration {
	pod := &podConfiguration{
		name: podName,
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

func podIPForUserDefinedPrimaryNetwork(k8sClient clientset.Interface, podNamespace string, podName string, attachmentName string) (string, error) {
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
	if len(netStatus.IPs) > 1 {
		return "", fmt.Errorf("attachment for network %q with more than one IP", attachmentName)
	}
	return netStatus.IPs[0].IP.String(), nil
}

func userDefinedNetworkStatus(pod *v1.Pod, networkName string) (PodAnnotation, error) {
	netStatus, err := unmarshalPodAnnotation(pod.Annotations, networkName)
	if err != nil {
		return PodAnnotation{}, fmt.Errorf("failed to unmarshall annotations for pod %q: %v", pod.Name, err)
	}

	return *netStatus, nil
}
