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
	// Routing table for ExternalIP communication
	localnetGatewayExternalIDTable = "6"
)

func (n *OvnNode) initLocalnetGateway(subnet *net.IPNet, nodeAnnotator kube.Annotator) error {
	// Create a localnet OVS bridge.
	localnetBridgeName := "br-local"
	_, stderr, err := util.RunOVSVsctl("--may-exist", "add-br",
		localnetBridgeName)
	if err != nil {
		return fmt.Errorf("failed to create localnet bridge %s"+
			", stderr:%s (%v)", localnetBridgeName, stderr, err)
	}

	ifaceID, macAddress, err := bridgedGatewayNodeSetup(n.name, localnetBridgeName, localnetBridgeName,
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

	var gatewayIP, gatewayNextHop net.IP
	var gatewaySubnetMask net.IPMask
	if utilnet.IsIPv6CIDR(subnet) {
		gatewayIP = net.ParseIP(v6localnetGatewayIP)
		gatewayNextHop = net.ParseIP(v6localnetGatewayNextHop)
		gatewaySubnetMask = net.CIDRMask(v6localnetGatewaySubnetPrefix, 128)
	} else {
		gatewayIP = net.ParseIP(v4localnetGatewayIP)
		gatewayNextHop = net.ParseIP(v4localnetGatewayNextHop)
		gatewaySubnetMask = net.CIDRMask(v4localnetGatewaySubnetPrefix, 32)
	}
	gatewayIPCIDR := &net.IPNet{IP: gatewayIP, Mask: gatewaySubnetMask}
	gatewayNextHopCIDR := &net.IPNet{IP: gatewayNextHop, Mask: gatewaySubnetMask}

	// Flush any addresses on localnetBridgeNextHopPort and add the new IP address.
	if err = util.LinkAddrFlush(link); err == nil {
		err = util.LinkAddrAdd(link, gatewayNextHopCIDR)
	}
	if err != nil {
		return err
	}

	chassisID, err := util.GetNodeChassisID()
	if err != nil {
		return err
	}

	err = util.SetL3GatewayConfig(nodeAnnotator, &util.L3GatewayConfig{
		Mode:           config.GatewayModeLocal,
		ChassisID:      chassisID,
		InterfaceID:    ifaceID,
		MACAddress:     macAddress,
		IPAddresses:    []*net.IPNet{gatewayIPCIDR},
		NextHops:       []net.IP{gatewayNextHop},
		NodePortEnable: config.Gateway.NodeportEnable,
	})
	if err != nil {
		return err
	}

	if utilnet.IsIPv6CIDR(subnet) {
		// TODO - IPv6 hack ... for some reason neighbor discovery isn't working here, so hard code a
		// MAC binding for the gateway IP address for now - need to debug this further
		err = util.LinkNeighAdd(link, gatewayIP, macAddress)
		if err == nil {
			klog.Infof("Added MAC binding for %s on %s", gatewayIP, localnetGatewayNextHopPort)
		} else {
			klog.Errorf("Error in adding MAC binding for %s on %s: %v", gatewayIP, localnetGatewayNextHopPort, err)
		}
	}

	err = initLocalGatewayNATRules(localnetGatewayNextHopPort, gatewayIP)
	if err != nil {
		return fmt.Errorf("failed to add NAT rules for localnet gateway (%v)", err)
	}

	if config.Gateway.NodeportEnable {
		localAddrSet, err := getLocalAddrs()
		if err != nil {
			return err
		}
		err = n.watchLocalPorts(
			newLocalPortWatcherData(gatewayIP, n.recorder, localAddrSet),
		)
		if err != nil {
			return err
		}
	}

	return err
}

type localPortWatcherData struct {
	recorder     record.EventRecorder
	gatewayIP    string
	localAddrSet map[string]net.IPNet
}

func newLocalPortWatcherData(gatewayIP net.IP, recorder record.EventRecorder, localAddrSet map[string]net.IPNet) *localPortWatcherData {
	return &localPortWatcherData{
		gatewayIP:    gatewayIP.String(),
		recorder:     recorder,
		localAddrSet: localAddrSet,
	}
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
	for _, port := range svc.Spec.Ports {
		if util.ServiceTypeHasNodePort(svc) {
			if err := util.ValidatePort(port.Protocol, port.NodePort); err != nil {
				klog.Warningf("Invalid service node port %s, err: %v", port.Name, err)
				continue
			}
			iptRules = append(iptRules, getNodePortIPTRules(port, nil, npw.gatewayIP, port.NodePort)...)
			klog.V(5).Infof("Will add iptables rule for NodePort: %v and protocol: %v", port.NodePort, port.Protocol)
		}
		for _, externalIP := range svc.Spec.ExternalIPs {
			if err := util.ValidatePort(port.Protocol, port.Port); err != nil {
				klog.Warningf("Invalid service port %s, err: %v", port.Name, err)
				break
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
				if stdout, stderr, err := util.RunIP("route", "replace", externalIP, "via", npw.gatewayIP, "dev", localnetGatewayNextHopPort, "table", localnetGatewayExternalIDTable); err != nil {
					klog.Errorf("Error adding routing table entry for ExternalIP %s: stdout: %s, stderr: %s, err: %v", externalIP, stdout, stderr, err)
				} else {
					klog.V(5).Infof("Successfully added route for ExternalIP: %s", externalIP)
				}
			}
		}
	}
	klog.V(5).Infof("Adding iptables rules: %v for service: %v", iptRules, svc.Name)
	return addIptRules(iptRules)
}

func (npw *localPortWatcherData) deleteService(svc *kapi.Service) error {
	iptRules := []iptRule{}
	for _, port := range svc.Spec.Ports {
		if util.ServiceTypeHasNodePort(svc) {
			iptRules = append(iptRules, getNodePortIPTRules(port, nil, npw.gatewayIP, port.NodePort)...)
			klog.V(5).Infof("Will delete iptables rule for NodePort: %v and protocol: %v", port.NodePort, port.Protocol)
		}
		for _, externalIP := range svc.Spec.ExternalIPs {
			if _, exists := npw.localAddrSet[externalIP]; exists {
				iptRules = append(iptRules, getExternalIPTRules(port, externalIP, svc.Spec.ClusterIP)...)
				klog.V(5).Infof("Will delete iptables rule for ExternalIP: %s", externalIP)
			} else if npw.networkHasAddress(net.ParseIP(externalIP)) {
				klog.V(5).Infof("ExternalIP: %s is reachable through one of the interfaces on this node, will skip cleanup", externalIP)
			} else {
				if stdout, stderr, err := util.RunIP("route", "del", externalIP, "via", npw.gatewayIP, "dev", localnetGatewayNextHopPort, "table", localnetGatewayExternalIDTable); err != nil {
					klog.Errorf("Error delete routing table entry for ExternalIP %s: stdout: %s, stderr: %s, err: %v", externalIP, stdout, stderr, err)
				} else {
					klog.V(5).Infof("Successfully deleted route for ExternalIP: %s", externalIP)
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
		keepIPTRules = append(keepIPTRules, getGatewayIPTRules(svc, npw.gatewayIP, nil)...)
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
	_, err := n.watchFactory.AddServiceHandler(cache.ResourceEventHandlerFuncs{
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
			if reflect.DeepEqual(svcNew.Spec, svcOld.Spec) {
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
	return err
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
