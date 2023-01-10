package e2e

import (
	"context"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"

	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	nadclient "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/typed/k8s.cni.cncf.io/v1"
)

var _ = Describe("Multi-Homing", func() {
	Context("Layer 3 topology", func() {
		const (
			podName              = "tinypod"
			secondaryNetworkName = "tenant-blue"
		)
		f := wrappedTestFramework("multi-homing")

		var (
			nadClient nadclient.K8sCniCncfIoV1Interface
			pod       *v1.Pod
		)

		var cs clientset.Interface

		BeforeEach(func() {
			cs = f.ClientSet

			var err error
			nadClient, err = nadclient.NewForConfig(f.ClientConfig())
			Expect(err).NotTo(HaveOccurred())

			_, err = nadClient.NetworkAttachmentDefinitions(f.Namespace.Name).Create(
				context.Background(),
				generateLayer3SecondaryOvnNetwork(f.Namespace.Name, secondaryNetworkName),
				metav1.CreateOptions{},
			)
			Expect(err).NotTo(HaveOccurred())

			pod, err = cs.CoreV1().Pods(f.Namespace.Name).Create(
				context.Background(),
				generatePodSpec(f.Namespace.Name, podName, nil, secondaryNetworkName),
				metav1.CreateOptions{},
			)
			Expect(err).NotTo(HaveOccurred())
		})

		It("A pod with an OVN-K secondary network is able to get to the Running phase", func() {
			Eventually(func() v1.PodPhase {
				updatedPod, err := cs.CoreV1().Pods(f.Namespace.Name).Get(context.Background(), pod.GetName(), metav1.GetOptions{})
				if err != nil {
					return v1.PodFailed
				}
				return updatedPod.Status.Phase
			}, 2*time.Minute, 6*time.Second).Should(Equal(v1.PodRunning))
		})
	})
})

func generateLayer3SecondaryOvnNetwork(namespace string, name string) *nadapi.NetworkAttachmentDefinition {
	nadSpec := fmt.Sprintf(`
{
        "cniVersion": "0.3.0",
        "name": %q,
        "type": "ovn-k8s-cni-overlay",
        "topology":"layer3",
        "subnets": "10.128.0.0/16/24",
        "mtu": 1300,
        "netAttachDefName": %q
}
`, name, fmt.Sprintf("%s/%s", namespace, name))
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
