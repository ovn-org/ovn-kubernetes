package ovn

import (
	"fmt"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"net"

	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
)

// SetupLocalnetMaster creates localnet switch for the network
func (oc *Controller) SetupLocalnetMaster() error {
	switchName := oc.nadInfo.Prefix + types.OVNLocalnetSwitch

	// Create a single common switch for the cluster.
	logicalSwitch := nbdb.LogicalSwitch{
		Name: switchName,
	}
	if oc.nadInfo.IsSecondary {
		logicalSwitch.ExternalIDs = map[string]string{"network_name": oc.nadInfo.NetName}
	}

	for _, subnet := range oc.clusterSubnets {
		hostSubnet := subnet.CIDR
		if utilnet.IsIPv6CIDR(hostSubnet) {
			logicalSwitch.OtherConfig = map[string]string{"ipv6_prefix": hostSubnet.IP.String()}
		} else {
			//mgmtIfAddr := util.GetNodeManagementIfAddr(hostSubnet)
			//excludeIPs := mgmtIfAddr.IP.String()
			logicalSwitch.OtherConfig = map[string]string{"subnet": hostSubnet.String()}
		}
	}

	// Create the Node's Logical Switch and set it's subnet
	opModels := []libovsdbops.OperationModel{
		{
			Name:           logicalSwitch.Name,
			Model:          &logicalSwitch,
			ModelPredicate: func(ls *nbdb.LogicalSwitch) bool { return ls.Name == switchName },
			OnModelUpdates: []interface{}{
				&logicalSwitch.OtherConfig,
				&logicalSwitch.ExternalIDs,
			},
		},
	}
	// TBD other-config:mcast_snoop, other-config:mcast_querier etc. see
	if _, err := oc.mc.modelClient.CreateOrUpdate(opModels...); err != nil {
		klog.Errorf("Failed to create a common localnet switch %s for network %s: %v", switchName, oc.nadInfo.NetName, err)
		return err
	}

	// Add external interface as a logical port to external_switch.
	// This is a learning switch port with "unknown" address. The external
	// world is accessed via this port.
	logicalSwitchPort := nbdb.LogicalSwitchPort{
		Addresses: []string{"unknown"},
		Type:      "localnet",
		Options: map[string]string{
			"network_name": oc.nadInfo.Prefix + types.LocalNetBridgeName,
		},
		Name: oc.nadInfo.Prefix + types.OVNLocalnetPort,
	}
	if oc.nadInfo.VlanId != 0 {
		intVlanID := int(oc.nadInfo.VlanId)
		logicalSwitchPort.TagRequest = &intVlanID
	}

	opModels = []libovsdbops.OperationModel{
		{
			Model: &logicalSwitchPort,
			OnModelUpdates: []interface{}{
				&logicalSwitchPort.Addresses,
				&logicalSwitchPort.Type,
				&logicalSwitchPort.TagRequest,
				&logicalSwitchPort.Options,
			},
			DoAfter: func() {
				logicalSwitch.Ports = []string{logicalSwitchPort.UUID}
			},
		},
		{
			Name:           logicalSwitch.Name,
			Model:          &logicalSwitch,
			ModelPredicate: func(ls *nbdb.LogicalSwitch) bool { return ls.Name == switchName },
			OnModelMutations: []interface{}{
				&logicalSwitch.Ports,
			},
			ErrNotFound: true,
		},
	}
	if _, err := oc.mc.modelClient.CreateOrUpdate(opModels...); err != nil {
		return fmt.Errorf("failed to add logical switch port: %s to switch %s, err: %v", oc.nadInfo.Prefix+types.OVNLocalnetPort, switchName, err)
	}

	var hostSubnets []*net.IPNet
	for _, subnet := range oc.clusterSubnets {
		hostSubnet := subnet.CIDR
		hostSubnets = append(hostSubnets, hostSubnet)
	}
	err := oc.lsManager.AddNode(switchName, logicalSwitch.UUID, hostSubnets)
	if err != nil {
		return fmt.Errorf("failed to initialize localnet switch IP manager for network %s: %v", oc.nadInfo.NetName, err)
	}
	for _, excludeIP := range oc.nadInfo.ExcludeIPs {
		var ipMask net.IPMask
		if excludeIP.To4() != nil {
			ipMask = net.CIDRMask(32, 32)
		} else {
			ipMask = net.CIDRMask(128, 128)
		}

		_ = oc.lsManager.AllocateIPs(switchName, []*net.IPNet{{IP: excludeIP, Mask: ipMask}})
	}
	return nil
}

// deleteLocalnetMaster delete localnet switch for the network
func (oc *Controller) deleteLocalnetMaster() {
	switchName := oc.nadInfo.Prefix + types.OVNLocalnetSwitch
	opModel := libovsdbops.OperationModel{
		ModelPredicate: func(ls *nbdb.LogicalSwitch) bool { return ls.Name == switchName },
		ExistingResult: &[]nbdb.LogicalSwitch{},
	}
	if err := oc.mc.modelClient.Delete(opModel); err != nil {
		klog.Errorf("Failed to delete logical switch %s: %v", switchName, err)
	}
}
