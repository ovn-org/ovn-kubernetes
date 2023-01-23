package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"time"

	. "github.com/onsi/ginkgo"
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
		podName              = "tinypod"
		secondaryNetworkCIDR = "10.128.0.0/16"
		secondaryNetworkName = "tenant-blue"
	)
	f := wrappedTestFramework("multi-homing")

	var (
		attachmentConfig *nadapi.NetworkAttachmentDefinition
		cs               clientset.Interface
		nadClient        nadclient.K8sCniCncfIoV1Interface
		pod              *v1.Pod
	)

	BeforeEach(func() {
		cs = f.ClientSet

		var err error
		nadClient, err = nadclient.NewForConfig(f.ClientConfig())
		Expect(err).NotTo(HaveOccurred())
	})

	JustBeforeEach(func() {
		_, err := nadClient.NetworkAttachmentDefinitions(f.Namespace.Name).Create(
			context.Background(),
			attachmentConfig,
			metav1.CreateOptions{},
		)
		Expect(err).NotTo(HaveOccurred())
	})

	Context("Layer 3 - routed - topology", func() {
		BeforeEach(func() {
			attachmentConfig = generateLayer3SecondaryOvnNetwork(f.Namespace.Name, secondaryNetworkName, netCIDR(secondaryNetworkCIDR, 24))
		})

		Context("A single pod with an OVN-K secondary network", func() {
			JustBeforeEach(func() {
				var err error

				pod, err = cs.CoreV1().Pods(f.Namespace.Name).Create(
					context.Background(),
					generatePodSpec(f.Namespace.Name, podName, nil, secondaryNetworkName),
					metav1.CreateOptions{},
				)
				Expect(err).NotTo(HaveOccurred())
			})

			It("is able to get to the Running phase", func() {
				Eventually(func() v1.PodPhase {
					updatedPod, err := cs.CoreV1().Pods(f.Namespace.Name).Get(context.Background(), pod.GetName(), metav1.GetOptions{})
					if err != nil {
						return v1.PodFailed
					}
					return updatedPod.Status.Phase
				}, 2*time.Minute, 6*time.Second).Should(Equal(v1.PodRunning))
			})
		})

		Context("multiple pods", func() {
			const (
				port = 9000
			)

			var (
				serverPod *v1.Pod
				serverIP  string
			)

			JustBeforeEach(func() {
				var err error
				serverPod, err = cs.CoreV1().Pods(f.Namespace.Name).Create(
					context.Background(),
					generatePodSpec(f.Namespace.Name, podName, httpServerContainerCmd(port), secondaryNetworkName),
					metav1.CreateOptions{},
				)
				Expect(err).NotTo(HaveOccurred())
				Expect(serverPod).NotTo(BeNil())

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
				Expect(netStatus[0].IPs).NotTo(BeEmpty())

				serverIP = netStatus[0].IPs[0]
				Expect(inRange(secondaryNetworkCIDR, serverIP)).To(Succeed())
			})

			It("can communicate over the secondary network", func() {
				const clientPodName = "client-pod"
				clientPod, err := cs.CoreV1().Pods(f.Namespace.Name).Create(
					context.Background(),
					generatePodSpec(f.Namespace.Name, clientPodName, nil, secondaryNetworkName),
					metav1.CreateOptions{},
				)
				Expect(err).NotTo(HaveOccurred())

				Eventually(func() error {
					updatedPod, err := cs.CoreV1().Pods(f.Namespace.Name).Get(context.Background(), serverPod.GetName(), metav1.GetOptions{})
					if err != nil {
						return err
					}

					if updatedPod.Status.Phase == v1.PodRunning {
						_, err = framework.RunKubectl(
							clientPod.GetNamespace(),
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

					return fmt.Errorf("pod not running. /me is sad")
				}, 2*time.Minute, 6*time.Second).Should(Succeed())
			})
		})
	})

	Context("Layer 2 - switched - topology", func() {
		const (
			secondaryNetworkCIDR = "10.128.1.0/24"
			secondaryNetworkName = "flat-network-tenant-blue"
			someExcludedCIDR     = "10.128.1.0/29"
		)

		BeforeEach(func() {
			attachmentConfig = generateSwitchedSecondaryOvnNetwork(
				f.Namespace.Name,
				secondaryNetworkName,
				secondaryNetworkCIDR,
				someExcludedCIDR,
			)
		})

		Context("A single pod with an OVN-K secondary network", func() {
			JustBeforeEach(func() {
				var err error

				pod, err = cs.CoreV1().Pods(f.Namespace.Name).Create(
					context.Background(),
					generatePodSpec(f.Namespace.Name, podName, nil, secondaryNetworkName),
					metav1.CreateOptions{},
				)
				Expect(err).NotTo(HaveOccurred())
			})

			It("is able to get to the Running phase", func() {
				Eventually(func() v1.PodPhase {
					updatedPod, err := cs.CoreV1().Pods(f.Namespace.Name).Get(context.Background(), pod.GetName(), metav1.GetOptions{})
					if err != nil {
						return v1.PodFailed
					}
					return updatedPod.Status.Phase
				}, 2*time.Minute, 6*time.Second).Should(Equal(v1.PodRunning))
			})
		})

		Context("multiple pods", func() {
			const port = 9000

			var (
				serverPod *v1.Pod
				serverIP  string
			)

			JustBeforeEach(func() {
				var err error
				serverPod, err = cs.CoreV1().Pods(f.Namespace.Name).Create(
					context.Background(),
					generatePodSpec(f.Namespace.Name, podName, httpServerContainerCmd(port), secondaryNetworkName),
					metav1.CreateOptions{},
				)
				Expect(err).NotTo(HaveOccurred())
				Expect(serverPod).NotTo(BeNil())

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
				Expect(netStatus[0].IPs).NotTo(BeEmpty())

				serverIP = netStatus[0].IPs[0]
				Expect(inRange(secondaryNetworkCIDR, serverIP)).To(Succeed())
			})

			It("can communicate over the secondary network", func() {
				const clientPodName = "client-pod"
				clientPod, err := cs.CoreV1().Pods(f.Namespace.Name).Create(
					context.Background(),
					generatePodSpec(f.Namespace.Name, clientPodName, nil, secondaryNetworkName),
					metav1.CreateOptions{},
				)
				Expect(err).NotTo(HaveOccurred())

				Eventually(func() error {
					updatedPod, err := cs.CoreV1().Pods(f.Namespace.Name).Get(context.Background(), serverPod.GetName(), metav1.GetOptions{})
					if err != nil {
						return err
					}

					if updatedPod.Status.Phase == v1.PodRunning {
						_, err = framework.RunKubectl(
							clientPod.GetNamespace(),
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

					return fmt.Errorf("pod not running. /me is sad")
				}, 2*time.Minute, 6*time.Second).Should(Succeed())
			})
		})
	})
})

func netCIDR(netCIDR string, netPrefixLengthPerNode int) string {
	return fmt.Sprintf("%s/%d", netCIDR, netPrefixLengthPerNode)
}

func generateLayer3SecondaryOvnNetwork(namespace string, name string, cidr string) *nadapi.NetworkAttachmentDefinition {
	nadSpec := fmt.Sprintf(`
{
        "cniVersion": "0.3.0",
        "name": %q,
        "type": "ovn-k8s-cni-overlay",
        "topology":"layer3",
        "subnets": %q,
        "mtu": 1300,
        "netAttachDefName": %q
}
`, name, cidr, fmt.Sprintf("%s/%s", namespace, name))
	return generateNetAttachDef(namespace, name, nadSpec)
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

func generatePodSpec(namespace, podName string, containerCmd []string, networkNames ...string) *v1.Pod {
	podSpec := e2epod.NewAgnhostPod(namespace, podName, nil, nil, nil, containerCmd...)
	podSpec.Annotations = networkSelectionElements(networkNames...)
	return podSpec
}

func networkSelectionElements(networkNames ...string) map[string]string {
	return map[string]string{
		nadapi.NetworkAttachmentAnnot: strings.Join(networkNames, ","),
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

func generateSwitchedSecondaryOvnNetwork(namespace string, name string, cidr string, excludeCIDRs ...string) *nadapi.NetworkAttachmentDefinition {
	// TODO: optional `excludeSubnets` parameter
	nadSpec := fmt.Sprintf(`
{
        "cniVersion": "0.3.0",
        "name": %q,
        "type": "ovn-k8s-cni-overlay",
        "topology":"layer2",
        "subnets": %q,
        "excludeSubnets": %q,
        "mtu": 1300,
        "netAttachDefName": %q
}
`, name, cidr, strings.Join(excludeCIDRs, ","), fmt.Sprintf("%s/%s", namespace, name))
	return generateNetAttachDef(namespace, name, nadSpec)
}
