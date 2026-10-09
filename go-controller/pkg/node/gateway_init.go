// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package node

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/vishvananda/netlink"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb"
	ovsops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/managementport"
	nodenft "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/nftables"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/routemanager"
	nodeutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func getGatewayNextHops(ovsClient libovsdbclient.Client) ([]net.IP, string, error) {
	var gatewayNextHops []net.IP
	var needIPv4NextHop bool
	var needIPv6NextHop bool

	if config.IPv4Mode {
		needIPv4NextHop = true
	}
	if config.IPv6Mode {
		needIPv6NextHop = true
	}

	if config.Gateway.NextHop != "" {
		nextHopsRaw := strings.Split(config.Gateway.NextHop, ",")
		if len(nextHopsRaw) > 2 {
			return nil, "", fmt.Errorf("unexpected next-hops are provided, more than 2 next-hops is not allowed: %s", config.Gateway.NextHop)
		}
		for _, nh := range nextHopsRaw {
			// Parse NextHop to make sure it is valid before using. Return error if not valid.
			nextHop := net.ParseIP(nh)
			if nextHop == nil {
				return nil, "", fmt.Errorf("failed to parse configured next-hop: %s", config.Gateway.NextHop)
			}
			if config.IPv4Mode {
				if needIPv4NextHop {
					if !utilnet.IsIPv6(nextHop) {
						gatewayNextHops = append(gatewayNextHops, nextHop)
						needIPv4NextHop = false
					}
				} else {
					if !utilnet.IsIPv6(nextHop) {
						return nil, "", fmt.Errorf("only one IPv4 next-hop is allowed: %s", config.Gateway.NextHop)
					}
				}
			}

			if config.IPv6Mode {
				if needIPv6NextHop {
					if utilnet.IsIPv6(nextHop) {
						gatewayNextHops = append(gatewayNextHops, nextHop)
						needIPv6NextHop = false
					}
				} else {
					if utilnet.IsIPv6(nextHop) {
						return nil, "", fmt.Errorf("only one IPv6 next-hop is allowed: %s", config.Gateway.NextHop)
					}
				}
			}
		}
	}
	gatewayIntf := config.Gateway.Interface
	if gatewayIntf != "" && (config.IsModeDPU() || config.IsModeFull()) {
		if bridge, err := ovsops.GetPortBridge(ovsClient, gatewayIntf); err == nil {
			// This is an OVS bridge's internal port
			gatewayIntf = bridge.Name
		}
	}

	if needIPv4NextHop || needIPv6NextHop || gatewayIntf == "" {
		defaultGatewayIntf, defaultGatewayNextHops, err := getDefaultGatewayInterfaceDetails(gatewayIntf, config.IPv4Mode, config.IPv6Mode)
		if err != nil {
			if !(errors.As(err, new(*GatewayInterfaceMismatchError)) && config.Gateway.Mode == config.GatewayModeLocal && config.Gateway.AllowNoUplink) {
				return nil, "", err
			}
		}
		if gatewayIntf == "" {
			if defaultGatewayIntf == "" {
				return nil, "", fmt.Errorf("unable to find default gateway and none provided via config")
			}
			gatewayIntf = defaultGatewayIntf
		} else {
			if gatewayIntf != defaultGatewayIntf {
				// Mismatch between configured interface and actual default gateway interface detected
				klog.Warningf("Found default gateway interface: %q does not match provided interface from config: %q", defaultGatewayIntf, gatewayIntf)
			} else if len(defaultGatewayNextHops) == 0 {
				// Gateway interface found, but no next hops identified in a default route
				klog.Warning("No default route identified in the host. Egress features may not function correctly! " +
					"Egress Pod traffic in shared gateway mode may not function correctly!")
			}

			if gatewayIntf != defaultGatewayIntf || len(defaultGatewayNextHops) == 0 {
				if config.Gateway.Mode == config.GatewayModeLocal {
					// For local gw, if there is no valid gateway interface found, or no valid nexthops, then
					// use nexthop masquerade IP as GR default gw to steer traffic to the gateway bridge, and then the host for routing
					if needIPv4NextHop {
						nexthop := config.Gateway.MasqueradeIPs.V4DummyNextHopMasqueradeIP
						gatewayNextHops = append(gatewayNextHops, nexthop)
						needIPv4NextHop = false
					}
					if needIPv6NextHop {
						nexthop := config.Gateway.MasqueradeIPs.V6DummyNextHopMasqueradeIP
						gatewayNextHops = append(gatewayNextHops, nexthop)
						needIPv6NextHop = false
					}
				}
			}
		}
		if needIPv4NextHop || needIPv6NextHop {
			for _, defaultGatewayNextHop := range defaultGatewayNextHops {
				if needIPv4NextHop && !utilnet.IsIPv6(defaultGatewayNextHop) {
					gatewayNextHops = append(gatewayNextHops, defaultGatewayNextHop)
				} else if needIPv6NextHop && utilnet.IsIPv6(defaultGatewayNextHop) {
					gatewayNextHops = append(gatewayNextHops, defaultGatewayNextHop)
				}
			}
		}
	}
	return gatewayNextHops, gatewayIntf, nil
}

// getInterfaceByIP retrieves Interface that has `ip` assigned to it
func getInterfaceByIP(ip net.IP) (string, error) {
	links, err := util.GetNetLinkOps().LinkList()
	if err != nil {
		return "", fmt.Errorf("failed to list network devices in the system. %v", err)
	}

	for _, link := range links {
		ips, err := util.GetFilteredInterfaceV4V6IPs(link.Attrs().Name)
		if err != nil {
			return "", err
		}
		for _, netdevIp := range ips {
			if netdevIp.Contains(ip) {
				return link.Attrs().Name, nil
			}
		}
	}
	return "", fmt.Errorf("failed to find network interface with IP: %s", ip)
}

// configureSvcRouteViaInterface routes svc traffic through the provided interface
func configureSvcRouteViaInterface(routeManager *routemanager.Controller, iface string, gwIPs []net.IP) error {
	link, err := util.LinkSetUp(iface)
	if err != nil {
		return fmt.Errorf("unable to get link for %s, error: %v", iface, err)
	}

	for _, subnet := range config.Kubernetes.ServiceCIDRs {
		isV6 := utilnet.IsIPv6CIDR(subnet)
		gwIP, err := util.MatchIPFamily(isV6, gwIPs)
		if err != nil {
			return fmt.Errorf("unable to find gateway IP for subnet: %v, found IPs: %v", subnet, gwIPs)
		}
		srcIP := config.Gateway.MasqueradeIPs.V4HostMasqueradeIP
		if isV6 {
			srcIP = config.Gateway.MasqueradeIPs.V6HostMasqueradeIP
		}
		// Remove MTU from service route once https://bugzilla.redhat.com/show_bug.cgi?id=2169839 is fixed.
		mtu := config.Default.MTU
		if config.Default.RoutableMTU != 0 {
			mtu = config.Default.RoutableMTU
		}
		subnetCopy := *subnet
		gwIPCopy := gwIP[0]
		err = routeManager.Add(netlink.Route{LinkIndex: link.Attrs().Index, Gw: gwIPCopy, Dst: &subnetCopy, Src: srcIP, MTU: mtu})
		if err != nil {
			return fmt.Errorf("unable to add gateway IP route for subnet: %v, %v", subnet, err)
		}
	}
	return nil
}

// getNodePrimaryIfAddrs returns the appropriate interface addresses based on the node mode
func getNodePrimaryIfAddrs(watchFactory factory.NodeWatchFactory, nodeName string, gatewayIntf string) ([]*net.IPNet, error) {
	switch config.OvnKubeNode.Mode {
	case types.NodeModeDPU:
		// For DPU mode, use the host IP address from node annotation
		node, err := watchFactory.GetNode(nodeName)
		if err != nil {
			return nil, fmt.Errorf("error retrieving node %s: %v", nodeName, err)
		}

		// Extract the primary DPU address annotation from the node
		nodeIfAddr, err := util.GetNodePrimaryDPUHostAddrAnnotation(node)
		if err != nil {
			return nil, err
		}

		if nodeIfAddr.IPv4 == "" {
			return nil, fmt.Errorf("node primary DPU address annotation is empty for node %s", nodeName)
		}

		nodeIP, nodeAddrs, err := net.ParseCIDR(nodeIfAddr.IPv4)
		if err != nil {
			return nil, fmt.Errorf("failed to parse node IP address %s: %v", nodeIfAddr.IPv4, err)
		}

		nodeAddrs.IP = nodeIP
		return []*net.IPNet{nodeAddrs}, nil
	default:
		// For other modes, get network interface IP addresses directly
		return nodeutil.GetNetworkInterfaceIPAddresses(gatewayIntf)
	}
}

// initGatewayPreStart executes the first part of the gateway initialization for the node.
// It creates the gateway object, the node IP manager, openflow manager and node port watcher
// once OVN controller is ready and the patch port exists for this node.
// It is split from initGatewayMainStart to allow for the gateway object and openflow manager to be created
// before the rest of the gateway functionality is started.
func (nc *DefaultNodeNetworkController) initGatewayPreStart(
	subnets []*net.IPNet,
	nodeAnnotator kube.Annotator,
	mgmtPort managementport.Interface,
) (*gateway, error) {

	klog.Info("Initializing Gateway Functionality for Gateway PreStart")
	var err error
	var ifAddrs []*net.IPNet

	waiter := newStartupWaiter()

	gatewayNextHops, gatewayIntf, err := getGatewayNextHops(nc.ovsClient)
	if err != nil {
		return nil, err
	}

	egressGWInterface := ""
	if config.Gateway.EgressGWInterface != "" {
		egressGWInterface, err = interfaceForEXGW(nc.ovsClient, config.Gateway.EgressGWInterface)
		if err != nil {
			return nil, err
		}
	}

	// Get interface addresses based on node mode
	ifAddrs, err = getNodePrimaryIfAddrs(nc.watchFactory, nc.name, gatewayIntf)
	if err != nil {
		return nil, err
	}

	if err := util.SetNodePrimaryIfAddrs(nodeAnnotator, ifAddrs); err != nil {
		klog.Errorf("Unable to set primary IP net label on node, err: %v", err)
	}

	var gw *gateway
	switch config.Gateway.Mode {
	case config.GatewayModeLocal, config.GatewayModeShared:
		klog.Info("Preparing Gateway")
		gw, err = newGateway(
			nc.name,
			subnets,
			gatewayNextHops,
			gatewayIntf,
			egressGWInterface,
			ifAddrs,
			nodeAnnotator,
			mgmtPort,
			nc.Kube,
			nc.watchFactory,
			nc.routeManager,
			nc.linkManager,
			nc.networkManager,
			config.Gateway.Mode,
			nc.ovsClient,
		)
	case config.GatewayModeDisabled:
		var chassisID string
		klog.Info("Gateway Mode is disabled")
		gw = &gateway{
			initFunc:     func() error { return nil },
			readyFunc:    func() (bool, error) { return true, nil },
			watchFactory: nc.watchFactory.(*factory.WatchFactory),
		}
		chassisID, err = util.GetNodeChassisID()
		if err != nil {
			return nil, err
		}
		err = util.SetL3GatewayConfig(nodeAnnotator, &util.L3GatewayConfig{
			Mode:      config.GatewayModeDisabled,
			ChassisID: chassisID,
		})
	}
	if err != nil {
		return nil, err
	}

	initGwFunc := func() error {
		return gw.initFunc()
	}

	readyGwFunc := func() (bool, error) {
		controllerReady, err := isOVNControllerReady(nc.ovsClient)
		if err != nil || !controllerReady {
			return false, err
		}
		return gw.readyFunc()
	}

	if err := nodeAnnotator.Run(); err != nil {
		return nil, fmt.Errorf("failed to set node %s annotations: %w", nc.name, err)
	}

	waiter.AddWait(readyGwFunc, initGwFunc)
	nc.Gateway = gw

	// Wait for management port and gateway resources to be created by the master
	start := time.Now()
	if err := waiter.Wait(); err != nil {
		return nil, err
	}
	klog.Infof("Gateway and management port readiness took %v", time.Since(start))

	return gw, nil
}

// initGatewayMainStart finishes the gateway initialization for the node: it initializes the
// LB health checker and port claim watcher; it starts watching for events on services and endpoint slices,
// so that LB health checker, port claim watcher, node port watcher and node port watcher ip tables can
// react to those events.
func (nc *DefaultNodeNetworkController) initGatewayMainStart(gw *gateway, waiter *startupWaiter) error {
	klog.Info("Initializing Gateway Functionality for gateway Start")

	var loadBalancerHealthChecker *loadBalancerHealthChecker
	var portClaimWatcher *portClaimWatcher

	var err error
	if config.Gateway.NodeportEnable && config.IsModeFull() {
		loadBalancerHealthChecker = newLoadBalancerHealthChecker(nc.name, nc.watchFactory)
		portClaimWatcher, err = newPortClaimWatcher(nc.recorder)
		if err != nil {
			return err
		}
	}

	if loadBalancerHealthChecker != nil {
		gw.loadBalancerHealthChecker = loadBalancerHealthChecker
	}
	if portClaimWatcher != nil {
		gw.portClaimWatcher = portClaimWatcher
	}

	initGwFunc := func() error {
		return gw.Init(nc.stopChan, nc.wg)
	}

	readyGwFunc := func() (bool, error) {
		return true, nil
	}
	waiter.AddWait(readyGwFunc, initGwFunc)
	nc.Gateway = gw

	return nc.validateVTEPInterfaceMTU()
}

// interfaceForEXGW takes the interface requested to act as exgw bridge
// and returns the name of the bridge if exists, or the interface itself
// if the bridge needs to be created. In this last scenario, BridgeForInterface
// will create the bridge.
func interfaceForEXGW(ovsClient libovsdbclient.Client, intfName string) (string, error) {
	if _, err := ovsops.GetBridge(ovsClient, intfName); err == nil {
		// It's a bridge
		return intfName, nil
	} else if !errors.Is(err, libovsdbclient.ErrNotFound) {
		return "", fmt.Errorf("failed to check whether %s is an OVS bridge: %w", intfName, err)
	}

	bridge := util.GetBridgeName(intfName)
	if _, err := ovsops.GetBridge(ovsClient, bridge); err == nil {
		// not a bridge, but the corresponding bridge was already created
		return bridge, nil
	} else if !errors.Is(err, libovsdbclient.ErrNotFound) {
		return "", fmt.Errorf("failed to check whether OVS bridge %s exists for interface %s: %w", bridge, intfName, err)
	}
	return intfName, nil
}

// TODO(adrianc): revisit if support for nodeIPManager is needed.
func (nc *DefaultNodeNetworkController) initGatewayDPUHostPreStart(kubeNodeIP net.IP, nodeAnnotator kube.Annotator) error {
	klog.Info("Initializing Shared Gateway Functionality for Gateway PreStart on DPU host")

	// Find the network interface that has the Kubernetes node IP assigned to it
	// This interface will be used for DPU host gateway operations
	kubeIntf, err := getInterfaceByIP(kubeNodeIP)
	if err != nil {
		return err
	}

	// Get all IP addresses (IPv4 and IPv6) configured on the detected interface
	ifAddrs, err := nodeutil.GetNetworkInterfaceIPAddresses(kubeIntf)
	if err != nil {
		return err
	}

	// Extract the IPv4 address from the interface addresses for node annotation
	nodeIPNet, _ := util.MatchFirstIPNetFamily(false, ifAddrs)
	nodeAddrSet := sets.New[string](nodeIPNet.String())

	// If no gateway interface is explicitly configured, use the detected interface
	if config.Gateway.Interface == "" {
		config.Gateway.Interface = kubeIntf
	}

	// If a different gateway interface is configured than the one with used for the kubernetes node IP,
	// get its addresses and add them to the node address set for routing purposes
	if config.Gateway.Interface != kubeIntf {
		ifAddrs, err = nodeutil.GetNetworkInterfaceIPAddresses(config.Gateway.Interface)
		if err != nil {
			return err
		}
		detectedIPNetv4, _ := util.MatchFirstIPNetFamily(false, ifAddrs)
		nodeAddrSet.Insert(detectedIPNetv4.String())
		// Use the configured interface for the masquerade route instead of the auto-detected one
		kubeIntf = config.Gateway.Interface
	}

	// Set the primary DPU address annotation on the node with the interface addresses
	if err := util.SetNodePrimaryDPUHostAddr(nodeAnnotator, ifAddrs); err != nil {
		klog.Errorf("Unable to set primary IP net label on node, err: %v", err)
		return err
	}

	// Set the host CIDRs annotation to include all detected network addresses
	// This helps with routing decisions for traffic coming from the host
	if err := util.SetNodeHostCIDRs(nodeAnnotator, nodeAddrSet); err != nil {
		klog.Errorf("Unable to set host-cidrs on node, err: %v", err)
		return err
	}

	// Apply all node annotations to the Kubernetes node object
	if err := nodeAnnotator.Run(); err != nil {
		return fmt.Errorf("failed to set node %s annotations: %w", nc.name, err)
	}

	// Delete stale masquerade resources if there are any. This is to make sure that there
	// are no Linux resources with IP from old masquerade subnet when masquerade subnet
	// gets changed as part of day2 operation.
	if err := deleteStaleMasqueradeResources(kubeIntf, nc.name, nc.watchFactory); err != nil {
		return fmt.Errorf("failed to remove stale masquerade resources: %w", err)
	}

	if err := nc.masqReconciler.ensure(); err != nil {
		return err
	}

	// Masquerade config mostly done on node, update annotation
	if err := updateMasqueradeAnnotation(nc.name, nc.Kube); err != nil {
		return fmt.Errorf("failed to update masquerade subnet annotation on node: %s, error: %v", nc.name, err)
	}

	gatewayNextHops, _, err := getGatewayNextHops(nc.ovsClient)
	if err != nil {
		return err
	}

	// In DPU-host mode, bridgeEIPAddrManager is not initialized because:
	// - There's no OVS on the host (it runs on the DPU)
	// - Traffic is handled on the DPU which has the EgressIP configuration
	// - There's no openflow manager to use the mark-to-IP cache
	nc.Gateway = &gateway{
		initFunc:     func() error { return nil },
		readyFunc:    func() (bool, error) { return true, nil },
		watchFactory: nc.watchFactory.(*factory.WatchFactory),
		nextHops:     gatewayNextHops,
	}
	return nil
}

func (nc *DefaultNodeNetworkController) initGatewayDPUHost() error {
	// A DPU host gateway is complementary to the shared gateway running
	// on the DPU embedded CPU. it performs some initializations and
	// watch on services for nftables rule updates and run a loadBalancerHealth checker
	// Note: all K8s Node related annotations are handled from DPU.
	klog.Info("Initializing Shared Gateway Functionality for Gateway Start on DPU host")
	var err error

	gw := nc.Gateway.(*gateway)
	gw.nodeIPManager = newAddressManager(nc.name, nc.Kube, nil, nc.watchFactory, nil, nc.ovsClient)
	if config.Gateway.NodeportEnable {
		if err := initGatewayNFTables(); err != nil {
			return err
		}
		if util.IsNetworkSegmentationSupportEnabled() {
			if err := configureUDNServicesNFTables(); err != nil {
				return fmt.Errorf("unable to configure UDN nftables: %w", err)
			}
		}
		gw.nodePortWatcherNFTables = newNodePortWatcherNFTables(nc.networkManager)
		gw.loadBalancerHealthChecker = newLoadBalancerHealthChecker(nc.name, nc.watchFactory)
		portClaimWatcher, err := newPortClaimWatcher(nc.recorder)
		if err != nil {
			return err
		}
		gw.portClaimWatcher = portClaimWatcher
	}

	err = gw.Init(nc.stopChan, nc.wg)
	nc.Gateway = gw
	return err
}

// CleanupClusterNode cleans up OVS resources on the k8s node on ovnkube-node
// daemonset deletion. Best-effort: OVS-side cleanup is skipped on DPU-host
// (no OVS to clean) or when the OVSDB client cannot be created; iptables
// and nftables cleanup runs only on DPU-host or full nodes (the DPU has
// no host-side rules to clean).
func CleanupClusterNode(ctx context.Context, name string) error {
	klog.V(5).Infof("Cleaning up gateway resources on node: %q", name)

	var ovsClient libovsdbclient.Client
	if config.IsModeDPU() || config.IsModeFull() {
		// NewOVSClient closes its client when the stop channel fires; use a
		// dedicated never-shared channel so an in-flight cleanup transaction
		// can't be torn down mid-operation. Close on return so the libovsdb
		// lifecycle goroutine exits cleanly.
		cleanupStopCh := make(chan struct{})
		defer close(cleanupStopCh)
		c, err := libovsdb.NewOVSClient(cleanupStopCh)
		if err != nil {
			klog.Warningf("Skipping OVS-side cleanup: failed to initialize libovsdb vswitchd client: %v", err)
		} else {
			ovsClient = c
		}
	}

	var err error
	if config.Gateway.Mode == config.GatewayModeLocal || config.Gateway.Mode == config.GatewayModeShared {
		if ovsClient != nil {
			if localErr := cleanupLocalnetGateway(ovsClient, types.LocalNetworkName); localErr != nil {
				klog.Errorf("Failed to cleanup Localnet Gateway, error: %v", localErr)
				err = localErr
			}
		}
		// cleanupSharedGateway handles both the OVS-side (no-op when ovsClient
		// is nil) and the host-side nftables chains.
		if sharedErr := cleanupSharedGateway(ctx, ovsClient); sharedErr != nil {
			klog.Errorf("Failed to cleanup Gateway, error: %v", sharedErr)
			err = sharedErr
		}
	}

	// Only strip ovn-bridge-mappings once gateway cleanup has fully
	// succeeded — otherwise we leave bridges/flows behind without the
	// external_ids that would let a retry re-attach them.
	if err == nil && ovsClient != nil && (config.IsModeDPU() || config.IsModeFull()) {
		if err := ovsops.RemoveOpenvSwitchExternalIDs(ovsClient, "ovn-bridge-mappings"); err != nil {
			klog.Errorf("Failed to delete ovn-bridge-mappings: %v", err)
		}
	}

	if config.IsModeDPUHost() || config.IsModeFull() {
		// Clean up legacy IPTables rules for management port
		managementport.DelLegacyMgtPortIptRules()

		// Delete nftables rules
		nodenft.CleanupNFTables()
	}

	return nil
}

func (nc *DefaultNodeNetworkController) updateGatewayMAC(link netlink.Link) error {
	// TBD-merge for dpu-host mode: if interface mac of the dpu-host interface that connects to the
	// gateway bridge on the dpu changes, we need to update dpu's gatewayBridge.macAddress L3 gateway
	// annotation (see BridgeForInterface)
	if config.IsModeDPU() || config.IsModeDPUHost() {
		return nil
	}

	if nc.Gateway.GetGatewayIface() != link.Attrs().Name {
		return nil
	}

	node, err := nc.watchFactory.GetNode(nc.name)
	if err != nil {
		return err
	}
	l3gwConf, err := util.ParseNodeL3GatewayAnnotation(node)
	if err != nil {
		return err
	}

	if l3gwConf == nil || l3gwConf.MACAddress.String() == link.Attrs().HardwareAddr.String() {
		return nil
	}
	// MAC must have changed, update node
	nc.Gateway.SetDefaultGatewayBridgeMAC(link.Attrs().HardwareAddr)
	if err := nc.Gateway.Reconcile(); err != nil {
		return fmt.Errorf("failed to reconcile gateway for MAC address update: %w", err)
	}
	nodeAnnotator := kube.NewNodeAnnotator(nc.Kube, node.Name)
	l3gwConf.MACAddress = link.Attrs().HardwareAddr
	if err := util.SetL3GatewayConfig(nodeAnnotator, l3gwConf); err != nil {
		return fmt.Errorf("failed to update L3 gateway config annotation for node: %s, error: %w", node.Name, err)
	}
	if err := nodeAnnotator.Run(); err != nil {
		return fmt.Errorf("failed to set node %s annotations: %w", nc.name, err)
	}

	return nil

}
