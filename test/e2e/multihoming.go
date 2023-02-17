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
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"

	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	nadclient "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/typed/k8s.cni.cncf.io/v1"
)

var _ = Describe("Multi Homing", func() {
	const (
		podName                    = "tinypod"
		secondaryNetworkCIDR       = "10.128.0.0/16"
		secondaryNetworkName       = "tenant-blue"
		secondaryFlatL2IgnoreCIDR  = "10.128.0.0/29"
		secondaryFlatL2NetworkCIDR = "10.128.0.0/24"
	)
	f := wrappedTestFramework("multi-homing")

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
		},
			table.Entry(
				"when attaching to an L3 - routed - network",
				networkAttachmentConfig{
					cidr:     netCIDR(secondaryNetworkCIDR, 24),
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
		)
	})

	Context("multiple pods connected to the same OVN-K secondary network", func() {
		const (
			clientPodName     = "client-pod"
			nodeHostnameKey   = "kubernetes.io/hostname"
			port              = 9000
			workerOneNodeName = "ovn-worker"
			workerTwoNodeName = "ovn-worker2"
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

				pod, err := cs.CoreV1().Pods(f.Namespace.Name).Get(context.Background(), serverPod.GetName(), metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())

				netStatus, err := podNetworkStatus(pod, func(status nadapi.NetworkStatus) bool {
					return status.Name == namespacedName(f.Namespace.Name, secondaryNetworkName)
				})
				Expect(err).NotTo(HaveOccurred())
				Expect(netStatus).To(HaveLen(1))

				serverIP := ""
				if netConfig.cidr != "" {
					By("asserting the server pod has an IP from the configured range")
					Expect(netStatus[0].IPs).NotTo(BeEmpty())
					serverIP = netStatus[0].IPs[0]
					Expect(inRange(secondaryNetworkCIDR, serverIP)).To(Succeed())
				}

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

				if netConfig.cidr == "" {
					By("configuring static IP addresses in the pods")
					const (
						clientIP       = "192.168.200.10/24"
						staticServerIP = "192.168.200.20/24"
					)
					Expect(configurePodStaticIP(f.Namespace.Name, clientPodName, clientIP)).To(Succeed())
					Expect(configurePodStaticIP(f.Namespace.Name, serverPod.GetName(), staticServerIP)).To(Succeed())
					serverIP = strings.ReplaceAll(staticServerIP, "/24", "")
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
					cidr:     netCIDR(secondaryNetworkCIDR, 24),
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

type networkAttachmentConfig struct {
	cidr         string
	excludeCIDRs []string
	namespace    string
	name         string
	topology     string
	networkName  string
}

func (nac networkAttachmentConfig) attachmentName() string {
	if nac.networkName != "" {
		return nac.networkName
	}
	return nac.name
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
        "netAttachDefName": %q
}
`,
		config.attachmentName(),
		config.topology,
		config.cidr,
		strings.Join(config.excludeCIDRs, ","),
		namespacedName(config.namespace, config.name),
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
}

func generatePodSpec(config podConfiguration) *v1.Pod {
	podSpec := e2epod.NewAgnhostPod(config.namespace, config.name, nil, nil, nil, config.containerCmd...)
	podSpec.Annotations = networkSelectionElements(config.attachments...)
	podSpec.Spec.NodeSelector = config.nodeSelector
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
		podNamespace,
		"exec",
		podName,
		"--",
		"ip",
		"addr",
		"add",
		staticIP,
		"dev",
		"net1",
	)
	return err
}
