// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"net/netip"
	"strings"

	. "github.com/onsi/ginkgo/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/images"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider"
	infraapi "github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/api"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	utilnet "k8s.io/utils/net"
)

// buildRouteToHostSubnetViaExternalContainer returns ip route add commands to reach the host subnets via the provided gateway IPs
func buildRouteToHostSubnetViaExternalContainer(cs clientset.Interface, nodeName string, gwV4, gwV6, interfaceName string) ([]string, error) {
	cmdTemplateV4 := "ip -4 route replace %s via " + gwV4 + " dev " + interfaceName
	cmdTemplateV6 := "ip -6 route replace %s via " + gwV6 + " dev " + interfaceName
	cmds := []string{}
	hostSubnets, err := getHostSubnetsForNode(cs, nodeName)
	if err != nil {
		return nil, fmt.Errorf("failed to get host subnets for node %s: %w", nodeName, err)
	}
	for _, hostSubnet := range hostSubnets {
		if utilnet.IsIPv4CIDRString(hostSubnet) && gwV4 != "" {
			cmds = append(cmds, fmt.Sprintf(cmdTemplateV4, hostSubnet))
		} else if utilnet.IsIPv6CIDRString(hostSubnet) && gwV6 != "" {
			cmds = append(cmds, fmt.Sprintf(cmdTemplateV6, hostSubnet))
		}
	}
	return cmds, nil
}

// injectRouteViaExternalContainerIntoPod computes and applies host routes inside the given pod to reach
// the host subnets via the external container (VLAN interface), depending on
// the pod's ipRequestFromSubnet field.
func injectRouteViaExternalContainerIntoPod(f *framework.Framework, cs clientset.Interface, podConfig podConfiguration,
	podInterfaceName, externalContainerName string, vlanID int) error {

	nodeName, ok := podConfig.nodeSelector[nodeHostnameKey]
	if !ok {
		return fmt.Errorf("nodeSelector should contain %s key", nodeHostnameKey)
	}

	cmds := []string{}
	vlanIface := fmt.Sprintf("%s.%d", "eth0", vlanID)
	gwV4IPs, gwV6IPs, err := getExternalContainerInterfaceIPs(externalContainerName, vlanIface)
	if err != nil {
		return fmt.Errorf("failed to get external container interface IPs: %w", err)
	}
	if len(gwV4IPs) == 0 && len(gwV6IPs) == 0 {
		return fmt.Errorf("no IPs found on VLAN interface %s of external container %s", vlanIface, externalContainerName)
	}

	// Normalize IP addresses by removing CIDR notation if present
	gwV4IPs, err = normalizeIPAddresses(gwV4IPs)
	if err != nil {
		return fmt.Errorf("failed to normalize IPv4 addresses from external container interface: %w", err)
	}
	gwV6IPs, err = normalizeIPAddresses(gwV6IPs)
	if err != nil {
		return fmt.Errorf("failed to normalize IPv6 addresses from external container interface: %w", err)
	}

	// Take the first container IP as gateway.
	var gwIPV4, gwIPV6 string
	if len(gwV4IPs) > 0 {
		gwIPV4 = gwV4IPs[0]
	}

	for _, ip := range gwV6IPs {
		if addr, err := netip.ParseAddr(ip); err == nil && !addr.IsLinkLocalUnicast() {
			gwIPV6 = ip
			break
		}
	}

	cmds, err = buildRouteToHostSubnetViaExternalContainer(cs, nodeName, gwIPV4, gwIPV6, podInterfaceName)
	if err != nil {
		return fmt.Errorf("failed to build route to host subnet via external container: %w", err)
	}

	for _, cmd := range cmds {
		framework.Logf("Adding to pod %s/%s route to host subnet via external container %s: %s", podConfig.namespace, podConfig.name, externalContainerName, cmd)
		_, stderr, err := ExecShellInPodWithFullOutput(f, podConfig.namespace, podConfig.name, cmd)
		if err != nil || stderr != "" {
			return fmt.Errorf("failed to add route to external container (cmd=%s): stderr=%s, err=%w\n", cmd, stderr, err)
		}
	}

	return nil
}

// createExternalRouter creates an external container that acts as a router for localnet testing
func createExternalRouter(providerCtx infraapi.Context, cs clientset.Interface, f *framework.Framework, vlanID, ipOffset int) (string, error) {
	// Add external container that will act as external router for the localnet
	primaryProviderNetwork, err := infraprovider.Get().PrimaryNetwork()
	if err != nil {
		return "", fmt.Errorf("failed to get primary provider network: %w", err)
	}
	externalContainerName := f.Namespace.Name + "-external-router"

	routerSubnets := filterCIDRs(cs, externalNetworkSubnetV4, externalNetworkSubnetV6)
	routerIPs, err := generateIPsFromSubnets(routerSubnets, ipOffset)
	if err != nil {
		return "", fmt.Errorf("failed to generate IP for external router: %w", err)
	}
	if len(routerIPs) == 0 {
		return "", fmt.Errorf("no supported IP families found for the external router")
	}

	// - create a VLAN interface on top of eth0.
	// - assign the generated IP to the VLAN interface.
	// - enable IP forwarding.
	// - sleep to keep the container running.
	var commandBuilder strings.Builder
	commandBuilder.WriteString(fmt.Sprintf("ip link add link eth0 name eth0.%d type vlan id %d; ", vlanID, vlanID))
	for _, ip := range routerIPs {
		commandBuilder.WriteString(fmt.Sprintf("ip addr add %s dev eth0.%d; ", ip, vlanID))
	}
	commandBuilder.WriteString(fmt.Sprintf("ip link set eth0.%d up; ", vlanID))
	commandBuilder.WriteString("sysctl -w net.ipv4.ip_forward=1; ")
	commandBuilder.WriteString("sysctl -w net.ipv6.conf.all.forwarding=1; ")
	commandBuilder.WriteString("sleep infinity")

	externalContainerSpec := infraapi.ExternalContainer{
		Name:       externalContainerName,
		Image:      images.AgnHost(),
		Network:    primaryProviderNetwork,
		Entrypoint: "bash",
		CmdArgs:    []string{"-c", commandBuilder.String()},
	}

	_, err = providerCtx.CreateExternalContainer(externalContainerSpec)
	if err != nil {
		return "", fmt.Errorf("failed to create external router container: %w", err)
	}

	return externalContainerName, nil
}

// injectStaticRoutesViaExternalContainer configures the localnet pod to reach the host subnet and
// the hosts/OVN to reach the localnet subnet.
// We need to inject static routes in the following places:
//
//  1. on the localnet pod we need a route to reach the host subnet
//     via the VLAN interface of the external container;
//
//  2. in NBDB, if the cluster is in shared gateway mode, we need a route that tells
//     OVN to route traffic to the localnet subnet via the external container IPs;
//
//  3. in the host routing table, if the cluster is in local gateway mode, we need a route
//     that tells the host to route traffic to the localnet subnet via the external container IPs.
//     We need this also for host-networked pods to reach the localnet subnet regardless of the gateway mode.
func injectStaticRoutesViaExternalContainer(f *framework.Framework, cs clientset.Interface,
	clientPodConfig, serverPodConfig podConfiguration, clientInterface, serverInterface, externalContainerName string, vlanID int) error {
	if clientPodConfig.usesExternalRouter && len(clientPodConfig.attachments) > 0 {
		if err := injectRouteViaExternalContainerIntoPod(f, cs, clientPodConfig, clientInterface, externalContainerName, vlanID); err != nil {
			return fmt.Errorf("failed to add route to client pod %s/%s: %w", clientPodConfig.namespace, clientPodConfig.name, err)
		}
	}

	if serverPodConfig.usesExternalRouter && len(serverPodConfig.attachments) > 0 {
		if err := injectRouteViaExternalContainerIntoPod(f, cs, serverPodConfig, serverInterface, externalContainerName, vlanID); err != nil {
			return fmt.Errorf("failed to add route to server pod %s/%s: %w", serverPodConfig.namespace, serverPodConfig.name, err)
		}
	}

	if err := injectStaticRoutesIntoNodes(f, cs, externalContainerName); err != nil {
		return fmt.Errorf("failed to add static routes into nodes: %w", err)
	}
	return nil
}

// injectStaticRoutesIntoNodes adds routes for externalNetworkSubnetV4/V6
// via the external container IPs on the primary provider network.
// Route injection differs by gateway mode:
// | Local GW  | linux route on all nodes                                                 |
// | Shared GW | linux routes on all nodes; OVN routes on all nodes for the local GW router |
func injectStaticRoutesIntoNodes(f *framework.Framework, cs clientset.Interface, externalContainerName string) error {
	framework.Logf("Injecting Linux kernel routes for host-networked pods (and for OVN pods when in local gateway mode)")
	if err := injectRoutesWithCommandBuilder(f, cs, externalContainerName, hostRoutingTableCommandBuilder{}); err != nil {
		return err
	}

	if !IsGatewayModeLocal(cs) {
		framework.Logf("Shared gateway mode: injecting OVN routes for overlay pods")
		if err := injectRoutesWithCommandBuilder(f, cs, externalContainerName, ovnLogicalRouterCommandBuilder{}); err != nil {
			return err
		}
	}

	return nil
}

// routeCommand represents a route add/delete command for a specific gateway mode
type routeCommand struct {
	addCmd    []string
	deleteCmd []string
	logMsg    string
	target    string // what we're adding the route to (logical router name or node name)
}

// routeCommandBuilder defines the interface for building gateway-mode-specific route commands
type routeCommandBuilder interface {
	buildRouteCommand(nodeName, cidr, nextHop string) routeCommand
}

// ovnLogicalRouterCommandBuilder builds commands for OVN logical routes (shared gateway mode)
type ovnLogicalRouterCommandBuilder struct{}

func (b ovnLogicalRouterCommandBuilder) buildRouteCommand(nodeName, cidr, nextHop string) routeCommand {
	logicalRouterName := "GR_" + nodeName
	return routeCommand{
		addCmd:    []string{"ovn-nbctl", "--may-exist", "--", "lr-route-add", logicalRouterName, cidr, nextHop},
		deleteCmd: []string{"ovn-nbctl", "--if-exists", "--", "lr-route-del", logicalRouterName, cidr},
		logMsg:    fmt.Sprintf("OVN logical router route %s via %s to %s", cidr, nextHop, logicalRouterName),
		target:    logicalRouterName,
	}
}

// hostRoutingTableCommandBuilder builds commands for routes on the host
type hostRoutingTableCommandBuilder struct{}

func (b hostRoutingTableCommandBuilder) buildRouteCommand(nodeName, cidr, nextHop string) routeCommand {
	return routeCommand{
		addCmd:    []string{"ip", "route", "replace", cidr, "via", nextHop},
		deleteCmd: []string{"ip", "route", "del", cidr, "via", nextHop},
		logMsg:    fmt.Sprintf("host route %s via %s to node %s", cidr, nextHop, nodeName),
		target:    nodeName,
	}
}

// getOvnKubePodsForRouteInjection determines which pods to use for route injection based on the command builder type
// and cluster configuration.
func getOvnKubePodsForRouteInjection(cs clientset.Interface, cmdBuilder routeCommandBuilder) (*v1.PodList, error) {
	ovnKubernetesNamespace := deploymentconfig.Get().OVNKubernetesNamespace()

	var podList *v1.PodList
	var err error

	if _, isHostRoutingCommand := cmdBuilder.(hostRoutingTableCommandBuilder); isHostRoutingCommand {
		framework.Logf("Host routing command: selecting all ovnkube-node pods")
		podList, err = cs.CoreV1().Pods(ovnKubernetesNamespace).List(context.TODO(), metav1.ListOptions{LabelSelector: "name=ovnkube-node"})
	} else {
		framework.Logf("OVN command: selecting all OVN DB pods")
		podList, err = cs.CoreV1().Pods(ovnKubernetesNamespace).List(context.TODO(), metav1.ListOptions{LabelSelector: "ovn-db-pod=true"})
	}

	if err != nil {
		return nil, err
	}

	if len(podList.Items) == 0 {
		return nil, fmt.Errorf("no ovnkube pods found to execute route commands")
	}

	return podList, nil
}

// getTargetNodesForRouteInjection determines which nodes should be targeted for route injection
// based on the command builder type and cluster configuration.
func getTargetNodesForRouteInjection(cmdBuilder routeCommandBuilder, nodeName string) ([]string, error) {
	// For host routing commands, always target the current pod's node
	if _, isHostRoutingCommand := cmdBuilder.(hostRoutingTableCommandBuilder); isHostRoutingCommand {
		return []string{nodeName}, nil
	}

	// Each OVN DB pod targets its own gateway router.
	return []string{nodeName}, nil
}

// routeInfo represents a route destination and next-hop pair
type routeInfo struct {
	destination string
	nextHop     string
}

// buildRouteList creates a list of routes based on available IP families and gateways
func buildRouteList(clientSet clientset.Interface, v4gateway, v6gateway string) ([]routeInfo, error) {
	var routes []routeInfo

	if isIPv4Supported(clientSet) && v4gateway != "" {
		routes = append(routes, routeInfo{
			destination: externalNetworkSubnetV4,
			nextHop:     v4gateway,
		})
	}

	if isIPv6Supported(clientSet) && v6gateway != "" {
		routes = append(routes, routeInfo{
			destination: externalNetworkSubnetV6,
			nextHop:     v6gateway,
		})
	}

	if len(routes) == 0 {
		return nil, fmt.Errorf("no routes to inject (check IP families and external container addresses)")
	}

	return routes, nil
}

// podExecutionContext holds pod-specific information for route execution
type podExecutionContext struct {
	pod           v1.Pod
	containerName string
	targetNodes   []string
	cmdBuilder    routeCommandBuilder
}

func newPodExecutionContext(cmdBuilder routeCommandBuilder, pod v1.Pod) (*podExecutionContext, error) {
	targetNodes, err := getTargetNodesForRouteInjection(cmdBuilder, pod.Spec.NodeName)
	if err != nil {
		return nil, fmt.Errorf("failed to get target nodes for pod %s: %w", pod.Name, err)
	}

	// Pick the container that can actually run the command:
	//   - Host-routing commands ("ip route ...") need NET_ADMIN, getNodeContainerName()
	//     returns the right ovnkube-controller container name.
	//   - OVN logical-router commands ("ovn-nbctl ...") must run in the container that
	//     owns the NB DB unix socket, which is "nb-ovsdb".
	containerName := "nb-ovsdb"
	if _, isHostRoutingCommand := cmdBuilder.(hostRoutingTableCommandBuilder); isHostRoutingCommand {
		containerName = getNodeContainerName()
	}

	return &podExecutionContext{
		pod:           pod,
		containerName: containerName,
		targetNodes:   targetNodes,
		cmdBuilder:    cmdBuilder,
	}, nil
}

func (ctx *podExecutionContext) executeAllRoutes(f *framework.Framework, routes []routeInfo) error {
	for _, route := range routes {
		for _, targetNode := range ctx.targetNodes {
			routeCmd := ctx.cmdBuilder.buildRouteCommand(targetNode, route.destination, route.nextHop)

			if err := addRoute(f, ctx.pod.Namespace, ctx.pod.Name, ctx.containerName, routeCmd); err != nil {
				return err
			}

			scheduleRouteCleanup(f, ctx.pod.Namespace, ctx.pod.Name, ctx.containerName, routeCmd)
		}
	}
	return nil
}

func addRoute(f *framework.Framework, namespace, podName, containerName string, routeCmd routeCommand) error {
	stdout, stderr, err := ExecCommandInContainerWithFullOutput(f, namespace, podName, containerName, routeCmd.addCmd...)
	if err != nil || stderr != "" {
		return fmt.Errorf("failed to add %s (pod=%s, container=%s): stdout=%q, stderr=%q, cmd=%v: %w",
			routeCmd.logMsg, podName, containerName, stdout, stderr, routeCmd.addCmd, err)
	}
	framework.Logf("Successfully added %s", routeCmd.logMsg)
	return nil
}

func scheduleRouteCleanup(f *framework.Framework, namespace, podName, containerName string, routeCmd routeCommand) {
	DeferCleanup(func() {
		_, stderr, err := ExecCommandInContainerWithFullOutput(f, namespace, podName, containerName, routeCmd.deleteCmd...)
		if err != nil {
			framework.Logf("Warning: Failed to delete route from %s (cmd=%s): %v, stderr: %s",
				routeCmd.target, routeCmd.deleteCmd, err, stderr)
		}
	})
}

func injectRoutesWithCommandBuilder(f *framework.Framework, cs clientset.Interface, externalContainerName string, cmdBuilder routeCommandBuilder) error {
	primaryProviderNetwork, err := infraprovider.Get().PrimaryNetwork()
	if err != nil {
		return fmt.Errorf("failed to get primary network: %w", err)
	}

	v4gateway, v6gateway, err := getExternalContainerInterfaceIPsOnNetwork(externalContainerName, primaryProviderNetwork.Name())
	if err != nil {
		return fmt.Errorf("failed to get external container interface IPs on provider network: %w", err)
	}

	// Build list of routes to inject
	routes, err := buildRouteList(f.ClientSet, v4gateway, v6gateway)
	if err != nil {
		return err
	}

	// Get target pods for route injection
	ovnkubePods, err := getOvnKubePodsForRouteInjection(cs, cmdBuilder)
	if err != nil {
		return fmt.Errorf("failed to select target pods: %w", err)
	}

	// Execute route commands for each pod
	for _, pod := range ovnkubePods.Items {
		podCtx, err := newPodExecutionContext(cmdBuilder, pod)
		if err != nil {
			return err
		}

		if err := podCtx.executeAllRoutes(f, routes); err != nil {
			return err
		}
	}

	return nil
}
