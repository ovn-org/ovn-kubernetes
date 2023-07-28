package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math/rand"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/onsi/ginkgo"
	"github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	utilnet "k8s.io/utils/net"
	"k8s.io/utils/pointer"

	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2eservice "k8s.io/kubernetes/test/e2e/framework/service"
)

var _ = ginkgo.Describe("Services", func() {
	const (
		serviceName = "testservice"

		echoServerPodNameTemplate = "echo-server-pod-%d"
		echoClientPodName         = "echo-client-pod"
		echoServiceNameTemplate   = "echo-service-%d"
		echoServerPodPortMin      = 9800
		echoServerPodPortMax      = 9899
		echoServicePortMin        = 31200
		echoServicePortMax        = 31299
	)

	f := wrappedTestFramework("services")

	var cs clientset.Interface

	ginkgo.BeforeEach(func() {
		cs = f.ClientSet
	})
	cleanupFn := func() {}

	ginkgo.AfterEach(func() {
		cleanupFn()
	})

	udpPort := int32(rand.Intn(1000) + 10000)
	udpPortS := fmt.Sprintf("%d", udpPort)

	ginkgo.It("Creates a host-network service, and ensures that host-network pods can connect to it", func() {
		namespace := f.Namespace.Name
		jig := e2eservice.NewTestJig(cs, namespace, serviceName)

		ginkgo.By("Creating a ClusterIP service")
		service, err := jig.CreateUDPService(func(s *v1.Service) {
			s.Spec.Ports = []v1.ServicePort{
				{
					Name:       "udp",
					Protocol:   v1.ProtocolUDP,
					Port:       80,
					TargetPort: intstr.FromInt(int(udpPort)),
				},
			}
		})
		framework.ExpectNoError(err)

		ginkgo.By("creating a host-network backend pod")

		serverPod := e2epod.NewAgnhostPod(namespace, "backend", nil, nil, []v1.ContainerPort{{ContainerPort: (udpPort)}, {ContainerPort: (udpPort), Protocol: "UDP"}},
			"netexec", "--udp-port="+udpPortS)
		serverPod.Labels = jig.Labels
		serverPod.Spec.HostNetwork = true

		serverPod = f.PodClient().CreateSync(serverPod)
		nodeName := serverPod.Spec.NodeName

		ginkgo.By("Connecting to the service from another host-network pod on node " + nodeName)
		// find the ovn-kube node pod on this node
		pods, err := cs.CoreV1().Pods("ovn-kubernetes").List(context.TODO(), metav1.ListOptions{
			LabelSelector: "app=ovnkube-node",
			FieldSelector: "spec.nodeName=" + nodeName,
		})
		framework.ExpectNoError(err)
		gomega.Expect(pods.Items).To(gomega.HaveLen(1))
		clientPod := pods.Items[0]

		cmd := fmt.Sprintf(`/bin/sh -c 'echo hostname | /usr/bin/socat -t 5 - "udp:%s"'`,
			net.JoinHostPort(service.Spec.ClusterIP, "80"))

		err = wait.PollImmediate(framework.Poll, 30*time.Second, func() (bool, error) {
			stdout, err := framework.RunHostCmdWithRetries(clientPod.Namespace, clientPod.Name, cmd, framework.Poll, 30*time.Second)
			if err != nil {
				return false, err
			}
			return stdout == nodeName, nil
		})
		framework.ExpectNoError(err)
	})

	// The below series of tests queries nodePort services with hostNetwork:true and hostNetwork:false pods as endpoints,
	// for both HTTP and UDP and different ingress and egress payload sizes.
	// Steps:
	// * Set up a hostNetwork:true|false pod (agnhost echo server) on node z
	// * Set up a nodePort service on node y
	// * Set up a hostNetwork:true client pod on node x
	// * Query from node x to the service on node y that targets the pod on node z
	for _, hostNetwork := range []bool{true, false} {
		hostNetwork := hostNetwork
		ginkgo.When(fmt.Sprintf("a nodePort service targeting a pod with hostNetwork:%t is created", hostNetwork), func() {
			var serverPod *v1.Pod
			var serverPodNodeName string
			var serverPodPort int
			var serverPodName string

			var svc v1.Service
			var serviceNode v1.Node
			var serviceNodeInternalIPs []string
			var servicePort int

			var clientPod *v1.Pod
			var clientPodNodeName string

			var echoPayloads = map[string]string{
				"small": fmt.Sprintf("%010d", 1),
				"large": fmt.Sprintf("%01420d", 1),
			}
			var echoMtuRegex = regexp.MustCompile(`cache expires.*mtu.*`)

			ginkgo.BeforeEach(func() {
				ginkgo.By("Selecting 3 schedulable nodes")
				nodes, err := e2enode.GetBoundedReadySchedulableNodes(f.ClientSet, 3)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(len(nodes.Items)).To(gomega.BeNumerically(">", 2))

				ginkgo.By("Selecting nodes for pods and service")
				serverPodNodeName = nodes.Items[0].Name
				serviceNode = nodes.Items[1]
				clientPodNodeName = nodes.Items[2].Name

				ginkgo.By("Getting all InternalIP addresses of the service node")
				serviceNodeInternalIPs = e2enode.GetAddresses(&serviceNode, v1.NodeInternalIP)
				gomega.Expect(len(serviceNodeInternalIPs)).To(gomega.BeNumerically(">", 0))

				ginkgo.By("Creating hostNetwork:true client pod")
				clientPod = e2epod.NewAgnhostPod(f.Namespace.Name, echoClientPodName, nil, nil, nil)
				clientPod.Spec.NodeName = clientPodNodeName
				clientPod.Spec.HostNetwork = true
				for k := range clientPod.Spec.Containers {
					if clientPod.Spec.Containers[k].Name == "agnhost-container" {
						clientPod.Spec.Containers[k].Command = []string{
							"sleep",
							"infinity",
						}
						clientPod.Spec.Containers[k].SecurityContext.Privileged = pointer.Bool(true)
					}
				}
				f.PodClient().CreateSync(clientPod)

				ginkgo.By(fmt.Sprintf("Creating the server pod with hostNetwork:%t", hostNetwork))
				// Create the server pod.
				// Wait for 1 minute and if the pod does not come up, select a different port and try again.
				// Wait for a max of 5 minutes.
				gomega.Eventually(func() error {
					serverPodPort = rand.Intn(echoServerPodPortMax-echoServerPodPortMin) + echoServerPodPortMin
					serverPodName = fmt.Sprintf(echoServerPodNameTemplate, serverPodPort)
					framework.Logf("Creating server pod listening on TCP and UDP port %d", serverPodPort)
					serverPod = e2epod.NewAgnhostPod(f.Namespace.Name, serverPodName, nil, nil, nil, "netexec",
						"--http-port",
						fmt.Sprintf("%d", serverPodPort),
						"--udp-port",
						fmt.Sprintf("%d", serverPodPort))
					serverPod.ObjectMeta.Labels = map[string]string{
						"app": serverPodName,
					}
					serverPod.Spec.HostNetwork = hostNetwork
					serverPod.Spec.NodeName = serverPodNodeName
					f.PodClient().Create(serverPod)

					err := e2epod.WaitTimeoutForPodReadyInNamespace(f.ClientSet, serverPod.Name, f.Namespace.Name, 1*time.Minute)
					if err != nil {
						f.PodClient().Delete(context.TODO(), serverPod.Name, metav1.DeleteOptions{})
						return err
					}
					serverPod, err = f.PodClient().Get(context.TODO(), serverPod.Name, metav1.GetOptions{})
					return err
				}, 5*time.Minute, 1*time.Second).Should(gomega.Succeed())

				ginkgo.By("Creating the nodePort service")
				// Create the service.
				// If the servicePorts are already in use, creating the service should fail and we should choose another
				// random port.
				gomega.Eventually(func() error {
					servicePort = rand.Intn(echoServicePortMax-echoServicePortMin) + echoServicePortMin
					framework.Logf("Creating the nodePort service listening on TCP and UDP port %d and targeting pod port %d",
						servicePort, serverPodPort)
					svc = v1.Service{
						ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf(echoServiceNameTemplate, servicePort)},
						Spec: v1.ServiceSpec{
							Ports: []v1.ServicePort{
								{
									Name:     "tcp-port",
									NodePort: int32(servicePort),
									Port:     int32(serverPodPort),
									Protocol: v1.ProtocolTCP,
								},
								{
									Name:     "udp-port",
									NodePort: int32(servicePort),
									Port:     int32(serverPodPort),
									Protocol: v1.ProtocolUDP,
								},
							},
							Selector: map[string]string{"app": serverPodName},
							Type:     v1.ServiceTypeNodePort},
					}
					_, err := f.ClientSet.CoreV1().Services(f.Namespace.Name).Create(context.TODO(), &svc, metav1.CreateOptions{})
					return err
				}, 60*time.Second, 1*time.Second).Should(gomega.Succeed())
			})

			// Run queries against the service both with a small (10 bytes + overhead for echo service) and
			// a large (1420 bytes + overhead for echo service) payload.
			// The payload is transmitted to and echoed from the echo service for both HTTP and UDP tests.
			ginkgo.When("tests are run towards the agnhost echo service", func() {
				ginkgo.It("queries to the nodePort service shall work for TCP", func() {
					for _, size := range []string{"small", "large"} {
						for _, serviceNodeIP := range serviceNodeInternalIPs {
							ginkgo.By(fmt.Sprintf("Sending TCP %s payload to service IP %s "+
								"and expecting to receive the same payload", size, serviceNodeIP))
							cmd := fmt.Sprintf("curl --max-time 10 -g -q -s http://%s:%d/echo?msg=%s",
								serviceNodeIP,
								servicePort,
								echoPayloads[size],
							)
							framework.Logf("Testing TCP %s with command %q", size, cmd)
							stdout, err := framework.RunHostCmdWithRetries(
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

				// We have 2 possible scenarios - hostNetwork endpoints and non-hostNetwork endpoints.
				// We will only see fragmentation for large (> 1400 Bytes) packets and non-hostNetwork endpoints.
				//
				// hostNetwork endpoints:
				// Packet from ovn-worker2 to GR_ovn-worker where it will hit LB:
				// # ovn-nbctl lr-lb-list GR_ovn-worker | grep udp | grep 31206
				// 083a13aa-13e0-45bb-8a1a-175ce7e9fe81    Service_services    udp        172.18.0.3:31206      10.244.2.3:9894
				// With routes:
				// # ovn-nbctl lr-route-list GR_ovn-worker  | grep dst-ip
				//            10.244.0.0/16                100.64.0.1 dst-ip
				//                0.0.0.0/0                172.18.0.1 dst-ip rtoe-GR_ovn-worker
				// This means that the packet will enter port rtoe-GR_ovn-worker, be load-balanced and
				// exit port rtoe-GR_ovn-worker right away.
				// In OVN, we must apply the gateway_mtu setting to the rtoj port:
				// # ovn-nbctl find Logical_Router_Port name=rtoj-GR_ovn-control-plane | grep options
				// options             : {gateway_mtu="1400"}
				// As a consequence, we shall never see fragmentation for hostNetwork:true pods.
				//
				// non-hostNetwork endpoints:
				// # ovn-nbctl lr-lb-list GR_ovn-worker | grep udp | grep 31206
				// 083a13aa-13e0-45bb-8a1a-175ce7e9fe81    Service_services    udp        172.18.0.3:31206      10.244.2.3:9894
				// # ovn-nbctl lr-route-list GR_ovn-worker  | grep dst-ip
				//            10.244.0.0/16                100.64.0.1 dst-ip
				//                0.0.0.0/0                172.18.0.1 dst-ip rtoe-GR_ovn-worker
				// This time, the packet will leave the rtoj port and it will be fragmented.
				ginkgo.It("queries to the nodePort service shall work for UDP", func() {
					for _, size := range []string{"small", "large"} {
						for _, serviceNodeIP := range serviceNodeInternalIPs {
							if size == "large" && !hostNetwork {
								// Flushing the IP route cache will remove any routes in the cache
								// that are a result of receiving a "need to frag" packet.
								ginkgo.By("Flushing the ip route cache")
								_, err := framework.RunHostCmdWithRetries(
									clientPod.Namespace,
									clientPod.Name,
									"ip route flush cache",
									framework.Poll,
									60*time.Second)
								framework.ExpectNoError(err, "Flushing the ip route cache failed")

								// List the current IP route cache for informative purposes.
								cmd := fmt.Sprintf("ip route get %s", serviceNodeIP)
								stdout, err := framework.RunHostCmd(
									clientPod.Namespace,
									clientPod.Name,
									cmd)
								framework.ExpectNoError(err, "Listing IP route cache")
								framework.Logf("%s: %s", cmd, stdout)
							}

							// We expect the following to fail at least once for large payloads and non-hostNetwork
							// endpoints: the first request will fail as we have to receive a "need to frag" ICMP
							// message, subsequent requests then should succeed.
							gomega.Eventually(func() error {
								ginkgo.By(fmt.Sprintf("Sending UDP %s payload to service IP %s "+
									"and expecting to receive the same payload", size, serviceNodeIP))
								// Send payload via UDP.
								cmd := fmt.Sprintf("echo 'echo %s' | nc -w2 -u %s %d",
									echoPayloads[size],
									serviceNodeIP,
									servicePort,
								)
								framework.Logf("Testing UDP %s with command %q", size, cmd)
								stdout, err := framework.RunHostCmd(
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

								if size == "large" && !hostNetwork {
									ginkgo.By("Making sure that the ip route cache contains an MTU route")
									// Get IP route cache and make sure that it contains an MTU route.
									cmd = fmt.Sprintf("ip route get %s", serviceNodeIP)
									stdout, err = framework.RunHostCmd(
										clientPod.Namespace,
										clientPod.Name,
										cmd)
									if err != nil {
										return fmt.Errorf("could not list IP route cache, err: %q", err)
									}
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
							ovnKubeNodePods, err := f.ClientSet.CoreV1().Pods(ovnNs).List(context.TODO(), metav1.ListOptions{
								LabelSelector: "name=ovnkube-node",
							})
							if err != nil {
								framework.Failf("could not get ovnkube-node pods: %v", err)
							}
							for _, ovnKubeNodePod := range ovnKubeNodePods.Items {
								framework.Logf("Flushing the ip route cache on %s", ovnKubeNodePod.Name)
								_, err := framework.RunHostCmdWithRetries(
									ovnNs,
									ovnKubeNodePod.Name,
									"ip route flush cache",
									framework.Poll,
									60*time.Second)
								framework.ExpectNoError(err, "Flushing the ip route cache failed")
							}
						}
					}
				})
			})
		})
	}

	// This test checks a special case: we add another IP address on the node *and* manually set that
	// IP address in to endpoints. It is used for some special apiserver hacks by remote cluster people.
	// So, ensure that it works for pod -> service and host -> service traffic
	ginkgo.It("All service features work when manually listening on a non-default address", func() {
		namespace := f.Namespace.Name
		jig := e2eservice.NewTestJig(cs, namespace, serviceName)
		nodes, err := e2enode.GetBoundedReadySchedulableNodes(cs, e2eservice.MaxNodesForEndpointsTests)
		framework.ExpectNoError(err)
		node := nodes.Items[0]
		nodeName := node.Name
		pods, err := cs.CoreV1().Pods("ovn-kubernetes").List(context.TODO(), metav1.ListOptions{
			LabelSelector: "app=ovnkube-node",
			FieldSelector: "spec.nodeName=" + nodeName,
		})
		framework.ExpectNoError(err)
		gomega.Expect(pods.Items).To(gomega.HaveLen(1))
		clientPod := &pods.Items[0]

		ginkgo.By("Using node" + nodeName + " and pod " + clientPod.Name)

		ginkgo.By("Creating an empty ClusterIP service")
		service, err := jig.CreateUDPService(func(s *v1.Service) {
			s.Spec.Ports = []v1.ServicePort{
				{
					Name:       "udp",
					Protocol:   v1.ProtocolUDP,
					Port:       80,
					TargetPort: intstr.FromInt(int(udpPort)),
				},
			}

			s.Spec.Selector = nil // because we will manage the endpoints ourselves
		})
		framework.ExpectNoError(err)

		ginkgo.By("Adding an extra IP address to the node's loopback interface")
		isV6 := strings.Contains(service.Spec.ClusterIP, ":")
		octet := rand.Intn(255)
		extraIP := fmt.Sprintf("192.0.2.%d", octet)
		extraCIDR := extraIP + "/32"
		if isV6 {
			extraIP = fmt.Sprintf("fc00::%d", octet)
			extraCIDR = extraIP + "/128"
		}

		cmd := fmt.Sprintf(`ip -br addr; ip addr del %s dev lo; ip addr add %s dev lo; ip -br addr`, extraCIDR, extraCIDR)
		_, err = framework.RunHostCmdWithRetries(clientPod.Namespace, clientPod.Name, cmd, framework.Poll, 30*time.Second)
		framework.ExpectNoError(err)
		cleanupFn = func() {
			// initial pod used for host command may be deleted at this point, refetch
			pods, err := cs.CoreV1().Pods("ovn-kubernetes").List(context.TODO(), metav1.ListOptions{
				LabelSelector: "app=ovnkube-node",
				FieldSelector: "spec.nodeName=" + nodeName,
			})
			framework.ExpectNoError(err)
			gomega.Expect(pods.Items).To(gomega.HaveLen(1))
			clientPod := &pods.Items[0]
			cmd := fmt.Sprintf(`ip addr del %s dev lo || true`, extraCIDR)
			_, err = framework.RunHostCmdWithRetries(clientPod.Namespace, clientPod.Name, cmd, framework.Poll, 30*time.Second)
		}

		ginkgo.By("Starting a UDP server listening on the additional IP")
		// now that 2.2.2.2 exists on the node's lo interface, let's start a server listening on it
		// we use UDP here since agnhost lets us pick the listen address only for UDP
		serverPod := e2epod.NewAgnhostPod(namespace, "backend", nil, nil, []v1.ContainerPort{{ContainerPort: (udpPort)}, {ContainerPort: (udpPort), Protocol: "UDP"}},
			"netexec", "--udp-port="+udpPortS, "--udp-listen-addresses="+extraIP)
		serverPod.Labels = jig.Labels
		serverPod.Spec.NodeName = nodeName
		serverPod.Spec.HostNetwork = true
		serverPod.Spec.Containers[0].TerminationMessagePolicy = v1.TerminationMessageFallbackToLogsOnError
		f.PodClient().CreateSync(serverPod)

		ginkgo.By("Ensuring the server is listening on the additional IP")
		// Connect from host -> additional IP. This shouldn't touch OVN at all, just acting as a basic
		// sanity check that we're actually listening on this IP
		err = wait.PollImmediate(framework.Poll, 30*time.Second, func() (bool, error) {
			cmd = fmt.Sprintf(`echo hostname | /usr/bin/socat -t 5 - "udp:%s"`,
				net.JoinHostPort(extraIP, udpPortS))
			stdout, err := framework.RunHostCmdWithRetries(clientPod.Namespace, clientPod.Name, cmd, framework.Poll, 30*time.Second)
			if err != nil {
				return false, err
			}
			return (stdout == nodeName), nil
		})
		framework.ExpectNoError(err)

		ginkgo.By("Adding this IP as a manual endpoint")
		_, err = f.ClientSet.CoreV1().Endpoints(namespace).Create(context.TODO(),
			&v1.Endpoints{
				ObjectMeta: metav1.ObjectMeta{Name: service.Name},
				Subsets: []v1.EndpointSubset{
					{
						Addresses: []v1.EndpointAddress{
							{
								IP: extraIP,
							},
						},
						Ports: []v1.EndpointPort{
							{
								Name:     "udp",
								Port:     udpPort,
								Protocol: "UDP",
							},
						},
					},
				},
			},
			metav1.CreateOptions{},
		)
		framework.ExpectNoError(err)

		ginkgo.By("Confirming that the service is accesible via the service IP from a host-network pod")
		err = wait.PollImmediate(framework.Poll, 30*time.Second, func() (bool, error) {
			cmd = fmt.Sprintf(`/bin/sh -c 'echo hostname | /usr/bin/socat -t 5 - "udp:%s"'`,
				net.JoinHostPort(service.Spec.ClusterIP, "80"))
			stdout, err := framework.RunHostCmdWithRetries(clientPod.Namespace, clientPod.Name, cmd, framework.Poll, 30*time.Second)
			if err != nil {
				return false, err
			}
			return stdout == nodeName, nil
		})
		framework.ExpectNoError(err)

		ginkgo.By("Confirming that the service is accessible from the node's pod network")
		// Now, spin up a pod-network pod on the same node, and ensure we can talk to the "local address" service
		clientServerPod := e2epod.NewAgnhostPod(namespace, "client", nil, nil, []v1.ContainerPort{{ContainerPort: (udpPort)}, {ContainerPort: (udpPort), Protocol: "UDP"}},
			"netexec")
		clientServerPod.Spec.NodeName = nodeName
		f.PodClient().CreateSync(clientServerPod)
		clientServerPod, err = f.PodClient().Get(context.TODO(), clientServerPod.Name, metav1.GetOptions{})
		framework.ExpectNoError(err)

		// annoying: need to issue a curl to the test pod to tell it to connect to the service
		err = wait.PollImmediate(framework.Poll, 30*time.Second, func() (bool, error) {
			cmd = fmt.Sprintf("curl -g -q -s 'http://%s/dial?request=%s&protocol=%s&host=%s&port=%d&tries=1'",
				net.JoinHostPort(clientServerPod.Status.PodIP, "8080"),
				"hostname",
				"udp",
				service.Spec.ClusterIP,
				80)
			stdout, err := framework.RunHostCmdWithRetries(clientPod.Namespace, clientPod.Name, cmd, framework.Poll, 30*time.Second)
			if err != nil {
				return false, err
			}
			return stdout == fmt.Sprintf(`{"responses":["%s"]}`, nodeName), nil
		})
		framework.ExpectNoError(err)
	})

	ginkgo.It("of type NodePort should listen on each host addresses", func() {
		const (
			endpointHTTPPort    = 80
			endpointUDPPort     = 90
			clusterHTTPPort     = 81
			clusterUDPPort      = 91
			clientContainerName = "npclient"
		)

		endPoints := make([]*v1.Pod, 0)
		endpointsSelector := map[string]string{"servicebackend": "true"}
		nodesHostnames := sets.NewString()

		nodes, err := e2enode.GetBoundedReadySchedulableNodes(f.ClientSet, 3)
		framework.ExpectNoError(err)

		if len(nodes.Items) < 3 {
			framework.Failf(
				"Test requires >= 3 Ready nodes, but there are only %v nodes",
				len(nodes.Items))
		}

		ginkgo.By("Creating the endpoints pod, one for each worker")
		for _, node := range nodes.Items {
			args := []string{
				"netexec",
				fmt.Sprintf("--http-port=%d", endpointHTTPPort),
				fmt.Sprintf("--udp-port=%d", endpointUDPPort),
			}
			pod, err := createPod(f, node.Name+"-ep", node.Name, f.Namespace.Name, []string{},
				endpointsSelector, func(p *v1.Pod) {
					p.Spec.Containers[0].Args = args
				})
			framework.ExpectNoError(err)

			endPoints = append(endPoints, pod)
			nodesHostnames.Insert(pod.Name)
		}

		ginkgo.By("Creating an external container to send the traffic from")
		createClusterExternalContainer(clientContainerName, agnhostImage,
			[]string{"--network", "kind", "-P"},
			[]string{"netexec", "--http-port=80"})

		// If `kindexgw` exists, connect client container to it
		runCommand(containerRuntime, "network", "connect", "kindexgw", clientContainerName)

		ginkgo.By("Adding ip addresses to each node")
		// add new secondary IP from node subnet to all nodes, if the cluster is v6 add an ipv6 address
		toCurlAddresses := sets.NewString()
		for i, node := range nodes.Items {

			addrAnnotation, ok := node.Annotations["k8s.ovn.org/host-addresses"]
			gomega.Expect(ok).To(gomega.BeTrue())

			var addrs []string
			err := json.Unmarshal([]byte(addrAnnotation), &addrs)
			framework.ExpectNoError(err, "failed to parse node[%s] host-address annotation[%s]", node.Name, addrAnnotation)
			for i, addr := range addrs {
				addrSplit := strings.Split(addr, "/")
				gomega.Expect(addrSplit).Should(gomega.HaveLen(2))
				addrs[i] = addrSplit[0]
			}
			toCurlAddresses.Insert(addrs...)

			var newIP string
			if utilnet.IsIPv6String(e2enode.GetAddresses(&node, v1.NodeInternalIP)[0]) {
				newIP = "fc00:f853:ccd:e794::" + strconv.Itoa(i)
			} else {
				newIP = "172.18.1." + strconv.Itoa(i+1)
			}
			// manually add the a secondary IP to each node
			_, err = runCommand(containerRuntime, "exec", node.Name, "ip", "addr", "add", newIP, "dev", "breth0")
			if err != nil {
				framework.Failf("failed to add new Addresses to node %s: %v", node.Name, err)
			}

			nodeName := node.Name
			defer func() {
				runCommand(containerRuntime, "exec", nodeName, "ip", "addr", "delete", newIP+"/32", "dev", "breth0")
				framework.ExpectNoError(err, "failed to remove ip address %s from node %s", newIP, nodeName)
			}()

			toCurlAddresses.Insert(newIP)
		}

		defer deleteClusterExternalContainer(clientContainerName)

		isIPv6Cluster := IsIPv6Cluster(f.ClientSet)

		ginkgo.By("Creating NodePort services")

		etpLocalServiceName := "etp-local-svc"
		etpLocalSvc := nodePortServiceSpecFrom(etpLocalServiceName, v1.IPFamilyPolicyPreferDualStack, endpointHTTPPort, endpointUDPPort, clusterHTTPPort, clusterUDPPort, endpointsSelector, v1.ServiceExternalTrafficPolicyTypeLocal)
		etpLocalSvc, err = f.ClientSet.CoreV1().Services(f.Namespace.Name).Create(context.Background(), etpLocalSvc, metav1.CreateOptions{})
		framework.ExpectNoError(err)

		etpClusterServiceName := "etp-cluster-svc"
		etpClusterSvc := nodePortServiceSpecFrom(etpClusterServiceName, v1.IPFamilyPolicyPreferDualStack, endpointHTTPPort, endpointUDPPort, clusterHTTPPort, clusterUDPPort, endpointsSelector, v1.ServiceExternalTrafficPolicyTypeCluster)
		etpClusterSvc, err = f.ClientSet.CoreV1().Services(f.Namespace.Name).Create(context.Background(), etpClusterSvc, metav1.CreateOptions{})
		framework.ExpectNoError(err)

		ginkgo.By("Waiting for the endpoints to pop up")

		err = framework.WaitForServiceEndpointsNum(f.ClientSet, f.Namespace.Name, etpLocalServiceName, len(endPoints), time.Second, wait.ForeverTestTimeout)
		framework.ExpectNoError(err, "failed to validate endpoints for service %s in namespace: %s", etpLocalServiceName, f.Namespace.Name)

		err = framework.WaitForServiceEndpointsNum(f.ClientSet, f.Namespace.Name, etpClusterServiceName, len(endPoints), time.Second, wait.ForeverTestTimeout)
		framework.ExpectNoError(err, "failed to validate endpoints for service %s in namespace: %s", etpClusterServiceName, f.Namespace.Name)

		for _, serviceSpec := range []*v1.Service{etpLocalSvc, etpClusterSvc} {
			tcpNodePort, udpNodePort := nodePortsFromService(serviceSpec)

			for _, protocol := range []string{"http", "udp"} {
				toCurlPort := int32(tcpNodePort)
				if protocol == "udp" {
					toCurlPort = int32(udpNodePort)
				}

				for _, address := range toCurlAddresses.List() {
					if !isIPv6Cluster && utilnet.IsIPv6String(address) {
						continue
					}

					ginkgo.By("Hitting service " + serviceSpec.Name + " on " + address + " via " + protocol)
					gomega.Eventually(func() bool {
						epHostname := pokeEndpoint("", clientContainerName, protocol, address, toCurlPort, "hostname")
						// Expect to receive a valid hostname
						return nodesHostnames.Has(epHostname)
					}, "20s", "1s").Should(gomega.BeTrue())
				}
			}
		}
	})
})

// This test ensures that - when a pod that's a backend for a service curls the
// service ip; if the traffic was DNAT-ed to the same src pod (hairpin/loopback case) -
// the srcIP of reply traffic is SNATed to the special masqurade IP 169.254.169.5
// or "fd69::5"
var _ = ginkgo.Describe("Service Hairpin SNAT", func() {
	const (
		svcName                 = "service-hairpin-test"
		backendName             = "hairpin-backend-pod"
		endpointHTTPPort        = "80"
		serviceHTTPPort         = 6666
		V4LBHairpinMasqueradeIP = "169.254.169.5"
		V6LBHairpinMasqueradeIP = "fd69::5"
	)

	var (
		svcIP           string
		isIpv6          bool
		namespaceName   string
		backendNodeName string
		nodeIP          string
	)

	f := wrappedTestFramework(svcName)
	hairpinPodSel := map[string]string{"hairpinbackend": "true"}

	ginkgo.BeforeEach(func() {
		nodes, err := e2enode.GetBoundedReadySchedulableNodes(f.ClientSet, 2)
		framework.ExpectNoError(err)
		if len(nodes.Items) < 2 {
			framework.Failf("Test requires >= 2 Ready nodes, but there are only %v nodes", len(nodes.Items))
		}
		ips := e2enode.CollectAddresses(nodes, v1.NodeInternalIP)
		namespaceName = f.Namespace.Name
		backendNodeName = nodes.Items[0].Name
		nodeIP = ips[1]
	})

	ginkgo.It("Should ensure service hairpin traffic is SNATed to hairpin masquerade IP; Switch LB", func() {

		ginkgo.By("creating an ovn-network backend pod")
		_, err := createGenericPodWithLabel(f, backendName, backendNodeName, namespaceName, []string{"/agnhost", "netexec", fmt.Sprintf("--http-port=%s", endpointHTTPPort)}, hairpinPodSel)
		framework.ExpectNoError(err, fmt.Sprintf("unable to create backend pod: %s, err: %v", backendName, err))

		ginkgo.By("creating a TCP service service-for-pods with type=ClusterIP in namespace " + namespaceName)

		svcIP, err = createServiceForPodsWithLabel(f, namespaceName, serviceHTTPPort, endpointHTTPPort, "ClusterIP", hairpinPodSel)
		framework.ExpectNoError(err, fmt.Sprintf("unable to create service: service-for-pods, err: %v", err))

		err = framework.WaitForServiceEndpointsNum(f.ClientSet, namespaceName, "service-for-pods", 1, time.Second, wait.ForeverTestTimeout)
		framework.ExpectNoError(err, fmt.Sprintf("service: service-for-pods never had an endpoint, err: %v", err))

		ginkgo.By("by sending a TCP packet to service service-for-pods with type=ClusterIP in namespace " + namespaceName + " from backend pod " + backendName)

		if utilnet.IsIPv6String(svcIP) {
			framework.Logf("service: service-for-pods is ipv6")
			isIpv6 = true
		}

		clientIP := pokeEndpoint(namespaceName, backendName, "http", svcIP, serviceHTTPPort, "clientip")
		clientIP, _, err = net.SplitHostPort(clientIP)
		framework.ExpectNoError(err, "failed to parse client ip:port")

		if isIpv6 {
			framework.ExpectEqual(clientIP, V6LBHairpinMasqueradeIP, fmt.Sprintf("returned client ipv6: %v was not correct", clientIP))
		} else {
			framework.ExpectEqual(clientIP, V4LBHairpinMasqueradeIP, fmt.Sprintf("returned client ipv4: %v was not correct", clientIP))
		}
	})

	ginkgo.It("Should ensure service hairpin traffic is NOT SNATed to hairpin masquerade IP; GR LB", func() {

		ginkgo.By("creating an host-network backend pod on " + backendNodeName)
		// create hostNeworkedPods
		_, err := createPod(f, backendName, backendNodeName, namespaceName, []string{}, hairpinPodSel, func(p *v1.Pod) {
			p.Spec.Containers[0].Command = []string{"/agnhost", "netexec", fmt.Sprintf("--http-port=%s", endpointHTTPPort)}
			p.Spec.HostNetwork = true
		})
		framework.ExpectNoError(err, fmt.Sprintf("unable to create backend pod: %s, err: %v", backendName, err))

		ginkgo.By("creating a TCP service service-for-pods with type=NodePort in namespace " + namespaceName)

		svcIP, err = createServiceForPodsWithLabel(f, namespaceName, serviceHTTPPort, endpointHTTPPort, "NodePort", hairpinPodSel)
		framework.ExpectNoError(err, fmt.Sprintf("unable to create service: service-for-pods, err: %v", err))

		err = framework.WaitForServiceEndpointsNum(f.ClientSet, namespaceName, "service-for-pods", 1, time.Second, wait.ForeverTestTimeout)
		framework.ExpectNoError(err, fmt.Sprintf("service: service-for-pods never had an endpoint, err: %v", err))

		svc, err := f.ClientSet.CoreV1().Services(namespaceName).Get(context.TODO(), "service-for-pods", metav1.GetOptions{})
		framework.ExpectNoError(err, "failed to fetch service: service-for-pods")

		ginkgo.By("by sending a TCP packet to service service-for-pods with type=NodePort(" + nodeIP + ":" + fmt.Sprint(svc.Spec.Ports[0].NodePort) + ") in namespace " + namespaceName + " from node " + backendNodeName)

		clientIP := pokeEndpoint("", backendNodeName, "http", nodeIP, svc.Spec.Ports[0].NodePort, "clientip")
		clientIP, _, err = net.SplitHostPort(clientIP)
		framework.ExpectNoError(err, "failed to parse client ip:port")

		framework.ExpectEqual(clientIP, nodeIP, fmt.Sprintf("returned client: %v was not correct", clientIP))
	})

})

var _ = ginkgo.Describe("Load Balancer Service Tests with MetalLB", func() {

	const (
		svcName          = "lbservice-test"
		backendName      = "lb-backend-pod"
		endpointHTTPPort = 80
		loadBalancerYaml = "loadbalancer.yaml"
		namespaceName    = "default"
		svcIP            = "192.168.10.0"
		clientContainer  = "lbclient"
		routerContainer  = "frr"
	)

	var (
		backendNodeName string
	)

	f := wrappedTestFramework(svcName)
	ginkgo.BeforeEach(func() {
		nodes, err := e2enode.GetBoundedReadySchedulableNodes(f.ClientSet, 2)
		framework.ExpectNoError(err)
		if len(nodes.Items) < 2 {
			framework.Failf("Test requires >= 2 Ready nodes, but there are only %v nodes", len(nodes.Items))
		}
		backendNodeName = nodes.Items[0].Name
		var loadBalancerServiceConfig = fmt.Sprintf(`
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: dynamic-claim
spec:
  accessModes:
    - ReadWriteOnce
  storageClassName: standard
  resources:
    requests:
      storage: 1000Mi

---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ` + backendName + `
spec:
  replicas: 4
  selector:
    matchLabels:
      app: nginx
  template:
    metadata:
      labels:
        app: nginx
    spec:
      volumes:
        - name: data
          persistentVolumeClaim:
           claimName: dynamic-claim
      initContainers:
      - name: get-big-file
        image: quay.io/itssurya/dev-images:metallb-lbservice
        command: ['sh', '-c', "dd if=/dev/zero of=/usr/share/nginx/html/big.iso  bs=1024 count=0 seek=102400"]
        volumeMounts:
        - name: data
          mountPath: "/usr/share/nginx/html"
      containers:
      - name: nginx
        image: nginx:1
        volumeMounts:
        - name: data
          mountPath: "/usr/share/nginx/html"
        ports:
        - name: http
          containerPort: 80
      nodeSelector:
        kubernetes.io/hostname: ` + backendNodeName + `

---
apiVersion: v1
kind: Service
metadata:
  name: ` + svcName + `
spec:
  ports:
  - name: http
    port: 80
    protocol: TCP
    targetPort: 80
  selector:
    app: nginx
  type: LoadBalancer
`)
		if err := ioutil.WriteFile(loadBalancerYaml, []byte(loadBalancerServiceConfig), 0644); err != nil {
			framework.Failf("Unable to write CRD config to disk: %v", err)
		}
		framework.Logf("Create the Load Balancer configuration")
		framework.RunKubectlOrDie("default", "create", "-f", loadBalancerYaml)

	})

	ginkgo.AfterEach(func() {
		framework.Logf("Delete the Load Balancer configuration")
		framework.RunKubectlOrDie("default", "delete", "-f", loadBalancerYaml)
		defer func() {
			if err := os.Remove(loadBalancerYaml); err != nil {
				framework.Logf("Unable to remove the CRD config from disk: %v", err)
			}
		}()
	})

	ginkgo.It("Should ensure load balancer service works with pmtu", func() {

		err := framework.WaitForServiceEndpointsNum(f.ClientSet, namespaceName, svcName, 4, time.Second, time.Second*120)
		framework.ExpectNoError(err, fmt.Sprintf("service: %s never had an endpoint, err: %v", svcName, err))

		time.Sleep(time.Second * 5) // buffer to ensure all rules are created correctly

		numberOfETPRules := pokeIPTableRules(backendNodeName, "OVN-KUBE-EXTERNALIP")
		framework.ExpectEqual(numberOfETPRules, 4)

		ginkgo.By("by sending a TCP packet to service " + svcName + " with type=LoadBalancer in namespace " + namespaceName + " from backend pod " + backendName)

		_, err = curlInContainer(clientContainer, svcIP, endpointHTTPPort, "big.iso -o big.iso", 120)
		framework.ExpectNoError(err, "failed to curl load balancer service")

		ginkgo.By("change MTU on intermediary router to force icmp related packets")
		cmd := []string{containerRuntime, "exec", routerContainer}
		mtuCommand := strings.Split("ip link set mtu 1200 dev eth1", " ")

		cmd = append(cmd, mtuCommand...)
		_, err = runCommand(cmd...)
		framework.ExpectNoError(err, "failed to change MTU on intermediary router")

		time.Sleep(time.Second * 5) // buffer to ensure MTU change took effect

		ginkgo.By("by sending a TCP packet to service " + svcName + " with type=LoadBalancer in namespace " + namespaceName + " from backend pod " + backendName)

		_, err = curlInContainer(clientContainer, svcIP, endpointHTTPPort, "big.iso -o big.iso", 120)
		framework.ExpectNoError(err, "failed to curl load balancer service")
	})

	ginkgo.It("Should ensure load balancer service works with 0 node ports when ETP=local", func() {

		err := framework.WaitForServiceEndpointsNum(f.ClientSet, namespaceName, svcName, 4, time.Second, time.Second*120)
		framework.ExpectNoError(err, fmt.Sprintf("service: %s never had an enpoint, err: %v", svcName, err))

		time.Sleep(time.Second * 5) // buffer to ensure all rules are created correctly

		checkNumberOfETPRules := func(value int, pattern string) wait.ConditionFunc {
			return func() (bool, error) {
				numberOfETPRules := pokeIPTableRules(backendNodeName, pattern)
				return (numberOfETPRules == value), nil
			}
		}
		err = wait.PollImmediate(retryInterval, retryTimeout, checkNumberOfETPRules(2, "OVN-KUBE-ETP"))
		framework.ExpectNoError(err, "Couldn't fetch the correct number of iptable rules, err: %v", err)
		err = wait.PollImmediate(retryInterval, retryTimeout, checkNumberOfETPRules(4, "OVN-KUBE-EXTERNALIP"))
		framework.ExpectNoError(err, "Couldn't fetch the correct number of iptable rules, err: %v", err)
		err = wait.PollImmediate(retryInterval, retryTimeout, checkNumberOfETPRules(3, "OVN-KUBE-SNAT-MGMTPORT"))
		framework.ExpectNoError(err, "Couldn't fetch the correct number of iptable rules, err: %v", err)

		ginkgo.By("by sending a TCP packet to service " + svcName + " with type=LoadBalancer in namespace " + namespaceName + " from backend pod " + backendName)

		_, err = curlInContainer(clientContainer, svcIP, endpointHTTPPort, "big.iso -o big.iso", 120)
		framework.ExpectNoError(err, "failed to curl load balancer service")

		ginkgo.By("patching service " + svcName + " to allocateLoadBalancerNodePorts=false and externalTrafficPolicy=local")

		err = patchServiceBoolValue(f.ClientSet, svcName, "default", "/spec/allocateLoadBalancerNodePorts", false)
		framework.ExpectNoError(err)

		output := framework.RunKubectlOrDie("default", "get", "svc", svcName, "-o=jsonpath='{.spec.allocateLoadBalancerNodePorts}'")
		framework.ExpectEqual(output, "'false'")

		err = patchServiceStringValue(f.ClientSet, svcName, "default", "/spec/externalTrafficPolicy", "Local")
		framework.ExpectNoError(err)

		output = framework.RunKubectlOrDie("default", "get", "svc", svcName, "-o=jsonpath='{.spec.externalTrafficPolicy}'")
		framework.ExpectEqual(output, "'Local'")

		time.Sleep(time.Second * 5) // buffer to ensure all rules are created correctly

		err = wait.PollImmediate(retryInterval, retryTimeout, checkNumberOfETPRules(6, "OVN-KUBE-ETP"))
		framework.ExpectNoError(err, "Couldn't fetch the correct number of iptable rules, err: %v", err)
		err = wait.PollImmediate(retryInterval, retryTimeout, checkNumberOfETPRules(7, "OVN-KUBE-SNAT-MGMTPORT"))
		framework.ExpectNoError(err, "Couldn't fetch the correct number of iptable rules, err: %v", err)

		ginkgo.By("by sending a TCP packet to service " + svcName + " with type=LoadBalancer in namespace " + namespaceName + " from backend pod " + backendName)

		_, err = curlInContainer(clientContainer, svcIP, endpointHTTPPort, "big.iso -o big.iso", 120)
		framework.ExpectNoError(err, "failed to curl load balancer service")

		err = wait.PollImmediate(retryInterval, retryTimeout, checkNumberOfETPRules(1, "[1:60] -A OVN-KUBE-ETP"))
		framework.ExpectNoError(err, "Couldn't fetch the correct number of iptable rules, err: %v", err)
		err = wait.PollImmediate(retryInterval, retryTimeout, checkNumberOfETPRules(1, "[1:60] -A OVN-KUBE-SNAT-MGMTPORT"))
		framework.ExpectNoError(err, "Couldn't fetch the correct number of iptable rules, err: %v", err)

		ginkgo.By("Scale down endpoints of service: " + svcName + " to ensure iptable rules are also getting recreated correctly")
		framework.RunKubectlOrDie("default", "scale", "deployment", backendName, "--replicas=3")
		err = framework.WaitForServiceEndpointsNum(f.ClientSet, namespaceName, svcName, 3, time.Second, time.Second*120)
		framework.ExpectNoError(err, fmt.Sprintf("service: %s never had an endpoint, err: %v", svcName, err))

		time.Sleep(time.Second * 5) // buffer to ensure all rules are created correctly
		// number of iptable rules should have decreased by 1
		err = wait.PollImmediate(retryInterval, retryTimeout, checkNumberOfETPRules(5, "OVN-KUBE-ETP"))
		framework.ExpectNoError(err, "Couldn't fetch the correct number of iptable rules, err: %v", err)
		err = wait.PollImmediate(retryInterval, retryTimeout, checkNumberOfETPRules(6, "OVN-KUBE-SNAT-MGMTPORT"))
		framework.ExpectNoError(err, "Couldn't fetch the correct number of iptable rules, err: %v", err)

		ginkgo.By("by sending a TCP packet to service " + svcName + " with type=LoadBalancer in namespace " + namespaceName + " from backend pod " + backendName)

		_, err = curlInContainer(clientContainer, svcIP, endpointHTTPPort, "big.iso -o big.iso", 120)
		framework.ExpectNoError(err, "failed to curl load balancer service")

		err = wait.PollImmediate(retryInterval, retryTimeout, checkNumberOfETPRules(1, "[1:60] -A OVN-KUBE-ETP"))
		framework.ExpectNoError(err, "Couldn't fetch the correct number of iptable rules, err: %v", err)
		err = wait.PollImmediate(retryInterval, retryTimeout, checkNumberOfETPRules(1, "[1:60] -A OVN-KUBE-SNAT-MGMTPORT"))
		framework.ExpectNoError(err, "Couldn't fetch the correct number of iptable rules, err: %v", err)

	})

})
