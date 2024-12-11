package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	"github.com/google/go-cmp/cmp"
	nettypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	"k8s.io/kubernetes/test/e2e/framework/skipper"
	utilnet "k8s.io/utils/net"
)

// This is the image used for the containers acting as externalgateways, built
// out from the e2e/images/Dockerfile.frr dockerfile
const (
	externalContainerImage          = "quay.io/trozet/ovnkbfdtest:0.3"
	srcHTTPPort                     = 80
	srcUDPPort                      = 90
	externalGatewayPodIPsAnnotation = "k8s.ovn.org/external-gw-pod-ips"
	defaultPolicyName               = "default-route-policy"
	anyLink                         = "any"
)

var externalContainerNetwork = "kind"
var externalContainerIPv4 = ""
var externalContainerIPv6 = ""

func init() {
	// When the env variable is specified, we use a different docker network for
	// containers acting as external gateways.
	// In a specific case where the variable is set to `host` we create only one
	// external container to act as an external gateway, as we can't create 2
	// because of overlapping ip/ports (like the bfd port).
	if exNetwork, found := os.LookupEnv("OVN_TEST_EX_GW_NETWORK"); found {
		externalContainerNetwork = exNetwork
	}

	// When OVN_TEST_EX_GW_NETWORK is set to "host" we need to set the container's IP from outside
	if exHostIPv4, found := os.LookupEnv("OVN_TEST_EX_GW_IPV4"); found {
		externalContainerIPv4 = exHostIPv4
	}

	if exHostIPv6, found := os.LookupEnv("OVN_TEST_EX_GW_IPV6"); found {
		externalContainerIPv6 = exHostIPv6
	}
}

// gatewayTestIPs collects all the addresses required for an external gateway
// test.
type gatewayTestIPs struct {
	gatewayIPs []string
	srcPodIP   string
	nodeIP     string
	targetIPs  []string
}

var _ = ginkgo.Describe("External Gateway", func() {

	// Validate pods can reach a network running in a container's looback address via
	// an external gateway running on eth0 of the container without any tunnel encap.
	// Next, the test updates the namespace annotation to point to a second container,
	// emulating the ext gateway. This test requires shared gateway mode in the job infra.
	var _ = ginkgo.Describe("e2e non-vxlan external gateway and update validation", func() {
		const (
			svcname             string = "multiple-novxlan-externalgw"
			ovnWorkerNode       string = "ovn-worker"
			ovnContainer        string = "ovnkube-node"
			gwContainerNameAlt1 string = "gw-novxlan-test-container-alt1"
			gwContainerNameAlt2 string = "gw-novxlan-test-container-alt2"
			ovnControlNode      string = "ovn-control-plane"
		)
		var (
			exGWRemoteIpAlt1 string
			exGWRemoteIpAlt2 string
		)
		f := wrappedTestFramework(svcname)

		// Determine what mode the CI is running in and get relevant endpoint information for the tests
		ginkgo.BeforeEach(func() {
			exGWRemoteIpAlt1 = "10.249.3.1"
			exGWRemoteIpAlt2 = "10.249.4.1"
			if IsIPv6Cluster(f.ClientSet) {
				exGWRemoteIpAlt1 = "fc00:f853:ccd:e793::1"
				exGWRemoteIpAlt2 = "fc00:f853:ccd:e794::1"
			}
		})

		ginkgo.AfterEach(func() {
			// tear down the containers simulating the gateways
			if cid, _ := runCommand(containerRuntime, "ps", "-qaf", fmt.Sprintf("name=%s", gwContainerNameAlt1)); cid != "" {
				if _, err := runCommand(containerRuntime, "rm", "-f", gwContainerNameAlt1); err != nil {
					framework.Logf("failed to delete the gateway test container %s %v", gwContainerNameAlt1, err)
				}
			}
			if cid, _ := runCommand(containerRuntime, "ps", "-qaf", fmt.Sprintf("name=%s", gwContainerNameAlt2)); cid != "" {
				if _, err := runCommand(containerRuntime, "rm", "-f", gwContainerNameAlt2); err != nil {
					framework.Logf("failed to delete the gateway test container %s %v", gwContainerNameAlt2, err)
				}
			}
		})

		ginkgo.It("Should validate connectivity without vxlan before and after updating the namespace annotation to a new external gateway", func() {

			var pingSrc string
			var validIP net.IP

			isIPv6Cluster := IsIPv6Cluster(f.ClientSet)
			srcPingPodName := "e2e-exgw-novxlan-src-ping-pod"
			command := []string{"bash", "-c", "sleep 20000"}
			testContainer := fmt.Sprintf("%s-container", srcPingPodName)
			testContainerFlag := fmt.Sprintf("--container=%s", testContainer)
			// non-ha ci mode runs a set of kind nodes prefixed with ovn-worker
			ciWorkerNodeSrc := ovnWorkerNode

			// start the container that will act as an external gateway
			_, err := runCommand(containerRuntime, "run", "-itd", "--privileged", "--network", externalContainerNetwork, "--name", gwContainerNameAlt1, agnhostImage)
			if err != nil {
				framework.Failf("failed to start external gateway test container %s: %v", gwContainerNameAlt1, err)
			}
			// retrieve the container ip of the external gateway container
			alt1IPv4, alt1IPv6 := getContainerAddressesForNetwork(gwContainerNameAlt1, externalContainerNetwork)
			if err != nil {
				framework.Failf("failed to start external gateway test container: %v", err)
			}
			nodeIPv4, nodeIPv6 := getContainerAddressesForNetwork(ciWorkerNodeSrc, externalContainerNetwork)

			exGWRemoteCidrAlt1 := fmt.Sprintf("%s/24", exGWRemoteIpAlt1)
			exGWRemoteCidrAlt2 := fmt.Sprintf("%s/24", exGWRemoteIpAlt2)
			exGWIpAlt1 := alt1IPv4
			nodeIP := nodeIPv4
			if isIPv6Cluster {
				exGWIpAlt1 = alt1IPv6
				exGWRemoteCidrAlt1 = fmt.Sprintf("%s/64", exGWRemoteIpAlt1)
				exGWRemoteCidrAlt2 = fmt.Sprintf("%s/64", exGWRemoteIpAlt2)
				nodeIP = nodeIPv6
			}

			// annotate the test namespace
			annotateArgs := []string{
				"annotate",
				"namespace",
				f.Namespace.Name,
				fmt.Sprintf("k8s.ovn.org/routing-external-gws=%s", exGWIpAlt1),
			}
			framework.Logf("Annotating the external gateway test namespace to a container gw: %s ", exGWIpAlt1)
			e2ekubectl.RunKubectlOrDie(f.Namespace.Name, annotateArgs...)

			podCIDR, err := getNodePodCIDR(ciWorkerNodeSrc)
			if err != nil {
				framework.Failf("Error retrieving the pod cidr from %s %v", ciWorkerNodeSrc, err)
			}
			framework.Logf("the pod cidr for node %s is %s", ciWorkerNodeSrc, podCIDR)
			// add loopback interface used to validate all traffic is getting drained through the gateway
			_, err = runCommand(containerRuntime, "exec", gwContainerNameAlt1, "ip", "address", "add", exGWRemoteCidrAlt1, "dev", "lo")
			if err != nil {
				framework.Failf("failed to add the loopback ip to dev lo on the test container: %v", err)
			}
			// Create the pod that will be used as the source for the connectivity test
			createGenericPod(f, srcPingPodName, ciWorkerNodeSrc, f.Namespace.Name, command)
			// wait for pod setup to return a valid address
			err = wait.PollImmediate(retryInterval, retryTimeout, func() (bool, error) {
				pingSrc = getPodAddress(srcPingPodName, f.Namespace.Name)
				validIP = net.ParseIP(pingSrc)
				if validIP == nil {
					return false, nil
				}
				return true, nil
			})
			// Fail the test if no address is ever retrieved
			if err != nil {
				framework.Failf("Error trying to get the pod IP address")
			}
			// add a host route on the first mock gateway for return traffic to the pod
			_, err = runCommand(containerRuntime, "exec", gwContainerNameAlt1, "ip", "route", "add", pingSrc, "via", nodeIP)
			if err != nil {
				framework.Failf("failed to add the pod host route on the test container: %v", err)
			}
			_, err = runCommand(containerRuntime, "exec", gwContainerNameAlt1, "ping", "-c", "5", pingSrc)
			framework.ExpectNoError(err, "Failed to ping %s from container %s", pingSrc, gwContainerNameAlt1)

			time.Sleep(time.Second * 15)
			// Verify the gateway and remote address is reachable from the initial pod
			ginkgo.By(fmt.Sprintf("Verifying connectivity without vxlan to the updated annotation and initial external gateway %s and remote address %s", exGWIpAlt1, exGWRemoteIpAlt1))
			_, err = e2ekubectl.RunKubectl(f.Namespace.Name, "exec", srcPingPodName, testContainerFlag, "--", "ping", "-w", "40", exGWRemoteIpAlt1)
			if err != nil {
				framework.Failf("Failed to ping the first gateway network %s from container %s on node %s: %v", exGWRemoteIpAlt1, ovnContainer, ovnWorkerNode, err)
			}
			// start the container that will act as a new external gateway that the tests will be updated to use
			_, err = runCommand(containerRuntime, "run", "-itd", "--privileged", "--network", externalContainerNetwork, "--name", gwContainerNameAlt2, agnhostImage)
			if err != nil {
				framework.Failf("failed to start external gateway test container %s: %v", gwContainerNameAlt2, err)
			}
			// retrieve the container ip of the external gateway container
			alt2IPv4, alt2IPv6 := getContainerAddressesForNetwork(gwContainerNameAlt2, externalContainerNetwork)
			exGWIpAlt2 := alt2IPv4
			if isIPv6Cluster {
				exGWIpAlt2 = alt2IPv6
			}

			// override the annotation in the test namespace with the new gateway
			annotateArgs = []string{
				"annotate",
				"namespace",
				f.Namespace.Name,
				fmt.Sprintf("k8s.ovn.org/routing-external-gws=%s", exGWIpAlt2),
				"--overwrite",
			}
			framework.Logf("Annotating the external gateway test namespace to a new container remote IP:%s gw:%s ", exGWIpAlt2, exGWRemoteIpAlt2)
			e2ekubectl.RunKubectlOrDie(f.Namespace.Name, annotateArgs...)
			// add loopback interface used to validate all traffic is getting drained through the gateway
			_, err = runCommand(containerRuntime, "exec", gwContainerNameAlt2, "ip", "address", "add", exGWRemoteCidrAlt2, "dev", "lo")
			if err != nil {
				framework.Failf("failed to add the loopback ip to dev lo on the test container: %v", err)
			}
			// add a host route on the second mock gateway for return traffic to the pod
			_, err = runCommand(containerRuntime, "exec", gwContainerNameAlt2, "ip", "route", "add", pingSrc, "via", nodeIP)
			if err != nil {
				framework.Failf("failed to add the pod route on the test container: %v", err)
			}

			_, err = runCommand(containerRuntime, "exec", gwContainerNameAlt2, "ping", "-c", "5", pingSrc)
			framework.ExpectNoError(err, "Failed to ping %s from container %s", pingSrc, gwContainerNameAlt1)

			// Verify the updated gateway and remote address is reachable from the initial pod
			ginkgo.By(fmt.Sprintf("Verifying connectivity without vxlan to the updated annotation and new external gateway %s and remote IP %s", exGWRemoteIpAlt2, exGWIpAlt2))
			_, err = e2ekubectl.RunKubectl(f.Namespace.Name, "exec", srcPingPodName, testContainerFlag, "--", "ping", "-w", "40", exGWRemoteIpAlt2)
			if err != nil {
				framework.Failf("Failed to ping the second gateway network %s from container %s on node %s: %v", exGWRemoteIpAlt2, ovnContainer, ovnWorkerNode, err)
			}
		})
	})

	// This test validates ingress traffic sourced from a mock external gateway
	// running as a container. Add a namespace annotated with the IP of the
	// mock external container's eth0 address. Add a loopback address and a
	// route pointing to the pod in the test namespace. Validate connectivity
	// sourcing from the mock gateway container loopback to the test ns pod.
	var _ = ginkgo.Describe("e2e ingress gateway traffic validation", func() {
		const (
			svcname     string = "novxlan-externalgw-ingress"
			gwContainer string = "gw-ingress-test-container"
		)

		f := wrappedTestFramework(svcname)

		type nodeInfo struct {
			name   string
			nodeIP string
		}

		var (
			workerNodeInfo nodeInfo
			IsIPv6         bool
		)

		ginkgo.BeforeEach(func() {

			// retrieve worker node names
			nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), f.ClientSet, 3)
			framework.ExpectNoError(err)
			if len(nodes.Items) < 3 {
				framework.Failf(
					"Test requires >= 3 Ready nodes, but there are only %v nodes",
					len(nodes.Items))
			}
			ips := e2enode.CollectAddresses(nodes, v1.NodeInternalIP)
			workerNodeInfo = nodeInfo{
				name:   nodes.Items[1].Name,
				nodeIP: ips[1],
			}
			IsIPv6 = IsIPv6Cluster(f.ClientSet)
		})

		ginkgo.AfterEach(func() {
			// tear down the container simulating the gateway
			if cid, _ := runCommand(containerRuntime, "ps", "-qaf", fmt.Sprintf("name=%s", gwContainer)); cid != "" {
				if _, err := runCommand(containerRuntime, "rm", "-f", gwContainer); err != nil {
					framework.Logf("failed to delete the gateway test container %s %v", gwContainer, err)
				}
			}
		})

		ginkgo.It("Should validate ingress connectivity from an external gateway", func() {

			var (
				pingDstPod     string
				dstPingPodName = "e2e-exgw-ingress-ping-pod"
				command        = []string{"bash", "-c", "sleep 20000"}
				exGWLo         = "10.30.1.1"
				exGWLoCidr     = fmt.Sprintf("%s/32", exGWLo)
				pingCmd        = ipv4PingCommand
				pingCount      = "3"
			)
			if IsIPv6 {
				exGWLo = "fc00::1" // unique local ipv6 unicast addr as per rfc4193
				exGWLoCidr = fmt.Sprintf("%s/64", exGWLo)
				pingCmd = ipv6PingCommand
			}

			// start the first container that will act as an external gateway
			_, err := runCommand(containerRuntime, "run", "-itd", "--privileged", "--network", externalContainerNetwork,
				"--name", gwContainer, agnhostImage)
			if err != nil {
				framework.Failf("failed to start external gateway test container %s: %v", gwContainer, err)
			}
			exGWIp, exGWIpv6 := getContainerAddressesForNetwork(gwContainer, externalContainerNetwork)
			if IsIPv6 {
				exGWIp = exGWIpv6
			}
			// annotate the test namespace with the external gateway address
			annotateArgs := []string{
				"annotate",
				"namespace",
				f.Namespace.Name,
				fmt.Sprintf("k8s.ovn.org/routing-external-gws=%s", exGWIp),
			}
			framework.Logf("Annotating the external gateway test namespace to container gateway: %s", exGWIp)
			e2ekubectl.RunKubectlOrDie(f.Namespace.Name, annotateArgs...)

			nodeIP, nodeIPv6 := getContainerAddressesForNetwork(workerNodeInfo.name, externalContainerNetwork)
			if IsIPv6 {
				nodeIP = nodeIPv6
			}
			framework.Logf("the pod side node is %s and the source node ip is %s", workerNodeInfo.name, nodeIP)
			podCIDR, err := getNodePodCIDR(workerNodeInfo.name)
			if err != nil {
				framework.Failf("Error retrieving the pod cidr from %s %v", workerNodeInfo.name, err)
			}
			framework.Logf("the pod cidr for node %s is %s", workerNodeInfo.name, podCIDR)

			// Create the pod that will be used as the source for the connectivity test
			createGenericPod(f, dstPingPodName, workerNodeInfo.name, f.Namespace.Name, command)
			// wait for the pod setup to return a valid address
			err = wait.PollImmediate(retryInterval, retryTimeout, func() (bool, error) {
				pingDstPod = getPodAddress(dstPingPodName, f.Namespace.Name)
				validIP := net.ParseIP(pingDstPod)
				if validIP == nil {
					return false, nil
				}
				return true, nil
			})
			// fail the test if a pod address is never retrieved
			if err != nil {
				framework.Failf("Error trying to get the pod IP address")
			}
			// add a host route on the gateway for return traffic to the pod
			_, err = runCommand(containerRuntime, "exec", gwContainer, "ip", "route", "add", pingDstPod, "via", nodeIP)
			if err != nil {
				framework.Failf("failed to add the pod host route on the test container %s: %v", gwContainer, err)
			}
			// add a loopback address to the mock container that will source the ingress test
			_, err = runCommand(containerRuntime, "exec", gwContainer, "ip", "address", "add", exGWLoCidr, "dev", "lo")
			if err != nil {
				framework.Failf("failed to add the loopback ip to dev lo on the test container: %v", err)
			}

			// Validate connectivity from the external gateway loopback to the pod in the test namespace
			ginkgo.By(fmt.Sprintf("Validate ingress traffic from the external gateway %s can reach the pod in the exgw annotated namespace", gwContainer))
			// generate traffic that will verify connectivity from the mock external gateway loopback
			_, err = runCommand(containerRuntime, "exec", gwContainer, string(pingCmd), "-c", pingCount, "-I", "eth0", pingDstPod)
			if err != nil {
				framework.Failf("failed to ping the pod address %s from mock container %s: %v", pingDstPod, gwContainer, err)
			}
		})
	})

	var _ = ginkgo.Context("With annotations", func() {

		// Validate pods can reach a network running in a container's looback address via
		// an external gateway running on eth0 of the container without any tunnel encap.
		// The traffic will get proxied through an annotated pod in the serving namespace.
		var _ = ginkgo.Describe("e2e non-vxlan external gateway through a gateway pod", func() {
			const (
				svcname         string = "externalgw-pod-novxlan"
				gwContainer1    string = "ex-gw-container1"
				gwContainer2    string = "ex-gw-container2"
				srcPingPodName  string = "e2e-exgw-src-ping-pod"
				gatewayPodName1 string = "e2e-gateway-pod1"
				gatewayPodName2 string = "e2e-gateway-pod2"
				externalTCPPort        = 91
				externalUDPPort        = 90
				ecmpRetry       int    = 20
				testTimeout     string = "20"
			)

			var (
				sleepCommand             = []string{"bash", "-c", "sleep 20000"}
				addressesv4, addressesv6 gatewayTestIPs
				clientSet                kubernetes.Interface
				servingNamespace         string
			)

			var (
				gwContainers []string
			)

			f := wrappedTestFramework(svcname)

			ginkgo.BeforeEach(func() {
				clientSet = f.ClientSet // so it can be used in AfterEach
				// retrieve worker node names
				nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), f.ClientSet, 3)
				framework.ExpectNoError(err)
				if len(nodes.Items) < 3 {
					framework.Failf(
						"Test requires >= 3 Ready nodes, but there are only %v nodes",
						len(nodes.Items))
				}

				ns, err := f.CreateNamespace(context.TODO(), "exgw-serving", nil)
				framework.ExpectNoError(err)
				servingNamespace = ns.Name

				gwContainers, addressesv4, addressesv6 = setupGatewayContainers(f, nodes, gwContainer1, gwContainer2, srcPingPodName, externalUDPPort, externalTCPPort, ecmpRetry)
				setupAnnotatedGatewayPods(f, nodes, gatewayPodName1, gatewayPodName2, servingNamespace, sleepCommand, addressesv4, addressesv6, false)
			})

			ginkgo.AfterEach(func() {
				cleanExGWContainers(clientSet, []string{gwContainer1, gwContainer2}, addressesv4, addressesv6)
				resetGatewayAnnotations(f)
			})

			ginkgo.DescribeTable("Should validate ICMP connectivity to an external gateway's loopback address via a pod with external gateway CR",
				func(addresses *gatewayTestIPs, icmpCommand string) {
					if addresses.srcPodIP == "" || addresses.nodeIP == "" {
						skipper.Skipf("Skipping as pod ip / node ip are not set pod ip %s node ip %s", addresses.srcPodIP, addresses.nodeIP)
					}

					ginkgo.By(fmt.Sprintf("Verifying connectivity to the pod [%s] from external gateways", addresses.srcPodIP))
					for _, gwContainer := range gwContainers {
						// Ping from a common IP address that exists on both gateways to ensure test coverage where ingress reply goes back to the same host.
						_, err := runCommand(containerRuntime, "exec", gwContainer, "ping", "-I", addresses.targetIPs[0], "-c", testTimeout, addresses.srcPodIP)
						framework.ExpectNoError(err, "Failed to ping %s from container %s", addresses.srcPodIP, gwContainer)
					}

					tcpDumpSync := sync.WaitGroup{}
					tcpDumpSync.Add(len(gwContainers))

					for _, gwContainer := range gwContainers {
						go checkReceivedPacketsOnContainer(gwContainer, srcPingPodName, anyLink, []string{icmpCommand}, &tcpDumpSync)
					}

					pingSync := sync.WaitGroup{}
					// Verify the external gateway loopback address running on the external container is reachable and
					// that traffic from the source ping pod is proxied through the pod in the serving namespace
					ginkgo.By("Verifying connectivity via the gateway namespace to the remote addresses")
					for _, t := range addresses.targetIPs {
						pingSync.Add(1)
						go func(target string) {
							defer ginkgo.GinkgoRecover()
							defer pingSync.Done()
							_, err := e2ekubectl.RunKubectl(f.Namespace.Name, "exec", srcPingPodName, "--", "ping", "-c", testTimeout, target)
							framework.ExpectNoError(err, "Failed to ping remote gateway %s from pod %s", target, srcPingPodName)
						}(t)
					}
					pingSync.Wait()
					tcpDumpSync.Wait()
				},
				ginkgo.Entry("ipv4", &addressesv4, "icmp"),
				ginkgo.Entry("ipv6", &addressesv6, "icmp6"))

			ginkgo.DescribeTable("Should validate TCP/UDP connectivity to an external gateway's loopback address via a pod with external gateway annotations enabled",
				func(protocol string, addresses *gatewayTestIPs, destPort, destPortOnPod int) {
					if addresses.srcPodIP == "" || addresses.nodeIP == "" {
						skipper.Skipf("Skipping as pod ip / node ip are not set pod ip %s node ip %s", addresses.srcPodIP, addresses.nodeIP)
					}

					for _, container := range gwContainers {
						reachPodFromGateway(addresses.srcPodIP, strconv.Itoa(destPortOnPod), srcPingPodName, container, protocol)
					}

					expectedHostNames := make(map[string]struct{})
					for _, c := range gwContainers {
						res, err := runCommand(containerRuntime, "exec", c, "hostname")
						framework.ExpectNoError(err, "failed to run hostname in %s", c)
						hostname := strings.TrimSuffix(res, "\n")
						framework.Logf("Hostname for %s is %s", c, hostname)
						expectedHostNames[hostname] = struct{}{}
					}
					framework.Logf("Expected hostnames are %v", expectedHostNames)

					ginkgo.By("Checking that external ips are reachable with both gateways")
					returnedHostNames := make(map[string]struct{})
					target := addresses.targetIPs[0]
					success := false
					for i := 0; i < 20; i++ {
						args := []string{"exec", srcPingPodName, "--"}
						if protocol == "tcp" {
							args = append(args, "bash", "-c", fmt.Sprintf("echo | nc -w 1 %s %d", target, destPort))
						} else {
							args = append(args, "bash", "-c", fmt.Sprintf("echo | nc -w 1 -u %s %d", target, destPort))
						}
						res, err := e2ekubectl.RunKubectl(f.Namespace.Name, args...)
						framework.ExpectNoError(err, "failed to reach %s (%s)", target, protocol)
						hostname := strings.TrimSuffix(res, "\n")
						if hostname != "" {
							returnedHostNames[hostname] = struct{}{}
						}

						if cmp.Equal(returnedHostNames, expectedHostNames) {
							success = true
							break
						}
					}
					framework.Logf("Received hostnames for protocol %s are %v ", protocol, returnedHostNames)

					if !success {
						framework.Failf("Failed to hit all the external gateways via for protocol %s, diff %s", protocol, cmp.Diff(expectedHostNames, returnedHostNames))
					}

				},
				ginkgo.Entry("UDP ipv4", "udp", &addressesv4, externalUDPPort, srcUDPPort),
				ginkgo.Entry("TCP ipv4", "tcp", &addressesv4, externalTCPPort, srcHTTPPort),
				ginkgo.Entry("UDP ipv6", "udp", &addressesv6, externalUDPPort, srcUDPPort),
				ginkgo.Entry("TCP ipv6", "tcp", &addressesv6, externalTCPPort, srcHTTPPort))
		})

		// Validate pods can reach a network running in multiple container's loopback
		// addresses via two external gateways running on eth0 of the container without
		// any tunnel encap. This test defines two external gateways and validates ECMP
		// functionality to the container loopbacks. To verify traffic reaches the
		// gateways, tcpdump is running on the external gateways and will exit successfully
		// once an ICMP packet is received from the annotated pod in the k8s cluster.
		// Two additional gateways are added to verify the tcp / udp protocols.
		// They run the netexec command, and the pod asks to return their hostname.
		// The test checks that both hostnames are collected at least once.
		var _ = ginkgo.Describe("e2e multiple external gateway validation", func() {
			const (
				svcname         string = "novxlan-externalgw-ecmp"
				gwContainer1    string = "gw-test-container1"
				gwContainer2    string = "gw-test-container2"
				testTimeout     string = "30"
				ecmpRetry       int    = 20
				srcPodName             = "e2e-exgw-src-pod"
				externalTCPPort        = 80
				externalUDPPort        = 90
			)

			f := wrappedTestFramework(svcname)

			var gwContainers []string
			var addressesv4, addressesv6 gatewayTestIPs

			ginkgo.BeforeEach(func() {
				// retrieve worker node names
				nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), f.ClientSet, 3)
				framework.ExpectNoError(err)
				if len(nodes.Items) < 3 {
					framework.Failf(
						"Test requires >= 3 Ready nodes, but there are only %v nodes",
						len(nodes.Items))
				}

				if externalContainerNetwork == "host" {
					skipper.Skipf("Skipping as host network doesn't support multiple external gateways")
				}

				gwContainers, addressesv4, addressesv6 = setupGatewayContainers(f, nodes, gwContainer1, gwContainer2, srcPodName, externalUDPPort, externalTCPPort, ecmpRetry)

			})

			ginkgo.AfterEach(func() {
				// tear down the containers simulating the gateways
				deleteClusterExternalContainer(gwContainer1)
				deleteClusterExternalContainer(gwContainer2)
				resetGatewayAnnotations(f)
			})

			ginkgo.DescribeTable("Should validate ICMP connectivity to multiple external gateways for an ECMP scenario", func(addresses *gatewayTestIPs, icmpToDump string) {
				if addresses.srcPodIP == "" || addresses.nodeIP == "" {
					skipper.Skipf("Skipping as pod ip / node ip are not set pod ip %s node ip %s", addresses.srcPodIP, addresses.nodeIP)
				}

				annotateNamespaceForGateway(f.Namespace.Name, false, addresses.gatewayIPs[:]...)

				ginkgo.By("Verifying connectivity to the pod from external gateways")
				for _, gwContainer := range gwContainers {
					// Ping from a common IP address that exists on both gateways to ensure test coverage where ingress reply goes back to the same host.
					_, err := runCommand(containerRuntime, "exec", gwContainer, "ping", "-I", addresses.targetIPs[0], "-c", testTimeout, addresses.srcPodIP)
					framework.ExpectNoError(err, "Failed to ping %s from container %s", addresses.srcPodIP, gwContainer)
				}

				ginkgo.By("Verifying connectivity to the pod from external gateways with large packets > pod MTU")
				for _, gwContainer := range gwContainers {
					_, err := runCommand(containerRuntime, "exec", gwContainer, "ping", "-s", "1420", "-c", testTimeout, addresses.srcPodIP)
					framework.ExpectNoError(err, "Failed to ping %s from container %s", addresses.srcPodIP, gwContainer)
				}

				// Verify the gateways and remote loopback addresses are reachable from the pod.
				// Iterate checking connectivity to the loopbacks on the gateways until tcpdump see
				// the traffic or 20 attempts fail. Odds of a false negative here is ~ (1/2)^20
				ginkgo.By("Verifying ecmp connectivity to the external gateways by iterating through the targets")

				// Check for egress traffic to both gateway loopback addresses using tcpdump, since
				// /proc/net/dev counters only record the ingress interface traffic is received on.
				// The test will waits until an ICMP packet is matched on the gateways or fail the
				// test if a packet to the loopback is not received within the timer interval.
				// If an ICMP packet is never detected, return the error via the specified chanel.

				tcpDumpSync := sync.WaitGroup{}
				tcpDumpSync.Add(len(gwContainers))
				for _, gwContainer := range gwContainers {
					go checkReceivedPacketsOnContainer(gwContainer, srcPodName, anyLink, []string{icmpToDump}, &tcpDumpSync)
				}

				pingSync := sync.WaitGroup{}

				// spawn a goroutine to asynchronously (to speed up the test)
				// to ping the gateway loopbacks on both containers via ECMP.
				for _, address := range addresses.targetIPs {
					pingSync.Add(1)
					go func(target string) {
						defer ginkgo.GinkgoRecover()
						defer pingSync.Done()
						_, err := e2ekubectl.RunKubectl(f.Namespace.Name, "exec", srcPodName, "--", "ping", "-c", testTimeout, target)
						if err != nil {
							framework.Logf("error generating a ping from the test pod %s: %v", srcPodName, err)
						}
					}(address)
				}
				pingSync.Wait()
				tcpDumpSync.Wait()

			}, ginkgo.Entry("IPV4", &addressesv4, "icmp"),
				ginkgo.Entry("IPV6", &addressesv6, "icmp6"))

			// This test runs a listener on the external container, returning the host name both on tcp and udp.
			// The src pod tries to hit the remote address until both the containers are hit.
			ginkgo.DescribeTable("Should validate TCP/UDP connectivity to multiple external gateways for a UDP / TCP scenario", func(addresses *gatewayTestIPs, protocol string, destPort, destPortOnPod int) {
				if addresses.srcPodIP == "" || addresses.nodeIP == "" {
					skipper.Skipf("Skipping as pod ip / node ip are not set pod ip %s node ip %s", addresses.srcPodIP, addresses.nodeIP)
				}

				annotateNamespaceForGateway(f.Namespace.Name, false, addresses.gatewayIPs[:]...)

				for _, container := range gwContainers {
					reachPodFromGateway(addresses.srcPodIP, strconv.Itoa(destPortOnPod), srcPodName, container, protocol)
				}

				expectedHostNames := hostNamesForContainers(gwContainers)
				framework.Logf("Expected hostnames are %v", expectedHostNames)

				returnedHostNames := make(map[string]struct{})
				success := false

				// Picking only the first address, the one the udp listener is set for
				target := addresses.targetIPs[0]
				for i := 0; i < 20; i++ {
					hostname := pokeHostnameViaNC(srcPodName, f.Namespace.Name, protocol, target, destPort)
					if hostname != "" {
						returnedHostNames[hostname] = struct{}{}
					}
					if cmp.Equal(returnedHostNames, expectedHostNames) {
						success = true
						break
					}
				}

				framework.Logf("Received hostnames for protocol %s are %v ", protocol, returnedHostNames)

				if !success {
					framework.Failf("Failed to hit all the external gateways via for protocol %s, diff %s", protocol, cmp.Diff(expectedHostNames, returnedHostNames))
				}

			}, ginkgo.Entry("IPV4 udp", &addressesv4, "udp", externalUDPPort, srcUDPPort),
				ginkgo.Entry("IPV4 tcp", &addressesv4, "tcp", externalTCPPort, srcHTTPPort),
				ginkgo.Entry("IPV6 udp", &addressesv6, "udp", externalUDPPort, srcUDPPort),
				ginkgo.Entry("IPV6 tcp", &addressesv6, "tcp", externalTCPPort, srcHTTPPort))
		})

		var _ = ginkgo.Describe("e2e multiple external gateway stale conntrack entry deletion validation", func() {
			const (
				svcname         string = "novxlan-externalgw-ecmp"
				gwContainer1    string = "gw-test-container1"
				gwContainer2    string = "gw-test-container2"
				srcPodName      string = "e2e-exgw-src-pod"
				gatewayPodName1 string = "e2e-gateway-pod1"
				gatewayPodName2 string = "e2e-gateway-pod2"
			)

			f := wrappedTestFramework(svcname)

			var (
				addressesv4, addressesv6 gatewayTestIPs
				sleepCommand             []string
				nodes                    *v1.NodeList
				err                      error
				clientSet                kubernetes.Interface
				servingNamespace         string
			)

			ginkgo.BeforeEach(func() {
				clientSet = f.ClientSet // so it can be used in AfterEach
				// retrieve worker node names
				nodes, err = e2enode.GetBoundedReadySchedulableNodes(context.TODO(), clientSet, 3)
				framework.ExpectNoError(err)
				if len(nodes.Items) < 3 {
					framework.Failf(
						"Test requires >= 3 Ready nodes, but there are only %v nodes",
						len(nodes.Items))
				}

				if externalContainerNetwork == "host" {
					skipper.Skipf("Skipping as host network doesn't support multiple external gateways")
				}

				ns, err := f.CreateNamespace(context.TODO(), "exgw-conntrack-serving", nil)
				framework.ExpectNoError(err)
				servingNamespace = ns.Name

				addressesv4, addressesv6 = setupGatewayContainersForConntrackTest(f, nodes, gwContainer1, gwContainer2, srcPodName)
				sleepCommand = []string{"bash", "-c", "trap : TERM INT; sleep infinity & wait"}
				_, err = createGenericPod(f, gatewayPodName1, nodes.Items[0].Name, servingNamespace, sleepCommand)
				framework.ExpectNoError(err, "Create and annotate the external gw pods to manage the src app pod namespace, failed: %v", err)
				_, err = createGenericPod(f, gatewayPodName2, nodes.Items[1].Name, servingNamespace, sleepCommand)
				framework.ExpectNoError(err, "Create and annotate the external gw pods to manage the src app pod namespace, failed: %v", err)
			})

			ginkgo.AfterEach(func() {
				// tear down the containers and pods simulating the gateways
				ginkgo.By("Deleting the gateway containers")
				deleteClusterExternalContainer(gwContainer1)
				deleteClusterExternalContainer(gwContainer2)
				resetGatewayAnnotations(f)
			})

			ginkgo.DescribeTable("Namespace annotation: Should validate conntrack entry deletion for TCP/UDP traffic via multiple external gateways a.k.a ECMP routes", func(addresses *gatewayTestIPs, protocol string) {
				if addresses.srcPodIP == "" || addresses.nodeIP == "" {
					skipper.Skipf("Skipping as pod ip / node ip are not set pod ip %s node ip %s", addresses.srcPodIP, addresses.nodeIP)
				}
				ginkgo.By("Annotate the app namespace to get managed by external gateways")
				annotateNamespaceForGateway(f.Namespace.Name, false, addresses.gatewayIPs...)

				setupIperf3Client := func(container, address string, port int) {
					// note iperf3 even when using udp also spawns tcp connection first; so we indirectly also have the tcp connection when using "-u" flag
					cmd := []string{containerRuntime, "exec", container, "iperf3", "-u", "-c", address, "-p", fmt.Sprintf("%d", port), "-b", "1M", "-i", "1", "-t", "3", "&"}
					_, err := runCommand(cmd...)
					framework.ExpectNoError(err, "failed to setup iperf3 client for %s", container)
				}
				macAddressGW := make([]string, 2)
				for i, containerName := range []string{gwContainer1, gwContainer2} {
					ginkgo.By("Start iperf3 client from external container to connect to iperf3 server running at the src pod")
					setupIperf3Client(containerName, addresses.srcPodIP, 5201+i)
					macAddressExtGW, err := net.ParseMAC(getMACAddressesForNetwork(containerName, externalContainerNetwork))
					framework.ExpectNoError(err, "failed to parse MAC address for %s", containerName)
					// Trim leading 0s because conntrack dumped labels are just integers
					// in hex without leading 0s.
					macAddressGW[i] = strings.TrimLeft(strings.Replace(macAddressExtGW.String(), ":", "", -1), "0")
				}

				ginkgo.By("Check if conntrack entries for ECMP routes are created for the 2 external gateways")
				nodeName := getPod(f, srcPodName).Spec.NodeName
				podConnEntriesWithMACLabelsSet := pokeConntrackEntries(nodeName, addresses.srcPodIP, protocol, macAddressGW)
				gomega.Expect(podConnEntriesWithMACLabelsSet).To(gomega.Equal(2))
				totalPodConnEntries := pokeConntrackEntries(nodeName, addresses.srcPodIP, protocol, nil)
				gomega.Expect(totalPodConnEntries).To(gomega.Equal(6)) // total conntrack entries for this pod/protocol

				ginkgo.By("Remove second external gateway IP from the app namespace annotation")
				annotateNamespaceForGateway(f.Namespace.Name, false, addresses.gatewayIPs[0])

				ginkgo.By("Check if conntrack entries for ECMP routes are removed for the deleted external gateway if traffic is UDP")
				podConnEntriesWithMACLabelsSet = pokeConntrackEntries(nodeName, addresses.srcPodIP, protocol, macAddressGW)
				totalPodConnEntries = pokeConntrackEntries(nodeName, addresses.srcPodIP, protocol, nil)

				gomega.Expect(podConnEntriesWithMACLabelsSet).To(gomega.Equal(1)) // we still have the conntrack entry for the remaining gateway
				gomega.Expect(totalPodConnEntries).To(gomega.Equal(5))            // 6-1

				ginkgo.By("Remove first external gateway IP from the app namespace annotation")
				annotateNamespaceForGateway(f.Namespace.Name, false, "")

				ginkgo.By("Check if conntrack entries for ECMP routes are removed for the deleted external gateway if traffic is UDP")
				podConnEntriesWithMACLabelsSet = pokeConntrackEntries(nodeName, addresses.srcPodIP, protocol, macAddressGW)
				totalPodConnEntries = pokeConntrackEntries(nodeName, addresses.srcPodIP, protocol, nil)

				gomega.Expect(podConnEntriesWithMACLabelsSet).To(gomega.Equal(0)) // we don't have any remaining gateways left
				gomega.Expect(totalPodConnEntries).To(gomega.Equal(4))            // 6-2

			},
				ginkgo.Entry("IPV4 udp", &addressesv4, "udp"),
				ginkgo.Entry("IPV4 tcp", &addressesv4, "tcp"),
				ginkgo.Entry("IPV6 udp", &addressesv6, "udp"),
				ginkgo.Entry("IPV6 tcp", &addressesv6, "tcp"))

			ginkgo.DescribeTable("ExternalGWPod annotation: Should validate conntrack entry deletion for TCP/UDP traffic via multiple external gateways a.k.a ECMP routes", func(addresses *gatewayTestIPs, protocol string, deletePod bool) {
				if addresses.srcPodIP == "" || addresses.nodeIP == "" {
					skipper.Skipf("Skipping as pod ip / node ip are not set pod ip %s node ip %s", addresses.srcPodIP, addresses.nodeIP)
				}
				ginkgo.By("Annotate the external gw pods to manage the src app pod namespace")
				for i, gwPod := range []string{gatewayPodName1, gatewayPodName2} {
					networkIPs := fmt.Sprintf("\"%s\"", addresses.gatewayIPs[i])
					if addresses.srcPodIP != "" && addresses.nodeIP != "" {
						networkIPs = fmt.Sprintf("\"%s\", \"%s\"", addresses.gatewayIPs[i], addresses.gatewayIPs[i])
					}
					annotatePodForGateway(gwPod, servingNamespace, f.Namespace.Name, networkIPs, false)
				}

				// ensure the conntrack deletion tracker annotation is updated
				if !isInterconnectEnabled() {
					ginkgo.By("Check if the k8s.ovn.org/external-gw-pod-ips got updated for the app namespace")
					err := wait.PollImmediate(retryInterval, retryTimeout, func() (bool, error) {
						ns := getNamespace(f, f.Namespace.Name)
						return (ns.Annotations[externalGatewayPodIPsAnnotation] == fmt.Sprintf("%s,%s", addresses.gatewayIPs[0], addresses.gatewayIPs[1])), nil
					})
					framework.ExpectNoError(err, "Check if the k8s.ovn.org/external-gw-pod-ips got updated, failed: %v", err)
				}

				setupIperf3Client := func(container, address string, port int) {
					// note iperf3 even when using udp also spawns tcp connection first; so we indirectly also have the tcp connection when using "-u" flag
					cmd := []string{containerRuntime, "exec", container, "iperf3", "-u", "-c", address, "-p", fmt.Sprintf("%d", port), "-b", "1M", "-i", "1", "-t", "3", "&"}
					_, err := runCommand(cmd...)
					framework.ExpectNoError(err, "failed to setup iperf3 client for %s", container)
				}
				macAddressGW := make([]string, 2)
				for i, containerName := range []string{gwContainer1, gwContainer2} {
					ginkgo.By("Start iperf3 client from external container to connect to iperf3 server running at the src pod")
					setupIperf3Client(containerName, addresses.srcPodIP, 5201+i)
					macAddressExtGW, err := net.ParseMAC(getMACAddressesForNetwork(containerName, externalContainerNetwork))
					framework.ExpectNoError(err, "failed to parse MAC address for %s", containerName)
					// Trim leading 0s because conntrack dumped labels are just integers
					// in hex without leading 0s.
					macAddressGW[i] = strings.TrimLeft(strings.Replace(macAddressExtGW.String(), ":", "", -1), "0")
				}

				ginkgo.By("Check if conntrack entries for ECMP routes are created for the 2 external gateways")
				nodeName := getPod(f, srcPodName).Spec.NodeName
				podConnEntriesWithMACLabelsSet := pokeConntrackEntries(nodeName, addresses.srcPodIP, protocol, macAddressGW)
				gomega.Expect(podConnEntriesWithMACLabelsSet).To(gomega.Equal(2))
				totalPodConnEntries := pokeConntrackEntries(nodeName, addresses.srcPodIP, protocol, nil)
				gomega.Expect(totalPodConnEntries).To(gomega.Equal(6)) // total conntrack entries for this pod/protocol

				if deletePod {
					ginkgo.By(fmt.Sprintf("Delete second external gateway pod %s from ns %s", gatewayPodName2, servingNamespace))
					err = f.ClientSet.CoreV1().Pods(servingNamespace).Delete(context.TODO(), gatewayPodName2, metav1.DeleteOptions{})
					framework.ExpectNoError(err, "Delete the gateway pod failed: %v", err)
					// give some time to handle pod delete event
					time.Sleep(5 * time.Second)
				} else {
					ginkgo.By("Remove second external gateway pod's routing-namespace annotation")
					annotatePodForGateway(gatewayPodName2, servingNamespace, "", addresses.gatewayIPs[1], false)
				}

				// ensure the conntrack deletion tracker annotation is updated
				if !isInterconnectEnabled() {
					ginkgo.By("Check if the k8s.ovn.org/external-gw-pod-ips got updated for the app namespace")
					err = wait.PollImmediate(retryInterval, retryTimeout, func() (bool, error) {
						ns := getNamespace(f, f.Namespace.Name)
						return (ns.Annotations[externalGatewayPodIPsAnnotation] == addresses.gatewayIPs[0]), nil
					})
					framework.ExpectNoError(err, "Check if the k8s.ovn.org/external-gw-pod-ips got updated, failed: %v", err)
				}

				ginkgo.By("Check if conntrack entries for ECMP routes are removed for the deleted external gateway if traffic is UDP")
				podConnEntriesWithMACLabelsSet = pokeConntrackEntries(nodeName, addresses.srcPodIP, protocol, macAddressGW)
				totalPodConnEntries = pokeConntrackEntries(nodeName, addresses.srcPodIP, protocol, nil)

				gomega.Expect(podConnEntriesWithMACLabelsSet).To(gomega.Equal(1)) // we still have the conntrack entry for the remaining gateway
				gomega.Expect(totalPodConnEntries).To(gomega.Equal(5))            // 6-1

				ginkgo.By("Remove first external gateway pod's routing-namespace annotation")
				annotatePodForGateway(gatewayPodName1, servingNamespace, "", addresses.gatewayIPs[0], false)

				// ensure the conntrack deletion tracker annotation is updated
				if !isInterconnectEnabled() {
					ginkgo.By("Check if the k8s.ovn.org/external-gw-pod-ips got updated for the app namespace")
					err = wait.PollImmediate(retryInterval, retryTimeout, func() (bool, error) {
						ns := getNamespace(f, f.Namespace.Name)
						return (ns.Annotations[externalGatewayPodIPsAnnotation] == ""), nil
					})
					framework.ExpectNoError(err, "Check if the k8s.ovn.org/external-gw-pod-ips got updated, failed: %v", err)
				}

				ginkgo.By("Check if conntrack entries for ECMP routes are removed for the deleted external gateway if traffic is UDP")
				podConnEntriesWithMACLabelsSet = pokeConntrackEntries(nodeName, addresses.srcPodIP, protocol, macAddressGW)
				totalPodConnEntries = pokeConntrackEntries(nodeName, addresses.srcPodIP, protocol, nil)

				gomega.Expect(podConnEntriesWithMACLabelsSet).To(gomega.Equal(0)) // we don't have any remaining gateways left
				gomega.Expect(totalPodConnEntries).To(gomega.Equal(4))            // 6-2
			},
				ginkgo.Entry("IPV4 udp", &addressesv4, "udp", false),
				ginkgo.Entry("IPV4 tcp", &addressesv4, "tcp", false),
				ginkgo.Entry("IPV6 udp", &addressesv6, "udp", false),
				ginkgo.Entry("IPV6 tcp", &addressesv6, "tcp", false),
				ginkgo.Entry("IPV4 udp + pod delete", &addressesv4, "udp", true),
				ginkgo.Entry("IPV6 tcp + pod delete", &addressesv6, "tcp", true),
			)
		})

		// BFD Tests are dual of external gateway. The only difference is that they enable BFD on ovn and
		// on the external containers, and after doing one round veryfing that the traffic reaches both containers,
		// they delete one and verify that the traffic is always reaching the only alive container.
		var _ = ginkgo.Context("BFD", func() {
			var _ = ginkgo.Describe("e2e non-vxlan external gateway through an annotated gateway pod", func() {
				const (
					svcname           string = "externalgw-pod-novxlan"
					gwContainer1      string = "ex-gw-container1"
					gwContainer2      string = "ex-gw-container2"
					srcPingPodName    string = "e2e-exgw-src-ping-pod"
					gatewayPodName1   string = "e2e-gateway-pod1"
					gatewayPodName2   string = "e2e-gateway-pod2"
					externalTCPPort          = 91
					externalUDPPort          = 90
					ecmpRetry         int    = 20
					testTimeout       string = "20"
					defaultPolicyName        = "default-route-policy"
				)

				var (
					sleepCommand             = []string{"bash", "-c", "sleep 20000"}
					addressesv4, addressesv6 gatewayTestIPs
					clientSet                kubernetes.Interface
					servingNamespace         string
				)

				var (
					gwContainers []string
				)

				f := wrappedTestFramework(svcname)

				ginkgo.BeforeEach(func() {
					clientSet = f.ClientSet // so it can be used in AfterEach
					// retrieve worker node names
					nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), f.ClientSet, 3)
					framework.ExpectNoError(err)
					if len(nodes.Items) < 3 {
						framework.Failf(
							"Test requires >= 3 Ready nodes, but there are only %v nodes",
							len(nodes.Items))
					}

					ns, err := f.CreateNamespace(context.TODO(), "exgw-bfd-serving", nil)
					framework.ExpectNoError(err)
					servingNamespace = ns.Name

					setupBFD := setupBFDOnContainer(nodes.Items)
					gwContainers, addressesv4, addressesv6 = setupGatewayContainers(f, nodes, gwContainer1, gwContainer2, srcPingPodName, externalUDPPort, externalTCPPort, ecmpRetry, setupBFD)
					setupAnnotatedGatewayPods(f, nodes, gatewayPodName1, gatewayPodName2, servingNamespace, sleepCommand, addressesv4, addressesv6, true)
				})

				ginkgo.AfterEach(func() {
					cleanExGWContainers(clientSet, []string{gwContainer1, gwContainer2}, addressesv4, addressesv6)
					resetGatewayAnnotations(f)
				})

				ginkgo.DescribeTable("Should validate ICMP connectivity to an external gateway's loopback address via a pod with external gateway annotations enabled",
					func(addresses *gatewayTestIPs, icmpCommand string) {
						if addresses.srcPodIP == "" || addresses.nodeIP == "" {
							skipper.Skipf("Skipping as pod ip / node ip are not set pod ip %s node ip %s", addresses.srcPodIP, addresses.nodeIP)
						}

						ginkgo.By("Verifying connectivity to the pod from external gateways")
						for _, gwContainer := range gwContainers {
							// Ping from a common IP address that exists on both gateways to ensure test coverage where ingress reply goes back to the same host.
							_, err := runCommand(containerRuntime, "exec", gwContainer, "ping", "-I", addresses.targetIPs[0], "-c", testTimeout, addresses.srcPodIP)
							framework.ExpectNoError(err, "Failed to ping %s from container %s", addresses.srcPodIP, gwContainer)
						}

						// This is needed for bfd to sync up
						time.Sleep(3 * time.Second)

						for _, gwContainer := range gwContainers {
							gomega.Expect(isBFDPaired(gwContainer, addresses.nodeIP)).To(gomega.Equal(true), "Bfd not paired")
						}

						tcpDumpSync := sync.WaitGroup{}
						tcpDumpSync.Add(len(gwContainers))
						for _, gwContainer := range gwContainers {
							go checkReceivedPacketsOnContainer(gwContainer, srcPingPodName, anyLink, []string{icmpCommand}, &tcpDumpSync)
						}

						// Verify the external gateway loopback address running on the external container is reachable and
						// that traffic from the source ping pod is proxied through the pod in the serving namespace
						ginkgo.By("Verifying connectivity via the gateway namespace to the remote addresses")

						pingSync := sync.WaitGroup{}
						// spawn a goroutine to asynchronously (to speed up the test)
						// to ping the gateway loopbacks on both containers via ECMP.
						for _, address := range addresses.targetIPs {
							pingSync.Add(1)
							go func(target string) {
								defer ginkgo.GinkgoRecover()
								defer pingSync.Done()
								_, err := e2ekubectl.RunKubectl(f.Namespace.Name, "exec", srcPingPodName, "--", "ping", "-c", testTimeout, target)
								if err != nil {
									framework.Logf("error generating a ping from the test pod %s: %v", srcPingPodName, err)
								}
							}(address)
						}

						pingSync.Wait()
						tcpDumpSync.Wait()

						if len(gwContainers) > 1 {
							ginkgo.By("Deleting one container")
							deleteClusterExternalContainer(gwContainers[1])
							time.Sleep(3 * time.Second) // bfd timeout

							tcpDumpSync = sync.WaitGroup{}
							tcpDumpSync.Add(1)
							go checkReceivedPacketsOnContainer(gwContainers[0], srcPingPodName, anyLink, []string{icmpCommand}, &tcpDumpSync)

							// Verify the external gateway loopback address running on the external container is reachable and
							// that traffic from the source ping pod is proxied through the pod in the serving namespace
							ginkgo.By("Verifying connectivity via the gateway namespace to the remote addresses")
							pingSync = sync.WaitGroup{}

							for _, t := range addresses.targetIPs {
								pingSync.Add(1)
								go func(target string) {
									defer ginkgo.GinkgoRecover()
									defer pingSync.Done()
									_, err := e2ekubectl.RunKubectl(f.Namespace.Name, "exec", srcPingPodName, "--", "ping", "-c", testTimeout, target)
									framework.ExpectNoError(err, "Failed to ping remote gateway %s from pod %s", target, srcPingPodName)
								}(t)
							}
							pingSync.Wait()
							tcpDumpSync.Wait()
						}
					},
					ginkgo.Entry("ipv4", &addressesv4, "icmp"),
					ginkgo.Entry("ipv6", &addressesv6, "icmp6"))

				ginkgo.DescribeTable("Should validate TCP/UDP connectivity to an external gateway's loopback address via a pod with external gateway annotations enabled",
					func(protocol string, addresses *gatewayTestIPs, destPort int) {
						if addresses.srcPodIP == "" || addresses.nodeIP == "" {
							skipper.Skipf("Skipping as pod ip / node ip are not set pod ip %s node ip %s", addresses.srcPodIP, addresses.nodeIP)
						}

						for _, gwContainer := range gwContainers {
							_, err := runCommand(containerRuntime, "exec", gwContainer, "ping", "-c", testTimeout, addresses.srcPodIP)
							framework.ExpectNoError(err, "Failed to ping %s from container %s", addresses.srcPodIP, gwContainer)
						}

						for _, gwContainer := range gwContainers {
							gomega.Expect(isBFDPaired(gwContainer, addresses.nodeIP)).To(gomega.Equal(true), "Bfd not paired")
						}

						expectedHostNames := hostNamesForContainers(gwContainers)
						framework.Logf("Expected hostnames are %v", expectedHostNames)

						returnedHostNames := make(map[string]struct{})
						target := addresses.targetIPs[0]
						success := false
						for i := 0; i < 20; i++ {
							hostname := pokeHostnameViaNC(srcPingPodName, f.Namespace.Name, protocol, target, destPort)
							if hostname != "" {
								returnedHostNames[hostname] = struct{}{}
							}

							if cmp.Equal(returnedHostNames, expectedHostNames) {
								success = true
								break
							}
						}
						framework.Logf("Received hostnames for protocol %s are %v ", protocol, returnedHostNames)

						if !success {
							framework.Failf("Failed to hit all the external gateways via for protocol %s, diff %s", protocol, cmp.Diff(expectedHostNames, returnedHostNames))
						}

						if len(gwContainers) > 1 {
							ginkgo.By("Deleting one container")
							deleteClusterExternalContainer(gwContainers[1])
							ginkgo.By("Waiting for BFD to sync")
							time.Sleep(3 * time.Second) // bfd timeout

							// ECMP should direct all the traffic to the only container
							expectedHostName := hostNameForContainer(gwContainers[0])

							ginkgo.By("Checking hostname multiple times")
							for i := 0; i < 20; i++ {
								hostname := pokeHostnameViaNC(srcPingPodName, f.Namespace.Name, protocol, target, destPort)
								gomega.Expect(expectedHostName).To(gomega.Equal(hostname), "Hostname returned by nc not as expected")
							}
						}
					},
					ginkgo.Entry("UDP ipv4", "udp", &addressesv4, externalUDPPort),
					ginkgo.Entry("TCP ipv4", "tcp", &addressesv4, externalTCPPort),
					ginkgo.Entry("UDP ipv6", "udp", &addressesv6, externalUDPPort),
					ginkgo.Entry("TCP ipv6", "tcp", &addressesv6, externalTCPPort))
			})

			// Validate pods can reach a network running in multiple container's loopback
			// addresses via two external gateways running on eth0 of the container without
			// any tunnel encap. This test defines two external gateways and validates ECMP
			// functionality to the container loopbacks. To verify traffic reaches the
			// gateways, tcpdump is running on the external gateways and will exit successfully
			// once an ICMP packet is received from the annotated pod in the k8s cluster.
			// Two additional gateways are added to verify the tcp / udp protocols.
			// They run the netexec command, and the pod asks to return their hostname.
			// The test checks that both hostnames are collected at least once.
			var _ = ginkgo.Describe("e2e multiple external gateway validation", func() {
				const (
					svcname         string = "novxlan-externalgw-ecmp"
					gwContainer1    string = "gw-test-container1"
					gwContainer2    string = "gw-test-container2"
					testTimeout     string = "30"
					ecmpRetry       int    = 20
					srcPodName             = "e2e-exgw-src-pod"
					externalTCPPort        = 80
					externalUDPPort        = 90
				)

				var (
					gwContainers []string
				)

				testContainer := fmt.Sprintf("%s-container", srcPodName)
				testContainerFlag := fmt.Sprintf("--container=%s", testContainer)

				f := wrappedTestFramework(svcname)

				var addressesv4, addressesv6 gatewayTestIPs

				ginkgo.BeforeEach(func() {
					nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), f.ClientSet, 3)
					framework.ExpectNoError(err)
					if len(nodes.Items) < 3 {
						framework.Failf(
							"Test requires >= 3 Ready nodes, but there are only %v nodes",
							len(nodes.Items))
					}

					if externalContainerNetwork == "host" {
						skipper.Skipf("Skipping as host network doesn't support multiple external gateways")
					}

					setupBFD := setupBFDOnContainer(nodes.Items)
					gwContainers, addressesv4, addressesv6 = setupGatewayContainers(f, nodes, gwContainer1, gwContainer2, srcPodName, externalUDPPort, externalTCPPort, ecmpRetry, setupBFD)

				})

				ginkgo.AfterEach(func() {
					// tear down the containers simulating the gateways
					deleteClusterExternalContainer(gwContainer1)
					deleteClusterExternalContainer(gwContainer2)
					resetGatewayAnnotations(f)
				})

				ginkgo.DescribeTable("Should validate ICMP connectivity to multiple external gateways for an ECMP scenario", func(addresses *gatewayTestIPs, icmpToDump string) {
					if addresses.srcPodIP == "" || addresses.nodeIP == "" {
						skipper.Skipf("Skipping as pod ip / node ip are not set pod ip %s node ip %s", addresses.srcPodIP, addresses.nodeIP)
					}

					annotateNamespaceForGateway(f.Namespace.Name, true, addresses.gatewayIPs[:]...)
					for _, gwContainer := range gwContainers {
						// Ping from a common IP address that exists on both gateways to ensure test coverage where ingress reply goes back to the same host.
						_, err := runCommand(containerRuntime, "exec", gwContainer, "ping", "-I", addresses.targetIPs[0], "-c", testTimeout, addresses.srcPodIP)
						framework.ExpectNoError(err, "Failed to ping %s from container %s", addresses.srcPodIP, gwContainer)
					}

					// This is needed for bfd to sync up
					time.Sleep(3 * time.Second)

					for _, gwContainer := range gwContainers {
						gomega.Expect(isBFDPaired(gwContainer, addresses.nodeIP)).To(gomega.Equal(true), "Bfd not paired")
					}

					// Verify the gateways and remote loopback addresses are reachable from the pod.
					// Iterate checking connectivity to the loopbacks on the gateways until tcpdump see
					// the traffic or 20 attempts fail. Odds of a false negative here is ~ (1/2)^20
					ginkgo.By("Verifying ecmp connectivity to the external gateways by iterating through the targets")

					// Check for egress traffic to both gateway loopback addresses using tcpdump, since
					// /proc/net/dev counters only record the ingress interface traffic is received on.
					// The test will waits until an ICMP packet is matched on the gateways or fail the
					// test if a packet to the loopback is not received within the timer interval.
					// If an ICMP packet is never detected, return the error via the specified chanel.

					tcpDumpSync := sync.WaitGroup{}
					tcpDumpSync.Add(len(gwContainers))
					for _, gwContainer := range gwContainers {
						go checkReceivedPacketsOnContainer(gwContainer, srcPodName, anyLink, []string{icmpToDump}, &tcpDumpSync)
					}

					// spawn a goroutine to asynchronously (to speed up the test)
					// to ping the gateway loopbacks on both containers via ECMP.

					pingSync := sync.WaitGroup{}

					// spawn a goroutine to asynchronously (to speed up the test)
					// to ping the gateway loopbacks on both containers via ECMP.
					for _, address := range addresses.targetIPs {
						pingSync.Add(1)
						go func(target string) {
							defer ginkgo.GinkgoRecover()
							defer pingSync.Done()
							_, err := e2ekubectl.RunKubectl(f.Namespace.Name, "exec", srcPodName, testContainerFlag, "--", "ping", "-c", testTimeout, target)
							if err != nil {
								framework.Logf("error generating a ping from the test pod %s: %v", srcPodName, err)
							}
						}(address)
					}

					pingSync.Wait()
					tcpDumpSync.Wait()

					ginkgo.By("Deleting one container")
					deleteClusterExternalContainer(gwContainers[1])
					time.Sleep(3 * time.Second) // bfd timeout

					pingSync = sync.WaitGroup{}
					tcpDumpSync = sync.WaitGroup{}

					tcpDumpSync.Add(1)
					go checkReceivedPacketsOnContainer(gwContainers[0], srcPodName, anyLink, []string{icmpToDump}, &tcpDumpSync)

					// spawn a goroutine to asynchronously (to speed up the test)
					// to ping the gateway loopbacks on both containers via ECMP.
					for _, address := range addresses.targetIPs {
						pingSync.Add(1)
						go func(target string) {
							defer ginkgo.GinkgoRecover()
							defer pingSync.Done()
							_, err := e2ekubectl.RunKubectl(f.Namespace.Name, "exec", srcPodName, testContainerFlag, "--", "ping", "-c", testTimeout, target)
							if err != nil {
								framework.Logf("error generating a ping from the test pod %s: %v", srcPodName, err)
							}
						}(address)
					}

					pingSync.Wait()
					tcpDumpSync.Wait()

				}, ginkgo.Entry("IPV4", &addressesv4, "icmp"),
					ginkgo.Entry("IPV6", &addressesv6, "icmp6"))

				// This test runs a listener on the external container, returning the host name both on tcp and udp.
				// The src pod tries to hit the remote address until both the containers are hit.
				ginkgo.DescribeTable("Should validate TCP/UDP connectivity to multiple external gateways for a UDP / TCP scenario", func(addresses *gatewayTestIPs, protocol string, destPort int) {
					if addresses.srcPodIP == "" || addresses.nodeIP == "" {
						skipper.Skipf("Skipping as pod ip / node ip are not set pod ip %s node ip %s", addresses.srcPodIP, addresses.nodeIP)
					}

					annotateNamespaceForGateway(f.Namespace.Name, true, addresses.gatewayIPs[:]...)

					for _, gwContainer := range gwContainers {
						_, err := runCommand(containerRuntime, "exec", gwContainer, "ping", "-c", testTimeout, addresses.srcPodIP)
						framework.ExpectNoError(err, "Failed to ping %s from container %s", addresses.srcPodIP, gwContainer)
					}

					// This is needed for bfd to sync up
					time.Sleep(3 * time.Second)

					for _, gwContainer := range gwContainers {
						gomega.Expect(isBFDPaired(gwContainer, addresses.nodeIP)).To(gomega.Equal(true), "Bfd not paired")
					}

					expectedHostNames := hostNamesForContainers(gwContainers)
					framework.Logf("Expected hostnames are %v", expectedHostNames)

					returnedHostNames := make(map[string]struct{})
					success := false

					// Picking only the first address, the one the udp listener is set for
					target := addresses.targetIPs[0]
					for i := 0; i < 20; i++ {
						hostname := pokeHostnameViaNC(srcPodName, f.Namespace.Name, protocol, target, destPort)
						if hostname != "" {
							returnedHostNames[hostname] = struct{}{}
						}
						if cmp.Equal(returnedHostNames, expectedHostNames) {
							success = true
							break
						}
					}

					framework.Logf("Received hostnames for protocol %s are %v ", protocol, returnedHostNames)

					if !success {
						framework.Failf("Failed to hit all the external gateways via for protocol %s, diff %s", protocol, cmp.Diff(expectedHostNames, returnedHostNames))
					}

					ginkgo.By("Deleting one container")
					deleteClusterExternalContainer(gwContainers[1])
					ginkgo.By("Waiting for BFD to sync")
					time.Sleep(3 * time.Second) // bfd timeout

					// ECMP should direct all the traffic to the only container
					expectedHostName := hostNameForContainer(gwContainers[0])

					ginkgo.By("Checking hostname multiple times")
					for i := 0; i < 20; i++ {
						hostname := pokeHostnameViaNC(srcPodName, f.Namespace.Name, protocol, target, destPort)
						gomega.Expect(expectedHostName).To(gomega.Equal(hostname), "Hostname returned by nc not as expected")
					}
				}, ginkgo.Entry("IPV4 udp", &addressesv4, "udp", externalUDPPort),
					ginkgo.Entry("IPV4 tcp", &addressesv4, "tcp", externalTCPPort),
					ginkgo.Entry("IPV6 udp", &addressesv6, "udp", externalUDPPort),
					ginkgo.Entry("IPV6 tcp", &addressesv6, "tcp", externalTCPPort))
			})
		})

	})

	var _ = ginkgo.Context("With Admin Policy Based External Route CRs", func() {

		// Validate pods can reach a network running in a container's looback address via
		// an external gateway running on eth0 of the container without any tunnel encap.
		// The traffic will get proxied through an annotated pod in the serving namespace.
		var _ = ginkgo.Describe("e2e non-vxlan external gateway through a gateway pod", func() {
			const (
				svcname         string = "externalgw-pod-novxlan"
				gwContainer1    string = "ex-gw-container1"
				gwContainer2    string = "ex-gw-container2"
				srcPingPodName  string = "e2e-exgw-src-ping-pod"
				gatewayPodName1 string = "e2e-gateway-pod1"
				gatewayPodName2 string = "e2e-gateway-pod2"
				externalTCPPort        = 91
				externalUDPPort        = 90
				ecmpRetry       int    = 20
				testTimeout     string = "20"
			)

			var (
				sleepCommand             = []string{"bash", "-c", "sleep 20000"}
				addressesv4, addressesv6 gatewayTestIPs
				clientSet                kubernetes.Interface
				servingNamespace         string
				gwContainers             []string
			)

			f := wrappedTestFramework(svcname)

			ginkgo.BeforeEach(func() {
				clientSet = f.ClientSet // so it can be used in AfterEach
				// retrieve worker node names
				nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), f.ClientSet, 3)
				framework.ExpectNoError(err)
				if len(nodes.Items) < 3 {
					framework.Failf(
						"Test requires >= 3 Ready nodes, but there are only %v nodes",
						len(nodes.Items))
				}

				ns, err := f.CreateNamespace(context.TODO(), "exgw-serving", nil)
				framework.ExpectNoError(err)
				servingNamespace = ns.Name

				gwContainers, addressesv4, addressesv6 = setupGatewayContainers(f, nodes, gwContainer1, gwContainer2, srcPingPodName, externalUDPPort, externalTCPPort, ecmpRetry)
				setupPolicyBasedGatewayPods(f, nodes, gatewayPodName1, gatewayPodName2, servingNamespace, sleepCommand, addressesv4, addressesv6)
			})

			ginkgo.AfterEach(func() {
				deleteAPBExternalRouteCR(defaultPolicyName)
				cleanExGWContainers(clientSet, []string{gwContainer1, gwContainer2}, addressesv4, addressesv6)
			})

			ginkgo.DescribeTable("Should validate ICMP connectivity to an external gateway's loopback address via a gateway pod",
				func(addresses *gatewayTestIPs, icmpCommand string) {
					if addresses.srcPodIP == "" || addresses.nodeIP == "" {
						skipper.Skipf("Skipping as pod ip / node ip are not set pod ip %s node ip %s", addresses.srcPodIP, addresses.nodeIP)
					}
					createAPBExternalRouteCRWithDynamicHop(defaultPolicyName, f.Namespace.Name, servingNamespace, false, addresses.gatewayIPs)

					ginkgo.By(fmt.Sprintf("Verifying connectivity to the pod [%s] from external gateways", addresses.srcPodIP))
					for _, gwContainer := range gwContainers {
						// Ping from a common IP address that exists on both gateways to ensure test coverage where ingress reply goes back to the same host.
						_, err := runCommand(containerRuntime, "exec", gwContainer, "ping", "-I", addresses.targetIPs[0], "-c", testTimeout, addresses.srcPodIP)
						framework.ExpectNoError(err, "Failed to ping %s from container %s", addresses.srcPodIP, gwContainer)
					}

					tcpDumpSync := sync.WaitGroup{}
					tcpDumpSync.Add(len(gwContainers))

					for _, gwContainer := range gwContainers {
						go checkReceivedPacketsOnContainer(gwContainer, srcPingPodName, anyLink, []string{icmpCommand}, &tcpDumpSync)
					}

					pingSync := sync.WaitGroup{}
					// Verify the external gateway loopback address running on the external container is reachable and
					// that traffic from the source ping pod is proxied through the pod in the serving namespace
					ginkgo.By("Verifying connectivity via the gateway namespace to the remote addresses")
					for _, t := range addresses.targetIPs {
						pingSync.Add(1)
						go func(target string) {
							defer ginkgo.GinkgoRecover()
							defer pingSync.Done()
							_, err := e2ekubectl.RunKubectl(f.Namespace.Name, "exec", srcPingPodName, "--", "ping", "-c", testTimeout, target)
							framework.ExpectNoError(err, "Failed to ping remote gateway %s from pod %s", target, srcPingPodName)
						}(t)
					}
					pingSync.Wait()
					tcpDumpSync.Wait()
					checkAPBExternalRouteStatus(defaultPolicyName)
				},
				ginkgo.Entry("ipv4", &addressesv4, "icmp"),
				ginkgo.Entry("ipv6", &addressesv6, "icmp6"))

			ginkgo.DescribeTable("Should validate TCP/UDP connectivity to an external gateway's loopback address via a gateway pod",
				func(protocol string, addresses *gatewayTestIPs, destPort, destPortOnPod int) {
					if addresses.srcPodIP == "" || addresses.nodeIP == "" {
						skipper.Skipf("Skipping as pod ip / node ip are not set pod ip %s node ip %s", addresses.srcPodIP, addresses.nodeIP)
					}
					createAPBExternalRouteCRWithDynamicHop(defaultPolicyName, f.Namespace.Name, servingNamespace, false, addressesv4.gatewayIPs)

					for _, container := range gwContainers {
						reachPodFromGateway(addresses.srcPodIP, strconv.Itoa(destPortOnPod), srcPingPodName, container, protocol)
					}

					expectedHostNames := make(map[string]struct{})
					for _, c := range gwContainers {
						res, err := runCommand(containerRuntime, "exec", c, "hostname")
						framework.ExpectNoError(err, "failed to run hostname in %s", c)
						hostname := strings.TrimSuffix(res, "\n")
						framework.Logf("Hostname for %s is %s", c, hostname)
						expectedHostNames[hostname] = struct{}{}
					}
					framework.Logf("Expected hostnames are %v", expectedHostNames)

					ginkgo.By("Checking that external ips are reachable with both gateways")
					returnedHostNames := make(map[string]struct{})
					target := addresses.targetIPs[0]
					success := false
					for i := 0; i < 20; i++ {
						args := []string{"exec", srcPingPodName, "--"}
						if protocol == "tcp" {
							args = append(args, "bash", "-c", fmt.Sprintf("echo | nc -w 1 %s %d", target, destPort))
						} else {
							args = append(args, "bash", "-c", fmt.Sprintf("echo | nc -w 1 -u %s %d", target, destPort))
						}
						res, err := e2ekubectl.RunKubectl(f.Namespace.Name, args...)
						framework.ExpectNoError(err, "failed to reach %s (%s)", target, protocol)
						hostname := strings.TrimSuffix(res, "\n")
						if hostname != "" {
							returnedHostNames[hostname] = struct{}{}
						}

						if cmp.Equal(returnedHostNames, expectedHostNames) {
							success = true
							break
						}
					}
					framework.Logf("Received hostnames for protocol %s are %v ", protocol, returnedHostNames)

					if !success {
						framework.Failf("Failed to hit all the external gateways via for protocol %s, diff %s", protocol, cmp.Diff(expectedHostNames, returnedHostNames))
					}
					checkAPBExternalRouteStatus(defaultPolicyName)
				},
				ginkgo.Entry("UDP ipv4", "udp", &addressesv4, externalUDPPort, srcUDPPort),
				ginkgo.Entry("TCP ipv4", "tcp", &addressesv4, externalTCPPort, srcHTTPPort),
				ginkgo.Entry("UDP ipv6", "udp", &addressesv6, externalUDPPort, srcUDPPort),
				ginkgo.Entry("TCP ipv6", "tcp", &addressesv6, externalTCPPort, srcHTTPPort))

			ginkgo.DescribeTable("Should validate TCP/UDP connectivity even after MAC change (gateway migration) for egress",
				func(protocol string, addresses *gatewayTestIPs, destPort, destPortOnPod int) {
					ncCmd := func(sourcePort int, target string) []string {
						if protocol == "tcp" {
							return []string {"exec", srcPingPodName, "--", "bash", "-c", fmt.Sprintf("echo | nc -p %d -s %s -w 1 %s %d", sourcePort, addresses.srcPodIP, target, destPort)}
						} else {
							return []string {"exec", srcPingPodName, "--", "bash", "-c", fmt.Sprintf("echo | nc -p %d -s %s -w 1 -u %s %d", sourcePort, addresses.srcPodIP, target, destPort)}
						}
					}
					if addresses.srcPodIP == "" || addresses.nodeIP == "" {
						skipper.Skipf("Skipping as pod ip / node ip are not set pod ip %s node ip %s", addresses.srcPodIP, addresses.nodeIP)
					}
					createAPBExternalRouteCRWithDynamicHop(defaultPolicyName, f.Namespace.Name, servingNamespace, false, addressesv4.gatewayIPs)

					ginkgo.By("Checking Ingress connectivity from gateways")
					// Check Ingress connectivity
					for _, container := range gwContainers {
						reachPodFromGateway(addresses.srcPodIP, strconv.Itoa(destPortOnPod), srcPingPodName, container, protocol)
					}

					// Get hostnames of gateways
					// map of hostname to gateway
					expectedHostNames := make(map[string]string)
					gwAddresses := make(map[string]string)
					for _, c := range gwContainers {
						res, err := runCommand(containerRuntime, "exec", c, "hostname")
						framework.ExpectNoError(err, "failed to run hostname in %s", c)
						hostname := strings.TrimSuffix(res, "\n")
						res, err = runCommand(containerRuntime, "exec", c, "hostname", "-I")
						framework.ExpectNoError(err, "failed to run hostname in %s", c)
						ips := strings.TrimSuffix(res, "\n")
						framework.Logf("Hostname for %s is %s, with IP addresses: %s", c, hostname, ips)
						expectedHostNames[hostname] = c
						gwAddresses[c] = ips
					}
					framework.Logf("Expected hostnames are %v", expectedHostNames)

					// We have to remove a gateway so that traffic consistently goes to the same gateway. This
					// is due to lack of consistent hashing support in github actions:
					// https://github.com/ovn-org/ovn-kubernetes/pull/4114#issuecomment-1940916326
					// TODO(trozet) change this back to 2 gateways once github actions kernel is updated
					ginkgo.By(fmt.Sprintf("Reducing to one gateway. Removing gateway: %s", gatewayPodName2))
					e2epod.DeletePodWithWaitByName(context.TODO(), f.ClientSet, gatewayPodName2, servingNamespace)
					time.Sleep(1 * time.Second)

					ginkgo.By("Checking if one of the external gateways are reachable via Egress")
					target := addresses.targetIPs[0]
					sourcePort := 50000

					res, err := e2ekubectl.RunKubectl(f.Namespace.Name, ncCmd(sourcePort, target)...)
					framework.ExpectNoError(err, "failed to reach %s (%s)", target, protocol)
					hostname := strings.TrimSuffix(res, "\n")
					var gateway string
					if g, ok := expectedHostNames[hostname]; !ok {
						framework.Failf("Unexpected gateway hostname %q, expected; %#v", hostname, expectedHostNames)
					} else {
						gateway = g
					}

					macAddressExGW, err := net.ParseMAC(getMACAddressesForNetwork(gateway, externalContainerNetwork))
					framework.ExpectNoError(err, "failed to find MAC address of hostname: %s", hostname)
					framework.Logf("Egress gateway reached: %s, with MAC: %q", gateway, macAddressExGW)

					ginkgo.By("Sending traffic again and verifying packet is received at gateway")
					tcpDumpSync := sync.WaitGroup{}
					tcpDumpSync.Add(1)
					go checkReceivedPacketsOnContainer(gateway, srcPingPodName, anyLink, []string{protocol, "and", "port", strconv.Itoa(sourcePort)}, &tcpDumpSync)
					res, err = e2ekubectl.RunKubectl(f.Namespace.Name, ncCmd(sourcePort, target)...)
					framework.ExpectNoError(err, "failed to reach %s (%s)", target, protocol)
					hostname2 := strings.TrimSuffix(res, "\n")
					gomega.Expect(hostname).To(gomega.Equal(hostname2))
					tcpDumpSync.Wait()

					newDummyMac := "02:11:22:33:44:56"
					gwLink := "eth0"
					ginkgo.By(fmt.Sprintf("Modifying MAC address of gateway %q to simulate migration, new MAC: %s", gateway, newDummyMac))
					defer setMACAddrOnContainer(gateway, macAddressExGW.String(), "eth0")
					setMACAddrOnContainer(gateway, newDummyMac, gwLink)

					ginkgo.By("Sending layer 2 advertisement from external gateway")
					time.Sleep(1 * time.Second)

					// we need to know which gateway IP we are using
					var gwAddr string
					for _, a := range addresses.gatewayIPs {
						if strings.Contains(gwAddresses[gateway], a) {
							gwAddr = a
							break
						}
					}
					gomega.Expect(gwAddr).To(gomega.Not(gomega.BeEmpty()))

					if utilnet.IsIPv4String(gwAddr) {
						sendGARP(gateway, gwAddr, gwLink)
					} else {
						sendNDPAdvertisement(gateway, gwAddr, gwLink)
					}
					time.Sleep(1 * time.Second)

					ginkgo.By("Post-Migration: Sending Egress traffic and verify it is received")
					// We don't want traffic to hit the already existing conntrack entry (created for source port 50000)
					// so we use a fresh source port.
					sourcePort = 50001

					tcpDumpSync = sync.WaitGroup{}
					tcpDumpSync.Add(1)
					go checkReceivedPacketsOnContainer(gateway, srcPingPodName, gwLink, []string{protocol, "and", "ether", "host", newDummyMac, "and", "port", strconv.Itoa(sourcePort)}, &tcpDumpSync)
					// Sometimes the external gateway will fail to respond to the request with
					// SKB_DROP_REASON_NEIGH_FAILED after changing the MAC address. Something breaks with ARP
					// on the gateway container. Therefore, ignore the reply from gateway, as we only care about the egress
					// packet arriving with correct MAC address.
					_, _ = e2ekubectl.RunKubectl(f.Namespace.Name, ncCmd(sourcePort, target)...)
					tcpDumpSync.Wait()

					checkAPBExternalRouteStatus(defaultPolicyName)
				},
				ginkgo.Entry("UDP ipv4", "udp", &addressesv4, externalUDPPort, srcUDPPort),
				ginkgo.Entry("TCP ipv4", "tcp", &addressesv4, externalTCPPort, srcHTTPPort),
				ginkgo.Entry("UDP ipv6", "udp", &addressesv6, externalUDPPort, srcUDPPort),
				ginkgo.Entry("TCP ipv6", "tcp", &addressesv6, externalTCPPort, srcHTTPPort))
		})

		// Validate pods can reach a network running in multiple container's loopback
		// addresses via two external gateways running on eth0 of the container without
		// any tunnel encap. This test defines two external gateways and validates ECMP
		// functionality to the container loopbacks. To verify traffic reaches the
		// gateways, tcpdump is running on the external gateways and will exit successfully
		// once an ICMP packet is received from the annotated pod in the k8s cluster.
		// Two additional gateways are added to verify the tcp / udp protocols.
		// They run the netexec command, and the pod asks to return their hostname.
		// The test checks that both hostnames are collected at least once.
		var _ = ginkgo.Describe("e2e multiple external gateway validation", func() {
			const (
				svcname         string = "novxlan-externalgw-ecmp"
				gwContainer1    string = "gw-test-container1"
				gwContainer2    string = "gw-test-container2"
				testTimeout     string = "30"
				ecmpRetry       int    = 20
				srcPodName             = "e2e-exgw-src-pod"
				externalTCPPort        = 80
				externalUDPPort        = 90
			)

			f := wrappedTestFramework(svcname)

			var gwContainers []string
			var addressesv4, addressesv6 gatewayTestIPs

			ginkgo.BeforeEach(func() {
				// retrieve worker node names
				nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), f.ClientSet, 3)
				framework.ExpectNoError(err)
				if len(nodes.Items) < 3 {
					framework.Failf(
						"Test requires >= 3 Ready nodes, but there are only %v nodes",
						len(nodes.Items))
				}

				if externalContainerNetwork == "host" {
					skipper.Skipf("Skipping as host network doesn't support multiple external gateways")
				}
				gwContainers, addressesv4, addressesv6 = setupGatewayContainers(f, nodes, gwContainer1, gwContainer2, srcPodName, externalUDPPort, externalTCPPort, ecmpRetry)
			})

			ginkgo.AfterEach(func() {
				// tear down the containers simulating the gateways
				deleteClusterExternalContainer(gwContainer1)
				deleteClusterExternalContainer(gwContainer2)
				deleteAPBExternalRouteCR(defaultPolicyName)
			})

			ginkgo.DescribeTable("Should validate ICMP connectivity to multiple external gateways for an ECMP scenario", func(addresses *gatewayTestIPs, icmpToDump string) {
				if addresses.srcPodIP == "" || addresses.nodeIP == "" {
					skipper.Skipf("Skipping as pod ip / node ip are not set pod ip %s node ip %s", addresses.srcPodIP, addresses.nodeIP)
				}
				createAPBExternalRouteCRWithStaticHop(defaultPolicyName, f.Namespace.Name, false, addresses.gatewayIPs...)

				ginkgo.By("Verifying connectivity to the pod from external gateways")
				for _, gwContainer := range gwContainers {
					// Ping from a common IP address that exists on both gateways to ensure test coverage where ingress reply goes back to the same host.
					_, err := runCommand(containerRuntime, "exec", gwContainer, "ping", "-I", addresses.targetIPs[0], "-c", testTimeout, addresses.srcPodIP)
					framework.ExpectNoError(err, "Failed to ping %s from container %s", addresses.srcPodIP, gwContainer)
				}

				ginkgo.By("Verifying connectivity to the pod from external gateways with large packets > pod MTU")
				for _, gwContainer := range gwContainers {
					_, err := runCommand(containerRuntime, "exec", gwContainer, "ping", "-s", "1420", "-c", testTimeout, addresses.srcPodIP)
					framework.ExpectNoError(err, "Failed to ping %s from container %s", addresses.srcPodIP, gwContainer)
				}

				// Verify the gateways and remote loopback addresses are reachable from the pod.
				// Iterate checking connectivity to the loopbacks on the gateways until tcpdump see
				// the traffic or 20 attempts fail. Odds of a false negative here is ~ (1/2)^20
				ginkgo.By("Verifying ecmp connectivity to the external gateways by iterating through the targets")

				// Check for egress traffic to both gateway loopback addresses using tcpdump, since
				// /proc/net/dev counters only record the ingress interface traffic is received on.
				// The test will waits until an ICMP packet is matched on the gateways or fail the
				// test if a packet to the loopback is not received within the timer interval.
				// If an ICMP packet is never detected, return the error via the specified chanel.

				tcpDumpSync := sync.WaitGroup{}
				tcpDumpSync.Add(len(gwContainers))
				for _, gwContainer := range gwContainers {
					go checkReceivedPacketsOnContainer(gwContainer, srcPodName, anyLink, []string{icmpToDump}, &tcpDumpSync)
				}

				pingSync := sync.WaitGroup{}

				// spawn a goroutine to asynchronously (to speed up the test)
				// to ping the gateway loopbacks on both containers via ECMP.
				for _, address := range addresses.targetIPs {
					pingSync.Add(1)
					go func(target string) {
						defer ginkgo.GinkgoRecover()
						defer pingSync.Done()
						_, err := e2ekubectl.RunKubectl(f.Namespace.Name, "exec", srcPodName, "--", "ping", "-c", testTimeout, target)
						if err != nil {
							framework.Logf("error generating a ping from the test pod %s: %v", srcPodName, err)
						}
					}(address)
				}
				pingSync.Wait()
				tcpDumpSync.Wait()

			}, ginkgo.Entry("IPV4", &addressesv4, "icmp"),
				ginkgo.Entry("IPV6", &addressesv6, "icmp6"))

			// This test runs a listener on the external container, returning the host name both on tcp and udp.
			// The src pod tries to hit the remote address until both the containers are hit.
			ginkgo.DescribeTable("Should validate TCP/UDP connectivity to multiple external gateways for a UDP / TCP scenario", func(addresses *gatewayTestIPs, protocol string, destPort, destPortOnPod int) {
				if addresses.srcPodIP == "" || addresses.nodeIP == "" {
					skipper.Skipf("Skipping as pod ip / node ip are not set pod ip %s node ip %s", addresses.srcPodIP, addresses.nodeIP)
				}
				createAPBExternalRouteCRWithStaticHop(defaultPolicyName, f.Namespace.Name, false, addresses.gatewayIPs...)

				for _, container := range gwContainers {
					reachPodFromGateway(addresses.srcPodIP, strconv.Itoa(destPortOnPod), srcPodName, container, protocol)
				}

				expectedHostNames := hostNamesForContainers(gwContainers)
				framework.Logf("Expected hostnames are %v", expectedHostNames)

				returnedHostNames := make(map[string]struct{})
				success := false

				// Picking only the first address, the one the udp listener is set for
				target := addresses.targetIPs[0]
				for i := 0; i < 20; i++ {
					hostname := pokeHostnameViaNC(srcPodName, f.Namespace.Name, protocol, target, destPort)
					if hostname != "" {
						returnedHostNames[hostname] = struct{}{}
					}
					if cmp.Equal(returnedHostNames, expectedHostNames) {
						success = true
						break
					}
				}

				framework.Logf("Received hostnames for protocol %s are %v ", protocol, returnedHostNames)

				if !success {
					framework.Failf("Failed to hit all the external gateways via for protocol %s, diff %s", protocol, cmp.Diff(expectedHostNames, returnedHostNames))
				}

			}, ginkgo.Entry("IPV4 udp", &addressesv4, "udp", externalUDPPort, srcUDPPort),
				ginkgo.Entry("IPV4 tcp", &addressesv4, "tcp", externalTCPPort, srcHTTPPort),
				ginkgo.Entry("IPV6 udp", &addressesv6, "udp", externalUDPPort, srcUDPPort),
				ginkgo.Entry("IPV6 tcp", &addressesv6, "tcp", externalTCPPort, srcHTTPPort))
		})

		var _ = ginkgo.Describe("e2e multiple external gateway stale conntrack entry deletion validation", func() {
			const (
				svcname         string = "novxlan-externalgw-ecmp"
				gwContainer1    string = "gw-test-container1"
				gwContainer2    string = "gw-test-container2"
				srcPodName      string = "e2e-exgw-src-pod"
				gatewayPodName1 string = "e2e-gateway-pod1"
				gatewayPodName2 string = "e2e-gateway-pod2"
			)

			f := wrappedTestFramework(svcname)

			var (
				servingNamespace         string
				addressesv4, addressesv6 gatewayTestIPs
				sleepCommand             []string
				nodes                    *v1.NodeList
				err                      error
				clientSet                kubernetes.Interface
			)

			ginkgo.BeforeEach(func() {
				clientSet = f.ClientSet // so it can be used in AfterEach
				// retrieve worker node names
				nodes, err = e2enode.GetBoundedReadySchedulableNodes(context.TODO(), clientSet, 3)
				framework.ExpectNoError(err)
				if len(nodes.Items) < 3 {
					framework.Failf(
						"Test requires >= 3 Ready nodes, but there are only %v nodes",
						len(nodes.Items))
				}

				if externalContainerNetwork == "host" {
					skipper.Skipf("Skipping as host network doesn't support multiple external gateways")
				}

				ns, err := f.CreateNamespace(context.TODO(), "exgw-conntrack-serving", nil)
				framework.ExpectNoError(err)
				servingNamespace = ns.Name

				addressesv4, addressesv6 = setupGatewayContainersForConntrackTest(f, nodes, gwContainer1, gwContainer2, srcPodName)
				sleepCommand = []string{"bash", "-c", "sleep 20000"}
				_, err = createGenericPodWithLabel(f, gatewayPodName1, nodes.Items[0].Name, servingNamespace, sleepCommand, map[string]string{"name": gatewayPodName1, "gatewayPod": "true"})
				framework.ExpectNoError(err, "Create the external gw pods to manage the src app pod namespace, failed: %v", err)
				_, err = createGenericPodWithLabel(f, gatewayPodName2, nodes.Items[1].Name, servingNamespace, sleepCommand, map[string]string{"name": gatewayPodName2, "gatewayPod": "true"})
				framework.ExpectNoError(err, "Create the external gw pods to manage the src app pod namespace, failed: %v", err)
			})

			ginkgo.AfterEach(func() {
				deleteClusterExternalContainer(gwContainer1)
				deleteClusterExternalContainer(gwContainer2)
				deleteAPBExternalRouteCR(defaultPolicyName)
			})

			ginkgo.DescribeTable("Static Hop: Should validate conntrack entry deletion for TCP/UDP traffic via multiple external gateways a.k.a ECMP routes", func(addresses *gatewayTestIPs, protocol string) {
				if addresses.srcPodIP == "" || addresses.nodeIP == "" {
					skipper.Skipf("Skipping as pod ip / node ip are not set pod ip %s node ip %s", addresses.srcPodIP, addresses.nodeIP)
				}
				ginkgo.By("Create a static hop in an Admin Policy Based External Route CR targeting the app namespace to get managed by external gateways")
				createAPBExternalRouteCRWithStaticHop(defaultPolicyName, f.Namespace.Name, false, addresses.gatewayIPs...)
				setupIperf3Client := func(container, address string, port int) {
					// note iperf3 even when using udp also spawns tcp connection first; so we indirectly also have the tcp connection when using "-u" flag
					cmd := []string{containerRuntime, "exec", container, "iperf3", "-u", "-c", address, "-p", fmt.Sprintf("%d", port), "-b", "1M", "-i", "1", "-t", "3", "&"}
					_, err := runCommand(cmd...)
					klog.Infof("iperf3 command %s", strings.Join(cmd, " "))
					framework.ExpectNoError(err, "failed to setup iperf3 client for %s", container)
				}
				macAddressGW := make([]string, 2)
				for i, containerName := range []string{gwContainer1, gwContainer2} {
					ginkgo.By("Start iperf3 client from external container to connect to iperf3 server running at the src pod")
					setupIperf3Client(containerName, addresses.srcPodIP, 5201+i)
					macAddressExtGW, err := net.ParseMAC(getMACAddressesForNetwork(containerName, externalContainerNetwork))
					framework.ExpectNoError(err, "failed to parse MAC address for %s", containerName)
					// Trim leading 0s because conntrack dumped labels are just integers
					// in hex without leading 0s.
					macAddressGW[i] = strings.TrimLeft(strings.Replace(macAddressExtGW.String(), ":", "", -1), "0")
				}
				ginkgo.By("Check if conntrack entries for ECMP routes are created for the 2 external gateways")
				nodeName := getPod(f, srcPodName).Spec.NodeName
				podConnEntriesWithMACLabelsSet := 2
				totalPodConnEntries := 6
				gomega.Eventually(func() int {
					return pokeConntrackEntries(nodeName, addresses.srcPodIP, protocol, macAddressGW)
				}, time.Minute, 5).Should(gomega.Equal(podConnEntriesWithMACLabelsSet))
				gomega.Expect(pokeConntrackEntries(nodeName, addresses.srcPodIP, protocol, nil)).To(gomega.Equal(totalPodConnEntries))

				ginkgo.By("Check if conntrack entries for ECMP routes are removed for the deleted external gateway if traffic is UDP")
				updateAPBExternalRouteCRWithStaticHop(defaultPolicyName, f.Namespace.Name, false, addresses.gatewayIPs[0])

				podConnEntriesWithMACLabelsSet = 1 // we still have the conntrack entry for the remaining gateway
				totalPodConnEntries = 5            // 6-1

				gomega.Eventually(func() int {
					n := pokeConntrackEntries(nodeName, addresses.srcPodIP, protocol, macAddressGW)
					klog.Infof("Number of entries with macAddressGW %s:%d", macAddressGW, n)
					return n
				}, 10).Should(gomega.Equal(podConnEntriesWithMACLabelsSet))

				gomega.Expect(pokeConntrackEntries(nodeName, addresses.srcPodIP, protocol, nil)).To(gomega.Equal(totalPodConnEntries))

				ginkgo.By("Remove the remaining static hop from the CR")
				deleteAPBExternalRouteCR(defaultPolicyName)
				ginkgo.By("Check if conntrack entries for ECMP routes are removed for the deleted external gateway if traffic is UDP")

				podConnEntriesWithMACLabelsSet = 0 // we don't have any remaining gateways left
				totalPodConnEntries = 4            // 6-2

				gomega.Eventually(func() int {
					n := pokeConntrackEntries(nodeName, addresses.srcPodIP, protocol, macAddressGW)
					klog.Infof("Number of entries with macAddressGW %s:%d", macAddressGW, n)
					return n
				}, time.Minute, 5).Should(gomega.Equal(podConnEntriesWithMACLabelsSet))

				gomega.Expect(pokeConntrackEntries(nodeName, addresses.srcPodIP, protocol, nil)).To(gomega.Equal(totalPodConnEntries))
			},
				ginkgo.Entry("IPV4 udp", &addressesv4, "udp"),
				ginkgo.Entry("IPV4 tcp", &addressesv4, "tcp"),
				ginkgo.Entry("IPV6 udp", &addressesv6, "udp"),
				ginkgo.Entry("IPV6 tcp", &addressesv6, "tcp"))

			ginkgo.DescribeTable("Dynamic Hop: Should validate conntrack entry deletion for TCP/UDP traffic via multiple external gateways a.k.a ECMP routes", func(addresses *gatewayTestIPs, protocol string) {
				if addresses.srcPodIP == "" || addresses.nodeIP == "" {
					skipper.Skipf("Skipping as pod ip / node ip are not set pod ip %s node ip %s", addresses.srcPodIP, addresses.nodeIP)
				}

				for i, gwPod := range []string{gatewayPodName1, gatewayPodName2} {
					annotateMultusNetworkStatusInPodGateway(gwPod, servingNamespace, []string{addresses.gatewayIPs[i], addresses.gatewayIPs[i]})
				}

				createAPBExternalRouteCRWithDynamicHop(defaultPolicyName, f.Namespace.Name, servingNamespace, false, addresses.gatewayIPs)

				setupIperf3Client := func(container, address string, port int) {
					// note iperf3 even when using udp also spawns tcp connection first; so we indirectly also have the tcp connection when using "-u" flag
					cmd := []string{containerRuntime, "exec", container, "iperf3", "-u", "-c", address, "-p", fmt.Sprintf("%d", port), "-b", "1M", "-i", "1", "-t", "3", "&"}
					klog.Infof("Run command %+v", cmd)
					_, err := runCommand(cmd...)
					framework.ExpectNoError(err, "failed to setup iperf3 client for %s", container)
				}
				macAddressGW := make([]string, 2)
				for i, containerName := range []string{gwContainer1, gwContainer2} {
					ginkgo.By("Start iperf3 client from external container to connect to iperf3 server running at the src pod")
					setupIperf3Client(containerName, addresses.srcPodIP, 5201+i)
					macAddressExtGW, err := net.ParseMAC(getMACAddressesForNetwork(containerName, externalContainerNetwork))
					framework.ExpectNoError(err, "failed to parse MAC address for %s", containerName)
					// Trim leading 0s because conntrack dumped labels are just integers
					// in hex without leading 0s.
					macAddressGW[i] = strings.TrimLeft(strings.Replace(macAddressExtGW.String(), ":", "", -1), "0")
				}

				ginkgo.By("Check if conntrack entries for ECMP routes are created for the 2 external gateways")
				nodeName := getPod(f, srcPodName).Spec.NodeName
				podConnEntriesWithMACLabelsSet := 2 // TCP
				totalPodConnEntries := 6            // TCP

				gomega.Eventually(func() int {
					n := pokeConntrackEntries(nodeName, addresses.srcPodIP, protocol, macAddressGW)
					klog.Infof("Number of entries with macAddressGW %s:%d", macAddressGW, n)
					return n
				}, time.Minute, 5).Should(gomega.Equal(podConnEntriesWithMACLabelsSet))
				gomega.Expect(pokeConntrackEntries(nodeName, addresses.srcPodIP, protocol, nil)).To(gomega.Equal(totalPodConnEntries)) // total conntrack entries for this pod/protocol

				ginkgo.By("Remove second external gateway pod's routing-namespace annotation")
				p := getGatewayPod(f, servingNamespace, gatewayPodName2)
				p.Labels = map[string]string{"name": gatewayPodName2}
				updatePod(f, p)

				ginkgo.By("Check if conntrack entries for ECMP routes are removed for the deleted external gateway if traffic is UDP")

				podConnEntriesWithMACLabelsSet = 1 // we still have the conntrack entry for the remaining gateway
				totalPodConnEntries = 5            // 6-1

				gomega.Eventually(func() int {
					n := pokeConntrackEntries(nodeName, addresses.srcPodIP, protocol, macAddressGW)
					klog.Infof("Number of entries with macAddressGW %s:%d", macAddressGW, n)
					return n
				}, 10).Should(gomega.Equal(podConnEntriesWithMACLabelsSet))
				gomega.Expect(pokeConntrackEntries(nodeName, addresses.srcPodIP, protocol, nil)).To(gomega.Equal(totalPodConnEntries))

				ginkgo.By("Remove first external gateway pod's routing-namespace annotation")
				p = getGatewayPod(f, servingNamespace, gatewayPodName1)
				p.Labels = map[string]string{"name": gatewayPodName1}
				updatePod(f, p)

				ginkgo.By("Check if conntrack entries for ECMP routes are removed for the deleted external gateway if traffic is UDP")

				podConnEntriesWithMACLabelsSet = 0 //we don't have any remaining gateways left
				totalPodConnEntries = 4
				gomega.Eventually(func() int {
					n := pokeConntrackEntries(nodeName, addresses.srcPodIP, protocol, macAddressGW)
					klog.Infof("Number of entries with macAddressGW %s:%d", macAddressGW, n)
					return n
				}, 5).Should(gomega.Equal(podConnEntriesWithMACLabelsSet))
				gomega.Expect(pokeConntrackEntries(nodeName, addresses.srcPodIP, protocol, nil)).To(gomega.Equal(totalPodConnEntries))
				checkAPBExternalRouteStatus(defaultPolicyName)
			},
				ginkgo.Entry("IPV4 udp", &addressesv4, "udp"),
				ginkgo.Entry("IPV4 tcp", &addressesv4, "tcp"),
				ginkgo.Entry("IPV6 udp", &addressesv6, "udp"),
				ginkgo.Entry("IPV6 tcp", &addressesv6, "tcp"))

		})

		// BFD Tests are dual of external gateway. The only difference is that they enable BFD on ovn and
		// on the external containers, and after doing one round veryfing that the traffic reaches both containers,
		// they delete one and verify that the traffic is always reaching the only alive container.
		var _ = ginkgo.Context("BFD", func() {

			var _ = ginkgo.Describe("e2e non-vxlan external gateway through a dynamic hop", func() {
				const (
					svcname           string = "externalgw-pod-novxlan"
					gwContainer1      string = "ex-gw-container1"
					gwContainer2      string = "ex-gw-container2"
					srcPingPodName    string = "e2e-exgw-src-ping-pod"
					gatewayPodName1   string = "e2e-gateway-pod1"
					gatewayPodName2   string = "e2e-gateway-pod2"
					externalTCPPort          = 91
					externalUDPPort          = 90
					ecmpRetry         int    = 20
					testTimeout       string = "20"
					defaultPolicyName        = "default-route-policy"
				)

				var (
					sleepCommand             = []string{"bash", "-c", "sleep 20000"}
					addressesv4, addressesv6 gatewayTestIPs
					clientSet                kubernetes.Interface
					servingNamespace         string
					gwContainers             []string
				)

				f := wrappedTestFramework(svcname)

				ginkgo.BeforeEach(func() {
					clientSet = f.ClientSet // so it can be used in AfterEach
					// retrieve worker node names
					nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), f.ClientSet, 3)
					framework.ExpectNoError(err)
					if len(nodes.Items) < 3 {
						framework.Failf(
							"Test requires >= 3 Ready nodes, but there are only %v nodes",
							len(nodes.Items))
					}

					ns, err := f.CreateNamespace(context.TODO(), "exgw-bfd-serving", nil)
					framework.ExpectNoError(err)
					servingNamespace = ns.Name

					setupBFD := setupBFDOnContainer(nodes.Items)
					gwContainers, addressesv4, addressesv6 = setupGatewayContainers(f, nodes, gwContainer1, gwContainer2, srcPingPodName, externalUDPPort, externalTCPPort, ecmpRetry, setupBFD)
					ginkgo.By("Create the external route policy with dynamic hops to manage the src app pod namespace")

					setupPolicyBasedGatewayPods(f, nodes, gatewayPodName1, gatewayPodName2, servingNamespace, sleepCommand, addressesv4, addressesv6)
				})

				ginkgo.AfterEach(func() {
					deleteAPBExternalRouteCR(defaultPolicyName)
					cleanExGWContainers(clientSet, []string{gwContainer1, gwContainer2}, addressesv4, addressesv6)
				})

				ginkgo.DescribeTable("Should validate ICMP connectivity to an external gateway's loopback address via a pod with dynamic hop",
					func(addresses *gatewayTestIPs, icmpCommand string) {
						if addresses.srcPodIP == "" || addresses.nodeIP == "" {
							skipper.Skipf("Skipping as pod ip / node ip are not set pod ip %s node ip %s", addresses.srcPodIP, addresses.nodeIP)
						}
						createAPBExternalRouteCRWithDynamicHop(defaultPolicyName, f.Namespace.Name, servingNamespace, true, addressesv4.gatewayIPs)

						ginkgo.By("Verifying connectivity to the pod from external gateways")
						for _, gwContainer := range gwContainers {
							// Ping from a common IP address that exists on both gateways to ensure test coverage where ingress reply goes back to the same host.
							_, err := runCommand(containerRuntime, "exec", gwContainer, "ping", "-I", addresses.targetIPs[0], "-c", testTimeout, addresses.srcPodIP)
							framework.ExpectNoError(err, "Failed to ping %s from container %s", addresses.srcPodIP, gwContainer)
						}

						// This is needed for bfd to sync up
						for _, gwContainer := range gwContainers {
							gomega.Eventually(func() bool {
								return isBFDPaired(gwContainer, addresses.nodeIP)
							}, time.Minute, 5).Should(gomega.BeTrue(), "Bfd not paired")
						}

						tcpDumpSync := sync.WaitGroup{}
						tcpDumpSync.Add(len(gwContainers))
						for _, gwContainer := range gwContainers {
							go checkReceivedPacketsOnContainer(gwContainer, srcPingPodName, anyLink, []string{icmpCommand}, &tcpDumpSync)
						}

						// Verify the external gateway loopback address running on the external container is reachable and
						// that traffic from the source ping pod is proxied through the pod in the serving namespace
						ginkgo.By("Verifying connectivity via the gateway namespace to the remote addresses")

						pingSync := sync.WaitGroup{}
						// spawn a goroutine to asynchronously (to speed up the test)
						// to ping the gateway loopbacks on both containers via ECMP.
						for _, address := range addresses.targetIPs {
							pingSync.Add(1)
							go func(target string) {
								defer ginkgo.GinkgoRecover()
								defer pingSync.Done()
								_, err := e2ekubectl.RunKubectl(f.Namespace.Name, "exec", srcPingPodName, "--", "ping", "-c", testTimeout, target)
								if err != nil {
									framework.Logf("error generating a ping from the test pod %s: %v", srcPingPodName, err)
								}
							}(address)
						}

						pingSync.Wait()
						tcpDumpSync.Wait()

						if len(gwContainers) > 1 {
							ginkgo.By("Deleting one container")
							deleteClusterExternalContainer(gwContainers[1])
							time.Sleep(3 * time.Second) // bfd timeout

							tcpDumpSync = sync.WaitGroup{}
							tcpDumpSync.Add(1)
							go checkReceivedPacketsOnContainer(gwContainers[0], srcPingPodName, anyLink, []string{icmpCommand}, &tcpDumpSync)

							// Verify the external gateway loopback address running on the external container is reachable and
							// that traffic from the source ping pod is proxied through the pod in the serving namespace
							ginkgo.By("Verifying connectivity via the gateway namespace to the remote addresses")
							pingSync = sync.WaitGroup{}

							for _, t := range addresses.targetIPs {
								pingSync.Add(1)
								go func(target string) {
									defer ginkgo.GinkgoRecover()
									defer pingSync.Done()
									_, err := e2ekubectl.RunKubectl(f.Namespace.Name, "exec", srcPingPodName, "--", "ping", "-c", testTimeout, target)
									framework.ExpectNoError(err, "Failed to ping remote gateway %s from pod %s", target, srcPingPodName)
								}(t)
							}
							pingSync.Wait()
							tcpDumpSync.Wait()
						}
						checkAPBExternalRouteStatus(defaultPolicyName)
					},
					ginkgo.Entry("ipv4", &addressesv4, "icmp"),
					ginkgo.Entry("ipv6", &addressesv6, "icmp6"))

				ginkgo.DescribeTable("Should validate TCP/UDP connectivity to an external gateway's loopback address via a pod with a dynamic hop",
					func(protocol string, addresses *gatewayTestIPs, destPort int) {
						if addresses.srcPodIP == "" || addresses.nodeIP == "" {
							skipper.Skipf("Skipping as pod ip / node ip are not set pod ip %s node ip %s", addresses.srcPodIP, addresses.nodeIP)
						}
						createAPBExternalRouteCRWithDynamicHop(defaultPolicyName, f.Namespace.Name, servingNamespace, true, addressesv4.gatewayIPs)

						for _, gwContainer := range gwContainers {
							_, err := runCommand(containerRuntime, "exec", gwContainer, "ping", "-c", testTimeout, addresses.srcPodIP)
							framework.ExpectNoError(err, "Failed to ping %s from container %s", addresses.srcPodIP, gwContainer)
						}

						for _, gwContainer := range gwContainers {
							gomega.Eventually(func() bool {
								return isBFDPaired(gwContainer, addresses.nodeIP)
							}, 10, 1).Should(gomega.BeTrue(), "Bfd not paired")
						}

						expectedHostNames := hostNamesForContainers(gwContainers)
						framework.Logf("Expected hostnames are %v", expectedHostNames)

						returnedHostNames := make(map[string]struct{})
						target := addresses.targetIPs[0]
						success := false
						for i := 0; i < 20; i++ {
							hostname := pokeHostnameViaNC(srcPingPodName, f.Namespace.Name, protocol, target, destPort)
							if hostname != "" {
								returnedHostNames[hostname] = struct{}{}
							}

							if cmp.Equal(returnedHostNames, expectedHostNames) {
								success = true
								break
							}
						}
						framework.Logf("Received hostnames for protocol %s are %v ", protocol, returnedHostNames)

						if !success {
							framework.Failf("Failed to hit all the external gateways via for protocol %s, diff %s", protocol, cmp.Diff(expectedHostNames, returnedHostNames))
						}

						if len(gwContainers) > 1 {
							ginkgo.By("Deleting one container")
							deleteClusterExternalContainer(gwContainers[1])
							ginkgo.By("Waiting for BFD to sync")
							time.Sleep(3 * time.Second) // bfd timeout

							// ECMP should direct all the traffic to the only container
							expectedHostName := hostNameForContainer(gwContainers[0])

							ginkgo.By("Checking hostname multiple times")
							for i := 0; i < 20; i++ {
								hostname := pokeHostnameViaNC(srcPingPodName, f.Namespace.Name, protocol, target, destPort)
								gomega.Expect(expectedHostName).To(gomega.Equal(hostname), "Hostname returned by nc not as expected")
							}
						}
						checkAPBExternalRouteStatus(defaultPolicyName)
					},
					ginkgo.Entry("UDP ipv4", "udp", &addressesv4, externalUDPPort),
					ginkgo.Entry("TCP ipv4", "tcp", &addressesv4, externalTCPPort),
					ginkgo.Entry("UDP ipv6", "udp", &addressesv6, externalUDPPort),
					ginkgo.Entry("TCP ipv6", "tcp", &addressesv6, externalTCPPort))
			})

			// Validate pods can reach a network running in multiple container's loopback
			// addresses via two external gateways running on eth0 of the container without
			// any tunnel encap. This test defines two external gateways and validates ECMP
			// functionality to the container loopbacks. To verify traffic reaches the
			// gateways, tcpdump is running on the external gateways and will exit successfully
			// once an ICMP packet is received from the annotated pod in the k8s cluster.
			// Two additional gateways are added to verify the tcp / udp protocols.
			// They run the netexec command, and the pod asks to return their hostname.
			// The test checks that both hostnames are collected at least once.
			var _ = ginkgo.Describe("e2e multiple external gateway validation", func() {
				const (
					svcname         string = "novxlan-externalgw-ecmp"
					gwContainer1    string = "gw-test-container1"
					gwContainer2    string = "gw-test-container2"
					testTimeout     string = "30"
					ecmpRetry       int    = 20
					srcPodName             = "e2e-exgw-src-pod"
					externalTCPPort        = 80
					externalUDPPort        = 90
				)

				var (
					gwContainers []string
				)

				testContainer := fmt.Sprintf("%s-container", srcPodName)
				testContainerFlag := fmt.Sprintf("--container=%s", testContainer)

				f := wrappedTestFramework(svcname)

				var addressesv4, addressesv6 gatewayTestIPs

				ginkgo.BeforeEach(func() {
					nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), f.ClientSet, 3)
					framework.ExpectNoError(err)
					if len(nodes.Items) < 3 {
						framework.Failf(
							"Test requires >= 3 Ready nodes, but there are only %v nodes",
							len(nodes.Items))
					}

					if externalContainerNetwork == "host" {
						skipper.Skipf("Skipping as host network doesn't support multiple external gateways")
					}

					setupBFD := setupBFDOnContainer(nodes.Items)
					gwContainers, addressesv4, addressesv6 = setupGatewayContainers(f, nodes, gwContainer1, gwContainer2, srcPodName, externalUDPPort, externalTCPPort, ecmpRetry, setupBFD)

				})

				ginkgo.AfterEach(func() {
					deleteClusterExternalContainer(gwContainer1)
					deleteClusterExternalContainer(gwContainer2)
					deleteAPBExternalRouteCR(defaultPolicyName)
				})

				ginkgo.DescribeTable("Should validate ICMP connectivity to multiple external gateways for an ECMP scenario", func(addresses *gatewayTestIPs, icmpToDump string) {
					if addresses.srcPodIP == "" || addresses.nodeIP == "" {
						skipper.Skipf("Skipping as pod ip / node ip are not set pod ip %s node ip %s", addresses.srcPodIP, addresses.nodeIP)
					}
					createAPBExternalRouteCRWithStaticHop(defaultPolicyName, f.Namespace.Name, true, addresses.gatewayIPs...)

					for _, gwContainer := range gwContainers {
						_, err := runCommand(containerRuntime, "exec", gwContainer, "ping", "-c", testTimeout, addresses.srcPodIP)
						framework.ExpectNoError(err, "Failed to ping %s from container %s", addresses.srcPodIP, gwContainer)
					}

					for _, gwContainer := range gwContainers {
						gomega.Eventually(func() bool {
							return isBFDPaired(gwContainer, addresses.nodeIP)
						}, 5).Should(gomega.BeTrue(), "Bfd not paired")
					}

					// Verify the gateways and remote loopback addresses are reachable from the pod.
					// Iterate checking connectivity to the loopbacks on the gateways until tcpdump see
					// the traffic or 20 attempts fail. Odds of a false negative here is ~ (1/2)^20
					ginkgo.By("Verifying ecmp connectivity to the external gateways by iterating through the targets")

					// Check for egress traffic to both gateway loopback addresses using tcpdump, since
					// /proc/net/dev counters only record the ingress interface traffic is received on.
					// The test will waits until an ICMP packet is matched on the gateways or fail the
					// test if a packet to the loopback is not received within the timer interval.
					// If an ICMP packet is never detected, return the error via the specified chanel.

					tcpDumpSync := sync.WaitGroup{}
					tcpDumpSync.Add(len(gwContainers))
					for _, gwContainer := range gwContainers {
						go checkReceivedPacketsOnContainer(gwContainer, srcPodName, anyLink, []string{icmpToDump}, &tcpDumpSync)
					}

					// spawn a goroutine to asynchronously (to speed up the test)
					// to ping the gateway loopbacks on both containers via ECMP.

					pingSync := sync.WaitGroup{}

					// spawn a goroutine to asynchronously (to speed up the test)
					// to ping the gateway loopbacks on both containers via ECMP.
					for _, address := range addresses.targetIPs {
						pingSync.Add(1)
						go func(target string) {
							defer ginkgo.GinkgoRecover()
							defer pingSync.Done()
							_, err := e2ekubectl.RunKubectl(f.Namespace.Name, "exec", srcPodName, testContainerFlag, "--", "ping", "-c", testTimeout, target)
							if err != nil {
								framework.Logf("error generating a ping from the test pod %s: %v", srcPodName, err)
							}
						}(address)
					}

					pingSync.Wait()
					tcpDumpSync.Wait()

					ginkgo.By("Deleting one container")
					deleteClusterExternalContainer(gwContainers[1])
					time.Sleep(3 * time.Second) // bfd timeout

					pingSync = sync.WaitGroup{}
					tcpDumpSync = sync.WaitGroup{}

					tcpDumpSync.Add(1)
					go checkReceivedPacketsOnContainer(gwContainers[0], srcPodName, anyLink, []string{icmpToDump}, &tcpDumpSync)

					// spawn a goroutine to asynchronously (to speed up the test)
					// to ping the gateway loopbacks on both containers via ECMP.
					for _, address := range addresses.targetIPs {
						pingSync.Add(1)
						go func(target string) {
							defer ginkgo.GinkgoRecover()
							defer pingSync.Done()
							_, err := e2ekubectl.RunKubectl(f.Namespace.Name, "exec", srcPodName, testContainerFlag, "--", "ping", "-c", testTimeout, target)
							if err != nil {
								framework.Logf("error generating a ping from the test pod %s: %v", srcPodName, err)
							}
						}(address)
					}

					pingSync.Wait()
					tcpDumpSync.Wait()

				}, ginkgo.Entry("IPV4", &addressesv4, "icmp"),
					ginkgo.Entry("IPV6", &addressesv6, "icmp6"))

				// This test runs a listener on the external container, returning the host name both on tcp and udp.
				// The src pod tries to hit the remote address until both the containers are hit.
				ginkgo.DescribeTable("Should validate TCP/UDP connectivity to multiple external gateways for a UDP / TCP scenario", func(addresses *gatewayTestIPs, protocol string, destPort int) {
					if addresses.srcPodIP == "" || addresses.nodeIP == "" {
						skipper.Skipf("Skipping as pod ip / node ip are not set pod ip %s node ip %s", addresses.srcPodIP, addresses.nodeIP)
					}
					createAPBExternalRouteCRWithStaticHop(defaultPolicyName, f.Namespace.Name, true, addresses.gatewayIPs...)

					for _, gwContainer := range gwContainers {
						_, err := runCommand(containerRuntime, "exec", gwContainer, "ping", "-c", testTimeout, addresses.srcPodIP)
						framework.ExpectNoError(err, "Failed to ping %s from container %s", addresses.srcPodIP, gwContainer)
					}

					// This is needed for bfd to sync up
					time.Sleep(3 * time.Second)

					for _, gwContainer := range gwContainers {
						gomega.Expect(isBFDPaired(gwContainer, addresses.nodeIP)).To(gomega.Equal(true), "Bfd not paired")
					}

					expectedHostNames := hostNamesForContainers(gwContainers)
					framework.Logf("Expected hostnames are %v", expectedHostNames)

					returnedHostNames := make(map[string]struct{})
					success := false

					// Picking only the first address, the one the udp listener is set for
					target := addresses.targetIPs[0]
					for i := 0; i < 20; i++ {
						hostname := pokeHostnameViaNC(srcPodName, f.Namespace.Name, protocol, target, destPort)
						if hostname != "" {
							returnedHostNames[hostname] = struct{}{}
						}
						if cmp.Equal(returnedHostNames, expectedHostNames) {
							success = true
							break
						}
					}

					framework.Logf("Received hostnames for protocol %s are %v ", protocol, returnedHostNames)

					if !success {
						framework.Failf("Failed to hit all the external gateways via for protocol %s, diff %s", protocol, cmp.Diff(expectedHostNames, returnedHostNames))
					}

					ginkgo.By("Deleting one container")
					deleteClusterExternalContainer(gwContainers[1])
					ginkgo.By("Waiting for BFD to sync")
					time.Sleep(3 * time.Second) // bfd timeout

					// ECMP should direct all the traffic to the only container
					expectedHostName := hostNameForContainer(gwContainers[0])

					ginkgo.By("Checking hostname multiple times")
					for i := 0; i < 20; i++ {
						hostname := pokeHostnameViaNC(srcPodName, f.Namespace.Name, protocol, target, destPort)
						gomega.Expect(expectedHostName).To(gomega.Equal(hostname), "Hostname returned by nc not as expected")
					}
				}, ginkgo.Entry("IPV4 udp", &addressesv4, "udp", externalUDPPort),
					ginkgo.Entry("IPV4 tcp", &addressesv4, "tcp", externalTCPPort),
					ginkgo.Entry("IPV6 udp", &addressesv6, "udp", externalUDPPort),
					ginkgo.Entry("IPV6 tcp", &addressesv6, "tcp", externalTCPPort))
			})
		})
	})

	var _ = ginkgo.Context("When migrating from Annotations to Admin Policy Based External Route CRs", func() {
		// Validate pods can reach a network running in a container's looback address via
		// an external gateway running on eth0 of the container without any tunnel encap.
		// The traffic will get proxied through an annotated pod in the serving namespace.
		var _ = ginkgo.Describe("e2e non-vxlan external gateway through a gateway pod", func() {
			const (
				svcname         string = "externalgw-pod-novxlan"
				gwContainer1    string = "ex-gw-container1"
				gwContainer2    string = "ex-gw-container2"
				srcPingPodName  string = "e2e-exgw-src-ping-pod"
				gatewayPodName1 string = "e2e-gateway-pod1"
				gatewayPodName2 string = "e2e-gateway-pod2"
				externalTCPPort        = 91
				externalUDPPort        = 90
				ecmpRetry       int    = 20
				testTimeout     string = "20"
			)

			var (
				sleepCommand             = []string{"bash", "-c", "sleep 20000"}
				addressesv4, addressesv6 gatewayTestIPs
				clientSet                kubernetes.Interface
				servingNamespace         string
				gwContainers             []string
			)

			f := wrappedTestFramework(svcname)

			ginkgo.BeforeEach(func() {
				clientSet = f.ClientSet // so it can be used in AfterEach
				// retrieve worker node names
				nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), f.ClientSet, 3)
				framework.ExpectNoError(err)
				if len(nodes.Items) < 3 {
					framework.Failf(
						"Test requires >= 3 Ready nodes, but there are only %v nodes",
						len(nodes.Items))
				}

				ns, err := f.CreateNamespace(context.TODO(), "exgw-serving", nil)
				framework.ExpectNoError(err)
				servingNamespace = ns.Name

				gwContainers, addressesv4, addressesv6 = setupGatewayContainers(f, nodes, gwContainer1, gwContainer2, srcPingPodName, externalUDPPort, externalTCPPort, ecmpRetry)
				setupAnnotatedGatewayPods(f, nodes, gatewayPodName1, gatewayPodName2, servingNamespace, sleepCommand, addressesv4, addressesv6, false)
			})

			ginkgo.AfterEach(func() {
				cleanExGWContainers(clientSet, []string{gwContainer1, gwContainer2}, addressesv4, addressesv6)
				deleteAPBExternalRouteCR(defaultPolicyName)
				resetGatewayAnnotations(f)
			})

			ginkgo.DescribeTable("Should validate ICMP connectivity to an external gateway's loopback address via a pod with external gateway annotations and a policy CR and after the annotations are removed",
				func(addresses *gatewayTestIPs, icmpCommand string) {
					if addresses.srcPodIP == "" || addresses.nodeIP == "" {
						skipper.Skipf("Skipping as pod ip / node ip are not set pod ip %s node ip %s", addresses.srcPodIP, addresses.nodeIP)
					}

					createAPBExternalRouteCRWithDynamicHop(defaultPolicyName, f.Namespace.Name, servingNamespace, false, addressesv4.gatewayIPs)
					ginkgo.By("Remove gateway annotations in pods")
					annotatePodForGateway(gatewayPodName2, servingNamespace, "", addresses.gatewayIPs[1], false)
					annotatePodForGateway(gatewayPodName1, servingNamespace, "", addresses.gatewayIPs[0], false)
					ginkgo.By("Validate ICMP connectivity again with only CR policy to support it")
					ginkgo.By(fmt.Sprintf("Verifying connectivity to the pod [%s] from external gateways", addresses.srcPodIP))
					for _, gwContainer := range gwContainers {
						// Ping from a common IP address that exists on both gateways to ensure test coverage where ingress reply goes back to the same host.
						_, err := runCommand(containerRuntime, "exec", gwContainer, "ping", "-I", addresses.targetIPs[0], "-c", testTimeout, addresses.srcPodIP)
						framework.ExpectNoError(err, "Failed to ping %s from container %s", addresses.srcPodIP, gwContainer)
					}
					tcpDumpSync := sync.WaitGroup{}
					tcpDumpSync.Add(len(gwContainers))

					for _, gwContainer := range gwContainers {
						go checkReceivedPacketsOnContainer(gwContainer, srcPingPodName, anyLink, []string{icmpCommand}, &tcpDumpSync)
					}

					// Verify the external gateway loopback address running on the external container is reachable and
					// that traffic from the source ping pod is proxied through the pod in the serving namespace
					ginkgo.By("Verifying connectivity via the gateway namespace to the remote addresses")
					pingSync := sync.WaitGroup{}
					for _, t := range addresses.targetIPs {
						pingSync.Add(1)
						go func(target string) {
							defer ginkgo.GinkgoRecover()
							defer pingSync.Done()
							_, err := e2ekubectl.RunKubectl(f.Namespace.Name, "exec", srcPingPodName, "--", "ping", "-c", testTimeout, target)
							framework.ExpectNoError(err, "Failed to ping remote gateway %s from pod %s", target, srcPingPodName)
						}(t)
					}
					pingSync.Wait()
					tcpDumpSync.Wait()
					checkAPBExternalRouteStatus(defaultPolicyName)
				},
				ginkgo.Entry("ipv4", &addressesv4, "icmp"))

			ginkgo.DescribeTable("Should validate TCP/UDP connectivity to an external gateway's loopback "+
				"address via a pod when deleting the annotation and supported by a CR with the same gateway IPs",
				func(protocol string, addresses *gatewayTestIPs, destPort, destPortOnPod int) {
					if addresses.srcPodIP == "" || addresses.nodeIP == "" {
						skipper.Skipf("Skipping as pod ip / node ip are not set pod ip %s node ip %s", addresses.srcPodIP, addresses.nodeIP)
					}
					createAPBExternalRouteCRWithDynamicHop(defaultPolicyName, f.Namespace.Name, servingNamespace, false, addressesv4.gatewayIPs)
					ginkgo.By("removing the annotations in the pod gateways")
					annotatePodForGateway(gatewayPodName2, servingNamespace, "", addresses.gatewayIPs[1], false)
					annotatePodForGateway(gatewayPodName1, servingNamespace, "", addresses.gatewayIPs[0], false)

					for _, container := range gwContainers {
						reachPodFromGateway(addresses.srcPodIP, strconv.Itoa(destPortOnPod), srcPingPodName, container, protocol)
					}

					expectedHostNames := make(map[string]struct{})
					for _, c := range gwContainers {
						res, err := runCommand(containerRuntime, "exec", c, "hostname")
						framework.ExpectNoError(err, "failed to run hostname in %s", c)
						hostname := strings.TrimSuffix(res, "\n")
						framework.Logf("Hostname for %s is %s", c, hostname)
						expectedHostNames[hostname] = struct{}{}
					}
					framework.Logf("Expected hostnames are %v", expectedHostNames)

					ginkgo.By("Checking that external ips are reachable with both gateways")
					returnedHostNames := make(map[string]struct{})
					target := addresses.targetIPs[0]
					success := false
					for i := 0; i < 20; i++ {
						args := []string{"exec", srcPingPodName, "--"}
						if protocol == "tcp" {
							args = append(args, "bash", "-c", fmt.Sprintf("echo | nc -w 1 %s %d", target, destPort))
						} else {
							args = append(args, "bash", "-c", fmt.Sprintf("echo | nc -w 1 -u %s %d", target, destPort))
						}
						res, err := e2ekubectl.RunKubectl(f.Namespace.Name, args...)
						framework.ExpectNoError(err, "failed to reach %s (%s)", target, protocol)
						hostname := strings.TrimSuffix(res, "\n")
						if hostname != "" {
							returnedHostNames[hostname] = struct{}{}
						}

						if cmp.Equal(returnedHostNames, expectedHostNames) {
							success = true
							break
						}
					}
					framework.Logf("Received hostnames for protocol %s are %v ", protocol, returnedHostNames)

					if !success {
						framework.Failf("Failed to hit all the external gateways via for protocol %s, diff %s", protocol, cmp.Diff(expectedHostNames, returnedHostNames))
					}
					checkAPBExternalRouteStatus(defaultPolicyName)
				},
				ginkgo.Entry("UDP ipv4", "udp", &addressesv4, externalUDPPort, srcUDPPort),
				ginkgo.Entry("TCP ipv4", "tcp", &addressesv4, externalTCPPort, srcHTTPPort),
				ginkgo.Entry("UDP ipv6", "udp", &addressesv6, externalUDPPort, srcUDPPort),
				ginkgo.Entry("TCP ipv6", "tcp", &addressesv6, externalTCPPort, srcHTTPPort))
		})

		var _ = ginkgo.Describe("e2e multiple external gateway stale conntrack entry deletion validation", func() {
			const (
				svcname         string = "novxlan-externalgw-ecmp"
				gwContainer1    string = "gw-test-container1"
				gwContainer2    string = "gw-test-container2"
				srcPodName      string = "e2e-exgw-src-pod"
				gatewayPodName1 string = "e2e-gateway-pod1"
				gatewayPodName2 string = "e2e-gateway-pod2"
			)

			var (
				servingNamespace         string
				addressesv4, addressesv6 gatewayTestIPs
				sleepCommand             []string
				nodes                    *v1.NodeList
				err                      error
				clientSet                kubernetes.Interface
			)

			f := wrappedTestFramework(svcname)

			ginkgo.BeforeEach(func() {
				clientSet = f.ClientSet // so it can be used in AfterEach
				// retrieve worker node names
				nodes, err = e2enode.GetBoundedReadySchedulableNodes(context.TODO(), clientSet, 3)
				framework.ExpectNoError(err)
				if len(nodes.Items) < 3 {
					framework.Failf(
						"Test requires >= 3 Ready nodes, but there are only %v nodes",
						len(nodes.Items))
				}

				if externalContainerNetwork == "host" {
					skipper.Skipf("Skipping as host network doesn't support multiple external gateways")
				}

				ns, err := f.CreateNamespace(context.TODO(), "exgw-conntrack-serving", nil)
				framework.ExpectNoError(err)
				servingNamespace = ns.Name

				addressesv4, addressesv6 = setupGatewayContainersForConntrackTest(f, nodes, gwContainer1, gwContainer2, srcPodName)
				sleepCommand = []string{"bash", "-c", "sleep 20000"}
				_, err = createGenericPodWithLabel(f, gatewayPodName1, nodes.Items[0].Name, servingNamespace, sleepCommand, map[string]string{"gatewayPod": "true"})
				framework.ExpectNoError(err, "Create and annotate the external gw pods to manage the src app pod namespace, failed: %v", err)
				_, err = createGenericPodWithLabel(f, gatewayPodName2, nodes.Items[1].Name, servingNamespace, sleepCommand, map[string]string{"gatewayPod": "true"})
				framework.ExpectNoError(err, "Create and annotate the external gw pods to manage the src app pod namespace, failed: %v", err)
			})

			ginkgo.AfterEach(func() {
				// tear down the containers and pods simulating the gateways
				ginkgo.By("Deleting the gateway containers")
				deleteClusterExternalContainer(gwContainer1)
				deleteClusterExternalContainer(gwContainer2)
				deleteAPBExternalRouteCR(defaultPolicyName)
				resetGatewayAnnotations(f)
			})

			ginkgo.DescribeTable("Namespace annotation: Should validate conntrack entry remains unchanged when deleting the annotation in the namespace while the CR static hop still references the same namespace in the policy", func(addresses *gatewayTestIPs, protocol string) {
				if addresses.srcPodIP == "" || addresses.nodeIP == "" {
					skipper.Skipf("Skipping as pod ip / node ip are not set pod ip %s node ip %s", addresses.srcPodIP, addresses.nodeIP)
				}
				ginkgo.By("Annotate the app namespace to get managed by external gateways")
				annotateNamespaceForGateway(f.Namespace.Name, false, addresses.gatewayIPs...)
				createAPBExternalRouteCRWithStaticHop(defaultPolicyName, f.Namespace.Name, false, addresses.gatewayIPs...)

				setupIperf3Client := func(container, address string, port int) {
					// note iperf3 even when using udp also spawns tcp connection first; so we indirectly also have the tcp connection when using "-u" flag
					cmd := []string{containerRuntime, "exec", container, "iperf3", "-u", "-c", address, "-p", fmt.Sprintf("%d", port), "-b", "1M", "-i", "1", "-t", "3", "&"}
					_, err := runCommand(cmd...)
					framework.ExpectNoError(err, "failed to setup iperf3 client for %s", container)
				}
				macAddressGW := make([]string, 2)
				for i, containerName := range []string{gwContainer1, gwContainer2} {
					ginkgo.By("Start iperf3 client from external container to connect to iperf3 server running at the src pod")
					setupIperf3Client(containerName, addresses.srcPodIP, 5201+i)
					macAddressExtGW, err := net.ParseMAC(getMACAddressesForNetwork(containerName, externalContainerNetwork))
					framework.ExpectNoError(err, "failed to parse MAC address for %s", containerName)
					// Trim leading 0s because conntrack dumped labels are just integers
					// in hex without leading 0s.
					macAddressGW[i] = strings.TrimLeft(strings.Replace(macAddressExtGW.String(), ":", "", -1), "0")
				}

				nodeName := getPod(f, srcPodName).Spec.NodeName
				expectedTotalEntries := 6
				expectedMACEntries := 2
				ginkgo.By("Check to ensure initial conntrack entries are 2 mac address label, and 6 total entries")
				gomega.Eventually(func() int {
					n := pokeConntrackEntries(nodeName, addresses.srcPodIP, protocol, macAddressGW)
					klog.Infof("Number of entries with macAddressGW %s:%d", macAddressGW, n)
					return n
				}, time.Minute, 5).Should(gomega.Equal(expectedMACEntries))
				gomega.Expect(pokeConntrackEntries(nodeName, addresses.srcPodIP, protocol, nil)).To(gomega.Equal(expectedTotalEntries)) // total conntrack entries for this pod/protocol

				ginkgo.By("Removing the namespace annotations to leave only the CR policy active")
				annotateNamespaceForGateway(f.Namespace.Name, false, "")

				ginkgo.By("Check if conntrack entries for ECMP routes still exist for the 2 external gateways")
				gomega.Eventually(func() int {
					n := pokeConntrackEntries(nodeName, addresses.srcPodIP, protocol, macAddressGW)
					klog.Infof("Number of entries with macAddressGW %s:%d", macAddressGW, n)
					return n
				}, time.Minute, 5).Should(gomega.Equal(expectedMACEntries))

				totalPodConnEntries := pokeConntrackEntries(nodeName, addresses.srcPodIP, protocol, nil)
				gomega.Expect(totalPodConnEntries).To(gomega.Equal(expectedTotalEntries)) // total conntrack entries for this pod/protocol

			},
				ginkgo.Entry("IPV4 udp", &addressesv4, "udp"),
				ginkgo.Entry("IPV4 tcp", &addressesv4, "tcp"),
				ginkgo.Entry("IPV6 udp", &addressesv6, "udp"),
				ginkgo.Entry("IPV6 tcp", &addressesv6, "tcp"))

			ginkgo.DescribeTable("ExternalGWPod annotation: Should validate conntrack entry remains unchanged when deleting the annotation in the pods while the CR dynamic hop still "+
				"references the same pods with the pod selector", func(addresses *gatewayTestIPs, protocol string) {
				if addresses.srcPodIP == "" || addresses.nodeIP == "" {
					skipper.Skipf("Skipping as pod ip / node ip are not set pod ip %s node ip %s", addresses.srcPodIP, addresses.nodeIP)
				}
				ginkgo.By("Annotate the external gw pods to manage the src app pod namespace")
				for i, gwPod := range []string{gatewayPodName1, gatewayPodName2} {
					networkIPs := fmt.Sprintf("\"%s\"", addresses.gatewayIPs[i])
					if addresses.srcPodIP != "" && addresses.nodeIP != "" {
						networkIPs = fmt.Sprintf("\"%s\", \"%s\"", addresses.gatewayIPs[i], addresses.gatewayIPs[i])
					}
					annotatePodForGateway(gwPod, servingNamespace, f.Namespace.Name, networkIPs, false)
				}
				createAPBExternalRouteCRWithDynamicHop(defaultPolicyName, f.Namespace.Name, servingNamespace, false, addresses.gatewayIPs)
				// ensure the conntrack deletion tracker annotation is updated
				if !isInterconnectEnabled() {
					ginkgo.By("Check if the k8s.ovn.org/external-gw-pod-ips got updated for the app namespace")
					err := wait.PollImmediate(retryInterval, retryTimeout, func() (bool, error) {
						ns := getNamespace(f, f.Namespace.Name)
						return ns.Annotations[externalGatewayPodIPsAnnotation] == fmt.Sprintf("%s,%s", addresses.gatewayIPs[0], addresses.gatewayIPs[1]), nil
					})
					framework.ExpectNoError(err, "Check if the k8s.ovn.org/external-gw-pod-ips got updated, failed: %v", err)
				}
				annotatePodForGateway(gatewayPodName2, servingNamespace, "", addresses.gatewayIPs[1], false)
				annotatePodForGateway(gatewayPodName1, servingNamespace, "", addresses.gatewayIPs[0], false)

				setupIperf3Client := func(container, address string, port int) {
					// note iperf3 even when using udp also spawns tcp connection first; so we indirectly also have the tcp connection when using "-u" flag
					cmd := []string{containerRuntime, "exec", container, "iperf3", "-u", "-c", address, "-p", fmt.Sprintf("%d", port), "-b", "1M", "-i", "1", "-t", "3", "&"}
					_, err := runCommand(cmd...)
					framework.ExpectNoError(err, "failed to setup iperf3 client for %s", container)
				}
				macAddressGW := make([]string, 2)
				for i, containerName := range []string{gwContainer1, gwContainer2} {
					ginkgo.By("Start iperf3 client from external container to connect to iperf3 server running at the src pod")
					setupIperf3Client(containerName, addresses.srcPodIP, 5201+i)
					macAddressExtGW, err := net.ParseMAC(getMACAddressesForNetwork(containerName, externalContainerNetwork))
					framework.ExpectNoError(err, "failed to parse MAC address for %s", containerName)
					// Trim leading 0s because conntrack dumped labels are just integers
					// in hex without leading 0s.
					macAddressGW[i] = strings.TrimLeft(strings.Replace(macAddressExtGW.String(), ":", "", -1), "0")
				}

				ginkgo.By("Check if conntrack entries for ECMP routes are created for the 2 external gateways")
				nodeName := getPod(f, srcPodName).Spec.NodeName
				podConnEntriesWithMACLabelsSet := pokeConntrackEntries(nodeName, addresses.srcPodIP, protocol, macAddressGW)
				gomega.Expect(podConnEntriesWithMACLabelsSet).To(gomega.Equal(2))
				totalPodConnEntries := pokeConntrackEntries(nodeName, addresses.srcPodIP, protocol, nil)
				gomega.Expect(totalPodConnEntries).To(gomega.Equal(6)) // total conntrack entries for this pod/protocol
				checkAPBExternalRouteStatus(defaultPolicyName)
			},
				ginkgo.Entry("IPV4 udp", &addressesv4, "udp"),
				ginkgo.Entry("IPV4 tcp", &addressesv4, "tcp"),
				ginkgo.Entry("IPV6 udp", &addressesv6, "udp"),
				ginkgo.Entry("IPV6 tcp", &addressesv6, "tcp"))
		})

	})

	var _ = ginkgo.Context("When validating the Admin Policy Based External Route status", func() {
		const (
			svcname          string = "novxlan-externalgw-ecmp"
			gwContainer1     string = "gw-test-container1"
			gwContainer2     string = "gw-test-container2"
			ecmpRetry        int    = 20
			srcPodName              = "e2e-exgw-src-pod"
			externalTCPPort         = 80
			externalUDPPort         = 90
			duplicatedPolicy        = "duplicated"
		)

		f := wrappedTestFramework(svcname)

		var addressesv4 gatewayTestIPs

		ginkgo.BeforeEach(func() {
			nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), f.ClientSet, 3)
			framework.ExpectNoError(err)
			if len(nodes.Items) < 3 {
				framework.Failf(
					"Test requires >= 3 Ready nodes, but there are only %v nodes",
					len(nodes.Items))
			}

			if externalContainerNetwork == "host" {
				skipper.Skipf("Skipping as host network doesn't support multiple external gateways")
			}
			_, addressesv4, _ = setupGatewayContainers(f, nodes, gwContainer1, gwContainer2, srcPodName, externalUDPPort, externalTCPPort, ecmpRetry)
		})

		ginkgo.AfterEach(func() {
			deleteClusterExternalContainer(gwContainer1)
			deleteClusterExternalContainer(gwContainer2)
			deleteAPBExternalRouteCR(defaultPolicyName)
			deleteAPBExternalRouteCR(duplicatedPolicy)
		})

		ginkgo.It("Should update the status of a successful and failed CRs", func() {
			if addressesv4.srcPodIP == "" || addressesv4.nodeIP == "" {
				skipper.Skipf("Skipping as pod ip / node ip are not set pod ip %s node ip %s", addressesv4.srcPodIP, addressesv4.nodeIP)
			}
			createAPBExternalRouteCRWithStaticHopAndStatus(defaultPolicyName, f.Namespace.Name, false, "Success", addressesv4.gatewayIPs...)
			createAPBExternalRouteCRWithStaticHopAndStatus(duplicatedPolicy, f.Namespace.Name, false, "Fail", addressesv4.gatewayIPs...)
		})
	})
})

// setupGatewayContainers sets up external containers, adds routes to the nodes, sets up udp / tcp listeners
// that return the container's hostname.
// All its needed for namespace / pod gateway tests.
func setupGatewayContainers(f *framework.Framework, nodes *v1.NodeList, container1, container2, srcPodName string, updPort, tcpPort, numOfIPs int, postCreations ...func(string)) ([]string, gatewayTestIPs, gatewayTestIPs) {
	gwContainers := []string{container1, container2}
	addressesv4 := gatewayTestIPs{targetIPs: make([]string, 0)}
	addressesv6 := gatewayTestIPs{targetIPs: make([]string, 0)}

	ginkgo.By("Creating the gateway containers for the icmp test")
	if externalContainerNetwork == "host" {
		gwContainers = []string{container1}
		ipv4 := net.ParseIP(externalContainerIPv4)
		if ipv4 == nil || ipv4.To4() == nil {
			framework.Fail(fmt.Sprintf("OVN_TEST_EX_GW_IPV4 is invalid: %s", externalContainerIPv4))
		}
		ipv6 := net.ParseIP(externalContainerIPv6)
		if ipv6 == nil || ipv6.To4() != nil {
			framework.Fail(fmt.Sprintf("OVN_TEST_EX_GW_IPV6 is invalid: %s", externalContainerIPv6))
		}

		_, _ = createClusterExternalContainer(gwContainers[0], externalContainerImage, []string{"-itd", "--privileged", "--network", externalContainerNetwork}, []string{})
		addressesv4.gatewayIPs = append(addressesv4.gatewayIPs, externalContainerIPv4)
		addressesv6.gatewayIPs = append(addressesv6.gatewayIPs, externalContainerIPv6)
	} else {
		for _, gwContainer := range gwContainers {
			gwipv4, gwipv6 := createClusterExternalContainer(gwContainer, externalContainerImage, []string{"-itd", "--privileged", "--network", externalContainerNetwork}, []string{})
			addressesv4.gatewayIPs = append(addressesv4.gatewayIPs, gwipv4)
			addressesv6.gatewayIPs = append(addressesv6.gatewayIPs, gwipv6)
		}
	}

	// Set up the destination ips to reach via the gw
	for lastOctet := 1; lastOctet <= numOfIPs; lastOctet++ {
		destIP := fmt.Sprintf("10.249.10.%d", lastOctet)
		addressesv4.targetIPs = append(addressesv4.targetIPs, destIP)
	}
	for lastGroup := 1; lastGroup <= numOfIPs; lastGroup++ {
		destIP := fmt.Sprintf("fc00:f853:ccd:e794::%d", lastGroup)
		addressesv6.targetIPs = append(addressesv6.targetIPs, destIP)
	}
	framework.Logf("target ips are %v", addressesv4.targetIPs)
	framework.Logf("target ipsv6 are %v", addressesv6.targetIPs)

	node := nodes.Items[0]
	// we must use container network for second bridge scenario
	// for host network we can use the node's ip
	if externalContainerNetwork != "host" {
		addressesv4.nodeIP, addressesv6.nodeIP = getContainerAddressesForNetwork(node.Name, externalContainerNetwork)
	} else {
		nodeList := &v1.NodeList{}
		nodeList.Items = append(nodeList.Items, node)

		addressesv4.nodeIP = e2enode.FirstAddressByTypeAndFamily(nodeList, v1.NodeInternalIP, v1.IPv4Protocol)
		addressesv6.nodeIP = e2enode.FirstAddressByTypeAndFamily(nodeList, v1.NodeInternalIP, v1.IPv6Protocol)
	}

	framework.Logf("the pod side node is %s and the source node ip is %s - %s", node.Name, addressesv4.nodeIP, addressesv6.nodeIP)

	ginkgo.By("Creating the source pod to reach the destination ips from")

	args := []string{
		"netexec",
		fmt.Sprintf("--http-port=%d", srcHTTPPort),
		fmt.Sprintf("--udp-port=%d", srcUDPPort),
	}
	clientPod, err := createPod(f, srcPodName, node.Name, f.Namespace.Name, []string{}, map[string]string{}, func(p *v1.Pod) {
		p.Spec.Containers[0].Args = args
	})

	framework.ExpectNoError(err)

	addressesv4.srcPodIP, addressesv6.srcPodIP = getPodAddresses(clientPod)
	framework.Logf("the pod source pod ip(s) are %s - %s", addressesv4.srcPodIP, addressesv6.srcPodIP)

	testIPv6 := false
	testIPv4 := false

	if addressesv6.srcPodIP != "" && addressesv6.nodeIP != "" {
		testIPv6 = true
	} else {
		addressesv6 = gatewayTestIPs{}
	}
	if addressesv4.srcPodIP != "" && addressesv4.nodeIP != "" {
		testIPv4 = true
	} else {
		addressesv4 = gatewayTestIPs{}
	}
	if !testIPv4 && !testIPv6 {
		framework.Fail("No ipv4 nor ipv6 addresses found in nodes and src pod")
	}

	// This sets up a listener that replies with the hostname, both on tcp and on udp
	setupListenersOrDie := func(container, address string) {
		cmd := []string{containerRuntime, "exec", container, "bash", "-c", fmt.Sprintf("while true; do echo $(hostname) | nc -l -u %s %d; done &", address, updPort)}
		_, err = runCommand(cmd...)
		framework.ExpectNoError(err, "failed to setup UDP listener for %s on %s", address, container)

		cmd = []string{containerRuntime, "exec", container, "bash", "-c", fmt.Sprintf("while true; do echo $(hostname) | nc -l %s %d; done &", address, tcpPort)}
		_, err = runCommand(cmd...)
		framework.ExpectNoError(err, "failed to setup TCP listener for %s on %s", address, container)
	}

	// The target ips are addresses added to the lo of each container.
	// By setting the gateway annotation and using them as destination, we verify that
	// the routing is able to reach the containers.
	// A route back to the src pod must be set in order for the ping reply to work.
	for _, containerName := range gwContainers {
		if testIPv4 {
			ginkgo.By(fmt.Sprintf("Setting up the destination ips to %s", containerName))
			for _, address := range addressesv4.targetIPs {
				_, err = runCommand(containerRuntime, "exec", containerName, "ip", "address", "add", address+"/32", "dev", "lo")
				framework.ExpectNoError(err, "failed to add the loopback ip to dev lo on the test container %s", containerName)
			}

			ginkgo.By(fmt.Sprintf("Adding a route from %s to the src pod", containerName))
			_, err = runCommand(containerRuntime, "exec", containerName, "ip", "route", "add", addressesv4.srcPodIP, "via", addressesv4.nodeIP)
			framework.ExpectNoError(err, "failed to add the pod host route on the test container %s", containerName)

			ginkgo.By("Setting up the listeners on the gateway")
			setupListenersOrDie(containerName, addressesv4.targetIPs[0])
		}
		if testIPv6 {
			ginkgo.By(fmt.Sprintf("Setting up the destination ips to %s (ipv6)", containerName))
			for _, address := range addressesv6.targetIPs {
				_, err = runCommand(containerRuntime, "exec", containerName, "ip", "address", "add", address+"/128", "dev", "lo")
				framework.ExpectNoError(err, "ipv6: failed to add the loopback ip to dev lo on the test container %s", containerName)
			}
			ginkgo.By(fmt.Sprintf("Adding a route from %s to the src pod (ipv6)", containerName))
			_, err = runCommand(containerRuntime, "exec", containerName, "ip", "-6", "route", "add", addressesv6.srcPodIP, "via", addressesv6.nodeIP)
			framework.ExpectNoError(err, "ipv6: failed to add the pod host route on the test container %s", containerName)

			ginkgo.By("Setting up the listeners on the gateway (v6)")
			setupListenersOrDie(containerName, addressesv6.targetIPs[0])
		}
	}
	for _, containerName := range gwContainers {
		for _, postCreation := range postCreations {
			postCreation(containerName)
		}
	}
	return gwContainers, addressesv4, addressesv6
}

func setupAnnotatedGatewayPods(f *framework.Framework, nodes *v1.NodeList, pod1, pod2, ns string, cmd []string, addressesv4, addressesv6 gatewayTestIPs, bfd bool) []string {
	gwPods := []string{pod1, pod2}
	if externalContainerNetwork == "host" {
		gwPods = []string{pod1}
	}

	for i, gwPod := range gwPods {
		_, err := createGenericPodWithLabel(f, gwPod, nodes.Items[i].Name, ns, cmd, map[string]string{"gatewayPod": "true"})
		framework.ExpectNoError(err)
	}

	for i, gwPod := range gwPods {
		var networkIPs string
		if len(addressesv4.gatewayIPs) > 0 {
			// IPv4
			networkIPs = fmt.Sprintf("\"%s\"", addressesv4.gatewayIPs[i])
		}
		if addressesv6.srcPodIP != "" && addressesv6.nodeIP != "" {
			if len(networkIPs) > 0 {
				// IPv4 and IPv6
				networkIPs = fmt.Sprintf("%s, \"%s\"", networkIPs, addressesv6.gatewayIPs[i])
			} else {
				// IPv6 only
				networkIPs = fmt.Sprintf("\"%s\"", addressesv6.gatewayIPs[i])
			}
		}
		annotatePodForGateway(gwPod, ns, f.Namespace.Name, networkIPs, bfd)
	}

	return gwPods
}

func setupPolicyBasedGatewayPods(f *framework.Framework, nodes *v1.NodeList, pod1, pod2, ns string, cmd []string, addressesv4, addressesv6 gatewayTestIPs) []string {
	gwPods := []string{pod1, pod2}
	if externalContainerNetwork == "host" {
		gwPods = []string{pod1}
	}

	for i, gwPod := range gwPods {
		_, err := createGenericPodWithLabel(f, gwPod, nodes.Items[i].Name, ns, cmd, map[string]string{"gatewayPod": "true"})
		framework.ExpectNoError(err)
	}

	for i, gwPod := range gwPods {
		gwIPs := []string{}
		if len(addressesv4.gatewayIPs) > 0 {
			gwIPs = append(gwIPs, addressesv4.gatewayIPs[i])
		}
		if len(addressesv6.gatewayIPs) > 0 {
			gwIPs = append(gwIPs, addressesv6.gatewayIPs[i])
		}
		annotateMultusNetworkStatusInPodGateway(gwPod, ns, gwIPs)
	}

	return gwPods
}

func cleanExGWContainers(clientSet kubernetes.Interface, gwContainers []string, addressesv4, addressesv6 gatewayTestIPs) {
	ginkgo.By("Deleting the gateway containers")
	if externalContainerNetwork == "host" {
		cleanRoutesAndIPs(gwContainers[0], addressesv4, addressesv6)
		deleteClusterExternalContainer(gwContainers[0])
	} else {
		for _, container := range gwContainers {
			deleteClusterExternalContainer(container)
		}
	}
}

// setupGatewayContainersForConntrackTest sets up iperf3 external containers, adds routes to src
// pods via the nodes, starts up iperf3 server on src-pod
func setupGatewayContainersForConntrackTest(f *framework.Framework, nodes *v1.NodeList, gwContainer1, gwContainer2, srcPodName string) (gatewayTestIPs, gatewayTestIPs) {
	var (
		err       error
		clientPod *v1.Pod
	)
	addressesv4 := gatewayTestIPs{gatewayIPs: make([]string, 2)}
	addressesv6 := gatewayTestIPs{gatewayIPs: make([]string, 2)}
	ginkgo.By("Creating the gateway containers for the UDP test")
	addressesv4.gatewayIPs[0], addressesv6.gatewayIPs[0] = createClusterExternalContainer(gwContainer1, iperf3Image, []string{"-itd", "--privileged", "--network", externalContainerNetwork}, []string{})
	addressesv4.gatewayIPs[1], addressesv6.gatewayIPs[1] = createClusterExternalContainer(gwContainer2, iperf3Image, []string{"-itd", "--privileged", "--network", externalContainerNetwork}, []string{})

	node := nodes.Items[0]
	ginkgo.By("Creating the source pod to reach the destination ips from")
	clientPod, err = createPod(f, srcPodName, node.Name, f.Namespace.Name, []string{}, map[string]string{}, func(p *v1.Pod) {
		p.Spec.Containers[0].Image = iperf3Image
	})
	framework.ExpectNoError(err)

	addressesv4.nodeIP, addressesv6.nodeIP = getContainerAddressesForNetwork(node.Name, externalContainerNetwork)
	framework.Logf("the pod side node is %s and the source node ip is %s - %s", node.Name, addressesv4.nodeIP, addressesv6.nodeIP)

	// start iperf3 servers at ports 5201 and 5202 on the src app pod
	args := []string{"exec", srcPodName, "--", "iperf3", "-s", "--daemon", "-V", fmt.Sprintf("-p %d", 5201)}
	_, err = e2ekubectl.RunKubectl(f.Namespace.Name, args...)
	framework.ExpectNoError(err, "failed to start iperf3 server on pod %s at port 5201", srcPodName)

	args = []string{"exec", srcPodName, "--", "iperf3", "-s", "--daemon", "-V", fmt.Sprintf("-p %d", 5202)}
	_, err = e2ekubectl.RunKubectl(f.Namespace.Name, args...)
	framework.ExpectNoError(err, "failed to start iperf3 server on pod %s at port 5202", srcPodName)

	addressesv4.srcPodIP, addressesv6.srcPodIP = getPodAddresses(clientPod)
	framework.Logf("the pod source pod ip(s) are %s - %s", addressesv4.srcPodIP, addressesv6.srcPodIP)

	testIPv6 := false
	testIPv4 := false

	if addressesv6.srcPodIP != "" && addressesv6.nodeIP != "" {
		testIPv6 = true
	}
	if addressesv4.srcPodIP != "" && addressesv4.nodeIP != "" {
		testIPv4 = true
	}
	if !testIPv4 && !testIPv6 {
		framework.Fail("No ipv4 nor ipv6 addresses found in nodes and src pod")
	}

	// A route back to the src pod must be set in order for the ping reply to work.
	for _, containerName := range []string{gwContainer1, gwContainer2} {
		ginkgo.By(fmt.Sprintf("Install iproute in %s", containerName))
		_, err = runCommand(containerRuntime, "exec", containerName, "dnf", "install", "-y", "iproute")
		framework.ExpectNoError(err, "failed to install iproute package on the test container %s", containerName)
		if testIPv4 {
			ginkgo.By(fmt.Sprintf("Adding a route from %s to the src pod with IP %s", containerName, addressesv4.srcPodIP))
			_, err = runCommand(containerRuntime, "exec", containerName, "ip", "route", "add", addressesv4.srcPodIP, "via", addressesv4.nodeIP, "dev", "eth0")
			framework.ExpectNoError(err, "failed to add the pod host route on the test container %s", containerName)
		}
		if testIPv6 {
			ginkgo.By(fmt.Sprintf("Adding a route from %s to the src pod (ipv6)", containerName))
			_, err = runCommand(containerRuntime, "exec", containerName, "ip", "-6", "route", "add", addressesv6.srcPodIP, "via", addressesv6.nodeIP)
			framework.ExpectNoError(err, "ipv6: failed to add the pod host route on the test container %s", containerName)
		}
	}
	return addressesv4, addressesv6
}

func reachPodFromGateway(targetAddress, targetPort, targetPodName, srcContainer, protocol string) {
	ginkgo.By(fmt.Sprintf("Checking that %s can reach the pod", srcContainer))
	dockerCmd := []string{containerRuntime, "exec", srcContainer, "bash", "-c"}
	if protocol == "tcp" {
		dockerCmd = append(dockerCmd, fmt.Sprintf("curl -s http://%s/hostname", net.JoinHostPort(targetAddress, targetPort)))
	} else {
		dockerCmd = append(dockerCmd, fmt.Sprintf("cat <(echo hostname) <(sleep 1) | nc -u %s %s", targetAddress, targetPort))
	}

	res, err := runCommand(dockerCmd...)
	framework.ExpectNoError(err, "Failed to reach pod %s (%s) from external container %s", targetAddress, protocol, srcContainer)
	gomega.Expect(strings.Trim(res, "\n")).To(gomega.Equal(targetPodName))
}

func annotatePodForGateway(podName, podNS, targetNamespace, networkIPs string, bfd bool) {
	if !strings.HasPrefix(networkIPs, "\"") {
		networkIPs = fmt.Sprintf("\"%s\"", networkIPs)
	}
	// add the annotations to the pod to enable the gateway forwarding.
	// this fakes out the multus annotation so that the pod IP is
	// actually an IP of an external container for testing purposes
	annotateArgs := []string{
		fmt.Sprintf("k8s.v1.cni.cncf.io/network-status=[{\"name\":\"%s\",\"interface\":"+
			"\"net1\",\"ips\":[%s],\"mac\":\"%s\"}]", "foo", networkIPs, "01:23:45:67:89:10"),
		fmt.Sprintf("k8s.ovn.org/routing-namespaces=%s", targetNamespace),
		fmt.Sprintf("k8s.ovn.org/routing-network=%s", "foo"),
	}
	if bfd {
		annotateArgs = append(annotateArgs, "k8s.ovn.org/bfd-enabled=\"\"")
	}
	annotatePodForGatewayWithAnnotations(podName, podNS, annotateArgs)
}

func annotateMultusNetworkStatusInPodGateway(podName, podNS string, networkIPs []string) {
	// add the annotations to the pod to enable the gateway forwarding.
	// this fakes out the multus annotation so that the pod IP is
	// actually an IP of an external container for testing purposes
	nStatus := []nettypes.NetworkStatus{{Name: "foo", Interface: "net1", IPs: networkIPs, Mac: "01:23:45:67:89:10"}}
	out, err := json.Marshal(nStatus)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())

	annotatePodForGatewayWithAnnotations(podName, podNS, []string{fmt.Sprintf("k8s.v1.cni.cncf.io/network-status=%s", string(out))})
}

func annotatePodForGatewayWithAnnotations(podName, podNS string, annotations []string) {
	// add the annotations to the pod to enable the gateway forwarding.
	// this fakes out the multus annotation so that the pod IP is
	// actually an IP of an external container for testing purposes
	annotateArgs := []string{
		"annotate",
		"pods",
		podName,
		"--overwrite",
	}
	annotateArgs = append(annotateArgs, annotations...)
	framework.Logf("Annotating the external gateway pod with annotation '%s'", annotateArgs)
	e2ekubectl.RunKubectlOrDie(podNS, annotateArgs...)
}

func annotateNamespaceForGateway(namespace string, bfd bool, gateways ...string) {

	externalGateways := strings.Join(gateways, ",")
	// annotate the test namespace with multiple gateways defined
	annotateArgs := []string{
		"annotate",
		"namespace",
		namespace,
		fmt.Sprintf("k8s.ovn.org/routing-external-gws=%s", externalGateways),
		"--overwrite",
	}
	if bfd {
		annotateArgs = append(annotateArgs, "k8s.ovn.org/bfd-enabled=\"\"")
	}
	framework.Logf("Annotating the external gateway test namespace to container gateways: %s", externalGateways)
	e2ekubectl.RunKubectlOrDie(namespace, annotateArgs...)
}

func createAPBExternalRouteCRWithDynamicHop(policyName, targetNamespace, servingNamespace string, bfd bool, gateways []string) {
	data := fmt.Sprintf(`apiVersion: k8s.ovn.org/v1
kind: AdminPolicyBasedExternalRoute
metadata:
  name: %s
spec:
  from:
    namespaceSelector:
      matchLabels:
        kubernetes.io/metadata.name: %s
  nextHops:
    dynamic:
%s
`, policyName, targetNamespace, formatDynamicHops(bfd, servingNamespace))
	stdout, err := e2ekubectl.RunKubectlInput("", data, "create", "-f", "-")
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	gomega.Expect(stdout).To(gomega.Equal(fmt.Sprintf("adminpolicybasedexternalroute.k8s.ovn.org/%s created\n", policyName)))
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	gwIPs := sets.NewString(gateways...).List()
	gomega.Eventually(func() string {
		lastMsg, err := e2ekubectl.RunKubectl("", "get", "apbexternalroute", policyName, "-ojsonpath={.status.messages[-1:]}")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		return lastMsg
	}, time.Minute, 1).Should(gomega.ContainSubstring(fmt.Sprintf("configured external gateway IPs: %s", strings.Join(gwIPs, ","))))
	gomega.Eventually(func() string {
		status, err := e2ekubectl.RunKubectl("", "get", "apbexternalroute", policyName, "-ojsonpath={.status.status}")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		return status
	}, time.Minute, 1).Should(gomega.Equal("Success"))
}

func checkAPBExternalRouteStatus(policyName string) {
	status, err := e2ekubectl.RunKubectl("", "get", "apbexternalroute", policyName, "-ojsonpath={.status.status}")
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	gomega.Expect(status).To(gomega.Equal("Success"))
}

func createAPBExternalRouteCRWithStaticHop(policyName, namespaceName string, bfd bool, gateways ...string) {
	createAPBExternalRouteCRWithStaticHopAndStatus(policyName, namespaceName, bfd, "Success", gateways...)
	gwIPs := sets.NewString(gateways...).List()
	gomega.Eventually(func() string {
		lastMsg, err := e2ekubectl.RunKubectl("", "get", "apbexternalroute", policyName, "-ojsonpath={.status.messages[-1:]}")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		return lastMsg
	}, time.Minute, 1).Should(gomega.ContainSubstring(fmt.Sprintf("configured external gateway IPs: %s", strings.Join(gwIPs, ","))))
}

func createAPBExternalRouteCRWithStaticHopAndStatus(policyName, namespaceName string, bfd bool, status string, gateways ...string) {
	data := fmt.Sprintf(`apiVersion: k8s.ovn.org/v1
kind: AdminPolicyBasedExternalRoute
metadata:
  name: %s
spec:
  from:
    namespaceSelector:
      matchLabels:
        kubernetes.io/metadata.name: %s
  nextHops:
    static:
%s
`, policyName, namespaceName, formatStaticHops(bfd, gateways...))
	stdout, err := e2ekubectl.RunKubectlInput("", data, "create", "-f", "-", "--save-config")
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	gomega.Expect(stdout).To(gomega.Equal(fmt.Sprintf("adminpolicybasedexternalroute.k8s.ovn.org/%s created\n", policyName)))
	gomega.Eventually(func() string {
		status, err := e2ekubectl.RunKubectl("", "get", "apbexternalroute", policyName, "-ojsonpath={.status.status}")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		return status
	}, time.Minute, 1).Should(gomega.Equal(status))
}

func updateAPBExternalRouteCRWithStaticHop(policyName, namespaceName string, bfd bool, gateways ...string) {

	lastUpdatetime, err := e2ekubectl.RunKubectl("", "get", "apbexternalroute", policyName, "-ojsonpath={.status.lastTransitionTime}")
	gomega.Expect(err).NotTo(gomega.HaveOccurred())

	data := fmt.Sprintf(`apiVersion: k8s.ovn.org/v1
kind: AdminPolicyBasedExternalRoute
metadata:
  name: %s
spec:
  from:
    namespaceSelector:
      matchLabels:
        kubernetes.io/metadata.name: %s
  nextHops:
    static:
%s
`, policyName, namespaceName, formatStaticHops(bfd, gateways...))
	_, err = e2ekubectl.RunKubectlInput(namespaceName, data, "apply", "-f", "-")
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	gomega.Eventually(func() string {
		lastMsg, err := e2ekubectl.RunKubectl("", "get", "apbexternalroute", policyName, "-ojsonpath={.status.messages[-1:]}")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		return lastMsg
	}, 10).Should(gomega.ContainSubstring(fmt.Sprintf("configured external gateway IPs: %s", strings.Join(gateways, ","))))

	gomega.Eventually(func() string {
		s, err := e2ekubectl.RunKubectl("", "get", "apbexternalroute", policyName, "-ojsonpath={.status.status}")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		return s
	}, 10).Should(gomega.Equal("Success"))
	gomega.Eventually(func() string {
		t, err := e2ekubectl.RunKubectl("", "get", "apbexternalroute", policyName, "-ojsonpath={.status.lastTransitionTime}")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		return t
	}, 10, 1).ShouldNot(gomega.Equal(lastUpdatetime))

}

func deleteAPBExternalRouteCR(policyName string) {
	e2ekubectl.RunKubectl("", "delete", "apbexternalroute", policyName)
}
func formatStaticHops(bfd bool, gateways ...string) string {
	b := strings.Builder{}
	bfdEnabled := "true"
	if !bfd {
		bfdEnabled = "false"
	}
	for _, gateway := range gateways {
		b.WriteString(fmt.Sprintf(`     - ip: "%s"
       bfdEnabled: %s
`, gateway, bfdEnabled))
	}
	return b.String()
}

func formatDynamicHops(bfd bool, servingNamespace string) string {
	b := strings.Builder{}
	bfdEnabled := "true"
	if !bfd {
		bfdEnabled = "false"
	}
	b.WriteString(fmt.Sprintf(`      - podSelector:
          matchLabels:
            gatewayPod: "true"
        bfdEnabled: %s
        namespaceSelector:
          matchLabels:
            kubernetes.io/metadata.name: %s
        networkAttachmentName: foo
`, bfdEnabled, servingNamespace))
	return b.String()
}

func getGatewayPod(f *framework.Framework, podNamespace, podName string) *v1.Pod {
	pod, err := f.ClientSet.CoreV1().Pods(podNamespace).Get(context.Background(), podName, metav1.GetOptions{})
	framework.ExpectNoError(err, fmt.Sprintf("unable to get pod: %s, err: %v", podName, err))
	return pod
}

func hostNamesForContainers(containers []string) map[string]struct{} {
	res := make(map[string]struct{})
	for _, c := range containers {
		hostName := hostNameForContainer(c)
		res[hostName] = struct{}{}
	}
	return res
}

func hostNameForContainer(container string) string {
	res, err := runCommand(containerRuntime, "exec", container, "hostname")
	framework.ExpectNoError(err, "failed to run hostname in %s", container)
	framework.Logf("Hostname for %s is %s", container, res)
	return strings.TrimSuffix(res, "\n")
}

func pokeHostnameViaNC(podName, namespace, protocol, target string, port int) string {
	args := []string{"exec", podName, "--"}
	if protocol == "tcp" {
		args = append(args, "bash", "-c", fmt.Sprintf("echo | nc -w 1 %s %d", target, port))
	} else {
		args = append(args, "bash", "-c", fmt.Sprintf("echo | nc -w 1 -u %s %d", target, port))
	}
	res, err := e2ekubectl.RunKubectl(namespace, args...)
	framework.ExpectNoError(err, "failed to reach %s (%s)", target, protocol)
	hostname := strings.TrimSuffix(res, "\n")
	return hostname
}

// pokeConntrackEntries returns the number of conntrack entries that match the provided pattern, protocol and podIP
func pokeConntrackEntries(nodeName, podIP, protocol string, patterns []string) int {
	args := []string{"get", "pods", "--selector=app=ovs-node", "--field-selector", fmt.Sprintf("spec.nodeName=%s", nodeName), "-o", "jsonpath={.items..metadata.name}"}
	ovsPodName, err := e2ekubectl.RunKubectl(ovnNamespace, args...)
	framework.ExpectNoError(err, "failed to get the ovs pod on node %s", nodeName)
	args = []string{"exec", ovsPodName, "--", "ovs-appctl", "dpctl/dump-conntrack"}
	conntrackEntries, err := e2ekubectl.RunKubectl(ovnNamespace, args...)
	framework.ExpectNoError(err, "failed to get the conntrack entries from node %s", nodeName)
	numOfConnEntries := 0
	for _, connEntry := range strings.Split(conntrackEntries, "\n") {
		match := strings.Contains(connEntry, protocol) && strings.Contains(connEntry, podIP)
		for _, pattern := range patterns {
			if match {
				klog.Infof("%s in %s", pattern, connEntry)
				if strings.Contains(connEntry, pattern) {
					numOfConnEntries++
				}
			}
		}
		if len(patterns) == 0 && match {
			numOfConnEntries++
		}
	}

	return numOfConnEntries
}

func setupBFDOnContainer(nodes []v1.Node) func(string) {
	return func(containerName string) {
		// we set a bfd peer for each address of each node
		for _, node := range nodes {
			// we must use container network for second bridge scenario
			// for host network we can use the node's ip
			var ipv4, ipv6 string
			if externalContainerNetwork != "host" {
				ipv4, ipv6 = getContainerAddressesForNetwork(node.Name, externalContainerNetwork)
			} else {
				nodeList := &v1.NodeList{}
				nodeList.Items = append(nodeList.Items, node)

				ipv4 = e2enode.FirstAddressByTypeAndFamily(nodeList, v1.NodeInternalIP, v1.IPv4Protocol)
				ipv6 = e2enode.FirstAddressByTypeAndFamily(nodeList, v1.NodeInternalIP, v1.IPv6Protocol)
			}

			for _, a := range []string{ipv4, ipv6} {
				if a == "" {
					continue
				}
				// Configure the node as a bfd peer on the frr side
				cmd := []string{containerRuntime, "exec", containerName, "bash", "-c",
					fmt.Sprintf(`cat << EOF >> /etc/frr/frr.conf

bfd
 peer %s
   no shutdown
 !
!
EOF
`, a)}
				_, err := runCommand(cmd...)
				framework.ExpectNoError(err, "failed to setup FRR peer %s in %s", a, containerName)
			}
		}
		cmd := []string{containerRuntime, "exec", containerName, "/usr/libexec/frr/frrinit.sh", "start"}
		_, err := runCommand(cmd...)
		framework.ExpectNoError(err, "failed to start frr in %s", containerName)
	}
}

func isBFDPaired(container, peer string) bool {
	res, err := runCommand(containerRuntime, "exec", container, "bash", "-c", fmt.Sprintf("vtysh -c \"show bfd peer %s\"", peer))
	framework.ExpectNoError(err, "failed to check bfd status in %s", container)
	return strings.Contains(res, "Status: up")
}

// When running on host network we clean the routes and ips we added previously
func cleanRoutesAndIPs(containerName string, addressesv4, addressesv6 gatewayTestIPs) {
	testIPv6 := addressesv6.srcPodIP != "" && addressesv6.nodeIP != ""
	testIPv4 := addressesv4.srcPodIP != "" && addressesv4.nodeIP != ""

	if testIPv4 {
		cleanRoutesAndIPsForFamily(containerName, 4, addressesv4)
	}
	if testIPv6 {
		cleanRoutesAndIPsForFamily(containerName, 6, addressesv6)
	}
}

func cleanRoutesAndIPsForFamily(containerName string, family int, addresses gatewayTestIPs) {
	var err error
	var addressMask string
	addressSelector := fmt.Sprintf("-%d", family)
	mask := 32
	if family == 6 {
		mask = 128
	}
	ginkgo.By(fmt.Sprintf("Removing the destination ips from %s (IPv%d)", containerName, family))
	for _, address := range addresses.targetIPs {
		addressMask = fmt.Sprintf("%s/%d", address, mask)
		_, err = runCommand(containerRuntime, "exec", containerName, "ip", addressSelector, "address", "del", addressMask, "dev", "lo")
		framework.ExpectNoError(err, "failed to del the loopback ip %s from dev lo on the test container %s", addressMask, containerName)
	}

	ginkgo.By(fmt.Sprintf("Removing the route from %s to the src pod (IPv%d)", containerName, family))
	_, err = runCommand(containerRuntime, "exec", containerName, "ip", addressSelector, "route", "del", addresses.srcPodIP, "via", addresses.nodeIP)
	framework.ExpectNoError(err, "failed to del the pod host route on the test container %s", containerName)
}

func setMACAddrOnContainer(containerName, addr, link string) {
	_, err := runCommand(containerRuntime, "exec", containerName, "ip", "link", "set", "dev", link, "addr", addr)
	framework.ExpectNoError(err, "failed to set MAC address on  container %s", containerName)
}

func checkReceivedPacketsOnContainer(container, srcPodName, link string, filter []string, wg *sync.WaitGroup) {
	defer ginkgo.GinkgoRecover()
	defer wg.Done()
	if len(link) == 0 {
		link = anyLink
	}
	args := []string{containerRuntime, "exec", container, "timeout", "60", "tcpdump", "-c", "1", "-i", link}
	args = append(args, filter...)
	_, err := runCommand(args...)
	framework.ExpectNoError(err, "Failed to detect packets from %s on gateway %s", srcPodName, container)
	framework.Logf("Packet successfully detected on gateway %s", container)
}

func resetGatewayAnnotations(f *framework.Framework) {
	// remove the routing external annotation
	if f == nil || f.Namespace == nil {
		return
	}
	annotations := []string{
		"k8s.ovn.org/routing-external-gws-",
		"k8s.ovn.org/bfd-enabled-",
	}
	ginkgo.By("Resetting the gw annotations")
	for _, annotation := range annotations {
		e2ekubectl.RunKubectlOrDie("", []string{
			"annotate",
			"namespace",
			f.Namespace.Name,
			annotation}...)
	}
}

func sendGARP(container, ip, link string) {
	ginkgo.By(fmt.Sprintf("Sending ARP on container %s, for IP: %s", container, ip))
	args := []string{containerRuntime, "exec", container, "arping", "-U", ip, "-I", link, "-c", "1", "-s", ip}
	_, err := runCommand(args...)
	framework.ExpectNoError(err, "Failed to send arping on container: %s", container)
}

func sendNDPAdvertisement(container, ip, link string) {
	ginkgo.By(fmt.Sprintf("Sending NDP Unsolicited NA on container %s, for IP: %s", container, ip))
	args := []string{containerRuntime, "exec", container, "ndptool", "-t", "na", "-U", "-i", link, "-T", ip, "send"}
	_, err := runCommand(args...)
	framework.ExpectNoError(err, "Failed to send NDP unsolicted advertisement on container: %s", container)
}
