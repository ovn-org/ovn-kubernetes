//go:build linux
// +build linux

package node

import (
	"fmt"
	"net"
	"strings"
	"sync"

	"github.com/coreos/go-iptables/iptables"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
)

func newLocalGateway(nodeName string, hostSubnets []*net.IPNet, gwNextHops []net.IP, gwIntf string,
	gwIPs []*net.IPNet, nodeAnnotator kube.Annotator, cfg *managementPortConfig, kube kube.Interface, watchFactory factory.NodeWatchFactory) (*gateway, error) {
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
		err := initLocalGatewayNATRules(types.K8sMgmtIntfName, cidrNet)
		if err != nil {
			return nil, fmt.Errorf("failed to add local NAT rules for: %s, err: %v", types.K8sMgmtIntfName, err)
		}
	}

	gwBridge, _, err := gatewayInitInternal(
		nodeName, gwIntf, "", hostSubnets, gwNextHops, nil, nodeAnnotator)
	if err != nil {
		return nil, err
	}

	if config.OvnKubeNode.Mode == types.NodeModeFull {
		// add masquerade subnet route to avoid zeroconf routes
		err = addMasqueradeRoute(gwBridge.bridgeName, gwNextHops)
		if err != nil {
			return nil, err
		}
	}

	// OCP HACK -- block MCS ports https://github.com/openshift/ovn-kubernetes/pull/170
	rules := []iptRule{}
	if config.IPv4Mode {
		generateBlockMCSRules(&rules, iptables.ProtocolIPv4)
	}
	if config.IPv6Mode {
		generateBlockMCSRules(&rules, iptables.ProtocolIPv6)
	}
	if err := addIptRules(rules); err != nil {
		return nil, fmt.Errorf("failed to setup MCS-blocking rules: %w", err)
	}
	// END OCP HACK

	gw.readyFunc = func() (bool, error) {
		return gatewayReady(gwBridge.patchPort)
	}

	gw.initFunc = func() error {
		klog.Info("Creating Local Gateway Openflow Manager")
		err := setBridgeOfPorts(gwBridge)
		if err != nil {
			return err
		}
		gw.openflowManager, err = newLocalGatewayOpenflowManager(gwBridge)
		if err != nil {
			return err
		}

		gw.nodeIPManager = newAddressManager(nodeName, kube, cfg, watchFactory)

		if config.Gateway.NodeportEnable {
			gw.nodePortWatcher, err = newNodePortWatcher(gwBridge.patchPort, gwBridge.bridgeName, gwBridge.uplinkName, gwBridge.ips, gw.openflowManager, gw.nodeIPManager, watchFactory)
			if err != nil {
				return err
			}
		} else {
			// no service OpenFlows, request to sync flows now.
			gw.openflowManager.requestFlowSync()
		}
		return nil
	}

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

// since we share the host's k8s node IP, add OpenFlow flows
// -- to steer the NodePort traffic arriving on the host to the OVN logical topology and
// -- to also connection track the outbound north-south traffic through l3 gateway so that
//    the return traffic can be steered back to OVN logical topology
// -- to handle host -> service access, via masquerading from the host to OVN GR
// -- to handle external -> service(ExternalTrafficPolicy: Local) -> host access without SNAT
func newLocalGatewayOpenflowManager(gwBridge *bridgeConfiguration) (*openflowManager, error) {
	var dftFlows []string

	dftFlows, err := flowsForDefaultBridge(gwBridge.ofPortPhys, gwBridge.macAddress.String(), gwBridge.ofPortPatch,
		gwBridge.ofPortHost, gwBridge.ips)
	if err != nil {
		return nil, err
	}

	dftCommonFlows := commonFlows(gwBridge.ofPortPhys, gwBridge.macAddress.String(), gwBridge.ofPortPatch,
		gwBridge.ofPortHost)
	dftFlows = append(dftFlows, dftCommonFlows...)

	// add health check function to check default OpenFlow flows are on the shared gateway bridge
	ofm := &openflowManager{
		defaultBridge: gwBridge,
		flowCache:     make(map[string][]string),
		flowMutex:     sync.Mutex{},
		flowChan:      make(chan struct{}, 1),
	}
	ofm.updateFlowCacheEntry("NORMAL", []string{fmt.Sprintf("table=0,priority=0,actions=%s\n", util.NormalAction)})
	ofm.updateFlowCacheEntry("DEFAULT", dftFlows)
	ofm.requestFlowSync()
	return ofm, nil
}
