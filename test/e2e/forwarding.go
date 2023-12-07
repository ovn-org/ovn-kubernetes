package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/kubernetes/test/e2e/framework"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
)

type subnetInfo struct {
	Subnet  string
	Gateway string
}

func (s subnetInfo) isComplete() bool {
	return s.Subnet != "" && s.Gateway != ""
}

type extranetInfo struct {
	containerIP string
	subnetInfo  subnetInfo
}

func (e extranetInfo) isComplete() bool {
	return e.containerIP != "" && e.subnetInfo.isComplete()
}

var _ = ginkgo.Describe("Drop Forwarding", func() {

	const (
		ovnNamespace          = "ovn-kubernetes"
		clientContainerPrefix = "client"
		extranet0             = "extranet0"
		extranet1             = "extranet1"
		ipv4                  = 0
		ipv6                  = 1
	)

	var (
		node          corev1.Node // nodeName holds the name of the Kubernetes node that we choose to forward traffic.
		nodeAddresses []netip.Addr
		extranets     = map[string][2]extranetInfo{ // 0: IPv4, 1: IPv6
			extranet0: {},
			extranet1: {},
		}
	)
	extranetContainerName := func(extranetName string) string {
		return fmt.Sprintf("%s-%s", clientContainerPrefix, extranetName)
	}

	f := wrappedTestFramework("drop-forwarding")

	ginkgo.BeforeEach(func() {
		ginkgo.By("Determining if both extranets exist")
		if !containerNetworksExist(extranet0, extranet1) {
			ginkgo.Skip("Skipping as requirement for extranet0, extranet1 is not met")
		}

		ginkgo.By("Choosing one node as the gateway")
		nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), f.ClientSet, 1)
		if err != nil {
			framework.Failf("Could not get nodes, err: %q", err)
		}
		node = nodes.Items[0]
		framework.Logf("Chose node %s", node.Name)

		ginkgo.By("Extracting IP addresses from node CIDR annotation")
		nodeAddresses, err = getNodeHostCIDRs(node)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		ginkgo.By("Creating external containers on extranets")
		for _, extranetName := range []string{extranet0, extranet1} {
			createClusterExternalContainer(extranetContainerName(extranetName), agnhostImage,
				[]string{"--network", "kind", "-P"},
				[]string{"netexec", "--http-port=80"})
			_, err := runCommand(containerRuntime, "network", "connect", extranetName, extranetContainerName(extranetName))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}

		ginkgo.By("Getting extranet subnet info")
		for extranetName, extranet := range extranets {
			framework.Logf("Getting subnet info for network %s", extranetName)
			ipv4Subnet, ipv6Subnet, err := getContainerNetworkSubnets(extranetName)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			extranet[ipv4].subnetInfo.Subnet = ipv4Subnet
			extranet[ipv6].subnetInfo.Subnet = ipv6Subnet
			framework.Logf("Found subnet info for network %s, IPv4: %s, IPv6: %s", extranetName, ipv4Subnet, ipv6Subnet)

			framework.Logf("Getting node %s IPs on subnets (IPv4: %s, IPv6: %s)", node.Name, ipv4Subnet, ipv6Subnet)
			ipv4Gateway, ipv6Gateway, err := findIPsOnSubnet(ipv4Subnet, ipv6Subnet, nodeAddresses)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			extranet[ipv4].subnetInfo.Gateway = ipv4Gateway
			extranet[ipv6].subnetInfo.Gateway = ipv6Gateway
			framework.Logf("Found node %s IPv4: %q, IPv6: %q", node.Name, ipv4Gateway, ipv6Gateway)

			framework.Logf("Getting container %s IPs on network %s", extranetContainerName(extranetName), extranetName)
			ipv4ContainerAddress, ipv6ContainerAddress, err := getContainerIPsOnNetwork(
				extranetContainerName(extranetName), extranetName)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			extranet[ipv4].containerIP = ipv4ContainerAddress
			extranet[ipv6].containerIP = ipv6ContainerAddress
			framework.Logf("Found container %s IPs on network %s, IPv4: %s, IPv6: %s",
				extranetContainerName(extranetName), extranetName, ipv4ContainerAddress, ipv6ContainerAddress)

			// Needed because of "Cannot assign to struct field in map".
			extranets[extranetName] = extranet
		}

		ginkgo.By("Making sure that extranet information is complete for at least one address family")
		var count int
		for addressFamily := range extranets[extranet0] {
			if extranets[extranet0][addressFamily].isComplete() && extranets[extranet1][addressFamily].isComplete() {
				count++
			}
		}
		gomega.Expect(count).NotTo(gomega.Equal(0))

		ginkgo.By("Logging extranet information")
		framework.Logf("Extranet information %+v", extranets)
	})

	ginkgo.AfterEach(func() {
		for extranetName := range extranets {
			deleteClusterExternalContainer(extranetContainerName(extranetName))
		}
	})

	ginkgo.Context("IP Forwarding tests", func() {
		var isDisableForwarding bool

		// checkForwarding is just a wrapper because we have to run the same test 2x with different routes.
		checkForwarding := func() {
			ginkgo.By("Checking if traffic is forwarded")
			if isDisableForwarding {
				framework.Logf("Forwarding should be blocked")
			} else {
				framework.Logf("Forwarding should not be blocked")
			}

			for _, af := range []int{ipv4, ipv6} {
				addressFamily := "IPv4"
				if af == ipv6 {
					addressFamily = "IPv6"
				}

				// Skip if address family information is not complete. In the BeforeEach, we validate that at
				// least one AF is complete.
				if !extranets[extranet0][af].isComplete() || !extranets[extranet1][af].isComplete() {
					framework.Logf("Not checking address family %s as extranet information is not complete", addressFamily)
					continue
				}
				framework.Logf("Checking address family %s", addressFamily)

				if isDisableForwarding {
					framework.Logf("Verifying if node forwards ICMP requests (should fail)")
					err := pingTargetFromContainer(extranetContainerName(extranet0), extranets[extranet1][af].containerIP)
					gomega.Expect(err).To(gomega.HaveOccurred())
					gomega.Expect(err.Error()).To(gomega.ContainSubstring("100% packet loss"))
					err = pingTargetFromContainer(extranetContainerName(extranet1), extranets[extranet0][af].containerIP)
					gomega.Expect(err).To(gomega.HaveOccurred())
					gomega.Expect(err.Error()).To(gomega.ContainSubstring("100% packet loss"))
				} else {
					ginkgo.By(fmt.Sprintf("Making sure that ICMP traffic is allowed for %s", addressFamily))
					err := pingTargetFromContainer(extranetContainerName(extranet0), extranets[extranet1][af].containerIP)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = pingTargetFromContainer(extranetContainerName(extranet1), extranets[extranet0][af].containerIP)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
				}
			}
		}

		ginkgo.BeforeEach(func() {
			isDisableForwarding = getDisableForwarding()
		})

		// In this scenario, we send a packet from container(extranet0) via the node's extranet0 interface to
		// container(extranet1). The traffic is returned via extranet1. We also test the reverse path with a ping
		// in the opposite direction.
		// container(extranet0) <--- [extranet0] ---> node <--- [extranet1] ---> container(extranet1).
		//
		// We add a route to container(extranet0) towards extranet1 via the node's interface on extranet0.
		// We add a route to container(extranet1) towards extranet0 via the node's interface on extranet1.
		ginkgo.When("Traffic is sent between containers via both extranets", func() {
			ginkgo.BeforeEach(func() {
				ginkgo.By("Adding routes to containers")
				for _, af := range []int{ipv4, ipv6} {
					// Skip if address family information is not complete. In earlier BeforeEach, we validate that at
					// least one AF is complete.
					if !extranets[extranet0][af].isComplete() || !extranets[extranet1][af].isComplete() {
						continue
					}
					err := addRouteToContainer(
						extranetContainerName(extranet0),
						extranets[extranet1][af].subnetInfo.Subnet,
						extranets[extranet0][af].subnetInfo.Gateway)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					err = addRouteToContainer(
						extranetContainerName(extranet1),
						extranets[extranet0][af].subnetInfo.Subnet,
						extranets[extranet1][af].subnetInfo.Gateway)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
				}
			})

			ginkgo.It("Forwarding should be allowed or blocked depending on drop-forwarding", func() {
				checkForwarding()
			})
		})

		// In this scenario, we send a packet from container(extranet0) via breth0 to container(extranet1). The traffic
		// is returned via extranet1. We also test the reverse path with a ping in the opposite direction.
		// container(extranet0) <--- [kind net] ---> node <--- [extranet1] ---> container(extranet1).
		//
		// We add a route to container(extranet0) towards extranet1 via breth0. Without
		// drop-forwarding, the k8s node will forward the packet to container(extranet1). The source address for this
		// traffic is container(extranet0)'s address on the kind network. We'd normally get asymmetric routing here as
		// container(extranet1) would reply via network kind. Therefore, we add a /32 route to containter(extranet0)'s
		// IP on network kind via the node's address on extranet1.
		ginkgo.When("Traffic is sent between containers via breth0 and extranet1 network", func() {
			ginkgo.BeforeEach(func() {
				kindnet := "kind"
				ginkgo.By("Inspecting kind network (breth0)")
				ipv4Subnet, ipv6Subnet, err := getContainerNetworkSubnets(kindnet)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				ginkgo.By("Getting IP addresses of extranet0 container on kind network")
				ipv4ContainerAddress, ipv6ContainerAddress, err := getContainerIPsOnNetwork(
					extranetContainerName(extranet0), kindnet)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// Overwrite extranet0 container IP addresses with the ones on network kind as those are our new target
				// for pings.
				extranet := extranets[extranet0]
				extranet[ipv4].containerIP = ipv4ContainerAddress
				extranet[ipv6].containerIP = ipv6ContainerAddress
				extranets[extranet0] = extranet

				ginkgo.By("Adding routes to containers")
				for _, af := range []int{ipv4, ipv6} {
					// Skip if address family information is not complete. In earlier BeforeEach, we validate that at
					// least one AF is complete.
					if !extranets[extranet0][af].isComplete() || !extranets[extranet1][af].isComplete() {
						continue
					}
					subnet := ipv4Subnet
					if af == ipv6 {
						subnet = ipv6Subnet
					}
					kindGateway, err := findIPOnSubnet(subnet, nodeAddresses)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					// extranet0 has both an interface on the kind subnet and an interface on the extranet0 interface.
					// On extranet0 container, add a route to subnet extranet1 via breth0 on the node so that it reaches
					// container extranet1 via breth0.
					err = addRouteToContainer(
						extranetContainerName(extranet0),
						extranets[extranet1][af].subnetInfo.Subnet,
						kindGateway)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					// extranet0 has both an interface on the kind subnet and an interface on the extranet0 interface.
					// extranet1 has both an interface on the kind subnet and an interface on the extranet1 interface.
					// Therefore, on extranet1 container, add a more specific /32 route to extranet0's IP on kind via
					// node's extranet1 interface. Otherwise, we'd see asymmetric routing.
					err = addRouteToContainer(
						extranetContainerName(extranet1),
						extranets[extranet0][af].containerIP, // IP on kind network.
						extranets[extranet1][af].subnetInfo.Gateway)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
				}
			})

			ginkgo.It("Forwarding should be allowed or blocked depending on drop-forwarding", func() {
				checkForwarding()
			})
		})
	})

	// The ExternalIP tests make sure that an ExternalIP service with ExternalIPs 1.2.3.4 and fc::1234 can be reached
	// from container(external0).
	ginkgo.Context("ExternalIP tests", func() {
		const (
			serviceName      = "externalipsvc"
			endpointHTTPPort = 80
			clusterHTTPPort  = 81
			endpointUDPPort  = 90
			clusterUDPPort   = 91
			externalIPv4     = "1.2.3.4"
			externalIPv6     = "fc::1234"
		)

		var (
			maxTries            int
			nodeHostnames       sets.String
			endpointsSelector   map[string]string
			endpoints           []*v1.Pod
			clientContainerName string
			nodeExtranetIP      string
			externalIPs         []string
		)

		ginkgo.BeforeEach(func() {
			endpointsSelector = map[string]string{"servicebackend": "true"}
			clientContainerName = extranetContainerName(extranet0)

			ginkgo.By("Adding routes to externalIPs to client container")
			for _, af := range []int{ipv4, ipv6} {
				// Skip if address family information is not complete. In earlier BeforeEach, we validate that at
				// least one AF is complete.
				if !extranets[extranet0][af].isComplete() {
					continue
				}
				externalIP := externalIPv4
				if af == ipv6 {
					externalIP = externalIPv6
				}
				externalIPs = append(externalIPs, externalIP)
				nodeExtranetIP = extranets[extranet0][af].subnetInfo.Gateway
				err := addRouteToContainer(clientContainerName, externalIP, nodeExtranetIP)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			}

			ginkgo.By("Selecting 3 nodes to spawn service pods on")
			nodeHostnames = sets.NewString()
			nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), f.ClientSet, 3)
			framework.ExpectNoError(err)
			if len(nodes.Items) < 3 {
				framework.Failf("Test requires >= 3 Ready nodes, but there are only %v nodes", len(nodes.Items))
			}

			ginkgo.By("Creating the endpoints pod, one for each of the 3 nodes")
			for _, node := range nodes.Items {
				// This creates an UDP / HTTP netexec listener which is able to receive the "hostname" command. We use
				// this to validate that each endpoint is received at least once.
				args := []string{
					"netexec",
					fmt.Sprintf("--http-port=%d", endpointHTTPPort),
					fmt.Sprintf("--udp-port=%d", endpointUDPPort),
				}
				pod, err := createPod(f, node.Name+"-ep", node.Name, f.Namespace.Name, []string{}, endpointsSelector,
					func(p *v1.Pod) {
						p.Spec.Containers[0].Args = args
					})
				framework.ExpectNoError(err)
				endpoints = append(endpoints, pod)
				nodeHostnames.Insert(pod.Name)

			}

			ginkgo.By("Setting the value of maxTries")
			// This is arbitrary and mutuated from k8s network e2e tests. We aim to hit all the endpoints at least
			// once.
			maxTries = len(endpoints)*len(endpoints) + 30
		})

		ginkgo.It("ExternalIP can be reached", func() {
			ginkgo.By(fmt.Sprintf("Creating the externalip service with externalIPs %v", externalIPs))
			externalIPsvcSpec := externalIPServiceSpecFrom(serviceName, endpointHTTPPort, endpointUDPPort,
				clusterHTTPPort, clusterUDPPort, endpointsSelector, externalIPs)
			_, err := f.ClientSet.CoreV1().Services(f.Namespace.Name).Create(context.Background(), externalIPsvcSpec,
				metav1.CreateOptions{})
			framework.ExpectNoError(err)

			ginkgo.By("Waiting for the endpoints to pop up")
			err = framework.WaitForServiceEndpointsNum(context.TODO(), f.ClientSet, f.Namespace.Name, serviceName,
				len(endpoints), time.Second, wait.ForeverTestTimeout)
			framework.ExpectNoError(err, "failed to validate endpoints for service %s in namespace: %s", serviceName,
				f.Namespace.Name)

			for _, protocol := range []string{"http", "udp"} {
				for _, externalAddress := range externalIPs {
					responses := sets.NewString()
					valid := false
					externalPort := int32(clusterHTTPPort)
					if protocol == "udp" {
						externalPort = int32(clusterUDPPort)
					}

					ginkgo.By(fmt.Sprintf("Hitting the external service on %s and reaching all %s endpoints",
						externalAddress, protocol))
					for i := 0; i < maxTries; i++ {
						responses.Insert(pokeEndpoint("", clientContainerName, protocol, externalAddress, externalPort,
							"hostname"))
						// Each endpoint returns its hostname. By doing this, we validate that each ep was reached at
						// least once.
						if responses.Equal(nodeHostnames) {
							framework.Logf("Validated external address %s after %d tries", externalAddress, i)
							valid = true
							break
						}
					}
					framework.ExpectEqual(valid, true, "Validation failed for external address %q, wanted: %q, got: %q",
						externalAddress, nodeHostnames, responses)
				}
			}
		})
	})
})

// pingTargetFromContainer runs a ping 10 times from container to target IP. Evaluate the return code of the ping
// command and return an error if the return code is != 0.
func pingTargetFromContainer(containerName, targetIP string) error {
	cmd := []string{containerRuntime, "exec", containerName, "ping", "-c10", "-W1", targetIP}
	framework.Logf("Running command: %v", cmd)
	_, err := runCommand(cmd...)
	if err != nil {
		framework.Logf("Ping to %s from %s failed", targetIP, containerName)
	} else {
		framework.Logf("Ping to %s from %s succeeded", targetIP, containerName)
	}
	return err
}

// addRouteToContainer adds a route to subnet [target] via gateway [via] to container [containerName].
func addRouteToContainer(containerName, target, via string) error {
	framework.Logf("Adding route to %s via %s to container %s", target, via, containerName)
	cmd := []string{
		containerRuntime, "exec", "--privileged", containerName, "ip", "route", "add", target, "via", via}
	_, err := runCommand(cmd...)
	return err
}

// getDisableForwarding searches the environment of daemonset/ovnkube-node for env var OVN_DISABLE_FORWARDING. If the
// environment variable is set to "true" (case insensitive), return true, otherwise return false.
func getDisableForwarding() bool {
	daemonSet := "daemonset/ovnkube-node"
	ovnDisableForwardingEnv := "OVN_DISABLE_FORWARDING"
	disableForwarding := getTemplateContainerEnv(ovnNamespace, daemonSet, getNodeContainerName(),
		ovnDisableForwardingEnv)
	framework.Logf("%s is set to %q", ovnDisableForwardingEnv, disableForwarding)
	return strings.ToLower(disableForwarding) == "true"
}

// containerNetworksExist returns true if all listed docker networks exist.
func containerNetworksExist(containerNetworkNames ...string) bool {
	if len(containerNetworkNames) == 0 {
		return false
	}

	networkList, err := runCommand(containerRuntime, "network", "ls", "--format", "{{.Name}}")
	if err != nil {
		framework.Failf("Could not list networks with docker, err: %q", err)
	}
	for _, containerNetworkName := range containerNetworkNames {
		if !strings.Contains(networkList, containerNetworkName) {
			return false
		}
	}
	return true
}

// getContainerNetworkSubnets retrieves the IPv4 and IPv6 subnets that belong to the specified docker network.
func getContainerNetworkSubnets(containerNetwork string) (string, string, error) {
	var sinfo []subnetInfo
	var ipv4Subnet, ipv6Subnet string

	out, err := runCommand(containerRuntime, "network", "inspect", containerNetwork, "--format",
		"{{json .IPAM.Config}}")
	if err != nil {
		return "", "", fmt.Errorf("could not inspect network %s with docker, err: %q", containerNetwork, err)
	}
	if err := json.Unmarshal([]byte(out), &sinfo); err != nil {
		return "", "", fmt.Errorf("could not unmarshal network subnet info, output %q, err: %q", out, err)
	}
	// We should at least get one subnet.
	if len(sinfo) < 1 || len(sinfo) > 2 {
		return "", "", fmt.Errorf("invalid subnet count for network %q, got %v", containerNetwork, sinfo)
	}
	for _, subnet := range sinfo {
		prefix, err := netip.ParsePrefix(subnet.Subnet)
		if err != nil {
			return "", "", fmt.Errorf("error parsing subnet, err: %q", err)
		}
		if prefix.Addr().Is4() {
			if ipv4Subnet != "" {
				return "", "", fmt.Errorf("more than one IPv4 subnets are not valid for network %q, got %v",
					containerNetwork, sinfo)
			}
			ipv4Subnet = subnet.Subnet
		} else {
			if ipv6Subnet != "" {
				return "", "", fmt.Errorf("more than one IPv6 subnets are not valid for network %q, got %v",
					containerNetwork, sinfo)
			}
			ipv6Subnet = subnet.Subnet
		}
	}
	return ipv4Subnet, ipv6Subnet, nil
}

// getNodeHostCIDRs extracts the node CIDRs from the node's k8s.ovn.org/host-cidrs annotation, as a slice of netip.Addr.
func getNodeHostCIDRs(node v1.Node) ([]netip.Addr, error) {
	var addresses []netip.Addr
	var addressStrings []string
	cidrAnnotation := "k8s.ovn.org/host-cidrs"

	addressAnnotation, ok := node.Annotations[cidrAnnotation]
	if !ok {
		return nil, fmt.Errorf("could not get node annotation %q for node %q", cidrAnnotation, node.Name)
	}
	if err := json.Unmarshal([]byte(addressAnnotation), &addressStrings); err != nil {
		return nil, fmt.Errorf("could not unmarshal annotation %q for node %q, err: %q", cidrAnnotation, node.Name, err)
	}

	for _, a := range addressStrings {
		prefix, err := netip.ParsePrefix(a)
		if err != nil {
			return nil, fmt.Errorf("could not parse prefix from address %q, err: %q", a, err)
		}
		addresses = append(addresses, prefix.Addr())
	}

	return addresses, nil
}

// findIPsOnSubnet finds the first IPv4 address and the first IPv6 address from the list of provided addresses that
// reside on the ipv4Subnet and ipv6Subnet, respectively.
func findIPsOnSubnet(ipv4Subnet, ipv6Subnet string, addresses []netip.Addr) (string, string, error) {
	var ipv4, ipv6 string
	var errIPv4, errIPv6 error

	if ipv4Subnet == "" && ipv6Subnet == "" {
		return "", "", fmt.Errorf("invalid input provided for findIPsOnSubnet, got empty ipv4 and ipv6 subnet")
	}
	if ipv4Subnet != "" {
		ipv4, errIPv4 = findIPOnSubnet(ipv4Subnet, addresses)
	}
	if ipv6Subnet != "" {
		ipv6, errIPv6 = findIPOnSubnet(ipv6Subnet, addresses)
	}
	if errIPv4 != nil && errIPv6 != nil {
		return "", "", fmt.Errorf("could not find a node address on network, "+
			"IPv4 subnet: %s, IPv6 subnet: %s, addresses: %+v", ipv4Subnet, ipv6Subnet, addresses)
	}
	return ipv4, ipv6, nil
}

// findIPOnSubnet finds the first IP address from the list of provided addresses that resided on the provided subnet.
func findIPOnSubnet(subnet string, addresses []netip.Addr) (string, error) {
	// First, parse the subnet into a CIDR.
	subnetPrefix, err := netip.ParsePrefix(subnet)
	if err != nil {
		return "", fmt.Errorf("could not parse prefix from subnet info %v, err: %q", subnet, err)
	}
	for _, address := range addresses {
		if subnetPrefix.Contains(address) {
			return address.String(), nil
		}
	}
	return "", fmt.Errorf("could not find a node address on subnet %q", subnet)
}

// getContainerIPsOnNetwork returns the container's IPv4 address and IPv6 address on the specified docker network.
func getContainerIPsOnNetwork(containerName, networkName string) (string, string, error) {
	var ipv4Address, ipv6Address string

	// Do this for IPv4 and IPv6 and store IPv4 result at index 0, IPv6 at index 1.
	for _, addressFamily := range []string{"IPAddress", "GlobalIPv6Address"} {
		framework.Logf("Inspecting extranet information for addressFamily %s", addressFamily)
		jsonExpression := fmt.Sprintf("{{ json .NetworkSettings.Networks.%s.%s }}", networkName, addressFamily)
		cmd := []string{containerRuntime, "inspect", containerName, "-f", jsonExpression}
		out, err := runCommand(cmd...)
		if err != nil {
			framework.Logf("failed to inspect IP address info for container %q, extranet %q, "+
				"addressFamily %q, command: %v, err: %q", containerName, networkName, addressFamily, cmd, err)
			continue
		}
		ip := strings.Trim(strings.TrimSuffix(out, "\n"), `"'`)
		if ip == "" {
			framework.Logf("failed to inspect IP address info for container %q, extranet %q, command: %v",
				containerName, networkName, cmd)
			continue
		}
		if addressFamily == "IPAddress" {
			ipv4Address = ip
		} else {
			ipv6Address = ip
		}
	}
	// We should get at least one subnet.
	if ipv4Address == "" && ipv6Address == "" {
		return "", "", fmt.Errorf("could not find any address for container %s on network %s", containerName, networkName)
	}
	return ipv4Address, ipv6Address, nil
}
