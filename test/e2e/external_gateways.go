// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

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

	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/feature"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/images"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider"
	infraapi "github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/api"

	"github.com/google/go-cmp/cmp"
	nettypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	"k8s.io/kubernetes/test/e2e/framework/skipper"
)

// This is the image used for the containers acting as externalgateways, built
// out from the e2e/images/Dockerfile.frr dockerfile
const (
	externalContainerImage = "quay.io/trozet/ovnkbfdtest:0.3"
	defaultPolicyName      = "default-route-policy"
	anyLink                = "any"
)

// GatewayRemovalType defines ways to remove pod as external gateway
type GatewayRemovalType string

const (
	GatewayUpdate            GatewayRemovalType = "GatewayUpdate"
	GatewayDelete            GatewayRemovalType = "GatewayDelete"
	GatewayDeletionTimestamp GatewayRemovalType = "GatewayDeletionTimestamp"
	GatewayNotReady          GatewayRemovalType = "GatewayNotReady"
)

func getOverrideNetwork() (string, string, string) {
	// When the env variable is specified, we use a different docker network for
	// containers acting as external gateways.
	// In a specific case where the variable is set to `host` we create only one
	// external container to act as an external gateway, as we can't create 2
	// because of overlapping ip/ports (like the bfd port).
	var networkName, ipv4, ipv6 string
	if exNetwork, found := os.LookupEnv("OVN_TEST_EX_GW_NETWORK"); found {
		networkName = exNetwork
	}
	// When OVN_TEST_EX_GW_NETWORK is set to "host" we need to set the container's IP from outside
	if exHostIPv4, found := os.LookupEnv("OVN_TEST_EX_GW_IPV4"); found {
		ipv4 = exHostIPv4
	}
	if exHostIPv6, found := os.LookupEnv("OVN_TEST_EX_GW_IPV6"); found {
		ipv6 = exHostIPv6
	}
	return networkName, ipv4, ipv6
}

func getContainerName(template string, port uint16) string {
	return fmt.Sprintf(template, port)
}

// gatewayTestIPs collects all the addresses required for an external gateway
// test.
type gatewayTestIPs struct {
	gatewayIPs []string
	srcPodIP   string
	nodeIP     string
	targetIPs  []string
}

var _ = ginkgo.Describe("External Gateway", feature.ExternalGateway, func() {

	const (
		gwTCPPort           = 80
		gwUDPPort           = 90
		podTCPPort          = 80
		podUDPPort          = 90
		singleTargetRetries = 50 // enough attempts to avoid hashing to the same gateway
		bfdTimeout          = 4 * time.Second
	)

	var _ = ginkgo.Context("With Admin Policy Based External Route CRs", func() {

		// Validate pods can reach a network running in a container's looback address via
		// an external gateway running on eth0 of the container without any tunnel encap.
		// The traffic will get proxied through an annotated pod in the serving namespace.
		var _ = ginkgo.Describe("e2e non-vxlan external gateway through a gateway pod", func() {
			const (
				svcname              string        = "externalgw-pod-novxlan"
				gwContainer1Template string        = "ex-gw-container1-%d"
				gwContainer2Template string        = "ex-gw-container2-%d"
				srcPingPodName       string        = "e2e-exgw-src-ping-pod"
				gatewayPodName1      string        = "e2e-gateway-pod1"
				gatewayPodName2      string        = "e2e-gateway-pod2"
				ecmpRetry            int           = 20
				testTimeout          time.Duration = 20 * time.Second
			)

			var (
				sleepCommand             = []string{"bash", "-c", "sleep 20000"}
				addressesv4, addressesv6 gatewayTestIPs
				servingNamespace         string
				gwContainers             []infraapi.ExternalContainer
				providerCtx              infraapi.Context
			)

			f := wrappedTestFramework(svcname)

			ginkgo.BeforeEach(func() {
				providerCtx = infraprovider.Get().NewTestContext()
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
				network, err := infraprovider.Get().PrimaryNetwork()
				framework.ExpectNoError(err, "failed to get primary network information")
				if overrideNetworkName, _, _ := getOverrideNetwork(); overrideNetworkName != "" {
					overrideNetwork, err := infraprovider.Get().GetNetwork(overrideNetworkName)
					framework.ExpectNoError(err, "over ride network must exist")
					network = overrideNetwork
				}
				gwContainers, addressesv4, addressesv6 = setupGatewayContainers(f, providerCtx, nodes, network, gwContainer1Template,
					gwContainer2Template, srcPingPodName, gwUDPPort, gwTCPPort, podUDPPort, podTCPPort, ecmpRetry, false)
				setupPolicyBasedGatewayPods(f, nodes, network, gatewayPodName1, gatewayPodName2, servingNamespace, sleepCommand, addressesv4, addressesv6)
			})

			ginkgo.AfterEach(func() {
				deleteAPBExternalRouteCR(defaultPolicyName)
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
						gomega.Eventually(infraprovider.Get().ExecExternalContainerCommand).
							WithArguments(gwContainer, []string{"ping", "-B", "-c1", "-W1", "-I", addresses.targetIPs[0], addresses.srcPodIP}).
							WithTimeout(testTimeout).
							ShouldNot(gomega.BeEmpty(), "Failed to ping %s from container %s", addresses.srcPodIP, gwContainer.Name)
					}

					tcpDumpSync := sync.WaitGroup{}
					tcpDumpSync.Add(len(gwContainers))

					for _, gwContainer := range gwContainers {
						go checkReceivedPacketsOnExternalContainer(gwContainer, srcPingPodName, anyLink, []string{icmpCommand}, &tcpDumpSync)
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
							gomega.Eventually(e2ekubectl.RunKubectl).
								WithArguments(f.Namespace.Name, "exec", srcPingPodName, "--", "ping", "-c1", "-W1", target).
								WithTimeout(testTimeout).
								ShouldNot(gomega.BeEmpty(), "Failed to ping remote gateway %s from pod %s", target, srcPingPodName)
						}(t)
					}
					pingSync.Wait()
					tcpDumpSync.Wait()
					checkAPBExternalRouteStatus(defaultPolicyName)
				},
				ginkgo.Entry("ipv4", &addressesv4, "icmp"),
				ginkgo.Entry("ipv6", &addressesv6, "icmp6"))

			ginkgo.DescribeTable("Should validate TCP/UDP connectivity to an external gateway's loopback address via a gateway pod",
				func(protocol string, addresses *gatewayTestIPs, gwPort, podPort int) {
					if addresses.srcPodIP == "" || addresses.nodeIP == "" {
						skipper.Skipf("Skipping as pod ip / node ip are not set pod ip %s node ip %s", addresses.srcPodIP, addresses.nodeIP)
					}
					createAPBExternalRouteCRWithDynamicHop(defaultPolicyName, f.Namespace.Name, servingNamespace, false, addressesv4.gatewayIPs)

					for _, container := range gwContainers {
						reachPodFromGateway(container, addresses.srcPodIP, strconv.Itoa(podPort), srcPingPodName, protocol)
					}

					expectedHostNames := make(map[string]struct{})
					for _, c := range gwContainers {
						res, err := infraprovider.Get().ExecExternalContainerCommand(c, []string{"hostname"})
						framework.ExpectNoError(err, "failed to run hostname in %s", c.Name)
						hostname := strings.TrimSuffix(res, "\n")
						framework.Logf("Hostname for %s is %s", c.Name, hostname)
						expectedHostNames[hostname] = struct{}{}
					}
					framework.Logf("Expected hostnames are %v", expectedHostNames)

					ginkgo.By("Checking that external ips are reachable with both gateways")
					returnedHostNames := make(map[string]struct{})
					gwIP := addresses.targetIPs[0]
					success := false
					for i := 0; i < singleTargetRetries; i++ {
						args := []string{"exec", srcPingPodName, "--"}
						if protocol == "tcp" {
							args = append(args, "bash", "-c", fmt.Sprintf("echo | nc -w 1 %s %d", gwIP, gwPort))
						} else {
							args = append(args, "bash", "-c", fmt.Sprintf("echo | nc -w 1 -u %s %d", gwIP, gwPort))
						}
						res, err := e2ekubectl.RunKubectl(f.Namespace.Name, args...)
						framework.ExpectNoError(err, "failed to reach %s (%s)", gwIP, protocol)
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
				ginkgo.Entry("UDP ipv4", "udp", &addressesv4, gwUDPPort, podUDPPort),
				ginkgo.Entry("TCP ipv4", "tcp", &addressesv4, gwTCPPort, podTCPPort),
				ginkgo.Entry("UDP ipv6", "udp", &addressesv6, gwUDPPort, podUDPPort),
				ginkgo.Entry("TCP ipv6", "tcp", &addressesv6, gwTCPPort, podTCPPort))

			ginkgo.DescribeTable("Should validate TCP/UDP connectivity even after MAC change (gateway migration) for egress",
				func(protocol string, addresses *gatewayTestIPs, gwPort, podPort int) {
					ncCmd := func(sourcePort int, target string) []string {
						if protocol == "tcp" {
							return []string{"exec", srcPingPodName, "--", "bash", "-c", fmt.Sprintf("echo | nc -p %d -s %s -w 1 %s %d", sourcePort, addresses.srcPodIP, target, gwPort)}
						} else {
							return []string{"exec", srcPingPodName, "--", "bash", "-c", fmt.Sprintf("echo | nc -p %d -s %s -w 1 -u %s %d", sourcePort, addresses.srcPodIP, target, gwPort)}
						}
					}
					if addresses.srcPodIP == "" || addresses.nodeIP == "" {
						skipper.Skipf("Skipping as pod ip / node ip are not set pod ip %s node ip %s", addresses.srcPodIP, addresses.nodeIP)
					}
					createAPBExternalRouteCRWithDynamicHop(defaultPolicyName, f.Namespace.Name, servingNamespace, false, addressesv4.gatewayIPs)

					ginkgo.By("Checking Ingress connectivity from gateways")
					// Check Ingress connectivity
					for _, container := range gwContainers {
						reachPodFromGateway(container, addresses.srcPodIP, strconv.Itoa(podPort), srcPingPodName, protocol)
					}

					// Get hostnames of gateways
					// map of hostname to gateway
					expectedHostNames := make(map[string]infraapi.ExternalContainer)
					gwAddresses := make(map[string]string)
					for _, c := range gwContainers {
						res, err := infraprovider.Get().ExecExternalContainerCommand(c, []string{"hostname"})
						framework.ExpectNoError(err, "failed to run hostname in %s", c)
						hostname := strings.TrimSuffix(res, "\n")
						res, err = infraprovider.Get().ExecExternalContainerCommand(c, []string{"hostname", "-I"})
						framework.ExpectNoError(err, "failed to run hostname in %s", c)
						ips := strings.TrimSuffix(res, "\n")
						framework.Logf("Hostname for %s is %s, with IP addresses: %s", c, hostname, ips)
						expectedHostNames[hostname] = c
						gwAddresses[c.Name] = ips
					}
					framework.Logf("Expected hostnames are %v", expectedHostNames)

					// We have to remove a gateway so that traffic consistently goes to the same gateway. This
					// is due to lack of consistent hashing support in github actions:
					// https://github.com/ovn-kubernetes/ovn-kubernetes/pull/4114#issuecomment-1940916326
					// TODO(trozet) change this back to 2 gateways once github actions kernel is updated
					ginkgo.By(fmt.Sprintf("Reducing to one gateway. Removing gateway: %s", gatewayPodName2))
					err := e2epod.DeletePodWithWaitByName(context.TODO(), f.ClientSet, gatewayPodName2, servingNamespace)
					framework.ExpectNoError(err, "failed to delete pod %s/%s", servingNamespace, gatewayPodName2)
					time.Sleep(1 * time.Second)

					ginkgo.By("Checking if one of the external gateways are reachable via Egress")
					gwIP := addresses.targetIPs[0]
					sourcePort := 50000

					res, err := e2ekubectl.RunKubectl(f.Namespace.Name, ncCmd(sourcePort, gwIP)...)
					framework.ExpectNoError(err, "failed to reach %s (%s)", gwIP, protocol)
					hostname := strings.TrimSuffix(res, "\n")
					var gateway infraapi.ExternalContainer
					if g, ok := expectedHostNames[hostname]; !ok {
						framework.Failf("Unexpected gateway hostname %q, expected; %#v", hostname, expectedHostNames)
					} else {
						gateway = g
					}
					network, err := infraprovider.Get().PrimaryNetwork()
					framework.ExpectNoError(err, "failed to get primary network information")
					if overrideNetworkName, _, _ := getOverrideNetwork(); overrideNetworkName != "" {
						overrideNetwork, err := infraprovider.Get().GetNetwork(overrideNetworkName)
						framework.ExpectNoError(err, "over ride network must exist")
						network = overrideNetwork
					}
					gatewayContainerNetworkInfo, err := infraprovider.Get().GetExternalContainerNetworkInterface(gateway, network)
					framework.ExpectNoError(err, "failed to get network information for gateway container")
					framework.Logf("Egress gateway reached: %s, with MAC: %q", gateway, gatewayContainerNetworkInfo.MAC)

					ginkgo.By("Sending traffic again and verifying packet is received at gateway")
					tcpDumpSync := sync.WaitGroup{}
					tcpDumpSync.Add(1)
					go checkReceivedPacketsOnExternalContainer(gateway, srcPingPodName, anyLink, []string{protocol, "and", "port", strconv.Itoa(sourcePort)}, &tcpDumpSync)
					res, err = e2ekubectl.RunKubectl(f.Namespace.Name, ncCmd(sourcePort, gwIP)...)
					framework.ExpectNoError(err, "failed to reach %s (%s)", gwIP, protocol)
					hostname2 := strings.TrimSuffix(res, "\n")
					gomega.Expect(hostname).To(gomega.Equal(hostname2))
					tcpDumpSync.Wait()

					newDummyMac := "02:11:22:33:44:56"

					ginkgo.By(fmt.Sprintf("Modifying MAC address of gateway %q to simulate migration, new MAC: %s", gateway, newDummyMac))
					_, err = infraprovider.Get().ExecExternalContainerCommand(gateway, []string{"ip", "link", "set", "dev", infraprovider.Get().ExternalContainerPrimaryInterfaceName(), "addr", newDummyMac})
					framework.ExpectNoError(err, "failed to set MAC on external container")
					providerCtx.AddCleanUpFn(func() error {
						_, err = infraprovider.Get().ExecExternalContainerCommand(gateway, []string{"ip", "link", "set", "dev",
							infraprovider.Get().ExternalContainerPrimaryInterfaceName(), "addr", gatewayContainerNetworkInfo.MAC})
						return err
					})

					ginkgo.By("Sending layer 2 advertisement from external gateway")
					time.Sleep(1 * time.Second)

					if IsIPv6Cluster(f.ClientSet) {
						_, err = infraprovider.Get().ExecExternalContainerCommand(gateway, []string{"ndptool", "-t", "na", "-U",
							"-i", infraprovider.Get().ExternalContainerPrimaryInterfaceName(), "-T", gatewayContainerNetworkInfo.IPv6, "send"})
					} else {
						_, err = infraprovider.Get().ExecExternalContainerCommand(gateway, []string{"arping", "-U", gatewayContainerNetworkInfo.IPv4,
							"-I", infraprovider.Get().ExternalContainerPrimaryInterfaceName(), "-c", "1", "-s", gatewayContainerNetworkInfo.IPv4})
					}
					framework.ExpectNoError(err, "arp / nd must succeed")
					time.Sleep(1 * time.Second)

					ginkgo.By("Post-Migration: Sending Egress traffic and verify it is received")
					// We don't want traffic to hit the already existing conntrack entry (created for source port 50000)
					// so we use a fresh source port.
					sourcePort = 50001

					tcpDumpSync = sync.WaitGroup{}
					tcpDumpSync.Add(1)
					go checkReceivedPacketsOnExternalContainer(gateway, srcPingPodName, infraprovider.Get().ExternalContainerPrimaryInterfaceName(),
						[]string{protocol, "and", "ether", "host", newDummyMac, "and", "port", strconv.Itoa(sourcePort)}, &tcpDumpSync)
					// Sometimes the external gateway will fail to respond to the request with
					// SKB_DROP_REASON_NEIGH_FAILED after changing the MAC address. Something breaks with ARP
					// on the gateway container. Therefore, ignore the reply from gateway, as we only care about the egress
					// packet arriving with correct MAC address.
					_, _ = e2ekubectl.RunKubectl(f.Namespace.Name, ncCmd(sourcePort, gwIP)...)
					tcpDumpSync.Wait()

					checkAPBExternalRouteStatus(defaultPolicyName)
				},
				ginkgo.Entry("UDP ipv4", "udp", &addressesv4, gwUDPPort, podUDPPort),
				ginkgo.Entry("TCP ipv4", "tcp", &addressesv4, gwTCPPort, podTCPPort),
				ginkgo.Entry("UDP ipv6", "udp", &addressesv6, gwUDPPort, podUDPPort),
				ginkgo.Entry("TCP ipv6", "tcp", &addressesv6, gwTCPPort, podTCPPort))
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
				svcname              string        = "novxlan-externalgw-ecmp"
				gwContainer1Template string        = "gw-test-container1-%d"
				gwContainer2Template string        = "gw-test-container2-%d"
				testTimeout          time.Duration = 30 * time.Second
				ecmpRetry            int           = 20
				srcPodName                         = "e2e-exgw-src-pod"
			)

			f := wrappedTestFramework(svcname)

			var (
				providerCtx              infraapi.Context
				gwContainers             []infraapi.ExternalContainer
				addressesv4, addressesv6 gatewayTestIPs
			)

			ginkgo.BeforeEach(func() {
				providerCtx = infraprovider.Get().NewTestContext()
				// retrieve worker node names
				nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), f.ClientSet, 3)
				framework.ExpectNoError(err)
				if len(nodes.Items) < 3 {
					framework.Failf(
						"Test requires >= 3 Ready nodes, but there are only %v nodes",
						len(nodes.Items))
				}
				network, err := infraprovider.Get().PrimaryNetwork()
				framework.ExpectNoError(err, "failed to get primary network information")
				if overrideNetworkName, _, _ := getOverrideNetwork(); overrideNetworkName != "" {
					overrideNetwork, err := infraprovider.Get().GetNetwork(overrideNetworkName)
					framework.ExpectNoError(err, "over ride network must exist")
					network = overrideNetwork
				}
				if network.Name() == "host" {
					skipper.Skipf("Skipping as host network doesn't support multiple external gateways")
				}
				gwContainers, addressesv4, addressesv6 = setupGatewayContainers(f, providerCtx, nodes, network, gwContainer1Template, gwContainer2Template,
					srcPodName, gwUDPPort, gwTCPPort, podUDPPort, podTCPPort, ecmpRetry, false)
			})

			ginkgo.AfterEach(func() {
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
					gomega.Eventually(infraprovider.Get().ExecExternalContainerCommand).
						WithArguments(gwContainer, []string{"ping", "-B", "-c1", "-W1", "-I", addresses.targetIPs[0], addresses.srcPodIP}).
						WithTimeout(testTimeout).
						ShouldNot(gomega.BeEmpty(), "Failed to ping %s from container %s", addresses.srcPodIP, gwContainer.Name)
				}

				ginkgo.By("Verifying connectivity to the pod from external gateways with large packets > pod MTU")
				for _, gwContainer := range gwContainers {
					gomega.Eventually(infraprovider.Get().ExecExternalContainerCommand).
						WithArguments(gwContainer, []string{"ping", "-s", "1420", "-c1", "-W1", addresses.srcPodIP}).
						WithTimeout(testTimeout).
						ShouldNot(gomega.BeEmpty(), "Failed to ping %s from container %s", addresses.srcPodIP, gwContainer.Name)
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
					go checkReceivedPacketsOnExternalContainer(gwContainer, srcPodName, anyLink, []string{icmpToDump}, &tcpDumpSync)
				}

				pingSync := sync.WaitGroup{}

				// spawn a goroutine to asynchronously (to speed up the test)
				// to ping the gateway loopbacks on both containers via ECMP.
				for _, address := range addresses.targetIPs {
					pingSync.Add(1)
					go func(target string) {
						defer ginkgo.GinkgoRecover()
						defer pingSync.Done()
						gomega.Eventually(e2ekubectl.RunKubectl).
							WithArguments(f.Namespace.Name, "exec", srcPodName, "--", "ping", "-c1", "-W1", target).
							WithTimeout(testTimeout).
							ShouldNot(gomega.BeEmpty(), "Failed to ping remote gateway %s from pod %s", target, srcPodName)
					}(address)
				}
				pingSync.Wait()
				tcpDumpSync.Wait()

			}, ginkgo.Entry("IPV4", &addressesv4, "icmp"),
				ginkgo.Entry("IPV6", &addressesv6, "icmp6"))

			// This test runs a listener on the external container, returning the host name both on tcp and udp.
			// The src pod tries to hit the remote address until both the containers are hit.
			ginkgo.DescribeTable("Should validate TCP/UDP connectivity to multiple external gateways for a UDP / TCP scenario", func(addresses *gatewayTestIPs, protocol string, gwPort, podPort int) {
				if addresses.srcPodIP == "" || addresses.nodeIP == "" {
					skipper.Skipf("Skipping as pod ip / node ip are not set pod ip %s node ip %s", addresses.srcPodIP, addresses.nodeIP)
				}
				createAPBExternalRouteCRWithStaticHop(defaultPolicyName, f.Namespace.Name, false, addresses.gatewayIPs...)

				for _, container := range gwContainers {
					reachPodFromGateway(container, addresses.srcPodIP, strconv.Itoa(podPort), srcPodName, protocol)
				}

				expectedHostNames := hostNamesForExternalContainers(gwContainers)
				framework.Logf("Expected hostnames are %v", expectedHostNames)

				returnedHostNames := make(map[string]struct{})
				success := false

				// Picking only the first address, the one the udp listener is set for
				gwIP := addresses.targetIPs[0]
				for i := 0; i < singleTargetRetries; i++ {
					hostname := pokeHostnameViaNC(srcPodName, f.Namespace.Name, protocol, gwIP, gwPort)
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

			}, ginkgo.Entry("IPV4 udp", &addressesv4, "udp", gwUDPPort, podUDPPort),
				ginkgo.Entry("IPV4 tcp", &addressesv4, "tcp", gwTCPPort, podTCPPort),
				ginkgo.Entry("IPV6 udp", &addressesv6, "udp", gwUDPPort, podUDPPort),
				ginkgo.Entry("IPV6 tcp", &addressesv6, "tcp", gwTCPPort, podTCPPort))
		})

		var _ = ginkgo.Describe("e2e multiple external gateway ingress stale conntrack entry deletion validation", func() {
			const (
				svcname              string = "novxlan-externalgw-ecmp"
				gwContainer1Template string = "gw-test-container1-%d"
				gwContainer2Template string = "gw-test-container2-%d"
				srcPodName           string = "e2e-exgw-src-pod"
				gatewayPodName1      string = "e2e-gateway-pod1"
				gatewayPodName2      string = "e2e-gateway-pod2"
			)

			f := wrappedTestFramework(svcname)

			var (
				servingNamespace         string
				addressesv4, addressesv6 gatewayTestIPs
				sleepCommand             []string
				nodes                    *corev1.NodeList
				err                      error
				providerCtx              infraapi.Context
				gwContainers             []infraapi.ExternalContainer
			)

			ginkgo.BeforeEach(func() {
				providerCtx = infraprovider.Get().NewTestContext()
				// retrieve worker node names
				nodes, err = e2enode.GetBoundedReadySchedulableNodes(context.TODO(), f.ClientSet, 3)
				framework.ExpectNoError(err)
				if len(nodes.Items) < 3 {
					framework.Failf(
						"Test requires >= 3 Ready nodes, but there are only %v nodes",
						len(nodes.Items))
				}
				network, err := infraprovider.Get().PrimaryNetwork()
				framework.ExpectNoError(err, "failed to get primary network information")
				if overrideNetworkStr, _, _ := getOverrideNetwork(); overrideNetworkStr != "" {
					overrideNetwork, err := infraprovider.Get().GetNetwork(overrideNetworkStr)
					framework.ExpectNoError(err, "over ride network must exist")
					network = overrideNetwork
				}
				if network.Name() == "host" {
					skipper.Skipf("Skipping as host network doesn't support multiple external gateways")
				}

				ns, err := f.CreateNamespace(context.TODO(), "exgw-conntrack-serving", nil)
				framework.ExpectNoError(err)
				servingNamespace = ns.Name

				gwContainers, addressesv4, addressesv6 = setupGatewayContainersForConntrackTest(f, providerCtx, nodes, network, gwContainer1Template, gwContainer2Template, srcPodName)
				sleepCommand = []string{"bash", "-c", "sleep 20000"}
				_, err = createGenericPodWithLabel(f, gatewayPodName1, nodes.Items[0].Name, servingNamespace, sleepCommand, map[string]string{"name": gatewayPodName1, "gatewayPod": "true"})
				framework.ExpectNoError(err, "Create the external gw pods to manage the src app pod namespace, failed: %v", err)
				_, err = createGenericPodWithLabel(f, gatewayPodName2, nodes.Items[1].Name, servingNamespace, sleepCommand, map[string]string{"name": gatewayPodName2, "gatewayPod": "true"})
				framework.ExpectNoError(err, "Create the external gw pods to manage the src app pod namespace, failed: %v", err)
			})

			ginkgo.AfterEach(func() {
				deleteAPBExternalRouteCR(defaultPolicyName)
			})

			ginkgo.DescribeTable("Static Hop: Should validate conntrack entry deletion for TCP/UDP traffic via multiple external gateways a.k.a ECMP routes", func(addresses *gatewayTestIPs, protocol string) {
				if addresses.srcPodIP == "" || addresses.nodeIP == "" {
					skipper.Skipf("Skipping as pod ip / node ip are not set pod ip %s node ip %s", addresses.srcPodIP, addresses.nodeIP)
				}
				ginkgo.By("Create a static hop in an Admin Policy Based External Route CR targeting the app namespace to get managed by external gateways")
				createAPBExternalRouteCRWithStaticHop(defaultPolicyName, f.Namespace.Name, false, addresses.gatewayIPs...)
				network, err := infraprovider.Get().PrimaryNetwork()
				framework.ExpectNoError(err, "failed to get primary network information")
				if overrideNetworkName, _, _ := getOverrideNetwork(); overrideNetworkName != "" {
					overrideNetwork, err := infraprovider.Get().GetNetwork(overrideNetworkName)
					framework.ExpectNoError(err, "over ride network must exist")
					network = overrideNetwork
				}
				macAddressGW := make([]string, 2)
				for i, container := range gwContainers {
					ginkgo.By("Start iperf3 client from external container to connect to iperf3 server running at the src pod")
					// note iperf3 even when using udp also spawns tcp connection first; so we indirectly also have the tcp connection when using "-u" flag
					_, err = infraprovider.Get().ExecExternalContainerCommand(container, []string{"iperf3", "-u", "-c", addresses.srcPodIP, "-p", fmt.Sprintf("%d", 5201+i), "-b", "1M", "-i", "1", "-t", "3", "&"})
					framework.ExpectNoError(err, "failed to connect to iperf3 server")
					networkInfo, err := infraprovider.Get().GetExternalContainerNetworkInterface(container, network)
					framework.ExpectNoError(err, "failed to get network %s info from external container %s", network.Name(), container.Name)

					// Trim leading 0s because conntrack dumped labels are just integers
					// in hex without leading 0s.
					macAddressGW[i] = strings.TrimLeft(strings.Replace(networkInfo.MAC, ":", "", -1), "0")
				}
				ginkgo.By("Check if conntrack entries for ECMP routes are created for the 2 external gateways")
				nodeName := getPod(f, srcPodName).Spec.NodeName
				podConnEntriesWithMACLabelsSet := 2
				totalPodConnEntries := 4
				gomega.Eventually(func() int {
					return pokeConntrackEntries(nodeName, addresses.srcPodIP, protocol, macAddressGW)
				}, time.Minute, 5).Should(gomega.Equal(podConnEntriesWithMACLabelsSet))
				gomega.Expect(pokeConntrackEntries(nodeName, addresses.srcPodIP, protocol, nil)).To(gomega.Equal(totalPodConnEntries))

				ginkgo.By("Check if conntrack entries for ECMP routes are removed for the deleted external gateway if traffic is UDP")
				updateAPBExternalRouteCRWithStaticHop(defaultPolicyName, f.Namespace.Name, false, addresses.gatewayIPs[0])

				podConnEntriesWithMACLabelsSet = 1 // we still have the conntrack entry for the remaining gateway
				totalPodConnEntries = 2            // remaining hop: 1 labeled + 1 unlabeled zone copy

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
				totalPodConnEntries = 0            // labeled and unlabeled copies of both hops are deleted

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

			ginkgo.DescribeTable("Dynamic Hop: Should validate conntrack entry deletion for TCP/UDP traffic via multiple external gateways a.k.a ECMP routes", func(addresses *gatewayTestIPs, protocol string, removalType GatewayRemovalType) {
				if addresses.srcPodIP == "" || addresses.nodeIP == "" {
					skipper.Skipf("Skipping as pod ip / node ip are not set pod ip %s node ip %s", addresses.srcPodIP, addresses.nodeIP)
				}

				if removalType == GatewayNotReady {
					recreatePodWithReadinessProbe(f, gatewayPodName2, nodes.Items[1].Name, servingNamespace, sleepCommand, map[string]string{"name": gatewayPodName2, "gatewayPod": "true"})
				}

				for i, gwPod := range []string{gatewayPodName1, gatewayPodName2} {
					annotateMultusNetworkStatusInPodGateway(gwPod, servingNamespace, []string{addresses.gatewayIPs[i], addresses.gatewayIPs[i]})
				}

				createAPBExternalRouteCRWithDynamicHop(defaultPolicyName, f.Namespace.Name, servingNamespace, false, addresses.gatewayIPs)
				network, err := infraprovider.Get().PrimaryNetwork()
				framework.ExpectNoError(err, "failed to get primary network information")
				if overrideNetworkName, _, _ := getOverrideNetwork(); overrideNetworkName != "" {
					overrideNetwork, err := infraprovider.Get().GetNetwork(overrideNetworkName)
					framework.ExpectNoError(err, "over ride network must exist")
					network = overrideNetwork
				}
				macAddressGW := make([]string, 2)
				for i, container := range gwContainers {
					ginkgo.By("Start iperf3 client from external container to connect to iperf3 server running at the src pod")
					// note iperf3 even when using udp also spawns tcp connection first; so we indirectly also have the tcp connection when using "-u" flag
					_, err = infraprovider.Get().ExecExternalContainerCommand(container, []string{"iperf3", "-u", "-c", addresses.srcPodIP,
						"-p", fmt.Sprintf("%d", 5201+i), "-b", "1M", "-i", "1", "-t", "3", "&"})
					framework.ExpectNoError(err, "failed to start iperf3 client command")
					networkInterface, err := infraprovider.Get().GetExternalContainerNetworkInterface(container, network)
					framework.ExpectNoError(err, "failed to get network %s information for container %s", network.Name(), container.Name)
					// Trim leading 0s because conntrack dumped labels are just integers
					// in hex without leading 0s.
					macAddressGW[i] = strings.TrimLeft(strings.Replace(networkInterface.MAC, ":", "", -1), "0")
				}

				ginkgo.By("Check if conntrack entries for ECMP routes are created for the 2 external gateways")
				nodeName := getPod(f, srcPodName).Spec.NodeName
				podConnEntriesWithMACLabelsSet := 2 // TCP
				totalPodConnEntries := 4            // TCP

				gomega.Eventually(func() int {
					n := pokeConntrackEntries(nodeName, addresses.srcPodIP, protocol, macAddressGW)
					klog.Infof("Number of entries with macAddressGW %s:%d", macAddressGW, n)
					return n
				}, time.Minute, 5).Should(gomega.Equal(podConnEntriesWithMACLabelsSet))
				gomega.Expect(pokeConntrackEntries(nodeName, addresses.srcPodIP, protocol, nil)).To(gomega.Equal(totalPodConnEntries)) // total conntrack entries for this pod/protocol

				cleanUpFn := handleGatewayPodRemoval(f, removalType, gatewayPodName2, servingNamespace)
				if cleanUpFn != nil {
					defer cleanUpFn()
				}

				ginkgo.By("Check if conntrack entries for ECMP routes are removed for the deleted external gateway if traffic is UDP")

				podConnEntriesWithMACLabelsSet = 1 // we still have the conntrack entry for the remaining gateway
				totalPodConnEntries = 2            // remaining hop: 1 labeled + 1 unlabeled zone copy

				gomega.Eventually(func() int {
					n := pokeConntrackEntries(nodeName, addresses.srcPodIP, protocol, macAddressGW)
					klog.Infof("Number of entries with macAddressGW %s:%d", macAddressGW, n)
					return n
				}, 10).Should(gomega.Equal(podConnEntriesWithMACLabelsSet))
				gomega.Expect(pokeConntrackEntries(nodeName, addresses.srcPodIP, protocol, nil)).To(gomega.Equal(totalPodConnEntries))

				ginkgo.By("Update first external gateway pod's labels so it no longer matches the policy")
				p := getGatewayPod(f, servingNamespace, gatewayPodName1)
				p.Labels = map[string]string{"name": gatewayPodName1}
				updatePod(f, p)

				ginkgo.By("Check if conntrack entries for ECMP routes are removed for the deleted external gateway if traffic is UDP")

				podConnEntriesWithMACLabelsSet = 0 //we don't have any remaining gateways left
				totalPodConnEntries = 0            // labeled and unlabeled copies of both hops are deleted
				gomega.Eventually(func() int {
					n := pokeConntrackEntries(nodeName, addresses.srcPodIP, protocol, macAddressGW)
					klog.Infof("Number of entries with macAddressGW %s:%d", macAddressGW, n)
					return n
				}, 5).Should(gomega.Equal(podConnEntriesWithMACLabelsSet))
				gomega.Expect(pokeConntrackEntries(nodeName, addresses.srcPodIP, protocol, nil)).To(gomega.Equal(totalPodConnEntries))
				checkAPBExternalRouteStatus(defaultPolicyName)
			},
				ginkgo.Entry("IPV4 udp + pod label update", &addressesv4, "udp", GatewayUpdate),
				ginkgo.Entry("IPV4 tcp + pod label update", &addressesv4, "tcp", GatewayUpdate),
				ginkgo.Entry("IPV6 udp + pod label update", &addressesv6, "udp", GatewayUpdate),
				ginkgo.Entry("IPV6 tcp + pod label update", &addressesv6, "tcp", GatewayUpdate),
				ginkgo.Entry("IPV4 udp + pod delete", &addressesv4, "udp", GatewayDelete),
				ginkgo.Entry("IPV4 tcp + pod delete", &addressesv4, "tcp", GatewayDelete),
				ginkgo.Entry("IPV6 udp + pod delete", &addressesv6, "udp", GatewayDelete),
				ginkgo.Entry("IPV6 tcp + pod delete", &addressesv6, "tcp", GatewayDelete),
				ginkgo.Entry("IPV4 udp + pod deletion timestamp", &addressesv4, "udp", GatewayDeletionTimestamp),
				ginkgo.Entry("IPV4 tcp + pod deletion timestamp", &addressesv4, "tcp", GatewayDeletionTimestamp),
				ginkgo.Entry("IPV6 udp + pod deletion timestamp", &addressesv6, "udp", GatewayDeletionTimestamp),
				ginkgo.Entry("IPV6 tcp + pod deletion timestamp", &addressesv6, "tcp", GatewayDeletionTimestamp),
				ginkgo.Entry("IPV4 udp + pod not ready", &addressesv4, "udp", GatewayNotReady),
				ginkgo.Entry("IPV4 tcp + pod not ready", &addressesv4, "tcp", GatewayNotReady),
				ginkgo.Entry("IPV6 udp + pod not ready", &addressesv6, "udp", GatewayNotReady),
				ginkgo.Entry("IPV6 tcp + pod not ready", &addressesv6, "tcp", GatewayNotReady),
			)
		})

		// BFD Tests are dual of external gateway. The only difference is that they enable BFD on ovn and
		// on the external containers, and after doing one round veryfing that the traffic reaches both containers,
		// they delete one and verify that the traffic is always reaching the only alive container.
		var _ = ginkgo.Context("BFD", func() {

			var _ = ginkgo.Describe("e2e non-vxlan external gateway through a dynamic hop", func() {
				const (
					svcname              string        = "externalgw-pod-novxlan"
					gwContainer1Template string        = "ex-gw-container1-%d"
					gwContainer2Template string        = "ex-gw-container2-%d"
					srcPingPodName       string        = "e2e-exgw-src-ping-pod"
					gatewayPodName1      string        = "e2e-gateway-pod1"
					gatewayPodName2      string        = "e2e-gateway-pod2"
					ecmpRetry            int           = 20
					testTimeout          time.Duration = 20 * time.Second
					defaultPolicyName                  = "default-route-policy"
				)

				var (
					sleepCommand             = []string{"bash", "-c", "sleep 20000"}
					addressesv4, addressesv6 gatewayTestIPs
					servingNamespace         string
					gwContainers             []infraapi.ExternalContainer
					providerCtx              infraapi.Context
				)

				f := wrappedTestFramework(svcname)

				ginkgo.BeforeEach(func() {
					providerCtx = infraprovider.Get().NewTestContext()
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
					network, err := infraprovider.Get().PrimaryNetwork()
					framework.ExpectNoError(err, "failed to get primary network information")
					if overrideNetworkName, _, _ := getOverrideNetwork(); overrideNetworkName != "" {
						overrideNetwork, err := infraprovider.Get().GetNetwork(overrideNetworkName)
						framework.ExpectNoError(err, "over ride network must exist")
						network = overrideNetwork
					}
					gwContainers, addressesv4, addressesv6 = setupGatewayContainers(f, providerCtx, nodes, network,
						gwContainer1Template, gwContainer2Template, srcPingPodName, gwUDPPort, gwTCPPort, podUDPPort, podTCPPort, ecmpRetry, true)
					ginkgo.By("Create the external route policy with dynamic hops to manage the src app pod namespace")

					setupPolicyBasedGatewayPods(f, nodes, network, gatewayPodName1, gatewayPodName2, servingNamespace, sleepCommand, addressesv4, addressesv6)
				})

				ginkgo.AfterEach(func() {
					deleteAPBExternalRouteCR(defaultPolicyName)

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
							gomega.Eventually(infraprovider.Get().ExecExternalContainerCommand).
								WithArguments(gwContainer, []string{"ping", "-B", "-c1", "-W1", "-I", addresses.targetIPs[0], addresses.srcPodIP}).
								WithTimeout(testTimeout).
								ShouldNot(gomega.BeEmpty(), "Failed to ping %s from container %s", addresses.srcPodIP, gwContainer.Name)
						}

						// This is needed for bfd to sync up
						for _, gwContainer := range gwContainers {
							gomega.Eventually(isBFDPaired).
								WithArguments(gwContainer, addresses.nodeIP).
								WithTimeout(time.Minute).
								WithPolling(5*time.Second).
								Should(gomega.BeTrue(), "Bfd not paired")
						}

						tcpDumpSync := sync.WaitGroup{}
						tcpDumpSync.Add(len(gwContainers))
						for _, gwContainer := range gwContainers {
							go checkReceivedPacketsOnExternalContainer(gwContainer, srcPingPodName, anyLink, []string{icmpCommand}, &tcpDumpSync)
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
								gomega.Eventually(e2ekubectl.RunKubectl).
									WithArguments(f.Namespace.Name, "exec", srcPingPodName, "--", "ping", "-c1", "-W1", target).
									WithTimeout(testTimeout).
									ShouldNot(gomega.BeEmpty(), "Failed to ping remote gateway %s from pod %s", target, srcPingPodName)
							}(address)
						}

						pingSync.Wait()
						tcpDumpSync.Wait()

						if len(gwContainers) > 1 {
							ginkgo.By("Deleting one container")
							err := providerCtx.DeleteExternalContainer(gwContainers[1])
							framework.ExpectNoError(err, "failed to delete external container %s", gwContainers[1].Name)

							time.Sleep(bfdTimeout)

							tcpDumpSync = sync.WaitGroup{}
							tcpDumpSync.Add(1)
							go checkReceivedPacketsOnExternalContainer(gwContainers[0], srcPingPodName, anyLink, []string{icmpCommand}, &tcpDumpSync)

							// Verify the external gateway loopback address running on the external container is reachable and
							// that traffic from the source ping pod is proxied through the pod in the serving namespace
							ginkgo.By("Verifying connectivity via the gateway namespace to the remote addresses")
							pingSync = sync.WaitGroup{}

							for _, t := range addresses.targetIPs {
								pingSync.Add(1)
								go func(target string) {
									defer ginkgo.GinkgoRecover()
									defer pingSync.Done()
									gomega.Eventually(e2ekubectl.RunKubectl).
										WithArguments(f.Namespace.Name, "exec", srcPingPodName, "--", "ping", "-c1", "-W1", target).
										WithTimeout(testTimeout).
										ShouldNot(gomega.BeEmpty(), "Failed to ping remote gateway %s from pod %s", target, srcPingPodName)
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
					func(protocol string, addresses *gatewayTestIPs, gwPort int) {
						if addresses.srcPodIP == "" || addresses.nodeIP == "" {
							skipper.Skipf("Skipping as pod ip / node ip are not set pod ip %s node ip %s", addresses.srcPodIP, addresses.nodeIP)
						}
						createAPBExternalRouteCRWithDynamicHop(defaultPolicyName, f.Namespace.Name, servingNamespace, true, addressesv4.gatewayIPs)

						for _, gwContainer := range gwContainers {
							gomega.Eventually(infraprovider.Get().ExecExternalContainerCommand).
								WithArguments(gwContainer, []string{"ping", "-B", "-c1", "-W1", "-I", addresses.targetIPs[0], addresses.srcPodIP}).
								WithTimeout(testTimeout).
								ShouldNot(gomega.BeEmpty(), "Failed to ping %s from container %s", addresses.srcPodIP, gwContainer.Name)
						}

						for _, gwContainer := range gwContainers {
							gomega.Eventually(isBFDPaired).
								WithArguments(gwContainer, addresses.nodeIP).
								WithTimeout(time.Minute).
								WithPolling(5*time.Second).
								Should(gomega.BeTrue(), "Bfd not paired")
						}

						expectedHostNames := hostNamesForExternalContainers(gwContainers)
						framework.Logf("Expected hostnames are %v", expectedHostNames)

						returnedHostNames := make(map[string]struct{})
						gwIP := addresses.targetIPs[0]
						success := false
						for i := 0; i < singleTargetRetries; i++ {
							hostname := pokeHostnameViaNC(srcPingPodName, f.Namespace.Name, protocol, gwIP, gwPort)
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
							err := providerCtx.DeleteExternalContainer(gwContainers[1])
							framework.ExpectNoError(err, "failed to delete external container %s", gwContainers[1].Name)
							ginkgo.By("Waiting for BFD to sync")
							time.Sleep(bfdTimeout)

							// ECMP should direct all the traffic to the only container
							expectedHostName := hostNameForExternalContainer(gwContainers[0])

							ginkgo.By("Checking hostname multiple times")
							for i := 0; i < 20; i++ {
								hostname := pokeHostnameViaNC(srcPingPodName, f.Namespace.Name, protocol, gwIP, gwPort)
								gomega.Expect(expectedHostName).To(gomega.Equal(hostname), "Hostname returned by nc not as expected")
							}
						}
						checkAPBExternalRouteStatus(defaultPolicyName)
					},
					ginkgo.Entry("UDP ipv4", "udp", &addressesv4, gwUDPPort),
					ginkgo.Entry("TCP ipv4", "tcp", &addressesv4, gwTCPPort),
					ginkgo.Entry("UDP ipv6", "udp", &addressesv6, gwUDPPort),
					ginkgo.Entry("TCP ipv6", "tcp", &addressesv6, gwTCPPort))
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
					svcname              string        = "novxlan-externalgw-ecmp"
					gwContainer1Template string        = "gw-test-container1-%d"
					gwContainer2Template string        = "gw-test-container2-%d"
					testTimeout          time.Duration = 30 * time.Second
					ecmpRetry            int           = 20
					srcPodName                         = "e2e-exgw-src-pod"
				)

				var (
					gwContainers             []infraapi.ExternalContainer
					testContainer            = fmt.Sprintf("%s-container", srcPodName)
					testContainerFlag        = fmt.Sprintf("--container=%s", testContainer)
					f                        = wrappedTestFramework(svcname)
					providerCtx              infraapi.Context
					addressesv4, addressesv6 gatewayTestIPs
				)

				ginkgo.BeforeEach(func() {
					providerCtx = infraprovider.Get().NewTestContext()
					nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), f.ClientSet, 3)
					framework.ExpectNoError(err)
					if len(nodes.Items) < 3 {
						framework.Failf(
							"Test requires >= 3 Ready nodes, but there are only %v nodes",
							len(nodes.Items))
					}
					network, err := infraprovider.Get().PrimaryNetwork()
					framework.ExpectNoError(err, "failed to get primary network information")
					if overrideNetworkName, _, _ := getOverrideNetwork(); overrideNetworkName != "" {
						overrideNetwork, err := infraprovider.Get().GetNetwork(overrideNetworkName)
						framework.ExpectNoError(err, "over ride network must exist")
						network = overrideNetwork
					}
					if network.Name() == "host" {
						skipper.Skipf("Skipping as host network doesn't support multiple external gateways")
					}
					gwContainers, addressesv4, addressesv6 = setupGatewayContainers(f, providerCtx, nodes, network, gwContainer1Template,
						gwContainer2Template, srcPodName, gwUDPPort, gwTCPPort, podUDPPort, podTCPPort, ecmpRetry, true)
				})

				ginkgo.AfterEach(func() {
					deleteAPBExternalRouteCR(defaultPolicyName)
				})

				ginkgo.DescribeTable("Should validate ICMP connectivity to multiple external gateways for an ECMP scenario", func(addresses *gatewayTestIPs, icmpToDump string) {
					if addresses.srcPodIP == "" || addresses.nodeIP == "" {
						skipper.Skipf("Skipping as pod ip / node ip are not set pod ip %s node ip %s", addresses.srcPodIP, addresses.nodeIP)
					}
					createAPBExternalRouteCRWithStaticHop(defaultPolicyName, f.Namespace.Name, true, addresses.gatewayIPs...)

					for _, gwContainer := range gwContainers {
						gomega.Eventually(infraprovider.Get().ExecExternalContainerCommand).
							WithArguments(gwContainer, []string{"ping", "-c1", "-W1", addresses.srcPodIP}).
							WithTimeout(testTimeout).
							ShouldNot(gomega.BeEmpty(), "Failed to ping %s from container %s", addresses.srcPodIP, gwContainer.Name)
					}

					for _, gwContainer := range gwContainers {
						gomega.Eventually(isBFDPaired).
							WithArguments(gwContainer, addresses.nodeIP).
							WithTimeout(time.Minute).
							WithPolling(5*time.Second).
							Should(gomega.BeTrue(), "Bfd not paired")
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
						go checkReceivedPacketsOnExternalContainer(gwContainer, srcPodName, anyLink, []string{icmpToDump}, &tcpDumpSync)
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
							gomega.Eventually(e2ekubectl.RunKubectl).
								WithArguments(f.Namespace.Name, "exec", srcPodName, testContainerFlag, "--", "ping", "-c1", "-W1", target).
								WithTimeout(testTimeout).
								ShouldNot(gomega.BeEmpty(), "Failed to ping remote gateway %s from pod %s", target, srcPodName)
						}(address)
					}

					pingSync.Wait()
					tcpDumpSync.Wait()

					ginkgo.By("Deleting one container")
					err := providerCtx.DeleteExternalContainer(gwContainers[1])
					framework.ExpectNoError(err, "failed to delete external container %s", gwContainers[1].Name)
					time.Sleep(bfdTimeout)

					pingSync = sync.WaitGroup{}
					tcpDumpSync = sync.WaitGroup{}

					tcpDumpSync.Add(1)
					go checkReceivedPacketsOnExternalContainer(gwContainers[0], srcPodName, anyLink, []string{icmpToDump}, &tcpDumpSync)

					// spawn a goroutine to asynchronously (to speed up the test)
					// to ping the gateway loopbacks on both containers via ECMP.
					for _, address := range addresses.targetIPs {
						pingSync.Add(1)
						go func(target string) {
							defer ginkgo.GinkgoRecover()
							defer pingSync.Done()
							gomega.Eventually(e2ekubectl.RunKubectl).
								WithArguments(f.Namespace.Name, "exec", srcPodName, testContainerFlag, "--", "ping", "-c1", "-W1", target).
								WithTimeout(testTimeout).
								ShouldNot(gomega.BeEmpty(), "Failed to ping remote gateway %s from pod %s", target, srcPodName)
						}(address)
					}

					pingSync.Wait()
					tcpDumpSync.Wait()

				}, ginkgo.Entry("IPV4", &addressesv4, "icmp"),
					ginkgo.Entry("IPV6", &addressesv6, "icmp6"))

				// This test runs a listener on the external container, returning the host name both on tcp and udp.
				// The src pod tries to hit the remote address until both the containers are hit.
				ginkgo.DescribeTable("Should validate TCP/UDP connectivity to multiple external gateways for a UDP / TCP scenario", func(addresses *gatewayTestIPs, protocol string, gwPort int) {
					if addresses.srcPodIP == "" || addresses.nodeIP == "" {
						skipper.Skipf("Skipping as pod ip / node ip are not set pod ip %s node ip %s", addresses.srcPodIP, addresses.nodeIP)
					}
					createAPBExternalRouteCRWithStaticHop(defaultPolicyName, f.Namespace.Name, true, addresses.gatewayIPs...)

					for _, gwContainer := range gwContainers {
						gomega.Eventually(infraprovider.Get().ExecExternalContainerCommand).
							WithArguments(gwContainer, []string{"ping", "-c1", "-W1", addresses.srcPodIP}).
							WithTimeout(testTimeout).
							ShouldNot(gomega.BeEmpty(), "Failed to ping %s from container %s", addresses.srcPodIP, gwContainer.Name)
					}

					for _, gwContainer := range gwContainers {
						gomega.Eventually(isBFDPaired).
							WithArguments(gwContainer, addresses.nodeIP).
							WithTimeout(time.Minute).
							WithPolling(5*time.Second).
							Should(gomega.BeTrue(), "Bfd not paired")
					}

					expectedHostNames := hostNamesForExternalContainers(gwContainers)
					framework.Logf("Expected hostnames are %v", expectedHostNames)

					returnedHostNames := make(map[string]struct{})
					success := false

					// Picking only the first address, the one the udp listener is set for
					gwIP := addresses.targetIPs[0]
					for i := 0; i < singleTargetRetries; i++ {
						hostname := pokeHostnameViaNC(srcPodName, f.Namespace.Name, protocol, gwIP, gwPort)
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
					err := providerCtx.DeleteExternalContainer(gwContainers[1])
					framework.ExpectNoError(err, "failed to delete external container %s", gwContainers[1].Name)
					ginkgo.By("Waiting for BFD to sync")
					time.Sleep(bfdTimeout)

					// ECMP should direct all the traffic to the only container
					expectedHostName := hostNameForExternalContainer(gwContainers[0])

					ginkgo.By("Checking hostname multiple times")
					for i := 0; i < 20; i++ {
						hostname := pokeHostnameViaNC(srcPodName, f.Namespace.Name, protocol, gwIP, gwPort)
						gomega.Expect(expectedHostName).To(gomega.Equal(hostname), "Hostname returned by nc not as expected")
					}
				}, ginkgo.Entry("IPV4 udp", &addressesv4, "udp", gwUDPPort),
					ginkgo.Entry("IPV4 tcp", &addressesv4, "tcp", gwTCPPort),
					ginkgo.Entry("IPV6 udp", &addressesv6, "udp", gwUDPPort),
					ginkgo.Entry("IPV6 tcp", &addressesv6, "tcp", gwTCPPort))
			})
		})
	})

	var _ = ginkgo.Context("When validating the Admin Policy Based External Route status", func() {
		const (
			svcname              string = "novxlan-externalgw-ecmp"
			gwContainer1Template string = "gw-test-container1-%d"
			gwContainer2Template string = "gw-test-container2-%d"
			ecmpRetry            int    = 20
			srcPodName                  = "e2e-exgw-src-pod"
			duplicatedPolicy            = "duplicated"
		)

		f := wrappedTestFramework(svcname)

		var (
			addressesv4 gatewayTestIPs
			providerCtx infraapi.Context
		)

		ginkgo.BeforeEach(func() {
			providerCtx = infraprovider.Get().NewTestContext()
			nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), f.ClientSet, 3)
			framework.ExpectNoError(err)
			if len(nodes.Items) < 3 {
				framework.Failf(
					"Test requires >= 3 Ready nodes, but there are only %v nodes",
					len(nodes.Items))
			}
			network, err := infraprovider.Get().PrimaryNetwork()
			framework.ExpectNoError(err, "failed to get primary network information")
			if overrideNetworkName, _, _ := getOverrideNetwork(); overrideNetworkName != "" {
				overrideNetwork, err := infraprovider.Get().GetNetwork(overrideNetworkName)
				framework.ExpectNoError(err, "over ride network must exist")
				network = overrideNetwork
			}
			if network.Name() == "host" {
				skipper.Skipf("Skipping as host network doesn't support multiple external gateways")
			}
			_, addressesv4, _ = setupGatewayContainers(f, providerCtx, nodes, network, gwContainer1Template, gwContainer2Template, srcPodName,
				gwUDPPort, gwTCPPort, podUDPPort, podTCPPort, ecmpRetry, false)
		})

		ginkgo.AfterEach(func() {
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
func setupGatewayContainers(f *framework.Framework, providerCtx infraapi.Context, nodes *corev1.NodeList, network infraapi.Network, container1Template, container2Template,
	srcPodName string, gwUDPPort, gwTCPPort, podUDPPort, podHTTPPort, numOfIPs int, setupBFD bool) ([]infraapi.ExternalContainer, gatewayTestIPs, gatewayTestIPs) {

	var err error
	externalContainer1 := infraapi.ExternalContainer{Name: getContainerName(container1Template, uint16(gwTCPPort)),
		Image: externalContainerImage, Network: network, CmdArgs: []string{}, ExtPort: uint16(gwTCPPort)}
	externalContainer2 := infraapi.ExternalContainer{Name: getContainerName(container2Template, uint16(gwTCPPort)),
		Image: externalContainerImage, Network: network, CmdArgs: []string{}, ExtPort: uint16(gwTCPPort)}

	gwContainers := []infraapi.ExternalContainer{externalContainer1, externalContainer2}
	addressesv4 := gatewayTestIPs{targetIPs: make([]string, 0)}
	addressesv6 := gatewayTestIPs{targetIPs: make([]string, 0)}

	ginkgo.By("Creating the gateway containers for the icmp test")
	// for host networked containers, we don't look-up the IP addresses and instead rely on overrides. container engine
	// is unable to supply this IP information.
	if network.Name() == "host" {
		gwContainers = []infraapi.ExternalContainer{externalContainer1}
		externalContainer1, err = providerCtx.CreateExternalContainer(externalContainer1)
		framework.ExpectNoError(err, "failed to create external container: %s", externalContainer1.String())
		providerCtx.AddCleanUpFn(func() error {
			return providerCtx.DeleteExternalContainer(externalContainer1)
		})
		overrideNetwork, ipv4, ipv6 := getOverrideNetwork()
		gomega.Expect(overrideNetwork).Should(gomega.Equal(network.Name()), "network is 'host' for external container, therefore require host IP information")
		// TODO; why do we require both IPs?
		if ipv4 == "" && ipv6 == "" {
			framework.Failf("host network is specified therefore, IPs must be defined for the container")
		}
		if ipv4 != "" {
			addressesv4.gatewayIPs = append(addressesv4.gatewayIPs, ipv4)
		}
		if ipv6 != "" {
			addressesv6.gatewayIPs = append(addressesv6.gatewayIPs, ipv6)
		}
	} else {
		for i, gwContainer := range gwContainers {
			gwContainers[i], err = providerCtx.CreateExternalContainer(gwContainer)
			framework.ExpectNoError(err, "failed to create external container: %s", gwContainer.String())
			if gwContainers[i].GetIPv4() != "" {
				addressesv4.gatewayIPs = append(addressesv4.gatewayIPs, gwContainers[i].GetIPv4())
			}
			if gwContainers[i].GetIPv6() != "" {
				addressesv6.gatewayIPs = append(addressesv6.gatewayIPs, gwContainers[i].GetIPv6())
			}
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
	if network.Name() != "host" {
		nodeInf, err := infraprovider.Get().GetK8NodeNetworkInterface(node.Name, network)
		framework.ExpectNoError(err, "failed to get network interface info for an interface on network %s within node %s", network.Name(), node.Name)
		addressesv4.nodeIP, addressesv6.nodeIP = nodeInf.IPv4, nodeInf.IPv6
	} else {
		nodeList := &corev1.NodeList{}
		nodeList.Items = append(nodeList.Items, node)

		addressesv4.nodeIP = e2enode.FirstAddressByTypeAndFamily(nodeList, corev1.NodeInternalIP, corev1.IPv4Protocol)
		addressesv6.nodeIP = e2enode.FirstAddressByTypeAndFamily(nodeList, corev1.NodeInternalIP, corev1.IPv6Protocol)
	}

	framework.Logf("the pod side node is %s and the source node ip is %s - %s", node.Name, addressesv4.nodeIP, addressesv6.nodeIP)

	ginkgo.By("Creating the source pod to reach the destination ips from")

	args := []string{
		"netexec",
		fmt.Sprintf("--http-port=%d", podHTTPPort),
		fmt.Sprintf("--udp-port=%d", podUDPPort),
	}
	clientPod, err := createPod(f, srcPodName, node.Name, f.Namespace.Name, []string{}, map[string]string{}, func(p *corev1.Pod) {
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
	setupListenersOrDie := func(container infraapi.ExternalContainer, gwAddress string) {
		_, err = infraprovider.Get().ExecExternalContainerCommand(container,
			[]string{"bash", "-c", fmt.Sprintf("while true; do echo $(hostname) | nc -l -u %s %d; done &", gwAddress, gwUDPPort)})
		framework.ExpectNoError(err, "failed to setup UDP listener for %s on %s", gwAddress, container)

		_, err = infraprovider.Get().ExecExternalContainerCommand(container,
			[]string{"bash", "-c", fmt.Sprintf("while true; do echo $(hostname) | nc -l %s %d; done &", gwAddress, gwTCPPort)})
		framework.ExpectNoError(err, "failed to setup TCP listener for %s on %s", gwAddress, container)
	}

	// The target ips are addresses added to the lo of each container.
	// By setting the gateway annotation and using them as destination, we verify that
	// the routing is able to reach the containers.
	// A route back to the src pod must be set in order for the ping reply to work.
	for _, gwContainer := range gwContainers {
		if testIPv4 {
			ginkgo.By(fmt.Sprintf("Setting up the destination ips to %s", gwContainer.Name))
			for _, address := range addressesv4.targetIPs {
				framework.Logf("adding IP %q to gateway container %q", address, gwContainer.Name)
				_, err = infraprovider.Get().ExecExternalContainerCommand(gwContainer, []string{"ip", "address", "add", address + "/32", "dev", "lo"})
				framework.ExpectNoError(err, "failed to add the loopback ip to dev lo on the test container %s", gwContainer.Name)
				providerCtx.AddCleanUpFn(func() error {
					infraprovider.Get().ExecExternalContainerCommand(gwContainer, []string{"ip", "address", "del", address + "/32", "dev", "lo"})
					return nil
				})
			}

			ginkgo.By(fmt.Sprintf("Adding a route from %s to the src pod", gwContainer.Name))
			_, err = infraprovider.Get().ExecExternalContainerCommand(gwContainer, []string{"ip", "route", "add", addressesv4.srcPodIP, "via", addressesv4.nodeIP})
			framework.ExpectNoError(err, "failed to add the pod host route on the test container %s", gwContainer.Name)
			providerCtx.AddCleanUpFn(func() error {
				infraprovider.Get().ExecExternalContainerCommand(gwContainer, []string{"ip", "route", "del", addressesv4.srcPodIP, "via", addressesv4.nodeIP})
				return nil
			})

			// cluster nodes don't know where to send ARP replies to requests
			// from the IPs that we just added to the containers so force an
			// entry on the neighbor table with a ping. This speeds up the tests
			// which would otherwise eventually discover the neighbor through
			// other link layer protocols.
			ginkgo.By(fmt.Sprintf("Adding node %s as neighbor of %s", addressesv4.nodeIP, gwContainer.Name))
			_, err = infraprovider.Get().ExecExternalContainerCommand(gwContainer, []string{"ping", "-c1", addressesv4.nodeIP})
			framework.ExpectNoError(err, "failed to add node %s as neighbor of %s", addressesv4.nodeIP, gwContainer.Name)

			ginkgo.By("Setting up the listeners on the gateway")
			setupListenersOrDie(gwContainer, addressesv4.targetIPs[0])
		}
		if testIPv6 {
			ginkgo.By(fmt.Sprintf("Setting up the destination ips to %s (ipv6)", gwContainer.Name))
			for _, address := range addressesv6.targetIPs {
				_, err = infraprovider.Get().ExecExternalContainerCommand(gwContainer, []string{"ip", "address", "add", address + "/128", "dev", "lo"})
				framework.ExpectNoError(err, "ipv6: failed to add the loopback ip to dev lo on the test container %s", gwContainer.Name)
				providerCtx.AddCleanUpFn(func() error {
					infraprovider.Get().ExecExternalContainerCommand(gwContainer, []string{"ip", "address", "del", address + "/128", "dev", "lo"})
					return nil
				})
			}
			ginkgo.By(fmt.Sprintf("Adding a route from %s to the src pod (ipv6)", gwContainer.Name))
			_, err = infraprovider.Get().ExecExternalContainerCommand(gwContainer, []string{"ip", "-6", "route", "add", addressesv6.srcPodIP, "via", addressesv6.nodeIP})
			framework.ExpectNoError(err, "ipv6: failed to add the pod host route on the test container %s", gwContainer.Name)

			ginkgo.By("Setting up the listeners on the gateway (v6)")
			setupListenersOrDie(gwContainer, addressesv6.targetIPs[0])
		}
	}
	if setupBFD {
		for _, gwContainer := range gwContainers {
			setupBFDOnExternalContainer(network, gwContainer, nodes.Items)
		}
	}

	return gwContainers, addressesv4, addressesv6
}

func setupPolicyBasedGatewayPods(f *framework.Framework, nodes *corev1.NodeList, network infraapi.Network, pod1, pod2, ns string, cmd []string, addressesv4, addressesv6 gatewayTestIPs) []string {
	gwPods := []string{pod1, pod2}
	if network.Name() == "host" {
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

// setupGatewayContainersForConntrackTest sets up iperf3 external containers, adds routes to src
// pods via the nodes, starts up iperf3 server on src-pod
func setupGatewayContainersForConntrackTest(f *framework.Framework, providerCtx infraapi.Context, nodes *corev1.NodeList, network infraapi.Network,
	gwContainer1Template, gwContainer2Template string, srcPodName string) ([]infraapi.ExternalContainer, gatewayTestIPs, gatewayTestIPs) {

	var (
		err       error
		clientPod *corev1.Pod
	)
	if network.Name() == "host" {
		panic("not supported")
	}
	addressesv4 := gatewayTestIPs{gatewayIPs: make([]string, 2)}
	addressesv6 := gatewayTestIPs{gatewayIPs: make([]string, 2)}
	ginkgo.By("Creating the gateway containers for the UDP test")
	gwExternalContainer1 := infraapi.ExternalContainer{Name: getContainerName(gwContainer1Template, 12345),
		Image: images.IPerf3(), Network: network, CmdArgs: []string{}, ExtPort: 12345}
	gwExternalContainer1, err = providerCtx.CreateExternalContainer(gwExternalContainer1)
	framework.ExpectNoError(err, "failed to create external container (%s)", gwExternalContainer1)

	gwExternalContainer2 := infraapi.ExternalContainer{Name: getContainerName(gwContainer2Template, 12345),
		Image: images.IPerf3(), Network: network, CmdArgs: []string{}, ExtPort: 12345}
	gwExternalContainer2, err = providerCtx.CreateExternalContainer(gwExternalContainer2)
	framework.ExpectNoError(err, "failed to create external container (%s)", gwExternalContainer2)
	if network.Name() == "host" {
		// manually cleanup because cleanup doesnt cleanup host network
		providerCtx.AddCleanUpFn(func() error {
			return providerCtx.DeleteExternalContainer(gwExternalContainer2)
		})
	}
	addressesv4.gatewayIPs[0], addressesv6.gatewayIPs[0] = gwExternalContainer1.GetIPv4(), gwExternalContainer1.GetIPv6()
	addressesv4.gatewayIPs[1], addressesv6.gatewayIPs[1] = gwExternalContainer2.GetIPv4(), gwExternalContainer2.GetIPv6()
	gwExternalContainers := []infraapi.ExternalContainer{gwExternalContainer1, gwExternalContainer2}
	node := nodes.Items[0]
	ginkgo.By("Creating the source pod to reach the destination ips from")
	clientPod, err = createPod(f, srcPodName, node.Name, f.Namespace.Name, []string{}, map[string]string{}, func(p *corev1.Pod) {
		p.Spec.Containers[0].Image = images.IPerf3()
	})
	framework.ExpectNoError(err)
	networkInfo, err := infraprovider.Get().GetK8NodeNetworkInterface(node.Name, network)
	framework.ExpectNoError(err, "failed to get kubernetes node %s network information for network %s", node.Name, network.Name())
	addressesv4.nodeIP, addressesv6.nodeIP = networkInfo.IPv4, networkInfo.IPv6
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
	for _, gwExternalContainer := range gwExternalContainers {
		ginkgo.By(fmt.Sprintf("Install iproute in %s", gwExternalContainer.Name))
		_, err = infraprovider.Get().ExecExternalContainerCommand(gwExternalContainer, []string{"dnf", "install", "-y", "iproute"})
		framework.ExpectNoError(err, "failed to install iproute package on the test container %s", gwExternalContainer.Name)
		if testIPv4 {
			ginkgo.By(fmt.Sprintf("Adding a route from %s to the src pod with IP %s", gwExternalContainer.Name, addressesv4.srcPodIP))
			_, err = infraprovider.Get().ExecExternalContainerCommand(gwExternalContainer, []string{"ip", "-4", "route", "add", addressesv4.srcPodIP,
				"via", addressesv4.nodeIP, "dev", infraprovider.Get().ExternalContainerPrimaryInterfaceName()})
			framework.ExpectNoError(err, "failed to add the pod host route on the test container %s", gwExternalContainer.Name)
			providerCtx.AddCleanUpFn(func() error {
				_, err = infraprovider.Get().ExecExternalContainerCommand(gwExternalContainer, []string{"ip", "-4", "route", "del", addressesv4.srcPodIP,
					"via", addressesv4.nodeIP, "dev", infraprovider.Get().ExternalContainerPrimaryInterfaceName()})
				if err != nil {
					return fmt.Errorf("failed to remove IPv4 route from external container %s: %v", gwExternalContainer.Name, err)
				}
				return nil
			})
		}
		if testIPv6 {
			ginkgo.By(fmt.Sprintf("Adding a route from %s to the src pod (ipv6)", gwExternalContainer.Name))
			_, err = infraprovider.Get().ExecExternalContainerCommand(gwExternalContainer, []string{"ip", "-6", "route", "add", addressesv6.srcPodIP, "via", addressesv6.nodeIP})
			framework.ExpectNoError(err, "ipv6: failed to add the pod host route on the test container %s", gwExternalContainer)
			providerCtx.AddCleanUpFn(func() error {
				_, err = infraprovider.Get().ExecExternalContainerCommand(gwExternalContainer, []string{"ip", "-6", "route", "del", addressesv6.srcPodIP, "via", addressesv6.nodeIP})
				if err != nil {
					return fmt.Errorf("failed to delete IPv6 route from external container %s: %v", gwExternalContainer.Name, err)
				}
				return nil
			})
		}
	}
	return gwExternalContainers, addressesv4, addressesv6
}

func reachPodFromGateway(srcContainer infraapi.ExternalContainer, targetAddress, targetPort, targetPodName, protocol string) {
	ginkgo.By(fmt.Sprintf("Checking that %s can reach the pod", srcContainer))
	var cmd []string
	if protocol == "tcp" {
		cmd = []string{"curl", "-s", fmt.Sprintf("http://%s/hostname", net.JoinHostPort(targetAddress, targetPort))}
	} else {
		cmd = []string{"bash", "-c", "cat <(echo hostname) <(sleep 1) | nc -u " + targetAddress + " " + targetPort}
	}
	res, err := infraprovider.Get().ExecExternalContainerCommand(srcContainer, cmd)
	framework.ExpectNoError(err, "Failed to reach pod %s (%s) from external container %s", targetAddress, protocol, srcContainer)
	gomega.Expect(strings.Trim(res, "\n")).To(gomega.Equal(targetPodName))
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
	gwIPs := sets.List(sets.New(gateways...))
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
	gwIPs := sets.List(sets.New(gateways...))
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

func getGatewayPod(f *framework.Framework, podNamespace, podName string) *corev1.Pod {
	pod, err := f.ClientSet.CoreV1().Pods(podNamespace).Get(context.Background(), podName, metav1.GetOptions{})
	framework.ExpectNoError(err, fmt.Sprintf("unable to get pod: %s, err: %v", podName, err))
	return pod
}

func hostNamesForExternalContainers(containers []infraapi.ExternalContainer) map[string]struct{} {
	res := make(map[string]struct{})
	for _, c := range containers {
		hostName := hostNameForExternalContainer(c)
		res[hostName] = struct{}{}
	}
	return res
}

func hostNameForExternalContainer(container infraapi.ExternalContainer) string {
	res, err := infraprovider.Get().ExecExternalContainerCommand(container, []string{"hostname"})
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
	ovnKubernetesNamespace := deploymentconfig.Get().OVNKubernetesNamespace()
	ovsPodName, err := e2ekubectl.RunKubectl(ovnKubernetesNamespace, args...)
	framework.ExpectNoError(err, "failed to get the ovs pod on node %s", nodeName)
	args = []string{"exec", ovsPodName, "--", "ovs-appctl", "dpctl/dump-conntrack"}
	conntrackEntries, err := e2ekubectl.RunKubectl(ovnKubernetesNamespace, args...)
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

func setupBFDOnExternalContainer(network infraapi.Network, container infraapi.ExternalContainer, nodes []corev1.Node) {
	// we set a bfd peer for each address of each node
	for _, node := range nodes {
		// we must use container network for second bridge scenario
		// for host network we can use the node's ip
		var ipv4, ipv6 string
		if network.Name() != "host" {
			networkInfo, err := infraprovider.Get().GetK8NodeNetworkInterface(node.Name, network)
			framework.ExpectNoError(err, "failed to get network information from node %s for network %s", node.Name, network.Name())
			ipv4, ipv6 = networkInfo.IPv4, networkInfo.IPv6
		} else {
			nodeList := &corev1.NodeList{}
			nodeList.Items = append(nodeList.Items, node)

			ipv4 = e2enode.FirstAddressByTypeAndFamily(nodeList, corev1.NodeInternalIP, corev1.IPv4Protocol)
			ipv6 = e2enode.FirstAddressByTypeAndFamily(nodeList, corev1.NodeInternalIP, corev1.IPv6Protocol)
		}

		for _, a := range []string{ipv4, ipv6} {
			if a == "" {
				continue
			}
			// Configure the node as a bfd peer on the frr side
			_, err := infraprovider.Get().ExecExternalContainerCommand(container, []string{"bash", "-c",
				fmt.Sprintf(`cat << EOF >> /etc/frr/frr.conf

bfd
 peer %s
   no shutdown
 !
!
EOF
`, a)})
			framework.ExpectNoError(err, "failed to setup FRR peer %s in %s", a, container)
		}
	}
	_, err := infraprovider.Get().ExecExternalContainerCommand(container, []string{"/usr/libexec/frr/frrinit.sh", "start"})
	framework.ExpectNoError(err, "failed to start frr in %s", container)
}

func isBFDPaired(container infraapi.ExternalContainer, peer string) (bool, error) {
	res, err := infraprovider.Get().ExecExternalContainerCommand(container, []string{"bash", "-c", fmt.Sprintf("vtysh -c \"show bfd peer %s\"", peer)})
	if err != nil {
		return false, fmt.Errorf("failed to check bfd status in %s: %w", container, err)
	}
	return strings.Contains(res, "Status: up"), nil
}

func checkReceivedPacketsOnExternalContainer(container infraapi.ExternalContainer, srcPodName, link string, filter []string, wg *sync.WaitGroup) {
	defer ginkgo.GinkgoRecover()
	defer wg.Done()
	if len(link) == 0 {
		link = anyLink
	}
	_, err := infraprovider.Get().ExecExternalContainerCommand(container, append([]string{"timeout", "60", "tcpdump", "-c", "1", "-i", link}, filter...))
	framework.ExpectNoError(err, "Failed to detect packets from %s on gateway %s", srcPodName, container)
	framework.Logf("Packet successfully detected on gateway %s", container)
}

func setupPodWithReadinessProbe(f *framework.Framework, podName, nodeSelector, namespace string, command []string, labels map[string]string) (*corev1.Pod, error) {
	// Handle bash -c commands specially to preserve argument structure
	if len(command) >= 3 && command[0] == "bash" && command[1] == "-c" {
		// Extract the script part and wrap it to preserve logic
		script := strings.Join(command[2:], " ")
		command = []string{"bash", "-c", "touch /tmp/ready && (" + script + ")"}
	} else {
		// For non-bash commands, preserve their structure
		var quotedArgs []string
		for _, arg := range command {
			// Escape single quotes and wrap in single quotes
			escaped := strings.ReplaceAll(arg, "'", "'\"'\"'")
			quotedArgs = append(quotedArgs, "'"+escaped+"'")
		}
		command = []string{"bash", "-c", "touch /tmp/ready && " + strings.Join(quotedArgs, " ")}
	}
	return createPod(f, podName, nodeSelector, namespace, command, labels, func(p *corev1.Pod) {
		p.Spec.Containers[0].ReadinessProbe = &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				Exec: &corev1.ExecAction{
					Command: []string{"cat", "/tmp/ready"},
				},
			},
			InitialDelaySeconds: 5,
			PeriodSeconds:       5,
			FailureThreshold:    1,
		}
	})
}

func recreatePodWithReadinessProbe(f *framework.Framework, podName, nodeSelector, namespace string, command []string, labels map[string]string) {
	ginkgo.By(fmt.Sprintf("Delete second external gateway pod %s from ns %s", podName, namespace))
	err := deletePodWithWaitByName(context.TODO(), f.ClientSet, podName, namespace)
	gomega.Expect(err).NotTo(gomega.HaveOccurred(), fmt.Sprintf("Delete second external gateway pod %s from ns %s, failed: %v", podName, namespace, err))

	ginkgo.By(fmt.Sprintf("Create second external gateway pod %s from ns %s with readiness probe", podName, namespace))
	_, err = setupPodWithReadinessProbe(f, podName, nodeSelector, namespace, command, labels)
	gomega.Expect(err).NotTo(gomega.HaveOccurred(), fmt.Sprintf("Create second external gateway pod %s from ns %s with readiness probe, failed: %v", podName, namespace, err))
	gomega.Eventually(func() bool {
		var p *corev1.Pod
		p, err = f.ClientSet.CoreV1().Pods(namespace).Get(context.Background(), podName, metav1.GetOptions{})
		if err != nil {
			return false
		}
		for _, condition := range p.Status.Conditions {
			if condition.Type == corev1.PodReady {
				return condition.Status == corev1.ConditionTrue
			}
		}
		return false
	}).Should(gomega.Equal(true), fmt.Sprintf("Readiness probe for second external gateway pod %s from ns %s, failed: %v", podName, namespace, err))
}

func handleGatewayPodRemoval(f *framework.Framework, removalType GatewayRemovalType, gatewayPodName, servingNamespace string) func() {
	var err error
	switch removalType {
	case GatewayDelete:
		ginkgo.By(fmt.Sprintf("Delete second external gateway pod %s from ns %s", gatewayPodName, servingNamespace))
		err := deletePodWithWaitByName(context.TODO(), f.ClientSet, gatewayPodName, servingNamespace)
		framework.ExpectNoError(err, "Delete the gateway pod failed: %v", err)
		return nil
	case GatewayUpdate:
		ginkgo.By("Updating external gateway pod labels")
		p := getGatewayPod(f, servingNamespace, gatewayPodName)
		p.Labels = map[string]string{"name": gatewayPodName}
		updatePod(f, p)
		return nil
	case GatewayDeletionTimestamp:
		ginkgo.By("Setting finalizer then deleting external gateway pod with grace period to set deletion timestamp")
		p := getGatewayPod(f, servingNamespace, gatewayPodName)
		p.Finalizers = append(p.Finalizers, "k8s.ovn.org/external-gw-pod-finalizer")
		updatePod(f, p)
		gomega.Eventually(func() bool {
			p, err = f.ClientSet.CoreV1().Pods(servingNamespace).Get(context.Background(), gatewayPodName, metav1.GetOptions{})
			if err != nil {
				return false
			}
			return strings.Contains(strings.Join(p.GetFinalizers(), ","), "k8s.ovn.org/external-gw-pod-finalizer")
		}).Should(gomega.Equal(true), fmt.Sprintf("Update second external gateway pod %s from ns %s with finalizer, failed: %v", gatewayPodName, servingNamespace, err))

		p = getGatewayPod(f, servingNamespace, gatewayPodName)
		err = e2epod.DeletePodWithGracePeriod(context.Background(), f.ClientSet, p, 1000)
		framework.ExpectNoError(err, fmt.Sprintf("unable to delete pod with grace period: %s, err: %v", p.Name, err))

		gomega.Eventually(func() bool {
			p, err = f.ClientSet.CoreV1().Pods(servingNamespace).Get(context.Background(), gatewayPodName, metav1.GetOptions{})
			if err != nil {
				return false
			}
			return p.DeletionTimestamp != nil
		}).Should(gomega.BeTrue(), fmt.Sprintf("Gateway pod %s in ns %s should have deletion timestamp, failed: %v", gatewayPodName, servingNamespace, err))

		// return a function to remove the finalizer
		return func() {
			p = getGatewayPod(f, servingNamespace, gatewayPodName)
			p.Finalizers = []string{}
			updatePod(f, p)
		}
	case GatewayNotReady:
		ginkgo.By("Remove /tmp/ready in external gateway pod so that readiness probe fails")
		_, err = e2ekubectl.RunKubectl(servingNamespace, "exec", gatewayPodName, "--", "rm", "/tmp/ready")
		framework.ExpectNoError(err, fmt.Sprintf("unable to remove /tmp/ready in pod: %s, err: %v", gatewayPodName, err))
		gomega.Eventually(func() bool {
			var p *corev1.Pod
			p, err = f.ClientSet.CoreV1().Pods(servingNamespace).Get(context.Background(), gatewayPodName, metav1.GetOptions{})
			if err != nil {
				return false
			}
			podReadyStatus := corev1.ConditionTrue
			for _, condition := range p.Status.Conditions {
				if condition.Type == corev1.PodReady {
					podReadyStatus = condition.Status
					break
				}
			}
			return podReadyStatus == corev1.ConditionFalse
		}).WithTimeout(5*time.Minute).Should(gomega.Equal(true), fmt.Sprintf("Mark second external gateway pod %s from ns %s not ready, failed: %v", gatewayPodName, servingNamespace, err))
		return nil
	default:
		framework.Failf("unexpected GatewayRemovalType passed: %s", removalType)
		return nil
	}
}
