// +build linux

package node

import (
	//"debug/elf"
	"fmt"
	"github.com/vishvananda/netlink"
	"net"
	"reflect"
	"strings"

	"github.com/coreos/go-iptables/iptables"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"k8s.io/klog"

	kapi "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/cache"
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
	iptableNodePortChain      = "OVN-KUBE-NODEPORT"
	IPv4                      = 4
	IPv6                      = 6
	TotalSupportedSubnets     = 2
	SupportedSubnetsPerFamily = 1
)

type iptRule struct {
	table string
	chain string
	args  []string
}

func ensureChain(ipt util.IPTablesHelper, table, chain string) error {
	chains, err := ipt.ListChains(table)
	if err != nil {
		return fmt.Errorf("failed to list iptables chains: %v", err)
	}
	for _, ch := range chains {
		if ch == chain {
			return nil
		}
	}

	return ipt.NewChain(table, chain)
}

func addIptRules(ipt util.IPTablesHelper, rules []iptRule) error {
	for _, r := range rules {
		if err := ensureChain(ipt, r.table, r.chain); err != nil {
			return fmt.Errorf("failed to ensure %s/%s: %v", r.table, r.chain, err)
		}
		exists, err := ipt.Exists(r.table, r.chain, r.args...)
		if !exists && err == nil {
			err = ipt.Insert(r.table, r.chain, 1, r.args...)
		}
		if err != nil {
			return fmt.Errorf("failed to add iptables %s/%s rule %q: %v",
				r.table, r.chain, strings.Join(r.args, " "), err)
		}
	}

	return nil
}

func delIptRules(ipt util.IPTablesHelper, rules []iptRule) {
	for _, r := range rules {
		err := ipt.Delete(r.table, r.chain, r.args...)
		if err != nil {
			klog.Warningf("failed to delete iptables %s/%s rule %q: %v", r.table, r.chain,
				strings.Join(r.args, " "), err)
		}
	}
}

func generateGatewayNATRules(ifname string, ip net.IP) []iptRule {
	// Allow packets to/from the gateway interface in case defaults deny
	rules := make([]iptRule, 0)
	rules = append(rules, iptRule{
		table: "filter",
		chain: "FORWARD",
		args:  []string{"-i", ifname, "-j", "ACCEPT"},
	})
	rules = append(rules, iptRule{
		table: "filter",
		chain: "FORWARD",
		args: []string{"-o", ifname, "-m", "conntrack", "--ctstate",
			"RELATED,ESTABLISHED", "-j", "ACCEPT"},
	})
	rules = append(rules, iptRule{
		table: "filter",
		chain: "INPUT",
		args:  []string{"-i", ifname, "-m", "comment", "--comment", "from OVN to localhost", "-j", "ACCEPT"},
	})

	// NAT for the interface
	rules = append(rules, iptRule{
		table: "nat",
		chain: "POSTROUTING",
		args:  []string{"-s", ip.String(), "-j", "MASQUERADE"},
	})
	return rules
}

func localnetGatewayNAT(localNetdata []*localnetData, ifname string) error {
	for _, lnData := range localNetdata {
		rules := generateGatewayNATRules(ifname, lnData.gatewayIP)
		err := addIptRules(lnData.ipt, rules)
		if err != nil {
			return err
		}
	}
	return nil
}

func initLocalnetGateway(nodeName string, subnets []*net.IPNet, wf *factory.WatchFactory, nodeAnnotator kube.Annotator) error {

	localnetBridgeName := "br-local"

	// Evaluate input subnets
	err := evaluateInputNetworks(subnets)
	if err != nil {
		return err
	}

	// Setup local brifge and ports
	ifaceID, macAddress, link, err := createAndSetupLocalBridgePorts(localnetBridgeName, nodeName)
	if err != nil {
		return err
	}

	// Create IpData structure required for all subseqhent operations
	ipData, err := constructIPData(subnets)
	if err != nil {
		return err
	}

	// Set the link with the addresses from IP object

	err = addLinkAddresses(link, ipData)
	if err != nil {
		return err
	}

	// Setup L3 Gateway

	err = setupL3Gateway(nodeAnnotator, ifaceID, macAddress, ipData)
	if err != nil {
		return err
	}

	// TODO - IPv6 hack ... for some reason neighbor discovery isn't working here, so hard code a
	// MAC binding for the gateway IP address for now - need to debug this further

	ipV6MacBindingWorkaround(link, macAddress, ipData)

	// Install gateway NAT rules for dual stack
	err = localnetGatewayNAT(ipData, localnetGatewayNextHopPort)
	if err != nil {
		return fmt.Errorf("Failed to add NAT rules for localnet gateway (%v)", err)
	}

	// Set up node port services if enabled
	if config.Gateway.NodeportEnable {
		err = localnetNodePortWatcher(ipData, wf)
	}
	return err
}

func createAndSetupLocalBridgePorts(localnetBridgeName string, nodeName string) (string, net.HardwareAddr, netlink.Link, error) {

	// Create a localnet OVS bridge.
	err := createLocalnetBridge(localnetBridgeName)
	if err != nil {
		return "", nil, nil, err
	}

	// Setup Bridge gateway
	ifaceID, macAddress, err := bridgedGatewayNodeSetup(nodeName, localnetBridgeName, localnetBridgeName, true)
	if err != nil {
		return "", nil, nil, fmt.Errorf("failed to set up shared interface gateway: %v", err)
	}

	// Link set up for bridge
	_, err = setUpLink(localnetBridgeName)
	if err != nil {
		return "", nil, nil, err
	}

	// Create a localnet bridge gateway port
	err = createGatewayPort(localnetBridgeName)
	if err != nil {
		return "", nil, nil, err
	}

	//Setup link for port
	link, err := setUpLink(localnetGatewayNextHopPort)
	if err != nil {
		return "", nil, nil, err
	}
	return ifaceID, macAddress, link, nil
}

func createGatewayPort(localnetBridgeName string) error {
	_, stderr, err := util.RunOVSVsctl(
		"--if-exists", "del-port", localnetBridgeName, legacyLocalnetGatewayNextHopPort,
		"--", "--may-exist", "add-port", localnetBridgeName, localnetGatewayNextHopPort,
		"--", "set", "interface", localnetGatewayNextHopPort, "type=internal",
		"mtu_request="+fmt.Sprintf("%d", config.Default.MTU),
		fmt.Sprintf("mac=%s", strings.ReplaceAll(localnetGatewayNextHopMac, ":", "\\:")))
	if err != nil {
		return fmt.Errorf("Failed to create localnet bridge gateway port %s"+
			", stderr:%s (%v)", localnetGatewayNextHopPort, stderr, err)
	}
	return nil
}

func setUpLink(localnetBridgeName string) (netlink.Link, error) {
	link, err := util.LinkSetUp(localnetBridgeName)
	if err != nil {
		return nil, err
	}
	return link, nil
}

func createLocalnetBridge(localnetBridgeName string) error {
	_, stderr, err := util.RunOVSVsctl("--may-exist", "add-br",
		localnetBridgeName)
	if err != nil {
		return fmt.Errorf("Failed to create localnet bridge %s"+
			", stderr:%s (%v)", localnetBridgeName, stderr, err)
	}
	return nil
}

func ipV6MacBindingWorkaround(link netlink.Link, macAddress net.HardwareAddr,
	localNetdata []*localnetData) {
	for _, lnData := range localNetdata {
		if lnData.ipVersion == IPv6 {
			gatewayIP := lnData.gatewayIP
			err := util.LinkNeighAdd(link, gatewayIP, macAddress)
			if err == nil {
				klog.Infof("Added MAC binding for %s on %s", gatewayIP, localnetGatewayNextHopPort)
			} else {
				klog.Errorf("Error in adding MAC binding for %s on %s: %v", gatewayIP, localnetGatewayNextHopPort, err)
			}
		}
	}
}

func setupL3Gateway(nodeAnnotator kube.Annotator, ifaceID string,
	macAddress net.HardwareAddr, localNetdata []*localnetData) error {

	// Get the chassisID
	chassisID, err := util.GetNodeChassisID()
	if err != nil {
		return err
	}

	ipAddrs := make([]*net.IPNet, 0)
	nextHops := make([]net.IP, 0)

	for _, lnData := range localNetdata {
		ipAddrs = append(ipAddrs, lnData.gatewayIPCIDR)
		nextHops = append(nextHops, lnData.gatewayNextHop)
	}

	return util.SetL3GatewayConfig(nodeAnnotator, &util.L3GatewayConfig{
		Mode:           config.GatewayModeLocal,
		ChassisID:      chassisID,
		InterfaceID:    ifaceID,
		MACAddress:     macAddress,
		IPAddresses:    ipAddrs,
		NextHops:       nextHops,
		NodePortEnable: config.Gateway.NodeportEnable,
	})

}

func addLinkAddresses(link netlink.Link, ipData []*localnetData) error {

	// First flush addressed
	err := flushLinkAddress(link)
	if err != nil {
		return err
	}

	// Now add all addresses
	for _, data := range ipData {
		err := util.LinkAddrAdd(link, data.gatewayNextHopCIDR)
		if err != nil {
			return err
		}
	}

	return nil
}

func flushLinkAddress(link netlink.Link) error {

	return util.LinkAddrFlush(link)

}

type localnetData struct {
	ipVersion                         int
	ipt                               util.IPTablesHelper
	gatewayIP, gatewayNextHop         net.IP
	gatewaySubnetMask                 net.IPMask
	gatewayIPCIDR, gatewayNextHopCIDR *net.IPNet
}

// Function that populates releavant IP structures

func evaluateInputNetworks(subnets []*net.IPNet) error {

	if len(subnets) > TotalSupportedSubnets {
		return fmt.Errorf("Number of subnets provided is : %d  which is more than %d", len(subnets), TotalSupportedSubnets)
	}
	var ipV4NetworkCount, ipV6NetworkCount int
	for _, subnet := range subnets {
		if utilnet.IsIPv6CIDR(subnet) {
			ipV6NetworkCount++
		} else {
			ipV4NetworkCount++
		}
	}
	if ipV4NetworkCount > SupportedSubnetsPerFamily || ipV6NetworkCount > SupportedSubnetsPerFamily {
		return fmt.Errorf("Only one IPv4 and/or IPv6 subnets can be specified. Specified %d IP v4 and %d "+
			"IP v6 subnets", ipV4NetworkCount, ipV6NetworkCount)
	}
	return nil
}

func constructIPData(subnets []*net.IPNet) ([]*localnetData, error) {
	netData := make([]*localnetData, 0)
	for _, subnet := range subnets {
		if utilnet.IsIPv6CIDR(subnet) {
			data, err := newIPV6Data()
			if err != nil {
				return nil, err
			}
			netData = append(netData, data)
		} else {
			data, err := newIPV4Data()
			if err != nil {
				return nil, err
			}
			netData = append(netData, data)
		}
	}
	return netData, nil
}

func newIPV4Data() (*localnetData, error) {

	ipt, err := util.GetIPTablesHelper(iptables.ProtocolIPv4)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize iptables: %v", err)
	}
	gatewayIP := net.ParseIP(v4localnetGatewayIP)
	gatewayNextHop := net.ParseIP(v4localnetGatewayNextHop)
	gatewaySubnetMask := net.CIDRMask(v4localnetGatewaySubnetPrefix, 32)

	data := newIPData(IPv4, ipt, gatewayIP, gatewayNextHop, gatewaySubnetMask)
	return data, nil
}

func newIPV6Data() (*localnetData, error) {

	ipt, err := util.GetIPTablesHelper(iptables.ProtocolIPv6)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize iptables: %v", err)
	}
	gatewayIP := net.ParseIP(v6localnetGatewayIP)
	gatewayNextHop := net.ParseIP(v6localnetGatewayNextHop)
	gatewaySubnetMask := net.CIDRMask(v6localnetGatewaySubnetPrefix, 128)

	data := newIPData(IPv6, ipt, gatewayIP, gatewayNextHop, gatewaySubnetMask)
	return data, nil
}

func newIPData(iPVersion int, ipt util.IPTablesHelper, gatewayIP net.IP, gatewayNextHop net.IP, gatewaySubnetMask net.IPMask) *localnetData {
	data := &localnetData{
		ipVersion:          iPVersion,
		ipt:                ipt,
		gatewayIP:          gatewayIP,
		gatewayNextHop:     gatewayNextHop,
		gatewaySubnetMask:  gatewaySubnetMask,
		gatewayIPCIDR:      &net.IPNet{IP: gatewayIP, Mask: gatewaySubnetMask},
		gatewayNextHopCIDR: &net.IPNet{IP: gatewayNextHop, Mask: gatewaySubnetMask},
	}
	return data
}

func localnetIptRules(svc *kapi.Service, gatewayIP string) []iptRule {
	rules := make([]iptRule, 0)
	for _, svcPort := range svc.Spec.Ports {
		protocol, err := util.ValidateProtocol(svcPort.Protocol)
		if err != nil {
			klog.Errorf("Invalid service port %s: %v", svcPort.Name, err)
			continue
		}

		nodePort := fmt.Sprintf("%d", svcPort.NodePort)
		rules = append(rules, iptRule{
			table: "nat",
			chain: iptableNodePortChain,
			args: []string{
				"-p", string(protocol), "--dport", nodePort,
				"-j", "DNAT", "--to-destination", net.JoinHostPort(gatewayIP, nodePort),
			},
		})
		rules = append(rules, iptRule{
			table: "filter",
			chain: iptableNodePortChain,
			args: []string{
				"-p", string(protocol), "--dport", nodePort,
				"-j", "ACCEPT",
			},
		})
	}
	return rules
}

type localnetNodePortWatcherData struct {
	localNetdata []*localnetData
}

func (npw *localnetNodePortWatcherData) addService(svc *kapi.Service) error {
	if !util.ServiceTypeHasNodePort(svc) {
		return nil
	}
	lnData := getLocalNetDataForService(npw.localNetdata, svc)
	rules := localnetIptRules(svc, lnData.gatewayIP.String())
	klog.V(5).Infof("Add rules %v for service %v", rules, svc.Name)
	return addIptRules(lnData.ipt, rules)
}

func (npw *localnetNodePortWatcherData) deleteService(svc *kapi.Service) error {
	if !util.ServiceTypeHasNodePort(svc) {
		return nil
	}
	lnData := getLocalNetDataForService(npw.localNetdata, svc)
	rules := localnetIptRules(svc, lnData.gatewayIP.String())
	klog.V(5).Infof("Delete rules %v for service %v", rules, svc.Name)
	delIptRules(lnData.ipt, rules)
	return nil
}

func getLocalNetDataForService(localNetdata []*localnetData, svc *kapi.Service) *localnetData {
	var ipVersion = IPv4
	if utilnet.IsIPv6String(svc.Spec.ClusterIP) {
		ipVersion = IPv6
	}
	for _, lnData := range localNetdata {
		if lnData.ipVersion == ipVersion {
			return lnData
		}
	}
	return nil
}

func addDualStackIptRules(localNetdata []*localnetData, rules []iptRule) error {
	for _, lndata := range localNetdata {
		err := addIptRules(lndata.ipt, rules)
		if err != nil {
			return err
		}
	}
	return nil

}

func clearOvnNodeportRules(localNetdata []*localnetData) {
	// TODO: Add a localnetSyncService method to remove the stale entries only
	for _, lndata := range localNetdata {
		_ = lndata.ipt.ClearChain("nat", iptableNodePortChain)
		_ = lndata.ipt.ClearChain("filter", iptableNodePortChain)
	}
}

func localnetNodePortWatcher(localNetdata []*localnetData, wf *factory.WatchFactory) error {
	// delete all the existing OVN-NODEPORT rules
	clearOvnNodeportRules(localNetdata)

	rules := constructBaseIptRules()

	if err := addDualStackIptRules(localNetdata, rules); err != nil {
		return err
	}

	npw := &localnetNodePortWatcherData{localNetdata}
	err := addServiuceHandler(wf, npw)
	return err
}

func addServiuceHandler(wf *factory.WatchFactory, npw *localnetNodePortWatcherData) error {
	_, err := wf.AddServiceHandler(cache.ResourceEventHandlerFuncs{
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
	}, nil)
	return err
}

func constructBaseIptRules() []iptRule {
	rules := make([]iptRule, 0)
	rules = append(rules, iptRule{
		table: "nat",
		chain: "PREROUTING",
		args:  []string{"-j", iptableNodePortChain},
	})
	rules = append(rules, iptRule{
		table: "nat",
		chain: "OUTPUT",
		args:  []string{"-j", iptableNodePortChain},
	})
	rules = append(rules, iptRule{
		table: "filter",
		chain: "FORWARD",
		args:  []string{"-j", iptableNodePortChain},
	})
	return rules
}

// cleanupLocalnetGateway cleans up Localnet Gateway
func cleanupLocalnetGateway() error {
	// get bridgeName from ovn-bridge-mappings.
	stdout, stderr, err := util.RunOVSVsctl("--if-exists", "get", "Open_vSwitch", ".",
		"external_ids:ovn-bridge-mappings")
	if err != nil {
		return fmt.Errorf("Failed to get ovn-bridge-mappings stderr:%s (%v)", stderr, err)
	}
	bridgeName := strings.Split(stdout, ":")[1]
	_, stderr, err = util.RunOVSVsctl("--", "--if-exists", "del-br", bridgeName)
	if err != nil {
		return fmt.Errorf("Failed to ovs-vsctl del-br %s stderr:%s (%v)", bridgeName, stderr, err)
	}
	return err
}
