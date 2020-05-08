// +build linux

package node

import (
	"fmt"
	"net"
	"reflect"
	"strings"

	"github.com/coreos/go-iptables/iptables"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/vishvananda/netlink"

	kapi "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog"
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
	ipv4                      = 4
	ipv6                      = 6
)

type iptRule struct {
	table string
	chain string
	args  []string
}

type localnetData struct {
	ipVersion                 int
	ipt                       util.IPTablesHelper
	gatewayIP, gatewayNextHop net.IP
	gatewaySubnetMask         net.IPMask
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

func initLocalnetGateway(nodeName string, subnets []*net.IPNet, wf *factory.WatchFactory,
	nodeAnnotator kube.Annotator) error {

	localnetBridgeName := "br-local"

	lnData, err := constructLocalnetData(subnets)
	if err != nil {
		return err
	}

	ifaceID, macAddress, link, err := createAndSetupLocalBridgePorts(localnetBridgeName, nodeName, lnData)
	if err != nil {
		return err
	}

	err = setupL3Gateway(nodeAnnotator, ifaceID, macAddress, lnData)
	if err != nil {
		return err
	}

	// TODO - IPv6 hack ... for some reason neighbor discovery isn't working here, so hard code a
	// MAC binding for the gateway IP address for now - need to debug this further

	ipV6MacBindingWorkaround(link, macAddress, lnData)

	err = localnetGatewayNAT(lnData, localnetGatewayNextHopPort)
	if err != nil {
		return fmt.Errorf("Failed to add NAT rules for localnet gateway (%v)", err)
	}

	if config.Gateway.NodeportEnable {
		err = localnetNodePortWatcher(lnData, wf)
	}
	return err
}

func createAndSetupLocalBridgePorts(localnetBridgeName string, nodeName string, lnData []*localnetData) (string,
	net.HardwareAddr, netlink.Link, error) {

	err := createLocalnetBridge(localnetBridgeName)
	if err != nil {
		return "", nil, nil, err
	}

	ifaceID, macAddress, err := bridgedGatewayNodeSetup(nodeName, localnetBridgeName, localnetBridgeName, true)
	if err != nil {
		return "", nil, nil, fmt.Errorf("failed to set up shared interface gateway: %v", err)
	}

	_, err = setUpLink(localnetBridgeName)
	if err != nil {
		return "", nil, nil, err
	}

	err = createGatewayPort(localnetBridgeName)
	if err != nil {
		return "", nil, nil, err
	}

	link, err := setUpLink(localnetGatewayNextHopPort)
	if err != nil {
		return "", nil, nil, err
	}

	err = addLinkAddresses(link, lnData)
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
		if lnData.ipVersion == ipv6 {
			gatewayIP := lnData.gatewayIP
			if exists := linkNeighExists(link, gatewayIP, macAddress); !exists {
				err := util.LinkNeighAdd(link, gatewayIP, macAddress)
				if err == nil {
					klog.Infof("Added MAC binding for %s on %s", gatewayIP, localnetGatewayNextHopPort)
				} else {
					klog.Errorf("Error in adding MAC binding for %s on %s: %v", gatewayIP, localnetGatewayNextHopPort, err)
				}
			}
		}
	}
}

func linkNeighExists(link netlink.Link, gatewayIP net.IP, macAddress net.HardwareAddr) bool {
	exists, err := util.LinkNeighExists(link, gatewayIP, macAddress)
	if err != nil {
		klog.Errorf("Error in exists call for MAC binding for %s on %s: %v. Will try to add the binding.",
			gatewayIP, localnetGatewayNextHopPort, err)
	}
	return exists
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
		ipAddrs = append(ipAddrs, &net.IPNet{IP: lnData.gatewayIP, Mask: lnData.gatewaySubnetMask})
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
	err := util.LinkAddrFlush(link)
	if err != nil {
		return err
	}
	for _, data := range ipData {
		err := util.LinkAddrAdd(link, &net.IPNet{IP: data.gatewayNextHop, Mask: data.gatewaySubnetMask})
		if err != nil {
			return err
		}
	}
	return nil
}

// Function that populates releavant IP structures for localnet

func constructLocalnetData(subnets []*net.IPNet) ([]*localnetData, error) {
	netData := make([]*localnetData, 0)
	for _, subnet := range subnets {
		if utilnet.IsIPv6CIDR(subnet) {
			data, err := newIPV6LocalnetData()
			if err != nil {
				return nil, err
			}
			netData = append(netData, data)
		} else {
			data, err := newIPV4LocalnetData()
			if err != nil {
				return nil, err
			}
			netData = append(netData, data)
		}
	}
	return netData, nil
}

func newIPV4LocalnetData() (*localnetData, error) {
	ipt, err := util.GetIPTablesHelper(iptables.ProtocolIPv4)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize iptables: %v", err)
	}
	gatewayIP := net.ParseIP(v4localnetGatewayIP)
	gatewayNextHop := net.ParseIP(v4localnetGatewayNextHop)
	gatewaySubnetMask := net.CIDRMask(v4localnetGatewaySubnetPrefix, 32)

	data := newLocalnetData(ipv4, ipt, gatewayIP, gatewayNextHop, gatewaySubnetMask)
	return data, nil
}

func newIPV6LocalnetData() (*localnetData, error) {
	ipt, err := util.GetIPTablesHelper(iptables.ProtocolIPv6)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize iptables: %v", err)
	}
	gatewayIP := net.ParseIP(v6localnetGatewayIP)
	gatewayNextHop := net.ParseIP(v6localnetGatewayNextHop)
	gatewaySubnetMask := net.CIDRMask(v6localnetGatewaySubnetPrefix, 128)

	data := newLocalnetData(ipv6, ipt, gatewayIP, gatewayNextHop, gatewaySubnetMask)
	return data, nil
}

func newLocalnetData(iPVersion int, ipt util.IPTablesHelper, gatewayIP net.IP, gatewayNextHop net.IP, gatewaySubnetMask net.IPMask) *localnetData {
	data := &localnetData{
		ipVersion:         iPVersion,
		ipt:               ipt,
		gatewayIP:         gatewayIP,
		gatewayNextHop:    gatewayNextHop,
		gatewaySubnetMask: gatewaySubnetMask,
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
	var ipVersion = ipv4
	if utilnet.IsIPv6String(svc.Spec.ClusterIP) {
		ipVersion = ipv6
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
	err := addServiceHandler(wf, npw)
	return err
}

func addServiceHandler(wf *factory.WatchFactory, npw *localnetNodePortWatcherData) error {
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
