// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package macbinding

import (
	"encoding/json"
	"fmt"
	"net"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// syncNodeIPsForUplink recomputes the node-IP ip->mac map for uplink, updates
// the cache, and enqueues a per-IP reconcile for each added, changed, or
// vanished IP.
func (c *MACBindingController) syncNodeIPsForUplink(uplink string) error {
	start := time.Now()
	fresh, err := c.getOnLinkNodeIPs(uplink)
	if err != nil {
		return err
	}
	ensure, remove := c.updateNodeIPCache(uplink, fresh)
	// Applies and removals go through the same key and queue so a given IP's
	// syncs are serialized; syncStaticMacBinding reads the cache to decide.
	for ip := range ensure.Union(remove) {
		c.enqueueStaticMacBinding(uplink, ip)
	}
	klog.V(5).Infof("Recomputed node IP static MAC bindings for uplink %q: %d current, %d to ensure, %d to remove, took %s",
		uplink, len(fresh), ensure.Len(), remove.Len(), time.Since(start))
	return nil
}

// updateNodeIPCache replaces the cached ip->mac map for uplink with fresh and
// returns the IPs to (re)apply (added or MAC changed) and to remove (vanished).
func (c *MACBindingController) updateNodeIPCache(uplink string, fresh map[string]string) (ensure, remove sets.Set[string]) {
	c.Lock()
	defer c.Unlock()
	old := c.nodeIPs[uplink]
	ensure, remove = sets.New[string](), sets.New[string]()
	for ip, mac := range fresh {
		if old[ip] != mac {
			ensure.Insert(ip)
		}
	}
	for ip := range old {
		if _, ok := fresh[ip]; !ok {
			remove.Insert(ip)
		}
	}
	if len(fresh) == 0 {
		delete(c.nodeIPs, uplink)
	} else {
		c.nodeIPs[uplink] = fresh
	}
	return
}

// getOnLinkNodeIPs collects the IPs of nodes on uplink's network segment (those
// directly L2-reachable, hence worth a static MAC binding) as an ip->mac map.
// Only the default (CDN) uplink is handled for now; dedicated uplinks return
// nothing.
func (c *MACBindingController) getOnLinkNodeIPs(uplink string) (map[string]string, error) {
	if uplink != "" {
		// TODO dedicated uplinks are not handled yet
		return nil, nil
	}
	return c.getDefaultOnLinkNodeIPs()
}

// getDefaultOnLinkNodeIPs implements getOnLinkNodeIPs for the default (CDN)
// uplink.
func (c *MACBindingController) getDefaultOnLinkNodeIPs() (map[string]string, error) {
	nodes, err := c.watchFactory.GetNodes()
	if err != nil {
		return nil, fmt.Errorf("failed to list nodes: %w", err)
	}

	localSubnets, err := c.getDefaultNodeSubnets()
	if err != nil {
		return nil, err
	}

	macBindings := map[string]string{}
	for _, node := range nodes {
		ips, mac := c.getDefaultNodeIPAndMAC(node)
		if mac == "" {
			continue
		}
		for _, ip := range ips {
			// keep only on-link IPs. localSubnets are the node's gateway subnets,
			// which exist only for enabled IP families, so this also drops IPs of a
			// disabled family.
			if !util.IsIPContainedInAnyCIDR(net.ParseIP(ip), localSubnets...) {
				continue
			}
			// When the same IP is transiently claimed by more than one node
			// (e.g. during a live migration) the smallest MAC wins, for a result
			// independent of node iteration order.
			if cur, ok := macBindings[ip]; !ok || mac < cur {
				macBindings[ip] = mac
			}
		}
	}
	return macBindings, nil
}

// getDefaultNodeSubnets returns this node's subnets on the default (CDN)
// uplink's network segment.
func (c *MACBindingController) getDefaultNodeSubnets() ([]*net.IPNet, error) {
	node, err := c.watchFactory.GetNode(c.nodeName)
	if err != nil {
		return nil, fmt.Errorf("failed to get node %q: %w", c.nodeName, err)
	}
	l3gw, err := util.ParseNodeL3GatewayAnnotation(node)
	if err != nil {
		return nil, fmt.Errorf("failed to parse l3 gateway config for node %q: %w", c.nodeName, err)
	}
	return l3gw.IPAddresses, nil
}

// getDefaultNodeIPAndMAC returns a node's internal IPs and its gateway MAC.
func (c *MACBindingController) getDefaultNodeIPAndMAC(node *corev1.Node) ([]string, string) {
	mac := l3GatewayMAC(node.Annotations[util.OvnNodeL3GatewayConfig])
	if mac == "" {
		return nil, ""
	}
	return internalIPs(node).UnsortedList(), mac
}

// registerNodeIPsHandlers subscribes to the node events that change the static
// bindings: a node being added, removed or its IPs or MAC changing.
func (c *MACBindingController) registerNodeIPsHandlers() error {
	var err error
	c.nodeEventHandler, err = c.watchFactory.NodeCoreInformer().Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { c.onNodeIPsChange(nil, obj) },
		UpdateFunc: func(oldObj, newObj any) { c.onNodeIPsChange(oldObj, newObj) },
		DeleteFunc: func(any) { c.enqueueNodeIPs("") },
	})
	if err != nil {
		return fmt.Errorf("failed to add node event handler: %w", err)
	}
	return nil
}

// deRegisterNodeIPsHandlers from informer.
func (c *MACBindingController) deRegisterNodeIPsHandlers() {
	if c.nodeEventHandler == nil {
		return
	}
	err := c.watchFactory.NodeCoreInformer().Informer().RemoveEventHandler(c.nodeEventHandler)
	if err != nil {
		klog.Errorf("Failed to remove node event handler: %v", err)
	}
}

// onNodeIPsChange enqueues a default uplink node IPs reconcile when any node's
// internal IPs or gateway MAC change, or local node subnet changes
func (c *MACBindingController) onNodeIPsChange(oldObj, newObj any) {
	newNode, ok := newObj.(*corev1.Node)
	if !ok {
		return
	}
	if oldNode, ok := oldObj.(*corev1.Node); ok &&
		internalIPs(oldNode).Equal(internalIPs(newNode)) &&
		!nodeGatewayMACChanged(oldNode, newNode) &&
		(newNode.Name != c.nodeName ||
			oldNode.Annotations[util.OvnNodeL3GatewayConfig] == newNode.Annotations[util.OvnNodeL3GatewayConfig]) {
		return
	}
	// enqueue default uplink ""
	c.enqueueNodeIPs("")
}

// internalIPs returns a node's internal IPs from its status.
func internalIPs(node *corev1.Node) sets.Set[string] {
	ips := sets.New[string]()
	for _, addr := range node.Status.Addresses {
		if addr.Type != corev1.NodeInternalIP {
			continue
		}
		ips.Insert(addr.Address)
	}
	return ips
}

// nodeGatewayMACChanged reports whether the gateway MAC in a node's
// l3-gateway-config annotation changed.
func nodeGatewayMACChanged(oldNode, newNode *corev1.Node) bool {
	oldAnnotation := oldNode.Annotations[util.OvnNodeL3GatewayConfig]
	newAnnotation := newNode.Annotations[util.OvnNodeL3GatewayConfig]
	if oldAnnotation == newAnnotation {
		return false
	}
	return l3GatewayMAC(oldAnnotation) != l3GatewayMAC(newAnnotation)
}

// l3GatewayMAC extracts just the default network's gateway MAC from a raw
// l3-gateway-config annotation value.
func l3GatewayMAC(annotation string) string {
	if annotation == "" {
		return ""
	}
	var cfgs map[string]struct {
		MACAddress string `json:"mac-address"`
	}
	if err := json.Unmarshal([]byte(annotation), &cfgs); err != nil {
		return ""
	}
	return cfgs[types.DefaultNetworkName].MACAddress
}
