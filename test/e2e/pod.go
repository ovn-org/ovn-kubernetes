package e2e

import (
	"context"
	"fmt"
	"math/rand"
	"regexp"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2epodoutput "k8s.io/kubernetes/test/e2e/framework/pod/output"
)

var _ = ginkgo.Describe("Pod to external server PMTUD", func() {
	const (
		echoServerPodNameTemplate = "echo-server-pod-%d"
		echoClientPodName         = "echo-client-pod"
		echoServerPodPortMin      = 9800
		echoServerPodPortMax      = 9899
		primaryNetworkName        = "kind"
	)

	f := wrappedTestFramework("pod2external-pmtud")
	cleanupFn := func() {}

	ginkgo.AfterEach(func() {
		cleanupFn()
	})

	// The below series of tests queries a server running as a hostNetwork:true pod on nodeB from a client pod running as hostNetwork:false on nodeA
	// This traffic scenario mimics a pod2external setup where large packets and needs frag is involved.
	// for both HTTP and UDP and different ingress and egress payload sizes.
	// Steps:
	// * Set up a hostNetwork:false client pod (agnhost echo server) on nodeA
	// * Set up a external docker container as a server
	// * Query from client pod to server pod
	// Traffic Flow:
	// Req: podA on nodeA -> nodeA switch -> nodeA cluster-route -> nodeA transit switch -> nodeA join switch -> nodeA GR -> nodeA ext switch -> nodeA br-ex -> underlay
	// underlay -> server
	// Res: server sends large packet -> br-ex on nodeA -> nodeA ext-switch -> rtoe-GR port sends back needs frag thanks to gateway_mtu option
	// ICMP needs frag goes back to external server
	// server now fragments packets correctly.
	// NOTE: on LGW, the pkt exits via mp0 on nodeA and path is different than what is described above
	// Frag needed is sent by nodeA using ovn-k8s-mp0 interface mtu and not OVN's GR for flows where services are not involved in LGW
	ginkgo.When("a client ovnk pod targeting an external server is created", func() {
		var serverPodPort int
		var serverPodName string
		var serverNodeInternalIPs []string

		var clientPod *v1.Pod
		var clientPodNodeName string

		var echoPayloads = map[string]string{
			"small": fmt.Sprintf("%010d", 1),
			"large": fmt.Sprintf("%01420d", 1),
		}
		var echoMtuRegex = regexp.MustCompile(`cache expires.*mtu.*`)
		ginkgo.BeforeEach(func() {
			ginkgo.By("Selecting 3 schedulable nodes")
			nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), f.ClientSet, 3)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(len(nodes.Items)).To(gomega.BeNumerically(">", 2))

			ginkgo.By("Selecting nodes for client pod and server host-networked pod")
			clientPodNodeName = nodes.Items[1].Name

			ginkgo.By("Creating hostNetwork:false (ovnk) client pod")
			clientPod = e2epod.NewAgnhostPod(f.Namespace.Name, echoClientPodName, nil, nil, nil)
			clientPod.Spec.NodeName = clientPodNodeName
			for k := range clientPod.Spec.Containers {
				if clientPod.Spec.Containers[k].Name == "agnhost-container" {
					clientPod.Spec.Containers[k].Command = []string{
						"sleep",
						"infinity",
					}
				}
			}
			e2epod.NewPodClient(f).CreateSync(context.TODO(), clientPod)

			ginkgo.By("Creating the external server")
			serverPodPort = rand.Intn(echoServerPodPortMax-echoServerPodPortMin) + echoServerPodPortMin
			serverPodName = fmt.Sprintf(echoServerPodNameTemplate, serverPodPort)
			framework.Logf("Creating server pod listening on TCP and UDP port %d", serverPodPort)
			agntHostCmds := []string{"netexec", "--http-port", fmt.Sprintf("%d", serverPodPort), "--udp-port", fmt.Sprintf("%d", serverPodPort)}
			externalIpv4, externalIpv6 := createClusterExternalContainer(serverPodName, agnhostImage,
				[]string{"--network", "kind", "-P", "--cap-add", "NET_ADMIN"},
				agntHostCmds,
			)

			if isIPv4Supported() {
				serverNodeInternalIPs = append(serverNodeInternalIPs, externalIpv4)
			}

			if isIPv6Supported() {
				serverNodeInternalIPs = append(serverNodeInternalIPs, externalIpv6)
			}

			gomega.Expect(len(serverNodeInternalIPs)).To(gomega.BeNumerically(">", 0))
		})

		ginkgo.AfterEach(func() {
			ginkgo.By("Removing external container")
			if len(serverPodName) > 0 {
				deleteClusterExternalContainer(serverPodName)
			}
		})

		// Run queries against the server both with a small (10 bytes + overhead for echo service) and
		// a large (1420 bytes + overhead for echo service) payload.
		// The payload is transmitted to and echoed from the echo service for both HTTP and UDP tests.
		ginkgo.When("tests are run towards the agnhost echo server", func() {
			ginkgo.It("queries to the hostNetworked server pod on another node shall work for TCP", func() {
				for _, size := range []string{"small", "large"} {
					for _, serverNodeIP := range serverNodeInternalIPs {
						ginkgo.By(fmt.Sprintf("Sending TCP %s payload to node IP %s "+
							"and expecting to receive the same payload", size, serverNodeIP))
						cmd := fmt.Sprintf("curl --max-time 10 -g -q -s http://%s:%d/echo?msg=%s",
							serverNodeIP,
							serverPodPort,
							echoPayloads[size],
						)
						framework.Logf("Testing TCP %s with command %q", size, cmd)
						stdout, err := e2epodoutput.RunHostCmdWithRetries(
							clientPod.Namespace,
							clientPod.Name,
							cmd,
							framework.Poll,
							60*time.Second)
						framework.ExpectNoError(err, fmt.Sprintf("Testing TCP with %s payload failed", size))
						gomega.Expect(stdout).To(gomega.Equal(echoPayloads[size]), fmt.Sprintf("Testing TCP with %s payload failed", size))
					}
				}
			})
			ginkgo.It("queries to the hostNetworked server pod on another node shall work for UDP", func() {
				clientNodeIPv4, clientNodeIPv6 := getContainerAddressesForNetwork(clientPodNodeName, primaryNetworkName) // we always want to fetch from primary network
				clientnodeIP := clientNodeIPv4
				if IsIPv6Cluster(f.ClientSet) {
					clientnodeIP = clientNodeIPv6
				}
				for _, size := range []string{"small", "large"} {
					for _, serverNodeIP := range serverNodeInternalIPs {
						if size == "large" {
							// Flushing the IP route cache will remove any routes in the cache
							// that are a result of receiving a "need to frag" packet.
							ginkgo.By("Flushing the ip route cache")
							stdout, err := runCommand(containerRuntime, "exec", "-i", serverPodName, "ip", "route", "flush", "cache")
							framework.ExpectNoError(err, "Flushing the ip route cache failed")
							framework.Logf("Flushed cache on %s", serverPodName)
							// List the current IP route cache for informative purposes.
							cmd := fmt.Sprintf("ip route get %s", clientnodeIP)
							stdout, err = runCommand(containerRuntime, "exec", "-i", serverPodName, "ip", "route", "get", clientnodeIP)
							framework.ExpectNoError(err, "Listing IP route cache")
							framework.Logf("%s: %s", cmd, stdout)
						}
						// We expect the following to fail at least once for large payloads and non-hostNetwork
						// endpoints: the first request will fail as we have to receive a "need to frag" ICMP
						// message, subsequent requests then should succeed.
						gomega.Eventually(func() error {
							ginkgo.By(fmt.Sprintf("Sending UDP %s payload to server IP %s "+
								"and expecting to receive the same payload", size, serverNodeIP))
							// Send payload via UDP.
							cmd := fmt.Sprintf("echo 'echo %s' | nc -w2 -u %s %d",
								echoPayloads[size],
								serverNodeIP,
								serverPodPort,
							)
							framework.Logf("Testing UDP %s with command %q", size, cmd)
							stdout, err := e2epodoutput.RunHostCmd(
								clientPod.Namespace,
								clientPod.Name,
								cmd)
							if err != nil {
								return err
							}
							// Compare received payload vs sent payload.
							if stdout != echoPayloads[size] {
								return fmt.Errorf("stdout does not match payloads[%s], %s != %s", size, stdout, echoPayloads[size])
							}
							if size == "large" {
								ginkgo.By("Making sure that the ip route cache contains an MTU route")
								// Get IP route cache and make sure that it contains an MTU route on the server side.
								stdout, err = runCommand(containerRuntime, "exec", "-i", serverPodName, "ip", "route", "get", clientnodeIP)
								if err != nil {
									return fmt.Errorf("could not list IP route cache using cmd: %s, err: %q", cmd, err)
								}
								framework.Logf("Route cache on server pod %s", stdout)
								if !echoMtuRegex.Match([]byte(stdout)) {
									return fmt.Errorf("cannot find MTU cache entry in route: %s", stdout)
								}
							}
							return nil
						}, 60*time.Second, 1*time.Second).Should(gomega.Succeed())
						// Flushing the IP route cache will remove any routes in the cache
						// that are a result of receiving a "need to frag" packet. Let's
						// flush this on all 3 nodes else we will run into the
						// bug: https://issues.redhat.com/browse/OCPBUGS-7609.
						// TODO: Revisit this once https://bugzilla.redhat.com/show_bug.cgi?id=2169839 is fixed.
						ovnKubeNodePods, err := f.ClientSet.CoreV1().Pods(ovnNamespace).List(context.TODO(), metav1.ListOptions{
							LabelSelector: "name=ovnkube-node",
						})
						if err != nil {
							framework.Failf("could not get ovnkube-node pods: %v", err)
						}
						for _, ovnKubeNodePod := range ovnKubeNodePods.Items {
							framework.Logf("Flushing the ip route cache on %s", ovnKubeNodePod.Name)
							containerName := "ovnkube-node"
							if isInterconnectEnabled() {
								containerName = "ovnkube-controller"
							}
							_, err := e2ekubectl.RunKubectl(ovnNamespace, "exec", ovnKubeNodePod.Name, "--container", containerName, "--",
								"ip", "route", "flush", "cache")
							framework.ExpectNoError(err, "Flushing the ip route cache failed")
						}
						framework.Logf("Flushing the ip route cache on %s", serverPodName)
						_, err = runCommand(containerRuntime, "exec", "-i", serverPodName, "ip", "route", "flush", "cache")
						framework.ExpectNoError(err, "Flushing the ip route cache failed")
					}
				}
			})
		})
	})
})

var _ = ginkgo.Describe("Pod to pod TCP with low MTU", func() {
	const (
		echoServerPodNameTemplate = "echo-server-pod-%d"
		echoClientPodName         = "echo-client-pod"
		serverPodPort             = 9899
		mtu                       = 1400
	)

	f := wrappedTestFramework("pod2pod-tcp-low-mtu")
	cleanupFn := func() {}

	ginkgo.AfterEach(func() {
		cleanupFn()
	})

	ginkgo.When("a client ovnk pod targeting an ovnk pod server(running on another node) with low mtu", func() {
		var serverPod *v1.Pod
		var serverPodNodeName string
		var serverPodName string
		var serverNode v1.Node
		var clientNode v1.Node
		var serverNodeInternalIPs []string
		var clientNodeInternalIPs []string

		var clientPod *v1.Pod
		var clientPodNodeName string

		payload := fmt.Sprintf("%01360d", 1)

		ginkgo.BeforeEach(func() {
			ginkgo.By("Selecting 2 schedulable nodes")
			nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), f.ClientSet, 2)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(len(nodes.Items)).To(gomega.BeNumerically(">", 1))

			ginkgo.By("Selecting nodes for client pod and server host-networked pod")
			serverPodNodeName = nodes.Items[0].Name
			serverNode = nodes.Items[0]
			clientPodNodeName = nodes.Items[1].Name
			clientNode = nodes.Items[1]

			ginkgo.By("Creating hostNetwork:false (ovnk) client pod")
			clientPod = e2epod.NewAgnhostPod(f.Namespace.Name, echoClientPodName, nil, nil, nil)
			clientPod.Spec.NodeName = clientPodNodeName
			for k := range clientPod.Spec.Containers {
				if clientPod.Spec.Containers[k].Name == "agnhost-container" {
					clientPod.Spec.Containers[k].Command = []string{
						"sleep",
						"infinity",
					}
				}
			}
			e2epod.NewPodClient(f).CreateSync(context.TODO(), clientPod)

			ginkgo.By("Creating hostNetwork:false (ovnk) server pod")
			serverPodName = fmt.Sprintf(echoServerPodNameTemplate, serverPodPort)
			serverPod = e2epod.NewAgnhostPod(f.Namespace.Name, serverPodName, nil, nil, nil,
				"netexec",
				"--http-port", fmt.Sprintf("%d", serverPodPort),
				"--udp-port", fmt.Sprintf("%d", serverPodPort),
			)
			serverPod.ObjectMeta.Labels = map[string]string{
				"app": serverPodName,
			}
			serverPod.Spec.NodeName = serverPodNodeName
			serverPod = e2epod.NewPodClient(f).CreateSync(context.TODO(), serverPod)

			ginkgo.By("Getting all InternalIP addresses of the server node")
			serverNodeInternalIPs = e2enode.GetAddresses(&serverNode, v1.NodeInternalIP)
			gomega.Expect(len(serverNodeInternalIPs)).To(gomega.BeNumerically(">", 0))

			ginkgo.By("Getting all InternalIP addresses of the client node")
			clientNodeInternalIPs = e2enode.GetAddresses(&clientNode, v1.NodeInternalIP)
			gomega.Expect(len(serverNodeInternalIPs)).To(gomega.BeNumerically(">", 0))

			ginkgo.By("Lowering the MTU route from server -> client")
			fmt.Println(clientNodeInternalIPs)
			err = addRouteToNode(serverPodNodeName, clientNodeInternalIPs, mtu)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			ginkgo.By("Lowering the MTU route from client -> server")
			fmt.Println(serverNodeInternalIPs)
			err = addRouteToNode(clientPodNodeName, serverNodeInternalIPs, mtu)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			cleanupFn = func() {
				err = delRouteToNode(serverPodNodeName, clientNodeInternalIPs)
				err = delRouteToNode(clientPodNodeName, serverNodeInternalIPs)
			}

		})

		// Lower the MTU between the two nodes to 1400, this will cause pod->pod 1400 byte packets
		// to be too big for geneve encapsulation
		ginkgo.When("MTU is lowered between the two nodes", func() {
			ginkgo.It("large queries to the server pod on another node shall work for TCP", func() {
				for _, serverPodIP := range serverPod.Status.PodIPs {
					ginkgo.By(fmt.Sprintf("Sending TCP large payload to server IP %s "+
						"and expecting to receive the same payload", serverPodIP))
					cmd := fmt.Sprintf("curl --max-time 10 -g -q -s http://%s:%d/echo?msg=%s",
						serverPodIP.IP,
						serverPodPort,
						payload,
					)
					framework.Logf("Testing large TCP segments with command %q", cmd)
					stdout, err := e2epodoutput.RunHostCmdWithRetries(
						clientPod.Namespace,
						clientPod.Name,
						cmd,
						framework.Poll,
						30*time.Second)
					framework.ExpectNoError(err, "Sending large TCP payload from client failed")
					gomega.Expect(stdout).To(gomega.Equal(payload),
						"Received TCP payload from server does not equal expected payload")

					cmd = fmt.Sprintf("ip route get %s", serverPodIP.IP)
					stdout, err = e2epodoutput.RunHostCmd(
						clientPod.Namespace,
						clientPod.Name,
						cmd)
					framework.ExpectNoError(err, "Checking ip route cache output failed")
					framework.Logf(" ip route output in client pod: %s", stdout)
					gomega.Expect(stdout).To(gomega.MatchRegexp("mtu 1342"))
				}
			})
		})
	})
})
