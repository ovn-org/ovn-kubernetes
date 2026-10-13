// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"

	nadclient "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/typed/k8s.cni.cncf.io/v1"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kapitypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/rand"
	clientset "k8s.io/client-go/kubernetes"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	"k8s.io/utils/ptr"

	mnpapi "github.com/k8snetworkplumbingwg/multi-networkpolicy/pkg/apis/k8s.cni.cncf.io/v1beta1"
	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	utilnet "k8s.io/utils/net"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/generator/ip"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/allocators"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig/api"
	deploymentconfigapi "github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig/api"
)

func netCIDR(netCIDR string, netPrefixLengthPerNode int) string {
	return fmt.Sprintf("%s/%d", netCIDR, netPrefixLengthPerNode)
}

func joinStrings(vals ...string) string {
	return strings.Join(vals, ",")
}

func primaryLayer3MultiCIDRs() string {
	if deploymentconfig.Get().IsConfigurationEnabled(deploymentconfigapi.L3UDNMultiSubnetConfig) {
		return joinStrings(primaryLayer3MultiIPv4CIDRs(), primaryLayer3MultiIPv6CIDRs())
	}
	// Backward compatible: single subnet per IP family
	return joinStrings(primaryLayer3SingleIPv4CIDR(), primaryLayer3SingleIPv6CIDR())
}

// primaryLayer3IPv4CIDRs returns two IPv4 CIDRs for a primary network in layer3
// topology. The first CIDR is resized to a /23 to force an overflow to the
// second CIDR for a third and subsequent nodes hostSubnet.
func primaryLayer3IPv4CIDRs() (string, string) {
	subnets4, _ := allocators.GetNthFirstUDNSubnets(2)
	subnets4[0] = firstSubnetOf(subnets4[0], 23) + "/24"
	subnets4[1] = subnets4[1] + "/24"
	return subnets4[0], subnets4[1]
}

// primaryLayer3IPv6CIDRs returns two IPv6 CIDRs for a primary network in layer3
// topology. The first CIDR is resized to a /63 to force an overflow to the
// second CIDR for a third and subsequent nodes hostSubnet.
func primaryLayer3IPv6CIDRs() (string, string) {
	_, subnets6 := allocators.GetNthFirstUDNSubnets(2)
	subnets6[0] = firstSubnetOf(subnets6[0], 63) + "/64"
	subnets6[1] = subnets6[1] + "/64"
	return subnets6[0], subnets6[1]
}

// primaryLayer3SingleIPv4CIDR returns a single IPv4 CIDR for backward compatibility
// with CRDs that don't support multiple subnets per Layer3 topology.
func primaryLayer3SingleIPv4CIDR() string {
	subnets4, _ := allocators.GetNthFirstUDNSubnets(1)
	return subnets4[0] + "/24"
}

// primaryLayer3SingleIPv6CIDR returns a single IPv6 CIDR for backward compatibility
// with CRDs that don't support multiple subnets per Layer3 topology.
func primaryLayer3SingleIPv6CIDR() string {
	_, subnets6 := allocators.GetNthFirstUDNSubnets(1)
	return subnets6[0] + "/64"
}

func primaryLayer3MultiIPv4CIDRs() string {
	if deploymentconfig.Get().IsConfigurationEnabled(api.L3UDNMultiSubnetConfig) {
		subnet1, subnet2 := primaryLayer3IPv4CIDRs()
		return joinStrings(subnet1, subnet2)
	}
	return primaryLayer3SingleIPv4CIDR()
}

func primaryLayer3MultiIPv6CIDRs() string {
	if deploymentconfig.Get().IsConfigurationEnabled(api.L3UDNMultiSubnetConfig) {
		subnet1, subnet2 := primaryLayer3IPv6CIDRs()
		return joinStrings(subnet1, subnet2)
	}
	return primaryLayer3SingleIPv6CIDR()
}

func filterCIDRsAndJoin(cs clientset.Interface, cidrs string) string {
	if cidrs == "" {
		return "" // we may not always set CIDR - i.e. CDN
	}
	return joinStrings(filterCIDRs(cs, strings.Split(cidrs, ",")...)...)
}

func filterCIDRs(cs clientset.Interface, cidrs ...string) []string {
	var supportedCIDRs []string
	for _, cidr := range cidrs {
		if strings.TrimSpace(cidr) == "" {
			continue
		}
		if !isCIDRIPFamilySupported(cs, cidr) {
			continue
		}
		supportedCIDRs = append(supportedCIDRs, cidr)
	}
	return supportedCIDRs
}

func filterSupportedNetworkConfig(client clientset.Interface, config *networkAttachmentConfigParams) {
	config.cidr = filterCIDRsAndJoin(client, config.cidr)
	config.excludeCIDRs = filterCIDRs(client, config.excludeCIDRs...)
	config.reservedCIDRs = filterCIDRsAndJoin(client, config.reservedCIDRs)
	config.infrastructureCIDRs = filterCIDRsAndJoin(client, config.infrastructureCIDRs)
	config.defaultGatewayIPs = filterIPsAndJoin(client, config.defaultGatewayIPs)
}

func filterIPs(cs clientset.Interface, ips ...string) []string {
	var supportedIPs []string
	for _, ip := range ips {
		isIPv6 := utilnet.IsIPv6String(ip)
		if (!isIPv6 && isIPv4Supported(cs)) || (isIPv6 && isIPv6Supported(cs)) {
			supportedIPs = append(supportedIPs, ip)
		}
	}
	return supportedIPs
}

func filterIPsAndJoin(cs clientset.Interface, ips string) string {
	return joinStrings(filterIPs(cs, strings.Split(ips, ",")...)...)
}

func getNetCIDRSubnet(netCIDR string) (string, error) {
	subStrings := strings.Split(netCIDR, "/")
	if len(subStrings) == 3 {
		return subStrings[0] + "/" + subStrings[1], nil
	} else if len(subStrings) == 2 {
		return netCIDR, nil
	}
	return "", fmt.Errorf("invalid network cidr: %q", netCIDR)
}

// containingNetworkSubnet returns the configured UDN network subnet that contains
// the given node subnet. clusterCIDRsv4/v6 are maps of configured network
// subnets keyed by their CIDR string (for example "10.0.0.0/23").
func containingNetworkSubnet(nodeSubnet *net.IPNet, clusterCIDRsv4, clusterCIDRsv6 map[string]struct{}) (string, error) {
	candidates := clusterCIDRsv4
	if utilnet.IsIPv6CIDR(nodeSubnet) {
		candidates = clusterCIDRsv6
	}
	for cidr := range candidates {
		_, networkSubnet, err := net.ParseCIDR(cidr)
		if err != nil {
			return "", fmt.Errorf("invalid configured network subnet %q: %w", cidr, err)
		}
		if networkSubnet.Contains(nodeSubnet.IP) {
			return cidr, nil
		}
	}
	return "", fmt.Errorf("node subnet %s is not contained in any configured network subnet", nodeSubnet)
}

type networkAttachmentConfigParams struct {
	cidr                string
	excludeCIDRs        []string
	infrastructureCIDRs string
	defaultGatewayIPs   string
	reservedCIDRs       string
	namespace           string
	name                string
	topology            string
	networkName         string
	vlanID              int
	allowPersistentIPs  bool
	role                string
	mtu                 int
	physicalNetworkName string
	disableMACSecurity  bool
}

type networkAttachmentConfig struct {
	networkAttachmentConfigParams
}

func newNetworkAttachmentConfig(params networkAttachmentConfigParams) networkAttachmentConfig {
	networkAttachmentConfig := networkAttachmentConfig{
		networkAttachmentConfigParams: params,
	}
	if networkAttachmentConfig.networkName == "" {
		networkAttachmentConfig.networkName = uniqueNadName(networkAttachmentConfig.name)
	}
	return networkAttachmentConfig
}

func uniqueNadName(originalNetName string) string {
	const randomStringLength = 5
	return fmt.Sprintf("%s_%s", rand.String(randomStringLength), originalNetName)
}

func generateNADSpec(config networkAttachmentConfig) string {
	if config.mtu == 0 {
		config.mtu = 1300
	}
	return fmt.Sprintf(
		`
{
        "cniVersion": "0.3.0",
        "name": %q,
        "type": "ovn-k8s-cni-overlay",
        "topology":%q,
        "subnets": %q,
        "excludeSubnets": %q,
        "reservedSubnets": %q,
        "infrastructureSubnets": %q,
		"defaultGatewayIPs": %q,
        "mtu": %d,
        "netAttachDefName": %q,
        "vlanID": %d,
        "allowPersistentIPs": %t,
        "physicalNetworkName": %q,
        "role": %q
}
`,
		config.networkName,
		config.topology,
		config.cidr,
		strings.Join(config.excludeCIDRs, ","),
		config.reservedCIDRs,
		config.infrastructureCIDRs,
		config.defaultGatewayIPs,
		config.mtu,
		namespacedName(config.namespace, config.name),
		config.vlanID,
		config.allowPersistentIPs,
		config.physicalNetworkName,
		config.role,
	)
}

func generateNAD(config networkAttachmentConfig, client clientset.Interface) *nadapi.NetworkAttachmentDefinition {
	filterSupportedNetworkConfig(client, &config.networkAttachmentConfigParams)

	nadSpec := generateNADSpec(config)
	return generateNetAttachDef(config.namespace, config.name, nadSpec)
}

func generateNetAttachDef(namespace, nadName, nadSpec string) *nadapi.NetworkAttachmentDefinition {
	return &nadapi.NetworkAttachmentDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name:      nadName,
			Namespace: namespace,
		},
		Spec: nadapi.NetworkAttachmentDefinitionSpec{Config: nadSpec},
	}
}

func patchNADSpec(nadClient nadclient.K8sCniCncfIoV1Interface, name, namespace string, patch []byte) error {
	_, err := nadClient.NetworkAttachmentDefinitions(namespace).Patch(
		context.Background(),
		name,
		kapitypes.JSONPatchType,
		patch,
		metav1.PatchOptions{},
	)
	return err
}

type podConfiguration struct {
	attachments            []nadapi.NetworkSelectionElement
	containerCmd           []string
	name                   string
	namespace              string
	nodeSelector           map[string]string
	isPrivileged           bool
	labels                 map[string]string
	annotations            map[string]string
	requiresExtraNamespace bool
	hostNetwork            bool
	ipRequestFromSubnet    string
	usesExternalRouter     bool
}

func generatePodSpec(config podConfiguration) *v1.Pod {
	podSpec := e2epod.NewAgnhostPod(config.namespace, config.name, nil, nil, nil, config.containerCmd...)

	// Merge network attachments and custom annotations
	if podSpec.Annotations == nil {
		podSpec.Annotations = make(map[string]string)
	}
	if len(config.attachments) > 0 {
		attachmentAnnotations := networkSelectionElements(config.attachments...)
		for k, v := range attachmentAnnotations {
			podSpec.Annotations[k] = v
		}
	}
	for k, v := range config.annotations {
		podSpec.Annotations[k] = v
	}

	podSpec.Spec.NodeSelector = config.nodeSelector
	podSpec.Labels = config.labels
	podSpec.Spec.HostNetwork = config.hostNetwork
	if config.isPrivileged {
		podSpec.Spec.Containers[0].SecurityContext.Privileged = ptr.To(true)
	} else {
		for _, container := range podSpec.Spec.Containers {
			if container.SecurityContext.Capabilities == nil {
				container.SecurityContext.Capabilities = &v1.Capabilities{}
			}
			container.SecurityContext.Capabilities.Drop = []v1.Capability{"ALL"}
			container.SecurityContext.Privileged = ptr.To(false)
			container.SecurityContext.RunAsNonRoot = ptr.To(true)
			container.SecurityContext.RunAsUser = ptr.To(int64(1000))
			container.SecurityContext.AllowPrivilegeEscalation = ptr.To(false)
			container.SecurityContext.SeccompProfile = &v1.SeccompProfile{Type: v1.SeccompProfileTypeRuntimeDefault}
		}
	}
	return podSpec
}

func networkSelectionElements(elements ...nadapi.NetworkSelectionElement) map[string]string {
	marshalledElements, err := json.Marshal(elements)
	if err != nil {
		panic(fmt.Errorf("programmer error: you've provided wrong input to the test data: %v", err))
	}
	return map[string]string{
		nadapi.NetworkAttachmentAnnot: string(marshalledElements),
	}
}

func httpServerContainerCmd(port uint16) []string {
	return []string{"netexec", "--http-port", fmt.Sprintf("%d", port)}
}

func podNetworkStatus(pod *v1.Pod, predicates ...func(nadapi.NetworkStatus) bool) ([]nadapi.NetworkStatus, error) {
	podNetStatus, found := pod.Annotations[nadapi.NetworkStatusAnnot]
	if !found {
		return nil, fmt.Errorf("the pod must feature the `networks-status` annotation")
	}

	var netStatus []nadapi.NetworkStatus
	if err := json.Unmarshal([]byte(podNetStatus), &netStatus); err != nil {
		return nil, err
	}

	var netStatusMeetingPredicates []nadapi.NetworkStatus
	for i := range netStatus {
		for _, predicate := range predicates {
			if predicate(netStatus[i]) {
				netStatusMeetingPredicates = append(netStatusMeetingPredicates, netStatus[i])
				continue
			}
		}
	}
	return netStatusMeetingPredicates, nil
}

func podNetworkStatusByNetConfigPredicate(namespace, name, role string) func(nadapi.NetworkStatus) bool {
	return func(networkStatus nadapi.NetworkStatus) bool {
		if role == "primary" {
			return networkStatus.Default
		} else {
			return networkStatus.Name == namespace+"/"+name
		}
	}
}

func namespacedName(namespace, name string) string {
	return fmt.Sprintf("%s/%s", namespace, name)
}

func inRange(cidr string, ip string) error {
	_, cidrRange, err := net.ParseCIDR(cidr)
	if err != nil {
		return err
	}

	if cidrRange.Contains(net.ParseIP(ip)) {
		return nil
	}

	return fmt.Errorf("ip [%s] is NOT in range %s", ip, cidr)
}

func inAnyConfiguredSubnet(cidrs string, ip string) error {
	parsedIP := net.ParseIP(ip)
	if parsedIP == nil {
		return fmt.Errorf("invalid IP %q", ip)
	}
	ipv4 := parsedIP.To4() != nil

	var sameFamilyCIDRs []string
	for _, cidr := range strings.Split(cidrs, ",") {
		cidr = strings.TrimSpace(cidr)
		if cidr == "" {
			continue
		}
		subnet, err := getNetCIDRSubnet(cidr)
		if err != nil {
			return err
		}
		_, ipnet, err := net.ParseCIDR(subnet)
		if err != nil {
			return err
		}
		if (ipnet.IP.To4() != nil) != ipv4 {
			continue
		}
		sameFamilyCIDRs = append(sameFamilyCIDRs, subnet)
		if ipnet.Contains(parsedIP) {
			return nil
		}
	}
	if len(sameFamilyCIDRs) == 0 {
		return fmt.Errorf("no configured subnet for IP %q", ip)
	}
	return fmt.Errorf("ip [%s] is NOT in any configured subnet %s", ip, strings.Join(sameFamilyCIDRs, ","))
}

func cidrsContainIPFamily(cidrs string, ipv6 bool) bool {
	for _, cidr := range strings.Split(cidrs, ",") {
		cidr = strings.TrimSpace(cidr)
		if cidr == "" {
			continue
		}
		subnet, err := getNetCIDRSubnet(cidr)
		if err != nil {
			continue
		}
		if utilnet.IsIPv6CIDRString(subnet) == ipv6 {
			return true
		}
	}
	return false
}

func connectToServer(clientPodConfig podConfiguration, serverIP string, port uint16, args ...string) error {
	target := net.JoinHostPort(serverIP, fmt.Sprintf("%d", port))
	baseArgs := []string{
		"exec",
		clientPodConfig.name,
		"--",
		"curl",
		"-g", // Disable URL globbing to support IPv6 addresses with brackets
		"--connect-timeout",
		"2",
	}
	baseArgs = append(baseArgs, args...)

	_, err := e2ekubectl.RunKubectl(clientPodConfig.namespace, append(baseArgs, target)...)
	return err
}

func getMTUByInterfaceName(output, interfaceName string) (int, error) {
	var ifaces []struct {
		Name string `json:"ifname"`
		MTU  int    `json:"mtu"`
	}

	if err := json.Unmarshal([]byte(output), &ifaces); err != nil {
		return 0, fmt.Errorf("%s: %v", output, err)
	}

	for _, iface := range ifaces {
		if iface.Name == interfaceName {
			return iface.MTU, nil
		}
	}
	return 0, fmt.Errorf("interface %s not found", interfaceName)
}

func getSecondaryInterfaceMTU(clientPodConfig podConfiguration) (int, error) {
	const podSecondaryInterface = "net1"
	deviceInfoJSON, err := e2ekubectl.RunKubectl(
		clientPodConfig.namespace,
		"exec",
		clientPodConfig.name,
		"--",
		"ip",
		"-j",
		"link",
	)
	if err != nil {
		return 0, err
	}

	mtu, err := getMTUByInterfaceName(deviceInfoJSON, podSecondaryInterface)
	if err != nil {
		return 0, err
	}

	return mtu, nil
}

func pingServer(clientPodConfig podConfiguration, serverIP string, args ...string) error {
	baseArgs := []string{
		"exec",
		clientPodConfig.name,
		"--",
		"ping",
		"-c", "3", // send three ICMP echo requests
		"-W", "2", // timeout after 2 seconds if no response
	}
	baseArgs = append(baseArgs, args...)

	_, err := e2ekubectl.RunKubectl(clientPodConfig.namespace, append(baseArgs, serverIP)...)

	return err
}

func newAttachmentConfigWithOverriddenName(name, namespace, networkName, topology, cidr string) networkAttachmentConfig {
	return newNetworkAttachmentConfig(
		networkAttachmentConfigParams{
			cidr:        cidr,
			name:        name,
			namespace:   namespace,
			networkName: networkName,
			topology:    topology,
		},
	)
}

func configurePodStaticIP(podNamespace string, podName string, staticIP string) error {
	_, err := e2ekubectl.RunKubectl(
		podNamespace, "exec", podName, "--",
		"ip", "addr", "add", staticIP, "dev", "net1",
	)
	return err
}

func areStaticIPsConfiguredViaCNI(podConfig podConfiguration) bool {
	for _, attachment := range podConfig.attachments {
		if len(attachment.IPRequest) > 0 {
			return true
		}
	}
	return false
}

func podIPsForAttachment(k8sClient clientset.Interface, podNamespace string, podName string, attachmentName string) ([]string, error) {
	pod, err := k8sClient.CoreV1().Pods(podNamespace).Get(context.Background(), podName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	netStatus, err := podNetworkStatus(pod, func(status nadapi.NetworkStatus) bool {
		return status.Name == namespacedName(podNamespace, attachmentName)
	})
	if err != nil {
		return nil, err
	}

	if len(netStatus) == 0 {
		return nil, fmt.Errorf("no status entry for attachment %s on pod %s", attachmentName, namespacedName(podNamespace, podName))
	}
	if len(netStatus) > 1 {
		return nil, fmt.Errorf("more than one status entry for attachment %s on pod %s", attachmentName, namespacedName(podNamespace, podName))
	}
	if len(netStatus[0].IPs) == 0 {
		return nil, fmt.Errorf("no IPs for attachment %s on pod %s", attachmentName, namespacedName(podNamespace, podName))
	}
	return netStatus[0].IPs, nil
}

func podIPForAttachment(k8sClient clientset.Interface, podNamespace string, podName string, attachmentName string, ipIndex int) (string, error) {
	ips, err := podIPsForAttachment(k8sClient, podNamespace, podName, attachmentName)
	if err != nil {
		return "", err
	}
	if ipIndex >= len(ips) {
		return "", fmt.Errorf("no IP at index %d for attachment %s on pod %s", ipIndex, attachmentName, namespacedName(podNamespace, podName))
	}
	return ips[ipIndex], nil
}

func podIPsFromStatus(k8sClient clientset.Interface, podNamespace string, podName string) ([]string, error) {
	pod, err := k8sClient.CoreV1().Pods(podNamespace).Get(context.Background(), podName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	podIPs := make([]string, 0, len(pod.Status.PodIPs))
	for _, podIP := range pod.Status.PodIPs {
		podIPs = append(podIPs, podIP.IP)
	}
	return podIPs, nil
}

func allowedClient(podName string) string {
	return "allowed-" + podName
}

func blockedClient(podName string) string {
	return "blocked-" + podName
}

func multiNetPolicyPort(port int) mnpapi.MultiNetworkPolicyPort {
	tcp := v1.ProtocolTCP
	p := intstr.FromInt32(int32(port))
	return mnpapi.MultiNetworkPolicyPort{
		Protocol: &tcp,
		Port:     &p,
	}
}

func multiNetPolicyPortRange(port, endPort int) mnpapi.MultiNetworkPolicyPort {
	netpolPort := multiNetPolicyPort(port)
	endPort32 := int32(endPort)
	netpolPort.EndPort = &endPort32
	return netpolPort
}

func multiNetIngressLimitingPolicy(policyFor string, appliesFor metav1.LabelSelector, allowForSelector metav1.LabelSelector, allowPorts ...mnpapi.MultiNetworkPolicyPort) *mnpapi.MultiNetworkPolicy {
	var (
		portAllowlist []mnpapi.MultiNetworkPolicyPort
	)
	portAllowlist = append(portAllowlist, allowPorts...)
	return &mnpapi.MultiNetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
			PolicyForAnnotation: policyFor,
		},
			Name: "allow-ports-via-pod-selector",
		},

		Spec: mnpapi.MultiNetworkPolicySpec{
			PodSelector: appliesFor,
			Ingress: []mnpapi.MultiNetworkPolicyIngressRule{
				{
					Ports: portAllowlist,
					From: []mnpapi.MultiNetworkPolicyPeer{
						{
							PodSelector: &allowForSelector,
						},
					},
				},
			},
			PolicyTypes: []mnpapi.MultiPolicyType{mnpapi.PolicyTypeIngress},
		},
	}
}

func multiNetIngressLimitingIPBlockPolicy(
	policyFor string,
	appliesFor metav1.LabelSelector,
	allowForIPBlock mnpapi.IPBlock,
	allowPorts ...int,
) *mnpapi.MultiNetworkPolicy {
	var (
		portAllowlist []mnpapi.MultiNetworkPolicyPort
	)
	tcp := v1.ProtocolTCP
	for _, port := range allowPorts {
		p := intstr.FromInt(port)
		portAllowlist = append(portAllowlist, mnpapi.MultiNetworkPolicyPort{
			Protocol: &tcp,
			Port:     &p,
		})
	}
	return &mnpapi.MultiNetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
			PolicyForAnnotation: policyFor,
		},
			Name: "allow-ports-ip-block",
		},

		Spec: mnpapi.MultiNetworkPolicySpec{
			PodSelector: appliesFor,
			Ingress: []mnpapi.MultiNetworkPolicyIngressRule{
				{
					Ports: portAllowlist,
					From: []mnpapi.MultiNetworkPolicyPeer{
						{
							IPBlock: &allowForIPBlock,
						},
					},
				},
			},
			PolicyTypes: []mnpapi.MultiPolicyType{mnpapi.PolicyTypeIngress},
		},
	}
}

func multiNetEgressLimitingIPBlockPolicy(
	policyName string,
	policyFor string,
	appliesFor metav1.LabelSelector,
	allowForIPBlock mnpapi.IPBlock,
) *mnpapi.MultiNetworkPolicy {
	return &mnpapi.MultiNetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name: policyName,
			Annotations: map[string]string{
				PolicyForAnnotation: policyFor,
			},
		},

		Spec: mnpapi.MultiNetworkPolicySpec{
			PodSelector: appliesFor,
			Egress: []mnpapi.MultiNetworkPolicyEgressRule{
				{
					To: []mnpapi.MultiNetworkPolicyPeer{
						{
							IPBlock: &allowForIPBlock,
						},
					},
				},
			},
			PolicyTypes: []mnpapi.MultiPolicyType{mnpapi.PolicyTypeEgress},
		},
	}
}

func multiNetPolicy(
	policyName string,
	policyFor string,
	appliesFor metav1.LabelSelector,
	policyTypes []mnpapi.MultiPolicyType,
	ingress []mnpapi.MultiNetworkPolicyIngressRule,
	egress []mnpapi.MultiNetworkPolicyEgressRule,
) *mnpapi.MultiNetworkPolicy {
	return &mnpapi.MultiNetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name: policyName,
			Annotations: map[string]string{
				PolicyForAnnotation: policyFor,
			},
		},
		Spec: mnpapi.MultiNetworkPolicySpec{
			PodSelector: appliesFor,
			PolicyTypes: policyTypes,
			Ingress:     ingress,
			Egress:      egress,
		},
	}
}

func doesPolicyFeatAnIPBlock(policy *mnpapi.MultiNetworkPolicy) bool {
	for _, rule := range policy.Spec.Ingress {
		for _, peer := range rule.From {
			if peer.IPBlock != nil {
				return true
			}
		}
	}
	for _, rule := range policy.Spec.Egress {
		for _, peer := range rule.To {
			if peer.IPBlock != nil {
				return true
			}
		}
	}
	return false
}

func setBlockedClientIPInPolicyIPBlockExcludedRanges(policy *mnpapi.MultiNetworkPolicy, blockedIP string) {
	if policy.Spec.Ingress != nil {
		for _, rule := range policy.Spec.Ingress {
			for _, peer := range rule.From {
				if peer.IPBlock != nil {
					peer.IPBlock.Except = []string{blockedIP}
				}
			}
		}
	}
	if policy.Spec.Egress != nil {
		for _, rule := range policy.Spec.Egress {
			for _, peer := range rule.To {
				if peer.IPBlock != nil {
					peer.IPBlock.Except = []string{blockedIP}
				}
			}
		}
	}
}

func multiNetIngressLimitingPolicyAllowFromNamespace(
	policyFor string, appliesFor metav1.LabelSelector, allowForSelector metav1.LabelSelector, allowPorts ...int,
) *mnpapi.MultiNetworkPolicy {
	return &mnpapi.MultiNetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
			PolicyForAnnotation: policyFor,
		},
			Name: "allow-ports-same-ns",
		},

		Spec: mnpapi.MultiNetworkPolicySpec{
			PodSelector: appliesFor,
			Ingress: []mnpapi.MultiNetworkPolicyIngressRule{
				{
					Ports: allowedTCPPortsForPolicy(allowPorts...),
					From: []mnpapi.MultiNetworkPolicyPeer{
						{
							NamespaceSelector: &allowForSelector,
						},
					},
				},
			},
			PolicyTypes: []mnpapi.MultiPolicyType{mnpapi.PolicyTypeIngress},
		},
	}
}

func allowedTCPPortsForPolicy(allowPorts ...int) []mnpapi.MultiNetworkPolicyPort {
	var (
		portAllowlist []mnpapi.MultiNetworkPolicyPort
	)
	tcp := v1.ProtocolTCP
	for _, port := range allowPorts {
		p := intstr.FromInt(port)
		portAllowlist = append(portAllowlist, mnpapi.MultiNetworkPolicyPort{
			Protocol: &tcp,
			Port:     &p,
		})
	}
	return portAllowlist
}

func reachServerPodFromClient(cs clientset.Interface, serverConfig podConfiguration, clientConfig podConfiguration, serverIP string, serverPort uint16, args ...string) error {
	updatedPod, err := cs.CoreV1().Pods(serverConfig.namespace).Get(context.Background(), serverConfig.name, metav1.GetOptions{})
	if err != nil {
		return err
	}

	if updatedPod.Status.Phase == v1.PodRunning {
		return connectToServer(clientConfig, serverIP, serverPort, args...)
	}

	return fmt.Errorf("pod not running. /me is sad")
}

func pingServerPodFromClient(cs clientset.Interface, serverConfig podConfiguration, clientConfig podConfiguration, serverIP string, args ...string) error {
	updatedPod, err := cs.CoreV1().Pods(serverConfig.namespace).Get(context.Background(), serverConfig.name, metav1.GetOptions{})
	if err != nil {
		return err
	}

	if updatedPod.Status.Phase == v1.PodRunning {
		return pingServer(clientConfig, serverIP, args...)
	}

	return fmt.Errorf("pod not running. /me is sad")
}

func findInterfaceByIP(targetIP string) (string, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return "", err
	}

	for _, iface := range interfaces {
		addrs, err := iface.Addrs()
		if err != nil {
			return "", err
		}

		for _, addr := range addrs {
			ip, _, err := net.ParseCIDR(addr.String())
			if err != nil {
				return "", err
			}

			if ip.String() == targetIP {
				return iface.Name, nil
			}
		}
	}

	return "", fmt.Errorf("Interface with IP %s not found", targetIP)
}

func getNetworkGateway(cli *client.Client, networkName string) (string, error) {
	network, err := cli.NetworkInspect(context.Background(), networkName, types.NetworkInspectOptions{})
	if err != nil {
		return "", err
	}

	if network.IPAM.Config != nil && len(network.IPAM.Config) > 0 {
		gatewayIP := network.IPAM.Config[0].Gateway
		return gatewayIP, nil
	}

	return "", fmt.Errorf("Gateway not found for network %q", networkName)
}

func getPodAnnotationForAttachment(pod *v1.Pod, attachmentName string) (PodAnnotation, error) {
	podAnnotation, err := unmarshalPodAnnotation(pod.Annotations, attachmentName)
	if err != nil {
		return PodAnnotation{}, fmt.Errorf("failed to unmarshall annotations for pod %q: %v", pod.Name, err)
	}

	return *podAnnotation, nil
}

func getPodAnnotationIPsForAttachment(k8sClient clientset.Interface, podNamespace string, podName string, attachmentName string) ([]*net.IPNet, error) {
	pod, err := k8sClient.CoreV1().Pods(podNamespace).Get(context.Background(), podName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	podAnnotation, err := getPodAnnotationForAttachment(pod, attachmentName)
	if err != nil {
		return nil, err
	}
	return podAnnotation.IPs, nil
}

// podIPsForNetworkByIndex returns the v4 or v6 IPs for a pod on the UDN
func getPodAnnotationIPsForAttachmentByIndex(k8sClient clientset.Interface, podNamespace string, podName string, attachmentName string, index int) (string, error) {
	ipnets, err := getPodAnnotationIPsForAttachment(k8sClient, podNamespace, podName, attachmentName)
	if err != nil {
		return "", err
	}
	if index >= len(ipnets) {
		return "", fmt.Errorf("no IP at index %d for attachment %s on pod %s", index, attachmentName, namespacedName(podNamespace, podName))
	}
	if len(ipnets) > 2 {
		return "", fmt.Errorf("attachment for network %q with more than two IPs", attachmentName)
	}
	return ipnets[index].IP.String(), nil
}

func getPodAnnotationIPsForAttachmentByIPFamily(k8sClient clientset.Interface, podNamespace string, podName string, attachmentName string, family utilnet.IPFamily) (string, error) {
	ipnets, err := getPodAnnotationIPsForAttachment(k8sClient, podNamespace, podName, attachmentName)
	if err != nil {
		return "", err
	}
	if len(ipnets) > 2 {
		return "", fmt.Errorf("attachment for network %q with more than two IPs", attachmentName)
	}
	ipnet := getFirstCIDROfFamily(family, ipnets)
	if ipnet == nil {
		return "", fmt.Errorf("no %s IP for attachment %s on pod %s", family, attachmentName, namespacedName(podNamespace, podName))
	}
	return ipnet.IP.String(), nil
}

// generateIPsFromSubnets generates IP addresses from the given subnets with the specified offset
func generateIPsFromSubnets(subnets []string, offset int) ([]string, error) {
	var addrs []string
	for _, s := range subnets {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		ipGen, err := ip.NewIPGenerator(s)
		if err != nil {
			return nil, err
		}
		ip, err := ipGen.GenerateIP(offset)
		if err != nil {
			return nil, err
		}
		addrs = append(addrs, ip.String())
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("no valid subnets provided")
	}
	return addrs, nil
}
