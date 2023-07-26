package util

import (
	"fmt"
	"net"
	"sync"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
)

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

	// Only query the cache for mp0 and HO LSPs
	haveManagementPort := true
	managmentPort := &nbdb.LogicalSwitchPort{Name: types.K8sPrefix + nodeName}
	_, err := libovsdbops.GetLogicalSwitchPort(nbClient, managmentPort)
	if err == libovsdbclient.ErrNotFound {
		klog.V(5).Infof("Management port does not exist for node %s", nodeName)
		haveManagementPort = false
	} else if err != nil {
		return fmt.Errorf("failed to get management port for node %s error: %v", nodeName, err)
	}

	haveHybridOverlayPort := true
	HOPort := &nbdb.LogicalSwitchPort{Name: types.HybridOverlayPrefix + nodeName}
	_, err = libovsdbops.GetLogicalSwitchPort(nbClient, HOPort)
	if err == libovsdbclient.ErrNotFound {
		klog.V(5).Infof("Hybridoverlay port does not exist for node %s", nodeName)
		haveHybridOverlayPort = false
	} else if err != nil {
		return fmt.Errorf("failed to get hybrid overlay port for node %s error: %v", nodeName, err)
	}

	mgmtIfAddr := util.GetNodeManagementIfAddr(subnet)
	hybridOverlayIfAddr := util.GetNodeHybridOverlayIfAddr(subnet)

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

	sw := nbdb.LogicalSwitch{
		Name:        nodeName,
		OtherConfig: map[string]string{"exclude_ips": excludeIPs},
	}
	err = libovsdbops.UpdateLogicalSwitchSetOtherConfig(nbClient, &sw)
	if err != nil {
		return fmt.Errorf("failed to update exclude_ips %+v: %v", sw, err)
	}

	return nil
}
