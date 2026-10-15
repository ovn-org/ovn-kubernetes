// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"text/template"
	"time"

	iputils "github.com/containernetworking/plugins/pkg/ip"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	rav1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1"
	raclientset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1/apis/clientset/versioned"
	apitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/types"
	udnv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	udnclientset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/clientset/versioned"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	ovnutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/allocators"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
	deploymentconfigapi "github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig/api"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/feature"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/images"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider"
	infraapi "github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/api"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/kubectl/pkg/util/podutils"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2epodoutput "k8s.io/kubernetes/test/e2e/framework/pod/output"
	e2eservice "k8s.io/kubernetes/test/e2e/framework/service"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	utilnet "k8s.io/utils/net"
	"k8s.io/utils/ptr"
)

const (
	echoClientPodName                       = "echo-client-pod"
	netexecPort                             = 8080
	defaultNetworkRouteAdvertisementNameEnv = "OVN_TEST_DEFAULT_NETWORK_RA_NAME"
)

// Names of the external BGP scaffolding deployed by contrib/kind.sh or
// contrib/kind-helm.sh. Overridable so that suites can run against clusters
// whose scaffolding uses non-default names (e.g. the dpu-sim environment,
// which coexists with a regular kind cluster on the same machine).
var (
	bgpExternalServerContainerName = envOrDefault("OVN_TEST_BGP_SERVER_CONTAINER", "bgpserver")
	routerContainerName            = envOrDefault("OVN_TEST_BGP_ROUTER_CONTAINER", "frr")
	bgpExternalNetworkName         = envOrDefault("OVN_TEST_BGP_SERVER_NETWORK", "bgpnet")
)

func defaultNetworkRouteAdvertisementName() string {
	if name := os.Getenv(defaultNetworkRouteAdvertisementNameEnv); name != "" {
		return name
	}
	return "default"
}

var _ = ginkgo.Describe("BGP: When default podNetwork is advertised", feature.RouteAdvertisements, func() {
	var serverContainerIPs []string
	var frrContainerIPv4, frrContainerIPv6 string
	var nodes *corev1.NodeList
	f := wrappedTestFramework("pod2external-route-advertisements")
	defaultNetworkRAName := defaultNetworkRouteAdvertisementName()

	ginkgo.BeforeEach(func() {
		serverContainerIPs = getBGPServerContainerIPs(f)
		framework.Logf("The external server IPs are: %+v", serverContainerIPs)
		providerPrimaryNetwork, err := infraprovider.Get().PrimaryNetwork()
		framework.ExpectNoError(err, "provider primary network must be available")
		externalContainerNetInf, err := infraprovider.Get().GetExternalContainerNetworkInterface(infraapi.ExternalContainer{Name: routerContainerName}, providerPrimaryNetwork)
		framework.ExpectNoError(err, "external container %s network %s information must be available", routerContainerName, providerPrimaryNetwork.Name())
		frrContainerIPv4, frrContainerIPv6 = externalContainerNetInf.IPv4, externalContainerNetInf.IPv6
		framework.Logf("The frr router container IPs are: %s/%s", frrContainerIPv4, frrContainerIPv6)
	})

	ginkgo.When("a client ovnk pod is created", func() {

		var clientPod, hostNetworkedPod *corev1.Pod
		var clientPodNodeName string
		var err error

		ginkgo.BeforeEach(func() {
			if !isDefaultNetworkAdvertised(defaultNetworkRAName) {
				e2eskipper.Skipf(
					"skipping pod to external server tests when podNetwork is not advertised",
				)
			}
			ginkgo.By("Selecting 3 schedulable nodes")
			nodes, err = e2enode.GetBoundedReadySchedulableNodes(context.TODO(), f.ClientSet, 3)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(len(nodes.Items)).To(gomega.BeNumerically(">", 2))

			ginkgo.By("Selecting node for client pod")
			clientPodNodeName = nodes.Items[1].Name

			ginkgo.By("Creating client pod")
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

			gomega.Expect(len(serverContainerIPs)).To(gomega.BeNumerically(">", 0))
		})
		// -----------------               ------------------                         ---------------------
		// |               | 172.26.0.0/16 |                |       172.18.0.0/16     | ovn-control-plane |
		// |   external    |<------------- |   FRR router   |<------ KIND cluster --  ---------------------
		// |    server     |               |                |                         |    ovn-worker     |   (client pod advertised
		// -----------------               ------------------                         ---------------------    using RouteAdvertisements
		//                                                                            |    ovn-worker2    |    from default pod network)
		//                                                                            ---------------------
		// The client pod inside the KIND cluster on the default network exposed using default network Router
		// Advertisement will curl the external server container sitting outside the cluster via a FRR router
		// This test ensures the north-south connectivity is happening through podIP
		ginkgo.It("tests are run towards the external agnhost echo server", func() {
			ginkgo.By("routes from external bgp server are imported by nodes in the cluster")
			bgpNetwork, err := infraprovider.Get().GetNetwork(bgpExternalNetworkName)
			framework.ExpectNoError(err, "network %s must be available and precreated before test run", bgpExternalNetworkName)
			externalServerV4CIDR, externalServerV6CIDR, err := bgpNetwork.IPv4IPv6Subnets()
			framework.ExpectNoError(err, "must get %s subnets", bgpExternalNetworkName)
			framework.Logf("the network cidrs to be imported are v4=%s and v6=%s", externalServerV4CIDR, externalServerV6CIDR)
			for _, node := range nodes.Items {
				if isIPv4Supported(f.ClientSet) {
					ipVer := ""
					bgpRouteCommand := strings.Split(fmt.Sprintf("ip%s route show %s", ipVer, externalServerV4CIDR), " ")
					framework.Logf("Checking for server's route in node %s", node.Name)
					gomega.Eventually(func() bool {
						routes, err := infraprovider.Get().ExecK8NodeCommand(node.GetName(), bgpRouteCommand)
						framework.ExpectNoError(err, "failed to get BGP routes from node")
						framework.Logf("Routes in node %s", routes)
						return strings.Contains(routes, frrContainerIPv4)
					}, 30*time.Second).Should(gomega.BeTrue())
				}
				if isIPv6Supported(f.ClientSet) {
					ipVer := " -6"
					nodeIPv6LLA, err := GetNodeIPv6LinkLocalAddressForEth0(routerContainerName)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					bgpRouteCommand := strings.Split(fmt.Sprintf("ip%s route show %s", ipVer, externalServerV6CIDR), " ")
					framework.Logf("Checking for server's route in node %s", node.Name)
					gomega.Eventually(func() bool {
						routes, err := infraprovider.Get().ExecK8NodeCommand(node.GetName(), bgpRouteCommand)
						framework.ExpectNoError(err, "failed to get BGP routes from node")
						framework.Logf("Routes in node %s", routes)
						return strings.Contains(routes, nodeIPv6LLA)
					}, 30*time.Second).Should(gomega.BeTrue())
				}
			}

			ginkgo.By("routes to the default pod network are advertised to external frr router")
			// Get the first element in the advertisements array (assuming you want to check the first one)
			gomega.Eventually(func() string {
				podNetworkValue, err := e2ekubectl.RunKubectl("", "get", "ra", defaultNetworkRAName, "--template={{index .spec.advertisements 0}}")
				if err != nil {
					return ""
				}
				return podNetworkValue
			}, 5*time.Second, time.Second).Should(gomega.Equal("PodNetwork"))

			gomega.Eventually(func() string {
				reason, err := e2ekubectl.RunKubectl("", "get", "ra", defaultNetworkRAName, "-o", "jsonpath={.status.conditions[?(@.type=='Accepted')].reason}")
				if err != nil {
					return ""
				}
				return reason
			}, 30*time.Second, time.Second).Should(gomega.Equal("Accepted"))

			ginkgo.By("all 3 node's podSubnet routes are exported correctly to external FRR router by frr-k8s speakers")
			// sample
			//10.244.0.0/24 nhid 27 via 172.18.0.3 dev eth0 proto bgp metric 20
			//10.244.1.0/24 nhid 30 via 172.18.0.2 dev eth0 proto bgp metric 20
			//10.244.2.0/24 nhid 25 via 172.18.0.4 dev eth0 proto bgp metric 20
			for _, serverContainerIP := range serverContainerIPs {
				for _, node := range nodes.Items {
					checkL3NodePodRoute(node, serverContainerIP, routerContainerName, types.DefaultNetworkName)
				}
			}

			var expectedV4IP, expectedV6IP string
			snatEnabled := isNoOverlayOutboundSNATEnabled(f)

			if snatEnabled {
				ginkgo.By("queries to the external server are SNATed (uses node IP)")
				// Get the node where the client pod is running
				clientPodNode, err := f.ClientSet.CoreV1().Nodes().Get(context.TODO(), clientPodNodeName, metav1.GetOptions{})
				framework.ExpectNoError(err, fmt.Sprintf("Getting node %s failed: %v", clientPodNodeName, err))

				// Get node IPs
				nodeV4Addrs := e2enode.GetAddressesByTypeAndFamily(clientPodNode, corev1.NodeInternalIP, corev1.IPv4Protocol)
				nodeV6Addrs := e2enode.GetAddressesByTypeAndFamily(clientPodNode, corev1.NodeInternalIP, corev1.IPv6Protocol)
				if len(nodeV4Addrs) > 0 {
					expectedV4IP = nodeV4Addrs[0]
				}
				if len(nodeV6Addrs) > 0 {
					expectedV6IP = nodeV6Addrs[0]
				}
				framework.Logf("Client pod node IP address v4=%s, v6=%s", expectedV4IP, expectedV6IP)
			} else {
				ginkgo.By("queries to the external server are not SNATed (uses podIP)")
				podv4IP, podv6IP, err := podIPsForDefaultNetwork(f.ClientSet, f.Namespace.Name, clientPod.Name)
				framework.ExpectNoError(err, fmt.Sprintf("Getting podIPs for pod %s failed: %v", clientPod.Name, err))
				expectedV4IP = podv4IP
				expectedV6IP = podv6IP
				framework.Logf("Client pod IP address v4=%s, v6=%s", expectedV4IP, expectedV6IP)
			}
			for _, serverContainerIP := range serverContainerIPs {
				ginkgo.By(fmt.Sprintf("Sending request to node IP %s "+
					"and expecting to receive the same payload", serverContainerIP))
				cmd := fmt.Sprintf("curl --max-time 10 -g -q -s http://%s/clientip",
					net.JoinHostPort(serverContainerIP, "8080"),
				)
				framework.Logf("Testing pod to external traffic with command %q", cmd)
				stdout, err := e2epodoutput.RunHostCmdWithRetries(
					clientPod.Namespace,
					clientPod.Name,
					cmd,
					framework.Poll,
					60*time.Second)
				framework.ExpectNoError(err, fmt.Sprintf("Testing pod to external traffic failed: %v", err))
				expectedIP := expectedV4IP
				if isIPv6Supported(f.ClientSet) && utilnet.IsIPv6String(serverContainerIP) {
					expectedIP = expectedV6IP
					// For IPv6 addresses, need to handle the brackets in the output
					outputIP := strings.TrimPrefix(strings.Split(stdout, "]:")[0], "[")
					gomega.Expect(outputIP).To(gomega.Equal(expectedIP),
						fmt.Sprintf("Testing pod %s to external traffic failed while analysing output %v", echoClientPodName, stdout))
				} else {
					// Original IPv4 handling
					gomega.Expect(strings.Split(stdout, ":")[0]).To(gomega.Equal(expectedIP),
						fmt.Sprintf("Testing pod %s to external traffic failed while analysing output %v", echoClientPodName, stdout))
				}
			}
		})

		ginkgo.It("can connect to an external server and another cluster node after toggling default network advertisement off and back on", ginkgo.Serial, func() {
			ginkgo.By("routes from external bgp server are imported by nodes in the cluster")
			bgpNetwork, err := infraprovider.Get().GetNetwork(bgpExternalNetworkName)
			framework.ExpectNoError(err, "network %s must be available and precreated before test run", bgpExternalNetworkName)
			externalServerV4CIDR, externalServerV6CIDR, err := bgpNetwork.IPv4IPv6Subnets()
			framework.ExpectNoError(err, "must get %s subnets", bgpExternalNetworkName)
			framework.Logf("the network cidrs to be imported are v4=%s and v6=%s", externalServerV4CIDR, externalServerV6CIDR)
			for _, node := range nodes.Items {
				if isIPv4Supported(f.ClientSet) {
					ipVer := ""
					bgpRouteCommand := strings.Split(fmt.Sprintf("ip%s route show %s", ipVer, externalServerV4CIDR), " ")
					framework.Logf("Checking for server's route in node %s", node.Name)
					gomega.Eventually(func() bool {
						routes, err := infraprovider.Get().ExecK8NodeCommand(node.GetName(), bgpRouteCommand)
						framework.ExpectNoError(err, "failed to get BGP routes from node")
						framework.Logf("Routes in node %s", routes)
						return strings.Contains(routes, frrContainerIPv4)
					}, 30*time.Second).Should(gomega.BeTrue())
				}
				if isIPv6Supported(f.ClientSet) {
					ipVer := " -6"
					nodeIPv6LLA, err := GetNodeIPv6LinkLocalAddressForEth0(routerContainerName)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					bgpRouteCommand := strings.Split(fmt.Sprintf("ip%s route show %s", ipVer, externalServerV6CIDR), " ")
					framework.Logf("Checking for server's route in node %s", node.Name)
					gomega.Eventually(func() bool {
						routes, err := infraprovider.Get().ExecK8NodeCommand(node.GetName(), bgpRouteCommand)
						framework.ExpectNoError(err, "failed to get BGP routes from node")
						framework.Logf("Routes in node %s", routes)
						return strings.Contains(routes, nodeIPv6LLA)
					}, 30*time.Second).Should(gomega.BeTrue())
				}
			}

			ginkgo.By("routes to the default pod network are advertised to external frr router")
			// Get the first element in the advertisements array (assuming you want to check the first one)
			gomega.Eventually(func() string {
				podNetworkValue, err := e2ekubectl.RunKubectl("", "get", "ra", defaultNetworkRAName, "--template={{index .spec.advertisements 0}}")
				if err != nil {
					return ""
				}
				return podNetworkValue
			}, 5*time.Second, time.Second).Should(gomega.Equal("PodNetwork"))

			gomega.Eventually(func() string {
				reason, err := e2ekubectl.RunKubectl("", "get", "ra", defaultNetworkRAName, "-o", "jsonpath={.status.conditions[?(@.type=='Accepted')].reason}")
				if err != nil {
					return ""
				}
				return reason
			}, 30*time.Second, time.Second).Should(gomega.Equal("Accepted"))

			ginkgo.By("all 3 node's podSubnet routes are exported correctly to external FRR router by frr-k8s speakers")
			// sample
			//10.244.0.0/24 nhid 27 via 172.18.0.3 dev eth0 proto bgp metric 20
			//10.244.1.0/24 nhid 30 via 172.18.0.2 dev eth0 proto bgp metric 20
			//10.244.2.0/24 nhid 25 via 172.18.0.4 dev eth0 proto bgp metric 20
			for _, serverContainerIP := range serverContainerIPs {
				for _, node := range nodes.Items {
					checkL3NodePodRoute(node, serverContainerIP, routerContainerName, types.DefaultNetworkName)
				}
			}

			// Get client pod IPs and its host's nodeIPs, get the nodeIPs for the node where the host networked pod is running
			var clientPodIPs, clientPodNodeIPs, hostNetworkedPodNodeIPs []string

			podv4IP, podv6IP, err := podIPsForDefaultNetwork(f.ClientSet, f.Namespace.Name, clientPod.Name)
			framework.ExpectNoError(err, fmt.Sprintf("Getting podIPs for pod %s failed: %v", clientPod.Name, err))
			framework.Logf("Client pod IP address v4=%s, v6=%s", podv4IP, podv6IP)
			if podv4IP != "" {
				clientPodIPs = append(clientPodIPs, podv4IP)
			}
			if podv6IP != "" {
				clientPodIPs = append(clientPodIPs, podv6IP)
			}

			clientNode, err := f.ClientSet.CoreV1().Nodes().Get(context.TODO(), clientPod.Spec.NodeName, metav1.GetOptions{})
			framework.ExpectNoError(err)

			// Get the nodeIPs for the node where the client pod is running
			var nodeIPv4, nodeIPv6 string
			if isIPv4Supported(f.ClientSet) {
				v4Addrs := e2enode.GetAddressesByTypeAndFamily(clientNode, corev1.NodeInternalIP, corev1.IPv4Protocol)
				if len(v4Addrs) > 0 {
					nodeIPv4 = v4Addrs[0]
				}
			}
			if isIPv6Supported(f.ClientSet) {
				v6Addrs := e2enode.GetAddressesByTypeAndFamily(clientNode, corev1.NodeInternalIP, corev1.IPv6Protocol)
				if len(v6Addrs) > 0 {
					nodeIPv6 = v6Addrs[0]
				}
			}
			if nodeIPv4 != "" {
				clientPodNodeIPs = append(clientPodNodeIPs, nodeIPv4)
			}
			if nodeIPv6 != "" {
				clientPodNodeIPs = append(clientPodNodeIPs, nodeIPv6)
			}
			framework.Logf("clientPodNodeIPs: %v", clientPodNodeIPs)

			ginkgo.By("Creating a host networked pod on a second node")
			// Add labels so the service can select this pod
			podLabels := map[string]string{
				"app": "internal-server-pod",
			}

			// Create host networked pod on another cluster node using createPod function
			// Use a random high port to avoid collisions on the host
			min, max := 25000, 25999
			hostNetPort := rand.Intn(max-min+1) + min
			framework.Logf("host-networked netexec port: %d", hostNetPort)
			hostNetworkedPod, err = createPod(f, "internal-server-pod", nodes.Items[2].Name, f.Namespace.Name,
				[]string{}, podLabels, func(p *corev1.Pod) {
					// Set host networking
					p.Spec.HostNetwork = true
					// Run netexec on the chosen port
					p.Spec.Containers[0].Args = []string{"netexec", fmt.Sprintf("--http-port=%d", hostNetPort)}

					// Add required security context to comply with PodSecurity "restricted" policy
					for i := range p.Spec.Containers {
						if p.Spec.Containers[i].SecurityContext == nil {
							p.Spec.Containers[i].SecurityContext = &corev1.SecurityContext{}
						}

						// Set required security context fields
						p.Spec.Containers[i].SecurityContext.AllowPrivilegeEscalation = ptr.To(false)
						p.Spec.Containers[i].SecurityContext.RunAsNonRoot = ptr.To(true)
						p.Spec.Containers[i].SecurityContext.RunAsUser = ptr.To(int64(1000))
						p.Spec.Containers[i].SecurityContext.Capabilities = &corev1.Capabilities{
							Drop: []corev1.Capability{"ALL"},
						}
						p.Spec.Containers[i].SecurityContext.SeccompProfile = &corev1.SeccompProfile{
							Type: corev1.SeccompProfileTypeRuntimeDefault,
						}
					}
				})
			framework.ExpectNoError(err)

			hostNetworkedPodNode, err := f.ClientSet.CoreV1().Nodes().Get(context.TODO(), hostNetworkedPod.Spec.NodeName, metav1.GetOptions{})
			framework.ExpectNoError(err)

			// reset before collecting for a different node
			nodeIPv4, nodeIPv6 = "", ""

			if isIPv4Supported(f.ClientSet) {
				v4Addrs := e2enode.GetAddressesByTypeAndFamily(hostNetworkedPodNode, corev1.NodeInternalIP, corev1.IPv4Protocol)
				if len(v4Addrs) > 0 {
					nodeIPv4 = v4Addrs[0]
				}
			}
			if isIPv6Supported(f.ClientSet) {
				v6Addrs := e2enode.GetAddressesByTypeAndFamily(hostNetworkedPodNode, corev1.NodeInternalIP, corev1.IPv6Protocol)
				if len(v6Addrs) > 0 {
					nodeIPv6 = v6Addrs[0]
				}
			}
			if nodeIPv4 != "" {
				hostNetworkedPodNodeIPs = append(hostNetworkedPodNodeIPs, nodeIPv4)
			}
			if nodeIPv6 != "" {
				hostNetworkedPodNodeIPs = append(hostNetworkedPodNodeIPs, nodeIPv6)
			}
			framework.Logf("hostNetworkedPodNodeIPs: %v", hostNetworkedPodNodeIPs)

			// When SNAT is enabled, external traffic uses node IPs even when advertised
			snatEnabled := isNoOverlayOutboundSNATEnabled(f)
			expectedExternalSourceIPs := clientPodIPs
			if snatEnabled {
				expectedExternalSourceIPs = clientPodNodeIPs
			}

			if snatEnabled {
				ginkgo.By("With default network being advertised, queries to the external server are SNATed (uses nodeIP)")
			} else {
				ginkgo.By("With default network being advertised, queries to the external server are not SNATed (uses podIP)")
			}
			gomega.Eventually(func() error {
				for _, serverIP := range serverContainerIPs {
					if serverIP == "" {
						continue
					}
					isV6 := utilnet.IsIPv6String(serverIP)
					var expectedIP string
					for _, srcIP := range expectedExternalSourceIPs {
						if srcIP != "" && utilnet.IsIPv6String(srcIP) == isV6 {
							expectedIP = srcIP
							break
						}
					}
					if expectedIP == "" {
						continue
					}
					if err := curlAgnHostClientIPFromPod(clientPod.Namespace, clientPod.Name, expectedIP, serverIP, strconv.Itoa(netexecPort)); err != nil {
						return err
					}
				}
				return nil
			}, 30*time.Second, 2*time.Second).Should(gomega.Succeed(), "With default network being advertised initially, pod to external server test failed")

			ginkgo.By("With default network being advertised, queries to the second node are SNATed (uses nodeIP)")
			gomega.Eventually(func() error {
				for _, nodeIP := range hostNetworkedPodNodeIPs {
					if nodeIP == "" {
						continue
					}
					isV6 := utilnet.IsIPv6String(nodeIP)
					var expectedIP string
					for _, clientNodeIP := range clientPodNodeIPs {
						if clientNodeIP != "" && utilnet.IsIPv6String(clientNodeIP) == isV6 {
							expectedIP = clientNodeIP
							break
						}
					}
					if expectedIP == "" {
						continue
					}
					if err := curlAgnHostClientIPFromPod(clientPod.Namespace, clientPod.Name, expectedIP, nodeIP, strconv.Itoa(hostNetPort)); err != nil {
						return err
					}
				}
				return nil
			}, 30*time.Second, 2*time.Second).Should(gomega.Succeed(), "With default network being advertised initially, pod to second node test failed")

			raClient, err := raclientset.NewForConfig(f.ClientConfig())
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			originalRA, err := raClient.K8sV1().RouteAdvertisements().Get(context.TODO(), defaultNetworkRAName, metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Defer adding the original default-network RA back to restore the original
			// test setup, including its FRR configuration selector.
			ra := &rav1.RouteAdvertisements{
				ObjectMeta: metav1.ObjectMeta{Name: originalRA.Name},
				Spec:       originalRA.Spec,
			}

			defer func() {
				ra, err = raClient.K8sV1().RouteAdvertisements().Create(context.TODO(), ra, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Wait for RouteAdvertisement to be accepted, but don't fail the test if it times out
				// We'll handle the failure with AbortSuite instead
				accepted := false
				err = wait.PollUntilContextTimeout(context.TODO(), time.Second, 30*time.Second, true, func(ctx context.Context) (bool, error) {
					ra, err := raClient.K8sV1().RouteAdvertisements().Get(ctx, ra.Name, metav1.GetOptions{})
					if err != nil {
						return false, nil // Continue polling on error
					}
					condition := meta.FindStatusCondition(ra.Status.Conditions, "Accepted")
					if condition == nil {
						return false, nil // Continue polling if condition not found
					}
					if condition.Reason == "Accepted" {
						accepted = true
						return true, nil // Success - stop polling
					}
					return false, nil // Continue polling if not accepted yet
				})
				// Note: we ignore the error from PollImmediate since we handle timeout via the accepted variable

				// Abort suite if default RA failed to reach Accepted state in defer restore
				if !accepted {
					ginkgo.AbortSuite("CRITICAL: Default route advertisement failed to reach Accepted state in defer cleanup - test environment is corrupted")
				}

				// repeat pod to external and pod to second node tests
				if snatEnabled {
					ginkgo.By("With default network being advertised again, queries to the external server are SNATed (uses nodeIP)")
				} else {
					ginkgo.By("With default network being advertised again, queries to the external server are not SNATed (uses podIP)")
				}
				gomega.Eventually(func() error {
					for _, serverIP := range serverContainerIPs {
						if serverIP == "" {
							continue
						}
						isV6 := utilnet.IsIPv6String(serverIP)
						var expectedIP string
						for _, srcIP := range expectedExternalSourceIPs {
							if srcIP != "" && utilnet.IsIPv6String(srcIP) == isV6 {
								expectedIP = srcIP
								break
							}
						}
						if expectedIP == "" {
							continue
						}
						if err := curlAgnHostClientIPFromPod(clientPod.Namespace, clientPod.Name, expectedIP, serverIP, strconv.Itoa(netexecPort)); err != nil {
							return err
						}
					}
					return nil
				}, 30*time.Second, 2*time.Second).Should(gomega.Succeed(), "With default network being advertised again, pod to external server test failed, test environment may be corrupted")

				ginkgo.By("With default network being advertised again, queries to the second node are SNATed (uses nodeIP)")
				gomega.Eventually(func() error {
					for _, nodeIP := range hostNetworkedPodNodeIPs {
						if nodeIP == "" {
							continue
						}
						isV6 := utilnet.IsIPv6String(nodeIP)
						var expectedIP string
						for _, clientNodeIP := range clientPodNodeIPs {
							if clientNodeIP != "" && utilnet.IsIPv6String(clientNodeIP) == isV6 {
								expectedIP = clientNodeIP
								break
							}
						}
						if expectedIP == "" {
							continue
						}
						if err := curlAgnHostClientIPFromPod(clientPod.Namespace, clientPod.Name, expectedIP, nodeIP, strconv.Itoa(hostNetPort)); err != nil {
							return err
						}
					}
					return nil
				}, 30*time.Second, 2*time.Second).Should(gomega.Succeed(), "With default network being advertised again, pod to second node test failed, test environment may be corrupted")

			}()

			ginkgo.By("Delete route advertisement")
			_, err = e2ekubectl.RunKubectl("", "delete", "ra", defaultNetworkRAName, "--ignore-not-found=true")
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Make sure default RA is deleted
			_, err = e2ekubectl.RunKubectl("", "get", "ra", defaultNetworkRAName)
			gomega.Expect(err).To(gomega.HaveOccurred())

			ginkgo.By("After default network is toggled to unadvertised, run test towards the external agnhost echo server from client pod again, egressing packets should be SNATed to pod's host nodeIP")
			gomega.Eventually(func() error {
				for _, serverIP := range serverContainerIPs {
					if serverIP == "" {
						continue
					}
					isV6 := utilnet.IsIPv6String(serverIP)
					var expectedIP string
					for _, clientNodeIP := range clientPodNodeIPs {
						if clientNodeIP != "" && utilnet.IsIPv6String(clientNodeIP) == isV6 {
							expectedIP = clientNodeIP
							break
						}
					}
					if expectedIP == "" {
						continue
					}
					if err := curlAgnHostClientIPFromPod(clientPod.Namespace, clientPod.Name, expectedIP, serverIP, strconv.Itoa(netexecPort)); err != nil {
						return err
					}
				}
				return nil
			}, 30*time.Second, 2*time.Second).Should(gomega.Succeed(), "After default network is toggled to unadvertised, pod to external server test failed")

			ginkgo.By("After default network is toggled to unadvertised, run test towards the second node from client pod, egressing packets should be SNATed to pod's host nodeIP")
			gomega.Eventually(func() error {
				for _, nodeIP := range hostNetworkedPodNodeIPs {
					if nodeIP == "" {
						continue
					}
					isV6 := utilnet.IsIPv6String(nodeIP)
					var expectedIP string
					for _, clientNodeIP := range clientPodNodeIPs {
						if clientNodeIP != "" && utilnet.IsIPv6String(clientNodeIP) == isV6 {
							expectedIP = clientNodeIP
							break
						}
					}
					if expectedIP == "" {
						continue
					}
					if err := curlAgnHostClientIPFromPod(clientPod.Namespace, clientPod.Name, expectedIP, nodeIP, strconv.Itoa(hostNetPort)); err != nil {
						return err
					}
				}
				return nil
			}, 30*time.Second, 2*time.Second).Should(gomega.Succeed(), "After default network is toggled to unadvertised, pod to second node test failed")

		})
	})
})

// Helper functions for Layer3 subnet configurations
func cudnLayer3SubnetsA() []udnv1.Layer3Subnet {
	if deploymentconfig.Get().IsConfigurationEnabled(deploymentconfigapi.L3UDNMultiSubnetConfig) {
		return []udnv1.Layer3Subnet{{
			CIDR:       "102.102.0.0/23",
			HostSubnet: 24,
		}, {
			CIDR:       "102.104.0.0/16",
			HostSubnet: 24,
		}, {
			CIDR:       "2013:100:200::0/63",
			HostSubnet: 64,
		}, {
			CIDR:       "2013:100:201::0/48",
			HostSubnet: 64,
		}}
	}
	return []udnv1.Layer3Subnet{{
		CIDR:       "102.102.0.0/16",
		HostSubnet: 24,
	}, {
		CIDR:       "2013:100:200::0/60",
		HostSubnet: 64,
	}}
}

func cudnLayer3SubnetsB() []udnv1.Layer3Subnet {
	if deploymentconfig.Get().IsConfigurationEnabled(deploymentconfigapi.L3UDNMultiSubnetConfig) {
		return []udnv1.Layer3Subnet{{
			CIDR:       "103.103.0.0/23",
			HostSubnet: 24,
		}, {
			CIDR:       "103.104.0.0/16",
			HostSubnet: 24,
		}, {
			CIDR:       "2014:100:200::0/63",
			HostSubnet: 64,
		}, {
			CIDR:       "2014:100:201::0/48",
			HostSubnet: 64,
		}}
	}
	return []udnv1.Layer3Subnet{{
		CIDR:       "103.103.0.0/16",
		HostSubnet: 24,
	}, {
		CIDR:       "2014:100:200::0/60",
		HostSubnet: 64,
	}}
}

// expectNoUDNSubnetOnNodesExcept asserts that, with dynamic UDN allocation,
// no node other than activeNodeName gets a subnet allocated for the network.
func expectNoUDNSubnetOnNodesExcept(nodes []corev1.Node, activeNodeName, netName string) {
	ginkgo.GinkgoHelper()
	gomega.Consistently(func() error {
		for _, node := range nodes {
			if node.Name == activeNodeName {
				continue
			}
			if _, _, err := getNodePodCIDRs(node.Name, netName); err == nil {
				return fmt.Errorf("node %s has a subnet allocated for network %s", node.Name, netName)
			}
		}
		return nil
	}, 10*time.Second, 2*time.Second).Should(gomega.Succeed())
}

// expectNodesNotAdvertisedInFRR asserts that the external FRR router has no
// route within the network's subnets via any node other than activeNodeName.
func expectNodesNotAdvertisedInFRR(nodes []corev1.Node, activeNodeName, v4CIDR, v6CIDR string) {
	ginkgo.GinkgoHelper()
	externalContainer := infraapi.ExternalContainer{Name: routerContainerName}
	type familyCheck struct {
		routeCmd []string
		nextHop  func(corev1.Node) string
	}
	var checks []familyCheck
	if v4CIDR != "" {
		checks = append(checks, familyCheck{
			routeCmd: []string{"ip", "route", "show", "root", v4CIDR},
			nextHop: func(node corev1.Node) string {
				ips := e2enode.GetAddressesByTypeAndFamily(&node, corev1.NodeInternalIP, corev1.IPv4Protocol)
				if len(ips) == 0 {
					return ""
				}
				return ips[0]
			},
		})
	}
	if v6CIDR != "" {
		checks = append(checks, familyCheck{
			routeCmd: []string{"ip", "-6", "route", "show", "root", v6CIDR},
			nextHop: func(node corev1.Node) string {
				// BGP uses the link-local address as the IPv6 nexthop
				lla, err := GetNodeIPv6LinkLocalAddressForEth0(node.Name)
				if err != nil {
					return ""
				}
				return lla
			},
		})
	}
	gomega.Consistently(func() error {
		for _, check := range checks {
			routes, err := infraprovider.Get().ExecExternalContainerCommand(externalContainer, check.routeCmd)
			if err != nil {
				return fmt.Errorf("failed to get BGP routes from the external router: %w", err)
			}
			for _, node := range nodes {
				if node.Name == activeNodeName {
					continue
				}
				if nextHop := check.nextHop(node); nextHop != "" && strings.Contains(routes, nextHop) {
					return fmt.Errorf("external router has a route within %q via node %s (%s): %s",
						strings.Join(check.routeCmd, " "), node.Name, nextHop, routes)
				}
			}
		}
		return nil
	}, 10*time.Second, 2*time.Second).Should(gomega.Succeed())
}

var _ = ginkgo.Describe("BGP: Pod to external server when CUDN network is advertised", feature.RouteAdvertisements, func() {
	var serverContainerIPs []string
	var frrContainerIPv4, frrContainerIPv6 string
	var nodes *corev1.NodeList
	var clientPod *corev1.Pod

	f := wrappedTestFramework("pod2external-route-advertisements")
	f.SkipNamespaceCreation = true

	ginkgo.BeforeEach(func() {
		var err error
		namespace, err := f.CreateNamespace(context.TODO(), f.BaseName, map[string]string{
			"e2e-framework":           f.BaseName,
			RequiredUDNNamespaceLabel: "",
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		f.Namespace = namespace

		serverContainerIPs = []string{}

		bgpNetwork, err := infraprovider.Get().GetNetwork(bgpExternalNetworkName) // pre-created network
		framework.ExpectNoError(err, "must get %s network", bgpExternalNetworkName)
		bgpServer := infraapi.ExternalContainer{Name: bgpExternalServerContainerName}
		networkInterface, err := infraprovider.Get().GetExternalContainerNetworkInterface(bgpServer, bgpNetwork)
		framework.ExpectNoError(err, "container %s attached to network %s must contain network info", bgpExternalServerContainerName, bgpExternalNetworkName)
		if isIPv4Supported(f.ClientSet) && len(networkInterface.IPv4) > 0 {
			serverContainerIPs = append(serverContainerIPs, networkInterface.IPv4)
		}
		if isIPv6Supported(f.ClientSet) && len(networkInterface.IPv6) > 0 {
			serverContainerIPs = append(serverContainerIPs, networkInterface.IPv6)
		}
		gomega.Expect(len(serverContainerIPs)).Should(gomega.BeNumerically(">", 0), "failed to find external container IPs")
		framework.Logf("The external server IPs are: %+v", serverContainerIPs)
		providerPrimaryNetwork, err := infraprovider.Get().PrimaryNetwork()
		framework.ExpectNoError(err, "provider primary network must be available")
		frrContainer := infraapi.ExternalContainer{Name: routerContainerName}
		networkInterface, err = infraprovider.Get().GetExternalContainerNetworkInterface(frrContainer, providerPrimaryNetwork)
		framework.ExpectNoError(err, "container %s attached to network %s must contain network info", routerContainerName, providerPrimaryNetwork.Name())
		frrContainerIPv4, frrContainerIPv6 = networkInterface.IPv4, networkInterface.IPv6
		framework.Logf("The frr router container IPs are: %s/%s", frrContainerIPv4, frrContainerIPv6)

		// Select nodes here so they're available for all tests
		ginkgo.By("Selecting 3 schedulable nodes")
		nodes, err = e2enode.GetBoundedReadySchedulableNodes(context.TODO(), f.ClientSet, 3)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(len(nodes.Items)).To(gomega.BeNumerically(">", 2))
	})

	ginkgo.DescribeTable("Route Advertisements",
		func(ctx ginkgo.SpecContext, cudnTemplate *udnv1.ClusterUserDefinedNetwork, ra *rav1.RouteAdvertisements) {
			// set the exact selector
			cudnTemplate.Spec.NamespaceSelector = metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
				Key:      "kubernetes.io/metadata.name",
				Operator: metav1.LabelSelectorOpIn,
				Values:   []string{f.Namespace.Name},
			}}}

			// Create CUDN
			ginkgo.By("create ClusterUserDefinedNetwork")
			udnClient, err := udnclientset.NewForConfig(f.ClientConfig())
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			if cudnTemplate.Spec.Network.Layer3 != nil {
				cudnTemplate.Spec.Network.Layer3.Subnets = filterL3Subnets(f.ClientSet, cudnTemplate.Spec.Network.Layer3.Subnets)
			}
			if cudnTemplate.Spec.Network.Layer2 != nil {
				cudnTemplate.Spec.Network.Layer2.Subnets = filterDualStackCIDRs(f.ClientSet, cudnTemplate.Spec.Network.Layer2.Subnets)
			}
			cUDN, err := udnClient.K8sV1().ClusterUserDefinedNetworks().Create(context.Background(), cudnTemplate, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			ginkgo.DeferCleanup(func() {
				udnClient.K8sV1().ClusterUserDefinedNetworks().Delete(context.TODO(), cUDN.Name, metav1.DeleteOptions{})
			})
			gomega.Eventually(ctx, clusterUserDefinedNetworkReadyFunc(ctx, f.DynamicClient, cUDN.Name), 5*time.Second, time.Second).Should(gomega.Succeed())

			ginkgo.DeferCleanup(func() {
				ginkgo.By(fmt.Sprintf("delete pods in %s namespace to unblock CUDN CR & associate NAD deletion", f.Namespace.Name))
				gomega.Expect(f.ClientSet.CoreV1().Pods(f.Namespace.Name).DeleteCollection(context.Background(), metav1.DeleteOptions{}, metav1.ListOptions{})).To(gomega.Succeed())
			})

			// Create client pod
			ginkgo.By("Creating client pod")
			clientPod, err = createPod(f, echoClientPodName, nodes.Items[1].Name, f.Namespace.Name, []string{"bash", "-c", "sleep infinity"}, nil)
			framework.ExpectNoError(err)

			// With dynamic UDN allocation, the network only exists on nodes
			// that run workloads attached to it, which here is just the
			// client pod's node: only expect it to be advertised.
			advertisedNodes := nodes.Items
			if isDynamicUDNEnabled() {
				advertisedNodes = []corev1.Node{nodes.Items[1]}
			}

			// Create route advertisement
			ginkgo.By("create router advertisement")
			raClient, err := raclientset.NewForConfig(f.ClientConfig())
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			ra, err = raClient.K8sV1().RouteAdvertisements().Create(context.TODO(), ra, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			ginkgo.DeferCleanup(func() { raClient.K8sV1().RouteAdvertisements().Delete(context.TODO(), ra.Name, metav1.DeleteOptions{}) })
			ginkgo.By("ensure route advertisement matching CUDN was created successfully")
			gomega.Eventually(func() string {
				ra, err := raClient.K8sV1().RouteAdvertisements().Get(context.TODO(), ra.Name, metav1.GetOptions{})
				if err != nil {
					return ""
				}
				condition := meta.FindStatusCondition(ra.Status.Conditions, "Accepted")
				if condition == nil {
					return ""
				}
				return condition.Reason
			}, 30*time.Second, time.Second).Should(gomega.Equal("Accepted"))

			// Check CUDN TransportAccepted condition is True after RA is created
			if cudnTemplate.Spec.Network.Transport != "" && cudnTemplate.Spec.Network.Transport != udnv1.TransportOption("Geneve") {
				ginkgo.By("ensure CUDN TransportAccepted condition is True")
				gomega.Eventually(func(g gomega.Gomega) {
					cudnObj, err := f.DynamicClient.Resource(clusterUDNGVR).Get(context.Background(), cUDN.Name, metav1.GetOptions{}, "status")
					g.Expect(err).NotTo(gomega.HaveOccurred())
					conditions, err := getConditions(cudnObj)
					g.Expect(err).NotTo(gomega.HaveOccurred())
					for _, condition := range conditions {
						if condition.Type == "TransportAccepted" {
							g.Expect(string(condition.Status)).To(gomega.Equal("True"))
							g.Expect(condition.Message).To(gomega.Equal("Transport has been configured as 'no-overlay'."))
							return
						}
					}
					g.Expect(false).To(gomega.BeTrue(), "TransportAccepted condition not found in CUDN %s", cUDN.Name)
				}, 30*time.Second, time.Second).Should(gomega.Succeed())
			}

			gomega.Expect(len(serverContainerIPs)).To(gomega.BeNumerically(">", 0))

			// -----------------               ------------------                         ---------------------
			// |               | 172.26.0.0/16 |                |       172.18.0.0/16     | ovn-control-plane |
			// |   external    |<------------- |   FRR router   |<------ KIND cluster --  ---------------------
			// |    server     |               |                |                         |    ovn-worker     |   (client UDN pod advertised
			// -----------------               ------------------                         ---------------------    using RouteAdvertisements
			//                                                                            |    ovn-worker2    |    from default pod network)
			//                                                                            ---------------------
			// The client pod inside the KIND cluster on the default network exposed using default network Router
			// Advertisement will curl the external server container sitting outside the cluster via a FRR router
			// This test ensures the north-south connectivity is happening through podIP
			ginkgo.By("routes from external bgp server are imported by nodes in the cluster")
			bgpNetwork, err := infraprovider.Get().GetNetwork(bgpExternalNetworkName)
			framework.ExpectNoError(err, "network %s must be available", bgpExternalNetworkName)
			externalServerV4CIDR, externalServerV6CIDR, err := bgpNetwork.IPv4IPv6Subnets()
			framework.ExpectNoError(err, "must get BGP network subnets")
			framework.Logf("the network cidrs to be imported are v4=%s and v6=%s", externalServerV4CIDR, externalServerV6CIDR)
			var frrIPv6LLA string
			if isIPv6Supported(f.ClientSet) {
				var err error
				frrIPv6LLA, err = GetNodeIPv6LinkLocalAddressForEth0(routerContainerName)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			}
			for _, node := range nodes.Items {
				if isIPv4Supported(f.ClientSet) {
					ipVer := ""
					bgpRouteCommand := strings.Split(fmt.Sprintf("ip%s route show %s", ipVer, externalServerV4CIDR), " ")
					framework.Logf("Checking for server's route in node %s", node.Name)
					gomega.Eventually(func() bool {
						routes, err := infraprovider.Get().ExecK8NodeCommand(node.GetName(), bgpRouteCommand)
						framework.ExpectNoError(err, "failed to get BGP routes from node")
						framework.Logf("Routes in node %s", routes)
						return strings.Contains(routes, frrContainerIPv4)
					}, 30*time.Second).Should(gomega.BeTrue())
				}
				if isIPv6Supported(f.ClientSet) {
					ipVer := " -6"
					bgpRouteCommand := strings.Split(fmt.Sprintf("ip%s route show %s", ipVer, externalServerV6CIDR), " ")
					framework.Logf("Checking for server's route in node %s", node.Name)
					gomega.Eventually(func() bool {
						routes, err := infraprovider.Get().ExecK8NodeCommand(node.GetName(), bgpRouteCommand)
						framework.ExpectNoError(err, "failed to get BGP routes from node")
						framework.Logf("Routes in node %s", routes)
						return strings.Contains(routes, frrIPv6LLA)
					}, 30*time.Second).Should(gomega.BeTrue())
				}
			}

			ginkgo.By("ensure CUDN pod subnet is advertised to the external FRR router")
			for _, serverContainerIP := range serverContainerIPs {
				for _, node := range advertisedNodes {
					if cudnTemplate.Spec.Network.Layer3 != nil {
						checkL3NodePodRoute(node, serverContainerIP, routerContainerName, types.CUDNPrefix+cUDN.Name)
					} else if cudnTemplate.Spec.Network.Layer2 != nil {
						checkL2NodePodRoute(node, serverContainerIP, routerContainerName, cudnTemplate.Spec.Network.Layer2.Subnets)
					} else {
						ginkgo.Fail("unexpected topology: neither Layer3 nor Layer2 network spec is set")
					}
				}
			}

			if isDynamicUDNEnabled() && cudnTemplate.Spec.Network.Layer3 != nil {
				ginkgo.By("ensure nodes without workloads attached to the network get no subnet allocated for it")
				expectNoUDNSubnetOnNodesExcept(nodes.Items, clientPod.Spec.NodeName, types.CUDNPrefix+cUDN.Name)
			}

			layer2LocalGateway := cudnTemplate.Spec.Network.Layer2 != nil && IsGatewayModeLocal(f.ClientSet)
			if layer2LocalGateway {
				ginkgo.By("ensure CUDN VRF has a BGP-imported route to the external server CIDR")
			} else {
				ginkgo.By("ensure CUDN gateway router has a BGP-imported route to the external server CIDR")
			}
			// the network topology only exists on the nodes it is allocated on
			for _, node := range advertisedNodes {
				nodeName := node.Name
				for _, tc := range []struct {
					cidr, nextHop string
				}{
					{externalServerV4CIDR, frrContainerIPv4},
					{externalServerV6CIDR, frrIPv6LLA},
				} {
					if tc.cidr == "" || tc.nextHop == "" {
						continue
					}
					isV6 := utilnet.IsIPv6CIDRString(tc.cidr)
					if isV6 && !isIPv6Supported(f.ClientSet) {
						continue
					}
					if !isV6 && !isIPv4Supported(f.ClientSet) {
						continue
					}
					if layer2LocalGateway {
						framework.Logf("Checking on node %s for CUDN VRF %s route to %s via %s", nodeName, cUDN.Name, tc.cidr, tc.nextHop)
						gomega.Eventually(func() bool {
							found, err := hasRouteInCUDNVRF(node, cUDN.Name, tc.cidr, tc.nextHop)
							if err != nil {
								framework.Logf("failed to check CUDN VRF route on node %s: %v", nodeName, err)
								return false
							}
							return found
						}, 60*time.Second, 5*time.Second).Should(gomega.BeTrue(),
							"route for %s via %s not found in CUDN VRF %s on node %s",
							tc.cidr, tc.nextHop, cUDN.Name, nodeName)
						continue
					}
					// Assert the GR has a static-route line matching
					// the CIDR and the expected next hop, that is the external FRR
					// container IP.
					gomega.Eventually(func() bool {
						routes, err := cudnGRRoutesForNode(f.ClientSet, cUDN.Name, nodeName)
						if err != nil {
							framework.Logf("failed to list CUDN GR routes on node %s: %v", nodeName, err)
							return false
						}
						framework.Logf("CUDN GR routes on node %s:\n%s", nodeName, routes)
						for _, line := range strings.Split(routes, "\n") {
							if strings.Contains(line, tc.cidr) && strings.Contains(line, tc.nextHop) {
								return true
							}
						}
						return false
					}, 60*time.Second, 5*time.Second).Should(gomega.BeTrue(),
						"CUDN %q GR on node %s is missing a route %s -> %s",
						cUDN.Name, nodeName, tc.cidr, tc.nextHop)
				}
			}

			expectSNAT := cudnTemplate.Spec.Network.NoOverlay != nil &&
				cudnTemplate.Spec.Network.NoOverlay.OutboundSNAT == udnv1.SNATEnabled

			if expectSNAT {
				ginkgo.By("queries to the external server are SNATed (uses node IP) since OutboundSNAT is enabled")
			} else {
				ginkgo.By("queries to the external server are not SNATed (uses podIP)")
			}
			var expectedSNATSourceIPs []string
			if expectSNAT {
				clientPodNode, err := f.ClientSet.CoreV1().Nodes().Get(context.TODO(), clientPod.Spec.NodeName, metav1.GetOptions{})
				framework.ExpectNoError(err, fmt.Sprintf("Getting node %s failed: %v", clientPod.Spec.NodeName, err))
				expectedSNATSourceIPs = e2enode.GetAddresses(clientPodNode, corev1.NodeInternalIP)
				gomega.Expect(expectedSNATSourceIPs).NotTo(gomega.BeEmpty(), "Expected node %s to have an InternalIP", clientPod.Spec.NodeName)
				framework.Logf("Client pod node IP addresses=%v", expectedSNATSourceIPs)
			}
			for _, serverContainerIP := range serverContainerIPs {
				podIP, err := getPodAnnotationIPsForAttachmentByIPFamily(
					f.ClientSet, f.Namespace.Name, clientPod.Name, namespacedName(f.Namespace.Name, cUDN.Name), utilnet.IPFamilyOfString(serverContainerIP))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				framework.ExpectNoError(err, fmt.Sprintf("Getting podIPs for pod %s failed: %v", clientPod.Name, err))
				framework.Logf("Client pod IP address=%s", podIP)

				ginkgo.By(fmt.Sprintf("Sending request to node IP %s "+
					"and expecting to receive the same payload", serverContainerIP))
				cmd := fmt.Sprintf("curl --max-time 10 -g -q -s http://%s/clientip",
					net.JoinHostPort(serverContainerIP, "8080"),
				)
				framework.Logf("Testing pod to external traffic with command %q", cmd)
				stdout, err := e2epodoutput.RunHostCmdWithRetries(
					clientPod.Namespace,
					clientPod.Name,
					cmd,
					framework.Poll,
					60*time.Second)
				framework.ExpectNoError(err, fmt.Sprintf("Testing pod to external traffic failed: %v", err))

				sourceIP, _, err := net.SplitHostPort(strings.TrimSpace(stdout))
				gomega.Expect(err).NotTo(gomega.HaveOccurred(),
					fmt.Sprintf("Failed to parse client address from output %q", stdout))
				if expectSNAT {
					// When OutboundSNAT is enabled, traffic to external destinations
					// is masqueraded to the node IP. Verify the source is NOT the pod IP.
					expectedSourceIP := getFirstIPStringOfFamily(utilnet.IPFamilyOfString(serverContainerIP), expectedSNATSourceIPs)
					gomega.Expect(expectedSourceIP).NotTo(gomega.BeEmpty(),
						"Expected node %s to have an InternalIP for the external server IP family %s",
						clientPod.Spec.NodeName, serverContainerIP)
					gomega.Expect(sourceIP).To(gomega.Equal(expectedSourceIP),
						fmt.Sprintf("Expected SNAT for pod %s to use %s, output: %v", echoClientPodName, expectedSourceIP, stdout))
				} else {
					gomega.Expect(sourceIP).To(gomega.Equal(podIP),
						fmt.Sprintf("Testing pod %s to external traffic failed while analysing output %v", echoClientPodName, stdout))
				}
			}
		},
		ginkgo.Entry("layer3",
			&udnv1.ClusterUserDefinedNetwork{
				ObjectMeta: metav1.ObjectMeta{
					// Keep generated CUDN names under the Linux 15-byte interface
					// limit so the VRF name is unique instead of network ID based.
					GenerateName: "bgp-l3-",
					Labels:       map[string]string{"bgp-l3": ""},
				},
				Spec: udnv1.ClusterUserDefinedNetworkSpec{
					Network: udnv1.NetworkSpec{
						Topology: udnv1.NetworkTopologyLayer3,
						Layer3: &udnv1.Layer3Config{
							Role:    "Primary",
							Subnets: cudnLayer3SubnetsB(),
						},
					},
				},
			},
			&rav1.RouteAdvertisements{
				ObjectMeta: metav1.ObjectMeta{
					Name: "bgp-l3-ra",
				},
				Spec: rav1.RouteAdvertisementsSpec{
					NetworkSelectors: apitypes.NetworkSelectors{
						apitypes.NetworkSelector{
							NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
							ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{
								NetworkSelector: metav1.LabelSelector{
									MatchLabels: map[string]string{"bgp-l3": ""},
								},
							},
						},
					},
					NodeSelector:             metav1.LabelSelector{},
					FRRConfigurationSelector: metav1.LabelSelector{},
					Advertisements: []rav1.AdvertisementType{
						rav1.PodNetwork,
					},
				},
			},
		),
		ginkgo.Entry("layer3 no-overlay SNAT enabled unmanaged routing", feature.NoOverlay,
			&udnv1.ClusterUserDefinedNetwork{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "bgp-l3a-",
					Labels:       map[string]string{"bgp-udn-layer3-no-overlay-snat-enabled-unmanaged": ""},
				},
				Spec: udnv1.ClusterUserDefinedNetworkSpec{
					Network: udnv1.NetworkSpec{
						Topology: udnv1.NetworkTopologyLayer3,
						Layer3: &udnv1.Layer3Config{
							Role: "Primary",
							Subnets: []udnv1.Layer3Subnet{{
								CIDR:       "103.103.0.0/16",
								HostSubnet: 24,
							}, {
								CIDR:       "2014:100:200::0/60",
								HostSubnet: 64,
							}},
						},
						Transport: udnv1.TransportOptionNoOverlay,
						NoOverlay: &udnv1.NoOverlayConfig{
							OutboundSNAT: udnv1.SNATEnabled,
							Routing:      udnv1.RoutingUnmanaged,
						},
					},
				},
			},
			&rav1.RouteAdvertisements{
				ObjectMeta: metav1.ObjectMeta{
					Name: "bgp-udn-layer3-no-overlay-snat-enabled-unmanaged-ra",
				},
				Spec: rav1.RouteAdvertisementsSpec{
					NetworkSelectors: apitypes.NetworkSelectors{
						apitypes.NetworkSelector{
							NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
							ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{
								NetworkSelector: metav1.LabelSelector{
									MatchLabels: map[string]string{"bgp-udn-layer3-no-overlay-snat-enabled-unmanaged": ""},
								},
							},
						},
					},
					NodeSelector:             metav1.LabelSelector{},
					FRRConfigurationSelector: metav1.LabelSelector{},
					Advertisements: []rav1.AdvertisementType{
						rav1.PodNetwork,
					},
				},
			},
		),
		ginkgo.Entry("layer3 no-overlay SNAT disabled unmanaged routing", feature.NoOverlay,
			&udnv1.ClusterUserDefinedNetwork{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "bgp-l3b-",
					Labels:       map[string]string{"bgp-udn-layer3-no-overlay-snat-disabled-unmanaged": ""},
				},
				Spec: udnv1.ClusterUserDefinedNetworkSpec{
					Network: udnv1.NetworkSpec{
						Topology: udnv1.NetworkTopologyLayer3,
						Layer3: &udnv1.Layer3Config{
							Role: "Primary",
							Subnets: []udnv1.Layer3Subnet{{
								CIDR:       "103.103.0.0/16",
								HostSubnet: 24,
							}, {
								CIDR:       "2014:100:200::0/60",
								HostSubnet: 64,
							}},
						},
						Transport: udnv1.TransportOptionNoOverlay,
						NoOverlay: &udnv1.NoOverlayConfig{
							OutboundSNAT: udnv1.SNATDisabled,
							Routing:      udnv1.RoutingUnmanaged,
						},
					},
				},
			},
			&rav1.RouteAdvertisements{
				ObjectMeta: metav1.ObjectMeta{
					Name: "bgp-udn-layer3-no-overlay-snat-disabled-unmanaged-ra",
				},
				Spec: rav1.RouteAdvertisementsSpec{
					NetworkSelectors: apitypes.NetworkSelectors{
						apitypes.NetworkSelector{
							NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
							ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{
								NetworkSelector: metav1.LabelSelector{
									MatchLabels: map[string]string{"bgp-udn-layer3-no-overlay-snat-disabled-unmanaged": ""},
								},
							},
						},
					},
					NodeSelector:             metav1.LabelSelector{},
					FRRConfigurationSelector: metav1.LabelSelector{},
					Advertisements: []rav1.AdvertisementType{
						rav1.PodNetwork,
					},
				},
			},
		),
		ginkgo.Entry("layer2",
			&udnv1.ClusterUserDefinedNetwork{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "bgp-l2-",
					Labels:       map[string]string{"bgp-l2": ""},
				},
				Spec: udnv1.ClusterUserDefinedNetworkSpec{
					Network: udnv1.NetworkSpec{
						Topology: udnv1.NetworkTopologyLayer2,
						Layer2: &udnv1.Layer2Config{
							Role:    "Primary",
							Subnets: udnv1.DualStackCIDRs{"103.0.0.0/16", "2014:100::0/60"},
						},
					},
				},
			},
			&rav1.RouteAdvertisements{
				ObjectMeta: metav1.ObjectMeta{
					Name: "bgp-l2-ra",
				},
				Spec: rav1.RouteAdvertisementsSpec{
					NetworkSelectors: apitypes.NetworkSelectors{
						apitypes.NetworkSelector{
							NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
							ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{
								NetworkSelector: metav1.LabelSelector{
									MatchLabels: map[string]string{"bgp-l2": ""},
								},
							},
						},
					},
					NodeSelector:             metav1.LabelSelector{},
					FRRConfigurationSelector: metav1.LabelSelector{},
					Advertisements: []rav1.AdvertisementType{
						rav1.PodNetwork,
					},
				},
			},
		),
	)
})

// With dynamic UDN allocation, a network only exists on nodes that run
// workloads attached to it and only those nodes get a subnet allocated for it.
// A pod-network RouteAdvertisements selecting such a network must not wait for
// subnets on the remaining nodes to reach the Accepted status, and must start
// advertising a node as soon as the network is activated on it.
var _ = ginkgo.Describe("BGP: When an advertised CUDN network is dynamically allocated", feature.RouteAdvertisementsDynamicUDN, func() {
	var nodes *corev1.NodeList

	f := wrappedTestFramework("dynamic-udn-route-advertisements")
	f.SkipNamespaceCreation = true

	ginkgo.BeforeEach(func() {
		if !isDynamicUDNEnabled() {
			ginkgo.Skip("test requires DYNAMIC_UDN_ALLOCATION=true")
		}
		namespace, err := f.CreateNamespace(context.TODO(), f.BaseName, map[string]string{
			"e2e-framework":           f.BaseName,
			RequiredUDNNamespaceLabel: "",
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		f.Namespace = namespace

		ginkgo.By("Selecting 3 schedulable nodes")
		nodes, err = e2enode.GetBoundedReadySchedulableNodes(context.TODO(), f.ClientSet, 3)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(len(nodes.Items)).To(gomega.BeNumerically(">", 2))
	})

	ginkgo.It("is accepted when the network is allocated on a subset of the nodes only", func(ctx ginkgo.SpecContext) {
		ginkgo.By("create ClusterUserDefinedNetwork")
		cudnTemplate := &udnv1.ClusterUserDefinedNetwork{
			ObjectMeta: metav1.ObjectMeta{
				// Keep generated CUDN names under the Linux 15-byte interface
				// limit so the VRF name is unique instead of network ID based.
				GenerateName: "bgp-dyn-",
				Labels:       map[string]string{"bgp-dynamic-udn": ""},
			},
			Spec: udnv1.ClusterUserDefinedNetworkSpec{
				NamespaceSelector: metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
					Key:      "kubernetes.io/metadata.name",
					Operator: metav1.LabelSelectorOpIn,
					Values:   []string{f.Namespace.Name},
				}}},
				Network: udnv1.NetworkSpec{
					Topology: udnv1.NetworkTopologyLayer3,
					Layer3: &udnv1.Layer3Config{
						Role: "Primary",
						// use a dedicated range: other suites advertise their
						// networks to the same external router and may run in
						// parallel with this test
						Subnets: []udnv1.Layer3Subnet{{
							CIDR:       "106.106.0.0/16",
							HostSubnet: 24,
						}, {
							CIDR:       "2016:100:200::0/60",
							HostSubnet: 64,
						}},
					},
				},
			},
		}
		cudnTemplate.Spec.Network.Layer3.Subnets = filterL3Subnets(f.ClientSet, cudnTemplate.Spec.Network.Layer3.Subnets)
		var v4CIDR, v6CIDR string
		for _, subnet := range cudnTemplate.Spec.Network.Layer3.Subnets {
			if utilnet.IsIPv6CIDRString(string(subnet.CIDR)) {
				v6CIDR = string(subnet.CIDR)
			} else {
				v4CIDR = string(subnet.CIDR)
			}
		}
		udnClient, err := udnclientset.NewForConfig(f.ClientConfig())
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		cUDN, err := udnClient.K8sV1().ClusterUserDefinedNetworks().Create(context.Background(), cudnTemplate, metav1.CreateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		ginkgo.DeferCleanup(func() {
			udnClient.K8sV1().ClusterUserDefinedNetworks().Delete(context.TODO(), cUDN.Name, metav1.DeleteOptions{})
		})
		gomega.Eventually(ctx, clusterUserDefinedNetworkReadyFunc(ctx, f.DynamicClient, cUDN.Name), 5*time.Second, time.Second).Should(gomega.Succeed())
		netName := types.CUDNPrefix + cUDN.Name

		ginkgo.DeferCleanup(func() {
			ginkgo.By(fmt.Sprintf("delete pods in %s namespace to unblock CUDN CR & associate NAD deletion", f.Namespace.Name))
			gomega.Expect(f.ClientSet.CoreV1().Pods(f.Namespace.Name).DeleteCollection(context.Background(), metav1.DeleteOptions{}, metav1.ListOptions{})).To(gomega.Succeed())
		})

		activeNode := nodes.Items[0]
		otherNode := nodes.Items[1]

		ginkgo.By(fmt.Sprintf("Creating a pod attached to the network on node %s only", activeNode.Name))
		_, err = createPod(f, "dynamic-udn-pod-1", activeNode.Name, f.Namespace.Name, []string{"bash", "-c", "sleep infinity"}, nil)
		framework.ExpectNoError(err)

		ginkgo.By(fmt.Sprintf("Node %s gets a subnet allocated for the network while the other nodes do not", activeNode.Name))
		gomega.Eventually(func() error {
			_, _, err := getNodePodCIDRs(activeNode.Name, netName)
			return err
		}, 30*time.Second, time.Second).Should(gomega.Succeed(),
			"node %s must have a subnet allocated for network %s", activeNode.Name, netName)
		expectNoUDNSubnetOnNodesExcept(nodes.Items, activeNode.Name, netName)

		ginkgo.By("create route advertisement advertising the network")
		raClient, err := raclientset.NewForConfig(f.ClientConfig())
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		ra := &rav1.RouteAdvertisements{
			ObjectMeta: metav1.ObjectMeta{
				Name: "bgp-dynamic-udn-ra",
			},
			Spec: rav1.RouteAdvertisementsSpec{
				NetworkSelectors: apitypes.NetworkSelectors{
					apitypes.NetworkSelector{
						NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
						ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{
							NetworkSelector: metav1.LabelSelector{
								MatchLabels: map[string]string{"bgp-dynamic-udn": ""},
							},
						},
					},
				},
				NodeSelector:             metav1.LabelSelector{},
				FRRConfigurationSelector: metav1.LabelSelector{},
				Advertisements: []rav1.AdvertisementType{
					rav1.PodNetwork,
				},
			},
		}
		ra, err = raClient.K8sV1().RouteAdvertisements().Create(context.TODO(), ra, metav1.CreateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		ginkgo.DeferCleanup(func() { raClient.K8sV1().RouteAdvertisements().Delete(context.TODO(), ra.Name, metav1.DeleteOptions{}) })

		ginkgo.By("ensure the route advertisement is accepted although the network is not allocated on all nodes")
		gomega.Eventually(routeAdvertisementsReadyFunc(*raClient, ra.Name), 30*time.Second, time.Second).Should(gomega.Succeed())

		checkNodeAdvertisedInFRR := func(node corev1.Node) {
			var podv4CIDR, podv6CIDR string
			gomega.Eventually(func() error {
				var err error
				podv4CIDR, podv6CIDR, err = getNodePodCIDRs(node.Name, netName)
				return err
			}, 30*time.Second, time.Second).Should(gomega.Succeed(),
				"node %s must have a subnet allocated for network %s", node.Name, netName)
			if isIPv4Supported(f.ClientSet) && podv4CIDR != "" {
				checkRouteInFRR(node, podv4CIDR, routerContainerName, false)
			}
			if isIPv6Supported(f.ClientSet) && podv6CIDR != "" {
				checkRouteInFRR(node, podv6CIDR, routerContainerName, true)
			}
		}

		ginkgo.By(fmt.Sprintf("ensure the network pod subnet of node %s is advertised to the external FRR router", activeNode.Name))
		checkNodeAdvertisedInFRR(activeNode)

		ginkgo.By("ensure nodes where the network is not allocated are not advertised to the external FRR router")
		expectNodesNotAdvertisedInFRR(nodes.Items, activeNode.Name, v4CIDR, v6CIDR)

		ginkgo.By(fmt.Sprintf("Creating a second pod on node %s activates the network on it and gets it advertised", otherNode.Name))
		_, err = createPod(f, "dynamic-udn-pod-2", otherNode.Name, f.Namespace.Name, []string{"bash", "-c", "sleep infinity"}, nil)
		framework.ExpectNoError(err)
		checkNodeAdvertisedInFRR(otherNode)

		gomega.Eventually(routeAdvertisementsReadyFunc(*raClient, ra.Name), 10*time.Second, time.Second).Should(gomega.Succeed(),
			"the route advertisement must remain accepted")
	})
})

var _ = ginkgo.Describe("BGP: isolation", feature.RouteAdvertisements, func() {
	ginkgo.DescribeTableSubtree("between advertised networks", func(cudnATemplate, cudnBTemplate *udnv1.ClusterUserDefinedNetwork) {
		const nodePortBackendLabel = "nodeport-backend"
		const clientNodeBackend = "client-node"
		const remoteNodeBackend = "remote-node"
		const nodePortNodeBackend = "nodeport-node"

		// The pod-to-X tests use e2epodoutput.RunHostCmdWithFullOutput(), which
		// returns output like "exit code 28" on error, while the host-to-X tests
		// use infraprovider.Get().ExecK8NodeCommand()), which returns output like
		// "exit status 28" ("status" rather than "code"). We just look for just
		// "28" (and hope there are no false positives).
		const curlConnectionTimeoutCode = "28"

		// Currently only pod-to-X tests ever expect "connection refused", so we
		// can match the longer error string.
		const curlConnectionRefusedCode = "exit code 7"

		f := wrappedTestFramework("bgp-network-isolation")
		f.SkipNamespaceCreation = true
		var udnNamespaceA, udnNamespaceB *corev1.Namespace
		var nodes *corev1.NodeList
		// podsNetA has 4 pods in cudnA, two on nodes[0], one on nodes[1], and one on nodes[2] - done in BeforeEach
		var podsNetA []*corev1.Pod

		// podNetB is in cudnB hosted on nodes[1], podNetDefault is in the default network hosted on nodes[1] - done in BeforeEach
		var podNetB, podNetDefault *corev1.Pod
		// nodesNetB are the nodes where cudnB is expected to exist: all of them,
		// or just the node running its only pod with dynamic UDN allocation
		var nodesNetB []corev1.Node
		var svcNodePortNetA, svcNodePortNetAClientNodeBackend, svcNodePortNetARemoteNodeBackend, svcNodePortNetANodePortNodeBackend, svcNodePortNetB, svcNodePortNetDefault, svcNodePortETPLocalDefault, svcNodePortETPLocalNetA *corev1.Service
		var cudnA, cudnB *udnv1.ClusterUserDefinedNetwork
		var ra *rav1.RouteAdvertisements
		var hostNetworkPort int
		ginkgo.Context("", ginkgo.Ordered, ginkgo.ContinueOnFailure, func() {
			ginkgo.BeforeAll(func(ctx ginkgo.SpecContext) {
				ginkgo.By("Configuring primary UDN namespaces")
				var err error
				// Create namespaces directly via the API instead of f.CreateNamespace()
				// to avoid framework cleaning them up in AfterEach
				udnNamespaceA, err = f.ClientSet.CoreV1().Namespaces().Create(context.TODO(), &corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						GenerateName: f.BaseName + "-",
						Labels: map[string]string{
							"e2e-framework":           f.BaseName,
							RequiredUDNNamespaceLabel: "",
						},
					},
				}, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				f.Namespace = udnNamespaceA
				udnNamespaceB, err = f.ClientSet.CoreV1().Namespaces().Create(context.TODO(), &corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						GenerateName: f.BaseName + "-",
						Labels: map[string]string{
							"e2e-framework":           f.BaseName,
							RequiredUDNNamespaceLabel: "",
						},
					},
				}, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				ginkgo.By("Configuring networks")
				cudnATemplate.Spec.NamespaceSelector = metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
					Key:      "kubernetes.io/metadata.name",
					Operator: metav1.LabelSelectorOpIn,
					Values:   []string{udnNamespaceA.Name},
				}}}
				cudnBTemplate.Spec.NamespaceSelector = metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
					Key:      "kubernetes.io/metadata.name",
					Operator: metav1.LabelSelectorOpIn,
					Values:   []string{udnNamespaceB.Name},
				}}}

				// set a common label used to advertise both networks with one RA
				cudnATemplate.Labels["advertised-networks-isolation"] = ""
				cudnBTemplate.Labels["advertised-networks-isolation"] = ""

				udnClient, err := udnclientset.NewForConfig(f.ClientConfig())
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				if cudnATemplate.Spec.Network.Layer3 != nil {
					cudnATemplate.Spec.Network.Layer3.Subnets = filterL3Subnets(f.ClientSet, cudnATemplate.Spec.Network.Layer3.Subnets)
				}
				if cudnATemplate.Spec.Network.Layer2 != nil {
					cudnATemplate.Spec.Network.Layer2.Subnets = filterDualStackCIDRs(f.ClientSet, cudnATemplate.Spec.Network.Layer2.Subnets)
				}
				if cudnBTemplate.Spec.Network.Layer3 != nil {
					cudnBTemplate.Spec.Network.Layer3.Subnets = filterL3Subnets(f.ClientSet, cudnBTemplate.Spec.Network.Layer3.Subnets)
				}
				if cudnBTemplate.Spec.Network.Layer2 != nil {
					cudnBTemplate.Spec.Network.Layer2.Subnets = filterDualStackCIDRs(f.ClientSet, cudnBTemplate.Spec.Network.Layer2.Subnets)
				}

				cudnA, err = udnClient.K8sV1().ClusterUserDefinedNetworks().Create(context.Background(), cudnATemplate, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				cudnB, err = udnClient.K8sV1().ClusterUserDefinedNetworks().Create(context.Background(), cudnBTemplate, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				ginkgo.By("Waiting for networks to be ready")
				gomega.Eventually(ctx, clusterUserDefinedNetworkReadyFunc(ctx, f.DynamicClient, cudnA.Name), 5*time.Second, time.Second).Should(gomega.Succeed())
				gomega.Eventually(ctx, clusterUserDefinedNetworkReadyFunc(ctx, f.DynamicClient, cudnB.Name), 5*time.Second, time.Second).Should(gomega.Succeed())

				ginkgo.By("Selecting 3 schedulable nodes")
				// bound the node list: network pods are only scheduled on the
				// first 3 nodes, and with dynamic UDN allocation the networks
				// are only expected on the nodes that run their pods
				nodes, err = e2enode.GetBoundedReadySchedulableNodes(context.TODO(), f.ClientSet, 3)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(len(nodes.Items)).To(gomega.BeNumerically(">", 2))
				// create host networked pod
				ginkgo.By("Creating host network pods on each node")
				// get random port in case the test retries and port is already in use on host node
				min := 25000
				max := 25999
				hostNetworkPort = rand.Intn(max-min+1) + min
				framework.Logf("Random host networked port chosen: %d", hostNetworkPort)

				ginkgo.By("Setting up pods and services")

				// Create all pod specs upfront as distinct objects.
				var hostNetPods []*corev1.Pod
				for _, node := range nodes.Items {
					p := e2epod.NewAgnhostPod(f.Namespace.Name, node.Name+"-hostnet-ep", nil, nil, nil,
						"netexec",
						fmt.Sprintf("--http-port=%d", hostNetworkPort),
						fmt.Sprintf("--udp-port=%d", hostNetworkPort))
					p.Spec.NodeName = node.Name
					p.Spec.HostNetwork = true
					hostNetPods = append(hostNetPods, e2epod.NewPodClient(f).Create(context.TODO(), p))
				}

				podNetASpecs := []*corev1.Pod{
					e2epod.NewAgnhostPod(udnNamespaceA.Name, fmt.Sprintf("pod-1-%s-net-%s", nodes.Items[0].Name, cudnA.Name), nil, nil, []corev1.ContainerPort{{ContainerPort: 8080}}, "netexec"),
					e2epod.NewAgnhostPod(udnNamespaceA.Name, fmt.Sprintf("pod-2-%s-net-%s", nodes.Items[0].Name, cudnA.Name), nil, nil, []corev1.ContainerPort{{ContainerPort: 8080}}, "netexec"),
					e2epod.NewAgnhostPod(udnNamespaceA.Name, fmt.Sprintf("pod-3-%s-net-%s", nodes.Items[1].Name, cudnA.Name), nil, nil, []corev1.ContainerPort{{ContainerPort: 8080}}, "netexec"),
					e2epod.NewAgnhostPod(udnNamespaceA.Name, fmt.Sprintf("pod-4-%s-net-%s", nodes.Items[2].Name, cudnA.Name), nil, nil, []corev1.ContainerPort{{ContainerPort: 8080}}, "netexec"),
				}
				for _, p := range podNetASpecs {
					p.Spec.NodeName = nodes.Items[0].Name
					p.Labels = map[string]string{"network": cudnA.Name}
				}
				podNetASpecs[2].Spec.NodeName = nodes.Items[1].Name
				podNetASpecs[2].Labels[nodePortBackendLabel] = remoteNodeBackend
				podNetASpecs[3].Spec.NodeName = nodes.Items[2].Name
				podNetASpecs[3].Labels[nodePortBackendLabel] = nodePortNodeBackend
				podNetASpecs[1].Labels[nodePortBackendLabel] = clientNodeBackend

				podNetBSpec := e2epod.NewAgnhostPod(udnNamespaceB.Name, fmt.Sprintf("pod-1-%s-net-%s", nodes.Items[1].Name, cudnB.Name), nil, nil, []corev1.ContainerPort{{ContainerPort: 8080}}, "netexec")
				podNetBSpec.Spec.NodeName = nodes.Items[1].Name
				podNetBSpec.Labels = map[string]string{"network": cudnB.Name}

				podNetDefaultSpec := e2epod.NewAgnhostPod("default", fmt.Sprintf("pod-1-%s-net-default", nodes.Items[1].Name), nil, nil, []corev1.ContainerPort{{ContainerPort: 8080}}, "netexec")
				podNetDefaultSpec.Spec.NodeName = nodes.Items[1].Name
				podNetDefaultSpec.Labels = map[string]string{"network": "default"}

				// Submit all pods to the API without waiting for readiness.
				podsNetA = []*corev1.Pod{}
				for _, p := range podNetASpecs {
					podsNetA = append(podsNetA, e2epod.NewPodClient(f).Create(context.TODO(), p))
				}
				podNetB = e2epod.PodClientNS(f, udnNamespaceB.Name).Create(context.TODO(), podNetBSpec)
				podNetDefault = e2epod.PodClientNS(f, "default").Create(context.TODO(), podNetDefaultSpec)

				// Create services (don't need pods to be ready).
				familyPolicy := corev1.IPFamilyPolicyPreferDualStack

				svc := e2eservice.CreateServiceSpec(fmt.Sprintf("service-%s", cudnA.Name), "", false, map[string]string{"network": cudnA.Name})
				svc.Spec.Ports = []corev1.ServicePort{{Port: 8080}}
				svc.Spec.IPFamilyPolicy = &familyPolicy
				svc.Spec.Type = corev1.ServiceTypeNodePort
				svcNodePortNetA, err = f.ClientSet.CoreV1().Services(udnNamespaceA.Name).Create(context.Background(), svc, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				createNodePortNetAService := func(name, backend string) *corev1.Service {
					svc := e2eservice.CreateServiceSpec(name, "", false, map[string]string{"network": cudnA.Name, nodePortBackendLabel: backend})
					svc.Spec.Ports = []corev1.ServicePort{{Port: 8080}}
					svc.Spec.IPFamilyPolicy = &familyPolicy
					svc.Spec.Type = corev1.ServiceTypeNodePort
					svc, err := f.ClientSet.CoreV1().Services(udnNamespaceA.Name).Create(context.Background(), svc, metav1.CreateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					return svc
				}
				svcNodePortNetAClientNodeBackend = createNodePortNetAService(fmt.Sprintf("service-%s-client-node-backend", cudnA.Name), clientNodeBackend)
				svcNodePortNetARemoteNodeBackend = createNodePortNetAService(fmt.Sprintf("service-%s-remote-node-backend", cudnA.Name), remoteNodeBackend)
				svcNodePortNetANodePortNodeBackend = createNodePortNetAService(fmt.Sprintf("service-%s-nodeport-node-backend", cudnA.Name), nodePortNodeBackend)

				svc.Name = fmt.Sprintf("service-%s", cudnB.Name)
				svc.Namespace = udnNamespaceB.Name
				svc.Spec.Selector = map[string]string{"network": cudnB.Name}
				svcNodePortNetB, err = f.ClientSet.CoreV1().Services(udnNamespaceB.Name).Create(context.Background(), svc, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				svc.Name = "service-default"
				svc.Namespace = "default"
				svc.Spec.Selector = map[string]string{"network": "default"}
				svc.Spec.Type = corev1.ServiceTypeNodePort
				svcNodePortNetDefault, err = f.ClientSet.CoreV1().Services("default").Create(context.Background(), svc, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// create one nodePort service with externalTrafficPolicy=Local in default namespace
				svc.Name = "nodeport-default-etp-local"
				svc.Spec.ExternalTrafficPolicy = corev1.ServiceExternalTrafficPolicyTypeLocal
				svcNodePortETPLocalDefault, err = f.ClientSet.CoreV1().Services("default").Create(context.Background(), svc, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// create one nodePort service with externalTrafficPolicy=Local in udnNamespaceA
				svc.Name = fmt.Sprintf("nodeport-etp-local-%s", cudnA.Name)
				svc.Namespace = udnNamespaceA.Name
				svc.Spec.Selector = map[string]string{"network": cudnA.Name}
				svcNodePortETPLocalNetA, err = f.ClientSet.CoreV1().Services(udnNamespaceA.Name).Create(context.Background(), svc, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Wait for all pods to be ready (they've been scheduling in parallel).
				for _, p := range append(hostNetPods, append(podsNetA, podNetB, podNetDefault)...) {
					framework.ExpectNoError(e2epod.WaitTimeoutForPodReadyInNamespace(context.TODO(), f.ClientSet, p.Name, p.Namespace, framework.PodStartTimeout))
				}
				// Re-get pods to have updated status (e.g. pod IPs).
				for i, p := range podsNetA {
					podsNetA[i], err = f.ClientSet.CoreV1().Pods(p.Namespace).Get(context.TODO(), p.Name, metav1.GetOptions{})
					framework.ExpectNoError(err)
				}
				podNetB, err = f.ClientSet.CoreV1().Pods(podNetB.Namespace).Get(context.TODO(), podNetB.Name, metav1.GetOptions{})
				framework.ExpectNoError(err)
				framework.Logf("created pod %s/%s", podNetB.Namespace, podNetB.Name)
				podNetDefault, err = f.ClientSet.CoreV1().Pods(podNetDefault.Namespace).Get(context.TODO(), podNetDefault.Name, metav1.GetOptions{})
				framework.ExpectNoError(err)

				ginkgo.By("Expose networks")
				ra = &rav1.RouteAdvertisements{
					ObjectMeta: metav1.ObjectMeta{
						GenerateName: "advertised-networks-isolation-ra",
					},
					Spec: rav1.RouteAdvertisementsSpec{
						NetworkSelectors: apitypes.NetworkSelectors{
							apitypes.NetworkSelector{
								NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
								ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{
									NetworkSelector: metav1.LabelSelector{
										MatchLabels: map[string]string{"advertised-networks-isolation": ""},
									},
								},
							},
						},
						NodeSelector:             metav1.LabelSelector{},
						FRRConfigurationSelector: metav1.LabelSelector{},
						Advertisements: []rav1.AdvertisementType{
							rav1.PodNetwork,
						},
					},
				}

				raClient, err := raclientset.NewForConfig(f.ClientConfig())
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				ra, err = raClient.K8sV1().RouteAdvertisements().Create(context.TODO(), ra, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				ginkgo.By("ensure route advertisement matching both networks was created successfully")
				gomega.Eventually(func() string {
					ra, err := raClient.K8sV1().RouteAdvertisements().Get(context.TODO(), ra.Name, metav1.GetOptions{})
					if err != nil {
						return ""
					}
					condition := meta.FindStatusCondition(ra.Status.Conditions, "Accepted")
					if condition == nil {
						return ""
					}
					return condition.Reason
				}, 30*time.Second, time.Second).Should(gomega.Equal("Accepted"))

				// Check CUDN TransportAccepted condition is True after RA is created
				expectedTransportMsg := func(t udnv1.TransportOption) string {
					switch t {
					case udnv1.TransportOptionNoOverlay:
						return "Transport has been configured as 'no-overlay'."
					case udnv1.TransportOptionEVPN:
						return "Transport has been configured as 'EVPN'."
					default:
						return ""
					}
				}
				expectedMsgByCUDN := map[string]string{
					cudnA.Name: expectedTransportMsg(cudnATemplate.Spec.Network.Transport),
					cudnB.Name: expectedTransportMsg(cudnBTemplate.Spec.Network.Transport),
				}
				if expectedMsgByCUDN[cudnA.Name] != "" || expectedMsgByCUDN[cudnB.Name] != "" {
					ginkgo.By("ensure CUDN TransportAccepted condition is True")
					for _, cudnName := range []string{cudnA.Name, cudnB.Name} {
						gomega.Eventually(func(g gomega.Gomega) {
							cudnObj, err := f.DynamicClient.Resource(clusterUDNGVR).Get(context.Background(), cudnName, metav1.GetOptions{}, "status")
							g.Expect(err).NotTo(gomega.HaveOccurred())
							conditions, err := getConditions(cudnObj)
							g.Expect(err).NotTo(gomega.HaveOccurred())
							for _, condition := range conditions {
								if condition.Type == "TransportAccepted" {
									g.Expect(string(condition.Status)).To(gomega.Equal("True"))
									g.Expect(condition.Message).To(gomega.Equal(expectedMsgByCUDN[cudnName]))
									return
								}
							}
							g.Expect(false).To(gomega.BeTrue(), "TransportAccepted condition not found in CUDN %s", cudnName)
						}, 30*time.Second, time.Second).Should(gomega.Succeed())
					}
				}

				// With dynamic UDN allocation, networks only exist on nodes that
				// run workloads attached to them: network A runs pods on every
				// node while network B only runs a pod on nodes[1].
				nodesNetB = nodes.Items
				if isDynamicUDNEnabled() {
					nodesNetB = []corev1.Node{nodes.Items[1]}
				}

				ginkgo.By("ensure routes from UDNs are learned by the external FRR router")
				serverContainerIPs := getBGPServerContainerIPs(f)
				for _, serverContainerIP := range serverContainerIPs {
					for _, node := range nodes.Items {
						if cudnA.Spec.Network.Topology == udnv1.NetworkTopologyLayer3 {
							checkL3NodePodRoute(node, serverContainerIP, routerContainerName, types.CUDNPrefix+cudnA.Name)
						} else {
							checkL2NodePodRoute(node, serverContainerIP, routerContainerName, cudnATemplate.Spec.Network.Layer2.Subnets)
						}
					}
					for _, node := range nodesNetB {
						if cudnB.Spec.Network.Topology == udnv1.NetworkTopologyLayer3 {
							checkL3NodePodRoute(node, serverContainerIP, routerContainerName, types.CUDNPrefix+cudnB.Name)
						} else {
							checkL2NodePodRoute(node, serverContainerIP, routerContainerName, cudnBTemplate.Spec.Network.Layer2.Subnets)
						}
					}
				}

				if isDynamicUDNEnabled() && cudnB.Spec.Network.Topology == udnv1.NetworkTopologyLayer3 {
					ginkgo.By("ensure nodes without workloads attached to network B get no subnet allocated for it")
					expectNoUDNSubnetOnNodesExcept(nodes.Items, podNetB.Spec.NodeName, types.CUDNPrefix+cudnB.Name)
				}
			})

			ginkgo.AfterAll(func() {
				if udnNamespaceA != nil {
					gomega.Expect(f.ClientSet.CoreV1().Pods(udnNamespaceA.Name).DeleteCollection(context.Background(), metav1.DeleteOptions{}, metav1.ListOptions{})).To(gomega.Succeed())
				}
				if udnNamespaceB != nil {
					gomega.Expect(f.ClientSet.CoreV1().Pods(udnNamespaceB.Name).DeleteCollection(context.Background(), metav1.DeleteOptions{}, metav1.ListOptions{})).To(gomega.Succeed())
				}
				udnClient, err := udnclientset.NewForConfig(f.ClientConfig())
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				if cudnB != nil {
					err = udnClient.K8sV1().ClusterUserDefinedNetworks().Delete(context.TODO(), cudnB.Name, metav1.DeleteOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					gomega.Eventually(func() bool {
						_, err := udnClient.K8sV1().ClusterUserDefinedNetworks().Get(context.TODO(), cudnB.Name, metav1.GetOptions{})
						return apierrors.IsNotFound(err)
					}, time.Second*60).Should(gomega.BeTrue())
					cudnB = nil
				}
				if cudnA != nil {
					err = udnClient.K8sV1().ClusterUserDefinedNetworks().Delete(context.TODO(), cudnA.Name, metav1.DeleteOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					gomega.Eventually(func() bool {
						_, err := udnClient.K8sV1().ClusterUserDefinedNetworks().Get(context.TODO(), cudnA.Name, metav1.GetOptions{})
						return apierrors.IsNotFound(err)
					}, time.Second*60).Should(gomega.BeTrue())
					cudnA = nil
				}

				if podNetDefault != nil {
					err = f.ClientSet.CoreV1().Pods(podNetDefault.Namespace).Delete(context.Background(), podNetDefault.Name, metav1.DeleteOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					podNetDefault = nil
				}

				if svcNodePortNetDefault != nil {
					err = f.ClientSet.CoreV1().Services(svcNodePortNetDefault.Namespace).Delete(context.Background(), svcNodePortNetDefault.Name, metav1.DeleteOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					svcNodePortNetDefault = nil
				}
				if svcNodePortETPLocalDefault != nil {
					err = f.ClientSet.CoreV1().Services(svcNodePortETPLocalDefault.Namespace).Delete(context.Background(), svcNodePortETPLocalDefault.Name, metav1.DeleteOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					svcNodePortETPLocalDefault = nil
				}

				raClient, err := raclientset.NewForConfig(f.ClientConfig())
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				if ra != nil {
					err = raClient.K8sV1().RouteAdvertisements().Delete(context.TODO(), ra.Name, metav1.DeleteOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					ra = nil
				}

				// Delete the namespaces manually since they were created directly
				// via the API (not via f.CreateNamespace) to avoid framework's
				// AfterEach cleanup.
				if udnNamespaceA != nil {
					err = f.ClientSet.CoreV1().Namespaces().Delete(context.Background(), udnNamespaceA.Name, metav1.DeleteOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					udnNamespaceA = nil
				}
				if udnNamespaceB != nil {
					err = f.ClientSet.CoreV1().Namespaces().Delete(context.Background(), udnNamespaceB.Name, metav1.DeleteOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					udnNamespaceB = nil
				}
			})

			nodePortServiceTarget := func(ipFamily utilnet.IPFamily, nodeName string, svc *corev1.Service) string {
				node, err := f.ClientSet.CoreV1().Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
				framework.ExpectNoError(err)
				nodeIPv4, nodeIPv6 := getNodeAddresses(node)
				nodeIP := nodeIPv4
				if ipFamily == utilnet.IPv6 {
					nodeIP = nodeIPv6
				}
				return net.JoinHostPort(nodeIP, fmt.Sprint(svc.Spec.Ports[0].NodePort)) + "/hostname"
			}

			ginkgo.DescribeTable("connectivity between networks",
				func(connInfo func(ipFamily utilnet.IPFamily) (clientName string, clientNamespace string, dst string, expectedOutput string, expectErr bool)) {
					// checkConnectivity performs a curl command from a specified client (pod or node)
					// to targetAddress. If clientNamespace is empty the function assumes clientName is a node that will be used as the
					// client.
					var checkConnectivity = func(clientName, clientNamespace, targetAddress string) (string, error) {
						curlCmd := []string{"curl", "-g", "-q", "-s", "--max-time", "1", "--insecure", targetAddress}
						var out string
						var err error
						if clientNamespace != "" {
							framework.Logf("Attempting connectivity from pod: %s/%s -> %s", clientNamespace, clientName, targetAddress)
							stdout, stderr, err := e2epodoutput.RunHostCmdWithFullOutput(clientNamespace, clientName, strings.Join(curlCmd, " "))
							out = stdout + "\n" + stderr
							if err != nil {
								return out, fmt.Errorf("connectivity check failed from Pod %s/%s to %s: %w", clientNamespace, clientName, targetAddress, err)
							}
						} else {
							framework.Logf("Attempting connectivity from node: %s -> %s", clientName, targetAddress)
							out, err = infraprovider.Get().ExecK8NodeCommand(clientName, curlCmd)
							if err != nil {
								// out is empty on error and error contains out...
								return err.Error(), fmt.Errorf("connectivity check failed from node %s to %s: %w", clientName, targetAddress, err)
							}
						}

						client := clientName
						if clientNamespace != "" {
							client = clientNamespace + "/" + client
						}
						framework.Logf("Connectivity check successful:'%s' -> %s", client, targetAddress)
						return out, nil
					}
					for _, ipFamily := range getSupportedIPFamiliesSlice(f.ClientSet) {
						clientName, clientNamespace, dst, expectedOutput, expectErr := connInfo(ipFamily)
						asyncAssertion := gomega.Eventually
						timeout := time.Second * 30
						if expectErr {
							// When the connectivity check is expected to fail it should be failing consistently
							asyncAssertion = gomega.Consistently
							timeout = time.Second * 5
						}
						asyncAssertion(func() error {
							out, err := checkConnectivity(clientName, clientNamespace, dst)
							if expectErr != (err != nil) {
								return fmt.Errorf("expected connectivity check to return error(%t), got %v, output %v", expectErr, err, out)
							}
							if expectedOutput != "" {
								if !strings.Contains(out, expectedOutput) {
									return fmt.Errorf("expected connectivity check to contain %q, got %q", expectedOutput, out)
								}
							}
							return nil
						}, timeout).Should(gomega.BeNil())
					}
				},
				ginkgo.Entry("pod to pod on the same network and same node should work",
					func(ipFamily utilnet.IPFamily) (clientName string, clientNamespace string, dst string, expectedOutput string, expectErr bool) {
						// podsNetA[0] and podsNetA[1] are on the same node
						clientPod := podsNetA[0]
						srvPod := podsNetA[1]

						clientPodStatus, err := getPodAnnotationForAttachment(clientPod, namespacedName(clientPod.Namespace, cudnA.Name))
						framework.ExpectNoError(err)
						srvPodStatus, err := getPodAnnotationForAttachment(srvPod, namespacedName(srvPod.Namespace, cudnA.Name))
						framework.ExpectNoError(err)
						return clientPod.Name, clientPod.Namespace, net.JoinHostPort(getFirstCIDROfFamily(ipFamily, srvPodStatus.IPs).IP.String(), "8080") + "/clientip",
							getFirstCIDROfFamily(ipFamily, clientPodStatus.IPs).IP.String(), false
					}),
				ginkgo.Entry("pod to pod on the same network and different nodes should work",
					func(ipFamily utilnet.IPFamily) (clientName string, clientNamespace string, dst string, expectedOutput string, expectErr bool) {
						// podsNetA[0] and podsNetA[2] are on different nodes
						clientPod := podsNetA[0]
						srvPod := podsNetA[2]

						clientPodStatus, err := getPodAnnotationForAttachment(clientPod, namespacedName(clientPod.Namespace, cudnA.Name))
						framework.ExpectNoError(err)
						srvPodStatus, err := getPodAnnotationForAttachment(srvPod, namespacedName(srvPod.Namespace, cudnA.Name))
						framework.ExpectNoError(err)
						return clientPod.Name, clientPod.Namespace, net.JoinHostPort(getFirstCIDROfFamily(ipFamily, srvPodStatus.IPs).IP.String(), "8080") + "/clientip",
							getFirstCIDROfFamily(ipFamily, clientPodStatus.IPs).IP.String(), false
					}),
				ginkgo.Entry("pod to pod connectivity on different networks and same node",
					func(ipFamily utilnet.IPFamily) (clientName string, clientNamespace string, dst string, expectedOutput string, expectErr bool) {
						// podsNetA[2] and podNetB are on the same node
						clientPod := podsNetA[2]
						srvPod := podNetB

						srvPodStatus, err := getPodAnnotationForAttachment(srvPod, namespacedName(srvPod.Namespace, cudnB.Name))
						framework.ExpectNoError(err)
						var (
							curlOutput string
							curlErr    bool
						)
						// Test behavior depends on the ADVERTISED_UDN_ISOLATION_MODE environment variable:
						// - "loose": Pod connectivity is allowed, test expects success
						// - anything else (including unset): Treated as "strict", pod connectivity is blocked
						if os.Getenv("ADVERTISED_UDN_ISOLATION_MODE") == "loose" {
							clientPodStatus, err := getPodAnnotationForAttachment(clientPod, namespacedName(clientPod.Namespace, cudnA.Name))
							framework.ExpectNoError(err)

							// With the above underlay routing configuration client pod can reach server pod.
							curlOutput = getFirstCIDROfFamily(ipFamily, clientPodStatus.IPs).IP.String()
							curlErr = false
						} else {
							curlOutput = curlConnectionTimeoutCode
							curlErr = true
						}
						return clientPod.Name, clientPod.Namespace, net.JoinHostPort(getFirstCIDROfFamily(ipFamily, srvPodStatus.IPs).IP.String(), "8080") + "/clientip",
							curlOutput, curlErr
					}),

				ginkgo.Entry("pod to pod connectivity on different networks and different nodes",
					func(ipFamily utilnet.IPFamily) (clientName string, clientNamespace string, dst string, expectedOutput string, expectErr bool) {
						// podsNetA[0] and podNetB are on different nodes
						clientPod := podsNetA[0]
						srvPod := podNetB

						srvPodStatus, err := getPodAnnotationForAttachment(srvPod, namespacedName(srvPod.Namespace, cudnB.Name))
						framework.ExpectNoError(err)
						var (
							curlOutput string
							curlErr    bool
						)
						if os.Getenv("ADVERTISED_UDN_ISOLATION_MODE") == "loose" {
							clientPodStatus, err := getPodAnnotationForAttachment(clientPod, namespacedName(clientPod.Namespace, cudnA.Name))
							framework.ExpectNoError(err)

							curlOutput = getFirstCIDROfFamily(ipFamily, clientPodStatus.IPs).IP.String()
							curlErr = false
						} else {
							curlOutput = curlConnectionTimeoutCode
							curlErr = true
						}
						return clientPod.Name, clientPod.Namespace, net.JoinHostPort(getFirstCIDROfFamily(ipFamily, srvPodStatus.IPs).IP.String(), "8080") + "/clientip",
							curlOutput, curlErr
					}),
				ginkgo.Entry("pod in the default network should not be able to access an advertised UDN pod on the same node",
					func(ipFamily utilnet.IPFamily) (clientName string, clientNamespace string, dst string, expectedOutput string, expectErr bool) {
						// podNetDefault and podNetB are on the same node
						clientPod := podNetDefault
						srvPod := podNetB

						srvPodStatus, err := getPodAnnotationForAttachment(srvPod, namespacedName(srvPod.Namespace, cudnB.Name))
						framework.ExpectNoError(err)
						return clientPod.Name, clientPod.Namespace, net.JoinHostPort(getFirstCIDROfFamily(ipFamily, srvPodStatus.IPs).IP.String(), "8080") + "/clientip",
							curlConnectionTimeoutCode, true
					}),
				ginkgo.Entry("pod in the default network should not be able to access an advertised UDN pod on a different node",
					func(ipFamily utilnet.IPFamily) (clientName string, clientNamespace string, dst string, expectedOutput string, expectErr bool) {
						// podNetDefault and podsNetA[0] are on different nodes
						clientPod := podNetDefault
						srvPod := podsNetA[0]

						srvPodStatus, err := getPodAnnotationForAttachment(srvPod, namespacedName(srvPod.Namespace, cudnA.Name))
						framework.ExpectNoError(err)
						return clientPod.Name, clientPod.Namespace, net.JoinHostPort(getFirstCIDROfFamily(ipFamily, srvPodStatus.IPs).IP.String(), "8080") + "/clientip",
							curlConnectionTimeoutCode, true
					}),
				ginkgo.Entry("pod in the default network should not be able to access a UDN service",
					func(ipFamily utilnet.IPFamily) (clientName string, clientNamespace string, dst string, expectedOutput string, expectErr bool) {
						return podNetDefault.Name, podNetDefault.Namespace, net.JoinHostPort(getFirstIPStringOfFamily(ipFamily, svcNodePortNetA.Spec.ClusterIPs), "8080") + "/clientip",
							curlConnectionTimeoutCode, true
					}),
				ginkgo.Entry("pod in the UDN should be able to access a service in the same network",
					func(ipFamily utilnet.IPFamily) (clientName string, clientNamespace string, dst string, expectedOutput string, expectErr bool) {
						return podsNetA[0].Name, podsNetA[0].Namespace, net.JoinHostPort(getFirstIPStringOfFamily(ipFamily, svcNodePortNetA.Spec.ClusterIPs), "8080") + "/clientip", "", false
					}),
				ginkgo.Entry("pod in the UDN should not be able to access a default network service",
					func(ipFamily utilnet.IPFamily) (clientName string, clientNamespace string, dst string, expectedOutput string, expectErr bool) {
						err := true
						out := curlConnectionTimeoutCode
						if cudnATemplate.Spec.Network.Topology == udnv1.NetworkTopologyLayer2 {
							// FIXME: prevent looping of traffic in L2 UDNs
							// bad behaviour: packet is looping from management port -> breth0 -> GR -> management port -> breth0 and so on
							// which is a never ending loop
							// this causes curl timeout with code 7 host unreachable instead of code 28
							out = ""
						}
						return podsNetA[0].Name, podsNetA[0].Namespace, net.JoinHostPort(getFirstIPStringOfFamily(ipFamily, svcNodePortNetDefault.Spec.ClusterIPs), "8080") + "/clientip", out, err
					}),
				ginkgo.Entry("pod in the UDN should be able to access kapi in default network service",
					func(ipFamily utilnet.IPFamily) (clientName string, clientNamespace string, dst string, expectedOutput string, expectErr bool) {
						return podsNetA[0].Name, podsNetA[0].Namespace, "https://kubernetes.default/healthz", "", false
					}),
				ginkgo.Entry("pod in the UDN should be able to access kapi service cluster IP directly",
					func(ipFamily utilnet.IPFamily) (clientName string, clientNamespace string, dst string, expectedOutput string, expectErr bool) {
						// Get kubernetes service from default namespace
						kubernetesService, err := f.ClientSet.CoreV1().Services("default").Get(context.TODO(), "kubernetes", metav1.GetOptions{})
						framework.ExpectNoError(err, "should be able to get kubernetes service")

						// NOTE: See https://github.com/kubernetes/enhancements/tree/master/keps/sig-network/2438-dual-stack-apiserver
						// Today the kubernetes.default service is single-stack and cannot be dual-stack.
						if isDualStackCluster(nodes) && ipFamily == utilnet.IPv6 {
							e2eskipper.Skipf("Dual stack kubernetes.default service is not supported in kubernetes")
						}
						// Get the cluster IP for the specified IP family
						clusterIP := getFirstIPStringOfFamily(ipFamily, kubernetesService.Spec.ClusterIPs)
						gomega.Expect(clusterIP).NotTo(gomega.BeEmpty(), fmt.Sprintf("no cluster IP available for IP family %v", ipFamily))

						// Access the kubernetes API at the cluster IP directly on port 443
						return podsNetA[0].Name, podsNetA[0].Namespace, fmt.Sprintf("https://%s/healthz", net.JoinHostPort(clusterIP, "443")), "", false
					}),
				ginkgo.Entry("pod in the UDN should not be able to access a service in a different UDN",
					func(ipFamily utilnet.IPFamily) (clientName string, clientNamespace string, dst string, expectedOutput string, expectErr bool) {
						return podsNetA[0].Name, podsNetA[0].Namespace, net.JoinHostPort(getFirstIPStringOfFamily(ipFamily, svcNodePortNetB.Spec.ClusterIPs), "8080") + "/clientip",
							curlConnectionTimeoutCode, true
					}),
				ginkgo.Entry("host to a local UDN pod should not work",
					func(ipFamily utilnet.IPFamily) (clientName string, clientNamespace string, dst string, expectedOutput string, expectErr bool) {
						clientNode := podsNetA[0].Spec.NodeName
						srvPod := podsNetA[0]

						srvPodStatus, err := getPodAnnotationForAttachment(srvPod, namespacedName(srvPod.Namespace, cudnA.Name))
						framework.ExpectNoError(err)
						return clientNode, "", net.JoinHostPort(getFirstCIDROfFamily(ipFamily, srvPodStatus.IPs).IP.String(), "8080") + "/clientip",
							curlConnectionTimeoutCode, true
					}),
				ginkgo.Entry("host to a different node UDN pod should not work",
					func(ipFamily utilnet.IPFamily) (clientName string, clientNamespace string, dst string, expectedOutput string, expectErr bool) {
						// podsNetA[0] and podsNetA[2] are on different nodes
						clientNode := podsNetA[2].Spec.NodeName
						srvPod := podsNetA[0]

						srvPodStatus, err := getPodAnnotationForAttachment(srvPod, namespacedName(srvPod.Namespace, cudnA.Name))
						framework.ExpectNoError(err)
						return clientNode, "", net.JoinHostPort(getFirstCIDROfFamily(ipFamily, srvPodStatus.IPs).IP.String(), "8080") + "/clientip",
							curlConnectionTimeoutCode, true
					}),
				ginkgo.Entry("UDN pod to local node should not work",
					func(ipFamily utilnet.IPFamily) (clientName string, clientNamespace string, dst string, expectedOutput string, expectErr bool) {
						clientPod := podsNetA[0]
						node, err := f.ClientSet.CoreV1().Nodes().Get(context.TODO(), clientPod.Spec.NodeName, metav1.GetOptions{})
						framework.ExpectNoError(err)
						nodeIPv4, nodeIPv6 := getNodeAddresses(node)
						nodeIP := nodeIPv4
						if ipFamily == utilnet.IPv6 {
							nodeIP = nodeIPv6
						}
						// FIXME: add the host process socket to the VRF for this test to work.
						// This scenario is something that is not supported yet. So the test will continue to fail.
						// This works the same on both normal UDNs and advertised UDNs.
						// So because the process is not bound to the VRF, packet reaches the host but kernel sends a RESET. So its not code 28 but code7.
						// 10:59:55.351067 319594f193d4d_3 P   ifindex 191 0a:58:5d:5d:01:05 ethertype IPv4 (0x0800), length 80: (tos 0x0, ttl 64, id 57264,
						//    offset 0, flags [DF], proto TCP (6), length 60)
						// 93.93.1.5.36363 > 172.18.0.2.25022: Flags [S], cksum 0x0aa5 (incorrect -> 0xe0b7), seq 3879759281, win 65280,
						//    options [mss 1360,sackOK,TS val 3006752321 ecr 0,nop,wscale 7], length 0
						// 10:59:55.352404 ovn-k8s-mp87 In  ifindex 186 0a:58:5d:5d:01:01 ethertype IPv4 (0x0800), length 80: (tos 0x0, ttl 63, id 57264,
						//    offset 0, flags [DF], proto TCP (6), length 60)
						//    169.154.169.12.36363 > 172.18.0.2.25022: Flags [S], cksum 0xe0b7 (correct), seq 3879759281, win 65280,
						//    options [mss 1360,sackOK,TS val 3006752321 ecr 0,nop,wscale 7], length 0
						// 10:59:55.352461 ovn-k8s-mp87 Out ifindex 186 0a:58:5d:5d:01:02 ethertype IPv4 (0x0800), length 60: (tos 0x0, ttl 64, id 0,
						//    offset 0, flags [DF], proto TCP (6), length 40)
						//    172.18.0.2.25022 > 169.154.169.12.36363: Flags [R.], cksum 0x609d (correct), seq 0, ack 3879759282, win 0, length 0
						//    10:59:55.352927 319594f193d4d_3 Out ifindex 191 0a:58:5d:5d:01:02 ethertype IPv4 (0x0800), length 60: (tos 0x0, ttl 64, id 0,
						//    offset 0, flags [DF], proto TCP (6), length 40)
						//    172.18.0.2.25022 > 93.93.1.5.36363: Flags [R.], cksum 0x609d (correct), seq 0, ack 1, win 0, length 0
						return clientPod.Name, clientPod.Namespace, net.JoinHostPort(nodeIP, fmt.Sprint(hostNetworkPort)) + "/hostname", "", true
					}),
				ginkgo.Entry("UDN pod to a different node should work",
					func(ipFamily utilnet.IPFamily) (clientName string, clientNamespace string, dst string, expectedOutput string, expectErr bool) {
						clientPod := podsNetA[0]
						// podsNetA[0] and podsNetA[2] are on different nodes so we can pick the node of podsNetA[2] as the different node destination
						node, err := f.ClientSet.CoreV1().Nodes().Get(context.TODO(), podsNetA[2].Spec.NodeName, metav1.GetOptions{})
						framework.ExpectNoError(err)
						nodeIPv4, nodeIPv6 := getNodeAddresses(node)
						nodeIP := nodeIPv4
						if ipFamily == utilnet.IPv6 {
							nodeIP = nodeIPv6
						}

						clientNode, err := f.ClientSet.CoreV1().Nodes().Get(context.TODO(), clientPod.Spec.NodeName, metav1.GetOptions{})
						framework.ExpectNoError(err)
						clientNodeIPv4, clientNodeIPv6 := getNodeAddresses(clientNode)
						clientNodeIP := clientNodeIPv4
						if ipFamily == utilnet.IPv6 {
							clientNodeIP = clientNodeIPv6
						}
						// pod -> node traffic should use the node's IP as the source for advertised UDNs.
						return clientPod.Name, clientPod.Namespace, net.JoinHostPort(nodeIP, fmt.Sprint(hostNetworkPort)) + "/clientip", clientNodeIP, false
					}),
				ginkgo.Entry("[ETP=Cluster] UDN pod to the same node nodeport service in default network should not work",
					// FIXME: https://github.com/ovn-kubernetes/ovn-kubernetes/issues/5419
					func(ipFamily utilnet.IPFamily) (clientName string, clientNamespace string, dst string, expectedOutput string, expectErr bool) {
						clientPod := podsNetA[0]
						// podsNetA[0] is on nodes[0]. We need the same node. Let's hit the nodeport on nodes[0].
						node, err := f.ClientSet.CoreV1().Nodes().Get(context.TODO(), nodes.Items[0].Name, metav1.GetOptions{})
						framework.ExpectNoError(err)
						nodeIPv4, nodeIPv6 := getNodeAddresses(node)
						nodeIP := nodeIPv4
						if ipFamily == utilnet.IPv6 {
							nodeIP = nodeIPv6
						}
						nodePort := svcNodePortNetDefault.Spec.Ports[0].NodePort

						// Because of the difference in IPv4 vs IPv6 "fib daddr type local" (see #5419, above),
						// forbidden IPv4 NodePort connections do not match the nftables rule, don't get DNATted,
						// and then get rejected by the host, while forbidden IPv6 NodePort connections do get
						// DNATted but then get dropped by OVN ACLs.
						expectedOutput = curlConnectionRefusedCode
						if ipFamily == utilnet.IPv6 {
							expectedOutput = curlConnectionTimeoutCode
						}

						return clientPod.Name, clientPod.Namespace, net.JoinHostPort(nodeIP, fmt.Sprint(nodePort)) + "/hostname", expectedOutput, true
					}),
				ginkgo.Entry("[ETP=Cluster] UDN pod to a different node nodeport service in default network should work",
					func(ipFamily utilnet.IPFamily) (clientName string, clientNamespace string, dst string, expectedOutput string, expectErr bool) {
						clientPod := podsNetA[0]
						// podsNetA[0] is on nodes[0]. We need a different node. podNetDefault is on nodes[1].
						// The service is backed by podNetDefault. Let's hit the nodeport on nodes[2].
						node, err := f.ClientSet.CoreV1().Nodes().Get(context.TODO(), nodes.Items[2].Name, metav1.GetOptions{})
						framework.ExpectNoError(err)
						nodeIPv4, nodeIPv6 := getNodeAddresses(node)
						nodeIP := nodeIPv4
						if ipFamily == utilnet.IPv6 {
							nodeIP = nodeIPv6
						}
						nodePort := svcNodePortNetDefault.Spec.Ports[0].NodePort

						return clientPod.Name, clientPod.Namespace, net.JoinHostPort(nodeIP, fmt.Sprint(nodePort)) + "/hostname", "", false
					}),
				ginkgo.Entry("[ETP=Cluster] UDN pod to the same node nodeport service in same UDN network should work",
					func(ipFamily utilnet.IPFamily) (clientName string, clientNamespace string, dst string, expectedOutput string, expectErr bool) {
						clientPod := podsNetA[0]
						// The service is backed by pods in podsNetA.
						// We want to hit the nodeport on the same node.
						// client is on nodes[0]. Let's hit nodeport on nodes[0].
						node, err := f.ClientSet.CoreV1().Nodes().Get(context.TODO(), nodes.Items[0].Name, metav1.GetOptions{})
						framework.ExpectNoError(err)
						nodeIPv4, nodeIPv6 := getNodeAddresses(node)
						nodeIP := nodeIPv4
						if ipFamily == utilnet.IPv6 {
							nodeIP = nodeIPv6
						}
						nodePort := svcNodePortNetA.Spec.Ports[0].NodePort

						// The service can be backed by any of the pods in podsNetA, so we can't reliably check the output hostname.
						// Just check that the connection is successful.
						return clientPod.Name, clientPod.Namespace, net.JoinHostPort(nodeIP, fmt.Sprint(nodePort)) + "/hostname", "", false
					}),
				ginkgo.Entry("[ETP=Cluster] UDN pod to a different node nodeport service in same UDN network with backend on client node should work",
					func(ipFamily utilnet.IPFamily) (clientName string, clientNamespace string, dst string, expectedOutput string, expectErr bool) {
						clientPod := podsNetA[0]
						target := nodePortServiceTarget(ipFamily, nodes.Items[2].Name, svcNodePortNetAClientNodeBackend)
						return clientPod.Name, clientPod.Namespace, target, podsNetA[1].Name, false
					}),
				ginkgo.Entry("[ETP=Cluster] UDN pod to a different node nodeport service in same UDN network with backend on nodeport node should work",
					func(ipFamily utilnet.IPFamily) (clientName string, clientNamespace string, dst string, expectedOutput string, expectErr bool) {
						clientPod := podsNetA[0]
						target := nodePortServiceTarget(ipFamily, nodes.Items[2].Name, svcNodePortNetANodePortNodeBackend)
						return clientPod.Name, clientPod.Namespace, target, podsNetA[3].Name, false
					}),
				ginkgo.Entry("[ETP=Cluster] UDN pod to a different node nodeport service in same UDN network with backend on a different node should work",
					func(ipFamily utilnet.IPFamily) (clientName string, clientNamespace string, dst string, expectedOutput string, expectErr bool) {
						clientPod := podsNetA[0]
						target := nodePortServiceTarget(ipFamily, nodes.Items[2].Name, svcNodePortNetARemoteNodeBackend)
						return clientPod.Name, clientPod.Namespace, target, podsNetA[2].Name, false
					}),
				ginkgo.Entry("[ETP=Cluster] UDN pod to the same node nodeport service in different UDN network should only work for IPv6",
					// FIXME: IPv4 should also work: https://github.com/ovn-kubernetes/ovn-kubernetes/issues/5419
					//
					// Cross-UDN same-node nodeport traffic goes through the management port in both
					// local and shared gateway modes (the priority 1004 LRP on the cluster router
					// reroutes traffic destined for the local node's IP to the management port):
					// UDN pod in networkA -> ovn-switch -> ovn-cluster-router (SNAT to masqueradeIP)
					// -> mpX interface -> host nftables (DNAT nodePort->clusterIP) -> breth0 table 0 -> table 2
					//
					// IPv6 works: nftables "fib daddr type local" succeeds for IPv6 regardless of
					// incoming interface, so the pkt_mark is set and breth0 table 2 dispatches the
					// packet to networkB's patch port. The mask narrowing (/112 -> /128) on the GR
					// external port makes the static route (fd69::/112 -> fd69::4) take effect for
					// the reply, routing it back through the management port to networkA.
					//
					// IPv4 fails: the kernel's IPv4 FIB lookup in "fib daddr type local" is
					// interface-aware. Since the node IP (e.g. 172.18.0.2) is local on breth0, not
					// on ovn-k8s-mpX, the lookup fails and the pkt_mark is never set:
					//   $ ip route get 172.18.0.2 iif ovn-k8s-mp7
					//   RTNETLINK answers: Invalid argument
					//   $ ip -6 route get fc00:f853:ccd:e793::2 iif ovn-k8s-mp7
					//   local fc00:f853:ccd:e793::2 ... iif ovn-k8s-mp7 pref medium
					// Without the mark, the packet hits the priority=200 drop flow in breth0 table 2
					// (nw_src=masqueradeIP -> drop).
					// The original pkt_mark design (PR#4792) was intended for external clients hitting
					// the node IP, not for cross-UDN traffic arriving on a UDN management port.
					func(ipFamily utilnet.IPFamily) (clientName string, clientNamespace string, dst string, expectedOutput string, expectErr bool) {
						clientPod := podsNetA[0]
						node, err := f.ClientSet.CoreV1().Nodes().Get(context.TODO(), nodes.Items[0].Name, metav1.GetOptions{})
						framework.ExpectNoError(err)
						nodeIPv4, nodeIPv6 := getNodeAddresses(node)
						nodeIP := nodeIPv4
						if ipFamily == utilnet.IPv6 {
							nodeIP = nodeIPv6
						}
						nodePort := svcNodePortNetB.Spec.Ports[0].NodePort
						expectErr = ipFamily == utilnet.IPv4
						if isDynamicUDNEnabled() {
							// network B is not active on the client's node: the
							// nodeport is unreachable for both families
							expectErr = true
						}
						if expectErr {
							expectedOutput = curlConnectionRefusedCode
						}
						return clientPod.Name, clientPod.Namespace, net.JoinHostPort(nodeIP, fmt.Sprint(nodePort)) + "/hostname", expectedOutput, expectErr
					}),
				ginkgo.Entry("[ETP=Cluster] UDN pod to a different node nodeport service in different UDN network should work",
					func(ipFamily utilnet.IPFamily) (clientName string, clientNamespace string, dst string, expectedOutput string, expectErr bool) {
						clientPod := podsNetA[0]
						// The service is backed by podNetB.
						// We want to hit the nodeport on a different node from the client.
						// client is on nodes[0]. Let's hit nodeport on nodes[2]. With
						// dynamic UDN allocation the service is only reachable through
						// nodes where network B is active, so use the backend's node
						// nodes[1] instead, still different from the client's.
						targetNodeName := nodes.Items[2].Name
						if isDynamicUDNEnabled() {
							targetNodeName = podNetB.Spec.NodeName
						}
						node, err := f.ClientSet.CoreV1().Nodes().Get(context.TODO(), targetNodeName, metav1.GetOptions{})
						framework.ExpectNoError(err)
						nodeIPv4, nodeIPv6 := getNodeAddresses(node)
						nodeIP := nodeIPv4
						if ipFamily == utilnet.IPv6 {
							nodeIP = nodeIPv6
						}
						nodePort := svcNodePortNetB.Spec.Ports[0].NodePort

						// sourceIP will be joinSubnetIP for nodeports, so only using hostname endpoint
						return clientPod.Name, clientPod.Namespace, net.JoinHostPort(nodeIP, fmt.Sprint(nodePort)) + "/hostname", "", false
					}),
				ginkgo.Entry("[ETP=Cluster] UDN pod to a nodeport service in a different UDN network via a node where that network is not active should not work",
					func(ipFamily utilnet.IPFamily) (clientName string, clientNamespace string, dst string, expectedOutput string, expectErr bool) {
						if !isDynamicUDNEnabled() {
							e2eskipper.Skipf("Test requires dynamic UDN allocation: networks are active on every node otherwise")
						}
						// Documented limitation of dynamic UDN allocation (okep-5552):
						// NodePort services with external traffic policy "Cluster" do
						// not work when sending traffic to nodes where the backing
						// network is not active. Network B is only active on nodes[1],
						// so its nodeport service is not reachable through nodes[2].
						clientPod := podsNetA[0]
						node, err := f.ClientSet.CoreV1().Nodes().Get(context.TODO(), nodes.Items[2].Name, metav1.GetOptions{})
						framework.ExpectNoError(err)
						nodeIPv4, nodeIPv6 := getNodeAddresses(node)
						nodeIP := nodeIPv4
						if ipFamily == utilnet.IPv6 {
							nodeIP = nodeIPv6
						}
						nodePort := svcNodePortNetB.Spec.Ports[0].NodePort

						return clientPod.Name, clientPod.Namespace, net.JoinHostPort(nodeIP, fmt.Sprint(nodePort)) + "/hostname", "", true
					}),
				ginkgo.Entry("[ETP=LOCAL] UDN pod to the same node nodeport service in same UDN network should work",
					func(ipFamily utilnet.IPFamily) (clientName string, clientNamespace string, dst string, expectedOutput string, expectErr bool) {
						clientPod := podsNetA[0]
						node, err := f.ClientSet.CoreV1().Nodes().Get(context.TODO(), clientPod.Spec.NodeName, metav1.GetOptions{})
						framework.ExpectNoError(err)
						nodeIPv4, nodeIPv6 := getNodeAddresses(node)
						nodeIP := nodeIPv4
						if ipFamily == utilnet.IPv6 {
							nodeIP = nodeIPv6
						}
						nodePortA := svcNodePortETPLocalNetA.Spec.Ports[0].NodePort
						return clientPod.Name, clientPod.Namespace, net.JoinHostPort(nodeIP, fmt.Sprint(nodePortA)) + "/hostname", "", false
					}),

				ginkgo.Entry("[ETP=LOCAL] UDN pod to a different node nodeport service in same UDN network should work",
					func(ipFamily utilnet.IPFamily) (clientName string, clientNamespace string, dst string, expectedOutput string, expectErr bool) {
						clientPod := podsNetA[0]
						node, err := f.ClientSet.CoreV1().Nodes().Get(context.TODO(), podsNetA[2].Spec.NodeName, metav1.GetOptions{})
						framework.ExpectNoError(err)
						nodeIPv4, nodeIPv6 := getNodeAddresses(node)
						nodeIP := nodeIPv4
						if ipFamily == utilnet.IPv6 {
							nodeIP = nodeIPv6
						}
						nodePortA := svcNodePortETPLocalNetA.Spec.Ports[0].NodePort
						return clientPod.Name, clientPod.Namespace, net.JoinHostPort(nodeIP, fmt.Sprint(nodePortA)) + "/hostname", "", false
					}),

				ginkgo.Entry("[ETP=LOCAL] UDN pod to the same node nodeport service in different UDN network should only work for IPv6 in SGW",
					// FIXME: IPv4 should also work: https://github.com/ovn-kubernetes/ovn-kubernetes/issues/5419
					// Shared Gateway (SGW):
					//   IPv4 fails because nftables "fib daddr type local" does not match when the
					//   packet arrives on ovn-k8s-mpX (kernel IPv4 FIB lookup is interface-aware).
					//   Without the pkt_mark, the packet is dropped by breth0 table 2 isolation flows.
					//   IPv6 works because "fib daddr type local" succeeds for IPv6 regardless of
					//   incoming interface, so pkt_mark is set and breth0 dispatches the packet correctly.
					// Local Gateway (LGW):
					//   Both IPv4 and IPv6 fail. ETP=Local DNATs to the host masquerade IP (fd69::3 /
					//   169.254.0.3), which routes back to the UDN management port (each UDN routing
					//   table has a route for the masquerade IP pointing to its own mp), creating a
					//   routing loop between the management port and OVN.
					func(ipFamily utilnet.IPFamily) (clientName string, clientNamespace string, dst string, expectedOutput string, expectErr bool) {
						clientPod := podNetB
						node, err := f.ClientSet.CoreV1().Nodes().Get(context.TODO(), clientPod.Spec.NodeName, metav1.GetOptions{})
						framework.ExpectNoError(err)
						nodeIPv4, nodeIPv6 := getNodeAddresses(node)
						nodeIP := nodeIPv4
						if ipFamily == utilnet.IPv6 {
							nodeIP = nodeIPv6
						}
						nodePortA := svcNodePortETPLocalNetA.Spec.Ports[0].NodePort
						expectErr = ipFamily == utilnet.IPv4 || IsGatewayModeLocal(f.ClientSet)
						if expectErr {
							// Because of the difference in IPv4 vs IPv6 "fib daddr type local" (see #5419, above),
							// forbidden IPv4 NodePort connections do not match the nftables rule, don't get DNATted,
							// and then get rejected by the host, while forbidden IPv6 NodePort connections do get
							// DNATted but then get dropped by OVN ACLs.
							expectedOutput = curlConnectionRefusedCode
							if ipFamily == utilnet.IPv6 {
								expectedOutput = curlConnectionTimeoutCode
							}
						}
						return clientPod.Name, clientPod.Namespace, net.JoinHostPort(nodeIP, fmt.Sprint(nodePortA)) + "/hostname", expectedOutput, expectErr
					}),
				ginkgo.Entry("[ETP=LOCAL] UDN pod to a different node nodeport service in different UDN network should work",
					func(ipFamily utilnet.IPFamily) (clientName string, clientNamespace string, dst string, expectedOutput string, expectErr bool) {
						clientPod := podNetB
						node, err := f.ClientSet.CoreV1().Nodes().Get(context.TODO(), podsNetA[0].Spec.NodeName, metav1.GetOptions{})
						framework.ExpectNoError(err)
						nodeIPv4, nodeIPv6 := getNodeAddresses(node)
						nodeIP := nodeIPv4
						if ipFamily == utilnet.IPv6 {
							nodeIP = nodeIPv6
						}
						nodePortA := svcNodePortETPLocalNetA.Spec.Ports[0].NodePort
						return clientPod.Name, clientPod.Namespace, net.JoinHostPort(nodeIP, fmt.Sprint(nodePortA)) + "/hostname", "", false
					}),
				ginkgo.Entry("[ETP=LOCAL] UDN pod to the same node nodeport service in default network should not work",
					func(ipFamily utilnet.IPFamily) (clientName string, clientNamespace string, dst string, expectedOutput string, expectErr bool) {
						// FIXME: This test should work: https://github.com/ovn-kubernetes/ovn-kubernetes/issues/5419
						clientPod := podNetB
						node, err := f.ClientSet.CoreV1().Nodes().Get(context.TODO(), clientPod.Spec.NodeName, metav1.GetOptions{})
						framework.ExpectNoError(err)
						nodeIPv4, nodeIPv6 := getNodeAddresses(node)
						nodeIP := nodeIPv4
						if ipFamily == utilnet.IPv6 {
							nodeIP = nodeIPv6
						}
						nodePortB := svcNodePortETPLocalDefault.Spec.Ports[0].NodePort
						// Because of the difference in IPv4 vs IPv6 "fib daddr type local" (see #5419, above),
						// forbidden IPv4 NodePort connections do not match the nftables rule, don't get DNATted,
						// and then get rejected by the host, while forbidden IPv6 NodePort connections do get
						// DNATted but then get dropped by OVN ACLs.
						expectedOutput = curlConnectionRefusedCode
						if ipFamily == utilnet.IPv6 {
							expectedOutput = curlConnectionTimeoutCode
						}

						return clientPod.Name, clientPod.Namespace, net.JoinHostPort(nodeIP, fmt.Sprint(nodePortB)) + "/hostname", expectedOutput, true
					}),
				ginkgo.Entry("[ETP=LOCAL] UDN pod to a different node nodeport service in default network should work",
					func(ipFamily utilnet.IPFamily) (clientName string, clientNamespace string, dst string, expectedOutput string, expectErr bool) {
						// podsNetA[0] is on nodes[0]. We need a different node. podNetDefault is on nodes[1].
						// So we hit nodeport on nodes[1].
						clientPod := podsNetA[0]
						node, err := f.ClientSet.CoreV1().Nodes().Get(context.TODO(), podNetDefault.Spec.NodeName, metav1.GetOptions{})
						framework.ExpectNoError(err)
						nodeIPv4, nodeIPv6 := getNodeAddresses(node)
						nodeIP := nodeIPv4
						if ipFamily == utilnet.IPv6 {
							nodeIP = nodeIPv6
						}
						nodePortB := svcNodePortETPLocalDefault.Spec.Ports[0].NodePort
						return clientPod.Name, clientPod.Namespace, net.JoinHostPort(nodeIP, fmt.Sprint(nodePortB)) + "/hostname", "", false
					}),
				ginkgo.Entry("[ETP=LOCAL] Default network pod to same node nodeport service in UDN network should not work",
					func(ipFamily utilnet.IPFamily) (clientName string, clientNamespace string, dst string, expectedOutput string, expectErr bool) {
						// FIXME: This test should work: https://github.com/ovn-kubernetes/ovn-kubernetes/issues/5419
						clientPod := podNetDefault
						node, err := f.ClientSet.CoreV1().Nodes().Get(context.TODO(), clientPod.Spec.NodeName, metav1.GetOptions{})
						framework.ExpectNoError(err)
						nodeIPv4, nodeIPv6 := getNodeAddresses(node)
						nodeIP := nodeIPv4
						if ipFamily == utilnet.IPv6 {
							nodeIP = nodeIPv6
						}
						nodePortA := svcNodePortETPLocalNetA.Spec.Ports[0].NodePort
						return clientPod.Name, clientPod.Namespace, net.JoinHostPort(nodeIP, fmt.Sprint(nodePortA)) + "/hostname", curlConnectionTimeoutCode, true
					}),
				ginkgo.Entry("[ETP=LOCAL] Default network pod to different node nodeport service in UDN network should work",
					func(ipFamily utilnet.IPFamily) (clientName string, clientNamespace string, dst string, expectedOutput string, expectErr bool) {
						// podNetDefault is on nodes[1]. We need a different node. podsNetA[0] is on nodes[0].
						// So we hit nodeport on nodes[0].
						clientPod := podNetDefault
						node, err := f.ClientSet.CoreV1().Nodes().Get(context.TODO(), podsNetA[0].Spec.NodeName, metav1.GetOptions{})
						framework.ExpectNoError(err)
						nodeIPv4, nodeIPv6 := getNodeAddresses(node)
						nodeIP := nodeIPv4
						if ipFamily == utilnet.IPv6 {
							nodeIP = nodeIPv6
						}
						nodePortA := svcNodePortETPLocalNetA.Spec.Ports[0].NodePort
						return clientPod.Name, clientPod.Namespace, net.JoinHostPort(nodeIP, fmt.Sprint(nodePortA)) + "/hostname", "", false
					}),
			)
		})
	},
		ginkgo.Entry("Layer3",
			&udnv1.ClusterUserDefinedNetwork{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "bgp-l3a-",
					Labels:       map[string]string{"bgp-l3a": ""},
				},
				Spec: udnv1.ClusterUserDefinedNetworkSpec{
					Network: udnv1.NetworkSpec{
						Topology: udnv1.NetworkTopologyLayer3,
						Layer3: &udnv1.Layer3Config{
							Role:    "Primary",
							Subnets: cudnLayer3SubnetsA(),
						},
					},
				},
			}, &udnv1.ClusterUserDefinedNetwork{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "bgp-l3b-",
					Labels:       map[string]string{"bgp-l3b": ""},
				},
				Spec: udnv1.ClusterUserDefinedNetworkSpec{
					Network: udnv1.NetworkSpec{
						Topology: udnv1.NetworkTopologyLayer3,
						Layer3: &udnv1.Layer3Config{
							Role:    "Primary",
							Subnets: cudnLayer3SubnetsB(),
						},
					},
				},
			},
		),
		ginkgo.Entry("Layer3 no-overlay SNAT disabled unmanaged routing", feature.NoOverlay,
			&udnv1.ClusterUserDefinedNetwork{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "bgp-l3-noovl-unmgd-net-a",
					Labels: map[string]string{"bgp-l3-noovl-unmgd-net-a": ""},
				},
				Spec: udnv1.ClusterUserDefinedNetworkSpec{
					Network: udnv1.NetworkSpec{
						Topology: udnv1.NetworkTopologyLayer3,
						Layer3: &udnv1.Layer3Config{
							Role: "Primary",
							Subnets: []udnv1.Layer3Subnet{{
								CIDR:       "102.102.0.0/16",
								HostSubnet: 24,
							}, {
								CIDR:       "2013:100:200::0/60",
								HostSubnet: 64,
							}},
						},
						Transport: udnv1.TransportOptionNoOverlay,
						NoOverlay: &udnv1.NoOverlayConfig{
							OutboundSNAT: udnv1.SNATDisabled,
							Routing:      udnv1.RoutingUnmanaged,
						},
					},
				},
			}, &udnv1.ClusterUserDefinedNetwork{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "bgp-l3-noovl-unmgd-net-b",
					Labels: map[string]string{"bgp-l3-noovl-unmgd-net-b": ""},
				},
				Spec: udnv1.ClusterUserDefinedNetworkSpec{
					Network: udnv1.NetworkSpec{
						Topology: udnv1.NetworkTopologyLayer3,
						Layer3: &udnv1.Layer3Config{
							Role: "Primary",
							Subnets: []udnv1.Layer3Subnet{{
								CIDR:       "103.103.0.0/16",
								HostSubnet: 24,
							}, {
								CIDR:       "2014:100:200::0/60",
								HostSubnet: 64,
							}},
						},
						Transport: udnv1.TransportOptionNoOverlay,
						NoOverlay: &udnv1.NoOverlayConfig{
							OutboundSNAT: udnv1.SNATDisabled,
							Routing:      udnv1.RoutingUnmanaged,
						},
					},
				},
			},
		),
		ginkgo.Entry("Layer2",
			&udnv1.ClusterUserDefinedNetwork{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "bgp-l2a-",
					Labels:       map[string]string{"bgp-l2a": ""},
				},
				Spec: udnv1.ClusterUserDefinedNetworkSpec{
					Network: udnv1.NetworkSpec{
						Topology: udnv1.NetworkTopologyLayer2,
						Layer2: &udnv1.Layer2Config{
							Role:    "Primary",
							Subnets: udnv1.DualStackCIDRs{"102.102.0.0/16", "2013:100:200::0/60"},
						},
					},
				},
			}, &udnv1.ClusterUserDefinedNetwork{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "bgp-l2b-",
					Labels:       map[string]string{"bgp-l2b": ""},
				},
				Spec: udnv1.ClusterUserDefinedNetworkSpec{
					Network: udnv1.NetworkSpec{
						Topology: udnv1.NetworkTopologyLayer2,
						Layer2: &udnv1.Layer2Config{
							Role:    "Primary",
							Subnets: udnv1.DualStackCIDRs{"103.103.0.0/16", "2014:100:200::0/60"},
						},
					},
				},
			},
		),
	)
})

var _ = ginkgo.Describe("BGP: For BGP configured networks", feature.RouteAdvertisements, func() {

	// infra constants
	const (
		bgpASN = 64512
	)

	// configuration helper to setup infra
	configureNetworkWithInfra := func(
		f *framework.Framework,
		ictx infraapi.Context,
		testName string,
		ipFamilySet sets.Set[utilnet.IPFamily],
		networkName string,
		networkType networkType,
		networkSpec *udnv1.NetworkSpec,
		bgpAlloc allocators.BGPAllocation,
	) (*corev1.Namespace, []string, map[string]infraapi.NetworkInterface) {
		ginkgo.GinkgoHelper()

		framework.Logf("Configuring the infra for network %s of type %s with allocations %#v", networkName, networkType, bgpAlloc)

		var servers []string
		vrfLiteNodeInterfaces := map[string]infraapi.NetworkInterface{}
		switch networkType {
		case cudnAdvertisedVRFLite:
			ginkgo.By("Running an external BGP network")
			agnhostName := networkName + "-vrflite-agnhost"
			agnhostNetworkName := agnhostName
			bgpPeerCIDRs := []string{bgpAlloc.BGPPeerSubnet, bgpAlloc.BGPPeerSubnet6}
			bgpServerCIDRs := []string{bgpAlloc.IPVRFSubnet, bgpAlloc.IPVRFSubnet6}
			gomega.Expect(
				runBGPNetworkAndServer(
					f,
					ictx,
					ipFamilySet,
					networkName,
					agnhostName,
					agnhostNetworkName,
					bgpPeerCIDRs,
					bgpServerCIDRs,
				),
			).To(gomega.Succeed())
			servers = append(servers, agnhostName)
		case cudnAdvertisedEVPNUnmanagedSharedVTEP, cudnAdvertisedEVPNUnmanagedRandomVTEP, cudnAdvertisedEVPNOverlappingCIDRSharedVTEP:
			ginkgo.By("Running an external EVPN network")

			bridgeName := "br" + networkName
			vxlanName := "vx" + networkName
			vtepName := networkName + "-vtep"
			// IPv6 VTEPs are not yet supported
			bgpAlloc.VTEPSubnet6 = ""
			if networkType != cudnAdvertisedEVPNUnmanagedRandomVTEP {
				// KIND network subnet: node InternalIPs fall within this range,
				// so the node-side controller can discover them via host-cidrs.
				kindNetwork, err := infraprovider.Get().PrimaryNetwork()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				kindV4Subnet, _, err := kindNetwork.IPv4IPv6Subnets()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				bgpAlloc.VTEPSubnet = kindV4Subnet
				vtepName = sharedNodeIPsVTEPName
				// All networks sharing the same VTEP also share the same bridge and
				// VXLAN interface on the external FRR; derive names from testName so
				// every network in the test lands on the same device.
				bridgeName = "br" + testName
				vxlanName = "vx" + testName
			}

			macVRFContainer := infraapi.ExternalContainer{
				Name:    networkName + "-macvrf-agnhost",
				Image:   images.AgnHost(),
				CmdArgs: []string{"netexec", fmt.Sprintf("--http-port=%d", agnhostHTTPPort)},
			}
			macVRFNetworkName := macVRFContainer.Name
			ipVRFContainer := infraapi.ExternalContainer{
				Name:    networkName + "-ipvrf-agnhost",
				Image:   images.AgnHost(),
				CmdArgs: []string{"netexec", fmt.Sprintf("--http-port=%d", agnhostHTTPPort)},
			}
			ipVRFNetworkName := ipVRFContainer.Name
			gomega.Expect(
				runEVPNNetworkAndServers(
					f,
					ictx,
					networkName,
					ipFamilySet,
					networkSpec,
					bgpAlloc,
					bgpASN,
					bridgeName,
					vxlanName,
					vtepName,
					&macVRFContainer,
					macVRFNetworkName,
					&ipVRFContainer,
					ipVRFNetworkName,
					networkType == cudnAdvertisedEVPNOverlappingCIDRSharedVTEP,
				),
			).To(gomega.Succeed())
			if networkSpec.EVPN.MACVRF != nil {
				servers = append(servers, macVRFContainer.Name)
			}
			if networkSpec.EVPN.IPVRF != nil {
				servers = append(servers, ipVRFContainer.Name)
			}
		}

		ginkgo.By("Configuring the namespace and CUDN")
		testNamespace, err := createNamespaceWithPrimaryNetworkOfType(f, ictx, testName, networkName, networkType, networkSpec)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		// attach network to the VRF on all nodes
		switch networkType {
		case cudnAdvertisedVRFLite:
			ginkgo.By("Attaching the BGP peer network to the CUDN VRF")
			const (
				vrfLiteInfraTimeout = 60 * time.Second
				vrfLiteInfraPolling = time.Second
			)
			waitForNodeLink := func(nodeName, linkName string) {
				ginkgo.GinkgoHelper()
				gomega.Eventually(func() error {
					_, err := infraprovider.Get().ExecK8NodeCommand(nodeName, []string{"ip", "link", "show", "dev", linkName})
					return err
				}).WithTimeout(vrfLiteInfraTimeout).WithPolling(vrfLiteInfraPolling).Should(
					gomega.Succeed(),
					"expected link %q to exist on node %q",
					linkName,
					nodeName,
				)
			}
			waitForNodePatchPort := func(nodeName string) {
				ginkgo.GinkgoHelper()
				scopedNodeName := ovnutil.GetUserDefinedNetworkPrefix(types.CUDNPrefix+networkName) + nodeName
				patchPortName := ovnutil.GetPatchPortName(deploymentconfig.Get().ExternalBridgeName(), scopedNodeName)
				gomega.Eventually(func() error {
					pods, err := f.ClientSet.CoreV1().Pods(deploymentconfig.Get().OVNKubernetesNamespace()).List(context.Background(), metav1.ListOptions{
						LabelSelector: "app=ovnkube-node",
						FieldSelector: "spec.nodeName=" + nodeName,
					})
					if err != nil {
						return err
					}
					if len(pods.Items) == 0 {
						return fmt.Errorf("failed to find ovnkube-node pod on node %q", nodeName)
					}
					command := fmt.Sprintf("ovs-vsctl --timeout=15 get Interface %s ofport", patchPortName)
					output, err := e2epodoutput.RunHostCmd(pods.Items[0].Namespace, pods.Items[0].Name, command)
					if err != nil {
						return fmt.Errorf("failed to get ofport for patch port %q on node %q: %w", patchPortName, nodeName, err)
					}
					ofPort := strings.TrimSpace(output)
					if ofPort == "" || ofPort == "[]" || ofPort == "-1" {
						return fmt.Errorf("patch port %q on node %q has no valid ofport: %q", patchPortName, nodeName, ofPort)
					}
					return nil
				}).WithTimeout(vrfLiteInfraTimeout).WithPolling(vrfLiteInfraPolling).Should(
					gomega.Succeed(),
					"expected patch port for network %q on node %q to have a valid ofport",
					networkName,
					nodeName,
				)
			}
			attachInterfaceToVRF := func(nodeName string, iface infraapi.NetworkInterface) {
				ginkgo.GinkgoHelper()
				gomega.Eventually(func() error {
					// This is idempotent when the interface is already attached to the VRF.
					if _, err := infraprovider.Get().ExecK8NodeCommand(nodeName, []string{"ip", "link", "set", "dev", iface.InfName, "master", networkName}); err != nil {
						return err
					}
					output, err := infraprovider.Get().ExecK8NodeCommand(nodeName, []string{"ip", "-o", "link", "show", "dev", iface.InfName})
					if err != nil {
						return err
					}
					fields := strings.Fields(output)
					for idx := 0; idx < len(fields)-1; idx++ {
						if fields[idx] == "master" && fields[idx+1] == networkName {
							return nil
						}
					}
					return fmt.Errorf("interface %q on node %q is not attached to VRF %q: %s", iface.InfName, nodeName, networkName, output)
				}).WithTimeout(vrfLiteInfraTimeout).WithPolling(vrfLiteInfraPolling).Should(
					gomega.Succeed(),
					"expected interface %q on node %q to be attached to VRF %q",
					iface.InfName,
					nodeName,
					networkName,
				)
			}
			nodeList, err := f.ClientSet.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			network, err := infraprovider.Get().GetNetwork(networkName)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			for _, node := range nodeList.Items {
				iface, err := infraprovider.Get().GetK8NodeNetworkInterface(node.Name, network)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				waitForNodeLink(node.Name, iface.InfName)
				waitForNodeLink(node.Name, networkName)
				waitForNodePatchPort(node.Name)
				if ipFamilySet.Has(utilnet.IPv6) {
					// prevent the IPv6 address of the interface from being removed when attaching to VRF
					_, err = infraprovider.Get().ExecK8NodeCommand(node.Name, []string{"sysctl", "-w", "net.ipv6.conf." + iface.InfName + ".keep_addr_on_down=1"})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
				}
				attachInterfaceToVRF(node.Name, iface)
				vrfLiteNodeInterfaces[node.Name] = iface
			}
		}

		return testNamespace, servers, vrfLiteNodeInterfaces
	}

	// testing helpers used throughout this testing node
	const (
		// FIXME: each test brings its own topology up, and sometimes zebra on
		// external FRR container fails to start on the first attempt for
		// unknown reasons delaying the overall availability, so we need to use
		// long timeouts
		timeout = 240 * time.Second
		polling = 1 * time.Second
		// Isolation tests, consistently check isolation with these timeouts
		timeoutNOK = 5 * time.Second        // 4-6 attempts
		pollingNOK = 100 * time.Millisecond // poll immediately

		// 1s, minimum
		curlMaxTime    = 1
		curlMaxTimeStr = "1"
	)
	var netexecPortStr = fmt.Sprintf("%d", netexecPort)
	testPodToHostnameAndExpect := func(src *corev1.Pod, dstIP, expect string) {
		ginkgo.GinkgoHelper()
		hostname, err := e2epodoutput.RunHostCmdWithRetries(
			src.Namespace,
			src.Name,
			fmt.Sprintf("curl --max-time %d -g -q -s http://%s/hostname", curlMaxTime, net.JoinHostPort(dstIP, netexecPortStr)),
			polling,
			timeout,
		)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(hostname).To(gomega.Equal(expect))
	}
	testPodToClientIP := func(src *corev1.Pod, dstIP string) {
		ginkgo.GinkgoHelper()
		_, err := e2epodoutput.RunHostCmdWithRetries(
			src.Namespace,
			src.Name,
			fmt.Sprintf("curl --max-time %d -g -q -s http://%s/clientip", curlMaxTime, net.JoinHostPort(dstIP, netexecPortStr)),
			polling,
			timeout,
		)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	}
	testPodToClientIPAndExpect := func(src *corev1.Pod, dstIP, expect string) {
		ginkgo.GinkgoHelper()
		ip, err := e2epodoutput.RunHostCmdWithRetries(
			src.Namespace,
			src.Name,
			fmt.Sprintf("curl --max-time %d -g -q -s http://%s/clientip", curlMaxTime, net.JoinHostPort(dstIP, netexecPortStr)),
			polling,
			timeout,
		)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		ip, _, err = net.SplitHostPort(ip)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(ip).To(gomega.Equal(expect))
	}
	testContainerToClientIPAndExpect := func(src, dstIP, expect string) {
		ginkgo.GinkgoHelper()
		gomega.Eventually(func(g gomega.Gomega) {
			ip, err := infraprovider.Get().ExecExternalContainerCommand(
				infraapi.ExternalContainer{Name: src},
				[]string{"curl", "--max-time", curlMaxTimeStr, "-g", "-q", "-s", fmt.Sprintf("http://%s/clientip", net.JoinHostPort(dstIP, netexecPortStr))},
			)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			ip, _, err = net.SplitHostPort(ip)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(ip).To(gomega.Equal(expect))
		}).WithTimeout(timeout).WithPolling(polling).Should(gomega.Succeed())
	}
	testPodToClientIPNOK := func(src *corev1.Pod, dstIP string) {
		ginkgo.GinkgoHelper()
		gomega.Consistently(func(g gomega.Gomega) {
			_, err := e2epodoutput.RunHostCmd(
				src.Namespace,
				src.Name,
				fmt.Sprintf("curl --max-time %d -g -q -s http://%s/clientip", curlMaxTime, net.JoinHostPort(dstIP, netexecPortStr)),
			)
			g.Expect(err).To(gomega.HaveOccurred())
		}).WithTimeout(timeoutNOK).WithPolling(pollingNOK).Should(gomega.Succeed())
	}
	testNodeToClientIPNOK := func(src, dstIP string) {
		ginkgo.GinkgoHelper()
		gomega.Consistently(func(g gomega.Gomega) {
			_, err := infraprovider.Get().ExecK8NodeCommand(
				src,
				[]string{"curl", "--max-time", curlMaxTimeStr, "-g", "-q", "-s", fmt.Sprintf("http://%s/clientip", net.JoinHostPort(dstIP, netexecPortStr))},
			)
			g.Expect(err).To(gomega.HaveOccurred())
		}).WithTimeout(timeoutNOK).WithPolling(pollingNOK).Should(gomega.Succeed())
	}
	testContainerToClientIPNOK := func(src, dstIP string) {
		ginkgo.GinkgoHelper()
		gomega.Consistently(func(g gomega.Gomega) {
			_, err := infraprovider.Get().ExecExternalContainerCommand(
				infraapi.ExternalContainer{Name: src},
				[]string{"curl", "--max-time", curlMaxTimeStr, "-g", "-q", "-s", fmt.Sprintf("http://%s/clientip", net.JoinHostPort(dstIP, netexecPortStr))},
			)
			g.Expect(err).To(gomega.HaveOccurred())
		}).WithTimeout(timeoutNOK).WithPolling(pollingNOK).Should(gomega.Succeed())
	}

	testForIPFamilies := func(ipFamilySet sets.Set[utilnet.IPFamily], test func(utilnet.IPFamily)) {
		ginkgo.GinkgoHelper()
		for _, family := range ipFamilySet.UnsortedList() {
			test(family)
		}
	}

	const (
		baseName = "bgp"
	)

	f := wrappedTestFramework(baseName)
	f.SkipNamespaceCreation = true
	var ipFamilySet sets.Set[utilnet.IPFamily]
	var ictx infraapi.Context
	var testBaseName, testSuffix, testNetworkName string
	var externalServers []string

	ginkgo.BeforeEach(func() {
		if !isLocalGWModeEnabled() {
			e2eskipper.Skipf("Test case only supported in Local Gateway mode")
		}
		framework.Logf("Running in ginkgo process %d", ginkgo.GinkgoParallelProcess())
		ipFamilySet = sets.New(getSupportedIPFamiliesSlice(f.ClientSet)...)
		ictx = infraprovider.Get().NewTestContext()
		testSuffix = framework.RandomSuffix()
		testBaseName = baseName + testSuffix
		testNetworkName = testBaseName
	})

	// define networks to test with
	layer3NetworkSpecGen := func(udnIPv4, udnIPv6 string, _ allocators.BGPAllocation) *udnv1.NetworkSpec {
		return &udnv1.NetworkSpec{
			Topology: udnv1.NetworkTopologyLayer3,
			Layer3: &udnv1.Layer3Config{
				Role:    "Primary",
				Subnets: []udnv1.Layer3Subnet{{CIDR: udnv1.CIDR(udnIPv4)}, {CIDR: udnv1.CIDR(udnIPv6)}},
			},
		}
	}
	layer2NetworkSpecGen := func(udnIPv4, udnIPv6 string, _ allocators.BGPAllocation) *udnv1.NetworkSpec {
		return &udnv1.NetworkSpec{
			Topology: udnv1.NetworkTopologyLayer2,
			Layer2: &udnv1.Layer2Config{
				Role:    "Primary",
				Subnets: udnv1.DualStackCIDRs{udnv1.CIDR(udnIPv4), udnv1.CIDR(udnIPv6)},
			},
		}
	}
	layer2MACVRFNetworkSpecGen := func(udnIPv4, udnIPv6 string, bgpAlloc allocators.BGPAllocation) *udnv1.NetworkSpec {
		return &udnv1.NetworkSpec{
			Topology: udnv1.NetworkTopologyLayer2,
			Layer2: &udnv1.Layer2Config{
				Role:    udnv1.NetworkRolePrimary,
				Subnets: udnv1.DualStackCIDRs{udnv1.CIDR(udnIPv4), udnv1.CIDR(udnIPv6)},
			},
			Transport: udnv1.TransportOptionEVPN,
			EVPN: &udnv1.EVPNConfig{
				MACVRF: &udnv1.VRFConfig{
					VNI: int32(bgpAlloc.MACVRFVNI),
				},
			},
		}
	}
	layer2MACVRFIPVRFNetworkSpecGen := func(udnIPv4, udnIPv6 string, bgpAlloc allocators.BGPAllocation) *udnv1.NetworkSpec {
		return &udnv1.NetworkSpec{
			Topology: udnv1.NetworkTopologyLayer2,
			Layer2: &udnv1.Layer2Config{
				Role:    udnv1.NetworkRolePrimary,
				Subnets: udnv1.DualStackCIDRs{udnv1.CIDR(udnIPv4), udnv1.CIDR(udnIPv6)},
			},
			Transport: udnv1.TransportOptionEVPN,
			EVPN: &udnv1.EVPNConfig{
				MACVRF: &udnv1.VRFConfig{
					VNI: int32(bgpAlloc.MACVRFVNI),
				},
				IPVRF: &udnv1.VRFConfig{
					VNI: int32(bgpAlloc.IPVRFVNI),
				},
			},
		}
	}
	layer3IPVRFNetworkSpecGen := func(udnIPv4, udnIPv6 string, bgpAlloc allocators.BGPAllocation) *udnv1.NetworkSpec {
		return &udnv1.NetworkSpec{
			Topology: udnv1.NetworkTopologyLayer3,
			Layer3: &udnv1.Layer3Config{
				Role:    udnv1.NetworkRolePrimary,
				Subnets: []udnv1.Layer3Subnet{{CIDR: udnv1.CIDR(udnIPv4)}, {CIDR: udnv1.CIDR(udnIPv6)}},
			},
			Transport: udnv1.TransportOptionEVPN,
			EVPN: &udnv1.EVPNConfig{
				IPVRF: &udnv1.VRFConfig{
					VNI: int32(bgpAlloc.IPVRFVNI),
				},
			},
		}
	}
	layer3NoOverlaySNATEnabledUnmanagedSpecGen := func(udnIPv4, udnIPv6 string, _ allocators.BGPAllocation) *udnv1.NetworkSpec {
		return &udnv1.NetworkSpec{
			Topology: udnv1.NetworkTopologyLayer3,
			Layer3: &udnv1.Layer3Config{
				Role:    udnv1.NetworkRolePrimary,
				Subnets: []udnv1.Layer3Subnet{{CIDR: udnv1.CIDR(udnIPv4)}, {CIDR: udnv1.CIDR(udnIPv6)}},
			},
			Transport: udnv1.TransportOptionNoOverlay,
			NoOverlay: &udnv1.NoOverlayConfig{
				OutboundSNAT: udnv1.SNATEnabled,
				Routing:      udnv1.RoutingUnmanaged,
			},
		}
	}
	layer3NoOverlaySNATDisabledUnmanagedSpecGen := func(udnIPv4, udnIPv6 string, _ allocators.BGPAllocation) *udnv1.NetworkSpec {
		return &udnv1.NetworkSpec{
			Topology: udnv1.NetworkTopologyLayer3,
			Layer3: &udnv1.Layer3Config{
				Role:    udnv1.NetworkRolePrimary,
				Subnets: []udnv1.Layer3Subnet{{CIDR: udnv1.CIDR(udnIPv4)}, {CIDR: udnv1.CIDR(udnIPv6)}},
			},
			Transport: udnv1.TransportOptionNoOverlay,
			NoOverlay: &udnv1.NoOverlayConfig{
				OutboundSNAT: udnv1.SNATDisabled,
				Routing:      udnv1.RoutingUnmanaged,
			},
		}
	}

	networksToTest := []ginkgo.TableEntry{
		ginkgo.Entry("Layer 3 CUDN VRF-Lite", cudnAdvertisedVRFLite, layer3NetworkSpecGen),
		ginkgo.Entry("Layer 2 CUDN VRF-Lite", cudnAdvertisedVRFLite, layer2NetworkSpecGen),
		ginkgo.Entry("Layer 3 CUDN VRF-Lite no-overlay SNAT enabled unmanaged routing", feature.NoOverlay, cudnAdvertisedVRFLite, layer3NoOverlaySNATEnabledUnmanagedSpecGen),
		ginkgo.Entry("Layer 3 CUDN VRF-Lite no-overlay SNAT disabled unmanaged routing", feature.NoOverlay, cudnAdvertisedVRFLite, layer3NoOverlaySNATDisabledUnmanagedSpecGen),
		ginkgo.Entry("Layer 3 CUDN EVPN IP-VRF shared VTEP", feature.EVPN, cudnAdvertisedEVPNUnmanagedSharedVTEP, layer3IPVRFNetworkSpecGen),
		ginkgo.Entry("Layer 2 CUDN EVPN MAC-VRF shared VTEP", feature.EVPN, cudnAdvertisedEVPNUnmanagedSharedVTEP, layer2MACVRFNetworkSpecGen),
		ginkgo.Entry("Layer 2 CUDN EVPN MAC-VRF and IP-VRF shared VTEP", feature.EVPN, cudnAdvertisedEVPNUnmanagedSharedVTEP, layer2MACVRFIPVRFNetworkSpecGen),
		ginkgo.Entry("Layer 3 CUDN EVPN IP-VRF random VTEP", feature.EVPN, cudnAdvertisedEVPNUnmanagedRandomVTEP, layer3IPVRFNetworkSpecGen),
		ginkgo.Entry("Layer 2 CUDN EVPN MAC-VRF random VTEP", feature.EVPN, cudnAdvertisedEVPNUnmanagedRandomVTEP, layer2MACVRFNetworkSpecGen),
		ginkgo.Entry("Layer 2 CUDN EVPN MAC-VRF and IP-VRF random VTEP", feature.EVPN, cudnAdvertisedEVPNUnmanagedRandomVTEP, layer2MACVRFIPVRFNetworkSpecGen),
	}

	ginkgo.DescribeTableSubtree("When the tested network is of type",
		func(testedNetworkType networkType, networkSpecGen func(string, string, allocators.BGPAllocation) *udnv1.NetworkSpec) {
			var testNamespace *corev1.Namespace
			var testPod *corev1.Pod
			var vrfLiteNodeInterfaces map[string]infraapi.NetworkInterface
			var networkSpec *udnv1.NetworkSpec
			var testNetworkSpec *udnv1.NetworkSpec

			expectedSNATSourceIP := func(family utilnet.IPFamily, node *corev1.Node) string {
				ginkgo.GinkgoHelper()
				// In LGW VRF-Lite, host nftables masquerade picks the source IP
				// from the node's egress interface in the target VRF when no-overlay
				// outbound SNAT is enabled.
				if networkSpec.NoOverlay != nil && networkSpec.NoOverlay.OutboundSNAT == udnv1.SNATEnabled &&
					testedNetworkType == cudnAdvertisedVRFLite && IsGatewayModeLocal(f.ClientSet) {
					iface, ok := vrfLiteNodeInterfaces[node.Name]
					gomega.Expect(ok).To(gomega.BeTrue(), "Expected VRF-Lite egress interface for node %s", node.Name)
					expectedIP := getFirstIPStringOfFamily(family, []string{iface.IPv4, iface.IPv6})
					gomega.Expect(expectedIP).NotTo(gomega.BeEmpty(), "Expected VRF-Lite egress interface for node %s to have an IP for %v", node.Name, family)
					return expectedIP
				}
				expectedNodeIPs := e2enode.GetAddressesByTypeAndFamily(node, corev1.NodeInternalIP, corev1.IPv4Protocol)
				if family == utilnet.IPv6 {
					expectedNodeIPs = e2enode.GetAddressesByTypeAndFamily(node, corev1.NodeInternalIP, corev1.IPv6Protocol)
				}
				gomega.Expect(expectedNodeIPs).NotTo(gomega.BeEmpty(), "Expected node %s to have an InternalIP for %v", node.Name, family)
				return expectedNodeIPs[0]
			}

			getExternalServerIPForFamily := func(server string, family utilnet.IPFamily) string {
				ginkgo.GinkgoHelper()
				bgpServerNetwork, err := infraprovider.Get().GetNetwork(server)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				iface, err := infraprovider.Get().GetExternalContainerNetworkInterface(
					infraapi.ExternalContainer{Name: server},
					bgpServerNetwork,
				)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				return getFirstIPStringOfFamily(family, []string{iface.IPv4, iface.IPv6})
			}

			getSameNode := func() string {
				return testPod.Spec.NodeName
			}
			getDifferentNode := func() string {
				ginkgo.GinkgoHelper()
				nodes, err := e2enode.GetReadySchedulableNodes(context.Background(), f.ClientSet)
				gomega.Expect(err).NotTo(gomega.HaveOccurred(), "Failed to get ready schedulable nodes")
				for _, node := range nodes.Items {
					if node.Name != testPod.Spec.NodeName {
						return node.Name
					}
				}
				ginkgo.Fail(fmt.Sprintf("Failed to find a different ready schedulable node than %s", testPod.Spec.NodeName))
				return ""
			}

			ginkgo.BeforeEach(func() {
				bgpAlloc, err := allocators.AllocateBGP(f, ictx)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				udnIPv4, udnIPv6 := bgpAlloc.UDNSubnet, bgpAlloc.UDNSubnet6

				networkSpec = networkSpecGen(udnIPv4, udnIPv6, bgpAlloc)
				switch {
				case networkSpec.Layer3 != nil:
					networkSpec.Layer3.Subnets = matchL3SubnetsByIPFamilies(ipFamilySet, networkSpec.Layer3.Subnets...)
				case networkSpec.Layer2 != nil:
					networkSpec.Layer2.Subnets = matchL2SubnetsByIPFamilies(ipFamilySet, networkSpec.Layer2.Subnets...)
				}
				testNetworkSpec = networkSpec

				testNamespace, externalServers, vrfLiteNodeInterfaces = configureNetworkWithInfra(
					f,
					ictx,
					testBaseName,
					ipFamilySet,
					testNetworkName,
					testedNetworkType,
					networkSpec,
					bgpAlloc,
				)
			})

			ginkgo.Describe("When a pod runs on the tested network", func() {
				ginkgo.BeforeEach(func() {
					ginkgo.By("Running a pod on the tested network namespace")
					testPod = e2epod.CreateExecPodOrFail(
						context.Background(),
						f.ClientSet,
						testNamespace.Name,
						testNamespace.Name+"-netexec-pod",
						func(p *corev1.Pod) {
							p.Spec.Containers[0].Args = []string{"netexec"}
						},
					)
					var err error
					testPod, err = f.ClientSet.CoreV1().Pods(testPod.Namespace).Get(context.Background(), testPod.Name, metav1.GetOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					gomega.Expect(testPod.Spec.NodeName).NotTo(gomega.BeEmpty())
				})

				ginkgo.DescribeTable("It can reach external servers on the same network",
					func(family utilnet.IPFamily) {
						if !ipFamilySet.Has(family) {
							e2eskipper.Skipf("IP family %v not supported", family)
						}
						expectSNAT := networkSpec.NoOverlay != nil &&
							networkSpec.NoOverlay.OutboundSNAT == udnv1.SNATEnabled

						if expectSNAT {
							ginkgo.By("Ensuring a request from the pod can reach the external servers (SNATed since OutboundSNAT is enabled)")
						} else {
							ginkgo.By("Ensuring a request from the pod can reach the external servers without being SNATed")
						}
						for _, externalServer := range externalServers {
							serverIP := getExternalServerIPForFamily(externalServer, family)
							gomega.Expect(serverIP).NotTo(gomega.BeEmpty())
							framework.Logf("Checking request from pod reaches server %q", externalServer)
							testPodToHostnameAndExpect(testPod, serverIP, externalServer)

							if expectSNAT {
								framework.Logf("Sending request from pod to server %q is SNATed (OutboundSNAT enabled)", externalServer)
								// When OutboundSNAT is enabled, pod-to-external traffic is masqueraded
								// to the node IP.
								ip, err := e2epodoutput.RunHostCmdWithRetries(
									testPod.Namespace,
									testPod.Name,
									fmt.Sprintf("curl --max-time %d -g -q -s http://%s/clientip", curlMaxTime, net.JoinHostPort(serverIP, netexecPortStr)),
									polling,
									timeout,
								)
								gomega.Expect(err).NotTo(gomega.HaveOccurred())
								sourceIP, _, err := net.SplitHostPort(ip)
								gomega.Expect(err).NotTo(gomega.HaveOccurred())
								testPodNode, err := f.ClientSet.CoreV1().Nodes().Get(context.TODO(), testPod.Spec.NodeName, metav1.GetOptions{})
								gomega.Expect(err).NotTo(gomega.HaveOccurred())
								expectedSourceIP := expectedSNATSourceIP(family, testPodNode)
								gomega.Expect(sourceIP).To(gomega.Equal(expectedSourceIP),
									fmt.Sprintf("Expected SNAT for pod %s to use node egress IP %s, output: %v", testPod.Name, expectedSourceIP, ip))
							} else {
								testPodIP, err := getPodAnnotationIPsForPrimaryNetworkByIPFamily(
									f.ClientSet,
									testPod.Namespace,
									testPod.Name,
									testNetworkName,
									family,
								)
								gomega.Expect(err).NotTo(gomega.HaveOccurred())
								gomega.Expect(testPodIP).ToNot(gomega.BeEmpty())
								framework.Logf("Sending request from pod to server %q is not SNATed", externalServer)
								testPodToClientIPAndExpect(testPod, serverIP, testPodIP)
							}
						}
					},
					ginkgo.Entry("When the network is IPv4", utilnet.IPv4),
					ginkgo.Entry("When the network is IPv6", utilnet.IPv6),
				)

				ginkgo.DescribeTable("It can be reached by an external server on the same network",
					func(family utilnet.IPFamily) {
						if !ipFamilySet.Has(family) {
							e2eskipper.Skipf("IP family %v not supported", family)
						}
						ginkgo.By("Ensuring a request from the external servers can reach the pod")
						for _, externalServer := range externalServers {
							serverIP := getExternalServerIPForFamily(externalServer, family)
							gomega.Expect(serverIP).NotTo(gomega.BeEmpty())
							podIP, err := getPodAnnotationIPsForPrimaryNetworkByIPFamily(
								f.ClientSet,
								testPod.Namespace,
								testPod.Name,
								testNetworkName,
								family,
							)
							gomega.Expect(err).NotTo(gomega.HaveOccurred())
							gomega.Expect(podIP).ToNot(gomega.BeEmpty())
							framework.Logf("Checking request from server %q reaches pod", externalServer)
							testContainerToClientIPAndExpect(externalServer, podIP, serverIP)
						}
					},
					ginkgo.Entry("When the network is IPv4", utilnet.IPv4),
					ginkgo.Entry("When the network is IPv6", utilnet.IPv6),
				)

				ginkgo.It("Can reach KAPI service", func() {
					ginkgo.By("Ensuring a request from the pod can reach KAPI service")
					output, err := e2epodoutput.RunHostCmdWithRetries(
						testPod.Namespace,
						testPod.Name,
						fmt.Sprintf("curl --max-time %d -g -q -s -k https://kubernetes.default/healthz", curlMaxTime),
						polling,
						timeout,
					)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					gomega.Expect(output).To(gomega.Equal("ok"))
				})

				ginkgo.DescribeTable("It cannot reach an external server on a different network",
					func(family utilnet.IPFamily) {
						if !ipFamilySet.Has(family) {
							e2eskipper.Skipf("IP family %v not supported", family)
						}
						ginkgo.By("Ensuring a request from the pod cannot reach the external server")
						// using the external server setup for the default network
						bgpServerNetwork, err := infraprovider.Get().GetNetwork(bgpExternalNetworkName)
						gomega.Expect(err).NotTo(gomega.HaveOccurred())
						iface, err := infraprovider.Get().GetExternalContainerNetworkInterface(
							infraapi.ExternalContainer{Name: bgpExternalServerContainerName},
							bgpServerNetwork,
						)
						gomega.Expect(err).NotTo(gomega.HaveOccurred())
						serverIP := getFirstIPStringOfFamily(family, []string{iface.IPv4, iface.IPv6})
						gomega.Expect(serverIP).NotTo(gomega.BeEmpty())
						testPodToClientIPNOK(testPod, serverIP)
					},
					ginkgo.Entry("When the network is IPv4", utilnet.IPv4),
					ginkgo.Entry("When the network is IPv6", utilnet.IPv6),
				)

				ginkgo.DescribeTable("It cannot be reached by an external server on a different network",
					func(family utilnet.IPFamily) {
						if !ipFamilySet.Has(family) {
							e2eskipper.Skipf("IP family %v not supported", family)
						}
						ginkgo.By("Ensuring a request from the external server cannot reach the pod")
						podIP, err := getPodAnnotationIPsForPrimaryNetworkByIPFamily(
							f.ClientSet,
							testPod.Namespace,
							testPod.Name,
							testNetworkName,
							family,
						)
						gomega.Expect(err).NotTo(gomega.HaveOccurred())
						gomega.Expect(podIP).ToNot(gomega.BeEmpty())
						// using the external server setup for the default network
						testContainerToClientIPNOK(bgpExternalServerContainerName, podIP)
					},
					ginkgo.Entry("When the network is IPv4", utilnet.IPv4),
					ginkgo.Entry("When the network is IPv6", utilnet.IPv6),
				)

				ginkgo.DescribeTableSubtree("It cannot be reached by a cluster node",
					func(getNode func() string) {
						ginkgo.DescribeTable("",
							func(family utilnet.IPFamily) {
								if !ipFamilySet.Has(family) {
									e2eskipper.Skipf("IP family %v not supported", family)
								}
								ginkgo.By("Ensuring a request from the node cannot reach the tested network pod")
								podIP, err := getPodAnnotationIPsForPrimaryNetworkByIPFamily(
									f.ClientSet,
									testPod.Namespace,
									testPod.Name,
									testNetworkName,
									family,
								)
								gomega.Expect(err).NotTo(gomega.HaveOccurred())
								gomega.Expect(podIP).ToNot(gomega.BeEmpty())
								testNodeToClientIPNOK(getNode(), podIP)
							},
							ginkgo.Entry("When the network is IPv4", utilnet.IPv4),
							ginkgo.Entry("When the network is IPv6", utilnet.IPv6),
						)
					},
					ginkgo.Entry("When it is the same node", getSameNode),
					ginkgo.Entry("When it is a different node", getDifferentNode),
				)

				ginkgo.DescribeTableSubtree("When other pod runs on the tested network",
					func(getNode func() string) {
						var otherPod *corev1.Pod

						ginkgo.BeforeEach(func() {
							ginkgo.By("Running other pod on the tested network namespace")
							otherPod = e2epod.CreateExecPodOrFail(
								context.Background(),
								f.ClientSet,
								testNamespace.Name,
								testNamespace.Name+"-netexec-pod",
								func(p *corev1.Pod) {
									p.Spec.Containers[0].Args = []string{"netexec"}
									p.Spec.NodeName = getNode()
									p.Labels = map[string]string{"app": "netexec-pod"}
								},
							)
						})

						ginkgo.DescribeTable("The pods on the tested network can reach each other",
							func(family utilnet.IPFamily) {
								if !ipFamilySet.Has(family) {
									e2eskipper.Skipf("IP family %v not supported", family)
								}
								ginkgo.By("Ensuring a request from the first pod can reach the second pod")
								otherPodIP, err := getPodAnnotationIPsForPrimaryNetworkByIPFamily(
									f.ClientSet,
									otherPod.Namespace,
									otherPod.Name,
									testNetworkName,
									family,
								)
								gomega.Expect(err).NotTo(gomega.HaveOccurred())
								gomega.Expect(otherPodIP).ToNot(gomega.BeEmpty())
								testPodToClientIP(testPod, otherPodIP)
							},
							ginkgo.Entry("When the networks are IPv4", utilnet.IPv4),
							ginkgo.Entry("When the networks are IPv6", utilnet.IPv6),
						)

						ginkgo.Describe("Backing a ClusterIP service", func() {
							var service *corev1.Service

							ginkgo.BeforeEach(func() {
								ginkgo.By("Creating a service backed by the other network pod")
								service = e2eservice.CreateServiceSpec(
									"service-for-netexec",
									"",
									false,
									otherPod.Labels,
								)
								service.Spec.Ports = []corev1.ServicePort{{Port: netexecPort}}
								familyPolicy := corev1.IPFamilyPolicyPreferDualStack
								service.Spec.IPFamilyPolicy = &familyPolicy
								var err error
								service, err = f.ClientSet.CoreV1().Services(otherPod.Namespace).Create(context.Background(), service, metav1.CreateOptions{})
								gomega.Expect(err).NotTo(gomega.HaveOccurred())
							})

							ginkgo.DescribeTable("The first pod can reach the ClusterIP service on the same network",
								func(family utilnet.IPFamily) {
									if !ipFamilySet.Has(family) {
										e2eskipper.Skipf("IP family %v not supported", family)
									}
									ginkgo.By("Ensuring a request from the first pod can reach the ClusterIP service")
									clusterIP := getFirstIPStringOfFamily(family, service.Spec.ClusterIPs)
									gomega.Expect(clusterIP).ToNot(gomega.BeEmpty())
									testPodToClientIP(testPod, clusterIP)
								},
								ginkgo.Entry("When the networks are IPv4", utilnet.IPv4),
								ginkgo.Entry("When the networks are IPv6", utilnet.IPv6),
							)
						})
					},
					ginkgo.Entry("On the same node", getSameNode),
					ginkgo.Entry("On a different node", getDifferentNode),
				)

				ginkgo.Describe("When there is other network", func() {

					nilNetworkSpecGen := func(string, string, allocators.BGPAllocation) *udnv1.NetworkSpec {
						return nil
					}

					// overlappingSpecGen reuses the tested network's topology and subnets
					// with fresh VNIs to prove VPN-level isolation with overlapping IP
					// address space. Only appended to otherNetworksToTest when the outer
					// tested network is cudnAdvertisedEVPNUnmanagedSharedVTEP, so
					// testNetworkSpec is guaranteed to have an EVPN config here.
					overlappingSpecGen := func(_ string, _ string, bgpAlloc allocators.BGPAllocation) *udnv1.NetworkSpec {
						spec := testNetworkSpec.DeepCopy()
						if spec.EVPN.MACVRF != nil {
							spec.EVPN.MACVRF.VNI = int32(bgpAlloc.MACVRFVNI)
						}
						if spec.EVPN.IPVRF != nil {
							spec.EVPN.IPVRF.VNI = int32(bgpAlloc.IPVRFVNI)
						}
						spec.EVPN.VTEP = ""
						return spec
					}

					otherNetworksToTest := []ginkgo.TableEntry{
						ginkgo.Entry("Default", defaultNetwork, nilNetworkSpecGen),
						ginkgo.Entry("Layer 3 CUDN VRF-Lite", cudnAdvertisedVRFLite, layer3NetworkSpecGen),
						ginkgo.Entry("Layer 2 CUDN VRF-Lite", cudnAdvertisedVRFLite, layer2NetworkSpecGen),
						ginkgo.Entry("Layer 3 UDN", udn, layer3NetworkSpecGen),
						ginkgo.Entry("Layer 3 CUDN advertised", cudnAdvertised, layer3NetworkSpecGen),
						ginkgo.Entry("Layer 2 UDN", udn, layer2NetworkSpecGen),
						ginkgo.Entry("Layer 2 CUDN advertised", cudnAdvertised, layer2NetworkSpecGen),
						ginkgo.Entry("Layer 3 CUDN EVPN IP-VRF shared VTEP", feature.EVPN, cudnAdvertisedEVPNUnmanagedSharedVTEP, layer3IPVRFNetworkSpecGen),
						ginkgo.Entry("Layer 2 CUDN EVPN MAC-VRF shared VTEP", feature.EVPN, cudnAdvertisedEVPNUnmanagedSharedVTEP, layer2MACVRFNetworkSpecGen),
						ginkgo.Entry("Layer 2 CUDN EVPN MAC-VRF and IP-VRF shared VTEP", feature.EVPN, cudnAdvertisedEVPNUnmanagedSharedVTEP, layer2MACVRFIPVRFNetworkSpecGen),
						ginkgo.Entry("Layer 3 CUDN EVPN IP-VRF random VTEP", feature.EVPN, cudnAdvertisedEVPNUnmanagedRandomVTEP, layer3IPVRFNetworkSpecGen),
						ginkgo.Entry("Layer 2 CUDN EVPN MAC-VRF random VTEP", feature.EVPN, cudnAdvertisedEVPNUnmanagedRandomVTEP, layer2MACVRFNetworkSpecGen),
						ginkgo.Entry("Layer 2 CUDN EVPN MAC-VRF and IP-VRF random VTEP", feature.EVPN, cudnAdvertisedEVPNUnmanagedRandomVTEP, layer2MACVRFIPVRFNetworkSpecGen),
					}
					if testedNetworkType == cudnAdvertisedEVPNUnmanagedSharedVTEP {
						otherNetworksToTest = append(otherNetworksToTest,
							ginkgo.Entry("CUDN EVPN overlapping subnet shared VTEP", feature.EVPN, cudnAdvertisedEVPNOverlappingCIDRSharedVTEP, overlappingSpecGen),
						)
					}

					ginkgo.DescribeTableSubtree("Of type",
						func(networkType networkType, otherNetworkSpecGen func(string, string, allocators.BGPAllocation) *udnv1.NetworkSpec) {
							var otherNamespace *corev1.Namespace
							var otherNetworkName string

							ginkgo.BeforeEach(func() {
								otherNetworkName = testBaseName + "o"
								otherNamespaceName := otherNetworkName

								otherBGPAlloc, err := allocators.AllocateBGP(f, ictx)
								gomega.Expect(err).NotTo(gomega.HaveOccurred())
								otherUDNIPv4, otherUDNIPv6 := otherBGPAlloc.UDNSubnet, otherBGPAlloc.UDNSubnet6

								otherNetworkSpec := otherNetworkSpecGen(otherUDNIPv4, otherUDNIPv6, otherBGPAlloc)
								switch {
								case otherNetworkSpec == nil && networkType == defaultNetwork:
									otherNetworkName = "default"
								case otherNetworkSpec.Layer3 != nil:
									otherNetworkSpec.Layer3.Subnets = matchL3SubnetsByIPFamilies(ipFamilySet, otherNetworkSpec.Layer3.Subnets...)
								case otherNetworkSpec.Layer2 != nil:
									otherNetworkSpec.Layer2.Subnets = matchL2SubnetsByIPFamilies(ipFamilySet, otherNetworkSpec.Layer2.Subnets...)
								}

								otherNamespace, _, _ = configureNetworkWithInfra(
									f,
									ictx,
									testBaseName,
									ipFamilySet,
									otherNamespaceName,
									networkType,
									otherNetworkSpec,
									otherBGPAlloc,
								)
							})

							ginkgo.It("Both networks are isolated", func() {
								ginkgo.By("Running two pods on the other network namespace on different nodes")
								var otherPodSameNode, otherPodDiffNode *corev1.Pod
								wg := sync.WaitGroup{}
								wg.Add(2)
								go func() {
									ginkgo.GinkgoHelper()
									defer ginkgo.GinkgoRecover()
									defer wg.Done()
									otherPodSameNode = e2epod.CreateExecPodOrFail(
										context.Background(),
										f.ClientSet,
										otherNamespace.Name,
										otherNamespace.Name+"-netexec-samenode-pod",
										func(p *corev1.Pod) {
											p.Spec.Containers[0].Args = []string{"netexec"}
											p.Spec.NodeName = getSameNode()
											p.Labels = map[string]string{"app": "netexec-samenode-pod"}
										},
									)
								}()
								go func() {
									ginkgo.GinkgoHelper()
									defer ginkgo.GinkgoRecover()
									defer wg.Done()
									otherPodDiffNode = e2epod.CreateExecPodOrFail(
										context.Background(),
										f.ClientSet,
										otherNamespace.Name,
										otherNamespace.Name+"-netexec-diffnode-pod",
										func(p *corev1.Pod) {
											p.Spec.Containers[0].Args = []string{"netexec"}
											p.Spec.NodeName = getDifferentNode()
											p.Labels = map[string]string{"app": "netexec-diffnode-pod"}
										},
									)
								}()
								wg.Wait()

								ginkgo.By("Ensuring the pods from the other network can talk to each other")
								testForIPFamilies(
									ipFamilySet,
									func(family utilnet.IPFamily) {
										ginkgo.GinkgoHelper()
										otherPodSameNodeIP, err := getPodAnnotationIPsForPrimaryNetworkByIPFamily(
											f.ClientSet,
											otherPodSameNode.Namespace,
											otherPodSameNode.Name,
											otherNetworkName,
											family,
										)
										gomega.Expect(err).NotTo(gomega.HaveOccurred())
										gomega.Expect(otherPodSameNodeIP).ToNot(gomega.BeEmpty())
										testPodToClientIP(otherPodDiffNode, otherPodSameNodeIP)
									},
								)

								ginkgo.By("Ensuring a request from the tested network pod cannot reach the other network pods")
								for _, target := range []*corev1.Pod{otherPodSameNode, otherPodDiffNode} {
									testForIPFamilies(
										ipFamilySet,
										func(family utilnet.IPFamily) {
											ginkgo.GinkgoHelper()
											framework.Logf("Ensuring a request from the tested network pod cannot reach the other network pod %s on IPv%v", target.Name, family)
											otherPodIP, err := getPodAnnotationIPsForPrimaryNetworkByIPFamily(
												f.ClientSet,
												target.Namespace,
												target.Name,
												otherNetworkName,
												family,
											)
											gomega.Expect(err).NotTo(gomega.HaveOccurred())
											gomega.Expect(otherPodIP).ToNot(gomega.BeEmpty())
											testPodIP, err := getPodAnnotationIPsForPrimaryNetworkByIPFamily(
												f.ClientSet,
												testPod.Namespace,
												testPod.Name,
												testNetworkName,
												family,
											)
											gomega.Expect(err).NotTo(gomega.HaveOccurred())
											gomega.Expect(testPodIP).ToNot(gomega.BeEmpty())
											if testPodIP == otherPodIP {
												// Overlapping CUDNs assigned the same IP; verify the tested pod
												// reaches itself (its own hostname), proving traffic stays within
												// its own CUDN.
												framework.Logf("Tested pod and other pod share IP %s; verifying hostname to confirm per-CUDN isolation", otherPodIP)
												testPodToHostnameAndExpect(testPod, otherPodIP, testPod.Name)
											} else {
												testPodToClientIPNOK(testPod, otherPodIP)
											}
										},
									)
								}

								ginkgo.By("Ensuring a request from the other network pods on the same node cannot reach the tested network pod")
								for _, source := range []*corev1.Pod{otherPodSameNode, otherPodDiffNode} {
									testForIPFamilies(
										ipFamilySet,
										func(family utilnet.IPFamily) {
											ginkgo.GinkgoHelper()
											framework.Logf("Ensuring a request from the other network pod %s on the same node cannot reach the tested network pod on IPv%v", source.Name, family)
											testPodIP, err := getPodAnnotationIPsForPrimaryNetworkByIPFamily(
												f.ClientSet,
												testPod.Namespace,
												testPod.Name,
												testNetworkName,
												family,
											)
											gomega.Expect(err).NotTo(gomega.HaveOccurred())
											gomega.Expect(testPodIP).ToNot(gomega.BeEmpty())
											sourcePodIP, err := getPodAnnotationIPsForPrimaryNetworkByIPFamily(
												f.ClientSet,
												source.Namespace,
												source.Name,
												otherNetworkName,
												family,
											)
											gomega.Expect(err).NotTo(gomega.HaveOccurred())
											gomega.Expect(sourcePodIP).ToNot(gomega.BeEmpty())
											if sourcePodIP == testPodIP {
												// Overlapping CUDNs assigned the same IP; verify the other pod
												// reaches itself (its own hostname) to confirm per-CUDN isolation.
												framework.Logf("Other pod %s and tested pod share IP %s; verifying hostname to confirm per-CUDN isolation", source.Name, testPodIP)
												testPodToHostnameAndExpect(source, testPodIP, source.Name)
											} else {
												// Source pod has a different IP from the tested pod. In the
												// overlapping CIDR case, the source's same-network peer may
												// share the tested pod's IP. If so, when the source curls
												// testPodIP, routing within the other network delivers the
												// packet to that peer — not to the tested pod. Determine
												// which pod is the peer and check its IP.
												var peerPod *corev1.Pod
												if source == otherPodSameNode {
													peerPod = otherPodDiffNode
												} else {
													peerPod = otherPodSameNode
												}
												peerPodIP, err := getPodAnnotationIPsForPrimaryNetworkByIPFamily(
													f.ClientSet,
													peerPod.Namespace,
													peerPod.Name,
													otherNetworkName,
													family,
												)
												gomega.Expect(err).NotTo(gomega.HaveOccurred())
												gomega.Expect(peerPodIP).ToNot(gomega.BeEmpty())
												if peerPodIP == testPodIP {
													framework.Logf("Other network peer pod %s and tested pod share IP %s; verifying pod %s reaches its same-network peer, not the tested pod", peerPod.Name, testPodIP, source.Name)
													testPodToHostnameAndExpect(source, testPodIP, peerPod.Name)
												} else {
													testPodToClientIPNOK(source, testPodIP)
												}
											}
										},
									)
								}

								ginkgo.By("Creating services backed by the other network pods")
								var services []*corev1.Service
								for _, backend := range []*corev1.Pod{otherPodSameNode, otherPodDiffNode} {
									service := e2eservice.CreateServiceSpec(
										"service-"+backend.Name,
										"",
										false,
										backend.Labels,
									)
									service.Spec.Ports = []corev1.ServicePort{{Port: netexecPort}}
									familyPolicy := corev1.IPFamilyPolicyPreferDualStack
									service.Spec.IPFamilyPolicy = &familyPolicy
									var err error
									service, err = f.ClientSet.CoreV1().Services(backend.Namespace).Create(context.Background(), service, metav1.CreateOptions{})
									gomega.Expect(err).NotTo(gomega.HaveOccurred())
									services = append(services, service)
								}

								ginkgo.By("Ensuring the pods from the other network can reach their services")
								for source, target := range map[*corev1.Pod]*corev1.Service{otherPodSameNode: services[1], otherPodDiffNode: services[0]} {
									testForIPFamilies(
										ipFamilySet,
										func(family utilnet.IPFamily) {
											ginkgo.GinkgoHelper()
											framework.Logf("Ensuring a request from the other network pod %s can reach the its network service %s on IPv%v", source.Name, target.Name, family)
											clusterIP := getFirstIPStringOfFamily(family, target.Spec.ClusterIPs)
											gomega.Expect(clusterIP).ToNot(gomega.BeEmpty())
											testPodToClientIP(source, clusterIP)
										},
									)
								}

								ginkgo.By("Ensuring a request from the tested network pod cannot reach the other network services")
								for _, service := range services {
									testForIPFamilies(
										ipFamilySet,
										func(family utilnet.IPFamily) {
											ginkgo.GinkgoHelper()
											framework.Logf("Ensuring a request from the tested network pod cannot reach the other network service %s on IPv%v", service.Name, family)
											clusterIP := getFirstIPStringOfFamily(family, service.Spec.ClusterIPs)
											gomega.Expect(clusterIP).ToNot(gomega.BeEmpty())
											testPodToClientIPNOK(testPod, clusterIP)
										},
									)
								}

								if networkType == cudnAdvertisedEVPNOverlappingCIDRSharedVTEP &&
									testNetworkSpec.EVPN != nil && testNetworkSpec.EVPN.MACVRF != nil {
									// The outer "It can reach external servers on the same network" block already
									// tests north-south connectivity for the tested network in isolation.
									// This additional check runs within the "other network is set up" context,
									// verifying that VNI isolation holds for north-south traffic: even when
									// the other network shares the same VTEP IP and its MAC-VRF external server
									// is reachable at the same IP (both networks derive container IPs from the
									// shared CUDN subnet), the tested pod still reaches only its own external
									// server (identified by hostname) rather than the other network's.
									// Only the MAC-VRF server is checked here: it is the only one whose IP is
									// shared between both networks (secondToLastIP of the CUDN subnet).
									// IP-VRF containers are allocated from a separate per-network subnet
									// (bgpAlloc.IPVRFSubnet6), so they have distinct IPs and there is no
									// shared-IP VNI ambiguity to verify.
									ginkgo.By("Ensuring the tested network pod reaches its own MAC-VRF external server at the shared IP (north-south VNI isolation)")
									macVRFServerName := testNetworkName + "-macvrf-agnhost"
									testForIPFamilies(
										ipFamilySet,
										func(family utilnet.IPFamily) {
											ginkgo.GinkgoHelper()
											serverIP := getExternalServerIPForFamily(macVRFServerName, family)
											gomega.Expect(serverIP).NotTo(gomega.BeEmpty())
											framework.Logf("Ensuring testPod reaches its external server %q at shared IP %s (IPv%v)", macVRFServerName, serverIP, family)
											testPodToHostnameAndExpect(testPod, serverIP, macVRFServerName)
										},
									)
								}
							})
						},
						otherNetworksToTest,
					)
				})
			})
		},
		networksToTest,
	)

	ginkgo.DescribeTable("Validates BUM suppression is in effect for L2 EVPN networks",
		func(bumTestBaseName string, networkSpecGen func(string, string, allocators.BGPAllocation) *udnv1.NetworkSpec) {
			if !isLocalGWModeEnabled() {
				e2eskipper.Skipf("BUM suppression test on L2 EVPN requires Local Gateway mode")
			}
			// Short name (≤10 chars): ensures "svl2-<name>" L2 SVI fits within Linux's 15-char iface limit.
			// Each entry must use a unique name so parallel runs don't conflict on the FRR bridge.
			bumL2SVI := "svl2-" + bumTestBaseName
			ictx := infraprovider.Get().NewTestContext()
			bumTestNetworkName := bumTestBaseName
			ipFamilySet := sets.New(getSupportedIPFamiliesSlice(f.ClientSet)...)
			bgpAlloc, err := allocators.AllocateBGP(f, ictx)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			bumNetworkSpec := networkSpecGen(bgpAlloc.UDNSubnet, bgpAlloc.UDNSubnet6, bgpAlloc)
			bumNetworkSpec.Layer2.Subnets = matchL2SubnetsByIPFamilies(ipFamilySet, bumNetworkSpec.Layer2.Subnets...)

			bumTestNamespace, externalServers, _ := configureNetworkWithInfra(
				f,
				ictx,
				bumTestNetworkName,
				ipFamilySet,
				bumTestNetworkName,
				cudnAdvertisedEVPNUnmanagedRandomVTEP,
				bumNetworkSpec,
				bgpAlloc,
			)
			gomega.Expect(externalServers).NotTo(gomega.BeEmpty(), "MAC-VRF external agnhost container must exist for EVPN L2 test")
			macVRFAgnhostContainer := externalServers[0]

			macVRFNetwork, err := infraprovider.Get().GetNetwork(macVRFAgnhostContainer)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			extAgnhostNetInf, err := infraprovider.Get().GetExternalContainerNetworkInterface(infraapi.ExternalContainer{Name: macVRFAgnhostContainer}, macVRFNetwork)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			extMAC := extAgnhostNetInf.MAC
			gomega.Expect(extMAC).NotTo(gomega.BeEmpty())

			extMACVRFIPs := make(map[utilnet.IPFamily]string)
			if extAgnhostNetInf.IPv4 != "" {
				extMACVRFIPs[utilnet.IPv4] = extAgnhostNetInf.IPv4
			}
			if extAgnhostNetInf.IPv6 != "" {
				extMACVRFIPs[utilnet.IPv6] = extAgnhostNetInf.IPv6
			}
			for _, fam := range ipFamilySet.UnsortedList() {
				gomega.Expect(extMACVRFIPs[fam]).NotTo(gomega.BeEmpty(), "MAC-VRF external IP for family %v", fam)
			}

			vtepCRName := bumTestNetworkName + "-vtep"

			ginkgo.By("Validation 1: Type-3 (BUM flood list) routes for each VTEP")
			macVRFVNI := fmt.Sprintf("%d", bumNetworkSpec.EVPN.MACVRF.VNI)
			externalFRRIP, err := getExternalFRRIP(ipFamilySet)
			gomega.Expect(err).NotTo(gomega.HaveOccurred(), "external FRR VTEP IP")
			// Collect VTEP IPs from the k8s.ovn.org/vteps node annotation written by the EVPN controller.
			var nodes *corev1.NodeList
			var vtepIPs sets.Set[string]
			gomega.Eventually(func() error {
				var err error
				nodes, err = e2enode.GetReadySchedulableNodes(context.Background(), f.ClientSet)
				if err != nil {
					return err
				}
				vtepIPs = sets.New[string]()
				for i := range nodes.Items {
					ann, err := ovnutil.ParseNodeVTEPs(&nodes.Items[i])
					if err != nil {
						return fmt.Errorf("node %s: VTEP annotation not ready: %w", nodes.Items[i].Name, err)
					}
					if entry, ok := ann[vtepCRName]; ok {
						vtepIPs.Insert(entry.IPs...)
					}
				}
				if vtepIPs.Len() == 0 {
					return fmt.Errorf("no VTEP IPs found for CR %q yet", vtepCRName)
				}
				return nil
			}, 60*time.Second, 5*time.Second).Should(gomega.Succeed(), "collecting VTEP IPs for CR %q", vtepCRName)
			gomega.Expect(len(nodes.Items)).To(gomega.BeNumerically(">=", 2), "need at least 2 nodes (PodA on nodeA, tcpdump on nodeB L2 SVI)")
			nodeA, nodeB := nodes.Items[0].Name, nodes.Items[1].Name
			// Also include the external FRR VTEP IP but only for families present in the node tunnel IPs.
			vtepFamilies := sets.New[utilnet.IPFamily]()
			for _, s := range vtepIPs.UnsortedList() {
				vtepFamilies.Insert(utilnet.IPFamilyOfString(s))
			}
			if vtepFamilies.Has(utilnet.IPFamilyOfString(externalFRRIP)) {
				vtepIPs.Insert(externalFRRIP)
			}
			framework.Logf("Validation 1: expect Type-3 routes per VTEP annotation for CR %q (VNI %s)", vtepCRName, macVRFVNI)
			for _, node := range nodes.Items {
				var output string
				gomega.Eventually(func() error {
					var err error
					output, err = execVtyshOnFRRK8s(f, node.Name, fmt.Sprintf("show bgp l2vpn evpn route vni %s type multicast", macVRFVNI))
					if err != nil {
						return err
					}
					for _, vtepIP := range vtepIPs.UnsortedList() {
						nlri := evpnType3MulticastNLRI(vtepIP)
						if !strings.Contains(output, nlri) {
							return fmt.Errorf("node %s: missing Type-3 NLRI %q for VTEP %s", node.Name, nlri, vtepIP)
						}
					}
					return nil
				}, 60*time.Second, 5*time.Second).Should(gomega.Succeed(), "Type-3 routes on %s, last output:\n%s", node.Name, output)
				framework.Logf("node %s: Type-3 (multicast) on frr-k8s:\n%s", node.Name, output)
			}

			ginkgo.By("Place PodA on a dedicated node")
			podASpec := e2epod.NewAgnhostPod(bumTestNamespace.Name, "pod-a", nil, nil, nil, "netexec")
			podASpec.Spec.NodeName = nodeA
			podA := e2epod.PodClientNS(f, bumTestNamespace.Name).CreateSync(context.Background(), podASpec)

			podANetStatus, err := podNetworkStatus(podA, podNetworkStatusByNetConfigPredicate(bumTestNamespace.Name, bumTestNetworkName, "primary"))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(podANetStatus).NotTo(gomega.BeEmpty())
			podAMAC := podANetStatus[0].Mac
			gomega.Expect(podAMAC).NotTo(gomega.BeEmpty())
			podAIPs := make(map[utilnet.IPFamily]string, ipFamilySet.Len())
			for _, ip := range podANetStatus[0].IPs {
				fam := utilnet.IPFamilyOfString(ip)
				if _, ok := podAIPs[fam]; !ok {
					podAIPs[fam] = ip
				}
			}
			for _, fam := range ipFamilySet.UnsortedList() {
				gomega.Expect(podAIPs[fam]).NotTo(gomega.BeEmpty(), "PodA must have a CUDN address for every cluster IP family (missing %v)", fam)
			}

			ginkgo.By("Validation 2: Type-2 (MAC/IP) for PodA and MAC-VRF external agnhost on PodA's node")
			var type2Output string
			gomega.Eventually(func() error {
				var err error
				type2Output, err = execVtyshOnFRRK8s(f, nodeA, fmt.Sprintf("show bgp l2vpn evpn route vni %s type macip", macVRFVNI))
				framework.Logf("Type-2 MAC/IP routes on %s, vtysh out:\n%s", nodeA, type2Output)
				if err != nil {
					return err
				}
				for _, fam := range ipFamilySet.UnsortedList() {
					nlriPod := evpnType2MACIPNLRI(podAMAC, podAIPs[fam], fam)
					if !strings.Contains(strings.ToLower(type2Output), strings.ToLower(nlriPod)) {
						return fmt.Errorf("node %s: missing PodA Type-2 NLRI %q (family %v)", nodeA, nlriPod, fam)
					}
					nlriExt := evpnType2MACIPNLRI(extMAC, extMACVRFIPs[fam], fam)
					if !strings.Contains(strings.ToLower(type2Output), strings.ToLower(nlriExt)) {
						return fmt.Errorf("node %s: missing external MAC-VRF Type-2 NLRI %q (family %v)", nodeA, nlriExt, fam)
					}
				}
				return nil
			}, 60*time.Second, 5*time.Second).Should(gomega.Succeed(), "Type-2 MAC/IP routes on %s, last vtysh out:\n%s", nodeA, type2Output)

			ginkgo.By("Create hostNetwork tcpdump helper on nodeB for L2 SVI capture")
			tcpdumpPodRemote := e2epod.PodClientNS(f, bumTestNamespace.Name).CreateSync(context.Background(), &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "tcpdump-helper-remote", Namespace: bumTestNamespace.Name},
				Spec: corev1.PodSpec{
					NodeName:      nodeB,
					HostNetwork:   true,
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:            "tcpdump",
						Image:           images.Netshoot(),
						Command:         []string{"sleep", "3600"},
						SecurityContext: &corev1.SecurityContext{Privileged: ptr.To(true)},
					}},
				},
			})

			l2SVI := bumL2SVI

			for fam, podAIP := range podAIPs {
				familyStr := fmt.Sprintf("IPv%v", fam)
				parsedPodA := net.ParseIP(podAIP)
				gomega.Expect(parsedPodA).NotTo(gomega.BeNil())
				parsedExtIP := net.ParseIP(extMACVRFIPs[fam])
				gomega.Expect(parsedExtIP).NotTo(gomega.BeNil())

				framework.Logf("L2 SVI iface for %s (CUDN %q): %s (PodA on %s, tcpdump on %s)",
					familyStr, bumTestNetworkName, l2SVI, nodeA, nodeB)
				// Wait for the L2 SVI to appear on nodeB before running tcpdump.
				gomega.Eventually(func() error {
					_, err := e2ekubectl.RunKubectl(
						bumTestNamespace.Name, "exec", tcpdumpPodRemote.Name, "--", "ip", "link", "show", "dev", l2SVI)
					return err
				}).WithTimeout(3*time.Minute).WithPolling(3*time.Second).Should(gomega.Succeed(),
					"L2 SVI device %s must exist on host for pod %s", l2SVI, tcpdumpPodRemote.Name)

				ginkgo.By(fmt.Sprintf("Validation 3 (%s): unknown unicast is flooded (ARP/NS on %s)", familyStr, l2SVI))
				var unallocated string
				for _, cidr := range bumNetworkSpec.Layer2.Subnets {
					_, subnet, err := net.ParseCIDR(string(cidr))
					if err != nil {
						continue
					}
					if isV6 := utilnet.IsIPv6CIDRString(string(cidr)); (fam == utilnet.IPv6) != isV6 {
						continue
					}
					// use PrevIP(secondToLastIP) to avoid the MAC-VRF external agnhost address which uses secondToLastIP in the same subnet
					unallocated = iputils.PrevIP(secondToLastIP(subnet)).String()
					break
				}
				gomega.Expect(unallocated).NotTo(gomega.BeEmpty(), "no unallocated %s in pod subnet for flood probe", familyStr)
				probe := net.ParseIP(unallocated)
				gomega.Expect(probe).NotTo(gomega.BeNil())
				var bumToUnknownFilter string
				if fam == utilnet.IPv4 {
					bumToUnknownFilter = arpBroadcastRequestFilter(probe.To4())
				} else {
					bumToUnknownFilter = ndpBroadcastNSFilter(probe.To16())
				}
				floodCtx, stopFloodTraffic := context.WithCancel(context.Background())
				_ = bumE2EStartFloodAgnhostHTTP(floodCtx, bumTestNamespace.Name, podA.Name, unallocated)
				floodOut, floodErr := e2ekubectl.RunKubectl(
					bumTestNamespace.Name, "exec", tcpdumpPodRemote.Name, "--", "sh", "-c",
					fmt.Sprintf("timeout -s KILL 25 tcpdump -i %s -nne -c 1 %s 2>&1", l2SVI, bumE2EQuoteTcpdumpFilter(bumToUnknownFilter)))
				framework.Logf("tcpdump for %s on %s, floodErr=%v, output:\n%s", unallocated, l2SVI, floodErr, floodOut)
				stopFloodTraffic()
				gomega.Expect(floodErr).NotTo(gomega.HaveOccurred(), "expected one flooded ARP/NS for %s on %s but did not receive it", unallocated, l2SVI, floodOut)

				ginkgo.By(fmt.Sprintf("Validation 4 (%s): BUM suppression both directions — no broadcast ARP/NS for known PodA or external IP on L2 SVI (%s)", familyStr, l2SVI))
				var bumSupComboFilter string
				if fam == utilnet.IPv4 {
					bumSupComboFilter = fmt.Sprintf("(%s) or (%s)",
						arpBroadcastRequestFilter(parsedPodA.To4()),
						arpBroadcastRequestFilter(parsedExtIP.To4()))
				} else {
					bumSupComboFilter = fmt.Sprintf("(%s) or (%s)",
						ndpBroadcastNSFilter(parsedPodA.To16()),
						ndpBroadcastNSFilter(parsedExtIP.To16()))
				}
				var supOutB string
				var supErrB error
				var supWg sync.WaitGroup
				supWg.Add(1)
				go func() {
					defer supWg.Done()
					supOutB, supErrB = e2ekubectl.RunKubectl(
						bumTestNamespace.Name, "exec", tcpdumpPodRemote.Name, "--", "sh", "-c",
						fmt.Sprintf(`out=$(timeout 25 tcpdump -i %s -nne -c 1 %s 2>&1); printf "%%s" "$out"; echo "$out" | grep -q "^0 packets captured"`,
							l2SVI, bumE2EQuoteTcpdumpFilter(bumSupComboFilter)))
				}()
				time.Sleep(5 * time.Second)
				floodSupCtx, stopFloodSup := context.WithCancel(context.Background())
				floodErrCh := bumE2EStartFloodAgnhostHTTP(floodSupCtx, bumTestNamespace.Name, podA.Name, extMACVRFIPs[fam])
				supWg.Wait()
				stopFloodSup()
				framework.Logf("tcpdump node %s for PodA %s OR external %s on %s:\n%s", nodeB, podAIP, extMACVRFIPs[fam], l2SVI, supOutB)
				gomega.Expect(supErrB).NotTo(gomega.HaveOccurred(),
					fmt.Sprintf("node %s: expected tcpdump to time out without matching broadcast ARP/NS for PodA %s or external %s on %s (suppression failed)", nodeB, podAIP, extMACVRFIPs[fam], l2SVI))
				gomega.Expect(<-floodErrCh).NotTo(gomega.HaveOccurred(),
					"%s: PodA should reach external MAC-VRF agnhost at %s", familyStr, bumE2EAgnhostNetexecURL(extMACVRFIPs[fam]))
			}
			framework.Logf("BUM suppression: done for all families for network %q", bumTestNetworkName)
		},
		ginkgo.Entry("MAC-VRF only", feature.EVPN, "bsup1", layer2MACVRFNetworkSpecGen),
		ginkgo.Entry("MAC-VRF and IP-VRF", feature.EVPN, "bsup2", layer2MACVRFIPVRFNetworkSpecGen),
	)
})

// getFRRK8sPodOnNode returns the frr-k8s DaemonSet pod on the given node.
func getFRRK8sPodOnNode(f *framework.Framework, nodeName string) (*corev1.Pod, error) {
	ns := deploymentconfig.Get().FRRK8sNamespace()
	pods, err := f.ClientSet.CoreV1().Pods(ns).List(context.Background(), metav1.ListOptions{
		FieldSelector: "spec.nodeName=" + nodeName,
		LabelSelector: "app=frr-k8s",
	})
	if err != nil {
		return nil, fmt.Errorf("list frr-k8s pods in %s: %w", ns, err)
	}
	if len(pods.Items) != 1 {
		return nil, fmt.Errorf("expected 1 frr-k8s daemon on node %s, got %d", nodeName, len(pods.Items))
	}
	p := &pods.Items[0]
	if !podutils.IsPodReady(p) {
		return nil, fmt.Errorf("frr-k8s pod on node %s is not ready", nodeName)
	}
	return p, nil
}

// execVtyshOnFRRK8s runs a vtysh -c command in the FRR sidecar of the frr-k8s pod on the given node.
func execVtyshOnFRRK8s(f *framework.Framework, nodeName string, vtyshCmd string) (string, error) {
	pod, err := getFRRK8sPodOnNode(f, nodeName)
	if err != nil {
		return "", err
	}
	args := []string{"exec", pod.Name, "-c", frrContainerName, "--", "vtysh", "-c", vtyshCmd}
	return e2ekubectl.RunKubectl(deploymentconfig.Get().FRRK8sNamespace(), args...)
}

// bumE2EQuoteTcpdumpFilter wraps a BPF for safe use in sh -c for tcpdump.
func bumE2EQuoteTcpdumpFilter(f string) string {
	return `'` + strings.ReplaceAll(f, `'`, `'"'"'`) + `'`
}

// bumE2EAgnhostNetexecURL is the in-pod URL for the default agnhost netexec --http-port=%d/hostname (see netexecPort). Uses net.JoinHostPort for IPv4/IPv6.
func bumE2EAgnhostNetexecURL(host string) string {
	return fmt.Sprintf("http://%s/hostname", net.JoinHostPort(host, strconv.Itoa(netexecPort)))
}

// bumE2EStartFloodAgnhostHTTP issues periodic HTTP GETs from podA to agnhost
// on targetIP in a background goroutine, running until ctx is cancelled.
// The returned channel receives the last error (nil on success) when the goroutine exits.
func bumE2EStartFloodAgnhostHTTP(ctx context.Context, namespace, podAName, targetIP string) <-chan error {
	url := bumE2EAgnhostNetexecURL(targetIP)
	errCh := make(chan error, 1)
	go func() {
		var lastErr error
		for {
			select {
			case <-ctx.Done():
				errCh <- lastErr
				return
			default:
			}
			_, lastErr = e2ekubectl.RunKubectl(namespace, "exec", podAName, "--", "curl", "-g", "-s", "-o", "/dev/null", "-m", "1", "--connect-timeout", "1", url)
			time.Sleep(200 * time.Millisecond)
		}
	}()
	return errCh
}

// arpBroadcastRequestFilter matches an IPv4 ARP who-has to targetIP with broadcast eth dst.
func arpBroadcastRequestFilter(targetIP net.IP) string {
	ip := targetIP.To4()
	if ip == nil {
		return ""
	}
	return fmt.Sprintf("arp and ether dst ff:ff:ff:ff:ff:ff and arp[6:2] == 0x0001 and arp[24:4] == 0x%02x%02x%02x%02x",
		ip[0], ip[1], ip[2], ip[3])
}

// ndpBroadcastNSFilter matches an ICMPv6 Neighbor Solicitation to targetIP with solicited-node multicast dmac.
func ndpBroadcastNSFilter(targetIP net.IP) string {
	ip := targetIP.To16()
	if ip == nil {
		return ""
	}
	if len(ip) != 16 {
		return ""
	}
	return fmt.Sprintf("icmp6 and ip6[40] == 135 and ether[0:2] == 0x3333"+
		" and ip6[48:4] == 0x%02x%02x%02x%02x"+
		" and ip6[52:4] == 0x%02x%02x%02x%02x"+
		" and ip6[56:4] == 0x%02x%02x%02x%02x"+
		" and ip6[60:4] == 0x%02x%02x%02x%02x",
		ip[0], ip[1], ip[2], ip[3], ip[4], ip[5], ip[6], ip[7], ip[8], ip[9], ip[10], ip[11], ip[12], ip[13], ip[14], ip[15])
}

// routeAdvertisementsReadyFunc returns a function that checks for the
// Accepted condition in the provided RouteAdvertisements
func routeAdvertisementsReadyFunc(c raclientset.Clientset, name string) func() error {
	return func() error {
		ra, err := c.K8sV1().RouteAdvertisements().Get(context.Background(), name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		conditionType := "Accepted"
		condition := meta.FindStatusCondition(ra.Status.Conditions, conditionType)
		if condition == nil {
			return fmt.Errorf("no %q condition found in: %v", conditionType, ra)
		}
		if condition.Status != metav1.ConditionTrue {
			return fmt.Errorf("condition %v has unexpected status %v", condition, condition.Status)
		}
		return nil
	}
}

// templateInputRouter data
type templateInputRouter struct {
	VRF           string
	NeighborsIPv4 []string
	NeighborsIPv6 []string
	NetworksIPv4  []string
	NetworksIPv6  []string
}

// templateInputFRR data
type templateInputFRR struct {
	// Name and Label are used for FRRConfiguration metadata
	Name    string
	Labels  map[string]string
	Routers []templateInputRouter
}

// for routeadvertisements test cases we generate configuration from templates embed in the program
//
//go:embed testdata/routeadvertisements
var ratestdata embed.FS
var tmplDir = filepath.Join("testdata", "routeadvertisements")

// generateFRRConfiguration to establish a BGP session towards the provided
// neighbors in the network's VRF configured to advertised the provided
// networks. Returns a temporary directory where the configuration is generated.
func generateFRRConfiguration(neighborIPs, advertiseNetworks []string) (directory string, err error) {
	// parse configuration templates
	var templates *template.Template
	templates, err = template.ParseFS(ratestdata, filepath.Join(tmplDir, "frr", "*.tmpl"))
	if err != nil {
		return "", fmt.Errorf("failed to parse templates: %w", err)
	}

	// create the directory that will hold the configuration files
	directory, err = os.MkdirTemp("", "frrconf-")
	if err != nil {
		return "", fmt.Errorf("failed to make temp directory: %w", err)
	}
	defer func() {
		if err != nil {
			os.RemoveAll(directory)
		}
	}()

	// generate external frr configuration executing the templates
	networksIPv4, networksIPv6 := splitCIDRStringsByIPFamily(advertiseNetworks)
	neighborsIPv4, neighborsIPv6 := splitIPStringsByIPFamily(neighborIPs)
	conf := templateInputFRR{
		Routers: []templateInputRouter{
			{
				NeighborsIPv4: neighborsIPv4,
				NetworksIPv4:  networksIPv4,
				NeighborsIPv6: neighborsIPv6,
				NetworksIPv6:  networksIPv6,
			},
		},
	}

	err = executeFileTemplate(templates, directory, "frr.conf", conf)
	if err != nil {
		return "", fmt.Errorf("failed to execute template %q: %w", "frr.conf", err)
	}
	err = executeFileTemplate(templates, directory, "daemons", nil)
	if err != nil {
		return "", fmt.Errorf("failed to execute template %q: %w", "daemons", err)
	}

	return directory, nil
}

// generateFRRk8sConfiguration for the provided network (which doubles up as the
// FRRConfiguration instance name, VRF name and used as value of `network`
// label) to establish a BGP session towards the provided neighbors in the
// network's VRF, configured to receive advertisements for the provided
// networks. Returns a temporary directory where the configuration is generated.
func generateFRRk8sConfiguration(networkName string, neighborIPs, receiveNetworks []string) (directory string, err error) {
	return generateFRRk8sConfigurationForVRF(networkName, networkName, neighborIPs, receiveNetworks)
}

func generateFRRk8sConfigurationForVRF(networkName, vrf string, neighborIPs, receiveNetworks []string) (directory string, err error) {
	// parse configuration templates
	var templates *template.Template
	templates, err = template.ParseFS(ratestdata, filepath.Join(tmplDir, "frr-k8s", "*.tmpl"))
	if err != nil {
		return "", fmt.Errorf("failed to parse templates: %w", err)
	}

	// create the directory that will hold the configuration files
	directory, err = os.MkdirTemp("", "frrk8sconf-")
	if err != nil {
		return "", fmt.Errorf("failed to make temp directory: %w", err)
	}
	defer func() {
		if err != nil {
			os.RemoveAll(directory)
		}
	}()

	receivesIPv4, receivesIPv6 := splitCIDRStringsByIPFamily(receiveNetworks)
	neighborsIPv4, neighborsIPv6 := splitIPStringsByIPFamily(neighborIPs)
	conf := templateInputFRR{
		Name:   networkName,
		Labels: map[string]string{"network": networkName},
		Routers: []templateInputRouter{
			{
				VRF:           vrf,
				NeighborsIPv4: neighborsIPv4,
				NeighborsIPv6: neighborsIPv6,
				NetworksIPv4:  receivesIPv4,
				NetworksIPv6:  receivesIPv6,
			},
		},
	}
	err = executeFileTemplate(templates, directory, "frrconf.yaml", conf)
	if err != nil {
		return "", fmt.Errorf("failed to execute template %q: %w", "frrconf.yaml", err)
	}

	return directory, nil
}

// runBGPNetworkAndServer configures a topology appropriate to be used with
// route advertisement test cases. For VRF-Lite test cases, the caller is
// resposible to attach the peer network interface to the CUDN VRF on the nodes.
//
// -----------------                 ------------------                            ---------------
// |               |  serverNetwork  |                |       peerNetwork          |             |
// |   external    |<--------------- |   FRR router   |<--( Default / CUDN VRF )-- |   cluster   |
// |    server     |                 |                |                            |             |
// -----------------                 ------------------                            ---------------
func runBGPNetworkAndServer(
	f *framework.Framework,
	ictx infraapi.Context,
	ipFamilySet sets.Set[utilnet.IPFamily],
	networkName string,
	serverName string,
	serverNetworkName string,
	peerNetworks []string,
	serverNetworks []string,
) error {
	return runBGPNetworkAndServerWithFRRVRF(
		f,
		ictx,
		ipFamilySet,
		networkName,
		serverName,
		serverNetworkName,
		peerNetworks,
		serverNetworks,
		networkName,
	)
}

func runBGPNetworkAndServerWithFRRVRF(
	f *framework.Framework,
	ictx infraapi.Context,
	ipFamilySet sets.Set[utilnet.IPFamily],
	networkName string,
	serverName string,
	serverNetworkName string,
	peerNetworks []string,
	serverNetworks []string,
	frrVRF string,
) error {
	// filter networks by supported IP families
	peerNetworks = matchCIDRStringsByIPFamilySet(peerNetworks, ipFamilySet)
	serverNetworks = matchCIDRStringsByIPFamilySet(serverNetworks, ipFamilySet)

	// create BGP peer network
	bgpPeerNetwork, err := ictx.CreateNetwork(networkName, peerNetworks...)
	if err != nil {
		return fmt.Errorf("failed to create peer network %v: %w", peerNetworks, err)
	}

	// create the server network
	serverNetwork, err := ictx.CreateNetwork(serverNetworkName, serverNetworks...)
	if err != nil {
		return fmt.Errorf("failed to create server network %v: %w", serverNetworks, err)
	}

	// attach BGP peer network to all nodes
	var nodeIPs []string
	nodeList, err := f.ClientSet.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list nodes: %w", err)
	}
	for _, node := range nodeList.Items {
		iface, err := ictx.AttachNetwork(bgpPeerNetwork, node.Name)
		if err != nil {
			return fmt.Errorf("failed to attach node %q to network: %w", node.Name, err)
		}
		nodeIPs = append(nodeIPs, iface.IPv4, iface.IPv6)
	}

	// run frr container
	advertiseNetworks := serverNetworks
	frrConfig, err := generateFRRConfiguration(nodeIPs, advertiseNetworks)
	if err != nil {
		return fmt.Errorf("failed to generate FRR configuration: %w", err)
	}
	ictx.AddCleanUpFn(func() error { return os.RemoveAll(frrConfig) })
	frr := infraapi.ExternalContainer{
		Name:        networkName + "-frr",
		Image:       images.FRR(),
		Network:     bgpPeerNetwork,
		RuntimeArgs: []string{"--volume", frrConfig + ":" + filepath.Join(filepath.FromSlash("/"), "etc", "frr")},
	}
	frr, err = ictx.CreateExternalContainer(frr)
	if err != nil {
		return fmt.Errorf("failed to create frr container: %w", err)
	}
	// enable IPv6 forwarding if required
	if frr.IPv6 != "" {
		_, err = infraprovider.Get().ExecExternalContainerCommand(frr, []string{"sysctl", "-w", "net.ipv6.conf.all.forwarding=1"})
		if err != nil {
			return fmt.Errorf("failed to set enable IPv6 forwading on frr container: %w", err)
		}
	}

	// connect frr to server network
	frrServerNetworkInterface, err := ictx.AttachNetwork(serverNetwork, frr.Name)
	if err != nil {
		return fmt.Errorf("failed to connect frr to server network: %w", err)
	}

	// run server container
	server := infraapi.ExternalContainer{
		Name:    serverName,
		Image:   images.AgnHost(),
		CmdArgs: []string{"netexec"},
		Network: serverNetwork,
	}
	_, err = ictx.CreateExternalContainer(server)
	if err != nil {
		return fmt.Errorf("failed to create BGP server container: %w", err)
	}

	// set frr as default gateway for the server
	if frrServerNetworkInterface.IPv4 != "" {
		_, err = infraprovider.Get().ExecExternalContainerCommand(server, []string{"ip", "route", "add", "default", "via", frrServerNetworkInterface.IPv4})
		if err != nil {
			return fmt.Errorf("failed to set default IPv4 gateway on BGP server container: %w", err)
		}
	}
	if frrServerNetworkInterface.IPv6 != "" {
		_, err = infraprovider.Get().ExecExternalContainerCommand(server, []string{"ip", "-6", "route", "add", "default", "via", frrServerNetworkInterface.IPv6})
		if err != nil {
			return fmt.Errorf("failed to set default IPv6 gateway on BGP server container: %w", err)
		}

	}

	// apply FRR-K8s Configuration
	receiveNetworks := serverNetworks
	frrK8sConfig, err := generateFRRk8sConfigurationForVRF(networkName, frrVRF, []string{frr.IPv4, frr.IPv6}, receiveNetworks)
	if err != nil {
		return fmt.Errorf("failed to generate FRR-k8s configuration: %w", err)
	}
	ictx.AddCleanUpFn(func() error { return os.RemoveAll(frrK8sConfig) })
	_, err = e2ekubectl.RunKubectl(deploymentconfig.Get().FRRK8sNamespace(), "create", "-f", frrK8sConfig)
	if err != nil {
		return fmt.Errorf("failed to apply FRRConfiguration: %w", err)
	}
	ictx.AddCleanUpFn(func() error {
		_, err = e2ekubectl.RunKubectl(deploymentconfig.Get().FRRK8sNamespace(), "delete", "-f", frrK8sConfig)
		if err != nil {
			return fmt.Errorf("failed to delete FRRConfiguration: %w", err)
		}
		return nil
	})

	return nil
}

type networkType string

const (
	defaultNetwork                              networkType = "DEFAULT"
	udn                                         networkType = "UDN"
	cudn                                        networkType = "CUDN"
	cudnAdvertised                              networkType = "CUDN_ADVERTISED"
	cudnAdvertisedVRFLite                       networkType = "CUDN_ADVERTISED_VRFLITE"
	cudnAdvertisedEVPNUnmanagedSharedVTEP       networkType = "CUDN_ADVERTISED_EVPN_UNMANAGED_SHARED_VTEP"
	cudnAdvertisedEVPNUnmanagedRandomVTEP       networkType = "CUDN_ADVERTISED_EVPN_UNMANAGED_RANDOM_VTEP"
	cudnAdvertisedEVPNOverlappingCIDRSharedVTEP networkType = "CUDN_ADVERTISED_EVPN_OVERLAPPING_CIDR_SHARED_VTEP"
)

// createNamespaceWithPrimaryNetworkOfType helper function configures a
// namespace, a optional(C)UDN and an optional RouteAdvertisements as determined
// by `networkType` argument. The RouteAdvertisements is aligned with the
// configuration done with `runBGPNetworkAndServer` for VRF-Lite scenarios.
func createNamespaceWithPrimaryNetworkOfType(
	f *framework.Framework,
	ictx infraapi.Context,
	testName string,
	networkName string,
	networkType networkType,
	networkSpec *udnv1.NetworkSpec,
) (*corev1.Namespace, error) {
	// define some configuration based on the type of namespace/network/advertisement
	var targetVRF string
	var networkLabels map[string]string
	var frrConfigurationLabels map[string]string
	switch networkType {
	case cudnAdvertised:
		networkLabels = map[string]string{"advertise": networkName}
		frrConfigurationLabels = map[string]string{"name": "receive-all"}
	case cudnAdvertisedVRFLite, cudnAdvertisedEVPNUnmanagedSharedVTEP, cudnAdvertisedEVPNUnmanagedRandomVTEP, cudnAdvertisedEVPNOverlappingCIDRSharedVTEP:
		targetVRF = networkName
		networkLabels = map[string]string{"advertise": networkName}
		frrConfigurationLabels = map[string]string{"network": networkName}
	}

	nsLabels := map[string]string{
		"e2e-framework": testName,
	}
	if networkType != defaultNetwork {
		nsLabels[RequiredUDNNamespaceLabel] = ""
	}
	namespace, err := f.CreateNamespace(context.Background(), networkName, nsLabels)
	if err != nil {
		return nil, fmt.Errorf("failed to create namespace: %w", err)
	}
	ictx.AddCleanUpFn(func() error {
		return f.ClientSet.CoreV1().Namespaces().Delete(context.Background(), namespace.Name, metav1.DeleteOptions{})
	})

	// just creating a namespace with default network, return
	if networkType == defaultNetwork {
		return namespace, nil
	}

	err = createUserDefinedNetwork(
		f,
		ictx,
		namespace,
		networkName,
		networkType != udn,
		networkSpec,
		networkLabels,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create primary network: %w", err)
	}

	// not advertised, return
	if networkType == udn || networkType == cudn {
		return namespace, nil
	}

	err = createRouteAdvertisements(
		f,
		ictx,
		networkName,
		targetVRF,
		networkLabels,
		frrConfigurationLabels,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create primary network: %w", err)
	}

	return namespace, nil
}

func createUserDefinedNetwork(
	f *framework.Framework,
	ictx infraapi.Context,
	namespace *corev1.Namespace,
	name string,
	cudnType bool,
	networkSpec *udnv1.NetworkSpec,
	networkLabels map[string]string,
) error {
	var gvr schema.GroupVersionResource
	var gvk schema.GroupVersionKind
	var obj runtime.Object
	var client dynamic.ResourceInterface
	switch {
	case cudnType:
		gvr = clusterUDNGVR
		gvk = schema.GroupVersionKind{
			Group:   gvr.Group,
			Version: gvr.Version,
			Kind:    "ClusterUserDefinedNetwork",
		}
		client = f.DynamicClient.Resource(gvr)
		obj = &udnv1.ClusterUserDefinedNetwork{
			ObjectMeta: metav1.ObjectMeta{
				Name:   name,
				Labels: networkLabels,
			},
			Spec: udnv1.ClusterUserDefinedNetworkSpec{
				NamespaceSelector: metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
					Key:      "kubernetes.io/metadata.name",
					Operator: metav1.LabelSelectorOpIn,
					Values:   []string{namespace.Name},
				}}},
				Network: *networkSpec,
			},
		}
	default:
		gvr = udnGVR
		gvk = schema.GroupVersionKind{
			Group:   gvr.Group,
			Version: gvr.Version,
			Kind:    "UserDefinedNetwork",
		}
		client = f.DynamicClient.Resource(gvr).Namespace(namespace.Name)
		obj = &udnv1.UserDefinedNetwork{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace.Name,
				Labels:    networkLabels,
			},
			Spec: udnv1.UserDefinedNetworkSpec{
				Topology: networkSpec.Topology,
				Layer3:   networkSpec.Layer3,
				Layer2:   networkSpec.Layer2,
			},
		}
	}

	unstructuredMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return fmt.Errorf("failed to convert network to unstructured: %w", err)
	}
	unstructuredObj := &unstructured.Unstructured{Object: unstructuredMap}
	ok := unstructuredObj.GetObjectKind()
	ok.SetGroupVersionKind(gvk)

	_, err = client.Create(context.Background(), unstructuredObj, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("failed to convert network to unstructured: %w", err)
	}
	ictx.AddCleanUpFn(func() error {
		return client.Delete(context.Background(), name, metav1.DeleteOptions{})
	})
	var lastReadyErr error
	if err := wait.PollUntilContextTimeout(
		context.Background(),
		time.Second,
		30*time.Second,
		true,
		func(ctx context.Context) (bool, error) {
			lastReadyErr = networkReadyFunc(ctx, client, name)()
			return lastReadyErr == nil, nil
		},
	); err != nil {
		if lastReadyErr != nil {
			return fmt.Errorf("failed to wait for the network to be ready: %w", lastReadyErr)
		}
		return fmt.Errorf("failed to wait for the network to be ready: %w", err)
	}

	return nil
}

func createRouteAdvertisements(
	f *framework.Framework,
	ictx infraapi.Context,
	name string,
	targetVRF string,
	networkMatchLabels map[string]string,
	frrconfigurationMatchLabels map[string]string,
) error {
	ra := &rav1.RouteAdvertisements{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: rav1.RouteAdvertisementsSpec{
			NetworkSelectors: apitypes.NetworkSelectors{
				apitypes.NetworkSelector{
					NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
					ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{
						NetworkSelector: metav1.LabelSelector{
							MatchLabels: networkMatchLabels,
						},
					},
				},
			},
			FRRConfigurationSelector: metav1.LabelSelector{
				MatchLabels: frrconfigurationMatchLabels,
			},
			NodeSelector: metav1.LabelSelector{},
			Advertisements: []rav1.AdvertisementType{
				rav1.PodNetwork,
			},
			TargetVRF: targetVRF,
		},
	}

	raClient, err := raclientset.NewForConfig(f.ClientConfig())
	if err != nil {
		return fmt.Errorf("failed to create RouteAdvertisements client: %w", err)
	}
	_, err = raClient.K8sV1().RouteAdvertisements().Create(context.TODO(), ra, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("failed to create RouteAdvertisements: %w", err)
	}
	ictx.AddCleanUpFn(func() error {
		err := raClient.K8sV1().RouteAdvertisements().Delete(context.Background(), name, metav1.DeleteOptions{})
		if apierrors.IsNotFound(err) {
			// tolerate tests that delete the RouteAdvertisements themselves
			return nil
		}
		return err
	})
	var lastReadyErr error
	if err := wait.PollUntilContextTimeout(
		context.Background(),
		time.Second,
		30*time.Second,
		true,
		func(ctx context.Context) (bool, error) {
			lastReadyErr = routeAdvertisementsReadyFunc(*raClient, name)()
			return lastReadyErr == nil, nil
		},
	); err != nil {
		if lastReadyErr != nil {
			return fmt.Errorf("failed to wait for the RouteAdvertisements to be ready: %w", lastReadyErr)
		}
		return fmt.Errorf("failed to wait for the RouteAdvertisements to be ready: %w", err)
	}

	return nil
}

// getBGPServerContainerIPs retrieves the IP addresses of the BGP server container.
func getBGPServerContainerIPs(f *framework.Framework) (serverContainerIPs []string) {
	bgpNetwork, err := infraprovider.Get().GetNetwork(bgpExternalNetworkName) // pre-created network
	framework.ExpectNoError(err, "must get %s network", bgpExternalNetworkName)
	bgpServer := infraapi.ExternalContainer{Name: bgpExternalServerContainerName}
	networkInterface, err := infraprovider.Get().GetExternalContainerNetworkInterface(bgpServer, bgpNetwork)
	framework.ExpectNoError(err, "container %s attached to network %s must contain network info", bgpExternalServerContainerName, bgpExternalNetworkName)
	if isIPv4Supported(f.ClientSet) && len(networkInterface.IPv4) > 0 {
		serverContainerIPs = append(serverContainerIPs, networkInterface.IPv4)
	}
	if isIPv6Supported(f.ClientSet) && len(networkInterface.IPv6) > 0 {
		serverContainerIPs = append(serverContainerIPs, networkInterface.IPv6)
	}
	return
}

// isNoOverlayOutboundSNATEnabled reads the ovnkube-config configmap to determine
// whether outbound SNAT is enabled in no-overlay mode.
func isNoOverlayOutboundSNATEnabled(f *framework.Framework) bool {
	cm, err := f.ClientSet.CoreV1().ConfigMaps(deploymentconfig.Get().OVNKubernetesNamespace()).Get(
		context.TODO(), "ovnkube-config", metav1.GetOptions{})
	gomega.Expect(err).NotTo(gomega.HaveOccurred(), "must get ovnkube-config configmap")
	conf, ok := cm.Data["ovnkube.conf"]
	gomega.Expect(ok).To(gomega.BeTrue(), "ovnkube.conf key must be present in ovnkube-config configmap")
	re := regexp.MustCompile(`(?m)^\s*outbound-snat\s*=\s*enabled\s*$`)
	return re.MatchString(conf)
}

// checkL3NodePodRoute checks that the BGP route for the given node's pod subnet is present in the FRR router.
// It takes the node to check, a serverContainerIP to determine the IP family in use, and the router container name.
func checkL3NodePodRoute(node corev1.Node, serverContainerIP, routerContainerName, netName string) {
	var podv4CIDR, podv6CIDR string

	gomega.Eventually(func() error {
		var err error
		podv4CIDR, podv6CIDR, err = getNodePodCIDRs(node.Name, netName)
		return err
	}, 5*time.Second).Should(gomega.Succeed(), "failed to get pod CIDR for node %s, network %s", node.Name, netName)

	framework.Logf("The pod CIDRs for node %s are: v4=%s, v6=%s", node.Name, podv4CIDR, podv6CIDR)
	isIPv6 := utilnet.IsIPv6String(serverContainerIP)
	podCIDR := podv4CIDR
	if isIPv6 {
		podCIDR = podv6CIDR
	}
	gomega.Expect(podCIDR).NotTo(gomega.BeEmpty(),
		"pod CIDR for family (isIPv6=%t) missing for node %s on network %s", isIPv6, node.Name, netName)

	checkRouteInFRR(node, podCIDR, routerContainerName, isIPv6)
}

// checkL2NodePodRoute checks that BGP routes for the given CIDRs are present in the FRR router.
func checkL2NodePodRoute(node corev1.Node, serverContainerIP, routerContainerName string, cidrs udnv1.DualStackCIDRs) {
	isServerIPv6 := utilnet.IsIPv6String(serverContainerIP)
	for _, podCIDR := range cidrs {
		isPodCIDRv6 := utilnet.IsIPv6CIDRString(string(podCIDR))
		// Skip checking if the CIDR family does not match the serverContainerIP family.
		if isServerIPv6 != isPodCIDRv6 {
			continue
		}
		checkRouteInFRR(node, string(podCIDR), routerContainerName, isPodCIDRv6)
	}
}

// checkRouteInFRR verifies that a route for a given podCIDR exists in the specified FRR container,
// with the correct node as its nexthop.
func checkRouteInFRR(node corev1.Node, podCIDR, routerContainerName string, isIPv6 bool) {
	var (
		ipVer  string
		nodeIP []string
		err    error
	)

	if isIPv6 {
		ipVer = " -6"
		// BGP uses the link-local address as the nexthop for IPv6 routes by default.
		var nodeIPv6LLA string
		nodeIPv6LLA, err = GetNodeIPv6LinkLocalAddressForEth0(node.Name)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		nodeIP = []string{nodeIPv6LLA}
	} else {
		nodeIP = e2enode.GetAddressesByTypeAndFamily(&node, corev1.NodeInternalIP, corev1.IPv4Protocol)
	}
	gomega.Expect(len(nodeIP)).To(gomega.BeNumerically(">", 0), "must find a valid nexthop IP for node %s", node.Name)
	framework.Logf("Using nexthop %s for node %s", nodeIP[0], node.Name)

	externalContainer := infraapi.ExternalContainer{Name: routerContainerName}
	bgpRouteCommand := strings.Split(fmt.Sprintf("ip%s route show %s", ipVer, podCIDR), " ")
	framework.Logf("Checking on router %s for node %s's route to pod subnet %s", routerContainerName, node.Name, podCIDR)

	gomega.Eventually(func() bool {
		routes, err := infraprovider.Get().ExecExternalContainerCommand(externalContainer, bgpRouteCommand)
		framework.ExpectNoError(err, "failed to get BGP routes from intermediary router")
		framework.Logf("Routes in FRR for %s: %s", podCIDR, routes)
		return strings.Contains(routes, nodeIP[0])
	}, 30*time.Second).Should(gomega.BeTrue(), "route for %s via %s not found on %s", podCIDR, nodeIP[0], routerContainerName)
}

func hasRouteInCUDNVRF(node corev1.Node, cudnName, cidr string, nextHops ...string) (bool, error) {
	if len(cudnName) > 15 {
		return false, fmt.Errorf("CUDN name %q is too long to be used as the Linux VRF device name", cudnName)
	}

	routeCommand := []string{"ip", "--json", "route", "show", "vrf", cudnName, cidr}
	if utilnet.IsIPv6CIDRString(cidr) {
		routeCommand = []string{"ip", "-6", "--json", "route", "show", "vrf", cudnName, cidr}
	}

	out, err := infraprovider.Get().ExecK8NodeCommand(node.GetName(), routeCommand)
	if err != nil {
		return false, fmt.Errorf("failed to get routes from CUDN VRF %s on node %s: %w", cudnName, node.Name, err)
	}
	routes := []kernelRoute{}
	if err := json.Unmarshal([]byte(out), &routes); err != nil {
		return false, fmt.Errorf("failed to parse routes from CUDN VRF %s on node %s: %w; output: %s", cudnName, node.Name, err, out)
	}
	framework.Logf("Routes in CUDN VRF %s on node %s for %s: %s", cudnName, node.Name, cidr, out)
	return hasBGPRoute(routes, cidr, nextHops...), nil
}

type kernelRoute struct {
	Dst      string               `json:"dst"`
	Gateway  string               `json:"gateway"`
	Protocol string               `json:"protocol"`
	Nexthops []kernelRouteNexthop `json:"nexthops"`
}

type kernelRouteNexthop struct {
	Gateway string `json:"gateway"`
}

func hasBGPRoute(routes []kernelRoute, cidr string, nextHops ...string) bool {
	for _, route := range routes {
		if route.Dst == cidr && route.Protocol == "bgp" && routeHasGateway(route, nextHops...) {
			return true
		}
	}
	return false
}

func routeHasGateway(route kernelRoute, nextHops ...string) bool {
	for _, nextHop := range nextHops {
		if route.Gateway == nextHop {
			return true
		}
		for _, nexthop := range route.Nexthops {
			if nexthop.Gateway == nextHop {
				return true
			}
		}
	}
	return false
}
