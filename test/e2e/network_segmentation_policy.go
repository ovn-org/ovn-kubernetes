package e2e

import (
	"context"
	"fmt"
	"strings"
	"time"

	nadclient "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/typed/k8s.cni.cncf.io/v1"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	v1 "k8s.io/api/core/v1"
	knet "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
)

var _ = ginkgo.Describe("Network Segmentation: Network Policies", func() {
	f := wrappedTestFramework("network-segmentation")
	f.SkipNamespaceCreation = true

	ginkgo.Context("on a user defined primary network", func() {
		const (
			nadName                      = "tenant-red"
			userDefinedNetworkIPv4Subnet = "10.128.0.0/16"
			userDefinedNetworkIPv6Subnet = "2014:100:200::0/60"
			nodeHostnameKey              = "kubernetes.io/hostname"
			workerOneNodeName            = "ovn-worker"
			workerTwoNodeName            = "ovn-worker2"
			port                         = 9000
			netPrefixLengthPerNode       = 24
			randomStringLength           = 5
			nameSpaceYellowSuffix        = "yellow"
			namespaceBlueSuffix          = "blue"
		)

		var (
			cs                  clientset.Interface
			nadClient           nadclient.K8sCniCncfIoV1Interface
			allowServerPodLabel = map[string]string{"foo": "bar"}
			denyServerPodLabel  = map[string]string{"abc": "xyz"}
		)

		ginkgo.BeforeEach(func() {
			cs = f.ClientSet
			namespace, err := f.CreateNamespace(context.TODO(), f.BaseName, map[string]string{
				"e2e-framework":           f.BaseName,
				RequiredUDNNamespaceLabel: "",
			})
			f.Namespace = namespace
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			nadClient, err = nadclient.NewForConfig(f.ClientConfig())
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			namespaceYellow := getNamespaceName(f, nameSpaceYellowSuffix)
			namespaceBlue := getNamespaceName(f, namespaceBlueSuffix)
			for _, namespace := range []string{namespaceYellow, namespaceBlue} {
				ginkgo.By("Creating namespace " + namespace)
				ns, err := cs.CoreV1().Namespaces().Create(context.Background(), &v1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name:   namespace,
						Labels: map[string]string{RequiredUDNNamespaceLabel: ""},
					},
				}, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				f.AddNamespacesToDelete(ns)
			}
		})

		ginkgo.DescribeTable(
			"pods within namespace should be isolated when deny policy is present",
			func(
				netConfigParams networkAttachmentConfigParams,
				clientPodConfig podConfiguration,
				serverPodConfig podConfiguration,
			) {
				ginkgo.By("Creating the attachment configuration")
				netConfig := newNetworkAttachmentConfig(netConfigParams)
				netConfig.namespace = f.Namespace.Name
				_, err := nadClient.NetworkAttachmentDefinitions(f.Namespace.Name).Create(
					context.Background(),
					generateNAD(netConfig),
					metav1.CreateOptions{},
				)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				ginkgo.By("creating client/server pods")
				serverPodConfig.namespace = f.Namespace.Name
				clientPodConfig.namespace = f.Namespace.Name
				runUDNPod(cs, f.Namespace.Name, serverPodConfig, nil)
				runUDNPod(cs, f.Namespace.Name, clientPodConfig, nil)

				var serverIP string
				for i, cidr := range strings.Split(netConfig.cidr, ",") {
					if cidr != "" {
						ginkgo.By("asserting the server pod has an IP from the configured range")
						serverIP, err = podIPsForUserDefinedPrimaryNetwork(
							cs,
							f.Namespace.Name,
							serverPodConfig.name,
							namespacedName(f.Namespace.Name, netConfig.name),
							i,
						)
						gomega.Expect(err).NotTo(gomega.HaveOccurred())
						const netPrefixLengthPerNode = 24
						ginkgo.By(fmt.Sprintf("asserting the server pod IP %v is from the configured range %v/%v", serverIP, cidr, netPrefixLengthPerNode))
						subnet, err := getNetCIDRSubnet(cidr)
						gomega.Expect(err).NotTo(gomega.HaveOccurred())
						gomega.Expect(inRange(subnet, serverIP)).To(gomega.Succeed())
					}

					ginkgo.By("asserting the *client* pod can contact the server pod exposed endpoint")
					gomega.Eventually(func() error {
						return reachToServerPodFromClient(cs, serverPodConfig, clientPodConfig, serverIP, port)
					}, 2*time.Minute, 6*time.Second).Should(gomega.Succeed())
				}

				ginkgo.By("creating a \"default deny\" network policy")
				_, err = makeDenyAllPolicy(f, f.Namespace.Name, "deny-all")
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				ginkgo.By("asserting the *client* pod can not contact the server pod exposed endpoint")
				gomega.Eventually(func() error {
					return reachToServerPodFromClient(cs, serverPodConfig, clientPodConfig, serverIP, port)
				}, 1*time.Minute, 6*time.Second).ShouldNot(gomega.Succeed())

			},
			ginkgo.Entry(
				"in L2 dualstack primary UDN",
				networkAttachmentConfigParams{
					name:     nadName,
					topology: "layer2",
					cidr:     correctCIDRFamily(userDefinedNetworkIPv4Subnet, userDefinedNetworkIPv6Subnet),
					role:     "primary",
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
			ginkgo.Entry(
				"in L3 dualstack primary UDN",
				networkAttachmentConfigParams{
					name:     nadName,
					topology: "layer3",
					cidr:     correctCIDRFamily(userDefinedNetworkIPv4Subnet, userDefinedNetworkIPv6Subnet),
					role:     "primary",
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

		ginkgo.DescribeTable(
			"allow ingress traffic to one pod from a particular namespace",
			func(
				topology string,
				clientPodConfig podConfiguration,
				allowServerPodConfig podConfiguration,
				denyServerPodConfig podConfiguration,
			) {

				namespaceYellow := getNamespaceName(f, nameSpaceYellowSuffix)
				namespaceBlue := getNamespaceName(f, namespaceBlueSuffix)

				nad := networkAttachmentConfigParams{
					topology: topology,
					cidr:     correctCIDRFamily(userDefinedNetworkIPv4Subnet, userDefinedNetworkIPv6Subnet),
					// Both yellow and blue namespaces are going to served by green network.
					// Use random suffix for the network name to avoid race between tests.
					networkName: fmt.Sprintf("%s-%s", "green", rand.String(randomStringLength)),
					role:        "primary",
				}

				// Use random suffix in net conf name to avoid race between tests.
				netConfName := fmt.Sprintf("sharednet-%s", rand.String(randomStringLength))
				for _, namespace := range []string{namespaceYellow, namespaceBlue} {
					ginkgo.By("creating the attachment configuration for " + netConfName + " in namespace " + namespace)
					netConfig := newNetworkAttachmentConfig(nad)
					netConfig.namespace = namespace
					netConfig.name = netConfName

					_, err := nadClient.NetworkAttachmentDefinitions(namespace).Create(
						context.Background(),
						generateNAD(netConfig),
						metav1.CreateOptions{},
					)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
				}

				ginkgo.By("creating client/server pods")
				allowServerPodConfig.namespace = namespaceYellow
				denyServerPodConfig.namespace = namespaceYellow
				clientPodConfig.namespace = namespaceBlue
				runUDNPod(cs, namespaceYellow, allowServerPodConfig, nil)
				runUDNPod(cs, namespaceYellow, denyServerPodConfig, nil)
				runUDNPod(cs, namespaceBlue, clientPodConfig, nil)

				ginkgo.By("asserting the server pods have an IP from the configured range")
				allowServerPodIP, err := podIPsForUserDefinedPrimaryNetwork(cs, namespaceYellow, allowServerPodConfig.name,
					namespacedName(namespaceYellow, netConfName), 0)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				ginkgo.By(fmt.Sprintf("asserting the allow server pod IP %v is from the configured range %v/%v", allowServerPodIP,
					userDefinedNetworkIPv4Subnet, netPrefixLengthPerNode))
				subnet, err := getNetCIDRSubnet(userDefinedNetworkIPv4Subnet)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(inRange(subnet, allowServerPodIP)).To(gomega.Succeed())
				denyServerPodIP, err := podIPsForUserDefinedPrimaryNetwork(cs, namespaceYellow, denyServerPodConfig.name,
					namespacedName(namespaceYellow, netConfName), 0)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				ginkgo.By(fmt.Sprintf("asserting the deny server pod IP %v is from the configured range %v/%v", denyServerPodIP,
					userDefinedNetworkIPv4Subnet, netPrefixLengthPerNode))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(inRange(subnet, denyServerPodIP)).To(gomega.Succeed())

				ginkgo.By("asserting the *client* pod can contact the allow server pod exposed endpoint")
				gomega.Eventually(func() error {
					return reachToServerPodFromClient(cs, allowServerPodConfig, clientPodConfig, allowServerPodIP, port)
				}, 2*time.Minute, 6*time.Second).Should(gomega.Succeed())

				ginkgo.By("asserting the *client* pod can contact the deny server pod exposed endpoint")
				gomega.Eventually(func() error {
					return reachToServerPodFromClient(cs, denyServerPodConfig, clientPodConfig, denyServerPodIP, port)
				}, 2*time.Minute, 6*time.Second).Should(gomega.Succeed())

				ginkgo.By("creating a \"default deny\" network policy")
				_, err = makeDenyAllPolicy(f, namespaceYellow, "deny-all")
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				ginkgo.By("asserting the *client* pod can not contact the allow server pod exposed endpoint")
				gomega.Eventually(func() error {
					return reachToServerPodFromClient(cs, allowServerPodConfig, clientPodConfig, allowServerPodIP, port)
				}, 1*time.Minute, 6*time.Second).ShouldNot(gomega.Succeed())

				ginkgo.By("asserting the *client* pod can not contact the deny server pod exposed endpoint")
				gomega.Eventually(func() error {
					return reachToServerPodFromClient(cs, denyServerPodConfig, clientPodConfig, denyServerPodIP, port)
				}, 1*time.Minute, 6*time.Second).ShouldNot(gomega.Succeed())

				ginkgo.By("creating a \"allow-traffic-to-pod\" network policy")
				_, err = allowTrafficToPodFromNamespacePolicy(f, namespaceYellow, namespaceBlue, "allow-traffic-to-pod", allowServerPodLabel)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				ginkgo.By("asserting the *client* pod can contact the allow server pod exposed endpoint")
				gomega.Eventually(func() error {
					return reachToServerPodFromClient(cs, allowServerPodConfig, clientPodConfig, allowServerPodIP, port)
				}, 1*time.Minute, 6*time.Second).Should(gomega.Succeed())

				ginkgo.By("asserting the *client* pod can not contact deny server pod exposed endpoint")
				gomega.Eventually(func() error {
					return reachToServerPodFromClient(cs, denyServerPodConfig, clientPodConfig, denyServerPodIP, port)
				}, 1*time.Minute, 6*time.Second).ShouldNot(gomega.Succeed())

			},
			ginkgo.Entry(
				"in L2 primary UDN",
				"layer2",
				*podConfig(
					"client-pod",
					withNodeSelector(map[string]string{nodeHostnameKey: workerOneNodeName}),
				),
				*podConfig(
					"allow-server-pod",
					withCommand(func() []string {
						return httpServerContainerCmd(port)
					}),
					withNodeSelector(map[string]string{nodeHostnameKey: workerTwoNodeName}),
					withLabels(allowServerPodLabel),
				),
				*podConfig(
					"deny-server-pod",
					withCommand(func() []string {
						return httpServerContainerCmd(port)
					}),
					withNodeSelector(map[string]string{nodeHostnameKey: workerTwoNodeName}),
					withLabels(denyServerPodLabel),
				),
			),
			ginkgo.Entry(
				"in L3 primary UDN",
				"layer3",
				*podConfig(
					"client-pod",
					withNodeSelector(map[string]string{nodeHostnameKey: workerOneNodeName}),
				),
				*podConfig(
					"allow-server-pod",
					withCommand(func() []string {
						return httpServerContainerCmd(port)
					}),
					withNodeSelector(map[string]string{nodeHostnameKey: workerTwoNodeName}),
					withLabels(allowServerPodLabel),
				),
				*podConfig(
					"deny-server-pod",
					withCommand(func() []string {
						return httpServerContainerCmd(port)
					}),
					withNodeSelector(map[string]string{nodeHostnameKey: workerTwoNodeName}),
					withLabels(denyServerPodLabel),
				),
			))
	})
})

func getNamespaceName(f *framework.Framework, nsSuffix string) string {
	return fmt.Sprintf("%s-%s", f.Namespace.Name, nsSuffix)
}

func allowTrafficToPodFromNamespacePolicy(f *framework.Framework, namespace, fromNamespace, policyName string, podLabel map[string]string) (*knet.NetworkPolicy, error) {
	policy := &knet.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name: policyName,
		},
		Spec: knet.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: podLabel},
			PolicyTypes: []knet.PolicyType{knet.PolicyTypeIngress},
			Ingress: []knet.NetworkPolicyIngressRule{{From: []knet.NetworkPolicyPeer{
				{NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": fromNamespace}}}}}},
		},
	}
	return f.ClientSet.NetworkingV1().NetworkPolicies(namespace).Create(context.TODO(), policy, metav1.CreateOptions{})
}
