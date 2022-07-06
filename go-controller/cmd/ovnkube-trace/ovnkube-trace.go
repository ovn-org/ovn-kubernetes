package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"

	types "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	util "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	kapi "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/klog/v2"
)

const (
	// Colors for bash highlighting.
	reset  = "\033[0m"
	red    = "\033[31m"
	green  = "\033[32m"
	italic = "\033[3m"
	bold   = "\033[1m"

	// OVN related.
	ovnNodeL3GatewayConfig = "k8s.ovn.org/l3-gateway-config"
)

var (
	level klog.Level
)

type l3GatewayConfig struct {
	Mode string
}

type OvsInterface struct {
	Name   string
	Ofport string
}

type SvcInfo struct {
	SvcName      string // The service's name
	SvcNamespace string // The service's namespace
	ClusterIP    string // The service's cluster IP address
	PodName      string // The first Endpoint subset.Addresses[].TargetRef.Name that can be found (a pod name)
	PodNamespace string // The namespace of the selected pod
	PodIP        string // The IP address of the selected pod
	PodPort      string // Endpoint target port used to reach the pod in PodName
}

type NodeInfo struct {
	NodeExternalBridgeName string // The name of the node's bridge, e.g. breth0 or br-ex
	OvnK8sMp0PortName      string // ovn-k8s-mp0
	OvnK8sMp0OfportNum     string // ofport num of ovn-k8s-mp0
	K8sNodeNamePort        string // k8s-<nodeName>, e.g. k8s-ovn-worker, only useful for host networked pods
	NodeName               string // The name of the node that the pod runs on
	OvnKubePodName         string // The OvnKube pod on the same node as this pod
	RoutingViaHost         bool   // The gateway mode, true for 'routingViaHost' or false for 'routingViaOVN'
}

type PodInfo struct {
	NodeInfo
	PrimaryInterfaceName string // primary pod interface name inside the pod
	IP                   string // the primary interface's primary IP address
	MAC                  string // the primary interface's MAC address
	VethName             string // veth peer of the primary interface of the pod
	OfportNum            string // ofport number of veth interface or for host net pods of ovn-k8s-mp0
	PodName              string // name of the pod
	PodNamespace         string // the pod's namespace
	ContainerName        string // the pod's principal container name (the first container found atm)
	RtosMAC              string // router to switch mac address, the L2 address of the first hop router of the pod
	HostNetwork          bool   // if this pod is host networked or not
}

// String returns a JSON representation of the SvcInfo object, or "" on failure.
func (si *SvcInfo) String() string {
	b, err := json.Marshal(*si)
	if err != nil {
		return ""
	}
	return string(b)
}

// String returns a JSON representation of the PodInfo object, or "" on failure.
func (pi *PodInfo) String() string {
	b, err := json.Marshal(*pi)
	if err != nil {
		return ""
	}
	return string(b)
}

func (si SvcInfo) getL3Ver() string {
	if net.ParseIP(si.ClusterIP).To4() != nil {
		return "ip4"
	}
	return "ip6"
}

func (si PodInfo) getL3Ver() string {
	if net.ParseIP(si.IP).To4() != nil {
		return "ip4"
	}
	return "ip6"
}

func (si *SvcInfo) FullyQualifiedPodName() string {
	return fmt.Sprintf("%s_%s", si.PodNamespace, si.PodName)
}

func (pi *PodInfo) FullyQualifiedPodName() string {
	return fmt.Sprintf("%s_%s", pi.PodNamespace, pi.PodName)
}

// execInPod runs a command inside the given container. Requires bash. Returns Stdout, Stderr, err.
func execInPod(coreclient *corev1client.CoreV1Client, restconfig *rest.Config, namespace string, podName string, containerName string, cmd string, in string) (string, string, error) {
	klog.V(5).Infof(
		"Running command inside container: namespace: %s, podName: %s, containerName: %s, cmd: %s, stdin: %s%s%s",
		namespace,
		podName,
		containerName,
		cmd,
		italic, in, reset,
	)

	scheme := runtime.NewScheme()
	if err := kapi.AddToScheme(scheme); err != nil {
		klog.Exitf("Error adding to scheme: %v", err)
	}
	parameterCodec := runtime.NewParameterCodec(scheme)

	useStdin := false
	if in != "" {
		useStdin = true
	}

	// Prepare the API URL used to execute another process within the Pod.
	req := coreclient.RESTClient().
		Post().
		Namespace(namespace).
		Resource("pods").
		Name(podName).
		SubResource("exec").
		VersionedParams(&kapi.PodExecOptions{
			Container: containerName,
			Command:   []string{"bash", "-c", cmd},
			Stdin:     useStdin,
			Stdout:    true,
			Stderr:    true,
			TTY:       false,
		}, parameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(restconfig, "POST", req.URL())
	if err != nil {
		return "", "", err
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	var stdin io.Reader

	if useStdin {
		stdin = strings.NewReader(in)
	} else {
		stdin = nil
	}

	err = exec.Stream(remotecommand.StreamOptions{
		Stdin:  stdin,
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if err != nil {
		return stdout.String(), stderr.String(), err
	}

	return stdout.String(), stderr.String(), err
}

// isRoutingViaHost returns the gateway mode, either 'true' for 'routingViaHost' or 'false' for 'routingViaOVN'.
// In order to do so, it looks for annotation 'k8s.ovn.org/l3-gateway-config' on the provided node.
// That annotation should contain a JSON string like: '{"default":{"mode":"shared", ...}}'.
// It will then determine the routing mode from that annotation if it is valid or return error otherwise.
func isRoutingViaHost(coreclient *corev1client.CoreV1Client, restconfig *rest.Config, ovnNamespace, ovnKubePodName, nodeName string) (bool, error) {
	node, err := coreclient.Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
	if err != nil {
		return false, err
	}

	l3GwConfigParsed := make(map[string]l3GatewayConfig)
	var l3GwConfig string
	var ok bool
	var defaultL3GwConfigParsed l3GatewayConfig

	annotations := node.GetAnnotations()
	l3GwConfig, ok = annotations[ovnNodeL3GatewayConfig]
	if !ok {
		return false, fmt.Errorf("could not find l3GwConfig annotation '%s' on node '%s'", ovnNodeL3GatewayConfig, nodeName)
	}
	err = json.Unmarshal([]byte(l3GwConfig), &l3GwConfigParsed)
	if err != nil {
		return false, fmt.Errorf("could not determine gateway mode from annotations on node %s, err: %q", node.Name, err)
	}
	defaultL3GwConfigParsed, ok = l3GwConfigParsed["default"]
	if !ok {
		return false, fmt.Errorf("could not determine gateway mode from annotations on node %s, no default entry in l3GwConfig: %v", node.Name, l3GwConfigParsed)
	}
	if defaultL3GwConfigParsed.Mode == "local" {
		klog.V(5).Infof("Cluster gateway mode is routingViaHost according to annotation on node %s, %s", node.Name, l3GwConfig)
		return true, nil
	} else if defaultL3GwConfigParsed.Mode == "shared" {
		klog.V(5).Infof("Cluster gateway mode is routingViaOVN according to annotation on node %s, %s", node.Name, l3GwConfig)
		return false, nil
	}

	return false, fmt.Errorf("could not determine gateway mode from annotations on node %s, unknown mode in l3GwConfig: %s", node.Name, defaultL3GwConfigParsed.Mode)
}

// getPodMAC returns the pod's MAC address.
func getPodMAC(client *corev1client.CoreV1Client, pod *kapi.Pod) (podMAC string, err error) {
	if pod.Spec.HostNetwork {
		node, err := client.Nodes().Get(context.TODO(), pod.Spec.NodeName, metav1.GetOptions{})
		if err != nil {
			return "", err
		}

		nodeMAC, err := util.ParseNodeManagementPortMACAddress(node)
		if err != nil {
			return "", err
		}
		if nodeMAC != nil {
			podMAC = nodeMAC.String()
		}
	} else {
		podAnnotation, err := util.UnmarshalPodAnnotation(pod.ObjectMeta.Annotations)
		if err != nil {
			return "", err
		}
		if podAnnotation != nil {
			podMAC = podAnnotation.MAC.String()
		}
	}

	return podMAC, nil
}

// getOvnKubePodOnNode returns the name of the ovnkube-node pod that is running on a given node.
func getOvnKubePodOnNode(coreclient *corev1client.CoreV1Client, ovnNamespace string, nodeName string) (string, error) {
	// Get pods in the openshift-ovn-kubernetes namespace
	podsOvn, errOvn := coreclient.Pods(ovnNamespace).List(context.TODO(), metav1.ListOptions{})
	if errOvn != nil {
		klog.Infof("Cannot find pods in %s namespace, err: %v", ovnNamespace, errOvn)
		return "", errOvn
	}
	var ovnkubePod *kapi.Pod
	// Find ovnkube-node-xxx pod running on the same node as Pod
	for _, podOvn := range podsOvn.Items {
		podOvn := podOvn
		if podOvn.Spec.NodeName == nodeName {
			if appLabel, ok := podOvn.Labels["app"]; ok && appLabel == "ovnkube-node" {
				klog.V(5).Infof("==> pod %s is running on node %s", podOvn.Name, nodeName)
				ovnkubePod = &podOvn
				break
			}
		}
	}
	if ovnkubePod == nil {
		err := fmt.Errorf("cannot find ovnkube-node pod on node %s in namespace %s", nodeName, ovnNamespace)
		return "", err
	}
	return ovnkubePod.Name, nil
}

// getPodOvsInterfaceNameAndOfport searches the node's OVS database for information
// about this pod's OVS interface and returns the name and ofport fields.
// It will run `ovs-vsctl --columns name,ofport find interface external_ids:iface-id=%s` with the given `$namespace-$pod` tuple and it will then parse the
// result into a map[string]string that maps the keys to their values.
func getPodOvsInterfaceNameAndOfport(coreclient *corev1client.CoreV1Client, restconfig *rest.Config, ovnNamespace, ovnkubePodName, fullyQualifiedPodName string) (*OvsInterface, error) {
	var interfaceInfo OvsInterface

	findInterfaceCmd := fmt.Sprintf("ovs-vsctl --columns name,ofport find interface external_ids:iface-id=%s", fullyQualifiedPodName)
	findInterfaceStdout, findInterfaceStderr, err := execInPod(coreclient, restconfig, ovnNamespace, ovnkubePodName, "ovnkube-node", findInterfaceCmd, "")
	if err != nil {
		return nil, err
	}

	var key string
	var value string
	scanner := bufio.NewScanner(strings.NewReader(findInterfaceStdout))
	for scanner.Scan() {
		splitLine := strings.Split(scanner.Text(), ":")
		if len(splitLine) != 2 {
			continue
		}
		key = strings.TrimSpace(splitLine[0])
		value = strings.TrimSpace(splitLine[1])
		switch key {
		case "name":
			interfaceInfo.Name = strings.Trim(value, "\"")
		case "ofport":
			interfaceInfo.Ofport = value
		default:
		}
	}

	if interfaceInfo.Name == "" || interfaceInfo.Ofport == "" {
		return nil, fmt.Errorf("could not find interface info for: "+
			"fullyQualifiedPodName: %s, ovnNamespace: %s, ovnkubePodName: %s, cmd: %s. Got: %s, %s, parsed interface info: %v",
			fullyQualifiedPodName,
			ovnNamespace,
			ovnkubePodName,
			findInterfaceCmd,
			findInterfaceStdout,
			findInterfaceStderr,
			interfaceInfo,
		)
	}
	return &interfaceInfo, nil
}

// getSvcInfo builds the SvcInfo object for this service. PodName/PodNamespace/PodIP are for the first valid endpoint pod that can be found for this service.
func getSvcInfo(coreclient *corev1client.CoreV1Client, restconfig *rest.Config, svcName string, ovnNamespace string, namespace string, cmd string) (svcInfo *SvcInfo, err error) {
	// Get service with the name supplied by svcName
	svc, err := coreclient.Services(namespace).Get(context.TODO(), svcName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("service %s in namespace %s not found, err: %v", svcName, namespace, err)
	}
	klog.V(5).Infof("==> Got service %s in namespace %s\n", svcName, namespace)

	clusterIP := svc.Spec.ClusterIP
	if clusterIP == "" || clusterIP == "None" {
		return nil, fmt.Errorf("ClusterIP for service %s in namespace %s not available", svcName, namespace)
	}
	klog.V(5).Infof("==> Got service %s ClusterIP is %s\n", svcName, clusterIP)

	svcInfo = &SvcInfo{
		SvcName:      svcName,
		SvcNamespace: namespace,
		ClusterIP:    svc.Spec.ClusterIP,
	}

	ep, err := coreclient.Endpoints(namespace).Get(context.TODO(), svcName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("endpoints for service %s in namespace %s not found, err: %v", svcName, namespace, err)
	}
	klog.V(5).Infof("==> Got Endpoint %v for service %s in namespace %s\n", ep, svcName, namespace)

	err = extractSubsetInfo(ep.Subsets, svcInfo)
	if err != nil {
		return nil, err
	}

	return svcInfo, err
}

// extractSubsetInfo copies information from the endpoint subsets into the SvcInfo object.
// Modifies the svcInfo object the pointer of which is passed to it.
func extractSubsetInfo(subsets []kapi.EndpointSubset, svcInfo *SvcInfo) error {
	for _, subset := range subsets {
		klog.V(5).Infof("==> Trying to extract information for service %s in namespace %s from subset %v",
			svcInfo.SvcName, svcInfo.SvcNamespace, subset)

		// Find a port for the subset.
		var podPort string
		for _, port := range subset.Ports {
			podPort = strconv.Itoa(int(port.Port))
			if podPort == "" {
				klog.V(5).Infof("==> Could not parse port %d, skipping.", port)
				continue // with the next port
			}
		}
		// Continue with the next subset if podPort is empty.
		if podPort == "" {
			continue // with the next subset
		}

		// Parse pod information from the subset addresses. One of the subsets must have a valid address which does not belong
		// to a host networked pod.
		for _, epAddress := range subset.Addresses {
			// This is nil for host networked services.
			if epAddress.TargetRef == nil {
				klog.V(5).Infof("Address %v belongs to a host networked pod. Skipping.", epAddress)
				continue // with the next address
			}
			if epAddress.TargetRef.Name == "" || epAddress.TargetRef.Namespace == "" || epAddress.IP == "" {
				klog.V(5).Infof("Address %v contains invalid data. podName %s, podNamespace %s, podIP %s. Skipping.",
					epAddress, epAddress.TargetRef.Name, epAddress.TargetRef.Namespace, epAddress.IP)
				continue // with the next address
			}

			// At this point, we should have found valid pod information + a port, so set them and return nil.
			svcInfo.PodName = epAddress.TargetRef.Name
			svcInfo.PodNamespace = epAddress.TargetRef.Namespace
			svcInfo.PodIP = epAddress.IP
			svcInfo.PodPort = podPort
			klog.V(5).Infof("==> Got address and port information for service endpoint. podName: %s, podNamespace: %s, podIP: %s, podPort: %s",
				svcInfo.PodName, svcInfo.PodNamespace, svcInfo.PodIP, svcInfo.PodPort)
			return nil
		}
	}

	return fmt.Errorf("could not extract pod and port information from endpoints for service %s in namespace %s.", svcInfo.SvcName, svcInfo.SvcNamespace)
}

// getPodInfo returns a pointer to a fully populated PodInfo struct, or error on failure.
func getPodInfo(coreclient *corev1client.CoreV1Client, restconfig *rest.Config, podName string, ovnNamespace string, namespace string, cmd string) (podInfo *PodInfo, err error) {
	// Create a PodInfo object with the base information already added, such as
	// IP, PodName, ContainerName, NodeName, HostNetwork, Namespace, PrimaryInterfaceName
	pod, err := coreclient.Pods(namespace).Get(context.TODO(), podName, metav1.GetOptions{})
	if err != nil {
		klog.V(1).Infof("Pod %s in namespace %s not found\n", podName, namespace)
		return nil, err
	}
	podInfo = &PodInfo{
		IP:            pod.Status.PodIP,
		PodName:       pod.Name,
		ContainerName: pod.Spec.Containers[0].Name,
		HostNetwork:   pod.Spec.HostNetwork,
		PodNamespace:  pod.Namespace,
	}
	podInfo.NodeName = pod.Spec.NodeName

	// Get the pod's ovnkubePod.
	podInfo.OvnKubePodName, err = getOvnKubePodOnNode(coreclient, ovnNamespace, podInfo.NodeName)
	if err != nil {
		klog.V(1).Infof("Problem obtaining ovnkube pod name of Pod %s in namespace %s\n", podName, namespace)
		return nil, err
	}

	// Get the node's gateway mode
	podInfo.RoutingViaHost, err = isRoutingViaHost(coreclient, restconfig, ovnNamespace, podInfo.OvnKubePodName, podInfo.NodeName)
	if err != nil {
		return nil, err
	}

	// Get the pod's MAC address.
	podInfo.MAC, err = getPodMAC(coreclient, pod)
	if err != nil {
		klog.V(1).Infof("Problem obtaining Ethernet address of Pod %s in namespace %s\n", podName, namespace)
		return nil, err
	}

	// Find rtos MAC (this is the pod's first hop router).
	lspCmd := "ovn-nbctl " + cmd + " --bare --no-heading --column=mac list logical-router-port " + types.RouterToSwitchPrefix + podInfo.NodeName
	ipOutput, ipError, err := execInPod(coreclient, restconfig, ovnNamespace, podInfo.OvnKubePodName, "ovnkube-node", lspCmd, "")
	if err != nil {
		return nil, fmt.Errorf("execInPod() failed. err: %s, stderr: %s, stdout: %s, podInfo: %v", err, ipError, ipOutput, podInfo)
	}
	podInfo.RtosMAC = strings.Replace(ipOutput, "\n", "", -1)

	// Set information specific to ovn-k8s-mp0. This info is required for routingViaHost gateway mode traffic to an external IP
	// destination.
	podInfo.OvnK8sMp0PortName = types.K8sMgmtIntfName
	portCmd := fmt.Sprintf("ovs-vsctl get Interface %s ofport", podInfo.OvnK8sMp0PortName)
	localOutput, localError, err := execInPod(coreclient, restconfig, ovnNamespace, podInfo.OvnKubePodName, "ovnkube-node", portCmd, "")
	if err != nil {
		return nil, fmt.Errorf("execInPod() failed. err: %s, stderr: %s, stdout: %s, podInfo: %v", err, localError, localOutput, podInfo)
	}
	podInfo.OvnK8sMp0OfportNum = strings.Replace(localOutput, "\n", "", -1)

	// Set information specific to host networked pods or non-host networked pods.
	if podInfo.HostNetwork {
		podInfo.PrimaryInterfaceName = util.GetLegacyK8sMgmtIntfName(podInfo.NodeName)
		podInfo.K8sNodeNamePort = types.K8sPrefix + podInfo.NodeName
		podInfo.VethName = podInfo.OvnK8sMp0PortName
		podInfo.OfportNum = podInfo.OvnK8sMp0OfportNum
	} else {
		// Get the pod's interface information
		ovsInterfaceInformation, err := getPodOvsInterfaceNameAndOfport(coreclient, restconfig, ovnNamespace, podInfo.OvnKubePodName, podInfo.FullyQualifiedPodName())
		if err != nil {
			return nil, err
		}
		podInfo.PrimaryInterfaceName = "eth0"
		podInfo.VethName = ovsInterfaceInformation.Name
		podInfo.OfportNum = ovsInterfaceInformation.Ofport
	}

	podInfo.NodeExternalBridgeName, err = getNodeExternalBridgeName(coreclient, restconfig, ovnNamespace, podInfo.OvnKubePodName, cmd, podInfo.NodeName)
	if err != nil {
		return nil, err
	}

	return podInfo, err
}

// getNodeExternalBridgeName gets the name of the external bridge of this node, e.g. breth0 or br-ex.
func getNodeExternalBridgeName(coreclient *corev1client.CoreV1Client, restconfig *rest.Config, ovnNamespace, podName, nbcmd, nodeName string) (string, error) {
	cmd := "ovn-nbctl " + nbcmd + " --bare --no-heading --column=name find Logical_Switch_Port options:network_name=" + types.PhysicalNetworkName
	stdout, stderr, err := execInPod(coreclient, restconfig, ovnNamespace, podName, "ovnkube-node", cmd, "")
	if err != nil {
		return "", fmt.Errorf("execInPod() failed with %s stderr %s stdout %s \n", err, stderr, stdout)
	}
	scanner := bufio.NewScanner(strings.NewReader(stdout))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, "_"+nodeName) {
			splitLine := strings.Split(scanner.Text(), "_")
			if len(splitLine) == 2 {
				return splitLine[0], nil
			}
		}
	}
	return "", fmt.Errorf("could not find external bridge for node %s in getNodeBridgeName()", nodeName)
}

// getOvnNamespace searches all namespaces for pods with the label selector app=ovnkube-node.
// If it can find such pods, it returns the namespace that they reside in, or error otherwise.
func getOvnNamespace(coreclient *corev1client.CoreV1Client, override string) (string, error) {
	if override != "" {
		return override, nil
	}

	listOptions := metav1.ListOptions{
		LabelSelector: "app=ovnkube-node",
	}
	pods, err := coreclient.Pods("").List(context.TODO(), listOptions)
	if err != nil || len(pods.Items) == 0 {
		klog.Infof("Cannot find ovnkube pods in any namespace")
		return "", err
	}

	return pods.Items[0].Namespace, nil
}

// Get the OVN Database URIs from the first container found in any pod in the ovn-kubernetes namespace with name "ovnkube-master"
// Returns nbAddress, sbAddress, protocol == "ssl", nil
func getDatabaseURIs(coreclient *corev1client.CoreV1Client, restconfig *rest.Config, ovnNamespace string) (string, string, bool, error) {
	containerName := "ovnkube-master"
	var err error

	found := false
	var podName string

	listOptions := metav1.ListOptions{}
	pods, err := coreclient.Pods(ovnNamespace).List(context.TODO(), listOptions)
	if err != nil {
		return "", "", false, err
	}
	for _, pod := range pods.Items {
		for _, container := range pod.Spec.Containers {
			if container.Name == containerName {
				found = true
				podName = pod.Name
				break
			}
		}
	}
	if !found {
		klog.V(5).Infof("Cannot find ovnkube pods with container %s", containerName)
		return "", "", false, fmt.Errorf("cannot find ovnkube pods with container: %s", containerName)
	}
	klog.V(5).Infof("Found pod '%s' with container '%s'", podName, containerName)

	psCmd := "ps -eo args | grep '/usr/bin/[o]vnkube'"
	hostOutput, hostError, err := execInPod(coreclient, restconfig, ovnNamespace, podName, containerName, psCmd, "")
	if err != nil {
		klog.V(5).Infof("execInPod('%s') failed with err: '%s', stderr: '%s', stdout: '%s', Pod Name '%s' \n", psCmd, err, hostError, hostOutput, podName)
		return "", "", false, err
	}

	re := regexp.MustCompile(`--nb-address(=| )[^\s]+`)
	nbAddress := strings.Replace(
		re.FindString(hostOutput)[13:],
		"://",
		":",
		-1)
	re = regexp.MustCompile(`--sb-address(=| )[^\s]+`)
	sbAddress := strings.Replace(
		re.FindString(hostOutput)[13:],
		"://",
		":",
		-1)
	re = regexp.MustCompile(`(ssl|tcp)`)
	protocol := re.FindString(nbAddress)

	klog.V(5).Infof("Nb address for OVN database communication is %s", nbAddress)
	klog.V(5).Infof("Sb address for OVN database communication is %s", sbAddress)
	klog.V(5).Infof("Protocol for OVN database communication is %s", protocol)

	return nbAddress, sbAddress, protocol == "ssl", nil
}

// printSuccessOrFailure will print a success or failure message. If searchString is set, then we expect to find a match for the
// regexp given in searchString.
func printSuccessOrFailure(commandDescription, src, dst, commandStdout, commandStderr string, err error, searchString string) {
	if err != nil {
		klog.Exitf("%s error %v stdOut: %s\n stdErr: %s", commandDescription, err, commandStdout, commandStderr)
	}
	klog.V(2).Infof("%s Output:\n%s%s%s\n", commandDescription, italic, commandStdout, reset)

	if searchString != "" {
		match, err := regexp.MatchString(searchString, commandStdout)
		if err != nil {
			klog.Exitf("Unexpected failure matching regex '%s' to commandStdout '%s', err: %s", searchString, commandStdout, err)
		}
		if match {
			// Write the result to stdout.
			fmt.Printf("%s%s%s indicates success from %s to %s%s\n", green, bold, commandDescription, src, dst, reset)
			// Log further info on log level 1.
			klog.V(1).Infof("%sSearch string matched:\n%s%s\n", green, searchString, reset)
		} else {
			// Write the result to stdout.
			fmt.Printf("%s%s%s indicates failure from %s to %s%s\n", red, bold, commandDescription, src, dst, reset)
			// Log further info on log level 1.
			klog.V(1).Infof("%sSearch string not matched:\n%s%s\n", red, searchString, reset)
			os.Exit(-1)
		}
	} else {
		// Write the result to stdout.
		fmt.Printf("%s%s%s indicates success from %s to %s%s\n", green, bold, commandDescription, src, dst, reset)
	}
}

// runOvnTraceToService runs an ovntrace from src pod to dst service. If dstSvcInfo == nil, then skip all steps.
func runOvnTraceToService(coreclient *corev1client.CoreV1Client, restconfig *rest.Config, srcPodInfo *PodInfo, dstSvcInfo *SvcInfo, sbcmd, ovnNamespace, protocol, dstPort string) {
	var inport string
	inport = srcPodInfo.FullyQualifiedPodName()
	if srcPodInfo.HostNetwork {
		inport = srcPodInfo.K8sNodeNamePort
	}
	cmd := fmt.Sprintf(`ovn-trace %[1]s %[2]s --ct=new `+
		`'inport=="%[3]s" && eth.src==%[4]s && eth.dst==%[5]s && %[6]s.src==%[7]s && %[8]s.dst==%[9]s && ip.ttl==64 && %[10]s.dst==%[11]s && %[10]s.src==52888' --lb-dst %[12]s:%[13]s`,
		sbcmd,                 // 1
		srcPodInfo.NodeName,   // 2
		inport,                // 3
		srcPodInfo.MAC,        // 4
		srcPodInfo.RtosMAC,    // 5
		srcPodInfo.getL3Ver(), // 6
		srcPodInfo.IP,         // 7
		dstSvcInfo.getL3Ver(), // 8
		dstSvcInfo.ClusterIP,  // 9
		protocol,              // 10
		dstPort,               // 11
		dstSvcInfo.PodIP,      // 12
		dstSvcInfo.PodPort,    // 13
	)
	klog.V(4).Infof("ovn-trace command from src to service clusterIP is %s", cmd)

	ovnSrcDstOut, ovnSrcDstErr, err := execInPod(coreclient, restconfig, ovnNamespace, srcPodInfo.OvnKubePodName, "ovnkube-node", cmd, "")
	successString := fmt.Sprintf(`output to "%s"`, dstSvcInfo.FullyQualifiedPodName())
	printSuccessOrFailure("ovn-trace from source pod to service clusterIP", srcPodInfo.PodName, dstSvcInfo.SvcName, ovnSrcDstOut, ovnSrcDstErr, err, successString)
}

// runOvnTraceToIP runs an ovntrace from src pod to dst IP address (should be external to the cluster).
// Returns the node that the trace will exit on.
func runOvnTraceToIP(coreclient *corev1client.CoreV1Client, restconfig *rest.Config, srcPodInfo *PodInfo, parsedDstIP net.IP, sbcmd, ovnNamespace, protocol, dstPort string) (string, string) {
	if srcPodInfo.HostNetwork {
		klog.Exitf("Pod cannot be on Host Network when tracing to an IP address; use ping\n")
	}

	l3ver := "ip6"
	if parsedDstIP.To4() != nil {
		l3ver = "ip4"
	}
	if srcPodInfo.getL3Ver() != l3ver {
		klog.Exitf("Pod src IP address family (address: %s) and destination IP address family (address: %s) do not match",
			srcPodInfo.IP, parsedDstIP)
	}

	cmd := fmt.Sprintf(`ovn-trace %[1]s %[2]s `+
		`'inport=="%[3]s" && eth.src==%[4]s && eth.dst==%[5]s && %[6]s.src==%[7]s && %[8]s.dst==%[9]s && ip.ttl==64 && %[10]s.dst==%[11]s && %[10]s.src==52888'`,
		sbcmd,                              // 1
		srcPodInfo.NodeName,                // 2
		srcPodInfo.FullyQualifiedPodName(), // 3
		srcPodInfo.MAC,                     // 4
		srcPodInfo.RtosMAC,                 // 5
		l3ver,                              // 6
		srcPodInfo.IP,                      // 7
		l3ver,                              // 8
		parsedDstIP,                        // 9
		protocol,                           // 10
		dstPort,                            // 11
	)
	klog.V(4).Infof("ovn-trace command from pod to IP is %s", cmd)

	// This is different depending on:
	// a) if this is routingViaHost gateway mode, output to "k8s-<nodename>"
	// b) for routingViaHost gateway egressip and routingViaOVN gateway mode, go out of <bridge name>_<node name>
	successString := fmt.Sprintf(`output to "(.*)_(.*)", type "localnet"|output to "k8s-%s"`, srcPodInfo.NodeName)
	// Run the command and check if succesString was found.
	ovnSrcDstOut, ovnSrcDstErr, err := execInPod(coreclient, restconfig, ovnNamespace, srcPodInfo.OvnKubePodName, "ovnkube-node", cmd, "")
	printSuccessOrFailure("ovn-trace from pod to IP", srcPodInfo.PodName, parsedDstIP.String(), ovnSrcDstOut, ovnSrcDstErr, err, successString)

	// Print some additional information about the node where this request leaves from as well
	// as the SNAT IP address.
	snatString := `ct_snat\((.*)\)`
	re := regexp.MustCompile(snatString)
	subMatches := re.FindSubmatch([]byte(ovnSrcDstOut))
	if len(subMatches) >= 2 {
		klog.V(5).Infof("Could find SNAT for this trace command, this must be routingViaOVN gateway mode, any mode with EgressIP or any mode with EgressGW.")
		snat := subMatches[len(subMatches)-1]
		re = regexp.MustCompile(successString)
		subMatches = re.FindSubmatch([]byte(ovnSrcDstOut))
		// We should never hit this (printSuccessOrFailure checks the same already above).
		if len(subMatches) < 3 {
			klog.Exitf("Could not determine the output port for this trace command, subMatches: %q\n", subMatches)
		}
		node := subMatches[len(subMatches)-1]
		bridgeName := subMatches[len(subMatches)-2]
		klog.V(1).Infof("%sout on node %s via Logical_Switch_Port %s with SNAT %s%s\n", green, node, bridgeName, snat, reset)

		return string(node), string(bridgeName)
	}

	klog.V(5).Infof("Could not find SNAT for this trace command, this must be routingViaHost gateway mode without EgressIP.")
	nodeNameRegex := `output to "k8s-(.*)",`
	re = regexp.MustCompile(nodeNameRegex)
	subMatches = re.FindSubmatch([]byte(ovnSrcDstOut))
	if len(subMatches) < 2 {
		klog.Exitf("Could not determine node name / bridge name of egress node in runOvnTraceToIP()")
	}
	node := subMatches[len(subMatches)-1]
	klog.V(1).Infof("%sout on node %s%s\n", green, node, reset)
	return string(node), ""
}

// runOvnTraceToPod runs an ovntrace from src pod to dst pod.
func runOvnTraceToPod(coreclient *corev1client.CoreV1Client, restconfig *rest.Config, direction string, srcPodInfo, dstPodInfo *PodInfo, sbcmd, ovnNamespace, protocol, dstPort string) {
	var inport string
	inport = srcPodInfo.FullyQualifiedPodName()
	if srcPodInfo.HostNetwork {
		inport = srcPodInfo.K8sNodeNamePort
	}
	cmd := fmt.Sprintf(`ovn-trace %[1]s %[2]s `+
		`'inport=="%[3]s" && eth.src==%[4]s && eth.dst==%[5]s && %[6]s.src==%[7]s && %[8]s.dst==%[9]s && ip.ttl==64 && %[10]s.dst==%[11]s && %[10]s.src==52888'`,
		sbcmd,                 // 1
		srcPodInfo.NodeName,   // 2
		inport,                // 3
		srcPodInfo.MAC,        // 4
		srcPodInfo.RtosMAC,    // 5
		srcPodInfo.getL3Ver(), // 6
		srcPodInfo.IP,         // 7
		dstPodInfo.getL3Ver(), // 8
		dstPodInfo.IP,         // 9
		protocol,              // 10
		dstPort,               // 11
	)
	klog.V(4).Infof("ovn-trace command from %s is %s", direction, cmd)

	var successString string
	if dstPodInfo.HostNetwork {
		// OVN will get as far as this sending node (the src node).
		// routingViaHost gateway mode or if both pods are on the same node example: k8s-ovn-worker.
		// routingViaOVN gateway mode example: "breth0_ovn-worker2.
		if srcPodInfo.RoutingViaHost || srcPodInfo.NodeName == dstPodInfo.NodeName {
			successString = fmt.Sprintf(`output to "%s%s"`, types.K8sPrefix, srcPodInfo.NodeName)
		} else {
			successString = fmt.Sprintf(`output to "%s_%s"`, srcPodInfo.NodeExternalBridgeName, srcPodInfo.NodeName)
		}
	} else {
		successString = fmt.Sprintf(`output to "%s"`, dstPodInfo.FullyQualifiedPodName())
	}
	ovnSrcDstOut, ovnSrcDstErr, err := execInPod(coreclient, restconfig, ovnNamespace, srcPodInfo.OvnKubePodName, "ovnkube-node", cmd, "")
	printSuccessOrFailure("ovn-trace "+direction, srcPodInfo.PodName, dstPodInfo.PodName, ovnSrcDstOut, ovnSrcDstErr, err, successString)
}

// runOfprotoTraceToPod runs an ofproto/trace command from the src to the destination pod.
func runOfprotoTraceToPod(coreclient *corev1client.CoreV1Client, restconfig *rest.Config, direction string, srcPodInfo, dstPodInfo *PodInfo, ovnNamespace, protocol, dstPort string) string {
	cmd := fmt.Sprintf(`ovs-appctl ofproto/trace br-int `+
		`"in_port=%[1]s, %[2]s, dl_src=%[3]s, dl_dst=%[4]s, nw_src=%[5]s, nw_dst=%[6]s, nw_ttl=64, %[7]s_dst=%[8]s, %[7]s_src=12345"`,
		srcPodInfo.VethName, // 1
		protocol,            // 2
		srcPodInfo.MAC,      // 3
		srcPodInfo.RtosMAC,  // 4
		srcPodInfo.IP,       // 5
		dstPodInfo.IP,       // 6
		protocol,            // 7
		dstPort,             // 8
	)
	klog.V(4).Infof("ovs-appctl ofproto/trace command from %s is %s", direction, cmd)

	var successString string
	if srcPodInfo.NodeName == dstPodInfo.NodeName {
		klog.V(5).Infof("Pods are on the same node %s", dstPodInfo.NodeName)
		// Trace will end at the ovs port number of the dest pod.
		// For host networked pods, getPodInfo sets OfportNum to the number of OvnK8sMp0OfportNum, see getPodInfo.
		successString = "output:" + dstPodInfo.OfportNum + "\n\nFinal flow:"
	} else if dstPodInfo.HostNetwork {
		klog.V(5).Infof("Pod %s is on host network on node %s", dstPodInfo.PodName, dstPodInfo.NodeName)
		// Different paths for routingViaHost gateway mode and routingViaOVN gateway mode.
		// Trace will end at the ovs port number of the management port of this sending node for routingViaHost gateway mode.
		// For routingViaOVN gateway mode, we will simply look for an SNAT.
		if !srcPodInfo.RoutingViaHost {
			successString = `ct\(.*,nat`
		} else {
			successString = fmt.Sprintf(`output:%s\n\nFinal flow:`, srcPodInfo.OvnK8sMp0OfportNum)
		}
	} else {
		klog.V(5).Infof("Pods are on node: %s and node %s", srcPodInfo.NodeName, dstPodInfo.NodeName)
		successString = "-> output to kernel tunnel"
	}
	appSrcDstOut, appSrcDstErr, err := execInPod(coreclient, restconfig, ovnNamespace, srcPodInfo.OvnKubePodName, "ovnkube-node", cmd, "")
	printSuccessOrFailure("ovs-appctl ofproto/trace "+direction, srcPodInfo.PodName, dstPodInfo.PodName, appSrcDstOut, appSrcDstErr, err, successString)

	return appSrcDstOut
}

// runOfprotoTraceToIP runs an ofproto/trace command from the src to the destination pod.
// egressNodeName is the exit node, as determined by an ovn-trace command that was run earlier.
// egressBridgeName is the name of the exit bridge (for EgressIPs, EgressGW and also for routingViaOVN mode).
// If egressBridgeName == "", then this is routingViaHost Gateway mode without an EgressIP / EgressGW.
func runOfprotoTraceToIP(coreclient *corev1client.CoreV1Client, restconfig *rest.Config, srcPodInfo *PodInfo, dstIP net.IP, ovnNamespace, protocol, dstPort, egressNodeName, egressBridgeName string) string {
	cmd := fmt.Sprintf(`ovs-appctl ofproto/trace br-int `+
		`"in_port=%[1]s, %[2]s, dl_src=%[3]s, dl_dst=%[4]s, nw_src=%[5]s, nw_dst=%[6]s, nw_ttl=64, %[2]s_dst=%[7]s, %[2]s_src=12345"`,
		srcPodInfo.VethName, // 1
		protocol,            // 2
		srcPodInfo.MAC,      // 3
		srcPodInfo.RtosMAC,  // 4
		srcPodInfo.IP,       // 5
		dstIP.String(),      // 6
		dstPort,             // 7
	)
	direction := "pod to IP"
	klog.V(4).Infof("ovs-appctl ofproto/trace command from %s is %s", direction, cmd)

	var successString string
	if srcPodInfo.NodeName != egressNodeName {
		klog.V(5).Infof("Pod is on node %s and traffic egress via node %s", srcPodInfo.NodeName, egressNodeName)
		successString = "-> output to kernel tunnel"
	} else {
		if egressBridgeName != "" {
			// routingViaOVN gateway mode or EgressIP matched traffic, or ICNI traffic.
			klog.V(5).Infof("Pod is on node %s and traffic egress via the same node's bridge %s", srcPodInfo.NodeName, egressBridgeName)
			successString = fmt.Sprintf(`bridge\("%s"\)`, egressBridgeName)
		} else {
			// routingViaHost gateway mode and no EgressIP matched traffic.
			klog.V(5).Infof("Pod is on node %s and traffic egress via the same node's port %s (%s)", srcPodInfo.NodeName, srcPodInfo.OvnK8sMp0PortName, srcPodInfo.OvnK8sMp0OfportNum)
			successString = fmt.Sprintf(`output:%s`, srcPodInfo.OvnK8sMp0OfportNum)
		}
	}
	appSrcDstOut, appSrcDstErr, err := execInPod(coreclient, restconfig, ovnNamespace, srcPodInfo.OvnKubePodName, "ovnkube-node", cmd, "")
	printSuccessOrFailure(fmt.Sprintf("ovs-appctl ofproto/trace %s", direction), srcPodInfo.PodName, dstIP.String(), appSrcDstOut, appSrcDstErr, err, successString)

	return appSrcDstOut
}

// installOvnDetraceDependencies installs dependencies for ovn-detrace with pip3 in case they are missing (for older images).
func installOvnDetraceDependencies(coreclient *corev1client.CoreV1Client, restconfig *rest.Config, podName, ovnNamespace string) {
	dependencies := map[string]string{
		"ovs":       "if type -p ovn-detrace >/dev/null 2>&1; then echo 'true' ; fi",
		"pyOpenSSL": "if rpm -qa | egrep -q python3-pyOpenSSL; then echo 'true'; fi",
	}
	for dependency, dependencyCmd := range dependencies {
		depVerifyOut, depVerifyErr, err := execInPod(coreclient, restconfig, ovnNamespace, podName, "ovnkube-node", dependencyCmd, "")
		if err != nil {
			klog.Exitf("Dependency verification error in pod %s, container %s. Error '%v', stdOut: '%s'\n stdErr: %s", podName, "ovnkube-node", err, depVerifyOut, depVerifyErr)
		}
		trueFalse := strings.TrimSuffix(depVerifyOut, "\n")
		klog.V(10).Infof("Dependency check '%s' in pod '%s', container '%s' yielded '%s'", dependencyCmd, podName, "ovnkube-node", trueFalse)
		if trueFalse != "true" {
			installCmd := "pip3 install " + dependency
			depInstallOut, depInstallErr, err := execInPod(coreclient, restconfig, ovnNamespace, podName, "ovnkube-node", installCmd, "")
			if err != nil {
				klog.Exitf("ovn-detrace error in pod %s, container %s. Error '%v', stdOut: '%s'\n stdErr: %s", podName, "ovnkube-node", err, depInstallOut, depInstallErr)
			}
			klog.V(1).Infof("Install ovn-detrace Output: %s\n", depInstallOut)
		}
	}
}

// runOvnDetrace runs an ovn-detrace command for the given input.
func runOvnDetrace(coreclient *corev1client.CoreV1Client, restconfig *rest.Config, direction string, srcPodInfo *PodInfo, dstName string, appSrcDstOut, ovnNamespace, nbUri, sbUri, sslCertKeys string) {
	cmd := fmt.Sprintf(`ovn-detrace --ovnnb=%[1]s --ovnsb=%[2]s %[3]s --ovsdb=unix:/var/run/openvswitch/db.sock`,
		nbUri,       // 1
		sbUri,       // 2
		sslCertKeys, // 3
	)
	klog.V(4).Infof("ovn-detrace command from %s is %s", direction, cmd)

	dtraceSrcDstOut, dtraceSrcDstErr, err := execInPod(coreclient, restconfig, ovnNamespace, srcPodInfo.OvnKubePodName, "ovnkube-node", cmd, appSrcDstOut)
	printSuccessOrFailure("ovn-detrace "+direction, srcPodInfo.PodName, dstName, dtraceSrcDstOut, dtraceSrcDstErr, err, "")
}

// displayNodeInfo shows a summary about nodes in this cluster.
func displayNodeInfo(coreclient *corev1client.CoreV1Client) {
	// List all Nodes.
	nodes, err := coreclient.Nodes().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		klog.Exitf(" Unexpected error: %v", err)
	}

	masters := make(map[string]string)
	workers := make(map[string]string)

	klog.V(5).Infof(" Nodes: ")
	for _, node := range nodes.Items {
		// look for both labels until master label is removed in kubernetes 1.25
		// https://github.com/kubernetes/kubernetes/pull/107533
		_, foundMaster := node.Labels["node-role.kubernetes.io/master"]
		_, foundControlPlane := node.Labels["node-role.kubernetes.io/control-plane"]
		if foundMaster || foundControlPlane {
			klog.V(5).Infof("  Name: %s is a master", node.Name)
			for _, s := range node.Status.Addresses {
				klog.V(5).Infof("  Address Type: %s - Address: %s", s.Type, s.Address)
				//if s.Type == corev1client.NodeInternalIP {
				if s.Type == "InternalIP" {
					masters[node.Name] = s.Address
				}
			}
		} else {
			klog.V(5).Infof("  Name: %s is a worker", node.Name)
			for _, s := range node.Status.Addresses {
				klog.V(5).Infof("  Address Type: %s - Address: %s", s.Type, s.Address)
				//if s.Type == corev1client.NodeInternalIP {
				if s.Type == "InternalIP" {
					workers[node.Name] = s.Address
				}
			}
		}
	}

	if len(masters) < 3 {
		klog.V(5).Infof("Cluster does not have 3 masters, found %d", len(masters))
	}
}

// setLogLevel sets the log level for this application.
func setLogLevel(loglevel string) {
	klog.InitFlags(nil)
	klog.SetOutput(os.Stderr)
	err := level.Set(loglevel)
	if err != nil {
		klog.Exitf("fatal: cannot set logging level\n")
	}
	klog.V(1).Infof("Log level set to: %s", loglevel)
}

func main() {
	var protocol string
	var parsedDstIP net.IP
	var err error

	// Parse CLI flags.
	cliConfig := flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	cfgNamespace := flag.String("ovn-config-namespace", "", "namespace used by ovn-config itself")
	srcNamespace := flag.String("src-namespace", "default", "k8s namespace of source pod")
	dstNamespace := flag.String("dst-namespace", "default", "k8s namespace of dest pod")
	srcPodName := flag.String("src", "", "src: source pod name")
	dstPodName := flag.String("dst", "", "dest: destination pod name")
	dstSvcName := flag.String("service", "", "service: destination service name")
	dstIP := flag.String("dst-ip", "", "destination IP address (meant for tests to external targets)")
	dstPort := flag.String("dst-port", "80", "dst-port: destination port")
	tcp := flag.Bool("tcp", false, "use tcp transport protocol")
	udp := flag.Bool("udp", false, "use udp transport protocol")
	loglevel := flag.String("loglevel", "0", "loglevel: klog level")
	flag.Parse()

	// Set the application's log level.
	setLogLevel(*loglevel)

	// Verify CLI flags.
	if *srcPodName == "" {
		klog.Exitf("Usage: source pod must be specified")
	}
	if !*tcp && !*udp {
		klog.Exitf("Usage: either tcp or udp must be specified")
	}
	if *udp && *tcp {
		klog.Exitf("Usage: Both tcp and udp cannot be specified at the same time")
	}
	if *tcp {
		protocol = "tcp"
	}
	if *udp {
		if *dstSvcName != "" {
			klog.Exitf("Usage: udp option is not compatible with destination service trace")
		}
		protocol = "udp"
	}
	targetOptions := 0
	if *dstPodName != "" {
		targetOptions++
	}
	if *dstSvcName != "" {
		targetOptions++
	}
	if *dstIP != "" {
		targetOptions++
		parsedDstIP = net.ParseIP(*dstIP)
		if parsedDstIP == nil {
			klog.Exitf("Usage: cannot parse IP address provided in -dst-ip")
		}
	}
	if targetOptions != 1 {
		klog.Exitf("Usage: exactly one of -dst, -service or -dst-ip must be set")
	}

	// Get the ClientConfig.
	// This might work better?  https://godoc.org/sigs.k8s.io/controller-runtime/pkg/client/config
	// When supplied the kubeconfig supplied via cli takes precedence
	var restconfig *rest.Config
	if *cliConfig != "" {
		// use the current context in kubeconfig
		restconfig, err = clientcmd.BuildConfigFromFlags("", *cliConfig)
		if err != nil {
			klog.Exitf(" Unexpected error: %v", err)
		}
	} else {
		// Instantiate loader for kubeconfig file.
		kubeconfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			clientcmd.NewDefaultClientConfigLoadingRules(),
			&clientcmd.ConfigOverrides{},
		)

		// Get a rest.Config from the kubeconfig file.  This will be passed into all
		// the client objects we create.
		restconfig, err = kubeconfig.ClientConfig()
		if err != nil {
			klog.Exitf(" Unexpected error: %v", err)
		}
	}

	// Create a Kubernetes core/v1 client.
	coreclient, err := corev1client.NewForConfig(restconfig)
	if err != nil {
		klog.Exitf(" Unexpected error: %v", err)
	}

	// Get the namespace that OVN pods reside in.
	ovnNamespace, err := getOvnNamespace(coreclient, *cfgNamespace)
	if err != nil {
		klog.Exitf(" Unexpected error: %v", err)
	}
	klog.V(5).Infof("OVN Kubernetes namespace is %s", ovnNamespace)

	// Show some information about the nodes in this cluster - only if log level 5 or higher.
	if lvl, err := strconv.Atoi(*loglevel); err == nil && lvl >= 5 {
		displayNodeInfo(coreclient)
	}

	// Common ssl parameters
	var sslCertKeys string
	nbUri, sbUri, useSSL, err := getDatabaseURIs(coreclient, restconfig, ovnNamespace)
	if err != nil {
		klog.Exitf("Failed to get database URIs: %v\n", err)
	}
	if useSSL {
		sslCertKeys = "-p /ovn-cert/tls.key -c /ovn-cert/tls.crt -C /ovn-ca/ca-bundle.crt "
	} else {
		sslCertKeys = " "
	}
	nbcmd := sslCertKeys + "--db " + nbUri
	klog.V(5).Infof("The nbcmd is %s", nbcmd)
	sbcmd := sslCertKeys + "--db " + sbUri
	klog.V(5).Infof("The sbcmd is %s", sbcmd)

	// Get info needed for the src Pod
	srcPodInfo, err := getPodInfo(coreclient, restconfig, *srcPodName, ovnNamespace, *srcNamespace, nbcmd)
	if err != nil {
		klog.Exitf("Failed to get information from pod %s: %v", *srcPodName, err)
	}
	klog.V(5).Infof("srcPodInfo is %s\n", srcPodInfo)

	// 1) Either run a trace from source pod to destination IP and return ...
	if parsedDstIP != nil {
		klog.V(5).Infof("Running a trace to an IP address")
		egressNodeName, egressBridgeName := runOvnTraceToIP(coreclient, restconfig, srcPodInfo, parsedDstIP, sbcmd, ovnNamespace, protocol, *dstPort)
		appSrcDstOut := runOfprotoTraceToIP(coreclient, restconfig, srcPodInfo, parsedDstIP, ovnNamespace, protocol, *dstPort, egressNodeName, egressBridgeName)
		runOvnDetrace(coreclient, restconfig, "pod to external IP", srcPodInfo, parsedDstIP.String(), appSrcDstOut, ovnNamespace, nbUri, sbUri, sslCertKeys)
		return
	}

	// 2) ... or run a trace to destination service / destination pod.
	// Get destination service information if a destination service name was provided.
	klog.V(5).Infof("Running a trace to a cluster local svc or to another pod")
	var dstSvcInfo *SvcInfo
	if *dstSvcName != "" {
		// Get dst service
		dstSvcInfo, err = getSvcInfo(coreclient, restconfig, *dstSvcName, ovnNamespace, *dstNamespace, nbcmd)
		if err != nil {
			klog.Exitf("Failed to get information from service %s: %v", *dstSvcName, err)
		}
		klog.V(5).Infof("dstSvcInfo is %s\n", dstSvcInfo)
		// Set dst pod name, we'll use this to run through pod-pod tests as if use supplied this pod
		*dstPodName = dstSvcInfo.PodName
		klog.V(1).Infof("Using pod %s in service %s to test against", dstSvcInfo.PodName, *dstSvcName)
	}

	// Now get info needed for the dst Pod
	dstPodInfo, err := getPodInfo(coreclient, restconfig, *dstPodName, ovnNamespace, *dstNamespace, nbcmd)
	if err != nil {
		klog.Exitf("Failed to get information from pod %s: %v", *dstPodName, err)
	}
	klog.V(5).Infof("dstPodInfo is %s\n", dstPodInfo)

	// At least one pod must not be on the Host Network
	if srcPodInfo.HostNetwork && dstPodInfo.HostNetwork {
		klog.Exitf("Both pods cannot be on Host Network; use ping")
	}

	// ovn-trace commands
	if dstSvcInfo != nil {
		runOvnTraceToService(coreclient, restconfig, srcPodInfo, dstSvcInfo, sbcmd, ovnNamespace, protocol, *dstPort)
	}
	runOvnTraceToPod(coreclient, restconfig, "source pod to destination pod", srcPodInfo, dstPodInfo, sbcmd, ovnNamespace, protocol, *dstPort)
	runOvnTraceToPod(coreclient, restconfig, "destination pod to source pod", dstPodInfo, srcPodInfo, sbcmd, ovnNamespace, protocol, *dstPort)

	// ovs-appctl ofproto/trace commands
	appSrcDstOut := runOfprotoTraceToPod(coreclient, restconfig, "source pod to destination pod", srcPodInfo, dstPodInfo, ovnNamespace, protocol, *dstPort)
	appDstSrcOut := runOfprotoTraceToPod(coreclient, restconfig, "destination pod to source pod", dstPodInfo, srcPodInfo, ovnNamespace, protocol, *dstPort)

	// ovn-detrace commands below
	// Install dependencies with pip3 in case they are missing (for older images)
	installOvnDetraceDependencies(coreclient, restconfig, srcPodInfo.OvnKubePodName, ovnNamespace)
	installOvnDetraceDependencies(coreclient, restconfig, dstPodInfo.OvnKubePodName, ovnNamespace)
	runOvnDetrace(coreclient, restconfig, "source pod to destination pod", srcPodInfo, dstPodInfo.PodName, appSrcDstOut, ovnNamespace, nbUri, sbUri, sslCertKeys)
	runOvnDetrace(coreclient, restconfig, "destination pod to source pod", dstPodInfo, srcPodInfo.PodName, appDstSrcOut, ovnNamespace, nbUri, sbUri, sslCertKeys)
}
