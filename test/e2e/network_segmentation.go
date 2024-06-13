package e2e

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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
		expectedActiveNetwork string
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

		By("Wait for active-network annotation")
		Eventually(thisNamespace(f.ClientSet, f.Namespace)).
			WithPolling(time.Second / 2).
			WithTimeout(5 * time.Second).
			Should(WithTransform(getAnnotations,
				HaveKeyWithValue(activeNetworkAnnotation, td.expectedActiveNetwork)))

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
	)
})
