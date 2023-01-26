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
		table.DescribeTable("is able to get to the Running phase", func(generatorFn attachmentGeneratorFn, cidr string, excludeCIDRs ...string) {
			netAttachDef := generatorFn(f.Namespace.Name, secondaryNetworkName, cidr, excludeCIDRs...)
			By("creating the attachment configuration")
			_, err := nadClient.NetworkAttachmentDefinitions(f.Namespace.Name).Create(
				context.Background(),
				netAttachDef,
				metav1.CreateOptions{},
			)
			Expect(err).NotTo(HaveOccurred())

			By("creating the pod using a secondary network")
			pod, err := cs.CoreV1().Pods(f.Namespace.Name).Create(
				context.Background(),
				generatePodSpec(f.Namespace.Name, podName, nil, netAttachDef.Name),
				metav1.CreateOptions{},
			)
			Expect(err).NotTo(HaveOccurred())

			By("asserting the pod gets to the `Ready` phase")
			Eventually(func() v1.PodPhase {
				updatedPod, err := cs.CoreV1().Pods(f.Namespace.Name).Get(context.Background(), pod.GetName(), metav1.GetOptions{})
				if err != nil {
					return v1.PodFailed
				}
				return updatedPod.Status.Phase
			}, 2*time.Minute, 6*time.Second).Should(Equal(v1.PodRunning))
		},
			table.Entry(
				"when attaching to an L3 - routed - network",
				generateLayer3SecondaryOvnNetwork,
				netCIDR(secondaryNetworkCIDR, 24),
			),
			table.Entry(
				"when attaching to an L2 - switched - network featuring `excludeCIDR`s",
				generateSwitchedSecondaryOvnNetwork,
				secondaryFlatL2NetworkCIDR,
				secondaryFlatL2IgnoreCIDR,
			),
		)
	})

	Context("multiple pods connected to the same OVN-K secondary network", func() {
		const port = 9000

		table.DescribeTable("can communicate over the secondary network", func(generatorFn attachmentGeneratorFn, cidr string, excludeCIDRs ...string) {
			netAttachDef := generatorFn(f.Namespace.Name, secondaryNetworkName, cidr, excludeCIDRs...)
			By("creating the attachment configuration")
			_, err := nadClient.NetworkAttachmentDefinitions(f.Namespace.Name).Create(
				context.Background(),
				netAttachDef,
				metav1.CreateOptions{},
			)
			Expect(err).NotTo(HaveOccurred())

			By("instantiating the server pod")
			serverPod, err := cs.CoreV1().Pods(f.Namespace.Name).Create(
				context.Background(),
				generatePodSpec(f.Namespace.Name, podName, httpServerContainerCmd(port), secondaryNetworkName),
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

			By("asserting the server pod has an IP from the configured range")
			pod, err := cs.CoreV1().Pods(f.Namespace.Name).Get(context.Background(), serverPod.GetName(), metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			netStatus, err := podNetworkStatus(pod, func(status nadapi.NetworkStatus) bool {
				return status.Name == namespacedName(f.Namespace.Name, secondaryNetworkName)
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(netStatus).To(HaveLen(1))
			Expect(netStatus[0].IPs).NotTo(BeEmpty())

			serverIP := netStatus[0].IPs[0]
			Expect(inRange(secondaryNetworkCIDR, serverIP)).To(Succeed())

			By("instantiating the *client* pod")
			const clientPodName = "client-pod"
			clientPod, err := cs.CoreV1().Pods(f.Namespace.Name).Create(
				context.Background(),
				generatePodSpec(f.Namespace.Name, clientPodName, nil, secondaryNetworkName),
				metav1.CreateOptions{},
			)
			Expect(err).NotTo(HaveOccurred())

			By("asserting the *client* pod can contact the server pod exposed endpoint")
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
		},
			table.Entry(
				"can communicate over an L2 - switched - secondary network",
				generateSwitchedSecondaryOvnNetwork,
				secondaryFlatL2NetworkCIDR,
				secondaryFlatL2IgnoreCIDR,
			),
			table.Entry(
				"can communicate over an L3 - routed - secondary network",
				generateLayer3SecondaryOvnNetwork,
				netCIDR(secondaryNetworkCIDR, 24),
			),
		)
	})
})

func netCIDR(netCIDR string, netPrefixLengthPerNode int) string {
	return fmt.Sprintf("%s/%d", netCIDR, netPrefixLengthPerNode)
}

type attachmentGeneratorFn func(namespace string, name string, cidr string, excludeCIDRs ...string) *nadapi.NetworkAttachmentDefinition

func generateLayer3SecondaryOvnNetwork(namespace string, name string, cidr string, excludeCIDRs ...string) *nadapi.NetworkAttachmentDefinition {
	nadSpec := fmt.Sprintf(`
{
        "cniVersion": "0.3.0",
        "name": %q,
        "type": "ovn-k8s-cni-overlay",
        "topology":"layer3",
        "subnets": %q,
        "excludeSubnets": %q,
        "mtu": 1300,
        "netAttachDefName": %q
}
`, name, cidr, strings.Join(excludeCIDRs, ","), fmt.Sprintf("%s/%s", namespace, name))
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
