package ovn

import (
	"fmt"
	"net"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	"k8s.io/klog/v2"
)

// getJoinLRPAddresses check if IPs of gateway logical router port are within the join switch IP range, and return them if true.
func (oc *Controller) getJoinLRPAddresses(nodeName string) []*net.IPNet {
	// try to get the IPs from the logical router port
	gwLRPIPs := []*net.IPNet{}
	gwLrpName := types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + nodeName
	joinSubnets := oc.joinSwIPManager.lsm.GetSwitchSubnets(nodeName)
	ifAddrs, err := util.GetLRPAddrs(gwLrpName)
	if err == nil {
		for _, ifAddr := range ifAddrs {
			for _, subnet := range joinSubnets {
				if subnet.Contains(ifAddr.IP) {
					gwLRPIPs = append(gwLRPIPs, &net.IPNet{IP: ifAddr.IP, Mask: subnet.Mask})
					break
				}
			}
		}
	}

	if len(gwLRPIPs) != len(joinSubnets) {
		var errStr string
		if len(gwLRPIPs) == 0 {
			errStr = fmt.Sprintf("Failed to get IPs for logical router port %s", gwLrpName)
		} else {
			errStr = fmt.Sprintf("Invalid IPs %s (possibly not in the range of subnet %s)",
				util.JoinIPNetIPs(gwLRPIPs, " "), util.JoinIPNetIPs(joinSubnets, " "))
		}
		klog.Warningf("%s for logical router port %s", errStr, gwLrpName)
		return []*net.IPNet{}
	}
	return gwLRPIPs
}
