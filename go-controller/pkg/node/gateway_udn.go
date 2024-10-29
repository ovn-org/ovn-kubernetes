package node

import (
	"fmt"
	"net"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	"github.com/vishvananda/netlink"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/generator/udn"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/iprulemanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/vrfmanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

const (
	// ctMarkUDNBase is the conntrack mark base value for user defined networks to use
	// Each network gets its own mark == base + network-id
	ctMarkUDNBase = 3
	// waitForPatchPortTimeout is the maximum time we wait for a UDN's patch
	// port to be created by OVN.
	waitForPatchPortTimeout = 30 * time.Second
	// UDNMasqueradeIPRulePriority the priority of the ip routing rules created for masquerade IP address
	// allocated for every user defined network.
	UDNMasqueradeIPRulePriority = 2000
)

// UserDefinedNetworkGateway contains information
// required to program a UDN at each node's
// gateway.
// NOTE: Currently invoked only for primary networks.
type UserDefinedNetworkGateway struct {
	// network information
	util.NetInfo
	// stores the networkID of this network
	networkID int
	// node that its programming things on
	node          *v1.Node
	nodeLister    listers.NodeLister
	kubeInterface kube.Interface
	// vrf manager that creates and manages vrfs for all UDNs
	// used with a lock since its shared between all network controllers
	vrfManager *vrfmanager.Controller
	// masqCTMark holds the mark value for this network
	// which is used for egress traffic in shared gateway mode
	masqCTMark uint
	// v4MasqIPs holds the IPv4 masquerade IPs for this network
	v4MasqIPs *udn.MasqueradeIPs
	// v6MasqIPs holds the IPv6 masquerade IPs for this network
	v6MasqIPs *udn.MasqueradeIPs
	// stores the pointer to default network's gateway so that
	// we can leverage it from here to program UDN flows on breth0
	// Currently we use the openflowmanager and nodeIPManager from
	// gateway, but maybe we could invoke our own instance of these
	// for UDNs in the future. For now default network and UDNs will
	// use the same gateway struct instance
	*gateway
	// iprules manager that creates and manages iprules for
	// all UDNs. Must be accessed with a lock
	ruleManager *iprulemanager.Controller
}

// UTILS Needed for UDN (also leveraged for default netInfo) in bridgeConfiguration

// getBridgePortConfigurations returns a slice of Network port configurations along with the
// uplinkName and physical port's ofport value
func (b *bridgeConfiguration) getBridgePortConfigurations() ([]bridgeUDNConfiguration, string, string) {
	b.Lock()
	defer b.Unlock()
	netConfigs := make([]bridgeUDNConfiguration, len(b.netConfig))
	for _, netConfig := range b.netConfig {
		netConfigs = append(netConfigs, *netConfig)
	}
	return netConfigs, b.uplinkName, b.ofPortPhys
}

// addNetworkBridgeConfig adds the patchport and ctMark value for the provided netInfo into the bridge configuration cache
func (b *bridgeConfiguration) addNetworkBridgeConfig(nInfo util.NetInfo, masqCTMark uint, v4MasqIPs, v6MasqIPs *udn.MasqueradeIPs) {
	b.Lock()
	defer b.Unlock()

	netName := nInfo.GetNetworkName()
	patchPort := nInfo.GetNetworkScopedPatchPortName(b.bridgeName, b.nodeName)

	_, found := b.netConfig[netName]
	if !found {
		netConfig := &bridgeUDNConfiguration{
			patchPort:  patchPort,
			masqCTMark: fmt.Sprintf("0x%x", masqCTMark),
			v4MasqIPs:  v4MasqIPs,
			v6MasqIPs:  v6MasqIPs,
		}

		b.netConfig[netName] = netConfig
	} else {
		klog.Warningf("Trying to update bridge config for network %s which already"+
			"exists in cache...networks are not mutable...ignoring update", nInfo.GetNetworkName())
	}
}

// delNetworkBridgeConfig deletes the provided netInfo from the bridge configuration cache
func (b *bridgeConfiguration) delNetworkBridgeConfig(nInfo util.NetInfo) {
	b.Lock()
	defer b.Unlock()

	delete(b.netConfig, nInfo.GetNetworkName())
}

// getActiveNetworkBridgeConfig returns a copy of the network configuration corresponding to the
// provided netInfo.
//
// NOTE: if the network configuration can't be found or if the network is not patched by OVN
// yet this returns nil.
func (b *bridgeConfiguration) getActiveNetworkBridgeConfig(nInfo util.NetInfo) *bridgeUDNConfiguration {
	b.Lock()
	defer b.Unlock()

	if netConfig, found := b.netConfig[nInfo.GetNetworkName()]; found && netConfig.ofPortPatch != "" {
		result := *netConfig
		return &result
	}
	return nil
}

func (b *bridgeConfiguration) patchedNetConfigs() []*bridgeUDNConfiguration {
	result := make([]*bridgeUDNConfiguration, 0, len(b.netConfig))
	for _, netConfig := range b.netConfig {
		if netConfig.ofPortPatch == "" {
			continue
		}
		result = append(result, netConfig)
	}
	return result
}

// END UDN UTILs for bridgeConfiguration

// bridgeUDNConfiguration holds the patchport and ctMark
// information for a given network
type bridgeUDNConfiguration struct {
	patchPort   string
	ofPortPatch string
	masqCTMark  string
	v4MasqIPs   *udn.MasqueradeIPs
	v6MasqIPs   *udn.MasqueradeIPs
}

func (netConfig *bridgeUDNConfiguration) isDefaultNetwork() bool {
	return netConfig.masqCTMark == ctMarkOVN
}

func (netConfig *bridgeUDNConfiguration) setBridgeNetworkOfPortsInternal() error {
	ofportPatch, stderr, err := util.GetOVSOfPort("get", "Interface", netConfig.patchPort, "ofport")
	if err != nil {
		return fmt.Errorf("failed while waiting on patch port %q to be created by ovn-controller and "+
			"while getting ofport. stderr: %v, error: %v", netConfig.patchPort, stderr, err)
	}
	netConfig.ofPortPatch = ofportPatch
	return nil
}

func setBridgeNetworkOfPorts(bridge *bridgeConfiguration, netName string) error {
	bridge.Lock()
	defer bridge.Unlock()

	netConfig, found := bridge.netConfig[netName]
	if !found {
		return fmt.Errorf("failed to find network %s configuration on bridge %s", netName, bridge.bridgeName)
	}
	return netConfig.setBridgeNetworkOfPortsInternal()
}

func NewUserDefinedNetworkGateway(netInfo util.NetInfo, networkID int, node *v1.Node, nodeLister listers.NodeLister,
	kubeInterface kube.Interface, vrfManager *vrfmanager.Controller, ruleManager *iprulemanager.Controller,
	defaultNetworkGateway Gateway) (*UserDefinedNetworkGateway, error) {
	// Generate a per network conntrack mark and masquerade IPs to be used for egress traffic.
	var (
		v4MasqIPs *udn.MasqueradeIPs
		v6MasqIPs *udn.MasqueradeIPs
		err       error
	)
	masqCTMark := ctMarkUDNBase + uint(networkID)
	if config.IPv4Mode {
		v4MasqIPs, err = udn.AllocateV4MasqueradeIPs(networkID)
		if err != nil {
			return nil, fmt.Errorf("failed to get v4 masquerade IP, network %s (%d): %v", netInfo.GetNetworkName(), networkID, err)
		}
	}
	if config.IPv6Mode {
		v6MasqIPs, err = udn.AllocateV6MasqueradeIPs(networkID)
		if err != nil {
			return nil, fmt.Errorf("failed to get v6 masquerade IP, network %s (%d): %v", netInfo.GetNetworkName(), networkID, err)
		}
	}

	gw, ok := defaultNetworkGateway.(*gateway)
	if !ok {
		return nil, fmt.Errorf("unable to dereference default node network controller gateway object")
	}

	return &UserDefinedNetworkGateway{
		NetInfo:       netInfo,
		networkID:     networkID,
		node:          node,
		nodeLister:    nodeLister,
		kubeInterface: kubeInterface,
		vrfManager:    vrfManager,
		masqCTMark:    masqCTMark,
		v4MasqIPs:     v4MasqIPs,
		v6MasqIPs:     v6MasqIPs,
		gateway:       gw,
		ruleManager:   ruleManager,
	}, nil
}

// AddNetwork will be responsible to create all plumbings
// required by this UDN on the gateway side
func (udng *UserDefinedNetworkGateway) AddNetwork() error {
	// port is created first and its MAC address configured. The IP(s) on that link are added after enslaving to a VRF device (addUDNManagementPortIPs)
	// because IPv6 addresses are removed by the kernel (if not link local) when enslaved to a VRF device.
	mplink, macAddress, err := udng.addUDNManagementPort()
	if err != nil {
		return fmt.Errorf("could not create management port netdevice for network %s: %w", udng.GetNetworkName(), err)
	}
	vrfDeviceName := util.GetVRFDeviceNameForUDN(udng.networkID)
	vrfTableId := util.CalculateRouteTableID(mplink.Attrs().Index)
	routes, err := udng.computeRoutesForUDN(vrfTableId, mplink)
	if err != nil {
		return fmt.Errorf("failed to compute routes for network %s, err: %v", udng.GetNetworkName(), err)
	}
	if err = udng.vrfManager.AddVRF(vrfDeviceName, mplink.Attrs().Name, uint32(vrfTableId), routes); err != nil {
		return fmt.Errorf("could not add VRF %d for network %s, err: %v", vrfTableId, udng.GetNetworkName(), err)
	}
	if err = udng.addUDNManagementPortIPs(mplink); err != nil {
		return fmt.Errorf("unable to add management port IP(s) for link %s, for network %s: %w", mplink.Attrs().Name, udng.GetNetworkName(), err)
	}
	if err := util.UpdateNodeManagementPortMACAddressesWithRetry(udng.node, udng.nodeLister, udng.kubeInterface, macAddress, udng.GetNetworkName()); err != nil {
		return fmt.Errorf("unable to update mac address annotation for node %s, for network %s, err: %w", udng.node.Name, udng.GetNetworkName(), err)
	}
	// create the iprules for this network
	udnReplyIPRules, err := udng.constructUDNVRFIPRules(vrfTableId)
	if err != nil {
		return fmt.Errorf("unable to get iprules for network %s, err: %v", udng.GetNetworkName(), err)
	}
	for _, rule := range udnReplyIPRules {
		if err = udng.ruleManager.AddWithMetadata(rule, udng.GetNetworkRuleMetadata()); err != nil {
			return fmt.Errorf("unable to create iprule %v for network %s, err: %v", rule, udng.GetNetworkName(), err)
		}
	}
	// add loose mode for rp filter on management port
	mgmtPortName := util.GetNetworkScopedK8sMgmtHostIntfName(uint(udng.networkID))
	if err := addRPFilterLooseModeForManagementPort(mgmtPortName); err != nil {
		return fmt.Errorf("could not set loose mode for reverse path filtering on management port %s: %v", mgmtPortName, err)
	}
	if udng.openflowManager != nil {
		udng.openflowManager.addNetwork(udng.NetInfo, udng.masqCTMark, udng.v4MasqIPs, udng.v6MasqIPs)

		waiter := newStartupWaiterWithTimeout(waitForPatchPortTimeout)
		readyFunc := func() (bool, error) {
			if err := setBridgeNetworkOfPorts(udng.openflowManager.defaultBridge, udng.GetNetworkName()); err != nil {
				return false, fmt.Errorf("failed to set network %s's openflow ports for default bridge; error: %v", udng.GetNetworkName(), err)
			}
			if udng.openflowManager.externalGatewayBridge != nil {
				if err := setBridgeNetworkOfPorts(udng.openflowManager.externalGatewayBridge, udng.GetNetworkName()); err != nil {
					return false, fmt.Errorf("failed to set network %s's openflow ports for secondary bridge; error: %v", udng.GetNetworkName(), err)
				}
			}
			return true, nil
		}
		postFunc := func() error {
			if err := udng.Reconcile(); err != nil {
				return fmt.Errorf("failed to reconcile flows on bridge for network %s; error: %v", udng.GetNetworkName(), err)
			}
			return nil
		}
		waiter.AddWait(readyFunc, postFunc)
		if err := waiter.Wait(); err != nil {
			return err
		}
	} else {
		klog.Warningf("Openflow manager has not been invoked for network %s; we will skip programming flows"+
			"on the bridge for this network.", udng.NetInfo.GetNetworkName())
	}
	return nil
}

func (udng *UserDefinedNetworkGateway) GetNetworkRuleMetadata() string {
	return fmt.Sprintf("%s-%d", udng.GetNetworkName(), udng.networkID)
}

// DelNetwork will be responsible to remove all plumbings
// used by this UDN on the gateway side
func (udng *UserDefinedNetworkGateway) DelNetwork() error {
	vrfDeviceName := util.GetVRFDeviceNameForUDN(udng.networkID)
	// delete the iprules for this network
	if err := udng.ruleManager.DeleteWithMetadata(udng.GetNetworkRuleMetadata()); err != nil {
		return fmt.Errorf("unable to delete iprules for network %s, err: %v", udng.GetNetworkName(), err)
	}
	// delete the VRF device for this network
	if err := udng.vrfManager.DeleteVRF(vrfDeviceName); err != nil {
		return err
	}
	// delete the openflows for this network
	if udng.openflowManager != nil {
		udng.openflowManager.delNetwork(udng.NetInfo)
		if err := udng.Reconcile(); err != nil {
			return fmt.Errorf("failed to reconcile default gateway for network %s, err: %v", udng.GetNetworkName(), err)
		}
	}
	// delete the management port interface for this network
	return udng.deleteUDNManagementPort()
}

// addUDNManagementPort does the following:
// STEP1: creates the (netdevice) OVS interface on br-int for the UDN's management port
// STEP2: It saves the MAC address generated on the 1st go as an option on the OVS interface
// so that it persists on reboots
// STEP3: sets up the management port link on the host
// STEP4: enables IPv4 forwarding on the interface if the network has a v4 subnet
// Returns a netlink Link which is the UDN management port interface along with its MAC address
func (udng *UserDefinedNetworkGateway) addUDNManagementPort() (netlink.Link, net.HardwareAddr, error) {
	var err error
	interfaceName := util.GetNetworkScopedK8sMgmtHostIntfName(uint(udng.networkID))
	// STEP1
	stdout, stderr, err := util.RunOVSVsctl(
		"--", "--may-exist", "add-port", "br-int", interfaceName,
		"--", "set", "interface", interfaceName,
		"type=internal", "mtu_request="+fmt.Sprintf("%d", udng.NetInfo.MTU()),
		"external-ids:iface-id="+udng.GetNetworkScopedK8sMgmtIntfName(udng.node.Name),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to add port to br-int for network %s, stdout: %q, stderr: %q, error: %w",
			udng.GetNetworkName(), stdout, stderr, err)
	}
	klog.V(3).Infof("Added OVS management port interface %s for network %s", interfaceName, udng.GetNetworkName())

	// STEP2
	macAddress, err := util.GetOVSPortMACAddress(interfaceName)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get management port MAC address for network %s: %v", udng.GetNetworkName(), err)
	}
	// persist the MAC address so that upon node reboot we get back the same mac address.
	_, stderr, err = util.RunOVSVsctl("set", "interface", interfaceName,
		fmt.Sprintf("mac=%s", strings.ReplaceAll(macAddress.String(), ":", "\\:")))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to persist MAC address %q for %q while plumbing network %s: stderr:%s (%v)",
			macAddress.String(), interfaceName, udng.GetNetworkName(), stderr, err)
	}

	// STEP3
	mplink, err := util.LinkSetUp(interfaceName)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to set the link up for interface %s while plumbing network %s, err: %v",
			interfaceName, udng.GetNetworkName(), err)
	}
	klog.V(3).Infof("Setup management port link %s for network %s succeeded", interfaceName, udng.GetNetworkName())

	// STEP4
	// IPv6 forwarding is enabled globally
	if ipv4, _ := udng.IPMode(); ipv4 {
		stdout, stderr, err := util.RunSysctl("-w", fmt.Sprintf("net.ipv4.conf.%s.forwarding=1", interfaceName))
		if err != nil || stdout != fmt.Sprintf("net.ipv4.conf.%s.forwarding = 1", interfaceName) {
			return nil, nil, fmt.Errorf("could not set the correct forwarding value for interface %s: stdout: %v, stderr: %v, err: %v",
				interfaceName, stdout, stderr, err)
		}
	}
	return mplink, macAddress, nil
}

func (udng *UserDefinedNetworkGateway) addUDNManagementPortIPs(mpLink netlink.Link) error {
	var err error
	var networkLocalSubnets []*net.IPNet
	// fetch subnets which we will use to get management port IP(s)
	if udng.TopologyType() == types.Layer3Topology {
		networkLocalSubnets, err = util.ParseNodeHostSubnetAnnotation(udng.node, udng.GetNetworkName())
		if err != nil {
			return fmt.Errorf("waiting for node %s to start, no annotation found on node for network %s: %w",
				udng.node.Name, udng.GetNetworkName(), err)
		}
	} else if udng.TopologyType() == types.Layer2Topology {
		// NOTE: We don't support L2 networks without subnets as primary UDNs
		globalFlatL2Networks := udng.Subnets()
		for _, globalFlatL2Network := range globalFlatL2Networks {
			networkLocalSubnets = append(networkLocalSubnets, globalFlatL2Network.CIDR)
		}
	}
	// extract management port IP from subnets and add it to link
	for _, subnet := range networkLocalSubnets {
		if config.IPv6Mode && utilnet.IsIPv6CIDR(subnet) || config.IPv4Mode && utilnet.IsIPv4CIDR(subnet) {
			ip := util.GetNodeManagementIfAddr(subnet)
			var err error
			var exists bool
			if exists, err = util.LinkAddrExist(mpLink, ip); err == nil && !exists {
				err = util.LinkAddrAdd(mpLink, ip, 0, 0, 0)
			}
			if err != nil {
				return fmt.Errorf("failed to add management port IP from subnet %s to netdevice %s for network %s, err: %v",
					subnet, mpLink.Attrs().Name, udng.GetNetworkName(), err)
			}
		}
	}
	return nil
}

// deleteUDNManagementPort does the following:
// STEP1: deletes the OVS interface on br-int for the UDN's management port interface
// STEP2: deletes the mac address from the annotation
func (udng *UserDefinedNetworkGateway) deleteUDNManagementPort() error {
	var err error
	interfaceName := util.GetNetworkScopedK8sMgmtHostIntfName(uint(udng.networkID))
	// STEP1
	stdout, stderr, err := util.RunOVSVsctl(
		"--", "--if-exists", "del-port", "br-int", interfaceName,
	)
	if err != nil {
		return fmt.Errorf("failed to delete port from br-int for network %s, stdout: %q, stderr: %q, error: %v",
			udng.GetNetworkName(), stdout, stderr, err)
	}
	klog.V(3).Infof("Removed OVS management port interface %s for network %s", interfaceName, udng.GetNetworkName())
	// sending nil mac address will delete the network's annotation value
	if err := util.UpdateNodeManagementPortMACAddressesWithRetry(udng.node, udng.nodeLister, udng.kubeInterface, nil, udng.GetNetworkName()); err != nil {
		return fmt.Errorf("unable to remove mac address annotation for node %s, for network %s, err: %v", udng.node.Name, udng.GetNetworkName(), err)
	}
	klog.V(3).Infof("Removed management port mac address information of %s for network %s", interfaceName, udng.GetNetworkName())
	return nil
}

// computeRoutesForUDN returns a list of routes programmed into a given UDN's VRF
// when adding new routes please leave a sample comment on how that route looks like
func (udng *UserDefinedNetworkGateway) computeRoutesForUDN(vrfTableId int, mpLink netlink.Link) ([]netlink.Route, error) {
	nextHops, intfName, err := getGatewayNextHops()
	if err != nil {
		return nil, fmt.Errorf("unable to get the gateway next hops for node %s, err: %v", udng.node.Name, err)
	}
	link, err := util.GetNetLinkOps().LinkByName(intfName)
	if err != nil {
		return nil, fmt.Errorf("unable to get link for %s, error: %v", intfName, err)
	}
	networkMTU := udng.NetInfo.MTU()
	if networkMTU == 0 {
		networkMTU = config.Default.MTU
	}
	var retVal []netlink.Route
	// Route1: Add serviceCIDR route: 10.96.0.0/16 via 169.254.169.4 dev breth0 mtu 1400
	// necessary for UDN CNI and host-networked pods to talk to services
	for _, serviceSubnet := range config.Kubernetes.ServiceCIDRs {
		serviceSubnet := serviceSubnet
		isV6 := utilnet.IsIPv6CIDR(serviceSubnet)
		gwIP := config.Gateway.MasqueradeIPs.V4DummyNextHopMasqueradeIP
		if isV6 {
			gwIP = config.Gateway.MasqueradeIPs.V6DummyNextHopMasqueradeIP
		}
		retVal = append(retVal, netlink.Route{
			LinkIndex: link.Attrs().Index,
			Dst:       serviceSubnet,
			MTU:       networkMTU,
			Gw:        gwIP,
			Table:     vrfTableId,
		})
	}

	// Route2: Add default route: default via 172.18.0.1 dev breth0 mtu 1400
	// necessary for UDN CNI and host-networked pods default traffic to go to node's gatewayIP
	var defaultAnyCIDR *net.IPNet
	for _, nextHop := range nextHops {
		isV6 := utilnet.IsIPv6(nextHop)
		_, defaultAnyCIDR, _ = net.ParseCIDR("0.0.0.0/0")
		if isV6 {
			_, defaultAnyCIDR, _ = net.ParseCIDR("::/0")
		}
		retVal = append(retVal, netlink.Route{
			LinkIndex: link.Attrs().Index,
			Dst:       defaultAnyCIDR,
			MTU:       networkMTU,
			Gw:        nextHop,
			Table:     vrfTableId,
		})
	}

	// Route3: Add MasqueradeRoute for reply traffic route: 169.254.169.12 dev ovn-k8s-mpX mtu 1400
	// necessary for reply traffic towards UDN CNI pods to go into OVN
	masqIPv4, err := udng.getV4MasqueradeIP()
	if err != nil {
		return nil, fmt.Errorf("unable to fetch masqueradeV4 IP for network %s, err: %v", udng.GetNetworkName(), err)
	}
	if masqIPv4 != nil {
		retVal = append(retVal, netlink.Route{
			LinkIndex: mpLink.Attrs().Index,
			Dst:       masqIPv4,
			MTU:       networkMTU,
			Table:     vrfTableId,
		})
	}

	masqIPv6, err := udng.getV6MasqueradeIP()
	if err != nil {
		return nil, fmt.Errorf("unable to fetch masqueradeV6 IP for network %s, err: %v", udng.GetNetworkName(), err)
	}
	if masqIPv6 != nil {
		retVal = append(retVal, netlink.Route{
			LinkIndex: mpLink.Attrs().Index,
			Dst:       masqIPv6,
			MTU:       networkMTU,
			Table:     vrfTableId,
		})
	}

	return retVal, nil
}

// getV4MasqueradeIP returns the V4 management port masqueradeIP for this network
func (udng *UserDefinedNetworkGateway) getV4MasqueradeIP() (*net.IPNet, error) {
	if !config.IPv4Mode {
		return nil, nil
	}
	masqIPs, err := udn.AllocateV4MasqueradeIPs(udng.networkID)
	if err != nil {
		return nil, fmt.Errorf("failed to allocate masquerade IPs for v4 stack for network %s: %w", udng.GetNetworkName(), err)
	}
	return util.GetIPNetFullMaskFromIP(masqIPs.ManagementPort.IP), nil
}

// getV6MasqueradeIP returns the V6 management port masqueradeIP for this network
func (udng *UserDefinedNetworkGateway) getV6MasqueradeIP() (*net.IPNet, error) {
	if !config.IPv6Mode {
		return nil, nil
	}
	masqIPs, err := udn.AllocateV6MasqueradeIPs(udng.networkID)
	if err != nil {
		return nil, fmt.Errorf("failed to allocate masquerade IPs for v6 stack for network %s: %w", udng.GetNetworkName(), err)
	}
	return util.GetIPNetFullMaskFromIP(masqIPs.ManagementPort.IP), nil
}

// constructUDNVRFIPRules constructs rules that redirect packets towards the per-UDN masquerade IP
// into the corresponding UDN VRF routing table.
// Example:
// 2000:   from all to 169.254.0.12 lookup 1007
// 2000:   from all to 169.254.0.14 lookup 1009
func (udng *UserDefinedNetworkGateway) constructUDNVRFIPRules(vrfTableId int) ([]netlink.Rule, error) {
	var masqIPRules []netlink.Rule
	masqIPv4, err := udng.getV4MasqueradeIP()
	if err != nil {
		return nil, err
	}
	if masqIPv4 != nil {
		masqIPRules = append(masqIPRules, generateIPRuleForMasqIP(masqIPv4.IP, false, uint(vrfTableId)))
	}
	masqIPv6, err := udng.getV6MasqueradeIP()
	if err != nil {
		return nil, err
	}
	if masqIPv6 != nil {
		masqIPRules = append(masqIPRules, generateIPRuleForMasqIP(masqIPv6.IP, true, uint(vrfTableId)))
	}
	return masqIPRules, nil
}

func generateIPRuleForMasqIP(masqIP net.IP, isIPv6 bool, vrfTableId uint) netlink.Rule {
	r := *netlink.NewRule()
	r.Table = int(vrfTableId)
	r.Priority = UDNMasqueradeIPRulePriority
	r.Family = netlink.FAMILY_V4
	if isIPv6 {
		r.Family = netlink.FAMILY_V6
	}
	r.Dst = util.GetIPNetFullMaskFromIP(masqIP)
	return r
}

func addRPFilterLooseModeForManagementPort(mgmtPortName string) error {
	// update the reverse path filtering options for ovn-k8s-mpX interface to avoid dropping packets with masqueradeIP
	// coming out of managementport interface
	// NOTE: v6 doesn't have rp_filter strict mode block
	rpFilterLooseMode := "2"
	// TODO: Convert testing framework to mock golang module utilities. Example:
	// result, err := sysctl.Sysctl(fmt.Sprintf("net/ipv4/conf/%s/rp_filter", types.K8sMgmtIntfName), rpFilterLooseMode)
	stdout, stderr, err := util.RunSysctl("-w", fmt.Sprintf("net.ipv4.conf.%s.rp_filter=%s", mgmtPortName, rpFilterLooseMode))
	if err != nil || stdout != fmt.Sprintf("net.ipv4.conf.%s.rp_filter = %s", mgmtPortName, rpFilterLooseMode) {
		return fmt.Errorf("could not set the correct rp_filter value for interface %s: stdout: %v, stderr: %v, err: %v",
			mgmtPortName, stdout, stderr, err)
	}
	return nil
}
