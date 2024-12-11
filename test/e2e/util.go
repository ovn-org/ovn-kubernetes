package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	"k8s.io/kubernetes/test/e2e/framework/debug"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	testutils "k8s.io/kubernetes/test/utils"
	admissionapi "k8s.io/pod-security-admission/api"
	utilnet "k8s.io/utils/net"
)

const (
	ovnNamespace   = "ovn-kubernetes"
	ovnNodeSubnets = "k8s.ovn.org/node-subnets"
	// ovnNodeZoneNameAnnotation is the node annotation name to store the node zone name.
	ovnNodeZoneNameAnnotation = "k8s.ovn.org/zone-name"
	// ovnGatewayMTUSupport annotation determines if options:gateway_mtu shall be set for a node's gateway router
	ovnGatewayMTUSupport = "k8s.ovn.org/gateway-mtu-support"
)

var containerRuntime = "docker"
var singleNodePerZoneResult *bool

func init() {
	if cr, found := os.LookupEnv("CONTAINER_RUNTIME"); found {
		containerRuntime = cr
	}
}

type IpNeighbor struct {
	Dst    string `dst`
	Lladdr string `lladdr`
}

// PodAnnotation describes the assigned network details for a single pod network. (The
// actual annotation may include the equivalent of multiple PodAnnotations.)
type PodAnnotation struct {
	// IPs are the pod's assigned IP addresses/prefixes
	IPs []*net.IPNet
	// MAC is the pod's assigned MAC address
	MAC net.HardwareAddr
	// Gateways are the pod's gateway IP addresses; note that there may be
	// fewer Gateways than IPs.
	Gateways []net.IP
	// Routes are additional routes to add to the pod's network namespace
	Routes []PodRoute
	// Primary reveals if this network is the primary network of the pod or not
	Primary bool
}

// PodRoute describes any routes to be added to the pod's network namespace
type PodRoute struct {
	// Dest is the route destination
	Dest *net.IPNet
	// NextHop is the IP address of the next hop for traffic destined for Dest
	NextHop net.IP
}

// Internal struct used to marshal PodAnnotation to the pod annotation
type podAnnotation struct {
	IPs      []string   `json:"ip_addresses"`
	MAC      string     `json:"mac_address"`
	Gateways []string   `json:"gateway_ips,omitempty"`
	Routes   []podRoute `json:"routes,omitempty"`

	IP      string `json:"ip_address,omitempty"`
	Gateway string `json:"gateway_ip,omitempty"`
	Primary bool   `json:"primary"`
}

// Internal struct used to marshal PodRoute to the pod annotation
type podRoute struct {
	Dest    string `json:"dest"`
	NextHop string `json:"nextHop"`
}

type annotationNotSetError struct {
	msg string
}

// newAgnhostPod returns a pod that uses the agnhost image. The image's binary supports various subcommands
// that behave the same, no matter the underlying OS.
func newAgnhostPod(namespace, name string, command ...string) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:    name,
					Image:   agnhostImage,
					Command: command,
				},
			},
			RestartPolicy: v1.RestartPolicyNever,
		},
	}
}

// newAgnhostPod returns a pod that uses the agnhost image. The image's binary supports various subcommands
// that behave the same, no matter the underlying OS.
func newAgnhostPodOnNode(name, nodeName string, labels map[string]string, command ...string) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: labels,
		},
		Spec: v1.PodSpec{
			NodeName: nodeName,
			Containers: []v1.Container{
				{
					Name:    name,
					Image:   agnhostImage,
					Command: command,
				},
			},
			RestartPolicy: v1.RestartPolicyNever,
		},
	}
}

// IsIPv6Cluster returns true if the kubernetes default service is IPv6
func IsIPv6Cluster(c clientset.Interface) bool {
	// Get the ClusterIP of the kubernetes service created in the default namespace
	svc, err := c.CoreV1().Services(metav1.NamespaceDefault).Get(context.Background(), "kubernetes", metav1.GetOptions{})
	if err != nil {
		framework.Failf("Failed to get kubernetes service ClusterIP: %v", err)
	}
	if utilnet.IsIPv6String(svc.Spec.ClusterIP) {
		return true
	}
	return false
}

func (anse annotationNotSetError) Error() string {
	return anse.msg
}

// newAnnotationNotSetError returns an error for an annotation that is not set
func newAnnotationNotSetError(format string, args ...interface{}) error {
	return annotationNotSetError{msg: fmt.Sprintf(format, args...)}
}

// UnmarshalPodAnnotation returns the default network info from pod.Annotations
func unmarshalPodAnnotation(annotations map[string]string, networkName string) (*PodAnnotation, error) {
	ovnAnnotation, ok := annotations[podNetworkAnnotation]
	if !ok {
		return nil, newAnnotationNotSetError("could not find OVN pod annotation in %v", annotations)
	}

	podNetworks := make(map[string]podAnnotation)
	if err := json.Unmarshal([]byte(ovnAnnotation), &podNetworks); err != nil {
		return nil, fmt.Errorf("failed to unmarshal ovn pod annotation %q: %v",
			ovnAnnotation, err)
	}
	tempA := podNetworks[networkName]
	a := &tempA

	podAnnotation := &PodAnnotation{Primary: a.Primary}
	var err error
	podAnnotation.MAC, err = net.ParseMAC(a.MAC)
	if err != nil {
		return nil, fmt.Errorf("failed to parse pod MAC %q: %v", a.MAC, err)
	}

	if len(a.IPs) == 0 {
		if a.IP == "" {
			return nil, fmt.Errorf("bad annotation data (neither ip_address nor ip_addresses is set)")
		}
		a.IPs = append(a.IPs, a.IP)
	} else if a.IP != "" && a.IP != a.IPs[0] {
		return nil, fmt.Errorf("bad annotation data (ip_address and ip_addresses conflict)")
	}
	for _, ipstr := range a.IPs {
		ip, ipnet, err := net.ParseCIDR(ipstr)
		if err != nil {
			return nil, fmt.Errorf("failed to parse pod IP %q: %v", ipstr, err)
		}
		ipnet.IP = ip
		podAnnotation.IPs = append(podAnnotation.IPs, ipnet)
	}

	if len(a.Gateways) == 0 {
		if a.Gateway != "" {
			a.Gateways = append(a.Gateways, a.Gateway)
		}
	} else if a.Gateway != "" && a.Gateway != a.Gateways[0] {
		return nil, fmt.Errorf("bad annotation data (gateway_ip and gateway_ips conflict)")
	}
	for _, gwstr := range a.Gateways {
		gw := net.ParseIP(gwstr)
		if gw == nil {
			return nil, fmt.Errorf("failed to parse pod gateway %q", gwstr)
		}
		podAnnotation.Gateways = append(podAnnotation.Gateways, gw)
	}

	for _, r := range a.Routes {
		route := PodRoute{}
		_, route.Dest, err = net.ParseCIDR(r.Dest)
		if err != nil {
			return nil, fmt.Errorf("failed to parse pod route dest %q: %v", r.Dest, err)
		}
		if route.Dest.IP.IsUnspecified() {
			return nil, fmt.Errorf("bad podNetwork data: default route %v should be specified as gateway", route)
		}
		if r.NextHop != "" {
			route.NextHop = net.ParseIP(r.NextHop)
			if route.NextHop == nil {
				return nil, fmt.Errorf("failed to parse pod route next hop %q", r.NextHop)
			} else if utilnet.IsIPv6(route.NextHop) != utilnet.IsIPv6CIDR(route.Dest) {
				return nil, fmt.Errorf("pod route %s has next hop %s of different family", r.Dest, r.NextHop)
			}
		}
		podAnnotation.Routes = append(podAnnotation.Routes, route)
	}

	return podAnnotation, nil
}

func nodePortServiceSpecFrom(svcName string, ipFamily v1.IPFamilyPolicyType, httpPort, updPort, clusterHTTPPort, clusterUDPPort int, selector map[string]string, local v1.ServiceExternalTrafficPolicyType) *v1.Service {
	res := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: svcName,
		},
		Spec: v1.ServiceSpec{
			Type: v1.ServiceTypeNodePort,
			Ports: []v1.ServicePort{
				{Port: int32(clusterHTTPPort), Name: "http", Protocol: v1.ProtocolTCP, TargetPort: intstr.FromInt(httpPort)},
				{Port: int32(clusterUDPPort), Name: "udp", Protocol: v1.ProtocolUDP, TargetPort: intstr.FromInt(updPort)},
			},
			Selector:              selector,
			IPFamilyPolicy:        &ipFamily,
			ExternalTrafficPolicy: local,
		},
	}

	return res
}

func externalIPServiceSpecFrom(svcName string, httpPort, updPort, clusterHTTPPort, clusterUDPPort int, selector map[string]string, externalIps []string) *v1.Service {
	preferDual := v1.IPFamilyPolicyPreferDualStack

	res := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: svcName,
		},
		Spec: v1.ServiceSpec{
			Ports: []v1.ServicePort{
				{Port: int32(clusterHTTPPort), Name: "http", Protocol: v1.ProtocolTCP, TargetPort: intstr.FromInt(httpPort)},
				{Port: int32(clusterUDPPort), Name: "udp", Protocol: v1.ProtocolUDP, TargetPort: intstr.FromInt(updPort)},
			},
			Selector:       selector,
			IPFamilyPolicy: &preferDual,
			ExternalIPs:    externalIps,
		},
	}

	return res
}

// pokeEndpoint leverages a container running the netexec command to send a "request" to a target running
// netexec on the given target host / protocol / port.
// Returns the response based on the provided "request".
func pokeEndpoint(namespace, clientContainer, protocol, targetHost string, targetPort int32, request string) string {
	ipPort := net.JoinHostPort("localhost", "80")
	cmd := []string{containerRuntime, "exec", clientContainer}
	if len(namespace) != 0 {
		// command is to be run inside a pod, not containerRuntime
		cmd = []string{"exec", clientContainer, "--"}
	}

	// we leverage the dial command from netexec, that is already supporting multiple protocols
	curlCommand := strings.Split(fmt.Sprintf("curl -g -q -s http://%s/dial?request=%s&protocol=%s&host=%s&port=%d&tries=1",
		ipPort,
		request,
		protocol,
		targetHost,
		targetPort), " ")

	cmd = append(cmd, curlCommand...)
	var res string
	var err error
	if len(namespace) != 0 {
		res, err = e2ekubectl.RunKubectl(namespace, cmd...)
	} else {
		// command is to be run inside runtime container
		res, err = runCommand(cmd...)
	}
	framework.ExpectNoError(err, "failed to run command on external container")
	response, err := parseNetexecResponse(res)
	if err != nil {
		framework.Logf("FAILED Command was %s", curlCommand)
		framework.Logf("FAILED Response was %v", res)
		return ""
	}
	framework.ExpectNoError(err)

	return response
}

// wrapper logic around pokeEndpoint
// contact the ExternalIP service until each endpoint returns its hostname and return true, or false otherwise
func pokeExternalIpService(clientContainerName, protocol, externalAddress string, externalPort int32, maxTries int, nodesHostnames sets.String) bool {
	responses := sets.NewString()

	for i := 0; i < maxTries; i++ {
		epHostname := pokeEndpoint("", clientContainerName, protocol, externalAddress, externalPort, "hostname")
		responses.Insert(epHostname)

		// each endpoint returns its hostname. By doing this, we validate that each ep was reached at least once.
		if responses.Equal(nodesHostnames) {
			framework.Logf("Validated external address %s after %d tries", externalAddress, i)
			return true
		}
	}
	return false
}

// run a few iterations to make sure that the hwaddr is stable
// we will always run iterations + 1 in the loop to make sure that we have values
// to compare
func isNeighborEntryStable(clientContainer, targetHost string, iterations int) bool {
	cmd := []string{containerRuntime, "exec", clientContainer}
	var hwAddrOld string
	var hwAddrNew string
	// used for reporting only
	var hwAddrList []string

	// delete the neighbor entry, ping the IP once, and print new neighbor entries
	// make sure that we do not get Operation not permitted for neighbor entry deletion,
	// ignore everything else for the delete and the ping
	// RTNETLINK answers: Operation not permitted would indicate missing Cap NET_ADMIN
	script := fmt.Sprintf(
		"OUTPUT=$(ip neigh del %s dev eth0 2>&1); "+
			"if [[ \"$OUTPUT\" =~ \"Operation not permitted\" ]]; then "+
			"echo \"$OUTPUT\";"+
			"else "+
			"ping -c1 -W1 %s &>/dev/null; ip -j neigh; "+
			"fi",
		targetHost,
		targetHost,
	)
	command := []string{
		"/bin/bash",
		"-c",
		script,
	}
	cmd = append(cmd, command...)

	// run this for time of iterations + 1 to make sure that the entry is stable
	for i := 0; i <= iterations; i++ {
		// run the command
		output, err := runCommand(cmd...)
		if err != nil {
			framework.ExpectNoError(
				fmt.Errorf("FAILED Command was: %s\nFAILED Response was: %v\nERROR is: %s",
					command, output, err))
		}
		// unmarshal the output into an IpNeighbor object
		var neighbors []IpNeighbor
		err = json.Unmarshal([]byte(output), &neighbors)
		if err != nil {
			framework.ExpectNoError(
				fmt.Errorf("FAILED Command was: %s\nFAILED Response was: %v\nERROR is: %s",
					command, output, err))
		}

		// cycle through the results and find our Lladdr
		hwAddrNew = ""
		for _, n := range neighbors {
			if n.Dst == targetHost {
				hwAddrNew = n.Lladdr
				break
			}
		}
		// if we cannot find an Lladdr, report an issue
		if hwAddrNew == "" {
			framework.ExpectNoError(fmt.Errorf(
				"Cannot resolve neighbor entry for %s. Full array is %v",
				targetHost,
				output,
			))
		}

		// make sure that we did not flap since the last iteration
		if hwAddrOld != "" {
			if hwAddrOld != hwAddrNew {
				framework.Logf("The hwAddr for IP %s flapped from %s to %s on iteration %d (%s)",
					targetHost,
					hwAddrOld,
					hwAddrNew,
					i,
					strings.Join(hwAddrList, ","))
				return false
			}
		}
		hwAddrOld = hwAddrNew
		// used for reporting only
		hwAddrList = append(hwAddrList, hwAddrNew)
	}

	framework.Logf("hwAddr is stable after %d iterations: %s", iterations, strings.Join(hwAddrList, ","))

	return true
}

// curlInContainer leverages a container running the netexec command to send a request to a target running
// netexec on the given target host / protocol / port.
// Returns a pair of either result, nil or "", error in case of an error.
func curlInContainer(clientContainer, targetHost string, targetPort int32, endPoint string, maxTime int) (string, error) {
	cmd := []string{containerRuntime, "exec", clientContainer}
	if utilnet.IsIPv6String(targetHost) {
		targetHost = fmt.Sprintf("[%s]", targetHost)
	}

	// we leverage the dial command from netexec, that is already supporting multiple protocols
	curlCommand := strings.Split(fmt.Sprintf("curl --max-time %d http://%s:%d/%s",
		maxTime,
		targetHost,
		targetPort,
		endPoint), " ")

	cmd = append(cmd, curlCommand...)
	return runCommand(cmd...)
}

// parseNetexecResponse parses a json string of type '{"responses":"...", "errors":""}'.
// it returns "", error if the errors value is not empty, or the responses otherwise.
func parseNetexecResponse(response string) (string, error) {
	res := struct {
		Responses []string `json:"responses"`
		Errors    []string `json:"errors"`
	}{}
	if err := json.Unmarshal([]byte(response), &res); err != nil {
		return "", fmt.Errorf("failed to unmarshal curl response %s", response)
	}
	if len(res.Errors) > 0 {
		return "", fmt.Errorf("curl response %s contains errors", response)
	}
	if len(res.Responses) == 0 {
		return "", fmt.Errorf("curl response %s has no values", response)
	}
	return res.Responses[0], nil
}

func nodePortsFromService(service *v1.Service) (int32, int32) {
	var resTCP, resUDP int32
	for _, p := range service.Spec.Ports {
		if p.Protocol == v1.ProtocolTCP {
			resTCP = p.NodePort
		}
		if p.Protocol == v1.ProtocolUDP {
			resUDP = p.NodePort
		}
	}
	return resTCP, resUDP
}

// addressIsIP tells wether the given address is an
// address or a hostname
func addressIsIP(address v1.NodeAddress) bool {
	addr := net.ParseIP(address.Address)
	if addr == nil {
		return false
	}
	return true
}

// addressIsIPv4 tells whether the given address is an
// IPv4 address.
func addressIsIPv4(address v1.NodeAddress) bool {
	addr := net.ParseIP(address.Address)
	if addr == nil {
		return false
	}
	return utilnet.IsIPv4String(addr.String())
}

// addressIsIPv6 tells whether the given address is an
// IPv6 address.
func addressIsIPv6(address v1.NodeAddress) bool {
	addr := net.ParseIP(address.Address)
	if addr == nil {
		return false
	}
	return utilnet.IsIPv6String(addr.String())
}

// Returns pod's ipv4 and ipv6 addresses IN ORDER
func getPodAddresses(pod *v1.Pod) (string, string) {
	var ipv4Res, ipv6Res string
	for _, a := range pod.Status.PodIPs {
		if utilnet.IsIPv4String(a.IP) {
			ipv4Res = a.IP
		}
		if utilnet.IsIPv6String(a.IP) {
			ipv6Res = a.IP
		}
	}
	return ipv4Res, ipv6Res
}

// Returns nodes's ipv4 and ipv6 addresses IN ORDER
func getNodeAddresses(node *v1.Node) (string, string) {
	var ipv4Res, ipv6Res string
	for _, a := range node.Status.Addresses {
		if utilnet.IsIPv4String(a.Address) {
			ipv4Res = a.Address
		}
		if utilnet.IsIPv6String(a.Address) {
			ipv6Res = a.Address
		}
	}
	return ipv4Res, ipv6Res
}

func getNodeStatus(node string) string {
	status, err := e2ekubectl.RunKubectl("default", "get", "node", "-o", "jsonpath={.status.conditions[?(@.type==\"Ready\")].status}", node)
	if err != nil {
		framework.Failf("Unable to retrieve the status for node: %s %v", node, err)
	}
	return status
}

// Returns the container's ipv4 and ipv6 addresses IN ORDER
// related to the given network.
func getContainerAddressesForNetwork(container, network string) (string, string) {
	ipv4Format := fmt.Sprintf("{{.NetworkSettings.Networks.%s.IPAddress}}", network)
	ipv6Format := fmt.Sprintf("{{.NetworkSettings.Networks.%s.GlobalIPv6Address}}", network)

	ipv4, err := runCommand(containerRuntime, "inspect", "-f", ipv4Format, container)
	if err != nil {
		framework.Failf("failed to inspect external test container for its IPv4: %v", err)
	}
	ipv6, err := runCommand(containerRuntime, "inspect", "-f", ipv6Format, container)
	if err != nil {
		framework.Failf("failed to inspect external test container for its IPv4: %v", err)
	}
	return strings.TrimSuffix(ipv4, "\n"), strings.TrimSuffix(ipv6, "\n")
}

// Returns the container's MAC addresses
// related to the given network.
func getMACAddressesForNetwork(container, network string) string {
	mac := fmt.Sprintf("{{.NetworkSettings.Networks.%s.MacAddress}}", network)

	macAddr, err := runCommand(containerRuntime, "inspect", "-f", mac, container)
	if err != nil {
		framework.Failf("failed to inspect external test container for its MAC: %v", err)
	}
	return strings.TrimSuffix(macAddr, "\n")
}

// waitClusterHealthy ensures we have a given number of ovn-k worker and master nodes,
// as well as all nodes are healthy
func waitClusterHealthy(f *framework.Framework, numControlPlanePods int, controlPlanePodName string) error {
	return wait.PollImmediate(2*time.Second, 120*time.Second, func() (bool, error) {
		nodes, err := f.ClientSet.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
		if err != nil {
			return false, fmt.Errorf("failed to list nodes: %w", err)
		}

		numNodes := len(nodes.Items)
		if numNodes == 0 {
			return false, fmt.Errorf("list returned no Node objects, something is wrong")
		}

		// Check that every node is schedulable
		afterNodes, err := e2enode.GetReadySchedulableNodes(context.TODO(), f.ClientSet)
		if err != nil {
			return false, fmt.Errorf("failed to look for healthy nodes: %w", err)
		}
		if len(afterNodes.Items) != numNodes {
			framework.Logf("Not enough schedulable nodes, have %d want %d", len(afterNodes.Items), numNodes)
			return false, nil
		}

		podClient := f.ClientSet.CoreV1().Pods(ovnNamespace)
		// Ensure all nodes are running and healthy
		podList, err := podClient.List(context.Background(), metav1.ListOptions{
			LabelSelector: "app=ovnkube-node",
		})
		if err != nil {
			return false, fmt.Errorf("failed to list ovn-kube node pods: %w", err)
		}
		if len(podList.Items) != numNodes {
			framework.Logf("Not enough running ovnkube-node pods, want %d, have %d", numNodes, len(podList.Items))
			return false, nil
		}

		for _, pod := range podList.Items {
			if ready, err := testutils.PodRunningReady(&pod); !ready {
				framework.Logf("%v", err)
				return false, nil
			}
		}

		podList, err = podClient.List(context.Background(), metav1.ListOptions{
			LabelSelector: "name=" + controlPlanePodName,
		})
		if err != nil {
			return false, fmt.Errorf("failed to list ovn-kube master pods: %w", err)
		}
		if len(podList.Items) != numControlPlanePods {
			framework.Logf("Not enough running %s pods, want %d, have %d", controlPlanePodName, numControlPlanePods, len(podList.Items))
			return false, nil
		}

		for _, pod := range podList.Items {
			if ready, err := testutils.PodRunningReady(&pod); !ready {
				framework.Logf("%v", err)
				return false, nil
			}
		}

		return true, nil
	})
}

// waitForRollout waits for the daemon set in a given namespace to be
// successfully rolled out following an update.
//
// If allowedNotReadyNodes is -1, this method returns immediately without waiting.
func waitForRollout(c clientset.Interface, ns string, resource string, allowedNotReadyNodes int32, timeout time.Duration) error {
	if allowedNotReadyNodes == -1 {
		return nil
	}

	resourceAtoms := strings.Split(resource, "/")
	if len(resourceAtoms) != 2 {
		return fmt.Errorf("invalid resource format, expected <type>/<name>, got %s", resource)
	}
	resourceType := resourceAtoms[0]
	resourceName := resourceAtoms[1]

	start := time.Now()
	framework.Logf("Waiting up to %v for daemonset %s in namespace %s to update",
		timeout, resource, ns)

	return wait.Poll(framework.Poll, timeout, func() (bool, error) {
		var generation, observedGeneration int64
		var updated, desired, available int32
		switch resourceType {
		case "daemonset", "daemonsets", "ds":
			ds, err := c.AppsV1().DaemonSets(ns).Get(context.TODO(), resourceName, metav1.GetOptions{})
			if err != nil {
				framework.Logf("Error getting resource %s in namespace: %s: %v", resource, ns, err)
				return false, err
			}
			generation = ds.Generation
			observedGeneration = ds.Status.ObservedGeneration
			updated = ds.Status.UpdatedNumberScheduled
			desired = ds.Status.DesiredNumberScheduled
			available = ds.Status.NumberAvailable

		case "deployment", "deployments", "deploy":
			dp, err := c.AppsV1().Deployments(ns).Get(context.TODO(), resourceName, metav1.GetOptions{})
			if err != nil {
				framework.Logf("Error getting resource %s in namespace: %s: %v", resource, ns, err)
				return false, err
			}
			generation = dp.Generation
			observedGeneration = dp.Status.ObservedGeneration
			updated = dp.Status.UpdatedReplicas
			desired = dp.Status.Replicas
			available = dp.Status.AvailableReplicas

		default:
			return false, fmt.Errorf("unsupported resource type %s", resourceType)
		}

		if generation <= observedGeneration {
			if updated < desired {
				framework.Logf("Waiting for %s rollout to finish: %d out of %d new pods have been updated (%d seconds elapsed)", resource,
					updated, desired, int(time.Since(start).Seconds()))
				return false, nil
			}
			if available < desired {
				framework.Logf("Waiting for %s rollout to finish: %d of %d updated pods are available (%d seconds elapsed)", resource,
					available, desired, int(time.Since(start).Seconds()))
				return false, nil
			}
			framework.Logf("resource %q successfully rolled out", resource)
			return true, nil
		}

		framework.Logf("Waiting for %s spec update to be observed...", resource)
		return false, nil
	})
}

func pokePod(fr *framework.Framework, srcPodName string, dstPodIP string) error {
	targetIP := dstPodIP
	if utilnet.IsIPv6String(dstPodIP) {
		targetIP = fmt.Sprintf("[%s]", dstPodIP)
	}
	stdout, stderr, err := e2epod.ExecShellInPodWithFullOutput(
		context.TODO(),
		fr,
		srcPodName,
		fmt.Sprintf("curl --output /dev/stdout -m 1 -I %s:8000 | head -n1", targetIP))
	if err == nil && stdout == "HTTP/1.1 200 OK" {
		return nil
	}
	framework.Logf("HTTP request failed; stdout: %s, err: %v", stdout+stderr, err)
	return fmt.Errorf("http request failed; stdout: %s, err: %v", stdout+stderr, err)
}

// pokeAllPodIPs will either poke the single dstPod's PodIP or all IPs in the pod's PodIPs list. The returned error
// will be an aggregate of the errors encountered poking all destination IPs.
func pokeAllPodIPs(fr *framework.Framework, srcPodName string, dstPod *v1.Pod) error {
	var errors []error
	if len(dstPod.Status.PodIPs) > 0 {
		for _, podIP := range dstPod.Status.PodIPs {
			if err := pokePod(fr, srcPodName, podIP.IP); err != nil {
				errors = append(errors, err)
			}
		}
		return utilerrors.NewAggregate(errors)
	}
	return pokePod(fr, srcPodName, dstPod.Status.PodIP)
}

func pokeExternalHostFromPod(fr *framework.Framework, namespace string, srcPodName, dstIp string, dstPort int) error {
	if utilnet.IsIPv6String(dstIp) {
		dstIp = fmt.Sprintf("[%s]", dstIp)
	}
	stdout, stderr, err := ExecShellInPodWithFullOutput(
		fr,
		namespace,
		srcPodName,
		fmt.Sprintf("curl --output /dev/stdout -m 1 -I %s:%d | head -n1", dstIp, dstPort))
	if err == nil && stdout == "HTTP/1.1 200 OK" {
		return nil
	}
	return fmt.Errorf("http request failed; stdout: %s, err: %v", stdout+stderr, err)
}

// ExecShellInPodWithFullOutput is a shameless copy/paste from the framework methods so that we can specify the pod namespace.
func ExecShellInPodWithFullOutput(f *framework.Framework, namespace, podName string, cmd string) (string, string, error) {
	return execCommandInPodWithFullOutput(f, namespace, podName, "/bin/sh", "-c", cmd)
}

// execCommandInPodWithFullOutput is a shameless copy/paste from the framework methods so that we can specify the pod namespace.
func execCommandInPodWithFullOutput(f *framework.Framework, namespace, podName string, cmd ...string) (string, string, error) {
	pod, err := f.ClientSet.CoreV1().Pods(namespace).Get(context.TODO(), podName, metav1.GetOptions{})
	framework.ExpectNoError(err, "failed to get pod %v", podName)
	gomega.Expect(pod.Spec.Containers).NotTo(gomega.BeEmpty())
	return ExecCommandInContainerWithFullOutput(f, namespace, podName, pod.Spec.Containers[0].Name, cmd...)
}

// ExecCommandInContainerWithFullOutput is a shameless copy/paste from the framework methods so that we can specify the pod namespace.
func ExecCommandInContainerWithFullOutput(f *framework.Framework, namespace, podName, containerName string, cmd ...string) (string, string, error) {
	options := e2epod.ExecOptions{
		Command:            cmd,
		Namespace:          namespace,
		PodName:            podName,
		ContainerName:      containerName,
		Stdin:              nil,
		CaptureStdout:      true,
		CaptureStderr:      true,
		PreserveWhitespace: false,
	}
	return e2epod.ExecWithOptions(f, options)
}

func assertACLLogs(targetNodeName string, policyNameRegex string, expectedACLVerdict string, expectedACLSeverity string) (bool, error) {
	framework.Logf("collecting the ovn-controller logs for node: %s", targetNodeName)
	targetNodeLog, err := runCommand([]string{containerRuntime, "exec", targetNodeName, "grep", "acl_log", ovnControllerLogPath}...)
	if err != nil {
		return false, fmt.Errorf("error accessing logs in node %s: %v", targetNodeName, err)
	}

	framework.Logf("Ensuring the audit log contains: 'name=\"%s\"', 'verdict=%s' AND 'severity=%s'", policyNameRegex, expectedACLVerdict, expectedACLSeverity)
	for _, logLine := range strings.Split(targetNodeLog, "\n") {
		matched, err := regexp.MatchString(fmt.Sprintf("name=\"%s\"", policyNameRegex), logLine)
		if err != nil {
			return false, err
		}
		if matched &&
			strings.Contains(logLine, fmt.Sprintf("verdict=%s", expectedACLVerdict)) &&
			strings.Contains(logLine, fmt.Sprintf("severity=%s", expectedACLSeverity)) {
			return true, nil
		}
	}
	return false, nil
}

// patchServiceStringValue patches service serviceName in namespace serviceNamespace with provided string value.
func patchServiceStringValue(c kubernetes.Interface, serviceName, serviceNamespace, jsonPath, value string) error {
	patch := []struct {
		Op    string `json:"op"`
		Path  string `json:"path"`
		Value string `json:"value"`
	}{{
		Op:    "replace",
		Path:  jsonPath,
		Value: value,
	}}
	patchBytes, _ := json.Marshal(patch)

	return patchService(c, serviceName, serviceNamespace, jsonPath, patchBytes)
}

// patchServiceBoolValue patches service serviceName in namespace serviceNamespace with provided bool value.
func patchServiceBoolValue(c kubernetes.Interface, serviceName, serviceNamespace, jsonPath string, value bool) error {
	patch := []struct {
		Op    string `json:"op"`
		Path  string `json:"path"`
		Value bool   `json:"value"`
	}{{
		Op:    "replace",
		Path:  jsonPath,
		Value: value,
	}}
	patchBytes, _ := json.Marshal(patch)

	return patchService(c, serviceName, serviceNamespace, jsonPath, patchBytes)
}

// patchService patches service serviceName in namespace serviceNamespace.
func patchService(c kubernetes.Interface, serviceName, serviceNamespace, jsonPath string, patchBytes []byte) error {
	_, err := c.CoreV1().Services(serviceNamespace).Patch(
		context.TODO(),
		serviceName,
		types.JSONPatchType,
		patchBytes,
		metav1.PatchOptions{})
	if err != nil {
		return err
	}

	return nil
}

// pokeIPTableRules returns the number of iptables (both ipv6 and ipv4) rules that match the provided pattern
func pokeIPTableRules(clientContainer, pattern string) int {
	cmd := []string{containerRuntime, "exec", clientContainer}

	ipv4Cmd := append(cmd, strings.Split("iptables-save -c", " ")...)
	ipt4Rules, err := runCommand(ipv4Cmd...)
	framework.ExpectNoError(err, "failed to get iptables rules from node %s", clientContainer)

	ipv6Cmd := append(cmd, strings.Split("ip6tables-save -c", " ")...)
	ipt6Rules, err := runCommand(ipv6Cmd...)
	framework.ExpectNoError(err, "failed to get ip6tables rules from node %s", clientContainer)

	iptRules := ipt4Rules + ipt6Rules
	framework.Logf("DEBUG: Dumping IPTRules %v", iptRules)
	numOfMatchRules := 0
	for _, iptRule := range strings.Split(iptRules, "\n") {
		match := strings.Contains(iptRule, pattern)
		if match {
			framework.Logf("DEBUG: Matched rule %s for pattern %s", iptRule, pattern)
			numOfMatchRules++
		}
	}
	return numOfMatchRules
}

// countNFTablesElements returns the number of nftables elements in the indicated set
// of the "ovn-kubernetes" table.
func countNFTablesElements(clientContainer, name string) int {
	cmd := []string{containerRuntime, "exec", clientContainer}

	nftCmd := append(cmd, "nft", "-j", "list", "set", "inet", "ovn-kubernetes", name)
	nftElements, err := runCommand(nftCmd...)
	framework.ExpectNoError(err, "failed to get nftables elements from node %s", clientContainer)

	framework.Logf("DEBUG: Dumping NFTElements %v", nftElements)
	// The output will look like
	//
	// {
	//   "nftables": [
	//     {
	//       "metainfo": {
	//         ...
	//       }
	//     },
	//     {
	//       "set": {
	//         ...
	//         "elem": [
	//           ...
	//         ]
	//       }
	//     }
	//   ]
	// }
	//
	// (Where the "elem" element will be omitted if the set is empty.)
	// We just parse this optimistically and catch the panic if it fails.
	count := -1
	defer func() {
		if recover() != nil {
			framework.Logf("JSON parsing error!")
		}
	}()

	jsonResult := map[string][]map[string]map[string]any{}
	json.Unmarshal([]byte(nftElements), &jsonResult)
	elem := jsonResult["nftables"][1]["set"]["elem"]
	if elem == nil {
		return 0
	}
	elemArray := elem.([]any)
	count = len(elemArray)
	return count
}

// isDualStackCluster returns 'true' if at least one of the nodes has more than one node subnet.
func isDualStackCluster(nodes *v1.NodeList) bool {
	for _, node := range nodes.Items {
		annotation, ok := node.Annotations[ovnNodeSubnets]
		if !ok {
			continue
		}

		subnets := make(map[string][]string)
		if err := json.Unmarshal([]byte(annotation), &subnets); err == nil {
			if len(subnets["default"]) > 1 {
				return true
			}
		}
	}
	return false
}

// used to inject OVN specific test actions
func wrappedTestFramework(basename string) *framework.Framework {
	f := newPrivelegedTestFramework(basename)
	// inject dumping dbs on failure
	ginkgo.JustAfterEach(func() {
		if !ginkgo.CurrentSpecReport().Failed() {
			return
		}

		logLocation := "/var/log"
		dbLocation := "/var/lib/openvswitch"
		// Potential database locations
		ovsdbLocations := []string{"/etc/origin/openvswitch", "/etc/openvswitch"}
		dbs := []string{"ovnnb_db.db", "ovnsb_db.db"}
		ovsdb := "conf.db"

		testName := strings.Replace(ginkgo.CurrentSpecReport().LeafNodeText, " ", "_", -1)
		logDir := fmt.Sprintf("%s/e2e-dbs/%s-%s", logLocation, testName, f.UniqueName)

		var args []string

		// grab all OVS and OVN dbs
		nodes, err := f.ClientSet.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})
		framework.ExpectNoError(err)
		for _, node := range nodes.Items {
			// ensure e2e-dbs directory with test case exists
			args = []string{containerRuntime, "exec", node.Name, "mkdir", "-p", logDir}
			_, err = runCommand(args...)
			framework.ExpectNoError(err)

			// Loop through potential OVSDB db locations
			for _, ovsdbLocation := range ovsdbLocations {
				args = []string{containerRuntime, "exec", node.Name, "stat", fmt.Sprintf("%s/%s", ovsdbLocation, ovsdb)}
				_, err = runCommand(args...)
				if err == nil {
					// node name is the same in kapi and docker
					args = []string{containerRuntime, "exec", node.Name, "cp", "-f", fmt.Sprintf("%s/%s", ovsdbLocation, ovsdb),
						fmt.Sprintf("%s/%s", logDir, fmt.Sprintf("%s-%s", node.Name, ovsdb))}
					_, err = runCommand(args...)
					framework.ExpectNoError(err)
					break // Stop the loop: the file is found and copied successfully
				}
			}

			// IC will have dbs on every node, but legacy mode wont, check if they exist
			args = []string{containerRuntime, "exec", node.Name, "stat", fmt.Sprintf("%s/%s", dbLocation, dbs[0])}
			_, err = runCommand(args...)
			if err == nil {
				for _, db := range dbs {
					args = []string{containerRuntime, "exec", node.Name, "cp", "-f", fmt.Sprintf("%s/%s", dbLocation, db),
						fmt.Sprintf("%s/%s", logDir, db)}
					_, err = runCommand(args...)
					framework.ExpectNoError(err)
				}
			}
		}
	})

	return f
}

func newPrivelegedTestFramework(basename string) *framework.Framework {
	f := framework.NewDefaultFramework(basename)
	f.NamespacePodSecurityEnforceLevel = admissionapi.LevelPrivileged
	f.DumpAllNamespaceInfo = func(ctx context.Context, f *framework.Framework, namespace string) {
		debug.DumpAllNamespaceInfo(context.TODO(), f.ClientSet, namespace)
	}
	return f
}

// countACLLogs connects to <targetNodeName> (ovn-control-plane, ovn-worker or ovn-worker2 in kind environments) via the docker exec
// command and it greps for the string "acl_log" inside the OVN controller logs. It then checks if the line contains name=<policyNameRegex>
// and if it does, it increases the counter if both the verdict and the severity for this line match what's expected.
func countACLLogs(targetNodeName string, policyNameRegex string, expectedACLVerdict string, expectedACLSeverity string) (int, error) {
	count := 0

	framework.Logf("collecting the ovn-controller logs for node: %s", targetNodeName)
	targetNodeLog, err := runCommand([]string{containerRuntime, "exec", targetNodeName, "cat", ovnControllerLogPath}...)
	if err != nil {
		return 0, fmt.Errorf("error accessing logs in node %s: %v", targetNodeName, err)
	}

	stringToMatch := fmt.Sprintf(
		".*acl_log.*name=\"%s\".*verdict=%s.*severity=%s.*",
		policyNameRegex,
		expectedACLVerdict,
		expectedACLSeverity)

	for _, logLine := range strings.Split(targetNodeLog, "\n") {
		matched, err := regexp.MatchString(stringToMatch, logLine)
		if err != nil {
			return 0, err
		}
		if matched {
			count++
		}
	}

	framework.Logf("The audit log contains %d occurrences of: '%s'", count, stringToMatch)
	return count, nil
}

// getTemplateContainerEnv gets the value of an environment variable in a container template
func getTemplateContainerEnv(namespace, resource, container, key string) string {
	args := []string{"get", resource,
		"-o=jsonpath='{.spec.template.spec.containers[?(@.name==\"" + container + "\")].env[?(@.name==\"" + key + "\")].value}'"}
	value := e2ekubectl.RunKubectlOrDie(ovnNamespace, args...)
	return strings.Trim(value, "'")
}

// setUnsetTemplateContainerEnv sets and unsets environment variables in a container
// template and waits for the rollout
func setUnsetTemplateContainerEnv(c kubernetes.Interface, namespace, resource, container string, set map[string]string, unset ...string) {
	args := []string{"set", "env", resource, "-c", container}
	env := make([]string, 0, len(set)+len(unset))
	for k, v := range set {
		env = append(env, fmt.Sprintf("%s=%s", k, v))
	}
	for _, k := range unset {
		env = append(env, fmt.Sprintf("%s-", k))
	}
	framework.Logf("Setting environment in %s container %s of namespace %s to %v", resource, container, namespace, env)
	e2ekubectl.RunKubectlOrDie(namespace, append(args, env...)...)

	// Make sure the change has rolled out
	// TODO (Change this to use the exported upstream function)
	err := waitForRollout(c, namespace, resource, 0, rolloutTimeout)
	framework.ExpectNoError(err)
}

// allowOrDropNodeInputTrafficOnPort ensures or deletes a drop iptables
// input rule for the specified node, protocol and port
func allowOrDropNodeInputTrafficOnPort(op, nodeName, protocol, port string) {
	ipTablesArgs := []string{"INPUT", "-p", protocol, "--dport", port, "-j", "DROP"}
	switch op {
	case "Allow":
		op = "delete"
	case "Drop":
		op = "insert"
	default:
		framework.Failf("unsupported op %s", op)
	}
	updateIPTablesRulesForNode(op, nodeName, ipTablesArgs, false)
	updateIPTablesRulesForNode(op, nodeName, ipTablesArgs, true)
}

func updateIPTablesRulesForNode(op, nodeName string, ipTablesArgs []string, ipv6 bool) {
	args := []string{"get", "pods", "--selector=app=ovnkube-node", "--field-selector", fmt.Sprintf("spec.nodeName=%s", nodeName), "-o", "jsonpath={.items..metadata.name}"}
	ovnKubePodName := e2ekubectl.RunKubectlOrDie(ovnNamespace, args...)
	iptables := "iptables"
	if ipv6 {
		iptables = "ip6tables"
	}

	args = []string{"exec", ovnKubePodName, "-c", getNodeContainerName(), "--", iptables, "--check"}
	_, err := e2ekubectl.RunKubectl(ovnNamespace, append(args, ipTablesArgs...)...)
	// errors known to be equivalent to not found
	notFound1 := "No chain/target/match by that name"
	notFound2 := "does a matching rule exist in that chain?"
	notFound := err != nil && (strings.Contains(err.Error(), notFound1) || strings.Contains(err.Error(), notFound2))
	if err != nil && !notFound {
		framework.Failf("failed to check existance of %s rule on node %s: %v", iptables, nodeName, err)
	}
	if op == "delete" && notFound {
		// rule is not there
		return
	} else if op == "insert" && err == nil {
		// rule is already there
		return
	}
	args = []string{"exec", ovnKubePodName, "-c", getNodeContainerName(), "--", iptables, "--" + op}
	framework.Logf("%s %s rule: %q on node %s", op, iptables, strings.Join(ipTablesArgs, ","), nodeName)
	e2ekubectl.RunKubectlOrDie(ovnNamespace, append(args, ipTablesArgs...)...)
}

func randStr(n int) string {
	rand.Seed(time.Now().UnixNano())
	b := make([]byte, n)
	for i := range b {
		// randomly select 1 character from given charset
		b[i] = charset[rand.Intn(len(charset))]
	}
	return string(b)
}

func isIPv4Supported() bool {
	val, present := os.LookupEnv("KIND_IPV4_SUPPORT")
	return present && val == "true"
}

func isIPv6Supported() bool {
	val, present := os.LookupEnv("KIND_IPV6_SUPPORT")
	return present && val == "true"
}

func isInterconnectEnabled() bool {
	val, present := os.LookupEnv("OVN_ENABLE_INTERCONNECT")
	return present && val == "true"
}

func isUDNHostIsolationDisabled() bool {
	val, present := os.LookupEnv("DISABLE_UDN_HOST_ISOLATION")
	return present && val == "true"
}

func isNetworkSegmentationEnabled() bool {
	val, present := os.LookupEnv("ENABLE_NETWORK_SEGMENTATION")
	return present && val == "true"
}

func isLocalGWModeEnabled() bool {
	val, present := os.LookupEnv("OVN_GATEWAY_MODE")
	return present && val == "local"
}

func singleNodePerZone() bool {
	if singleNodePerZoneResult == nil {
		args := []string{"get", "pods", "--selector=app=ovnkube-node", "-o", "jsonpath={.items[0].spec.containers[*].name}"}
		containerNames := e2ekubectl.RunKubectlOrDie(ovnNamespace, args...)
		result := true
		for _, containerName := range strings.Split(containerNames, " ") {
			if containerName == "ovnkube-node" {
				result = false
				break
			}
		}
		singleNodePerZoneResult = &result
	}
	return *singleNodePerZoneResult
}

func getNodeContainerName() string {
	if singleNodePerZone() {
		return "ovnkube-controller"
	}
	return "ovnkube-node"
}

// getNodeZone returns the node's zone
func getNodeZone(node *v1.Node) (string, error) {
	nodeZone, ok := node.Annotations[ovnNodeZoneNameAnnotation]
	if !ok {
		return "", fmt.Errorf("zone for the node %s not set in the annotation %s", node.Name, ovnNodeZoneNameAnnotation)
	}

	return nodeZone, nil
}

// adds route to a docker node with a full mask
func addRouteToNode(nodeName string, ips []string, mtu int) error {
	return routeToNode(nodeName, ips, mtu, true)
}

// removes a route on a docker node
func delRouteToNode(nodeName string, ips []string) error {
	return routeToNode(nodeName, ips, 0, false)
}

// executes route commands on a node, if add is true, the route is added
// otherwise removed
func routeToNode(nodeName string, ips []string, mtu int, add bool) error {
	ipOp := "del"
	if add {
		ipOp = "add"
	}
	for _, ip := range ips {
		mask := 32
		ipCmd := []string{"ip"}
		if utilnet.IsIPv6String(ip) {
			mask = 128
			ipCmd = []string{"ip", "-6"}
		}
		var err error
		cmd := []string{"docker", "exec", nodeName}
		cmd = append(cmd, ipCmd...)
		cmd = append(cmd, "route", ipOp, fmt.Sprintf("%s/%d", ip, mask), "dev", "breth0")
		if mtu != 0 {
			cmd = append(cmd, "mtu", strconv.Itoa(mtu))
		}
		_, err = runCommand(cmd...)
		if err != nil {
			return err
		}
	}
	return nil
}

// CaptureContainerOutput captures output of a container according to the
// right-most match of the provided regex. Returns a map of subexpression name
// to subexpression capture. A zero string name `""` maps to the full expression
// capture.
func CaptureContainerOutput(ctx context.Context, c clientset.Interface, namespace, pod, container, regexpr string) (map[string]string, error) {
	regex, err := regexp.Compile(regexpr)
	if err != nil {
		return nil, fmt.Errorf("failed to compile regexp %q: %w", regexpr, err)
	}

	output, err := e2epod.GetPodLogs(ctx, c, namespace, pod, container)
	if err != nil {
		return nil, fmt.Errorf("failed to get output for container %q of pod %q in namespace %q", container, pod, namespace)
	}

	matches := regex.FindAllStringSubmatch(output, -1)
	if len(matches) == 0 {
		return nil, fmt.Errorf("failed to match regexp %q in output %q", regexpr, output)
	}
	match := matches[len(matches)-1]

	numSubExp := regex.NumSubexp()
	matchMap := make(map[string]string, numSubExp+1)
	matchMap[""] = match[0]
	if numSubExp == 0 {
		return matchMap, nil
	}

	subExpNames := regex.SubexpNames()
	for _, name := range subExpNames[1:] {
		index := regex.SubexpIndex(name)
		matchMap[name] = match[index]
	}

	return matchMap, nil
}

// It checks whether config.DisablePacketMTUCheck is set or not
func isDisablePacketMTUCheckEnabled() bool {
	val, present := os.LookupEnv("OVN_DISABLE_PKT_MTU_CHECK")
	return present && val == "true"
}

// getGatewayMTUSupport returns true if gateway-mtu-support annotataion
// is not set on the node, otherwise it returns false as the value of the
// annotation also get set to false
func getGatewayMTUSupport(node *v1.Node) bool {
	_, ok := node.Annotations[ovnGatewayMTUSupport]
	if !ok {
		return true
	}
	return false
}

func isKernelModuleLoaded(nodeName, kernelModuleName string) bool {
	out, err := runCommand(containerRuntime, "exec", nodeName, "lsmod")
	if err != nil {
		framework.Failf("failed to list kernel modules for node %s: %v", nodeName, err)
	}
	for _, module := range strings.Split(out, "\n") {
		if strings.HasPrefix(module, kernelModuleName) {
			return true
		}
	}
	return false
}

func matchIPv4StringFamily(ipStrings []string) (string, error) {
	return util.MatchIPStringFamily(false /*ipv4*/, ipStrings)
}

func matchIPv6StringFamily(ipStrings []string) (string, error) {
	return util.MatchIPStringFamily(true /*ipv6*/, ipStrings)
}
