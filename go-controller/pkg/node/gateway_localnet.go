// +build linux

package node

import (
	"fmt"
	"net"
	"reflect"
	"strings"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"k8s.io/klog"

	kapi "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	utilnet "k8s.io/utils/net"
)

const (
	v4localnetGatewayIP           = "169.254.33.2"
	v4localnetGatewayNextHop      = "169.254.33.1"
	v4localnetGatewaySubnetPrefix = 24

	v6localnetGatewayIP           = "fd99::2"
	v6localnetGatewayNextHop      = "fd99::1"
	v6localnetGatewaySubnetPrefix = 64

	// localnetGatewayNextHopPort is the name of the gateway port on the host to which all
	// the packets leaving the OVN logical topology will be forwarded
	localnetGatewayNextHopPort       = "ovn-k8s-gw0"
	legacyLocalnetGatewayNextHopPort = "br-nexthop"
	// fixed MAC address for the br-nexthop interface. the last 4 hex bytes
	// translates to the br-nexthop's IP address
	localnetGatewayNextHopMac = "00:00:a9:fe:21:01"
	localnetGatewayMac        = "00:00:a9:fe:21:02"
	// Routing table for ExternalIP communication
	localnetGatewayExternalIDTable = "6"
)

func (n *OvnNode) initLocalnetGateway(hostSubnets []*net.IPNet, nodeAnnotator kube.Annotator, defaultGatewayIntf string) error {
	// Create a localnet OVS bridge.
	localnetBridgeName := "br-local"
	_, stderr, err := util.RunOVSVsctl("--may-exist", "add-br",
		localnetBridgeName)
	if err != nil {
		return fmt.Errorf("failed to create localnet bridge %s"+
			", stderr:%s (%v)", localnetBridgeName, stderr, err)
	}

	ifaceID, _, err := bridgedGatewayNodeSetup(n.name, localnetBridgeName, localnetBridgeName,
		util.PhysicalNetworkName, true)
	if err != nil {
		return fmt.Errorf("failed to set up shared interface gateway: %v", err)
	}
	_, err = util.LinkSetUp(localnetBridgeName)
	if err != nil {
		return err
	}

	// Create a localnet bridge gateway port
	_, stderr, err = util.RunOVSVsctl(
		"--if-exists", "del-port", localnetBridgeName, legacyLocalnetGatewayNextHopPort,
		"--", "--may-exist", "add-port", localnetBridgeName, localnetGatewayNextHopPort,
		"--", "set", "interface", localnetGatewayNextHopPort, "type=internal",
		"mtu_request="+fmt.Sprintf("%d", config.Default.MTU),
		fmt.Sprintf("mac=%s", strings.ReplaceAll(localnetGatewayNextHopMac, ":", "\\:")))
	if err != nil {
		return fmt.Errorf("failed to create localnet bridge gateway port %s"+
			", stderr:%s (%v)", localnetGatewayNextHopPort, stderr, err)
	}
	link, err := util.LinkSetUp(localnetGatewayNextHopPort)
	if err != nil {
		return err
	}
	// Flush any addresses on localnetGatewayNextHopPort first
	if err := util.LinkAddrFlush(link); err != nil {
		return err
	}

	// bounce interface to get link local address back for ipv6
	if _, err = util.LinkSetDown(localnetGatewayNextHopPort); err != nil {
		return err
	}
	if _, err = util.LinkSetUp(localnetGatewayNextHopPort); err != nil {
		return err
	}

	var gatewayIfAddrs []*net.IPNet
	var nextHops []net.IP
	for _, hostSubnet := range hostSubnets {
		var gatewayIP, gatewayNextHop net.IP
		var gatewaySubnetMask net.IPMask
		if utilnet.IsIPv6CIDR(hostSubnet) {
			gatewayIP = net.ParseIP(v6localnetGatewayIP)
			gatewayNextHop = net.ParseIP(v6localnetGatewayNextHop)
			gatewaySubnetMask = net.CIDRMask(v6localnetGatewaySubnetPrefix, 128)
		} else {
			gatewayIP = net.ParseIP(v4localnetGatewayIP)
			gatewayNextHop = net.ParseIP(v4localnetGatewayNextHop)
			gatewaySubnetMask = net.CIDRMask(v4localnetGatewaySubnetPrefix, 32)
		}
		gatewayIfAddr := &net.IPNet{IP: gatewayIP, Mask: gatewaySubnetMask}
		gatewayNextHopIfAddr := &net.IPNet{IP: gatewayNextHop, Mask: gatewaySubnetMask}
		gatewayIfAddrs = append(gatewayIfAddrs, gatewayIfAddr)
		nextHops = append(nextHops, gatewayNextHop)

		if err = util.LinkAddrAdd(link, gatewayNextHopIfAddr); err != nil {
			return err
		}
	}

	n.initLocalEgressIP(gatewayIfAddrs, defaultGatewayIntf)

	chassisID, err := util.GetNodeChassisID()
	if err != nil {
		return err
	}

	gwMacAddress, _ := net.ParseMAC(localnetGatewayMac)

	err = util.SetL3GatewayConfig(nodeAnnotator, &util.L3GatewayConfig{
		Mode:           config.GatewayModeLocal,
		ChassisID:      chassisID,
		InterfaceID:    ifaceID,
		MACAddress:     gwMacAddress,
		IPAddresses:    gatewayIfAddrs,
		NextHops:       nextHops,
		NodePortEnable: config.Gateway.NodeportEnable,
	})
	if err != nil {
		return err
	}

	for _, ifaddr := range gatewayIfAddrs {
		err = initLocalGatewayNATRules(localnetGatewayNextHopPort, ifaddr.IP)
		if err != nil {
			return fmt.Errorf("failed to add NAT rules for localnet gateway (%v)", err)
		}
	}

	if config.Gateway.NodeportEnable {
		localAddrSet, err := getLocalAddrs()
		if err != nil {
			return err
		}
		err = n.watchLocalPorts(
			newLocalPortWatcherData(gatewayIfAddrs, n.recorder, localAddrSet, nil),
		)
		if err != nil {
			return err
		}
	}

	return nil
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

func (n *OvnNode) initLocalEgressIP(gatewayIfAddrs []*net.IPNet, defaultGatewayIntf string) {
	if !config.OVNKubernetesFeature.EnableEgressIP {
		return
	}

	gatewayIPv4, gatewayIPv6 := getGatewayFamilyAddrs(gatewayIfAddrs)
	n.watchEgressIP(&egressIPLocal{
		gatewayIPv4:        gatewayIPv4,
		gatewayIPv6:        gatewayIPv6,
		nodeName:           n.name,
		defaultGatewayIntf: defaultGatewayIntf,
	})
}

type localPortWatcherData struct {
	recorder     record.EventRecorder
	gatewayIPv4  string
	gatewayIPv6  string
	localAddrSet map[string]net.IPNet
	nodeIP       *net.IPNet
}

func newLocalPortWatcherData(gatewayIfAddrs []*net.IPNet, recorder record.EventRecorder, localAddrSet map[string]net.IPNet, nodeIP []*net.IPNet) *localPortWatcherData {
	gatewayIPv4, gatewayIPv6 := getGatewayFamilyAddrs(gatewayIfAddrs)

	lpw := &localPortWatcherData{
		gatewayIPv4:  gatewayIPv4,
		gatewayIPv6:  gatewayIPv6,
		recorder:     recorder,
		localAddrSet: localAddrSet,
	}

	if len(nodeIP) > 0 {
		lpw.nodeIP = nodeIP[0]
	}
	return lpw
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

func (npw *localPortWatcherData) networkHasAddress(ip net.IP) bool {
	for _, net := range npw.localAddrSet {
		if net.Contains(ip) {
			return true
		}
	}
	return false
}

func (npw *localPortWatcherData) addService(svc *kapi.Service) error {
	iptRules := []iptRule{}
	isIPv6Service := utilnet.IsIPv6String(svc.Spec.ClusterIP)
	gatewayIP := npw.gatewayIPv4
	if isIPv6Service {
		gatewayIP = npw.gatewayIPv6
	}
	for _, port := range svc.Spec.Ports {
		if util.ServiceTypeHasNodePort(svc) {
			if err := util.ValidatePort(port.Protocol, port.NodePort); err != nil {
				klog.Warningf("Invalid service node port %s, err: %v", port.Name, err)
				continue
			}

			if gatewayIP != "" {
				// Fix Azure/GCP LoadBalancers. They will forward traffic directly to the node with the
				// dest address as the load-balancer ingress IP and port
				iptRules = append(iptRules, getLoadBalancerIPTRules(svc, port, gatewayIP, port.NodePort)...)
				iptRules = append(iptRules, getNodePortIPTRules(port, nil, gatewayIP, port.NodePort)...)
				if config.Gateway.Mode == config.GatewayModeHybrid {
					iptRules = append(iptRules, getHybridNodePortIPTRules(port, npw.nodeIP, svc.Spec.ClusterIP)...)
				}
				klog.V(5).Infof("Will add iptables rule for NodePort and Cloud load balancers: %v and "+
					"protocol: %v", port.NodePort, port.Protocol)
			} else {
				klog.Warningf("No gateway of appropriate IP family for NodePort Service %s/%s %s",
					svc.Namespace, svc.Name, svc.Spec.ClusterIP)
			}
		}
		for _, externalIP := range svc.Spec.ExternalIPs {
			if err := util.ValidatePort(port.Protocol, port.Port); err != nil {
				klog.Warningf("Invalid service port %s, err: %v", port.Name, err)
				break
			}
			if utilnet.IsIPv6String(externalIP) != isIPv6Service {
				klog.Warningf("Invalid ExternalIP %s for Service %s/%s with ClusterIP %s",
					externalIP, svc.Namespace, svc.Name, svc.Spec.ClusterIP)
				continue
			}
			if _, exists := npw.localAddrSet[externalIP]; exists {
				if !util.IsClusterIPSet(svc) {
					serviceRef := kapi.ObjectReference{
						Kind:      "Service",
						Namespace: svc.Namespace,
						Name:      svc.Name,
					}
					npw.recorder.Eventf(&serviceRef, kapi.EventTypeWarning, "UnsupportedServiceDefinition", "Unsupported service definition, headless service: %s with a local ExternalIP is not supported by ovn-kubernetes in local gateway mode", svc.Name)
					klog.Warningf("UnsupportedServiceDefinition event for service %s in namespace %s", svc.Name, svc.Namespace)
					continue
				}
				iptRules = append(iptRules, getExternalIPTRules(port, externalIP, svc.Spec.ClusterIP)...)
				klog.V(5).Infof("Will add iptables rule for ExternalIP: %s", externalIP)
			} else if npw.networkHasAddress(net.ParseIP(externalIP)) {
				klog.V(5).Infof("ExternalIP: %s is reachable through one of the interfaces on this node, will skip setup", externalIP)
			} else {
				if gatewayIP != "" {
					if stdout, stderr, err := util.RunIP("route", "replace", externalIP, "via", gatewayIP, "dev", localnetGatewayNextHopPort, "table", localnetGatewayExternalIDTable); err != nil {
						klog.Errorf("Error adding routing table entry for ExternalIP %s: stdout: %s, stderr: %s, err: %v", externalIP, stdout, stderr, err)
					} else {
						klog.V(5).Infof("Successfully added route for ExternalIP: %s", externalIP)
					}
				} else {
					klog.Warningf("No gateway of appropriate IP family for ExternalIP %s for Service %s/%s",
						externalIP, svc.Namespace, svc.Name)
				}
			}
		}
	}
	klog.V(5).Infof("Adding iptables rules: %v for service: %v", iptRules, svc.Name)
	return addIptRules(iptRules)
}

func (npw *localPortWatcherData) deleteService(svc *kapi.Service) error {
	iptRules := []iptRule{}
	isIPv6Service := utilnet.IsIPv6String(svc.Spec.ClusterIP)
	gatewayIP := npw.gatewayIPv4
	if isIPv6Service {
		gatewayIP = npw.gatewayIPv6
	}
	// Note that unlike with addService we just silently ignore IPv4/IPv6 mismatches here
	for _, port := range svc.Spec.Ports {
		if util.ServiceTypeHasNodePort(svc) {
			if gatewayIP != "" {
				// Fix Azure/GCP LoadBalancers. They will forward traffic directly to the node with the
				// dest address as the load-balancer ingress IP and port
				iptRules = append(iptRules, getLoadBalancerIPTRules(svc, port, gatewayIP, port.NodePort)...)
				iptRules = append(iptRules, getNodePortIPTRules(port, nil, gatewayIP, port.NodePort)...)
				if config.Gateway.Mode == config.GatewayModeHybrid {
					iptRules = append(iptRules, getHybridNodePortIPTRules(port, npw.nodeIP, svc.Spec.ClusterIP)...)
				}
				klog.V(5).Infof("Will delete iptables rule for NodePort and cloud load balancers: %v and "+
					"protocol: %v", port.NodePort, port.Protocol)
			}
		}
		for _, externalIP := range svc.Spec.ExternalIPs {
			if utilnet.IsIPv6String(externalIP) != isIPv6Service {
				continue
			}
			if _, exists := npw.localAddrSet[externalIP]; exists {
				iptRules = append(iptRules, getExternalIPTRules(port, externalIP, svc.Spec.ClusterIP)...)
				klog.V(5).Infof("Will delete iptables rule for ExternalIP: %s", externalIP)
			} else if npw.networkHasAddress(net.ParseIP(externalIP)) {
				klog.V(5).Infof("ExternalIP: %s is reachable through one of the interfaces on this node, will skip cleanup", externalIP)
			} else {
				if gatewayIP != "" {
					if stdout, stderr, err := util.RunIP("route", "del", externalIP, "via", gatewayIP, "dev", localnetGatewayNextHopPort, "table", localnetGatewayExternalIDTable); err != nil {
						klog.Errorf("Error delete routing table entry for ExternalIP %s: stdout: %s, stderr: %s, err: %v", externalIP, stdout, stderr, err)
					} else {
						klog.V(5).Infof("Successfully deleted route for ExternalIP: %s", externalIP)
					}
				}
			}
		}
	}
	klog.V(5).Infof("Deleting iptables rules: %v for service: %v", iptRules, svc.Name)
	return delIptRules(iptRules)
}

func (npw *localPortWatcherData) syncServices(serviceInterface []interface{}) {
	removeStaleRoutes := func(keepRoutes []string) {
		stdout, stderr, err := util.RunIP("route", "list", "table", localnetGatewayExternalIDTable)
		if err != nil || stdout == "" {
			klog.Infof("No routing table entries for ExternalIP table %s: stdout: %s, stderr: %s, err: %v", localnetGatewayExternalIDTable, stdout, stderr, err)
			return
		}
		for _, existingRoute := range strings.Split(stdout, "\n") {
			isFound := false
			for _, keepRoute := range keepRoutes {
				if strings.Contains(existingRoute, keepRoute) {
					isFound = true
					break
				}
			}
			if !isFound {
				klog.V(5).Infof("Deleting stale routing rule: %s", existingRoute)
				if _, stderr, err := util.RunIP("route", "del", existingRoute, "table", localnetGatewayExternalIDTable); err != nil {
					klog.Errorf("Error deleting stale routing rule: stderr: %s, err: %v", stderr, err)
				}
			}
		}
	}
	keepIPTRules := []iptRule{}
	keepRoutes := []string{}
	for _, service := range serviceInterface {
		svc, ok := service.(*kapi.Service)
		if !ok {
			klog.Errorf("Spurious object in syncServices: %v", serviceInterface)
			continue
		}
		gatewayIP := npw.gatewayIPv4
		if utilnet.IsIPv6String(svc.Spec.ClusterIP) {
			gatewayIP = npw.gatewayIPv6
		}
		if gatewayIP != "" {
			keepIPTRules = append(keepIPTRules, getGatewayIPTRules(svc, gatewayIP, nil)...)
		}
		keepRoutes = append(keepRoutes, svc.Spec.ExternalIPs...)
	}
	for _, chain := range []string{iptableNodePortChain, iptableExternalIPChain} {
		recreateIPTRules("nat", chain, keepIPTRules)
		recreateIPTRules("filter", chain, keepIPTRules)
	}
	removeStaleRoutes(keepRoutes)
}

func initRoutingRules() error {
	stdout, stderr, err := util.RunIP("rule")
	if err != nil {
		return fmt.Errorf("error listing routing rules, stdout: %s, stderr: %s, err: %v", stdout, stderr, err)
	}
	if !strings.Contains(stdout, fmt.Sprintf("from all lookup %s", localnetGatewayExternalIDTable)) {
		if stdout, stderr, err := util.RunIP("rule", "add", "from", "all", "table", localnetGatewayExternalIDTable); err != nil {
			return fmt.Errorf("error adding routing rule for ExternalIP table (%s): stdout: %s, stderr: %s, err: %v", localnetGatewayExternalIDTable, stdout, stderr, err)
		}
	}
	return nil
}

func (n *OvnNode) watchLocalPorts(npw *localPortWatcherData) error {
	if err := initLocalGatewayIPTables(); err != nil {
		return err
	}
	if err := initRoutingRules(); err != nil {
		return err
	}
	n.watchFactory.AddServiceHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			svc := obj.(*kapi.Service)
			err := npw.addService(svc)
			if err != nil {
				klog.Errorf("Error in adding service: %v", err)
			}
		},
		UpdateFunc: func(old, new interface{}) {
			svcNew := new.(*kapi.Service)
			svcOld := old.(*kapi.Service)
			if reflect.DeepEqual(svcNew.Spec, svcOld.Spec) &&
				reflect.DeepEqual(svcNew.Status, svcOld.Status) {
				return
			}
			err := npw.deleteService(svcOld)
			if err != nil {
				klog.Errorf("Error in deleting service - %v", err)
			}
			err = npw.addService(svcNew)
			if err != nil {
				klog.Errorf("Error in modifying service: %v", err)
			}
		},
		DeleteFunc: func(obj interface{}) {
			svc := obj.(*kapi.Service)
			err := npw.deleteService(svc)
			if err != nil {
				klog.Errorf("Error in deleting service - %v", err)
			}
		},
	}, npw.syncServices)
	return nil
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

func getLoadBalancerIPTRules(svc *kapi.Service, svcPort kapi.ServicePort, gatewayIP string, targetPort int32) []iptRule {
	var rules []iptRule
	ingPort := fmt.Sprintf("%d", svcPort.Port)
	for _, ing := range svc.Status.LoadBalancer.Ingress {
		if ing.IP == "" {
			continue
		}
		rules = append(rules, iptRule{
			table: "nat",
			chain: iptableNodePortChain,
			args: []string{
				"-d", ing.IP,
				"-p", string(svcPort.Protocol), "--dport", ingPort,
				"-j", "DNAT", "--to-destination", util.JoinHostPortInt32(gatewayIP, targetPort),
			},
		})
		rules = append(rules, iptRule{
			table: "filter",
			chain: iptableNodePortChain,
			args: []string{
				"-d", ing.IP,
				"-p", string(svcPort.Protocol), "--dport", ingPort,
				"-j", "ACCEPT",
			},
		})
	}
	return rules
}
