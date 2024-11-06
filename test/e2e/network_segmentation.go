package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	nadclient "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/typed/k8s.cni.cncf.io/v1"

	v1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubectl/pkg/util/podutils"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	utilnet "k8s.io/utils/net"
	"k8s.io/utils/pointer"
)

const openDefaultPortsAnnotation = "k8s.ovn.org/open-default-ports"

var _ = Describe("Network Segmentation", func() {
	f := wrappedTestFramework("network-segmentation")

	var (
		cs        clientset.Interface
		nadClient nadclient.K8sCniCncfIoV1Interface
	)

	const (
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

	BeforeEach(func() {
		cs = f.ClientSet

		var err error
		nadClient, err = nadclient.NewForConfig(f.ClientConfig())
		Expect(err).NotTo(HaveOccurred())
	})

	Context("a user defined primary network", func() {

		DescribeTableSubtree("created using",
			func(createNetworkFn func(c networkAttachmentConfigParams) error) {

				DescribeTable(
					"creates a networkStatus Annotation with UDN interface",
					func(netConfig networkAttachmentConfigParams) {
						By("creating the network")
						netConfig.namespace = f.Namespace.Name
						Expect(createNetworkFn(netConfig)).To(Succeed())

						By("creating a pod on the udn namespace")
						podConfig := *podConfig("some-pod")
						podConfig.namespace = f.Namespace.Name
						pod := runUDNPod(cs, f.Namespace.Name, podConfig, nil)

						By("asserting the pod UDN interface on the network-status annotation")
						udnNetStat, err := podNetworkStatus(pod, func(status nadapi.NetworkStatus) bool {
							return status.Default
						})
						Expect(err).NotTo(HaveOccurred())
						const (
							expectedDefaultNetStatusLen = 1
							ovnUDNInterface             = "ovn-udn1"
						)
						Expect(udnNetStat).To(HaveLen(expectedDefaultNetStatusLen))
						Expect(udnNetStat[0].Interface).To(Equal(ovnUDNInterface))

						cidrs := strings.Split(netConfig.cidr, ",")
						for i, serverIP := range udnNetStat[0].IPs {
							cidr := cidrs[i]
							if cidr != "" {
								By("asserting the server pod has an IP from the configured range")
								const netPrefixLengthPerNode = 24
								By(fmt.Sprintf("asserting the pod IP %s is from the configured range %s/%d", serverIP, cidr, netPrefixLengthPerNode))
								subnet, err := getNetCIDRSubnet(cidr)
								Expect(err).NotTo(HaveOccurred())
								Expect(inRange(subnet, serverIP)).To(Succeed())
							}
						}
					},
					Entry("L2 primary UDN",
						networkAttachmentConfigParams{
							name:     nadName,
							topology: "layer2",
							cidr:     correctCIDRFamily(userDefinedNetworkIPv4Subnet, userDefinedNetworkIPv6Subnet),
							role:     "primary",
						},
					),
					Entry("L3 primary UDN",
						networkAttachmentConfigParams{
							name:     nadName,
							topology: "layer3",
							cidr:     correctCIDRFamily(userDefinedNetworkIPv4Subnet, userDefinedNetworkIPv6Subnet),
							role:     "primary",
						},
					),
				)

				DescribeTable(
					"can perform east/west traffic between nodes",
					func(
						netConfig networkAttachmentConfigParams,
						clientPodConfig podConfiguration,
						serverPodConfig podConfiguration,
					) {
						By("creating the network")
						netConfig.namespace = f.Namespace.Name
						Expect(createNetworkFn(netConfig)).To(Succeed())

						By("creating client/server pods")
						serverPodConfig.namespace = f.Namespace.Name
						clientPodConfig.namespace = f.Namespace.Name
						runUDNPod(cs, f.Namespace.Name, serverPodConfig, nil)
						runUDNPod(cs, f.Namespace.Name, clientPodConfig, nil)

						var err error
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
						"two pods connected over a L2 primary UDN",
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
					Entry(
						"two pods connected over a L3 primary UDN",
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
						// required so the namespaces get cleaned up
						defer func() {
							Expect(cs.CoreV1().Namespaces().Delete(context.Background(), defaultNetNamespace, metav1.DeleteOptions{})).To(Succeed())
						}()

						By("creating the network")
						netConfigParams.namespace = f.Namespace.Name
						Expect(createNetworkFn(netConfigParams)).To(Succeed())

						udnPodConfig.namespace = f.Namespace.Name
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
							pod.Spec.Containers[0].StartupProbe = &v1.Probe{
								ProbeHandler: v1.ProbeHandler{
									HTTPGet: &v1.HTTPGetAction{
										Path: "/healthz",
										Port: intstr.FromInt32(port),
									},
								},
								InitialDelaySeconds: 1,
								PeriodSeconds:       1,
								FailureThreshold:    3,
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

						// connectivity check is run every second + 1sec initialDelay
						// By this time we have spent at least 8 seconds doing the above checks
						udnPod, err = cs.CoreV1().Pods(udnPod.Namespace).Get(context.Background(), udnPod.Name, metav1.GetOptions{})
						Expect(err).NotTo(HaveOccurred())

						By("asserting healthcheck works (kubelet can access the UDN pod)")
						// The pod should be ready
						Expect(podutils.IsPodReady(udnPod)).To(BeTrue(), fmt.Sprintf("UDN pod is not ready: %v", udnPod))

						Expect(udnPod.Status.ContainerStatuses[0].RestartCount).To(Equal(int32(0)))

						// TODO
						//By("checking non-kubelet default network host process can't reach the UDN pod")

						By("asserting UDN pod can reach the kapi service in the default network")
						// Use the service name to get test the DNS access
						Consistently(func() bool {
							_, err := e2ekubectl.RunKubectl(
								udnPodConfig.namespace,
								"exec",
								udnPodConfig.name,
								"--",
								"curl",
								"--connect-timeout",
								"2",
								"--insecure",
								"https://kubernetes.default/healthz")
							return err == nil
						}, 5*time.Second).Should(BeTrue())
						By("asserting UDN pod can't reach host via default network interface")
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
									ping, "-I", "eth0", "-c", "1", "-W", "1", hostIP.IP,
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
								_, err := e2ekubectl.RunKubectl(
									udnPodConfig.namespace,
									"exec",
									udnPodConfig.name,
									"--",
									"curl",
									"--connect-timeout",
									"2",
									"--interface",
									"eth0",
									"--insecure",
									fmt.Sprintf("https://%s/healthz", kapiIP))
								return err != nil
							}, 5*time.Second).Should(BeTrue())
						}
					},
					Entry(
						"with L2 primary UDN",
						networkAttachmentConfigParams{
							name:     nadName,
							topology: "layer2",
							cidr:     correctCIDRFamily(userDefinedNetworkIPv4Subnet, userDefinedNetworkIPv6Subnet),
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
						"with L3 primary UDN",
						networkAttachmentConfigParams{
							name:     nadName,
							topology: "layer3",
							cidr:     correctCIDRFamily(userDefinedNetworkIPv4Subnet, userDefinedNetworkIPv6Subnet),
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
				DescribeTable(
					"isolates overlapping CIDRs",
					func(
						topology string,
						numberOfPods int,
						userDefinedv4Subnet string,
						userDefinedv6Subnet string,

					) {

						red := "red"
						blue := "blue"

						namespaceRed := f.Namespace.Name + "-" + red
						namespaceBlue := f.Namespace.Name + "-" + blue

						netConfig := networkAttachmentConfigParams{

							topology: topology,
							cidr:     correctCIDRFamily(userDefinedv4Subnet, userDefinedv6Subnet),
							role:     "primary",
						}
						for _, namespace := range []string{namespaceRed, namespaceBlue} {
							By("Creating namespace " + namespace)
							_, err := cs.CoreV1().Namespaces().Create(context.Background(), &v1.Namespace{
								ObjectMeta: metav1.ObjectMeta{
									Name: namespace,
								},
							}, metav1.CreateOptions{})
							Expect(err).NotTo(HaveOccurred())
							defer func() {
								Expect(cs.CoreV1().Namespaces().Delete(context.Background(), namespace, metav1.DeleteOptions{})).To(Succeed())
							}()
						}
						networkNamespaceMap := map[string]string{namespaceRed: red, namespaceBlue: blue}
						for namespace, network := range networkNamespaceMap {
							By("creating the network " + network + " in namespace " + namespace)
							netConfig.namespace = namespace
							netConfig.name = network
							Expect(createNetworkFn(netConfig)).To(Succeed())
						}
						pods := []*v1.Pod{}
						redIPs := []string{}
						blueIPs := []string{}
						for namespace, network := range networkNamespaceMap {
							for i := range numberOfPods {
								podConfig := *podConfig(
									fmt.Sprintf("%s-pod-%d", network, i),
									withCommand(func() []string {
										return httpServerContainerCmd(port)
									}),
								)
								podConfig.namespace = namespace
								//ensure testing accross nodes
								if i%2 == 0 {
									podConfig.nodeSelector = map[string]string{nodeHostnameKey: workerOneNodeName}

								} else {

									podConfig.nodeSelector = map[string]string{nodeHostnameKey: workerTwoNodeName}
								}
								By("creating pod " + podConfig.name + " in " + podConfig.namespace)
								pod := runUDNPod(cs, podConfig.namespace, podConfig, nil)
								pods = append(pods, pod)
								podIP, err := podIPsForUserDefinedPrimaryNetwork(
									cs,
									pod.Namespace,
									pod.Name,
									namespacedName(namespace, network),
									0,
								)
								Expect(err).NotTo(HaveOccurred())
								if network == red {
									redIPs = append(redIPs, podIP)
								} else {
									blueIPs = append(blueIPs, podIP)
								}
							}
						}

						By("ensuring pods only communicate with pods in their network")
						for _, pod := range pods {
							isRedPod := strings.Contains(pod.Name, red)
							ips := redIPs
							if !isRedPod {
								ips = blueIPs
							}
							for _, ip := range ips {
								result, err := e2ekubectl.RunKubectl(
									pod.Namespace,
									"exec",
									pod.Name,
									"--",
									"curl",
									"--connect-timeout",
									"2",
									net.JoinHostPort(ip, fmt.Sprintf("%d", port)+"/hostname"),
								)
								Expect(err).NotTo(HaveOccurred())
								if isRedPod {
									Expect(strings.Contains(result, red)).To(BeTrue())
								} else {
									Expect(strings.Contains(result, blue)).To(BeTrue())
								}
							}
						}

						By("Deleting pods in network blue except " + fmt.Sprintf("%s-pod-%d", blue, numberOfPods-1))
						for i := range numberOfPods - 1 {
							err := cs.CoreV1().Pods(namespaceBlue).Delete(
								context.Background(),
								fmt.Sprintf("%s-pod-%d", blue, i),
								metav1.DeleteOptions{},
							)
							Expect(err).NotTo(HaveOccurred())
						}

						podIP, err := podIPsForUserDefinedPrimaryNetwork(
							cs,
							namespaceBlue,
							fmt.Sprintf("%s-pod-%d", blue, numberOfPods-1),
							namespacedName(namespaceBlue, blue),
							0,
						)
						Expect(err).NotTo(HaveOccurred())

						By("Remaining blue pod cannot communicate with red networks overlapping CIDR")
						for _, ip := range redIPs {
							if podIP == ip {
								//don't try with your own IP
								continue
							}
							_, err := e2ekubectl.RunKubectl(
								namespaceBlue,
								"exec",
								fmt.Sprintf("%s-pod-%d", blue, numberOfPods-1),
								"--",
								"curl",
								"--connect-timeout",
								"2",
								net.JoinHostPort(ip, fmt.Sprintf("%d", port)),
							)
							Expect(err).To(MatchError(ContainSubstring("exit code 28")))
						}
					},
					// can completely fill the L2 topology because it does not depend on the size of the clusters hostsubnet
					Entry(
						"with L2 primary UDN",
						"layer2",
						4,
						"10.128.0.0/29",
						"2014:100:200::0/125",
					),
					// limit the number of pods to 10
					Entry(
						"with L3 primary UDN",
						"layer3",
						10,
						userDefinedNetworkIPv4Subnet,
						userDefinedNetworkIPv6Subnet,
					),
				)
			},
			Entry("NetworkAttachmentDefinitions", func(c networkAttachmentConfigParams) error {
				netConfig := newNetworkAttachmentConfig(c)
				nad := generateNAD(netConfig)
				_, err := nadClient.NetworkAttachmentDefinitions(c.namespace).Create(context.Background(), nad, metav1.CreateOptions{})
				return err
			}),
			Entry("UserDefinedNetwork", func(c networkAttachmentConfigParams) error {
				udnManifest := generateUserDefinedNetworkManifest(&c)
				cleanup, err := createManifest(c.namespace, udnManifest)
				DeferCleanup(cleanup)
				Expect(waitForUserDefinedNetworkReady(c.namespace, c.name, 5*time.Second)).To(Succeed())
				return err
			}),
			Entry("ClusterUserDefinedNetwork", func(c networkAttachmentConfigParams) error {
				cudnManifest := generateClusterUserDefinedNetworkManifest(&c)
				cleanup, err := createManifest("", cudnManifest)
				DeferCleanup(func() {
					cleanup()
					By("delete pods in test namespace to unblock CUDN CR & associate NAD deletion")
					_, err := e2ekubectl.RunKubectl(c.namespace, "delete", "pod", "--all")
					Expect(err).NotTo(HaveOccurred())
					_, err = e2ekubectl.RunKubectl("", "delete", "clusteruserdefinednetwork", c.name)
					Expect(err).NotTo(HaveOccurred())
				})
				Expect(waitForClusterUserDefinedNetworkReady(c.name, 5*time.Second)).To(Succeed())
				return err
			}),
		)
	})

	Context("UserDefinedNetwork CRD Controller", func() {
		const (
			testUdnName                = "test-net"
			userDefinedNetworkResource = "userdefinednetwork"
		)

		Context("for L2 secondary network", func() {
			BeforeEach(func() {
				By("create tests UserDefinedNetwork")
				cleanup, err := createManifest(f.Namespace.Name, newL2SecondaryUDNManifest(testUdnName))
				DeferCleanup(cleanup)
				Expect(err).NotTo(HaveOccurred())
				Expect(waitForUserDefinedNetworkReady(f.Namespace.Name, testUdnName, 5*time.Second)).To(Succeed())
			})

			It("should create NetworkAttachmentDefinition according to spec", func() {
				udnUidRaw, err := e2ekubectl.RunKubectl(f.Namespace.Name, "get", userDefinedNetworkResource, testUdnName, "-o", "jsonpath='{.metadata.uid}'")
				Expect(err).NotTo(HaveOccurred(), "should get the UserDefinedNetwork UID")
				testUdnUID := strings.Trim(udnUidRaw, "'")

				By("verify a NetworkAttachmentDefinition is created according to spec")
				assertL2SecondaryNetAttachDefManifest(nadClient, f.Namespace.Name, testUdnName, testUdnUID)
			})

			It("should delete NetworkAttachmentDefinition when UserDefinedNetwork is deleted", func() {
				By("delete UserDefinedNetwork")
				_, err := e2ekubectl.RunKubectl(f.Namespace.Name, "delete", userDefinedNetworkResource, testUdnName)
				Expect(err).NotTo(HaveOccurred())

				By("verify a NetworkAttachmentDefinition has been deleted")
				Eventually(func() bool {
					_, err := nadClient.NetworkAttachmentDefinitions(f.Namespace.Name).Get(context.Background(), testUdnName, metav1.GetOptions{})
					return err != nil && kerrors.IsNotFound(err)
				}, time.Second*3, time.Second*1).Should(BeTrue(),
					"NetworkAttachmentDefinition should be deleted following UserDefinedNetwork deletion")
			})

			Context("pod connected to UserDefinedNetwork", func() {
				const testPodName = "test-pod-udn"

				var (
					udnInUseDeleteTimeout = 65 * time.Second
					deleteNetworkTimeout  = 5 * time.Second
					deleteNetworkInterval = 1 * time.Second
				)

				BeforeEach(func() {
					By("create pod")
					networkAttachments := []nadapi.NetworkSelectionElement{
						{Name: testUdnName, Namespace: f.Namespace.Name},
					}
					cfg := podConfig(testPodName, withNetworkAttachment(networkAttachments))
					cfg.namespace = f.Namespace.Name
					runUDNPod(cs, f.Namespace.Name, *cfg, nil)
				})

				It("cannot be deleted when being used", func() {
					By("verify UserDefinedNetwork cannot be deleted")
					cmd := e2ekubectl.NewKubectlCommand(f.Namespace.Name, "delete", userDefinedNetworkResource, testUdnName)
					cmd.WithTimeout(time.NewTimer(deleteNetworkTimeout).C)
					_, err := cmd.Exec()
					Expect(err).To(HaveOccurred(),
						"should fail to delete UserDefinedNetwork when used")

					By("verify UserDefinedNetwork associated NetworkAttachmentDefinition cannot be deleted")
					Eventually(func() error {
						ctx, cancel := context.WithTimeout(context.Background(), deleteNetworkTimeout)
						defer cancel()
						_ = nadClient.NetworkAttachmentDefinitions(f.Namespace.Name).Delete(ctx, testUdnName, metav1.DeleteOptions{})
						_, err := nadClient.NetworkAttachmentDefinitions(f.Namespace.Name).Get(ctx, testUdnName, metav1.GetOptions{})
						return err
					}).ShouldNot(HaveOccurred(),
						"should fail to delete UserDefinedNetwork associated NetworkAttachmentDefinition when used")

					By("verify UserDefinedNetwork status reports consuming pod")
					assertUDNStatusReportsConsumers(f.Namespace.Name, testUdnName, testPodName)

					By("delete test pod")
					err = cs.CoreV1().Pods(f.Namespace.Name).Delete(context.Background(), testPodName, metav1.DeleteOptions{})
					Expect(err).ToNot(HaveOccurred())

					By("verify UserDefinedNetwork has been deleted")
					Eventually(func() error {
						_, err := e2ekubectl.RunKubectl(f.Namespace.Name, "get", userDefinedNetworkResource, testUdnName)
						return err
					}, udnInUseDeleteTimeout, deleteNetworkInterval).Should(HaveOccurred(),
						"UserDefinedNetwork should be deleted following test pod deletion")

					By("verify UserDefinedNetwork associated NetworkAttachmentDefinition has been deleted")
					Eventually(func() bool {
						_, err := nadClient.NetworkAttachmentDefinitions(f.Namespace.Name).Get(context.Background(), testUdnName, metav1.GetOptions{})
						return err != nil && kerrors.IsNotFound(err)
					}, deleteNetworkTimeout, deleteNetworkInterval).Should(BeTrue(),
						"NetworkAttachmentDefinition should be deleted following UserDefinedNetwork deletion")
				})
			})
		})

		It("should correctly report subsystem error on node subnet allocation", func() {
			cs = f.ClientSet

			nodes, err := e2enode.GetReadySchedulableNodes(context.TODO(), cs)
			framework.ExpectNoError(err)

			By("create tests UserDefinedNetwork")
			// create network that only has 2 node subnets (/24 cluster subnet has only 2 /25 node subnets)
			udnManifest := `
apiVersion: k8s.ovn.org/v1
kind: UserDefinedNetwork
metadata:
  name: ` + testUdnName + `
spec:
  topology: "Layer3"
  layer3:
    role: Secondary
    subnets: 
      - cidr: "10.10.100.0/24"
        hostSubnet: 25
`
			cleanup, err := createManifest(f.Namespace.Name, udnManifest)
			defer cleanup()
			Expect(err).NotTo(HaveOccurred())
			Expect(waitForUserDefinedNetworkReady(f.Namespace.Name, testUdnName, 5*time.Second)).To(Succeed())

			conditionsJSON, err := e2ekubectl.RunKubectl(f.Namespace.Name, "get", "userdefinednetwork", testUdnName, "-o", "jsonpath={.status.conditions}")
			Expect(err).NotTo(HaveOccurred())
			var actualConditions []metav1.Condition
			Expect(json.Unmarshal([]byte(conditionsJSON), &actualConditions)).To(Succeed())

			netAllocationCondition := "NetworkAllocationSucceeded"

			if len(nodes.Items) <= 2 {
				By("when cluster has <= 2 nodes, no error is expected")
				found := false
				for _, condition := range actualConditions {
					if condition.Type == netAllocationCondition && condition.Status == metav1.ConditionTrue {
						found = true
					}
				}
				Expect(found).To(BeTrue(), "NetworkAllocationSucceeded condition should be True when cluster has <= 2 nodes")
			} else {
				By("when cluster has > 2 nodes, error is expected")
				found := false
				for _, condition := range actualConditions {
					if condition.Type == netAllocationCondition && condition.Status == metav1.ConditionFalse {
						found = true
					}
				}
				Expect(found).To(BeTrue(), "NetworkAllocationSucceeded condition should be False when cluster has > 2 nodes")
				events, err := cs.CoreV1().Events(f.Namespace.Name).List(context.Background(), metav1.ListOptions{})
				Expect(err).NotTo(HaveOccurred())
				found = false
				for _, event := range events.Items {
					if event.Reason == "NetworkAllocationFailed" && event.LastTimestamp.After(time.Now().Add(-30*time.Second)) &&
						strings.Contains(event.Message, "error allocating network") {
						found = true
						break
					}
				}
				Expect(found).To(BeTrue(), "should have found an event for failed node allocation")
			}
		})
	})

	It("when primary network exist, UserDefinedNetwork status should report not-ready", func() {
		const (
			primaryNadName = "cluster-primary-net"
			primaryUdnName = "primary-net"
		)

		By("create primary network NetworkAttachmentDefinition")
		primaryNetNad := generateNAD(newNetworkAttachmentConfig(networkAttachmentConfigParams{
			role:        "primary",
			topology:    "layer3",
			name:        primaryNadName,
			networkName: primaryNadName,
			cidr:        "10.10.100.0/24",
		}))
		_, err := nadClient.NetworkAttachmentDefinitions(f.Namespace.Name).Create(context.Background(), primaryNetNad, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		By("create primary network UserDefinedNetwork")
		cleanup, err := createManifest(f.Namespace.Name, newPrimaryUserDefinedNetworkManifest(primaryUdnName))
		DeferCleanup(cleanup)
		Expect(err).NotTo(HaveOccurred())

		conditionsJSON, err := e2ekubectl.RunKubectl(f.Namespace.Name, "get", "userdefinednetwork", primaryUdnName, "-o", "jsonpath={.status.conditions}")
		Expect(err).NotTo(HaveOccurred())
		var actualConditions []metav1.Condition
		Expect(json.Unmarshal([]byte(conditionsJSON), &actualConditions)).To(Succeed())

		Expect(actualConditions[0].Type).To(Equal("NetworkReady"))
		Expect(actualConditions[0].Status).To(Equal(metav1.ConditionFalse))
		Expect(actualConditions[0].Reason).To(Equal("SyncError"))
		expectedMessage := fmt.Sprintf("primary network already exist in namespace %q: %q", f.Namespace.Name, primaryNadName)
		Expect(actualConditions[0].Message).To(Equal(expectedMessage))
	})

	Context("ClusterUserDefinedNetwork CRD Controller", func() {
		const (
			testClusterUdnName                = "test-cluster-net"
			clusterUserDefinedNetworkResource = "clusteruserdefinednetwork"
		)
		var (
			testTenantNamespaces []string
		)
		BeforeEach(func() {
			testTenantNamespaces = []string{
				f.Namespace.Name + "blue",
				f.Namespace.Name + "red",
			}

			By("Creating test tenants namespaces")
			for _, nsName := range testTenantNamespaces {
				_, err := cs.CoreV1().Namespaces().Create(context.Background(), &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}}, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())
				DeferCleanup(func() error {
					err := cs.CoreV1().Namespaces().Delete(context.Background(), nsName, metav1.DeleteOptions{})
					return err
				})
			}
		})

		BeforeEach(func() {
			By("create test CR")
			cleanup, err := createManifest("", newClusterUDNManifest(testClusterUdnName, testTenantNamespaces...))
			DeferCleanup(func() error {
				cleanup()
				_, _ = e2ekubectl.RunKubectl("", "delete", clusterUserDefinedNetworkResource, testClusterUdnName)
				return nil
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(waitForClusterUserDefinedNetworkReady(testClusterUdnName, 5*time.Second)).To(Succeed())
		})

		It("should create NAD according to spec in each target namespace and report active namespaces", func() {
			assertClusterUDNStatusReportsActiveNamespaces(testClusterUdnName, testTenantNamespaces...)

			udnUidRaw, err := e2ekubectl.RunKubectl("", "get", clusterUserDefinedNetworkResource, testClusterUdnName, "-o", "jsonpath='{.metadata.uid}'")
			Expect(err).NotTo(HaveOccurred(), "should get the ClsuterUserDefinedNetwork UID")
			testUdnUID := strings.Trim(udnUidRaw, "'")

			By("verify a NetworkAttachmentDefinition is created according to spec")
			for _, testNsName := range testTenantNamespaces {
				assertClusterNADManifest(nadClient, testNsName, testClusterUdnName, testUdnUID)
			}
		})

		It("when CR is deleted, should delete all managed NAD in each target namespace", func() {
			By("delete test CR")
			_, err := e2ekubectl.RunKubectl("", "delete", clusterUserDefinedNetworkResource, testClusterUdnName)
			Expect(err).NotTo(HaveOccurred())

			for _, nsName := range testTenantNamespaces {
				By(fmt.Sprintf("verify a NAD has been deleted from namesapce %q", nsName))
				Eventually(func() bool {
					_, err := nadClient.NetworkAttachmentDefinitions(nsName).Get(context.Background(), testClusterUdnName, metav1.GetOptions{})
					return err != nil && kerrors.IsNotFound(err)
				}, time.Second*3, time.Second*1).Should(BeTrue(),
					"NADs in target namespaces should be deleted following ClusterUserDefinedNetwork deletion")
			}
		})

		It("should create NAD in new created namespaces that apply to namespace-selector", func() {
			testNewNs := f.Namespace.Name + "green"

			By("add new target namespace to CR namespace-selector")
			patch := fmt.Sprintf(`[{"op": "add", "path": "./spec/namespaceSelector/matchExpressions/0/values/-", "value": "%s"}]`, testNewNs)
			_, err := e2ekubectl.RunKubectl("", "patch", clusterUserDefinedNetworkResource, testClusterUdnName, "--type=json", "-p="+patch)
			Expect(err).NotTo(HaveOccurred())
			Expect(waitForClusterUserDefinedNetworkReady(testClusterUdnName, 5*time.Second)).To(Succeed())
			assertClusterUDNStatusReportsActiveNamespaces(testClusterUdnName, testTenantNamespaces...)

			By("create the new target namespace")
			_, err = cs.CoreV1().Namespaces().Create(context.Background(), &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testNewNs}}, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() error {
				err := cs.CoreV1().Namespaces().Delete(context.Background(), testNewNs, metav1.DeleteOptions{})
				return err
			})

			expectedActiveNamespaces := append(testTenantNamespaces, testNewNs)
			assertClusterUDNStatusReportsActiveNamespaces(testClusterUdnName, expectedActiveNamespaces...)

			udnUidRaw, err := e2ekubectl.RunKubectl("", "get", clusterUserDefinedNetworkResource, testClusterUdnName, "-o", "jsonpath='{.metadata.uid}'")
			Expect(err).NotTo(HaveOccurred(), "should get the ClsuterUserDefinedNetwork UID")
			testUdnUID := strings.Trim(udnUidRaw, "'")

			By("verify a NAD exist in new namespace according to spec")
			assertClusterNADManifest(nadClient, testNewNs, testClusterUdnName, testUdnUID)
		})

		When("namespace-selector is mutated", func() {
			It("should create NAD in namespaces that apply to mutated namespace-selector", func() {
				testNewNs := f.Namespace.Name + "green"

				By("create new namespace")
				_, err := cs.CoreV1().Namespaces().Create(context.Background(), &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testNewNs}}, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())
				DeferCleanup(func() error {
					err := cs.CoreV1().Namespaces().Delete(context.Background(), testNewNs, metav1.DeleteOptions{})
					return err
				})

				By("add new namespace to CR namespace-selector")
				patch := fmt.Sprintf(`[{"op": "add", "path": "./spec/namespaceSelector/matchExpressions/0/values/-", "value": "%s"}]`, testNewNs)
				_, err = e2ekubectl.RunKubectl("", "patch", clusterUserDefinedNetworkResource, testClusterUdnName, "--type=json", "-p="+patch)
				Expect(err).NotTo(HaveOccurred())

				By("verify status reports the new added namespace as active")
				expectedActiveNs := append(testTenantNamespaces, testNewNs)
				assertClusterUDNStatusReportsActiveNamespaces(testClusterUdnName, expectedActiveNs...)

				By("verify a NAD is created in new target namespace according to spec")
				udnUidRaw, err := e2ekubectl.RunKubectl("", "get", clusterUserDefinedNetworkResource, testClusterUdnName, "-o", "jsonpath='{.metadata.uid}'")
				Expect(err).NotTo(HaveOccurred(), "should get the ClusterUserDefinedNetwork UID")
				testUdnUID := strings.Trim(udnUidRaw, "'")
				assertClusterNADManifest(nadClient, testNewNs, testClusterUdnName, testUdnUID)
			})

			It("should delete managed NAD in namespaces that no longer apply to namespace-selector", func() {
				By("remove one active namespace from CR namespace-selector")
				activeTenantNs := testTenantNamespaces[1]
				patch := fmt.Sprintf(`[{"op": "replace", "path": "./spec/namespaceSelector/matchExpressions/0/values", "value": [%q]}]`, activeTenantNs)
				_, err := e2ekubectl.RunKubectl("", "patch", clusterUserDefinedNetworkResource, testClusterUdnName, "--type=json", "-p="+patch)
				Expect(err).NotTo(HaveOccurred())

				By("verify status reports remained target namespaces only as active")
				expectedActiveNs := []string{activeTenantNs}
				assertClusterUDNStatusReportsActiveNamespaces(testClusterUdnName, expectedActiveNs...)

				removedTenantNs := testTenantNamespaces[0]
				By("verify managed NAD not exist in removed target namespace")
				Eventually(func() bool {
					_, err := nadClient.NetworkAttachmentDefinitions(removedTenantNs).Get(context.Background(), testClusterUdnName, metav1.GetOptions{})
					return err != nil && kerrors.IsNotFound(err)
				}, time.Second*300, time.Second*1).Should(BeTrue(),
					"NAD in target namespaces should be deleted following CR namespace-selector mutation")
			})
		})

		Context("pod connected to ClusterUserDefinedNetwork", func() {
			const testPodName = "test-pod-cluster-udn"

			var (
				udnInUseDeleteTimeout = 65 * time.Second
				deleteNetworkTimeout  = 5 * time.Second
				deleteNetworkInterval = 1 * time.Second

				inUseNetTestTenantNamespace string
			)

			BeforeEach(func() {
				inUseNetTestTenantNamespace = testTenantNamespaces[0]

				By("create pod in one of the test tenant namespaces")
				networkAttachments := []nadapi.NetworkSelectionElement{
					{Name: testClusterUdnName, Namespace: inUseNetTestTenantNamespace},
				}
				cfg := podConfig(testPodName, withNetworkAttachment(networkAttachments))
				cfg.namespace = inUseNetTestTenantNamespace
				runUDNPod(cs, inUseNetTestTenantNamespace, *cfg, nil)
			})

			It("CR & managed NADs cannot be deleted when being used", func() {
				By("verify CR cannot be deleted")
				cmd := e2ekubectl.NewKubectlCommand("", "delete", clusterUserDefinedNetworkResource, testClusterUdnName)
				cmd.WithTimeout(time.NewTimer(deleteNetworkTimeout).C)
				_, err := cmd.Exec()
				Expect(err).To(HaveOccurred(), "should fail to delete ClusterUserDefinedNetwork when used")

				By("verify CR associate NAD cannot be deleted")
				Eventually(func() error {
					ctx, cancel := context.WithTimeout(context.Background(), deleteNetworkTimeout)
					defer cancel()
					_ = nadClient.NetworkAttachmentDefinitions(inUseNetTestTenantNamespace).Delete(ctx, testClusterUdnName, metav1.DeleteOptions{})
					_, err := nadClient.NetworkAttachmentDefinitions(inUseNetTestTenantNamespace).Get(ctx, testClusterUdnName, metav1.GetOptions{})
					return err
				}).ShouldNot(HaveOccurred(),
					"should fail to delete UserDefinedNetwork associated NetworkAttachmentDefinition when used")

				By("verify CR status reports consuming pod")
				conditionsJSON, err := e2ekubectl.RunKubectl("", "get", clusterUserDefinedNetworkResource, testClusterUdnName, "-o", "jsonpath='{.status.conditions}'")
				Expect(err).NotTo(HaveOccurred())
				assertClusterUDNStatusReportConsumers(conditionsJSON, testClusterUdnName, inUseNetTestTenantNamespace, testPodName)

				By("delete test pod")
				err = cs.CoreV1().Pods(inUseNetTestTenantNamespace).Delete(context.Background(), testPodName, metav1.DeleteOptions{})
				Expect(err).ToNot(HaveOccurred())

				By("verify CR is gone")
				Eventually(func() error {
					_, err := e2ekubectl.RunKubectl("", "get", clusterUserDefinedNetworkResource, testClusterUdnName)
					return err
				}, udnInUseDeleteTimeout, deleteNetworkInterval).Should(HaveOccurred(),
					"ClusterUserDefinedNetwork should be deleted following test pod deletion")

				By("verify CR associate NADs are gone")
				for _, nsName := range testTenantNamespaces {
					Eventually(func() bool {
						_, err := nadClient.NetworkAttachmentDefinitions(nsName).Get(context.Background(), testClusterUdnName, metav1.GetOptions{})
						return err != nil && kerrors.IsNotFound(err)
					}, deleteNetworkTimeout, deleteNetworkInterval).Should(BeTrue(),
						"NADs in target namespaces should be deleted following ClusterUserDefinedNetwork deletion")
				}
			})
		})
	})

	It("when primary network exist, ClusterUserDefinedNetwork status should report not-ready", func() {
		testTenantNamespaces := []string{
			f.Namespace.Name + "blue",
			f.Namespace.Name + "red",
		}
		By("Creating test tenants namespaces")
		for _, nsName := range testTenantNamespaces {
			_, err := cs.CoreV1().Namespaces().Create(context.Background(), &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}}, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() error {
				err := cs.CoreV1().Namespaces().Delete(context.Background(), nsName, metav1.DeleteOptions{})
				return err
			})
		}

		By("create primary network NAD in one of the tenant namespaces")
		const primaryNadName = "some-primary-net"
		primaryNetTenantNs := testTenantNamespaces[0]
		primaryNetNad := generateNAD(newNetworkAttachmentConfig(networkAttachmentConfigParams{
			role:        "primary",
			topology:    "layer3",
			name:        primaryNadName,
			networkName: primaryNadName,
			cidr:        "10.10.100.0/24",
		}))
		_, err := nadClient.NetworkAttachmentDefinitions(primaryNetTenantNs).Create(context.Background(), primaryNetNad, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		By("create primary Cluster UDN CR")
		const cudnName = "primary-net"
		cleanup, err := createManifest(f.Namespace.Name, newPrimaryClusterUDNManifest(cudnName, testTenantNamespaces...))
		DeferCleanup(func() error {
			cleanup()
			_, _ = e2ekubectl.RunKubectl("", "delete", "clusteruserdefinednetwork", cudnName)
			return nil
		})

		conditionsJSON, err := e2ekubectl.RunKubectl(f.Namespace.Name, "get", "clusteruserdefinednetwork", cudnName, "-o", "jsonpath={.status.conditions}")
		Expect(err).NotTo(HaveOccurred())
		var actualConditions []metav1.Condition
		Expect(json.Unmarshal([]byte(conditionsJSON), &actualConditions)).To(Succeed())

		Expect(actualConditions[0].Type).To(Equal("NetworkReady"))
		Expect(actualConditions[0].Status).To(Equal(metav1.ConditionFalse))
		Expect(actualConditions[0].Reason).To(Equal("NetworkAttachmentDefinitionSyncError"))
		expectedMessage := fmt.Sprintf("primary network already exist in namespace %q: %q", primaryNetTenantNs, primaryNadName)
		Expect(actualConditions[0].Message).To(Equal(expectedMessage))
	})

	Context("pod2Egress on a user defined primary network", func() {
		const (
			externalContainerName = "ovn-k-egress-test-helper"
		)
		var externalIpv4, externalIpv6 string
		BeforeEach(func() {
			externalIpv4, externalIpv6 = createClusterExternalContainer(
				externalContainerName,
				"registry.k8s.io/e2e-test-images/agnhost:2.45",
				runExternalContainerCmd(),
				httpServerContainerCmd(port),
			)

			DeferCleanup(func() {
				deleteClusterExternalContainer(externalContainerName)
			})
		})
		DescribeTableSubtree("created using",
			func(createNetworkFn func(c networkAttachmentConfigParams) error) {

				DescribeTable(
					"can be accessed to from the pods running in the Kubernetes cluster",
					func(netConfigParams networkAttachmentConfigParams, clientPodConfig podConfiguration) {
						if netConfigParams.topology == "layer2" && !isInterconnectEnabled() {
							const upstreamIssue = "https://github.com/ovn-org/ovn-kubernetes/issues/4642"
							e2eskipper.Skipf(
								"Egress e2e tests for layer2 topologies are known to fail on non-IC deployments. Upstream issue: %s", upstreamIssue,
							)
						}
						clientPodConfig.namespace = f.Namespace.Name

						By("creating the network")
						netConfigParams.namespace = f.Namespace.Name
						Expect(createNetworkFn(netConfigParams)).To(Succeed())

						By("instantiating the client pod")
						clientPod, err := cs.CoreV1().Pods(clientPodConfig.namespace).Create(
							context.Background(),
							generatePodSpec(clientPodConfig),
							metav1.CreateOptions{},
						)
						Expect(err).NotTo(HaveOccurred())
						Expect(clientPod).NotTo(BeNil())

						By("asserting the client pod reaches the `Ready` state")
						var updatedPod *v1.Pod
						Eventually(func() v1.PodPhase {
							updatedPod, err = cs.CoreV1().Pods(f.Namespace.Name).Get(context.Background(), clientPod.GetName(), metav1.GetOptions{})
							if err != nil {
								return v1.PodFailed
							}
							return updatedPod.Status.Phase
						}, 2*time.Minute, 6*time.Second).Should(Equal(v1.PodRunning))
						framework.Logf("Client pod was created on node %s", updatedPod.Spec.NodeName)

						By("asserting UDN pod is connected to UDN network")
						podAnno, err := unmarshalPodAnnotation(updatedPod.Annotations, f.Namespace.Name+"/"+userDefinedNetworkName)
						Expect(err).NotTo(HaveOccurred())
						framework.Logf("Client pod's annotation for network %s is %v", userDefinedNetworkName, podAnno)

						Expect(podAnno.Routes).To(HaveLen(expectedNumberOfRoutes(netConfigParams)))

						assertClientExternalConnectivity(clientPodConfig, externalIpv4, externalIpv6, port)
					},
					Entry("by one pod over a layer2 network",
						networkAttachmentConfigParams{
							name:     userDefinedNetworkName,
							topology: "layer2",
							cidr:     correctCIDRFamily(userDefinedNetworkIPv4Subnet, userDefinedNetworkIPv6Subnet),
							role:     "primary",
						},
						*podConfig("client-pod"),
					),
					Entry("by one pod over a layer3 network",
						networkAttachmentConfigParams{
							name:     userDefinedNetworkName,
							topology: "layer3",
							cidr:     correctCIDRFamily(userDefinedNetworkIPv4Subnet, userDefinedNetworkIPv6Subnet),
							role:     "primary",
						},
						*podConfig("client-pod"),
					),
				)
			},
			Entry("NetworkAttachmentDefinitions", func(c networkAttachmentConfigParams) error {
				netConfig := newNetworkAttachmentConfig(c)
				nad := generateNAD(netConfig)
				_, err := nadClient.NetworkAttachmentDefinitions(f.Namespace.Name).Create(context.Background(), nad, metav1.CreateOptions{})
				return err
			}),
			Entry("UserDefinedNetwork", func(c networkAttachmentConfigParams) error {
				udnManifest := generateUserDefinedNetworkManifest(&c)
				cleanup, err := createManifest(f.Namespace.Name, udnManifest)
				DeferCleanup(cleanup)
				Expect(waitForUserDefinedNetworkReady(f.Namespace.Name, c.name, 5*time.Second)).To(Succeed())
				return err
			}),
			Entry("ClusterUserDefinedNetwork", func(c networkAttachmentConfigParams) error {
				cudnManifest := generateClusterUserDefinedNetworkManifest(&c)
				cleanup, err := createManifest("", cudnManifest)
				DeferCleanup(func() {
					cleanup()
					By("delete pods in test namespace to unblock CUDN CR & associate NAD deletion")
					_, err := e2ekubectl.RunKubectl(c.namespace, "delete", "pod", "--all")
					Expect(err).NotTo(HaveOccurred())
					_, err = e2ekubectl.RunKubectl("", "delete", "clusteruserdefinednetwork", c.name)
					Expect(err).NotTo(HaveOccurred())
				})
				Expect(waitForClusterUserDefinedNetworkReady(c.name, 5*time.Second)).To(Succeed())
				return err
			}),
		)
	})

	Context("UDN Pod", func() {
		const (
			testUdnName = "test-net"
			testPodName = "test-pod-udn"
		)

		var udnPod *v1.Pod

		BeforeEach(func() {
			By("create tests UserDefinedNetwork")
			cleanup, err := createManifest(f.Namespace.Name, newPrimaryUserDefinedNetworkManifest(testUdnName))
			DeferCleanup(cleanup)
			Expect(err).NotTo(HaveOccurred())
			Expect(waitForUserDefinedNetworkReady(f.Namespace.Name, testUdnName, 5*time.Second)).To(Succeed())
			By("create UDN pod")
			cfg := podConfig(testPodName, withCommand(func() []string {
				return httpServerContainerCmd(port)
			}))
			cfg.namespace = f.Namespace.Name
			udnPod = runUDNPod(cs, f.Namespace.Name, *cfg, nil)
		})

		It("should react to k8s.ovn.org/open-default-ports annotations changes", func() {
			By("Creating second namespace for default network pod")
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

			By("creating default network client pod")
			defaultClientPod, err := createPod(f, "default-net-client-pod", workerOneNodeName,
				defaultNetNamespace, []string{}, nil)
			Expect(err).NotTo(HaveOccurred())

			udnIPv4, udnIPv6, err := podIPsForDefaultNetwork(
				cs,
				f.Namespace.Name,
				udnPod.GetName(),
			)
			Expect(err).NotTo(HaveOccurred())

			By(fmt.Sprintf("verify default network client pod can't access UDN pod on port %d", port))
			for _, destIP := range []string{udnIPv4, udnIPv6} {
				if destIP == "" {
					continue
				}
				By("checking the default network pod can't reach UDN pod on IP " + destIP)
				Consistently(func() bool {
					return connectToServer(podConfiguration{namespace: defaultClientPod.Namespace, name: defaultClientPod.Name}, destIP, port) != nil
				}, 5*time.Second).Should(BeTrue())
			}

			By("Open UDN pod port")

			udnPod.Annotations[openDefaultPortsAnnotation] = fmt.Sprintf(
				`- protocol: tcp
  port: %d`, port)
			udnPod, err = cs.CoreV1().Pods(udnPod.Namespace).Update(context.Background(), udnPod, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())

			By(fmt.Sprintf("verify default network client pod can access UDN pod on open port %d", port))
			for _, destIP := range []string{udnIPv4, udnIPv6} {
				if destIP == "" {
					continue
				}
				By("checking the default network pod can't reach UDN pod on IP " + destIP)
				Eventually(func() bool {
					return connectToServer(podConfiguration{namespace: defaultClientPod.Namespace, name: defaultClientPod.Name}, destIP, port) == nil
				}, 5*time.Second).Should(BeTrue())
			}

			By("Update UDN pod port with the wrong syntax")
			// this should clean up open ports and throw an event
			udnPod.Annotations[openDefaultPortsAnnotation] = fmt.Sprintf(
				`- protocol: ppp
  port: %d`, port)
			udnPod, err = cs.CoreV1().Pods(udnPod.Namespace).Update(context.Background(), udnPod, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())

			By(fmt.Sprintf("verify default network client pod can't access UDN pod on port %d", port))
			for _, destIP := range []string{udnIPv4, udnIPv6} {
				if destIP == "" {
					continue
				}
				By("checking the default network pod can't reach UDN pod on IP " + destIP)
				Eventually(func() bool {
					return connectToServer(podConfiguration{namespace: defaultClientPod.Namespace, name: defaultClientPod.Name}, destIP, port) != nil
				}, 5*time.Second).Should(BeTrue())
			}
			By("Verify syntax error is reported via event")
			events, err := cs.CoreV1().Events(udnPod.Namespace).List(context.Background(), metav1.ListOptions{})
			found := false
			for _, event := range events.Items {
				if event.Reason == "ErrorUpdatingResource" && strings.Contains(event.Message, "invalid protocol ppp") {
					found = true
					break
				}
			}
			Expect(found).To(BeTrue(), "should have found an event for invalid protocol")
		})
	})
})

var nadToUdnParams = map[string]string{
	"primary":   "Primary",
	"secondary": "Secondary",
	"layer2":    "Layer2",
	"layer3":    "Layer3",
}

func generateUserDefinedNetworkManifest(params *networkAttachmentConfigParams) string {
	subnets := generateSubnetsYaml(params)
	return `
apiVersion: k8s.ovn.org/v1
kind: UserDefinedNetwork
metadata:
  name: ` + params.name + `
spec:
  topology: ` + nadToUdnParams[params.topology] + `
  ` + params.topology + `: 
    role: ` + nadToUdnParams[params.role] + `
    subnets: ` + subnets + `
`
}

func generateClusterUserDefinedNetworkManifest(params *networkAttachmentConfigParams) string {
	subnets := generateSubnetsYaml(params)
	return `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: ` + params.name + `
spec:
  namespaceSelector:
    matchExpressions:
    - key: kubernetes.io/metadata.name
      operator: In
      values: [` + params.namespace + `]
  network:
    topology: ` + nadToUdnParams[params.topology] + `
    ` + params.topology + `: 
      role: ` + nadToUdnParams[params.role] + `
      subnets: ` + subnets + `
`
}

func generateSubnetsYaml(params *networkAttachmentConfigParams) string {
	if params.topology == "layer3" {
		l3Subnets := generateLayer3Subnets(params.cidr)
		return fmt.Sprintf("[%s]", strings.Join(l3Subnets, ","))
	}
	return fmt.Sprintf("[%s]", params.cidr)
}

func generateLayer3Subnets(cidrs string) []string {
	cidrList := strings.Split(cidrs, ",")
	var subnets []string
	for _, cidr := range cidrList {
		cidrSplit := strings.Split(cidr, "/")
		switch len(cidrSplit) {
		case 2:
			subnets = append(subnets, fmt.Sprintf(`{cidr: "%s/%s"}`, cidrSplit[0], cidrSplit[1]))
		case 3:
			subnets = append(subnets, fmt.Sprintf(`{cidr: "%s/%s", hostSubnet: %q }`, cidrSplit[0], cidrSplit[1], cidrSplit[2]))
		default:
			panic(fmt.Sprintf("invalid layer3 subnet: %v", cidr))
		}
	}
	return subnets
}

func waitForUserDefinedNetworkReady(namespace, name string, timeout time.Duration) error {
	_, err := e2ekubectl.RunKubectl(namespace, "wait", "userdefinednetwork", name, "--for", "condition=NetworkReady=True", "--timeout", timeout.String())
	return err
}

func waitForClusterUserDefinedNetworkReady(name string, timeout time.Duration) error {
	_, err := e2ekubectl.RunKubectl("", "wait", "clusteruserdefinednetwork", name, "--for", "condition=NetworkReady=True", "--timeout", timeout.String())
	return err
}

func createManifest(namespace, manifest string) (func(), error) {
	path := "test-" + randString(5) + ".yaml"
	if err := os.WriteFile(path, []byte(manifest), 0644); err != nil {
		framework.Failf("Unable to write yaml to disk: %v", err)
	}
	cleanup := func() {
		if err := os.Remove(path); err != nil {
			framework.Logf("Unable to remove yaml from disk: %v", err)
		}
	}
	_, err := e2ekubectl.RunKubectl(namespace, "create", "-f", path)
	if err != nil {
		return cleanup, err
	}
	return cleanup, nil
}

func assertL2SecondaryNetAttachDefManifest(nadClient nadclient.K8sCniCncfIoV1Interface, namespace, udnName, udnUID string) {
	nad, err := nadClient.NetworkAttachmentDefinitions(namespace).Get(context.Background(), udnName, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())

	ExpectWithOffset(1, nad.Name).To(Equal(udnName))
	ExpectWithOffset(1, nad.Namespace).To(Equal(namespace))
	ExpectWithOffset(1, nad.OwnerReferences).To(Equal([]metav1.OwnerReference{{
		APIVersion:         "k8s.ovn.org/v1",
		Kind:               "UserDefinedNetwork",
		Name:               "test-net",
		UID:                types.UID(udnUID),
		BlockOwnerDeletion: pointer.Bool(true),
		Controller:         pointer.Bool(true),
	}}))
	expectedNetworkName := namespace + "." + udnName
	expectedNadName := namespace + "/" + udnName
	ExpectWithOffset(1, nad.Spec.Config).To(MatchJSON(`{
		"cniVersion":"1.0.0",
		"type": "ovn-k8s-cni-overlay",
		"name": "` + expectedNetworkName + `",
		"netAttachDefName": "` + expectedNadName + `",
		"topology": "layer2",
		"role": "secondary",
		"subnets": "10.10.100.0/24"
	}`))
}

func assertUDNStatusReportsConsumers(udnNamesapce, udnName, expectedPodName string) {
	conditionsRaw, err := e2ekubectl.RunKubectl(udnNamesapce, "get", "userdefinednetwork", udnName, "-o", "jsonpath='{.status.conditions}'")
	Expect(err).NotTo(HaveOccurred())
	conditionsRaw = strings.ReplaceAll(conditionsRaw, `\`, ``)
	conditionsRaw = strings.ReplaceAll(conditionsRaw, `'`, ``)
	var conditions []metav1.Condition
	Expect(json.Unmarshal([]byte(conditionsRaw), &conditions)).To(Succeed())
	conditions = normalizeConditions(conditions)
	expectedMsg := fmt.Sprintf("failed to delete NetworkAttachmentDefinition [%[1]s/%[2]s]: network in use by the following pods: [%[1]s/%[3]s]",
		udnNamesapce, udnName, expectedPodName)
	found := false
	for _, condition := range conditions {
		if found, _ = Equal(metav1.Condition{
			Type:    "NetworkReady",
			Status:  "False",
			Reason:  "SyncError",
			Message: expectedMsg,
		}).Match(condition); found {
			break
		}
	}
	Expect(found).To(BeTrue(), "expected condition not found in %v", conditions)
}

func normalizeConditions(conditions []metav1.Condition) []metav1.Condition {
	for i := range conditions {
		t := metav1.NewTime(time.Time{})
		conditions[i].LastTransitionTime = t
	}
	return conditions
}

func assertClusterNADManifest(nadClient nadclient.K8sCniCncfIoV1Interface, namespace, udnName, udnUID string) {
	nad, err := nadClient.NetworkAttachmentDefinitions(namespace).Get(context.Background(), udnName, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())

	ExpectWithOffset(1, nad.Name).To(Equal(udnName))
	ExpectWithOffset(1, nad.Namespace).To(Equal(namespace))
	ExpectWithOffset(1, nad.OwnerReferences).To(Equal([]metav1.OwnerReference{{
		APIVersion:         "k8s.ovn.org/v1",
		Kind:               "ClusterUserDefinedNetwork",
		Name:               udnName,
		UID:                types.UID(udnUID),
		BlockOwnerDeletion: pointer.Bool(true),
		Controller:         pointer.Bool(true),
	}}))
	ExpectWithOffset(1, nad.Labels).To(Equal(map[string]string{"k8s.ovn.org/user-defined-network": ""}))
	ExpectWithOffset(1, nad.Finalizers).To(Equal([]string{"k8s.ovn.org/user-defined-network-protection"}))

	expectedNetworkName := "cluster.udn." + udnName
	expectedNadName := namespace + "/" + udnName
	ExpectWithOffset(1, nad.Spec.Config).To(MatchJSON(`{
		"cniVersion":"1.0.0",
		"type": "ovn-k8s-cni-overlay",
		"name": "` + expectedNetworkName + `",
		"netAttachDefName": "` + expectedNadName + `",
		"topology": "layer2",
		"role": "secondary",
		"subnets": "10.100.0.0/16"
	}`))
}

func assertClusterUDNStatusReportsActiveNamespaces(cudnName string, expectedActiveNsNames ...string) {
	conditionsRaw, err := e2ekubectl.RunKubectl("", "get", "clusteruserdefinednetwork", cudnName, "-o", "jsonpath='{.status.conditions}'")
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	conditionsRaw = strings.ReplaceAll(conditionsRaw, `\`, ``)
	conditionsRaw = strings.ReplaceAll(conditionsRaw, `'`, ``)
	var conditions []metav1.Condition
	ExpectWithOffset(1, json.Unmarshal([]byte(conditionsRaw), &conditions)).To(Succeed())

	c := conditions[0]
	// equality matcher cannot be used since condition message namespaces order is inconsistent
	ExpectWithOffset(1, c.Type).Should(Equal("NetworkReady"))
	ExpectWithOffset(1, c.Status).Should(Equal(metav1.ConditionTrue))
	ExpectWithOffset(1, c.Reason).Should(Equal("NetworkAttachmentDefinitionReady"))

	ExpectWithOffset(1, c.Message).To(ContainSubstring("NetworkAttachmentDefinition has been created in following namespaces:"))
	for _, ns := range expectedActiveNsNames {
		Expect(c.Message).To(ContainSubstring(ns))
	}
}

func assertClusterUDNStatusReportConsumers(conditionsJSON, udnName, udnNamespace, expectedPodName string) {
	conditionsJSON = strings.ReplaceAll(conditionsJSON, `\`, ``)
	conditionsJSON = strings.ReplaceAll(conditionsJSON, `'`, ``)

	var conditions []metav1.Condition
	ExpectWithOffset(1, json.Unmarshal([]byte(conditionsJSON), &conditions)).To(Succeed())
	conditions = normalizeConditions(conditions)
	expectedMsg := fmt.Sprintf("failed to delete NetworkAttachmentDefinition [%[1]s/%[2]s]: network in use by the following pods: [%[1]s/%[3]s]",
		udnNamespace, udnName, expectedPodName)
	ExpectWithOffset(1, conditions).To(Equal([]metav1.Condition{
		{
			Type:    "NetworkReady",
			Status:  "False",
			Reason:  "NetworkAttachmentDefinitionSyncError",
			Message: expectedMsg,
		},
	}))
}

func newClusterUDNManifest(name string, targetNamespaces ...string) string {
	targetNs := strings.Join(targetNamespaces, ",")
	return `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: ` + name + `
spec:
  namespaceSelector:
    matchExpressions:
    - key: kubernetes.io/metadata.name
      operator: In
      values: [ ` + targetNs + ` ]
  network:
    topology: Layer2
    layer2:
      role: Secondary
      subnets: ["10.100.0.0/16"]
`
}

func newPrimaryClusterUDNManifest(name string, targetNamespaces ...string) string {
	targetNs := strings.Join(targetNamespaces, ",")
	return `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: ` + name + `
spec:
  namespaceSelector:
    matchExpressions:
    - key: kubernetes.io/metadata.name
      operator: In
      values: [ ` + targetNs + ` ]
  network:
    topology: Layer3
    layer3:
      role: Primary
      subnets: [{cidr: "10.100.0.0/16"}]
`
}

func newL2SecondaryUDNManifest(name string) string {
	return `
apiVersion: k8s.ovn.org/v1
kind: UserDefinedNetwork
metadata:
  name: ` + name + `
spec:
  topology: "Layer2"
  layer2:
    role: Secondary
    subnets: ["10.10.100.0/24"]
`
}

func newPrimaryUserDefinedNetworkManifest(name string) string {
	return `
apiVersion: k8s.ovn.org/v1
kind: UserDefinedNetwork
metadata:
  name: ` + name + `
spec:
  topology: Layer3
  layer3:
    role: Primary
    subnets: ` + generateCIDRforUDN()
}

func generateCIDRforUDN() string {
	cidr := `
    - cidr: 10.20.100.0/16
`
	if isIPv6Supported() && isIPv4Supported() {
		cidr = `
    - cidr: 10.20.100.0/16
    - cidr: 2014:100:200::0/60
`
	} else if isIPv6Supported() {
		cidr = `
    - cidr: 2014:100:200::0/60
`
	}
	return cidr

}

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

func withLabels(labels map[string]string) podOption {
	return func(pod *podConfiguration) {
		pod.labels = labels
	}
}

func withNetworkAttachment(networks []nadapi.NetworkSelectionElement) podOption {
	return func(pod *podConfiguration) {
		pod.attachments = networks
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

// connectToServerViaDefaultNetwork sends the traffic via the pod's default interface
func connectToServerViaDefaultNetwork(clientPodConfig podConfiguration, serverIP string, port int) error {
	_, err := e2ekubectl.RunKubectl(
		clientPodConfig.namespace,
		"exec",
		clientPodConfig.name,
		"--",
		"curl",
		"--connect-timeout",
		"2",
		"--interface",
		"eth0",
		net.JoinHostPort(serverIP, fmt.Sprintf("%d", port)),
	)
	return err
}

// assertClientExternalConnectivity checks if the client can connect to an externally created IP outside the cluster
func assertClientExternalConnectivity(clientPodConfig podConfiguration, externalIpv4 string, externalIpv6 string, port int) {
	if isIPv4Supported() {
		By("asserting the *client* pod can contact the server's v4 IP located outside the cluster")
		Eventually(func() error {
			return connectToServer(clientPodConfig, externalIpv4, port)
		}, 2*time.Minute, 6*time.Second).Should(Succeed())
	}

	if isIPv6Supported() {
		By("asserting the *client* pod can contact the server's v6 IP located outside the cluster")
		Eventually(func() error {
			return connectToServer(clientPodConfig, externalIpv6, port)
		}, 2*time.Minute, 6*time.Second).Should(Succeed())
	}
}

func runExternalContainerCmd() []string {
	return []string{"--network", "kind"}
}

func expectedNumberOfRoutes(netConfig networkAttachmentConfigParams) int {
	if netConfig.topology == "layer2" {
		if isIPv6Supported() && isIPv4Supported() {
			return 4 // 2 routes per family
		} else {
			return 2 //one family supported
		}
	}
	if isIPv6Supported() && isIPv4Supported() {
		return 6 // 3 v4 routes + 3 v6 routes for UDN
	}
	return 3 //only one family, each has 3 routes
}
