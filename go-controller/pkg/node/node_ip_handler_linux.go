//go:build linux
// +build linux

package node

import (
	"net"
	"sync"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	"github.com/vishvananda/netlink"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
)

type addressManager struct {
	nodeName       string
	watchFactory   factory.NodeWatchFactory
	addresses      sets.String
	nodeAnnotator  kube.Annotator
	mgmtPortConfig *managementPortConfig
	// useNetlink indicates the addressManager should use machine
	// information from netlink. Set to false for testcases.
	useNetlink bool

	OnChanged func()
	sync.Mutex
}

// initializes a new address manager which will hold all the IPs on a node
func newAddressManager(nodeName string, k kube.Interface, config *managementPortConfig, watchFactory factory.NodeWatchFactory) *addressManager {
	return newAddressManagerInternal(nodeName, k, config, watchFactory, true)
}

// newAddressManagerInternal creates a new address manager; this function is
// only expose for testcases to disable netlink subscription to ensure
// reproducibility of unit tests.
func newAddressManagerInternal(nodeName string, k kube.Interface, config *managementPortConfig, watchFactory factory.NodeWatchFactory, useNetlink bool) *addressManager {
	mgr := &addressManager{
		nodeName:       nodeName,
		watchFactory:   watchFactory,
		addresses:      sets.NewString(),
		mgmtPortConfig: config,
		OnChanged:      func() {},
		useNetlink:     useNetlink,
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

// ListAddresses returns all the addresses we know about
func (c *addressManager) ListAddresses() []net.IP {
	c.Lock()
	defer c.Unlock()
	addrs := c.addresses.List()
	out := make([]net.IP, 0, len(addrs))
	for _, addr := range addrs {
		ip := net.ParseIP(addr)
		if ip == nil {
			continue
		}
		out = append(out, ip)
	}
	return out
}

func (c *addressManager) Run(stopChan <-chan struct{}, doneWg *sync.WaitGroup) {
	if !c.useNetlink {
		// For testcases just return; we don't want to talk to
		// netlink since that pollutes the testcases with the local
		// CI machine configuration.
		return
	}

	var addrChan chan netlink.AddrUpdate
	addrSubscribeOptions := netlink.AddrSubscribeOptions{
		ErrorCallback: func(err error) {
			klog.Errorf("Failed during AddrSubscribe callback: %v", err)
			// Note: Not calling sync() from here: it is redudant and unsafe when stopChan is closed.
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

	doneWg.Add(1)
	go func() {
		defer doneWg.Done()

		addressSyncTimer := time.NewTicker(30 * time.Second)
		defer addressSyncTimer.Stop()

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

				if addrChanged || !c.doesNodeHostAddressesMatch() {
					if err := util.SetNodeHostAddresses(c.nodeAnnotator, c.addresses); err != nil {
						klog.Errorf("Failed to set node annotations: %v", err)
						continue
					}
					if err := c.nodeAnnotator.Run(); err != nil {
						klog.Errorf("Failed to set node annotations: %v", err)
					}
					c.OnChanged()
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

func (c *addressManager) assignAddresses(nodeHostAddresses sets.String) bool {
	c.Lock()
	defer c.Unlock()

	if nodeHostAddresses.Equal(c.addresses) {
		return false
	}
	c.addresses = nodeHostAddresses
	return true
}

func (c *addressManager) doesNodeHostAddressesMatch() bool {
	c.Lock()
	defer c.Unlock()

	node, err := c.watchFactory.GetNode(c.nodeName)
	if err != nil {
		klog.Errorf("Unable to get node from informer")
		return false
	}
	// check to see if ips on the node differ from what we have
	nodeHostAddresses, err := util.ParseNodeHostAddresses(node)
	if err != nil {
		klog.Errorf("Unable to parse addresses from node host")
		return false
	}

	return nodeHostAddresses.Equal(c.addresses)
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
	var err error
	var addrs []net.Addr

	if c.useNetlink {
		addrs, err = net.InterfaceAddrs()
		if err != nil {
			klog.Errorf("Failed to sync Node IP Manager: unable list all IPs on the node, error: %v", err)
			return
		}
	}

	currAddresses := sets.NewString()
	for _, addr := range addrs {
		ip, _, err := net.ParseCIDR(addr.String())
		if err != nil {
			klog.Errorf("Invalid IP address found on host: %s", addr.String())
			continue
		}
		if !c.isValidNodeIP(ip) {
			klog.V(5).Infof("Skipping non-useable IP address for host: %s", ip.String())
			continue
		}
		currAddresses.Insert(ip.String())
	}

	addrChanged := c.assignAddresses(currAddresses)
	if addrChanged || !c.doesNodeHostAddressesMatch() {
		klog.Infof("Node address annotation being set to: %v addrChanged: %v", currAddresses, addrChanged)
		if err := util.SetNodeHostAddresses(c.nodeAnnotator, c.addresses); err != nil {
			klog.Errorf("Failed to set node annotations: %v", err)
		} else if err := c.nodeAnnotator.Run(); err != nil {
			klog.Errorf("Failed to set node annotations: %v", err)
		}
	}
}
