//go:build linux
// +build linux

package node

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	"github.com/vishvananda/netlink"
	kapi "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
)

type addressManager struct {
	nodeName       string
	watchFactory   factory.NodeWatchFactory
	addresses      sets.Set[string]
	nodeAnnotator  kube.Annotator
	mgmtPortConfig *managementPortConfig
	// useNetlink indicates the addressManager should use machine
	// information from netlink. Set to false for testcases.
	useNetlink bool

	// compare node primary IP change
	nodePrimaryAddr net.IP
	gatewayBridge   *bridgeConfiguration

	OnChanged func()
	sync.Mutex
}

// initializes a new address manager which will hold all the IPs on a node
func newAddressManager(nodeName string, k kube.Interface, config *managementPortConfig, watchFactory factory.NodeWatchFactory, gwBridge *bridgeConfiguration) *addressManager {
	return newAddressManagerInternal(nodeName, k, config, watchFactory, gwBridge, true)
}

// newAddressManagerInternal creates a new address manager; this function is
// only expose for testcases to disable netlink subscription to ensure
// reproducibility of unit tests.
func newAddressManagerInternal(nodeName string, k kube.Interface, config *managementPortConfig, watchFactory factory.NodeWatchFactory, gwBridge *bridgeConfiguration, useNetlink bool) *addressManager {
	mgr := &addressManager{
		nodeName:       nodeName,
		watchFactory:   watchFactory,
		addresses:      sets.New[string](),
		mgmtPortConfig: config,
		gatewayBridge:  gwBridge,
		OnChanged:      func() {},
		useNetlink:     useNetlink,
	}
	mgr.nodeAnnotator = kube.NewNodeAnnotator(k, nodeName)
	mgr.sync()
	return mgr
}

// updates the address manager with a new IP
// returns true if there was an update
func (c *addressManager) addAddr(ipnet net.IPNet) bool {
	c.Lock()
	defer c.Unlock()
	if !c.addresses.Has(ipnet.String()) && c.isValidNodeIP(ipnet.IP) {
		klog.Infof("Adding IP: %s, to node IP manager", ipnet)
		c.addresses.Insert(ipnet.String())
		return true
	}

	return false
}

// removes IP from address manager
// returns true if there was an update
func (c *addressManager) delAddr(ipnet net.IPNet) bool {
	c.Lock()
	defer c.Unlock()
	if c.addresses.Has(ipnet.String()) && c.isValidNodeIP(ipnet.IP) {
		klog.Infof("Removing IP: %s, from node IP manager", ipnet)
		c.addresses.Delete(ipnet.String())
		return true
	}

	return false
}

// ListAddresses returns all the addresses we know about
func (c *addressManager) ListAddresses() []net.IP {
	c.Lock()
	defer c.Unlock()
	addrs := sets.List(c.addresses)
	out := make([]net.IP, 0, len(addrs))
	for _, addr := range addrs {
		ip, _, err := net.ParseCIDR(addr)
		if err != nil {
			klog.Errorf("Failed to parse %s: %v", addr, err)
			continue
		}
		out = append(out, ip)
	}
	return out
}

type subscribeFn func() (bool, chan netlink.AddrUpdate, error)

func (c *addressManager) Run(stopChan <-chan struct{}, doneWg *sync.WaitGroup) {
	addrSubscribeOptions := netlink.AddrSubscribeOptions{
		ErrorCallback: func(err error) {
			klog.Errorf("Failed during AddrSubscribe callback: %v", err)
			// Note: Not calling sync() from here: it is redudant and unsafe when stopChan is closed.
		},
	}

	subscribe := func() (bool, chan netlink.AddrUpdate, error) {
		addrChan := make(chan netlink.AddrUpdate)
		if err := netlink.AddrSubscribeWithOptions(addrChan, stopChan, addrSubscribeOptions); err != nil {
			return false, nil, err
		}
		// sync the manager with current addresses on the node
		c.sync()
		return true, addrChan, nil
	}

	c.runInternal(stopChan, doneWg, subscribe)
}

// runInternal can be used by testcases to provide a fake subscription function
// rather than using netlink
func (c *addressManager) runInternal(stopChan <-chan struct{}, doneWg *sync.WaitGroup, subscribe subscribeFn) {
	// Add an event handler to the node informer. This is needed for cases where users first update the node's IP
	// address but only later update kubelet configuration and restart kubelet (which in turn will update the reported
	// IP address inside the node's status field).
	nodeInformer := c.watchFactory.NodeInformer()
	_, err := nodeInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(old, new interface{}) {
			c.handleNodePrimaryAddrChange()
		},
	})
	if err != nil {
		klog.Fatalf("Could not add node event handler while starting address manager %v", err)
	}

	doneWg.Add(1)
	go func() {
		defer doneWg.Done()

		addressSyncTimer := time.NewTicker(30 * time.Second)
		defer addressSyncTimer.Stop()

		subscribed, addrChan, err := subscribe()
		if err != nil {
			klog.Error("Error during netlink subscribe for IP Manager: %v", err)
		}

		for {
			select {
			case a, ok := <-addrChan:
				addressSyncTimer.Reset(30 * time.Second)
				if !ok {
					if subscribed, addrChan, err = subscribe(); err != nil {
						klog.Error("Error during netlink re-subscribe due to channel closing for IP Manager: %v", err)
					}
					continue
				}
				addrChanged := false
				if a.NewAddr {
					addrChanged = c.addAddr(a.LinkAddress)
				} else {
					addrChanged = c.delAddr(a.LinkAddress)
				}

				c.handleNodePrimaryAddrChange()
				if addrChanged || !c.doesNodeHostAddressesMatch() {
					klog.Infof("Host addresses changed to %v. Updating node address annotation.", c.addresses)
					err := c.updateNodeAddressAnnotations()
					if err != nil {
						klog.Errorf("Address Manager failed to update node address annotations: %v", err)
					}
					c.OnChanged()
				}
			case <-addressSyncTimer.C:
				if subscribed {
					klog.V(5).Info("Node IP manager calling sync() explicitly")
					c.sync()
				} else {
					if subscribed, addrChan, err = subscribe(); err != nil {
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

// updates OVN's EncapIP if the node IP changed
func (c *addressManager) handleNodePrimaryAddrChange() {
	nodePrimaryAddrChanged, err := c.nodePrimaryAddrChanged()
	if err != nil {
		klog.Errorf("Address Manager failed to check node primary address change: %v", err)
		return
	}
	if nodePrimaryAddrChanged {
		klog.Infof("Node primary address changed to %v. Updating OVN encap IP.", c.nodePrimaryAddr)
		c.updateOVNEncapIPAndReconnect()
	}
}

// updateNodeAddressAnnotations updates all relevant annotations for the node including
// k8s.ovn.org/host-addresses, k8s.ovn.org/node-primary-ifaddr, k8s.ovn.org/l3-gateway-config.
func (c *addressManager) updateNodeAddressAnnotations() error {
	var err error
	var ifAddrs []*net.IPNet

	// Get node information
	node, err := c.watchFactory.GetNode(c.nodeName)
	if err != nil {
		return err
	}

	if c.useNetlink {
		// get updated interface IP addresses for the gateway bridge
		ifAddrs, err = c.gatewayBridge.updateInterfaceIPAddresses(node)
		if err != nil {
			return err
		}
	}

	// update k8s.ovn.org/host-addresses
	if err = c.updateHostAddresses(node); err != nil {
		return err
	}

	// sets both IPv4 and IPv6 primary IP addr in annotation k8s.ovn.org/node-primary-ifaddr
	// Note: this is not the API node's internal interface, but the primary IP on the gateway
	// bridge (cf. gateway_init.go)
	if err = util.SetNodePrimaryIfAddrs(c.nodeAnnotator, ifAddrs); err != nil {
		return err
	}

	// update k8s.ovn.org/l3-gateway-config
	gatewayCfg, err := util.ParseNodeL3GatewayAnnotation(node)
	if err != nil {
		return err
	}
	gatewayCfg.IPAddresses = ifAddrs
	err = util.SetL3GatewayConfig(c.nodeAnnotator, gatewayCfg)
	if err != nil {
		return err
	}

	// push all updates to the node
	err = c.nodeAnnotator.Run()
	if err != nil {
		return err
	}
	return nil
}

func (c *addressManager) updateHostAddresses(node *kapi.Node) error {
	if config.OvnKubeNode.Mode == types.NodeModeDPU {
		// For DPU mode, here we need to use the DPU host's IP address which is the tenant cluster's
		// host internal IP address instead.
		nodeAddrStr, err := util.GetNodePrimaryIP(node)
		if err != nil {
			return err
		}
		nodeAddrSet := sets.New[string](nodeAddrStr)
		return util.SetNodeHostAddresses(c.nodeAnnotator, nodeAddrSet)
	}

	return util.SetNodeHostAddresses(c.nodeAnnotator, c.addresses)
}

func (c *addressManager) assignAddresses(nodeHostAddresses sets.Set[string]) bool {
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
	// check to see if ips on the node differ from what we stored
	// in host-address annotation
	nodeHostAddresses, err := util.ParseNodeHostAddresses(node)
	if err != nil {
		klog.Errorf("Unable to parse addresses from node host %s: %s", node.Name, err.Error())
		return false
	}

	return nodeHostAddresses.Equal(c.addresses)
}

// nodePrimaryAddrChanged returns false if there is an error or if the IP does
// match, otherwise it returns true and updates the current primary IP address.
func (c *addressManager) nodePrimaryAddrChanged() (bool, error) {
	node, err := c.watchFactory.GetNode(c.nodeName)
	if err != nil {
		return false, err
	}
	// check to see if ips on the node differ from what we stored
	// in addressManager and it's an address that is known locally
	nodePrimaryAddrStr, err := util.GetNodePrimaryIP(node)
	if err != nil {
		return false, err
	}
	nodePrimaryAddr := net.ParseIP(nodePrimaryAddrStr)

	if nodePrimaryAddr == nil {
		return false, fmt.Errorf("failed to parse the primary IP address string from kubernetes node status")
	}
	c.Lock()
	exists := c.addresses.Has(nodePrimaryAddrStr)
	c.Unlock()

	if !exists || c.nodePrimaryAddr.Equal(nodePrimaryAddr) {
		return false, nil
	}
	c.nodePrimaryAddr = nodePrimaryAddr

	return true, nil
}

// updateOVNEncapIP updates encap IP to OVS when the node primary IP changed.
func (c *addressManager) updateOVNEncapIPAndReconnect() {
	checkCmd := []string{
		"get",
		"Open_vSwitch",
		".",
		"external_ids:ovn-encap-ip",
	}
	encapIP, stderr, err := util.RunOVSVsctl(checkCmd...)
	if err != nil {
		klog.Warningf("Unable to retrieve configured ovn-encap-ip from OVS: %v, %q", err, stderr)
	} else {
		encapIP = strings.TrimSuffix(encapIP, "\n")
		if len(encapIP) > 0 && c.nodePrimaryAddr.String() == encapIP {
			klog.V(4).Infof("Will not update encap IP, value: %s is the already configured", c.nodePrimaryAddr)
			return
		}
	}

	confCmd := []string{
		"set",
		"Open_vSwitch",
		".",
		fmt.Sprintf("external_ids:ovn-encap-ip=%s", c.nodePrimaryAddr),
	}

	_, stderr, err = util.RunOVSVsctl(confCmd...)
	if err != nil {
		klog.Errorf("Error setting OVS encap IP: %v  %q", err, stderr)
		return
	}

	// force ovn-controller to reconnect SB with new encap IP immediately.
	// otherwise there will be a max delay of 200s due to the 100s
	// ovn-controller inactivity probe.
	_, stderr, err = util.RunOVNAppctlWithTimeout(5, "-t", "ovn-controller", "exit", "--restart")
	if err != nil {
		klog.Errorf("Failed to exit ovn-controller %v %q", err, stderr)
		return
	}
}

// detects if the IP is valid for a node
// excludes things like local IPs, mgmt port ip, special masquerade IP
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

	if util.IsAddressReservedForInternalUse(addr) {
		return false
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

	currAddresses := sets.New[string]()
	for _, addr := range addrs {
		ip, ipnet, err := net.ParseCIDR(addr.String())
		if err != nil {
			klog.Errorf("Invalid IP address found on host: %s", addr.String())
			continue
		}
		if !c.isValidNodeIP(ip) {
			klog.V(5).Infof("Skipping non-useable IP address for host: %s", ip.String())
			continue
		}
		netAddr := &net.IPNet{IP: ip, Mask: ipnet.Mask}
		currAddresses.Insert(netAddr.String())
	}

	addrChanged := c.assignAddresses(currAddresses)
	c.handleNodePrimaryAddrChange()
	if addrChanged || !c.doesNodeHostAddressesMatch() {
		klog.Infof("Node address changed to %v. Updating annotations.", currAddresses)
		err := c.updateNodeAddressAnnotations()
		if err != nil {
			klog.Errorf("Address Manager failed to update node address annotations: %v", err)
		}
	}
}
