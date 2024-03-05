package node

import (
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/vishvananda/netlink"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/routemanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	util "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

// bridgedGatewayNodeSetup enables forwarding on bridge interface, sets up the physical network name mappings for the bridge,
// and returns an ifaceID created from the bridge name and the node name
func bridgedGatewayNodeSetup(nodeName, bridgeName, physicalNetworkName string) (string, error) {
	// enable forwarding on bridge interface always
	createForwardingRule := func(family string) error {
		stdout, stderr, err := util.RunSysctl("-w", fmt.Sprintf("net.%s.conf.%s.forwarding=1", family, bridgeName))
		if err != nil || stdout != fmt.Sprintf("net.%s.conf.%s.forwarding = 1", family, bridgeName) {
			return fmt.Errorf("could not set the correct forwarding value for interface %s: stdout: %v, stderr: %v, err: %v",
				bridgeName, stdout, stderr, err)
		}
		return nil
	}
	if config.IPv4Mode {
		if err := createForwardingRule("ipv4"); err != nil {
			return "", fmt.Errorf("could not add IPv4 forwarding rule: %v", err)
		}
	}
	if config.IPv6Mode {
		if err := createForwardingRule("ipv6"); err != nil {
			return "", fmt.Errorf("could not add IPv6 forwarding rule: %v", err)
		}
	}

	// ovn-bridge-mappings maps a physical network name to a local ovs bridge
	// that provides connectivity to that network. It is in the form of physnet1:br1,physnet2:br2.
	// Note that there may be multiple ovs bridge mappings, be sure not to override
	// the mappings for the other physical network
	stdout, stderr, err := util.RunOVSVsctl("--if-exists", "get", "Open_vSwitch", ".",
		"external_ids:ovn-bridge-mappings")
	if err != nil {
		return "", fmt.Errorf("failed to get ovn-bridge-mappings stderr:%s (%v)", stderr, err)
	}
	// skip the existing mapping setting for the specified physicalNetworkName
	mapString := ""
	bridgeMappings := strings.Split(stdout, ",")
	for _, bridgeMapping := range bridgeMappings {
		m := strings.Split(bridgeMapping, ":")
		if network := m[0]; network != physicalNetworkName {
			if len(mapString) != 0 {
				mapString += ","
			}
			mapString += bridgeMapping
		}
	}
	if len(mapString) != 0 {
		mapString += ","
	}
	mapString += physicalNetworkName + ":" + bridgeName

	_, stderr, err = util.RunOVSVsctl("set", "Open_vSwitch", ".",
		fmt.Sprintf("external_ids:ovn-bridge-mappings=%s", mapString))
	if err != nil {
		return "", fmt.Errorf("failed to set ovn-bridge-mappings for ovs bridge %s"+
			", stderr:%s (%v)", bridgeName, stderr, err)
	}

	ifaceID := bridgeName + "_" + nodeName
	return ifaceID, nil
}

// getNetworkInterfaceIPAddresses returns the IP addresses for the network interface 'iface'.
func getNetworkInterfaceIPAddresses(iface string) ([]*net.IPNet, error) {
	allIPs, err := util.GetFilteredInterfaceV4V6IPs(iface)
	if err != nil {
		return nil, fmt.Errorf("could not find IP addresses: %v", err)
	}

	var ips []*net.IPNet
	var foundIPv4 bool
	var foundIPv6 bool
	for _, ip := range allIPs {
		if utilnet.IsIPv6CIDR(ip) {
			if config.IPv6Mode && !foundIPv6 {
				// For IPv6 addresses with 128 prefix, let's try to find an appropriate subnet
				// in the routing table
				subnetIP, err := util.GetIPv6OnSubnet(iface, ip)
				if err != nil {
					return nil, fmt.Errorf("could not find IPv6 address on subnet: %v", err)
				}
				ips = append(ips, subnetIP)
				foundIPv6 = true
			}
		} else if config.IPv4Mode && !foundIPv4 {
			ips = append(ips, ip)
			foundIPv4 = true
		}
	}
	if config.IPv4Mode && !foundIPv4 {
		return nil, fmt.Errorf("failed to find IPv4 address on interface %s", iface)
	} else if config.IPv6Mode && !foundIPv6 {
		return nil, fmt.Errorf("failed to find IPv6 address on interface %s", iface)
	}
	return ips, nil
}

func getGatewayNextHops() ([]net.IP, string, error) {
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
	if gatewayIntf != "" {
		if bridgeName, _, err := util.RunOVSVsctl("port-to-br", gatewayIntf); err == nil {
			// This is an OVS bridge's internal port
			gatewayIntf = bridgeName
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

// getDPUHostPrimaryIPAddresses returns the DPU host IP/Network based on K8s Node IP
// and DPU IP subnet overriden by config config.Gateway.RouterSubnet
func getDPUHostPrimaryIPAddresses(k8sNodeIP net.IP, ifAddrs []*net.IPNet) ([]*net.IPNet, error) {
	// Note(adrianc): No Dual-Stack support at this point as we rely on k8s node IP to derive gateway information
	// for each node.
	var gwIps []*net.IPNet
	isIPv4 := utilnet.IsIPv4(k8sNodeIP)

	// override subnet mask via config
	if config.Gateway.RouterSubnet != "" {
		_, addr, err := net.ParseCIDR(config.Gateway.RouterSubnet)
		if err != nil {
			return nil, err
		}
		if utilnet.IsIPv4CIDR(addr) != isIPv4 {
			return nil, fmt.Errorf("unexpected gateway router subnet provided (%s). "+
				"does not match Node IP address format", config.Gateway.RouterSubnet)
		}
		if !addr.Contains(k8sNodeIP) {
			return nil, fmt.Errorf("unexpected gateway router subnet provided (%s). "+
				"subnet does not contain Node IP address (%s)", config.Gateway.RouterSubnet, k8sNodeIP)
		}
		addr.IP = k8sNodeIP
		gwIps = append(gwIps, addr)
	} else {
		// Assume Host and DPU share the same subnet
		// in this case just update the matching IPNet with the Host's IP address
		for _, addr := range ifAddrs {
			if utilnet.IsIPv4CIDR(addr) != isIPv4 {
				continue
			}
			// expect k8s Node IP to be contained in the given subnet
			if !addr.Contains(k8sNodeIP) {
				continue
			}
			newAddr := *addr
			newAddr.IP = k8sNodeIP
			gwIps = append(gwIps, &newAddr)
		}
		if len(gwIps) == 0 {
			return nil, fmt.Errorf("could not find subnet on DPU matching node IP %s", k8sNodeIP)
		}
	}
	return gwIps, nil
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
		routeManager.Add(netlink.Route{LinkIndex: link.Attrs().Index, Gw: gwIPCopy, Dst: &subnetCopy, Src: srcIP, MTU: mtu})
	}
	return nil
}

func (nc *DefaultNodeNetworkController) initGateway(subnets []*net.IPNet, nodeAnnotator kube.Annotator,
	waiter *startupWaiter, managementPortConfig *managementPortConfig, kubeNodeIP net.IP) error {
	klog.Info("Initializing Gateway Functionality")
	var err error
	var ifAddrs []*net.IPNet

	var loadBalancerHealthChecker *loadBalancerHealthChecker
	var portClaimWatcher *portClaimWatcher

	if config.Gateway.NodeportEnable && config.OvnKubeNode.Mode == types.NodeModeFull {
		loadBalancerHealthChecker = newLoadBalancerHealthChecker(nc.name, nc.watchFactory)
		portClaimWatcher, err = newPortClaimWatcher(nc.recorder)
		if err != nil {
			return err
		}
	}

	gatewayNextHops, gatewayIntf, err := getGatewayNextHops()
	if err != nil {
		return err
	}

	egressGWInterface := ""
	if config.Gateway.EgressGWInterface != "" {
		egressGWInterface = interfaceForEXGW(config.Gateway.EgressGWInterface)
	}

	ifAddrs, err = getNetworkInterfaceIPAddresses(gatewayIntf)
	if err != nil {
		return err
	}

	// For DPU need to use the host IP addr which currently is assumed to be K8s Node cluster
	// internal IP address.
	if config.OvnKubeNode.Mode == types.NodeModeDPU {
		ifAddrs, err = getDPUHostPrimaryIPAddresses(kubeNodeIP, ifAddrs)
		if err != nil {
			return err
		}
	}

	if err := util.SetNodePrimaryIfAddrs(nodeAnnotator, ifAddrs); err != nil {
		klog.Errorf("Unable to set primary IP net label on node, err: %v", err)
	}

	var gw *gateway
	switch config.Gateway.Mode {
	case config.GatewayModeLocal:
		klog.Info("Preparing Local Gateway")
		gw, err = newLocalGateway(nc.name, subnets, gatewayNextHops, gatewayIntf, egressGWInterface, ifAddrs, nodeAnnotator,
			managementPortConfig, nc.Kube, nc.watchFactory, nc.routeManager)
	case config.GatewayModeShared:
		klog.Info("Preparing Shared Gateway")
		gw, err = newSharedGateway(nc.name, subnets, gatewayNextHops, gatewayIntf, egressGWInterface, ifAddrs, nodeAnnotator, nc.Kube,
			managementPortConfig, nc.watchFactory, nc.routeManager)
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
			return err
		}
		err = util.SetL3GatewayConfig(nodeAnnotator, &util.L3GatewayConfig{
			Mode:      config.GatewayModeDisabled,
			ChassisID: chassisID,
		})
	}
	if err != nil {
		return err
	}
	// a golang interface has two values <type, value>. an interface is nil if both type and
	// value is nil. so, you cannot directly set the value to an interface and later check if
	// value was nil by comparing the interface to nil. this is because if the value is `nil`,
	// then the interface will still hold the type of the value being set.

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
		controllerReady, err := isOVNControllerReady()
		if err != nil || !controllerReady {
			return false, err
		}

		return gw.readyFunc()
	}

	waiter.AddWait(readyGwFunc, initGwFunc)
	nc.gateway = gw

	return nc.validateVTEPInterfaceMTU()
}

// interfaceForEXGW takes the interface requested to act as exgw bridge
// and returns the name of the bridge if exists, or the interface itself
// if the bridge needs to be created. In this last scenario, bridgeForInterface
// will create the bridge.
func interfaceForEXGW(intfName string) string {
	if _, _, err := util.RunOVSVsctl("br-exists", intfName); err == nil {
		// It's a bridge
		return intfName
	}

	bridge := util.GetBridgeName(intfName)
	if _, _, err := util.RunOVSVsctl("br-exists", bridge); err == nil {
		// not a bridge, but the corresponding bridge was already created
		return bridge
	}
	return intfName
}

func (nc *DefaultNodeNetworkController) initGatewayDPUHost(kubeNodeIP net.IP) error {
	// A DPU host gateway is complementary to the shared gateway running
	// on the DPU embedded CPU. it performs some initializations and
	// watch on services for iptable rule updates and run a loadBalancerHealth checker
	// Note: all K8s Node related annotations are handled from DPU.
	klog.Info("Initializing Shared Gateway Functionality on DPU host")
	var err error

	// Force gateway interface to be the interface associated with kubeNodeIP
	gwIntf, err := getInterfaceByIP(kubeNodeIP)
	if err != nil {
		return err
	}
	config.Gateway.Interface = gwIntf

	gatewayNextHops, gatewayIntf, err := getGatewayNextHops()
	if err != nil {
		return err
	}

	ifAddrs, err := getNetworkInterfaceIPAddresses(gatewayIntf)
	if err != nil {
		return err
	}

	if err := setNodeMasqueradeIPOnExtBridge(gwIntf); err != nil {
		return fmt.Errorf("failed to set the node masquerade IP on the ext bridge %s: %v", gwIntf, err)
	}

	if err := addMasqueradeRoute(nc.routeManager, gwIntf, nc.name, ifAddrs, nc.watchFactory); err != nil {
		return fmt.Errorf("failed to set the node masquerade route to OVN: %v", err)
	}

	err = configureSvcRouteViaInterface(nc.routeManager, gatewayIntf, gatewayNextHops)
	if err != nil {
		return err
	}

	gw := &gateway{
		initFunc:     func() error { return nil },
		readyFunc:    func() (bool, error) { return true, nil },
		watchFactory: nc.watchFactory.(*factory.WatchFactory),
	}

	// TODO(adrianc): revisit if support for nodeIPManager is needed.

	if config.Gateway.NodeportEnable {
		if err := initSharedGatewayIPTables(); err != nil {
			return err
		}
		gw.nodePortWatcherIptables = newNodePortWatcherIptables()
		gw.loadBalancerHealthChecker = newLoadBalancerHealthChecker(nc.name, nc.watchFactory)
		portClaimWatcher, err := newPortClaimWatcher(nc.recorder)
		if err != nil {
			return err
		}
		gw.portClaimWatcher = portClaimWatcher
	}

	if err := addHostMACBindings(gwIntf); err != nil {
		return fmt.Errorf("failed to add MAC bindings for service routing")
	}

	err = gw.Init(nc.stopChan, nc.wg)
	nc.gateway = gw
	return err
}

// CleanupClusterNode cleans up OVS resources on the k8s node on ovnkube-node daemonset deletion.
// This is going to be a best effort cleanup.
func CleanupClusterNode(name string) error {
	var err error

	klog.V(5).Infof("Cleaning up gateway resources on node: %q", name)
	if config.Gateway.Mode == config.GatewayModeLocal || config.Gateway.Mode == config.GatewayModeShared {
		err = cleanupLocalnetGateway(types.LocalNetworkName)
		if err != nil {
			klog.Errorf("Failed to cleanup Localnet Gateway, error: %v", err)
		}
		err = cleanupSharedGateway()
	}
	if err != nil {
		klog.Errorf("Failed to cleanup Gateway, error: %v", err)
	}

	stdout, stderr, err := util.RunOVSVsctl("--", "--if-exists", "remove", "Open_vSwitch", ".", "external_ids",
		"ovn-bridge-mappings")
	if err != nil {
		klog.Errorf("Failed to delete ovn-bridge-mappings, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
	}

	// Delete iptable rules for management port
	DelMgtPortIptRules()

	return nil
}

func (nc *DefaultNodeNetworkController) updateGatewayMAC(link netlink.Link) error {
	// TBD-merge for dpu-host mode: if interface mac of the dpu-host interface that connects to the
	// gateway bridge on the dpu changes, we need to update dpu's gatewayBridge.macAddress L3 gateway
	// annotation (see bridgeForInterface)
	if config.OvnKubeNode.Mode != types.NodeModeFull {
		return nil
	}

	if nc.gateway.GetGatewayBridgeIface() != link.Attrs().Name {
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
	nc.gateway.SetDefaultGatewayBridgeMAC(link.Attrs().HardwareAddr)
	if err := nc.gateway.Reconcile(); err != nil {
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
