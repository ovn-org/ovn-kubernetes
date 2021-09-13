// +build linux

package node

import (
	"fmt"
	"net"
	"sync"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	"github.com/vishvananda/netlink"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
)

type addressManager struct {
	addresses      sets.String
	kube           kube.Interface
	nodeName       string
	mgmtPortConfig *managementPortConfig
	sync.Mutex
}

// initializes a new address manager which will hold all the IPs on a node
func newAddressManager(nodeName string, kube kube.Interface, config *managementPortConfig) *addressManager {
	mgr := &addressManager{
		addresses:      sets.NewString(),
		nodeName:       nodeName,
		kube:           kube,
		mgmtPortConfig: config,
	}
	mgr.sync()
	return mgr
}

// updates the address manager with a new IP
// returns true if there was an update
func (c *addressManager) addAddr(ip net.IP) bool {
	c.Lock()
	defer c.Unlock()
	if !c.addresses.Has(ip.String()) && c.isValidNodeIP(ip) {
		klog.V(5).Infof("Adding IP: %s, to node IP manager", ip)
		c.addresses.Insert(ip.String())
		return true
	}

	return false
}

// removes IP from address manager
// returns true if there was an update
func (c *addressManager) delAddr(ip net.IP) bool {
	c.Lock()
	defer c.Unlock()
	if c.addresses.Has(ip.String()) && c.isValidNodeIP(ip) {
		klog.V(5).Infof("Removing IP: %s, from node IP manager", ip)
		c.addresses.Delete(ip.String())
		return true
	}

	return false
}

func (c *addressManager) Run(stopChan <-chan struct{}) {
	addrChan := make(chan netlink.AddrUpdate)
	if err := netlink.AddrSubscribe(addrChan, stopChan); err != nil {
		klog.Errorf("Unable to run Node IP Manager, error during netlink subscribe: %v", err)
		return
	}

	// sync the manager with current addresses on the node before we start processing events
	c.sync()

	go func() {
		for {
			select {
			case a := <-addrChan:
				addrChanged := func() bool {
					if a.NewAddr {
						return c.addAddr(a.LinkAddress.IP)
					}
					return c.delAddr(a.LinkAddress.IP)
				}()

				if addrChanged {
					if err := c.setAnnotations(); err != nil {
						klog.Errorf("Failed to set node annotations: %v", err)
						continue
					}
				}
			case <-stopChan:
				return
			}
		}
	}()

	klog.Info("Node IP manager is running")
}

// detects if the IP is valid for a node
// excludes things like local IPs, mgmt port ip
func (c *addressManager) isValidNodeIP(addr net.IP) bool {
	if addr == nil {
		return false
	}
	if addr.IsLinkLocalUnicast() {
		return false
	}
	if addr.IsLoopback() {
		return false
	}

	if utilnet.IsIPv4(addr) {
		if c.mgmtPortConfig.ipv4 != nil && c.mgmtPortConfig.ipv4.ifAddr.IP.Equal(addr) {
			return false
		}
	} else if utilnet.IsIPv6(addr) {
		if c.mgmtPortConfig.ipv6 != nil && c.mgmtPortConfig.ipv6.ifAddr.IP.Equal(addr) {
			return false
		}
	}

	return true
}

// sets address annotations on the node
func (c *addressManager) setAnnotations() error {
	node, err := c.kube.GetNode(c.nodeName)
	if err != nil {
		return fmt.Errorf("unable to retrieve node %s: %v", c.nodeName, err)
	}
	nodeAnnotator := kube.NewNodeAnnotator(c.kube, node)

	if err := util.SetNodeHostAddresses(nodeAnnotator, c.addresses); err != nil {
		return fmt.Errorf("failed to add node annotations: %v", err)
	}

	return nodeAnnotator.Run()
}

func (c *addressManager) sync() {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		klog.Errorf("Failed to initialize Node IP Manager: unable list all IPs on the node, error: %v", err)
	}

	for _, addr := range addrs {
		ip, _, err := net.ParseCIDR(addr.String())
		if err != nil {
			klog.Errorf("Invalid IP address found on host: %s", addr.String())
			continue
		}
		_ = c.addAddr(ip)
	}

	if err := c.setAnnotations(); err != nil {
		klog.Errorf("Failed to set node annotations: %v", err)
	}
}
