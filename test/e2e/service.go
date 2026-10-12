// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/feature"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/images"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider"
	infraapi "github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/api"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/ipalloc"
	e2eendpointslice "k8s.io/kubernetes/test/e2e/framework/endpointslice"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2epodoutput "k8s.io/kubernetes/test/e2e/framework/pod/output"
	e2erc "k8s.io/kubernetes/test/e2e/framework/rc"
	e2eservice "k8s.io/kubernetes/test/e2e/framework/service"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	testutils "k8s.io/kubernetes/test/utils"
	utilnet "k8s.io/utils/net"
	"k8s.io/utils/pointer"
	"k8s.io/utils/ptr"
)

const (
	metallbNamespace      = "metallb-system"
	speakerLabelgSelector = "component=speaker"
)

var (
	reportPath string
)

var _ = ginkgo.Describe("Services", feature.Service, func() {
	const (
		serviceName               = "testservice"
		echoServerPodNameTemplate = "echo-server-pod-%d"
		echoClientPodName         = "echo-client-pod"
		echoServiceName           = "echo-service"
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

	ginkgo.It("Allow connection to an external IP using a source port that is equal to a node port", func() {
		var nodePort int32 = 31990
		const (
			connTimeout = "2"
			dstIPv4     = "1.1.1.1"
			dstPort     = "80"
		)
		if IsIPv6Cluster(f.ClientSet) {
			e2eskipper.Skipf("Test requires IPv4 or IPv4 primary dualstack cluster")
		}
		ginkgo.By("create node port service")
		jig := e2eservice.NewTestJig(cs, f.Namespace.Name, serviceName)
		err := wait.PollImmediate(10*time.Second, 60*time.Second,
			func() (bool, error) {
				_, err := jig.CreateTCPService(context.TODO(), func(svc *v1.Service) {
					svc.Spec.Type = v1.ServiceTypeNodePort
					svc.Spec.Ports[0].NodePort = nodePort
				})
				if err != nil {
					if strings.Contains(err.Error(), "provided port is already allocated") {
						nodePort++
						return false, nil
					}
					return false, err
				}
				return true, nil
			})
		framework.ExpectNoError(err, "failed to create TCP node port service")
		ginkgo.By("create pod selected by node port service")
		serverPod := e2epod.NewAgnhostPod(f.Namespace.Name, "svc-backend", nil, nil, nil)
		serverPod.Labels = jig.Labels
		e2epod.NewPodClient(f).CreateSync(context.TODO(), serverPod)
		ginkgo.By("create pod which will connect externally")
		clientPod := e2epod.NewAgnhostPod(f.Namespace.Name, "client-for-external", nil, nil, nil)
		e2epod.NewPodClient(f).CreateSync(context.TODO(), clientPod)
		ginkgo.By("connect externally pinning the source port to equal the node port")
		_, err = e2ekubectl.RunKubectl(clientPod.Namespace, "exec", clientPod.Name, "--", "nc",
			"-p", strconv.Itoa(int(nodePort)), "-z", "-w", connTimeout, dstIPv4, dstPort)
		framework.ExpectNoError(err, "expected connection to succeed using source port identical to node port")
	})

	ginkgo.It("Creates a host-network service, and ensures that host-network pods can connect to it", func() {
		namespace := f.Namespace.Name
		jig := e2eservice.NewTestJig(cs, namespace, serviceName)

		ginkgo.By("Creating a ClusterIP service")
		targetPort := infraprovider.Get().GetK8HostPort()
		service, err := jig.CreateUDPService(context.TODO(), func(s *v1.Service) {
			s.Spec.Ports = []v1.ServicePort{
				{
					Name:       "udp",
					Protocol:   v1.ProtocolUDP,
					Port:       80,
					TargetPort: intstr.FromInt(int(targetPort)),
				},
			}
		})
		framework.ExpectNoError(err)

		ginkgo.By("creating a host-network backend pod")

		httpPort := infraprovider.Get().GetK8HostPort()
		serverPod := e2epod.NewAgnhostPod(namespace, "backend", nil, nil, []v1.ContainerPort{{ContainerPort: (int32(targetPort))}, {ContainerPort: (int32(targetPort)), Protocol: "UDP"}},
			"netexec", fmt.Sprintf("--http-port=%d", httpPort), fmt.Sprintf("--udp-port=%d", targetPort))
		serverPod.Labels = jig.Labels
		serverPod.Spec.HostNetwork = true

		serverPod = e2epod.NewPodClient(f).CreateSync(context.TODO(), serverPod)
		nodeName := serverPod.Spec.NodeName

		ginkgo.By("Connecting to the service from node " + nodeName)
		err = wait.PollImmediate(framework.Poll, 30*time.Second, func() (bool, error) {
			response, err := tryPokeEndpointViaNode(nodeName, "udp", service.Spec.ClusterIP, uint16(httpPort), 80, "hostname")
			if err != nil {
				return false, nil
			}
			return hostnameMatchesNode(response, nodeName), nil
		})
		framework.ExpectNoError(err)
	})

	ginkgo.It("Creates a service with session-affinity, and ensures it works after backend deletion", func() {
		namespace := f.Namespace.Name
		servicePort := 80
		jig := e2eservice.NewTestJig(cs, namespace, serviceName)

		ginkgo.By("Creating a session-affinity service")
		var createdPods []*v1.Pod
		maxContainerFailures := 0
		replicas := 3
		targetPort := infraprovider.Get().GetK8HostPort()
		config := testutils.RCConfig{
			Client:               cs,
			Image:                images.AgnHost(),
			Command:              []string{"/agnhost", "serve-hostname", fmt.Sprintf("--port=%d", targetPort)},
			Name:                 "backend",
			Labels:               jig.Labels,
			Namespace:            namespace,
			PollInterval:         3 * time.Second,
			Timeout:              framework.PodReadyBeforeTimeout,
			Replicas:             replicas,
			CreatedPods:          &createdPods,
			MaxContainerFailures: &maxContainerFailures,
		}
		err := e2erc.RunRC(context.TODO(), config)
		gomega.Expect(err).ToNot(gomega.HaveOccurred())
		gomega.Expect(len(createdPods)).To(gomega.Equal(replicas), fmt.Sprintf("incorrect number of running pods: %v", len(createdPods)))

		svc, err := jig.CreateTCPService(context.TODO(), func(s *v1.Service) {
			s.Spec.SessionAffinity = "ClientIP"
			s.Spec.Type = v1.ServiceTypeClusterIP
			s.Spec.Ports = []v1.ServicePort{{
				Port: int32(servicePort),
				// agnhost serve-hostname port
				TargetPort: intstr.FromInt(int(targetPort)),
				Protocol:   v1.ProtocolTCP,
			}}
		})
		framework.ExpectNoError(err)

		execPod := e2epod.CreateExecPodOrFail(context.TODO(), cs, namespace, "execpod-affinity", nil)
		err = jig.CheckServiceReachability(context.TODO(), svc, execPod)
		framework.ExpectNoError(err)

		ensureStickySession := func() string {
			hosts := getServiceBackendsFromPod(execPod, svc.Spec.ClusterIP, int(svc.Spec.Ports[0].Port))
			uniqHosts := sets.New(hosts...)
			gomega.Expect(uniqHosts.Len()).To(gomega.Equal(1), fmt.Sprintf("expected the same backend for every connection with session-affinity set, got %v", uniqHosts))
			backendPod, _ := uniqHosts.PopAny()
			return backendPod
		}

		ginkgo.By("check sessions affinity from a client pod")
		backendPod := ensureStickySession()

		ginkgo.By(fmt.Sprintf("delete chosen backend pod %v", backendPod))
		err = e2epod.NewPodClient(f).Delete(context.TODO(), backendPod, metav1.DeleteOptions{})
		framework.ExpectNoError(err)
		err = e2epod.WaitForPodNotFoundInNamespace(context.TODO(), cs, backendPod, namespace, 60*time.Second)
		framework.ExpectNoError(err)

		ginkgo.By("check sessions affinity from a client pod again")
		ensureStickySession()
	})

	// The below series of tests queries nodePort services with hostNetwork:true and hostNetwork:false pods as endpoints,
	// for both HTTP and UDP and different ingress and egress payload sizes. It tests both for named target ports and
	// integer target ports, as well as for different external traffic policies.
	// Steps:
	// * Set up a hostNetwork:true|false pod (agnhost echo server) on node z
	// * Set up a nodePort service on node y with ETP:cluster|local and named ports:true|false
	// * Set up a hostNetwork:true client pod on node x
	// * For ETP==cluster: Query from node x to the service on node y that targets the pod on node z
	//   For ETP==local: Query from node x to the service on node y that targets the pod on node y
	for _, hostNetwork := range []bool{true, false} {
		for _, namedPort := range []bool{true, false} {
			for _, etp := range []v1.ServiceExternalTrafficPolicy{v1.ServiceExternalTrafficPolicyCluster, v1.ServiceExternalTrafficPolicyLocal} {
				ginkgo.When(fmt.Sprintf("a nodePort service targeting a pod with hostNetwork:%t, namedPort:%t, ETP:%s is created",
					hostNetwork, namedPort, etp), func() {
					var serverPod *v1.Pod
					var serverPodNodeName string
					var serverPodName string

					var svc *v1.Service
					var serviceNode v1.Node
					var serviceNodeInternalIPs []string

					var clientPod *v1.Pod
					var clientPodNodeName string

					var echoPayloads = map[string]string{
						"small": fmt.Sprintf("%010d", 1),
						"large": fmt.Sprintf("%01420d", 1),
					}
					var echoMtuRegex = regexp.MustCompile(`cache expires.*mtu.*`)
					tcpPortName := "tcp-port"
					udpPortName := "udp-port"

					ginkgo.BeforeEach(func() {
						ginkgo.By("Selecting 3 schedulable nodes")
						nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), f.ClientSet, 3)
						gomega.Expect(err).NotTo(gomega.HaveOccurred())
						if len(nodes.Items) < 3 {
							e2eskipper.Skipf("Test requires >= 3 Ready nodes, but there are only %v nodes", len(nodes.Items))
						}

						ginkgo.By("Selecting node for pods")
						serverPodNodeName = nodes.Items[0].Name
						if etp == v1.ServiceExternalTrafficPolicyLocal {
							ginkgo.By("Selecting the same node for the service due to ETP=local")
							serviceNode = nodes.Items[0]
						} else {
							ginkgo.By("Selecting another node for the service due to ETP=cluster")
							serviceNode = nodes.Items[1]
						}
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
						e2epod.NewPodClient(f).CreateSync(context.TODO(), clientPod)

						ginkgo.By(fmt.Sprintf("Creating the server pod with hostNetwork:%t", hostNetwork))
						// Create the server pod.
						// Wait for 1 minute and if the pod does not come up, select a different port and try again.
						// Wait for a max of 5 minutes.
						var serverPodPortTCP, serverPodPortUDP uint16
						gomega.Eventually(func() error {
							// May be a host net or cluster net pod but select host port anyway.
							serverPodPortTCP = infraprovider.Get().GetK8HostPort()
							serverPodPortUDP = infraprovider.Get().GetK8HostPort()
							serverPodName = fmt.Sprintf(echoServerPodNameTemplate, serverPodPortTCP)
							framework.Logf("Creating server pod listening on TCP port %d and UDP port %d",
								serverPodPortTCP, serverPodPortUDP)
							serverPod = e2epod.NewAgnhostPod(f.Namespace.Name, serverPodName, nil, nil, nil, "netexec",
								"--http-port",
								fmt.Sprintf("%d", serverPodPortTCP),
								"--udp-port",
								fmt.Sprintf("%d", serverPodPortUDP))
							serverPod.ObjectMeta.Labels = map[string]string{
								"app": serverPodName,
							}
							serverPod.Spec.HostNetwork = hostNetwork
							serverPod.Spec.NodeName = serverPodNodeName
							if namedPort {
								serverPod.Spec.Containers[0].Ports = []v1.ContainerPort{
									{
										ContainerPort: int32(serverPodPortTCP),
										HostPort:      int32(serverPodPortTCP),
										Name:          "http",
										Protocol:      v1.ProtocolTCP,
									},
									{
										ContainerPort: int32(serverPodPortUDP),
										HostPort:      int32(serverPodPortUDP),
										Name:          "udp",
										Protocol:      v1.ProtocolUDP,
									},
								}
							}
							e2epod.NewPodClient(f).Create(context.TODO(), serverPod)

							err := e2epod.WaitTimeoutForPodReadyInNamespace(context.TODO(), f.ClientSet, serverPod.Name, f.Namespace.Name, 1*time.Minute)
							if err != nil {
								e2epod.NewPodClient(f).Delete(context.TODO(), serverPod.Name, metav1.DeleteOptions{})
								return err
							}
							serverPod, err = e2epod.NewPodClient(f).Get(context.TODO(), serverPod.Name, metav1.GetOptions{})
							return err
						}, 5*time.Minute, 1*time.Second).Should(gomega.Succeed())

						ginkgo.By("Creating the nodePort service")
						// Create the service.
						// If the servicePorts are already in use, creating the service should fail and we should choose another
						// random port.
						gomega.Eventually(func() error {
							servicePortTCP := rand.Intn(32767-30000) + 30000
							servicePortUDP := rand.Intn(32767-30000) + 30000

							framework.Logf("Creating the nodePort service")
							targetPortTCP := intstr.FromInt(int(serverPodPortTCP))
							targetPortUDP := intstr.FromInt(int(serverPodPortUDP))
							if namedPort {
								targetPortTCP = intstr.FromString("http")
								targetPortUDP = intstr.FromString("udp")
							}
							svc = &v1.Service{
								ObjectMeta: metav1.ObjectMeta{Name: echoServiceName},
								Spec: v1.ServiceSpec{
									ExternalTrafficPolicy: etp,
									Ports: []v1.ServicePort{
										{
											Name:       tcpPortName,
											NodePort:   int32(servicePortTCP),
											Port:       int32(serverPodPortTCP),
											TargetPort: targetPortTCP,
											Protocol:   v1.ProtocolTCP,
										},
										{
											Name:       udpPortName,
											NodePort:   int32(servicePortUDP),
											Port:       int32(serverPodPortUDP),
											TargetPort: targetPortUDP,
											Protocol:   v1.ProtocolUDP,
										},
									},
									Selector: map[string]string{"app": serverPodName},
									Type:     v1.ServiceTypeNodePort},
							}
							if isDualStackCluster(nodes) {
								svc.Spec.IPFamilyPolicy = ptr.To(v1.IPFamilyPolicyRequireDualStack)
							}
							svc, err = f.ClientSet.CoreV1().Services(f.Namespace.Name).Create(context.TODO(), svc, metav1.CreateOptions{})
							if err != nil {
								framework.Logf("creating service failed, err: %v", err)
							}
							return err
						}, 60*time.Second, 1*time.Second).Should(gomega.Succeed())

						ginkgo.By("Waiting for endpoint to be created")
						err = e2eendpointslice.WaitForEndpointPods(context.TODO(), f.ClientSet, f.Namespace.Name, echoServiceName, serverPod.Name)
						framework.ExpectNoError(err, "failed to wait for endpoint pod %s", serverPod.Name)
					})

					// Run queries against the service both with a small (10 bytes + overhead for echo service) and
					// a large (1420 bytes + overhead for echo service) payload.
					// The payload is transmitted to and echoed from the echo service for both HTTP and UDP tests.
					ginkgo.When("tests are run towards the agnhost echo service", func() {

						ginkgo.It("queries to the nodePort service shall work for TCP", func() {
							tcpNodePort, _ := nodePortsFromService(svc)
							packetSizes := []string{"small", "large"}
							if isLocalGWModeEnabled() && hostNetwork {
								// if local gateway mode the intermediary node will attempt to fragment the packet, if the DF
								// bit is not set. However, the decision on setting DF bit is left up to the kernel, and
								// is unpredictable. If the DF bit is set, the nftables rule that DNATs nodeport -> cluster IP
								// will then attempt to route the packet, and hit our 1400 byte MTU route. This will cause:
								// 172.18.0.2:37755->10.96.141.254:9881(udp) sk_skb_reason_drop(SKB_DROP_REASON_PKT_TOO_BIG)
								packetSizes = []string{"small"}
							}
							for _, size := range packetSizes {
								for _, serviceNodeIP := range serviceNodeInternalIPs {
									serviceNodeIP := serviceNodeIP
									if utilnet.IsIPv6String(serviceNodeIP) {
										serviceNodeIP = fmt.Sprintf("[%s]", serviceNodeIP)
									}
									ginkgo.By(fmt.Sprintf("Sending TCP %s payload to service IP %s "+
										"and expecting to receive the same payload", size, serviceNodeIP))
									cmd := fmt.Sprintf("curl --max-time 10 -g -q -s http://%s:%d/echo?msg=%s",
										serviceNodeIP,
										tcpNodePort,
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
							packetSizes := []string{"small", "large"}
							// If gateway mode is shared, and endpoint is OVN networked, host networked originated packets
							// exceeding pod MTU will not be delivered. This is because ICMP needs frag will be sent back to the original
							// Kubernetes node by OVN (even if DF bit is not set) and the node will refuse to install an MTU cache route.
							// To fix this later we can install ip rules that match on nodeport and lower the MTU from the originator
							// but for now we consider nodeport access from a k8s node as not a practical use case. See
							// https://issues.redhat.com/browse/OCPBUGS-7609
							// Furthermore, in local gateway mode, if the DF bit was not set the packet will go into the host of
							// intermediary node, where nodeport will be DNAT'ed to cluster IP service, and then hit the MTU 1400 route.
							// Netcat will not set Don't Fragment (DF) bit, so packet will be fragmented at intermediary
							// node and sent to server. However, it is up to the kernel to decide whether to set the DF bit,
							// and it is not predictable. Therefore, we have to skip large packet size for local gateway mode
							// as well. This is true when the endpoint is host or ovn networked, because the route for the cluster
							// cidr service is set to 1400, which causes:
							// 172.18.0.2:37755->10.96.141.254:9881(udp) sk_skb_reason_drop(SKB_DROP_REASON_PKT_TOO_BIG)
							if !hostNetwork || isLocalGWModeEnabled() {
								packetSizes = []string{"small"}
							}

							for _, size := range packetSizes {
								for _, serviceNodeIP := range serviceNodeInternalIPs {
									flushCmd := "ip route flush cache"
									if utilnet.IsIPv6String(serviceNodeIP) {
										flushCmd = "ip -6 route flush cache"
									}
									if size == "large" {
										// Flushing the IP route cache will remove any routes in the cache
										// that are a result of receiving a "need to frag" packet.
										ginkgo.By("Flushing the ip route cache")
										_, err := e2epodoutput.RunHostCmdWithRetries(
											clientPod.Namespace,
											clientPod.Name,
											flushCmd,
											framework.Poll,
											60*time.Second)
										framework.ExpectNoError(err, "Flushing the ip route cache failed")

										// List the current IP route cache for informative purposes.
										cmd := fmt.Sprintf("ip route get %s", serviceNodeIP)
										stdout, err := e2epodoutput.RunHostCmd(
											clientPod.Namespace,
											clientPod.Name,
											cmd)
										framework.ExpectNoError(err, "Listing IP route cache")
										framework.Logf("%s: %s", cmd, stdout)
									}
									// We expect the following to fail at least once for large payloads and non-hostNetwork
									// endpoints: the first request will fail as we have to receive a "need to frag" ICMP
									// message, subsequent requests then should succeed.
									_, udpNodePort := nodePortsFromService(svc)
									gomega.Eventually(func() error {
										ginkgo.By(fmt.Sprintf("Sending UDP %s payload to service IP %s "+
											"and expecting to receive the same payload", size, serviceNodeIP))
										// Send payload via UDP.
										cmd := fmt.Sprintf("echo 'echo %s' | nc -w2 -u %s %d",
											echoPayloads[size],
											serviceNodeIP,
											udpNodePort,
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
											return fmt.Errorf("stdout does not match payloads: stdout:%q != payloads[%s]:%q", stdout, size, echoPayloads[size])
										}
										// fc00:f853:ccd:e793::3 from :: dev breth0 src fc00:f853:ccd:e793::4 metric 256 expires 537sec mtu 1400 pref medium
										// for IPV6 the regex changes a bit
										if IsIPv6Cluster(f.ClientSet) {
											echoMtuRegex = regexp.MustCompile(`expires.*mtu.*`)
										}

										if size == "large" {
											cmd = fmt.Sprintf("ip route get %s", serviceNodeIP)
											stdout, err = e2epodoutput.RunHostCmd(
												clientPod.Namespace,
												clientPod.Name,
												cmd)
											if err != nil {
												return fmt.Errorf("could not list IP route cache, err: %q", err)
											}
											ginkgo.By("Making sure that the ip route cache does NOT contain an MTU route")
											if echoMtuRegex.Match([]byte(stdout)) {
												framework.Failf("found unexpected MTU cache route: %s", stdout)
											}
										}
										return nil
									}, 60*time.Second, 1*time.Second).Should(gomega.Succeed())
									// Flushing the IP route cache will remove any routes in the cache
									// that are a result of receiving a "need to frag" packet. Let's
									// flush this on all 3 nodes else we will run into the
									// bug: https://issues.redhat.com/browse/OCPBUGS-7609.
									// TODO: Revisit this once https://bugzilla.redhat.com/show_bug.cgi?id=2169839 is fixed.
									ovnKubernetesNamespace := deploymentconfig.Get().OVNKubernetesNamespace()
									ovnKubeNodePods, err := f.ClientSet.CoreV1().Pods(ovnKubernetesNamespace).List(context.TODO(), metav1.ListOptions{
										LabelSelector: "app=ovnkube-node",
									})
									if err != nil {
										framework.Failf("could not get ovnkube-node pods: %v", err)
									}
									for _, ovnKubeNodePod := range ovnKubeNodePods.Items {
										framework.Logf("Flushing the ip route cache on %s", ovnKubeNodePod.Name)
										_, err := e2ekubectl.RunKubectl(ovnKubernetesNamespace, "exec", ovnKubeNodePod.Name, "--container", getNodeContainerName(), "--",
											"ip", "route", "flush", "cache")
										framework.ExpectNoError(err, "Flushing the ip route cache failed")
									}
								}
							}
						})
					})
				})
			}
		}
	}

	ginkgo.It("does not use host masquerade address as source IP address when communicating externally", func() {
		const (
			v6ExternAddr = "2001:db8:3333:4444:CCCC:DDDD:EEEE:FFFF"
			v4ExternAddr = "8.8.8.8"
			hostMasqIPv4 = "169.254.0.2"
			hostMasqIPv6 = "fd69::2"
		)
		nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), cs, e2eservice.MaxNodesForEndpointsTests)
		framework.ExpectNoError(err)
		gomega.Expect(len(nodes.Items)).To(gomega.BeNumerically(">", 0))
		v4NodeAddrs := e2enode.FirstAddressByTypeAndFamily(nodes, v1.NodeInternalIP, v1.IPv4Protocol)
		v6NodeAddrs := e2enode.FirstAddressByTypeAndFamily(nodes, v1.NodeInternalIP, v1.IPv6Protocol)
		if v4NodeAddrs == "" && v6NodeAddrs == "" {
			framework.Failf("unable to detect if cluster supports IPv4 or IPv6")
		}
		nodeName := nodes.Items[0].Name
		getIPRouteGetOutput := func(dst string) string {
			cmd := []string{"ip"}
			if utilnet.IsIPv6String(dst) {
				cmd = append(cmd, "-6")
			}
			cmd = append(cmd, "route", "get", dst)
			output, err := infraprovider.Get().ExecK8NodeCommand(nodeName, cmd)
			framework.ExpectNoError(err, fmt.Sprintf("failed to exec '%v': %v", cmd, err))
			return output
		}
		isHostMasqSrcIP := func(dst, masqIP string) bool {
			output := getIPRouteGetOutput(dst)
			// if its not included in the output of ip route get, its sufficient to say, its not being used as src ip
			if strings.Contains(output, masqIP) {
				return true
			}
			return false
		}
		explain := "host masquerade IP incorrectly used as source IP for external communication"
		if v4NodeAddrs != "" { // v4 enabled
			gomega.Expect(isHostMasqSrcIP(v4ExternAddr, hostMasqIPv4)).Should(gomega.BeFalse(), explain)
		}
		if v6NodeAddrs != "" { // v6 enabled
			gomega.Expect(isHostMasqSrcIP(v6ExternAddr, hostMasqIPv6)).Should(gomega.BeFalse(), explain)
		}
	})

	// This test checks a special case: we add another IP address on the node *and* manually set that
	// IP address in to endpoints. It is used for some special apiserver hacks by remote cluster people.
	// So, ensure that it works for pod -> service and host -> service traffic
	ginkgo.It("All service features work when manually listening on a non-default address", func() {
		namespace := f.Namespace.Name
		jig := e2eservice.NewTestJig(cs, namespace, serviceName)
		nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), cs, e2eservice.MaxNodesForEndpointsTests)
		framework.ExpectNoError(err)
		node := nodes.Items[0]
		nodeName := node.Name
		pods, err := cs.CoreV1().Pods(deploymentconfig.Get().OVNKubernetesNamespace()).List(context.TODO(), metav1.ListOptions{
			LabelSelector: "app=ovnkube-node",
			FieldSelector: "spec.nodeName=" + nodeName,
		})
		framework.ExpectNoError(err)
		gomega.Expect(pods.Items).To(gomega.HaveLen(1))
		clientPod := &pods.Items[0]

		ginkgo.By("Using node" + nodeName + " and pod " + clientPod.Name)

		ginkgo.By("Creating an empty ClusterIP service")
		udpHostNsPort := infraprovider.Get().GetK8HostPort()
		service, err := jig.CreateUDPService(context.TODO(), func(s *v1.Service) {
			s.Spec.Ports = []v1.ServicePort{
				{
					Name:       "udp",
					Protocol:   v1.ProtocolUDP,
					Port:       80,
					TargetPort: intstr.FromInt(int(udpHostNsPort)),
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
		_, err = e2epodoutput.RunHostCmdWithRetries(clientPod.Namespace, clientPod.Name, cmd, framework.Poll, 30*time.Second)
		framework.ExpectNoError(err)
		cleanupFn = func() {
			// initial pod used for host command may be deleted at this point, refetch
			pods, err := cs.CoreV1().Pods(deploymentconfig.Get().OVNKubernetesNamespace()).List(context.TODO(), metav1.ListOptions{
				LabelSelector: "app=ovnkube-node",
				FieldSelector: "spec.nodeName=" + nodeName,
			})
			framework.ExpectNoError(err)
			gomega.Expect(pods.Items).To(gomega.HaveLen(1))
			clientPod := &pods.Items[0]
			cmd := fmt.Sprintf(`ip addr del %s dev lo || true`, extraCIDR)
			_, _ = e2epodoutput.RunHostCmdWithRetries(clientPod.Namespace, clientPod.Name, cmd, framework.Poll, 30*time.Second)
		}

		ginkgo.By("Starting a UDP server listening on the additional IP")
		// now that 2.2.2.2 exists on the node's lo interface, let's start a server listening on it
		// we use UDP here since agnhost lets us pick the listen address only for UDP
		httpPort := infraprovider.Get().GetK8HostPort()
		serverPod := e2epod.NewAgnhostPod(namespace, "backend", nil, nil, []v1.ContainerPort{{ContainerPort: int32(udpHostNsPort)}, {ContainerPort: int32(udpHostNsPort), Protocol: "UDP"}},
			"netexec", fmt.Sprintf("--http-port=%d", httpPort), "--udp-port="+fmt.Sprintf("%d", udpHostNsPort), "--udp-listen-addresses="+extraIP)
		serverPod.Labels = jig.Labels
		serverPod.Spec.NodeName = nodeName
		serverPod.Spec.HostNetwork = true
		serverPod.Spec.Containers[0].TerminationMessagePolicy = v1.TerminationMessageFallbackToLogsOnError
		e2epod.NewPodClient(f).CreateSync(context.TODO(), serverPod)

		ginkgo.By("Ensuring the server is listening on the additional IP")
		// Connect from host -> additional IP. This shouldn't touch OVN at all, just acting as a basic
		// sanity check that we're actually listening on this IP
		err = wait.PollImmediate(framework.Poll, 30*time.Second, func() (bool, error) {
			response, err := tryPokeEndpointViaNode(nodeName, "udp", extraIP, uint16(httpPort), uint16(udpHostNsPort), "hostname")
			if err != nil {
				return false, nil
			}
			return hostnameMatchesNode(response, nodeName), nil
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
								Port:     int32(udpHostNsPort),
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
			response, err := tryPokeEndpointViaNode(nodeName, "udp", service.Spec.ClusterIP, uint16(httpPort), 80, "hostname")
			if err != nil {
				return false, nil
			}
			return hostnameMatchesNode(response, nodeName), nil
		})
		framework.ExpectNoError(err)

		ginkgo.By("Confirming that the service is accessible from the node's pod network")
		// Now, spin up a pod-network pod on the same node, and ensure we can talk to the "local address" service
		clientServerPod := e2epod.NewAgnhostPod(namespace, "client", nil, nil, []v1.ContainerPort{{ContainerPort: 12345}, {ContainerPort: 12345, Protocol: "UDP"}},
			"netexec")
		clientServerPod.Spec.NodeName = nodeName
		e2epod.NewPodClient(f).CreateSync(context.TODO(), clientServerPod)
		clientServerPod, err = e2epod.NewPodClient(f).Get(context.TODO(), clientServerPod.Name, metav1.GetOptions{})
		framework.ExpectNoError(err)

		// annoying: need to issue a curl to the test pod to tell it to connect to the service
		err = wait.PollImmediate(framework.Poll, 30*time.Second, func() (bool, error) {
			cmd = fmt.Sprintf("curl -g -q -s 'http://%s/dial?request=%s&protocol=%s&host=%s&port=%d&tries=1'",
				net.JoinHostPort(clientServerPod.Status.PodIP, "8080"),
				"hostname",
				"udp",
				service.Spec.ClusterIP,
				80)
			stdout, err := e2epodoutput.RunHostCmdWithRetries(clientPod.Namespace, clientPod.Name, cmd, framework.Poll, 30*time.Second)
			if err != nil {
				return false, err
			}
			return stdout == fmt.Sprintf(`{"responses":["%s"]}`, nodeName) ||
				stdout == fmt.Sprintf(`{"responses":["%s"]}`, strings.Split(nodeName, ".")[0]), nil
		})
		framework.ExpectNoError(err)
	})

	ginkgo.Context("of type NodePort", func() {
		var nodes *v1.NodeList
		var err error
		nodeIPs := make(map[string]map[int]string)
		var egressNode string
		var providerCtx infraapi.Context
		var isDualStack bool

		const (
			endpointHTTPPort             = 80
			endpointUDPPort              = 90
			clusterHTTPPort              = 81
			clusterUDPPort               = 91
			clientContainerName          = "npclient"
			targetSecondaryContainerName = "egressSecondaryTargetNode-allowed"
		)

		ginkgo.BeforeEach(func() {
			providerCtx = infraprovider.Get().NewTestContext()
			nodeIPs = make(map[string]map[int]string)
			egressNode = ""
		})

		ginkgo.AfterEach(func() {
			ginkgo.By("Deleting additional IP addresses from nodes")
			for nodeName, ipFamilies := range nodeIPs {
				for _, ip := range ipFamilies {
					subnetMask := "/32"
					if utilnet.IsIPv6String(ip) {
						subnetMask = "/128"
					}
					cidr := ip + subnetMask
					_, err := infraprovider.Get().ExecK8NodeCommand(nodeName, []string{"ip", "addr", "delete",
						cidr, "dev", deploymentconfig.Get().ExternalBridgeName()})
					if err != nil && !(strings.Contains(err.Error(),
						"RTNETLINK answers: Cannot assign requested address") || strings.Contains(err.Error(), "Address not found")) {
						framework.Failf("failed to remove ip address %s from node %s, err: %q", ip, nodeName, err)
					}
				}
			}
			ginkgo.By("Deleting EgressIP Setup if any")
			e2ekubectl.RunKubectlOrDie("default", "delete", "eip", "egressip", "--ignore-not-found=true")
			if egressNode != "" {
				e2ekubectl.RunKubectlOrDie("default", "label", "node", egressNode, "k8s.ovn.org/egress-assignable-")
			}
			// network is removed by provider Context API
		})

		ginkgo.It("should be able to preserve UDP traffic when server pod cycles for a NodePort service via a different node", func(ctx context.Context) {
			const (
				serviceName = "svc-udp"
				srcPort     = 12345
				podClient   = "pod-client"
				podBackend1 = "pod-server-1"
				podBackend2 = "pod-server-2"
			)
			var clientNodeInfo, serverNodeInfo, backendNodeInfo nodeInfo

			cs := f.ClientSet
			ns := f.Namespace.Name

			nodes, err := e2enode.GetBoundedReadySchedulableNodes(ctx, cs, 3)
			framework.ExpectNoError(err)
			if len(nodes.Items) < 3 {
				e2eskipper.Skipf(
					"Test requires >= 3 Ready nodes, but there are only %v nodes",
					len(nodes.Items))
			}

			family := v1.IPv4Protocol
			if IsIPv6Cluster(cs) {
				family = v1.IPv6Protocol
			}

			ips := e2enode.GetAddressesByTypeAndFamily(&nodes.Items[0], v1.NodeInternalIP, family)
			gomega.Expect(ips).ToNot(gomega.BeEmpty())

			clientNodeInfo = nodeInfo{
				name:   nodes.Items[0].Name,
				nodeIP: ips[0],
			}

			ips = e2enode.GetAddressesByTypeAndFamily(&nodes.Items[1], v1.NodeInternalIP, family)
			gomega.Expect(ips).ToNot(gomega.BeEmpty())

			backendNodeInfo = nodeInfo{
				name:   nodes.Items[1].Name,
				nodeIP: ips[0],
			}

			ips = e2enode.GetAddressesByTypeAndFamily(&nodes.Items[2], v1.NodeInternalIP, family)
			gomega.Expect(ips).ToNot(gomega.BeEmpty())

			serverNodeInfo = nodeInfo{
				name:   nodes.Items[2].Name,
				nodeIP: ips[0],
			}

			// Create a NodePort service
			udpJig := e2eservice.NewTestJig(cs, ns, serviceName)
			ginkgo.By("creating a UDP service " + serviceName + " with type=NodePort in " + ns)
			udpService, err := udpJig.CreateUDPService(ctx, func(svc *v1.Service) {
				svc.Spec.Type = v1.ServiceTypeNodePort
				svc.Spec.Ports = []v1.ServicePort{
					{Port: 80, Name: "udp", Protocol: v1.ProtocolUDP, TargetPort: intstr.FromInt32(80)},
				}
			})
			framework.ExpectNoError(err)

			// Create a pod in one node to create the UDP traffic against the NodePort service every 5 seconds
			ginkgo.By("creating a client pod for probing the service " + serviceName)
			clientPod := e2epod.NewAgnhostPod(ns, podClient, nil, nil, nil)
			nodeSelection := e2epod.NodeSelection{Name: clientNodeInfo.name}
			e2epod.SetNodeSelection(&clientPod.Spec, nodeSelection)
			cmd := fmt.Sprintf(`date; for i in $(seq 1 3000); do echo "$(date) Try: ${i}"; echo hostname | nc -u -w 5 -p %d %s %d; echo; done`, srcPort, serverNodeInfo.nodeIP, udpService.Spec.Ports[0].NodePort)
			clientPod.Spec.Containers[0].Command = []string{"/bin/sh", "-c", cmd}
			clientPod.Spec.Containers[0].Name = podClient
			e2epod.NewPodClient(f).CreateSync(ctx, clientPod)

			// Read the client pod logs
			logs, err := e2epod.GetPodLogs(ctx, cs, ns, podClient, podClient)
			framework.ExpectNoError(err)
			framework.Logf("Pod client logs: %s", logs)

			// Add a backend pod to the service in the other node
			ginkgo.By("creating a backend pod " + podBackend1 + " for the service " + serviceName + " at node " + backendNodeInfo.name)
			serverPod1 := e2epod.NewAgnhostPod(ns, podBackend1, nil, nil, nil, "netexec", fmt.Sprintf("--udp-port=%d", 80))
			serverPod1.Labels = udpJig.Labels
			nodeSelection = e2epod.NodeSelection{Name: backendNodeInfo.name}
			e2epod.SetNodeSelection(&serverPod1.Spec, nodeSelection)
			e2epod.NewPodClient(f).CreateSync(ctx, serverPod1)

			ginkgo.By("Waiting for the endpoint to be ready")
			err = e2eendpointslice.WaitForEndpointCount(ctx, f.ClientSet, f.Namespace.Name,
				serviceName, 1)
			framework.ExpectNoError(err, "failed to validate endpoints for service %s in namespace: %s",
				serviceName, f.Namespace.Name)

			logContainsFn := func(text, podName string) wait.ConditionWithContextFunc {
				return func(ctx context.Context) (bool, error) {
					logs, err := e2epod.GetPodLogs(ctx, cs, ns, podName, podName)
					if err != nil {
						// Retry the error next time.
						return false, nil
					}
					if !strings.Contains(string(logs), text) {
						return false, nil
					}
					return true, nil
				}
			}
			// Note that the fact that Endpoints object already exists, does NOT mean
			// that openflows were already programmed.
			// Additionally take into account that UDP conntract entries timeout is
			// 30 seconds by default.
			// Based on the above check if the pod receives the traffic.
			ginkgo.By("checking client pod connected to the backend 1 on Node IP " + serverNodeInfo.nodeIP)
			if err := wait.PollUntilContextTimeout(ctx, 5*time.Second, time.Minute, true, logContainsFn(podBackend1, podClient)); err != nil {
				logs, err = e2epod.GetPodLogs(ctx, cs, ns, podClient, podClient)
				framework.ExpectNoError(err)
				framework.Logf("Pod client logs: %s", logs)
				framework.Failf("Failed to connect to backend 1")
			}

			// Create a second pod
			ginkgo.By("creating a second backend pod " + podBackend2 + " for the service " + serviceName + " at node " + backendNodeInfo.name)
			serverPod2 := e2epod.NewAgnhostPod(ns, podBackend2, nil, nil, nil, "netexec", fmt.Sprintf("--udp-port=%d", 80))
			serverPod2.Labels = udpJig.Labels
			nodeSelection = e2epod.NodeSelection{Name: backendNodeInfo.name}
			e2epod.SetNodeSelection(&serverPod2.Spec, nodeSelection)
			e2epod.NewPodClient(f).CreateSync(ctx, serverPod2)

			// and delete the first pod
			framework.Logf("Cleaning up %s pod", podBackend1)
			e2epod.NewPodClient(f).DeleteSync(ctx, podBackend1, metav1.DeleteOptions{}, e2epod.DefaultPodDeletionTimeout)

			ginkgo.By("Waiting for the endpoint to be ready")
			err = e2eendpointslice.WaitForEndpointCount(ctx, f.ClientSet, f.Namespace.Name,
				serviceName, 1)
			framework.ExpectNoError(err, "failed to validate endpoints for service %s in namespace: %s",
				serviceName, f.Namespace.Name)

			// Check that the second pod keeps receiving traffic
			// UDP conntrack entries timeout is 30 sec by default
			ginkgo.By("checking client pod connected to the backend 2 on Node IP " + serverNodeInfo.nodeIP)
			if err := wait.PollUntilContextTimeout(ctx, 5*time.Second, time.Minute, true, logContainsFn(podBackend2, podClient)); err != nil {
				logs, err = e2epod.GetPodLogs(ctx, cs, ns, podClient, podClient)
				framework.ExpectNoError(err)
				framework.Logf("Pod client logs: %s", logs)
				framework.Failf("Failed to connect to backend 2")
			}
		})

		ginkgo.It("should listen on each host addresses", func() {
			endPoints := make([]*v1.Pod, 0)
			endpointsSelector := map[string]string{"servicebackend": "true"}
			nodesHostnames := sets.New[string]()
			nodes, err = e2enode.GetBoundedReadySchedulableNodes(context.TODO(), f.ClientSet, 3)
			framework.ExpectNoError(err)

			if len(nodes.Items) < 3 {
				e2eskipper.Skipf(
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
			primaryProviderNetwork, err := infraprovider.Get().PrimaryNetwork()
			framework.ExpectNoError(err, "failed to get primary network")
			externalContainerPort := infraprovider.Get().GetExternalContainerPort()
			externalContainer := infraapi.ExternalContainer{Name: clientContainerName, Image: images.AgnHost(), Network: primaryProviderNetwork,
				CmdArgs: getAgnHostHTTPPortBindCMDArgs(externalContainerPort), ExtPort: externalContainerPort}
			externalContainer, err = providerCtx.CreateExternalContainer(externalContainer)
			framework.ExpectNoError(err, "external container %s must be created", externalContainer.Name)

			// If `xgw` exists, connect client container to it
			exGwNetwork, err := infraprovider.Get().GetNetwork("xgw")
			if err == nil {
				_, _ = providerCtx.AttachNetwork(exGwNetwork, externalContainer.Name)
			}

			ginkgo.By("Selecting additional IP addresses for each node")
			// add new secondary IP from node subnet to all nodes, if the cluster is v6 add an ipv6 address
			toCurlAddresses := sets.New[string]()
			primaryIPv4Subnet, ipv6, err := primaryProviderNetwork.IPv4IPv6Subnets()
			framework.ExpectNoError(err, "must get primary provider network subnets")
			primaryNetworkSubnet := primaryIPv4Subnet
			if IsIPv6Cluster(f.ClientSet) {
				primaryNetworkSubnet = ipv6
			}
			_, primaryNetworkNet, err := net.ParseCIDR(primaryNetworkSubnet)
			framework.ExpectNoError(err, "failed to parse primary subnet %q", primaryNetworkSubnet)
			for _, node := range nodes.Items {
				addrAnnotation, ok := node.Annotations["k8s.ovn.org/host-cidrs"]
				gomega.Expect(ok).To(gomega.BeTrue())

				var addrs []string
				var addrsFiltered []string
				err := json.Unmarshal([]byte(addrAnnotation), &addrs)
				framework.ExpectNoError(err, "failed to parse node[%s] host-address annotation[%s]", node.Name,
					addrAnnotation)
				for _, addr := range addrs {
					addrSplit := strings.Split(addr, "/")
					gomega.Expect(addrSplit).Should(gomega.HaveLen(2))
					ip := net.ParseIP(addrSplit[0])
					if !primaryNetworkNet.Contains(ip) {
						framework.Logf("skipping IP %s because its outside primary provider network %s", ip, primaryNetworkSubnet)
						continue
					}
					addrsFiltered = append(addrsFiltered, ip.String())
				}
				toCurlAddresses.Insert(addrsFiltered...)

				// Calculate and store for AfterEach new target IP addresses.
				var newIP string
				if nodeIPs[node.Name] == nil {
					nodeIPs[node.Name] = make(map[int]string)
				}
				if IsIPv6Cluster(f.ClientSet) {
					newIPIP, err := ipalloc.NewPrimaryIPv6()
					framework.ExpectNoError(err, "must get new primary provider IPv4")
					newIP = newIPIP.String()
					nodeIPs[node.Name][6] = newIP
				} else {
					newIPIP, err := ipalloc.NewPrimaryIPv4()
					framework.ExpectNoError(err, "must get new primary provider IPv4")
					nodeIPs[node.Name][4] = newIPIP.String()
				}
			}
			iproute2Proto := "-4"
			if IsIPv6Cluster(f.ClientSet) {
				iproute2Proto = "-6"
			}
			ginkgo.By("Adding additional IP addresses to each node")
			for nodeName, ipFamilies := range nodeIPs {
				for _, ip := range ipFamilies {
					cidr := ip + util.GetIPFullMaskString(ip)
					// manually add the a secondary IP to each node
					framework.Logf("adding IP %q to Node %s", ip, nodeName)
					_, err = infraprovider.Get().ExecK8NodeCommand(nodeName, []string{"ip", iproute2Proto, "addr", "add", cidr, "dev", deploymentconfig.Get().ExternalBridgeName()})
					if err != nil {
						framework.Failf("failed to add new IP address %s to node %s: %v", ip, nodeName, err)
					}
					toCurlAddresses.Insert(ip)
				}
			}

			isIPv6Cluster := IsIPv6Cluster(f.ClientSet)

			ginkgo.By("Creating NodePort services")

			etpLocalServiceName := "etp-local-svc"
			etpLocalSvc := nodePortServiceSpecFrom(etpLocalServiceName, v1.IPFamilyPolicyPreferDualStack,
				endpointHTTPPort, endpointUDPPort, clusterHTTPPort, clusterUDPPort, endpointsSelector,
				v1.ServiceExternalTrafficPolicyTypeLocal)
			etpLocalSvc, err = f.ClientSet.CoreV1().Services(f.Namespace.Name).Create(context.Background(), etpLocalSvc,
				metav1.CreateOptions{})
			framework.ExpectNoError(err)

			etpClusterServiceName := "etp-cluster-svc"
			etpClusterSvc := nodePortServiceSpecFrom(etpClusterServiceName, v1.IPFamilyPolicyPreferDualStack,
				endpointHTTPPort, endpointUDPPort, clusterHTTPPort, clusterUDPPort, endpointsSelector,
				v1.ServiceExternalTrafficPolicyTypeCluster)
			etpClusterSvc, err = f.ClientSet.CoreV1().Services(f.Namespace.Name).Create(context.Background(),
				etpClusterSvc, metav1.CreateOptions{})
			framework.ExpectNoError(err)

			ginkgo.By("Waiting for the endpoints to pop up")
			var expectedEndpointsNum int
			if isDualStackCluster(nodes) {
				expectedEndpointsNum = len(endPoints) * 2
			} else {
				expectedEndpointsNum = len(endPoints)
			}

			err = e2eendpointslice.WaitForEndpointCount(context.TODO(), f.ClientSet, f.Namespace.Name,
				etpLocalServiceName, expectedEndpointsNum)
			framework.ExpectNoError(err, "failed to validate endpoints for service %s in namespace: %s",
				etpLocalServiceName, f.Namespace.Name)

			err = e2eendpointslice.WaitForEndpointCount(context.TODO(), f.ClientSet, f.Namespace.Name,
				etpClusterServiceName, expectedEndpointsNum)
			framework.ExpectNoError(err, "failed to validate endpoints for service %s in namespace: %s",
				etpClusterServiceName, f.Namespace.Name)

			for _, serviceSpec := range []*v1.Service{etpLocalSvc, etpClusterSvc} {
				tcpNodePort, udpNodePort := nodePortsFromService(serviceSpec)

				for _, protocol := range []string{"http", "udp"} {
					toCurlPort := int32(tcpNodePort)
					if protocol == "udp" {
						toCurlPort = int32(udpNodePort)
					}

					for _, address := range sets.List(toCurlAddresses) {
						if !isIPv6Cluster && utilnet.IsIPv6String(address) {
							continue
						}

						ginkgo.By("Hitting service " + serviceSpec.Name + " on " + address + " via " + protocol)
						gomega.Eventually(func() bool {
							epHostname := pokeEndpointViaExternalContainer(externalContainer, protocol, address, toCurlPort,
								"hostname")
							// Expect to receive a valid hostname
							return nodesHostnames.Has(epHostname)
						}, "40s", "1s").Should(gomega.BeTrue())
					}
				}
			}
		})

		ginkgo.It("should work on secondary node interfaces for ETP=local and ETP=cluster when backend pods are also served by EgressIP", func() {
			endPoints := make([]*v1.Pod, 0)
			endpointsSelector := map[string]string{"servicebackend": "true"}
			nodesHostnames := sets.New[string]()
			isIPv6Cluster := IsIPv6Cluster(f.ClientSet)

			nodes, err = e2enode.GetBoundedReadySchedulableNodes(context.TODO(), f.ClientSet, 3)
			framework.ExpectNoError(err)

			if len(nodes.Items) < 3 {
				e2eskipper.Skipf(
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

			ginkgo.By("Choosing egressIP pod")
			egressPod := endPoints[0]
			framework.Logf("EgressIP pod is %s/%s", endPoints[0].Namespace, endPoints[0].Name)

			ginkgo.By("Label egress node" + egressNode + " create external container to send egress traffic to via secondary MultiNIC EIP")
			egressNode = egressPod.Spec.NodeName
			e2enode.AddOrUpdateLabelOnNode(f.ClientSet, egressNode, "k8s.ovn.org/egress-assignable", "dummy")
			ginkgo.By("creating secondary network and attaching to all Nodes and external container")
			// configure and add additional network to worker containers for EIP multi NIC feature
			subnet := secondaryIPV4Subnet
			if isIPv6Cluster {
				subnet = secondaryIPV6Subnet
			}
			secondaryProviderNetwork, err := providerCtx.CreateNetwork(secondaryNetworkName, subnet)
			framework.ExpectNoError(err, "creation of network %s must be available to attach containers", secondaryNetworkName)
			nodes, err = f.ClientSet.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
			framework.ExpectNoError(err, "must list all Nodes")
			for _, node := range nodes.Items {
				_, err = providerCtx.AttachNetwork(secondaryProviderNetwork, node.Name)
				if err != nil {
					e2eskipper.Skipf("Test requires nodes that support attaching provider networks; network %s could not attach to node %s: %v", secondaryProviderNetwork.Name(), node.Name, err)
				}
			}
			serverExternalContainerPort := infraprovider.Get().GetExternalContainerPort()
			serverExternalContainerSpec := infraapi.ExternalContainer{
				Name:    targetSecondaryContainerName,
				Image:   images.AgnHost(),
				Network: secondaryProviderNetwork,
				CmdArgs: getAgnHostHTTPPortBindCMDArgs(serverExternalContainerPort),
				ExtPort: serverExternalContainerPort,
			}
			serverExternalContainer, err := providerCtx.CreateExternalContainer(serverExternalContainerSpec)
			framework.ExpectNoError(err, "creation of external container %s attached to network must succeed",
				serverExternalContainerSpec.Name, serverExternalContainer.Network.Name())
			var serverExternalContainerIP string
			if isIPv6Cluster {
				serverExternalContainerIP = serverExternalContainer.GetIPv6()
			} else {
				serverExternalContainerIP = serverExternalContainer.GetIPv4()
			}
			ginkgo.By("Create an EgressIP object with one secondary multi NIC egress IP defined")
			egressIP := "10.10.10.105" // secondary subnet as defined in EIP test suite
			if isIPv6Cluster {
				egressIP = "2001:db8:abcd:1234:c001::" // secondary subnet as defined in EIP test suite
			}

			var egressIPConfig = `apiVersion: k8s.ovn.org/v1
kind: EgressIP
metadata:
    name: ` + "egressip" + `
spec:
    egressIPs:
    - "` + egressIP + `"
    namespaceSelector:
        matchLabels:
            kubernetes.io/metadata.name: ` + f.Namespace.Name + `
`
			if err := os.WriteFile("egressip.yaml", []byte(egressIPConfig), 0644); err != nil {
				framework.Failf("Unable to write CRD config to disk: %v", err)
			}
			ginkgo.DeferCleanup(func() error {
				e2ekubectl.RunKubectlOrDie("default", "delete", "-f", "egressip.yaml", "--ignore-not-found=true")
				if err := os.Remove("egressip.yaml"); err != nil {
					return fmt.Errorf("unable to remove the CRD config from disk: %v", err)
				}
				return nil
			})

			framework.Logf("Create the EgressIP configuration")
			e2ekubectl.RunKubectlOrDie("default", "create", "-f", "egressip.yaml")

			ginkgo.By("Check that the status is of length one and that it is assigned to " + egressNode)
			err = wait.PollImmediate(retryInterval, retryTimeout, func() (bool, error) {
				egressIP := egressIPs{}
				egressIPStdout, err := e2ekubectl.RunKubectl("default", "get", "eip", "-o", "json")
				if err != nil {
					framework.Logf("Error: failed to get the EgressIP object, err: %v", err)
					return false, nil
				}
				json.Unmarshal([]byte(egressIPStdout), &egressIP)
				if len(egressIP.Items) > 1 {
					framework.Failf("Didn't expect to retrieve more than one egress IP during the execution of this test, saw: %v", len(egressIP.Items))
				}
				if len(egressIP.Items[0].Status.Items) == 0 {
					framework.Logf("EgressIP %s is not assigned to node %s yet", egressIP, egressNode)
					return false, nil
				}
				return egressIP.Items[0].Status.Items[0].Node == egressNode, nil
			})
			if err != nil {
				framework.Failf("Error: expected to have 1 egress IP assignment")
			}

			// FIXME: implement feature to connect networks
			// FIXME: tests shouldnt depend on external resources precreated and instead manage those resources within
			// the lifecycle of the test.
			// If `xgw` exists, connect client container to it
			exGWNetwork, err := infraprovider.Get().GetNetwork("xgw")
			if err == nil {
				_, _ = providerCtx.AttachNetwork(exGWNetwork, serverExternalContainer.GetName())
			}

			ginkgo.By("Selecting additional IP addresses for each node")
			// add new secondary IP from node subnet to all nodes, if the cluster is v6 add an ipv6 address
			secondaryNetworkV4Subnet, secondaryNetworkV6Subnet, err := secondaryProviderNetwork.IPv4IPv6Subnets()
			framework.ExpectNoError(err, "must get secondary provider network subnets")
			secondarySubnetStr := secondaryNetworkV4Subnet
			if isIPv6Cluster {
				secondarySubnetStr = secondaryNetworkV6Subnet
			}
			_, secondarySubnet, err := net.ParseCIDR(secondarySubnetStr)
			framework.ExpectNoError(err, "must parse secondary subnet %q", secondarySubnetStr)
			toCurlAddressesSecondary := sets.New[string]()
			for i, node := range nodes.Items {
				addrAnnotation, ok := node.Annotations["k8s.ovn.org/host-cidrs"]
				gomega.Expect(ok).To(gomega.BeTrue())
				var allAddrs []string
				var filteredAddrs []string
				err := json.Unmarshal([]byte(addrAnnotation), &allAddrs)
				framework.ExpectNoError(err, "failed to parse node[%s] host-address annotation[%s]", node.Name,
					addrAnnotation)
				for _, addr := range allAddrs {
					addrSplit := strings.Split(addr, "/")
					gomega.Expect(addrSplit).Should(gomega.HaveLen(2))
					// ensure only IPs for the secondary subnet are added
					ip := net.ParseIP(addrSplit[0])
					gomega.Expect(ip).NotTo(gomega.BeNil())
					if !secondarySubnet.Contains(ip) {
						framework.Logf("skipping IP %s because it is not within networks subnet %s", ip, subnet)
						continue // skip if not within the networks subnet
					}
					filteredAddrs = append(filteredAddrs, ip.String())
				}
				toCurlAddressesSecondary.Insert(filteredAddrs...)

				// Calculate and store for AfterEach new target IP addresses.
				if nodeIPs[node.Name] == nil {
					nodeIPs[node.Name] = make(map[int]string)
				}

				if isIPv6Cluster {
					newIPIP := fmt.Sprintf("2001:db8:abcd:1234::%d", i+140)
					nodeIPs[node.Name][6] = newIPIP
				} else {
					newIPIP := fmt.Sprintf("10.10.10.%d", i+140)
					nodeIPs[node.Name][4] = newIPIP
				}
			}

			ginkgo.By("Adding additional IP addresses to each node")
			for nodeName, ipFamilies := range nodeIPs {
				secondaryNetworkInterface, err := infraprovider.Get().GetK8NodeNetworkInterface(nodeName, secondaryProviderNetwork)
				framework.ExpectNoError(err, "must get Node %s network interface %s", nodeName, secondaryProviderNetwork)
				gomega.Expect(secondaryNetworkInterface.InfName).NotTo(gomega.BeEmpty(), "failed to fetch interface name from a k8 node attached to a secondary network")
				for _, ip := range ipFamilies {
					cidr := ip + util.GetIPFullMaskString(ip)
					// manually add the a secondary IP to each node
					_, err = infraprovider.Get().ExecK8NodeCommand(nodeName, []string{"ip", "addr", "add", cidr, "dev", secondaryNetworkInterface.InfName})
					if err != nil {
						framework.Failf("failed to add new IP address %s to node %s: %v", ip, nodeName, err)
					}
					providerCtx.AddCleanUpFn(func() error {
						_, err = infraprovider.Get().ExecK8NodeCommand(nodeName, []string{"ip", "addr", "del", cidr, "dev", secondaryNetworkInterface.InfName})
						if err != nil {
							return fmt.Errorf("failed to del newly assigned node IP address %s to node %s: %v", ip, nodeName, err)
						}
						return nil
					})
					toCurlAddressesSecondary.Insert(ip)
				}
			}

			ginkgo.By("Creating NodePort services")

			etpLocalServiceName := "etp-local-svc"
			etpLocalSvc := nodePortServiceSpecFrom(etpLocalServiceName, v1.IPFamilyPolicyPreferDualStack,
				endpointHTTPPort, endpointUDPPort, clusterHTTPPort, clusterUDPPort, endpointsSelector,
				v1.ServiceExternalTrafficPolicyTypeLocal)
			etpLocalSvc, err = f.ClientSet.CoreV1().Services(f.Namespace.Name).Create(context.Background(), etpLocalSvc,
				metav1.CreateOptions{})
			framework.ExpectNoError(err)

			etpClusterServiceName := "etp-cluster-svc"
			etpClusterSvc := nodePortServiceSpecFrom(etpClusterServiceName, v1.IPFamilyPolicyPreferDualStack,
				endpointHTTPPort, endpointUDPPort, clusterHTTPPort, clusterUDPPort, endpointsSelector,
				v1.ServiceExternalTrafficPolicyTypeCluster)
			etpClusterSvc, err = f.ClientSet.CoreV1().Services(f.Namespace.Name).Create(context.Background(),
				etpClusterSvc, metav1.CreateOptions{})
			framework.ExpectNoError(err)

			ginkgo.By("Waiting for the endpoints to pop up")

			var expectedEndpointsNum int
			if isDualStackCluster(nodes) {
				expectedEndpointsNum = len(endPoints) * 2
			} else {
				expectedEndpointsNum = len(endPoints)
			}

			err = e2eendpointslice.WaitForEndpointCount(context.TODO(), f.ClientSet, f.Namespace.Name,
				etpLocalServiceName, expectedEndpointsNum)
			framework.ExpectNoError(err, "failed to validate endpoints for service %s in namespace: %s",
				etpLocalServiceName, f.Namespace.Name)

			err = e2eendpointslice.WaitForEndpointCount(context.TODO(), f.ClientSet, f.Namespace.Name,
				etpClusterServiceName, expectedEndpointsNum)
			framework.ExpectNoError(err, "failed to validate endpoints for service %s in namespace: %s",
				etpClusterServiceName, f.Namespace.Name)

			ginkgo.By("Checking connectivity to the external container from egressIP pod " + egressPod.Name + " and verify that the source IP is the secondary NIC egress IP")
			framework.Logf("Destination IPs for external container are ip=%v", serverExternalContainerIP)
			err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(serverExternalContainer,
				egressPod.Namespace, egressPod.Name, true, []string{egressIP}))
			framework.ExpectNoError(err, "Check connectivity from pod (%s/%s) to an external container attached to "+
				"a network that is a secondary host network and verify that the src IP is the expected egressIP %s, failed: %v",
				egressPod.Namespace, egressPod.Name, egressIP, err)

			externalSvcClientIPs := sets.New(serverExternalContainerIP)
			for _, serviceSpec := range []*v1.Service{etpLocalSvc, etpClusterSvc} {
				tcpNodePort, udpNodePort := nodePortsFromService(serviceSpec)

				for _, protocol := range []string{"http", "udp"} {
					toCurlPort := int32(tcpNodePort)
					if protocol == "udp" {
						toCurlPort = int32(udpNodePort)
					}
					for _, address := range sets.List(toCurlAddressesSecondary) {
						if !isIPv6Cluster && utilnet.IsIPv6String(address) {
							continue
						}
						ginkgo.By("Hitting service " + serviceSpec.Name + " on " + address + " via " + protocol)
						gomega.Eventually(func() bool {
							epHostname := pokeEndpointViaExternalContainer(serverExternalContainer, protocol, address, toCurlPort,
								"hostname")
							// Expect to receive a valid hostname
							return nodesHostnames.Has(epHostname)
						}, "40s", "1s").Should(gomega.BeTrue())
					}
					secondaryNetworkInterface, err := infraprovider.Get().GetK8NodeNetworkInterface(egressNode, secondaryProviderNetwork)
					framework.ExpectNoError(err, "must get secondary network interface information")
					egressNodeIP := secondaryNetworkInterface.IPv4
					if isIPv6Cluster {
						egressNodeIP = secondaryNetworkInterface.IPv6
					}
					gomega.Expect(egressNodeIP).NotTo(gomega.BeEmpty(), "secondary external IP must be available")
					framework.Logf("NodeIP of node %s is %s", egressNode, egressNodeIP)
					ginkgo.By("Hitting service nodeport " + serviceSpec.Name + " on " + egressNodeIP + " via " + protocol)
					// send ingress traffic from external container to egressNode where the pod lives
					// On secondary bridges CI lane we will also created eth1 interface on each node
					// in the cluster. In that case:
					// (1) SGW: npclient's eth1 -> node's eth1-> node's breth1 -> nftables -> DNAT to CIP ->
					//          route to breth0 -> send to OVN -> hit GR; ETP=local will not be respected
					//          in this case and its broken at the moment. (FIXME)
					// (2) LGW: npclient's eth1 -> node's eth1-> node's breth1 -> nftables -> DNAT to .3 masquerade ->
					//          route to mp0 -> send to OVN -> hit switch; ETP=local will be respected
					//          in this case and its delivered to the pod. (test works for this case)
					if !isLocalGWModeEnabled() || serviceSpec.Name != etpLocalServiceName {
						framework.Logf("Mode is shared gateway OR service is ETP=cluster, so skipping srcIP verification")
						continue // cannot verify sourceIP for ETP=local with SGW on secondary interfaces
					}

					// verify srcIP of traffic is that of the external container npclient when for nodeport service type ETP=local
					// we try to hit the backend pod which is on the egressNode
					framework.Logf("%+v", externalSvcClientIPs)
					gomega.Eventually(func() bool {
						epClientIP := pokeEndpointViaExternalContainer(serverExternalContainer, protocol, egressNodeIP, toCurlPort, "clientip") // Returns the request's IP address.
						framework.Logf("Received srcIP: %v", epClientIP)
						IP, _, err := net.SplitHostPort(epClientIP)
						if err != nil {
							return false
						}
						// Expect to receive a valid hostname
						return externalSvcClientIPs.Has(IP)
					}, "40s", "1s").Should(gomega.BeTrue())
				}
			}
		})

		// This tests specific flows required to handle IP fragments towards
		// node port services to avoid forwarding via host in SGW mode. On one
		// side, it is undesireable due to performance considerations. On the
		// other side, it could be problematic if some fragmented packets within
		// a stream are forwarded via host while other non fragmented packets of
		// that same stream are forwarded directly to OVN, as the NATing in both
		// scenarios is different such that OVN could interpret them as
		// different streams and replace what it thinks to be a conflicting port
		// with a different one, breaking the stream for the involved peers.
		ginkgo.It("should handle IP fragments", func() {
			ginkgo.By("Selecting a schedulable node")
			nodes, err = e2enode.GetBoundedReadySchedulableNodes(context.TODO(), f.ClientSet, 1)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(len(nodes.Items)).To(gomega.BeNumerically(">", 0))
			isDualStack = isDualStackCluster(nodes)
			nodeName := nodes.Items[0].Name
			primaryProviderNetwork, err := infraprovider.Get().PrimaryNetwork()
			framework.ExpectNoError(err, "failed to get primary network")
			primaryInf, err := infraprovider.Get().GetK8NodeNetworkInterface(nodeName, primaryProviderNetwork)
			framework.ExpectNoError(err, "failed to get primary interface for node %s", nodeName)
			ginkgo.By("Creating the backend pod")
			args := []string{
				"netexec",
				fmt.Sprintf("--http-port=%d", endpointHTTPPort),
				fmt.Sprintf("--udp-port=%d", endpointUDPPort),
			}
			endpointsSelector := map[string]string{"servicebackend": "true"}

			serverPodName := nodeName + "-ep"
			var serverContainerName string
			_, err = createPod(f, serverPodName, nodeName, f.Namespace.Name, []string{}, endpointsSelector,
				func(p *v1.Pod) {
					p.Spec.Containers[0].Args = args
					serverContainerName = p.Spec.Containers[0].Name
				},
			)
			framework.ExpectNoError(err)

			ginkgo.By("Creating NodePort service")
			serviceName := "service"
			service := nodePortServiceSpecFrom(
				serviceName,
				v1.IPFamilyPolicyPreferDualStack,
				endpointHTTPPort,
				endpointUDPPort,
				clusterHTTPPort,
				clusterUDPPort,
				endpointsSelector,
				v1.ServiceExternalTrafficPolicyTypeCluster,
			)
			service, err = f.ClientSet.CoreV1().Services(f.Namespace.Name).Create(context.Background(), service, metav1.CreateOptions{})
			framework.ExpectNoError(err)

			ginkgo.By("Waiting for the endpoints to pop up")
			expectedEndpointsNum := 1
			if isDualStack {
				expectedEndpointsNum = expectedEndpointsNum * 2
			}
			err = e2eendpointslice.WaitForEndpointCount(
				context.TODO(),
				f.ClientSet,
				f.Namespace.Name,
				serviceName,
				expectedEndpointsNum,
			)
			framework.ExpectNoError(err)

			ginkgo.By("Creating an external client")
			externalContainer := infraapi.ExternalContainer{Name: clientContainerName, Image: images.AgnHost(), Network: primaryProviderNetwork,
				CmdArgs: []string{"pause"}, ExtPort: infraprovider.Get().GetExternalContainerPort()}
			externalContainer, err = providerCtx.CreateExternalContainer(externalContainer)
			framework.ExpectNoError(err, "failed to create external container", externalContainer)

			clientIP := externalContainer.GetIPv4()
			serverNodeIP := primaryInf.IPv4
			ipContainerCmdPrefix := []string{"ip"}
			if IsIPv6Cluster(f.ClientSet) {
				clientIP = externalContainer.GetIPv6()
				serverNodeIP = primaryInf.IPv6
				ipContainerCmdPrefix = append(ipContainerCmdPrefix, "-6")
			}
			gomega.Expect(clientIP).ShouldNot(gomega.BeEmpty())

			const pmtu = "1300"
			payloads := map[string]string{
				"non-fragmented": "1220",
				"fragmented":     "1320",
			}

			// We set a route MTU towards the server node emulating that PMTUD
			// has already happened resulting in a plausible low PMTU.
			// For UDP the system wide default IP_PMTUDISC_WANT will
			// result in fragmentation for packets bigger than the PMTU.
			// Note that fragmentation could also happen if a client chooses to
			// not use PMTUD with IP_PMTUDISC_DONT both for TCP or UDP but the
			// test setup required to achieve fragmentation without emulating
			// PMTUD is more complex so we stick to UDP.
			ginkgo.By("Lowering PMTU towards the server")
			ipContainerCmd := append(ipContainerCmdPrefix, "route", "add", serverNodeIP, "dev", infraprovider.Get().ExternalContainerPrimaryInterfaceName(), "src", clientIP, "mtu", pmtu)
			_, err = infraprovider.Get().ExecExternalContainerCommand(externalContainer, ipContainerCmd)
			framework.ExpectNoError(err, "lowering MTU in the external kind container failed: %v", err)
			providerCtx.AddCleanUpFn(func() error {
				ipContainerCmd := append(ipContainerCmdPrefix, "route", "del", serverNodeIP, "dev", infraprovider.Get().ExternalContainerPrimaryInterfaceName(), "src", clientIP, "mtu", pmtu)
				_, err = infraprovider.Get().ExecExternalContainerCommand(externalContainer, ipContainerCmd)
				return err
			})
			var udpPort int32
			for _, port := range service.Spec.Ports {
				if port.Protocol == v1.ProtocolUDP {
					udpPort = port.NodePort
				}
			}
			gomega.Expect(udpPort).NotTo(gomega.Equal(0))

			// To check that forwarding did not happen via host, we send a
			// non-fragmented packet first and then a fragmented one on the same
			// source port. If the server sees the same source port it means
			// that both packets were forwarded the same. This is because when
			// forwarding via OVN, GR SNATs from the node IP to the join subnet,
			// whereas forwarding via host, GR SNATs from the masquerade IP to
			// the join subnet. Thus, OVN sees both as different streams and
			// ends up replacing the source port to avoid the collision.
			sourcePortRegex := `to UDP client .*:(?P<Port>\d{1,5})`
			var sourcePort string
			for _, payload := range []string{"non-fragmented", "fragmented"} {
				ginkgo.By(fmt.Sprintf("Sending a %s UDP payload to the service node port", payload))
				payload := fmt.Sprintf("%0"+payloads[payload]+"d", 1)
				containerCmd := fmt.Sprintf("echo 'echo %s' | nc -w2 -u %s %d", payload, serverNodeIP, udpPort)
				if sourcePort != "" {
					containerCmd = fmt.Sprintf("echo 'echo %s' | nc -w2 -u -p %s %s %d", payload, sourcePort, serverNodeIP, udpPort)
				}
				framework.Logf("Running %v", containerCmd)
				stdout, err := infraprovider.Get().ExecExternalContainerCommand(externalContainer, []string{"/bin/sh", "-c", containerCmd})
				framework.ExpectNoError(err, "sending echo request failed: %v", err)

				ginkgo.By("Checking that the service received the request and replied")
				framework.Logf("Server replied with %s", stdout)
				gomega.Expect(stdout).To(gomega.Equal(payload), "server did not reply with the requested payload")

				ginkgo.By("Checking that the request was done on the intended source port")
				matches, err := CaptureContainerOutput(context.TODO(), f.ClientSet, f.Namespace.Name, serverPodName, serverContainerName, sourcePortRegex)
				framework.ExpectNoError(err)
				gomega.Expect(matches).To(gomega.HaveKey("Port"))
				gomega.Expect(matches["Port"]).ToNot(gomega.BeEmpty())
				if sourcePort != "" {
					gomega.Expect(matches).To(gomega.HaveKeyWithValue("Port", sourcePort), "request did not use the intended source port")
				}
				sourcePort = matches["Port"]
			}
		})
	})

	// This test verifies that a ClusterIP service remains reachable during a
	// rolling update that changes the container's target port. During the
	// rolling update, endpoint slices will contain endpoints with different
	// target port numbers for the same named port. OVN-K must program LB
	// rules for all distinct target ports so that both old and new pods
	// remain reachable.
	//
	// The test pauses the rollout once both target ports are observed in the
	// endpoint slices. While paused, no pods are being terminated, so every
	// request must succeed -- any failure indicates that OVN-K failed to
	// program LB rules for both target ports (the SDN-3551 bug).
	ginkgo.It("Maintains service connectivity during rolling update when target port changes", func() {
		namespace := f.Namespace.Name
		ctx := context.TODO()

		const (
			deploymentName = "rolling-update-backend"
			svcName        = "rolling-update-svc"
			portName       = "http"
			svcPort        = int32(80)
			oldTargetPort  = int32(8080)
			newTargetPort  = int32(9090)
		)
		replicas := int32(4)

		ginkgo.By(fmt.Sprintf("Creating a deployment with %d replicas listening on port %d", replicas, oldTargetPort))
		labels := map[string]string{"app": "rolling-update-test"}
		maxUnavailable := intstr.FromInt(0)
		maxSurge := intstr.FromInt(1)
		deployment := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      deploymentName,
				Namespace: namespace,
			},
			Spec: appsv1.DeploymentSpec{
				Replicas: &replicas,
				Selector: &metav1.LabelSelector{
					MatchLabels: labels,
				},
				Strategy: appsv1.DeploymentStrategy{
					Type: appsv1.RollingUpdateDeploymentStrategyType,
					RollingUpdate: &appsv1.RollingUpdateDeployment{
						MaxUnavailable: &maxUnavailable,
						MaxSurge:       &maxSurge,
					},
				},
				Template: v1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels: labels,
					},
					Spec: v1.PodSpec{
						Containers: []v1.Container{
							{
								Name:    "agnhost",
								Image:   images.AgnHost(),
								Command: []string{"/agnhost", "serve-hostname", fmt.Sprintf("--port=%d", oldTargetPort)},
								Ports: []v1.ContainerPort{
									{
										Name:          portName,
										ContainerPort: oldTargetPort,
										Protocol:      v1.ProtocolTCP,
									},
								},
							},
						},
					},
				},
			},
		}

		_, err := cs.AppsV1().Deployments(namespace).Create(ctx, deployment, metav1.CreateOptions{})
		framework.ExpectNoError(err)

		ginkgo.By("Waiting for deployment to become ready")
		err = wait.PollImmediate(framework.Poll, 2*time.Minute, func() (bool, error) {
			dp, err := cs.AppsV1().Deployments(namespace).Get(ctx, deploymentName, metav1.GetOptions{})
			if err != nil {
				return false, err
			}
			return dp.Status.ReadyReplicas == replicas, nil
		})
		framework.ExpectNoError(err, "deployment did not become ready in time")

		ginkgo.By("Creating a ClusterIP service with named target port")
		svc := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      svcName,
				Namespace: namespace,
			},
			Spec: v1.ServiceSpec{
				Selector: labels,
				Type:     v1.ServiceTypeClusterIP,
				Ports: []v1.ServicePort{
					{
						Name:       portName,
						Protocol:   v1.ProtocolTCP,
						Port:       svcPort,
						TargetPort: intstr.FromString(portName),
					},
				},
			},
		}
		svc, err = cs.CoreV1().Services(namespace).Create(ctx, svc, metav1.CreateOptions{})
		framework.ExpectNoError(err)

		ginkgo.By("Creating an exec pod for connectivity checks")
		execPod := e2epod.CreateExecPodOrFail(ctx, cs, namespace, "exec-pod-rolling-update", nil)

		serviceAddr := net.JoinHostPort(svc.Spec.ClusterIP, fmt.Sprintf("%d", svcPort))
		curlCmd := fmt.Sprintf("curl -q -s --connect-timeout 3 http://%s/", serviceAddr)

		ginkgo.By("Verifying initial service connectivity")
		gomega.Eventually(func() error {
			stdout, err := e2epodoutput.RunHostCmd(namespace, execPod.Name, curlCmd)
			if err != nil {
				return fmt.Errorf("connection failed: %v", err)
			}
			if strings.TrimSpace(stdout) == "" {
				return fmt.Errorf("empty response from service")
			}
			framework.Logf("Initial connectivity check succeeded, got response: %s", strings.TrimSpace(stdout))
			return nil
		}, 30*time.Second, framework.Poll).Should(gomega.Succeed())

		ginkgo.By("Recording initial (old) pod names before rolling update")
		oldPodNames := sets.New[string]()
		podList, err := cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
			LabelSelector: "app=rolling-update-test",
		})
		framework.ExpectNoError(err)
		for _, pod := range podList.Items {
			oldPodNames.Insert(pod.Name)
		}
		framework.Logf("Old pod names: %v", oldPodNames.UnsortedList())

		ginkgo.By(fmt.Sprintf("Triggering rolling update: changing target port from %d to %d", oldTargetPort, newTargetPort))
		err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
			deployment, err = cs.AppsV1().Deployments(namespace).Get(ctx, deploymentName, metav1.GetOptions{})
			if err != nil {
				return err
			}
			deployment.Spec.Template.Spec.Containers[0].Command = []string{
				"/agnhost", "serve-hostname", fmt.Sprintf("--port=%d", newTargetPort),
			}
			deployment.Spec.Template.Spec.Containers[0].Ports[0].ContainerPort = newTargetPort
			_, err = cs.AppsV1().Deployments(namespace).Update(ctx, deployment, metav1.UpdateOptions{})
			return err
		})
		framework.ExpectNoError(err)

		ginkgo.By("Waiting for endpoint slices to contain both old and new target ports")
		err = wait.PollImmediate(time.Second, 2*time.Minute, func() (bool, error) {
			ports, err := getReadyEndpointSlicePorts(cs, namespace, svcName)
			if err != nil {
				framework.Logf("Transient error listing endpoint slices: %v", err)
				return false, nil
			}
			if ports.Has(oldTargetPort) && ports.Has(newTargetPort) {
				framework.Logf("Detected both target ports in endpoint slices: %v", ports.UnsortedList())
				return true, nil
			}
			framework.Logf("Current target ports in endpoint slices: %v (waiting for both %d and %d)",
				ports.UnsortedList(), oldTargetPort, newTargetPort)
			return false, nil
		})
		framework.ExpectNoError(err, "timed out waiting for both target ports to appear in endpoint slices")

		ginkgo.By("Pausing the rolling update to freeze the multi-port state")
		err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
			deployment, err = cs.AppsV1().Deployments(namespace).Get(ctx, deploymentName, metav1.GetOptions{})
			if err != nil {
				return err
			}
			deployment.Spec.Paused = true
			_, err = cs.AppsV1().Deployments(namespace).Update(ctx, deployment, metav1.UpdateOptions{})
			return err
		})
		framework.ExpectNoError(err)

		ginkgo.By("Waiting for all backend pods to be running and ready (no in-flight terminations)")
		err = wait.PollImmediate(framework.Poll, 1*time.Minute, func() (bool, error) {
			pods, err := cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
				LabelSelector: "app=rolling-update-test",
			})
			if err != nil {
				return false, err
			}
			for _, pod := range pods.Items {
				if pod.DeletionTimestamp != nil {
					framework.Logf("Pod %s is still terminating", pod.Name)
					return false, nil
				}
				if pod.Status.Phase != v1.PodRunning {
					framework.Logf("Pod %s is in phase %s", pod.Name, pod.Status.Phase)
					return false, nil
				}
				ready := false
				for _, cond := range pod.Status.Conditions {
					if cond.Type == v1.PodReady && cond.Status == v1.ConditionTrue {
						ready = true
						break
					}
				}
				if !ready {
					framework.Logf("Pod %s is not ready", pod.Name)
					return false, nil
				}
			}
			framework.Logf("All %d backend pods are running and ready", len(pods.Items))
			return true, nil
		})
		framework.ExpectNoError(err, "timed out waiting for all pods to be running and ready after pause")

		ginkgo.By("Verifying endpoint slices still contain both target ports while paused")
		var portNumbers sets.Set[int32]
		gomega.Eventually(func() error {
			var err error
			portNumbers, err = getReadyEndpointSlicePorts(cs, namespace, svcName)
			return err
		}, 30*time.Second, framework.Poll).Should(gomega.Succeed())
		framework.Logf("Target ports in endpoint slices while paused: %v", portNumbers.UnsortedList())
		gomega.Expect(portNumbers.Has(oldTargetPort) && portNumbers.Has(newTargetPort)).To(gomega.BeTrue(),
			"expected both target ports %d and %d in endpoint slices while paused, got: %v",
			oldTargetPort, newTargetPort, portNumbers.UnsortedList())

		ginkgo.By("Identifying new pods created by the rolling update")
		newPodNames := sets.New[string]()
		podList, err = cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
			LabelSelector: "app=rolling-update-test",
		})
		framework.ExpectNoError(err)
		for _, pod := range podList.Items {
			if pod.DeletionTimestamp == nil && !oldPodNames.Has(pod.Name) {
				newPodNames.Insert(pod.Name)
			}
		}
		framework.Logf("New pod names (listening on port %d): %v", newTargetPort, newPodNames.UnsortedList())
		gomega.Expect(newPodNames.Len()).To(gomega.BeNumerically(">", 0),
			"expected at least one new pod after rolling update pause")

		ginkgo.By("Verifying service traffic reaches both old and new pods (validates SDN-3551 fix)")
		// Without the SDN-3551 fix, OVN-K programs LB backends with only
		// one target port, so traffic never reaches pods on the other port.
		// We verify that responses come from both old pods (port 8080) and
		// new pods (port 9090), proving the LB has correct entries for both.
		hitOld := sets.New[string]()
		hitNew := sets.New[string]()
		gomega.Eventually(func() error {
			stdout, err := e2epodoutput.RunHostCmd(namespace, execPod.Name, curlCmd)
			if err != nil {
				return fmt.Errorf("connection failed: %v", err)
			}
			hostname := strings.TrimSpace(stdout)
			if hostname == "" {
				return fmt.Errorf("empty response from service")
			}
			if oldPodNames.Has(hostname) {
				hitOld.Insert(hostname)
			} else if newPodNames.Has(hostname) {
				hitNew.Insert(hostname)
			}
			if hitOld.Len() > 0 && hitNew.Len() > 0 {
				return nil
			}
			return fmt.Errorf("have not yet hit both old and new pods: hitOld=%v, hitNew=%v",
				hitOld.UnsortedList(), hitNew.UnsortedList())
		}, 30*time.Second, 500*time.Millisecond).Should(gomega.Succeed(),
			"traffic must reach both old pods (port %d) and new pods (port %d) to confirm LB has rules for both target ports",
			oldTargetPort, newTargetPort)
		framework.Logf("Confirmed traffic reached old pods %v and new pods %v", hitOld.UnsortedList(), hitNew.UnsortedList())

		ginkgo.By("Resuming the rolling update")
		err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
			deployment, err = cs.AppsV1().Deployments(namespace).Get(ctx, deploymentName, metav1.GetOptions{})
			if err != nil {
				return err
			}
			deployment.Spec.Paused = false
			_, err = cs.AppsV1().Deployments(namespace).Update(ctx, deployment, metav1.UpdateOptions{})
			return err
		})
		framework.ExpectNoError(err)

		ginkgo.By("Waiting for the rolling update to complete")
		err = wait.PollImmediate(framework.Poll, 3*time.Minute, func() (bool, error) {
			dp, err := cs.AppsV1().Deployments(namespace).Get(ctx, deploymentName, metav1.GetOptions{})
			if err != nil {
				return false, err
			}
			return dp.Status.UpdatedReplicas == replicas &&
				dp.Status.ReadyReplicas == replicas &&
				dp.Status.AvailableReplicas == replicas, nil
		})
		framework.ExpectNoError(err, "rolling update did not complete in time")

		ginkgo.By("Verifying service connectivity after the rolling update completes")
		gomega.Eventually(func() error {
			stdout, err := e2epodoutput.RunHostCmd(namespace, execPod.Name, curlCmd)
			if err != nil {
				return fmt.Errorf("connection failed: %v", err)
			}
			if strings.TrimSpace(stdout) == "" {
				return fmt.Errorf("empty response from service")
			}
			framework.Logf("Post-rollout connectivity check succeeded, got response: %s", strings.TrimSpace(stdout))
			return nil
		}, 30*time.Second, framework.Poll).Should(gomega.Succeed())

		ginkgo.By("Verifying all endpoint slices with ready endpoints now use only the new target port")
		gomega.Eventually(func() error {
			ports, err := getReadyEndpointSlicePorts(cs, namespace, svcName)
			if err != nil {
				return fmt.Errorf("listing endpoint slices: %v", err)
			}
			if ports.Has(oldTargetPort) {
				return fmt.Errorf("old target port %d still present in endpoint slices: %v", oldTargetPort, ports.UnsortedList())
			}
			if !ports.Has(newTargetPort) {
				return fmt.Errorf("new target port %d not found in endpoint slices: %v", newTargetPort, ports.UnsortedList())
			}
			return nil
		}, 2*time.Minute, framework.Poll).Should(gomega.Succeed(),
			"after rolling update completes, all endpoint slices should use the new target port")
	})

	// This test verifies that a ClusterIP service with multiple endpoints
	// using different numerical values for the same named port correctly
	// load-balances across all target ports. This can occur in steady state
	// when pods with different container images resolve a named port to
	// different numbers, or transiently during rolling updates.
	ginkgo.It("Distributes traffic to multiple endpoints with different named target ports", func() {
		namespace := f.Namespace.Name
		ctx := context.TODO()

		const (
			svcName  = "multi-target-port-svc"
			portName = "http"
			svcPort  = int32(80)
			port1    = int32(8080)
			port2    = int32(9090)
		)

		labels := map[string]string{"app": "multi-target-port"}

		ginkgo.By(fmt.Sprintf("Creating first pod listening on port %d", port1))
		pod1 := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "backend-port-1",
				Namespace: namespace,
				Labels:    labels,
			},
			Spec: v1.PodSpec{
				Containers: []v1.Container{
					{
						Name:    "agnhost",
						Image:   images.AgnHost(),
						Command: []string{"/agnhost", "serve-hostname", fmt.Sprintf("--port=%d", port1)},
						Ports: []v1.ContainerPort{
							{
								Name:          portName,
								ContainerPort: port1,
								Protocol:      v1.ProtocolTCP,
							},
						},
					},
				},
			},
		}
		_, err := cs.CoreV1().Pods(namespace).Create(ctx, pod1, metav1.CreateOptions{})
		framework.ExpectNoError(err)

		ginkgo.By(fmt.Sprintf("Creating second pod listening on port %d", port2))
		pod2 := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "backend-port-2",
				Namespace: namespace,
				Labels:    labels,
			},
			Spec: v1.PodSpec{
				Containers: []v1.Container{
					{
						Name:    "agnhost",
						Image:   images.AgnHost(),
						Command: []string{"/agnhost", "serve-hostname", fmt.Sprintf("--port=%d", port2)},
						Ports: []v1.ContainerPort{
							{
								Name:          portName,
								ContainerPort: port2,
								Protocol:      v1.ProtocolTCP,
							},
						},
					},
				},
			},
		}
		_, err = cs.CoreV1().Pods(namespace).Create(ctx, pod2, metav1.CreateOptions{})
		framework.ExpectNoError(err)

		ginkgo.By("Waiting for both pods to be running and ready")
		err = e2epod.WaitForPodsRunningReady(ctx, cs, namespace, 2, 2*time.Minute)
		framework.ExpectNoError(err)

		ginkgo.By("Creating a ClusterIP service with a named target port")
		svc := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      svcName,
				Namespace: namespace,
			},
			Spec: v1.ServiceSpec{
				Selector: labels,
				Type:     v1.ServiceTypeClusterIP,
				Ports: []v1.ServicePort{
					{
						Name:       portName,
						Protocol:   v1.ProtocolTCP,
						Port:       svcPort,
						TargetPort: intstr.FromString(portName),
					},
				},
			},
		}
		svc, err = cs.CoreV1().Services(namespace).Create(ctx, svc, metav1.CreateOptions{})
		framework.ExpectNoError(err)

		ginkgo.By("Waiting for endpoint slices to contain both target ports")
		err = wait.PollImmediate(framework.Poll, 1*time.Minute, func() (bool, error) {
			ports, err := getReadyEndpointSlicePorts(cs, namespace, svcName)
			if err != nil {
				return false, nil
			}
			if ports.Has(port1) && ports.Has(port2) {
				framework.Logf("Endpoint slices contain both ports: %v", ports.UnsortedList())
				return true, nil
			}
			framework.Logf("Current endpoint slice ports: %v (waiting for both %d and %d)", ports.UnsortedList(), port1, port2)
			return false, nil
		})
		framework.ExpectNoError(err, "timed out waiting for both target ports in endpoint slices")

		ginkgo.By("Creating a client pod to curl the service")
		clientPod := e2epod.NewAgnhostPod(namespace, "curl-client", nil, nil, nil)
		clientPod.Spec.Containers[0].Command = []string{"sleep", "infinity"}
		clientPod = e2epod.NewPodClient(f).CreateSync(ctx, clientPod)

		ginkgo.By("Verifying service traffic reaches both backends (different target ports)")
		clusterIP := svc.Spec.ClusterIP
		curlCmd := fmt.Sprintf("curl -q -s --connect-timeout 2 http://%s/",
			net.JoinHostPort(clusterIP, fmt.Sprintf("%d", svcPort)))

		hitPod1 := false
		hitPod2 := false
		gomega.Eventually(func() error {
			stdout, err := e2epodoutput.RunHostCmd(namespace, clientPod.Name, curlCmd)
			if err != nil {
				return fmt.Errorf("connection failed: %v", err)
			}
			hostname := strings.TrimSpace(stdout)
			if hostname == "" {
				return fmt.Errorf("empty response from service")
			}
			framework.Logf("Got response from: %s", hostname)
			if hostname == pod1.Name {
				hitPod1 = true
			} else if hostname == pod2.Name {
				hitPod2 = true
			}
			if hitPod1 && hitPod2 {
				return nil
			}
			return fmt.Errorf("have not yet hit both pods: hitPod1=%v, hitPod2=%v", hitPod1, hitPod2)
		}, 30*time.Second, 500*time.Millisecond).Should(gomega.Succeed(),
			"traffic must reach both endpoints (ports %d and %d) to confirm LB has rules for all target ports",
			port1, port2)
		framework.Logf("Confirmed traffic reached both pods: %s (port %d) and %s (port %d)",
			pod1.Name, port1, pod2.Name, port2)
	})

	// This test verifies that ETP=Local NodePort services with host-networked
	// endpoints correctly load-balance across multiple target ports when
	// using named ports. Two host-network pods on the same node listen on
	// different ports but share the same named port in their container spec.
	// The OpenFlow group installed on breth0 must select between both target
	// ports for new incoming connections.
	ginkgo.It("Distributes traffic to multiple host-network endpoints with different named target ports", func() {
		namespace := f.Namespace.Name
		ctx := context.TODO()

		const (
			svcName  = "host-net-multi-port-svc"
			portName = "http"
			svcPort  = int32(80)
		)
		port1 := int32(infraprovider.Get().GetK8HostPort())
		port2 := int32(infraprovider.Get().GetK8HostPort())
		gomega.Expect(port1).ToNot(gomega.Equal(port2),
			"allocated host ports must differ")

		ginkgo.By("Selecting a schedulable node for host-network pods")
		nodeList, err := e2enode.GetBoundedReadySchedulableNodes(ctx, cs, 1)
		framework.ExpectNoError(err)
		gomega.Expect(nodeList.Items).ToNot(gomega.BeEmpty())
		nodeName := nodeList.Items[0].Name

		labels := map[string]string{"app": "host-net-multi-port"}

		ginkgo.By(fmt.Sprintf("Creating first host-network pod listening on port %d", port1))
		pod1 := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "host-net-pod-1",
				Namespace: namespace,
				Labels:    labels,
			},
			Spec: v1.PodSpec{
				NodeName:    nodeName,
				HostNetwork: true,
				Containers: []v1.Container{
					{
						Name:    "agnhost",
						Image:   images.AgnHost(),
						Command: []string{"/agnhost", "netexec", fmt.Sprintf("--http-port=%d", port1), "--udp-port=-1"},
						Ports: []v1.ContainerPort{
							{
								Name:          portName,
								ContainerPort: port1,
								Protocol:      v1.ProtocolTCP,
							},
						},
					},
				},
			},
		}
		_, err = cs.CoreV1().Pods(namespace).Create(ctx, pod1, metav1.CreateOptions{})
		framework.ExpectNoError(err)

		ginkgo.By(fmt.Sprintf("Creating second host-network pod listening on port %d", port2))
		pod2 := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "host-net-pod-2",
				Namespace: namespace,
				Labels:    labels,
			},
			Spec: v1.PodSpec{
				NodeName:    nodeName,
				HostNetwork: true,
				Containers: []v1.Container{
					{
						Name:    "agnhost",
						Image:   images.AgnHost(),
						Command: []string{"/agnhost", "netexec", fmt.Sprintf("--http-port=%d", port2), "--udp-port=-1"},
						Ports: []v1.ContainerPort{
							{
								Name:          portName,
								ContainerPort: port2,
								Protocol:      v1.ProtocolTCP,
							},
						},
					},
				},
			},
		}
		_, err = cs.CoreV1().Pods(namespace).Create(ctx, pod2, metav1.CreateOptions{})
		framework.ExpectNoError(err)

		ginkgo.By("Waiting for both pods to be running and ready")
		err = e2epod.WaitForPodsRunningReady(ctx, cs, namespace, 2, 2*time.Minute)
		framework.ExpectNoError(err)

		ginkgo.By("Creating an ETP=Local NodePort service with a named target port")
		svc := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      svcName,
				Namespace: namespace,
			},
			Spec: v1.ServiceSpec{
				Selector:              labels,
				Type:                  v1.ServiceTypeNodePort,
				IPFamilyPolicy:        ptr.To(v1.IPFamilyPolicyPreferDualStack),
				ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyLocal,
				Ports: []v1.ServicePort{
					{
						Name:       portName,
						Protocol:   v1.ProtocolTCP,
						Port:       svcPort,
						TargetPort: intstr.FromString(portName),
					},
				},
			},
		}
		svc, err = cs.CoreV1().Services(namespace).Create(ctx, svc, metav1.CreateOptions{})
		framework.ExpectNoError(err)
		nodePort := svc.Spec.Ports[0].NodePort

		ginkgo.By("Waiting for endpoint slices to contain both target ports")
		err = wait.PollImmediate(framework.Poll, 1*time.Minute, func() (bool, error) {
			ports, err := getReadyEndpointSlicePorts(cs, namespace, svcName)
			if err != nil {
				return false, nil
			}
			if ports.Has(port1) && ports.Has(port2) {
				framework.Logf("Endpoint slices contain both ports: %v", ports.UnsortedList())
				return true, nil
			}
			framework.Logf("Current endpoint slice ports: %v (waiting for both %d and %d)", ports.UnsortedList(), port1, port2)
			return false, nil
		})
		framework.ExpectNoError(err, "timed out waiting for both target ports in endpoint slices")

		ginkgo.By("Creating a client pod on a different node to send external traffic")
		clientNodeName := nodeName
		if len(nodeList.Items) > 1 {
			clientNodeName = nodeList.Items[1].Name
		}
		clientPod := e2epod.NewAgnhostPod(namespace, "curl-client", nil, nil, nil)
		clientPod.Spec.NodeName = clientNodeName
		clientPod.Spec.Containers[0].Command = []string{"sleep", "infinity"}
		clientPod = e2epod.NewPodClient(f).CreateSync(ctx, clientPod)

		ginkgo.By(fmt.Sprintf("Curling NodePort %d on node %s to verify traffic reaches both target ports", nodePort, nodeName))
		nodeIP := nodeList.Items[0].Status.Addresses[0].Address
		curlCmd := fmt.Sprintf("curl -q -s --max-time 2 --connect-timeout 1 http://%s/serverport",
			net.JoinHostPort(nodeIP, fmt.Sprintf("%d", nodePort)))

		hitPort1 := false
		hitPort2 := false
		gomega.Eventually(func() error {
			stdout, err := e2epodoutput.RunHostCmd(namespace, clientPod.Name, curlCmd)
			if err != nil {
				return fmt.Errorf("connection failed: %v", err)
			}
			resp := strings.TrimSpace(stdout)
			if resp == "" {
				return fmt.Errorf("empty response from service")
			}
			framework.Logf("Got response from server port: %s", resp)
			if resp == fmt.Sprintf("%d", port1) {
				hitPort1 = true
			} else if resp == fmt.Sprintf("%d", port2) {
				hitPort2 = true
			}
			if hitPort1 && hitPort2 {
				return nil
			}
			return fmt.Errorf("have not yet hit both ports: hitPort1=%v, hitPort2=%v", hitPort1, hitPort2)
		}, 30*time.Second, 500*time.Millisecond).Should(gomega.Succeed(),
			"traffic must reach both host-network endpoints (ports %d and %d) via the OpenFlow select group",
			port1, port2)
		framework.Logf("Confirmed traffic reached both target ports %d and %d", port1, port2)
	})

	// This test verifies that external NodePort traffic arriving at breth0
	// (in_port=physical) is load-balanced across multiple host-network target
	// ports via the OpenFlow select group. A host-network client on a
	// different node ensures traffic traverses the physical network and
	// enters the target node's OVS bridge from the external port, exercising
	// the breth0 ETP=Local DNAT path rather than the OVN LB pipeline.
	ginkgo.It("Distributes external traffic to multiple host-network endpoints with different named target ports", func() {
		namespace := f.Namespace.Name
		ctx := context.TODO()

		const (
			svcName  = "host-ext-multi-port-svc"
			portName = "http"
			svcPort  = int32(80)
		)
		port1 := int32(infraprovider.Get().GetK8HostPort())
		port2 := int32(infraprovider.Get().GetK8HostPort())
		gomega.Expect(port1).ToNot(gomega.Equal(port2),
			"allocated host ports must differ")

		ginkgo.By("Selecting 2 schedulable nodes")
		nodeList, err := e2enode.GetBoundedReadySchedulableNodes(ctx, cs, 2)
		framework.ExpectNoError(err)
		if len(nodeList.Items) < 2 {
			e2eskipper.Skipf("Test requires >= 2 Ready nodes so the client sends traffic via the physical network, but there are only %v nodes", len(nodeList.Items))
		}
		serverNodeName := nodeList.Items[0].Name
		clientNodeName := nodeList.Items[1].Name

		labels := map[string]string{"app": "host-ext-multi-port"}

		ginkgo.By(fmt.Sprintf("Creating first host-network pod on %s listening on port %d", serverNodeName, port1))
		pod1 := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "host-net-pod-1",
				Namespace: namespace,
				Labels:    labels,
			},
			Spec: v1.PodSpec{
				NodeName:    serverNodeName,
				HostNetwork: true,
				Containers: []v1.Container{
					{
						Name:    "agnhost",
						Image:   images.AgnHost(),
						Command: []string{"/agnhost", "netexec", fmt.Sprintf("--http-port=%d", port1), "--udp-port=-1"},
						Ports: []v1.ContainerPort{
							{
								Name:          portName,
								ContainerPort: port1,
								Protocol:      v1.ProtocolTCP,
							},
						},
					},
				},
			},
		}
		_, err = cs.CoreV1().Pods(namespace).Create(ctx, pod1, metav1.CreateOptions{})
		framework.ExpectNoError(err)

		ginkgo.By(fmt.Sprintf("Creating second host-network pod on %s listening on port %d", serverNodeName, port2))
		pod2 := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "host-net-pod-2",
				Namespace: namespace,
				Labels:    labels,
			},
			Spec: v1.PodSpec{
				NodeName:    serverNodeName,
				HostNetwork: true,
				Containers: []v1.Container{
					{
						Name:    "agnhost",
						Image:   images.AgnHost(),
						Command: []string{"/agnhost", "netexec", fmt.Sprintf("--http-port=%d", port2), "--udp-port=-1"},
						Ports: []v1.ContainerPort{
							{
								Name:          portName,
								ContainerPort: port2,
								Protocol:      v1.ProtocolTCP,
							},
						},
					},
				},
			},
		}
		_, err = cs.CoreV1().Pods(namespace).Create(ctx, pod2, metav1.CreateOptions{})
		framework.ExpectNoError(err)

		ginkgo.By("Waiting for both pods to be running and ready")
		err = e2epod.WaitForPodsRunningReady(ctx, cs, namespace, 2, 2*time.Minute)
		framework.ExpectNoError(err)

		ginkgo.By("Creating an ETP=Local NodePort service with a named target port")
		svc := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      svcName,
				Namespace: namespace,
			},
			Spec: v1.ServiceSpec{
				Selector:              labels,
				Type:                  v1.ServiceTypeNodePort,
				IPFamilyPolicy:        ptr.To(v1.IPFamilyPolicyPreferDualStack),
				ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyLocal,
				Ports: []v1.ServicePort{
					{
						Name:       portName,
						Protocol:   v1.ProtocolTCP,
						Port:       svcPort,
						TargetPort: intstr.FromString(portName),
					},
				},
			},
		}
		svc, err = cs.CoreV1().Services(namespace).Create(ctx, svc, metav1.CreateOptions{})
		framework.ExpectNoError(err)
		nodePort := svc.Spec.Ports[0].NodePort

		ginkgo.By("Waiting for endpoint slices to contain both target ports")
		err = wait.PollImmediate(framework.Poll, 1*time.Minute, func() (bool, error) {
			ports, err := getReadyEndpointSlicePorts(cs, namespace, svcName)
			if err != nil {
				return false, nil
			}
			if ports.Has(port1) && ports.Has(port2) {
				framework.Logf("Endpoint slices contain both ports: %v", ports.UnsortedList())
				return true, nil
			}
			framework.Logf("Current endpoint slice ports: %v (waiting for both %d and %d)", ports.UnsortedList(), port1, port2)
			return false, nil
		})
		framework.ExpectNoError(err, "timed out waiting for both target ports in endpoint slices")

		ginkgo.By(fmt.Sprintf("Creating a host-network client pod on %s", clientNodeName))
		clientPod := e2epod.NewAgnhostPod(namespace, "curl-host-client", nil, nil, nil)
		clientPod.Spec.NodeName = clientNodeName
		clientPod.Spec.HostNetwork = true
		for k := range clientPod.Spec.Containers {
			if clientPod.Spec.Containers[k].Name == "agnhost-container" {
				clientPod.Spec.Containers[k].Command = []string{"sleep", "infinity"}
				clientPod.Spec.Containers[k].SecurityContext.Privileged = pointer.Bool(true)
			}
		}
		clientPod = e2epod.NewPodClient(f).CreateSync(ctx, clientPod)

		serverNode := nodeList.Items[0]
		serverNodeIPs := e2enode.GetAddresses(&serverNode, v1.NodeInternalIP)
		gomega.Expect(serverNodeIPs).ToNot(gomega.BeEmpty())

		for _, serverNodeIP := range serverNodeIPs {
			addrLabel := "IPv4"
			if utilnet.IsIPv6String(serverNodeIP) {
				addrLabel = "IPv6"
			}

			ginkgo.By(fmt.Sprintf("Curling %s NodePort %d on %s (%s) to verify traffic reaches both target ports",
				addrLabel, nodePort, serverNodeName, serverNodeIP))
			curlCmd := fmt.Sprintf("curl -q -s --max-time 2 --connect-timeout 1 http://%s/serverport",
				net.JoinHostPort(serverNodeIP, fmt.Sprintf("%d", nodePort)))

			hitPort1 := false
			hitPort2 := false
			gomega.Eventually(func() error {
				stdout, err := e2epodoutput.RunHostCmd(namespace, clientPod.Name, curlCmd)
				if err != nil {
					return fmt.Errorf("connection failed: %v", err)
				}
				resp := strings.TrimSpace(stdout)
				if resp == "" {
					return fmt.Errorf("empty response from service")
				}
				framework.Logf("[%s] Got response from server port: %s", addrLabel, resp)
				if resp == fmt.Sprintf("%d", port1) {
					hitPort1 = true
				} else if resp == fmt.Sprintf("%d", port2) {
					hitPort2 = true
				}
				if hitPort1 && hitPort2 {
					return nil
				}
				return fmt.Errorf("have not yet hit both ports via %s: hitPort1=%v, hitPort2=%v", addrLabel, hitPort1, hitPort2)
			}, 30*time.Second, 500*time.Millisecond).Should(gomega.Succeed(),
				"external traffic via %s must reach both host-network endpoints (ports %d and %d) via breth0 OpenFlow group",
				addrLabel, port1, port2)
			framework.Logf("[%s] Confirmed traffic reached both target ports %d and %d", addrLabel, port1, port2)
		}
	})

})

// getReadyEndpointSlicePorts returns the set of target port numbers from endpoint
// slices that have at least one ready endpoint. Slices with no endpoints or no
// ready endpoints are excluded.
func getReadyEndpointSlicePorts(cs clientset.Interface, namespace, svcName string) (sets.Set[int32], error) {
	slices, err := cs.DiscoveryV1().EndpointSlices(namespace).List(context.TODO(), metav1.ListOptions{
		LabelSelector: fmt.Sprintf("kubernetes.io/service-name=%s", svcName),
	})
	if err != nil {
		return nil, err
	}
	ports := sets.New[int32]()
	for _, slice := range slices.Items {
		if len(slice.Endpoints) == 0 {
			continue
		}
		hasReady := false
		for _, ep := range slice.Endpoints {
			cond := ep.Conditions
			ready := cond.Ready == nil || *cond.Ready
			serving := cond.Serving == nil || *cond.Serving
			terminating := cond.Terminating != nil && *cond.Terminating
			if ready && serving && !terminating {
				hasReady = true
				break
			}
		}
		if !hasReady {
			continue
		}
		for _, port := range slice.Ports {
			if port.Port != nil {
				ports.Insert(*port.Port)
			}
		}
	}
	return ports, nil
}

func getServiceBackendsFromPod(execPod *v1.Pod, serviceIP string, servicePort int) []string {
	connectionAttempts := 15
	serviceIPPort := net.JoinHostPort(serviceIP, strconv.Itoa(servicePort))
	curl := fmt.Sprintf(`curl -q -s --connect-timeout 2 http://%s/`, serviceIPPort)
	cmd := fmt.Sprintf("for i in $(seq 1 %d); do echo; %s ; done", connectionAttempts, curl)

	stdout, err := e2epodoutput.RunHostCmd(execPod.Namespace, execPod.Name, cmd)
	if err != nil {
		framework.Logf("Failed to get response from %s. Retry until timeout", serviceIPPort)
		return nil
	}
	hosts := strings.Split(stdout, "\n")
	nonEmptyHosts := []string{}
	for _, host := range hosts {
		if len(host) > 0 {
			nonEmptyHosts = append(nonEmptyHosts, strings.TrimSpace(host))
		}
	}
	gomega.Expect(len(nonEmptyHosts)).To(gomega.Equal(connectionAttempts), fmt.Sprintf("Expected %v replies, got %v", connectionAttempts, nonEmptyHosts))
	return nonEmptyHosts
}

// This test ensures that - when a pod that's a backend for a service curls the
// service ip; if the traffic was DNAT-ed to the same src pod (hairpin/loopback case) -
// the srcIP of reply traffic is SNATed to the special masqurade IP 169.254.0.5
// or "fd69::5"
var _ = ginkgo.Describe("Service Hairpin SNAT", feature.Service, func() {
	const (
		svcName                        = "service-hairpin-test"
		backendName                    = "hairpin-backend-pod"
		endpointHTTPPort        uint16 = 80
		serviceHTTPPort         uint16 = 6666
		V4LBHairpinMasqueradeIP        = "169.254.0.5"
		V6LBHairpinMasqueradeIP        = "fd69::5"
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
		nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), f.ClientSet, 2)
		framework.ExpectNoError(err)
		if len(nodes.Items) < 2 {
			e2eskipper.Skipf("Test requires >= 2 Ready nodes, but there are only %v nodes", len(nodes.Items))
		}
		nodeIPs := e2enode.GetAddresses(&nodes.Items[1], v1.NodeInternalIP)
		gomega.Expect(nodeIPs).NotTo(gomega.BeEmpty(), "second Ready node must have an InternalIP")
		namespaceName = f.Namespace.Name
		backendNodeName = nodes.Items[0].Name
		nodeIP = nodeIPs[0]
	})

	ginkgo.It("Should ensure service hairpin traffic is SNATed to hairpin masquerade IP; Switch LB", func() {

		ginkgo.By("creating an ovn-network backend pod")
		_, err := createGenericPodWithLabel(f, backendName, backendNodeName, namespaceName, getAgnHostHTTPPortBindFullCMD(endpointHTTPPort), hairpinPodSel)
		framework.ExpectNoError(err, fmt.Sprintf("unable to create backend pod: %s, err: %v", backendName, err))

		ginkgo.By("creating a TCP service service-for-pods with type=ClusterIP in namespace " + namespaceName)

		svcIP, err = createServiceForPodsWithLabel(f, namespaceName, serviceHTTPPort, endpointHTTPPort, "ClusterIP", hairpinPodSel)
		framework.ExpectNoError(err, fmt.Sprintf("unable to create service: service-for-pods, err: %v", err))

		svc, err := f.ClientSet.CoreV1().Services(namespaceName).Get(context.TODO(), "service-for-pods", metav1.GetOptions{})
		framework.ExpectNoError(err, "failed to fetch service: service-for-pods")
		// one backend pod -> one endpoint address per service IP family
		numBackendPods := 1
		err = e2eendpointslice.WaitForEndpointCount(context.TODO(), f.ClientSet, namespaceName, "service-for-pods", numBackendPods*len(svc.Spec.IPFamilies))
		framework.ExpectNoError(err, fmt.Sprintf("service: service-for-pods never had an endpoint, err: %v", err))

		ginkgo.By("by sending a TCP packet to service service-for-pods with type=ClusterIP in namespace " + namespaceName + " from backend pod " + backendName)

		if utilnet.IsIPv6String(svcIP) {
			framework.Logf("service: service-for-pods is ipv6")
			isIpv6 = true
		}

		clientIP := pokeEndpointViaPod(f, namespaceName, backendName, svcIP, serviceHTTPPort, "clientip")
		clientIP, _, err = net.SplitHostPort(clientIP)
		framework.ExpectNoError(err, "failed to parse client ip:port")

		if isIpv6 {
			gomega.Expect(clientIP).To(gomega.Equal(V6LBHairpinMasqueradeIP), fmt.Sprintf("returned client ipv6: %v was not correct", clientIP))
		} else {
			gomega.Expect(clientIP).To(gomega.Equal(V4LBHairpinMasqueradeIP), fmt.Sprintf("returned client ipv4: %v was not correct", clientIP))
		}
	})

	ginkgo.It("Should ensure service hairpin traffic is NOT SNATed to hairpin masquerade IP; GR LB", func() {

		ginkgo.By("creating an host-network backend pod on " + backendNodeName)
		// create hostNeworkedPods
		hostNetPort := infraprovider.Get().GetK8HostPort()
		_, err := createPod(f, backendName, backendNodeName, namespaceName, []string{}, hairpinPodSel, func(p *v1.Pod) {
			p.Spec.Containers[0].Command = getAgnHostHTTPPortBindFullCMD(hostNetPort)
			p.Spec.HostNetwork = true
		})
		framework.ExpectNoError(err, fmt.Sprintf("unable to create backend pod: %s, err: %v", backendName, err))

		ginkgo.By("creating a TCP service service-for-pods with type=NodePort in namespace " + namespaceName)

		svcIP, err = createServiceForPodsWithLabel(f, namespaceName, serviceHTTPPort, hostNetPort, "NodePort", hairpinPodSel)
		framework.ExpectNoError(err, fmt.Sprintf("unable to create service: service-for-pods, err: %v", err))

		svc, err := f.ClientSet.CoreV1().Services(namespaceName).Get(context.TODO(), "service-for-pods", metav1.GetOptions{})
		framework.ExpectNoError(err, "failed to fetch service: service-for-pods")

		// one backend pod -> one endpoint address per service IP family
		numBackendPods := 1
		err = e2eendpointslice.WaitForEndpointCount(context.TODO(), f.ClientSet, namespaceName, "service-for-pods", numBackendPods*len(svc.Spec.IPFamilies))
		framework.ExpectNoError(err, fmt.Sprintf("service: service-for-pods never had an endpoint, err: %v", err))

		ginkgo.By("by sending a TCP packet to service service-for-pods with type=NodePort(" + nodeIP + ":" + fmt.Sprint(svc.Spec.Ports[0].NodePort) + ") in namespace " + namespaceName + " from node " + backendNodeName)

		var clientIP string
		err = wait.PollImmediate(2*time.Second, 30*time.Second, func() (bool, error) {
			var pokeErr error
			clientIP, pokeErr = tryPokeEndpointViaNode(backendNodeName, "http", nodeIP, hostNetPort, uint16(svc.Spec.Ports[0].NodePort), "clientip")
			if pokeErr != nil || clientIP == "" {
				return false, nil
			}
			return true, nil
		})
		framework.ExpectNoError(err, "timed out waiting for successful NodePort response")

		clientIP, _, err = net.SplitHostPort(clientIP)
		framework.ExpectNoError(err, "failed to parse client ip:port")

		gomega.Expect(clientIP).To(gomega.Equal(nodeIP), fmt.Sprintf("returned client: %v was not correct", clientIP))
	})

})

var _ = ginkgo.Describe("Load Balancer Service Tests with MetalLB", feature.Service, func() {

	const (
		svcName                     = "lbservice-test"
		backendName                 = "lb-backend-pod"
		endpointHTTPPort            = 80
		endpointAgnhostPort         = 10000
		endpointUDPPort             = 10001
		loadBalancerYaml            = "loadbalancer.yaml"
		bgpAddYaml                  = "bgpAdd.yaml"
		bgpEmptyYaml                = "bgpEmptyAdd.yaml"
		externalClientContainerName = "lbclient"
		externalRouterContainerName = "frr"
	)

	var (
		backendNodeName    string
		nonBackendNodeName string
		namespaceName      = "default"
	)
	f := wrappedTestFramework(svcName)
	ginkgo.BeforeEach(func() {
		nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), f.ClientSet, 2)
		framework.ExpectNoError(err)
		if len(nodes.Items) < 2 {
			e2eskipper.Skipf("Test requires >= 2 Ready nodes, but there are only %v nodes", len(nodes.Items))
		}
		backendNodeName = nodes.Items[0].Name
		nonBackendNodeName = nodes.Items[1].Name
		var loadBalancerServiceConfig = `
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
        image: ` + images.MetalLBLBService() + `
        command: ['sh', '-c', "dd if=/dev/zero of=/usr/share/nginx/html/big.iso  bs=1024 count=0 seek=102400"]
        volumeMounts:
        - name: data
          mountPath: "/usr/share/nginx/html"
      containers:
      - name: nginx
        image: ` + images.Nginx() + `
        volumeMounts:
        - name: data
          mountPath: "/usr/share/nginx/html"
        ports:
        - name: http
          containerPort: 80
      - name: agnhost
        image: ` + images.AgnHost() + `
        command: ["/agnhost", "netexec", "--http-port=10000"]
        ports:
        - name: agnhost
          containerPort: 10000
      - name: udp-server
        image: ` + images.UDPServerSrcIPPrinter() + `
        imagePullPolicy: Always
        ports:
        - containerPort: 10001
          protocol: UDP
          name: udp
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
  - name: agnhost
    port: 10000
    protocol: TCP
    targetPort: 10000
  - name: udp
    port: 10001
    protocol: UDP
    targetPort: 10001
  selector:
    app: nginx
  type: LoadBalancer
`
		if err := os.WriteFile(loadBalancerYaml, []byte(loadBalancerServiceConfig), 0644); err != nil {
			framework.Failf("Unable to write CRD config to disk: %v", err)
		}
		framework.Logf("Create the Load Balancer configuration")
		e2ekubectl.RunKubectlOrDie("default", "create", "-f", loadBalancerYaml)

	})

	ginkgo.JustAfterEach(func() {
		if ginkgo.CurrentSpecReport().Failed() {
			DumpBGPInfo(reportPath, ginkgo.CurrentSpecReport().LeafNodeText, f)
			k8sReporter := InitReporter(framework.TestContext.KubeConfig, reportPath,
				[]string{metallbNamespace, namespaceName})
			DumpInfo(k8sReporter)
		}
	})

	ginkgo.AfterEach(func() {
		framework.Logf("Delete the Load Balancer configuration")
		e2ekubectl.RunKubectlOrDie("default", "delete", "-f", loadBalancerYaml, "--ignore-not-found=true")
		defer func() {
			if err := os.Remove(loadBalancerYaml); err != nil {
				framework.Logf("Unable to remove the CRD config from disk: %v", err)
			}
			framework.Logf("Reset MTU on intermediary router to allow large packets")
			primaryProviderNetwork, err := infraprovider.Get().PrimaryNetwork()
			framework.ExpectNoError(err, "must get primary provider network")
			externalContainer := infraapi.ExternalContainer{Name: externalRouterContainerName, Network: primaryProviderNetwork}
			_, err = infraprovider.Get().ExecExternalContainerCommand(externalContainer, []string{"ip", "link", "set", "mtu", "1500", "dev", "eth1"})
			framework.ExpectNoError(err, "failed to reset MTU on intermediary router")
			framework.Logf("Delete the custom BGP Advertisement configuration")
			e2ekubectl.RunKubectlOrDie("metallb-system", "delete", "bgpadvertisement", "example", "--ignore-not-found=true")
			var bgpEmptyConfig = fmt.Sprintf(`
---
apiVersion: metallb.io/v1beta1
kind: BGPAdvertisement
metadata:
  name: empty
  namespace: metallb-system
`)
			if err := os.WriteFile(bgpEmptyYaml, []byte(bgpEmptyConfig), 0644); err != nil {
				framework.Failf("Unable to write CRD config to disk: %v", err)
			}
			framework.Logf("Re-create the default BGP Advertisement configuration for other tests")
			e2ekubectl.RunKubectlOrDie("metallb-system", "apply", "-f", bgpEmptyYaml)
		}()
		e2ekubectl.RunKubectlOrDie("default", "delete", "eip", "egressip", "--ignore-not-found=true")
		e2ekubectl.RunKubectlOrDie("default", "label", "node", nonBackendNodeName, "k8s.ovn.org/egress-assignable-")
	})

	ginkgo.It("Should ensure connectivity works on an external service when mtu changes in intermediate node", func() {
		err := WaitForServingAndReadyServiceEndpointsNum(context.TODO(), f.ClientSet, namespaceName, svcName, 4, time.Second, time.Second*180)
		framework.ExpectNoError(err, fmt.Sprintf("service: %s never had an endpoint, err: %v", svcName, err))

		time.Sleep(time.Second * 5) // buffer to ensure all rules are created correctly

		svcLoadBalancerIP, err := getServiceLoadBalancerIP(f.ClientSet, namespaceName, svcName)
		framework.ExpectNoError(err, fmt.Sprintf("failed to get service lb ip: %s for %s, err: %v", svcLoadBalancerIP, svcName, err))
		discoveryClient := f.ClientSet.DiscoveryV1()
		endpointSlice, err := discoveryClient.EndpointSlices(namespaceName).List(context.TODO(), metav1.ListOptions{
			LabelSelector: fmt.Sprintf("kubernetes.io/service-name=%s", svcName),
		})
		framework.ExpectNoError(err, fmt.Sprintf("failed to get endpoints slice for service %s", svcName))
		gomega.Expect(endpointSlice).NotTo(gomega.BeNil())
		gomega.Expect(len(endpointSlice.Items)).To(gomega.Equal(1))
		gomega.Expect(len(endpointSlice.Items[0].Endpoints)).To(gomega.Equal(4))
		endPointIP := endpointSlice.Items[0].Endpoints[0].Addresses
		framework.ExpectNoError(err, fmt.Sprintf("failed to get endpoint slice's %s ip address", endPointIP))
		nodeName := *endpointSlice.Items[0].Endpoints[0].NodeName
		nodeIP, err := getNodeIP(f.ClientSet, nodeName)
		framework.ExpectNoError(err, fmt.Sprintf("failed to get endpoint's %s node ip address", nodeIP))

		svcIPforCurl := svcLoadBalancerIP
		if !utilnet.IsIPv6String(svcLoadBalancerIP) {
			ginkgo.By("Setting up external IPv4 client with an intermediate node")
			defer func() {
				cleanupIPv4NetworkForExternalClient(svcLoadBalancerIP, endpointHTTPPort)
			}()
			setupIPv4NetworkForExternalClient(svcLoadBalancerIP, endpointHTTPPort, nodeIP)
		} else {
			ginkgo.By("Setting up external IPv6 client with an intermediate node")
			defer func() {
				cleanupIPv6NetworkForExternalClient(svcLoadBalancerIP, endpointHTTPPort)
			}()
			setupIPv6NetworkForExternalClient(svcLoadBalancerIP, endpointHTTPPort, nodeIP)
			svcIPforCurl = fmt.Sprintf("[%s]", svcLoadBalancerIP)
		}

		ginkgo.By("Test sevice connectivity before changing MTU on the intermediate node")
		// Ensure service connectivity works from external client with default settings.
		// Use Eventually because IPv6 takes a while to finish its network configuration
		// with network namespaces.
		gomega.Eventually(func() error {
			return buildAndRunCommand(fmt.Sprintf("sudo ip netns exec client curl %s:%d/big.iso -o big.iso", svcIPforCurl, endpointHTTPPort))
		}, 5*time.Second).Should(gomega.BeNil(), "failed to connect with external load balancer service")

		// Change MTU size of vmtobridge veth pair and verify service connectivity still works.
		// Set the value >=1280 so that it works for IPv6 as well.
		err = buildAndRunCommand("sudo ip link set vmtobridge mtu 1280")
		framework.ExpectNoError(err, "failed to change mtu size on vmtobridge gateway interface")
		err = buildAndRunCommand("sudo ip netns exec bridge ip link set bridgetovm up mtu 1280")
		framework.ExpectNoError(err, "failed to change mtu size on bridgetovm interface")
		ginkgo.By("Test sevice connectivity after changing MTU on the intermediate node")
		gomega.Eventually(func() error {
			return buildAndRunCommand(fmt.Sprintf("sudo ip netns exec client curl %s:%d/big.iso -o big.iso", svcIPforCurl, endpointHTTPPort))
		}, 5*time.Second).Should(gomega.BeNil(), "failed to connect with external load balancer service after changing mtu size")
	})

	ginkgo.It("Should ensure load balancer service works with pmtud", func() {

		// TEST LOGIC: This test uses metaLB BGP for advertising routes towards the 3 KIND ovnk cluster nodes
		// (control-plane, 2 workers) as potential candidates to reach the load balancer service (192.168.10.0 service VIP).
		// There is also a FRR router that sits in front of the KIND cluster through which traffic flows to the service VIP.
		// External client (name: lbclient container {installation details in kind.sh script}) tries to reach a
		// load balancer service with VIP: 192.168.10.0 that has 4 backends running on one of the nodes in the cluster through
		// the FRR router.
		// -----------------       ------------------      VIP: 192.168.10.0  ---------------------
		// |               | 1500  |                | 1500                    | ovn-control-plane |
		// |   lbclient    |------>|   FRR router   |------> KIND cluster --> ---------------------
		// |               | change|                |                         |    ovn-worker     |   (4 backend CNI pods running
		// -----------------  to   ------------------                         ---------------------    on one of the nodes serving
		//                   1200                                             |    ovn-worker2    |    lb service)
		//              generates ICMP                                        ---------------------
		//                 needs FRAG
		//
		// NOTE: There is no guarantee which node gets picked for serving the request since metalLB uses ECMP.
		// Hence we could either have:
		// A) lbclient->FRR router->ovn-worker->br-ex->GR_ovn-worker->join->cluster-router->ovn-worker-switch->pod OR
		// B) lbclient->FRR router->ovn-worker2->br-ex->GR_ovn-worker2->join->cluster-router-ovn-worker->transit-switch->GENEVE->
		//    transit-switch->cluster-router-ovn-worker->ovn-worker-switch->pod
		// depending on which node is hit for the service traffic and which node the backendpod lives on.
		err := WaitForServingAndReadyServiceEndpointsNum(context.TODO(), f.ClientSet, namespaceName, svcName, 4, time.Second, time.Second*180)
		framework.ExpectNoError(err, fmt.Sprintf("service: %s never had an endpoint, err: %v", svcName, err))

		time.Sleep(time.Second * 5) // buffer to ensure all rules are created correctly

		svcLoadBalancerIP, err := getServiceLoadBalancerIP(f.ClientSet, namespaceName, svcName)
		framework.ExpectNoError(err, fmt.Sprintf("failed to get service lb ip: %s, err: %v", svcName, err))

		primaryProviderNetwork, err := infraprovider.Get().PrimaryNetwork()
		framework.ExpectNoError(err, "must fetch primary provider network")

		ginkgo.By("waiting for BGP route to be installed on the FRR router")
		waitForFRRBGPRoute(svcLoadBalancerIP, externalRouterContainerName, primaryProviderNetwork)

		// curl the LB service from the client container to trigger BGP route advertisement
		ginkgo.By("by sending a TCP packet to service " + svcName + " with type=LoadBalancer in namespace " + namespaceName + " with backend pod " + backendName)
		externalContainer := infraapi.ExternalContainer{Name: "lbclient", Network: primaryProviderNetwork} // pre-created
		_, err = wgetInExternalContainer(externalContainer, svcLoadBalancerIP, endpointHTTPPort, "big.iso")
		framework.ExpectNoError(err, "failed to curl load balancer service")

		ginkgo.By("all 3 nodeIP routes are advertised correctly by metalb BGP routes")
		// sample
		// 192.168.10.0 nhid 84 proto bgp metric 20
		//	nexthop via 172.19.0.3 dev eth0 weight 1
		//	nexthop via 172.19.0.4 dev eth0 weight 1
		//	nexthop via 172.19.0.2 dev eth0 weight 1

		cmd := []string{}
		ipVer := ""
		if utilnet.IsIPv6String(svcLoadBalancerIP) {
			ipVer = " -6"
		}
		bgpRouteCommand := strings.Split(fmt.Sprintf("ip%s route show %s", ipVer, svcLoadBalancerIP), " ")
		cmd = append(cmd, bgpRouteCommand...)

		backendNodeIP, err := getNodeIP(f.ClientSet, backendNodeName)
		framework.ExpectNoError(err, fmt.Sprintf("failed to get node's %s node ip address", backendNodeName))
		nonBackendNodeIP, err := getNodeIP(f.ClientSet, backendNodeName)
		framework.ExpectNoError(err, fmt.Sprintf("failed to get node's %s node ip address", backendNodeName))
		frrExternalContainer := infraapi.ExternalContainer{Name: "frr", Network: primaryProviderNetwork}
		gomega.Eventually(func() bool {
			routes, err := infraprovider.Get().ExecExternalContainerCommand(frrExternalContainer, cmd)
			framework.ExpectNoError(err, "failed to get BGP routes from intermediary router")
			framework.Logf("Routes in FRR %s", routes)
			return strings.Contains(routes, backendNodeIP)
		}, 30*time.Second).Should(gomega.BeTrue())
		gomega.Eventually(func() bool {
			routes, err := infraprovider.Get().ExecExternalContainerCommand(frrExternalContainer, cmd)
			framework.ExpectNoError(err, "failed to get BGP routes from intermediary router")
			framework.Logf("Routes in FRR %s", routes)
			return strings.Contains(routes, nonBackendNodeIP)
		}, 30*time.Second).Should(gomega.BeTrue())

		framework.Logf("Delete the default BGP Advertisement configuration")
		e2ekubectl.RunKubectlOrDie("metallb-system", "delete", "bgpadvertisement", "empty", "--ignore-not-found=true")

		// test CASE A: traffic lands on the same node where backend lives
		// test CASE B: traffic lands on different node than where the backend lives
		for _, node := range []string{backendNodeName, nonBackendNodeName} {
			var bgpConfig = `
---
apiVersion: metallb.io/v1beta1
kind: BGPAdvertisement
metadata:
  name: example
  namespace: metallb-system
spec:
  ipAddressPools:
  - dev-env-bgp
  nodeSelectors:
  - matchLabels:
      kubernetes.io/hostname: ` + node + `
`
			if err := os.WriteFile(bgpAddYaml, []byte(bgpConfig), 0644); err != nil {
				framework.Failf("Unable to write CRD config to disk: %v", err)
			}
			framework.Logf("Create the BGP Advertisement configuration")
			e2ekubectl.RunKubectlOrDie("metallb-system", "apply", "-f", bgpAddYaml)

			ginkgo.By("only 1 nodeIP route is advertised correctly by metalb BGP routes")
			// ensure only this node's IP route is advertised correctly by metalb BGP routes
			// sample:
			// 192.168.10.0 nhid 31 via 172.19.0.4 dev eth0 proto bgp metric 20
			nodeIP, err := getNodeIP(f.ClientSet, node)
			framework.ExpectNoError(err, fmt.Sprintf("failed to get nodes's %s node ip address", node))
			framework.Logf("NodeIP of node %s is %s", node, nodeIP)
			externalContainer := infraapi.ExternalContainer{Name: externalRouterContainerName, Network: primaryProviderNetwork}
			cmd := []string{}
			cmd = append(cmd, bgpRouteCommand...)
			gomega.Eventually(func() bool {
				routes, err := infraprovider.Get().ExecExternalContainerCommand(externalContainer, cmd)
				framework.ExpectNoError(err, "failed to get BGP routes from intermediary router")
				framework.Logf("Routes in FRR %s", routes)
				routeCount := 0
				matchedRoute := ""
				for _, route := range strings.Split(routes, "\n") {
					match := strings.Contains(route, nodeIP)
					if match {
						framework.Logf("DEBUG: Matched route %s for pattern %s", route, nodeIP)
						matchedRoute = route
					}
					if strings.Contains(route, "via") {
						routeCount++
					}
				}
				return routeCount == 1 && strings.Contains(matchedRoute, nodeIP)
			}, 60*time.Second).Should(gomega.BeTrue())

			ginkgo.By("by sending a TCP packet to service " + svcName + " with type=LoadBalancer in namespace " + namespaceName + " with backend pod " + backendName + " via node " + node)

			_, err = wgetInExternalContainer(externalContainer, svcLoadBalancerIP, endpointHTTPPort, "big.iso")
			framework.ExpectNoError(err, "failed to curl load balancer service")

			ginkgo.By("change MTU on intermediary router to force icmp related packets")
			cmd = []string{}
			mtuCommand := strings.Split("ip link set mtu 1280 dev eth1", " ")
			cmd = append(cmd, mtuCommand...)
			_, err = infraprovider.Get().ExecExternalContainerCommand(externalContainer, cmd)
			framework.ExpectNoError(err, "failed to change MTU on intermediary router")

			time.Sleep(time.Second * 5) // buffer to ensure MTU change took effect

			ginkgo.By("by sending a TCP packet to service " + svcName + " with type=LoadBalancer in namespace " + namespaceName + " with backend pod " + backendName + " via node " + node)

			_, err = wgetInExternalContainer(externalContainer, svcLoadBalancerIP, endpointHTTPPort, "big.iso")
			framework.ExpectNoError(err, "failed to curl load balancer service")

			ginkgo.By("reset MTU on intermediary router to allow large packets")
			cmd = []string{}
			mtuCommand = strings.Split("ip link set mtu 1500 dev eth1", " ")
			cmd = append(cmd, mtuCommand...)
			_, err = infraprovider.Get().ExecExternalContainerCommand(externalContainer, cmd)
			framework.ExpectNoError(err, "failed to reset MTU on intermediary router")
		}
	})

	testLBEndpoints := func(externalContainer infraapi.ExternalContainer, svcLoadBalancerIP string, wantEndpoints int) {
		target := fmt.Sprintf("http://%s/hostname", util.JoinHostPortInt32(svcLoadBalancerIP, endpointAgnhostPort))
		gotEndpoints := make(map[string]int)
		// Give the test extra time, to ensure we eventually hit all endpoints
		totalTimeout := retryTimeout * time.Duration(wantEndpoints)
		_ = wait.PollImmediate(retryInterval, totalTimeout, func() (bool, error) {
			endpoint, err := infraprovider.Get().ExecExternalContainerCommand(externalContainer, []string{"curl", "-s", "-g", target})
			if err != nil {
				framework.Logf("retrying curl to %s: %v", svcLoadBalancerIP, err)
				return false, nil
			}
			gotEndpoints[endpoint]++
			if gotEndpoints[endpoint] == 1 {
				framework.Logf("reached endpoint %s", endpoint)
			}
			done := len(gotEndpoints) == wantEndpoints
			return done, nil
		})
		gomega.Expect(len(gotEndpoints)).NotTo(gomega.BeZero(), "failed to reach any endpoint of service %s", svcLoadBalancerIP)
		gomega.Expect(len(gotEndpoints)).To(gomega.Equal(wantEndpoints), "failed to reach the expected number of load balancer endpoints")
	}

	ginkgo.It("Should ensure load balancer service works without node ports when ETP=local", func() {

		err := WaitForServingAndReadyServiceEndpointsNum(context.TODO(), f.ClientSet, namespaceName, svcName, 4, time.Second, time.Second*180)
		framework.ExpectNoError(err, fmt.Sprintf("service: %s never had an enpoint, err: %v", svcName, err))

		svcLoadBalancerIP, err := getServiceLoadBalancerIP(f.ClientSet, namespaceName, svcName)
		framework.ExpectNoError(err, fmt.Sprintf("failed to get service lb ip: %s, err: %v", svcName, err))

		time.Sleep(time.Second * 5) // buffer to ensure all rules are created correctly

		primaryProviderNetwork, err := infraprovider.Get().PrimaryNetwork()
		framework.ExpectNoError(err, "must get primary provider network")

		ginkgo.By("waiting for BGP route to be installed on the FRR router")
		waitForFRRBGPRoute(svcLoadBalancerIP, externalRouterContainerName, primaryProviderNetwork)

		ginkgo.By("by sending TCP packets to service " + svcName + " with type=LoadBalancer and ensuring all endpoints are reached")
		externalContainer := infraapi.ExternalContainer{Name: externalClientContainerName}
		testLBEndpoints(externalContainer, svcLoadBalancerIP, 4)

		ginkgo.By("patching service " + svcName + " to allocateLoadBalancerNodePorts=false and externalTrafficPolicy=local")

		err = patchServiceBoolValue(f.ClientSet, svcName, "default", "/spec/allocateLoadBalancerNodePorts", false)
		framework.ExpectNoError(err)

		output := e2ekubectl.RunKubectlOrDie("default", "get", "svc", svcName, "-o=jsonpath='{.spec.allocateLoadBalancerNodePorts}'")
		gomega.Expect(output).To(gomega.Equal("'false'"))

		err = patchServiceStringValue(f.ClientSet, svcName, "default", "/spec/externalTrafficPolicy", "Local")
		framework.ExpectNoError(err)

		output = e2ekubectl.RunKubectlOrDie("default", "get", "svc", svcName, "-o=jsonpath='{.spec.externalTrafficPolicy}'")
		gomega.Expect(output).To(gomega.Equal("'Local'"))

		time.Sleep(time.Second * 5) // buffer to ensure all rules are created correctly

		ginkgo.By("by sending TCP packets to service " + svcName + " with type=LoadBalancer and ensuring all endpoints are reached")
		testLBEndpoints(externalContainer, svcLoadBalancerIP, 4)

		ginkgo.By("Scale down endpoints of service: " + svcName + " to ensure rules are also getting recreated correctly")
		e2ekubectl.RunKubectlOrDie("default", "scale", "deployment", backendName, "--replicas=3")
		err = e2eendpointslice.WaitForEndpointCount(context.TODO(), f.ClientSet, namespaceName, svcName, 3)
		framework.ExpectNoError(err, fmt.Sprintf("service: %s never had an endpoint, err: %v", svcName, err))
		time.Sleep(time.Second * 5) // buffer to ensure all rules are created correctly

		ginkgo.By("by sending TCP packets to service " + svcName + " with type=LoadBalancer and ensuring all endpoints are reached")
		testLBEndpoints(externalContainer, svcLoadBalancerIP, 3)
	})

	ginkgo.It("Should ensure load balancer service works without node ports when named targetPorts are used and ETP=local", func() {
		err := WaitForServingAndReadyServiceEndpointsNum(context.TODO(), f.ClientSet, namespaceName, svcName, 4, time.Second, time.Second*180)
		framework.ExpectNoError(err, fmt.Sprintf("service: %s never had an endpoint, err: %v", svcName, err))

		svcLoadBalancerIP, err := getServiceLoadBalancerIP(f.ClientSet, namespaceName, svcName)
		framework.ExpectNoError(err, fmt.Sprintf("failed to get service lb ip: %s, err: %v", svcName, err))

		time.Sleep(time.Second * 5) // buffer to ensure all rules are created correctly

		ginkgo.By("by sending TCP packets to service " + svcName + " with type=LoadBalancer and ensuring all endpoints are reached")
		externalContainer := infraapi.ExternalContainer{Name: externalClientContainerName}
		testLBEndpoints(externalContainer, svcLoadBalancerIP, 4)

		// Patch the service to use named ports.
		ginkgo.By("patching service " + svcName + " to named ports")
		err = patchServiceStringValue(f.ClientSet, svcName, "default", "/spec/ports/0/targetPort", "http")
		framework.ExpectNoError(err)
		err = patchServiceStringValue(f.ClientSet, svcName, "default", "/spec/ports/1/targetPort", "agnhost")
		framework.ExpectNoError(err)
		err = patchServiceStringValue(f.ClientSet, svcName, "default", "/spec/ports/2/targetPort", "udp")
		framework.ExpectNoError(err)
		output := e2ekubectl.RunKubectlOrDie("default", "get", "svc", svcName, "-o=jsonpath='{.spec.ports[1].targetPort}'")
		gomega.Expect(output).To(gomega.Equal("'agnhost'"))
		time.Sleep(time.Second * 5) // buffer to ensure all rules are created correctly

		ginkgo.By("by sending TCP packets to service " + svcName + " with type=LoadBalancer and ensuring all endpoints are reached")
		testLBEndpoints(externalContainer, svcLoadBalancerIP, 4)

		// Patch the service to use allocateLoadBalancerNodeProts=false and externalTrafficPolicy=local.
		ginkgo.By("patching service " + svcName + " to allocateLoadBalancerNodePorts=false and externalTrafficPolicy=local")

		err = patchServiceBoolValue(f.ClientSet, svcName, "default", "/spec/allocateLoadBalancerNodePorts", false)
		framework.ExpectNoError(err)

		output = e2ekubectl.RunKubectlOrDie("default", "get", "svc", svcName, "-o=jsonpath='{.spec.allocateLoadBalancerNodePorts}'")
		gomega.Expect(output).To(gomega.Equal("'false'"))

		err = patchServiceStringValue(f.ClientSet, svcName, "default", "/spec/externalTrafficPolicy", "Local")
		framework.ExpectNoError(err)

		output = e2ekubectl.RunKubectlOrDie("default", "get", "svc", svcName, "-o=jsonpath='{.spec.externalTrafficPolicy}'")
		gomega.Expect(output).To(gomega.Equal("'Local'"))

		time.Sleep(time.Second * 5) // buffer to ensure all rules are created correctly

		ginkgo.By("by sending TCP packets to service " + svcName + " with type=LoadBalancer and ensuring all endpoints are reached")
		testLBEndpoints(externalContainer, svcLoadBalancerIP, 4)

		ginkgo.By("Scale down endpoints of service: " + svcName + " to ensure rules are also getting recreated correctly")
		e2ekubectl.RunKubectlOrDie("default", "scale", "deployment", backendName, "--replicas=3")
		err = WaitForServingAndReadyServiceEndpointsNum(context.TODO(), f.ClientSet, namespaceName, svcName, 3, time.Second, time.Second*180)
		framework.ExpectNoError(err, fmt.Sprintf("service: %s never had an endpoint, err: %v", svcName, err))
		time.Sleep(time.Second * 5) // buffer to ensure all rules are created correctly

		ginkgo.By("by sending TCP packets to service " + svcName + " with type=LoadBalancer and ensuring all endpoints are reached")
		testLBEndpoints(externalContainer, svcLoadBalancerIP, 3)
	})

	ginkgo.It("Should ensure load balancer service works when ETP=local and session affinity is set", func() {

		err := WaitForServingAndReadyServiceEndpointsNum(context.TODO(), f.ClientSet, namespaceName, svcName, 4, time.Second, time.Second*180)
		framework.ExpectNoError(err, fmt.Sprintf("service: %s never had an enpoint, err: %v", svcName, err))

		svcLoadBalancerIP, err := getServiceLoadBalancerIP(f.ClientSet, namespaceName, svcName)
		framework.ExpectNoError(err, fmt.Sprintf("failed to get service lb ip: %s, err: %v", svcName, err))

		ginkgo.By("patching service " + svcName + " to externalTrafficPolicy=local")
		err = patchServiceStringValue(f.ClientSet, svcName, "default", "/spec/externalTrafficPolicy", "Local")
		framework.ExpectNoError(err)
		output := e2ekubectl.RunKubectlOrDie("default", "get", "svc", svcName, "-o=jsonpath='{.spec.externalTrafficPolicy}'")
		gomega.Expect(output).To(gomega.Equal("'Local'"))
		time.Sleep(time.Second * 5) // buffer to ensure all rules are created correctly

		ginkgo.By("1 nodeIP route is advertised correctly by metalb BGP routes")
		// since ETP=local; ensure only this node's IP route is advertised correctly by metalb BGP routes
		// sample:
		// 192.168.10.0 nhid 31 via 172.19.0.4 dev eth0 proto bgp metric 20
		nodeIP, err := getNodeIP(f.ClientSet, backendNodeName)
		framework.ExpectNoError(err, fmt.Sprintf("failed to get nodes's %s node ip address", backendNodeName))
		framework.Logf("NodeIP of node %s is %s", backendNodeName, nodeIP)
		primaryProviderNetwork, err := infraprovider.Get().PrimaryNetwork()
		framework.ExpectNoError(err, "must get primary provider network")
		frrExternalContainer := infraapi.ExternalContainer{Name: externalRouterContainerName, Network: primaryProviderNetwork}
		cmd := []string{}
		ipVer := ""
		if utilnet.IsIPv6String(svcLoadBalancerIP) {
			ipVer = " -6"
		}
		bgpRouteCommand := strings.Split(fmt.Sprintf("ip%s route show %s", ipVer, svcLoadBalancerIP), " ")
		cmd = append(cmd, bgpRouteCommand...)

		gomega.Eventually(func() bool {
			routes, err := infraprovider.Get().ExecExternalContainerCommand(frrExternalContainer, cmd)
			framework.ExpectNoError(err, "failed to get BGP routes from intermediary router")
			framework.Logf("Routes in FRR %s", routes)
			routeCount := 0
			matchedRoute := ""
			for _, route := range strings.Split(routes, "\n") {
				match := strings.Contains(route, nodeIP)
				if match {
					framework.Logf("DEBUG: Matched route %s for pattern %s", route, nodeIP)
					matchedRoute = route
				}
				if strings.Contains(route, "via") {
					routeCount++
				}
			}
			return routeCount == 1 && strings.Contains(matchedRoute, nodeIP)
		}, 60*time.Second).Should(gomega.BeTrue())

		ginkgo.By("by sending a UDP packet to service " + svcName + " with type=LoadBalancer in namespace " + namespaceName + " with backend pod " + backendName)
		netcatCmd := fmt.Sprintf("echo hostname | nc -uv -w2 %s %d",
			svcLoadBalancerIP,
			endpointUDPPort,
		)
		cmd = []string{"bash", "-x", "-c", netcatCmd}
		framework.Logf("netcat command %s", cmd)
		clientNetNetwork, err := infraprovider.Get().GetNetwork("clientnet")
		if errors.Is(err, infraapi.NotFound) {
			ginkgo.Skip("clientnet network is not available. Skipping")
		}
		framework.ExpectNoError(err, "clientnet network must be available")
		lbClientExternalContainer := infraapi.ExternalContainer{Name: "lbclient", Network: clientNetNetwork}
		output, err = infraprovider.Get().ExecExternalContainerCommand(lbClientExternalContainer, cmd)
		framework.ExpectNoError(err, "failed to connect to load balancer service")
		framework.Logf("netcat command output %s", output)

		ginkgo.By("ensure the sourceIP of the external container is preserved!")
		// Check that sourceIP of the LBService is preserved
		targetPodLogs, err := e2ekubectl.RunKubectl("default", "logs", "-l", "app=nginx", "--container", "udp-server")
		framework.ExpectNoError(err, "failed to inspect logs in backend pods")
		framework.Logf("Target pod logs (nginx): %q", targetPodLogs)
		lbClientInf, err := infraprovider.Get().GetExternalContainerNetworkInterface(lbClientExternalContainer, clientNetNetwork)
		framework.ExpectNoError(err, "failed to get network interface info for network %s within lbclient external container %s", clientNetNetwork.Name(), frrExternalContainer.GetName())
		framework.Logf("found external container IPv4: %q, IPv6: %q", lbClientInf.IPv4, lbClientInf.IPv6)
		if strings.Contains(targetPodLogs, lbClientInf.IPv4) {
			framework.Logf("found the expected srcIP %s!", lbClientInf.IPv4)
		} else if strings.Contains(targetPodLogs, lbClientInf.IPv6) {
			framework.Logf("found the expected srcIP %s!", lbClientInf.IPv6)
		} else {
			framework.Failf("could not get expected srcIP!, target pod logs:\n%q", targetPodLogs)
		}

		ginkgo.By("patching service " + svcName + " to sessionAffinity=ClientIP at default timeout of 10800")
		err = patchServiceStringValue(f.ClientSet, svcName, "default", "/spec/sessionAffinity", "ClientIP")
		framework.ExpectNoError(err)
		output = e2ekubectl.RunKubectlOrDie("default", "get", "svc", svcName, "-o=jsonpath='{.spec.sessionAffinity}'")
		gomega.Expect(output).To(gomega.Equal("'ClientIP'"))
		output = e2ekubectl.RunKubectlOrDie("default", "get", "svc", svcName, "-o=jsonpath='{.spec.sessionAffinityConfig.clientIP.timeoutSeconds}'")
		gomega.Expect(output).To(gomega.Equal("'10800'"))
		time.Sleep(time.Second * 5) // buffer to ensure all rules are created correctly

		ginkgo.By("by sending a UDP packet to service " + svcName + " with type=LoadBalancer in namespace " + namespaceName + " with backend pod " + backendName)
		// OVN drops the 1st packet so this one does nothing basically.
		// See https://issues.redhat.com/browse/FDP-223 for details
		output, err = infraprovider.Get().ExecExternalContainerCommand(lbClientExternalContainer, cmd)
		framework.ExpectNoError(err, "failed to connect to load balancer service")
		framework.Logf("netcat command output %s", output)
		time.Sleep(time.Second * 10) // buffer to ensure all learn flows are created correctly after the previous drop

		// OVN drops the 1st packet so let's be sure to another set of netcat connections at least to check the srcIP
		output, err = infraprovider.Get().ExecExternalContainerCommand(lbClientExternalContainer, cmd)
		framework.ExpectNoError(err, "failed to connect to load balancer service")
		framework.Logf("netcat command output %s", output)

		// Check that sourceIP of the LBService is preserved
		ginkgo.By("ensure the sourceIP of the external container is preserved!")
		targetPodLogs, err = e2ekubectl.RunKubectl("default", "logs", "-l", "app=nginx", "--container", "udp-server")
		framework.ExpectNoError(err, "failed to inspect logs in backend pods")
		framework.Logf("%v", targetPodLogs)
		if strings.Count(targetPodLogs, lbClientInf.IPv4) >= 2 {
			framework.Logf("found the expected srcIP %s!", lbClientInf.IPv4)
		} else if strings.Count(targetPodLogs, lbClientInf.IPv6) >= 2 {
			framework.Logf("found the expected srcIP %s!", lbClientInf.IPv6)
		} else {
			framework.Failf("could not get expected srcIP!, target pod logs:\n%q", targetPodLogs)
		}
	})
	ginkgo.It("Should ensure load balancer service works when ETP=local and backend pods are also egressIP served pods", func() {
		// TEST LOGIC: This test uses metaLB BGP for advertising routes towards 1 KIND ovnk cluster node
		// (node where the service backend pods live) as potential candidate to reach the load balancer service (192.168.10.0 service VIP).
		// There is also a FRR router that sits in front of the KIND cluster through which traffic flows to the service VIP.
		// External client (name: lbclient container {installation details in kind.sh script}) tries to reach a
		// load balancer service with VIP: 192.168.10.0 that has 4 backends running on one of the nodes in the cluster through
		// the FRR router.
		// -----------------       ------------------      VIP: 192.168.10.0  ---------------------
		// |               |       |                |                         | ovn-control-plane |
		// |   lbclient    |------>|   FRR router   |------> KIND cluster --> ---------------------
		// |               |       |                |                         |    ovn-worker     |   (4 backend CNI pods running
		// -----------------       ------------------                         ---------------------    on one of the nodes - say on ovn-worker -
		//                                                                    |    ovn-worker2    |    serving lb service; they are also
		//                                                                    ---------------------    served by EIP on primary network)
		//
		// So as an example here ovn-worker is where pods live and pods are served by EIP which is assigned on ovn-worker2
		// Now we test ETP=local works as expected without EIP re-routes messing with the reply traffic:
		// lbclient->FRR router->ovn-worker->br-ex->GR_ovn-worker->join->cluster-router->ovn-worker-switch->pod and response goes back
		// same way without it getting re-routed to egressNode ovn-worker2
		err := e2eendpointslice.WaitForEndpointCount(context.TODO(), f.ClientSet, namespaceName, svcName, 4)
		framework.ExpectNoError(err, fmt.Sprintf("service: %s never had an enpoint, err: %v", svcName, err))

		svcLoadBalancerIP, err := getServiceLoadBalancerIP(f.ClientSet, namespaceName, svcName)
		framework.ExpectNoError(err, fmt.Sprintf("failed to get service lb ip: %s, err: %v", svcName, err))

		ginkgo.By("patching service " + svcName + " to externalTrafficPolicy=local")
		err = patchServiceStringValue(f.ClientSet, svcName, "default", "/spec/externalTrafficPolicy", "Local")
		framework.ExpectNoError(err)
		output := e2ekubectl.RunKubectlOrDie("default", "get", "svc", svcName, "-o=jsonpath='{.spec.externalTrafficPolicy}'")
		gomega.Expect(output).To(gomega.Equal("'Local'"))
		time.Sleep(time.Second * 5) // buffer to ensure all rules are created correctly

		ginkgo.By("1 nodeIP route is advertised correctly by metalb BGP routes")
		// since ETP=local; ensure only this node's IP route is advertised correctly by metalb BGP routes
		// sample:
		// 192.168.10.0 nhid 31 via 172.19.0.4 dev eth0 proto bgp metric 20
		nodeIP, err := getNodeIP(f.ClientSet, backendNodeName)
		framework.ExpectNoError(err, fmt.Sprintf("failed to get nodes's %s node ip address", backendNodeName))
		framework.Logf("NodeIP of node %s is %s", backendNodeName, nodeIP)
		ipVer := ""
		if utilnet.IsIPv6String(svcLoadBalancerIP) {
			ipVer = " -6"
		}
		bgpRouteCommand := strings.Split(fmt.Sprintf("ip%s route show %s", ipVer, svcLoadBalancerIP), " ")
		primaryProviderNetwork, err := infraprovider.Get().PrimaryNetwork()
		framework.ExpectNoError(err, "must fetch primary provider network")
		frrExternalContainer := infraapi.ExternalContainer{Name: "frr", Network: primaryProviderNetwork}
		gomega.Eventually(func() bool {
			routes, err := infraprovider.Get().ExecExternalContainerCommand(frrExternalContainer, bgpRouteCommand)
			framework.ExpectNoError(err, "failed to get BGP routes from intermediary router")
			framework.Logf("Routes in FRR %s", routes)
			routeCount := 0
			matchedRoute := ""
			for _, route := range strings.Split(routes, "\n") {
				match := strings.Contains(route, nodeIP)
				if match {
					framework.Logf("DEBUG: Matched route %s for pattern %s", route, nodeIP)
					matchedRoute = route
				}
				if strings.Contains(route, "via") {
					routeCount++
				}
			}
			return routeCount == 1 && strings.Contains(matchedRoute, nodeIP)
		}, 60*time.Second).Should(gomega.BeTrue())

		ginkgo.By("by sending a UDP packet to service " + svcName + " with type=LoadBalancer in namespace " + namespaceName + " with backend pod " + backendName)
		netcatCmd := fmt.Sprintf("echo hostname | nc -uv -w2 %s %d",
			svcLoadBalancerIP,
			endpointUDPPort,
		)
		cmd := []string{"bash", "-x", "-c", netcatCmd}
		framework.Logf("netcat command %s", cmd)
		clientNetNetwork, err := infraprovider.Get().GetNetwork("clientnet")
		if errors.Is(err, infraapi.NotFound) {
			ginkgo.Skip("clientnet network is not available. Skipping")
		}
		framework.ExpectNoError(err, "clientnet network must be available")
		lbClientExternalContainer := infraapi.ExternalContainer{Name: "lbclient", Network: clientNetNetwork}
		output, err = infraprovider.Get().ExecExternalContainerCommand(lbClientExternalContainer, cmd)
		framework.ExpectNoError(err, "failed to connect to load balancer service")
		framework.Logf("netcat command output %s", output)

		ginkgo.By("ensure the sourceIP of the external container is preserved!")
		// Check that sourceIP of the LBService is preserved
		targetPodLogs, err := e2ekubectl.RunKubectl("default", "logs", "-l", "app=nginx", "--container", "udp-server")
		framework.ExpectNoError(err, "failed to inspect logs in backend pods")
		framework.Logf("Target pod logs:\n%q", targetPodLogs)
		lbClientNetworkInterface, err := infraprovider.Get().GetExternalContainerNetworkInterface(lbClientExternalContainer, clientNetNetwork)
		framework.ExpectNoError(err, "failed to get network interface info for external container %s connected to network %s", frrExternalContainer.Name, clientNetNetwork.Name())
		lbClientIPv4, lbClientIPv6 := lbClientNetworkInterface.IPv4, lbClientNetworkInterface.IPv6
		framework.Logf("found external container lbclient IPs: IPv4 %q, IPv6: %q", lbClientIPv4, lbClientIPv6)
		if strings.Contains(targetPodLogs, lbClientIPv4) {
			framework.Logf("found the expected srcIP %s!", lbClientIPv4)
		} else if strings.Contains(targetPodLogs, lbClientIPv6) {
			framework.Logf("found the expected srcIP %s!", lbClientIPv6)
		} else {
			framework.Failf("could not get expected srcIP!, target pod logs:\n%q", targetPodLogs)
		}

		ginkgo.By("label " + nonBackendNodeName + " as egressIP assignable")
		e2enode.AddOrUpdateLabelOnNode(f.ClientSet, nonBackendNodeName, "k8s.ovn.org/egress-assignable", "dummy")

		ginkgo.By("Create an EgressIP object with one egress IP defined")
		// Assign the egress IP without conflicting with any node IP,
		// the kind subnet is /16 or /64 so the following should be fine.
		var egressIP1 net.IP
		if utilnet.IsIPv6String(svcLoadBalancerIP) {
			egressIP1, err = ipalloc.NewPrimaryIPv6()
		} else {
			egressIP1, err = ipalloc.NewPrimaryIPv4()
		}
		framework.ExpectNoError(err, "must allocate new Node IP for EgressIP IP")
		var egressIPConfig = `apiVersion: k8s.ovn.org/v1
kind: EgressIP
metadata:
    name: ` + "egressip" + `
spec:
    egressIPs:
    - ` + egressIP1.String() + `
    podSelector:
        matchLabels:
            app: nginx
    namespaceSelector:
        matchLabels:
            kubernetes.io/metadata.name: ` + namespaceName + `
`
		if err := os.WriteFile("egressip.yaml", []byte(egressIPConfig), 0644); err != nil {
			framework.Failf("Unable to write CRD config to disk: %v", err)
		}
		ginkgo.DeferCleanup(func() error {
			e2ekubectl.RunKubectlOrDie("default", "delete", "-f", "egressip.yaml", "--ignore-not-found=true")
			if err := os.Remove("egressip.yaml"); err != nil {
				return fmt.Errorf("unable to remove the CRD config from disk: %v", err)
			}
			return nil
		})

		framework.Logf("Create the EgressIP configuration")
		e2ekubectl.RunKubectlOrDie("default", "create", "-f", "egressip.yaml")

		ginkgo.By("4. Check that the status is of length one and that it is assigned to " + nonBackendNodeName)
		err = wait.PollImmediate(retryInterval, retryTimeout, func() (bool, error) {
			egressIP := egressIPs{}
			egressIPStdout, err := e2ekubectl.RunKubectl("default", "get", "eip", "-o", "json")
			if err != nil {
				framework.Logf("Error: failed to get the EgressIP object, err: %v", err)
				return false, nil
			}
			json.Unmarshal([]byte(egressIPStdout), &egressIP)
			if len(egressIP.Items) > 1 {
				framework.Failf("Didn't expect to retrieve more than one egress IP during the execution of this test, saw: %v", len(egressIP.Items))
			}
			if len(egressIP.Items[0].Status.Items) == 0 {
				framework.Logf("EgressIP %s is not assigned to node %s yet", egressIP1.String(), nonBackendNodeName)
				return false, nil
			}
			return egressIP.Items[0].Status.Items[0].Node == nonBackendNodeName, nil
		})
		if err != nil {
			framework.Failf("Error: expected to have 1 egress IP assignment")
		}

		ginkgo.By("by sending a UDP packet to service " + svcName + " with type=LoadBalancer in namespace " + namespaceName + " with backend pod " + backendName)
		output, err = infraprovider.Get().ExecExternalContainerCommand(lbClientExternalContainer, cmd)
		framework.ExpectNoError(err, "failed to connect to load balancer service")
		framework.Logf("netcat command output %s", output)

		// Check that sourceIP of the LBService is preserved
		ginkgo.By("ensure the sourceIP of the external container is preserved!")
		targetPodLogs, err = e2ekubectl.RunKubectl("default", "logs", "-l", "app=nginx", "--container", "udp-server")
		framework.ExpectNoError(err, "failed to inspect logs in backend pods")
		framework.Logf("%v", targetPodLogs)
		if strings.Count(targetPodLogs, lbClientIPv4) >= 2 {
			framework.Logf("found the expected srcIP %s!", lbClientIPv4)
		} else if strings.Count(targetPodLogs, lbClientIPv6) >= 2 {
			framework.Logf("found the expected srcIP %s!", lbClientIPv6)
		} else {
			framework.Failf("could not get expected srcIP!, target pod logs:\n%q", targetPodLogs)
		}
	})
})

// waitForFRRBGPRoute waits for a BGP route for the given service VIP to be
// installed in the external FRR router's kernel routing table.
func waitForFRRBGPRoute(svcLoadBalancerIP string, frrContainerName string, providerNetwork infraapi.Network) {
	ipVer := ""
	if utilnet.IsIPv6String(svcLoadBalancerIP) {
		ipVer = " -6"
	}
	cmd := strings.Split(fmt.Sprintf("ip%s route show %s", ipVer, svcLoadBalancerIP), " ")
	frrContainer := infraapi.ExternalContainer{Name: frrContainerName, Network: providerNetwork}
	gomega.Eventually(func() bool {
		routes, err := infraprovider.Get().ExecExternalContainerCommand(frrContainer, cmd)
		if err != nil {
			framework.Logf("Failed to get routes from FRR: %v", err)
			return false
		}
		framework.Logf("FRR routes for %s: %s", svcLoadBalancerIP, routes)
		return strings.Contains(routes, "proto bgp")
	}, 30*time.Second, 1*time.Second).Should(gomega.BeTrue(), "BGP route for %s was not installed on the FRR router", svcLoadBalancerIP)
}

func getNodeIP(c clientset.Interface, nodeName string) (string, error) {
	node, err := c.CoreV1().Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	ips := e2enode.GetAddresses(node, v1.NodeInternalIP)
	return ips[0], nil
}

func buildAndRunCommand(command string) error {
	cmd := strings.Split(command, " ")
	output, err := exec.Command(cmd[0], cmd[1:]...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to run %q: %s (%s)", strings.Join(cmd, " "), err, output)
	}
	return err
}

func getServiceLoadBalancerIP(c clientset.Interface, namespace, serviceName string) (string, error) {
	svc, err := c.CoreV1().Services(namespace).Get(context.TODO(), serviceName, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	if len(svc.Status.LoadBalancer.Ingress) != 1 {
		return "", fmt.Errorf("service %s has no load balancer IPs", serviceName)
	}
	return svc.Status.LoadBalancer.Ingress[0].IP, nil
}

func setupIPv4NetworkForExternalClient(svcLoadBalancerIP string, svcLoadBalancerPort int, nodeIP string) {
	// The external client configuration done in install_metallb can not be used because routes for external client
	// installed in K8s node https://github.com/ovn-kubernetes/ovn-kubernetes/blob/master/contrib/kind.sh#L1045-L1047
	// are ignored in shared gateway mode and traffic coming back from pod is put on the docker bridge directly by
	// br-ex flows which needs to be handled in host(or vm) network.
	// Hence the following set of ip commands set up two networks called bridge (192.168.222.0/24) and
	// client 192.168.223.0/24 on the host(or vm) machine. The external client (192.168.223.2) runs on the client
	// network which tries to connect with load balancer service via bridge network. There would be also a route
	// created for the load balancer service forwarding packet into the node which hosts one of the endpoint.
	//                                     +------------------------------------+
	//                                     |         kind-ovn-cluster           |
	//                                     |                                    |
	//                                     |                                    |
	//                                     +----------------+-------------------+
	//                                                      |
	//                                                      |
	//                                                      |
	//                                                      |
	//                                                      |
	//                   +----------------------------------+-------------------------------------------+
	//                   |                                  172.18.0.1                                  |
	//                   |                                                     ip route add 192.168.223.0/24 via 192.168.222.2
	//                   |                                                     ip route add <svc-ip> via|<endpoint-node-ip>
	//                   |                                                     iptables -t filter -I FORWARD -d <svc-ip> -p tcp -m tcp --dport <svc-port> -j ACCEPT
	//                   |                                                                              |
	//                   |  vm                                    192.168.222.1                         |
	//                   +----------------------------------------+-------------------------------------+
	//                                                            |
	//                                                            |
	//                +-----------------------+         +---------+-----------------+
	//                |                       |         |         192.168.222.2     |
	//                |                       |         |                           |
	//                |          192.168.223.2+---------+ 192.168.223.1             |
	//                |  client               |         |                     bridge|
	//                +-----------------------+         +---------------------------+
	setupNetNamespaceAndLinks()
	err := buildAndRunCommand("sudo ip addr add 192.168.222.1/24 dev vmtobridge")
	framework.ExpectNoError(err, "failed to add ip address on vmtobridge gateway interface")
	err = buildAndRunCommand("sudo ip netns exec bridge ip addr add 192.168.222.2/24 dev bridgetovm")
	framework.ExpectNoError(err, "failed to add ip address on bridgetovm interface")
	err = buildAndRunCommand("sudo ip netns exec bridge ip addr add 192.168.223.1/24 dev bridgetoclient")
	framework.ExpectNoError(err, "failed to add ip address on bridgetoclient gateway interface")
	err = buildAndRunCommand("sudo ip netns exec client ip addr add 192.168.223.2/24 dev clienttobridge")
	framework.ExpectNoError(err, "failed to add ip address on clienttobridge interface")

	err = buildAndRunCommand("sudo ip netns exec client ip route add default via 192.168.223.1")
	framework.ExpectNoError(err, "failed to add default route on client netns")
	err = buildAndRunCommand("sudo ip netns exec bridge ip route add default via 192.168.222.1")
	framework.ExpectNoError(err, "failed to add default route on bridge netns")

	buildAndRunCommand("sudo ip route delete 192.168.223.0/24")
	err = buildAndRunCommand("sudo ip route add 192.168.223.0/24 via 192.168.222.2")
	framework.ExpectNoError(err, "failed to add route for client to handle reverse service traffic")

	err = buildAndRunCommand(fmt.Sprintf("sudo iptables -t filter -I FORWARD -d %s -p tcp -m tcp --dport %d -j ACCEPT", svcLoadBalancerIP, svcLoadBalancerPort))
	framework.ExpectNoError(err, "failed to add iptables rule for service")

	err = buildAndRunCommand(fmt.Sprintf("sudo ip route add %s via %s", svcLoadBalancerIP, nodeIP))
	framework.ExpectNoError(err, "failed to add route for external load balancer service")
}

func cleanupIPv4NetworkForExternalClient(svcLoadBalancerIP string, svcLoadBalancerPort int) {
	cleanupNetNamespace()
	buildAndRunCommand("sudo ip route delete 192.168.223.0/24 via 192.168.222.2")
	buildAndRunCommand(fmt.Sprintf("sudo ip route delete %s", svcLoadBalancerIP))
	buildAndRunCommand(fmt.Sprintf("sudo iptables -t filter -D FORWARD -d %s -p tcp -m tcp --dport %d -j ACCEPT", svcLoadBalancerIP, svcLoadBalancerPort))
}

func setupIPv6NetworkForExternalClient(svcLoadBalancerIP string, svcLoadBalancerPort int, nodeIP string) {
	// The external client configuration done in install_metallb can not be used because routes for external client
	// installed in K8s node https://github.com/ovn-kubernetes/ovn-kubernetes/blob/master/contrib/kind.sh#L1045-L1047
	// are ignored in shared gateway mode and traffic coming back from pod is put on the docker bridge directly by
	// br-ex flows which needs to be handled in host(or vm) network.
	// Hence the following set of ip -6 commands set up two IPv6 networks called bridge (fc00:f853:ccd:e222::0/64) and
	// client fc00:f853:ccd:e223::0/64 on the host(or vm) machine. The external client (fc00:f853:ccd:e223::2) runs on the client
	// network which tries to connect with load balancer service via bridge network. There would be also a route
	// created for the load balancer service forwarding packet into the node which hosts one of the endpoint.
	//                                               +------------------------------+
	//                                               |        kind-ovn-cluster      |
	//                                               +---------------+--------------+
	//                                                               |
	//                                     +-------------------------+----------------------------------------------+
	//                                     |                      fc00:f853:ccd:e793::1                             |
	//                                     |                                       ip -6 route add fc00:f853:ccd:e223::2 dev vmtobridge via fc00:f853:ccd:e222::2
	//                                     |                                       ip -6 route add %s via %s", svcLoadBalancerIP, nodeIP
	//                                     |        vm                                                              |
	//                                     +-----------------------------------------------------------+------------+
	//                                                                                                 | fc00:f853:ccd:e222::1/64
	//                                                                                                 |
	//                                                                                                 | fc00:f853:ccd:e222::2/64
	//              +--------------------------------------------+                              +------+-------------+
	//              |                                            |                              | ip -6 route add default dev bridgetovm via fc00:f853:ccd:e222::1
	//              |                             fc00:f853:ccd:e223::2/64----------------fc00:f853:ccd:e223::1/64   |
	//              |     client                                 |                              |           bridge   |
	//              |ip -6 route add default dev clienttobridge via fc00:f853:ccd:e223::1       +--------------------+
	//              +--------------------------------------------+
	setupNetNamespaceAndLinks()
	err := buildAndRunCommand("sudo ip -6 addr add fc00:f853:ccd:e222::1/64 dev vmtobridge")
	framework.ExpectNoError(err, "failed to add ip address on vmtobridge gateway interface")
	err = buildAndRunCommand("sudo ip netns exec bridge ip -6 addr add fc00:f853:ccd:e222::2/64 dev bridgetovm")
	framework.ExpectNoError(err, "failed to add ip address on bridgetovm interface")
	err = buildAndRunCommand("sudo ip netns exec bridge ip -6 addr add fc00:f853:ccd:e223::1/64 dev bridgetoclient")
	framework.ExpectNoError(err, "failed to add ip address on bridgetoclient gateway interface")
	err = buildAndRunCommand("sudo ip netns exec client ip -6 addr add fc00:f853:ccd:e223::2/64 dev clienttobridge")
	framework.ExpectNoError(err, "failed to add ip address on clienttobridge interface")

	err = buildAndRunCommand("sudo ip netns exec bridge sysctl -w net.ipv6.conf.all.forwarding=1")
	framework.ExpectNoError(err, "failed to enable ipv6 packet forwarding on bridge net namespace")

	err = buildAndRunCommand("sudo ip netns exec client ip -6 route add default dev clienttobridge via fc00:f853:ccd:e223::1")
	framework.ExpectNoError(err, "failed to add default route on client netns")
	err = buildAndRunCommand("sudo ip netns exec bridge ip -6 route add default dev bridgetovm via fc00:f853:ccd:e222::1")
	framework.ExpectNoError(err, "failed to add default route on bridge netns")

	err = buildAndRunCommand("sudo ip -6 route add fc00:f853:ccd:e223::2 dev vmtobridge via fc00:f853:ccd:e222::2")
	framework.ExpectNoError(err, "failed to add route for client to handle reverse service traffic")

	err = buildAndRunCommand(fmt.Sprintf("sudo ip -6 route add %s via %s", svcLoadBalancerIP, nodeIP))
	framework.ExpectNoError(err, "failed to add route for external load balancer service")

	err = buildAndRunCommand(fmt.Sprintf("sudo ip6tables -t filter -I FORWARD -d %s -p tcp -m tcp --dport %d -j ACCEPT", svcLoadBalancerIP, svcLoadBalancerPort))
	framework.ExpectNoError(err, "failed to add iptables rule for service")
}

func cleanupIPv6NetworkForExternalClient(svcLoadBalancerIP string, svcLoadBalancerPort int) {
	cleanupNetNamespace()
	buildAndRunCommand("sudo ip -6 route delete fc00:f853:ccd:e223::2")
	buildAndRunCommand(fmt.Sprintf("sudo ip -6 route delete %s", svcLoadBalancerIP))
	buildAndRunCommand(fmt.Sprintf("sudo ip6tables -t filter -D FORWARD -d %s -p tcp -m tcp --dport %d -j ACCEPT", svcLoadBalancerIP, svcLoadBalancerPort))
}

func setupNetNamespaceAndLinks() {
	err := buildAndRunCommand("sudo ip netns add bridge")
	framework.ExpectNoError(err, "failed to add brige network namespace")
	err = buildAndRunCommand("sudo ip netns add client")
	framework.ExpectNoError(err, "failed to add client network namespace")

	err = buildAndRunCommand("sudo ip link add vmtobridge type veth peer name bridgetovm")
	framework.ExpectNoError(err, "failed to add veth pair for bridge")
	err = buildAndRunCommand("sudo ip link add clienttobridge type veth peer name bridgetoclient")
	framework.ExpectNoError(err, "failed to add veth pair for client")
	err = buildAndRunCommand("sudo ip link set bridgetovm netns bridge")
	framework.ExpectNoError(err, "failed to move bridgetovm into bridge netns")
	err = buildAndRunCommand("sudo ip link set bridgetoclient netns bridge")
	framework.ExpectNoError(err, "failed to move bridgetoclient into bridge netns")
	err = buildAndRunCommand("sudo ip link set clienttobridge netns client")
	framework.ExpectNoError(err, "failed to move clienttobridge into client netns")

	err = buildAndRunCommand("sudo ip link set vmtobridge up")
	framework.ExpectNoError(err, "failed to get vmtobridge up")
	err = buildAndRunCommand("sudo ip netns exec bridge ip link set bridgetovm up")
	framework.ExpectNoError(err, "failed to get bridgetovm up")

	err = buildAndRunCommand("sudo ip netns exec bridge ip link set bridgetoclient up")
	framework.ExpectNoError(err, "failed to get bridgetoclient up")
	err = buildAndRunCommand("sudo ip netns exec client ip link set clienttobridge up")
	framework.ExpectNoError(err, "failed to get clienttobridge up")
}

func cleanupNetNamespace() {
	buildAndRunCommand("sudo ip netns delete bridge")
	buildAndRunCommand("sudo ip netns delete client")
}

// WaitForServingAndReadyServiceEndpointsNum waits until there are EndpointSlices for serviceName
// containing a total of expectNum endpoints which are in both serving and ready state.
// (If the service is dual-stack, expectNum must count the endpoints of both IP families.)
func WaitForServingAndReadyServiceEndpointsNum(ctx context.Context, c clientset.Interface, namespace, serviceName string, expectNum int, interval, timeout time.Duration) error {
	return wait.PollUntilContextTimeout(ctx, interval, timeout, false, func(ctx context.Context) (bool, error) {
		framework.Logf("Waiting for amount of service:%s endpoints to be %d", serviceName, expectNum)
		esList, err := c.DiscoveryV1().EndpointSlices(namespace).List(ctx, metav1.ListOptions{LabelSelector: fmt.Sprintf("%s=%s", discoveryv1.LabelServiceName, serviceName)})
		if err != nil {
			framework.Logf("Unexpected error trying to get EndpointSlices for %s : %v", serviceName, err)
			return false, nil
		}

		if len(esList.Items) == 0 {
			if expectNum == 0 {
				return true, nil
			}
			framework.Logf("Waiting for at least 1 EndpointSlice to exist")
			return false, nil
		}

		ready := countServingAndReadyEndpointsSlicesNum(esList)
		if ready != expectNum {
			framework.Logf("Unexpected number of Serving And Ready Endpoints on Slices, got %d, expected %d", ready, expectNum)
			return false, nil
		}
		return true, nil
	})
}

func countServingAndReadyEndpointsSlicesNum(epList *discoveryv1.EndpointSliceList) int {
	// Only count unique addresses that are Ready and not Terminating
	addresses := sets.New[string]()
	for _, epSlice := range epList.Items {
		addresses = addresses.Union(getServingAndReadyEndpointSliceAddresses(epSlice))
	}
	return addresses.Len()
}

func getServingAndReadyEndpointSliceAddresses(epSlice discoveryv1.EndpointSlice) sets.Set[string] {
	addresses := sets.New[string]()
	for _, ep := range epSlice.Endpoints {
		cond := ep.Conditions
		ready := cond.Ready == nil || *cond.Ready
		serving := cond.Serving == nil || *cond.Serving
		terminating := cond.Terminating != nil && *cond.Terminating
		if !ready || !serving || terminating {
			continue
		}
		if len(ep.Addresses) == 0 || ep.Addresses[0] == "" {
			continue
		}
		addresses.Insert(ep.Addresses[0])
	}
	return addresses
}

// hostnameMatchesNode matches an agnhost hostname against a node name,
// accepting either the full node name or its short hostname.
func hostnameMatchesNode(hostname, nodeName string) bool {
	return hostname == nodeName || hostname == strings.Split(nodeName, ".")[0]
}
