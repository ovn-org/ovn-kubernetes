package node

import (
	"encoding/json"
	"fmt"
	"math"
	"net"
	"sync"

	egressipv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	egressipinformers "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/informers/externalversions/egressip/v1"
	egressiplisters "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/listers/egressip/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/linkmanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"k8s.io/apimachinery/pkg/util/sets"
	corev1informers "k8s.io/client-go/informers/core/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
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

type markIPsCache struct {
	mu          sync.Mutex
	hasSyncOnce bool
	markToIPs   markIPs
	IPToMark    map[string]int
}

func newMarkIPsCache() *markIPsCache {
	return &markIPsCache{
		mu: sync.Mutex{},
		markToIPs: markIPs{
			v4: make(map[int]string),
			v6: make(map[int]string),
		},
		IPToMark: map[string]int{},
	}
}

func (mic *markIPsCache) IsIPPresent(ip net.IP) bool {
	mic.mu.Lock()
	defer mic.mu.Unlock()
	if ip == nil {
		return false
	}
	_, isFound := mic.IPToMark[ip.String()]
	return isFound
}

func (mic *markIPsCache) insertMarkIP(pktMark util.EgressIPMark, ip net.IP) {
	mic.mu.Lock()
	defer mic.mu.Unlock()
	if ip == nil {
		return
	}
	mic.markToIPs.insert(pktMark, ip)
	mic.IPToMark[ip.String()] = pktMark.ToInt()
}

func (mic *markIPsCache) deleteMarkIP(pktMark util.EgressIPMark, ip net.IP) {
	mic.mu.Lock()
	defer mic.mu.Unlock()
	if ip == nil {
		return
	}
	mic.markToIPs.delete(pktMark, ip)
	delete(mic.IPToMark, ip.String())
}

func (mic *markIPsCache) replaceAll(markIPs markIPs) {
	mic.mu.Lock()
	mic.markToIPs = markIPs
	for mark, ipv4 := range markIPs.v4 {
		mic.IPToMark[ipv4] = mark
	}
	for mark, ipv6 := range markIPs.v6 {
		mic.IPToMark[ipv6] = mark
	}
	mic.mu.Unlock()
}

func (mic *markIPsCache) GetIPv4() map[int]string {
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

func (mic *markIPsCache) GetIPv6() map[int]string {
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

func (mic *markIPsCache) HasSyncdOnce() bool {
	mic.mu.Lock()
	defer mic.mu.Unlock()
	return mic.hasSyncOnce
}

func (mic *markIPsCache) setSyncdOnce() {
	mic.mu.Lock()
	mic.hasSyncOnce = true
	mic.mu.Unlock()
}

type bridgeEIPAddrManager struct {
	nodeName         string
	bridgeName       string
	nodeAnnotationMu sync.Mutex
	eIPLister        egressiplisters.EgressIPLister
	eIPInformer      cache.SharedIndexInformer
	nodeLister       corev1listers.NodeLister
	kube             kube.Interface
	addrManager      *linkmanager.Controller
	cache            *markIPsCache
}

// newBridgeEIPAddrManager manages EgressIP IPs that must be added to ovs bridges to support EgressIP feature for user
// defined networks. It saves the assigned IPs to its respective Node annotation in-order to understand which IPs it assigned
// prior to restarting.
// It provides the assigned IPs info node IP handler. Node IP handler must not consider assigned EgressIP IPs as possible node IPs.
// Openflow manager must generate the SNAT openflow conditional on packet marks and therefore needs access to EIP IPs and associated packet marks.
// bridgeEIPAddrManager must be able to force Openflow manager to resync if EgressIP assignment for the node changes.
func newBridgeEIPAddrManager(nodeName, bridgeName string, linkManager *linkmanager.Controller,
	kube kube.Interface, eIPInformer egressipinformers.EgressIPInformer, nodeInformer corev1informers.NodeInformer) *bridgeEIPAddrManager {
	return &bridgeEIPAddrManager{
		nodeName:         nodeName,     // k8 node name
		bridgeName:       bridgeName,   // bridge name for which EIP IPs are managed
		nodeAnnotationMu: sync.Mutex{}, // mu for updating Node annotation
		eIPLister:        eIPInformer.Lister(),
		eIPInformer:      eIPInformer.Informer(),
		nodeLister:       nodeInformer.Lister(),
		kube:             kube,
		addrManager:      linkManager,
		cache:            newMarkIPsCache(), // cache to store pkt mark -> EIP IP.
	}
}

func (g *bridgeEIPAddrManager) GetCache() *markIPsCache {
	return g.cache
}

func (g *bridgeEIPAddrManager) addEgressIP(eip *egressipv1.EgressIP) (bool, error) {
	var isUpdated bool
	if !util.IsEgressIPMarkSet(eip.Annotations) {
		return isUpdated, nil
	}
	for _, status := range eip.Status.Items {
		if status.Node != g.nodeName {
			continue
		}
		ip, pktMark, err := parseEIPMarkIP(eip.Annotations, status.EgressIP)
		if err != nil {
			return isUpdated, fmt.Errorf("failed to add EgressIP gateway config because unable to extract config from EgressIP obj: %v", err)
		}
		// must always add to cache before adding IP because we want to inform node ip handler that this is not a valid node IP
		g.cache.insertMarkIP(pktMark, ip)
		if err = g.addIPToAnnotation(ip); err != nil {
			return isUpdated, fmt.Errorf("failed to add EgressIP gateway config because unable to add EgressIP IP to Node annotation: %v", err)
		}
		if err = g.addIPBridge(ip); err != nil {
			return isUpdated, fmt.Errorf("failed to add EgressIP gateway config because failed to add address to link: %v", err)
		}
		isUpdated = true
		break // no need to continue as only one EIP IP is assigned to a node
	}
	return isUpdated, nil
}

func (g *bridgeEIPAddrManager) updateEgressIP(oldEIP, newEIP *egressipv1.EgressIP) (bool, error) {
	var isUpdated bool
	// at most, one status item for this node will be found.
	for _, oldStatus := range oldEIP.Status.Items {
		if oldStatus.Node != g.nodeName {
			continue
		}
		if !util.IsEgressIPMarkSet(oldEIP.Annotations) {
			// this scenario may occur during upgrade from when ovn-k didn't apply marks to EIP objs
			break
		}
		if util.IsItemInSlice(newEIP.Status.Items, oldStatus) {
			// if one status entry exists in both status items, then nothing needs to be done because no status update.
			// also, because at most only one status item can be assigned to a node, we can return early.
			return isUpdated, nil
		}
		ip, pktMark, err := parseEIPMarkIP(oldEIP.Annotations, oldStatus.EgressIP)
		if err != nil {
			return isUpdated, fmt.Errorf("failed to update EgressIP SNAT for ext bridge cache because unable to extract config from old EgressIP obj: %v", err)
		}
		if err = g.deleteIPBridge(ip); err != nil {
			return isUpdated, fmt.Errorf("failed to update EgressIP gateway config because failed to delete address from link: %v", err)
		}
		g.cache.deleteMarkIP(pktMark, ip)
		if err = g.deleteIPsFromAnnotation(ip); err != nil {
			return isUpdated, fmt.Errorf("failed to update EgressIP gateway config because unable to delete EgressIP IP from Node annotation: %v", err)
		}
		isUpdated = true
		break
	}
	for _, newStatus := range newEIP.Status.Items {
		if newStatus.Node != g.nodeName {
			continue
		}
		if !util.IsEgressIPMarkSet(newEIP.Annotations) {
			// this scenario may occur during upgrade from when ovn-k didn't apply marks to EIP objs
			return isUpdated, nil
		}
		ip, pktMark, err := parseEIPMarkIP(newEIP.Annotations, newStatus.EgressIP)
		if err != nil {
			return isUpdated, fmt.Errorf("failed to update EgressIP gateway config because unable to extract config from EgressIP obj: %v", err)
		}
		// must always add to OF cache before adding IP because we want to inform node ip handler that this is not a valid node IP
		g.cache.insertMarkIP(pktMark, ip)
		if err = g.addIPToAnnotation(ip); err != nil {
			return isUpdated, fmt.Errorf("failed to update EgressIP gateway config because unable to add EgressIP IP to Node annotation: %v", err)
		}
		if err = g.addIPBridge(ip); err != nil {
			return isUpdated, fmt.Errorf("failed to update EgressIP gateway config because failed to add address to link: %v", err)
		}
		isUpdated = true
		break
	}
	return isUpdated, nil
}

func (g *bridgeEIPAddrManager) deleteEgressIP(eip *egressipv1.EgressIP) (bool, error) {
	var isUpdated bool
	if !util.IsEgressIPMarkSet(eip.Annotations) {
		return isUpdated, nil
	}
	for _, status := range eip.Status.Items {
		if status.Node != g.nodeName {
			continue
		}
		if !util.IsEgressIPMarkSet(eip.Annotations) {
			continue
		}
		ip, pktMark, err := parseEIPMarkIP(eip.Annotations, status.EgressIP)
		if err != nil {
			return isUpdated, fmt.Errorf("failed to delete EgressIP gateway config because unable to extract config from EgressIP obj: %v", err)
		}
		if err = g.deleteIPBridge(ip); err != nil {
			return isUpdated, fmt.Errorf("failed to delete EgressIP gateway config because failed to delete address from link: %v", err)
		}
		g.cache.deleteMarkIP(pktMark, ip)
		if err = g.deleteIPsFromAnnotation(ip); err != nil {
			return isUpdated, fmt.Errorf("failed to delete EgressIP gateway config because failed to delete EgressIP IP from Node annotation: %v", err)
		}
		isUpdated = true
		break // no need to continue as only one EIP IP is assigned per node
	}
	return isUpdated, nil
}

func (g *bridgeEIPAddrManager) syncEgressIP(objs []interface{}) error {
	// caller must synchronise
	annotIPs, err := g.getAnnotationIPs()
	if err != nil {
		return fmt.Errorf("failed to sync EgressIP gateway config because unable to get Node annotation: %v", err)
	}
	configs := markIPs{v4: map[int]string{}, v6: map[int]string{}}
	for _, obj := range objs {
		eip, ok := obj.(*egressipv1.EgressIP)
		if !ok {
			return fmt.Errorf("expected EgressIP type but received %T", obj)
		}
		// This may happen during upgrade when node controllers upgrade before cluster manager upgrades when cluster manager doesn't contain func
		// to add a pkt mark to EgressIPs.
		if !util.IsEgressIPMarkSet(eip.Annotations) {
			continue
		}
		for _, status := range eip.Status.Items {
			if status.Node != g.nodeName {
				continue
			}
			if ip, pktMark, err := parseEIPMarkIP(eip.Annotations, status.EgressIP); err != nil {
				klog.Errorf("Failed to sync EgressIP %s gateway config because unable to extract config from EIP obj: %v", eip.Name, err)
			} else {
				configs.insert(pktMark, ip)
				if err = g.addIPToAnnotation(ip); err != nil {
					return fmt.Errorf("failed to sync EgressIP gateway config because unable to add EgressIP IP to Node annotation: %v", err)
				}
				if err = g.addIPBridge(ip); err != nil {
					return fmt.Errorf("failed to sync EgressIP gateway config because failed to add address to link: %v", err)
				}
			}
			break
		}
	}
	ipsToDel := make([]net.IP, 0)
	for _, annotIP := range annotIPs {
		if configs.containsIP(annotIP) {
			continue
		}
		if err = g.deleteIPBridge(annotIP); err != nil {
			klog.Errorf("Failed to delete stale EgressIP IP %s from gateway: %v", annotIP, err)
			continue
		}
		ipsToDel = append(ipsToDel, annotIP)
	}
	if len(ipsToDel) > 0 {
		if err = g.deleteIPsFromAnnotation(ipsToDel...); err != nil {
			return fmt.Errorf("failed to delete EgressIP IPs from Node annotation: %v", err)
		}
	}
	g.cache.replaceAll(configs)
	g.cache.setSyncdOnce()
	return nil
}

// addIPToAnnotation adds an address to the collection of existing addresses stored in the nodes annotation. Caller
// may repeat addition of addresses without care for duplicate addresses being added.
func (g *bridgeEIPAddrManager) addIPToAnnotation(candidateIP net.IP) error {
	g.nodeAnnotationMu.Lock()
	defer g.nodeAnnotationMu.Unlock()
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		node, err := g.nodeLister.Get(g.nodeName)
		if err != nil {
			return err
		}
		existingIPsStr, err := util.ParseNodeBridgeEgressIPsAnnotation(node)
		if err != nil {
			if util.IsAnnotationNotSetError(err) {
				existingIPsStr = make([]string, 0)
			} else {
				return fmt.Errorf("failed to parse annotation key %q from node object: %v", util.OVNNodeBridgeEgressIPs, err)
			}
		}
		existingIPsSet := sets.New[string](existingIPsStr...)
		candidateIPStr := candidateIP.String()
		if existingIPsSet.Has(candidateIPStr) {
			return nil
		}
		patch, err := json.Marshal(existingIPsSet.Insert(candidateIPStr).UnsortedList())
		if err != nil {
			return err
		}
		node.Annotations[util.OVNNodeBridgeEgressIPs] = string(patch)
		return g.kube.UpdateNodeStatus(node)
	})
}

// deleteIPsFromAnnotation deletes address from annotation. If multiple users, callers must synchronise.
// deletion of address that doesn't exist will not cause an error.
func (g *bridgeEIPAddrManager) deleteIPsFromAnnotation(candidateIPs ...net.IP) error {
	g.nodeAnnotationMu.Lock()
	defer g.nodeAnnotationMu.Unlock()
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		node, err := g.nodeLister.Get(g.nodeName)
		if err != nil {
			return err
		}
		existingIPsStr, err := util.ParseNodeBridgeEgressIPsAnnotation(node)
		if err != nil {
			if util.IsAnnotationNotSetError(err) {
				existingIPsStr = make([]string, 0)
			} else {
				return fmt.Errorf("failed to parse annotation key %q from node object: %v", util.OVNNodeBridgeEgressIPs, err)
			}
		}
		if len(existingIPsStr) == 0 {
			return nil
		}
		existingIPsSet := sets.New[string](existingIPsStr...)
		candidateIPsStr := getIPsStr(candidateIPs...)
		if !existingIPsSet.HasAny(candidateIPsStr...) {
			return nil
		}
		existingIPsSet.Delete(candidateIPsStr...)
		patch, err := json.Marshal(existingIPsSet.UnsortedList())
		if err != nil {
			return err
		}
		node.Annotations[util.OVNNodeBridgeEgressIPs] = string(patch)
		return g.kube.UpdateNodeStatus(node)
	})
}

func (g *bridgeEIPAddrManager) addIPBridge(ip net.IP) error {
	link, err := util.GetNetLinkOps().LinkByName(g.bridgeName)
	if err != nil {
		return fmt.Errorf("failed to get link obj by name %s: %v", g.bridgeName, err)
	}
	return g.addrManager.AddAddress(getEIPBridgeNetlinkAddress(ip, link.Attrs().Index))
}

func (g *bridgeEIPAddrManager) deleteIPBridge(ip net.IP) error {
	link, err := util.GetNetLinkOps().LinkByName(g.bridgeName)
	if err != nil {
		return fmt.Errorf("failed to get link obj by name %s: %v", g.bridgeName, err)
	}
	return g.addrManager.DelAddress(getEIPBridgeNetlinkAddress(ip, link.Attrs().Index))
}

// getAnnotationIPs retrieves the egress IP annotation from the current node Nodes object. If multiple users, callers must synchronise.
// if annotation isn't present, empty set is returned
func (g *bridgeEIPAddrManager) getAnnotationIPs() ([]net.IP, error) {
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

func parseEIPMarkIP(annotations map[string]string, eip string) (net.IP, util.EgressIPMark, error) {
	pktMark, err := util.ParseEgressIPMark(annotations)
	if err != nil {
		return nil, pktMark, fmt.Errorf("failed to extract packet mark from EgressIP annotations: %v", err)
	}
	// status update and pkt mark should be configured as one operation by cluster manager
	if !pktMark.IsAvailable() {
		return nil, pktMark, fmt.Errorf("packet mark is not set")
	}
	if !pktMark.IsValid() {
		return nil, pktMark, fmt.Errorf("packet mark is not valid")
	}
	ip := net.ParseIP(eip)
	if ip == nil {
		return nil, pktMark, fmt.Errorf("invalid IP")
	}
	return ip, pktMark, nil
}

func getIPsStr(ips ...net.IP) []string {
	ipsStr := make([]string, 0, len(ips))
	for _, ip := range ips {
		ipsStr = append(ipsStr, ip.String())
	}
	return ipsStr
}

func getEIPBridgeNetlinkAddress(ip net.IP, ifindex int) netlink.Addr {
	return netlink.Addr{
		IPNet:     &net.IPNet{IP: ip, Mask: util.GetIPFullMask(ip)},
		Flags:     getEIPNetlinkAddressFlag(ip),
		Scope:     int(netlink.SCOPE_LINK),
		ValidLft:  getEIPNetlinkAddressValidLft(ip),
		LinkIndex: ifindex,
	}
}

func getEIPNetlinkAddressFlag(ip net.IP) int {
	// isV6?
	if ip.To4() == nil && ip.To16() != nil {
		return unix.IFA_F_NODAD
	}
	return 0
}

func getEIPNetlinkAddressValidLft(ip net.IP) int {
	// isV6?
	if ip.To4() == nil && ip.To16() != nil {
		return math.MaxUint32
	}
	return 0
}
