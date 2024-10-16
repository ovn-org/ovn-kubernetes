package e2e

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"math/big"
	"math/rand"
	"net"
	"os"
	"path"
	"regexp"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2epodoutput "k8s.io/kubernetes/test/e2e/framework/pod/output"
	e2eservice "k8s.io/kubernetes/test/e2e/framework/service"
	utilnet "k8s.io/utils/net"
)

var _ = Describe("Node IP and MAC address migration", func() {
	const (
		namespacePrefix            = "node-ip-migration"
		podWorkerNodeName          = "primary"
		podSecondaryWorkerNodeName = "secondary"
		pollingTimeout             = 120
		pollingInterval            = 10
		settleTimeout              = 10
		egressIPYaml               = "egressip.yaml"
		externalContainerImage     = "registry.k8s.io/e2e-test-images/agnhost:2.26"
		ciNetworkName              = "kind"
		externalContainerName      = "ip-migration-external"
		externalContainerPort      = "80"
		externalContainerEndpoint  = "/clientip"
		egressIPYamlTemplate       = `apiVersion: k8s.ovn.org/v1
kind: EgressIP
metadata:
    name: %s
spec:
    egressIPs:
    - %s
    podSelector:
        matchLabels:
            %s: %s
    namespaceSelector:
        matchLabels:
            kubernetes.io/metadata.name: %s
`
		serviceName = "testservice"
		podName     = "backend"
		migratedMAC = "02:42:ac:12:d1:d1"
	)

	var (
		tmpDirIPMigration      string
		workerNodeIPs          map[int]string
		workerNode             v1.Node
		workerNodeMAC          net.HardwareAddr
		secondaryWorkerNodeIPs map[int]string
		secondaryWorkerNode    v1.Node
		externalContainerIPs   map[int]string
		podWorkerNode          *v1.Pod
		podSecondaryWorkerNode *v1.Pod
		migrationWorkerNodeIP  string
		rollbackNeeded         bool
		egressIP               string
		assignedNodePort       int32
		ovnkPod                v1.Pod

		podLabels = map[string]string{
			"app": "ip-migration-test",
		}
		podCommand               = []string{"/bin/bash", "-c", "/agnhost netexec --http-port 8000"}
		externalContainerCommand = []string{"netexec", "--http-port=" + externalContainerPort}

		updateKubeletIPAddressMsg = map[bool]string{
			true:  "update kubelet first, the IP address later",
			false: "update the IP address first, kubelet later",
		}

		f = wrappedTestFramework(namespacePrefix)

		udpPort  = int32(rand.Intn(1000) + 10000)
		udpPortS = fmt.Sprintf("%d", udpPort)
	)

	BeforeEach(func() {
		By("Creating the temp directory")
		var err error
		tmpDirIPMigration, err = os.MkdirTemp("", "e2e")
		Expect(err).NotTo(HaveOccurred())

		By("Selecting 2 random worker nodes from the list for IP address migration tests")
		// Get the primary worker node for IP address migration.
		// Get the secondary worker node to spawn another pod on for pod to pod reachability tests.
		var workerNodes *v1.NodeList
		Eventually(func(g Gomega) {
			workerNodes, err = e2enode.GetReadySchedulableNodes(context.TODO(), f.ClientSet)
			g.Expect(err).NotTo(HaveOccurred())
			e2enode.Filter(workerNodes, func(node v1.Node) bool {
				_, ok1 := node.Labels["node-role.kubernetes.io/control-plane"]
				_, ok2 := node.Labels["node-role.kubernetes.io/master"]
				return !ok1 && !ok2
			})
			g.Expect(len(workerNodes.Items)).Should(BeNumerically(">=", 2))
		}, pollingTimeout, pollingInterval).Should(Succeed())
		workerNode = workerNodes.Items[0]
		secondaryWorkerNode = workerNodes.Items[1]
		framework.Logf("Selected worker node %s and secondary worker node %s", workerNode.Name, secondaryWorkerNode.Name)

		workerNodeIPs = make(map[int]string)
		workerNodeIPs[4], workerNodeIPs[6] = getNodeInternalAddresses(&workerNode)
		secondaryWorkerNodeIPs = make(map[int]string)
		secondaryWorkerNodeIPs[4], secondaryWorkerNodeIPs[6] = getNodeInternalAddresses(&secondaryWorkerNode)
		framework.Logf("Found node IPs for worker node: %v. Found node IPs for secondary worker node: %v",
			workerNodeIPs, secondaryWorkerNodeIPs)

		By("Creating a cluster external container")
		externalContainerIPs = make(map[int]string)
		externalContainerIPs[4], externalContainerIPs[6] = createClusterExternalContainer(externalContainerName,
			externalContainerImage, []string{"--network", ciNetworkName, "-P"}, externalContainerCommand)
	})

	AfterEach(func() {
		By("Removing the external container")
		deleteClusterExternalContainer(externalContainerName)

		By("Removing the temp directory")
		Expect(os.RemoveAll(tmpDirIPMigration)).To(Succeed())
	})

	for _, ipAddrFamily := range []int{4, 6} {
		ipAddrFamily := ipAddrFamily // Required to avoid race conditions due to pointer assignment.
		When(fmt.Sprintf("the node IPv%d address is updated", ipAddrFamily), func() {
			BeforeEach(func() {
				By("Setting rollbackNeeded to false")
				rollbackNeeded = false

				By(fmt.Sprintf("Checking if IP address family %d should be working on this cluster", ipAddrFamily))
				if workerNodeIPs[ipAddrFamily] == "" {
					framework.Logf("IP address family %d not found in worker IPs: %v", ipAddrFamily, workerNodeIPs)
					Skip(fmt.Sprintf("IP address family %d is not supported on this cluster", ipAddrFamily))
				}

				By("Creating a test pod on both selected worker nodes")
				podWorkerNode = newAgnhostPodOnNode(
					podWorkerNodeName,
					workerNode.Name,
					podLabels,
					podCommand...)
				podSecondaryWorkerNode = newAgnhostPodOnNode(
					podSecondaryWorkerNodeName,
					secondaryWorkerNode.Name,
					podLabels,
					podCommand...)
				_ = e2epod.NewPodClient(f).CreateSync(context.TODO(), podWorkerNode)
				_ = e2epod.NewPodClient(f).CreateSync(context.TODO(), podSecondaryWorkerNode)

				By("Waiting until both pods have an IP address")
				Eventually(func() error {
					var err error
					podWorkerNode, err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Get(context.TODO(), podWorkerNode.Name, metav1.GetOptions{})
					if err != nil {
						return err
					}
					if podWorkerNode.Status.PodIP == "" {
						return fmt.Errorf("pod %s has no valid IP address yet", podWorkerNode.Name)
					}
					podSecondaryWorkerNode, err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Get(context.TODO(), podSecondaryWorkerNode.Name, metav1.GetOptions{})
					if err != nil {
						return err
					}
					if podSecondaryWorkerNode.Status.PodIP == "" {
						return fmt.Errorf("pod %s has no valid IP address yet", podSecondaryWorkerNode.Name)
					}
					return nil
				}, pollingTimeout, pollingInterval).Should(Succeed())

				By(fmt.Sprintf("Poking from pod %s on node %s to pod %s on node %s for a maximum of %d seconds",
					podWorkerNode.Name, podWorkerNode.Spec.NodeName,
					podSecondaryWorkerNode.Name, podSecondaryWorkerNode.Spec.NodeName,
					pollingTimeout))
				Eventually(func() bool {
					By("Poking the pod")
					err := pokeAllPodIPs(f, podWorkerNodeName, podSecondaryWorkerNode)
					if err != nil {
						framework.Logf("Poking all pod IPs failed: %q", err)
					}
					return err == nil
				}, pollingTimeout, pollingInterval).Should(BeTrue())

				By(fmt.Sprintf("Finding worker node %s's IPv%d migration IP address", workerNode.Name, ipAddrFamily))
				// Pick something at the end of the range to avoid conflicts with the kind / docker network setup.
				// Also exclude the current node IPs and the egressIP (if already selected).
				var err error
				migrationWorkerNodeIP, err = findLastFreeSubnetIP(
					externalContainerName,
					externalContainerIPs[ipAddrFamily],
					[]string{workerNodeIPs[ipAddrFamily], secondaryWorkerNodeIPs[ipAddrFamily], egressIP},
				)
				framework.Logf("New worker node IP will be %s", migrationWorkerNodeIP)
				Expect(err).NotTo(HaveOccurred())
			})

			JustAfterEach(func() {
				if rollbackNeeded {
					By(fmt.Sprintf("Migrating worker node %s back from IP address %s to IP address %s (inverted)",
						workerNode.Name, migrationWorkerNodeIP, workerNodeIPs[ipAddrFamily]))
					err := migrateWorkerNodeIP(workerNode.Name, migrationWorkerNodeIP, workerNodeIPs[ipAddrFamily],
						true)
					Expect(err).NotTo(HaveOccurred())

					ovnkubeNodePods, err := f.ClientSet.CoreV1().Pods(ovnNamespace).List(context.TODO(), metav1.ListOptions{
						LabelSelector: "app=ovnkube-node",
						FieldSelector: "spec.nodeName=" + workerNode.Name,
					})
					Expect(err).NotTo(HaveOccurred())
					Expect(ovnkubeNodePods.Items).To(HaveLen(1))
					ovnkubePodWorkerNode := ovnkubeNodePods.Items[0]

					Eventually(func() bool {
						By("waiting for the ovn-encap-ip to be reconfigured")
						return isOVNEncapIPReady(workerNode.Name, workerNodeIPs[ipAddrFamily], ovnkubePodWorkerNode.Name)
					}, pollingTimeout, pollingInterval).Should(BeTrue())

					err = e2epod.DeletePodWithWait(context.TODO(), f.ClientSet, &ovnkubePodWorkerNode)
					Expect(err).NotTo(HaveOccurred())

					By(fmt.Sprintf("Sleeping for %d seconds to give things time to settle", settleTimeout))
					time.Sleep(time.Duration(settleTimeout) * time.Second)

				}
			})

			Context("when no EgressIPs are configured", func() {
				for _, updateKubeletFirst := range []bool{true, false} {
					updateKubeletFirst := updateKubeletFirst // Required to avoid race conditions due to pointer assignment.
					It(fmt.Sprintf("makes sure that the cluster is still operational (%s)",
						updateKubeletIPAddressMsg[updateKubeletFirst]),
						func() {
							By(fmt.Sprintf("Migrating worker node %s from IP address %s to IP address %s",
								workerNode.Name, workerNodeIPs[ipAddrFamily], migrationWorkerNodeIP))
							err := migrateWorkerNodeIP(workerNode.Name, workerNodeIPs[ipAddrFamily], migrationWorkerNodeIP,
								true)
							Expect(err).NotTo(HaveOccurred())

							By("Setting rollbackNeeded to true")
							rollbackNeeded = true

							ovnkubeNodePods, err := f.ClientSet.CoreV1().Pods(ovnNamespace).List(context.TODO(), metav1.ListOptions{
								LabelSelector: "app=ovnkube-node",
								FieldSelector: "spec.nodeName=" + workerNode.Name,
							})
							Expect(err).NotTo(HaveOccurred())
							Expect(ovnkubeNodePods.Items).To(HaveLen(1))
							ovnkubePodWorkerNode := ovnkubeNodePods.Items[0]

							Eventually(func() bool {
								By("waiting for the ovn-encap-ip to be reconfigured")
								return isOVNEncapIPReady(workerNode.Name, migrationWorkerNodeIP, ovnkubePodWorkerNode.Name)
							}, pollingTimeout, pollingInterval).Should(BeTrue())

							By(fmt.Sprintf("Sleeping for %d seconds to give things time to settle", settleTimeout))
							time.Sleep(time.Duration(settleTimeout) * time.Second)

							By(fmt.Sprintf("Poking from pod %s on node %s to pod %s on node %s for a maximum of %d seconds",
								podWorkerNode.Name, podWorkerNode.Spec.NodeName,
								podSecondaryWorkerNode.Name, podSecondaryWorkerNode.Spec.NodeName,
								pollingTimeout))
							Eventually(func() bool {
								By("Poking the pod")
								err := pokeAllPodIPs(f, podWorkerNodeName, podSecondaryWorkerNode)
								if err != nil {
									framework.Logf("Poking all pod IPs failed: %q", err)
								}
								return err == nil
							}, pollingTimeout, pollingInterval).Should(BeTrue())
						})
				}
			})

			Context("when EgressIPs are configured", func() {
				BeforeEach(func() {
					By("Adding the \"k8s.ovn.org/egress-assignable\" label to the first node")
					e2enode.AddOrUpdateLabelOnNode(f.ClientSet, workerNode.Name, "k8s.ovn.org/egress-assignable", "")

					By("Creating an EgressIP object with one egress IP defined")
					// Assign the egress IP without conflicting with any node IP,
					// the kind subnet is /16 or /64 so the following should be fine.
					// pick something at the end of the range to avoid conflicts with the kind / docker network setup.
					// Exclude current node IPs and the migrationWorkerNodeIP (if already selected).
					var err error
					egressIP, err = findLastFreeSubnetIP(
						externalContainerName,
						externalContainerIPs[ipAddrFamily],
						[]string{workerNodeIPs[ipAddrFamily], secondaryWorkerNodeIPs[ipAddrFamily], migrationWorkerNodeIP},
					)
					Expect(err).NotTo(HaveOccurred())
					framework.Logf("EgressIP will be %s", egressIP)

					var egressIPConfig = fmt.Sprintf(egressIPYamlTemplate, podLabels["app"], egressIP, "app",
						podLabels["app"], f.Namespace.Name)
					if err := os.WriteFile(
						tmpDirIPMigration+"/"+egressIPYaml, []byte(egressIPConfig), 0644); err != nil {
						framework.Failf("Unable to write CRD config to disk: %v", err)
					}
					_, err = e2ekubectl.RunKubectl("default", "apply", "-f", tmpDirIPMigration+"/"+egressIPYaml)
					Expect(err).NotTo(HaveOccurred())

					By("Verifying egress IP works")
					expectedAnswer := "^" + egressIP + ":.*"
					if utilnet.IsIPv6String(egressIP) {
						expectedAnswer = "^\\[" + egressIP + "\\]:.*"
					}
					Eventually(func() bool {
						By("Checking the egress IP")
						res, err := targetExternalContainerConnectToEndpoint(externalContainerName,
							externalContainerIPs[ipAddrFamily], externalContainerPort, externalContainerEndpoint,
							podWorkerNode.Name, f.Namespace.Name, expectedAnswer)
						if err != nil {
							framework.Logf("Current verification failed with %s", err)
							return false
						}
						return res
					}, pollingTimeout, pollingInterval).Should(BeTrue())
				})

				JustAfterEach(func() {
					By("Deleting the gressip service")
					e2ekubectl.RunKubectl("default", "delete", "eip", podLabels["app"])

					By(fmt.Sprintf("Removing the egress assignable label from node %s", workerNode.Name))
					e2ekubectl.RunKubectl("default", "label", "node", workerNode.Name, "k8s.ovn.org/egress-assignable-")
				})

				for _, updateKubeletFirst := range []bool{true, false} {
					updateKubeletFirst := updateKubeletFirst // Required to avoid race conditions due to pointer assignment.
					It(fmt.Sprintf("makes sure that the EgressIP is still operational (%s)",
						updateKubeletIPAddressMsg[updateKubeletFirst]),
						func() {
							By(fmt.Sprintf("Migrating worker node %s from IP address %s to IP address %s",
								workerNode.Name, workerNodeIPs[ipAddrFamily], migrationWorkerNodeIP))
							err := migrateWorkerNodeIP(workerNode.Name, workerNodeIPs[ipAddrFamily], migrationWorkerNodeIP,
								updateKubeletFirst)
							Expect(err).NotTo(HaveOccurred())

							By("Setting rollbackNeeded to true")
							rollbackNeeded = true

							ovnkubeNodePods, err := f.ClientSet.CoreV1().Pods(ovnNamespace).List(context.TODO(), metav1.ListOptions{
								LabelSelector: "app=ovnkube-node",
								FieldSelector: "spec.nodeName=" + workerNode.Name,
							})
							Expect(err).NotTo(HaveOccurred())
							Expect(ovnkubeNodePods.Items).To(HaveLen(1))
							ovnkubePodWorkerNode := ovnkubeNodePods.Items[0]

							Eventually(func() bool {
								By("waiting for the ovn-encap-ip to be reconfigured")
								return isOVNEncapIPReady(workerNode.Name, migrationWorkerNodeIP, ovnkubePodWorkerNode.Name)
							}, pollingTimeout, pollingInterval).Should(BeTrue())

							By(fmt.Sprintf("Sleeping for %d seconds to give things time to settle", settleTimeout))
							time.Sleep(time.Duration(settleTimeout) * time.Second)

							By("Verifying Egress IP still works")
							expectedAnswer := "^" + egressIP + ":.*"
							if utilnet.IsIPv6String(egressIP) {
								expectedAnswer = "^\\[" + egressIP + "\\]:.*"
							}
							Eventually(func() bool {
								By("Checking the egress IP")
								res, err := targetExternalContainerConnectToEndpoint(externalContainerName,
									externalContainerIPs[ipAddrFamily], externalContainerPort, externalContainerEndpoint,
									podWorkerNode.Name, f.Namespace.Name, expectedAnswer)
								if err != nil {
									framework.Logf("Current verification failed with %s", err)
									return false
								}
								return res
							}, pollingTimeout, pollingInterval).Should(BeTrue())

							By(fmt.Sprintf("Poking from pod %s on node %s to pod %s on node %s for a maximum of %d seconds",
								podWorkerNode.Name, podWorkerNode.Spec.NodeName,
								podSecondaryWorkerNode.Name, podSecondaryWorkerNode.Spec.NodeName,
								pollingTimeout))
							Eventually(func() bool {
								By("Poking the pod")
								err := pokeAllPodIPs(f, podWorkerNodeName, podSecondaryWorkerNode)
								if err != nil {
									framework.Logf("Poking all pod IPs failed: %q", err)
								}
								return err == nil
							}, pollingTimeout, pollingInterval).Should(BeTrue())
						})
				}
			})

			Context("when ETP=Local service with host network backend is configured", func() {
				BeforeEach(func() {
					By("creating a host-network backend pod")
					jig := e2eservice.NewTestJig(f.ClientSet, f.Namespace.Name, serviceName)
					serverPod := e2epod.NewAgnhostPod(f.Namespace.Name, podName, nil, nil, []v1.ContainerPort{{ContainerPort: udpPort}, {ContainerPort: udpPort, Protocol: "UDP"}},
						"netexec", "--udp-port="+udpPortS)
					serverPod.Labels = jig.Labels
					serverPod.Spec.HostNetwork = true
					serverPod.Spec.NodeName = workerNode.Name
					serverPod = e2epod.NewPodClient(f).CreateSync(context.TODO(), serverPod)

					By("Creating ETP local service")
					svc, err := jig.CreateUDPService(context.TODO(), func(s *v1.Service) {
						s.Spec.Ports = []v1.ServicePort{
							{
								Name:       serviceName,
								Protocol:   v1.ProtocolUDP,
								Port:       80,
								TargetPort: intstr.FromInt(int(udpPort)),
							},
						}
						s.Spec.ExternalTrafficPolicy = v1.ServiceExternalTrafficPolicyLocal
						s.Spec.Type = v1.ServiceTypeNodePort
					})
					framework.ExpectNoError(err)

					By("Checking flows have correct IP address")
					assignedNodePort = svc.Spec.Ports[0].NodePort

					// find the ovn-kube node pod on this node
					pods, err := f.ClientSet.CoreV1().Pods(ovnNamespace).List(context.TODO(), metav1.ListOptions{
						LabelSelector: "app=ovnkube-node",
						FieldSelector: "spec.nodeName=" + workerNode.Name,
					})
					framework.ExpectNoError(err)
					Expect(pods.Items).To(HaveLen(1))
					ovnkPod = pods.Items[0]

					cmd := "ovs-ofctl dump-flows breth0 table=0"
					err = wait.PollImmediate(framework.Poll, 30*time.Second, func() (bool, error) {
						stdout, err := e2epodoutput.RunHostCmdWithRetries(ovnkPod.Namespace, ovnkPod.Name, cmd, framework.Poll, 30*time.Second)
						if err != nil {
							return false, err
						}
						lines := strings.Split(stdout, "\n")
						for _, line := range lines {
							if strings.Contains(line, "priority=110,udp") &&
								strings.Contains(line, fmt.Sprintf("tp_dst=%d", assignedNodePort)) &&
								strings.Contains(line, workerNodeIPs[ipAddrFamily]) {
								framework.Logf("Matching OpenFlow found: %s", line)
								return true, nil
							}
						}
						return false, nil
					})
					framework.ExpectNoError(err)
				})

				JustAfterEach(func() {
					By("Deleting the service")
					e2ekubectl.RunKubectl(f.Namespace.Name, "delete", "service", serviceName)

					By("Deleting host network backend pod")
					e2ekubectl.RunKubectl(f.Namespace.Name, "delete", "pod", podName)
				})

				for _, updateKubeletFirst := range []bool{true, false} {
					updateKubeletFirst := updateKubeletFirst // Required to avoid race conditions due to pointer assignment.
					It(fmt.Sprintf("makes sure that the flows are updated with new IP address (%s)",
						updateKubeletIPAddressMsg[updateKubeletFirst]),
						func() {
							By(fmt.Sprintf("Migrating worker node %s from IP address %s to IP address %s",
								workerNode.Name, workerNodeIPs[ipAddrFamily], migrationWorkerNodeIP))
							err := migrateWorkerNodeIP(workerNode.Name, workerNodeIPs[ipAddrFamily], migrationWorkerNodeIP,
								updateKubeletFirst)
							Expect(err).NotTo(HaveOccurred())

							By("Setting rollbackNeeded to true")
							rollbackNeeded = true

							ovnkubeNodePods, err := f.ClientSet.CoreV1().Pods(ovnNamespace).List(context.TODO(), metav1.ListOptions{
								LabelSelector: "app=ovnkube-node",
								FieldSelector: "spec.nodeName=" + workerNode.Name,
							})
							Expect(err).NotTo(HaveOccurred())
							Expect(ovnkubeNodePods.Items).To(HaveLen(1))
							ovnkubePodWorkerNode := ovnkubeNodePods.Items[0]

							Eventually(func() bool {
								By("waiting for the ovn-encap-ip to be reconfigured")
								return isOVNEncapIPReady(workerNode.Name, migrationWorkerNodeIP, ovnkubePodWorkerNode.Name)
							}, pollingTimeout, pollingInterval).Should(BeTrue())

							By(fmt.Sprintf("Sleeping for %d seconds to give things time to settle", settleTimeout))
							time.Sleep(time.Duration(settleTimeout) * time.Second)

							By(fmt.Sprintf("Checking nodeport flows have been updated to use new IP: %s", migrationWorkerNodeIP))
							cmd := "ovs-ofctl dump-flows breth0 table=0"
							err = wait.PollImmediate(framework.Poll, 30*time.Second, func() (bool, error) {
								stdout, err := e2epodoutput.RunHostCmdWithRetries(ovnkPod.Namespace, ovnkPod.Name, cmd, framework.Poll, 30*time.Second)
								if err != nil {
									return false, err
								}
								lines := strings.Split(stdout, "\n")
								for _, line := range lines {
									if strings.Contains(line, "priority=110,udp") &&
										strings.Contains(line, fmt.Sprintf("tp_dst=%d", assignedNodePort)) &&
										strings.Contains(line, migrationWorkerNodeIP) {
										framework.Logf("Matching OpenFlow found: %s", line)
										return true, nil
									}
								}
								// Due to potential k8s bug described here: https://github.com/ovn-org/ovn-kubernetes/issues/4073
								// We may need to restart kubelet for the backend pod to update its host networked IP address
								restartCmd := []string{"docker", "exec", workerNode.Name, "systemctl", "restart", "kubelet"}
								_, restartErr := runCommand(restartCmd...)
								framework.ExpectNoError(restartErr)
								return false, nil
							})
							framework.ExpectNoError(err)
						})
				}
			})
		})
	}

	When("when MAC address changes", func() {
		BeforeEach(func() {
			By("Storing original MAC")
			ovnkubeNodePods, err := f.ClientSet.CoreV1().Pods(ovnNamespace).List(context.TODO(), metav1.ListOptions{
				LabelSelector: "app=ovnkube-node",
				FieldSelector: "spec.nodeName=" + workerNode.Name,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(ovnkubeNodePods.Items).To(HaveLen(1))
			ovnkPod = ovnkubeNodePods.Items[0]
			workerNodeMAC, err = getMACAddress(ovnkPod)
			framework.Logf("Original MAC found to be: %s", workerNodeMAC)
			Expect(err).NotTo(HaveOccurred())

			By("Checking flows have current original MAC address")
			Expect(checkFlowsForMAC(ovnkPod, workerNodeMAC)).NotTo(HaveOccurred())
		})

		JustAfterEach(func() {
			if len(workerNodeMAC) > 0 {
				By("Reverting to original MAC address")
				setMACAddress(ovnkPod, workerNodeMAC.String())
			}
		})

		Context("when a nodeport service is configured", func() {
			BeforeEach(func() {
				By("Creating service")
				jig := e2eservice.NewTestJig(f.ClientSet, f.Namespace.Name, serviceName)
				_, err := jig.CreateUDPService(context.TODO(), func(s *v1.Service) {
					s.Spec.Ports = []v1.ServicePort{
						{
							Name:       serviceName,
							Protocol:   v1.ProtocolUDP,
							Port:       80,
							TargetPort: intstr.FromInt(int(udpPort)),
						},
					}
					s.Spec.Type = v1.ServiceTypeNodePort
				})
				framework.ExpectNoError(err)

			})

			JustAfterEach(func() {
				By("Deleting the service")
				e2ekubectl.RunKubectl(f.Namespace.Name, "delete", "service", serviceName)
			})

			It(fmt.Sprintf("Ensures flows are updated when MAC address changes"), func() {
				By(fmt.Sprintf("Updating the mac address to a new value: %s", migratedMAC))
				framework.ExpectNoError(setMACAddress(ovnkPod, migratedMAC))
				By("Checking flows are updated with the correct MAC address")
				mac, err := net.ParseMAC(migratedMAC)
				framework.ExpectNoError(err)
				Expect(checkFlowsForMACPeriodically(ovnkPod, mac, 30*time.Second)).NotTo(HaveOccurred())
				By("Checking the L3 gateway annotation has been updated with the new MAC address")
				Eventually(func() string {
					node, err := f.ClientSet.CoreV1().Nodes().Get(context.TODO(), workerNode.Name, metav1.GetOptions{})
					if err != nil {
						return ""
					}
					return node.Annotations["k8s.ovn.org/l3-gateway-config"]
				}, 30*time.Second, 2*time.Second).Should(ContainSubstring(migratedMAC))
			})
		})
	})
})

func checkFlowsForMACPeriodically(ovnkPod v1.Pod, addr net.HardwareAddr, duration time.Duration) error {
	var endErr error
	if err := wait.PollImmediate(framework.Poll, duration, func() (bool, error) {
		if err := checkFlowsForMAC(ovnkPod, addr); err != nil {
			endErr = err
			return false, nil
		}
		return true, nil
	}); err != nil {
		return fmt.Errorf("failed checking for MAC periodically: %w, final error: %v", err, endErr)
	}
	return nil
}

func checkFlowsForMAC(ovnkPod v1.Pod, mac net.HardwareAddr) error {
	cmd := "ovs-ofctl dump-flows breth0"
	flowOutput := e2epodoutput.RunHostCmdOrDie(ovnkPod.Namespace, ovnkPod.Name, cmd)
	lines := strings.Split(flowOutput, "\n")
	for _, line := range lines {
		if !strings.Contains(line, "dl_dst") && !strings.Contains(line, "dl_src") {
			// flows we don't care about without MACs
			continue
		}

		// must be a flow with a MAC, if it isn't the right MAC address, fail
		if !strings.Contains(line, mac.String()) {
			return fmt.Errorf("flow found with incorrect MAC: %s, expected MAC: %s", line, mac)
		}
	}
	return nil
}

func setMACAddress(ovnkubePod v1.Pod, mac string) error {
	cmd := []string{"kubectl", "-n", ovnkubePod.Namespace, "exec", ovnkubePod.Name, "-c", "ovn-controller",
		"--", "ovs-vsctl", "set", "bridge", "breth0", fmt.Sprintf("other-config:hwaddr=%s", mac)}
	_, err := runCommand(cmd...)
	return err
}

func getMACAddress(ovnkubePod v1.Pod) (net.HardwareAddr, error) {
	cmd := []string{"kubectl", "-n", ovnkubePod.Namespace, "exec", ovnkubePod.Name, "-c", "ovn-controller",
		"--", "ip", "link", "show", "breth0"}
	output, err := runCommand(cmd...)
	if err != nil {
		return nil, fmt.Errorf("failed to get ip link output: %w", err)
	}

	re := regexp.MustCompile("([0-9A-Fa-f]{2}[:-]){5}([0-9A-Fa-f]{2})|([0-9a-fA-F]{4}\\.[0-9a-fA-F]{4}\\.[0-9a-fA-F]{4})")
	return net.ParseMAC(re.FindString(output))
}

// getNodeInternalAddresses returns the first IPv4 and/or IPv6 InternalIP defined for the node. Node IPs are ordered,
// meaning that the returned IPs should be kubelet's node IPs.
//
//	Copied from: https://github.com/ovn-org/ovn-kubernetes/blob/\
//	                              2cceeebd4f66ee8dd9e683551b883e549b5cd7da/go-controller/pkg/ovn/egressip.go#L2580
func getNodeInternalAddresses(node *v1.Node) (string, string) {
	var v4Addr, v6Addr string
	for _, nodeAddr := range node.Status.Addresses {
		if nodeAddr.Type == v1.NodeInternalIP {
			ip := nodeAddr.Address
			if !utilnet.IsIPv6String(ip) && v4Addr == "" {
				v4Addr = ip
			} else if utilnet.IsIPv6String(ip) && v6Addr == "" {
				v6Addr = ip
			}
		}
	}
	return v4Addr, v6Addr
}

// findIpAddressMaskOnHost finds the string "<IP address>/<mask>" and interface name on container <containerName> for
// nodeIP.
func findIPAddressMaskInterfaceOnHost(containerName, containerIP string) (net.IPNet, string, error) {
	ipAddressCmdOutput, err := runCommand("docker", "exec", containerName, "ip", "-o", "address")
	if err != nil {
		return net.IPNet{}, "", err
	}
	re, err := regexp.Compile(fmt.Sprintf("%s/[0-9]{1,3}", containerIP))
	if err != nil {
		return net.IPNet{}, "", err
	}
	ipAddressMask := ""
	iface := ""
	scanner := bufio.NewScanner(strings.NewReader(ipAddressCmdOutput))
	for scanner.Scan() {
		line := scanner.Text()
		ipAddressMask = re.FindString(line)
		if ipAddressMask != "" {
			if exploded := strings.Fields(line); len(exploded) > 1 {
				iface = exploded[1]
			}
			break
		}
	}
	if ipAddressMask == "" {
		return net.IPNet{}, "", fmt.Errorf("IP address and mask were not found via `ip address` for node %s with IP %s",
			containerName,
			containerIP)
	}
	if iface == "" {
		return net.IPNet{}, "", fmt.Errorf("interface not found for node %s with IP %s",
			containerName, containerIP)
	}
	parsedNetIP, parsedNetCIDR, err := net.ParseCIDR(ipAddressMask)
	if err != nil {
		return net.IPNet{}, "", err
	}
	return net.IPNet{IP: parsedNetIP, Mask: parsedNetCIDR.Mask}, iface, nil
}

// nextIP returns IP incremented by 1. If the incremented IP does not fit in the IPNet, it returns an error.
func nextIP(ipNet net.IPNet) (net.IPNet, error) {
	i := ipToInt(ipNet.IP)
	newIP := intToIP(i.Add(i, big.NewInt(1)))
	if !ipNet.Contains(newIP) {
		return ipNet, fmt.Errorf("could not find a suitable IP address for the container")
	}
	nextIPNet := net.IPNet{IP: newIP, Mask: ipNet.Mask}
	return nextIPNet, nil
}

// priorIP returns IP minus 1. If the decreased IP does not fit in the IPNet, it returns an error.
func priorIP(ipNet net.IPNet) (net.IPNet, error) {
	i := ipToInt(ipNet.IP)
	newIP := intToIP(i.Sub(i, big.NewInt(1)))
	if !ipNet.Contains(newIP) {
		return ipNet, fmt.Errorf("could not find a suitable IP address for the container")
	}
	nextIPNet := net.IPNet{IP: newIP, Mask: ipNet.Mask}
	return nextIPNet, nil
}

// ipToInt converts a net.ipToInt to a big.Int. Helper for priorIP / nextIP.
func ipToInt(ip net.IP) *big.Int {
	if v := ip.To4(); v != nil {
		return big.NewInt(0).SetBytes(v)
	}
	return big.NewInt(0).SetBytes(ip.To16())
}

// intToIP convertes a big.Int to an IP address. Helper for priorIP / nextIP.
func intToIP(i *big.Int) net.IP {
	return net.IP(i.Bytes())
}

// findLastFreeSubnetIP will find the last available IP on the container's subnet. It'll try the last 20 IPs in the
// subnet, that should be good enough.
func findLastFreeSubnetIP(containerName, containerIP string, excludedIPs []string) (string, error) {
	parsedNetIPMask, _, err := findIPAddressMaskInterfaceOnHost(containerName, containerIP)
	if err != nil {
		return "", err
	}

	// We are using 172.18.0.0/16 for the docker subnet. We should be able to find something that we need here and
	// that's not assigned.
	broadcastIP := subnetBroadcastIP(net.IPNet{IP: parsedNetIPMask.IP, Mask: parsedNetIPMask.Mask})
	decrementedIPNet := net.IPNet{IP: broadcastIP, Mask: parsedNetIPMask.Mask}

	var isReachable bool
outer:
	for i := 0; i < 20; i++ {
		decrementedIPNet, err = priorIP(decrementedIPNet)
		if err != nil {
			return "", err
		}
		for _, excludedIP := range excludedIPs {
			if excludedIP == decrementedIPNet.IP.String() {
				continue outer
			}
		}
		isReachable, err = isAddressReachableFromContainer(containerName, decrementedIPNet.IP.String())
		if err != nil {
			return "", err
		}
		if !isReachable {
			return decrementedIPNet.IP.String(), nil
		}
	}
	return "", fmt.Errorf("unexpected error while trying to find the last free subnet IP %s", containerIP)
}

// subnetBroadcastIP returns the IP network's broadcast IP.
func subnetBroadcastIP(ipnet net.IPNet) net.IP {
	// For IPv4.
	if ipnet.IP.To4() != nil {
		// ip address in uint32
		ipBits := binary.BigEndian.Uint32(ipnet.IP.To4())
		// mask will give us all fixed bits of the subnet
		maskBits := binary.BigEndian.Uint32(ipnet.Mask)
		// inverted mask will give us all moving bits of the subnet
		invertedMaskBits := maskBits ^ 0xffffffff // xor the mask
		// network ip
		networkIPBits := ipBits & maskBits
		// broadcastIP = networkIP added to the inverted mask
		broadcastIPBits := networkIPBits | invertedMaskBits
		broadcastIP := make(net.IP, 4)
		binary.BigEndian.PutUint32(broadcastIP, broadcastIPBits)
		return broadcastIP
	}
	// For IPv6. This conversion is actually easier, it follows the same principle as above.
	byteIP := []byte(ipnet.IP)                // []byte representation of IP
	byteMask := []byte(ipnet.Mask)            // []byte representation of mask
	byteTargetIP := make([]byte, len(byteIP)) // []byte holding target IP
	for k := range byteIP {
		invertedMask := byteMask[k] ^ 0xff // inverted mask byte
		byteTargetIP[k] = byteIP[k]&byteMask[k] | invertedMask
	}

	return net.IP(byteTargetIP)
}

// isAddressReachableFromContainer will curl towards targetIP. If the curl succeeds, return true. Otherwise, check the
// node's neighbor table. If a neighbor entry for targetIP exists, return true, false otherwise. We use curl because
// it's installed by default in the ubuntu kind containers; ping/arping are unfortunately not available.
func isAddressReachableFromContainer(containerName, targetIP string) (bool, error) {
	// There's no ping/arping inside the default containers, so just use curl instead. It's good enough to trigger
	// ARP resolution.
	cmd := []string{"docker", "exec", containerName}
	if utilnet.IsIPv6String(targetIP) {
		targetIP = fmt.Sprintf("[%s]", targetIP)
	}
	curlCommand := strings.Split(fmt.Sprintf("curl -g -q -s http://%s:%d", targetIP, 80), " ")
	cmd = append(cmd, curlCommand...)
	_, err := runCommand(cmd...)
	// If this curl works, then the node is logically reachable, shortcut.
	if err == nil {
		return true, nil
	}

	// Now, check the neighbor table and if the entry does not have REACHABLE or STALE or PERMANENT, then this must be
	// an unreachable entry (could be FAILED or INCOMPLETE).
	ipNeighborOutput, err := runCommand("docker", "exec", containerName, "ip", "neigh")
	if err != nil {
		return false, err
	}
	re, err := regexp.Compile(fmt.Sprintf("^%s ", targetIP))
	if err != nil {
		return false, err
	}
	scanner := bufio.NewScanner(strings.NewReader(ipNeighborOutput))
	for scanner.Scan() {
		line := scanner.Text()
		if re.MatchString(line) {
			if strings.Contains(line, "REACHABLE") ||
				strings.Contains(line, "STALE") ||
				strings.Contains(line, "PERMANENT") {
				return true, nil
			}
			return false, nil
		}
	}
	return false, nil
}

func isOVNEncapIPReady(nodeName, nodeIP, ovnkubePodName string) bool {
	framework.Logf("Verifying ovn-encap-ip for node %s", nodeName)
	cmd := []string{"kubectl", "-n", ovnNamespace, "exec", ovnkubePodName, "-c", "ovn-controller",
		"--", "ovs-vsctl", "get", "open_vswitch", ".", "external-ids:ovn-encap-ip"}
	output, err := runCommand(cmd...)
	if err != nil {
		framework.Logf("Failed to get ovn-encap-ip: %q", err)
		return false
	}

	output = strings.Replace(output, "\"", "", -1)
	output = strings.Replace(output, "\n", "", -1)

	if output != nodeIP {
		framework.Logf("current ovn-encap-ip %q doesn't match %q", output, nodeIP)
		return false
	}
	return true
}

// migrateWorkerNodeIP migrates node <nodeName> from <fromIP> to <targetIP>. Change IP address first, then update
// kubelet configuration. If invertOrder is true, then update kubelet configuration before changing the IP address.
// Warning: This isn't atomic. We might end up with a broken state in between if any of the commands fail even though
// we try to clean up things if something goes wrong.
func migrateWorkerNodeIP(nodeName, fromIP, targetIP string, invertOrder bool) (err error) {
	// Run a cleanup sequence if something goes wrong.
	var cleanupCommands [][]string
	defer func() {
		if err != nil {
			for _, cmd := range cleanupCommands {
				framework.Logf("Attempting cleanup with command %q", cmd)
				runCommand(cmd...)
			}
		}
	}()

	framework.Logf("Finding fromIP %s on host %s", fromIP, nodeName)
	parsedNetIPMask, iface, err := findIPAddressMaskInterfaceOnHost(nodeName, fromIP)
	if err != nil {
		return err
	}
	exploded := strings.Split(parsedNetIPMask.String(), "/")
	if len(exploded) < 2 {
		return fmt.Errorf("not a valid CIDR: %v", parsedNetIPMask)
	}
	mask := exploded[1]

	// Define a function to change the IP address for later use.
	changeIPAddress := func() error {
		// Add new IP first - this will preserve the default route.
		newIPMask := targetIP + "/" + mask
		framework.Logf("Adding new IP address %s to node %s", newIPMask, nodeName)
		// Add cleanup command.
		cleanupCmd := []string{"docker", "exec", nodeName, "ip", "address", "del", newIPMask, "dev", iface}
		cleanupCommands = append(cleanupCommands, cleanupCmd)
		// Run command.
		cmd := []string{"docker", "exec", nodeName, "ip", "address", "add", newIPMask, "dev", iface}
		_, err = runCommand(cmd...)
		if err != nil {
			return err
		}
		// Delete current IP address. On rollback, first add the old IP and then delete the new one.
		framework.Logf("Deleting current IP address %s from node %s", parsedNetIPMask.String(), nodeName)
		// Add cleanup command.
		cleanupCmd = []string{"docker", "exec", nodeName, "ip", "address", "add", parsedNetIPMask.String(), "dev", iface}
		cleanupCommands = append([][]string{cleanupCmd}, cleanupCommands...)
		// Run command.
		cmd = []string{"docker", "exec", nodeName, "ip", "address", "del", parsedNetIPMask.String(), "dev", iface}
		_, err = runCommand(cmd...)
		if err != nil {
			return err
		}
		return nil
	}
	// Define a function to update the IP address in kubelet configuration and to restart kubelet for later use.
	changeKubeletIP := func() error {
		// Change kubeadm-flags.env IP.
		framework.Logf("Modifying kubelet configuration for node %s", nodeName)
		// Add cleanup commands.
		cleanupCmd := []string{"docker", "exec", nodeName, "sed", "-i", fmt.Sprintf("s/node-ip=%s/node-ip=%s/", targetIP, fromIP),
			"/var/lib/kubelet/kubeadm-flags.env"}
		cleanupCommands = append(cleanupCommands, cleanupCmd)
		cleanupCmd = []string{"docker", "exec", nodeName, "systemctl", "restart", "kubelet"}
		cleanupCommands = append(cleanupCommands, cleanupCmd)
		// Run command.
		cmd := []string{"docker", "exec", nodeName, "sed", "-i", fmt.Sprintf("s/node-ip=%s/node-ip=%s/", fromIP, targetIP),
			"/var/lib/kubelet/kubeadm-flags.env"}
		_, err = runCommand(cmd...)
		if err != nil {
			return err
		}

		// Restart kubelet.
		framework.Logf("Restarting kubelet on node %s", nodeName)
		cmd = []string{"docker", "exec", nodeName, "systemctl", "restart", "kubelet"}
		_, err = runCommand(cmd...)
		if err != nil {
			return err
		}
		return nil
	}

	fs := []func() error{changeIPAddress, changeKubeletIP}
	if invertOrder {
		fs = []func() error{changeKubeletIP, changeIPAddress}
	}
	for _, f := range fs {
		err = f()
		if err != nil {
			return err
		}
	}
	return nil
}

// targetExternalContainerConnectToEndpoint targets the external test container from the specified pod and compares
// expectedAnswer to the actual answer.
func targetExternalContainerConnectToEndpoint(externalContainerName, externalContainerIP, externalContainerPort,
	externalContainerEndpoint, podName, podNamespace string, expectedAnswer string) (bool, error) {
	containerIPAndPort := net.JoinHostPort(externalContainerIP, externalContainerPort)
	u := path.Join(containerIPAndPort, externalContainerEndpoint)
	output, err := e2ekubectl.RunKubectl(podNamespace, "exec", podName, "--", "curl", "--max-time", "2", u)
	if err != nil {
		return false, fmt.Errorf("running curl failed, err: %q", err)
	}

	isMatch, err := regexp.MatchString(expectedAnswer, output)
	if err != nil {
		return false, fmt.Errorf("cannot evaluate provided regular expression, err: %q", err)
	}

	if !isMatch {
		framework.Logf("No match found. Expected answer: '%s'. Got output: '%s'", expectedAnswer, output)
	}

	return isMatch, nil
}
