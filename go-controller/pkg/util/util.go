package util

import (
	"context"
	"fmt"
	"hash/fnv"
	"net"
	"strconv"
	"strings"
	"sync"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	cnitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"

	"github.com/urfave/cli/v2"

	kapi "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
)

// StringArg gets the named command-line argument or returns an error if it is empty
func StringArg(context *cli.Context, name string) (string, error) {
	val := context.String(name)
	if val == "" {
		return "", fmt.Errorf("argument --%s should be non-null", name)
	}
	return val, nil
}

// GetLegacyK8sMgmtIntfName returns legacy management ovs-port name
func GetLegacyK8sMgmtIntfName(nodeName string) string {
	if len(nodeName) > 11 {
		return types.K8sPrefix + (nodeName[:11])
	}
	return types.K8sPrefix + nodeName
}

// GetWorkerFromGatewayRouter determines a node's corresponding worker switch name from a gateway router name
func GetWorkerFromGatewayRouter(gr string) string {
	return strings.TrimPrefix(gr, types.GWRouterPrefix)
}

// GetGatewayRouterFromNode determines a node's corresponding gateway router name
func GetGatewayRouterFromNode(node string) string {
	return types.GWRouterPrefix + node
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

func (anse annotationNotSetError) Error() string {
	return anse.msg
}

// newAnnotationNotSetError returns an error for an annotation that is not set
func newAnnotationNotSetError(format string, args ...interface{}) error {
	return annotationNotSetError{msg: fmt.Sprintf(format, args...)}
}

// IsAnnotationNotSetError returns true if the error indicates that an annotation is not set
func IsAnnotationNotSetError(err error) bool {
	_, ok := err.(annotationNotSetError)
	return ok
}

// CalculateHostSubnetsForClusterEntry calculates the host subnets
// available in a CIDR entry
func CalculateHostSubnetsForClusterEntry(cidrEntry config.CIDRNetworkEntry,
	v4HostSubnetCount, v6HostSubnetCount *float64) {
	prefixLength, _ := cidrEntry.CIDR.Mask.Size()
	var one uint64 = 1
	if prefixLength > cidrEntry.HostSubnetLength {
		klog.Warningf("Invalid cidr entry: %+v found while calculating subnet count",
			cidrEntry)
		return
	}
	if !utilnet.IsIPv6CIDR(cidrEntry.CIDR) {
		*v4HostSubnetCount = *v4HostSubnetCount + float64(one<<(cidrEntry.HostSubnetLength-prefixLength))
	} else {
		*v6HostSubnetCount = *v6HostSubnetCount + float64(one<<(cidrEntry.HostSubnetLength-prefixLength))

	}
}

// UpdateUsedHostSubnetsCount increments the v4/v6 host subnets count based on
// the subnet being visited
func UpdateUsedHostSubnetsCount(subnet *net.IPNet,
	v4SubnetsAllocated, v6SubnetsAllocated *float64, isAdd bool) {
	op := -1
	if isAdd {
		op = 1
	}
	if !utilnet.IsIPv6CIDR(subnet) {
		*v4SubnetsAllocated = *v4SubnetsAllocated + float64(1*op)
	} else {
		*v6SubnetsAllocated = *v6SubnetsAllocated + float64(1*op)
	}
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

type NetNameInfo struct {
	// netconf's name
	NetName    string
	Prefix     string
	NotDefault bool
}

type NetAttachDefInfo struct {
	NetNameInfo
	// net-attach-defs shared the same CNI Conf, key is <Namespace>/<Name> of net-attach-def.
	// Note that it means they share the same logical switch (subnet cidr/MTU etc), but they might
	// have different resource requirement (requires or not require VF, or different VF resource set)
	NetAttachDefs sync.Map
	NetCidr       string
	MTU           int
}

func NewNetAttachDefInfo(netconf *cnitypes.NetConf) (*NetAttachDefInfo, error) {
	netName := "default"
	if netconf.NotDefault {
		netName = netconf.Name
	}
	prefix := GetNetworkPrefix(netName, !netconf.NotDefault)

	nadInfo := NetAttachDefInfo{
		NetCidr:     netconf.NetCidr,
		MTU:         netconf.MTU,
		NetNameInfo: NetNameInfo{netName, prefix, netconf.NotDefault},
	}

	return &nadInfo, nil
}

// Note that for port_group and address_set, it does not allow the '-' character
func GetNadName(namespace, name string, isDefault bool) string {
	if isDefault {
		return "default"
	}
	return GetNadKeyName(namespace, name)
}

// key of NetAttachDefInfo.NetAttachDefs map
func GetNadKeyName(namespace, name string) string {
	return fmt.Sprintf("%s/%s", namespace, name)
}

// Note that for port_group and address_set, it does not allow the '-' character
// Also replace "/" in nadName with "."
func GetNetworkPrefix(netName string, isDefault bool) string {
	if isDefault {
		return ""
	}
	name := strings.ReplaceAll(netName, "-", ".")
	name = strings.ReplaceAll(name, "/", ".")
	return name + "_"
}

// UpdateIPsSlice will search for values of oldIPs in the slice "s" and update it with newIPs values of same IP family
func UpdateIPsSlice(s, oldIPs, newIPs []string) []string {
	n := make([]string, len(s))
	copy(n, s)
	for i, entry := range s {
		for _, oldIP := range oldIPs {
			if entry == oldIP {
				for _, newIP := range newIPs {
					if utilnet.IsIPv6(net.ParseIP(oldIP)) {
						if utilnet.IsIPv6(net.ParseIP(newIP)) {
							n[i] = newIP
							break
						}
					} else {
						if !utilnet.IsIPv6(net.ParseIP(newIP)) {
							n[i] = newIP
							break
						}
					}
				}
				break
			}
		}
	}
	return n
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

func GetLogicalPortName(podNamespace, podName, nadName string, isDefault bool) string {
	netPrefix := GetNetworkPrefix(nadName, isDefault)
	return composePortName(podNamespace, podName, netPrefix)
}

func GetIfaceId(podNamespace, podName, nadName string, isDefault bool) string {
	netPrefix := GetNetworkPrefix(nadName, isDefault)
	return composePortName(podNamespace, podName, netPrefix)
}

// composePortName should be called both for LogicalPortName and iface-id
// because ovn-nb man says:
// Logical_Switch_Port.name must match external_ids:iface-id
// in the Open_vSwitch database’s Interface table,
// because hypervisors use external_ids:iface-id as a lookup key to
// identify the network interface of that entity.
func composePortName(podNamespace, podName, netPrefix string) string {
	return netPrefix + podNamespace + "_" + podName
}

// Get all possible logical ports name of this network
func GetAllLogicalPortNames(pod *kapi.Pod, nadInfo *NetAttachDefInfo) []string {
	ports := []string{}
	on, networkMap, err := IsNetworkOnPod(pod, nadInfo)
	if err != nil {
		klog.Errorf(err.Error())
	} else if on {
		// the pod is attached to this specific network
		for nadName := range networkMap {
			portName := GetLogicalPortName(pod.Namespace, pod.Name, nadName, !nadInfo.NotDefault)
			ports = append(ports, portName)
		}
	}
	return ports
}

func SliceHasStringItem(slice []string, item string) bool {
	for _, i := range slice {
		if i == item {
			return true
		}
	}
	return false
}

var updateNodeSwitchLock sync.Mutex

// UpdateNodeSwitchExcludeIPs should be called after adding the management port
// and after adding the hybrid overlay port, and ensures that each port's IP
// is added to the logical switch's exclude_ips. This prevents ovn-northd log
// spam about duplicate IP addresses.
// See https://github.com/ovn-org/ovn-kubernetes/pull/779
func UpdateNodeSwitchExcludeIPs(nbClient libovsdbclient.Client, nodeName string, subnet *net.IPNet) error {
	if utilnet.IsIPv6CIDR(subnet) {
		// We don't exclude any IPs in IPv6
		return nil
	}

	updateNodeSwitchLock.Lock()
	defer updateNodeSwitchLock.Unlock()

	managmentPort := &nbdb.LogicalSwitchPort{Name: types.K8sPrefix + nodeName}
	HOPort := &nbdb.LogicalSwitchPort{Name: types.HybridOverlayPrefix + nodeName}
	haveManagementPort := true
	haveHybridOverlayPort := true
	// Only Query The cache for mp0 and HO LSPs
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	if err := nbClient.Get(ctx, managmentPort); err != nil {
		if err != libovsdbclient.ErrNotFound {
			return fmt.Errorf("failed to get management port for node %s error: %v", nodeName, err)
		}
		klog.V(5).Infof("Management port does not exist for node %s", nodeName)
		haveManagementPort = false
	}

	if err := nbClient.Get(ctx, HOPort); err != nil {
		if err != libovsdbclient.ErrNotFound {
			return fmt.Errorf("failed to get hybrid overlay port for node %s error: %v", nodeName, err)
		}
		klog.V(5).Infof("Hybridoverlay port does not exist for node %s", nodeName)
		haveHybridOverlayPort = false
	}

	mgmtIfAddr := GetNodeManagementIfAddr(subnet)
	hybridOverlayIfAddr := GetNodeHybridOverlayIfAddr(subnet)

	klog.V(5).Infof("haveMP %v haveHO %v ManagementPortAddress %v HybridOverlayAddressOA %v", haveManagementPort, haveHybridOverlayPort, mgmtIfAddr, hybridOverlayIfAddr)
	var excludeIPs string
	if config.HybridOverlay.Enabled {
		if haveHybridOverlayPort && haveManagementPort {
			// no excluded IPs required
		} else if !haveHybridOverlayPort && !haveManagementPort {
			// exclude both
			excludeIPs = mgmtIfAddr.IP.String() + ".." + hybridOverlayIfAddr.IP.String()
		} else if haveHybridOverlayPort {
			// exclude management port IP
			excludeIPs = mgmtIfAddr.IP.String()
		} else if haveManagementPort {
			// exclude hybrid overlay port IP
			excludeIPs = hybridOverlayIfAddr.IP.String()
		}
	} else if !haveManagementPort {
		// exclude management port IP
		excludeIPs = mgmtIfAddr.IP.String()
	}

	logicalSwitchDes := nbdb.LogicalSwitch{
		Name:        nodeName,
		OtherConfig: map[string]string{"exclude_ips": excludeIPs},
	}

	opModels := []libovsdbops.OperationModel{
		{
			Model:          &logicalSwitchDes,
			ModelPredicate: func(ls *nbdb.LogicalSwitch) bool { return ls.Name == nodeName },
			OnModelMutations: []interface{}{
				&logicalSwitchDes.OtherConfig,
			},
			ErrNotFound: true,
		},
	}

	m := libovsdbops.NewModelClient(nbClient)
	// If excludeIPs is empty ensure that we have no excludeIPs in the Switch's OtherConfig
	if len(excludeIPs) == 0 {
		if err := m.Delete(opModels...); err != nil {
			return fmt.Errorf("failed to delete otherConfig:exclude_ips from logical switch %s, error: %v", nodeName, err)
		}
	} else {
		if _, err := m.CreateOrUpdate(opModels...); err != nil {
			return fmt.Errorf("failed to configure otherConfig:exclude_ips from logical switch %s, error: %v", nodeName, err)
		}
	}

	return nil
}
