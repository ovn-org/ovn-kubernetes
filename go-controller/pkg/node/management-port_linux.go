// +build linux

package node

import (
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/coreos/go-iptables/iptables"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/vishvananda/netlink"

	"k8s.io/klog"
	utilnet "k8s.io/utils/net"
)

const (
	iptableMgmPortChain = "OVN-KUBE-SNAT-MGMTPORT"
)

type managementPortConfig struct {
	link       netlink.Link
	ipt        util.IPTablesHelper
	allSubnets []string
	ifName     string
	ifIPMask   string
	ifIP       string
	routerIP   string
	routerMAC  string
}

func newManagementPortConfig(interfaceName, interfaceIP, routerIP, routerMAC string) (*managementPortConfig, error) {
	var err error

	cfg := &managementPortConfig{}
	cfg.ifName = interfaceName
	cfg.ifIPMask = interfaceIP
	cfg.routerIP = routerIP
	cfg.routerMAC = routerMAC
	if cfg.link, err = util.LinkSetUp(cfg.ifName); err != nil {
		return nil, err
	}

	// capture all the subnets for which we need to add routes through management port
	for _, subnet := range config.Default.ClusterSubnets {
		cfg.allSubnets = append(cfg.allSubnets, subnet.CIDR.String())
	}
	for _, subnet := range config.Kubernetes.ServiceCIDRs {
		cfg.allSubnets = append(cfg.allSubnets, subnet.String())
	}

	cfg.ifIP = strings.Split(cfg.ifIPMask, "/")[0]
	if utilnet.IsIPv6(net.ParseIP(cfg.ifIP)) {
		cfg.ipt, err = util.GetIPTablesHelper(iptables.ProtocolIPv6)
	} else {
		cfg.ipt, err = util.GetIPTablesHelper(iptables.ProtocolIPv4)
	}
	if err != nil {
		return nil, err
	}

	return cfg, nil
}

func tearDownManagementPortConfig(cfg *managementPortConfig) error {
	// for the initial setup we need to start from the clean slate, so flush
	// all addresses on this link, routes through this link, and
	// finally any IPtable rules for this link.
	if err := util.LinkAddrFlush(cfg.link); err != nil {
		return err
	}

	if err := util.LinkRoutesDel(cfg.link, cfg.allSubnets); err != nil {
		return err
	}

	if err := cfg.ipt.ClearChain("nat", iptableMgmPortChain); err != nil {
		return fmt.Errorf("could not clear the iptables chain for management port: %v", err)
	}
	return nil
}

func setupManagementPortConfig(cfg *managementPortConfig) ([]string, error) {
	var warnings []string
	var err error
	var exists bool

	if exists, err = util.LinkAddrExist(cfg.link, cfg.ifIPMask); err == nil && !exists {
		// we should log this so that one can debug as to why addresses are
		// disappearing
		warnings = append(warnings, fmt.Sprintf("missing IP address %s on the interface %s, adding it...",
			cfg.ifIPMask, cfg.ifName))
		err = util.LinkAddrAdd(cfg.link, cfg.ifIPMask)
	}
	if err != nil {
		return warnings, err
	}

	for _, subnet := range cfg.allSubnets {
		if exists, err = util.LinkRouteExists(cfg.link, cfg.routerIP, subnet); err == nil && !exists {
			// we need to warn so that it can be debugged as to why routes are disappearing
			warnings = append(warnings, fmt.Sprintf("missing route entry for subnet %s via gateway %s on link %v",
				subnet, cfg.routerIP, cfg.ifName))
			err = util.LinkRoutesAdd(cfg.link, cfg.routerIP, []string{subnet})
			if err != nil {
				if os.IsExist(err) {
					klog.V(5).Infof("Ignoring error %s from 'route add %s via %s' - already added via IPv6 RA?",
						err.Error(), subnet, cfg.routerIP)
					continue
				}
			}
		}
		if err != nil {
			return warnings, err
		}
	}

	// Add a neighbour entry on the K8s node to map routerIP with routerMAC. This is
	// required because in certain cases ARP requests from the K8s Node to the routerIP
	// arrives on OVN Logical Router pipeline with ARP source protocol address set to
	// K8s Node IP. OVN Logical Router pipeline drops such packets since it expects
	// source protocol address to be in the Logical Switch's subnet.
	if exists, err = util.LinkNeighExists(cfg.link, cfg.routerIP, cfg.routerMAC); err == nil && !exists {
		warnings = append(warnings, fmt.Sprintf("missing arp entry for MAC/IP binding (%s/%s) on link %s",
			cfg.routerMAC, cfg.routerIP, util.K8sMgmtIntfName))
		err = util.LinkNeighAdd(cfg.link, cfg.routerIP, cfg.routerMAC)
	}
	if err != nil {
		return warnings, err
	}

	rule := []string{"-o", cfg.ifName, "-j", iptableMgmPortChain}
	if exists, err = cfg.ipt.Exists("nat", "POSTROUTING", rule...); err == nil && !exists {
		warnings = append(warnings, fmt.Sprintf("missing iptables postrouting nat chain %s, adding it",
			iptableMgmPortChain))
		err = cfg.ipt.Insert("nat", "POSTROUTING", 1, rule...)
	}
	if err != nil {
		return warnings, fmt.Errorf("could not set up iptables chain rules for management port: %v", err)
	}
	rule = []string{"-o", cfg.ifName, "-j", "SNAT", "--to-source", cfg.ifIP,
		"-m", "comment", "--comment", "OVN SNAT to Management Port"}
	if exists, err = cfg.ipt.Exists("nat", iptableMgmPortChain, rule...); err == nil && !exists {
		warnings = append(warnings, fmt.Sprintf("missing management port nat rule in chain %s, adding it",
			iptableMgmPortChain))
		err = cfg.ipt.Insert("nat", iptableMgmPortChain, 1, rule...)
	}
	if err != nil {
		return warnings, fmt.Errorf("could not set up iptables rules for management port: %v", err)
	}

	return warnings, nil
}

// createPlatformManagementPort creates a management port attached to the node switch
// that lets the node access its pods via their private IP address. This is used
// for health checking and other management tasks.
func createPlatformManagementPort(interfaceName, interfaceIP, routerIP, routerMAC string,
	stopChan chan struct{}) error {
	var cfg *managementPortConfig
	var err error

	if cfg, err = newManagementPortConfig(interfaceName, interfaceIP, routerIP, routerMAC); err != nil {
		return err
	}

	if err = tearDownManagementPortConfig(cfg); err != nil {
		return err
	}

	if _, err = setupManagementPortConfig(cfg); err != nil {
		return err
	}

	// start the management port health check
	go checkManagementPortHealth(cfg, stopChan)
	return nil
}

//DelMgtPortIptRules delete all the iptable rules for the management port.
func DelMgtPortIptRules() {
	// Clean up all iptables and ip6tables remnants that may be left around
	ipt, err := util.GetIPTablesHelper(iptables.ProtocolIPv4)
	if err != nil {
		return
	}
	ipt6, err := util.GetIPTablesHelper(iptables.ProtocolIPv6)
	if err != nil {
		return
	}
	rule := []string{"-o", util.K8sMgmtIntfName, "-j", iptableMgmPortChain}
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
// 3. IPtables chain and rule for SNATing packets entering the logical topology
func checkManagementPortHealth(cfg *managementPortConfig, stopChan chan struct{}) {
	for {
		select {
		case <-time.After(30 * time.Second):
			warnings, err := setupManagementPortConfig(cfg)
			for _, warning := range warnings {
				klog.Warningf(warning)
			}
			if err != nil {
				klog.Errorf(err.Error())
			}
		case <-stopChan:
			return
		}
	}
}
