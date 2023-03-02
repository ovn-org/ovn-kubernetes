package ovn

import (
	"fmt"
	"net"
	"strings"
	"sync/atomic"

	"github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/types"
	hotypes "github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/types"
	houtil "github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"

	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	kapi "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
)

// handleHybridOverlayPort reconciles the node's overlay port with OVN.
// It needs to handle the following cases:
//   - no subnet allocated: unset MAC annotation
//   - no MAC annotation, no lsp: configure lsp, set annotation
//   - annotation, no lsp: configure lsp
//   - annotation, lsp: ensure lsp matches annotation
//   - no annotation, lsp: set annotation from lsp
func (oc *DefaultNetworkController) handleHybridOverlayPort(node *kapi.Node, annotator kube.Annotator) error {
	var err error
	var annotationMAC, portMAC net.HardwareAddr
	var drIP net.IP
	portName := util.GetHybridOverlayPortName(node.Name)

	// retrieve mac annotation
	am, annotationOK := node.Annotations[types.HybridOverlayDRMAC]
	if annotationOK {
		annotationMAC, err = net.ParseMAC(am)
		if err != nil {
			klog.Errorf("MAC annotation %s on node %s is invalid, ignoring.", annotationMAC, node.Name)
			annotationOK = false
		}
	}

	// no subnet allocated? unset mac annotation, be done.
	subnets, err := util.ParseNodeHostSubnetAnnotation(node, ovntypes.DefaultNetworkName)
	if subnets == nil || err != nil {
		// No subnet allocated yet; clean up
		klog.V(5).Infof("No subnet allocation yet for %s", node.Name)
		if annotationOK {
			oc.deleteHybridOverlayPort(node)
			annotator.Delete(types.HybridOverlayDRMAC)
		}
		return nil
	}

	// Retrieve port MAC address; if the port isn't set up, portMAC will be nil
	lsp := &nbdb.LogicalSwitchPort{Name: portName}
	lsp, err = libovsdbops.GetLogicalSwitchPort(oc.nbClient, lsp)
	if err == nil {
		portMAC, _, _ = util.ExtractPortAddresses(lsp)
	}

	// compare port configuration to annotation MAC, reconcile as needed
	lspOK := false

	if drIP = net.ParseIP(node.Annotations[hotypes.HybridOverlayDRIP]); drIP == nil {
		return fmt.Errorf("cannot set up hybrid overlay ports, distributed router ip is nil")
	}
	// nothing allocated, allocate mac from HybridOverlayDRIP
	if portMAC == nil && annotationMAC == nil {
		portMAC = util.IPAddrToHWAddr(drIP)
		annotationMAC = portMAC
		klog.V(5).Infof("Allocating MAC %s to node %s", portMAC.String(), node.Name)
	} else if portMAC == nil && annotationMAC != nil { // annotation, no port
		portMAC = annotationMAC
	} else if portMAC != nil && annotationMAC == nil { // port, no annotation
		lspOK = true
		annotationMAC = portMAC
	} else if portMAC != nil && annotationMAC != nil { // port & annotation: anno wins
		if portMAC.String() != annotationMAC.String() {
			klog.V(2).Infof("Warning: node %s lsp %s has mismatching hybrid port mac, correcting", node.Name, portName)
			portMAC = annotationMAC
		} else {
			lspOK = true
		}
	}

	// we need to setup a reroute policy for hybrid overlay subnet
	// this is so hybrid pod -> service -> hybrid endpoint will reroute to the DR IP
	if err := oc.setupHybridLRPolicySharedGw(subnets, node.Name, portMAC, drIP); err != nil {
		return fmt.Errorf("unable to setup Hybrid Subnet Logical Route Policy for node: %s, error: %v",
			node.Name, err)
	}

	if !lspOK {
		klog.Infof("Creating / updating node %s hybrid overlay port with mac %s", node.Name, portMAC.String())

		// create / update lsps
		lsp := nbdb.LogicalSwitchPort{
			Name:      portName,
			Addresses: []string{portMAC.String()},
		}
		sw := nbdb.LogicalSwitch{Name: node.Name}

		err := libovsdbops.CreateOrUpdateLogicalSwitchPortsOnSwitch(oc.nbClient, &sw, &lsp)
		if err != nil {
			return fmt.Errorf("failed to add hybrid overlay port %+v for node %s: %v", lsp, node.Name, err)
		}
		for _, subnet := range subnets {
			if err := util.UpdateNodeSwitchExcludeIPs(oc.nbClient, node.Name, subnet); err != nil {
				return err
			}
		}
	}

	if !annotationOK {
		klog.Infof("Setting node %s hybrid overlay mac annotation to %s", node.Name, annotationMAC.String())
		if err := annotator.Set(types.HybridOverlayDRMAC, portMAC.String()); err != nil {
			return fmt.Errorf("failed to set node %s hybrid overlay DRMAC annotation: %v", node.Name, err)
		}
	}

	return nil
}

func (oc *DefaultNetworkController) deleteHybridOverlayPort(node *kapi.Node) {
	klog.Infof("Removing node %s hybrid overlay port", node.Name)
	portName := util.GetHybridOverlayPortName(node.Name)
	lsp := nbdb.LogicalSwitchPort{Name: portName}
	sw := nbdb.LogicalSwitch{Name: node.Name}
	if err := libovsdbops.DeleteLogicalSwitchPorts(oc.nbClient, &sw, &lsp); err != nil {
		klog.Errorf("Failed deleting hybrind overlay port %s for node %s err: %v", portName, node.Name, err)
	}
}

func (oc *DefaultNetworkController) setupHybridLRPolicySharedGw(nodeSubnets []*net.IPNet, nodeName string, portMac net.HardwareAddr, drIPs net.IP) error {
	klog.Infof("Setting up logical route policy for hybrid subnet on node: %s", nodeName)
	var L3Prefix string
	for _, nodeSubnet := range nodeSubnets {
		if utilnet.IsIPv6CIDR(nodeSubnet) {
			L3Prefix = "ip6"
		} else {
			L3Prefix = "ip4"
		}
		var hybridCIDRs []*net.IPNet
		if len(config.HybridOverlay.ClusterSubnets) > 0 {
			for _, hybridSubnet := range config.HybridOverlay.ClusterSubnets {
				if utilnet.IsIPv6CIDR(hybridSubnet.CIDR) == utilnet.IsIPv6CIDR(nodeSubnet) {
					hybridCIDRs = append(hybridCIDRs, hybridSubnet.CIDR)
					break
				}
			}
		} else {
			nodes, err := oc.kube.GetNodes()
			if err != nil {
				return err
			}
			for _, node := range nodes.Items {
				if subnet, _ := houtil.ParseHybridOverlayHostSubnet(&node); subnet != nil {
					hybridCIDRs = append(hybridCIDRs, subnet)
				}
			}
		}

		for _, hybridCIDR := range hybridCIDRs {
			if utilnet.IsIPv6CIDR(hybridCIDR) != utilnet.IsIPv6CIDR(nodeSubnet) {
				// skip if the IP family is not match
				continue
			}
			drIP := drIPs
			matchStr := fmt.Sprintf(`inport == "%s%s" && %s.dst == %s`,
				ovntypes.RouterToSwitchPrefix, nodeName, L3Prefix, hybridCIDR)

			// Logic route policy to steer packet from pod to hybrid overlay nodes
			logicalRouterPolicy := nbdb.LogicalRouterPolicy{
				Priority: ovntypes.HybridOverlaySubnetPriority,
				ExternalIDs: map[string]string{
					"name": ovntypes.HybridSubnetPrefix + nodeName,
				},
				Action:   nbdb.LogicalRouterPolicyActionReroute,
				Nexthops: []string{drIP.String()},
				Match:    matchStr,
			}

			if err := libovsdbops.CreateOrUpdateLogicalRouterPolicyWithPredicate(oc.nbClient, ovntypes.OVNClusterRouter, &logicalRouterPolicy, func(item *nbdb.LogicalRouterPolicy) bool {
				return item.Priority == logicalRouterPolicy.Priority &&
					item.ExternalIDs["name"] == logicalRouterPolicy.ExternalIDs["name"]
			}, &logicalRouterPolicy.Nexthops, &logicalRouterPolicy.Match, &logicalRouterPolicy.Action); err != nil {
				return fmt.Errorf("failed to add policy route '%s' for host %q on %s , error: %v", matchStr, nodeName, ovntypes.OVNClusterRouter, err)
			}

			logicalPort := ovntypes.RouterToSwitchPrefix + nodeName
			if err := util.CreateMACBinding(oc.sbClient, logicalPort, ovntypes.OVNClusterRouter, portMac, drIP); err != nil {
				return fmt.Errorf("failed to create MAC Binding for hybrid overlay: %v", err)
			}

			// Logic route policy to steer packet from external to nodePort service backed by non-ovnkube pods to hybrid overlay nodes
			gwLRPIfAddrs, err := util.GetLRPAddrs(oc.nbClient, ovntypes.GWRouterToJoinSwitchPrefix+ovntypes.GWRouterPrefix+nodeName)
			if err != nil {
				return err
			}
			gwLRPIfAddr, err := util.MatchFirstIPNetFamily(utilnet.IsIPv6CIDR(hybridCIDR), gwLRPIfAddrs)
			if err != nil {
				return err
			}
			grMatchStr := fmt.Sprintf(`%s.src == %s && %s.dst == %s`,
				L3Prefix, gwLRPIfAddr.IP.String(), L3Prefix, hybridCIDR)
			grLogicalRouterPolicy := nbdb.LogicalRouterPolicy{
				Priority: ovntypes.HybridOverlaySubnetPriority,
				ExternalIDs: map[string]string{
					"name": ovntypes.HybridSubnetPrefix + nodeName + "-gr",
				},
				Action:   nbdb.LogicalRouterPolicyActionReroute,
				Nexthops: []string{drIP.String()},
				Match:    grMatchStr,
			}

			if err := libovsdbops.CreateOrUpdateLogicalRouterPolicyWithPredicate(oc.nbClient, ovntypes.OVNClusterRouter, &grLogicalRouterPolicy, func(item *nbdb.LogicalRouterPolicy) bool {
				return item.Priority == grLogicalRouterPolicy.Priority &&
					item.ExternalIDs["name"] == grLogicalRouterPolicy.ExternalIDs["name"]
			}, &grLogicalRouterPolicy.Nexthops, &grLogicalRouterPolicy.Match, &grLogicalRouterPolicy.Action); err != nil {
				return fmt.Errorf("failed to add policy route '%s' for host %q on %s , error: %v", matchStr, nodeName, ovntypes.OVNClusterRouter, err)
			}
			klog.Infof("Created hybrid overlay logical route policies for node %s", nodeName)

			// Static route to steer packets from external to nodePort service backed by pods on hybrid overlay node.
			// This route is to used for triggering above route policy
			clusterRouterStaticRoutes := nbdb.LogicalRouterStaticRoute{
				IPPrefix: hybridCIDR.String(),
				Nexthop:  drIP.String(),
				ExternalIDs: map[string]string{
					"name": ovntypes.HybridSubnetPrefix + nodeName,
				},
			}
			if err := libovsdbops.CreateOrReplaceLogicalRouterStaticRouteWithPredicate(oc.nbClient,
				ovntypes.OVNClusterRouter, &clusterRouterStaticRoutes,
				func(item *nbdb.LogicalRouterStaticRoute) bool {
					return item.IPPrefix == clusterRouterStaticRoutes.IPPrefix &&
						item.ExternalIDs["name"] == clusterRouterStaticRoutes.ExternalIDs["name"] &&
						libovsdbops.PolicyEqualPredicate(clusterRouterStaticRoutes.Policy, item.Policy)
				}, &clusterRouterStaticRoutes.Nexthop); err != nil {
				return fmt.Errorf("failed to add policy route static '%s %s' for on %s , error: %v",
					clusterRouterStaticRoutes.IPPrefix, clusterRouterStaticRoutes.Nexthop,
					ovntypes.GWRouterPrefix+nodeName, err)
			}
			klog.Infof("Created hybrid overlay logical route static route at cluster router for node %s", nodeName)

			// Static route to steer packets from external to nodePort service backed by pods on hybrid overlay node to cluster router.
			drLRPIfAddrs, err := util.GetLRPAddrs(oc.nbClient, ovntypes.GWRouterToJoinSwitchPrefix+ovntypes.OVNClusterRouter)
			if err != nil {
				return err
			}
			drLRPIfAddr, err := util.MatchFirstIPNetFamily(utilnet.IsIPv6CIDR(hybridCIDR), drLRPIfAddrs)
			if err != nil {
				return fmt.Errorf("failed to match cluster router join interface IPs: %v, err: %v", drLRPIfAddr, err)
			}
			nodeGWRouterStaticRoutes := nbdb.LogicalRouterStaticRoute{
				IPPrefix: hybridCIDR.String(),
				Nexthop:  drLRPIfAddr.IP.String(),
				ExternalIDs: map[string]string{
					"name": ovntypes.HybridSubnetPrefix + nodeName + "-gr",
				},
			}
			if err := libovsdbops.CreateOrReplaceLogicalRouterStaticRouteWithPredicate(oc.nbClient,
				ovntypes.GWRouterPrefix+nodeName, &nodeGWRouterStaticRoutes,
				func(item *nbdb.LogicalRouterStaticRoute) bool {
					return item.IPPrefix == nodeGWRouterStaticRoutes.IPPrefix &&
						item.ExternalIDs["name"] == nodeGWRouterStaticRoutes.ExternalIDs["name"] &&
						libovsdbops.PolicyEqualPredicate(nodeGWRouterStaticRoutes.Policy, item.Policy)
				}, &nodeGWRouterStaticRoutes.Nexthop); err != nil {
				return fmt.Errorf("failed to add policy route static '%s %s' for on %s , error: %v",
					nodeGWRouterStaticRoutes.IPPrefix, nodeGWRouterStaticRoutes.Nexthop,
					ovntypes.GWRouterPrefix+nodeName, err)
			}
			klog.Infof("Created hybrid overlay logical route static route at gateway router for node %s", nodeName)
		}
	}
	return nil
}

func (oc *DefaultNetworkController) removeHybridLRPolicySharedGW(nodeName string) error {
	name := ovntypes.HybridSubnetPrefix + nodeName

	if err := libovsdbops.DeleteLogicalRouterPoliciesWithPredicate(oc.nbClient, ovntypes.OVNClusterRouter, func(item *nbdb.LogicalRouterPolicy) bool {
		return item.ExternalIDs["name"] == ovntypes.HybridSubnetPrefix+nodeName || item.ExternalIDs["name"] == ovntypes.HybridSubnetPrefix+nodeName+"-gr"
	}); err != nil {
		return fmt.Errorf("failed to delete policy %s from %s, error: %v", name, ovntypes.OVNClusterRouter, err)
	}

	if err := libovsdbops.DeleteLogicalRouterStaticRoutesWithPredicate(oc.nbClient, ovntypes.OVNClusterRouter, func(item *nbdb.LogicalRouterStaticRoute) bool {
		return item.ExternalIDs["name"] == name
	}); err != nil {
		return fmt.Errorf("failed to delete static route %s from %s, error: %v", name, ovntypes.OVNClusterRouter, err)
	}
	if err := libovsdbops.DeleteLogicalRouterStaticRoutesWithPredicate(oc.nbClient, ovntypes.GWRouterPrefix+nodeName, func(item *nbdb.LogicalRouterStaticRoute) bool {
		return item.ExternalIDs["name"] == name+"-gr"
	}); err != nil {
		return fmt.Errorf("failed to delete static route %s from %s, error: %v", name+"gr", ovntypes.GWRouterPrefix+nodeName, err)
	}
	return nil
}

// takes the node name and allocates the hybrid overlay distributed router ip address
func (oc *DefaultNetworkController) allocateHybridOverlayDRIP(node *kapi.Node) error {
	if atomic.LoadUint32(&oc.allInitialPodsProcessed) == 0 {
		return fmt.Errorf("cannot allocate hybrid overlay distributed router ip for nodes until all initial pods are processed")
	}

	var ips []string
	// check if there is a hybrid distributed router IP set as an annotation
	nodeHybridOverlayDRIP := node.Annotations[hotypes.HybridOverlayDRIP]
	if len(nodeHybridOverlayDRIP) > 0 {
		ips = strings.Split(nodeHybridOverlayDRIP, ",")
	}

	allocatedHybridOverlayDRIPs, err := oc.lsManager.AllocateHybridOverlay(node.Name, ips)
	if err != nil {
		return fmt.Errorf("cannot allocate hybrid overlay interface addresses on node %s: %v", node.Name, err)
	}

	var sliceHybridOverlayDRIP []string
	for _, ip := range allocatedHybridOverlayDRIPs {
		sliceHybridOverlayDRIP = append(sliceHybridOverlayDRIP, ip.IP.String())
	}

	err = oc.kube.SetAnnotationsOnNode(node.Name, map[string]interface{}{hotypes.HybridOverlayDRIP: strings.Join(sliceHybridOverlayDRIP, ",")})
	if err != nil {
		return fmt.Errorf("cannot set hybrid annotation on node %s: %v", node.Name, err)
	}

	return nil
}
