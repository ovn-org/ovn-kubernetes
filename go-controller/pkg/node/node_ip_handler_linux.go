// +build linux

package node

import (
	"net"
	"sync"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	"github.com/vishvananda/netlink"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
)

type addressManager struct {
	addresses      sets.String
	nodeAnnotator  kube.Annotator
	mgmtPortConfig *managementPortConfig
	sync.Mutex
}

// initializes a new address manager which will hold all the IPs on a node
func newAddressManager(nodeName string, k kube.Interface, config *managementPortConfig) *addressManager {
	mgr := &addressManager{
		addresses:      sets.NewString(),
		mgmtPortConfig: config,
	}
	mgr.nodeAnnotator = kube.NewNodeAnnotator(k, nodeName)
	mgr.sync()
	return mgr
}

// updates the address manager with a new IP
// returns true if there was an update
func (c *addressManager) addAddr(ip net.IP) bool {
	c.Lock()
	defer c.Unlock()
	if !c.addresses.Has(ip.String()) && c.isValidNodeIP(ip) {
		klog.Infof("Adding IP: %s, to node IP manager", ip)
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
		klog.Infof("Removing IP: %s, from node IP manager", ip)
		c.addresses.Delete(ip.String())
		return true
	}

	return false
}

func (c *addressManager) Run(stopChan <-chan struct{}) {
	var addrChan chan netlink.AddrUpdate
	addrSubscribeOptions := netlink.AddrSubscribeOptions{
		ErrorCallback: func(err error) {
			klog.Errorf("Failed during AddrSubscribe callback: %v. Calling sync() explicitly", err)
			// sync the manager with current addresses on the node
			c.sync()
		},
	}

	subScribeFcn := func() (bool, error) {
		addrChan = make(chan netlink.AddrUpdate)
		if err := netlink.AddrSubscribeWithOptions(addrChan, stopChan, addrSubscribeOptions); err != nil {
			return false, err
		}
		// sync the manager with current addresses on the node
		c.sync()
		return true, nil
	}

	go func() {
		addressSyncTimer := time.NewTicker(30 * time.Second)

		subscribed, err := subScribeFcn()
		if err != nil {
			klog.Error("Error during netlink subscribe for IP Manager: %v", err)
		}

		for {
			select {
			case a, ok := <-addrChan:
				addressSyncTimer.Reset(30 * time.Second)
				if !ok {
					if subscribed, err = subScribeFcn(); err != nil {
						klog.Error("Error during netlink re-subscribe due to channel closing for IP Manager: %v", err)
					}
					continue
				}
				addrChanged := false
				if a.NewAddr {
					addrChanged = c.addAddr(a.LinkAddress.IP)
				} else {
					addrChanged = c.delAddr(a.LinkAddress.IP)
				}

				if addrChanged {
					if err := util.SetNodeHostAddresses(c.nodeAnnotator, c.addresses); err != nil {
						klog.Errorf("Failed to set node annotations: %v", err)
						continue
					}
					if err := c.nodeAnnotator.Run(); err != nil {
						klog.Errorf("Failed to set node annotations: %v", err)
					}
				}
			case <-addressSyncTimer.C:
				if subscribed {
					klog.V(5).Info("Node IP manager calling sync() explicitly")
					c.sync()
				} else {
					if subscribed, err = subScribeFcn(); err != nil {
						klog.Error("Error during netlink re-subscribe for IP Manager: %v", err)
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

func (c *addressManager) sync() {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		klog.Errorf("Failed to initialize Node IP Manager: unable list all IPs on the node, error: %v", err)
	}

	addrChanged := false
	for _, addr := range addrs {
		ip, _, err := net.ParseCIDR(addr.String())
		if err != nil {
			klog.Errorf("Invalid IP address found on host: %s", addr.String())
			continue
		}
		addrChanged = c.addAddr(ip) || addrChanged
	}

	if addrChanged {
		if err := util.SetNodeHostAddresses(c.nodeAnnotator, c.addresses); err != nil {
			klog.Errorf("Failed to set node annotations: %v", err)
		}
		if err := c.nodeAnnotator.Run(); err != nil {
			klog.Errorf("Failed to set node annotations: %v", err)
		}
	}
}
