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
	utilnet "k8s.io/utils/net"
	"k8s.io/utils/strings/slices"
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

const (
	// nbdb and sbdb local socket file path and protocol string which are
	// used by ovnkube-trace for establishing the connection when node
	// runs on its own zone in an interconnect environment.
	nbdbServerSock = "unix:/var/run/ovn/ovnnb_db.sock"
	sbdbServerSock = "unix:/var/run/ovn/ovnsb_db.sock"
	sockProtocol   = "unix"
)

const (
	ip4 = "ip4"
	ip6 = "ip6"
)

var (
	level                    klog.Level
	ovnKubeNodePodContainers = []string{"ovnkube-node", "ovnkube-controller"}
)

type l3GatewayConfig struct {
	Mode string
}

// OvsInterface describes an OVS interface.
type OvsInterface struct {
	Name   string
	Ofport string
}

// SvcInfo contains information about a service.
type SvcInfo struct {
	SvcName      string   // The service's name
	SvcNamespace string   // The service's namespace
	ClusterIP    string   // The service's cluster IP address
	PodInfo      *PodInfo // The endpoint pod associated with the service
	PodPort      string   // Endpoint target port used to reach the pod in PodName
}

// NodeInfo contains node information.
type NodeInfo struct {
	NodeExternalBridgeName string // The name of the node's bridge, e.g. breth0 or br-ex
	OvnK8sMp0PortName      string // ovn-k8s-mp0
	OvnK8sMp0OfportNum     string // ofport num of ovn-k8s-mp0
	K8sNodeNamePort        string // k8s-<nodeName>, e.g. k8s-ovn-worker, only useful for host networked pods
	NodeName               string // The name of the node that the pod runs on
	OvnKubePodName         string // The OvnKube pod on the same node as this pod
	RoutingViaHost         bool   // The gateway mode, true for 'routingViaHost' or false for 'routingViaOVN'
}

// PodInfo contains pod information.
type PodInfo struct {
	NodeInfo
	PrimaryInterfaceName string // primary pod interface name inside the pod
	IP                   string // the primary interface's primary IP address
	IPVer                string // the address family of the primary IP address
	MAC                  string // the primary interface's MAC address
	VethName             string // veth peer of the primary interface of the pod
	OfportNum            string // ofport number of veth interface or for host net pods of ovn-k8s-mp0
	PodName              string // name of the pod
	PodNamespace         string // the pod's namespace
	ContainerName        string // the pod's principal container name (the first container found atm)
	OvnKubeContainerName string // name of the container running ovnkube-node component
	RtosMAC              string // router to switch mac address, the L2 address of the first hop router of the pod
	RtotsMAC             string // router to transit switch port mac address
	HostNetwork          bool   // if this pod is host networked or not
	IsInterConnect       bool   // indicates if the pod is running on ovn interconnect environment or not
	InterConnectZoneName string // contains interconnect zone name of the pod's hosting node.
	NbURI                string // pod's ovn nb db uri string
	SbURI                string // pod's ovn sb db uri string
	SslCertKeys          string // ssl cert keys string to access ovn nbdb/sbdb
	NbCommand            string // contains subset of nb command string to execute on ovn nbdb
	SbCommand            string // contains subset of sb command string to execute on ovn sbdb
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

// FullyQualifiedPodName returns the full name of the pod, <namespace>_<pod>.
func (si *SvcInfo) FullyQualifiedPodName() string {
	return si.PodInfo.FullyQualifiedPodName()
}

// FullyQualifiedPodName returns the full name of the pod, <namespace>_<pod>.
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

	//exec.Stream is deprecated, so we have to use withContext, context.TODO returns a non-nil empty context
	err = exec.StreamWithContext(context.TODO(), remotecommand.StreamOptions{
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
func getPodMAC(pod *kapi.Pod) (podMAC string, err error) {
	podAnnotation, err := util.UnmarshalPodAnnotation(pod.ObjectMeta.Annotations, types.DefaultNetworkName)
	if err != nil {
		return "", err
	}
	if podAnnotation != nil {
		podMAC = podAnnotation.MAC.String()
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
func getPodOvsInterfaceNameAndOfport(coreclient *corev1client.CoreV1Client, restconfig *rest.Config, podInfo *PodInfo, ovnNamespace, fullyQualifiedPodName string) (*OvsInterface, error) {
	var interfaceInfo OvsInterface

	findInterfaceCmd := fmt.Sprintf("ovs-vsctl --columns name,ofport find interface external_ids:iface-id=%s", fullyQualifiedPodName)
	findInterfaceStdout, findInterfaceStderr, err := execInPod(coreclient, restconfig, ovnNamespace, podInfo.OvnKubePodName, podInfo.OvnKubeContainerName, findInterfaceCmd, "")
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
			podInfo.OvnKubePodName,
			findInterfaceCmd,
			findInterfaceStdout,
			findInterfaceStderr,
			interfaceInfo,
		)
	}
	return &interfaceInfo, nil
}

// getSvcInfo builds the SvcInfo object for this service. PodName/PodNamespace/PodIP are for the first valid endpoint pod that can be found for this service.
func getSvcInfo(coreclient *corev1client.CoreV1Client, restconfig *rest.Config, svcName string, ovnNamespace string, namespace, addressFamily string) (svcInfo *SvcInfo, err error) {
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
	clusterIPStr := utilnet.ParseIPSloppy(clusterIP).String()
	klog.V(5).Infof("==> Got service %s ClusterIP is %s\n", svcName, clusterIPStr)

	svcInfo = &SvcInfo{
		SvcName:      svcName,
		SvcNamespace: namespace,
		ClusterIP:    clusterIPStr,
	}

	ep, err := coreclient.Endpoints(namespace).Get(context.TODO(), svcName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("endpoints for service %s in namespace %s not found, err: %v", svcName, namespace, err)
	}
	klog.V(5).Infof("==> Got Endpoint %v for service %s in namespace %s\n", ep, svcName, namespace)

	err = extractSubsetInfo(coreclient, restconfig, ep.Subsets, svcInfo, ovnNamespace, addressFamily)
	if err != nil {
		return nil, err
	}

	return svcInfo, err
}

// extractSubsetInfo copies information from the endpoint subsets into the SvcInfo object.
// Modifies the svcInfo object the pointer of which is passed to it.
func extractSubsetInfo(coreclient *corev1client.CoreV1Client, restconfig *rest.Config, subsets []kapi.EndpointSubset, svcInfo *SvcInfo, ovnNamespace, addressFamily string) error {
	for _, subset := range subsets {
		klog.V(5).Infof("==> Trying to extract information for service %s in namespace %s from subset %v",
			svcInfo.SvcName, svcInfo.SvcNamespace, subset)

		// Find a port for the subset.
		var podPort string
		for _, port := range subset.Ports {
			podPort = strconv.Itoa(int(port.Port))
			if podPort == "" {
				klog.V(5).Infof("==> Could not parse port %v, skipping.", port)
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

			// Get info needed for the src Pod
			svcPodInfo, err := getPodInfo(coreclient, restconfig, epAddress.TargetRef.Name, ovnNamespace, epAddress.TargetRef.Namespace, addressFamily)
			if err != nil {
				klog.Exitf("Failed to get information from pod %s: %v", epAddress.TargetRef.Name, err)
			}
			klog.V(5).Infof("svcPodInfo is %s\n", svcPodInfo)

			// At this point, we should have found valid pod information + a port, so set them and return nil.
			svcInfo.PodInfo = svcPodInfo
			svcInfo.PodPort = podPort
			klog.V(5).Infof("==> Got address and port information for service endpoint. podName: %s, podNamespace: %s, podIP: %s, podPort: %s, podNodeName: %s",
				svcInfo.PodInfo.PodName, svcInfo.PodInfo.PodNamespace, svcInfo.PodInfo.IP, svcInfo.PodPort, svcInfo.PodInfo.NodeName)
			return nil
		}
	}

	return fmt.Errorf("could not extract pod and port information from endpoints for service %s in namespace %s", svcInfo.SvcName, svcInfo.SvcNamespace)
}

// getPodInfo returns a pointer to a fully populated PodInfo struct, or error on failure.
func getPodInfo(coreclient *corev1client.CoreV1Client, restconfig *rest.Config, podName string, ovnNamespace string, namespace, addressFamily string) (podInfo *PodInfo, err error) {
	// Create a PodInfo object with the base information already added, such as
	// IP, PodName, ContainerName, NodeName, HostNetwork, Namespace, PrimaryInterfaceName
	pod, err := coreclient.Pods(namespace).Get(context.TODO(), podName, metav1.GetOptions{})
	if err != nil {
		klog.V(1).Infof("Pod %s in namespace %s not found\n", podName, namespace)
		return nil, err
	}

	podIP, err := getDesiredPodIP(pod, addressFamily)
	if err != nil {
		klog.V(1).Infof("Pod %s in namespace %s doesn't have desired ip address configured\n", podName, namespace)
		return nil, err
	}

	podInfo = &PodInfo{
		IP:            podIP,
		IPVer:         addressFamily,
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

	podInfo, err = getDatabaseURIs(coreclient, restconfig, ovnNamespace, podInfo)
	if err != nil {
		klog.Exitf("Failed to get database URIs: %v\n", err)
	}

	// Get the pod's MAC address.
	// If hostnetwork, use mp0 mac
	if pod.Spec.HostNetwork {
		podInfo.OvnK8sMp0PortName = types.K8sMgmtIntfName
		portCmd := fmt.Sprintf("ovs-vsctl get Interface %s mac_in_use", podInfo.OvnK8sMp0PortName)
		localOutput, localError, err := execInPod(coreclient, restconfig, ovnNamespace, podInfo.OvnKubePodName, podInfo.OvnKubeContainerName, portCmd, "")
		if err != nil {
			return nil, fmt.Errorf("execInPod() failed. err: %s, stderr: %s, stdout: %s, podInfo: %v", err, localError, localOutput, podInfo)
		}
		localOutput = strings.ReplaceAll(localOutput, "\n", "")
		podInfo.MAC = strings.ReplaceAll(localOutput, "\"", "")
	} else {
		podInfo.MAC, err = getPodMAC(pod)
		if err != nil {
			klog.V(1).Infof("Problem obtaining Ethernet address of Pod %s in namespace %s\n", podName, namespace)
			return nil, err
		}
	}

	// Find rtos MAC (this is the pod's first hop router).
	podInfo.RtosMAC, err = getRouterPortMacAddress(coreclient, restconfig, podInfo, ovnNamespace, types.RouterToSwitchPrefix)
	if err != nil {
		return nil, err
	}

	// Find rtots MAC (this is the pod's first hop router when ovn is in interconnected zone).
	if podInfo.IsInterConnect {
		podInfo.RtotsMAC, err = getRouterPortMacAddress(coreclient, restconfig, podInfo, ovnNamespace, types.RouterToTransitSwitchPrefix)
		if err != nil {
			return nil, err
		}
	}

	// Set information specific to ovn-k8s-mp0. This info is required for routingViaHost gateway mode traffic to an external IP
	// destination.
	podInfo.OvnK8sMp0PortName = types.K8sMgmtIntfName
	portCmd := fmt.Sprintf("ovs-vsctl get Interface %s ofport", podInfo.OvnK8sMp0PortName)
	localOutput, localError, err := execInPod(coreclient, restconfig, ovnNamespace, podInfo.OvnKubePodName, podInfo.OvnKubeContainerName, portCmd, "")
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
		ovsInterfaceInformation, err := getPodOvsInterfaceNameAndOfport(coreclient, restconfig, podInfo, ovnNamespace, podInfo.FullyQualifiedPodName())
		if err != nil {
			return nil, err
		}
		podInfo.PrimaryInterfaceName = "eth0"
		podInfo.VethName = ovsInterfaceInformation.Name
		podInfo.OfportNum = ovsInterfaceInformation.Ofport
	}

	podInfo.NodeExternalBridgeName, err = getNodeExternalBridgeName(coreclient, restconfig, ovnNamespace, podInfo)
	if err != nil {
		return nil, err
	}

	return podInfo, err
}

func getRouterPortMacAddress(coreclient *corev1client.CoreV1Client, restconfig *rest.Config, podInfo *PodInfo, ovnNamespace, portPrefix string) (string, error) {
	tspCmd := "ovn-sbctl --no-leader-only " + podInfo.SbCommand + " --bare --no-heading --column=mac list Port_Binding " + portPrefix + podInfo.NodeName
	ipOutput, ipError, err := execInPod(coreclient, restconfig, ovnNamespace, podInfo.OvnKubePodName, podInfo.OvnKubeContainerName, tspCmd, "")
	if err != nil {
		return "", fmt.Errorf("execInPod() failed. err: %s, stderr: %s, stdout: %s, podInfo: %v", err, ipError, ipOutput, podInfo)
	}
	// The ipOutput is with the following format: 0a:58:a8:fe:00:03 100.88.0.3/16 or
	// 0a:58:0a:f4:02:01 10.244.2.1/24 fd00:10:244:3::1/64 for dual stack cluster.
	// Parse the mac address from it.
	macIP := strings.Split(strings.Replace(ipOutput, "\n", "", -1), " ")
	if len(macIP) < 1 {
		return "", fmt.Errorf("invalid mac ip output %s", ipOutput)
	}
	return macIP[0], nil
}

// getNodeExternalBridgeName gets the name of the external bridge of this node, e.g. breth0 or br-ex.
func getNodeExternalBridgeName(coreclient *corev1client.CoreV1Client, restconfig *rest.Config, ovnNamespace string, podInfo *PodInfo) (string, error) {
	cmd := "ovn-sbctl --no-leader-only " + podInfo.SbCommand + " --bare --no-heading --column=logical_port find Port_Binding options:network_name=" + types.PhysicalNetworkName
	stdout, stderr, err := execInPod(coreclient, restconfig, ovnNamespace, podInfo.OvnKubePodName, podInfo.OvnKubeContainerName, cmd, "")
	if err != nil {
		return "", fmt.Errorf("execInPod() failed with %s stderr %s stdout %s", err, stderr, stdout)
	}
	scanner := bufio.NewScanner(strings.NewReader(stdout))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, "_"+podInfo.NodeName) {
			splitLine := strings.Split(scanner.Text(), "_")
			if len(splitLine) == 2 {
				return splitLine[0], nil
			}
		}
	}
	return "", fmt.Errorf("could not find external bridge for node %s in getNodeBridgeName()", podInfo.NodeName)
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

// Get the OVN Database URIs from the first container found in any pod in the ovn-kubernetes namespace with name "ovnkube-node"
// Returns nbAddress, sbAddress, protocol == "ssl", nil
func getDatabaseURIs(coreclient *corev1client.CoreV1Client, restconfig *rest.Config, ovnNamespace string, podInfo *PodInfo) (*PodInfo, error) {
	podName := podInfo.OvnKubePodName
	var ovnContainerName string
	pod, err := coreclient.Pods(ovnNamespace).Get(context.TODO(), podName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	for _, container := range pod.Spec.Containers {
		if slices.Contains(ovnKubeNodePodContainers, container.Name) {
			ovnContainerName = container.Name
			break
		}
	}
	if ovnContainerName == "" {
		klog.Errorf("Cannot find ovnkube pods with any of containers %v", ovnKubeNodePodContainers)
		return nil, fmt.Errorf("cannot find ovnkube pods with containers: %v", ovnKubeNodePodContainers)
	}
	klog.V(5).Infof("Found pod '%s' with container '%s'", podName, ovnContainerName)
	podInfo.OvnKubeContainerName = ovnContainerName

	psCmd := "ps -eo args | grep '/usr/bin/[o]vnkube'"
	hostOutput, hostError, err := execInPod(coreclient, restconfig, ovnNamespace, podName, ovnContainerName, psCmd, "")
	if err != nil {
		klog.V(5).Infof("execInPod('%s') failed with err: '%s', stderr: '%s', stdout: '%s', Pod Name '%s' \n", psCmd, err, hostError, hostOutput, podName)
		return nil, err
	}
	podInfo.IsInterConnect = len(regexp.MustCompile("--enable-interconnect").FindString(hostOutput)) > 0
	if podInfo.IsInterConnect {
		// When interconnect is enabled, then retrieve its zone name from psCmd output.
		// The psCmd output contains zone string like below:
		// ... --enable-interconnect --zone ovn-worker2 ...
		re := regexp.MustCompile(`--zone(=| )[^\s]+`)
		res := re.FindString(hostOutput)
		if len(res) > 6 {
			podInfo.InterConnectZoneName = strings.TrimSpace(res[6:])
		}
	}
	re := regexp.MustCompile(`--nb-address(=| )[^\s]+`)
	nbAddress := re.FindString(hostOutput)
	if len(nbAddress) > 13 {
		nbAddress = strings.Replace(
			re.FindString(hostOutput)[13:],
			"://",
			":",
			-1)
	} else {
		nbAddress = nbdbServerSock
	}
	re = regexp.MustCompile(`--sb-address(=| )[^\s]+`)
	sbAddress := re.FindString(hostOutput)
	if len(sbAddress) > 13 {
		sbAddress = strings.Replace(
			re.FindString(hostOutput)[13:],
			"://",
			":",
			-1)
	} else {
		sbAddress = sbdbServerSock
	}
	re = regexp.MustCompile(`(ssl|tcp|unix)`)
	protocol := re.FindString(nbAddress)

	klog.V(5).Infof("Nb address for OVN database communication is %s", nbAddress)
	klog.V(5).Infof("Sb address for OVN database communication is %s", sbAddress)
	klog.V(5).Infof("Protocol for OVN database communication is %s", protocol)
	if podInfo.IsInterConnect {
		klog.V(5).Infof("The pod %s's interconnect zone name is %s", podInfo.OvnKubePodName,
			podInfo.InterConnectZoneName)
	}
	podInfo.NbURI = nbAddress
	podInfo.SbURI = sbAddress
	if protocol == "ssl" {
		podInfo.SslCertKeys = "-p /ovn-cert/tls.key -c /ovn-cert/tls.crt -C /ovn-ca/ca-bundle.crt "
	} else {
		podInfo.SslCertKeys = " "
	}
	podInfo.NbCommand = podInfo.SslCertKeys + "--db " + podInfo.NbURI
	klog.V(5).Infof("The nbcmd of pod %s is %s", podInfo.OvnKubePodName, podInfo.NbCommand)
	podInfo.SbCommand = podInfo.SslCertKeys + "--db " + podInfo.SbURI
	klog.V(5).Infof("The sbcmd of pod %s is %s", podInfo.OvnKubePodName, podInfo.SbCommand)

	return podInfo, nil
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
func runOvnTraceToService(coreclient *corev1client.CoreV1Client, restconfig *rest.Config, srcPodInfo *PodInfo, dstSvcInfo *SvcInfo, ovnNamespace, protocol, dstPort string) {
	var inport string
	inport = srcPodInfo.FullyQualifiedPodName()
	if srcPodInfo.HostNetwork {
		inport = srcPodInfo.K8sNodeNamePort
	}
	svcL3Ver := dstSvcInfo.getL3Ver()
	if srcPodInfo.IPVer != svcL3Ver {
		klog.Exitf("Pod src IP address family (address: %s) and service IP address family (address: %s) do not match",
			srcPodInfo.IP, dstSvcInfo.ClusterIP)
	}
	cmd := fmt.Sprintf(`ovn-trace --no-leader-only %[1]s %[2]s --ct=new `+
		`'inport=="%[3]s" && eth.src==%[4]s && eth.dst==%[5]s && %[6]s.src==%[7]s && %[8]s.dst==%[9]s && ip.ttl==64 && %[10]s.dst==%[11]s && %[10]s.src==52888' --lb-dst %[12]s:%[13]s`,
		srcPodInfo.SbCommand,  // 1
		srcPodInfo.NodeName,   // 2
		inport,                // 3
		srcPodInfo.MAC,        // 4
		srcPodInfo.RtosMAC,    // 5
		srcPodInfo.IPVer,      // 6
		srcPodInfo.IP,         // 7
		svcL3Ver,              // 8
		dstSvcInfo.ClusterIP,  // 9
		protocol,              // 10
		dstPort,               // 11
		dstSvcInfo.PodInfo.IP, // 12
		dstSvcInfo.PodPort,    // 13
	)
	klog.V(4).Infof("ovn-trace command from src to service clusterIP is %s", cmd)

	ovnSrcDstOut, ovnSrcDstErr, err := execInPod(coreclient, restconfig, ovnNamespace, srcPodInfo.OvnKubePodName, srcPodInfo.OvnKubeContainerName, cmd, "")
	var successString string
	if !srcPodInfo.IsInterConnect || podsInSameInterconnectZone(srcPodInfo, dstSvcInfo.PodInfo) {
		successString = fmt.Sprintf(`output to "%s"`, dstSvcInfo.FullyQualifiedPodName())
	} else {
		successString = fmt.Sprintf(`output to "tstor-%s"`, dstSvcInfo.PodInfo.NodeName)
	}
	direction := "source pod to service clusterIP"
	printSuccessOrFailure("ovn-trace "+direction, srcPodInfo.PodName, dstSvcInfo.SvcName, ovnSrcDstOut, ovnSrcDstErr, err, successString)
	runOvnTraceToRemotePod(coreclient, restconfig, direction, srcPodInfo, dstSvcInfo.PodInfo, ovnNamespace, protocol, dstPort)

}

// runOvnTraceToIP runs an ovntrace from src pod to dst IP address (should be external to the cluster).
// Returns the node that the trace will exit on.
func runOvnTraceToIP(coreclient *corev1client.CoreV1Client, restconfig *rest.Config, srcPodInfo *PodInfo, parsedDstIP net.IP, ovnNamespace, protocol, dstPort string) (string, string) {
	if srcPodInfo.HostNetwork {
		klog.Exitf("Pod cannot be on Host Network when tracing to an IP address; use ping\n")
	}

	l3ver := getIPVer(parsedDstIP)

	if srcPodInfo.IPVer != l3ver {
		klog.Exitf("Pod src IP address family (address: %s) and destination IP address family (address: %s) do not match",
			srcPodInfo.IP, parsedDstIP)
	}

	cmd := fmt.Sprintf(`ovn-trace --no-leader-only %[1]s %[2]s `+
		`'inport=="%[3]s" && eth.src==%[4]s && eth.dst==%[5]s && %[6]s.src==%[7]s && %[8]s.dst==%[9]s && ip.ttl==64 && %[10]s.dst==%[11]s && %[10]s.src==52888'`,
		srcPodInfo.SbCommand,               // 1
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
	// c) when interconnect enabled and egressip available for the pod, then go out of tstor-<egress-node> with type "remote".
	successString := fmt.Sprintf(`output to "(.*)_(.*)", type "localnet"|output to "k8s-%s"|remote`, srcPodInfo.NodeName)
	// Run the command and check if succesString was found.
	ovnSrcDstOut, ovnSrcDstErr, err := execInPod(coreclient, restconfig, ovnNamespace, srcPodInfo.OvnKubePodName, srcPodInfo.OvnKubeContainerName, cmd, "")
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

	// Try to find egress node name when ovnSrcDstOut contains "output to tstor-<egress-node>"".
	nodeNameRegex := `output to "tstor-(.*)",`
	re = regexp.MustCompile(nodeNameRegex)
	subMatches = re.FindSubmatch([]byte(ovnSrcDstOut))
	if len(subMatches) > 1 {
		node := subMatches[len(subMatches)-1]
		klog.V(1).Infof("%sout on node %s%s\n", green, node, reset)
		return string(node), ""
	}

	klog.V(5).Infof("Could not find SNAT for this trace command, this must be routingViaHost gateway mode without EgressIP.")
	nodeNameRegex = `output to "k8s-(.*)",`
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
func runOvnTraceToPod(coreclient *corev1client.CoreV1Client, restconfig *rest.Config, direction string, srcPodInfo, dstPodInfo *PodInfo, ovnNamespace, protocol, dstPort string) {
	var inport string
	inport = srcPodInfo.FullyQualifiedPodName()
	if srcPodInfo.HostNetwork {
		inport = srcPodInfo.K8sNodeNamePort
	}
	cmd := fmt.Sprintf(`ovn-trace --no-leader-only %[1]s %[2]s `+
		`'inport=="%[3]s" && eth.src==%[4]s && eth.dst==%[5]s && %[6]s.src==%[7]s && %[8]s.dst==%[9]s && ip.ttl==64 && %[10]s.dst==%[11]s && %[10]s.src==52888'`,
		srcPodInfo.SbCommand, // 1
		srcPodInfo.NodeName,  // 2
		inport,               // 3
		srcPodInfo.MAC,       // 4
		srcPodInfo.RtosMAC,   // 5
		srcPodInfo.IPVer,     // 6
		srcPodInfo.IP,        // 7
		dstPodInfo.IPVer,     // 8
		dstPodInfo.IP,        // 9
		protocol,             // 10
		dstPort,              // 11
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
	} else if !srcPodInfo.IsInterConnect || podsInSameInterconnectZone(srcPodInfo, dstPodInfo) {
		successString = fmt.Sprintf(`output to "%s"`, dstPodInfo.FullyQualifiedPodName())
	} else {
		successString = fmt.Sprintf(`output to "tstor-%s"`, dstPodInfo.NodeName)
	}
	ovnSrcDstOut, ovnSrcDstErr, err := execInPod(coreclient, restconfig, ovnNamespace, srcPodInfo.OvnKubePodName, srcPodInfo.OvnKubeContainerName, cmd, "")
	printSuccessOrFailure("ovn-trace "+direction, srcPodInfo.PodName, dstPodInfo.PodName, ovnSrcDstOut, ovnSrcDstErr, err, successString)
	runOvnTraceToRemotePod(coreclient, restconfig, direction, srcPodInfo, dstPodInfo, ovnNamespace, protocol, dstPort)
}

func runOvnTraceToRemotePod(coreclient *corev1client.CoreV1Client, restconfig *rest.Config, direction string, srcPodInfo, dstPodInfo *PodInfo, ovnNamespace, protocol, dstPort string) {
	if dstPodInfo.HostNetwork || !srcPodInfo.IsInterConnect || podsInSameInterconnectZone(srcPodInfo, dstPodInfo) {
		return
	}
	cmd := fmt.Sprintf(`ovn-trace --no-leader-only %[1]s `+
		`'inport=="%[2]s" && eth.src==%[3]s && eth.dst==%[4]s && %[5]s.src==%[6]s && %[7]s.dst==%[8]s && ip.ttl==64 && %[9]s.dst==%[10]s && %[9]s.src==52888'`,
		dstPodInfo.SbCommand, // 1
		types.TransitSwitchToRouterPrefix+srcPodInfo.NodeName, // 2
		srcPodInfo.MAC,      // 3
		dstPodInfo.RtotsMAC, // 4
		srcPodInfo.IPVer,    // 5
		srcPodInfo.IP,       // 6
		dstPodInfo.IPVer,    // 7
		dstPodInfo.IP,       // 8
		protocol,            // 9
		dstPort,             // 10
	)
	klog.V(4).Infof("ovn-trace command on destination pod node is %s", cmd)
	successString := fmt.Sprintf(`output to "%s"`, dstPodInfo.FullyQualifiedPodName())
	ovnSrcDstOut, ovnSrcDstErr, err := execInPod(coreclient, restconfig, ovnNamespace, dstPodInfo.OvnKubePodName, srcPodInfo.OvnKubeContainerName, cmd, "")
	printSuccessOrFailure("ovn-trace (remote) "+direction, srcPodInfo.PodName, dstPodInfo.PodName, ovnSrcDstOut, ovnSrcDstErr, err, successString)
}

func podsInSameInterconnectZone(srcPodInfo, dstPodInfo *PodInfo) bool {
	return srcPodInfo.IsInterConnect && dstPodInfo.IsInterConnect &&
		srcPodInfo.InterConnectZoneName == dstPodInfo.InterConnectZoneName
}

// runOfprotoTraceToPod runs an ofproto/trace command from the src to the destination pod.
func runOfprotoTraceToPod(coreclient *corev1client.CoreV1Client, restconfig *rest.Config, direction string, srcPodInfo, dstPodInfo *PodInfo, ovnNamespace, protocol, dstPort string) string {
	protocolSelector, nwSrc, nwDst := getOfprotoIPFamilyArgs(protocol, net.ParseIP(dstPodInfo.IP))
	cmd := fmt.Sprintf(`ovs-appctl ofproto/trace br-int `+
		`"in_port=%[1]s, %[9]s, dl_src=%[3]s, dl_dst=%[4]s, %[10]s=%[5]s, %[11]s=%[6]s, nw_ttl=64, %[7]s_dst=%[8]s, %[7]s_src=12345"`,
		srcPodInfo.VethName, // 1
		protocol,            // 2
		srcPodInfo.MAC,      // 3
		srcPodInfo.RtosMAC,  // 4
		srcPodInfo.IP,       // 5
		dstPodInfo.IP,       // 6
		protocol,            // 7
		dstPort,             // 8
		protocolSelector,    // 9
		nwSrc,               // 10
		nwDst,               // 11
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
	appSrcDstOut, appSrcDstErr, err := execInPod(coreclient, restconfig, ovnNamespace, srcPodInfo.OvnKubePodName, srcPodInfo.OvnKubeContainerName, cmd, "")
	printSuccessOrFailure("ovs-appctl ofproto/trace "+direction, srcPodInfo.PodName, dstPodInfo.PodName, appSrcDstOut, appSrcDstErr, err, successString)

	return appSrcDstOut
}

// runOfprotoTraceToIP runs an ofproto/trace command from the src to the destination pod.
// egressNodeName is the exit node, as determined by an ovn-trace command that was run earlier.
// egressBridgeName is the name of the exit bridge (for EgressIPs, EgressGW and also for routingViaOVN mode).
// If egressBridgeName == "", then this is routingViaHost Gateway mode without an EgressIP / EgressGW.
func runOfprotoTraceToIP(coreclient *corev1client.CoreV1Client, restconfig *rest.Config, srcPodInfo *PodInfo, dstIP net.IP, ovnNamespace, protocol, dstPort, egressNodeName, egressBridgeName string) string {
	protocolSelector, nwSrc, nwDst := getOfprotoIPFamilyArgs(protocol, dstIP)
	cmd := fmt.Sprintf(`ovs-appctl ofproto/trace br-int `+
		`"in_port=%[1]s, %[8]s, dl_src=%[3]s, dl_dst=%[4]s, %[9]s=%[5]s, %[10]s=%[6]s, nw_ttl=64, %[2]s_dst=%[7]s, %[2]s_src=12345"`,
		srcPodInfo.VethName, // 1
		protocol,            // 2
		srcPodInfo.MAC,      // 3
		srcPodInfo.RtosMAC,  // 4
		srcPodInfo.IP,       // 5
		dstIP.String(),      // 6
		dstPort,             // 7
		protocolSelector,    // 8
		nwSrc,               // 9
		nwDst,               // 10
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
	appSrcDstOut, appSrcDstErr, err := execInPod(coreclient, restconfig, ovnNamespace, srcPodInfo.OvnKubePodName, srcPodInfo.OvnKubeContainerName, cmd, "")
	printSuccessOrFailure(fmt.Sprintf("ovs-appctl ofproto/trace %s", direction), srcPodInfo.PodName, dstIP.String(), appSrcDstOut, appSrcDstErr, err, successString)

	return appSrcDstOut
}

// getOfprotoIPFamilyArgs generates the protocol parameter name and the src and dst parameter names.
// We must do this as syntax for ofproto/trace with IPv6 is slightly different.
func getOfprotoIPFamilyArgs(protocol string, ip net.IP) (string, string, string) {
	protocolSelector := protocol
	nwSrc := "nw_src"
	nwDst := "nw_dst"
	if ip.To4() == nil {
		protocolSelector += "6"
		nwSrc = "ipv6_src"
		nwDst = "ipv6_dst"
	}
	return protocolSelector, nwSrc, nwDst
}

// installOvnDetraceDependencies installs dependencies for ovn-detrace with pip3 in case they are missing (for older images).
// Returns error if dependencies are missing but cannot be installed.
func installOvnDetraceDependencies(coreclient *corev1client.CoreV1Client, restconfig *rest.Config, podInfo *PodInfo, ovnNamespace string) error {
	dependencies := map[string]string{
		"ovs":       "if type -p ovn-detrace >/dev/null 2>&1; then echo 'true' ; fi",
		"pyOpenSSL": "if python -c 'import ssl; print(ssl.OPENSSL_VERSION)' > /dev/null; then echo 'true'; fi",
	}
	for dependency, dependencyCmd := range dependencies {
		verifyOut, _, err := verifyDependency(coreclient, restconfig, podInfo, ovnNamespace, dependency, dependencyCmd)
		if err != nil {
			return err
		}
		if verifyOut != "true" {
			verifyOut, verifyErr, err := verifyDependency(coreclient, restconfig, podInfo, ovnNamespace, "pip3", "if type -p pip3 >/dev/null 2>&1; then echo 'true' ; fi")
			if err != nil {
				return err
			}
			if verifyOut != "true" {
				return fmt.Errorf("ovn-detrace error while verifying dependency pip3 in pod %s, container %s. stdOut: '%s'\n stdErr: %s", podInfo.OvnKubePodName,
					podInfo.OvnKubeContainerName, verifyOut, verifyErr)
			}
			installCmd := "pip3 install " + dependency
			depInstallOut, depInstallErr, err := execInPod(coreclient, restconfig, ovnNamespace, podInfo.OvnKubePodName, podInfo.OvnKubeContainerName, installCmd, "")
			if err != nil {
				return fmt.Errorf("ovn-detrace error while installing dependency %s in pod %s, container %s. Error '%v', stdOut: '%s'\n stdErr: %s",
					dependency, podInfo.OvnKubePodName, podInfo.OvnKubeContainerName, err, depInstallOut, depInstallErr)

			}
			klog.V(1).Infof("Install ovn-detrace dependencies output: %s\n", depInstallOut)
		}
	}
	return nil
}

func verifyDependency(coreclient *corev1client.CoreV1Client, restconfig *rest.Config, podInfo *PodInfo, ovnNamespace, dependency, depCheckCommand string) (string, string, error) {
	depVerifyOut, depVerifyErr, err := execInPod(coreclient, restconfig, ovnNamespace, podInfo.OvnKubePodName, podInfo.OvnKubeContainerName, depCheckCommand, "")
	if err != nil {
		return "", "", fmt.Errorf("ovn-detrace error while verifying dependency %s in pod %s, container %s. Error '%v', stdOut: '%s'\n stdErr: %s",
			dependency, podInfo.OvnKubePodName, podInfo.OvnKubeContainerName, err, depVerifyOut, depVerifyErr)
	}
	trueFalse := strings.TrimSuffix(depVerifyOut, "\n")
	klog.V(10).Infof("Dependency %s check '%s' in pod '%s', container '%s' yielded '%s'", dependency, depCheckCommand, podInfo.OvnKubePodName, podInfo.OvnKubeContainerName, trueFalse)
	return trueFalse, depVerifyErr, nil
}

// runOvnDetrace runs an ovn-detrace command for the given input.
// Returns error if dependencies are not met (allows for graceful handling of those issues).
func runOvnDetrace(coreclient *corev1client.CoreV1Client, restconfig *rest.Config, direction string, srcPodInfo *PodInfo,
	dstName string, appSrcDstOut, ovnNamespace string) error {
	// If NBDB connectivity is not available do not run ovn-detrace.
	if _, stdErr, err := execInPod(coreclient, restconfig, ovnNamespace, srcPodInfo.OvnKubePodName, srcPodInfo.OvnKubeContainerName, fmt.Sprintf("ovn-nbctl %s get-connection", srcPodInfo.NbCommand), ""); err != nil {
		return fmt.Errorf("nbdb is not available %q", stdErr)
	}
	// If dependencies aren't satisfied do not run ovn-detrace.
	if err := installOvnDetraceDependencies(coreclient, restconfig, srcPodInfo, ovnNamespace); err != nil {
		return fmt.Errorf("dependencies check failed: %q", err)
	}

	cmd := fmt.Sprintf(`ovn-detrace --ovnnb=%[1]s --ovnsb=%[2]s %[3]s --ovsdb=unix:/var/run/openvswitch/db.sock`,
		srcPodInfo.NbURI,       // 1
		srcPodInfo.SbURI,       // 2
		srcPodInfo.SslCertKeys, // 3
	)
	klog.V(4).Infof("ovn-detrace command from %s is %s", direction, cmd)

	dtraceSrcDstOut, dtraceSrcDstErr, err := execInPod(coreclient, restconfig, ovnNamespace, srcPodInfo.OvnKubePodName, srcPodInfo.OvnKubeContainerName, cmd, appSrcDstOut)
	printSuccessOrFailure("ovn-detrace "+direction, srcPodInfo.PodName, dstName, dtraceSrcDstOut, dtraceSrcDstErr, err, "")

	return nil
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
				addrStr := utilnet.ParseIPSloppy(s.Address).String()
				klog.V(5).Infof("  Address Type: %s - Address: %s", s.Type, addrStr)
				//if s.Type == corev1client.NodeInternalIP {
				if s.Type == "InternalIP" {
					masters[node.Name] = addrStr
				}
			}
		} else {
			klog.V(5).Infof("  Name: %s is a worker", node.Name)
			for _, s := range node.Status.Addresses {
				addrStr := utilnet.ParseIPSloppy(s.Address).String()
				klog.V(5).Infof("  Address Type: %s - Address: %s", s.Type, addrStr)
				//if s.Type == corev1client.NodeInternalIP {
				if s.Type == "InternalIP" {
					workers[node.Name] = addrStr
				}
			}
		}
	}

	if len(masters) < 3 {
		klog.V(5).Infof("Cluster does not have 3 masters, found %d", len(masters))
	}
}

func getDesiredPodIP(pod *kapi.Pod, addressFamily string) (string, error) {
	for _, podIP := range pod.Status.PodIPs {
		ip := utilnet.ParseIPSloppy(podIP.IP)
		if getIPVer(ip) == addressFamily {
			return ip.String(), nil
		}
	}
	return "", fmt.Errorf("could not find desired pod ip address for the given address family")
}

func getIPVer(ip net.IP) string {
	if ip.To4() != nil {
		return ip4
	}
	return ip6
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
	addressFamily := flag.String("addr-family", ip4, "Address family (ip4 or ip6) to be used for tracing")
	skipOvnDetrace := flag.Bool("skip-detrace", false, "skip ovn-detrace command")
	dumpVRFTableIDs := flag.Bool("dump-udn-vrf-table-ids", false, "Dump the VRF table ID per node for all the user defined networks")
	loglevel := flag.String("loglevel", "0", "loglevel: klog level")
	flag.Parse()

	// Set the application's log level.
	setLogLevel(*loglevel)

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

	if *dumpVRFTableIDs {
		nodesVRFTableIDs, err := findUserDefinedNetworkVRFTableIDs(coreclient, restconfig, ovnNamespace)
		if err != nil {
			klog.Exitf("Failed dumping VRF table IDs: %s", err)
		}
		fmt.Println(string(nodesVRFTableIDs))
		return
	}

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

	// Show some information about the nodes in this cluster - only if log level 5 or higher.
	if lvl, err := strconv.Atoi(*loglevel); err == nil && lvl >= 5 {
		displayNodeInfo(coreclient)
	}

	// Get info needed for the src Pod
	srcPodInfo, err := getPodInfo(coreclient, restconfig, *srcPodName, ovnNamespace, *srcNamespace, *addressFamily)
	if err != nil {
		klog.Exitf("Failed to get information from pod %s: %v", *srcPodName, err)
	}
	klog.V(5).Infof("srcPodInfo is %s\n", srcPodInfo)

	// 1) Either run a trace from source pod to destination IP and return ...
	if parsedDstIP != nil {
		klog.V(5).Infof("Running a trace to an IP address")
		egressNodeName, egressBridgeName := runOvnTraceToIP(coreclient, restconfig, srcPodInfo, parsedDstIP, ovnNamespace, protocol, *dstPort)
		appSrcDstOut := runOfprotoTraceToIP(coreclient, restconfig, srcPodInfo, parsedDstIP, ovnNamespace, protocol, *dstPort, egressNodeName, egressBridgeName)
		if *skipOvnDetrace {
			return
		}
		err = runOvnDetrace(coreclient, restconfig, "pod to external IP", srcPodInfo, parsedDstIP.String(), appSrcDstOut, ovnNamespace)
		if err != nil {
			klog.Infof("Skipped ovn-detrace due to: %q", err)
		}
		return
	}

	// 2) ... or run a trace to destination service / destination pod.
	// Get destination service information if a destination service name was provided.
	klog.V(5).Infof("Running a trace to a cluster local svc or to another pod")
	var dstSvcInfo *SvcInfo
	if *dstSvcName != "" {
		// Get dst service
		dstSvcInfo, err = getSvcInfo(coreclient, restconfig, *dstSvcName, ovnNamespace, *dstNamespace, *addressFamily)
		if err != nil {
			klog.Exitf("Failed to get information from service %s: %v", *dstSvcName, err)
		}
		klog.V(5).Infof("dstSvcInfo is %s\n", dstSvcInfo)
		// Set dst pod name, we'll use this to run through pod-pod tests as if use supplied this pod
		*dstPodName = dstSvcInfo.PodInfo.PodName
		klog.V(1).Infof("Using pod %s in service %s to test against", dstSvcInfo.PodInfo.PodName, *dstSvcName)
	}

	// Now get info needed for the dst Pod
	dstPodInfo, err := getPodInfo(coreclient, restconfig, *dstPodName, ovnNamespace, *dstNamespace, *addressFamily)
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
		runOvnTraceToService(coreclient, restconfig, srcPodInfo, dstSvcInfo, ovnNamespace, protocol, *dstPort)
	}
	runOvnTraceToPod(coreclient, restconfig, "source pod to destination pod", srcPodInfo, dstPodInfo, ovnNamespace, protocol, *dstPort)
	runOvnTraceToPod(coreclient, restconfig, "destination pod to source pod", dstPodInfo, srcPodInfo, ovnNamespace, protocol, *dstPort)

	// ovs-appctl ofproto/trace commands
	appSrcDstOut := runOfprotoTraceToPod(coreclient, restconfig, "source pod to destination pod", srcPodInfo, dstPodInfo, ovnNamespace, protocol, *dstPort)
	appDstSrcOut := runOfprotoTraceToPod(coreclient, restconfig, "destination pod to source pod", dstPodInfo, srcPodInfo, ovnNamespace, protocol, *dstPort)

	// ovn-detrace commands below
	if *skipOvnDetrace {
		return
	}
	err = runOvnDetrace(coreclient, restconfig, "source pod to destination pod", srcPodInfo, dstPodInfo.PodName, appSrcDstOut, ovnNamespace)
	if err != nil {
		klog.Infof("Skipped ovn-detrace due to: %q", err)
		return
	}
	err = runOvnDetrace(coreclient, restconfig, "destination pod to source pod", dstPodInfo, srcPodInfo.PodName, appDstSrcOut, ovnNamespace)
	if err != nil {
		klog.Infof("Skipped ovn-detrace due to: %q", err)
		return
	}
}
