// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build linux
// +build linux

package node

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/vishvananda/netlink"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	ovsops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/bridgeconfig"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/managementport"
	nodeutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

type addressManager struct {
	nodeName      string
	watchFactory  factory.NodeWatchFactory
	cidrs         sets.Set[string]
	nodeAnnotator kube.Annotator
	mgmtPort      managementport.Interface
	ovsClient     libovsdbclient.Client
	// useNetlink indicates the addressManager should use machine
	// information from netlink. Set to false for testcases.
	useNetlink bool
	syncPeriod time.Duration
	// compare node primary IP change
	nodePrimaryAddr net.IP
	gatewayBridge   *bridgeconfig.BridgeConfiguration

	onAddressesChangedHandlers []func()
	OnMasqueradeIPChanged      func()
	// gatewayIfIndex caches the link index of config.Gateway.Interface.
	// Used in DPUHost mode to filter address events to only the gateway
	// interface. Refreshed in sync() every 30s; all access is from the
	// runInternal goroutine so no synchronization is needed.
	gatewayIfIndex int
	sync.Mutex
}

// initializes a new address manager which will hold all the IPs on a node
func newAddressManager(nodeName string, k kube.Interface, mgmtPort managementport.Interface, watchFactory factory.NodeWatchFactory, gwBridge *bridgeconfig.BridgeConfiguration, ovsClient libovsdbclient.Client) *addressManager {
	return newAddressManagerInternal(nodeName, k, mgmtPort, watchFactory, gwBridge, ovsClient, true)
}

// newAddressManagerInternal creates a new address manager; this function is
// only expose for testcases to disable netlink subscription to ensure
// reproducibility of unit tests.
func newAddressManagerInternal(nodeName string, k kube.Interface, mgmtPort managementport.Interface, watchFactory factory.NodeWatchFactory, gwBridge *bridgeconfig.BridgeConfiguration, ovsClient libovsdbclient.Client, useNetlink bool) *addressManager {
	mgr := &addressManager{
		nodeName:              nodeName,
		watchFactory:          watchFactory,
		cidrs:                 sets.New[string](),
		mgmtPort:              mgmtPort,
		gatewayBridge:         gwBridge,
		ovsClient:             ovsClient,
		OnMasqueradeIPChanged: func() {},
		useNetlink:            useNetlink,
		syncPeriod:            30 * time.Second,
	}
	mgr.nodeAnnotator = kube.NewNodeAnnotator(k, nodeName)
	if config.IsModeDPU() {
		if err := mgr.updateHostCIDRs(); err != nil {
			klog.Errorf("Failed to update host-cidrs annotations on node %s: %v", nodeName, err)
			return nil
		}
		if err := mgr.nodeAnnotator.Run(); err != nil {
			klog.Errorf("Failed to set host-cidrs annotations on node %s: %v", nodeName, err)
			return nil
		}
	} else {
		mgr.sync()
	}

	return mgr
}

// updates the address manager with a new IP
// returns true if there was an update
func (c *addressManager) addAddr(ipnet net.IPNet, linkIndex int) bool {
	c.Lock()
	defer c.Unlock()
	if !c.cidrs.Has(ipnet.String()) && c.isValidNodeIP(ipnet.IP, linkIndex) {
		klog.Infof("Adding IP: %s, to node IP manager", ipnet)
		c.cidrs.Insert(ipnet.String())
		return true
	}

	return false
}

// removes IP from address manager
// returns true if there was an update
func (c *addressManager) delAddr(ipnet net.IPNet) bool {
	c.Lock()
	defer c.Unlock()
	if c.cidrs.Has(ipnet.String()) {
		klog.Infof("Removing IP: %s, from node IP manager", ipnet)
		c.cidrs.Delete(ipnet.String())
		return true
	}

	return false
}

// GetManagementPort returns the management port interface in a thread-safe manner
func (c *addressManager) GetManagementPort() managementport.Interface {
	c.Lock()
	defer c.Unlock()
	return c.mgmtPort
}

// ListAddresses returns all the addresses we know about
func (c *addressManager) ListAddresses() ([]net.IP, []*net.IPNet) {
	c.Lock()
	defer c.Unlock()
	addrs := sets.List(c.cidrs)
	addresses := make([]net.IP, 0, len(addrs))
	networkAddresses := make([]*net.IPNet, 0, len(addrs))
	for _, addr := range addrs {
		ip, networkAddress, err := net.ParseCIDR(addr)
		if err != nil {
			klog.Errorf("Failed to parse %s: %v", addr, err)
			continue
		}
		addresses = append(addresses, ip)
		networkAddresses = append(networkAddresses, networkAddress)
	}
	return addresses, networkAddresses
}

// AddOnAddressesChangedHandler registers a callback that will be invoked whenever
// the node's addresses change. Safe to call after the address manager goroutine
// has started. No corresponding remove is provided: callers are expected to be
// lifecycle-bound to the process.
func (c *addressManager) AddOnAddressesChangedHandler(handler func()) {
	c.Lock()
	defer c.Unlock()
	c.onAddressesChangedHandlers = append(c.onAddressesChangedHandlers, handler)
}

func (c *addressManager) notifyAddressesChanged() {
	c.Lock()
	handlers := make([]func(), len(c.onAddressesChangedHandlers))
	copy(handlers, c.onAddressesChangedHandlers)
	c.Unlock()
	for _, handler := range handlers {
		handler()
	}
}

type subscribeFn func() (bool, chan netlink.AddrUpdate, error)

func (c *addressManager) Run(stopChan <-chan struct{}, doneWg *sync.WaitGroup) {
	if config.IsModeDPU() {
		return
	}

	if !config.IsModeDPUHost() {
		c.addHandlerForAddrChange()
	}
	doneWg.Add(1)
	go func() {
		c.runInternal(stopChan, c.getNetlinkAddrSubFunc(stopChan))
		doneWg.Done()
	}()
}

// runInternal gathers node IP information and publishes it on the k8 node annotations.
// The annotations it updates are k8s.ovn.org/host-cidrs, k8s.ovn.org/node-primary-ifaddr and k8s.ovn.org/l3-gateway-config.
// It waits on 3 events and only the "stop" event may end execution.
// Event 1: Address change events using a subscription func. In normal execution, this is a netlink addr subscription func that returns a channel that
// conveys address updates that can be processed upon immediately.
// Event 2: Ticker events which is used to trigger a sync func. This is required in-case address change events are missed.
// Event 3: Stop events which stops event watching and returns.
func (c *addressManager) runInternal(stopChan <-chan struct{}, subscribe subscribeFn) {
	addressSyncTimer := time.NewTicker(c.syncPeriod)
	defer addressSyncTimer.Stop()

	subscribed, addrChan, err := subscribe()
	if err != nil {
		klog.Errorf("Error during netlink subscribe for IP Manager: %v", err)
	}
	klog.Info("Node IP manager is running")
	for {
		select {
		case a, ok := <-addrChan:
			addressSyncTimer.Reset(c.syncPeriod)
			if !ok {
				if subscribed, addrChan, err = subscribe(); err != nil {
					klog.Errorf("Error during netlink re-subscribe due to channel closing for IP Manager: %v", err)
				}
				continue
			}
			if a.LinkAddress.IP != nil && util.IsAddressReservedForInternalUse(a.LinkAddress.IP) {
				c.reconcileMasqueradeResources()
				continue
			}
			if config.IsModeDPUHost() {
				if c.gatewayIfIndex != 0 && a.LinkIndex == c.gatewayIfIndex {
					c.reconcileMasqueradeResources()
				}
			}
			addrChanged := false
			if a.NewAddr {
				addrChanged = c.addAddr(a.LinkAddress, a.LinkIndex)
			} else {
				addrChanged = c.delAddr(a.LinkAddress)
			}

			c.handleNodePrimaryAddrChange()
			if addrChanged || !c.doNodeHostCIDRsMatch() {
				klog.Infof("Host CIDRs changed to %v. Updating node address annotations.", c.cidrs)
				err := c.updateNodeAddressAnnotations()
				if err != nil {
					klog.Errorf("Address Manager failed to update node address annotations: %v", err)
				}
				c.notifyAddressesChanged()
			}
		case <-addressSyncTimer.C:
			if subscribed {
				klog.V(5).Info("Node IP manager calling sync() explicitly")
				c.sync()
			} else {
				if subscribed, addrChan, err = subscribe(); err != nil {
					klog.Errorf("Error during netlink re-subscribe for IP Manager: %v", err)
				}
			}
		case <-stopChan:
			klog.Info("Node IP manager is finished")
			return
		}
	}
}

func (c *addressManager) getNetlinkAddrSubFunc(stopChan <-chan struct{}) func() (bool, chan netlink.AddrUpdate, error) {
	addrSubscribeOptions := netlink.AddrSubscribeOptions{
		ErrorCallback: func(err error) {
			klog.Errorf("Failed during AddrSubscribe callback: %v", err)
			// Note: Not calling sync() from here: it is redudant and unsafe when stopChan is closed.
		},
	}
	return func() (bool, chan netlink.AddrUpdate, error) {
		addrChan := make(chan netlink.AddrUpdate)
		if err := netlink.AddrSubscribeWithOptions(addrChan, stopChan, addrSubscribeOptions); err != nil {
			return false, nil, err
		}
		// sync the manager with current addresses on the node
		c.sync()
		return true, addrChan, nil
	}
}

// addHandlerForAddrChange handles reconfiguration of a node primary IP address change or egress IP annotation changes
func (c *addressManager) addHandlerForAddrChange() {
	// Add an event handler to the node informer. This is needed for cases where users first update the node's IP
	// address but only later update kubelet configuration and restart kubelet (which in turn will update the reported
	// IP address inside the node's status field).
	// It is also needed to cover gaps when the egress IPs are updated in annotations, in order to
	// maintain a consistent host-cidrs set, without stale Egress IPs.
	nodeInformer := c.watchFactory.NodeInformer()
	_, err := nodeInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldNode, oldOK := oldObj.(*corev1.Node)
			newNode, newOK := newObj.(*corev1.Node)
			if oldOK && newOK && newNode.Name == c.nodeName && nodeEgressIPAnnotationsChanged(oldNode, newNode) {
				klog.V(5).Infof("Node %s egress IP annotations changed, syncing node IP manager", c.nodeName)
				c.sync()
				// c.sync() already calls c.handleNodePrimaryAddrChange, so safe to return
				return
			}
			c.handleNodePrimaryAddrChange()
		},
	})
	if err != nil {
		klog.Fatalf("Could not add node event handler while starting address manager %v", err)
	}
}

func nodeEgressIPAnnotationsChanged(oldNode, newNode *corev1.Node) bool {
	if oldNode == nil || newNode == nil {
		return false
	}
	for _, key := range []string{util.OVNNodeSecondaryHostEgressIPs, util.OVNNodeBridgeEgressIPs} {
		oldVal, oldSet := oldNode.Annotations[key]
		newVal, newSet := newNode.Annotations[key]
		if oldSet != newSet || oldVal != newVal {
			return true
		}
	}
	return false
}

// updates OVN's EncapIP if the node IP changed
func (c *addressManager) handleNodePrimaryAddrChange() {
	c.Lock()
	defer c.Unlock()
	nodePrimaryAddrChanged, err := c.nodePrimaryAddrChanged()
	if err != nil {
		klog.Errorf("Address Manager failed to check node primary address change: %v", err)
		return
	}
	if nodePrimaryAddrChanged && config.Default.EncapIP == "" && (config.IsModeDPU() || config.IsModeFull()) {
		klog.Infof("Node primary address changed to %v. Updating OVN encap IP.", c.nodePrimaryAddr)
		c.updateOVNEncapIPAndReconnect(c.nodePrimaryAddr)
	}
}

// updateNodeAddressAnnotations updates the node address annotations. In DPUHost mode
// the host has no gateway bridge and node-primary-ifaddr / l3-gateway-config are owned
// by the DPU, so only host-cidrs is maintained; in all other modes the full set
// (k8s.ovn.org/host-cidrs, k8s.ovn.org/node-primary-ifaddr, k8s.ovn.org/l3-gateway-config)
// is updated.
func (c *addressManager) updateNodeAddressAnnotations() error {
	if config.IsModeDPUHost() {
		return c.updateDPUHostAddressAnnotations()
	}
	return c.updateGatewayAddressAnnotations()
}

// updateDPUHostAddressAnnotations refreshes the host-owned annotations in DPUHost mode:
// k8s.ovn.org/host-cidrs and k8s.ovn.org/primary-dpu-host-addr. The latter tracks the
// gateway interface addresses; node-primary-ifaddr and l3-gateway-config are derived
// from it by the DPU.
func (c *addressManager) updateDPUHostAddressAnnotations() error {
	if err := c.updateHostCIDRs(); err != nil {
		return err
	}

	ifAddrs, err := nodeutil.GetNetworkInterfaceIPAddresses(config.Gateway.Interface)
	if err != nil {
		return err
	}
	if err := util.SetNodePrimaryDPUHostAddr(c.nodeAnnotator, ifAddrs); err != nil {
		return err
	}

	return c.nodeAnnotator.Run()
}

// updateGatewayAddressAnnotations updates k8s.ovn.org/host-cidrs,
// k8s.ovn.org/node-primary-ifaddr and k8s.ovn.org/l3-gateway-config from the gateway
// bridge addresses.
func (c *addressManager) updateGatewayAddressAnnotations() error {
	var ifAddrs []*net.IPNet

	node, err := c.watchFactory.GetNode(c.nodeName)
	if err != nil {
		return err
	}

	if config.IsModeDPU() {
		// On the DPU, br-dpu has no IP of its own (the address lives on the host and is
		// conveyed through the primary-dpu-host-addr annotation), so the bridge netlink
		// path below would fail. Derive the addresses from the annotation, as done at
		// gateway init.
		ifAddrs, err = getNodePrimaryIfAddrs(c.watchFactory, c.nodeName, "")
		if err != nil {
			return err
		}
	} else if c.useNetlink {
		// get updated interface IP addresses for the gateway bridge
		ifAddrs, err = c.gatewayBridge.UpdateInterfaceIPAddresses(node)
		if err != nil {
			return err
		}
	}

	// update k8s.ovn.org/host-cidrs
	if err = c.updateHostCIDRs(); err != nil {
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
	if err = util.SetL3GatewayConfig(c.nodeAnnotator, gatewayCfg); err != nil {
		return err
	}

	// push all updates to the node
	return c.nodeAnnotator.Run()
}

func (c *addressManager) updateHostCIDRs() error {
	if config.IsModeDPU() {
		// For DPU mode, we don't need to update the host-cidrs annotation.
		return nil
	}

	return util.SetNodeHostCIDRs(c.nodeAnnotator, c.cidrs)
}

func (c *addressManager) assignCIDRs(nodeHostCIDRs sets.Set[string]) bool {
	c.Lock()
	defer c.Unlock()

	if nodeHostCIDRs.Equal(c.cidrs) {
		return false
	}
	c.cidrs = nodeHostCIDRs
	return true
}

func (c *addressManager) doNodeHostCIDRsMatch() bool {
	c.Lock()
	defer c.Unlock()

	node, err := c.watchFactory.GetNode(c.nodeName)
	if err != nil {
		klog.Errorf("Unable to get node from informer")
		return false
	}
	// check to see if ips on the node differ from what we stored
	// in host-cidrs annotation
	nodeHostAddresses, err := util.ParseNodeHostCIDRs(node)
	if err != nil {
		klog.Errorf("Unable to parse addresses from node host %s: %s", node.Name, err.Error())
		return false
	}

	return nodeHostAddresses.Equal(c.cidrs)
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

	var exists bool
	for _, hostCIDR := range c.cidrs.UnsortedList() {
		ip, _, err := net.ParseCIDR(hostCIDR)
		if err != nil {
			klog.Errorf("Node IP: failed to parse node address %q. Unable to detect if node primary address changed: %v",
				hostCIDR, err)
			continue
		}
		if ip.Equal(nodePrimaryAddr) {
			exists = true
			break
		}
	}

	if !exists || c.nodePrimaryAddr.Equal(nodePrimaryAddr) {
		return false, nil
	}
	c.nodePrimaryAddr = nodePrimaryAddr

	return true, nil
}

// isValidNodeIP detects if the IP is valid for a node.
// It excludes things like local IPs, mgmt port ip, special masquerade IP and Egress IPs
// for non-ovs type interfaces.
// Note, it possible that the node annotations may not be up to date when this check is executed.
// For this reason, sync is triggered on annotation change via addHandlerForAddrChange.
func (c *addressManager) isValidNodeIP(addr net.IP, linkIndex int) bool {
	if addr == nil {
		return false
	}
	if addr.IsLinkLocalUnicast() {
		return false
	}
	if addr.IsLoopback() {
		return false
	}
	// check CDN management port
	// mgmtPort is nil in DPUHost mode (the address manager is constructed without one),
	// so guard against a nil dereference before consulting it.
	if c.mgmtPort != nil {
		mgmtPortAddress, _ := util.MatchFirstIPNetFamily(utilnet.IsIPv6(addr), c.mgmtPort.GetAddresses())
		if mgmtPortAddress != nil && addr.Equal(mgmtPortAddress.IP) {
			return false
		}
	}

	if util.IsNetworkSegmentationSupportEnabled() {
		// check CDN + UDN management ports
		if mpLink, err := util.GetNetLinkOps().LinkByIndex(linkIndex); err != nil {
			klog.Errorf("Unable to determine if link is an OVN management port for address %s and link index %d: %v", addr.String(), linkIndex, err)
		} else {
			if strings.HasPrefix(mpLink.Attrs().Name, types.K8sMgmtIntfNamePrefix) {
				return false
			}
		}
	}

	if util.IsAddressReservedForInternalUse(addr) {
		return false
	}
	// Egress IP exclusions don't apply in DPUHost mode, and the address manager there
	// has no gateway bridge (c.gatewayBridge is nil), so skip this block to avoid a nil
	// dereference below.
	if config.OVNKubernetesFeature.EnableEgressIP && !config.IsModeDPUHost() {
		// EIP assigned to the primary interface which selects pods with a role primary user defined network must be excluded.
		if util.IsNetworkSegmentationSupportEnabled() && config.Gateway.Mode != config.GatewayModeDisabled {
			// Two methods to lookup EIPs assigned to the gateway bridge. Fast path from a shared cache or slow path from node annotations.
			// At startup, gateway bridge cache gets sync
			eipMarkIPs := c.gatewayBridge.GetEIPMarkIPs()
			if eipMarkIPs != nil && eipMarkIPs.HasSyncdOnce() && eipMarkIPs.IsIPPresent(addr) {
				return false
			} else {
				if eipAddresses, err := c.getPrimaryHostEgressIPs(); err != nil {
					klog.Errorf("Failed to get primary host assigned Egress IPs and ensure they are excluded: %v", err)
				} else {
					if eipAddresses.Has(addr.String()) {
						return false
					}
				}
			}
		}
		if !util.PlatformTypeIsEgressIPCloudProvider() {
			// IPs assigned to host interfaces to support the egress IP multi NIC feature must be excluded.
			if eipAddresses, err := c.getSecondaryHostEgressIPs(); err != nil {
				klog.Errorf("Failed to get secondary host assigned Egress IPs and ensure they are excluded: %v", err)
			} else {
				if eipAddresses.Has(addr.String()) {
					return false
				}
			}
		}
	}

	return true
}

func (c *addressManager) reconcileMasqueradeResources() {
	c.OnMasqueradeIPChanged()
	c.refreshGatewayIfIndex()
}

func (c *addressManager) refreshGatewayIfIndex() {
	if config.Gateway.Interface == "" {
		return
	}
	link, err := util.GetNetLinkOps().LinkByName(config.Gateway.Interface)
	if err != nil {
		klog.V(5).Infof("Gateway interface %s not found, resetting cached index: %v", config.Gateway.Interface, err)
		c.gatewayIfIndex = 0
		return
	}
	c.gatewayIfIndex = link.Attrs().Index
}

func (c *addressManager) sync() {
	if config.IsModeDPU() {
		return
	}

	var addrs []netlink.Addr

	if c.useNetlink {
		var err error
		addrs, err = util.GetNetLinkOps().AddrList(nil, getSupportedIPFamily())
		if err != nil {
			klog.Errorf("Failed to sync node addresses: unable to list all interface addresses: %v", err)
			return
		}
	}

	currAddresses := sets.New[string]()
	for _, addr := range addrs {
		if !c.isValidNodeIP(addr.IP, addr.LinkIndex) {
			klog.V(5).Infof("Skipping non-useable IP address for host: %s", addr.String())
			continue
		}
		netAddr := net.IPNet{IP: addr.IP, Mask: addr.Mask}
		currAddresses.Insert(netAddr.String())
	}

	addrChanged := c.assignCIDRs(currAddresses)
	c.handleNodePrimaryAddrChange()
	if addrChanged || !c.doNodeHostCIDRsMatch() {
		klog.Infof("Node address changed to %v. Updating annotations.", currAddresses)
		err := c.updateNodeAddressAnnotations()
		if err != nil {
			klog.Errorf("Address Manager failed to update node address annotations: %v", err)
		}
		c.notifyAddressesChanged()
	}
	c.reconcileMasqueradeResources()
}

// getSecondaryHostEgressIPs returns the set of egress IPs that are assigned to standard linux interfaces (non ovs type). The
// addresses are used to support Egress IP multi NIC feature. The addresses must not be included in address manager
// because the addresses are only to support Egress IP multi NIC feature and must not be exposed via host-cidrs annot.
func (c *addressManager) getSecondaryHostEgressIPs() (sets.Set[string], error) {
	node, err := c.watchFactory.GetNode(c.nodeName)
	if err != nil {
		return nil, fmt.Errorf("unable to get Node from informer: %v", err)
	}
	eipAddrs, err := util.ParseNodeSecondaryHostEgressIPsAnnotation(node)
	if err != nil {
		if util.IsAnnotationNotSetError(err) {
			return sets.New[string](), nil
		}
		return nil, err
	}
	return eipAddrs, nil
}

func (c *addressManager) getPrimaryHostEgressIPs() (sets.Set[string], error) {
	node, err := c.watchFactory.GetNode(c.nodeName)
	if err != nil {
		return nil, fmt.Errorf("unable to get Node from informer: %v", err)
	}
	eipAddrs, err := util.ParseNodeBridgeEgressIPsAnnotation(node)
	if err != nil {
		if util.IsAnnotationNotSetError(err) {
			eipAddrs = make([]string, 0)
		} else {
			return nil, err
		}
	}
	return sets.New[string](eipAddrs...), nil
}

// updateOVNEncapIPAndReconnect updates encap IP to OVS when the node primary IP changed.
func (c *addressManager) updateOVNEncapIPAndReconnect(newIP net.IP) {
	alreadyConfigured := false
	ovs, err := ovsops.GetOpenvSwitch(c.ovsClient)
	if err != nil {
		klog.Warningf("Unable to retrieve configured ovn-encap-ip from OVS: %v", err)
	} else if encapIP := ovs.ExternalIDs["ovn-encap-ip"]; encapIP != "" && newIP.String() == encapIP {
		klog.V(4).Infof("Will not update encap IP %s - it is already configured", newIP.String())
		alreadyConfigured = true
	}

	if !alreadyConfigured {
		if err := ovsops.UpdateOpenvSwitchExternalIDs(c.ovsClient, map[string]string{
			"ovn-encap-ip": newIP.String(),
		}); err != nil {
			klog.Errorf("Error setting OVS encap IP %s: %v", newIP.String(), err)
			return
		}
	}
	config.Default.EffectiveEncapIP = newIP.String()

	if !alreadyConfigured {
		// force ovn-controller to reconnect SB with new encap IP immediately.
		// otherwise there will be a max delay of 200s due to the 100s
		// ovn-controller inactivity probe. Best-effort: still let the
		// annotation below converge on failure.
		if _, stderr, err := util.RunOVNAppctlWithTimeout(5, "-t", "ovn-controller", "exit", "--restart"); err != nil {
			klog.Errorf("Failed to exit ovn-controller %v %q", err, stderr)
		}
	}

	// Update node-encap-ips annotation
	encapIPList := sets.New[string](config.Default.EffectiveEncapIP)
	if err := util.SetNodeEncapIPs(c.nodeAnnotator, encapIPList); err != nil {
		klog.Errorf("Failed to set node-encap-ips annotation for node %s: %v", c.nodeName, err)
		return
	}

	if err := c.nodeAnnotator.Run(); err != nil {
		klog.Errorf("Failed to set node %s annotations: %v", c.nodeName, err)
		return
	}
}

func getSupportedIPFamily() int {
	var ipFamily int // value of 0 means include both IP v4 and v6 addresses
	if config.IPv4Mode && !config.IPv6Mode {
		ipFamily = netlink.FAMILY_V4
	} else if !config.IPv4Mode && config.IPv6Mode {
		ipFamily = netlink.FAMILY_V6
	}
	return ipFamily
}
