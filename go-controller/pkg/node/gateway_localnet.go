//go:build linux
// +build linux

package node

import (
	"fmt"
	"net"
	"strings"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/routemanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	utilerrors "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/errors"

	"k8s.io/klog/v2"
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
			for _, netConfig := range gwBridge.netConfig {
				ready, err := gatewayReady(netConfig.patchPort)
				if err != nil || !ready {
					return false, err
				}
			}
			for _, netConfig := range exGwBridge.netConfig {
				exGWReady, err := gatewayReady(netConfig.patchPort)
				if err != nil || !exGWReady {
					return false, err
				}
			}
			return true, nil
		}
	} else {
		gw.readyFunc = func() (bool, error) {
			for _, netConfig := range gwBridge.netConfig {
				ready, err := gatewayReady(netConfig.patchPort)
				if err != nil || !ready {
					return false, err
				}
			}
			return true, nil
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

		gw.nodeIPManager = newAddressManager(nodeName, kube, cfg, watchFactory, gwBridge)

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

		return nil
	}
	gw.watchFactory = watchFactory.(*factory.WatchFactory)
	klog.Info("Local Gateway Creation Complete")
	return gw, nil
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
