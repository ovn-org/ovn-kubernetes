//go:build linux
// +build linux

package node

import (
	"context"
	"fmt"
	"net"
	"sync/atomic"
	"time"

	"github.com/coreos/go-iptables/iptables"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	nodenft "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/nftables"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/routemanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/vishvananda/netlink"

	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
	"sigs.k8s.io/knftables"
)

const (
	// The legacy iptables management port chain
	iptableMgmPortChain = "OVN-KUBE-SNAT-MGMTPORT"

	// The "mgmtport-snat" chain contains the rules to SNAT traffic sent to the
	// management port (except for `externalTrafficPolicy: Local` traffic, where
	// the source IP must be preserved).
	nftablesMgmtPortChain = "mgmtport-snat"

	// "mgmtport-no-snat-nodeports" is a set containing protocol / nodePort tuples
	// indicating traffic that should not be SNATted when passing through the
	// management port because it is addressed to an `externalTrafficPolicy: Local`
	// NodePort.
	nftablesMgmtPortNoSNATNodePorts = "mgmtport-no-snat-nodeports"

	// "mgmtport-no-snat-services-v4" and "mgmtport-no-snat-services-v6" are sets
	// containing loadBalancerIP / protocol / port tuples indicating traffic that
	// should not be SNATted when passing through the management port because it is
	// addressed to an `externalTrafficPolicy: Local` load balancer IP.
	nftablesMgmtPortNoSNATServicesV4 = "mgmtport-no-snat-services-v4"
	nftablesMgmtPortNoSNATServicesV6 = "mgmtport-no-snat-services-v6"
)

type managementPortIPFamilyConfig struct {
	allSubnets []*net.IPNet
	ifAddr     *net.IPNet
	gwIP       net.IP
}

type managementPortConfig struct {
	ifName                 string
	link                   netlink.Link
	routerMAC              net.HardwareAddr
	isPodNetworkAdvertised atomic.Bool
	reconcilePeriod        time.Duration

	ipv4 *managementPortIPFamilyConfig
	ipv6 *managementPortIPFamilyConfig
}

func newManagementPortIPFamilyConfig(hostSubnet *net.IPNet, isIPv6 bool) (*managementPortIPFamilyConfig, error) {
	cfg := &managementPortIPFamilyConfig{
		ifAddr: util.GetNodeManagementIfAddr(hostSubnet),
		gwIP:   util.GetNodeGatewayIfAddr(hostSubnet).IP,
	}

	// capture all the subnets for which we need to add routes through management port
	for _, subnet := range config.Default.ClusterSubnets {
		if utilnet.IsIPv6CIDR(subnet.CIDR) == isIPv6 {
			cfg.allSubnets = append(cfg.allSubnets, subnet.CIDR)
		}
	}
	// add the .3 masqueradeIP to add the route via mp0 for ETP=local case
	// used only in LGW but we create it in SGW as well to maintain parity.
	if isIPv6 {
		_, masqueradeSubnet, err := net.ParseCIDR(config.Gateway.MasqueradeIPs.V6HostETPLocalMasqueradeIP.String() + "/128")
		if err != nil {
			return nil, err
		}
		cfg.allSubnets = append(cfg.allSubnets, masqueradeSubnet)
	} else {
		_, masqueradeSubnet, err := net.ParseCIDR(config.Gateway.MasqueradeIPs.V4HostETPLocalMasqueradeIP.String() + "/32")
		if err != nil {
			return nil, err
		}
		cfg.allSubnets = append(cfg.allSubnets, masqueradeSubnet)
	}

	return cfg, nil
}

func newManagementPortConfig(interfaceName string, hostSubnets []*net.IPNet, isPodNetworkAdvertised bool) (*managementPortConfig, error) {
	mpcfg := &managementPortConfig{
		ifName:          interfaceName,
		reconcilePeriod: 30 * time.Second,
	}
	mpcfg.isPodNetworkAdvertised.Store(isPodNetworkAdvertised)

	var err error
	if mpcfg.link, err = util.LinkSetUp(mpcfg.ifName); err != nil {
		return nil, err
	}

	for _, hostSubnet := range hostSubnets {
		isIPv6 := utilnet.IsIPv6CIDR(hostSubnet)

		var family string
		if isIPv6 {
			if mpcfg.ipv6 != nil {
				klog.Warningf("Ignoring duplicate IPv6 hostSubnet %s", hostSubnet)
				continue
			}
			family = "IPv6"
		} else {
			if mpcfg.ipv4 != nil {
				klog.Warningf("Ignoring duplicate IPv4 hostSubnet %s", hostSubnet)
				continue
			}
			family = "IPv4"
		}

		cfg, err := newManagementPortIPFamilyConfig(hostSubnet, isIPv6)
		if err != nil {
			return nil, err
		}
		if len(cfg.allSubnets) == 0 {
			klog.Warningf("Ignoring %s hostSubnet %s due to lack of %s cluster networks", family, hostSubnet, family)
			continue
		}

		if isIPv6 {
			mpcfg.ipv6 = cfg
		} else {
			mpcfg.ipv4 = cfg
		}
	}

	if mpcfg.ipv4 != nil {
		mpcfg.routerMAC = util.IPAddrToHWAddr(mpcfg.ipv4.gwIP)
	} else if mpcfg.ipv6 != nil {
		mpcfg.routerMAC = util.IPAddrToHWAddr(mpcfg.ipv6.gwIP)
	} else {
		klog.Fatalf("Management port configured with neither IPv4 nor IPv6 subnets")
	}

	return mpcfg, nil
}

func tearDownManagementPortConfig(link netlink.Link, nft knftables.Interface) error {
	if err := util.LinkAddrFlush(link); err != nil {
		return err
	}

	if err := util.LinkRoutesDel(link, nil); err != nil {
		return err
	}

	tx := nft.NewTransaction()
	// Delete would return an error if we tried to delete a chain that didn't exist, so
	// we do an Add first (which is a no-op if the chain already exists) and then Delete.
	tx.Add(&knftables.Chain{
		Name: nftablesMgmtPortChain,
	})
	tx.Delete(&knftables.Chain{
		Name: nftablesMgmtPortChain,
	})
	err := nft.Run(context.TODO(), tx)
	if err != nil && !knftables.IsNotFound(err) {
		return fmt.Errorf("could not clear the nftables chain for management port: %v", err)
	}

	return nil
}

func setupManagementPortIPFamilyConfig(routeManager *routemanager.Controller, mpcfg *managementPortConfig, cfg *managementPortIPFamilyConfig) ([]string, error) {
	var warnings []string
	var err error
	var exists bool

	// synchronize IP addresses, removing undesired addresses
	// should also remove routes specifying those undesired addresses
	err = util.SyncAddresses(mpcfg.link, []*net.IPNet{cfg.ifAddr})
	if err != nil {
		return warnings, err
	}

	// now check for addition of any missing routes
	for _, subnet := range cfg.allSubnets {
		route, err := util.LinkRouteGetByDstAndGw(mpcfg.link, cfg.gwIP, subnet)
		if err != nil || route == nil {
			// we need to warn so that it can be debugged as to why routes are incorrect
			warnings = append(warnings, fmt.Sprintf("missing or unable to find route entry for subnet %s "+
				"via gateway %s on link %v with MTU: %d", subnet, cfg.gwIP, mpcfg.ifName, config.Default.RoutableMTU))
		}

		subnetCopy := *subnet
		err = routeManager.Add(netlink.Route{LinkIndex: mpcfg.link.Attrs().Index, Gw: cfg.gwIP, Dst: &subnetCopy, MTU: config.Default.RoutableMTU})
		if err != nil {
			return warnings, fmt.Errorf("error adding route entry for subnet %s via gateway %s: %w", subnet, cfg.gwIP, err)
		}
	}

	// Add a neighbour entry on the K8s node to map routerIP with routerMAC. This is
	// required because in certain cases ARP requests from the K8s Node to the routerIP
	// arrives on OVN Logical Router pipeline with ARP source protocol address set to
	// K8s Node IP. OVN Logical Router pipeline drops such packets since it expects
	// source protocol address to be in the Logical Switch's subnet.
	if exists, err = util.LinkNeighExists(mpcfg.link, cfg.gwIP, mpcfg.routerMAC); err == nil && !exists {
		warnings = append(warnings, fmt.Sprintf("missing arp entry for MAC/IP binding (%s/%s) on link %s",
			mpcfg.routerMAC.String(), cfg.gwIP, types.K8sMgmtIntfName))
		// LinkNeighExists checks if the mac also matches, but it is possible there is a stale entry
		// still in the neighbor cache which would prevent add. Therefore execute a delete first if an IP entry exists.
		if exists, err = util.LinkNeighIPExists(mpcfg.link, cfg.gwIP); err != nil {
			return warnings, fmt.Errorf("failed to detect if stale IP neighbor entry exists for IP %s, on iface %s: %v",
				cfg.gwIP.String(), types.K8sMgmtIntfName, err)
		} else if exists {
			warnings = append(warnings, fmt.Sprintf("found stale neighbor entry IP binding (%s) on link %s",
				cfg.gwIP.String(), types.K8sMgmtIntfName))
			if err = util.LinkNeighDel(mpcfg.link, cfg.gwIP); err != nil {
				warnings = append(warnings, fmt.Sprintf("failed to remove stale IP neighbor entry for IP %s, on iface %s: %v",
					cfg.gwIP.String(), types.K8sMgmtIntfName, err))
			}
		}
		err = util.LinkNeighAdd(mpcfg.link, cfg.gwIP, mpcfg.routerMAC)
	}
	if err != nil {
		return warnings, err
	}

	protocol := iptables.ProtocolIPv4
	if mpcfg.ipv6 != nil && cfg == mpcfg.ipv6 {
		protocol = iptables.ProtocolIPv6
	}

	// IPv6 forwarding is enabled globally
	if protocol == iptables.ProtocolIPv4 {
		stdout, stderr, err := util.RunSysctl("-w", fmt.Sprintf("net.ipv4.conf.%s.forwarding=1", types.K8sMgmtIntfName))
		if err != nil || stdout != fmt.Sprintf("net.ipv4.conf.%s.forwarding = 1", types.K8sMgmtIntfName) {
			return warnings, fmt.Errorf("could not set the correct forwarding value for interface %s: stdout: %v, stderr: %v, err: %v",
				types.K8sMgmtIntfName, stdout, stderr, err)
		}
	}

	return warnings, nil
}

func setupManagementPortConfig(routeManager *routemanager.Controller, cfg *managementPortConfig) ([]string, error) {
	var warnings, allWarnings []string
	var err error

	if cfg.ipv4 != nil {
		warnings, err = setupManagementPortIPFamilyConfig(routeManager, cfg, cfg.ipv4)
		allWarnings = append(allWarnings, warnings...)
	}
	if cfg.ipv6 != nil && err == nil {
		warnings, err = setupManagementPortIPFamilyConfig(routeManager, cfg, cfg.ipv6)
		allWarnings = append(allWarnings, warnings...)
	}

	return allWarnings, err
}

func setupManagementPortNFTables(cfg *managementPortConfig) error {
	counterIfDebug := ""
	if config.Logging.Level > 4 {
		counterIfDebug = "counter"
	}

	nft, err := nodenft.GetNFTablesHelper()
	if err != nil {
		return err
	}

	tx := nft.NewTransaction()
	tx.Add(&knftables.Chain{
		Name:    nftablesMgmtPortChain,
		Comment: knftables.PtrTo("OVN SNAT to Management Port"),

		Type:     knftables.PtrTo(knftables.NATType),
		Hook:     knftables.PtrTo(knftables.PostroutingHook),
		Priority: knftables.PtrTo(knftables.SNATPriority),
	})
	tx.Add(&knftables.Set{
		Name:    nftablesMgmtPortNoSNATNodePorts,
		Comment: knftables.PtrTo("NodePorts not subject to management port SNAT"),
		Type:    "inet_proto . inet_service",
	})
	tx.Add(&knftables.Set{
		Name:    nftablesMgmtPortNoSNATServicesV4,
		Comment: knftables.PtrTo("eTP:Local short-circuit not subject to management port SNAT (IPv4)"),
		Type:    "ipv4_addr . inet_proto . inet_service",
	})
	tx.Add(&knftables.Set{
		Name:    nftablesMgmtPortNoSNATServicesV6,
		Comment: knftables.PtrTo("eTP:Local short-circuit not subject to management port SNAT (IPv6)"),
		Type:    "ipv6_addr . inet_proto . inet_service",
	})

	tx.Flush(&knftables.Chain{
		Name: nftablesMgmtPortChain,
	})
	tx.Add(&knftables.Rule{
		Chain: nftablesMgmtPortChain,
		Rule: knftables.Concat(
			"oifname", "!=", fmt.Sprintf("%q", cfg.ifName),
			"return",
		),
	})
	tx.Add(&knftables.Rule{
		Chain: nftablesMgmtPortChain,
		Rule: knftables.Concat(
			"meta l4proto", ".", "th dport", "@", nftablesMgmtPortNoSNATNodePorts,
			counterIfDebug,
			"return",
		),
	})

	isPodNetworkAdvertised := cfg.isPodNetworkAdvertised.Load()

	if cfg.ipv4 != nil {
		if isPodNetworkAdvertised {
			tx.Add(&knftables.Rule{
				Chain: nftablesMgmtPortChain,
				Rule: knftables.Concat(
					"meta nfproto ipv4",
					"fib saddr type != local",
					counterIfDebug,
					"return",
				),
			})
		}
		// don't SNAT if the source IP is already the Mgmt port IP
		tx.Add(&knftables.Rule{
			Chain: nftablesMgmtPortChain,
			Rule: knftables.Concat(
				"meta nfproto ipv4",
				"ip saddr", cfg.ipv4.ifAddr.IP,
				counterIfDebug,
				"return",
			),
		})
		tx.Add(&knftables.Rule{
			Chain: nftablesMgmtPortChain,
			Rule: knftables.Concat(
				"ip daddr . meta l4proto . th dport", "@", nftablesMgmtPortNoSNATServicesV4,
				counterIfDebug,
				"return",
			),
		})
		tx.Add(&knftables.Rule{
			Chain: nftablesMgmtPortChain,
			Rule: knftables.Concat(
				counterIfDebug,
				"snat ip to", cfg.ipv4.ifAddr.IP,
			),
		})
	}

	if cfg.ipv6 != nil {
		if isPodNetworkAdvertised {
			tx.Add(&knftables.Rule{
				Chain: nftablesMgmtPortChain,
				Rule: knftables.Concat(
					"meta nfproto ipv6",
					"fib saddr type != local",
					counterIfDebug,
					"return",
				),
			})
		}
		// don't SNAT if the source IP is already the Mgmt port IP
		tx.Add(&knftables.Rule{
			Chain: nftablesMgmtPortChain,
			Rule: knftables.Concat(
				"meta nfproto ipv6",
				"ip6 saddr", cfg.ipv6.ifAddr.IP,
				counterIfDebug,
				"return",
			),
		})
		tx.Add(&knftables.Rule{
			Chain: nftablesMgmtPortChain,
			Rule: knftables.Concat(
				"ip6 daddr . meta l4proto . th dport", "@", nftablesMgmtPortNoSNATServicesV6,
				counterIfDebug,
				"return",
			),
		})
		tx.Add(&knftables.Rule{
			Chain: nftablesMgmtPortChain,
			Rule: knftables.Concat(
				counterIfDebug,
				"snat ip6 to", cfg.ipv6.ifAddr.IP,
			),
		})
	}

	err = nft.Run(context.TODO(), tx)
	if err != nil {
		return fmt.Errorf("could not update nftables rule for management port: %v", err)
	}
	return nil
}

// createPlatformManagementPort creates a management port attached to the node switch
// that lets the node access its pods via their private IP address. This is used
// for health checking and other management tasks.
func createPlatformManagementPort(routeManager *routemanager.Controller, interfaceName string, localSubnets []*net.IPNet, isRoutingAdvertised bool) (*managementPortConfig, error) {
	var cfg *managementPortConfig
	var err error

	if cfg, err = newManagementPortConfig(interfaceName, localSubnets, isRoutingAdvertised); err != nil {
		return nil, err
	}

	if _, err = setupManagementPortConfig(routeManager, cfg); err != nil {
		return nil, err
	}

	if err = setupManagementPortNFTables(cfg); err != nil {
		return nil, err
	}
	DelLegacyMgtPortIptRules()

	return cfg, nil
}

// syncMgmtPortInterface verifies if no other interface configured as management port. This may happen if another
// interface had been used as management port or Node was running in different mode.
// If old management port is found, its IP configuration is flushed and interface renamed.
func syncMgmtPortInterface(hostSubnets []*net.IPNet, mgmtPortName string, isExpectedToBeInternal bool) error {
	// Query both type and name, because with type only stdout will be empty for both non-existing port and representor netdevice
	stdout, _, _ := util.RunOVSVsctl("--no-headings",
		"--data", "bare",
		"--format", "csv",
		"--columns", "type,name",
		"find", "Interface", "name="+mgmtPortName)
	if stdout == "" {
		// Not found on the bridge. But could be that interface with the same name exists
		return unconfigureMgmtNetdevicePort(hostSubnets, mgmtPortName)
	}

	// Found existing port. Check its type
	if stdout == "internal,"+mgmtPortName {
		if isExpectedToBeInternal {
			// Do nothing
			return nil
		}

		klog.Infof("Found OVS internal port. Removing it")
		_, stderr, err := util.RunOVSVsctl("del-port", "br-int", mgmtPortName)
		if err != nil {
			return fmt.Errorf("failed to remove OVS internal port: %s", stderr)
		}
		return nil
	}

	// It is representor which was used as management port.
	// Remove it from the bridge and rename.
	klog.Infof("Found existing representor management port. Removing it")
	return unconfigureMgmtRepresentorPort(mgmtPortName)
}

func unconfigureMgmtRepresentorPort(mgmtPortName string) error {
	// Get saved port name
	savedName, stderr, err := util.RunOVSVsctl("--if-exists", "get", "Interface", mgmtPortName, "external-ids:ovn-orig-mgmt-port-rep-name")
	if err != nil {
		klog.Warningf("Failed to get external-ds:ovn-orig-mgmt-port-rep-name: %s", stderr)
	}

	if savedName == "" {
		// rename to "rep" + "ddmmyyHHMMSS"
		savedName = time.Now().Format("rep010206150405")
		klog.Warningf("No saved management port representor name for %s, renaming to %s", mgmtPortName, savedName)
	}

	_, stderr, err = util.RunOVSVsctl("--if-exists", "del-port", "br-int", mgmtPortName)
	if err != nil {
		return fmt.Errorf("failed to remove OVS port: %s", stderr)
	}

	link, err := util.GetNetLinkOps().LinkByName(mgmtPortName)
	if err != nil {
		return fmt.Errorf("failed to lookup %s link: %v", mgmtPortName, err)
	}

	if err := util.GetNetLinkOps().LinkSetDown(link); err != nil {
		return fmt.Errorf("failed to set link down: %v", err)
	}

	if err := util.GetNetLinkOps().LinkSetName(link, savedName); err != nil {
		return fmt.Errorf("failed to rename %s link to %s: %v", mgmtPortName, savedName, err)
	}
	return nil
}

func unconfigureMgmtNetdevicePort(hostSubnets []*net.IPNet, mgmtPortName string) error {
	link, err := util.GetNetLinkOps().LinkByName(mgmtPortName)
	if err != nil {
		if !util.GetNetLinkOps().IsLinkNotFoundError(err) {
			return fmt.Errorf("failed to lookup %s link: %v", mgmtPortName, err)
		}
		// Nothing to unconfigure. Return.
		return nil
	}

	klog.Infof("Found existing management interface. Unconfiguring it")
	nft, err := nodenft.GetNFTablesHelper()
	if err != nil {
		return fmt.Errorf("failed to get nftables: %v", err)
	}

	if err := tearDownManagementPortConfig(link, nft); err != nil {
		return fmt.Errorf("teardown failed: %v", err)
	}

	if err := util.GetNetLinkOps().LinkSetDown(link); err != nil {
		return fmt.Errorf("failed to set %s link down: %v", mgmtPortName, err)
	}

	savedName := ""
	if config.OvnKubeNode.Mode != types.NodeModeDPUHost {
		// Get original interface name saved at OVS database
		stdout, stderr, err := util.RunOVSVsctl("--if-exists", "get", "Open_vSwitch", ".", "external-ids:ovn-orig-mgmt-port-netdev-name")
		if err != nil {
			klog.Warningf("Failed to get external-ds:ovn-orig-mgmt-port-netdev-name: %s", stderr)
		}
		savedName = stdout
	}

	if savedName == "" {
		// rename to "net" + "ddmmyyHHMMSS"
		savedName = time.Now().Format("net010206150405")
		klog.Warningf("No saved management port netdevice name for %s, renaming to %s", mgmtPortName, savedName)
	}

	// rename to PortName + "-ddmmyyHHMMSS"
	if err := util.GetNetLinkOps().LinkSetName(link, savedName); err != nil {
		return fmt.Errorf("failed to rename %s link to %s: %v", mgmtPortName, savedName, err)
	}
	return nil
}

// DelLegacyMgtPortIptRules deletes legacy iptables rules for the management port; this is
// only used for cleaning up stale rules when upgrading, and can eventually be removed.
func DelLegacyMgtPortIptRules() {
	// Clean up all iptables and ip6tables remnants that may be left around
	ipt, err := util.GetIPTablesHelper(iptables.ProtocolIPv4)
	if err != nil {
		return
	}
	ipt6, err := util.GetIPTablesHelper(iptables.ProtocolIPv6)
	if err != nil {
		return
	}
	rule := []string{"-o", types.K8sMgmtIntfName, "-j", iptableMgmPortChain}
	_ = ipt.Delete("nat", "POSTROUTING", rule...)
	_ = ipt6.Delete("nat", "POSTROUTING", rule...)
	_ = ipt.ClearChain("nat", iptableMgmPortChain)
	_ = ipt6.ClearChain("nat", iptableMgmPortChain)
	_ = ipt.DeleteChain("nat", iptableMgmPortChain)
	_ = ipt6.DeleteChain("nat", iptableMgmPortChain)
}

// checks to make sure that following configurations are present on the k8s node
// 1. route entries to cluster CIDR and service CIDR through management port
// 2. ARP entry for the node subnet's gateway ip
// 3. nftables rules for SNATing packets entering the logical topology
func checkManagementPortHealth(routeManager *routemanager.Controller, cfg *managementPortConfig) error {
	warnings, err := setupManagementPortConfig(routeManager, cfg)
	for _, warning := range warnings {
		klog.Warningf(warning)
	}
	if err != nil {
		return err
	}

	return setupManagementPortNFTables(cfg)
}
