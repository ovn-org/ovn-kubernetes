package util

import (
	"errors"
	"fmt"
	"hash/fnv"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/exp/constraints"
	"k8s.io/client-go/tools/cache"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	nadlister "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/listers/k8s.cni.cncf.io/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"

	"crypto/rand"

	"github.com/urfave/cli/v2"
	v1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	discoverylisters "k8s.io/client-go/listers/discovery/v1"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
)

// OvnConflictBackoff is the backoff used for pod annotation update conflict
var OvnConflictBackoff = wait.Backoff{
	Steps:    2,
	Duration: 10 * time.Millisecond,
	Factor:   5.0,
	Jitter:   0.1,
}

var (
	rePciDeviceName = regexp.MustCompile(`^[0-9a-f]{4}:[0-9a-f]{2}:[01][0-9a-f]\.[0-7]$`)
	reAuxDeviceName = regexp.MustCompile(`^\w+.\w+.\d+$`)
)

// IsPCIDeviceName check if passed device id is a PCI device name
func IsPCIDeviceName(deviceID string) bool {
	return rePciDeviceName.MatchString(deviceID)
}

// IsAuxDeviceName check if passed device id is a Auxiliary device name
func IsAuxDeviceName(deviceID string) bool {
	return reAuxDeviceName.MatchString(deviceID)
}

// StringArg gets the named command-line argument or returns an error if it is empty
func StringArg(context *cli.Context, name string) (string, error) {
	val := context.String(name)
	if val == "" {
		return "", fmt.Errorf("argument --%s should be non-null", name)
	}
	return val, nil
}

// GetIPNetFullMask returns an IPNet object for IPV4 or IPV6 address with a full subnet mask
func GetIPNetFullMask(ipStr string) (*net.IPNet, error) {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return nil, fmt.Errorf("failed to parse IP %q", ipStr)
	}
	mask := GetIPFullMask(ip)
	return &net.IPNet{
		IP:   ip,
		Mask: mask,
	}, nil
}

// GetIPNetFullMaskFromIP returns an IPNet object for IPV4 or IPV6 address with a full subnet mask
func GetIPNetFullMaskFromIP(ip net.IP) *net.IPNet {
	mask := GetIPFullMask(ip)
	return &net.IPNet{
		IP:   ip,
		Mask: mask,
	}
}

// GetIPFullMaskString returns /32 if ip is IPV4 family and /128 if ip is IPV6 family
func GetIPFullMaskString(ip string) string {
	const (
		// IPv4FullMask is the maximum prefix mask for an IPv4 address
		IPv4FullMask = "/32"
		// IPv6FullMask is the maxiumum prefix mask for an IPv6 address
		IPv6FullMask = "/128"
	)

	if utilnet.IsIPv6(net.ParseIP(ip)) {
		return IPv6FullMask
	}
	return IPv4FullMask
}

// GetIPFullMask returns a full IPv4 IPMask if ip is IPV4 family or a full IPv6
// IPMask otherwise
func GetIPFullMask(ip net.IP) net.IPMask {
	if utilnet.IsIPv6(ip) {
		return net.CIDRMask(128, 128)
	}
	return net.CIDRMask(32, 32)
}

// GetK8sMgmtIntfName returns the management port name for a given node.
func GetK8sMgmtIntfName(nodeName string) string {
	return types.K8sPrefix + nodeName
}

// GetLegacyK8sMgmtIntfName returns legacy management ovs-port name
func GetLegacyK8sMgmtIntfName(nodeName string) string {
	if len(nodeName) > 11 {
		return types.K8sPrefix + (nodeName[:11])
	}
	return GetK8sMgmtIntfName(nodeName)
}

// GetNetworkScopedK8sMgmtHostIntfName returns the management port host interface name for a network id
// NOTE: network id is used instead of name so we don't reach the linux device name limit of 15 chars
func GetNetworkScopedK8sMgmtHostIntfName(networkID uint) string {
	intfName := types.K8sMgmtIntfNamePrefix + fmt.Sprintf("%d", networkID)
	// We are over linux 15 chars limit for network devices, let's trim it
	// for the prefix so we keep networkID as much as possible
	if len(intfName) > 15 {
		return intfName[:15]
	}
	return intfName
}

func GetVRFDeviceNameForUDN(networkID int) string {
	return fmt.Sprintf("%s%d%s", types.UDNVRFDevicePrefix, networkID, types.UDNVRFDeviceSuffix)
}

// GetWorkerFromGatewayRouter determines a node's corresponding worker switch name from a gateway router name
func GetWorkerFromGatewayRouter(gr string) string {
	return strings.TrimPrefix(gr, types.GWRouterPrefix)
}

// GetGatewayRouterFromNode determines a node's corresponding gateway router name
func GetGatewayRouterFromNode(node string) string {
	return types.GWRouterPrefix + node
}

// GetGatewayRouterFromNode determines a node's corresponding gateway router name
func GetExtSwitchFromNode(node string) string {
	return types.ExternalSwitchPrefix + node
}

// GetExtPortName determines the name of a node's logical port to the external
// bridge.
func GetExtPortName(bridgeID, nodeName string) string {
	return bridgeID + "_" + nodeName
}

// GetPatchPortName determines the name of the patch port on the external
// bridge, which connects to br-int
func GetPatchPortName(bridgeID, nodeName string) string {
	return types.PatchPortPrefix + GetExtPortName(bridgeID, nodeName) + types.PatchPortSuffix
}

// GetNodeInternalAddrs returns the first IPv4 and/or IPv6 InternalIP defined
// for the node. On certain cloud providers (AWS) the egress IP will be added to
// the list of node IPs as an InternalIP address, we don't want to create the
// default allow logical router policies for that IP. Node IPs are ordered,
// meaning the egress IP will never be first in this list.
func GetNodeInternalAddrs(node *v1.Node) (net.IP, net.IP) {
	var v4Addr, v6Addr net.IP
	for _, nodeAddr := range node.Status.Addresses {
		if nodeAddr.Type == v1.NodeInternalIP {
			ip := utilnet.ParseIPSloppy(nodeAddr.Address)
			if !utilnet.IsIPv6(ip) && v4Addr == nil {
				v4Addr = ip
			} else if utilnet.IsIPv6(ip) && v6Addr == nil {
				v6Addr = ip
			}
		}
	}
	return v4Addr, v6Addr
}

// GetNodeAddresses returns all of the node's IPv4 and/or IPv6 annotated
// addresses as requested. Note that nodes not annotated will be ignored.
func GetNodeAddresses(ipv4, ipv6 bool, nodes ...*v1.Node) (ipsv4 []net.IP, ipsv6 []net.IP, err error) {
	allCIDRs := sets.Set[string]{}
	for _, node := range nodes {
		ips, err := ParseNodeHostCIDRs(node)
		if IsAnnotationNotSetError(err) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		allCIDRs = allCIDRs.Insert(ips.UnsortedList()...)
	}

	for _, cidr := range allCIDRs.UnsortedList() {
		ip, _, err := net.ParseCIDR(cidr)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to get parse CIDR %v: %w", cidr, err)
		}
		if ipv4 && utilnet.IsIPv4(ip) {
			ipsv4 = append(ipsv4, ip)
		} else if ipv6 && utilnet.IsIPv6(ip) {
			ipsv6 = append(ipsv6, ip)
		}
	}
	return
}

// GetNodeChassisID returns the machine's OVN chassis ID
func GetNodeChassisID() (string, error) {
	chassisID, stderr, err := RunOVSVsctl("--if-exists", "get",
		"Open_vSwitch", ".", "external_ids:system-id")
	if err != nil {
		klog.Errorf("No system-id configured in the local host, "+
			"stderr: %q, error: %v", stderr, err)
		return "", err
	}
	if chassisID == "" {
		return "", fmt.Errorf("no system-id configured in the local host")
	}

	return chassisID, nil
}

// GetHybridOverlayPortName returns the name of the hybrid overlay switch port
// for a given node
func GetHybridOverlayPortName(nodeName string) string {
	return "int-" + nodeName
}

type annotationNotSetError struct {
	msg string
}

func (anse *annotationNotSetError) Error() string {
	return anse.msg
}

// newAnnotationNotSetError returns an error for an annotation that is not set
func newAnnotationNotSetError(format string, args ...interface{}) error {
	return &annotationNotSetError{msg: fmt.Sprintf(format, args...)}
}

// IsAnnotationNotSetError returns true if the error indicates that an annotation is not set
func IsAnnotationNotSetError(err error) bool {
	var annotationNotSetError *annotationNotSetError
	return errors.As(err, &annotationNotSetError)
}

type annotationAlreadySetError struct {
	msg string
}

func (aase *annotationAlreadySetError) Error() string {
	return aase.msg
}

// newAnnotationAlreadySetError returns an error for an annotation that is not set
func newAnnotationAlreadySetError(format string, args ...interface{}) error {
	return &annotationAlreadySetError{msg: fmt.Sprintf(format, args...)}
}

// IsAnnotationAlreadySetError returns true if the error indicates that an annotation is already set
func IsAnnotationAlreadySetError(err error) bool {
	var annotationAlreadySetError *annotationAlreadySetError
	return errors.As(err, &annotationAlreadySetError)
}

// HashforOVN hashes the provided input to make it a valid addressSet or portGroup name.
func HashForOVN(s string) string {
	h := fnv.New64a()
	_, err := h.Write([]byte(s))
	if err != nil {
		klog.Errorf("Failed to hash %s", s)
		return ""
	}
	hashString := strconv.FormatUint(h.Sum64(), 10)
	return fmt.Sprintf("a%s", hashString)
}

// UpdateIPsSlice will search for values of oldIPs in the slice "s" and update it with newIPs values of same IP family
func UpdateIPsSlice(s, oldIPs, newIPs []string) ([]string, bool) {
	n := make([]string, len(s))
	copy(n, s)
	updated := false
	for i, entry := range s {
		for _, oldIP := range oldIPs {
			if entry == oldIP {
				for _, newIP := range newIPs {
					if utilnet.IsIPv6(net.ParseIP(oldIP)) {
						if utilnet.IsIPv6(net.ParseIP(newIP)) {
							n[i] = newIP
							updated = true
							break
						}
					} else {
						if !utilnet.IsIPv6(net.ParseIP(newIP)) {
							n[i] = newIP
							updated = true
							break
						}
					}
				}
				break
			}
		}
	}
	return n, updated
}

// FilterIPsSlice will filter a list of IPs by a list of CIDRs. By default,
// it will *remove* all IPs that match filter, unless keep is true.
//
// It is dual-stack aware.
func FilterIPsSlice(s []string, filter []net.IPNet, keep bool) []string {
	out := make([]string, 0, len(s))
ipLoop:
	for _, ipStr := range s {
		ip := net.ParseIP(ipStr)
		is4 := ip.To4() != nil

		for _, cidr := range filter {
			if is4 && cidr.IP.To4() != nil && cidr.Contains(ip) {
				if keep {
					out = append(out, ipStr)
					continue ipLoop
				} else {
					continue ipLoop
				}
			}
			if !is4 && cidr.IP.To4() == nil && cidr.Contains(ip) {
				if keep {
					out = append(out, ipStr)
					continue ipLoop
				} else {
					continue ipLoop
				}
			}
		}
		if !keep { // discard mode, and nothing matched.
			out = append(out, ipStr)
		}
	}

	return out
}

// IsClusterIP checks if the provided IP is a clusterIP
func IsClusterIP(svcVIP string) bool {
	ip := net.ParseIP(svcVIP)
	is4 := ip.To4() != nil
	for _, svcCIDR := range config.Kubernetes.ServiceCIDRs {
		if is4 && svcCIDR.IP.To4() != nil && svcCIDR.Contains(ip) {
			return true
		}
		if !is4 && svcCIDR.IP.To4() == nil && svcCIDR.Contains(ip) {
			return true
		}
	}
	return false
}

type UnknownActiveNetworkError struct {
	namespace string
}

func (m *UnknownActiveNetworkError) Error() string {
	return fmt.Sprintf("unable to determine what is the "+
		"primary role network for namespace '%s'; please remove multiple primary role network"+
		"NADs from it", m.namespace)
}

func IsUnknownActiveNetworkError(err error) bool {
	var unknownActiveNetworkError *UnknownActiveNetworkError
	return errors.As(err, &unknownActiveNetworkError)
}

// GetActiveNetworkForNamespace returns the NetInfo struct of the active network
// for the given namespace based on the NADs present in that namespace.
// active network here means the network managing this namespace and responsible for
// plumbing all the entities for this namespace
// this is:
// 1) &DefaultNetInfo if there are no NADs in the namespace OR all NADs are Role: "primary"
// 2) &NetConf{Name: "<secondary-network-name>"} if there is exactly ONE NAD with Role: "primary"
// 3) Multiple primary network role NADs ActiveNetworkUnknown error
// 4) error under all other conditions
func GetActiveNetworkForNamespace(namespace string, nadLister nadlister.NetworkAttachmentDefinitionLister) (NetInfo, error) {
	if nadLister == nil {
		return &DefaultNetInfo{}, nil
	}
	if !IsNetworkSegmentationSupportEnabled() {
		return &DefaultNetInfo{}, nil
	}
	namespaceNADs, err := nadLister.NetworkAttachmentDefinitions(namespace).List(labels.Everything())
	if err != nil {
		return nil, err
	}
	if len(namespaceNADs) == 0 {
		return &DefaultNetInfo{}, nil
	}
	numberOfPrimaryNetworks := 0
	var primaryNetwork NetInfo
	for _, nad := range namespaceNADs {
		netInfo, err := ParseNADInfo(nad)
		if err != nil {
			klog.Warningf("Skipping nad '%s/%s' as active network after failing parsing it with %v", nad.Namespace, nad.Name, err)
			continue
		}

		if netInfo.IsPrimaryNetwork() {
			primaryNetwork = netInfo
			numberOfPrimaryNetworks++
			primaryNetwork.AddNADs(GetNADName(nad.Namespace, nad.Name))
		}
	}
	if numberOfPrimaryNetworks == 1 {
		return primaryNetwork, nil
	} else if numberOfPrimaryNetworks == 0 {
		return &DefaultNetInfo{}, nil
	}
	return nil, &UnknownActiveNetworkError{namespace: namespace}
}

func GetUserDefinedNetworkRole(isPrimary bool) string {
	networkRole := types.NetworkRoleSecondary
	if isPrimary {
		networkRole = types.NetworkRolePrimary
	}
	return networkRole
}

// generateExternalIDs returns the external IDs for logical switches and logical routers
// when it runs on a primary or secondary network. It returns an empty map
// when on the default cluster network, for backward compatibility.
func GenerateExternalIDsForSwitchOrRouter(netInfo NetInfo) map[string]string {
	externalIDs := make(map[string]string)
	if netInfo.IsSecondary() {
		externalIDs[types.NetworkExternalID] = netInfo.GetNetworkName()
		externalIDs[types.NetworkRoleExternalID] = GetUserDefinedNetworkRole(netInfo.IsPrimaryNetwork())
		externalIDs[types.TopologyExternalID] = netInfo.TopologyType()
	}
	return externalIDs
}

func GetSecondaryNetworkLogicalPortName(podNamespace, podName, nadName string) string {
	return GetSecondaryNetworkPrefix(nadName) + composePortName(podNamespace, podName)
}

func GetLogicalPortName(podNamespace, podName string) string {
	return composePortName(podNamespace, podName)
}

func GetSecondaryNetworkIfaceId(podNamespace, podName, nadName string) string {
	return GetSecondaryNetworkPrefix(nadName) + composePortName(podNamespace, podName)
}

func GetIfaceId(podNamespace, podName string) string {
	return composePortName(podNamespace, podName)
}

// composePortName should be called both for LogicalPortName and iface-id
// because ovn-nb man says:
// Logical_Switch_Port.name must match external_ids:iface-id
// in the Open_vSwitch database’s Interface table,
// because hypervisors use external_ids:iface-id as a lookup key to
// identify the network interface of that entity.
func composePortName(podNamespace, podName string) string {
	return podNamespace + "_" + podName
}

func SliceHasStringItem(slice []string, item string) bool {
	for _, i := range slice {
		if i == item {
			return true
		}
	}
	return false
}

// StringSlice converts to a slice of the string representation of the input
// items
func StringSlice[T fmt.Stringer](items []T) []string {
	s := make([]string, len(items))
	for i := range items {
		s[i] = items[i].String()
	}
	return s
}

func SortedKeys[K constraints.Ordered, V any](m map[K]V) []K {
	keys := make([]K, len(m))
	i := 0
	for k := range m {
		keys[i] = k
		i++
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	return keys
}

var chars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ1234567890-"

// GenerateId returns a random id as a string with the requested length
func GenerateId(length int) string {
	charsLength := len(chars)
	b := make([]byte, length)
	_, err := rand.Read(b) // generates len(b) random bytes
	if err != nil {
		klog.Errorf("Failed when generating a random ID: %v", err)
		return ""
	}

	for i := 0; i < length; i++ {
		b[i] = chars[int(b[i])%charsLength]
	}
	return string(b)
}

// IsMirrorEndpointSlice checks if the provided EndpointSlice is meant for the user defined network
func IsMirrorEndpointSlice(endpointSlice *discoveryv1.EndpointSlice) bool {
	_, ok := endpointSlice.Labels[types.LabelUserDefinedServiceName]
	return ok
}

// IsDefaultEndpointSlice checks if the provided EndpointSlice is meant for the default network
func IsDefaultEndpointSlice(endpointSlice *discoveryv1.EndpointSlice) bool {
	_, ok := endpointSlice.Labels[discoveryv1.LabelServiceName]
	return ok
}

// IsEndpointSliceForNetwork checks if the provided EndpointSlice is meant for the given network
func IsMirroredEndpointSliceForNetwork(endpointSlice *discoveryv1.EndpointSlice, network string) bool {
	if endpointSliceNetwork, ok := endpointSlice.Labels[types.LabelUserDefinedEndpointSliceNetwork]; ok {
		return endpointSliceNetwork == network
	}
	return false
}

func GetDefaultEndpointSlicesEventHandler(handlerFuncs cache.ResourceEventHandlerFuncs) cache.ResourceEventHandler {
	return GetEndpointSlicesEventHandlerForNetwork(handlerFuncs, &DefaultNetInfo{})
}

// GetEndpointSlicesEventHandlerForNetwork returns an event handler based on the provided handlerFuncs and netInfo.
// On the default network, it returns a handler that filters out the mirrored EndpointSlices. Conversely in
// a primary network it returns a handler that only keeps the mirrored EndpointSlices and filters out the original ones.
// Otherwise, returns handlerFuncs as is.
func GetEndpointSlicesEventHandlerForNetwork(handlerFuncs cache.ResourceEventHandlerFuncs, netInfo NetInfo) cache.ResourceEventHandler {
	var eventHandler cache.ResourceEventHandler
	eventHandler = handlerFuncs
	if !IsNetworkSegmentationSupportEnabled() {
		return eventHandler
	}

	var filterFunc func(obj interface{}) bool

	if netInfo.IsDefault() {
		// Filter out objects without the "kubernetes.io/service-name" label to exclude mirrored EndpointSlices
		filterFunc = func(obj interface{}) bool {
			if endpointSlice, ok := obj.(*discoveryv1.EndpointSlice); ok {
				return IsDefaultEndpointSlice(endpointSlice)
			}
			klog.Errorf("Failed to cast the object to *discovery.EndpointSlice: %v", obj)
			return true
		}

	} else if netInfo.IsPrimaryNetwork() {
		// Only consider mirrored endpointslices for the given network
		filterFunc = func(obj interface{}) bool {
			if endpointSlice, ok := obj.(*discoveryv1.EndpointSlice); ok {
				isDefault := IsDefaultEndpointSlice(endpointSlice)
				isMirror := IsMirrorEndpointSlice(endpointSlice)
				isForThisNetwork := IsMirroredEndpointSliceForNetwork(endpointSlice, netInfo.GetNetworkName())
				return !isDefault && isMirror && isForThisNetwork
			}
			klog.Errorf("Failed to cast the object to *discovery.EndpointSlice: %v", obj)
			return true
		}
	}
	if filterFunc != nil {
		eventHandler = cache.FilteringResourceEventHandler{
			FilterFunc: filterFunc,
			Handler:    handlerFuncs,
		}
	}

	return eventHandler
}

// GetEndpointSlicesBySelector returns a list of EndpointSlices in a given namespace by the label selector
func GetEndpointSlicesBySelector(namespace string, labelSelector metav1.LabelSelector, endpointSliceLister discoverylisters.EndpointSliceLister) ([]*discoveryv1.EndpointSlice, error) {
	selector, err := metav1.LabelSelectorAsSelector(&labelSelector)
	if err != nil {
		return nil, err
	}
	return endpointSliceLister.EndpointSlices(namespace).List(selector)
}

// GetServiceEndpointSlices returns the endpointSlices associated with a service for the specified network
// if network is DefaultNetworkName the default endpointSlices are returned, otherwise the function looks for mirror endpointslices
// for the specified network.
func GetServiceEndpointSlices(namespace, svcName, network string, endpointSliceLister discoverylisters.EndpointSliceLister) ([]*discovery.EndpointSlice, error) {
	var selector metav1.LabelSelector
	if network == types.DefaultNetworkName {
		selector = metav1.LabelSelector{MatchLabels: map[string]string{
			discovery.LabelServiceName: svcName,
		}}
	} else {
		selector = metav1.LabelSelector{MatchLabels: map[string]string{
			types.LabelUserDefinedServiceName:          svcName,
			types.LabelUserDefinedEndpointSliceNetwork: network,
		}}
	}
	return GetEndpointSlicesBySelector(namespace, selector, endpointSliceLister)
}
