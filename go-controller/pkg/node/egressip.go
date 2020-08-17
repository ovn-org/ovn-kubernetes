// +build linux

package node

import (
	"fmt"
	"reflect"
	"strings"
	"time"

	egressipv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/vishvananda/netlink"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog"
	utilnet "k8s.io/utils/net"
)

const egressLabel = "egress"

type egressIPLocal struct {
	gatewayIPv4        string
	gatewayIPv6        string
	nodeName           string
	defaultGatewayIntf string
}

func (n *OvnNode) watchEgressIP(egressIPLocal *egressIPLocal) {
	n.watchFactory.AddEgressIPHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			eIP := obj.(*egressipv1.EgressIP)
			if err := egressIPLocal.addEgressIP(eIP); err != nil {
				klog.Error(err)
			}
		},
		UpdateFunc: func(old, new interface{}) {
			oldEIP := old.(*egressipv1.EgressIP)
			newEIP := new.(*egressipv1.EgressIP)
			if !reflect.DeepEqual(oldEIP.Status, newEIP.Status) {
				if err := egressIPLocal.deleteEgressIP(oldEIP); err != nil {
					klog.Error(err)
				}
				if err := egressIPLocal.addEgressIP(newEIP); err != nil {
					klog.Error(err)
				}
			}
		},
		DeleteFunc: func(obj interface{}) {
			eIP := obj.(*egressipv1.EgressIP)
			if err := egressIPLocal.deleteEgressIP(eIP); err != nil {
				klog.Error(err)
			}
		},
	}, egressIPLocal.syncEgressIPs)
}

func (e *egressIPLocal) isNodeIP(link netlink.Link, addr *netlink.Addr) (bool, error) {
	nodeAddrs, err := netlink.AddrList(link, netlink.FAMILY_ALL)
	if err != nil {
		return true, err
	}
	for _, nodeAddr := range nodeAddrs {
		if nodeAddr.Label != getEgressLabel(e.defaultGatewayIntf) {
			if addr.IP.Equal(nodeAddr.IP) {
				return true, nil
			}
		}
	}
	return false, nil
}

func (e *egressIPLocal) addHandler(link netlink.Link, egressIP string) error {
	var maskSize string
	if utilnet.IsIPv6String(egressIP) {
		maskSize = "128"
	} else {
		maskSize = "32"
	}
	egressAddr, err := netlink.ParseAddr(fmt.Sprintf("%s/%s", egressIP, maskSize))
	if err != nil {
		return fmt.Errorf("unable to parse EgressIP: %s, err: %v", egressIP, err)
	}
	isNodeIP, err := e.isNodeIP(link, egressAddr)
	if err != nil {
		return fmt.Errorf("unable to list node's IPs")
	}
	if isNodeIP {
		return fmt.Errorf("unable to add own node's IP to interface")
	}
	egressAddr.Label = e.defaultGatewayIntf + egressLabel
	return netlink.AddrReplace(link, egressAddr)
}

func (e *egressIPLocal) delHandler(link netlink.Link, egressIP string) error {
	addrs, err := netlink.AddrList(link, netlink.FAMILY_ALL)
	if err != nil {
		return err
	}
	for _, addr := range addrs {
		if strings.Compare(addr.IP.String(), egressIP) == 0 {
			if addr.Label == getEgressLabel(e.defaultGatewayIntf) {
				return netlink.AddrDel(link, &addr)
			}
		}
	}
	return nil
}

func (e *egressIPLocal) addEgressIP(eIP *egressipv1.EgressIP) error {
	for _, status := range eIP.Status.Items {
		if status.Node == e.nodeName {
			if err := e.handleEgressIPLink(status.EgressIP, e.addHandler); err != nil {
				return fmt.Errorf("unable to add EgressIP to primary interface, err: %v", err)
			}
			go e.notifyClusterNodes(status.EgressIP)
			if err := e.setupEgressIP(status); err != nil {
				return fmt.Errorf("unable to setup EgressIP, err: %v ", err)
			}
		}
	}
	return nil
}

func (e *egressIPLocal) deleteEgressIP(eIP *egressipv1.EgressIP) error {
	for _, status := range eIP.Status.Items {
		if status.Node == e.nodeName {
			if err := e.handleEgressIPLink(status.EgressIP, e.delHandler); err != nil {
				return fmt.Errorf("unable to delete EgressIP from primary interface, err: %v", err)
			}
			if err := e.cleanupEgressIP(status); err != nil {
				return fmt.Errorf("unable to delete EgressIP, err: %v ", err)
			}
		}
	}
	return nil
}

func (e *egressIPLocal) setupEgressIP(eIPStatus egressipv1.EgressIPStatusItem) error {
	if utilnet.IsIPv6String(eIPStatus.EgressIP) {
		return addIptRules(getEgressIPTRules(eIPStatus, e.gatewayIPv6))
	}
	return addIptRules(getEgressIPTRules(eIPStatus, e.gatewayIPv4))
}

func (e *egressIPLocal) cleanupEgressIP(eIPStatus egressipv1.EgressIPStatusItem) error {
	if utilnet.IsIPv6String(eIPStatus.EgressIP) {
		return delIptRules(getEgressIPTRules(eIPStatus, e.gatewayIPv6))
	}
	return delIptRules(getEgressIPTRules(eIPStatus, e.gatewayIPv4))
}

func (e *egressIPLocal) syncEgressIPs(eIPs []interface{}) {
	validEgressIPs := make(map[string]bool)
	for _, eIP := range eIPs {
		eIP, ok := eIP.(*egressipv1.EgressIP)
		if !ok {
			klog.Errorf("Spurious object in syncEgressIPs: %v", eIP)
			continue
		}
		for _, status := range eIP.Status.Items {
			if status.Node == e.nodeName {
				validEgressIPs[status.EgressIP] = true
			}
		}
	}
	primaryLink, err := netlink.LinkByName(e.defaultGatewayIntf)
	if err != nil {
		klog.Errorf("Unable to get link for primary interface: %s, err: %v", e.defaultGatewayIntf, err)
		return
	}
	nodeAddr, err := netlink.AddrList(primaryLink, netlink.FAMILY_ALL)
	if err != nil {
		klog.Errorf("Unable to list addresses on primary interface: %s, err: %v", e.defaultGatewayIntf, err)
		return
	}
	for _, addr := range nodeAddr {
		if addr.Label == getEgressLabel(e.defaultGatewayIntf) {
			if _, exists := validEgressIPs[addr.IP.String()]; !exists {
				if err := netlink.AddrDel(primaryLink, &addr); err != nil {
					klog.Errorf("Unable to delete stale egress IP: %s, err: %v", addr.IP.String(), err)
				}
			}
		}
	}
}

// Notify cluster nodes of change in case an egress IP is moved from one node to another
func (e *egressIPLocal) notifyClusterNodes(egressIP string) {
	primaryLink, err := netlink.LinkByName(e.defaultGatewayIntf)
	if err != nil {
		klog.Errorf("Unable to get link for primary interface: %s, err: %v", e.defaultGatewayIntf, err)
		return
	}
	_, stderr, err := util.RunArping("-q", "-A", "-c", "1", "-I", primaryLink.Attrs().Name, egressIP)
	if err != nil {
		klog.Warningf("Failed to send GARP claim for egress IP %q: %v (%s)", egressIP, err, string(stderr))
		return
	}
	time.Sleep(2 * time.Second)
	if _, stderr, err := util.RunArping("-q", "-U", "-c", "1", "-I", primaryLink.Attrs().Name, egressIP); err != nil {
		klog.Warningf("Failed to GARP for egress IP %q: %v (%s)", egressIP, err, string(stderr))
	}
}

func (e *egressIPLocal) handleEgressIPLink(egressIP string, handler func(link netlink.Link, ip string) error) error {
	primaryLink, err := netlink.LinkByName(e.defaultGatewayIntf)
	if err != nil {
		return fmt.Errorf("unable to get link for primary interface: %s, err: %v", e.defaultGatewayIntf, err)
	}
	if err := handler(primaryLink, egressIP); err != nil {
		return fmt.Errorf("unable to handle egressIP on primary interface, err: %v", err)
	}
	return nil
}

func getEgressLabel(defaultGatewayIntf string) string {
	return defaultGatewayIntf + egressLabel
}
