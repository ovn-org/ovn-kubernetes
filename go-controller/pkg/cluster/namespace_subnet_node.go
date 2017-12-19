package cluster

import (
	"fmt"
	"net"
	"strings"
	"syscall"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/coreos/go-iptables/iptables"
	"github.com/vishvananda/netlink"

	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/util"

	kapi "k8s.io/api/core/v1"
)

func (cluster  *namespaceSubnetController) NodeStart(name string) error {
	// Make sure br-int is created.
	stdout, stderr, err := util.RunOVSVsctl("--", "--may-exist", "add-br", "br-int")
	if err != nil {
		logrus.Errorf("Failed to create br-int, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
	}

	_, err = newNamespaceWatcher(cluster.Kube, name, cluster.nodeNamespaceAdded, cluster.nodeNamespaceChanged, cluster.nodeNamespaceRemoved, cluster.watchFactory)
	return err
}

func getDefaultRouteInterfaceIP() (net.IP, error) {
	routes, err := netlink.RouteList(nil, syscall.AF_INET)
	if err != nil {
		return nil, err
	}
	for _, route := range routes {
		if route.Dst != nil && !route.Dst.IP.To4().Equal(net.IPv4zero) {
			continue
		}
		link, err := netlink.LinkByIndex(route.LinkIndex)
		if err != nil {
			continue
		}
		addrs, err := netlink.AddrList(link, syscall.AF_INET)
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			if addr.IP.IsGlobalUnicast() {
				return addr.IP, nil
			}
		}
	}
	return nil, fmt.Errorf("failed to find default route interface address")
}

func ifnameForNamespace(ns *kapi.Namespace) (string, *net.IPNet, error) {
	nsSubnet := ns.Annotations[OvnNamespaceSubnet]
	ip, ipnet, err := net.ParseCIDR(nsSubnet)
	if err != nil || ipnet.IP.To4() == nil {
		return "", nil, fmt.Errorf("invalid namespace subnet annotation %q (cannot convert to IPv4)", nsSubnet)
	}
	ipnet.IP = util.NextIP(ip)

	// OVN requires unique port names across the entire cluster
	defIP, err := getDefaultRouteInterfaceIP()
	if err != nil {
		return "", nil, err
	}
	ifname := fmt.Sprintf("%02x%02x%02x%02x%02x%02x%02x%02x", defIP[0], defIP[1], defIP[2], defIP[3], ipnet.IP[0], ipnet.IP[1], ipnet.IP[2], ipnet.IP[3])

	// Linux interface names are 15 characters max; chop of first byte of node
	// gateway address on the assumption that it's least unique of all the bytes
	return ifname[1:], ipnet, nil
}

type iptRule struct {
	table string
	chain string
	args  []string
}

func generateGatewayNATRules(ifname string, cidr *net.IPNet) []iptRule {
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
		args:  []string{"-o", ifname, "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT"},
	})

	// NAT for the interface
	_, ipnet, _ := net.ParseCIDR(cidr.String())
	rules = append(rules, iptRule{
		table: "nat",
		chain: "POSTROUTING",
		args:  []string{"-s", ipnet.String(), "-j", "MASQUERADE"},
	})
	return rules
}

func ensureChain(ipt *iptables.IPTables, table, chain string) error {
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

func (cluster  *namespaceSubnetController) enableGatewayNAT(ifname string, cidr *net.IPNet) error {
	ipt, err := iptables.NewWithProtocol(iptables.ProtocolIPv4)
	if err != nil {
		return fmt.Errorf("failed to initialize iptables: %v", err)
	}

	rules := generateGatewayNATRules(ifname, cidr)
	for _, r := range rules {
		if err := ensureChain(ipt, r.table, r.chain); err != nil {
			return fmt.Errorf("failed to ensure %s/%s: %v", r.table, r.chain, err)
		}
		exists, err := ipt.Exists(r.table, r.chain, r.args...)
		if !exists && err == nil {
			err = ipt.Insert(r.table, r.chain, 1, r.args...)
		}
		if err != nil {
			return fmt.Errorf("failed to add iptables %s/%s rule %q: %v", r.table, r.chain, strings.Join(r.args, " "))
		}
	}

	return nil
}

func (cluster  *namespaceSubnetController) cleanupGatewayNAT(ifname string, cidr *net.IPNet) error {
	ipt, err := iptables.NewWithProtocol(iptables.ProtocolIPv4)
	if err != nil {
		return fmt.Errorf("failed to initialize iptables: %v", err)
	}

	rules := generateGatewayNATRules(ifname, cidr)
	for _, r := range rules {
		ipt.Delete(r.table, r.chain, r.args...)
	}

	return nil
}

func waitForNamespaceSwitch(name string) error {
	for i := 0; i < 30; i++ {
		nsSwitch, _, err := util.RunOVNNbctl("--data=bare", "--no-heading", "--columns=_uuid", "find", "logical_switch", "name="+name)
		if err == nil && nsSwitch != "" {
			// Switch exists
			return nil
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("timed out waiting for namespace %q logical switch", name)
}

func (cluster  *namespaceSubnetController) tryAddNamespaceGateway(ns *kapi.Namespace) error {
	if err := waitForNamespaceSwitch(ns.Name); err != nil {
		return err
	}

	ifname, ipnet, err := ifnameForNamespace(ns)
	if err != nil {
		return err
	}
	macaddr := gatewayMACForNamespaceSubnet(ipnet)

	if _, err := netlink.LinkByName(ifname); err == nil {
		// link already exists, nothing to do
		return nil
	} else if _, ok := err.(netlink.LinkNotFoundError); !ok {
		return fmt.Errorf("error looking for namespace gateway link: %v", err)
	}

	logrus.Debugf("Adding namespace %q gateway %s / %s / %s", ns.Name, ifname, ipnet.String(), macaddr)

	// namespace gateway link not found, add it
	stdout, stderr, err := util.RunOVSVsctl("--", "--may-exist", "add-port",
		"br-int", ifname, "--", "set", "interface", ifname,
		"type=internal",
		"external-ids:iface-id=k8s-gw-"+ns.Name,
		"mac=\""+macaddr+"\"")
	if err != nil {
		logrus.Errorf("Failed to add port %q to br-int, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
		return err
	}
	// Refetch link now that it's added
	link, err := netlink.LinkByName(ifname)
	if err != nil {
		fmt.Errorf("failed to get gateway port %q: %v", ifname, err)
	}

	addr := &netlink.Addr{IPNet: ipnet, Label: ""}
	if err := netlink.AddrAdd(link, addr); err != nil {
		return fmt.Errorf("failed to add IP %s to %q: %v", ipnet.String(), ifname, err)
	}

	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("failed to set gateway port %q up: %v", ifname, err)
	}

	// NAT egress packets
	if err := cluster.enableGatewayNAT(ifname, ipnet); err != nil {
		return fmt.Errorf("failed to enable gateway port %q NAT: %v", ifname, err)
	}

	logrus.Infof("Added namespace %q gateway port %s IP %s", ns.Name, ifname, ipnet.String())
	return nil
}

func (cluster  *namespaceSubnetController) nodeNamespaceRemoved(ns *kapi.Namespace) error {
	ifname, ipnet, err := ifnameForNamespace(ns)
	if err != nil {
		return err
	}

	if err := cluster.cleanupGatewayNAT(ifname, ipnet); err != nil {
		// Log error but ignore
		logrus.Errorf("failed to clean up gateway port %s NAT: %v", ifname, err)
	}

	stdout, stderr, err := util.RunOVNNbctl("--", "--if-exists", "lsp-del", ifname)
	if err != nil {
		return fmt.Errorf("failed to add gateway port %q to namespace switch: %v", ifname, err)
	}

	stdout, stderr, err = util.RunOVSVsctl("--", "--if-exists", "del-port", "br-int", ifname)
	if err != nil {
		logrus.Errorf("Failed to remove port %q from br-int, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
		return err
	}

	logrus.Infof("Removed namespace %q gateway port %s", ns.Name, ifname)
	return nil
}

func (cluster  *namespaceSubnetController) nodeNamespaceAdded(ns *kapi.Namespace) error {
	if _, ok := ns.Annotations[OvnNamespaceSubnet]; ok {
		return cluster.tryAddNamespaceGateway(ns)
	}
	return nil
}

func (cluster  *namespaceSubnetController) nodeNamespaceChanged(ns *kapi.Namespace) error {
	return cluster.nodeNamespaceAdded(ns)
}
