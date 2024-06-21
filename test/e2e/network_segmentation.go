package e2e

import (
	"context"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	iputils "github.com/containernetworking/plugins/pkg/ip"

	nadclient "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/typed/k8s.cni.cncf.io/v1"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubectl/pkg/util/podutils"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	utilnet "k8s.io/utils/net"
)

var _ = Describe("Network Segmentation", func() {
	f := wrappedTestFramework("network-segmentation")
	Context("a user defined primary network", func() {
		const (
			gatewayIPv4Address           = "10.128.0.1"
			gatewayIPv6Address           = "2014:100:200::1"
			nodeHostnameKey              = "kubernetes.io/hostname"
			port                         = 9000
			defaultPort                  = 8080
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

				runUDNPod(cs, f.Namespace.Name, serverPodConfig, nil)
				runUDNPod(cs, f.Namespace.Name, clientPodConfig, nil)

				var serverIP string
				for i, cidr := range strings.Split(netConfig.cidr, ",") {
					if cidr != "" {
						By("asserting the server pod has an IP from the configured range")
						serverIP, err = podIPsForUserDefinedPrimaryNetwork(
							cs,
							f.Namespace.Name,
							serverPodConfig.name,
							namespacedName(f.Namespace.Name, netConfig.name),
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
					name:     nadName,
					topology: "layer2",
					cidr:     fmt.Sprintf("%s,%s", userDefinedNetworkIPv4Subnet, userDefinedNetworkIPv6Subnet),
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
			Entry(
				"two pods connected over a L3 dualstack primary UDN",
				networkAttachmentConfigParams{
					name:     nadName,
					topology: "layer3",
					cidr:     fmt.Sprintf("%s,%s", userDefinedNetworkIPv4Subnet, userDefinedNetworkIPv6Subnet),
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

		DescribeTable(
			"is isolated from the default network",
			func(
				netConfigParams networkAttachmentConfigParams,
				udnPodConfig podConfiguration,
			) {
				if !isInterconnectEnabled() {
					const upstreamIssue = "https://github.com/ovn-org/ovn-kubernetes/issues/4528"
					e2eskipper.Skipf(
						"These tests are known to fail on non-IC deployments. Upstream issue: %s", upstreamIssue,
					)
				}

				By("Creating second namespace for default network pods")
				defaultNetNamespace := f.Namespace.Name + "-default"
				_, err := cs.CoreV1().Namespaces().Create(context.Background(), &v1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name: defaultNetNamespace,
					},
				}, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())
				defer func() {
					Expect(cs.CoreV1().Namespaces().Delete(context.Background(), defaultNetNamespace, metav1.DeleteOptions{})).To(Succeed())
				}()

				netConfig := newNetworkAttachmentConfig(netConfigParams)
				netConfig.namespace = f.Namespace.Name
				udnPodConfig.namespace = f.Namespace.Name

				By("creating the attachment configuration")
				_, err = nadClient.NetworkAttachmentDefinitions(f.Namespace.Name).Create(
					context.Background(),
					generateNAD(netConfig),
					metav1.CreateOptions{},
				)
				Expect(err).NotTo(HaveOccurred())

				udnPod := runUDNPod(cs, f.Namespace.Name, udnPodConfig, func(pod *v1.Pod) {
					pod.Spec.Containers[0].ReadinessProbe = &v1.Probe{
						ProbeHandler: v1.ProbeHandler{
							HTTPGet: &v1.HTTPGetAction{
								Path: "/healthz",
								Port: intstr.FromInt32(port),
							},
						},
						InitialDelaySeconds: 1,
						PeriodSeconds:       1,
						FailureThreshold:    1,
					}
					pod.Spec.Containers[0].LivenessProbe = &v1.Probe{
						ProbeHandler: v1.ProbeHandler{
							HTTPGet: &v1.HTTPGetAction{
								Path: "/healthz",
								Port: intstr.FromInt32(port),
							},
						},
						InitialDelaySeconds: 1,
						PeriodSeconds:       1,
						FailureThreshold:    1,
					}
					// add NET_ADMIN to change pod routes
					pod.Spec.Containers[0].SecurityContext = &v1.SecurityContext{
						Capabilities: &v1.Capabilities{
							Add: []v1.Capability{"NET_ADMIN"},
						},
					}
				})

				By("creating default network pod")
				defaultPod, err := createPod(f, "default-net-pod", udnPodConfig.nodeSelector[nodeHostnameKey],
					defaultNetNamespace, []string{"/agnhost", "netexec"}, nil)
				Expect(err).NotTo(HaveOccurred())
				By("creating default network client pod")
				defaultClientPod, err := createPod(f, "default-net-client-pod", udnPodConfig.nodeSelector[nodeHostnameKey],
					defaultNetNamespace, []string{}, nil)
				Expect(err).NotTo(HaveOccurred())

				udnIPv4, udnIPv6, err := podIPsForDefaultNetwork(
					cs,
					f.Namespace.Name,
					udnPod.GetName(),
				)
				Expect(err).NotTo(HaveOccurred())

				for _, destIP := range []string{udnIPv4, udnIPv6} {
					if destIP == "" {
						continue
					}
					// positive case for UDN pod is a successful healthcheck, checked later
					By("checking the default network pod can't reach UDN pod on IP " + destIP)
					Consistently(func() bool {
						return connectToServer(podConfiguration{namespace: defaultPod.Namespace, name: defaultPod.Name}, destIP, port) != nil
					}, 5*time.Second).Should(BeTrue())
				}

				defaultIPv4, defaultIPv6, err := podIPsForDefaultNetwork(
					cs,
					defaultPod.Namespace,
					defaultPod.Name,
				)
				Expect(err).NotTo(HaveOccurred())

				for _, destIP := range []string{defaultIPv4, defaultIPv6} {
					if destIP == "" {
						continue
					}
					By("checking the default network client pod can reach default pod on IP " + destIP)
					Eventually(func() bool {
						return connectToServer(podConfiguration{namespace: defaultClientPod.Namespace, name: defaultClientPod.Name}, destIP, defaultPort) == nil
					}).Should(BeTrue())
					By("checking the UDN pod can't reach the default network pod on IP " + destIP)
					Consistently(func() bool {
						return connectToServer(udnPodConfig, destIP, defaultPort) != nil
					}, 5*time.Second).Should(BeTrue())
				}

				By("asserting healthcheck works (kubelet can access the UDN pod)")
				// The pod should be ready
				Expect(podutils.IsPodReady(udnPod)).To(BeTrue())

				// connectivity check is run every second + 1sec initialDelay
				// By this time we have spent at least 8 seconds doing the above checks
				udnPod, err = cs.CoreV1().Pods(udnPod.Namespace).Get(context.Background(), udnPod.Name, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				Expect(udnPod.Status.ContainerStatuses[0].RestartCount).To(Equal(int32(0)))

				// TODO
				//By("checking non-kubelet default network host process can't reach the UDN pod")

				By("asserting UDN pod can't reach host via default network interface")
				// tweak pod route to use default network interface as default
				podAnno, err := unmarshalPodAnnotation(udnPod.Annotations, "default")
				Expect(err).NotTo(HaveOccurred())
				for _, podIP := range podAnno.IPs {
					ipCommand := []string{"exec", udnPod.Name, "--", "ip"}
					if podIP.IP.To4() == nil {
						ipCommand = append(ipCommand, "-6")
					}
					// 1. Find current default route and delete it
					defRoute, err := e2ekubectl.RunKubectl(udnPod.Namespace,
						append(ipCommand, "route", "show", "default")...)
					Expect(err).NotTo(HaveOccurred())
					defRoute = strings.TrimSpace(defRoute)
					if defRoute == "" {
						continue
					}
					framework.Logf("Found default route %v, deleting", defRoute)
					cmd := append(ipCommand, "route", "del")
					_, err = e2ekubectl.RunKubectl(udnPod.Namespace,
						append(cmd, strings.Split(defRoute, " ")...)...)
					Expect(err).NotTo(HaveOccurred())

					// 2. Add a new default route to use default network interface
					gatewayIP := iputils.NextIP(iputils.Network(podIP).IP)
					_, err = e2ekubectl.RunKubectl(udnPod.Namespace,
						append(ipCommand, "route", "add", "default", "via", gatewayIP.String(), "dev", "eth0")...)
					Expect(err).NotTo(HaveOccurred())
				}
				// Now try to reach the host from the UDN pod
				defaultPodHostIP := udnPod.Status.HostIPs
				for _, hostIP := range defaultPodHostIP {
					By("checking the UDN pod can't reach the host on IP " + hostIP.IP)
					ping := "ping"
					if utilnet.IsIPv6String(hostIP.IP) {
						ping = "ping6"
					}
					Consistently(func() bool {
						_, err := e2ekubectl.RunKubectl(udnPod.Namespace, "exec", udnPod.Name, "--",
							ping, "-c", "1", "-W", "1", hostIP.IP,
						)
						return err == nil
					}, 4*time.Second).Should(BeFalse())
				}

				By("asserting UDN pod can't reach default services via default network interface")
				// route setup is already done, get kapi IPs
				kapi, err := cs.CoreV1().Services("default").Get(context.Background(), "kubernetes", metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				for _, kapiIP := range kapi.Spec.ClusterIPs {
					By("checking the UDN pod can't reach kapi service on IP " + kapiIP)
					Consistently(func() bool {
						return connectToServer(udnPodConfig, kapiIP, int(kapi.Spec.Ports[0].Port)) != nil
					}, 5*time.Second).Should(BeTrue())
				}
			},
			Entry(
				"with L2 dualstack primary UDN",
				networkAttachmentConfigParams{
					name:     nadName,
					topology: "layer2",
					cidr:     fmt.Sprintf("%s,%s", userDefinedNetworkIPv4Subnet, userDefinedNetworkIPv6Subnet),
					role:     "primary",
				},
				*podConfig(
					"udn-pod",
					withCommand(func() []string {
						return httpServerContainerCmd(port)
					}),
					withNodeSelector(map[string]string{nodeHostnameKey: workerOneNodeName}),
				),
			),
			Entry(
				"with L3 dualstack primary UDN",
				networkAttachmentConfigParams{
					name:     nadName,
					topology: "layer3",
					cidr:     fmt.Sprintf("%s,%s", userDefinedNetworkIPv4Subnet, userDefinedNetworkIPv6Subnet),
					role:     "primary",
				},
				*podConfig(
					"udn-pod",
					withCommand(func() []string {
						return httpServerContainerCmd(port)
					}),
					withNodeSelector(map[string]string{nodeHostnameKey: workerOneNodeName}),
				),
			),
		)

		Context("an HTTP service which is deployed outside the Kubernetes cluster", func() {
			const (
				externalContainerName = "ovn-k-egress-test-helper"
			)
			var externalIpv4 string
			BeforeEach(func() {
				externalIpv4, _ = createClusterExternalContainer(
					externalContainerName,
					"registry.k8s.io/e2e-test-images/agnhost:2.45",
					runExternalContainerCmd(),
					httpServerContainerCmd(port),
				)

				DeferCleanup(func() {
					deleteClusterExternalContainer(externalContainerName)
				})
			})

			XDescribeTable(
				"can be accessed to from the pods running in the Kubernetes cluster",
				func(netConfigParams networkAttachmentConfigParams, clientPodConfig podConfiguration) {
					netConfig := newNetworkAttachmentConfig(netConfigParams)

					netConfig.namespace = f.Namespace.Name
					clientPodConfig.namespace = f.Namespace.Name

					By("creating the attachment configuration")
					_, err := nadClient.NetworkAttachmentDefinitions(f.Namespace.Name).Create(
						context.Background(),
						generateNAD(netConfig),
						metav1.CreateOptions{},
					)
					Expect(err).NotTo(HaveOccurred())

					By("instantiating the client pod")
					clientPod, err := cs.CoreV1().Pods(clientPodConfig.namespace).Create(
						context.Background(),
						generatePodSpec(clientPodConfig),
						metav1.CreateOptions{},
					)
					Expect(err).NotTo(HaveOccurred())
					Expect(clientPod).NotTo(BeNil())

					By("asserting the client pod reaches the `Ready` state")
					Eventually(func() v1.PodPhase {
						updatedPod, err := cs.CoreV1().Pods(f.Namespace.Name).Get(context.Background(), clientPod.GetName(), metav1.GetOptions{})
						if err != nil {
							return v1.PodFailed
						}
						return updatedPod.Status.Phase
					}, 2*time.Minute, 6*time.Second).Should(Equal(v1.PodRunning))

					By("asserting the *client* pod can contact the server located outside the cluster")
					Eventually(func() error {
						return connectToServer(clientPodConfig, externalIpv4, port)
					}, 2*time.Minute, 6*time.Second).Should(Succeed())
				},
				Entry("by one pod with a single IPv4 address over a layer2 network",
					networkAttachmentConfigParams{
						name:     userDefinedNetworkName,
						topology: "layer2",
						cidr:     userDefinedNetworkIPv4Subnet,
						role:     "primary",
					},
					*podConfig("client-pod"),
				),
				Entry("by one pod with a single IPv4 address over a layer3 network",
					networkAttachmentConfigParams{
						name:     userDefinedNetworkName,
						topology: "layer3",
						cidr:     userDefinedNetworkIPv4Subnet,
						role:     "primary",
					},
					*podConfig("client-pod"),
				),
			)
		})
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

func podIPsForDefaultNetwork(k8sClient clientset.Interface, podNamespace string, podName string) (string, string, error) {
	pod, err := k8sClient.CoreV1().Pods(podNamespace).Get(context.Background(), podName, metav1.GetOptions{})
	if err != nil {
		return "", "", err
	}
	ipv4, ipv6 := getPodAddresses(pod)
	return ipv4, ipv6, nil
}

func userDefinedNetworkStatus(pod *v1.Pod, networkName string) (PodAnnotation, error) {
	netStatus, err := unmarshalPodAnnotation(pod.Annotations, networkName)
	if err != nil {
		return PodAnnotation{}, fmt.Errorf("failed to unmarshall annotations for pod %q: %v", pod.Name, err)
	}

	return *netStatus, nil
}

func runUDNPod(cs clientset.Interface, namespace string, serverPodConfig podConfiguration, podSpecTweak func(*v1.Pod)) *v1.Pod {
	By(fmt.Sprintf("instantiating the UDN pod %s", serverPodConfig.name))
	podSpec := generatePodSpec(serverPodConfig)
	if podSpecTweak != nil {
		podSpecTweak(podSpec)
	}
	serverPod, err := cs.CoreV1().Pods(serverPodConfig.namespace).Create(
		context.Background(),
		podSpec,
		metav1.CreateOptions{},
	)
	Expect(err).NotTo(HaveOccurred())
	Expect(serverPod).NotTo(BeNil())

	By(fmt.Sprintf("asserting the UDN pod %s reaches the `Ready` state", serverPodConfig.name))
	var updatedPod *v1.Pod
	Eventually(func() v1.PodPhase {
		updatedPod, err = cs.CoreV1().Pods(namespace).Get(context.Background(), serverPod.GetName(), metav1.GetOptions{})
		if err != nil {
			return v1.PodFailed
		}
		return updatedPod.Status.Phase
	}, 2*time.Minute, 6*time.Second).Should(Equal(v1.PodRunning))
	return updatedPod
}

func runExternalContainerCmd() []string {
	return []string{"--network", "kind"}
}
