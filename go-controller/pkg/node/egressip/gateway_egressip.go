// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package egressip

import (
	"encoding/json"
	"fmt"
	"net"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	corev1informers "k8s.io/client-go/informers/core/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"

	egressipv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	egressipinformers "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/informers/externalversions/egressip/v1"
	egressiplisters "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/listers/egressip/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/linkmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/egressip"
)

// markIPs contains packet mark and associated EgressIP IP for IPv4 / IPv6. Key is packet mark, value egress IP
type markIPs struct {
	v4 map[int]string
	v6 map[int]string
}

func (e markIPs) insert(mark util.EgressIPMark, ip net.IP) {
	if len(ip) == 0 || !mark.IsAvailable() || !mark.IsValid() {
		klog.Errorf("Insertion of EgressIP config failed: invalid: IP %v, mark %v", ip, mark)
		return
	}
	if ip.To4() != nil {
		e.v4[mark.ToInt()] = ip.String()
	} else if ip.To16() != nil {
		e.v6[mark.ToInt()] = ip.String()
	}
}

func (e markIPs) delete(mark util.EgressIPMark, ip net.IP) {
	if ip == nil || !mark.IsAvailable() || !mark.IsValid() {
		klog.Errorf("Deletion of EgressIP config failed: invalid: IP %v, mark %v", ip, mark)
		return
	}
	if ip.To4() != nil {
		delete(e.v4, mark.ToInt())
	} else if ip.To16() != nil {
		delete(e.v6, mark.ToInt())
	}
}

func (e markIPs) containsIP(ip net.IP) bool {
	if len(ip) == 0 {
		klog.Errorf("Invalid IP argument therefore not checking EgressIP config cache: IP %v", ip)
		return false
	}
	ipStr := ip.String()
	var m map[int]string
	if ip.To4() != nil {
		m = e.v4
	} else if ip.To16() != nil {
		m = e.v6
	}
	for _, existingIP := range m {
		if existingIP == ipStr {
			return true
		}
	}
	return false
}

type MarkIPsCache struct {
	mu          sync.Mutex
	hasSyncOnce bool
	markToIPs   markIPs
	IPToMark    map[string]int
}

func NewMarkIPsCache() *MarkIPsCache {
	return &MarkIPsCache{
		mu: sync.Mutex{},
		markToIPs: markIPs{
			v4: make(map[int]string),
			v6: make(map[int]string),
		},
		IPToMark: map[string]int{},
	}
}

func (mic *MarkIPsCache) IsIPPresent(ip net.IP) bool {
	mic.mu.Lock()
	defer mic.mu.Unlock()
	if ip == nil {
		return false
	}
	_, isFound := mic.IPToMark[ip.String()]
	return isFound
}

func (mic *MarkIPsCache) insertMarkIP(pktMark util.EgressIPMark, ip net.IP) {
	mic.mu.Lock()
	defer mic.mu.Unlock()
	if ip == nil {
		return
	}
	mic.markToIPs.insert(pktMark, ip)
	mic.IPToMark[ip.String()] = pktMark.ToInt()
}

func (mic *MarkIPsCache) deleteMarkIP(pktMark util.EgressIPMark, ip net.IP) {
	mic.mu.Lock()
	defer mic.mu.Unlock()
	if ip == nil {
		return
	}
	mic.markToIPs.delete(pktMark, ip)
	delete(mic.IPToMark, ip.String())
}

func (mic *MarkIPsCache) replaceAll(markIPs markIPs) {
	mic.mu.Lock()
	mic.markToIPs = markIPs
	mic.IPToMark = make(map[string]int, len(markIPs.v4)+len(markIPs.v6))
	for mark, ipv4 := range markIPs.v4 {
		mic.IPToMark[ipv4] = mark
	}
	for mark, ipv6 := range markIPs.v6 {
		mic.IPToMark[ipv6] = mark
	}
	mic.mu.Unlock()
}

func (mic *MarkIPsCache) GetIPv4() map[int]string {
	mic.mu.Lock()
	defer mic.mu.Unlock()
	dupe := make(map[int]string)
	for key, value := range mic.markToIPs.v4 {
		if value == "" {
			continue
		}
		dupe[key] = value
	}
	return dupe
}

func (mic *MarkIPsCache) GetIPv6() map[int]string {
	mic.mu.Lock()
	defer mic.mu.Unlock()
	dupe := make(map[int]string)
	for key, value := range mic.markToIPs.v6 {
		if value == "" {
			continue
		}
		dupe[key] = value
	}
	return dupe
}

func (mic *MarkIPsCache) HasSyncdOnce() bool {
	mic.mu.Lock()
	defer mic.mu.Unlock()
	return mic.hasSyncOnce
}

func (mic *MarkIPsCache) setSyncdOnce() {
	mic.mu.Lock()
	mic.hasSyncOnce = true
	mic.mu.Unlock()
}

type BridgeEIPAddrManager struct {
	nodeName         string
	bridgeName       string
	nodeAnnotationMu sync.Mutex
	annotationIPs    sets.Set[string]
	eIPLister        egressiplisters.EgressIPLister
	eIPInformer      cache.SharedIndexInformer
	nodeLister       corev1listers.NodeLister
	kube             kube.Interface
	addrManager      *linkmanager.Controller
	cache            *MarkIPsCache
}

// NewBridgeEIPAddrManager manages EgressIP IPs that must be added to ovs bridges to support EgressIP feature for user
// defined networks. It saves the assigned IPs to its respective Node annotation in-order to understand which IPs it assigned
// prior to restarting.
// It provides the assigned IPs info node IP handler. Node IP handler must not consider assigned EgressIP IPs as possible node IPs.
// Openflow manager must generate the SNAT openflow conditional on packet marks and therefore needs access to EIP IPs and associated packet marks.
// BridgeEIPAddrManager must be able to force Openflow manager to resync if EgressIP assignment for the node changes.
func NewBridgeEIPAddrManager(nodeName, bridgeName string, linkManager *linkmanager.Controller,
	kube kube.Interface, eIPInformer egressipinformers.EgressIPInformer, nodeInformer corev1informers.NodeInformer) *BridgeEIPAddrManager {
	mgr := &BridgeEIPAddrManager{
		nodeName:         nodeName,     // k8 node name
		bridgeName:       bridgeName,   // bridge name for which EIP IPs are managed
		nodeAnnotationMu: sync.Mutex{}, // mu for updating Node annotation
		annotationIPs:    sets.New[string](),
		eIPLister:        eIPInformer.Lister(),
		eIPInformer:      eIPInformer.Informer(),
		nodeLister:       nodeInformer.Lister(),
		kube:             kube,
		addrManager:      linkManager,
		cache:            NewMarkIPsCache(), // cache to store pkt mark -> EIP IP.
	}

	_, err := nodeInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(old, cur interface{}) {
			mgr.handleNodeLabelRemoval(old, cur)
		},
	})
	if err != nil {
		klog.Errorf("Failed to add node event handler for EgressIP label removal: %v", err)
	}

	return mgr
}

func (g *BridgeEIPAddrManager) GetCache() *MarkIPsCache {
	return g.cache
}

func (g *BridgeEIPAddrManager) AddEgressIP(eip *egressipv1.EgressIP) (bool, error) {
	ip, pktMark, shouldSkip, err := g.parseAndValidateEIP(eip)
	if err != nil {
		return false, fmt.Errorf("failed to add EgressIP gateway config because unable to parse and validate EgressIP: %v", err)
	}
	if shouldSkip {
		return false, nil
	}
	// must always add to cache before adding IP because we want to inform node ip handler that this is not a valid node IP
	g.cache.insertMarkIP(pktMark, ip)
	if err = g.addIPToAnnotation(ip); err != nil {
		return false, fmt.Errorf("failed to add EgressIP gateway config because unable to add EgressIP IP to Node annotation: %v", err)
	}
	if err = g.addIPBridge(ip); err != nil {
		return false, fmt.Errorf("failed to add EgressIP gateway config because failed to add address to link: %v", err)
	}
	return true, nil
}

func (g *BridgeEIPAddrManager) UpdateEgressIP(oldEIP, newEIP *egressipv1.EgressIP) (bool, error) {
	// Parse and validate old EgressIP
	oldIP, oldMark, oldSkip, err := g.parseAndValidateEIP(oldEIP)
	if err != nil {
		return false, fmt.Errorf("unable to parse and validate old EgressIP: %v", err)
	}

	// Parse and validate new EgressIP
	newIP, newMark, newSkip, err := g.parseAndValidateEIP(newEIP)
	if err != nil {
		return false, fmt.Errorf("unable to parse and validate new EgressIP: %v", err)
	}

	// Both should be skipped - no change
	if oldSkip && newSkip {
		return false, nil
	}

	// Both exist and are the same - no change
	if !oldSkip && !newSkip && oldIP.Equal(newIP) {
		return false, nil
	}

	var isUpdated bool

	// Delete old from bridge and cache if it exists
	if !oldSkip {
		if err = g.deleteIPBridge(oldIP); err != nil {
			return isUpdated, fmt.Errorf("failed to update EgressIP gateway config because failed to delete address from link: %v", err)
		}
		g.cache.deleteMarkIP(oldMark, oldIP)
		isUpdated = true
	}

	// Add new to cache before annotation update
	if !newSkip {
		g.cache.insertMarkIP(newMark, newIP)
	}

	// Combined annotation update: delete old and add new in a single patch
	// to avoid stale-lister races between consecutive patches.
	var addIPs, delIPs []net.IP
	if !oldSkip {
		delIPs = []net.IP{oldIP}
	}
	if !newSkip {
		addIPs = []net.IP{newIP}
	}
	if err = g.updateAnnotationIPs(addIPs, delIPs); err != nil {
		return isUpdated, fmt.Errorf("failed to update EgressIP gateway config because unable to update Node annotation: %v", err)
	}

	// Add new to bridge if it exists
	if !newSkip {
		if err = g.addIPBridge(newIP); err != nil {
			return isUpdated, fmt.Errorf("failed to update EgressIP gateway config because failed to add address to link: %v", err)
		}
		isUpdated = true
	}

	return isUpdated, nil
}

func (g *BridgeEIPAddrManager) DeleteEgressIP(eip *egressipv1.EgressIP) (bool, error) {
	ip, pktMark, shouldSkip, err := g.parseAndValidateEIP(eip)
	if err != nil {
		return false, fmt.Errorf("failed to delete EgressIP gateway config because unable to parse and validate EgressIP: %v", err)
	}
	// Skip secondary network IPs. Cleanup of stale secondary IPs from old code is handled in SyncEgressIP.
	if shouldSkip {
		return false, nil
	}
	if err = g.deleteIPBridge(ip); err != nil {
		return false, fmt.Errorf("failed to delete EgressIP gateway config because failed to delete address from link: %v", err)
	}
	g.cache.deleteMarkIP(pktMark, ip)
	if err = g.deleteIPsFromAnnotation(ip); err != nil {
		return false, fmt.Errorf("failed to delete EgressIP gateway config because failed to delete EgressIP IP from Node annotation: %v", err)
	}
	return true, nil
}

func (g *BridgeEIPAddrManager) SyncEgressIP(objs []interface{}) error {
	// caller must synchronise
	annotIPs, err := g.getAnnotationIPs()
	if err != nil {
		return fmt.Errorf("failed to sync EgressIP gateway config because unable to get Node annotation: %v", err)
	}
	g.nodeAnnotationMu.Lock()
	g.annotationIPs = sets.New[string](getIPsStr(annotIPs...)...)
	g.nodeAnnotationMu.Unlock()
	configs := markIPs{v4: map[int]string{}, v6: map[int]string{}}
	ipsToAdd := make([]net.IP, 0)
	for _, obj := range objs {
		eip, ok := obj.(*egressipv1.EgressIP)
		if !ok {
			return fmt.Errorf("expected EgressIP type but received %T", obj)
		}
		ip, pktMark, shouldSkip, err := g.parseAndValidateEIP(eip)
		if err != nil {
			klog.Errorf("Failed to sync EgressIP %s gateway config because unable to parse and validate EgressIP: %v", eip.Name, err)
			continue
		}
		if shouldSkip {
			// Skip IPs not on OVN network (i.e., secondary network IPs)
			continue
		}
		configs.insert(pktMark, ip)
		if err = g.addIPBridge(ip); err != nil {
			return fmt.Errorf("failed to sync EgressIP gateway config because failed to add address to link: %v", err)
		}
		ipsToAdd = append(ipsToAdd, ip)
	}
	ipsToDel := make([]net.IP, 0)
	for _, annotIP := range annotIPs {
		if configs.containsIP(annotIP) {
			continue
		}
		if err = g.deleteIPBridge(annotIP); err != nil {
			return fmt.Errorf("failed to delete stale EgressIP IP %s from bridge: %v", annotIP, err)
		}
		ipsToDel = append(ipsToDel, annotIP)
	}
	if len(ipsToAdd) > 0 || len(ipsToDel) > 0 {
		if err = g.updateAnnotationIPs(ipsToAdd, ipsToDel); err != nil {
			return fmt.Errorf("failed to update EgressIP IPs in Node annotation: %v", err)
		}
	}
	g.cache.replaceAll(configs)
	g.cache.setSyncdOnce()
	return nil
}

// updateAnnotationLocked updates the node's egress IPs
// Must be called with nodeAnnotationMu locked
func (g *BridgeEIPAddrManager) updateAnnotationLocked(updatedIPs sets.Set[string]) error {
	return retry.OnError(retry.DefaultRetry, util.IsNodeAnnotationPatchRetryable, func() error {
		node, err := g.nodeLister.Get(g.nodeName)
		if err != nil {
			return err
		}
		patch, err := json.Marshal(updatedIPs.UnsortedList())
		if err != nil {
			return err
		}
		nodeToUpdate := node.DeepCopy()
		if nodeToUpdate.Annotations == nil {
			nodeToUpdate.Annotations = map[string]string{}
		}
		nodeToUpdate.Annotations[util.OVNNodeBridgeEgressIPs] = string(patch)
		return g.kube.PatchNodeStatusAnnotations(node, nodeToUpdate)
	})
}

// updateAnnotationIPs atomically adds and deletes IPs from the node annotation
// in a single patch. Either addIPs or delIPs may be nil.
func (g *BridgeEIPAddrManager) updateAnnotationIPs(addIPs, delIPs []net.IP) error {
	g.nodeAnnotationMu.Lock()
	defer g.nodeAnnotationMu.Unlock()
	updatedIPs := sets.New[string](g.annotationIPs.UnsortedList()...)
	updatedIPs.Delete(getIPsStr(delIPs...)...)
	updatedIPs.Insert(getIPsStr(addIPs...)...)
	if updatedIPs.Equal(g.annotationIPs) {
		return nil
	}
	if err := g.updateAnnotationLocked(updatedIPs); err != nil {
		return err
	}
	g.annotationIPs = updatedIPs
	return nil
}

func (g *BridgeEIPAddrManager) addIPToAnnotation(candidateIP net.IP) error {
	return g.updateAnnotationIPs([]net.IP{candidateIP}, nil)
}

func (g *BridgeEIPAddrManager) deleteIPsFromAnnotation(candidateIPs ...net.IP) error {
	return g.updateAnnotationIPs(nil, candidateIPs)
}

func (g *BridgeEIPAddrManager) addIPBridge(ip net.IP) error {
	link, err := util.GetNetLinkOps().LinkByName(g.bridgeName)
	if err != nil {
		return fmt.Errorf("failed to get link obj by name %s: %v", g.bridgeName, err)
	}
	return g.addrManager.AddAddress(*egressip.GetNetlinkAddress(ip, link.Attrs().Index))
}

func (g *BridgeEIPAddrManager) deleteIPBridge(ip net.IP) error {
	link, err := util.GetNetLinkOps().LinkByName(g.bridgeName)
	if err != nil {
		return fmt.Errorf("failed to get link obj by name %s: %v", g.bridgeName, err)
	}
	return g.addrManager.DelAddress(*egressip.GetNetlinkAddress(ip, link.Attrs().Index))
}

// handleNodeLabelRemoval watches for the egress-assignable label being removed
// from this node. When detected, it immediately removes all EgressIP addresses
// from br-ex so this node stops responding to ARP before the cluster manager
// reassigns the EgressIPs to another node. This prevents the ARP conflict that
// occurs when both old and new nodes claim the same IP simultaneously.
//
// Only the IP is removed from the interface — the link manager store is left
// intact so that its periodic sync can re-add the addresses if the label
// removal was accidental (removed and re-added quickly).
func (g *BridgeEIPAddrManager) handleNodeLabelRemoval(old, cur interface{}) {
	oldNode, ok := old.(*corev1.Node)
	if !ok {
		return
	}
	newNode, ok := cur.(*corev1.Node)
	if !ok {
		return
	}
	if newNode.Name != g.nodeName {
		return
	}
	_, oldHasLabel := oldNode.Labels[util.GetNodeEgressLabel()]
	_, newHasLabel := newNode.Labels[util.GetNodeEgressLabel()]
	if !oldHasLabel || newHasLabel {
		return
	}
	klog.Infof("Egress-assignable label removed from node %s, preemptively removing EgressIP addresses from bridge %s",
		g.nodeName, g.bridgeName)
	g.preemptiveEgressIPCleanup()
}

// preemptiveEgressIPCleanup removes all EgressIP addresses from the bridge
// interface using direct netlink AddrDel. It intentionally bypasses the link
// manager's DelAddress to preserve the link manager's store — if the label
// removal was accidental, the link manager's periodic sync (every 2 min) will
// re-add the addresses. For a real failover, the normal EgressIP handler will
// later call deleteIPBridge which properly cleans up the store.
func (g *BridgeEIPAddrManager) preemptiveEgressIPCleanup() {
	link, err := util.GetNetLinkOps().LinkByName(g.bridgeName)
	if err != nil {
		klog.Errorf("Failed to get bridge %s for preemptive EgressIP cleanup: %v", g.bridgeName, err)
		return
	}

	for _, ipStr := range g.cache.GetIPv4() {
		if ip := net.ParseIP(ipStr); ip != nil {
			addr := egressip.GetNetlinkAddress(ip, link.Attrs().Index)
			if err := util.GetNetLinkOps().AddrDel(link, addr); err != nil {
				klog.V(4).Infof("Preemptive removal of EgressIP %s from bridge %s: %v", ip, g.bridgeName, err)
			} else {
				klog.Infof("Preemptively removed EgressIP %s from bridge %s", ip, g.bridgeName)
			}
		}
	}
	for _, ipStr := range g.cache.GetIPv6() {
		if ip := net.ParseIP(ipStr); ip != nil {
			addr := egressip.GetNetlinkAddress(ip, link.Attrs().Index)
			if err := util.GetNetLinkOps().AddrDel(link, addr); err != nil {
				klog.V(4).Infof("Preemptive removal of EgressIP %s from bridge %s: %v", ip, g.bridgeName, err)
			} else {
				klog.Infof("Preemptively removed EgressIP %s from bridge %s", ip, g.bridgeName)
			}
		}
	}
}

// getAnnotationIPs retrieves the egress IP annotation from the current node Nodes object. If multiple users, callers must synchronise.
// if annotation isn't present, empty set is returned
func (g *BridgeEIPAddrManager) getAnnotationIPs() ([]net.IP, error) {
	node, err := g.nodeLister.Get(g.nodeName)
	if err != nil {
		return nil, fmt.Errorf("failed to get node %s from lister: %v", g.nodeName, err)
	}
	ipsStr, err := util.ParseNodeBridgeEgressIPsAnnotation(node)
	if err != nil {
		if util.IsAnnotationNotSetError(err) {
			ipsStr = make([]string, 0)
		} else {
			return nil, err
		}
	}
	ips := make([]net.IP, 0, len(ipsStr))
	for _, ipStr := range ipsStr {
		ip := net.ParseIP(ipStr)
		if ip == nil {
			return nil, fmt.Errorf("failed to parse IPs from Node annotation %s: %v", util.OVNNodeBridgeEgressIPs, ipsStr)
		}
		ips = append(ips, ip)
	}
	return ips, nil
}

// isOVNNetworkIP checks if the given IP belongs to the OVN (primary) network
// Returns true if the IP is on the OVN network, false if it's on a secondary network
func (g *BridgeEIPAddrManager) isOVNNetworkIP(ip net.IP) (bool, error) {
	node, err := g.nodeLister.Get(g.nodeName)
	if err != nil {
		return false, fmt.Errorf("failed to get node %s: %v", g.nodeName, err)
	}
	nodePrimaryIPs, err := util.ParseNodePrimaryIfAddr(node)
	if err != nil {
		return false, fmt.Errorf("failed to parse node primary interface address for node %s: %v", g.nodeName, err)
	}
	return util.IsOVNNetwork(nodePrimaryIPs, ip), nil
}

// parseAndValidateEIP parses and validates an EgressIP for this node
// Returns:
// - ip: the parsed IP address
// - pktMark: the parsed packet mark
// - shouldSkip: true if this EgressIP should be skipped (e.g., no mark set, not assigned to this node, belongs to secondary network)
// - error: any error encountered during parsing/validation
func (g *BridgeEIPAddrManager) parseAndValidateEIP(eip *egressipv1.EgressIP) (net.IP, util.EgressIPMark, bool, error) {
	var pktMark util.EgressIPMark

	// Check if packet mark is set
	// This scenario may occur during upgrade from when ovn-k didn't apply marks to EIP objs
	if !util.IsEgressIPMarkSet(eip.Annotations) {
		return nil, pktMark, true, nil
	}

	// Find the EgressIP assigned to this node
	var eipAddr string
	for _, status := range eip.Status.Items {
		if status.Node == g.nodeName {
			eipAddr = status.EgressIP
			break
		}
	}
	if eipAddr == "" {
		return nil, pktMark, true, nil
	}

	// Parse the IP address
	ip := net.ParseIP(eipAddr)
	if ip == nil {
		return nil, pktMark, false, fmt.Errorf("failed to parse EgressIP %s", eipAddr)
	}

	// Parse the packet mark
	var err error
	pktMark, err = util.ParseEgressIPMark(eip.Annotations)
	if err != nil {
		return nil, pktMark, false, fmt.Errorf("failed to extract packet mark from EgressIP annotations: %v", err)
	}

	// Validate packet mark
	if !pktMark.IsAvailable() {
		return nil, pktMark, false, fmt.Errorf("packet mark is not set")
	}
	if !pktMark.IsValid() {
		return nil, pktMark, false, fmt.Errorf("packet mark is not valid")
	}

	// Check if this IP belongs to the OVN (primary) network
	isOVN, err := g.isOVNNetworkIP(ip)
	if err != nil {
		return nil, pktMark, false, fmt.Errorf("failed to check if IP is OVN network: %w", err)
	}
	if !isOVN {
		// Skip IPs not on OVN network (i.e., secondary network IPs)
		klog.V(5).Infof("Skipping EgressIP %s on bridge %s because it does not belong to the OVN network", ip.String(), g.bridgeName)
		return ip, pktMark, true, nil
	}

	return ip, pktMark, false, nil
}

func getIPsStr(ips ...net.IP) []string {
	ipsStr := make([]string, 0, len(ips))
	for _, ip := range ips {
		ipsStr = append(ipsStr, ip.String())
	}
	return ipsStr
}
