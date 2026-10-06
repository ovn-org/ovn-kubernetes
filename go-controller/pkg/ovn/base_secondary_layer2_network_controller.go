// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"fmt"
	"net"

	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	zoneinterconnect "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/zone_interconnect"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// method/structure shared by all layer 2 network controller, including localnet and layer2 network controllres.

// BaseLayer2UserDefinedNetworkController structure holds per-network fields and network specific
// configuration for secondary layer2/localnet network controller
type BaseLayer2UserDefinedNetworkController struct {
	BaseUserDefinedNetworkController
}

// stop gracefully stops the controller, and delete all logical entities for this network if requested
func (oc *BaseLayer2UserDefinedNetworkController) stop() {
	if oc.stopChan == nil {
		klog.Infof("Secondary %s network controller of network %s is already stopped", oc.TopologyType(), oc.GetNetworkName())
		return
	}
	klog.Infof("Stop secondary %s network controller of network %s", oc.TopologyType(), oc.GetNetworkName())
	close(oc.stopChan)
	oc.stopChan = nil
	oc.cancelableCtx.Cancel()
	oc.wg.Wait()

	if oc.netPolicyHandler != nil {
		oc.watchFactory.RemovePolicyHandler(oc.netPolicyHandler)
	}
	if oc.multiNetPolicyHandler != nil {
		oc.watchFactory.RemoveMultiNetworkPolicyHandler(oc.multiNetPolicyHandler)
	}
	if oc.podHandler != nil {
		oc.watchFactory.RemovePodHandler(oc.podHandler)
	}
	if oc.namespaceHandler != nil {
		oc.watchFactory.RemoveNamespaceHandler(oc.namespaceHandler)
	}
	if oc.routeImportManager != nil && config.Gateway.Mode == config.GatewayModeShared {
		oc.routeImportManager.ForgetNetwork(oc.GetNetworkName())
	}
}

// cleanup cleans up logical entities for the given network, called from net-attach-def routine
// could be called from a dummy Controller (only has CommonNetworkControllerInfo set)
func (oc *BaseLayer2UserDefinedNetworkController) cleanup() error {
	netName := oc.GetNetworkName()
	klog.Infof("Delete OVN logical entities for network %s", netName)

	// delete layer 2 logical switches
	ops, err := libovsdbops.DeleteLogicalSwitchesWithPredicateOps(oc.nbClient, nil,
		func(item *nbdb.LogicalSwitch) bool {
			return item.ExternalIDs[types.NetworkExternalID] == netName
		})
	if err != nil {
		return fmt.Errorf("failed to get ops for deleting switches of network %s: %v", netName, err)
	}

	ops, err = cleanupPolicyLogicalEntities(oc.nbClient, ops, oc.controllerName)
	if err != nil {
		return err
	}

	// cleanup address sets after ACLs are deleted to avoid ovn-controller errors
	if oc.addressSetManager != nil {
		if err := oc.addressSetManager.CleanupForController(oc.controllerName); err != nil {
			return fmt.Errorf("failed to cleanup address sets for controller %s: %v", oc.controllerName, err)
		}
	}

	ops, err = libovsdbops.DeleteQoSesWithPredicateOps(oc.nbClient, ops,
		func(item *nbdb.QoS) bool {
			return item.ExternalIDs[types.NetworkExternalID] == netName
		})
	if err != nil {
		return fmt.Errorf("failed to get ops for deleting QoSes of network %s: %v", netName, err)
	}

	ops, err = libovsdbops.DeleteAddressSetsWithPredicateOps(oc.nbClient, ops,
		func(item *nbdb.AddressSet) bool {
			return item.ExternalIDs[types.NetworkExternalID] == netName
		})
	if err != nil {
		return fmt.Errorf("failed to get ops for deleting address sets of network %s: %v", netName, err)
	}

	_, err = libovsdbops.TransactAndCheck(oc.nbClient, ops)
	if err != nil {
		return fmt.Errorf("failed to deleting switches of network %s: %v", netName, err)
	}

	return nil
}

func (oc *BaseLayer2UserDefinedNetworkController) run() error {
	// WatchNamespaces() should be started first because it has no other
	// dependencies.
	if err := oc.WatchNamespaces(); err != nil {
		return err
	}

	if err := oc.WatchPods(); err != nil {
		return err
	}

	if util.IsMultiNetworkPoliciesSupportEnabled() && !oc.IsPrimaryNetwork() {
		// WatchMultiNetworkPolicy depends on WatchPods and WatchNamespaces
		if err := oc.WatchMultiNetworkPolicy(); err != nil {
			return err
		}
	}

	if oc.IsPrimaryNetwork() {
		// WatchNetworkPolicy depends on WatchPods and WatchNamespaces
		if err := oc.WatchNetworkPolicy(); err != nil {
			return err
		}
	}

	// start NetworkQoS controller if feature is enabled
	if config.OVNKubernetesFeature.EnableNetworkQoS {
		err := oc.newNetworkQoSController()
		if err != nil {
			return fmt.Errorf("unable to create network qos controller, err: %w", err)
		}
		oc.wg.Add(1)
		go func(ch <-chan struct{}) {
			defer oc.wg.Done()
			// Until we have scale issues in future let's spawn only one thread
			oc.nqosController.Run(1, ch)
		}(oc.stopChan)
	}

	// Add ourselves to the route import manager
	if oc.routeImportManager != nil && config.Gateway.Mode == config.GatewayModeShared {
		err := oc.routeImportManager.AddNetwork(oc.GetNetInfo())
		if err != nil {
			return fmt.Errorf("failed to add network %s to the route import manager: %v", oc.GetNetworkName(), err)
		}
	}
	return nil
}

func (oc *BaseLayer2UserDefinedNetworkController) initializeLogicalSwitch(switchName string, clusterSubnets []config.CIDRNetworkEntry, excludeSubnets, reservedSubnets []*net.IPNet, clusterLoadBalancerGroupUUID, switchLoadBalancerGroupUUID string) (*nbdb.LogicalSwitch, error) {
	logicalSwitch := nbdb.LogicalSwitch{
		Name:        switchName,
		ExternalIDs: util.GenerateExternalIDsForSwitchOrRouter(oc.GetNetInfo()),
		OtherConfig: map[string]string{},
	}

	hostSubnets := make([]*net.IPNet, 0, len(clusterSubnets))
	// might use these later
	var nodeLRPMAC net.HardwareAddr
	var gwIfAddrv4, gwIfAddrv6 *net.IPNet
	for _, clusterSubnet := range clusterSubnets {
		subnet := clusterSubnet.CIDR
		hostSubnets = append(hostSubnets, subnet)
		if utilnet.IsIPv6CIDR(subnet) {
			logicalSwitch.OtherConfig["ipv6_prefix"] = subnet.IP.String()
			gwIfAddrv6 = oc.GetNodeGatewayIP(subnet)
			if len(nodeLRPMAC) == 0 {
				// only derive mac from IPv6 if there is no IPv4
				nodeLRPMAC = util.IPAddrToHWAddr(gwIfAddrv6.IP)
			}
		} else {
			logicalSwitch.OtherConfig["subnet"] = subnet.String()
			gwIfAddrv4 = oc.GetNodeGatewayIP(subnet)
			nodeLRPMAC = util.IPAddrToHWAddr(gwIfAddrv4.IP)
		}
	}

	var lsps []*nbdb.LogicalSwitchPort
	var acls []*nbdb.ACL
	switch {
	case oc.isLayer2WithInterconnectTransport():
		tunnelKey := zoneinterconnect.BaseTransitSwitchTunnelKey + oc.GetNetworkID()
		if config.Layer2UsesTransitRouter && oc.IsPrimaryNetwork() {
			if len(oc.GetTunnelKeys()) != 2 {
				return nil, fmt.Errorf("layer2 network %s with transit router enabled requires exactly 2 tunnel keys, got: %v", oc.GetNetworkName(), oc.GetTunnelKeys())
			}
			tunnelKey = oc.GetTunnelKeys()[0]
		}
		err := oc.zoneICHandler.AddTransitSwitchConfig(&logicalSwitch, tunnelKey)
		if err != nil {
			return nil, err
		}
	case oc.Transport() == types.NetworkTransportEVPN:
		// enable IGMP snooping to send multicast traffic just to registered
		// pods, flood unregistered
		logicalSwitch.OtherConfig["mcast_snoop"] = "true"
		logicalSwitch.OtherConfig["mcast_flood_unregistered"] = "true"
		logicalSwitch.OtherConfig["mcast_querier"] = "false"
		// connect the switch to the EVPN macvrf
		macvrfportName := util.GetMACVRFPortName(switchName)
		macvrfport := &nbdb.LogicalSwitchPort{
			Name:      macvrfportName,
			Addresses: []string{"unknown"},
			ExternalIDs: map[string]string{
				types.NetworkExternalID:  oc.GetNetworkName(),
				types.TopologyExternalID: oc.TopologyType(),
			},
		}
		lsps = append(lsps, macvrfport)
		acls = getDenyARPAndNSOnMACVRF(oc.controllerName, macvrfportName, nodeLRPMAC, gwIfAddrv4, gwIfAddrv6)
	}

	if clusterLoadBalancerGroupUUID != "" && switchLoadBalancerGroupUUID != "" {
		logicalSwitch.LoadBalancerGroup = []string{clusterLoadBalancerGroupUUID, switchLoadBalancerGroupUUID}
	}

	ops, err := libovsdbops.CreateOrUpdateACLsOps(oc.nbClient, nil, oc.GetSamplingConfig(), acls...)
	if err != nil {
		return nil, fmt.Errorf("failed to create ACLs: %w", err)
	}

	for _, acl := range acls {
		logicalSwitch.ACLs = append(logicalSwitch.ACLs, acl.UUID)
	}

	ops, err = libovsdbops.CreateOrUpdateLogicalSwitchPortsAndSwitchOps(oc.nbClient, ops, &logicalSwitch, lsps...)
	if err != nil {
		return nil, fmt.Errorf("failed to create logical switch %+v: %v", logicalSwitch, err)
	}

	// Add the MACVRF port to ClusterRtrPortGroupNameBase so the
	// higher-priority AllowInterNode multicast ACL overrides the
	// default deny multicast ACL and permits multicast traffic
	// to/from the EVPN fabric.
	if oc.multicastSupport && oc.Transport() == types.NetworkTransportEVPN {
		for _, lsp := range lsps {
			if lsp.Name == util.GetMACVRFPortName(switchName) {
				ops, err = libovsdbops.AddPortsToPortGroupOps(oc.nbClient, ops,
					oc.getClusterPortGroupName(types.ClusterRtrPortGroupNameBase), lsp.UUID)
				if err != nil {
					return nil, fmt.Errorf("failed to create ops to add MACVRF port to router port group: %w", err)
				}
				break
			}
		}
	}

	_, err = libovsdbops.TransactAndCheckAndSetUUIDs(oc.nbClient, lsps, ops)
	if err != nil {
		return nil, fmt.Errorf("failed to transact logical switch operations: %w", err)
	}

	if err = oc.lsManager.AddOrUpdateSwitch(switchName, hostSubnets, reservedSubnets, nil, excludeSubnets...); err != nil {
		return nil, err
	}

	return &logicalSwitch, nil
}

// getDenyARPAndNSOnMACVRF provides ACLs to drop ARP and NS from pods to the
// gateway IP on the MACVRF port. Even though these requests are unicast, OVN is
// flooding them for historic reasons. We don't want these request to be flooded
// over the EVPN overlay.
func getDenyARPAndNSOnMACVRF(controllerName, macvrfportName string, nodeLRPMAC net.HardwareAddr, gwIfAddrv4, gwIfAddrv6 *net.IPNet) []*nbdb.ACL {
	var acls []*nbdb.ACL
	if gwIfAddrv4 != nil {
		acls = append(acls, libovsdbutil.BuildACLWithDefaultTier(
			libovsdbops.NewDbObjectIDs(
				libovsdbops.ACLUDN,
				controllerName,
				map[libovsdbops.ExternalIDKey]string{
					libovsdbops.ObjectNameKey:      "DenyOnMACVRF-GatewayARP",
					libovsdbops.PolicyDirectionKey: string(libovsdbutil.ACLIngress),
				},
			),
			types.DefaultDenyPriority,
			fmt.Sprintf(
				"outport==%q && eth.dst==%s && arp && arp.op==1 && arp.tpa==%s",
				macvrfportName,
				nodeLRPMAC.String(),
				gwIfAddrv4.IP.String(),
			),
			nbdb.ACLActionDrop,
			nil,
			libovsdbutil.LportIngress,
		))
	}
	if gwIfAddrv6 != nil {
		acls = append(acls, libovsdbutil.BuildACLWithDefaultTier(
			libovsdbops.NewDbObjectIDs(
				libovsdbops.ACLUDN,
				controllerName,
				map[libovsdbops.ExternalIDKey]string{
					libovsdbops.ObjectNameKey:      "DenyOnMACVRF-GatewayNS",
					libovsdbops.PolicyDirectionKey: string(libovsdbutil.ACLIngress),
				},
			),
			types.DefaultDenyPriority,
			fmt.Sprintf(
				"outport==%q && eth.dst==%s && nd && icmp.type==135 && nd.target==%s",
				macvrfportName,
				nodeLRPMAC.String(),
				gwIfAddrv6.IP.String(),
			),
			nbdb.ACLActionDrop,
			nil,
			libovsdbutil.LportIngress,
		))
	}
	return acls
}
