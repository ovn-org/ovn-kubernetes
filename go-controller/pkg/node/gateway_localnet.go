//go:build linux
// +build linux

package node

import (
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/allocator/udn"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/iprulemanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/iptables"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/routemanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/vrfmanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	utilerrors "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/errors"
	"github.com/vishvananda/netlink"

	"k8s.io/klog/v2"
	utiliptables "k8s.io/kubernetes/pkg/util/iptables"
	utilnet "k8s.io/utils/net"
)

func newLocalGateway(nodeName string, hostSubnets []*net.IPNet, gwNextHops []net.IP, gwIntf, egressGWIntf string, gwIPs []*net.IPNet,
	nodeAnnotator kube.Annotator, cfg *managementPortConfig, kube kube.Interface, watchFactory factory.NodeWatchFactory,
	routeManager *routemanager.Controller) (*gateway, error) {
	klog.Info("Creating new local gateway")
	gw := &gateway{}

	for _, hostSubnet := range hostSubnets {
		// local gateway mode uses mp0 as default path for all ingress traffic into OVN
		var nextHop *net.IPNet
		if utilnet.IsIPv6CIDR(hostSubnet) {
			nextHop = cfg.ipv6.ifAddr
		} else {
			nextHop = cfg.ipv4.ifAddr
		}

		// add iptables masquerading for mp0 to exit the host for egress
		cidr := nextHop.IP.Mask(nextHop.Mask)
		cidrNet := &net.IPNet{IP: cidr, Mask: nextHop.Mask}
		err := initLocalGatewayNATRules(cfg.ifName, cidrNet)
		if err != nil {
			return nil, fmt.Errorf("failed to add local NAT rules for: %s, err: %v", cfg.ifName, err)
		}
	}

	gwBridge, exGwBridge, err := gatewayInitInternal(
		nodeName, gwIntf, egressGWIntf, gwNextHops, gwIPs, nodeAnnotator)
	if err != nil {
		return nil, err
	}

	if exGwBridge != nil {
		gw.readyFunc = func() (bool, error) {
			ready, err := gatewayReady(gwBridge.patchPort)
			if err != nil {
				return false, err
			}
			exGWReady, err := gatewayReady(exGwBridge.patchPort)
			if err != nil {
				return false, err
			}
			return ready && exGWReady, nil
		}
	} else {
		gw.readyFunc = func() (bool, error) {
			return gatewayReady(gwBridge.patchPort)
		}
	}

	gw.initFunc = func() error {
		klog.Info("Creating Local Gateway Openflow Manager")
		err := setBridgeOfPorts(gwBridge)
		if err != nil {
			return err
		}
		if exGwBridge != nil {
			err = setBridgeOfPorts(exGwBridge)
			if err != nil {
				return err
			}

		}

		gw.nodeIPManager = newAddressManager(nodeName, gwIntf, kube, cfg, watchFactory, gwBridge)

		if err := setNodeMasqueradeIPOnExtBridge(gwBridge.bridgeName); err != nil {
			return fmt.Errorf("failed to set the node masquerade IP on the ext bridge %s: %v", gwBridge.bridgeName, err)
		}

		if err := addMasqueradeRoute(routeManager, gwBridge.bridgeName, nodeName, gwIPs, watchFactory); err != nil {
			return fmt.Errorf("failed to set the node masquerade route to OVN: %v", err)
		}

		gw.openflowManager, err = newGatewayOpenFlowManager(gwBridge, exGwBridge, hostSubnets, gw.nodeIPManager.ListAddresses())
		if err != nil {
			return err
		}
		// resync flows on IP change
		gw.nodeIPManager.OnChanged = func() {
			klog.V(5).Info("Node addresses changed, re-syncing bridge flows")
			if err := gw.openflowManager.updateBridgeFlowCache(hostSubnets, gw.nodeIPManager.ListAddresses()); err != nil {
				// very unlikely - somehow node has lost its IP address
				klog.Errorf("Failed to re-generate gateway flows after address change: %v", err)
			}
			// update gateway IPs for service openflows programmed by nodePortWatcher interface
			npw, _ := gw.nodePortWatcher.(*nodePortWatcher)
			npw.updateGatewayIPs(gw.nodeIPManager)
			// Services create OpenFlow flows as well, need to update them all
			if gw.servicesRetryFramework != nil {
				if errs := gw.addAllServices(); errs != nil {
					err := utilerrors.Join(errs...)
					klog.Errorf("Failed to sync all services after node IP change: %v", err)
				}
			}
			gw.openflowManager.requestFlowSync()
		}

		if config.Gateway.NodeportEnable {
			if config.OvnKubeNode.Mode == types.NodeModeFull {
				// (TODO): Internal Traffic Policy is not supported in DPU mode
				if err := initSvcViaMgmPortRoutingRules(hostSubnets); err != nil {
					return err
				}
			}
			gw.nodePortWatcher, err = newNodePortWatcher(gwBridge, gw.openflowManager, gw.nodeIPManager, watchFactory)
			if err != nil {
				return err
			}
		} else {
			// no service OpenFlows, request to sync flows now.
			gw.openflowManager.requestFlowSync()
		}

		if err := addHostMACBindings(gwBridge.bridgeName); err != nil {
			return fmt.Errorf("failed to add MAC bindings for service routing")
		}

		gw.vrfManager = vrfmanager.NewController(routeManager)
		gw.routeManager = routeManager
		gw.ruleManager = iprulemanager.NewController(config.IPv4Mode, config.IPv6Mode)
		gw.ipTablesManager = iptables.NewController()

		gw.wg.Add(1)
		go func() {
			defer gw.wg.Done()
			gw.vrfManager.Run(gw.stopChan, gw.wg)
		}()

		gw.wg.Add(1)
		go func() {
			defer gw.wg.Done()
			gw.ruleManager.Run(gw.stopChan, 5*time.Minute)
		}()

		gw.wg.Add(1)
		go func() {
			defer gw.wg.Done()
			gw.ipTablesManager.Run(gw.stopChan, 5*time.Minute)
		}()

		iptJumpRule := iptables.RuleArg{Args: []string{"-j", types.UserNetIPChainName}}
		if config.IPv4Mode {
			if err := gw.ipTablesManager.OwnChain(utiliptables.TableNAT, types.UserNetIPTableChainName, utiliptables.ProtocolIPv4); err != nil {
				return fmt.Errorf("unable to own chain %s: %v", types.UserNetIPTableChainName, err)
			}
			if err = gw.ipTablesManager.EnsureRule(utiliptables.TableNAT, utiliptables.ChainPostrouting, utiliptables.ProtocolIPv4, iptJumpRule); err != nil {
				return fmt.Errorf("failed to create rule in chain %s to jump to chain %s: %v", utiliptables.ChainPostrouting, types.UserNetIPTableChainName, err)
			}
		}
		if config.IPv6Mode {
			if err := gw.ipTablesManager.OwnChain(utiliptables.TableNAT, types.UserNetIPTableChainName, utiliptables.ProtocolIPv6); err != nil {
				return fmt.Errorf("unable to own chain %s: %v", types.UserNetIPTableChainName, err)
			}
			if err = gw.ipTablesManager.EnsureRule(utiliptables.TableNAT, utiliptables.ChainPostrouting, utiliptables.ProtocolIPv6, iptJumpRule); err != nil {
				return fmt.Errorf("failed to create rule in chain %s to jump to chain %s: %v", utiliptables.ChainPostrouting, types.UserNetIPTableChainName, err)
			}
		}

		return nil
	}
	gw.watchFactory = watchFactory.(*factory.WatchFactory)

	klog.Info("Local Gateway Creation Complete")
	return gw, nil
}

func (g *gateway) computeRoutesForUDN(nInfo util.NetInfo, vrfTableId int, managementPortName string) ([]netlink.Route, error) {
	/*err := configureSvcRouteViaInterface(g.routeManager, g.GetGatewayBridgeIface(), DummyNextHopIPs(), vrfTableId)
	if err != nil {
		return err
	}*/
	interfaceName := g.GetGatewayBridgeIface()
	link, err := util.LinkSetUp(interfaceName)
	if err != nil {
		return nil, fmt.Errorf("unable to get link for %s, error: %v", interfaceName, err)
	}
	mtu := config.Default.MTU
	if config.Default.RoutableMTU != 0 {
		mtu = config.Default.RoutableMTU
	}
	var retVal []netlink.Route
	// TODO add other routes.
	// Route1: Add serviceCIDR route: 10.96.0.0/16 via 169.254.169.4 dev breth0 mtu 1400
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
			MTU:       mtu,
			Gw:        gwIP,
			Table:     vrfTableId,
		})
	}
	nextHops, _, err := getGatewayNextHops()
	if err != nil {
		return nil, err
	}
	// Route2: Add default route: default via 172.18.0.1 dev breth0 mtu 1400
	if config.IPv4Mode {
		_, defaultV4AnyCIDR, _ := net.ParseCIDR("0.0.0.0/0")
		retVal = append(retVal, netlink.Route{
			LinkIndex: link.Attrs().Index,
			Dst:       defaultV4AnyCIDR,
			MTU:       mtu,
			Gw:        nextHops[0],
			Table:     vrfTableId,
		})
	}
	if config.IPv6Mode {
		_, defaultV6AnyCIDR, _ := net.ParseCIDR("::/0")
		retVal = append(retVal, netlink.Route{
			LinkIndex: link.Attrs().Index,
			Dst:       defaultV6AnyCIDR,
			MTU:       mtu,
			Gw:        nextHops[1], // TODO(tssurya): Check out of bounds
			Table:     vrfTableId,
		})
	}
	// Route3: Add MasqueradeRoute for reply traffic route: 169.254.169.12 dev ovn-k8s-mpX mtu 1400
	link, err = util.LinkSetUp(managementPortName)
	if err != nil {
		return nil, fmt.Errorf("unable to get link for %s, error: %v", managementPortName, err)
	}
	masqIPv4, err := g.getV4MasqueradeIP(nInfo)
	if err != nil {
		return nil, err
	}
	if config.IPv4Mode && masqIPv4 != nil {
		retVal = append(retVal, netlink.Route{
			LinkIndex: link.Attrs().Index,
			Dst:       util.GetIPNetFullMaskFromIP(*masqIPv4),
			MTU:       mtu,
			Table:     vrfTableId,
		})
	}
	masqIPv6, err := g.getV6MasqueradeIP(nInfo)
	if err != nil {
		return nil, err
	}
	if config.IPv6Mode && masqIPv6 != nil {
		if config.IPv4Mode && masqIPv4 != nil {
			retVal = append(retVal, netlink.Route{
				LinkIndex: link.Attrs().Index,
				Dst:       util.GetIPNetFullMaskFromIP(*masqIPv6),
				MTU:       mtu,
				Table:     vrfTableId,
			})
		}
	}
	return retVal, nil
}

func getGatewayFamilyAddrs(gatewayIfAddrs []*net.IPNet) (string, string) {
	var gatewayIPv4, gatewayIPv6 string
	for _, gatewayIfAddr := range gatewayIfAddrs {
		if utilnet.IsIPv6(gatewayIfAddr.IP) {
			gatewayIPv6 = gatewayIfAddr.IP.String()
		} else {
			gatewayIPv4 = gatewayIfAddr.IP.String()
		}
	}
	return gatewayIPv4, gatewayIPv6
}

func getLocalAddrs() (map[string]net.IPNet, error) {
	localAddrSet := make(map[string]net.IPNet)
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	for _, addr := range addrs {
		ip, ipNet, err := net.ParseCIDR(addr.String())
		if err != nil {
			return nil, err
		}
		localAddrSet[ip.String()] = *ipNet
	}
	klog.V(5).Infof("Node local addresses initialized to: %v", localAddrSet)
	return localAddrSet, nil
}

func cleanupLocalnetGateway(physnet string) error {
	stdout, stderr, err := util.RunOVSVsctl("--if-exists", "get", "Open_vSwitch", ".",
		"external_ids:ovn-bridge-mappings")
	if err != nil {
		return fmt.Errorf("failed to get ovn-bridge-mappings stderr:%s (%v)", stderr, err)
	}
	bridgeMappings := strings.Split(stdout, ",")
	for _, bridgeMapping := range bridgeMappings {
		m := strings.Split(bridgeMapping, ":")
		if physnet == m[0] {
			bridgeName := m[1]
			_, stderr, err = util.RunOVSVsctl("--", "--if-exists", "del-br", bridgeName)
			if err != nil {
				return fmt.Errorf("failed to ovs-vsctl del-br %s stderr:%s (%v)", bridgeName, stderr, err)
			}
			break
		}
	}
	return err
}

func (g *gateway) getUDNVRFRules(nInfo util.NetInfo) ([]netlink.Rule, error) {
	var masqIPRules []netlink.Rule
	networkID, err := g.getNetworkID(nInfo)
	if err != nil {
		return nil, err
	}
	mgmtPortLinkName := util.GetNetworkScopedK8sMgmtHostIntfName(uint(networkID))
	ifIndex, err := util.GetIfIndex(mgmtPortLinkName)
	if err != nil {
		return nil, err
	}
	vrfTableId := util.CalculateRouteTableID(ifIndex)
	//return generateIPRulesForVRFInterface(vrfDeviceName, uint(vrfTableId)), nil
	masqIPv4, err := g.getV4MasqueradeIP(nInfo)
	if err != nil {
		return nil, err
	}
	if masqIPv4 != nil {
		masqIPRules = append(masqIPRules, generateIPRuleForMasqIP(*masqIPv4, false, uint(vrfTableId)))
	}
	masqIPv6, err := g.getV6MasqueradeIP(nInfo)
	if err != nil {
		return nil, err
	}
	if masqIPv6 != nil {
		masqIPRules = append(masqIPRules, generateIPRuleForMasqIP(*masqIPv6, true, uint(vrfTableId)))
	}
	return masqIPRules, nil
}

func (g *gateway) getV4MasqueradeIP(nInfo util.NetInfo) (*net.IP, error) {
	if !config.IPv4Mode {
		return nil, nil
	}
	networkID, err := g.getNetworkID(nInfo)
	if err != nil {
		return nil, err
	}
	masqIPs, err := udn.AllocateV4MasqueradeIPs(networkID)
	if err != nil {
		return nil, err
	}
	return &masqIPs.Local, nil
}

func (g *gateway) getV6MasqueradeIP(nInfo util.NetInfo) (*net.IP, error) {
	if !config.IPv6Mode {
		return nil, nil
	}
	networkID, err := g.getNetworkID(nInfo)
	if err != nil {
		return nil, err
	}
	masqIPs, err := udn.AllocateV6MasqueradeIPs(networkID)
	if err != nil {
		return nil, err
	}
	return &masqIPs.Local, nil
}

func (g *gateway) getNetworkID(nInfo util.NetInfo) (int, error) {
	node, err := g.watchFactory.GetNode(g.nodeIPManager.nodeName)
	if err != nil {
		return 0, err
	}
	networkID, err := util.ParseNetworkIDAnnotation(node, nInfo.GetNetworkName())
	if err != nil {
		return 0, err
	}
	return networkID, nil
}

func generateIPRuleForMasqIP(masqIP net.IP, isIPv6 bool, vrfTableId uint) netlink.Rule {
	r := *netlink.NewRule()
	r.Table = int(vrfTableId)
	r.Priority = types.MasqueradeIPRulePriority
	/*rinput := r
	rinput.IifName = vrfDeviceName
	routput := r
	routput.OifName = vrfDeviceName*/
	var ipFullMask string
	if isIPv6 {
		ipFullMask = fmt.Sprintf("%s/128", masqIP.String())
		r.Family = netlink.FAMILY_V6
	} else {
		ipFullMask = fmt.Sprintf("%s/32", masqIP.String())
		r.Family = netlink.FAMILY_V4
	}
	_, ipNet, _ := net.ParseCIDR(ipFullMask)
	r.Dst = ipNet
	//klog.Infof("SURYA %v/%v", rinput, routput)
	//return []netlink.Rule{rinput, routput}
	return r
}

func generateIPTablesSNATRuleArg(masqIP net.IP, isIPv6 bool, gwinfName, snatIP string) iptables.RuleArg {
	var srcIPFullMask string
	if isIPv6 {
		srcIPFullMask = fmt.Sprintf("%s/128", masqIP.String())
	} else {
		srcIPFullMask = fmt.Sprintf("%s/32", masqIP.String())
	}
	return iptables.RuleArg{Args: []string{"-s", srcIPFullMask, "-o", gwinfName, "-j", "SNAT", "--to-source", snatIP}}
}
