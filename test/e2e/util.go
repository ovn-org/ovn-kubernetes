package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"regexp"
	"strings"
	"time"

	"github.com/onsi/ginkgo"
	"github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	testutils "k8s.io/kubernetes/test/utils"
	admissionapi "k8s.io/pod-security-admission/api"
	utilnet "k8s.io/utils/net"
)

const (
	ovnNamespace   = "ovn-kubernetes"
	ovnNodeSubnets = "k8s.ovn.org/node-subnets"
)

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
func unmarshalPodAnnotation(annotations map[string]string) (*PodAnnotation, error) {
	ovnAnnotation, ok := annotations[podNetworkAnnotation]
	if !ok {
		return nil, newAnnotationNotSetError("could not find OVN pod annotation in %v", annotations)
	}

	podNetworks := make(map[string]podAnnotation)
	if err := json.Unmarshal([]byte(ovnAnnotation), &podNetworks); err != nil {
		return nil, fmt.Errorf("failed to unmarshal ovn pod annotation %q: %v",
			ovnAnnotation, err)
	}
	tempA := podNetworks["default"]
	a := &tempA

	podAnnotation := &PodAnnotation{}
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

// pokeEndpointHostname leverages a container running the netexec command to send a "hostname" request to a target running
// netexec on the given target host / protocol / port.
// Returns the name of backend pod.
func pokeEndpointHostname(clientContainer, protocol, targetHost string, targetPort int32) string {
	ipPort := net.JoinHostPort("localhost", "80")
	cmd := []string{"docker", "exec", clientContainer}

	// we leverage the dial command from netexec, that is already supporting multiple protocols
	curlCommand := strings.Split(fmt.Sprintf("curl -g -q -s http://%s/dial?request=hostname&protocol=%s&host=%s&port=%d&tries=1",
		ipPort,
		protocol,
		targetHost,
		targetPort), " ")

	cmd = append(cmd, curlCommand...)
	res, err := runCommand(cmd...)
	framework.ExpectNoError(err, "failed to run command on external container")
	hostName, err := parseNetexecResponse(res)
	if err != nil {
		framework.Logf("FAILED Command was %s", curlCommand)
		framework.Logf("FAILED Response was %v", res)
	}
	framework.ExpectNoError(err)

	return hostName
}

// wrapper logic around pokeEndpointHostname
// contact the ExternalIP service until each endpoint returns its hostname and return true, or false otherwise
func pokeExternalIpService(clientContainerName, protocol, externalAddress string, externalPort int32, maxTries int, nodesHostnames sets.String) bool {
	responses := sets.NewString()

	for i := 0; i < maxTries; i++ {
		epHostname := pokeEndpointHostname(clientContainerName, protocol, externalAddress, externalPort)
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
	cmd := []string{"docker", "exec", clientContainer}
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

// pokeEndpointClientIP leverages a container running the netexec command to send a "clientip" request to a target running
// netexec on the given target host / protocol / port.
// Returns the src ip of the packet.
func pokeEndpointClientIP(clientContainer, protocol, targetHost string, targetPort int32) string {
	ipPort := net.JoinHostPort("localhost", "80")
	cmd := []string{"docker", "exec", clientContainer}

	// we leverage the dial command from netexec, that is already supporting multiple protocols
	curlCommand := strings.Split(fmt.Sprintf("curl -g -q -s http://%s/dial?request=clientip&protocol=%s&host=%s&port=%d&tries=1",
		ipPort,
		protocol,
		targetHost,
		targetPort), " ")

	cmd = append(cmd, curlCommand...)
	res, err := runCommand(cmd...)
	framework.ExpectNoError(err, "failed to run command on external container")
	clientIP, err := parseNetexecResponse(res)
	framework.ExpectNoError(err)
	ip, _, err := net.SplitHostPort(clientIP)
	if err != nil {
		framework.Logf("FAILED Command was %s", curlCommand)
		framework.Logf("FAILED Response was %v", res)
	}
	framework.ExpectNoError(err, "failed to parse client ip:port")

	return ip
}

// curlInContainer leverages a container running the netexec command to send a request to a target running
// netexec on the given target host / protocol / port.
// Returns a pair of either result, nil or "", error in case of an error.
func curlInContainer(clientContainer, protocol, targetHost string, targetPort int32, endPoint string, maxTime int) (string, error) {
	cmd := []string{"docker", "exec", clientContainer}
	if utilnet.IsIPv6String(targetHost) {
		targetHost = fmt.Sprintf("[%s]", targetHost)
	}

	// we leverage the dial command from netexec, that is already supporting multiple protocols
	curlCommand := strings.Split(fmt.Sprintf("curl -g -q -s --max-time %d http://%s:%d/%s",
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

// Returns the container's ipv4 and ipv6 addresses IN ORDER
// related to the given network.
func getContainerAddressesForNetwork(container, network string) (string, string) {
	ipv4Format := fmt.Sprintf("{{.NetworkSettings.Networks.%s.IPAddress}}", network)
	ipv6Format := fmt.Sprintf("{{.NetworkSettings.Networks.%s.GlobalIPv6Address}}", network)

	ipv4, err := runCommand("docker", "inspect", "-f", ipv4Format, container)
	if err != nil {
		framework.Failf("failed to inspect external test container for its IPv4: %v", err)
	}
	ipv6, err := runCommand("docker", "inspect", "-f", ipv6Format, container)
	if err != nil {
		framework.Failf("failed to inspect external test container for its IPv4: %v", err)
	}
	return strings.TrimSuffix(ipv4, "\n"), strings.TrimSuffix(ipv6, "\n")
}

// deletePodSyncNS deletes a pod and wait for its deletion.
// accept the namespace as a parameter.
func deletePodSyncNS(clientSet kubernetes.Interface, namespace, podName string) {
	err := clientSet.CoreV1().Pods(namespace).Delete(context.Background(), podName, metav1.DeleteOptions{})
	framework.ExpectNoError(err, "Failed to delete pod %s in the default namespace", podName)

	gomega.Eventually(func() bool {
		_, err := clientSet.CoreV1().Pods(namespace).Get(context.Background(), podName, metav1.GetOptions{})
		return apierrors.IsNotFound(err)
	}, 3*time.Minute, 5*time.Second).Should(gomega.BeTrue(), "Pod was not being deleted")
}

// waitClusterHealthy ensures we have a given number of ovn-k worker and master nodes,
// as well as all nodes are healthy
func waitClusterHealthy(f *framework.Framework, numMasters int) error {
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
		afterNodes, err := e2enode.GetReadySchedulableNodes(f.ClientSet)
		if err != nil {
			return false, fmt.Errorf("failed to look for healthy nodes: %w", err)
		}
		if len(afterNodes.Items) != numNodes {
			framework.Logf("Not enough schedulable nodes, have %d want %d", len(afterNodes.Items), numNodes)
			return false, nil
		}

		podClient := f.ClientSet.CoreV1().Pods("ovn-kubernetes")
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
			LabelSelector: "name=ovnkube-master",
		})
		if err != nil {
			return false, fmt.Errorf("failed to list ovn-kube node pods: %w", err)
		}
		if len(podList.Items) != numMasters {
			framework.Logf("Not enough running ovnkube-master pods, want %d, have %d", numMasters, len(podList.Items))
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

// waitForDaemonSetUpdate waits for the daemon set in a given namespace to be
// successfully rolled out following an update.
//
// If allowedNotReadyNodes is -1, this method returns immediately without waiting.
func waitForDaemonSetUpdate(c clientset.Interface, ns string, dsName string, allowedNotReadyNodes int32, timeout time.Duration) error {
	if allowedNotReadyNodes == -1 {
		return nil
	}

	start := time.Now()
	framework.Logf("Waiting up to %v for daemonset %s in namespace %s to update",
		timeout, dsName, ns)

	return wait.Poll(framework.Poll, timeout, func() (bool, error) {
		ds, err := c.AppsV1().DaemonSets(ns).Get(context.TODO(), dsName, metav1.GetOptions{})
		if err != nil {
			framework.Logf("Error getting daemonset: %s in namespace: %s: %v", dsName, ns, err)
			return false, err
		}

		if ds.Generation <= ds.Status.ObservedGeneration {
			if ds.Status.UpdatedNumberScheduled < ds.Status.DesiredNumberScheduled {
				framework.Logf("Waiting for daemon set %q rollout to finish: %d out of %d new pods have been updated (%d seconds elapsed)", ds.Name,
					ds.Status.UpdatedNumberScheduled, ds.Status.DesiredNumberScheduled, int(time.Since(start).Seconds()))
				return false, nil
			}
			if ds.Status.NumberAvailable < ds.Status.DesiredNumberScheduled {
				framework.Logf("Waiting for daemon set %q rollout to finish: %d of %d updated pods are available (%d seconds elapsed)", ds.Name,
					ds.Status.NumberAvailable, ds.Status.DesiredNumberScheduled, int(time.Since(start).Seconds()))
				return false, nil
			}
			framework.Logf("daemon set %q successfully rolled out", ds.Name)
			return true, nil
		}

		framework.Logf("Waiting for daemon set: %s spec update to be observed...", dsName)
		return false, nil
	})
}

func pokePod(fr *framework.Framework, srcPodName string, dstPodIP string) error {
	stdout, stderr, err := fr.ExecShellInPodWithFullOutput(
		srcPodName,
		fmt.Sprintf("curl --output /dev/stdout -m 1 -I %s:8000 | head -n1", dstPodIP))
	if err == nil && stdout == "HTTP/1.1 200 OK" {
		return nil
	}
	return fmt.Errorf("http request failed; stdout: %s, err: %v", stdout+stderr, err)
}

func pokeExternalHostFromPod(fr *framework.Framework, namespace string, srcPodName, dstIp string, dstPort int) error {
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
	pod, err := f.PodClientNS(namespace).Get(context.TODO(), podName, metav1.GetOptions{})
	framework.ExpectNoError(err, "failed to get pod %v", podName)
	gomega.Expect(pod.Spec.Containers).NotTo(gomega.BeEmpty())
	return ExecCommandInContainerWithFullOutput(f, namespace, podName, pod.Spec.Containers[0].Name, cmd...)
}

// ExecCommandInContainerWithFullOutput is a shameless copy/paste from the framework methods so that we can specify the pod namespace.
func ExecCommandInContainerWithFullOutput(f *framework.Framework, namespace, podName, containerName string, cmd ...string) (string, string, error) {
	options := framework.ExecOptions{
		Command:            cmd,
		Namespace:          namespace,
		PodName:            podName,
		ContainerName:      containerName,
		Stdin:              nil,
		CaptureStdout:      true,
		CaptureStderr:      true,
		PreserveWhitespace: false,
	}
	return f.ExecWithOptions(options)
}

func assertAclLogs(targetNodeName string, policyNameRegex string, expectedAclVerdict string, expectedAclSeverity string) (bool, error) {
	framework.Logf("collecting the ovn-controller logs for node: %s", targetNodeName)
	targetNodeLog, err := runCommand([]string{"docker", "exec", targetNodeName, "grep", "acl_log", ovnControllerLogPath}...)
	if err != nil {
		return false, fmt.Errorf("error accessing logs in node %s: %v", targetNodeName, err)
	}

	framework.Logf("Ensuring the audit log contains: 'name=\"%s\"', 'verdict=%s' AND 'severity=%s'", policyNameRegex, expectedAclVerdict, expectedAclSeverity)
	for _, logLine := range strings.Split(targetNodeLog, "\n") {
		matched, err := regexp.MatchString(fmt.Sprintf("name=\"%s\"", policyNameRegex), logLine)
		if err != nil {
			return false, err
		}
		if matched &&
			strings.Contains(logLine, fmt.Sprintf("verdict=%s", expectedAclVerdict)) &&
			strings.Contains(logLine, fmt.Sprintf("severity=%s", expectedAclSeverity)) {
			return true, nil
		}
	}
	return false, nil
}

// patchService patches service serviceName in namespace serviceNamespace.
func patchService(c kubernetes.Interface, serviceName, serviceNamespace, jsonPath, value string) error {
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

// isDualStackCluster returns 'true' if at least one of the nodes has more than one node subnet.
// This can reliably be determined by checking that Annotations["k8s.ovn.org/node-subnets"] parses into map[string][]string.
func isDualStackCluster(nodes *v1.NodeList) bool {
	for _, node := range nodes.Items {
		annotation, ok := node.Annotations[ovnNodeSubnets]
		if !ok {
			continue
		}

		subnetsDual := make(map[string][]string)
		if err := json.Unmarshal([]byte(annotation), &subnetsDual); err == nil {
			return true
		}
	}
	return false
}

// used to inject OVN specific test actions
func wrappedTestFramework(basename string) *framework.Framework {
	f := newPrivelegedTestFramework(basename)
	// inject dumping dbs on failure
	ginkgo.JustAfterEach(func() {
		if !ginkgo.CurrentGinkgoTestDescription().Failed {
			return
		}

		ovnDocker := "ovn-control-plane"

		logLocation := "/var/log"
		dbLocation := "/var/lib/openvswitch"
		ovsdbLocation := "/etc/origin/openvswitch"
		dbs := []string{"ovnnb_db.db", "ovnsb_db.db"}
		ovsdb := "conf.db"

		testName := strings.Replace(ginkgo.CurrentGinkgoTestDescription().TestText, " ", "_", -1)
		logDir := fmt.Sprintf("%s/e2e-dbs/%s-%s", logLocation, testName, f.UniqueName)

		var args []string

		// grab all OVS dbs
		nodes, err := f.ClientSet.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})
		framework.ExpectNoError(err)
		for _, node := range nodes.Items {
			// ensure e2e-dbs directory with test case exists
			args = []string{"docker", "exec", node.Name, "mkdir", "-p", logDir}
			_, err = runCommand(args...)
			framework.ExpectNoError(err)

			// node name is the same in kapi and docker
			args = []string{"docker", "exec", node.Name, "cp", "-f", fmt.Sprintf("%s/%s", ovsdbLocation, ovsdb),
				fmt.Sprintf("%s/%s", logDir, fmt.Sprintf("%s-%s", node.Name, ovsdb))}
			_, err = runCommand(args...)
			framework.ExpectNoError(err)
		}

		args = []string{"docker", "exec", ovnDocker, "stat", fmt.Sprintf("%s/%s", dbLocation, dbs[0])}
		_, err = runCommand(args...)
		framework.ExpectNoError(err)

		// grab the OVN dbs
		for _, db := range dbs {
			args = []string{"docker", "exec", ovnDocker, "cp", "-f", fmt.Sprintf("%s/%s", dbLocation, db),
				fmt.Sprintf("%s/%s", logDir, db)}
			_, err = runCommand(args...)
			framework.ExpectNoError(err)
		}
	})

	return f
}

func newPrivelegedTestFramework(basename string) *framework.Framework {
	f := framework.NewDefaultFramework(basename)
	f.NamespacePodSecurityEnforceLevel = admissionapi.LevelPrivileged
	return f
}

// countAclLogs connects to <targetNodeName> (ovn-control-plane, ovn-worker or ovn-worker2 in kind environments) via the docker exec
// command and it greps for the string "acl_log" inside the OVN controller logs. It then checks if the line contains name=<policyNameRegex>
// and if it does, it increases the counter if both the verdict and the severity for this line match what's expected.
func countAclLogs(targetNodeName string, policyNameRegex string, expectedAclVerdict string, expectedAclSeverity string) (int, error) {
	count := 0

	framework.Logf("collecting the ovn-controller logs for node: %s", targetNodeName)
	targetNodeLog, err := runCommand([]string{"docker", "exec", targetNodeName, "cat", ovnControllerLogPath}...)
	if err != nil {
		return 0, fmt.Errorf("error accessing logs in node %s: %v", targetNodeName, err)
	}

	stringToMatch := fmt.Sprintf(
		".*acl_log.*name=\"%s\".*verdict=%s.*severity=%s.*",
		policyNameRegex,
		expectedAclVerdict,
		expectedAclSeverity)

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
